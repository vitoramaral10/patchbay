# Plano: patchbay de Go para Rust

Fase 2 da migração (TASKS.md, "Migracao de tudo para Rust"). Documento de pesquisa e plano,
escrito em 27/11/2026 e copiado para o repositório na fatia F0.

> **Correção (F0):** o plano original partiu de que o patchbay não estava implantado. Está: o Go roda
> em produção no stack `mcp` do homelab. A decisão D0 ("implantar o Go agora") não se aplica, e a seção
> 2.6 foi corrigida. A troca da F14 substitui um serviço que está no ar.

**Base lida:** `origin/main` 85c4f21 (PR #21, 27/11). O checkout local está 2 commits atrás; a
diferença é só a exibição da revisão da MCP e do servidor de cada upstream
(`internal/upstream/gerente.go` `situacao()`, `admin.templ`). rmcp lido da fonte em `~/.cargo/registry/src/*/rmcp-3.12.0`, goose em
`~/go/pkg/mod/github.com/pressly/goose/v3@v3.28.0`.

**Conclusão em cinco linhas**

1. Das 44,4 mil linhas de Go, 15,7 mil são geradas pelo templ. O que se porta de fato: 28,8 mil de Go
   escrito à mão e 4,8 mil de `.templ`. Rust estimado: ~27 mil de produção + ~4,5 mil de template + ~15 mil de teste.
2. O mesmo arquivo SQLite com o mesmo esquema funciona: o Rust aplica os mesmos `.sql` com um executor
   compatível com a goose (mesma tabela `goose_db_version`), e o formato `pbc1:` dos segredos é
   reproduzível bit a bit (HKDF-SHA256 + AES-256-GCM + AAD em uvarint).
3. 16 fatias de port (F0 a F15) e 7 de repasse novo (N1 a N7). A F0 é uma caixa-preta que roda
   contra o binário Go antes de existir Rust de produção. Cada fatia tem critério verificável.
4. Lado a lado útil a partir da F6 (upstreams HTTP), completo a partir da F11. Rodar o Rust contra uma
   **cópia** do banco exige desligar os upstreams OAuth na cópia: a rotação de refresh token
   invalidaria a concessão do Go.
5. Os repasses novos roteiam sem estado de sessão: o nome (prompt) ou a URI (resource) vêm do snapshot,
   e o upstream fica codificado no identificador quando é o gateway quem o cria (taskId, `skill://`, `ui://`).
   O único estado em memória é por requisição (progress, cancel).

---

## 1. Inventário por pacote

### 1.1 Tamanho e responsabilidade

Linhas de `origin/main`. "Go" = produção escrita à mão (sem `_templ.go` nem `_test.go`); "templ" = `.templ`
fonte; "teste" = `_test.go` (número de `func Test`).

| Pacote Go | Responsabilidade | Go | templ | teste (n) | Dependências Go | Módulo Rust e equivalente |
|---|---|---:|---:|---:|---|---|
| `cmd/patchbay` | subcomandos (`serve`, `seed`, `export`, `import`, `biblioteca-semente`, `chave-mestra`, `versao`), `montar()` com o grafo inteiro, rotas, reinício interno (`errReiniciar`), painel, adaptadores entre features, log de acesso | 2.744 | 146 | 6.465 (87) | stdlib `flag`, `net/http`, `slog` | `main.rs`, `cli.rs` (parser próprio que aceita `-x` e `--x` como o `flag` do Go; sem clap), `app.rs` (axum `Router`, `tokio::select!` no laço de reinício), `adaptadores.rs` |
| `internal/platform/store` | 2 pools (leitura livre, escrita com 1 conexão), WAL, `busy_timeout`, `foreign_keys`, `_txlock=immediate`, migrações embutidas, tabela `settings` | 200 | — | 329 (10) | `modernc.org/sqlite`, `goose/v3` | `plataforma/store.rs`: `sqlx` 0.9 (`sqlite`, `runtime-tokio`), dois `SqlitePool` (escrita `max_connections(1)`), `SqliteConnectOptions` com os mesmos pragmas; `migracoes.rs`: executor goose-compatível próprio (~120 linhas, ver 2.1) |
| `internal/platform/cripto` | chave mestra (base64 em 4 codificações), cofre AES-256-GCM com HKDF, formato `pbc1:`, AAD por campo, canário, tipo `Segredo` que se redige | 406 | — | 624 (19) | `crypto/*`, `x/crypto/hkdf` | `plataforma/cripto.rs`: `aes-gcm`, `hkdf`, `sha2`, `base64`, `rand`, `zeroize`; `Segredo` com `Debug`/`Display` que imprimem `«redigido»` |
| `internal/platform/stdioproc` | árvore de processo (process group no Unix, Job Object no Windows), ambiente herdado e filtrado, stderr em linhas com tetos, `WaitDelay` | 538 | — | 630 (12) | `x/sys/windows` | `plataforma/stdioproc.rs`: `process-wrap` 10 (o mesmo que o rmcp usa: `ProcessGroup::leader()`, `JobObject`, `KillOnDrop`), `tokio::process`; espera de pipe com `tokio::time::timeout` sobre a tarefa que lê o stderr |
| `internal/platform/webui` | layout, componentes (campo, selo, alerta), constantes de rota, estáticos embutidos (CSS Tailwind compilado, htmx 2.0.7, ext SSE, fonte) | 349 | 443 | 0 | `templ` | `plataforma/webui/`: `askama` (ver 1.3); estáticos com `include_bytes!` ou `rust-embed` apontando para `internal/platform/webui/estatico` até a F15 (uma cópia só) |
| `internal/platform/versao` | número, commit e data por `-ldflags` | 19 | — | — | — | `env!("CARGO_PKG_VERSION")` + `option_env!` no build |
| `internal/admin` | setup do primeiro acesso, login, sessão por cookie `patchbay_sessao_admin` (token com hash SHA-256 no banco), senha argon2id em PHC, middleware `Proteger` | 829 | 120 | 566 (13) | `x/crypto/argon2` | `admin/`: `argon2` (RustCrypto, aceita a string PHC `$argon2id$v=19$m=..,t=..,p=..$sal$hash` em base64 sem padding), `sha2`, `subtle`; extractor axum de sessão |
| `internal/apikey` | emissão `pbk_<8 base32>_<base64url>`, hash SHA-256, verificação, gravação de último uso em lote, telas | 712 | 199 | 378 (7) | stdlib | `apikey/`: `data-encoding` (base32) ou base32 à mão, `sha2` |
| `internal/catalogo` | normalização de ferramenta (nome ≤128, caractere inválido vira `_`, esquema permissivo, `x-mcp-header` inválido removido, `outputSchema` descartável), regras `excluir`/`renomear` com glob e captura, prefixo, desambiguação, texto de lápide | 973 | — | 966 (12) | go-sdk `mcp`, `jsonschema-go` | `catalogo/`: lógica pura sobre `serde_json::Value` e `rmcp::model::Tool`; não precisa de crate de JSON Schema (o Go só usa `jsonschema-go` para o tipo `*jsonschema.Schema` que o SDK devolve) |
| `internal/endpoint` | um `*mcp.Server` por endpoint em `/mcp/{slug}`, materialização sob mutex por endpoint, lápide com janela de 5 min, `tools/list_changed`, encaminhamento com MRTR (só elicitação), observador da trilha, telas de endpoint e composição | 2.189 | 493 | 1.138 (20) | go-sdk `mcp`, `auth` | `endpoint/`: `impl ServerHandler` dinâmico (sem macros: `list_tools`, `call_tool`, `get_tool`, `get_info`, `accepted_subscription_filter`, `listen`), um `StreamableHttpService` por endpoint com `legacy_session_mode(false)` e `disable_allowed_hosts()` como no `mcp_base/http.rs` do igpsport |
| `internal/upstream` | gerente (7 estados, backoff com jitter, watchdog de connect, contagem de abandonos, autodesabilitação), transportes HTTP/SSE/STDIO, credenciais estáticas cifradas, OAuth de upstream (broker, fonte de token, consentimento pela tela, loopback colado, renovação proativa), sonda funcional, detecção de transporte e de OAuth, colar comando/JSON/`claude mcp add`/`add-mcp`, telas | 9.357 | 1.690 | 8.787 (130) | go-sdk `mcp`, `auth`, `oauthex`; `x/oauth2`; `x/sync/singleflight` | `upstream/`: `rmcp` cliente (`transport-streamable-http-client-reqwest`, `transport-child-process`, `auth`), transporte SSE legado próprio, `reqwest` 0.13 rustls, `tokio` (`select!`, `time`) |
| `internal/authsrv` | authorization server OAuth 2.1: metadata RFC 8414/9728/OIDC em 8 rotas, authorize com consentimento, token (code + PKCE S256, refresh com rotação e detecção de replay por família), revogação RFC 7009, DCR com teto por origem, CIMD com guarda de SSRF, limite de taxa, telas de clientes | 4.402 | 645 | 1.197 (23) | go-sdk `auth`, `oauthex` | `authsrv/`: código próprio como no Go (a nota do pacote explica por que não há biblioteca: `resource` do RFC 8707); `serde` para os documentos; SSRF reaproveitando as faixas do igpsport (`no_proxy`, CGNAT, 0/8, 198.18/15, 240/4, NAT64, 6to4) |
| `internal/configuracao` | export/import YAML, plano e diferença, trava otimista por `revisao` (SHA-256 do `json.Marshal`), segredo só por `${VAR}`, telas | 1.997 | 313 | 1.306 (15) | `yaml.v3` | `configuracao/`: fork mantido do serde_yaml (decidir na fatia entre `serde_norway` e `yaml_serde`, este já entra com `rmcp/skills-fs`); serialização Go-compatível para o resumo (ver 2.3) |
| `internal/trilha` | eventos por chamada em fila não bloqueante, gravação em lote, poda por retenção, redação de segredo (URL, query, campos), tela filtrável com cursor, log ao vivo por SSE (handler de `slog` que publica num hub) | 2.074 | 473 | 2.307 (38) | stdlib | `trilha/`: `tokio::sync::mpsc` com `try_send`, `tracing_subscriber::Layer` no lugar do `HandlerLog`, `tokio::sync::broadcast` no hub, `axum::response::Sse` |
| `internal/biblioteca` | catálogo de servidores (semente `semente.json.gz` embutida), sincronizador de fundo, curadoria por regex sobre o HTML do mcpservers.org, telas, "adicionar" que preenche o formulário | 2.059 | 317 | 2.369 (48) | stdlib `regexp` | `biblioteca/`: `regex` (a sintaxe usada, `(?s)` e `.*?`, existe no crate), `flate2`, `html-escape`; fixtures de `testdata/*.html` reaproveitadas |
| `internal/arquitetura` | teste: feature não importa feature (exceção `endpoint`→`catalogo`), platform não importa feature | 0 | — | 135 (2) | `go list` | teste que varre `use crate::<feature>` em `src/<feature>/**`; mesma lista de exceções |
| **Total** | | **28.848** | **4.839** | **27.165 (436)** | | |

Gerado pelo templ: 15.681 linhas (`*_templ.go`), que somem.

### 1.2 Pilha Rust

Fixada como nas fases 0 e 1 (igpsport e hersir): edição 2024, `rust-version = "1.100"`,
`rmcp = "=3.12.0"`. Código em `rust/` ao lado do Go, binário `patchbay`, comentários em português,
`clippy -D warnings` e `fmt --check` limpos.

| Papel | Crate | Observação |
|---|---|---|
| Runtime e HTTP | `tokio`, `axum` 0.8, `tower`, `hyper-util` | `axum::serve` com `with_graceful_shutdown`; timeout de cabeçalho de 10 s como o `ReadHeaderTimeout` do Go (copiar de `mcp_base/http.rs`) |
| MCP servidor | `rmcp` `server`, `transport-streamable-http-server` | `ServerHandler` escrito à mão, sem `#[tool_router]`: o catálogo é dinâmico |
| MCP cliente | `rmcp` `client`, `transport-streamable-http-client-reqwest`, `transport-child-process`, `auth` | início `Auto` (discover com queda para `initialize`, `service/client.rs:746-760`) |
| HTTP de saída | `reqwest` 0.13 `rustls`, `stream` | um cliente por upstream (credencial não vaza entre upstreams, como em `credencial.go:clienteDe`) |
| Banco | `sqlx` 0.9 `sqlite` | consultas em tempo de execução (`sqlx::query_as`), sem macros: o build não depende de `DATABASE_URL` nem de `.sqlx` |
| Template | `askama` | ver 1.3 |
| Cripto | `aes-gcm`, `hkdf`, `sha2`, `hmac`, `argon2`, `subtle`, `rand`, `base64`, `zeroize` | |
| JSON | `serde`, `serde_json` **sem** `preserve_order` | o Go ordena chaves de mapa ao serializar; `BTreeMap` dá a mesma ordem. Conferir por `cargo tree -e features` que nenhuma dependência liga `preserve_order` por unificação |
| YAML | fork mantido do serde_yaml | decidir na F12 |
| Log | `tracing`, `tracing-subscriber` (`json`, `env-filter`) | `PATCHBAY_LOG_LEVEL`, `PATCHBAY_LOG_TEXTO` |
| Diversos | `regex`, `flate2`, `url`, `jiff`, `futures`, `tokio-stream`, `sse-stream` (já vem do rmcp; parser do SSE legado), `html-escape` | |

### 1.3 Template no lugar do templ: askama

| | askama | maud | minijinja |
|---|---|---|---|
| Sintaxe | HTML + `{{ }}`/`{% %}`, arquivo `.html` | DSL Rust (`html! { div.classe { ... } }`) | Jinja2 em tempo de execução |
| Conversão dos 4.839 de `.templ` | quase mecânica: o `.templ` já é HTML com expressões | reescrever cada tag na DSL | mecânica, mas sem checagem |
| Erro de campo/variável | em compilação | em compilação | em execução |
| Escape HTML | automático | automático | automático |

Recomendação: **askama**. Mantém as mesmas classes nos mesmos elementos, e isso tem um efeito
verificável: o `patchbay.css` que já está no repositório continua valendo. Critério da F2: o Tailwind CLI
(v4.3.3, `@source` apontado para `rust/templates/**/*.html`) gera um CSS **idêntico** ao commitado. Se uma
classe some, o diff do CSS acusa. Componentes do templ com parâmetros (`webui.Campo`, `webui.Selo`) viram
macros do askama ou structs com `Template`. As funções Go dentro dos `.templ` (`avisoDeRemocao`,
`proximaTentativa`) viram métodos do struct de dados.

### 1.4 Onde o rmcp 3.12 não cobre o que a go-sdk dá hoje

| O que o Go usa | Onde | Situação no rmcp 3.12 | O que fazer |
|---|---|---|---|
| `mcp.SSEClientTransport` (HTTP+SSE de 2024-11-05) | `upstream/sse.go`, `gerente.go:transporteDe` | **não existe** (`grep SseClient` vazio; `client_side_sse.rs` é só o parser do streamable) | transporte próprio implementando `rmcp::transport::Transport`: GET no SSE, evento `endpoint`, POST das mensagens; ~250 linhas; manter o `Abandonar()` do Go (fechar o GET quando o connect é abandonado) |
| `auth.RequireBearerToken` + `auth.ProtectedResourceMetadataHandler` (servidor) | `endpoint/http.go`, `authsrv/metadata.go` | **não existe** do lado servidor | middleware axum: `Bearer`, verificador combinado (chave de API ou token do AS), escopo do endpoint, 401 com `WWW-Authenticate: Bearer resource_metadata="..."`, 403 `insufficient_scope`, sem exigir expiração para chave de API (`AllowMissingExpiration`). Texto dos cabeçalhos gravado do Go |
| `oauthex.ClientRegistrationMetadata`, `AuthServerMeta`, `ProtectedResourceMetadata` (do lado AS) | `authsrv/registro.go`, `cimd.go`, `metadata.go` | só structs do lado cliente (`AuthorizationMetadata`, `ClientRegistrationResponse` em `transport/auth.rs`) | structs serde próprias no `authsrv` |
| `auth.AuthorizationCodeHandler` com `AuthorizationCodeFetcher` que **bloqueia** até o admin clicar | `upstream/oauth_broker.go:704 buscarCodigo` | modelo partido: `AuthorizationManager::get_authorization_url` → callback → `exchange_code_for_token`, com `StateStore` e `CredentialStore` plugáveis (`transport/auth.rs:349, 517`) | redesenho do broker (fica mais simples). Ganho: o `code_verifier` do PKCE passa a poder ser persistido (`StoredAuthorizationState`), o que o comentário de `oauth_broker.go:46-54` diz ser impossível na go-sdk |
| `x/oauth2` `AuthStyle` com autodetecção, gravado em `upstream_oauth.estilo_auth` | `oauth_broker.go:469, 773` | `oauth2`-rs não autodetecta | mapear: 1 (InParams) → corpo; 2 (InHeader) → Basic; 0 → escolher por `token_endpoint_auth_methods_supported`. Teste com linha gravada pelo Go |
| `AddTool`/`RemoveTools` com `tools/list_changed` em debounce | `endpoint/servidores.go`, `lapide.go` | não há registro dinâmico com notificação automática | snapshot por endpoint (`ArcSwap` ou `RwLock`), `listen()` que espera um `tokio::sync::watch` do endpoint e chama `notify_tool_list_changed`; debounce próprio (~30 linhas). As lápides viram entradas do snapshot com prazo, sem registro/remoção |
| capacidade do cliente por requisição (`mcp.MetaKeyClientCapabilities`) no `CallTool` do upstream | `endpoint/servidores.go:350`, `gerente.go:Encaminhar` | o rmcp **sobrescreve** `clientCapabilities` do `_meta` da requisição com as da sessão no modo discover (`service.rs:946-951`) e só depois aplica `PeerRequestOptions.meta` (`:952`); `call_tool_once` não aceita opções | enviar com `send_request_with_option` e `PeerRequestOptions { meta }` contendo só a chave `io.modelcontextprotocol/clientCapabilities`, e reproduzir o `match` de `call_tool_once` (`client.rs:1526-1549`). Verificar na F5 se esse caminho cancela ao ser descartado como o `send_request_cancel_on_drop` |
| `http.CrossOriginProtection` (stdlib Go 1.25, não é SDK) | `cmd/patchbay/aplicacao.go:587` | — | middleware próprio: recusar método inseguro com `Sec-Fetch-Site` diferente de `same-origin`/`none`, ou `Origin` com host diferente do `Host`, salvo origem confiável. Cenários do `TestUI_CSRF` |
| proteção contra DNS rebinding do streamable da go-sdk | `endpoint/http.go:57-60` | `allowed_hosts` é lista fixa | política de Host do `mcp_base/http.rs` do igpsport (403 quando chega por loopback com Host de fora) |
| `mcp.CommandTransport` com `exec.Cmd` e `WaitDelay` | `upstream/stdio.go` | `TokioChildProcess::builder(CommandWrap)` com `.stderr(piped)` devolve o `ChildStderr` (`child_process.rs:174`) | árvore via `process-wrap`; o `WaitDelay` (neto que segura o stderr) vira timeout na tarefa leitora depois da saída do filho |

O que o rmcp tem e a go-sdk não: Tasks (SEP-2663), Skills (`list_skills`, `get_skill`,
`read_resource_directory`), `subscriptions/listen`, `request-state` HMAC, OAuth cliente com CIMD e DCR,
`traceparent`. Lacuna do rmcp que afeta o plano: `notifications/tasks` não sai por
`subscriptions/listen` (`service/server.rs:232`); o Claude não usa (faz poll), então não bloqueia.

Some por desnecessário: o `recover` em volta do `AddTool` (`servidores.go:registrar`), o vazamento
deliberado de goroutine no connect (`gerente.go:conectar`, issue #1189 da go-sdk: no tokio o futuro
descartado some), o `semValoresDoCliente` (não há contexto com valores; a regra que fica é nunca repassar o
`_meta` do cliente inteiro, só a chave de capacidade), a recriação do `*mcp.Server` quando nome, descrição
ou instruções mudam (`get_info` lê o registro atual).

---

## 2. Estratégia de troca

### 2.1 Mesmo SQLite, mesmo esquema

Sim, e é o que torna o rollback trivial. Regras:

1. **Os mesmos 16 arquivos** (`00001`…`00018`, faltam 3 e 9) entram no binário Rust por `include_str!`
   apontando para `internal/platform/store/migracoes/`: uma cópia só até a F15.
2. **Executor goose-compatível próprio** (~120 linhas), não `sqlx::migrate!` (que usa `_sqlx_migrations`
   com checksum e não enxergaria o que o Go aplicou). Forma exata da goose 3.28
   (`internal/dialects/sqlite3.go:17-30`):
   `CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY AUTOINCREMENT, version_id INTEGER NOT NULL, is_applied INTEGER NOT NULL, tstamp TIMESTAMP DEFAULT (datetime('now')))`,
   linha zero `(0, 1)` na criação, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)` por
   migração. Ler só a seção `-- +goose Up`, respeitar `StatementBegin/End` e o `-- +goose NO TRANSACTION`
   do `00017` (o `PRAGMA foreign_keys` não funciona dentro de transação).
3. **Compatibilidade nos dois sentidos, conferida no código da goose:** `gooseutil.UpVersions`
   (`internal/gooseutil/resolve.go:19-54`) só acusa versão "faltando" se ela for menor que a maior aplicada;
   versão do banco maior que as do binário não é erro. Logo o Go sobe num banco em que o Rust aplicou uma
   migração nova. Mesmo assim:
4. **Esquema congelado até a F15**: nenhuma migração nova enquanto o rollback para o Go for opção. Depois da
   troca e antes de apagar o Go, só migração **aditiva** (tabela nova, coluna com `DEFAULT`), testada subindo o
   binário Go no banco migrado.
5. Pools e pragmas iguais (`store.go:abrirPool`): WAL, `busy_timeout=5000`, `synchronous=NORMAL`,
   `foreign_keys=1`, `BEGIN IMMEDIATE` na escrita, uma conexão de escrita.

### 2.2 Segredos com `PATCHBAY_MASTER_KEY` continuam legíveis

O formato é pequeno e determinístico (`platform/cripto/cofre.go`):

- chave: `PATCHBAY_MASTER_KEY` decodificada aceitando base64 Std, RawStd, URL e RawURL **nessa ordem**, 32
  bytes; sem a variável, `<dados>/chave-mestra` (0600, criada por arquivo temporário + rename).
- chave de cifra: HKDF-SHA256(ikm = mestra, salt = `patchbay:cripto:v1`, info =
  `patchbay:cifra-de-campo:aes-256-gcm:v1`), 32 bytes.
- valor: `"pbc1:" + base64url_sem_padding(nonce12 ‖ ciphertext ‖ tag16)`.
- AAD: para cada parte de `["pbc1", tabela, coluna, id]`, `uvarint(len)` (LEB128) seguido dos bytes.
- campos em uso: `settings/valor/canario_chave_mestra` (texto `patchbay: canário da chave mestra`);
  `upstream_secret/valor_cifrado/"<upstream_id>/<tipo>/<nome>"` (tipo `bearer`, `header`, `env`);
  `upstream_oauth/<coluna>/"<upstream_id>"` para `client_secret_cifrado`, `access_token_cifrado`,
  `refresh_token_cifrado`.
- canário: ausente → grava; presente e não decifra → **não sobe** (mesma mensagem).

Os demais contratos de credencial também são portáveis sem reemissão: chave de API e tokens do AS são
SHA-256 do valor em claro (`pbk_`, `pbat`, `pbrt`, `pbac`, `pbcs`); sessão de admin é SHA-256 do token do
cookie; senha de admin é argon2id em PHC com base64 sem padding (`admin/senha.go:66`).

Critério (F1): fixture de banco gerada pelo Go com um segredo de cada tipo; o Rust decifra todos e verifica o
canário; o banco recém-criado pelo Rust sobe no Go com canário válido.

### 2.3 Outros contratos que precisam sair iguais byte a byte

- **`revisao` do YAML** (`configuracao/configuracao.go:360`): `sha256:` + 32 hex de
  `json.Marshal` da struct normalizada. Chaves com o **nome do campo Go** (`Nome`, `Tipo`, `TimeoutMS`…,
  sem tag json salvo `omitempty` em `Sonda`), na ordem de declaração; mapas ordenados; `<`, `>` e `&`
  escapados como `<` (padrão do Go); slice nil vira `null` e vazio vira `[]`. É o risco mais traiçoeiro
  da troca: errar faz todo import mostrar "o banco avançou". Critério: revisões de um YAML exportado pelo Go
  batem no Rust para todos os itens.
- Rotas, métodos e status (lista completa em `webui/rotas.go` e `authsrv/metadata.go:13-84`), nomes de
  cookie, nomes de campo dos formulários e atributos htmx (o JS do log ao vivo e o htmx dependem deles).
- Variáveis e flags: `PATCHBAY_LISTEN`, `PATCHBAY_DATA_DIR`, `PATCHBAY_PUBLIC_URL`, `PATCHBAY_MASTER_KEY`,
  `PATCHBAY_LOG_LEVEL`, `PATCHBAY_LOG_TEXTO`, `PATCHBAY_SEGREDO_*`; flags `-listen`, `-data-dir`, `-public-url`,
  `-log-level`, `-log-texto`, `-o`, `-forcar`, `-dry-run`, `-remover-ausentes`, `-endpoint`, `-upstream`,
  `-upstream-url`, `-nome-chave`, `-timeout-ms`, em forma `-x`, `--x` e `-x=v`. Códigos de saída do `import`.
- Mensagens que o cliente MCP vê: `patchbay: upstream %q não atendeu a chamada de %q`, a da elicitação que o
  cliente não atende (`servidores.go:semPedidoQueOClienteNaoAtende`), o texto da lápide (`catalogo/lapide.go`),
  o 400 de credencial na query.
- `serverInfo` do endpoint: `name = "patchbay/<slug>"`, `title` = nome ou slug, `version`.

### 2.4 Como provar paridade de um gateway

Quatro camadas, da mais barata à mais forte.

**(a) Tabelas de teste transcritas.** O que é lógica pura no Go vira teste unitário Rust com os mesmos
casos: `catalogo` (normalizador, regras, glob com captura), `upstream/comando_test.go` (455 linhas de casos de
colagem), `backoff_test.go`, `trilha/redacao`, `biblioteca` (fixtures em `testdata/`), `cripto`, `escritor`
do stdioproc. O que depende de tempo (curva do backoff, 5 abandonos, janela de 5 min da lápide, renovação
proativa) só é testável aqui, com relógio falso como o `Relogio` do Go.

**(b) Caixa-preta contra os dois binários** (`rust/tests/caixa_preta/`, padrão igpsport/hersir):

- sobe `PATCHBAY_BIN` (padrão: o Rust; `PATCHBAY_BIN=/caminho/patchbay-go` para o Go) com diretório de dados
  temporário, chave mestra fixa, porta aleatória, `env_clear`;
- configuração pelos próprios subcomandos (`seed`, `import` de YAML) e pelos formulários da tela (login com
  cookie, cabeçalhos de mesma origem), o que já exercita a superfície;
- **upstreams falsos** em processo: rmcp servidor streamable 2026-07-28 sem sessão; rmcp servidor streamable
  2025-06-18 com sessão; servidor SSE 2024-11-05 escrito à mão em axum; STDIO por um binário de apoio
  `patchbay-upstream-falso` (target `[[bin]]` só de teste). Ferramentas de propósito difíceis: nome com
  espaço, nome de 140 caracteres, colisão entre upstreams, `x-mcp-header` inválido, `outputSchema`,
  imagem e áudio, resultado grande, `isError`, elicitação (`input_required`), ferramenta que pendura, um
  gatilho HTTP no falso para acrescentar ferramenta (list_changed);
- **provedor OAuth falso** (AS + PRM + DCR + CIMD + rotação de refresh + recusa `invalid_grant`), na F10/F11;
- **clientes reais**: rmcp cliente em 2026-07-28 (discover) e em 2025-06-18 (initialize); JSON-RPC cru por
  `reqwest` para conferir status, cabeçalhos (`WWW-Authenticate`, `Mcp-Session-Id` ausente) e corpo;
  `AuthorizationManager` do rmcp como cliente OAuth do AS (o Go usava o cliente da go-sdk, o que dá duas
  famílias de cliente contra o mesmo AS). Na F14, um terceiro: o inspector CLI (SDK TypeScript).

**(c) Respostas gravadas do Go.** `PATCHBAY_GRAVAR=1` com o binário Go grava `tests/respostas_go/<cena>.json`;
contra o Rust, compara por valor. Normalização antes de gravar: ids de requisição, portas, instantes,
tokens, cookies, `requestState`, hashes de sessão. Telas: HTML normalizado (espaços colapsados, valores
dinâmicos mascarados) comparado como texto. Toda diferença aceita vai para "Diferenças conhecidas" no
`rust/README.md`, como no hersir. `tests/superficie.json` guarda o `tools/list` agregado do Go para o
cenário padrão.

**(d) Paridade de banco e troca cruzada.** Depois de cada cena, despejo ordenado de todas as tabelas (sem
instantes e sem valores cifrados, que têm nonce aleatório) comparado entre Go e Rust: prova que os dois
escrevem o mesmo. E duas cenas cruzadas: "Go configura, para; Rust sobe no mesmo diretório e serve tudo
(ferramentas, chave de API, concessão OAuth decifrada)" e o inverso.

### 2.5 Fatias, em ordem de dependência

Tamanho: P até ~1 mil linhas Rust (produção + teste), M 1 a 3 mil, G 3 a 6 mil. "Sessões" é uma ordem de
grandeza de sessões de agente, para planejar cota.

| Fatia | Conteúdo (de onde vem no Go) | Depende de | Critério de pronto verificável | Tam. |
|---|---|---|---|---|
| **F0** Caixa-preta contra o Go | crate `rust/` com `main` que só imprime a versão; harness, upstreams falsos (HTTP 2026, HTTP 2025, SSE, STDIO), clientes, `GRAVAR`, despejo de banco. Cenas: `TestPontaAPonta`, `TestAutenticacaoNoEndpoint`, `TestCredencialEmQueryString`, `TestUpstreamForaDoAr_CatalogoVazio`, `TestFerramentaNovaDoUpstreamChegaAoCliente`, `TestElicitacaoAtravessaOGateway`, `TestUI_SessaoNaoAtravessaEndpoint` (de `cmd/patchbay/*_test.go`) | — | `PATCHBAY_BIN=<go> cargo test -j 2 --test caixa_preta` verde; `respostas_go/` gravado e commitado | M (2 sessões) |
| **F1** Plataforma | `main`/`config`/`chave.go`, `store`, executor goose, `cripto`, canário, `/saude`, log, desligamento e reinício interno | F0 | fixtures do 2.2 decifradas; banco do Go migrado pelo Rust sem mudança em `goose_db_version`; banco novo do Rust sobe no Go; `chave-mestra gerar` aceito pelo Go; testes de `cripto` (19) e `store` (10) transcritos | M (1–2) |
| **F2** Web base e admin | `webui` (layout, componentes, estáticos), `admin` (setup, login, sair, sessões, `Proteger`), CSRF, política de Host, `acesso.go` | F1 | admin e sessão criados pelo Go valem no Rust; cenas `TestUI_CSRF`, `TestUI_RotaProtegidaSemSessao`, `TestUI_EstaticosServidosDoBinario`; CSS do Tailwind gerado dos templates Rust idêntico ao commitado | M (2) |
| **F3** Catálogo e chaves de API | `catalogo/*` (puro + `composicao_sqlite`), `apikey/*` com telas | F2 | 12 testes do catálogo transcritos; chave emitida pelo Go autentica no Rust; `TestUI_ChaveApareceUmaVezSo`, `TestUI_RevogarChaveDerrubaOAcesso` | M (1–2) |
| **F4** Gerente de upstream HTTP | `gerente.go`, `backoff.go`, `upstream.go`, `credencial*.go`, `configs_sqlite.go`, `repositorio_sqlite.go`, telas de MCPs sem OAuth/STDIO; cliente rmcp `Auto`; relistar no list_changed; revisão e servidor declarados (PR #21) | F3 | estados na tela iguais ao Go para upstream bom, que pendura e que cai; `TestUI_ReconectarUpstreamRearmaASupervisao`, `TestUI_TelaDeUpstreamMostraFerramentasDescobertas`, `TestUI_NomeDeUpstreamDuplicadoRecusado`; unidade: backoff, abandonos, autodesabilitação com relógio falso (`resiliencia_test.go`, `hotapply_test.go`) | G (3) |
| **F5** Endpoint MCP | `endpoint/servidores.go`, `http.go`, `lapide.go`, `observador.go`; middleware bearer (só chave de API aqui), recusa da query, `subscriptions/listen` com debounce, MRTR de elicitação | F4 | todas as cenas de F0 verdes contra o Rust com respostas iguais às gravadas; `superficie.json` igual; lápide: ferramenta removida continua listada e responde o texto do Go (expiração em teste unitário) | M (2) |
| **F6** Endpoints e composição na tela | `endpoint/admin*.go`, `ferramentas_form.go`, `repositorio_sqlite.go`, painel e "Conectar um cliente", `seed` | F5 | `TestUI_ComposicaoFina`, `TestUI_DesmarcarERemarcarUpstreamNaoTrazRegraAntiga`, `TestUI_RegraInvalidaNaoGrava`, `TestUI_EditarEndpointTrocaComposicao`, `TestUI_EndpointRemovidoSaiDoAr`, `TestUI_FluxoCompletoSemReiniciar`, `TestUI_SlugDuplicadoRecusado`; HTML das telas igual ao gravado. **Primeiro lado a lado possível** | M (2) |
| **F7** STDIO | `stdioproc`, `upstream/stdio.go`, `comando.go`, `colar.go`, `comando_http.go`, telas de colar | F6 | 12 testes do stdioproc (o neto morre junto) e os casos de `comando_test.go` transcritos; `TestUI_ComandoColadoChegaAoUpstream`; nenhum processo sobra depois de parar o binário (`pgrep` do falso vazio) | G (2–3) |
| **F8** SSE legado e detecção | transporte SSE próprio, `deteccao.go`, `sem_auth`, `ajustarModoPorDescoberta` | F6 | cenas de `sse_test.go` (inclui o GET pendurado no abandono) e `deteccao_interno_test.go` em caixa-preta | M (1) |
| **F9** Trilha e sonda | `trilha/*`, `sonda.go`, `sonda_supervisao.go`, log ao vivo | F5, F6 | 38 testes da trilha (redação inclusa) e 24 da sonda nas camadas certas; `TestUI_Trilha*`, `TestUI_Sonda*` iguais ao Go; despejo de `call_log` igual | G (2–3) |
| **F10** Authorization server | `authsrv/*` inteiro com telas, verificador combinado no middleware | F2, F5 | cenas de `oauth_test.go` e `oauth_dinamico_test.go` (`TestOAuthPontaAPonta`, `TestDCRPontaAPonta`, `TestCIMDPontaAPonta`, `TestCodigoReusadoRevogaAFamilia`, `TestRefreshRotacionaEDetectaReplay`, `TestRevogacaoRFC7009`, `TestTokenDeOutroEndpointRecebe403`, `TestLimiteDeTaxaNoToken`, `TestRedirectLoopbackIgnoraPorta`…) verdes contra os dois; token emitido pelo Go vale no Rust | G (3) |
| **F11** OAuth de upstream | broker sobre `AuthorizationManager`, `CredentialStore` e `StateStore` cifrados no SQLite, documento CIMD em `/oauth/patchbay-cliente.json`, consentimento pela tela, callback, loopback colado, renovação proativa serializada (`CredentialRefreshGuard`), `on_refresh_token_rejected` → `sem_consentimento`, refresh do Google, OAuth no SSE | F4, F8, F10 | cenas de `oauth_test.go`/`oauth_loopback_test.go`/`oauth_fonte_interno_test.go` contra o provedor falso; concessão gravada pelo Go usada pelo Rust **sem novo consentimento**; `estilo_auth` 0, 1 e 2 testados. **Lado a lado completo** | G (3) |
| **F12** Configuração YAML | `configuracao/*`, `cmd/patchbay/configuracao*.go`, tela de ajustes, URL pública com reinício | F6, F7, F10, F11 | YAML exportado pelo Go importa no Rust com plano vazio e vice-versa; revisões iguais (2.3); `TestConfiguracao_IdaEVoltaEntreInstalacoes`, `TestComandoImport_CodigoDeSaidaCobreConflito`, `TestImport_PreservaModoDeCredencial` | M (2) |
| **F13** Biblioteca | `biblioteca/*`, `biblioteca-semente`, migração do catálogo antigo | F4 | 48 testes transcritos sobre os mesmos `testdata/*.html`; `TestBiblioteca*`, `TestAdicionarDa*`, `TestCatalogoDoRegistryEDescartadoNaAtualizacao` | M (2) |
| **F14** Troca | caixa-preta completa verde contra os dois; Dockerfile Rust (multi-stage, mesma base node+uv, usuário 65532, mesmo `ENTRYPOINT`/`CMD`); workflow `imagem.yml`; ensaio do roteiro 2.6; inspector CLI como terceiro cliente | F0–F13 | relatório da caixa-preta com N cenas e N respostas iguais contra Go e Rust; imagem publicada; roteiro executado com Claude Code real | M (1–2) |
| **F15** Saída do Go | apaga Go e templ, move migrações e estáticos para `rust/`, descongela o esquema | F14 + período de observação | `go.mod` some; `cargo test` verde; imagem nova sobe no banco da produção | P (1) |

Paralelizável depois da F6: F7, F8, F9, F10 e F13 mexem em módulos diferentes e podem sair de agentes
simultâneos (análise e escrita em paralelo; build e teste em fila, com `flock` no `build.lock`).

### 2.6 Lado a lado e a troca

O patchbay Go está em produção no stack `mcp` do homelab (imagem `ghcr.io/vitoramaral10/patchbay`, volume
`patchbay-dados`), e é por ele que os clientes do dono chegam aos MCPs. "Produção" é esse volume: o roteiro
abaixo é a troca de um serviço no ar, e o rollback precisa funcionar de verdade.

**Lado a lado (a partir da F6), Rust em outra porta lendo uma cópia:**

1. Cópia consistente com WAL: `sqlite3 <dados>/patchbay.db "VACUUM INTO '<copia>/patchbay.db'"` (copiar o
   arquivo cru perde o `-wal`). Copiar `<dados>/chave-mestra` ou passar a mesma `PATCHBAY_MASTER_KEY`.
2. Neutralizar na cópia o que tem efeito fora do processo:
   `UPDATE upstream SET habilitado = 0 WHERE modo_credencial = 'oauth';` (refresh token rotativo usado pela
   cópia invalida o do Go, e provedor com detecção de reuso revoga a família inteira) e desligar STDIO que
   segure recurso exclusivo (perfil de navegador, arquivo travado). Para testar OAuth de upstream, um
   upstream novo só na cópia, com consentimento próprio.
3. Subir com `PATCHBAY_LISTEN=127.0.0.1:8788` e `PATCHBAY_PUBLIC_URL=http://127.0.0.1:8788` (a variável
   vence a URL gravada em `settings`). Os tokens do AS emitidos pelo Go não valem aqui (o `resource` inclui a
   URL pública, `authsrv/servico.go:183`); chave de API vale.
4. Comparar ao vivo: o mesmo `tools/list` dos dois por chave de API (script no estilo `rpc.sh`), Claude
   Code apontado para o Rust por um dia de uso.

**Troca (F14):** parar o Go; `VACUUM INTO` de backup; subir a imagem Rust no mesmo volume e porta. Os
clientes não reconfiguram nada (URL, chaves e tokens iguais). **Rollback:** parar o Rust, subir a imagem Go
anterior no mesmo volume; funciona enquanto o esquema estiver congelado (2.1), e as concessões que o Rust
renovou estão nas mesmas colunas, legíveis pelo Go.

---

## 3. Novos repasses

### 3.1 Princípio

O gateway continua sem estado por sessão. Cada decisão de roteamento vem de uma de três fontes:

1. **Snapshot do catálogo** (como as ferramentas hoje): o gerente lista o upstream na supervisão
   (`list_all_*`), relista no `*/list_changed`, e o endpoint materializa com a composição. Nome de prompt e
   URI de resource se resolvem nesse snapshot.
2. **Identificador que o gateway reescreve e sabe desfazer**: `taskId`, `skill://`, `ui://` levam o upstream
   dentro de si. Sobrevive a reinício e a outra conexão, sem tabela.
