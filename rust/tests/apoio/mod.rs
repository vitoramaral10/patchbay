//! O arnês da caixa-preta: sobe o binário de `PATCHBAY_BIN` como processo, num diretório de dados
//! próprio e numa porta própria, com o ambiente limpo; dirige a tela de administração como um
//! navegador; fala MCP com clientes rmcp reais; e grava ou confere as respostas.
//!
//! Nada sai de 127.0.0.1: o ambiente do processo aponta `HTTP(S)_PROXY` para uma porta fechada, e o
//! Go não passa loopback por proxy. Assim a varredura da biblioteca (mcpservers.org) falha na hora,
//! e qualquer outra saída para a rede também.

#![allow(dead_code)] // cada cena usa uma parte

pub mod falso_mcp;
pub mod falsos;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::process::{Child, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, LazyLock, Mutex};
use std::time::{Duration, Instant};

use regex::Regex;
use reqwest::header::HeaderMap;
use rmcp::model::{ClientConfig, ElicitRequestParams, ElicitResult, ElicitationAction, Implementation, ProtocolVersion};
use rmcp::service::{ClientLifecycleMode, ClientServiceExt, NotificationContext, RequestContext, RunningService};
use rmcp::transport::StreamableHttpClientTransport;
use rmcp::transport::streamable_http_client::StreamableHttpClientTransportConfig;
use rmcp::{ErrorData, RoleClient};
use serde_json::{Value, json};

/// A chave mestra fixa: 32 bytes em base64. Fixa para o banco ser reproduzível entre os binários.
pub const CHAVE_MESTRA: &str = "cGF0Y2hiYXktY2FpeGEtcHJldGEtY2hhdmUtZml4YSE=";
pub const USUARIO: &str = "vitor";
pub const SENHA: &str = "senha-de-doze-ou-mais";

/// O binário sob teste: `PATCHBAY_BIN`, ou o deste crate.
pub fn binario() -> String {
    std::env::var("PATCHBAY_BIN").ok().filter(|b| !b.is_empty()).unwrap_or_else(|| env!("CARGO_BIN_EXE_patchbay").to_string())
}

/// O upstream STDIO falso.
pub fn bin_stdio() -> &'static str {
    env!("CARGO_BIN_EXE_patchbay-upstream-falso")
}

/// Se esta execução regrava `tests/respostas_go.json`.
pub fn gravando() -> bool {
    std::env::var("PATCHBAY_GRAVAR").is_ok_and(|v| !v.is_empty())
}

fn porta_livre() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port()
}

/// A saída de um subcomando.
#[derive(Debug)]
pub struct Saida {
    pub codigo: Option<i32>,
    pub stdout: String,
    pub stderr: String,
}

/// Um patchbay: o diretório de dados e, depois de [`Patchbay::subir`], o processo `serve`.
pub struct Patchbay {
    /// `http://127.0.0.1:<porta>`, que é também a URL pública.
    pub raiz: String,
    pub dir: PathBuf,
    pub dados: PathBuf,
    processo: Option<Child>,
}

