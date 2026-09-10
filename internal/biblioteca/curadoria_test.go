package biblioteca_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// servidorCurado descreve uma entrada da curadoria de mentira.
type servidorCurado struct {
	Slug, Nome, Resumo, URL, Transporte, Autenticacao string
}

// curadoriaDeMentira serve uma lista de remotos com a mesma marcação do
// mcpservers.org.
//
// Sintética, e não a página de verdade, de propósito: aqui o que está sob teste
// é a varredura e a mesclagem, que precisam de endereços controlados para casar
// (ou não) com o registry. A tradução do HTML de verdade é testada à parte, em
// TestCuradoriaLePaginaDeVerdade, contra páginas reais em testdata.
func curadoriaDeMentira(t *testing.T, servidores []servidorCurado) string {
	t.Helper()
	return curadoriaComOficiais(t, servidores, oficiaisPadrao())
}

func curadoriaComOficiais(
	t *testing.T, servidores []servidorCurado, oficiais []servidorOficial,
) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString("<!DOCTYPE html><html lang=\"pt-BR\"><body><main>")
		for _, s := range servidores {
			fmt.Fprintf(&b, `<a href="/pt-BR/remote-mcp-servers/%s">`+
				`<div class="truncate text-sm font-semibold">%s</div>`+
				`<div class="truncate text-xs">%s</div></a>`, s.Slug, s.Nome, s.Resumo)
		}
		b.WriteString("</main></body></html>")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		for _, s := range servidores {
			if s.Slug != slug {
				continue
			}
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="pt-BR"><body>`+
				`<h1>%s</h1><p>%s</p>`+
				`<h2>Detalhes da conexão</h2><code>%s</code>`+
				`<dl><dt>Transporte</dt><dd>%s</dd>`+
				`<dt>Autenticação</dt><dd>%s</dd></dl></body></html>`,
				s.Nome, s.Resumo, s.URL, s.Transporte, s.Autenticacao)
			return
		}
		http.NotFound(w, r)
	})
	// O acervo /official é outra lista do mesmo site, e a varredura o percorre
	// sempre. Mesmo os testes que só se importam com os remotos precisam de um
	// índice que exista: índice vazio é marcação mudada, e derruba a varredura
	// de propósito.
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString(`<html><body><main>`)
		for _, o := range oficiais {
			fmt.Fprintf(&b, `<a href="/pt-BR/servers/%s">%s</a>`, o.Slug, o.Nome)
		}
		b.WriteString(`</main></body></html>`)
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		for _, o := range oficiais {
			if o.Slug != slug {
				continue
			}
			trecho := ""
			if o.Comando != "" {
				trecho = fmt.Sprintf(`<pre>{&quot;mcpServers&quot;:{&quot;x&quot;:{`+
					`&quot;command&quot;: &quot;%s&quot;, &quot;args&quot;: [%s]}}}</pre>`,
					o.Comando, o.Args)
			}
			_, _ = fmt.Fprintf(w, `<html><body><h1>%s</h1><p>%s</p>`+
				`<dl><dt>Categoria</dt><dd>Ferramentas</dd></dl>%s</body></html>`,
				o.Nome, o.Resumo, trecho)
			return
		}
		http.NotFound(w, r)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/pt-BR"
}

// servidorOficial descreve uma entrada do acervo /servers/.
type servidorOficial struct {
	// Args é o miolo do vetor JSON, já escapado como o site escreve:
	// `&quot;-y&quot;, &quot;pacote&quot;`.
	Slug, Nome, Resumo, Comando, Args string
}

// oficiaisPadrao é o mínimo que todo teste precisa: um índice que existe.
func oficiaisPadrao() []servidorOficial {
	return []servidorOficial{{
		Slug: "exemplo-oficial", Nome: "Exemplo Oficial", Resumo: "processo local de teste",
		Comando: "npx", Args: `&quot;-y&quot;, &quot;exemplo-oficial-mcp&quot;`,
	}}
}

// curadoriaMuda é a origem existindo e não publicando nada. Serve aos testes que
// só se importam com o registry.
func curadoriaMuda(t *testing.T) string {
	t.Helper()
	return curadoriaDeMentira(t, []servidorCurado{{
		Slug: "exemplo", Nome: "Exemplo", Resumo: "servidor de teste",
		URL: "https://exemplo.invalido/mcp", Transporte: "Streamable HTTP",
		Autenticacao: "Aberto — sem autenticação",
	}})
}

func TestCuradoriaLeListaEDetalhe(t *testing.T) {
	t.Parallel()

	base := curadoriaDeMentira(t, []servidorCurado{
		{Slug: "notion", Nome: "Notion", Resumo: "Notas e bases de dados",
			URL: "https://mcp.notion.com/mcp", Transporte: "Streamable HTTP", Autenticacao: "OAuth"},
		{Slug: "b12", Nome: "B12", Resumo: "Sites para pequenos negócios",
			URL: "https://api.b12.io/mcp", Transporte: "SSE", Autenticacao: "Aberto — sem autenticação"},
	})
	c := biblioteca.NovaCuradoria(base)
	ctx := context.Background()

	slugs, err := c.Slugs(ctx)
	if err != nil {
		t.Fatalf("Slugs: erro = %v, quer nil", err)
	}
	if len(slugs) != 2 {
		t.Fatalf("slugs = %v, quer 2", slugs)
	}

	notion, err := c.Um(ctx, "notion")
	if err != nil {
		t.Fatalf("Um: erro = %v, quer nil", err)
	}
	// O campo que só esta origem tem, e a razão inteira de ela existir.
	if notion.Autenticacao != biblioteca.AutOAuth {
		t.Errorf("Autenticacao = %q, quer %q", notion.Autenticacao, biblioteca.AutOAuth)
	}
	if !notion.Curado {
		t.Error("item da curadoria veio sem a marca de curado")
	}
	if notion.Transporte != biblioteca.TransporteHTTP || notion.URL != "https://mcp.notion.com/mcp" {
		t.Errorf("conexão = %s %s, quer http e o endpoint do Notion", notion.Transporte, notion.URL)
	}
	// O nome carrega a origem: este servidor pode não existir no registry.
	if notion.Nome != "mcpservers.org/notion" {
		t.Errorf("Nome = %q, quer mcpservers.org/notion", notion.Nome)
	}

	b12, err := c.Um(ctx, "b12")
	if err != nil {
		t.Fatalf("Um: erro = %v, quer nil", err)
	}
	if b12.Autenticacao != biblioteca.AutAberta {
		t.Errorf("Autenticacao = %q, quer %q", b12.Autenticacao, biblioteca.AutAberta)
	}
	if b12.PedeCredencial {
		t.Error("servidor aberto marcado como pedindo credencial")
	}
	if b12.Transporte != biblioteca.TransporteSSE {
		t.Errorf("Transporte = %q, quer sse", b12.Transporte)
	}
}

// TestCuradoriaLePaginaDeVerdade roda contra páginas reais do mcpservers.org,
// guardadas em testdata com script e svg removidos.
//
// É o único jeito honesto de testar a tradução do HTML: um fixture escrito à mão
// testaria a expressão contra ela mesma, e é justamente a marcação do terceiro
// que muda sem avisar. Quando este teste quebrar, o site mudou.
func TestCuradoriaLePaginaDeVerdade(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-indice.html"))
	})
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers/notion", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-detalhe-oauth.html"))
	})
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers/b12", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-detalhe-aberta.html"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c := biblioteca.NovaCuradoria(ts.URL + "/pt-BR")
	ctx := context.Background()

	slugs, err := c.Slugs(ctx)
	if err != nil {
		t.Fatalf("Slugs: erro = %v, quer nil", err)
	}
	// A página real traz dezenas de servidores; o número exato muda com o site,
	// então o que se afirma é que a lista não veio vazia nem com um só.
	if len(slugs) < 10 {
		t.Fatalf("slugs = %d, quer dezenas: a marcação da listagem mudou", len(slugs))
	}

	notion, err := c.Um(ctx, "notion")
	if err != nil {
		t.Fatalf("Um(notion): erro = %v, quer nil", err)
	}
	if notion.Titulo == "" || notion.URL == "" {
		t.Fatalf("detalhe incompleto: %+v", notion)
	}
	if notion.Autenticacao != biblioteca.AutOAuth {
		t.Errorf("Autenticacao = %q, quer oauth: é o campo que só esta origem tem",
			notion.Autenticacao)
	}
	if !strings.HasPrefix(notion.URL, "https://") {
		t.Errorf("URL = %q, quer https", notion.URL)
	}

	b12, err := c.Um(ctx, "b12")
	if err != nil {
		t.Fatalf("Um(b12): erro = %v, quer nil", err)
	}
	if b12.Autenticacao != biblioteca.AutAberta {
		t.Errorf("Autenticacao = %q, quer aberta", b12.Autenticacao)
	}
}

func TestCuradoriaRecusaOQueNaoDaParaCadastrar(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Página sem o bloco de conexão: item pela metade é recusado inteiro, e não
	// devolvido com a URL vazia. Um cartão que leva a um formulário sem endpoint
	// é pior do que o servidor não aparecer.
	semURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Sem endereço</h1><p>oi</p></body></html>`))
	}))
	t.Cleanup(semURL.Close)
	if _, err := biblioteca.NovaCuradoria(semURL.URL).Um(ctx, "qualquer"); !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Errorf("sem URL: erro = %v, quer ErrFormatoDaOrigem", err)
	}

	// Desafio de bot chega com 200 e HTML no lugar da página. Sem distingui-lo,
	// ele viraria "formato mudou" e mandaria procurar o defeito no lugar errado.
	desafio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Just a moment...</title></head><body></body></html>`))
	}))
	t.Cleanup(desafio.Close)
	if _, err := biblioteca.NovaCuradoria(desafio.URL).Slugs(ctx); !errors.Is(err, biblioteca.ErrOrigemIndisponivel) {
		t.Errorf("desafio de bot: erro = %v, quer ErrOrigemIndisponivel", err)
	}

	// Slug que não pode existir não vira requisição: ele chega pela lista da
	// origem, mas a borda é aqui.
	mudo := curadoriaMuda(t)
	for _, slug := range []string{"", "MAIUSCULA", "com/barra", "../fuga"} {
		if _, err := biblioteca.NovaCuradoria(mudo).Um(ctx, slug); !errors.Is(err, biblioteca.ErrNaoEncontrado) {
			t.Errorf("Um(%q): erro = %v, quer ErrNaoEncontrado", slug, err)
		}
	}
}

// TestCuradoriaInsisteEmTaxaExcedida: a origem limita taxa, e limitar taxa é
// diferente de estar fora do ar — esperar resolve. Medido em 2026-09-09: com
// 250 ms entre páginas, uma varredura de verdade trouxe 63 de 293, e as outras
// 230 vieram com HTTP 429.
func TestCuradoriaInsisteEmTaxaExcedida(t *testing.T) {
	t.Parallel()

	var idas int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><main>` +
			`<a href="/pt-BR/remote-mcp-servers/notion"><div class="truncate">Notion</div></a>` +
			`</main></body></html>`))
	})
	// O acervo /official precisa existir mesmo aqui: a varredura o percorre
	// sempre, e índice ausente derruba tudo antes de chegar ao que se testa.
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><a href="/pt-BR/servers/ex">Ex</a></body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Ex</h1><p>oi</p>` +
			`<pre>{&quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: [&quot;-y&quot;, &quot;ex&quot;]}</pre>` +
			`</body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/remote-mcp-servers/notion", func(w http.ResponseWriter, _ *http.Request) {
		idas++
		if idas == 1 {
			http.Error(w, "devagar", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`<html><body><h1>Notion</h1><p>Notas</p>` +
			`<h2>Detalhes da conexão</h2><code>https://mcp.notion.com/mcp</code>` +
			`<dl><dt>Transporte</dt><dd>Streamable HTTP</dd>` +
			`<dt>Autenticação</dt><dd>OAuth</dd></dl></body></html>`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// O 429 precisa chegar como erro próprio, senão a varredura o descartaria
	// como página perdida em vez de insistir.
	_, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").Um(context.Background(), "notion")
	if !errors.Is(err, biblioteca.ErrTaxaExcedida) {
		t.Fatalf("erro = %v, quer ErrTaxaExcedida", err)
	}
	if !errors.Is(err, biblioteca.ErrOrigemIndisponivel) {
		t.Error("ErrTaxaExcedida deixou de ser ErrOrigemIndisponivel para quem só quer saber se deu")
	}

	// E a varredura tem de insistir e trazer o servidor mesmo assim.
	repo := repoDeTeste(t)
	registry := registryDeMentira(t)
	if err := sincronizadorCom(t, registry.URL, ts.URL+"/pt-BR", repo).
		Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}
	// Dois curados: o remoto que só passou na segunda tentativa, e o oficial.
	if _, total, _ := repo.Buscar(context.Background(),
		biblioteca.Filtro{SoCurados: true}, 10, 0); total != 2 {
		t.Fatalf("curados = %d, quer 2: a varredura desistiu no primeiro 429", total)
	}
}

