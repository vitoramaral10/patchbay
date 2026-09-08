package authsrv

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// HTTP é a borda do authorization server: os dois documentos de metadata e os
// três endpoints do protocolo.
type HTTP struct {
	s   *Servico
	log *slog.Logger

	limiteToken     *limitador
	limiteAutorizar *limitador
}

// NovoHTTP monta a borda HTTP do AS.
func NovoHTTP(s *Servico, log *slog.Logger) *HTTP {
	return &HTTP{
		s:               s,
		log:             log,
		limiteToken:     novoLimitador(LimiteTokenPorMinuto, time.Minute, s.agora),
		limiteAutorizar: novoLimitador(LimiteAutorizarPorMinuto, time.Minute, s.agora),
	}
}

// Rotas registra o que é público: os três documentos de metadata (RFC 8414,
// RFC 9728 por endpoint e o fallback dele na raiz), o token endpoint e o
// revocation endpoint.
//
// Nenhum deles passa por sessão de admin — são o protocolo, e quem autentica ali
// é o cliente OAuth. Registrados sem restrição de método porque respondem
// também a OPTIONS, que é o preflight de CORS.
func (h *HTTP) Rotas(mux *http.ServeMux) {
	mux.HandleFunc(RotaMetadataAS, h.metadataAS)
	mux.HandleFunc(RotaMetadataRecurso, h.metadataRecurso)
	mux.HandleFunc(RotaMetadataRecursoRaiz, h.metadataRecursoRaiz)
	mux.HandleFunc(RotaToken, h.token)
	mux.HandleFunc(RotaRevogar, h.revogar)
}

// RotasAutorizacao registra o authorize endpoint.
//
// Fica separado porque ele é o único que exige sessão de admin: o "usuário" do
// AS é o administrador do patchbay, e quem monta o grafo embrulha estas rotas no
// portão de sessão antes de pendurá-las no mux público.
func (h *HTTP) RotasAutorizacao(mux *http.ServeMux) {
	mux.HandleFunc("GET "+RotaAutorizar, h.autorizarForm)
	mux.HandleFunc("POST "+RotaAutorizar, h.autorizarDecidir)
}

// PadroesSemProtecaoDeOrigem são as rotas que não podem passar pela proteção de
// Origin do net/http.
//
// O token e o revocation endpoint são API de protocolo, chamados de outra
// origem por desenho: um cliente MCP que rode no navegador manda Origin, e a
// proteção — que existe para formulário de UI — recusaria a troca de código com
// 403. A defesa deles é a autenticação de cliente e o PKCE, não o Origin.
var PadroesSemProtecaoDeOrigem = []string{RotaToken, RotaRevogar}

// --- metadata ---

func (h *HTTP) metadataAS(w http.ResponseWriter, r *http.Request) {
	if pararNoPreflight(w, r) {
		return
	}
	escopos, err := h.s.Escopos(r.Context())
	if err != nil {
		h.log.Error("falha ao montar scopes_supported", "erro", err)
		http.Error(w, "metadata indisponível", http.StatusInternalServerError)
		return
	}
	escreverJSONPublico(w, h.log, h.s.Metadata(escopos))
}

func (h *HTTP) metadataRecurso(w http.ResponseWriter, r *http.Request) {
	if pararNoPreflight(w, r) {
		return
	}
	ref, err := h.s.Endpoint(r.Context(), r.PathValue("endpoint"))
	switch {
	case errors.Is(err, ErrEndpointNaoEncontrado):
		http.Error(w, "endpoint não encontrado", http.StatusNotFound)
		return
	case err != nil:
		h.log.Error("falha ao montar metadata de recurso", "erro", err)
		http.Error(w, "metadata indisponível", http.StatusInternalServerError)
		return
	}
	// O handler do go-sdk serve o JSON do RFC 9728 já com CORS *, que é o certo
	// para um documento público (auth/auth.go:188). Metade desta rota vem de
	// graça, e reusá-la é o que garante que os nomes de campo são os que o
	// cliente lê.
	auth.ProtectedResourceMetadataHandler(h.s.MetadataRecurso(ref)).ServeHTTP(w, r)
}

// metadataRecursoRaiz serve o fallback do RFC 9728 na raiz — ver a suposição
// no doc de Servico.MetadataRecursoRaiz.
func (h *HTTP) metadataRecursoRaiz(w http.ResponseWriter, r *http.Request) {
	if pararNoPreflight(w, r) {
		return
	}
	escopos, err := h.s.Escopos(r.Context())
	if err != nil {
		h.log.Error("falha ao montar scopes_supported do fallback de recurso", "erro", err)
		http.Error(w, "metadata indisponível", http.StatusInternalServerError)
		return
	}
	auth.ProtectedResourceMetadataHandler(h.s.MetadataRecursoRaiz(escopos)).ServeHTTP(w, r)
}

