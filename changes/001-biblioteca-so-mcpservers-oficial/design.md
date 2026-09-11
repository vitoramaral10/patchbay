# Desenho — 001-biblioteca-so-mcpservers-oficial

Contrato em `proposal.md` (RQ-01..06, CA-01..14); fatos do código em `estudo.md`.

## Constitution Check — antes de pesquisar

O projeto não tem `CLAUDE.md` nem `.claude/rules/`: valem as globais e as da skill `go-idiomatico`.

| Regra (origem) | O que ela exige aqui |
|---|---|
| Nunca código de produção sem mudança ativa e tarefa `- [~]` (global, spec-driven) | este arquivo só descreve; nenhuma linha de código sai daqui |
| Nunca editar `specs/` fora de `spec-arquivar` (global, spec-driven) | o desenho não cria spec viva; o delta segue em `proposal.md` |
| Binário novo: mínimo 5 caracteres, com nome do projeto, sem nome genérico (global) | dizer se nasce binário ou task — e não nasce: só o `patchbay` e a `biblioteca:semente` que já existem |
| Interface se declara no pacote consumidor (`go-idiomatico`) | a remoção do registry não pode criar interface nova em `internal/biblioteca` para uso de fora |
| Erro com `%w` e contexto, nunca comparado por string (`go-idiomatico`) | `ErrFormatoDaOrigem`/`ErrOrigemIndisponivel`/`ErrTaxaExcedida` continuam sendo a régua da varredura |
| Package by feature, sem feature importar outra (`go-idiomatico`) | `biblioteca` continua falando com `upstream` só pela query string de `rotaDeCadastro` (`admin_http.go:171-191`) |

## Decisões

### D-01 Apagar o código do registry e o da lista de remotos, em vez de desligá-los

- **Contexto** — CA-02 exige que `registry.modelcontextprotocol.io` e `remote-mcp-servers` não
  apareçam em `internal/` e `cmd/` fora de comentário histórico ou de teste de ausência. Hoje
  as duas são constantes de produção: `BaseRegistry` (`origem.go:22`), `/v0/servers`
  (`origem.go:50`), `remote-mcp-servers` (`curadoria.go:87` e `:123`). Código atrás de flag
  desligada continuaria casando com o `grep` da CA-02.
- **Decisão** — remover `origem.go`, `origem_test.go`, `Curadoria.Slugs`/`Um`/`lerCurado` e as
  expressões só delas, `varrerRegistry`/`varrerCuradoria`/`pedirComTentativas`/`curadaComTentativas`/
  `mesclar`/`chaveDeEndpoint` (`sincronizador.go:443-514, 549-569, 593-693`),
  `ComOrigemDaBiblioteca`/`ComCuradoriaDaBiblioteca` (`aplicacao.go:107-122`) e as fixtures
  `mcpservers-indice.html`, `-detalhe-aberta.html`, `-detalhe-oauth.html`, `pagina.json`,
  `ultima-pagina.json`, `vazia.json`. `varrer` (`sincronizador.go:358-372`) absorve o corpo de
  `varrerOficiais` e mantém o nome `varrer` (nota de 2026-09-11: a implementação preferiu não
  criar um wrapper), levando o dedup por nome que hoje mora em `mesclar`; `NovoSincronizador` perde o `*Origem`.
  Três símbolos de `origem.go` **não** são do registry e mudam de arquivo antes da remoção:
  `timeoutPadrao` e `tetoDaResposta` (`origem.go:30-51`, usados em `curadoria.go:70,154`)
  vão para `curadoria.go`; `padraoDoNome`/`reNome`/`nomeValido` (`origem.go:386-392`, usado
  em `repositorio.go:148`) vão para `biblioteca.go`.
- **Alternativas rejeitadas** — *manter as duas origens atrás de opção desligada*: falha a CA-02
  por leitura literal e deixa ~600 linhas que ninguém executa e todo teste compila. **Gatilho de
  revisão:** o dono pedir a lista de remotos de volta (única fonte de OAuth, risco já registrado
  no `proposal.md`) — o caminho é reverter este commit, que o histórico guarda inteiro.
