//! Os upstreams falsos em processo: HTTP streamable 2026-07-28 (sem sessão), HTTP streamable
//! 2025-11-25 (com sessão), SSE 2024-11-05 escrito à mão, um que aceita e nunca responde, e o
//! provedor OAuth falso com um MCP protegido atrás dele. O STDIO é o binário
//! `patchbay-upstream-falso`. Nenhum deles sai de 127.0.0.1.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};

use axum::Router;
use axum::extract::{Query, Request, State};
use axum::http::{HeaderMap, StatusCode, header};
use axum::middleware::{self, Next};
use axum::response::sse::{Event, Sse};
use axum::response::{IntoResponse, Redirect, Response};
use axum::routing::{any, get, post};
use base64::Engine;
use futures::stream::{self, Stream, StreamExt};
use rmcp::transport::streamable_http_server::session::local::LocalSessionManager;
use rmcp::transport::streamable_http_server::{StreamableHttpServerConfig, StreamableHttpService};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::sync::mpsc;

use super::falso_mcp::{Falso, Perfil};

/// Um servidor em processo; a tarefa morre junto com ele.
pub struct Servidor {
    pub base: String,
    tarefa: tokio::task::JoinHandle<()>,
}

impl Drop for Servidor {
    fn drop(&mut self) {
        self.tarefa.abort();
    }
}

async fn servir(rotas: Router) -> Servidor {
    let ouvinte = tokio::net::TcpListener::bind("127.0.0.1:0").await.expect("porta do falso");
    let base = format!("http://{}", ouvinte.local_addr().unwrap());
    let tarefa = tokio::spawn(async move {
        let _ = axum::serve(ouvinte, rotas).await;
    });
    Servidor { base, tarefa }
}

fn mcp_de(falso: Falso, sessao: bool) -> StreamableHttpService<Falso, LocalSessionManager> {
    let config = StreamableHttpServerConfig::default().with_legacy_session_mode(sessao).with_sse_keep_alive(None);
    StreamableHttpService::new(move || Ok(falso.clone()), LocalSessionManager::default().into(), config)
}

/// Um upstream MCP por HTTP streamable em `<base>/mcp`.
pub struct UpstreamHttp {
    pub servidor: Servidor,
    pub falso: Falso,
}

impl UpstreamHttp {
    pub fn url(&self) -> String {
        format!("{}/mcp", self.servidor.base)
    }
}

/// 2026-07-28, sem sessão (como os servidores do dono).
pub async fn http_moderno() -> UpstreamHttp {
    let falso = Falso::new(Perfil::Moderno);
    let servidor = servir(Router::new().nest_service("/mcp", mcp_de(falso.clone(), false))).await;
    UpstreamHttp { servidor, falso }
}

/// 2025-11-25, com `Mcp-Session-Id`.
pub async fn http_antigo() -> UpstreamHttp {
    let falso = Falso::new(Perfil::Antigo);
    let servidor = servir(Router::new().nest_service("/mcp", mcp_de(falso.clone(), true))).await;
    UpstreamHttp { servidor, falso }
}

/// Aceita a conexão e nunca responde: o "fora do ar" que mais importa (recusa é rápida, hang não).
pub async fn que_pendura() -> Servidor {
    servir(Router::new().fallback(any(std::future::pending::<StatusCode>))).await
}

// ---------- SSE 2024-11-05 ----------

type Sessoes = Arc<Mutex<HashMap<String, mpsc::UnboundedSender<Value>>>>;

/// O transporte HTTP+SSE da 2024-11-05, à mão: GET `/sse` abre o stream e anuncia no evento
/// `endpoint` onde postar; cada POST em `/mensagens` responde 202 e a resposta sai pelo stream.
pub async fn sse_legado() -> Servidor {
    let sessoes: Sessoes = Arc::default();
    let rotas = Router::new().route("/sse", get(abrir_sse)).route("/mensagens", post(mensagem_sse)).with_state(sessoes);
    servir(rotas).await
}

async fn abrir_sse(State(sessoes): State<Sessoes>) -> Sse<impl Stream<Item = Result<Event, std::convert::Infallible>>> {
    static N: AtomicU32 = AtomicU32::new(0);
    let id = format!("s{}", N.fetch_add(1, Ordering::SeqCst));
    let (tx, rx) = mpsc::unbounded_channel::<Value>();
    sessoes.lock().unwrap().insert(id.clone(), tx);
    let anuncio = stream::once(async move { Ok(Event::default().event("endpoint").data(format!("/mensagens?sessao={id}"))) });
    let mensagens =
        stream::unfold(
            rx,
            |mut rx| async move { rx.recv().await.map(|m| (Ok(Event::default().event("message").data(m.to_string())), rx)) },
        );
    Sse::new(anuncio.chain(mensagens))
}

