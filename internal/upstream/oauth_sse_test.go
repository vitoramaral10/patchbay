package upstream_test

import (
	"context"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestOAuth_UpstreamSSE fecha a fatia 14 com a 7: o SSE legado não tem campo de
// OAuth no transporte do SDK, então o token entra por RoundTripper — e o
// consentimento é o mesmo fluxo, pela mesma tela.
func TestOAuth_UpstreamSSE(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	recurso := novoRecursoProtegido(t, as, true, "consultar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		f := formOAuth("legado-oauth", recurso.URLMCP)
		f.Tipo = upstream.TipoSSE
		return []upstream.Form{f}
	}, opcoesAmbiente{})

	const id int64 = 1
	esperarEstado(t, a.gerente, id, upstream.EstadoSemConsentimento)
	autorizarPelaUI(t, a, id)
	esperarEstado(t, a.gerente, id, upstream.EstadoPronto)

	if f := a.gerente.Ferramentas(id); len(f) != 1 || f[0].Name != "consultar" {
		t.Fatalf("ferramentas = %v, quer [consultar]", f)
	}
	estado, err := a.repo.EstadoOAuth(context.Background(), id)
	if err != nil {
		t.Fatalf("estado OAuth: erro = %v, quer nil", err)
	}
	if !estado.Consentido {
		t.Error("consentido = false, quer true")
	}
}