3. **Estado por requisição**, que nasce e morre com ela: o mapa de progress token e o cancelamento.

Capacidades do endpoint: a união do que os upstreams vinculados declaram (`resources`, `prompts`,
`completions`, extensões `tasks`, `skills`, `ui`), com `listChanged: true` onde houver lista. Para o
upstream, o gateway repassa por requisição só o que sabe atravessar: `elicitation` (hoje),
`extensions.io.modelcontextprotocol/tasks` e `extensions.io.modelcontextprotocol/ui`. Nunca sampling nem roots
(o `shortcut` de `servidores.go:clienteAtende` continua valendo). Lista agregada vai numa página só, como
o `tools/list` de hoje; paginação própria (cursor opaco com deslocamento) só se um endpoint passar do limite
de 16 MiB do Claude Code.

Dica de cache (`ttlMs`/`cacheScope`) das listas agregadas: o menor `ttlMs` e o `cacheScope` mais restrito
entre os upstreams, nunca mais permissivo que o do upstream.

### 3.2 Por recurso

**Prompts (N1).** Mesmo tratamento das ferramentas: prefixo do vínculo, desambiguação de colisão
(`catalogo/normalizador.go:desambiguar`), mapa nome exposto → (upstream, nome original) no snapshot.
`prompts/get` repassa com o nome original e devolve `GetPromptResponse` inteiro, inclusive
`InputRequired` (`get_prompt_once`, `client.rs:1680`), com o mesmo filtro de elicitação das ferramentas.
As regras `excluir`/`renomear` valem só para ferramentas neste corte (decisão D8).

