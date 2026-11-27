package endpoint

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClienteAtende(t *testing.T) {
	t.Parallel()

	form := &mcp.ElicitParams{Message: "ok?"}
	url := &mcp.ElicitParams{Mode: "url", Message: "abra", URL: "https://exemplo.test"}
	casos := []struct {
		nome string
		cap  *mcp.ElicitationCapabilities
		ir   mcp.InputRequest
		quer bool
	}{
		{"sem capacidade", nil, form, false},
		{"vazia vale form", &mcp.ElicitationCapabilities{}, form, true},
		{"vazia não vale url", &mcp.ElicitationCapabilities{}, url, false},
		{"só url recusa form", &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}, form, false},
		{"url sem modo é url", &mcp.ElicitationCapabilities{}, &mcp.ElicitParams{Message: "abra", URL: "https://exemplo.test"}, false},
		{"url declarada", &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}, url, true},
		{"sampling nunca sobe", &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}, &mcp.CreateMessageParams{}, false},
	}
	for _, c := range casos {
		if got := clienteAtende(c.cap, c.ir); got != c.quer {
			t.Errorf("%s: clienteAtende = %v, quer %v", c.nome, got, c.quer)
		}
	}
}
