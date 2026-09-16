package main

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// A biblioteca e o formulário de upstream vivem em features diferentes, e a
// regra de arquitetura proíbe uma importar a outra. O contrato entre elas — os
// nomes dos parâmetros da query que preenchem o formulário — não tem, portanto,
// como ser uma constante compartilhada, e o compilador não o confere.
//
// Estes testes são esse contrato. Eles sobem o patchbay inteiro contra um
// mcpservers.org de mentira, esperam a primeira varredura povoar o catálogo,
// clicam no botão que a biblioteca desenha e conferem que o formulário do outro
// lado volta preenchido. Se alguém renomear um parâmetro em qualquer um dos dois
// lados, falha aqui.

// bibliotecaFalsa publica três servidores no acervo /official do
// mcpservers.org: dois locais, cada um com um comando aproveitável, e um
// remoto, que publica endpoint e OAuth do jeito que a origem escreve.
//
// As amostras de resposta de verdade ficam em internal/biblioteca/testdata,
// onde a tradução é testada; aqui o que está sob teste é a costura entre as
// telas. São três servidores porque cada teste precisa achar o seu item na
// lista sem depender do que os outros fazem com o deles.
func bibliotecaFalsa(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><main>` +
			`<a href="/pt-BR/servers/acme-mcp">Acme</a>` +
			`<a href="/pt-BR/servers/acme-local">Acme Local</a>` +
			`<a href="/pt-BR/servers/acme-remoto">Acme Remoto</a>` +
			`</main></body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("slug") {
		case "acme-mcp":
			_, _ = w.Write([]byte(`<html><body><h1>Acme</h1><p>Pedidos, notas e clientes</p>` +
				`<pre>{&quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: ` +
				`[&quot;-y&quot;, &quot;acme-mcp&quot;]}</pre></body></html>`))
		case "acme-local":
			_, _ = w.Write([]byte(`<html><body><h1>Acme Local</h1><p>O mesmo, como processo local</p>` +
				`<pre>{&quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: ` +
				`[&quot;-y&quot;, &quot;acme-mcp@0.4.0&quot;]}</pre></body></html>`))
		case "acme-remoto":
			// A forma em que a origem publica um remoto na lista oficial: sem
			// bloco estruturado, o que há é a prosa da descrição dizendo que o
			// servidor é remoto, com que autenticação, e onde conectar.
			_, _ = w.Write([]byte(`<html><body><h1>Acme Remoto</h1>` +
				`<p>Servidor MCP remoto (HTTP streamable, OAuth 2.1) para pedidos e notas. ` +
				`Conecte-se em https://exemplo.test/api/mcp</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/pt-BR"
}

// esperarBiblioteca espera a primeira varredura de fundo povoar o catálogo e
// devolve a tela já com ele.
//
// A varredura é assíncrona por desenho — em produção ela leva minutos contra a
// origem de verdade. Espera por sinal, nunca pelo relógio: o observador é
// registrado em subirUI, antes de Iniciar, que é onde a varredura começa.
func esperarBiblioteca(t *testing.T, u uiDeTeste) string {
	t.Helper()

	select {
	case <-u.bibliotecaSincronizou:
	case <-time.After(10 * time.Second):
		t.Fatal("a primeira varredura da biblioteca não terminou no prazo")
	}
	pagina := corpo(t, u.abrir(t, webui.RotaBiblioteca))
	if !strings.Contains(pagina, "Acme") {
		t.Fatal("a varredura terminou e a tela não mostra o que a origem publicou")
	}
	return pagina
}

func TestBibliotecaApareceNoPainel(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComCuradoriaDaBiblioteca(bibliotecaFalsa(t)))
	u.setup(t)

	res := u.abrir(t, webui.RotaBiblioteca)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, quer %d", webui.RotaBiblioteca, res.StatusCode, http.StatusOK)
	}
	pagina := esperarBiblioteca(t, u)
	if !strings.Contains(pagina, `href="`+webui.RotaBiblioteca+`"`) {
		t.Error("a navegação do painel não tem o item Biblioteca")
	}
	if !strings.Contains(pagina, "Catálogo local:") {
		t.Error("a tela não diz de quando é a cópia")
	}
}

func TestBibliotecaExigeSessao(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComCuradoriaDaBiblioteca(bibliotecaFalsa(t)))
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

// reAdicionar tira do HTML o mesmo href em que o admin clicaria. O nome do
// servidor tem barra no meio, e é justamente ela que precisa sobreviver ao
// caminho — por isso o casamento não para no primeiro segmento.
var reAdicionar = regexp.MustCompile(`href="(/admin/biblioteca/adicionar/[^"]+)"`)

func linkDeAdicionar(t *testing.T, pagina, nome string) string {
	t.Helper()

	for _, achado := range reAdicionar.FindAllStringSubmatch(pagina, -1) {
		if strings.HasSuffix(achado[1], nome) {
			return html.UnescapeString(achado[1])
		}
	}
	t.Fatalf("a tela não trouxe o botão de adicionar de %s", nome)
	return ""
}

// TestAdicionarDaBibliotecaAbreFormularioPreenchido é o teste de ponta a ponta
// do "só adicionar": ele não constrói o caminho, tira do HTML o href do botão e
// o segue até o formulário.
//
// São os dois lados do contrato, e por isso os dois estão aqui: o item local,
// que enche tipo, nome, comando e args; e o remoto, que em vez da execução
// manda a URL e o modo de credencial — com o radio de OAuth já marcado, que é a
// única parte do formulário que a biblioteca consegue adivinhar com segurança.
func TestAdicionarDaBibliotecaAbreFormularioPreenchido(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComCuradoriaDaBiblioteca(bibliotecaFalsa(t)))
	u.setup(t)

	pagina := esperarBiblioteca(t, u)
	// O cliente segue o 303, então o que chega é o formulário de upstream.
	res := u.abrir(t, linkDeAdicionar(t, pagina, "acme-mcp"))
	if !strings.HasPrefix(res.Request.URL.Path, webui.RotaUpstreams+"/novo") {
		t.Fatalf("adicionar caiu em %q, quer o formulário de upstream novo", res.Request.URL.Path)
	}
	form := corpo(t, res)
	casos := map[string]string{
		"tipo":    `name="tipo" value="stdio"`,
		"nome":    `value="Acme"`,
		"comando": `value="npx"`,
	}
	for campo, quer := range casos {
		if !strings.Contains(form, quer) {
			t.Errorf("o formulário não veio com %s preenchido (queria %s)", campo, quer)
		}
	}
	// Os argumentos vão na caixa de texto, um por linha.
	if !strings.Contains(form, "-y\nacme-mcp") {
		t.Error("o formulário não veio com os argumentos preenchidos, na ordem")
	}

	// O outro lado: o remoto não tem comando nenhum para mandar, e o que o
	// formulário precisa receber é a URL e o modo de credencial.
	resRemoto := u.abrir(t, linkDeAdicionar(t, pagina, "acme-remoto"))
	if !strings.HasPrefix(resRemoto.Request.URL.Path, webui.RotaUpstreams+"/novo") {
		t.Fatalf("adicionar caiu em %q, quer o formulário de upstream novo", resRemoto.Request.URL.Path)
	}
	formRemoto := corpo(t, resRemoto)
	if !strings.Contains(formRemoto, `value="https://exemplo.test/api/mcp"`) {
		t.Error("o formulário não veio com a url do remoto preenchida")
	}
	// O radio de OAuth marcado é o que prova que o modo atravessou: o
	// formulário desenha os dois modos sempre, e só um deles vem checked.
	oauth := strings.Index(formRemoto, `value="oauth"`)
	if oauth < 0 {
		t.Fatal("o formulário não trouxe o modo oauth")
	}
	if !strings.Contains(campoDoRadio(formRemoto, oauth), "checked") {
		t.Error("o formulário veio com o modo oauth desmarcado, e a curadoria declarou OAuth")
	}
}

// campoDoRadio devolve o resto do <input> que começa em pos, para se afirmar
// sobre os atributos daquele radio e não sobre os do vizinho.
func campoDoRadio(form string, pos int) string {
	fim := strings.Index(form[pos:], ">")
	if fim < 0 {
		return form[pos:]
	}
	return form[pos : pos+fim]
}

// TestAdicionarDeServidorLocalPreencheAExecucao é o mesmo contrato sobre outra
// linha do catálogo: o que o formulário precisa receber é comando e
// argumentos, não URL.
func TestAdicionarDeServidorLocalPreencheAExecucao(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComCuradoriaDaBiblioteca(bibliotecaFalsa(t)))
	u.setup(t)

	pagina := esperarBiblioteca(t, u)
	res := u.abrir(t, linkDeAdicionar(t, pagina, "acme-local"))
	if !strings.HasPrefix(res.Request.URL.Path, webui.RotaUpstreams+"/novo") {
		t.Fatalf("adicionar caiu em %q, quer o formulário de upstream novo", res.Request.URL.Path)
	}
	form := corpo(t, res)
	if !strings.Contains(form, `value="npx"`) {
		t.Error("o formulário não veio com o comando preenchido")
	}
	// Os argumentos vão na caixa de texto, um por linha. O -y e o pacote têm de
	// estar lá, nessa ordem.
	if !strings.Contains(form, "-y\nacme-mcp@0.4.0") {
		t.Error("o formulário não veio com os argumentos preenchidos, na ordem")
	}
}

