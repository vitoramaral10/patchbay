package trilha_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/trilha"
)

// tetoDeEspera está em hub_test.go, e vale para os testes deste arquivo também.

func semLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// repoFake é o dublê do Repositorio. Escrito à mão, com um canal por lote
// gravado: é o que permite ao teste esperar por sinal em vez de pelo relógio.
type repoFake struct {
	mu       sync.Mutex
	lotes    [][]trilha.Evento
	gravados []trilha.Evento
	// erro, quando não nil, faz toda gravação falhar.
	erro error
	// bloquear, quando não nil, segura cada Gravar até o canal ser liberado.
	bloquear chan struct{}

	gravou chan int

	// podados são as chamadas de Podar, para o teste de retenção.
	podados []poda
	// restantes é quantas linhas o fake "tem" acima do limite de retenção.
	aPodar int
}

type poda struct {
	antesDe time.Time
	limite  int
}

func novoRepoFake() *repoFake {
	return &repoFake{gravou: make(chan int, 64)}
}

func (r *repoFake) Gravar(_ context.Context, eventos []trilha.Evento) error {
	if r.bloquear != nil {
		<-r.bloquear
	}
	r.mu.Lock()
	if r.erro != nil {
		err := r.erro
		r.mu.Unlock()
		return err
	}
	r.lotes = append(r.lotes, slices.Clone(eventos))
	r.gravados = append(r.gravados, eventos...)
	r.mu.Unlock()

	select {
	case r.gravou <- len(eventos):
	default:
	}
	return nil
}

func (r *repoFake) Podar(_ context.Context, antesDe time.Time, limite int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.podados = append(r.podados, poda{antesDe: antesDe, limite: limite})
	n := min(r.aPodar, limite)
	r.aPodar -= n
	return int64(n), nil
}

func (r *repoFake) todos() []trilha.Evento {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.gravados)
}

func (r *repoFake) todasAsPodas() []poda {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.podados)
}

// falta é quanto ainda resta para a varredura apagar, lido sob o mutex: o -race
// acusaria a leitura crua de outra goroutine.
func (r *repoFake) falta() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aPodar
}

func evento(ferramenta string) trilha.Evento {
	return trilha.Evento{
		Inicio:       time.Now(),
		Duracao:      3 * time.Millisecond,
		EndpointSlug: "pessoal",
		UpstreamNome: "falso",
		Ferramenta:   ferramenta,
		Original:     ferramenta,
		Resultado:    trilha.ResultadoOK,
	}
}

// TestRegistrador_FilaCheiaDescartaSemBloquear é a prova do resíduo assumido na
// seção 08.8: com o consumidor parado, Observar continua voltando na hora e o
// excedente é contado.
//
// O consumidor nunca é iniciado de propósito — é o cenário em que a fila
// realmente enche. Se Observar bloqueasse, o teste não falharia por assertiva:
// ele travaria. Por isso as chamadas vão numa goroutine e o teto de espera
// transforma o travamento numa falha legível.
func TestRegistrador_FilaCheiaDescartaSemBloquear(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		capacidade   int
		observados   int
		querDescarte uint64
	}{
		"cabe tudo, não descarta nada":       {capacidade: 8, observados: 8, querDescarte: 0},
		"uma além da capacidade":             {capacidade: 4, observados: 5, querDescarte: 1},
		"rajada de dez vezes a capacidade":   {capacidade: 4, observados: 40, querDescarte: 36},
		"capacidade mínima, rajada de cinco": {capacidade: 1, observados: 5, querDescarte: 4},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			// Consumidor jamais iniciado: nada drena a fila.
			sut := trilha.NovoRegistrador(novoRepoFake(), semLog(),
				trilha.ComCapacidade(tc.capacidade))

			pronto := make(chan struct{})
			go func() {
				defer close(pronto)
				for i := 0; i < tc.observados; i++ {
					sut.Observar(evento("somar"))
				}
			}()

			select {
			case <-pronto:
			case <-time.After(tetoDeEspera):
				t.Fatalf("Observar bloqueou com a fila cheia (capacidade %d, %d eventos)",
					tc.capacidade, tc.observados)
			}

			if got := sut.Descartes(); got != tc.querDescarte {
				t.Errorf("Descartes() = %d, quer %d", got, tc.querDescarte)
			}
			if got := sut.Gravados(); got != 0 {
				t.Errorf("Gravados() = %d, quer 0 (nada foi consumido)", got)
			}
		})
	}
}