- **Consequências** — a varredura fica com um alvo só e cai de ~48 min (16 do registry + ~10 dos
  293 remotos + ~22 dos oficiais) para ~22-35 min, dentro de `PrazoDaVarredura`
  (`sincronizador.go:34`). User-Agent e pausa de 2 s ficam como estão (`curadoria.go:39-53`,
  `:144`): são o que a origem exige e nada aqui os afeta. A paginação também fica: `paginaOficial`
  monta `base + "official" + "?page=N"` (`curadoria.go:397-406`) e `ultimaPagina` só lê o número
  dos links (`curadoria.go:474-482`) — o `href` literal sem `/pt-BR` nunca é seguido. Pior: uma
  mudança de marcação do mcpservers.org passa a derrubar a biblioteca inteira, não uma fatia.

### D-02 Reconhecer remoto pela descrição do servidor e por URL ancorada em rótulo de conexão

- **Contexto** — a página de detalhe do `/official` não tem bloco estruturado: o
  `<dt>Transporte</dt>` que `lerCurado` lê (`curadoria.go:260-268`) não existe lá (6 detalhes
  baixados em 2026-09-11; o único `<dt>` do AdMake é "Categoria"). "Detalhes da conexão",
  "Transporte" e "Autenticação" aparecem em toda página dentro do dicionário i18n do payload —
  casar com elas acerta por acidente. E a página traz URL de MCP **de outros servidores**: cartões
  de relacionados (`mcp.dexi.net/mcp` na página do 1Password) e um bloco gerado pelo site,
  "Servidor remoto hospedado", apontando para `/pt-BR/remote-mcp-servers/<vendor>`.
- **Decisão** — ler só a região entre o `</h1>` e o primeiro `href=".../servers/"` ou
  `href=".../remote-mcp-servers/"` posterior, e nela: (1) **sinal de remoto** é a descrição do
  próprio servidor (o `<p>` logo após o `<h1>`) casar
  `(?i)servidor[^.<]{0,24}remoto|remote mcp server|streamable http|http (streamable|transmiss)`,
  ou existir URL ancorada; (2) **URL de conexão** é `https://` cujo caminho termina em `/mcp`,
  `/mcp/`, `/sse` ou `/sse/`, tirada da descrição ou precedida em até 120 caracteres por
  `endpoint|conecte-se em|connect to|adicione|streamable http|http transmissível|url de conexão|connection urls?`;
  (3) só há remoto quando 1 e 2 valem — sem URL, o item segue o D-03. **Guarda de negação
  (emenda de 2026-09-11, decisão do dono após ESCALAR de T-06):** um candidato a URL é
  descartado quando os 120 caracteres antes **ou** depois dele citam
  `(?i)removido|removed|legado|legacy|deprecated|descontinuado|não está mais|no longer`, venha
  o candidato do rótulo ou da descrição; o próximo candidato da região é avaliado, e sem
  candidato válido o item segue o D-03. Caso
  medido: a página do Apify publica `https://mcp.apify.com/sse` na frase "Transporte SSE
  legado removido … migre para `https://mcp.apify.com`", e o endereço vivo não termina em
  `/mcp` nem `/sse` — falso negativo aceito, melhor que URL morta no catálogo. Transporte
  `sse` quando o caminho termina em `/sse`, senão `http`. `Autenticacao` recebe `AutOAuth` só quando a mesma
  região cita OAuth; sem citação fica vazia, e `ModoDeCredencial` (`biblioteca.go:195-200`)
  continua não chutando. **Emenda 2 (2026-09-11, relato do dono sobre o Cloudflare):** dentro da
  mesma região, uma `<table>` cujo `<thead>` tem uma coluna cujo texto contém `URL` ou `endpoint`
  (sem distinção de caixa) ancora, por si só, as URLs `https` terminadas em `/mcp`, `/mcp/`, `/sse`
  ou `/sse/` que estiverem nas `<td>` dessa coluna — sem exigir rótulo em 120 caracteres nem sinal
  na descrição; a guarda de negação continua valendo célula a célula, e URLs de outras colunas
  (nome, descrição, link de repositório) não contam. Com uma URL, é o caso comum. Com várias, a
  URL do item é a da primeira linha cujo texto contém `recomendado`/`recommended`, senão a da
  primeira linha, e **todas** entram em `Item.Endpoints`, na ordem da página (D-07). Medido na
  página do Cloudflare (108 kB, 2026-09-11): tabela "Nome do Servidor | Descrição | URL do
  Servidor", 17 linhas, primeira "Servidor Code Mode (recomendado)" → `https://mcp.cloudflare.com/mcp`;
  a coluna de nome traz links `github.com/cloudflare/mcp…`, que a restrição de coluna descarta.
  **Precedência e invariantes:** havendo tabela válida na região, ela ganha do rótulo e da
  descrição — `URL` e `Endpoints` saem da tabela, e o rótulo/descrição só valem quando não há
  tabela com coluna de URL. `Endpoints` recebe todos os endpoints da tabela (um ou mais), na
  ordem; sem tabela fica vazio. Invariante: se `Endpoints` não é vazio, `URL` é um deles. Em
  qualquer coluna, URL cujo host é `github.com`, `gitlab.com` ou `bitbucket.org` nunca é
  endpoint, mesmo terminando em `/mcp` — é o caso real da coluna de nome do Cloudflare.
  Não-regressão das páginas locais calibradas em T-06 (1Password, Airtable, Magic) e do Ahrefs:
  T-18 baixa as quatro de novo; as que tiverem `<table>` na região viram fixture de teste, as
  demais são reconferidas à mão e o resultado fica datado no retorno da tarefa (nota de
  2026-09-11: a calibração original foi anterior à regra de tabela).
