package upstream_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// formOAuth é o formulário mínimo de um upstream HTTP no modo OAuth.
func formOAuth(nome, urlMCP string) upstream.Form {
	return upstream.Form{
		Nome:       nome,
		Tipo:       upstream.TipoHTTP,
		URL:        urlMCP,
		TimeoutMS:  upstream.TimeoutPadraoMS,
		Habilitado: true,
		Modo:       upstream.ModoOAuth,
	}
}

// autorizarPelaUI faz o que o admin faz: clica em "Autorizar", segue para o
// provedor, e volta pelo callback.
//
// É o fluxo inteiro pela borda HTTP, e não pelas funções internas, porque o que
// a fatia entrega é justamente a ponte entre um serviço e um navegador que está
// noutra máquina — testar por dentro pularia exatamente a parte difícil.
func autorizarPelaUI(t *testing.T, a *ambiente, id int64) {
	t.Helper()

	cliente := clienteSemSeguir()
	rota := a.admin.URL + webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)

	// 0. A tela. O botão só existe porque ela o desenha, e uma tela que entra em
	// panic ao renderizar passaria despercebida num teste que só chama a rota do
	// POST.
	tela, err := cliente.Get(rota)
	if err != nil {
		t.Fatalf("GET detalhe: erro = %v, quer nil", err)
	}
	corpo, err := io.ReadAll(tela.Body)
	_ = tela.Body.Close()
	if err != nil {
		t.Fatalf("ler detalhe: erro = %v, quer nil", err)
	}
	if tela.StatusCode != http.StatusOK {
		t.Fatalf("status do detalhe = %d, quer %d", tela.StatusCode, http.StatusOK)
	}
	html := string(corpo)
	if !strings.Contains(html, "/autorizar") {
		t.Fatal("tela de detalhe sem o botão de autorizar")
	}
	if !strings.Contains(html, "Consentimento OAuth") {
		t.Error("tela de detalhe sem o cartão de consentimento")
	}

	// 1. O clique. A resposta é o redirecionamento para o authorization server,
	// com a URL que o go-sdk montou — com state e code_challenge dentro.
	resp, err := cliente.Post(rota+"/autorizar", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST autorizar: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status do autorizar = %d, quer %d", resp.StatusCode, http.StatusSeeOther)
	}
	destino := resp.Header.Get("Location")
	if !strings.Contains(destino, "code_challenge=") || !strings.Contains(destino, "state=") {
		t.Fatalf("URL de autorização = %q, quer state e code_challenge (PKCE)", destino)
	}

	// 2. O provedor. Ele devolve o navegador para o callback no hostname público.
	resp, err = cliente.Get(destino)
	if err != nil {
		t.Fatalf("GET no provedor: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status do provedor = %d, quer %d", resp.StatusCode, http.StatusFound)
	}
	volta := resp.Header.Get("Location")
	if !strings.HasPrefix(volta, a.admin.URL+webui.RotaCallbackOAuthUpstream) {
		t.Fatalf("callback = %q, quer %q", volta, a.admin.URL+webui.RotaCallbackOAuthUpstream)
	}

	// 3. O callback. Ele entrega o code à tentativa que espera por ele e volta
	// para a tela do upstream.
	resp, err = cliente.Get(volta)
	if err != nil {
		t.Fatalf("GET callback: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status do callback = %d, quer %d", resp.StatusCode, http.StatusSeeOther)
	}
	if got := resp.Header.Get("Location"); !strings.Contains(got, "aviso=autorizado") {
		t.Errorf("destino do callback = %q, quer aviso=autorizado", got)
	}
}

