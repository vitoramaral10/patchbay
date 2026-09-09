package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

const (
	usuarioAdmin = "vitor"
	senhaAdmin   = "senha-de-doze-ou-mais"
)

// uiDeTeste é um patchbay em processo servido por httptest, com um navegador de
// mentira (cookie jar + Sec-Fetch-Site) por cima.
//
// Sem seed: tudo o que existe neste banco foi criado pela tela, que é a
// demonstração pedida para a fatia 2.
type uiDeTeste struct {
	url         string
	cliente     *http.Client
	app         *Aplicacao
	sincronizou <-chan struct{}
	// bibliotecaSincronizou avisa que uma varredura do catálogo de servidores
	// MCP terminou. Registrado antes de Iniciar, porque é lá que a primeira
	// varredura começa.
	bibliotecaSincronizou <-chan struct{}
}

func subirUI(t *testing.T, opcoes ...OpcaoApp) uiDeTeste {
	t.Helper()

	// Servidor sem handler primeiro, só para saber a porta: a URL pública precisa
	// ser a de verdade, porque é dela que sai a origem confiável da proteção de
	// CSRF e a URL que a tela mostra para o cliente MCP registrar.
	ts := httptest.NewUnstartedServer(nil)
	publica := "http://" + ts.Listener.Addr().String()

	cfg := Config{
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
		PublicURL: publica,
		NivelLog:  slog.LevelError,
	}
	ctx, cancelar := context.WithCancel(context.Background())
	t.Cleanup(cancelar)

	// A biblioteca varre o registry no boot quando o catálogo local está vazio —
	// e num t.TempDir() ele está sempre. Sem um padrão aqui, cada teste de UI
	// dispararia trezentas requisições ao registry de verdade só por subir o
	// patchbay. O padrão vem antes das opções do chamador, então quem precisa de
	// um catálogo específico continua trocando a origem.
	opcoes = append([]OpcaoApp{
		ComOrigemDaBiblioteca(registryMudo(t)),
		ComCuradoriaDaBiblioteca(curadoriaMudaDeTeste(t)),
	}, opcoes...)

	app, err := montar(ctx, cfg, cofreDeTeste(t), slog.New(slog.DiscardHandler), opcoes...)
	if err != nil {
		t.Fatalf("montar: erro = %v, quer nil", err)
	}

	// Espera por sinal, não pelo relógio.
	sincronizou := make(chan struct{}, 32)
	app.Observar(func() {
		select {
		case sincronizou <- struct{}{}:
		default:
		}
	})
	varreu := make(chan struct{}, 8)
	app.sincBib.Observar(func() {
		select {
		case varreu <- struct{}{}:
		default:
		}
	})

	app.Iniciar(ctx)

	ts.Config.Handler = app.Handler()
	ts.Config.ReadHeaderTimeout = 10 * time.Second
	ts.Start()
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		cancelar()
		if err := app.Fechar(); err != nil {
			t.Errorf("fechar aplicação: erro = %v, quer nil", err)
		}
	})

	jarro, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: erro = %v, quer nil", err)
	}
	return uiDeTeste{
		url:                   ts.URL,
		cliente:               &http.Client{Jar: jarro},
		app:                   app,
		sincronizou:           sincronizou,
		bibliotecaSincronizou: varreu,
	}
}

// registryMudo é uma origem que existe e não publica nada.
//
// Devolve página vazia: o sincronizador a recusa como catálogo (varredura vazia
// nunca apaga nada) e registra a falha, que é exatamente o estado de "o catálogo
// ainda não chegou" — o mesmo que o teste veria com a internet fora, e sem sair
// da máquina.
func registryMudo(t *testing.T) string {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"servers":[],"metadata":{}}`))
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// curadoriaMudaDeTeste é a segunda origem da biblioteca, local e mínima.
//
// Mínima e não vazia: a varredura recusa uma curadoria que não trouxe nada — é
// como ela distingue "o site mudou" de "não há servidor curado" —, então um
// servidor só é o que a mantém acima do piso sem interferir em nada.
func curadoriaMudaDeTeste(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><main>` +
			`<a href="/pt-BR/remote-mcp-servers/exemplo">` +
			`<div class="truncate">Exemplo</div></a></main></body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers/exemplo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Exemplo</h1><p>servidor de teste</p>` +
			`<h2>Detalhes da conexão</h2><code>https://exemplo.invalido/mcp</code>` +
			`<dl><dt>Transporte</dt><dd>Streamable HTTP</dd>` +
			`<dt>Autenticação</dt><dd>Aberto — sem autenticação</dd></dl></body></html>`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/pt-BR"
}

