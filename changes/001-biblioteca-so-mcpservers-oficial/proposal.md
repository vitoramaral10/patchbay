---
id: 001-biblioteca-so-mcpservers-oficial
titulo: biblioteca de MCPs passa a listar somente os servidores oficiais do mcpservers.org
status: verificada
capacidades: [biblioteca]
criada: 2026-09-11
aprovada_por: Vitor Melo, 2026-09-11, "pode ir"
---

<!-- status: rascunho → esclarecida → aprovada → em-implementacao → verificada → arquivada.
     Só o usuário promove para `aprovada`. O modelo propõe e registra `aprovada_por`. -->

# Proposta: biblioteca só com os oficiais do mcpservers.org

## Por que

Hoje a biblioteca mescla três origens (`estudo.md`, "Em uma tela"): o registry
`modelcontextprotocol.io` com ~29,6 mil servidores aceitos sem curadoria, a lista de remotos
curados do mcpservers.org e a lista `/official` do mesmo site. O pedido é ficar só com a
última: uma lista de 652 servidores (título da página em 2026-09-11) que alguém já chamou de
oficiais, em vez de dezenas de milhares onde o usuário precisa garimpar.

O custo de não fazer nada é o que a própria tela descreve hoje: "dezenas de milhares que o
registry aceita sem olhar" (`estudo.md`, texto de `admin.templ:122`) atrás de um filtro. A
troca também tira uma dependência inteira (a API do registry) e deixa uma só, mas essa uma é
HTML raspado de um site de terceiro, o que muda o perfil de risco (seção Riscos).

A motivação, confirmada em Esclarecimentos, é quádrupla, e cada parte tem critério próprio:
ruído na busca (CA-10: o catálogo tem o tamanho da lista alvo), qualidade dos cadastros
(CA-05, CA-06 e CA-10: descrição, site e comando ou URL preenchidos quando a origem os dá),
varredura lenta (CA-10: duração da varredura real registrada, dentro de 1 h; a comparação com a antiga foi dispensada em Esclarecimentos) e
política de listar só o oficial (CA-01 e CA-02: nenhuma requisição e nenhuma referência ao
registry).

## O que muda (delta de requisitos)

Não há `specs/biblioteca/spec.md` ainda: o comportamento atual está descrito no `estudo.md`,
e o que sai está em REMOVED sem id.

### ADDED Requisitos

#### RQ-01 Origem única: a lista oficial do mcpservers.org

Como administrador do patchbay, quero que a biblioteca liste exatamente os servidores
publicados em `https://mcpservers.org/pt-BR/official` para escolher entre servidores
oficiais sem garimpar um registry aberto.
Prioridade: P1.

```gherkin
Cenário: varredura completa
  Dado o índice oficial com 22 páginas de cerca de 30 servidores
  Quando a sincronização roda
  Então o catálogo passa a ter um item por servidor listado nas 22 páginas cujo detalhe respondeu (detalhe indisponível segue a tolerância de RQ-02 e CA-12)
  E nenhuma requisição é feita a registry.modelcontextprotocol.io
  E nenhuma requisição é feita a mcpservers.org/pt-BR/remote-mcp-servers

Cenário: página além do fim
  Dado que o índice oficial responde 404 para a página 23
  Quando a varredura chega ao fim dos links de paginação
  Então ela para na última página anunciada pelo índice e conclui com sucesso

Cenário: site fora do ar ou bloqueando (caminho triste)
  Dado que mcpservers.org responde 403, 429 ou 5xx
  Quando a sincronização roda
  Então o catálogo anterior continua sendo servido sem alteração
  E a tela do admin mostra a falha com a hora da tentativa

Cenário: marcação do site mudou (caminho triste)
  Dado que o índice oficial responde 200 mas sem nenhum link de servidor reconhecível
  Quando a sincronização roda
  Então a varredura registra falha de formato e não substitui o catálogo por vazio
```

