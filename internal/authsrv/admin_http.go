package authsrv

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP do CRUD de cliente OAuth e da revogação de sessão.
type Admin struct {
	repo       *RepositorioSQLite
	endpoints  Endpoints
	urlPublica string
	log        *slog.Logger
	agora      func() time.Time
}

// NovoAdmin monta o CRUD de cliente OAuth.
func NovoAdmin(repo *RepositorioSQLite, endpoints Endpoints, urlPublica string, log *slog.Logger) *Admin {
	return &Admin{
		repo:       repo,
		endpoints:  endpoints,
		urlPublica: strings.TrimRight(urlPublica, "/"),
		log:        log,
		agora:      time.Now,
	}
}

// Rotas registra as telas de cliente OAuth. Todas exigem sessão de admin.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaClientesOAuth, a.listar)
	mux.HandleFunc("GET "+webui.RotaClientesOAuth+"/novo", a.formNovo)
	mux.HandleFunc("POST "+webui.RotaClientesOAuth, a.criar)
	mux.HandleFunc("GET "+webui.RotaClientesOAuth+"/{id}", a.detalhe)
	mux.HandleFunc("POST "+webui.RotaClientesOAuth+"/{id}/revogar", a.revogarCliente)
	mux.HandleFunc("POST "+webui.RotaClientesOAuth+"/{id}/sessoes/{familia}/revogar", a.revogarSessao)
}

func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	clientes, err := a.repo.TodosClientes(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaClientes(clientes, webui.Avisos(r, avisosOAuth)))
}

func (a *Admin) formNovo(w http.ResponseWriter, r *http.Request) {
	opcoes, err := a.opcoesMarcadas(r.Context(), nil)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	// A URI fixa do claude.ai vem sugerida: ela é a de todas as superfícies
	// hospedadas da Anthropic e é a que o admin vai colar em nove de dez
	// cadastros. Sugerida, não imposta — o campo continua editável.
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaFormCliente(FormCliente{RedirectTexto: RedirectClaudeAI}, opcoes))
}

func (a *Admin) criar(w http.ResponseWriter, r *http.Request) {
	form, err := lerFormCliente(r)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	if !form.Validar() {
		a.reexibir(w, r, form, http.StatusUnprocessableEntity)
		return
	}

	clientID, err := gerarClientID()
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	var segredo, prefixo, hash string
	if form.Confidencial {
		if segredo, prefixo, hash, err = gerarSegredo(); err != nil {
			webui.ErroInterno(w, r, a.log, err)
			return
		}
	}

	cliente, err := a.repo.CriarCliente(r.Context(), form, clientID, prefixo, hash, a.agora())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	// O segredo não entra no log, nem em Debug: a tela de log é a via mais fácil
	// de vazar exatamente o que o hash em repouso protege (seção 11).
	a.log.Info("cliente oauth cadastrado",
		"client_id", cliente.ClientID, "nome", cliente.Nome,
		"confidencial", cliente.Confidencial, "endpoints", len(cliente.Endpoints))

	// Sem redirecionamento: um 303 para uma URL com o segredo o colocaria no
	// histórico do navegador e no log de qualquer proxy no caminho.
	webui.Renderizar(w, r, http.StatusCreated, a.log, TelaClienteCriado(ClienteCriado{Cliente: cliente, Segredo: segredo}, a.urlPublica))
}

func (a *Admin) detalhe(w http.ResponseWriter, r *http.Request) {
	id, ok := idDaRota(w, r)
	if !ok {
		return
	}
	cliente, err := a.repo.ClientePorID(r.Context(), id)
	switch {
	case errors.Is(err, ErrClienteNaoEncontrado):
		http.NotFound(w, r)
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	sessoes, err := a.repo.SessoesDoCliente(r.Context(), id)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaCliente(cliente, sessoes, a.urlPublica, webui.Avisos(r, avisosOAuth)))
}

func (a *Admin) revogarCliente(w http.ResponseWriter, r *http.Request) {
	id, ok := idDaRota(w, r)
	if !ok {
		return
	}
	if err := a.repo.RevogarCliente(r.Context(), id, a.agora()); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	a.log.Info("cliente oauth revogado", "oauth_client_id", id)
	webui.Redirecionar(w, r, webui.RotaClientesOAuth+"?aviso=cliente_revogado")
}

func (a *Admin) revogarSessao(w http.ResponseWriter, r *http.Request) {
	id, ok := idDaRota(w, r)
	if !ok {
		return
	}
	familia := r.PathValue("familia")
	pertence, err := a.repo.FamiliaDoCliente(r.Context(), id, familia)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	if !pertence {
		http.NotFound(w, r)
		return
	}
	if err := a.repo.RevogarFamilia(r.Context(), familia, a.agora()); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	a.log.Info("sessão oauth revogada pelo admin", "oauth_client_id", id, "familia_id", familia)
	webui.Redirecionar(w, r,
		webui.RotaClientesOAuth+"/"+strconv.FormatInt(id, 10)+"?aviso=sessao_revogada")
}

func (a *Admin) reexibir(w http.ResponseWriter, r *http.Request, form FormCliente, status int) {
	opcoes, err := a.opcoesMarcadas(r.Context(), form.EndpointIDs)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, status, a.log, TelaFormCliente(form, opcoes))
}

func (a *Admin) opcoesMarcadas(ctx context.Context, escolhidos []int64) ([]EndpointOpcao, error) {
	refs, err := a.endpoints.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]EndpointOpcao, 0, len(refs))
	for _, ref := range refs {
		out = append(out, EndpointOpcao{
			EndpointRef: ref,
			Escolhido:   slices.Contains(escolhidos, ref.ID),
		})
	}
	return out, nil
}

func idDaRota(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func lerFormCliente(r *http.Request) (FormCliente, error) {
	if err := r.ParseForm(); err != nil {
		return FormCliente{}, err
	}
	f := FormCliente{
		Nome:          r.PostFormValue("nome"),
		RedirectTexto: r.PostFormValue("redirect_uris"),
		Confidencial:  r.PostFormValue("tipo") == "confidencial",
	}
	for _, bruto := range r.PostForm["endpoint"] {
		id, err := strconv.ParseInt(bruto, 10, 64)
		if err != nil {
			continue
		}
		f.EndpointIDs = append(f.EndpointIDs, id)
	}
	return f, nil
}

var avisosOAuth = map[string]webui.Alerta{
	"cliente_revogado": {
		Tom:    webui.TomInfo,
		Titulo: "Cliente revogado.",
		Texto: "As sessões dele foram derrubadas junto: a próxima chamada recebe 401. " +
			"A linha continua na lista para você saber qual client_id existiu.",
	},
	"sessao_revogada": {
		Tom:    webui.TomInfo,
		Titulo: "Sessão revogada.",
		Texto: "O access e o refresh daquela família pararam de valer. " +
			"O cliente precisa passar pelo consentimento de novo.",
	},
}
