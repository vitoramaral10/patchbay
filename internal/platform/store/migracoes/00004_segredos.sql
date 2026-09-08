-- +goose Up
-- +goose StatementBegin

-- Fatia 6: segredos cifrados em repouso (seções 08.8 e 14 do estudo).

-- settings é a configuração do processo que vive no banco. É chave/valor porque
-- as três coisas que a seção 08.8 previu para ela — o canário, a URL pública e o
-- generation da trava otimista do import de YAML — não têm nada em comum além de
-- serem uma linha só cada.
--
-- O canário mora aqui, na chave 'canario_chave_mestra', como valor cifrado: no
-- primeiro boot com chave ele é gravado, e em todo boot seguinte é decifrado e
-- comparado. Se não confere, o processo não sobe. É a escolha deliberada de
-- indisponibilidade sobre corrupção silenciosa: a alternativa é subir
-- "funcionando" e transformar todo segredo em repouso em lixo ilegível sem que
-- ninguém perceba.
CREATE TABLE settings (
    chave         TEXT    NOT NULL PRIMARY KEY,
    valor         TEXT    NOT NULL,
    atualizado_em INTEGER NOT NULL
);

-- upstream_secret guarda as credenciais estáticas que o patchbay *apresenta* ao
-- upstream: bearer e header estático. Cifra reversível, AES-256-GCM — hash aqui
-- simplesmente não funcionaria, porque o valor precisa voltar em claro para
-- entrar no header da requisição de saída.
--
-- valor_cifrado carrega o nonce dentro de si, junto com o prefixo de versão do
-- formato ("pbc1:"), e não numa coluna separada. Nonce em coluna própria permite
-- que valor e nonce venham de linhas diferentes por acidente de UPDATE, e um par
-- trocado é indistinguível de chave errada na hora de diagnosticar; e o prefixo
-- de versão só serve para rotação se ele governar o valor inteiro.
--
-- O AAD da cifra é (tabela, coluna, upstream_id/tipo/nome). Copiar o
-- valor_cifrado do upstream A para a linha do upstream B falha a autenticação
-- mesmo com a chave certa: quem tem escrita no banco e não tem a chave não
-- consegue apontar a credencial de um upstream para outro.
--
-- nome é '' para bearer e o nome do header para header. O índice único é o que
-- faz "um bearer por upstream" ser invariante do banco, e não convenção do
-- código.
CREATE TABLE upstream_secret (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    upstream_id   INTEGER NOT NULL REFERENCES upstream (id) ON DELETE CASCADE,
    tipo          TEXT    NOT NULL CHECK (tipo IN ('bearer', 'header')),
    nome          TEXT    NOT NULL DEFAULT '',
    valor_cifrado TEXT    NOT NULL,
    criado_em     INTEGER NOT NULL,
    atualizado_em INTEGER NOT NULL,
    CHECK ((tipo = 'bearer' AND nome = '') OR (tipo = 'header' AND nome <> ''))
);

CREATE UNIQUE INDEX idx_upstream_secret_slot ON upstream_secret (upstream_id, tipo, nome);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_upstream_secret_slot;
DROP TABLE upstream_secret;
DROP TABLE settings;
-- +goose StatementEnd
