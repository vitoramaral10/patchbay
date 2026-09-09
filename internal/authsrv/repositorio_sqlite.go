package authsrv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RepositorioSQLite guarda o estado do authorization server.
//
// Dois pools pelo mesmo motivo do resto do patchbay: o SQLite aceita um escritor
// por vez, e a rotação de refresh é justamente uma escrita que não pode
// intercalar com outra (seção 08.8).
type RepositorioSQLite struct {
	leitura *sql.DB
	escrita *sql.DB
}

// NovoRepositorioSQLite monta o repositório sobre os dois pools.
func NovoRepositorioSQLite(leitura, escrita *sql.DB) *RepositorioSQLite {
	return &RepositorioSQLite{leitura: leitura, escrita: escrita}
}

// nulo é um instante opcional do banco: as colunas de data são INTEGER com
// segundos de epoch e NULL quando o evento não aconteceu.
type nulo struct{ sql.NullInt64 }

func (n nulo) instante() time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}

// colunasCliente é a lista única de colunas de oauth_client lida onde o hash do
// segredo importa — a leitura por id ou por client_id, que alimentam a
// autenticação e a tela de detalhe. Uma lista só porque escanear a mesma tabela
// com duas listas diferentes é como uma coluna nova entra em metade dos
// caminhos.
const colunasCliente = `id, client_id, nome, tipo, confidencial, segredo_hash,
       segredo_prefixo, criado_em, expira_em, revogado_em, origem, escopo_aberto`

// lerCliente escaneia uma linha de oauth_client na ordem de colunasCliente.
func lerCliente(escanear func(...any) error) (Cliente, error) {
	var (
		c            Cliente
		confidencial int
		abertoInt    int
		criadoEm     int64
		expiraEm     nulo
		revogadoEm   nulo
	)
	if err := escanear(&c.ID, &c.ClientID, &c.Nome, &c.Tipo, &confidencial,
		&c.segredoHash, &c.SegredoPrefixo, &criadoEm, &expiraEm, &revogadoEm,
		&c.Origem, &abertoInt); err != nil {
		return Cliente{}, err
	}
	c.Confidencial = confidencial == 1
	c.EscopoAberto = abertoInt == 1
	c.CriadoEm = time.Unix(criadoEm, 0).UTC()
	c.ExpiraEm = expiraEm.instante()
	c.RevogadoEm = revogadoEm.instante()
	return c, nil
}

// colunasClienteLista é colunasCliente sem segredo_hash: a tela de listagem
// mostra todo cliente cadastrado, e trazer o hash do segredo de cada linha para
// a memória do processo é superfície que aquela tela não usa — só a de detalhe
// e a autenticação precisam dele.
const colunasClienteLista = `id, client_id, nome, tipo, confidencial,
       segredo_prefixo, criado_em, expira_em, revogado_em, origem, escopo_aberto`

// lerClienteLista escaneia uma linha na ordem de colunasClienteLista. c.segredoHash
// fica vazio: quem lê por aqui nunca autentica ninguém.
func lerClienteLista(escanear func(...any) error) (Cliente, error) {
	var (
		c            Cliente
		confidencial int
		abertoInt    int
		criadoEm     int64
		expiraEm     nulo
		revogadoEm   nulo
	)
	if err := escanear(&c.ID, &c.ClientID, &c.Nome, &c.Tipo, &confidencial,
		&c.SegredoPrefixo, &criadoEm, &expiraEm, &revogadoEm,
		&c.Origem, &abertoInt); err != nil {
		return Cliente{}, err
	}
	c.Confidencial = confidencial == 1
	c.EscopoAberto = abertoInt == 1
	c.CriadoEm = time.Unix(criadoEm, 0).UTC()
	c.ExpiraEm = expiraEm.instante()
	c.RevogadoEm = revogadoEm.instante()
	return c, nil
}

var sqlClientePorClientID = `SELECT ` + colunasCliente + ` FROM oauth_client WHERE client_id = ?`

// ClientePorClientID devolve o cliente ativo com o escopo e a allowlist
// carregados.
func (r *RepositorioSQLite) ClientePorClientID(ctx context.Context, clientID string) (Cliente, error) {
	c, err := r.ClienteMesmoRevogado(ctx, clientID)
	if err != nil {
		return Cliente{}, err
	}
	if c.Revogado() {
		// Cliente revogado é indistinguível de inexistente para quem chama: as
		// duas respostas são o mesmo invalid_client.
		return Cliente{}, ErrClienteNaoEncontrado
	}
	return c, nil
}

