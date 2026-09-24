package upstream

import (
	"net/url"
	"testing"
)

// TestComRefreshDoGoogle: sem access_type=offline o Google consente, o upstream
// fica pronto, e uma hora depois o access token vence sem refresh token nenhum.
func TestComRefreshDoGoogle(t *testing.T) {
	t.Parallel()

	const base = "https://accounts.google.com/o/oauth2/v2/auth?client_id=c&state=s1&code_challenge=x&redirect_uri=https%3A%2F%2Fgw%2Fcb"

	casos := []struct {
		nome, entrada          string
		querAccess, querPrompt string
	}{
		{"google sem os parâmetros", base, "offline", "consent"},
		{"google respeita o que já veio", base + "&access_type=online&prompt=select_account", "online", "select_account"},
		{"outro AS fica como está", "https://auth.exemplo.com/authorize?state=s1", "", ""},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			saida := comRefreshDoGoogle(c.entrada)
			u, err := url.Parse(saida)
			if err != nil {
				t.Fatalf("url.Parse(%q) erro = %v, quer nil", saida, err)
			}
			q := u.Query()
			if got := q.Get("access_type"); got != c.querAccess {
				t.Errorf("access_type = %q, quer %q", got, c.querAccess)
			}
			if got := q.Get("prompt"); got != c.querPrompt {
				t.Errorf("prompt = %q, quer %q", got, c.querPrompt)
			}
			// state e PKCE são do SDK: mudar qualquer um invalida a troca do code.
			if got := q.Get("state"); got != "s1" {
				t.Errorf("state = %q, quer s1 intacto", got)
			}
			if c.querAccess != "" && q.Get("code_challenge") != "x" {
				t.Errorf("code_challenge = %q, quer x intacto", q.Get("code_challenge"))
			}
		})
	}
}
