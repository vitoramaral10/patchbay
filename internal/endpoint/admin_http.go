package endpoint

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP do CRUD de endpoint.
type Admin struct {
	repo *RepositorioSQLite
	srv  *Servidores
	ups  Upstreams
	// urlPublica monta o endereço que o cliente MCP usa; é a mesma da
	// configuração, porque atrás de proxy o patchbay não descobre a URL externa
	// olhando a requisição.
	urlPublica string
	log        *slog.Logger
}

// NovoAdmin monta o CRUD de endpoint.
func NovoAdmin(repo *RepositorioSQLite, srv *Servidores, ups Upstreams, urlPublica string, log *slog.Logger) *Admin {
	return &Admin{repo: repo, srv: srv, ups: ups, urlPublica: urlPublica, log: log}
}

// Rotas registra as telas de endpoint. Todas exigem sessão de admin: quem
// registra embrulha este mux no portão.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaEndpoints, a.listar)
	mux.HandleFunc("GET "+webui.RotaEndpoints+"/novo", a.formNovo)
	mux.HandleFunc("POST "+webui.RotaEndpoints, a.criar)
	mux.HandleFunc("GET "+webui.RotaEndpoints+"/{id}", a.detalhe)
	mux.HandleFunc("GET "+webui.RotaEndpoints+"/{id}/editar", a.formEditar)
	mux.HandleFunc("POST "+webui.RotaEndpoints+"/{id}", a.atualizar)
	mux.HandleFunc("POST "+webui.RotaEndpoints+"/{id}/remover", a.remover)
}

