package upstream

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/versao"
)

// ErrGerenteParado indica operação de ciclo de vida pedida a um gerente que
// nunca iniciou ou que já desligou.
var ErrGerenteParado = errors.New("upstream: gerente não está em execução")

// Gerente supervisiona todos os upstreams configurados.
//
// O ciclo de vida (adicionar, reconfigurar, remover) é serializado numa única
// goroteina despachante, e é ela que possui o mapa de supervisões e o contexto
// raiz. Duas razões, e as duas doem se ignoradas:
//
//   - Guardar o contexto raiz num campo faria o struct carregar contexto (o que
//     containedctx reprova, e com motivo: o ciclo de vida do gerente passaria a
//     depender de qual requisição chegou primeiro).
//   - Sem serialização, dois cliques na UI podem parar e subir a supervisão do
//     mesmo upstream em ordem trocada, e o resultado é um upstream sem ninguém
//     supervisionando — ou dois.
type Gerente struct {
	log           *slog.Logger
	cliente       *http.Client
	backoff       Backoff
	relogio       Relogio
	tetoAbandonos int
	aoMudar       func(context.Context)
	credenciais   LerCredenciais
	oauth         *BrokerOAuth
	margemRenovar time.Duration
	tiqueRenovar  time.Duration

	comandos  chan comando
	encerrado chan struct{}
	iniciou   sync.Once

	mu         sync.RWMutex
	servidores map[int64]*servidor

	wg sync.WaitGroup
}

type servidor struct {
	cfg         Config
	estado      Estado
	ultimoErro  string
	motivo      string
	ferramentas []*mcp.Tool
	sessao      *mcp.ClientSession
	tentativaEm time.Time
	proximaEm   time.Time
	falhas      int
	// abandonos é o consecutivo desde o último pronto: é o que abandonosEstouraram
	// compara contra o teto. abandonosTotais nunca zera sozinho (só definir o
	// apaga) e é o resíduo acumulado que a tela mostra por trás dele.
	abandonos       int
	abandonosTotais int
}

// comando é uma mudança de ciclo de vida pedida de fora.
type comando struct {
	// aplicar não-nulo adiciona ou reconfigura; nulo remove removerID.
	aplicar   *Config
	removerID int64
	pronto    chan error
}

// supervisao é a alça de uma goroutine de supervisão. Só a despachante a toca,
// então não precisa de lock.
type supervisao struct {
	cancelar context.CancelFunc
	feito    chan struct{}
}

// Opcao ajusta o gerente na construção.
type Opcao func(*Gerente)

// ComIntervaloTentativa fixa a espera entre tentativas, sem crescimento nem
// jitter. É o que o teste usa para não depender da curva do backoff.
func ComIntervaloTentativa(d time.Duration) Opcao {
	return func(g *Gerente) { g.backoff = BackoffFixo(d) }
}

// ComBackoff troca a curva de reconexão.
func ComBackoff(b Backoff) Opcao {
	return func(g *Gerente) { g.backoff = b }
}

// ComRelogio troca o relógio da supervisão. O teste injeta o seu para o backoff
// ficar determinístico sem esperar por tempo de verdade.
func ComRelogio(r Relogio) Opcao {
	return func(g *Gerente) {
		if r != nil {
			g.relogio = r
		}
	}
}

// ComTetoDeAbandonos troca quantos connects abandonados a supervisão de um
// upstream tolera antes de se desligar sozinha. Zero ou negativo desliga a
// autoproteção, e aí o resíduo de goroutines é ilimitado — só faz sentido em
// teste.
func ComTetoDeAbandonos(n int) Opcao {
	return func(g *Gerente) { g.tetoAbandonos = n }
}

// ComClienteHTTP troca o cliente HTTP usado nos upstreams HTTP e SSE.
func ComClienteHTTP(c *http.Client) Opcao {
	return func(g *Gerente) { g.cliente = c }
}

// ComRenovacaoDeToken troca a margem e a frequência da renovação proativa do
// token OAuth de upstream.
//
// margem é com quanta antecedência o token é renovado; tique é de quanto em
// quanto tempo a supervisão verifica. O teste encurta os dois para não esperar
// pelo relógio.
func ComRenovacaoDeToken(margem, tique time.Duration) Opcao {
	return func(g *Gerente) {
		if margem > 0 {
			g.margemRenovar = margem
		}
		if tique > 0 {
			g.tiqueRenovar = tique
		}
	}
}

// AoMudar registra o que fazer quando o catálogo de algum upstream muda. É por
// aí que o endpoint rematerializa.
func AoMudar(fn func(context.Context)) Opcao {
	return func(g *Gerente) { g.aoMudar = fn }
}

