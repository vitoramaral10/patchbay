package endpoint_test

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
)

// composicaoFake materializa de verdade — normalizador, filtro, renome, prefixo
// e desambiguação — a partir de um snapshot por upstream e de uma composição por
// endpoint. É o dublê que exercita a fatia 4 sem banco.
type composicaoFake struct {
	mu sync.Mutex
	// snapshots é o que cada upstream expôs no último tools/list.
	snapshots map[int64][]*mcp.Tool
	// composicoes é a composição de cada endpoint, na ordem.
	composicoes map[int64][]catalogo.Origem
}

func novaComposicaoFake() *composicaoFake {
	return &composicaoFake{
		snapshots:   map[int64][]*mcp.Tool{},
		composicoes: map[int64][]catalogo.Origem{},
	}
}

func (c *composicaoFake) Materializar(_ context.Context, endpointID int64) ([]catalogo.Ferramenta, error) {
	c.mu.Lock()
	origens := slices.Clone(c.composicoes[endpointID])
	for i := range origens {
		origens[i].Ferramentas = c.snapshots[origens[i].UpstreamID]
	}
	c.mu.Unlock()
	return catalogo.Materializar(slog.New(slog.DiscardHandler), origens), nil
}

func (c *composicaoFake) expor(upstreamID int64, nomes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*mcp.Tool, 0, len(nomes))
	for _, nome := range nomes {
		out = append(out, &mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}})
	}
	c.snapshots[upstreamID] = out
}

func (c *composicaoFake) compor(endpointID int64, origens ...catalogo.Origem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.composicoes[endpointID] = origens
}

func montarComposicao(t *testing.T, comp *composicaoFake, regs ...endpoint.Registro) *endpoint.Servidores {
	t.Helper()

	sut := endpoint.NovoServidores(
		&repoFake{regs: regs}, comp, executorFake{},
		slog.New(slog.DiscardHandler), endpoint.ComJanelaDeGraca(0))
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar inicial: erro = %v, quer nil", err)
	}
	return sut
}

// TestServidores_MesmoUpstreamEmDoisEndpoints é o critério de aceite da fatia 4:
// um upstream, duas composições, dois catálogos vivos ao mesmo tempo.
func TestServidores_MesmoUpstreamEmDoisEndpoints(t *testing.T) {
	t.Parallel()

	comp := novaComposicaoFake()
	comp.expor(1, "search", "write_page", "read_page")
	comp.compor(10, catalogo.Origem{UpstreamID: 1, Nome: "notion", Prefixo: "nt_"})
	comp.compor(20, catalogo.Origem{
		UpstreamID: 1, Nome: "notion", Prefixo: "wk.",
		Regras: []catalogo.Regra{
			{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
			{Acao: catalogo.AcaoRenomear, Padrao: "read_*", Renome: "ler_*"},
		},
	})

	sut := montarComposicao(t, comp,
		endpoint.Registro{ID: 10, Slug: "pessoal", Nome: "Pessoal"},
		endpoint.Registro{ID: 20, Slug: "trabalho", Nome: "Trabalho"},
	)

	pessoal := ligarCliente(t, sut.Servidor("pessoal"), nil)
	trabalho := ligarCliente(t, sut.Servidor("trabalho"), nil)

	querPessoal := []string{"nt_read_page", "nt_search", "nt_write_page"}
	querTrabalho := []string{"wk.ler_page", "wk.search"}

	if nomes := nomesDoServidor(t, pessoal); !slices.Equal(nomes, querPessoal) {
		t.Errorf("ferramentas de pessoal = %v, quer %v", nomes, querPessoal)
	}
	if nomes := nomesDoServidor(t, trabalho); !slices.Equal(nomes, querTrabalho) {
		t.Errorf("ferramentas de trabalho = %v, quer %v", nomes, querTrabalho)
	}
	if n := sut.Contagem("pessoal"); n != 3 {
		t.Errorf("contagem de pessoal = %d, quer 3", n)
	}
	if n := sut.Contagem("trabalho"); n != 2 {
		t.Errorf("contagem de trabalho = %d, quer 2", n)
	}
}

// TestServidores_ComposicaoAplicaAQuente é o hot-apply: mudar filtro, prefixo e
// renome vale na hora, na mesma instância de *mcp.Server e na mesma sessão do
// cliente — nenhum reinício, nenhuma reconexão.
func TestServidores_ComposicaoAplicaAQuente(t *testing.T) {
	t.Parallel()

	comp := novaComposicaoFake()
	comp.expor(1, "search", "write_page", "read_page")
	comp.compor(10, catalogo.Origem{UpstreamID: 1, Nome: "notion"})

	sut := montarComposicao(t, comp, endpoint.Registro{ID: 10, Slug: slugDeTeste, Nome: "Pessoal"})
	antes := sut.Servidor(slugDeTeste)
	sessao := ligarCliente(t, antes, nil)

	quer := []string{"read_page", "search", "write_page"}
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, quer) {
		t.Fatalf("ferramentas = %v, quer %v", nomes, quer)
	}

	passos := []struct {
		nome   string
		origem catalogo.Origem
		quer   []string
	}{
		{
			nome: "filtro entra",
			origem: catalogo.Origem{
				UpstreamID: 1, Nome: "notion",
				Regras: []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
			},
			quer: []string{"read_page", "search"},
		},
		{
			nome: "prefixo entra junto com o filtro",
			origem: catalogo.Origem{
				UpstreamID: 1, Nome: "notion", Prefixo: "nt_",
				Regras: []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
			},
			quer: []string{"nt_read_page", "nt_search"},
		},
		{
			nome: "renome entra por cima do prefixo",
			origem: catalogo.Origem{
				UpstreamID: 1, Nome: "notion", Prefixo: "nt_",
				Regras: []catalogo.Regra{
					{Acao: catalogo.AcaoExcluir, Padrao: "write_*"},
					{Acao: catalogo.AcaoRenomear, Padrao: "search", Renome: "buscar"},
				},
			},
			quer: []string{"nt_buscar", "nt_read_page"},
		},
		{
			nome:   "toda regra sai e o catálogo volta inteiro",
			origem: catalogo.Origem{UpstreamID: 1, Nome: "notion"},
			quer:   []string{"read_page", "search", "write_page"},
		},
	}

	for _, passo := range passos {
		comp.compor(10, passo.origem)
		if err := sut.Sincronizar(context.Background()); err != nil {
			t.Fatalf("%s: Sincronizar: erro = %v, quer nil", passo.nome, err)
		}
		if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, passo.quer) {
			t.Errorf("%s: ferramentas = %v, quer %v", passo.nome, nomes, passo.quer)
		}
		if sut.Servidor(slugDeTeste) != antes {
			t.Fatalf("%s: mudança de composição recriou o *mcp.Server e derrubaria as sessões", passo.nome)
		}
	}
}

