package biblioteca_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

func repoDeTeste(t *testing.T) *biblioteca.RepositorioSQLite {
	t.Helper()

	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("abrir store: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar store: erro = %v, quer nil", err)
		}
	})
	return biblioteca.NovoRepositorio(st.Leitura(), st.Escrita())
}

func itensDeTeste() []biblioteca.Item {
	return []biblioteca.Item{
		{
			Nome: "com.atlassian/mcp", Titulo: "Atlassian",
			Descricao: "Jira, Confluence and Compass for agents",
			Versao:    "1.0.0", Transporte: biblioteca.TransporteHTTP,
			URL: "https://mcp.atlassian.test/mcp", PedeCredencial: true,
			Site: "https://atlassian.test",
		},
		{
			Nome: "com.notion/mcp", Titulo: "Notion",
			Descricao: "Official Notion MCP server",
			Versao:    "1.0.1", Transporte: biblioteca.TransporteSSE,
			URL: "https://mcp.notion.test/sse",
		},
		{
			Nome: "com.acme/local", Titulo: "Acme Local",
			Descricao: "Runs beside the gateway",
			Versao:    "0.4.0", Transporte: biblioteca.TransporteSTDIO,
			Comando: "npx", Args: []string{"-y", "acme-mcp@0.4.0"},
		},
	}
}

func TestSubstituirGravaEBuscarDevolve(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	itens, total, err := r.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if total != 3 || len(itens) != 3 {
		t.Fatalf("total = %d, itens = %d, quer 3 e 3", total, len(itens))
	}
	// Os argumentos precisam voltar na ordem: eles viram a linha de comando de
	// um processo, e trocar a ordem é trocar o que é executado.
	for _, i := range itens {
		if i.Nome != "com.acme/local" {
			continue
		}
		if len(i.Args) != 2 || i.Args[0] != "-y" || i.Args[1] != "acme-mcp@0.4.0" {
			t.Fatalf("Args = %v, quer [-y acme-mcp@0.4.0]", i.Args)
		}
	}
}

func TestBuscarCasaNoNomeENaDescricao(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	// "jira" não está no nome nem no título do Atlassian: está na descrição. É
	// exatamente o caso em que a busca da origem antiga devolvia zero.
	itens, total, err := r.Buscar(ctx, biblioteca.Filtro{Termo: "jira"}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if total != 1 || len(itens) != 1 || itens[0].Nome != "com.atlassian/mcp" {
		t.Fatalf("busca por jira = %v (total %d), quer só o Atlassian", itens, total)
	}

	// Vários termos exigem todos, em qualquer ordem.
	if _, total, _ := r.Buscar(ctx, biblioteca.Filtro{Termo: "confluence atlassian"}, 10, 0); total != 1 {
		t.Errorf("busca por dois termos = %d, quer 1", total)
	}
	if _, total, _ := r.Buscar(ctx, biblioteca.Filtro{Termo: "jira notion"}, 10, 0); total != 0 {
		t.Errorf("busca por termos de servidores diferentes = %d, quer 0", total)
	}
}

func TestBuscarNaoDeixaOTermoVirarCuringa(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	// Sem escape, % e _ do termo viram curinga do LIKE: quem digitasse "100%"
	// receberia o catálogo inteiro em vez de nada.
	for _, termo := range []string{"%", "_", "100%"} {
		if _, total, err := r.Buscar(ctx, biblioteca.Filtro{Termo: termo}, 10, 0); err != nil || total != 0 {
			t.Errorf("busca por %q = %d (erro %v), quer 0", termo, total, err)
		}
	}
}

func TestBuscarPaginaSemRepetirNemPular(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	vistos := map[string]bool{}
	for deslocamento := 0; deslocamento < 3; deslocamento += 2 {
		itens, total, err := r.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 2, deslocamento)
		if err != nil {
			t.Fatalf("Buscar: erro = %v, quer nil", err)
		}
		if total != 3 {
			t.Fatalf("total = %d, quer 3 em toda página", total)
		}
		for _, i := range itens {
			if vistos[i.Nome] {
				t.Fatalf("%s apareceu em duas páginas", i.Nome)
			}
			vistos[i.Nome] = true
		}
	}
	if len(vistos) != 3 {
		t.Fatalf("vistos = %d, quer 3: a paginação pulou alguém", len(vistos))
	}
}