// --- navegador de mentira ---

// enviarForm faz o que um navegador faz ao submeter um formulário da própria
// página, inclusive o Sec-Fetch-Site que a proteção nativa do net/http lê.
func (u uiDeTeste) enviarForm(t *testing.T, caminho string, campos url.Values) *http.Response {
	t.Helper()
	return u.requisitar(t, http.MethodPost, caminho, campos, "same-origin")
}

func (u uiDeTeste) abrir(t *testing.T, caminho string) *http.Response {
	t.Helper()
	return u.requisitar(t, http.MethodGet, caminho, nil, "same-origin")
}

func (u uiDeTeste) requisitar(t *testing.T, metodo, caminho string, campos url.Values, fetchSite string) *http.Response {
	t.Helper()

	var corpo io.Reader
	if campos != nil {
		corpo = strings.NewReader(campos.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), metodo, u.url+caminho, corpo)
	if err != nil {
		t.Fatalf("montar %s %s: erro = %v, quer nil", metodo, caminho, err)
	}
	if campos != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	req.Header.Set("Origin", u.url)

	res, err := u.cliente.Do(req)
	if err != nil {
		t.Fatalf("%s %s: erro = %v, quer nil", metodo, caminho, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func corpo(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ler corpo: erro = %v, quer nil", err)
	}
	return string(b)
}

// --- passos do fluxo, cada um pela tela ---

func (u uiDeTeste) setup(t *testing.T) {
	t.Helper()

	res := u.enviarForm(t, webui.RotaSetup, url.Values{
		"usuario":   {usuarioAdmin},
		"senha":     {senhaAdmin},
		"confirmar": {senhaAdmin},
	})
	// O cliente segue o 303, então o que chega é o painel.
	if res.StatusCode != http.StatusOK {
		t.Fatalf("setup: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
	}
}

var idNaURL = regexp.MustCompile(`/(\d+)(\?|$)`)

// idDoDestino tira o id do recurso da URL final, que é onde o 303 do POST caiu.
func idDoDestino(t *testing.T, res *http.Response) int64 {
	t.Helper()

	achado := idNaURL.FindStringSubmatch(res.Request.URL.Path + "?")
	if achado == nil {
		t.Fatalf("URL final = %q, quer terminar com o id do recurso criado", res.Request.URL)
	}
	id, err := strconv.ParseInt(achado[1], 10, 64)
	if err != nil {
		t.Fatalf("id %q: erro = %v, quer nil", achado[1], err)
	}
	return id
}

func (u uiDeTeste) criarUpstream(t *testing.T, nome, alvo string) int64 {
	t.Helper()

	res := u.enviarForm(t, webui.RotaUpstreams, url.Values{
		"nome":       {nome},
		"url":        {alvo},
		"timeout_ms": {"5000"},
		"habilitado": {"1"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar upstream: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
	}
	return idDoDestino(t, res)
}

func (u uiDeTeste) criarEndpoint(t *testing.T, slug, nome string, upstreams ...int64) int64 {
	t.Helper()

	campos := url.Values{"slug": {slug}, "nome": {nome}}
	for _, id := range upstreams {
		campos.Add("upstream", strconv.FormatInt(id, 10))
	}
	res := u.enviarForm(t, webui.RotaEndpoints, campos)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar endpoint: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
	}
	return idDoDestino(t, res)
}

var chaveNaTela = regexp.MustCompile(`pbk_[a-z0-9]+_[A-Za-z0-9_-]{20,}`)

func (u uiDeTeste) criarChave(t *testing.T, nome string, endpoints ...int64) string {
	t.Helper()

	campos := url.Values{"nome": {nome}}
	for _, id := range endpoints {
		campos.Add("endpoint", strconv.FormatInt(id, 10))
	}
	res := u.enviarForm(t, webui.RotaChaves, campos)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("criar chave: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusCreated, corpo(t, res))
	}
	texto := corpo(t, res)
	claro := chaveNaTela.FindString(texto)
	if claro == "" {
		t.Fatalf("chave em claro não apareceu na resposta de criação; corpo: %q", texto)
	}
	return claro
}

// --- cliente MCP de verdade ---

func conectarMCP(t *testing.T, u uiDeTeste, slug, chave string) *mcp.ClientSession {
	t.Helper()

	cliente := mcp.NewClient(&mcp.Implementation{Name: "cliente-de-teste", Version: "0.0.1"}, nil)
	transporte := &mcp.StreamableClientTransport{
		Endpoint:   u.url + "/mcp/" + slug,
		HTTPClient: clienteComBearer(chave),
		MaxRetries: -1,
	}
	ctx, cancelar := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancelar)

	sessao, err := cliente.Connect(ctx, transporte, nil)
	if err != nil {
		t.Fatalf("conectar em /mcp/%s: erro = %v, quer nil", slug, err)
	}
	t.Cleanup(func() { _ = sessao.Close() })
	return sessao
}

func nomesDeFerramenta(t *testing.T, sessao *mcp.ClientSession) []string {
	t.Helper()

	ctx, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelar()

	res, err := sessao.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: erro = %v, quer nil", err)
	}
	nomes := make([]string, 0, len(res.Tools))
	for _, f := range res.Tools {
		nomes = append(nomes, f.Name)
	}
	slices.Sort(nomes)
	return nomes
}

// esperarFerramentas espera o catálogo do endpoint chegar ao tamanho pedido,
// esperando pelo sinal de sincronização e não pelo relógio.
func esperarFerramentas(t *testing.T, u uiDeTeste, slug string, quantas int) {
	t.Helper()

	limite := time.After(20 * time.Second)
	for {
		if u.app.endpoints.Contagem(slug) == quantas {
			return
		}
		select {
		case <-u.sincronizou:
		case <-limite:
			t.Fatalf("endpoint %s ficou com %d ferramentas, quer %d",
				slug, u.app.endpoints.Contagem(slug), quantas)
		}
	}
}

// TestUI_FluxoCompletoSemReiniciar é a demonstração da fatia 2: setup, login,
// upstream, endpoint, chave e um cliente MCP de verdade chamando a ferramenta —
// tudo pela tela, no mesmo processo, sem nenhum reinício e sem seed.
func TestUI_FluxoCompletoSemReiniciar(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamFalso(t)
	upstreamID := u.criarUpstream(t, "falso", alvo)
	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	chave := u.criarChave(t, "notebook", endpointID)

	esperarFerramentas(t, u, "pessoal", 2)

	sessao := conectarMCP(t, u, "pessoal", chave)
	quer := []string{nomeNormalizado, "somar"}
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, quer) {
		t.Fatalf("ferramentas = %v, quer %v", nomes, quer)
	}

	res, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call: erro = %v, quer nil", err)
	}
	if res.IsError {
		t.Fatalf("tools/call devolveu erro de ferramenta: %v", textoDe(res))
	}
	if texto := textoDe(res); texto != "42" {
		t.Errorf("resultado = %q, quer %q", texto, "42")
	}

	// Desabilitar o upstream pela tela esvazia o tools/list, sem reiniciar nada e
	// sem derrubar a sessão do cliente.
	res2 := u.enviarForm(t, webui.RotaUpstreams+"/"+strconv.FormatInt(upstreamID, 10), url.Values{
		"nome":       {"falso"},
		"url":        {alvo},
		"timeout_ms": {"5000"},
		// habilitado ausente = checkbox desmarcado.
	})
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("desabilitar upstream: status = %d, quer %d", res2.StatusCode, http.StatusOK)
	}
	esperarFerramentas(t, u, "pessoal", 0)

	// O endpoint não expõe mais nenhuma ferramenta que funcione, mas as duas
	// continuam listadas como lápide pela janela de graça (fatia 3): o cliente
	// desta sessão ainda tem a lista antiga, e unknown tool o faria concluir que
	// o endpoint quebrou em vez de que a configuração mudou.
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, quer) {
		t.Errorf("ferramentas depois de desabilitar = %v, quer as lápides %v", nomes, quer)
	}
	if lapides := u.app.endpoints.Lapides("pessoal"); !slices.Equal(lapides, quer) {
		t.Errorf("lápides = %v, quer %v", lapides, quer)
	}
	morta, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call na lápide: erro = %v, quer erro de ferramenta", err)
	}
	if !morta.IsError {
		t.Error("tools/call na lápide = sucesso, quer erro de ferramenta explicando que ela saiu")
	}
	if texto := textoDe(morta); !strings.Contains(texto, "somar") {
		t.Errorf("texto da lápide = %q, quer o nome da ferramenta", texto)
	}
}

