// Package authsrv guarda os testes de unidade do authorization server.
//
// É teste interno (package authsrv, não authsrv_test) porque o que ele exercita
// é justamente o que não é exportado: a comparação de PKCE, a normalização do
// resource do RFC 8707, a validação de redirect_uri e o balde do limitador. O
// comportamento de protocolo, esse, é exercitado ponta a ponta pelo cliente
// OAuth real do go-sdk em cmd/patchbay.
package authsrv

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func servicoDeTeste(urlPublica string) *Servico {
	return NovoServico(nil, nil, urlPublica,
		func(slug string) string { return "endpoint:" + slug },
		slog.New(slog.DiscardHandler))
}

func TestConferirPKCE(t *testing.T) {
	t.Parallel()

	const bom = "verificador-de-teste-com-mais-de-quarenta-e-tres-caracteres"
	soma := sha256.Sum256([]byte(bom))
	desafioBom := base64.RawURLEncoding.EncodeToString(soma[:])

	casos := map[string]struct {
		verificador string
		desafio     string
		querCodigo  string
	}{
		"verificador correto passa":             {verificador: bom, desafio: desafioBom},
		"verificador ausente é invalid_request": {verificador: "", desafio: desafioBom, querCodigo: ErroInvalidRequest},
		"verificador curto demais é invalid_grant": {
			verificador: "curto", desafio: desafioBom, querCodigo: ErroInvalidGrant,
		},
		"verificador errado é invalid_grant": {
			verificador: "outro-verificador-de-teste-com-mais-de-quarenta-e-tres", desafio: desafioBom,
			querCodigo: ErroInvalidGrant,
		},
		"desafio de outro pedido é invalid_grant": {
			verificador: bom, desafio: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			querCodigo: ErroInvalidGrant,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			err := conferirPKCE(tc.verificador, tc.desafio)
			if tc.querCodigo == "" {
				if err != nil {
					t.Fatalf("erro = %v, quer nil", err)
				}
				return
			}
			var oerr *ErroOAuth
			if !errors.As(err, &oerr) {
				t.Fatalf("erro = %v, quer *ErroOAuth", err)
			}
			if oerr.Codigo != tc.querCodigo {
				t.Errorf("código = %q, quer %q", oerr.Codigo, tc.querCodigo)
			}
		})
	}
}

func TestServico_slugDoRecurso(t *testing.T) {
	t.Parallel()

	sut := servicoDeTeste("https://patchbay.exemplo")

	casos := map[string]struct {
		recurso  string
		querSlug string
		querOK   bool
	}{
		"URL canônica do endpoint":     {recurso: "https://patchbay.exemplo/mcp/pessoal", querSlug: "pessoal", querOK: true},
		"barra final é tolerada":       {recurso: "https://patchbay.exemplo/mcp/pessoal/", querSlug: "pessoal", querOK: true},
		"outro host não vale":          {recurso: "https://outro.exemplo/mcp/pessoal"},
		"raiz do gateway não vale":     {recurso: "https://patchbay.exemplo/"},
		"caminho mais fundo não vale":  {recurso: "https://patchbay.exemplo/mcp/pessoal/extra"},
		"slug vazio não vale":          {recurso: "https://patchbay.exemplo/mcp/"},
		"query pendurada não vale":     {recurso: "https://patchbay.exemplo/mcp/pessoal?x=1"},
		"prefixo parecido não vale":    {recurso: "https://patchbay.exemplo.mau/mcp/pessoal"},
		"esquema diferente não vale":   {recurso: "http://patchbay.exemplo/mcp/pessoal"},
		"texto que não é URL não vale": {recurso: "pessoal"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			slug, ok := sut.slugDoRecurso(tc.recurso)
			if ok != tc.querOK {
				t.Fatalf("ok = %v, quer %v", ok, tc.querOK)
			}
			if slug != tc.querSlug {
				t.Errorf("slug = %q, quer %q", slug, tc.querSlug)
			}
		})
	}
}

