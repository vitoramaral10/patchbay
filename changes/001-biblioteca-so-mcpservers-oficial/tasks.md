# Tarefas — 001-biblioteca-so-mcpservers-oficial

Estado: `[ ]` aberta · `[~]` em andamento · `[x]` concluída com evidência · `[!]` bloqueada.
`[P]` = paralelizável com as demais marcadas.

Ordem é por dependência: T-01 → T-03 deixam a árvore compilando com uma origem só, e todo o
resto pende delas. Decisões em `design.md` (D-01..D-07); contrato em `proposal.md`.

- [x] T-01 Varrer só a lista oficial, sem passar pelo registry
  _Requisitos: RQ-01, CA-01, CA-02_
  Arquivos: o pacote `internal/biblioteca/` (código e testes), restrito ao que a remoção do registry e da varredura de remotos exige para compilar e passar — pontos conhecidos: `sincronizador.go`, `sincronizador_test.go`, `curadoria.go` (recebe `timeoutPadrao` e `tetoDaResposta`, hoje em `origem.go:30-51`, usados em `curadoria.go:70,154`), `biblioteca.go` (recebe `padraoDoNome`/`reNome`/`nomeValido`, hoje em `origem.go:386-392`, usados em `repositorio.go:148`), `semente_test.go` (as chamadas de `NovaOrigem` em `:44-45`, `:107-108`, `:196-197` e o teste `:183-217`), `curadoria_test.go` (a montagem em `:327-328` e a expectativa de `:332-336`, que cai de 2 para 1 item porque o remoto da segunda tentativa não é mais varrido), `admin_http_test.go` (`telaComSinal`, `:41-42`, e `:235`, que passa a esperar um nome `mcpservers.org/<slug>` em vez de `com.exemplo/um`), `internal/biblioteca/origem.go` (apagar), `internal/biblioteca/origem_test.go` (apagar), `internal/biblioteca/testdata/pagina.json` (apagar), `internal/biblioteca/testdata/ultima-pagina.json` (apagar), `internal/biblioteca/testdata/vazia.json` (apagar)
  Verificação: `go test ./internal/biblioteca/ -count=1`
  Tier: opus
  Evidência: `go test ./internal/biblioteca/ -count=1` → ok (2026-09-11); vet e gofmt limpos; revisor APROVADA; origem.go, origem_test.go e 3 JSON de testdata apagados

  D-01. `varrer` passa a ser só `varrerOficiais`, levando o dedup por nome que hoje mora em
  `mesclar`; saem `varrerRegistry`, `pedirComTentativas`, `varrerCuradoria`,
  `curadaComTentativas`, `mesclar`, `chaveDeEndpoint`, `chaveDeExecucao`, `semVersao`, as
  constantes `pisoDaCuradoria` (`:44`) e `tetoDePaginas` (`:58`), que ficariam sem uso e o
  `golangci-lint` acusaria, o campo `origem` do struct e o parâmetro `*Origem` de
  `NovoSincronizador`; o log de `tentar`
  (`sincronizador.go:329`) deixa de citar `s.origem.base`. Testes que morrem com o registry:
  `TestVarreduraPercorreTodasAsPaginas`, `TestVarreduraInsisteNaPaginaQueFalhou`,
  `TestCursorQueSeRepeteNaoViraVarreduraSemFim`, `TestMesclagemJuntaAsDuasOrigens`,
  `TestMesclagemNaoDeixaNomeColidir`, `TestCuradoriaForaDoArDerrubaAVarredura` — o dedup por
  nome ganha teste próprio sobre a lista oficial. **Antes de apagar `origem_test.go`**, os
  auxiliares que ele hospeda e que outros testes usam — `amostra` (`origem_test.go:29`,
  usado em `curadoria_test.go:349-355`), `servir` (`:45`, usado em `semente_test.go:41,104`)
  e o tipo `origemDeMentira` (`:39`) — mudam para `sincronizador_test.go` com os mesmos
  nomes; `servir` deixa de responder `/v0/servers` (`:54`) e passa a servir um índice oficial
  vazio. `curadoria_test.go` só perde a origem do registry em `:327-328` e `admin_http_test.go`
  só em `telaComSinal` (`:41-42`); as linhas `:41`/`:104` de `semente_test.go` não mudam. Já
  `TestSementeFrescaAindaAssimVarre` (`semente_test.go:183-217`) muda: ele monta o cenário com
  `registryDeMentira` (`:190`, definido em `sincronizador_test.go:31`) e assere
  `repo.Um("com.exemplo/um")` (`:214`); passa a servir índice `/pt-BR/official` + detalhe e a
  asserir um nome `mcpservers.org/<slug>`. `registryDeMentira` sai; `curadoriaDeMentira` e
  `curadoriaMuda` (`curadoria_test.go:27`) ficam para T-02 apagar ou reescrever. **Deixa `./cmd/patchbay/` sem compilar até
  T-03**: é dependência de ordem conhecida, não regressão.

