package catalogo

import (
	"context"
	"database/sql"
	"fmt"
)

// ComposicaoSQLite lê a composição endpoint↔upstream do pool de leitura.
type ComposicaoSQLite struct {
	leitura *sql.DB
}

// NovaComposicaoSQLite monta o leitor de composição.
func NovaComposicaoSQLite(leitura *sql.DB) *ComposicaoSQLite {
	return &ComposicaoSQLite{leitura: leitura}
}

const sqlVinculos = `
SELECT u.id, u.nome, eu.prefixo
  FROM endpoint_upstream eu
  JOIN upstream u ON u.id = eu.upstream_id
 WHERE eu.endpoint_id = ?
   AND u.habilitado = 1
 ORDER BY eu.ordem, u.nome`

// Vinculos devolve os upstreams habilitados do endpoint, na ordem da composição.
func (c *ComposicaoSQLite) Vinculos(ctx context.Context, endpointID int64) ([]Vinculo, error) {
	rows, err := c.leitura.QueryContext(ctx, sqlVinculos, endpointID)
	if err != nil {
		return nil, fmt.Errorf("selecionar vínculos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Vinculo
	for rows.Next() {
		var v Vinculo
		if err := rows.Scan(&v.UpstreamID, &v.UpstreamNome, &v.Prefixo); err != nil {
			return nil, fmt.Errorf("ler vínculo: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar vínculos: %w", err)
	}
	return out, nil
}
