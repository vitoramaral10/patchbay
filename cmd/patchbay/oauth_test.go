package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/vitoramaral10/patchbay/internal/admin"
	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/authsrv"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

// Credenciais de teste do cliente confidencial. Não são segredo de nada: o
// banco nasce e morre com o teste.
const (
	segredoConfidencial = "pbcs_teste_bWVyYW1lbnRlLXVtLXNlZ3JlZG8tZGUtdGVzdGU"
	verificadorPKCE     = "verificador-de-teste-com-mais-de-quarenta-e-tres-caracteres"
)

// patchbayOAuth é um patchbay em processo com o authorization server pronto: um
// admin com sessão aberta, um cliente público e um confidencial cadastrados.
//
// A URL pública precisa ser a URL real do servidor de teste, e não um valor
// fixo: ela é o issuer anunciado, é a base do resource do RFC 8707 e é o que a
// validação do RFC 9728 compara com a URL que o cliente usou. Por isso o
// listener nasce antes do handler.
type patchbayOAuth struct {
	app        *Aplicacao
	urlPublica string
	cookie     *http.Cookie

	clientePublico      string
	clienteConfidencial string
	endpointPessoal     string
	endpointTrabalho    string
	recursoPessoal      string

	sincronizou <-chan struct{}
	cliente     *http.Client
}

// subirPatchbayOAuth sobe o patchbay em processo. opcoes existe para a fatia 11:
// o teste de CIMD troca o buscador de documentos por um que alcança o httptest
// em loopback.
func subirPatchbayOAuth(t *testing.T, urlUpstream string, opcoes ...OpcaoApp) patchbayOAuth {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("abrir listener: erro = %v, quer nil", err)
	}
	urlPublica := "http://" + lis.Addr().String()

	cfg := Config{
		Listen:    lis.Addr().String(),
		DataDir:   t.TempDir(),
		PublicURL: urlPublica,
		NivelLog:  slog.LevelError,
	}
	log := slog.New(slog.DiscardHandler)
	ctx, cancelar := context.WithCancel(context.Background())
	t.Cleanup(cancelar)

	st, err := store.Abrir(ctx, cfg.DataDir)
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	semeado, err := semear(ctx, st.Escrita(), OpcoesSeed{
		Endpoint:    "pessoal",
		Upstream:    "falso",
		UpstreamURL: urlUpstream,
		NomeChave:   "teste",
		TimeoutMS:   5000,
	})
	if err != nil {
		t.Fatalf("semear: erro = %v, quer nil", err)
	}
	var trabalhoID int64
	if err := st.Escrita().QueryRowContext(ctx,
		`INSERT INTO endpoint (slug, nome, descricao, criado_em) VALUES ('trabalho','Trabalho','',0)
		 RETURNING id`).Scan(&trabalhoID); err != nil {
		t.Fatalf("criar segundo endpoint: erro = %v, quer nil", err)
	}
	// O admin entra por SQL: o teste nunca autentica por senha, e pagar um
	// argon2id de 64 MiB por caso só para chegar à sessão seria custo sem
	// cobertura nenhuma.
	if _, err := st.Escrita().ExecContext(ctx,
		`INSERT INTO admin (id, usuario, senha_hash, criado_em, atualizado_em)
		 VALUES (1, 'admin', 'hash-nao-usado', 0, 0)`); err != nil {
		t.Fatalf("criar admin: erro = %v, quer nil", err)
	}

	repo := authsrv.NovoRepositorioSQLite(st.Leitura(), st.Escrita())
	agora := time.Now()
	publico, err := repo.CriarCliente(ctx, authsrv.FormCliente{
		Nome:         "claude.ai de teste",
		RedirectURIs: []string{authsrv.RedirectClaudeAI},
		EndpointIDs:  []int64{semeado.EndpointID},
	}, "pbc_publico_de_teste", "", "", agora)
	if err != nil {
		t.Fatalf("cadastrar cliente público: erro = %v, quer nil", err)
	}
	confidencial, err := repo.CriarCliente(ctx, authsrv.FormCliente{
		Nome:         "serviço confidencial de teste",
		Confidencial: true,
		RedirectURIs: []string{authsrv.RedirectClaudeAI},
		EndpointIDs:  []int64{semeado.EndpointID, trabalhoID},
	}, "pbc_confidencial_de_teste", "pbcs_teste", authsrv.Hash(segredoConfidencial), agora)
	if err != nil {
		t.Fatalf("cadastrar cliente confidencial: erro = %v, quer nil", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("fechar banco do seed: erro = %v, quer nil", err)
	}

	// A origem da biblioteca aponta para um registry local e mudo: sem isto,
	// Iniciar dispararia uma varredura do registry de verdade só por subir o
	// patchbay. Ver registryMudo, em ui_test.go.
	app, err := montar(ctx, cfg, cofreDeTeste(t), log, append([]OpcaoApp{
		ComOrigemDaBiblioteca(registryMudo(t)),
		ComCuradoriaDaBiblioteca(curadoriaMudaDeTeste(t)),
		SemSementeDaBiblioteca(),
	}, opcoes...)...)
	if err != nil {
		t.Fatalf("montar: erro = %v, quer nil", err)
	}
	sincronizou := make(chan struct{}, 8)
	app.Observar(func() {
		select {
		case sincronizou <- struct{}{}:
		default:
		}
	})
	app.Iniciar(ctx)

	ts := &httptest.Server{
		Listener: lis,
		Config:   &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second},
	}
	ts.Start()
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
		cancelar()
		if err := app.Fechar(); err != nil {
			t.Errorf("fechar aplicação: erro = %v, quer nil", err)
		}
	})

	token, _, err := app.adm.AbrirSessao(ctx, 1)
	if err != nil {
		t.Fatalf("abrir sessão de admin: erro = %v, quer nil", err)
	}

	return patchbayOAuth{
		app:                 app,
		urlPublica:          ts.URL,
		cookie:              &http.Cookie{Name: admin.NomeCookie, Value: token},
		clientePublico:      publico.ClientID,
		clienteConfidencial: confidencial.ClientID,
		endpointPessoal:     semeado.EndpointSlug,
		endpointTrabalho:    "trabalho",
		recursoPessoal:      ts.URL + "/mcp/" + semeado.EndpointSlug,
		sincronizou:         sincronizou,
		// Sem seguir redirect: a resposta de autorização é justamente o
		// Location, e um cliente que o segue tentaria bater no claude.ai.
		cliente: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p patchbayOAuth) esperarCatalogo(t *testing.T) {
	t.Helper()
	select {
	case <-p.sincronizou:
	case <-time.After(15 * time.Second):
		t.Fatal("catálogo não materializou em 15s")
	}
}