// TestUI_RemoverUpstreamEsvaziaOEndpoint é o mesmo efeito por outro caminho, e o
// que garante que remover não deixa ferramenta zumbi registrada.
func TestUI_RemoverUpstreamEsvaziaOEndpoint(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "falso", upstreamFalso(t))
	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	chave := u.criarChave(t, "notebook", endpointID)
	esperarFerramentas(t, u, "pessoal", 2)

	sessao := conectarMCP(t, u, "pessoal", chave)

	res := u.enviarForm(t, webui.RotaUpstreams+"/"+strconv.FormatInt(upstreamID, 10)+"/remover", url.Values{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remover upstream: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	esperarFerramentas(t, u, "pessoal", 0)

	// Nenhuma ferramenta viva sobrou. As lápides ficam pela janela de graça e
	// respondem com a explicação — o que a fatia 3 troca por "ferramenta zumbi"
	// é o unknown tool, não o registro.
	quer := []string{nomeNormalizado, "somar"}
	if lapides := u.app.endpoints.Lapides("pessoal"); !slices.Equal(lapides, quer) {
		t.Errorf("lápides = %v, quer %v", lapides, quer)
	}
	morta, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call na lápide: erro = %v, quer erro de ferramenta", err)
	}
	if !morta.IsError {
		t.Error("tools/call na lápide = sucesso, quer erro de ferramenta explicando que ela saiu")
	}

	// E a sessão do cliente continua de pé: remover upstream não derruba
	// endpoint.
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, quer) {
		t.Errorf("ferramentas depois de remover = %v, quer as lápides %v", nomes, quer)
	}
}

