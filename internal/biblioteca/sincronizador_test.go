package biblioteca_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// As amostras em testdata são páginas de verdade do mcpservers.org, recortadas
// para caber (script e svg removidos). São o único jeito honesto de testar a
// tradução: um fixture escrito à mão testaria a expressão contra ela mesma, e é
// justamente a marcação do terceiro que muda sem avisar.
func amostra(t *testing.T, nome string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", nome))
	if err != nil {
		t.Fatalf("ler amostra %s: erro = %v, quer nil", nome, err)
	}
	return b
}

// origemDeMentira serve respostas e conta as idas.
type origemDeMentira struct {
	*httptest.Server
	idas *atomic.Int64
}

// servir sobe uma origem que responde a mesma coisa a qualquer caminho.
//
// A qualquer caminho de propósito: quem chama descreve a origem inteira —
// índice fora do ar, marcação mudada, silêncio —, e um dublê amarrado ao
// desenho da paginação faria o teste passar por combinar com a implementação
// em vez de com o comportamento.
func servir(t *testing.T, resposta func(w http.ResponseWriter, r *http.Request)) origemDeMentira {
	t.Helper()

	var idas atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idas.Add(1)
		resposta(w, r)
	}))
	t.Cleanup(ts.Close)
	return origemDeMentira{Server: ts, idas: &idas}
}

// sincronizadorDeTeste monta a rotina sobre a origem dada, sem espera entre
// tentativas: provar que a varredura insiste custaria segundos de relógio de
// verdade a cada execução da suíte.
func sincronizadorDeTeste(
	t *testing.T, curada string, repo *biblioteca.RepositorioSQLite,
) *biblioteca.Sincronizador {
	t.Helper()

	return biblioteca.NovoSincronizador(
		biblioteca.NovaCuradoria(curada), repo, slog.New(slog.DiscardHandler),
		biblioteca.ComEsperaEntreTentativas(0),
	)
}

func TestEsquemaMudadoNaoEInsistido(t *testing.T) {
	t.Parallel()

	ts := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("isto não é a marcação do índice"))
	})

	err := sincronizadorDeTeste(t, ts.URL, repoDeTeste(t)).Sincronizar(context.Background())
	if !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("erro = %v, quer ErrFormatoDaOrigem", err)
	}
	// Repetir não melhora marcação mudada: seria peso na origem sem chance de
	// sucesso.
	if n := ts.idas.Load(); n != 1 {
		t.Errorf("idas à origem = %d, quer 1", n)
	}
}

func TestVarreduraQueFalhaNaoDerrubaOCatalogoAnterior(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := repo.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	fora := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora do ar", http.StatusBadGateway)
	})
	if err := sincronizadorDeTeste(t, fora.URL, repo).Sincronizar(ctx); err == nil {
		t.Fatal("Sincronizar: erro = nil, quer a falha da origem")
	}

	// Dado velho e útil vale mais do que tela vazia: a cópia anterior continua
	// servindo, e a tela mostra a idade dela junto com o erro.
	if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3", total)
	}
	estado, _ := repo.Sincronizacao(ctx)
	if estado.Erro == "" {
		t.Error("a falha não ficou visível para a tela")
	}
}

