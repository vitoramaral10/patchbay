package admin_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/admin"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

const (
	usuario = "vitor"
	senha   = "senha-de-doze-ou-mais"
)

// sistema é o admin sob teste com um banco real de arquivo temporário.
//
// Banco de verdade e não fake: o CHECK (id = 1) e o filtro de expiração no SQL
// são parte da regra, e um dublê de repositório concordaria com o meu
// entendimento deles em vez de exercitá-los.
type sistema struct {
	sut      *admin.HTTP
	servico  *admin.Servico
	servidor *httptest.Server
}

func montarSistema(t *testing.T, opcoes ...admin.Opcao) sistema {
	t.Helper()

	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})

	log := slog.New(slog.DiscardHandler)
	opcoes = append([]admin.Opcao{admin.ComParametros(baratos)}, opcoes...)
	servico := admin.NovoServico(admin.NovoRepositorioSQLite(st.Leitura(), st.Escrita()), log, opcoes...)
	sut := admin.NovoHTTP(servico, "http://127.0.0.1:8787", log)

	mux := http.NewServeMux()
	sut.Rotas(mux)
	mux.Handle(webui.RotaPainel, sut.Proteger(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			a, ok := admin.DoContexto(r.Context())
			if !ok {
				t.Error("rota protegida sem admin no contexto")
			}
			_, _ = w.Write([]byte("painel de " + a.Usuario))
		})))

	servidor := httptest.NewServer(mux)
	t.Cleanup(servidor.Close)
	return sistema{sut: sut, servico: servico, servidor: servidor}
}

// clienteSemSeguir não segue redirecionamento: o teste quer ver o 303 e o
// Location, não a página de destino.
func clienteSemSeguir(t *testing.T) *http.Client {
	t.Helper()
	jarro, err := newJar()
	if err != nil {
		t.Fatalf("cookie jar: erro = %v, quer nil", err)
	}
	return &http.Client{
		Jar:           jarro,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestProteger_SemAdminVaiParaSetup(t *testing.T) {
	t.Parallel()

	s := montarSistema(t)
	res := pegar(t, clienteSemSeguir(t), s.servidor.URL+webui.RotaPainel)

	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusSeeOther)
	}
	if destino := res.Header.Get("Location"); destino != webui.RotaSetup {
		t.Errorf("Location = %q, quer %q", destino, webui.RotaSetup)
	}
}

func TestSetup_SoExisteAntesDoPrimeiroAdmin(t *testing.T) {
	t.Parallel()

	s := montarSistema(t)
	cliente := clienteSemSeguir(t)

	// Antes: a tela existe.
	if res := pegar(t, cliente, s.servidor.URL+webui.RotaSetup); res.StatusCode != http.StatusOK {
		t.Fatalf("status do setup = %d, quer %d", res.StatusCode, http.StatusOK)
	}

	criarAdmin(t, cliente, s)

	// Depois: a rota deixa de existir e vira o caminho do login, tanto no GET
	// quanto no POST — senão um segundo POST trocaria o admin cadastrado.
	for _, metodo := range []string{http.MethodGet, http.MethodPost} {
		res := requisitar(t, cliente, metodo, s.servidor.URL+webui.RotaSetup, url.Values{
			"usuario":   {"invasor"},
			"senha":     {"outra-senha-longa"},
			"confirmar": {"outra-senha-longa"},
		})
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s no setup: status = %d, quer %d", metodo, res.StatusCode, http.StatusSeeOther)
		}
		if destino := res.Header.Get("Location"); destino != webui.RotaLogin {
			t.Errorf("%s no setup: Location = %q, quer %q", metodo, destino, webui.RotaLogin)
		}
	}

	// E o admin que vale continua sendo o primeiro.
	if _, err := s.servico.Autenticar(context.Background(), usuario, senha); err != nil {
		t.Errorf("autenticar o admin original: erro = %v, quer nil", err)
	}
}

