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

const sqlRegras = `
SELECT upstream_id, acao, padrao, renome
  FROM endpoint_tool_rule
 WHERE endpoint_id = ?
 ORDER BY upstream_id, ordem, id`

// Vinculos devolve os upstreams habilitados do endpoint, na ordem da composição,
// cada um com o prefixo e as regras daquele endpoint.
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

	// Duas consultas e não um LEFT JOIN: o join multiplicaria a linha do vínculo
	// pelo número de regras e a montagem teria de desduplicar upstream a
	// upstream. Duas leituras curtas num pool sem limite custam menos que isso —
	// e nenhuma delas está no caminho da requisição do cliente.
	regras, err := c.regrasDe(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Regras = regras[out[i].UpstreamID]
	}
	return out, nil
}

// regrasDe devolve as regras do endpoint agrupadas por upstream, na ordem em que
// o admin as escreveu — que é a ordem em que Aplicar as avalia.
func (c *ComposicaoSQLite) regrasDe(ctx context.Context, endpointID int64) (map[int64][]Regra, error) {
	rows, err := c.leitura.QueryContext(ctx, sqlRegras, endpointID)
	if err != nil {
		return nil, fmt.Errorf("selecionar regras: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][]Regra)
	for rows.Next() {
		var (
			upstreamID int64
			acao       string
			r          Regra
		)
		// acao entra como string e não como Acao: o database/sql não converte
		// para tipo nomeado sem um sql.Scanner, e um Scanner aqui seria
		// cerimônia para uma conversão de uma linha.
		if err := rows.Scan(&upstreamID, &acao, &r.Padrao, &r.Renome); err != nil {
			return nil, fmt.Errorf("ler regra: %w", err)
		}
		r.Acao = Acao(acao)
		out[upstreamID] = append(out[upstreamID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterar regras: %w", err)
	}
	return out, nil
}
