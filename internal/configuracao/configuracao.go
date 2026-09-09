// Package configuracao é o export e o import do YAML versionável do patchbay.
//
// O YAML é artefato de export/import e nunca é lido no boot (seção 08.9 do
// estudo prévio). É essa restrição que elimina a pergunta "quem ganha, o arquivo
// ou a UI": não existe configuração em vigor que a tela não mostre, e a
// divergência entre o arquivo e o banco é um diff que o dono pede quando quer —
// nunca um comportamento que entra em efeito sozinho.
//
// Duas consequências ficam visíveis no modelo deste pacote:
//
//   - Segredo nunca sai. O export escreve o slot da credencial e o nome da
//     variável de ambiente de onde o import pode lê-la; o valor não passa pelo
//     arquivo em nenhuma direção. Ausência de segredo no YAML significa "mantém
//     o que está gravado", nunca "apaga" — apagar é explícito, pelo `limpar`.
//   - O import mescla. O YAML carrega a revisão do estado de onde ele saiu, e a
//     trava otimista serve para detectar que o banco avançou, não para recusar o
//     arquivo: item que só o YAML mudou é aplicado, item que só o banco mudou
//     fica como está, e item que os dois mudaram é reportado como conflito sem
//     bloquear o resto (decisão do dono em 2026-09-08, seção 12).
//
// A serialização é gopkg.in/yaml.v3, e não koanf nem viper: eles resolvem
// precedência entre arquivo, ambiente e flag — a camada que a 08.9 elimina. Aqui
// não há sobreposição de fontes para resolver, só um documento a escrever e a
// ler, e o que o boot lê continua sendo uma dezena de variáveis de ambiente em
// cmd/patchbay/config.go. goccy/go-yaml faria o mesmo trabalho, mas yaml.v3 já
// está no grafo de módulos do projeto (dependência transitiva do goose), então
// adotá-lo não acrescenta uma linha ao go.sum.
package configuracao

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// VersaoAtual é a versão do schema do YAML.
//
// O formato é contrato do dono com o próprio dono no futuro (seção 10): ele
// aparece no arquivo, e um documento com versão que este binário não conhece é
// recusado com mensagem, nunca interpretado por adivinhação.
const VersaoAtual = 1

// Tipos de upstream aceitos no YAML.
//
// Repetidos aqui, e não importados de internal/upstream, porque feature não
// importa feature: o que atravessa a fronteira é o texto do tipo, que é contrato
// dos dois lados. sse existe no schema do banco e ainda não é suportado pela
// supervisão, então não entra na lista do que o import aceita cadastrar.
const (
	TipoHTTP  = "http"
	TipoSTDIO = "stdio"
)

// Tipos de slot de credencial de upstream. São os mesmos três nomes gravados na
// coluna tipo de upstream_secret.
const (
	SegredoBearer = "bearer"
	SegredoHeader = "header"
	SegredoEnv    = "env"
)

// Ações de regra de composição, na mesma grafia que internal/catalogo analisa.
const (
	RegraIncluir  = "incluir"
	RegraExcluir  = "excluir"
	RegraRenomear = "renomear"
)

// Erros sentinela do pacote. A borda traduz cada um em mensagem e código de
// saída; nenhum deles é decidido pelo texto.
var (
	// ErrYAMLInvalido indica documento que não dá para ler.
	ErrYAMLInvalido = errors.New("configuracao: yaml inválido")
	// ErrVersaoDesconhecida indica versão de schema que este binário não conhece.
	ErrVersaoDesconhecida = errors.New("configuracao: versão de schema desconhecida")
	// ErrNomeRepetido indica dois itens com a mesma identidade no mesmo documento.
	ErrNomeRepetido = errors.New("configuracao: identidade repetida no documento")
	// ErrSegredoEmClaro indica valor de credencial escrito no YAML em vez de uma
	// referência a variável de ambiente.
	ErrSegredoEmClaro = errors.New("configuracao: segredo em claro no yaml")
	// ErrAplicacaoParcial indica import em que algum item falhou. Os outros
	// continuam aplicados: cada item é uma transação própria.
	ErrAplicacaoParcial = errors.New("configuracao: import aplicado em parte")
)

