package spec_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/spec"
)

func TestConsultarPegaAMaiorRevisaoValida(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<urlset>
<url><loc>https://modelcontextprotocol.io/specification/2025-11-25/changelog</loc></url>
<url><loc>https://modelcontextprotocol.io/specification/2026-07-28/basic</loc></url>
<url><loc>https://modelcontextprotocol.io/specification/9999-99-99/lixo</loc></url>
<url><loc>https://modelcontextprotocol.io/specification/draft/basic</loc></url>
</urlset>`)
	}))
	t.Cleanup(ts.Close)

	v := spec.Nova(ts.URL, slog.New(slog.DiscardHandler))
	if err := v.Consultar(context.Background()); err != nil {
		t.Fatalf("Consultar: %v", err)
	}
	if r, em := v.Publicada(); r != "2026-07-28" || em.IsZero() {
		t.Errorf("Publicada = %q, %v; quer 2026-07-28 com data de leitura", r, em)
	}
}

func TestConsultarFalhaSemTrocarALeituraBoa(t *testing.T) {
	t.Parallel()

	var fora atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fora.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `<loc>https://x/specification/2026-07-28/a</loc>`)
	}))
	t.Cleanup(ts.Close)

	v := spec.Nova(ts.URL, slog.New(slog.DiscardHandler))
	if err := v.Consultar(context.Background()); err != nil {
		t.Fatal(err)
	}
	fora.Store(true)
	if err := v.Consultar(context.Background()); err == nil {
		t.Fatal("Consultar com 502 = nil, quer erro")
	}
	if r, _ := v.Publicada(); r != "2026-07-28" {
		t.Errorf("Publicada depois da falha = %q, quer a leitura boa anterior", r)
	}
}

func TestVigiaDesligadaNaoConsulta(t *testing.T) {
	t.Parallel()

	v := spec.Nova("", slog.New(slog.DiscardHandler))
	v.Rodar(context.Background(), 0) // volta na hora: sem URL não há laço
	if r, _ := v.Publicada(); r != "" {
		t.Errorf("Publicada = %q, quer vazio", r)
	}
}
