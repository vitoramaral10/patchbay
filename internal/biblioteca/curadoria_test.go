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

// curadoriaDeMentira serve o acervo oficial do mcpservers.org com a mesma
// marcação do site de verdade.
//
// Sintética, e não a página de verdade, de propósito: aqui o que está sob teste
// é a varredura e a mesclagem, que precisam de endereços controlados. A
// tradução do HTML de verdade é testada à parte, em
// TestOficiaisLeemPaginaDeVerdade, contra páginas reais em testdata. oficiais
// vazio usa oficiaisPadrao — o mínimo que toda varredura precisa: um índice que
// existe.
func curadoriaDeMentira(t *testing.T, oficiais []servidorOficial) string {
	t.Helper()
	if len(oficiais) == 0 {
		oficiais = oficiaisPadrao()
	}
	return curadoriaComOficiais(t, oficiais)
}

func curadoriaComOficiais(t *testing.T, oficiais []servidorOficial) string {
	t.Helper()

	mux := http.NewServeMux()
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

// curadoriaMuda é uma origem de mentira mínima, só para o sincronizador ter
// para onde ir. Serve aos testes que não se importam com o que a varredura
// traz.
func curadoriaMuda(t *testing.T) string {
	t.Helper()
	return curadoriaDeMentira(t, nil)
}

// TestCuradoriaInsisteEmTaxaExcedida: a origem limita taxa, e limitar taxa é
// diferente de estar fora do ar — esperar resolve. Medido em 2026-09-09: com
// 250 ms entre páginas, uma varredura de verdade trouxe 63 de 293, e as outras
// 230 vieram com HTTP 429.
func TestCuradoriaInsisteEmTaxaExcedida(t *testing.T) {
	t.Parallel()

	var idas int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><a href="/pt-BR/servers/ex">Ex</a></body></html>`))
	})
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
		idas++
		if idas == 1 {
			http.Error(w, "devagar", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`<html><body><h1>Ex</h1><p>oi</p>` +
			`<pre>{&quot;command&quot;: &quot;npx&quot;, &quot;args&quot;: [&quot;-y&quot;, &quot;ex&quot;]}</pre>` +
			`</body></html>`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// O 429 precisa chegar como erro próprio, senão a varredura o descartaria
	// como página perdida em vez de insistir.
	_, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").Oficial(context.Background(), "ex")
	if !errors.Is(err, biblioteca.ErrTaxaExcedida) {
		t.Fatalf("erro = %v, quer ErrTaxaExcedida", err)
	}
	if !errors.Is(err, biblioteca.ErrOrigemIndisponivel) {
		t.Error("ErrTaxaExcedida deixou de ser ErrOrigemIndisponivel para quem só quer saber se deu")
	}

	// E a varredura tem de insistir e trazer o servidor mesmo assim: zera a
	// contagem para o próximo 429 acontecer dentro da própria tentativa da
	// rotina, e não antes dela.
	idas = 0
	repo := repoDeTeste(t)
	if err := sincronizadorDeTeste(t, ts.URL+"/pt-BR", repo).
		Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}
	// Um item: o único oficial que o índice publica.
	if _, total, _ := repo.Buscar(context.Background(),
		biblioteca.Filtro{}, 10, 0); total != 1 {
		t.Fatalf("itens = %d, quer 1: a varredura desistiu no primeiro 429", total)
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
	// E a maioria não tem comando: o item entra assim mesmo, com nome,
	// descrição e site — recusar deixou de ser o comportamento (D-03).
	semComando, err := c.Oficial(ctx, "apify-mcp-server")
	if err != nil {
		t.Fatalf("sem comando: erro = %v, quer nil", err)
	}
	if semComando.Comando != "" {
		t.Errorf("Comando = %q, quer vazio", semComando.Comando)
	}
	if semComando.Titulo == "" || semComando.Descricao == "" {
		t.Errorf("item sem comando incompleto: titulo=%q descricao=%q",
			semComando.Titulo, semComando.Descricao)
	}
	if semComando.Site == "" {
		t.Error("item sem comando sem site")
	}
}

// TestOficialSemComandoEntraComSite prova o valor exato que a página real do
// Apify produz: nome, descrição e site preenchidos, comando vazio, sem erro.
func TestOficialSemComandoEntraComSite(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/servers/apify-mcp-server", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-oficial-sem-comando.html"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").Oficial(context.Background(), "apify-mcp-server")
	if err != nil {
		t.Fatalf("Oficial: erro = %v, quer nil", err)
	}
	if item.Nome != "mcpservers.org/apify-mcp-server" {
		t.Errorf("Nome = %q, quer mcpservers.org/apify-mcp-server", item.Nome)
	}
	if item.Comando != "" {
		t.Errorf("Comando = %q, quer vazio: a página não tem snippet", item.Comando)
	}
	if len(item.Args) != 0 {
		t.Errorf("Args = %v, quer vazio", item.Args)
	}
	if item.Transporte != biblioteca.TransporteSTDIO {
		t.Errorf("Transporte = %q, quer stdio mesmo sem comando", item.Transporte)
	}
	if item.Descricao == "" {
		t.Error("Descricao vazia, quer o resumo da página")
	}
	const site = "https://github.com/apify/apify-mcp-server"
	if item.Site != site {
		t.Errorf("Site = %q, quer %q", item.Site, site)
	}
}

func TestSnippetComMarcadorDeExemploNaoViraCadastro(t *testing.T) {
	t.Parallel()

	// O README manda a pessoa trocar o caminho. Cadastrar isso entrega um
	// upstream quebrado com cara de pronto — foi o defeito que se viu no
	// catálogo do MetaMCP (--user-agent=YourUserAgent). O item ainda entra no
	// catálogo (D-03), só que sem comando.
	base := curadoriaComOficiais(t, []servidorOficial{{
		Slug: "com-marcador", Nome: "Com Marcador", Resumo: "exemplo",
		Comando: "node",
		Args:    `&quot;C:\PATH\TO\PARENT\FOLDER\build\index.js&quot;`,
	}})
	item, err := biblioteca.NovaCuradoria(base).Oficial(context.Background(), "com-marcador")
	if err != nil {
		t.Fatalf("Oficial: erro = %v, quer nil", err)
	}
	if item.Comando != "" {
		t.Errorf("Comando = %q, quer vazio: caminho de exemplo não é comando aproveitável", item.Comando)
	}
	if len(item.Args) != 0 {
		t.Errorf("Args = %v, quer vazio", item.Args)
	}
	if item.Titulo == "" {
		t.Error("item sem comando ainda deve trazer nome")
	}
}

// paginaDeDetalhe serve um único HTML em /pt-BR/servers/<qualquer> e devolve a
// base da curadoria. É o dublê dos casos que precisam de uma marcação
// específica, sem o formato fixo de curadoriaComOficiais.
func paginaDeDetalhe(t *testing.T, html string) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(html))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/pt-BR"
}

// TestOficialRemotoViraItemHTTP: o oficial que publica endpoint de MCP entra
// como remoto, e não como o processo local que o acervo /servers/ costuma ser
// (D-02, CA-06).
//
// As duas páginas são reais e cobrem as duas formas em que a origem escreve o
// endereço: no meio da descrição ("Conecte-se em https://…") e na tabela do
// README, atrás de um <strong>Endpoint</strong>.
func TestOficialRemotoViraItemHTTP(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome, slug, arquivo, url string
	}{{
		nome: "URL na descrição", slug: "admake-ai-mcp",
		arquivo: "mcpservers-oficial-remoto.html", url: "https://admakeai.com/api/mcp",
	}, {
		nome: "endpoint na tabela do README", slug: "ansvar-systems/ansvar-gateway",
		arquivo: "mcpservers-oficial-remoto-tabela.html", url: "https://gateway.ansvar.eu/mcp",
	}}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(amostra(t, c.arquivo))
			})
			ts := httptest.NewServer(mux)
			t.Cleanup(ts.Close)

			item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").Oficial(context.Background(), c.slug)
			if err != nil {
				t.Fatalf("Oficial: erro = %v, quer nil", err)
			}
			if item.Transporte != biblioteca.TransporteHTTP {
				t.Errorf("Transporte = %q, quer http", item.Transporte)
			}
			if item.URL != c.url {
				t.Errorf("URL = %q, quer %q", item.URL, c.url)
			}
			if !item.Remoto() {
				t.Error("item remoto não se reconhece como remoto")
			}
			if item.Comando != "" || len(item.Args) != 0 {
				t.Errorf("execução = %q %v, quer vazia num remoto", item.Comando, item.Args)
			}
			// As duas páginas dizem OAuth na mesma região em que publicam o
			// endereço, e é disso que o formulário de upstream precisa para já
			// vir com o fluxo de consentimento marcado.
			if item.Autenticacao != biblioteca.AutOAuth {
				t.Errorf("Autenticacao = %q, quer oauth", item.Autenticacao)
			}
			if !item.PedeCredencial {
				t.Error("remoto com OAuth entrou sem pedir credencial")
			}
		})
	}

	t.Run("snippet de comando não sobrevive ao remoto", func(t *testing.T) {
		t.Parallel()

		base := curadoriaComOficiais(t, []servidorOficial{{
			Slug: "remoto-com-snippet", Nome: "Remoto Com Snippet",
			Resumo:  "Servidor MCP remoto (HTTP streamable). Conecte-se em https://exemplo.test/api/mcp",
			Comando: "npx", Args: `&quot;-y&quot;, &quot;exemplo-mcp&quot;`,
		}})
		item, err := biblioteca.NovaCuradoria(base).Oficial(context.Background(), "remoto-com-snippet")
		if err != nil {
			t.Fatalf("Oficial: erro = %v, quer nil", err)
		}
		if item.URL != "https://exemplo.test/api/mcp" {
			t.Errorf("URL = %q, quer o endereço da descrição", item.URL)
		}
		if item.Comando != "" || len(item.Args) != 0 {
			t.Errorf("execução = %q %v, quer vazia: no remoto quem conecta é a URL",
				item.Comando, item.Args)
		}
	})
}

// TestURLDeSiteNaoViraConexao guarda o outro lado do D-02: nem toda URL numa
// página de servidor é endereço de conexão (CA-06).
func TestURLDeSiteNaoViraConexao(t *testing.T) {
	t.Parallel()

	t.Run("página real cuja única URL é o repositório", func(t *testing.T) {
		t.Parallel()

		// O Anki é processo local: a página fala de OAuth (o login do túnel) e
		// cita endpoint, mas o único endereço https que ela publica é o
		// repositório do fornecedor.
		mux := http.NewServeMux()
		mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(amostra(t, "mcpservers-oficial-comando.html"))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").
			Oficial(context.Background(), "anki-mcp/anki-mcp-desktop")
		if err != nil {
			t.Fatalf("Oficial: erro = %v, quer nil", err)
		}
		if item.Transporte != biblioteca.TransporteSTDIO {
			t.Errorf("Transporte = %q, quer stdio", item.Transporte)
		}
		if item.URL != "" {
			t.Errorf("URL = %q, quer vazia: a página não publica endpoint de MCP", item.URL)
		}
		if item.Site == "" {
			t.Error("Site vazio: o repositório continua sendo o link da tela")
		}
		// Autenticação é campo de remoto. Marcar um processo local como OAuth
		// faria o formulário abrir um consentimento que não existe.
		if item.Autenticacao != "" {
			t.Errorf("Autenticacao = %q, quer vazia num item local", item.Autenticacao)
		}
	})

	t.Run("endpoint que a própria página declara removido", func(t *testing.T) {
		t.Parallel()

		// O Apify: a única URL com caminho de MCP na região é o endpoint SSE
		// legado, citado justamente na frase que anuncia a remoção dele. O
		// endereço vivo (https://mcp.apify.com) não tem caminho de MCP e a
		// regra não o enxerga — o item entra sem URL, que é o erro barato.
		// Cadastrar o legado seria entregar um upstream morto com cara de
		// pronto.
		mux := http.NewServeMux()
		mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(amostra(t, "mcpservers-oficial-sem-comando.html"))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").
			Oficial(context.Background(), "apify-mcp-server")
		if err != nil {
			t.Fatalf("Oficial: erro = %v, quer nil", err)
		}
		if item.Transporte != biblioteca.TransporteSTDIO {
			t.Errorf("Transporte = %q, quer stdio: o endpoint citado foi removido", item.Transporte)
		}
		if item.URL != "" {
			t.Errorf("URL = %q, quer vazia: esse endereço não atende mais", item.URL)
		}
		if item.Comando != "" {
			t.Errorf("Comando = %q, quer vazio", item.Comando)
		}
	})

	t.Run("endereço de MCP dentro de configuração de exemplo", func(t *testing.T) {
		t.Parallel()

		// A forma do Airtable e do 21st.dev Magic: servidor local cujo README
		// mostra um bloco de configuração com uma URL de MCP dentro. Sem sinal
		// de remoto na descrição e sem rótulo de conexão, ela não é endereço.
		base := curadoriaComOficiais(t, []servidorOficial{{
			Slug: "local-com-exemplo", Nome: "Local Com Exemplo",
			Resumo: `Servidor MCP oficial do Exemplo. Configuração: url = "https://mcp.exemplo.test/mcp"`,
		}})
		item, err := biblioteca.NovaCuradoria(base).Oficial(context.Background(), "local-com-exemplo")
		if err != nil {
			t.Fatalf("Oficial: erro = %v, quer nil", err)
		}
		if item.Transporte != biblioteca.TransporteSTDIO || item.URL != "" {
			t.Errorf("transporte/URL = %q/%q, quer stdio sem endereço", item.Transporte, item.URL)
		}
	})

	t.Run("endpoint de servidor relacionado", func(t *testing.T) {
		t.Parallel()

		// O bloco "Servidores relacionados" do rodapé descreve outros
		// servidores, muitos deles remotos e com endereço rotulado. A leitura
		// para no primeiro link para outro servidor do acervo — sem isso, o
		// endereço do vizinho viraria o endereço deste (foi o que a página do
		// 1Password mostrou, com mcp.dexi.net/mcp).
		base := paginaDeDetalhe(t, `<html><body><h1>Vizinho Curioso</h1>`+
			`<p>Servidor MCP remoto para exemplo, sem endereço publicado aqui.</p>`+
			`<section><h2>Servidores relacionados</h2>`+
			`<a href="/pt-BR/servers/outro/exemplo" target="_blank">Outro</a>`+
			`<p>Endpoint: https://outro.test/mcp</p></section></body></html>`)
		item, err := biblioteca.NovaCuradoria(base).Oficial(context.Background(), "vizinho-curioso")
		if err != nil {
			t.Fatalf("Oficial: erro = %v, quer nil", err)
		}
		if item.URL != "" {
			t.Errorf("URL = %q, quer vazia: o endereço é do servidor vizinho", item.URL)
		}
		if item.Transporte != biblioteca.TransporteSTDIO {
			t.Errorf("Transporte = %q, quer stdio", item.Transporte)
		}
	})

	// Tabela na região não é, por si, tabela de endpoints: quem ancora é o nome
	// da coluna. Estas duas páginas reais (baixadas em 2026-09-11) publicam
	// tabela de ferramentas e continuam sendo processo local — é a
	// não-regressão de D-02 emenda 2. Uma implementação que tratasse "há tabela
	// na região" como licença para varrer a região inteira derruba o caso do
	// Magic: ele cita https://21st.dev/mcp fora de qualquer tabela.
	locaisComTabela := []struct{ nome, slug, arquivo, comando string }{{
		nome: "tabela de ferramentas do 1Password", slug: "1password-mcp",
		arquivo: "mcpservers-oficial-local-tabela-1password.html", comando: "1password-mcp",
	}, {
		// A região do Magic cita https://21st.dev/mcp — a página de
		// configuração da CLI nova, fora de qualquer tabela. A tabela dele
		// mapeia nome velho para nome novo de ferramenta.
		nome: "tabela de equivalência do Magic", slug: "21st-dev/magic-mcp",
		arquivo: "mcpservers-oficial-local-tabela-magic.html",
	}}
	for _, c := range locaisComTabela {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(amostra(t, c.arquivo))
			})
			ts := httptest.NewServer(mux)
			t.Cleanup(ts.Close)

			item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").Oficial(context.Background(), c.slug)
			if err != nil {
				t.Fatalf("Oficial: erro = %v, quer nil", err)
			}
			if item.Transporte != biblioteca.TransporteSTDIO {
				t.Errorf("Transporte = %q, quer stdio: a tabela é de ferramentas", item.Transporte)
			}
			if item.URL != "" {
				t.Errorf("URL = %q, quer vazia", item.URL)
			}
			if len(item.Endpoints) != 0 {
				t.Errorf("Endpoints = %v, quer vazio", item.Endpoints)
			}
			if item.Comando != c.comando {
				t.Errorf("Comando = %q, quer %q", item.Comando, c.comando)
			}
		})
	}
}

