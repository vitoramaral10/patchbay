package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/vitoramaral10/patchbay/internal/authsrv"
)

// Os testes da fatia 11: o cliente que se registra sozinho.
//
// Dois caminhos, e os dois com o cliente OAuth real do go-sdk — o mesmo
// auth.AuthorizationCodeHandler que o Claude Code usa contra um servidor de
// terceiro. Um mock escrito à mão concordaria com o meu erro de entendimento da
// ordem CIMD → pré-registrado → DCR que o SDK implementa
// (auth/authorization_code.go:526-556).

// portaEfemera reserva e devolve uma porta livre de loopback.
//
// Reservar e soltar é de propósito: o que o teste precisa é de um número de porta
// que o sistema tenha escolhido, como um cliente nativo RFC 8252 faria ao abrir o
// listener. Ninguém conecta nela — o fetcher faz o papel do navegador, e é ele
// que devolve o code em vez de o AS redirecionar de verdade.
func portaEfemera(t *testing.T) int {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reservar porta efêmera: erro = %v, quer nil", err)
	}
	porta := lis.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // tcp sempre devolve *net.TCPAddr
	if err := lis.Close(); err != nil {
		t.Fatalf("soltar porta efêmera: erro = %v, quer nil", err)
	}
	return porta
}

// navegadorDoAdmin devolve o AuthorizationCodeFetcher que faz o papel do
// navegador do administrador: abre a URL de autorização com o cookie de sessão,
// confere que a tela de consentimento apareceu e devolve o code do redirect.
//
// Conta as visitas: é como o teste prova que o fluxo humano aconteceu uma vez, e
// não zero (token vindo de outro lugar) nem duas (registro refeito no meio).
func (p patchbayOAuth) navegadorDoAdmin(t *testing.T, visitas *int, mostra ...string) auth.AuthorizationCodeFetcher {
	t.Helper()

	return func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		*visitas++

		alvo, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		tela := p.enviar(t, http.MethodGet, args.URL, nil, "", true)
		if tela.StatusCode != http.StatusOK {
			corpo, _ := io.ReadAll(tela.Body)
			t.Fatalf("tela de consentimento: status = %d, quer 200 (corpo: %.300q)",
				tela.StatusCode, corpo)
		}
		html, err := io.ReadAll(tela.Body)
		if err != nil {
			return nil, err
		}
		// O hostname do redirect_uri em destaque é a única defesa contra um
		// documento de CIMD que se apresente como outro cliente: qualquer um pode
		// publicar um.
		for _, esperado := range mostra {
			if !strings.Contains(string(html), esperado) {
				t.Errorf("tela de consentimento não mostra %q", esperado)
			}
		}

		res := p.consentir(t, alvo.Query(), "aceitar")
		if res.StatusCode != http.StatusFound {
			corpo, _ := io.ReadAll(res.Body)
			t.Fatalf("consentimento: status = %d, quer 302 (corpo: %.300q)", res.StatusCode, corpo)
		}
		loc, err := url.Parse(res.Header.Get("Location"))
		if err != nil {
			return nil, err
		}
		q := loc.Query()
		return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
}

// conectarComOAuth liga um cliente MCP a /mcp/{slug} usando o manipulador dado e
// chama uma ferramenta, que é a prova de que o token vale de verdade.
func (p patchbayOAuth) conectarComOAuth(t *testing.T, manipulador *auth.AuthorizationCodeHandler) {
	t.Helper()

	ctx, cancelar := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancelar)

	sessao, err := mcp.NewClient(&mcp.Implementation{Name: "cliente-dinamico-de-teste", Version: "0.0.1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:     p.urlPublica + "/mcp/" + p.endpointPessoal,
			OAuthHandler: manipulador,
			MaxRetries:   -1,
		}, nil)
	if err != nil {
		t.Fatalf("conectar com OAuth em /mcp/%s: erro = %v, quer nil", p.endpointPessoal, err)
	}
	t.Cleanup(func() { _ = sessao.Close() })

	chamada, err := sessao.CallTool(ctx, &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call com token do registro dinâmico: erro = %v, quer nil", err)
	}
	if got := textoDe(chamada); got != "42" {
		t.Errorf("resultado = %q, quer %q", got, "42")
	}
}