// TestOAuth_ConsentimentoPontaAPonta é a fatia 7 e a fatia 8 juntas, pelo mesmo
// caminho que o admin percorre.
//
// Sem client_id informado e com a URL pública em http, a ordem do SDK (CIMD →
// pré-registrado → DCR) cai no registro dinâmico — e o client_id que o provedor
// emitiu tem que ficar gravado, senão o próximo consentimento registraria outro
// cliente e o provedor acumularia lixo.
func TestOAuth_ConsentimentoPontaAPonta(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	recurso := novoRecursoProtegido(t, as, false, "buscar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formOAuth("notion", recurso.URLMCP)}
	}, opcoesAmbiente{})

	const id int64 = 1

	// Antes do consentimento o upstream para em sem_consentimento sem nunca
	// chegar a falar com o servidor: o portão de Preparar barra a tentativa na
	// hora, porque não há pendente nem fonte viva — e o que falta é uma pessoa
	// autorizando, não mais uma requisição.
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)
	if s, _ := a.gerente.Situacao(id); s.ProximaEm != (time.Time{}) {
		t.Errorf("próxima tentativa = %v, quer zero (não se volta de sem_consentimento por tempo)", s.ProximaEm)
	}

	autorizarPelaUI(t, a, id)

	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)
	if f := a.gerente.Ferramentas(id); len(f) != 1 || f[0].Name != "buscar" {
		t.Fatalf("ferramentas = %v, quer [buscar]", f)
	}

	// Fatia 8: o client_id do registro dinâmico ficou gravado, e a tela sabe
	// dizer de onde ele veio sem decifrar nada.
	estado, err := a.repo.EstadoOAuth(context.Background(), id)
	if err != nil {
		t.Fatalf("estado OAuth: erro = %v, quer nil", err)
	}
	if estado.Registro != upstream.RegistroDCR {
		t.Errorf("registro = %q, quer %q", estado.Registro, upstream.RegistroDCR)
	}
	if !strings.HasPrefix(estado.ClientIDEfetivo, "dcr-") {
		t.Errorf("client_id efetivo = %q, quer o emitido pelo registro dinâmico", estado.ClientIDEfetivo)
	}
	if !estado.Consentido {
		t.Error("consentido = false, quer true")
	}
	if estado.ExpiraEm.IsZero() {
		t.Error("expira_em zerado, quer o prazo que o provedor devolveu")
	}
	if n := as.idasAoRegistro.Load(); n != 1 {
		t.Errorf("registros dinâmicos = %d, quer 1", n)
	}

	// A concessão está cifrada no disco: quem abre o arquivo .db não encontra o
	// token.
	acesso := colunaOAuthCrua(t, a.st, id, "access_token_cifrado")
	refresh := colunaOAuthCrua(t, a.st, id, "refresh_token_cifrado")
	for nome, valor := range map[string]string{"access": acesso, "refresh": refresh} {
		if valor == "" {
			t.Fatalf("coluna de %s vazia, quer valor cifrado", nome)
		}
		if !strings.HasPrefix(valor, "pbc1:") {
			t.Errorf("coluna de %s = %q, quer o formato cifrado pbc1:", nome, valor)
		}
		if strings.Contains(valor, "acesso-") || strings.Contains(valor, "refresh-") {
			t.Errorf("coluna de %s carrega o token em claro", nome)
		}
	}

	// E volta em claro pelo repositório, que é o que a supervisão consome.
	c, ok, err := a.repo.Concessao(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("concessão: ok = %v, erro = %v, quer true e nil", ok, err)
	}
	if !strings.HasPrefix(c.Token.Acesso.Revelar(), "acesso-") {
		t.Errorf("access token decifrado = %q, quer o emitido pelo provedor", c.Token.Acesso.Revelar())
	}
	if c.URLToken != as.URL+"/token" {
		t.Errorf("url do token = %q, quer %q", c.URLToken, as.URL+"/token")
	}
}

