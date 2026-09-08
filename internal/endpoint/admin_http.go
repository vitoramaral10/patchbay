package endpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
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
			Lapides:     len(a.srv.Lapides(reg.Slug)),
			URL:         a.urlDo(reg.Slug),
		})
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaLista(linhas, webui.Avisos(r, avisos)))
}

func (a *Admin) formNovo(w http.ResponseWriter, r *http.Request) {
	opcoes, err := a.opcoesDoForm(r.Context(), Form{})
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
	composicao, err := a.composicao(r.Context(), reg)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	webui.Renderizar(w, r, http.StatusOK, a.log, TelaDetalhe(Detalhe{
		Registro:    reg,
		URL:         a.urlDo(reg.Slug),
		Ferramentas: a.ferramentasExpostas(reg.Slug),
		Lapides:     a.srv.Lapides(reg.Slug),
		Composicao:  composicao,
	}, webui.Avisos(r, avisos)))
}

func (a *Admin) formEditar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.endpointDaRota(w, r)
	if !ok {
		return
	}
	form, err := a.formDe(r.Context(), reg)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	opcoes, err := a.opcoesDoForm(r.Context(), form)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(form, opcoes))
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
		Slug:        r.PostFormValue("slug"),
		Nome:        strings.TrimSpace(r.PostFormValue("nome")),
		Descricao:   strings.TrimSpace(r.PostFormValue("descricao")),
		Instrucoes:  strings.TrimSpace(r.PostFormValue("instrucoes")),
		Prefixos:    map[int64]string{},
		RegrasTexto: map[int64]string{},
		SlugFixo:    slugFixo,
	}
	for _, bruto := range r.PostForm["upstream"] {
		id, err := strconv.ParseInt(bruto, 10, 64)
		if err != nil {
			// Checkbox com valor que não é id vem de formulário forjado; ignorar
			// é melhor que 400, porque o resto do formulário é aproveitável.
			continue
		}
		form.UpstreamIDs = append(form.UpstreamIDs, id)
		// Prefixo e regras chegam num campo por upstream, e só os do upstream
		// marcado entram: campo de upstream desmarcado é sobra de formulário, e
		// gravá-lo faria uma composição que a tela não mostra.
		form.Prefixos[id] = strings.TrimSpace(r.PostFormValue(ChavePrefixo(id)))
		form.RegrasTexto[id] = r.PostFormValue(ChaveRegras(id))
	}
	return form, nil
}

func (a *Admin) reexibir(w http.ResponseWriter, r *http.Request, form Form, status int) {
	opcoes, err := a.opcoesDoForm(r.Context(), form)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, status, a.log, TelaForm(form, opcoes))
}

// opcoesDoForm devolve o catálogo de upstreams já marcado com o que o formulário
// carrega — inclusive o que a pessoa digitou e ainda não passou na validação.
func (a *Admin) opcoesDoForm(ctx context.Context, form Form) ([]UpstreamOpcao, error) {
	opcoes, err := a.ups.Opcoes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range opcoes {
		id := opcoes[i].ID
		opcoes[i].Escolhido = slices.Contains(form.UpstreamIDs, id)
		opcoes[i].Prefixo = form.Prefixo(id)
		opcoes[i].Regras = form.RegrasDe(id)
		opcoes[i].ErroPrefixo = form.Erros[ChavePrefixo(id)]
		opcoes[i].ErroRegras = form.Erros[ChaveRegras(id)]
		// O aviso de regra que não casou nada só faz sentido para quem está
		// marcado e cuja sintaxe já passou — sem isso o textarea reexibe erro
		// de sintaxe e aviso de "não casou nada" ao mesmo tempo, para a mesma
		// linha quebrada.
		if opcoes[i].Escolhido && opcoes[i].ErroRegras == "" {
			opcoes[i].AvisoRegras = avisoDeRegraSemCasar(opcoes[i].Regras, opcoes[i].NomesOriginais)
		}
	}
	return opcoes, nil
}

