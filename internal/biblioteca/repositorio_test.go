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
			Descricao:  "Jira, Confluence and Compass for agents",
			Transporte: biblioteca.TransporteHTTP,
			URL:        "https://mcp.atlassian.test/mcp", PedeCredencial: true,
			Site: "https://atlassian.test",
		},
		{
			Nome: "com.notion/mcp", Titulo: "Notion",
			Descricao:  "Official Notion MCP server",
			Transporte: biblioteca.TransporteSSE,
			URL:        "https://mcp.notion.test/sse",
		},
		{
			Nome: "com.acme/local", Titulo: "Acme Local",
			Descricao:  "Runs beside the gateway",
			Transporte: biblioteca.TransporteSTDIO,
			Comando:    "npx", Args: []string{"-y", "acme-mcp@0.4.0"},
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

// TestEndpointsIdaEVolta prova a coluna endpoints (D-07): item com vários
// endpoints volta com todos, na ordem; item sem nenhum volta com fatia vazia,
// nunca nula — um formulário que faz range sobre nil não quebra, mas um teste
// que compara com nil sim.
func TestEndpointsIdaEVolta(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)

	comEndpoints := biblioteca.Item{
		Nome: "mcpservers.org/varios-endpoints", Titulo: "Vários Endpoints",
		Transporte: biblioteca.TransporteHTTP,
		URL:        "https://a.test/mcp",
		Endpoints: []string{
			"https://a.test/mcp", "https://b.test/mcp", "https://c.test/mcp",
		},
	}
	semEndpoints := biblioteca.Item{
		Nome: "mcpservers.org/sem-endpoints", Titulo: "Sem Endpoints",
		Transporte: biblioteca.TransporteSTDIO,
		Comando:    "npx",
	}
	if err := r.Substituir(ctx, []biblioteca.Item{comEndpoints, semEndpoints}, time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	itens, _, err := r.Buscar(ctx, biblioteca.Filtro{}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	var achouCom, achouSem bool
	for _, i := range itens {
		switch i.Nome {
		case comEndpoints.Nome:
			achouCom = true
			if len(i.Endpoints) != 3 || i.Endpoints[0] != "https://a.test/mcp" ||
				i.Endpoints[1] != "https://b.test/mcp" || i.Endpoints[2] != "https://c.test/mcp" {
				t.Fatalf("Endpoints (Buscar) = %v, quer [a b c] na ordem", i.Endpoints)
			}
		case semEndpoints.Nome:
			achouSem = true
			if i.Endpoints == nil || len(i.Endpoints) != 0 {
				t.Fatalf("Endpoints (Buscar) = %#v, quer fatia vazia não nula", i.Endpoints)
			}
		}
	}
	if !achouCom || !achouSem {
		t.Fatalf("Buscar não devolveu os dois itens: achouCom=%v achouSem=%v", achouCom, achouSem)
	}

	// Um segue a mesma regra.
	um, err := r.Um(ctx, comEndpoints.Nome)
	if err != nil {
		t.Fatalf("Um: erro = %v, quer nil", err)
	}
	if len(um.Endpoints) != 3 {
		t.Fatalf("Endpoints (Um) = %v, quer 3", um.Endpoints)
	}
	umSem, err := r.Um(ctx, semEndpoints.Nome)
	if err != nil {
		t.Fatalf("Um: erro = %v, quer nil", err)
	}
	if umSem.Endpoints == nil || len(umSem.Endpoints) != 0 {
		t.Fatalf("Endpoints (Um) = %#v, quer fatia vazia não nula", umSem.Endpoints)
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

// TestOrdemPorNome: sem curadoria para desempatar, a listagem vem em ordem
// alfabética de nome — e essa ordem precisa se manter estável mesmo quando
// mais de um item casa com o termo buscado.
func TestOrdemPorNome(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r := repoDeTeste(t)
	itens := []biblioteca.Item{
		{Nome: "br.com.nineoneninetwo/9192", Titulo: "Nine One Nine Two",
			Descricao: "algo qualquer", Transporte: biblioteca.TransporteHTTP,
			URL: "https://a.test/mcp"},
		{Nome: "mcpservers.org/neon", Titulo: "Neon",
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
	// "br.com.nineoneninetwo" vem antes de "mcpservers.org/neon" em ordem
	// alfabética de nome.
	if achados[0].Nome != "br.com.nineoneninetwo/9192" {
		t.Fatalf("primeiro = %q, quer br.com.nineoneninetwo/9192 — ordem por nome",
			achados[0].Nome)
	}
}
