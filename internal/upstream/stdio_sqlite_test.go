package upstream_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// formSTDIOBase é o formulário de um upstream stdio pronto para gravar.
func formSTDIOBase(nome string) upstream.Form {
	f := upstream.Form{
		Nome: nome, Tipo: upstream.TipoSTDIO,
		Comando:    "npx",
		ArgsTexto:  "-y\n@modelcontextprotocol/server-filesystem\nC:\\Arquivos de Programas",
		EnvTexto:   "NODE_ENV=production\nLOG_LEVEL=debug",
		TimeoutMS:  upstream.TimeoutPadraoMS,
		Habilitado: true,
	}
	return f
}

// TestRepositorio_STDIOIdaEVolta prova que comando, argumentos e ambiente
// sobrevivem ao banco do jeito que foram digitados.
//
// Argumento com espaço e valor com igual dentro estão no caso de propósito: são
// os dois que qualquer serialização por separador quebraria em silêncio.
func TestRepositorio_STDIOIdaEVolta(t *testing.T) {
	t.Parallel()

	sut, st := repositorioDeTeste(t)
	ctx := context.Background()

	f := formSTDIOBase("arquivos")
	f.EnvTexto = "NODE_ENV=production\nURL=https://x/?a=b"
	if !f.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", f.Erros)
	}

	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar() = %v, quer nil", err)
	}

	reg, err := sut.Obter(ctx, id)
	if err != nil {
		t.Fatalf("Obter() = %v, quer nil", err)
	}

	querArgs := []string{"-y", "@modelcontextprotocol/server-filesystem", "C:\\Arquivos de Programas"}
	querEnv := map[string]string{"NODE_ENV": "production", "URL": "https://x/?a=b"}

	if reg.Tipo != upstream.TipoSTDIO {
		t.Errorf("Tipo = %q, quer %q", reg.Tipo, upstream.TipoSTDIO)
	}
	if reg.Comando != "npx" {
		t.Errorf("Comando = %q, quer %q", reg.Comando, "npx")
	}
	if !reflect.DeepEqual(reg.Args, querArgs) {
		t.Errorf("Args = %q, quer %q", reg.Args, querArgs)
	}
	if !reflect.DeepEqual(reg.Env, querEnv) {
		t.Errorf("Env = %v, quer %v", reg.Env, querEnv)
	}

	// E a Config que o gerente supervisiona carrega tudo isso.
	cfg := reg.Config()
	if err := cfg.Validar(); err != nil {
		t.Errorf("Config().Validar() = %v, quer nil", err)
	}
	if cfg.Timeout != time.Duration(upstream.TimeoutPadraoMS)*time.Millisecond {
		t.Errorf("Timeout = %v, quer %v", cfg.Timeout,
			time.Duration(upstream.TimeoutPadraoMS)*time.Millisecond)
	}

	// E o upstream entra na lista dos habilitados com o processo montado.
	cfgs, err := upstream.Habilitados(ctx, st.Leitura())
	if err != nil {
		t.Fatalf("Habilitados() = %v, quer nil", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("habilitados = %d, quer 1", len(cfgs))
	}
	if !reflect.DeepEqual(cfgs[0].Args, querArgs) {
		t.Errorf("Habilitados().Args = %q, quer %q", cfgs[0].Args, querArgs)
	}
	if !reflect.DeepEqual(cfgs[0].Env, querEnv) {
		t.Errorf("Habilitados().Env = %v, quer %v", cfgs[0].Env, querEnv)
	}
}

// TestRepositorio_VariavelSensivelFicaCifradaNoBanco é o requisito da fatia lido
// do lado do disco: quem abre o arquivo .db não encontra o token do processo.
func TestRepositorio_VariavelSensivelFicaCifradaNoBanco(t *testing.T) {
	t.Parallel()

	const token = "ghp_token-de-upstream-stdio-em-claro"

	sut, st := repositorioDeTeste(t)
	ctx := context.Background()

	f := formSTDIOBase("arquivos")
	f.EnvSecretos = []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Valor: token}}
	if !f.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", f.Erros)
	}

	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar() = %v, quer nil", err)
	}

	guardado := colunaCrua(t, st, id, upstream.CredencialEnv, "GITHUB_TOKEN")
	if strings.Contains(guardado, token) {
		t.Fatal("o token está em claro no banco")
	}
	if !strings.HasPrefix(guardado, "pbc1:") {
		t.Errorf("valor guardado = %q, quer o prefixo de versão da cifra", guardado)
	}

	// E volta em claro para quem sabe a chave, que é o gerente montando o
	// ambiente do processo.
	creds, err := sut.Credenciais(ctx, id)
	if err != nil {
		t.Fatalf("Credenciais() = %v, quer nil", err)
	}
	achou := false
	for _, c := range creds {
		if c.Tipo == upstream.CredencialEnv && c.Nome == "GITHUB_TOKEN" {
			achou = true
			if c.Valor.Revelar() != token {
				t.Errorf("valor decifrado = %q, quer o token gravado", c.Valor.Revelar())
			}
		}
	}
	if !achou {
		t.Fatalf("GITHUB_TOKEN não voltou de Credenciais(); veio %d credencial(is)", len(creds))
	}

	// A tela vê o nome e nunca o valor.
	definidas, err := sut.CredenciaisDefinidas(ctx, id)
	if err != nil {
		t.Fatalf("CredenciaisDefinidas() = %v, quer nil", err)
	}
	if len(definidas) != 1 || definidas[0].Tipo != upstream.CredencialEnv ||
		definidas[0].Nome != "GITHUB_TOKEN" {
		t.Fatalf("definidas = %+v, quer só o nome GITHUB_TOKEN", definidas)
	}
}