ENQUANTO a varredura roda, O SISTEMA DEVE respeitar a pausa entre páginas que a origem exige
(hoje 2 s, medido em `curadoria.go:39-53`) e concluir dentro do prazo já existente de 1 h por
varredura (`PrazoDaVarredura`).

#### RQ-02 O que uma entrada carrega

Como administrador, quero que cada item do catálogo traga nome, descrição em pt-BR, site e,
quando a página de detalhe expõe um comando de instalação limpo, o comando e os argumentos,
para que "Adicionar" abra o formulário já preenchido.
Prioridade: P1.

```gherkin
Cenário: detalhe com comando aproveitável
  Dado a página de detalhe de um servidor oficial com um snippet "command"/"args" sem caminho de exemplo
  Quando a varredura lê o detalhe
  Então o item entra com transporte stdio, comando e argumentos preenchidos

Cenário: detalhe sem comando aproveitável (caminho triste)
  Dado a página de detalhe de um servidor oficial sem snippet de comando, ou com caminho de exemplo (C:\PATH\TO\...)
  Quando a varredura lê o detalhe
  Então o item entra no catálogo sem comando (com nome, descrição e site gravados), e "Adicionar" abre o formulário com o nome preenchido e o campo de comando vazio

Cenário: detalhe que não pôde ser lido (caminho triste)
  Dado que uma página de detalhe responde 5xx, 404 ou 200 sem título enquanto o índice respondeu 200
  Quando a varredura lê o detalhe
  Então o item é contado como indisponível, a varredura segue e conclui com sucesso sem esse item

Cenário: detalhe sem descrição (caminho triste)
  Dado a página de detalhe de um servidor oficial com título mas sem descrição
  Quando a varredura lê o detalhe
  Então o item entra com descrição vazia, sem erro, e conta na medição de itens sem descrição de CA-10

Cenário: detalhes indisponíveis demais (caminho triste)
  Dado que mais de 10% das páginas de detalhe listadas no índice não puderam ser lidas (5xx, 404 ou sem título)
  Quando a varredura termina de percorrer o índice
  Então ela registra falha com a contagem de indisponíveis e o catálogo anterior é preservado
```

#### RQ-03 Servidores remotos dentro da lista oficial

Como administrador, quero que um servidor oficial que é remoto (HTTP ou SSE) seja cadastrado
como remoto, com a URL de conexão, para não receber um item stdio sem comando quando o
servidor nem roda localmente.
Prioridade: P2.

```gherkin
Cenário: entrada oficial que declara ser remota
  Dado a página de detalhe de um servidor oficial cuja descrição ou bloco de conexão expõe uma URL HTTP de MCP
  Quando a varredura lê o detalhe
  Então o item entra com transporte http e a URL de conexão preenchida, sem comando

Cenário: URL que não é de MCP (caminho triste)
  Dado uma página de detalhe cuja única URL é a do site do fornecedor
  Quando a varredura lê o detalhe
  Então o item não é cadastrado como remoto

Cenário: endpoint declarado removido (caminho triste)
  Dado uma página de detalhe cujo único endpoint de MCP aparece numa frase que o declara removido ou legado
  Quando a varredura lê o detalhe
  Então o item não é cadastrado como remoto e entra sem URL de conexão

Cenário: tabela de endpoints (emenda de 2026-09-11, caso Cloudflare)
  Dado uma página de detalhe cujo README traz uma tabela com uma coluna de cabeçalho "URL" ou "endpoint" e, nessa coluna, uma ou mais URLs https terminadas em /mcp ou /sse
  Quando a varredura lê o detalhe
  Então o item entra como remoto, com a URL da primeira linha marcada "recomendado" (ou, sem marcação, a primeira linha) e com todos os endpoints da tabela gravados, na ordem da página — um ou mais, e a URL do item é sempre um deles

Cenário: tabela cujas URLs são de repositório (caminho triste)
  Dado uma tabela cuja coluna de nome traz links de repositório terminados em /mcp (github.com/fornecedor/mcp) e cuja coluna de URL só traz links de github.com, gitlab.com ou do site do fornecedor
  Quando a varredura lê o detalhe
  Então nenhum desses links vira endpoint e o item não é cadastrado como remoto
```

