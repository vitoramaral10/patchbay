package upstream

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A sonda dentro da máquina de estados.
//
// Tudo neste arquivo roda na goroutine de supervisão do upstream, sobre a sessão
// que já está aberta. Nada dele toca o caminho da requisição do cliente (decisão
// 4 do estudo), e nada dele derruba a sessão: uma sondagem que falha muda o
// estado e o catálogo, nunca o transporte. É essa separação que faz sonda_falhou
// significar "falo com ele, ele lista ferramentas, e a chamada de verdade não
// funciona" — o que degradado não consegue dizer.

// situacaoDaSonda monta o retrato da sonda. Chamado com g.mu segurado.
func (s *servidor) situacaoDaSonda() SituacaoSonda {
	return SituacaoSonda{
		Config:   s.cfg.Sonda,
		Em:       s.sondaEm,
		OKEm:     s.sondaOKEm,
		Falhas:   s.sondaFalhas,
		Erro:     s.sondaErro,
		Pedido:   s.sondaPedido,
		Resposta: s.sondaResposta,
	}
}

// relogioDeSonda é o tique da sondagem periódica. Nulo quando a sonda está
// desligada, e um canal nulo num select bloqueia para sempre.
func (g *Gerente) relogioDeSonda(id int64) <-chan time.Time {
	s, ok := g.sondaDe(id)
	if !ok || !s.Ativa() {
		return nil
	}
	return g.relogio.Depois(s.Normalizada().Intervalo)
}

// pedidosDeSonda é o canal em que a supervisão recebe o "Sondar agora" da tela.
//
// Nulo quando a sonda está desligada: um clique não pode ligar a sonda por
// tabela, e sem o canal a tela responde na hora que ela está desligada em vez de
// esperar por uma sondagem que não vai acontecer.
func (g *Gerente) pedidosDeSonda(id int64) <-chan pedidoSonda {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.servidores[id]
	if !ok || !s.cfg.Sonda.Ativa() {
		return nil
	}
	return s.sondas
}

// sondaDe lê a configuração de sonda em vigor.
func (g *Gerente) sondaDe(id int64) (Sonda, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.servidores[id]
	if !ok {
		return Sonda{}, false
	}
	return s.cfg.Sonda, true
}

// sondarSeLigada executa uma sondagem se a sonda estiver ligada. É o que o tique
// e a conexão nova chamam.
func (g *Gerente) sondarSeLigada(ctx context.Context, id int64, sessao *mcp.ClientSession) {
	if s, ok := g.sondaDe(id); !ok || !s.Ativa() {
		return
	}
	g.sondar(ctx, id, sessao)
}

// sondar executa um tools/call de verdade e aplica o desfecho à máquina de
// estados.
func (g *Gerente) sondar(ctx context.Context, id int64, sessao *mcp.ClientSession) ResultadoSonda {
	cfg, ok := g.config(id)
	if !ok {
		return ResultadoSonda{Erro: fmt.Sprintf("%v: id %d", ErrDesconhecido, id)}
	}
	r := g.executarSonda(ctx, cfg, sessao)
	if ctx.Err() != nil {
		// A supervisão está sendo desligada: o que a sondagem viu foi o
		// cancelamento, e gravá-lo como falha marcaria sonda_falhou sobre um
		// upstream que estava saudável até o processo pedir para sair.
		return r
	}
	g.aplicarResultadoSonda(ctx, cfg, r)
	g.observarSonda(cfg, r)
	return r
}