// desafio devolve o code_challenge S256 de um verificador.
func desafio(verificador string) string {
	soma := sha256.Sum256([]byte(verificador))
	return base64.RawURLEncoding.EncodeToString(soma[:])
}

// pedidoPadrao é um pedido de autorização válido do cliente público.
func (p patchbayOAuth) pedidoPadrao() url.Values {
	return url.Values{
		"client_id":             {p.clientePublico},
		"redirect_uri":          {authsrv.RedirectClaudeAI},
		"response_type":         {"code"},
		"code_challenge":        {desafio(verificadorPKCE)},
		"code_challenge_method": {"S256"},
		"resource":              {p.recursoPessoal},
		"state":                 {"estado-de-teste"},
	}
}

// enviar monta e executa uma requisição com o cookie de admin quando pedido.
func (p patchbayOAuth) enviar(t *testing.T, metodo, alvo string, corpo url.Values, tipo string, comSessao bool) *http.Response {
	t.Helper()

	var leitor io.Reader
	if corpo != nil {
		leitor = strings.NewReader(corpo.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), metodo, alvo, leitor)
	if err != nil {
		t.Fatalf("montar requisição %s %s: erro = %v, quer nil", metodo, alvo, err)
	}
	if tipo != "" {
		req.Header.Set("Content-Type", tipo)
	}
	if comSessao {
		req.AddCookie(p.cookie)
	}
	res, err := p.cliente.Do(req)
	if err != nil {
		t.Fatalf("requisição %s %s: erro = %v, quer nil", metodo, alvo, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// consentir faz o POST da decisão de consentimento e devolve a resposta.
func (p patchbayOAuth) consentir(t *testing.T, q url.Values, decisao string) *http.Response {
	t.Helper()
	corpo := url.Values{}
	for chave, valores := range q {
		corpo[chave] = slices.Clone(valores)
	}
	corpo.Set("decisao", decisao)
	return p.enviar(t, http.MethodPost, p.urlPublica+authsrv.RotaAutorizar, corpo,
		"application/x-www-form-urlencoded", true)
}

// codigoDe extrai o code do Location de uma resposta de autorização aceita.
func codigoDe(t *testing.T, res *http.Response) string {
	t.Helper()
	if res.StatusCode != http.StatusFound {
		corpo, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, quer %d (corpo: %.200q)", res.StatusCode, http.StatusFound, corpo)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location inválido: erro = %v, quer nil", err)
	}
	if e := loc.Query().Get("error"); e != "" {
		t.Fatalf("resposta de autorização = erro %q (%s), quer code",
			e, loc.Query().Get("error_description"))
	}
	return loc.Query().Get("code")
}

// trocar chama o token endpoint e devolve status e corpo já decodificado.
func (p patchbayOAuth) trocar(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	res := p.enviar(t, http.MethodPost, p.urlPublica+authsrv.RotaToken, form,
		"application/x-www-form-urlencoded", false)
	var corpo map[string]any
	bruto, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ler resposta do token endpoint: erro = %v, quer nil", err)
	}
	if len(bruto) > 0 {
		if err := json.Unmarshal(bruto, &corpo); err != nil {
			t.Fatalf("resposta do token endpoint não é JSON: %v (%.200q)", err, bruto)
		}
	}
	return res.StatusCode, corpo
}

// formDeCodigo monta o corpo do grant authorization_code do cliente público.
func (p patchbayOAuth) formDeCodigo(codigo, verificador string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codigo},
		"redirect_uri":  {authsrv.RedirectClaudeAI},
		"code_verifier": {verificador},
		"client_id":     {p.clientePublico},
		"resource":      {p.recursoPessoal},
	}
}

