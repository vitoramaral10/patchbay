// Comando patchbay-biblioteca regenera o catálogo embutido de servidores MCP
// remotos (internal/biblioteca/catalogo.json) a partir do mcpservers.org.
//
// Não roda em produção e não é distribuído: é ferramenta de manutenção, e o
// artefato dela é um arquivo versionado. Existe para que o catálogo não seja um
// blob que ninguém sabe refazer — a origem, o recorte e a tradução dos campos
// ficam neste arquivo, e a atualização é `task biblioteca` seguido do diff.
//
// A origem é a lista de servidores *remotos* (o sitemap remote-mcp-servers), e
// não o diretório inteiro do site. É um recorte deliberado: aquelas páginas
// declaram endpoint, transporte e forma de autenticação em campos próprios, que
// é exatamente o que o formulário de upstream precisa. As páginas de servidor
// local são prosa de README, de onde um comando executável só sairia por
// adivinhação — e um comando adivinhado vira um processo filho que não sobe.
//
// Sobre o Cloudflare: o site responde a desafio de bot sob rajada. Por isso o
// laço é serial, com pausa que dobra a cada recusa, e por isso existe -cache:
// cada página baixada fica em disco, e uma segunda execução só busca o que
// faltou. Rodar duas ou três vezes até `faltam 0` é o fluxo normal, não sinal de
// defeito.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

const (
	sitemap = "https://mcpservers.org/sitemaps/remote-mcp-servers.xml"
	base    = "https://mcpservers.org/remote-mcp-servers/"
	// agente é um User-Agent de navegador porque o site recusa os demais. Não é
	// disfarce: o robots.txt libera /remote-mcp-servers, e o que se busca aqui é
	// a mesma página que um leitor humano abre.
	agente = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

func main() {
	saida := flag.String("saida", filepath.Join("internal", "biblioteca", "catalogo.json"),
		"arquivo do catálogo a escrever")
	cache := flag.String("cache", "", "diretório de cache das páginas HTML (vazio desliga o cache)")
	pausa := flag.Duration("pausa", 700*time.Millisecond, "pausa inicial entre requisições")
	limite := flag.Int("limite", 0, "processa no máximo N servidores (0 = todos); para conferir o formato")
	flag.Parse()

	if err := executar(*saida, *cache, *pausa, *limite); err != nil {
		log.Fatalf("patchbay-biblioteca: %v", err)
	}
}

func executar(saida, cache string, pausa time.Duration, limite int) error {
	c := &coletor{
		cliente: &http.Client{Timeout: 30 * time.Second},
		cache:   cache,
		pausa:   pausa,
	}
	if cache != "" {
		if err := os.MkdirAll(cache, 0o750); err != nil {
			return fmt.Errorf("cache: %w", err)
		}
	}

	indice, err := c.buscar(sitemap, "", sitemapValido)
	if err != nil {
		return fmt.Errorf("sitemap: %w", err)
	}
	slugs := slugsDoSitemap(indice)
	if len(slugs) == 0 {
		return errors.New("sitemap sem nenhum servidor: o formato da origem mudou")
	}
	if limite > 0 && limite < len(slugs) {
		slugs = slugs[:limite]
	}
	log.Printf("%d servidores no sitemap", len(slugs))

	itens := make([]biblioteca.Item, 0, len(slugs))
	var faltaram []string
	for n, slug := range slugs {
		pagina, err := c.buscar(base+slug, slug, paginaValida)
		if err != nil {
			faltaram = append(faltaram, slug)
			continue
		}
		item, err := extrair(slug, pagina)
		if err != nil {
			faltaram = append(faltaram, slug)
			log.Printf("%s: %v", slug, err)
			continue
		}
		itens = append(itens, item)
		if (n+1)%25 == 0 {
			log.Printf("%d/%d lidos, %d faltaram", n+1, len(slugs), len(faltaram))
		}
	}

	// Falha parcial não sobrescreve o catálogo com uma versão menor: o que
	// caiu foi rede, não o servidor sumindo da origem, e gravar assim apagaria
	// itens bons do arquivo versionado. Rode de novo com o mesmo -cache.
	if len(faltaram) > 0 {
		log.Printf("faltaram %d: %s", len(faltaram), strings.Join(faltaram, " "))
		if cache == "" {
			return errors.New("houve falha e não há -cache: nada foi gravado; rode com -cache para poder continuar de onde parou")
		}
		return fmt.Errorf("faltaram %d servidores; nada foi gravado — rode de novo com o mesmo -cache", len(faltaram))
	}

	sort.Slice(itens, func(a, b int) bool { return itens[a].Slug < itens[b].Slug })
	// Passa pelo carregador do próprio pacote antes de gravar: se o catálogo não
	// carrega, ele não vira arquivo. É o mesmo código que roda no boot.
	bruto, err := json.MarshalIndent(itens, "", "  ")
	if err != nil {
		return err
	}
	if _, err := biblioteca.Carregar(bruto); err != nil {
		return err
	}
	if err := os.WriteFile(saida, append(bruto, '\n'), 0o600); err != nil {
		return err
	}
	log.Printf("%d servidores gravados em %s", len(itens), saida)
	return nil
}

// coletor busca páginas com pausa adaptativa e cache em disco.
type coletor struct {
	cliente *http.Client
	cache   string
	pausa   time.Duration
}

// buscar devolve o corpo da URL, do cache quando houver.
//
// chave vazia desliga o cache para aquela busca (é o caso do sitemap, que muda
// a cada atualização da origem e cujo cache só serviria para mascarar servidor
// novo).
//
// valida separa a resposta boa do desafio de bot, que chega com 200 e corpo de
// HTML. Ele é parâmetro porque o sitemap é XML e a página é HTML: uma única
// regra recusaria um dos dois.
func (c *coletor) buscar(url, chave string, valida func(string) bool) (string, error) {
	arquivo := ""
	if c.cache != "" && chave != "" {
		arquivo = filepath.Join(c.cache, chave+".html")
		//nolint:gosec // o caminho é o -cache que o operador escolheu, mais o slug
		// que veio do sitemap da origem; não há entrada de terceiro no meio.
		if b, err := os.ReadFile(arquivo); err == nil && valida(string(b)) {
			return string(b), nil
		}
	}

	var ultimo error
	for tentativa := range 4 {
		if tentativa > 0 || chave != "" {
			// Jitter para não bater sempre no mesmo intervalo: rajada regular é
			// o que o desafio de bot reconhece primeiro.
			//
			//nolint:gosec // é espaçamento de requisição, não segredo: previsível
			// aqui não é fraqueza.
			time.Sleep(c.pausa + rand.N(400*time.Millisecond))
		}
		corpo, err := c.pegar(url)
		switch {
		case err != nil:
			ultimo = err
		case !valida(corpo):
			ultimo = errors.New("desafio de bot em vez da página")
		default:
			if c.pausa > 700*time.Millisecond {
				c.pausa = max(700*time.Millisecond, c.pausa*9/10)
			}
			if arquivo != "" {
				if err := os.WriteFile(arquivo, []byte(corpo), 0o600); err != nil {
					return "", err
				}
			}
			return corpo, nil
		}
		c.pausa = min(20*time.Second, c.pausa*2)
	}
	return "", ultimo
}

func (c *coletor) pegar(url string) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", agente)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://mcpservers.org/remote-mcp-servers")

	resp, err := c.cliente.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	corpo, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(corpo), nil
}