func TestVarreduraQueEstouraOPrazoRegistraFalha(t *testing.T) {
	t.Parallel()

	// Origem que nunca termina a página. Com o prazo da varredura em zero, o
	// efeito é o mesmo de uma origem lenta demais — e o que se prova é que isso
	// não é confundido com o patchbay desligando: a falha tem de ficar
	// registrada, senão a tela nunca conta que a última tentativa não terminou.
	lenta := servir(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancelar := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelar()

	repo := repoDeTeste(t)
	if err := repo.Substituir(context.Background(), itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}
	if err := sincronizadorDeTeste(t, lenta.URL, repo).Sincronizar(ctx); err == nil {
		t.Fatal("Sincronizar: erro = nil, quer o prazo estourado")
	}

	// Contexto de fora cancelado é desligamento: aí a falha não é registrada, de
	// propósito. Aqui o catálogo anterior é o que precisa ter sobrevivido.
	if _, total, _ := repo.Buscar(context.Background(), biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3: a varredura interrompida apagou o catálogo", total)
	}
}

// TestIndiceForaDoArPreservaOCatalogo: índice em 403 ou 5xx não derruba o
// catálogo anterior, e a falha fica visível com a hora da tentativa.
//
// Table-driven por status porque os dois são o mesmo comportamento — origem
// bloqueando ou fora do ar —, e um teste só por status duplicaria a asserção
// sem ganhar cobertura.
func TestIndiceForaDoArPreservaOCatalogo(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome   string
		status int
	}{
		{"403", http.StatusForbidden},
		{"500", http.StatusInternalServerError},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			repo := repoDeTeste(t)
			antes := time.Now()
			if err := repo.Substituir(ctx, itensDeTeste(), antes); err != nil {
				t.Fatalf("Substituir: erro = %v, quer nil", err)
			}

			bloqueado := servir(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bloqueado", c.status)
			})

			err := sincronizadorDeTeste(t, bloqueado.URL, repo).Sincronizar(ctx)
			if !errors.Is(err, biblioteca.ErrOrigemIndisponivel) {
				t.Fatalf("Sincronizar: erro = %v, quer ErrOrigemIndisponivel", err)
			}

			// O catálogo anterior continua intacto.
			if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
				t.Fatalf("total = %d, quer 3: catálogo anterior não sobreviveu", total)
			}
			estado, err := repo.Sincronizacao(ctx)
			if err != nil {
				t.Fatalf("Sincronizacao: erro = %v, quer nil", err)
			}
			if estado.Erro == "" {
				t.Error("a falha não ficou registrada em biblioteca_sincronizacao.erro")
			}
			if estado.TentadaEm.IsZero() {
				t.Error("tentada_em não foi gravado")
			}
			// Comparação truncada ao segundo: é a precisão que o SQLite guarda,
			// e a tentativa acontece no mesmo segundo do Substituir acima.
			if estado.TentadaEm.Before(antes.Truncate(time.Second)) {
				t.Errorf("tentada_em = %v, quer não antes de %v", estado.TentadaEm, antes)
			}
		})
	}
}

