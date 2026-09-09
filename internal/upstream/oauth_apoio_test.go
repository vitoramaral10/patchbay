package upstream_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// Este arquivo monta um provedor OAuth de verdade em httptest — metadata,
// authorize, token, refresh e registro dinâmico — e um servidor MCP protegido
// por bearer.
//
// De verdade e não um dublê do handler do SDK de propósito: o que a fatia
// entrega é a interoperação com um authorization server, e um mock escrito à mão
// no lugar dele concordaria com o meu erro de entendimento do fluxo em vez de
// pegá-lo.

// --- authorization server falso ---------------------------------------------

type codigoEmitido struct {
	desafio  string
	clientID string
	recurso  string
	escopos  string
}

type concessaoFalsa struct {
	clientID string
	escopos  string
}

// asFalso é o authorization server do upstream.
type asFalso struct {
	*httptest.Server

	mu       sync.Mutex
	codigos  map[string]codigoEmitido
	acessos  map[string]concessaoFalsa
	refreshs map[string]concessaoFalsa
	// clientes registrados, por client_id. O valor é o client_secret, vazio para
	// cliente público.
	clientes map[string]string
	// recusaRefresh liga a resposta invalid_grant, que é como um provedor diz
	// que o consentimento acabou.
	recusaRefresh bool
	// segundos é a validade do access token emitido.
	segundos int

	idasAoToken     atomic.Int32
	idasAoRegistro  atomic.Int32
	idasAoAuthorize atomic.Int32
}

func novoASFalso(t *testing.T) *asFalso {
	t.Helper()

	as := &asFalso{
		codigos:  map[string]codigoEmitido{},
		acessos:  map[string]concessaoFalsa{},
		refreshs: map[string]concessaoFalsa{},
		clientes: map[string]string{},
		segundos: 3600,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", as.metadata)
	mux.HandleFunc("GET /authorize", as.autorizar)
	mux.HandleFunc("POST /token", as.token)
	mux.HandleFunc("POST /register", as.registrar)

	as.Server = httptest.NewServer(mux)
	t.Cleanup(as.Close)
	return as
}

// preRegistrar cadastra um cliente à mão, que é o caminho do Google: sem
// registration_endpoint e sem CIMD, o client_id vem colado no formulário.
func (as *asFalso) preRegistrar(clientID, segredo string) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.clientes[clientID] = segredo
}

func (as *asFalso) revogar() {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.recusaRefresh = true
	as.acessos = map[string]concessaoFalsa{}
}

func (as *asFalso) permitir() {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.recusaRefresh = false
}

func (as *asFalso) aceitaAcesso(token string) bool {
	as.mu.Lock()
	defer as.mu.Unlock()
	_, ok := as.acessos[token]
	return ok
}

func (as *asFalso) metadata(w http.ResponseWriter, _ *http.Request) {
	escreverJSON(w, map[string]any{
		"issuer":                                as.URL,
		"authorization_endpoint":                as.URL + "/authorize",
		"token_endpoint":                        as.URL + "/token",
		"registration_endpoint":                 as.URL + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "none"},
		"scopes_supported":                      []string{"leitura", "offline_access"},
		// RFC 9207: o iss volta na resposta de autorização e o cliente confere.
		// É a proteção contra mix-up quando há vários provedores em jogo.
		"authorization_response_iss_parameter_supported": true,
	})
}

func (as *asFalso) autorizar(w http.ResponseWriter, r *http.Request) {
	as.idasAoAuthorize.Add(1)
	q := r.URL.Query()

	destino, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || destino.Host == "" {
		http.Error(w, "redirect_uri inválido", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE obrigatório", http.StatusBadRequest)
		return
	}

	codigo := "cod-" + strconv.Itoa(int(as.idasAoAuthorize.Load()))
	as.mu.Lock()
	as.codigos[codigo] = codigoEmitido{
		desafio:  q.Get("code_challenge"),
		clientID: q.Get("client_id"),
		recurso:  q.Get("resource"),
		escopos:  q.Get("scope"),
	}
	as.mu.Unlock()

	volta := destino.Query()
	volta.Set("code", codigo)
	volta.Set("state", q.Get("state"))
	volta.Set("iss", as.URL)
	destino.RawQuery = volta.Encode()
	http.Redirect(w, r, destino.String(), http.StatusFound)
}

func (as *asFalso) token(w http.ResponseWriter, r *http.Request) {
	as.idasAoToken.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form inválido", http.StatusBadRequest)
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		as.trocarCodigo(w, r)
	case "refresh_token":
		as.renovar(w, r)
	default:
		erroOAuth(w, "unsupported_grant_type")
	}
}