**Completion (N1 para prompt, N2 para resource).** `completion/complete` com `ref/prompt` resolve pelo
mapa de prompts e troca o nome exposto pelo original; com `ref/resource` (URI template), pelo dono do
template (abaixo). Resultado repassado. `completions` só é declarado se algum upstream declara.

**Resources e templates (N2).** `resources/list` e `resources/templates/list` agregados do snapshot. URI
**repassada sem reescrita** (`https://`, `file://` e esquemas próprios têm significado para o cliente).
Roteamento de `resources/read` por URI:
1. URI exata no snapshot → o upstream dono;
2. senão, o template cuja parte literal antes do primeiro `{` é o prefixo mais longo da URI;
3. senão, tentativa em ordem do vínculo entre os upstreams que declaram `resources`, primeira resposta sem
   `-32602` vence (cobre `resource_link` devolvido por ferramenta e fora da lista).
Colisão de URI entre upstreams: vence o primeiro na ordem do vínculo, e a tela do endpoint mostra o aviso
como já mostra colisão de ferramenta. `ReadResourceResponse::InputRequired` repassado. Assinatura de
resource (`resources/updated`) **fica de fora**: exigiria uma assinatura viva no upstream por assinatura de
cliente, e o Claude não suporta. `prompts/list_changed` e `resources/list_changed` seguem o caminho de
`tools/list_changed` (relistar, rematerializar, `listen`).

