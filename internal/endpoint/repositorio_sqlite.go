package endpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
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

// ItemComposicao é um upstream dentro da composição de um endpoint, com o
// prefixo e as regras que valem só ali.
type ItemComposicao struct {
	UpstreamID int64
	Prefixo    string
	Regras     []catalogo.Regra
}

// ComposicaoDe devolve a composição do endpoint na ordem gravada.
func (r *RepositorioSQLite) ComposicaoDe(ctx context.Context, id int64) ([]ItemComposicao, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT upstream_id, prefixo FROM endpoint_upstream WHERE endpoint_id = ? ORDER BY ordem, upstream_id`, id)
	if err != nil {
		return nil, fmt.Errorf("selecionar composição do endpoint %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ItemComposicao
	for rows.Next() {
		var item ItemComposicao
		if err := rows.Scan(&item.UpstreamID, &item.Prefixo); err != nil {
			return nil, fmt.Errorf("ler composição do endpoint %d: %w", id, err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar composição do endpoint %d: %w", id, err)
	}

	regras, err := r.regrasDe(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Regras = regras[out[i].UpstreamID]
	}
	return out, nil
}

func (r *RepositorioSQLite) regrasDe(ctx context.Context, id int64) (map[int64][]catalogo.Regra, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT upstream_id, acao, padrao, renome
  FROM endpoint_tool_rule
 WHERE endpoint_id = ?
 ORDER BY upstream_id, ordem, id`, id)
	if err != nil {
		return nil, fmt.Errorf("selecionar regras do endpoint %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][]catalogo.Regra)
	for rows.Next() {
		var (
			upstreamID int64
			acao       string
			regra      catalogo.Regra
		)
		if err := rows.Scan(&upstreamID, &acao, &regra.Padrao, &regra.Renome); err != nil {
			return nil, fmt.Errorf("ler regra do endpoint %d: %w", id, err)
		}
		regra.Acao = catalogo.Acao(acao)
		out[upstreamID] = append(out[upstreamID], regra)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar regras do endpoint %d: %w", id, err)
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
	if err := gravarComposicao(ctx, tx, id, f); err != nil {
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
	// As regras saem à mão: elas referenciam o endpoint, não a linha de
	// endpoint_upstream, então apagar a composição não as leva junto por
	// cascata. Sem este DELETE, desmarcar um upstream e remarcá-lo depois traria
	// de volta um filtro que o admin já tinha apagado.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM endpoint_tool_rule WHERE endpoint_id = ?`, id); err != nil {
		return fmt.Errorf("limpar regras do endpoint %d: %w", id, err)
	}
	if err := gravarComposicao(ctx, tx, id, f); err != nil {
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

func gravarComposicao(ctx context.Context, tx *sql.Tx, endpointID int64, f Form) error {
	// A ordem da composição é a ordem em que o admin marcou, e ela decide qual
	// upstream ganha o nome quando dois expõem a mesma ferramenta.
	for ordem, upstreamID := range slices.Compact(slices.Sorted(slices.Values(f.UpstreamIDs))) {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO endpoint_upstream (endpoint_id, upstream_id, prefixo, ordem)
VALUES (?, ?, ?, ?)`, endpointID, upstreamID, f.Prefixo(upstreamID), ordem); err != nil {
			return fmt.Errorf("compor endpoint %d com upstream %d: %w", endpointID, upstreamID, err)
		}
		if err := gravarRegras(ctx, tx, endpointID, upstreamID, f.Regras(upstreamID)); err != nil {
			return err
		}
	}
	return nil
}

// gravarRegras grava as regras de um upstream naquele endpoint, na ordem em que
// o admin as escreveu — que é a ordem em que elas são avaliadas.
func gravarRegras(ctx context.Context, tx *sql.Tx, endpointID, upstreamID int64, regras []catalogo.Regra) error {
	for ordem, regra := range regras {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO endpoint_tool_rule (endpoint_id, upstream_id, ordem, acao, padrao, renome)
VALUES (?, ?, ?, ?, ?, ?)`,
			endpointID, upstreamID, ordem, string(regra.Acao), regra.Padrao, regra.Renome); err != nil {
			return fmt.Errorf("gravar regra %d do endpoint %d com upstream %d: %w",
				ordem, endpointID, upstreamID, err)
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
