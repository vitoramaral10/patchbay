package upstream_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestAdmin_ImportarComando percorre o fluxo inteiro pela borda HTTP: colar,
// conferir e criar.
//
// O que ele prova e nenhum teste de domínio prova é a costura: o nome do campo
// do formulário (`comando`), a rota de aplicar, e — o que mais importa — que a
// credencial que veio na linha de comando chegou cifrada ao banco pelo mesmo
// caminho do formulário, em vez de ficar pelo meio.
func TestAdmin_ImportarComando(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	const comando = `claude mcp add --transport http --scope user xpoz-mcp ` +
		`https://mcp.exemplo.invalid/mcp --header "Authorization: Bearer sk-do-comando"`

	// 1. Conferência: mostra o que seria criado, e não grava nada.
	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar",
		url.Values{"comando": {comando}})
	if status != http.StatusOK {
		t.Fatalf("status da conferência = %d, quer 200 (corpo: %s)", status, corpo)
	}
	for _, trecho := range []string{"xpoz-mcp", "https://mcp.exemplo.invalid/mcp", "streamable http"} {
		if !strings.Contains(corpo, trecho) {
			t.Errorf("a tela de conferência não mostra %q", trecho)
		}
	}
	// O token não pode aparecer em lugar nenhum da conferência: o resumo diz
	// que há um bearer, nunca qual é.
	if strings.Contains(corpo, "sk-do-comando") {
		t.Error("a tela de conferência revela o token do comando")
	}
	if regs, err := a.repo.Todos(context.Background()); err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	} else if len(regs) != 0 {
		t.Fatalf("conferir gravou %d upstream(s); ela não pode escrever nada", len(regs))
	}

	// 2. Aplicar: cria o MCP e redireciona para o detalhe dele.
	corpo, status = postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar/aplicar",
		url.Values{"comando": {comando}})
	if status != http.StatusSeeOther {
		t.Fatalf("status de aplicar = %d, quer 303 (corpo: %s)", status, corpo)
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

// TestAdmin_ConferenciaNaoLevaOSegredoParaATela cobre a razão de o comando
// ficar guardado no processo em vez de voltar num campo escondido: entre
// conferir e criar, o token não pode estar dentro de nenhum HTML.
//
// O identificador da conferência é o que viaja, e ele vale uma vez só — o
// segundo clique no mesmo botão não pode tentar criar de novo.
func TestAdmin_ConferenciaNaoLevaOSegredoParaATela(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	const comando = `claude mcp add -t http exemplo https://mcp.exemplo.invalid/mcp ` +
		`-H "X-Api-Key: chave-secreta-do-comando"`

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar",
		url.Values{"comando": {comando}})
	if status != http.StatusOK {
		t.Fatalf("status da conferência = %d, quer 200 (corpo: %s)", status, corpo)
	}
	// O nome do header aparece — é o que permite conferir que foi entendido —,
	// o valor não. E o comando inteiro também não: ele carrega o valor junto.
	if !strings.Contains(corpo, "X-Api-Key") {
		t.Error("a conferência não diz qual header o comando trazia")
	}
	if strings.Contains(corpo, "chave-secreta-do-comando") {
		t.Error("a tela de conferência revela o valor do header")
	}
	if strings.Contains(corpo, "-H ") {
		t.Error("a tela de conferência devolve o comando bruto ao navegador")
	}

	pendencia := pendenciaDoCorpo(t, corpo)
	corpo, status = postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar/aplicar",
		url.Values{"pendencia": {pendencia}})
	if status != http.StatusSeeOther {
		t.Fatalf("status de aplicar = %d, quer 303 (corpo: %s)", status, corpo)
	}

	// Segundo clique no mesmo botão: a pendência já foi consumida.
	corpo, status = postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar/aplicar",
		url.Values{"pendencia": {pendencia}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status do segundo clique = %d, quer 422 (corpo: %s)", status, corpo)
	}
	if !strings.Contains(corpo, "não vale mais") {
		t.Errorf("a tela não explica a conferência consumida; corpo = %s", corpo)
	}
	if regs, err := a.repo.Todos(context.Background()); err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	} else if len(regs) != 1 {
		t.Errorf("upstreams gravados = %d, quer 1: o segundo clique não pode duplicar", len(regs))
	}
}