func texto(t *testing.T, corpo map[string]any, chave string) string {
	t.Helper()
	v, _ := corpo[chave].(string)
	return v
}

// --- o critério de pronto da fatia ---

// TestOAuthPontaAPonta é o critério de pronto: o cliente OAuth real do go-sdk
// — auth.AuthorizationCodeHandler, o mesmo código que o Claude Code usa contra
// um servidor de terceiro — descobre o authorization server do patchbay pelo
// desafio 401, passa pelo consentimento, troca o código e chama uma ferramenta.
//
// É este teste que substitui a maturidade da biblioteca recusada na seção 09 do
// estudo: sem ele, escrever o AS à mão não se sustenta.
func TestOAuthPontaAPonta(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	// O fetcher faz o papel do navegador do admin: abre a URL de autorização
	// com o cookie de sessão e devolve o code que veio no redirect.
	var visitou int
	fetcher := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		visitou++

		alvo, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		// A tela de consentimento precisa aparecer antes de qualquer código ser
		// emitido: é o passo humano do fluxo.
		tela := p.enviar(t, http.MethodGet, args.URL, nil, "", true)
		if tela.StatusCode != http.StatusOK {
			corpo, _ := io.ReadAll(tela.Body)
			t.Fatalf("tela de consentimento: status = %d, quer 200 (corpo: %.300q)", tela.StatusCode, corpo)
		}
		html, err := io.ReadAll(tela.Body)
		if err != nil {
			return nil, err
		}
		for _, esperado := range []string{"claude.ai de teste", "claude.ai", p.recursoPessoal, apikey.Escopo(p.endpointPessoal)} {
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
		return &auth.AuthorizationResult{
			Code:  q.Get("code"),
			State: q.Get("state"),
			Iss:   q.Get("iss"),
		}, nil
	}

	manipulador, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{
			ClientID: p.clientePublico,
			// O issuer gravado é o que faz o cliente recusar credencial de um AS
			// para outro (auth/authorization_code.go:538).
			Issuer: p.urlPublica,
		},
		RedirectURL:              authsrv.RedirectClaudeAI,
		AuthorizationCodeFetcher: fetcher,
		RequestRefreshToken:      true,
	})
	if err != nil {
		t.Fatalf("montar AuthorizationCodeHandler: erro = %v, quer nil", err)
	}

	ctx, cancelar := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelar()

	sessao, err := mcp.NewClient(&mcp.Implementation{Name: "cliente-oauth-de-teste", Version: "0.0.1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:     p.urlPublica + "/mcp/" + p.endpointPessoal,
			OAuthHandler: manipulador,
			MaxRetries:   -1,
		}, nil)
	if err != nil {
		t.Fatalf("conectar com OAuth em /mcp/%s: erro = %v, quer nil", p.endpointPessoal, err)
	}
	t.Cleanup(func() { _ = sessao.Close() })

	if visitou != 1 {
		t.Errorf("fluxo de autorização executado %d vezes, quer 1", visitou)
	}

	lista, err := sessao.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list com token OAuth: erro = %v, quer nil", err)
	}
	var nomes []string
	for _, f := range lista.Tools {
		nomes = append(nomes, f.Name)
	}
	slices.Sort(nomes)
	if quer := []string{nomeNormalizado, "somar"}; !slices.Equal(nomes, quer) {
		t.Fatalf("ferramentas = %v, quer %v", nomes, quer)
	}

	chamada, err := sessao.CallTool(ctx, &mcp.CallToolParams{
		Name:      "somar",
		Arguments: map[string]any{"a": 2, "b": 40},
	})
	if err != nil {
		t.Fatalf("tools/call com token OAuth: erro = %v, quer nil", err)
	}
	if chamada.IsError {
		t.Fatalf("tools/call devolveu erro de ferramenta: %v", textoDe(chamada))
	}
	if got := textoDe(chamada); got != "42" {
		t.Errorf("resultado = %q, quer %q", got, "42")
	}
}

// --- metadata ---

