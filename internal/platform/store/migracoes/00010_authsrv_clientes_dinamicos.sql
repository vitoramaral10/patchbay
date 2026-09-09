-- +goose Up
-- +goose StatementBegin

-- Fatia 11: o cliente que se registra sozinho — DCR (RFC 7591) e CIMD
-- (Client ID Metadata Document, o que a spec MCP 2026-07-28 pôs no lugar do
-- DCR sem removê-lo).
--
-- Nenhuma tabela nova. A 00005 já criou oauth_client com
-- tipo IN ('prereg', 'dcr', 'cimd_cache') e com expira_em, que é exatamente o
-- TTL do documento de CIMD. O que falta são as duas colunas que separam um
-- registro dinâmico de um cadastro feito à mão pela UI.

-- escopo_aberto marca o cliente cujo escopo é "todos os endpoints existentes",
-- em vez das linhas de oauth_client_endpoint.
--
-- É o que um registro dinâmico precisa: ninguém escolheu endpoint por ele, e
-- gravar as linhas do escopo no momento do registro deixaria o cliente cego
-- para todo endpoint criado depois. Não é um furo de autorização: o que
-- autoriza de fato é o consentimento, que acontece uma vez por endpoint, na
-- sessão do admin, com o slug e o hostname do redirect na tela. Escopo aberto
-- só quer dizer "pode *pedir* qualquer endpoint".
ALTER TABLE oauth_client ADD COLUMN escopo_aberto INTEGER NOT NULL DEFAULT 0;

-- origem é quem pediu o registro: o IP remoto do POST /oauth/register.
--
-- Existe por duas razões. A primeira é o teto por origem/hora — sem uma chave
-- por onde contar, um endpoint de registro aberto vira tabela infinita. A
-- segunda é a tela: "de onde veio este cliente que eu nunca cadastrei" é a
-- primeira pergunta de quem encontra uma linha de DCR na lista.
ALTER TABLE oauth_client ADD COLUMN origem TEXT NOT NULL DEFAULT '';

-- O teto de registros conta por (tipo, criado_em) no total e por
-- (origem, criado_em) por quem pediu; a limpeza varre cimd_cache por expira_em.
CREATE INDEX idx_oauth_client_registro ON oauth_client (tipo, criado_em);
CREATE INDEX idx_oauth_client_origem ON oauth_client (origem, criado_em);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_oauth_client_origem;
DROP INDEX idx_oauth_client_registro;
ALTER TABLE oauth_client DROP COLUMN origem;
ALTER TABLE oauth_client DROP COLUMN escopo_aberto;
-- +goose StatementEnd