// NovoGerente monta o gerente. Configuração inválida é recusada com log e o
// upstream fica fora da supervisão — um upstream mal configurado não impede os
// outros de subir.
func NovoGerente(log *slog.Logger, cfgs []Config, opcoes ...Opcao) *Gerente {
	g := &Gerente{
		log:           log,
		cliente:       &http.Client{},
		backoff:       BackoffPadrao(),
		relogio:       relogioReal{},
		tetoAbandonos: TetoAbandonosPadrao,
		margemRenovar: MargemDeRenovacaoPadrao,
		tiqueRenovar:  IntervaloDeRenovacaoPadrao,
		comandos:      make(chan comando),
		encerrado:     make(chan struct{}),
		servidores:    make(map[int64]*servidor, len(cfgs)),
	}
	for _, o := range opcoes {
		o(g)
	}
	for _, cfg := range cfgs {
		if err := cfg.Validar(); err != nil {
			log.Error("upstream fora da supervisão", "upstream", cfg.Nome, "erro", err)
			continue
		}
		g.servidores[cfg.ID] = &servidor{cfg: cfg, estado: EstadoNovo}
	}
	return g
}

// Iniciar sobe a goroutine despachante e volta na hora.
//
// Tudo morre com o cancelamento de ctx; Aguardar espera. Chamar duas vezes é
// no-op: o gerente tem um único ciclo de vida.
func (g *Gerente) Iniciar(ctx context.Context) {
	g.iniciou.Do(func() {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			g.despachar(ctx)
		}()
	})
}

// Aguardar espera a despachante e todas as supervisões terminarem.
func (g *Gerente) Aguardar() { g.wg.Wait() }

// Aplicar adiciona um upstream à supervisão ou reconfigura um que já está lá.
//
// Reconfigurar é parar e subir de novo, nunca mutar a sessão viva: URL e timeout
// entram no transporte na hora do Connect, e a conexão pode estar envenenada por
// um erro transitório (issue #683 do go-sdk). O retorno só acontece depois que a
// supervisão antiga morreu e a nova está no ar, para que a UI nunca mostre
// "salvo" sobre um estado que ainda não existe.
func (g *Gerente) Aplicar(ctx context.Context, cfg Config) error {
	if err := cfg.Validar(); err != nil {
		return err
	}
	return g.enviar(ctx, comando{aplicar: &cfg})
}

// Remover tira um upstream da supervisão, fechando a sessão e a goroutine.
//
// Só volta depois que a goroutine morreu: como o Close da sessão está no defer
// dela, voltar antes deixaria a sessão do upstream aberta enquanto a UI já
// mostra o upstream como removido. Remover o que não existe não é erro.
func (g *Gerente) Remover(ctx context.Context, id int64) error {
	return g.enviar(ctx, comando{removerID: id})
}

func (g *Gerente) enviar(ctx context.Context, c comando) error {
	c.pronto = make(chan error, 1)
	select {
	case g.comandos <- c:
	case <-g.encerrado:
		return ErrGerenteParado
	case <-ctx.Done():
		return fmt.Errorf("upstream: enviar comando: %w", ctx.Err())
	}
	select {
	case err := <-c.pronto:
		return err
	case <-g.encerrado:
		return ErrGerenteParado
	case <-ctx.Done():
		return fmt.Errorf("upstream: aguardar comando: %w", ctx.Err())
	}
}

// despachar é a dona do mapa de supervisões e do contexto raiz.
func (g *Gerente) despachar(ctx context.Context) {
	supervisoes := make(map[int64]*supervisao)
	defer close(g.encerrado)
	defer func() {
		// Desligamento: cada supervisão morre pelo próprio contexto derivado, e
		// esperar aqui é o que garante que nenhuma sessão de upstream fica
		// aberta depois que o processo diz que desligou.
		for id := range supervisoes {
			pararSupervisao(supervisoes, id)
		}
	}()

	g.mu.RLock()
	ids := make([]int64, 0, len(g.servidores))
	for id := range g.servidores {
		ids = append(ids, id)
	}
	g.mu.RUnlock()
	for _, id := range ids {
		supervisoes[id] = g.iniciarSupervisao(ctx, id)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case c := <-g.comandos:
			c.pronto <- g.executar(ctx, supervisoes, c)
		}
	}
}

func (g *Gerente) executar(ctx context.Context, supervisoes map[int64]*supervisao, c comando) error {
	if c.aplicar != nil {
		cfg := *c.aplicar
		if err := cfg.Validar(); err != nil {
			return err
		}
		pararSupervisao(supervisoes, cfg.ID)
		// O OAuth do upstream é descartado junto: handler e fonte de token foram
		// construídos com o client_id e o segredo que estavam gravados, e o que
		// acabou de ser salvo pode ser outro. Reaproveitá-los faria trocar o
		// client_id pela tela não ter efeito nenhum até o próximo boot.
		g.esquecerOAuth(cfg.ID)
		g.definir(cfg)
		supervisoes[cfg.ID] = g.iniciarSupervisao(ctx, cfg.ID)
		g.log.Info("upstream sob supervisão", "upstream", cfg.Nome, "upstream_id", cfg.ID)
		// O catálogo mudou agora: as ferramentas da sessão antiga saíram e as da
		// nova ainda não chegaram. Notificar aqui é o que faz o endpoint ficar
		// com lista honesta durante a reconexão, em vez de mostrar ferramenta de
		// uma sessão que já morreu.
		g.notificarMudanca(ctx)
		return nil
	}

	pararSupervisao(supervisoes, c.removerID)
	g.esquecerOAuth(c.removerID)
	nome := g.esquecer(c.removerID)
	if nome != "" {
		g.log.Info("upstream fora da supervisão", "upstream", nome, "upstream_id", c.removerID)
	}
	g.notificarMudanca(ctx)
	return nil
}