#### RQ-04 A tela reflete a origem única

Como administrador, quero que a tela da biblioteca diga de onde vêm os servidores e quanto
tempo a primeira varredura leva, para não esperar por um registry que não é mais consultado.
Prioridade: P2.

```gherkin
Cenário: textos da tela
  Dado a tela da biblioteca aberta
  Quando o administrador a lê
  Então o subtítulo e o link de rodapé apontam para mcpservers.org/pt-BR/official
  E nenhum texto da tela cita "registry" nem "modelcontextprotocol.io"

Cenário: primeira varredura
  Dado uma instalação cujo catálogo ainda está vazio
  Quando o administrador abre a tela durante a primeira varredura
  Então o aviso fala do número de páginas e do tempo da lista oficial, não de "trezentas páginas"

Cenário: filtro de curados (caminho triste)
  Dado que todos os itens agora vêm da mesma lista curada
  Quando o administrador procura o filtro "só curados"
  Então não existe filtro "só curados" nem "domínio verificado" na tela, e a listagem e a busca por termo continuam funcionando sem eles

Cenário: item com vários endpoints (emenda de 2026-09-11, caso Cloudflare)
  Dado um item remoto cuja página publicou mais de um endpoint
  Quando o administrador vê o cartão
  Então o cartão lista cada endpoint com um "Adicionar" próprio, que abre o formulário de MCP com aquela URL

Cenário: item sem comando nem URL (emenda de 2026-09-11)
  Dado um item que entrou sem comando e sem URL
  Quando o administrador vê o cartão
  Então o cartão diz que a página do servidor não publica comando nem endpoint reconhecível, traz o link para a página no mcpservers.org e para o site, e "Adicionar" abre o formulário com o nome preenchido para completar à mão
```

#### RQ-05 Semente embutida só com a lista oficial

Como quem instala o patchbay pela primeira vez, quero ver a lista oficial já no primeiro
boot, antes da primeira varredura, para não abrir uma biblioteca vazia.
Prioridade: P2.

```gherkin
Cenário: primeiro boot
  Dado uma instalação nova sem rede
  Quando o patchbay sobe
  Então a biblioteca lista os servidores da semente embutida, todos com nome "mcpservers.org/<slug>"
  E nenhum item da semente tem nome no formato do registry (io.github.*, com.*, etc.)

Cenário: semente vencida não segura a varredura (caminho triste)
  Dado uma instalação que subiu com a semente
  Quando o intervalo de sincronização vence
  Então a varredura roda e substitui a semente pelo que a lista oficial tem hoje
```

#### RQ-06 Instalação que já tem o catálogo antigo

Como administrador de uma instalação que já rodou com o registry, quero que a troca de origem
não deixe um catálogo misto, para não ver itens do registry ao lado dos oficiais.
Prioridade: P1.

```gherkin
Cenário: primeira varredura depois da atualização
  Dado uma instalação com catálogo cheio de itens com nome no formato do registry
  Quando a primeira varredura com a nova origem conclui
  Então o catálogo contém só itens "mcpservers.org/<slug>"

Cenário: varredura falha depois da atualização (caminho triste)
  Dado a mesma instalação e a nova origem fora do ar
  Quando a primeira varredura falha
  Então a tela mostra a semente nova, sem nenhum item do registry, e a falha da varredura com a hora da tentativa

Cenário: primeira subida do binário atualizado
  Dado uma instalação com catálogo cheio de itens com nome no formato do registry
  Quando o binário com a nova origem sobe pela primeira vez
  Então o catálogo antigo é descartado e a semente nova é carregada antes de qualquer varredura

Cenário: conexões já criadas
  Dado uma conexão MCP criada a partir de um item do registry antes da atualização
  Quando o catálogo é substituído
  Então a conexão continua existindo e funcionando sem alteração
```