- **Alternativas rejeitadas** — *qualquer URL da página que contenha `/mcp`*: nas 6 amostras isso
  cadastraria `mcp.dexi.net/mcp` (relacionado) na página do 1Password e `mcp.airtable.com/mcp`
  (bloco do site) no Airtable, que é servidor local — a URL errada do caminho triste de RQ-03.
  *Usar o campo `url:` do payload da página*: nas 6 amostras ele é o **site** do fornecedor
  (`url:"https://admakeai.com"`), não o endpoint. **Gatilho de revisão:** o site passar a publicar
  `<dt>Transporte</dt>` no `/official`, como já faz nos remotos — aí a regra vira leitura de campo.
- **Consequências** — nas 6 amostras: 3 remotos com a URL certa (`admakeai.com/api/mcp`,
  `gateway.ansvar.eu/mcp`, `api.ahrefs.com/mcp/mcp`) e 3 locais sem URL nenhuma (1Password,
  21st.dev Magic, Airtable). Falso negativo previsto: remoto que publica o endpoint sem nenhum
  desses rótulos ou com caminho que não termina em `/mcp`//`sse` — entra como item sem comando
  (D-03), o erro barato. Falso positivo previsto: README que documenta o endpoint remoto de
  *outro* produto da mesma empresa dentro da região lida. Pior: a regra é uma pilha de
  heurísticas sobre prosa de terceiro, e cada uma tem prazo de validade.

### D-03 Item sem comando entra como `stdio` com comando vazio, e o site vem do primeiro link

- **Contexto** — RQ-02 manda o item entrar sem comando, e é justamente esse caso que `lerOficial`
  recusa hoje com `ErrFormatoDaOrigem` (`curadoria.go:511-516`). O `CHECK` de `transporte` só
  aceita `http`, `sse` ou `stdio` (`00013_biblioteca.sql:30`): "sem transporte" não é
  representável. E CA-05 exige site, que `lerOficial` nunca preenche.
