# Progresso — 001-biblioteca-so-mcpservers-oficial

Mudança: `changes/001-biblioteca-so-mcpservers-oficial/`
Iniciado em: 2026-09-11

<!-- Ledger da execução (skill spec-implementar). Uma linha por rodada, logo depois de ler o retorno. -->

## T-01 Varrer só a lista oficial, sem passar pelo registry

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | opus | PRONTO (vermelho: TestVarreduraSoVaiAoOficial falha pedindo /v0/servers; verde: go test ./internal/biblioteca/ ok, vet e gofmt limpos) | APROVADA | concluída |

## T-02 Apagar o cliente da lista de remotos curados

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: grep acha remote-mcp-servers em curadoria.go e curadoria_test.go; verde: suíte ok + grep vazio, vet e gofmt limpos) | APROVADA | concluída |

## T-03 Subir o painel com a origem única

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: cmd/patchbay não compila, undefined NovaOrigem; verde: go test ./cmd/patchbay/ ok, build/vet/gofmt/grep limpos) | APROVADA | concluída |

## T-04 Aceitar servidor oficial sem comando, com site preenchido

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: 3 testes falham com ErrFormatoDaOrigem "sem comando aproveitável"; verde: os 3 passam, suíte ok, vet e gofmt limpos; Site do Apify = github.com/apify/apify-mcp-server) | APROVADA | concluída |

## T-05 Abrir o formulário sem comando quando o item não tem comando

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: TestAdicionarSemComandoAbreFormularioVazio falha "a query leva comando"; verde: 4 TestAdicionar PASS, suíte ok, vet e gofmt limpos) | APROVADA | concluída |

## T-06 Cadastrar como remoto o oficial que publica URL de MCP

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | opus | ESCALAR (spec: D-02 cadastra `https://mcp.apify.com/sse`, endpoint que a página do Apify declara removido; testes da tarefa verdes, `TestOficialSemComandoEntraComSite` de T-04 vermelho) | — | devolvida para /spec-desenhar; usuário escolheu guarda de negação (D-02 emendado, RQ-03 e CA-06 estendidos) |
| 2 | opus | PRONTO (vermelho: subcaso Apify Transporte=sse; verde: suíte de biblioteca e cmd/patchbay ok, vet/gofmt/grep limpos; 3 remotos com mesmas URLs, 4 locais sem URL) | APROVADA | concluída |

## T-07 Preservar o catálogo quando o índice cai ou muda de marcação

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (testes novos só; comportamento já existia em `buscar`; primeira escrita falhou por precisão de segundo em `tentada_em`, asserção truncada; verde: 2 testes PASS, suíte ok, vet e gofmt limpos; curadoria.go intocado) | APROVADA | concluída |

## T-08 Provar que a varredura só fala com a lista oficial

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (teste já existia de T-01; faltava provar paginação: dublê paginado + contador de idas ao índice; vermelho controlado "idas = 1, quer 2", verde depois; suíte ok, vet/gofmt limpos) | APROVADA | concluída |

## T-17 Falhar a varredura quando mais de 10% dos detalhes estão indisponíveis
| 2 | sonnet | PRONTO (servirOficiaisComFalhas parametrizado por modoDeFalha; 3 subcasos novos: 11 em 404 falha, 11 sem título falha, 5 sem título passa; vermelho controlado invertendo expectativa; 7 subcasos PASS, suíte ok, gofmt limpo) | APROVADA | concluída |

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: 2 subcasos "acima do piso" com erro nil; verde: 4 subcasos PASS N=20/100, suítes de biblioteca e cmd/patchbay ok, vet/gofmt limpos; regra `indisponiveis*10 > total`) | APROVADA (nota: ErrFormatoDaOrigem de detalhe conta no piso) | concluída |

## T-09 Apontar a tela para a lista oficial e tirar os filtros de curadoria

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: 6 asserções do teste novo falham; verde: TestTela ok, suítes ok, vet/gofmt limpos, grep de registry no templ vazio; só admin_templ.go regenerado) | APROVADA | concluída |
| 2 | sonnet | PRONTO (horaDaTentativa em admin.templ, aviso mostra hora local DD/MM/AAAA HH:MM:SS e o erro; vermelho: 2 asserções do teste novo; verde: TestTela 4 PASS, suítes ok, vet/gofmt/grep limpos; admin_templ.go regenerado) | APROVADA (hora só quando há erro; erro escapado) | concluída |

