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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/versao"
)

// IntervaloTentativaPadrao é a espera entre tentativas de conexão.
//
// Constante de propósito: o backoff exponencial com jitter e teto é da fatia 3,
// e um backoff pela metade seria pior que nenhum — esconderia o problema sem
// resolver a frequência de tentativa.
const IntervaloTentativaPadrao = 5 * time.Second

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
	log       *slog.Logger
	cliente   *http.Client
	intervalo time.Duration
	aoMudar   func(context.Context)

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
	ferramentas []*mcp.Tool
	sessao      *mcp.ClientSession
	tentativaEm time.Time
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

// ComIntervaloTentativa troca a espera entre tentativas de conexão.
func ComIntervaloTentativa(d time.Duration) Opcao {
	return func(g *Gerente) { g.intervalo = d }
}

// ComClienteHTTP troca o cliente HTTP usado nos upstreams HTTP.
func ComClienteHTTP(c *http.Client) Opcao {
	return func(g *Gerente) { g.cliente = c }
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
		log:        log,
		cliente:    &http.Client{},
		intervalo:  IntervaloTentativaPadrao,
		comandos:   make(chan comando),
		encerrado:  make(chan struct{}),
		servidores: make(map[int64]*servidor, len(cfgs)),
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
	nome := g.esquecer(c.removerID)
	if nome != "" {
		g.log.Info("upstream fora da supervisão", "upstream", nome, "upstream_id", c.removerID)
	}
	g.notificarMudanca(ctx)
	return nil
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
func (g *Gerente) supervisionar(ctx context.Context, id int64) {
	for {
		if err := g.conectarEDescobrir(ctx, id); err != nil {
			if ctx.Err() != nil {
				return
			}
			g.marcarDegradado(ctx, id, err)
		}

		// Voltar de degradado passa obrigatoriamente por uma sessão nova: a
		// conexão pode estar envenenada por um erro transitório (issue #683 do
		// go-sdk) e reaproveitar o transporte reintroduz o bug.
		select {
		case <-ctx.Done():
			return
		case <-time.After(g.intervalo):
		}
	}
}

// conectarEDescobrir abre a sessão, lista as ferramentas e só volta quando a
// sessão morre ou o contexto é cancelado.
func (g *Gerente) conectarEDescobrir(ctx context.Context, id int64) error {
	cfg, ok := g.config(id)
	if !ok {
		return fmt.Errorf("%w: id %d", ErrDesconhecido, id)
	}
	g.marcarConectando(id)

	sessao, err := g.conectar(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := sessao.Close(); err != nil {
			g.log.Debug("erro ao fechar sessão de upstream", "upstream", cfg.Nome, "erro", err)
		}
		g.esquecerSessao(id)
	}()

	if err := g.descobrir(ctx, id, cfg, sessao); err != nil {
		return err
	}

	// A sessão fica viva enquanto o transporte estiver de pé. Quando ele cai, o
	// Wait volta e o laço de supervisão tenta de novo com uma sessão nova.
	fim := make(chan error, 1)
	go func() { fim <- sessao.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-fim:
		if err != nil {
			return fmt.Errorf("upstream %s: sessão encerrada: %w", cfg.Nome, err)
		}
		return fmt.Errorf("upstream %s: sessão encerrada pelo servidor", cfg.Nome)
	}
}

// conectar abre a sessão MCP com timeout.
//
// O Connect roda numa goroutine própria e é esperado com select: a issue #1189
// do go-sdk registra que ele bloqueia além do deadline contra um servidor que
// não responde, e Go não mata goroutine. O contador de connects abandonados e a
// desabilitação automática são da fatia 3; aqui a goroutine é abandonada e o
// fato aparece no log.
func (g *Gerente) conectar(ctx context.Context, cfg Config) (*mcp.ClientSession, error) {
	if cfg.Tipo != TipoHTTP {
		return nil, fmt.Errorf("%w: %s", ErrTipoNaoSuportado, cfg.Tipo)
	}

	cliente := mcp.NewClient(&mcp.Implementation{Name: "patchbay", Version: versao.Numero}, &mcp.ClientOptions{
		Logger: g.log.With("componente", "cliente_upstream", "upstream", cfg.Nome),
	})
	transporte := &mcp.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: g.cliente,
	}

	ctxConexao, cancelar := context.WithTimeout(ctx, cfg.Timeout)
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
			return nil, fmt.Errorf("upstream %s: conectar: %w", cfg.Nome, r.err)
		}
		return r.sessao, nil
	case <-ctxConexao.Done():
		// A goroutine acima fica para trás de propósito: é a mitigação da
		// issue #1189, e o resíduo tem que ser visível.
		g.log.Warn("connect de upstream abandonado por timeout",
			"upstream", cfg.Nome, "timeout", cfg.Timeout)
		go func() {
			r := <-pronto
			if r.sessao != nil {
				_ = r.sessao.Close()
			}
		}()
		return nil, fmt.Errorf("upstream %s: conectar: %w", cfg.Nome, ctxConexao.Err())
	}
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
		s.ferramentas = res.Tools
		s.sessao = sessao
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
		Config:      s.cfg,
		Estado:      s.estado,
		UltimoErro:  s.ultimoErro,
		Ferramentas: len(s.ferramentas),
		TentativaEm: s.tentativaEm,
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
	s.ferramentas = nil
	s.sessao = nil
	s.tentativaEm = time.Time{}
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
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.servidores[id]; ok {
		s.estado = EstadoConectando
		s.tentativaEm = time.Now()
	}
}

func (g *Gerente) marcarDegradado(ctx context.Context, id int64, causa error) {
	g.mu.Lock()
	s, ok := g.servidores[id]
	if ok {
		s.estado = EstadoDegradado
		s.ultimoErro = causa.Error()
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
	g.log.Warn("upstream degradado", "upstream", nome, "erro", causa)
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
