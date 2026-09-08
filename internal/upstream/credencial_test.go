package upstream_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// bufferSincronizado deixa o slog escrever de uma goroutine enquanto o teste lê
// de outra. Sem ele o -race acusa a corrida, que é real: a supervisão loga fora
// da goroutine do teste.
type bufferSincronizado struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bufferSincronizado) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *bufferSincronizado) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// espiao guarda os headers que chegaram ao upstream.
type espiao struct {
	mu       sync.Mutex
	recebido http.Header
}

func (e *espiao) anotar(h http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.recebido == nil {
		e.recebido = h.Clone()
	}
}

func (e *espiao) headers() http.Header {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recebido
}

// servidorEspiao é o servidor MCP de verdade do go-sdk com um espião de headers
// na frente: o que ele prova é o que o upstream real veria.
func servidorEspiao(t *testing.T) (string, *espiao) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-espiao", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "eco", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})

	e := &espiao{}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.anotar(r.Header)
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL, e
}

const (
	bearerDeTeste = "sk-upstream-bearer-que-nao-pode-vazar"
	headerDeTeste = "chave-de-header-que-nao-pode-vazar"
)

// TestGerente_CredenciaisChegamAoUpstream é a demonstração da fatia do lado do
// transporte: o bearer e o header estático saem na requisição, e saem em header
// — nunca na URL, que vaza em log de proxy, histórico e Referer.
func TestGerente_CredenciaisChegamAoUpstream(t *testing.T) {
	t.Parallel()

	alvo, espia := servidorEspiao(t)
	registro := &bufferSincronizado{}
	log := slog.New(slog.NewJSONHandler(registro, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := upstream.Config{
		ID: 3, Nome: "com-credencial", Tipo: upstream.TipoHTTP,
		URL: alvo, Timeout: 5 * time.Second,
	}
	mudou := make(chan struct{}, 64)
	sut := upstream.NovoGerente(log, []upstream.Config{cfg},
		upstream.ComIntervaloTentativa(20*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
		upstream.ComCredenciais(func(_ context.Context, id int64) ([]upstream.Credencial, error) {
			if id != cfg.ID {
				t.Errorf("credenciais pedidas para o upstream %d, quer %d", id, cfg.ID)
			}
			return []upstream.Credencial{
				{Tipo: upstream.CredencialBearer, Valor: cripto.Segredo(bearerDeTeste)},
				{Tipo: upstream.CredencialHeader, Nome: "X-Api-Key", Valor: cripto.Segredo(headerDeTeste)},
			}, nil
		}),
	)

	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})
	esperarPronto(t, sut, cfg.ID, mudou)

	recebido := espia.headers()
	if recebido == nil {
		t.Fatal("nenhuma requisição chegou ao upstream")
	}
	if got, quer := recebido.Get("Authorization"), "Bearer "+bearerDeTeste; got != quer {
		t.Errorf("Authorization = %q, quer %q", got, quer)
	}
	if got := recebido.Get("X-Api-Key"); got != headerDeTeste {
		t.Errorf("X-Api-Key = %q, quer %q", got, headerDeTeste)
	}

	// E a chamada de ferramenta funciona pelo mesmo transporte.
	if _, err := sut.Chamar(ctx, cfg.ID, "eco", nil); err != nil {
		t.Fatalf("Chamar: erro = %v, quer nil", err)
	}

	// A trilha completa de log — conectar, descobrir, chamar — em nível debug,
	// sem nenhum pedaço de credencial. A tela de log é a via mais fácil de vazar
	// exatamente o que a cifra em repouso protege (seção 11).
	saida := registro.String()
	for _, segredo := range []string{bearerDeTeste, headerDeTeste} {
		if strings.Contains(saida, segredo) {
			t.Errorf("log contém o segredo %q", segredo)
		}
	}
}

// TestGerente_SemCredenciaisNaoMandaHeader garante que o caminho sem credencial
// continua o de antes: nada acrescentado à requisição.
func TestGerente_SemCredenciaisNaoMandaHeader(t *testing.T) {
	t.Parallel()

	alvo, espia := servidorEspiao(t)

	cfg := upstream.Config{
		ID: 4, Nome: "sem-credencial", Tipo: upstream.TipoHTTP,
		URL: alvo, Timeout: 5 * time.Second,
	}
	mudou := make(chan struct{}, 64)
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComIntervaloTentativa(20*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
		upstream.ComCredenciais(func(context.Context, int64) ([]upstream.Credencial, error) {
			return nil, nil
		}),
	)

	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})
	esperarPronto(t, sut, cfg.ID, mudou)

	if got := espia.headers().Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, quer vazio", got)
	}
}

func TestNomeDeHeaderValido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		nome string
		quer bool
	}{
		"nome comum":          {nome: "X-Api-Key", quer: true},
		"com underscore":      {nome: "X_Api_Key", quer: true},
		"com dígito e ponto":  {nome: "x.api2", quer: true},
		"vazio":               {nome: "", quer: false},
		"com espaço":          {nome: "X Api Key", quer: false},
		"com dois-pontos":     {nome: "X:Api", quer: false},
		"com quebra de linha": {nome: "X-Api\r\nInjetado", quer: false},
		"com acento":          {nome: "X-Chave-Válida", quer: false},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := upstream.NomeDeHeaderValido(tc.nome); got != tc.quer {
				t.Errorf("NomeDeHeaderValido(%q) = %v, quer %v", tc.nome, got, tc.quer)
			}
		})
	}
}

func TestValorDeHeaderValido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		valor string
		quer  bool
	}{
		"token opaco":         {valor: "sk-0123456789", quer: true},
		"vazio":               {valor: "", quer: true},
		"com espaço":          {valor: "duas palavras", quer: true},
		"com quebra de linha": {valor: "valor\r\nX-Injetado: sim", quer: false},
		"com tab":             {valor: "valor\tcom-tab", quer: false},
		"com nulo":            {valor: "valor\x00", quer: false},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := upstream.ValorDeHeaderValido(tc.valor); got != tc.quer {
				t.Errorf("ValorDeHeaderValido(%q) = %v, quer %v", tc.valor, got, tc.quer)
			}
		})
	}
}
