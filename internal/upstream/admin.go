package upstream

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
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
	ID      int64
	Nome    string
	Tipo    string
	URL     string
	Comando string
	Args    []string
	// Env são as variáveis não sensíveis. As sensíveis moram em upstream_secret
	// e não passam por aqui — Registro alimenta a tela.
	Env        map[string]string
	TimeoutMS  int64
	Habilitado bool
	UltimoErro string
	// Modo é como o patchbay se apresenta ao upstream: estatica ou oauth. Vazio
	// é estatica. Nenhum segredo passa por aqui — Registro alimenta a tela.
	Modo string
}

// STDIO informa se o upstream é um processo local.
func (r Registro) STDIO() bool { return r.Tipo == TipoSTDIO }

// ModoEfetivo normaliza o modo de credencial. Vazio é estatica.
func (r Registro) ModoEfetivo() string {
	if r.Modo == ModoOAuth {
		return ModoOAuth
	}
	return ModoEstatica
}

// UsaOAuth informa se o upstream se autentica por consentimento OAuth.
func (r Registro) UsaOAuth() bool {
	return r.ModoEfetivo() == ModoOAuth && (r.Tipo == TipoHTTP || r.Tipo == TipoSSE)
}

// Descricao é a linha de identificação do upstream na lista: a URL para HTTP, a
// linha de comando para STDIO.
func (r Registro) Descricao() string {
	if r.Tipo == TipoSTDIO {
		return LinhaDeComando(r.Comando, r.Args)
	}
	return r.URL
}

// Config traduz o registro para a configuração que o gerente supervisiona.
func (r Registro) Config() Config {
	return Config{
		ID:      r.ID,
		Nome:    r.Nome,
		Tipo:    r.Tipo,
		URL:     r.URL,
		Comando: r.Comando,
		Args:    r.Args,
		Env:     r.Env,
		Timeout: time.Duration(r.TimeoutMS) * time.Millisecond,
		Modo:    r.ModoEfetivo(),
	}
}

// LinhaDeComando junta comando e argumentos para exibição.
//
// Só para a tela: o processo recebe os argumentos como vetor, e reconstruir a
// linha nunca é o caminho de volta. Argumento com espaço aparece entre aspas
// para o admin não confundir dois argumentos com um.
func LinhaDeComando(comando string, args []string) string {
	partes := make([]string, 0, len(args)+1)
	partes = append(partes, comando)
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			a = strconv.Quote(a)
		}
		partes = append(partes, a)
	}
	return strings.Join(partes, " ")
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

// LinhasEnvEmBranco é quantas linhas vazias de variável sensível o formulário
// oferece além das já gravadas.
const LinhasEnvEmBranco = 2

// LimiteDeArgs e LimiteDeEnv recusam formulário absurdo antes de ele virar um
// bloco de argumentos ou de ambiente absurdo. Não é limite de produto: é o
// mesmo motivo pelo qual o timeout tem teto.
const (
	LimiteDeArgs = 64
	LimiteDeEnv  = 64
)

// CampoEnv é uma linha da tabela de variáveis de ambiente sensíveis.
//
// Mesma forma e mesma regra do CampoHeader: valor em branco mantém o gravado,
// apagar é explícito pelo Limpar. São dois tipos e não um porque o que a tela
// pede em cada um é diferente — nome de header é um token da RFC 9110, nome de
// variável é um identificador POSIX — e unificá-los faria a validação aceitar
// nos dois o que só vale num.
type CampoEnv struct {
	Nome     string
	Valor    cripto.Segredo
	Definido bool
	Limpar   bool
	Erro     string
}

