package apikey

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP do CRUD de chave de API.
type Admin struct {
	repo       *RepositorioSQLite
	endpoints  Endpoints
	urlPublica string
	log        *slog.Logger
}

// NovoAdmin monta o CRUD de chave.
func NovoAdmin(repo *RepositorioSQLite, endpoints Endpoints, urlPublica string, log *slog.Logger) *Admin {
	return &Admin{repo: repo, endpoints: endpoints, urlPublica: urlPublica, log: log}
}

// Rotas registra as telas de chave. Todas exigem sessão de admin.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaChaves, a.listar)
	mux.HandleFunc("GET "+webui.RotaChaves+"/nova", a.formNova)
	mux.HandleFunc("POST "+webui.RotaChaves, a.criar)
	mux.HandleFunc("POST "+webui.RotaChaves+"/{id}/revogar", a.revogar)
}

func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	chaves, err := a.repo.Todas(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaLista(chaves, webui.Avisos(r, avisos)))
}

func (a *Admin) formNova(w http.ResponseWriter, r *http.Request) {
	opcoes, err := a.opcoesMarcadas(r.Context(), nil)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(Form{}, opcoes))
}

// criar emite a chave e responde a tela que mostra o texto claro.
//
// Sem redirecionamento de propósito: um 303 para uma URL com a chave a colocaria
// no histórico do navegador e no log de qualquer proxy no caminho. A chave aparece
// no corpo desta resposta, uma vez, e nunca mais.
func (a *Admin) criar(w http.ResponseWriter, r *http.Request) {
	form, err := lerForm(r)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	if !form.Validar() {
		a.reexibir(w, r, form, http.StatusUnprocessableEntity)
		return
	}

	emitida, err := Gerar()
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	chave, err := a.repo.Emitir(r.Context(), form.Nome, emitida, form.EndpointIDs)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	a.log.Info("chave de api emitida",
		"chave", chave.Nome, "prefixo", chave.PrefixoVisivel, "endpoints", chave.Endpoints)
	webui.Renderizar(w, r, http.StatusCreated, a.log, TelaCriada(Criada{
		Chave:    chave,
		Claro:    emitida.Claro,
		Comandos: a.comandos(chave, emitida.Claro),
	}))
}

func (a *Admin) revogar(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.repo.Revogar(r.Context(), id); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	// Nada a rematerializar: a verificação da chave lê o banco a cada requisição,
	// então a revogação já vale para a próxima chamada de qualquer cliente.
	a.log.Info("chave de api revogada", "api_key_id", id)
	webui.Redirecionar(w, r, webui.RotaChaves+"?aviso=revogada")
}

// comandos monta um "claude mcp add" por endpoint do escopo.
func (a *Admin) comandos(c Chave, claro string) []Comando {
	out := make([]Comando, 0, len(c.Endpoints))
	for _, slug := range c.Endpoints {
		out = append(out, Comando{
			Slug: slug,
			Linha: "claude mcp add --transport http patchbay-" + slug + " " +
				a.urlPublica + "/mcp/" + slug +
				` --header "Authorization: Bearer ` + claro + `"`,
		})
	}
	return out
}

func (a *Admin) reexibir(w http.ResponseWriter, r *http.Request, form Form, status int) {
	opcoes, err := a.opcoesMarcadas(r.Context(), form.EndpointIDs)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, status, a.log, TelaForm(form, opcoes))
}

func (a *Admin) opcoesMarcadas(ctx context.Context, escolhidos []int64) ([]EndpointOpcao, error) {
	opcoes, err := a.endpoints.Opcoes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range opcoes {
		opcoes[i].Escolhido = slices.Contains(escolhidos, opcoes[i].ID)
	}
	return opcoes, nil
}

func lerForm(r *http.Request) (Form, error) {
	if err := r.ParseForm(); err != nil {
		return Form{}, err
	}
	form := Form{Nome: r.PostFormValue("nome")}
	for _, bruto := range r.PostForm["endpoint"] {
		id, err := strconv.ParseInt(bruto, 10, 64)
		if err != nil {
			continue
		}
		form.EndpointIDs = append(form.EndpointIDs, id)
	}
	return form, nil
}

var avisos = map[string]webui.Alerta{
	"revogada": {
		Tom:    webui.TomInfo,
		Titulo: "Chave revogada.",
		Texto:  "A próxima requisição de quem a usava recebe 401. A linha continua na lista para você saber qual prefixo era.",
	},
}