- **Decisão** — item sem comando entra com `transporte='stdio'`, `comando=''`, `args='[]'`. Em
  `rotaDeCadastro` (`admin_http.go:171-191`) `comando` e `arg` só entram na query quando há
  comando — o formulário abre com `tipo=stdio`, `nome=` e comando vazio; o cartão
  (`admin.templ:211`) mostra "sem comando publicado" no lugar da linha de execução. `Site` passa a
  ser o primeiro `<a href="https://…" target="_blank">` dentro de 4 KB depois da descrição: nas 6
  amostras é sempre a origem do servidor, e o segundo é sempre anúncio de terceiro
  (`ottermind.ai`), que a regra do "primeiro" descarta. Aceitar também snippet com `"command"` sem
  `"args"` — caso do 1Password, hoje recusado por `reComandoDoSnippet` (`curadoria.go:446`). Descrição continua opcional, como `lerOficial` já faz (`curadoria.go:505-509`):
  item sem descrição entra com o campo vazio e é contado, não recusado; CA-10 mede quantos
  foram e reprova acima de zero — a lista real traz descrição em todo card (2026-09-11).
- **Alternativas rejeitadas** — *seguir exigindo `command` + `args` juntos*: joga fora comando
  publicado e limpo (1 das 6 amostras) por uma vírgula de JSON. **Gatilho de revisão:** aparecer
  servidor cujo comando sem args não sobe sozinho.
- **Consequências** — passa a existir cartão que não preenche o formulário inteiro: o admin
  escolhe o servidor e ainda digita o comando. `TestAdicionarDeServidorLocalLevaAExecucao`
  (`admin_http_test.go:172`) segue valendo para o item **com** comando; a asserção
  `comConexao != len(itens)` de `TestSementeVersionadaEValida` (`semente_test.go:165`) deixa de
  valer e vira contagem no `t.Logf`. Pior: "Adicionar" deixa de ser promessa de formulário pronto.

### D-04 Descartar o catálogo antigo numa migração goose, não em `semear`

- **Contexto** — RQ-06 quer o catálogo antigo fora **antes** de qualquer varredura, e `semear` só
  grava quando `estado.Nunca()` (`sincronizador.go:197-201`), falso em instalação que já varreu.
  Distinguir "linha do registry" pelo formato do nome não funciona: os 293 remotos curados também
  se chamam `mcpservers.org/<slug>` (`curadoria.go:226`) e são exatamente o que precisa sair.
  Nenhuma tabela referencia `biblioteca_servidor` — `grep biblioteca_servidor` acha só
  `repositorio.go` e as duas migrações — e o cadastro copia os valores pela query string
  (`admin_http.go:171-191`), então apagar linha não toca upstream nenhum.
- **Decisão** — migração `00015_biblioteca_so_oficiais.sql` com `DELETE FROM biblioteca_servidor`
  e `UPDATE biblioteca_sincronizacao SET concluida_em=0, tentada_em=0, servidores=0, erro='' WHERE id=1`.
  Com o estado zerado o boot cai em `Nunca()`, `semear` grava a semente nova e `Manter` varre em
  seguida (`sincronizador.go:172`), sem uma linha de Go nova. `Down` é no-op comentado: linha
  apagada não volta, e a próxima varredura reconstrói.
- **Alternativas rejeitadas** — *limpar dentro de `semear`, filtrando por formato de nome*: não
  separa registry de remoto curado (acima) e deixa regra de migração dentro do sincronizador para
  sempre. **Gatilho de revisão:** aparecer catálogo que precise sobreviver à troca de origem.
- **Como o teste prova** — `store.Migrar` aplica tudo de uma vez (`migracoes.go:33`, `p.Up`), então
  não há como deixar linhas de registry no banco "antes" da 00015 pelo caminho normal. O teste de
  `cmd/patchbay` encena o estado: roda `store.Migrar` inteiro num banco temporário, insere linhas
  em formato de registry e um upstream, apaga a linha `version_id = 15` de `goose_db_version`
  e sobe o app — a 00015 roda de novo e o resto do boot segue. A semente entra por opção de
  teste nova, `ComSementeDaBiblioteca(itens, geradoEm)`, irmã de `SemSementeDaBiblioteca`
  (`aplicacao.go:131`) e pelo mesmo motivo: sem ela o teste dependeria do `semente.json.gz`
  real, que muda a cada release.
