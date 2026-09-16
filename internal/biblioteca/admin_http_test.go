package biblioteca_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// telaSobre monta a tela sobre um catálogo local já povoado.
//
// A origem apontada é a de mentira, e ela existe só para o botão de atualizar
// ter para onde ir: a tela em si nunca fala com a rede, e é isso que faz a
// busca digitada responder na hora.
func telaSobre(t *testing.T, itens []biblioteca.Item) (http.Handler, *biblioteca.RepositorioSQLite) {
	t.Helper()
	h, repo, _ := telaComSinal(t, itens)
	return h, repo
}

// telaComSinal devolve também o aviso de que uma varredura terminou, para o
// teste do botão de atualizar esperar por sinal em vez de pelo relógio.
func telaComSinal(
	t *testing.T, itens []biblioteca.Item,
) (http.Handler, *biblioteca.RepositorioSQLite, <-chan struct{}) {
	t.Helper()

	repo := repoDeTeste(t)
	if len(itens) > 0 {
		if err := repo.Substituir(context.Background(), itens, time.Now()); err != nil {
			t.Fatalf("povoar catálogo: erro = %v, quer nil", err)
		}
	}
	sinc := sincronizadorDeTeste(t, curadoriaMuda(t), repo)

	sincronizou := make(chan struct{}, 4)
	sinc.Observar(func() {
		select {
		case sincronizou <- struct{}{}:
		default:
		}
	})

	mux := http.NewServeMux()
	biblioteca.NovoAdmin(repo, sinc, slog.New(slog.DiscardHandler)).Rotas(mux)
	return mux, repo, sincronizou
}

func abrir(t *testing.T, h http.Handler, alvo string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, alvo, nil)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func TestTelaMostraOCatalogoLocal(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	res := abrir(t, h, webui.RotaBiblioteca, false)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, quer 200", res.Code)
	}
	corpo := res.Body.String()
	for _, quer := range []string{"Atlassian", "com.acme/local", "Processo local", "Atualizar agora"} {
		if !strings.Contains(corpo, quer) {
			t.Errorf("a tela não traz %q", quer)
		}
	}
	// A idade do catálogo fica à vista: sem ela, não achar um servidor pode ser
	// ele não existir ou a cópia ser velha, e o admin não tem como saber qual.
	if !strings.Contains(corpo, "Catálogo local:") {
		t.Error("a tela não diz de quando é a lista")
	}
}

func TestBuscaAchaPelaDescricao(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	corpo := abrir(t, h, webui.RotaBiblioteca+"?q=jira", false).Body.String()

	// "jira" não está no nome nem no título do Atlassian. Achar por aqui é a
	// queixa original resolvida.
	if !strings.Contains(corpo, "Atlassian") {
		t.Error("a busca por jira não achou o Atlassian")
	}
	if strings.Contains(corpo, "Notion") {
		t.Error("a busca por jira trouxe quem não casa")
	}
}

func TestCatalogoVazioNaoPareceBuscaSemResultado(t *testing.T) {
	t.Parallel()

	// Instalação nova: a primeira varredura ainda não terminou. Dizer "nenhum
	// servidor com esse termo" aqui faria o admin achar que o patchbay quebrou.
	h, _ := telaSobre(t, nil)
	corpo := abrir(t, h, webui.RotaBiblioteca, false).Body.String()

	if !strings.Contains(corpo, "ainda está sendo baixado") {
		t.Error("a tela não explica que o catálogo ainda vem")
	}
	if !strings.Contains(corpo, "Cadastrar à mão") {
		t.Error("a tela não oferece a saída que já funciona")
	}
}

func TestRespostaDoHtmxEhSoOFragmento(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	corpo := abrir(t, h, webui.RotaBiblioteca+"?q=notion", true).Body.String()

	if strings.Contains(corpo, "<html") {
		t.Error("a resposta do htmx traz a página inteira")
	}
	if !strings.Contains(corpo, "resultados-biblioteca") {
		t.Error("a resposta do htmx não traz o alvo da troca")
	}
}