// desafioDeBot reconhece a interstitial do Cloudflare, que chega com 200 e
// corpo de HTML no lugar do que se pediu.
func desafioDeBot(s string) bool {
	return len(s) < 600 || strings.Contains(s[:600], "Just a moment")
}

// paginaValida aceita a página de um servidor: HTML com título.
func paginaValida(s string) bool {
	return !desafioDeBot(s) && strings.Contains(s, "<h1")
}

// sitemapValido aceita o índice: XML com pelo menos uma entrada.
func sitemapValido(s string) bool {
	return !desafioDeBot(s) && strings.Contains(s, "<loc>")
}

var (
	reLoc      = regexp.MustCompile(`<loc>\s*https://mcpservers\.org/remote-mcp-servers/([a-z0-9][a-z0-9-]*)\s*</loc>`)
	reTitulo   = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>\s*(?:<p[^>]*>(.*?)</p>)?`)
	reEndereco = regexp.MustCompile(`(?s)<code[^>]*>\s*(https?://[^<\s]+)\s*</code>`)
	reDocs     = regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*>\s*Official docs`)
	reSobre    = regexp.MustCompile(`(?s)<h2[^>]*>\s*About [^<]*</h2>.{0,400}?<p[^>]*>(.*?)</p>`)
	reMarcacao = regexp.MustCompile(`<[^>]+>`)
	reEspacos  = regexp.MustCompile(`\s+`)
)

// moldeListaDefine vira uma expressão por rótulo lido: os campos "Transport" e
// "Authentication" vêm no mesmo <dl>, e só o texto do <dt> os separa.
const moldeListaDefine = `(?s)<dt[^>]*>\s*%s\s*</dt>\s*<dd[^>]*>(.*?)</dd>`

// slugsDoSitemap tira os slugs únicos do sitemap.
//
// O sitemap repete cada servidor em uma dezena de idiomas (/ja/, /de/...); a
// expressão só casa o caminho sem prefixo de idioma, então o mesmo servidor
// entra uma vez só.
func slugsDoSitemap(xml string) []string {
	visto := map[string]bool{}
	var slugs []string
	for _, m := range reLoc.FindAllStringSubmatch(xml, -1) {
		if visto[m[1]] {
			continue
		}
		visto[m[1]] = true
		slugs = append(slugs, m[1])
	}
	sort.Strings(slugs)
	return slugs
}

// extrair traduz a página no item do catálogo.
//
// Recusa a página em vez de completar com padrão quando falta URL ou
// transporte: item pela metade vira um botão "adicionar" que abre um formulário
// vazio, e o admin descobre isso depois do clique.
func extrair(slug, pagina string) (biblioteca.Item, error) {
	item := biblioteca.Item{Slug: slug, Fonte: base + slug}

	if m := reTitulo.FindStringSubmatch(pagina); m != nil {
		item.Nome = texto(m[1])
		item.Resumo = texto(m[2])
	}
	if item.Nome == "" {
		return item, errors.New("sem nome")
	}
	if m := reSobre.FindStringSubmatch(pagina); m != nil {
		item.Descricao = texto(m[1])
	}
	if m := reDocs.FindStringSubmatch(pagina); m != nil {
		item.Docs = html.UnescapeString(m[1])
	}

	// A URL sai do bloco "Connection details", e a busca começa nele: a página
	// tem outros <code> (os comandos de setup de cada cliente), e o primeiro de
	// todos nem sempre é o endereço.
	if i := strings.Index(pagina, "Connection details"); i >= 0 {
		if m := reEndereco.FindStringSubmatch(recorte(pagina, i, 4000)); m != nil {
			item.URL = texto(m[1])
		}
	}
	if item.URL == "" {
		return item, errors.New("sem URL de conexão")
	}
	// Só https entra no catálogo. Um endpoint em texto claro carregaria o
	// bearer do upstream pela rede sem cifra, e a biblioteca não é lugar de
	// oferecer isso com um clique — quem realmente precisar cadastra à mão.
	if !strings.HasPrefix(item.URL, "https://") {
		return item, fmt.Errorf("URL sem https: %q", item.URL)
	}

	transporte := campo(pagina, "Transport")
	switch {
	case transporte == "":
		return item, errors.New("sem transporte declarado")
	case strings.Contains(strings.ToLower(transporte), "sse"):
		item.Transporte = biblioteca.TransporteSSE
	default:
		item.Transporte = biblioteca.TransporteHTTP
	}

	item.Autenticacao = autenticacaoDe(campo(pagina, "Authentication"))

	return item, nil
}

// autenticacaoDe traduz a frase da origem numa das três formas conhecidas.
//
// O padrão é token, e não aberta: errar para "precisa de token" faz a tela pedir
// uma credencial que talvez não seja necessária — chato. Errar para "aberta"
// faz o admin cadastrar sem credencial um servidor que exige uma, e o upstream
// nasce degradado com 401. Entre os dois erros, o primeiro é o barato.
func autenticacaoDe(frase string) string {
	f := strings.ToLower(frase)
	switch {
	case strings.Contains(f, "oauth"):
		return biblioteca.AutOAuth
	case strings.Contains(f, "no auth"), strings.Contains(f, "open"), strings.Contains(f, "none"):
		return biblioteca.AutAberta
	default:
		return biblioteca.AutToken
	}
}

// campo lê um par <dt>rótulo</dt><dd>valor</dd>.
func campo(pagina, rotulo string) string {
	re := regexp.MustCompile(fmt.Sprintf(moldeListaDefine, regexp.QuoteMeta(rotulo)))
	m := re.FindStringSubmatch(pagina)
	if m == nil {
		return ""
	}
	return texto(m[1])
}

// texto tira marcação, resolve entidade e normaliza espaço.
func texto(s string) string {
	s = reMarcacao.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(reEspacos.ReplaceAllString(s, " "))
}

func recorte(s string, de, tamanho int) string {
	ate := min(de+tamanho, len(s))
	return s[de:ate]
}
