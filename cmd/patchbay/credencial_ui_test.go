package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

const (
	bearerNaTela = "sk-bearer-cadastrado-pela-tela"
	headerNaTela = "valor-de-header-cadastrado-pela-tela"
)

// upstreamComEspiao é o servidor MCP de verdade com um espião de headers na
// frente. É ele que diz o que o upstream real receberia.
func upstreamComEspiao(t *testing.T) (string, func() http.Header) {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-espiao", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "eco", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})

	var (
		mu       sync.Mutex
		recebido http.Header
	)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if recebido == nil {
			recebido = r.Header.Clone()
		}
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	return ts.URL, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return recebido
	}
}

// TestUI_CredencialCadastradaPelaTelaChegaAoUpstream é a demonstração da fatia 6
// ponta a ponta: o admin digita bearer e header no formulário, o valor some da
// tela, e o upstream recebe os dois headers na conexão seguinte.
func TestUI_CredencialCadastradaPelaTelaChegaAoUpstream(t *testing.T) {
	t.Parallel()

	alvo, headersRecebidos := upstreamComEspiao(t)
	u := subirUI(t)
	u.setup(t)

	res := u.enviarForm(t, webui.RotaUpstreams, url.Values{
		"nome":         {"com-credencial"},
		"url":          {alvo},
		"timeout_ms":   {"5000"},
		"habilitado":   {"1"},
		"bearer":       {bearerNaTela},
		"header_nome":  {"X-Api-Key", ""},
		"header_valor": {headerNaTela, ""},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar upstream: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
	}
	id := idDoDestino(t, res)

	// A tela de detalhe diz "definido", e não o valor.
	detalhe := corpo(t, u.abrir(t, webui.RotaUpstreams+"/"+strconv.FormatInt(id, 10)))
	for _, segredo := range []string{bearerNaTela, headerNaTela} {
		if strings.Contains(detalhe, segredo) {
			t.Errorf("tela de detalhe reexibiu o segredo %q", segredo)
		}
	}
	if !strings.Contains(detalhe, "definido") {
		t.Error("tela de detalhe não diz que a credencial está definida")
	}
	if !strings.Contains(detalhe, "X-Api-Key") {
		t.Error("tela de detalhe não lista o header estático cadastrado")
	}

	// Nem a tela de edição.
	edicao := corpo(t, u.abrir(t, webui.RotaUpstreams+"/"+strconv.FormatInt(id, 10)+"/editar"))
	for _, segredo := range []string{bearerNaTela, headerNaTela} {
		if strings.Contains(edicao, segredo) {
			t.Errorf("formulário de edição reexibiu o segredo %q", segredo)
		}
	}

	// E o upstream recebeu as duas credenciais, em header.
	esperarUpstreamPronto(t, u, id)
	recebido := headersRecebidos()
	if recebido == nil {
		t.Fatal("nenhuma requisição chegou ao upstream")
	}
	if got, quer := recebido.Get("Authorization"), "Bearer "+bearerNaTela; got != quer {
		t.Errorf("Authorization = %q, quer %q", got, quer)
	}
	if got := recebido.Get("X-Api-Key"); got != headerNaTela {
		t.Errorf("X-Api-Key = %q, quer %q", got, headerNaTela)
	}
}

// TestUI_LimparCredencialTiraOHeaderDaSaida fecha o ciclo do formulário: o campo
// em branco mantém, o "limpar" apaga.
func TestUI_LimparCredencialTiraOHeaderDaSaida(t *testing.T) {
	t.Parallel()

	alvo, _ := upstreamComEspiao(t)
	u := subirUI(t)
	u.setup(t)

	res := u.enviarForm(t, webui.RotaUpstreams, url.Values{
		"nome":         {"para-limpar"},
		"url":          {alvo},
		"timeout_ms":   {"5000"},
		"habilitado":   {"1"},
		"bearer":       {bearerNaTela},
		"header_nome":  {"X-Api-Key"},
		"header_valor": {headerNaTela},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar upstream: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	id := idDoDestino(t, res)
	rota := webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)

	// Uma edição que só mexe no timeout: as credenciais ficam.
	res = u.enviarForm(t, rota, url.Values{
		"nome":         {"para-limpar"},
		"url":          {alvo},
		"timeout_ms":   {"7000"},
		"habilitado":   {"1"},
		"bearer":       {""},
		"header_nome":  {"X-Api-Key"},
		"header_valor": {""},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editar upstream: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	if !strings.Contains(corpo(t, u.abrir(t, rota)), "X-Api-Key") {
		t.Error("o header sumiu de uma edição que não o tocou")
	}

	// Agora limpando os dois de propósito.
	res = u.enviarForm(t, rota, url.Values{
		"nome":          {"para-limpar"},
		"url":           {alvo},
		"timeout_ms":    {"7000"},
		"habilitado":    {"1"},
		"bearer_limpar": {"1"},
		"header_nome":   {"X-Api-Key"},
		"header_valor":  {""},
		"header_limpar": {"X-Api-Key"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("limpar credenciais: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	detalhe := corpo(t, u.abrir(t, rota))
	if strings.Contains(detalhe, "X-Api-Key") {
		t.Error("o header continua listado depois de limpar")
	}
	if !strings.Contains(detalhe, "não definido") {
		t.Error("a tela não diz que o bearer deixou de existir")
	}
}

// TestUI_HeaderInvalidoRecusadoSemPerderOEstado prova que a validação recusa o
// que viraria header injetado, e que o formulário volta dizendo o que já existe.
func TestUI_HeaderInvalidoRecusadoSemPerderOEstado(t *testing.T) {
	t.Parallel()

	alvo, _ := upstreamComEspiao(t)
	u := subirUI(t)
	u.setup(t)

	id := u.criarUpstream(t, "com-header", alvo)
	rota := webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)

	res := u.enviarForm(t, rota, url.Values{
		"nome":         {"com-header"},
		"url":          {alvo},
		"timeout_ms":   {"5000"},
		"habilitado":   {"1"},
		"header_nome":  {"X-Api Key"},
		"header_valor": {"qualquer"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusUnprocessableEntity)
	}
	if texto := corpo(t, res); !strings.Contains(texto, "Nome de header inválido") {
		t.Errorf("corpo não explica o erro do header; corpo: %q", texto)
	}

	// Authorization pela tabela de headers também é recusado: ele é montado pelo
	// campo de bearer, e aceitar os dois deixaria um sobrescrevendo o outro.
	res = u.enviarForm(t, rota, url.Values{
		"nome":         {"com-header"},
		"url":          {alvo},
		"timeout_ms":   {"5000"},
		"habilitado":   {"1"},
		"header_nome":  {"Authorization"},
		"header_valor": {"Bearer x"},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status com Authorization = %d, quer %d", res.StatusCode, http.StatusUnprocessableEntity)
	}
}

// esperarUpstreamPronto espera a sessão do upstream abrir, por sinal e não pelo
// relógio: a supervisão avisa a aplicação a cada mudança de catálogo.
func esperarUpstreamPronto(t *testing.T, u uiDeTeste, id int64) {
	t.Helper()

	limite := time.After(20 * time.Second)
	for {
		if s, ok := u.app.gerente.Situacao(id); ok && s.Estado == upstream.EstadoPronto {
			return
		}
		select {
		case <-u.sincronizou:
		case <-limite:
			s, _ := u.app.gerente.Situacao(id)
			t.Fatalf("upstream %d não ficou pronto em 20s (estado = %q, erro = %q)",
				id, s.Estado, s.UltimoErro)
		}
	}
}
