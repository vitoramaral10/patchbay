# patchbay

Gateway MCP self-hosted: um endpoint agrega vários servidores MCP upstream e os
serve a um cliente de IA como se fossem um só.

Binário único, sem dependência de stack externa. Estado em SQLite embutido.

> Estado: **fatia 2** do épico — UI de administração mínima. Ainda não tem
> resiliência completa de upstream (fatia 3), composição fina de endpoint
> (fatia 4), upstream STDIO (fatia 5), cifra de segredo em repouso (fatia 6),
> OAuth de upstream (fatias 7-8) nem authorization server próprio
> (fatias 10-11).
>
> A especificação é `docs/estudos/2026-09-08-patchbay-estudo-previo.html`.

## O que já funciona

- `internal/platform/store` — SQLite com WAL, `busy_timeout`, dois pools
  (leitura livre, escritor único) e migrações `goose` embutidas em `embed.FS`.
- `internal/catalogo` — normalizador obrigatório entre o upstream e o SDK.
  `(*mcp.Server).AddTool` entra em panic em oito pontos com dado que vem do
  `tools/list` de terceiro; ferramenta que não normaliza é descartada com log.
- `internal/upstream` — uma sessão MCP por servidor HTTP configurado, conectada
  em goroutine de supervisão. Nenhuma operação de upstream no caminho da
  requisição do cliente. **Adicionar, reconfigurar e remover upstream valem em
  tempo de execução**, sem reiniciar o processo.
- `internal/endpoint` — um `*mcp.Server` e um `StreamableHTTPHandler` vivos por
  endpoint, servidos em `/mcp/{slug}` com sessão retida.
- `internal/apikey` — chave com prefixo legível, verificada por hash SHA-256,
  com escopo de endpoints, plugada em `auth.RequireBearerToken` do go-sdk.
  Credencial em query string vem desligada.
- `internal/admin` — administrador único com senha em argon2id, setup no
  primeiro acesso, sessão por cookie `HttpOnly`/`SameSite=Lax` em tabela com
  expiração, e o portão que protege as rotas de UI.
- `internal/platform/webui` — o layout da UI: tokens de cor semânticos em duas
  camadas, componentes de página, e htmx + extensão de SSE vendorizados dentro
  do binário.

## Administração pela tela

Toda a configuração é feita em `/admin/...`, servida pelo mesmo binário:

| Tela | O que faz |
|---|---|
| `/admin/setup` | Cria o administrador único. Existe **só** no primeiro acesso |
| `/admin/login` · `/admin/sair` | Entrada e saída |
| `/admin/` | Painel: upstreams por estado, endpoints, ferramentas, chaves |
| `/admin/upstreams` | CRUD de upstream HTTP; detalhe com estado, último erro e as ferramentas descobertas (nome exposto, nome original, descrição) |
| `/admin/endpoints` | CRUD de endpoint com composição de upstreams e contagem de ferramentas |
| `/admin/chaves` | Emissão de chave com escopo, comando `claude mcp add` pronto, revogação |

**Nada exige reiniciar o processo.** Criar, editar, desabilitar ou remover um
upstream reconfigura a supervisão na mesma requisição; mudar a composição de um
endpoint rematerializa o catálogo e dispara `tools/list_changed` para as sessões
vivas. A única exceção é editar o **nome, a descrição ou as instruções** de um
endpoint: esses três entram no `*mcp.Server` na construção e o SDK não permite
trocá-los numa instância viva, então o servidor é recriado e as sessões daquele
endpoint são encerradas — o formulário avisa.

O **slug do endpoint não muda** depois de criado. Ele está na URL, no caminho da
metadata RFC 9728 e no público-alvo de todo token daquele endpoint: renomear
invalidaria em silêncio a credencial de todos os clientes.

A chave de API aparece em texto claro **uma única vez**, na resposta da criação.
Ela é guardada como hash; depois disso só o prefixo visível continua na tela.

## Rodar

