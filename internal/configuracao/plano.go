package configuracao

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// Operacao é o que o import faria com um item.
type Operacao string

// As operações do plano.
//
// Oito e não três porque o plano é o que uma pessoa lê antes de decidir: "não
// muda nada", "o banco andou e eu não vou mexer" e "os dois mudaram, decida
// você" são resultados diferentes, e colapsá-los em "nada a fazer" esconderia
// exatamente a informação pela qual a trava otimista existe.
const (
	// OperacaoCriar é item do YAML que não existe no banco.
	OperacaoCriar Operacao = "criar"
	// OperacaoAtualizar é item que o YAML mudou e o banco não.
	OperacaoAtualizar Operacao = "atualizar"
	// OperacaoRemover é item do banco fora do YAML, com --remover-ausentes.
	OperacaoRemover Operacao = "remover"
	// OperacaoSemMudanca é item idêntico dos dois lados — ou item que só o banco
	// mudou, caso em que o banco fica e o motivo diz por quê.
	OperacaoSemMudanca Operacao = "sem-mudança"
	// OperacaoConflito é item que mudou dos dois lados desde o export. Não é
	// aplicado, e não impede nenhum outro item de ser.
	OperacaoConflito Operacao = "conflito"
	// OperacaoAusente é item do banco fora do YAML, sem --remover-ausentes.
	OperacaoAusente Operacao = "ausente"
	// OperacaoInformativo é item que o export registra e o import não aplica:
	// chave de API e cliente OAuth, que guardam credencial por hash.
	OperacaoInformativo Operacao = "informativo"
	// OperacaoErro é item que não dá para aplicar. O motivo diz o quê.
	OperacaoErro Operacao = "erro"
)

// Tipos de item do plano.
const (
	ItemUpstream     = "upstream"
	ItemSegredo      = "segredo"
	ItemEndpoint     = "endpoint"
	ItemChaveAPI     = "chave-api"
	ItemClienteOAuth = "cliente-oauth"
)

// Divergencia é um campo que difere entre o YAML e o banco. É o que transforma
// "conflitou" em algo que o dono consegue decidir sem abrir o banco.
type Divergencia struct {
	Campo   string
	NoYAML  string
	NoBanco string
}

// Item é uma linha do plano.
//
// Os campos de aplicação não são exportados: o plano é para ler e para mostrar, e
// quem aplica é o Aplicar deste pacote. Exportá-los convidaria uma borda a montar
// um plano à mão e a pular o Planejar, que é onde a mescla acontece.
type Item struct {
	Tipo     string
	Nome     string
	Operacao Operacao
	// Motivo é o texto para uma pessoa: por que conflitou, por que é erro, por
	// que ficou sem mudança. Vazio quando a operação já se explica.
	Motivo       string
	Divergencias []Divergencia

	id       int64
	upstream *Upstream
	endpoint *Endpoint
	segredo  *slotDeSegredo
}

// Aplicavel informa se este item chega a escrever no banco.
func (i Item) Aplicavel() bool {
	switch i.Operacao {
	case OperacaoCriar, OperacaoAtualizar, OperacaoRemover:
		return true
	default:
		return false
	}
}

// fase é a ordem em que o item entra no banco.
//
// Upstream antes de endpoint porque a composição referencia upstream por nome, e
// um endpoint novo pode citar um upstream que este mesmo import acabou de criar.
// Remoção na ordem inversa, e endpoint antes de upstream: apagar um upstream que
// um endpoint ainda cita mudaria o tools/list daquele endpoint por cascata, sem
// que o relatório dissesse que foi isso que aconteceu.
func (i Item) fase() int {
	switch {
	case i.Tipo == ItemUpstream && i.Operacao == OperacaoRemover:
		return 5
	case i.Tipo == ItemEndpoint && i.Operacao == OperacaoRemover:
		return 4
	case i.Tipo == ItemEndpoint:
		return 3
	case i.Tipo == ItemSegredo:
		return 2
	default:
		return 1
	}
}

// slotDeSegredo é a intenção de escrita de uma credencial estática.
//
// O valor é cripto.Segredo e não string para que ele não apareça em log nem em
// %v por acidente: o tipo redige a si mesmo.
type slotDeSegredo struct {
	upstreamID   int64
	upstreamNome string
	tipo         string
	nome         string
	valor        cripto.Segredo
	limpar       bool
}

// Plano é o que o import faria, item a item, antes de escrever qualquer coisa.
type Plano struct {
	// Versao é a versão de schema do documento lido.
	Versao int
	// Revisao é a revisão que o YAML trouxe, e RevisaoAtual a do banco agora.
	Revisao      string
	RevisaoAtual string
	// BancoAvancou é a trava otimista: verdadeiro quando o banco mudou desde o
	// export de onde este arquivo saiu. Não recusa nada — muda o resultado de
	// recusa para mescla, que é a decisão do dono de 2026-09-08.
	BancoAvancou bool
	// RemoverAusentes é a flag com que o plano foi montado. Está aqui porque o
	// plano precisa ser autoexplicativo: sem ela, "remover" e "ausente" seriam
	// dois planos idênticos com resultados opostos.
	RemoverAusentes bool

	Itens []Item
}

// Contar devolve quantos itens há de cada operação.
func (p Plano) Contar() map[Operacao]int {
	out := make(map[Operacao]int, len(p.Itens))
	for _, i := range p.Itens {
		out[i.Operacao]++
	}
	return out
}

// Aplicaveis é quantos itens escrevem no banco.
func (p Plano) Aplicaveis() int {
	n := 0
	for _, i := range p.Itens {
		if i.Aplicavel() {
			n++
		}
	}
	return n
}