// Documento é o YAML inteiro.
//
// A ordem dos campos é a ordem em que eles saem no arquivo, e ela é deliberada:
// versão e revisão primeiro, porque são o que alguém confere antes de editar
// qualquer outra coisa.
type Documento struct {
	Versao  int    `yaml:"versao"`
	Revisao string `yaml:"revisao"`

	Upstreams []Upstream `yaml:"upstreams,omitempty"`
	Endpoints []Endpoint `yaml:"endpoints,omitempty"`

	// ChavesAPI e ClientesOAuth saem no arquivo como registro do que existe, e
	// o import não os cadastra: os dois guardam credencial por hash, e emitir
	// uma chave nova a partir do YAML produziria um segredo que nenhum cliente
	// tem. O plano os classifica como informativo.
	ChavesAPI     []ChaveAPI     `yaml:"chaves_api,omitempty"`
	ClientesOAuth []ClienteOAuth `yaml:"clientes_oauth,omitempty"`
}

// Upstream é um servidor MCP upstream no YAML. A identidade é o nome.
type Upstream struct {
	Nome string `yaml:"nome"`
	// Revisao é o resumo do item como ele estava no banco na hora do export. É
	// a base da mescla de três vias: quem edita o item não mexe nela, e é essa
	// assimetria que permite distinguir "só o YAML mudou" de "só o banco mudou".
	Revisao string `yaml:"revisao,omitempty"`

	Tipo string `yaml:"tipo"`
	// URL só vale para http.
	URL string `yaml:"url,omitempty"`
	// Comando, Args e Env só valem para stdio. Env é a lista em claro: variável
	// sensível vai em Segredos, cifrada no banco e fora do arquivo.
	Comando string            `yaml:"comando,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`

	TimeoutMS  int64 `yaml:"timeout_ms"`
	Habilitado bool  `yaml:"habilitado"`

	// Sonda é a sonda de saúde funcional, quando há uma configurada. Nada aqui
	// é segredo: é a mesma configuração que a tela do upstream mostra.
	//
	// Ponteiro com omitempty, e o json:",omitempty" junto: o resumo do item é
	// um json.Marshal da struct normalizada, e um campo que serializasse como
	// null faria "o banco avançou" aparecer em todo upstream sem sonda no
	// primeiro import depois desta fatia. Nulo é "sem sonda", e um upstream sem
	// sonda resume exatamente como antes.
	Sonda *SondaDoUpstream `yaml:"sonda,omitempty" json:",omitempty"`

	// Segredos são os slots de credencial, sem valor nenhum. Não entram na
	// revisão do item de propósito: rotacionar um bearer não é o banco avançar
	// na configuração, e cada slot vira um item próprio do plano.
	Segredos []Segredo `yaml:"segredos,omitempty"`
}

// SondaDoUpstream é a sonda de saúde funcional no arquivo versionável.
//
// Args é o JSON literal e não um mapa aninhado, apesar de um mapa dar um YAML
// mais bonito: o valor precisa chegar ao tools/call byte a byte, e passar por
// mapa e voltar reordenaria as chaves — o que mudaria o resumo do item a cada
// export e faria a trava otimista acusar mudança que ninguém fez.
type SondaDoUpstream struct {
	Habilitada  bool   `yaml:"habilitada"`
	Ferramenta  string `yaml:"ferramenta"`
	Args        string `yaml:"args,omitempty"`
	Espera      string `yaml:"espera,omitempty"`
	IntervaloMS int64  `yaml:"intervalo_ms"`
	TimeoutMS   int64  `yaml:"timeout_ms"`
	Tolerancia  int    `yaml:"tolerancia"`
}