// TestUI_SessaoNaoAtravessaEndpoint fecha o furo de isolamento entre endpoints.
//
// O StreamableHTTPHandler do go-sdk v1.7.0 resolve a sessão só por Mcp-Session-Id
// mais TokenInfo.UserID (mcp/streamable.go:559-574) e nunca olha o path. Com um
// handler compartilhado, uma chave com escopo em dois endpoints abriria sessão em
// /mcp/a e reusaria o mesmo id em /mcp/b — o middleware validaria o escopo de b e
// quem atenderia seria o servidor de a. Um handler por endpoint fecha isso.
func TestUI_SessaoNaoAtravessaEndpoint(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	comFerramentas := u.criarUpstream(t, "com-ferramentas", upstreamFalso(t))
	idA := u.criarEndpoint(t, "alfa", "Alfa", comFerramentas)
	idB := u.criarEndpoint(t, "beta", "Beta")
	chave := u.criarChave(t, "as-duas", idA, idB)
	esperarFerramentas(t, u, "alfa", 2)

	sessaoA := conectarMCP(t, u, "alfa", chave)
	if sessaoA.ID() == "" {
		t.Fatal("sessão de /mcp/alfa sem Mcp-Session-Id, quer sessão retida")
	}

	// O endpoint beta não compõe upstream nenhum, então a sua resposta legítima é
	// lista vazia. Se o id da sessão de alfa fosse aceito aqui, viriam as duas
	// ferramentas de alfa.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		u.url+"/mcp/beta", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+chave)
	req.Header.Set("Mcp-Session-Id", sessaoA.ID())

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requisição: erro = %v, quer nil", err)
	}
	defer func() { _ = res.Body.Close() }()

	texto := corpo(t, res)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 com a sessão de alfa em /mcp/beta, quer recusa (corpo: %q)", texto)
	}
	if strings.Contains(texto, "somar") {
		t.Errorf("resposta de /mcp/beta traz ferramenta de alfa: %q", texto)
	}
}