// --- DCR ---

// TestDCRPontaAPonta é metade do critério de pronto da fatia 11: o Claude Code
// conecta com DCR.
//
// O handler do go-sdk só cai em DCR quando nada mais está configurado e o AS
// anuncia registration_endpoint (auth/authorization_code.go:554), então este
// teste é também o que prova que o anúncio na metadata está certo. A redirect_uri
// é loopback em porta efêmera, como manda o RFC 8252 para cliente nativo.
func TestDCRPontaAPonta(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	porta := portaEfemera(t)
	loopback := "http://127.0.0.1:" + strconv.Itoa(porta) + "/callback"

	var visitas int
	manipulador, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		// Só DCR: sem PreregisteredClient e sem CIMD, o handler não tem outro
		// caminho a tentar.
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName: "Claude Code de teste",
				RedirectURIs: []string{
					loopback,
					"http://localhost:" + strconv.Itoa(porta) + "/callback",
				},
				TokenEndpointAuthMethod: "none",
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
			},
		},
		RedirectURL: loopback,
		AuthorizationCodeFetcher: p.navegadorDoAdmin(t, &visitas,
			"Claude Code de teste", "127.0.0.1",
			// O cliente que se registrou sozinho tem de chegar marcado como tal:
			// esta tela é a primeira vez que alguém olha para ele.
			"Este cliente não foi cadastrado por você."),
		RequestRefreshToken: true,
	})
	if err != nil {
		t.Fatalf("montar AuthorizationCodeHandler com DCR: erro = %v, quer nil", err)
	}

	p.conectarComOAuth(t, manipulador)
	if visitas != 1 {
		t.Errorf("fluxo de autorização executado %d vezes, quer 1", visitas)
	}

	// O cliente tem de aparecer na lista de admin, distinguível do cadastrado à
	// mão: origem, data e allowlist. Sem isso, uma linha que ninguém pediu vira
	// mistério em três meses.
	clientes, err := p.app.repoOAuth.TodosClientes(context.Background())
	if err != nil {
		t.Fatalf("listar clientes: erro = %v, quer nil", err)
	}
	var achou bool
	for _, c := range clientes {
		if c.Tipo != authsrv.TipoDCR {
			continue
		}
		achou = true
		switch {
		case c.Nome != "Claude Code de teste":
			t.Errorf("nome = %q, quer o client_name do registro", c.Nome)
		case c.Confidencial:
			t.Error("cliente de DCR com token_endpoint_auth_method none saiu confidencial")
		case !c.EscopoAberto:
			t.Error("cliente de DCR sem escopo aberto não conseguiria pedir endpoint nenhum")
		case c.Origem == "":
			t.Error("origem vazia: a tela não teria como dizer de onde o registro veio")
		case c.CriadoEm.IsZero():
			t.Error("criado_em zerado")
		case !slices.Contains(c.RedirectURIs, loopback):
			t.Errorf("redirect_uris = %v, quer conter %q", c.RedirectURIs, loopback)
		}
	}
	if !achou {
		t.Fatal("nenhum cliente de DCR na lista de admin depois do fluxo")
	}
}

