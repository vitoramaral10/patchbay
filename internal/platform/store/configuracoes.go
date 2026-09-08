package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Configuracoes é o acesso à tabela settings: a configuração do processo que
// vive no banco, em chave/valor.
//
// Fica em platform porque não é assunto de nenhuma feature — o canário da chave
// mestra, a URL pública e o generation do import de YAML dividem a tabela sem
// dividir domínio.
type Configuracoes struct {
	leitura *sql.DB
	escrita *sql.DB
}

// NovasConfiguracoes monta o acesso a settings sobre os dois pools.
func NovasConfiguracoes(leitura, escrita *sql.DB) *Configuracoes {
	return &Configuracoes{leitura: leitura, escrita: escrita}
}

// Ler devolve o valor de uma chave. O segundo retorno é falso quando ela não
// existe, que não é erro: "ainda não configurado" é um estado normal.
func (c *Configuracoes) Ler(ctx context.Context, chave string) (string, bool, error) {
	var valor string
	err := c.leitura.QueryRowContext(ctx,
		`SELECT valor FROM settings WHERE chave = ?`, chave).Scan(&valor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: ler configuração %s: %w", chave, err)
	}
	return valor, true, nil
}

// Gravar cria ou substitui o valor de uma chave.
func (c *Configuracoes) Gravar(ctx context.Context, chave, valor string) error {
	_, err := c.escrita.ExecContext(ctx, `
INSERT INTO settings (chave, valor, atualizado_em)
VALUES (?, ?, ?)
ON CONFLICT (chave) DO UPDATE SET
    valor = excluded.valor,
    atualizado_em = excluded.atualizado_em`,
		chave, valor, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: gravar configuração %s: %w", chave, err)
	}
	return nil
}