**Progress e cancelamento (N3).** O rmcp já põe um progress token novo em toda requisição de saída
(`service.rs:944`). O gateway assina esse token no `ProgressDispatcher` do cliente do upstream
(`handler/client/progress.rs:65`) e repassa cada `notifications/progress` ao cliente com o token **dele**,
pelo `Peer` da requisição de entrada; desassina no fim. Cancelamento: o `RequestContext.ct` do lado servidor
dispara quando o cliente fecha o stream; o gateway descarta o futuro da chamada de saída e o rmcp manda
`notifications/cancelled` ao upstream (ou fecha o stream, em 2026-07-28). Critério: o upstream falso registra
o cancelamento.

**Tasks (N4).** Só quando o cliente declara a extensão naquela requisição **e** o upstream a declarou no
discover; senão o gateway não repassa a extensão e o upstream responde síncrono, como hoje.
- `tools/call` devolve `CallToolResponse::Task` (`model/mrtr.rs:105`); o gateway troca o `taskId` T do
  upstream por `u<id do upstream em base36>.<T>.<mac>`, com `mac` = 10 caracteres base32 de
  HMAC-SHA256(chave derivada da mestra por HKDF com rótulo próprio, `slug | sujeito da credencial | upstream | T`).
  O upstream vê todos os clientes como um só (a credencial é do gateway); o `mac` é o que impede um cliente de
  ler ou cancelar a task de outro, e o que exige autorizar toda requisição de task. Sem tabela: sobrevive a
  reinício.