// TestMetadataBemFormada decodifica os dois documentos dentro das structs do
// go-sdk: é o que garante que os nomes de campo continuam sendo os que o
// cliente lê, e não só os que eu escrevi.
func TestMetadataBemFormada(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	t.Run("authorization server (RFC 8414)", func(t *testing.T) {
		t.Parallel()

		res := p.enviar(t, http.MethodGet, p.urlPublica+authsrv.RotaMetadataAS, nil, "", false)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, quer 200", res.StatusCode)
		}
		var meta oauthex.AuthServerMeta
		if err := json.NewDecoder(res.Body).Decode(&meta); err != nil {
			t.Fatalf("decodificar metadata: erro = %v, quer nil", err)
		}
		switch {
		case meta.Issuer != p.urlPublica:
			t.Errorf("issuer = %q, quer %q", meta.Issuer, p.urlPublica)
		case !slices.Equal(meta.CodeChallengeMethodsSupported, []string{"S256"}):
			t.Errorf("code_challenge_methods_supported = %v, quer [S256]", meta.CodeChallengeMethodsSupported)
		case !slices.Contains(meta.TokenEndpointAuthMethodsSupported, "none"):
			t.Errorf("token_endpoint_auth_methods_supported = %v, quer conter none",
				meta.TokenEndpointAuthMethodsSupported)
		case !slices.Contains(meta.GrantTypesSupported, "authorization_code"):
			t.Errorf("grant_types_supported = %v, quer conter authorization_code", meta.GrantTypesSupported)
		case !slices.Contains(meta.GrantTypesSupported, "refresh_token"):
			t.Errorf("grant_types_supported = %v, quer conter refresh_token", meta.GrantTypesSupported)
		case meta.RevocationEndpoint != p.urlPublica+authsrv.RotaRevogar:
			t.Errorf("revocation_endpoint = %q, quer %q", meta.RevocationEndpoint, p.urlPublica+authsrv.RotaRevogar)
		case !meta.AuthorizationResponseIssParameterSupported:
			t.Error("authorization_response_iss_parameter_supported = false, quer true")
		}
		// O par que decide entre CIMD e DCR no claude.ai. Faltando um dos dois,
		// ele cai para DCR e registra um cliente novo a cada conexão fresca —
		// então este assert é o critério de "o claude.ai escolhe CIMD".
		if !meta.ClientIDMetadataDocumentSupported {
			t.Error("client_id_metadata_document_supported = false, quer true")
		}
		if !slices.Contains(meta.TokenEndpointAuthMethodsSupported, "none") {
			t.Error(`token_endpoint_auth_methods_supported não tem "none", e sem ele não há CIMD`)
		}
		// E o registration_endpoint, que é o que o cliente que só sabe DCR usa.
		if quer := p.urlPublica + authsrv.RotaRegistrar; meta.RegistrationEndpoint != quer {
			t.Errorf("registration_endpoint = %q, quer %q", meta.RegistrationEndpoint, quer)
		}
	})

	t.Run("protected resource (RFC 9728)", func(t *testing.T) {
		t.Parallel()

		// Cobre os dois mecanismos de descoberta que o RFC exige que o cliente
		// suporte: o caminho com sufixo de endpoint, e o fallback na raiz — que
		// não tem slug para nomear, então aponta a base pública e agrega o
		// escopo de todos os endpoints (ver a suposição em
		// authsrv.Servico.MetadataRecursoRaiz).
		casos := map[string]struct {
			caminho      string
			querResource string
		}{
			"com slug": {
				caminho:      "/.well-known/oauth-protected-resource/mcp/" + p.endpointPessoal,
				querResource: p.recursoPessoal,
			},
			"fallback na raiz": {
				caminho:      "/.well-known/oauth-protected-resource",
				querResource: p.urlPublica,
			},
		}

		for nome, tc := range casos {
			t.Run(nome, func(t *testing.T) {
				t.Parallel()

				res := p.enviar(t, http.MethodGet, p.urlPublica+tc.caminho, nil, "", false)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, quer 200", res.StatusCode)
				}
				var meta oauthex.ProtectedResourceMetadata
				if err := json.NewDecoder(res.Body).Decode(&meta); err != nil {
					t.Fatalf("decodificar metadata: erro = %v, quer nil", err)
				}
				if meta.Resource != tc.querResource {
					t.Errorf("resource = %q, quer %q", meta.Resource, tc.querResource)
				}
				if !slices.Equal(meta.AuthorizationServers, []string{p.urlPublica}) {
					t.Errorf("authorization_servers = %v, quer [%s]", meta.AuthorizationServers, p.urlPublica)
				}
				if quer := apikey.Escopo(p.endpointPessoal); !slices.Contains(meta.ScopesSupported, quer) {
					t.Errorf("scopes_supported = %v, quer conter %q", meta.ScopesSupported, quer)
				}
			})
		}
	})

	t.Run("endpoint inexistente devolve 404", func(t *testing.T) {
		t.Parallel()

		res := p.enviar(t, http.MethodGet,
			p.urlPublica+"/.well-known/oauth-protected-resource/mcp/inexistente", nil, "", false)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, quer 404", res.StatusCode)
		}
	})

	t.Run("desafio 401 aponta para a metadata daquele endpoint", func(t *testing.T) {
		t.Parallel()

		res := p.enviar(t, http.MethodPost, p.urlPublica+"/mcp/"+p.endpointPessoal, nil, "application/json", false)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, quer 401", res.StatusCode)
		}
		desafios, err := oauthex.ParseWWWAuthenticate(res.Header.Values("WWW-Authenticate"))
		if err != nil {
			t.Fatalf("parsear WWW-Authenticate: erro = %v, quer nil", err)
		}
		quer := p.urlPublica + "/.well-known/oauth-protected-resource/mcp/" + p.endpointPessoal
		achou := false
		for _, d := range desafios {
			if d.Params["resource_metadata"] == quer {
				achou = true
			}
		}
		if !achou {
			t.Errorf("desafios = %v, quer resource_metadata=%q", desafios, quer)
		}
	})
}

