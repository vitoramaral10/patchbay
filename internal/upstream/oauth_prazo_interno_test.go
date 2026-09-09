// Este arquivo prova, por dentro (package upstream), que um pedido de
// consentimento tem prazo próprio: um upstream fora do ar quando o admin clica
// em "Autorizar" não pode deixar a sessão de OAuth esperando para sempre.
package upstream

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// relogioAjustavel é um relógio de teste com Agora mutável direto pelo campo e
// Depois que nunca dispara sozinho — os testes deste arquivo avançam o tempo à
// mão, sem esperar por nada.
type relogioAjustavel struct{ agora time.Time }

func (r *relogioAjustavel) Agora() time.Time { return r.agora }

func (*relogioAjustavel) Depois(time.Duration) <-chan time.Time { return nil }

// TestConsentimentoPedido_PrazoProprio é o cenário do upstream fora do ar: o
// admin clica em "Autorizar", o provedor nunca responde ao callback, e o prazo
// de consentimento passa.
//
// Sem prazo próprio o pedido ficaria pendurado para sempre: ConsentimentoPedido
// continuaria verdadeiro (o watchdog do supervisor continuaria estendido para
// sempre) e Preparar continuaria deixando passar uma tentativa que já devia ter
// voltado a exigir um clique novo.
func TestConsentimentoPedido_PrazoProprio(t *testing.T) {
	t.Parallel()

	rel := &relogioAjustavel{agora: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	b := NovoBrokerOAuth(&cofreEspiao{}, "https://patchbay.exemplo", slog.New(slog.DiscardHandler),
		ComRelogioOAuth(rel), ComTempoDeConsentimento(time.Minute))
	cfg := Config{ID: 1, Nome: "fora-do-ar", Tipo: TipoHTTP, URL: "https://exemplo.invalido/mcp"}
	s := b.sessao(cfg)

	// Antes de qualquer pedido: sem fonte e sem pendente, Preparar barra.
	if err := b.Preparar(context.Background(), cfg); !errors.Is(err, ErrSemConsentimento) {
		t.Fatalf("Preparar antes do pedido: erro = %v, quer %v", err, ErrSemConsentimento)
	}

	// O clique em "Autorizar": nasce um pedido, com o relógio de agora.
	p := &pedidoConsentimento{
		upstreamID: cfg.ID,
		criadoEm:   rel.Agora(),
		urlPronta:  make(chan string, 1),
		resposta:   make(chan respostaConsentimento, 1),
		cancelado:  make(chan struct{}),
	}
	s.mu.Lock()
	s.pendente = p
	s.precisa = false
	s.mu.Unlock()

	if !b.ConsentimentoPedido(cfg.ID) {
		t.Fatal("ConsentimentoPedido = false logo após o pedido, quer true")
	}
	if err := b.Preparar(context.Background(), cfg); err != nil {
		t.Errorf("Preparar com pedido em curso: erro = %v, quer nil (a tentativa existe para completá-lo)", err)
	}

	// O provedor nunca respondeu — upstream fora do ar — e o prazo de
	// consentimento passa.
	rel.agora = rel.agora.Add(time.Minute + time.Second)

	if b.ConsentimentoPedido(cfg.ID) {
		t.Error("ConsentimentoPedido = true depois do prazo, quer false (o pedido vencido não conta mais)")
	}
	if !b.PrecisaConsentimento(cfg.ID) {
		t.Error("PrecisaConsentimento = false depois do prazo, quer true (volta a pedir o clique)")
	}
	if err := b.Preparar(context.Background(), cfg); !errors.Is(err, ErrSemConsentimento) {
		t.Errorf("Preparar depois do prazo: erro = %v, quer %v (a tentativa vencida não é mais um sinal verde)",
			err, ErrSemConsentimento)
	}
}

// TestConsentimentoPedido_PrazoLimpaOState prova a segunda metade do prazo
// vencido: o registro de state de uso único também é limpo, e não só o
// pendente. Sem isto, um callback atrasado — o provedor demora a redirecionar
// o navegador — ainda encontraria o state e trocaria o code por um token que
// ninguém mais espera.
func TestConsentimentoPedido_PrazoLimpaOState(t *testing.T) {
	t.Parallel()

	rel := &relogioAjustavel{agora: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	b := NovoBrokerOAuth(&cofreEspiao{}, "https://patchbay.exemplo", slog.New(slog.DiscardHandler),
		ComRelogioOAuth(rel), ComTempoDeConsentimento(time.Minute))
	cfg := Config{ID: 1, Nome: "fora-do-ar", Tipo: TipoHTTP, URL: "https://exemplo.invalido/mcp"}
	s := b.sessao(cfg)

	const state = "state-do-pedido-vencido"
	p := &pedidoConsentimento{
		upstreamID: cfg.ID,
		criadoEm:   rel.Agora(),
		state:      state,
		urlPronta:  make(chan string, 1),
		resposta:   make(chan respostaConsentimento, 1),
		cancelado:  make(chan struct{}),
	}
	s.mu.Lock()
	s.pendente = p
	s.mu.Unlock()
	b.mu.Lock()
	b.porState[state] = p
	b.mu.Unlock()

	rel.agora = rel.agora.Add(time.Minute + time.Second)

	// A consulta que descobre o vencimento (por ConsentimentoPedido, que a
	// supervisão chama a cada volta) é o que dispara a limpeza.
	if b.ConsentimentoPedido(cfg.ID) {
		t.Fatal("ConsentimentoPedido = true depois do prazo, quer false")
	}

	if _, err := b.Entregar(state, "code-atrasado", "", ""); !errors.Is(err, ErrConsentimentoDesconhecido) {
		t.Errorf("Entregar com state vencido: erro = %v, quer %v", err, ErrConsentimentoDesconhecido)
	}
}
