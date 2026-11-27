//! Caixa-preta do patchbay: sobe o binário de `PATCHBAY_BIN` (padrão: o deste crate) como processo,
//! configura pelos subcomandos e pela tela de administração, põe upstreams falsos atrás dele (HTTP
//! 2026-07-28, HTTP 2025-11-25, SSE 2024-11-05, STDIO e um protegido por um provedor OAuth falso) e
//! fala com ele por clientes MCP rmcp reais nas revisões 2026-07-28 e 2025-11-25 e por HTTP cru.
//! Nunca toca o patchbay de produção, upstream real nem claude.ai.
//!
//! É o juiz de paridade do port: as respostas do Go ficam em `tests/respostas_go.json`, e toda
//! execução contra outro binário compara por valor, passo a passo.
//!
//! ```sh
//! PATCHBAY_BIN=/caminho/patchbay-go cargo test -j 2 --test caixa_preta                    # confere
//! PATCHBAY_BIN=/caminho/patchbay-go PATCHBAY_GRAVAR=1 cargo test -j 2 --test caixa_preta  # regrava
//! ```
//!
//! Na F0 o binário Rust ainda é um esboço: sem `PATCHBAY_BIN`, as cenas falham ao subir.
//! `PATCHBAY_MANTER=1` preserva o diretório de cada cena (banco e log) para inspeção.

mod apoio;

use std::time::{Duration, Instant};

use apoio::falsos::{self, Servidor, UpstreamHttp};
use apoio::{Cliente, Gravacao, Navegador, Patchbay, Usuario, conectar, despejar, rpc};
use base64::Engine;
use regex::Regex;
use rmcp::model::{CallToolRequestParams, ElicitationAction, SubscriptionFilter};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

/// Espera `cond` (uma expressão que pode ter `.await`) ficar verdadeira, sondando a cada 100 ms.
macro_rules! esperar {
    ($o_que:expr, $segundos:expr, $cond:expr) => {{
        let prazo = Instant::now() + Duration::from_secs($segundos);
        loop {
            if $cond {
                break;
            }
            assert!(Instant::now() < prazo, "esperei {} s por: {}", $segundos, $o_que);
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }};
}

const VERIFICADOR: &str = "verificador-de-teste-com-mais-de-quarenta-e-tres-caracteres";

fn desafio(verificador: &str) -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(Sha256::digest(verificador))
}

/// Chama uma ferramenta e devolve o resultado inteiro, ou o erro como texto.
async fn chamar(c: &Cliente, nome: &str, args: Value) -> Value {
    let mut p = CallToolRequestParams::new(nome.to_string());
    if let Value::Object(m) = args {
        p = p.with_arguments(m);
    }
    match c.call_tool(p).await {
        Ok(r) => json!({"resultado": r}),
        Err(e) => json!({"erro": e.to_string()}),
    }
}

/// As ferramentas como o cliente as vê.
async fn ferramentas(c: &Cliente) -> Vec<Value> {
    let lista = c.list_all_tools().await.expect("tools/list");
    lista.iter().map(|t| serde_json::to_value(t).unwrap()).collect()
}

fn nomes(lista: &[Value]) -> Vec<String> {
    lista.iter().map(|t| t["name"].as_str().unwrap_or("").to_string()).collect()
}

/// O nome exposto que termina com `original` (o patchbay pode prefixar).
fn exposto(lista: &[Value], original: &str) -> String {
    let n = nomes(lista);
    n.iter()
        .find(|x| x.as_str() == original)
        .or_else(|| n.iter().find(|x| x.ends_with(original)))
        .cloned()
        .unwrap_or_else(|| panic!("{original} fora de {n:?}"))
}

/// As trocas literais comuns: URL do patchbay, diretório da cena e o binário STDIO.
fn gravacao(cena: &'static str, pb: &Patchbay) -> Gravacao {
    let mut g = Gravacao::new(cena);
    g.trocar(pb.raiz.clone(), "http://PATCHBAY");
    g.trocar(pb.dir.display().to_string(), "DIR");
    g.trocar(apoio::bin_stdio(), "BIN_STDIO");
    g
}

// ---------- o cenário padrão, montado pela tela ----------

