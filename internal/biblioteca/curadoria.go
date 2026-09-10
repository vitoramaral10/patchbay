package biblioteca

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BaseCuradoria é a segunda origem: a lista curada de servidores MCP remotos do
// mcpservers.org.
//
// Ela existe ao lado do registry, e não no lugar dele, porque as duas ganham em
// coisas diferentes. Medido em 2026-09-09, numa amostra de 25 dos 293 remotos
// de lá:
//
//   - 25 de 25 declaram **autenticação** (22 delas OAuth). O esquema do registry
//     não tem esse campo, e é justamente ele que decide se o formulário de
//     upstream abre em OAuth ou em credencial estática.
//   - só 3 de 25 existem no registry. Neon, MDN, Pendo, Blackbaud, Candid e
//     Unthread são remotos conhecidos que simplesmente não estão lá.
//   - o resumo vem em português, o que faz a busca por "banco de dados"
//     funcionar — o registry publica só em inglês.
//
// O caminho carrega o idioma: /pt-BR traz resumo e descrição traduzidos, e os
// valores que viram configuração (a URL, "Streamable HTTP") não são traduzidos.
const BaseCuradoria = "https://mcpservers.org/pt-BR"

const (
	// esperaEntreCuradas é a pausa entre duas páginas de detalhe.
	//
	// Dois segundos, e o número é medido, não escolhido. A origem limita taxa:
	// com 250 ms de pausa, uma varredura de verdade em 2026-09-09 trouxe **63
	// de 293** — as outras 230 vieram com HTTP 429. Com dois segundos, a mesma
	// sequência passa. São ~10 minutos para as 293 páginas, dentro de uma
	// varredura de fundo que roda de doze em doze horas: a pressa aqui não vale
	// nada.
	//
	// A origem não manda Retry-After, então não há o que obedecer — o que
	// resta é ir devagar e insistir, e quem insiste é o sincronizador.
	esperaEntreCuradas = 2 * time.Second
	// agenteDeNavegador é o User-Agent que a origem aceita. Não é disfarce: o
	// robots.txt de lá libera /remote-mcp-servers, e o que se pede é a mesma
	// página que um leitor humano abre.
	agenteDeNavegador = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// Curadoria é o cliente do mcpservers.org.
type Curadoria struct {
	base    string
	cliente *http.Client
}

// NovaCuradoria monta o cliente. base vazio usa BaseCuradoria.
func NovaCuradoria(base string) *Curadoria {
	if base == "" {
		base = BaseCuradoria
	}
	return &Curadoria{
		base: strings.TrimSuffix(base, "/"),
		cliente: &http.Client{
			Timeout: timeoutPadrao,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					return fmt.Errorf("%w: redirecionamento para outro host (%s)",
						ErrOrigemIndisponivel, req.URL.Host)
				}
				if len(via) >= 5 {
					return errors.New("redirecionamentos demais")
				}
				return nil
			},
		},
	}
}

