package upstream

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// TimeoutPadraoMS é o timeout sugerido no formulário de upstream.
//
// Todo timeout aparece no formulário com o valor em vigor (seção 11): timeout
// invisível é o que transforma "está lento" numa investigação de meia hora.
const TimeoutPadraoMS int64 = 15000

// Limites do timeout que a UI aceita. O teto existe porque um timeout maior que
// isso deixa de ser timeout: a supervisão fica presa numa tentativa só.
const (
	TimeoutMinimoMS int64 = 250
	TimeoutMaximoMS int64 = 120000
)

// Registro é o upstream como ele está no banco.
type Registro struct {
	ID         int64
	Nome       string
	Tipo       string
	URL        string
	TimeoutMS  int64
	Habilitado bool
	UltimoErro string
}

// Config traduz o registro para a configuração que o gerente supervisiona.
func (r Registro) Config() Config {
	return Config{
		ID:      r.ID,
		Nome:    r.Nome,
		Tipo:    r.Tipo,
		URL:     r.URL,
		Timeout: time.Duration(r.TimeoutMS) * time.Millisecond,
	}
}

// LinhasHeaderEmBranco é quantas linhas vazias de header estático o formulário
// oferece além das já gravadas.
const LinhasHeaderEmBranco = 2

// CampoHeader é uma linha da tabela de headers estáticos no formulário.
//
// Valor em branco significa "manter o que está gravado", nunca "apagar": o
// formulário não reexibe segredo, então um campo vazio é o estado normal de
// quem só veio mudar o timeout. Apagar é explícito, pelo Limpar.
type CampoHeader struct {
	Nome     string
	Valor    cripto.Segredo
	Definido bool
	Limpar   bool
	Erro     string
}

// Form é o formulário de upstream HTTP.
//
// Bearer e headers estáticos são a classe de segredo que o patchbay apresenta:
// cifra reversível em repouso, e nunca de volta à tela. O que a tela mostra é
// "definido" ou "não definido", com a opção de trocar ou de limpar.
type Form struct {
	ID         int64
	Nome       string
	URL        string
	TimeoutMS  int64
	Habilitado bool

	// Bearer é o token novo. Vazio mantém o gravado.
	Bearer cripto.Segredo
	// BearerDefinido diz se já existe bearer gravado, para a tela dizer qual dos
	// dois estados é o atual sem revelar o valor.
	BearerDefinido bool
	// BearerLimpar apaga o bearer gravado.
	BearerLimpar bool

	Headers []CampoHeader

	Erros map[string]string
}

// Validar preenche Erros e informa se o formulário passa.
func (f *Form) Validar() bool {
	f.Erros = map[string]string{}
	f.Nome = strings.TrimSpace(f.Nome)
	f.URL = strings.TrimSpace(f.URL)

	if f.Nome == "" {
		f.Erros["nome"] = "Dê um nome ao upstream."
	}
	switch u, err := url.Parse(f.URL); {
	case f.URL == "":
		f.Erros["url"] = "Informe a URL do endpoint MCP Streamable HTTP do servidor."
	case err != nil || u.Host == "":
		f.Erros["url"] = "URL inválida. Use algo como https://exemplo.com/mcp."
	case u.Scheme != "http" && u.Scheme != "https":
		f.Erros["url"] = "Só http e https são aceitos aqui."
	}
	if f.TimeoutMS < TimeoutMinimoMS || f.TimeoutMS > TimeoutMaximoMS {
		f.Erros["timeout_ms"] = "Use um valor entre 250 e 120000 milissegundos."
	}
	f.validarCredenciais()
	return len(f.Erros) == 0
}

// validarCredenciais recusa o que viraria requisição malformada ou header
// injetado, e diz na tela qual linha está errada.
func (f *Form) validarCredenciais() {
	if !ValorDeHeaderValido(f.Bearer.Revelar()) || strings.ContainsAny(f.Bearer.Revelar(), " \t") {
		f.Erros["bearer"] = "O token não pode ter espaço, quebra de linha nem caractere de controle."
	}

	vistos := make(map[string]bool, len(f.Headers))
	comErro := false
	for i := range f.Headers {
		h := &f.Headers[i]
		h.Nome = strings.TrimSpace(h.Nome)
		h.Erro = ""

		switch {
		case h.Nome == "" && h.Valor.Vazio():
			// Linha em branco: o formulário sempre oferece algumas.
			continue
		case h.Nome == "":
			h.Erro = "Informe o nome do header."
		case !NomeDeHeaderValido(h.Nome):
			h.Erro = "Nome de header inválido. Use letras, dígitos e - _ . como em X-Api-Key."
		case strings.EqualFold(h.Nome, "authorization"):
			h.Erro = "Authorization é montado pelo campo de bearer acima."
		case vistos[strings.ToLower(h.Nome)]:
			h.Erro = "Este header já aparece acima."
		case !ValorDeHeaderValido(h.Valor.Revelar()):
			h.Erro = "O valor não pode ter quebra de linha nem caractere de controle."
		}
		if h.Erro != "" {
			comErro = true
			continue
		}
		vistos[strings.ToLower(h.Nome)] = true
	}
	if comErro {
		f.Erros["headers"] = "Corrija os headers marcados abaixo."
	}
}

