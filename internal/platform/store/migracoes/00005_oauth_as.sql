-- +goose Up
-- +goose StatementBegin

-- Fatia 10: o authorization server OAuth 2.1 embutido.
--
-- Tudo o que este AS guarda é credencial que ele *verifica* — código,
-- access token, refresh token e segredo de cliente. Nenhuma delas precisa
-- voltar em claro, e por isso nenhuma delas é cifrada: vai hash (decisão 12 do
-- estudo). Guardar de forma reversível o que só se compara é criar um cofre de
-- credencial alheia sem necessidade nenhuma.

-- O cliente OAuth. tipo distingue a origem do registro: 'prereg' é o que a UI
-- cadastra (fatia 10); 'cimd_cache' e 'dcr' entram na fatia 11 e por isso o
-- CHECK já os aceita — misturar cache de CIMD com registro permanente numa
-- tabela sem tipo faz o cache virar registro que ninguém expira.
CREATE TABLE oauth_client (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id       TEXT    NOT NULL UNIQUE,
    nome            TEXT    NOT NULL,
    tipo            TEXT    NOT NULL DEFAULT 'prereg' CHECK (tipo IN ('prereg', 'dcr', 'cimd_cache')),
    confidencial    INTEGER NOT NULL DEFAULT 0 CHECK (confidencial IN (0, 1)),
    segredo_hash    TEXT    NOT NULL DEFAULT '',
    segredo_prefixo TEXT    NOT NULL DEFAULT '',
    criado_em       INTEGER NOT NULL,
    -- expira_em é o TTL do documento de CIMD (fatia 11). NULL = não expira.
    expira_em       INTEGER,
    revogado_em     INTEGER
);

-- A allowlist de redirect. Linha por URI e comparação exata: o match de
-- loopback ignorando a porta, que o Claude Code exige, é da fatia 11 e entra
-- como regra de comparação, não como coluna nova.
CREATE TABLE oauth_client_redirect (
    client_id_ref INTEGER NOT NULL REFERENCES oauth_client (id) ON DELETE CASCADE,
    redirect_uri  TEXT    NOT NULL,
    PRIMARY KEY (client_id_ref, redirect_uri)
);

-- O escopo de um cliente é o conjunto de endpoints que ele pode pedir. É a
-- mesma forma da api_key_endpoint, e pelo mesmo motivo: a fronteira de
-- autorização é o endpoint (seção 07).
CREATE TABLE oauth_client_endpoint (
    client_id_ref INTEGER NOT NULL REFERENCES oauth_client (id) ON DELETE CASCADE,
    endpoint_id   INTEGER NOT NULL REFERENCES endpoint (id) ON DELETE CASCADE,
    PRIMARY KEY (client_id_ref, endpoint_id)
);

-- O código de autorização. Uso único: usado_em é preenchido por UPDATE
-- condicional, e o segundo uso revoga a família inteira que o primeiro emitiu.
-- familia_id nasce aqui para que o código e os tokens que ele gera fiquem
-- amarrados desde antes de existir token.
CREATE TABLE oauth_code (
    code_hash      TEXT    NOT NULL PRIMARY KEY,
    familia_id     TEXT    NOT NULL,
    client_id_ref  INTEGER NOT NULL REFERENCES oauth_client (id) ON DELETE CASCADE,
    endpoint_id    INTEGER NOT NULL REFERENCES endpoint (id) ON DELETE CASCADE,
    resource       TEXT    NOT NULL,
    escopo         TEXT    NOT NULL,
    redirect_uri   TEXT    NOT NULL,
    code_challenge TEXT    NOT NULL,
    criado_em      INTEGER NOT NULL,
    expira_em      INTEGER NOT NULL,
    usado_em       INTEGER
);

-- Access e refresh na mesma tabela porque a rotação é uma operação sobre as
-- duas: substituido_por liga o refresh velho ao novo, e familia_id é o que
-- permite revogar tudo de uma vez quando um refresh já rotacionado reaparece.
CREATE TABLE oauth_token (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    hash            TEXT    NOT NULL UNIQUE,
    tipo            TEXT    NOT NULL CHECK (tipo IN ('access', 'refresh')),
    familia_id      TEXT    NOT NULL,
    client_id_ref   INTEGER NOT NULL REFERENCES oauth_client (id) ON DELETE CASCADE,
    endpoint_id     INTEGER NOT NULL REFERENCES endpoint (id) ON DELETE CASCADE,
    resource        TEXT    NOT NULL,
    escopo          TEXT    NOT NULL,
    substituido_por INTEGER REFERENCES oauth_token (id) ON DELETE SET NULL,
    criado_em       INTEGER NOT NULL,
    expira_em       INTEGER NOT NULL,
    revogado_em     INTEGER,
    ultimo_uso_em   INTEGER
);

-- oauth_token(hash) já é único pela declaração acima: é o lookup de toda
-- requisição a /mcp/{slug}. familia_id tem índice porque revogar a família é
-- escrita em lote, e client_id_ref porque a tela do cliente lista por cliente.
CREATE INDEX idx_oauth_token_familia ON oauth_token (familia_id);
CREATE INDEX idx_oauth_token_cliente ON oauth_token (client_id_ref);
CREATE INDEX idx_oauth_token_expira ON oauth_token (expira_em);
CREATE INDEX idx_oauth_code_expira ON oauth_code (expira_em);
CREATE INDEX idx_oauth_client_endpoint_endpoint ON oauth_client_endpoint (endpoint_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_oauth_client_endpoint_endpoint;
DROP INDEX idx_oauth_code_expira;
DROP INDEX idx_oauth_token_expira;
DROP INDEX idx_oauth_token_cliente;
DROP INDEX idx_oauth_token_familia;
DROP TABLE oauth_token;
DROP TABLE oauth_code;
DROP TABLE oauth_client_endpoint;
DROP TABLE oauth_client_redirect;
DROP TABLE oauth_client;
-- +goose StatementEnd
