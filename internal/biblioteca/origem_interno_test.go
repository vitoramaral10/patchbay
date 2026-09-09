package biblioteca

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// O único teste interno do pacote, e é de propósito.
//
// O que ele precisa provar é o que acontece **depois** que a lista vence, e o
// vencimento é tempo passando. Esperar pelo relógio é proibido no projeto — e
// aqui nem funcionaria: a resolução do monotônico no Windows faz duas chamadas
// seguidas darem a mesma marca, então uma validade curtíssima seria servida como
// se estivesse fresca. Empurrar lidoEm para trás é determinístico, e o preço é
// este arquivo ser interno.
func TestListaVencidaNaoEServidaQuandoAOrigemCai(t *testing.T) {
	t.Parallel()

	indice, err := os.ReadFile(filepath.Join("testdata", "indice.html"))
	if err != nil {
		t.Fatalf("ler amostra: erro = %v, quer nil", err)
	}
	var fora atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fora.Load() {
			http.Error(w, "fora do ar", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(indice)
	}))
	t.Cleanup(ts.Close)

	o := NovaOrigem(ts.URL, time.Minute)
	if _, err := o.Listar(context.Background()); err != nil {
		t.Fatalf("primeira Listar: erro = %v, quer nil", err)
	}

	// A lista existe e está fresca. Vence-se ela e derruba-se a origem: o que a
	// decisão de não guardar nada exige é que a tela dê erro, e não que a lista
	// de um minuto atrás apareça calada como se fosse de agora.
	o.mu.Lock()
	o.lidoEm = time.Now().Add(-2 * o.validade)
	o.mu.Unlock()
	fora.Store(true)

	if _, err := o.Listar(context.Background()); !errors.Is(err, ErrOrigemIndisponivel) {
		t.Fatalf("erro = %v, quer ErrOrigemIndisponivel", err)
	}
}