// TestOAuth_ClientePreRegistrado cobre o caminho do Google: o provedor não
// registra cliente dinamicamente, e o client_id e o client_secret vêm colados no
// formulário.
func TestOAuth_ClientePreRegistrado(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	as.preRegistrar("cliente-colado", "segredo-colado")
	recurso := novoRecursoProtegido(t, as, false, "listar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		f := formOAuth("google", recurso.URLMCP)
		f.OAuthClientID = "cliente-colado"
		f.OAuthSegredo = "segredo-colado"
		return []upstream.Form{f}
	}, opcoesAmbiente{})

	const id int64 = 1
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)
	autorizarPelaUI(t, a, id)
	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)

	estado, err := a.repo.EstadoOAuth(context.Background(), id)
	if err != nil {
		t.Fatalf("estado OAuth: erro = %v, quer nil", err)
	}
	if estado.Registro != upstream.RegistroPreRegistrado {
		t.Errorf("registro = %q, quer %q", estado.Registro, upstream.RegistroPreRegistrado)
	}
	if estado.ClientIDEfetivo != "cliente-colado" {
		t.Errorf("client_id efetivo = %q, quer cliente-colado", estado.ClientIDEfetivo)
	}
	if !estado.SegredoDefinido {
		t.Error("segredo definido = false, quer true")
	}
	if n := as.idasAoRegistro.Load(); n != 0 {
		t.Errorf("registros dinâmicos = %d, quer 0 (o cliente já estava registrado)", n)
	}

	// O segredo do cliente também está cifrado no disco.
	if v := colunaOAuthCrua(t, a.st, id, "client_secret_cifrado"); strings.Contains(v, "segredo-colado") {
		t.Error("client_secret gravado em claro")
	}
}

// TestOAuth_TokenRevogadoVoltaParaSemConsentimento é o requisito de "sem loop":
// quando o provedor recusa o refresh, o upstream para e espera o clique, em vez
// de reconectar em laço para tomar 401.
func TestOAuth_TokenRevogadoVoltaParaSemConsentimento(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	// Token curto e renovação frequente: é o que faz a revogação aparecer no
	// tempo do teste em vez de na hora em que um token de produção venceria.
	as.segundos = 2
	recurso := novoRecursoProtegido(t, as, false, "buscar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formOAuth("sentry", recurso.URLMCP)}
	}, opcoesAmbiente{
		margemRenovacao: 1500 * time.Millisecond,
		tiqueRenovacao:  50 * time.Millisecond,
	})

	const id int64 = 1
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)
	autorizarPelaUI(t, a, id)
	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)

	// O provedor revoga: o access token deixa de valer e o refresh é recusado
	// com invalid_grant, que é como a RFC 6749 diz "este consentimento acabou".
	as.revogar()
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)

	// A partir daqui a supervisão para de tentar. O contador do provedor é a
	// prova: sem a parada, o backoff de 20ms produziria dezenas de tentativas
	// nesta janela.
	antes := recurso.tentativas.Load()
	aguardar(t, 500*time.Millisecond)
	depois := recurso.tentativas.Load()
	if depois-antes > 2 {
		t.Errorf("tentativas depois da revogação = %d em 500ms, quer no máximo 2 (sem laço)",
			depois-antes)
	}
	if s, _ := a.gerente.Situacao(id); s.Estado != upstream.EstadoSemConsentimento {
		t.Errorf("estado = %q, quer %q", s.Estado, upstream.EstadoSemConsentimento)
	}

	// E o clique traz de volta, sem esperar backoff nenhum.
	as.permitir()
	autorizarPelaUI(t, a, id)
	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)
}

// TestOAuth_CallbackRecusado cobre os dois desfechos ruins do callback: state que
// não pertence a nenhuma tentativa, e o provedor devolvendo erro em vez de code.
func TestOAuth_CallbackRecusado(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	recurso := novoRecursoProtegido(t, as, false, "buscar")
	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formOAuth("notion", recurso.URLMCP)}
	}, opcoesAmbiente{})

	casos := map[string]struct {
		query       string
		querDestino string
	}{
		"state que ninguém emitiu": {
			query:       "?code=qualquer&state=inventado",
			querDestino: webui.RotaUpstreams + "?aviso=consentimento_invalido",
		},
		"callback sem state nenhum": {
			query:       "?code=qualquer",
			querDestino: webui.RotaUpstreams + "?aviso=consentimento_invalido",
		},
		"erro do provedor sem tentativa em curso": {
			query:       "?error=access_denied&state=inventado",
			querDestino: webui.RotaUpstreams + "?aviso=consentimento_invalido",
		},
	}

	cliente := clienteSemSeguir()
	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			resp, err := cliente.Get(a.admin.URL + webui.RotaCallbackOAuthUpstream + tc.query)
			if err != nil {
				t.Fatalf("GET callback: erro = %v, quer nil", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status = %d, quer %d", resp.StatusCode, http.StatusSeeOther)
			}
			if got := resp.Header.Get("Location"); got != tc.querDestino {
				t.Errorf("destino = %q, quer %q", got, tc.querDestino)
			}
		})
	}
}