- [x] T-02 Apagar o cliente da lista de remotos curados
  _Requisitos: RQ-01, CA-02_
  Arquivos: `internal/biblioteca/curadoria.go`, `internal/biblioteca/curadoria_test.go`, `internal/biblioteca/testdata/mcpservers-indice.html` (apagar), `internal/biblioteca/testdata/mcpservers-detalhe-aberta.html` (apagar), `internal/biblioteca/testdata/mcpservers-detalhe-oauth.html` (apagar)
  Verificação: `go test ./internal/biblioteca/ -count=1 && ! grep -rl --include=*.go --include=*.templ "remote-mcp-servers" internal/biblioteca/`
  Tier: sonnet
  Evidência: suíte ok + grep vazio (2026-09-11); vet e gofmt limpos; revisor APROVADA; 3 fixtures de remotos apagadas

  D-01. Saem `Slugs`, `Um`, `lerCurado`, `indiceDaConexao`, `campoCurado`, `moldeListaDefine`,
  `slugValido`, `reCartaoCurado`, `reEnderecoCurado`, `reSlug` e os testes
  `TestCuradoriaLeListaEDetalhe`, `TestCuradoriaLePaginaDeVerdade`,
  `TestCuradoriaRecusaOQueNaoDaParaCadastrar`. Ficam: `buscar`, `desafioDeBot`,
  `agenteDeNavegador`, `esperaEntreCuradas`, `reTituloCurado`, `reSobreCurado`,
  `autenticacaoDe` (usada por T-06) e `TestCuradoriaInsisteEmTaxaExcedida`, que passa a
  insistir numa página de `/servers/`. `mcpservers-desafio.html` fica. O comentário de
  `agenteDeNavegador` (`curadoria.go:50`) cita `/remote-mcp-servers` e é reescrito aqui,
  senão o grep da verificação não passa. Os dublês `curadoriaDeMentira` e `curadoriaMuda`
  (`curadoria_test.go:27`), que servem `/pt-BR/remote-mcp-servers`, passam a servir
  `/pt-BR/official` + `/pt-BR/servers/{slug}` ou saem, conforme o que os testes
  sobreviventes ainda usarem.

- [x] T-03 Subir o painel com a origem única
  _Requisitos: RQ-01, CA-01, CA-02_
  Arquivos: `cmd/patchbay/aplicacao.go`, `cmd/patchbay/biblioteca_semente.go` (só a chamada de `NovoSincronizador` em `:40-41`, que perde o `NovaOrigem("")`), `cmd/patchbay/biblioteca_ui_test.go`, `cmd/patchbay/ui_test.go`, `cmd/patchbay/chave_test.go`, `cmd/patchbay/integracao_test.go`, `cmd/patchbay/oauth_test.go`
  Verificação: `go test ./cmd/patchbay/ -count=1`
  Tier: sonnet
  Evidência: `go test ./cmd/patchbay/ -count=1` → ok (2026-09-11); build/vet/gofmt limpos; grep de registry e remotos em cmd/patchbay vazio; revisor APROVADA

  D-01. Saem `ComOrigemDaBiblioteca`, `opcoesApp.origemBiblioteca` e `biblioteca.NovaOrigem`
  do wiring (`aplicacao.go:107-114, 297-304`) e da chamada em `biblioteca_semente.go:40-41`; `ComCuradoriaDaBiblioteca` fica, e é por ela
  que todo teste aponta para o servidor falso. Os falsos de teste passam a servir
  `GET /pt-BR/official` e `GET /pt-BR/servers/{slug}` no lugar de `/v0/servers` e
  `/pt-BR/remote-mcp-servers` — `origemFalsa` e `registryMudo` somem, `curadoriaMudaDeTeste`
  passa a responder um índice oficial vazio de servidores. `biblioteca_ui_test.go:133` deixa
  de exigir "registry.modelcontextprotocol.io" na página. Os três últimos arquivos só perdem
  um argumento na chamada de `subirUI`. `TestAdicionarDaBibliotecaAbreFormularioPreenchido`
  (`biblioteca_ui_test.go:177-208`) é o guardião do contrato com `internal/upstream`
  (`admin_http.go:161-165`) e **não sai**: até T-06 nenhum item do `/official` é remoto
  (`lerOficial` fixa `TransporteSTDIO`, `curadoria.go:489-518`) nem tem `Autenticacao`, então
  aqui ele passa a provar `tipo=stdio`, `nome` e `comando`/`arg` do item oficial **local**
  servido pelo falso; as asserções de `url`, `value="oauth"` e `checked` voltam em T-06,
  sobre um detalhe oficial remoto com OAuth.

- [x] T-04 Aceitar servidor oficial sem comando, com site preenchido
  _Requisitos: RQ-02, CA-05_
  Arquivos: `internal/biblioteca/curadoria.go`, `internal/biblioteca/curadoria_test.go`, `internal/biblioteca/sincronizador.go`, `internal/biblioteca/sincronizador_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestOficiaisLeemPaginaDeVerdade|TestOficialSemComandoEntraComSite|TestSnippetComMarcadorDeExemploNaoViraCadastro' -count=1`
  Tier: sonnet
  Evidência: os 3 testes PASS e suíte ok (2026-09-11); vet e gofmt limpos; revisor APROVADA; Site do Apify = https://github.com/apify/apify-mcp-server

  D-03. `lerOficial` para de devolver `ErrFormatoDaOrigem` quando não há comando
  (`curadoria.go:511-516`) e devolve item com `TransporteSTDIO`, `Comando` e `Args` vazios —
  inclusive para snippet com caminho de exemplo (`C:\PATH\TO\...`), que RQ-02 trata como
  "sem comando aproveitável": `TestSnippetComMarcadorDeExemploNaoViraCadastro`
  (`curadoria_test.go:399-414`) passa a esperar item sem comando em vez de erro, e em
  `TestOficiaisLeemPaginaDeVerdade` (`curadoria_test.go:394`) a asserção de
  `ErrFormatoDaOrigem` para `apify-mcp-server` passa a esperar item sem comando, com nome,
  descrição e site;
  passa a aceitar snippet com `"command"` sem `"args"`; e preenche `Site` com o primeiro
  `<a href="https://…" target="_blank">` nos 4 KB seguintes à descrição. Em `varrerOficiais`
  o contador `sem_comando` deixa de contar recusa e passa a contar item gravado sem comando.
  **As duas fixtures atuais já bastam e não devem ser rebaixadas**: conferido em 2026-09-11,
  `mcpservers-oficial-comando.html` é a página real do Anki (`npx` + 3 args, site
  `github.com/ankimcp/anki-mcp-server`) e `-sem-comando.html` é a do Apify (sem snippet, site
  `github.com/apify/apify-mcp-server`). O teste novo cobre a segunda: item com nome, descrição
  e site, sem comando e sem erro.

