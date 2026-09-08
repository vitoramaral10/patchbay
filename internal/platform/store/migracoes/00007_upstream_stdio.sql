-- +goose Up
-- +goose StatementBegin

-- Fatia 5: upstream STDIO com supervisor de processo (seções 08.4 e 08.8).
--
-- As colunas do processo — comando, args e env — já nasceram na 00001, porque a
-- forma do schema é contrato desde o começo. O que falta é o lugar das variáveis
-- de ambiente *sensíveis*.
--
-- Elas vão para upstream_secret, junto com bearer e header, e não para a coluna
-- env em claro. O token de um servidor MCP lançado por linha de comando é a
-- mesma classe de segredo que o bearer de um servidor HTTP — algo que o patchbay
-- *apresenta* e que precisa voltar em claro —, e guardá-lo em claro só porque
-- ele viaja pelo bloco de ambiente em vez de por um header seria a distinção
-- errada. A coluna env continua existindo para o que não é segredo: PATH extra,
-- NODE_ENV, nível de log. Esse o admin precisa poder reler na tela, e por isso
-- ele não é cifrado.
--
-- O SQLite não sabe alterar um CHECK, então a tabela é reconstruída — o que só
-- é seguro porque nenhuma outra tabela referencia upstream_secret; se algum dia
-- alguma passar a referenciá-la, esta migração para de valer como está e
-- precisa recriar essa referência também, não só a de upstream(id).
--
-- O número final é 00007, e não 00006: a fatia 4 estava sendo escrita em
-- paralelo e acabou ocupando o 00006, então esta migração entra depois dela.
-- Buraco na sequência não é problema para o goose (o 00003 já não existe);
-- dois arquivos com o mesmo número, sim.

CREATE TABLE upstream_secret_novo (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    upstream_id   INTEGER NOT NULL REFERENCES upstream (id) ON DELETE CASCADE,
    tipo          TEXT    NOT NULL CHECK (tipo IN ('bearer', 'header', 'env')),
    nome          TEXT    NOT NULL DEFAULT '',
    valor_cifrado TEXT    NOT NULL,
    criado_em     INTEGER NOT NULL,
    atualizado_em INTEGER NOT NULL,
    CHECK ((tipo = 'bearer' AND nome = '') OR (tipo IN ('header', 'env') AND nome <> ''))
);

INSERT INTO upstream_secret_novo (id, upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em)
SELECT id, upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em
  FROM upstream_secret;

DROP INDEX idx_upstream_secret_slot;
DROP TABLE upstream_secret;
ALTER TABLE upstream_secret_novo RENAME TO upstream_secret;

-- O índice único é o que faz "um valor por slot (upstream, tipo, nome)" ser
-- invariante do banco e não convenção do código. O AAD da cifra usa a mesma
-- chave natural, então duas linhas para o mesmo slot seriam dois valores que se
-- autenticam com o mesmo contexto.
CREATE UNIQUE INDEX idx_upstream_secret_slot ON upstream_secret (upstream_id, tipo, nome);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- A volta descarta as variáveis de ambiente cifradas: o CHECK antigo não as
-- aceita, e não há para onde movê-las sem decifrá-las — o que uma migração não
-- pode fazer, porque ela não tem a chave mestra.
CREATE TABLE upstream_secret_antigo (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    upstream_id   INTEGER NOT NULL REFERENCES upstream (id) ON DELETE CASCADE,
    tipo          TEXT    NOT NULL CHECK (tipo IN ('bearer', 'header')),
    nome          TEXT    NOT NULL DEFAULT '',
    valor_cifrado TEXT    NOT NULL,
    criado_em     INTEGER NOT NULL,
    atualizado_em INTEGER NOT NULL,
    CHECK ((tipo = 'bearer' AND nome = '') OR (tipo = 'header' AND nome <> ''))
);

INSERT INTO upstream_secret_antigo (id, upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em)
SELECT id, upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em
  FROM upstream_secret
 WHERE tipo IN ('bearer', 'header');

DROP INDEX idx_upstream_secret_slot;
DROP TABLE upstream_secret;
ALTER TABLE upstream_secret_antigo RENAME TO upstream_secret;
CREATE UNIQUE INDEX idx_upstream_secret_slot ON upstream_secret (upstream_id, tipo, nome);

-- +goose StatementEnd
