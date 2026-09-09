package biblioteca_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// item completo, para os testes mexerem em um campo por vez.
func item() biblioteca.Item {
	return biblioteca.Item{
		Slug:         "acme",
		Nome:         "Acme",
		Resumo:       "Pedidos, notas e clientes",
		URL:          "https://mcp.acme.example/mcp",
		Transporte:   biblioteca.TransporteHTTP,
		Autenticacao: biblioteca.AutOAuth,
	}
}

func comoJSON(t *testing.T, itens ...biblioteca.Item) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("[")
	for n, i := range itens {
		if n > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"slug":"` + i.Slug + `","nome":"` + i.Nome +
			`","resumo":"` + i.Resumo + `","url":"` + i.URL +
			`","transporte":"` + i.Transporte + `","autenticacao":"` + i.Autenticacao + `"}`)
	}
	b.WriteString("]")
	return []byte(b.String())
}

func TestCarregarRecusaItemQueNaoDaParaCadastrar(t *testing.T) {
	t.Parallel()

	casos := map[string]func(i *biblioteca.Item){
		"sem slug":               func(i *biblioteca.Item) { i.Slug = "" },
		"sem nome":               func(i *biblioteca.Item) { i.Nome = "" },
		"sem URL":                func(i *biblioteca.Item) { i.URL = "" },
		"transporte vazio":       func(i *biblioteca.Item) { i.Transporte = "" },
		"transporte inventado":   func(i *biblioteca.Item) { i.Transporte = "grpc" },
		"autenticação vazia":     func(i *biblioteca.Item) { i.Autenticacao = "" },
		"autenticação inventada": func(i *biblioteca.Item) { i.Autenticacao = "magica" },
	}
	for nome, estragar := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()
			i := item()
			estragar(&i)
			if _, err := biblioteca.Carregar(comoJSON(t, i)); err == nil {
				t.Fatalf("Carregar(%s): erro = nil, quer recusa", nome)
			}
		})
	}
}

func TestCarregarRecusaSlugRepetido(t *testing.T) {
	t.Parallel()

	if _, err := biblioteca.Carregar(comoJSON(t, item(), item())); err == nil {
		t.Fatal("Carregar com slug repetido: erro = nil, quer recusa")
	}
}

func TestCarregarOrdenaPeloNome(t *testing.T) {
	t.Parallel()

	zeta, alfa := item(), item()
	zeta.Slug, zeta.Nome = "zeta", "Zeta"
	alfa.Slug, alfa.Nome = "alfa", "alfa minúscula"

	c, err := biblioteca.Carregar(comoJSON(t, zeta, alfa))
	if err != nil {
		t.Fatalf("Carregar: erro = %v, quer nil", err)
	}
	todos := c.Todos()
	if len(todos) != 2 || todos[0].Slug != "alfa" {
		t.Fatalf("ordem = %v, quer alfabética sem distinguir caixa", nomes(todos))
	}
	// O índice por slug precisa sobreviver à ordenação: um Item que devolve o
	// vizinho é um botão "adicionar" que cadastra o servidor errado.
	achado, ok := c.Item("zeta")
	if !ok || achado.Nome != "Zeta" {
		t.Fatalf("Item(\"zeta\") = %v, %v, quer o item Zeta", achado.Nome, ok)
	}
}

func TestBuscar(t *testing.T) {
	t.Parallel()

	jira := item()
	jira.Slug, jira.Nome, jira.Resumo = "atlassian", "Atlassian", "Jira, Confluence, Compass"
	notas := item()
	notas.Slug, notas.Nome, notas.Resumo = "notion", "Notion", "Páginas e bancos de dados"

	c, err := biblioteca.Carregar(comoJSON(t, jira, notas))
	if err != nil {
		t.Fatalf("Carregar: erro = %v, quer nil", err)
	}

	casos := map[string]struct {
		termo string
		quer  []string
	}{
		"vazio devolve tudo":           {"", []string{"atlassian", "notion"}},
		"acha pelo nome":               {"notion", []string{"notion"}},
		"ignora a caixa":               {"NOTION", []string{"notion"}},
		"acha pelo resumo":             {"jira", []string{"atlassian"}},
		"dois termos exigem os dois":   {"atlassian confluence", []string{"atlassian"}},
		"ordem dos termos não importa": {"confluence atlassian", []string{"atlassian"}},
		"termo que não existe":         {"kubernetes", nil},
	}
	for nome, caso := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()
			achados := nomesDeSlug(c.Buscar(caso.termo))
			if strings.Join(achados, ",") != strings.Join(caso.quer, ",") {
				t.Fatalf("Buscar(%q) = %v, quer %v", caso.termo, achados, caso.quer)
			}
		})
	}
}

