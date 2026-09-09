package trilha

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Padrões da fila e do escritor. Todos ajustáveis por Opcao, e nenhum lido do
// ambiente: quem monta o grafo é main.
const (
	// CapacidadeFilaPadrao é quantos eventos cabem esperando gravação. Mil
	// linhas cobrem uma rajada de segundos inteiros de um gateway pessoal; acima
	// disso o certo é descartar contando, não crescer memória sem teto.
	CapacidadeFilaPadrao = 1024
	// LotePadrao é o tamanho máximo de uma transação de gravação. Lote grande
	// amortiza o fsync; lote grande *demais* segura o escritor único do SQLite
	// justo quando a UI quer escrever uma configuração.
	LotePadrao = 128
	// IntervaloLotePadrao é o teto de espera de um lote incompleto: a trilha
	// aparece na tela em até um segundo mesmo com uma chamada só.
	IntervaloLotePadrao = time.Second
	// RetencaoPadrao é a idade máxima de uma linha da trilha.
	RetencaoPadrao = 7 * 24 * time.Hour
	// IntervaloVarreduraPadrao é a frequência da varredura de retenção.
	IntervaloVarreduraPadrao = time.Hour
	// LotePodaPadrao é quantas linhas cada DELETE da varredura apaga. Pequeno de
	// propósito: a retenção não pode travar o pool de escrita.
	LotePodaPadrao = 500
	// PodasPorVarreduraPadrao limita quantos lotes uma varredura apaga antes de
	// devolver o escritor. Com o padrão, uma varredura apaga até 100 mil linhas e
	// a próxima continua de onde esta parou.
	PodasPorVarreduraPadrao = 200
)

// timeoutDescarga é o prazo da última gravação, a do desligamento. Ela roda com
// o ctx já cancelado (por WithoutCancel), e sem prazo próprio um banco travado
// prenderia o processo no shutdown.
const timeoutDescarga = 5 * time.Second

// limiteErro é o teto de Evento.Erro em bytes. Um upstream que devolve um
// corpo de erro gigante não pode inflar uma linha da trilha nem o lote inteiro
// na memória — e a mensagem que importa para diagnosticar cabe muito antes
// disso.
const limiteErro = 2 * 1024

// truncar corta s em no máximo n bytes, com reticências quando cortou.
func truncar(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Recua até um ponto de rune válido: cortar no meio de um rune multibyte
	// quebraria a codificação UTF-8 da linha.
	corte := n
	for corte > 0 && !utf8.RuneStart(s[corte]) {
		corte--
	}
	return s[:corte] + "…"
}

// Registrador é a ponta da trilha que o caminho da requisição toca.
//
// A única operação do caminho quente é Observar, que é um envio não bloqueante
// num canal com buffer. Tudo o mais — lote, gravação, retenção, fan-out — roda
// em Consumir e Varrer, que são goroutines de fundo com dono e com saída pelo
// ctx.
type Registrador struct {
	repo      Repositorio
	hub       *Hub
	log       *slog.Logger
	fila      chan Evento
	lote      int
	intervalo time.Duration

	retencao     time.Duration
	intervaloVar time.Duration
	lotePoda     int
	podasPorVar  int

	descartes atomic.Uint64
	gravados  atomic.Uint64
	// falhasGravacao é separado de descartes: descarte é fila cheia (o
	// caminho da requisição nem chegou a enfileirar); falha de gravação é
	// evento que a fila aceitou e o banco recusou gravar. Contá-los juntos
	// escondia qual dos dois problemas estava acontecendo.
	falhasGravacao atomic.Uint64
	// descartesRelatados é quanto do contador de descartes já saiu em log, para
	// que o aviso apareça uma vez por rajada e não uma vez por linha perdida.
	descartesRelatados atomic.Uint64
}

// Opcao ajusta o Registrador na construção.
type Opcao func(*Registrador)

// ComCapacidade troca o tamanho da fila. Zero ou negativo mantém o padrão.
func ComCapacidade(n int) Opcao {
	return func(r *Registrador) {
		if n > 0 {
			r.fila = make(chan Evento, n)
		}
	}
}

// ComLote troca o tamanho máximo de uma transação de gravação.
func ComLote(n int) Opcao {
	return func(r *Registrador) {
		if n > 0 {
			r.lote = n
		}
	}
}

// ComIntervaloLote troca o teto de espera de um lote incompleto.
func ComIntervaloLote(d time.Duration) Opcao {
	return func(r *Registrador) {
		if d > 0 {
			r.intervalo = d
		}
	}
}

// ComRetencao troca a idade máxima de uma linha da trilha.
func ComRetencao(d time.Duration) Opcao {
	return func(r *Registrador) {
		if d > 0 {
			r.retencao = d
		}
	}
}