/// Um patchbay configurado só pela tela: admin, quatro MCPs (HTTP 2026, HTTP 2025, SSE e STDIO, que
/// entram sozinhos no endpoint `principal` criado no primeiro boot), um endpoint `trabalho` vazio e
/// uma chave com escopo em `principal`.
struct Padrao {
    pb: Patchbay,
    nav: Navegador,
    u26: UpstreamHttp,
    u25: UpstreamHttp,
    _sse: Servidor,
    chave: String,
    ups: Vec<(&'static str, i64)>,
    trabalho: i64,
    g: Gravacao,
}

async fn padrao(cena: &'static str) -> Padrao {
    let (u26, u25, sse) = (falsos::http_moderno().await, falsos::http_antigo().await, falsos::sse_legado().await);
    let pb = Patchbay::subir().await;
    let mut g = gravacao(cena, &pb);
    g.trocar(u26.servidor.base.clone(), "http://U26");
    g.trocar(u25.servidor.base.clone(), "http://U25");
    g.trocar(sse.base.clone(), "http://SSE");
    let mut nav = Navegador::new(&pb.raiz);
    nav.setup().await;
    let sse_url = format!("{}/sse", sse.base);
    let mut ups = Vec::new();
    for (nome, tipo, url) in [("u26", "http", u26.url()), ("u25", "http", u25.url()), ("sse", "sse", sse_url)] {
        let id = nav
            .criar(
                "/admin/mcps",
                &[("nome", nome), ("tipo", tipo), ("url", &url), ("timeout_ms", "4000"), ("habilitado", "1"), ("modo", "nenhum")],
            )
            .await;
        ups.push((nome, id));
    }
    let id = nav
        .criar(
            "/admin/mcps",
            &[("nome", "local"), ("tipo", "stdio"), ("comando", apoio::bin_stdio()), ("timeout_ms", "4000"), ("habilitado", "1")],
        )
        .await;
    ups.push(("local", id));
    let trabalho = nav.criar("/admin/endpoints", &[("slug", "trabalho"), ("nome", "Trabalho")]).await;
    let chave = nav.criar_chave("teste", &[1]).await;
    // Os quatro no ar: uma ferramenta de cada no catálogo de `principal`.
    let url = pb.mcp("principal");
    let c = conectar(&url, &chave, &Usuario::new()).await;
    esperar!("as ferramentas dos quatro upstreams", 30, {
        let n = nomes(&ferramentas(&c).await);
        ["pendurar", "eco_antigo", "sse_eco", "stdio_eco"].iter().all(|m| n.iter().any(|x| x.ends_with(m)))
    });
    Padrao { pb, nav, u26, u25, _sse: sse, chave, ups, trabalho, g }
}

/// tools/list e tools/call de cada tipo de ferramenta difícil, pelo cliente 2026-07-28; as telas
/// de status; a sessão que não atravessa endpoint; o que o upstream recebeu; o banco no fim.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn superficie_e_chamadas() {
    let mut p = padrao("superficie").await;
    let c = conectar(&p.pb.mcp("principal"), &p.chave, &Usuario::new()).await;
    let lista = ferramentas(&c).await;
    p.g.anotar("tools_list", json!(lista));
    if apoio::gravando() {
        let arquivo = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/superficie.json");
        std::fs::write(arquivo, serde_json::to_string_pretty(&p.g.normalizar(&json!(lista))).unwrap() + "\n").unwrap();
    }
    let n = nomes(&lista);
    // Os dois `somar` (u26 e u25) e o que sobrou do nome longo, como o patchbay os expôs.
    p.g.anotar("nomes_somar", json!(n.iter().filter(|x| x.contains("somar")).collect::<Vec<_>>()));
    for nome in n.iter().filter(|x| x.contains("somar")) {
        let r = chamar(&c, nome, json!({"a": 2, "b": 40})).await;
        p.g.anotar(&format!("chamada/{nome}"), r);
    }
    let casos = [
        ("eco_com_espaco", json!({})),
        ("cabecalho_valido", json!({"regiao": "sul"})),
        ("cabecalho_invalido", json!({"regiao": "sul"})),
        ("cabecalho_em_lista", json!({"lista": ["a"]})),
        ("esquema_ruim", json!({"x": 1})),
        ("estruturada", json!({})),
        ("midia", json!({})),
        ("falha", json!({})),
        ("erro_protocolo", json!({})),
        ("eco_args", json!({"k": "v"})),
        ("sse_eco", json!({"texto": "oi"})),
        ("stdio_eco", json!({"texto": "oi"})),
        ("stdio_ambiente", json!({})),
    ];
    for (original, args) in casos {
        // A ferramenta que o patchbay não expôs fica registrada como ausente.
        let r = match nomes(&lista).iter().find(|x| x.ends_with(original)) {
            Some(nome) => chamar(&c, nome, args).await,
            None => json!({"ausente": true}),
        };
        p.g.anotar(&format!("chamada/{original}"), r);
    }
    let longo = n.iter().find(|x| x.contains("ferramenta_com_nome_comprido")).cloned().expect("o nome longo some?");
    p.g.anotar("chamada/nome_longo", chamar(&c, &longo, json!({})).await);
    // O resultado grande: tamanho e pontas, não os 300 kB.
    let r = chamar(&c, &exposto(&lista, "grande"), json!({})).await;
    let t = r["resultado"]["content"][0]["text"].as_str().unwrap_or("");
    p.g.anotar("chamada/grande", json!({"bytes": t.len(), "inicio": &t[..t.len().min(40)], "fim": &t[t.len().saturating_sub(40)..]}));
    // A ferramenta que pendura cai no timeout do upstream (4 s), sem derrubar o resto.
    let inicio = Instant::now();
    let r = chamar(&c, &exposto(&lista, "pendurar"), json!({})).await;
    let levou = inicio.elapsed();
    assert!(levou < Duration::from_secs(15), "pendurar levou {levou:?}");
    p.g.anotar("chamada/pendurar", r);
    p.g.anotar("chamada/inexistente", chamar(&c, "nao_existe", json!({})).await);
    p.g.anotar("chamada/depois_do_timeout", chamar(&c, &exposto(&lista, "u26_somar"), json!({"a": 1, "b": 1})).await);

    // O que o upstream 2026 recebeu (nome original, argumentos, chaves do _meta, capacidades).
    p.g.anotar("u26_recebeu", json!(p.u26.falso.chamadas()));
    p.g.anotar("u25_recebeu", json!(p.u25.falso.chamadas()));

    // HTTP cru: status, tipo e a ausência de Mcp-Session-Id (o endpoint é sem sessão).
    let corpo = json!({"jsonrpc": "2.0", "id": 7, "method": "tools/list", "params": {"_meta": apoio::meta_2026()}});
    let r = rpc(
        &p.pb.mcp("principal"),
        Some(&format!("Bearer {}", p.chave)),
        Some(corpo),
        &[("MCP-Protocol-Version", "2026-07-28"), ("Mcp-Method", "tools/list")],
    )
    .await;
    p.g.anotar("cru/tools_list", json!({"status": r.status, "content-type": r.cabecalho("content-type"), "mcp-session-id": r.cabecalho("mcp-session-id"), "quantas": r.json()["result"]["tools"].as_array().map(Vec::len)}));
    let discover = json!({"jsonrpc": "2.0", "id": 8, "method": "server/discover", "params": {"_meta": apoio::meta_2026()}});
    let r = rpc(
        &p.pb.mcp("principal"),
        Some(&format!("Bearer {}", p.chave)),
        Some(discover),
        &[("MCP-Protocol-Version", "2026-07-28"), ("Mcp-Method", "server/discover")],
    )
    .await;
    p.g.anotar("cru/server_discover", r.resumo(&["content-type", "mcp-session-id"]));

