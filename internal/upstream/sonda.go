package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Padrões da sonda funcional (seção 08.7 do estudo).
//
// Nenhum deles é um default medido, e o intervalo é o caso mais evidente: a
// sonda consome cota da API do provedor, então quem escolhe é quem paga a cota.
// O que estes números garantem é que o formulário abre com um valor coerente em
// vez de zero — timeout invisível é o que transforma "está lento" numa
// investigação de meia hora (seção 11), e o mesmo vale para intervalo.
const (
	// SondaIntervaloPadraoMS é o intervalo sugerido: 15 min, o número do
	// exemplo de YAML da seção 08.8.
	SondaIntervaloPadraoMS int64 = 900_000
	// SondaIntervaloMinimoMS impede que a sonda vire a carga que ela deveria
	// diagnosticar.
	SondaIntervaloMinimoMS int64 = 5_000
	// SondaIntervaloMaximoMS é um dia: acima disso a sonda deixa de detectar
	// qualquer coisa antes de o admin notar sozinho.
	SondaIntervaloMaximoMS int64 = 24 * 60 * 60 * 1000
	// SondaTimeoutPadraoMS é o prazo sugerido de uma sondagem.
	SondaTimeoutPadraoMS int64 = 15_000
	// SondaToleranciaPadrao é quantas sondagens seguidas precisam falhar antes
	// de o upstream ir a sonda_falhou. Duas, e não uma: um soluço isolado é o
	// que o backoff já cobre, e derrubar o catálogo por causa dele faria da
	// sonda uma fonte de instabilidade em vez de um detector.
	SondaToleranciaPadrao int = 2
	// SondaToleranciaMaxima é o teto que o formulário aceita. Acima disso a
	// sonda demora tanto para reagir que já não protege ninguém.
	SondaToleranciaMaxima int = 10
)

// limiteEvidenciaSonda é o teto, em bytes, do pedido e da resposta guardados
// para a tela.
//
// A seção 11 exige a requisição e a resposta exatas — sem elas o admin não
// distingue "minha sonda está mal configurada" de "o servidor está quebrado" —,
// mas "exatas" não pode significar "inteiras": a resposta é dado de terceiro e
// pode ter megabytes. O corte é anunciado com reticências, e nada disto vai ao
// banco: vive só em memória, na situação do upstream. "Exata" também não pode
// significar "crua": a resposta pode repetir de volta o header de autorização
// que o upstream recusou, e é por isso que aplicarResultadoSonda passa
// Resposta e Erro pelo Redator injetado (ComRedator) antes de eles chegarem à
// tela.
const limiteEvidenciaSonda = 2 * 1024

// limiteArgsDeSonda é o teto, em bytes, do texto de argumentos que o
// formulário aceita.
//
// Sem teto, um JSON absurdo gravado por acidente (ou colado errado) viraria
// bytes de sobra em toda sondagem — no pedido guardado em memória e no corpo
// de toda requisição de verdade que a sonda manda ao upstream.
const limiteArgsDeSonda = 4 * 1024

// ErrSondaDesligada indica sondagem pedida a um upstream sem sonda configurada
// ou com ela desligada.
var ErrSondaDesligada = errors.New("upstream: sonda desligada")

// Sonda é a configuração da sonda funcional de um upstream.
//
// Ela executa um tools/call de verdade, com a ferramenta e os argumentos que o
// admin escolheu. Nada aqui é inferido: o patchbay não tem como saber se
// send_message é inócua, e adivinhar seria a forma de mandar mensagem para
// alguém sem querer (seção 08.7).
//
// Args é json.RawMessage e é imutável depois de construída, como Config.Args e
// Config.Env: a Sonda viaja por valor entre o repositório, o gerente e a
// supervisão, e a cópia é rasa. Reconfigurar é sempre construir outra.
type Sonda struct {
	// Habilitada é o opt-in. Falso por padrão, e é a defesa principal do
	// resíduo nomeado na seção 13: uma sonda mal configurada apaga as
	// ferramentas de um servidor saudável.
	Habilitada bool
	// Ferramenta é o nome no upstream, nunca o nome exposto por um endpoint: a
	// sonda fala com o upstream direto, sem prefixo nem renome no caminho.
	Ferramenta string
	// Args é o objeto de argumentos do tools/call, como JSON. Vazio manda a
	// chamada sem argumentos.
	Args json.RawMessage
	// Espera é o trecho que precisa aparecer no texto da resposta. Vazio
	// significa "basta não dar erro"; preenchido, cobre o servidor que responde
	// um corpo de erro amigável sem marcar isError.
	Espera string
	// Intervalo é de quanto em quanto tempo a sondagem se repete.
	Intervalo time.Duration
	// Timeout é o prazo de uma sondagem. Cancela só a chamada — nunca a sessão.
	Timeout time.Duration
	// Tolerancia é quantas sondagens seguidas precisam falhar para o upstream
	// ir a sonda_falhou.
	Tolerancia int
}

