package endpoint

import (
	"slices"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

// A lista de ferramentas do formulário de endpoint.
//
// A tela mostra cada ferramenta do MCP com uma caixa (ligada ou não) e um campo
// de renome; o banco continua guardando regras, porque é o que o catálogo sabe
// aplicar. Este arquivo traduz nos dois sentidos. O que não é regra de nome
// exato de uma ferramenta conhecida (glob, nome que o MCP não expõe agora)
// fica em "Regras avançadas", intocado.

// ItemFerramenta é uma linha da lista de ferramentas de um MCP no formulário.
type ItemFerramenta struct {
	Nome   string
	Ligada bool
	Renome string
}

// Nomes dos campos da lista de ferramentas de um upstream.
func chaveNomes(id int64) string  { return "nomes_" + strconv.FormatInt(id, 10) }
func chaveLigada(id int64) string { return "ligada_" + strconv.FormatInt(id, 10) }
func chaveRenome(id int64) string { return "renome_" + strconv.FormatInt(id, 10) }

// separarRegras reparte as regras gravadas entre a lista de ferramentas e o
// texto avançado. Texto que não analisa volta inteiro como avançado, para a
// tela mostrar o erro sobre o que a pessoa escreveu.
func separarRegras(texto string, nomes []string) ([]ItemFerramenta, string) {
	itens := make([]ItemFerramenta, len(nomes))
	indice := map[string]int{}
	for i, n := range nomes {
		itens[i] = ItemFerramenta{Nome: n, Ligada: true}
		indice[n] = i
	}
	regras, err := catalogo.AnalisarRegras(texto)
	if err != nil {
		return itens, texto
	}
	var avancadas []string
	for _, r := range regras {
		i, conhecida := indice[r.Padrao]
		exata := conhecida && !strings.Contains(r.Padrao, "*")
		switch {
		case exata && r.Acao == catalogo.AcaoExcluir:
			itens[i].Ligada = false
		case exata && r.Acao == catalogo.AcaoIncluir:
			// gerada pela própria tela para vencer um filtro avançado
		case exata && r.Acao == catalogo.AcaoRenomear && !strings.Contains(r.Renome, "*"):
			itens[i].Renome = r.Renome
		default:
			avancadas = append(avancadas, formatarRegra(r))
		}
	}
	return itens, strings.Join(avancadas, "\n")
}

// montarRegras faz o caminho inverso: lista de ferramentas + avançado → texto
// de regras. As regras da lista vêm antes, então a caixa vence o avançado para
// a ferramenta que ela nomeia; o `incluir` explícito só sai quando o avançado
// tem filtro, que é o único caso em que ele muda alguma coisa.
func montarRegras(nomes, ligadas, renomes []string, avancadas string) string {
	temFiltro := false
	if rs, err := catalogo.AnalisarRegras(avancadas); err == nil {
		temFiltro = slices.ContainsFunc(rs, func(r catalogo.Regra) bool {
			return r.Acao != catalogo.AcaoRenomear
		})
	}
	var linhas []string
	for i, n := range nomes {
		ligada := slices.Contains(ligadas, n)
		switch {
		case !ligada:
			linhas = append(linhas, "excluir "+n)
		case temFiltro:
			linhas = append(linhas, "incluir "+n)
		}
		if ligada && i < len(renomes) {
			if novo := strings.TrimSpace(renomes[i]); novo != "" && novo != n {
				linhas = append(linhas, "renomear "+n+" "+novo)
			}
		}
	}
	if a := strings.TrimSpace(avancadas); a != "" {
		linhas = append(linhas, a)
	}
	return strings.Join(linhas, "\n")
}

func formatarRegra(r catalogo.Regra) string {
	if r.Acao == catalogo.AcaoRenomear {
		return string(r.Acao) + " " + r.Padrao + " " + r.Renome
	}
	return string(r.Acao) + " " + r.Padrao
}