// TestRegistrador_DescarteNaoBloqueiaNemComGravacaoTravada prova o pior caso: o
// banco travado no meio de um Gravar. O consumidor está parado dentro da
// gravação, a fila enche, e o caminho da requisição continua sem esperar.
func TestRegistrador_DescarteNaoBloqueiaNemComGravacaoTravada(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	repo.bloquear = make(chan struct{})

	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComCapacidade(2), trilha.ComLote(1))

	parar := make(chan struct{})
	consumindo := make(chan struct{})
	go func() {
		defer close(consumindo)
		sut.Consumir(context.Background(), parar)
	}()

	pronto := make(chan struct{})
	go func() {
		defer close(pronto)
		for i := 0; i < 50; i++ {
			sut.Observar(evento("somar"))
		}
	}()

	select {
	case <-pronto:
	case <-time.After(tetoDeEspera):
		t.Fatal("Observar bloqueou enquanto a gravação estava travada")
	}
	if sut.Descartes() == 0 {
		t.Error("Descartes() = 0, quer mais de zero: a fila tinha de ter enchido")
	}

	// Solta a gravação e desliga limpo, para o -race não acusar goroutine viva.
	close(repo.bloquear)
	close(parar)
	select {
	case <-consumindo:
	case <-time.After(tetoDeEspera):
		t.Fatal("Consumir não voltou depois de parar fechar")
	}
}

// TestRegistrador_ConsomeEmLote prova que o consumidor agrupa: com lote de três,
// nove eventos viram três transações, e não nove.
func TestRegistrador_ConsomeEmLote(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComCapacidade(64), trilha.ComLote(3),
		// Intervalo longo: o que fecha o lote neste teste é o tamanho, não o
		// tique. Se fosse curto, o teste passaria por acidente de temporização.
		trilha.ComIntervaloLote(time.Hour))

	parar := make(chan struct{})
	t.Cleanup(func() { close(parar) })
	go sut.Consumir(context.Background(), parar)

	for i := 0; i < 9; i++ {
		sut.Observar(evento("somar"))
	}
	esperarGravacoes(t, repo, 9)

	repo.mu.Lock()
	lotes := slices.Clone(repo.lotes)
	repo.mu.Unlock()

	if len(lotes) != 3 {
		t.Fatalf("transações = %d, quer 3 (nove eventos em lotes de três)", len(lotes))
	}
	for i, lote := range lotes {
		if len(lote) != 3 {
			t.Errorf("lote %d tem %d eventos, quer 3", i, len(lote))
		}
	}
	if got := sut.Gravados(); got != 9 {
		t.Errorf("Gravados() = %d, quer 9", got)
	}
	if got := sut.Descartes(); got != 0 {
		t.Errorf("Descartes() = %d, quer 0", got)
	}
}

// TestRegistrador_LoteIncompletoSaiNoTique prova que uma chamada solitária não
// fica presa esperando o lote encher.
func TestRegistrador_LoteIncompletoSaiNoTique(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComLote(100), trilha.ComIntervaloLote(5*time.Millisecond))

	parar := make(chan struct{})
	t.Cleanup(func() { close(parar) })
	go sut.Consumir(context.Background(), parar)

	sut.Observar(evento("somar"))
	esperarGravacoes(t, repo, 1)
}

// TestRegistrador_DescarregaNoDesligamento prova que o que já foi aceito na fila
// é gravado antes de o processo sair. Perder a trilha justo do que aconteceu por
// último é perder a parte que mais importa.
func TestRegistrador_DescarregaNoDesligamento(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComCapacidade(64), trilha.ComLote(100), trilha.ComIntervaloLote(time.Hour))

	parar := make(chan struct{})
	voltou := make(chan struct{})
	go func() {
		defer close(voltou)
		sut.Consumir(context.Background(), parar)
	}()

	for i := 0; i < 5; i++ {
		sut.Observar(evento("somar"))
	}
	close(parar)

	select {
	case <-voltou:
	case <-time.After(tetoDeEspera):
		t.Fatal("Consumir não voltou depois de parar fechar")
	}
	if got := len(repo.todos()); got != 5 {
		t.Errorf("eventos gravados = %d, quer 5 (a descarga final não pode perder o que já foi aceito)", got)
	}
}