func (as *asFalso) trocarCodigo(w http.ResponseWriter, r *http.Request) {
	as.mu.Lock()
	emitido, ok := as.codigos[r.PostFormValue("code")]
	delete(as.codigos, r.PostFormValue("code"))
	as.mu.Unlock()
	if !ok {
		// Uso único: o segundo uso do mesmo code é recusado.
		erroOAuth(w, "invalid_grant")
		return
	}
	if desafioDe(r.PostFormValue("code_verifier")) != emitido.desafio {
		erroOAuth(w, "invalid_grant")
		return
	}
	as.emitir(w, concessaoFalsa{clientID: emitido.clientID, escopos: emitido.escopos})
}

func (as *asFalso) renovar(w http.ResponseWriter, r *http.Request) {
	as.mu.Lock()
	recusa := as.recusaRefresh
	c, ok := as.refreshs[r.PostFormValue("refresh_token")]
	if ok {
		// Rotação: o refresh token usado sai de circulação, como fazem os
		// provedores que detectam replay.
		delete(as.refreshs, r.PostFormValue("refresh_token"))
	}
	as.mu.Unlock()

	if recusa || !ok {
		erroOAuth(w, "invalid_grant")
		return
	}
	as.emitir(w, c)
}

func (as *asFalso) emitir(w http.ResponseWriter, c concessaoFalsa) {
	as.mu.Lock()
	n := len(as.acessos) + len(as.refreshs) + 1
	acesso := "acesso-" + strconv.Itoa(n)
	refresh := "refresh-" + strconv.Itoa(n)
	as.acessos[acesso] = c
	as.refreshs[refresh] = c
	segundos := as.segundos
	as.mu.Unlock()

	escreverJSON(w, map[string]any{
		"access_token":  acesso,
		"token_type":    "Bearer",
		"expires_in":    segundos,
		"refresh_token": refresh,
		"scope":         c.escopos,
	})
}

func (as *asFalso) registrar(w http.ResponseWriter, r *http.Request) {
	n := as.idasAoRegistro.Add(1)
	var pedido map[string]any
	if err := json.NewDecoder(r.Body).Decode(&pedido); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}

	clientID := "dcr-" + strconv.Itoa(int(n))
	as.mu.Lock()
	as.clientes[clientID] = ""
	as.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	escreverJSON(w, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              pedido["redirect_uris"],
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// desafioDe monta o code_challenge S256 a partir do verifier, que é como o
// provedor confere o PKCE.
func desafioDe(verifier string) string {
	soma := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(soma[:])
}

func erroOAuth(w http.ResponseWriter, codigo string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": codigo})
}

func escreverJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- servidor MCP protegido por bearer ---------------------------------------

// recursoProtegido é um servidor MCP de verdade atrás de um bearer, com a
// metadata RFC 9728 que o cliente usa para achar o authorization server.
type recursoProtegido struct {
	*httptest.Server
	URLMCP     string
	tentativas atomic.Int32
	recusas    atomic.Int32
}

func novoRecursoProtegido(t *testing.T, as *asFalso, sse bool, ferramentas ...string) *recursoProtegido {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-oauth-falso", Version: "0.0.1"}, nil)
	for _, nome := range ferramentas {
		srv.AddTool(
			&mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}

	r := &recursoProtegido{}
	var protocolo http.Handler
	caminho := "/mcp"
	if sse {
		caminho = "/sse"
		protocolo = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	} else {
		protocolo = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	}

	mux := http.NewServeMux()
	mux.Handle(caminho, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.tentativas.Add(1)
		portador, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if portador == "" || !as.aceitaAcesso(portador) {
			r.recusas.Add(1)
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+r.URL+`/.well-known/oauth-protected-resource`+caminho+`"`)
			http.Error(w, "não autorizado", http.StatusUnauthorized)
			return
		}
		protocolo.ServeHTTP(w, req)
	}))
	// O POST de mensagem do transporte SSE vai para o caminho que o primeiro
	// evento anuncia, e ele fica sob o mesmo prefixo.
	if sse {
		mux.Handle("/", protocolo)
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource"+caminho,
		func(w http.ResponseWriter, _ *http.Request) {
			escreverJSON(w, map[string]any{
				"resource":              r.URLMCP,
				"authorization_servers": []string{as.URL},
				"scopes_supported":      []string{"leitura"},
			})
		})

	r.Server = httptest.NewServer(mux)
	r.URLMCP = r.URL + caminho
	t.Cleanup(func() {
		r.CloseClientConnections()
		r.Close()
	})
	return r
}