// ClienteMesmoRevogado devolve o cliente pelo client_id inclusive revogado ou
// com o cache de CIMD vencido.
//
// Existe pela resolução de CIMD: ela precisa distinguir "nunca vi este
// documento" de "o admin revogou este cliente", e a segunda não pode virar uma
// busca de saída nova. O expira_em vem no Cliente para que a decisão de
// rebuscar seja do serviço, e não do SQL.
func (r *RepositorioSQLite) ClienteMesmoRevogado(ctx context.Context, clientID string) (Cliente, error) {
	c, err := lerCliente(r.leitura.QueryRowContext(ctx, sqlClientePorClientID, clientID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Cliente{}, ErrClienteNaoEncontrado
	}
	if err != nil {
		return Cliente{}, fmt.Errorf("authsrv: selecionar cliente %q: %w", clientID, err)
	}
	if err := r.completar(ctx, &c); err != nil {
		return Cliente{}, err
	}
	return c, nil
}

// completar carrega a allowlist e o escopo de um cliente já escaneado.
func (r *RepositorioSQLite) completar(ctx context.Context, c *Cliente) error {
	var err error
	if c.RedirectURIs, err = r.redirects(ctx, c.ID); err != nil {
		return err
	}
	// Escopo aberto (todo registro dinâmico) resolve na leitura, e não em linhas
	// gravadas no registro: gravar deixaria o cliente cego para todo endpoint
	// criado depois dele.
	if c.EscopoAberto {
		c.Endpoints, err = r.todosEndpoints(ctx)
		return err
	}
	c.Endpoints, err = r.endpointsDoCliente(ctx, c.ID)
	return err
}

const sqlTodosEndpoints = `SELECT id, slug, nome FROM endpoint ORDER BY slug`

func (r *RepositorioSQLite) todosEndpoints(ctx context.Context) ([]EndpointRef, error) {
	rows, err := r.leitura.QueryContext(ctx, sqlTodosEndpoints)
	if err != nil {
		return nil, fmt.Errorf("authsrv: selecionar endpoints do escopo aberto: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EndpointRef
	for rows.Next() {
		var e EndpointRef
		if err := rows.Scan(&e.ID, &e.Slug, &e.Nome); err != nil {
			return nil, fmt.Errorf("authsrv: ler endpoint do escopo aberto: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authsrv: iterar endpoints do escopo aberto: %w", err)
	}
	return out, nil
}

func (r *RepositorioSQLite) redirects(ctx context.Context, id int64) ([]string, error) {
	rows, err := r.leitura.QueryContext(ctx,
		`SELECT redirect_uri FROM oauth_client_redirect WHERE client_id_ref = ? ORDER BY redirect_uri`, id)
	if err != nil {
		return nil, fmt.Errorf("authsrv: selecionar redirects do cliente %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, fmt.Errorf("authsrv: ler redirect do cliente %d: %w", id, err)
		}
		out = append(out, uri)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authsrv: iterar redirects do cliente %d: %w", id, err)
	}
	return out, nil
}

const sqlEndpointsDoCliente = `
SELECT e.id, e.slug, e.nome
  FROM oauth_client_endpoint ce
  JOIN endpoint e ON e.id = ce.endpoint_id
 WHERE ce.client_id_ref = ?
 ORDER BY e.slug`

func (r *RepositorioSQLite) endpointsDoCliente(ctx context.Context, id int64) ([]EndpointRef, error) {
	rows, err := r.leitura.QueryContext(ctx, sqlEndpointsDoCliente, id)
	if err != nil {
		return nil, fmt.Errorf("authsrv: selecionar endpoints do cliente %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []EndpointRef
	for rows.Next() {
		var e EndpointRef
		if err := rows.Scan(&e.ID, &e.Slug, &e.Nome); err != nil {
			return nil, fmt.Errorf("authsrv: ler endpoint do cliente %d: %w", id, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authsrv: iterar endpoints do cliente %d: %w", id, err)
	}
	return out, nil
}

// --- código de autorização ---

// GravarCodigo grava um código recém-emitido.
func (r *RepositorioSQLite) GravarCodigo(ctx context.Context, c Codigo) error {
	_, err := r.escrita.ExecContext(ctx, `
INSERT INTO oauth_code (code_hash, familia_id, client_id_ref, endpoint_id, resource,
                        escopo, redirect_uri, code_challenge, criado_em, expira_em)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Hash, c.FamiliaID, c.ClienteID, c.EndpointID, c.Resource,
		c.Escopo, c.RedirectURI, c.CodeChallenge, c.CriadoEm.Unix(), c.ExpiraEm.Unix())
	if err != nil {
		return fmt.Errorf("authsrv: gravar código de autorização: %w", err)
	}
	return nil
}

const sqlCodigoPorHash = `
SELECT familia_id, client_id_ref, endpoint_id, resource, escopo, redirect_uri,
       code_challenge, criado_em, expira_em, usado_em
  FROM oauth_code
 WHERE code_hash = ?`

// ConsumirCodigo marca o código como usado numa escrita condicional.
//
// A escrita vem antes da leitura de propósito: é o UPDATE ... WHERE usado_em IS
// NULL que decide quem foi o primeiro, e fazer a leitura antes deixaria a janela
// em que duas trocas simultâneas leem "não usado" e ambas passam. O pool de
// escrita tem uma conexão só, então o segundo UPDATE espera pelo primeiro.
func (r *RepositorioSQLite) ConsumirCodigo(ctx context.Context, hash string, agora time.Time) (Codigo, bool, error) {
	res, err := r.escrita.ExecContext(ctx,
		`UPDATE oauth_code SET usado_em = ? WHERE code_hash = ? AND usado_em IS NULL`,
		agora.Unix(), hash)
	if err != nil {
		return Codigo{}, false, fmt.Errorf("authsrv: consumir código: %w", err)
	}
	afetadas, err := res.RowsAffected()
	if err != nil {
		return Codigo{}, false, fmt.Errorf("authsrv: conferir consumo do código: %w", err)
	}

	var (
		c        Codigo
		criadoEm int64
		expiraEm int64
		usadoEm  nulo
	)
	c.Hash = hash
	err = r.leitura.QueryRowContext(ctx, sqlCodigoPorHash, hash).Scan(
		&c.FamiliaID, &c.ClienteID, &c.EndpointID, &c.Resource, &c.Escopo,
		&c.RedirectURI, &c.CodeChallenge, &criadoEm, &expiraEm, &usadoEm)
	if errors.Is(err, sql.ErrNoRows) {
		return Codigo{}, false, ErrCodigoNaoEncontrado
	}
	if err != nil {
		return Codigo{}, false, fmt.Errorf("authsrv: selecionar código: %w", err)
	}
	c.CriadoEm = time.Unix(criadoEm, 0).UTC()
	c.ExpiraEm = time.Unix(expiraEm, 0).UTC()
	c.UsadoEm = usadoEm.instante()

	return c, afetadas == 1, nil
}

// --- tokens ---

// G101 marca as duas consultas abaixo como credencial embutida por causa do
// nome da tabela: são SQL, e o único valor literal nelas é a interrogação do
// parâmetro.
//
//nolint:gosec // G101: SQL parametrizado, sem credencial no texto
const sqlInserirToken = `
INSERT INTO oauth_token (hash, tipo, familia_id, client_id_ref, endpoint_id,
                         resource, escopo, criado_em, expira_em)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id`

func inserirToken(ctx context.Context, tx *sql.Tx, t Token) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, sqlInserirToken,
		t.Hash, t.Tipo, t.FamiliaID, t.ClienteID, t.EndpointID,
		t.Resource, t.Escopo, t.CriadoEm.Unix(), t.ExpiraEm.Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("authsrv: gravar token %s: %w", t.Tipo, err)
	}
	return id, nil
}

// GravarPar grava o access e o refresh emitidos juntos.
//
// Transação porque um access sem o refresh correspondente é uma sessão que morre
// em uma hora sem o cliente entender por quê.
func (r *RepositorioSQLite) GravarPar(ctx context.Context, acesso, refresh Token) error {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authsrv: abrir transação de emissão: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := inserirToken(ctx, tx, acesso); err != nil {
		return err
	}
	if _, err := inserirToken(ctx, tx, refresh); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authsrv: confirmar emissão de token: %w", err)
	}
	return nil
}

//nolint:gosec // G101: SQL parametrizado, sem credencial no texto
const sqlTokenPorHash = `
SELECT t.id, t.tipo, t.familia_id, t.client_id_ref, t.endpoint_id, t.resource,
       t.escopo, t.substituido_por, t.criado_em, t.expira_em, t.revogado_em,
       t.ultimo_uso_em, c.client_id, c.revogado_em
  FROM oauth_token t
  JOIN oauth_client c ON c.id = t.client_id_ref
 WHERE t.hash = ?`

// TokenPorHash busca o token pelo hash de armazenamento, com o estado do cliente
// dono junto — é a única ida ao banco do caminho de toda requisição a
// /mcp/{slug}.
func (r *RepositorioSQLite) TokenPorHash(ctx context.Context, hash string) (Token, error) {
	var (
		t              Token
		substituidoPor nulo
		criadoEm       int64
		expiraEm       int64
		revogadoEm     nulo
		ultimoUsoEm    nulo
		clienteRevogou nulo
	)
	t.Hash = hash
	err := r.leitura.QueryRowContext(ctx, sqlTokenPorHash, hash).Scan(
		&t.ID, &t.Tipo, &t.FamiliaID, &t.ClienteID, &t.EndpointID, &t.Resource,
		&t.Escopo, &substituidoPor, &criadoEm, &expiraEm, &revogadoEm,
		&ultimoUsoEm, &t.ClienteClientID, &clienteRevogou)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrTokenNaoEncontrado
	}
	if err != nil {
		return Token{}, fmt.Errorf("authsrv: selecionar token: %w", err)
	}
	t.SubstituidoPor = substituidoPor.Int64
	t.CriadoEm = time.Unix(criadoEm, 0).UTC()
	t.ExpiraEm = time.Unix(expiraEm, 0).UTC()
	t.RevogadoEm = revogadoEm.instante()
	t.UltimoUsoEm = ultimoUsoEm.instante()
	t.ClienteRevogado = clienteRevogou.Valid
	return t, nil
}

// Rotacionar troca um refresh por um par novo, na mesma família.
//
// O UPDATE condicional é a detecção de replay: se o refresh já tinha sido
// substituído ou revogado, nenhuma linha é afetada e nada é gravado — o serviço
// lê o falso e queima a família.
func (r *RepositorioSQLite) Rotacionar(
	ctx context.Context, antigoID int64, acesso, refresh Token, agora time.Time,
) (bool, error) {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("authsrv: abrir transação de rotação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE oauth_token
   SET ultimo_uso_em = ?
 WHERE id = ? AND tipo = 'refresh' AND substituido_por IS NULL AND revogado_em IS NULL`,
		agora.Unix(), antigoID)
	if err != nil {
		return false, fmt.Errorf("authsrv: reservar refresh %d para rotação: %w", antigoID, err)
	}
	afetadas, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("authsrv: conferir reserva do refresh %d: %w", antigoID, err)
	}
	if afetadas != 1 {
		return false, nil
	}

	if _, err := inserirToken(ctx, tx, acesso); err != nil {
		return false, err
	}
	novoID, err := inserirToken(ctx, tx, refresh)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_token SET substituido_por = ?, revogado_em = ? WHERE id = ?`,
		novoID, agora.Unix(), antigoID); err != nil {
		return false, fmt.Errorf("authsrv: encadear rotação do refresh %d: %w", antigoID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("authsrv: confirmar rotação: %w", err)
	}
	return true, nil
}

// RevogarFamilia queima todos os tokens de uma família e os códigos que ainda
// pudessem emitir mais.
func (r *RepositorioSQLite) RevogarFamilia(ctx context.Context, familiaID string, agora time.Time) error {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authsrv: abrir transação de revogação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_token SET revogado_em = ? WHERE familia_id = ? AND revogado_em IS NULL`,
		agora.Unix(), familiaID); err != nil {
		return fmt.Errorf("authsrv: revogar tokens da família %s: %w", familiaID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_code SET usado_em = ? WHERE familia_id = ? AND usado_em IS NULL`,
		agora.Unix(), familiaID); err != nil {
		return fmt.Errorf("authsrv: queimar códigos da família %s: %w", familiaID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authsrv: confirmar revogação da família %s: %w", familiaID, err)
	}
	return nil
}

// RevogarToken revoga um token só.
func (r *RepositorioSQLite) RevogarToken(ctx context.Context, id int64, agora time.Time) error {
	if _, err := r.escrita.ExecContext(ctx,
		`UPDATE oauth_token SET revogado_em = ? WHERE id = ? AND revogado_em IS NULL`,
		agora.Unix(), id); err != nil {
		return fmt.Errorf("authsrv: revogar token %d: %w", id, err)
	}
	return nil
}

// RegistrarUsoToken grava o instante do último uso.
func (r *RepositorioSQLite) RegistrarUsoToken(ctx context.Context, id int64, quando time.Time) error {
	if _, err := r.escrita.ExecContext(ctx,
		`UPDATE oauth_token SET ultimo_uso_em = ? WHERE id = ?`, quando.Unix(), id); err != nil {
		return fmt.Errorf("authsrv: gravar último uso do token %d: %w", id, err)
	}
	return nil
}

// LimparExpirados apaga o que já não autoriza nada.
//
// O corte é o expira_em, e não o criado_em: um refresh vencido continua sendo a
// prova de que aquela família existiu, e apagá-lo cedo demais apagaria a
// detecção de replay junto.
func (r *RepositorioSQLite) LimparExpirados(ctx context.Context, antesDe time.Time) error {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authsrv: abrir transação de limpeza: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_code WHERE expira_em < ?`, antesDe.Unix()); err != nil {
		return fmt.Errorf("authsrv: limpar códigos vencidos: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_token WHERE expira_em < ?`, antesDe.Unix()); err != nil {
		return fmt.Errorf("authsrv: limpar tokens vencidos: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authsrv: confirmar limpeza: %w", err)
	}
	return nil
}