- [x] T-05 Abrir o formulário sem comando quando o item não tem comando
  _Requisitos: RQ-02, CA-05_
  Arquivos: `internal/biblioteca/admin_http.go`, `internal/biblioteca/admin_http_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestAdicionar' -count=1`
  Tier: sonnet
  Evidência: 4 TestAdicionar PASS e suíte ok (2026-09-11); vet e gofmt limpos; revisor APROVADA

  D-03. Em `rotaDeCadastro` (`admin_http.go:171-191`), `comando` e `arg` só entram na query
  quando há comando; `tipo` e `nome` continuam sempre.
  `TestAdicionarDeServidorLocalLevaAExecucao` (`admin_http_test.go:172`) fica como está e o
  teste novo prova o caso vazio: `Location` sem `comando=` e sem `arg=`.

- [x] T-06 Cadastrar como remoto o oficial que publica URL de MCP
  _Requisitos: RQ-03, CA-06_
  Arquivos: `internal/biblioteca/curadoria.go`, `internal/biblioteca/curadoria_test.go`, `internal/biblioteca/testdata/mcpservers-oficial-remoto.html` (novo), `internal/biblioteca/testdata/mcpservers-oficial-remoto-tabela.html` (novo), `cmd/patchbay/biblioteca_ui_test.go` (só `TestAdicionarDaBibliotecaAbreFormularioPreenchido`)
  Verificação: `go test ./internal/biblioteca/ -run 'TestOficialRemotoViraItemHTTP|TestURLDeSiteNaoViraConexao' -count=1 && go test ./cmd/patchbay/ -run 'TestAdicionarDaBibliotecaAbreFormularioPreenchido' -count=1`
  Tier: opus
  Evidência: testes da tarefa PASS, suítes de biblioteca e cmd/patchbay ok (2026-09-11); vet/gofmt/grep limpos; revisor APROVADA na rodada 2 (rodada 1 ESCALAR → D-02 emendado com guarda de negação); calibração: AdMake/Ansvar/Ahrefs remotos, 1Password/Airtable/Magic/Apify locais

  Cruza para `cmd/patchbay` de propósito, num único teste: a asserção `value="oauth"` +
  `checked` que T-03 suspendeu volta aqui, com o falso de teste servindo um detalhe oficial
  remoto com OAuth (a fixture `-remoto.html`), fechando o contrato com `internal/upstream`.

  D-02, com a regra escrita lá (região lida, sinal de remoto, forma da URL, precedência).
  Fixtures novas, baixadas com o User-Agent de navegador e 2 s entre elas:
  `curl -sS -L -A "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36" -o internal/biblioteca/testdata/mcpservers-oficial-remoto.html https://mcpservers.org/pt-BR/servers/admake-ai-mcp`
  (URL na descrição) e o mesmo para `ansvar-systems/ansvar-gateway` em
  `-remoto-tabela.html` (endpoint na tabela do README). Página acima de 120 kB pode ser
  cortada a partir do primeiro `href="/pt-BR/servers/` — é exatamente onde a regra para de
  ler. O caso negativo de CA-06 usa a fixture do Anki, que já existe e cuja única URL é o
  repositório do fornecedor; o terceiro caso (emenda de D-02 após ESCALAR) usa a fixture do
  Apify, `mcpservers-oficial-sem-comando.html`, cujo único endpoint é declarado removido na
  mesma frase → stdio, sem URL, pela guarda de negação. `Autenticacao` recebe `AutOAuth` só quando a região lida cita
  OAuth; `PedeCredencial` acompanha.

- [x] T-07 Preservar o catálogo quando o índice cai ou muda de marcação
  _Requisitos: RQ-01, CA-03, CA-04_
  Arquivos: `internal/biblioteca/sincronizador_test.go`, `internal/biblioteca/curadoria.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestIndiceForaDoArPreservaOCatalogo|TestIndiceSemServidorNaoEsvaziaOCatalogo' -count=1`
  Tier: sonnet
  Evidência: 2 testes PASS (403, 500, índice sem links) e suíte ok (2026-09-11); vet e gofmt limpos; revisor APROVADA; código de produção intocado, comportamento já existia

  Dois testes sobre o mesmo comando: índice em 403 e em 500 → catálogo anterior intacto,
  `biblioteca_sincronizacao.erro` e `tentada_em` gravados; índice em 200 sem nenhum
  `href=".../servers/"` → `ErrFormatoDaOrigem` de `SlugsOficiais` (`curadoria.go:391-393`) e
  `Substituir` nunca chamado. Se o 403 não estiver coberto por `buscar`
  (`curadoria.go:158-167`), o ajuste cabe aqui.

- [x] T-08 Provar que a varredura só fala com a lista oficial
  _Requisitos: RQ-01, CA-01_
  Arquivos: `internal/biblioteca/sincronizador_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestVarreduraSoVaiAoOficial' -count=1`
  Tier: sonnet
  Evidência: teste PASS com índice paginado em 2 páginas, lista branca de caminhos e itens conferidos por nome; suíte ok (2026-09-11); vet e gofmt limpos; revisor APROVADA

  Servidor de teste que grava todo caminho pedido e responde índice + detalhes: ao fim da
  varredura, um item por link do índice no repositório, e nenhum caminho registrado sob
  `/v0/servers` ou `/remote-mcp-servers` — a asserção é sobre a lista de caminhos vistos, não
  sobre o texto do código.

