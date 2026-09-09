package authsrv

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClienteDinamico é o que o repositório precisa para gravar um cliente que se
// registrou sozinho — por DCR, ou como cache de um documento de CIMD.
//
// É struct de entrada e não o próprio Cliente porque o que entra e o que sai são
// conjuntos diferentes de campos: aqui não há ID nem escopo (é sempre aberto), e
// há o hash do segredo, que Cliente não expõe.
type ClienteDinamico struct {
	ClientID       string
	Nome           string
	Tipo           string
	Confidencial   bool
	SegredoHash    string
	SegredoPrefixo string
	RedirectURIs   []string
	Origem         string
	CriadoEm       time.Time
	// ExpiraEm só é preenchido no cache de CIMD. Zero grava NULL, que é
	// "registro, não expira".
	ExpiraEm time.Time
}

// RegistrarClienteDinamico grava um cliente vindo de DCR, com a allowlist,
// contando o teto de registros na mesma transação da escrita.
//
// A contagem entra na tx do INSERT, e não numa consulta separada no pool de
// leitura, porque é isso que fecha a corrida entre duas requisições
// concorrentes: o SQLite tem um escritor só, então a segunda só chega a contar
// depois que a primeira já commitou (ou não) a linha dela. Contar antes de
// abrir a tx — como era — deixava as duas lerem o mesmo total e as duas
// passarem, furando o teto por dois.
//
// escopo_aberto = 1 e nenhuma linha em oauth_client_endpoint: ninguém escolheu
// endpoint por este cliente, e a autorização de verdade é o consentimento, que
// acontece uma vez por endpoint na sessão do admin.
func (r *RepositorioSQLite) RegistrarClienteDinamico(
	ctx context.Context, d ClienteDinamico, origem string, desde time.Time, tetoOrigem, tetoTotal int,
) (Cliente, error) {
	if len(d.RedirectURIs) == 0 {
		return Cliente{}, ErrSemRedirect
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return Cliente{}, fmt.Errorf("authsrv: abrir transação de registro dinâmico: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	porOrigem, total, err := contarRegistrosDinamicosTx(ctx, tx, origem, desde)
	if err != nil {
		return Cliente{}, err
	}
	if porOrigem >= tetoOrigem || total >= tetoTotal {
		return Cliente{}, ErrRegistroExcedido
	}

	id, err := inserirClienteDinamico(ctx, tx, d)
	if err != nil {
		return Cliente{}, err
	}
	if err := gravarRedirects(ctx, tx, id, d.RedirectURIs); err != nil {
		return Cliente{}, err
	}
	if err := tx.Commit(); err != nil {
		return Cliente{}, fmt.Errorf("authsrv: confirmar registro dinâmico de %s: %w", d.ClientID, err)
	}
	return r.ClientePorID(ctx, id)
}

// SalvarCacheCIMD grava ou atualiza o cache de um documento de CIMD.
//
// Atualizar substitui a allowlist inteira pela do documento recém-lido, em vez
// de acumular: o documento é a fonte da verdade, e uma redirect_uri que o dono
// do cliente removeu do documento não pode continuar valendo aqui.
func (r *RepositorioSQLite) SalvarCacheCIMD(ctx context.Context, d ClienteDinamico) (Cliente, error) {
	if len(d.RedirectURIs) == 0 {
		return Cliente{}, ErrSemRedirect
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return Cliente{}, fmt.Errorf("authsrv: abrir transação de cache de cimd: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM oauth_client WHERE client_id = ?`, d.ClientID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if id, err = inserirClienteDinamico(ctx, tx, d); err != nil {
			return Cliente{}, err
		}
		if err := gravarRedirects(ctx, tx, id, d.RedirectURIs); err != nil {
			return Cliente{}, err
		}
	case err != nil:
		return Cliente{}, fmt.Errorf("authsrv: procurar cache de cimd de %s: %w", d.ClientID, err)
	default:
		if _, err := tx.ExecContext(ctx, `
UPDATE oauth_client
   SET nome = ?, expira_em = ?, origem = ?, escopo_aberto = 1
 WHERE id = ? AND tipo = 'cimd_cache' AND revogado_em IS NULL`,
			d.Nome, instanteOuNulo(d.ExpiraEm), d.Origem, id); err != nil {
			return Cliente{}, fmt.Errorf("authsrv: atualizar cache de cimd de %s: %w", d.ClientID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM oauth_client_redirect WHERE client_id_ref = ?`, id); err != nil {
			return Cliente{}, fmt.Errorf("authsrv: limpar allowlist do cache %d: %w", id, err)
		}
		if err := gravarRedirects(ctx, tx, id, d.RedirectURIs); err != nil {
			return Cliente{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Cliente{}, fmt.Errorf("authsrv: confirmar cache de cimd de %s: %w", d.ClientID, err)
	}
	return r.ClientePorID(ctx, id)
}

//nolint:gosec // G101: SQL parametrizado, sem credencial no texto
const sqlInserirClienteDinamico = `
INSERT INTO oauth_client (client_id, nome, tipo, confidencial, segredo_hash,
                          segredo_prefixo, criado_em, expira_em, origem, escopo_aberto)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
RETURNING id`

func inserirClienteDinamico(ctx context.Context, tx *sql.Tx, d ClienteDinamico) (int64, error) {
	confidencial := 0
	if d.Confidencial {
		confidencial = 1
	}
	var id int64
	err := tx.QueryRowContext(ctx, sqlInserirClienteDinamico,
		d.ClientID, d.Nome, d.Tipo, confidencial, d.SegredoHash, d.SegredoPrefixo,
		d.CriadoEm.Unix(), instanteOuNulo(d.ExpiraEm), d.Origem).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("authsrv: gravar cliente dinâmico %s: %w", d.ClientID, err)
	}
	return id, nil
}

func gravarRedirects(ctx context.Context, tx *sql.Tx, id int64, uris []string) error {
	for _, uri := range uris {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO oauth_client_redirect (client_id_ref, redirect_uri) VALUES (?, ?)`,
			id, uri); err != nil {
			return fmt.Errorf("authsrv: gravar redirect do cliente %d: %w", id, err)
		}
	}
	return nil
}

// instanteOuNulo traduz o instante zero em NULL: é a diferença entre "não
// expira" e "expirou em 1970".
func instanteOuNulo(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

const sqlContarRegistros = `
SELECT COUNT(*), SUM(CASE WHEN origem = ? THEN 1 ELSE 0 END)
  FROM oauth_client
 WHERE tipo = ? AND criado_em >= ?`

// contarRegistrosDinamicosTx conta os registros de DCR feitos desde um
// instante, na conexão da transação — e não no pool de leitura — para que a
// contagem e o INSERT que a segue enxerguem o mesmo estado.
//
// Uma consulta só para os dois números: são o mesmo predicado com uma
// discriminação a mais, e duas idas ao banco por registro seriam duas chances de
// a contagem não fechar com ela mesma.
func contarRegistrosDinamicosTx(
	ctx context.Context, tx *sql.Tx, origem string, desde time.Time,
) (porOrigem, total int, err error) {
	var daOrigem sql.NullInt64
	err = tx.QueryRowContext(ctx, sqlContarRegistros, origem, TipoDCR, desde.Unix()).
		Scan(&total, &daOrigem)
	if err != nil {
		return 0, 0, fmt.Errorf("authsrv: contar registros dinâmicos de %q: %w", origem, err)
	}
	return int(daOrigem.Int64), total, nil
}

// ContarCacheCIMD conta as linhas de cache de CIMD *criadas* desde um
// instante — não atualizadas: criado_em só muda no INSERT, então isto conta
// documentos novos, nunca a renovação de um que já existia.
//
// É o teto de TetoCacheCIMDPorHora: sem ele, quem escolhe o client_id (que é
// uma URL, em CIMD) força o processo a buscar e cachear um documento novo por
// identificador diferente, sem limite algum.
func (r *RepositorioSQLite) ContarCacheCIMD(ctx context.Context, desde time.Time) (int, error) {
	var total int
	err := r.leitura.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM oauth_client WHERE tipo = ? AND criado_em >= ?`,
		TipoCIMD, desde.Unix()).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("authsrv: contar cache de cimd criado: %w", err)
	}
	return total, nil
}

// LimparCacheCIMD apaga o cache de documento vencido que não deixou token.
//
// Vencido e sem token é cache morto: nenhuma sessão depende dele, e um
// consentimento novo rebusca o documento. Cache que *deixou* token fica, porque
// a linha é o cliente dono daquele token — apagá-la levaria os tokens junto pelo
// ON DELETE CASCADE, revogando sessão viva por conta de uma varredura.
//
// revogado_em IS NULL exclui o cliente que o admin revogou: a linha revogada é
// lápide, não cache morto. Sem essa condição, a varredura apagava a lápide e
// resolverCIMD, não achando mais nada em ClienteMesmoRevogado, rebuscava o
// documento e recriava o cliente — a revogação durava até a limpeza rodar.
func (r *RepositorioSQLite) LimparCacheCIMD(ctx context.Context, antesDe time.Time) error {
	_, err := r.escrita.ExecContext(ctx, `
DELETE FROM oauth_client
 WHERE tipo = ?
   AND expira_em IS NOT NULL
   AND expira_em < ?
   AND revogado_em IS NULL
   AND NOT EXISTS (SELECT 1 FROM oauth_token t WHERE t.client_id_ref = oauth_client.id)
   AND NOT EXISTS (SELECT 1 FROM oauth_code c WHERE c.client_id_ref = oauth_client.id)`,
		TipoCIMD, antesDe.Unix())
	if err != nil {
		return fmt.Errorf("authsrv: limpar cache de cimd vencido: %w", err)
	}
	return nil
}
