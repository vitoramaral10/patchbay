package catalogo_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

func logDescartado() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// naoSerializavel é um valor que json.Marshal recusa. É o que reproduz o
// caminho "can't marshal input schema to a JSON object" do AddTool.
type naoSerializavel struct{}

func (naoSerializavel) MarshalJSON() ([]byte, error) {
	return nil, errors.New("este schema não serializa")
}

func TestNormalizar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		prefixo      string
		entrada      *mcp.Tool
		querDescarte bool
		querNome     string
		querAvisos   []catalogo.Aviso
	}{
		"ferramenta válida passa intacta": {
			entrada: &mcp.Tool{
				Name:        "buscar",
				InputSchema: map[string]any{"type": "object"},
			},
			querNome: "buscar",
		},
		"prefixo entra no nome exposto e não no original": {
			prefixo: "nt_",
			entrada: &mcp.Tool{
				Name:        "buscar",
				InputSchema: map[string]any{"type": "object"},
			},
			querNome: "nt_buscar",
		},
		"input schema nulo é substituído por objeto permissivo": {
			entrada:    &mcp.Tool{Name: "sem_schema", InputSchema: nil},
			querNome:   "sem_schema",
			querAvisos: []catalogo.Aviso{catalogo.AvisoSchemaDegradado},
		},
		"ponteiro tipado nulo de schema é substituído": {
			entrada: &mcp.Tool{
				Name:        "ponteiro_nulo",
				InputSchema: (*jsonschema.Schema)(nil),
			},
			querNome:   "ponteiro_nulo",
			querAvisos: []catalogo.Aviso{catalogo.AvisoSchemaDegradado},
		},
		"input schema com type diferente de object é substituído": {
			entrada: &mcp.Tool{
				Name:        "tipo_errado",
				InputSchema: map[string]any{"type": "string"},
			},
			querNome:   "tipo_errado",
			querAvisos: []catalogo.Aviso{catalogo.AvisoSchemaDegradado},
		},
		"jsonschema tipado com type diferente de object é substituído": {
			entrada: &mcp.Tool{
				Name:        "tipado_errado",
				InputSchema: &jsonschema.Schema{Type: "array"},
			},
			querNome:   "tipado_errado",
			querAvisos: []catalogo.Aviso{catalogo.AvisoSchemaDegradado},
		},
		"input schema que não serializa é substituído": {
			entrada: &mcp.Tool{
				Name:        "schema_quebrado",
				InputSchema: naoSerializavel{},
			},
			querNome:   "schema_quebrado",
			querAvisos: []catalogo.Aviso{catalogo.AvisoSchemaDegradado},
		},
		"output schema que não serializa é descartado": {
			entrada: &mcp.Tool{
				Name:         "saida_quebrada",
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: naoSerializavel{},
			},
			querNome:   "saida_quebrada",
			querAvisos: []catalogo.Aviso{catalogo.AvisoOutputDescartado},
		},
		"output schema ponteiro tipado nulo é descartado": {
			entrada: &mcp.Tool{
				Name:         "saida_nula",
				InputSchema:  map[string]any{"type": "object"},
				OutputSchema: (*jsonschema.Schema)(nil),
			},
			querNome:   "saida_nula",
			querAvisos: []catalogo.Aviso{catalogo.AvisoOutputDescartado},
		},
		"nome com rune inválido é saneado": {
			entrada: &mcp.Tool{
				Name:        "buscar coisas/agora!",
				InputSchema: map[string]any{"type": "object"},
			},
			querNome:   "buscar_coisas_agora_",
			querAvisos: []catalogo.Aviso{catalogo.AvisoNomeSaneado},
		},
		"nome acima de 128 caracteres é cortado": {
			entrada: &mcp.Tool{
				Name:        strings.Repeat("a", 200),
				InputSchema: map[string]any{"type": "object"},
			},
			querNome:   strings.Repeat("a", catalogo.MaxNomeFerramenta),
			querAvisos: []catalogo.Aviso{catalogo.AvisoNomeSaneado},
		},
		"nome vazio é descartado": {
			entrada:      &mcp.Tool{Name: "", InputSchema: map[string]any{"type": "object"}},
			querDescarte: true,
		},
		"nome só de runes inválidos é descartado": {
			entrada: &mcp.Tool{
				Name:        "!@#$%",
				InputSchema: map[string]any{"type": "object"},
			},
			querDescarte: true,
		},
		"ferramenta nula é descartada": {
			entrada:      nil,
			querDescarte: true,
		},
		"anotação x-mcp-header é removida do input schema": {
			entrada: &mcp.Tool{
				Name: "com_anotacao",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"corpo": map[string]any{
							"type":         "object",
							"x-mcp-header": "X-Coisa",
						},
					},
				},
			},
			querNome:   "com_anotacao",
			querAvisos: []catalogo.Aviso{catalogo.AvisoAnotacaoRemovida},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			f, err := catalogo.Normalizar(7, "upstream_de_teste", tc.prefixo, tc.entrada)

			if tc.querDescarte {
				if !errors.Is(err, catalogo.ErrDescartada) {
					t.Fatalf("erro = %v, quer %v", err, catalogo.ErrDescartada)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if f.NomeExposto() != tc.querNome {
				t.Errorf("nome exposto = %q, quer %q", f.NomeExposto(), tc.querNome)
			}
			if f.NomeOriginal != tc.entrada.Name {
				t.Errorf("nome original = %q, quer %q", f.NomeOriginal, tc.entrada.Name)
			}
			if f.UpstreamID != 7 {
				t.Errorf("upstream id = %d, quer 7", f.UpstreamID)
			}
			for _, quer := range tc.querAvisos {
				if !f.TemAviso(quer) {
					t.Errorf("avisos = %v, quer conter %q", f.Avisos, quer)
				}
			}
			if len(tc.querAvisos) == 0 && len(f.Avisos) != 0 {
				t.Errorf("avisos = %v, quer nenhum", f.Avisos)
			}

			// O que a normalização produz tem que sobreviver ao AddTool: é o
			// requisito inteiro deste pacote.
			registrarSemPanic(t, f.Tool)
		})
	}
}

