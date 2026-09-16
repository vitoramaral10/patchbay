# Verificação — 001-biblioteca-so-mcpservers-oficial

Verificado por: subagente `spec-verificador` (sonnet), em 2026-09-11 — rodada 4, completa, com as 22 tarefas fechadas (inclusive a emenda do Cloudflare: T-18..T-22). Rodadas anteriores do mesmo dia: 1 (12/12 PASSA, antes da análise de drift), 2 (13/13, após correções pós-drift), 3 (13/13, antes da emenda); todas substituídas por esta porque as correções tocaram código de várias tarefas.
Diff verificado: working tree contra a base — `git diff d53f9a9 -- internal/ cmd/ README.md Taskfile.yml` mais `git status --porcelain` (trabalho não commitado, tudo no índice; branch `feature/biblioteca-so-mcpservers-oficial`)

| CA | Comando | Saída resumida | Veredito | Motivo |
|---|---|---|---|---|
| CA-01 | `go test ./internal/biblioteca/ -run 'TestVarreduraSoVaiAoOficial' -count=1` | PASS; teste checa lista branca de prefixos pedidos (`/pt-BR/official`, `/pt-BR/servers/`) e falha se qualquer caminho fora disso for pedido, mais 1 item por link do índice | PASSA | — |
| CA-02 | `grep -rn --include=*.go --include=*.templ -i "registry.modelcontextprotocol.io\|remote-mcp-servers" internal/ cmd/` | zero ocorrências | PASSA | — |
| CA-03 | `go test ./internal/biblioteca/ -run 'TestIndiceForaDoArPreservaOCatalogo' -count=1 -v` | PASS nos casos 403 e 500; confirma catálogo intacto (total=3) e `erro`/`tentada_em` gravados | PASSA | — |
| CA-04 | `go test ./internal/biblioteca/ -run 'TestIndiceSemServidorNaoEsvaziaOCatalogo' -count=1 -v` | PASS; índice 200 sem links produz `ErrFormatoDaOrigem` e catálogo anterior (total=3) sobrevive | PASSA | — |
| CA-05 | `go test ./internal/biblioteca/ -run 'TestOficiaisLeemPaginaDeVerdade|TestOficialSemComandoEntraComSite|TestSnippetComMarcadorDeExemploNaoViraCadastro' -count=1 -v` | PASS; usa fixtures `mcpservers-oficial-comando.html` (stdio com comando+args) e `mcpservers-oficial-sem-comando.html` (sem comando, com nome/descrição/site) | PASSA | — |
| CA-06 | `go test ./internal/biblioteca/ -run 'TestOficialRemotoViraItemHTTP|TestURLDeSiteNaoViraConexao' -count=1 -v` | PASS; cobre URL http de MCP → item http; site-só → sem URL; Apify com endpoint declarado removido → stdio sem URL | PASSA | — |
| CA-07 | `go test ./internal/biblioteca/ -run 'TestTela' -count=1 -v` | PASS; `TestTelaNaoCitaORegistryNemOsFiltros` confere ausência de "registry"/"modelcontextprotocol.io" e dos filtros, e link de rodapé | PASSA | — |
| CA-08 | `go test ./internal/biblioteca/ -run 'TestSementeVersionadaEValida' -count=1 -v` | PASS; `651 servidores, gerada em 2026-09-11`; teste valida `Nome` com prefixo `mcpservers.org/` e total > 0 (`t.Fatal` com semente vazia) | PASSA | — |
| CA-09 | `go test ./cmd/patchbay/ -run 'TestCatalogoDoRegistryEDescartadoNaAtualizacao' -count=1 -v` | PASS; monta banco com itens formato registry + upstream cadastrado, volta o esquema ao estado 14, sobe binário, confirma catálogo virou semente nova e upstream íntegro | PASSA | — |
| CA-10 | `go test ./internal/biblioteca/ -run 'TestSementeVersionadaEValida' -count=1 -v` + decodificação independente do `.gz` real + `progresso.md` `## Medições` | segunda varredura: 22m33s, 651 itens (652 anunciado, diferença 0,15%, dentro de ±2%), 0 sem descrição, 0 sem site; decodificação própria confirma 651 itens, mesmos zeros | PASSA | — |
| CA-11 | `grep -in "registry\|modelcontextprotocol\.io" README.md` | zero ocorrências | PASSA | — |
| CA-12 | `go test ./internal/biblioteca/ -run 'TestPisoDeDetalhesIndisponiveis' -count=1 -v` | PASS; caso `100_acima_do_piso_falha` (11/100) falha e preserva catálogo; caso `100_abaixo_do_piso_nao_falha` (5/100) conclui e grava os demais; 404 e página sem título contam igual a 500 (7 subcasos) | PASSA | — |
| CA-13 | `go test ./internal/biblioteca/ -run 'TestTabelaDeEndpointsViraRemoto|TestTabelaSoDeRepositoriosNaoViraConexao' -count=1 -v` + decodificação da semente real | PASS; fixture Cloudflare real (17 endpoints) → http, URL=`https://mcp.cloudflare.com/mcp` (linha recomendada, primeiro endpoint), 17 endpoints na ordem; tabela sintética com nome `github.com/x/mcp` e só links de repositório → sem URL, sem endpoints. Semente embutida confirma `cloudflare/mcp-server-cloudflare` com 17 endpoints, mesma URL | PASSA | — |
| CA-14 | `go test ./internal/biblioteca/ -run 'TestTelaListaEndpoints|TestTelaExplicaItemSemComandoNemURL' -count=1 -v` | PASS; item com 2 endpoints → 2 links "Adicionar", cada um com `url=` do endpoint certo; item sem comando/URL → texto explicativo, link para página do mcpservers.org e para o site | PASSA | — |
| RQ-01/06 hora | `go test ./internal/biblioteca/ -run 'TestTelaMostraAHoraDaUltimaTentativaQueFalhou' -count=1 -v` | PASS; tela mostra hora local da `tentada_em` e o texto do erro só quando há falha registrada | PASSA | — |