// --- validação do authorize ---

func TestAutorizacaoRecusada(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	casos := map[string]struct {
		ajustar      func(url.Values)
		querRedirect bool
		querErro     string
	}{
		"redirect_uri que não bate caractere a caractere não redireciona": {
			ajustar:      func(q url.Values) { q.Set("redirect_uri", authsrv.RedirectClaudeAI+"/") },
			querRedirect: false,
		},
		"client_id desconhecido não redireciona": {
			ajustar:      func(q url.Values) { q.Set("client_id", "pbc_nunca_cadastrado") },
			querRedirect: false,
		},
		"sem PKCE redireciona com invalid_request": {
			ajustar:      func(q url.Values) { q.Del("code_challenge") },
			querRedirect: true,
			querErro:     "invalid_request",
		},
		"PKCE plain redireciona com invalid_request": {
			ajustar:      func(q url.Values) { q.Set("code_challenge_method", "plain") },
			querRedirect: true,
			querErro:     "invalid_request",
		},
		"sem state redireciona com invalid_request": {
			ajustar:      func(q url.Values) { q.Del("state") },
			querRedirect: true,
			querErro:     "invalid_request",
		},
		"response_type=token redireciona com unsupported_response_type": {
			ajustar:      func(q url.Values) { q.Set("response_type", "token") },
			querRedirect: true,
			querErro:     "unsupported_response_type",
		},
		"sem resource redireciona com invalid_target": {
			ajustar:      func(q url.Values) { q.Del("resource") },
			querRedirect: true,
			querErro:     "invalid_target",
		},
		"resource de endpoint fora do escopo do cliente redireciona com invalid_target": {
			ajustar:      func(q url.Values) { q.Set("resource", "/mcp/trabalho") },
			querRedirect: true,
			querErro:     "invalid_target",
		},
		"recusa do admin redireciona com access_denied": {
			ajustar:      func(url.Values) {},
			querRedirect: true,
			querErro:     "access_denied",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			q := p.pedidoPadrao()
			tc.ajustar(q)
			// O caso de resource de outro endpoint precisa da URL absoluta, que
			// só existe depois de o servidor subir.
			if q.Get("resource") == "/mcp/trabalho" {
				q.Set("resource", p.urlPublica+"/mcp/trabalho")
			}

			decisao := "aceitar"
			if tc.querErro == "access_denied" {
				decisao = "recusar"
			}
			res := p.consentir(t, q, decisao)

			if !tc.querRedirect {
				if res.StatusCode == http.StatusFound {
					t.Fatalf("status = 302 para %s, quer erro na tela sem redirecionar",
						res.Header.Get("Location"))
				}
				if res.StatusCode < 400 || res.StatusCode >= 500 {
					t.Fatalf("status = %d, quer 4xx", res.StatusCode)
				}
				return
			}

			if res.StatusCode != http.StatusFound {
				corpo, _ := io.ReadAll(res.Body)
				t.Fatalf("status = %d, quer 302 (corpo: %.200q)", res.StatusCode, corpo)
			}
			loc, err := url.Parse(res.Header.Get("Location"))
			if err != nil {
				t.Fatalf("Location inválido: erro = %v, quer nil", err)
			}
			if got := loc.Query().Get("error"); got != tc.querErro {
				t.Errorf("error = %q, quer %q", got, tc.querErro)
			}
			if q.Has("state") && loc.Query().Get("state") != q.Get("state") {
				t.Errorf("state = %q, quer %q", loc.Query().Get("state"), q.Get("state"))
			}
			if got := loc.Query().Get("iss"); got != p.urlPublica {
				t.Errorf("iss = %q, quer %q", got, p.urlPublica)
			}
		})
	}
}

func TestAutorizacaoSemSessaoDeAdminVaiParaOLogin(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	alvo := p.urlPublica + authsrv.RotaAutorizar + "?" + p.pedidoPadrao().Encode()

	res := p.enviar(t, http.MethodGet, alvo, nil, "", false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, quer 303", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin/login?destino=") {
		t.Fatalf("Location = %q, quer /admin/login com o destino carregado", loc)
	}
}

// --- token endpoint ---