// TestOAuth_CallbackRepetidoComMesmoState prova o uso único do registro de
// state: o segundo GET no mesmo callback — o navegador voltando duas vezes por
// um duplo clique, ou um replay — não entrega o code de novo. O primeiro já
// apagou o state do registro, e o segundo encontra exatamente o que um state
// que ninguém emitiu encontraria.
func TestOAuth_CallbackRepetidoComMesmoState(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	recurso := novoRecursoProtegido(t, as, false, "buscar")
	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formOAuth("notion", recurso.URLMCP)}
	}, opcoesAmbiente{})

	const id int64 = 1
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)

	cliente := clienteSemSeguir()
	rota := a.admin.URL + webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)

	resp, err := cliente.Post(rota+"/autorizar", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST autorizar: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	destinoProvedor := resp.Header.Get("Location")

	resp, err = cliente.Get(destinoProvedor)
	if err != nil {
		t.Fatalf("GET no provedor: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	callback := resp.Header.Get("Location")

	// Primeiro GET: consome o state e completa o consentimento.
	resp, err = cliente.Get(callback)
	if err != nil {
		t.Fatalf("primeiro GET callback: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Location"); !strings.Contains(got, "aviso=autorizado") {
		t.Fatalf("primeiro callback: destino = %q, quer aviso=autorizado", got)
	}

	// Segundo GET no mesmo callback: o state já foi consumido.
	resp, err = cliente.Get(callback)
	if err != nil {
		t.Fatalf("segundo GET callback: erro = %v, quer nil", err)
	}
	_ = resp.Body.Close()
	querDestino := webui.RotaUpstreams + "?aviso=consentimento_invalido"
	if got := resp.Header.Get("Location"); got != querDestino {
		t.Errorf("segundo callback: destino = %q, quer %q", got, querDestino)
	}

	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)
}

// TestOAuth_MetadataDeClienteExigeHTTPS documenta a decisão: sobre http o Client
// ID Metadata Document não é publicado, porque um client_id que é uma URL só vale
// como identidade se ninguém no caminho puder trocar o documento.
func TestOAuth_MetadataDeClienteExigeHTTPS(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	recurso := novoRecursoProtegido(t, as, false, "buscar")
	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{formOAuth("notion", recurso.URLMCP)}
	}, opcoesAmbiente{})

	if u := a.broker.URLMetadataCliente(); u != "" {
		t.Errorf("URL do CIMD = %q, quer vazia sobre http", u)
	}

	resp, err := http.Get(a.admin.URL + webui.RotaMetadataClienteUpstream) //nolint:noctx // teste
	if err != nil {
		t.Fatalf("GET metadata: erro = %v, quer nil", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, quer %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestOAuth_RedirectURIEhContrato fixa o caminho do callback: ele é registrado no
// provedor e mudá-lo invalida todo consentimento existente.
func TestOAuth_RedirectURIEhContrato(t *testing.T) {
	t.Parallel()

	sut := upstream.NovoBrokerOAuth(nil, "https://patchbay.exemplo/", nil)
	if got, quer := sut.RedirectURI(), "https://patchbay.exemplo/admin/upstreams/oauth/callback"; got != quer {
		t.Errorf("redirect_uri = %q, quer %q", got, quer)
	}
	if got, quer := sut.URLMetadataCliente(), "https://patchbay.exemplo/oauth/patchbay-cliente.json"; got != quer {
		t.Errorf("url do CIMD = %q, quer %q", got, quer)
	}
}

// aguardar espera um intervalo sem time.Sleep, que o linter proíbe e com razão:
// aqui a espera é o próprio objeto do teste — provar que nada acontece nela.
func aguardar(t *testing.T, d time.Duration) {
	t.Helper()
	tempo := time.NewTimer(d)
	defer tempo.Stop()
	<-tempo.C
}
