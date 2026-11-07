package endpoint

import (
	"testing"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

func TestFerramentasDoForm_IdaEVolta(t *testing.T) {
	nomes := []string{"buscar", "ler", "apagar"}

	texto := montarRegras(nomes, []string{"buscar", "ler"}, []string{"procurar", "", ""}, "")
	if texto != "renomear buscar procurar\nexcluir apagar" {
		t.Fatalf("regras = %q", texto)
	}

	itens, avancadas := separarRegras(texto, nomes)
	quer := []ItemFerramenta{{"buscar", true, "procurar"}, {"ler", true, ""}, {"apagar", false, ""}}
	for i := range quer {
		if itens[i] != quer[i] {
			t.Errorf("item %d = %+v, quer %+v", i, itens[i], quer[i])
		}
	}
	if avancadas != "" {
		t.Errorf("avançadas = %q", avancadas)
	}
}

// Com `excluir *` no avançado, a caixa marcada precisa continuar valendo, e
// ferramenta nova (fora da lista) fica desligada.
func TestFerramentasDoForm_CaixaVenceFiltroAvancado(t *testing.T) {
	nomes := []string{"buscar", "apagar"}
	texto := montarRegras(nomes, []string{"buscar"}, nil, "excluir *")
	regras, err := catalogo.AnalisarRegras(texto)
	if err != nil {
		t.Fatal(err)
	}
	for nome, quer := range map[string]bool{"buscar": true, "apagar": false, "nova": false} {
		if _, entra, _ := catalogo.Aplicar(regras, nome); entra != quer {
			t.Errorf("%s entra = %v, quer %v", nome, entra, quer)
		}
	}
	itens, avancadas := separarRegras(texto, nomes)
	if !itens[0].Ligada || itens[1].Ligada || avancadas != "excluir *" {
		t.Fatalf("itens = %+v, avançadas = %q", itens, avancadas)
	}
}

func TestFerramentasDoForm_GlobFicaNoAvancado(t *testing.T) {
	itens, avancadas := separarRegras("excluir write_*\nexcluir sumiu", []string{"ler"})
	if !itens[0].Ligada || avancadas != "excluir write_*\nexcluir sumiu" {
		t.Fatalf("itens = %+v, avançadas = %q", itens, avancadas)
	}
}
