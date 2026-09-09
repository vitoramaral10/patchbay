package upstream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// relogioFalso é o relógio da supervisão sob controle do teste.
//
// Sem ele, provar "o backoff cresce, com jitter, até o teto" custaria dezenas de
// segundos de espera de verdade — e o teste passaria a medir a máquina em vez do
// código.
type relogioFalso struct {
	mu        sync.Mutex
	agora     time.Time
	pendentes []chan time.Time

	// pedidos entrega ao teste cada espera pedida, para ele saber que a
	// supervisão chegou ao backoff sem consultar o relógio de parede.
	pedidos chan time.Duration
}

func novoRelogioFalso() *relogioFalso {
	return &relogioFalso{
		agora:   time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		pedidos: make(chan time.Duration, 64),
	}
}

func (r *relogioFalso) Agora() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agora
}

func (r *relogioFalso) Depois(d time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	r.mu.Lock()
	r.pendentes = append(r.pendentes, c)
	r.mu.Unlock()

	// Sem bloquear: se o teste já terminou de ler, a supervisão não pode ficar
	// presa aqui e travar o desligamento.
	select {
	case r.pedidos <- d:
	default:
	}
	return c
}

// esperarPedido devolve a próxima espera pedida pela supervisão.
func (r *relogioFalso) esperarPedido(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-r.pedidos:
		return d
	case <-time.After(15 * time.Second):
		t.Fatal("a supervisão não pediu backoff em 15s")
		return 0
	}
}

// liberar avança o relógio e solta todas as esperas pendentes.
func (r *relogioFalso) liberar(d time.Duration) {
	r.mu.Lock()
	r.agora = r.agora.Add(d)
	pendentes, agora := r.pendentes, r.agora
	r.pendentes = nil
	r.mu.Unlock()

	for _, c := range pendentes {
		c <- agora
	}
}

// transporteQuePendura ignora o contexto da requisição, que é exatamente o que a
// issue #1189 do go-sdk descreve: o Connect bloqueia além do deadline contra um
// servidor que não responde.
//
// Um httptest que dorme não serve para provar o watchdog: nele o cliente HTTP
// respeita o contexto e a corrida entre "erro do Connect" e "timer venceu" é
// decidida no sorteio do select. Aqui só existe uma saída possível — o timer —,
// e o abandono da goroutine é determinístico.
type transporteQuePendura struct {
	solto chan struct{}
	// presas conta quantas requisições ficaram penduradas: é o resíduo que o
	// teto de abandonos existe para limitar.
	presas chan struct{}
}

func (t *transporteQuePendura) RoundTrip(*http.Request) (*http.Response, error) {
	select {
	case t.presas <- struct{}{}:
	default:
	}
	<-t.solto
	return nil, errors.New("transporte de teste solto")
}

func novoTransporteQuePendura(t *testing.T) *transporteQuePendura {
	t.Helper()
	tp := &transporteQuePendura{
		solto:  make(chan struct{}),
		presas: make(chan struct{}, 64),
	}
	// Soltar no fim é o que faz as goroutines abandonadas terminarem antes de o
	// teste acabar; em produção elas só somem no próximo boot.
	t.Cleanup(func() { close(tp.solto) })
	return tp
}

