package endpoint

import (
	"context"
	"regexp"
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

	// SlugFixo marca a edição: o slug aparece como texto, não como campo.
	SlugFixo bool
	Erros    map[string]string
}

// Validar preenche Erros e informa se o formulário passa.
func (f *Form) Validar() bool {
	f.Erros = map[string]string{}

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
	return len(f.Erros) == 0
}

// UpstreamOpcao é um upstream oferecido na composição de um endpoint.
//
// O estado e a contagem de ferramentas aparecem na lista porque compor um
// endpoint com um upstream degradado é uma decisão diferente de compor com um
// pronto — e a contagem é o custo de contexto que o cliente vai pagar (seção 11).
type UpstreamOpcao struct {
	ID          int64
	Nome        string
	Habilitado  bool
	Estado      string
	Ferramentas int
	Escolhido   bool
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
	// URL é o endereço público que o cliente MCP usa.
	URL string
}

// Detalhe é a tela de um endpoint.
type Detalhe struct {
	Registro
	URL         string
	Ferramentas []string
	Composicao  []UpstreamOpcao
}