### MODIFIED Requisitos

Nenhum: não há spec viva anterior.

### REMOVED Requisitos

Sem id (não há spec viva). O que sai, com quem dependia:

- **Varredura do registry `modelcontextprotocol.io`** (`/v0/servers`, paginado por cursor). Dependiam: o campo `versao` do item, a semântica de "domínio verificado" e o namespace DNS invertido (`estudo.md`, "Semântica de nome"). Passam a não existir ou a ser tratados em RQ-04.
- **Varredura da lista de remotos curados** (`mcpservers.org/pt-BR/remote-mcp-servers`). Dependiam: os campos de autenticação (`OAuth`, `token`, `aberta`) e `pede_credencial`, que só essa lista fornece. Confirmado (Esclarecimentos): a lista de remotos sai junto; a única origem passa a ser `/official`, e autenticação só volta a existir para o que RQ-03 conseguir ler do detalhe.

## Critérios de aceite

- CA-01 (RQ-01) Dado o sincronizador apontado para um servidor de teste que serve as páginas de índice e detalhe gravadas, quando a varredura roda, então o repositório recebe um item por link de servidor do índice e o servidor de teste não registra nenhuma requisição a caminhos de `/v0/servers` nem `/remote-mcp-servers`. Verificável por teste Go.
- CA-02 (RQ-01) Dado o código compilado, quando se procura `registry.modelcontextprotocol.io` e `remote-mcp-servers` nos arquivos `.go` e `.templ` de `internal/` e `cmd/`, então não há nenhuma ocorrência, nem em comentário. Ficam fora da busca só os `.sql` das migrações 00013/00014 (registro histórico) e `testdata/`, HTML capturado do site de terceiro cuja navegação cita as duas strings. Verificável por `grep --include=*.go --include=*.templ`.
- CA-03 (RQ-01) Dado o servidor de teste respondendo 403 ou 5xx no índice, quando a varredura roda, então o catálogo anterior fica intacto e `biblioteca_sincronizacao` registra `erro` e `tentada_em`. Verificável por teste Go.
- CA-04 (RQ-01) Dado o índice respondendo 200 sem nenhum link `/servers/<slug>`, quando a varredura roda, então ela falha com erro de formato e o catálogo não é substituído por vazio. Verificável por teste Go.
- CA-05 (RQ-02) Dado as fixtures `mcpservers-oficial-comando.html` e `-sem-comando.html`, quando o detalhe é lido, então a primeira produz item stdio com comando e args e a segunda produz item sem comando, sem erro, com nome, descrição e site preenchidos. Verificável por teste Go.
- CA-06 (RQ-03) Dado uma fixture de detalhe com URL HTTP de MCP, quando o detalhe é lido, então o item sai com transporte http, a URL preenchida e comando vazio; dado uma fixture cuja única URL é o site do fornecedor, o item sai sem URL de conexão; dado a fixture do Apify, cujo único endpoint de MCP é declarado removido na mesma frase, o item sai stdio sem URL de conexão. Verificável por teste Go.
- CA-07 (RQ-04) Dado a tela renderizada, quando se inspeciona o HTML, então não contém "registry" nem "modelcontextprotocol.io", não contém os controles de filtro "só curados" e "domínio verificado", e o link de rodapé é `https://mcpservers.org/pt-BR/official`. Verificável por teste de handler.
- CA-08 (RQ-05) Dado a semente embutida, quando ela é decodificada, então todo `Nome` começa com `mcpservers.org/` e o total é maior que zero. Verificável por teste Go.
- CA-09 (RQ-06) Dado um banco pré-carregado com itens no formato do registry, quando o binário atualizado sobe, então o catálogo passa a ser a semente nova antes de qualquer varredura e nenhum item com o formato do registry sobra; as conexões já cadastradas continuam íntegras. Verificável por teste Go.
- CA-10 (RQ-01, RQ-06) Dado um binário construído da branch e rede disponível, quando a varredura real roda contra `mcpservers.org/pt-BR/official`, então ela conclui dentro de 1 h, a duração fica registrada no log, o total de itens fica dentro de ±2% do anunciado no título da página naquele dia (652 em 2026-09-11), nenhum item sai sem descrição e no máximo 1% saem sem site. Verificável por execução com log, evidência em `verificacao.md`.
- CA-11 (RQ-04) Dado o `README.md` inteiro, quando se procura `registry` e `modelcontextprotocol.io`, então não há ocorrência: o arquivo descreve uma origem só. Verificável por `grep`.
- CA-13 (RQ-03) Dado a fixture do Cloudflare (`mcpservers-oficial-tabela-endpoints.html`, página real de 2026-09-11 com tabela de 17 endpoints), quando o detalhe é lido, então o item sai com transporte http, URL `https://mcp.cloudflare.com/mcp` (linha "recomendado") e 17 endpoints gravados na ordem da página (a URL é o primeiro deles); dado uma tabela sintética cuja coluna de nome traz `github.com/x/mcp` e cuja coluna de URL só traz links de repositório, o item sai sem URL e sem endpoints. Verificável por teste Go.
- CA-14 (RQ-04) Dado um item com dois endpoints gravados e um item sem comando nem URL, quando a tela é renderizada, então o primeiro cartão traz um link "Adicionar" por endpoint, cada um abrindo o formulário com `url=` daquele endpoint, e o segundo cartão traz o texto de que a página não publica comando nem endpoint reconhecível, o link para `https://mcpservers.org/pt-BR/servers/<slug>` e para o site. Verificável por teste de handler.
- CA-12 (RQ-02) Dado um servidor de teste em que 11% dos detalhes do índice não podem ser lidos (respondem 500; 404 e página sem título contam do mesmo jeito), quando a varredura roda, então ela registra falha e o catálogo anterior fica intacto; dado o mesmo servidor com 5% em 500, a varredura conclui com sucesso e grava os demais itens. Verificável por teste Go.