func (g *Gerente) esquecerOAuth(id int64) {
	if g.oauth != nil {
		g.oauth.Esquecer(id)
	}
}

func (g *Gerente) iniciarSupervisao(base context.Context, id int64) *supervisao {
	ctx, cancelar := context.WithCancel(base)
	s := &supervisao{cancelar: cancelar, feito: make(chan struct{})}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer close(s.feito)
		g.supervisionar(ctx, id)
	}()
	return s
}

// pararSupervisao cancela e espera. A espera é o ponto: sem ela, "removido" na
// UI não significa "sessão fechada" no processo.
func pararSupervisao(supervisoes map[int64]*supervisao, id int64) {
	s, ok := supervisoes[id]
	if !ok {
		return
	}
	s.cancelar()
	<-s.feito
	delete(supervisoes, id)
}

// supervisionar mantém uma sessão viva para um upstream.
//
// É o laço da máquina de estados da seção 05: novo → conectando → pronto, e
// conectando/pronto → degradado → (backoff) → conectando. Não existe transição
// direta degradado → pronto, porque a conexão pode estar envenenada por um erro
// transitório (issue #683 do go-sdk) e a única correção conhecida é descartar o
// transporte e criar outro.
//
// A saída de fim de linha é degradado → desabilitado, quando o contador de
// connects abandonados passa do teto. Aí esta goroutine devolve, e só um
// Aplicar (o botão de reconectar, ou salvar o upstream) a traz de volta.
func (g *Gerente) supervisionar(ctx context.Context, id int64) {
	falhas := 0
	for {
		chegouPronto, err := g.conectarEDescobrir(ctx, id)
		if ctx.Err() != nil {
			return
		}
		if chegouPronto {
			// A sessão chegou a servir: o que a derrubou é a primeira falha
			// desta rodada, não a enésima de uma sequência antiga.
			falhas = 0
		}
		if err != nil {
			falhas++
			g.marcarDegradado(ctx, id, err, falhas)
		}

		if motivo, estourou := g.abandonosEstouraram(id); estourou {
			g.autoDesabilitar(ctx, id, motivo)
			return
		}

		espera := g.backoff.Espera(falhas)
		if g.esperaConsentimento(id) {
			// Em sem_consentimento não há próxima tentativa agendada, e a tela
			// tem que dizer isso: o upstream não volta por tempo, volta porque o
			// admin clicou em "Autorizar".
			espera = -1
		}
		g.agendarProxima(id, espera)

		cfg, _ := g.config(id)
		select {
		case <-ctx.Done():
			return
		case <-g.esperaDeReconexao(id, max(espera, 0)):
		case <-g.pedidosDeConsentimento(cfg):
			// Consentimento novo agenda conectando na hora, sem esperar o
			// backoff pendente: sem isso o admin clica em autorizar e nada
			// acontece por alguns minutos, o que parece bug.
			falhas = 0
		}
	}
}

// esperaDeReconexao é o canal que libera a próxima tentativa.
//
// Nulo em sem_consentimento, e um canal nulo num select bloqueia para sempre —
// que é o comportamento certo. Insistir com backoff aqui seria um laço de 401
// contra o provedor que não resolve nada: o que falta é uma pessoa autorizando,
// não uma tentativa. Quem tira a supervisão da espera é o pedido de
// consentimento, ou o cancelamento do contexto.
func (g *Gerente) esperaDeReconexao(id int64, espera time.Duration) <-chan time.Time {
	if g.esperaConsentimento(id) {
		return nil
	}
	return g.relogio.Depois(espera)
}

// esperaConsentimento informa se o upstream está parado esperando um clique em
// "Autorizar".
func (g *Gerente) esperaConsentimento(id int64) bool {
	if g.oauth == nil {
		return false
	}
	return g.oauth.PrecisaConsentimento(id) && !g.oauth.ConsentimentoPedido(id)
}

// pedidosDeConsentimento é o canal em que a supervisão espera o clique do admin.
// Nulo para upstream que não usa OAuth.
func (g *Gerente) pedidosDeConsentimento(cfg Config) <-chan struct{} {
	if g.oauth == nil {
		return nil
	}
	return g.oauth.Pedidos(cfg)
}

