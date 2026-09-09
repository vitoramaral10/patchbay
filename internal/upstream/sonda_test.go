package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// modoDaFerramenta é o que a ferramenta sondada faz na próxima chamada.
type modoDaFerramenta int

const (
	// ferramentaOK responde com o texto combinado.
	ferramentaOK modoDaFerramenta = iota
	// ferramentaComIsError responde 200 com isError, que é o modo de falha que
	// só uma sonda funcional detecta: tools/list continua perfeito.
	ferramentaComIsError
	// ferramentaPendura só volta quando o teste soltar — é o que faz a sondagem
	// estourar o próprio prazo.
	ferramentaPendura
	// ferramentaOutroTexto responde sem o trecho esperado.
	ferramentaOutroTexto
	// ferramentaComSegredo ecoa uma credencial na resposta — o caso que prova
	// que a evidência da sonda passa por redação antes de chegar à tela.
	ferramentaComSegredo
)

// upstreamSondavel é um servidor MCP de verdade do go-sdk cuja ferramenta muda
// de comportamento sob comando do teste.
//
// O "falso" é o comportamento da ferramenta, nunca o protocolo: a sonda existe
// justamente para separar "o servidor conversa" de "a chamada funciona", e um
// mock de protocolo escrito à mão concordaria com o meu entendimento do MCP em
// vez de exercitá-lo.
type upstreamSondavel struct {
	url string
	ts  *httptest.Server
	// chamadas conta os tools/call que chegaram na ferramenta sondada. É o que
	// prova que a sonda desligada não chama nada.
	chamadas atomic.Int64
	modo     atomic.Int64
	// solto libera a ferramenta que pendura, no fim do teste.
	solto     chan struct{}
	fecharUma sync.Once
}

func (u *upstreamSondavel) definir(m modoDaFerramenta) { u.modo.Store(int64(m)) }

func (u *upstreamSondavel) liberar() {
	u.fecharUma.Do(func() { close(u.solto) })
}

// derrubarConexoes fecha as conexões em curso sem derrubar o listener — o
// mesmo CloseClientConnections que resiliencia_test.go usa para derrubar uma
// sessão HTTP já pronta. Só isto não basta contra um transporte que ainda
// aceita RoundTrip; é por isso que quem chama também troca o transporte para
// modoErro antes.
func (u *upstreamSondavel) derrubarConexoes() { u.ts.CloseClientConnections() }

const textoDaSonda = "pong"

func novoUpstreamSondavel(t *testing.T) *upstreamSondavel {
	t.Helper()

	u := &upstreamSondavel{solto: make(chan struct{})}
	srv := mcp.NewServer(&mcp.Implementation{Name: "sondavel", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			u.chamadas.Add(1)
			switch modoDaFerramenta(u.modo.Load()) {
			case ferramentaComIsError:
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "cota da api esgotada"}},
				}, nil
			case ferramentaPendura:
				select {
				case <-u.solto:
				case <-ctx.Done():
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "tarde demais"}},
				}, nil
			case ferramentaOutroTexto:
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "outra coisa"}},
				}, nil
			case ferramentaComSegredo:
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "autorização recusada: Bearer abc123def456"}},
				}, nil
			default:
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: textoDaSonda}},
				}, nil
			}
		})
	// Uma segunda ferramenta que nunca muda: é por ela que o teste prova que a
	// sessão continua de pé depois de a sondagem falhar.
	srv.AddTool(
		&mcp.Tool{Name: "eco", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "eco"}}}, nil
		})

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(func() {
		// Soltar antes de fechar: uma ferramenta pendurada seguraria o Close,
		// que espera pelas requisições em curso.
		u.liberar()
		ts.CloseClientConnections()
		ts.Close()
	})
	u.url = ts.URL
	u.ts = ts
	return u
}

// sondaDeTeste é a configuração usada nos testes: intervalo alto o bastante para
// nenhuma sondagem periódica disparar sozinha, porque quem dispara aqui é o
// connect e o "Sondar agora".
func sondaDeTeste() upstream.Sonda {
	return upstream.Sonda{
		Habilitada: true,
		Ferramenta: "ping",
		Intervalo:  time.Hour,
		Timeout:    2 * time.Second,
		Tolerancia: 2,
	}
}

