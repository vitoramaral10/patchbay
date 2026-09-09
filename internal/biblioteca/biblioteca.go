// Package biblioteca é o catálogo embutido de servidores MCP remotos
// conhecidos, e a tela que transforma um deles num upstream cadastrado.
//
// Existe porque cadastrar um upstream hoje é digitar URL, transporte e modo de
// credencial certos na primeira tentativa: três campos em que errar não dá erro
// de formulário, dá um upstream degradado. A biblioteca troca isso por escolher
// um nome de uma lista.
//
// O catálogo é um instantâneo embutido no binário (o catalogo.json deste
// diretório, incorporado na build), e não uma consulta a um site em tempo de
// execução. Três razões, e a ordem importa:
//
//  1. O patchbay é entregue como binário único e roda em rede fechada. Uma tela
//     que só funciona com saída para a internet não é binário único — é o mesmo
//     argumento que vendoriza o htmx em vez de puxá-lo de CDN.
//  2. A origem (mcpservers.org) fica atrás de desafio de bot. Uma busca ao vivo
//     falharia de forma intermitente e sem causa visível para o admin.
//  3. Instantâneo é auditável: o que a tela oferece está no diff de quem
//     atualizou o catálogo, não no que o site respondeu naquele segundo.
//
// Quem regenera o catalogo.json é ./cmd/patchbay-biblioteca. Este pacote só lê.
//
// A biblioteca não cadastra o upstream: ela leva o admin ao formulário de
// upstream já preenchido (RotaCadastro). Isso não é só desenho de tela — a
// regra de arquitetura proíbe uma feature de importar outra, e o formulário
// preenchido é o contrato entre as duas, verificado no teste de integração em
// cmd/patchbay. O admin ainda revisa e salva, que é o certo: a URL vem de um
// terceiro, e ninguém deveria cadastrá-la sem olhar.
package biblioteca

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Transportes possíveis de um item. São os mesmos rótulos que o formulário de
// upstream usa em ?tipo=, e é de propósito: o valor viaja daqui para lá sem
// tradução, e uma tabela de conversão no meio é onde os dois lados divergem.
const (
	TransporteHTTP = "http"
	TransporteSSE  = "sse"
)

// Formas de autenticação que a origem declara.
//
// Não é o modo de credencial do upstream — é o que o servidor exige. A tradução
// para o modo acontece em ModoDeCredencial, e ela é de mão única: oauth vira
// oauth, e todo o resto vira estática, porque estática com bearer em branco é
// um formulário que o admin completa, enquanto oauth errado é um fluxo de
// consentimento que não fecha.
const (
	// AutOAuth é consentimento por navegador.
	AutOAuth = "oauth"
	// AutToken é chave ou token colado pelo admin.
	AutToken = "token"
	// AutAberta é servidor sem autenticação nenhuma.
	AutAberta = "aberta"
)

// Item é um servidor MCP remoto do catálogo.
//
// Só carrega o que o cadastro precisa mais o que ajuda a decidir. Não guarda
// contagem de estrelas, popularidade nem data: número que envelhece dentro de
// um binário é número que mente, e o instantâneo não tem como atualizá-lo.
type Item struct {
	// Slug identifica o item dentro do catálogo e na origem.
	Slug string `json:"slug"`
	// Nome é como o servidor se chama, e vira o nome sugerido do upstream.
	Nome string `json:"nome"`
	// Resumo é a linha curta da lista.
	Resumo string `json:"resumo"`
	// Descricao é o parágrafo da tela de detalhe. Pode ser vazio.
	Descricao string `json:"descricao,omitempty"`
	// URL é o endpoint MCP do servidor.
	URL string `json:"url"`
	// Transporte é http ou sse.
	Transporte string `json:"transporte"`
	// Autenticacao é oauth, token ou aberta.
	Autenticacao string `json:"autenticacao"`
	// Docs é a documentação oficial do servidor, quando a origem a declara.
	Docs string `json:"docs,omitempty"`
	// Fonte é a página de onde este item foi lido, para o admin conferir.
	Fonte string `json:"fonte,omitempty"`
}

// ModoDeCredencial traduz a autenticação declarada no modo que o formulário de
// upstream entende.
func (i Item) ModoDeCredencial() string {
	if i.Autenticacao == AutOAuth {
		return "oauth"
	}
	return "estatica"
}

// RotaCadastro é o link de "adicionar": o formulário de upstream novo já
// preenchido com este item.
//
// Os nomes dos parâmetros são contrato com internal/upstream, e a regra de
// arquitetura impede importar aquele pacote para pegá-los de uma constante
// compartilhada. Quem guarda o contrato é
// TestAdicionarDaBibliotecaAbreFormularioPreenchido, em cmd/patchbay: ele segue
// este link e confere que o formulário volta preenchido.
func (i Item) RotaCadastro() string {
	q := url.Values{
		"tipo": {i.Transporte},
		"nome": {i.Nome},
		"url":  {i.URL},
		"modo": {i.ModoDeCredencial()},
	}
	return webui.RotaUpstreams + "/novo?" + q.Encode()
}