// Ativa informa se a supervisão deve sondar este upstream.
//
// Habilitada sozinha não basta: uma sonda sem ferramenta não tem o que chamar, e
// tratá-la como ligada faria o upstream cair em sonda_falhou por configuração
// pela metade — exatamente o modo de falha que a fatia existe para evitar.
func (s Sonda) Ativa() bool { return s.Habilitada && s.Ferramenta != "" }

// Normalizada devolve a sonda com os padrões aplicados no que veio fora de
// faixa.
//
// Normaliza em vez de recusar porque quem chama é a supervisão, e um intervalo
// zero vindo de um banco antigo não pode virar um laço de sondagem sem espera.
// Quem recusa valor inválido é o formulário, com a mensagem no campo certo.
func (s Sonda) Normalizada() Sonda {
	if s.Intervalo < time.Duration(SondaIntervaloMinimoMS)*time.Millisecond {
		s.Intervalo = time.Duration(SondaIntervaloPadraoMS) * time.Millisecond
	}
	if s.Timeout <= 0 {
		s.Timeout = time.Duration(SondaTimeoutPadraoMS) * time.Millisecond
	}
	if s.Tolerancia < 1 {
		s.Tolerancia = SondaToleranciaPadrao
	}
	return s
}

// Validar recusa a configuração que a supervisão não saberia executar.
func (s Sonda) Validar(nomeDoUpstream string) error {
	if !s.Habilitada {
		return nil
	}
	if strings.TrimSpace(s.Ferramenta) == "" {
		return fmt.Errorf("upstream %s: sonda habilitada sem ferramenta", nomeDoUpstream)
	}
	if err := ValidarArgsDeSonda(string(s.Args)); err != nil {
		return fmt.Errorf("upstream %s: %w", nomeDoUpstream, err)
	}
	return nil
}

// ValidarArgsDeSonda confere que o texto é um objeto JSON — ou vazio.
//
// Objeto e não qualquer JSON: Arguments do tools/call é um objeto por
// especificação, e um array aqui vira um erro do servidor que a tela
// descreveria como "o upstream está quebrado".
func ValidarArgsDeSonda(bruto string) error {
	bruto = strings.TrimSpace(bruto)
	if bruto == "" {
		return nil
	}
	if len(bruto) > limiteArgsDeSonda {
		return fmt.Errorf("argumentos da sonda passam do teto de %d KiB", limiteArgsDeSonda/1024)
	}
	var objeto map[string]any
	if err := json.Unmarshal([]byte(bruto), &objeto); err != nil {
		return fmt.Errorf("argumentos da sonda não são um objeto JSON: %w", err)
	}
	if objeto == nil {
		// json.Unmarshal([]byte("null"), &objeto) não erra — um mapa nulo é
		// unmarshal válido de "null" — e sem esta checagem "null" passaria
		// como argumento e a sonda chamaria a ferramenta com Arguments nulo.
		return errors.New("argumentos da sonda não são um objeto JSON: null não conta como objeto")
	}
	return nil
}

// ArgsDeSonda normaliza o texto do formulário no que vai para o tools/call.
// Texto vazio (ou só espaço) vira nada, e não "{}": chamada sem argumentos é o
// caso comum, e mandar um objeto vazio não é a mesma coisa para todo servidor.
func ArgsDeSonda(bruto string) json.RawMessage {
	bruto = strings.TrimSpace(bruto)
	if bruto == "" {
		return nil
	}
	return json.RawMessage(bruto)
}

// ResultadoSonda é o desfecho de uma sondagem, do jeito que a tela o mostra.
//
// Pedido e Resposta são a evidência que a seção 11 exige: sem os dois exatos, o
// admin não distingue "minha sonda está mal configurada" de "o servidor está
// quebrado", e a saída mais fácil passa a ser desligar a sonda. Os dois vivem
// só em memória.
type ResultadoSonda struct {
	// Em é quando a sondagem começou.
	Em time.Time
	// Duracao é quanto ela levou.
	Duracao time.Duration
	// OK diz se a sondagem passou.
	OK bool
	// Timeout separa "estourou o prazo" de "respondeu erro": são diagnósticos
	// diferentes, e o texto do erro sozinho não os separa.
	Timeout bool
	// Erro é o motivo da falha, vazio quando OK.
	Erro string
	// Pedido é a chamada exata: nome da ferramenta e argumentos.
	Pedido string
	// Resposta é o texto exato que voltou, truncado em limiteEvidenciaSonda.
	Resposta string
	// BytesEntrada e BytesSaida são os tamanhos, para a trilha.
	BytesEntrada int
	BytesSaida   int
}

