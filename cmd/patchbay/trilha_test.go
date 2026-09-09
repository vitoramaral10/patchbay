package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// tetoDaTrilha é quanto o teste espera pela trilha aparecer. A gravação é em
// lote com tique de um segundo, então "logo depois de responder" não é "no mesmo
// instante" — e é justamente esse o desenho que a fatia 12 defende.
const tetoDaTrilha = 20 * time.Second

// TestUI_TrilhaRegistraAChamadaEMostraNaTela é a demonstração ponta a ponta da
// fatia 12: um cliente MCP de verdade chama uma ferramenta, a chamada aparece no
// log ao vivo por SSE na hora e na tela de trilha depois do lote — e em nenhum
// dos dois aparece a chave de API que abriu a sessão.
func TestUI_TrilhaRegistraAChamadaEMostraNaTela(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "falso", upstreamFalso(t))
	u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	chave := u.criarChave(t, "navegador", 1)
	esperarFerramentas(t, u, "pessoal", 2)

	// O stream de SSE é aberto *antes* da chamada: o log ao vivo não tem
	// histórico, e quem conecta depois não vê o que passou.
	fluxo := abrirFluxo(t, u)

	sessao := conectarMCP(t, u, "pessoal", chave)
	res, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call somar: erro = %v, quer nil", err)
	}
	if texto := textoDe(res); texto != "42" {
		t.Fatalf("resultado = %q, quer %q", texto, "42")
	}

	// 1. O log ao vivo entrega a chamada, sem esperar pelo lote de gravação.
	evento := fluxo.esperarEvento(t, "chamada")
	if !strings.Contains(evento, "somar") {
		t.Errorf("evento de SSE = %q, quer conter a ferramenta chamada", evento)
	}
	if !strings.Contains(evento, "pessoal") || !strings.Contains(evento, "falso") {
		t.Errorf("evento de SSE = %q, quer nomear o endpoint e o upstream", evento)
	}

	// 2. A trilha gravada mostra a mesma chamada, com quem chamou e o desfecho.
	html := esperarNaTrilha(t, u, "somar")
	for _, esperado := range []string{"pessoal", "falso", "apikey:", "ok"} {
		if !strings.Contains(html, esperado) {
			t.Errorf("a trilha não mostra %q", esperado)
		}
	}

	// 3. Nem o SSE nem a trilha carregam a chave de API em claro.
	if strings.Contains(fluxo.tudoLido(), chave) {
		t.Error("a chave de API em claro apareceu no log ao vivo")
	}
	if strings.Contains(html, chave) {
		t.Error("a chave de API em claro apareceu na tela de trilha")
	}
}

// TestUI_TrilhaRegistraDesfechoRuim prova a decisão de classificação: uma
// ferramenta que responde com IsError é uma chamada que não fez o que o cliente
// pediu, entra na trilha como erro e o filtro por resultado a encontra.
func TestUI_TrilhaRegistraDesfechoRuim(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	upstreamID := u.criarUpstream(t, "quebrado", upstreamQueFalha(t))
	u.criarEndpoint(t, "pessoal", "Pessoal", upstreamID)
	chave := u.criarChave(t, "navegador", 1)
	esperarFerramentas(t, u, "pessoal", 1)

	sessao := conectarMCP(t, u, "pessoal", chave)
	res, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{Name: "falhar"})
	if err != nil {
		t.Fatalf("tools/call falhar: erro = %v, quer nil (erro de ferramenta, não de protocolo)", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false, quer true")
	}

	// A linha entra filtrável por resultado...
	html := esperarNaTrilhaCom(t, u, "?resultado=erro", "falhar")
	if !strings.Contains(html, "quebrado") {
		t.Error("a trilha filtrada por erro não nomeia o upstream")
	}
	// ...e não aparece no filtro do desfecho oposto.
	semErro := corpo(t, u.abrir(t, webui.RotaTrilha+"?resultado=ok"))
	if strings.Contains(corpoDaTabela(semErro), "falhar") {
		t.Error("a chamada com erro aparece no filtro de resultado ok")
	}
}

// upstreamQueFalha sobe um servidor MCP de verdade com uma ferramenta que
// sempre responde com erro de ferramenta. O "falso" é o catálogo, não o
// protocolo.
func upstreamQueFalha(t *testing.T) string {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-quebrado", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "falhar", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "não achei o arquivo"}},
			}, nil
		})

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL
}

