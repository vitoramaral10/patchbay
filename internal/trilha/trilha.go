// Package trilha é a observabilidade do patchbay: a trilha por chamada de
// ferramenta, o log ao vivo por SSE e a redação de segredo dos dois.
//
// A regra que organiza o pacote inteiro está na seção 08.8 do estudo prévio: a
// trilha é a escrita mais frequente do sistema e o SQLite tem um único
// escritor, então nada dela pode acontecer no caminho da latência. O que o
// caminho da requisição faz é um envio não bloqueante num canal com buffer; o
// resto — lote, gravação, retenção, fan-out para as telas — roda em goroutines
// de fundo com dono e com saída pelo ctx.
//
// O resíduo dessa escolha é assumido e visível: sob rajada a fila enche e a
// linha é descartada. O número de descartes aparece na tela, porque trilha que
// mente é pior que trilha faltando.
package trilha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Resultado classifica o desfecho de uma chamada de ferramenta.
//
// Três valores e não dois: timeout é o desfecho que o admin procura primeiro
// quando alguém diz "está lento", e afogá-lo em "erro" obriga a ler a mensagem
// de cada linha para separar o que o índice já poderia separar.
type Resultado string

// Resultados possíveis. Os mesmos textos estão no CHECK da migração 00011.
const (
	// ResultadoOK é a chamada que voltou sem erro.
	ResultadoOK Resultado = "ok"
	// ResultadoErro é a chamada que voltou com erro do upstream ou do gateway.
	ResultadoErro Resultado = "erro"
	// ResultadoTimeout é a chamada que estourou o prazo do upstream.
	ResultadoTimeout Resultado = "timeout"
)

// Valido informa se r é um dos três resultados conhecidos.
func (r Resultado) Valido() bool {
	switch r {
	case ResultadoOK, ResultadoErro, ResultadoTimeout:
		return true
	default:
		return false
	}
}

// Rotulo é como o resultado aparece na tela.
func (r Resultado) Rotulo() string {
	switch r {
	case ResultadoOK:
		return "ok"
	case ResultadoErro:
		return "erro"
	case ResultadoTimeout:
		return "timeout"
	default:
		return string(r)
	}
}

// Origem diz quem pediu a chamada.
//
// Coluna própria em vez de um valor novo em Resultado, e a diferença é a
// pergunta que cada um responde: Resultado é "o que aconteceu" (ok, erro,
// timeout) e Origem é "quem pediu". Um 'sonda_erro' dentro de Resultado faria
// uma sondagem que estourou o prazo deixar de ser encontrável pelo filtro de
// timeout, e obrigaria todo consumidor de Resultado a aprender dois
// vocabulários para a mesma pergunta. Separadas, o resumo do painel exclui a
// sonda com um predicado só e os contadores continuam significando "chamada de
// cliente de verdade".
type Origem string

// Origens possíveis. Os mesmos textos estão no CHECK da migração 00012.
const (
	// OrigemCliente é o tools/call que chegou por um endpoint. É o padrão: toda
	// linha gravada antes da fatia 9 é desta origem, porque era o único caminho
	// que existia.
	OrigemCliente Origem = "cliente"
	// OrigemSonda é a sondagem funcional da fatia 9, disparada pela supervisão
	// do upstream ou pelo botão "Sondar agora".
	OrigemSonda Origem = "sonda"
)

// Valida informa se o é uma das duas origens conhecidas.
func (o Origem) Valida() bool {
	switch o {
	case OrigemCliente, OrigemSonda:
		return true
	default:
		return false
	}
}

// OuCliente devolve a origem, tratando a vazia ou desconhecida como cliente.
//
// Existe porque "não disse de onde veio" é chamada de cliente, e não erro:
// quem preenche o evento no caminho da requisição não deveria precisar dizer o
// óbvio. Normalizar no tipo, e não só em quem grava, é o que impede um caller
// futuro de esbarrar no CHECK da migração por um campo que ele nem sabia que
// existia.
func (o Origem) OuCliente() Origem {
	if o.Valida() {
		return o
	}
	return OrigemCliente
}