// avisoDeRegraSemCasar avisa, sem bloquear, de uma regra que não casou nenhuma
// ferramenta do catálogo vivo do upstream — como uma regra escrita contra o
// nome já prefixado (seção 1.4 da revisão), que nunca vai casar porque o
// filtro roda contra o nome original.
//
// Não é erro de formulário porque o catálogo vivo é só o snapshot de agora: o
// upstream pode reconectar depois com ferramentas diferentes, e uma regra que
// não casa hoje pode passar a casar amanhã sem o admin ter mudado nada.
func avisoDeRegraSemCasar(regrasTexto string, nomesOriginais []string) string {
	regras, err := catalogo.AnalisarRegras(regrasTexto)
	if err != nil {
		return ""
	}
	var semCasar []string
	for _, r := range regras {
		casou := false
		for _, nome := range nomesOriginais {
			if r.Casa(nome) {
				casou = true
				break
			}
		}
		if !casou {
			semCasar = append(semCasar, string(r.Acao)+" "+r.Padrao)
		}
	}
	if len(semCasar) == 0 {
		return ""
	}
	return fmt.Sprintf("Não casou nenhuma ferramenta do catálogo atual: %s.", strings.Join(semCasar, ", "))
}

// formDe monta o formulário de edição a partir do que está gravado.
func (a *Admin) formDe(ctx context.Context, reg Registro) (Form, error) {
	itens, err := a.repo.ComposicaoDe(ctx, reg.ID)
	if err != nil {
		return Form{}, err
	}
	form := Form{
		ID:          reg.ID,
		Slug:        reg.Slug,
		Nome:        reg.Nome,
		Descricao:   reg.Descricao,
		Instrucoes:  reg.Instrucoes,
		Prefixos:    make(map[int64]string, len(itens)),
		RegrasTexto: make(map[int64]string, len(itens)),
		SlugFixo:    true,
	}
	for _, item := range itens {
		form.UpstreamIDs = append(form.UpstreamIDs, item.UpstreamID)
		form.Prefixos[item.UpstreamID] = item.Prefixo
		form.RegrasTexto[item.UpstreamID] = catalogo.TextoDeRegras(item.Regras)
	}
	return form, nil
}

// composicao é a lista da tela de detalhe: só os upstreams que compõem, cada um
// com o que ele entrega *a este endpoint*.
func (a *Admin) composicao(ctx context.Context, reg Registro) ([]UpstreamOpcao, error) {
	form, err := a.formDe(ctx, reg)
	if err != nil {
		return nil, err
	}
	opcoes, err := a.opcoesDoForm(ctx, form)
	if err != nil {
		return nil, err
	}
	// A tela de detalhe mostra só o que compõe, não o catálogo inteiro.
	opcoes = slices.DeleteFunc(opcoes, func(o UpstreamOpcao) bool { return !o.Escolhido })

	// A contagem por upstream vem da materialização, não do banco: é o resultado
	// da composição, e é a única forma de o número na tela ser o número que o
	// cliente vê.
	contagens := a.srv.ContagemPorUpstream(reg.Slug)
	for i := range opcoes {
		opcoes[i].NoEndpoint = contagens[opcoes[i].ID]
	}
	return opcoes, nil
}

// ferramentasExpostas monta a lista da tela de detalhe, na mesma ordem de
// Expostos: quem decide a ordem é o nome exposto, e Detalhes só empresta de
// onde cada nome veio e o que a composição fez a ele.
func (a *Admin) ferramentasExpostas(slug string) []FerramentaExposta {
	nomes := a.srv.Expostos(slug)
	detalhes := a.srv.Detalhes(slug)
	out := make([]FerramentaExposta, 0, len(nomes))
	for _, nome := range nomes {
		if d, ok := detalhes[nome]; ok {
			out = append(out, d)
			continue
		}
		// Sem detalhe correspondente não deveria acontecer — Expostos e
		// Detalhes vêm da mesma materialização —, mas a tela mostra o nome de
		// qualquer forma em vez de escondê-lo.
		out = append(out, FerramentaExposta{Nome: nome})
	}
	return out
}

// avisos traduz o ?aviso= da URL na caixa de resultado da ação anterior.
var avisos = map[string]webui.Alerta{
	"criado":   {Tom: webui.TomSucesso, Titulo: "Endpoint criado.", Texto: "Emita uma chave de API com escopo nele para um cliente conectar."},
	"salvo":    {Tom: webui.TomSucesso, Titulo: "Endpoint salvo.", Texto: "A mudança já está no ar; nenhum reinício é necessário."},
	"removido": {Tom: webui.TomInfo, Titulo: "Endpoint removido.", Texto: "As sessões MCP que estavam abertas nele foram encerradas."},
}