func TestSubstituirTrocaOCatalogoEmVezDeSomar(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}
	// A varredura seguinte não traz mais o Notion: ele saiu do registry. Se o
	// catálogo somasse em vez de trocar, ele ficaria aqui para sempre — e um
	// servidor que saiu de lá quase sempre saiu porque o endpoint morreu.
	if err := r.Substituir(ctx, itensDeTeste()[:1], time.Now()); err != nil {
		t.Fatalf("segunda Substituir: erro = %v, quer nil", err)
	}

	if _, total, _ := r.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 1 {
		t.Fatalf("total = %d, quer 1", total)
	}
	if _, err := r.Um(ctx, "com.notion/mcp"); !errors.Is(err, biblioteca.ErrNaoEncontrado) {
		t.Fatalf("Um do que saiu: erro = %v, quer ErrNaoEncontrado", err)
	}
}

func TestVarreduraVaziaNaoApagaOCatalogo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	// Zero servidor é sempre defeito — esquema mudado, ou página vazia por
	// engano. Trocar uma foto boa por uma vazia deixaria a tela pior do que a
	// origem estar fora do ar.
	err := r.Substituir(ctx, nil, time.Now())
	if !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("erro = %v, quer ErrFormatoDaOrigem", err)
	}
	if _, total, _ := r.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3: o catálogo foi apagado", total)
	}
}

func TestFalhaDeSincronizacaoPreservaOCatalogoEFicaVisivel(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	quando := time.Now()
	if err := r.Substituir(ctx, itensDeTeste(), quando); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}
	if err := r.RegistrarFalha(ctx, time.Now(), errors.New("a origem não respondeu")); err != nil {
		t.Fatalf("RegistrarFalha: erro = %v, quer nil", err)
	}

	if _, total, _ := r.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3: a falha apagou o catálogo", total)
	}
	estado, err := r.Sincronizacao(ctx)
	if err != nil {
		t.Fatalf("Sincronizacao: erro = %v, quer nil", err)
	}
	// A idade sozinha mentiria: catálogo de ontem com a última tentativa
	// falhando é situação diferente de catálogo de ontem sincronizado no
	// horário, e a tela mostra as duas coisas.
	if estado.Erro == "" {
		t.Error("a falha não ficou registrada")
	}
	if estado.Nunca() {
		t.Error("a falha apagou a marca da última varredura completa")
	}
	if estado.Servidores != 3 {
		t.Errorf("Servidores = %d, quer 3", estado.Servidores)
	}
}

func TestSincronizacaoDeInstalacaoNova(t *testing.T) {
	t.Parallel()

	estado, err := repoDeTeste(t).Sincronizacao(context.Background())
	if err != nil {
		t.Fatalf("Sincronizacao: erro = %v, quer nil", err)
	}
	// É o que faz a tela dizer "o catálogo ainda está sendo baixado" em vez de
	// "nenhum servidor", que faria a instalação nova parecer defeito.
	if !estado.Nunca() {
		t.Error("instalação nova não deveria ter varredura concluída")
	}
}

func TestUmRecusaNomeQueNaoPodeExistir(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	if err := r.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	// Nada disto é nome de registry. A borda é aqui porque o nome chega pela
	// URL da tela.
	for _, nome := range []string{"", "semnamespace", "com.notion/mcp/extra", "../../etc", "com notion/mcp"} {
		if _, err := r.Um(ctx, nome); !errors.Is(err, biblioteca.ErrNaoEncontrado) {
			t.Errorf("Um(%q): erro = %v, quer ErrNaoEncontrado", nome, err)
		}
	}
	if _, err := r.Um(ctx, "com.notion/mcp"); err != nil {
		t.Errorf("Um do nome legítimo: erro = %v, quer nil", err)
	}
}

// itensDeTodosOsNamespaces cobre cada forma de nome que o registry publica.
func itensDeTodosOsNamespaces() []biblioteca.Item {
	nomes := []string{
		"com.notion/mcp",                        // domínio verificado
		"ac.inference.sh/mcp",                   // domínio verificado, com subdomínio
		"io.github.fulano/servidor",             // conta de GitHub
		"io.github.MrRefactoring/atlassian-mcp", // conta de GitHub, com maiúscula
		"io.gitlab.beltrano/servidor",           // conta de GitLab
		"io.modelcontextprotocol.anonymous/x",   // publicado sem identificação
	}
	itens := make([]biblioteca.Item, 0, len(nomes))
	for _, n := range nomes {
		itens = append(itens, biblioteca.Item{
			Nome: n, Titulo: n, Descricao: "servidor de teste",
			Transporte: biblioteca.TransporteHTTP, URL: "https://exemplo.test/mcp",
		})
	}
	return itens
}

