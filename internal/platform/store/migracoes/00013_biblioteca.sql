-- +goose Up
-- +goose StatementBegin

-- A biblioteca passa a ter cópia local do catálogo do registry oficial.
--
-- A decisão anterior era não guardar nada: cada abertura da tela ia à origem.
-- Ela caiu contra a medição (2026-09-09). O registry publica 29.610 servidores
-- ativos em páginas de 100, a varredura inteira leva ~16 minutos, e buscas
-- avulsas chegaram a estourar 40 segundos sem responder. Ler ao vivo fazia a
-- tela herdar a latência e a instabilidade de um terceiro, a cada tecla
-- digitada.
--
-- O que se ganha guardando: a busca vira consulta local, instantânea, e a tela
-- funciona com a internet fora. O que se paga: o catálogo tem idade, e ela pode
-- estar errada. Por isso a idade é gravada e mostrada na tela — servidor que
-- entrou hoje não aparece até a próxima sincronização, e o admin precisa saber
-- disso em vez de descobrir procurando.
CREATE TABLE biblioteca_servidor (
    -- nome é o identificador do registry em DNS invertido (com.notion/mcp). É
    -- chave primária porque é ele que o registry promete estável, e é por ele
    -- que a tela encontra o servidor no clique em adicionar.
    nome            TEXT PRIMARY KEY,
    titulo          TEXT    NOT NULL DEFAULT '',
    descricao       TEXT    NOT NULL DEFAULT '',
    versao          TEXT    NOT NULL DEFAULT '',

    -- A forma de conexão, já traduzida na hora da sincronização. Traduzir ao
    -- gravar e não ao ler é o que mantém a leitura da tela sem regra de
    -- negócio: linha que não vira upstream cadastrável nunca chega aqui.
    transporte      TEXT    NOT NULL CHECK (transporte IN ('http', 'sse', 'stdio')),
    url             TEXT    NOT NULL DEFAULT '',
    comando         TEXT    NOT NULL DEFAULT '',
    -- args é o JSON do vetor de argumentos, guardado como texto pelo mesmo
    -- motivo de upstream.args: é o que vai para a execução na ordem em que veio,
    -- e um formato próprio só criaria uma regra de escape para descobrir
    -- errando.
    args            TEXT    NOT NULL DEFAULT '[]',

    pede_credencial INTEGER NOT NULL DEFAULT 0,
    site            TEXT    NOT NULL DEFAULT '',

    -- busca é nome, título e descrição juntos e em minúsculas, para a consulta
    -- da tela ser um LIKE só em vez de três com OR.
    --
    -- Não há índice sobre ela de propósito: LIKE '%termo%' não usa índice, e um
    -- índice que nunca é lido só custaria escrita a cada sincronização. Medido
    -- em 2026-09-09 com 30 mil linhas: de 30 a 96 ms por busca, contando a
    -- contagem e a página. O que era lento era a rede, não a comparação de
    -- texto — a leitura ao vivo passava de 40 segundos.
    busca           TEXT    NOT NULL DEFAULT ''
);

-- O estado da última sincronização, numa linha só.
--
-- Linha única e não histórico: o que a tela precisa responder é "de quando é o
-- que estou vendo", e um histórico de varreduras seria tabela crescendo para
-- ninguém ler. O erro fica junto porque a idade sozinha mente — catálogo de
-- ontem com a última tentativa falhando é uma situação diferente de catálogo de
-- ontem sincronizado no horário.
CREATE TABLE biblioteca_sincronizacao (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    -- concluida_em é o unix da última varredura que terminou inteira. Zero
    -- significa que nunca terminou nenhuma, e é o estado de instalação nova.
    concluida_em INTEGER NOT NULL DEFAULT 0,
    -- servidores é quantos ficaram gravados nessa varredura.
    servidores   INTEGER NOT NULL DEFAULT 0,
    -- erro é o motivo da última tentativa que não terminou. Vazio quando a
    -- última tentativa deu certo.
    erro         TEXT    NOT NULL DEFAULT '',
    -- tentada_em é o unix da última tentativa, tenha ela terminado ou não.
    tentada_em   INTEGER NOT NULL DEFAULT 0
);

INSERT INTO biblioteca_sincronizacao (id) VALUES (1);

-- Uma nota sobre o custo de escrita, para quem for mexer: a sincronização troca
-- o catálogo inteiro numa transação só, e ela segura o escritor único do SQLite
-- por ~640 ms com 30 mil linhas (medido em 2026-09-09). Isso acontece duas vezes
-- por dia, e a fila da trilha absorve a espera. Quebrar em lotes deixaria a
-- biblioteca aparecer pela metade para quem estivesse com a tela aberta — que é
-- pior do que o meio segundo.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE biblioteca_sincronizacao;
DROP TABLE biblioteca_servidor;
-- +goose StatementEnd