// observadorDeSonda guarda as sondagens observadas, para o teste da trilha.
type observadorDeSonda struct {
	mu     sync.Mutex
	vistas []upstream.Sondagem
}

func (o *observadorDeSonda) ObservarSonda(s upstream.Sondagem) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.vistas = append(o.vistas, s)
}

func (o *observadorDeSonda) todas() []upstream.Sondagem {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]upstream.Sondagem, len(o.vistas))
	copy(out, o.vistas)
	return out
}

// gerenteSondavel sobe um gerente com um upstream apontado para u, já pronto.
func gerenteSondavel(
	t *testing.T, u *upstreamSondavel, sonda upstream.Sonda, opcoes ...upstream.Opcao,
) (*upstream.Gerente, <-chan struct{}) {
	t.Helper()

	cfg := upstream.Config{
		ID: 1, Nome: "sondavel", Tipo: upstream.TipoHTTP,
		URL: u.url, Timeout: 5 * time.Second, Sonda: sonda,
	}
	mudou := make(chan struct{}, 64)
	opcoes = append([]upstream.Opcao{
		upstream.ComIntervaloTentativa(20 * time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
	}, opcoes...)

	g := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg}, opcoes...)
	ctx, cancelar := context.WithCancel(context.Background())
	g.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		g.Aguardar()
	})
	return g, mudou
}

// sondarAgora dispara uma sondagem manual e falha o teste se ela nem pôde
// acontecer. O desfecho da sondagem em si é o valor devolvido.
func sondarAgora(t *testing.T, g *upstream.Gerente, id int64) upstream.ResultadoSonda {
	t.Helper()

	ctx, cancelar := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelar()

	r, err := g.Sondar(ctx, id)
	if err != nil {
		t.Fatalf("Sondar: erro = %v, quer nil", err)
	}
	return r
}

// TestGerente_SondaDesligadaNaoChamaNada é o opt-in lido do lado do servidor:
// nenhum tools/call sai enquanto ninguém ligou a sonda, e "Sondar agora"
// responde que ela está desligada em vez de chamar assim mesmo.
//
// É a defesa do resíduo nomeado na seção 13: o patchbay não tem como saber que
// send_message manda mensagem para alguém, então ele não chama nada por conta
// própria.
func TestGerente_SondaDesligadaNaoChamaNada(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	sut, mudou := gerenteSondavel(t, u, upstream.Sonda{})
	esperarPronto(t, sut, 1, mudou)

	_, err := sut.Sondar(context.Background(), 1)
	if !errors.Is(err, upstream.ErrSondaDesligada) {
		t.Fatalf("Sondar com sonda desligada: erro = %v, quer %v", err, upstream.ErrSondaDesligada)
	}
	if n := u.chamadas.Load(); n != 0 {
		t.Errorf("tools/call na ferramenta = %d, quer 0; sonda desligada não chama nada", n)
	}

	s, _ := sut.Situacao(1)
	if s.Sonda.Ligada() {
		t.Error("sonda ligada, quer desligada por padrão")
	}
	if !s.Sonda.Em.IsZero() {
		t.Errorf("última sondagem = %v, quer zero", s.Sonda.Em)
	}
	if s.NoCatalogo == 0 {
		t.Error("ferramentas no catálogo = 0; sonda desligada não tira nada do catálogo")
	}
}

// TestGerente_SondaQuePassaMantemPronto: com a sonda ligada e a ferramenta
// respondendo, o upstream continua pronto e o catálogo continua servido — e a
// primeira sondagem sai no connect, para que "pronto" não seja um verde que
// significa "não sei".
func TestGerente_SondaQuePassaMantemPronto(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	sonda := sondaDeTeste()
	sonda.Espera = textoDaSonda
	sonda.Args = json.RawMessage(`{"eco":"ping"}`)

	sut, mudou := gerenteSondavel(t, u, sonda)
	esperarPronto(t, sut, 1, mudou)

	r := sondarAgora(t, sut, 1)
	if !r.OK {
		t.Fatalf("sondagem = %+v, quer OK", r)
	}
	if r.Pedido != `ping({"eco":"ping"})` {
		t.Errorf("pedido = %q, quer a chamada exata", r.Pedido)
	}
	if r.Resposta != textoDaSonda {
		t.Errorf("resposta = %q, quer %q", r.Resposta, textoDaSonda)
	}

	s, _ := sut.Situacao(1)
	if s.Estado != upstream.EstadoPronto {
		t.Errorf("estado = %q, quer %q", s.Estado, upstream.EstadoPronto)
	}
	if s.Sonda.OKEm.IsZero() {
		t.Error("última sondagem bem-sucedida = zero, quer preenchida")
	}
	if s.Sonda.Falhas != 0 {
		t.Errorf("falhas de sonda = %d, quer 0", s.Sonda.Falhas)
	}
	if s.NoCatalogo != 2 {
		t.Errorf("ferramentas no catálogo = %d, quer 2", s.NoCatalogo)
	}
}

