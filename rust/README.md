# patchbay em Rust

Port do patchbay de Go para Rust, em fatias (plano em `docs/estudos/2026-11-27-migracao-rust.md`).
A F0 é a **caixa-preta**, que julga a paridade de todas as fatias seguintes.

## O que já existe (F1, plataforma)

- `serve`: configuração por flag e ambiente (`src/config.rs`, flags com a semântica do `flag` do Go
  em `src/cli.rs`), chave mestra de `PATCHBAY_MASTER_KEY` ou de `<dados>/chave-mestra`
  (`src/chave.rs`), banco com as migrações do Go, canário, `GET /saude`, log JSON (ou texto) no
  formato do `slog`, desligamento no SIGTERM e reinício interno (`src/app.rs`).
- `src/plataforma/`: `store.rs` (dois pools sqlx com os pragmas do Go, tabela `settings`),
  `migracoes.rs` (executor compatível com a goose 3.28 sobre os mesmos `.sql` de
  `internal/platform/store/migracoes/`), `cripto.rs` (formato `pbc1:`, HKDF-SHA256, AES-256-GCM, AAD
  em uvarint, canário), `log.rs`.
- Subcomandos: `versao`, `ajuda`, `chave-mestra gerar`; `seed` até a validação (gravar entra com a
  F6); `export`, `import` e `biblioteca-semente` respondem "ainda não portado".

Testes: os de `cripto` (19) e `store` (10) do Go transcritos com os mesmos casos
(`cargo test -j 2 --bin patchbay`); `tests/fixtures/segredos-go.db` é um banco criado pelo Go com um
segredo de cada tipo, que o Rust decifra (o gerador, `tests/fixtures/gerar_segredos_go.go`, roda com
`go run` e não entra no build do Go). `tests/processo.rs` roda o binário Rust de fora: a chave fora de `/proc/<pid>/environ`, o accept
sem descritor livre (`ulimit -n 64`) e duas instâncias subindo juntas. A troca cruzada contra o
binário Go:

```sh
go build -p 2 -o /tmp/patchbay-go ./cmd/patchbay
PATCHBAY_GO=/tmp/patchbay-go cargo test -j 2 --test troca_com_go
```

## Caixa-preta

`tests/caixa_preta.rs` sobe o binário de `PATCHBAY_BIN` como processo (diretório de dados e porta
próprios, ambiente limpo, chave mestra fixa), configura pelos subcomandos e pela tela de
administração, e fala com ele por clientes MCP rmcp reais (2026-07-28 por `server/discover`,
2025-11-25 por `initialize`) e por HTTP cru. Nada sai de 127.0.0.1: `HTTP(S)_PROXY` aponta para uma
porta fechada, então a varredura da biblioteca (mcpservers.org) falha na hora.

```sh
cd /caminho/do/patchbay && go build -p 2 -o /tmp/patchbay-go ./cmd/patchbay
cd rust
PATCHBAY_BIN=/tmp/patchbay-go cargo test -j 2 --test caixa_preta                    # confere
PATCHBAY_BIN=/tmp/patchbay-go PATCHBAY_GRAVAR=1 cargo test -j 2 --test caixa_preta  # regrava do Go
PATCHBAY_MANTER=1 ...   # preserva o diretório de cada cena (banco e log) em $TMPDIR
```

Sem `PATCHBAY_BIN` o alvo é o binário deste crate; até a F5 só a cena `linha_de_comando` passa
contra ele (`cargo test -j 2 --test caixa_preta linha_de_comando`).

