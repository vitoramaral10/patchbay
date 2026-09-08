package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP do CRUD de upstream, e é onde o hot-apply acontece: toda
// escrita no banco é seguida de uma mudança no gerente e de uma rematerialização
// dos endpoints, na mesma requisição.
type Admin struct {
	repo           *RepositorioSQLite
	gerente        *Gerente
	rematerializar Rematerializar
	nomeExposto    NomeExpostoDe
	log            *slog.Logger
}

// NovoAdmin monta o CRUD de upstream.
func NovoAdmin(repo *RepositorioSQLite, gerente *Gerente, rematerializar Rematerializar, nomeExposto NomeExpostoDe, log *slog.Logger) *Admin {
	return &Admin{
		repo:           repo,
		gerente:        gerente,
		rematerializar: rematerializar,
		nomeExposto:    nomeExposto,
		log:            log,
	}
}

// Rotas registra as telas de upstream. Todas exigem sessão de admin.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaUpstreams, a.listar)
	mux.HandleFunc("GET "+webui.RotaUpstreams+"/novo", a.formNovo)
	mux.HandleFunc("POST "+webui.RotaUpstreams, a.criar)
	mux.HandleFunc("GET "+webui.RotaUpstreams+"/{id}", a.detalhe)
	mux.HandleFunc("GET "+webui.RotaUpstreams+"/{id}/editar", a.formEditar)
	mux.HandleFunc("POST "+webui.RotaUpstreams+"/{id}", a.atualizar)
	mux.HandleFunc("POST "+webui.RotaUpstreams+"/{id}/remover", a.remover)
}

func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	regs, err := a.repo.Todos(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	contagens, err := a.repo.ContagemDeEndpoints(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	linhas := make([]Linha, 0, len(regs))
	for _, reg := range regs {
		l := Linha{Registro: reg, Endpoints: contagens[reg.ID]}
		if s, ok := a.gerente.Situacao(reg.ID); ok {
			l.Estado, l.Ferramentas, l.TentativaEm = s.Estado, s.Ferramentas, s.TentativaEm
			l.Supervisionado = true
			// O último erro em memória é mais novo que o do banco: é o da
			// tentativa em curso, e é o que o admin precisa ver.
			if s.UltimoErro != "" {
				l.UltimoErro = s.UltimoErro
			}
		}
		linhas = append(linhas, l)
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaLista(linhas, webui.Avisos(r, avisos)))
}

func (a *Admin) formNovo(w http.ResponseWriter, r *http.Request) {
	form := Form{TimeoutMS: TimeoutPadraoMS, Habilitado: true}
	form.CompletarHeaders(nil)
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(form))
}

// reexibir devolve o formulário recusado com o estado das credenciais gravadas
// preenchido de novo.
//
// Sem isto, um erro de validação apagaria o "definido" da tela e o admin leria
// "nenhum bearer" sobre um upstream que tem um.
func (a *Admin) reexibir(w http.ResponseWriter, r *http.Request, status int, form Form) {
	if form.ID != 0 {
		definidas, err := a.repo.CredenciaisDefinidas(r.Context(), form.ID)
		if err != nil {
			webui.ErroInterno(w, r, a.log, err)
			return
		}
		form.CompletarHeaders(definidas)
	} else {
		form.CompletarHeaders(nil)
	}
	webui.Renderizar(w, r, status, a.log, TelaForm(form))
}