// TestRegistroDCRRecusa cobre o que o registration endpoint tem de recusar antes
// de gravar linha nenhuma.
func TestRegistroDCRRecusa(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	casos := map[string]struct {
		corpo      any
		tipo       string
		querStatus int
		querCodigo string
	}{
		"sem redirect_uris": {
			corpo:      map[string]any{"client_name": "sem redirect"},
			querStatus: http.StatusBadRequest, querCodigo: "invalid_redirect_uri",
		},
		"redirect http fora de loopback": {
			corpo:      map[string]any{"redirect_uris": []string{"http://mau.test/cb"}},
			querStatus: http.StatusBadRequest, querCodigo: "invalid_redirect_uri",
		},
		"redirect com fragmento": {
			corpo:      map[string]any{"redirect_uris": []string{"https://mau.test/cb#x"}},
			querStatus: http.StatusBadRequest, querCodigo: "invalid_redirect_uri",
		},
		"grant que o AS não implementa": {
			corpo: map[string]any{
				"redirect_uris": []string{"https://ok.test/cb"},
				"grant_types":   []string{"client_credentials"},
			},
			querStatus: http.StatusBadRequest, querCodigo: "invalid_client_metadata",
		},
		"token_endpoint_auth_method desconhecido": {
			corpo: map[string]any{
				"redirect_uris":              []string{"https://ok.test/cb"},
				"token_endpoint_auth_method": "private_key_jwt",
			},
			querStatus: http.StatusBadRequest, querCodigo: "invalid_client_metadata",
		},
		"corpo em form-urlencoded": {
			corpo: map[string]any{"redirect_uris": []string{"https://ok.test/cb"}},
			tipo:  "application/x-www-form-urlencoded",
			// Content-Type errado é invalid_request, e não 415: um status fora do
			// RFC deixa o cliente sem saber o que fazer.
			querStatus: http.StatusBadRequest, querCodigo: "invalid_request",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			tipo := tc.tipo
			if tipo == "" {
				tipo = "application/json"
			}
			status, corpo := p.registrar(t, tc.corpo, tipo)
			if status != tc.querStatus {
				t.Errorf("status = %d, quer %d (corpo: %v)", status, tc.querStatus, corpo)
			}
			if got := texto(t, corpo, "error"); got != tc.querCodigo {
				t.Errorf("error = %q, quer %q", got, tc.querCodigo)
			}
		})
	}
}

// TestRegistroDCRSegredoSoComoHash prova as duas metades da regra de segredo:
// o cliente que pede autenticação com segredo recebe um, e o que fica no banco é
// só o hash mais o prefixo visível.
func TestRegistroDCRSegredoSoComoHash(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	status, corpo := p.registrar(t, map[string]any{
		"client_name":                "serviço confidencial por dcr",
		"redirect_uris":              []string{"https://servico.test/cb"},
		"token_endpoint_auth_method": "client_secret_basic",
	}, "application/json")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, quer 201 (corpo: %v)", status, corpo)
	}
	segredo := texto(t, corpo, "client_secret")
	clientID := texto(t, corpo, "client_id")
	switch {
	case clientID == "":
		t.Fatal("resposta sem client_id")
	case segredo == "":
		t.Fatal("client_secret_basic pedido e nenhum segredo emitido")
	case texto(t, corpo, "token_endpoint_auth_method") != "client_secret_basic":
		t.Errorf("token_endpoint_auth_method = %q, quer client_secret_basic",
			texto(t, corpo, "token_endpoint_auth_method"))
	}

	// O que o banco guarda é o hash. A prova é indireta e é a única possível sem
	// espiar coluna: o segredo em claro autentica, um segredo parecido não.
	cliente, err := p.app.repoOAuth.ClientePorClientID(context.Background(), clientID)
	if err != nil {
		t.Fatalf("ler cliente registrado: erro = %v, quer nil", err)
	}
	if !cliente.Confidencial {
		t.Error("cliente que pediu segredo saiu público")
	}
	if _, err := p.app.oauth.AutenticarCliente(context.Background(), clientID, segredo); err != nil {
		t.Errorf("autenticar com o segredo emitido: erro = %v, quer nil", err)
	}
	if _, err := p.app.oauth.AutenticarCliente(context.Background(), clientID, segredo+"x"); err == nil {
		t.Error("autenticou com segredo errado")
	}

	// E o prefixo visível é o começo do próprio segredo, que é o que permite a
	// tela dizer qual credencial é qual sem guardá-la.
	if !strings.HasPrefix(segredo, cliente.SegredoPrefixo) {
		t.Errorf("prefixo %q não é o começo do segredo emitido", cliente.SegredoPrefixo)
	}
}

