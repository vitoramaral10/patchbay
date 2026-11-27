//! O servidor MCP falso que fica atrás do patchbay: de verdade no protocolo (rmcp) e de mentira no
//! catálogo, como o `upstreamFalso` dos testes Go. Um mock escrito à mão concordaria com o nosso
//! erro de entendimento da MCP; o rmcp não.
//!
//! O mesmo código serve os upstreams HTTP (em processo, pela caixa-preta) e o upstream STDIO (o
//! binário `patchbay-upstream-falso`, que o patchbay executa). O [`Perfil`] decide o catálogo.
//!
//! As ferramentas são de propósito difíceis: nome com espaço, nome de 140 caracteres, anotação
//! `x-mcp-header`, esquema que não é objeto, `outputSchema`, imagem e áudio, resultado grande,
//! `isError`, erro de protocolo, elicitação por MRTR e uma que pendura. A lista muda em tempo de
//! execução ([`Falso::acrescentar`], [`Falso::remover`]) e avisa quem escuta por
//! `subscriptions/listen`.

#![allow(dead_code)] // o binário STDIO usa só uma parte

use std::borrow::Cow;
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use rmcp::model::{
    CallToolRequestParams, CallToolResponse, CallToolResult, ElicitRequest, ElicitRequestParams, ElicitationSchema, InputRequest,
    InputRequiredResult, ListToolsResult, PaginatedRequestParams, ProtocolVersion, ServerConfig, SubscriptionFilter, Tool,
};
use rmcp::service::{RequestContext, SubscriptionContext};
use rmcp::{ErrorData, RoleServer, ServerHandler};
use serde_json::{Value, json};
use tokio::sync::watch;

/// O nome com espaço que o SDK aceita e o protocolo não deveria: o patchbay normaliza.
pub const NOME_COM_ESPACO: &str = "eco com espaco";

/// 140 caracteres: passa do teto de 128 do Claude.
pub fn nome_longo() -> String {
    let base = "ferramenta_com_nome_comprido_";
    format!("{base}{}", "x".repeat(140 - base.len()))
}

/// Tamanho do texto da ferramenta `grande`.
pub const TAMANHO_GRANDE: usize = 300_000;

/// O catálogo e a revisão da MCP que o upstream fala.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Perfil {
    /// HTTP streamable sem sessão, só 2026-07-28; o catálogo inteiro.
    Moderno,
    /// HTTP streamable com sessão, só 2025-11-25; colide com o moderno em `somar`.
    Antigo,
    /// O processo local, em qualquer revisão que o patchbay pedir.
    Stdio,
    /// Atrás de OAuth (o provedor falso); uma ferramenta só.
    Protegido,
}

impl Perfil {
    fn nome(self) -> &'static str {
        match self {
            Perfil::Moderno => "upstream-falso-moderno",
            Perfil::Antigo => "upstream-falso-antigo",
            Perfil::Stdio => "upstream-falso-stdio",
            Perfil::Protegido => "upstream-falso-protegido",
        }
    }

    fn versoes(self) -> &'static [ProtocolVersion] {
        const MODERNA: &[ProtocolVersion] = &[ProtocolVersion::V_2026_07_28];
        const ANTIGA: &[ProtocolVersion] = &[ProtocolVersion::V_2025_11_25];
        match self {
            Perfil::Moderno | Perfil::Protegido => MODERNA,
            Perfil::Antigo => ANTIGA,
            Perfil::Stdio => ProtocolVersion::KNOWN_VERSIONS,
        }
    }
}

fn objeto() -> Value {
    json!({"type": "object"})
}

fn ferramenta(nome: &str, descricao: &str, entrada: Value) -> Value {
    json!({"name": nome, "description": descricao, "inputSchema": entrada})
}

