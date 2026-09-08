package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RepositorioSQLite guarda o admin e as sessões nos dois pools.
type RepositorioSQLite struct {
	leitura *sql.DB
	escrita *sql.DB
}

// NovoRepositorioSQLite monta o repositório do admin.
func NovoRepositorioSQLite(leitura, escrita *sql.DB) *RepositorioSQLite {
	return &RepositorioSQLite{leitura: leitura, escrita: escrita}
}

// Existe informa se o admin único já foi cadastrado.
func (r *RepositorioSQLite) Existe(ctx context.Context) (bool, error) {
	var n int
	if err := r.leitura.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin`).Scan(&n); err != nil {
		return false, fmt.Errorf("admin: contar administradores: %w", err)
	}
	return n > 0, nil
}

// Criar grava o admin único.
//
// O id fixo em 1 casa com o CHECK (id = 1) da migração: a segunda tentativa
// falha no banco, não numa checagem do Go que uma corrida poderia furar.
func (r *RepositorioSQLite) Criar(ctx context.Context, usuario, senhaHash string, agora time.Time) (Admin, error) {
	ts := agora.Unix()
	_, err := r.escrita.ExecContext(ctx, `
INSERT INTO admin (id, usuario, senha_hash, criado_em, atualizado_em)
VALUES (1, ?, ?, ?, ?)`, usuario, senhaHash, ts, ts)
	if err != nil {
		// UNIQUE/CHECK: só existe uma linha possível, então qualquer recusa de
		// inserção aqui é "já tem admin".
		if jaExiste(ctx, r.leitura) {
			return Admin{}, ErrJaExiste
		}
		return Admin{}, fmt.Errorf("admin: gravar administrador: %w", err)
	}
	return Admin{ID: 1, Usuario: usuario, CriadoEm: time.Unix(ts, 0).UTC()}, nil
}

func jaExiste(ctx context.Context, leitura *sql.DB) bool {
	var n int
	if err := leitura.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// PorUsuario devolve o admin e o hash da senha.
func (r *RepositorioSQLite) PorUsuario(ctx context.Context, usuario string) (Admin, string, error) {
	var (
		a        Admin
		hash     string
		criadoEm int64
	)
	err := r.leitura.QueryRowContext(ctx, `
SELECT id, usuario, senha_hash, criado_em FROM admin WHERE usuario = ?`, usuario).
		Scan(&a.ID, &a.Usuario, &hash, &criadoEm)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, "", ErrCredencial
	}
	if err != nil {
		return Admin{}, "", fmt.Errorf("admin: selecionar administrador: %w", err)
	}
	a.CriadoEm = time.Unix(criadoEm, 0).UTC()
	return a, hash, nil
}

// CriarSessao grava a sessão pelo hash do token.
func (r *RepositorioSQLite) CriarSessao(ctx context.Context, hash string, adminID int64, criadaEm, expiraEm time.Time) error {
	_, err := r.escrita.ExecContext(ctx, `
INSERT INTO sessao_admin (hash, admin_id, criado_em, expira_em, ultimo_uso_em)
VALUES (?, ?, ?, ?, ?)`, hash, adminID, criadaEm.Unix(), expiraEm.Unix(), criadaEm.Unix())
	if err != nil {
		return fmt.Errorf("admin: gravar sessão: %w", err)
	}
	return nil
}

// SessaoPorHash devolve a sessão viva, com o admin dono dela.
//
// A expiração é comparada no SQL: sessão vencida é indistinguível de sessão
// inexistente para quem chama, e é isso que evita um caminho de código em que
// "expirada" vira "válida" por descuido.
func (r *RepositorioSQLite) SessaoPorHash(ctx context.Context, hash string, agora time.Time) (Sessao, error) {
	var (
		s         Sessao
		criadoEm  int64
		expiraEm  int64
		adminData int64
	)
	err := r.leitura.QueryRowContext(ctx, `
SELECT s.criado_em, s.expira_em, a.id, a.usuario, a.criado_em
  FROM sessao_admin s
  JOIN admin a ON a.id = s.admin_id
 WHERE s.hash = ? AND s.expira_em > ?`, hash, agora.Unix()).
		Scan(&criadoEm, &expiraEm, &s.Admin.ID, &s.Admin.Usuario, &adminData)
	if errors.Is(err, sql.ErrNoRows) {
		return Sessao{}, ErrSessao
	}
	if err != nil {
		return Sessao{}, fmt.Errorf("admin: selecionar sessão: %w", err)
	}
	s.CriadaEm = time.Unix(criadoEm, 0).UTC()
	s.ExpiraEm = time.Unix(expiraEm, 0).UTC()
	s.Admin.CriadoEm = time.Unix(adminData, 0).UTC()
	return s, nil
}

// ApagarSessao encerra uma sessão.
func (r *RepositorioSQLite) ApagarSessao(ctx context.Context, hash string) error {
	if _, err := r.escrita.ExecContext(ctx, `DELETE FROM sessao_admin WHERE hash = ?`, hash); err != nil {
		return fmt.Errorf("admin: apagar sessão: %w", err)
	}
	return nil
}

// ApagarSessoesExpiradas limpa o que venceu e devolve quantas linhas saíram.
func (r *RepositorioSQLite) ApagarSessoesExpiradas(ctx context.Context, agora time.Time) (int64, error) {
	res, err := r.escrita.ExecContext(ctx,
		`DELETE FROM sessao_admin WHERE expira_em <= ?`, agora.Unix())
	if err != nil {
		return 0, fmt.Errorf("admin: apagar sessões expiradas: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("admin: contar sessões apagadas: %w", err)
	}
	return n, nil
}
