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
	"sync"
	"time"
)

// BaseMCPServers é a origem em produção.
//
// O caminho carrega o idioma: as páginas em /pt-BR trazem resumo e descrição
// traduzidos, e a tela do patchbay é em português. Os valores que viram
// configuração — a URL do endpoint, "Streamable HTTP" — não são traduzidos pela
// origem, então o idioma muda o texto e não o que é cadastrado.
const BaseMCPServers = "https://mcpservers.org/pt-BR"

// ValidadePadrao é por quanto tempo a lista buscada continua servindo.
//
// Não é catálogo guardado: é o intervalo mínimo entre duas idas à origem. A tela
// busca a cada abertura e a cada busca digitada, e sem isto uma pessoa
// digitando "notion" mandaria uma rajada de requisições ao site — que é
// exatamente o que faz o Cloudflare de lá responder com desafio de bot. Um
// minuto absorve a digitação e ainda deixa a tela refletir o site do mesmo
// minuto.
//
// A lista vencida nunca é servida: se a origem cair, a tela dá erro em vez de
// mostrar o que sobrou da última vez. Mostrar o velho calado é justamente o que
// a decisão de não guardar nada recusa.
const ValidadePadrao = time.Minute

const (
	// tetoDaResposta corta o corpo da origem. As páginas ficam em ~50 kB (índice)
	// e ~25 kB (detalhe); o teto é folga, e existe porque ler sem limite o corpo
	// de um terceiro é como se enche a memória do processo.
	tetoDaResposta = 8 << 20
	// timeoutPadrao é o prazo de uma ida à origem, do connect ao último byte.
	timeoutPadrao = 15 * time.Second
	// agente é um User-Agent de navegador porque a origem recusa os demais. Não
	// é disfarce: o robots.txt de lá libera /remote-mcp-servers, e o que se pede
	// aqui é a mesma página que um leitor humano abre.
	agente = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// Origem é o cliente do mcpservers.org.
//
// Guarda a última lista por ValidadePadrao e nada mais: não há arquivo, não há
// tabela, e nada sobrevive ao processo.
type Origem struct {
	base     string
	cliente  *http.Client
	validade time.Duration

	mu     sync.Mutex
	lista  []Item
	lidoEm time.Time
}

// NovaOrigem monta o cliente. base vazio usa BaseMCPServers; validade zero usa
// ValidadePadrao.
func NovaOrigem(base string, validade time.Duration) *Origem {
	if base == "" {
		base = BaseMCPServers
	}
	if validade <= 0 {
		validade = ValidadePadrao
	}
	return &Origem{
		base:     strings.TrimSuffix(base, "/"),
		validade: validade,
		cliente: &http.Client{
			Timeout: timeoutPadrao,
			// Nunca seguir redirecionamento para fora do host da origem: a
			// biblioteca busca uma página conhecida, e um 302 para outro lugar é
			// coisa que só interessa a quem quer que o patchbay busque outra
			// coisa.
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

// Listar devolve os servidores remotos que a origem publica agora.
func (o *Origem) Listar(ctx context.Context) ([]Item, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.lista != nil && time.Since(o.lidoEm) < o.validade {
		return o.lista, nil
	}
	alvo, err := url.JoinPath(o.base, "remote-mcp-servers")
	if err != nil {
		return nil, fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	pagina, err := o.buscar(ctx, alvo)
	if err != nil {
		return nil, err
	}
	itens, err := lerIndice(pagina)
	if err != nil {
		return nil, err
	}
	o.lista, o.lidoEm = itens, time.Now()
	return itens, nil
}

// Detalhe busca a página de um servidor: é dela que saem a URL, o transporte e a
// forma de autenticação que o formulário de upstream precisa.
//
// Sem cache: acontece uma vez, quando o admin clica em adicionar, e é a
// informação que mais precisa estar certa no momento do cadastro.
func (o *Origem) Detalhe(ctx context.Context, slug string) (Detalhe, error) {
	if !slugValido(slug) {
		return Detalhe{}, ErrNaoEncontrado
	}
	// JoinPath escapa cada segmento: junto com slugValido, é o que garante que
	// o slug vira um pedaço do caminho e nunca outro host, outra porta ou um
	// caminho acima da base.
	alvo, err := url.JoinPath(o.base, "remote-mcp-servers", slug)
	if err != nil {
		return Detalhe{}, fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	pagina, err := o.buscar(ctx, alvo)
	if err != nil {
		return Detalhe{}, err
	}
	return lerDetalhe(slug, pagina)
}

// Buscar filtra a lista da origem por termo livre.
//
// Sobre nome e resumo, sem distinguir maiúscula de minúscula, exigindo todos os
// pedaços do termo em qualquer ordem. Substring e não prefixo: quem procura
// "jira" precisa achar Atlassian, cujo resumo cita Jira e cujo nome não — que é
// exatamente o caso em que a busca da própria origem falha.
func Buscar(itens []Item, termo string) []Item {
	campos := strings.Fields(strings.ToLower(strings.TrimSpace(termo)))
	if len(campos) == 0 {
		return itens
	}
	achados := make([]Item, 0, len(itens))
	for _, i := range itens {
		alvo := strings.ToLower(i.Nome + " " + i.Resumo + " " + i.Slug)
		if contemTodos(alvo, campos) {
			achados = append(achados, i)
		}
	}
	return achados
}

func contemTodos(alvo string, campos []string) bool {
	for _, c := range campos {
		if !strings.Contains(alvo, c) {
			return false
		}
	}
	return true
}

var reSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,80}$`)

// slugValido barra o que nunca poderia ser um slug da origem antes de virar
// caminho de URL. É higiene de borda: o slug chega pela URL da tela.
func slugValido(s string) bool { return reSlug.MatchString(s) }

// buscar traz uma página da origem.
//
// O gosec marca as duas chamadas abaixo como SSRF porque a URL é variável e não
// literal. Ela não vem de fora: é a base — constante do pacote em produção, e um
// httptest no teste — mais um caminho fixo, juntados por url.JoinPath, com o
// único pedaço variável (o slug) já recusado por slugValido antes de chegar
// aqui. O cliente ainda recusa redirecionamento que troque de host, então nem a
// resposta da origem consegue mover a requisição para outro lugar.
//
//nolint:gosec // ver o parágrafo acima: a URL não é entrada de terceiro
func (o *Origem) buscar(ctx context.Context, alvo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	req.Header.Set("User-Agent", agente)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")

	resp, err := o.cliente.Do(req)
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
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: HTTP %d", ErrOrigemIndisponivel, resp.StatusCode)
	}
	// O desafio de bot do Cloudflare chega com 200 e corpo de HTML no lugar da
	// página. Sem esta checagem ele viraria "formato da origem mudou", que manda
	// procurar o defeito no lugar errado.
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
// São regulares e não um parser de HTML de verdade porque o que se lê são quatro
// pedaços de marcação estáveis, e uma árvore inteira para pegar quatro campos
// custaria uma dependência nova. Cada uma está amarrada a um pedaço nomeado da
// página, e quando uma delas para de casar o pacote devolve ErrFormatoDaOrigem
// — nunca um item pela metade.
var (
	// reCartao é o item da página índice: o link para o servidor, com nome e
	// resumo nos dois primeiros <div class="truncate ...">.
	reCartao = regexp.MustCompile(
		`(?s)<a href="(?:/[a-zA-Z-]+)?/remote-mcp-servers/([a-z0-9][a-z0-9-]*)"(.*?)</a>`)
	reTruncado = regexp.MustCompile(`(?s)<div class="truncate[^"]*">(.*?)</div>`)

	// Os quatro campos da página de detalhe.
	reTitulo   = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>\s*(?:<p[^>]*>(.*?)</p>)?`)
	reEndereco = regexp.MustCompile(`(?s)<code[^>]*>\s*(https?://[^<\s]+)\s*</code>`)
	reDocs     = regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*>\s*(?:Documenta\S+ oficial|Official docs)`)
	reSobre    = regexp.MustCompile(`(?s)<h2[^>]*>\s*(?:Sobre|About) [^<]*</h2>.{0,400}?<p[^>]*>(.*?)</p>`)

	reMarcacao = regexp.MustCompile(`<[^>]+>`)
	reEspacos  = regexp.MustCompile(`\s+`)
)

// moldeListaDefine vira uma expressão por rótulo lido: transporte e autenticação
// vêm no mesmo <dl>, e só o texto do <dt> os separa.
const moldeListaDefine = `(?s)<dt[^>]*>\s*%s\s*</dt>\s*<dd[^>]*>(.*?)</dd>`

// lerIndice tira os servidores da página de listagem.
func lerIndice(pagina string) ([]Item, error) {
	achados := reCartao.FindAllStringSubmatch(pagina, -1)
	visto := make(map[string]bool, len(achados))
	itens := make([]Item, 0, len(achados))
	for _, c := range achados {
		slug := c[1]
		if visto[slug] {
			continue
		}
		textos := reTruncado.FindAllStringSubmatch(c[2], 2)
		if len(textos) == 0 {
			continue
		}
		nome := texto(textos[0][1])
		if nome == "" {
			continue
		}
		var resumo string
		if len(textos) > 1 {
			resumo = texto(textos[1][1])
		}
		visto[slug] = true
		itens = append(itens, Item{Slug: slug, Nome: nome, Resumo: resumo})
	}
	// Zero item numa página que chegou inteira é a marcação tendo mudado. Uma
	// lista vazia devolvida como sucesso viraria "a origem não tem nada", que é
	// a leitura errada e a que faz o admin desconfiar do próprio patchbay.
	if len(itens) == 0 {
		return nil, fmt.Errorf("%w: nenhum servidor na página de listagem", ErrFormatoDaOrigem)
	}
	return itens, nil
}

// lerDetalhe tira da página de um servidor o que o cadastro precisa.
func lerDetalhe(slug, pagina string) (Detalhe, error) {
	d := Detalhe{Item: Item{Slug: slug}}

	if m := reTitulo.FindStringSubmatch(pagina); m != nil {
		d.Nome = texto(m[1])
		d.Resumo = texto(m[2])
	}
	if d.Nome == "" {
		return Detalhe{}, fmt.Errorf("%w: %s sem nome", ErrFormatoDaOrigem, slug)
	}
	if m := reSobre.FindStringSubmatch(pagina); m != nil {
		d.Descricao = texto(m[1])
	}
	if m := reDocs.FindStringSubmatch(pagina); m != nil {
		d.Docs = html.UnescapeString(m[1])
	}

	// A URL sai do bloco de conexão, e a procura começa nele: a página tem outros
	// <code> — os comandos de configuração de cada cliente —, e o primeiro de
	// todos nem sempre é o endereço.
	if i := indiceDaConexao(pagina); i >= 0 {
		if m := reEndereco.FindStringSubmatch(recorte(pagina, i, 4000)); m != nil {
			d.URL = texto(m[1])
		}
	}
	switch {
	case d.URL == "":
		return Detalhe{}, fmt.Errorf("%w: %s sem URL de conexão", ErrFormatoDaOrigem, slug)
	case !strings.HasPrefix(d.URL, "https://"):
		// Endpoint em texto claro carregaria o bearer do upstream pela rede sem
		// cifra. Quem realmente precisar cadastra à mão, de olhos abertos.
		return Detalhe{}, fmt.Errorf("%w: %s com URL sem https (%s)", ErrFormatoDaOrigem, slug, d.URL)
	}

	transporte := campo(pagina, "Transporte", "Transport")
	if transporte == "" {
		return Detalhe{}, fmt.Errorf("%w: %s sem transporte declarado", ErrFormatoDaOrigem, slug)
	}
	if strings.Contains(strings.ToLower(transporte), "sse") {
		d.Transporte = TransporteSSE
	} else {
		d.Transporte = TransporteHTTP
	}

	d.Autenticacao = autenticacaoDe(campo(pagina, "Autentica\\S+", "Authentication"))
	return d, nil
}

func indiceDaConexao(pagina string) int {
	for _, marca := range []string{"Detalhes da conex", "Connection details"} {
		if i := strings.Index(pagina, marca); i >= 0 {
			return i
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

// campo lê um par <dt>rótulo</dt><dd>valor</dd>, tentando os rótulos na ordem
// dada: a origem traduz o <dt> conforme o idioma do caminho, e o inglês fica
// como segunda tentativa para a leitura não depender de qual /pt-BR está em uso.
//
// Os rótulos entram na expressão como fragmento de regex, e não como literal, só
// para "Autentica\S+" cobrir a acentuação sem o arquivo depender de como o
// editor gravou a cedilha. São constantes deste pacote — nada de fora chega aqui.
func campo(pagina string, rotulos ...string) string {
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
