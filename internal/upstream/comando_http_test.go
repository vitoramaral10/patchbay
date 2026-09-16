package upstream_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// rotaColar é a única rota do caminho de colar: um POST, um MCP criado.
const rotaColar = "/colar"

// TestAdmin_ColarComando percorre o caminho inteiro pela borda HTTP: colar e
// criar, sem tela no meio.
//
// O que ele prova e nenhum teste de domínio prova é a costura: o nome do campo
// do formulário (`comando`), a rota, e — o que mais importa — que a credencial
// que veio na linha de comando chegou cifrada ao banco pelo mesmo caminho do
// formulário, em vez de ficar pelo meio.
func TestAdmin_ColarComando(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	const comando = `claude mcp add --transport http --scope user xpoz-mcp ` +
		`https://mcp.exemplo.invalid/mcp --header "Authorization: Bearer sk-do-comando"`

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {comando}})
	if status != http.StatusSeeOther {
		t.Fatalf("status de colar = %d, quer 303 (corpo: %s)", status, corpo)
	}

	regs, err := a.repo.Todos(context.Background())
	if err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	}
	if len(regs) != 1 {
		t.Fatalf("upstreams gravados = %d, quer 1", len(regs))
	}
	reg := regs[0]
	switch {
	case reg.Nome != "xpoz-mcp":
		t.Errorf("nome gravado = %q, quer xpoz-mcp", reg.Nome)
	case reg.Tipo != upstream.TipoHTTP:
		t.Errorf("tipo gravado = %q, quer http", reg.Tipo)
	case reg.URL != "https://mcp.exemplo.invalid/mcp":
		t.Errorf("url gravada = %q, quer a do comando", reg.URL)
	case !reg.Habilitado:
		t.Error("o upstream nasceu desabilitado; o comando descreve um servidor para usar")
	}

	// A credencial precisa ter chegado ao banco, e ao slot certo: Authorization
	// é montado pelo bearer, e um header com esse nome nem seria aceito.
	definidas, err := a.repo.CredenciaisDefinidas(context.Background(), reg.ID)
	if err != nil {
		t.Fatalf("credenciais definidas: erro = %v, quer nil", err)
	}
	if len(definidas) != 1 || definidas[0].Tipo != upstream.CredencialBearer {
		t.Fatalf("credenciais = %+v, quer só um bearer", definidas)
	}
	creds, err := a.repo.Credenciais(context.Background(), reg.ID)
	if err != nil {
		t.Fatalf("credenciais: erro = %v, quer nil", err)
	}
	if len(creds) != 1 || creds[0].Tipo != upstream.CredencialBearer {
		t.Fatalf("credenciais decifradas = %+v, quer só um bearer", creds)
	}
	if got := creds[0].Valor.Revelar(); got != "sk-do-comando" {
		t.Errorf("bearer gravado = %q, quer o token do comando", got)
	}
}

// TestAdmin_ColarLevaAsNotasParaODetalhe cobre o que sobrou da tela de
// conferência: o comando pedia coisas que o cadastro não tem — o --scope do
// Claude Code —, e isso precisa aparecer no MCP recém-criado em vez de sumir no
// log. E precisa aparecer uma vez só: a nota é consumida na primeira exibição.
func TestAdmin_ColarLevaAsNotasParaODetalhe(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	const comando = `claude mcp add --transport http --scope user exemplo ` +
		`https://mcp.exemplo.invalid/mcp --header "X-Api-Key: chave-secreta-do-comando"`

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {comando}})
	if status != http.StatusSeeOther {
		t.Fatalf("status de colar = %d, quer 303 (corpo: %s)", status, corpo)
	}
	destino := localDoRedirecionamento(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {strings.Replace(comando, "exemplo ", "exemplo2 ", 1)}})
	if !strings.Contains(destino, "aviso=importado") || !strings.Contains(destino, "notas=") {
		t.Fatalf("destino = %q, quer o aviso e o identificador das notas", destino)
	}

	primeira := obter(t, cliente, a.admin.URL+destino)
	if !strings.Contains(primeira, "escopo") {
		t.Errorf("a tela do MCP não traz a nota do escopo ignorado; corpo = %s", primeira)
	}
	// O valor do header nunca chega à tela, nem pela nota: ela nomeia o header,
	// não o repete.
	if strings.Contains(primeira, "chave-secreta-do-comando") {
		t.Error("a tela do MCP revela o valor do header do comando")
	}

	segunda := obter(t, cliente, a.admin.URL+destino)
	if strings.Contains(segunda, "escopo") {
		t.Error("a nota da importação apareceu duas vezes; ela vale para uma exibição")
	}
}

// TestAdmin_ColarComandoRecusado garante que a recusa volta como a própria tela
// de MCPs — com o comando de volta na caixa para editar — e não grava nada.
func TestAdmin_ColarComandoRecusado(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	// O erro mais provável de todos: o token ainda é o exemplo da documentação.
	const comando = `claude mcp add --transport http xpoz-mcp https://mcp.exemplo.invalid/mcp ` +
		`--header "Authorization: Bearer [your Xpoz API token]"`

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {comando}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer 422 (corpo: %s)", status, corpo)
	}
	if !strings.Contains(corpo, "marcador da documentação") {
		t.Errorf("a recusa não explica o marcador; corpo = %s", corpo)
	}
	// O comando volta para a caixa: sem ele, corrigir o token exigiria copiar
	// tudo da documentação de novo.
	if !strings.Contains(corpo, "mcp.exemplo.invalid") {
		t.Error("a recusa não devolve o comando para edição")
	}
	// E volta na tela de MCPs, não numa página de erro à parte.
	if !strings.Contains(corpo, "Adicionar colando o comando de instalação") {
		t.Error("a recusa não volta para a caixa de colar da lista")
	}

	if regs, err := a.repo.Todos(context.Background()); err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	} else if len(regs) != 0 {
		t.Fatalf("uma recusa gravou %d upstream(s)", len(regs))
	}
}