// TestRegistroDCRTemTetoPorOrigem prova que a tabela não cresce sem fim.
func TestRegistroDCRTemTetoPorOrigem(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	corpo := map[string]any{
		"redirect_uris":              []string{"https://inflacao.test/cb"},
		"token_endpoint_auth_method": "none",
	}
	// O teto por origem é o menor dos dois, e o balde por minuto é do mesmo
	// tamanho: passar de qualquer um deles devolve 429, que é a resposta certa.
	var recusas int
	for i := 0; i < authsrv.TetoRegistroPorOrigem+2; i++ {
		status, resposta := p.registrar(t, corpo, "application/json")
		if status == http.StatusTooManyRequests {
			recusas++
			if got := texto(t, resposta, "error"); got == "" {
				t.Error("recusa por teto sem código de erro no corpo")
			}
			continue
		}
		if status != http.StatusCreated {
			t.Fatalf("registro %d: status = %d, quer 201 ou 429 (corpo: %v)", i, status, resposta)
		}
	}
	if recusas == 0 {
		t.Errorf("%d registros seguidos e nenhuma recusa: o teto não está valendo",
			authsrv.TetoRegistroPorOrigem+2)
	}
}

// TestRegistroDCRTetoSemCorrida prova que o teto por origem não estoura sob
// concorrência.
//
// Antes da correção, a contagem rodava numa consulta de leitura separada do
// INSERT: duas requisições concorrentes liam o mesmo total abaixo do teto e as
// duas gravavam, furando-o. Contar dentro da mesma transação de escrita — que
// no SQLite tem um escritor só — fecha essa janela: a segunda só enxerga a
// contagem depois que a primeira commitou. O teste dispara mais tentativas
// concorrentes do que o teto permite e confere que nunca mais que o teto é
// aceito.
func TestRegistroDCRTetoSemCorrida(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	corpo, err := json.Marshal(map[string]any{
		"redirect_uris":              []string{"https://concorrencia.test/cb"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		t.Fatalf("serializar corpo do registro: erro = %v, quer nil", err)
	}

	const tentativas = authsrv.TetoRegistroPorOrigem + 10
	var aceitos atomic.Int64
	var grupo sync.WaitGroup
	for range tentativas {
		grupo.Add(1)
		go func() {
			defer grupo.Done()

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				p.urlPublica+authsrv.RotaRegistrar, bytes.NewReader(corpo))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := p.cliente.Do(req)
			if err != nil {
				return
			}
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode == http.StatusCreated {
				aceitos.Add(1)
			}
		}()
	}
	grupo.Wait()

	if got := aceitos.Load(); got > int64(authsrv.TetoRegistroPorOrigem) {
		t.Errorf("registros aceitos concorrentemente = %d, quer no máximo %d (teto por origem)",
			got, authsrv.TetoRegistroPorOrigem)
	}
}

// registrar chama o registration endpoint e devolve status e corpo decodificado.
func (p patchbayOAuth) registrar(t *testing.T, corpo any, tipo string) (int, map[string]any) {
	t.Helper()

	bruto, err := json.Marshal(corpo)
	if err != nil {
		t.Fatalf("serializar corpo do registro: erro = %v, quer nil", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.urlPublica+authsrv.RotaRegistrar, bytes.NewReader(bruto))
	if err != nil {
		t.Fatalf("montar requisição de registro: erro = %v, quer nil", err)
	}
	req.Header.Set("Content-Type", tipo)

	res, err := p.cliente.Do(req)
	if err != nil {
		t.Fatalf("registro: erro = %v, quer nil", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })

	var decodificado map[string]any
	lido, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ler resposta do registro: erro = %v, quer nil", err)
	}
	if len(lido) > 0 {
		if err := json.Unmarshal(lido, &decodificado); err != nil {
			t.Fatalf("resposta do registro não é JSON: %v (%.200q)", err, lido)
		}
	}
	return res.StatusCode, decodificado
}

// --- CIMD ---

// servidorCIMD sobe o servidor https em processo que publica o documento de
// client id, e devolve a URL do documento e o buscador que o alcança.
//
// O buscador de teste é o buscador de verdade com duas opções: o transporte do
// httptest, para confiar no certificado que ele emitiu, e um destino permissivo,
// porque o httptest escuta em 127.0.0.1 — que é exatamente o que o guarda de
// SSRF bloqueia, e o que TestBuscadorCIMD_GuardaDeDestinoLigado, em
// internal/authsrv, prova que ele bloqueia. Esquema, redirect, Content-Type e
// teto de corpo continuam sendo os de produção.
func servidorCIMD(t *testing.T, doc func(urlDoDocumento string) any) (string, *authsrv.BuscadorCIMD, *int) {
	t.Helper()

	var visitas int
	var urlDocumento string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		visitas++
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(doc(urlDocumento)); err != nil {
			t.Errorf("escrever documento de cimd: erro = %v, quer nil", err)
		}
	}))
	t.Cleanup(ts.Close)
	urlDocumento = ts.URL + "/.well-known/oauth-client"

	transporte, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("o cliente do httptest não usa *http.Transport")
	}
	buscador := authsrv.NovoBuscadorCIMD(slog.New(slog.DiscardHandler),
		authsrv.ComTransporteCIMD(transporte),
		authsrv.ComDestinoCIMD(func(netip.Addr) error { return nil }),
	)
	return urlDocumento, buscador, &visitas
}