// TestRegistrador_ObservaAposCtxCancelado prova a correção da seção 12: o
// consumidor só para quando o sinal dele (parar) fecha, não quando o ctx do
// serviço cancela. Sem essa separação, uma chamada que termina durante a
// janela em que srv.Shutdown ainda espera as requisições em curso cairia numa
// fila que ninguém mais drena — perda silenciosa, sem nem contar como
// descarte.
func TestRegistrador_ObservaAposCtxCancelado(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComLote(1), trilha.ComIntervaloLote(5*time.Millisecond))

	// ctxServico simula o ctx do processo, cancelado pelo sinal do SO — que
	// chega antes de srv.Shutdown() sequer começar a esperar as requisições em
	// curso. Consumir nunca vê este ctx: recebe context.Background() no lugar,
	// exatamente como main faz (ver cmd/patchbay/aplicacao.go).
	_, cancelarServico := context.WithCancel(context.Background())

	parar := make(chan struct{})
	voltou := make(chan struct{})
	go func() {
		defer close(voltou)
		sut.Consumir(context.Background(), parar)
	}()

	cancelarServico()

	// Uma chamada que só termina depois do cancelamento do ctx do serviço
	// ainda tem de ser gravada: é o que estava se perdendo antes da correção.
	sut.Observar(evento("tardia"))
	esperarGravacoes(t, repo, 1)

	close(parar)
	select {
	case <-voltou:
	case <-time.After(tetoDeEspera):
		t.Fatal("Consumir não voltou depois de parar fechar")
	}

	if got := ferramentasDe(repo.todos()); !slices.Contains(got, "tardia") {
		t.Errorf("eventos gravados = %v, quer conter %q", got, "tardia")
	}
}

// TestRegistrador_FalhaDeGravacaoNaoContaComoDescarte: a trilha não derruba
// nada, e falha de gravação vira um contador separado de descarte por fila
// cheia — os dois viram problemas diferentes de log, e misturá-los escondia
// qual dos dois estava acontecendo.
func TestRegistrador_FalhaDeGravacaoNaoContaComoDescarte(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	repo.erro = errors.New("disco cheio")

	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComLote(2), trilha.ComIntervaloLote(5*time.Millisecond))

	parar := make(chan struct{})
	t.Cleanup(func() { close(parar) })
	go sut.Consumir(context.Background(), parar)

	sut.Observar(evento("somar"))
	sut.Observar(evento("somar"))

	esperar(t, func() bool { return sut.FalhasGravacao() >= 2 },
		"FalhasGravacao() não chegou a 2 com a gravação falhando")
	if got := sut.Gravados(); got != 0 {
		t.Errorf("Gravados() = %d, quer 0", got)
	}
	if got := sut.Descartes(); got != 0 {
		t.Errorf("Descartes() = %d, quer 0: a fila não estava cheia, o banco é que recusou", got)
	}
}

// TestRegistrador_ObservarAnonimizaERedige prova as duas garantias de segredo do
// caminho de captura: o id de sessão nunca é gravado cru, e a mensagem de erro
// do upstream passa pela redação antes de virar linha da trilha.
func TestRegistrador_ObservarAnonimizaERedige(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		entrada   trilha.Evento
		querErro  string
		querNaoSe []string
	}{
		"bearer na mensagem do upstream sai redigido": {
			entrada: trilha.Evento{
				Ferramenta: "somar", Resultado: trilha.ResultadoErro,
				Erro:   "upstream notion: 401 com Authorization: Bearer abcdefghijklmnop",
				Sessao: "sessao-do-cliente-1",
			},
			querErro:  "upstream notion: 401 com Authorization: Bearer «redigido»",
			querNaoSe: []string{"abcdefghijklmnop", "sessao-do-cliente-1"},
		},
		"chave de api mantém o prefixo visível": {
			entrada: trilha.Evento{
				Ferramenta: "somar", Resultado: trilha.ResultadoErro,
				Erro: "chave pbk_a1b2c3d4_QUALQUERSEGREDOAQUI recusada",
			},
			querErro:  "chave pbk_a1b2c3d4_«redigido» recusada",
			querNaoSe: []string{"QUALQUERSEGREDOAQUI"},
		},
		"resultado inválido cai em erro": {
			entrada:  trilha.Evento{Ferramenta: "somar", Resultado: trilha.Resultado("inventado")},
			querErro: "",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			repo := novoRepoFake()
			sut := trilha.NovoRegistrador(repo, semLog(),
				trilha.ComLote(1), trilha.ComIntervaloLote(5*time.Millisecond))

			parar := make(chan struct{})
			t.Cleanup(func() { close(parar) })
			go sut.Consumir(context.Background(), parar)

			sut.Observar(tc.entrada)
			esperarGravacoes(t, repo, 1)

			gravado := repo.todos()[0]
			if gravado.Erro != tc.querErro {
				t.Errorf("Erro = %q, quer %q", gravado.Erro, tc.querErro)
			}
			if !gravado.Resultado.Valido() {
				t.Errorf("Resultado = %q, quer um dos três conhecidos", gravado.Resultado)
			}
			if tc.entrada.Sessao != "" {
				if gravado.Sessao == tc.entrada.Sessao {
					t.Errorf("Sessao = %q, quer o id anonimizado", gravado.Sessao)
				}
				if quer := trilha.Anonimizar(tc.entrada.Sessao); gravado.Sessao != quer {
					t.Errorf("Sessao = %q, quer %q", gravado.Sessao, quer)
				}
			}
			for _, proibido := range tc.querNaoSe {
				if contemEmAlgumCampo(gravado, proibido) {
					t.Errorf("o evento gravado contém %q, que nunca pode chegar à trilha", proibido)
				}
			}
		})
	}
}

