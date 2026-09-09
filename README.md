# patchbay

Gateway MCP self-hosted: um endpoint agrega vários servidores MCP upstream e os
serve a um cliente de IA como se fossem um só.

Binário único, sem dependência de stack externa. Estado em SQLite embutido.

> Estado: fatias **1, 2, 3, 4, 5, 6, 10 e 13** do épico entregues — catálogo e
> endpoint, resiliência de upstream, composição fina do endpoint, upstream
> STDIO com supervisor de processo, segredos cifrados em repouso, o
> authorization server essencial e o export/import da configuração em YAML.
> OAuth de upstream (fatias 7-8), sonda funcional (fatia 9) e CIMD/DCR/redirect
> URI de loopback (fatia 11) seguem pendentes.
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
| `/admin/configuracao` | Baixa o YAML da configuração e importa um colado, mostrando o plano item a item antes de aplicar |

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
| `PATCHBAY_MASTER_KEY` | Chave mestra de cifra, 32 bytes em base64. `serve`, `seed`, `export` e `import` não sobem sem ela |

Ela não tem flag de propósito: argumento de processo aparece em `ps` e no
histórico do shell. E não tem arquivo de chave em disco nem entrada pela UI —
a origem é essa variável e só ela.

A URL pública é o que a UI mostra aos clientes e o que decide o atributo
`Secure` do cookie de sessão: o TLS é terminado por um proxy reverso na frente
do patchbay, então o processo não descobre o esquema externo olhando a
requisição.

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
  de admin) vai como hash, e não volta nunca; o que ele *apresenta* (bearer e
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
