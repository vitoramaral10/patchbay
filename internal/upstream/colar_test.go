package upstream

import (
	"strings"
	"testing"
)

func TestLerFormasLivres(t *testing.T) {
	casos := []struct {
		nome, texto, tipo, nomeMCP, url, comando, args string
	}{
		{"url pura", "https://mcp.canva.com/mcp", TipoHTTP, "canva", "https://mcp.canva.com/mcp", "", ""},
		{"npx", "npx -y @modelcontextprotocol/server-filesystem /tmp", TipoSTDIO, "filesystem", "", "npx", "-y\n@modelcontextprotocol/server-filesystem\n/tmp"},
		{"uvx", "uvx mcp-server-fetch", TipoSTDIO, "fetch", "", "uvx", "mcp-server-fetch"},
		{"json desktop", `{"mcpServers":{"github":{"command":"npx","args":["-y","@x/server-github"],"env":{"TOKEN":"abc"}}}}`, TipoSTDIO, "github", "", "npx", "-y\n@x/server-github"},
		{"json url sse", `{"mcpServers":{"lin":{"type":"sse","url":"https://mcp.linear.app/sse"}}}`, TipoSSE, "lin", "https://mcp.linear.app/sse", "", ""},
		{"json vscode", `{"servers":{"ctx":{"type":"http","url":"https://mcp.context7.com/mcp"}}}`, TipoHTTP, "ctx", "https://mcp.context7.com/mcp", "", ""},
		{"servidor solto", `{"url":"https://mcp.context7.com/mcp"}`, TipoHTTP, "context7", "https://mcp.context7.com/mcp", "", ""},
		{"claude continua", "claude mcp add --transport http canva https://mcp.canva.com/mcp", TipoHTTP, "canva", "https://mcp.canva.com/mcp", "", ""},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			imp, err := LerComandoDeInstalacao(c.texto)
			if err != nil {
				t.Fatalf("erro: %v", err)
			}
			f := imp.Form
			if f.Tipo != c.tipo || f.Nome != c.nomeMCP || f.URL != c.url || f.Comando != c.comando || f.ArgsTexto != c.args {
				t.Fatalf("form = tipo %q nome %q url %q cmd %q args %q", f.Tipo, f.Nome, f.URL, f.Comando, f.ArgsTexto)
			}
		})
	}
}

func TestJSONComVariosServidoresPedeUmPorVez(t *testing.T) {
	_, err := LerComandoDeInstalacao(`{"mcpServers":{"a":{"url":"https://a.dev/mcp"},"b":{"url":"https://b.dev/mcp"}}}`)
	if err == nil || !strings.Contains(err.Error(), "um servidor por vez") {
		t.Fatalf("erro = %v", err)
	}
}

func TestJSONInvalido(t *testing.T) {
	if _, err := LerComandoDeInstalacao(`{"mcpServers": `); err == nil {
		t.Fatal("JSON quebrado foi aceito")
	}
}