// TestGerente_SondaFalhaDerrubaOCatalogoERecupera é o coração da fatia.
//
// A ferramenta passa a responder isError — o modo de falha que só uma sonda
// funcional detecta, porque tools/list continua perfeito —, e depois da segunda
// sondagem seguida o upstream vai a sonda_falhou e sai do catálogo. Consertar o
// servidor e sondar de novo traz tudo de volta, sem reconectar.
func TestGerente_SondaFalhaDerrubaOCatalogoERecupera(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaComIsError)
	sut, mudou := gerenteSondavel(t, u, sondaDeTeste())
	esperarPronto(t, sut, 1, mudou)

	// A primeira sondagem já saiu no connect e falhou; com tolerância 2, uma
	// falha isolada não pode derrubar nada.
	primeira := sondarAgora(t, sut, 1)
	if primeira.OK {
		t.Fatalf("primeira sondagem = %+v, quer falha", primeira)
	}
	esperarEstado(t, sut, 1, upstream.EstadoSondaFalhou)

	s, _ := sut.Situacao(1)
	if s.Sonda.Falhas < 2 {
		t.Errorf("falhas de sonda = %d, quer ao menos 2", s.Sonda.Falhas)
	}
	if s.UltimoErro == "" {
		t.Error("último erro vazio, quer o motivo da sonda para a UI mostrar")
	}
	if s.Sonda.Resposta == "" {
		t.Error("resposta da sonda vazia; a tela precisa da resposta exata")
	}
	// Sondagem que falha não é connect abandonado nem falha de conexão: contá-la
	// como uma desabilitaria por autoproteção um upstream que responde bem.
	if s.Abandonos != 0 || s.AbandonosTotais != 0 {
		t.Errorf("abandonos = %d/%d, quer 0/0", s.Abandonos, s.AbandonosTotais)
	}
	if s.Falhas != 0 {
		t.Errorf("falhas de conexão = %d, quer 0", s.Falhas)
	}

	// O catálogo esvaziou, mas o tools/list continua guardado: é dele que a
	// recuperação rematerializa, sem gastar uma reconexão.
	if f := sut.Ferramentas(1); len(f) != 0 {
		t.Errorf("ferramentas no catálogo = %d, quer 0", len(f))
	}
	if f := sut.FerramentasDescobertas(1); len(f) != 2 {
		t.Errorf("ferramentas descobertas = %d, quer 2", len(f))
	}

	// Servidor consertado: a sondagem seguinte volta a passar.
	u.definir(ferramentaOK)
	if r := sondarAgora(t, sut, 1); !r.OK {
		t.Fatalf("sondagem depois do conserto = %+v, quer OK", r)
	}
	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	if f := sut.Ferramentas(1); len(f) != 2 {
		t.Errorf("ferramentas de volta no catálogo = %d, quer 2", len(f))
	}
	depois, _ := sut.Situacao(1)
	if depois.UltimoErro != "" {
		t.Errorf("último erro = %q, quer vazio depois da recuperação", depois.UltimoErro)
	}
}

