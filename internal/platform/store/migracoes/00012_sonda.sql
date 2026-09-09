-- +goose Up
-- +goose StatementBegin

-- Fatia 9: a sonda de saúde funcional (seções 08.7 e 11 do estudo).
--
-- A sonda executa um tools/call de verdade contra o upstream, com a ferramenta
-- e os argumentos escolhidos pelo admin. É o diferencial do produto — nenhum
-- gateway do survey confirma fazer isso — e é também a única feature capaz de
-- remover capacidade por erro do próprio admin: uma sonda mal configurada apaga
-- as ferramentas de um servidor saudável.
--
-- Por isso tudo aqui nasce desligado. sonda_habilitada tem DEFAULT 0, e nenhum
-- upstream já cadastrado passa a ser sondado por causa desta migração. Inferir a
-- ferramenta seria a forma de mandar mensagem para alguém sem querer: o patchbay
-- não tem como saber se send_message é inócua, e quem sabe é quem configurou o
-- servidor.
--
-- O que NÃO está aqui é tão importante quanto o que está: nenhuma coluna de
-- estado, de última execução ou de último erro da sonda. O único estado
-- persistido de um upstream continua sendo habilitado (seções 05 e 08.8), e
-- sonda_falhou vive em memória como degradado e sem_consentimento — todo boot
-- recomeça em novo. Persistir "a sonda falhou" seria gravar uma decisão em vez
-- de ler uma tentativa, que é exatamente o que travava a reconexão no MetaMCP.
ALTER TABLE upstream ADD COLUMN sonda_habilitada   INTEGER NOT NULL DEFAULT 0;

-- A ferramenta e os argumentos exatos da chamada. args é o JSON do objeto de
-- argumentos, guardado como texto pelo mesmo motivo de upstream.args: é o corpo
-- que vai para o tools/call byte a byte, e reconstruí-lo a partir de um formato
-- próprio só criaria uma regra de escape para o admin descobrir errando.
ALTER TABLE upstream ADD COLUMN sonda_ferramenta   TEXT    NOT NULL DEFAULT '';
ALTER TABLE upstream ADD COLUMN sonda_args         TEXT    NOT NULL DEFAULT '';

-- sonda_espera é o trecho que precisa aparecer no texto da resposta para a
-- sondagem contar como sucesso. Opcional: vazio significa "basta não dar erro".
-- Existe porque há servidor que responde 200 com um corpo de erro amigável, sem
-- isError, e para esse a única sonda honesta é a que confere o conteúdo.
ALTER TABLE upstream ADD COLUMN sonda_espera       TEXT    NOT NULL DEFAULT '';

-- O intervalo consome cota da API do provedor: sem ele configurável, o
-- diagnóstico vira o problema. 900000 ms (15 min) é o número ilustrativo do
-- exemplo da seção 08.8, não um default medido — quem paga a cota escolhe.
ALTER TABLE upstream ADD COLUMN sonda_intervalo_ms INTEGER NOT NULL DEFAULT 900000;

-- Timeout próprio, separado do timeout do upstream: a sonda pode chamar uma
-- ferramenta legitimamente lenta sem que isso alargue o prazo de conectar e de
-- listar. Ele cancela só a chamada — nunca a sessão.
ALTER TABLE upstream ADD COLUMN sonda_timeout_ms   INTEGER NOT NULL DEFAULT 15000;

-- Quantas sondagens seguidas precisam falhar para o upstream ir a sonda_falhou.
-- Duas por padrão: uma falha isolada é o soluço de rede que o backoff já cobre,
-- e derrubar o catálogo por causa dela transformaria a sonda numa fonte de
-- instabilidade em vez de um detector.
ALTER TABLE upstream ADD COLUMN sonda_tolerancia   INTEGER NOT NULL DEFAULT 2;

-- A sonda deixa rastro na trilha, e origem é o que a separa da chamada de
-- cliente.
--
-- Uma coluna nova em vez de um valor novo em resultado, de propósito. resultado
-- responde "o que aconteceu" (ok, erro, timeout) e origem responde "quem
-- pediu"; enfiar 'sonda_erro' no primeiro faria uma sondagem que estourou o
-- prazo deixar de ser encontrável pelo filtro de timeout, e obrigaria todo
-- consumidor de resultado a aprender dois vocabulários para a mesma pergunta.
-- Com origem separada, o resumo do painel exclui a sonda com um predicado só e
-- os contadores continuam significando "chamada de cliente de verdade".
--
-- DEFAULT 'cliente' porque toda linha já gravada é de chamada de cliente: era o
-- único caminho que existia antes desta fatia.
ALTER TABLE call_log ADD COLUMN origem TEXT NOT NULL DEFAULT 'cliente'
    CHECK (origem IN ('cliente', 'sonda'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE call_log DROP COLUMN origem;
ALTER TABLE upstream DROP COLUMN sonda_tolerancia;
ALTER TABLE upstream DROP COLUMN sonda_timeout_ms;
ALTER TABLE upstream DROP COLUMN sonda_intervalo_ms;
ALTER TABLE upstream DROP COLUMN sonda_espera;
ALTER TABLE upstream DROP COLUMN sonda_args;
ALTER TABLE upstream DROP COLUMN sonda_ferramenta;
ALTER TABLE upstream DROP COLUMN sonda_habilitada;
-- +goose StatementEnd