// Slugs devolve os identificadores que a lista de remotos publica agora.
func (c *Curadoria) Slugs(ctx context.Context) ([]string, error) {
	alvo, err := url.JoinPath(c.base, "remote-mcp-servers")
	if err != nil {
		return nil, fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	pagina, err := c.buscar(ctx, alvo)
	if err != nil {
		return nil, err
	}
	achados := reCartaoCurado.FindAllStringSubmatch(pagina, -1)
	visto := make(map[string]bool, len(achados))
	slugs := make([]string, 0, len(achados))
	for _, a := range achados {
		if visto[a[1]] {
			continue
		}
		visto[a[1]] = true
		slugs = append(slugs, a[1])
	}
	// Zero item numa página que chegou inteira é a marcação ter mudado. Lista
	// vazia devolvida como sucesso viraria "a curadoria acabou", que é a leitura
	// errada — e apagaria a autenticação de todo mundo na mesclagem.
	if len(slugs) == 0 {
		return nil, fmt.Errorf("%w: nenhum servidor na lista de remotos", ErrFormatoDaOrigem)
	}
	return slugs, nil
}

// Um lê a página de um servidor curado.
//
// É uma requisição por servidor porque a página de listagem traz só nome e
// resumo: URL, transporte e autenticação — o que o cadastro precisa — só existem
// no detalhe.
func (c *Curadoria) Um(ctx context.Context, slug string) (Item, error) {
	if !slugValido(slug) {
		return Item{}, ErrNaoEncontrado
	}
	alvo, err := url.JoinPath(c.base, "remote-mcp-servers", slug)
	if err != nil {
		return Item{}, fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	pagina, err := c.buscar(ctx, alvo)
	if err != nil {
		return Item{}, err
	}
	return lerCurado(slug, pagina)
}

// buscar traz uma página da origem.
//
// recusado por slugValido; o cliente ainda barra redirecionamento para outro host
//
//nolint:gosec // a URL é a base do pacote mais um caminho fixo, com o slug já
func (c *Curadoria) buscar(ctx context.Context, alvo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	req.Header.Set("User-Agent", agenteDeNavegador)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")

	resp, err := c.cliente.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	defer func() { _ = resp.Body.Close() }()

	corpo, err := io.ReadAll(io.LimitReader(resp.Body, tetoDaResposta))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrNaoEncontrado
	case resp.StatusCode == http.StatusTooManyRequests:
		// Separado dos demais porque a ação é outra: 429 passa se esperar, e é
		// o erro que a varredura precisa insistir em vez de descartar a página.
		return "", fmt.Errorf("%w: HTTP 429", ErrTaxaExcedida)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: HTTP %d", ErrOrigemIndisponivel, resp.StatusCode)
	}
	// O desafio de bot do Cloudflare chega com 200 e HTML no lugar da página.
	// Sem esta checagem ele viraria "formato mudou", que manda procurar o
	// defeito no lugar errado.
	if desafioDeBot(string(corpo)) {
		return "", fmt.Errorf("%w: a origem respondeu com desafio de bot", ErrOrigemIndisponivel)
	}
	return string(corpo), nil
}

func desafioDeBot(s string) bool {
	inicio := s
	if len(inicio) > 600 {
		inicio = inicio[:600]
	}
	return strings.Contains(inicio, "Just a moment")
}

// As expressões que traduzem a página da origem.
//
// São regulares e não um parser de HTML de verdade porque o que se lê são cinco
// pedaços de marcação estáveis, e uma árvore inteira para pegar cinco campos
// custaria uma dependência nova. Cada uma está amarrada a um pedaço nomeado da
// página, e quando uma delas para de casar o pacote devolve ErrFormatoDaOrigem
// — nunca um item pela metade, que viraria um cadastro errado.
var (
	// reCartaoCurado é o item da lista: o link para o servidor.
	reCartaoCurado = regexp.MustCompile(
		`<a href="(?:/[a-zA-Z-]+)?/remote-mcp-servers/([a-z0-9][a-z0-9-]*)"`)

	// Os campos da página de detalhe.
	reTituloCurado   = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>\s*(?:<p[^>]*>(.*?)</p>)?`)
	reEnderecoCurado = regexp.MustCompile(`(?s)<code[^>]*>\s*(https?://[^<\s]+)\s*</code>`)
	reSobreCurado    = regexp.MustCompile(`(?s)<h2[^>]*>\s*(?:Sobre|About) [^<]*</h2>.{0,400}?<p[^>]*>(.*?)</p>`)

	reMarcacao = regexp.MustCompile(`<[^>]+>`)
	reEspacos  = regexp.MustCompile(`\s+`)
	reSlug     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,80}$`)
)

// moldeListaDefine vira uma expressão por rótulo lido: transporte e autenticação
// vêm no mesmo <dl>, e só o texto do <dt> os separa.
const moldeListaDefine = `(?s)<dt[^>]*>\s*%s\s*</dt>\s*<dd[^>]*>(.*?)</dd>`

// slugValido barra o que nunca poderia ser um slug da origem antes de virar
// caminho de URL.
func slugValido(s string) bool { return reSlug.MatchString(s) }

// lerCurado tira da página de um servidor o que o cadastro precisa.
//
// Item pela metade é recusado inteiro: um cartão que leva a um formulário com a
// URL certa e o transporte errado é pior do que o servidor não aparecer, porque
// o erro só se manifesta na primeira conexão.
func lerCurado(slug, pagina string) (Item, error) {
	i := Item{
		// O nome carrega a origem porque este servidor pode não existir no
		// registry — medido em 2026-09-09, 22 de 25 não existiam. Ele precisa
		// de identidade própria para o botão de adicionar ter para onde
		// apontar, e "mcpservers.org/<slug>" é honesto: diz de onde veio.
		Nome:   "mcpservers.org/" + slug,
		Curado: true,
	}

	if m := reTituloCurado.FindStringSubmatch(pagina); m != nil {
		i.Titulo = texto(m[1])
		i.Descricao = texto(m[2])
	}
	if i.Titulo == "" {
		return Item{}, fmt.Errorf("%w: %s sem nome", ErrFormatoDaOrigem, slug)
	}
	if i.Descricao == "" {
		if m := reSobreCurado.FindStringSubmatch(pagina); m != nil {
			i.Descricao = texto(m[1])
		}
	}

	// A URL sai do bloco de conexão, e a procura começa nele: a página tem
	// outros <code> — os comandos de configuração de cada cliente —, e o
	// primeiro de todos nem sempre é o endereço.
	if j := indiceDaConexao(pagina); j >= 0 {
		if m := reEnderecoCurado.FindStringSubmatch(recorte(pagina, j, 4000)); m != nil {
			i.URL = texto(m[1])
		}
	}
	switch {
	case i.URL == "":
		return Item{}, fmt.Errorf("%w: %s sem URL de conexão", ErrFormatoDaOrigem, slug)
	case !strings.HasPrefix(i.URL, "https://"):
		// Endpoint em texto claro carregaria o bearer do upstream pela rede sem
		// cifra. Quem realmente precisar cadastra à mão, de olhos abertos.
		return Item{}, fmt.Errorf("%w: %s com URL sem https (%s)", ErrFormatoDaOrigem, slug, i.URL)
	}

	transporte := campoCurado(pagina, "Transporte", "Transport")
	if transporte == "" {
		return Item{}, fmt.Errorf("%w: %s sem transporte declarado", ErrFormatoDaOrigem, slug)
	}
	if strings.Contains(strings.ToLower(transporte), "sse") {
		i.Transporte = TransporteSSE
	} else {
		i.Transporte = TransporteHTTP
	}

	i.Autenticacao = autenticacaoDe(campoCurado(pagina, `Autentica\S+`, "Authentication"))
	i.PedeCredencial = i.Autenticacao != AutAberta
	return i, nil
}

func indiceDaConexao(pagina string) int {
	for _, marca := range []string{"Detalhes da conex", "Connection details"} {
		if j := strings.Index(pagina, marca); j >= 0 {
			return j
		}
	}
	return -1
}

// autenticacaoDe traduz a frase da origem numa das três formas conhecidas.
//
// O padrão é token, e não aberta: errar para "precisa de token" faz a tela pedir
// uma credencial que talvez não seja necessária — chato. Errar para "aberta" faz
// o admin cadastrar sem credencial um servidor que exige uma, e o upstream nasce
// degradado com 401. Entre os dois erros, o primeiro é o barato.
func autenticacaoDe(frase string) string {
	f := strings.ToLower(frase)
	switch {
	case strings.Contains(f, "oauth"):
		return AutOAuth
	case strings.Contains(f, "aberto"), strings.Contains(f, "sem autentica"),
		strings.Contains(f, "no auth"), strings.Contains(f, "open"), strings.Contains(f, "none"):
		return AutAberta
	default:
		return AutToken
	}
}

// campoCurado lê um par <dt>rótulo</dt><dd>valor</dd>, tentando os rótulos na
// ordem dada: a origem traduz o <dt> conforme o idioma do caminho, e o inglês
// fica como segunda tentativa.
//
// Os rótulos entram como fragmento de regex, e não como literal, só para
// `Autentica\S+` cobrir a acentuação sem o arquivo depender de como o editor
// gravou a cedilha. São constantes deste pacote — nada de fora chega aqui.
func campoCurado(pagina string, rotulos ...string) string {
	for _, r := range rotulos {
		re := regexp.MustCompile(fmt.Sprintf(moldeListaDefine, r))
		if m := re.FindStringSubmatch(pagina); m != nil {
			return texto(m[1])
		}
	}
	return ""
}

// texto tira marcação, resolve entidade e normaliza espaço.
func texto(s string) string {
	s = reMarcacao.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(reEspacos.ReplaceAllString(s, " "))
}

func recorte(s string, de, tamanho int) string {
	ate := de + tamanho
	if ate > len(s) {
		ate = len(s)
	}
	return s[de:ate]
}

// BaseOficiais é a lista de "servidores MCP oficiais" do mcpservers.org.
//
// Mesmo site da curadoria de remotos, acervo diferente: estes vivem em
// /servers/<slug> e são, quase todos, processo local. Medido em 2026-09-09, o
// que eles declaram é bem menos do que os remotos declaram:
//
//   - **nenhum** tem <dt>Transporte</dt>, <dt>Autenticação</dt> nem URL no bloco
//     de conexão (0 de 10 amostrados);
//   - o comando existe só como trecho de copiar-e-colar do README, e numa
//     amostra de 14 apenas 4 eram aproveitáveis: 9 não tinham bloco algum e 1
//     trazia caminho de exemplo (C:\PATH\TO\PARENT\FOLDER).
//
// Por isso este acervo entra pelo que ele é — uma lista de nomes que alguém
// chamou de oficiais — e só quando o comando sai limpo. O acervo maior do mesmo
// site (/all, 12.173) fica de fora: seriam 406 páginas de índice mais 12.173 de
// detalhe, ~7 horas por varredura, para a mesma qualidade de dado que o registry
// já entrega estruturada em packages[].
const caminhoOficiais = "official"

// TetoDePaginasOficiais fecha a paginação de /official.
//
// Medido em 22 páginas de 30 em 2026-09-09. O teto é folga com fim: paginação
// de terceiro que passa a devolver sempre a mesma página viraria varredura
// eterna sem ele.
const TetoDePaginasOficiais = 60

// SlugsOficiais percorre a paginação de /official e devolve os identificadores.
//
// Diferente dos remotos, aqui o índice pagina: 647 servidores de 30 em 30. A
// última página é descoberta pelos próprios links de paginação, e não chutada.
func (c *Curadoria) SlugsOficiais(ctx context.Context) ([]string, error) {
	primeira, err := c.paginaOficial(ctx, 1)
	if err != nil {
		return nil, err
	}
	slugs, visto := slugsDeServidor(primeira), map[string]bool{}
	for _, s := range slugs {
		visto[s] = true
	}
	ultima := ultimaPagina(primeira)
	if ultima > TetoDePaginasOficiais {
		ultima = TetoDePaginasOficiais
	}
	for p := 2; p <= ultima; p++ {
		pagina, err := c.paginaOficial(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("página %d: %w", p, err)
		}
		for _, s := range slugsDeServidor(pagina) {
			if visto[s] {
				continue
			}
			visto[s] = true
			slugs = append(slugs, s)
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("%w: nenhum servidor na lista de oficiais", ErrFormatoDaOrigem)
	}
	return slugs, nil
}

func (c *Curadoria) paginaOficial(ctx context.Context, n int) (string, error) {
	alvo, err := url.JoinPath(c.base, caminhoOficiais)
	if err != nil {
		return "", fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	if n > 1 {
		alvo += "?page=" + strconv.Itoa(n)
	}
	return c.buscar(ctx, alvo)
}

// Oficial lê a página de um servidor do acervo /servers/.
//
// Devolve ErrFormatoDaOrigem quando não há comando aproveitável — que é o caso
// da maioria. Recusar é o comportamento: um cartão com "adicionar" que abre um
// formulário sem comando é pior do que o servidor não aparecer.
func (c *Curadoria) Oficial(ctx context.Context, slug string) (Item, error) {
	if !slugDeServidorValido(slug) {
		return Item{}, ErrNaoEncontrado
	}
	alvo, err := url.JoinPath(c.base, "servers", slug)
	if err != nil {
		return Item{}, fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	pagina, err := c.buscar(ctx, alvo)
	if err != nil {
		return Item{}, err
	}
	return lerOficial(slug, pagina)
}

// As expressões do acervo /servers/.
var (
	// reCartaoOficial casa o link de um servidor no índice. O slug pode ter
	// uma barra no meio (AudienseCo/mcp-audiense-insights) e maiúsculas.
	reCartaoOficial = regexp.MustCompile(`href="[^"]*?/servers/([^"?#]+)"`)
	// rePaginaOficial casa os links de paginação, para descobrir a última.
	rePaginaOficial = regexp.MustCompile(`/official\?page=(\d+)`)
	// reSlugDeServidor é a forma de um slug do acervo /servers/.
	reSlugDeServidor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}(?:/[A-Za-z0-9][A-Za-z0-9._-]{0,80})?$`)

	// reComandoDoSnippet tira o comando e os argumentos do trecho de
	// copiar-e-colar que a página mostra. É JSON dentro do HTML, escapado, e por
	// isso a leitura acontece depois de resolver as entidades.
	//
	// Os dois na mesma expressão, e nesta ordem, porque é assim que o site os
	// escreve. Comando sem args, ou args antes do comando, não casa — e não
	// casar é o resultado certo: o que sai daqui vira linha de comando de um
	// processo, e meio acerto ali é um upstream que não sobe.
	reComandoDoSnippet = regexp.MustCompile(`(?s)"command"\s*:\s*"([^"]+)"\s*,\s*"args"\s*:\s*\[([^\])]{0,400})\]`)
	reArgDoSnippet     = regexp.MustCompile(`"([^"]*)"`)

	// rePlaceholder são os marcadores que o README deixa para a pessoa trocar.
	// Um comando com isso dentro não sobe, e cadastrá-lo assim seria entregar um
	// upstream quebrado com cara de pronto.
	replaceholderNoArg = regexp.MustCompile(`(?i)PATH[/\\]TO|YOUR[_ ]|<[A-Z_]{3,}>|CAMINHO[_/ ]|/path/to|SEU[_ ]`)
)

func slugsDeServidor(pagina string) []string {
	achados := reCartaoOficial.FindAllStringSubmatch(pagina, -1)
	visto := make(map[string]bool, len(achados))
	slugs := make([]string, 0, len(achados))
	for _, a := range achados {
		s := a[1]
		if visto[s] || !slugDeServidorValido(s) {
			continue
		}
		visto[s] = true
		slugs = append(slugs, s)
	}
	return slugs
}

// ultimaPagina lê o maior número que os links de paginação citam.
//
// A página mostra os vizinhos e a última — é dela que sai o total, e é por isso
// que este código não precisa adivinhar quantas páginas existem.
func ultimaPagina(pagina string) int {
	maior := 1
	for _, m := range rePaginaOficial.FindAllStringSubmatch(pagina, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maior {
			maior = n
		}
	}
	return maior
}

func slugDeServidorValido(s string) bool { return reSlugDeServidor.MatchString(s) }

// lerOficial tira da página de um servidor do acervo /servers/ o que dá para
// cadastrar.
func lerOficial(slug, pagina string) (Item, error) {
	i := Item{
		// A barra do slug sobrevive no nome, e o nome fica com três segmentos
		// (mcpservers.org/AudienseCo/mcp-audiense-insights). É de propósito: o
		// nome é identidade, e encurtá-lo criaria colisão entre dois servidores
		// da mesma organização.
		Nome:       "mcpservers.org/" + slug,
		Curado:     true,
		Transporte: TransporteSTDIO,
	}
	if m := reTituloCurado.FindStringSubmatch(pagina); m != nil {
		i.Titulo = texto(m[1])
		i.Descricao = texto(m[2])
	}
	if i.Titulo == "" {
		return Item{}, fmt.Errorf("%w: %s sem nome", ErrFormatoDaOrigem, slug)
	}
	if i.Descricao == "" {
		if m := reSobreCurado.FindStringSubmatch(pagina); m != nil {
			i.Descricao = texto(m[1])
		}
	}

	comando, args, ok := execucaoDoSnippet(pagina)
	if !ok {
		// A maioria cai aqui, e cair aqui é o certo. Ver o comentário de
		// BaseOficiais: numa amostra de 14, só 4 tinham comando aproveitável.
		return Item{}, fmt.Errorf("%w: %s sem comando aproveitável", ErrFormatoDaOrigem, slug)
	}
	i.Comando, i.Args = comando, args
	return i, nil
}

// execucaoDoSnippet lê o comando do trecho de configuração da página.
//
// Recusa o que tem marcador de exemplo dentro: "C:/PATH/TO/PARENT/FOLDER" e
// "YOUR_API_KEY" são pedidos para a pessoa trocar, e cadastrá-los como se
// fossem configuração entrega um upstream quebrado com cara de pronto.
func execucaoDoSnippet(pagina string) (string, []string, bool) {
	m := reComandoDoSnippet.FindStringSubmatch(html.UnescapeString(pagina))
	if m == nil {
		return "", nil, false
	}
	comando := strings.TrimSpace(m[1])
	if comando == "" || replaceholderNoArg.MatchString(comando) {
		return "", nil, false
	}
	var args []string
	for _, a := range reArgDoSnippet.FindAllStringSubmatch(m[2], -1) {
		v := strings.TrimSpace(a[1])
		if v == "" {
			continue
		}
		if replaceholderNoArg.MatchString(v) {
			return "", nil, false
		}
		args = append(args, v)
	}
	if len(args) == 0 {
		return "", nil, false
	}
	return comando, args, true
}
