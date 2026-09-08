-- +goose Up
-- +goose StatementBegin

-- Fatia 2: o admin único da UI web e a sessão dele.
--
-- A senha do admin é a única credencial escolhida por uma pessoa, e por isso é
-- a única que vai em argon2id (seção 10 do estudo): a chave de API tem 256 bits
-- sorteados e não tem dicionário a encarecer.
--
-- O CHECK (id = 1) é o que faz "admin único" ser invariante do banco e não
-- convenção do código: multiusuário e RBAC são não-objetivos da v1, e uma
-- segunda linha aqui seria meio caminho para um controle de acesso que ninguém
-- escreveu.
CREATE TABLE admin (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    usuario       TEXT    NOT NULL,
    senha_hash    TEXT    NOT NULL,
    criado_em     INTEGER NOT NULL,
    atualizado_em INTEGER NOT NULL
);

-- A sessão é credencial que o patchbay verifica: guarda o hash do token, nunca
-- o token. É o mesmo motivo da api_key — reexibir sessão não serve para nada e
-- guardar reversível cria cofre sem necessidade.
CREATE TABLE sessao_admin (
    hash          TEXT    NOT NULL PRIMARY KEY,
    admin_id      INTEGER NOT NULL REFERENCES admin (id) ON DELETE CASCADE,
    criado_em     INTEGER NOT NULL,
    expira_em     INTEGER NOT NULL,
    ultimo_uso_em INTEGER NOT NULL
);

CREATE INDEX idx_sessao_admin_expira ON sessao_admin (expira_em);

-- nome e instrucoes entram para a UI. O slug continua fora de qualquer UPDATE:
-- ele está na URL, na metadata RFC 9728 e no aud de todo token emitido, e
-- renomeá-lo invalidaria em silêncio a credencial de todo cliente daquele
-- endpoint (seção 10). A UI mostra o slug como texto fixo depois da criação.
ALTER TABLE endpoint ADD COLUMN nome       TEXT NOT NULL DEFAULT '';
ALTER TABLE endpoint ADD COLUMN instrucoes TEXT NOT NULL DEFAULT '';

-- Endpoint criado antes desta migração fica com o slug como nome, para que a
-- lista da UI nunca tenha linha sem rótulo.
UPDATE endpoint SET nome = slug WHERE nome = '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_sessao_admin_expira;
DROP TABLE sessao_admin;
DROP TABLE admin;
ALTER TABLE endpoint DROP COLUMN instrucoes;
ALTER TABLE endpoint DROP COLUMN nome;
-- +goose StatementEnd