// ComVarredura troca a frequência da varredura de retenção e o tamanho do lote
// que cada uma apaga.
func ComVarredura(intervalo time.Duration, lote int) Opcao {
	return func(r *Registrador) {
		if intervalo > 0 {
			r.intervaloVar = intervalo
		}
		if lote > 0 {
			r.lotePoda = lote
		}
	}
}

// ComHub liga o fan-out para as telas abertas. Sem hub, a trilha continua sendo
// gravada — o log ao vivo é que fica sem assinantes.
func ComHub(h *Hub) Opcao {
	return func(r *Registrador) { r.hub = h }
}

// NovoRegistrador monta a fila e o escritor.
func NovoRegistrador(repo Repositorio, log *slog.Logger, opcoes ...Opcao) *Registrador {
	r := &Registrador{
		repo:         repo,
		log:          log,
		fila:         make(chan Evento, CapacidadeFilaPadrao),
		lote:         LotePadrao,
		intervalo:    IntervaloLotePadrao,
		retencao:     RetencaoPadrao,
		intervaloVar: IntervaloVarreduraPadrao,
		lotePoda:     LotePodaPadrao,
		podasPorVar:  PodasPorVarreduraPadrao,
	}
	for _, o := range opcoes {
		o(r)
	}
	return r
}

// Observar enfileira um evento cru e volta na hora.
//
// Não recebe ctx e não devolve erro, de propósito: as duas coisas convidariam
// quem chama a esperar por algo. Fila cheia descarta e conta — o contador
// aparece na tela, porque trilha que mente é pior que trilha faltando.
//
// Anonimização, redação e validação do resultado saem daqui e entram em
// Consumir (ver redigirEvento): as três são trabalho de CPU — SHA-256, regex —
// e trabalho nenhum entra no caminho da requisição, nem para redigir um
// segredo antes de descartar por fila cheia.
//
// Nada é logado aqui. Um Warn por linha perdida transformaria a rajada que
// encheu a fila numa rajada de log, que é a fila de escrita do outro lado; quem
// relata o descarte é o consumidor, uma vez por lote.
func (r *Registrador) Observar(ev Evento) {
	select {
	case r.fila <- ev:
	default:
		r.descartes.Add(1)
	}
}

// redigirEvento aplica a anonimização da sessão, a redação do erro e a
// validação do resultado — o que Observar fazia antes de enfileirar, e que
// agora roda no consumidor, antes de publicar e de entrar no lote de
// gravação.
//
// O erro é truncado depois de redigido, nunca antes: truncar primeiro cortaria
// um segredo no meio e deixaria a metade dele em claro na tela e no banco.
func redigirEvento(ev Evento) Evento {
	ev.Sessao = Anonimizar(ev.Sessao)
	ev.Erro, _ = Redigir(ev.Erro)
	ev.Erro = truncar(ev.Erro, limiteErro)
	if !ev.Resultado.Valido() {
		ev.Resultado = ResultadoErro
	}
	// Origem vazia é cliente: o log ao vivo publica este evento antes de ele
	// chegar ao banco, e sem isto a marca da tela dependeria de quem preencheu
	// a struct.
	ev.Origem = ev.Origem.OuCliente()
	return ev
}

// Descartes é quantos eventos a fila recusou desde o boot. É o número que a
// seção 11 exige na tela. Só fila cheia — falha de gravação é FalhasGravacao,
// separado.
func (r *Registrador) Descartes() uint64 { return r.descartes.Load() }

// FalhasGravacao é quantos eventos saíram da fila e o banco recusou gravar,
// desde o boot. Separado de Descartes porque são problemas diferentes de
// diagnosticar: "a fila não escoa" contra "o banco está recusando".
func (r *Registrador) FalhasGravacao() uint64 { return r.falhasGravacao.Load() }

// Gravados é quantos eventos chegaram ao banco desde o boot.
func (r *Registrador) Gravados() uint64 { return r.gravados.Load() }

// Consumir drena a fila em lote até parar ser fechado, e grava o que sobrou
// antes de voltar.
//
// parar é um sinal próprio, e não o ctx do serviço: o desligamento do
// servidor HTTP espera as requisições em curso terminarem (srv.Shutdown), e só
// depois disso a última chamada de Observar já aconteceu de verdade. Se o
// consumidor parasse no cancelamento do ctx do serviço — que chega *antes* de
// Shutdown começar a esperar —, ele sairia durante essa janela, e uma chamada
// que terminasse depois cairia numa fila que ninguém mais drena: perda
// silenciosa, sem nem contar como descarte. Quem chama Consumir passa um ctx
// que não é cancelado por esse mesmo sinal (main usa context.Background()),
// exatamente para as gravações em curso não falharem por causa da mesma
// janela.
//
// Um consumidor só: o SQLite aceita um escritor por vez, e dois consumidores
// competiriam pela mesma conexão para gravar a mesma tabela.
func (r *Registrador) Consumir(ctx context.Context, parar <-chan struct{}) {
	tique := time.NewTicker(r.intervalo)
	defer tique.Stop()

	lote := make([]Evento, 0, r.lote)
	for {
		select {
		case <-parar:
			// Descarga final com prazo próprio (timeoutDescarga): o que já
			// está na fila foi aceito, e jogá-lo fora no desligamento seria
			// perder a trilha justo do que aconteceu por último.
			r.descarregarFinal(ctx, lote)
			return

		case ev := <-r.fila:
			ev = redigirEvento(ev)
			r.publicar(ev)
			lote = append(lote, ev)
			if len(lote) >= r.lote {
				lote = r.descarregar(ctx, lote)
			}

		case <-tique.C:
			lote = r.descarregar(ctx, lote)
			r.relatarDescartes(ctx)
		}
	}
}