    // A sessão de um endpoint não abre outro (TestUI_SessaoNaoAtravessaEndpoint): com uma chave das
    // duas, `trabalho` responde o próprio catálogo, vazio, mesmo com o id de sessão de `principal`.
    let das_duas = p.nav.criar_chave("as-duas", &[1, p.trabalho]).await;
    let corpo = json!({"jsonrpc": "2.0", "id": 2, "method": "tools/list"});
    let r =
        rpc(&p.pb.mcp("trabalho"), Some(&format!("Bearer {das_duas}")), Some(corpo), &[("Mcp-Session-Id", "sessao-de-principal")]).await;
    assert!(!r.corpo.contains("somar"), "trabalho trouxe ferramenta de principal: {}", r.corpo);
    p.g.anotar("sessao_nao_atravessa", r.resumo(&["content-type", "mcp-session-id"]));

    // As telas de status: painel, MCPs, cada MCP, endpoints, chaves, trilha.
    let mut telas =
        vec!["/admin/".to_string(), "/admin/mcps".into(), "/admin/endpoints".into(), "/admin/endpoints/1".into(), "/admin/chaves".into()];
    telas.extend(p.ups.iter().map(|(_, id)| format!("/admin/mcps/{id}")));
    for tela in telas {
        let r = p.nav.get(&tela).await;
        assert_eq!(r.status, 200, "{tela}: {}", r.corpo);
        p.g.anotar_html(&format!("tela{tela}"), &r.corpo);
    }

    drop(c);
    p.pb.parar();
    p.g.anotar("banco", despejar(&p.pb.banco()));
    p.g.conferir();
}

