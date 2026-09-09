package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

// nomeComEspacos é registrado no upstream falso com um nome que o SDK aceita
// mas o protocolo não deveria — validateToolName só loga (mcp/tool.go:163).
// Serve para provar que o normalizador roda no caminho real, não só no unitário.
const (
	nomeComEspacos  = "eco com espaco"
	nomeNormalizado = "eco_com_espaco"
)

type argumentosSoma struct {
	A int `json:"a"`
	B int `json:"b"`
}

// upstreamFalso sobe um servidor MCP de verdade, do próprio go-sdk, com
// ferramentas de mentira. O "falso" é o catálogo, não o protocolo: um mock
// escrito à mão concordaria com o meu erro de entendimento do MCP.
func upstreamFalso(t *testing.T) string {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "upstream-falso", Version: "0.0.1"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "somar",
		Description: "soma dois inteiros",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in argumentosSoma) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d", in.A+in.B)}},
		}, nil, nil
	})

	srv.AddTool(
		&mcp.Tool{Name: nomeComEspacos, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "eco"}},
			}, nil
		})

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(func() {
		// CloseClientConnections antes de Close: o patchbay mantém aberto o stream
		// SSE deste upstream, e Close espera pelas requisições em curso. Sem isto,
		// a limpeza trava quando este servidor é criado depois do patchbay — a
		// ordem de t.Cleanup é a inversa da criação.
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL
}

// upstreamQuePendura aceita a conexão e nunca responde. É o cenário "upstream
// fora do ar" que mais importa: recusa de conexão é rápida, hang não.
func upstreamQuePendura(t *testing.T) string {
	t.Helper()

	solto := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-solto:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(solto)
		ts.Close()
	})
	return ts.URL
}

// patchbayDeTeste sobe um patchbay em processo apontado para urlUpstream, com um
// endpoint e uma chave de API criados pelo mesmo seed que o subcomando usa.
type patchbayDeTeste struct {
	url         string
	chave       string
	endpoint    string
	app         *Aplicacao
	sincronizou <-chan struct{}
}

func subirPatchbay(t *testing.T, urlUpstream string, timeoutMS int64) patchbayDeTeste {
	t.Helper()

	cfg := Config{
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
		PublicURL: "http://127.0.0.1:8787",
		NivelLog:  slog.LevelError,
	}
	log := slog.New(slog.DiscardHandler)
	ctx, cancelar := context.WithCancel(context.Background())
	t.Cleanup(cancelar)

	// Seed antes do boot: o gerente lê os upstreams habilitados uma vez, em
	// montar.
	st, err := store.Abrir(ctx, cfg.DataDir)
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	semeado, err := semear(ctx, st.Escrita(), OpcoesSeed{
		Endpoint:    "pessoal",
		Upstream:    "falso",
		UpstreamURL: urlUpstream,
		NomeChave:   "teste",
		TimeoutMS:   timeoutMS,
	})
	if err != nil {
		t.Fatalf("semear: erro = %v, quer nil", err)
	}
	// Um segundo endpoint sem upstream nenhum: é o alvo da chave sem escopo.
	if _, err := st.Escrita().ExecContext(ctx,
		`INSERT INTO endpoint (slug, descricao, criado_em) VALUES ('trabalho', '', 0)`); err != nil {
		t.Fatalf("criar segundo endpoint: erro = %v, quer nil", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("fechar banco do seed: erro = %v, quer nil", err)
	}

	// A origem da biblioteca aponta para um registry local e mudo: sem isto,
	// Iniciar dispararia uma varredura do registry de verdade só por subir o
	// patchbay. Ver registryMudo, em ui_test.go.
	app, err := montar(ctx, cfg, cofreDeTeste(t), log,
		ComOrigemDaBiblioteca(registryMudo(t)), ComCuradoriaDaBiblioteca(curadoriaMudaDeTeste(t)))
	if err != nil {
		t.Fatalf("montar: erro = %v, quer nil", err)
	}

	// Espera por sinal, não pelo relógio: o observador é o mesmo gancho que a UI
	// da fatia 2 usa para o log ao vivo.
	sincronizou := make(chan struct{}, 8)
	app.Observar(func() {
		select {
		case sincronizou <- struct{}{}:
		default:
		}
	})
	app.Iniciar(ctx)

	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		ts.Close()
		cancelar()
		if err := app.Fechar(); err != nil {
			t.Errorf("fechar aplicação: erro = %v, quer nil", err)
		}
	})

	return patchbayDeTeste{
		url:         ts.URL,
		chave:       semeado.ChaveClaro,
		endpoint:    semeado.EndpointSlug,
		app:         app,
		sincronizou: sincronizou,
	}
}

