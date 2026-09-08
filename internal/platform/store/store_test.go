package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

func abrir(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Abrir: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: erro = %v, quer nil", err)
		}
	})
	return st
}

func TestAbrir_RecusaDiretorioVazio(t *testing.T) {
	t.Parallel()

	_, err := store.Abrir(context.Background(), "")
	if !errors.Is(err, store.ErrDiretorioVazio) {
		t.Fatalf("erro = %v, quer %v", err, store.ErrDiretorioVazio)
	}
}

func TestAbrir_AplicaMigracoes(t *testing.T) {
	t.Parallel()

	st := abrir(t)

	tabelas := []string{
		"upstream", "endpoint", "endpoint_upstream", "api_key", "api_key_endpoint",
	}
	for _, tabela := range tabelas {
		var nome string
		err := st.Leitura().QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, tabela).Scan(&nome)
		if err != nil {
			t.Errorf("tabela %s: erro = %v, quer nil", tabela, err)
		}
	}
}

func TestAbrir_Idempotente(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 2 {
		st, err := store.Abrir(context.Background(), dir)
		if err != nil {
			t.Fatalf("Abrir na volta %d: erro = %v, quer nil", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close na volta %d: erro = %v, quer nil", i, err)
		}
	}
}

func TestAbrir_PragmasEmVigor(t *testing.T) {
	t.Parallel()

	st := abrir(t)
	ctx := context.Background()

	casos := map[string]struct {
		pragma string
		quer   string
	}{
		"journal em WAL":        {pragma: "journal_mode", quer: "wal"},
		"busy_timeout em 5s":    {pragma: "busy_timeout", quer: "5000"},
		"synchronous em NORMAL": {pragma: "synchronous", quer: "1"},
		"foreign_keys ligado":   {pragma: "foreign_keys", quer: "1"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			var valor string
			//nolint:gosec // o nome do pragma vem da tabela do teste, não de entrada
			if err := st.Leitura().QueryRowContext(ctx, "PRAGMA "+tc.pragma).Scan(&valor); err != nil {
				t.Fatalf("PRAGMA %s: erro = %v, quer nil", tc.pragma, err)
			}
			if valor != tc.quer {
				t.Errorf("%s = %q, quer %q", tc.pragma, valor, tc.quer)
			}
		})
	}
}

// TestPools_EscritorUnico prova que o pool de escrita serializa: é o que troca
// SQLITE_BUSY por espera na fila do database/sql.
func TestPools_EscritorUnico(t *testing.T) {
	t.Parallel()

	st := abrir(t)
	if got := st.Escrita().Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections do pool de escrita = %d, quer 1", got)
	}
	if got := st.Leitura().Stats().MaxOpenConnections; got == 1 {
		t.Error("MaxOpenConnections do pool de leitura = 1, quer sem limite")
	}
}

// TestChaveEstrangeira prova que a integridade referencial está ligada: sem ela
// composição órfã de endpoint passaria em silêncio.
func TestChaveEstrangeira(t *testing.T) {
	t.Parallel()

	st := abrir(t)
	_, err := st.Escrita().ExecContext(context.Background(),
		`INSERT INTO endpoint_upstream (endpoint_id, upstream_id) VALUES (999, 999)`)
	if err == nil {
		t.Fatal("erro = nil, quer violação de chave estrangeira")
	}
}