// conectarEDescobrir abre a sessão, lista as ferramentas e só volta quando a
// sessão morre ou o contexto é cancelado.
//
// O primeiro retorno diz se o upstream chegou a pronto nesta tentativa. É o que
// separa "caiu depois de horas servindo" de "não conecta desde sempre", e sem
// essa distinção o backoff de um upstream estável ficaria no teto para sempre
// depois de um soluço.
func (g *Gerente) conectarEDescobrir(ctx context.Context, id int64) (chegouPronto bool, err error) {
	cfg, ok := g.config(id)
	if !ok {
		return false, fmt.Errorf("%w: id %d", ErrDesconhecido, id)
	}
	// O portão do OAuth vem antes de qualquer requisição, e é ele que elimina o
	// laço: um upstream sem consentimento nem tenta conectar. Sem o portão, cada
	// volta do backoff seria um 401 no provedor e — no caminho do registro
	// dinâmico — um cliente novo registrado lá, para nunca ser usado.
	if err := g.prepararOAuth(ctx, cfg); err != nil {
		return false, err
	}
	g.marcarConectando(id)

	sessao, processo, err := g.conectar(ctx, cfg)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := sessao.Close(); err != nil {
			g.log.Debug("erro ao fechar sessão de upstream", "upstream", cfg.Nome, "erro", err)
		}
		// Fechar a sessão fecha o stdin do processo, que é o adeus que a
		// especificação do transporte STDIO pede. Encerrar é o que vem depois:
		// a árvore inteira morre, inclusive o neto que sobreviveu ao pai. Isto
		// roda na goroutine de supervisão, então quando Aguardar volta não há
		// processo de upstream vivo — que é o critério da fatia.
		if processo != nil {
			processo.Encerrar()
		}
		g.esquecerSessao(id)
	}()

	if err := g.descobrir(ctx, id, cfg, sessao); err != nil {
		return false, err
	}

	// A sessão fica viva enquanto o transporte estiver de pé. Quando ele cai, o
	// Wait volta e o laço de supervisão tenta de novo com uma sessão nova.
	fim := make(chan error, 1)
	go func() { fim <- sessao.Wait() }()

	// Renovação proativa do token OAuth. Ela mora aqui, na supervisão, e não no
	// caminho da requisição do cliente: o transporte pede o token a cada
	// requisição de saída, e um token vencido nessa hora faria o tools/call do
	// cliente pagar a ida ao token endpoint. Nulo para upstream sem OAuth.
	renovar := g.relogioDeRenovacao(cfg)
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case err := <-fim:
			if err != nil {
				return true, fmt.Errorf("upstream %s: sessão encerrada: %w", cfg.Nome, err)
			}
			return true, fmt.Errorf("upstream %s: sessão encerrada pelo servidor", cfg.Nome)
		case <-renovar:
			//nolint:contextcheck // a renovação não pode herdar contexto de
			// requisição: o x/oauth2 guarda o contexto que recebe e o reusa em
			// todo refresh seguinte, e a gravação do token novo tem prazo
			// próprio para não ficar pela metade quando um cliente desiste.
			if err := g.renovarToken(cfg); err != nil {
				// Refresh recusado é a transição pronto → sem_consentimento da
				// seção 05: o consentimento acabou, e manter a sessão de pé só
				// adiaria o 401 para dentro da chamada de um cliente.
				return true, err
			}
			renovar = g.relogioDeRenovacao(cfg)
		}
	}
}

// relogioDeRenovacao é o tique da renovação proativa. Nulo quando o upstream não
// usa OAuth, e um canal nulo num select bloqueia para sempre.
func (g *Gerente) relogioDeRenovacao(cfg Config) <-chan time.Time {
	if g.oauth == nil || !cfg.UsaOAuth() {
		return nil
	}
	return g.relogio.Depois(g.tiqueRenovar)
}

// renovarToken renova o token do upstream quando ele está perto de vencer.
//
// Falha transitória não derruba a sessão: o access token em vigor continua
// valendo até vencer, e a próxima volta tenta de novo. Só a recusa definitiva —
// o invalid_grant que significa consentimento revogado ou expirado — sobe, e é
// ela que leva o upstream a sem_consentimento.
func (g *Gerente) renovarToken(cfg Config) error {
	err := g.oauth.Renovar(cfg.ID, g.margemRenovar)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrSemConsentimento) {
		return err
	}
	g.log.Warn("falha ao renovar token de upstream",
		"upstream", cfg.Nome, "upstream_id", cfg.ID, "erro", mensagemDeFalha(err))
	return nil
}

