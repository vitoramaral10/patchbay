package upstream

import (
	"errors"
	"strconv"
	"strings"
	"unicode"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// A leitura do comando de instalação que a documentação dos servidores publica.
//
// Quase todo servidor MCP hoje documenta a instalação como uma linha de
// `claude mcp add`, e transcrever aquela linha para o formulário é trabalho
// mecânico com quatro chances de errar: o transporte, a URL, o nome do header e
// o token. Este arquivo faz a transcrição.
//
// O resultado é um Form e não uma escrita no banco: colar é a entrada, não a
// decisão. Quem grava continua sendo o mesmo caminho do formulário — a mesma
// validação, a mesma cifra de credencial, o mesmo hot-apply —, e é por isso que
// nada aqui fala com o repositório. Um comando colado que criasse o upstream
// direto seria, no caso STDIO, um processo local lançado a partir do clipboard
// sem ninguém ler o que ele executa.

// LimiteDoComando é o teto do texto colado.
//
// Existe pelo mesmo motivo do teto do YAML: o handler lê o corpo inteiro em
// memória antes de analisar. Nenhum comando de instalação real chega perto
// disso — o maior que este projeto viu tem menos de 400 bytes.
const LimiteDoComando = 16 << 10

// LimiteDeHeaders é quantos --header um comando pode trazer. É o mesmo espírito
// do LimiteDeArgs: recusar o absurdo antes de ele virar linha de formulário.
const LimiteDeHeaders = 32

// Importacao é o que o patchbay entendeu de um comando de instalação.
//
// Escopo e Avisos existem para a tela de conferência: o comando traz coisas que
// o patchbay não tem onde guardar (o escopo do Claude Code é uma delas), e
// descartá-las em silêncio faria a tela mentir por omissão sobre o que foi lido.
type Importacao struct {
	// Form é o formulário preenchido, pronto para Validar e Criar.
	Form Form
	// Escopo é o --scope do comando, quando havia um. Não vira nada: o patchbay
	// não tem escopo de instalação.
	Escopo string
	// Avisos são as diferenças entre o que o comando dizia e o que o patchbay
	// vai gravar, em texto de tela.
	Avisos []string
}

// LerComandoDeInstalacao traduz um `claude mcp add ...` colado num formulário de
// upstream.
//
// O erro devolvido é texto de tela, não diagnóstico de programador: quem cola um
// comando errado precisa saber qual pedaço corrigir, e "unexpected token" não
// diz isso. Nenhum erro daqui repete o valor de uma credencial — a mensagem
// nomeia o header, nunca o token.
func LerComandoDeInstalacao(texto string) (Importacao, error) {
	tokens, err := tokenizarComando(texto)
	if err != nil {
		return Importacao{}, err
	}
	tokens, err = pularPrefixoDoComando(tokens)
	if err != nil {
		return Importacao{}, err
	}
	return montarImportacao(tokens)
}

// pularPrefixoDoComando descarta o que vem antes do `add` e confere que o
// comando é mesmo o de adicionar servidor.
//
// Aceita a linha com ou sem o executável na frente, porque quem copia de uma
// documentação às vezes traz o `$` do prompt junto e às vezes copia só a partir
// do `mcp`. O que não é aceito é adivinhar: um `claude mcp remove` colado por
// engano precisa ser recusado, não interpretado como adição.
func pularPrefixoDoComando(tokens []string) ([]string, error) {
	for i, t := range tokens {
		switch t {
		case "add":
			return tokens[i+1:], nil
		case "add-json":
			return nil, errors.New("O formato `claude mcp add-json` ainda não é lido aqui. " +
				"Use a linha `claude mcp add`, ou cadastre pelo formulário.")
		case "mcp", "claude", "$", ">", "#", "%", "PS>":
			continue
		default:
			if ehPrefixoDePrompt(t) {
				continue
			}
			return nil, errors.New("Não reconheci este comando. Cole a linha inteira de " +
				"`claude mcp add`, como aparece na documentação do servidor.")
		}
	}
	return nil, ErrComandoVazio
}

// ErrComandoVazio é o campo em branco, e não um comando malformado. A tela
// separa os dois: um pede que se cole algo, o outro pede que se corrija.
var ErrComandoVazio = errors.New("Cole o comando de instalação antes de continuar.")

// ehPrefixoDePrompt reconhece o começo de linha que o copiar-colar traz junto,
// como `vitor@maquina:~$`. Só o que termina em $, > ou #: qualquer outra coisa
// é palavra de verdade e precisa cair na recusa.
func ehPrefixoDePrompt(t string) bool {
	return len(t) > 1 && strings.ContainsAny(t[len(t)-1:], "$>#")
}

// montarImportacao percorre as opções e os posicionais do `claude mcp add`.
//
// A forma é `claude mcp add [opções] <nome> <comando-ou-url> [args...]`, com
// `--` separando as opções do processo a lançar. As opções conhecidas são as
// quatro que a documentação usa: --transport, --scope, --header e --env.
// Opção desconhecida é erro e não é ignorada — uma flag nova do Claude Code
// pode ser justamente a que muda o significado do resto da linha, e descartá-la
// em silêncio gravaria um servidor diferente do que o comando descrevia.
func montarImportacao(tokens []string) (Importacao, error) {
	var (
		imp         Importacao
		posicionais []string
		transporte  string
		headers     []CampoHeader
		ambiente    []CampoEnv
		fimDeOpcoes bool
	)

	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		switch {
		case fimDeOpcoes || !strings.HasPrefix(t, "-") || t == "-":
			posicionais = append(posicionais, t)
			continue
		case t == "--":
			// Tudo depois do -- é o processo a lançar, inclusive o que começa
			// com hífen: `-y` do npx é argumento, não opção do claude.
			fimDeOpcoes = true
			continue
		}

		nome, valor, colado := strings.Cut(t, "=")
		if !colado {
			nome = t
		}
		precisaValor := func() (string, error) {
			if colado {
				return valor, nil
			}
			if i+1 >= len(tokens) {
				return "", errors.New("A opção " + nome + " ficou sem valor no fim do comando.")
			}
			i++
			return tokens[i], nil
		}

		switch nome {
		case "-t", "--transport":
			v, err := precisaValor()
			if err != nil {
				return Importacao{}, err
			}
			transporte = strings.ToLower(strings.TrimSpace(v))
		case "-s", "--scope":
			v, err := precisaValor()
			if err != nil {
				return Importacao{}, err
			}
			imp.Escopo = strings.ToLower(strings.TrimSpace(v))
		case "-H", "--header":
			v, err := precisaValor()
			if err != nil {
				return Importacao{}, err
			}
			headers = append(headers, CampoHeader{Nome: v})
		case "-e", "--env":
			v, err := precisaValor()
			if err != nil {
				return Importacao{}, err
			}
			ambiente = append(ambiente, CampoEnv{Nome: v})
		default:
			return Importacao{}, errors.New("Não conheço a opção " + resumir(nome) +
				". As opções lidas aqui são --transport, --scope, --header e --env.")
		}
	}

	if len(posicionais) == 0 {
		return Importacao{}, errors.New("Faltou o nome do servidor no comando. " +
			"A forma é `claude mcp add <nome> <url-ou-comando>`.")
	}
	if len(posicionais) == 1 {
		return Importacao{}, errors.New("O comando trouxe só o nome " + resumir(posicionais[0]) +
			", sem a URL nem o processo a executar.")
	}

	imp.Form = formularioBase()
	imp.Form.Nome = posicionais[0]
	destino := posicionais[1]

	tipo, aviso, err := tipoDoComando(transporte, destino)
	if err != nil {
		return Importacao{}, err
	}
	imp.Form.Tipo = tipo
	if aviso != "" {
		imp.Avisos = append(imp.Avisos, aviso)
	}

	if tipo == TipoSTDIO {
		imp.Form.Comando = destino
		imp.Form.ArgsTexto = strings.Join(posicionais[2:], "\n")
	} else {
		imp.Form.URL = destino
		if extras := posicionais[2:]; len(extras) > 0 {
			return Importacao{}, errors.New("Depois da URL veio " + resumir(extras[0]) +
				", e um servidor HTTP não recebe argumentos. Confira se o comando não " +
				"perdeu um `--` no meio.")
		}
	}

	if err := aplicarHeaders(&imp, headers, tipo); err != nil {
		return Importacao{}, err
	}
	if err := aplicarAmbiente(&imp, ambiente, tipo); err != nil {
		return Importacao{}, err
	}
	if imp.Escopo != "" {
		imp.Avisos = append(imp.Avisos, "O comando pedia escopo "+resumir(imp.Escopo)+
			", que só existe no Claude Code. Aqui quem decide a visibilidade do MCP são "+
			"os endpoints a que você ligá-lo.")
	}
	return imp, nil
}

