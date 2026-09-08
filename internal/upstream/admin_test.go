package upstream_test

import (
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

func TestForm_ValidarCredenciais(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		bearer    cripto.Segredo
		headers   []upstream.CampoHeader
		querPassa bool
		querErro  string // chave em Erros que precisa existir
	}{
		"sem credencial nenhuma": {querPassa: true},
		"bearer simples":         {bearer: "sk-0123456789", querPassa: true},
		"bearer com espaço": {
			bearer: "sk 0123", querPassa: false, querErro: "bearer",
		},
		"bearer com quebra de linha": {
			bearer: "sk\r\nX-Injetado: sim", querPassa: false, querErro: "bearer",
		},
		"header válido": {
			headers: []upstream.CampoHeader{{Nome: "X-Api-Key", Valor: "abc"}}, querPassa: true,
		},
		"linha em branco é ignorada": {
			headers: []upstream.CampoHeader{{}, {}}, querPassa: true,
		},
		"header sem nome": {
			headers:   []upstream.CampoHeader{{Valor: "abc"}},
			querPassa: false, querErro: "headers",
		},
		"nome de header inválido": {
			headers:   []upstream.CampoHeader{{Nome: "X Api Key", Valor: "abc"}},
			querPassa: false, querErro: "headers",
		},
		"authorization é do campo de bearer": {
			headers:   []upstream.CampoHeader{{Nome: "authorization", Valor: "Bearer x"}},
			querPassa: false, querErro: "headers",
		},
		"header duplicado": {
			headers: []upstream.CampoHeader{
				{Nome: "X-Api-Key", Valor: "a"},
				{Nome: "x-api-key", Valor: "b"},
			},
			querPassa: false, querErro: "headers",
		},
		"valor com quebra de linha": {
			headers:   []upstream.CampoHeader{{Nome: "X-Api-Key", Valor: "a\r\nX-Outro: b"}},
			querPassa: false, querErro: "headers",
		},
		"header já gravado sem valor novo": {
			headers:   []upstream.CampoHeader{{Nome: "X-Api-Key", Definido: true}},
			querPassa: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := upstream.Form{
				Nome: "notion", URL: "https://exemplo.com/mcp",
				TimeoutMS: upstream.TimeoutPadraoMS,
				Bearer:    tc.bearer,
				Headers:   tc.headers,
			}
			if got := sut.Validar(); got != tc.querPassa {
				t.Fatalf("Validar() = %v, quer %v (erros: %v)", got, tc.querPassa, sut.Erros)
			}
			if tc.querErro != "" && sut.Erros[tc.querErro] == "" {
				t.Errorf("Erros[%q] vazio, quer a mensagem da tela; erros = %v", tc.querErro, sut.Erros)
			}
		})
	}
}

// TestForm_CompletarHeaders garante que o formulário mostra "definido" para o
// que já existe e ainda oferece linhas em branco para o que vai entrar.
func TestForm_CompletarHeaders(t *testing.T) {
	t.Parallel()

	sut := upstream.Form{}
	sut.CompletarHeaders([]upstream.CredencialDefinida{
		{Tipo: upstream.CredencialBearer},
		{Tipo: upstream.CredencialHeader, Nome: "X-Api-Key"},
	})

	if !sut.BearerDefinido {
		t.Error("BearerDefinido = false, quer true")
	}
	if len(sut.Headers) != 1+upstream.LinhasHeaderEmBranco {
		t.Fatalf("linhas de header = %d, quer %d", len(sut.Headers), 1+upstream.LinhasHeaderEmBranco)
	}
	if sut.Headers[0].Nome != "X-Api-Key" || !sut.Headers[0].Definido {
		t.Errorf("primeira linha = %+v, quer X-Api-Key definido", sut.Headers[0])
	}
	for i, h := range sut.Headers[1:] {
		if h.Nome != "" || h.Definido {
			t.Errorf("linha em branco %d = %+v, quer vazia", i, h)
		}
	}
}

// TestForm_CompletarHeadersPreservaODigitado: ao reexibir um formulário
// recusado, o que o admin digitou não pode ser sobrescrito pelo estado do banco.
func TestForm_CompletarHeadersPreservaODigitado(t *testing.T) {
	t.Parallel()

	sut := upstream.Form{
		Headers: []upstream.CampoHeader{{Nome: "X-Api-Key", Erro: "algum erro"}},
	}
	sut.CompletarHeaders([]upstream.CredencialDefinida{
		{Tipo: upstream.CredencialHeader, Nome: "X-Api-Key"},
	})

	vezes := 0
	for _, h := range sut.Headers {
		if h.Nome == "X-Api-Key" {
			vezes++
		}
	}
	if vezes != 1 {
		t.Errorf("X-Api-Key aparece %d vezes, quer 1", vezes)
	}
	if sut.Headers[0].Erro != "algum erro" {
		t.Errorf("erro da linha = %q, quer preservado", sut.Headers[0].Erro)
	}
	if !sut.Headers[0].Definido {
		t.Error("Definido = false, quer true: o header existe no banco")
	}
}