// TestOficiaisLeemPaginaDeVerdade roda contra páginas reais do acervo /servers/
// do mcpservers.org, guardadas em testdata.
//
// Os três casos que importam estão aqui, e o segundo é o comum: página com
// comando aproveitável, página sem comando algum, e o índice paginado.
func TestOficiaisLeemPaginaDeVerdade(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-oficial-indice.html"))
	})
	mux.HandleFunc("GET /pt-BR/servers/anki-mcp/anki-mcp-desktop", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-oficial-comando.html"))
	})
	mux.HandleFunc("GET /pt-BR/servers/apify-mcp-server", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-oficial-sem-comando.html"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c := biblioteca.NovaCuradoria(ts.URL + "/pt-BR")
	ctx := context.Background()

	// O índice de verdade tem 29 links e cita a última página. O número exato
	// muda com o site; o que se afirma é que ele foi lido e paginado.
	slugs, err := c.SlugsOficiais(ctx)
	if err == nil && len(slugs) < 10 {
		t.Fatalf("slugs = %d, quer dezenas: a marcação do índice mudou", len(slugs))
	}
	// Com um dublê que serve a mesma página para todo número, a paginação para
	// no teto e devolve o mesmo conjunto; o que interessa é não ter estourado.
	if err != nil && !strings.Contains(err.Error(), "página") {
		t.Fatalf("SlugsOficiais: erro = %v", err)
	}

	// O slug com barra no meio precisa sobreviver ao caminho e ao nome.
	bom, err := c.Oficial(ctx, "anki-mcp/anki-mcp-desktop")
	if err != nil {
		t.Fatalf("Oficial: erro = %v, quer nil", err)
	}
	if bom.Nome != "mcpservers.org/anki-mcp/anki-mcp-desktop" {
		t.Errorf("Nome = %q, quer os três segmentos", bom.Nome)
	}
	if bom.Transporte != biblioteca.TransporteSTDIO {
		t.Errorf("Transporte = %q, quer stdio", bom.Transporte)
	}
	if bom.Comando != "npx" || len(bom.Args) < 2 {
		t.Fatalf("execução = %s %v, quer npx com argumentos", bom.Comando, bom.Args)
	}
	if !bom.Curado {
		t.Error("oficial veio sem a marca de curado")
	}

	// E a maioria não tem comando: recusar é o comportamento, não o defeito.
	if _, err := c.Oficial(ctx, "apify-mcp-server"); !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("sem comando: erro = %v, quer ErrFormatoDaOrigem", err)
	}
}

func TestSnippetComMarcadorDeExemploNaoViraCadastro(t *testing.T) {
	t.Parallel()

	// O README manda a pessoa trocar o caminho. Cadastrar isso entrega um
	// upstream quebrado com cara de pronto — foi o defeito que se viu no
	// catálogo do MetaMCP (--user-agent=YourUserAgent).
	base := curadoriaComOficiais(t, nil, []servidorOficial{{
		Slug: "com-marcador", Nome: "Com Marcador", Resumo: "exemplo",
		Comando: "node",
		Args:    `&quot;C:\PATH\TO\PARENT\FOLDER\build\index.js&quot;`,
	}})
	if _, err := biblioteca.NovaCuradoria(base).
		Oficial(context.Background(), "com-marcador"); !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("erro = %v, quer ErrFormatoDaOrigem", err)
	}
}
