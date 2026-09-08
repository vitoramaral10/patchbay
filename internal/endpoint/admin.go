package endpoint

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

// slugValido é a forma do slug: minúsculas, dígitos e hífen, começando e
// terminando em alfanumérico, até 40 caracteres.
//
// Restritivo porque o slug entra em URL, no path da metadata RFC 9728 e no
// resource do token: qualquer caractere que precise de escape em um desses três
// lugares vira bug de credencial meses depois.
var slugValido = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)

// Form é o formulário de endpoint, com os erros de validação por campo para que
// a tela reexiba o que a pessoa digitou em vez de limpar tudo.
type Form struct {
	// ID é zero na criação; na edição é o endpoint que o formulário grava.
	ID          int64
	Slug        string
	Nome        string
	Descricao   string
	Instrucoes  string
	UpstreamIDs []int64

	// Prefixos e RegrasTexto são a composição fina, indexadas pelo id do
	// upstream. Ficam como texto até Validar: o que a tela reexibe depois de um
	// erro precisa ser o que a pessoa escreveu, não a interpretação dele.
	Prefixos    map[int64]string
	RegrasTexto map[int64]string

	// SlugFixo marca a edição: o slug aparece como texto, não como campo.
	SlugFixo bool
	Erros    map[string]string

	// regras é o resultado de analisar RegrasTexto, preenchido por Validar. Não
	// exportado porque só existe depois da validação: quem grava passa pelo
	// Regras, e formulário não validado não tem regra nenhuma para gravar.
	regras map[int64][]catalogo.Regra
}

// ChavePrefixo e ChaveRegras são os nomes dos campos de composição de um
// upstream no formulário — e também as chaves de Erros daquele upstream.
func ChavePrefixo(upstreamID int64) string {
	return "prefixo_" + strconv.FormatInt(upstreamID, 10)
}

// ChaveRegras é o nome do textarea de regras de um upstream.
func ChaveRegras(upstreamID int64) string {
	return "regras_" + strconv.FormatInt(upstreamID, 10)
}

// Prefixo devolve o prefixo escolhido para um upstream.
func (f Form) Prefixo(upstreamID int64) string { return f.Prefixos[upstreamID] }

// Regras devolve as regras já analisadas de um upstream. Só tem conteúdo depois
// de Validar passar.
func (f Form) Regras(upstreamID int64) []catalogo.Regra { return f.regras[upstreamID] }

// RegrasDe devolve o texto cru das regras de um upstream, para a tela reexibir.
func (f Form) RegrasDe(upstreamID int64) string { return f.RegrasTexto[upstreamID] }

// Validar preenche Erros e informa se o formulário passa.
//
// A composição é validada só para os upstreams marcados: prefixo digitado num
// upstream que a pessoa desmarcou não é erro, é sobra de formulário.
func (f *Form) Validar() bool {
	f.Erros = map[string]string{}
	f.regras = map[int64][]catalogo.Regra{}

	if !f.SlugFixo {
		f.Slug = NormalizarSlug(f.Slug)
		switch {
		case f.Slug == "":
			f.Erros["slug"] = "Escolha um slug: ele é o que vai na URL do endpoint."
		case !slugValido.MatchString(f.Slug):
			f.Erros["slug"] = "Use minúsculas, números e hífen, começando e terminando com letra ou número."
		}
	}
	if f.Nome == "" {
		f.Erros["nome"] = "Dê um nome ao endpoint."
	}
	for _, id := range f.UpstreamIDs {
		f.validarComposicao(id)
	}
	return len(f.Erros) == 0
}

func (f *Form) validarComposicao(upstreamID int64) {
	prefixo := f.Prefixos[upstreamID]
	if !catalogo.PrefixoValido(prefixo) {
		f.Erros[ChavePrefixo(upstreamID)] = fmt.Sprintf(
			"Use só letras, números, %q, %q e %q, com até %d caracteres — o prefixo entra no nome que o cliente vê.",
			"_", "-", ".", catalogo.MaxPrefixo)
	}

	regras, err := catalogo.AnalisarRegras(f.RegrasTexto[upstreamID])
	if err != nil {
		var erroDeRegra *catalogo.ErroDeRegra
		if errors.As(err, &erroDeRegra) {
			f.Erros[ChaveRegras(upstreamID)] = erroDeRegra.Mensagem()
			return
		}
		f.Erros[ChaveRegras(upstreamID)] = "Não foi possível ler as regras."
		return
	}
	if len(regras) > catalogo.MaxRegras {
		f.Erros[ChaveRegras(upstreamID)] = fmt.Sprintf(
			"No máximo %d regras por upstream neste endpoint; esta lista tem %d.",
			catalogo.MaxRegras, len(regras))
		return
	}
	f.regras[upstreamID] = regras
}

// UpstreamOpcao é um upstream oferecido na composição de um endpoint.
//
// O estado e a contagem de ferramentas aparecem na lista porque compor um
// endpoint com um upstream degradado é uma decisão diferente de compor com um
// pronto — e a contagem é o custo de contexto que o cliente vai pagar (seção 11).
type UpstreamOpcao struct {
	ID         int64
	Nome       string
	Habilitado bool
	Estado     string
	// Ferramentas é quantas o upstream expõe, antes de qualquer composição.
	Ferramentas int
	Escolhido   bool

	// NomesOriginais é o nome de cada ferramenta que o upstream expõe agora,
	// antes de qualquer composição — o catálogo vivo contra o qual as regras
	// casam. É o que permite avisar de uma regra escrita contra um nome que o
	// upstream não tem, como uma regra escrita contra o nome já prefixado.
	NomesOriginais []string

	// NoEndpoint é quantas ferramentas deste upstream sobrevivem à composição
	// deste endpoint. É o número que a seção 11 exige ao lado de cada upstream
	// dentro do endpoint: sem ele, um filtro que apagou tudo é indistinguível de
	// um upstream que não conectou.
	NoEndpoint int
	// Prefixo e Regras são a composição fina deste upstream neste endpoint, como
	// texto para o formulário reexibir.
	Prefixo string
	Regras  string
	// ErroPrefixo e ErroRegras são a validação daquele upstream: bloqueiam salvar.
	ErroPrefixo string
	ErroRegras  string
	// AvisoRegras é o aviso não bloqueante de que alguma regra deste upstream
	// não casou nenhuma ferramenta do catálogo vivo — por exemplo, uma regra
	// escrita contra o nome já prefixado. Não impede salvar: o upstream pode
	// reconectar depois com um catálogo diferente em que a regra passa a casar.
	AvisoRegras string
}

// Upstreams é o que a tela de composição precisa saber dos upstreams.
//
// Declarada aqui, no consumidor, e implementada fora: quem é dono de upstream é
// outra feature, e feature não importa feature — só main conhece o grafo.
type Upstreams interface {
	Opcoes(ctx context.Context) ([]UpstreamOpcao, error)
}

// Linha é um endpoint na lista da UI.
type Linha struct {
	Registro
	Upstreams   int
	Ferramentas int
	// Lapides é quantas ferramentas removidas ainda respondem explicando que
	// saíram (seção 08.3). Contagem não inclui lápide, mas elas também custam
	// contexto no cliente, e por isso aparecem ao lado na tela.
	Lapides int
	// URL é o endereço público que o cliente MCP usa.
	URL string
}

// Detalhe é a tela de um endpoint.
type Detalhe struct {
	Registro
	URL         string
	Ferramentas []string
	// Lapides são os nomes das ferramentas removidas que ainda respondem só
	// para explicar que saíram, até a janela de graça vencer.
	Lapides    []string
	Composicao []UpstreamOpcao
}