// Form é o formulário de upstream, HTTP ou STDIO.
//
// Bearer, headers estáticos e variáveis de ambiente sensíveis são a classe de
// segredo que o patchbay apresenta: cifra reversível em repouso, e nunca de
// volta à tela. O que a tela mostra é "definido" ou "não definido", com a opção
// de trocar ou de limpar.
type Form struct {
	ID   int64
	Nome string
	// Tipo é http ou stdio. Vazio é http, para que o formulário antigo — e todo
	// teste que o monta sem tipo — continue significando o que significava.
	Tipo       string
	URL        string
	TimeoutMS  int64
	Habilitado bool

	// Comando é o programa do upstream stdio.
	Comando string
	// ArgsTexto é a caixa de texto com um argumento por linha, e Args é o que
	// a validação extraiu dela.
	//
	// Uma linha por argumento, e não uma linha de comando partida por espaço:
	// argumento com espaço dentro é normal — um caminho do Windows, um prompt —
	// e qualquer separador escolhido aqui viraria uma regra de escape que o
	// admin descobre errando.
	ArgsTexto string
	Args      []string
	// EnvTexto é a caixa NOME=valor por linha das variáveis não sensíveis, e
	// Env é o que a validação extraiu dela.
	EnvTexto string
	Env      map[string]string
	// EnvSecretos são as variáveis cifradas, uma linha por variável.
	EnvSecretos []CampoEnv

	// Bearer é o token novo. Vazio mantém o gravado.
	Bearer cripto.Segredo
	// BearerDefinido diz se já existe bearer gravado, para a tela dizer qual dos
	// dois estados é o atual sem revelar o valor.
	BearerDefinido bool
	// BearerLimpar apaga o bearer gravado.
	BearerLimpar bool

	Headers []CampoHeader

	// Modo é estatica ou oauth. Só vale para http e sse.
	Modo string
	// OAuthClientID é o client_id do cliente pré-registrado. Vazio deixa a ordem
	// do SDK cair em CIMD ou em registro dinâmico.
	//
	// Pré-registro é caminho de primeira classe e não recuperação de erro: o
	// Google não anuncia registration_endpoint nem CIMD, então sem este campo ele
	// simplesmente não funciona.
	OAuthClientID string
	// OAuthSegredo é o client_secret novo. Vazio mantém o gravado.
	OAuthSegredo cripto.Segredo
	// OAuthSegredoDefinido diz se há segredo gravado, sem revelá-lo.
	OAuthSegredoDefinido bool
	// OAuthSegredoLimpar apaga o segredo gravado, o que transforma o cliente em
	// público — é o certo para provedor que não emite segredo.
	OAuthSegredoLimpar bool
	// OAuthIssuer é o issuer do authorization server a que o cliente
	// pré-registrado pertence. Vazio desliga a conferência; preenchido, recusa
	// usar a credencial com outro AS (SEP-2352).
	OAuthIssuer string
	// OAuthDisponivel diz se este processo tem broker de OAuth. Falso esconde o
	// modo da tela: oferecer um fluxo que ninguém completaria é pior que não
	// oferecer.
	OAuthDisponivel bool
	// OAuthRedirectURI é o redirect_uri completo (URL pública mais o caminho do
	// callback) que a tela mostra para o admin colar no cadastro do cliente no
	// provedor. Só é preenchido quando OAuthDisponivel.
	OAuthRedirectURI string

	Erros map[string]string
}

// TipoEfetivo normaliza o tipo do formulário. Vazio é http.
func (f Form) TipoEfetivo() string {
	switch f.Tipo {
	case TipoSTDIO:
		return TipoSTDIO
	case TipoSSE:
		return TipoSSE
	default:
		return TipoHTTP
	}
}

// STDIO informa se o formulário descreve um processo local.
func (f Form) STDIO() bool { return f.TipoEfetivo() == TipoSTDIO }

// ModoEfetivo normaliza o modo de credencial. Vazio é estatica, e STDIO nunca
// tem modo: o processo filho recebe credencial por variável de ambiente, e um
// fluxo de redirect de navegador não tem onde encaixar ali.
func (f Form) ModoEfetivo() string {
	if f.Modo == ModoOAuth && !f.STDIO() {
		return ModoOAuth
	}
	return ModoEstatica
}

