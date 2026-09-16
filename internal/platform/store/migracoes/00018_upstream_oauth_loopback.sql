-- +goose Up
-- +goose StatementBegin

-- Fatia 15: provedor que só aceita redirect de loopback (seção 06 do estudo).
--
-- A Canva — e é ela o caso que abriu esta fatia — só registra
-- http://127.0.0.1:<porta> como redirect_uri enquanto a integração não passa
-- por revisão. Não aceita localhost como hostname, não aceita domínio próprio.
-- E um gateway que roda como serviço não tem loopback que o navegador do admin
-- alcance: o 127.0.0.1 do admin é a máquina dele, não a do patchbay.
--
-- O estudo previa duas saídas — um binário auxiliar na máquina do admin, ou
-- colar o código à mão na UI — e nenhuma fonte cobria o cenário. A escolhida é
-- a segunda, porque não exige distribuir nem rodar um segundo programa: o
-- navegador do admin é redirecionado para um 127.0.0.1 onde ninguém escuta, a
-- página falha, e a URL que ficou na barra carrega o code e o state. Colar
-- aquela URL é a entrega.
--
-- Por que a coluna é por upstream, e não uma configuração global: o redirect é
-- contrato com um provedor específico. O que vale para a Canva não vale para o
-- Notion, e um valor global obrigaria todo upstream OAuth a usar loopback assim
-- que um único provedor precisasse dele.
--
-- Vazio é o normal e significa "usa o callback público do patchbay", que é o
-- caminho de todo upstream cadastrado antes desta fatia.
ALTER TABLE upstream_oauth ADD COLUMN redirect_loopback TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- A volta descarta o redirect de loopback. Quem estava nesse modo passa a
-- montar o callback público, que o provedor vai recusar — é a mesma situação de
-- antes da fatia, e não há para onde mover o valor.
ALTER TABLE upstream_oauth DROP COLUMN redirect_loopback;

-- +goose StatementEnd
