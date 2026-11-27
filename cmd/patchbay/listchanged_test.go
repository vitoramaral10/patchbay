package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ferramentaEco(nome string) (*mcp.Tool, mcp.ToolHandler) {
	return &mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: nome}}}, nil
		}
}

// TestFerramentaNovaDoUpstreamChegaAoCliente: upstream stateless (como os
// nossos) que ganha ferramenta depois de conectado. O patchbay precisa ouvir o
// aviso, reler a lista e avisar o cliente, sem esperar reconexão.
func TestFerramentaNovaDoUpstreamChegaAoCliente(t *testing.T) {
	t.Parallel()

	up := mcp.NewServer(&mcp.Implementation{Name: "upstream-que-cresce", Version: "0.0.1"}, nil)
	up.AddTool(ferramentaEco("velha"))
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return up }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })

	p := subirPatchbay(t, ts.URL, 5000)
	select {
	case <-p.sincronizou:
	case <-time.After(15 * time.Second):
		t.Fatal("catálogo não materializou em 15s")
	}

	avisado := make(chan struct{}, 4)
	sessao := conectarCom(t, p, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case avisado <- struct{}{}:
			default:
			}
		},
	}, "")

	// A assinatura do patchbay no upstream sai junto com a conexão; dá a ela
	// um instante antes de mudar a lista, como faria um deploy de verdade.
	time.Sleep(300 * time.Millisecond)
	up.AddTool(ferramentaEco("nova"))

	prazo := time.After(15 * time.Second)
	for {
		res, err := sessao.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		nomes := make([]string, 0, len(res.Tools))
		for _, f := range res.Tools {
			nomes = append(nomes, f.Name)
		}
		if slices.Contains(nomes, "nova") {
			break
		}
		select {
		case <-prazo:
			t.Fatalf("ferramentas = %v depois de 15s, quer a nova", nomes)
		case <-time.After(200 * time.Millisecond):
		}
	}
	select {
	case <-avisado:
	case <-time.After(5 * time.Second):
		t.Error("o cliente não recebeu tools/list_changed")
	}
}