// formularioBase é o formulário novo, com os mesmos padrões da tela de cadastro:
// timeout preenchido, habilitado e sonda desligada com os números prontos.
//
// Repetir os padrões aqui em vez de chamar o handler é deliberado: o formulário
// vindo do comando precisa ser indistinguível de um digitado à mão, e a única
// forma de garantir isso é partir do mesmo estado inicial.
func formularioBase() Form {
	return Form{
		Tipo: TipoHTTP, TimeoutMS: TimeoutPadraoMS, Habilitado: true,
		SondaIntervaloMS: SondaIntervaloPadraoMS,
		SondaTimeoutMS:   SondaTimeoutPadraoMS,
		SondaTolerancia:  SondaToleranciaPadrao,
	}
}

// tipoDoComando escolhe o transporte, do --transport quando ele veio e do
// formato do destino quando não veio.
//
// O Claude Code assume stdio quando a flag falta; aqui o destino manda, porque
// uma URL cadastrada como comando viraria uma tentativa de executar um programa
// chamado "https://..." e o erro sairia como "executável não encontrado" — que
// não descreve o que aconteceu. Quando a inferência age, ela vira aviso na tela.
func tipoDoComando(transporte, destino string) (string, string, error) {
	pareceURL := strings.HasPrefix(destino, "http://") || strings.HasPrefix(destino, "https://")

	switch transporte {
	case TipoHTTP, TipoSSE:
		if !pareceURL {
			return "", "", errors.New("O comando pede transporte " + transporte +
				", mas " + resumir(destino) + " não é uma URL http ou https.")
		}
		return transporte, "", nil
	case TipoSTDIO:
		if pareceURL {
			return "", "", errors.New("O comando pede transporte stdio, mas o destino é uma URL. " +
				"Confira se não faltou `--transport http`.")
		}
		return TipoSTDIO, "", nil
	case "":
		if pareceURL {
			return TipoHTTP, "O comando não trazia `--transport`, e o destino é uma URL: " +
				"cadastrei como Streamable HTTP. Se o servidor for SSE legado, troque antes de criar.", nil
		}
		return TipoSTDIO, "", nil
	default:
		return "", "", errors.New("Transporte desconhecido: " + resumir(transporte) +
			". O patchbay fala http, sse e stdio.")
	}
}

