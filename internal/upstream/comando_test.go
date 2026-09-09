package upstream_test

import (
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestLerComandoDeInstalacao_HTTP cobre a forma que a documentação dos
// servidores remotos publica: transporte explícito, escopo do Claude Code e o
// token no Authorization.
func TestLerComandoDeInstalacao_HTTP(t *testing.T) {
	t.Parallel()

	const comando = `claude mcp add --transport http --scope user xpoz-mcp ` +
		`https://mcp.xpoz.ai/mcp --header "Authorization: Bearer sk-abc123"`

	imp, err := upstream.LerComandoDeInstalacao(comando)
	if err != nil {
		t.Fatalf("LerComandoDeInstalacao() = %v, quer sucesso", err)
	}
	if imp.Form.Nome != "xpoz-mcp" {
		t.Errorf("Nome = %q, quer xpoz-mcp", imp.Form.Nome)
	}
	if imp.Form.TipoEfetivo() != upstream.TipoHTTP {
		t.Errorf("Tipo = %q, quer http", imp.Form.TipoEfetivo())
	}
	if imp.Form.URL != "https://mcp.xpoz.ai/mcp" {
		t.Errorf("URL = %q, quer https://mcp.xpoz.ai/mcp", imp.Form.URL)
	}
	// O Authorization vira bearer e não header: o slot de header recusa
	// Authorization, porque é o bearer que o monta.
	if got := imp.Form.Bearer.Revelar(); got != "sk-abc123" {
		t.Errorf("Bearer = %q, quer sk-abc123", got)
	}
	if len(imp.Form.Headers) != 0 {
		t.Errorf("Headers = %v, quer nenhum", imp.Form.Headers)
	}
	if imp.Escopo != "user" {
		t.Errorf("Escopo = %q, quer user", imp.Escopo)
	}
	if len(imp.Avisos) != 1 || !strings.Contains(imp.Avisos[0], "escopo") {
		t.Errorf("Avisos = %v, quer um aviso sobre o escopo", imp.Avisos)
	}
	// O formulário precisa sair pronto para gravar, com os padrões da tela.
	if !imp.Form.Habilitado || imp.Form.TimeoutMS != upstream.TimeoutPadraoMS {
		t.Errorf("padrões do formulário perdidos: habilitado=%v timeout=%d",
			imp.Form.Habilitado, imp.Form.TimeoutMS)
	}
	if !imp.Form.Validar() {
		t.Errorf("Validar() recusou o que o comando descrevia: %v", imp.Form.Erros)
	}
}

// TestLerComandoDeInstalacao_STDIO cobre o processo local, com o `--` separando
// as opções do claude dos argumentos do programa.
func TestLerComandoDeInstalacao_STDIO(t *testing.T) {
	t.Parallel()

	const comando = `claude mcp add filesystem -e GITHUB_TOKEN=ghp_1 ` +
		`-- npx -y @modelcontextprotocol/server-filesystem "C:\dados meus"`

	imp, err := upstream.LerComandoDeInstalacao(comando)
	if err != nil {
		t.Fatalf("LerComandoDeInstalacao() = %v, quer sucesso", err)
	}
	if imp.Form.TipoEfetivo() != upstream.TipoSTDIO {
		t.Fatalf("Tipo = %q, quer stdio", imp.Form.TipoEfetivo())
	}
	if imp.Form.Comando != "npx" {
		t.Errorf("Comando = %q, quer npx", imp.Form.Comando)
	}
	// Um argumento por linha: é o que a caixa de texto do formulário espera, e
	// o `-y` precisa ter sobrevivido ao parser de opções.
	quer := "-y\n@modelcontextprotocol/server-filesystem\nC:\\dados meus"
	if imp.Form.ArgsTexto != quer {
		t.Errorf("ArgsTexto = %q, quer %q", imp.Form.ArgsTexto, quer)
	}
	if len(imp.Form.EnvSecretos) != 1 {
		t.Fatalf("EnvSecretos = %v, quer uma variável", imp.Form.EnvSecretos)
	}
	if e := imp.Form.EnvSecretos[0]; e.Nome != "GITHUB_TOKEN" || e.Valor.Revelar() != "ghp_1" {
		t.Errorf("EnvSecretos[0] = %+v, quer GITHUB_TOKEN cifrado", e)
	}
	if !imp.Form.Validar() {
		t.Errorf("Validar() recusou o que o comando descrevia: %v", imp.Form.Erros)
	}
	if imp.Form.Args[0] != "-y" {
		t.Errorf("Args = %v, quer o -y como primeiro argumento", imp.Form.Args)
	}
}

// TestLerComandoDeInstalacao_Aceito varre as variações de forma que chegam do
// copiar-colar e que precisam produzir o mesmo cadastro.
func TestLerComandoDeInstalacao_Aceito(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		comando   string
		querTipo  string
		querNome  string
		querURL   string
		querToken string
	}{
		"sem o executável na frente": {
			comando:  `mcp add -t http exemplo https://exemplo.com/mcp`,
			querTipo: upstream.TipoHTTP, querNome: "exemplo", querURL: "https://exemplo.com/mcp",
		},
		"com o cifrão do prompt": {
			comando:  `$ claude mcp add -t sse exemplo https://exemplo.com/sse`,
			querTipo: upstream.TipoSSE, querNome: "exemplo", querURL: "https://exemplo.com/sse",
		},
		"com prompt completo do shell": {
			comando:  `vitor@maquina:~$ claude mcp add -t http exemplo https://exemplo.com/mcp`,
			querTipo: upstream.TipoHTTP, querNome: "exemplo", querURL: "https://exemplo.com/mcp",
		},
		"quebrado em várias linhas": {
			comando: "claude mcp add --transport http exemplo \\\n" +
				"  https://exemplo.com/mcp \\\n" +
				"  --header \"Authorization: Bearer sk-x\"",
			querTipo: upstream.TipoHTTP, querNome: "exemplo",
			querURL: "https://exemplo.com/mcp", querToken: "sk-x",
		},
		"opção com valor colado por igual": {
			comando:  `claude mcp add --transport=http exemplo https://exemplo.com/mcp`,
			querTipo: upstream.TipoHTTP, querNome: "exemplo", querURL: "https://exemplo.com/mcp",
		},
		"aspas tipográficas do site": {
			comando:  "claude mcp add -t http exemplo https://exemplo.com/mcp -H “X-Api-Key: abc”",
			querTipo: upstream.TipoHTTP, querNome: "exemplo", querURL: "https://exemplo.com/mcp",
		},
		"sem transporte, destino é URL": {
			comando:  `claude mcp add exemplo https://exemplo.com/mcp`,
			querTipo: upstream.TipoHTTP, querNome: "exemplo", querURL: "https://exemplo.com/mcp",
		},
		"sem transporte, destino é programa": {
			comando:  `claude mcp add exemplo uvx mcp-server-git`,
			querTipo: upstream.TipoSTDIO, querNome: "exemplo",
		},
		"aspas simples em volta do header": {
			comando: `claude mcp add -t http exemplo https://exemplo.com/mcp ` +
				`-H 'Authorization: Bearer sk-y'`,
			querTipo: upstream.TipoHTTP, querNome: "exemplo",
			querURL: "https://exemplo.com/mcp", querToken: "sk-y",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			imp, err := upstream.LerComandoDeInstalacao(tc.comando)
			if err != nil {
				t.Fatalf("LerComandoDeInstalacao() = %v, quer sucesso", err)
			}
			if imp.Form.TipoEfetivo() != tc.querTipo {
				t.Errorf("Tipo = %q, quer %q", imp.Form.TipoEfetivo(), tc.querTipo)
			}
			if imp.Form.Nome != tc.querNome {
				t.Errorf("Nome = %q, quer %q", imp.Form.Nome, tc.querNome)
			}
			if imp.Form.URL != tc.querURL {
				t.Errorf("URL = %q, quer %q", imp.Form.URL, tc.querURL)
			}
			if got := imp.Form.Bearer.Revelar(); got != tc.querToken {
				t.Errorf("Bearer = %q, quer %q", got, tc.querToken)
			}
		})
	}
}