- **Consequências** — quem atualiza vê no primeiro boot a semente nova e a idade dela.
  `TestSementeNaoSobrescreveCatalogoJaVarrido` (`semente_test.go:86`) continua válido sem
  alteração: ele monta o estado pelo repositório, sem passar pela migração. Pior: rollback para o
  binário anterior acha catálogo vazio e espera uma varredura inteira do registry.

### D-05 Tirar do Go o que perdeu sentido; manter as colunas e derrubar só o índice

- **Contexto** — com origem única somem `versao` (só o registry publicava), `curado` (todo item é
  curado), `Namespace()`/`DominioVerificado()`/`namespacesDeFoundry` (`biblioteca.go:206-241`),
  que dependem de nome em DNS invertido, e os filtros da tela (`admin.templ:111-125`,
  `admin_http.go:66`, `repositorio.go:67-74, 202-206`).
- **Decisão** — remover do Go os campos `Versao` e `Curado`, os dois métodos e o vetor,
  `Filtro.SoCurados`, o `curado = 1` de `filtroDe`, o `ORDER BY curado DESC` (`repositorio.go:125`,
  que vira `ORDER BY nome`), o checkbox e os selos do `.templ`, e `versao`/`curado` da projeção e
  do `INSERT` (`repositorio.go:58-59, 247-251`). Manter as duas colunas (`versao` e `curado`) com seus `DEFAULT` e
  derrubar só o índice `biblioteca_servidor_curado` (`00014:43`) na 00015.
  `Autenticacao`/`PedeCredencial` continuam vivos: são o que D-02 preenche nos remotos.
- **Alternativas rejeitadas** — *`DROP COLUMN` das duas*: custa quebrar o binário anterior no
  `INSERT` da varredura, se alguém voltar, e ganha uma coluna vazia a menos em 652 linhas.
  *Manter o índice*: escrita a cada varredura para consulta que não existe mais. **Gatilho de
  revisão:** outra mudança precisar mexer no esquema — aí o `DROP COLUMN` entra sozinho.
- **Consequências** — o esquema passa a mentir em duas colunas, e só o comentário da 00015 conta
  por quê. A tela perde o selo "curado" e a ordenação por curadoria; a lista sai por nome.

### D-06 Regerar a semente pela task existente, sem o `-so-curados`

- **Contexto** — a semente tem 509 itens da mescla das três origens (`semente.go:26-27`); o
  comentário da task documenta o uso com `-- --so-curados` (`Taskfile.yml:70`; o `cmds` em `:74`
  só repassa `{{.CLI_ARGS}}`), flag implementada em `biblioteca_semente.go:32-33, 49-57` e
  anunciada na ajuda de `main.go:98`. Com `Curado` fora do `Item` (D-05) ela não tem o que filtrar.
- **Decisão** — remover a flag do comando, do texto que ele imprime e do comentário da task; a
  task segue sendo o único caminho e passa a gravar os ~652 itens da lista oficial.
- **Alternativas rejeitadas** — *passo novo de build*: duplicaria um comando que já existe e já
  grava por arquivo temporário + rename.
- **Consequências** — a semente cresce (23 kB hoje, com 509 itens curtos) e blob no git é
  permanente. Quem garante que ela não traz nome de registry é `TestSementeVersionadaEValida`
  (`semente_test.go:136`), que ganha a asserção de prefixo `mcpservers.org/` em todo `Nome`
  (CA-08) e perde a de `curados == 0` (`:168`).

### D-07 Guardar os endpoints publicados e deixar o admin escolher na tela