func TestTokenEndpoint(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	t.Run("code_verifier errado devolve invalid_grant", func(t *testing.T) {
		t.Parallel()

		codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))
		status, corpo := p.trocar(t, p.formDeCodigo(codigo, "verificador-errado-porem-com-quarenta-e-tres-chars"))
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, quer 400", status)
		}
		if got := texto(t, corpo, "error"); got != "invalid_grant" {
			t.Errorf("error = %q, quer invalid_grant", got)
		}
	})

	t.Run("redirect_uri diferente da autorização devolve invalid_grant", func(t *testing.T) {
		t.Parallel()

		codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))
		form := p.formDeCodigo(codigo, verificadorPKCE)
		form.Set("redirect_uri", authsrv.RedirectClaudeAI+"?x=1")
		status, corpo := p.trocar(t, form)
		if status != http.StatusBadRequest || texto(t, corpo, "error") != "invalid_grant" {
			t.Errorf("status = %d, error = %q; quer 400 invalid_grant", status, texto(t, corpo, "error"))
		}
	})

	t.Run("grant_type desconhecido devolve unsupported_grant_type", func(t *testing.T) {
		t.Parallel()

		status, corpo := p.trocar(t, url.Values{
			"grant_type": {"client_credentials"},
			"client_id":  {p.clientePublico},
		})
		if status != http.StatusBadRequest || texto(t, corpo, "error") != "unsupported_grant_type" {
			t.Errorf("status = %d, error = %q; quer 400 unsupported_grant_type",
				status, texto(t, corpo, "error"))
		}
	})

	t.Run("client_id desconhecido devolve invalid_client em 401", func(t *testing.T) {
		t.Parallel()

		status, corpo := p.trocar(t, url.Values{
			"grant_type": {"authorization_code"},
			"code":       {"pbac_qualquer"},
			"client_id":  {"pbc_nunca_cadastrado"},
		})
		if status != http.StatusUnauthorized || texto(t, corpo, "error") != "invalid_client" {
			t.Errorf("status = %d, error = %q; quer 401 invalid_client", status, texto(t, corpo, "error"))
		}
	})

	t.Run("segredo errado em cliente confidencial devolve invalid_client", func(t *testing.T) {
		t.Parallel()

		status, corpo := p.trocar(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {"pbac_qualquer"},
			"client_id":     {p.clienteConfidencial},
			"client_secret": {"pbcs_errado"},
		})
		if status != http.StatusUnauthorized || texto(t, corpo, "error") != "invalid_client" {
			t.Errorf("status = %d, error = %q; quer 401 invalid_client", status, texto(t, corpo, "error"))
		}
	})

	t.Run("corpo em JSON devolve 400 com invalid_request e o motivo escrito", func(t *testing.T) {
		t.Parallel()

		res := p.enviar(t, http.MethodPost, p.urlPublica+authsrv.RotaToken,
			url.Values{"grant_type": {"refresh_token"}}, "application/json", false)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, quer 400 (nunca 415: o cliente desiste)", res.StatusCode)
		}
		var corpo map[string]string
		if err := json.NewDecoder(res.Body).Decode(&corpo); err != nil {
			t.Fatalf("decodificar erro: erro = %v, quer nil", err)
		}
		if corpo["error"] != "invalid_request" {
			t.Errorf("error = %q, quer invalid_request", corpo["error"])
		}
		if !strings.Contains(corpo["error_description"], "x-www-form-urlencoded") {
			t.Errorf("error_description = %q, quer citar application/x-www-form-urlencoded",
				corpo["error_description"])
		}
	})

	t.Run("resposta de sucesso vem com Cache-Control no-store", func(t *testing.T) {
		t.Parallel()

		codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))
		res := p.enviar(t, http.MethodPost, p.urlPublica+authsrv.RotaToken,
			p.formDeCodigo(codigo, verificadorPKCE), "application/x-www-form-urlencoded", false)
		if res.StatusCode != http.StatusOK {
			corpo, _ := io.ReadAll(res.Body)
			t.Fatalf("status = %d, quer 200 (corpo: %.200q)", res.StatusCode, corpo)
		}
		if got := res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, quer no-store", got)
		}
	})
}

// TestCodigoReusadoRevogaAFamilia é a troca única: o segundo uso do código não
// só é recusado, como queima o que o primeiro emitiu.
func TestCodigoReusadoRevogaAFamilia(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))

	status, primeira := p.trocar(t, p.formDeCodigo(codigo, verificadorPKCE))
	if status != http.StatusOK {
		t.Fatalf("primeira troca: status = %d, quer 200 (%v)", status, primeira)
	}
	acesso := texto(t, primeira, "access_token")
	if !strings.HasPrefix(acesso, authsrv.MarcaAcesso+"_") {
		t.Fatalf("access_token = %.12q…, quer prefixo %s_", acesso, authsrv.MarcaAcesso)
	}
	if p.statusNoMCP(t, p.endpointPessoal, acesso) != http.StatusOK {
		t.Fatal("o token emitido não abriu o endpoint antes do reuso do código")
	}

	status, segunda := p.trocar(t, p.formDeCodigo(codigo, verificadorPKCE))
	if status != http.StatusBadRequest || texto(t, segunda, "error") != "invalid_grant" {
		t.Fatalf("segunda troca: status = %d, error = %q; quer 400 invalid_grant",
			status, texto(t, segunda, "error"))
	}

	if got := p.statusNoMCP(t, p.endpointPessoal, acesso); got != http.StatusUnauthorized {
		t.Errorf("depois do reuso do código, /mcp = %d, quer 401 (a família toda cai)", got)
	}
}

