package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/trilha"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// upstreamComPing é um servidor MCP de verdade cuja ferramenta sondada passa a
// devolver isError sob comando do teste.
//
// É o modo de falha que a fatia 9 existe para pegar: o servidor conversa, o
// tools/list continua perfeito, e a chamada de verdade não funciona. Sem sonda,
// o patchbay não teria como distinguir isso de um servidor saudável.
type upstreamComPing struct {
	url      string
	quebrado atomic.Bool
	// chamadas conta os tools/call da ferramenta sondada, para o teste provar
	// que a sonda desligada não chama nada.
	chamadas atomic.Int64
}

func upstreamSondavelHTTP(t *testing.T) *upstreamComPing {
	t.Helper()

	u := &upstreamComPing{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "sondavel", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			u.chamadas.Add(1)
			if u.quebrado.Load() {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "cota da api esgotada"}},
				}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "pong"}},
			}, nil
		})
	srv.AddTool(
		&mcp.Tool{Name: "buscar", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "achei"}}}, nil
		})

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(func() {
		// CloseClientConnections antes de Close: o patchbay mantém o stream SSE
		// deste upstream aberto, e Close espera pelas requisições em curso.
		ts.CloseClientConnections()
		ts.Close()
	})
	u.url = ts.URL
	return u
}

// camposComSonda monta os campos do formulário de upstream com a sonda ligada.
func camposComSonda(nome, alvo, ferramenta string, tolerancia int) url.Values {
	return url.Values{
		"nome":               {nome},
		"url":                {alvo},
		"timeout_ms":         {"5000"},
		"habilitado":         {"1"},
		"sonda_habilitada":   {"1"},
		"sonda_ferramenta":   {ferramenta},
		"sonda_intervalo_ms": {"3600000"},
		"sonda_timeout_ms":   {"3000"},
		"sonda_tolerancia":   {strconv.Itoa(tolerancia)},
	}
}

// TestUI_SondaFuncionalDerrubaEDevolveOCatalogo é a demonstração da fatia 9 pela
// tela, ponta a ponta.
//
// A sonda é ligada no formulário, o servidor passa a responder isError, o botão
// "Sondar agora" leva o upstream a sonda_falhou, e o cliente MCP de verdade vê a
// ferramenta virar lápide — continua listada, explicando que saiu, em vez de
// sumir e produzir unknown tool. Consertar o servidor e sondar de novo devolve
// tudo, sem reiniciar nada.
func TestUI_SondaFuncionalDerrubaEDevolveOCatalogo(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamSondavelHTTP(t)
	res := u.enviarForm(t, webui.RotaUpstreams,
		camposComSonda("sondavel", alvo.url, "ping", 1))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar upstream com sonda: status = %d, quer %d (corpo: %q)",
			res.StatusCode, http.StatusOK, corpo(t, res))
	}
	upstreamID := idDoDestino(t, res)
	endpointID := u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	chave := u.criarChave(t, "cliente", endpointID)

	esperarFerramentas(t, u, "pessoal", 2)
	sessao := conectarMCP(t, u, "pessoal", chave)
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, []string{"buscar", "ping"}) {
		t.Fatalf("ferramentas = %v, quer [buscar ping]", nomes)
	}

	// O servidor quebra de um jeito que só a sonda detecta: tools/list continua
	// idêntico, e é a chamada que passa a falhar.
	alvo.quebrado.Store(true)
	res = u.enviarForm(t, rotaSondar(upstreamID), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Sondar agora: status = %d, quer %d (corpo: %q)",
			res.StatusCode, http.StatusOK, corpo(t, res))
	}
	if texto := corpo(t, res); !strings.Contains(texto, "A sondagem falhou") {
		t.Errorf("a tela não avisou que a sondagem falhou; corpo: %q", resumirCorpo(texto))
	}

	esperarEstadoNaUI(t, u, upstreamID, upstream.EstadoSondaFalhou)
	esperarFerramentas(t, u, "pessoal", 0)

	lapides := u.app.endpoints.Lapides("pessoal")
	if !slices.Equal(lapides, []string{"buscar", "ping"}) {
		t.Fatalf("lápides = %v, quer [buscar ping]; a remoção pela sonda usa a mesma "+
			"janela de graça de qualquer outra remoção", lapides)
	}
	// O cliente que já estava conectado continua vendo os nomes: a lápide
	// responde explicando que a ferramenta saiu, em vez de unknown tool.
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, []string{"buscar", "ping"}) {
		t.Errorf("ferramentas com lápide = %v, quer [buscar ping]", nomes)
	}
	ctx, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelar()
	resposta, err := sessao.CallTool(ctx, &mcp.CallToolParams{Name: "buscar"})
	if err != nil {
		t.Fatalf("chamar ferramenta com lápide: erro = %v, quer nil", err)
	}
	if !resposta.IsError {
		t.Error("a lápide respondeu sem erro, quer erro de ferramenta explicando que ela saiu")
	}

	// Servidor consertado: sondar de novo devolve o catálogo inteiro.
	alvo.quebrado.Store(false)
	res = u.enviarForm(t, rotaSondar(upstreamID), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Sondar agora depois do conserto: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	esperarEstadoNaUI(t, u, upstreamID, upstream.EstadoPronto)
	esperarFerramentas(t, u, "pessoal", 2)

	depois, err := sessao.CallTool(ctx, &mcp.CallToolParams{Name: "buscar"})
	if err != nil {
		t.Fatalf("chamar ferramenta recuperada: erro = %v, quer nil", err)
	}
	if depois.IsError {
		t.Error("a ferramenta recuperada respondeu com erro, quer sucesso")
	}
}

