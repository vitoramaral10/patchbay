-- +goose Up
-- +goose StatementBegin

-- Fatia 4: composição fina do endpoint (seções 08.2, 08.8 e 11 do estudo).
--
-- endpoint_upstream.prefixo já existia desde a 00001 e passa a ser escrito pela
-- tela; o que falta aqui é a regra por ferramenta.
--
-- A regra é por (endpoint, upstream) e não por upstream: o mesmo upstream compõe
-- vários endpoints com filtros diferentes, e é essa linha que faz cada endpoint
-- enxergar um catálogo próprio a partir do mesmo snapshot descoberto uma vez só.
--
-- ordem é o que decide o resultado: a primeira regra de incluir/excluir que casa
-- com o nome original da ferramenta é a que vale, e a primeira de renomear que
-- casa é a que troca o nome-base. Sem a ordem persistida, editar a composição
-- mudaria o catálogo sem ninguém ter mudado nenhuma regra.
--
-- padrao é glob de `*` e casa contra o nome original no upstream, nunca contra o
-- nome já prefixado: filtrar pelo nome exposto faria mexer no prefixo apagar
-- ferramenta em silêncio.
--
-- renome só existe para a ação renomear, e o CHECK é o que impede uma linha
-- meio-preenchida de virar uma renomeação para nome vazio três telas depois.
CREATE TABLE endpoint_tool_rule (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint_id INTEGER NOT NULL REFERENCES endpoint (id) ON DELETE CASCADE,
    upstream_id INTEGER NOT NULL REFERENCES upstream (id) ON DELETE CASCADE,
    ordem       INTEGER NOT NULL DEFAULT 0,
    acao        TEXT    NOT NULL CHECK (acao IN ('incluir', 'excluir', 'renomear')),
    padrao      TEXT    NOT NULL CHECK (padrao <> ''),
    renome      TEXT    NOT NULL DEFAULT '',
    CHECK ((acao = 'renomear' AND renome <> '') OR (acao <> 'renomear' AND renome = ''))
);

-- A rematerialização de um endpoint lê todas as regras dele de uma vez, na
-- ordem: é exatamente este índice.
CREATE INDEX idx_endpoint_tool_rule_alvo
    ON endpoint_tool_rule (endpoint_id, upstream_id, ordem);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_endpoint_tool_rule_alvo;
DROP TABLE endpoint_tool_rule;
-- +goose StatementEnd
