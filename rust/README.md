# patchbay em Rust

Port do patchbay de Go para Rust, em fatias (plano em `docs/estudos/2026-11-27-migracao-rust.md`).
Na F0 não há Rust de produção: o binário `patchbay` só imprime a versão. O que existe é a
**caixa-preta**, que julga a paridade de todas as fatias seguintes.

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

Sem `PATCHBAY_BIN` o alvo é o binário deste crate; até a F5 as cenas falham contra ele.

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

Nenhuma ainda. Toda diferença aceita entre o Rust e o Go entra aqui e em `DIFERENCAS_CONHECIDAS`
(`tests/apoio/mod.rs`), com o porquê.
