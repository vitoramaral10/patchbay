package upstream

import (
	"math/rand/v2"
	"time"
)

// Padrões do backoff de reconexão de upstream.
//
// Base de um segundo porque a falha mais comum é transitória e esperar mais que
// isso na primeira tentativa é castigo sem motivo; teto de cinco minutos porque
// acima disso o upstream que voltou fica fora do ar por tempo que o admin nota
// antes do gateway.
const (
	// BackoffBasePadrao é a espera depois da primeira falha.
	BackoffBasePadrao = time.Second
	// BackoffTetoPadrao é o maior intervalo entre tentativas.
	BackoffTetoPadrao = 5 * time.Minute
	// BackoffFatorPadrao é quanto o intervalo cresce a cada falha consecutiva.
	BackoffFatorPadrao = 2.0
	// BackoffJitterPadrao é a fração do intervalo que é sorteada.
	BackoffJitterPadrao = 0.5
)

// TetoAbandonosPadrao é quantos connects abandonados um upstream acumula antes
// de a supervisão se desligar sozinha.
//
// Cada abandono é uma goroutine e um socket que ninguém recolhe enquanto a
// issue #1189 do go-sdk não for corrigida (seção 08.3). Sem teto, um upstream
// que pendura sempre acumula um resíduo por ciclo de backoff até o processo
// morrer de exaustão — que é trocar o modo de falha do MetaMCP por outro.
const TetoAbandonosPadrao = 5

// Relogio é o mínimo do tempo que a supervisão consome.
//
// Declarado aqui, no consumidor, e injetado: sem ele o teste de backoff teria
// que esperar pelo relógio de verdade, e um teste que espera é um teste que
// mede a máquina em vez do código.
type Relogio interface {
	// Agora é o instante atual, usado para calcular a próxima tentativa que a
	// UI mostra.
	Agora() time.Time
	// Depois devolve um canal que recebe quando d tiver passado.
	Depois(d time.Duration) <-chan time.Time
}

// relogioReal é o relógio do processo.
type relogioReal struct{}

func (relogioReal) Agora() time.Time { return time.Now() }

func (relogioReal) Depois(d time.Duration) <-chan time.Time { return time.After(d) }

// Backoff é o estado de espera entre tentativas de conexão de um upstream.
//
// Escrito à mão e não cenkalti/backoff (alternativa rejeitada, seção 09): o que
// o produto precisa é o oposto de um driver de laço — um intervalo por upstream,
// inspecionável ("próxima tentativa em 8s") e visível na tela.
type Backoff struct {
	// Base é a espera depois da primeira falha.
	Base time.Duration
	// Teto limita o crescimento. Intervalo sem teto vira upstream que nunca
	// mais volta sozinho.
	Teto time.Duration
	// Fator multiplica o intervalo a cada falha consecutiva.
	Fator float64
	// Jitter é a fração do intervalo que é sorteada, de 0 a 1.
	Jitter float64
	// Sorteio devolve um valor em [0,1). Nulo usa o gerador do processo; o
	// teste injeta o seu para a espera ficar determinística. Precisa ser
	// seguro para uso concorrente: cada upstream supervisionado chama Espera
	// da sua própria goroutine, e todas compartilham o mesmo Backoff.
	Sorteio func() float64
}

// BackoffPadrao é o backoff de reconexão de um upstream em produção.
func BackoffPadrao() Backoff {
	return Backoff{
		Base:   BackoffBasePadrao,
		Teto:   BackoffTetoPadrao,
		Fator:  BackoffFatorPadrao,
		Jitter: BackoffJitterPadrao,
	}
}

// BackoffFixo é o backoff que espera sempre o mesmo tanto. Existe para teste e
// para quem quiser desligar o crescimento sem desligar a reconexão.
func BackoffFixo(d time.Duration) Backoff {
	return Backoff{Base: d, Teto: d, Fator: 1, Jitter: 0}
}

// Espera devolve quanto esperar depois de falhas consecutivas.
//
// O jitter é parcial e não total (a espera nunca cai abaixo de 1-Jitter do
// intervalo cheio): com jitter total, a décima falha ainda poderia sortear uma
// espera de milissegundos, e a única coisa que o teto garante deixaria de valer
// — que a frequência de tentativa cai quando o upstream não volta.
func (b Backoff) Espera(falhas int) time.Duration {
	cheio := b.Intervalo(falhas)
	if cheio <= 0 {
		return 0
	}
	jitter := b.Jitter
	switch {
	case jitter <= 0:
		return cheio
	case jitter > 1:
		jitter = 1
	}

	sorteio := b.Sorteio
	if sorteio == nil {
		sorteio = sorteioPadrao
	}
	fixa := float64(cheio) * (1 - jitter)
	return time.Duration(fixa + float64(cheio)*jitter*sorteio())
}

// Intervalo é a espera cheia, sem jitter, depois de falhas consecutivas. É o
// número que descreve a curva; Espera é o que a supervisão de fato dorme.
func (b Backoff) Intervalo(falhas int) time.Duration {
	if falhas <= 0 {
		return 0
	}
	base := b.Base
	if base <= 0 {
		base = BackoffBasePadrao
	}
	teto := b.Teto
	if teto <= 0 || teto < base {
		teto = base
	}
	fator := b.Fator
	if fator < 1 {
		fator = 1
	}

	// Sem crescimento, o laço abaixo giraria falhas-1 vezes só para chegar ao
	// mesmo lugar: a espera é sempre a base, ou o teto se ele for menor.
	if fator == 1 {
		if base >= teto {
			return teto
		}
		return base
	}

	d := float64(base)
	limite := float64(teto)
	for range falhas - 1 {
		d *= fator
		if d >= limite {
			return teto
		}
	}
	if d >= limite {
		return teto
	}
	return time.Duration(d)
}

// sorteioPadrao é a fonte de jitter do processo.
//
// math/rand e não crypto/rand de propósito: o jitter existe para descorrelacionar
// tentativas de upstreams que caíram juntos, não para esconder nada. Ler
// entropia do sistema a cada reconexão seria custo sem ganho nenhum.
func sorteioPadrao() float64 {
	return rand.Float64() //nolint:gosec // jitter de backoff não é material criptográfico
}
