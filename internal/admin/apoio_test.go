package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// Helpers de requisição compartilhados pelos testes deste pacote.

func newJar() (http.CookieJar, error) { return cookiejar.New(nil) }

func pegar(t *testing.T, cliente *http.Client, endereco string) *http.Response {
	t.Helper()
	return requisitar(t, cliente, http.MethodGet, endereco, nil)
}

// requisitar monta e executa a requisição, já fechando o corpo no fim do teste.
//
// Formulário vai como application/x-www-form-urlencoded e com
// Sec-Fetch-Site: same-origin, que é o que um navegador manda numa submissão da
// própria página — o teste de CSRF é o que omite esse header de propósito.
func requisitar(t *testing.T, cliente *http.Client, metodo, endereco string, campos url.Values) *http.Response {
	t.Helper()

	var corpo io.Reader
	if campos != nil {
		corpo = strings.NewReader(campos.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), metodo, endereco, corpo)
	if err != nil {
		t.Fatalf("montar requisição %s %s: erro = %v, quer nil", metodo, endereco, err)
	}
	if campos != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	res, err := cliente.Do(req)
	if err != nil {
		t.Fatalf("requisição %s %s: erro = %v, quer nil", metodo, endereco, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func corpoDe(t *testing.T, res *http.Response) string {
	t.Helper()
	bytes, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ler corpo: erro = %v, quer nil", err)
	}
	return string(bytes)
}
