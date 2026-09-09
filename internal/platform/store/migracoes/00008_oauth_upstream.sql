-- +goose Up
-- +goose StatementBegin

-- Fatias 7, 8 e 14: OAuth de upstream (pré-registrado, CIMD e DCR) e o
-- transporte SSE legado (seções 06, 08.5 e 08.8).

-- modo_credencial separa as duas formas de o patchbay se apresentar a um
-- upstream HTTP ou SSE. São excludentes de propósito: um upstream que manda o
-- bearer estático *e* o bearer do OAuth manda dois Authorization, e o servidor
-- escolhe um sem dizer qual — que é a classe de bug que só aparece como 401
-- intermitente. STDIO não tem modo: ele não fala HTTP.
ALTER TABLE upstream ADD COLUMN modo_credencial TEXT NOT NULL DEFAULT 'estatica'
    CHECK (modo_credencial IN ('estatica', 'oauth'));

-- upstream_oauth é a linha de OAuth de um upstream: no máximo uma, por isso
-- upstream_id é a chave primária. Tabela própria, e não slots novos em
-- upstream_secret, por dois motivos.
--
-- O primeiro é que aqui há campo que não é segredo e que a tela precisa ler sem
-- decifrar nada: qual client_id está em uso, por qual dos três caminhos ele
-- veio, quando o token expira, quando foi o último refresh. Enfiar isso em
-- upstream_secret obrigaria a decifrar para desenhar a tela, e a tela de um
-- upstream tem que continuar abrindo depois de uma troca de chave mestra — é
-- durante esse diagnóstico que ela é mais necessária.
--
-- O segundo é que upstream_secret já foi reconstruída duas vezes para alargar o
-- CHECK de tipo (00007), e cada reconstrução é um risco que não paga nada aqui.
--
-- O que é segredo continua cifrado exatamente como o bearer estático:
-- AES-256-GCM com chave derivada da chave mestra, e o AAD é (tabela, coluna,
-- upstream_id). Coluna no AAD, e não um id de slot: assim um access token
-- transplantado para a coluna de refresh não autentica, mesmo com a chave certa.
--
-- client_id e client_id_efetivo são dois campos e não um. O primeiro é o que o
-- admin colou no formulário (pré-registro — o caminho do Google, que não
-- anuncia nem CIMD nem registration_endpoint). O segundo é o que o
-- consentimento acabou usando: o informado, a URL do Client ID Metadata
-- Document, ou o que o DCR emitiu. Um só campo faria o DCR sobrescrever o que o
-- admin digitou, e aí ninguém sabe mais qual dos dois estava configurado.
--
-- issuer é informado pelo admin e vazio significa "não confira". Quando
-- preenchido, o SDK recusa credencial pré-registrada de um authorization server
-- para uso com outro (SEP-2352) — é a proteção contra mix-up quando há vários
-- provedores em jogo.
--
-- url_token, estilo_auth e escopos são o mínimo para renovar o token depois de
-- um reinício sem refazer a descoberta inteira. Sem eles, o processo que sobe
-- com um refresh token válido não teria como usá-lo: o token endpoint só é
-- conhecido depois do RFC 8414, e refazer a descoberta exigiria o 401 do
-- upstream — o que joga o refresh de volta para o caminho da requisição.
CREATE TABLE upstream_oauth (
    upstream_id           INTEGER NOT NULL PRIMARY KEY REFERENCES upstream (id) ON DELETE CASCADE,

    client_id             TEXT    NOT NULL DEFAULT '',
    client_secret_cifrado TEXT    NOT NULL DEFAULT '',
    issuer                TEXT    NOT NULL DEFAULT '',

    client_id_efetivo     TEXT    NOT NULL DEFAULT '',
    registro              TEXT    NOT NULL DEFAULT ''
                              CHECK (registro IN ('', 'cimd', 'preregistrado', 'dcr')),
    url_token             TEXT    NOT NULL DEFAULT '',
    estilo_auth           INTEGER NOT NULL DEFAULT 0,
    escopos               TEXT    NOT NULL DEFAULT '[]',

    access_token_cifrado  TEXT    NOT NULL DEFAULT '',
    refresh_token_cifrado TEXT    NOT NULL DEFAULT '',
    tipo_token            TEXT    NOT NULL DEFAULT '',
    -- NULL é "sem prazo declarado", que é diferente de zero: um token sem
    -- expires_in é válido até o provedor dizer o contrário, e tratá-lo como
    -- vencido faria o patchbay pedir refresh a cada requisição.
    expira_em             INTEGER,
    ultimo_refresh_em     INTEGER,

    criado_em             INTEGER NOT NULL,
    atualizado_em         INTEGER NOT NULL
);

-- O state e o code_verifier do PKCE não têm tabela aqui de propósito, e a
-- ausência é decisão, não esquecimento. O verifier é gerado e guardado dentro
-- do AuthorizationCodeHandler do go-sdk e nunca é exposto
-- (auth/authorization_code.go:580): persisti-lo exigiria reimplementar a troca
-- do code por token por fora da biblioteca, que é exatamente o caminho em que
-- se reintroduz o bug de sobrescrever refresh_token vazio que o x/oauth2 já não
-- tem. Como o AuthorizationCodeFetcher do SDK é um callback que espera pelo
-- callback HTTP, a tentativa inteira vive numa única chamada, e o registro de
-- state de uso único é o mapa em memória do broker — que morre com o processo
-- junto com o verifier que ele emparelha.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE upstream_oauth;
ALTER TABLE upstream DROP COLUMN modo_credencial;

-- +goose StatementEnd