// TestTabelaDeEndpointsViraRemoto: a página que publica os endereços numa
// tabela de endpoints entra como remoto, com todos eles guardados (D-02
// emenda 2, D-07, CA-13).
//
// A fixture é a página real da Cloudflare de 2026-09-11: 17 linhas de
// "Nome do Servidor | Descrição | URL do Servidor", sem rótulo de conexão em
// lugar nenhum e sem endereço citado na descrição — nada que a regra anterior
// enxergasse. A coluna de nome traz links para github.com/cloudflare/mcp…, que
// não são endpoint.
func TestTabelaDeEndpointsViraRemoto(t *testing.T) {
	t.Parallel()

	queridos := []string{
		"https://mcp.cloudflare.com/mcp",
		"https://docs.mcp.cloudflare.com/mcp",
		"https://bindings.mcp.cloudflare.com/mcp",
		"https://builds.mcp.cloudflare.com/mcp",
		"https://observability.mcp.cloudflare.com/mcp",
		"https://containers.mcp.cloudflare.com/mcp",
		"https://browser.mcp.cloudflare.com/mcp",
		"https://logs.mcp.cloudflare.com/mcp",
		"https://ai-gateway.mcp.cloudflare.com/mcp",
		"https://autorag.mcp.cloudflare.com/mcp",
		"https://auditlogs.mcp.cloudflare.com/mcp",
		"https://dns-analytics.mcp.cloudflare.com/mcp",
		"https://dex.mcp.cloudflare.com/mcp",
		"https://casb.mcp.cloudflare.com/mcp",
		"https://radar.mcp.cloudflare.com/mcp",
		"https://blog.mcp.cloudflare.com/mcp",
		"https://demo-day.mcp.cloudflare.com/mcp",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-oficial-tabela-endpoints.html"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").
		Oficial(context.Background(), "cloudflare/mcp-server-cloudflare")
	if err != nil {
		t.Fatalf("Oficial: erro = %v, quer nil", err)
	}
	if item.Transporte != biblioteca.TransporteHTTP {
		t.Errorf("Transporte = %q, quer http", item.Transporte)
	}
	// A primeira linha da tabela é a marcada "(recomendado)", e é ela que vira
	// a URL do item — não a primeira por acaso.
	if item.URL != queridos[0] {
		t.Errorf("URL = %q, quer %q (a linha recomendada)", item.URL, queridos[0])
	}
	if item.Comando != "" || len(item.Args) != 0 {
		t.Errorf("execução = %q %v, quer vazia num remoto", item.Comando, item.Args)
	}
	if len(item.Endpoints) != len(queridos) {
		t.Fatalf("Endpoints = %d (%v), quer %d", len(item.Endpoints), item.Endpoints, len(queridos))
	}
	for n, quero := range queridos {
		if item.Endpoints[n] != quero {
			t.Errorf("Endpoints[%d] = %q, quer %q", n, item.Endpoints[n], quero)
		}
	}
	// A invariante de D-07: a URL do item é um dos endpoints, e aqui é o
	// primeiro, porque a linha recomendada é a primeira da página.
	if item.Endpoints[0] != item.URL {
		t.Errorf("Endpoints[0] = %q, quer a própria URL %q", item.Endpoints[0], item.URL)
	}
}

// TestTabelaSoDeRepositoriosNaoViraConexao: tabela com coluna de URL é convite
// a conectar só quando o que está nela é endpoint. Link de repositório não é
// (D-02: github.com, gitlab.com e bitbucket.org nunca são endpoint), e o nome
// do servidor terminar em "/mcp" também não basta — ele está na coluna errada.
func TestTabelaSoDeRepositoriosNaoViraConexao(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/servers/{slug...}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amostra(t, "mcpservers-oficial-tabela-repositorios.html"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	item, err := biblioteca.NovaCuradoria(ts.URL+"/pt-BR").
		Oficial(context.Background(), "coletanea/repositorios-mcp")
	if err != nil {
		t.Fatalf("Oficial: erro = %v, quer nil", err)
	}
	if item.Transporte != biblioteca.TransporteSTDIO {
		t.Errorf("Transporte = %q, quer stdio: a tabela não publica endpoint", item.Transporte)
	}
	if item.URL != "" {
		t.Errorf("URL = %q, quer vazia: os endereços da tabela são repositórios", item.URL)
	}
	if len(item.Endpoints) != 0 {
		t.Errorf("Endpoints = %v, quer vazio", item.Endpoints)
	}
}