// TestServidores_ContagemPorUpstream: o número ao lado de cada upstream dentro do
// endpoint é o que ele entrega àquele endpoint, não o que ele expõe. É o que a
// seção 11 exige, e é a única forma de ver que um filtro apagou tudo.
func TestServidores_ContagemPorUpstream(t *testing.T) {
	t.Parallel()

	comp := novaComposicaoFake()
	comp.expor(1, "search", "write_page", "read_page")
	comp.expor(2, "ping")
	comp.compor(10,
		catalogo.Origem{
			UpstreamID: 1, Nome: "notion",
			Regras: []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "write_*"}},
		},
		catalogo.Origem{UpstreamID: 2, Nome: "outro"},
	)

	sut := montarComposicao(t, comp, endpoint.Registro{ID: 10, Slug: slugDeTeste, Nome: "Pessoal"})

	quer := map[int64]int{1: 2, 2: 1}
	if got := sut.ContagemPorUpstream(slugDeTeste); !maps.Equal(got, quer) {
		t.Errorf("contagem por upstream = %v, quer %v", got, quer)
	}

	// Filtro que apaga tudo de um upstream: ele fica em zero e some do mapa, e o
	// outro não muda. Zero é informação — é o que diferencia "filtrei demais" de
	// "o upstream caiu".
	comp.compor(10,
		catalogo.Origem{
			UpstreamID: 1, Nome: "notion",
			Regras: []catalogo.Regra{{Acao: catalogo.AcaoExcluir, Padrao: "*"}},
		},
		catalogo.Origem{UpstreamID: 2, Nome: "outro"},
	)
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	got := sut.ContagemPorUpstream(slugDeTeste)
	// A chave some do mapa quando o upstream fica em zero: um upstream
	// filtrado não contribui ferramenta nenhuma para porUpstream, então nunca
	// escreve a chave. "got[1] != 0" passaria mesmo com o mapa nil — leitura
	// de mapa ausente em Go devolve zero — e não provaria nada.
	if _, ok := got[1]; ok {
		t.Fatalf("contagem do upstream filtrado = %v, quer a chave 1 ausente do mapa", got)
	}
	if got[2] != 1 {
		t.Errorf("contagem do outro upstream = %d, quer 1", got[2])
	}
	if n := sut.Contagem(slugDeTeste); n != 1 {
		t.Errorf("contagem do endpoint = %d, quer 1", n)
	}
}

// TestServidores_ColisaoDepoisDoPrefixoENoRenome: a desambiguação é a última
// etapa e vale para o nome já composto. Sem ela, dois upstreams com o mesmo
// prefixo registrariam o mesmo nome e o segundo AddTool sobrescreveria o
// primeiro em silêncio.
func TestServidores_ColisaoDepoisDoPrefixoENoRenome(t *testing.T) {
	t.Parallel()

	comp := novaComposicaoFake()
	comp.expor(1, "search")
	comp.expor(2, "search")
	comp.expor(3, "buscar")
	comp.compor(10,
		catalogo.Origem{UpstreamID: 1, Nome: "a", Prefixo: "x_"},
		catalogo.Origem{UpstreamID: 2, Nome: "b", Prefixo: "x_"},
		catalogo.Origem{
			UpstreamID: 3, Nome: "c", Prefixo: "x_",
			Regras: []catalogo.Regra{{Acao: catalogo.AcaoRenomear, Padrao: "buscar", Renome: "search"}},
		},
	)

	sut := montarComposicao(t, comp, endpoint.Registro{ID: 10, Slug: slugDeTeste, Nome: "Pessoal"})
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	quer := []string{"x_search", "x_search_2", "x_search_3"}
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, quer) {
		t.Errorf("ferramentas = %v, quer %v", nomes, quer)
	}
	// Três nomes distintos e três upstreams: nenhuma ferramenta foi engolida.
	if got := sut.ContagemPorUpstream(slugDeTeste); !maps.Equal(got, map[int64]int{1: 1, 2: 1, 3: 1}) {
		t.Errorf("contagem por upstream = %v, quer uma de cada", got)
	}
}