// Segredo é a referência a uma credencial estática de um upstream.
//
// O valor é sempre `${NOME_DA_VARIAVEL}` e nunca o segredo: o export escreve a
// referência canônica, e o import lê do ambiente do processo. Referência que não
// resolve não é erro — é "mantém o que está gravado", que é o estado normal de
// quem exportou num lugar e importou noutro sem carregar os segredos junto.
type Segredo struct {
	Tipo string `yaml:"tipo"`
	// Nome é o nome do header ou da variável de ambiente. Vazio no bearer, que é
	// um por upstream.
	Nome string `yaml:"nome,omitempty"`
	// Valor é a referência `${VAR}`. Valor literal é recusado: aceitar segredo em
	// claro aqui transformaria o arquivo de configuração em cofre, que é
	// exatamente o que a 08.9 evita.
	Valor string `yaml:"valor,omitempty"`
	// Limpar apaga a credencial gravada. É a única forma de apagar segredo pelo
	// import; ausência nunca apaga.
	Limpar bool `yaml:"limpar,omitempty"`
}

// Endpoint é um endpoint no YAML. A identidade é o slug, que é contrato: ele
// entra na URL, na metadata RFC 9728 e no aud de todo token daquele endpoint, e
// por isso o import nunca o renomeia — slug diferente é endpoint diferente.
type Endpoint struct {
	Slug    string `yaml:"slug"`
	Revisao string `yaml:"revisao,omitempty"`

	Nome       string `yaml:"nome"`
	Descricao  string `yaml:"descricao,omitempty"`
	Instrucoes string `yaml:"instrucoes,omitempty"`

	Upstreams []Vinculo `yaml:"upstreams,omitempty"`
}

// Vinculo é um upstream dentro da composição de um endpoint, com o prefixo e as
// regras que valem só ali.
type Vinculo struct {
	Nome    string  `yaml:"nome"`
	Prefixo string  `yaml:"prefixo,omitempty"`
	Regras  []Regra `yaml:"regras,omitempty"`
}

// Regra é uma linha da composição fina. A ordem da lista é a ordem de avaliação:
// a primeira incluir/excluir que casa decide se a ferramenta entra, e a primeira
// renomear que casa decide o nome-base.
type Regra struct {
	Acao   string `yaml:"acao"`
	Padrao string `yaml:"padrao"`
	Renome string `yaml:"renome,omitempty"`
}

// ChaveAPI é uma chave de API no YAML, por referência: nome, prefixo visível e
// escopo. O segredo é guardado por hash e não tem como voltar.
type ChaveAPI struct {
	Nome           string   `yaml:"nome"`
	PrefixoVisivel string   `yaml:"prefixo_visivel"`
	Endpoints      []string `yaml:"endpoints,omitempty"`
	Revogada       bool     `yaml:"revogada"`
}

// ClienteOAuth é um cliente do authorization server no YAML, por metadados. O
// segredo segue a mesma regra da chave de API.
type ClienteOAuth struct {
	ClientID     string   `yaml:"client_id"`
	Nome         string   `yaml:"nome"`
	Tipo         string   `yaml:"tipo"`
	Confidencial bool     `yaml:"confidencial"`
	RedirectURIs []string `yaml:"redirect_uris,omitempty"`
	Endpoints    []string `yaml:"endpoints,omitempty"`
	Revogado     bool     `yaml:"revogado"`
}

// normalizado devolve a forma canônica do upstream.
//
// A normalização roda nos dois lados de toda comparação — no item que veio do
// YAML e no item construído a partir do banco —, e é ela que faz o ida-e-volta
// fechar sem diferença: `args: []` e a ausência de args querem dizer a mesma
// coisa, e um deles precisa ganhar antes de qualquer resumo ser calculado.
//
// O lado irrelevante do transporte é zerado: um upstream http não tem comando
// para preservar, e deixar o campo passar faria o export mostrar configuração
// que o formulário da UI já não grava.
func (u Upstream) normalizado() Upstream {
	u.Nome = strings.TrimSpace(u.Nome)
	u.Tipo = strings.TrimSpace(u.Tipo)
	if u.Tipo == "" {
		u.Tipo = TipoHTTP
	}
	if u.Tipo == TipoSTDIO {
		u.URL = ""
		u.Comando = strings.TrimSpace(u.Comando)
	} else {
		u.URL = strings.TrimSpace(u.URL)
		u.Comando = ""
		u.Args = nil
		u.Env = nil
	}
	u.Args = semVazio(u.Args)
	u.Env = mapaSemVazio(u.Env)
	u.Segredos = segredosCanonicos(u.Segredos)
	u.Sonda = sondaCanonica(u.Sonda)
	return u
}