// CompletarHeaders acrescenta ao formulário as linhas dos headers já gravados e
// as linhas em branco para os novos.
//
// Recebe o que está no banco em vez de ler dele: o formulário é dado, e quem
// consulta é a borda HTTP.
func (f *Form) CompletarHeaders(definidas []CredencialDefinida) {
	// O que o admin digitou tem prioridade sobre o que veio do banco: esta
	// função também roda ao reexibir um formulário recusado pela validação.
	digitados := make(map[string]bool, len(f.Headers))
	for _, h := range f.Headers {
		if h.Nome != "" {
			digitados[strings.ToLower(h.Nome)] = true
		}
	}

	existentes := make([]CampoHeader, 0, len(definidas))
	for _, d := range definidas {
		switch {
		case d.Tipo == CredencialBearer:
			f.BearerDefinido = true
		case d.Tipo == CredencialHeader && !digitados[strings.ToLower(d.Nome)]:
			existentes = append(existentes, CampoHeader{Nome: d.Nome, Definido: true})
		}
	}

	for i := range f.Headers {
		for _, d := range definidas {
			if d.Tipo == CredencialHeader && strings.EqualFold(d.Nome, f.Headers[i].Nome) {
				f.Headers[i].Definido = true
			}
		}
	}
	f.Headers = append(existentes, f.Headers...)

	emBranco := 0
	for _, h := range f.Headers {
		if h.Nome == "" {
			emBranco++
		}
	}
	for ; emBranco < LinhasHeaderEmBranco; emBranco++ {
		f.Headers = append(f.Headers, CampoHeader{})
	}
}

// Linha é um upstream na lista da UI.
type Linha struct {
	Registro
	Estado         Estado
	Ferramentas    int
	TentativaEm    time.Time
	Endpoints      int
	Supervisionado bool
}

// FerramentaDescoberta é uma ferramenta do último tools/list do upstream, do
// jeito que a tela de detalhe a mostra.
//
// Nome exposto e nome original aparecem juntos porque o normalizador pode ter
// mudado o primeiro: normalização silenciosa é indistinguível de servidor
// quebrado (seção 08.2).
type FerramentaDescoberta struct {
	NomeExposto  string
	NomeOriginal string
	Descricao    string
	Avisos       []string
}

// Detalhe é a tela de um upstream.
type Detalhe struct {
	Registro
	Estado         Estado
	Supervisionado bool
	TentativaEm    time.Time
	Ferramentas    []FerramentaDescoberta
	Endpoints      []string
	// Credenciais lista o que está gravado, sem valor nenhum.
	Credenciais []CredencialDefinida
}

// TemBearer informa se há bearer gravado.
func (d Detalhe) TemBearer() bool {
	for _, c := range d.Credenciais {
		if c.Tipo == CredencialBearer {
			return true
		}
	}
	return false
}

// HeadersEstaticos devolve só os headers estáticos gravados.
func (d Detalhe) HeadersEstaticos() []CredencialDefinida {
	var out []CredencialDefinida
	for _, c := range d.Credenciais {
		if c.Tipo == CredencialHeader {
			out = append(out, c)
		}
	}
	return out
}

// NomeExpostoDe traduz uma ferramenta bruta no nome que o cliente veria, com os
// avisos do que a normalização mudou.
//
// Vem de fora porque normalizar é assunto do catálogo, e feature não importa
// feature. O prefixo por composição é da fatia 4: aqui o nome é o do upstream
// isolado, que é o que a tela de detalhe do upstream tem para mostrar.
type NomeExpostoDe func(t *mcp.Tool) (nome string, avisos []string)

// Rematerializar é o que o CRUD chama depois de mudar um upstream, para que os
// endpoints que o incluem passem a refletir a mudança sem reiniciar o processo.
type Rematerializar func(ctx context.Context) error
