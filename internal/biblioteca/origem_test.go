package biblioteca_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// As amostras em testdata são páginas de verdade do mcpservers.org, com script
// e svg removidos. São o único jeito honesto de testar a tradução do HTML: um
// fixture escrito à mão testaria a expressão contra ela mesma, e é justamente a
// marcação do terceiro que muda sem avisar.
func amostra(t *testing.T, nome string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", nome))
	if err != nil {
		t.Fatalf("ler amostra %s: erro = %v, quer nil", nome, err)
	}
	return string(b)
}

// origemDeMentira serve as amostras nos mesmos caminhos da origem de verdade.
type origemDeMentira struct {
	*httptest.Server
	idas *atomic.Int64
}

func servir(t *testing.T, rotas map[string]func(w http.ResponseWriter, r *http.Request)) origemDeMentira {
	t.Helper()

	var idas atomic.Int64
	mux := http.NewServeMux()
	for caminho, fn := range rotas {
		mux.HandleFunc(caminho, fn)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idas.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return origemDeMentira{Server: ts, idas: &idas}
}

func pagina(corpo string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(corpo))
	}
}

func origemCompleta(t *testing.T) origemDeMentira {
	t.Helper()
	return servir(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /remote-mcp-servers":        pagina(amostra(t, "indice.html")),
		"GET /remote-mcp-servers/notion": pagina(amostra(t, "detalhe-oauth.html")),
		"GET /remote-mcp-servers/b12":    pagina(amostra(t, "detalhe-aberta.html")),
	})
}

func TestListarLeOsServidoresDaOrigem(t *testing.T) {
	t.Parallel()

	o := biblioteca.NovaOrigem(origemCompleta(t).URL, time.Minute)
	itens, err := o.Listar(context.Background())
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	if len(itens) != 10 {
		t.Fatalf("itens = %d, quer 10 (a amostra tem dez cartões)", len(itens))
	}
	// Nome e resumo vêm da página, no idioma que a origem serve em /pt-BR.
	var achou bool
	for _, i := range itens {
		if i.Slug == "atlassian" {
			achou = true
			if i.Nome != "Atlassian" {
				t.Errorf("nome = %q, quer Atlassian", i.Nome)
			}
			if !strings.Contains(i.Resumo, "Jira") {
				t.Errorf("resumo = %q, quer citar Jira", i.Resumo)
			}
		}
	}
	if !achou {
		t.Error("a amostra não trouxe o atlassian")
	}
}

// TestListarNaoVaiNaOrigemDuasVezesSeguidas guarda o motivo de existir a
// validade: a tela busca a cada tecla digitada, e sem isto a origem levaria uma
// rajada — que é o que faz o Cloudflare de lá responder com desafio de bot.
func TestListarNaoVaiNaOrigemDuasVezesSeguidas(t *testing.T) {
	t.Parallel()

	fonte := origemCompleta(t)
	o := biblioteca.NovaOrigem(fonte.URL, time.Minute)
	for range 5 {
		if _, err := o.Listar(context.Background()); err != nil {
			t.Fatalf("Listar: erro = %v, quer nil", err)
		}
	}
	if idas := fonte.idas.Load(); idas != 1 {
		t.Fatalf("idas à origem = %d, quer 1", idas)
	}
}

func TestDetalheTrazOQueOCadastroPrecisa(t *testing.T) {
	t.Parallel()

	o := biblioteca.NovaOrigem(origemCompleta(t).URL, time.Minute)

	casos := map[string]struct {
		slug         string
		nome         string
		url          string
		transporte   string
		autenticacao string
		modo         string
	}{
		"servidor com consentimento": {
			slug: "notion", nome: "Notion", url: "https://mcp.notion.com/mcp",
			transporte: biblioteca.TransporteHTTP, autenticacao: biblioteca.AutOAuth, modo: "oauth",
		},
		"servidor aberto": {
			slug: "b12", transporte: biblioteca.TransporteHTTP,
			autenticacao: biblioteca.AutAberta, modo: "estatica",
		},
	}
	for nome, caso := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()
			d, err := o.Detalhe(context.Background(), caso.slug)
			if err != nil {
				t.Fatalf("Detalhe(%s): erro = %v, quer nil", caso.slug, err)
			}
			if caso.nome != "" && d.Nome != caso.nome {
				t.Errorf("nome = %q, quer %q", d.Nome, caso.nome)
			}
			if caso.url != "" && d.URL != caso.url {
				t.Errorf("url = %q, quer %q", d.URL, caso.url)
			}
			if d.Transporte != caso.transporte {
				t.Errorf("transporte = %q, quer %q", d.Transporte, caso.transporte)
			}
			if d.Autenticacao != caso.autenticacao {
				t.Errorf("autenticação = %q, quer %q", d.Autenticacao, caso.autenticacao)
			}
			if d.ModoDeCredencial() != caso.modo {
				t.Errorf("modo = %q, quer %q", d.ModoDeCredencial(), caso.modo)
			}
			if !strings.HasPrefix(d.URL, "https://") {
				t.Errorf("url = %q, quer https", d.URL)
			}
		})
	}
}

