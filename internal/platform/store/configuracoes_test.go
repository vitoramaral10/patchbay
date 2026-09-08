package store_test

import (
	"context"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

func configuracoesDeTeste(t *testing.T) *store.Configuracoes {
	t.Helper()

	st := abrir(t)
	return store.NovasConfiguracoes(st.Leitura(), st.Escrita())
}

func TestConfiguracoes_ChaveInexistenteNaoEErro(t *testing.T) {
	t.Parallel()

	sut := configuracoesDeTeste(t)
	valor, existe, err := sut.Ler(context.Background(), "nunca_gravada")
	if err != nil {
		t.Fatalf("Ler: erro = %v, quer nil", err)
	}
	if existe {
		t.Errorf("existe = true, quer false")
	}
	if valor != "" {
		t.Errorf("valor = %q, quer vazio", valor)
	}
}

func TestConfiguracoes_GravarELerDeVolta(t *testing.T) {
	t.Parallel()

	sut := configuracoesDeTeste(t)
	ctx := context.Background()

	if err := sut.Gravar(ctx, "canario_chave_mestra", "pbc1:primeiro"); err != nil {
		t.Fatalf("Gravar: erro = %v, quer nil", err)
	}
	// Regravar substitui: o canário do primeiro boot é único por banco.
	if err := sut.Gravar(ctx, "canario_chave_mestra", "pbc1:segundo"); err != nil {
		t.Fatalf("Gravar de novo: erro = %v, quer nil", err)
	}

	valor, existe, err := sut.Ler(ctx, "canario_chave_mestra")
	if err != nil || !existe {
		t.Fatalf("Ler: existe = %v, erro = %v, quer true e nil", existe, err)
	}
	if valor != "pbc1:segundo" {
		t.Errorf("valor = %q, quer %q", valor, "pbc1:segundo")
	}
}

// TestMigracoes_TabelasDaFatia6 garante que a migração 00004 entrou.
func TestMigracoes_TabelasDaFatia6(t *testing.T) {
	t.Parallel()

	st := abrir(t)
	for _, tabela := range []string{"settings", "upstream_secret"} {
		var nome string
		err := st.Leitura().QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, tabela).Scan(&nome)
		if err != nil {
			t.Errorf("tabela %s: erro = %v, quer nil", tabela, err)
		}
	}
}