- [x] T-17 Falhar a varredura quando mais de 10% dos detalhes estão indisponíveis
  _Requisitos: RQ-02, CA-12_
  Arquivos: `internal/biblioteca/sincronizador.go`, `internal/biblioteca/sincronizador_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestPisoDeDetalhesIndisponiveis' -count=1`
  Tier: sonnet
  reaberta por drift rodada 2 (2026-09-11): CA-12 emendado diz que 404 e página sem título contam no piso; o teste só emite 500 — acrescentar subcasos 404 e 200 sem título.
  Evidência (rodada 2): 3 subcasos novos (11 em 404 falha; 11 sem título falha; 5 sem título passa com 95 gravados), 7 PASS; sincronizador.go intocado; revisor APROVADA (2026-09-11)
  Evidência: 4 subcasos PASS (N=20: 10% passa, 15% falha; N=100: 5% passa, 11% falha), suítes ok (2026-09-11); vet e gofmt limpos; revisor APROVADA; nota: página sem título conta como indisponível no piso

  Numerada fora de ordem porque nasceu da análise; entra aqui por dependência (depois de
  T-01 e T-08, no mesmo arquivo). Em `varrer` (ex-`varrerOficiais`; `sincronizador.go:405-412`, que hoje
  só incrementa `indisponiveis` e conclui), um detalhe em 5xx continua sendo contado e
  pulado; ao fim do índice, `indisponiveis > total/10` devolve erro embrulhando
  `ErrOrigemIndisponivel` com a contagem, e `Substituir` não é chamado. O teste sobe um
  servidor com N detalhes e faz 11% deles responderem 500 (falha, catálogo intacto) e 5%
  (sucesso, demais itens gravados).

- [x] T-18 Reconhecer a tabela de endpoints da página de detalhe
  _Requisitos: RQ-03, CA-13_
  Arquivos: `internal/biblioteca/biblioteca.go` (campo `Endpoints`), `internal/biblioteca/curadoria.go`, `internal/biblioteca/curadoria_test.go`, `internal/biblioteca/testdata/mcpservers-oficial-tabela-endpoints.html` (novo, página real do Cloudflare), `internal/biblioteca/testdata/mcpservers-oficial-tabela-repositorios.html` (novo, sintético)
  Verificação: `go test ./internal/biblioteca/ -run 'TestTabelaDeEndpointsViraRemoto|TestTabelaSoDeRepositoriosNaoViraConexao|TestOficialRemotoViraItemHTTP|TestURLDeSiteNaoViraConexao' -count=1`
  Tier: opus
  Evidência: 4 testes PASS e suítes ok (2026-09-11); vet/gofmt limpos; Cloudflare → http, https://mcp.cloudflare.com/mcp, 17 endpoints; 9 páginas calibradas sem regressão; revisor APROVADA

  D-02 emenda 2 e D-07 (só o campo). Dentro da região já recortada por `regiaoDoServidor`,
  localizar `<table>` cujo `<thead>` tenha coluna com texto contendo `URL`/`endpoint`
  (case-insensitive), ler as `<td>` dessa coluna (índice da coluna, não regex solta), aceitar
  URLs `https` terminadas em `/mcp`, `/mcp/`, `/sse`, `/sse/` com a guarda de negação por célula;
  host `github.com`/`gitlab.com`/`bitbucket.org` nunca é endpoint (regra de D-02, vale em
  qualquer coluna); colunas de nome/descrição não contam. Tabela válida tem precedência sobre
  rótulo e descrição. `URL` = linha com `recomendado`/`recommended` no texto, senão a primeira;
  `Endpoints` = todos os da tabela (um ou mais), na ordem — invariante: `URL` está em `Endpoints`.
  Fixture sintética de repositórios: tabela com coluna de nome `github.com/x/mcp` e coluna URL só
  com links de repositório → sem URL, sem endpoints. Não-regressão: AdMake, Ansvar, Apify e Anki
  (fixtures existentes) continuam com o mesmo resultado; 1Password, Airtable, Magic e Ahrefs são
  baixadas de novo (`curl` com User-Agent de navegador, 2 s entre elas) — as que tiverem
  `<table>` na região viram fixture (`mcpservers-oficial-local-tabela-*.html`), as demais são
  reconferidas à mão e o resultado entra no retorno. A fixture do Cloudflare é a página real
  baixada em 2026-09-11 (108 kB, `C:\Users\VITORM~1\AppData\Local\Temp\det-cloudflare.html`);
  pode ser cortada a partir do primeiro `href="/pt-BR/servers/` depois do `</h1>`, mantendo a
  tabela.

- [x] T-19 Persistir os endpoints publicados
  _Requisitos: RQ-03, CA-13_
  Arquivos: `internal/platform/store/migracoes/00016_biblioteca_endpoints.sql` (novo), `internal/biblioteca/repositorio.go`, `internal/biblioteca/repositorio_test.go`, `internal/biblioteca/semente.go`, `internal/biblioteca/semente_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestEndpointsIdaEVolta|TestSementeIdaEVolta|TestSubstituirGravaEBuscarDevolve' -count=1 && go test ./internal/platform/store/ -count=1`
  Tier: sonnet
  Evidência: 3 testes PASS, store ok, suíte de biblioteca ok (2026-09-11); vet/gofmt limpos; 00016 ADD COLUMN endpoints DEFAULT '[]'; revisor APROVADA

  D-07. Coluna `endpoints TEXT NOT NULL DEFAULT '[]'`; `colunas`/`lerLinha`/`INSERT` do
  repositório gravam e leem JSON (`[]string`, `'[]'` quando vazio); `Sincronizacao` e o resto
  não mudam. Semente: `GravarSemente`/`lerSemente` carregam o campo (a semente atual, sem o
  campo, continua decodificando — `TestSementeVersionadaEValida` segue verde). Teste novo
  `TestEndpointsIdaEVolta`: item com 3 endpoints gravado e relido igual; item sem endpoints
  relido com fatia vazia, não nula. Depende de T-18 (campo).

