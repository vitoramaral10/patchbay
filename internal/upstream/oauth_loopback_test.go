package upstream_test

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// A fatia 15: o provedor que só aceita redirect de loopback.
//
// O fluxo é o de sempre até o provedor devolver o navegador — e aí ele o devolve
// para um endereço da máquina do admin, onde ninguém escuta. O que o patchbay
// entrega é a ponte: uma tela que explica isso antes de acontecer, e uma caixa
// onde a URL que ficou na barra de endereços vira consentimento.

// formLoopback é um upstream OAuth com redirect de loopback e cliente
// pré-registrado — a combinação que o modo exige.
func formLoopback(nome, urlMCP, clientID, redirect string) upstream.Form {
	f := formOAuth(nome, urlMCP)
	f.OAuthClientID = clientID
	f.OAuthLoopback = redirect
	return f
}

// TestOAuth_LoopbackPontaAPonta percorre o consentimento inteiro pela borda
// HTTP, do clique em Autorizar à colagem da URL de retorno.
//
// A asserção que dá sentido à fatia é a do fim: o redirect_uri que foi para o
// authorize e o que foi para o token são o mesmo, e são o de loopback. Provedor
// de verdade responde invalid_grant quando os dois diferem, e é justamente aí
// que um "redirect global" quebraria sem o teste perceber.
func TestOAuth_LoopbackPontaAPonta(t *testing.T) {
	t.Parallel()

	const (
		clientID = "cli-da-canva"
		redirect = "http://127.0.0.1:53682/callback"
		id       = int64(1)
	)

	as := novoASFalso(t)
	as.preRegistrar(clientID, "")
	recurso := novoRecursoProtegido(t, as, false, "buscar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formLoopback("canva", recurso.URLMCP, clientID, redirect)}
	}, opcoesAmbiente{})

	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)

	cliente := clienteSemSeguir()
	rota := a.admin.URL + webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)

	// 1. O clique em Autorizar não redireciona: ele abre a tela de entrega
	// manual, porque o destino seria uma página de erro na máquina do admin.
	resp, err := cliente.Post(rota+"/autorizar", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST autorizar: erro = %v, quer nil", err)
	}
	corpo, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("ler a tela de entrega: erro = %v, quer nil", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status do autorizar = %d, quer 200 (corpo: %s)", resp.StatusCode, corpo)
	}
	tela := string(corpo)
	if !strings.Contains(tela, redirect) {
		t.Error("a tela não diz para onde o provedor vai devolver o navegador")
	}
	if !strings.Contains(tela, "/autorizar/colar") {
		t.Error("a tela não traz a caixa de colar o retorno")
	}

	// 2. O provedor. A URL sai da própria tela, que é como o admin a alcança.
	destino := urlDoProvedor(t, tela)
	if !strings.Contains(destino, url.QueryEscape(redirect)) {
		t.Fatalf("URL de autorização = %q, quer o redirect de loopback", destino)
	}
	resp, err = cliente.Get(destino)
	if err != nil {
		t.Fatalf("GET no provedor: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status do provedor = %d, quer 302", resp.StatusCode)
	}
	volta := resp.Header.Get("Location")
	if !strings.HasPrefix(volta, redirect) {
		t.Fatalf("o provedor devolveu para %q, quer o loopback", volta)
	}

	// 3. A colagem. É aqui que o navegador do admin entraria com a URL da barra
	// de endereços, depois de ver a página falhar.
	resp, err = cliente.PostForm(rota+"/autorizar/colar", url.Values{"retorno": {volta}})
	if err != nil {
		t.Fatalf("POST colar: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status da colagem = %d, quer 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); !strings.Contains(got, "aviso=autorizado") {
		t.Errorf("destino da colagem = %q, quer aviso=autorizado", got)
	}

	// 4. O consentimento valeu: o upstream conecta e lista as ferramentas.
	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)
	if f := a.gerente.Ferramentas(id); len(f) != 1 || f[0].Name != "buscar" {
		t.Fatalf("ferramentas = %v, quer [buscar]", f)
	}

	// 5. E o redirect_uri foi o mesmo nas duas etapas.
	noAuthorize, noToken := as.redirectsVistos()
	if noAuthorize != redirect {
		t.Errorf("redirect_uri no authorize = %q, quer %q", noAuthorize, redirect)
	}
	if noToken != redirect {
		t.Errorf("redirect_uri no token = %q, quer %q", noToken, redirect)
	}
}