// TestGerente_BackoffCresceComJitterEParaNoTetoDeAbandonos é a fatia 3 inteira
// no caminho do upstream pendurado: o watchdog abandona o Connect preso, o
// backoff cresce com jitter até o teto, a próxima tentativa fica visível, e a
// supervisão se desliga sozinha quando o resíduo de goroutines passa do limite.
func TestGerente_BackoffCresceComJitterEParaNoTetoDeAbandonos(t *testing.T) {
	t.Parallel()

	const teto = 5
	tp := novoTransporteQuePendura(t)
	rel := novoRelogioFalso()

	cfg := upstream.Config{
		ID: 1, Nome: "que-pendura", Tipo: upstream.TipoHTTP,
		URL: "http://upstream.invalido/mcp", Timeout: 30 * time.Millisecond,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComClienteHTTP(&http.Client{Transport: tp}),
		upstream.ComRelogio(rel),
		upstream.ComTetoDeAbandonos(teto),
		upstream.ComBackoff(upstream.Backoff{
			Base: time.Second, Teto: 4 * time.Second, Fator: 2, Jitter: 0.5,
			// Sorteio no piso: com a fonte de jitter fixa, a espera é uma conta
			// e não uma faixa.
			Sorteio: func() float64 { return 0 },
		}),
	)

	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	// Intervalos cheios: 1s, 2s, 4s, 4s (teto). Com jitter no piso, metade de
	// cada um. A quinta falha estoura o teto de abandonos e não chega a esperar.
	quer := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		2 * time.Second,
	}
	for i, esperado := range quer {
		if got := rel.esperarPedido(t); got != esperado {
			t.Fatalf("espera %d = %v, quer %v", i+1, got, esperado)
		}

		s, ok := sut.Situacao(1)
		if !ok {
			t.Fatalf("upstream saiu da supervisão na tentativa %d", i+1)
		}
		if s.Estado != upstream.EstadoDegradado {
			t.Errorf("estado na tentativa %d = %q, quer %q", i+1, s.Estado, upstream.EstadoDegradado)
		}
		if s.Falhas != i+1 {
			t.Errorf("falhas na tentativa %d = %d, quer %d", i+1, s.Falhas, i+1)
		}
		if s.ProximaEm.IsZero() {
			t.Errorf("próxima tentativa na tentativa %d = zero, quer o instante agendado", i+1)
		}
		if quero := rel.Agora().Add(esperado); !s.ProximaEm.Equal(quero) {
			t.Errorf("próxima tentativa = %v, quer %v", s.ProximaEm, quero)
		}

		rel.liberar(esperado)
	}

	// Quinta falha: o teto estoura e a supervisão se desliga.
	esperarEstado(t, sut, 1, upstream.EstadoDesabilitado)

	s, _ := sut.Situacao(1)
	if s.Abandonos != teto {
		t.Errorf("abandonos = %d, quer %d", s.Abandonos, teto)
	}
	if s.Motivo == "" {
		t.Error("motivo vazio; upstream que se desabilita em silêncio é indistinguível de upstream apagado")
	}
	if !s.ProximaEm.IsZero() {
		t.Error("próxima tentativa agendada num upstream que parou de tentar")
	}
	if _, err := sut.Chamar(context.Background(), 1, "qualquer", nil); !errors.Is(err, upstream.ErrIndisponivel) {
		t.Errorf("erro de Chamar = %v, quer %v", err, upstream.ErrIndisponivel)
	}
}

// TestGerente_ReconectarRearmaUpstreamDesabilitado: Aplicar é o botão de
// reconectar. Sem ele, o upstream que se desligou sozinho só voltaria com um
// boot — e o admin não teria como agir sobre o que a tela está mostrando.
func TestGerente_ReconectarRearmaUpstreamDesabilitado(t *testing.T) {
	t.Parallel()

	tp := novoTransporteQuePendura(t)
	rel := novoRelogioFalso()

	cfg := upstream.Config{
		ID: 1, Nome: "que-pendura", Tipo: upstream.TipoHTTP,
		URL: "http://upstream.invalido/mcp", Timeout: 30 * time.Millisecond,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComClienteHTTP(&http.Client{Transport: tp}),
		upstream.ComRelogio(rel),
		upstream.ComTetoDeAbandonos(1),
		upstream.ComBackoff(upstream.BackoffFixo(time.Second)),
	)

	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	esperarEstado(t, sut, 1, upstream.EstadoDesabilitado)

	if err := sut.Aplicar(context.Background(), cfg); err != nil {
		t.Fatalf("Aplicar: erro = %v, quer nil", err)
	}
	s, ok := sut.Situacao(1)
	if !ok {
		t.Fatal("upstream saiu da supervisão depois de reconectar")
	}
	if s.Estado == upstream.EstadoDesabilitado {
		t.Errorf("estado = %q, quer a máquina de volta ao começo", s.Estado)
	}
	if s.Motivo != "" {
		t.Errorf("motivo = %q, quer vazio depois de reconectar", s.Motivo)
	}
}