// UsaOAuth informa se o formulário descreve um upstream com consentimento OAuth.
func (f Form) UsaOAuth() bool { return f.ModoEfetivo() == ModoOAuth }

// Validar preenche Erros e informa se o formulário passa.
func (f *Form) Validar() bool {
	f.Erros = map[string]string{}
	f.Nome = strings.TrimSpace(f.Nome)
	f.Tipo = f.TipoEfetivo()

	if f.Nome == "" {
		f.Erros["nome"] = "Dê um nome ao upstream."
	}
	if f.Tipo == TipoSTDIO {
		f.validarProcesso()
	} else {
		f.validarURL()
	}
	if f.TimeoutMS < TimeoutMinimoMS || f.TimeoutMS > TimeoutMaximoMS {
		f.Erros["timeout_ms"] = "Use um valor entre 250 e 120000 milissegundos."
	}
	f.validarCredenciais()
	f.validarOAuth()
	return len(f.Erros) == 0
}

// validarOAuth recusa a combinação que produziria dois Authorization e o issuer
// que não é URL.
//
// Bearer estático junto com OAuth é o caso a barrar: os dois montam o mesmo
// header, o servidor escolhe um sem dizer qual, e o sintoma é 401 intermitente
// que ninguém liga a um formulário salvo semanas antes.
func (f *Form) validarOAuth() {
	f.OAuthClientID = strings.TrimSpace(f.OAuthClientID)
	f.OAuthIssuer = strings.TrimSpace(f.OAuthIssuer)

	if !f.UsaOAuth() {
		if f.Modo == ModoOAuth && f.STDIO() {
			f.Erros["modo"] = "Upstream STDIO não usa OAuth: a credencial dele vai por variável de ambiente."
		}
		return
	}

	if !f.Bearer.Vazio() || f.BearerDefinido && !f.BearerLimpar {
		f.Erros["modo"] = "No modo OAuth o Authorization vem do token. " +
			"Limpe o bearer estático antes de trocar de modo."
	}
	if f.OAuthClientID != "" && !ValorDeHeaderValido(f.OAuthClientID) {
		f.Erros["oauth_client_id"] = "O client_id não pode ter quebra de linha nem caractere de controle."
	}
	if !ValorDeHeaderValido(f.OAuthSegredo.Revelar()) {
		f.Erros["oauth_segredo"] = "O client_secret não pode ter quebra de linha nem caractere de controle."
	}
	if f.OAuthIssuer != "" {
		u, err := url.Parse(f.OAuthIssuer)
		if err != nil || u.Scheme == "" || u.Host == "" {
			f.Erros["oauth_issuer"] = "O issuer é uma URL, como https://accounts.google.com."
		}
	}
	if f.OAuthIssuer != "" && f.OAuthClientID == "" {
		f.Erros["oauth_issuer"] = "O issuer só vale com client_id pré-registrado: é ele que " +
			"a conferência protege."
	}
}

func (f *Form) validarURL() {
	f.URL = strings.TrimSpace(f.URL)
	vazia := "Informe a URL do endpoint MCP Streamable HTTP do servidor."
	if f.TipoEfetivo() == TipoSSE {
		vazia = "Informe a URL do endpoint SSE do servidor."
	}
	switch u, err := url.Parse(f.URL); {
	case f.URL == "":
		f.Erros["url"] = vazia
	case err != nil || u.Host == "":
		f.Erros["url"] = "URL inválida. Use algo como https://exemplo.com/mcp."
	case u.Scheme != "http" && u.Scheme != "https":
		f.Erros["url"] = "Só http e https são aceitos aqui."
	}
}