- [x] T-20 Deixar o admin escolher o endpoint na tela e explicar o item sem comando
  _Requisitos: RQ-04, CA-14_
  Arquivos: `internal/biblioteca/admin.templ`, `internal/biblioteca/admin_templ.go`, `internal/biblioteca/admin_http.go`, `internal/biblioteca/admin_http_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestTelaListaEndpoints|TestTelaExplicaItemSemComandoNemURL|TestAdicionar|TestTela' -count=1`
  Tier: sonnet
  Evidência: 10 testes PASS, suítes de biblioteca e cmd/patchbay ok (2026-09-11); vet/gofmt limpos; admin_templ.go sincronizado; índice inválido cai na URL do item; hrefs por templ.SafeURL; revisor APROVADA

  D-07. Cartão com `len(Endpoints) > 1`: lista cada endpoint (texto da URL) com um link
  "Adicionar" para `/admin/biblioteca/adicionar/{nome}?endpoint=<i>`; a rota lê `endpoint`,
  valida o índice e monta `rotaDeCadastro` com `url` daquele endpoint (índice inválido ou ausente:
  comportamento atual). Cartão sem comando nem URL: no lugar de "sem comando publicado", texto
  "a página deste servidor não publica comando nem endpoint reconhecível — abra a página e
  cadastre à mão", com link para `https://mcpservers.org/pt-BR/servers/<slug>` (slug = `Nome`
  sem `mcpservers.org/`) e para o `Site` quando houver; "Adicionar" continua abrindo o
  formulário só com o nome. Regerar `admin_templ.go` (`task ui:gen`). Depende de T-18 e T-19.

- [x] T-21 Regerar a semente com a regra de tabela de endpoints
  _Requisitos: RQ-05, CA-08, CA-13_
  Arquivos: `internal/biblioteca/semente.json.gz`, `changes/001-biblioteca-so-mcpservers-oficial/progresso.md` (só `## Medições`)
  Verificação: `git diff --stat d53f9a9 -- internal/biblioteca/semente.json.gz && go test ./internal/biblioteca/ -run 'TestSementeVersionadaEValida' -count=1 -v`
  Tier: sonnet

  Depois de T-18 e T-19: `task biblioteca:semente` (rede, ~30 min) para a semente embutida
  refletir a regra de tabela — sem isso, instalação nova nasce com o Cloudflare sem URL até a
  primeira varredura. Registrar em `## Medições` a linha impressa (total, kB, duração) e, do
  `t.Logf`, quantos itens têm endpoints; conferir que
  `mcpservers.org/cloudflare/mcp-server-cloudflare` sai com URL `https://mcp.cloudflare.com/mcp`
  e 17 endpoints. Sem veredito — CA-10 continua sendo do spec-verificador.
  Evidência: semente regerada pela rede (22m33s, 651 servidores, .gz 58443 bytes); Cloudflare http com 17 endpoints; 3 itens com endpoints; TestSementeVersionadaEValida PASS; revisor APROVADA por decodificação independente (2026-09-11)

- [x] T-09 Apontar a tela para a lista oficial e tirar os filtros de curadoria
  reaberta por drift (2026-09-11): RQ-01/RQ-06 exigem a falha "com a hora da tentativa"; a tela mostra só texto fixo. Renderizar `TentadaEm` e `Erro` do estado de sincronização no aviso, com teste em `TestTela*`.
  Evidência (rodada 2): aviso de falha mostra hora local `DD/MM/AAAA HH:MM:SS` de TentadaEm e o erro escapado, só quando há erro; TestTelaMostraAHoraDaUltimaTentativaQueFalhou PASS; revisor APROVADA (2026-09-11)
  _Requisitos: RQ-04, CA-07_
  Arquivos: `internal/biblioteca/admin.templ`, `internal/biblioteca/admin_templ.go`, `internal/biblioteca/admin_http.go`, `internal/biblioteca/admin_http_test.go`
  Verificação: `go test ./internal/biblioteca/ -run 'TestTela' -count=1`
  Tier: sonnet
  Evidência: TestTela ok (3 testes), suítes ok (2026-09-11); vet/gofmt limpos; grep de registry no .templ e no gerado vazio; admin_templ.go regenerado e sincronizado; revisor APROVADA

  D-05. Mudam: `resumoDaPagina` (`admin.templ:23-33`, gerado em `admin_templ.go:34`), que lê
  `Filtro.SoCurados` e deixa de fazê-lo aqui para T-10 poder apagar o campo; o subtítulo
  (`admin.templ:80`), o bloco do checkbox "Só oficiais" (`:111-125`), o aviso da primeira varredura (`:173-174`), os selos "curado" e "domínio
  verificado" e a linha de versão (`:197-205`), a linha de execução vazia do cartão (`:211`,
  que ganha rótulo de "sem comando publicado") e o rodapé (`:288-295`, que passa a apontar
  `https://mcpservers.org/pt-BR/official`); em `admin_http.go` saem a leitura de
  `q.Get("curados")` (`:66`), o `curados` de `Pagina.rota` (`:259-261`) e o texto do aviso
  "atualizando" que fala em trezentas idas ao registry (`:202-206`). Saem os testes
  `TestFiltroDeCuradosNaTela` e `TestFiltroDeCuradosSobreviveAPaginacao`; entra
  `TestTelaNaoCitaORegistryNemOsFiltros`, conferindo no HTML a ausência de "registry",
  "modelcontextprotocol.io" e `name="curados"` e a presença do rodapé novo. **`admin.templ`
  mudou: regerar com `task ui:gen` (roda `templ generate`) e commitar `admin_templ.go`** — sem
  isso o teste lê a tela antiga e falha.

