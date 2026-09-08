package endpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RepositorioSQLite lê e escreve endpoints nos dois pools.
type RepositorioSQLite struct {
	leitura *sql.DB
	escrita *sql.DB
}

// NovoRepositorioSQLite monta o repositório de endpoints.
//
// escrita pode ser nil quando o repositório só serve o caminho MCP; o CRUD da UI
// precisa dos dois pools.
func NovoRepositorioSQLite(leitura, escrita *sql.DB) *RepositorioSQLite {
	return &RepositorioSQLite{leitura: leitura, escrita: escrita}
}

const colunas = `id, slug, nome, descricao, instrucoes`

// Todos devolve todos os endpoints cadastrados, em ordem de slug.
func (r *RepositorioSQLite) Todos(ctx context.Context) ([]Registro, error) {
	rows, err := r.leitura.QueryContext(ctx,
		`SELECT `+colunas+` FROM endpoint ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("selecionar endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Registro
	for rows.Next() {
		var reg Registro
		if err := rows.Scan(&reg.ID, &reg.Slug, &reg.Nome, &reg.Descricao, &reg.Instrucoes); err != nil {
			return nil, fmt.Errorf("ler endpoint: %w", err)
		}
		out = append(out, reg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar endpoints: %w", err)
	}
	return out, nil
}

// Obter devolve um endpoint pelo id.
func (r *RepositorioSQLite) Obter(ctx context.Context, id int64) (Registro, error) {
	var reg Registro
	err := r.leitura.QueryRowContext(ctx,
		`SELECT `+colunas+` FROM endpoint WHERE id = ?`, id).
		Scan(&reg.ID, &reg.Slug, &reg.Nome, &reg.Descricao, &reg.Instrucoes)
	if errors.Is(err, sql.ErrNoRows) {
		return Registro{}, ErrNaoEncontrado
	}
	if err != nil {
		return Registro{}, fmt.Errorf("selecionar endpoint %d: %w", id, err)
	}
	return reg, nil
}

// ContagemDeUpstreams devolve quantos upstreams compõem cada endpoint.
func (r *RepositorioSQLite) ContagemDeUpstreams(ctx context.Context) (map[int64]int, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT endpoint_id, COUNT(*) FROM endpoint_upstream GROUP BY endpoint_id`)
	if err != nil {
		return nil, fmt.Errorf("contar upstreams por endpoint: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]int)
	for rows.Next() {
		var (
			id int64
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("ler contagem de upstreams: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar contagem de upstreams: %w", err)
	}
	return out, nil
}

// UpstreamsDo devolve os ids dos upstreams que compõem o endpoint.
func (r *RepositorioSQLite) UpstreamsDo(ctx context.Context, id int64) ([]int64, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT upstream_id FROM endpoint_upstream WHERE endpoint_id = ? ORDER BY ordem, upstream_id`, id)
	if err != nil {
		return nil, fmt.Errorf("selecionar composição do endpoint %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var upstreamID int64
		if err := rows.Scan(&upstreamID); err != nil {
			return nil, fmt.Errorf("ler composição do endpoint %d: %w", id, err)
		}
		out = append(out, upstreamID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar composição do endpoint %d: %w", id, err)
	}
	return out, nil
}

// Criar grava o endpoint e a composição numa transação.
//
// Uma transação porque endpoint sem a composição que o admin escolheu é um
// endpoint que serve tools/list vazio sem ninguém ter pedido isso.
func (r *RepositorioSQLite) Criar(ctx context.Context, f Form) (int64, error) {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("abrir transação de endpoint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO endpoint (slug, nome, descricao, instrucoes, criado_em)
VALUES (?, ?, ?, ?, ?)
RETURNING id`,
		f.Slug, f.Nome, f.Descricao, f.Instrucoes, time.Now().Unix()).Scan(&id)
	if err != nil {
		if slugEmUso(ctx, r.leitura, f.Slug) {
			return 0, ErrSlugEmUso
		}
		return 0, fmt.Errorf("gravar endpoint %s: %w", f.Slug, err)
	}
	if err := gravarComposicao(ctx, tx, id, f.UpstreamIDs); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("confirmar endpoint %s: %w", f.Slug, err)
	}
	return id, nil
}

// Atualizar grava nome, descrição, instruções e composição.
//
// O slug não está no UPDATE de propósito: ele entra na URL, na metadata RFC 9728
// e no aud de todo token daquele endpoint, e trocá-lo invalidaria em silêncio a
// credencial de todo cliente (seção 10).
func (r *RepositorioSQLite) Atualizar(ctx context.Context, id int64, f Form) error {
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abrir transação de endpoint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE endpoint SET nome = ?, descricao = ?, instrucoes = ? WHERE id = ?`,
		f.Nome, f.Descricao, f.Instrucoes, id)
	if err != nil {
		return fmt.Errorf("atualizar endpoint %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNaoEncontrado
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM endpoint_upstream WHERE endpoint_id = ?`, id); err != nil {
		return fmt.Errorf("limpar composição do endpoint %d: %w", id, err)
	}
	if err := gravarComposicao(ctx, tx, id, f.UpstreamIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirmar endpoint %d: %w", id, err)
	}
	return nil
}

// Remover apaga o endpoint. As linhas de composição e de escopo de chave saem
// por ON DELETE CASCADE.
func (r *RepositorioSQLite) Remover(ctx context.Context, id int64) error {
	res, err := r.escrita.ExecContext(ctx, `DELETE FROM endpoint WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("apagar endpoint %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNaoEncontrado
	}
	return nil
}

func gravarComposicao(ctx context.Context, tx *sql.Tx, endpointID int64, upstreamIDs []int64) error {
	// A ordem da composição é a ordem em que o admin marcou, e ela decide qual
	// upstream ganha o nome quando dois expõem a mesma ferramenta.
	for ordem, upstreamID := range slices.Compact(slices.Sorted(slices.Values(upstreamIDs))) {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO endpoint_upstream (endpoint_id, upstream_id, prefixo, ordem)
VALUES (?, ?, '', ?)`, endpointID, upstreamID, ordem); err != nil {
			return fmt.Errorf("compor endpoint %d com upstream %d: %w", endpointID, upstreamID, err)
		}
	}
	return nil
}

func slugEmUso(ctx context.Context, leitura *sql.DB, slug string) bool {
	var n int
	if err := leitura.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM endpoint WHERE slug = ?`, slug).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// NormalizarSlug baixa a caixa e tira o espaço em volta. Não conserta caractere
// inválido de propósito: o slug é contrato, e "consertar" em silêncio faria o
// admin achar que cadastrou um slug que não é o que está na URL.
func NormalizarSlug(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