/// O mesmo cenário por um cliente 2025-11-25 (`initialize`): o que ele vê e o que consegue chamar.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cliente_2025_11_25() {
    let mut p = padrao("cliente_2025_11_25").await;
    let c = conectar(&p.pb.mcp("principal"), &p.chave, &Usuario::new().antigo()).await;
    p.g.anotar("server_info", serde_json::to_value(c.peer_info()).unwrap());
    let lista = ferramentas(&c).await;
    p.g.anotar("nomes", json!(nomes(&lista)));
    for (original, args) in
        [("eco_antigo", json!({})), ("sse_eco", json!({"texto": "x"})), ("stdio_eco", json!({"texto": "y"})), ("estruturada", json!({}))]
    {
        let r = chamar(&c, &exposto(&lista, original), args).await;
        p.g.anotar(&format!("chamada/{original}"), r);
    }
    // initialize cru: a resposta e se volta Mcp-Session-Id.
    let init = json!({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
        "protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "caixa-preta", "version": "0"}}});
    let r = rpc(&p.pb.mcp("principal"), Some(&format!("Bearer {}", p.chave)), Some(init), &[]).await;
    p.g.anotar("cru/initialize", r.resumo(&["content-type", "mcp-session-id"]));
    p.g.conferir();
}

// ---------- cenas com `seed` (as de cmd/patchbay/integracao_test.go) ----------

/// `seed` e `serve` com um upstream HTTP; devolve o patchbay no ar e a chave emitida.
async fn semeado(cena: &'static str, upstream_url: &str, timeout_ms: &str) -> (Patchbay, String, Gravacao) {
    let mut pb = Patchbay::preparar();
    let s = pb
        .comando(&[
            "seed",
            "-endpoint",
            "pessoal",
            "-upstream",
            "falso",
            "-upstream-url",
            upstream_url,
            "-nome-chave",
            "teste",
            "-timeout-ms",
            timeout_ms,
        ])
        .await;
    assert_eq!(s.codigo, Some(0), "seed: {s:?}");
    let chave =
        Regex::new(r"pbk_[a-z0-9]+_[A-Za-z0-9_-]{20,}").unwrap().find(&s.stdout).expect("chave no stdout do seed").as_str().to_string();
    let mut g = gravacao(cena, &pb);
    g.trocar(upstream_url.trim_end_matches("/mcp"), "http://UPSTREAM");
    g.anotar("seed", json!({"codigo": s.codigo, "stdout": s.stdout}));
    pb.iniciar().await;
    (pb, chave, g)
}

/// A borda de autenticação por HTTP cru (TestAutenticacaoNoEndpoint, TestCredencialEmQueryString),
/// a metadata RFC 9728 anunciada no desafio e `/saude`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn autenticacao_no_endpoint() {
    let u = falsos::http_moderno().await;
    let mut pb = Patchbay::preparar();
    // O segundo endpoint, sem upstream nenhum, entra por `import` antes do primeiro boot.
    let yaml = pb.dir.join("trabalho.yaml");
    std::fs::write(&yaml, "versao: 1\nendpoints:\n  - slug: trabalho\n    nome: Trabalho\n").unwrap();
    let s = pb
        .comando(&[
            "seed",
            "-endpoint",
            "pessoal",
            "-upstream",
            "falso",
            "-upstream-url",
            &u.url(),
            "-nome-chave",
            "teste",
            "-timeout-ms",
            "5000",
        ])
        .await;
    assert_eq!(s.codigo, Some(0), "seed: {s:?}");
    let chave = Regex::new(r"pbk_[a-z0-9]+_[A-Za-z0-9_-]{20,}").unwrap().find(&s.stdout).unwrap().as_str().to_string();
    let i = pb.comando(&["import", yaml.to_str().unwrap()]).await;
    let mut g = gravacao("autenticacao", &pb);
    g.trocar(u.servidor.base.clone(), "http://UPSTREAM");
    g.anotar("import", json!({"codigo": i.codigo, "stdout": i.stdout, "stderr": i.stderr}));
    pb.iniciar().await;
    let bearer = format!("Bearer {chave}");
    let corpo = json!({"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {"_meta": apoio::meta_2026()}});
    let cabecalhos = ["www-authenticate", "content-type", "mcp-session-id"];
    let casos: [(&str, &str, Option<&str>); 5] = [
        ("sem_chave", "pessoal", None),
        ("chave_inventada", "pessoal", Some("Bearer pbk_aaaabbbb_chave-que-nunca-foi-emitida")),
        ("esquema_basic", "pessoal", Some("Basic dXNlcjpzZW5oYQ==")),
        ("sem_escopo", "trabalho", Some(&bearer)),
        ("endpoint_inexistente", "inexistente", Some(&bearer)),
    ];
    for (nome, slug, auth) in casos {
        let r =
            rpc(&pb.mcp(slug), auth, Some(corpo.clone()), &[("MCP-Protocol-Version", "2026-07-28"), ("Mcp-Method", "tools/list")]).await;
        g.anotar(&format!("http/{nome}"), r.resumo(&cabecalhos));
    }
    let r = rpc(
        &pb.mcp("pessoal"),
        Some(&bearer),
        Some(corpo.clone()),
        &[("MCP-Protocol-Version", "2026-07-28"), ("Mcp-Method", "tools/list")],
    )
    .await;
    assert_eq!(r.status, 200, "{}", r.corpo);
    g.anotar("http/chave_certa", json!({"status": r.status, "mcp-session-id": r.cabecalho("mcp-session-id"), "nomes": r.json()["result"]["tools"].as_array().map(|a| a.iter().map(|t| t["name"].clone()).collect::<Vec<_>>())}));
    for param in ["api_key", "apikey", "key", "token", "access_token"] {
        let url = format!("{}?{param}={chave}", pb.mcp("pessoal"));
        let r = rpc(&url, None, None, &[]).await;
        g.anotar(&format!("query/{param}"), r.resumo(&cabecalhos));
    }
    let r = reqwest::get(format!("{}/saude", pb.raiz)).await.unwrap();
    g.anotar("saude", json!({"status": r.status().as_u16(), "content-type": r.headers().get("content-type").map(|v| v.to_str().unwrap().to_string()), "corpo": r.text().await.unwrap()}));
    for caminho in [
        "/.well-known/oauth-protected-resource/mcp/pessoal",
        "/mcp/pessoal/.well-known/oauth-protected-resource",
        "/.well-known/oauth-protected-resource",
    ] {
        let r = reqwest::get(format!("{}{caminho}", pb.raiz)).await.unwrap();
        let status = r.status().as_u16();
        let corpo: Value = r.json().await.unwrap_or(Value::Null);
        g.anotar(&format!("metadata{caminho}"), json!({"status": status, "corpo": corpo}));
    }
    g.conferir();
}

/// Endpoint sem upstream pronto devolve tools/list vazio, nunca erro, e em menos de 1 s
/// (TestUpstreamForaDoAr_CatalogoVazio).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn upstream_fora_do_ar_catalogo_vazio() {
    let pendura = falsos::que_pendura().await;
    let (pb, chave, mut g) = semeado("fora_do_ar", &format!("{}/mcp", pendura.base), "5000").await;
    let c = conectar(&pb.mcp("pessoal"), &chave, &Usuario::new()).await;
    let inicio = Instant::now();
    let lista =
        tokio::time::timeout(Duration::from_secs(1), c.list_all_tools()).await.expect("tools/list levou mais de 1 s").expect("tools/list");
    assert!(lista.is_empty(), "{lista:?}");
    assert!(inicio.elapsed() < Duration::from_secs(1));
    g.anotar("tools_list", json!(lista));
    g.conferir();
}

/// Upstream sem sessão que ganha e perde ferramenta depois de conectado: o patchbay ouve o aviso,
/// relista e avisa o cliente por `subscriptions/listen` (TestFerramentaNovaDoUpstreamChegaAoCliente);
/// a ferramenta que some fica como lápide, listada e respondendo o texto do Go.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn lista_alterada_e_lapide() {
    let u = falsos::http_moderno().await;
    let (pb, chave, mut g) = semeado("lista_alterada", &u.url(), "5000").await;
    let c = conectar(&pb.mcp("pessoal"), &chave, &Usuario::new()).await;
    esperar!("o catálogo inicial", 15, nomes(&ferramentas(&c).await).contains(&"somar".to_string()));
    let mut assinatura = c.listen(SubscriptionFilter::builder().tools_list_changed().build()).await.expect("subscriptions/listen");
    g.anotar("listen/aceito", serde_json::to_value(assinatura.acknowledged()).unwrap());
    // A assinatura do patchbay no upstream sai junto com a dele; um instante antes de mudar a lista.
    tokio::time::sleep(Duration::from_millis(300)).await;
    u.falso.acrescentar("nova");
    let aviso = tokio::time::timeout(Duration::from_secs(15), assinatura.next()).await.expect("o cliente não recebeu tools/list_changed");
    g.anotar("listen/aviso", serde_json::to_value(aviso.expect("aviso").expect("aviso")).unwrap());
    esperar!("a ferramenta nova", 15, nomes(&ferramentas(&c).await).contains(&"nova".to_string()));
    g.anotar("chamada/nova", chamar(&c, "nova", json!({})).await);

    u.falso.remover("somar");
    let _ = tokio::time::timeout(Duration::from_secs(15), assinatura.next()).await.expect("sem aviso da remoção");
    let mut lapide = Value::Null;
    esperar!("a lápide de somar", 15, {
        let l = ferramentas(&c).await;
        match l.iter().find(|t| t["name"] == "somar") {
            Some(t) if !t["description"].as_str().unwrap_or("").contains("soma dois inteiros") => {
                lapide = t.clone();
                true
            }
            _ => false,
        }
    });
    g.anotar("lapide/listada", lapide);
    g.anotar("lapide/chamada", chamar(&c, "somar", json!({"a": 1, "b": 2})).await);
    g.anotar("upstream_recebeu", json!(u.falso.chamadas()));
    g.conferir();
}