- [x] T-10 Tirar do catálogo os campos que a origem única não preenche
  _Requisitos: RQ-04, CA-07_
  Arquivos: `internal/biblioteca/biblioteca.go`, `internal/biblioteca/curadoria.go`, `internal/biblioteca/curadoria_test.go` (só `Filtro{SoCurados: true}` em `:334` e `!bom.Curado` em `:389`), `internal/biblioteca/repositorio.go`, `internal/biblioteca/repositorio_test.go`, `internal/biblioteca/semente_test.go`, `internal/biblioteca/admin_http_test.go` (só o literal `Versao: "1.0.0"` em `TestTelaEscapaOQueVeioDaOrigem`, `:245-249`)
  Verificação: `go test ./internal/biblioteca/ -count=1`
  Tier: sonnet
  Evidência: suíte de biblioteca ok (2026-09-11); vet/gofmt limpos; grep dos símbolos removidos vazio; semente atual ainda decodifica; revisor APROVADA; cmd/patchbay quebrado só em biblioteca_semente.go até T-12

  D-05. Saem de `Item` os campos `Versao` e `Curado`, os métodos `Namespace()` e
  `DominioVerificado()` e o vetor `namespacesDeFoundry` (`biblioteca.go:159-160, 178-182,
  205-241`); `lerOficial` para de marcar `Curado: true` (`curadoria.go:495`); no repositório
  saem `Filtro.SoCurados`, o `curado = 1` de `filtroDe` (`:202-206`), `versao` e `curado` da
  projeção `colunas` (`:58-59`) e do `INSERT` (`:247-251`), e o `ORDER BY curado DESC`
  (`:125`) vira `ORDER BY nome`. Saem os testes `TestDominioVerificadoSeparaFornecedorDeContaDeFoundry`,
  `TestFiltroDeCuradosRecortaAListaCurta`, `TestFiltroDeCuradosSomaComABuscaPorTermo`,
  `TestCuradosVemPrimeiro` (vira "ordem por nome") e, em `semente_test.go`, as contagens de
  `i.Curado` (`:155-157, 168`). As colunas do banco **ficam**, com seus `DEFAULT`.

- [x] T-12 Tirar o recorte `-so-curados` do gerador de semente
  _Requisitos: RQ-05, CA-08_
  Arquivos: `cmd/patchbay/biblioteca_semente.go`, `cmd/patchbay/main.go` (só o texto de ajuda em `:98`), `Taskfile.yml`
  Verificação: `go vet ./cmd/patchbay/ && ! grep -rn "so-curados" Taskfile.yml cmd/patchbay/`
  Tier: haiku
  Evidência: vet ok, grep vazio, build e suíte de cmd/patchbay ok (2026-09-11); gofmt limpo; revisor APROVADA; -o e gravação por temp+rename intactos

  D-06. Vem **antes** de T-11 de propósito: T-10 tirou `Item.Curado`, e
  `biblioteca_semente.go:52,97` ainda lê `i.Curado` — `./cmd/patchbay/` não compila entre
  T-10 e esta tarefa, dependência de ordem conhecida como a de T-01 → T-03. Saem a flag
  (`biblioteca_semente.go:32-33`), o filtro por `i.Curado` (`:49-57`), a contagem de curados
  do texto impresso (`:95-102`), o `[--so-curados]` da ajuda (`main.go:98`) e o comentário da
  task que documenta `-- --so-curados` (`Taskfile.yml:70`; o `cmds` em `:74` só passa
  `{{.CLI_ARGS}}`). A task continua sendo o único caminho para regerar a semente. A menção
  em `README.md:281` sai em T-14.

- [x] T-11 Descartar o catálogo antigo na primeira subida do binário novo
  reaberta (2026-09-11, ao entrar a 00016 em T-19): o teste `TestCatalogoDoRegistryEDescartadoNaAtualizacao` encena o estado pré-00015 apagando `goose_db_version` versão 15 por número fixo; com a 00016 aplicada o goose recusa a lacuna. Encenar de verdade o esquema 14: apagar as versões >= 15 e desfazer o que elas criaram (hoje só a coluna `endpoints`, via `ALTER TABLE … DROP COLUMN`), para o goose reaplicar 15 e 16 no boot; deixar comentário de que migração nova depois da 00016 precisa entrar nessa lista.
  Evidência (rodada 2): teste volta o banco ao esquema 14 (versões >= 15 apagadas, coluna endpoints derrubada), goose reaplica 15 e 16 no boot; teste PASS e cmd/patchbay ok; revisor APROVADA (2026-09-11)
  _Requisitos: RQ-06, CA-09_
  Arquivos: `internal/platform/store/migracoes/00015_biblioteca_so_oficiais.sql` (novo), `cmd/patchbay/aplicacao.go` (só a opção nova), `cmd/patchbay/biblioteca_migracao_test.go` (novo)
  Verificação: `go test ./cmd/patchbay/ -run 'TestCatalogoDoRegistryEDescartadoNaAtualizacao' -count=1`
  Tier: sonnet
  Evidência: teste PASS, build ok, suítes de cmd/patchbay e platform ok (2026-09-11); vet/gofmt limpos; revisor APROVADA com mutação (sem DELETE → itens do registry sobrevivem; sem UPDATE → catálogo vazio)

  D-04. A migração faz `DELETE FROM biblioteca_servidor`, zera `concluida_em`, `tentada_em`,
  `servidores` e `erro` em `biblioteca_sincronizacao` e derruba o índice
  `biblioteca_servidor_curado` (`00014_biblioteca_curadoria.sql:43`); `Down` é no-op
  comentado. Mecanismo do teste (D-04, "Como o teste prova"): (1) opção de teste nova
  `ComSementeDaBiblioteca(itens []biblioteca.Item, geradoEm time.Time) OpcaoApp` em
  `aplicacao.go`, ao lado de `SemSementeDaBiblioteca` (`:131`), que troca a semente
  embutida pela injetada — só teste usa, pelo mesmo motivo documentado ali; (2) o teste abre
  o banco em arquivo temporário, roda `store.Migrar` inteiro, insere linhas em formato de
  registry (`com.notion/mcp`, `io.github.fulano/x`) e um upstream cadastrado, e encena o
  estado anterior à 00015 com `DELETE FROM goose_db_version WHERE version_id = 15`; (3) sobe
  o app com a semente injetada (dois itens `mcpservers.org/…`) e `ComCuradoriaDaBiblioteca`
  apontando para um servidor mudo, e confere: nenhum nome fora de `mcpservers.org/`, o
  catálogo igual à semente injetada antes de qualquer varredura, e o upstream ainda legível.
  Cruza dois diretórios de propósito — a migração só é observável pelo boot do app.