async fn mensagem_sse(State(sessoes): State<Sessoes>, Query(q): Query<HashMap<String, String>>, corpo: String) -> StatusCode {
    let Some(tx) = q.get("sessao").and_then(|s| sessoes.lock().unwrap().get(s).cloned()) else {
        return StatusCode::NOT_FOUND;
    };
    let Ok(pedido) = serde_json::from_str::<Value>(&corpo) else {
        return StatusCode::BAD_REQUEST;
    };
    // Notificação não tem resposta.
    let Some(id) = pedido.get("id").cloned() else {
        return StatusCode::ACCEPTED;
    };
    let resultado = match pedido["method"].as_str().unwrap_or("") {
        "initialize" => json!({
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "upstream-falso-sse", "version": "0.0.1"}
        }),
        "ping" => json!({}),
        "tools/list" => json!({"tools": [{
            "name": "sse_eco",
            "description": "eco pelo SSE de 2024-11-05",
            "inputSchema": {"type": "object", "properties": {"texto": {"type": "string"}}}
        }]}),
        "tools/call" if pedido["params"]["name"] == "sse_eco" => {
            let t = pedido["params"]["arguments"]["texto"].as_str().unwrap_or("").to_string();
            json!({"content": [{"type": "text", "text": format!("sse:{t}")}]})
        }
        metodo => {
            let _ = tx.send(json!({"jsonrpc": "2.0", "id": id, "error": {"code": -32601, "message": format!("sem {metodo}")}}));
            return StatusCode::ACCEPTED;
        }
    };
    let _ = tx.send(json!({"jsonrpc": "2.0", "id": id, "result": resultado}));
    StatusCode::ACCEPTED
}

// ---------- o provedor OAuth falso ----------

/// O que o provedor emitiu e viu. `recusar_refresh` liga o `invalid_grant`, que é como um provedor
/// diz que o consentimento acabou.
#[derive(Default)]
pub struct Provedor {
    codigos: HashMap<String, (String, String, String)>,
    acessos: HashSet<String>,
    refreshs: HashSet<String>,
    n: u32,
    pub registros: Vec<Value>,
    pub pedidos_de_autorizacao: Vec<HashMap<String, String>>,
    pub trocas: Vec<HashMap<String, String>>,
    pub recusar_refresh: bool,
}

/// Um MCP em `<base>/mcp` protegido por bearer, com o authorization server no mesmo host:
/// metadata RFC 9728 e RFC 8414, DCR, authorize (consente na hora), token com PKCE S256 e
/// rotação de refresh com detecção de reuso.
pub struct UpstreamOAuth {
    pub servidor: Servidor,
    pub falso: Falso,
    pub provedor: Arc<Mutex<Provedor>>,
}

impl UpstreamOAuth {
    pub fn url(&self) -> String {
        format!("{}/mcp", self.servidor.base)
    }
}

#[derive(Clone)]
struct EstadoOAuth {
    base: Arc<Mutex<String>>,
    provedor: Arc<Mutex<Provedor>>,
}

pub async fn http_com_oauth() -> UpstreamOAuth {
    let falso = Falso::new(Perfil::Protegido);
    let provedor: Arc<Mutex<Provedor>> = Arc::default();
    // A base só existe depois de abrir a porta; os handlers a leem daqui.
    let estado = EstadoOAuth { base: Arc::default(), provedor: provedor.clone() };
    let protegido = Router::new()
        .nest_service("/mcp", mcp_de(falso.clone(), false))
        .layer(middleware::from_fn_with_state(estado.clone(), exigir_token));
    let rotas = Router::new()
        .route("/.well-known/oauth-protected-resource", get(metadata_recurso))
        .route("/.well-known/oauth-protected-resource/mcp", get(metadata_recurso))
        .route("/.well-known/oauth-authorization-server", get(metadata_as))
        .route("/registrar", post(registrar))
        .route("/autorizar", get(autorizar))
        .route("/token", post(token))
        .with_state(estado.clone())
        .merge(protegido);
    let servidor = servir(rotas).await;
    *estado.base.lock().unwrap() = servidor.base.clone();
    UpstreamOAuth { servidor, falso, provedor }
}

