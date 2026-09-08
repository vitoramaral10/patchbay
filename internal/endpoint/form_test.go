package endpoint_test

import (
	"testing"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
)

func TestForm_ValidarComposicao(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		form      endpoint.Form
		querPassa bool
		querErro  string
	}{
		"composição vazia passa": {
			form:      endpoint.Form{Slug: "pessoal", Nome: "Pessoal"},
			querPassa: true,
		},
		"prefixo e regras válidos passam": {
			form: endpoint.Form{
				Slug: "pessoal", Nome: "Pessoal",
				UpstreamIDs: []int64{7},
				Prefixos:    map[int64]string{7: "nt_"},
				RegrasTexto: map[int64]string{7: "excluir write_*\nrenomear search buscar\n"},
			},
			querPassa: true,
		},
		"prefixo com espaço não passa": {
			form: endpoint.Form{
				Slug: "pessoal", Nome: "Pessoal",
				UpstreamIDs: []int64{7},
				Prefixos:    map[int64]string{7: "nt "},
			},
			querErro: endpoint.ChavePrefixo(7),
		},
		"regra sem padrão não passa": {
			form: endpoint.Form{
				Slug: "pessoal", Nome: "Pessoal",
				UpstreamIDs: []int64{7},
				RegrasTexto: map[int64]string{7: "excluir\n"},
			},
			querErro: endpoint.ChaveRegras(7),
		},
		"ação desconhecida não passa": {
			form: endpoint.Form{
				Slug: "pessoal", Nome: "Pessoal",
				UpstreamIDs: []int64{7},
				RegrasTexto: map[int64]string{7: "apagar tudo\n"},
			},
			querErro: endpoint.ChaveRegras(7),
		},
		"composição de upstream não marcado é sobra e não é erro": {
			form: endpoint.Form{
				Slug: "pessoal", Nome: "Pessoal",
				UpstreamIDs: []int64{7},
				Prefixos:    map[int64]string{7: "nt_", 9: "prefixo inválido"},
				RegrasTexto: map[int64]string{9: "apagar tudo\n"},
			},
			querPassa: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := tc.form
			passou := sut.Validar()

			if passou != tc.querPassa {
				t.Fatalf("Validar() = %v, quer %v (erros: %v)", passou, tc.querPassa, sut.Erros)
			}
			if tc.querErro != "" && sut.Erros[tc.querErro] == "" {
				t.Errorf("erros = %v, quer mensagem em %q", sut.Erros, tc.querErro)
			}
		})
	}
}

// TestForm_RegrasSoDepoisDeValidar: o formulário só entrega regra analisada
// depois de a validação passar. É o que impede uma linha inválida de chegar ao
// banco por um caminho que não passou pela tela.
func TestForm_RegrasSoDepoisDeValidar(t *testing.T) {
	t.Parallel()

	sut := endpoint.Form{
		Slug: "pessoal", Nome: "Pessoal",
		UpstreamIDs: []int64{7},
		RegrasTexto: map[int64]string{7: "excluir write_*\nincluir search\n"},
	}

	if regras := sut.Regras(7); len(regras) != 0 {
		t.Fatalf("regras antes de Validar = %v, quer nenhuma", regras)
	}
	if !sut.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", sut.Erros)
	}

	quer := []catalogo.Regra{
		{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
		{Acao: catalogo.AcaoIncluir, Padrao: "search"},
	}
	regras := sut.Regras(7)
	if len(regras) != len(quer) {
		t.Fatalf("regras = %v, quer %v", regras, quer)
	}
	for i := range quer {
		if regras[i] != quer[i] {
			t.Errorf("regras[%d] = %+v, quer %+v", i, regras[i], quer[i])
		}
	}
}