// TestUI_SondaDesligadaNaoChamaNadaEAparNaTela é o opt-in pela tela: um upstream
// criado sem mexer na sonda não recebe nenhum tools/call, o botão "Sondar agora"
// nem aparece, e a tela diz que a sonda está desligada — nunca verde.
func TestUI_SondaDesligadaNaoChamaNadaEApareceNaTela(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamSondavelHTTP(t)
	upstreamID := u.criarUpstream(t, "sem-sonda", alvo.url)
	u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	esperarFerramentas(t, u, "pessoal", 2)

	if n := alvo.chamadas.Load(); n != 0 {
		t.Errorf("tools/call na ferramenta = %d, quer 0; a sonda vem desligada", n)
	}

	tela := corpo(t, u.abrir(t, rotaDoUpstream(upstreamID)))
	if !strings.Contains(tela, "desligada") {
		t.Errorf("a tela não mostra a sonda como desligada; corpo: %q", resumirCorpo(tela))
	}
	if strings.Contains(tela, "Sondar agora") {
		t.Error("o botão Sondar agora apareceu com a sonda desligada")
	}

	// A rota existe e recusa: um POST forjado não liga a sonda por tabela.
	res := u.enviarForm(t, rotaSondar(upstreamID), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Sondar agora com sonda desligada: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	if n := alvo.chamadas.Load(); n != 0 {
		t.Errorf("tools/call depois do POST = %d, quer 0", n)
	}
}

// TestUI_SondaDeixaRastroDistinguivelNaTrilha: a sondagem vira linha de trilha
// com origem sonda, e os contadores do painel — que contam só chamada de
// cliente — não a somam.
func TestUI_SondaDeixaRastroDistinguivelNaTrilha(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamSondavelHTTP(t)
	res := u.enviarForm(t, webui.RotaUpstreams,
		camposComSonda("sondavel", alvo.url, "ping", 2))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar upstream com sonda: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	upstreamID := idDoDestino(t, res)
	u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	esperarFerramentas(t, u, "pessoal", 2)

	if r := u.enviarForm(t, rotaSondar(upstreamID), nil); r.StatusCode != http.StatusOK {
		t.Fatalf("Sondar agora: status = %d, quer %d", r.StatusCode, http.StatusOK)
	}

	// Pela tela e não pelo repositório: o que a fatia promete é que a sondagem
	// *aparece* distinguível na trilha, e uma assertiva contra o banco passaria
	// mesmo se o handler filtrasse tudo fora.
	tela := esperarNaTrilhaCom(t, u, "?origem="+string(trilha.OrigemSonda), "ping")
	if !strings.Contains(tela, ">sonda<") {
		t.Errorf("a linha da trilha não veio marcada como sonda; corpo: %q", resumirCorpo(tela))
	}
	// O mesmo filtro do outro lado não pode mostrar a sondagem: é assim que a
	// tela separa o que um cliente pediu do que o patchbay pediu por conta.
	cliente := corpo(t, u.abrir(t, webui.RotaTrilha+"?origem="+string(trilha.OrigemCliente)))
	if strings.Contains(cliente, "ping") {
		t.Errorf("a sondagem apareceu no filtro de chamadas de cliente; corpo: %q", resumirCorpo(cliente))
	}

	// Os contadores do painel contam só chamada de cliente: a sonda é o custo
	// da observação, não tráfego.
	resumo, err := u.app.repoTrilha.Resumo(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Resumo: erro = %v, quer nil", err)
	}
	if resumo.Chamadas != 0 {
		t.Errorf("chamadas no resumo = %d, quer 0; a sonda não pode inflar a métrica de cliente",
			resumo.Chamadas)
	}
}

func rotaDoUpstream(id int64) string {
	return webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)
}

func rotaSondar(id int64) string { return rotaDoUpstream(id) + "/sondar" }

// esperarEstadoNaUI espera o upstream chegar ao estado pedido, por sinal e não
// por relógio.
func esperarEstadoNaUI(t *testing.T, u uiDeTeste, id int64, quer upstream.Estado) {
	t.Helper()

	limite := time.After(20 * time.Second)
	tique := time.NewTicker(10 * time.Millisecond)
	defer tique.Stop()
	for {
		if s, ok := u.app.gerente.Situacao(id); ok && s.Estado == quer {
			return
		}
		select {
		case <-tique.C:
		case <-limite:
			s, _ := u.app.gerente.Situacao(id)
			t.Fatalf("upstream %d não chegou a %q em 20s (estado = %q, erro = %q)",
				id, quer, s.Estado, s.UltimoErro)
		}
	}
}

// resumirCorpo corta o HTML da tela antes de ele entrar numa mensagem de falha.
func resumirCorpo(s string) string {
	const limite = 400
	if len(s) <= limite {
		return s
	}
	return s[:limite] + "…"
}