// TestCIMDPontaAPonta é a outra metade do critério: um cliente que só tem CIMD
// conecta.
//
// O handler do go-sdk escolhe CIMD quando ele está configurado *e* o AS anuncia
// client_id_metadata_document_supported (auth/authorization_code.go:528-534). O
// client_id que viaja no /authorize é a própria URL do documento.
func TestCIMDPontaAPonta(t *testing.T) {
	t.Parallel()

	porta := portaEfemera(t)
	loopback := "http://127.0.0.1:" + strconv.Itoa(porta) + "/callback"

	urlDocumento, buscador, visitasDoc := servidorCIMD(t, func(urlDoc string) any {
		return map[string]any{
			"client_id":   urlDoc,
			"client_name": "cliente de CIMD de teste",
			"redirect_uris": []string{
				"http://127.0.0.1/callback",
				"http://localhost/callback",
			},
			"token_endpoint_auth_method": "none",
			"grant_types":                []string{"authorization_code", "refresh_token"},
			"response_types":             []string{"code"},
		}
	})

	p := subirPatchbayOAuth(t, upstreamFalso(t), ComBuscadorCIMD(buscador))
	p.esperarCatalogo(t)

	var visitas int
	manipulador, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: urlDocumento},
		// O documento declara a porta em branco; o cliente redireciona para a
		// porta efêmera de verdade. É a regra sem-porta do RFC 8252 §7.3 sendo
		// exercitada dentro do fluxo, e não só em teste de unidade.
		RedirectURL: loopback,
		AuthorizationCodeFetcher: p.navegadorDoAdmin(t, &visitas,
			"cliente de CIMD de teste", "127.0.0.1",
			// O hostname do client_id em destaque é a outra metade da defesa: o
			// client_name vem do documento e qualquer um pode publicar um.
			"Documento publicado em",
			"Este cliente não foi cadastrado por você."),
		RequestRefreshToken: true,
	})
	if err != nil {
		t.Fatalf("montar AuthorizationCodeHandler com CIMD: erro = %v, quer nil", err)
	}

	p.conectarComOAuth(t, manipulador)
	if visitas != 1 {
		t.Errorf("fluxo de autorização executado %d vezes, quer 1", visitas)
	}
	// Uma busca no /authorize; a troca no /token vem do cache. Mais que isso
	// significaria requisição de saída por requisição de protocolo.
	if *visitasDoc != 1 {
		t.Errorf("documento de cimd buscado %d vezes, quer 1", *visitasDoc)
	}

	// A linha de cache tem de existir, marcada como cache e com TTL — não como
	// registro. Confundir os dois é o que faz o cache virar cliente permanente.
	cliente, err := p.app.repoOAuth.ClientePorClientID(context.Background(), urlDocumento)
	if err != nil {
		t.Fatalf("ler cache de cimd: erro = %v, quer nil", err)
	}
	switch {
	case cliente.Tipo != authsrv.TipoCIMD:
		t.Errorf("tipo = %q, quer %q", cliente.Tipo, authsrv.TipoCIMD)
	case cliente.ExpiraEm.IsZero():
		t.Error("cache de cimd sem expira_em: seria registro permanente")
	case cliente.Confidencial:
		t.Error("cliente de cimd é público por definição e saiu confidencial")
	case !cliente.EscopoAberto:
		t.Error("cliente de cimd sem escopo aberto não conseguiria pedir endpoint nenhum")
	case !slices.Contains(cliente.RedirectURIs, "http://127.0.0.1/callback"):
		t.Errorf("redirect_uris = %v, quer a do documento", cliente.RedirectURIs)
	}
}

