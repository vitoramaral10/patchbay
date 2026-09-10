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
// registry de mentira, esperam a primeira varredura povoar o catálogo local,
// clicam no botão que a biblioteca desenha e conferem que o formulário do outro
// lado volta preenchido. Se alguém renomear um parâmetro em qualquer um dos dois
// lados, falha aqui.

// A origem de mentira serve o mínimo do esquema do registry que o pacote lê. As
// amostras de resposta de verdade ficam em internal/biblioteca/testdata, onde a
// tradução é testada; aqui o que está sob teste é a costura entre as telas.
//
// São dois servidores porque o contrato tem dois lados: um remoto, que preenche
// url, e um local, que preenche comando e argumentos.
const catalogoFalso = `{"servers":[
	{"server":{
		"name":"com.acme/mcp","title":"Acme","version":"1.2.3",
		"description":"Pedidos, notas e clientes",
		"remotes":[{"type":"streamable-http","url":"https://mcp.acme.example/mcp"}]
	}},
	{"server":{
		"name":"com.acme/local","title":"Acme Local","version":"0.4.0",
		"description":"O mesmo, como processo local",
		"packages":[{
			"registryType":"npm","identifier":"acme-mcp","version":"0.4.0",
			"runtimeHint":"npx","transport":{"type":"stdio"},
			"runtimeArguments":[{"type":"positional","value":"-y"}]
		}]
	}}
],"metadata":{}}`

// curadoriaFalsa publica o mesmo endpoint que o registry de mentira, declarando
// OAuth.
//
// É a razão de existirem duas origens: o esquema do registry não tem campo de
// autenticação, e sem esta segunda fonte o admin cadastraria em credencial
// estática um servidor que só fala OAuth — e descobriria no primeiro 401.
func curadoriaFalsa(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><main>` +
			`<a href="/pt-BR/remote-mcp-servers/acme"><div class="truncate">Acme</div></a>` +
			`</main></body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers/acme", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Acme</h1><p>Pedidos, notas e clientes</p>` +
			`<h2>Detalhes da conexão</h2><code>https://mcp.acme.example/mcp</code>` +
			`<dl><dt>Transporte</dt><dd>Streamable HTTP</dd>` +
			`<dt>Autenticação</dt><dd>OAuth</dd></dl></body></html>`))
	})
	// O acervo /official é a terceira lista que a varredura percorre. Índice
	// ausente derruba a varredura inteira, então todo dublê precisa de um.
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><a href="/pt-BR/servers/exemplo">Exemplo</a></body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Exemplo Oficial</h1><p>processo local</p>` +
			`<pre>{&quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: ` +
			`[&quot;-y&quot;, &quot;exemplo-oficial-mcp&quot;]}</pre></body></html>`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/pt-BR"
}

func origemFalsa(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/servers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(catalogoFalso))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// esperarBiblioteca espera a primeira varredura de fundo povoar o catálogo e
// devolve a tela já com ele.
//
// A varredura é assíncrona por desenho — em produção ela leva minutos contra o
// registry de verdade. Espera por sinal, nunca pelo relógio: o observador é
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
		t.Fatal("a varredura terminou e a tela não mostra o que o registry publicou")
	}
	return pagina
}

func TestBibliotecaApareceNoPainel(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComOrigemDaBiblioteca(origemFalsa(t)))
	u.setup(t)

	res := u.abrir(t, webui.RotaBiblioteca)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, quer %d", webui.RotaBiblioteca, res.StatusCode, http.StatusOK)
	}
	pagina := esperarBiblioteca(t, u)
	if !strings.Contains(pagina, `href="`+webui.RotaBiblioteca+`"`) {
		t.Error("a navegação do painel não tem o item Biblioteca")
	}
	if !strings.Contains(pagina, "registry.modelcontextprotocol.io") {
		t.Error("a tela não diz de onde a lista veio")
	}
	if !strings.Contains(pagina, "Catálogo local:") {
		t.Error("a tela não diz de quando é a cópia")
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
func TestAdicionarDaBibliotecaAbreFormularioPreenchido(t *testing.T) {
	t.Parallel()

	u := subirUI(t,
		ComOrigemDaBiblioteca(origemFalsa(t)),
		ComCuradoriaDaBiblioteca(curadoriaFalsa(t)))
	u.setup(t)

	pagina := esperarBiblioteca(t, u)
	// O cliente segue o 303, então o que chega é o formulário de upstream.
	res := u.abrir(t, linkDeAdicionar(t, pagina, "com.acme/mcp"))
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
	// salvaria em credencial estática um servidor que só fala OAuth — e essa é
	// a informação que só a curadoria tem.
	if !strings.Contains(form, `value="oauth"`) || !strings.Contains(form, "checked") {
		t.Error("o formulário não veio com o modo OAuth marcado")
	}
}

// TestAdicionarDeServidorLocalPreencheAExecucao é o outro lado do contrato: o
// registry publica milhares de servidores que só existem como pacote, e para
// eles o que o formulário precisa receber é comando e argumentos, não URL.
func TestAdicionarDeServidorLocalPreencheAExecucao(t *testing.T) {
	t.Parallel()

	u := subirUI(t, ComOrigemDaBiblioteca(origemFalsa(t)))
	u.setup(t)

	pagina := esperarBiblioteca(t, u)
	res := u.abrir(t, linkDeAdicionar(t, pagina, "com.acme/local"))
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

	u := subirUI(t, ComOrigemDaBiblioteca(fora.URL))
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

	u := subirUI(t, ComOrigemDaBiblioteca(origemFalsa(t)))
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
