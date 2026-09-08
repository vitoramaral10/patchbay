package upstream_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// servidorFalso sobe um servidor MCP de verdade do go-sdk com ferramentas de
// mentira. O "falso" é o catálogo, não o protocolo: um mock escrito à mão
// concordaria com o meu erro de entendimento do MCP.
func servidorFalso(t *testing.T, ferramentas ...string) string {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-falso", Version: "0.0.1"}, nil)
	for _, nome := range ferramentas {
		srv.AddTool(
			&mcp.Tool{Name: nome, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
				}, nil
			})
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(func() {
		// CloseClientConnections antes de Close: o patchbay mantém o stream SSE
		// do upstream aberto, e Close espera pelas requisições em curso. Sem
		// isto, a limpeza do teste trava para sempre quando o servidor falso é
		// criado depois do gerente — a ordem de t.Cleanup é a inversa da
		// criação, e o gerente ainda não desligou nessa hora.
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL
}

// esperarPronto espera o upstream chegar a pronto, por sinal e não por relógio.
func esperarPronto(t *testing.T, g *upstream.Gerente, id int64, mudou <-chan struct{}) {
	t.Helper()

	limite := time.After(15 * time.Second)
	for {
		if s, ok := g.Situacao(id); ok && s.Estado == upstream.EstadoPronto {
			return
		}
		select {
		case <-mudou:
		case <-limite:
			s, _ := g.Situacao(id)
			t.Fatalf("upstream %d não ficou pronto em 15s (estado = %q, erro = %q)",
				id, s.Estado, s.UltimoErro)
		}
	}
}

func gerenteDeTeste(t *testing.T, cfgs []upstream.Config) (*upstream.Gerente, <-chan struct{}) {
	t.Helper()

	mudou := make(chan struct{}, 64)
	g := upstream.NovoGerente(slog.New(slog.DiscardHandler), cfgs,
		upstream.ComIntervaloTentativa(20*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
	)
	ctx, cancelar := context.WithCancel(context.Background())
	g.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		g.Aguardar()
	})
	return g, mudou
}

// TestGerente_AplicarPoeUpstreamNoArSemReiniciar é o coração da fatia: um
// upstream cadastrado depois do boot entra em supervisão e fica pronto, sem que o
// processo tenha reiniciado.
func TestGerente_AplicarPoeUpstreamNoArSemReiniciar(t *testing.T) {
	t.Parallel()

	sut, mudou := gerenteDeTeste(t, nil)
	if n := len(sut.Situacoes()); n != 0 {
		t.Fatalf("upstreams no boot = %d, quer 0", n)
	}

	cfg := upstream.Config{
		ID: 7, Nome: "novo", Tipo: upstream.TipoHTTP,
		URL: servidorFalso(t, "alfa", "beta"), Timeout: 5 * time.Second,
	}
	if err := sut.Aplicar(context.Background(), cfg); err != nil {
		t.Fatalf("Aplicar: erro = %v, quer nil", err)
	}
	esperarPronto(t, sut, 7, mudou)

	if f := sut.Ferramentas(7); len(f) != 2 {
		t.Errorf("ferramentas = %d, quer 2", len(f))
	}
	res, err := sut.Chamar(context.Background(), 7, "alfa", nil)
	if err != nil {
		t.Fatalf("Chamar: erro = %v, quer nil", err)
	}
	if res.IsError {
		t.Error("chamada devolveu erro de ferramenta, quer sucesso")
	}
}

// TestGerente_AplicarReconfiguraTrocandoASessao prova que editar a URL de um
// upstream troca a sessão em vez de mutar a que já existe — e que o catálogo
// passa a ser o do servidor novo.
func TestGerente_AplicarReconfiguraTrocandoASessao(t *testing.T) {
	t.Parallel()

	antigo := upstream.Config{
		ID: 1, Nome: "servico", Tipo: upstream.TipoHTTP,
		URL: servidorFalso(t, "antiga"), Timeout: 5 * time.Second,
	}
	sut, mudou := gerenteDeTeste(t, []upstream.Config{antigo})
	esperarPronto(t, sut, 1, mudou)

	novo := antigo
	novo.URL = servidorFalso(t, "nova-a", "nova-b")
	if err := sut.Aplicar(context.Background(), novo); err != nil {
		t.Fatalf("Aplicar: erro = %v, quer nil", err)
	}
	esperarPronto(t, sut, 1, mudou)

	ferramentas := sut.Ferramentas(1)
	if len(ferramentas) != 2 {
		t.Fatalf("ferramentas = %d, quer 2 (as do servidor novo)", len(ferramentas))
	}
	for _, f := range ferramentas {
		if f.Name == "antiga" {
			t.Error("ferramenta do servidor antigo continua no catálogo")
		}
	}
	if s, _ := sut.Situacao(1); s.Config.URL != novo.URL {
		t.Errorf("URL supervisionada = %q, quer %q", s.Config.URL, novo.URL)
	}
}

// TestGerente_RemoverNaoVazaGoroutineNemSessao é o requisito de "remover fecha a
// sessão e a goroutine de supervisão sem vazar".
//
// A assertiva é sobre as goroutines *nossas*, contadas pela pilha: um total de
// runtime.NumGoroutine incluiria os readLoop/writeLoop que o transporte HTTP
// mantém em conexão ociosa, e o teste ficaria ou frouxo ou instável. Como Remover
// só volta depois de a goroutine sair, a contagem é determinística — nada de
// esperar pelo relógio.
//
// Este é o único teste do pacote sem t.Parallel(), e de propósito: ele conta
// goroutines do processo inteiro, e um teste paralelo rodando ao lado apareceria
// na contagem. Sem paralelo ele ganha a janela exclusiva que o próprio go test
// dá aos testes sequenciais.
//
//nolint:paralleltest // conta goroutines do processo: precisa rodar sozinho
func TestGerente_RemoverNaoVazaGoroutineNemSessao(t *testing.T) {
	url := servidorFalso(t, "alfa")
	sut, mudou := gerenteDeTeste(t, nil)

	const rodadas = 8
	for i := range rodadas {
		id := int64(100 + i)
		cfg := upstream.Config{
			ID: id, Nome: fmt.Sprintf("efemero-%d", i), Tipo: upstream.TipoHTTP,
			URL: url, Timeout: 5 * time.Second,
		}
		if err := sut.Aplicar(context.Background(), cfg); err != nil {
			t.Fatalf("Aplicar %d: erro = %v, quer nil", id, err)
		}
		esperarPronto(t, sut, id, mudou)
		if n := goroutinesCom(t, trechoSupervisao); n != 1 {
			t.Fatalf("goroutines de supervisão com o upstream no ar = %d, quer 1", n)
		}

		if err := sut.Remover(context.Background(), id); err != nil {
			t.Fatalf("Remover %d: erro = %v, quer nil", id, err)
		}
		if _, ok := sut.Situacao(id); ok {
			t.Fatalf("upstream %d continua supervisionado depois de Remover", id)
		}
		if _, err := sut.Chamar(context.Background(), id, "alfa", nil); !errors.Is(err, upstream.ErrDesconhecido) {
			t.Fatalf("Chamar depois de Remover: erro = %v, quer %v", err, upstream.ErrDesconhecido)
		}
		if n := goroutinesCom(t, trechoSupervisao); n != 0 {
			t.Fatalf("goroutines de supervisão depois de Remover = %d, quer 0", n)
		}
		// A goroutine que espera o fim da sessão MCP some junto: ela vive dentro
		// de conectarEDescobrir, e é o defer dela que fecha a sessão.
		if n := goroutinesCom(t, trechoSessao); n != 0 {
			t.Fatalf("goroutines esperando sessão de upstream = %d, quer 0", n)
		}
	}
}

// Trechos de pilha que identificam as goroutines deste pacote.
const (
	trechoSupervisao = "internal/upstream.(*Gerente).supervisionar"
	trechoSessao     = "internal/upstream.(*Gerente).conectarEDescobrir"
)

// goroutinesCom conta quantas goroutines têm trecho na pilha.
func goroutinesCom(t *testing.T, trecho string) int {
	t.Helper()

	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	total := 0
	for _, pilha := range strings.Split(string(buf), "\n\ngoroutine ") {
		if strings.Contains(pilha, trecho) {
			total++
		}
	}
	return total
}

// TestGerente_RemoverDesconhecidoNaoErra: o resultado desejado ("este upstream
// não está no ar") já vale, então remover duas vezes é idempotente.
func TestGerente_RemoverDesconhecidoNaoErra(t *testing.T) {
	t.Parallel()

	sut, _ := gerenteDeTeste(t, nil)
	if err := sut.Remover(context.Background(), 999); err != nil {
		t.Errorf("Remover inexistente: erro = %v, quer nil", err)
	}
}

// TestGerente_AplicarConcorrenteNoMesmoID é o teste de corrida do ciclo de vida:
// vários Aplicar e Remover do mesmo upstream ao mesmo tempo têm que terminar com
// exatamente um estado, e nunca com duas supervisões ou nenhuma.
//
// Roda com -race. Sem a serialização na goroutine despachante, aqui aparece ou
// corrida no mapa, ou um upstream supervisionado por duas goroutines.
func TestGerente_AplicarConcorrenteNoMesmoID(t *testing.T) {
	t.Parallel()

	url := servidorFalso(t, "alfa")
	sut, mudou := gerenteDeTeste(t, nil)

	cfg := upstream.Config{
		ID: 42, Nome: "disputado", Tipo: upstream.TipoHTTP,
		URL: url, Timeout: 5 * time.Second,
	}

	var grupo sync.WaitGroup
	const paralelos = 12
	for i := range paralelos {
		grupo.Add(1)
		go func(i int) {
			defer grupo.Done()
			if i%3 == 2 {
				if err := sut.Remover(context.Background(), cfg.ID); err != nil {
					t.Errorf("Remover concorrente: erro = %v, quer nil", err)
				}
				return
			}
			if err := sut.Aplicar(context.Background(), cfg); err != nil {
				t.Errorf("Aplicar concorrente: erro = %v, quer nil", err)
			}
		}(i)
	}
	grupo.Wait()

	// Estado final determinístico: um Aplicar depois de toda a rajada deixa
	// exatamente uma supervisão viva e funcional.
	if err := sut.Aplicar(context.Background(), cfg); err != nil {
		t.Fatalf("Aplicar final: erro = %v, quer nil", err)
	}
	esperarPronto(t, sut, cfg.ID, mudou)

	if n := len(sut.Situacoes()); n != 1 {
		t.Fatalf("upstreams supervisionados = %d, quer 1", n)
	}
	if _, err := sut.Chamar(context.Background(), cfg.ID, "alfa", nil); err != nil {
		t.Errorf("Chamar depois da rajada: erro = %v, quer nil", err)
	}
}

// TestGerente_AplicarRecusaConfiguracaoInvalida: validação antes de mexer no ar,
// para que a UI possa reexibir o erro sem ter derrubado nada.
func TestGerente_AplicarRecusaConfiguracaoInvalida(t *testing.T) {
	t.Parallel()

	sut, _ := gerenteDeTeste(t, nil)

	casos := map[string]upstream.Config{
		"sem url":            {ID: 1, Nome: "x", Tipo: upstream.TipoHTTP, Timeout: time.Second},
		"sem nome":           {ID: 1, Tipo: upstream.TipoHTTP, URL: "http://x", Timeout: time.Second},
		"timeout zero":       {ID: 1, Nome: "x", Tipo: upstream.TipoHTTP, URL: "http://x"},
		"tipo não suportado": {ID: 1, Nome: "x", Tipo: upstream.TipoSTDIO, Timeout: time.Second},
	}
	for nome, cfg := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if err := sut.Aplicar(context.Background(), cfg); err == nil {
				t.Error("erro = nil, quer recusa da configuração")
			}
			if _, ok := sut.Situacao(1); ok {
				t.Error("upstream inválido entrou na supervisão")
			}
		})
	}
}

// TestGerente_AplicarDepoisDoDesligamento devolve erro em vez de travar: a
// requisição da UI não pode ficar pendurada porque o processo está desligando.
func TestGerente_AplicarDepoisDoDesligamento(t *testing.T) {
	t.Parallel()

	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), nil)
	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	cancelar()
	sut.Aguardar()

	err := sut.Aplicar(context.Background(), upstream.Config{
		ID: 1, Nome: "tarde", Tipo: upstream.TipoHTTP, URL: "http://x", Timeout: time.Second,
	})
	if !errors.Is(err, upstream.ErrGerenteParado) {
		t.Errorf("erro = %v, quer %v", err, upstream.ErrGerenteParado)
	}
}