async fn exigir_token(State(e): State<EstadoOAuth>, pedido: Request, seguinte: Next) -> Response {
    let token = pedido.headers().get(header::AUTHORIZATION).and_then(|v| v.to_str().ok()).and_then(|v| v.strip_prefix("Bearer "));
    if token.is_some_and(|t| e.provedor.lock().unwrap().acessos.contains(t)) {
        return seguinte.run(pedido).await;
    }
    let base = e.base.lock().unwrap().clone();
    let desafio = format!(r#"Bearer resource_metadata="{base}/.well-known/oauth-protected-resource/mcp""#);
    (StatusCode::UNAUTHORIZED, [(header::WWW_AUTHENTICATE, desafio)], "sem token").into_response()
}

async fn metadata_recurso(State(e): State<EstadoOAuth>) -> axum::Json<Value> {
    let base = e.base.lock().unwrap().clone();
    axum::Json(json!({"resource": format!("{base}/mcp"), "authorization_servers": [base], "bearer_methods_supported": ["header"]}))
}

async fn metadata_as(State(e): State<EstadoOAuth>) -> axum::Json<Value> {
    let base = e.base.lock().unwrap().clone();
    axum::Json(json!({
        "issuer": base,
        "authorization_endpoint": format!("{base}/autorizar"),
        "token_endpoint": format!("{base}/token"),
        "registration_endpoint": format!("{base}/registrar"),
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "code_challenge_methods_supported": ["S256"],
        "authorization_response_iss_parameter_supported": true,
        "token_endpoint_auth_methods_supported": ["none", "client_secret_post", "client_secret_basic"]
    }))
}

async fn registrar(State(e): State<EstadoOAuth>, axum::Json(mut pedido): axum::Json<Value>) -> (StatusCode, axum::Json<Value>) {
    let mut p = e.provedor.lock().unwrap();
    p.registros.push(pedido.clone());
    pedido["client_id"] = json!(format!("cliente-{}", p.registros.len()));
    pedido["token_endpoint_auth_method"] = json!("none");
    (StatusCode::CREATED, axum::Json(pedido))
}

/// Consente na hora: devolve o navegador ao `redirect_uri` com o code.
async fn autorizar(State(e): State<EstadoOAuth>, Query(q): Query<HashMap<String, String>>) -> Response {
    let mut p = e.provedor.lock().unwrap();
    p.pedidos_de_autorizacao.push(q.clone());
    let (Some(volta), Some(desafio)) = (q.get("redirect_uri"), q.get("code_challenge")) else {
        return (StatusCode::BAD_REQUEST, "faltou redirect_uri ou code_challenge").into_response();
    };
    p.n += 1;
    let codigo = format!("codigo-{}", p.n);
    let cliente = q.get("client_id").cloned().unwrap_or_default();
    p.codigos.insert(codigo.clone(), (desafio.clone(), cliente, volta.clone()));
    let estado = q.get("state").cloned().unwrap_or_default();
    let base = e.base.lock().unwrap().clone();
    let destino = reqwest::Url::parse_with_params(volta, &[("code", codigo.as_str()), ("state", estado.as_str()), ("iss", base.as_str())]);
    match destino {
        Ok(u) => Redirect::to(u.as_str()).into_response(),
        Err(_) => (StatusCode::BAD_REQUEST, "redirect_uri inválido").into_response(),
    }
}

fn erro_oauth(codigo: &str) -> Response {
    (StatusCode::BAD_REQUEST, axum::Json(json!({"error": codigo}))).into_response()
}

async fn token(State(e): State<EstadoOAuth>, _: HeaderMap, axum::Form(f): axum::Form<HashMap<String, String>>) -> Response {
    let mut p = e.provedor.lock().unwrap();
    p.trocas.push(f.clone());
    let campo = |k: &str| f.get(k).cloned().unwrap_or_default();
    match campo("grant_type").as_str() {
        "authorization_code" => {
            let Some((desafio, _, volta)) = p.codigos.remove(&campo("code")) else {
                return erro_oauth("invalid_grant");
            };
            let calculado = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(Sha256::digest(campo("code_verifier")));
            if calculado != desafio || volta != campo("redirect_uri") {
                return erro_oauth("invalid_grant");
            }
        }
        "refresh_token" => {
            if p.recusar_refresh || !p.refreshs.remove(&campo("refresh_token")) {
                return erro_oauth("invalid_grant");
            }
        }
        _ => return erro_oauth("unsupported_grant_type"),
    }
    p.n += 1;
    let (acesso, refresh) = (format!("acesso-{}", p.n), format!("refresh-{}", p.n));
    p.acessos.insert(acesso.clone());
    p.refreshs.insert(refresh.clone());
    axum::Json(json!({"access_token": acesso, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600})).into_response()
}