impl Patchbay {
    /// Diretório novo e porta nova, sem processo ainda (para rodar `seed` antes do `serve`).
    pub fn preparar() -> Self {
        static N: AtomicUsize = AtomicUsize::new(0);
        let dir = std::env::temp_dir().join(format!("patchbay-caixa-preta-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let raiz = format!("http://127.0.0.1:{}", porta_livre());
        Patchbay { raiz, dados: dir.join("dados"), dir, processo: None }
    }

    /// Prepara e sobe.
    pub async fn subir() -> Self {
        let mut p = Patchbay::preparar();
        p.iniciar().await;
        p
    }

    fn ambiente(&self) -> Vec<(&'static str, String)> {
        let fechada = "http://127.0.0.1:9".to_string();
        vec![
            ("PATH", "/usr/local/bin:/usr/bin:/bin".into()),
            ("HOME", self.dir.display().to_string()),
            ("HTTP_PROXY", fechada.clone()),
            ("HTTPS_PROXY", fechada),
            ("PATCHBAY_LISTEN", self.raiz.trim_start_matches("http://").to_string()),
            ("PATCHBAY_DATA_DIR", self.dados.display().to_string()),
            ("PATCHBAY_PUBLIC_URL", self.raiz.clone()),
            ("PATCHBAY_MASTER_KEY", CHAVE_MESTRA.into()),
            ("PATCHBAY_LOG_LEVEL", "debug".into()),
        ]
    }

    /// Roda um subcomando com o mesmo ambiente do `serve`.
    pub async fn comando(&self, args: &[&str]) -> Saida {
        let saida = tokio::process::Command::new(binario())
            .args(args)
            .env_clear()
            .envs(self.ambiente())
            .current_dir(&self.dir)
            .output()
            .await
            .unwrap_or_else(|e| panic!("rodar {} {args:?}: {e}", binario()));
        Saida {
            codigo: saida.status.code(),
            stdout: String::from_utf8_lossy(&saida.stdout).into_owned(),
            stderr: String::from_utf8_lossy(&saida.stderr).into_owned(),
        }
    }

    pub fn log(&self) -> PathBuf {
        self.dir.join("patchbay.log")
    }

    /// Sobe `serve` e espera `/saude`.
    pub async fn iniciar(&mut self) {
        let log = std::fs::OpenOptions::new().create(true).append(true).open(self.log()).unwrap();
        let filho = std::process::Command::new(binario())
            .arg("serve")
            .env_clear()
            .envs(self.ambiente())
            .current_dir(&self.dir)
            .stdin(Stdio::null())
            .stdout(log.try_clone().unwrap())
            .stderr(log)
            .spawn()
            .unwrap_or_else(|e| panic!("subir {}: {e}", binario()));
        self.processo = Some(filho);
        let prazo = Instant::now() + Duration::from_secs(10);
        while Instant::now() < prazo {
            if reqwest::get(format!("{}/saude", self.raiz)).await.is_ok_and(|r| r.status().is_success()) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        panic!("{} não subiu em 10 s:\n{}", binario(), self.cauda_do_log());
    }

    /// SIGTERM e espera o desligamento limpo (o Go drena a trilha e fecha os STDIO).
    pub fn parar(&mut self) {
        let Some(mut filho) = self.processo.take() else { return };
        let _ = std::process::Command::new("kill").args(["-TERM", &filho.id().to_string()]).status();
        let prazo = Instant::now() + Duration::from_secs(15);
        while Instant::now() < prazo {
            if filho.try_wait().ok().flatten().is_some() {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        let _ = filho.kill();
        let _ = filho.wait();
    }

    pub fn cauda_do_log(&self) -> String {
        let todo = std::fs::read_to_string(self.log()).unwrap_or_default();
        // Sem o ruído de cada requisição: acesso e sessões do servidor MCP.
        let ruido = ["\"acesso http\"", "\"server session", "\"server connecting\""];
        let linhas: Vec<&str> = todo.lines().filter(|l| !ruido.iter().any(|r| l.contains(r))).collect();
        linhas[linhas.len().saturating_sub(60)..].join("\n")
    }

    /// O banco, para o despejo.
    pub fn banco(&self) -> PathBuf {
        self.dados.join("patchbay.db")
    }

    /// URL do endpoint MCP.
    pub fn mcp(&self, slug: &str) -> String {
        format!("{}/mcp/{slug}", self.raiz)
    }
}

impl Drop for Patchbay {
    fn drop(&mut self) {
        self.parar();
        if std::thread::panicking() {
            eprintln!("---- log do patchbay ({}) ----\n{}", self.dir.display(), self.cauda_do_log());
        }
        if std::env::var_os("PATCHBAY_MANTER").is_none() {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }
}

// ---------- navegador ----------

/// Uma resposta HTTP inteira.
#[derive(Debug, Clone)]
pub struct Resposta {
    pub status: u16,
    pub cabecalhos: HeaderMap,
    pub corpo: String,
}

impl Resposta {
    pub fn local(&self) -> Option<String> {
        self.cabecalhos.get("location").and_then(|v| v.to_str().ok()).map(str::to_string)
    }

    pub fn cabecalho(&self, nome: &str) -> Option<String> {
        self.cabecalhos.get(nome).and_then(|v| v.to_str().ok()).map(str::to_string)
    }

    pub fn json(&self) -> Value {
        serde_json::from_str(&self.corpo).unwrap_or_else(|e| panic!("não é JSON ({e}): {}", self.corpo))
    }

    /// Status, cabeçalhos que contam e corpo: o que se grava de uma resposta de protocolo.
    pub fn resumo(&self, cabecalhos: &[&str]) -> Value {
        let mut c = serde_json::Map::new();
        for n in cabecalhos {
            c.insert(n.to_string(), json!(self.cabecalho(n)));
        }
        let corpo = serde_json::from_str::<Value>(&self.corpo).unwrap_or_else(|_| Value::String(self.corpo.trim().to_string()));
        json!({"status": self.status, "cabecalhos": c, "corpo": corpo})
    }
}

/// O navegador do admin: cookie de sessão à mão, sem seguir redirect, e os cabeçalhos de mesma
/// origem que um formulário da própria página manda.
pub struct Navegador {
    pub raiz: String,
    http: reqwest::Client,
    pub cookie: Option<String>,
}

pub const COOKIE: &str = "patchbay_sessao_admin";

impl Navegador {
    pub fn new(raiz: &str) -> Self {
        let http = reqwest::Client::builder().redirect(reqwest::redirect::Policy::none()).build().unwrap();
        Navegador { raiz: raiz.to_string(), http, cookie: None }
    }

    fn url(&self, caminho: &str) -> String {
        if caminho.starts_with("http") { caminho.to_string() } else { format!("{}{caminho}", self.raiz) }
    }

    /// Uma requisição; `site` é o `Sec-Fetch-Site`, `origem` o `Origin` (os dois omitidos com `None`).
    pub async fn pedir(
        &mut self,
        metodo: reqwest::Method,
        caminho: &str,
        campos: Option<&[(&str, &str)]>,
        site: Option<&str>,
        origem: Option<&str>,
    ) -> Resposta {
        let mut p = self.http.request(metodo.clone(), self.url(caminho));
        if let Some(c) = campos {
            p = p.form(c);
        }
        if let Some(s) = site {
            p = p.header("Sec-Fetch-Site", s);
        }
        if let Some(o) = origem {
            p = p.header("Origin", o);
        }
        if let Some(c) = &self.cookie {
            p = p.header("Cookie", format!("{COOKIE}={c}"));
        }
        let r = p.send().await.unwrap_or_else(|e| panic!("{metodo} {caminho}: {e}"));
        let status = r.status().as_u16();
        let cabecalhos = r.headers().clone();
        for v in cabecalhos.get_all("set-cookie") {
            let v = v.to_str().unwrap_or("");
            if let Some(resto) = v.strip_prefix(&format!("{COOKIE}=")) {
                let valor = resto.split(';').next().unwrap_or("");
                self.cookie = (!valor.is_empty() && !v.contains("Max-Age=0")).then(|| valor.to_string());
            }
        }
        Resposta { status, cabecalhos, corpo: r.text().await.unwrap_or_default() }
    }

    pub async fn get(&mut self, caminho: &str) -> Resposta {
        let raiz = self.raiz.clone();
        self.pedir(reqwest::Method::GET, caminho, None, Some("same-origin"), Some(&raiz)).await
    }

    /// POST de formulário da própria página.
    pub async fn post(&mut self, caminho: &str, campos: &[(&str, &str)]) -> Resposta {
        let raiz = self.raiz.clone();
        self.pedir(reqwest::Method::POST, caminho, Some(campos), Some("same-origin"), Some(&raiz)).await
    }

    /// Segue os 3xx com GET, como o navegador.
    pub async fn seguir(&mut self, mut r: Resposta) -> Resposta {
        for _ in 0..5 {
            match (r.status, r.local()) {
                (300..=399, Some(destino)) => r = self.get(&destino).await,
                _ => break,
            }
        }
        r
    }

    /// Cria o admin pela tela de primeiro acesso; a sessão fica aberta.
    pub async fn setup(&mut self) -> Resposta {
        let r = self.post("/admin/setup", &[("usuario", USUARIO), ("senha", SENHA), ("confirmar", SENHA)]).await;
        assert_eq!(r.status, 303, "setup: {}", r.corpo);
        assert!(self.cookie.is_some(), "setup sem cookie de sessão");
        r
    }

    /// POST que cria um recurso e cai em `<rota>/<id>`; devolve o id.
    pub async fn criar(&mut self, rota: &str, campos: &[(&str, &str)]) -> i64 {
        let r = self.post(rota, campos).await;
        let destino = r.local().unwrap_or_else(|| panic!("POST {rota}: status {} sem Location: {}", r.status, r.corpo));
        let id = destino.trim_start_matches(rota).trim_start_matches('/').split(['?', '/']).next().unwrap_or("");
        id.parse().unwrap_or_else(|_| panic!("POST {rota}: destino {destino} sem id"))
    }

    /// Emite uma chave de API pela tela e devolve o valor em claro (aparece uma vez só).
    pub async fn criar_chave(&mut self, nome: &str, endpoints: &[i64]) -> String {
        let ids: Vec<String> = endpoints.iter().map(i64::to_string).collect();
        let mut campos = vec![("nome", nome)];
        campos.extend(ids.iter().map(|i| ("endpoint", i.as_str())));
        let r = self.post("/admin/chaves", &campos).await;
        assert_eq!(r.status, 201, "criar chave: {}", r.corpo);
        static CHAVE: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"pbk_[a-z0-9]+_[A-Za-z0-9_-]{20,}").unwrap());
        CHAVE.find(&r.corpo).expect("chave em claro na tela de criação").as_str().to_string()
    }
}

// ---------- clientes MCP ----------

/// O usuário atrás do cliente MCP: o que responde à elicitação, se o cliente fala a revisão antiga,
/// e o que chegou a ele.
#[derive(Clone, Default)]
pub struct Usuario {
    /// `None`: o cliente não declara elicitação.
    pub resposta: Option<ElicitationAction>,
    /// Cliente da 2025-11-25 (`initialize`), em vez da 2026-07-28 (`server/discover`).
    pub antigo: bool,
    pub perguntas: Arc<Mutex<Vec<String>>>,
    pub listas_mudadas: Arc<AtomicUsize>,
}

impl Usuario {
    pub fn new() -> Self {
        Usuario::default()
    }

    pub fn respondendo(acao: ElicitationAction) -> Self {
        Usuario { resposta: Some(acao), ..Usuario::default() }
    }

    pub fn antigo(mut self) -> Self {
        self.antigo = true;
        self
    }
}

impl rmcp::ClientHandler for Usuario {
    fn get_info(&self) -> ClientConfig {
        let caps = if self.resposta.is_some() { json!({"elicitation": {"form": {}}}) } else { json!({}) };
        let mut c = ClientConfig::new(serde_json::from_value(caps).unwrap(), Implementation::new("caixa-preta", env!("CARGO_PKG_VERSION")));
        if self.antigo {
            c.protocol_version = ProtocolVersion::V_2025_11_25;
        }
        c
    }

    async fn create_elicitation(&self, pedido: ElicitRequestParams, _: RequestContext<RoleClient>) -> Result<ElicitResult, ErrorData> {
        if let ElicitRequestParams::FormElicitationParams { message, .. } = &pedido {
            self.perguntas.lock().unwrap().push(message.clone());
        }
        let r = self.resposta.clone().ok_or_else(|| ErrorData::internal_error("perguntou a um cliente sem elicitação", None))?;
        Ok(ElicitResult::new(r))
    }

    async fn on_tool_list_changed(&self, _: NotificationContext<RoleClient>) {
        self.listas_mudadas.fetch_add(1, Ordering::SeqCst);
    }
}

pub type Cliente = RunningService<RoleClient, Usuario>;

/// Um cliente rmcp real: 2026-07-28 por `server/discover`, ou 2025-11-25 por `initialize` quando o
/// usuário é [`Usuario::antigo`]. Confere a revisão negociada.
pub async fn conectar(url: &str, token: &str, usuario: &Usuario) -> Cliente {
    let transporte =
        StreamableHttpClientTransport::from_config(StreamableHttpClientTransportConfig::with_uri(url.to_string()).auth_header(token));
    let (ciclo, quer) = if usuario.antigo {
        (ClientLifecycleMode::Initialize, ProtocolVersion::V_2025_11_25)
    } else {
        (ClientLifecycleMode::Discover { preferred_versions: vec![ProtocolVersion::V_2026_07_28] }, ProtocolVersion::V_2026_07_28)
    };
    let cliente = usuario.clone().serve_with_lifecycle(transporte, ciclo).await.unwrap_or_else(|e| panic!("conectar em {url}: {e}"));
    let versao = cliente.peer_info().map(|i| i.protocol_version.to_string());
    assert_eq!(versao.as_deref(), Some(quer.as_str()), "revisão negociada");
    cliente
}

/// JSON-RPC cru por HTTP, para o que o cliente não expõe: status, cabeçalhos, corpo.
pub async fn rpc(url: &str, autorizacao: Option<&str>, corpo: Option<Value>, extras: &[(&str, &str)]) -> Resposta {
    let mut p =
        reqwest::Client::new().post(url).header("Content-Type", "application/json").header("Accept", "application/json, text/event-stream");
    for (k, v) in extras {
        p = p.header(*k, *v);
    }
    if let Some(a) = autorizacao {
        p = p.header("Authorization", a);
    }
    if let Some(c) = corpo {
        p = p.body(c.to_string());
    }
    let r = p.send().await.unwrap_or_else(|e| panic!("POST {url}: {e}"));
    let status = r.status().as_u16();
    let cabecalhos = r.headers().clone();
    let texto = r.text().await.unwrap_or_default();
    // SSE ou JSON: a mensagem é a linha data: com id, ou o corpo inteiro.
    let corpo =
        texto.lines().filter_map(|l| l.strip_prefix("data:")).map(str::trim).find(|l| l.contains("\"id\"")).unwrap_or(&texto).to_string();
    Resposta { status, cabecalhos, corpo }
}

/// O `_meta` por requisição da 2026-07-28, para o JSON-RPC cru.
pub fn meta_2026() -> Value {
    json!({
        "io.modelcontextprotocol/protocolVersion": "2026-07-28",
        "io.modelcontextprotocol/clientInfo": {"name": "caixa-preta", "version": env!("CARGO_PKG_VERSION")},
        "io.modelcontextprotocol/clientCapabilities": {}
    })
}

// ---------- gravação e conferência ----------

/// Regras de máscara para o que muda a cada execução e não diz nada sobre o comportamento.
static MASCARAS: LazyLock<Vec<(Regex, &'static str)>> = LazyLock::new(|| {
    [
        (r"pbk_[a-z0-9]+_[A-Za-z0-9_-]{20,}", "pbk_«chave»"),
        (r"pbk_[a-z0-9]{8}", "pbk_«prefixo»"),
        (r"\b(pbat|pbrt|pbac|pbcs|pbc)_[A-Za-z0-9_-]{8,}", "${1}_«aleatório»"),
        (r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?", "«instante»"),
        (r"\d{2}/\d{2}/\d{4}", "«data»"),
        (r"\b\d{2}:\d{2}(:\d{2})?\b", "«hora»"),
        (r"127\.0\.0\.1:\d{2,5}", "127.0.0.1:«porta»"),
        (r"localhost:\d{2,5}", "localhost:«porta»"),
        (r"\b[0-9a-f]{64}\b", "«sha256»"),
    ]
    .into_iter()
    .map(|(r, s)| (Regex::new(r).unwrap(), s))
    .collect()
});

/// Respostas de uma cena: gravadas do Go com `PATCHBAY_GRAVAR=1`, conferidas por valor nas outras
/// execuções. Cada string passa pelas trocas literais (URL do patchbay e dos falsos, diretórios) e
/// pelas [`MASCARAS`] antes de entrar.
pub struct Gravacao {
    cena: &'static str,
    passos: BTreeMap<String, Value>,
    trocas: Vec<(String, String)>,
}

/// Diferenças aceitas entre o binário sob teste e o Go: `(cena/passo, porquê)`. Vazia na F0.
const DIFERENCAS_CONHECIDAS: &[(&str, &str)] = &[];

impl Gravacao {
    pub fn new(cena: &'static str) -> Self {
        Gravacao { cena, passos: BTreeMap::new(), trocas: Vec::new() }
    }

    /// Troca `literal` por `por` em todo texto gravado (as mais longas primeiro), também na forma
    /// codificada de URL, uma e duas vezes (o `destino` do login carrega uma URL dentro de outra).
    pub fn trocar(&mut self, literal: impl Into<String>, por: &str) {
        let literal = literal.into();
        if literal.is_empty() {
            return;
        }
        let uma = codificar(&literal);
        let duas = codificar(&uma);
        if uma != literal {
            self.trocas.push((duas, codificar(&codificar(por))));
            self.trocas.push((uma, codificar(por)));
        }
        self.trocas.push((literal, por.to_string()));
        self.trocas.sort_by_key(|(l, _)| std::cmp::Reverse(l.len()));
    }

    pub fn texto(&self, s: &str) -> String {
        let mut s = s.to_string();
        for (l, p) in &self.trocas {
            s = s.replace(l.as_str(), p);
        }
        for (r, p) in MASCARAS.iter() {
            s = r.replace_all(&s, *p).into_owned();
        }
        s
    }

    pub fn normalizar(&self, v: &Value) -> Value {
        match v {
            Value::String(s) => Value::String(self.texto(s)),
            Value::Array(a) => Value::Array(a.iter().map(|x| self.normalizar(x)).collect()),
            Value::Object(m) => Value::Object(
                m.iter()
                    // Instante em segundos Unix nos documentos OAuth (client_id_issued_at...) e a
                    // validade, que cai de 3600 para 3599 conforme o segundo em que a troca acontece.
                    .map(|(k, x)| {
                        let v = match () {
                            _ if k.ends_with("_at") && x.is_number() => json!("«instante»"),
                            _ if k == "expires_in" && x.is_number() => json!("«segundos»"),
                            _ => self.normalizar(x),
                        };
                        (self.texto(k), v)
                    })
                    .collect(),
            ),
            outro => outro.clone(),
        }
    }

    pub fn anotar(&mut self, passo: &str, v: Value) {
        let n = self.normalizar(&v);
        assert!(self.passos.insert(passo.to_string(), n).is_none(), "passo repetido: {passo}");
    }

    /// HTML como texto: espaços colapsados, uma tag por linha, valores dinâmicos mascarados.
    pub fn anotar_html(&mut self, passo: &str, html: &str) {
        static ESPACOS: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\s+").unwrap());
        let colapsado = ESPACOS.replace_all(html, " ");
        let linhas: Vec<Value> =
            colapsado.replace("> <", "><").replace("><", ">\n<").lines().map(|l| Value::String(l.trim().to_string())).collect();
        self.anotar(passo, Value::Array(linhas));
    }

    /// Grava (com `PATCHBAY_GRAVAR=1`) ou confere com o que o Go respondeu.
    pub fn conferir(self) {
        static TRAVA: Mutex<()> = Mutex::new(());
        let arquivo = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/respostas_go.json");
        let _trava = TRAVA.lock().unwrap_or_else(|e| e.into_inner());
        let mut todas: BTreeMap<String, BTreeMap<String, Value>> =
            std::fs::read_to_string(&arquivo).ok().and_then(|t| serde_json::from_str(&t).ok()).unwrap_or_default();
        if gravando() {
            todas.insert(self.cena.to_string(), self.passos);
            std::fs::write(&arquivo, serde_json::to_string_pretty(&todas).unwrap() + "\n").unwrap();
            return;
        }
        let go = todas.remove(self.cena).unwrap_or_else(|| panic!("cena {} sem gravação do Go: rode com PATCHBAY_GRAVAR=1", self.cena));
        let mut erradas = Vec::new();
        let chaves: std::collections::BTreeSet<&String> = go.keys().chain(self.passos.keys()).collect();
        for passo in chaves {
            let (quer, veio) = (go.get(passo), self.passos.get(passo));
            if quer == veio {
                continue;
            }
            let nome = format!("{}/{passo}", self.cena);
            match DIFERENCAS_CONHECIDAS.iter().find(|(n, _)| *n == nome) {
                Some((_, porque)) => eprintln!("[diferença conhecida] {nome}: {porque}"),
                None => {
                    let mut d = Vec::new();
                    diferencas(quer.unwrap_or(&Value::Null), veio.unwrap_or(&Value::Null), &nome, 0, &mut d);
                    erradas.extend(d);
                }
            }
        }
        assert!(erradas.is_empty(), "{} passo(s) diferente(s) do Go:\n{}", erradas.len(), erradas.join("\n"));
    }
}

/// Onde dois valores diferem, descendo por objetos e listas até três níveis.
fn diferencas(quer: &Value, veio: &Value, caminho: &str, nivel: usize, saida: &mut Vec<String>) {
    if quer == veio {
        return;
    }
    match (quer, veio) {
        (Value::Object(a), Value::Object(b)) if nivel < 3 => {
            let chaves: std::collections::BTreeSet<&String> = a.keys().chain(b.keys()).collect();
            for k in chaves {
                diferencas(a.get(k).unwrap_or(&Value::Null), b.get(k).unwrap_or(&Value::Null), &format!("{caminho}.{k}"), nivel + 1, saida);
            }
        }
        (Value::Array(a), Value::Array(b)) if nivel < 3 && a.len() == b.len() => {
            for (i, (x, y)) in a.iter().zip(b).enumerate() {
                diferencas(x, y, &format!("{caminho}[{i}]"), nivel + 1, saida);
            }
        }
        _ => saida.push(format!("{caminho}:\n  Go:   {}\n  aqui: {}", resumir(Some(quer)), resumir(Some(veio)))),
    }
}

/// Codificação de URL dos caracteres reservados (`:`, `/`, `%`...), como num parâmetro de query.
fn codificar(s: &str) -> String {
    s.bytes()
        .map(|b| if b.is_ascii_alphanumeric() || b"-._~".contains(&b) { (b as char).to_string() } else { format!("%{b:02X}") })
        .collect()
}

/// Um valor numa linha, cortado para caber na mensagem.
fn resumir(v: Option<&Value>) -> String {
    let t = v.map_or("(ausente)".to_string(), Value::to_string);
    if t.len() > 2000 { format!("{}… ({} bytes)", &t[..t.floor_char_boundary(2000)], t.len()) } else { t }
}

// ---------- despejo do banco ----------

/// Tabelas fora do despejo: o catálogo da biblioteca (semente de 651 servidores, igual nos dois
/// binários por construção, e fatia própria, F13) e a sequência interna do SQLite.
fn tabela_fora(nome: &str) -> bool {
    nome.starts_with("biblioteca") || nome == "sqlite_sequence"
}

/// Todas as tabelas, linhas ordenadas, com instantes, hashes e cifras mascarados (o nonce do
/// AES-GCM e o sal do argon2 são aleatórios). Abre só para leitura; chame depois de parar.
pub fn despejar(banco: &Path) -> Value {
    let c = rusqlite::Connection::open_with_flags(banco, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY).expect("abrir o banco");
    let mut tabelas: Vec<String> = c
        .prepare("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
        .unwrap()
        .query_map([], |r| r.get(0))
        .unwrap()
        .map(Result::unwrap)
        .collect();
    tabelas.retain(|t| !tabela_fora(t));
    // A família de tokens do AS é um id aleatório; vira um rótulo pela ordem do primeiro token dela,
    // o que preserva quem é da mesma família.
    let mut familias: Vec<String> = Vec::new();
    if tabelas.iter().any(|t| t == "oauth_token") {
        let mut q = c.prepare("SELECT familia_id FROM oauth_token ORDER BY id").unwrap();
        for f in q.query_map([], |r| r.get::<_, String>(0)).unwrap().map(Result::unwrap) {
            if !familias.contains(&f) {
                familias.push(f);
            }
        }
    }
    let mut saida = serde_json::Map::new();
    for t in tabelas {
        let mut q = c.prepare(&format!("SELECT * FROM \"{t}\"")).unwrap();
        let colunas: Vec<String> = q.column_names().iter().map(|s| s.to_string()).collect();
        let mut linhas: Vec<Value> = q
            .query_map([], |r| {
                let mut m = serde_json::Map::new();
                for (i, col) in colunas.iter().enumerate() {
                    let v = match r.get_ref(i)? {
                        rusqlite::types::ValueRef::Null => Value::Null,
                        rusqlite::types::ValueRef::Integer(n) => json!(n),
                        rusqlite::types::ValueRef::Real(f) => json!(f),
                        rusqlite::types::ValueRef::Text(b) => json!(String::from_utf8_lossy(b)),
                        rusqlite::types::ValueRef::Blob(b) => json!(format!("«blob {} bytes»", b.len())),
                    };
                    let v = match v.as_str().and_then(|f| familias.iter().position(|x| x == f)) {
                        Some(n) if col == "familia_id" => json!(format!("«família {n}»")),
                        _ if col == "familia_id" => json!("«família sem token»"),
                        _ => mascarar_coluna(col, v),
                    };
                    m.insert(col.clone(), v);
                }
                Ok(Value::Object(m))
            })
            .unwrap()
            .map(Result::unwrap)
            .collect();
        linhas.sort_by_key(Value::to_string);
        saida.insert(t, Value::Array(linhas));
    }
    Value::Object(saida)
}

fn mascarar_coluna(col: &str, v: Value) -> Value {
    let vazio = v.is_null() || v == json!("") || v == json!(0);
    let instante = col.ends_with("_em") || col == "tstamp" || col == "ts";
    let segredo = col.contains("hash") || col.contains("cifrad") || v.as_str().is_some_and(|s| s.starts_with("pbc1:"));
    match () {
        // Duração muda a cada execução, inclusive entre 0 e 1 ms.
        _ if col == "duracao_ms" && !v.is_null() => json!("«ms»"),
        _ if vazio => v,
        _ if instante => json!("«instante»"),
        _ if segredo => json!("«secreto»"),
        _ => v,
    }
}

#[cfg(test)]
mod testes {
    use super::*;

    #[test]
    fn mascaras_e_trocas() {
        let mut g = Gravacao::new("teste");
        g.trocar("http://127.0.0.1:4321", "http://PATCHBAY");
        let t = g.texto("http://127.0.0.1:4321/x?d=http%3A%2F%2F127.0.0.1%3A4321&e=http%253A%252F%252F127.0.0.1%253A4321");
        assert_eq!(t, "http://PATCHBAY/x?d=http%3A%2F%2FPATCHBAY&e=http%253A%252F%252FPATCHBAY");
        assert_eq!(g.texto("pbk_abcd2345_AAAAAAAAAAAAAAAAAAAAAAAA e pbk_abcd2345"), "pbk_«chave» e pbk_«prefixo»");
        assert_eq!(g.texto("2026-11-27T19:43:10.2-03:00 em 27/11/2026 14:03"), "«instante» em «data» «hora»");
        // A revisão do protocolo não é instante.
        assert_eq!(g.texto("2026-07-28"), "2026-07-28");
        let v = g.normalizar(&json!({"client_id_issued_at": 1795819480, "expires_in": 3599, "n": 3}));
        assert_eq!(v, json!({"client_id_issued_at": "«instante»", "expires_in": "«segundos»", "n": 3}));
    }

    #[test]
    fn diferencas_apontam_o_caminho() {
        let mut d = Vec::new();
        diferencas(&json!({"a": [1, {"b": 2}], "c": 1}), &json!({"a": [1, {"b": 3}], "c": 1}), "cena/passo", 0, &mut d);
        assert_eq!(d.len(), 1, "{d:?}");
        assert!(d[0].starts_with("cena/passo.a[1].b:"), "{d:?}");
    }
}
