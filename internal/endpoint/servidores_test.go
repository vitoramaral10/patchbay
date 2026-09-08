package endpoint_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
)

const slugDeTeste = "pessoal"

// repoFake é o dublê da persistência de endpoints, escrito à mão sobre a
// interface pequena que o pacote declara.
type repoFake struct {
	mu   sync.Mutex
	regs []endpoint.Registro
}

func (r *repoFake) Todos(context.Context) ([]endpoint.Registro, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.regs), nil
}

func (r *repoFake) definir(regs ...endpoint.Registro) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regs = regs
}

// catalogoFake devolve o conjunto de ferramentas que o teste mandar, e conta
// quantas vezes foi chamado.
type catalogoFake struct {
	mu          sync.Mutex
	ferramentas []catalogo.Ferramenta
}

func (c *catalogoFake) Materializar(context.Context, int64) ([]catalogo.Ferramenta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ferramentas), nil
}

func (c *catalogoFake) definir(nomes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ferramentas = ferramentasChamadas(nomes...)
}

func ferramentasChamadas(nomes ...string) []catalogo.Ferramenta {
	out := make([]catalogo.Ferramenta, 0, len(nomes))
	for _, nome := range nomes {
		out = append(out, catalogo.Ferramenta{
			UpstreamID:   1,
			UpstreamNome: "falso",
			NomeOriginal: nome,
			Tool:         &mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}},
		})
	}
	return out
}

// executorFake responde toda chamada com sucesso: o roteamento da chamada é
// testado na integração, não aqui.
type executorFake struct{}

func (executorFake) Chamar(context.Context, int64, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
}

func montar(t *testing.T, reg endpoint.Registro, nomes ...string) (*endpoint.Servidores, *repoFake, *catalogoFake) {
	t.Helper()

	repo := &repoFake{regs: []endpoint.Registro{reg}}
	cat := &catalogoFake{ferramentas: ferramentasChamadas(nomes...)}
	sut := endpoint.NovoServidores(repo, cat, executorFake{}, slog.New(slog.DiscardHandler))
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar inicial: erro = %v, quer nil", err)
	}
	return sut, repo, cat
}

// ligarCliente conecta um cliente MCP de verdade ao *mcp.Server do endpoint, por
// transporte em memória.
//
// É o cliente que dá a verdade sobre quais ferramentas o servidor tem: a lista
// interna do patchbay é justamente o que pode divergir, e conferi-la contra si
// mesma não provaria nada.
func ligarCliente(t *testing.T, srv *mcp.Server, opcoes *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()

	doCliente, doServidor := mcp.NewInMemoryTransports()
	ctx := context.Background()

	sessaoServidor, err := srv.Connect(ctx, doServidor, nil)
	if err != nil {
		t.Fatalf("conectar servidor: erro = %v, quer nil", err)
	}
	t.Cleanup(func() { _ = sessaoServidor.Close() })

	cliente := mcp.NewClient(&mcp.Implementation{Name: "cliente-de-teste", Version: "0.0.1"}, opcoes)
	sessao, err := cliente.Connect(ctx, doCliente, nil)
	if err != nil {
		t.Fatalf("conectar cliente: erro = %v, quer nil", err)
	}
	t.Cleanup(func() { _ = sessao.Close() })
	return sessao
}

func nomesDoServidor(t *testing.T, sessao *mcp.ClientSession) []string {
	t.Helper()

	res, err := sessao.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: erro = %v, quer nil", err)
	}
	nomes := make([]string, 0, len(res.Tools))
	for _, f := range res.Tools {
		nomes = append(nomes, f.Name)
	}
	slices.Sort(nomes)
	return nomes
}

// TestServidores_RematerializarRemoveOQueSaiuDoCatalogo cobre o RemoveTools: a
// ferramenta que deixou de existir no upstream sai do *mcp.Server, e não só da
// contabilidade interna.
func TestServidores_RematerializarRemoveOQueSaiuDoCatalogo(t *testing.T) {
	t.Parallel()

	sut, _, cat := montar(t, endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, "alfa", "beta")
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"alfa", "beta"}) {
		t.Fatalf("ferramentas = %v, quer [alfa beta]", nomes)
	}

	cat.definir("beta")
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"beta"}) {
		t.Errorf("ferramentas = %v, quer [beta]; alfa ficou como zumbi", nomes)
	}
	if n := sut.Contagem(slugDeTeste); n != 1 {
		t.Errorf("contagem = %d, quer 1", n)
	}
}