// TestGerente_SondaComTimeoutNaoDerrubaASessao: o prazo da sondagem cancela só a
// chamada.
//
// A ferramenta pendura, a sondagem estoura o próprio timeout, e a sessão
// continua de pé — provado chamando outra ferramenta pelo mesmo transporte. Sem
// isto, "esta ferramenta não responde" viraria "perdi o servidor", que é
// exatamente a confusão que a sonda existe para desfazer.
func TestGerente_SondaComTimeoutNaoDerrubaASessao(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaPendura)
	sonda := sondaDeTeste()
	sonda.Timeout = 150 * time.Millisecond
	sonda.Tolerancia = 1

	sut, mudou := gerenteSondavel(t, u, sonda)
	esperarPronto(t, sut, 1, mudou)
	esperarEstado(t, sut, 1, upstream.EstadoSondaFalhou)

	s, _ := sut.Situacao(1)
	if s.Estado != upstream.EstadoSondaFalhou {
		t.Fatalf("estado = %q, quer %q", s.Estado, upstream.EstadoSondaFalhou)
	}
	if s.Sonda.Erro == "" {
		t.Error("erro da sonda vazio, quer o motivo do prazo estourado")
	}

	// A sessão sobreviveu: outra ferramenta responde pelo mesmo transporte.
	ctx, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelar()
	res, err := sut.Chamar(ctx, 1, "eco", nil)
	if err != nil {
		t.Fatalf("Chamar depois do timeout da sonda: erro = %v, quer nil", err)
	}
	if res.IsError {
		t.Error("chamada devolveu erro de ferramenta, quer sucesso")
	}
}

// TestGerente_SondaConfereOTrechoEsperado cobre o servidor que responde 200 com
// um erro amigável no corpo, sem marcar isError: sem o trecho esperado, a única
// sonda honesta é a que confere o conteúdo.
func TestGerente_SondaConfereOTrechoEsperado(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaOutroTexto)
	sonda := sondaDeTeste()
	sonda.Espera = textoDaSonda
	sonda.Tolerancia = 1

	sut, mudou := gerenteSondavel(t, u, sonda)
	esperarPronto(t, sut, 1, mudou)
	esperarEstado(t, sut, 1, upstream.EstadoSondaFalhou)

	u.definir(ferramentaOK)
	if r := sondarAgora(t, sut, 1); !r.OK {
		t.Fatalf("sondagem com o trecho esperado = %+v, quer OK", r)
	}
	esperarEstado(t, sut, 1, upstream.EstadoPronto)
}

// TestGerente_SondaPeriodicaDisparaPeloRelogio prova que a sondagem se repete
// sozinha, sem esperar por relógio de verdade: o intervalo é pedido ao relógio
// injetado, e liberar a espera dispara a sondagem seguinte.
func TestGerente_SondaPeriodicaDisparaPeloRelogio(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaComIsError)
	sonda := sondaDeTeste()
	sonda.Intervalo = 30 * time.Second
	sonda.Tolerancia = 2

	relogio := novoRelogioFalso()
	sut, mudou := gerenteSondavel(t, u, sonda, upstream.ComRelogio(relogio))
	esperarPronto(t, sut, 1, mudou)

	// A primeira sondagem saiu no connect e falhou (1 de 2). Sincronizar pelo
	// registro do timer no relógio falso, e não por esperarChamadas: soltar a
	// espera antes de a supervisão pedir o intervalo é uma corrida — o timer
	// ainda não existe para ser liberado, e a segunda sondagem nunca dispara.
	for {
		d := relogio.esperarPedido(t)
		if d == sonda.Intervalo {
			relogio.liberar(d)
			break
		}
	}
	esperarEstado(t, sut, 1, upstream.EstadoSondaFalhou)
}

// TestGerente_SondaDeixaRastroDistinguivelNaTrilha: cada sondagem chega a quem
// observa com o desfecho e sem nenhum endpoint — ela não passa por endpoint
// nenhum, e é por isso que quem grava consegue separá-la da chamada de cliente.
func TestGerente_SondaDeixaRastroDistinguivelNaTrilha(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	obs := &observadorDeSonda{}
	sut, mudou := gerenteSondavel(t, u, sondaDeTeste(), upstream.ComObservadorDeSonda(obs))
	esperarPronto(t, sut, 1, mudou)

	if r := sondarAgora(t, sut, 1); !r.OK {
		t.Fatalf("sondagem = %+v, quer OK", r)
	}

	vistas := obs.todas()
	if len(vistas) == 0 {
		t.Fatal("nenhuma sondagem observada, quer ao menos uma")
	}
	ultima := vistas[len(vistas)-1]
	if !ultima.OK {
		t.Errorf("sondagem observada OK = false, quer true")
	}
	if ultima.UpstreamNome != "sondavel" || ultima.Ferramenta != "ping" {
		t.Errorf("sondagem observada = %+v, quer upstream sondavel e ferramenta ping", ultima)
	}
	if ultima.BytesSaida != len(textoDaSonda) {
		t.Errorf("bytes de saída = %d, quer %d", ultima.BytesSaida, len(textoDaSonda))
	}
}