- **Contexto** — `Item` tem uma `URL` só (`biblioteca.go`) e a tabela `biblioteca_servidor` uma
  coluna `url`. A página do Cloudflare publica 17 endpoints, um por produto; escolher um em
  silêncio esconde os outros 16, e não gravar nenhum foi exatamente o que deixou o dono sem
  saber como instalar. O cartão hoje diz só "sem comando publicado" (`admin.templ`).
- **Decisão** — novo campo `Item.Endpoints []string` (todos os endpoints da tabela, um ou mais;
  vazio sem tabela; `URL` é sempre um deles) persistido em coluna nova `endpoints TEXT NOT NULL DEFAULT '[]'` (JSON),
  migração `00016_biblioteca_endpoints.sql` (`Down` no-op comentado, como a 00015); o
  `INSERT`/`SELECT` do repositório passam a ler/gravar a coluna; a semente serializa o campo (o
  `json` já ignora campo ausente nas sementes antigas). Na tela: cartão com `len(Endpoints) > 1`
  lista cada endpoint com um link "Adicionar" próprio — rota
  `GET /admin/biblioteca/adicionar/{nome}?endpoint=<i>`, que monta a query de `rotaDeCadastro`
  com `url` daquele índice (índice fora da faixa cai no padrão, a `URL` do item); cartão de item
  sem comando nem URL passa a dizer que a página do servidor não publica comando nem endpoint
  reconhecível, com link para `https://mcpservers.org/pt-BR/servers/<slug>` (o slug é o `Nome`
  sem o prefixo) e para o `Site`, e "Adicionar" segue abrindo o formulário só com o nome.
- **Alternativas rejeitadas** — *escolher o primeiro e não guardar o resto*: simples, mas
  volta a esconder 16 servidores do Cloudflare atrás de um só, e o admin não descobre pela tela.
  **Gatilho de revisão:** se a lista oficial passar a publicar um servidor por endpoint, o campo
  fica vazio em todo lugar e pode sair. *Tabela própria `biblioteca_endpoint`*: normalização para
  um dado que é lido junto do item e nunca consultado sozinho — coluna JSON basta.
- **Consequências** — mais uma migração (00016) e uma coluna que fica vazia em ~640 de 651
  itens; a semente cresce um pouco; o cartão do Cloudflare fica alto (17 linhas), e é o preço
  de mostrar o que existe. Pior: a regra de tabela é mais uma heurística sobre HTML de
  terceiro, com o mesmo prazo de validade das outras.

## Modelo de dados e contratos

| O quê | Antes | Depois | Migração |
|---|---|---|---|
| `biblioteca_servidor` (linhas) | mescla de 3 origens, ~29,6 mil | só `/official`, ~652 | `00015`: `DELETE FROM biblioteca_servidor` |
| `biblioteca_sincronizacao` | estado da última varredura | igual | `00015`: zera `concluida_em`, `tentada_em`, `servidores`, `erro` para o boot semear |
| `biblioteca_servidor.versao` / `.curado` | preenchidas na varredura | colunas mantidas, nunca escritas (ficam no `DEFAULT`) | nenhuma; saem da projeção e do `INSERT` |
| índice `biblioteca_servidor_curado` | filtro "só curados" | nenhuma consulta o usa | `00015`: `DROP INDEX` |
| `Item` (Go) | com `Versao` e `Curado` | sem os dois; `Autenticacao`/`PedeCredencial` só nos remotos de D-02 | não aplicável |
| `NovoSincronizador` | `(origem, curadoria, repo, log, …)` | `(curadoria, repo, log, …)` | não aplicável; só `cmd/patchbay` chama |
| `GET /admin/biblioteca?curados=1` | recorta por curadoria | parâmetro ignorado (some da tela e do `Filtro`) | link velho responde 200, sem filtro |
| `GET /admin/biblioteca/adicionar/{nome…}` | query com `comando`+`arg` | sem `comando` quando não há comando | contrato com `internal/upstream` inalterado |
| `biblioteca_servidor.endpoints` (nova) | — | `TEXT NOT NULL DEFAULT '[]'`, JSON com a lista de endpoints publicados (D-07) | `00016`: `ALTER TABLE … ADD COLUMN`; compatível, linhas antigas ficam `'[]'` |
| `Item.Endpoints` (Go) | — | `[]string`: todos os endpoints da tabela (um ou mais), na ordem; vazio sem tabela; `URL` é sempre um deles | não aplicável; semente antiga decodifica com o campo vazio; T-21 regera a semente |
| `GET /admin/biblioteca/adicionar/{nome…}?endpoint=<i>` | — | redireciona ao formulário com `url` = `Endpoints[i]`; sem `endpoint` ou índice inválido, usa `URL` | contrato com `internal/upstream` inalterado |

