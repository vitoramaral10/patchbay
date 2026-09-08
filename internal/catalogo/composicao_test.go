package catalogo_test

import (
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

func objeto() map[string]any { return map[string]any{"type": "object"} }

func ferramentas(nomes ...string) []*mcp.Tool {
	out := make([]*mcp.Tool, 0, len(nomes))
	for _, nome := range nomes {
		out = append(out, &mcp.Tool{Name: nome, InputSchema: objeto()})
	}
	return out
}

// TestMaterializar_Composicao é o teste da fatia 4: filtro, renome e prefixo
// combinados, na ordem que a composição promete.
func TestMaterializar_Composicao(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		origens       []catalogo.Origem
		querNomes     []string
		querAvisos    map[string]catalogo.Aviso
		querOriginais map[string]string
	}{
		"filtro de exclusão tira a ferramenta do endpoint": {
			origens: []catalogo.Origem{{
				UpstreamID:  1,
				Nome:        "fs",
				Regras:      []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
				Ferramentas: ferramentas("read_file", "write_file", "write_dir"),
			}},
			querNomes: []string{"read_file"},
		},
		"vários filtros aplicáveis: a primeira regra que casa decide": {
			origens: []catalogo.Origem{{
				UpstreamID: 1,
				Nome:       "fs",
				Regras: []catalogo.Regra{
					{Acao: catalogo.AcaoIncluir, Padrao: "write_seguro"},
					{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
					{Acao: catalogo.AcaoExcluir, Padrao: "delete_*"},
				},
				Ferramentas: ferramentas("read_file", "write_seguro", "write_file", "delete_tudo"),
			}},
			querNomes: []string{"read_file", "write_seguro"},
		},
		"lista de permissão: incluir o que vale e excluir o resto": {
			origens: []catalogo.Origem{{
				UpstreamID: 1,
				Nome:       "notion",
				Regras: []catalogo.Regra{
					{Acao: catalogo.AcaoIncluir, Padrao: "search*"},
					{Acao: catalogo.AcaoExcluir, Padrao: "*"},
				},
				Ferramentas: ferramentas("search", "search_all", "create_page"),
			}},
			querNomes: []string{"search", "search_all"},
		},
		"renome e prefixo se combinam, nessa ordem": {
			origens: []catalogo.Origem{{
				UpstreamID:  1,
				Nome:        "notion",
				Prefixo:     "nt_",
				Regras:      []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "buscar_no_notion", Renome: "buscar"}},
				Ferramentas: ferramentas("buscar_no_notion"),
			}},
			querNomes:  []string{"nt_buscar"},
			querAvisos: map[string]catalogo.Aviso{"nt_buscar": catalogo.AvisoRenomeada},
		},
		"renome que colide com ferramenta do mesmo upstream é desambiguado": {
			origens: []catalogo.Origem{{
				UpstreamID:  1,
				Nome:        "notion",
				Regras:      []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "search_all", Renome: "search"}},
				Ferramentas: ferramentas("search", "search_all"),
			}},
			querNomes:  []string{"search", "search_2"},
			querAvisos: map[string]catalogo.Aviso{"search_2": catalogo.AvisoColisaoDeNome},
		},
		"prefixo igual em dois upstreams ainda colide e é desambiguado": {
			origens: []catalogo.Origem{
				{UpstreamID: 1, Nome: "a", Prefixo: "x_", Ferramentas: ferramentas("search")},
				{UpstreamID: 2, Nome: "b", Prefixo: "x_", Ferramentas: ferramentas("search")},
			},
			querNomes:  []string{"x_search", "x_search_2"},
			querAvisos: map[string]catalogo.Aviso{"x_search_2": catalogo.AvisoColisaoDeNome},
		},
		"renome de um upstream colide com o nome nativo do outro": {
			origens: []catalogo.Origem{
				{UpstreamID: 1, Nome: "a", Ferramentas: ferramentas("search")},
				{
					UpstreamID:  2,
					Nome:        "b",
					Regras:      []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "buscar", Renome: "search"}},
					Ferramentas: ferramentas("buscar"),
				},
			},
			querNomes: []string{"search", "search_2"},
			// O nativo de a fica com o nome dele; quem foi desambiguado é o
			// renomeado de b, nunca o contrário.
			querOriginais: map[string]string{"search": "search", "search_2": "buscar"},
		},
		"renome intra-upstream não rouba o nome nativo mesmo vindo antes no tools/list": {
			origens: []catalogo.Origem{{
				UpstreamID:  1,
				Nome:        "fs",
				Regras:      []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "z_last", Renome: "a_first"}},
				Ferramentas: ferramentas("z_last", "a_first"),
			}},
			// z_last chega primeiro no tools/list e seria o primeiro a reservar
			// "a_first" numa passada só; a nativa a_first não pode perder o
			// próprio nome para uma regra de renome por causa da ordem.
			querNomes:     []string{"a_first_2", "a_first"},
			querAvisos:    map[string]catalogo.Aviso{"a_first_2": catalogo.AvisoColisaoDeNome},
			querOriginais: map[string]string{"a_first_2": "z_last", "a_first": "a_first"},
		},
		"filtro que apaga tudo deixa o endpoint com o resto": {
			origens: []catalogo.Origem{
				{
					UpstreamID:  1,
					Nome:        "a",
					Regras:      []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "*"}},
					Ferramentas: ferramentas("search", "write"),
				},
				{UpstreamID: 2, Nome: "b", Ferramentas: ferramentas("ping")},
			},
			querNomes: []string{"ping"},
		},
		"regra não muda o nome chamado no upstream": {
			origens: []catalogo.Origem{{
				UpstreamID:  9,
				Nome:        "notion",
				Prefixo:     "nt_",
				Regras:      []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "search", Renome: "buscar"}},
				Ferramentas: ferramentas("search"),
			}},
			querNomes: []string{"nt_buscar"},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			out := catalogo.Materializar(logDescartado(), tc.origens)

			nomes := make([]string, 0, len(out))
			for _, f := range out {
				nomes = append(nomes, f.NomeExposto())
				registrarSemPanic(t, f.Tool)
			}
			if !slices.Equal(nomes, tc.querNomes) {
				t.Fatalf("nomes = %v, quer %v", nomes, tc.querNomes)
			}
			for _, f := range out {
				if quer, ok := tc.querAvisos[f.NomeExposto()]; ok && !f.TemAviso(quer) {
					t.Errorf("avisos de %s = %v, quer conter %q", f.NomeExposto(), f.Avisos, quer)
				}
				if quer, ok := tc.querOriginais[f.NomeExposto()]; ok && f.NomeOriginal != quer {
					t.Errorf("nome original de %s = %q, quer %q", f.NomeExposto(), f.NomeOriginal, quer)
				}
			}
		})
	}
}