// TestServidores_ListChangedChegaAoCliente prova que a rematerialização notifica
// as sessões vivas: sem isso o cliente segue com o catálogo velho em cache.
func TestServidores_ListChangedChegaAoCliente(t *testing.T) {
	t.Parallel()

	sut, _, cat := montar(t, endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, "alfa")

	avisado := make(chan struct{}, 8)
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case avisado <- struct{}{}:
			default:
			}
		},
	})

	cat.definir("alfa", "gama")
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	select {
	case <-avisado:
	case <-time.After(10 * time.Second):
		t.Fatal("tools/list_changed não chegou ao cliente em 10s")
	}
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"alfa", "gama"}) {
		t.Errorf("ferramentas = %v, quer [alfa gama]", nomes)
	}
}

// TestServidores_SincronizarConcorrente é o teste da corrida de materialização.
//
// AoMudar dispara de uma goroutine por upstream, então dois upstreams que ficam
// prontos quase juntos entram aqui em paralelo. Sem o mutex por endpoint cobrindo
// cálculo e aplicação, a última escrita sobrepõe um snapshot mais novo — e o
// resultado é ferramenta morta que fica ou ferramenta viva que sai. Roda com
// -race.
func TestServidores_SincronizarConcorrente(t *testing.T) {
	t.Parallel()

	sut, _, cat := montar(t, endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, "alfa")
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	conjuntos := [][]string{
		{"alfa"},
		{"alfa", "beta"},
		{"beta", "gama"},
		{},
		{"delta"},
	}

	var grupo sync.WaitGroup
	for i := range 24 {
		grupo.Add(1)
		go func(i int) {
			defer grupo.Done()
			cat.definir(conjuntos[i%len(conjuntos)]...)
			if err := sut.Sincronizar(context.Background()); err != nil {
				t.Errorf("Sincronizar concorrente: erro = %v, quer nil", err)
			}
		}(i)
	}
	grupo.Wait()

	// Depois da rajada, um estado final e uma sincronização: o servidor tem que
	// expor exatamente o conjunto final, sem sobra nem falta.
	cat.definir("final-a", "final-b")
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar final: erro = %v, quer nil", err)
	}

	quer := []string{"final-a", "final-b"}
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, quer) {
		t.Errorf("ferramentas no servidor = %v, quer %v", nomes, quer)
	}
	if nomes := sut.Expostos(slugDeTeste); !slices.Equal(nomes, quer) {
		t.Errorf("expostos = %v, quer %v; a contabilidade interna divergiu do servidor", nomes, quer)
	}
}

// TestServidores_EndpointRemovidoSaiDoArEFechaSessao: o endpoint apagado do banco
// sai do mapa e a sessão MCP que estava aberta nele é encerrada. Sem o fechamento,
// o cliente continuaria conversando com um endpoint que não existe.
func TestServidores_EndpointRemovidoSaiDoArEFechaSessao(t *testing.T) {
	t.Parallel()

	sut, repo, _ := montar(t, endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, "alfa")
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	if nomes := nomesDoServidor(t, sessao); len(nomes) != 1 {
		t.Fatalf("ferramentas = %v, quer uma", nomes)
	}

	repo.definir()
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	if sut.Existe(slugDeTeste) {
		t.Error("endpoint continua no ar depois de sair do banco")
	}
	if _, err := sessao.ListTools(context.Background(), nil); err == nil {
		t.Error("tools/list na sessão antiga = nil, quer erro de sessão encerrada")
	}
}

// TestServidores_IdentidadeEditadaRecriaOServidor: nome, descrição e instruções
// entram no *mcp.Server na construção e o SDK não deixa trocá-los numa instância
// viva, então editá-los recria o servidor e encerra as sessões daquele endpoint.
// Mudar só a composição não passa por aqui.
func TestServidores_IdentidadeEditadaRecriaOServidor(t *testing.T) {
	t.Parallel()

	reg := endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal", Instrucoes: "use com calma"}
	sut, repo, cat := montar(t, reg, "alfa")
	antes := sut.Servidor(slugDeTeste)
	sessao := ligarCliente(t, antes, nil)

	// Composição muda: mesma instância, sessão sobrevive.
	cat.definir("alfa", "beta")
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}
	if sut.Servidor(slugDeTeste) != antes {
		t.Fatal("mudança de composição recriou o *mcp.Server, quer a mesma instância")
	}
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"alfa", "beta"}) {
		t.Fatalf("ferramentas = %v, quer [alfa beta]", nomes)
	}

	// Instruções mudam: instância nova, catálogo reconstruído nela.
	reg.Instrucoes = "outro texto"
	repo.definir(reg)
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar depois da edição: erro = %v, quer nil", err)
	}
	depois := sut.Servidor(slugDeTeste)
	if depois == antes {
		t.Fatal("instruções editadas não recriaram o *mcp.Server")
	}

	nova := ligarCliente(t, depois, nil)
	if nomes := nomesDoServidor(t, nova); !slices.Equal(nomes, []string{"alfa", "beta"}) {
		t.Errorf("ferramentas na instância nova = %v, quer [alfa beta]", nomes)
	}
}