// TestUI_CSRF prova que a proteção nativa do net/http cobre as rotas de
// formulário e que o htmx passa.
func TestUI_CSRF(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		fetchSite  string
		querStatus int
	}{
		// same-origin é o que o navegador manda ao submeter o formulário da
		// própria página — e é também o que o htmx manda, porque um hx-post é um
		// fetch da mesma página. Por isso a UI não precisa de token de formulário.
		"same-origin passa": {fetchSite: "same-origin", querStatus: http.StatusOK},
		"cross-site cai":    {fetchSite: "cross-site", querStatus: http.StatusForbidden},
		"same-site cai":     {fetchSite: "same-site", querStatus: http.StatusForbidden},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			u := subirUI(t)
			// Origin de outro site junto: sem isso a origem confiável configurada
			// (a URL pública) isentaria a requisição.
			res := u.requisitarDeOutraOrigem(t, webui.RotaSetup, url.Values{
				"usuario":   {usuarioAdmin},
				"senha":     {senhaAdmin},
				"confirmar": {senhaAdmin},
			}, tc.fetchSite)
			if res.StatusCode != tc.querStatus {
				t.Fatalf("status = %d, quer %d (corpo: %q)", res.StatusCode, tc.querStatus, corpo(t, res))
			}
		})
	}
}

// requisitarDeOutraOrigem manda o POST com Origin de outro site, que é o cenário
// de CSRF de verdade.
func (u uiDeTeste) requisitarDeOutraOrigem(t *testing.T, caminho string, campos url.Values, fetchSite string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		u.url+caminho, strings.NewReader(campos.Encode()))
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://site-do-atacante.invalido")
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	res, err := u.cliente.Do(req)
	if err != nil {
		t.Fatalf("requisição: erro = %v, quer nil", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// TestUI_RotaProtegidaSemSessao: com admin cadastrado e sem cookie, toda rota de
// UI cai no login carregando o destino.
func TestUI_RotaProtegidaSemSessao(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	// Sem admin ainda não é este caso: aqui já existe, e o cliente novo não tem
	// cookie.
	jarro, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: erro = %v, quer nil", err)
	}
	semSessao := uiDeTeste{url: u.url, app: u.app, cliente: &http.Client{
		Jar:           jarro,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}

	for _, caminho := range []string{webui.RotaPainel, webui.RotaUpstreams, webui.RotaEndpoints, webui.RotaChaves} {
		res := semSessao.abrir(t, caminho)
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: status = %d, quer %d", caminho, res.StatusCode, http.StatusSeeOther)
			continue
		}
		if destino := res.Header.Get("Location"); !strings.HasPrefix(destino, webui.RotaLogin) {
			t.Errorf("%s: Location = %q, quer o login", caminho, destino)
		}
	}
}

// TestUI_ChaveApareceUmaVezSo é a regra da seção 11: texto claro na criação,
// prefixo depois.
func TestUI_ChaveApareceUmaVezSo(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal")
	chave := u.criarChave(t, "notebook", endpointID)

	lista := corpo(t, u.abrir(t, webui.RotaChaves))
	if strings.Contains(lista, chave) {
		t.Error("a chave em claro reaparece na lista, quer só o prefixo")
	}
	// O prefixo são os dois primeiros segmentos: o terceiro é base64url e pode
	// conter '_'.
	partes := strings.SplitN(chave, "_", 3)
	prefixo := partes[0] + "_" + partes[1]
	if !strings.Contains(lista, prefixo) {
		t.Errorf("prefixo %q não aparece na lista, quer identificar a chave", prefixo)
	}
	if !strings.Contains(lista, "notebook") {
		t.Error("nome da chave não aparece na lista")
	}
}

// TestUI_ComandoDeRegistroVemPronto: a tela de criação entrega o comando do
// cliente MCP montado, com a URL pública e o endpoint certos.
func TestUI_ComandoDeRegistroVemPronto(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal")
	campos := url.Values{"nome": {"notebook"}, "endpoint": {strconv.FormatInt(endpointID, 10)}}
	texto := corpo(t, u.enviarForm(t, webui.RotaChaves, campos))

	quer := "claude mcp add --transport http patchbay-pessoal " + u.url + "/mcp/pessoal"
	if !strings.Contains(texto, quer) {
		t.Errorf("comando de registro ausente; quer conter %q", quer)
	}
}

// TestUI_RevogarChaveDerrubaOAcesso: a verificação lê o banco por requisição,
// então revogar vale para a chamada seguinte sem nenhuma rematerialização.
func TestUI_RevogarChaveDerrubaOAcesso(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "falso", upstreamFalso(t))
	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	chave := u.criarChave(t, "notebook", endpointID)
	esperarFerramentas(t, u, "pessoal", 2)

	// Antes: conecta.
	conectarMCP(t, u, "pessoal", chave)

	lista := corpo(t, u.abrir(t, webui.RotaChaves))
	if !strings.Contains(lista, "Revogar") {
		t.Fatal("a lista de chaves não oferece revogar")
	}
	chaveID := "1" // primeira chave emitida neste banco
	res := u.enviarForm(t, webui.RotaChaves+"/"+chaveID+"/revogar", url.Values{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revogar: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}

	// Depois: 401 na borda, com o desafio.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		u.url+"/mcp/pessoal", http.NoBody)
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+chave)

	depois, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requisição: erro = %v, quer nil", err)
	}
	defer func() { _ = depois.Body.Close() }()
	if depois.StatusCode != http.StatusUnauthorized {
		t.Errorf("status com chave revogada = %d, quer %d", depois.StatusCode, http.StatusUnauthorized)
	}
}

