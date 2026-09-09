package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP do CRUD de upstream, e é onde o hot-apply acontece: toda
// escrita no banco é seguida de uma mudança no gerente e de uma rematerialização
// dos endpoints, na mesma requisição.
type Admin struct {
	repo           *RepositorioSQLite
	gerente        *Gerente
	oauth          *BrokerOAuth
	rematerializar Rematerializar
	nomeExposto    NomeExpostoDe
	log            *slog.Logger
}

// NovoAdmin monta o CRUD de upstream.
//
// O broker de OAuth pode ser nulo: sem ele o modo oauth não aparece na tela e o
// botão "Autorizar" responde que a autorização está indisponível, em vez de
// oferecer um fluxo que ninguém completaria.
func NovoAdmin(
	repo *RepositorioSQLite, gerente *Gerente, oauth *BrokerOAuth,
	rematerializar Rematerializar, nomeExposto NomeExpostoDe, log *slog.Logger,
) *Admin {
	return &Admin{
		repo:           repo,
		gerente:        gerente,
		oauth:          oauth,
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
	mux.HandleFunc("POST "+webui.RotaUpstreams+"/{id}/reconectar", a.reconectar)
	mux.HandleFunc("POST "+webui.RotaUpstreams+"/{id}/sondar", a.sondar)
	mux.HandleFunc("POST "+webui.RotaUpstreams+"/{id}/autorizar", a.autorizar)
	// O callback é caminho literal de quatro segmentos, então não compete com o
	// /{id} de três acima: o ServeMux resolve os dois sem ambiguidade.
	mux.HandleFunc("GET "+webui.RotaCallbackOAuthUpstream, a.callback)
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
			// NoCatalogo e não Ferramentas: a coluna diz o que os endpoints
			// servem, e em sonda_falhou isso é zero mesmo com o tools/list
			// guardado em memória.
			l.Estado, l.Ferramentas, l.TentativaEm = s.Estado, s.NoCatalogo, s.TentativaEm
			l.ProximaEm, l.Abandonos, l.Motivo = s.ProximaEm, s.Abandonos, s.Motivo
			l.AbandonosTotais = s.AbandonosTotais
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

// formNovo abre o formulário do tipo pedido na query.
//
// O tipo entra pela URL e não por um seletor dentro do formulário porque ele não
// muda depois: um upstream HTTP e um upstream STDIO pedem campos diferentes, e
// um formulário que mostra os dois conjuntos ao mesmo tempo obriga o admin a
// adivinhar quais valem.
//
// Nome, URL e modo de credencial também entram pela query, e é assim que a
// biblioteca (internal/biblioteca) adiciona um servidor do catálogo: ela não
// cria o upstream, ela abre este formulário preenchido. Preencher e não criar é
// deliberado — a URL vem de um catálogo de terceiro, e o admin confere antes de
// salvar. Nenhum campo de credencial entra por aqui: segredo em URL vaza para
// histórico do navegador, log de proxy e Referer.
func (a *Admin) formNovo(w http.ResponseWriter, r *http.Request) {
	// A sonda vem com os números preenchidos e desligada. Os dois juntos: campo
	// numérico em branco obrigaria o admin a inventar um valor para ligar a
	// sonda, e sonda marcada por padrão apagaria ferramenta de servidor
	// saudável no primeiro cadastro mal preenchido.
	form := Form{
		Tipo: TipoHTTP, TimeoutMS: TimeoutPadraoMS, Habilitado: true,
		SondaIntervaloMS: SondaIntervaloPadraoMS,
		SondaTimeoutMS:   SondaTimeoutPadraoMS,
		SondaTolerancia:  SondaToleranciaPadrao,
	}
	q := r.URL.Query()
	switch q.Get("tipo") {
	case TipoSTDIO:
		form.Tipo = TipoSTDIO
	case TipoSSE:
		form.Tipo = TipoSSE
	}
	preencherDaQuery(&form, q)
	if err := a.completarForm(r.Context(), &form); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaForm(form))
}

// limiteDePreenchimento corta o que vem da query antes de ela virar valor de
// input.
//
// Não é validação — Validar() ainda roda no POST. É contenção: a query é
// escrita por quem monta o link, e um nome de 200 kB no atributo value seria
// uma tela ilegível servida a partir de uma URL.
const limiteDePreenchimento = 512

// preencherDaQuery aplica os campos não sensíveis vindos da URL.
//
// Só nome, URL e modo, e só o que faz sentido para o tipo: preencher URL num
// formulário STDIO deixaria na tela um campo que aquele transporte ignora, e o
// admin leria isso como configuração em vigor. Modo só é aceito nos dois
// valores conhecidos — qualquer outra coisa cai no padrão de ModoEfetivo, em
// vez de gravar um modo que nenhum caminho do código entende.
func preencherDaQuery(form *Form, q url.Values) {
	if nome := cortar(q.Get("nome")); nome != "" {
		form.Nome = nome
	}
	if form.STDIO() {
		// No STDIO o que preenche é a execução, e ela vem em dois parâmetros
		// porque argumento com espaço não sobrevive a uma string só: "arg"
		// repete, e cada repetição é um argumento, na ordem em que veio.
		//
		// URL não entra aqui: preenchê-la num formulário STDIO deixaria na tela
		// um campo que aquele transporte ignora, e o admin leria isso como
		// configuração em vigor.
		if comando := cortar(q.Get("comando")); comando != "" {
			form.Comando = comando
		}
		if args := argsDaQuery(q); len(args) > 0 {
			form.ArgsTexto = TextoDeArgs(args)
		}
		return
	}
	if bruta := cortar(q.Get("url")); bruta != "" {
		form.URL = bruta
	}
	if q.Get("modo") == ModoOAuth {
		form.Modo = ModoOAuth
	}
}

// limiteDeArgs corta quantos argumentos a query pode preencher.
//
// Mesma razão de limiteDePreenchimento: quem monta o link escreve isto, e uma
// lista sem fim viraria uma caixa de texto sem fim servida a partir de uma URL.
const limiteDeArgs = 32

// argsDaQuery lê os argumentos repetidos, apara cada um e descarta o que não
// pode virar argumento — vazio, ou com quebra de linha, que partiria a caixa de
// texto onde um argumento é uma linha.
func argsDaQuery(q url.Values) []string {
	brutos := q["arg"]
	if len(brutos) > limiteDeArgs {
		brutos = brutos[:limiteDeArgs]
	}
	args := make([]string, 0, len(brutos))
	for _, a := range brutos {
		a = cortar(a)
		if a == "" || strings.ContainsAny(a, "\x00\n\r") {
			continue
		}
		args = append(args, a)
	}
	return args
}

// cortar apara e limita, contando runas e não bytes: cortar no byte partiria um
// caractere acentuado ao meio e o valor sairia da tela como um losango.
func cortar(v string) string {
	v = strings.TrimSpace(v)
	runas := []rune(v)
	if len(runas) > limiteDePreenchimento {
		return string(runas[:limiteDePreenchimento])
	}
	return v
}

// completarForm preenche o formulário com o que já está gravado — credenciais
// estáticas e estado de OAuth — a partir do banco.
//
// Roda antes de Validar(), e não só ao reexibir: validarOAuth recusa bearer
// estático junto com modo oauth olhando f.BearerDefinido, e esse campo só
// existe depois desta chamada. Sem isto, salvar em modo oauth um upstream que
// já tinha bearer gravado passaria pela validação vendo BearerDefinido = false
// e sairia com as duas credenciais em vigor — o bearer que clienteDe ainda
// filtra por defesa, mas que a tela diria "sem credencial nenhuma".
func (a *Admin) completarForm(ctx context.Context, form *Form) error {
	form.OAuthDisponivel = a.oauth != nil
	if form.OAuthDisponivel {
		// O redirect_uri completo, e não só o caminho: é o valor byte a byte que
		// precisa ir para o cadastro do cliente no provedor, e o admin não tem
		// como montar isso de cabeça a partir da URL pública mais o caminho.
		form.OAuthRedirectURI = a.oauth.RedirectURI()
	}
	if form.ID == 0 {
		// Upstream ainda não existe: não há o que ler, e as linhas em branco do
		// formulário são as mesmas de um formulário novo.
		form.CompletarCredenciais(nil)
		return nil
	}
	definidas, err := a.repo.CredenciaisDefinidas(ctx, form.ID)
	if err != nil {
		return err
	}
	form.CompletarCredenciais(definidas)
	estado, err := a.repo.EstadoOAuth(ctx, form.ID)
	if err != nil {
		return err
	}
	form.CompletarOAuth(estado)
	// Aviso não bloqueante: a ferramenta da sonda pode ter sido renomeada ou
	// removida do upstream depois de configurada, e só o catálogo em memória
	// sabe disso — o banco não guarda o tools/list.
	form.AvisoSondaFerramenta = avisoFerramentaForaDoCatalogo(
		form.SondaFerramenta, a.gerente.FerramentasDescobertas(form.ID))
	// O mesmo tipo de aviso, para o transporte que executa um programa: o
	// comando pode simplesmente não existir aqui.
	if form.STDIO() {
		form.AvisoComando = avisoComandoForaDoPath(form.Comando)
	}
	return nil
}

// reexibir devolve o formulário recusado com o estado das credenciais gravadas
// preenchido de novo.
//
// Sem isto, um erro de validação apagaria o "definido" da tela e o admin leria
// "nenhum bearer" sobre um upstream que tem um.
func (a *Admin) reexibir(w http.ResponseWriter, r *http.Request, status int, form Form) {
	if err := a.completarForm(r.Context(), &form); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, status, a.log, TelaForm(form))
}

