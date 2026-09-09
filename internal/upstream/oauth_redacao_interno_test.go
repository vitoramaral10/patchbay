package upstream

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// TestMensagemDeFalha_NaoVazaCorpoDoTokenEndpoint fecha a porta estreita por onde
// um refresh token chegaria à tela e ao log.
//
// Quando o x/oauth2 não consegue ler a resposta do token endpoint como erro
// OAuth, o RetrieveError imprime o corpo bruto dela (token.go:212). Um provedor
// atrás de um proxy que ecoa a requisição — ou uma página de erro que repete o
// formulário enviado — poria o refresh token dentro dessa mensagem, e ela vai
// direto para ultimo_erro, para a tela do upstream e para o slog.
func TestMensagemDeFalha_NaoVazaCorpoDoTokenEndpoint(t *testing.T) {
	t.Parallel()

	const refresh = "refresh-token-super-secreto"

	casos := map[string]struct {
		causa      error
		querContem []string
		querSem    []string
	}{
		"erro comum passa inteiro": {
			causa:      errors.New("dial tcp: connection refused"),
			querContem: []string{"connection refused"},
		},
		"erro OAuth reconhecível mantém código e descrição": {
			causa: fmt.Errorf("renovar: %w", &oauth2.RetrieveError{
				ErrorCode:        "invalid_grant",
				ErrorDescription: "refresh token expired",
			}),
			querContem: []string{"invalid_grant", "refresh token expired"},
		},
		"resposta ilegível tem o corpo omitido": {
			causa: fmt.Errorf("renovar: %w", &oauth2.RetrieveError{
				Response: &http.Response{
					Status: "502 Bad Gateway",
					Body:   io.NopCloser(strings.NewReader("")),
				},
				Body: []byte("erro ao repassar grant_type=refresh_token&refresh_token=" + refresh),
			}),
			querContem: []string{"502 Bad Gateway", "corpo"},
			querSem:    []string{refresh, "grant_type"},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			got := mensagemDeFalha(tc.causa)
			for _, quer := range tc.querContem {
				if !strings.Contains(got, quer) {
					t.Errorf("mensagem = %q, quer conter %q", got, quer)
				}
			}
			for _, proibido := range tc.querSem {
				if strings.Contains(got, proibido) {
					t.Errorf("mensagem = %q, não pode conter %q", got, proibido)
				}
			}
		})
	}
}