// TestRepositorio_VariavelSensivelEmBrancoMantemELimparApaga fixa a regra do
// formulário: campo vazio é ausência de mudança, apagar é explícito.
//
// Sem isso, um admin que abrisse a tela só para mexer no timeout apagaria todos
// os tokens do processo sem nenhum aviso.
func TestRepositorio_VariavelSensivelEmBrancoMantemELimparApaga(t *testing.T) {
	t.Parallel()

	sut, _ := repositorioDeTeste(t)
	ctx := context.Background()

	criar := formSTDIOBase("arquivos")
	criar.EnvSecretos = []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Valor: "primeiro"}}
	if !criar.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", criar.Erros)
	}
	id, err := sut.Criar(ctx, criar)
	if err != nil {
		t.Fatalf("Criar() = %v, quer nil", err)
	}

	casos := []struct {
		nome      string
		secretos  []upstream.CampoEnv
		querValor string
		querSumiu bool
	}{
		{
			nome:      "em branco mantém o gravado",
			secretos:  []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Definido: true}},
			querValor: "primeiro",
		},
		{
			nome:      "valor novo substitui",
			secretos:  []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Definido: true, Valor: "segundo"}},
			querValor: "segundo",
		},
		{
			nome:      "limpar apaga",
			secretos:  []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Definido: true, Limpar: true}},
			querSumiu: true,
		},
	}

	// Sequencial e não table-driven paralelo de propósito: cada passo depende do
	// estado que o anterior deixou no banco, que é o que a tela faz de verdade.
	for _, tc := range casos {
		atualizar := formSTDIOBase("arquivos")
		atualizar.ID = id
		atualizar.EnvSecretos = tc.secretos
		if !atualizar.Validar() {
			t.Fatalf("%s: Validar() = false, quer true (erros: %v)", tc.nome, atualizar.Erros)
		}
		if err := sut.Atualizar(ctx, id, atualizar); err != nil {
			t.Fatalf("%s: Atualizar() = %v, quer nil", tc.nome, err)
		}

		creds, err := sut.Credenciais(ctx, id)
		if err != nil {
			t.Fatalf("%s: Credenciais() = %v, quer nil", tc.nome, err)
		}
		valor, existe := "", false
		for _, c := range creds {
			if c.Tipo == upstream.CredencialEnv && c.Nome == "GITHUB_TOKEN" {
				valor, existe = c.Valor.Revelar(), true
			}
		}
		switch {
		case tc.querSumiu && existe:
			t.Fatalf("%s: a variável continua gravada com %q", tc.nome, valor)
		case !tc.querSumiu && valor != tc.querValor:
			t.Fatalf("%s: valor = %q (presente=%v), quer %q", tc.nome, valor, existe, tc.querValor)
		}
	}
}

// TestRepositorio_TipoNaoMudaNaEdicao: o transporte é escolhido na criação. Um
// POST forjado com outro tipo não pode trocar o transporte de um upstream vivo.
func TestRepositorio_TipoNaoMudaNaEdicao(t *testing.T) {
	t.Parallel()

	sut, _ := repositorioDeTeste(t)
	ctx := context.Background()

	criar := formSTDIOBase("arquivos")
	if !criar.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", criar.Erros)
	}
	id, err := sut.Criar(ctx, criar)
	if err != nil {
		t.Fatalf("Criar() = %v, quer nil", err)
	}

	forjado := formSTDIOBase("arquivos")
	forjado.ID = id
	forjado.Tipo = upstream.TipoHTTP
	forjado.URL = "https://exemplo.com/mcp"
	if !forjado.Validar() {
		t.Fatalf("Validar() = false, quer true (erros: %v)", forjado.Erros)
	}
	if err := sut.Atualizar(ctx, id, forjado); err != nil {
		t.Fatalf("Atualizar() = %v, quer nil", err)
	}

	reg, err := sut.Obter(ctx, id)
	if err != nil {
		t.Fatalf("Obter() = %v, quer nil", err)
	}
	if reg.Tipo != upstream.TipoSTDIO {
		t.Errorf("Tipo = %q, quer %q: a edição não muda o transporte", reg.Tipo, upstream.TipoSTDIO)
	}
}
