package biblioteca_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// As amostras em testdata são respostas de verdade do registry, recortadas para
// caber. São o único jeito honesto de testar a tradução: um fixture escrito à
// mão testaria o decoder contra ele mesmo, e é justamente o esquema do terceiro
// que muda sem avisar.
//
// pagina.json tem os quatro casos que importam, todos reais:
//   - com.notion/mcp, com dois remotes (streamable-http e sse);
//   - ae.propick/propick, com header obrigatório declarado;
//   - com.pulsemcp/remote-filesystem, que só existe como pacote npm;
//   - io.github.rghsoftware/linux-filesystem, cujo único pacote é mcpb — sem
//     comando previsível, e por isso descartado.
func amostra(t *testing.T, nome string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", nome))
	if err != nil {
		t.Fatalf("ler amostra %s: erro = %v, quer nil", nome, err)
	}
	return b
}

// origemDeMentira serve respostas e guarda o que foi perguntado.
type origemDeMentira struct {
	*httptest.Server
	idas     *atomic.Int64
	ultimaQS *atomic.Pointer[url.Values]
}

func servir(t *testing.T, resposta func(w http.ResponseWriter, r *http.Request)) origemDeMentira {
	t.Helper()

	var idas atomic.Int64
	var ultima atomic.Pointer[url.Values]
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idas.Add(1)
		q := r.URL.Query()
		ultima.Store(&q)
		if r.URL.Path != "/v0/servers" {
			http.NotFound(w, r)
			return
		}
		resposta(w, r)
	}))
	t.Cleanup(ts.Close)
	return origemDeMentira{Server: ts, idas: &idas, ultimaQS: &ultima}
}

// servirAmostra é o caso comum: sempre a mesma resposta.
func servirAmostra(t *testing.T, nome string) origemDeMentira {
	t.Helper()
	corpo := amostra(t, nome)
	return servir(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(corpo)
	})
}

func TestListarTraduzOQueAOrigemPublica(t *testing.T) {
	t.Parallel()

	ts := servirAmostra(t, "pagina.json")
	res, err := biblioteca.NovaOrigem(ts.URL).Listar(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}

	// Quatro servidores na amostra, três com conexão que o patchbay sabe
	// cadastrar. O quarto sai fora, e sair fora é o comportamento: um cartão
	// com "adicionar" que abre formulário vazio é pior do que não aparecer.
	if len(res.Itens) != 3 {
		nomes := make([]string, 0, len(res.Itens))
		for _, i := range res.Itens {
			nomes = append(nomes, i.Nome)
		}
		t.Fatalf("itens = %d %v, quer 3", len(res.Itens), nomes)
	}
	if res.ProximoCursor != "a-proxima-pagina" {
		t.Errorf("ProximoCursor = %q, quer %q", res.ProximoCursor, "a-proxima-pagina")
	}

	porNome := map[string]biblioteca.Item{}
	for _, i := range res.Itens {
		porNome[i.Nome] = i
	}

	// O primeiro remote utilizável ganha: o notion publica streamable-http antes
	// do sse, e é o streamable-http que precisa chegar no formulário.
	notion := porNome["com.notion/mcp"]
	if notion.Transporte != biblioteca.TransporteHTTP {
		t.Errorf("notion.Transporte = %q, quer %q", notion.Transporte, biblioteca.TransporteHTTP)
	}
	if !strings.HasPrefix(notion.URL, "https://") {
		t.Errorf("notion.URL = %q, quer um https", notion.URL)
	}
	// A origem não declara autenticação em lugar nenhum — nem para o notion,
	// que é OAuth. É o motivo de a biblioteca não mandar modo de credencial no
	// link.
	if notion.PedeCredencial {
		t.Error("notion.PedeCredencial = true, quer false: a amostra não declara header")
	}

	if !porNome["ae.propick/propick"].PedeCredencial {
		t.Error("propick.PedeCredencial = false, quer true: a amostra declara header")
	}

	local := porNome["com.pulsemcp/remote-filesystem"]
	if local.Transporte != biblioteca.TransporteSTDIO {
		t.Fatalf("local.Transporte = %q, quer %q", local.Transporte, biblioteca.TransporteSTDIO)
	}
	if local.Comando != "npx" {
		t.Errorf("local.Comando = %q, quer npx", local.Comando)
	}
	// O -y precisa estar lá: sem ele o npx pergunta, e um upstream stdio roda
	// sem terminal para responder.
	if !strings.Contains(strings.Join(local.Args, " "), "-y") {
		t.Errorf("local.Args = %v, quer conter -y", local.Args)
	}
	if local.URL != "" {
		t.Errorf("local.URL = %q, quer vazio: stdio não tem endpoint", local.URL)
	}
}