func TestDetalheRecusaSlugQueNaoPodeExistir(t *testing.T) {
	t.Parallel()

	fonte := origemCompleta(t)
	o := biblioteca.NovaOrigem(fonte.URL, time.Minute)

	// Nada disto deveria virar caminho de URL. O slug chega pela URL da tela, e
	// a checagem acontece antes de qualquer requisição — por isso o teste
	// também confere que a origem não foi procurada.
	for _, ruim := range []string{"../admin", "a/b", "MAIÚSCULO", "", "com espaço", strings.Repeat("a", 200)} {
		if _, err := o.Detalhe(context.Background(), ruim); !errors.Is(err, biblioteca.ErrNaoEncontrado) {
			t.Errorf("Detalhe(%q): erro = %v, quer ErrNaoEncontrado", ruim, err)
		}
	}
	if idas := fonte.idas.Load(); idas != 0 {
		t.Errorf("idas à origem = %d, quer 0: slug inválido não deve virar requisição", idas)
	}
}

func TestErrosDaOrigem(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		responder func(http.ResponseWriter, *http.Request)
		quer      error
	}{
		"origem fora do ar": {
			responder: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "erro", http.StatusInternalServerError)
			},
			quer: biblioteca.ErrOrigemIndisponivel,
		},
		"desafio de bot com 200": {
			responder: pagina(amostra(t, "desafio.html")),
			quer:      biblioteca.ErrOrigemIndisponivel,
		},
		"página sem os cartões que o pacote sabe ler": {
			responder: pagina("<!DOCTYPE html><html><body><h1>oi</h1>" +
				strings.Repeat(" ", 700) + "</body></html>"),
			quer: biblioteca.ErrFormatoDaOrigem,
		},
	}
	for nome, caso := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(caso.responder))
			t.Cleanup(ts.Close)

			o := biblioteca.NovaOrigem(ts.URL, time.Minute)
			if _, err := o.Listar(context.Background()); !errors.Is(err, caso.quer) {
				t.Fatalf("erro = %v, quer %v", err, caso.quer)
			}
		})
	}
}

func TestDetalheRecusaPaginaIncompleta(t *testing.T) {
	t.Parallel()

	// Item pela metade é pior que erro: vira um botão que leva a um formulário
	// errado, e o admin só descobre depois de salvar.
	casos := map[string]string{
		"sem URL de conexão": `<h1>Acme</h1><p>resumo</p><dl><dt>Transporte</dt><dd>Streamable HTTP</dd></dl>`,
		"sem transporte": `<h1>Acme</h1><p>resumo</p><h2>Detalhes da conexão</h2>` +
			`<code>https://mcp.acme.example/mcp</code>`,
		"endpoint sem https": `<h1>Acme</h1><p>resumo</p><h2>Detalhes da conexão</h2>` +
			`<code>http://mcp.acme.example/mcp</code><dl><dt>Transporte</dt><dd>Streamable HTTP</dd></dl>`,
	}
	for nome, corpo := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(
				pagina("<!DOCTYPE html><html><body>" + corpo + strings.Repeat(" ", 700) + "</body></html>")))
			t.Cleanup(ts.Close)

			o := biblioteca.NovaOrigem(ts.URL, time.Minute)
			if _, err := o.Detalhe(context.Background(), "acme"); !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
				t.Fatalf("erro = %v, quer ErrFormatoDaOrigem", err)
			}
		})
	}
}

func TestDetalheDeServidorQueSaiuDaOrigem(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(ts.Close)

	o := biblioteca.NovaOrigem(ts.URL, time.Minute)
	if _, err := o.Detalhe(context.Background(), "sumiu"); !errors.Is(err, biblioteca.ErrNaoEncontrado) {
		t.Fatalf("erro = %v, quer ErrNaoEncontrado", err)
	}
}

func TestBuscar(t *testing.T) {
	t.Parallel()

	itens := []biblioteca.Item{
		{Slug: "atlassian", Nome: "Atlassian", Resumo: "Jira, Confluence, Compass"},
		{Slug: "notion", Nome: "Notion", Resumo: "Páginas e bancos de dados"},
	}
	casos := map[string]struct {
		termo string
		quer  []string
	}{
		"vazio devolve tudo": {"", []string{"atlassian", "notion"}},
		"acha pelo nome":     {"notion", []string{"notion"}},
		"ignora a caixa":     {"NOTION", []string{"notion"}},
		// O caso que justifica filtrar aqui em vez de delegar: a busca da própria
		// origem casa só pelo nome, e devolve zero para "jira".
		"acha pelo resumo":             {"jira", []string{"atlassian"}},
		"dois termos exigem os dois":   {"atlassian confluence", []string{"atlassian"}},
		"ordem dos termos não importa": {"confluence atlassian", []string{"atlassian"}},
		"termo que não existe":         {"kubernetes", nil},
	}
	for nome, caso := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()
			var achados []string
			for _, i := range biblioteca.Buscar(itens, caso.termo) {
				achados = append(achados, i.Slug)
			}
			if strings.Join(achados, ",") != strings.Join(caso.quer, ",") {
				t.Fatalf("Buscar(%q) = %v, quer %v", caso.termo, achados, caso.quer)
			}
		})
	}
}
