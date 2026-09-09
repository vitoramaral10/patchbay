package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// TestCallbackOAuthUpstream_ProtegidoNaAplicacaoReal monta a aplicação de
// verdade (montar, não o ambiente reduzido de internal/upstream) e prova as
// duas pontas do portão sobre o callback de OAuth de upstream:
//
//   - sem sessão de admin, o navegador vai para o login, e o code/state do
//     callback não vazam para ?destino= (a correção da revisão);
//   - com sessão de admin, o caminho chega mesmo ao handler de callback — a
//     rota está de verdade atrás de Proteger na composição real de
//     cmd/patchbay, e não só no mux isolado que os testes de
//     internal/upstream montam à mão.
func TestCallbackOAuthUpstream_ProtegidoNaAplicacaoReal(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	callback := webui.RotaCallbackOAuthUpstream + "?code=codigo-de-uso-unico&state=inventado"

	// Sem sessão: cliente à parte, sem o cookie que u.setup abriu no jar de u.
	semSessao := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.url+callback, nil)
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	res, err := semSessao.Do(req)
	if err != nil {
		t.Fatalf("GET callback sem sessão: erro = %v, quer nil", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status sem sessão = %d, quer %d", res.StatusCode, http.StatusSeeOther)
	}
	destino := res.Header.Get("Location")
	if !strings.HasPrefix(destino, webui.RotaLogin+"?destino=") {
		t.Fatalf("Location = %q, quer %s com destino", destino, webui.RotaLogin)
	}
	if strings.Contains(destino, "codigo-de-uso-unico") {
		t.Errorf("Location = %q contém o code do callback, quer só o caminho", destino)
	}
	if !strings.Contains(destino, url.QueryEscape(webui.RotaCallbackOAuthUpstream)) {
		t.Errorf("Location = %q, quer o caminho do callback preservado (sem a query)", destino)
	}

	// Com sessão: o cliente de u já tem o cookie do setup, e segue redirects —
	// o que sobra ao final é a lista de upstreams com o aviso do callback, a
	// prova de que a rota realmente chega ao handler.
	comSessao := u.abrir(t, callback)
	if comSessao.StatusCode != http.StatusOK {
		t.Fatalf("status com sessão = %d, quer %d (a rota chegou ao handler e a lista renderizou)",
			comSessao.StatusCode, http.StatusOK)
	}
	if got := comSessao.Request.URL.Path; got != webui.RotaUpstreams {
		t.Errorf("caminho final = %q, quer %q", got, webui.RotaUpstreams)
	}
	if got := comSessao.Request.URL.Query().Get("aviso"); got != "consentimento_invalido" {
		t.Errorf("aviso final = %q, quer consentimento_invalido (state inventado)", got)
	}
}