// TestMaterializar_NomeOriginalSobreviveAoRenome: o renome é do nome exposto, e
// o tools/call de saída continua usando o nome que o upstream conhece. Se isto
// quebrar, toda ferramenta renomeada passa a falhar no upstream.
func TestMaterializar_NomeOriginalSobreviveAoRenome(t *testing.T) {
	t.Parallel()

	out := catalogo.Materializar(logDescartado(), []catalogo.Origem{{
		UpstreamID:  9,
		Nome:        "notion",
		Prefixo:     "nt_",
		Regras:      []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "search", Renome: "buscar"}},
		Ferramentas: ferramentas("search"),
	}})

	if len(out) != 1 {
		t.Fatalf("ferramentas = %d, quer 1", len(out))
	}
	if out[0].NomeExposto() != "nt_buscar" {
		t.Errorf("nome exposto = %q, quer %q", out[0].NomeExposto(), "nt_buscar")
	}
	if out[0].NomeOriginal != "search" {
		t.Errorf("nome original = %q, quer %q", out[0].NomeOriginal, "search")
	}
}

// TestMaterializar_MesmoUpstreamEmDoisEndpoints é o critério de aceite da fatia
// 4 no nível do catálogo: um snapshot, duas composições, dois catálogos — e o
// snapshot não é mutado por nenhuma das duas.
func TestMaterializar_MesmoUpstreamEmDoisEndpoints(t *testing.T) {
	t.Parallel()

	snapshot := ferramentas("search", "write_page", "read_page")

	pessoal := catalogo.Materializar(logDescartado(), []catalogo.Origem{{
		UpstreamID:  1,
		Nome:        "notion",
		Prefixo:     "nt_",
		Ferramentas: snapshot,
	}})
	trabalho := catalogo.Materializar(logDescartado(), []catalogo.Origem{{
		UpstreamID: 1,
		Nome:       "notion",
		Prefixo:    "wk.",
		Regras: []catalogo.Regra{
			{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
			{Acao: catalogo.AcaoRenomear, Padrao: "read_*", Renome: "ler_*"},
		},
		Ferramentas: snapshot,
	}})

	querPessoal := []string{"nt_search", "nt_write_page", "nt_read_page"}
	querTrabalho := []string{"wk.search", "wk.ler_page"}

	if nomes := expostos(pessoal); !slices.Equal(nomes, querPessoal) {
		t.Errorf("catálogo de pessoal = %v, quer %v", nomes, querPessoal)
	}
	if nomes := expostos(trabalho); !slices.Equal(nomes, querTrabalho) {
		t.Errorf("catálogo de trabalho = %v, quer %v", nomes, querTrabalho)
	}

	querSnapshot := []string{"search", "write_page", "read_page"}
	for i, quer := range querSnapshot {
		if snapshot[i].Name != quer {
			t.Errorf("snapshot[%d] = %q, quer %q; uma composição mutou o que o upstream descobriu",
				i, snapshot[i].Name, quer)
		}
	}
}

func expostos(fs []catalogo.Ferramenta) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.NomeExposto())
	}
	return out
}