func TestClientePermiteRedirect(t *testing.T) {
	t.Parallel()

	// A comparação é exata: comparação frouxa de redirect_uri é a falha clássica
	// que entrega o código a quem registrou um caminho parecido.
	sut := Cliente{RedirectURIs: []string{RedirectClaudeAI, "http://127.0.0.1:1455/callback"}}

	casos := map[string]struct {
		uri     string
		querSim bool
	}{
		"exatamente a cadastrada":         {uri: RedirectClaudeAI, querSim: true},
		"barra final a mais":              {uri: RedirectClaudeAI + "/"},
		"query pendurada":                 {uri: RedirectClaudeAI + "?x=1"},
		"prefixo da cadastrada":           {uri: "https://claude.ai/api/mcp"},
		"host parecido":                   {uri: "https://claude.ai.mau/api/mcp/auth_callback"},
		"loopback com outra porta":        {uri: "http://127.0.0.1:1456/callback"},
		"loopback com a porta cadastrada": {uri: "http://127.0.0.1:1455/callback", querSim: true},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := sut.PermiteRedirect(tc.uri); got != tc.querSim {
				t.Errorf("PermiteRedirect(%q) = %v, quer %v", tc.uri, got, tc.querSim)
			}
		})
	}
}

func TestEscopoContido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		pedido    string
		concedido string
		querSim   bool
	}{
		"igual":               {pedido: "endpoint:a", concedido: "endpoint:a", querSim: true},
		"vazio cabe em tudo":  {pedido: "", concedido: "endpoint:a", querSim: true},
		"subconjunto":         {pedido: "endpoint:a", concedido: "endpoint:a endpoint:b", querSim: true},
		"amplia não cabe":     {pedido: "endpoint:a endpoint:b", concedido: "endpoint:a"},
		"escopo desconhecido": {pedido: "admin", concedido: "endpoint:a"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := escopoContido(tc.pedido, tc.concedido); got != tc.querSim {
				t.Errorf("escopoContido(%q, %q) = %v, quer %v", tc.pedido, tc.concedido, got, tc.querSim)
			}
		})
	}
}

// relogioFalso é o relógio injetado do limitador: esperar pelo tempo real num
// teste de janela de um minuto seria um teste de um minuto.
type relogioFalso struct{ t time.Time }

func (r *relogioFalso) agora() time.Time        { return r.t }
func (r *relogioFalso) avancar(d time.Duration) { r.t = r.t.Add(d) }

func TestLimitador(t *testing.T) {
	t.Parallel()

	relogio := &relogioFalso{t: time.Unix(1_700_000_000, 0)}
	sut := novoLimitador(3, time.Minute, relogio.agora)

	for i := range 3 {
		if !sut.permitir("a") {
			t.Fatalf("requisição %d recusada, quer permitida", i+1)
		}
	}
	if sut.permitir("a") {
		t.Fatal("quarta requisição permitida, quer recusada")
	}
	// Chave diferente tem balde próprio: um cliente barulhento não derruba outro.
	if !sut.permitir("b") {
		t.Error("chave nova recusada, quer permitida")
	}

	// Um terço da janela repõe uma ficha.
	relogio.avancar(21 * time.Second)
	if !sut.permitir("a") {
		t.Error("depois da reposição parcial, recusada; quer permitida")
	}
	if sut.permitir("a") {
		t.Error("duas fichas repostas em um terço de janela, quer uma")
	}

	// Janela inteira repõe até o teto e não além.
	relogio.avancar(10 * time.Minute)
	for i := range 3 {
		if !sut.permitir("a") {
			t.Fatalf("depois da janela cheia, requisição %d recusada", i+1)
		}
	}
	if sut.permitir("a") {
		t.Error("balde encheu acima do teto")
	}
}

func TestDesafioBemFormado(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		desafio string
		querOK  bool
	}{
		"43 caracteres base64url":    {desafio: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", querOK: true},
		"com hífen e underscore":     {desafio: "-_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", querOK: true},
		"curto demais":               {desafio: "AAAA"},
		"com padding":                {desafio: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		"caractere fora do alfabeto": {desafio: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA*"},
		"vazio":                      {desafio: ""},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := desafioBemFormado(tc.desafio); got != tc.querOK {
				t.Errorf("desafioBemFormado(%q) = %v, quer %v", tc.desafio, got, tc.querOK)
			}
		})
	}
}

func TestTemMarca(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		claro  string
		marca  string
		querOK bool
	}{
		"access token do AS":        {claro: "pbat_abc", marca: MarcaAcesso, querOK: true},
		"refresh não é access":      {claro: "pbrt_abc", marca: MarcaAcesso},
		"chave de API não é access": {claro: "pbk_abc_def", marca: MarcaAcesso},
		"marca sem corpo":           {claro: "pbat_", marca: MarcaAcesso},
		"marca sem separador":       {claro: "pbatabc", marca: MarcaAcesso},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := TemMarca(tc.claro, tc.marca); got != tc.querOK {
				t.Errorf("TemMarca(%q, %q) = %v, quer %v", tc.claro, tc.marca, got, tc.querOK)
			}
		})
	}
}