// TestOAuth_LoopbackRecusaRetornoInvalido cobre as três formas de errar a
// colagem: texto que não é retorno nenhum, e state que não pertence a tentativa
// alguma. Nenhuma delas pode virar consentimento, e todas voltam para a tela com
// o texto de volta no campo.
func TestOAuth_LoopbackRecusaRetornoInvalido(t *testing.T) {
	t.Parallel()

	const (
		clientID = "cli-da-canva"
		redirect = "http://127.0.0.1:53682/callback"
		id       = int64(1)
	)

	as := novoASFalso(t)
	as.preRegistrar(clientID, "")
	recurso := novoRecursoProtegido(t, as, false, "buscar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formLoopback("canva", recurso.URLMCP, clientID, redirect)}
	}, opcoesAmbiente{})
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)

	cliente := clienteSemSeguir()
	rota := a.admin.URL + webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10) + "/autorizar/colar"

	casos := map[string]struct {
		retorno string
		trecho  string
	}{
		"campo vazio": {
			retorno: "   ",
			trecho:  "Cole a URL",
		},
		"URL sem code nem error": {
			retorno: redirect + "?outra=coisa",
			trecho:  "nem code nem error",
		},
		"state que não pertence a tentativa nenhuma": {
			retorno: redirect + "?code=cod-1&state=inventado",
			trecho:  "não corresponde a uma autorização em curso",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			resp, err := cliente.PostForm(rota, url.Values{"retorno": {tc.retorno}})
			if err != nil {
				t.Fatalf("POST colar: erro = %v, quer nil", err)
			}
			corpo, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("ler corpo: erro = %v, quer nil", err)
			}
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, quer 422 (corpo: %s)", resp.StatusCode, corpo)
			}
			if !strings.Contains(string(corpo), tc.trecho) {
				t.Errorf("a recusa não explica o erro; quer conter %q", tc.trecho)
			}
		})
	}

	// Nada disso pode ter autorizado coisa alguma.
	if a.gerente.Ferramentas(id) != nil {
		t.Error("uma colagem recusada produziu catálogo")
	}
}

// TestForm_ValidarLoopback trava as regras do campo, que são as do provedor e
// não do patchbay: http, endereço literal de loopback, porta explícita, e
// client_id junto.
func TestForm_ValidarLoopback(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		ajuste    func(*upstream.Form)
		querPassa bool
	}{
		"loopback com client_id": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthLoopback = "cli", "http://127.0.0.1:53682/callback"
			},
			querPassa: true,
		},
		"ipv6 também vale": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthLoopback = "cli", "http://[::1]:53682/callback"
			},
			querPassa: true,
		},
		"sem client_id não há como registrar": {
			ajuste: func(f *upstream.Form) { f.OAuthLoopback = "http://127.0.0.1:53682/callback" },
		},
		"localhost é nome, não endereço": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthLoopback = "cli", "http://localhost:53682/callback"
			},
		},
		"sem porta não bate byte a byte": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthLoopback = "cli", "http://127.0.0.1/callback"
			},
		},
		"https não existe em loopback aqui": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthLoopback = "cli", "https://127.0.0.1:53682/callback"
			},
		},
		"endereço que não é loopback": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthLoopback = "cli", "http://10.0.0.5:53682/callback"
			},
		},
		"loopback fora do modo oauth": {
			ajuste: func(f *upstream.Form) {
				f.Modo = upstream.ModoEstatica
				f.OAuthLoopback = "http://127.0.0.1:53682/callback"
			},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			f := formOAuth("canva", "https://mcp.canva.com/mcp")
			tc.ajuste(&f)

			passou := f.Validar()
			if passou != tc.querPassa {
				t.Fatalf("passou = %v, quer %v (erros = %v)", passou, tc.querPassa, f.Erros)
			}
			if !tc.querPassa && f.Erros["oauth_loopback"] == "" {
				t.Errorf("erros = %v, quer a chave oauth_loopback", f.Erros)
			}
		})
	}
}

// urlDoProvedor tira da tela de entrega o link que leva ao consentimento.
func urlDoProvedor(t *testing.T, tela string) string {
	t.Helper()

	const marca = `href="`
	i := strings.Index(tela, marca+"http")
	if i < 0 {
		t.Fatalf("a tela de entrega não traz o link do provedor")
	}
	resto := tela[i+len(marca):]
	fim := strings.IndexByte(resto, '"')
	if fim <= 0 {
		t.Fatalf("link do provedor malformado na tela")
	}
	return strings.ReplaceAll(resto[:fim], "&amp;", "&")
}