## T-10 Tirar do catálogo os campos que a origem única não preenche

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: pacote não compila após tirar Versao/Curado do struct; verde: suíte de biblioteca ok, vet/gofmt limpos, grep dos símbolos vazio; cmd/patchbay sem compilar até T-12, declarado) | APROVADA | concluída |

## T-12 Tirar o recorte `-so-curados` do gerador de semente

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | haiku | PRONTO (vermelho: vet falha com i.Curado undefined e grep acha so-curados em 3 lugares; verde: vet ok, grep vazio, build e suíte de cmd/patchbay ok, gofmt limpo) | APROVADA | concluída |

## T-11 Descartar o catálogo antigo na primeira subida do binário novo

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: itens do registry sobrevivem ao boot; verde: teste de atualização PASS, build ok, suítes de cmd/patchbay e platform ok, vet/gofmt limpos; migração 00015 com DROP INDEX IF EXISTS; opção ComSementeDaBiblioteca) | APROVADA (mutação confirmou as duas metades da 00015) | concluída |
| 2 | sonnet | PRONTO (teste apaga versões >= 15 e derruba a coluna endpoints para encenar o esquema 14; lista comentada de "o que desfazer" por migração; vermelho: goose recusa lacuna; verde: teste PASS e cmd/patchbay ok, gofmt limpo) | APROVADA (nota: não reconsulta o schema após o boot) | concluída |

## T-15 Fechar sem nenhuma referência ao registry no código
| 4 | opus (rodada 4: tier acima) | PRONTO (Taskfile desc/comentário e ajuda do CLI para origem única e ~30 min; doc de pacote "5xx, 404 ou 200 sem título"; 651/22 páginas/29m26s nos comentários; dublês renomeados; grep ampliado vazio; build/vet/gofmt/suítes ok) | APROVADA | concluída |
| 5 | sonnet (rodada 5, última; tier acima do haiku da tarefa) | PRONTO (frase final do comentário de esperaEntreCuradas → "~22 min para as ~651 páginas (29m26s medidos em 2026-09-11)"; greps vazios; gofmt e build ok) | APROVADA | concluída |

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet (tarefa dizia haiku; sessão subiu o tier: doc de pacote é prosa de desenho) | PRONTO (vermelho: 1 ocorrência literal + 20 menções a registry fora de testes; verde: grep literal vazio, só `goose.WithDisableGlobalRegistry` resta e não é resíduo; build/vet/gofmt ok; cmd/patchbay ok; biblioteca falha só em TestSementeVersionadaEValida por semente antiga, escopo de T-13) | APROVADA | concluída |
| 2 | sonnet | PRONTO (tipo Resultado apagado; textos do comando de semente para uma origem e ~30 min; comentários de Item/Oficial/caminhoOficiais/Manter/Filtro/Pagina.Filtro/aplicacao.go reescritos, 651 servidores; grep ampliado vazio incl. testes; build/vet/gofmt/suítes ok) | A-01 curadoria.go:283-284 — comentário de `Oficial` diz que falha de busca devolve ErrFormatoDaOrigem; `buscar` devolve ErrOrigemIndisponivel/ErrNaoEncontrado/ErrTaxaExcedida | corrigir — rodada 3, mesmo tier |
| 3 | sonnet | PRONTO (comentário de Oficial separa erro de buscar de ErrFormatoDaOrigem; gofmt/build/suíte ok) | APROVADA | concluída |

## T-13 Regerar a semente embutida só com a lista oficial

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (vermelho: 86 nomes fora de mcpservers.org/ na semente antiga; verde: semente regerada pela rede em 29m26s, 651 servidores, .gz 23871→57805 bytes, TestSementeVersionadaEValida PASS, suíte ok, gofmt limpo) | APROVADA (decodificação independente bate com o ledger) | concluída |
| 2 | sonnet | PRONTO (t.Skip → t.Fatal em semente_test.go; vermelho controlado com teste temporário removido; verde: TestSementeVersionadaEValida PASS) | APROVADA | concluída |