// TestLerComandoDeInstalacao_Recusado garante que o que o patchbay não sabe
// gravar volta como texto de tela, e não como cadastro silenciosamente
// diferente do que o comando pedia.
func TestLerComandoDeInstalacao_Recusado(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		comando   string
		querTexto string
	}{
		"campo em branco": {comando: "   \n  ", querTexto: "Cole o comando"},
		"outro comando":   {comando: "npm install algo", querTexto: "Não reconheci"},
		"remover em vez de adicionar": {
			comando: "claude mcp remove exemplo", querTexto: "Não reconheci",
		},
		"add-json ainda não": {
			comando: `claude mcp add-json exemplo '{"type":"http"}'`, querTexto: "add-json",
		},
		"sem nome": {comando: "claude mcp add", querTexto: "Faltou o nome"},
		"só o nome": {
			comando: "claude mcp add exemplo", querTexto: "sem a URL",
		},
		"opção desconhecida": {
			comando:   "claude mcp add --tudo exemplo https://exemplo.com/mcp",
			querTexto: "Não conheço a opção",
		},
		"opção sem valor no fim": {
			comando:   "claude mcp add exemplo https://exemplo.com/mcp --header",
			querTexto: "ficou sem valor",
		},
		"aspa aberta": {
			comando:   `claude mcp add -t http exemplo https://exemplo.com/mcp -H "X: y`,
			querTexto: "aspa aberta",
		},
		"header fora do formato": {
			comando:   `claude mcp add -t http exemplo https://exemplo.com/mcp -H "X-Api-Key abc"`,
			querTexto: "formato Nome: valor",
		},
		"authorization que não é bearer": {
			comando: `claude mcp add -t http exemplo https://exemplo.com/mcp ` +
				`-H "Authorization: Basic dXNlcjpzZW5oYQ=="`,
			querTexto: "não é Bearer",
		},
		"dois authorization": {
			comando: `claude mcp add -t http exemplo https://exemplo.com/mcp ` +
				`-H "Authorization: Bearer a" -H "Authorization: Bearer b"`,
			querTexto: "dois Authorization",
		},
		"header em servidor stdio": {
			comando:   `claude mcp add exemplo -H "X-Api-Key: abc" -- npx foo`,
			querTexto: "Header é coisa de HTTP",
		},
		"env em servidor http": {
			comando:   `claude mcp add -t http exemplo https://exemplo.com/mcp -e TOKEN=abc`,
			querTexto: "só chega a um processo local",
		},
		"env fora do formato": {
			comando:   `claude mcp add exemplo -e TOKEN -- npx foo`,
			querTexto: "formato NOME=valor",
		},
		"transporte http com destino que não é URL": {
			comando:   `claude mcp add -t http exemplo npx`,
			querTexto: "não é uma URL",
		},
		"transporte stdio com destino que é URL": {
			comando:   `claude mcp add -t stdio exemplo https://exemplo.com/mcp`,
			querTexto: "faltou `--transport http`",
		},
		"transporte desconhecido": {
			comando:   `claude mcp add -t grpc exemplo https://exemplo.com/mcp`,
			querTexto: "Transporte desconhecido",
		},
		"argumento sobrando depois da URL": {
			comando:   `claude mcp add -t http exemplo https://exemplo.com/mcp npx foo`,
			querTexto: "não recebe argumentos",
		},
		// O token de exemplo da documentação é o erro mais provável de todos, e
		// gravá-lo produziria um 401 dias depois, longe do cadastro.
		"marcador entre colchetes": {
			comando: `claude mcp add -t http exemplo https://exemplo.com/mcp ` +
				`-H "Authorization: Bearer [your Xpoz API token]"`,
			querTexto: "marcador da documentação",
		},
		"marcador entre sinais": {
			comando: `claude mcp add -t http exemplo https://exemplo.com/mcp ` +
				`-H "X-Api-Key: <YOUR_KEY>"`,
			querTexto: "marcador da documentação",
		},
		"marcador com your no começo": {
			comando: `claude mcp add -t http exemplo https://exemplo.com/mcp ` +
				`-H "Authorization: Bearer YOUR_API_TOKEN"`,
			querTexto: "marcador da documentação",
		},
		"marcador de variável de shell": {
			comando:   `claude mcp add exemplo -e TOKEN=${GITHUB_TOKEN} -- npx foo`,
			querTexto: "marcador da documentação",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			imp, err := upstream.LerComandoDeInstalacao(tc.comando)
			if err == nil {
				t.Fatalf("LerComandoDeInstalacao() aceitou %q e devolveu %+v", tc.comando, imp.Form)
			}
			if !strings.Contains(err.Error(), tc.querTexto) {
				t.Errorf("erro = %q, quer conter %q", err.Error(), tc.querTexto)
			}
		})
	}
}

