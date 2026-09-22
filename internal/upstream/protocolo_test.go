package upstream_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// revisaoDoUpstream é a revisão que o upstream de teste aceita: a mais nova que
// os servidores presos em SDK antigo conhecem, e a que o WhatsApp negociava
// quando o gateway começou a mandar 2026-07-28 para ele.
const revisaoDoUpstream = "2025-11-25"

// TestGerente_ChamarNaoVazaRevisaoDoCliente prova a borda entre as duas
// sessões: a revisão que o cliente negociou com o endpoint não pode aparecer no
// header Mcp-Protocol-Version das chamadas ao upstream, que negociou a dele.
//
// O caminho é o de produção inteiro, porque o vazamento só existe nele: um
// cliente fala com um endpoint stateless (é o stateless que libera a revisão
// 2026-07-28), a ferramenta do endpoint chama o Gerente com o contexto da
// requisição, e o Gerente chama um upstream que só conhece até 2025-11-25.
func TestGerente_ChamarNaoVazaRevisaoDoCliente(t *testing.T) {
	t.Parallel()

	upSrv, visto := servidorUpstreamAntigo(t)

	cfg := upstream.Config{
		ID: 1, Nome: "antigo", Tipo: upstream.TipoHTTP,
		URL: upSrv.URL, Timeout: 5 * time.Second,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg})
	ctxGerente, pararGerente := context.WithCancel(context.Background())
	sut.Iniciar(ctxGerente)
	t.Cleanup(func() {
		pararGerente()
		sut.Aguardar()
	})
	esperarEstado(t, sut, 1, upstream.EstadoPronto)

	// O endpoint: uma ferramenta que só repassa a chamada ao Gerente, com o
	// contexto da requisição do cliente — exatamente o que servidores.go faz.
	endpoint := mcp.NewServer(&mcp.Implementation{Name: "endpoint", Version: "0.0.1"}, nil)
	endpoint.AddTool(
		&mcp.Tool{Name: "eco", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return sut.Chamar(ctx, 1, "eco", json.RawMessage(`{}`))
		})
	epSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return endpoint },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(epSrv.Close)

	ctx, cancelar := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelar()

	cliente := mcp.NewClient(&mcp.Implementation{Name: "cliente", Version: "0.0.1"}, nil)
	sessao, err := cliente.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: epSrv.URL}, nil)
	if err != nil {
		t.Fatalf("conectar no endpoint: %v", err)
	}
	defer sessao.Close()

	if _, err := sessao.CallTool(ctx, &mcp.CallToolParams{Name: "eco"}); err != nil {
		t.Fatalf("chamar pelo endpoint: %v", err)
	}

	revisaoDoCliente := sessao.InitializeResult().ProtocolVersion
	got := visto.ler()
	if got != revisaoDoUpstream {
		t.Errorf("upstream viu Mcp-Protocol-Version = %q, quer %q (a que ele negociou)", got, revisaoDoUpstream)
	}
	if got == revisaoDoCliente && revisaoDoCliente != revisaoDoUpstream {
		t.Errorf("a revisão do cliente (%q) vazou para o upstream", revisaoDoCliente)
	}
}

// cabecalhoVisto guarda o Mcp-Protocol-Version do último tools/call recebido.
type cabecalhoVisto struct {
	mu  sync.Mutex
	val string
}

func (c *cabecalhoVisto) gravar(v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.val = v
}

func (c *cabecalhoVisto) ler() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// servidorUpstreamAntigo é um upstream MCP que só fala revisaoDoUpstream.
//
// A go-sdk não tem opção de teto de revisão no servidor, então o teto entra
// reescrevendo o initialize que chega: o servidor responde a revisão do pedido,
// e é essa resposta que o cliente guarda como negociada. O resto passa intacto,
// e o handler anota o header de protocolo de cada tools/call.
func servidorUpstreamAntigo(t *testing.T) (*httptest.Server, *cabecalhoVisto) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "antigo", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "eco", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	protocolo := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	visto := &cabecalhoVisto{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		corpo, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body.Close()

		switch {
		case bytes.Contains(corpo, []byte(`"method":"initialize"`)):
			corpo = rebaixarInitialize(corpo)
		case bytes.Contains(corpo, []byte(`"method":"tools/call"`)):
			visto.gravar(r.Header.Get("Mcp-Protocol-Version"))
		}

		r.Body = io.NopCloser(bytes.NewReader(corpo))
		r.ContentLength = int64(len(corpo))
		protocolo.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts, visto
}

// rebaixarInitialize troca a revisão pedida no initialize pela única que este
// upstream conhece.
func rebaixarInitialize(corpo []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(corpo, &msg); err != nil {
		return corpo
	}
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return corpo
	}
	pedida, ok := params["protocolVersion"].(string)
	if !ok || strings.Compare(pedida, revisaoDoUpstream) <= 0 {
		return corpo
	}
	params["protocolVersion"] = revisaoDoUpstream
	novo, err := json.Marshal(msg)
	if err != nil {
		return corpo
	}
	return novo
}