// sondaCanonica normaliza a sonda e devolve nulo quando ela não diz nada.
//
// Um bloco de sonda desligado e sem ferramenta é indistinguível de nenhum bloco,
// e escrever os dois de formas diferentes faria o mesmo banco produzir dois
// resumos.
func sondaCanonica(s *SondaDoUpstream) *SondaDoUpstream {
	if s == nil {
		return nil
	}
	n := *s
	n.Ferramenta = strings.TrimSpace(n.Ferramenta)
	n.Args = strings.TrimSpace(n.Args)
	n.Espera = strings.TrimSpace(n.Espera)
	if !n.Habilitada && n.Ferramenta == "" {
		return nil
	}
	return &n
}

// resumo é o identificador de conteúdo do upstream: o que a trava otimista
// compara. A revisão que o próprio item carrega sai da conta, e os segredos
// também — o resumo descreve a configuração, não a credencial.
func (u Upstream) resumo() string {
	n := u.normalizado()
	n.Revisao = ""
	n.Segredos = nil
	return resumoDe(n)
}

// normalizado devolve a forma canônica do endpoint.
//
// Nome vazio vira o slug: endpoint criado antes da UI (por `patchbay seed`, por
// exemplo) não tem nome, e exportá-lo com nome vazio produziria um YAML que o
// próprio import recusaria por falta de nome.
//
// Os vínculos saem em ordem de nome porque a ordem gravada é derivada do id do
// upstream, que é interno e não viaja no arquivo — ordenar por nome é a única
// ordem que o YAML consegue reproduzir.
func (e Endpoint) normalizado() Endpoint {
	e.Slug = strings.ToLower(strings.TrimSpace(e.Slug))
	e.Nome = strings.TrimSpace(e.Nome)
	if e.Nome == "" {
		e.Nome = e.Slug
	}
	vinculos := make([]Vinculo, 0, len(e.Upstreams))
	for _, v := range e.Upstreams {
		v.Nome = strings.TrimSpace(v.Nome)
		v.Prefixo = strings.TrimSpace(v.Prefixo)
		v.Regras = regrasCanonicas(v.Regras)
		vinculos = append(vinculos, v)
	}
	slices.SortFunc(vinculos, func(a, b Vinculo) int { return strings.Compare(a.Nome, b.Nome) })
	e.Upstreams = semVazio(vinculos)
	return e
}

// resumo é o identificador de conteúdo do endpoint.
func (e Endpoint) resumo() string {
	n := e.normalizado()
	n.Revisao = ""
	return resumoDe(n)
}

// Linha devolve a regra na forma canônica de uma linha de texto, que é a forma
// que internal/catalogo analisa: `<ação> <padrão> [renome]`.
func (r Regra) Linha() string {
	if r.Acao == RegraRenomear {
		return r.Acao + " " + r.Padrao + " " + r.Renome
	}
	return r.Acao + " " + r.Padrao
}

// TextoDeRegras devolve as regras de um vínculo como o textarea da UI as mostra,
// uma por linha. É o que o import entrega à feature de endpoint para ela
// analisar com o próprio analisador — o formato da linha é o contrato entre os
// dois pacotes, e reimplementar o analisador aqui faria os dois divergirem.
func TextoDeRegras(regras []Regra) string {
	var b strings.Builder
	for _, r := range regras {
		b.WriteString(r.Linha())
		b.WriteByte('\n')
	}
	return b.String()
}

// resumoDe calcula o identificador de conteúdo de um item.
//
// A serialização é JSON, e não YAML, por uma razão só: encoding/json documenta
// que ordena as chaves de mapa, e a estabilidade do resumo entre execuções é o
// que faz a trava otimista significar algo. O resumo é opaco — ninguém o lê como
// dado —, então o formato interno dele não precisa combinar com o do arquivo.
//
// Trunca em 16 bytes: 128 bits bastam para detectar edição, e um hexadecimal de
// 64 caracteres em toda linha de item deixaria o YAML ilegível.
func resumoDe(v any) string {
	bruto, err := json.Marshal(v)
	if err != nil {
		// Só acontece com tipo que não serializa, o que aqui é erro de
		// programação: todos os campos são string, número, bool, slice e mapa.
		// Um resumo que nunca casa é melhor que um panic no meio de um export.
		return "erro"
	}
	soma := sha256.Sum256(bruto)
	return "sha256:" + hex.EncodeToString(soma[:16])
}