// TestIndiceSemServidorNaoEsvaziaOCatalogo: índice em 200 sem nenhum link de
// servidor reconhecível é marcação que mudou, não ausência de servidores — e
// não pode substituir o catálogo por vazio.
func TestIndiceSemServidorNaoEsvaziaOCatalogo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := repo.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	semLinks := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><main>sem nenhum servidor aqui</main></body></html>`))
	})

	err := sincronizadorDeTeste(t, semLinks.URL, repo).Sincronizar(ctx)
	if !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("Sincronizar: erro = %v, quer ErrFormatoDaOrigem", err)
	}

	if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3: catálogo anterior foi substituído por vazio", total)
	}
}

// siteDeMentira é o mcpservers.org de teste: serve o acervo /official e guarda
// todo caminho pedido, inclusive os que ele não conhece.
//
// Guardar o caminho é o ponto. O que se prova aqui é uma **ausência** — nenhuma
// ida ao registry, nenhuma à lista de remotos —, e ausência só se prova olhando
// tudo o que foi pedido; um dublê que responde 404 calado deixaria a varredura
// tentar o que não devia sem ninguém ver.
type siteDeMentira struct {
	*httptest.Server
	mu      sync.Mutex
	pedidos []string
}

// Pedidos devolve os caminhos pedidos até agora, na ordem.
func (s *siteDeMentira) Pedidos() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pedidos...)
}

// porPaginaNoIndiceDeMentira é quantos servidores o índice de teste mostra por
// página: um só, para que qualquer índice com mais de um servidor force mais
// de uma página — e assim prove que a varredura percorre a paginação inteira
// (T-08/CA-01), e não só a primeira página.
const porPaginaNoIndiceDeMentira = 1

// servirOficiais monta o acervo /official com os servidores dados, paginado.
// Caminho que não é do acervo responde 404 — e fica registrado.
func servirOficiais(t *testing.T, oficiais []servidorOficial) *siteDeMentira {
	t.Helper()

	total := (len(oficiais) + porPaginaNoIndiceDeMentira - 1) / porPaginaNoIndiceDeMentira
	if total < 1 {
		total = 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pt-BR/official", func(w http.ResponseWriter, r *http.Request) {
		pagina := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				pagina = n
			}
		}
		ini := (pagina - 1) * porPaginaNoIndiceDeMentira
		fim := ini + porPaginaNoIndiceDeMentira
		if fim > len(oficiais) {
			fim = len(oficiais)
		}

		var b strings.Builder
		b.WriteString(`<html><body><main>`)
		if ini < len(oficiais) {
			for _, o := range oficiais[ini:fim] {
				fmt.Fprintf(&b, `<a href="/pt-BR/servers/%s">%s</a>`, o.Slug, o.Nome)
			}
		}
		if total > 1 {
			// Só o link da última página importa: é dele que ultimaPagina tira o
			// total a percorrer.
			fmt.Fprintf(&b, `<a href="/pt-BR/official?page=%d">última</a>`, total)
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
			_, _ = fmt.Fprintf(w, `<html><body><h1>%s</h1><p>%s</p>`+
				`<dl><dt>Categoria</dt><dd>Ferramentas</dd></dl>`+
				`<pre>{&quot;mcpServers&quot;:{&quot;x&quot;:{`+
				`&quot;command&quot;: &quot;%s&quot;, &quot;args&quot;: [%s]}}}</pre>`+
				`</body></html>`, o.Nome, o.Resumo, o.Comando, o.Args)
			return
		}
		http.NotFound(w, r)
	})

	site := &siteDeMentira{}
	site.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		site.pedidos = append(site.pedidos, r.URL.Path)
		site.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(site.Close)
	return site
}

// TestVarreduraSoVaiAoOficial: a varredura tem um alvo só, e é o acervo
// /official do mcpservers.org.
//
// Ir ao registry ou à lista de remotos é exatamente o que esta mudança tirou, e
// é por isso que a asserção é sobre os caminhos pedidos e não só sobre o
// catálogo: um cliente do registry que continuasse lá, mesmo com o resultado
// descartado, ainda gastaria ~16 minutos de varredura contra um serviço de
// terceiro.
func TestVarreduraSoVaiAoOficial(t *testing.T) {
	t.Parallel()

	site := servirOficiais(t, []servidorOficial{
		{Slug: "um", Nome: "Um", Resumo: "primeiro",
			Comando: "npx", Args: `&quot;-y&quot;, &quot;um-mcp&quot;`},
		{Slug: "dois/sub", Nome: "Dois", Resumo: "segundo",
			Comando: "uvx", Args: `&quot;dois-mcp&quot;`},
	})

	ctx := context.Background()
	repo := repoDeTeste(t)
	err := sincronizadorDeTeste(t, site.URL+"/pt-BR", repo).Sincronizar(ctx)

	// Todo caminho pedido tem de ser do acervo oficial. A lista é branca de
	// propósito: enumerar os endereços proibidos — o do registry, o da lista de
	// remotos — os traria de volta para dentro do código, que é justamente o que
	// esta mudança tirou. Quem aparecer fora da lista sai impresso na falha.
	paginasDoIndice := 0
	for _, caminho := range site.Pedidos() {
		if !strings.HasPrefix(caminho, "/pt-BR/official") &&
			!strings.HasPrefix(caminho, "/pt-BR/servers/") {
			t.Errorf("a varredura pediu %q: só o acervo oficial pode ser varrido", caminho)
		}
		if caminho == "/pt-BR/official" {
			paginasDoIndice++
		}
	}
	// servirOficiais divide o índice em uma página por servidor (ver comentário
	// no helper): com 2 servidores, provar as 2 idas ao índice é provar que a
	// paginação inteira foi percorrida, e não só a primeira página.
	if paginasDoIndice != 2 {
		t.Errorf("idas a /pt-BR/official = %d, quer 2: a paginação não foi percorrida até o fim", paginasDoIndice)
	}
	if err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	// Um item por link de servidor do índice, com o nome que a origem dá.
	itens, total, err := repo.Buscar(ctx, biblioteca.Filtro{}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if total != 2 {
		t.Fatalf("servidores gravados = %d, quer 2 (um por link do índice)", total)
	}
	nomes := map[string]bool{}
	for _, i := range itens {
		nomes[i.Nome] = true
	}
	for _, quer := range []string{"mcpservers.org/um", "mcpservers.org/dois/sub"} {
		if !nomes[quer] {
			t.Errorf("catálogo sem %q: %v", quer, nomes)
		}
	}
}

// TestIndiceQueRepeteOServidorNaoDuplicaOItem: link repetido no índice não pode
// virar dois itens.
//
// O nome é chave primária no banco, e o segundo INSERT derrubaria a varredura
// inteira com erro de constraint — longe da causa, e num lugar em que ninguém
// procuraria. O índice de verdade repete link: o mesmo servidor aparece em
// destaque e na grade da mesma página.
func TestIndiceQueRepeteOServidorNaoDuplicaOItem(t *testing.T) {
	t.Parallel()

	repetido := servidorOficial{Slug: "um", Nome: "Um", Resumo: "primeiro",
		Comando: "npx", Args: `&quot;-y&quot;, &quot;um-mcp&quot;`}
	site := servirOficiais(t, []servidorOficial{repetido, repetido})

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := sincronizadorDeTeste(t, site.URL+"/pt-BR", repo).Sincronizar(ctx); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil — o nome repetido derrubou a varredura", err)
	}
	if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{}, 10, 0); total != 1 {
		t.Fatalf("servidores gravados = %d, quer 1: o link repetido virou dois itens", total)
	}
}

// oficiaisComN monta N servidores de mentira com slug e nome previsíveis
// (srv-0 .. srv-(N-1)), todos com comando aproveitável.
func oficiaisComN(n int) []servidorOficial {
	oficiais := make([]servidorOficial, n)
	for i := range n {
		oficiais[i] = servidorOficial{
			Slug: fmt.Sprintf("srv-%d", i), Nome: fmt.Sprintf("Servidor %d", i),
			Resumo: "de teste", Comando: "npx",
			Args: fmt.Sprintf(`&quot;-y&quot;, &quot;srv-%d-mcp&quot;`, i),
		}
	}
	return oficiais
}

// modoDeFalha é como o detalhe de um slug "fora do ar" responde — os três
// jeitos que CA-12 (emendado) manda contar do mesmo jeito no piso.
type modoDeFalha int

const (
	// falha500 é a origem fora do ar de verdade — ErrOrigemIndisponivel.
	falha500 modoDeFalha = iota
	// falha404 é o servidor que saiu do acervo — ErrNaoEncontrado.
	falha404
	// falhaSemTitulo é a página que chega (200) mas sem <h1> — ErrFormatoDaOrigem.
	falhaSemTitulo
)

// servirOficiaisComFalhas é servirOficiais, mas numa página de índice só (sem
// paginação — a paginação em si já é coberta por TestVarreduraSoVaiAoOficial) e
// com as páginas de detalhe dos primeiros comFalha slugs (na ordem do índice)
// respondendo de acordo com modo em vez do conteúdo normal — o "detalhe que
// não pôde ser lido" de RQ-02, nos três jeitos que CA-12 conta igual.
//
// Página única de propósito: TetoDePaginasOficiais é 60, e um índice de um
// servidor por página (como servirOficiais) estouraria o teto bem antes dos
// N=100 que este teste precisa para os números do CA-12 (11%, 5%).
func servirOficiaisComFalhas(t *testing.T, oficiais []servidorOficial, comFalha int, modo modoDeFalha) *siteDeMentira {
	t.Helper()

	falhando := make(map[string]bool, comFalha)
	for i := 0; i < comFalha && i < len(oficiais); i++ {
		falhando[oficiais[i].Slug] = true
	}

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
		if falhando[slug] {
			switch modo {
			case falha404:
				http.NotFound(w, r)
			case falhaSemTitulo:
				// 200 de verdade, mas sem <h1>: lerOficial devolve
				// ErrFormatoDaOrigem por falta de nome.
				_, _ = fmt.Fprint(w, `<html><body><p>sem título aqui</p></body></html>`)
			default:
				http.Error(w, "fora do ar", http.StatusInternalServerError)
			}
			return
		}
		for _, o := range oficiais {
			if o.Slug != slug {
				continue
			}
			_, _ = fmt.Fprintf(w, `<html><body><h1>%s</h1><p>%s</p>`+
				`<dl><dt>Categoria</dt><dd>Ferramentas</dd></dl>`+
				`<pre>{&quot;mcpServers&quot;:{&quot;x&quot;:{`+
				`&quot;command&quot;: &quot;%s&quot;, &quot;args&quot;: [%s]}}}</pre>`+
				`</body></html>`, o.Nome, o.Resumo, o.Comando, o.Args)
			return
		}
		http.NotFound(w, r)
	})

	site := &siteDeMentira{}
	site.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		site.pedidos = append(site.pedidos, r.URL.Path)
		site.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(site.Close)
	return site
}

// TestPisoDeDetalhesIndisponiveis (T-17, RQ-02, CA-12): mais de 10% dos
// detalhes do índice em 5xx falha a varredura com a contagem, preservando o
// catálogo anterior; até 10% conclui com sucesso, gravando os itens cujo
// detalhe respondeu.
//
// N=20 prova a borda exata (2 é 10%, 3 é 15%) e N=100 prova os números do
// CA-12 (11% e 5%) — aritmética inteira sem surpresa nos dois.
func TestPisoDeDetalhesIndisponiveis(t *testing.T) {
	t.Parallel()

	casos := []struct {
		nome        string
		n, comFalha int
		modo        modoDeFalha
		querFalhar  bool
	}{
		{"20_no_piso_nao_falha", 20, 2, falha500, false},
		{"20_acima_do_piso_falha", 20, 3, falha500, true},
		{"100_abaixo_do_piso_nao_falha", 100, 5, falha500, false},
		{"100_acima_do_piso_falha", 100, 11, falha500, true},
		// CA-12 emendado: 404 e página sem título contam do mesmo jeito que
		// 500 no piso — acima dele falha, e 5% de sem-título ainda é
		// tolerado (prova o lado tolerante para o modo novo, não só o 500).
		{"100_acima_do_piso_falha_404", 100, 11, falha404, true},
		{"100_acima_do_piso_falha_sem_titulo", 100, 11, falhaSemTitulo, true},
		{"100_abaixo_do_piso_nao_falha_sem_titulo", 100, 5, falhaSemTitulo, false},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			repo := repoDeTeste(t)
			antes := time.Now()
			if err := repo.Substituir(ctx, itensDeTeste(), antes); err != nil {
				t.Fatalf("Substituir: erro = %v, quer nil", err)
			}

			oficiais := oficiaisComN(c.n)
			site := servirOficiaisComFalhas(t, oficiais, c.comFalha, c.modo)

			err := sincronizadorDeTeste(t, site.URL+"/pt-BR", repo).Sincronizar(ctx)

			if c.querFalhar {
				if !errors.Is(err, biblioteca.ErrOrigemIndisponivel) {
					t.Fatalf("Sincronizar: erro = %v, quer ErrOrigemIndisponivel", err)
				}
				// Acima do piso: o catálogo anterior fica intacto, e a falha
				// visível para a tela.
				if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{}, 10, 0); total != 3 {
					t.Fatalf("total = %d, quer 3: catálogo anterior não sobreviveu", total)
				}
				estado, err := repo.Sincronizacao(ctx)
				if err != nil {
					t.Fatalf("Sincronizacao: erro = %v, quer nil", err)
				}
				if estado.Erro == "" {
					t.Error("a falha não ficou registrada em biblioteca_sincronizacao.erro")
				}
				return
			}

			if err != nil {
				t.Fatalf("Sincronizar: erro = %v, quer nil", err)
			}
			// No piso ou abaixo: os itens cujo detalhe respondeu foram
			// gravados, e os indisponíveis ficaram de fora.
			itens, total, err := repo.Buscar(ctx, biblioteca.Filtro{}, c.n, 0)
			if err != nil {
				t.Fatalf("Buscar: erro = %v, quer nil", err)
			}
			if quer := c.n - c.comFalha; total != quer {
				t.Fatalf("servidores gravados = %d, quer %d (%d - %d indisponíveis)",
					total, quer, c.n, c.comFalha)
			}
			nomes := map[string]bool{}
			for _, i := range itens {
				nomes[i.Nome] = true
			}
			for _, o := range oficiais[c.comFalha:] {
				if !nomes["mcpservers.org/"+o.Slug] {
					t.Errorf("catálogo sem %q", o.Slug)
				}
			}
		})
	}
}