// TestGerente_RedatorLimpaAResposta prova que Resposta passa pelo redator
// injetado (ComRedator) antes de virar SituacaoSonda: sem isto, a resposta de
// um upstream que ecoa de volta o header que recusou apareceria crua na tela.
func TestGerente_RedatorLimpaAResposta(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaComSegredo)

	redator := func(s string) string {
		return strings.ReplaceAll(s, "Bearer abc123def456", "Bearer «redigido»")
	}
	sut, mudou := gerenteSondavel(t, u, sondaDeTeste(), upstream.ComRedator(redator))
	esperarPronto(t, sut, 1, mudou)

	if r := sondarAgora(t, sut, 1); !r.OK {
		t.Fatalf("sondagem = %+v, quer OK", r)
	}

	s, _ := sut.Situacao(1)
	if strings.Contains(s.Sonda.Resposta, "abc123def456") {
		t.Errorf("resposta na situação = %q, credencial não foi redigida", s.Sonda.Resposta)
	}
	if !strings.Contains(s.Sonda.Resposta, "«redigido»") {
		t.Errorf("resposta na situação = %q, quer o texto redigido pelo redator", s.Sonda.Resposta)
	}
}

// TestGerente_SondaManualNaoAbreSegundaChamadaEmParalelo prova que duas
// sondagens manuais pedidas ao mesmo tempo não disparam dois tools/call em
// paralelo: o canal de pedidos não tem buffer, e a supervisão só está livre
// para receber o próximo depois de terminar o que está em curso.
func TestGerente_SondaManualNaoAbreSegundaChamadaEmParalelo(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	sut, mudou := gerenteSondavel(t, u, sondaDeTeste())
	esperarPronto(t, sut, 1, mudou)
	// A sondagem do connect já rodou (modo padrão, sem pendurar).
	esperarChamadas(t, u, 1)

	u.definir(ferramentaPendura)

	primeira := make(chan upstream.ResultadoSonda, 1)
	go func() {
		r, err := sut.Sondar(context.Background(), 1)
		if err != nil {
			t.Errorf("primeira Sondar: erro = %v, quer nil", err)
			return
		}
		primeira <- r
	}()
	// A primeira precisa estar de fato dentro do tools/call pendurado antes de
	// a segunda ser disparada, senão o teste não prova nada sobre ordem.
	esperarChamadas(t, u, 2)

	segunda := make(chan upstream.ResultadoSonda, 1)
	go func() {
		r, err := sut.Sondar(context.Background(), 1)
		if err != nil {
			t.Errorf("segunda Sondar: erro = %v, quer nil", err)
			return
		}
		segunda <- r
	}()

	// Com a primeira ainda pendurada, a segunda não pode ter completado nem
	// aberto um terceiro tools/call: ela está bloqueada mandando para o canal
	// sem buffer, que só a supervisão livre esvazia.
	select {
	case <-segunda:
		t.Fatal("a segunda sondagem completou com a primeira ainda em curso")
	case <-time.After(300 * time.Millisecond):
	}
	if n := u.chamadas.Load(); n != 2 {
		t.Fatalf("chamadas com a primeira em curso = %d, quer 2 (a segunda não pôde abrir)", n)
	}

	u.liberar()
	<-primeira
	<-segunda
	if n := u.chamadas.Load(); n != 3 {
		t.Errorf("chamadas depois de liberar as duas = %d, quer 3", n)
	}
}