// TestBibliotecaComOrigemForaNaoDerrubaOPainel: a varredura fala com um serviço
// de terceiro, e ele pode estar fora. Isso não pode virar erro de gateway, nem
// atrapalhar as outras telas, nem deixar a biblioteca sem próximo passo.
//
// É a diferença que a cópia local trouxe: antes, origem fora era tela fora.
// Agora é só o catálogo não ter chegado ainda — e, numa instalação que já
// sincronizou uma vez, nem isso.
func TestBibliotecaComOrigemForaNaoDerrubaOPainel(t *testing.T) {
	t.Parallel()

	fora := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora do ar", http.StatusBadGateway)
	}))
	t.Cleanup(fora.Close)

	u := subirUI(t, ComCuradoriaDaBiblioteca(fora.URL))
	u.setup(t)

	res := u.abrir(t, webui.RotaBiblioteca)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	pagina := corpo(t, res)
	if !strings.Contains(pagina, "Cadastrar à mão") {
		t.Error("a tela não oferece a saída que continua funcionando")
	}
	if !strings.Contains(pagina, "Catálogo local:") {
		t.Error("a tela não diz em que estado o catálogo está")
	}
	if res := u.abrir(t, webui.RotaUpstreams); res.StatusCode != http.StatusOK {
		t.Errorf("a lista de upstreams caiu junto: status = %d", res.StatusCode)
	}
}

