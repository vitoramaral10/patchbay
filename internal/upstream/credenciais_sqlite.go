package upstream

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// Credenciais devolve as credenciais estáticas de um upstream, em claro.
//
// É o que o gerente chama ao abrir a sessão. Ordem estável — bearer antes dos
// headers, headers por nome — para que o conjunto de headers de saída não
// dependa da ordem em que as linhas saíram do banco.
func (r *RepositorioSQLite) Credenciais(ctx context.Context, upstreamID int64) ([]Credencial, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT tipo, nome, valor_cifrado
  FROM upstream_secret
 WHERE upstream_id = ?
 ORDER BY tipo, nome`, upstreamID)
	if err != nil {
		return nil, fmt.Errorf("upstream: selecionar credenciais de %d: %w", upstreamID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Credencial
	for rows.Next() {
		var c Credencial
		var cifrado string
		if err := rows.Scan(&c.Tipo, &c.Nome, &cifrado); err != nil {
			return nil, fmt.Errorf("upstream: ler credencial de %d: %w", upstreamID, err)
		}
		c.Valor, err = r.cifrador.Decifrar(campoDe(upstreamID, c.Tipo, c.Nome), cifrado)
		if err != nil {
			// A mensagem nomeia o slot, nunca o valor: é o suficiente para o
			// operador saber qual credencial regravar.
			return nil, fmt.Errorf("upstream: decifrar credencial %s/%s de %d: %w",
				c.Tipo, c.Nome, upstreamID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar credenciais de %d: %w", upstreamID, err)
	}
	return out, nil
}

// CredenciaisDefinidas devolve o que existe gravado, sem decifrar nada.
//
// É o que a UI usa: ela precisa saber que há um bearer definido e quais headers
// existem, e não precisa — nem pode — do valor. Não decifrar também significa
// que a tela de upstream continua abrindo depois de uma troca de chave mestra,
// em vez de virar um 500.
func (r *RepositorioSQLite) CredenciaisDefinidas(ctx context.Context, upstreamID int64) ([]CredencialDefinida, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT tipo, nome
  FROM upstream_secret
 WHERE upstream_id = ?
 ORDER BY tipo, nome`, upstreamID)
	if err != nil {
		return nil, fmt.Errorf("upstream: selecionar credenciais definidas de %d: %w", upstreamID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CredencialDefinida
	for rows.Next() {
		var d CredencialDefinida
		if err := rows.Scan(&d.Tipo, &d.Nome); err != nil {
			return nil, fmt.Errorf("upstream: ler credencial definida de %d: %w", upstreamID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar credenciais definidas de %d: %w", upstreamID, err)
	}
	return out, nil
}

// aplicarCredenciais grava, substitui e apaga as credenciais que o formulário
// pediu, na mesma transação do upstream.
//
// Valor vazio significa "manter o que está gravado": o formulário nunca reexibe
// o valor, então um campo em branco é ausência de mudança, não ordem de apagar.
// Apagar é explícito, pelo "limpar".
func (r *RepositorioSQLite) aplicarCredenciais(ctx context.Context, tx *sql.Tx, upstreamID int64, f Form) error {
	if f.BearerLimpar {
		if err := apagarCredencial(ctx, tx, upstreamID, CredencialBearer, ""); err != nil {
			return err
		}
	} else if !f.Bearer.Vazio() {
		if err := r.gravarCredencial(ctx, tx, upstreamID, CredencialBearer, "", f.Bearer); err != nil {
			return err
		}
	}

	for _, h := range f.Headers {
		if h.Nome == "" {
			continue
		}
		if h.Limpar {
			if err := apagarCredencial(ctx, tx, upstreamID, CredencialHeader, h.Nome); err != nil {
				return err
			}
			continue
		}
		if h.Valor.Vazio() {
			continue
		}
		if err := r.gravarCredencial(ctx, tx, upstreamID, CredencialHeader, h.Nome, h.Valor); err != nil {
			return err
		}
	}
	return nil
}

func (r *RepositorioSQLite) gravarCredencial(
	ctx context.Context, tx *sql.Tx, upstreamID int64, tipo, nome string, valor cripto.Segredo,
) error {
	cifrado, err := r.cifrador.Cifrar(campoDe(upstreamID, tipo, nome), valor)
	if err != nil {
		return fmt.Errorf("upstream: cifrar credencial %s/%s de %d: %w", tipo, nome, upstreamID, err)
	}
	agora := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `
INSERT INTO upstream_secret (upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (upstream_id, tipo, nome) DO UPDATE SET
    valor_cifrado = excluded.valor_cifrado,
    atualizado_em = excluded.atualizado_em`,
		upstreamID, tipo, nome, cifrado, agora, agora)
	if err != nil {
		return fmt.Errorf("upstream: gravar credencial %s/%s de %d: %w", tipo, nome, upstreamID, err)
	}
	return nil
}

func apagarCredencial(ctx context.Context, tx *sql.Tx, upstreamID int64, tipo, nome string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM upstream_secret WHERE upstream_id = ? AND tipo = ? AND nome = ?`,
		upstreamID, tipo, nome)
	if err != nil {
		return fmt.Errorf("upstream: apagar credencial %s/%s de %d: %w", tipo, nome, upstreamID, err)
	}
	return nil
}
