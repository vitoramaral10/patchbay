package catalogo_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

func TestAplicar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		regras       []catalogo.Regra
		nome         string
		querBase     string
		querEntra    bool
		querRenomeou bool
	}{
		"sem regra tudo entra com o nome original": {
			nome:      "buscar",
			querBase:  "buscar",
			querEntra: true,
		},
		"excluir por glob tira a ferramenta": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
			nome:      "write_file",
			querEntra: false,
		},
		"excluir por glob não pega quem não casa": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
			nome:      "read_file",
			querBase:  "read_file",
			querEntra: true,
		},
		"regra incluir sozinha não fecha o catálogo": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoIncluir, Padrao: "search"}},
			nome:      "delete_tudo",
			querBase:  "delete_tudo",
			querEntra: true,
		},
		"lista de permissão é incluir mais excluir tudo": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoIncluir, Padrao: "search"},
				{Acao: catalogo.AcaoExcluir, Padrao: "*"},
			},
			nome:      "search",
			querBase:  "search",
			querEntra: true,
		},
		"lista de permissão fecha o resto com excluir tudo": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoIncluir, Padrao: "search"},
				{Acao: catalogo.AcaoExcluir, Padrao: "*"},
			},
			nome:      "delete_tudo",
			querEntra: false,
		},
		"primeira regra que casa decide, mesmo com outra depois": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoIncluir, Padrao: "write_seguro"},
				{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
			},
			nome:      "write_seguro",
			querBase:  "write_seguro",
			querEntra: true,
		},
		"a ordem inversa exclui o mesmo nome": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
				{Acao: catalogo.AcaoIncluir, Padrao: "write_seguro"},
			},
			nome:      "write_seguro",
			querEntra: false,
		},
		"renomear troca o nome-base": {
			regras:       []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "buscar_no_notion", Renome: "buscar"}},
			nome:         "buscar_no_notion",
			querBase:     "buscar",
			querEntra:    true,
			querRenomeou: true,
		},
		"renomear com estrela reusa o que o padrão capturou": {
			regras:       []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "notion_*", Renome: "nt.*"}},
			nome:         "notion_search",
			querBase:     "nt.search",
			querEntra:    true,
			querRenomeou: true,
		},
		"renome que não muda nada não conta como renomeação": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "*", Renome: "*"}},
			nome:      "buscar",
			querBase:  "buscar",
			querEntra: true,
		},
		"renome que resolve para vazio é ignorado": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "buscar", Renome: "*"}},
			nome:      "buscar",
			querBase:  "buscar",
			querEntra: true,
		},
		"a primeira regra de renome que casa é a que vale": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoRenomear, Padrao: "notion_*", Renome: "nt_*"},
				{Acao: catalogo.AcaoRenomear, Padrao: "*", Renome: "outro"},
			},
			nome:         "notion_search",
			querBase:     "nt_search",
			querEntra:    true,
			querRenomeou: true,
		},
		"renome não roda em ferramenta excluída": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
				{Acao: catalogo.AcaoRenomear, Padrao: "write_file", Renome: "escrever"},
			},
			nome:      "write_file",
			querEntra: false,
		},
		"o filtro casa contra o nome original e não contra o já renomeado": {
			regras: []catalogo.Regra{
				{Acao: catalogo.AcaoRenomear, Padrao: "write_file", Renome: "escrever"},
				{Acao: catalogo.AcaoExcluir, Padrao: "escrever"},
			},
			nome:         "write_file",
			querBase:     "escrever",
			querEntra:    true,
			querRenomeou: true,
		},
		"estrela no meio do padrão": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "api_*_write"}},
			nome:      "api_v2_write",
			querEntra: false,
		},
		"padrão sem estrela exige igualdade exata": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "search"}},
			nome:      "search_all",
			querBase:  "search_all",
			querEntra: true,
		},
		"estrela casa trecho vazio": {
			regras:    []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "read*"}},
			nome:      "read",
			querEntra: false,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			base, entra, renomeou := catalogo.Aplicar(tc.regras, tc.nome)

			if entra != tc.querEntra {
				t.Fatalf("entra = %v, quer %v", entra, tc.querEntra)
			}
			if !entra {
				return
			}
			if base != tc.querBase {
				t.Errorf("base = %q, quer %q", base, tc.querBase)
			}
			if renomeou != tc.querRenomeou {
				t.Errorf("renomeou = %v, quer %v", renomeou, tc.querRenomeou)
			}
		})
	}
}

