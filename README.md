# patchbay

Gateway MCP self-hosted: um endpoint agrega vários servidores MCP upstream e os
serve a um cliente de IA como se fossem um só.

Binário único, sem dependência de stack externa. Estado em SQLite embutido.

> Estado: fatias **1, 2, 3, 4, 5, 6, 10 e 11** do épico entregues — catálogo e
> endpoint, resiliência de upstream, composição fina do endpoint, upstream
> STDIO com supervisor de processo, segredos cifrados em repouso e o
> authorization server completo (CIMD, DCR e redirect URI de loopback
> incluídos). OAuth de upstream (fatias 7-8), sonda funcional (fatia 9) e
> observabilidade (fatia 12) seguem pendentes.
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
  `novo → conectando`. **Dois transportes na mesma máquina de estados:**
  Streamable HTTP e STDIO — um processo por servidor, compartilhado por todas as
  sessões de cliente, com a árvore inteira morrendo junto e restart pelo mesmo
  backoff.
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
| `/admin/upstreams` | CRUD de upstream HTTP (bearer e headers estáticos) e STDIO (comando, argumentos e ambiente); detalhe com estado, último erro, próxima tentativa, falhas consecutivas, connects abandonados e as ferramentas descobertas (nome exposto, nome original, descrição); botão **Reconectar** que descarta a sessão e rearma a supervisão na hora |
| `/admin/endpoints` | CRUD de endpoint com composição fina — quais upstreams entram, com que prefixo e com que regras de filtro/renomeação — e a contagem de ferramentas do endpoint e de cada upstream dentro dele |
| `/admin/chaves` | Emissão de chave com escopo, comando `claude mcp add` pronto, revogação |
| `/admin/oauth` | Clientes do authorization server: cadastro à mão, e as linhas que aparecem sozinhas por **CIMD** ou **DCR** — a coluna Origem diz qual é qual. Detalhe com a allowlist de redirect, o escopo, as sessões vivas e a revogação de cliente ou de sessão |

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
| `PATCHBAY_MASTER_KEY` | Chave mestra de cifra, 32 bytes em base64. `serve` e `seed` não sobem sem ela |

Ela não tem flag de propósito: argumento de processo aparece em `ps` e no
histórico do shell. E não tem arquivo de chave em disco nem entrada pela UI —
a origem é essa variável e só ela.

A URL pública é o que a UI mostra aos clientes e o que decide o atributo
`Secure` do cookie de sessão: o TLS é terminado por um proxy reverso na frente
do patchbay, então o processo não descobre o esquema externo olhando a
requisição.

## Chave mestra

O patchbay guarda duas coisas que precisam voltar em claro: as credenciais
estáticas de upstream (bearer, headers) e, adiante, os tokens de OAuth. As duas
são cifradas em repouso com uma chave derivada da **chave mestra**.

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
- **Nunca em query string**: a credencial de upstream vai em header, injetada
  por um `http.RoundTripper` por upstream. Query string vaza em log de proxy,
  em histórico e em `Referer` — e é o vazamento que a cifra em repouso não teria
  como desfazer.

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