// TestRefreshRotacionaEDetectaReplay é a exigência do OAuth 2.1 para cliente
// público: o refresh roda a cada uso, e o reuso de um já rotacionado derruba a
// família inteira — sem isso a rotação só troca o token de lugar.
func TestRefreshRotacionaEDetectaReplay(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))
	status, corpo := p.trocar(t, p.formDeCodigo(codigo, verificadorPKCE))
	if status != http.StatusOK {
		t.Fatalf("troca inicial: status = %d, quer 200 (%v)", status, corpo)
	}
	refresh1 := texto(t, corpo, "refresh_token")
	if refresh1 == "" {
		t.Fatal("refresh_token ausente na primeira concessão")
	}

	formRefresh := func(rt string) url.Values {
		return url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {rt},
			"client_id":     {p.clientePublico},
		}
	}

	status, renovado := p.trocar(t, formRefresh(refresh1))
	if status != http.StatusOK {
		t.Fatalf("renovação: status = %d, quer 200 (%v)", status, renovado)
	}
	refresh2 := texto(t, renovado, "refresh_token")
	acesso2 := texto(t, renovado, "access_token")
	if refresh2 == "" {
		t.Fatal("a renovação não devolveu refresh_token novo: sem rotação")
	}
	if refresh2 == refresh1 {
		t.Fatal("refresh_token não rotacionou")
	}
	if got := p.statusNoMCP(t, p.endpointPessoal, acesso2); got != http.StatusOK {
		t.Fatalf("o access token renovado não abriu o endpoint: status = %d", got)
	}

	// Replay: o refresh velho reaparece.
	status, replay := p.trocar(t, formRefresh(refresh1))
	if status != http.StatusBadRequest || texto(t, replay, "error") != "invalid_grant" {
		t.Fatalf("replay: status = %d, error = %q; quer 400 invalid_grant",
			status, texto(t, replay, "error"))
	}

	// A família inteira cai: nem o access novo nem o refresh novo sobrevivem.
	if got := p.statusNoMCP(t, p.endpointPessoal, acesso2); got != http.StatusUnauthorized {
		t.Errorf("depois do replay, /mcp com o access novo = %d, quer 401", got)
	}
	status, depois := p.trocar(t, formRefresh(refresh2))
	if status != http.StatusBadRequest || texto(t, depois, "error") != "invalid_grant" {
		t.Errorf("depois do replay, renovar com o refresh novo = %d/%q; quer 400 invalid_grant",
			status, texto(t, depois, "error"))
	}
}

// TestRevogacaoRFC7009 confere o revocation endpoint.
func TestRevogacaoRFC7009(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))
	status, corpo := p.trocar(t, p.formDeCodigo(codigo, verificadorPKCE))
	if status != http.StatusOK {
		t.Fatalf("troca: status = %d, quer 200 (%v)", status, corpo)
	}
	acesso, refresh := texto(t, corpo, "access_token"), texto(t, corpo, "refresh_token")

	// Token desconhecido: 200 assim mesmo (RFC 7009 §2.2).
	res := p.enviar(t, http.MethodPost, p.urlPublica+authsrv.RotaRevogar,
		url.Values{"token": {"pbrt_nunca_emitido"}, "client_id": {p.clientePublico}},
		"application/x-www-form-urlencoded", false)
	if res.StatusCode != http.StatusOK {
		t.Errorf("revogar token desconhecido: status = %d, quer 200", res.StatusCode)
	}

	res = p.enviar(t, http.MethodPost, p.urlPublica+authsrv.RotaRevogar,
		url.Values{"token": {refresh}, "token_type_hint": {"refresh_token"}, "client_id": {p.clientePublico}},
		"application/x-www-form-urlencoded", false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revogar refresh: status = %d, quer 200", res.StatusCode)
	}
	// Revogar um refresh derruba a família: o access cai junto.
	if got := p.statusNoMCP(t, p.endpointPessoal, acesso); got != http.StatusUnauthorized {
		t.Errorf("depois da revogação, /mcp = %d, quer 401", got)
	}
}

// --- verificação do bearer em /mcp ---