func TestAdicionarRedirecionaParaFormularioPreenchido(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	res := abrir(t, h, webui.RotaBiblioteca+"/adicionar/com.atlassian/mcp", false)

	if res.Code != http.StatusSeeOther && res.Code != http.StatusFound {
		t.Fatalf("status = %d, quer um redirecionamento", res.Code)
	}
	destino, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: erro = %v, quer nil", err)
	}
	if !strings.HasPrefix(destino.Path, webui.RotaUpstreams+"/novo") {
		t.Fatalf("destino = %q, quer o formulário de upstream novo", destino.Path)
	}
	q := destino.Query()
	if got := q.Get("tipo"); got != biblioteca.TransporteHTTP {
		t.Errorf("tipo = %q, quer %q", got, biblioteca.TransporteHTTP)
	}
	if got := q.Get("url"); got != "https://mcp.atlassian.test/mcp" {
		t.Errorf("url = %q, quer o endpoint do catálogo", got)
	}
	if got := q.Get("nome"); got != "Atlassian" {
		t.Errorf("nome = %q, quer Atlassian", got)
	}
	// Nem credencial nem modo: a query vaza para histórico e Referer, e a
	// origem não declara autenticação para haver modo a mandar.
	for _, proibido := range []string{"bearer", "token", "segredo", "modo"} {
		if q.Get(proibido) != "" {
			t.Errorf("a query leva %q, e não devia", proibido)
		}
	}
}

func TestAdicionarDeServidorLocalLevaAExecucao(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	res := abrir(t, h, webui.RotaBiblioteca+"/adicionar/com.acme/local", false)

	destino, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: erro = %v, quer nil", err)
	}
	q := destino.Query()
	if got := q.Get("tipo"); got != biblioteca.TransporteSTDIO {
		t.Fatalf("tipo = %q, quer %q", got, biblioteca.TransporteSTDIO)
	}
	if got := q.Get("comando"); got != "npx" {
		t.Errorf("comando = %q, quer npx", got)
	}
	// Um parâmetro por argumento, na ordem: argumento com espaço não sobrevive
	// a uma string só.
	args := q["arg"]
	if len(args) != 2 || args[0] != "-y" || args[1] != "acme-mcp@0.4.0" {
		t.Errorf("arg = %v, quer [-y acme-mcp@0.4.0]", args)
	}
	if q.Get("url") != "" {
		t.Error("a query leva url num cadastro stdio")
	}
}

func TestAdicionarSemComandoAbreFormularioVazio(t *testing.T) {
	t.Parallel()

	itens := []biblioteca.Item{
		{
			Nome: "com.exemplo/sem-comando", Titulo: "Sem Comando",
			Descricao:  "Publicado sem execução aproveitável",
			Transporte: biblioteca.TransporteSTDIO,
		},
	}
	h, _ := telaSobre(t, itens)
	res := abrir(t, h, webui.RotaBiblioteca+"/adicionar/com.exemplo/sem-comando", false)

	destino, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: erro = %v, quer nil", err)
	}
	q := destino.Query()
	if got := q.Get("tipo"); got != biblioteca.TransporteSTDIO {
		t.Errorf("tipo = %q, quer %q", got, biblioteca.TransporteSTDIO)
	}
	if got := q.Get("nome"); got != "Sem Comando" {
		t.Errorf("nome = %q, quer Sem Comando", got)
	}
	// Sem comando publicado, nada de comando/arg na query: o formulário fica
	// no padrão dele, e o admin escolhe.
	if _, ok := q["comando"]; ok {
		t.Error("a query leva comando, e não devia, para item sem comando")
	}
	if _, ok := q["arg"]; ok {
		t.Error("a query leva arg, e não devia, para item sem comando")
	}
}

func TestAdicionarServidorQueSaiuDoCatalogo(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	res := abrir(t, h, webui.RotaBiblioteca+"/adicionar/com.exemplo/sumido", false)

	if destino := res.Header().Get("Location"); !strings.Contains(destino, "aviso=sumiu") {
		t.Fatalf("destino = %q, quer voltar à biblioteca com o aviso", destino)
	}
}

func TestAtualizarAgoraDisparaAVarredura(t *testing.T) {
	t.Parallel()

	h, repo, sincronizou := telaComSinal(t, itensDeTeste())

	req := httptest.NewRequest(http.MethodPost, webui.RotaBiblioteca+"/atualizar", nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)

	if destino := res.Header().Get("Location"); !strings.Contains(destino, "aviso=atualizando") {
		t.Fatalf("destino = %q, quer o aviso de que começou", destino)
	}

	// A varredura roda no fundo, e a resposta não espera por ela — é assim de
	// propósito: em produção ela leva minutos. O sinal é como o teste sabe que
	// ela terminou sem dormir e torcer.
	select {
	case <-sincronizou:
	case <-time.After(10 * time.Second):
		t.Fatal("a varredura disparada pelo botão não terminou")
	}

	// A origem de mentira publica outro nome: se ele chegou ao banco, a
	// varredura foi de verdade e trocou o catálogo.
	if _, err := repo.Um(context.Background(), "mcpservers.org/exemplo-oficial"); err != nil {
		t.Fatalf("Um depois da varredura: erro = %v, quer nil", err)
	}
}