// validarProcesso recusa o que o supervisor não conseguiria lançar, e diz na
// tela qual campo está errado.
//
// A existência do executável não é conferida aqui de propósito: o PATH do
// processo patchbay pode mudar entre o salvar e o próximo restart, e recusar o
// cadastro por causa disso transformaria um erro de execução — que a tela já
// mostra como último erro, com backoff e reconexão — num erro de formulário que
// o admin não tem como resolver.
func (f *Form) validarProcesso() {
	f.Comando = strings.TrimSpace(f.Comando)
	if f.Comando == "" {
		f.Erros["comando"] = "Informe o programa a executar, por exemplo npx ou uvx."
	} else if strings.ContainsAny(f.Comando, "\x00\n\r") {
		f.Erros["comando"] = "O comando não pode ter quebra de linha nem caractere nulo."
	}

	f.Args = linhas(f.ArgsTexto)
	switch {
	case len(f.Args) > LimiteDeArgs:
		f.Erros["args"] = "São no máximo " + strconv.Itoa(LimiteDeArgs) + " argumentos."
		f.Args = nil
	case argComCaractereNulo(f.Args):
		// Mesma regra do comando e do valor de variável de ambiente: um vetor
		// de argumentos vai para exec.Command como está, sem shell no meio, e
		// o caractere nulo é o único jeito de um argumento confundir o exec
		// por baixo — o valor de env já recusa isto em lerParesEnv.
		f.Erros["args"] = "Nenhum argumento pode ter caractere nulo."
		f.Args = nil
	}

	env, err := lerParesEnv(f.EnvTexto)
	switch {
	case err != nil:
		f.Erros["env"] = err.Error()
	case len(env) > LimiteDeEnv:
		f.Erros["env"] = "São no máximo " + strconv.Itoa(LimiteDeEnv) + " variáveis."
	default:
		f.Env = env
	}
}

// linhas quebra uma caixa de texto em itens, uma linha por item, descartando as
// vazias. Não faz trim do conteúdo: um argumento pode legitimamente terminar em
// espaço, e comer isso em silêncio seria pior que o incômodo de ver a linha em
// branco sumir.
func linhas(texto string) []string {
	var out []string
	for _, linha := range strings.Split(texto, "\n") {
		linha = strings.TrimRight(linha, "\r")
		if strings.TrimSpace(linha) == "" {
			continue
		}
		out = append(out, linha)
	}
	return out
}

// lerParesEnv lê a caixa de NOME=valor por linha.
func lerParesEnv(texto string) (map[string]string, error) {
	out := map[string]string{}
	for _, linha := range linhas(texto) {
		nome, valor, ok := strings.Cut(linha, "=")
		nome = strings.TrimSpace(nome)
		_, repetido := out[nome]
		switch {
		case !ok:
			return nil, errors.New("Cada linha precisa ser NOME=valor. Faltou o = em: " + resumir(linha))
		case !NomeDeVariavelValido(nome):
			return nil, errors.New("Nome de variável inválido: " + resumir(nome) +
				". Use letras, dígitos e _, começando por letra ou _.")
		case strings.ContainsRune(valor, 0):
			return nil, errors.New("O valor de " + nome + " tem caractere nulo.")
		case repetido:
			return nil, errors.New("A variável " + nome + " aparece duas vezes.")
		}
		out[nome] = valor
	}
	return out, nil
}

// resumir corta texto de usuário antes de ele entrar numa mensagem de tela.
func resumir(s string) string {
	const limite = 40
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
	if len(s) > limite {
		return s[:limite] + "…"
	}
	return s
}

// argComCaractereNulo informa se algum argumento tem um \x00, que quebraria o
// vetor de argv do processo por baixo do exec.
func argComCaractereNulo(args []string) bool {
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return true
		}
	}
	return false
}