## T-16 Medir a varredura real contra o site

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sessão (reaproveita a varredura real de T-13, mesmo dia, como a tarefa prevê) | PRONTO (números em `## Medições`; sem veredito — quem julga contra CA-10 é o spec-verificador) | conferidos pelo revisor de T-13: 651 itens, 0 fora do prefixo, 348 sem comando nem URL, 90 remotos, 56 OAuth | concluída |

## Medições

Varredura real de `https://mcpservers.org/pt-BR/official`, 2026-09-11, pela task `biblioteca:semente` (binário da branch `feature/biblioteca-so-mcpservers-oficial`), gravada em `internal/biblioteca/semente.json.gz`:

| Medida | Valor | Fonte |
|---|---|---|
| Duração da varredura | 29m26s | linha impressa pelo gerador: `semente gravada em internal/biblioteca/semente.json.gz: 651 servidores, 56 kB, em 29m26s` |
| Total de servidores gravados | 651 | mesma linha; log da varredura: `oficiais lidos aproveitados=651 sem_comando=438 indisponiveis=0 total=651` |
| Número anunciado no título da página naquele dia | 652 (medido de manhã em 2026-09-11) | `estudo.md`, "Fatos — a página alvo" |
| Itens sem descrição | 0 | `TestSementeVersionadaEValida -v`: `651 servidores, gerada em 2026-09-11, 348 sem comando, 0 sem descrição, 0 sem site` |
| Itens sem site | 0 | idem |
| Itens sem comando (informativo) | 348 sem comando nem URL pelo teste; 438 com `Comando == ""` pelo log da varredura (a diferença são os remotos, que têm URL e não comando) | idem + log |
| Detalhes indisponíveis | 0 | log da varredura |
| Remotos (`URL != ""`) / com OAuth | 90 / 56 | decodificação independente pelo revisor de T-13 |
| Duração do log `biblioteca sincronizada` de uma subida normal | não medida nesta rodada (a task gera a semente sem subir o app) | — |

A comparação com a varredura antiga foi dispensada pelo dono (proposal.md, Esclarecimentos).

### Segunda varredura (após T-18/T-19, 2026-09-11)

Regeração da semente pela T-21, mesma origem, já com a regra de tabela de endpoints (T-18/T-19) aplicada, pela task `biblioteca:semente`:

| Medida | Valor | Fonte |
|---|---|---|
| Duração da varredura | 22m33s | linha impressa pelo gerador: `semente gravada em internal/biblioteca/semente.json.gz: 651 servidores, 57 kB, em 22m33s` |
| Total de servidores gravados | 651 | mesma linha; log da varredura: `oficiais lidos aproveitados=651 sem_comando=438 indisponiveis=0 total=651` |
| Tamanho do `.gz` | 57 kB | mesma linha |
| Itens sem descrição | 0 | `TestSementeVersionadaEValida -v`: `651 servidores, gerada em 2026-09-11, 347 sem comando, 0 sem descrição, 0 sem site` |
| Itens sem site | 0 | idem |
| Itens com tabela de endpoints (`len(Endpoints) > 0`) | 3 | decodificação da semente (gzip+JSON): `cloudflare/mcp-server-cloudflare` (17), `htag-analytics/htag-mcp` (3), `planwright/planwright` (1) |
| Remotos (`URL != ""`) | 91 | decodificação da semente |
| Com OAuth | 57 | decodificação da semente |
| Item `mcpservers.org/cloudflare/mcp-server-cloudflare` | transporte `http`, URL `https://mcp.cloudflare.com/mcp`, 17 endpoints; 3 primeiros: `https://mcp.cloudflare.com/mcp`, `https://docs.mcp.cloudflare.com/mcp`, `https://bindings.mcp.cloudflare.com/mcp` | decodificação da semente |

## T-18 Reconhecer a tabela de endpoints da página de detalhe

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | opus | PRONTO (endpointsDaTabela antes de conexaoRemota; coluna por índice do thead; guarda de negação por célula; guarda de repositório; linha "recomendado"; vermelho em 3 estágios + mutação; verde: 4 testes PASS, suítes ok, vet/gofmt limpos; calibração: Cloudflare http 17 endpoints, AdMake/Ansvar/Ahrefs remotos, Apify/Anki/1Password/Airtable/Magic locais; fixtures: Cloudflare, sintética, 1Password, Magic) | APROVADA | concluída |