// TestCIMDRecusaDocumentoQueNaoFecha cobre as recusas do lado do AS que não são
// de rede: documento que se apresenta como outro cliente, e redirect_uri pedida
// que não está no documento.
func TestCIMDRecusaDocumentoQueNaoFecha(t *testing.T) {
	t.Parallel()

	t.Run("client_id do documento é de outro cliente", func(t *testing.T) {
		t.Parallel()

		// Qualquer um pode publicar um documento; a amarração que impede um se
		// apresentar como outro é o client_id de dentro ser igual à URL de fora.
		urlDocumento, buscador, _ := servidorCIMD(t, func(string) any {
			return map[string]any{
				"client_id":     "https://claude.ai/.well-known/oauth-client",
				"redirect_uris": []string{"http://127.0.0.1/callback"},
			}
		})
		p := subirPatchbayOAuth(t, upstreamFalso(t), ComBuscadorCIMD(buscador))

		q := p.pedidoPadrao()
		q.Set("client_id", urlDocumento)
		q.Set("redirect_uri", "http://127.0.0.1:1455/callback")
		res := p.enviar(t, http.MethodGet,
			p.urlPublica+authsrv.RotaAutorizar+"?"+q.Encode(), nil, "", true)

		// Erro de client_id não pode virar redirect (RFC 6749 §4.1.2.1): ele
		// fica na tela do admin.
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, quer 401", res.StatusCode)
		}
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("Location = %q, quer vazio: client_id inválido não redireciona", loc)
		}
	})

	t.Run("redirect_uri fora do documento", func(t *testing.T) {
		t.Parallel()

		urlDocumento, buscador, _ := servidorCIMD(t, func(urlDoc string) any {
			return map[string]any{
				"client_id":     urlDoc,
				"redirect_uris": []string{"http://127.0.0.1/callback"},
			}
		})
		p := subirPatchbayOAuth(t, upstreamFalso(t), ComBuscadorCIMD(buscador))

		q := p.pedidoPadrao()
		q.Set("client_id", urlDocumento)
		q.Set("redirect_uri", "https://mau.test/callback")
		res := p.enviar(t, http.MethodGet,
			p.urlPublica+authsrv.RotaAutorizar+"?"+q.Encode(), nil, "", true)

		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, quer 400", res.StatusCode)
		}
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("Location = %q, quer vazio: redirect fora da allowlist não redireciona", loc)
		}
	})

	t.Run("cliente de cimd revogado não é rebuscado", func(t *testing.T) {
		t.Parallel()

		urlDocumento, buscador, visitasDoc := servidorCIMD(t, func(urlDoc string) any {
			return map[string]any{
				"client_id":     urlDoc,
				"redirect_uris": []string{"http://127.0.0.1/callback"},
			}
		})
		p := subirPatchbayOAuth(t, upstreamFalso(t), ComBuscadorCIMD(buscador))
		ctx := context.Background()

		q := p.pedidoPadrao()
		q.Set("client_id", urlDocumento)
		q.Set("redirect_uri", "http://127.0.0.1:1455/callback")
		alvo := p.urlPublica + authsrv.RotaAutorizar + "?" + q.Encode()

		if res := p.enviar(t, http.MethodGet, alvo, nil, "", true); res.StatusCode != http.StatusOK {
			t.Fatalf("primeira autorização: status = %d, quer 200", res.StatusCode)
		}
		cliente, err := p.app.repoOAuth.ClientePorClientID(ctx, urlDocumento)
		if err != nil {
			t.Fatalf("ler cache de cimd: erro = %v, quer nil", err)
		}
		if err := p.app.repoOAuth.RevogarCliente(ctx, cliente.ID, time.Now()); err != nil {
			t.Fatalf("revogar cliente: erro = %v, quer nil", err)
		}

		antes := *visitasDoc
		res := p.enviar(t, http.MethodGet, alvo, nil, "", true)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status depois de revogar = %d, quer 401", res.StatusCode)
		}
		// Sem esta assertiva, revogar um cliente de CIMD na tela duraria até o
		// TTL vencer e o documento voltar sozinho.
		if *visitasDoc != antes {
			t.Errorf("documento buscado de novo depois da revogação (%d → %d)", antes, *visitasDoc)
		}
	})

	t.Run("limpeza não revive cliente de cimd revogado", func(t *testing.T) {
		t.Parallel()

		urlDocumento, buscador, visitasDoc := servidorCIMD(t, func(urlDoc string) any {
			return map[string]any{
				"client_id":     urlDoc,
				"redirect_uris": []string{"http://127.0.0.1/callback"},
			}
		})
		p := subirPatchbayOAuth(t, upstreamFalso(t), ComBuscadorCIMD(buscador))
		ctx := context.Background()

		q := p.pedidoPadrao()
		q.Set("client_id", urlDocumento)
		q.Set("redirect_uri", "http://127.0.0.1:1455/callback")
		alvo := p.urlPublica + authsrv.RotaAutorizar + "?" + q.Encode()

		if res := p.enviar(t, http.MethodGet, alvo, nil, "", true); res.StatusCode != http.StatusOK {
			t.Fatalf("primeira autorização: status = %d, quer 200", res.StatusCode)
		}
		cliente, err := p.app.repoOAuth.ClientePorClientID(ctx, urlDocumento)
		if err != nil {
			t.Fatalf("ler cache de cimd: erro = %v, quer nil", err)
		}
		if err := p.app.repoOAuth.RevogarCliente(ctx, cliente.ID, time.Now()); err != nil {
			t.Fatalf("revogar cliente: erro = %v, quer nil", err)
		}

		// Corte bem generoso — 24h à frente, muito além do TTL de uma hora do
		// CIMD — para garantir que a varredura alcançaria um cache comum
		// vencido e sem token. A linha revogada é lápide, não cache morto: se
		// LimparCacheCIMD a apagasse, resolverCIMD não acharia mais nada em
		// ClienteMesmoRevogado e rebuscaria o documento, recriando o cliente
		// que o admin acabou de revogar.
		if err := p.app.repoOAuth.LimparCacheCIMD(ctx, time.Now().Add(24*time.Hour)); err != nil {
			t.Fatalf("limpar cache de cimd: erro = %v, quer nil", err)
		}

		antes := *visitasDoc
		res := p.enviar(t, http.MethodGet, alvo, nil, "", true)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status depois da limpeza = %d, quer 401: a lápide não devia ter sumido", res.StatusCode)
		}
		if *visitasDoc != antes {
			t.Errorf("documento buscado de novo depois da limpeza (%d → %d)", antes, *visitasDoc)
		}
	})
}

