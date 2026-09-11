---
id: 001-biblioteca-so-mcpservers-oficial
tipo: estudo
data: 2026-09-11
estado: levantamento concluído; D1, D2 e D3 decididas em proposal.md, seção Esclarecimentos (2026-09-11)
---

# Estudo — a biblioteca hoje e a página `mcpservers.org/pt-BR/official`

Insumo para o `proposal.md`. Só fatos com `arquivo:linha`, medições da página alvo feitas em
2026-09-11 e o que precisa ser decidido pelo dono do projeto. Não decide nada.

## Em uma tela

| | Antes (hoje) | Depois (pedido) |
|---|---|---|
| Origens de rede | 3: registry `/v0/servers`, mcpservers.org `/remote-mcp-servers`, mcpservers.org `/official` | 1: mcpservers.org `/pt-BR/official` |
| Volume | ~29.6k (registry) + 293 remotos + ~650 oficiais, mesclados | 652 (título da página, 2026-09-11) |
| Chave do item (`Item.Nome`) | DNS invertido do registry (`com.notion/mcp`) ou `mcpservers.org/<slug>` | só `mcpservers.org/<slug>` |
| Remotos com OAuth/token | 293, vindos de `/remote-mcp-servers` | nenhum, salvo decisão em aberto (D1) |
| Comando de instalação | `packages[]` estruturado do registry; regex sobre snippet no `/official` | só o snippet do `/official`, aproveitável em ~4 de 14 (D2) |

## Fatos — o código

- **Três origens no mesmo sincronizador.** Registry em `BaseRegistry` (`internal/biblioteca/origem.go:22`), endpoint `/v0/servers` (`origem.go:50`). mcpservers.org em `BaseCuradoria = "https://mcpservers.org/pt-BR"` (`curadoria.go:34`), com a lista de remotos em `remote-mcp-servers` (`curadoria.go:87`, detalhe em `:123`) e a lista oficial em `caminhoOficiais = "official"` (`curadoria.go:352`), índice paginado por `?page=N` (`curadoria.go:397`), última página lida dos links do próprio índice (`ultimaPagina`, `curadoria.go:474`), detalhe em `servers/<slug>` (`curadoria.go:417`).
- **A mesclagem é fixa em três listas**: `mesclar(doRegistry, curados, oficiais)` (`sincronizador.go:593`). A varredura troca o catálogo inteiro numa transação (`RepositorioSQLite.Substituir`, `repositorio.go:229`); falha preserva o catálogo anterior e fica visível no admin (`RegistrarFalha`, `repositorio.go:292`).
- **Disparo e prazos**: no boot e a cada 12h (`IntervaloDeSincronizacao`, `sincronizador.go:22`; `Manter`, `:163`), sob demanda pelo botão do admin (`Disparar`, `:261`); teto de 1h por varredura (`PrazoDaVarredura`, `sincronizador.go:34`).
- **O que o `/official` entrega hoje, medido pelo próprio código** (`curadoria.go:338-350`): nenhum dos 10 amostrados declara transporte, autenticação ou URL; o comando existe só como snippet do README, aproveitável em 4 de 14 (9 sem bloco, 1 com caminho de exemplo). Por isso `lerOficial` fixa `TransporteSTDIO` (`curadoria.go:496`), nomeia `mcpservers.org/<slug>` (`:494`) e recusa com `ErrFormatoDaOrigem` o que não tem comando limpo (`reComandoDoSnippet`, `curadoria.go:446`; testdata `mcpservers-oficial-comando.html` e `-sem-comando.html`).
- **Por que a lista de remotos existe** (`biblioteca.go:20-26`): dos 25 remotos amostrados, 25 declaram autenticação (22 OAuth) e só 3 existem no registry. Ela é a única fonte de `Autenticacao`/`PedeCredencial` (`biblioteca.go:170-177`).
- **Semântica de nome**: `Namespace()` e `DominioVerificado()` (`biblioteca.go:206-241`) dependem do formato DNS invertido do registry; com nome `mcpservers.org/<slug>` ambos deixam de significar algo.
- **Semente embutida** `semente.json.gz` (`semente.go:27`) foi gerada da mescla das três origens; só bootstrap do primeiro boot (`Sincronizador.semear`, `sincronizador.go:197`).
- **Sem configuração de produção**: `BaseRegistry` e `BaseCuradoria` são constantes; `NovaOrigem(base)`/`NovaCuradoria(base)` e `ComOrigemDaBiblioteca`/`ComCuradoriaDaBiblioteca` (`cmd/patchbay/aplicacao.go:107-122`) existem só para teste.
- **Texto ao usuário que cita o registry**: subtítulo da tela (`internal/biblioteca/admin.templ:80`), explicação do filtro de curados (`:122`), aviso da primeira varredura "cerca de trezentas páginas, uns quinze minutos" (`:174`), link de rodapé (`:291`). `README.md` seção "Biblioteca de servidores MCP" (`README.md:157` em diante) descreve as origens e a tabela comparativa.
- **Esquema**: `biblioteca_servidor` (`internal/platform/store/migracoes/00013_biblioteca.sql`) e colunas `autenticacao`/`curado` (`00014_biblioteca_curadoria.sql`). Nenhuma coluna é exclusiva do registry além de `versao`.