/// O catálogo fixo de cada perfil, como o upstream o declara (antes da normalização do patchbay).
fn catalogo(perfil: Perfil) -> Vec<Value> {
    let somar = ferramenta(
        "somar",
        "soma dois inteiros",
        json!({"type": "object", "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}}, "required": ["a", "b"]}),
    );
    match perfil {
        Perfil::Moderno => vec![
            somar,
            ferramenta(NOME_COM_ESPACO, "devolve eco", objeto()),
            ferramenta(&nome_longo(), "nome de 140 caracteres", objeto()),
            ferramenta(
                "cabecalho_valido",
                "anotação x-mcp-header bem formada",
                json!({"type": "object", "properties": {"regiao": {"type": "string", "x-mcp-header": "X-Regiao"}}}),
            ),
            ferramenta(
                "cabecalho_invalido",
                "anotação x-mcp-header com nome de cabeçalho inválido",
                json!({"type": "object", "properties": {"regiao": {"type": "string", "x-mcp-header": "Regiao Invalida"}}}),
            ),
            ferramenta(
                "cabecalho_em_lista",
                "anotação x-mcp-header em tipo não primitivo",
                json!({"type": "object", "properties": {"lista": {"type": "array", "items": {"type": "string"}, "x-mcp-header": "X-Lista"}}}),
            ),
            ferramenta("esquema_ruim", "inputSchema que não é objeto", json!({"type": "string"})),
            json!({
                "name": "estruturada", "title": "Estruturada", "description": "resultado com outputSchema",
                "inputSchema": {"type": "object", "additionalProperties": false},
                "outputSchema": {"type": "object", "properties": {"total": {"type": "integer"}}, "required": ["total"]},
                "annotations": {"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
            }),
            ferramenta("midia", "imagem e áudio", objeto()),
            ferramenta("grande", "resultado de 300 kB", objeto()),
            ferramenta("falha", "isError de propósito", objeto()),
            ferramenta("erro_protocolo", "erro JSON-RPC de propósito", objeto()),
            ferramenta("eco_args", "devolve os argumentos e o _meta que chegaram", objeto()),
            ferramenta("apagar", "pede confirmação por elicitação quando o cliente sabe", objeto()),
            ferramenta("apagar_teimoso", "pede confirmação sempre", objeto()),
            ferramenta("pendurar", "nunca responde", objeto()),
        ],
        Perfil::Antigo => vec![somar, ferramenta("eco_antigo", "eco na 2025-11-25", objeto())],
        Perfil::Stdio => vec![
            ferramenta("stdio_eco", "eco pelo processo local", json!({"type": "object", "properties": {"texto": {"type": "string"}}})),
            ferramenta("stdio_ambiente", "nomes das variáveis de ambiente que o processo recebeu", objeto()),
        ],
        Perfil::Protegido => vec![ferramenta("protegida", "só com o token do provedor", objeto())],
    }
}

#[derive(Default)]
struct Mutavel {
    extras: Vec<String>,
    removidas: Vec<String>,
}

/// O servidor. Clonar compartilha o estado: o mesmo catálogo para todas as conexões.
#[derive(Clone)]
pub struct Falso {
    pub perfil: Perfil,
    mutavel: Arc<Mutex<Mutavel>>,
    aviso: Arc<watch::Sender<u64>>,
    /// Cada `tools/call` recebido: nome, argumentos, chaves do `_meta`, capacidades e revisão.
    pub chamadas: Arc<Mutex<Vec<Value>>>,
}

impl Falso {
    pub fn new(perfil: Perfil) -> Self {
        Falso { perfil, mutavel: Arc::default(), aviso: Arc::new(watch::Sender::new(0)), chamadas: Arc::default() }
    }

    /// Acrescenta uma ferramenta de eco e avisa quem escuta.
    pub fn acrescentar(&self, nome: &str) {
        self.mutavel.lock().unwrap().extras.push(nome.to_string());
        self.aviso.send_modify(|v| *v += 1);
    }

    /// Tira uma ferramenta do catálogo e avisa quem escuta.
    pub fn remover(&self, nome: &str) {
        self.mutavel.lock().unwrap().removidas.push(nome.to_string());
        self.aviso.send_modify(|v| *v += 1);
    }

    /// As chamadas recebidas até agora.
    pub fn chamadas(&self) -> Vec<Value> {
        self.chamadas.lock().unwrap().clone()
    }

    fn ferramentas(&self) -> Vec<Tool> {
        let m = self.mutavel.lock().unwrap();
        let extras = m.extras.iter().map(|n| ferramenta(n, "acrescentada depois", objeto()));
        catalogo(self.perfil)
            .into_iter()
            .chain(extras)
            .filter(|f| !m.removidas.iter().any(|r| f["name"] == r.as_str()))
            .map(|f| serde_json::from_value(f).expect("ferramenta do catálogo falso"))
            .collect()
    }
}

fn resultado(v: Value) -> Result<CallToolResponse, ErrorData> {
    Ok(CallToolResponse::Complete(serde_json::from_value::<CallToolResult>(v).expect("resultado do catálogo falso")))
}

fn texto(t: &str) -> Result<CallToolResponse, ErrorData> {
    resultado(json!({"content": [{"type": "text", "text": t}]}))
}

/// O pedido de confirmação do MRTR, com ou sem `requestState`.
fn perguntar(estado: Option<&str>) -> Result<CallToolResponse, ErrorData> {
    let pedido = ElicitRequestParams::FormElicitationParams {
        meta: None,
        message: "Apagar mesmo?".into(),
        requested_schema: ElicitationSchema::new(BTreeMap::new()),
    };
    let pedidos = BTreeMap::from([("confirmar".to_string(), InputRequest::Elicitation(ElicitRequest::new(pedido)))]);
    Ok(CallToolResponse::InputRequired(InputRequiredResult::new(Some(pedidos), estado.map(str::to_string))))
}

impl ServerHandler for Falso {
    fn get_info(&self) -> ServerConfig {
        serde_json::from_value(json!({
            "protocolVersion": self.perfil.versoes()[0].as_str(),
            "capabilities": {"tools": {"listChanged": true}},
            "serverInfo": {"name": self.perfil.nome(), "title": "Upstream falso", "version": "0.0.1"},
            "instructions": "upstream falso da caixa-preta do patchbay"
        }))
        .expect("serverInfo do falso")
    }

    fn supported_protocol_versions(&self) -> Cow<'static, [ProtocolVersion]> {
        Cow::Borrowed(self.perfil.versoes())
    }

    async fn list_tools(&self, _: Option<PaginatedRequestParams>, _: RequestContext<RoleServer>) -> Result<ListToolsResult, ErrorData> {
        Ok(ListToolsResult::with_all_items(self.ferramentas()))
    }

    fn accepted_subscription_filter(&self, pedido: &SubscriptionFilter) -> Option<SubscriptionFilter> {
        Some(pedido.clone())
    }

    async fn listen(&self, assinatura: SubscriptionContext) -> Result<(), ErrorData> {
        let mut mudou = self.aviso.subscribe();
        loop {
            tokio::select! {
                _ = assinatura.cancelled() => return Ok(()),
                r = mudou.changed() => {
                    if r.is_err() {
                        return Ok(());
                    }
                    let _ = assinatura.sink().notify_tool_list_changed().await;
                }
            }
        }
    }

    async fn call_tool(&self, pedido: CallToolRequestParams, contexto: RequestContext<RoleServer>) -> Result<CallToolResponse, ErrorData> {
        let capacidades = contexto.client_capabilities();
        let args = Value::Object(pedido.arguments.clone().unwrap_or_default());
        let mut meta: Vec<String> = contexto.meta.keys().cloned().collect();
        meta.sort();
        self.chamadas.lock().unwrap().push(json!({
            "nome": pedido.name,
            "argumentos": args,
            "meta": meta,
            "capacidades": capacidades,
            "revisao": contexto.protocol_version().map(|v| v.as_str().to_string()),
            "respostas": pedido.input_responses,
            "requestState": pedido.request_state,
        }));
        if !self.ferramentas().iter().any(|f| f.name == pedido.name) {
            return Err(ErrorData::invalid_params(format!("ferramenta desconhecida: {}", pedido.name), None));
        }
        match pedido.name.as_ref() {
            "somar" => {
                let n = |k: &str| args.get(k).and_then(Value::as_i64).unwrap_or(0);
                let soma = n("a") + n("b");
                // O antigo responde diferente: é o que mostra qual dos dois `somar` atendeu.
                if self.perfil == Perfil::Antigo { texto(&format!("antigo:{soma}")) } else { texto(&soma.to_string()) }
            }
            NOME_COM_ESPACO => texto("eco"),
            "estruturada" => resultado(json!({
                "content": [{"type": "text", "text": "{\"total\":42}"}],
                "structuredContent": {"total": 42}
            })),
            "midia" => resultado(json!({"content": [
                {"type": "image", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "mimeType": "image/png"},
                {"type": "audio", "data": "UklGRiQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA=", "mimeType": "audio/wav"},
                {"type": "text", "text": "legenda"}
            ]})),
            "grande" => {
                let linha = "0123456789abcdefghijklmnopqrstuvwxyz\n";
                texto(&linha.repeat(TAMANHO_GRANDE / linha.len() + 1)[..TAMANHO_GRANDE])
            }
            "falha" => resultado(json!({"content": [{"type": "text", "text": "deu errado de propósito"}], "isError": true})),
            "erro_protocolo" => Err(ErrorData::invalid_params("argumento recusado de propósito", None)),
            "eco_args" => texto(&json!({"argumentos": args, "meta": meta}).to_string()),
            "apagar" => {
                // Como os servidores do dono: cliente que não sabe confirmar segue como antes.
                if capacidades.and_then(|c| c.elicitation).is_none() {
                    return texto("sem-confirmacao");
                }
                match pedido.input_responses.as_ref().and_then(|r| r.get("confirmar")) {
                    Some(r) if pedido.request_state.as_deref() != Some("estado-1") => {
                        resultado(json!({"content": [{"type": "text", "text": format!("requestState perdido: {r}")}], "isError": true}))
                    }
                    Some(r) => texto(r.get("action").and_then(Value::as_str).unwrap_or("?")),
                    None => perguntar(Some("estado-1")),
                }
            }
            // Ignora a capacidade do cliente e pede sempre: é o caso da defesa do gateway.
            "apagar_teimoso" => perguntar(None),
            "pendurar" => {
                tokio::select! {
                    _ = contexto.ct.cancelled() => {}
                    _ = tokio::time::sleep(Duration::from_secs(120)) => {}
                }
                texto("acordou")
            }
            "stdio_eco" => texto(args.get("texto").and_then(Value::as_str).unwrap_or("")),
            "stdio_ambiente" => {
                let mut nomes: Vec<String> = std::env::vars_os().map(|(k, _)| k.to_string_lossy().into_owned()).collect();
                nomes.sort();
                texto(&nomes.join(","))
            }
            "protegida" => texto("ok com token"),
            "cabecalho_valido" | "cabecalho_invalido" | "cabecalho_em_lista" | "esquema_ruim" => texto(&args.to_string()),
            outra => texto(outra),
        }
    }
}
