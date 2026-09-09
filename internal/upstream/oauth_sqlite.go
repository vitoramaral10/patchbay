package upstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// tabelaOAuth e as colunas cifradas entram no AAD da cifra. Constantes e não
// literais espalhados porque o AAD gravado precisa ser byte a byte o mesmo do
// AAD conferido — um typo aqui só apareceria como "não autentica" em produção.
const (
	tabelaOAuth        = "upstream_oauth"
	colunaSegredoOAuth = "client_secret_cifrado"
	colunaAcesso       = "access_token_cifrado"
	colunaRefresh      = "refresh_token_cifrado"
)

// campoOAuth monta o contexto da cifra de uma coluna de upstream_oauth.
//
// A coluna entra no AAD, e é isso que impede transplante *dentro da mesma
// linha*: um access token copiado para a coluna de refresh não autentica nem com
// a chave certa. Com AAD só de tabela e linha, os três valores seriam
// intercambiáveis entre si.
func campoOAuth(upstreamID int64, coluna string) cripto.Campo {
	return cripto.Campo{
		Tabela: tabelaOAuth,
		Coluna: coluna,
		ID:     strconv.FormatInt(upstreamID, 10),
	}
}

// ClienteOAuth devolve o cliente pré-registrado de um upstream, com o segredo em
// claro.
//
// É o que o broker lê ao montar o handler. Upstream sem linha de OAuth devolve o
// zero e nenhum erro: "não configurado" é um estado normal, não uma falha.
func (r *RepositorioSQLite) ClienteOAuth(ctx context.Context, upstreamID int64) (ClienteOAuth, error) {
	var (
		c       ClienteOAuth
		cifrado string
	)
	err := r.leitura.QueryRowContext(ctx, `
SELECT client_id, client_secret_cifrado, issuer
  FROM upstream_oauth
 WHERE upstream_id = ?`, upstreamID).Scan(&c.ClientID, &cifrado, &c.Issuer)
	if errors.Is(err, sql.ErrNoRows) {
		return ClienteOAuth{}, nil
	}
	if err != nil {
		return ClienteOAuth{}, fmt.Errorf("upstream: selecionar cliente OAuth de %d: %w", upstreamID, err)
	}
	if cifrado != "" {
		c.Segredo, err = r.cifrador.Decifrar(campoOAuth(upstreamID, colunaSegredoOAuth), cifrado)
		if err != nil {
			// A mensagem nomeia o slot, nunca o valor.
			return ClienteOAuth{}, fmt.Errorf("upstream: decifrar client_secret de %d: %w", upstreamID, err)
		}
	}
	return c, nil
}

// Concessao devolve a concessão em vigor de um upstream.
//
// O segundo retorno é falso quando não há access token gravado — nunca
// consentiu, ou o provedor revogou e a concessão foi apagada.
func (r *RepositorioSQLite) Concessao(ctx context.Context, upstreamID int64) (Concessao, bool, error) {
	var (
		c                 Concessao
		acesso, refresh   string
		escopos           string
		expira, refreshEm sql.NullInt64
	)
	err := r.leitura.QueryRowContext(ctx, `
SELECT client_id_efetivo, registro, url_token, estilo_auth, escopos,
       access_token_cifrado, refresh_token_cifrado, tipo_token, expira_em, ultimo_refresh_em
  FROM upstream_oauth
 WHERE upstream_id = ?`, upstreamID).Scan(
		&c.ClientIDEfetivo, &c.Registro, &c.URLToken, &c.Estilo, &escopos,
		&acesso, &refresh, &c.Token.Tipo, &expira, &refreshEm)
	if errors.Is(err, sql.ErrNoRows) {
		return Concessao{}, false, nil
	}
	if err != nil {
		return Concessao{}, false, fmt.Errorf("upstream: selecionar concessão OAuth de %d: %w", upstreamID, err)
	}
	if acesso == "" {
		return Concessao{}, false, nil
	}

	if c.Escopos, err = decodificarEscopos(escopos); err != nil {
		return Concessao{}, false, fmt.Errorf("upstream %d: %w", upstreamID, err)
	}
	if c.Token.Acesso, err = r.cifrador.Decifrar(campoOAuth(upstreamID, colunaAcesso), acesso); err != nil {
		return Concessao{}, false, fmt.Errorf("upstream: decifrar access token de %d: %w", upstreamID, err)
	}
	if refresh != "" {
		if c.Token.Refresh, err = r.cifrador.Decifrar(campoOAuth(upstreamID, colunaRefresh), refresh); err != nil {
			return Concessao{}, false, fmt.Errorf("upstream: decifrar refresh token de %d: %w", upstreamID, err)
		}
	}
	if expira.Valid {
		c.Token.Expira = time.Unix(expira.Int64, 0)
	}
	if refreshEm.Valid {
		c.RefreshEm = time.Unix(refreshEm.Int64, 0)
	}
	return c, true, nil
}

