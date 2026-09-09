package trilha

import (
	"sync"
	"sync/atomic"
	"time"
)

// CapacidadeAssinante é quantas mensagens um assinante de SSE pode ficar
// devendo antes de o hub começar a descartar as dele.
//
// Por assinante, e não global: um navegador numa aba de fundo, ou uma conexão
// que o proxy congelou, não pode segurar a fila de quem está olhando a tela.
const CapacidadeAssinante = 128

// TipoMensagem diz o que uma mensagem do hub carrega.
type TipoMensagem string

// Tipos de mensagem. O valor vira o campo `event:` do SSE, que é o que a
// extensão sse do htmx casa com o atributo sse-swap.
const (
	// TipoLog é uma linha de log estruturado, já redigida.
	TipoLog TipoMensagem = "log"
	// TipoChamada é um evento da trilha.
	TipoChamada TipoMensagem = "chamada"
)

// Mensagem é o que trafega do hub para cada assinante.
//
// Um struct com os dois campos e não uma interface: são dois tipos e só dois, o
// consumidor liga um switch sobre Tipo, e a cópia por valor evita que dois
// assinantes compartilhem o mesmo ponteiro.
type Mensagem struct {
	Tipo    TipoMensagem
	Log     LinhaLog
	Chamada Evento
}

// Hub faz o fan-out de eventos e de linhas de log para as telas abertas.
//
// Publicar nunca bloqueia: cada assinante tem o próprio canal com buffer e a
// mensagem que não cabe é descartada só para ele. É a mesma escolha da fila de
// gravação — o produtor é o caminho da requisição (ou uma goroutine de fundo
// que não pode parar), e um consumidor lento não pode virar contrapressão.
type Hub struct {
	capacidade int

	mu         sync.Mutex
	proximo    int64
	assinantes map[int64]chan Mensagem
	encerrado  bool

	perdidas atomic.Uint64
}

// NovoHub monta o hub com a capacidade padrão por assinante.
func NovoHub() *Hub {
	return &Hub{capacidade: CapacidadeAssinante, assinantes: make(map[int64]chan Mensagem)}
}

// Assinar registra uma tela e devolve o canal dela e a função que a desliga.
//
// A função de cancelamento é idempotente e é ela — não o fechamento do canal
// pelo produtor — que encerra a assinatura: quem escreve no canal é o hub, e
// quem escreve é quem fecha.
func (h *Hub) Assinar() (<-chan Mensagem, func()) {
	ch := make(chan Mensagem, h.capacidade)

	h.mu.Lock()
	if h.encerrado {
		h.mu.Unlock()
		// Hub já encerrado: devolve um canal fechado, para o handler de SSE
		// terminar pelo mesmo caminho de sempre em vez de precisar de um segundo
		// jeito de descobrir que é hora de sair.
		close(ch)
		return ch, func() {}
	}
	h.proximo++
	id := h.proximo
	h.assinantes[id] = ch
	h.mu.Unlock()

	var uma sync.Once
	return ch, func() {
		uma.Do(func() {
			h.mu.Lock()
			_, ainda := h.assinantes[id]
			delete(h.assinantes, id)
			h.mu.Unlock()
			// Se Encerrar já fechou este canal, fechar de novo entraria em
			// panic: quem escreve é quem fecha, e ali quem fechou foi o hub.
			if ainda {
				close(ch)
			}
		})
	}
}

// Encerrar fecha todas as assinaturas e recusa as próximas.
//
// É o que solta os handlers de SSE no desligamento. Sem isso o
// http.Server.Shutdown esperaria pelo prazo inteiro por conexões que, por
// desenho, só terminam quando o cliente desiste.
func (h *Hub) Encerrar() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.encerrado {
		return
	}
	h.encerrado = true
	for id, ch := range h.assinantes {
		delete(h.assinantes, id)
		close(ch)
	}
}

// Publicar entrega m a todos os assinantes, sem bloquear em nenhum.
func (h *Hub) Publicar(m Mensagem) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, ch := range h.assinantes {
		select {
		case ch <- m:
		default:
			h.perdidas.Add(1)
		}
	}
}

// Assinantes é quantas telas estão conectadas agora.
func (h *Hub) Assinantes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.assinantes)
}

// Perdidas é quantas mensagens não couberam na fila de algum assinante desde o
// boot. É o descarte do log ao vivo, irmão do descarte da trilha: os dois
// aparecem na tela pelo mesmo motivo.
func (h *Hub) Perdidas() uint64 { return h.perdidas.Load() }

// LinhaLog é uma linha de log estruturado pronta para a tela.
//
// Atributos é uma fatia ordenada e não um mapa: a ordem em que o slog recebeu
// os pares é informação, e um mapa a jogaria fora a cada render.
type LinhaLog struct {
	Instante  time.Time
	Nivel     string
	Mensagem  string
	Atributos []Atributo
}

// Atributo é um par chave/valor de uma linha de log, já redigido.
type Atributo struct {
	Chave string
	Valor string
}