func TestSetup_RecusaFormularioInvalido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		campos     url.Values
		querTrecho string
	}{
		"senhas diferentes": {
			campos:     url.Values{"usuario": {"vitor"}, "senha": {"senha-longa-o-suficiente"}, "confirmar": {"outra-coisa"}},
			querTrecho: "não são iguais",
		},
		"senha curta": {
			campos:     url.Values{"usuario": {"vitor"}, "senha": {"curta"}, "confirmar": {"curta"}},
			querTrecho: "12 caracteres",
		},
		"usuário vazio": {
			campos:     url.Values{"usuario": {"  "}, "senha": {"senha-longa-o-suficiente"}, "confirmar": {"senha-longa-o-suficiente"}},
			querTrecho: "nome de usuário",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			s := montarSistema(t)
			res := requisitar(t, clienteSemSeguir(t), http.MethodPost, s.servidor.URL+webui.RotaSetup, tc.campos)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusUnprocessableEntity)
			}
			if corpo := corpoDe(t, res); !strings.Contains(corpo, tc.querTrecho) {
				t.Errorf("corpo não contém %q; quer o erro reexibido no formulário", tc.querTrecho)
			}

			// Nada foi gravado: o próximo GET continua no setup.
			existe, err := s.servico.Existe(context.Background())
			if err != nil {
				t.Fatalf("Existe: erro = %v, quer nil", err)
			}
			if existe {
				t.Error("admin gravado apesar do formulário inválido")
			}
		})
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		campos      url.Values
		querStatus  int
		querDestino string
	}{
		"senha certa abre sessão e vai ao painel": {
			campos:      url.Values{"usuario": {usuario}, "senha": {senha}},
			querStatus:  http.StatusSeeOther,
			querDestino: webui.RotaPainel,
		},
		"senha certa respeita o destino pedido": {
			campos:      url.Values{"usuario": {usuario}, "senha": {senha}, "destino": {"/admin/upstreams"}},
			querStatus:  http.StatusSeeOther,
			querDestino: "/admin/upstreams",
		},
		"destino absoluto de outro host é ignorado": {
			campos:      url.Values{"usuario": {usuario}, "senha": {senha}, "destino": {"https://exemplo.invalido/roubar"}},
			querStatus:  http.StatusSeeOther,
			querDestino: webui.RotaPainel,
		},
		"destino com duas barras é ignorado": {
			campos:      url.Values{"usuario": {usuario}, "senha": {senha}, "destino": {"//exemplo.invalido/roubar"}},
			querStatus:  http.StatusSeeOther,
			querDestino: webui.RotaPainel,
		},
		"senha errada devolve 401": {
			campos:     url.Values{"usuario": {usuario}, "senha": {"senha-que-nao-e-a-certa"}},
			querStatus: http.StatusUnauthorized,
		},
		"usuário inexistente devolve o mesmo 401": {
			campos:     url.Values{"usuario": {"ninguem"}, "senha": {senha}},
			querStatus: http.StatusUnauthorized,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			s := montarSistema(t)
			criarAdmin(t, clienteSemSeguir(t), s)

			cliente := clienteSemSeguir(t)
			res := requisitar(t, cliente, http.MethodPost, s.servidor.URL+webui.RotaLogin, tc.campos)
			if res.StatusCode != tc.querStatus {
				t.Fatalf("status = %d, quer %d", res.StatusCode, tc.querStatus)
			}
			if tc.querStatus != http.StatusSeeOther {
				if temCookieDeSessao(res) {
					t.Error("cookie de sessão emitido numa tentativa recusada")
				}
				return
			}
			if destino := res.Header.Get("Location"); destino != tc.querDestino {
				t.Errorf("Location = %q, quer %q", destino, tc.querDestino)
			}
			conferirCookie(t, res)

			// A sessão emitida abre a rota protegida.
			protegida := pegar(t, cliente, s.servidor.URL+webui.RotaPainel)
			if protegida.StatusCode != http.StatusOK {
				t.Fatalf("painel com sessão: status = %d, quer %d", protegida.StatusCode, http.StatusOK)
			}
			if corpo := corpoDe(t, protegida); !strings.Contains(corpo, usuario) {
				t.Errorf("corpo do painel = %q, quer o usuário da sessão", corpo)
			}
		})
	}
}