// TestUI_ValidacaoDosFormularios: entrada inválida volta com o erro no campo e
// nada é gravado.
func TestUI_ValidacaoDosFormularios(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		rota       string
		campos     url.Values
		querTrecho string
	}{
		"upstream sem nome": {
			rota:       webui.RotaUpstreams,
			campos:     url.Values{"url": {"https://exemplo.invalido/mcp"}, "timeout_ms": {"5000"}},
			querTrecho: "nome ao MCP",
		},
		"upstream com url inválida": {
			rota:       webui.RotaUpstreams,
			campos:     url.Values{"nome": {"x"}, "url": {"não é url"}, "timeout_ms": {"5000"}},
			querTrecho: "URL inválida",
		},
		"upstream com esquema não http": {
			rota:       webui.RotaUpstreams,
			campos:     url.Values{"nome": {"x"}, "url": {"ftp://exemplo.invalido/mcp"}, "timeout_ms": {"5000"}},
			querTrecho: "http e https",
		},
		"upstream com timeout fora da faixa": {
			rota:       webui.RotaUpstreams,
			campos:     url.Values{"nome": {"x"}, "url": {"https://exemplo.invalido/mcp"}, "timeout_ms": {"1"}},
			querTrecho: "250 e 120000",
		},
		"endpoint com slug inválido": {
			rota:       webui.RotaEndpoints,
			campos:     url.Values{"slug": {"Slug Com Espaço"}, "nome": {"x"}},
			querTrecho: "minúsculas, números e hífen",
		},
		"endpoint sem slug": {
			rota:       webui.RotaEndpoints,
			campos:     url.Values{"nome": {"x"}},
			querTrecho: "Escolha um slug",
		},
		"chave sem endpoint": {
			rota:       webui.RotaChaves,
			campos:     url.Values{"nome": {"x"}},
			querTrecho: "ao menos um endpoint",
		},
	}

	// Uma instância só para todos os casos: nenhum deles grava nada, então não há
	// estado a isolar — e cada patchbay a mais custa um argon2id de 64 MiB.
	u := subirUI(t)
	u.setup(t)
	u.criarEndpoint(t, "existente", "Existente")

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			res := u.enviarForm(t, tc.rota, tc.campos)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusUnprocessableEntity)
			}
			if texto := corpo(t, res); !strings.Contains(texto, tc.querTrecho) {
				t.Errorf("corpo não contém %q, quer o erro reexibido no campo", tc.querTrecho)
			}
		})
	}
}

// TestUI_SlugDuplicadoRecusado: o slug é único e é contrato.
func TestUI_SlugDuplicadoRecusado(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)
	u.criarEndpoint(t, "pessoal", "Pessoal")

	res := u.enviarForm(t, webui.RotaEndpoints, url.Values{"slug": {"pessoal"}, "nome": {"Outro"}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusConflict)
	}
	if texto := corpo(t, res); !strings.Contains(texto, "já existe um endpoint") &&
		!strings.Contains(texto, "Já existe um endpoint") {
		t.Errorf("corpo não explica o conflito de slug: %q", texto)
	}
}

// TestUI_NomeDeUpstreamDuplicadoRecusado é o mesmo para o upstream, que também
// tem nome único.
func TestUI_NomeDeUpstreamDuplicadoRecusado(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)
	u.criarUpstream(t, "falso", upstreamFalso(t))

	res := u.enviarForm(t, webui.RotaUpstreams, url.Values{
		"nome":       {"falso"},
		"url":        {"https://exemplo.invalido/mcp"},
		"timeout_ms": {"5000"},
	})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusConflict)
	}
}