// corpoDaTabela devolve só o <tbody> da trilha. Fora dele ficam os seletores do
// filtro, que citam por desenho todo nome que já apareceu na trilha.
func corpoDaTabela(html string) string {
	i := strings.Index(html, "<tbody")
	if i < 0 {
		return ""
	}
	fim := strings.Index(html[i:], "</tbody>")
	if fim < 0 {
		return html[i:]
	}
	return html[i : i+fim]
}

// --- apoio: leitura do stream de SSE -----------------------------------------

type fluxoSSE struct {
	leitor *bufio.Reader
	lido   strings.Builder
}

// abrirFluxo conecta na rota de SSE com a sessão de admin do navegador de
// mentira.
func abrirFluxo(t *testing.T, u uiDeTeste) *fluxoSSE {
	t.Helper()

	ctx, cancelar := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url+webui.RotaLogsFluxo, http.NoBody)
	if err != nil {
		cancelar()
		t.Fatalf("montar requisição de SSE: erro = %v, quer nil", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", u.url)

	res, err := u.cliente.Do(req)
	if err != nil {
		cancelar()
		t.Fatalf("GET %s: erro = %v, quer nil", webui.RotaLogsFluxo, err)
	}
	t.Cleanup(func() {
		cancelar()
		_ = res.Body.Close()
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, quer 200", webui.RotaLogsFluxo, res.StatusCode)
	}
	if tipo := res.Header.Get("Content-Type"); tipo != "text/event-stream" {
		t.Fatalf("Content-Type = %q, quer %q", tipo, "text/event-stream")
	}
	return &fluxoSSE{leitor: bufio.NewReader(res.Body)}
}

// esperarEvento lê o stream até chegar um evento do tipo pedido e devolve os
// dados dele.
//
// Pula os outros tipos: o log do processo e a chamada de ferramenta correm no
// mesmo stream, e o teste que quer um não pode falhar por causa do outro.
func (f *fluxoSSE) esperarEvento(t *testing.T, tipo string) string {
	t.Helper()

	achado := make(chan string, 1)
	falha := make(chan error, 1)
	go func() {
		var atual struct{ tipo, dados string }
		for {
			linha, err := f.leitor.ReadString('\n')
			f.lido.WriteString(linha)
			if err != nil {
				falha <- err
				return
			}
			linha = strings.TrimRight(linha, "\r\n")
			switch {
			case strings.HasPrefix(linha, "event: "):
				atual.tipo = strings.TrimPrefix(linha, "event: ")
			case strings.HasPrefix(linha, "data: "):
				atual.dados += strings.TrimPrefix(linha, "data: ")
			case linha == "":
				if atual.tipo == tipo {
					achado <- atual.dados
					return
				}
				atual.tipo, atual.dados = "", ""
			}
		}
	}()

	select {
	case dados := <-achado:
		return dados
	case err := <-falha:
		t.Fatalf("ler o stream de SSE: erro = %v, quer nil", err)
		return ""
	case <-time.After(tetoDaTrilha):
		t.Fatalf("nenhum evento %q chegou pelo SSE em %v", tipo, tetoDaTrilha)
		return ""
	}
}

// tudoLido é o texto bruto que passou pelo stream, para a assertiva de que
// nenhum segredo trafegou por ele.
func (f *fluxoSSE) tudoLido() string { return f.lido.String() }

// --- apoio: espera pela trilha gravada ---------------------------------------

func esperarNaTrilha(t *testing.T, u uiDeTeste, agulha string) string {
	t.Helper()
	return esperarNaTrilhaCom(t, u, "", agulha)
}

// esperarNaTrilhaCom recarrega a tela de trilha até a agulha aparecer.
//
// Sonda a tela e não o banco de propósito: o que a fatia promete é que a chamada
// *aparece na tela*, e uma assertiva contra o repositório passaria mesmo se o
// handler filtrasse tudo fora.
func esperarNaTrilhaCom(t *testing.T, u uiDeTeste, query, agulha string) string {
	t.Helper()

	limite := time.After(tetoDaTrilha)
	tique := time.NewTicker(50 * time.Millisecond)
	defer tique.Stop()

	var ultimo string
	for {
		res := u.abrir(t, webui.RotaTrilha+query)
		ultimo = corpo(t, res)
		if strings.Contains(ultimo, agulha) {
			return ultimo
		}
		select {
		case <-tique.C:
		case <-limite:
			t.Fatalf("a trilha não mostrou %q em %v", agulha, tetoDaTrilha)
			return ultimo
		}
	}
}
