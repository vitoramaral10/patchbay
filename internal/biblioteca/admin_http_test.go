package biblioteca_test

import (
	"context"
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
// A origem apontada é a de mentira do registry, e ela existe só para o botão de
// atualizar ter para onde ir: a tela em si nunca fala com a rede, e é isso que
// faz a busca digitada responder na hora.
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
	ts := registryDeMentira(t)
	sinc := sincronizadorDeTeste(t, ts.URL, repo)

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

	// O registry de mentira publica outros nomes: se eles chegaram ao banco, a
	// varredura foi de verdade e trocou o catálogo.
	if _, err := repo.Um(context.Background(), "com.exemplo/um"); err != nil {
		t.Fatalf("Um depois da varredura: erro = %v, quer nil", err)
	}
}

func TestTelaEscapaOQueVeioDaOrigem(t *testing.T) {
	t.Parallel()

	// O catálogo é escrito por terceiros: título e descrição são texto de quem
	// publicou. Um <script> ali dentro não pode virar script no painel.
	h, _ := telaSobre(t, []biblioteca.Item{{
		Nome: "com.exemplo/xss", Titulo: "<script>alert(1)</script>",
		Descricao: "<img src=x onerror=alert(2)>", Versao: "1.0.0",
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

func TestFiltroDeCuradosNaTela(t *testing.T) {
	t.Parallel()

	itens := itensDeTeste()
	itens[0].Curado = true // o Atlassian veio da curadoria
	h, _ := telaSobre(t, append(itens, biblioteca.Item{
		Nome: "io.github.fulano/servidor", Titulo: "Servidor de Fulano",
		Descricao: "publicado por conta de GitHub", Versao: "0.1.0",
		Transporte: biblioteca.TransporteHTTP, URL: "https://fulano.test/mcp",
	}))

	// Sem o filtro, todo mundo aparece.
	todos := abrir(t, h, webui.RotaBiblioteca, false).Body.String()
	if !strings.Contains(todos, "Servidor de Fulano") || !strings.Contains(todos, "Atlassian") {
		t.Fatal("a tela sem filtro não trouxe todo mundo")
	}
	if !strings.Contains(todos, "Só oficiais") {
		t.Error("a tela não oferece o filtro")
	}
	if !strings.Contains(todos, "curado") {
		t.Error("o cartão do curado não leva o selo")
	}

	// Com o filtro, só o que a curadoria escolheu.
	curados := abrir(t, h, webui.RotaBiblioteca+"?curados=1", false).Body.String()
	if strings.Contains(curados, "Servidor de Fulano") {
		t.Error("o filtro deixou passar quem não é curado")
	}
	if !strings.Contains(curados, "Atlassian") {
		t.Error("o filtro escondeu quem é curado")
	}
	// A contagem tem de acompanhar o recorte: dizer "4 servidores" mostrando 1
	// é a contagem errada na tela.
	if !strings.Contains(curados, "1 servidores curados") {
		t.Error("o resumo não reflete o recorte")
	}
}

func TestFiltroDeCuradosSobreviveAPaginacao(t *testing.T) {
	t.Parallel()

	h, _ := telaSobre(t, itensDeTeste())
	corpo := abrir(t, h, webui.RotaBiblioteca+"?curados=1&q=mcp", false).Body.String()

	// O link de página tem de carregar os dois critérios: perder o recorte ao
	// virar a página é o defeito clássico de filtro que vive na URL.
	if strings.Contains(corpo, "Página 1 de 1") && !strings.Contains(corpo, "curados=1") {
		return // uma página só, não há link para conferir
	}
	if strings.Contains(corpo, "href=\"/admin/biblioteca?p=") {
		t.Error("o link de paginação perdeu o filtro")
	}
}
