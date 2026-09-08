package upstream_test

import (
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestBackoff_Intervalo cobre a curva sem jitter: é ela que garante que a
// frequência de tentativa cai quando o upstream não volta, e que ela para de
// cair no teto.
func TestBackoff_Intervalo(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		backoff upstream.Backoff
		falhas  int
		quer    time.Duration
	}{
		"sem falha não espera": {
			backoff: upstream.Backoff{Base: time.Second, Teto: time.Minute, Fator: 2},
			falhas:  0,
			quer:    0,
		},
		"primeira falha espera a base": {
			backoff: upstream.Backoff{Base: time.Second, Teto: time.Minute, Fator: 2},
			falhas:  1,
			quer:    time.Second,
		},
		"terceira falha dobra duas vezes": {
			backoff: upstream.Backoff{Base: time.Second, Teto: time.Minute, Fator: 2},
			falhas:  3,
			quer:    4 * time.Second,
		},
		"o teto para o crescimento": {
			backoff: upstream.Backoff{Base: time.Second, Teto: 5 * time.Second, Fator: 2},
			falhas:  10,
			quer:    5 * time.Second,
		},
		"falha absurda continua no teto": {
			backoff: upstream.Backoff{Base: time.Second, Teto: 5 * time.Minute, Fator: 2},
			falhas:  1000,
			quer:    5 * time.Minute,
		},
		"fator um não cresce": {
			backoff: upstream.BackoffFixo(20 * time.Millisecond),
			falhas:  9,
			quer:    20 * time.Millisecond,
		},
		"backoff zerado cai nos padrões": {
			backoff: upstream.Backoff{},
			falhas:  1,
			quer:    upstream.BackoffBasePadrao,
		},
		"teto menor que a base vira a base": {
			backoff: upstream.Backoff{Base: time.Second, Teto: time.Millisecond, Fator: 2},
			falhas:  4,
			quer:    time.Second,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := tc.backoff.Intervalo(tc.falhas); got != tc.quer {
				t.Fatalf("Intervalo(%d) = %v, quer %v", tc.falhas, got, tc.quer)
			}
		})
	}
}

// TestBackoff_EsperaComJitter é determinístico porque o sorteio é injetado: com
// a fonte fixa, a espera é uma conta, não uma faixa.
func TestBackoff_EsperaComJitter(t *testing.T) {
	t.Parallel()

	base := upstream.Backoff{Base: time.Second, Teto: time.Minute, Fator: 2, Jitter: 0.5}

	casos := map[string]struct {
		sorteio func() float64
		falhas  int
		quer    time.Duration
	}{
		"sorteio no piso devolve metade do intervalo": {
			sorteio: func() float64 { return 0 },
			falhas:  1,
			quer:    500 * time.Millisecond,
		},
		"sorteio no meio devolve três quartos": {
			sorteio: func() float64 { return 0.5 },
			falhas:  1,
			quer:    750 * time.Millisecond,
		},
		"sorteio no topo devolve o intervalo cheio": {
			sorteio: func() float64 { return 1 },
			falhas:  1,
			quer:    time.Second,
		},
		"o jitter acompanha o crescimento": {
			sorteio: func() float64 { return 0 },
			falhas:  4,
			quer:    4 * time.Second,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			b := base
			b.Sorteio = tc.sorteio
			if got := b.Espera(tc.falhas); got != tc.quer {
				t.Fatalf("Espera(%d) = %v, quer %v", tc.falhas, got, tc.quer)
			}
		})
	}
}

// TestBackoff_EsperaNuncaCaiAbaixoDoPiso é a razão de o jitter ser parcial: com
// jitter total, a décima falha ainda poderia sortear milissegundos e a promessa
// do backoff — a frequência cai — deixaria de valer.
func TestBackoff_EsperaNuncaCaiAbaixoDoPiso(t *testing.T) {
	t.Parallel()

	sorteios := []float64{0, 0.01, 0.25, 0.5, 0.75, 0.99}
	b := upstream.Backoff{Base: time.Second, Teto: time.Minute, Fator: 2, Jitter: 0.5}

	anterior := time.Duration(0)
	for falhas := 1; falhas <= 6; falhas++ {
		cheio := b.Intervalo(falhas)
		piso := cheio / 2

		for _, s := range sorteios {
			b.Sorteio = func() float64 { return s }
			got := b.Espera(falhas)
			if got < piso || got > cheio {
				t.Fatalf("Espera(%d) com sorteio %v = %v, quer entre %v e %v",
					falhas, s, got, piso, cheio)
			}
		}
		if piso < anterior {
			t.Fatalf("piso da falha %d = %v, quer não menor que o anterior %v", falhas, piso, anterior)
		}
		anterior = piso
	}
}