// executarSonda faz a chamada e classifica o resultado, sem tocar em estado
// nenhum.
//
// O prazo é próprio e derivado do contexto da supervisão: ele cancela só esta
// chamada. Cancelar a sessão aqui trocaria "esta ferramenta não responde" por
// "perdi o servidor", que é justamente a confusão que a sonda existe para
// desfazer.
func (g *Gerente) executarSonda(ctx context.Context, cfg Config, sessao *mcp.ClientSession) ResultadoSonda {
	s := cfg.Sonda.Normalizada()
	inicio := g.relogio.Agora()

	ctxSonda, cancelar := context.WithTimeout(ctx, s.Timeout)
	defer cancelar()

	res, err := sessao.CallTool(ctxSonda, &mcp.CallToolParams{
		Name: s.Ferramenta, Arguments: s.Args,
	})
	texto := textoDoResultado(res)
	motivo, estourou := avaliarSonda(s, res, err, texto)

	return ResultadoSonda{
		Em:      inicio,
		Duracao: g.relogio.Agora().Sub(inicio),
		OK:      motivo == "",
		Timeout: estourou,
		Erro:    motivo,
		// Truncado como Resposta: com o teto de argumentos em 4 KiB
		// (limiteArgsDeSonda), o pedido também pode passar de
		// limiteEvidenciaSonda antes deste corte existir.
		Pedido:       truncarEvidencia(pedidoDaSonda(s), limiteEvidenciaSonda),
		Resposta:     truncarEvidencia(texto, limiteEvidenciaSonda),
		BytesEntrada: len(s.Args),
		BytesSaida:   len(texto),
	}
}

// aplicarResultadoSonda move a máquina de estados conforme o desfecho.
//
// As duas transições da fatia, e só elas: pronto → sonda_falhou quando a
// tolerância estoura, e sonda_falhou → pronto na primeira sondagem que passa.
// Nada aqui mexe em falhas de conexão nem em abandonos — sondagem que falha não
// é connect abandonado, e contá-la como um desabilitaria por autoproteção um
// upstream que está respondendo perfeitamente.
func (g *Gerente) aplicarResultadoSonda(ctx context.Context, cfg Config, r ResultadoSonda) {
	tolerancia := cfg.Sonda.Normalizada().Tolerancia

	g.mu.Lock()
	s, ok := g.servidores[cfg.ID]
	if !ok {
		g.mu.Unlock()
		return
	}
	s.sondaEm = r.Em
	s.sondaPedido = r.Pedido
	// Redigida, e não crua: Resposta é dado de terceiro, e um servidor pode
	// devolver de volta o header de autorização que ele mesmo recusou. Ver o
	// comentário de limiteEvidenciaSonda, em sonda.go.
	s.sondaResposta = g.redator(r.Resposta)

	var transicao Estado
	if r.OK {
		s.sondaOKEm = r.Em
		s.sondaFalhas = 0
		s.sondaErro = ""
		if s.estado == EstadoSondaFalhou {
			s.estado = EstadoPronto
			s.ultimoErro = ""
			transicao = EstadoPronto
		}
	} else {
		s.sondaFalhas++
		// Mesma redação de Resposta: o motivo de falha às vezes é a resposta
		// crua ecoada de volta (avaliarSonda usa err.Error() ou o texto), e
		// sem isto o mesmo segredo que Resposta esconde reapareceria aqui.
		s.sondaErro = g.redator(r.Erro)
		if s.estado == EstadoPronto && s.sondaFalhas >= tolerancia {
			s.estado = EstadoSondaFalhou
			s.ultimoErro = r.Erro
			transicao = EstadoSondaFalhou
		}
	}
	falhas, ferramentas := s.sondaFalhas, len(s.ferramentas)
	g.mu.Unlock()

	switch transicao {
	case EstadoSondaFalhou:
		g.log.Warn("upstream em sonda_falhou",
			"upstream", cfg.Nome, "upstream_id", cfg.ID,
			"ferramenta", cfg.Sonda.Ferramenta, "erro", r.Erro,
			"sondagens_falhas", falhas, "tolerancia", tolerancia,
			"ferramentas_fora_do_catalogo", ferramentas)
	case EstadoPronto:
		g.log.Info("sonda de upstream voltou a passar",
			"upstream", cfg.Nome, "upstream_id", cfg.ID,
			"ferramenta", cfg.Sonda.Ferramenta, "ferramentas", ferramentas)
	default:
		// Sem transição: o catálogo não mudou, e notificar aqui faria cada
		// sondagem rematerializar todos os endpoints por nada.
		return
	}
	// Só quando o estado mudou: é esta notificação que tira as ferramentas dos
	// endpoints (com lápide) e as traz de volta.
	g.notificarMudanca(ctx)
}

