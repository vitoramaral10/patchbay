-- +goose NO TRANSACTION
-- +goose Up

-- Modo de credencial "nenhum": o admin declara que o MCP é aberto.
--
-- Até aqui a tela oferecia dois modos — estatica e oauth — e cadastrar um
-- servidor público era deixar o bearer em branco no modo estatica. Funcionava,
-- e era o modo de falha clássico de UI: a opção não existia na tela, então quem
-- chegava num MCP sem autenticação procurava o que preencher em vez de salvar.
-- Pior, o resultado ficava ambíguo no banco — "estatica sem bearer" é ao mesmo
-- tempo "é aberto" e "o admin ainda não colou o token", e a diferença entre as
-- duas é justamente o que explica um 401.
--
-- O SQLite não sabe alterar um CHECK, então a tabela é reconstruída pelo
-- procedimento de 12 passos da documentação. Diferente da reconstrução de
-- upstream_secret (00007), esta tem quatro tabelas-filhas apontando para
-- upstream (id) com ON DELETE CASCADE: endpoint_upstream, endpoint_tool_rule,
-- upstream_secret e upstream_oauth. (call_log guarda upstream_id solto, sem
-- referência, e por isso não entra nesta conta.)
--
-- Com foreign_keys ligado, DROP TABLE upstream dispararia essas cascatas e
-- apagaria composição, credencial e concessão de todo mundo. Por isso o
-- NO TRANSACTION acima: PRAGMA foreign_keys é no-op dentro de transação, e o
-- que protege esta migração é desligá-lo, fazer a troca numa transação
-- explícita e religá-lo. As filhas referenciam upstream *pelo nome*, então
-- depois do RENAME elas voltam a resolver sozinhas — nenhuma delas é tocada.
-- +goose StatementBegin
PRAGMA foreign_keys = off;
-- +goose StatementEnd

-- +goose StatementBegin
BEGIN;

CREATE TABLE upstream_novo (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    nome               TEXT    NOT NULL UNIQUE,
    tipo               TEXT    NOT NULL CHECK (tipo IN ('http', 'sse', 'stdio')),
    url                TEXT    NOT NULL DEFAULT '',
    comando            TEXT    NOT NULL DEFAULT '',
    args               TEXT    NOT NULL DEFAULT '[]',
    env                TEXT    NOT NULL DEFAULT '{}',
    timeout_ms         INTEGER NOT NULL DEFAULT 15000 CHECK (timeout_ms > 0),
    habilitado         INTEGER NOT NULL DEFAULT 1 CHECK (habilitado IN (0, 1)),
    ultimo_erro        TEXT    NOT NULL DEFAULT '',
    criado_em          INTEGER NOT NULL,
    modo_credencial    TEXT    NOT NULL DEFAULT 'estatica'
        CHECK (modo_credencial IN ('nenhum', 'estatica', 'oauth')),
    sonda_habilitada   INTEGER NOT NULL DEFAULT 0,
    sonda_ferramenta   TEXT    NOT NULL DEFAULT '',
    sonda_args         TEXT    NOT NULL DEFAULT '',
    sonda_espera       TEXT    NOT NULL DEFAULT '',
    sonda_intervalo_ms INTEGER NOT NULL DEFAULT 900000,
    sonda_timeout_ms   INTEGER NOT NULL DEFAULT 15000,
    sonda_tolerancia   INTEGER NOT NULL DEFAULT 2
);

-- Nenhuma linha muda de valor: quem estava em estatica continua em estatica.
-- Migrar "estatica sem bearer" para nenhum seria adivinhar a intenção de um
-- cadastro antigo, e adivinhar errado apagaria a distinção que esta migração
-- existe para criar.
INSERT INTO upstream_novo (id, nome, tipo, url, comando, args, env, timeout_ms,
                           habilitado, ultimo_erro, criado_em, modo_credencial,
                           sonda_habilitada, sonda_ferramenta, sonda_args, sonda_espera,
                           sonda_intervalo_ms, sonda_timeout_ms, sonda_tolerancia)
SELECT id, nome, tipo, url, comando, args, env, timeout_ms,
       habilitado, ultimo_erro, criado_em, modo_credencial,
       sonda_habilitada, sonda_ferramenta, sonda_args, sonda_espera,
       sonda_intervalo_ms, sonda_timeout_ms, sonda_tolerancia
  FROM upstream;

DROP TABLE upstream;
ALTER TABLE upstream_novo RENAME TO upstream;

COMMIT;
-- +goose StatementEnd

-- +goose StatementBegin
PRAGMA foreign_keys = on;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys = off;
-- +goose StatementEnd

-- +goose StatementBegin
BEGIN;

CREATE TABLE upstream_antigo (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    nome               TEXT    NOT NULL UNIQUE,
    tipo               TEXT    NOT NULL CHECK (tipo IN ('http', 'sse', 'stdio')),
    url                TEXT    NOT NULL DEFAULT '',
    comando            TEXT    NOT NULL DEFAULT '',
    args               TEXT    NOT NULL DEFAULT '[]',
    env                TEXT    NOT NULL DEFAULT '{}',
    timeout_ms         INTEGER NOT NULL DEFAULT 15000 CHECK (timeout_ms > 0),
    habilitado         INTEGER NOT NULL DEFAULT 1 CHECK (habilitado IN (0, 1)),
    ultimo_erro        TEXT    NOT NULL DEFAULT '',
    criado_em          INTEGER NOT NULL,
    modo_credencial    TEXT    NOT NULL DEFAULT 'estatica'
        CHECK (modo_credencial IN ('estatica', 'oauth')),
    sonda_habilitada   INTEGER NOT NULL DEFAULT 0,
    sonda_ferramenta   TEXT    NOT NULL DEFAULT '',
    sonda_args         TEXT    NOT NULL DEFAULT '',
    sonda_espera       TEXT    NOT NULL DEFAULT '',
    sonda_intervalo_ms INTEGER NOT NULL DEFAULT 900000,
    sonda_timeout_ms   INTEGER NOT NULL DEFAULT 15000,
    sonda_tolerancia   INTEGER NOT NULL DEFAULT 2
);

-- A volta reescreve nenhum para estatica, que é o que o CHECK antigo aceita e
-- o que esses upstreams significavam antes desta migração: modo estático sem
-- credencial gravada nenhuma.
INSERT INTO upstream_antigo (id, nome, tipo, url, comando, args, env, timeout_ms,
                             habilitado, ultimo_erro, criado_em, modo_credencial,
                             sonda_habilitada, sonda_ferramenta, sonda_args, sonda_espera,
                             sonda_intervalo_ms, sonda_timeout_ms, sonda_tolerancia)
SELECT id, nome, tipo, url, comando, args, env, timeout_ms,
       habilitado, ultimo_erro, criado_em,
       CASE modo_credencial WHEN 'nenhum' THEN 'estatica' ELSE modo_credencial END,
       sonda_habilitada, sonda_ferramenta, sonda_args, sonda_espera,
       sonda_intervalo_ms, sonda_timeout_ms, sonda_tolerancia
  FROM upstream;

DROP TABLE upstream;
ALTER TABLE upstream_antigo RENAME TO upstream;

COMMIT;
-- +goose StatementEnd

-- +goose StatementBegin
PRAGMA foreign_keys = on;
-- +goose StatementEnd
