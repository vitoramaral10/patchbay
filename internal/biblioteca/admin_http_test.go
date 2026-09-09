package biblioteca_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

func telaSobre(t *testing.T, base string) http.Handler {
	t.Helper()

	mux := http.NewServeMux()
	biblioteca.NovoAdmin(
		biblioteca.NovaOrigem(base, time.Minute),
		slog.New(slog.DiscardHandler),
	).Rotas(mux)
	return mux
}

func abrir(t *testing.T, h http.Handler, alvo string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, alvo, nil)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestTelaMostraOQueAOrigemPublica(t *testing.T) {
	t.Parallel()

	res := abrir(t, telaSobre(t, origemCompleta(t).URL), webui.RotaBiblioteca, false)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, quer %d", res.Code, http.StatusOK)
	}
	corpo := res.Body.String()
	for _, quer := range []string{"Atlassian", "<html", "10 servidores publicados agora pela origem"} {
		if !strings.Contains(corpo, quer) {
			t.Errorf("a tela não traz %q", quer)
		}
	}
}

// TestBuscaAchaPeloResumo é o caso que justifica filtrar aqui em vez de
// delegar: a busca da própria origem casa só pelo nome, e "jira" devolve zero
// remotos lá — o Atlassian, cujo resumo é "Jira, Confluence, Compass", fica de
// fora.
func TestBuscaAchaPeloResumo(t *testing.T) {
	t.Parallel()

	corpo := abrir(t, telaSobre(t, origemCompleta(t).URL),
		webui.RotaBiblioteca+"?q=jira", false).Body.String()
	if !strings.Contains(corpo, "Atlassian") {
		t.Error("busca por jira não trouxe Atlassian")
	}
	if !strings.Contains(corpo, "de 10 servidores") {
		t.Error("a contagem não acompanhou o filtro")
	}
}

// TestRespostaDoHtmxEhSoOFragmento guarda o que o navegador faz com a resposta:
// o htmx troca o conteúdo do alvo pelo corpo inteiro, então uma página completa
// aqui aninharia um <html> dentro do <body> a cada tecla digitada.
func TestRespostaDoHtmxEhSoOFragmento(t *testing.T) {
	t.Parallel()

	corpo := abrir(t, telaSobre(t, origemCompleta(t).URL),
		webui.RotaBiblioteca+"?q=atlassian", true).Body.String()
	if strings.Contains(corpo, "<html") || strings.Contains(corpo, "<nav") {
		t.Error("a resposta do htmx trouxe a página inteira, e não o fragmento")
	}
	if !strings.Contains(corpo, `id="resultados-biblioteca"`) {
		t.Error("o fragmento não traz o id do alvo, então a troca não acha onde encaixar")
	}
}

// TestOrigemForaExplicaEOfereceSaida é a contrapartida de não guardar nada: sem
// rede não há lista, e a tela precisa dizer isso em vez de parecer um catálogo
// vazio ou um defeito do gateway.
func TestOrigemForaExplicaEOfereceSaida(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora do ar", http.StatusBadGateway)
	}))
	t.Cleanup(ts.Close)

	res := abrir(t, telaSobre(t, ts.URL), webui.RotaBiblioteca, false)
	// 200 e não 502: quem está fora é o mcpservers.org, não o patchbay, e um
	// erro de servidor aqui mandaria o admin procurar defeito no lugar errado.
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, quer %d", res.Code, http.StatusOK)
	}
	corpo := res.Body.String()
	if !strings.Contains(corpo, "Não foi possível falar com o mcpservers.org") {
		t.Error("a tela não explica que a origem está fora")
	}
	if !strings.Contains(corpo, webui.RotaUpstreams+"/novo") {
		t.Error("a tela não oferece o cadastro à mão")
	}
}

func TestFormatoDaOrigemEDistintoDeOrigemFora(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(
		pagina("<!DOCTYPE html><html><body><h1>oi</h1>" + strings.Repeat(" ", 700) + "</body></html>")))
	t.Cleanup(ts.Close)

	corpo := abrir(t, telaSobre(t, ts.URL), webui.RotaBiblioteca, false).Body.String()
	// A distinção importa porque a ação do admin é outra: um pede para tentar de
	// novo, o outro pede um commit aqui.
	if !strings.Contains(corpo, "formato que o patchbay não sabe ler") {
		t.Error("a tela não distingue marcação mudada de origem fora do ar")
	}
}

func TestAdicionarRedirecionaParaFormularioPreenchido(t *testing.T) {
	t.Parallel()

	res := abrir(t, telaSobre(t, origemCompleta(t).URL),
		webui.RotaBiblioteca+"/notion/adicionar", false)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, quer %d", res.Code, http.StatusSeeOther)
	}
	destino := res.Header().Get("Location")
	for _, quer := range []string{
		webui.RotaUpstreams + "/novo?",
		"nome=Notion",
		"tipo=http",
		"modo=oauth",
		"url=https%3A%2F%2Fmcp.notion.com%2Fmcp",
	} {
		if !strings.Contains(destino, quer) {
			t.Errorf("destino %q não traz %q", destino, quer)
		}
	}
	// Query entra em histórico do navegador, log de proxy e Referer.
	for _, proibido := range []string{"bearer", "token", "senha", "client_secret"} {
		if strings.Contains(destino, proibido) {
			t.Errorf("o destino carrega %q, e não deveria carregar credencial", proibido)
		}
	}
}

func TestAdicionarServidorQueSaiuDaOrigem(t *testing.T) {
	t.Parallel()

	res := abrir(t, telaSobre(t, origemCompleta(t).URL),
		webui.RotaBiblioteca+"/nao-existe/adicionar", false)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, quer %d", res.Code, http.StatusSeeOther)
	}
	if destino := res.Header().Get("Location"); !strings.Contains(destino, "aviso=sumiu") {
		t.Fatalf("destino = %q, quer voltar à biblioteca com o aviso", destino)
	}
}

// TestTelaEscapaOQueVeioDaOrigem: o catálogo é HTML de terceiro, e ele chega à
// tela pelo mesmo caminho de qualquer texto — templ escapa. O teste existe para
// que trocar { } por @templ.Raw algum dia quebre alguma coisa.
func TestTelaEscapaOQueVeioDaOrigem(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(pagina(
		`<!DOCTYPE html><html><body><a href="/remote-mcp-servers/malicioso">` +
			`<div class="truncate x">&lt;script&gt;alert(1)&lt;/script&gt;</div>` +
			`<div class="truncate y">resumo</div></a>` + strings.Repeat(" ", 700) + `</body></html>`)))
	t.Cleanup(ts.Close)

	corpo := abrir(t, telaSobre(t, ts.URL), webui.RotaBiblioteca, false).Body.String()
	if strings.Contains(corpo, "<script>alert(1)</script>") {
		t.Fatal("o nome vindo da origem saiu sem escape")
	}
	if !strings.Contains(corpo, "&lt;script&gt;") {
		t.Fatal("o nome não apareceu escapado — o teste não está olhando o lugar certo")
	}
}