// registrarSemPanic falha o teste se (*mcp.Server).AddTool entrar em panic.
func registrarSemPanic(t *testing.T, tool *mcp.Tool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AddTool entrou em panic com ferramenta normalizada: %v", r)
		}
	}()
	srv := mcp.NewServer(&mcp.Implementation{Name: "sut", Version: "0"}, nil)
	srv.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, nil
	})
}

// TestAddTool_PanicaSemNormalizador prova que o risco é real e não teórico: se
// um bump do go-sdk parar de entrar em panic nestes caminhos, este teste
// avisa — e aí o normalizador pode encolher com evidência, não com fé.
func TestAddTool_PanicaSemNormalizador(t *testing.T) {
	t.Parallel()

	casos := map[string]*mcp.Tool{
		"input schema nulo":               {Name: "x", InputSchema: nil},
		"ponteiro tipado nulo":            {Name: "x", InputSchema: (*jsonschema.Schema)(nil)},
		"type diferente de object":        {Name: "x", InputSchema: map[string]any{"type": "string"}},
		"jsonschema tipado não object":    {Name: "x", InputSchema: &jsonschema.Schema{Type: "array"}},
		"input schema que não serializa":  {Name: "x", InputSchema: naoSerializavel{}},
		"output schema que não serializa": {Name: "x", InputSchema: map[string]any{"type": "object"}, OutputSchema: naoSerializavel{}},
		"output schema tipado nulo":       {Name: "x", InputSchema: map[string]any{"type": "object"}, OutputSchema: (*jsonschema.Schema)(nil)},
	}

	for nome, tool := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			panicou := func() (p bool) {
				defer func() { p = recover() != nil }()
				srv := mcp.NewServer(&mcp.Implementation{Name: "sut", Version: "0"}, nil)
				srv.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return nil, nil
				})
				return false
			}()

			if !panicou {
				t.Fatalf("AddTool não entrou em panic; o normalizador pode ter deixado de ser necessário para este caminho")
			}
		})
	}
}

