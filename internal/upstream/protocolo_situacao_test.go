package upstream_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestSituacaoTrazARevisaoDaMCPEOServidor: a tela de MCPs mostra a revisão
// negociada e o nome e a versão que o upstream declarou.
func TestSituacaoTrazARevisaoDaMCPEOServidor(t *testing.T) {
	t.Parallel()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-x", Version: "1.2.3"}, nil)
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close() })

	cfg := upstream.Config{ID: 1, Nome: "x", Tipo: upstream.TipoHTTP, URL: ts.URL, Timeout: 5 * time.Second}
	g, mudou := gerenteDeTeste(t, []upstream.Config{cfg})
	esperarPronto(t, g, 1, mudou)

	s, _ := g.Situacao(1)
	if s.Protocolo != "2026-07-28" || s.Servidor != "upstream-x 1.2.3" {
		t.Errorf("protocolo, servidor = %q, %q; quer 2026-07-28, upstream-x 1.2.3", s.Protocolo, s.Servidor)
	}
}