// TestGerente_NenhumEstadoDeErroVaiAoBanco é a trava da decisão que separa o
// patchbay do MetaMCP: por mais que a máquina caia, o que está gravado continua
// sendo só a intenção do admin, e o próximo boot recomeça em novo → conectando.
func TestGerente_NenhumEstadoDeErroVaiAoBanco(t *testing.T) {
	t.Parallel()

	repo, st := repositorioDeTeste(t)
	form := formBase("que-pendura")
	form.URL = "http://upstream.invalido/mcp"
	id, err := repo.Criar(context.Background(), form)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	tp := novoTransporteQuePendura(t)
	cfgs, err := upstream.Habilitados(context.Background(), st.Leitura())
	if err != nil {
		t.Fatalf("Habilitados: erro = %v, quer nil", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("upstreams habilitados = %d, quer 1", len(cfgs))
	}
	cfgs[0].Timeout = 30 * time.Millisecond

	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), cfgs,
		upstream.ComClienteHTTP(&http.Client{Transport: tp}),
		upstream.ComTetoDeAbandonos(1),
		upstream.ComIntervaloTentativa(time.Millisecond),
	)
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	esperarEstado(t, sut, id, upstream.EstadoDesabilitado)

	reg, err := repo.Obter(context.Background(), id)
	if err != nil {
		t.Fatalf("Obter: erro = %v, quer nil", err)
	}
	if !reg.Habilitado {
		t.Error("habilitado = false; a autoproteção gravou a decisão dela no banco")
	}
	if reg.UltimoErro != "" {
		t.Errorf("ultimo_erro gravado = %q, quer vazio; erro é memória, não banco", reg.UltimoErro)
	}

	// Um boot novo lê o banco e recomeça a máquina do começo.
	depois, err := upstream.Habilitados(context.Background(), st.Leitura())
	if err != nil {
		t.Fatalf("Habilitados no boot seguinte: erro = %v, quer nil", err)
	}
	if len(depois) != 1 {
		t.Fatalf("upstreams habilitados no boot seguinte = %d, quer 1", len(depois))
	}
}

// esperarEstado espera o upstream chegar ao estado pedido, por sinal e não por
// relógio: consulta a situação a cada mudança observada, com um teto de espera.
func esperarEstado(t *testing.T, g *upstream.Gerente, id int64, quer upstream.Estado) {
	t.Helper()

	limite := time.After(15 * time.Second)
	tique := time.NewTicker(5 * time.Millisecond)
	defer tique.Stop()
	for {
		if s, ok := g.Situacao(id); ok && s.Estado == quer {
			return
		}
		select {
		case <-tique.C:
		case <-limite:
			s, _ := g.Situacao(id)
			t.Fatalf("upstream %d não chegou a %q em 15s (estado = %q, erro = %q)",
				id, quer, s.Estado, s.UltimoErro)
		}
	}
}

// esperarSairDe espera o upstream sair do estado pedido, com um limite de
// espera próprio em vez do fixo de esperarEstado: derrubar uma sessão pronta
// custa mais que uma tentativa de conexão falhar, e quem chama sabe quanto.
func esperarSairDe(t *testing.T, g *upstream.Gerente, id int64, de upstream.Estado, limiteEspera time.Duration) {
	t.Helper()

	limite := time.After(limiteEspera)
	tique := time.NewTicker(20 * time.Millisecond)
	defer tique.Stop()
	for {
		if s, ok := g.Situacao(id); ok && s.Estado != de {
			return
		}
		select {
		case <-tique.C:
		case <-limite:
			s, _ := g.Situacao(id)
			t.Fatalf("upstream %d continuava em %q depois de %v (estado = %q, erro = %q)",
				id, de, limiteEspera, s.Estado, s.UltimoErro)
		}
	}
}