// TestPreenchimentoNaoAceitaCredencialNemModoInventado fecha a porta que o
// preenchimento abre: a query alimenta campos do formulário, e ela é escrita
// por quem monta o link.
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

// TestPreenchimentoDeArgsRecusaOQuePartiriaACaixa: um argumento é uma linha da
// caixa de texto, então argumento com quebra de linha viraria dois argumentos —
// escritos por quem montou o link, não pelo admin.
func TestPreenchimentoDeArgsRecusaOQuePartiriaACaixa(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	form := corpo(t, u.abrir(t, webui.RotaUpstreams+
		"/novo?tipo=stdio&nome=Local&comando=npx&arg=-y&arg=pacote%0Ainjetado"))

	if strings.Contains(form, "injetado") {
		t.Error("um argumento com quebra de linha entrou na caixa")
	}
	if !strings.Contains(form, "-y") {
		t.Error("o argumento legítimo não foi preenchido")
	}
}

// TestAtualizarBibliotecaPassaPelaProtecaoDeCSRF: o botão é um POST, e POST no
// painel atravessa a proteção nativa do net/http. Um teste no pacote não veria
// isso — lá o mux é montado sem o middleware do painel.
func TestAtualizarBibliotecaPassaPelaProtecaoDeCSRF(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComCuradoriaDaBiblioteca(bibliotecaFalsa(t)))
	u.setup(t)
	esperarBiblioteca(t, u)

	res := u.enviarForm(t, webui.RotaBiblioteca+"/atualizar", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, quer %d depois de seguir o redirecionamento",
			res.StatusCode, http.StatusOK)
	}
	if res.Request.URL.Path != webui.RotaBiblioteca {
		t.Fatalf("o POST caiu em %q, quer voltar à biblioteca", res.Request.URL.Path)
	}
	// A varredura foi disparada de novo; o que importa aqui é que o POST não foi
	// recusado no caminho.
	select {
	case <-u.bibliotecaSincronizou:
	case <-time.After(10 * time.Second):
		t.Fatal("a varredura pedida pelo botão não terminou")
	}
}