// descarregarFinal esvazia a fila e grava tudo no desligamento.
func (r *Registrador) descarregarFinal(ctx context.Context, lote []Evento) {
	for {
		select {
		case ev := <-r.fila:
			lote = append(lote, redigirEvento(ev))
			continue
		default:
		}
		break
	}
	if len(lote) == 0 {
		return
	}
	ctxFinal, cancelar := context.WithTimeout(context.WithoutCancel(ctx), timeoutDescarga)
	defer cancelar()
	r.gravar(ctxFinal, lote)
}

// descarregar grava o lote e devolve a fatia vazia para reuso.
func (r *Registrador) descarregar(ctx context.Context, lote []Evento) []Evento {
	if len(lote) == 0 {
		return lote
	}
	r.gravar(ctx, lote)
	return lote[:0]
}

func (r *Registrador) gravar(ctx context.Context, lote []Evento) {
	if err := r.repo.Gravar(ctx, lote); err != nil {
		if ctx.Err() != nil {
			return
		}
		// A trilha não pode derrubar nada: falha de gravação é aviso, e as
		// linhas do lote se perdem. FalhasGravacao e não descartes: a fila não
		// estava cheia, foi o banco que recusou — são diagnósticos diferentes,
		// e contá-los juntos escondia qual dos dois estava acontecendo.
		r.falhasGravacao.Add(uint64(len(lote)))
		r.log.Warn("trilha não conseguiu gravar eventos", "eventos", len(lote), "erro", err)
		return
	}
	r.gravados.Add(uint64(len(lote)))
}

// publicar manda o evento para as telas abertas.
//
// Roda no consumidor e não em Observar: montar a mensagem e percorrer os
// assinantes é trabalho, e trabalho nenhum entra no caminho da requisição.
func (r *Registrador) publicar(ev Evento) {
	if r.hub == nil {
		return
	}
	r.hub.Publicar(Mensagem{Tipo: TipoChamada, Chamada: ev})
}

// relatarDescartes loga o crescimento do contador de descartes, uma vez por
// tique. Fora do caminho da requisição, e por diferença: o que interessa é
// "perdi 400 linhas desde a última vez", não o total.
func (r *Registrador) relatarDescartes(ctx context.Context) {
	total := r.descartes.Load()
	antes := r.descartesRelatados.Swap(total)
	if total <= antes || ctx.Err() != nil {
		return
	}
	r.log.Warn("trilha descartou eventos por fila cheia",
		"descartados", total-antes, "descartados_total", total, "capacidade", cap(r.fila))
}

// Varrer apaga a trilha vencida até o ctx ser cancelado.
//
// A primeira varredura roda no boot: um processo que só sobe e desce nunca
// alcançaria o tique, e o banco cresceria para sempre.
func (r *Registrador) Varrer(ctx context.Context) {
	tique := time.NewTicker(r.intervaloVar)
	defer tique.Stop()
	for {
		r.podar(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tique.C:
		}
	}
}

// podar apaga em lotes pequenos, soltando o escritor entre eles.
//
// Um DELETE só, sem LIMIT, seria uma transação de tamanho imprevisível na única
// conexão de escrita: no primeiro boot depois de meses parado, ela seguraria a
// UI e o gravador da trilha pelo tempo que levasse. Em lotes, cada volta do laço
// é uma transação curta que o database/sql intercala com as outras.
func (r *Registrador) podar(ctx context.Context) {
	limite := time.Now().Add(-r.retencao)
	var total int64
	for i := 0; i < r.podasPorVar; i++ {
		if ctx.Err() != nil {
			return
		}
		n, err := r.repo.Podar(ctx, limite, r.lotePoda)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("falha ao aplicar retenção da trilha",
					"retencao_h", int(r.retencao.Hours()), "erro", err)
			}
			return
		}
		total += n
		if n < int64(r.lotePoda) {
			break
		}
	}
	if total > 0 {
		r.log.Info("trilha vencida apagada",
			"linhas", total, "retencao_h", int(r.retencao.Hours()))
	}
}