- `tasks/get`, `tasks/update`, `tasks/cancel`: desfaz o id, confere `mac` e que o upstream está vinculado ao
  endpoint da URL, repassa com T, troca T de volta na resposta. `-32602` para id inválido (o Claude lê como
  task desconhecida). `inputRequests` dentro da task passam pelo mesmo filtro de elicitação.
- Limite de 128 caracteres do Claude: com T de até ~110 cabe. `shortcut:` se `u…​.T.mac` passar de 128,
  responder `failed` com mensagem; trocar por tabela `tarefa_rota` só se um upstream real fizer isso.
- `notifications/tasks`: fora (o rmcp não roteia; o Claude faz poll).

**Skills (N5).** Upstream que declara `io.modelcontextprotocol/skills`: `skills/list` agregado com a URI
reescrita `skill://<skill-path>/<arquivo>` → `skill://<rótulo do vínculo>/<skill-path>/<arquivo>`, onde o
rótulo é o prefixo do vínculo (ou o nome normalizado do upstream). O último segmento do skill-path continua
igual ao `frontmatter.name`, como a spec exige, e o conteúdo não muda: digest e tamanho repassados intactos.
`skills/get`, `resources/read` e `resources/directory/read` de `skill://` tiram o primeiro segmento, roteiam,
e reescrevem as URIs da resposta (`uri` e cada `resources[].uri`). `directoryRead` declarado se algum upstream
declara; URI de upstream sem ele → `-32602`. Colisão de `name` entre upstreams (o Claude chama a skill de
`<servidor>:<nome>`, e o servidor é o endpoint): primeiro vence e a tela avisa (decisão D12). Teto de 100
skills por servidor do Claude: cortar e avisar.