## Estratégia de teste

Suíte do pacote: `go test ./internal/biblioteca/ -count=1`; integração: `go test ./cmd/patchbay/ -count=1`.

| CA | Nível | Como se prova |
|---|---|---|
| CA-01 | integração (httptest) | servidor de teste com índice e detalhes gravados, contando requisições por caminho: `go test ./internal/biblioteca/ -run 'TestVarreduraSoVaiAoOficial' -count=1` |
| CA-02 | comando | `grep -rn --include=*.go --include=*.templ "registry\.modelcontextprotocol\.io\|remote-mcp-servers" internal/ cmd/` sem saída (`testdata/` fora: HTML do site cita as strings) |
| CA-03 | unitário | origem falsa em 403 e em 500 no índice; catálogo anterior intacto e `erro`/`tentada_em` gravados: `-run 'TestIndiceForaDoArPreservaOCatalogo'` |
| CA-04 | unitário | índice 200 sem `href=".../servers/"` → `ErrFormatoDaOrigem` e `Substituir` não chamado: `-run 'TestIndiceSemServidorNaoEsvaziaOCatalogo'` |
| CA-05 | unitário | fixtures `mcpservers-oficial-comando.html` (Anki) e `-sem-comando.html` (Apify), já páginas reais e mantidas como estão → item com e sem comando, os dois com nome, descrição e site: `-run 'TestOficiaisLeemPaginaDeVerdade|TestOficialSemComandoEntraComSite|TestSnippetComMarcadorDeExemploNaoViraCadastro'` |
| CA-06 | unitário | fixtures novas: detalhe remoto (AdMake ou Ansvar) → `http` + URL; detalhe local cuja única URL é o site do fornecedor (Anki) → sem URL; fixture do Apify (`mcpservers-oficial-sem-comando.html`, também carga de CA-05), cujo único endpoint é declarado removido → stdio sem URL, pela guarda de negação: `-run 'TestOficialRemotoViraItemHTTP|TestURLDeSiteNaoViraConexao'` |
| CA-07 | handler | `-run 'TestTela'` conferindo no HTML: sem "registry", sem "modelcontextprotocol.io", sem `name="curados"`, rodapé em `https://mcpservers.org/pt-BR/official` |
| CA-08 | unitário | `-run 'TestSementeVersionadaEValida'` com a asserção de prefixo `mcpservers.org/` em todo `Nome` |
| CA-09 | integração | banco pré-carregado com nomes de registry, migrações aplicadas e app subido: catálogo só com a semente, e um upstream criado antes continua legível: `go test ./cmd/patchbay/ -run 'TestCatalogoDoRegistryEDescartadoNaAtualizacao' -count=1` |
| CA-10 | manual | binário da branch contra o site real; registrar em `verificacao.md`: início e fim, a `duracao` do log `biblioteca sincronizada`, o total gravado contra o número do título da página naquele dia, e quantos itens sem descrição ou sem site (a comparação com a varredura antiga foi dispensada pelo dono em 2026-09-11) |
| CA-11 | comando | `grep -in "registry\|modelcontextprotocol\.io" README.md` sem saída no arquivo inteiro |
| CA-13 | unitário | fixture `mcpservers-oficial-tabela-endpoints.html` (Cloudflare, página real de 2026-09-11) → `http`, `URL=https://mcp.cloudflare.com/mcp`, 17 `Endpoints` na ordem; fixture sintética com tabela cuja coluna URL só tem links de repositório → sem URL: `-run 'TestTabelaDeEndpointsViraRemoto|TestTabelaSoDeRepositoriosNaoViraConexao'` |
| CA-14 | handler | item com 2 endpoints → 2 links "Adicionar" com `?endpoint=0`/`1`, cada redirect com `url=` do endpoint certo; item sem comando nem URL → texto explicativo, link para `https://mcpservers.org/pt-BR/servers/<slug>` e para o site: `-run 'TestTelaListaEndpoints|TestTelaExplicaItemSemComandoNemURL'` |
| CA-08/CA-13 (semente) | comando + unitário | T-21 regera `semente.json.gz` depois da regra de tabela: `git diff --stat` mostra o .gz mudado e `TestSementeVersionadaEValida -v` imprime o item `mcpservers.org/cloudflare/mcp-server-cloudflare` com URL e 17 endpoints |
| CA-12 | unitário | servidor de teste com 11% dos detalhes em 500 → varredura falha e `Substituir` não chamado; com 5% → conclui e grava os demais: `-run 'TestPisoDeDetalhesIndisponiveis'`. Piso de 10% sobre o total de links do índice, contado em `varrerOficiais` (`sincronizador.go:405-412`, que hoje só incrementa `indisponiveis`) |