- [x] T-13 Regerar a semente embutida só com a lista oficial
  reaberta por drift (2026-09-11): CA-08 exige total > 0 e o teste faz `t.Skip` com semente vazia — trocar por `t.Fatal`.
  Evidência (rodada 2): `t.Skip` → `t.Fatal`; TestSementeVersionadaEValida PASS; revisor APROVADA (2026-09-11)
  _Requisitos: RQ-05, CA-08_
  Arquivos: `internal/biblioteca/semente.json.gz`, `internal/biblioteca/semente_test.go`
  Verificação: `git diff --stat internal/biblioteca/semente.json.gz && go test ./internal/biblioteca/ -run 'TestSementeVersionadaEValida' -count=1 -v`
  Tier: sonnet
  Evidência: semente.json.gz 23871→57805 bytes; TestSementeVersionadaEValida PASS com `651 servidores, gerada em 2026-09-11, 348 sem comando, 0 sem descrição, 0 sem site` (2026-09-11); suíte ok; revisor APROVADA com decodificação independente (651 itens, 0 fora do prefixo, 90 remotos, 56 OAuth)

  D-06. **Exige rede e leva ~25-35 min**: `task biblioteca:semente` varre o site de verdade,
  com 2 s entre páginas. `TestSementeVersionadaEValida` (`semente_test.go:136`) passa a exigir
  prefixo `mcpservers.org/` em todo `Nome` (CA-08) e perde a asserção
  `comConexao != len(itens)` (`:165`), que RQ-02 invalidou — a contagem de itens sem comando
  vira `t.Logf`, e o mesmo `t.Logf` (`:171-172`) passa a imprimir também quantos itens têm
  `Descricao == ""` e quantos têm `Site == ""`: são os dois números que CA-10 mede e que T-16
  copia para o ledger. O `git diff --stat` tem de mostrar `semente.json.gz` alterado: teste verde
  com semente velha seria falso positivo.