// conectar abre a sessão MCP com o watchdog armado.
//
// O Connect roda numa goroutine própria e é esperado com select: a issue #1189
// do go-sdk registra que ele bloqueia além do deadline contra um servidor que
// não responde, e Go não mata goroutine. Vencido o timer, o supervisor abandona
// a goroutine presa — vazamento deliberado, e é por isso que o abandono é
// contado, aparece na tela e leva à desabilitação automática no teto.
//
// O segundo retorno é o processo do upstream, quando o transporte é STDIO. Ele
// pertence a quem chamou: fechar a sessão fecha o stdin, e recolher a árvore é
// um passo à parte.
func (g *Gerente) conectar(ctx context.Context, cfg Config) (*mcp.ClientSession, *processoUpstream, error) {
	transporte, processo, err := g.transporteDe(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	cliente := mcp.NewClient(&mcp.Implementation{Name: "patchbay", Version: versao.Numero}, &mcp.ClientOptions{
		Logger: g.log.With("componente", "cliente_upstream", "upstream", cfg.Nome),
	})

	ctxConexao, cancelar := context.WithTimeout(ctx, g.prazoDeConexao(cfg))
	defer cancelar()

	type resultado struct {
		sessao *mcp.ClientSession
		err    error
	}
	pronto := make(chan resultado, 1)
	go func() {
		s, err := cliente.Connect(ctxConexao, transporte, nil)
		pronto <- resultado{sessao: s, err: err}
	}()

	select {
	case r := <-pronto:
		if r.err != nil {
			if processo != nil {
				processo.EncerrarAgora()
			}
			return nil, nil, fmt.Errorf("upstream %s: conectar: %w", cfg.Nome, r.err)
		}
		return r.sessao, processo, nil
	case <-ctxConexao.Done():
		// A goroutine acima fica para trás de propósito: é a mitigação da
		// issue #1189, e o resíduo tem que ser visível. No HTTP ela deixa duas
		// goroutines presas — esta e o coletor abaixo, que espera `<-pronto`
		// para fechar a sessão se ela chegar tarde —, e as duas só somem no
		// próximo boot.
		//
		// No STDIO o desfecho é melhor e o contador é o mesmo de propósito:
		// matar a árvore fecha o stdout do filho, o Connect preso volta com
		// erro e as duas goroutines terminam. O que continua valendo é o
		// diagnóstico — um upstream que pendura sempre passa do teto e se
		// desabiliza sozinho, com o motivo na tela, em vez de ficar reiniciando
		// um processo mudo para sempre.
		consecutivos, totais := g.contarAbandono(cfg.ID)
		g.log.Warn("connect de upstream abandonado por timeout",
			"upstream", cfg.Nome, "timeout", g.prazoDeConexao(cfg), "tipo", cfg.Tipo,
			"abandonos_consecutivos", consecutivos, "abandonos_totais", totais,
			"teto_abandonos", g.tetoAbandonos)
		if processo != nil {
			processo.EncerrarAgora()
		}
		// O SSE legado pode ter aberto o GET pendurado antes de cliente.Connect
		// travar na etapa seguinte (initialize) e nunca devolver a sessão — nesse
		// caso r.sessao abaixo nunca chega, e sem isto o stream ficaria aberto até
		// o processo reiniciar. HTTP e STDIO não implementam Abandonar e a
		// asserção simplesmente não bate, sem efeito para eles.
		if abandonavel, ok := transporte.(interface{ Abandonar() }); ok {
			abandonavel.Abandonar()
		}
		go func() {
			r := <-pronto
			if r.sessao != nil {
				_ = r.sessao.Close()
			}
		}()
		return nil, nil, fmt.Errorf("upstream %s: conectar: %w", cfg.Nome, ctxConexao.Err())
	}
}

// transporteDe monta o transporte do tipo configurado.
//
// É o único ponto do gerente que sabe que existe mais de um transporte: daqui
// para baixo a máquina de estados é a mesma para HTTP e para STDIO, e é isso que
// faz o backoff, o watchdog, o teto de abandonos e a tela valerem para os dois
// sem nenhum caso especial.
func (g *Gerente) transporteDe(ctx context.Context, cfg Config) (mcp.Transport, *processoUpstream, error) {
	switch cfg.Tipo {
	case TipoHTTP:
		// As credenciais estáticas são lidas do banco a cada conexão e entram no
		// transporte, nunca na URL nem na Config: Config alimenta a UI e o log.
		clienteHTTP, err := g.clienteDe(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		t := &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: clienteHTTP}
		if cfg.UsaOAuth() {
			// O Streamable HTTP do SDK já sabe pedir o token à fonte a cada
			// requisição e chamar Authorize num 401: é ele que conhece a
			// diferença entre 401, 403 com insufficient_scope e 403 comum, e
			// duplicar isso aqui seria reescrever pior.
			if t.OAuthHandler, err = g.autorizacaoDe(ctx, cfg); err != nil {
				return nil, nil, err
			}
		}
		return t, nil, nil
	case TipoSSE:
		// O SSE legado usa as mesmas credenciais do HTTP: do ponto de vista de
		// quem autentica, as duas coisas são requisição HTTP com Authorization.
		clienteHTTP, err := g.clienteDe(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		if cfg.UsaOAuth() {
			handler, err := g.autorizacaoDe(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			// mcp.SSEClientTransport não tem campo OAuthHandler, então o token e
			// o fluxo de autorização entram por RoundTripper (sse.go).
			clienteHTTP = comOAuthSSE(clienteHTTP, handler, cfg.Nome)
		}
		return &transporteSSE{
			base: &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: clienteHTTP},
		}, nil, nil
	case TipoSTDIO:
		return g.abrirProcesso(ctx, cfg)
	default:
		return nil, nil, fmt.Errorf("%w: %s", ErrTipoNaoSuportado, cfg.Tipo)
	}
}

// prepararOAuth garante que existe autorização utilizável antes de a supervisão
// gastar uma tentativa.
//
// Devolve ErrSemConsentimento quando o upstream depende de um clique do admin.
// Nada é enviado ao provedor nesse caso: reconectar só para tomar 401 não produz
// o consentimento que falta.
func (g *Gerente) prepararOAuth(ctx context.Context, cfg Config) error {
	if g.oauth == nil || !cfg.UsaOAuth() {
		return nil
	}
	ctxLeitura, cancelar := context.WithTimeout(ctx, cfg.Timeout)
	defer cancelar()
	return g.oauth.Preparar(ctxLeitura, cfg)
}

// autorizacaoDe devolve o handler de OAuth do upstream.
//
// Roda antes de abrir a sessão, na goroutine de supervisão: ele lê o cliente e a
// concessão do banco e pode registrar o cliente dinamicamente. Nada disso pode
// acontecer no caminho da requisição do cliente.
func (g *Gerente) autorizacaoDe(ctx context.Context, cfg Config) (auth.OAuthHandler, error) {
	if g.oauth == nil {
		// Falhar dizendo isso é melhor que conectar sem Authorization e receber
		// um 401 que a tela descreveria como problema do provedor.
		return nil, fmt.Errorf("upstream %s: modo oauth sem broker de OAuth configurado", cfg.Nome)
	}
	ctxLeitura, cancelar := context.WithTimeout(ctx, cfg.Timeout)
	defer cancelar()
	return g.oauth.Autorizacao(ctxLeitura, cfg)
}

// prazoDeConexao é o prazo do watchdog desta tentativa.
//
// Normalmente é o timeout do upstream. Quando há consentimento OAuth pedido pela
// UI, é o tempo de consentimento: a tentativa inclui esperar uma pessoa escolher
// uma conta e clicar em "permitir" no provedor, e cortá-la no timeout de operação
// transformaria todo consentimento num connect abandonado — cinco deles
// desabilitariam o upstream sozinho, com a mensagem errada na tela.
func (g *Gerente) prazoDeConexao(cfg Config) time.Duration {
	if g.oauth == nil || !cfg.UsaOAuth() || !g.oauth.ConsentimentoPedido(cfg.ID) {
		return cfg.Timeout
	}
	return g.oauth.tempoConsentimento + cfg.Timeout
}

// descobrir lê o tools/list e publica o snapshot.
func (g *Gerente) descobrir(ctx context.Context, id int64, cfg Config, sessao *mcp.ClientSession) error {
	ctxLista, cancelar := context.WithTimeout(ctx, cfg.Timeout)
	defer cancelar()

	res, err := sessao.ListTools(ctxLista, nil)
	if err != nil {
		return fmt.Errorf("upstream %s: listar ferramentas: %w", cfg.Nome, err)
	}

	g.mu.Lock()
	s, ok := g.servidores[id]
	if ok {
		s.estado = EstadoPronto
		s.ultimoErro = ""
		s.motivo = ""
		s.ferramentas = res.Tools
		s.sessao = sessao
		// Pronto zera falhas e abandonos consecutivos: a próxima queda começa a
		// curva do zero, e um timeout esporádico de connect não acumula por
		// semanas até desabilitar sozinho um upstream saudável. abandonosTotais
		// não zera aqui — só definir apaga o resíduo acumulado.
		s.falhas = 0
		s.abandonos = 0
		s.proximaEm = time.Time{}
	}
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: id %d", ErrDesconhecido, id)
	}

	g.log.Info("upstream pronto", "upstream", cfg.Nome, "ferramentas", len(res.Tools))
	g.notificarMudanca(ctx)
	return nil
}

// Chamar executa um tools/call no upstream, com timeout.
//
// É a única operação de upstream que o caminho da requisição do cliente
// dispara, e ela usa a sessão já aberta: não conecta, não descobre.
func (g *Gerente) Chamar(ctx context.Context, upstreamID int64, nome string, args json.RawMessage) (*mcp.CallToolResult, error) {
	g.mu.RLock()
	s, ok := g.servidores[upstreamID]
	var (
		sessao  *mcp.ClientSession
		timeout time.Duration
		upNome  string
	)
	if ok {
		sessao, timeout, upNome = s.sessao, s.cfg.Timeout, s.cfg.Nome
	}
	g.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrDesconhecido, upstreamID)
	}
	if sessao == nil {
		return nil, fmt.Errorf("%w: %s", ErrIndisponivel, upNome)
	}

	ctxChamada, cancelar := context.WithTimeout(ctx, timeout)
	defer cancelar()

	res, err := sessao.CallTool(ctxChamada, &mcp.CallToolParams{Name: nome, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("upstream %s: chamar %s: %w", upNome, nome, err)
	}
	return res, nil
}