// --- authorize ---

func (h *HTTP) autorizarForm(w http.ResponseWriter, r *http.Request) {
	if !h.limiteAutorizar.permitir(chaveDoCliente(r, "")) {
		h.recusarPorLimite(w, r)
		return
	}
	p := pedidoDaQuery(r.URL.Query())
	autz, err := h.s.Validar(r.Context(), p)
	if err != nil {
		h.erroDeAutorizacao(w, r, p, err) //nolint:contextcheck // ver o doc de erroDeAutorizacao
		return
	}
	webui.Renderizar(w, r, http.StatusOK, h.log, TelaConsentimento(DadosConsentimento{
		Autorizacao: autz,
		Recurso:     h.s.Recurso(autz.Endpoint.Slug),
		Hospedeiro:  hospedeiroDe(p.RedirectURI),
	}))
}

func (h *HTTP) autorizarDecidir(w http.ResponseWriter, r *http.Request) {
	if !h.limiteAutorizar.permitir(chaveDoCliente(r, "")) {
		h.recusarPorLimite(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	// r.Form e não r.PostForm: revalidar o pedido inteiro na decisão é o que
	// impede que a tela de consentimento vire um lugar onde o escopo muda entre
	// o que a pessoa leu e o que o código concede.
	p := pedidoDaQuery(r.Form)
	autz, err := h.s.Validar(r.Context(), p)
	if err != nil {
		h.erroDeAutorizacao(w, r, p, err) //nolint:contextcheck // ver o doc de erroDeAutorizacao
		return
	}

	if r.Form.Get("decisao") != "aceitar" {
		h.log.Info("consentimento recusado pelo admin",
			"client_id", autz.Cliente.ClientID, "endpoint", autz.Endpoint.Slug)
		//nolint:contextcheck // ver o doc de erroDeAutorizacao
		h.redirecionarErro(w, r, p, erroOAuth(ErroAccessDenied, "o administrador recusou o acesso"))
		return
	}

	codigo, err := h.s.EmitirCodigo(r.Context(), autz)
	if err != nil {
		h.erroDeAutorizacao(w, r, p, err) //nolint:contextcheck // ver o doc de erroDeAutorizacao
		return
	}
	//nolint:contextcheck // ver o doc de erroDeAutorizacao
	h.redirecionar(w, r, p.RedirectURI, map[string]string{
		"code":  codigo,
		"state": p.State,
		// RFC 9207: o iss identifica quem emitiu, e o cliente valida contra o
		// issuer que descobriu. É a defesa contra mix-up attack.
		"iss": h.s.Emissor(),
	})
}

// erroDeAutorizacao decide entre a página de erro e o redirect.
//
// Os chamadores levam //nolint:contextcheck: o caminho de erro termina numa
// tela templ, e o contextcheck atravessa as closures geradas em *_templ.go até
// concluir que a chamada devia receber um ctx. É o mesmo falso positivo que o
// .golangci.yml já exclui quando webui.Renderizar aparece na própria linha;
// aqui ele está dois quadros abaixo, fora do alcance daquela exclusão. O
// contexto chega normalmente, pelo r.Context() que o Renderizar usa.
func (h *HTTP) erroDeAutorizacao(w http.ResponseWriter, r *http.Request, p PedidoAutorizacao, err error) {
	oerr := comoErroOAuth(err)
	if oerr.Status >= http.StatusInternalServerError {
		h.log.Error("falha no authorize endpoint", "erro", err)
	}
	if oerr.SemRedirect {
		// client_id desconhecido ou redirect_uri fora da allowlist: o erro fica
		// na tela do admin. Redirecionar aqui seria mandar a resposta — e, na
		// variante com code, a credencial — a um destino que o AS não reconhece.
		status := oerr.Status
		if status == 0 || status >= http.StatusInternalServerError {
			status = http.StatusBadRequest
		}
		webui.Renderizar(w, r, status, h.log, TelaErro(DadosErro{
			Codigo:    oerr.Codigo,
			Descricao: oerr.Descricao,
			ClientID:  p.ClientID,
			Redirect:  p.RedirectURI,
		}))
		return
	}
	h.redirecionarErro(w, r, p, oerr)
}

func (h *HTTP) redirecionarErro(w http.ResponseWriter, r *http.Request, p PedidoAutorizacao, oerr *ErroOAuth) {
	h.redirecionar(w, r, p.RedirectURI, map[string]string{
		"error":             oerr.Codigo,
		"error_description": oerr.Descricao,
		"state":             p.State,
		"iss":               h.s.Emissor(),
	})
}

// redirecionar devolve a resposta de autorização na query do redirect_uri.
//
// O destino já passou pela allowlist de comparação exata do cliente em Validar:
// nenhum caminho chega aqui com URI que o admin não cadastrou.
func (h *HTTP) redirecionar(w http.ResponseWriter, r *http.Request, destino string, params map[string]string) {
	u, err := url.Parse(destino)
	if err != nil {
		// Não deveria acontecer: a URI foi validada no cadastro e conferida na
		// allowlist. Se acontecer, a saída segura é não redirecionar.
		h.log.Error("redirect_uri da allowlist não parseia", "erro", err)
		webui.Renderizar(w, r, http.StatusBadRequest, h.log, TelaErro(DadosErro{
			Codigo:    ErroInvalidRequest,
			Descricao: "a redirect_uri cadastrada para este cliente é inválida",
		}))
		return
	}
	q := u.Query()
	for chave, valor := range params {
		if valor != "" {
			q.Set(chave, valor)
		}
	}
	u.RawQuery = q.Encode()

	// A resposta de autorização carrega um código de uso único: nenhum cache
	// intermediário pode guardá-la.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *HTTP) recusarPorLimite(w http.ResponseWriter, r *http.Request) {
	h.log.Warn("requisição ao authorization server recusada por limite de taxa",
		"caminho", r.URL.Path, "remoto", r.RemoteAddr)
	w.Header().Set("Retry-After", "60")
	http.Error(w, "requisições demais; tente de novo em um minuto", http.StatusTooManyRequests)
}

// --- token ---

func (h *HTTP) token(w http.ResponseWriter, r *http.Request) {
	if pararNoPreflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		respostaErro(w, h.log, &ErroOAuth{
			Codigo: ErroInvalidRequest, Descricao: "o token endpoint só aceita POST",
			Status: http.StatusMethodNotAllowed,
		})
		return
	}
	if err := conferirFormURLEncoded(r); err != nil {
		respostaErro(w, h.log, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		respostaErro(w, h.log, erroOAuth(ErroInvalidRequest, "corpo do formulário ilegível"))
		return
	}

	clientID, segredo := credenciaisDoCliente(r)
	if !h.limiteToken.permitir(chaveDoCliente(r, clientID)) {
		w.Header().Set("Retry-After", "60")
		respostaErro(w, h.log, &ErroOAuth{
			Codigo: ErroTemporarilyUnavailable, Descricao: "requisições demais; tente de novo em um minuto",
			Status: http.StatusTooManyRequests,
		})
		return
	}

	cliente, err := h.s.AutenticarCliente(r.Context(), clientID, segredo)
	if err != nil {
		respostaErro(w, h.log, comoErroOAuth(err))
		return
	}

	p := PedidoToken{
		GrantType:    r.PostFormValue("grant_type"),
		Codigo:       r.PostFormValue("code"),
		RedirectURI:  r.PostFormValue("redirect_uri"),
		CodeVerifier: r.PostFormValue("code_verifier"),
		RefreshToken: r.PostFormValue("refresh_token"),
		Resource:     r.PostFormValue("resource"),
		Escopo:       r.PostFormValue("scope"),
	}

	var concessao Concessao
	switch p.GrantType {
	case "authorization_code":
		concessao, err = h.s.Trocar(r.Context(), cliente, p)
	case "refresh_token":
		concessao, err = h.s.Renovar(r.Context(), cliente, p)
	case "":
		err = erroOAuth(ErroInvalidRequest, "grant_type ausente")
	default:
		err = &ErroOAuth{
			Codigo:    ErroUnsupportedGrantType,
			Descricao: "só authorization_code e refresh_token são suportados",
			Status:    http.StatusBadRequest,
		}
	}
	if err != nil {
		respostaErro(w, h.log, comoErroOAuth(err))
		return
	}

	h.responderToken(w, concessao)
}

// respostaToken é o corpo do RFC 6749 §5.1.
type respostaToken struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

func (h *HTTP) responderToken(w http.ResponseWriter, c Concessao) {
	segundos := int64(c.ExpiraEm.Sub(h.s.agora()).Seconds())
	if segundos < 0 {
		segundos = 0
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// RFC 6749 §5.1 exige os dois: a resposta carrega credencial em claro.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)

	// G117 aponta que a struct serializa um campo com cara de segredo. É
	// exatamente o que o RFC 6749 §5.1 manda: esta resposta *é* a entrega da
	// credencial ao cliente, sobre a conexão que ele autenticou, com no-store.
	//nolint:gosec // corpo do token endpoint; ver o comentário acima
	if err := json.NewEncoder(w).Encode(respostaToken{
		AccessToken:  c.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    segundos,
		RefreshToken: c.RefreshToken,
		Scope:        c.Escopo,
	}); err != nil {
		// O cabeçalho já foi enviado: só resta registrar. O cliente vai ver um
		// corpo truncado e tentar de novo, que é o comportamento certo.
		h.log.Error("não escreveu a resposta do token endpoint", "erro", err)
	}
}

// --- revogação ---

func (h *HTTP) revogar(w http.ResponseWriter, r *http.Request) {
	if pararNoPreflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		respostaErro(w, h.log, &ErroOAuth{
			Codigo: ErroInvalidRequest, Descricao: "o revocation endpoint só aceita POST",
			Status: http.StatusMethodNotAllowed,
		})
		return
	}
	if err := conferirFormURLEncoded(r); err != nil {
		respostaErro(w, h.log, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		respostaErro(w, h.log, erroOAuth(ErroInvalidRequest, "corpo do formulário ilegível"))
		return
	}

	clientID, segredo := credenciaisDoCliente(r)
	if !h.limiteToken.permitir(chaveDoCliente(r, clientID)) {
		w.Header().Set("Retry-After", "60")
		respostaErro(w, h.log, &ErroOAuth{
			Codigo: ErroTemporarilyUnavailable, Descricao: "requisições demais; tente de novo em um minuto",
			Status: http.StatusTooManyRequests,
		})
		return
	}
	cliente, err := h.s.AutenticarCliente(r.Context(), clientID, segredo)
	if err != nil {
		respostaErro(w, h.log, comoErroOAuth(err))
		return
	}

	if err := h.s.Revogar(r.Context(), cliente, r.PostFormValue("token")); err != nil {
		respostaErro(w, h.log, comoErroOAuth(err))
		return
	}
	// RFC 7009 §2.2: 200 com corpo vazio, inclusive para token desconhecido.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// --- apoio ---

// pedidoDaQuery lê os parâmetros de autorização de uma coleção de valores. A
// mesma função serve ao GET (query) e ao POST da decisão (formulário), que é o
// que garante que os dois validam exatamente o mesmo pedido.
func pedidoDaQuery(v url.Values) PedidoAutorizacao {
	return PedidoAutorizacao{
		ClientID:            v.Get("client_id"),
		RedirectURI:         v.Get("redirect_uri"),
		ResponseType:        v.Get("response_type"),
		CodeChallenge:       v.Get("code_challenge"),
		CodeChallengeMethod: v.Get("code_challenge_method"),
		Resource:            v.Get("resource"),
		Escopo:              v.Get("scope"),
		State:               v.Get("state"),
	}
}

// credenciaisDoCliente extrai client_id e client_secret dos dois métodos
// anunciados na metadata: client_secret_basic e client_secret_post.
//
// O Basic tem prioridade quando presente, como manda o RFC 6749 §2.3.1.
func credenciaisDoCliente(r *http.Request) (clientID, segredo string) {
	if id, senha, ok := r.BasicAuth(); ok {
		return id, senha
	}
	return r.PostFormValue("client_id"), r.PostFormValue("client_secret")
}

// conferirFormURLEncoded recusa qualquer corpo que não seja o do RFC 6749
// §4.1.3.
//
// A falha mais comum de implementação é o AS que só parseia JSON e devolve 415:
// o cliente recebe um status que não está no RFC 6749, não sabe o que fazer e
// desiste. Aqui a recusa é 400 com invalid_request e o motivo escrito.
func conferirFormURLEncoded(r *http.Request) *ErroOAuth {
	bruto := r.Header.Get("Content-Type")
	if bruto == "" {
		return erroOAuth(ErroInvalidRequest,
			"Content-Type ausente: use application/x-www-form-urlencoded")
	}
	tipo, _, err := mime.ParseMediaType(bruto)
	if err != nil {
		return erroOAuth(ErroInvalidRequest, "Content-Type malformado")
	}
	if tipo != "application/x-www-form-urlencoded" {
		return erroOAuth(ErroInvalidRequest,
			"Content-Type "+tipo+" não é aceito: use application/x-www-form-urlencoded")
	}
	return nil
}

// pararNoPreflight escreve os cabeçalhos de CORS e responde ao preflight.
//
// CORS liberado nestas rotas porque as três são públicas por desenho: os dois
// documentos de metadata são de descoberta, e o token endpoint precisa ser
// chamável por cliente que roda no navegador. Nenhuma delas usa cookie, então
// não há credencial de sessão a proteger com origem.
func pararNoPreflight(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func escreverJSONPublico(w http.ResponseWriter, log *slog.Logger, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error("não escreveu documento de metadata", "erro", err)
	}
}

// hospedeiroDe extrai o host de uma URI para a tela de consentimento.
//
// O hostname do redirect é a informação que decide o consentimento: é ele que
// diz para onde o código vai. Aparece em destaque, e não perdido no meio da URI.
func hospedeiroDe(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return uri
	}
	return strings.ToLower(u.Host)
}
