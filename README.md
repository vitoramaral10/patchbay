# patchbay

Gateway MCP self-hosted: um endpoint agrega vários servidores MCP upstream e os
serve a um cliente de IA como se fossem um só.

Binário único, sem dependência de stack externa. Estado em SQLite embutido.

> Estado: fatias **1-14 e 16** do épico entregues — catálogo e endpoint,
> resiliência de upstream, composição fina do endpoint, upstream STDIO com
> supervisor de processo, segredos cifrados em repouso, **OAuth de upstream
> com consentimento pela tela, refresh serializado, CIMD e registro
> dinâmico**, a **sonda de saúde funcional** (um `tools/call` de verdade por
> servidor, opt-in), o authorization server completo (CIMD, DCR e redirect URI
> de loopback incluídos), o **transporte SSE legado**, a observabilidade
> (trilha por chamada, tela filtrável, log ao vivo por SSE e redação de
> segredo), o export/import da configuração em YAML e o empacotamento (binário
> multiplataforma por `goreleaser`, imagem Docker distroless).
>
> Fora da v1: a **fatia 15** (provedor loopback-only, tipo Canva) foi adiada por
> decisão do dono em 2026-09-08 — redirect só-loopback exigiria um binário
> auxiliar na máquina do admin ou colar o `code` à mão na UI, e nenhuma fonte da
> pesquisa cobre esse cenário.
>
> A especificação é `docs/estudos/2026-09-08-patchbay-estudo-previo.html`.