/// O repasse da elicitação por MRTR (TestElicitacaoAtravessaOGateway): aceite e recusa chegam ao
/// upstream com o `requestState`; cliente 2025-11-25 ou sem elicitação não é perguntado; upstream
/// que pede mesmo assim recebe a recusa do gateway.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn elicitacao_atravessa_o_gateway() {
    let u = falsos::http_moderno().await;
    let (pb, chave, mut g) = semeado("elicitacao", &u.url(), "5000").await;
    {
        let c = conectar(&pb.mcp("pessoal"), &chave, &Usuario::new()).await;
        esperar!("o catálogo", 15, nomes(&ferramentas(&c).await).contains(&"apagar".to_string()));
    }
    let casos = [
        ("aceita", Usuario::respondendo(ElicitationAction::Accept), "apagar"),
        ("recusa", Usuario::respondendo(ElicitationAction::Decline), "apagar"),
        ("cancela", Usuario::respondendo(ElicitationAction::Cancel), "apagar"),
        ("cliente_2025_11_25", Usuario::respondendo(ElicitationAction::Accept).antigo(), "apagar"),
        ("cliente_sem_elicitacao", Usuario::new(), "apagar"),
        ("teimoso_sem_elicitacao", Usuario::new(), "apagar_teimoso"),
        ("teimoso_com_elicitacao", Usuario::respondendo(ElicitationAction::Accept), "apagar_teimoso"),
    ];
    for (nome, usuario, ferramenta) in casos {
        let c = conectar(&pb.mcp("pessoal"), &chave, &usuario).await;
        let antes = u.falso.chamadas().len();
        let r = chamar(&c, ferramenta, json!({})).await;
        g.anotar(&format!("{nome}/resultado"), r);
        g.anotar(&format!("{nome}/perguntas"), json!(usuario.perguntas.lock().unwrap().clone()));
        g.anotar(&format!("{nome}/upstream_recebeu"), json!(u.falso.chamadas()[antes..]));
    }
    // O input_required cru, como o cliente 2026-07-28 o recebe.
    let caps = json!({"elicitation": {"form": {}}});
    let mut meta = apoio::meta_2026();
    meta["io.modelcontextprotocol/clientCapabilities"] = caps;
    let corpo = json!({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "apagar", "_meta": meta}});
    let r = rpc(
        &pb.mcp("pessoal"),
        Some(&format!("Bearer {chave}")),
        Some(corpo),
        &[("MCP-Protocol-Version", "2026-07-28"), ("Mcp-Method", "tools/call"), ("Mcp-Name", "apagar")],
    )
    .await;
    g.anotar("cru/input_required", r.resumo(&["content-type"]));
    g.conferir();
}

// ---------- administração ----------

/// O portão da tela: primeiro acesso, CSRF pelo Sec-Fetch-Site e Origin (TestUI_CSRF), rota
/// protegida sem sessão, login errado e certo, sair, estáticos.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn telas_de_acesso_e_csrf() {
    let pb = Patchbay::subir().await;
    let mut g = gravacao("acesso", &pb);
    let mut nav = Navegador::new(&pb.raiz);
    let r = nav.get("/admin/").await;
    g.anotar("antes_do_setup/painel", r.resumo(&["location"]));
    let r = nav.get("/admin/setup").await;
    g.anotar_html("antes_do_setup/tela_setup", &r.corpo);
    let campos = [("usuario", apoio::USUARIO), ("senha", apoio::SENHA), ("confirmar", apoio::SENHA)];
    for site in ["cross-site", "same-site"] {
        let r =
            nav.pedir(reqwest::Method::POST, "/admin/setup", Some(&campos), Some(site), Some("https://site-do-atacante.invalido")).await;
        g.anotar(&format!("csrf/{site}"), json!({"status": r.status, "corpo": r.corpo.trim()}));
    }
    let r = nav.post("/admin/setup", &[("usuario", apoio::USUARIO), ("senha", "curta"), ("confirmar", "curta")]).await;
    g.anotar("setup/senha_curta", json!({"status": r.status}));
    g.anotar_html("setup/senha_curta_tela", &r.corpo);
    let r = nav.setup().await;
    g.anotar("setup/ok", r.resumo(&["location"]));
    let r = nav.get("/admin/setup").await;
    g.anotar("depois_do_setup/tela_setup", r.resumo(&["location"]));
    let r = nav.get("/admin/").await;
    g.anotar_html("painel_vazio", &r.corpo);
    let r = nav.post("/admin/sair", &[]).await;
    g.anotar("sair", json!({"status": r.status, "location": r.local(), "sessao": nav.cookie}));
    for rota in ["/admin/", "/admin/mcps", "/admin/chaves", "/admin/trilha"] {
        let r = nav.get(rota).await;
        g.anotar(&format!("sem_sessao{rota}"), r.resumo(&["location"]));
    }
    let r = nav.get("/admin/login?destino=%2Fadmin%2Fmcps").await;
    g.anotar_html("tela_login", &r.corpo);
    let r = nav.post("/admin/login", &[("usuario", apoio::USUARIO), ("senha", "senha-errada-de-doze"), ("destino", "/admin/mcps")]).await;
    g.anotar("login/errado", json!({"status": r.status, "sessao": nav.cookie.is_some()}));
    let r = nav.post("/admin/login", &[("usuario", apoio::USUARIO), ("senha", apoio::SENHA), ("destino", "/admin/mcps")]).await;
    g.anotar("login/certo", json!({"status": r.status, "location": r.local(), "sessao": nav.cookie.is_some()}));
    let r = nav.get("/admin/mcps").await;
    g.anotar_html("tela_mcps_vazia", &r.corpo);
    // Os estáticos que o layout referencia.
    let css = Regex::new(r#"/static/[^"']+"#).unwrap();
    let mut estaticos: Vec<String> = css.find_iter(&r.corpo).map(|m| m.as_str().to_string()).collect();
    estaticos.sort();
    estaticos.dedup();
    for e in estaticos {
        let resp = reqwest::get(format!("{}{e}", pb.raiz)).await.unwrap();
        let tipo = resp.headers().get("content-type").map(|v| v.to_str().unwrap().to_string());
        let status = resp.status().as_u16();
        let bytes = resp.bytes().await.unwrap();
        g.anotar(
            &format!("estatico{e}"),
            json!({"status": status, "content-type": tipo, "sha256": format!("{:x}", Sha256::digest(&bytes))}),
        );
    }
    g.conferir();
}

