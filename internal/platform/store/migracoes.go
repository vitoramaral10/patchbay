package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// arquivosMigracao carrega os .sql versionados para dentro do binário: o deploy
// não depende de CLI externa nem de arquivo no disco.
//
//go:embed migracoes/*.sql
var arquivosMigracao embed.FS

// Migrar aplica as migrações pendentes usando o pool de escrita. Recebe o pool
// de escrita de propósito: migração é DDL e não pode competir com outra conexão.
func Migrar(ctx context.Context, escrita *sql.DB) error {
	// goose casa "*.sql" na raiz do fs.FS que recebe, não recursivamente.
	raiz, err := fs.Sub(arquivosMigracao, "migracoes")
	if err != nil {
		return fmt.Errorf("store: fs de migrações: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, escrita, raiz,
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("store: provider de migração: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: aplicar migrações: %w", err)
	}
	return nil
}