// Ferramentas devolve o último tools/list bem-sucedido do upstream. Nunca fala
// com o upstream: é leitura de snapshot.
func (g *Gerente) Ferramentas(upstreamID int64) []*mcp.Tool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.servidores[upstreamID]
	if !ok {
		return nil
	}
	// Cópia da fatia para que quem materializa não veja o slice mudar embaixo.
	// Os *mcp.Tool são compartilhados, e o normalizador não os muta.
	out := make([]*mcp.Tool, len(s.ferramentas))
	copy(out, s.ferramentas)
	return out
}

// Situacoes devolve o retrato de todos os upstreams supervisionados, em ordem de
// nome.
//
// Ordem estável porque isto alimenta uma lista de tela: linha que muda de lugar
// a cada recarga é ruído que faz o admin perder o item que estava olhando.
func (g *Gerente) Situacoes() []Situacao {
	g.mu.RLock()
	out := make([]Situacao, 0, len(g.servidores))
	for _, s := range g.servidores {
		out = append(out, s.situacao())
	}
	g.mu.RUnlock()

	slices.SortFunc(out, func(a, b Situacao) int {
		if c := cmp.Compare(a.Config.Nome, b.Config.Nome); c != 0 {
			return c
		}
		return cmp.Compare(a.Config.ID, b.Config.ID)
	})
	return out
}