Deliberadamente **não** testado: o HTML real do mcpservers.org em teste automatizado (a suíte não
sai para a rede — as fixtures são cópias datadas); a pausa de 2 s e o User-Agent, inalterados e já
cobertos por `TestCuradoriaInsisteEmTaxaExcedida`; e o `Down` da 00015, que é no-op.

## Riscos de segunda ordem

- **Duração encosta no prazo** — 652 detalhes a 2 s de pausa mais a resposta dão ~22-35 min contra
  `PrazoDaVarredura` de 1 h (`sincronizador.go:34`), que está fora de escopo mudar. Sinal: a
  `duracao` no log de cada varredura; passar de 40 min é hora de rever pausa ou prazo.
- **A lista cresce** — 652 hoje; `TetoDePaginasOficiais` é 60 páginas (`curadoria.go:359`), ~1.800
  servidores, e aí o prazo estoura antes do teto. Sinal: o total gravado subindo a cada varredura.
- **Fixtures datadas viram ficção** — os testes passam com HTML de 2026-09-11 enquanto a varredura
  real falha. Sinal: duas varreduras seguidas com `ErrFormatoDaOrigem` no admin, gatilho já
  registrado no `proposal.md`.
- **`semear` vira caminho crítico** — com a 00015 zerando o estado, toda instalação atualizada
  passa por ele; semente ilegível hoje só gera `Warn` (`sincronizador.go:204`) e deixaria a tela
  vazia até a primeira varredura.

## Constitution Check — depois de desenhar

| Regra | Veredito | Nota |
|---|---|---|
| Nunca código de produção sem tarefa `- [~]` | ✅ | o desenho descreve; a implementação entra por `tasks.md` |
| Nunca editar `specs/` fora de `spec-arquivar` | ✅ | nada em `specs/`; o delta segue em `proposal.md` |
| Nomenclatura de binário | ✅ | nenhum binário novo e nenhuma task nova — `biblioteca:semente` só perde uma flag (D-06) |
| Interface no pacote consumidor | ✅ | D-01 só apaga tipos; `NovoSincronizador` continua recebendo struct concreta do próprio pacote |
| Erro com `%w`, nunca comparado por string | ✅ | D-02 e D-03 devolvem `ErrFormatoDaOrigem` embrulhado; "sem comando" deixa de ser erro e vira campo vazio, não string comparada |
| Package by feature, sem feature importar outra | ✅ | `biblioteca` continua sem importar `upstream`; D-03 muda a query string do link, não a direção da dependência |