// --- loopback ignorando a porta ---

// TestRedirectLoopbackIgnoraPorta é o terceiro critério da fatia: a porta sai da
// comparação em loopback, e continua valendo em tudo o mais.
//
// O teste é ponta a ponta e não de unidade porque o que importa é o Location de
// verdade: o código tem de ir para a porta que o cliente pediu, não para a que
// ele registrou.
func TestRedirectLoopbackIgnoraPorta(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	portaRegistrada := portaEfemera(t)
	registrada127 := "http://127.0.0.1:" + strconv.Itoa(portaRegistrada) + "/callback"
	registradaLocal := "http://localhost:" + strconv.Itoa(portaRegistrada) + "/callback"

	status, corpo := p.registrar(t, map[string]any{
		"client_name":                "cliente nativo de teste",
		"redirect_uris":              []string{registrada127, registradaLocal},
		"token_endpoint_auth_method": "none",
	}, "application/json")
	if status != http.StatusCreated {
		t.Fatalf("registro: status = %d, quer 201 (corpo: %v)", status, corpo)
	}
	clientID := texto(t, corpo, "client_id")

	// A porta pedida é outra: é o cenário real, em que o cliente nativo reabre o
	// listener e o sistema lhe dá uma porta diferente da do registro.
	outraPorta := strconv.Itoa(portaEfemera(t))

	casos := map[string]struct {
		redirect string
		querCasa bool
	}{
		"127.0.0.1 na porta registrada":    {redirect: registrada127, querCasa: true},
		"127.0.0.1 em qualquer porta":      {redirect: "http://127.0.0.1:" + outraPorta + "/callback", querCasa: true},
		"127.0.0.1 sem porta":              {redirect: "http://127.0.0.1/callback", querCasa: true},
		"localhost em qualquer porta":      {redirect: "http://localhost:" + outraPorta + "/callback", querCasa: true},
		"127.0.0.1 com outro caminho":      {redirect: "http://127.0.0.1:" + outraPorta + "/outro"},
		"https em loopback não é loopback": {redirect: "https://127.0.0.1:" + outraPorta + "/callback"},
		// A folga é só de loopback: host externo com porta trocada continua
		// recusado, e é isso que impede a regra de virar redirecionador aberto.
		"host externo em outra porta": {redirect: "http://exemplo.test:" + outraPorta + "/callback"},
		"host externo em https":       {redirect: "https://exemplo.test/callback"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			q := p.pedidoPadrao()
			q.Set("client_id", clientID)
			q.Set("redirect_uri", tc.redirect)
			res := p.consentir(t, q, "aceitar")

			if !tc.querCasa {
				// redirect_uri fora da allowlist não pode virar redirecionamento:
				// seria entregar o code a um destino que o AS não reconhece.
				if res.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, quer 400", res.StatusCode)
				}
				if loc := res.Header.Get("Location"); loc != "" {
					t.Errorf("Location = %q, quer vazio", loc)
				}
				return
			}

			codigo := codigoDe(t, res)
			if codigo == "" {
				t.Fatal("nenhum code no redirect")
			}
			// O destino é a porta que o cliente pediu, e não a que ele registrou.
			loc, err := url.Parse(res.Header.Get("Location"))
			if err != nil {
				t.Fatalf("Location inválido: erro = %v, quer nil", err)
			}
			pedida, err := url.Parse(tc.redirect)
			if err != nil {
				t.Fatalf("redirect de teste inválido: erro = %v, quer nil", err)
			}
			if loc.Host != pedida.Host {
				t.Errorf("host do redirect = %q, quer %q", loc.Host, pedida.Host)
			}

			// E o code só vale com a mesma redirect_uri na troca (RFC 6749
			// §4.1.3): a porta que entrou na autorização é a que sai no token.
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {codigo},
				"redirect_uri":  {tc.redirect},
				"code_verifier": {verificadorPKCE},
				"client_id":     {clientID},
				"resource":      {p.recursoPessoal},
			}
			statusToken, respostaToken := p.trocar(t, form)
			if statusToken != http.StatusOK {
				t.Fatalf("troca: status = %d, quer 200 (corpo: %v)", statusToken, respostaToken)
			}
			if texto(t, respostaToken, "access_token") == "" {
				t.Error("troca sem access_token")
			}
		})
	}
}