// Sondagem é o que a sonda entrega a quem observa.
//
// Espelha o que a trilha precisa saber sem que este pacote conheça a trilha:
// feature não importa feature, e a tradução mora em cmd/.
type Sondagem struct {
	UpstreamID   int64
	UpstreamNome string
	// Ferramenta é o nome no upstream. Não há nome exposto aqui: a sonda não
	// passa por endpoint nenhum, então não há prefixo nem renome a aplicar.
	Ferramenta string

	Inicio  time.Time
	Duracao time.Duration

	OK      bool
	Timeout bool
	// Erro é a mensagem crua. Quem observa é responsável por redigir: a
	// resposta de um upstream pode repetir o header que ele recusou.
	Erro string

	BytesEntrada int
	BytesSaida   int
}

// ObservadorDeSonda recebe cada sondagem depois de ela ter terminado.
//
// **ObservarSonda não pode bloquear**: ela roda na goroutine de supervisão do
// upstream, e uma espera ali atrasa a reconexão e a renovação de token. A
// implementação enfileira e volta.
//
// Interface declarada aqui, no consumidor, com um método só — mesmo desenho de
// endpoint.Observador, e pelo mesmo motivo.
type ObservadorDeSonda interface {
	ObservarSonda(s Sondagem)
}

// ComObservadorDeSonda liga o rastro da sonda na trilha. Sem ele a sonda
// continua funcionando: o que falta é a linha na tela.
func ComObservadorDeSonda(o ObservadorDeSonda) Opcao {
	return func(g *Gerente) { g.obsSonda = o }
}

// ComRedator troca a função que limpa Pedido e Resposta antes de eles virarem
// SituacaoSonda.
//
// Existe como opção, e não como chamada direta a internal/trilha, porque
// features não se importam (a regra do pacote): este é o candidato natural a
// trilha.Redigir, e é cmd/patchbay/adaptadores.go que faz a ligação. Sem esta
// opção o redator é a identidade — a evidência chega crua na tela, o que só é
// seguro em teste que não sonda upstream de verdade.
func ComRedator(r func(string) string) Opcao {
	return func(g *Gerente) {
		if r != nil {
			g.redator = r
		}
	}
}

// pedidoSonda é uma sondagem pedida de fora, pela tela.
//
// O canal de resposta tem buffer 1 para que a supervisão nunca fique presa
// respondendo a quem já desistiu (o clique que virou timeout de requisição).
type pedidoSonda struct {
	pronto chan ResultadoSonda
}

// avaliarSonda decide o desfecho de uma sondagem.
//
// Os três critérios da seção 08.7 — erro de transporte, timeout e isError —
// mais o trecho esperado, que é opt-in. isError conta como falha porque, do
// ponto de vista de quem depende da ferramenta, "não achei o arquivo" é a
// ferramenta não fazendo o que promete.
func avaliarSonda(s Sonda, res *mcp.CallToolResult, err error, texto string) (motivo string, timeout bool) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("a sondagem de %s estourou o prazo de %s", s.Ferramenta, s.Timeout), true
	case err != nil:
		return err.Error(), false
	case res == nil:
		return "o upstream respondeu ao tools/call sem resultado nenhum", false
	case res.IsError:
		return "a ferramenta respondeu com isError", false
	case s.Espera != "" && !strings.Contains(texto, s.Espera):
		return fmt.Sprintf("a resposta não contém o trecho esperado %q", s.Espera), false
	default:
		return "", false
	}
}

// textoDoResultado junta o conteúdo textual da resposta, que é o que a tela
// mostra e o que o trecho esperado procura.
//
// Só texto de Content: imagem e áudio são bytes que não ajudam ninguém a
// diagnosticar e que inflariam a evidência guardada em memória. Quando não há
// nenhum (SEP-2106 permite responder só com StructuredContent), o JSON
// estruturado entra no lugar — sem ele, uma ferramenta de saída
// estruturada sondaria "certo" com Resposta sempre vazia, mesmo quando o
// trecho esperado (Sonda.Espera) precisaria olhar o valor de verdade.
func textoDoResultado(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if bruto, err := json.Marshal(res.StructuredContent); err == nil {
			return string(bruto)
		}
	}
	return b.String()
}

// pedidoDaSonda descreve a chamada exata, para a tela.
func pedidoDaSonda(s Sonda) string {
	if len(s.Args) == 0 {
		return s.Ferramenta + "()"
	}
	return s.Ferramenta + "(" + string(s.Args) + ")"
}

// truncarEvidencia corta em n bytes sem quebrar rune, anunciando o corte.
func truncarEvidencia(s string, n int) string {
	if len(s) <= n {
		return s
	}
	corte := n
	for corte > 0 && !utf8.RuneStart(s[corte]) {
		corte--
	}
	return s[:corte] + "… (cortado)"
}