// Rotulo é como a origem aparece na tela.
func (o Origem) Rotulo() string {
	switch o {
	case OrigemCliente:
		return "cliente"
	case OrigemSonda:
		return "sonda"
	default:
		return string(o)
	}
}

// Evento é uma chamada de ferramenta já respondida.
//
// Ele carrega tamanho de entrada e de saída, nunca o conteúdo: argumento e
// resultado são dado de terceiro, e guardá-los seria o caminho mais curto para
// um segredo entrar no banco em claro. O que se diagnostica com eles é
// "grande demais", e para isso o número basta.
type Evento struct {
	// ID é a linha no banco. Zero enquanto o evento ainda não foi gravado.
	ID int64
	// Inicio é o instante em que a chamada saiu para o upstream.
	Inicio time.Time
	// Duracao é o tempo até a resposta (ou até o timeout).
	Duracao time.Duration

	EndpointID   int64
	EndpointSlug string
	UpstreamID   int64
	UpstreamNome string

	// Ferramenta é o nome que o cliente chamou; Original é o nome no upstream.
	// Os dois porque prefixo e renome existem, e diagnosticar com só um dos
	// lados obriga a reconstruir a composição de cabeça.
	Ferramenta string
	Original   string

	Resultado Resultado
	// Origem separa a chamada de um cliente da sondagem funcional. Vazia é
	// cliente: Registrador.Consumir normaliza antes de gravar.
	Origem Origem
	// Erro é a mensagem, já redigida. Nunca o erro cru: a mensagem de um
	// upstream pode repetir o token que ele recusou.
	Erro string

	BytesEntrada int
	BytesSaida   int

	// Sessao é o identificador da sessão do cliente. Quem preenche pode passar
	// o valor cru: Registrador.Observar anonimiza antes de enfileirar.
	Sessao string
	// Credencial identifica quem chamou sem revelar a credencial — para chave
	// de API é "apikey:<id>".
	Credencial string
	// Era é a versão do protocolo MCP negociada naquela sessão. Diagnosticar
	// "funciona no Claude Code e não no claude.ai" começa por saber qual
	// versão cada um falou (seção 08.8).
	Era string
}

// DuracaoMS é a duração em milissegundos, que é a unidade gravada.
func (e Evento) DuracaoMS() int64 { return e.Duracao.Milliseconds() }

// Anonimizar transforma um id de sessão no rótulo que a trilha guarda.
//
// SHA-256 truncado, sem sal: a entrada é o id de sessão do SDK, um valor
// aleatório de alta entropia, e não um identificador de baixa cardinalidade
// como um e-mail — um dicionário de pré-imagem não existe para construir. Sem
// sal o rótulo continua estável entre reinícios, que é o que permite ver que
// duas rajadas separadas por um deploy vieram do mesmo cliente.
//
// Sessão vazia (transporte sem id) vira vazio, e não o hash da string vazia:
// um rótulo constante para "não sei" seria indistinguível de um cliente real.
func Anonimizar(sessao string) string {
	if sessao == "" {
		return ""
	}
	soma := sha256.Sum256([]byte(sessao))
	return hex.EncodeToString(soma[:8])
}

// Repositorio é o que o Registrador precisa da persistência. Declarada aqui, no
// consumidor, com os dois únicos métodos que a goroutine de fundo usa.
type Repositorio interface {
	// Gravar insere um lote inteiro numa transação.
	Gravar(ctx context.Context, eventos []Evento) error
	// Podar apaga até limite linhas anteriores a antesDe e devolve quantas
	// apagou. O limite existe para que a varredura não segure o escritor único.
	Podar(ctx context.Context, antesDe time.Time, limite int) (int64, error)
}