## Fora de escopo

- A lista `/all` do mcpservers.org (12 mil servidores, ~7 h por varredura, `curadoria.go:349-350`) — mesma qualidade de dado, custo proibitivo.
- Outros idiomas do site (`/official` sem prefixo, `/en/`, etc.) — o pedido é o `/pt-BR/`.
- Editor de curadoria local (aprovar, ocultar, anotar itens) — não existe hoje e o pedido não pede.
- Mudar intervalo (12 h) ou prazo (1 h) da sincronização — ficam como estão.
- Buscar o comando de instalação no README do repositório GitHub de cada servidor — seria uma quarta origem; fica para outra mudança se RQ-02 mostrar que ~190 itens com comando são poucos.

## Esclarecimentos

<!-- Preenchido pela skill `spec-esclarecer`. -->

- P: A lista de remotos curados do mcpservers.org (/remote-mcp-servers, 293 servidores, única fonte de OAuth/token hoje) sai junto com o registry? → R: Sai também: só /official
- P: Entrada do /official cuja página de detalhe expõe uma URL HTTP de MCP (ex.: 'Servidor MCP remoto, HTTP streamable'): como entra? → R: Entra como remoto com a URL
- P: Entrada do /official sem comando de instalação aproveitável (hoje cerca de 10 em 14): entra no catálogo? → R: Entra sem comando
- P: Instalação que já tem catálogo do registry e cuja primeira varredura com a nova origem falha: o que a tela mostra? → R: Limpa o antigo e usa a semente nova
- P: Com todos os itens vindo da mesma lista curada, o que acontece com os filtros 'só curados' e 'domínio verificado' da tela? → R: Os dois saem da tela
- P: Qual é o problema concreto com o registry que motiva a troca? Define o que a verificação precisa provar. → R: todas (ruído, qualidade dos cadastros, varredura lenta e política de listar só o oficial)
- P: (análise, CRITICO 1) Uma página de detalhe fora do ar (5xx) durante a varredura de 652 detalhes deve derrubar a varredura inteira? → R: Tolerar e reescrever o cenário: detalhe indisponível é contado e a varredura conclui; acima de 10% de detalhes indisponíveis a varredura falha e preserva o catálogo
- P: (análise, CRITICO 2 e três ALTO) Corrigir agora o grep de CA-02 excluindo testdata e os arquivos faltantes em T-01, T-03 e T-10, e reanalisar? → R: Corrigir agora e reanalisar
- P: (análise, MEDIO) Que tolerância adotar em CA-10 na varredura real? → R: ±2% no total, 0 sem descrição, ≤1% sem site
- P: (ESCALAR de T-06) D-02 cadastra como remoto `https://mcp.apify.com/sse`, endpoint que a página declara removido; como emendar a regra? → R: Descartar candidato citado como removido (guarda de negação na janela do rótulo; Apify entra como stdio sem comando)
- P: (T-16) A medição real precisa comparar a duração com a da varredura antiga (três origens, no commit base)? → R: não precisa comparar (CA-10 fica só com prazo de 1 h, total ±2%, descrição e site)
- P: (relato do dono, 2026-09-11) "tentei instalar o mcp da cloudflare pela UI, mas ela não reconhece como instalar" — a página publica uma tabela de 17 endpoints sem rótulo e o item entrou como stdio sem comando; como tratar? → R: Emendar nesta mudança: reconhecer tabela de endpoints + tela explicar (RQ-03 e RQ-04 emendados, CA-13/CA-14, D-02 emenda 2 e D-07, T-18..T-20)
- P: (drift, ALTO 1) RQ-01/RQ-06 prometem a falha "com a hora da tentativa" e a tela não mostra a hora: implementar ou encolher o requisito? → R: Implementar: reabrir T-09 e mostrar a hora
- P: (drift, MEDIO/BAIXO) README com regras da origem antiga, tipo `Resultado` órfão, textos do comando de semente, comentários com a regra antiga, `t.Skip` em CA-08, e piso de 10% contando página sem título e 404 além de 5xx: corrigir tudo na mesma rodada? → R: Corrigir tudo agora (T-13, T-14, T-15 reabertas; RQ-02/CA-12 emendados para "detalhe que não pôde ser lido")

## Riscos

- **Origem única e raspada por regex**: o site pode mudar a marcação e a varredura passa a falhar em toda rodada; o catálogo anterior fica servindo sem que ninguém perceba além do aviso no admin — mitigado pelos testes com fixture real e pelo aviso de falha existente; gatilho de revisão: duas varreduras seguidas com falha de formato.
- **Bloqueio por bot**: a página responde 403 sem User-Agent de navegador (medido em 2026-09-11) e tem rate limit — mitigado por manter o User-Agent e a pausa que o código já usa; se o site passar a exigir desafio JavaScript, a biblioteca inteira para.
- **Perda de cobertura de remotos com OAuth**: se a lista de remotos sair (RQ REMOVED), o patchbay deixa de sugerir os servidores que mais precisam do fluxo OAuth que ele mesmo oferece — mitigado só por decisão explícita do dono, registrada em Esclarecimentos.
- **Semente envelhece**: a semente é regerada uma vez nesta mudança e depois só quando alguém rodar `task biblioteca:semente` — igual a hoje.
- **Link de paginação sem `/pt-BR`**: o índice anuncia `/official?page=N`; se a varredura seguir o link literal, cai no idioma inglês — mitigado por montar a URL da página a partir da base, não do link.
