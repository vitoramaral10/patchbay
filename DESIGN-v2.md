# patchbay v2 — configurar sem precisar de IA

## Problema

Na v1, a primeira ferramenta só chega ao cliente depois de: gerar a chave mestra
(filtrando o texto do comando), exportar `PATCHBAY_MASTER_KEY`, acertar
`PATCHBAY_PUBLIC_URL`, criar o admin, cadastrar o MCP escolhendo o transporte e o
"modo de credencial", criar um endpoint, vincular o MCP escrevendo regras com glob,
emitir uma chave de API e montar o comando do cliente. São 9 itens de menu e
vocabulário de protocolo (upstream, vínculo, CIMD, DCR, sonda, lápide) na cara de
quem só quer ligar um MCP no Claude.

## Princípio

O motor da v1 está certo e testado (supervisão, OAuth de upstream, STDIO, AS,
trilha). A v2 refaz **o que a pessoa toca**: boot, navegação, cadastro, endpoint e
conexão do cliente. Nenhum recurso some; o que é raro vai para "Avançado".

## Boot sem configuração

- `PATCHBAY_MASTER_KEY` vira **opcional**: sem ela, a chave é gerada no primeiro boot
  e guardada em `<dados>/chave-mestra` (0600). Com ela, vale a do ambiente (deploys
  da v1 continuam iguais). Custo assumido: sem a variável, a cifra protege o banco
  copiado sozinho, não o diretório inteiro — dito na tela de Ajustes.
- `PATCHBAY_PUBLIC_URL` vira **opcional**: o primeiro acesso pergunta a URL pública,
  já preenchida com o endereço que o navegador está usando. Fica no banco; mudar em
  Ajustes reinicia a aplicação por dentro do processo (sem derrubar o container).
  Variável de ambiente, quando existe, vence e trava o campo.
- Resultado: `docker run -p 8787:8787 -v patchbay:/dados ghcr.io/.../patchbay` e
  abrir o navegador.

## Navegação: 9 itens → 3

| v2 | Absorve da v1 |
|---|---|
| **Endpoints** (início) | Painel, Endpoints, Chaves de API, Clientes OAuth |
| **MCPs** | MCPs, Biblioteca |
| **Atividade** | Trilha, Logs ao vivo |
| ⚙ Ajustes (canto) | Configuração YAML, URL pública, chave mestra |

## Fluxos

1. **Primeiro acesso** — usuário, senha, URL pública (pré-preenchida). Já nasce um
   endpoint `principal`.
2. **Adicionar MCP** — uma caixa: "cole a URL, o comando ou o JSON da documentação",
   e logo abaixo a busca na biblioteca. Aceita URL, `npx`/`uvx`/`docker run`,
   `claude mcp add …` e o JSON `mcpServers` do Claude Desktop/Cursor. O transporte é
   deduzido; a autenticação é descoberta (OAuth → botão **Conectar conta**; 401 sem
   OAuth → campo **Token**; aberto → nada). Timeout, headers e sonda ficam em
   "Avançado", fechado. O novo MCP entra em todos os endpoints marcados (todos, por
   padrão).
3. **Endpoint** — cada MCP é um cartão com liga/desliga; abrir mostra as
   ferramentas com caixa de seleção e campo de renome. A tela grava as regras
   (`excluir x`, `renomear x y`) que o motor já entende; as regras com glob seguem
   editáveis em "Regras avançadas". No topo, **Conectar um cliente**: abas claude.ai /
   Claude Code / Codex / Cursor com o texto pronto para copiar, e a chave de API
   gerada ali mesmo quando o cliente precisa dela.

## Fora

Nada de recurso. Sai só da superfície: o termo "upstream", o formulário por
transporte, a escolha manual do modo de credencial, a tela própria de chaves e de
clientes OAuth.