// termoDeBusca é o texto contra o qual Buscar compara, montado uma vez no
// carregamento.
//
// Pré-calculado e não montado a cada consulta porque a busca roda a cada tecla
// digitada (htmx), e 293 itens vezes quatro concatenações por tecla é trabalho
// que não precisa existir.
func (i Item) termoDeBusca() string {
	return strings.ToLower(strings.Join(
		[]string{i.Nome, i.Resumo, i.Descricao, i.Slug, i.URL}, " "))
}

// Catalogo é o instantâneo carregado, pronto para consulta.
//
// Imutável depois de construído: é compartilhado por todas as requisições sem
// trava, e nada aqui escreve depois de Carregar.
type Catalogo struct {
	itens   []Item
	termos  []string
	porSlug map[string]int
}

//go:embed catalogo.json
var catalogoEmbutido []byte

// Embutido é o catálogo que veio na build.
func Embutido() (*Catalogo, error) { return Carregar(catalogoEmbutido) }

// Vazio é o catálogo sem nenhum item.
//
// É o que a aplicação usa quando o instantâneo embutido não carrega: a tela
// fica vazia, e o gateway sobe. Uma tela de conveniência não derruba o
// processo.
func Vazio() *Catalogo {
	return &Catalogo{porSlug: map[string]int{}}
}

// Carregar constrói um catálogo a partir do JSON.
//
// Recusa item sem os campos que o cadastro precisa, em vez de aceitá-lo e
// deixar o admin descobrir na tela: um item sem URL é um botão "adicionar" que
// leva a um formulário vazio.
func Carregar(bruto []byte) (*Catalogo, error) {
	var itens []Item
	if err := json.Unmarshal(bruto, &itens); err != nil {
		return nil, fmt.Errorf("biblioteca: catálogo ilegível: %w", err)
	}
	c := &Catalogo{
		itens:   make([]Item, 0, len(itens)),
		termos:  make([]string, 0, len(itens)),
		porSlug: make(map[string]int, len(itens)),
	}
	for _, i := range itens {
		if err := i.validar(); err != nil {
			return nil, err
		}
		if _, repetido := c.porSlug[i.Slug]; repetido {
			return nil, fmt.Errorf("biblioteca: slug repetido: %q", i.Slug)
		}
		c.porSlug[i.Slug] = len(c.itens)
		c.itens = append(c.itens, i)
		c.termos = append(c.termos, i.termoDeBusca())
	}
	sort.SliceStable(c.itens, func(a, b int) bool {
		return strings.ToLower(c.itens[a].Nome) < strings.ToLower(c.itens[b].Nome)
	})
	// Os índices auxiliares são reconstruídos depois da ordenação: ordenar os
	// itens sem refazê-los deixaria porSlug apontando para o vizinho.
	c.termos = c.termos[:0]
	for n, i := range c.itens {
		c.porSlug[i.Slug] = n
		c.termos = append(c.termos, i.termoDeBusca())
	}
	return c, nil
}

func (i Item) validar() error {
	switch {
	case strings.TrimSpace(i.Slug) == "":
		return fmt.Errorf("biblioteca: item sem slug")
	case strings.TrimSpace(i.Nome) == "":
		return fmt.Errorf("biblioteca: item %q sem nome", i.Slug)
	case strings.TrimSpace(i.URL) == "":
		return fmt.Errorf("biblioteca: item %q sem URL", i.Slug)
	case i.Transporte != TransporteHTTP && i.Transporte != TransporteSSE:
		return fmt.Errorf("biblioteca: item %q com transporte inválido: %q", i.Slug, i.Transporte)
	case i.Autenticacao != AutOAuth && i.Autenticacao != AutToken && i.Autenticacao != AutAberta:
		return fmt.Errorf("biblioteca: item %q com autenticação inválida: %q", i.Slug, i.Autenticacao)
	}
	return nil
}

// Tamanho é quantos servidores o catálogo tem.
func (c *Catalogo) Tamanho() int { return len(c.itens) }

// Todos devolve o catálogo inteiro, já em ordem alfabética.
func (c *Catalogo) Todos() []Item { return c.itens }

// Item procura pelo slug.
func (c *Catalogo) Item(slug string) (Item, bool) {
	n, ok := c.porSlug[slug]
	if !ok {
		return Item{}, false
	}
	return c.itens[n], true
}

// Buscar filtra por termo livre, sem distinguir maiúscula de minúscula.
//
// Substring e não prefixo: quem procura "jira" precisa achar Atlassian, cujo
// resumo cita Jira e cujo nome não. Termo vazio devolve tudo, que é o estado
// inicial da tela.
func (c *Catalogo) Buscar(termo string) []Item {
	termo = strings.ToLower(strings.TrimSpace(termo))
	if termo == "" {
		return c.itens
	}
	campos := strings.Fields(termo)
	achados := make([]Item, 0, len(c.itens))
	for n, alvo := range c.termos {
		if contemTodos(alvo, campos) {
			achados = append(achados, c.itens[n])
		}
	}
	return achados
}

// contemTodos exige todos os pedaços do termo, em qualquer ordem.
//
// "notion pages" e "pages notion" acham o mesmo item; um único substring com o
// espaço dentro acharia só a primeira ordem.
func contemTodos(alvo string, campos []string) bool {
	for _, c := range campos {
		if !strings.Contains(alvo, c) {
			return false
		}
	}
	return true
}