// TestGerente_SondarUpstreamDegradadoEIndisponivel prova que o botão "Sondar
// agora" contra um upstream degradado — sonda ligada, mas sem sessão de pé —
// devolve ErrIndisponivel em vez de abrir um tools/call contra transporte
// nenhum.
func TestGerente_SondarUpstreamDegradadoEIndisponivel(t *testing.T) {
	t.Parallel()

	// Um servidor HTTP que não fala MCP: toda tentativa de Connect falha, e o
	// upstream fica degradado sem nunca ter chegado a pronto.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	sonda := sondaDeTeste()
	cfg := upstream.Config{
		ID: 1, Nome: "indisponivel", Tipo: upstream.TipoHTTP,
		URL: ts.URL, Timeout: 300 * time.Millisecond, Sonda: sonda,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComIntervaloTentativa(20*time.Millisecond))
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	esperarEstado(t, sut, 1, upstream.EstadoDegradado)

	_, err := sut.Sondar(context.Background(), 1)
	if !errors.Is(err, upstream.ErrIndisponivel) {
		t.Fatalf("Sondar em degradado: erro = %v, quer %v", err, upstream.ErrIndisponivel)
	}
}

// TestGerente_SondaFalhouDegradadoProntoRejulgaDoZero prova que a sonda
// esquece o julgamento antigo quando a sessão morre e outra é aberta: um
// upstream em sonda_falhou que perde a sessão e reconecta não continua
// contando a partir da tolerância já estourada, e sim do zero, com uma
// sondagem de connect nova decidindo o veredito.
//
// Lento como TestGerente_ProntoZeraAbandonosConsecutivos, e pelo mesmo motivo:
// derrubar uma sessão HTTP já pronta exige que o reconector do stream SSE
// autônomo do go-sdk desista sozinho, uns 15-20s reais que só mudar
// ComClienteHTTP em produção encurtaria.
func TestGerente_SondaFalhouDegradadoProntoRejulgaDoZero(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaComIsError)
	tp := &transporteControlavel{real: http.DefaultTransport, modo: modoPassar}
	sut, mudou := gerenteSondavel(t, u, sondaDeTeste(), upstream.ComClienteHTTP(&http.Client{Transport: tp}))
	t.Cleanup(func() { tp.definir(modoPassar) })

	esperarPronto(t, sut, 1, mudou)
	// A do connect é a primeira falha (1 de 2); com o intervalo de uma hora,
	// só uma segunda pedida manualmente estoura a tolerância.
	sondarAgora(t, sut, 1)
	esperarEstado(t, sut, 1, upstream.EstadoSondaFalhou)

	antes, _ := sut.Situacao(1)
	if antes.Sonda.Falhas == 0 {
		t.Fatalf("falhas de sonda antes da queda = %d, quer ao menos 1", antes.Sonda.Falhas)
	}

	// A sessão morre por falha de transporte — não é a sonda — e a supervisão
	// passa por degradado antes de reconectar. As duas juntas, como em
	// TestGerente_ProntoZeraAbandonosConsecutivos: só recusar RoundTrip não
	// derruba uma conexão que já está aberta.
	tp.definir(modoErro)
	u.derrubarConexoes()
	esperarSairDe(t, sut, 1, upstream.EstadoSondaFalhou, 30*time.Second)

	// O transporte volta e o servidor responde bem: a sessão nova reconecta,
	// e a sondagem do connect julga do zero — sem carregar as falhas da
	// sessão anterior.
	tp.definir(modoPassar)
	u.definir(ferramentaOK)
	esperarEstado(t, sut, 1, upstream.EstadoPronto)

	depois, _ := sut.Situacao(1)
	if depois.Sonda.Falhas != 0 {
		t.Errorf("falhas de sonda depois de reconectar = %d, quer 0", depois.Sonda.Falhas)
	}
	if depois.Sonda.Erro != "" {
		t.Errorf("erro de sonda depois de reconectar = %q, quer vazio", depois.Sonda.Erro)
	}
	if depois.UltimoErro != "" {
		t.Errorf("último erro depois de reconectar = %q, quer vazio", depois.UltimoErro)
	}
}