- [x] T-14 [P] Descrever uma origem só no README
  reaberta por drift rodada 3 (2026-09-11): `README.md:661-662` ainda diz que a biblioteca "passou a oferecer os ~11 mil" que só existem como pacote (número do registry); reescrever sem número da origem antiga. Também: documentar a tabela de endpoints e o "Adicionar" por endpoint (T-18..T-20) na seção da biblioteca.
  Evidência (rodada 5): "~11 mil" removido; tabela de endpoints e Adicionar por endpoint documentados; grep de CA-11 vazio; revisor CORRIGIR com um achado (citação antiga em README.md:285) adjudicado pelo disjuntor → T-22 (2026-09-11)
  reaberta por drift (2026-09-11): README ainda descreve as regras da origem removida na seção "O que entra na cópia" (só grava servidor que sabe cadastrar, runtimeHint, tabela npm/pypi/nuget, transporte desconhecido não aparece) e cita "~23 kB" e "30 mil linhas"; reescrever pelas regras de D-02/D-03 e pelos números de `## Medições` (651 itens, ~56 kB).
  reaberta por drift rodada 2 (2026-09-11): bullet "O que cada servidor traz" promete URL com autenticação e credencial Bearer/OAuth da origem removida; datas/durações da medição (22-35 min, 2026-09-09) divergem de `## Medições` (29m26s, 2026-09-11).
  Evidência (rodada 4): autenticação só OAuth citado na página (:128, :169, :198-202); 29m26s/2026-09-11/~651 detalhes; CA-11 grep vazio; revisor APROVADA (2026-09-11)
  Evidência (rodadas 2-3): seção "O que entra na cópia" reescrita por D-02/D-03 (bullet Remoto como disjunção após CORRIGIR A-01), 651 servidores/~56 kB, grep ampliado vazio; revisor APROVADA (2026-09-11)
  _Requisitos: RQ-04, CA-11_
  Arquivos: `README.md`
  Verificação: `! grep -in "registry\|modelcontextprotocol\.io\|so-curados\|11 mil" README.md && grep -c "endpoint" README.md`
  Tier: haiku
  Evidência: `! grep -in "registry\|modelcontextprotocol\.io\|so-curados" README.md` → sem saída, status 0 (2026-09-11); revisor APROVADA; diff 69+/145-

  O README inteiro: a seção da biblioteca (linhas 157-365) descreve hoje duas fontes e traz a
  tabela comparativa das três origens (`README.md:183-197`), e fora dela as linhas 49
  ("registry oficial do Model Context Protocol") e 128 ("Catálogo de duas origens … filtro
  Só oficiais") também mudam. A linha 281 documenta `task biblioteca:semente -- --so-curados`,
  flag que T-12 remove: sai daqui, porque este é o único dono de `README.md`. Passa a descrever a lista oficial do mcpservers.org, o que
  cada entrada traz (nome, descrição, site, e comando ou URL quando a página publica) e o que
  ela não é. `[P]`: não compartilha arquivo com nenhuma outra tarefa e não depende de código.

- [x] T-22 Acertar a citação do cartão sem comando no README
  _Requisitos: RQ-04, CA-11_
  Arquivos: `README.md`
  Verificação: `! grep -n "sem comando publicado" README.md && ! grep -in "registry\|modelcontextprotocol\.io\|so-curados\|11 mil" README.md`
  Tier: haiku

  Nasceu do achado A-01 da 5ª rodada de T-14: `README.md:285` cita "sem comando publicado" como
  o texto que o cartão mostra hoje; o texto vigente (T-20) é "a página deste servidor não
  publica comando nem endpoint reconhecível — abra a página e cadastre à mão". Trocar a
  citação (ou descrever o comportamento sem citar), sem mexer em mais nada.
  Evidência: greps vazios, frase igual à de admin.templ, diff 2 linhas; revisor APROVADA (2026-09-11)

- [x] T-15 Fechar sem nenhuma referência ao registry no código
  reaberta por drift rodada 3 (2026-09-11): comentário de `esperaEntreCuradas` (`curadoria.go:39-44`) fecha com "~10 minutos para as 293 páginas" (lista de remotos removida); trocar a última frase por "~22 min para as ~651 páginas (29m26s medidos em 2026-09-11)", mantendo a medição datada.
  Evidência (rodada 5): comentário de esperaEntreCuradas corrigido; greps de CA-02 e de "293 páginas" vazios; gofmt e build ok; revisor APROVADA (2026-09-11)
  reaberta por drift (2026-09-11): sobraram o tipo `Resultado`/`ProximoCursor` (cursor do registry, sem uso), textos de `biblioteca_semente.go` ("varre as origens", "dois sites", "perto de uma hora") e comentários de produção com a regra antiga (`Oficial` "recusar é o comportamento", `Item` "nunca é montado", cabeçalho `BaseOficiais`, `Manter` "só os curados", `aplicacao.go` "segunda origem"/`ComOrigemDaBiblioteca`/"509 servidores", `Filtro` "recorte de origem"). Entra depois de T-09 (compartilham `admin_http.go`).
  reaberta por drift rodada 2 (2026-09-11): Taskfile.yml (desc e comentário da task) e main.go:98 ainda dizem "varrendo as origens (leva perto de uma hora)"; doc de pacote diz só "5xx" no piso; comentários com 647 servidores/~670 requisições/~22 minutos; dublês de teste chamados de "registry"; semente_test.go:181 "poucas centenas de curados".
  Evidência (rodada 4): Taskfile/ajuda do CLI para origem única e ~30 min; doc de pacote com "5xx, 404 ou 200 sem título"; 651/22 páginas/29m26s nos comentários; dublês renomeados; grep ampliado vazio; build/vet/gofmt/suítes ok; revisor APROVADA (2026-09-11)
  Evidência (rodadas 2-3): tipo Resultado apagado; textos do comando de semente e comentários reescritos para uma origem (651 na semente, ~30 min); comentário de Oficial corrigido após CORRIGIR A-01; grep ampliado vazio incl. testes; build/vet/gofmt/suítes ok; revisor APROVADA (2026-09-11)
  _Requisitos: RQ-01, CA-02_
  Arquivos: `internal/**`, `cmd/**` (só remoção de resíduo: comentário, nome de variável, texto de teste)
  Verificação: `! grep -rn --include=*.go --include=*.templ "registry\.modelcontextprotocol\.io\|remote-mcp-servers" internal/ cmd/ && ! grep -n "293 páginas" internal/biblioteca/curadoria.go`
  Tier: haiku
  Evidência: grep literal vazio e `grep -i registry` fora de testes só com goose.WithDisableGlobalRegistry (2026-09-11); build/vet/gofmt ok; suítes ok; só comentários mudaram; revisor APROVADA (despachada em sonnet)

  Última do lote de código: varre o que sobrou de T-01 a T-14 — comentário de pacote
  (`biblioteca.go:9-46`), comentários das migrações 00013/00014 não são código e ficam como
  registro histórico, mas qualquer menção viva em `.go` ou `.templ` sai. `testdata/` fica
  fora: é HTML capturado do site, cuja navegação cita as strings (CA-02). Se o
  `grep` acusar algo que não é resíduo, é sinal de tarefa anterior incompleta — volte a ela.

- [x] T-16 Medir a varredura real contra o site
  _Requisitos: RQ-01, RQ-06, CA-10_
  Arquivos: `changes/001-biblioteca-so-mcpservers-oficial/progresso.md` (só a seção `## Medições`, com os números brutos)
  Verificação: `task biblioteca:semente -- -o /tmp/medicao-nova.json.gz`
  Tier: sonnet
  Evidência: medição reaproveitada da varredura real de T-13 no mesmo dia (2026-09-11), como a tarefa prevê: 29m26s, 651 servidores contra 652 no título, 0 sem descrição, 0 sem site, 0 indisponíveis — números em progresso.md `## Medições`, conferidos pelo revisor de T-13; sem veredito (CA-10 é do spec-verificador)

  **Esta tarefa mede, não julga.** Ela grava em `progresso.md`, seção `## Medições`, a saída
  literal dos comandos e os números (durações, total, número do título da página, itens sem
  descrição, itens sem site). Quem confere esses números contra os limites de CA-10 e escreve
  `verificacao.md` é o `spec-verificador`, em `spec-verificar` — nunca quem implementou.

  **Manual, exige rede, ~35 min de relógio.** Procedimento: (1) na branch, rodar o comando
  acima e anotar a linha impressa — `semente gravada em …: N servidores, K kB, em XXmYYs`;
  (2) anotar também a `duracao` do log `biblioteca sincronizada` de uma subida normal, se
  houver. Registrar em `progresso.md`, seção `## Medições`: a duração, o total de servidores
  contra o número do título de `https://mcpservers.org/pt-BR/official` naquele dia, e quantos
  itens saíram sem descrição ou sem site — este último número também sai do
  `TestSementeVersionadaEValida -v` de T-13, sobre os mesmos itens. A comparação com a
  varredura antiga foi dispensada pelo dono (Esclarecimentos, 2026-09-11). Se a regeneração
  da semente de T-13 tiver acabado de rodar no mesmo dia, a linha impressa por ela vale como
  a medição de (1): não é preciso varrer o site duas vezes.

## Cobertura

| Requisito | Tarefas |
|---|---|
| RQ-01 | T-01, T-02, T-03, T-07, T-08, T-15, T-16 |
| RQ-02 | T-04, T-05, T-17 |
| RQ-03 | T-06, T-18, T-19 |
| RQ-04 | T-09, T-10, T-14, T-20, T-22 |
| RQ-05 | T-12, T-13, T-21 |
| RQ-06 | T-11, T-16 |
| CA-01 | T-01, T-03, T-08 |
| CA-02 | T-01, T-02, T-03, T-15 |
| CA-03 | T-07 |
| CA-04 | T-07 |
| CA-05 | T-04, T-05 |
| CA-06 | T-06 |
| CA-07 | T-09, T-10 |
| CA-08 | T-12, T-13, T-21 |
| CA-09 | T-11 |
| CA-10 | T-16 |
| CA-11 | T-14, T-22 |
| CA-12 | T-17 |
| CA-13 | T-18, T-19, T-21 |
| CA-14 | T-20 |