// GravarConcessao grava o token e o que permite renová-lo.
//
// É chamada pela fonte de token a cada token novo — o inicial e todo refresh —,
// e por isso ela é UPSERT: a linha pode não existir quando o consentimento
// aconteceu num upstream em que o admin não colou client_id nenhum (caminho de
// CIMD ou DCR).
func (r *RepositorioSQLite) GravarConcessao(ctx context.Context, upstreamID int64, c Concessao) error {
	acesso, err := r.cifrador.Cifrar(campoOAuth(upstreamID, colunaAcesso), c.Token.Acesso)
	if err != nil {
		return fmt.Errorf("upstream: cifrar access token de %d: %w", upstreamID, err)
	}
	refresh := ""
	if !c.Token.Refresh.Vazio() {
		if refresh, err = r.cifrador.Cifrar(campoOAuth(upstreamID, colunaRefresh), c.Token.Refresh); err != nil {
			return fmt.Errorf("upstream: cifrar refresh token de %d: %w", upstreamID, err)
		}
	}
	escopos, err := codificarEscopos(c.Escopos)
	if err != nil {
		return err
	}

	agora := time.Now().Unix()
	_, err = r.escrita.ExecContext(ctx, `
INSERT INTO upstream_oauth (
    upstream_id, client_id_efetivo, registro, url_token, estilo_auth, escopos,
    access_token_cifrado, refresh_token_cifrado, tipo_token, expira_em,
    ultimo_refresh_em, criado_em, atualizado_em)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (upstream_id) DO UPDATE SET
    client_id_efetivo     = excluded.client_id_efetivo,
    registro              = excluded.registro,
    url_token             = excluded.url_token,
    estilo_auth           = excluded.estilo_auth,
    escopos               = excluded.escopos,
    access_token_cifrado  = excluded.access_token_cifrado,
    refresh_token_cifrado = excluded.refresh_token_cifrado,
    tipo_token            = excluded.tipo_token,
    expira_em             = excluded.expira_em,
    ultimo_refresh_em     = excluded.ultimo_refresh_em,
    atualizado_em         = excluded.atualizado_em`,
		upstreamID, c.ClientIDEfetivo, c.Registro, c.URLToken, c.Estilo, escopos,
		acesso, refresh, c.Token.Tipo, instanteSQL(c.Token.Expira),
		instanteSQL(c.RefreshEm), agora, agora)
	if err != nil {
		return fmt.Errorf("upstream: gravar concessão OAuth de %d: %w", upstreamID, err)
	}
	return nil
}

// ApagarConcessao zera o token de um upstream, mantendo o que o admin
// configurou.
//
// client_id, client_secret e issuer ficam: eles são configuração, e apagá-los
// junto obrigaria o admin a colar tudo de novo por causa de uma revogação do
// provedor. client_id_efetivo, registro e url_token também ficam, porque a tela
// precisa continuar dizendo qual cliente estava em uso — é a informação que
// explica *por que* o consentimento caiu. ultimo_refresh_em zera junto com o
// token: sem concessão não há "último refresh" nenhum, e deixá-lo gravado
// faria a tela mostrar a data de um refresh que já não vale mais nada.
func (r *RepositorioSQLite) ApagarConcessao(ctx context.Context, upstreamID int64) error {
	_, err := r.escrita.ExecContext(ctx, `
UPDATE upstream_oauth
   SET access_token_cifrado  = '',
       refresh_token_cifrado = '',
       tipo_token            = '',
       expira_em             = NULL,
       ultimo_refresh_em     = NULL,
       atualizado_em         = ?
 WHERE upstream_id = ?`, time.Now().Unix(), upstreamID)
	if err != nil {
		return fmt.Errorf("upstream: apagar concessão OAuth de %d: %w", upstreamID, err)
	}
	return nil
}

// EstadoOAuth devolve o que a tela mostra de OAuth, sem decifrar nada.
//
// Não decifrar é o requisito: a tela de um upstream tem que continuar abrindo
// depois de uma troca de chave mestra, e é justamente durante esse diagnóstico
// que ela é mais necessária. Aqui não passa nenhum segredo — só o fato de haver
// um.
func (r *RepositorioSQLite) EstadoOAuth(ctx context.Context, upstreamID int64) (EstadoOAuth, error) {
	var (
		e                 EstadoOAuth
		segredo, acesso   string
		expira, refreshEm sql.NullInt64
	)
	err := r.leitura.QueryRowContext(ctx, `
SELECT client_id, client_secret_cifrado, issuer, client_id_efetivo, registro,
       access_token_cifrado, expira_em, ultimo_refresh_em
  FROM upstream_oauth
 WHERE upstream_id = ?`, upstreamID).Scan(
		&e.ClientID, &segredo, &e.Issuer, &e.ClientIDEfetivo, &e.Registro,
		&acesso, &expira, &refreshEm)
	if errors.Is(err, sql.ErrNoRows) {
		return EstadoOAuth{}, nil
	}
	if err != nil {
		return EstadoOAuth{}, fmt.Errorf("upstream: selecionar estado OAuth de %d: %w", upstreamID, err)
	}
	e.SegredoDefinido = segredo != ""
	e.Consentido = acesso != ""
	if expira.Valid {
		e.ExpiraEm = time.Unix(expira.Int64, 0)
	}
	if refreshEm.Valid {
		e.RefreshEm = time.Unix(refreshEm.Int64, 0)
	}
	return e, nil
}

