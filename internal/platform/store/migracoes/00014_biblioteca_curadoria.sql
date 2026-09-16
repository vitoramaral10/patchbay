-- +goose Up
-- +goose StatementBegin

-- A biblioteca passa a ler duas origens e a mesclá-las.
--
-- O registry oficial dá alcance (29.843 servidores, dois terços deles contas de
-- GitHub) e é a única fonte de servidor de processo local. O mcpservers.org dá
-- curadoria: 293 remotos escolhidos a dedo. Medido em 2026-09-09, numa amostra
-- de 25 dos 293:
--
--   * 25 de 25 declaram autenticação — 22 delas OAuth. O esquema do registry
--     **não tem** campo de autenticação, e é ele que decide se o formulário de
--     upstream abre em OAuth ou em credencial estática. Sem isto, o admin
--     cadastrava o Notion em credencial estática e descobria no primeiro 401.
--   * só 3 de 25 existem no registry. Neon, MDN, Pendo, Blackbaud, Candid e
--     Unthread são remotos conhecidos que simplesmente não estão lá — não é
--     duplicata, é cobertura que faltava.
--
-- Duas colunas guardam o que a segunda origem acrescenta. Elas nascem vazias
-- para todo mundo que já está gravado, e a próxima varredura as preenche: o
-- catálogo é substituído inteiro a cada sincronização, então não há retrofit a
-- fazer aqui.

-- autenticacao é o que o servidor exige: 'oauth', 'token' ou 'aberta'. Vazio
-- quando ninguém declarou, que é o caso de todo servidor que vem só do registry
-- — e vazio precisa continuar sendo distinguível de 'aberta', porque um manda o
-- formulário ficar no padrão e o outro afirma que não há credencial.
ALTER TABLE biblioteca_servidor ADD COLUMN autenticacao TEXT NOT NULL DEFAULT ''
    CHECK (autenticacao IN ('', 'oauth', 'token', 'aberta'));

-- curado é o servidor estar na lista de remotos do mcpservers.org.
--
-- É o sinal de confiança mais forte que a biblioteca tem, e é por ele que a tela
-- filtra. A alternativa que existia antes — namespace de domínio verificado —
-- corta 29.843 para 9.871, mas em 7.701 domínios distintos: quase tudo é domínio
-- de um servidor só, então cortar dois terços não transforma cauda longa em
-- curadoria.
ALTER TABLE biblioteca_servidor ADD COLUMN curado INTEGER NOT NULL DEFAULT 0;

-- Índice sobre curado, e não sobre busca: aqui a cardinalidade é de dois
-- valores, mas a consulta filtrada é `curado = 1`, que casa poucas centenas de
-- linhas em trinta mil — exatamente o caso em que o índice paga.
CREATE INDEX biblioteca_servidor_curado ON biblioteca_servidor(curado) WHERE curado = 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX biblioteca_servidor_curado;
ALTER TABLE biblioteca_servidor DROP COLUMN curado;
ALTER TABLE biblioteca_servidor DROP COLUMN autenticacao;
-- +goose StatementEnd