// observarSonda entrega a sondagem a quem observa, sem deixar um observador de
// terceiro derrubar a supervisão.
//
// Mesmo recover de endpoint.observar, e pelo mesmo motivo: ObservarSonda é uma
// interface de um método implementada fora deste pacote, e um panic nela não
// pode matar a goroutine que mantém a sessão do upstream de pé.
func (g *Gerente) observarSonda(cfg Config, r ResultadoSonda) {
	if g.obsSonda == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			g.log.Error("observador da sonda entrou em panic",
				"upstream", cfg.Nome, "panic", p)
		}
	}()
	g.obsSonda.ObservarSonda(Sondagem{
		UpstreamID:   cfg.ID,
		UpstreamNome: cfg.Nome,
		Ferramenta:   cfg.Sonda.Ferramenta,
		Inicio:       r.Em,
		Duracao:      r.Duracao,
		OK:           r.OK,
		Timeout:      r.Timeout,
		Erro:         r.Erro,
		BytesEntrada: r.BytesEntrada,
		BytesSaida:   r.BytesSaida,
	})
}

// Sondar executa uma sondagem agora, a pedido da tela, e devolve o desfecho.
//
// Ela não roda aqui: o pedido atravessa um canal e é executado pela goroutine de
// supervisão, sobre a sessão viva. Assim continua existindo um único escritor do
// estado da sonda, e um clique não passa a abrir chamada de upstream a partir da
// goroutine que atende a requisição HTTP.
//
// O ctx de quem chamou é o limite da espera: um clique que virou timeout de
// requisição solta a tela, e a sondagem em curso termina sozinha do outro lado.
func (g *Gerente) Sondar(ctx context.Context, id int64) (ResultadoSonda, error) {
	g.mu.RLock()
	s, ok := g.servidores[id]
	var (
		pedidos   chan pedidoSonda
		estado    Estado
		nome      string
		ativa     bool
		temSessao bool
	)
	if ok {
		pedidos, estado, nome = s.sondas, s.estado, s.cfg.Nome
		ativa, temSessao = s.cfg.Sonda.Ativa(), s.sessao != nil
	}
	g.mu.RUnlock()

	switch {
	case !ok:
		return ResultadoSonda{}, fmt.Errorf("%w: id %d", ErrDesconhecido, id)
	case !ativa:
		return ResultadoSonda{}, fmt.Errorf("%w: %s", ErrSondaDesligada, nome)
	case !temSessao || (estado != EstadoPronto && estado != EstadoSondaFalhou):
		// Sem sessão aberta não há o que sondar, e esperar pelo canal aqui
		// prenderia a requisição até o backoff reconectar.
		return ResultadoSonda{}, fmt.Errorf("%w: %s", ErrIndisponivel, nome)
	}

	p := pedidoSonda{pronto: make(chan ResultadoSonda, 1)}
	select {
	case pedidos <- p:
	case <-g.encerrado:
		return ResultadoSonda{}, ErrGerenteParado
	case <-ctx.Done():
		return ResultadoSonda{}, fmt.Errorf("upstream %s: pedir sondagem: %w", nome, ctx.Err())
	}
	select {
	case r := <-p.pronto:
		return r, nil
	case <-g.encerrado:
		return ResultadoSonda{}, ErrGerenteParado
	case <-ctx.Done():
		return ResultadoSonda{}, fmt.Errorf("upstream %s: aguardar sondagem: %w", nome, ctx.Err())
	}
}