// TestUI_EditarEndpointTrocaComposicao: compor um endpoint com um upstream depois
// de criado leva as ferramentas até ele, na hora.
func TestUI_EditarEndpointTrocaComposicao(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "falso", upstreamFalso(t))
	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal")
	esperarFerramentas(t, u, "pessoal", 0)

	res := u.enviarForm(t, webui.RotaEndpoints+"/"+strconv.FormatInt(endpointID, 10), url.Values{
		"nome":     {"Pessoal"},
		"upstream": {strconv.FormatInt(upstreamID, 10)},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editar endpoint: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
	}
	esperarFerramentas(t, u, "pessoal", 2)
}

// TestUI_EndpointRemovidoSaiDoAr: remover pela tela tira o /mcp/<slug> do ar.
func TestUI_EndpointRemovidoSaiDoAr(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal")
	if !u.app.endpoints.Existe("pessoal") {
		t.Fatal("endpoint criado pela tela não subiu")
	}

	res := u.enviarForm(t, webui.RotaEndpoints+"/"+strconv.FormatInt(endpointID, 10)+"/remover", url.Values{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("remover endpoint: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	if u.app.endpoints.Existe("pessoal") {
		t.Error("endpoint continua no ar depois de removido pela tela")
	}
}

// TestUI_EstaticosServidosDoBinario: htmx, a extensão de SSE e o CSS saem do
// embed.FS, não de CDN.
func TestUI_EstaticosServidosDoBinario(t *testing.T) {
	t.Parallel()

	u := subirUI(t)

	casos := map[string]string{
		"css":      webui.URLCSS(),
		"htmx":     webui.URLHTMX(),
		"htmx-sse": webui.URLHTMXSSE(),
	}
	for nome, caminho := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			res := u.abrir(t, caminho)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusOK)
			}
			if len(corpo(t, res)) == 0 {
				t.Error("corpo vazio")
			}
			if cache := res.Header.Get("Cache-Control"); !strings.Contains(cache, "immutable") {
				t.Errorf("Cache-Control = %q, quer immutable (a URL carrega a impressão do conteúdo)", cache)
			}
		})
	}
}

// TestUI_TelaDeUpstreamMostraFerramentasDescobertas cobre o requisito da tela de
// detalhe: estado, contagem e a lista com nome exposto e nome original.
func TestUI_TelaDeUpstreamMostraFerramentasDescobertas(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "falso", upstreamFalso(t))
	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	_ = endpointID
	esperarFerramentas(t, u, "pessoal", 2)

	texto := corpo(t, u.abrir(t, webui.RotaUpstreams+"/"+strconv.FormatInt(upstreamID, 10)))
	for _, trecho := range []string{
		"pronto",             // estado
		"somar",              // nome de ferramenta
		nomeComEspacos,       // nome original, com o espaço que o upstream usou
		nomeNormalizado,      // nome exposto depois do normalizador
		"soma dois inteiros", // descrição
		"pessoal",            // endpoint que inclui o upstream
	} {
		if !strings.Contains(texto, trecho) {
			t.Errorf("tela de detalhe não contém %q", trecho)
		}
	}
}

// TestUI_ReconectarUpstreamRearmaASupervisao cobre o botão que a seção 11 exige
// ao lado do estado: o admin tem como agir sobre o upstream que a tela mostra
// como degradado ou desabilitado por autoproteção, sem editar um campo que ele
// não quer mudar e sem reiniciar o processo.
func TestUI_ReconectarUpstreamRearmaASupervisao(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "falso", upstreamFalso(t))
	u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	esperarFerramentas(t, u, "pessoal", 2)

	rota := webui.RotaUpstreams + "/" + strconv.FormatInt(upstreamID, 10)
	res := u.enviarForm(t, rota+"/reconectar", url.Values{})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reconectar: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}

	// A sessão antiga foi descartada e uma nova subiu: o catálogo volta sozinho.
	esperarFerramentas(t, u, "pessoal", 2)

	// A tela de estado carrega o resíduo do watchdog e o agendamento do backoff:
	// sem esses dois números, o admin não distingue "está tentando" de "desistiu".
	tela := corpo(t, u.abrir(t, rota))
	for _, trecho := range []string{"Próxima tentativa", "Connects abandonados", "Falhas consecutivas"} {
		if !strings.Contains(tela, trecho) {
			t.Errorf("tela de detalhe não mostra %q", trecho)
		}
	}
}