func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	regs, err := a.repo.Todos(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	contagens, err := a.repo.ContagemDeUpstreams(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	linhas := make([]Linha, 0, len(regs))
	for _, reg := range regs {
		linhas = append(linhas, Linha{
			Registro:    reg,
			Upstreams:   contagens[reg.ID],
			Ferramentas: a.srv.Contagem(reg.Slug),
			URL:         a.urlDo(reg.Slug),
		})
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaLista(linhas, webui.Avisos(r, avisos)))
}

func (a *Admin) formNovo(w http.ResponseWriter, r *http.Request) {
	opcoes, err := a.ups.Opcoes(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(Form{}, opcoes))
}

func (a *Admin) criar(w http.ResponseWriter, r *http.Request) {
	form, err := a.lerForm(r, false)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	if !form.Validar() {
		a.reexibir(w, r, form, http.StatusUnprocessableEntity)
		return
	}

	id, err := a.repo.Criar(r.Context(), form)
	switch {
	case errors.Is(err, ErrSlugEmUso):
		form.Erros = map[string]string{"slug": "Já existe um endpoint com este slug."}
		a.reexibir(w, r, form, http.StatusConflict)
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	// Sincroniza antes de responder: o endpoint precisa estar no ar quando a
	// próxima tela disser que ele existe.
	if err := a.srv.Sincronizar(r.Context()); err != nil {
		a.log.Error("endpoint criado mas não materializado", "endpoint_id", id, "erro", err)
	}
	a.log.Info("endpoint criado", "endpoint", form.Slug, "endpoint_id", id)
	webui.Redirecionar(w, r, webui.RotaEndpoints+"/"+strconv.FormatInt(id, 10)+"?aviso=criado")
}

func (a *Admin) detalhe(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.endpointDaRota(w, r)
	if !ok {
		return
	}
	composicao, err := a.composicao(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	webui.Renderizar(w, r, http.StatusOK, a.log, TelaDetalhe(Detalhe{
		Registro:    reg,
		URL:         a.urlDo(reg.Slug),
		Ferramentas: a.srv.Expostos(reg.Slug),
		Composicao:  composicao,
	}, webui.Avisos(r, avisos)))
}

func (a *Admin) formEditar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.endpointDaRota(w, r)
	if !ok {
		return
	}
	escolhidos, err := a.repo.UpstreamsDo(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	opcoes, err := a.opcoesMarcadas(r.Context(), escolhidos)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(Form{
		ID:          reg.ID,
		Slug:        reg.Slug,
		Nome:        reg.Nome,
		Descricao:   reg.Descricao,
		Instrucoes:  reg.Instrucoes,
		UpstreamIDs: escolhidos,
		SlugFixo:    true,
	}, opcoes))
}

func (a *Admin) atualizar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.endpointDaRota(w, r)
	if !ok {
		return
	}
	form, err := a.lerForm(r, true)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	form.ID, form.Slug = reg.ID, reg.Slug
	if !form.Validar() {
		a.reexibir(w, r, form, http.StatusUnprocessableEntity)
		return
	}

	if err := a.repo.Atualizar(r.Context(), reg.ID, form); err != nil {
		if errors.Is(err, ErrNaoEncontrado) {
			http.NotFound(w, r)
			return
		}
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	if err := a.srv.Sincronizar(r.Context()); err != nil {
		a.log.Error("endpoint atualizado mas não materializado", "endpoint_id", reg.ID, "erro", err)
	}
	a.log.Info("endpoint atualizado", "endpoint", reg.Slug, "endpoint_id", reg.ID)
	webui.Redirecionar(w, r, webui.RotaEndpoints+"/"+strconv.FormatInt(reg.ID, 10)+"?aviso=salvo")
}

func (a *Admin) remover(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.endpointDaRota(w, r)
	if !ok {
		return
	}
	if err := a.repo.Remover(r.Context(), reg.ID); err != nil {
		if errors.Is(err, ErrNaoEncontrado) {
			http.NotFound(w, r)
			return
		}
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	// Sincronizar aqui é o que fecha as sessões vivas daquele endpoint e o tira
	// do ar. Sem isso o cliente continuaria conversando com um endpoint apagado.
	if err := a.srv.Sincronizar(r.Context()); err != nil {
		a.log.Error("endpoint removido mas sincronização falhou", "endpoint_id", reg.ID, "erro", err)
	}
	a.log.Info("endpoint removido", "endpoint", reg.Slug, "endpoint_id", reg.ID)
	webui.Redirecionar(w, r, webui.RotaEndpoints+"?aviso=removido")
}

// --- apoio ---

func (a *Admin) urlDo(slug string) string { return a.urlPublica + "/mcp/" + slug }

func (a *Admin) endpointDaRota(w http.ResponseWriter, r *http.Request) (Registro, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return Registro{}, false
	}
	reg, err := a.repo.Obter(r.Context(), id)
	switch {
	case errors.Is(err, ErrNaoEncontrado):
		http.NotFound(w, r)
		return Registro{}, false
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return Registro{}, false
	}
	return reg, true
}

func (a *Admin) lerForm(r *http.Request, slugFixo bool) (Form, error) {
	if err := r.ParseForm(); err != nil {
		return Form{}, err
	}
	form := Form{
		Slug:       r.PostFormValue("slug"),
		Nome:       strings.TrimSpace(r.PostFormValue("nome")),
		Descricao:  strings.TrimSpace(r.PostFormValue("descricao")),
		Instrucoes: strings.TrimSpace(r.PostFormValue("instrucoes")),
		SlugFixo:   slugFixo,
	}
	for _, bruto := range r.PostForm["upstream"] {
		id, err := strconv.ParseInt(bruto, 10, 64)
		if err != nil {
			// Checkbox com valor que não é id vem de formulário forjado; ignorar
			// é melhor que 400, porque o resto do formulário é aproveitável.
			continue
		}
		form.UpstreamIDs = append(form.UpstreamIDs, id)
	}
	return form, nil
}

func (a *Admin) reexibir(w http.ResponseWriter, r *http.Request, form Form, status int) {
	opcoes, err := a.opcoesMarcadas(r.Context(), form.UpstreamIDs)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, status, a.log, TelaForm(form, opcoes))
}

func (a *Admin) opcoesMarcadas(ctx context.Context, escolhidos []int64) ([]UpstreamOpcao, error) {
	opcoes, err := a.ups.Opcoes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range opcoes {
		opcoes[i].Escolhido = slices.Contains(escolhidos, opcoes[i].ID)
	}
	return opcoes, nil
}

func (a *Admin) composicao(ctx context.Context, id int64) ([]UpstreamOpcao, error) {
	escolhidos, err := a.repo.UpstreamsDo(ctx, id)
	if err != nil {
		return nil, err
	}
	opcoes, err := a.opcoesMarcadas(ctx, escolhidos)
	if err != nil {
		return nil, err
	}
	// A tela de detalhe mostra só o que compõe, não o catálogo inteiro.
	return slices.DeleteFunc(opcoes, func(o UpstreamOpcao) bool { return !o.Escolhido }), nil
}

// avisos traduz o ?aviso= da URL na caixa de resultado da ação anterior.
var avisos = map[string]webui.Alerta{
	"criado":   {Tom: webui.TomSucesso, Titulo: "Endpoint criado.", Texto: "Emita uma chave de API com escopo nele para um cliente conectar."},
	"salvo":    {Tom: webui.TomSucesso, Titulo: "Endpoint salvo.", Texto: "A mudança já está no ar; nenhum reinício é necessário."},
	"removido": {Tom: webui.TomInfo, Titulo: "Endpoint removido.", Texto: "As sessões MCP que estavam abertas nele foram encerradas."},
}