// esperarAbandonos espera o consecutivo de abandonos chegar ao valor pedido
// (ou o upstream se desabilitar, o que vier primeiro), por sinal e não por
// relógio.
func esperarAbandonos(t *testing.T, g *upstream.Gerente, id int64, quer int) {
	t.Helper()

	limite := time.After(15 * time.Second)
	tique := time.NewTicker(5 * time.Millisecond)
	defer tique.Stop()
	for {
		if s, ok := g.Situacao(id); ok && (s.Abandonos >= quer || s.Estado == upstream.EstadoDesabilitado) {
			return
		}
		select {
		case <-tique.C:
		case <-limite:
			s, _ := g.Situacao(id)
			t.Fatalf("upstream %d não chegou a %d abandonos consecutivos em 15s (abandonos = %d, estado = %q)",
				id, quer, s.Abandonos, s.Estado)
		}
	}
}

// modoTransporte é o que transporteControlavel faz com a próxima requisição.
type modoTransporte int

const (
	// modoPassar encaminha de verdade para um servidor MCP funcional: é como o
	// upstream chega a pronto.
	modoPassar modoTransporte = iota
	// modoPendurar bloqueia até o contexto da requisição vencer, sem nunca
	// responder — a issue #1189 do go-sdk, e o que produz um abandono do
	// watchdog.
	modoPendurar
	// modoSessaoSumiu responde 404 a tudo, que no Streamable HTTP é "a sessão
	// não existe mais" (spec §2.5.3). É o que derruba na hora uma sessão já
	// pronta, sem passar pelo backoff do reconector.
	modoSessaoSumiu
)

// transporteControlavel alterna, sob comando do teste, entre as três respostas
// acima. É o que permite provar, num só upstream e sem mock de protocolo, que
// chegar a pronto zera o consecutivo de abandonos sem zerar o total
// acumulado: primeiro conecta de verdade a um servidor MCP real do go-sdk,
// depois pendura o watchdog, depois falha a sessão já pronta.
type transporteControlavel struct {
	mu   sync.Mutex
	modo modoTransporte
	real http.RoundTripper
}

func (t *transporteControlavel) definir(m modoTransporte) {
	t.mu.Lock()
	t.modo = m
	t.mu.Unlock()
}

func (t *transporteControlavel) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	modo := t.modo
	t.mu.Unlock()

	switch modo {
	case modoPendurar:
		<-req.Context().Done()
		return nil, req.Context().Err()
	case modoSessaoSumiu:
		// Uma resposta, e não um erro de transporte, é o que faz a diferença de
		// tempo. Erro de rede o cliente do go-sdk trata como falha transitória
		// do stream SSE autônomo e tenta reconectar 5 vezes com backoff próprio
		// (mcp/streamable.go:2680-2712): 1s, 1,5s, 2,25s, 3,375s e 5,06s, cada
		// uma com jitter cheio que pode dobrá-la — 13s no melhor caso e 26s no
		// pior, sorteados. Já um 404 vira ErrSessionMissing no checkResponse
		// (mcp/streamable.go:2534-2538) e derruba a sessão na primeira volta.
		// Para o que este teste afirma — chegar a pronto zera o consecutivo de
		// abandonos — tanto faz como a sessão morre; o que não pode é o teste
		// medir o sorteio do backoff do SDK contra um orçamento fixo.
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	default:
		return t.real.RoundTrip(req)
	}
}

// servidorMCPDeTeste sobe um servidor MCP de verdade do go-sdk, do jeito que
// hotapply_test.go já faz, mas devolvendo o *httptest.Server: este teste
// precisa de CloseClientConnections para derrubar uma sessão já pronta.
func servidorMCPDeTeste(t *testing.T) *httptest.Server {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "intermitente", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "eco", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts
}

