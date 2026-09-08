package apikey

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RepositorioSQLite lê a chave do SQLite. O pool de leitura e o de escrita são
// separados porque o SQLite aceita um escritor por vez (seção 08.8).
type RepositorioSQLite struct {
	leitura *sql.DB
	escrita *sql.DB
}

// NovoRepositorioSQLite monta o repositório sobre os dois pools.
func NovoRepositorioSQLite(leitura, escrita *sql.DB) *RepositorioSQLite {
	return &RepositorioSQLite{leitura: leitura, escrita: escrita}
}

const sqlPorHash = `
SELECT k.id, k.nome, k.prefixo_visivel, k.revogado_em, k.ultimo_uso_em
  FROM api_key k
 WHERE k.hash = ?`

const sqlEndpointsDaChave = `
SELECT e.slug
  FROM api_key_endpoint ke
  JOIN endpoint e ON e.id = ke.endpoint_id
 WHERE ke.api_key_id = ?
 ORDER BY e.slug`

// nulo é um instante opcional do banco: as colunas de data são INTEGER com
// segundos de epoch e NULL quando o evento não aconteceu.
type nulo struct{ sql.NullInt64 }

func (n nulo) instante() time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}

// PorHash busca a chave pelo hash de armazenamento.
//
// O lookup é por hash porque é o índice único que toda requisição usa; o hash
// de um segredo de 256 bits não é adivinhável, então a comparação feita pelo
// índice não abre canal de tempo útil.
func (r *RepositorioSQLite) PorHash(ctx context.Context, hash string) (Chave, error) {
	var (
		c           Chave
		revogadoEm  sql.NullInt64
		ultimoUsoEm sql.NullInt64
	)
	err := r.leitura.QueryRowContext(ctx, sqlPorHash, hash).
		Scan(&c.ID, &c.Nome, &c.PrefixoVisivel, &revogadoEm, &ultimoUsoEm)
	if errors.Is(err, sql.ErrNoRows) {
		return Chave{}, ErrNaoEncontrada
	}
	if err != nil {
		return Chave{}, fmt.Errorf("apikey: selecionar por hash: %w", err)
	}
	if revogadoEm.Valid {
		c.RevogadaEm = time.Unix(revogadoEm.Int64, 0).UTC()
	}
	if ultimoUsoEm.Valid {
		c.UltimoUsoEm = time.Unix(ultimoUsoEm.Int64, 0).UTC()
	}

	slugs, err := r.endpoints(ctx, c.ID)
	if err != nil {
		return Chave{}, err
	}
	c.Endpoints = slugs
	return c, nil
}

func (r *RepositorioSQLite) endpoints(ctx context.Context, id int64) ([]string, error) {
	rows, err := r.leitura.QueryContext(ctx, sqlEndpointsDaChave, id)
	if err != nil {
		return nil, fmt.Errorf("apikey: selecionar endpoints da chave %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("apikey: ler endpoint da chave %d: %w", id, err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apikey: iterar endpoints da chave %d: %w", id, err)
	}
	return slugs, nil
}

// RegistrarUso grava o instante do último uso.
func (r *RepositorioSQLite) RegistrarUso(ctx context.Context, id int64, quando time.Time) error {
	_, err := r.escrita.ExecContext(ctx,
		`UPDATE api_key SET ultimo_uso_em = ? WHERE id = ?`, quando.Unix(), id)
	if err != nil {
		return fmt.Errorf("apikey: gravar último uso da chave %d: %w", id, err)
	}
	return nil
}