// ---------- authorization server ----------

/// O AS do patchbay de ponta a ponta com DCR (TestDCRPontaAPonta e vizinhos): desafio 401,
/// metadata, registro, consentimento pela sessão de admin, troca do code com PKCE, chamada com o
/// token, rotação do refresh com detecção de reuso, code reusado, revogação, recusas do DCR e as
/// recusas de CIMD que dá para provocar sem rede (o documento CIMD exige https público).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn oauth_do_as_dcr_e_cimd() {
    let u = falsos::http_moderno().await;
    let (pb, _chave, mut g) = semeado("oauth_as", &u.url(), "5000").await;
    let mut nav = Navegador::new(&pb.raiz);
    nav.setup().await;
    let recurso = pb.mcp("pessoal");
    let ch = desafio(VERIFICADOR);

    let r = rpc(&recurso, None, Some(json!({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})), &[]).await;
    g.anotar("desafio", r.resumo(&["www-authenticate"]));
    for caminho in [
        "/.well-known/oauth-authorization-server",
        "/.well-known/oauth-authorization-server/mcp/pessoal",
        "/.well-known/openid-configuration",
        "/mcp/pessoal/.well-known/openid-configuration",
    ] {
        let r = nav.get(caminho).await;
        g.anotar(&format!("metadata{caminho}"), r.resumo(&["content-type"]));
    }

    // DCR: recusas e o registro bom.
    let http = reqwest::Client::builder().redirect(reqwest::redirect::Policy::none()).build().unwrap();
    let registrar = format!("{}/oauth/register", pb.raiz);
    let recusas = [
        ("sem_redirect", json!({"client_name": "sem redirect"})),
        ("http_fora_de_loopback", json!({"redirect_uris": ["http://mau.test/cb"]})),
        ("fragmento", json!({"redirect_uris": ["https://mau.test/cb#x"]})),
        ("grant_desconhecido", json!({"redirect_uris": ["https://ok.test/cb"], "grant_types": ["client_credentials"]})),
    ];
    for (nome, corpo) in recusas {
        let r = http.post(&registrar).json(&corpo).send().await.unwrap();
        let status = r.status().as_u16();
        g.anotar(&format!("dcr/recusa/{nome}"), json!({"status": status, "corpo": r.json::<Value>().await.unwrap_or(Value::Null)}));
    }
    let volta = "http://127.0.0.1:43210/callback";
    let pedido = json!({
        "client_name": "cliente DCR da caixa-preta",
        "redirect_uris": [volta, "http://localhost:43210/callback"],
        "token_endpoint_auth_method": "none",
        "grant_types": ["authorization_code", "refresh_token"],
        "response_types": ["code"]
    });
    let r = http.post(&registrar).json(&pedido).send().await.unwrap();
    assert_eq!(r.status().as_u16(), 201);
    let registro: Value = r.json().await.unwrap();
    let client_id = registro["client_id"].as_str().expect("client_id").to_string();
    g.trocar(client_id.clone(), "«client_id»");
    g.anotar("dcr/registro", registro);

    // Autorização: sem sessão vai para o login; com sessão mostra o consentimento.
    let q = [
        ("client_id", client_id.as_str()),
        ("redirect_uri", volta),
        ("response_type", "code"),
        ("code_challenge", ch.as_str()),
        ("code_challenge_method", "S256"),
        ("resource", recurso.as_str()),
        ("state", "estado-1"),
    ];
    let consulta = serde_urlencoded(&q);
    let cookie = nav.cookie.take();
    let r = nav.get(&format!("/oauth/authorize?{consulta}")).await;
    g.anotar("authorize/sem_sessao", r.resumo(&["location"]));
    nav.cookie = cookie;
    let r = nav.get(&format!("/oauth/authorize?{consulta}")).await;
    assert_eq!(r.status, 200, "{}", r.corpo);
    g.anotar_html("authorize/consentimento", &r.corpo);
    let mut decisao: Vec<(&str, &str)> = q.to_vec();
    decisao.push(("decisao", "aceitar"));
    let r = nav.post("/oauth/authorize", &decisao).await;
    let local = r.local().expect("Location do consentimento");
    g.anotar("authorize/aceito", json!({"status": r.status, "location": local}));
    let codigo = reqwest::Url::parse(&local).unwrap().query_pairs().find(|(k, _)| k == "code").map(|(_, v)| v.into_owned()).expect("code");

    let token = format!("{}/oauth/token", pb.raiz);
    let trocar = |form: Vec<(&'static str, String)>| {
        let http = http.clone();
        let token = token.clone();
        async move {
            let r = http.post(&token).form(&form).send().await.unwrap();
            (r.status().as_u16(), r.json::<Value>().await.unwrap_or(Value::Null))
        }
    };
    let form_codigo = vec![
        ("grant_type", "authorization_code".to_string()),
        ("code", codigo.clone()),
        ("redirect_uri", volta.to_string()),
        ("code_verifier", VERIFICADOR.to_string()),
        ("client_id", client_id.clone()),
        ("resource", recurso.clone()),
    ];
    let (status, par) = trocar(form_codigo.clone()).await;
    g.anotar("token/codigo", json!({"status": status, "corpo": par}));
    let acesso = par["access_token"].as_str().expect("access_token").to_string();
    let refresh = par["refresh_token"].as_str().expect("refresh_token").to_string();

    let c = conectar(&recurso, &acesso, &Usuario::new()).await;
    g.anotar("mcp_com_token/nomes", json!(nomes(&ferramentas(&c).await)));
    g.anotar("mcp_com_token/somar", chamar(&c, "somar", json!({"a": 20, "b": 22})).await);
    drop(c);
    let r = rpc(&pb.mcp("outro"), Some(&format!("Bearer {acesso}")), Some(json!({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})), &[])
        .await;
    g.anotar("token_em_endpoint_inexistente", json!({"status": r.status}));

    let (status, corpo) = trocar(form_codigo).await;
    g.anotar("token/codigo_reusado", json!({"status": status, "corpo": corpo}));
    let r = rpc(&recurso, Some(&format!("Bearer {acesso}")), Some(json!({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})), &[]).await;
    g.anotar("token/acesso_depois_do_reuso", r.resumo(&["www-authenticate"]));

    // Um segundo consentimento, para a rotação do refresh.
    let r = nav.post("/oauth/authorize", &decisao).await;
    let local = r.local().expect("Location do segundo consentimento");
    let codigo = reqwest::Url::parse(&local).unwrap().query_pairs().find(|(k, _)| k == "code").map(|(_, v)| v.into_owned()).unwrap();
    let (_, par) = trocar(vec![
        ("grant_type", "authorization_code".into()),
        ("code", codigo),
        ("redirect_uri", volta.into()),
        ("code_verifier", VERIFICADOR.into()),
        ("client_id", client_id.clone()),
        ("resource", recurso.clone()),
    ])
    .await;
    let refresh2 = par["refresh_token"].as_str().expect("refresh").to_string();
    let form_refresh =
        |rt: &str| vec![("grant_type", "refresh_token".to_string()), ("refresh_token", rt.to_string()), ("client_id", client_id.clone())];
    let (status, rotacionado) = trocar(form_refresh(&refresh2)).await;
    g.anotar("refresh/rotacao", json!({"status": status, "corpo": rotacionado, "mudou": rotacionado["refresh_token"] != json!(refresh2)}));
    let (status, corpo) = trocar(form_refresh(&refresh2)).await;
    g.anotar("refresh/reuso", json!({"status": status, "corpo": corpo}));
    let novo = rotacionado["refresh_token"].as_str().unwrap_or("").to_string();
    let (status, corpo) = trocar(form_refresh(&novo)).await;
    g.anotar("refresh/familia_depois_do_reuso", json!({"status": status, "corpo": corpo}));
    let (status, corpo) = trocar(form_refresh(&refresh)).await;
    g.anotar("refresh/da_familia_revogada_pelo_code", json!({"status": status, "corpo": corpo}));

    let r = http
        .post(format!("{}/oauth/revoke", pb.raiz))
        .form(&[("token", "pbat_que-nunca-existiu"), ("client_id", &client_id)])
        .send()
        .await
        .unwrap();
    g.anotar("revogar/desconhecido", json!({"status": r.status().as_u16(), "corpo": r.text().await.unwrap()}));

    // CIMD: o client_id é uma URL; sem https público não há documento a buscar.
    for (nome, id) in
        [("http", "http://127.0.0.1:9/cliente.json"), ("loopback", "https://127.0.0.1:9/cliente.json"), ("raiz", "https://127.0.0.1:9/")]
    {
        let q = [
            ("client_id", id),
            ("redirect_uri", volta),
            ("response_type", "code"),
            ("code_challenge", ch.as_str()),
            ("code_challenge_method", "S256"),
            ("resource", recurso.as_str()),
            ("state", "estado-cimd"),
        ];
        let r = nav.get(&format!("/oauth/authorize?{}", serde_urlencoded(&q))).await;
        g.anotar_html(&format!("cimd/{nome}/tela"), &r.corpo);
        g.anotar(&format!("cimd/{nome}/status"), json!({"status": r.status, "location": r.local()}));
    }

    let r = nav.get("/admin/oauth").await;
    g.anotar_html("tela/admin_oauth", &r.corpo);
    drop(nav);
    let mut pb = pb;
    pb.parar();
    g.anotar("banco", despejar(&pb.banco()));
    g.conferir();
}