func TestListarPedeAPaginaMaiorEOCursorVaiSemSerInterpretado(t *testing.T) {
	t.Parallel()

	ts := servirAmostra(t, "pagina.json")
	o := biblioteca.NovaOrigem(ts.URL)
	if _, err := o.Listar(context.Background(), "", "a-proxima-pagina"); err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	q := ts.ultimaQS.Load()
	if got := q.Get("cursor"); got != "a-proxima-pagina" {
		t.Errorf("cursor = %q, quer %q", got, "a-proxima-pagina")
	}
	// Cem é o teto da origem, e pedir menos multiplicaria as idas de uma
	// varredura que já são trezentas.
	if got := q.Get("limit"); got != "100" {
		t.Errorf("limit = %q, quer 100", got)
	}
	if got := q.Get("version"); got != "latest" {
		t.Errorf("version = %q, quer latest", got)
	}
}

func TestUltimaPaginaNaoTemProximoCursor(t *testing.T) {
	t.Parallel()

	ts := servirAmostra(t, "ultima-pagina.json")
	res, err := biblioteca.NovaOrigem(ts.URL).Listar(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	if res.ProximoCursor != "" {
		t.Errorf("ProximoCursor = %q, quer vazio", res.ProximoCursor)
	}
}

func TestErrosDaOrigem(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome     string
		resposta func(w http.ResponseWriter, r *http.Request)
		quer     error
	}{
		{
			nome: "erro do servidor",
			resposta: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "pane", http.StatusInternalServerError)
			},
			quer: biblioteca.ErrOrigemIndisponivel,
		},
		{
			// 404 no endpoint é o endpoint ter sumido, não um servidor não
			// existir: servidor que não existe chega como busca vazia, com 200.
			nome: "endpoint sumiu",
			resposta: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "não achei", http.StatusNotFound)
			},
			quer: biblioteca.ErrOrigemIndisponivel,
		},
		{
			nome: "corpo que não é json",
			resposta: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("<html>oi</html>"))
			},
			quer: biblioteca.ErrFormatoDaOrigem,
		},
		{
			// Json bem formado e sem a lista: o esquema mudou. É diferente de
			// lista vazia, e a distinção é o que decide se vale tentar de novo.
			nome: "json sem a lista de servidores",
			resposta: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[],"metadata":{}}`))
			},
			quer: biblioteca.ErrFormatoDaOrigem,
		},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()
			ts := servir(t, c.resposta)
			_, err := biblioteca.NovaOrigem(ts.URL).Listar(context.Background(), "", "")
			if !errors.Is(err, c.quer) {
				t.Fatalf("erro = %v, quer %v", err, c.quer)
			}
		})
	}
}

// Página vazia é resposta legítima da origem, e o sincronizador precisa
// distinguir isso de esquema quebrado — é o que decide se o catálogo local é
// preservado ou marcado como falho.
func TestPaginaVaziaNaoEErroDeFormato(t *testing.T) {
	t.Parallel()

	ts := servirAmostra(t, "vazia.json")
	res, err := biblioteca.NovaOrigem(ts.URL).Listar(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	if len(res.Itens) != 0 {
		t.Errorf("itens = %d, quer 0", len(res.Itens))
	}
}
