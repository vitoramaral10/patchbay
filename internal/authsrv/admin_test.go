package authsrv

import "testing"

func TestMotivoRedirectInvalido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		uri     string
		querMau bool
	}{
		"a URI fixa do claude.ai passa":   {uri: RedirectClaudeAI},
		"https qualquer passa":            {uri: "https://exemplo.com/callback"},
		"http em loopback passa":          {uri: "http://127.0.0.1:8080/callback"},
		"http em localhost passa":         {uri: "http://localhost/callback"},
		"esquema próprio de app passa":    {uri: "com.exemplo.app:/oauth"},
		"http fora de loopback não passa": {uri: "http://exemplo.com/callback", querMau: true},
		"relativa não passa":              {uri: "/callback", querMau: true},
		"com fragmento não passa":         {uri: "https://exemplo.com/cb#frag", querMau: true},
		"esquema sem ponto não passa":     {uri: "meuapp:/cb", querMau: true},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			motivo := motivoRedirectInvalido(tc.uri)
			if (motivo != "") != tc.querMau {
				t.Errorf("motivo = %q, quer inválido = %v", motivo, tc.querMau)
			}
		})
	}
}

func TestFormClienteValidar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		form      FormCliente
		querPassa bool
		querErro  string
	}{
		"cadastro completo passa": {
			form: FormCliente{
				Nome:          "claude.ai",
				RedirectTexto: RedirectClaudeAI,
				EndpointIDs:   []int64{1},
			},
			querPassa: true,
		},
		"sem nome recusa": {
			form:     FormCliente{RedirectTexto: RedirectClaudeAI, EndpointIDs: []int64{1}},
			querErro: "nome",
		},
		"sem redirect recusa": {
			form:     FormCliente{Nome: "x", EndpointIDs: []int64{1}},
			querErro: "redirect",
		},
		"redirect inválido recusa": {
			form:     FormCliente{Nome: "x", RedirectTexto: "http://exemplo.com/cb", EndpointIDs: []int64{1}},
			querErro: "redirect",
		},
		"sem endpoint recusa": {
			form:     FormCliente{Nome: "x", RedirectTexto: RedirectClaudeAI},
			querErro: "endpoint",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := tc.form
			passou := sut.Validar()
			if passou != tc.querPassa {
				t.Fatalf("Validar() = %v, quer %v (erros: %v)", passou, tc.querPassa, sut.Erros)
			}
			if tc.querErro != "" && sut.Erros[tc.querErro] == "" {
				t.Errorf("erros = %v, quer erro no campo %q", sut.Erros, tc.querErro)
			}
		})
	}
}

func TestFormClienteLeVariasRedirect(t *testing.T) {
	t.Parallel()

	sut := FormCliente{
		Nome:          "claude code",
		RedirectTexto: "  " + RedirectClaudeAI + "\n\nhttp://127.0.0.1:1455/callback  \n",
		EndpointIDs:   []int64{1},
	}
	if !sut.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", sut.Erros)
	}
	quer := []string{RedirectClaudeAI, "http://127.0.0.1:1455/callback"}
	if len(sut.RedirectURIs) != len(quer) {
		t.Fatalf("RedirectURIs = %v, quer %v", sut.RedirectURIs, quer)
	}
	for i := range quer {
		if sut.RedirectURIs[i] != quer[i] {
			t.Errorf("RedirectURIs[%d] = %q, quer %q", i, sut.RedirectURIs[i], quer[i])
		}
	}
}