// TetoDeAbandonos devolve quantos connects abandonados a supervisão tolera. A
// tela mostra o número ao lado do contador, porque "3 abandonos" só significa
// alguma coisa ao lado do teto.
func (g *Gerente) TetoDeAbandonos() int { return g.tetoAbandonos }

// Situacao devolve o retrato de um upstream. O segundo retorno é falso quando o
// upstream não está sob supervisão — desabilitado, por exemplo.
func (g *Gerente) Situacao(upstreamID int64) (Situacao, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.servidores[upstreamID]
	if !ok {
		return Situacao{}, false
	}
	return s.situacao(), true
}

func (s *servidor) situacao() Situacao {
	return Situacao{
		Config:          s.cfg,
		Estado:          s.estado,
		UltimoErro:      s.ultimoErro,
		Ferramentas:     len(s.ferramentas),
		TentativaEm:     s.tentativaEm,
		ProximaEm:       s.proximaEm,
		Falhas:          s.falhas,
		Abandonos:       s.abandonos,
		AbandonosTotais: s.abandonosTotais,
		Motivo:          s.motivo,
	}
}

func (g *Gerente) config(id int64) (Config, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.servidores[id]
	if !ok {
		return Config{}, false
	}
	return s.cfg, true
}

// definir grava a configuração nova e devolve o estado ao começo da máquina.
//
// Zerar ferramentas e sessão faz parte: o snapshot antigo era de um transporte
// que já foi cancelado, e servir ferramenta de sessão morta é o que faz o cliente
// receber "unknown tool" sem saber por quê.
func (g *Gerente) definir(cfg Config) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.servidores[cfg.ID]
	if !ok {
		g.servidores[cfg.ID] = &servidor{cfg: cfg, estado: EstadoNovo}
		return
	}
	s.cfg = cfg
	s.estado = EstadoNovo
	s.ultimoErro = ""
	s.motivo = ""
	s.ferramentas = nil
	s.sessao = nil
	s.tentativaEm = time.Time{}
	s.proximaEm = time.Time{}
	s.falhas = 0
	// Os dois contadores de abandono zeram junto, e é isto que faz Aplicar
	// servir de botão de reconectar para um upstream que se desabilitou
	// sozinho. O resíduo de goroutines presas não some com o contador — ele só
	// some no próximo boot —, mas quem reaplica está dizendo que quer gastar
	// mais um teto e ver o total acumulado recomeçar do zero.
	s.abandonos = 0
	s.abandonosTotais = 0
}

// esquecer tira o upstream do mapa e devolve o nome que ele tinha, para o log.
func (g *Gerente) esquecer(id int64) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.servidores[id]
	if !ok {
		return ""
	}
	delete(g.servidores, id)
	return s.cfg.Nome
}

func (g *Gerente) marcarConectando(id int64) {
	agora := g.relogio.Agora()
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.servidores[id]; ok {
		s.estado = EstadoConectando
		s.tentativaEm = agora
		s.proximaEm = time.Time{}
	}
}

