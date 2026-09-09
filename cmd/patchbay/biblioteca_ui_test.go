package main

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// A biblioteca e o formulário de upstream vivem em features diferentes, e a
// regra de arquitetura proíbe uma importar a outra. O contrato entre elas — os
// nomes dos parâmetros da query que preenchem o formulário — não tem, portanto,
// como ser uma constante compartilhada, e o compilador não o confere.
//
// Estes testes são esse contrato. Eles sobem o patchbay inteiro contra um
// mcpservers.org de mentira, clicam no botão que a biblioteca desenha e conferem
// que o formulário do outro lado volta preenchido. Se alguém renomear um
// parâmetro em qualquer um dos dois lados, falha aqui.

// A origem de mentira serve o mínimo que o tradutor do pacote sabe ler. As
// amostras da página de verdade ficam em internal/biblioteca/testdata, onde a
// tradução é testada; aqui o que está sob teste é a costura entre as telas.
const (
	indiceFalso = `<!DOCTYPE html><html lang="pt-BR"><body><main>` +
		`<a href="/remote-mcp-servers/acme">` +
		`<div class="truncate text-sm font-semibold">Acme</div>` +
		`<div class="truncate text-xs">Pedidos, notas e clientes</div></a>` +
		`</main><!-- espaço para a página não parecer resposta truncada ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`--></body></html>`

	detalheFalso = `<!DOCTYPE html><html lang="pt-BR"><body>` +
		`<h1>Acme</h1><p>Pedidos, notas e clientes</p>` +
		`<h2>Sobre Acme</h2><p>A Acme expõe pedidos e notas para agentes.</p>` +
		`<h2>Detalhes da conexão</h2><code>https://mcp.acme.example/mcp</code>` +
		`<dl><dt>Transporte</dt><dd>Streamable HTTP</dd>` +
		`<dt>Autenticação</dt><dd>OAuth</dd></dl>` +
		`<!-- ---------------------------------------------------------------- ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`------------------------------------------------------------------ ` +
		`--></body></html>`
)

func origemFalsa(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /remote-mcp-servers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(indiceFalso))
	})
	mux.HandleFunc("GET /remote-mcp-servers/acme", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(detalheFalso))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestBibliotecaApareceNoPainel(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComOrigemDaBiblioteca(origemFalsa(t)))
	u.setup(t)

	res := u.abrir(t, webui.RotaBiblioteca)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, quer %d", webui.RotaBiblioteca, res.StatusCode, http.StatusOK)
	}
	pagina := corpo(t, res)
	if !strings.Contains(pagina, `href="`+webui.RotaBiblioteca+`"`) {
		t.Error("a navegação do painel não tem o item Biblioteca")
	}
	if !strings.Contains(pagina, "mcpservers.org") {
		t.Error("a tela não diz de onde a lista veio")
	}
	if !strings.Contains(pagina, "Acme") {
		t.Error("a tela não mostra o que a origem publicou")
	}
}

func TestBibliotecaExigeSessao(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComOrigemDaBiblioteca(origemFalsa(t)))
	u.setup(t)
	// Sem o setup a tela nem existiria; o que se testa é o portão, então a
	// sessão é descartada depois de criada.
	u.cliente.Jar = nil

	res := u.abrir(t, webui.RotaBiblioteca)
	if res.Request.URL.Path != webui.RotaLogin {
		t.Fatalf("sem sessão, GET %s caiu em %q, quer o login",
			webui.RotaBiblioteca, res.Request.URL.Path)
	}
}

var reAdicionar = regexp.MustCompile(`href="(/admin/biblioteca/[a-z0-9-]+/adicionar)"`)

// TestAdicionarDaBibliotecaAbreFormularioPreenchido é o teste de ponta a ponta
// do "só adicionar": ele não constrói o caminho, tira do HTML o mesmo href em
// que o admin clicaria, e o segue até o formulário.
func TestAdicionarDaBibliotecaAbreFormularioPreenchido(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComOrigemDaBiblioteca(origemFalsa(t)))
	u.setup(t)

	pagina := corpo(t, u.abrir(t, webui.RotaBiblioteca))
	achados := reAdicionar.FindStringSubmatch(pagina)
	if achados == nil {
		t.Fatalf("a tela não trouxe nenhum botão de adicionar")
	}

	// O cliente segue o 303, então o que chega é o formulário de upstream.
	res := u.abrir(t, html.UnescapeString(achados[1]))
	if !strings.HasPrefix(res.Request.URL.Path, webui.RotaUpstreams+"/novo") {
		t.Fatalf("adicionar caiu em %q, quer o formulário de upstream novo", res.Request.URL.Path)
	}
	form := corpo(t, res)
	casos := map[string]string{
		"nome": `value="Acme"`,
		"url":  `value="https://mcp.acme.example/mcp"`,
	}
	for campo, quer := range casos {
		if !strings.Contains(form, quer) {
			t.Errorf("o formulário não veio com %s preenchido (queria %s)", campo, quer)
		}
	}
	// O modo é radio, e o preenchido é o que vem marcado. Sem isto o admin
	// salvaria em credencial estática um servidor que só fala OAuth.
	if !strings.Contains(form, `value="oauth"`) || !strings.Contains(form, "checked") {
		t.Error("o formulário não veio com o modo OAuth marcado")
	}
}

// TestBibliotecaComOrigemForaNaoDerrubaOPainel: a tela depende da internet por
// decisão, e o resto do patchbay não. Um mcpservers.org fora do ar não pode
// virar erro de gateway nem atrapalhar as outras telas.
func TestBibliotecaComOrigemForaNaoDerrubaOPainel(t *testing.T) {
	t.Parallel()

	fora := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora do ar", http.StatusBadGateway)
	}))
	t.Cleanup(fora.Close)

	u := subirUI(t, ComOrigemDaBiblioteca(fora.URL))
	u.setup(t)

	res := u.abrir(t, webui.RotaBiblioteca)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	if !strings.Contains(corpo(t, res), "Não foi possível falar com o mcpservers.org") {
		t.Error("a tela não explica que a origem está fora")
	}
	if res := u.abrir(t, webui.RotaUpstreams); res.StatusCode != http.StatusOK {
		t.Errorf("a lista de upstreams caiu junto: status = %d", res.StatusCode)
	}
}

// TestPreenchimentoNaoAceitaCredencialNemModoInventado fecha a porta que o
// preenchimento abre: a query passa a alimentar campos do formulário, e ela é
// escrita por quem monta o link.
func TestPreenchimentoNaoAceitaCredencialNemModoInventado(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	form := corpo(t, u.abrir(t, webui.RotaUpstreams+
		"/novo?tipo=http&nome=Teste&url=https%3A%2F%2Fexemplo.test%2Fmcp"+
		"&modo=magico&bearer=segredo-que-nao-deveria-entrar"))

	if strings.Contains(form, "segredo-que-nao-deveria-entrar") {
		t.Error("a query conseguiu preencher uma credencial")
	}
	if strings.Contains(form, `value="magico"`) {
		t.Error("a query conseguiu gravar um modo que o código não conhece")
	}
	if !strings.Contains(form, `value="https://exemplo.test/mcp"`) {
		t.Error("a URL legítima não foi preenchida")
	}
}

// TestPreenchimentoIgnoraURLEmFormularioSTDIO: STDIO não tem URL, e um campo
// preenchido que aquele transporte ignora seria lido como configuração em vigor.
func TestPreenchimentoIgnoraURLEmFormularioSTDIO(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	form := corpo(t, u.abrir(t, webui.RotaUpstreams+
		"/novo?tipo=stdio&nome=Local&url=https%3A%2F%2Fexemplo.test%2Fmcp"))

	if strings.Contains(form, "https://exemplo.test/mcp") {
		t.Error("a URL entrou num formulário STDIO")
	}
	if !strings.Contains(form, `value="Local"`) {
		t.Error("o nome não foi preenchido no formulário STDIO")
	}
}