// aplicarOAuth grava o que o formulário disse sobre OAuth, na mesma transação do
// upstream.
//
// Segredo em branco significa "manter o gravado", como em toda credencial desta
// tela: o formulário nunca reexibe o valor, então um campo vazio é o estado
// normal de quem só veio mudar o timeout. Apagar é explícito.
//
// Trocar o client_id apaga a concessão. É o comportamento certo e não um efeito
// colateral: um token emitido para outro cliente não vale para este, e mantê-lo
// gravado produziria um 401 que a tela descreveria como "consentido".
func (r *RepositorioSQLite) aplicarOAuth(ctx context.Context, tx *sql.Tx, upstreamID int64, f Form) error {
	if f.ModoEfetivo() != ModoOAuth {
		// Sair do modo OAuth apaga a linha inteira: as credenciais de um fluxo
		// que já não roda são cofre de credencial alheia sem necessidade.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM upstream_oauth WHERE upstream_id = ?`, upstreamID); err != nil {
			return fmt.Errorf("upstream: apagar OAuth de %d: %w", upstreamID, err)
		}
		return nil
	}

	// Entrar no modo oauth apaga o bearer estático na mesma transação: Form.Validar
	// já recusa gravar os dois juntos pela borda normal, mas esta é a defesa que
	// vale para uma linha já gravada antes — por uma corrida entre duas abas do
	// admin, ou por um formulário que chegou sem passar pela validação — e que só
	// seria pega por acaso na próxima edição do bearer.
	if err := apagarCredencial(ctx, tx, upstreamID, CredencialBearer, ""); err != nil {
		return err
	}

	anterior, err := clienteOAuthDaTx(ctx, tx, upstreamID)
	if err != nil {
		return err
	}

	segredo := anterior.segredo
	switch {
	case f.OAuthSegredoLimpar:
		segredo = ""
	case !f.OAuthSegredo.Vazio():
		if segredo, err = r.cifrador.Cifrar(
			campoOAuth(upstreamID, colunaSegredoOAuth), f.OAuthSegredo); err != nil {
			return fmt.Errorf("upstream: cifrar client_secret de %d: %w", upstreamID, err)
		}
	}

	agora := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO upstream_oauth (upstream_id, client_id, client_secret_cifrado, issuer, criado_em, atualizado_em)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (upstream_id) DO UPDATE SET
    client_id             = excluded.client_id,
    client_secret_cifrado = excluded.client_secret_cifrado,
    issuer                = excluded.issuer,
    atualizado_em         = excluded.atualizado_em`,
		upstreamID, f.OAuthClientID, segredo, f.OAuthIssuer, agora, agora); err != nil {
		return fmt.Errorf("upstream: gravar cliente OAuth de %d: %w", upstreamID, err)
	}

	if anterior.existe && anterior.clientID != f.OAuthClientID {
		if _, err := tx.ExecContext(ctx, `
UPDATE upstream_oauth
   SET access_token_cifrado  = '',
       refresh_token_cifrado = '',
       tipo_token            = '',
       expira_em             = NULL,
       client_id_efetivo     = '',
       registro              = '',
       url_token             = ''
 WHERE upstream_id = ?`, upstreamID); err != nil {
			return fmt.Errorf("upstream: invalidar concessão de %d: %w", upstreamID, err)
		}
	}
	return nil
}

// clienteOAuthAnterior é o que já estava gravado, sem decifrar: o segredo é
// reaproveitado como texto cifrado, então nunca precisa voltar em claro aqui.
type clienteOAuthAnterior struct {
	existe   bool
	clientID string
	segredo  string
}

func clienteOAuthDaTx(ctx context.Context, tx *sql.Tx, upstreamID int64) (clienteOAuthAnterior, error) {
	var a clienteOAuthAnterior
	err := tx.QueryRowContext(ctx,
		`SELECT client_id, client_secret_cifrado FROM upstream_oauth WHERE upstream_id = ?`,
		upstreamID).Scan(&a.clientID, &a.segredo)
	if errors.Is(err, sql.ErrNoRows) {
		return clienteOAuthAnterior{}, nil
	}
	if err != nil {
		return clienteOAuthAnterior{}, fmt.Errorf("upstream: ler cliente OAuth de %d: %w", upstreamID, err)
	}
	a.existe = true
	return a, nil
}

// instanteSQL traduz o instante zero em NULL: "sem prazo declarado" é diferente
// de "venceu em 1970".
func instanteSQL(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func decodificarEscopos(bruto string) ([]string, error) {
	if bruto == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(bruto), &out); err != nil {
		return nil, fmt.Errorf("decodificar escopos: %w", err)
	}
	return out, nil
}

func codificarEscopos(escopos []string) (string, error) {
	if escopos == nil {
		escopos = []string{}
	}
	b, err := json.Marshal(escopos)
	if err != nil {
		return "", fmt.Errorf("upstream: codificar escopos: %w", err)
	}
	return string(b), nil
}