## T-19 Persistir os endpoints publicados

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (00016 ADD COLUMN endpoints DEFAULT '[]'; repositório grava/lê JSON, fatia nunca nil; semente já serializa o campo sem mudança; vermelho: Endpoints nil na ida e volta; verde: 3 testes + store ok, vet/gofmt limpos; cmd/patchbay falha em TestCatalogoDoRegistryEDescartadoNaAtualizacao por versão fixa 15 no encenamento — escopo de T-11) | APROVADA | concluída |

## T-20 Deixar o admin escolher o endpoint na tela e explicar o item sem comando

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (cartão lista endpoints com "Adicionar" por índice quando len > 1; rota lê ?endpoint=<i> e troca a URL; item sem comando nem URL ganha texto explicativo + link para a página do servidor; vermelho: 3 testes; verde: 10 testes PASS, suíte ok, vet/gofmt limpos, só admin_templ.go regenerado) | APROVADA | concluída |

## T-14 Descrever uma origem só no README
| 4 | opus (rodada 4: tier acima) | PRONTO (bullets de autenticação em :128, :169 e :198-202 só com OAuth quando a página cita; durações 29m26s/2026-09-11 e ~651 detalhes; faixa 20-35 min marcada como estimativa da tela; CA-11 grep vazio; diff 13+/11-) | APROVADA | concluída |
| 5 | sonnet (rodada 5, última; tier acima do haiku da tarefa) | PRONTO ("~11 mil" → "centenas de servidores que só existem como pacote"; bullet da tabela de endpoints (D-02 emenda 2); parágrafo do Adicionar por endpoint e do cartão sem comando (D-07); meia frase na tabela do admin; grep de CA-11 vazio, "endpoint" 66→73; diff 20+/2-) | A-01 README.md:285 — cita "sem comando publicado" como texto atual do cartão; o texto vigente (T-20) é outro | 5ª rodada: adjudicado — A-01 vira T-22 (ver Decisões do controlador); T-14 concluída no restante |

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | haiku | PRONTO (grep vermelho: 14 ocorrências → verde: vazio; diff 69+/145-) | APROVADA | concluída |
| 2 | sonnet | PRONTO (seção "O que entra na cópia" reescrita por D-02/D-03; 651 servidores, ~56 kB; tempo de busca e "30 mil linhas" removidos; grep ampliado vazio; diff 23+/19-) | A-01 README.md:284-292 — bullet "Remoto" descreve sinal E URL como conjunção; o código (`conexaoRemota`) aceita URL rotulada sem o sinal na descrição | corrigir — rodada 3, mesmo tier |
| 3 | sonnet | PRONTO (bullet "Remoto" reescrito como disjunção: rótulo em 120 caracteres OU descrição com sinal citando a URL; guarda de negação; diff 26+/19-) | APROVADA (bullet bate com conexaoRemota) | concluída |