// conectar liga um cliente MCP de verdade do go-sdk no endpoint do patchbay.
func conectar(t *testing.T, p patchbayDeTeste, slug, chave string) *mcp.ClientSession {
	t.Helper()

	cliente := mcp.NewClient(&mcp.Implementation{Name: "cliente-de-teste", Version: "0.0.1"}, nil)
	transporte := &mcp.StreamableClientTransport{
		Endpoint:   p.url + "/mcp/" + slug,
		HTTPClient: clienteComBearer(chave),
		MaxRetries: -1, // sem retry: o teste quer ver a falha, não a insistência
	}

	ctx, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancelar)

	sessao, err := cliente.Connect(ctx, transporte, nil)
	if err != nil {
		t.Fatalf("conectar em /mcp/%s: erro = %v, quer nil", slug, err)
	}
	t.Cleanup(func() { _ = sessao.Close() })
	return sessao
}

func clienteComBearer(chave string) *http.Client {
	if chave == "" {
		return &http.Client{}
	}
	return &http.Client{Transport: bearer{chave: chave, base: http.DefaultTransport}}
}

type bearer struct {
	chave string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.chave)
	return b.base.RoundTrip(clone)
}

// TestPontaAPonta é a demonstração da fatia 1: um cliente MCP de verdade
// conecta em /mcp/pessoal com uma chave de API, vê a ferramenta de um upstream
// HTTP no tools/list e a chama com sucesso.
func TestPontaAPonta(t *testing.T) {
	t.Parallel()

	p := subirPatchbay(t, upstreamFalso(t), 5000)

	// Espera a supervisão materializar o catálogo. Sem sinal, o teste dependeria
	// do relógio.
	select {
	case <-p.sincronizou:
	case <-time.After(15 * time.Second):
		t.Fatal("catálogo não materializou em 15s")
	}

	sessao := conectar(t, p, p.endpoint, p.chave)
	ctx := context.Background()

	res, err := sessao.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: erro = %v, quer nil", err)
	}
	var nomes []string
	for _, ferramenta := range res.Tools {
		nomes = append(nomes, ferramenta.Name)
	}
	slices.Sort(nomes)
	quer := []string{nomeNormalizado, "somar"}
	if !slices.Equal(nomes, quer) {
		t.Fatalf("ferramentas = %v, quer %v", nomes, quer)
	}

	chamada, err := sessao.CallTool(ctx, &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call somar: erro = %v, quer nil", err)
	}
	if chamada.IsError {
		t.Fatalf("tools/call somar devolveu erro de ferramenta: %v", textoDe(chamada))
	}
	if texto := textoDe(chamada); texto != "42" {
		t.Errorf("resultado = %q, quer %q", texto, "42")
	}

	// A ferramenta renomeada é chamável pelo nome exposto, e o patchbay usa o
	// nome original no upstream.
	eco, err := sessao.CallTool(ctx, &mcp.CallToolParams{Name: nomeNormalizado})
	if err != nil {
		t.Fatalf("tools/call %s: erro = %v, quer nil", nomeNormalizado, err)
	}
	if eco.IsError {
		t.Fatalf("tools/call %s devolveu erro de ferramenta: %v", nomeNormalizado, textoDe(eco))
	}
	if texto := textoDe(eco); texto != "eco" {
		t.Errorf("resultado = %q, quer %q", texto, "eco")
	}
}