Precisa de Go 1.26 e, para o `Taskfile`, do [task](https://taskfile.dev).

```sh
task build                 # ou: go build -o patchbay ./cmd/patchbay
task run                   # escuta em 127.0.0.1:8787
```

Abra `http://127.0.0.1:8787/admin/` — sem administrador cadastrado, qualquer
rota de UI leva ao setup do primeiro acesso. De lá em diante o fluxo completo é
pela tela: upstream → endpoint → chave → o cliente MCP conecta.

O `patchbay seed` continua existindo como ferramenta de desenvolvimento (cria
endpoint, upstream e chave por SQL), mas **não é mais necessário**.

## Desenvolver o front-end

templ e o Tailwind CLI standalone. Nenhum Node, em nenhum momento — os dois são
binários baixados uma vez para `ferramentas/`, que o git ignora.

```sh
task ui:ferramentas   # go install do templ + download do Tailwind CLI oficial
task ui               # regenera *_templ.go e o CSS
task dev              # os dois em watch (deixe rodando; `task run` noutro terminal)
```

Os artefatos gerados — `*_templ.go` e `estatico/css/patchbay.css` — **são
versionados**, de propósito: `go build ./...` numa máquina que nunca viu templ
nem Tailwind precisa funcionar. Depois de editar um `.templ`, rode `task ui` e
commite o gerado junto.

htmx (2.0.7) e `htmx-ext-sse` (2.2.4) ficam em
`internal/platform/webui/estatico/vendor/`, com a versão no nome do arquivo e
servidos do `embed.FS`. Nunca de CDN: um binário único que precisa de rede
externa para desenhar a própria tela não é binário único.

### Visual

Duas camadas de token em `estatico/css/entrada.css`: os valores em `:root` e em
`@media (prefers-color-scheme: dark)`, e os utilitários do Tailwind em
`@theme inline`. Componente só toca a camada de cima (`bg-superficie`,
`text-conteudo-suave`) — não existe um `dark:` no markup, e trocar o tema é
trocar valores, não classes. Tema claro e escuro por `prefers-color-scheme`, sem
alternador. Todo par de cor usado para texto foi **medido** em 4.5:1, e borda de
campo e anel de foco em 3:1.

## Configuração

Flags e variáveis de ambiente, sem arquivo lido no boot. Flag ganha do ambiente.

| Flag | Variável | Padrão |
|---|---|---|
| `-listen` | `PATCHBAY_LISTEN` | `127.0.0.1:8787` |
| `-data-dir` | `PATCHBAY_DATA_DIR` | `./dados` |
| `-public-url` | `PATCHBAY_PUBLIC_URL` | derivada de `-listen` |
| `-log-level` | `PATCHBAY_LOG_LEVEL` | `info` |
| `-log-texto` | `PATCHBAY_LOG_TEXTO` | JSON |

A URL pública é o que a UI mostra aos clientes e o que decide o atributo
`Secure` do cookie de sessão: o TLS é terminado por um proxy reverso na frente
do patchbay, então o processo não descobre o esquema externo olhando a
requisição.

## Segurança

- **CSRF**: `http.NewCrossOriginProtection` (nativo desde Go 1.25) envolve o mux
  inteiro. Requisição de navegador com método não seguro e `Sec-Fetch-Site`
  cross-site é recusada com 403; o htmx passa porque um `hx-post` é um fetch da
  própria página e manda `same-origin`. Nenhum token de formulário, nenhuma
  dependência.
- **Senha do admin**: argon2id com 64 MiB, dois passes e quatro lanes, no formato
  PHC — os custos viajam junto com o hash, então trocá-los no futuro não
  invalida a senha gravada.
- **Sessão de admin**: token de 256 bits sorteado, guardado como hash SHA-256,
  cookie `HttpOnly` + `SameSite=Lax`, validade absoluta de 12 horas, varredura
  horária das vencidas.
- **Duas classes de segredo**: o que o patchbay *verifica* (chave de API, sessão)
  vai como hash; o que ele *apresenta* (bearer de upstream, token OAuth) exige
  cifra reversível e é da fatia 6 — por isso o formulário de upstream ainda não
  tem campo de credencial.

## Verificar

```sh
task verifica   # go vet + golangci-lint + go test -race
```

`-race` exige cgo; no Windows sem compilador C, rode os testes por WSL ou em
Linux.

## Layout

`cmd/patchbay` monta o grafo de dependências e é o único que conhece todas as
features. Cada `internal/<feature>` declara a interface mínima do que consome e
não importa outra feature; `internal/platform/*` é infraestrutura e não conhece
feature nenhuma. Um teste em `internal/arquitetura` quebra o build se isso
mudar.

As telas de administração moram na feature que elas administram
(`internal/upstream/admin.templ`, `internal/endpoint/admin_http.go`, …) e não num
pacote de UI separado: o CRUD de upstream precisa do `Gerente`, que é do próprio
pacote. O que é comum — o shell da página, os tokens, os campos de formulário —
está em `internal/platform/webui`, que não sabe o que é um upstream.