// aplicarHeaders reparte os --header entre o slot de bearer e os headers
// estáticos.
//
// Authorization não é um header como os outros aqui: o patchbay o monta a partir
// do bearer, e o CHECK da tabela recusa gravá-lo como slot de header. Por isso
// `Authorization: Bearer <token>` — a forma que praticamente toda documentação
// usa — vira bearer, e qualquer outro esquema de autenticação vira recusa com o
// nome do esquema, em vez de um header que o banco rejeitaria depois.
func aplicarHeaders(imp *Importacao, headers []CampoHeader, tipo string) error {
	if len(headers) == 0 {
		return nil
	}
	if tipo == TipoSTDIO {
		return errors.New("O comando traz --header num servidor STDIO. Header é coisa de HTTP; " +
			"um processo local recebe credencial por variável de ambiente.")
	}
	if len(headers) > LimiteDeHeaders {
		return errors.New("São no máximo " + strconv.Itoa(LimiteDeHeaders) + " headers.")
	}

	for _, h := range headers {
		nome, valor, ok := strings.Cut(h.Nome, ":")
		nome = strings.TrimSpace(nome)
		valor = strings.TrimSpace(valor)
		if !ok || nome == "" {
			return errors.New("O header " + resumir(h.Nome) + " não está no formato Nome: valor.")
		}
		if err := conferirPlaceholder(nome, valor); err != nil {
			return err
		}

		if !strings.EqualFold(nome, "authorization") {
			imp.Form.Headers = append(imp.Form.Headers, CampoHeader{
				Nome: nome, Valor: cripto.Segredo(valor),
			})
			continue
		}

		esquema, token, temEsquema := strings.Cut(valor, " ")
		if !temEsquema || !strings.EqualFold(esquema, "bearer") {
			return errors.New("O Authorization do comando não é Bearer. O patchbay monta " +
				"Authorization só a partir de um bearer; para outro esquema, cadastre o " +
				"header pelo formulário.")
		}
		if !imp.Form.Bearer.Vazio() {
			return errors.New("O comando traz dois Authorization. Deixe só um.")
		}
		imp.Form.Bearer = cripto.Segredo(strings.TrimSpace(token))
	}
	return nil
}