// TestSemChave_401 e os irmãos abaixo checam a borda de autenticação com HTTP
// cru, porque é o status e o header que importam, não o protocolo MCP.
func TestAutenticacaoNoEndpoint(t *testing.T) {
	t.Parallel()

	p := subirPatchbay(t, upstreamFalso(t), 5000)

	casos := map[string]struct {
		slug        string
		autorizacao string
		querStatus  int
		querDesafio bool
	}{
		"sem chave devolve 401 com desafio": {
			slug:        "pessoal",
			querStatus:  http.StatusUnauthorized,
			querDesafio: true,
		},
		"chave inventada devolve 401 com desafio": {
			slug:        "pessoal",
			autorizacao: "Bearer pbk_aaaabbbb_chave-que-nunca-foi-emitida",
			querStatus:  http.StatusUnauthorized,
			querDesafio: true,
		},
		"chave sem escopo para o endpoint devolve 403": {
			slug:        "trabalho",
			autorizacao: "Bearer " + p.chave,
			querStatus:  http.StatusForbidden,
			querDesafio: true,
		},
		"endpoint inexistente devolve 404": {
			slug:        "inexistente",
			autorizacao: "Bearer " + p.chave,
			querStatus:  http.StatusNotFound,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(context.Background(),
				http.MethodPost, p.url+"/mcp/"+tc.slug, http.NoBody)
			if err != nil {
				t.Fatalf("montar requisição: erro = %v, quer nil", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.autorizacao != "" {
				req.Header.Set("Authorization", tc.autorizacao)
			}

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("requisição: erro = %v, quer nil", err)
			}
			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != tc.querStatus {
				corpo, _ := io.ReadAll(res.Body)
				t.Fatalf("status = %d, quer %d (corpo: %q)", res.StatusCode, tc.querStatus, corpo)
			}
			desafio := res.Header.Get("WWW-Authenticate")
			if tc.querDesafio && desafio == "" {
				t.Error("WWW-Authenticate ausente, quer o desafio Bearer")
			}
		})
	}
}

// TestCredencialEmQueryString prova que a chave em query string vem desligada,
// e recusada com o motivo escrito em vez de ignorada em silêncio.
func TestCredencialEmQueryString(t *testing.T) {
	t.Parallel()

	p := subirPatchbay(t, upstreamFalso(t), 5000)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, p.url+"/mcp/pessoal?api_key="+p.chave, http.NoBody)
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("requisição: erro = %v, quer nil", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusBadRequest)
	}
}

// TestUpstreamForaDoAr_CatalogoVazio é o requisito da seção 11: endpoint sem
// nenhum upstream pronto devolve tools/list vazio, nunca erro, e sem esperar o
// upstream. O teto de 1 s é o que separa "degradação legível" de "o cliente
// achou que o gateway morreu".
func TestUpstreamForaDoAr_CatalogoVazio(t *testing.T) {
	t.Parallel()

	// Timeout de upstream bem acima do teto do teste: se a conexão estivesse no
	// caminho da requisição, o tools/list levaria 5 s e este teste falharia.
	p := subirPatchbay(t, upstreamQuePendura(t), 5000)

	sessao := conectar(t, p, p.endpoint, p.chave)

	ctx, cancelar := context.WithTimeout(context.Background(), time.Second)
	defer cancelar()

	res, err := sessao.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: erro = %v, quer nil (lista vazia é resposta legítima)", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("ferramentas = %d, quer 0", len(res.Tools))
	}
}

func textoDe(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if t, ok := res.Content[0].(*mcp.TextContent); ok {
		return t.Text
	}
	return fmt.Sprintf("%v", res.Content[0])
}