**MCP Apps (N6).** Mesma reescrita para `ui://`: `ui://<rótulo>/<resto>` em `resources/list`, em
`resources/read` e no vínculo da ferramenta (`_meta.ui.resourceUri` e a chave antiga
`_meta["ui/resourceUri"]`). O snapshot é listado declarando a extensão `ui` ao upstream (para ele incluir
as ferramentas com UI); no `tools/list` de cada requisição, cliente que não declara a extensão recebe as
ferramentas sem `_meta.ui` e sem as de `visibility: ["app"]`. `_meta.ui.csp`, `permissions` e `domain`
repassados como vieram. A chamada que o app faz é um `tools/call` comum pelo mesmo endpoint.

**Trilha para os métodos novos (N7).** `call_log` só conhece ferramenta e `resultado IN ('ok','erro','timeout')`.
Depois da F15: migração `00019` com coluna `metodo` e o valor `pendente` (corrige também o pendente
"trilha marca rodada input_required como ok" do TASKS.md). Até lá, os métodos novos gravam com
`ferramenta = "prompt:<nome>"` / `"resource:<uri>"`.

### 3.3 Upstream da era antiga

| Upstream | O que passa | O que não passa e por quê |
|---|---|---|
| SSE 2024-11-05 | ferramentas, prompts, resources, templates, completion, progress, cancelamento | elicitação, tasks, skills, apps: não existem nessa revisão |
| HTTP/STDIO 2025-03-26 a 2025-11-25 | o mesmo, mais `outputSchema`/`structuredContent` | **elicitação**: o upstream pede por `elicitation/create` na sessão compartilhada, dentro da chamada. Atravessar até um cliente 2026-07-28 exigiria segurar a chamada do upstream aberta entre duas requisições do cliente, ou seja, estado de sessão. Fica como hoje: a sessão do gateway não declara elicitação e o `_meta` por requisição não vai (`gerente.go:910-913`). Tasks da 2025-11-25 (o rascunho com `tasks/result` e `tasks/list`) ≠ SEP-2663: tratar como síncrono |
| qualquer revisão, ferramenta lenta | resposta síncrona | o claude.ai corta em 240 s. Um gateway que criasse a task ele mesmo resolveria, mas guardaria o resultado (SQLite com TTL): é estado do gateway, decisão D9 |
| skills sem a extensão (só resources `skill://`) | os resources aparecem como resources comuns | sintetizar `skills/list` lendo SKILL.md e calculando digest de cada arquivo é possível na supervisão, mas não há upstream assim hoje: fora, até aparecer |

