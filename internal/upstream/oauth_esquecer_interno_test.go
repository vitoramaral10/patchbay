// Este arquivo prova, por dentro (package upstream), que Esquecer solta um
// fetcher preso esperando consentimento — o botão de remover ou reconfigurar
// não pode ficar pendurado atrás de um upstream que nunca respondeu — e que a
// chamada é idempotente.
package upstream

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// TestEsquecer_SoltaFetcherBloqueadoEEhIdempotente reproduz o upstream sendo
// removido (ou reconfigurado) enquanto buscarCodigo está preso esperando o
// callback: sem s.parar, essa goroutine ficaria de pé para sempre, presa num
// pedido que ninguém mais vai completar — o mesmo tipo de vazamento que a
// fatia já evita no watchdog de conexão.
func TestEsquecer_SoltaFetcherBloqueadoEEhIdempotente(t *testing.T) {
	t.Parallel()

	b := NovoBrokerOAuth(&cofreEspiao{}, "https://patchbay.exemplo", slog.New(slog.DiscardHandler))
	cfg := Config{ID: 1, Nome: "preso", Tipo: TipoHTTP, URL: "https://exemplo.invalido/mcp"}
	s := b.sessao(cfg)

	// Um pedido pendente, mas sem ninguém para responder: o fetcher bloqueia
	// esperando resposta, cancelamento, o prazo ou s.parar.
	p := &pedidoConsentimento{
		upstreamID: cfg.ID,
		criadoEm:   b.relogio.Agora(),
		urlPronta:  make(chan string, 1),
		resposta:   make(chan respostaConsentimento, 1),
		cancelado:  make(chan struct{}),
	}
	s.mu.Lock()
	s.pendente = p
	s.mu.Unlock()

	terminou := make(chan error, 1)
	go func() {
		_, err := s.buscarCodigo(context.Background(),
			&auth.AuthorizationArgs{URL: "https://as.exemplo/authorize?state=abc123"})
		terminou <- err
	}()

	b.Esquecer(cfg.ID)

	select {
	case err := <-terminou:
		if !errors.Is(err, ErrSemConsentimento) {
			t.Errorf("erro do fetcher = %v, quer %v", err, ErrSemConsentimento)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("buscarCodigo não voltou depois de Esquecer — goroutine presa")
	}

	// Idempotente: a segunda chamada não encontra mais a sessão no mapa (a
	// primeira já a removeu) e não pode fechar s.parar de novo nem travar.
	b.Esquecer(cfg.ID)
}
