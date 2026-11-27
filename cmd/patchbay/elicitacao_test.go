package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// upstreamQueConfirma sobe um servidor stateless (como os nossos) com uma
// ferramenta que só age depois que o usuário confirma, pelo MRTR.
func upstreamQueConfirma(t *testing.T) string {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-confirma", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "apagar", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Como os nossos servidores: cliente que não sabe confirmar segue
			// como antes da elicitação existir.
			if c := req.ClientCapabilities(); c == nil || c.Elicitation == nil {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sem-confirmacao"}}}, nil
			}
			if r, ok := req.Params.InputResponses["confirmar"].(*mcp.ElicitResult); ok {
				if req.Params.RequestState != "estado-1" {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "requestState perdido"}}}, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: r.Action}}}, nil
			}
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{"confirmar": &mcp.ElicitParams{
					Message:         "Apagar mesmo?",
					RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				}},
				RequestState: "estado-1",
			}, nil
		})

	// Ignora a capacidade do cliente e pede sempre: é o caso da defesa do gateway.
	srv.AddTool(
		&mcp.Tool{Name: "apagar_teimoso", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"confirmar": &mcp.ElicitParams{
				Message:         "Apagar mesmo?",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			}}}, nil
		})

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL
}

func conectarCom(t *testing.T, p patchbayDeTeste, opts *mcp.ClientOptions, versao string) *mcp.ClientSession {
	t.Helper()
	cliente := mcp.NewClient(&mcp.Implementation{Name: "cliente-de-teste", Version: "0.0.1"}, opts)
	transporte := &mcp.StreamableClientTransport{
		Endpoint:   p.url + "/mcp/" + p.endpoint,
		HTTPClient: clienteComBearer(p.chave),
		MaxRetries: -1,
	}
	ctx, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancelar)
	sessao, err := cliente.Connect(ctx, transporte, &mcp.ClientSessionOptions{ProtocolVersion: versao})
	if err != nil {
		t.Fatalf("conectar: erro = %v, quer nil", err)
	}
	t.Cleanup(func() { _ = sessao.Close() })
	return sessao
}

func TestElicitacaoAtravessaOGateway(t *testing.T) {
	t.Parallel()

	p := subirPatchbay(t, upstreamQueConfirma(t), 5000)
	select {
	case <-p.sincronizou:
	case <-time.After(15 * time.Second):
		t.Fatal("catálogo não materializou em 15s")
	}

	responde := func(acao string) *mcp.ClientOptions {
		return &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: acao}, nil
		}}
	}

	casos := []struct {
		nome, versao string
		ferramenta   string
		opts         *mcp.ClientOptions
		quer         string
		querErro     bool
	}{
		{nome: "aceita", opts: responde("accept"), quer: "accept"},
		{nome: "recusa", opts: responde("decline"), quer: "decline"},
		// O downstream é stateless: na 2025-11-25 não há como perguntar, e o
		// upstream precisa saber disso para não pedir.
		{nome: "cliente 2025-11-25", versao: "2025-11-25", opts: responde("accept"), quer: "sem-confirmacao"},
		{nome: "cliente sem elicitação", opts: nil, quer: "sem-confirmacao"},
		{nome: "upstream teimoso e cliente sem elicitação", ferramenta: "apagar_teimoso", quer: "não sabe pedir", querErro: true},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			sessao := conectarCom(t, p, c.opts, c.versao)
			nome := c.ferramenta
			if nome == "" {
				nome = "apagar"
			}
			res, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{Name: nome})
			if err != nil {
				t.Fatalf("tools/call apagar: erro = %v, quer nil", err)
			}
			if res.IsError != c.querErro {
				t.Fatalf("IsError = %v, quer %v (%q)", res.IsError, c.querErro, textoDe(res))
			}
			if !strings.Contains(textoDe(res), c.quer) {
				t.Errorf("resultado = %q, quer conter %q", textoDe(res), c.quer)
			}
		})
	}
}