func TestDominioVerificadoSeparaFornecedorDeContaDeFoundry(t *testing.T) {
	t.Parallel()

	quer := map[string]bool{
		"com.notion/mcp":                        true,
		"ac.inference.sh/mcp":                   true,
		"io.github.fulano/servidor":             false,
		"io.github.MrRefactoring/atlassian-mcp": false,
		"io.gitlab.beltrano/servidor":           false,
		"io.modelcontextprotocol.anonymous/x":   false,
	}
	for _, i := range itensDeTodosOsNamespaces() {
		if got := i.DominioVerificado(); got != quer[i.Nome] {
			t.Errorf("DominioVerificado(%q) = %v, quer %v", i.Nome, got, quer[i.Nome])
		}
	}
}

// TestFiltroDeCuradosRecortaAListaCurta prova o filtro que a tela oferece: só o
// que a curadoria do mcpservers.org escolheu a dedo.
//
// Ao contrário do filtro de domínio que existia antes, curado é coluna gravada
// na sincronização — não há regra derivada do nome escrita em dois lugares para
// divergir.
func TestFiltroDeCuradosRecortaAListaCurta(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	itens := itensDeTeste()
	itens[0].Curado = true
	itens[0].Autenticacao = biblioteca.AutOAuth
	if err := r.Substituir(ctx, itens, time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	curados, total, err := r.Buscar(ctx, biblioteca.Filtro{SoCurados: true}, 100, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if total != 1 || len(curados) != 1 || curados[0].Nome != itens[0].Nome {
		t.Fatalf("curados = %v (total %d), quer só %s", curados, total, itens[0].Nome)
	}
	// A autenticação precisa sobreviver à ida e volta do banco: é ela que faz o
	// formulário abrir com OAuth marcado.
	if curados[0].Autenticacao != biblioteca.AutOAuth {
		t.Errorf("Autenticacao = %q, quer %q", curados[0].Autenticacao, biblioteca.AutOAuth)
	}
	if !curados[0].Curado {
		t.Error("Curado voltou falso do banco")
	}
}

func TestFiltroDeCuradosSomaComABuscaPorTermo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	itens := itensDeTeste()
	itens[0].Curado = true // o Atlassian
	if err := r.Substituir(ctx, itens, time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	// O que se prova aqui é que os dois critérios se somam em vez de um
	// sobrescrever o outro: o Atlassian casa no termo e está marcado como
	// curado, então sobra ele e mais ninguém.
	_, comAmbos, err := r.Buscar(ctx,
		biblioteca.Filtro{Termo: "jira", SoCurados: true}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if comAmbos != 1 {
		t.Errorf("termo + curados = %d, quer 1", comAmbos)
	}
}

// TestCuradosVemPrimeiro: a ordem alfabética sozinha enterrava o que importa.
// Medido numa varredura de verdade em 2026-09-09, buscar "neon" trazia
// "br.com.nineoneninetwo/9192" — que casa porque "neon" está dentro de
// "nineoneninetwo" — antes do Neon curado.
func TestCuradosVemPrimeiro(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	itens := []biblioteca.Item{
		{Nome: "br.com.nineoneninetwo/9192", Titulo: "Nine One Nine Two",
			Descricao: "algo qualquer", Transporte: biblioteca.TransporteHTTP,
			URL: "https://a.test/mcp"},
		{Nome: "mcpservers.org/neon", Titulo: "Neon", Curado: true,
			Descricao: "Postgres serverless", Transporte: biblioteca.TransporteHTTP,
			URL: "https://mcp.neon.tech/mcp"},
	}
	if err := r.Substituir(ctx, itens, time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	achados, total, err := r.Buscar(ctx, biblioteca.Filtro{Termo: "neon"}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, quer 2 (os dois casam em 'neon')", total)
	}
	if achados[0].Nome != "mcpservers.org/neon" {
		t.Fatalf("primeiro = %q, quer o curado — a ordem alfabética enterrou o que importa",
			achados[0].Nome)
	}
}