## Falhas

Nenhuma. Todos os 14 CAs e a linha extra passaram com comando executado e comportamento do código conferido contra o texto do critério (não só "teste verde").

## Cobertura

- Critérios na proposta: 14 (CA-01 a CA-14) + 1 linha extra (hora da tentativa falhada).
- Verificados: 15/15 — **15 PASSA, 0 FALHA, 0 NÃO VERIFICÁVEL**.
- Requisitos sem CA correspondente: nenhum — RQ-01 (CA-01/02/03/04/10), RQ-02 (CA-05/12), RQ-03 (CA-06/13), RQ-04 (CA-07/11/14), RQ-05 (CA-08), RQ-06 (CA-09/10 e a linha extra).

Gates gerais:
- `go build ./...` — limpo.
- `go vet ./...` — limpo.
- `go test ./... -count=1` — todos os pacotes OK.
- `gofmt -l internal/ cmd/` — vazio.
- `go mod tidy` + `git status --porcelain go.mod go.sum` — sem mudança.
- `golangci-lint run ./...` — 43 issues no repo, nenhum nos arquivos desta mudança exceto `internal/biblioteca/curadoria.go:534` (`QF1001`, De Morgan, staticcheck): estilo, não bloqueante.
- `-race` — não rodado (Windows sem suporte nesta máquina).

## Veredito

**aprovado para arquivamento**

## Achados fora da tabela de CA

- `internal/biblioteca/curadoria.go:534` — sugestão de estilo do staticcheck (De Morgan) em `conexaoRemota`. Não é bug nem diverge de nenhum CA.
- Análise de drift spec × código (subagente `spec-analista`), quatro rodadas em 2026-09-11: rodada 1 achou 2 ALTO / 4 MEDIO / 2 BAIXO, rodada 2 achou 2 MEDIO / 6 BAIXO, rodada 3 achou 3 MEDIO / 1 BAIXO — todos tratados nas rodadas de T-09, T-11, T-13, T-14, T-15, T-17 e nas emendas de RQ-02/CA-12; a rodada 4 (fechamento, após a emenda do Cloudflare) está registrada em `progresso.md`, seção `## Decisões do controlador`.
- Medições reais (`progresso.md`, `## Medições`): primeira varredura 29m26s / 651 itens (antes da regra de tabela); segunda varredura 22m33s / 651 itens, 3 itens com endpoints (Cloudflare 17), 91 remotos, 57 com OAuth, 0 sem descrição, 0 sem site.