func (a *Admin) criar(w http.ResponseWriter, r *http.Request) {
	form, err := lerForm(r)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	if !form.Validar() {
		a.reexibir(w, r, http.StatusUnprocessableEntity, form)
		return
	}

	id, err := a.repo.Criar(r.Context(), form)
	switch {
	case errors.Is(err, ErrNomeEmUso):
		form.Erros = map[string]string{"nome": "Já existe um upstream com este nome."}
		a.reexibir(w, r, http.StatusConflict, form)
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	form.ID = id
	a.aplicarNoAr(r.Context(), Registro{
		ID: id, Nome: form.Nome, Tipo: TipoHTTP, URL: form.URL,
		TimeoutMS: form.TimeoutMS, Habilitado: form.Habilitado,
	})
	a.log.Info("upstream criado", "upstream", form.Nome, "upstream_id", id)
	webui.Redirecionar(w, r, webui.RotaUpstreams+"/"+strconv.FormatInt(id, 10)+"?aviso=criado")
}

func (a *Admin) detalhe(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	slugs, err := a.repo.EndpointsDo(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	// Definidas e não decifradas: a tela precisa saber que existe credencial,
	// nunca qual é — e assim ela continua abrindo mesmo com a chave mestra
	// trocada, em vez de virar um 500 no meio do diagnóstico.
	credenciais, err := a.repo.CredenciaisDefinidas(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	d := Detalhe{Registro: reg, Endpoints: slugs, Credenciais: credenciais}
	if s, ok := a.gerente.Situacao(reg.ID); ok {
		d.Estado, d.TentativaEm, d.Supervisionado = s.Estado, s.TentativaEm, true
		if s.UltimoErro != "" {
			d.UltimoErro = s.UltimoErro
		}
	}
	for _, t := range a.gerente.Ferramentas(reg.ID) {
		if t == nil {
			continue
		}
		nome, avisos := a.nomeExposto(t)
		d.Ferramentas = append(d.Ferramentas, FerramentaDescoberta{
			NomeExposto:  nome,
			NomeOriginal: t.Name,
			Descricao:    t.Description,
			Avisos:       avisos,
		})
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaDetalhe(d, webui.Avisos(r, avisos)))
}

func (a *Admin) formEditar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	form := Form{
		ID:         reg.ID,
		Nome:       reg.Nome,
		URL:        reg.URL,
		TimeoutMS:  reg.TimeoutMS,
		Habilitado: reg.Habilitado,
	}
	definidas, err := a.repo.CredenciaisDefinidas(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	form.CompletarHeaders(definidas)
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(form))
}

func (a *Admin) atualizar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	form, err := lerForm(r)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	form.ID = reg.ID
	if !form.Validar() {
		a.reexibir(w, r, http.StatusUnprocessableEntity, form)
		return
	}

	err = a.repo.Atualizar(r.Context(), reg.ID, form)
	switch {
	case errors.Is(err, ErrNaoEncontrado):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrNomeEmUso):
		form.Erros = map[string]string{"nome": "Já existe um upstream com este nome."}
		a.reexibir(w, r, http.StatusConflict, form)
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	a.aplicarNoAr(r.Context(), Registro{
		ID: reg.ID, Nome: form.Nome, Tipo: TipoHTTP, URL: form.URL,
		TimeoutMS: form.TimeoutMS, Habilitado: form.Habilitado,
	})
	a.log.Info("upstream atualizado", "upstream", form.Nome, "upstream_id", reg.ID,
		"habilitado", form.Habilitado)
	webui.Redirecionar(w, r, webui.RotaUpstreams+"/"+strconv.FormatInt(reg.ID, 10)+"?aviso=salvo")
}

func (a *Admin) remover(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	// O banco é a fonte de verdade e sai primeiro: assim uma falha no meio deixa
	// o processo supervisionando um upstream que já não existe (visível no log e
	// resolvido no próximo boot), e não um upstream cadastrado que ninguém
	// supervisiona.
	if err := a.repo.Remover(r.Context(), reg.ID); err != nil {
		if errors.Is(err, ErrNaoEncontrado) {
			http.NotFound(w, r)
			return
		}
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	if err := a.gerente.Remover(r.Context(), reg.ID); err != nil {
		a.log.Error("upstream apagado do banco mas não saiu da supervisão",
			"upstream", reg.Nome, "upstream_id", reg.ID, "erro", err)
	}
	a.sincronizar(r.Context())
	a.log.Info("upstream removido", "upstream", reg.Nome, "upstream_id", reg.ID)
	webui.Redirecionar(w, r, webui.RotaUpstreams+"?aviso=removido")
}

// aplicarNoAr põe o upstream no estado que o banco agora descreve.
//
// habilitado é a intenção do admin e o único estado persistido (seção 08.8):
// habilitado significa "sob supervisão", desabilitado significa "fora dela, com
// a sessão fechada". Traduzir a intenção aqui é o que faz o botão da UI ter
// efeito imediato em vez de esperar o próximo boot.
func (a *Admin) aplicarNoAr(ctx context.Context, reg Registro) {
	var err error
	if reg.Habilitado {
		err = a.gerente.Aplicar(ctx, reg.Config())
	} else {
		err = a.gerente.Remover(ctx, reg.ID)
	}
	if err != nil {
		a.log.Error("upstream gravado mas não aplicado em tempo de execução",
			"upstream", reg.Nome, "upstream_id", reg.ID, "erro", err)
	}
	a.sincronizar(ctx)
}

func (a *Admin) sincronizar(ctx context.Context) {
	if a.rematerializar == nil {
		return
	}
	if err := a.rematerializar(ctx); err != nil {
		a.log.Error("falha ao rematerializar endpoints após mudança de upstream", "erro", err)
	}
}

func (a *Admin) upstreamDaRota(w http.ResponseWriter, r *http.Request) (Registro, bool) {
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

func lerForm(r *http.Request) (Form, error) {
	if err := r.ParseForm(); err != nil {
		return Form{}, err
	}
	timeout, err := strconv.ParseInt(r.PostFormValue("timeout_ms"), 10, 64)
	if err != nil {
		// Zero cai na faixa inválida e a validação escreve a mensagem certa —
		// melhor que um 400 sem explicação de qual campo estava errado.
		timeout = 0
	}
	return Form{
		Nome:      r.PostFormValue("nome"),
		URL:       r.PostFormValue("url"),
		TimeoutMS: timeout,
		// Checkbox só chega quando marcado.
		Habilitado:   r.PostFormValue("habilitado") != "",
		Bearer:       cripto.Segredo(r.PostFormValue("bearer")),
		BearerLimpar: r.PostFormValue("bearer_limpar") != "",
		Headers:      lerHeaders(r.PostForm),
	}, nil
}

// lerHeaders lê as linhas de header estático do formulário.
//
// header_nome e header_valor são arrays paralelos: cada linha da tela contribui
// com exatamente um de cada, na ordem do documento. O checkbox de limpar viaja
// à parte, por nome, porque checkbox só é enviado quando marcado e desalinharia
// os dois arrays.
func lerHeaders(campos url.Values) []CampoHeader {
	nomes := campos["header_nome"]
	valores := campos["header_valor"]

	limpar := make(map[string]bool, len(campos["header_limpar"]))
	for _, nome := range campos["header_limpar"] {
		limpar[strings.ToLower(strings.TrimSpace(nome))] = true
	}

	out := make([]CampoHeader, 0, len(nomes))
	for i, nome := range nomes {
		h := CampoHeader{Nome: nome, Limpar: limpar[strings.ToLower(strings.TrimSpace(nome))]}
		if i < len(valores) {
			h.Valor = cripto.Segredo(valores[i])
		}
		out = append(out, h)
	}
	return out
}

var avisos = map[string]webui.Alerta{
	"criado":   {Tom: webui.TomSucesso, Titulo: "Upstream criado.", Texto: "A conexão já está sendo tentada; o estado abaixo se atualiza a cada recarga."},
	"salvo":    {Tom: webui.TomSucesso, Titulo: "Upstream salvo.", Texto: "A sessão antiga foi fechada e uma nova está sendo aberta com a configuração nova."},
	"removido": {Tom: webui.TomInfo, Titulo: "Upstream removido.", Texto: "A sessão e a goroutine de supervisão foram encerradas, e os endpoints já refletem a remoção."},
}
