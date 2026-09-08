package apikey

import (
	"context"
	"fmt"
	"time"
)

const sqlTodas = `
SELECT id, nome, prefixo_visivel, criado_em, revogado_em, ultimo_uso_em
  FROM api_key
 ORDER BY revogado_em IS NOT NULL, criado_em DESC, id DESC`

// Todas devolve as chaves emitidas, ativas primeiro e mais novas antes.
//
// Nunca devolve segredo: só o prefixo visível, que é o que permite a UI dizer
// qual chave é qual sem guardar a chave.
func (r *RepositorioSQLite) Todas(ctx context.Context) ([]Chave, error) {
	rows, err := r.leitura.QueryContext(ctx, sqlTodas)
	if err != nil {
		return nil, fmt.Errorf("apikey: selecionar chaves: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Chave
	for rows.Next() {
		var (
			c           Chave
			criadoEm    int64
			revogadoEm  nulo
			ultimoUsoEm nulo
		)
		if err := rows.Scan(&c.ID, &c.Nome, &c.PrefixoVisivel, &criadoEm, &revogadoEm, &ultimoUsoEm); err != nil {
			return nil, fmt.Errorf("apikey: ler chave: %w", err)
		}
		c.CriadaEm = time.Unix(criadoEm, 0).UTC()
		c.RevogadaEm = revogadoEm.instante()
		c.UltimoUsoEm = ultimoUsoEm.instante()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apikey: iterar chaves: %w", err)
	}

	// O escopo é lido chave por chave: são poucas chaves numa instalação
	// pessoal, e um JOIN com agregação de texto trocaria clareza por nada.
	for i := range out {
		slugs, err := r.endpoints(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Endpoints = slugs
	}
	return out, nil
}

// Emitir grava uma chave nova com o escopo escolhido, numa transação.
//
// Transação porque chave sem escopo é chave que não abre nada: se a segunda
// escrita falhar, a primeira não pode ficar.
func (r *RepositorioSQLite) Emitir(ctx context.Context, nome string, e Emitida, endpointIDs []int64) (Chave, error) {
	if len(endpointIDs) == 0 {
		return Chave{}, ErrNenhumEndpoint
	}
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return Chave{}, fmt.Errorf("apikey: abrir transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	agora := time.Now().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO api_key (nome, hash, prefixo_visivel, criado_em)
VALUES (?, ?, ?, ?)
RETURNING id`, nome, e.Hash, e.PrefixoVisivel, agora).Scan(&id)
	if err != nil {
		return Chave{}, fmt.Errorf("apikey: gravar chave %s: %w", nome, err)
	}
	for _, endpointID := range endpointIDs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO api_key_endpoint (api_key_id, endpoint_id) VALUES (?, ?)`, id, endpointID); err != nil {
			return Chave{}, fmt.Errorf("apikey: dar escopo em %d à chave %d: %w", endpointID, id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Chave{}, fmt.Errorf("apikey: confirmar chave %s: %w", nome, err)
	}

	slugs, err := r.endpoints(ctx, id)
	if err != nil {
		return Chave{}, err
	}
	return Chave{
		ID:             id,
		Nome:           nome,
		PrefixoVisivel: e.PrefixoVisivel,
		Endpoints:      slugs,
		CriadaEm:       time.Unix(agora, 0).UTC(),
	}, nil
}

// Revogar marca a chave como revogada.
//
// Marca, não apaga: a linha revogada é o que permite a UI dizer que aquele
// prefixo existiu e quando foi usado por último. Revogar duas vezes mantém a
// primeira data.
func (r *RepositorioSQLite) Revogar(ctx context.Context, id int64) error {
	res, err := r.escrita.ExecContext(ctx,
		`UPDATE api_key SET revogado_em = ? WHERE id = ? AND revogado_em IS NULL`,
		time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("apikey: revogar chave %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Ou não existe, ou já estava revogada: as duas devolvem o mesmo, porque
		// o resultado desejado ("esta chave não abre mais nada") já vale.
		return nil
	}
	return nil
}