// TestLerComandoDeInstalacao_NaoVazaSegredo garante que nenhuma mensagem de
// recusa devolve o token para a tela: a mensagem nomeia o header, nunca o valor.
func TestLerComandoDeInstalacao_NaoVazaSegredo(t *testing.T) {
	t.Parallel()

	const segredo = "sk-nao-pode-aparecer"
	casos := []string{
		`claude mcp add -t http exemplo https://exemplo.com/mcp ` +
			`-H "Authorization: Basic ` + segredo + `"`,
		`claude mcp add -t http exemplo https://exemplo.com/mcp ` +
			`-H "Authorization: Bearer a" -H "Authorization: Bearer ` + segredo + `"`,
		`claude mcp add -t http exemplo https://exemplo.com/mcp -e TOKEN=` + segredo,
	}

	for _, comando := range casos {
		if _, err := upstream.LerComandoDeInstalacao(comando); err == nil {
			t.Errorf("comando %q foi aceito, quer recusa", comando)
		} else if strings.Contains(err.Error(), segredo) {
			t.Errorf("a mensagem de recusa carrega o segredo: %q", err.Error())
		}
	}
}

// TestLerComandoDeInstalacao_ComandoGrande recusa o corpo absurdo antes de ele
// virar formulário, como o import de YAML faz com o arquivo colado.
func TestLerComandoDeInstalacao_ComandoGrande(t *testing.T) {
	t.Parallel()

	grande := "claude mcp add exemplo " + strings.Repeat("a", upstream.LimiteDoComando)
	if _, err := upstream.LerComandoDeInstalacao(grande); err == nil {
		t.Fatal("LerComandoDeInstalacao() aceitou um comando acima do limite")
	}
}