// marcarDegradado põe o upstream no estado de falha da tentativa que acabou.
//
// Falta de consentimento OAuth não é degradado: degradado significa "não consigo
// falar com ele", e aqui o patchbay fala perfeitamente — o provedor é que não
// autoriza. A distinção existe porque o efeito é outro: degradado volta pelo
// backoff, sem_consentimento volta por um clique do admin, e chamar os dois de
// degradado foi por que o gateway anterior fez a ferramenta parar de funcionar
// sem a causa aparecer em lugar nenhum.
func (g *Gerente) marcarDegradado(ctx context.Context, id int64, causa error, falhas int) {
	estado := EstadoDegradado
	if g.semConsentimento(id, causa) {
		estado = EstadoSemConsentimento
	}

	g.mu.Lock()
	s, ok := g.servidores[id]
	if ok {
		s.estado = estado
		s.ultimoErro = mensagemDeFalha(causa)
		s.falhas = falhas
		s.sessao = nil
		// As ferramentas saem do catálogo: ferramenta que não funciona custa
		// contexto no cliente sem entregar nada (seção 05).
		s.ferramentas = nil
	}
	nome := ""
	if ok {
		nome = s.cfg.Nome
	}
	g.mu.Unlock()

	if !ok {
		return
	}
	mensagem := mensagemDeFalha(causa)
	if estado == EstadoSemConsentimento {
		g.log.Warn("upstream sem consentimento OAuth",
			"upstream", nome, "erro", mensagem, "falhas", falhas)
	} else {
		g.log.Warn("upstream degradado", "upstream", nome, "erro", mensagem, "falhas", falhas)
	}
	g.notificarMudanca(ctx)
}

// semConsentimento decide se a falha desta tentativa é falta de autorização.
//
// Duas fontes, e as duas de propósito. O erro sentinela é a resposta direta, mas
// ele atravessa o embrulho do jsonrpc2 e do Connect do SDK, e um único %v no
// caminho o apagaria sem nenhum sintoma além de a tela dizer "degradado". O
// broker, que é quem sabe que o fetcher recusou por falta de consentimento, é a
// resposta que não depende de biblioteca de terceiro preservar %w.
func (g *Gerente) semConsentimento(id int64, causa error) bool {
	if errors.Is(causa, ErrSemConsentimento) {
		return true
	}
	return g.oauth != nil && g.oauth.PrecisaConsentimento(id)
}

// agendarProxima grava quando o backoff libera a tentativa seguinte. É o número
// que a tela mostra ao lado do motivo (seção 11).
func (g *Gerente) agendarProxima(id int64, espera time.Duration) {
	// Espera negativa é "nenhuma agendada": é o caso de sem_consentimento, em
	// que o upstream não volta por tempo. Um horário na tela ali seria mentira —
	// a tentativa que ele promete nunca acontece sozinha.
	proximaEm := time.Time{}
	if espera >= 0 {
		proximaEm = g.relogio.Agora().Add(espera)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.servidores[id]; ok {
		s.proximaEm = proximaEm
	}
}

// contarAbandono registra mais um connect deixado para trás. Devolve o
// consecutivo desde o último pronto (o que abandonosEstouraram compara contra
// o teto) e o total acumulado (o resíduo que a tela mostra, mesmo depois de o
// consecutivo zerar).
func (g *Gerente) contarAbandono(id int64) (consecutivos, totais int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.servidores[id]
	if !ok {
		return 0, 0
	}
	s.abandonos++
	s.abandonosTotais++
	return s.abandonos, s.abandonosTotais
}

// abandonosEstouraram informa se o upstream passou do teto de abandonos
// consecutivos e devolve o motivo escrito, que é o que vai para a tela.
func (g *Gerente) abandonosEstouraram(id int64) (motivo string, estourou bool) {
	if g.tetoAbandonos <= 0 {
		return "", false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.servidores[id]
	if !ok || s.abandonos < g.tetoAbandonos {
		return "", false
	}
	return fmt.Sprintf(
		"%d connects consecutivos abandonados por timeout, teto de %d: a supervisão parou sozinha para não acumular goroutine e socket presos. Reconecte quando o servidor voltar a responder.",
		s.abandonos, g.tetoAbandonos), true
}

// autoDesabilitar desliga a supervisão de um upstream que pendura sempre.
//
// Nada disso vai ao banco: habilitado continua sendo a intenção do admin, e o
// próximo boot recomeça em novo → conectando. O que muda é só o estado em
// memória e o motivo na tela — upstream que se desabilita em silêncio é
// indistinguível de upstream que alguém apagou (seção 05).
func (g *Gerente) autoDesabilitar(ctx context.Context, id int64, motivo string) {
	g.mu.Lock()
	s, ok := g.servidores[id]
	nome := ""
	consecutivos, totais := 0, 0
	if ok {
		s.estado = EstadoDesabilitado
		s.motivo = motivo
		s.sessao = nil
		s.ferramentas = nil
		s.proximaEm = time.Time{}
		nome, consecutivos, totais = s.cfg.Nome, s.abandonos, s.abandonosTotais
	}
	g.mu.Unlock()

	if !ok {
		return
	}
	g.log.Error("upstream desabilitado por autoproteção",
		"upstream", nome, "upstream_id", id,
		"abandonos_consecutivos", consecutivos, "abandonos_totais", totais, "teto_abandonos", g.tetoAbandonos)
	g.notificarMudanca(ctx)
}

func (g *Gerente) esquecerSessao(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.servidores[id]; ok {
		s.sessao = nil
	}
}

func (g *Gerente) notificarMudanca(ctx context.Context) {
	if g.aoMudar == nil {
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	g.aoMudar(ctx)
}