> **O patchbay não sobe sem `PATCHBAY_MASTER_KEY`.** Gere a chave uma vez com
> `patchbay chave-mestra gerar` e guarde-a onde você guarda segredo de produção.
> Se ela mudar, o processo **se recusa a subir** — ver [Chave mestra](#chave-mestra).

## O que já funciona

- `internal/platform/store` — SQLite com WAL, `busy_timeout`, dois pools
  (leitura livre, escritor único) e migrações `goose` embutidas em `embed.FS`.
- `internal/catalogo` — a **composição fina** do endpoint (filtro por padrão,
  renomeação e prefixo, por endpoint) e o normalizador obrigatório entre o
  upstream e o SDK.
  `(*mcp.Server).AddTool` entra em panic em oito pontos com dado que vem do
  `tools/list` de terceiro; ferramenta que não normaliza é descartada com log.
  Ferramenta que sai do catálogo deixa uma **lápide** por uma janela de graça:
  continua listada e responde com um erro de ferramenta explicando que saiu, em
  vez de o cliente receber `unknown tool` e concluir que o endpoint quebrou.
- `internal/platform/stdioproc` — o supervisor de processo: **process group** no
  Linux e no macOS, **Job Object** no Windows, com `KILL_ON_JOB_CLOSE`. É o
  único pacote que fala `syscall`/`golang.org/x/sys/windows`, e existe separado
  para o supervisor ser testável com um processo que pendura de propósito, sem
  MCP no meio. `(*os.Process).Kill` mata só o filho direto e deixa o **neto**
  vivo — `npx` lança `node`, `uvx` lança `python` —, e é esse neto que vazava um
  processo por reconexão até esgotar os PIDs da máquina no gateway anterior.
- `internal/upstream` — uma sessão MCP por servidor configurado, conectada
  em goroutine de supervisão. Nenhuma operação de upstream no caminho da
  requisição do cliente. **Adicionar, reconfigurar e remover upstream valem em
  tempo de execução**, sem reiniciar o processo. A máquina de estados da seção
  05 completa: watchdog de conexão que abandona o `Connect` preso (issue #1189
  do go-sdk), contador de connects abandonados com teto e desabilitação
  automática, e backoff exponencial próprio com jitter e teto — sem
  `cenkalti/backoff`, com relógio injetado. **Nenhum estado de erro é
  persistido:** o banco guarda só `habilitado`, e todo boot recomeça em
  `novo → conectando`. **Três transportes na mesma máquina de estados:**
  Streamable HTTP, SSE legado (o HTTP+SSE da revisão 2024-11-05) e STDIO — um
  processo por servidor, compartilhado por todas as sessões de cliente, com a
  árvore inteira morrendo junto e restart pelo mesmo backoff.
  **OAuth de upstream** com consentimento pela tela, uma `oauth2.TokenSource`
  por upstream para o processo inteiro, refresh serializado e proativo na
  supervisão, e o estado `sem_consentimento` para o que depende de um clique —
  ver [OAuth de upstream](#oauth-de-upstream).
  A **sonda de saúde funcional**, opt-in por servidor, executa um `tools/call` de
  verdade e leva o upstream a `sonda_falhou` quando a chamada para de funcionar —
  ver [Sonda de saúde funcional](#sonda-de-saúde-funcional).
- `internal/endpoint` — um `*mcp.Server` e um `StreamableHTTPHandler` vivos por
  endpoint, servidos em `/mcp/{slug}` com sessão retida. **Catálogo parcial
  servido sem hesitar:** endpoint com três upstreams e um degradado serve as
  ferramentas dos outros dois, e `tools/list` vazio é resposta legítima quando
  nenhum está pronto — nunca erro. Rematerializar dispara `tools/list_changed`
  e recolhe as lápides vencidas.
- `internal/apikey` — chave com prefixo legível, verificada por hash SHA-256,
  com escopo de endpoints, plugada em `auth.RequireBearerToken` do go-sdk.
  Credencial em query string vem desligada.
- `internal/authsrv` — o **authorization server OAuth 2.1** embutido: PKCE S256
  obrigatório, metadata RFC 8414 e RFC 9728 por endpoint, `resource` do RFC 8707
  no `aud`, rotação de refresh com família e detecção de replay, revogação
  RFC 7009, e as três formas de um cliente existir — cadastrado na tela,
  **CIMD** (o documento que o próprio cliente publica) e **DCR** (RFC 7591,
  deprecado mas mantido). Ver [Authorization server](#authorization-server).
- `internal/admin` — administrador único com senha em argon2id, setup no
  primeiro acesso, sessão por cookie `HttpOnly`/`SameSite=Lax` em tabela com
  expiração, e o portão que protege as rotas de UI.
- `internal/platform/cripto` — a cifra de campo: AES-256-GCM com chave derivada
  por HKDF-SHA256 da chave mestra, nonce sorteado por valor, formato de
  armazenamento versionado e AAD com a linha de origem. Mais o canário que
  detecta chave mestra trocada no boot.
- `internal/configuracao` — o **export e o import do YAML versionável**, com
  `--dry-run`, trava otimista por item e mescla. O arquivo nunca é lido no boot:
  aplicar é sempre uma operação explícita, e o plano aparece antes de qualquer
  escrita. Segredo não sai no arquivo — o que sai é o nome da variável de
  ambiente de onde o import lê cada credencial. Ver
  [Export e import em YAML](#export-e-import-em-yaml).
- `internal/trilha` — a observabilidade: uma linha por `tools/call` gravada
  **fora do caminho da latência**, o log ao vivo por SSE e a redação de segredo
  dos dois. A captura no caminho da requisição é um envio não bloqueante num
  canal com buffer; um consumidor único grava em lote no pool de escrita, e ao
  encher a fila descarta **contando** — o contador aparece na tela. Ver
  [Observabilidade](#observabilidade).
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
| `/admin/upstreams` | CRUD de upstream HTTP e SSE (bearer e headers estáticos, ou OAuth) e STDIO (comando, argumentos e ambiente); detalhe com estado, último erro, próxima tentativa, falhas consecutivas, connects abandonados e as ferramentas descobertas (nome exposto, nome original, descrição); botão **Reconectar** que descarta a sessão e rearma a supervisão na hora, e botão **Autorizar** no modo OAuth |
| `/admin/endpoints` | CRUD de endpoint com composição fina — quais upstreams entram, com que prefixo e com que regras de filtro/renomeação — e a contagem de ferramentas do endpoint e de cada upstream dentro dele |
| `/admin/chaves` | Emissão de chave com escopo, comando `claude mcp add` pronto, revogação |
| `/admin/oauth` | Clientes do authorization server: cadastro à mão, e as linhas que aparecem sozinhas por **CIMD** ou **DCR** — a coluna Origem diz qual é qual. Detalhe com a allowlist de redirect, o escopo, as sessões vivas e a revogação de cliente ou de sessão |
| `/admin/configuracao` | Baixa o YAML da configuração e importa um colado, mostrando o plano item a item antes de aplicar |
| `/admin/trilha` | Trilha por chamada de ferramenta, filtrável por endpoint, upstream, ferramenta, resultado, origem (cliente ou sonda) e período, com os contadores de chamadas por minuto, erros, timeouts e **descartes** |
| `/admin/logs/ao-vivo` | Log do processo e chamadas de ferramenta em tempo real, por SSE, com token e header de autorização redigidos |
| `/admin/upstreams/oauth/callback` | Onde o provedor devolve o navegador depois do consentimento OAuth de upstream. Atrás da sessão de admin, como o resto de `/admin` |

Uma rota fora de `/admin` pertence ao OAuth de upstream:
`/oauth/patchbay-cliente.json` é o Client ID Metadata Document do patchbay como
cliente. É público porque quem o lê é o authorization server do provedor, e ele
só existe quando a URL pública é `https://`.

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

## Composição do endpoint

Cada upstream entra num endpoint com um **prefixo** e uma lista de **regras**, e
as duas coisas valem só naquele endpoint: o mesmo upstream pode compor `pessoal`
inteiro e compor `trabalho` com três ferramentas renomeadas. O catálogo é
descoberto **uma vez por upstream** e composto por endpoint — nenhuma operação
de upstream acontece no caminho da requisição do cliente.

Uma regra por linha, no formulário do endpoint:

```
excluir  write_*                  # tira do endpoint tudo que casa
incluir  write_seguro             # exceção, se vier antes do excluir
renomear buscar_no_notion buscar  # troca o nome-base
renomear notion_* nt.*            # o * do renome recebe o que o * do padrão casou
```

O `*` casa qualquer trecho, inclusive vazio, e é o único metacaractere — não é
regex. O padrão casa contra o **nome original no upstream**, nunca contra o nome
já prefixado; e por ser separado por espaço, um padrão não pode conter espaço.

A ordem em que tudo é aplicado, e ela é fixa:

1. **filtro** — vale a primeira regra `incluir`/`excluir` que casa. O que não
   casa com nenhuma **entra**. Para deixar só um conjunto, feche a lista com
   `excluir *` no fim — a linha que apaga o resto é uma linha que você escreveu
   e vê, em vez de um modo implícito;
2. **renomeação** — vale a primeira regra `renomear` que casa. Só muda o nome
   exposto: o `tools/call` de saída continua usando o nome do upstream;
3. **prefixo** do upstream naquele endpoint;
4. **saneamento** do nome pelas regras do SDK;
5. **desambiguação** — nome já usado no endpoint ganha sufixo `_2`, `_3`. É a
   última etapa de propósito: ela vê o nome final, então prefixo e renome
   resolvem a colisão antes de precisar de sufixo.

O resultado é determinístico — a ordem dos upstreams na composição decide quem
fica com o nome disputado —, porque o nome exposto é contrato: o cliente pode
tê-lo em cache de prompt. A desambiguação roda em duas passadas: primeiro
reserva nome quem manteve o nome nativo do upstream, depois quem foi
renomeado por regra. Sem isso, uma regra de renome poderia roubar o nome de
uma ferramenta nativa do mesmo upstream dependendo só da ordem em que o
`tools/list` a listou — o nome nativo é o que o cliente já pode ter em cache
de antes de a regra existir.

A contagem aparece **duas vezes** na tela do endpoint: quantas ferramentas ele
expõe no total e quantas cada upstream entrega *àquele endpoint*. Sem o segundo
número, um filtro que apagou tudo é indistinguível de um upstream que não
conectou. Acima de 40 ferramentas o número aparece destacado: é custo de contexto
que todo cliente daquele endpoint paga.

Mudar prefixo, regra ou composição **vale na hora**, na mesma instância de
`*mcp.Server` — as sessões abertas sobrevivem e recebem `tools/list_changed`. O
nome que sai do catálogo por uma mudança de prefixo ou de filtro deixa a lápide
da fatia 3: continua respondendo por uma janela de graça, explicando que saiu.

## Rodar

Precisa de Go 1.26 e, para o `Taskfile`, do [task](https://taskfile.dev).

```sh
task build                                        # ou: go build -o patchbay ./cmd/patchbay
export PATCHBAY_MASTER_KEY=$(task -s chave-mestra) # uma vez; guarde a chave
task run                                          # escuta em 127.0.0.1:8787
```

Abra `http://127.0.0.1:8787/admin/` — sem administrador cadastrado, qualquer
rota de UI leva ao setup do primeiro acesso. De lá em diante o fluxo completo é
pela tela: upstream → endpoint → chave → o cliente MCP conecta.

O `patchbay seed` continua existindo como ferramenta de desenvolvimento (cria
endpoint, upstream e chave por SQL), mas **não é mais necessário**.

## Upstream STDIO

Um servidor MCP que não fala HTTP: o patchbay o executa como processo filho e
conversa com ele pelo stdin e stdout. É o caso de `@modelcontextprotocol/server-filesystem`,
dos servidores lançados por `npx` e `uvx`, e de qualquer binário local.

**Um processo por upstream, compartilhado por todas as sessões de cliente.** Não
um por sessão: spawn por sessão é o que vazava um processo vivo por reconexão até
esgotar os PIDs da máquina no gateway anterior. O `jsonrpc2` do go-sdk já
multiplexa chamadas concorrentes sobre uma sessão só, então não há multiplexador
para escrever — o que sobra é o ciclo de vida do processo.

### Cadastrar

Em `/admin/upstreams`, botão **Novo processo STDIO**. O formulário pede:

| Campo | O quê |
|---|---|
| **Comando** | O programa, resolvido pelo `PATH` do processo patchbay: `npx`, `uvx`, `node`, ou um caminho completo |
| **Argumentos** | **Um por linha.** Nada de linha de comando partida por espaço — argumento com espaço dentro é normal (`C:\Arquivos de Programas\a.js`), e um separador aqui viraria uma regra de escape para você descobrir errando |
| **Variáveis de ambiente** | `NOME=valor`, uma por linha. Vão **em claro** no banco e aparecem na tela: é o lugar de `NODE_ENV`, nível de log, `PATH` extra |
| **Variáveis sensíveis** | Mesmo formato dos headers estáticos de um upstream HTTP: cifradas em repouso, nunca reexibidas, campo em branco mantém o gravado, apagar é explícito pelo *limpar* |
| **Timeout** | Vale para conectar, listar e chamar ferramenta neste upstream |

Exemplo de servidor de arquivos:

```
Comando:     npx
Argumentos:  -y
             @modelcontextprotocol/server-filesystem
             C:\dados
```

O processo **herda o ambiente do patchbay** e recebe as variáveis configuradas
por cima. Sem a herança, um servidor lançado por `npx` não acharia nem o próprio
interpretador — o erro que apareceria seria `executable file not found`, que não
diz nada sobre a causa.

Toda variável com prefixo `PATCHBAY_` fica de fora dessa herança, sempre —
inclusive `PATCHBAY_MASTER_KEY`. Um upstream stdio é um binário de terceiro, e
receber a chave que cifra os próprios segredos do patchbay no ambiente seria
entregar a chave do cofre para quem só devia ver o conteúdo já decifrado.

O **tipo não muda depois de criado**. Trocar o transporte de um upstream vivo não
é editá-lo, é substituí-lo: outras ferramentas, outras credenciais, outro modo de
falha. Para trocar, crie outro upstream e recomponha os endpoints.

### O que o supervisor garante

- **A árvore inteira morre junto.** O processo nasce num *process group* próprio
  (Linux, macOS) ou num *Job Object* com `KILL_ON_JOB_CLOSE` (Windows). Ao
  encerrar — desligamento, remoção, reconfiguração, hang —, morre o processo
  **e todo neto que ele tenha lançado**. `npx` lança `node`; matar só o `npx`
  deixaria o `node` rodando para sempre.
- **A despedida é a do protocolo, e só depois vem a força.** Fechar a sessão
  fecha o stdin do processo, que é o que a especificação do transporte STDIO
  pede; o supervisor espera ele sair sozinho e só então varre a árvore.
- **Hang é falha.** Um processo vivo e mudo não morre sozinho, então liveness de
  PID não o detecta — no Unix `os.FindProcess` sempre devolve sucesso e no
  Windows não há equivalente de sinal 0. Quem detecta é o timeout: passado ele, o
  upstream vai a **degradado**, a árvore é morta e o backoff é agendado.
- **Restart é o mesmo backoff.** Processo que morre sozinho derruba a sessão, o
  upstream vai a degradado, e a tentativa seguinte sobe um processo novo —
  exponencial com jitter e teto, com o teto de connects abandonados levando à
  desabilitação automática, exatamente como num upstream HTTP.
- **O stderr do processo vai para o log** em nível `debug`, linha a linha e com
  tamanho limitado. Sem isso, um servidor que morre na primeira linha morre em
  silêncio.
- **Timeout de chamada não mata o processo.** Ele é compartilhado por todas as
  sessões; derrubá-lo por uma chamada lenta trocaria um problema pequeno por um
  grande. Só a chamada falha.

Isolamento em container está fora do escopo da v1 — o patchbay roda o processo
nu, e é por isso que a morte de árvore é obrigatória e não opcional.

## OAuth de upstream

Upstream HTTP ou SSE tem dois **modos de credencial**, escolhidos na edição e
excludentes: `estatica` — bearer e headers colados por você — e `oauth`. São
excludentes porque os dois montam o mesmo header `Authorization`, e um servidor
que recebe dois escolhe um sem dizer qual: o sintoma seria 401 intermitente que
ninguém liga a um formulário salvo semanas antes.

No modo OAuth o patchbay é um **cliente** OAuth 2.1 com PKCE. A descoberta
(RFC 9728 e RFC 8414), a ordem de registro de cliente e o refresh são do
`go-sdk` e do `golang.org/x/oauth2`; o que o patchbay acrescenta é o
consentimento pelo navegador de um admin que está em outra máquina, e a
persistência cifrada do que ele produz.

### O fluxo

1. Cadastre o upstream com o modo **OAuth** e salve.
2. Na tela do upstream, clique em **Autorizar**. O patchbay descobre o
   authorization server do provedor, registra ou reaproveita o cliente, monta a
   URL de autorização com `state` e `code_challenge`, e leva você para lá.
3. Você autoriza no provedor. Ele devolve o navegador para
   `<URL pública>/admin/upstreams/oauth/callback`.
4. A troca do código por token acontece na **supervisão**, não na requisição do
   navegador. Assim que ela termina, o upstream sai de `sem_consentimento` e
   conecta.

O `redirect_uri` é **contrato**: registre no provedor exatamente
`<URL pública>/admin/upstreams/oauth/callback`. Mudar `PATCHBAY_PUBLIC_URL`
depois invalida todo consentimento existente.

### Qual client_id

A ordem é a da especificação, e é o `go-sdk` quem a executa: **Client ID
Metadata Document → cliente pré-registrado → registro dinâmico**.

| Caminho | Quando | O que você faz |
|---|---|---|
| CIMD | A URL pública é `https://` e o provedor anuncia suporte | Nada. O documento é servido em `/oauth/patchbay-cliente.json` |
| Pré-registrado | O provedor não anuncia CIMD nem `registration_endpoint` — o caso do Google | Cola `client_id` e `client_secret` no formulário |
| Registro dinâmico | O provedor anuncia `registration_endpoint` | Nada. O `client_id` emitido fica gravado e é reusado |

**Pré-registrado é caminho normal, não recuperação de erro.** O Google não
anuncia nenhuma das duas alternativas, então sem colar `client_id` e
`client_secret` à mão ele simplesmente não funciona.

O `client_id` que o registro dinâmico emitiu é **persistido e reusado**: sem
isso, cada reautorização registraria mais um cliente no provedor e o cadastro
de lá viraria uma lista de clientes órfãos.

O campo **issuer** é opcional e só vale com `client_id` pré-registrado. Quando
preenchido, o patchbay recusa usar aquela credencial com um authorization
server diferente daquele — é a proteção contra confundir dois provedores.

Sobre `http://` o CIMD fica **desligado**, e o boot diz isso no log: um
`client_id` que é uma URL só vale como identidade se ninguém no caminho puder
trocar o documento.

### Refresh, revogação e o estado `sem_consentimento`

- **Uma `oauth2.TokenSource` por upstream, para o processo inteiro.** É a
  unicidade da instância que serializa o refresh: duas instâncias para o mesmo
  upstream fariam duas requisições correrem com o mesmo refresh token, e
  provedor com rotação de família revoga tudo quando vê um token repetido.
- **O refresh roda na supervisão, com antecedência.** Nenhuma operação de
  upstream acontece no caminho da requisição de um cliente, e um token vencido
  na hora do `tools/call` faria o cliente pagar a ida ao token endpoint.
- **O que é gravado é o token que a `TokenSource` devolve**, nunca o corpo da
  resposta HTTP: o `x/oauth2` já não sobrescreve `refresh_token` com valor
  vazio, e parsear a resposta por conta própria seria a única forma de errar
  isso.
- **Refresh recusado com `invalid_grant`** — consentimento revogado ou expirado
  — apaga o token gravado e leva o upstream a `sem_consentimento`.

Em `sem_consentimento` a supervisão **para**: não há backoff, não há próxima
tentativa agendada, e nenhuma requisição é enviada ao provedor. O que falta é
uma pessoa autorizando, e reconectar só para tomar 401 não produz uma. O clique
em **Autorizar** é que reagenda a conexão, na hora.

### O que fica no banco

Tudo em `upstream_oauth`, uma linha por upstream. `client_secret`, access token
e refresh token vão cifrados com AES-256-GCM, com a **coluna dentro do dado
autenticado** — um access token transplantado para a coluna de refresh não
decifra nem com a chave certa. `client_id`, `issuer`, por qual caminho o
cliente foi registrado, o prazo do token e a hora do último refresh ficam em
claro: a tela precisa deles, nenhum é segredo, e assim ela continua abrindo
depois de uma troca de chave mestra — que é justamente quando ela é mais
necessária.

Nenhum valor de token aparece em log, em tela ou em mensagem de erro.

O `state` e o `code_verifier` do PKCE **não** têm tabela. O verifier vive dentro
do handler do `go-sdk` e não é exposto; persistir o `state` sem ele daria uma
linha inútil, e persistir os dois exigiria reimplementar a troca do código por
token por fora da biblioteca — que é exatamente onde se reintroduz o bug que ela
já não tem. Os dois têm o mesmo tempo de vida: a tentativa em curso.

### Upstream SSE legado

O tipo `sse` é o HTTP+SSE da revisão 2024-11-05 do MCP: um GET pendurado com os
eventos do servidor e um POST por mensagem do cliente, no endereço que o
primeiro evento anuncia. Ele entra na mesma máquina de estados dos outros dois
transportes — mesmo watchdog, mesmo backoff, mesmo teto de abandonos, mesma
tela — e aceita as mesmas credenciais, estáticas ou OAuth.

Duas diferenças ficam contidas em `internal/upstream/sse.go`: o
`SSEClientTransport` do go-sdk amarra o stream ao contexto da chamada de
`Connect`, então o patchbay o desamarra antes de entregá-lo ao supervisor; e ele
não tem campo `OAuthHandler`, então o token entra por `RoundTripper`.

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

E uma variável **obrigatória**, sem flag equivalente:

| Variável | O quê |
|---|---|
| `PATCHBAY_MASTER_KEY` | Chave mestra de cifra, 32 bytes em base64. `serve`, `seed`, `export` e `import` não sobem sem ela |

Ela não tem flag de propósito: argumento de processo aparece em `ps` e no
histórico do shell. E não tem arquivo de chave em disco nem entrada pela UI —
a origem é essa variável e só ela.

A URL pública é o que a UI mostra aos clientes, o que decide o atributo `Secure`
do cookie de sessão e o que monta o `redirect_uri` do OAuth de upstream: o TLS é
terminado por um proxy reverso na frente do patchbay, então o processo não
descobre o esquema externo olhando a requisição. **Mudá-la invalida o
consentimento OAuth de todo upstream** — o `redirect_uri` registrado no provedor
deixa de bater.

## Instalação e deploy

Três formas de rodar em produção, do mais simples ao mais isolado.

### Binário

Baixe o arquivo da plataforma em
[Releases](https://github.com/vitoramaral10/patchbay/releases) — `linux_amd64`,
`linux_arm64`, `darwin_arm64` ou `windows_amd64` — e confira o checksum contra
`patchbay_<versão>_checksums.txt`, publicado junto. O `.goreleaser.yaml`
compila os quatro com `CGO_ENABLED=0`, então não há biblioteca nativa para
instalar antes.

```sh
tar -xzf patchbay_<versão>_linux_amd64.tar.gz
export PATCHBAY_MASTER_KEY=...        # ver "Chave mestra" abaixo
export PATCHBAY_DATA_DIR=/var/lib/patchbay
export PATCHBAY_PUBLIC_URL=https://patchbay.exemplo.com
./patchbay serve
```

O TLS **não** é terminado pelo patchbay: coloque um proxy reverso na frente
(Caddy, Nginx, Traefik) e aponte `PATCHBAY_PUBLIC_URL` para o esquema e host
públicos — é o que a UI mostra ao cliente MCP e o que decide o atributo
`Secure` do cookie de sessão de admin.

### Docker

A imagem publicada é `ghcr.io/vitoramaral10/patchbay`, multi-arch
(`linux/amd64`, `linux/arm64`), a partir de `gcr.io/distroless/static-debian12:nonroot`
— sem shell, sem gerenciador de pacotes, processo como usuário não-root.

```sh
docker run --rm \
  -e PATCHBAY_MASTER_KEY=... \
  -e PATCHBAY_PUBLIC_URL=https://patchbay.exemplo.com \
  -p 8787:8787 \
  -v patchbay-dados:/dados \
  ghcr.io/vitoramaral10/patchbay:latest
```

Sem `HEALTHCHECK` no `Dockerfile`: a base distroless não tem `curl` nem shell
para escrevê-lo. O Kubernetes ignora `HEALTHCHECK` de qualquer forma — a sonda
de vida/prontidão é HTTP direta contra o gateway; em Compose, veja o exemplo
abaixo, que não depende de sonda alguma para subir.

### Docker Compose (com TLS)

`docker-compose.yml` sobe o patchbay e um Caddy na frente, com TLS automático
via ACME e o hardening de runtime que o Dockerfile sozinho não consegue
impor: `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges`, rootfs
gravável só no volume do data dir.

```sh
cp secrets.env.exemplo secrets.env
docker compose run --rm patchbay chave-mestra gerar   # cole o resultado em secrets.env
$EDITOR Caddyfile                                     # troque pelo seu domínio
docker compose up -d
```

### Chave mestra: onde guardar e o que acontece se perder

A chave mestra é gerada uma única vez pelo próprio binário
(`cmd/patchbay/chave.go`), não por uma ferramenta externa:

```sh
patchbay chave-mestra gerar   # ou: docker compose run --rm patchbay chave-mestra gerar
```

Guarde-a como segredo de produção — gerenciador de segredo do provedor de
nuvem, `secrets.env` fora do controle de versão, cofre de senha da equipe —
nunca em texto plano num repositório. **Perdê-la é perder tudo o que ela cifra
em repouso**: bearer e headers de upstream, e futuramente tokens de OAuth. Não
existe recuperação nem chave mestra alternativa; sem ela, o caminho é apagar o
banco e recadastrar upstreams do zero. Detalhes de por que a falha é dura e
como o canário detecta chave trocada estão em [Chave mestra](#chave-mestra).

### Atualizar a versão do Go

`modernc.org/sqlite` depende de `modernc.org/libc`, que segue a versão do Go
do `go.mod`. Ao subir o `go` do `go.mod` (e o `golang:*` do `Dockerfile`),
rode `go get -u modernc.org/libc modernc.org/sqlite && go mod tidy` e reveja o
changelog de `modernc.org/libc` — ele já quebrou build em bump de minor do Go
por depender de detalhe de runtime não exportado.

## Export e import em YAML

A configuração inteira — upstreams, endpoints e a composição de cada um — sai
num YAML versionável, e volta dele. **O arquivo nunca é lido no boot.** Aplicar é
sempre uma operação explícita, e é isso que elimina a pergunta "quem ganha, o
arquivo ou a tela": não existe configuração em vigor que a UI não mostre.

```sh
patchbay export -o patchbay.yaml                 # ou para a saída padrão
patchbay export -o patchbay.yaml --forcar        # sobrescreve se já existir
patchbay import patchbay.yaml --dry-run          # mostra o plano, não escreve
patchbay import patchbay.yaml                    # aplica
patchbay import patchbay.yaml --remover-ausentes # e apaga o que não está no arquivo
```

Os mesmos dois passos estão em `/admin/configuracao`: um botão baixa o YAML,
uma caixa recebe o colado, e o plano aparece antes de qualquer escrita.

### O formato

```yaml
versao: 1
revisao: sha256:00d727780552ae4b9da23ccb2c4b54fc
upstreams:
  - nome: notion
    revisao: sha256:f3e85e266897d3a7ee85e68aebeba68a
    tipo: http
    url: https://mcp.notion.com/mcp
    timeout_ms: 15000
    habilitado: true
    sonda:
      habilitada: true
      ferramenta: notion-search
      args: '{"query":"ping"}'
      espera: resultado
      intervalo_ms: 900000
      timeout_ms: 10000
      tolerancia: 2
    segredos:
      - tipo: bearer
        valor: ${PATCHBAY_SEGREDO_NOTION_BEARER}
  - nome: arquivos
    revisao: sha256:30c5379f13d923c143ccf955fbcbd4da
    tipo: stdio
    comando: npx
    args:
      - -y
      - "@modelcontextprotocol/server-filesystem"
      - /dados
    env:
      NODE_ENV: production
    timeout_ms: 20000
    habilitado: true
    segredos:
      - tipo: env
        nome: TOKEN
        valor: ${PATCHBAY_SEGREDO_ARQUIVOS_ENV_TOKEN}
endpoints:
  - slug: pessoal
    revisao: sha256:8ebc0778aaf10f786a9dc90b7259edbf
    nome: Pessoal
    descricao: o endpoint de todo dia
    upstreams:
      - nome: arquivos
        prefixo: fs_
        regras:
          - acao: excluir
            padrao: write_*
          - acao: renomear
            padrao: read_*
            renome: ler_*
      - nome: notion
        prefixo: nt_
chaves_api:
  - nome: desenvolvimento
    prefixo_visivel: pbk_aaaabbbb
    endpoints:
      - pessoal
    revogada: false
```

Upstream é identificado pelo **nome** e endpoint pelo **slug** — não por id, que
é interno e não sobrevive a uma reinstalação. O `tipo` de um upstream existente
não se troca pelo import: mudar o transporte não é editar o upstream, é
substituí-lo, e o arquivo é recusado com essa mensagem.

`chaves_api` e `clientes_oauth` saem no arquivo como **registro** e o import não
os aplica: os dois guardam a credencial por hash, e emitir uma chave a partir do
YAML produziria um segredo que nenhum cliente tem. Eles aparecem no plano como
`informativo`.

### Segredo nunca sai no arquivo

Cada credencial aparece como um **slot** com o nome da variável de ambiente de
onde o import a lê. O valor não passa pelo arquivo em nenhuma direção, e um
valor literal em `valor:` é recusado com mensagem — aceitá-lo transformaria o
arquivo de configuração num cofre.

| No arquivo | No import |
|---|---|
| slot presente, variável definida no ambiente | grava o valor novo |
| slot presente, variável não definida | mantém o que está gravado, e o plano diz qual variável falta |
| slot ausente | mantém o que está gravado — **ausência nunca apaga** |
| `limpar: true` | apaga a credencial daquele slot |

O nome da variável é previsível — `PATCHBAY_SEGREDO_<UPSTREAM>_<TIPO>[_<NOME>]` —
para que quem importa noutra máquina saiba o que exportar sem abrir o arquivo
item por item. Trocar a referência por outra (`valor: ${MEU_TOKEN}`) funciona e é
respeitada.

O consentimento de OAuth de upstream é o único resíduo que o arquivo não
reconstrói: o refresh token não se recria a partir de configuração, e o upstream
volta pedindo autorização.

### A trava otimista, e por que ela mescla

Todo item do export carrega uma `revisao`, que é o resumo daquele item no
momento em que ele saiu. Quem edita o arquivo mexe nos campos e **não** na
`revisao` — e é essa assimetria que permite decidir item a item:

| Situação | O que o import faz |
|---|---|
| o item é idêntico dos dois lados | `sem-mudança` |
| só o arquivo mudou desde o export | `atualizar` |
| só o banco mudou desde o export | `sem-mudança`, e o banco fica |
| os dois mudaram | `conflito`: nada é aplicado **neste item**, e o plano mostra os campos lado a lado |
| o item não existe no banco | `criar` |
| o item existe no banco e não no arquivo | `ausente` — vira `remover` só com `--remover-ausentes` |

Conflito **não aborta o import**: os outros itens entram normalmente, e cada item
é uma transação própria. Um arquivo escrito à mão, sem nenhuma linha de
`revisao`, vale como intenção e é aplicado — a trava não é pedágio para quem
nunca exportou.

Remover um upstream que um endpoint ainda cita na composição do próprio arquivo
é `erro`, não `remover`: a composição só é reescrita quando o endpoint em si é
aplicado, e deixar o upstream sair mesmo assim apagaria o vínculo por baixo
(`ON DELETE CASCADE`) sem o plano ter avisado que aquele endpoint seria afetado.
Tire o vínculo do arquivo antes de remover o upstream.

```
$ patchbay import patchbay.yaml --dry-run
plano de import (schema 1)

o banco avançou desde o export deste arquivo
  revisão no arquivo: sha256:2426bb6f49af8a772b4c846e6580c714
  revisão no banco:   sha256:2abfc6e4771133212b678744aa71cd73
o import mescla: aplica o que só o arquivo mudou, mantém o que só o banco mudou
e reporta como conflito o que os dois mudaram.

  conflito    upstream       exemplo
              o arquivo e o banco mudaram desde o export; nada foi aplicado neste item
              timeout_ms: arquivo "60000" / banco "50000"
  criar       upstream       notion
  sem-mudança endpoint       pessoal
  informativo chave-api      desenvolvimento

4 item(ns): 1 criar, 2 sem-mudança, 1 conflito
```

`import` devolve código de saída diferente de zero quando o plano tem item em
`conflito` ou em `erro` — inclusive com `--dry-run` —, para uma esteira de CI
recusar o merge sem precisar interpretar o texto do plano.

O `export` e o `import` abrem o mesmo banco que o `serve` e passam pelo mesmo
portão de chave mestra. Rodá-los com o gateway no ar escreve no banco, mas o
processo em execução **não relê a configuração sozinho** — é o mesmo
comportamento do `seed`. Para aplicar no ar, use `/admin/configuracao`: o import
pela tela reconfigura a supervisão e rematerializa os endpoints na mesma
requisição.

`koanf` e `viper` ficaram de fora de propósito: eles resolvem precedência entre
arquivo, ambiente e flag, que é a camada que a proposta 08.9 do estudo elimina.
A serialização é `gopkg.in/yaml.v3`, que já estava no grafo de módulos do
projeto — adotá-lo não acrescentou uma linha ao `go.sum`.

## Chave mestra

O patchbay guarda três coisas que precisam voltar em claro: as credenciais
estáticas de upstream (bearer, headers, variáveis de ambiente sensíveis do
STDIO), o `client_secret` do cliente OAuth e os tokens de OAuth de upstream. As
três são cifradas em repouso com uma chave derivada da **chave mestra**.

```sh
patchbay chave-mestra gerar     # imprime 32 bytes em base64, uma única vez
export PATCHBAY_MASTER_KEY=...  # e é só daqui que o patchbay a lê
```

Ela vem **exclusivamente** de `PATCHBAY_MASTER_KEY`. Não há arquivo de chave em
disco, não há campo na UI e não há flag de linha de comando — argumento de
processo aparece em `ps` e no histórico do shell. Sem a variável, `patchbay
serve` e `patchbay seed` terminam no boot com a instrução de como gerar uma.

**Se a chave mudar, o patchbay não sobe.** No primeiro boot ele grava um
*canário* — um valor conhecido, cifrado — na tabela `settings`; em todo boot
seguinte ele decifra e compara. Se não confere, o processo termina dizendo
exatamente isso.

É indisponibilidade escolhida de propósito, no lugar de corrupção silenciosa. O
modo de falha ruim seria subir "funcionando" com a chave errada e transformar
cada upstream autenticado em falha de credencial, como se todos os provedores
tivessem revogado o acesso no mesmo minuto — e ninguém liga isso a um `compose`
editado três dias antes.

**Perder a chave é perder os segredos.** Não existe recuperação: sem ela o que
está cifrado no banco não volta, e o caminho é apagar o banco e recadastrar.
Guarde-a onde você guarda segredo de produção, e faça backup dela junto com o
`patchbay.db` — um sem o outro não serve para nada.

## Authorization server

O patchbay é o próprio authorization server dos seus clientes. Um cliente MCP
remoto — claude.ai, Claude Code, o que você escrever — conecta em
`/mcp/<slug>`, recebe 401 com `WWW-Authenticate` apontando para a metadata
daquele endpoint, e dela chega ao `/oauth/authorize`. **Nada disso precisa ser
configurado no cliente:** só a URL do endpoint.

O "usuário" deste AS é o administrador do patchbay. `/oauth/authorize` é a única
rota do protocolo atrás da sessão de admin: sem sessão ela leva ao login
carregando o destino, e o clique do consentimento não se perde.

| Rota | O que é |
|---|---|
| `/.well-known/oauth-authorization-server` | Metadata RFC 8414 |
| `/.well-known/oauth-protected-resource/mcp/<slug>` | Metadata RFC 9728 do endpoint, mais o fallback na raiz |
| `/oauth/authorize` | Consentimento, atrás da sessão de admin |
| `/oauth/token` | `authorization_code` e `refresh_token`, em `application/x-www-form-urlencoded` |
| `/oauth/revoke` | Revogação RFC 7009 |
| `/oauth/register` | Registro dinâmico RFC 7591 (DCR), aberto e com teto |

**O token vale para um endpoint só.** O `resource` do RFC 8707 é obrigatório na
autorização e vira o `aud` do token; apresentá-lo em outro endpoint devolve 403,
não 401 — 403 porque o token é válido, só não é para ali. O slug entra nessa URL
canônica, e é por isso que ele não muda depois de criado.

**Refresh com rotação e família.** Cada consentimento abre uma família de
tokens; cada refresh troca o par e encadeia o antigo ao novo. Reapresentar um
refresh já rotacionado — ou um código de autorização já usado — **revoga a
família inteira**, o que força reautenticação. É o comportamento desejado, e é
o que torna a rotação útil em vez de decorativa.

### As três formas de um cliente existir

| Forma | Como nasce | Como morre |
|---|---|---|
| **Cadastro à mão** | `/admin/oauth` → *Novo cliente*. A `redirect_uri` do claude.ai já vem sugerida | Revogado na tela |
| **CIMD** | O `client_id` **é** uma URL https, e o documento de metadados está publicado nela. Nada é registrado: o documento é buscado e cacheado com TTL de uma hora | O cache vence e é rebuscado; revogar na tela impede o rebusque |
| **DCR** (RFC 7591) | O cliente faz `POST /oauth/register` e guarda o `client_id` que recebe | Revogado na tela, e aí o `client_id` deixa de existir |

CIMD é o que a spec MCP 2026-07-28 pôs no lugar do DCR. O claude.ai só o escolhe
se a metadata anunciar `client_id_metadata_document_supported: true` **e**
`"none"` em `token_endpoint_auth_methods_supported` — o patchbay anuncia os dois.
Faltando um, ele cai para DCR e registra um cliente novo a cada conexão fresca.
DCR continua ligado porque a remoção mais cedo possível é a primeira revisão da
spec publicada em ou depois de 2027-07-28, e porque há cliente que só tem ele.

Cliente que se registra sozinho **não tem escopo escolhido por ninguém**, então
ele pode *pedir* qualquer endpoint. O que autoriza de fato continua sendo o
consentimento: uma vez por endpoint, na sua sessão de administração, com o slug
e o hostname do redirect na tela. A lista de `/admin/oauth` separa as três
origens numa coluna própria — uma linha que apareceu sem você pedir tem de ser
reconhecível como tal.

### Redirect de loopback, e por que a porta sai da comparação

Duas regras de comparação de `redirect_uri` convivem no mesmo endpoint, e é a
URI cadastrada que escolhe qual vale:

- **Exata, caractere a caractere**, para tudo. Comparação por prefixo é a falha
  clássica que transforma um AS em redirecionador aberto.
- **Ignorando a porta**, para `http` em loopback (RFC 8252 §7.3). O Claude Code
  é cliente nativo: ele escuta numa porta efêmera que só conhece depois de abrir
  o listener, então a porta não pode fazer parte do que foi cadastrado. Esquema,
  hostname, caminho e query continuam sendo comparados exatamente — e `localhost`
  e `127.0.0.1` são hostnames **diferentes**, então quem precisa dos dois declara
  os dois, como o Claude Code faz no próprio documento de CIMD.

### O guard de SSRF do CIMD

Buscar um documento de CIMD é a única vez em que o patchbay faz uma requisição de
saída para uma URL escolhida por quem chama — de dentro do processo que tem o
banco e a chave mestra em mãos. É a troca que a spec fez ao deprecar DCR: menos
inflação de clientes, um fetch controlado por terceiro. Os guard-rails:

- **Só `https`**, e só com caminho (nunca a raiz de um domínio); sem `userinfo`,
  sem fragmento.
- **Faixas privadas bloqueadas antes e depois do DNS.** Se o host já é um IP, a
  recusa acontece sem abrir socket; se é um nome, a checagem roda no
  `Control` do discador, com o endereço resolvido, imediatamente antes de
  conectar — não sobra janela para **DNS rebinding**. Loopback, RFC 1918,
  link-local (onde mora `169.254.169.254`), unique-local IPv6, CGNAT e as
  reservadas ficam de fora, e o IPv4 mapeado em IPv6 é desembrulhado antes da
  checagem.
- **Redirect não é seguido**: seguir um 302 é como a URL validada deixa de ser a
  URL buscada.
- **`Content-Type` de JSON exigido**, corpo limitado a 64 KiB, timeout de 5 s
  para o fetch inteiro, e **single-flight** por identificador.
- **O documento tem de fechar com a URL**: o `client_id` de dentro é comparado
  exatamente com a URL de fora. É o que impede publicar, num domínio seu, um
  documento que se apresenta como cliente de outro.
- **Erro nunca é cacheado**, e uma busca que falha serve o **último documento
  bom** que houver. Um cliente que já funcionava não perde a conexão porque a
  CDN do dono piscou.

Resíduo aceito, e está no estudo: provedor legítimo atrás de CDN cujo IP caia
numa faixa bloqueada falha o registro. O erro aparece, e o caminho de saída é
cadastrar o cliente à mão em `/admin/oauth`.

O `/oauth/register` é aberto por desenho — é o que "dynamic" quer dizer — e por
isso tem teto: um balde de fichas por IP e por minuto, mais um teto de registros
por origem e por hora e outro agregado. Um só dos dois não bastaria: o balde
deixa gravar para sempre em ritmo lento, e o teto sozinho deixa gastar a cota da
hora em um segundo. `client_secret` de DCR só é emitido se o cliente pedir
autenticação com segredo, aparece **uma única vez** na resposta do registro, e o
que fica no banco é o hash mais o prefixo visível.

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
- **Duas classes de segredo**: o que o patchbay *verifica* (chave de API, sessão
  de admin, e todo o que o authorization server emite — código de autorização,
  access token, refresh token, `client_secret` de cliente) vai como hash, e não
  volta nunca; o que ele *apresenta* (bearer e
  header estático de upstream, variável de ambiente sensível de upstream STDIO,
  e adiante os tokens de OAuth) precisa voltar em claro e vai em cifra
  reversível. O token de um servidor lançado por linha de comando é a mesma
  classe do bearer de um servidor HTTP: mesma tabela, mesma cifra. A coluna `env`
  em claro fica para o que não é segredo e o admin precisa poder reler. Guardar a segunda classe como hash não
  funciona; guardar a primeira de forma reversível cria um cofre de credencial
  alheia sem necessidade.
- **Cifra em repouso**: AES-256-GCM com chave derivada por HKDF-SHA256 da chave
  mestra. Nonce sorteado por valor — repetir nonce em GCM destrói os dois
  valores. O valor gravado é `pbc1:<base64url(nonce ‖ selado)>`: o prefixo de
  versão é o que permite trocar de algoritmo no futuro sem reescrever todas as
  linhas de uma vez.
- **AAD com a linha de origem**: o dado autenticado adicional é
  `(tabela, coluna, id)` — para uma credencial de upstream, o id é
  `<upstream_id>/<tipo>/<nome>`, onde o tipo é `bearer`, `header` ou `env`. Copiar o `valor_cifrado` do upstream A para a
  linha do upstream B falha a autenticação mesmo com a chave certa: quem tem
  escrita no banco e não tem a chave não consegue apontar a credencial de um
  upstream para outro.
- **Redação**: o valor em claro é o tipo `cripto.Segredo`, cujo `String`,
  `GoString` e `LogValue` devolvem `«redigido»`. `%v`, `%s`, `%#v` e o `slog`
  imprimem a marca mesmo quando alguém esquece; sair do tipo exige chamar
  `Revelar()`, que é grep-ável. Nenhuma mensagem de erro da cifra carrega o
  valor guardado.
- **Redação no log do processo**: além do tipo `cripto.Segredo`, o `slog.Handler`
  inteiro passa por `trilha.HandlerLog`, que apaga por chave sensível e por
  padrão de valor (bearer, JWT, marcas do patchbay) antes de a linha ser
  escrita. A tela de log ao vivo é a via mais fácil de vazar exatamente o que a
  cifra em repouso protege — ver [Observabilidade](#observabilidade).
- **Nunca em query string**: a credencial de upstream vai em header, injetada
  por um `http.RoundTripper` por upstream. Query string vaza em log de proxy,
  em histórico e em `Referer` — e é o vazamento que a cifra em repouso não teria
  como desfazer.

## Sonda de saúde funcional

Um servidor MCP pode conversar perfeitamente, responder `tools/list` com quinze
ferramentas e **não funcionar**: o token expirou por dentro, a cota da API
acabou, o backend caiu atrás dele. Do lado do gateway isso é indistinguível de
saúde — a conexão está de pé, a lista chegou —, e o cliente só descobre quando
já gastou contexto chamando a ferramenta.

A sonda do patchbay executa um **`tools/call` de verdade**, de tempos em tempos,
com a ferramenta e os argumentos que você escolheu. Nenhum gateway do
levantamento faz isso.

**Ela vem desligada, e é opt-in por servidor.** Escolher a ferramenta é a parte
que não dá para automatizar: o patchbay não tem como saber que `send_message`
manda mensagem para alguém, e adivinhar seria a forma de descobrir isso do pior
jeito. Uma busca com um termo bobo é inócua; nada garante isso para uma
ferramenta arbitrária.

Configuração, na tela do upstream:

| Campo | O que faz |
|---|---|
| **Sondar este upstream** | O opt-in. Desligado por padrão. |
| **Ferramenta a chamar** | O nome como o upstream o expõe, sem prefixo de endpoint. |
| **Argumentos (JSON)** | O objeto de argumentos do `tools/call`. Em branco chama sem argumentos. |
| **Trecho esperado** | Opcional. Cobre o servidor que responde 200 com um erro amigável no corpo, sem marcar `isError`. |
| **Intervalo** | A sonda consome cota da API do provedor: quem escolhe é quem paga a cota. Padrão de 15 min. |
| **Timeout da sondagem** | Cancela **só a chamada**, nunca a sessão do upstream. |
| **Falhas seguidas para derrubar** | Padrão 2. Uma falha isolada é o soluço que o backoff já cobre. |

**O que acontece quando ela falha.** Erro de transporte, prazo estourado,
`isError` ou trecho esperado ausente contam como falha. Ao chegar na tolerância
configurada, o upstream vai a `sonda_falhou` e as ferramentas dele **saem do
catálogo de todos os endpoints** — com lápide, como qualquer outra remoção: o
cliente que ainda não relistou recebe um erro de ferramenta explicando que ela
saiu, em vez de `unknown tool`. A sessão continua de pé, e é por ela que a
sondagem seguinte descobre que o servidor voltou, sem gastar uma reconexão. A
primeira sondagem que passa devolve tudo e o upstream volta a `pronto`.

**O que ela não é.** Sondagem que falha não é connect abandonado nem falha de
conexão: ela não mexe no backoff, não conta para o teto de abandonos e não
desabilita nada por autoproteção. E ela nunca roda no caminho da requisição do
cliente — a chamada sai da goroutine de supervisão daquele upstream, sobre a
sessão que já está aberta. O botão **Sondar agora** na tela não muda isso: ele
entrega o pedido à supervisão e espera o desfecho, para continuar existindo um
único escritor do estado da sonda.

**Nada do resultado vai ao banco.** As colunas `sonda_*` guardam só a
configuração; quando a sondagem rodou, se passou, o erro, a requisição e a
resposta exatas vivem em memória, como `degradado` e `sem_consentimento`. Todo
boot recomeça em `novo`.

**A tela mostra a requisição e a resposta exatas.** Sem as duas, não dá para
separar "minha sonda está mal configurada" de "o servidor está quebrado" — e a
saída mais fácil passa a ser desligar a sonda em vez de consertar o servidor.
Pelo mesmo motivo, **`sonda desligada` é um estado próprio e nunca verde**: um
verde que significa "não sei" é exatamente a mentira que a sonda existe para
acabar.

**O resíduo, nomeado:** uma sonda mal configurada apaga as ferramentas de um
servidor saudável. A defesa é ela ser opt-in, o estado desligado ser explícito e
o erro carregar a evidência. O outro lado do mesmo resíduo é que a maioria dos
upstreams vai ficar sem sonda, porque configurar dá trabalho — o valor do
diferencial é proporcional à disciplina de configurar, e a tela do upstream diz
isso em vez de fingir que está tudo bem.

Cada sondagem deixa uma linha na trilha, marcada com **origem `sonda`**. Os
contadores do painel contam só chamada de cliente: a sonda é o custo da
observação, não tráfego, e somá-la inflaria a taxa por minuto de um gateway
parado e o percentual de erro de chamadas que nunca existiram.

## Observabilidade

Uma linha por `tools/call`, e nenhuma delas no caminho da latência.

O SQLite aceita **um escritor por vez** e a trilha é a escrita mais frequente do
sistema. Se ela entrasse na transação da chamada, cada `tools/call` passaria a
esperar pela fila de escrita e o gateway serializaria por causa do log. O
desenho, então, é:

```
tools/call ──► upstream ──► resposta ao cliente
                  │
                  └─► Observar(): envio não bloqueante numa fila de 1024
                          │  (fila cheia → descarta e conta; nunca espera)
                          ▼
                   consumidor único ──┬─► lote de até 128 → uma transação
                                      └─► hub SSE → cada tela aberta
```

O gancho de captura é uma interface de um método
(`endpoint.Observador`), declarada no pacote que a consome e ligada em `main` —
`internal/endpoint` não conhece `internal/trilha`. Ele roda no despacho da
chamada, depois de o resultado estar pronto.

**O descarte é resíduo assumido, e ele aparece na tela.** Sob rajada a fila
enche e a linha se perde; o contador de descartes está sempre visível em
`/admin/trilha`, com aviso quando é maior que zero. Trilha que mente é pior que
trilha faltando. Falha de gravação — o banco recusou um lote que a fila já
tinha aceitado — é contada à parte, em `Registrador.FalhasGravacao`: são
diagnósticos diferentes ("a fila não escoa" contra "o banco está recusando"),
e a tela mostra os dois.

No desligamento, o consumidor só para depois de o servidor HTTP confirmar que
não há requisição em curso (`Aplicacao.PararConsumoDaTrilha`, chamada depois
de `srv.Shutdown` retornar) — parar no cancelamento do `ctx` do serviço, que
chega antes, perderia sem contar como descarte a chamada que termina durante
essa janela.

A tela pagina por cursor `(ts, id)` da última linha vista, não por `OFFSET`:
numa tabela que só cresce, `OFFSET` fica mais caro a cada página, e a
comparação de tupla usa o mesmo índice sem escanear as páginas já vistas. Por
isso só existe o link "mais antigas" — sem numeração nem "voltar".

**A trilha guarda tamanho, nunca conteúdo.** Argumento e resultado de ferramenta
são dado de terceiro e o caminho mais curto para um segredo entrar no banco em
claro. O que se diagnostica com eles é "grande demais", e para isso o número
basta. O que a linha carrega é: instante, endpoint, upstream, nome exposto e
nome original da ferramenta, desfecho (`ok`/`erro`/`timeout`), duração, bytes de
entrada e de saída, id de sessão **anonimizado** (SHA-256 truncado), a
credencial na forma `apikey:<id>`/`oauth:<client_id>` e a era do protocolo MCP
negociada naquela sessão.

**Retenção de 7 dias**, varrida de hora em hora em lotes de 500 linhas. Em lotes
pequenos porque a varredura não pode segurar o escritor único: um `DELETE` sem
limite seria uma transação de tamanho imprevisível no primeiro boot depois de
meses parado.

### Log ao vivo

`/admin/logs/ao-vivo` é `templ` + `htmx` + a extensão oficial `htmx-ext-sse`
sobre um `http.Flusher` comum, servido em `/admin/logs/ao-vivo/fluxo`. O stream
entrega **fragmento de HTML**, não JSON: montar a linha aqui, com `templ`, é o
que garante o escape de tudo que veio de terceiro sem um render em JavaScript
escrito à mão.

Cada tela aberta tem a própria fila de 128 mensagens. Quando ela enche — aba de
fundo, conexão congelada por um proxy — a linha é descartada **só para aquela
tela**, e nunca vira espera para o processo. Esse descarte também aparece na
tela.

O único JavaScript escrito à mão da UI é
`internal/platform/webui/estatico/js/log-ao-vivo.js`, com um teto de linhas no
DOM: a extensão de SSE não tem modificador de "no máximo N filhos", e uma aba
deixada aberta a noite inteira acumularia dezenas de milhares de nós.

### Redação de segredo

O `slog.Handler` do processo é embrulhado por `trilha.HandlerLog`, que redige
**antes de delegar ao handler de baixo** — a redação vale para o stderr, para o
arquivo e para a tela pelo mesmo caminho. Redigir só na tela deixaria o
vazamento no destino que ninguém revisa.

Duas regras, e a segunda é a que pega o vazamento que mais acontece:

- **Por chave**: o valor de um atributo cuja chave contenha `authorization`,
  `token`, `secret`, `senha`, `password`, `chave`, `cookie`, `credencial`,
  `bearer`, `verifier` ou `pkce` sai como `«redigido»`. Vale dentro de
  `slog.Group`, inclusive quando a chave sensível é a **de fora** do grupo.
  `key` solto não entra na lista: ele casaria com `api_key_id`, que é um número
  de linha e é o que permite achar a chave na tela.
- **Por valor**, independente da chave: `Bearer …`/`Basic …` (o esquema fica, o
  resto some), JWT (`eyJ….….…`), todo segredo emitido pelo próprio patchbay
  (`pbk_`, `pbat_`, `pbrt_`, `pbac_`, `pbcs_`) e a marca de credencial de
  provedor conhecido (`ghp_`/`gho_`/`github_pat_` do GitHub, `sk-ant-` da
  Anthropic, `sk-` de vinte ou mais caracteres da OpenAI, `xoxb-`/`xoxp-` do
  Slack, `glpat-` do GitLab, `ya29.` do Google, `AKIA…` da AWS). Nas marcas do
  próprio patchbay a redação **preserva o prefixo visível** —
  `pbk_a1b2c3d4_«redigido»` — que é o mesmo prefixo que a UI mostra: o admin
  sabe de qual credencial o log falava sem que o log a entregue. Um parâmetro
  de query com nome sensível (`api_key`, `code`, `state`, `sig`...) dentro de
  uma URL também sai redigido — é o formato em que um `*url.Error` do
  `net/url` embute a URL inteira na mensagem.

A mesma redação roda sobre a mensagem de erro antes de ela virar linha da
trilha: a resposta de um upstream pode repetir o header que ele recusou. O
limite conhecido: um header estático de upstream é texto livre, e nada garante
que ele siga um dos formatos acima — nesse caso só a redação por chave
continua valendo.

## Verificar

```sh
task verifica   # go vet + golangci-lint + go test -race
```

`-race` exige cgo; no Windows sem compilador C, rode os testes por WSL ou em
Linux.

Os testes de upstream STDIO sobem **processos de verdade**: o binário de teste se
reexecuta como servidor MCP, como processo mudo e como neto, e prova que o neto
morre junto com a árvore. O caso de controle
(`TestArvore_MatarSoOFilhoDeixaONetoVivo`, em `internal/platform/stdioproc`)
mata só o filho direto e verifica que o neto **sobrevive** — sem ele, o teste
principal passaria mesmo que o neto estivesse morrendo por outro motivo.

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
