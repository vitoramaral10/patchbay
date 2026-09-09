package biblioteca_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

func servidorDeTeste(t *testing.T, itens ...biblioteca.Item) http.Handler {
	t.Helper()

	c, err := biblioteca.Carregar(comoJSON(t, itens...))
	if err != nil {
		t.Fatalf("Carregar: erro = %v, quer nil", err)
	}
	mux := http.NewServeMux()
	biblioteca.NovoAdmin(c, slog.New(slog.DiscardHandler)).Rotas(mux)
	return mux
}

func dois(t *testing.T) http.Handler {
	t.Helper()

	jira := item()
	jira.Slug, jira.Nome, jira.Resumo = "atlassian", "Atlassian", "Jira, Confluence, Compass"
	notas := item()
	notas.Slug, notas.Nome, notas.Resumo = "notion", "Notion", "Páginas e bancos"
	notas.URL = "https://mcp.notion.com/mcp"
	return servidorDeTeste(t, jira, notas)
}

func pedir(t *testing.T, h http.Handler, alvo string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, alvo, nil)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, quer %d", alvo, res.Code, http.StatusOK)
	}
	return res
}

func TestTelaListaTudoSemBusca(t *testing.T) {
	t.Parallel()

	corpo := pedir(t, dois(t), webui.RotaBiblioteca, false).Body.String()
	for _, quer := range []string{"Atlassian", "Notion", "<html", "2 servidores no catálogo"} {
		if !strings.Contains(corpo, quer) {
			t.Errorf("a tela não traz %q", quer)
		}
	}
}

func TestTelaFiltraPeloTermo(t *testing.T) {
	t.Parallel()

	corpo := pedir(t, dois(t), webui.RotaBiblioteca+"?q=jira", false).Body.String()
	if !strings.Contains(corpo, "Atlassian") {
		t.Error("busca por jira não trouxe Atlassian")
	}
	if strings.Contains(corpo, ">Notion<") {
		t.Error("busca por jira trouxe Notion")
	}
	if !strings.Contains(corpo, "1 de 2 servidores") {
		t.Error("a contagem não acompanhou o filtro")
	}
}

// TestRespostaDoHtmxEhSoOFragmento guarda o que o navegador faz com a resposta:
// o htmx troca o conteúdo do alvo pelo corpo inteiro, então uma página completa
// aqui aninharia um <html> dentro do <body> a cada tecla digitada.
func TestRespostaDoHtmxEhSoOFragmento(t *testing.T) {
	t.Parallel()

	corpo := pedir(t, dois(t), webui.RotaBiblioteca+"?q=notion", true).Body.String()
	if strings.Contains(corpo, "<html") || strings.Contains(corpo, "<nav") {
		t.Error("a resposta do htmx trouxe a página inteira, e não o fragmento")
	}
	if !strings.Contains(corpo, `id="resultados-biblioteca"`) {
		t.Error("o fragmento não traz o id do alvo, então a troca não acha onde encaixar")
	}
	if !strings.Contains(corpo, "Notion") {
		t.Error("o fragmento não traz o resultado")
	}
}

func TestBuscaSemResultadoOfereceCadastroManual(t *testing.T) {
	t.Parallel()

	corpo := pedir(t, dois(t), webui.RotaBiblioteca+"?q=nao-existe", false).Body.String()
	if !strings.Contains(corpo, "Nenhum servidor com esse termo") {
		t.Error("busca vazia não explica o que aconteceu")
	}
	if !strings.Contains(corpo, webui.RotaUpstreams+"/novo") {
		t.Error("busca vazia não oferece o cadastro à mão")
	}
}

// TestTelaEscapaOQueVeioDaOrigem: o catálogo é dado de terceiro, e ele chega à
// tela pelo mesmo caminho de qualquer texto — templ escapa. O teste existe para
// que trocar { } por @templ.Raw algum dia quebre alguma coisa.
func TestTelaEscapaOQueVeioDaOrigem(t *testing.T) {
	t.Parallel()

	malicioso := item()
	malicioso.Slug = "malicioso"
	malicioso.Nome = `<script>alert(1)</script>`

	corpo := pedir(t, servidorDeTeste(t, malicioso), webui.RotaBiblioteca, false).Body.String()
	if strings.Contains(corpo, "<script>alert(1)</script>") {
		t.Fatal("o nome vindo da origem saiu sem escape")
	}
	if !strings.Contains(corpo, "&lt;script&gt;") {
		t.Fatal("o nome não apareceu escapado — o teste não está olhando o lugar certo")
	}
}

// TestCatalogoVazioExplicaOQueAconteceu cobre a degradação: quando o
// instantâneo embutido não carrega, a aplicação serve um catálogo vazio em vez
// de recusar subir, e a tela precisa dizer isso — uma lista sumida sem
// explicação parece defeito da busca.
func TestCatalogoVazioExplicaOQueAconteceu(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	biblioteca.NovoAdmin(biblioteca.Vazio(), slog.New(slog.DiscardHandler)).Rotas(mux)

	corpo := pedir(t, mux, webui.RotaBiblioteca, false).Body.String()
	if !strings.Contains(corpo, "O catálogo não carregou") {
		t.Error("catálogo vazio não explica o que aconteceu")
	}
	if strings.Contains(corpo, "Nenhum servidor com esse termo") {
		t.Error("catálogo vazio foi confundido com busca sem resultado")
	}
}