// Conflitos é quantos itens mudaram dos dois lados.
func (p Plano) Conflitos() int { return p.Contar()[OperacaoConflito] }

// Erros é quantos itens não dão para aplicar.
func (p Plano) Erros() int { return p.Contar()[OperacaoErro] }

// Escrever imprime o plano em texto, uma linha por item.
//
// Texto e não JSON porque o destino é o terminal de quem vai decidir. A ordem é a
// de leitura — upstreams, segredos, endpoints, o resto —, e não a de aplicação:
// quem lê quer conferir a configuração, não a sequência de escritas.
func (p Plano) Escrever(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "plano de import (schema %d)\n", p.Versao); err != nil {
		return fmt.Errorf("configuracao: escrever plano: %w", err)
	}
	if p.BancoAvancou {
		_, _ = fmt.Fprintf(w, `
o banco avançou desde o export deste arquivo
  revisão no arquivo: %s
  revisão no banco:   %s
o import mescla: aplica o que só o arquivo mudou, mantém o que só o banco mudou
e reporta como conflito o que os dois mudaram.
`, ouVazio(p.Revisao), p.RevisaoAtual)
	}
	_, _ = fmt.Fprintln(w)
	for _, i := range p.Itens {
		_, _ = fmt.Fprintf(w, "  %-11s %-14s %s\n", i.Operacao, i.Tipo, i.Nome)
		if i.Motivo != "" {
			_, _ = fmt.Fprintf(w, "              %s\n", i.Motivo)
		}
		for _, d := range i.Divergencias {
			_, _ = fmt.Fprintf(w, "              %s: arquivo %s / banco %s\n",
				d.Campo, aspas(d.NoYAML), aspas(d.NoBanco))
		}
	}

	contagem := p.Contar()
	_, _ = fmt.Fprintf(w, "\n%d item(ns): %s\n", len(p.Itens), resumoDaContagem(contagem))
	if !p.RemoverAusentes && contagem[OperacaoAusente] > 0 {
		_, _ = fmt.Fprintf(w,
			"%d item(ns) existem no banco e não no arquivo; use --remover-ausentes para apagá-los.\n",
			contagem[OperacaoAusente])
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return fmt.Errorf("configuracao: escrever plano: %w", err)
	}
	return nil
}

// Resultado é o que aconteceu com um item na aplicação.
type Resultado struct {
	Item Item
	// Erro é o texto da falha daquele item. Vazio quando ele foi aplicado.
	Erro string
}

// Relatorio é o resultado do import.
//
// Cada item aplicado é uma transação própria, então um item que falha não desfaz
// os anteriores — e o relatório é o que diz onde o import parou de casar com o
// arquivo. Sem ele, "import falhou" deixaria o dono sem saber o que ficou.
type Relatorio struct {
	Resultados []Resultado
	// Ignorados são os itens do plano que nunca escrevem: conflito, ausente,
	// informativo, erro e sem-mudança.
	Ignorados []Item
}

// Aplicados é quantos itens entraram no banco.
func (r Relatorio) Aplicados() int {
	n := 0
	for _, res := range r.Resultados {
		if res.Erro == "" {
			n++
		}
	}
	return n
}

// Falhas é quantos itens não entraram.
func (r Relatorio) Falhas() int { return len(r.Resultados) - r.Aplicados() }

// Escrever imprime o relatório em texto.
func (r Relatorio) Escrever(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "import aplicado"); err != nil {
		return fmt.Errorf("configuracao: escrever relatório: %w", err)
	}
	_, _ = fmt.Fprintln(w)
	for _, res := range r.Resultados {
		if res.Erro == "" {
			_, _ = fmt.Fprintf(w, "  ok      %-11s %-14s %s\n",
				res.Item.Operacao, res.Item.Tipo, res.Item.Nome)
			continue
		}
		_, _ = fmt.Fprintf(w, "  falhou  %-11s %-14s %s\n              %s\n",
			res.Item.Operacao, res.Item.Tipo, res.Item.Nome, res.Erro)
	}
	for _, i := range r.Ignorados {
		_, _ = fmt.Fprintf(w, "  -       %-11s %-14s %s\n", i.Operacao, i.Tipo, i.Nome)
		if i.Motivo != "" {
			_, _ = fmt.Fprintf(w, "              %s\n", i.Motivo)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d aplicado(s), %d falha(s), %d não aplicado(s)\n\n",
		r.Aplicados(), r.Falhas(), len(r.Ignorados))
	return nil
}

// resumoDaContagem monta "3 criar, 1 conflito" na ordem fixa das operações, para
// que dois planos parecidos produzam linhas comparáveis.
func resumoDaContagem(contagem map[Operacao]int) string {
	ordem := []Operacao{
		OperacaoCriar, OperacaoAtualizar, OperacaoRemover, OperacaoSemMudanca,
		OperacaoConflito, OperacaoAusente, OperacaoInformativo, OperacaoErro,
	}
	partes := make([]string, 0, len(ordem))
	for _, op := range ordem {
		if n := contagem[op]; n > 0 {
			partes = append(partes, strconv.Itoa(n)+" "+string(op))
		}
	}
	if len(partes) == 0 {
		return "nada a fazer"
	}
	return strings.Join(partes, ", ")
}

func aspas(s string) string {
	if s == "" {
		return "(vazio)"
	}
	return strconv.Quote(s)
}

func ouVazio(s string) string {
	if s == "" {
		return "(sem revisão)"
	}
	return s
}
