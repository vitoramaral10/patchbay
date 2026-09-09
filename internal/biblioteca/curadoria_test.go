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
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/pt-BR"
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
	if _, total, _ := repo.Buscar(context.Background(),
		biblioteca.Filtro{SoCurados: true}, 10, 0); total != 1 {
		t.Fatalf("curados = %d, quer 1: a varredura desistiu no primeiro 429", total)
	}
}
