// Este arquivo prova, por dentro (package upstream), que a ordem de locks entre
// sessaoOAuth.mu e FonteToken.mu não fecha um ciclo — a correção da revisão
// para o ABBA entre Preparar e FonteToken.Token().
package upstream

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// baseBloqueante é a oauth2.TokenSource de baixo de uma FonteToken de teste:
// avisa quando entra em Token() (prova que f.mu está preso) e só volta quando o
// teste manda, com o erro que o teste escolher.
type baseBloqueante struct {
	entrou  chan struct{}
	liberar chan struct{}
	erro    error
}

func (b *baseBloqueante) Token() (*oauth2.Token, error) {
	close(b.entrou)
	<-b.liberar
	return nil, b.erro
}

// TestPreparar_SemDeadlockComRefreshConcorrente reproduz as duas pontas do ABBA
// entre sessaoOAuth.mu (s.mu) e FonteToken.mu (f.mu):
//
//   - Goroutine A está dentro de FonteToken.Token(), presa em f.base.Token() com
//     f.mu preso. Quando o base devolve invalid_grant, ela chama aoRevogar, que
//     entra em sessaoOAuth.marcarPrecisa e pede s.mu.
//   - Goroutine B está dentro de BrokerOAuth.Preparar, e antes da correção
//     segurava s.mu durante toda a checagem, inclusive ao chamar fonte.Morreu()
//     — que pede f.mu.
//
// Sem soltar s.mu antes de consultar Morreu() (e sem chamar aoRevogar fora de
// f.mu), A espera s.mu com f.mu preso e B espera f.mu com s.mu preso: as duas
// travam para sempre. O teste teria que ser cortado pelo timeout do select —
// prová-lo sem travar é a prova de que a correção resolveu o ciclo.
func TestPreparar_SemDeadlockComRefreshConcorrente(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	cofre := &cofreEspiao{}
	b := NovoBrokerOAuth(cofre, "https://patchbay.exemplo", log)
	cfg := Config{ID: 1, Nome: "trava", Tipo: TipoHTTP, URL: "https://exemplo.invalido/mcp"}
	s := b.sessao(cfg)

	base := &baseBloqueante{
		entrou:  make(chan struct{}),
		liberar: make(chan struct{}),
		erro:    &oauth2.RetrieveError{ErrorCode: "invalid_grant"},
	}
	f := novaFonteToken(cfg.ID, cfg.Nome, base, Concessao{}, cofre, relogioReal{}, log,
		func() { s.marcarPrecisa() })
	s.mu.Lock()
	s.fonte = f
	s.mu.Unlock()

	// Aquece o handler fora da corrida: Preparar chama Autorizacao antes de
	// tocar em s.fonte, e ela mesma pega s.mu para montar o handler na primeira
	// vez. Sem pré-aquecer, o tempo dessa montagem seria mais uma fonte de
	// variação na hora de B chegar em Morreu(), e o que o teste quer controlar
	// é só a corrida entre s.mu e f.mu.
	if _, err := b.Autorizacao(context.Background(), cfg); err != nil {
		t.Fatalf("Autorizacao: erro = %v, quer nil", err)
	}

	terminouToken := make(chan struct{})
	go func() {
		defer close(terminouToken)
		_, _ = f.Token()
	}()

	// A entrou em Token() e está com f.mu preso.
	<-base.entrou

	terminouPreparar := make(chan error, 1)
	go func() {
		terminouPreparar <- b.Preparar(context.Background(), cfg)
	}()

	// Cede a CPU algumas vezes antes de soltar A: com o handler já pronto, o
	// caminho de B até a chamada de Morreu() é só um punhado de operações de
	// mutex, e A não está rodando nada enquanto espera o sinal — é o que dá a B
	// a chance de chegar lá e travar em f.mu antes de A ser liberado. Não é
	// prova formal de interleaving (só um canal fechado por Morreu() seria), mas
	// é determinístico o bastante para não usar o relógio.
	for range 200 {
		runtime.Gosched()
	}
	close(base.liberar)

	const limite = 5 * time.Second
	select {
	case <-terminouToken:
	case <-time.After(limite):
		t.Fatal("FonteToken.Token() não voltou — deadlock entre f.mu e s.mu")
	}
	select {
	case <-terminouPreparar:
	case <-time.After(limite):
		t.Fatal("Preparar() não voltou — deadlock entre f.mu e s.mu")
	}

	if !f.Morreu() {
		t.Error("Morreu() = false, quer true depois do invalid_grant")
	}
}

// TestLimparPendente_StateSemCorrida prova, sob -race, que a leitura de
// pedidoConsentimento.state em limparPendente e a escrita em buscarCodigo estão
// sob o mesmo lock.
//
// As duas goroutines existem de verdade no fluxo: buscarCodigo roda na
// supervisão e escreve p.state assim que o SDK monta a URL; limparPendente roda
// tanto ali (no defer) quanto na goroutine da requisição HTTP que chamou Pedir
// e desistiu pelo ctx.Done() — e é essa segunda chamada, concorrente com a
// escrita, que corria sem o lock.
func TestLimparPendente_StateSemCorrida(t *testing.T) {
	t.Parallel()

	b := NovoBrokerOAuth(&cofreEspiao{}, "https://patchbay.exemplo", slog.New(slog.DiscardHandler))
	cfg := Config{ID: 1, Nome: "corrida"}
	s := b.sessao(cfg)

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

	var prontas sync.WaitGroup
	prontas.Add(2)
	largada := make(chan struct{})
	var depois sync.WaitGroup
	depois.Add(2)

	// Escritor: o que buscarCodigo faz ao montar o state.
	go func() {
		defer depois.Done()
		prontas.Done()
		<-largada
		s.mu.Lock()
		p.state = "state-simulado"
		s.mu.Unlock()
	}()

	// Leitor: o que a goroutine de Pedir faz ao desistir.
	go func() {
		defer depois.Done()
		prontas.Done()
		<-largada
		s.limparPendente(p)
	}()

	prontas.Wait()
	close(largada)
	depois.Wait()
}
