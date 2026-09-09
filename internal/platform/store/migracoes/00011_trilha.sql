-- +goose Up
-- +goose StatementBegin

-- Fatia 12: a trilha por chamada de ferramenta (seções 08.8 e 11 do estudo).
--
-- É a escrita mais frequente do sistema e o SQLite tem um único escritor: a
-- linha nunca é gravada dentro da transação da chamada. Ela vai por canal com
-- buffer para um escritor único que agrupa em lote, e ao encher o buffer o
-- registro é descartado com o descarte contado — o contador aparece na tela,
-- porque trilha que mente é pior que trilha faltando.
--
-- ts é epoch em MILISSEGUNDOS UTC, e é a única exceção à convenção de segundos
-- das outras tabelas. Sob rajada várias chamadas caem no mesmo segundo, e com
-- granularidade de segundo a ordem dentro da rajada — exatamente o que se olha
-- ao diagnosticar — vira sorteio do ORDER BY.
--
-- Não há FOREIGN KEY para endpoint nem para upstream, de propósito. Com
-- ON DELETE CASCADE, remover um endpoint apagaria a trilha dele: o histórico
-- sumiria justamente na hora em que se quer saber o que aquele endpoint andou
-- fazendo. Por isso slug e nome são retratos gravados na linha, não junções: a
-- trilha diz o que o endpoint se chamava quando a chamada aconteceu.
--
-- A trilha guarda tamanho de entrada e de saída, nunca o conteúdo. Argumento e
-- resultado de ferramenta são dado de terceiro e o caminho mais curto para um
-- segredo entrar no banco em claro; o que se diagnostica com eles é "grande
-- demais", e para isso o número basta.
CREATE TABLE call_log (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                  INTEGER NOT NULL,
    endpoint_id         INTEGER NOT NULL DEFAULT 0,
    endpoint_slug       TEXT    NOT NULL DEFAULT '',
    upstream_id         INTEGER NOT NULL DEFAULT 0,
    upstream_nome       TEXT    NOT NULL DEFAULT '',
    ferramenta          TEXT    NOT NULL DEFAULT '',
    ferramenta_original TEXT    NOT NULL DEFAULT '',
    resultado           TEXT    NOT NULL CHECK (resultado IN ('ok', 'erro', 'timeout')),
    erro                TEXT    NOT NULL DEFAULT '',
    duracao_ms          INTEGER NOT NULL DEFAULT 0,
    bytes_entrada       INTEGER NOT NULL DEFAULT 0,
    bytes_saida         INTEGER NOT NULL DEFAULT 0,
    sessao              TEXT    NOT NULL DEFAULT '',
    credencial          TEXT    NOT NULL DEFAULT '',
    era                 TEXT    NOT NULL DEFAULT ''
);

-- Os dois índices que a seção 08.8 nomeia: a tela ordena por tempo e filtra por
-- endpoint, e a varredura de retenção apaga por faixa de ts.
--
-- endpoint_slug e não endpoint_id: é por slug que a tela de trilha filtra
-- (Filtro.Endpoint, seção 11) — um endpoint_id exigiria a tela conhecer o id
-- interno, e um endpoint apagado não tem mais linha na tabela de endpoints
-- para traduzir um de volta no outro.
CREATE INDEX idx_call_log_ts ON call_log (ts);
CREATE INDEX idx_call_log_endpoint_ts ON call_log (endpoint_slug, ts);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_call_log_endpoint_ts;
DROP INDEX idx_call_log_ts;
DROP TABLE call_log;
-- +goose StatementEnd
