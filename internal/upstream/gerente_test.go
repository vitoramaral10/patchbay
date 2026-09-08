package upstream_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestGerente_UpstreamQuePenduraViraDegradado é a evidência de que a conexão não
// está no caminho da requisição e de que o Connect abandonado (issue #1189 do
// go-sdk) não segura o supervisor: o upstream chega a degradado dentro do
// timeout, e Chamar responde na hora com ErrIndisponivel em vez de esperar.
func TestGerente_UpstreamQuePenduraViraDegradado(t *testing.T) {
	t.Parallel()

	solto := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-solto:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(solto)
		ts.Close()
	})

	mudou := make(chan struct{}, 8)
	cfg := upstream.Config{
		ID: 1, Nome: "que-pendura", Tipo: upstream.TipoHTTP,
		URL: ts.URL, Timeout: 200 * time.Millisecond,
	}
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComIntervaloTentativa(50*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
	)

	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})

	// Chamar não espera pela conexão: responde agora, com erro legível.
	_, err := sut.Chamar(ctx, 1, "qualquer", nil)
	if !errors.Is(err, upstream.ErrIndisponivel) {
		t.Fatalf("erro de Chamar = %v, quer %v", err, upstream.ErrIndisponivel)
	}
	if f := sut.Ferramentas(1); len(f) != 0 {
		t.Errorf("ferramentas = %d, quer 0", len(f))
	}

	select {
	case <-mudou:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream não mudou de estado em 10s")
	}

	situacoes := sut.Situacoes()
	if len(situacoes) != 1 {
		t.Fatalf("situações = %d, quer 1", len(situacoes))
	}
	if situacoes[0].Estado != upstream.EstadoDegradado {
		t.Errorf("estado = %q, quer %q", situacoes[0].Estado, upstream.EstadoDegradado)
	}
	if situacoes[0].UltimoErro == "" {
		t.Error("último erro vazio, quer o motivo para a UI mostrar")
	}
}

// TestGerente_ConfiguracaoInvalidaFicaForaDaSupervisao garante que um upstream
// mal configurado não impede os outros de subir.
func TestGerente_ConfiguracaoInvalidaFicaForaDaSupervisao(t *testing.T) {
	t.Parallel()

	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{
		{ID: 1, Nome: "sem-url", Tipo: upstream.TipoHTTP, Timeout: time.Second},
		{ID: 2, Nome: "stdio", Tipo: upstream.TipoSTDIO, Timeout: time.Second},
	})

	if got := len(sut.Situacoes()); got != 0 {
		t.Fatalf("upstreams supervisionados = %d, quer 0", got)
	}
	_, err := sut.Chamar(context.Background(), 1, "x", nil)
	if !errors.Is(err, upstream.ErrDesconhecido) {
		t.Fatalf("erro = %v, quer %v", err, upstream.ErrDesconhecido)
	}
}
