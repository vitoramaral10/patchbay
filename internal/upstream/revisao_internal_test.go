package upstream

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAvisoDeRevisaoAcusaSoQuemEstaAtras(t *testing.T) {
	t.Parallel()

	nova := RevisaoMaisNova()
	if nova < "2026-07-28" {
		t.Fatalf("RevisaoMaisNova = %q, quer pelo menos 2026-07-28", nova)
	}
	linhas := []Linha{
		{Registro: Registro{Nome: "antigo"}, Protocolo: "2025-11-25"},
		{Registro: Registro{Nome: "em-dia"}, Protocolo: nova},
		{Registro: Registro{Nome: "sem-sessao"}},
	}

	a := avisoDeRevisao(linhas)
	if a == nil || a.Titulo != "1 MCP numa revisão antiga da spec" ||
		!strings.Contains(a.Texto, "antigo (2025-11-25)") || strings.Contains(a.Texto, "em-dia") || strings.Contains(a.Texto, "sem-sessao") {
		t.Fatalf("aviso = %+v", a)
	}
	if avisoDeRevisao(linhas[1:]) != nil {
		t.Error("sem ninguém atrás não deve haver aviso")
	}

	var html bytes.Buffer
	if err := revisaoDaMCP("2025-11-25", true).Render(context.Background(), &html); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html.String(), "desatualizada") {
		t.Errorf("célula da revisão antiga = %s, quer o selo desatualizada", html.String())
	}
}
