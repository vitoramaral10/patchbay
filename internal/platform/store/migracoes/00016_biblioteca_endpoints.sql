-- +goose Up
-- +goose StatementBegin

-- A página de detalhe do mcpservers.org publica, para parte dos servidores
-- remotos, uma tabela com mais de um endpoint — a Cloudflare, por exemplo,
-- lista 17. Até aqui só o primeiro (ou o "recomendado") entrava, em url; os
-- demais eram descartados na leitura. Esta coluna guarda a lista inteira, na
-- ordem da página, para o formulário de upstream oferecer a escolha (D-07).

-- endpoints é JSON com a lista de URLs. Vazio (sem tabela de endpoints, ou
-- servidor stdio) grava '[]', nunca NULL: o repositório sempre decodifica para
-- uma fatia, nunca para nil, e uma coluna que pudesse ser NULL abriria mais um
-- caso a tratar em toda leitura sem nenhum ganho.
ALTER TABLE biblioteca_servidor ADD COLUMN endpoints TEXT NOT NULL DEFAULT '[]';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: a coluna nasce com DEFAULT '[]', então nenhuma linha existente fica
-- inconsistente com ela presente — um rollback não precisa desfazer nada para
-- o schema anterior voltar a funcionar. Removê-la exigiria recriar a tabela
-- inteira (SQLite não tem DROP COLUMN barato em todas as versões do driver
-- embarcado), como já registrado na 00015.
-- +goose StatementEnd