func TestTelaEscapaOQueVeioDaOrigem(t *testing.T) {
	t.Parallel()

	// O catálogo é escrito por terceiros: título e descrição são texto de quem
	// publicou. Um <script> ali dentro não pode virar script no painel.
	h, _ := telaSobre(t, []biblioteca.Item{{
		Nome: "com.exemplo/xss", Titulo: "<script>alert(1)</script>",
		Descricao:  "<img src=x onerror=alert(2)>",
		Transporte: biblioteca.TransporteHTTP, URL: "https://exemplo.test/mcp",
	}})
	corpo := abrir(t, h, webui.RotaBiblioteca, false).Body.String()

	if strings.Contains(corpo, "<script>alert(1)</script>") {
		t.Error("o título da origem saiu sem escape")
	}
	if strings.Contains(corpo, "<img src=x onerror") {
		t.Error("a descrição da origem saiu sem escape")
	}
}

func TestPaginaAlemDoFimCaiNaUltimaENaoEmNadaEncontrado(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	corpo := abrir(t, h, webui.RotaBiblioteca+"?p=99", false).Body.String()

	// Há resultados; o que não existe é a página 99. Dizer "nenhum servidor com
	// esse termo" seria a mensagem errada, e manda o admin duvidar do catálogo.
	if strings.Contains(corpo, "Nenhum servidor com esse termo") {
		t.Error("página além do fim virou busca sem resultado")
	}
	if !strings.Contains(corpo, "Atlassian") {
		t.Error("página além do fim não caiu numa página com conteúdo")
	}
}

func TestTelaNaoCitaORegistryNemOsFiltros(t *testing.T) {
	t.Parallel()

	itens := []biblioteca.Item{
		{
			Nome: "com.atlassian/mcp", Titulo: "Atlassian",
			Descricao:  "Jira, Confluence and Compass for agents",
			Transporte: biblioteca.TransporteHTTP,
			URL:        "https://mcp.atlassian.test/mcp", PedeCredencial: true,
			Site: "https://atlassian.test",
		},
		{
			Nome: "com.exemplo/sem-comando", Titulo: "Sem Comando",
			Descricao:  "Publicado sem execução aproveitável",
			Transporte: biblioteca.TransporteSTDIO,
		},
	}
	h, _ := telaSobre(t, itens)
	corpo := abrir(t, h, webui.RotaBiblioteca, false).Body.String()

	for _, proibido := range []string{"registry", "modelcontextprotocol.io"} {
		if strings.Contains(corpo, proibido) {
			t.Errorf("a tela cita %q, e não devia mais", proibido)
		}
	}
	if strings.Contains(corpo, `name="curados"`) {
		t.Error("a tela ainda tem o controle de filtro só curados")
	}
	if strings.Contains(corpo, "domínio verificado") {
		t.Error("a tela ainda tem o selo de domínio verificado")
	}
	if !strings.Contains(corpo, `href="https://mcpservers.org/pt-BR/official"`) {
		t.Error("o rodapé não aponta para a lista oficial do mcpservers.org")
	}
	if !strings.Contains(corpo, "não publica comando nem endpoint reconhecível") {
		t.Error("o item sem comando não avisa que não tem comando nem endpoint publicado")
	}

	// Link velho com ?curados=1 continua respondendo 200, só ignorando o
	// parâmetro — ver design.md.
	res := abrir(t, h, webui.RotaBiblioteca+"?curados=1", false)
	if res.Code != http.StatusOK {
		t.Errorf("status com ?curados=1 = %d, quer 200", res.Code)
	}
}

func TestTelaMostraAHoraDaUltimaTentativaQueFalhou(t *testing.T) {
	t.Parallel()

	h, repo := telaSobre(t, itensDeTeste())

	// Instante conhecido, truncado ao segundo porque é o que o carimbo Unix
	// grava — comparar com nanossegundo faria o teste falhar por motivo que
	// não tem nada a ver com o que ele prova.
	tentada := time.Date(2026, time.September, 11, 14, 32, 7, 0, time.Local).Truncate(time.Second)
	if err := repo.RegistrarFalha(context.Background(), tentada, errors.New("mcpservers.org: 503")); err != nil {
		t.Fatalf("RegistrarFalha: erro = %v, quer nil", err)
	}

	corpo := abrir(t, h, webui.RotaBiblioteca, false).Body.String()

	// RQ-01/RQ-06: a falha some sem a hora da tentativa, porque aí o admin não
	// sabe se está vendo o problema de agora ou um já resolvido.
	if !strings.Contains(corpo, tentada.Format("02/01/2006 15:04:05")) {
		t.Error("a tela não mostra a hora da última tentativa que falhou")
	}
	if !strings.Contains(corpo, "mcpservers.org: 503") {
		t.Error("a tela não mostra o motivo da última tentativa que falhou")
	}
}

