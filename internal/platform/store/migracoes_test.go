package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// A 00017 reconstrói a tabela upstream para alargar o CHECK de modo_credencial,
// e reconstruir a tabela-mãe de quatro filhas com ON DELETE CASCADE é o tipo de
// migração que só falha em banco com dado — que é exatamente o que os outros
// testes não têm, porque abrem o banco já migrado até o fim.
//
// Este teste para no 16, povoa upstream e as filhas, e só então aplica a 17.
func TestMigracao00017_PreservaFilhasEAceitaModoNenhum(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	caminho := filepath.Join(t.TempDir(), NomeArquivo)
	db, err := abrirParaMigracao(t, ctx, caminho)
	if err != nil {
		t.Fatalf("abrir: erro = %v, quer nil", err)
	}

	p := provider(t, db)
	if _, err := p.UpTo(ctx, 16); err != nil {
		t.Fatalf("UpTo(16): erro = %v, quer nil", err)
	}

	popular(t, ctx, db)

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("Up: erro = %v, quer nil", err)
	}

	// As filhas continuam lá: se o DROP TABLE tivesse rodado com foreign_keys
	// ligado, o ON DELETE CASCADE teria levado as três.
	for _, c := range []struct {
		tabela string
		sql    string
	}{
		{"endpoint_upstream", `SELECT count(*) FROM endpoint_upstream WHERE upstream_id = 1`},
		{"upstream_secret", `SELECT count(*) FROM upstream_secret WHERE upstream_id = 1`},
		{"upstream_oauth", `SELECT count(*) FROM upstream_oauth WHERE upstream_id = 1`},
	} {
		var n int
		if err := db.QueryRowContext(ctx, c.sql).Scan(&n); err != nil {
			t.Fatalf("contar %s: erro = %v, quer nil", c.tabela, err)
		}
		if n != 1 {
			t.Errorf("%s: linhas = %d, quer 1", c.tabela, n)
		}
	}

	// O upstream sobreviveu com o modo que tinha: a migração não adivinha
	// intenção de cadastro antigo.
	var modo string
	if err := db.QueryRowContext(ctx,
		`SELECT modo_credencial FROM upstream WHERE id = 1`).Scan(&modo); err != nil {
		t.Fatalf("ler modo: erro = %v, quer nil", err)
	}
	if modo != "estatica" {
		t.Errorf("modo_credencial = %q, quer \"estatica\"", modo)
	}

	// O CHECK novo aceita nenhum, e continua recusando o que ninguém entende.
	if _, err := db.ExecContext(ctx,
		`UPDATE upstream SET modo_credencial = 'nenhum' WHERE id = 1`); err != nil {
		t.Errorf("gravar modo nenhum: erro = %v, quer nil", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE upstream SET modo_credencial = 'inventado' WHERE id = 1`); err == nil {
		t.Error("gravar modo inventado: erro = nil, quer violação do CHECK")
	}

	// E a cascata volta a funcionar depois da troca: é o que prova que as filhas
	// resolveram de novo para a tabela nova, e não ficaram apontando para nada.
	if _, err := db.ExecContext(ctx, `DELETE FROM upstream WHERE id = 1`); err != nil {
		t.Fatalf("apagar upstream: erro = %v, quer nil", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM upstream_secret`).Scan(&n); err != nil {
		t.Fatalf("contar upstream_secret: erro = %v, quer nil", err)
	}
	if n != 0 {
		t.Errorf("upstream_secret depois da cascata: linhas = %d, quer 0", n)
	}
}

func abrirParaMigracao(t *testing.T, ctx context.Context, caminho string) (*sql.DB, error) {
	t.Helper()
	db, err := abrirPool(ctx, caminho, true)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, nil
}

func provider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()
	raiz, err := fs.Sub(arquivosMigracao, "migracoes")
	if err != nil {
		t.Fatalf("fs de migrações: erro = %v, quer nil", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, raiz,
		goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatalf("provider: erro = %v, quer nil", err)
	}
	return p
}

// popular grava um upstream e uma linha em cada tabela que o referencia.
func popular(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	comandos := []string{
		`INSERT INTO upstream (id, nome, tipo, url, timeout_ms, habilitado, criado_em, modo_credencial)
		 VALUES (1, 'alvo', 'http', 'https://exemplo.com/mcp', 15000, 1, 0, 'estatica')`,
		`INSERT INTO endpoint (id, slug, criado_em) VALUES (1, 'principal', 0)`,
		`INSERT INTO endpoint_upstream (endpoint_id, upstream_id) VALUES (1, 1)`,
		`INSERT INTO upstream_secret (upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em)
		 VALUES (1, 'bearer', '', 'cifrado', 0, 0)`,
		`INSERT INTO upstream_oauth (upstream_id, criado_em, atualizado_em) VALUES (1, 0, 0)`,
	}
	for _, c := range comandos {
		if _, err := db.ExecContext(ctx, c); err != nil {
			t.Fatalf("popular (%s): erro = %v, quer nil", c, err)
		}
	}
}