// NomeDeVariavelValido aceita o identificador de variável de ambiente do POSIX.
//
// Existe para que a UI recuse na hora: um nome com '=' ou espaço dentro produz
// um bloco de ambiente que o filho lê torto, e o sintoma aparece muito depois,
// como "a variável não chegou".
func NomeDeVariavelValido(nome string) bool {
	if nome == "" {
		return false
	}
	for i, r := range nome {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
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
	f.validarEnvSecretos()
}

// validarEnvSecretos recusa nome de variável inválido e repetição.
//
// Roda para qualquer tipo, e não só para stdio: um upstream que já teve
// variáveis gravadas e foi trocado para HTTP continua com as linhas na tela, e
// deixá-las passar sem validação seria gravar lixo que só apareceria no dia em
// que ele voltasse a ser stdio.
func (f *Form) validarEnvSecretos() {
	vistos := make(map[string]bool, len(f.EnvSecretos))
	comErro := false
	for i := range f.EnvSecretos {
		e := &f.EnvSecretos[i]
		e.Nome = strings.TrimSpace(e.Nome)
		e.Erro = ""

		switch {
		case e.Nome == "" && e.Valor.Vazio():
			// Linha em branco: o formulário sempre oferece algumas.
			continue
		case e.Nome == "":
			e.Erro = "Informe o nome da variável."
		case !NomeDeVariavelValido(e.Nome):
			e.Erro = "Nome inválido. Use letras, dígitos e _, começando por letra ou _."
		case vistos[e.Nome]:
			e.Erro = "Esta variável já aparece acima."
		case f.Env[e.Nome] != "":
			e.Erro = "Esta variável também está na lista em claro acima. Deixe-a só num lugar."
		case strings.ContainsRune(e.Valor.Revelar(), 0):
			e.Erro = "O valor não pode ter caractere nulo."
		}
		if e.Erro != "" {
			comErro = true
			continue
		}
		vistos[e.Nome] = true
	}
	if comErro {
		f.Erros["env_secreto"] = "Corrija as variáveis marcadas abaixo."
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

// CompletarCredenciais preenche as linhas de header e de variável sensível a
// partir do que está gravado. É o que a borda HTTP chama antes de desenhar o
// formulário.
func (f *Form) CompletarCredenciais(definidas []CredencialDefinida) {
	f.CompletarHeaders(definidas)
	f.CompletarEnvSecretos(definidas)
}

// CompletarEnvSecretos acrescenta ao formulário as variáveis sensíveis já
// gravadas e as linhas em branco para as novas.
//
// Mesma regra do CompletarHeaders: o que o admin digitou tem prioridade sobre o
// que veio do banco, porque esta função também roda ao reexibir um formulário
// recusado pela validação.
func (f *Form) CompletarEnvSecretos(definidas []CredencialDefinida) {
	digitados := make(map[string]bool, len(f.EnvSecretos))
	for _, e := range f.EnvSecretos {
		if e.Nome != "" {
			digitados[e.Nome] = true
		}
	}

	existentes := make([]CampoEnv, 0, len(definidas))
	for _, d := range definidas {
		if d.Tipo == CredencialEnv && !digitados[d.Nome] {
			existentes = append(existentes, CampoEnv{Nome: d.Nome, Definido: true})
		}
	}
	for i := range f.EnvSecretos {
		for _, d := range definidas {
			if d.Tipo == CredencialEnv && d.Nome == f.EnvSecretos[i].Nome {
				f.EnvSecretos[i].Definido = true
			}
		}
	}
	f.EnvSecretos = append(existentes, f.EnvSecretos...)

	emBranco := 0
	for _, e := range f.EnvSecretos {
		if e.Nome == "" {
			emBranco++
		}
	}
	for ; emBranco < LinhasEnvEmBranco; emBranco++ {
		f.EnvSecretos = append(f.EnvSecretos, CampoEnv{})
	}
}

// CompletarOAuth traz para o formulário o que está gravado de OAuth.
//
// Recebe o estado em vez de ler do banco: o formulário é dado, e quem consulta é
// a borda HTTP. O que o admin digitou tem prioridade — esta função também roda ao
// reexibir um formulário recusado pela validação.
func (f *Form) CompletarOAuth(e EstadoOAuth) {
	f.OAuthSegredoDefinido = e.SegredoDefinido
	if f.OAuthClientID == "" {
		f.OAuthClientID = e.ClientID
	}
	if f.OAuthIssuer == "" {
		f.OAuthIssuer = e.Issuer
	}
}

// TextoDeArgs e TextoDeEnv devolvem o que a caixa de texto do formulário mostra
// ao editar um upstream existente.
func TextoDeArgs(args []string) string { return strings.Join(args, "\n") }

// TextoDeEnv ordena as variáveis por nome: a caixa de texto é reexibida a cada
// edição, e uma ordem que muda sozinha faz o admin achar que alguém mexeu.
func TextoDeEnv(env map[string]string) string {
	nomes := make([]string, 0, len(env))
	for nome := range env {
		nomes = append(nomes, nome)
	}
	sort.Strings(nomes)

	var b strings.Builder
	for i, nome := range nomes {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(nome)
		b.WriteByte('=')
		b.WriteString(env[nome])
	}
	return b.String()
}

// Linha é um upstream na lista da UI.
type Linha struct {
	Registro
	Estado          Estado
	Ferramentas     int
	TentativaEm     time.Time
	ProximaEm       time.Time
	Abandonos       int
	AbandonosTotais int
	Motivo          string
	Endpoints       int
	Supervisionado  bool
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
	// ProximaEm é quando o backoff libera a próxima tentativa. Sem ela na tela,
	// o admin fica clicando em reconectar sem saber que já está agendado.
	ProximaEm time.Time
	// Falhas, Abandonos e Motivo são o resíduo visível da mitigação da issue
	// #1189: cada abandono deixa duas goroutines presas até o próximo boot.
	Falhas int
	// Abandonos é o consecutivo desde o último pronto — o que TetoAbandonos
	// mede — e AbandonosTotais é o acumulado que não zera sozinho.
	Abandonos       int
	AbandonosTotais int
	TetoAbandonos   int
	Motivo          string
	Ferramentas     []FerramentaDescoberta
	Endpoints       []string
	// Credenciais lista o que está gravado, sem valor nenhum.
	Credenciais []CredencialDefinida
	// OAuth é o que a tela mostra do consentimento, sem nenhum segredo e sem
	// decifrar nada.
	OAuth EstadoOAuth
}

// UsaOAuth informa se o upstream se autentica por consentimento OAuth.
func (d Detalhe) UsaOAuth() bool { return d.Registro.UsaOAuth() }

// PodeAutorizar informa se o botão "Autorizar" faz sentido agora.
//
// Habilitado é pré-requisito: um upstream fora da supervisão não tem quem monte
// a URL de autorização, e um botão que não faz nada é pior que nenhum botão.
func (d Detalhe) PodeAutorizar() bool { return d.UsaOAuth() && d.Habilitado }

// RotuloDoRegistro é como o registro de cliente aparece na tela.
func (d Detalhe) RotuloDoRegistro() string { return RotuloDoRegistro(d.OAuth.Registro) }

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

// VariaveisSecretas devolve só os nomes das variáveis de ambiente cifradas.
func (d Detalhe) VariaveisSecretas() []CredencialDefinida {
	var out []CredencialDefinida
	for _, c := range d.Credenciais {
		if c.Tipo == CredencialEnv {
			out = append(out, c)
		}
	}
	return out
}

// VariaveisEmClaro devolve as variáveis não sensíveis em ordem de nome, para a
// tela de detalhe.
func (d Detalhe) VariaveisEmClaro() []ParEnv {
	out := make([]ParEnv, 0, len(d.Env))
	for nome, valor := range d.Env {
		out = append(out, ParEnv{Nome: nome, Valor: valor})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nome < out[j].Nome })
	return out
}

// ParEnv é uma variável de ambiente não sensível, do jeito que a tela a mostra.
type ParEnv struct {
	Nome  string
	Valor string
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
