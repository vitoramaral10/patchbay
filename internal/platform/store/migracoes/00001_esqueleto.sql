-- +goose Up
-- +goose StatementBegin

-- Tabelas mínimas da fatia 1, na forma prevista pela seção 08.8 do estudo.
-- Colunas que a fatia 1 ainda não usa entram aqui porque a forma é contrato:
-- upstream.comando/args/env são da fatia 5 (STDIO) e upstream.ultimo_erro é
-- texto para a UI, nunca gatilho de lógica.
--
-- Instante é INTEGER com segundos de epoch UTC: evita depender do parsing de
-- data do driver e ordena por comparação numérica.

CREATE TABLE upstream (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    nome        TEXT    NOT NULL UNIQUE,
    tipo        TEXT    NOT NULL CHECK (tipo IN ('http', 'sse', 'stdio')),
    url         TEXT    NOT NULL DEFAULT '',
    comando     TEXT    NOT NULL DEFAULT '',
    args        TEXT    NOT NULL DEFAULT '[]',
    env         TEXT    NOT NULL DEFAULT '{}',
    timeout_ms  INTEGER NOT NULL DEFAULT 15000 CHECK (timeout_ms > 0),
    habilitado  INTEGER NOT NULL DEFAULT 1 CHECK (habilitado IN (0, 1)),
    ultimo_erro TEXT    NOT NULL DEFAULT '',
    criado_em   INTEGER NOT NULL
);

-- O slug entra na URL e no resource do token: é contrato, e renomeá-lo invalida
-- silenciosamente a credencial de todo cliente daquele endpoint.
CREATE TABLE endpoint (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    slug      TEXT    NOT NULL UNIQUE,
    descricao TEXT    NOT NULL DEFAULT '',
    criado_em INTEGER NOT NULL
);

CREATE TABLE endpoint_upstream (
    endpoint_id INTEGER NOT NULL REFERENCES endpoint (id) ON DELETE CASCADE,
    upstream_id INTEGER NOT NULL REFERENCES upstream (id) ON DELETE CASCADE,
    prefixo     TEXT    NOT NULL DEFAULT '',
    ordem       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (endpoint_id, upstream_id)
);

-- Credencial que o patchbay verifica: hash, nunca reversível. O prefixo visível
-- é o que permite a UI dizer qual chave é qual sem guardar a chave.
CREATE TABLE api_key (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    nome            TEXT    NOT NULL,
    hash            TEXT    NOT NULL UNIQUE,
    prefixo_visivel TEXT    NOT NULL,
    criado_em       INTEGER NOT NULL,
    revogado_em     INTEGER,
    ultimo_uso_em   INTEGER
);

CREATE TABLE api_key_endpoint (
    api_key_id  INTEGER NOT NULL REFERENCES api_key (id) ON DELETE CASCADE,
    endpoint_id INTEGER NOT NULL REFERENCES endpoint (id) ON DELETE CASCADE,
    PRIMARY KEY (api_key_id, endpoint_id)
);

CREATE INDEX idx_endpoint_upstream_upstream ON endpoint_upstream (upstream_id);
CREATE INDEX idx_api_key_endpoint_endpoint ON api_key_endpoint (endpoint_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE api_key_endpoint;
DROP TABLE api_key;
DROP TABLE endpoint_upstream;
DROP TABLE endpoint;
DROP TABLE upstream;
-- +goose StatementEnd
