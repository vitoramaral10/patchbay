package main

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// A biblioteca e o formulário de upstream vivem em features diferentes, e a
// regra de arquitetura proíbe uma importar a outra. O contrato entre elas — os
// nomes dos parâmetros da query que preenchem o formulário — não tem, portanto,
// como ser uma constante compartilhada, e o compilador não o confere.
//
// Estes testes são esse contrato. Eles sobem o patchbay inteiro, clicam no link
// que a biblioteca monta e conferem que o formulário do outro lado volta
// preenchido. Se alguém renomear um parâmetro em qualquer um dos dois lados,
// falha aqui.

func TestBibliotecaApareceNoPainel(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
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
		t.Error("a tela não diz de onde o catálogo veio")
	}
}

func TestBibliotecaExigeSessao(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
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

var reAdicionar = regexp.MustCompile(`href="(/admin/upstreams/novo\?[^"]+)"`)

// TestAdicionarDaBibliotecaAbreFormularioPreenchido é o teste de ponta a ponta
// do "só adicionar": ele não constrói a URL, ele tira do HTML o mesmo href em
// que o admin clicaria.
func TestAdicionarDaBibliotecaAbreFormularioPreenchido(t *testing.T) {
	t.Parallel()

	catalogo, err := biblioteca.Embutido()
	if err != nil {
		t.Fatalf("catálogo embutido: erro = %v, quer nil", err)
	}
	escolhido, achou := primeiroComOAuth(catalogo)
	if !achou {
		t.Skip("o catálogo embutido não tem nenhum servidor com OAuth")
	}

	u := subirUI(t)
	u.setup(t)

	pagina := corpo(t, u.abrir(t, webui.RotaBiblioteca+"?q="+escolhido.Slug))
	achados := reAdicionar.FindStringSubmatch(pagina)
	if achados == nil {
		t.Fatalf("a tela filtrada por %q não trouxe nenhum link de adicionar", escolhido.Slug)
	}
	link := html.UnescapeString(achados[1])

	form := corpo(t, u.abrir(t, link))
	casos := map[string]string{
		"nome": `value="` + html.EscapeString(escolhido.Nome) + `"`,
		"url":  `value="` + html.EscapeString(escolhido.URL) + `"`,
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

func primeiroComOAuth(c *biblioteca.Catalogo) (biblioteca.Item, bool) {
	for _, i := range c.Todos() {
		// Slug sem hífen e nome de uma palavra: o teste filtra a tela pelo slug e
		// compara o nome dentro de um atributo HTML, e quanto mais simples o
		// item, menos o teste depende de escape para provar o que quer provar.
		if i.Autenticacao == biblioteca.AutOAuth &&
			!strings.ContainsAny(i.Slug, "-") && !strings.Contains(i.Nome, " ") {
			return i, true
		}
	}
	return biblioteca.Item{}, false
}