// TestGerente_AplicarComSondaVaziaSaiDeSondaFalhou prova que salvar o upstream
// com a sonda desligada é a saída manual de sonda_falhou: a máquina reconecta
// do zero, sem a sonda para julgar nada, e o catálogo volta a ser servido.
func TestGerente_AplicarComSondaVaziaSaiDeSondaFalhou(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaComIsError)
	sonda := sondaDeTeste()
	cfg := upstream.Config{
		ID: 1, Nome: "sondavel", Tipo: upstream.TipoHTTP,
		URL: u.url, Timeout: 5 * time.Second, Sonda: sonda,
	}
	mudou := make(chan struct{}, 64)
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComIntervaloTentativa(20*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}))
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})
	esperarPronto(t, sut, 1, mudou)
	// A do connect é a primeira falha (1 de 2); com o intervalo de uma hora,
	// só uma segunda pedida manualmente estoura a tolerância.
	sondarAgora(t, sut, 1)
	esperarEstado(t, sut, 1, upstream.EstadoSondaFalhou)
	if f := sut.Ferramentas(1); len(f) != 0 {
		t.Fatalf("ferramentas em sonda_falhou = %d, quer 0", len(f))
	}

	cfg.Sonda = upstream.Sonda{}
	if err := sut.Aplicar(context.Background(), cfg); err != nil {
		t.Fatalf("Aplicar com sonda vazia: erro = %v, quer nil", err)
	}

	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	if f := sut.Ferramentas(1); len(f) != 2 {
		t.Errorf("ferramentas depois de Aplicar sem sonda = %d, quer 2", len(f))
	}
	s, _ := sut.Situacao(1)
	if s.Sonda.Ligada() {
		t.Error("sonda ligada depois de Aplicar com Sonda{}, quer desligada")
	}
}

// TestGerente_SondaFalhouNaoVaiAoBanco fecha a regra da seção 05 para o estado
// novo da fatia: sonda_falhou vive em memória como degradado, e o próximo boot
// recomeça em novo.
func TestGerente_SondaFalhouNaoVaiAoBanco(t *testing.T) {
	t.Parallel()

	u := novoUpstreamSondavel(t)
	u.definir(ferramentaComIsError)

	repo, st := repositorioDeTeste(t)
	form := formBase("sondavel")
	form.URL = u.url
	form.SondaHabilitada = true
	form.SondaFerramenta = "ping"
	form.SondaIntervaloMS = 3_600_000
	form.SondaTimeoutMS = 2_000
	form.SondaTolerancia = 1
	if !form.Validar() {
		t.Fatalf("formulário recusado: %v", form.Erros)
	}
	id, err := repo.Criar(context.Background(), form)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	cfgs, err := upstream.Habilitados(context.Background(), st.Leitura())
	if err != nil {
		t.Fatalf("Habilitados: erro = %v, quer nil", err)
	}
	if len(cfgs) != 1 || !cfgs[0].Sonda.Ativa() {
		t.Fatalf("configuração lida = %+v, quer uma com sonda ativa", cfgs)
	}

	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), cfgs,
		upstream.ComIntervaloTentativa(20*time.Millisecond))
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	esperarEstado(t, sut, id, upstream.EstadoSondaFalhou)

	reg, err := repo.Obter(context.Background(), id)
	if err != nil {
		t.Fatalf("Obter: erro = %v, quer nil", err)
	}
	if !reg.Habilitado {
		t.Error("habilitado = false; a sonda gravou a decisão dela no banco")
	}
	if reg.UltimoErro != "" {
		t.Errorf("ultimo_erro gravado = %q, quer vazio; erro é memória, não banco", reg.UltimoErro)
	}
	if !reg.Sonda.Habilitada || reg.Sonda.Ferramenta != "ping" {
		t.Errorf("sonda gravada = %+v, quer a configuração intacta", reg.Sonda)
	}

	// O boot seguinte lê o banco e recomeça a máquina do começo: nenhuma coluna
	// carrega sonda_falhou adiante.
	depois := bootSeguinte(t, st)
	if len(depois) != 1 {
		t.Fatalf("upstreams habilitados no boot seguinte = %d, quer 1", len(depois))
	}
	outro := upstream.NovoGerente(slog.New(slog.DiscardHandler), depois)
	if s, ok := outro.Situacao(id); !ok || s.Estado != upstream.EstadoNovo {
		t.Errorf("estado no boot seguinte = %q, quer %q", s.Estado, upstream.EstadoNovo)
	}
}

func bootSeguinte(t *testing.T, st *store.Store) []upstream.Config {
	t.Helper()
	cfgs, err := upstream.Habilitados(context.Background(), st.Leitura())
	if err != nil {
		t.Fatalf("Habilitados no boot seguinte: erro = %v, quer nil", err)
	}
	return cfgs
}

