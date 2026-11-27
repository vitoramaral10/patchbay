package upstream

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRevisoesComparaComAMaisNova(t *testing.T) {
	t.Parallel()

	sdk := revisaoDoSDK()
	if sdk < "2026-07-28" {
		t.Fatalf("revisaoDoSDK = %q, quer pelo menos 2026-07-28", sdk)
	}
	casos := []struct {
		nome      string
		r         Revisoes
		negociada string
		atras     bool
	}{
		{"sem leitura do site, igual ao SDK", Revisoes{DoSDK: sdk}, sdk, false},
		{"sem leitura do site, abaixo do SDK", Revisoes{DoSDK: sdk}, "2025-11-25", true},
		{"site com spec nova que o SDK não fala", Revisoes{DoSDK: sdk, Publicada: "2099-01-01"}, sdk, true},
		{"sem sessão não acusa", Revisoes{DoSDK: sdk, Publicada: "2099-01-01"}, "", false},
	}
	for _, c := range casos {
		if got := c.r.Atras(c.negociada); got != c.atras {
			t.Errorf("%s: Atras(%q) = %v, quer %v", c.nome, c.negociada, got, c.atras)
		}
	}
}

func TestAvisoDeRevisao(t *testing.T) {
	t.Parallel()

	sdk := revisaoDoSDK()
	antigo := Linha{Registro: Registro{Nome: "antigo"}, Protocolo: "2025-11-25", Desatualizada: true}
	emDia := Linha{Registro: Registro{Nome: "em-dia"}, Protocolo: sdk}

	a := avisoDeRevisao([]Linha{antigo, emDia}, Revisoes{DoSDK: sdk})
	if a == nil || !strings.HasPrefix(a.Titulo, "1 MCP numa revisão antiga") ||
		!strings.Contains(a.Texto, "antigo (2025-11-25)") || strings.Contains(a.Texto, "em-dia") ||
		strings.Contains(a.Texto, "atualize o patchbay") {
		t.Fatalf("aviso = %+v", a)
	}
	if avisoDeRevisao([]Linha{emDia}, Revisoes{DoSDK: sdk}) != nil {
		t.Error("sem ninguém atrás e o patchbay em dia, não deve haver aviso")
	}
	// Spec nova que nem o patchbay fala: o aviso manda atualizar ele primeiro.
	b := avisoDeRevisao([]Linha{emDia}, Revisoes{DoSDK: sdk, Publicada: "2099-01-01"})
	if b == nil || !strings.Contains(b.Texto, "atualize o patchbay primeiro") || !strings.Contains(b.Titulo, "2099-01-01") {
		t.Fatalf("aviso com spec nova = %+v", b)
	}

	var html bytes.Buffer
	if err := revisaoDaMCP("2025-11-25", true).Render(context.Background(), &html); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html.String(), "desatualizada") {
		t.Errorf("célula da revisão antiga = %s, quer o selo desatualizada", html.String())
	}
}
