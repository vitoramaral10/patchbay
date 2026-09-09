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
