package endpoint

import (
	"log/slog"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Teste interno (package endpoint, não endpoint_test): observar é um método
// não exportado, e a alternativa — subir um *mcp.Server inteiro só para
// alcançá-lo — testaria o SDK, não o recover.

// observadorQuePanica é um Observador cujo Observar sempre entra em panic — o
// dublê que prova que o gancho de captura não pode derrubar a chamada de
// ferramenta por causa de um bug de quem implementa a interface.
type observadorQuePanica struct{ chamado chan struct{} }

func (o *observadorQuePanica) Observar(Chamada) {
	close(o.chamado)
	panic("observador quebrado")
}

// TestObservar_RecuperaDePanicSemDerrubarAChamada prova que um Observador que
// entra em panic não impede o handler de tools/call de terminar: o recover em
// observar absorve o panic e só loga.
func TestObservar_RecuperaDePanicSemDerrubarAChamada(t *testing.T) {
	t.Parallel()

	obs := &observadorQuePanica{chamado: make(chan struct{})}
	s := &Servidores{log: slog.New(slog.DiscardHandler), obs: obs}

	reg := Registro{ID: 1, Slug: "pessoal"}
	f := capturada{upstreamID: 1, upstreamNome: "falso", nomeExposto: "somar", nomeOriginal: "somar"}

	terminou := make(chan struct{})
	go func() {
		defer close(terminou)
		s.observar(reg, f, &mcp.CallToolRequest{}, time.Now(), 0, nil, nil)
	}()

	select {
	case <-terminou:
	case <-time.After(5 * time.Second):
		t.Fatal("observar não voltou depois do panic do Observador")
	}

	select {
	case <-obs.chamado:
	default:
		t.Fatal("o Observador nem chegou a ser chamado")
	}
}