// TestAdmin_ColarComandoNomeEmUso cobre o segundo clique no mesmo comando: o
// nome já existe, e a mensagem precisa dizer isso em vez de estourar um 500.
func TestAdmin_ColarComandoNomeEmUso(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{{
			Nome: "exemplo", Tipo: upstream.TipoHTTP, URL: "https://ja-existe.invalid/mcp",
			TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
		}}
	}, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {`claude mcp add -t http exemplo https://mcp.exemplo.invalid/mcp`}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer 422 (corpo: %s)", status, corpo)
	}
	if !strings.Contains(corpo, "Já existe um MCP chamado") {
		t.Errorf("a recusa não fala do nome em uso; corpo = %s", corpo)
	}
}

// TestAdmin_ColarComandoVazio cobre o submit sem nada colado.
func TestAdmin_ColarComandoVazio(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {"   "}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer 422 (corpo: %s)", status, corpo)
	}
	if !strings.Contains(corpo, "Cole o comando") {
		t.Errorf("a tela não pede o comando; corpo = %s", corpo)
	}
}

// postar faz o POST de formulário e devolve corpo e status, que é o par que
// todo caso deste arquivo afirma.
func postar(t *testing.T, cliente *http.Client, rota string, valores url.Values) (string, int) {
	t.Helper()

	resp, err := cliente.PostForm(rota, valores)
	if err != nil {
		t.Fatalf("POST %s: erro = %v, quer nil", rota, err)
	}
	defer func() { _ = resp.Body.Close() }()
	corpo, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ler corpo de %s: erro = %v, quer nil", rota, err)
	}
	return string(corpo), resp.StatusCode
}

// localDoRedirecionamento faz o POST e devolve o Location, que é onde o
// identificador das notas viaja.
func localDoRedirecionamento(
	t *testing.T, cliente *http.Client, rota string, valores url.Values,
) string {
	t.Helper()

	resp, err := cliente.PostForm(rota, valores)
	if err != nil {
		t.Fatalf("POST %s: erro = %v, quer nil", rota, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status de %s = %d, quer 303", rota, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// obter busca uma tela e devolve o corpo.
func obter(t *testing.T, cliente *http.Client, rota string) string {
	t.Helper()

	resp, err := cliente.Get(rota)
	if err != nil {
		t.Fatalf("GET %s: erro = %v, quer nil", rota, err)
	}
	defer func() { _ = resp.Body.Close() }()
	corpo, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ler corpo de %s: erro = %v, quer nil", rota, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status de %s = %d, quer 200 (corpo: %s)", rota, resp.StatusCode, corpo)
	}
	return string(corpo)
}

// TestAdmin_ColarDescobreModoOAuth cobre o buraco que o comando de instalação
// tem por natureza: ele não diz nada sobre autenticação.
//
// Sem a descoberta, todo MCP remoto colado nascia em modo estática — o padrão
// de ModoEfetivo —, e um servidor OAuth cadastrado assim nunca chega ao fluxo
// de consentimento: ele fica tomando 401 em laço, sem botão de Autorizar na
// tela, porque o transporte sai sem OAuthHandler.
//
// O servidor do teste publica o documento do RFC 9728 no caminho derivado, que
// é o que a Cloudflare e a Canva publicam de verdade.
func TestAdmin_ColarDescobreModoOAuth(t *testing.T) {
	t.Parallel()

	const metadado = "/.well-known/oauth-protected-resource/mcp"
	recurso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metadado {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource":"` + r.Host + `","authorization_servers":["https://as.test"]}`))
	}))
	t.Cleanup(recurso.Close)

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	destino := localDoRedirecionamento(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {"npx add-mcp '" + recurso.URL + "/mcp'"}})

	regs, err := a.repo.Todos(context.Background())
	if err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	}
	if len(regs) != 1 {
		t.Fatalf("upstreams gravados = %d, quer 1", len(regs))
	}
	if got := regs[0].ModoEfetivo(); got != upstream.ModoOAuth {
		t.Fatalf("modo gravado = %q, quer oauth", got)
	}

	// A troca de modo não pode ser silenciosa: ela aparece na tela do MCP
	// recém-criado, pela mesma guarda de notas dos outros ajustes do comando.
	tela := obter(t, cliente, a.admin.URL+destino)
	if !strings.Contains(tela, "OAuth") {
		t.Errorf("a tela do MCP não diz que o modo virou OAuth; corpo = %s", tela)
	}
}

// TestAdmin_ColarSemMetadadosSegueEstatica é o contrário do teste acima, e o que
// impede a descoberta de virar palpite: servidor que não publica nada continua
// nascendo em estática, que é o cadastro que o admin termina preenchendo o
// bearer.
func TestAdmin_ColarSemMetadadosSegueEstatica(t *testing.T) {
	t.Parallel()

	mudo := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(mudo.Close)

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rotaColar,
		url.Values{"comando": {"npx add-mcp '" + mudo.URL + "/mcp'"}})
	if status != http.StatusSeeOther {
		t.Fatalf("status de colar = %d, quer 303 (corpo: %s)", status, corpo)
	}

	regs, err := a.repo.Todos(context.Background())
	if err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	}
	if len(regs) != 1 {
		t.Fatalf("upstreams gravados = %d, quer 1", len(regs))
	}
	if got := regs[0].ModoEfetivo(); got != upstream.ModoEstatica {
		t.Errorf("modo gravado = %q, quer estatica", got)
	}
}