fn serde_urlencoded(pares: &[(&str, &str)]) -> String {
    let mut u = reqwest::Url::parse("http://x/").unwrap();
    u.query_pairs_mut().extend_pairs(pares);
    u.query().unwrap_or("").to_string()
}

// ---------- OAuth de upstream ----------

/// Um MCP atrás de OAuth, pela tela: cadastro em modo oauth, "Conectar conta", o navegador vai ao
/// provedor falso e volta pelo callback, o patchbay troca o code (DCR + PKCE) e as ferramentas
/// chegam ao endpoint.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn oauth_de_upstream_pela_tela() {
    let u = falsos::http_com_oauth().await;
    let mut pb = Patchbay::subir().await;
    let mut g = gravacao("oauth_upstream", &pb);
    g.trocar(u.servidor.base.clone(), "http://PROVEDOR");
    let mut nav = Navegador::new(&pb.raiz);
    nav.setup().await;
    let id = nav
        .criar(
            "/admin/mcps",
            &[("nome", "protegido"), ("tipo", "http"), ("url", &u.url()), ("timeout_ms", "4000"), ("habilitado", "1"), ("modo", "oauth")],
        )
        .await;
    let tela = format!("/admin/mcps/{id}");
    esperar!("o upstream pedir consentimento", 15, nav.get(&tela).await.corpo.contains("/autorizar"));
    let r = nav.get(&tela).await;
    g.anotar_html("antes/tela", &r.corpo);

    let r = nav.post(&format!("{tela}/autorizar"), &[]).await;
    let ida = r.local().unwrap_or_else(|| panic!("autorizar: {} {}", r.status, r.corpo));
    let estado_ida = mascarar_query(&ida);
    g.anotar("autorizar", json!({"status": r.status, "location": estado_ida}));
    // O "navegador" no provedor: ele consente na hora e devolve ao callback do patchbay.
    let volta = reqwest::Client::builder().redirect(reqwest::redirect::Policy::none()).build().unwrap().get(&ida).send().await.unwrap();
    let callback = volta.headers().get("location").expect("o provedor devolve ao callback").to_str().unwrap().to_string();
    g.anotar("callback_url", json!(mascarar_query(&callback)));
    let r = nav.get(&callback).await;
    g.anotar("callback", json!({"status": r.status, "location": r.local()}));

    let chave = nav.criar_chave("teste", &[1]).await;
    let c = conectar(&pb.mcp("principal"), &chave, &Usuario::new()).await;
    esperar!("a ferramenta protegida", 20, nomes(&ferramentas(&c).await).iter().any(|n| n.ends_with("protegida")));
    let lista = ferramentas(&c).await;
    g.anotar("chamada", chamar(&c, &exposto(&lista, "protegida"), json!({})).await);
    let r = nav.get(&tela).await;
    g.anotar_html("depois/tela", &r.corpo);
    {
        let p = u.provedor.lock().unwrap();
        g.anotar("provedor/registros", json!(p.registros));
        let pedidos: Vec<Value> = p.pedidos_de_autorizacao.iter().map(mascarar_mapa).collect();
        g.anotar("provedor/autorizacoes", json!(pedidos));
        let trocas: Vec<Value> = p.trocas.iter().map(mascarar_mapa).collect();
        g.anotar("provedor/trocas", json!(trocas));
    }
    drop(c);
    pb.parar();
    g.anotar("banco", despejar(&pb.banco()));
    g.conferir();
}

