package authsrv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var sqlTodosClientes = `SELECT ` + colunasClienteLista + `
  FROM oauth_client
 ORDER BY revogado_em IS NOT NULL, criado_em DESC, id DESC`

// TodosClientes devolve os clientes cadastrados, ativos primeiro.
//
// Sem o hash do segredo: esta é a listagem, e carregar a credencial de cada
// cliente cadastrado para montar uma tabela que nunca a usa é superfície sem
// necessidade — a tela de detalhe (ClientePorID) é quem lê a coluna inteira.
func (r *RepositorioSQLite) TodosClientes(ctx context.Context) ([]Cliente, error) {
	rows, err := r.leitura.QueryContext(ctx, sqlTodosClientes)
	if err != nil {
		return nil, fmt.Errorf("authsrv: selecionar clientes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Cliente
	for rows.Next() {
		c, err := lerClienteLista(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("authsrv: ler cliente: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authsrv: iterar clientes: %w", err)
	}

	// Escopo e allowlist linha a linha: são poucos clientes numa instalação
	// pessoal, e um JOIN com agregação de texto trocaria clareza por nada.
	for i := range out {
		if err := r.completar(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

var sqlClientePorID = `SELECT ` + colunasCliente + ` FROM oauth_client WHERE id = ?`

// ClientePorID devolve um cliente, revogado ou não — a tela de detalhe precisa
// mostrar o revogado para que "revogar" não seja indistinguível de "apaguei".
func (r *RepositorioSQLite) ClientePorID(ctx context.Context, id int64) (Cliente, error) {
	c, err := lerCliente(r.leitura.QueryRowContext(ctx, sqlClientePorID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Cliente{}, ErrClienteNaoEncontrado
	}
	if err != nil {
		return Cliente{}, fmt.Errorf("authsrv: selecionar cliente %d: %w", id, err)
	}
	if err := r.completar(ctx, &c); err != nil {
		return Cliente{}, err
	}
	return c, nil
}

// CriarCliente cadastra um cliente com a allowlist e o escopo, numa transação.
//
// Transação porque cliente sem redirect não consegue autorizar nada e cliente
// sem endpoint não consegue pedir token: se a segunda escrita falhar, a primeira
// não pode ficar.
func (r *RepositorioSQLite) CriarCliente(
	ctx context.Context, f FormCliente, clientID, segredoPrefixo, segredoHash string, agora time.Time,
) (Cliente, error) {
	if len(f.RedirectURIs) == 0 {
		return Cliente{}, ErrSemRedirect
	}
	if len(f.EndpointIDs) == 0 {
		return Cliente{}, ErrSemEndpoint
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return Cliente{}, fmt.Errorf("authsrv: abrir transação de cadastro: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	confidencial := 0
	if f.Confidencial {
		confidencial = 1
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO oauth_client (client_id, nome, tipo, confidencial, segredo_hash, segredo_prefixo, criado_em)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id`,
		clientID, f.Nome, TipoPrereg, confidencial, segredoHash, segredoPrefixo, agora.Unix()).Scan(&id)
	if err != nil {
		return Cliente{}, fmt.Errorf("authsrv: gravar cliente %s: %w", f.Nome, err)
	}
	for _, uri := range f.RedirectURIs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO oauth_client_redirect (client_id_ref, redirect_uri) VALUES (?, ?)`,
			id, uri); err != nil {
			return Cliente{}, fmt.Errorf("authsrv: gravar redirect do cliente %d: %w", id, err)
		}
	}
	for _, endpointID := range f.EndpointIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO oauth_client_endpoint (client_id_ref, endpoint_id) VALUES (?, ?)`,
			id, endpointID); err != nil {
			return Cliente{}, fmt.Errorf("authsrv: dar escopo em %d ao cliente %d: %w", endpointID, id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Cliente{}, fmt.Errorf("authsrv: confirmar cadastro do cliente %s: %w", f.Nome, err)
	}
	return r.ClientePorID(ctx, id)
}

// RevogarCliente marca o cliente como revogado e derruba tudo o que ele tinha.
//
// Marca, não apaga: a linha revogada é o que permite a tela dizer que aquele
// client_id existiu. E revoga os tokens junto, porque revogar o cliente sem
// revogar o que ele já emitiu deixaria as sessões vivas até expirarem.
func (r *RepositorioSQLite) RevogarCliente(ctx context.Context, id int64, agora time.Time) error {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authsrv: abrir transação de revogação de cliente: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_client SET revogado_em = ? WHERE id = ? AND revogado_em IS NULL`,
		agora.Unix(), id); err != nil {
		return fmt.Errorf("authsrv: revogar cliente %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_token SET revogado_em = ? WHERE client_id_ref = ? AND revogado_em IS NULL`,
		agora.Unix(), id); err != nil {
		return fmt.Errorf("authsrv: revogar tokens do cliente %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_code SET usado_em = ? WHERE client_id_ref = ? AND usado_em IS NULL`,
		agora.Unix(), id); err != nil {
		return fmt.Errorf("authsrv: queimar códigos do cliente %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authsrv: confirmar revogação do cliente %d: %w", id, err)
	}
	return nil
}

const sqlSessoesDoCliente = `
SELECT t.familia_id, e.slug, MIN(t.resource), MIN(t.criado_em), MAX(t.expira_em),
       MAX(COALESCE(t.ultimo_uso_em, 0)),
       SUM(CASE WHEN t.revogado_em IS NULL THEN 1 ELSE 0 END),
       COUNT(*)
  FROM oauth_token t
  JOIN endpoint e ON e.id = t.endpoint_id
 WHERE t.client_id_ref = ?
 GROUP BY t.familia_id, e.slug
 ORDER BY MIN(t.criado_em) DESC`

// SessoesDoCliente devolve as famílias de token daquele cliente.
func (r *RepositorioSQLite) SessoesDoCliente(ctx context.Context, clienteID int64) ([]Sessao, error) {
	rows, err := r.leitura.QueryContext(ctx, sqlSessoesDoCliente, clienteID)
	if err != nil {
		return nil, fmt.Errorf("authsrv: selecionar sessões do cliente %d: %w", clienteID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Sessao
	for rows.Next() {
		var (
			s        Sessao
			criadaEm int64
			expiraEm int64
			ultimo   int64
		)
		if err := rows.Scan(&s.FamiliaID, &s.EndpointSlug, &s.Resource,
			&criadaEm, &expiraEm, &ultimo, &s.Ativos, &s.Total); err != nil {
			return nil, fmt.Errorf("authsrv: ler sessão do cliente %d: %w", clienteID, err)
		}
		s.CriadaEm = time.Unix(criadaEm, 0).UTC()
		s.ExpiraEm = time.Unix(expiraEm, 0).UTC()
		if ultimo > 0 {
			s.UltimoUsoEm = time.Unix(ultimo, 0).UTC()
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authsrv: iterar sessões do cliente %d: %w", clienteID, err)
	}
	return out, nil
}

// FamiliaDoCliente confirma que a família pertence ao cliente antes de revogar.
//
// Sem essa conferência, o id da família na URL viraria uma forma de revogar
// sessão de outro cliente digitando o identificador certo.
func (r *RepositorioSQLite) FamiliaDoCliente(ctx context.Context, clienteID int64, familiaID string) (bool, error) {
	var n int
	err := r.leitura.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM oauth_token WHERE client_id_ref = ? AND familia_id = ?`,
		clienteID, familiaID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("authsrv: conferir família %s do cliente %d: %w", familiaID, clienteID, err)
	}
	return n > 0, nil
}