func TestMaterializar(t *testing.T) {
	t.Parallel()

	objeto := func() map[string]any { return map[string]any{"type": "object"} }

	casos := map[string]struct {
		origens    []catalogo.Origem
		querNomes  []string
		querAvisos map[string]catalogo.Aviso
	}{
		"colisão de nome entre upstreams é desambiguada": {
			origens: []catalogo.Origem{
				{UpstreamID: 1, Nome: "a", Ferramentas: []*mcp.Tool{{Name: "search", InputSchema: objeto()}}},
				{UpstreamID: 2, Nome: "b", Ferramentas: []*mcp.Tool{{Name: "search", InputSchema: objeto()}}},
			},
			querNomes:  []string{"search", "search_2"},
			querAvisos: map[string]catalogo.Aviso{"search_2": catalogo.AvisoColisaoDeNome},
		},
		"prefixo resolve a colisão antes de precisar de sufixo": {
			origens: []catalogo.Origem{
				{UpstreamID: 1, Nome: "a", Prefixo: "a_", Ferramentas: []*mcp.Tool{{Name: "search", InputSchema: objeto()}}},
				{UpstreamID: 2, Nome: "b", Prefixo: "b_", Ferramentas: []*mcp.Tool{{Name: "search", InputSchema: objeto()}}},
			},
			querNomes: []string{"a_search", "b_search"},
		},
		"colisão de três vira sufixo 2 e 3": {
			origens: []catalogo.Origem{
				{UpstreamID: 1, Nome: "a", Ferramentas: []*mcp.Tool{{Name: "ping", InputSchema: objeto()}}},
				{UpstreamID: 2, Nome: "b", Ferramentas: []*mcp.Tool{{Name: "ping", InputSchema: objeto()}}},
				{UpstreamID: 3, Nome: "c", Ferramentas: []*mcp.Tool{{Name: "ping", InputSchema: objeto()}}},
			},
			querNomes: []string{"ping", "ping_2", "ping_3"},
		},
		"ferramenta descartada não interrompe as outras do mesmo upstream": {
			origens: []catalogo.Origem{{
				UpstreamID: 1, Nome: "a",
				Ferramentas: []*mcp.Tool{
					{Name: "", InputSchema: objeto()},
					{Name: "vale", InputSchema: objeto()},
				},
			}},
			querNomes: []string{"vale"},
		},
		"endpoint sem upstream pronto materializa vazio": {
			origens:   []catalogo.Origem{{UpstreamID: 1, Nome: "a", Ferramentas: nil}},
			querNomes: nil,
		},
		"colisão entre nome saneado e nome já existente é desambiguada": {
			origens: []catalogo.Origem{{
				UpstreamID: 1, Nome: "a",
				Ferramentas: []*mcp.Tool{
					{Name: "meu_tool", InputSchema: objeto()},
					{Name: "meu tool", InputSchema: objeto()},
				},
			}},
			querNomes: []string{"meu_tool", "meu_tool_2"},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			ferramentas := catalogo.Materializar(logDescartado(), tc.origens)

			var nomes []string
			for _, f := range ferramentas {
				nomes = append(nomes, f.NomeExposto())
				registrarSemPanic(t, f.Tool)
			}
			if len(nomes) != len(tc.querNomes) {
				t.Fatalf("nomes = %v, quer %v", nomes, tc.querNomes)
			}
			for i, quer := range tc.querNomes {
				if nomes[i] != quer {
					t.Errorf("nomes[%d] = %q, quer %q", i, nomes[i], quer)
				}
			}
			for _, f := range ferramentas {
				if quer, ok := tc.querAvisos[f.NomeExposto()]; ok && !f.TemAviso(quer) {
					t.Errorf("avisos de %s = %v, quer conter %q", f.NomeExposto(), f.Avisos, quer)
				}
			}
		})
	}
}

// TestNormalizar_NaoMutaOriginal garante que o snapshot do upstream sobrevive à
// normalização: dois endpoints normalizam a mesma ferramenta com prefixos
// diferentes, e nenhum vê o prefixo do outro.
func TestNormalizar_NaoMutaOriginal(t *testing.T) {
	t.Parallel()

	original := &mcp.Tool{Name: "buscar", InputSchema: map[string]any{"type": "object"}}

	primeira, err := catalogo.Normalizar(1, "a", "a_", original)
	if err != nil {
		t.Fatalf("erro = %v, quer nil", err)
	}
	segunda, err := catalogo.Normalizar(1, "a", "b_", original)
	if err != nil {
		t.Fatalf("erro = %v, quer nil", err)
	}

	if original.Name != "buscar" {
		t.Errorf("nome do original = %q, quer %q", original.Name, "buscar")
	}
	if primeira.NomeExposto() != "a_buscar" {
		t.Errorf("primeira = %q, quer %q", primeira.NomeExposto(), "a_buscar")
	}
	if segunda.NomeExposto() != "b_buscar" {
		t.Errorf("segunda = %q, quer %q", segunda.NomeExposto(), "b_buscar")
	}
}