// --- ambiente completo -------------------------------------------------------

// handlerAtrasado deixa o servidor de administração existir antes do handler.
//
// Precisa ser assim porque a URL pública é o que monta o redirect_uri, e o
// redirect_uri é o que o broker precisa para montar o handler de OAuth: o
// endereço tem que ser conhecido antes das rotas.
type handlerAtrasado struct{ h atomic.Value }

func (h *handlerAtrasado) definir(next http.Handler) { h.h.Store(&next) }

func (h *handlerAtrasado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	next, _ := h.h.Load().(*http.Handler)
	if next == nil {
		http.Error(w, "ainda não montado", http.StatusServiceUnavailable)
		return
	}
	(*next).ServeHTTP(w, r)
}

// ambiente é o patchbay do teste: banco, broker, gerente e a borda HTTP de
// administração, sem sessão de admin na frente.
type ambiente struct {
	repo    *upstream.RepositorioSQLite
	st      *store.Store
	broker  *upstream.BrokerOAuth
	gerente *upstream.Gerente
	admin   *httptest.Server
	mudou   <-chan struct{}
}

type opcoesAmbiente struct {
	esperaURL          time.Duration
	tempoConsentimento time.Duration
	backoff            time.Duration
	// margemRenovacao e tiqueRenovacao encurtam a renovação proativa para o teste
	// não esperar pelos minutos de produção.
	margemRenovacao time.Duration
	tiqueRenovacao  time.Duration
}

func novoAmbiente(t *testing.T, cfgs func(publicURL string) []upstream.Form, op opcoesAmbiente) *ambiente {
	t.Helper()

	if op.esperaURL == 0 {
		op.esperaURL = 15 * time.Second
	}
	if op.tempoConsentimento == 0 {
		op.tempoConsentimento = 15 * time.Second
	}
	if op.backoff == 0 {
		op.backoff = 20 * time.Millisecond
	}

	repo, st := repositorioDeTeste(t)

	atrasado := &handlerAtrasado{}
	admin := httptest.NewServer(atrasado)
	t.Cleanup(func() {
		admin.CloseClientConnections()
		admin.Close()
	})

	broker := upstream.NovoBrokerOAuth(repo, admin.URL, slog.New(slog.DiscardHandler),
		upstream.ComTempoDeConsentimento(op.tempoConsentimento),
		upstream.ComEsperaDeURL(op.esperaURL),
	)

	var configs []upstream.Config
	for _, f := range cfgs(admin.URL) {
		id, err := repo.Criar(context.Background(), f)
		if err != nil {
			t.Fatalf("criar upstream %s: erro = %v, quer nil", f.Nome, err)
		}
		reg, err := repo.Obter(context.Background(), id)
		if err != nil {
			t.Fatalf("obter upstream %d: erro = %v, quer nil", id, err)
		}
		configs = append(configs, reg.Config())
	}

	mudou := make(chan struct{}, 64)
	g := upstream.NovoGerente(slog.New(slog.DiscardHandler), configs,
		upstream.ComIntervaloTentativa(op.backoff),
		upstream.ComRenovacaoDeToken(op.margemRenovacao, op.tiqueRenovacao),
		upstream.ComOAuth(broker),
		upstream.ComCredenciais(repo.Credenciais),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
	)

	a := upstream.NovoAdmin(repo, g, broker, nil,
		func(t *mcp.Tool) (string, []string) { return t.Name, nil },
		slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	a.Rotas(mux)
	a.RotasPublicas(mux)
	atrasado.definir(mux)

	ctx, cancelar := context.WithCancel(context.Background())
	g.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		g.Aguardar()
	})

	return &ambiente{repo: repo, st: st, broker: broker, gerente: g, admin: admin, mudou: mudou}
}

// clienteSemSeguir é o navegador do admin: ele para em cada redirecionamento
// para o teste poder afirmar para onde ele ia.
func clienteSemSeguir() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