// resumoDoDocumento resume as duas seções que o import aplica.
//
// Chave de API e cliente OAuth ficam fora: revogar uma chave faria "o banco
// avançou" aparecer num import que não tem nada a ver com chave nenhuma, e o
// aviso passaria a ser ruído.
func resumoDoDocumento(upstreams []Upstream, endpoints []Endpoint) string {
	type estado struct {
		Upstreams []Upstream
		Endpoints []Endpoint
	}
	e := estado{
		Upstreams: make([]Upstream, 0, len(upstreams)),
		Endpoints: make([]Endpoint, 0, len(endpoints)),
	}
	for _, u := range upstreams {
		n := u.normalizado()
		n.Revisao = ""
		n.Segredos = nil
		e.Upstreams = append(e.Upstreams, n)
	}
	for _, ep := range endpoints {
		n := ep.normalizado()
		n.Revisao = ""
		e.Endpoints = append(e.Endpoints, n)
	}
	slices.SortFunc(e.Upstreams, func(a, b Upstream) int { return strings.Compare(a.Nome, b.Nome) })
	slices.SortFunc(e.Endpoints, func(a, b Endpoint) int { return strings.Compare(a.Slug, b.Slug) })
	return resumoDe(e)
}

// segredosCanonicos ordena os slots e descarta linha sem tipo.
//
// Ordem estável por (tipo, nome) para que o arquivo não mude só porque as linhas
// saíram do banco em outra ordem.
func segredosCanonicos(segredos []Segredo) []Segredo {
	out := make([]Segredo, 0, len(segredos))
	for _, s := range segredos {
		s.Tipo = strings.TrimSpace(s.Tipo)
		s.Nome = strings.TrimSpace(s.Nome)
		s.Valor = strings.TrimSpace(s.Valor)
		if s.Tipo == "" {
			continue
		}
		if s.Tipo == SegredoBearer {
			s.Nome = ""
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Segredo) int {
		if c := strings.Compare(a.Tipo, b.Tipo); c != 0 {
			return c
		}
		return strings.Compare(a.Nome, b.Nome)
	})
	return semVazio(out)
}

// regrasCanonicas normaliza as regras sem reordená-las: a ordem é o que decide o
// resultado da composição, e ordenar aqui mudaria o catálogo em silêncio.
func regrasCanonicas(regras []Regra) []Regra {
	out := make([]Regra, 0, len(regras))
	for _, r := range regras {
		r.Acao = strings.TrimSpace(r.Acao)
		r.Padrao = strings.TrimSpace(r.Padrao)
		r.Renome = strings.TrimSpace(r.Renome)
		if r.Acao == "" && r.Padrao == "" {
			continue
		}
		if r.Acao != RegraRenomear {
			r.Renome = ""
		}
		out = append(out, r)
	}
	return semVazio(out)
}

// semVazio troca slice de tamanho zero por nil, para que "lista vazia" e
// "ausente" resumam igual.
func semVazio[T any](v []T) []T {
	if len(v) == 0 {
		return nil
	}
	return v
}