// aplicarAmbiente manda todo --env para o bloco cifrado.
//
// O comando não diz quais variáveis são segredo, e a diferença entre os dois
// blocos do formulário é justamente essa. Cifrar o que não precisava custa
// nada; deixar em claro um GITHUB_TOKEN porque o comando não avisou custa o
// token. O aviso na tela diz que a escolha foi do patchbay e onde desfazê-la.
func aplicarAmbiente(imp *Importacao, ambiente []CampoEnv, tipo string) error {
	if len(ambiente) == 0 {
		return nil
	}
	if tipo != TipoSTDIO {
		return errors.New("O comando traz --env num servidor " + rotuloDoTipo(tipo) +
			". Variável de ambiente só chega a um processo local; num servidor HTTP a " +
			"credencial vai por header.")
	}
	if len(ambiente) > LimiteDeEnv {
		return errors.New("São no máximo " + strconv.Itoa(LimiteDeEnv) + " variáveis.")
	}

	for _, e := range ambiente {
		nome, valor, ok := strings.Cut(e.Nome, "=")
		nome = strings.TrimSpace(nome)
		if !ok || nome == "" {
			return errors.New("A variável " + resumir(e.Nome) + " não está no formato NOME=valor.")
		}
		if err := conferirPlaceholder(nome, valor); err != nil {
			return err
		}
		imp.Form.EnvSecretos = append(imp.Form.EnvSecretos, CampoEnv{
			Nome: nome, Valor: cripto.Segredo(valor),
		})
	}
	imp.Avisos = append(imp.Avisos, "As variáveis do comando foram para o bloco cifrado, "+
		"porque o comando não diz quais são segredo. Depois de criar, o que não for "+
		"segredo pode ir para o bloco em claro na edição.")
	return nil
}

// conferirPlaceholder recusa a credencial que ainda é o exemplo da documentação.
//
// `--header "Authorization: Bearer [your API token]"` colado como está grava um
// token que só falha na primeira chamada, e o sintoma chega dias depois como 401
// sem ninguém lembrar do cadastro. Recusar na hora é a única chance de dizer
// "troque isto pelo seu token" enquanto o admin ainda está olhando o comando.
func conferirPlaceholder(nome, valor string) error {
	if !pareceMarcador(valor) {
		return nil
	}
	return errors.New("O valor de " + nome + " ainda é o marcador da documentação. " +
		"Troque-o pelo seu token no comando e cole de novo.")
}

// pareceMarcador reconhece o texto de exemplo que a documentação deixa no lugar
// da credencial. Só as formas que ninguém usaria num segredo de verdade:
// delimitado por colchetes ou sinais de maior/menor, expansão de shell, ou a
// palavra "your" no começo.
func pareceMarcador(valor string) bool {
	v := strings.TrimSpace(valor)
	if v == "" {
		return false
	}
	// O esquema fica de fora: em "Bearer <token>" quem é marcador é o token.
	if esquema, resto, ok := strings.Cut(v, " "); ok && strings.EqualFold(esquema, "bearer") {
		v = strings.TrimSpace(resto)
	}
	switch {
	case strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]"),
		strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">"),
		strings.HasPrefix(v, "${") || strings.HasPrefix(v, "$env:"):
		return true
	}
	primeira, _, _ := strings.Cut(strings.ToLower(v), " ")
	primeira = strings.Trim(primeira, "\"'_-")
	return primeira == "your" || strings.HasPrefix(primeira, "your_") || strings.HasPrefix(primeira, "your-")
}

