package biblioteca_test

import (
	"testing"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// TestItem_ModoDeCredencial trava a tradução entre a autenticação que a origem
// declara e o modo que o formulário de upstream entende.
//
// AutAberta virar nenhum é o que faz o link da biblioteca abrir o formulário já
// no modo certo para um servidor público, em vez de abrir num campo de bearer
// que não existe token para preencher.
func TestItem_ModoDeCredencial(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		item biblioteca.Item
		quer string
	}{
		"remoto aberto": {
			item: biblioteca.Item{
				Transporte: biblioteca.TransporteHTTP, Autenticacao: biblioteca.AutAberta,
			},
			quer: "nenhum",
		},
		"remoto com oauth": {
			item: biblioteca.Item{
				Transporte: biblioteca.TransporteHTTP, Autenticacao: biblioteca.AutOAuth,
			},
			quer: "oauth",
		},
		"remoto com token fica no padrão do formulário": {
			item: biblioteca.Item{
				Transporte: biblioteca.TransporteHTTP, Autenticacao: biblioteca.AutToken,
			},
			quer: "",
		},
		"origem que não declarou nada": {
			item: biblioteca.Item{Transporte: biblioteca.TransporteHTTP},
			quer: "",
		},
		"stdio não tem modo, mesmo declarado aberto": {
			item: biblioteca.Item{
				Transporte: biblioteca.TransporteSTDIO, Autenticacao: biblioteca.AutAberta,
			},
			quer: "",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := tc.item.ModoDeCredencial(); got != tc.quer {
				t.Errorf("ModoDeCredencial = %q, quer %q", got, tc.quer)
			}
		})
	}
}