func (a *Admin) criar(w http.ResponseWriter, r *http.Request) {
	form, err := lerForm(r)
	if err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	if err := a.completarForm(r.Context(), &form); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	if !form.Validar() {
		a.reexibir(w, r, http.StatusUnprocessableEntity, form)
		return
	}

	id, err := a.repo.Criar(r.Context(), form)
	switch {
	case errors.Is(err, ErrNomeEmUso):
		form.Erros = map[string]string{"nome": "Já existe um MCP com este nome."}
		a.reexibir(w, r, http.StatusConflict, form)
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	form.ID = id
	a.aplicarNoAr(r.Context(), registroDoForm(id, form))
	a.log.Info("upstream criado",
		"upstream", form.Nome, "upstream_id", id, "tipo", form.TipoEfetivo())
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

	// Também sem decifrar: só client_id, registro e instantes. O que a tela diz
	// de OAuth nunca inclui token, e é por isso que ela continua abrindo quando
	// nada do que está cifrado volta.
	estadoOAuth, err := a.repo.EstadoOAuth(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	d := Detalhe{
		Registro:      reg,
		Endpoints:     slugs,
		Credenciais:   credenciais,
		OAuth:         estadoOAuth,
		TetoAbandonos: a.gerente.TetoDeAbandonos(),
	}
	if s, ok := a.gerente.Situacao(reg.ID); ok {
		d.Estado, d.TentativaEm, d.Supervisionado = s.Estado, s.TentativaEm, true
		d.ProximaEm, d.Falhas, d.Abandonos, d.Motivo = s.ProximaEm, s.Falhas, s.Abandonos, s.Motivo
		d.AbandonosTotais = s.AbandonosTotais
		d.SondaAtual, d.NoCatalogo = s.Sonda, s.NoCatalogo
		if s.UltimoErro != "" {
			d.UltimoErro = s.UltimoErro
		}
	}
	// FerramentasDescobertas e não Ferramentas: em sonda_falhou o catálogo está
	// vazio de propósito, e é justamente aí que o admin precisa ver quais
	// ferramentas saíram dos endpoints por causa da sonda.
	for _, t := range a.gerente.FerramentasDescobertas(reg.ID) {
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

// reconectar rearma a supervisão de um upstream agora, sem esperar o backoff.
//
// É o botão que a seção 11 exige ao lado do motivo: sem ele, um upstream que se
// desabilitou por conta própria só voltaria com um boot ou com uma edição que
// não muda nada. Aplicar é a primitiva certa porque reconectar é descartar o
// transporte e criar outro — a única correção conhecida da issue #683.
func (a *Admin) reconectar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	if !reg.Habilitado {
		// Reconectar um upstream desabilitado seria desfazer a intenção do admin
		// por um clique que não diz isso.
		webui.Redirecionar(w, r, rotaDo(reg.ID)+"?aviso=desabilitado")
		return
	}
	a.aplicarNoAr(r.Context(), reg)
	a.log.Info("reconexão de upstream pedida pela tela",
		"upstream", reg.Nome, "upstream_id", reg.ID)
	webui.Redirecionar(w, r, rotaDo(reg.ID)+"?aviso=reconectando")
}

// margemDaSondagemManual é quanto o clique espera além do prazo de até duas
// sondagens.
//
// O pedido atravessa um canal até a goroutine de supervisão, e ela pode estar
// no meio de outra coisa quando ele chega: uma sondagem periódica já em curso,
// que só devolve o select ao fim do próprio timeout. Por isso o prazo do botão
// é 2×Timeout — a que já estava rodando mais a que o clique pediu — e não 1×; a
// margem cobre só a fila do canal e a volta da resposta. Passado isso, a tela
// desiste e diz para recarregar — nunca fica pendurada, porque uma requisição
// de admin presa é indistinguível de UI travada.
const margemDaSondagemManual = 5 * time.Second

// sondar executa uma sondagem agora, a pedido da tela.
//
// A chamada não acontece nesta goroutine: Sondar entrega o pedido à supervisão e
// espera o desfecho. É o que mantém um único escritor do estado da sonda e o que
// impede que um clique passe a abrir tools/call a partir do handler HTTP.
func (a *Admin) sondar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	if !reg.Sonda.Ativa() {
		webui.Redirecionar(w, r, rotaDo(reg.ID)+"?aviso=sonda_desligada")
		return
	}

	prazo := 2*reg.Sonda.Normalizada().Timeout + margemDaSondagemManual
	ctx, cancelar := context.WithTimeout(r.Context(), prazo)
	defer cancelar()

	res, err := a.gerente.Sondar(ctx, reg.ID)
	switch {
	case errors.Is(err, ErrSondaDesligada):
		webui.Redirecionar(w, r, rotaDo(reg.ID)+"?aviso=sonda_desligada")
		return
	case err != nil:
		// Erro aqui é "não deu para sondar" — sem sessão, gerente desligando,
		// prazo estourado. Nada disso é falha da sonda, e contá-lo como tal
		// derrubaria o catálogo por um clique num momento ruim.
		a.log.Warn("sondagem pedida pela tela não pôde ser executada",
			"upstream", reg.Nome, "upstream_id", reg.ID, "erro", err)
		webui.Redirecionar(w, r, rotaDo(reg.ID)+"?aviso=sonda_indisponivel")
		return
	}

	a.log.Info("sondagem pedida pela tela",
		"upstream", reg.Nome, "upstream_id", reg.ID,
		"ferramenta", reg.Sonda.Ferramenta, "ok", res.OK)
	aviso := "sonda_ok"
	if !res.OK {
		aviso = "sonda_falhou"
	}
	webui.Redirecionar(w, r, rotaDo(reg.ID)+"?aviso="+aviso)
}

func (a *Admin) formEditar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	form := Form{
		ID:         reg.ID,
		Nome:       reg.Nome,
		Tipo:       reg.Tipo,
		URL:        reg.URL,
		Comando:    reg.Comando,
		ArgsTexto:  TextoDeArgs(reg.Args),
		Args:       reg.Args,
		EnvTexto:   TextoDeEnv(reg.Env),
		Env:        reg.Env,
		TimeoutMS:  reg.TimeoutMS,
		Habilitado: reg.Habilitado,
		Modo:       reg.ModoEfetivo(),

		SondaHabilitada:  reg.Sonda.Habilitada,
		SondaFerramenta:  reg.Sonda.Ferramenta,
		SondaArgs:        string(reg.Sonda.Args),
		SondaEspera:      reg.Sonda.Espera,
		SondaIntervaloMS: reg.Sonda.Intervalo.Milliseconds(),
		SondaTimeoutMS:   reg.Sonda.Timeout.Milliseconds(),
		SondaTolerancia:  reg.Sonda.Tolerancia,
	}
	if err := a.completarForm(r.Context(), &form); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
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
	// O tipo vem do banco e não do corpo da requisição: ele não é editável, e
	// aceitá-lo do formulário deixaria um POST forjado trocar o transporte de um
	// upstream sem que nada na tela dissesse isso.
	form.Tipo = reg.Tipo
	if err := a.completarForm(r.Context(), &form); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
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
		form.Erros = map[string]string{"nome": "Já existe um MCP com este nome."}
		a.reexibir(w, r, http.StatusConflict, form)
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	a.aplicarNoAr(r.Context(), registroDoForm(reg.ID, form))
	a.log.Info("upstream atualizado", "upstream", form.Nome, "upstream_id", reg.ID,
		"tipo", form.TipoEfetivo(), "habilitado", form.Habilitado)
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
	// Nos números da sonda o zero significa outra coisa: campo em branco é
	// "use o padrão", e validarSonda o preenche. Só o que foi digitado e não é
	// número vira erro de campo, com a mensagem da faixa.
	sondaIntervalo := inteiroDoForm(r.PostFormValue("sonda_intervalo_ms"))
	sondaTimeout := inteiroDoForm(r.PostFormValue("sonda_timeout_ms"))
	sondaTolerancia := inteiroDoForm(r.PostFormValue("sonda_tolerancia"))
	return Form{
		Nome:      r.PostFormValue("nome"),
		Tipo:      r.PostFormValue("tipo"),
		URL:       r.PostFormValue("url"),
		Comando:   r.PostFormValue("comando"),
		ArgsTexto: r.PostFormValue("args"),
		EnvTexto:  r.PostFormValue("env"),
		TimeoutMS: timeout,
		// Checkbox só chega quando marcado.
		Habilitado:   r.PostFormValue("habilitado") != "",
		Bearer:       cripto.Segredo(r.PostFormValue("bearer")),
		BearerLimpar: r.PostFormValue("bearer_limpar") != "",
		Headers:      lerHeaders(r.PostForm),
		EnvSecretos:  lerEnvSecretos(r.PostForm),

		Modo:               r.PostFormValue("modo"),
		OAuthClientID:      r.PostFormValue("oauth_client_id"),
		OAuthSegredo:       cripto.Segredo(r.PostFormValue("oauth_segredo")),
		OAuthSegredoLimpar: r.PostFormValue("oauth_segredo_limpar") != "",
		OAuthIssuer:        r.PostFormValue("oauth_issuer"),

		SondaHabilitada:  r.PostFormValue("sonda_habilitada") != "",
		SondaFerramenta:  r.PostFormValue("sonda_ferramenta"),
		SondaArgs:        r.PostFormValue("sonda_args"),
		SondaEspera:      r.PostFormValue("sonda_espera"),
		SondaIntervaloMS: sondaIntervalo,
		SondaTimeoutMS:   sondaTimeout,
		SondaTolerancia:  int(sondaTolerancia),
	}, nil
}

// inteiroDoForm lê um número do formulário. Campo em branco vira zero, que os
// campos da sonda leem como "use o padrão"; texto que não é número vira -1, que
// cai fora de toda faixa e produz a mensagem de erro no campo certo.
func inteiroDoForm(bruto string) int64 {
	if strings.TrimSpace(bruto) == "" {
		return 0
	}
	n, err := strconv.ParseInt(bruto, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// registroDoForm monta o registro que vai para o gerente depois de o banco já
// ter aceitado o formulário.
//
// Existe para que criar e atualizar não repitam a lista de campos: repetição
// aqui é como um campo novo entra no banco e não entra na supervisão, e o
// sintoma é "salvei e não mudou nada até reiniciar".
func registroDoForm(id int64, f Form) Registro {
	return Registro{
		ID:         id,
		Nome:       f.Nome,
		Tipo:       f.TipoEfetivo(),
		URL:        f.URL,
		Comando:    f.Comando,
		Args:       f.Args,
		Env:        f.Env,
		TimeoutMS:  f.TimeoutMS,
		Habilitado: f.Habilitado,
		Modo:       f.ModoEfetivo(),
		Sonda:      f.SondaDoForm(),
	}
}

// lerEnvSecretos lê as linhas de variável de ambiente cifrada.
//
// Mesma mecânica dos headers: env_nome e env_valor são arrays paralelos na ordem
// do documento, e o checkbox de limpar viaja por nome porque checkbox só é
// enviado quando marcado e desalinharia os dois arrays.
func lerEnvSecretos(campos url.Values) []CampoEnv {
	nomes := campos["env_nome"]
	valores := campos["env_valor"]

	limpar := make(map[string]bool, len(campos["env_limpar"]))
	for _, nome := range campos["env_limpar"] {
		limpar[strings.TrimSpace(nome)] = true
	}

	out := make([]CampoEnv, 0, len(nomes))
	for i, nome := range nomes {
		e := CampoEnv{Nome: nome, Limpar: limpar[strings.TrimSpace(nome)]}
		if i < len(valores) {
			e.Valor = cripto.Segredo(valores[i])
		}
		out = append(out, e)
	}
	return out
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
	"criado":       {Tom: webui.TomSucesso, Titulo: "MCP criado.", Texto: "A conexão já está sendo tentada; o estado abaixo se atualiza a cada recarga."},
	"salvo":        {Tom: webui.TomSucesso, Titulo: "MCP salvo.", Texto: "A sessão antiga foi fechada e uma nova está sendo aberta com a configuração nova."},
	"removido":     {Tom: webui.TomInfo, Titulo: "MCP removido.", Texto: "A sessão e a goroutine de supervisão foram encerradas, e os endpoints já refletem a remoção."},
	"reconectando": {Tom: webui.TomInfo, Titulo: "Reconexão pedida.", Texto: "A sessão antiga foi descartada, o backoff voltou ao começo e o contador de abandonos zerou."},
	"desabilitado": {Tom: webui.TomAlerta, Titulo: "O MCP está desabilitado.", Texto: "Habilite-o na edição para que a supervisão volte a tentar."},

	"sonda_ok": {
		Tom:    webui.TomSucesso,
		Titulo: "A sondagem passou.",
		Texto: "A ferramenta configurada respondeu sem erro. Se o MCP estava em " +
			"sonda_falhou, as ferramentas dele já voltaram ao catálogo dos endpoints.",
	},
	"sonda_falhou": {
		Tom:    webui.TomPerigo,
		Titulo: "A sondagem falhou.",
		Texto: "A requisição e a resposta exatas estão abaixo. Confira primeiro se a sonda " +
			"está bem configurada — ferramenta errada ou argumento faltando parece servidor quebrado.",
	},
	"sonda_desligada": {
		Tom:    webui.TomAlerta,
		Titulo: "A sonda deste MCP está desligada.",
		Texto: "Ela é opt-in: escolha a ferramenta e ligue-a na edição. O patchbay não " +
			"adivinha qual chamada é inócua — quem sabe isso é você.",
	},
	"sonda_indisponivel": {
		Tom:    webui.TomAlerta,
		Titulo: "Não deu para sondar agora.",
		Texto: "A sonda usa a sessão que já está aberta, e ela não respondeu a tempo " +
			"— sem sessão de pé, ou ocupada demais para atender ao clique. Isto não " +
			"conta como falha da sonda. Recarregue a página para ver o estado atual.",
	},

	"autorizado": {
		Tom:    webui.TomSucesso,
		Titulo: "Consentimento recebido.",
		Texto: "O código voltou do provedor e a troca por token acontece na supervisão, " +
			"não nesta requisição. O estado abaixo se atualiza a cada recarga.",
	},
	"consentimento_invalido": {
		Tom:    webui.TomPerigo,
		Titulo: "Consentimento não reconhecido.",
		Texto: "O state não pertence a nenhuma autorização em curso — ou ela já foi usada, " +
			"ou expirou. Clique em Autorizar de novo.",
	},
	"consentimento_recusado": {
		Tom:    webui.TomAlerta,
		Titulo: "O provedor recusou a autorização.",
		Texto:  "Nada foi gravado. Confira a conta usada no provedor e tente de novo.",
	},
	"consentimento_demorou": {
		Tom:    webui.TomAlerta,
		Titulo: "A autorização ainda está sendo preparada.",
		Texto: "A supervisão precisa reconectar e descobrir o authorization server antes de " +
			"montar a URL. Clique em Autorizar de novo em alguns segundos.",
	},
	"consentimento_falhou": {
		Tom:    webui.TomPerigo,
		Titulo: "A autorização não pôde começar.",
		Texto:  "O motivo está no log do patchbay, com o nome do MCP e sem nenhum segredo.",
	},
	"oauth_indisponivel": {
		Tom:    webui.TomAlerta,
		Titulo: "Este MCP não usa OAuth.",
		Texto:  "Troque o modo de credencial na edição para autorizá-lo.",
	},
	"oauth_desabilitado": {
		Tom:    webui.TomAlerta,
		Titulo: "O MCP está desabilitado.",
		Texto:  "Sem supervisão não há quem monte a URL de autorização. Habilite-o antes.",
	},
}
