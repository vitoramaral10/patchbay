-- +goose Up
-- +goose StatementBegin

-- A biblioteca passa a ter uma origem só: o registry oficial some, e sobra a
-- curadoria do mcpservers.org.
--
-- Esta migração é o que faz a troca valer para quem já tem o patchbay
-- instalado. Sem ela, o binário novo subiria com um catálogo velho cheio de
-- nomes com.* e io.github.* que a tela não sabe mais explicar — a segunda
-- origem nunca escreveu neles, e a primeira varredura levaria até doze horas
-- para substituí-los. O catálogo antigo é descartado aqui, na subida, e não na
-- próxima varredura.

-- Zera o catálogo inteiro. Não há retrofit possível: o nome de cada linha é do
-- formato do registry (com.notion/mcp), e não existe tradução para
-- mcpservers.org/<slug> a partir dele.
DELETE FROM biblioteca_servidor;

-- Zera o estado de sincronização junto, e não só o catálogo: concluida_em
-- continuar apontando para a última varredura do registry deixaria
-- Sincronizador.semear (que só grava a semente embutida quando concluida_em é
-- zero) sem nunca rodar, e o binário novo subiria com o catálogo vazio até a
-- próxima varredura de até doze horas. Zerando, o próximo boot semeia na hora.
UPDATE biblioteca_sincronizacao
   SET concluida_em = 0, tentada_em = 0, servidores = 0, erro = ''
 WHERE id = 1;

-- O índice era sobre a coluna curado, que era o sinal de "está na curadoria do
-- mcpservers.org" quando o catálogo ainda misturava as duas origens. Com uma
-- origem só, todo servidor que sobra é curado por definição, e o índice não
-- filtra mais nada — só custaria escrita a cada sincronização.
--
-- IF EXISTS: esta migração precisa ser idempotente a uma segunda aplicação.
-- Quem prova o descarte reaplica esta mesma migração sobre um banco que já
-- está no schema de hoje (ver TestCatalogoDoRegistryEDescartadoNaAtualizacao),
-- e sem o IF EXISTS a segunda passagem falharia tentando derrubar um índice
-- que a primeira já derrubou.
DROP INDEX IF EXISTS biblioteca_servidor_curado;

-- As colunas curado e versao (00013/00014) ficam na tabela, mas nenhum código
-- as escreve mais: Item perdeu os dois campos quando a segunda origem virou a
-- única. Removê-las exigiria recriar a tabela inteira (SQLite não tem DROP
-- COLUMN barato em todas as versões do driver embarcado), e não há ganho que
-- pague esse risco numa migração que já é destrutiva por natureza.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: não há como devolver o catálogo do registry descartado acima — ele já
-- foi apagado, e "desfazer" essa migração recriando linhas vazias ou lixo seria
-- pior do que deixar o catálogo do jeito que a próxima varredura o deixar.
-- +goose StatementEnd