func mapaSemVazio(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// motivoDeUpstreamInvalido devolve, em texto para uma pessoa, por que o item não
// dá para aplicar — ou "" se a forma serve.
//
// É validação de forma, e não a validação de domínio: quem recusa URL sem host,
// comando com quebra de linha e nome de header inválido é o formulário da própria
// feature, pelo Conferir da porta. Duplicar a regra aqui faria as duas
// divergirem.
func motivoDeUpstreamInvalido(u Upstream) string {
	switch {
	case u.Nome == "":
		return "upstream sem nome"
	case u.Tipo != TipoHTTP && u.Tipo != TipoSTDIO:
		return fmt.Sprintf("tipo %q não é aceito no import; use %s ou %s", u.Tipo, TipoHTTP, TipoSTDIO)
	case u.TimeoutMS <= 0:
		return "timeout_ms precisa ser positivo"
	}
	for _, s := range u.Segredos {
		if motivo := motivoDeSegredoInvalido(s); motivo != "" {
			return motivo
		}
		// env em claro e segredo tipo env são gravados em colunas diferentes, e o
		// import escreve as duas independentes uma da outra. Sem esta recusa, o
		// mesmo nome nas duas listas gravaria a variável cifrada no slot de
		// credencial e, ao lado, uma cópia em claro na coluna de ambiente — que é
		// exatamente o segredo que a 08.9 promete nunca deixar sair em claro.
		if s.Tipo == SegredoEnv {
			if _, emClaro := u.Env[s.Nome]; emClaro {
				return "a variável " + s.Nome + " aparece em env (em claro) e em segredos (cifrada); " +
					"escolha um dos dois lugares"
			}
		}
	}
	return ""
}

// motivoDeSegredoInvalido recusa o slot que não dá para interpretar, e é aqui que
// segredo em claro é barrado.
func motivoDeSegredoInvalido(s Segredo) string {
	switch s.Tipo {
	case SegredoBearer:
	case SegredoHeader, SegredoEnv:
		if s.Nome == "" {
			return "segredo " + s.Tipo + " sem nome"
		}
	default:
		return fmt.Sprintf("tipo de segredo %q desconhecido; use %s, %s ou %s",
			s.Tipo, SegredoBearer, SegredoHeader, SegredoEnv)
	}
	if s.Valor == "" || ehReferencia(s.Valor) {
		return ""
	}
	return "o valor de " + rotuloDoSlot(s) + " precisa ser ${NOME_DA_VARIAVEL}; " +
		"segredo em claro não entra no YAML"
}

// motivoDeEndpointInvalido devolve por que o endpoint não dá para aplicar.
func motivoDeEndpointInvalido(e Endpoint) string {
	if e.Slug == "" {
		return "endpoint sem slug"
	}
	vistos := make(map[string]bool, len(e.Upstreams))
	for _, v := range e.Upstreams {
		switch {
		case v.Nome == "":
			return "vínculo sem nome de upstream"
		case vistos[v.Nome]:
			return "o upstream " + v.Nome + " aparece duas vezes na composição"
		}
		vistos[v.Nome] = true
		for i, r := range v.Regras {
			if motivo := motivoDeRegraInvalida(r); motivo != "" {
				return fmt.Sprintf("regra %d de %s: %s", i+1, v.Nome, motivo)
			}
		}
	}
	return ""
}

// motivoDeRegraInvalida recusa a regra que não sobreviveria à volta pela forma
// de texto — que é como ela atravessa a fronteira para internal/catalogo.
func motivoDeRegraInvalida(r Regra) string {
	switch r.Acao {
	case RegraIncluir, RegraExcluir:
		if r.Renome != "" {
			return r.Acao + " não leva renome"
		}
	case RegraRenomear:
		if r.Renome == "" {
			return "renomear exige renome"
		}
	default:
		return fmt.Sprintf("ação %q desconhecida; use %s, %s ou %s",
			r.Acao, RegraIncluir, RegraExcluir, RegraRenomear)
	}
	if r.Padrao == "" {
		return "regra sem padrão"
	}
	// O analisador de regras da composição separa os campos por espaço, então um
	// padrão com espaço dentro viraria duas regras — ou uma regra recusada — na
	// travessia. Recusar aqui diz onde está o problema.
	if temEspaco(r.Padrao) || temEspaco(r.Renome) {
		return "padrão e renome não podem ter espaço"
	}
	return ""
}

func temEspaco(s string) bool { return strings.ContainsAny(s, " \t\n\r") }

// rotuloDoSlot é como a credencial aparece no plano e na mensagem de erro: o
// slot, nunca o valor.
func rotuloDoSlot(s Segredo) string {
	if s.Nome == "" {
		return s.Tipo
	}
	return s.Tipo + "/" + s.Nome
}