// TestAdmin_ConferenciaVoltaParaEdicao cobre o "voltar e editar": o comando
// guardado volta para a caixa, para o admin corrigir sem colar tudo de novo.
func TestAdmin_ConferenciaVoltaParaEdicao(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	const comando = `claude mcp add -t http exemplo https://mcp.exemplo.invalid/mcp`
	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar",
		url.Values{"comando": {comando}})
	if status != http.StatusOK {
		t.Fatalf("status da conferência = %d, quer 200 (corpo: %s)", status, corpo)
	}

	corpo, status = postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar",
		url.Values{"pendencia": {pendenciaDoCorpo(t, corpo)}, "editar": {"1"}})
	if status != http.StatusOK {
		t.Fatalf("status do voltar = %d, quer 200 (corpo: %s)", status, corpo)
	}
	if !strings.Contains(corpo, "mcp.exemplo.invalid") {
		t.Errorf("o voltar não devolveu o comando para a caixa; corpo = %s", corpo)
	}
	if regs, err := a.repo.Todos(context.Background()); err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	} else if len(regs) != 0 {
		t.Errorf("o voltar gravou %d upstream(s)", len(regs))
	}
}

// pendenciaDoCorpo extrai da tela de conferência o identificador que os dois
// botões dela reenviam.
func pendenciaDoCorpo(t *testing.T, corpo string) string {
	t.Helper()

	const marca = `name="pendencia" value="`
	i := strings.Index(corpo, marca)
	if i < 0 {
		t.Fatalf("a tela de conferência não traz o campo de pendência; corpo = %s", corpo)
	}
	resto := corpo[i+len(marca):]
	fim := strings.IndexByte(resto, '"')
	if fim <= 0 {
		t.Fatalf("campo de pendência malformado; corpo = %s", corpo)
	}
	return resto[:fim]
}

// TestAdmin_ImportarComandoRecusado garante que a recusa volta como tela de
// correção — com o comando de volta na caixa para editar — e não grava nada.
func TestAdmin_ImportarComandoRecusado(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	// O erro mais provável de todos: o token ainda é o exemplo da documentação.
	const comando = `claude mcp add --transport http xpoz-mcp https://mcp.exemplo.invalid/mcp ` +
		`--header "Authorization: Bearer [your Xpoz API token]"`

	for _, rota := range []string{"/importar", "/importar/aplicar"} {
		corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+rota,
			url.Values{"comando": {comando}})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status de %s = %d, quer 422 (corpo: %s)", rota, status, corpo)
		}
		if !strings.Contains(corpo, "marcador da documentação") {
			t.Errorf("a recusa em %s não explica o marcador; corpo = %s", rota, corpo)
		}
		// O comando volta para a caixa: sem ele, corrigir o token exigiria
		// copiar tudo da documentação de novo.
		if !strings.Contains(corpo, "mcp.exemplo.invalid") {
			t.Errorf("a recusa em %s não devolve o comando para edição", rota)
		}
	}

	if regs, err := a.repo.Todos(context.Background()); err != nil {
		t.Fatalf("todos: erro = %v, quer nil", err)
	} else if len(regs) != 0 {
		t.Fatalf("uma recusa gravou %d upstream(s)", len(regs))
	}
}

// TestAdmin_ImportarComandoNomeEmUso cobre o segundo clique no mesmo comando: o
// nome já existe, e a mensagem precisa dizer isso em vez de estourar um 500.
func TestAdmin_ImportarComandoNomeEmUso(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{{
			Nome: "exemplo", Tipo: upstream.TipoHTTP, URL: "https://ja-existe.invalid/mcp",
			TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
		}}
	}, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar/aplicar",
		url.Values{"comando": {`claude mcp add -t http exemplo https://mcp.exemplo.invalid/mcp`}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer 422 (corpo: %s)", status, corpo)
	}
	if !strings.Contains(corpo, "Já existe um MCP chamado") {
		t.Errorf("a recusa não fala do nome em uso; corpo = %s", corpo)
	}
}

// TestAdmin_ImportarComandoVazio cobre o submit sem nada colado.
func TestAdmin_ImportarComandoVazio(t *testing.T) {
	t.Parallel()

	a := novoAmbiente(t, func(string) []upstream.Form { return nil }, opcoesAmbiente{})
	cliente := clienteSemSeguir()

	corpo, status := postar(t, cliente, a.admin.URL+webui.RotaUpstreams+"/importar",
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