func TestRotaCadastroLevaOsCamposParaOFormulario(t *testing.T) {
	t.Parallel()

	i := item()
	i.Nome = "Acme & Cia"

	bruta := i.RotaCadastro()
	caminho, consulta, achou := strings.Cut(bruta, "?")
	if !achou {
		t.Fatalf("RotaCadastro() = %q, quer caminho com query", bruta)
	}
	if caminho != "/admin/upstreams/novo" {
		t.Fatalf("caminho = %q, quer /admin/upstreams/novo", caminho)
	}
	q, err := url.ParseQuery(consulta)
	if err != nil {
		t.Fatalf("query %q: erro = %v, quer nil", consulta, err)
	}
	quer := map[string]string{
		"tipo": "http",
		"nome": "Acme & Cia",
		"url":  "https://mcp.acme.example/mcp",
		"modo": "oauth",
	}
	for campo, valor := range quer {
		if q.Get(campo) != valor {
			t.Errorf("%s = %q, quer %q", campo, q.Get(campo), valor)
		}
	}
	// Nenhum campo de credencial viaja na URL: query entra em histórico do
	// navegador, log de proxy e Referer.
	for _, proibido := range []string{"bearer", "token", "senha", "client_secret"} {
		if q.Has(proibido) {
			t.Errorf("query carrega %q, e não deveria carregar credencial", proibido)
		}
	}
}

func TestModoDeCredencial(t *testing.T) {
	t.Parallel()

	casos := map[string]string{
		biblioteca.AutOAuth:  "oauth",
		biblioteca.AutToken:  "estatica",
		biblioteca.AutAberta: "estatica",
	}
	for autenticacao, quer := range casos {
		i := item()
		i.Autenticacao = autenticacao
		if tem := i.ModoDeCredencial(); tem != quer {
			t.Errorf("ModoDeCredencial(%s) = %q, quer %q", autenticacao, tem, quer)
		}
	}
}

// TestCatalogoEmbutido é o que impede o instantâneo de chegar quebrado em
// produção.
//
// A aplicação degrada para catálogo vazio quando o embed não carrega, de
// propósito — uma tela de conveniência não derruba o gateway. Este teste é a
// contrapartida: a degradação existe para o impossível, e é aqui que ela
// continua impossível.
func TestCatalogoEmbutido(t *testing.T) {
	t.Parallel()

	c, err := biblioteca.Embutido()
	if err != nil {
		t.Fatalf("Embutido: erro = %v, quer nil", err)
	}
	if c.Tamanho() == 0 {
		t.Fatal("catálogo embutido vazio: rode `task biblioteca` para regerá-lo")
	}

	vistas := map[string]string{}
	for _, i := range c.Todos() {
		if !strings.HasPrefix(i.URL, "https://") {
			t.Errorf("%s: URL = %q, quer https", i.Slug, i.URL)
		}
		if antes, repetida := vistas[i.URL]; repetida {
			t.Errorf("%s e %s apontam para a mesma URL %q", antes, i.Slug, i.URL)
		}
		vistas[i.URL] = i.Slug
		if i.Docs != "" && !strings.HasPrefix(i.Docs, "https://") {
			t.Errorf("%s: Docs = %q, quer https", i.Slug, i.Docs)
		}
		if i.Fonte != "" && !strings.HasPrefix(i.Fonte, "https://") {
			t.Errorf("%s: Fonte = %q, quer https", i.Slug, i.Fonte)
		}
	}
}

func TestVazio(t *testing.T) {
	t.Parallel()

	c := biblioteca.Vazio()
	if c.Tamanho() != 0 {
		t.Fatalf("Tamanho() = %d, quer 0", c.Tamanho())
	}
	if achados := c.Buscar("qualquer"); len(achados) != 0 {
		t.Fatalf("Buscar em catálogo vazio = %v, quer nada", achados)
	}
	if _, ok := c.Item("acme"); ok {
		t.Fatal("Item em catálogo vazio: ok = true, quer false")
	}
}

func nomes(itens []biblioteca.Item) []string {
	var s []string
	for _, i := range itens {
		s = append(s, i.Nome)
	}
	return s
}

func nomesDeSlug(itens []biblioteca.Item) []string {
	var s []string
	for _, i := range itens {
		s = append(s, i.Slug)
	}
	return s
}
