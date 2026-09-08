package upstream

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const sqlHabilitados = `
SELECT id, nome, tipo, url, timeout_ms
  FROM upstream
 WHERE habilitado = 1
 ORDER BY nome`

// Habilitados lê do banco os upstreams que o admin quer no ar.
//
// habilitado é a intenção do admin e é o único estado persistido de um
// upstream: o estado da máquina vive em memória, e reiniciar o patchbay sempre
// recomeça em novo → conectando.
func Habilitados(ctx context.Context, leitura *sql.DB) ([]Config, error) {
	rows, err := leitura.QueryContext(ctx, sqlHabilitados)
	if err != nil {
		return nil, fmt.Errorf("upstream: selecionar habilitados: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Config
	for rows.Next() {
		var (
			c         Config
			timeoutMS int64
		)
		if err := rows.Scan(&c.ID, &c.Nome, &c.Tipo, &c.URL, &timeoutMS); err != nil {
			return nil, fmt.Errorf("upstream: ler linha: %w", err)
		}
		c.Timeout = time.Duration(timeoutMS) * time.Millisecond
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar habilitados: %w", err)
	}
	return out, nil
}
