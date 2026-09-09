package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResultadoChamada classifica o desfecho de um tools/call despachado.
//
// Vocabulário deste pacote e não de quem observa: o endpoint é quem sabe a
// diferença entre "o upstream recusou" e "o prazo estourou", e quem observa não
// pode precisar importar nada para entender o que recebeu.
type ResultadoChamada string

// Desfechos possíveis.
const (
	// ChamadaOK é a chamada que voltou sem erro nenhum.
	ChamadaOK ResultadoChamada = "ok"
	// ChamadaErro é a chamada que falhou, incluindo a que voltou com
	// IsError do próprio upstream.
	ChamadaErro ResultadoChamada = "erro"
	// ChamadaTimeout é a chamada que estourou o prazo do upstream.
	ChamadaTimeout ResultadoChamada = "timeout"
)

// Chamada é o que o endpoint sabe de um tools/call já respondido.
//
// Sem argumento e sem resultado, só os tamanhos: os dois são dado de terceiro e
// o caminho mais curto para um segredo sair do processo. Quem observa também não
// precisa deles — o que se diagnostica é "grande demais", e para isso o número
// basta.
type Chamada struct {
	EndpointID   int64
	EndpointSlug string
	UpstreamID   int64
	UpstreamNome string
	// Ferramenta é o nome que o cliente chamou; Original é o nome no upstream.
	Ferramenta string
	Original   string

	Inicio    time.Time
	Duracao   time.Duration
	Resultado ResultadoChamada
	// Erro é a mensagem do erro, crua. Quem observa é responsável por redigir:
	// a mensagem de um upstream pode repetir o header que ele recusou.
	Erro string

	BytesEntrada int
	BytesSaida   int

	// Sessao é o id de sessão do cliente, cru. Quem observa anonimiza.
	Sessao string
	// Credencial é o TokenInfo.UserID de quem chamou — "apikey:<id>" ou
	// "oauth:<client_id>". Nunca a credencial em si.
	Credencial string
	// Era é a versão do protocolo MCP negociada naquela sessão.
	Era string
}

// Observador recebe cada chamada de ferramenta depois de ela ter sido
// respondida.
//
// **Observar não pode bloquear**: ela roda na goroutine que atende o cliente,
// depois de o resultado já estar pronto, e qualquer espera ali vira latência do
// tools/call. A implementação enfileira e volta.
//
// Interface declarada aqui, no consumidor, com um método: quem grava a trilha é
// outra feature, e feature não importa feature.
type Observador interface {
	Observar(c Chamada)
}

// ComObservador liga o gancho de captura da trilha. Sem ele, nada é observado —
// e é assim que os testes deste pacote rodam.
func ComObservador(o Observador) Opcao {
	return func(s *Servidores) { s.obs = o }
}

// observar monta a Chamada e a entrega ao observador.
//
// Todo o custo que dá para tirar do caminho da requisição já está do outro lado
// da interface; o que sobra aqui é ler dois campos da sessão e somar tamanhos.
//
// recover cobre o Observador de terceiro: ele é uma interface de um método
// implementada fora deste pacote, e um bug nela — um nil não conferido, um
// índice fora da faixa — não pode derrubar a resposta ao cliente que já está
// pronta. Sem o recover, o panic subiria por cima do handler de tools/call e
// a chamada nunca voltaria, mesmo tendo funcionado no upstream.
func (s *Servidores) observar(reg Registro, f capturada, req *mcp.CallToolRequest, inicio time.Time, entrada int, res *mcp.CallToolResult, err error) {
	if s.obs == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("observador da trilha entrou em panic",
				"ferramenta", f.nomeExposto, "panic", r)
		}
	}()
	c := Chamada{
		EndpointID:   reg.ID,
		EndpointSlug: reg.Slug,
		UpstreamID:   f.upstreamID,
		UpstreamNome: f.upstreamNome,
		Ferramenta:   f.nomeExposto,
		Original:     f.nomeOriginal,
		Inicio:       inicio,
		Duracao:      time.Since(inicio),
		Resultado:    classificar(res, err),
		BytesEntrada: entrada,
		BytesSaida:   tamanhoDoResultado(res),
	}
	if err != nil {
		c.Erro = err.Error()
	}
	if req != nil && req.Session != nil {
		c.Sessao = req.Session.ID()
		if p := req.Session.InitializeParams(); p != nil {
			c.Era = p.ProtocolVersion
		}
	}
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
		c.Credencial = req.Extra.TokenInfo.UserID
	}
	s.obs.Observar(c)
}

// capturada são os campos da ferramenta sobre os quais o handler já fecha. Existe
// para observar não precisar de uma catalogo.Ferramenta inteira por chamada.
type capturada struct {
	upstreamID   int64
	upstreamNome string
	nomeExposto  string
	nomeOriginal string
}

// classificar decide o desfecho.
//
// IsError do upstream conta como erro, e não como ok: da perspectiva de quem
// diagnostica, uma ferramenta que responde "não achei o arquivo" é uma chamada
// que não fez o que o cliente pediu — e poder filtrar por isso é metade do valor
// da tela. O texto do erro continua na resposta ao cliente, intacto.
func classificar(res *mcp.CallToolResult, err error) ResultadoChamada {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ChamadaTimeout
	case err != nil:
		return ChamadaErro
	case res != nil && res.IsError:
		return ChamadaErro
	default:
		return ChamadaOK
	}
}

// tamanhoDoResultado estima quantos bytes o resultado tem.
//
// Soma o que já está em memória em vez de serializar: um json.Marshal por
// chamada duplicaria o resultado inteiro no caminho da requisição para produzir
// um número de ordem de grandeza. Conteúdo que não é texto, imagem ou áudio não
// entra na conta, e um número um pouco baixo é melhor que uma alocação a mais
// por tools/call.
func tamanhoDoResultado(res *mcp.CallToolResult) int {
	if res == nil {
		return 0
	}
	total := 0
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			total += len(v.Text)
		case *mcp.ImageContent:
			total += len(v.Data)
		case *mcp.AudioContent:
			total += len(v.Data)
		}
	}
	if bruto, ok := res.StructuredContent.(json.RawMessage); ok {
		total += len(bruto)
	}
	return total
}
