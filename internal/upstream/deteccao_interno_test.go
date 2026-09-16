package upstream

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

func TestMetadadosProtegidosDe(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome  string
		url   string
		quer  []string
		vazio bool
	}{
		{
			nome: "caminho entra depois do well-known",
			url:  "https://bindings.mcp.cloudflare.com/mcp",
			quer: []string{
				"https://bindings.mcp.cloudflare.com/.well-known/oauth-protected-resource/mcp",
				"https://bindings.mcp.cloudflare.com/.well-known/oauth-protected-resource",
			},
		},
		{
			nome: "sem caminho é só a raiz",
			url:  "https://exemplo.test",
			quer: []string{"https://exemplo.test/.well-known/oauth-protected-resource"},
		},
		{
			nome: "barra final não vira caminho vazio",
			url:  "https://exemplo.test/",
			quer: []string{"https://exemplo.test/.well-known/oauth-protected-resource"},
		},
		{nome: "esquema que não é http", url: "ftp://exemplo.test/mcp", vazio: true},
		{nome: "não é URL", url: "npx -y algum-mcp", vazio: true},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()
			got := metadadosProtegidosDe(c.url)
			if c.vazio {
				if len(got) != 0 {
					t.Fatalf("candidatas = %v, quer nenhuma", got)
				}
				return
			}
			if !slices.Equal(got, c.quer) {
				t.Errorf("candidatas = %v, quer %v", got, c.quer)
			}
		})
	}
}

// TestNovoDetectorPorMetadados cobre o que o detector aceita como evidência.
//
// O caso do 200 sem authorization_servers é o que mais importa: é o servidor
// que responde alguma coisa a qualquer caminho, e tratá-lo como OAuth criaria
// um upstream sem para onde mandar o admin consentir.
func TestNovoDetectorPorMetadados(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome    string
		caminho string
		corpo   string
		quer    bool
	}{
		{
			nome:    "documento no caminho derivado",
			caminho: "/.well-known/oauth-protected-resource/mcp",
			corpo:   `{"resource":"https://x.test","authorization_servers":["https://as.test"]}`,
			quer:    true,
		},
		{
			nome:    "documento na raiz",
			caminho: "/.well-known/oauth-protected-resource",
			corpo:   `{"resource":"https://x.test","authorization_servers":["https://as.test"]}`,
			quer:    true,
		},
		{
			nome:    "200 sem authorization_servers",
			caminho: "/.well-known/oauth-protected-resource/mcp",
			corpo:   `{"resource":"https://x.test"}`,
		},
		{
			nome:    "200 que não é JSON",
			caminho: "/.well-known/oauth-protected-resource/mcp",
			corpo:   "<!DOCTYPE html><html><body>não achei</body></html>",
		},
		{nome: "nada publicado"},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.caminho == "" || r.URL.Path != c.caminho {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.corpo))
			}))
			t.Cleanup(srv.Close)

			detectar := novoDetectorPorMetadados(srv.Client())
			if got := detectar(context.Background(), srv.URL+"/mcp"); got != c.quer {
				t.Errorf("detectar = %v, quer %v", got, c.quer)
			}
		})
	}
}

// TestAjustarModoPorDescoberta_Guardas prova que o cadastro que já diz o que é
// não passa pela descoberta — nem para confirmar.
//
// Um detector que conta chamadas, e não só o modo gravado no fim: o que se está
// afirmando é que nenhuma dessas colagens fala com a rede, e um teste que só
// olhasse o resultado passaria com uma implementação que sonda o servidor de
// terceiro antes de descartar a resposta.
func TestAjustarModoPorDescoberta_Guardas(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome     string
		form     Form
		semOAuth bool
		querModo string
		querCall bool
	}{
		{
			nome:     "http sem credencial é descoberto",
			form:     Form{Tipo: TipoHTTP, URL: "https://exemplo.test/mcp"},
			querModo: ModoOAuth,
			querCall: true,
		},
		{
			nome:     "stdio não tem modo de credencial",
			form:     Form{Tipo: TipoSTDIO, Comando: "npx"},
			querModo: "",
		},
		{
			nome:     "modo escolhido é escolha, não palpite",
			form:     Form{Tipo: TipoHTTP, URL: "https://exemplo.test/mcp", Modo: ModoNenhum},
			querModo: ModoNenhum,
		},
		{
			nome: "bearer do comando é credencial estática explícita",
			form: Form{
				Tipo: TipoHTTP, URL: "https://exemplo.test/mcp",
				Bearer: cripto.Segredo("sk-do-comando"),
			},
			querModo: "",
		},
		{
			nome: "header do comando também",
			form: Form{
				Tipo: TipoHTTP, URL: "https://exemplo.test/mcp",
				Headers: []CampoHeader{{Nome: "X-Api-Key", Valor: cripto.Segredo("chave")}},
			},
			querModo: "",
		},
		{
			nome:     "sem broker o modo oauth não existe nesta instalação",
			form:     Form{Tipo: TipoHTTP, URL: "https://exemplo.test/mcp"},
			semOAuth: true,
			querModo: "",
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			chamou := false
			a := &Admin{
				log: slog.New(slog.DiscardHandler),
				detectarOAuth: func(context.Context, string) bool {
					chamou = true
					return true
				},
			}
			if !c.semOAuth {
				a.oauth = &BrokerOAuth{}
			}

			form := c.form
			aviso := a.ajustarModoPorDescoberta(context.Background(), &form)

			if form.Modo != c.querModo {
				t.Errorf("modo = %q, quer %q", form.Modo, c.querModo)
			}
			if chamou != c.querCall {
				t.Errorf("detector chamado = %v, quer %v", chamou, c.querCall)
			}
			if temAviso := aviso != ""; temAviso != (c.querModo == ModoOAuth && c.querCall) {
				t.Errorf("aviso = %q, quer coerente com a troca de modo", aviso)
			}
		})
	}
}