Cliente da era antiga falando com o gateway: o endpoint é sem sessão (`legacy_session_mode(false)`), então a
capacidade declarada no `initialize` não acompanha as requisições seguintes. O cliente legado não recebe
elicitação, tasks nem apps, como hoje.

---

## 4. Riscos e decisões

### 4.1 Riscos

| # | Risco | Mitigação |
|---|---|---|
| R1 | Lado a lado com cópia do banco gasta o refresh token rotativo e derruba a concessão do Go (ou a família inteira) | desligar upstreams OAuth na cópia (2.6); upstream OAuth de teste só na cópia |
| R2 | `revisao` do YAML diferente por detalhe de `json.Marshal` (nome de campo, escape HTML, nil vs vazio) | struct espelho com `#[serde(rename = "NomeGo")]`, escape próprio, teste com revisões gravadas do Go (2.3) |
| R3 | rmcp sobrescreve a capacidade por requisição; caminho com `PeerRequestOptions` pode não cancelar ao descartar | cena de caixa-preta na F5 (elicitação) e na N3 (cancelamento observado no upstream falso) |
| R4 | Unificação de features liga `serde_json/preserve_order` e muda a ordem das chaves frente ao Go | `cargo tree -e features -i serde_json` no CI; comparação por valor nas respostas, por texto só no HTML |
| R5 | `estilo_auth` 0 (autodetecção do x/oauth2) sem equivalente | mapeamento do 1.4 testado com linha gravada pelo Go nos três valores |
| R6 | Semântica fina do `http.CrossOriginProtection` e da proteção de Host divergir | cenas do `TestUI_CSRF` e do rebinding na caixa-preta, contra os dois |
| R7 | Memória do build: LTO `fat` com `codegen-units = 1` sobre rmcp + sqlx + askama + reqwest pode passar de `MemoryMax=3G` no homelab | teste em `dev`; release só no CI do GitHub (este repo não usa o runner do Shuttle); `lto = "thin"` se o CI apertar |
| R8 | Imagem multi-arch: sqlite embutido (C) exige compilador cruzado para arm64 | `cargo-zigbuild` no estágio de build com `--platform=$BUILDPLATFORM`; ou cortar arm64 (D4) |
| R9 | Comportamento dependente de tempo (backoff, 5 abandonos, lápide de 5 min, renovação) não aparece na caixa-preta | teste unitário com relógio falso, portando `resiliencia_test.go` e `oauth_fonte_interno_test.go` |
| R10 | Árvore de processo: zumbi ou neto vivo no Linux com `process-wrap` | os testes do neto do `stdioproc_test.go` transcritos; `pgrep` vazio ao parar |
| R11 | Spec em movimento: Skills é beta, Tasks muda com a SEP, rmcp fixado em 3.12 | repasses novos depois da troca e cada um atrás da capacidade declarada; atualizar rmcp é fatia própria |
| R12 | Segurança dos repasses novos: task de um cliente lida por outro; resource lido do upstream errado por colisão; skill e HTML de app são entrada não confiável | `mac` no taskId; ordem determinística + aviso de colisão; o gateway não executa nem interpreta skill/HTML, só repassa (como já faz com descrição de ferramenta) |