// esperarChamadas espera a ferramenta sondada receber ao menos n chamadas, por
// sinal e não por relógio.
func esperarChamadas(t *testing.T, u *upstreamSondavel, n int64) {
	t.Helper()

	limite := time.After(15 * time.Second)
	tique := time.NewTicker(5 * time.Millisecond)
	defer tique.Stop()
	for {
		if u.chamadas.Load() >= n {
			return
		}
		select {
		case <-tique.C:
		case <-limite:
			t.Fatalf("a ferramenta recebeu %d chamadas em 15s, quer ao menos %d",
				u.chamadas.Load(), n)
		}
	}
}

// TestForm_ValidarSonda cobre o que o formulário aceita e o que ele recusa.
//
// A regra que mais importa aqui é a de baixo: com a sonda desligada, número
// fora de faixa não trava o salvamento — vira o padrão em silêncio, porque o
// campo está inerte. Com a sonda ligada, o mesmo número vira erro no campo, e é
// isso que impede uma sonda de 10 ms de virar a carga que ela deveria
// diagnosticar.
func TestForm_ValidarSonda(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		ajuste    func(*upstream.Form)
		querPassa bool
		querErro  string // chave em Erros que precisa existir
	}{
		"sonda desligada é o padrão e passa": {
			ajuste: func(*upstream.Form) {}, querPassa: true,
		},
		"sonda ligada com ferramenta passa": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
			},
			querPassa: true,
		},
		"sonda ligada com argumentos JSON passa": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaArgs = `{"query": "ping"}`
			},
			querPassa: true,
		},
		"sonda ligada sem ferramenta": {
			ajuste:   func(f *upstream.Form) { f.SondaHabilitada = true },
			querErro: "sonda_ferramenta",
		},
		"argumentos que não são JSON": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaArgs = "query=ping"
			},
			querErro: "sonda_args",
		},
		"argumentos que são array e não objeto": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaArgs = `["ping"]`
			},
			querErro: "sonda_args",
		},
		"argumentos inválidos recusam mesmo com a sonda desligada": {
			// O JSON é o único campo validado com a sonda desligada: ele fica
			// gravado e voltaria como erro de execução no dia em que alguém
			// ligasse a sonda, longe do formulário que o produziu.
			ajuste:   func(f *upstream.Form) { f.SondaArgs = "{" },
			querErro: "sonda_args",
		},
		"intervalo curto demais": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaIntervaloMS = 10
			},
			querErro: "sonda_intervalo_ms",
		},
		"timeout fora da faixa": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaTimeoutMS = 1
			},
			querErro: "sonda_timeout_ms",
		},
		"tolerância zero vira o padrão": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaTolerancia = 0
			},
			querPassa: true,
		},
		"tolerância acima do teto": {
			ajuste: func(f *upstream.Form) {
				f.SondaHabilitada, f.SondaFerramenta = true, "search"
				f.SondaTolerancia = 99
			},
			querErro: "sonda_tolerancia",
		},
		"número absurdo com a sonda desligada não trava o formulário": {
			ajuste:    func(f *upstream.Form) { f.SondaIntervaloMS = 3 },
			querPassa: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := upstream.Form{
				Nome: "notion", URL: "https://exemplo.invalido/mcp",
				TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
			}
			tc.ajuste(&sut)

			passou := sut.Validar()

			if passou != tc.querPassa {
				t.Fatalf("Validar() = %v, quer %v (erros: %v)", passou, tc.querPassa, sut.Erros)
			}
			if tc.querErro != "" && sut.Erros[tc.querErro] == "" {
				t.Errorf("Erros[%q] vazio, quer a mensagem do campo (erros: %v)", tc.querErro, sut.Erros)
			}
			if !tc.querPassa {
				return
			}
			// Formulário aceito nunca sai com número fora de faixa: é ele que
			// vai direto para o banco.
			sonda := sut.SondaDoForm()
			if sonda.Intervalo < time.Duration(upstream.SondaIntervaloMinimoMS)*time.Millisecond {
				t.Errorf("intervalo = %v, quer ao menos o mínimo", sonda.Intervalo)
			}
			if sonda.Timeout <= 0 || sonda.Tolerancia < 1 {
				t.Errorf("sonda normalizada = %+v, quer timeout e tolerância positivos", sonda)
			}
		})
	}
}