/// Os parâmetros aleatórios de um pedido OAuth (state, PKCE) viram marcadores.
fn mascarar_mapa(m: &std::collections::HashMap<String, String>) -> Value {
    let aleatorios = ["state", "code_challenge", "code_verifier"];
    let ordenado: std::collections::BTreeMap<&String, String> =
        m.iter().map(|(k, v)| (k, if aleatorios.contains(&k.as_str()) { "«aleatório»".to_string() } else { v.clone() })).collect();
    json!(ordenado)
}

fn mascarar_query(url: &str) -> Value {
    let u = reqwest::Url::parse(url).unwrap_or_else(|_| reqwest::Url::parse(&format!("http://x{url}")).unwrap());
    let pares: std::collections::HashMap<String, String> = u.query_pairs().into_owned().collect();
    json!({"caminho": format!("{}{}", u.origin().ascii_serialization(), u.path()), "query": mascarar_mapa(&pares)})
}

// ---------- linha de comando ----------

/// Os subcomandos: versão, ajuda, chave-mestra, erros de uso, seed, export e import (plano e
/// aplicação). Com URL de upstream fixa, a `revisao` do YAML é reproduzível e entra na gravação:
/// é o contrato mais traiçoeiro da troca (seção 2.3 do plano).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn linha_de_comando() {
    let pb = Patchbay::preparar();
    let mut g = gravacao("cli", &pb);
    let base64 = Regex::new(r"[A-Za-z0-9+/_-]{43}=?").unwrap();
    for args in
        [vec!["versao"], vec!["ajuda"], vec!["nada-disso"], vec!["chave-mestra", "gerar"], vec!["chave-mestra", "girar"], vec!["seed"]]
    {
        let s = pb.comando(&args).await;
        let mascara = |t: &str| base64.replace_all(t, "«chave»").into_owned();
        let stdout = if args[0] == "versao" { "«versão»".to_string() } else { mascara(&s.stdout) };
        g.anotar(&format!("cmd/{}", args.join("_")), json!({"codigo": s.codigo, "stdout": stdout, "stderr": mascara(&s.stderr)}));
    }
    let url = "http://127.0.0.1:9/mcp";
    let s = pb
        .comando(&[
            "seed",
            "--endpoint",
            "pessoal",
            "--upstream=falso",
            "-upstream-url",
            url,
            "-nome-chave",
            "teste",
            "-timeout-ms",
            "3000",
        ])
        .await;
    g.anotar("seed", json!({"codigo": s.codigo, "stdout": s.stdout, "stderr": s.stderr}));
    let export = pb.comando(&["export"]).await;
    g.anotar("export", json!({"codigo": export.codigo, "stdout": export.stdout, "stderr": export.stderr}));
    let arquivo = pb.dir.join("config.yaml");
    let s = pb.comando(&["export", "-o", arquivo.to_str().unwrap()]).await;
    g.anotar("export_o", json!({"codigo": s.codigo, "stdout": s.stdout}));
    let s = pb.comando(&["export", "-o", arquivo.to_str().unwrap()]).await;
    g.anotar("export_o_sem_forcar", json!({"codigo": s.codigo, "stderr": s.stderr}));
    // Edita o YAML: timeout novo no upstream e um endpoint novo.
    let editado = export.stdout.replace("timeout_ms: 3000", "timeout_ms: 7000").replacen(
        "endpoints:\n",
        "endpoints:\n  - slug: novo\n    nome: Novo\n",
        1,
    );
    std::fs::write(&arquivo, &editado).unwrap();
    let arq = arquivo.to_str().unwrap();
    for (passo, args) in [
        ("import_ensaio", vec!["import", arq, "--dry-run"]),
        ("import", vec!["import", arq]),
        ("import_de_novo", vec!["import", "--dry-run", arq]),
    ] {
        let s = pb.comando(&args).await;
        g.anotar(passo, json!({"codigo": s.codigo, "stdout": s.stdout, "stderr": s.stderr}));
    }
    std::fs::write(&arquivo, "versao: 99\n").unwrap();
    let s = pb.comando(&["import", arq]).await;
    g.anotar("import_versao_desconhecida", json!({"codigo": s.codigo, "stdout": s.stdout, "stderr": s.stderr}));
    std::fs::write(&arquivo, "versao: 1\nupstreams:\n  - nome: x\n    tipo: http\n    url: http://127.0.0.1:9/mcp\n    timeout_ms: 1000\n    habilitado: true\n    segredos:\n      - tipo: bearer\n        valor: em-claro\n").unwrap();
    let s = pb.comando(&["import", arq]).await;
    g.anotar("import_segredo_em_claro", json!({"codigo": s.codigo, "stdout": s.stdout, "stderr": s.stderr}));
    let s = pb.comando(&["export"]).await;
    g.anotar("export_final", json!({"codigo": s.codigo, "stdout": s.stdout}));
    g.anotar("banco", despejar(&pb.banco()));
    g.conferir();
}
