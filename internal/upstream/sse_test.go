package upstream_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// servidorSSEFalso sobe um servidor MCP de verdade falando o HTTP+SSE da revisão
// 2024-11-05, opcionalmente atrás de um bearer.
//
// Servidor de verdade e não um dublê do protocolo: o assunto da fatia é
// interoperar com o transporte legado, e um mock do formato de evento
// concordaria com o meu erro de entendimento em vez de pegá-lo.
func servidorSSEFalso(t *testing.T, bearer string, ferramentas ...string) (urlSSE string, vistos *atomic.Int32) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "sse-falso", Version: "0.0.1"}, nil)
	for _, nome := range ferramentas {
		srv.AddTool(
			&mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}

	autorizadas := &atomic.Int32{}
	protocolo := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer != "" {
			if r.Header.Get("Authorization") != "Bearer "+bearer {
				http.Error(w, "não autorizado", http.StatusUnauthorized)
				return
			}
			autorizadas.Add(1)
		}
		protocolo.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		// CloseClientConnections antes de Close: o GET do transporte SSE fica
		// pendurado, e Close espera pelas requisições em curso.
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL, autorizadas
}

// TestGerente_UpstreamSSELegado é a fatia 14: o tipo sse entra na mesma máquina
// de estados do Streamable HTTP e do STDIO, com as mesmas credenciais.
func TestGerente_UpstreamSSELegado(t *testing.T) {
	t.Parallel()

	urlSSE, _ := servidorSSEFalso(t, "", "consultar", "gravar")

	cfg := upstream.Config{
		ID: 1, Nome: "legado", Tipo: upstream.TipoSSE,
		URL: urlSSE, Timeout: 5 * time.Second,
	}
	g, mudou := gerenteDeTeste(t, []upstream.Config{cfg})

	esperarPronto(t, g, 1, mudou)

	ferramentas := g.Ferramentas(1)
	if len(ferramentas) != 2 {
		t.Fatalf("ferramentas = %d, quer 2", len(ferramentas))
	}
	nomes := []string{ferramentas[0].Name, ferramentas[1].Name}
	if !contem(nomes, "consultar") || !contem(nomes, "gravar") {
		t.Errorf("ferramentas = %v, quer consultar e gravar", nomes)
	}

	// E o tools/call anda pela sessão já aberta, como em qualquer outro tipo.
	res, err := g.Chamar(context.Background(), 1, "consultar", nil)
	if err != nil {
		t.Fatalf("chamar: erro = %v, quer nil", err)
	}
	if res.IsError {
		t.Errorf("resultado com erro, quer sucesso")
	}
}

// TestGerente_UpstreamSSEComCredencialEstatica prova que o bearer estático vale
// para o transporte legado do mesmo jeito que para o Streamable HTTP: do ponto de
// vista de quem autentica, as duas coisas são requisição HTTP com Authorization.
func TestGerente_UpstreamSSEComCredencialEstatica(t *testing.T) {
	t.Parallel()

	const token = "sk-legado-0123456789"
	urlSSE, autorizadas := servidorSSEFalso(t, token, "consultar")

	repo, _ := repositorioDeTeste(t)
	id, err := repo.Criar(context.Background(), upstream.Form{
		Nome: "legado-com-bearer", Tipo: upstream.TipoSSE, URL: urlSSE,
		TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
		Bearer: cripto.Segredo(token),
	})
	if err != nil {
		t.Fatalf("criar upstream: erro = %v, quer nil", err)
	}

	reg, err := repo.Obter(context.Background(), id)
	if err != nil {
		t.Fatalf("obter upstream: erro = %v, quer nil", err)
	}
	if reg.Tipo != upstream.TipoSSE {
		t.Fatalf("tipo gravado = %q, quer %q", reg.Tipo, upstream.TipoSSE)
	}

	mudou := make(chan struct{}, 64)
	g := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{reg.Config()},
		upstream.ComIntervaloTentativa(20*time.Millisecond),
		upstream.ComCredenciais(repo.Credenciais),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
	)
	ctx, cancelar := context.WithCancel(context.Background())
	g.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		g.Aguardar()
	})

	esperarPronto(t, g, id, mudou)
	if autorizadas.Load() == 0 {
		t.Error("nenhuma requisição autorizada, quer o bearer no transporte SSE")
	}
}

// TestGerente_UpstreamSSEQuedaDeConexaoDegradaEReconecta cobre o vazamento do
// stream SSE abandonado do outro lado: uma sessão que já estava pronta e cuja
// conexão cai (o servidor continua de pé — é a rede, ou o processo do lado de
// lá reiniciando) tem que ser detectada, degradar e reconectar sozinha —
// nunca ficar presa numa sessão morta que a supervisão acha que ainda serve.
func TestGerente_UpstreamSSEQuedaDeConexaoDegradaEReconecta(t *testing.T) {
	t.Parallel()

	urlSSE, ts := servidorSSEComServer(t, "consultar")

	mudou := make(chan struct{}, 64)
	g := upstream.NovoGerente(slog.New(slog.DiscardHandler),
		[]upstream.Config{{ID: 1, Nome: "legado-instavel", Tipo: upstream.TipoSSE, URL: urlSSE, Timeout: 5 * time.Second}},
		// Backoff maior que o padrão dos outros testes: degradado precisa durar
		// tempo suficiente para o polling de esperarEstado enxergá-lo, e não só
		// a reconexão que vem logo depois.
		upstream.ComIntervaloTentativa(300*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
	)
	ctx, cancelar := context.WithCancel(context.Background())
	g.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		g.Aguardar()
	})

	esperarPronto(t, g, 1, mudou)

	// Mata a conexão SSE sem derrubar o servidor: o GET pendurado do stream
	// morre à força, e o servidor continua de pé para a reconexão pegar.
	ts.CloseClientConnections()

	esperarEstado(t, g, 1, upstream.EstadoDegradado)
	esperarPronto(t, g, 1, mudou)
}

// servidorSSEComServer é como servidorSSEFalso, mas devolve o *httptest.Server
// também: este teste precisa derrubar a conexão no meio, sem fechar o
// servidor — só CloseClientConnections faz isso, e só quem tem o servidor
// pode chamá-lo.
func servidorSSEComServer(t *testing.T, ferramentas ...string) (urlSSE string, ts *httptest.Server) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "sse-falso-derrubavel", Version: "0.0.1"}, nil)
	for _, nome := range ferramentas {
		srv.AddTool(
			&mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}

	protocolo := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts = httptest.NewServer(protocolo)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL, ts
}

func contem(lista []string, alvo string) bool {
	for _, v := range lista {
		if strings.EqualFold(v, alvo) {
			return true
		}
	}
	return false
}