## T-21 Regerar a semente com a regra de tabela de endpoints

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | sonnet | PRONTO (semente regerada pela rede em 22m33s: 651 servidores, .gz 23871→58443; 3 itens com endpoints (Cloudflare 17, htag 3, planwright 1); 91 remotos, 57 OAuth, 0 sem descrição, 0 sem site; Cloudflare http https://mcp.cloudflare.com/mcp; TestSementeVersionadaEValida PASS; subseção "Segunda varredura" em Medições) | APROVADA (decodificação independente bate) | concluída |

## T-22 Acertar a citação do cartão sem comando no README

| Rodada | Tier | Resultado | Achados do revisor | Decisão |
|---|---|---|---|---|
| 1 | haiku | PRONTO (bullet reescrito com o texto vigente do cartão; "sem comando publicado" sumiu do README; greps vazios; diff 2+/2-) | APROVADA | concluída |

## Decisões do controlador

- **Drift rodada 4 (fechamento, 2026-09-11): 0 CRITICO, 0 ALTO, 0 MEDIO, 1 BAIXO — `README.md:235` diz "~56 kB" e a semente regerada em T-21 tem 58.443 bytes (~57 kB).** Decisão: aceitar com nota. Por quê: nenhum CA mede o tamanho da semente e o número muda a cada regeração; abrir rodada para 1 kB não paga o custo. Custo se estiver errado: um leitor do README vê 56 em vez de 57 kB. Verificação rodada 4: 15/15 PASSA; status promovido para `verificada`.

- **T-14 / A-01 — README.md:285 cita "sem comando publicado" como texto atual do cartão.** Decisão: devolver à skill dona das tarefas como T-22 (uma linha, haiku), em vez de 6ª rodada de T-14. Por quê: o achado é real e mínimo; o disjuntor proíbe a 6ª rodada e o controlador não corrige código nem doc por conta própria. Custo se estiver errado: nenhum além de uma rodada curta; se T-22 não fechar, o README fica com uma citação errada e a verificação de CA-11 (grep) não pega.

- **T-21 despachada com T-11 (revisão) e T-20 em curso.** Decisão: paralelizar. Por quê: T-21 depende só de T-18/T-19 (fechadas), toca só `semente.json.gz` e `## Medições`, e gasta ~30 min de rede; a troca do .gz é por arquivo temporário + rename, então uma suíte que rode no meio lê a semente velha ou a nova, ambas válidas. Custo se estiver errado: um teste de T-20 que dependa da contagem da semente (não há) teria de ser rerodado.

- **T-20 despachada com T-19 ainda em revisão e T-11 rodada 2 em curso.** Decisão: paralelizar. Por quê: arquivos disjuntos (T-20: `admin.templ`, `admin_templ.go`, `admin_http.go`, `admin_http_test.go`; T-19: repositório/semente/migração; T-11: `biblioteca_migracao_test.go`), e o campo/persistência de que T-20 depende já estão na árvore. Custo se estiver errado: uma correção em T-19 que mude a forma de `Endpoints` obrigaria a rerodar a verificação de T-20.

- **T-18: guarda de repositório só no caminho da tabela.** Decisão: aceitar. Por quê: D-02 emenda 2 fala de "qualquer coluna" da tabela; no caminho de rótulo/descrição nenhum caso medido produziu URL de repositório, e estender sem teste seria código morto. Custo se estiver errado: uma página que rotule um link de github.com terminado em /mcp como "endpoint" viraria remoto com URL de repositório — corrige-se com uma linha em conexaoRemota quando aparecer.

- **Emenda pós-verificação (relato do dono sobre o Cloudflare, 2026-09-11).** Decisão do dono: emendar nesta mudança (tabela de endpoints + tela). A verificação rodada 3 (13/13 PASSA) e o drift rodada 3 (0 CRITICO/ALTO, 3 MEDIO, 1 BAIXO) ficam registrados; a promoção para `verificada` espera T-18..T-20 e a reabertura de T-14/T-15 pelos resíduos do drift 3. Custo se estiver errado: a fatia nova atrasa o fechamento; o ganho é o Cloudflare (17 endpoints) e páginas parecidas instaláveis pela tela.

- **Piso de 10% conta página sem título e 404 além de 5xx (drift BAIXO).** Decisão: manter o código e emendar RQ-02/CA-12 para "detalhe que não pôde ser lido" (decisão do dono em Esclarecimentos, 2026-09-11). Por quê: uma página de detalhe que respondeu 200 sem título é tão inútil quanto uma em 5xx para o catálogo. Custo se estiver errado: um site que mude a marcação de todos os detalhes derruba a varredura pelo piso em vez de gravar itens vazios — que é o comportamento desejado.

- **T-11 e T-13 despachadas em paralelo sem `[P]`.** Decisão: rodar juntas. Por quê: não compartilham arquivo (T-11: migração 00015, `aplicacao.go`, teste novo em `cmd/patchbay`; T-13: `semente.json.gz` e `semente_test.go`), e T-13 gasta ~30 min de rede que dominam o relógio da mudança. Custo se estiver errado: uma verificação de T-11 rodar enquanto o embed da semente troca — se acontecer, repete-se a verificação em estado parado.