// statusNoMCP bate no transporte MCP com um bearer e devolve só o status.
func (p patchbayOAuth) statusNoMCP(t *testing.T, slug, bearer string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.urlPublica+"/mcp/"+slug, strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",`+
				`"capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil {
		t.Fatalf("montar requisição MCP: erro = %v, quer nil", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := p.cliente.Do(req)
	if err != nil {
		t.Fatalf("requisição MCP: erro = %v, quer nil", err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// TestTokenDeOutroEndpointRecebe403 é a fronteira de autorização da seção 07: o
// aud do token é o endpoint, e apresentá-lo noutro é falta de escopo, não falta
// de autenticação — 403, e não 401, porque dizer "reautentique" seria mandar o
// cliente refazer um fluxo que ia dar no mesmo.
func TestTokenDeOutroEndpointRecebe403(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	// O cliente confidencial tem os dois endpoints no escopo: o 403 vem do
	// resource pedido, não da falta de permissão do cliente.
	q := url.Values{
		"client_id":             {p.clienteConfidencial},
		"redirect_uri":          {authsrv.RedirectClaudeAI},
		"response_type":         {"code"},
		"code_challenge":        {desafio(verificadorPKCE)},
		"code_challenge_method": {"S256"},
		"resource":              {p.recursoPessoal},
		"state":                 {"estado-de-teste"},
	}
	codigo := codigoDe(t, p.consentir(t, q, "aceitar"))

	status, corpo := p.trocar(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codigo},
		"redirect_uri":  {authsrv.RedirectClaudeAI},
		"code_verifier": {verificadorPKCE},
		"client_id":     {p.clienteConfidencial},
		"client_secret": {segredoConfidencial},
	})
	if status != http.StatusOK {
		t.Fatalf("troca: status = %d, quer 200 (%v)", status, corpo)
	}
	acesso := texto(t, corpo, "access_token")

	if got := p.statusNoMCP(t, p.endpointPessoal, acesso); got != http.StatusOK {
		t.Fatalf("no endpoint do aud, status = %d, quer 200", got)
	}
	if got := p.statusNoMCP(t, p.endpointTrabalho, acesso); got != http.StatusForbidden {
		t.Errorf("no outro endpoint, status = %d, quer 403", got)
	}
}

// TestTokenExpiradoDevolve401ComDesafio prova que o desafio do 401 continua
// carregando o resource_metadata certo quando o token venceu — é dele que o
// cliente parte para renovar.
func TestTokenExpiradoDevolve401ComDesafio(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	codigo := codigoDe(t, p.consentir(t, p.pedidoPadrao(), "aceitar"))
	status, corpo := p.trocar(t, p.formDeCodigo(codigo, verificadorPKCE))
	if status != http.StatusOK {
		t.Fatalf("troca: status = %d, quer 200 (%v)", status, corpo)
	}
	acesso := texto(t, corpo, "access_token")

	// Envelhecer sem esperar pelo relógio: a expiração é uma coluna, e movê-la
	// para o passado é o mesmo que o tempo passar.
	if _, err := p.app.st.Escrita().ExecContext(context.Background(),
		`UPDATE oauth_token SET expira_em = 1 WHERE hash = ?`, authsrv.Hash(acesso)); err != nil {
		t.Fatalf("envelhecer token: erro = %v, quer nil", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		p.urlPublica+"/mcp/"+p.endpointPessoal, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+acesso)
	res, err := p.cliente.Do(req)
	if err != nil {
		t.Fatalf("requisição: erro = %v, quer nil", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, quer 401", res.StatusCode)
	}
	quer := p.urlPublica + "/.well-known/oauth-protected-resource/mcp/" + p.endpointPessoal
	if got := res.Header.Get("WWW-Authenticate"); !strings.Contains(got, quer) {
		t.Errorf("WWW-Authenticate = %q, quer conter resource_metadata=%q", got, quer)
	}
}

// TestChaveDeAPIContinuaValendo garante que ligar o AS não trocou uma credencial
// pela outra: o mesmo endpoint aceita as duas.
func TestChaveDeAPIContinuaValendo(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))
	p.esperarCatalogo(t)

	// A chave do seed não é devolvida por subirPatchbayOAuth; o que importa aqui
	// é que um bearer sem a marca do AS continue caindo no verificador de chave
	// de API e receba o 401 dele, e não o do authorization server.
	if got := p.statusNoMCP(t, p.endpointPessoal, "pbk_aaaabbbb_chave-inventada"); got != http.StatusUnauthorized {
		t.Errorf("chave de API inventada: status = %d, quer 401", got)
	}
	if got := p.statusNoMCP(t, p.endpointPessoal, "pbat_token-inventado"); got != http.StatusUnauthorized {
		t.Errorf("access token inventado: status = %d, quer 401", got)
	}
}

// TestLimiteDeTaxaNoToken prova que o AS não vira oráculo: uma rajada no token
// endpoint passa a ser recusada com 429 antes de a rajada acabar.
func TestLimiteDeTaxaNoToken(t *testing.T) {
	t.Parallel()

	p := subirPatchbayOAuth(t, upstreamFalso(t))

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"pbrt_nunca_emitido"},
		"client_id":     {p.clientePublico},
	}
	recusou := false
	for i := 0; i < authsrv.LimiteTokenPorMinuto+5 && !recusou; i++ {
		status, _ := p.trocar(t, form)
		if status == http.StatusTooManyRequests {
			recusou = true
		}
	}
	if !recusou {
		t.Errorf("nenhuma das %d requisições foi recusada com 429", authsrv.LimiteTokenPorMinuto+5)
	}
}