// TestGerente_ProntoZeraAbandonosConsecutivos é o teste do item 1: só o
// consecutivo de abandonos desde o último pronto conta para o teto, não o
// total acumulado. Ele alterna abandono e pronto no mesmo upstream — a
// sequência "abandono, ..., pronto, abandono, ..." da revisão — e prova que o
// total pode passar do teto sem nunca desabilitar, e que só uma sequência sem
// nenhum pronto no meio desabilita ao chegar no teto (subteste abaixo).
//
// A metade que derruba uma sessão já pronta responde 404 em vez de erro de rede
// de propósito: o porquê está em modoSessaoSumiu, e é a diferença entre o teste
// levar um segundo e levar um sorteio de 13 a 26.
func TestGerente_ProntoZeraAbandonosConsecutivos(t *testing.T) {
	t.Parallel()

	const teto = 3
	const grupo = teto - 1 // fica abaixo do teto nos dois grupos, mas a soma (4) passa dele.

	ts := servidorMCPDeTeste(t)
	tp := &transporteControlavel{real: http.DefaultTransport, modo: modoPendurar}
	cfg := upstream.Config{
		ID: 1, Nome: "intermitente", Tipo: upstream.TipoHTTP,
		URL: ts.URL, Timeout: 30 * time.Millisecond,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComClienteHTTP(&http.Client{Transport: tp}),
		upstream.ComTetoDeAbandonos(teto),
		upstream.ComIntervaloTentativa(150*time.Millisecond),
	)
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		tp.definir(modoPassar)
		cancelar()
		sut.Aguardar()
	})

	// Primeiro grupo: abandonos consecutivos, sem chegar no teto.
	esperarAbandonos(t, sut, 1, grupo)
	if s, _ := sut.Situacao(1); s.Estado == upstream.EstadoDesabilitado {
		t.Fatalf("desabilitou com %d abandonos, antes do teto de %d", grupo, teto)
	}

	// Chega a pronto: reseta o consecutivo, sem tocar no total acumulado.
	tp.definir(modoPassar)
	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	if s, _ := sut.Situacao(1); s.Abandonos != 0 {
		t.Fatalf("abandonos consecutivos = %d, quer 0 depois de chegar a pronto", s.Abandonos)
	}

	// Derruba a sessão pronta e volta a pendurar o watchdog para o segundo
	// grupo de abandonos.
	tp.definir(modoSessaoSumiu)
	ts.CloseClientConnections()
	esperarSairDe(t, sut, 1, upstream.EstadoPronto, 10*time.Second)
	tp.definir(modoPendurar)

	// Segundo grupo: mais abandonos consecutivos. Sem o reset do item 1, o
	// total (2*grupo = 4, para teto=3) já teria passado do teto no meio deste
	// grupo.
	esperarAbandonos(t, sut, 1, grupo)

	s, ok := sut.Situacao(1)
	if !ok {
		t.Fatal("upstream saiu da supervisão")
	}
	if s.Estado == upstream.EstadoDesabilitado {
		t.Errorf("desabilitou com %d abandonos consecutivos, quer só no teto de %d", grupo, teto)
	}
	if s.Abandonos != grupo {
		t.Errorf("abandonos consecutivos = %d, quer %d", s.Abandonos, grupo)
	}
	if quer := 2 * grupo; s.AbandonosTotais != quer {
		t.Errorf("abandonos totais = %d, quer %d (a soma dos dois grupos)", s.AbandonosTotais, quer)
	}
}

// TestGerente_AbandonosConsecutivosDesabilitam é o outro lado do item 1: sem
// nenhum pronto no meio, o consecutivo é o total, e chegar no teto desabilita
// — o mesmo teto que o teste acima mostra não disparar quando há reset.
func TestGerente_AbandonosConsecutivosDesabilitam(t *testing.T) {
	t.Parallel()

	const teto = 3
	tp := novoTransporteQuePendura(t)
	cfg := upstream.Config{
		ID: 1, Nome: "que-pendura-sempre", Tipo: upstream.TipoHTTP,
		URL: "http://upstream.invalido/mcp", Timeout: 20 * time.Millisecond,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComClienteHTTP(&http.Client{Transport: tp}),
		upstream.ComTetoDeAbandonos(teto),
		upstream.ComIntervaloTentativa(time.Millisecond),
	)
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	esperarEstado(t, sut, 1, upstream.EstadoDesabilitado)

	s, _ := sut.Situacao(1)
	if s.Abandonos != teto {
		t.Errorf("abandonos consecutivos = %d, quer %d", s.Abandonos, teto)
	}
	if s.AbandonosTotais != teto {
		t.Errorf("abandonos totais = %d, quer %d", s.AbandonosTotais, teto)
	}
}