**Upstreams falsos** (`tests/apoio/`): HTTP streamable 2026-07-28 sem sessão e 2025-11-25 com
sessão (rmcp servidor, `falso_mcp.rs`), SSE 2024-11-05 escrito à mão, STDIO (o binário
`patchbay-upstream-falso`, mesmo código do HTTP), um que aceita e nunca responde, e um protegido por
um provedor OAuth falso (metadata RFC 9728/8414, DCR, authorize que consente na hora, token com PKCE
S256 e rotação de refresh). O catálogo é de propósito difícil: nome com espaço, 140 caracteres,
`x-mcp-header` válido, inválido e em tipo não primitivo, `inputSchema` que não é objeto,
`outputSchema`, imagem e áudio, 300 kB, `isError`, erro JSON-RPC, elicitação por MRTR e uma
ferramenta que pendura. A lista muda em execução e avisa por `subscriptions/listen`.

**Cenas** (um processo patchbay cada): `superficie_e_chamadas`, `cliente_2025_11_25`,
`autenticacao_no_endpoint`, `upstream_fora_do_ar_catalogo_vazio`, `lista_alterada_e_lapide`,
`elicitacao_atravessa_o_gateway`, `telas_de_acesso_e_csrf`, `oauth_do_as_dcr_e_cimd`,
`oauth_de_upstream_pela_tela`, `linha_de_comando`.

**Respostas gravadas**: `tests/respostas_go.json` (cena → passo → valor) e `tests/superficie.json`
(o `tools/list` agregado do cenário padrão). Antes de gravar, cada texto passa pelas trocas literais
(URL do patchbay e dos falsos, diretório da cena) e por máscaras de valor aleatório ou de relógio:
chave e tokens, instantes, portas, `expires_in`, família de tokens (vira rótulo pela ordem), hashes e
valores cifrados do banco. HTML entra com espaços colapsados, uma tag por linha. Depois de algumas
cenas o banco é despejado inteiro (menos a biblioteca), linhas ordenadas. A conferência é por valor
e aponta o caminho de cada diferença (`cena/passo.campo[i]`).

## Diferenças conhecidas

Nenhuma nas respostas gravadas. Toda diferença aceita entre o Rust e o Go entra aqui e em
`DIFERENCAS_CONHECIDAS` (`tests/apoio/mod.rs`), com o porquê.

O que depende de fatia ainda não portada fica em `PENDENTES` (mesmo arquivo), por passo ou por
trecho de passo (`cli/banco.upstream`), com a fatia que o traz. Não é diferença aceita: cada fatia
tira de lá o que portou, e a conferência avisa quando um pendente já confere.

Diferenças de comportamento fora das respostas gravadas, todas sem efeito para quem usa:

- pool de leitura com teto de 16 conexões (o `database/sql` do Go não tem teto);
- desligamento que passa de 10 s sai com `desligar: conexões ainda abertas depois de 10 s` (o Go
  diz `desligar: context deadline exceeded`);
- erro de escuta diz `servir: escutar em <endereço>: <erro do SO>` (o Go,
  `servir: listen tcp <endereço>: bind: ...`);
- flag inteira só em decimal (o Go aceita `0x`, `0o` e `_`);
- o Rust cria `<dados>/patchbay.db-trava-migracao` (trava `flock` entre instâncias que abrem e
  migram o banco juntas; o Go não trava e pode aplicar a 00017 duas vezes nessa corrida);
- o processo não é "dumpable" (`PR_SET_DUMPABLE=0`) e zera a chave no bloco do ambiente: outro
  processo do mesmo uid não lê `/proc/<pid>/environ` nem `/proc/<pid>/mem` (o Go deixa a chave lá);
- banco numa versão de esquema à frente do binário gera um aviso no log (o Go sobe calado);
- erro de accept espera de 5 ms a 1 s como o `net/http`, mas o aviso sai no máximo a cada 10 s
  (o Go loga toda tentativa);
- primeiro boot de duas instâncias com chaves diferentes: a que perde a corrida do canário recusa
  subir (`INSERT ... ON CONFLICT DO NOTHING` e releitura); no Go a última sobrescrevia;
- um texto de log com caractere de controle sai escapado como `\u{1b}` no formato texto (o Go,
  `\x1b`); o JSON é igual.