// tokenizarComando quebra a linha colada em argumentos, com as regras de aspas
// do shell POSIX.
//
// Não é um shell: não expande variável, não resolve glob e não interpreta
// operador. É só o suficiente para desfazer o que as aspas da documentação
// fizeram — sem isto, `--header "Authorization: Bearer abc"` chegaria como três
// tokens e o header sairia truncado no primeiro espaço.
//
// A quebra de linha conta como espaço, e a contrabarra no fim da linha some:
// documentação costuma quebrar o comando em várias linhas, e exigir que o admin
// junte tudo numa só antes de colar seria devolver a ele o trabalho que esta
// tela existe para tirar.
func tokenizarComando(texto string) ([]string, error) {
	if len(texto) > LimiteDoComando {
		return nil, errors.New("O comando colado é grande demais.")
	}
	texto = normalizarAspas(texto)

	var (
		tokens []string
		atual  strings.Builder
		aberto bool // há token em construção, mesmo que vazio ("" é um token)
	)
	fechar := func() {
		if aberto {
			tokens = append(tokens, atual.String())
			atual.Reset()
			aberto = false
		}
	}

	runas := []rune(texto)
	for i := 0; i < len(runas); i++ {
		c := runas[i]
		switch {
		case c == '\\' && i+1 < len(runas) && runas[i+1] == '\n':
			// Continuação de linha: a contrabarra e a quebra somem juntas.
			i++
		case c == '\\' && i+1 < len(runas):
			i++
			atual.WriteRune(runas[i])
			aberto = true
		case c == '\'' || c == '"':
			trecho, prox, err := lerCitado(runas, i)
			if err != nil {
				return nil, err
			}
			atual.WriteString(trecho)
			aberto = true
			i = prox
		case unicode.IsSpace(c):
			fechar()
		default:
			atual.WriteRune(c)
			aberto = true
		}
	}
	fechar()

	if len(tokens) == 0 {
		return nil, ErrComandoVazio
	}
	return tokens, nil
}

// lerCitado consome um trecho entre aspas a partir de runas[inicio], e devolve o
// conteúdo e o índice da aspa de fechamento.
//
// Aspa simples é literal, como no shell. Aspa dupla desfaz a contrabarra apenas
// diante dos caracteres em que o shell a desfaz — \" e \\ —, para que um caminho
// do Windows dentro de aspas duplas continue com as barras que tinha.
func lerCitado(runas []rune, inicio int) (string, int, error) {
	aspa := runas[inicio]
	var b strings.Builder
	for i := inicio + 1; i < len(runas); i++ {
		c := runas[i]
		if c == aspa {
			return b.String(), i, nil
		}
		if aspa == '"' && c == '\\' && i+1 < len(runas) &&
			(runas[i+1] == '"' || runas[i+1] == '\\') {
			i++
			b.WriteRune(runas[i])
			continue
		}
		b.WriteRune(c)
	}
	return "", 0, errors.New("O comando tem aspa aberta e não fechada. " +
		"Confira se a linha foi copiada inteira.")
}

// normalizarAspas troca as aspas tipográficas pelas retas.
//
// Documentação renderizada em HTML costuma entregar “ ” e ‘ ’ no copiar-colar, e
// um token começado por aspa curva não fecharia nunca — o admin veria "aspa
// aberta e não fechada" sobre um comando que na tela dele parece correto.
func normalizarAspas(texto string) string {
	return strings.NewReplacer(
		"“", `"`, "”", `"`, "„", `"`, "‟", `"`,
		"‘", "'", "’", "'", "‚", "'", "‛", "'",
		" ", " ",
	).Replace(texto)
}