func TestProteger_SemSessaoVaiParaLoginComDestino(t *testing.T) {
	t.Parallel()

	s := montarSistema(t)
	criarAdmin(t, clienteSemSeguir(t), s)

	// Cliente novo: tem admin cadastrado, não tem cookie.
	res := pegar(t, clienteSemSeguir(t), s.servidor.URL+"/admin/upstreams?pagina=2")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusSeeOther)
	}
	destino := res.Header.Get("Location")
	if !strings.HasPrefix(destino, webui.RotaLogin+"?destino=") {
		t.Fatalf("Location = %q, quer %s com o destino original", destino, webui.RotaLogin)
	}
	if !strings.Contains(destino, url.QueryEscape("/admin/upstreams?pagina=2")) {
		t.Errorf("Location = %q, quer o caminho original preservado", destino)
	}
}

func TestProteger_SessaoExpiradaNaoAutentica(t *testing.T) {
	t.Parallel()

	// Relógio injetado, sem dormir: a sessão é aberta "agora" e conferida depois
	// da validade dela. A data é no futuro porque o Expires do cookie sai do
	// mesmo relógio, e um jarro de cookie descarta o que já venceu pelo relógio
	// de verdade — o teste ficaria verde por acidente, medindo o jarro.
	agora := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	relogio := func() time.Time { return agora }

	s := montarSistema(t, admin.ComDuracaoSessao(time.Hour), admin.ComRelogio(func() time.Time { return relogio() }))
	cliente := clienteSemSeguir(t)
	criarAdmin(t, cliente, s)

	if res := pegar(t, cliente, s.servidor.URL+webui.RotaPainel); res.StatusCode != http.StatusOK {
		t.Fatalf("painel com sessão nova: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}

	agora = agora.Add(2 * time.Hour)

	res := pegar(t, cliente, s.servidor.URL+webui.RotaPainel)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("painel com sessão vencida: status = %d, quer %d", res.StatusCode, http.StatusSeeOther)
	}
	if destino := res.Header.Get("Location"); !strings.HasPrefix(destino, webui.RotaLogin) {
		t.Errorf("Location = %q, quer o login", destino)
	}
}

func TestSair_EncerraASessao(t *testing.T) {
	t.Parallel()

	s := montarSistema(t)
	cliente := clienteSemSeguir(t)
	criarAdmin(t, cliente, s)

	res := requisitar(t, cliente, http.MethodPost, s.servidor.URL+webui.RotaSair, url.Values{})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusSeeOther)
	}

	// O mesmo cliente, com o cookie que o logout expirou, já não passa.
	depois := pegar(t, cliente, s.servidor.URL+webui.RotaPainel)
	if depois.StatusCode != http.StatusSeeOther {
		t.Fatalf("painel depois do logout: status = %d, quer %d", depois.StatusCode, http.StatusSeeOther)
	}
}

func TestSessao_TokenGravadoComoHash(t *testing.T) {
	t.Parallel()

	// Um token inventado, ainda que bem formado, não vira sessão: o banco guarda
	// o hash e a busca é por ele.
	s := montarSistema(t)
	criarAdmin(t, clienteSemSeguir(t), s)

	if _, err := s.servico.Sessao(context.Background(), "token-que-nunca-foi-emitido"); err == nil {
		t.Error("erro = nil, quer sessão inválida")
	}
	if admin.HashToken("a") == admin.HashToken("b") {
		t.Error("hashes iguais para tokens diferentes")
	}
}

// --- apoio ---

func criarAdmin(t *testing.T, cliente *http.Client, s sistema) {
	t.Helper()
	res := requisitar(t, cliente, http.MethodPost, s.servidor.URL+webui.RotaSetup, url.Values{
		"usuario":   {usuario},
		"senha":     {senha},
		"confirmar": {senha},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusSeeOther, corpoDe(t, res))
	}
}

func conferirCookie(t *testing.T, res *http.Response) {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name != admin.NomeCookie {
			continue
		}
		if !c.HttpOnly {
			t.Error("cookie de sessão sem HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, quer Lax", c.SameSite)
		}
		// A URL pública do sistema de teste é http, então Secure fica desligado:
		// ligá-lo aqui faria o cookie nunca chegar em desenvolvimento local.
		if c.Secure {
			t.Error("cookie Secure sobre URL pública http")
		}
		return
	}
	t.Error("nenhum cookie de sessão na resposta")
}

func temCookieDeSessao(res *http.Response) bool {
	for _, c := range res.Cookies() {
		if c.Name == admin.NomeCookie && c.Value != "" {
			return true
		}
	}
	return false
}