func TestTelaListaEndpoints(t *testing.T) {
	t.Parallel()

	item := biblioteca.Item{
		Nome: "mcpservers.org/varios-endpoints", Titulo: "Vários Endpoints",
		Descricao:  "Publica mais de um endpoint",
		Transporte: biblioteca.TransporteHTTP,
		URL:        "https://recomendado.test/mcp",
		Endpoints:  []string{"https://a.test/mcp", "https://b.test/mcp"},
	}
	h, _ := telaSobre(t, []biblioteca.Item{item})
	corpo := abrir(t, h, webui.RotaBiblioteca, false).Body.String()

	for _, quer := range []string{"https://a.test/mcp", "https://b.test/mcp"} {
		if !strings.Contains(corpo, quer) {
			t.Errorf("a tela não lista o endpoint %q", quer)
		}
	}
	rota0 := webui.RotaBiblioteca + "/adicionar/" + item.Nome + "?endpoint=0"
	rota1 := webui.RotaBiblioteca + "/adicionar/" + item.Nome + "?endpoint=1"
	if !strings.Contains(corpo, `href="`+rota0+`"`) {
		t.Errorf("a tela não traz o link do primeiro endpoint (%s)", rota0)
	}
	if !strings.Contains(corpo, `href="`+rota1+`"`) {
		t.Errorf("a tela não traz o link do segundo endpoint (%s)", rota1)
	}

	// Cada link de endpoint abre o formulário com a url daquele endpoint, não
	// com a URL "recomendada" do item.
	destino0, err := url.Parse(abrir(t, h, rota0, false).Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location do endpoint 0: erro = %v, quer nil", err)
	}
	if got := destino0.Query().Get("url"); got != "https://a.test/mcp" {
		t.Errorf("url do endpoint 0 = %q, quer https://a.test/mcp", got)
	}
	destino1, err := url.Parse(abrir(t, h, rota1, false).Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location do endpoint 1: erro = %v, quer nil", err)
	}
	if got := destino1.Query().Get("url"); got != "https://b.test/mcp" {
		t.Errorf("url do endpoint 1 = %q, quer https://b.test/mcp", got)
	}

	// Índice fora da faixa cai no comportamento atual: a URL do item.
	rotaInvalida := webui.RotaBiblioteca + "/adicionar/" + item.Nome + "?endpoint=9"
	destinoInvalido, err := url.Parse(abrir(t, h, rotaInvalida, false).Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location do endpoint inválido: erro = %v, quer nil", err)
	}
	if got := destinoInvalido.Query().Get("url"); got != item.URL {
		t.Errorf("url com índice inválido = %q, quer a URL do item (%s)", got, item.URL)
	}
}

func TestTelaExplicaItemSemComandoNemURL(t *testing.T) {
	t.Parallel()

	item := biblioteca.Item{
		Nome: "mcpservers.org/sem-comando-nem-url", Titulo: "Sem Comando Nem URL",
		Descricao:  "Publicado sem execução nem endpoint aproveitável",
		Transporte: biblioteca.TransporteSTDIO,
		Site:       "https://site-do-servidor.test",
	}
	h, _ := telaSobre(t, []biblioteca.Item{item})
	corpo := abrir(t, h, webui.RotaBiblioteca, false).Body.String()

	if strings.Contains(corpo, "sem comando publicado") {
		t.Error("o texto antigo ainda aparece")
	}
	if !strings.Contains(corpo,
		"a página deste servidor não publica comando nem endpoint reconhecível — "+
			"abra a página e cadastre à mão") {
		t.Error("a tela não explica que a página não publica comando nem endpoint reconhecível")
	}
	if !strings.Contains(corpo, `href="https://mcpservers.org/pt-BR/servers/sem-comando-nem-url"`) {
		t.Error("a tela não traz o link para a página do servidor")
	}
	if !strings.Contains(corpo, `href="https://site-do-servidor.test"`) {
		t.Error("a tela não traz o link para o site do servidor")
	}
}
