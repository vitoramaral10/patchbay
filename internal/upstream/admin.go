package upstream

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// Form é o formulário de upstream HTTP.
//
// Não tem campo de bearer nem de header estático: esses são segredos que o
// patchbay apresenta, precisam de cifra reversível em repouso, e a cifra é a
// fatia 6. Um campo de senha gravado em claro "só por enquanto" é exatamente o
// erro que a seção 08.8 separa em duas classes.
type Form struct {
	ID         int64
	Nome       string
	URL        string
	TimeoutMS  int64
	Habilitado bool
	Erros      map[string]string
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
	return len(f.Erros) == 0
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