// contemEmAlgumCampo procura a agulha em todo campo de texto do evento. É a
// assertiva negativa que importa: não basta o campo esperado estar redigido, o
// segredo não pode ter escorrido para nenhum outro.
func contemEmAlgumCampo(e trilha.Evento, agulha string) bool {
	if agulha == "" {
		return false
	}
	for _, campo := range []string{e.Erro, e.Sessao, e.Credencial, e.Ferramenta, e.Original,
		e.EndpointSlug, e.UpstreamNome, e.Era} {
		if strings.Contains(campo, agulha) {
			return true
		}
	}
	return false
}

// TestRegistrador_VarrerPodaEmLotesPequenos prova que a retenção não pede ao
// banco uma transação de tamanho imprevisível: ela apaga em lotes do tamanho
// configurado, e para quando um lote volta incompleto.
func TestRegistrador_VarrerPodaEmLotesPequenos(t *testing.T) {
	t.Parallel()

	repo := novoRepoFake()
	repo.aPodar = 25

	sut := trilha.NovoRegistrador(repo, semLog(),
		trilha.ComRetencao(48*time.Hour),
		trilha.ComVarredura(time.Hour, 10))

	ctx, cancelar := context.WithCancel(context.Background())
	go sut.Varrer(ctx)

	esperar(t, func() bool { return repo.falta() == 0 }, "a varredura não apagou tudo o que estava vencido")
	cancelar()

	podas := repo.todasAsPodas()
	if len(podas) < 3 {
		t.Fatalf("chamadas a Podar = %d, quer ao menos 3 (25 linhas em lotes de 10)", len(podas))
	}
	for i, p := range podas {
		if p.limite != 10 {
			t.Errorf("poda %d com limite %d, quer 10: lote grande segura o escritor único", i, p.limite)
		}
		// O corte é "agora menos a retenção": qualquer coisa mais nova que isso
		// não pode entrar no DELETE.
		if idade := time.Since(p.antesDe); idade < 47*time.Hour || idade > 49*time.Hour {
			t.Errorf("poda %d corta em %v atrás, quer perto de 48 h", i, idade.Round(time.Hour))
		}
	}
}

// esperarGravacoes espera até quantos eventos terem sido gravados. Espera por
// sinal, nunca pelo relógio.
func esperarGravacoes(t *testing.T, repo *repoFake, quantos int) {
	t.Helper()

	total := 0
	limite := time.After(tetoDeEspera)
	for total < quantos {
		select {
		case n := <-repo.gravou:
			total += n
		case <-limite:
			t.Fatalf("gravados = %d, quer %d dentro de %v", total, quantos, tetoDeEspera)
		}
	}
}

// esperar sonda uma condição até ela virar verdadeira. É o recurso de último
// caso, para condição que não tem canal próprio; o teto transforma "nunca
// aconteceu" em falha legível em vez de teste travado.
func esperar(t *testing.T, condicao func() bool, mensagem string) {
	t.Helper()

	limite := time.After(tetoDeEspera)
	tique := time.NewTicker(time.Millisecond)
	defer tique.Stop()
	for {
		if condicao() {
			return
		}
		select {
		case <-tique.C:
		case <-limite:
			t.Fatal(mensagem)
		}
	}
}