func TestAnalisarRegras(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		texto      string
		querRegras []catalogo.Regra
		querErro   bool
		querLinha  int
	}{
		"texto vazio não gera regra": {texto: ""},
		"só espaço e comentário não gera regra": {
			texto: "  \n# isto é um comentário\n\n",
		},
		"as três ações, na ordem escrita": {
			texto: "excluir write_*\nincluir search\nrenomear buscar_no_notion buscar\n",
			querRegras: []catalogo.Regra{
				{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
				{Acao: catalogo.AcaoIncluir, Padrao: "search"},
				{Acao: catalogo.AcaoRenomear, Padrao: "buscar_no_notion", Renome: "buscar"},
			},
		},
		"fim de linha do windows não vira campo": {
			texto:      "excluir write_*\r\n",
			querRegras: []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
		},
		"ação desconhecida é erro na linha certa": {
			texto:     "excluir a\napagar b\n",
			querErro:  true,
			querLinha: 2,
		},
		"regra sem padrão é erro": {
			texto:     "excluir\n",
			querErro:  true,
			querLinha: 1,
		},
		"renomear sem o nome novo é erro": {
			texto:     "renomear buscar\n",
			querErro:  true,
			querLinha: 1,
		},
		"excluir com terceiro campo é erro": {
			texto:     "excluir write_* outra_coisa\n",
			querErro:  true,
			querLinha: 1,
		},
		"quarto campo é erro": {
			texto:     "renomear a b c\n",
			querErro:  true,
			querLinha: 1,
		},
		"renome com caractere fora do alfabeto é erro": {
			texto:     "renomear * ///\n",
			querErro:  true,
			querLinha: 1,
		},
		"renome acima do teto de tamanho é erro": {
			texto: "renomear buscar " +
				strings.Repeat("a", catalogo.MaxRenome+1) + "\n",
			querErro:  true,
			querLinha: 1,
		},
		"renome com mais estrelas do que o padrão captura é erro": {
			texto:     "renomear buscar **\n",
			querErro:  true,
			querLinha: 1,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			regras, err := catalogo.AnalisarRegras(tc.texto)

			if tc.querErro {
				if !errors.Is(err, catalogo.ErrRegraInvalida) {
					t.Fatalf("erro = %v, quer %v", err, catalogo.ErrRegraInvalida)
				}
				var detalhado *catalogo.ErroDeRegra
				if !errors.As(err, &detalhado) {
					t.Fatalf("erro = %v, quer *catalogo.ErroDeRegra", err)
				}
				if detalhado.Linha != tc.querLinha {
					t.Errorf("linha = %d, quer %d", detalhado.Linha, tc.querLinha)
				}
				if detalhado.Mensagem() == "" {
					t.Error("mensagem para a tela = vazia, quer texto")
				}
				return
			}
			if err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if len(regras) != len(tc.querRegras) {
				t.Fatalf("regras = %v, quer %v", regras, tc.querRegras)
			}
			for i, quer := range tc.querRegras {
				if regras[i] != quer {
					t.Errorf("regras[%d] = %+v, quer %+v", i, regras[i], quer)
				}
			}
		})
	}
}

// TestTextoDeRegras_IdaEVolta: o texto canônico é o que a tela reexibe, então
// analisar o que ele produz precisa devolver as mesmas regras. Sem isso, abrir e
// salvar a tela de edição sem tocar em nada mudaria a composição.
func TestTextoDeRegras_IdaEVolta(t *testing.T) {
	t.Parallel()

	original := []catalogo.Regra{
		{Acao: catalogo.AcaoIncluir, Padrao: "search*"},
		{Acao: catalogo.AcaoExcluir, Padrao: "search_admin"},
		{Acao: catalogo.AcaoRenomear, Padrao: "search_*", Renome: "busca_*"},
	}

	volta, err := catalogo.AnalisarRegras(catalogo.TextoDeRegras(original))
	if err != nil {
		t.Fatalf("erro = %v, quer nil", err)
	}
	if len(volta) != len(original) {
		t.Fatalf("regras = %v, quer %v", volta, original)
	}
	for i, quer := range original {
		if volta[i] != quer {
			t.Errorf("regras[%d] = %+v, quer %+v", i, volta[i], quer)
		}
	}
}

// TestRegra_Casa: o que a tela usa para avisar de uma regra que não casou
// nenhuma ferramenta do catálogo vivo — casar contra o padrão isolado, sem o
// resto da decisão de Aplicar.
func TestRegra_Casa(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		regra catalogo.Regra
		nome  string
		quer  bool
	}{
		"glob que casa":     {regra: catalogo.Regra{Padrao: "write_*"}, nome: "write_file", quer: true},
		"glob que não casa": {regra: catalogo.Regra{Padrao: "write_*"}, nome: "read_file", quer: false},
		"igualdade exata":   {regra: catalogo.Regra{Padrao: "search"}, nome: "search", quer: true},
		"nome já prefixado": {regra: catalogo.Regra{Padrao: "nt_search"}, nome: "search", quer: false},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := tc.regra.Casa(tc.nome); got != tc.quer {
				t.Errorf("Casa(%q) = %v, quer %v", tc.nome, got, tc.quer)
			}
		})
	}
}

func TestPrefixoValido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		prefixo string
		quer    bool
	}{
		"vazio vale":                    {prefixo: "", quer: true},
		"underscore vale":               {prefixo: "nt_", quer: true},
		"ponto e hífen valem":           {prefixo: "nt.v2-", quer: true},
		"espaço não vale":               {prefixo: "nt ", quer: false},
		"barra não vale":                {prefixo: "nt/", quer: false},
		"acento não vale":               {prefixo: "não_", quer: false},
		"acima do teto de tamanho":      {prefixo: "abcdefghijabcdefghijabcdefghijabc", quer: false},
		"exatamente no teto de tamanho": {prefixo: "abcdefghijabcdefghijabcdefghijab", quer: true},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := catalogo.PrefixoValido(tc.prefixo); got != tc.quer {
				t.Errorf("PrefixoValido(%q) = %v, quer %v", tc.prefixo, got, tc.quer)
			}
		})
	}
}