### 4.2 Decisões do Vitor

| # | Decisão | Opções | Recomendação |
|---|---|---|---|
| D0 | ~~Implantar o Go agora ou esperar o Rust~~ | — | **não se aplica**: o Go já está em produção no stack `mcp` do homelab (correção da F0) |
| D1 | Template | askama / maud / minijinja | askama (1.3) |
| D2 | Banco | sqlx (TASKS.md) / rusqlite | sqlx sem macros |
| D3 | Windows | manter (Job Object via `process-wrap`, sem CI) / só Linux e macOS | manter, custo quase zero; não testar |
| D4 | arm64 na imagem | manter / só amd64 | depende de onde roda: se só no homelab amd64, cortar e ganhar build simples |
| D5 | claude.ai e ChatGPT contra o Rust antes da troca | hostname público temporário (rota Cloudflare, com Access) / trocar só com caixa-preta + Claude Code | hostname temporário se já houver uso real em claude.ai/ChatGPT; senão dispensar |
| D6 | Repasses novos antes ou depois da troca | antes (o Rust já sai com mais) / depois | depois: a paridade fica limpa e a troca tem um risco só |
| D7 | URI de resource | repassar e primeiro vence / sempre com prefixo | repassar (3.2); prefixo só em `skill://` e `ui://` |
| D8 | Composição para prompts/resources/skills | só prefixo / regras e liga-desliga por vínculo (migração) | só prefixo no 1º corte; liga-desliga por vínculo depois da F15 |
| D9 | Task criada pelo próprio gateway para upstream lento ou antigo | não / sim (tabela com TTL ≥ 1 h) | não por ora; reavaliar se alguma ferramenta bater nos 240 s do claude.ai |
| D10 | Elicitação de upstream antigo | não / sim com estado | não (3.3) |
| D11 | Esquema congelado até apagar o Go | aceitar / migrar antes | aceitar; o pendente da trilha (`input_required` como ok) espera a N7 |
| D12 | Skill com mesmo nome em dois upstreams | primeiro vence / reescrever o `name` (muda o SKILL.md e o digest) | primeiro vence com aviso |
| D13 | Quando apagar o Go (F15) | logo após a troca / depois de N semanas | 2 semanas de uso sem rollback |

---

## 5. Estimativa

| | Fatias | Tamanho | Sessões (ordem de grandeza) |
|---|---|---|---|
| Port | F0–F15: 16 | 1 P, 10 M, 5 G (F4, F7, F9, F10, F11) | ~32 |
| Repasses novos | N1 prompts + completion de prompt (M), N2 resources + templates + completion de resource + list_changed (M), N3 progress e cancelamento (P), N4 tasks (M), N5 skills (M), N6 apps (P/M), N7 trilha dos métodos novos (P): 7 | 2 P, 1 P/M, 4 M | ~9 |
| **Total** | **23** | | **~41** |

Volume Rust esperado: ~27 mil de produção (o rmcp absorve descoberta OAuth, DCR, CIMD, PKCE, discover e
tasks; o transporte SSE e o middleware de bearer entram a mais), ~4,5 mil de templates askama, ~15 mil de
teste (tabelas transcritas + caixa-preta; não se portam as 27 mil de Go, metade delas é montagem
in-process que a caixa-preta substitui). Caminho crítico: F0 → F1 → F2 → F3 → F4 → F5 → F6 → F10 → F11 →
F12 → F14. F7, F8, F9 e F13 correm em paralelo depois da F6.

## Suposições e fora de escopo

- "Aviso de spec nova" entendido como a exibição da revisão da MCP e do servidor de cada upstream (PR #21);
  não achei outro aviso de revisão no código.
- File uploads (`io.modelcontextprotocol/files`) e `x-mcp-header` como repasse novo ficaram fora: o primeiro
  exige aprovação de hosts pela Anthropic (opcionais, seção 4), o segundo já atravessa como hoje (a
  normalização do Go é portada e o rmcp valida do lado cliente, `transport/common/mcp_headers.rs`).
- Não verificado em execução: o cancelamento de `send_request_with_option` ao descartar (R3) e o
  comportamento do rmcp servidor com cliente 2025-06-18 sem sessão. Os dois são critério de pronto da F5.
- Sessões por fatia são ordem de grandeza para planejar cota, não prazo.