## Fatos — a página alvo (medido em 2026-09-11)

- `https://mcpservers.org/pt-BR/official` responde **403** ao fetch sem User-Agent de navegador e **200** com User-Agent de Chrome. Título: "652 Servidores MCP oficiais".
- Índice paginado por `?page=N`, 22 páginas; a página 1 e a 2 trazem 29 links únicos de detalhe cada; `?page=23` responde **404** (60 KB de HTML, não vazio).
- Card = `<a href="/pt-BR/servers/<slug>">` com título e descrição em pt-BR; sem badge de transporte ou autenticação no card.
- Os links de paginação apontam para `/official?page=N` **sem** o prefixo `/pt-BR`; a mesma consulta com o prefixo responde 200.
- A descrição de algumas entradas do `/official` diz explicitamente "Servidor MCP remoto (HTTP streamable, OAuth 2.1)" — ou seja, a lista oficial **não é** só processo local, embora o código de hoje a trate como stdio-only.
- Página de detalhe (`/pt-BR/servers/1password-mcp`, 92 KB) traz links de repositório GitHub misturados a "relacionados"; nenhum bloco `"mcpServers"` neste exemplo.

## Riscos

| Risco | Detalhe | Mitigação possível |
|---|---|---|
| Perda de cobertura | Sai o registry (29.6k) e, se D1 for "sim", os 293 remotos com OAuth; sobra 652, dos quais ~30% com comando aproveitável | decisão explícita do dono (D1, D2); número final medido na verificação, não estimado |
| Origem raspada por regex | Mudança de marcação do site quebra a varredura; hoje mitigado por `ErrFormatoDaOrigem` e catálogo anterior preservado | teste com fixture real e teste de "esquema mudado não é insistido" (já existem) |
| Bloqueio do site | 403 sem UA de navegador; rate limit medido em `curadoria.go:39-53` (pausa de 2 s) | manter UA e pausa; falha registra e preserva |
| Semente envelhecida | `semente.json.gz` traz nomes do registry até ser regerada | regerar na mesma mudança |
| Instalação existente | Catálogo com nomes do registry fica servindo até a primeira varredura bem-sucedida | D3 |

## O que eu preciso de você

1. **D1 — a lista de remotos (`/remote-mcp-servers`) sai também?** "Somente `/official`" implica que sim; é a única fonte de autenticação/OAuth hoje.
2. **D2 — entrada do `/official` sem comando aproveitável:** entra no catálogo (nome, descrição, site, sem comando, "Adicionar" abre o formulário vazio de comando), ou fica de fora como hoje?
3. **D3 — instalação que já tem catálogo do registry:** esperar a primeira varredura trocar tudo, ou limpar o catálogo antigo na migração?

## Proposta derivada

Vai para `proposal.md`: RQ-01 (origem única), RQ-02 (o que uma entrada carrega, D2), RQ-03 (entradas remotas dentro do `/official`), RQ-04 (tela e textos), RQ-05 (semente), RQ-06 (instalação existente, D3). Decisão técnica candidata para `design.md`: eliminar `Origem` (registry) e a varredura de remotos, ou mantê-las desligadas atrás da mesclagem.
