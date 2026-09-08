package upstream_test

import (
	"reflect"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// formSTDIO é o formulário mínimo que passa, para cada caso mexer num campo só.
func formSTDIO() upstream.Form {
	return upstream.Form{
		Nome:      "arquivos",
		Tipo:      upstream.TipoSTDIO,
		Comando:   "npx",
		TimeoutMS: upstream.TimeoutPadraoMS,
	}
}

func TestForm_ValidarProcesso(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		ajuste    func(*upstream.Form)
		querPassa bool
		querErro  string // chave em Erros que precisa existir
		querArgs  []string
		querEnv   map[string]string
	}{
		"comando simples": {
			ajuste:    func(*upstream.Form) {},
			querPassa: true,
		},
		"sem comando": {
			ajuste:   func(f *upstream.Form) { f.Comando = "" },
			querErro: "comando",
		},
		"comando só de espaço": {
			ajuste:   func(f *upstream.Form) { f.Comando = "   " },
			querErro: "comando",
		},
		"comando com quebra de linha": {
			ajuste:   func(f *upstream.Form) { f.Comando = "npx\nrm -rf /" },
			querErro: "comando",
		},
		"url vazia não é erro em stdio": {
			ajuste:    func(f *upstream.Form) { f.URL = "" },
			querPassa: true,
		},
		"um argumento por linha": {
			ajuste:    func(f *upstream.Form) { f.ArgsTexto = "-y\n@escopo/servidor\nC:\\Arquivos de Programas" },
			querPassa: true,
			querArgs:  []string{"-y", "@escopo/servidor", "C:\\Arquivos de Programas"},
		},
		"linha em branco entre argumentos é descartada": {
			ajuste:    func(f *upstream.Form) { f.ArgsTexto = "-y\n\n\n--porta\n" },
			querPassa: true,
			querArgs:  []string{"-y", "--porta"},
		},
		"argumentos com fim de linha do windows": {
			ajuste:    func(f *upstream.Form) { f.ArgsTexto = "-y\r\n--porta\r\n" },
			querPassa: true,
			querArgs:  []string{"-y", "--porta"},
		},
		"ambiente em pares": {
			ajuste:    func(f *upstream.Form) { f.EnvTexto = "NODE_ENV=production\nLOG_LEVEL=debug" },
			querPassa: true,
			querEnv:   map[string]string{"NODE_ENV": "production", "LOG_LEVEL": "debug"},
		},
		"valor de ambiente pode ter igual dentro": {
			ajuste:    func(f *upstream.Form) { f.EnvTexto = "URL=https://x/?a=b" },
			querPassa: true,
			querEnv:   map[string]string{"URL": "https://x/?a=b"},
		},
		"valor de ambiente pode ser vazio": {
			ajuste:    func(f *upstream.Form) { f.EnvTexto = "VAZIA=" },
			querPassa: true,
			querEnv:   map[string]string{"VAZIA": ""},
		},
		"ambiente sem igual": {
			ajuste:   func(f *upstream.Form) { f.EnvTexto = "NODE_ENV production" },
			querErro: "env",
		},
		"nome de variável com espaço": {
			ajuste:   func(f *upstream.Form) { f.EnvTexto = "NODE ENV=production" },
			querErro: "env",
		},
		"nome de variável começando por dígito": {
			ajuste:   func(f *upstream.Form) { f.EnvTexto = "1VAR=x" },
			querErro: "env",
		},
		"variável repetida": {
			ajuste:   func(f *upstream.Form) { f.EnvTexto = "A=1\nA=2" },
			querErro: "env",
		},
		"argumentos demais": {
			ajuste: func(f *upstream.Form) {
				for range upstream.LimiteDeArgs + 1 {
					f.ArgsTexto += "-v\n"
				}
			},
			querErro: "args",
		},
		"argumento com caractere nulo": {
			ajuste:   func(f *upstream.Form) { f.ArgsTexto = "-y\n@escopo/servidor\x00malicioso" },
			querErro: "args",
		},
		"timeout fora da faixa": {
			ajuste:   func(f *upstream.Form) { f.TimeoutMS = 1 },
			querErro: "timeout_ms",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := formSTDIO()
			tc.ajuste(&sut)

			if got := sut.Validar(); got != tc.querPassa && tc.querErro == "" {
				t.Fatalf("Validar() = %v, quer %v (erros: %v)", got, tc.querPassa, sut.Erros)
			}
			if tc.querErro != "" {
				if sut.Erros[tc.querErro] == "" {
					t.Fatalf("Erros[%q] vazio, quer a mensagem da tela; erros = %v", tc.querErro, sut.Erros)
				}
				return
			}
			if tc.querArgs != nil && !reflect.DeepEqual(sut.Args, tc.querArgs) {
				t.Errorf("Args = %q, quer %q", sut.Args, tc.querArgs)
			}
			if tc.querEnv != nil && !reflect.DeepEqual(sut.Env, tc.querEnv) {
				t.Errorf("Env = %v, quer %v", sut.Env, tc.querEnv)
			}
		})
	}
}

func TestForm_ValidarEnvSecretos(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		envTexto  string
		secretos  []upstream.CampoEnv
		querPassa bool
	}{
		"sem variável nenhuma": {querPassa: true},
		"variável simples": {
			secretos:  []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Valor: "ghp_0123"}},
			querPassa: true,
		},
		"linha em branco é ignorada": {
			secretos:  []upstream.CampoEnv{{}, {}},
			querPassa: true,
		},
		"variável já gravada sem valor novo": {
			secretos:  []upstream.CampoEnv{{Nome: "GITHUB_TOKEN", Definido: true}},
			querPassa: true,
		},
		"valor pode ter espaço e igual": {
			secretos:  []upstream.CampoEnv{{Nome: "CONN", Valor: "host=a b;user=c"}},
			querPassa: true,
		},
		"sem nome": {
			secretos: []upstream.CampoEnv{{Valor: "x"}},
		},
		"nome inválido": {
			secretos: []upstream.CampoEnv{{Nome: "GITHUB-TOKEN", Valor: "x"}},
		},
		"repetida": {
			secretos: []upstream.CampoEnv{
				{Nome: "TOKEN", Valor: "a"},
				{Nome: "TOKEN", Valor: "b"},
			},
		},
		"colide com a lista em claro": {
			envTexto: "TOKEN=publico",
			secretos: []upstream.CampoEnv{{Nome: "TOKEN", Valor: "secreto"}},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := formSTDIO()
			sut.EnvTexto = tc.envTexto
			sut.EnvSecretos = tc.secretos

			if got := sut.Validar(); got != tc.querPassa {
				t.Fatalf("Validar() = %v, quer %v (erros: %v)", got, tc.querPassa, sut.Erros)
			}
			if !tc.querPassa && sut.Erros["env_secreto"] == "" {
				t.Errorf("Erros[env_secreto] vazio, quer a mensagem da tela; erros = %v", sut.Erros)
			}
		})
	}
}

// TestForm_CompletarEnvSecretos: a tela mostra "definida" para o que já existe e
// ainda oferece linhas em branco para o que vai entrar — e o que o admin digitou
// tem prioridade sobre o que veio do banco, porque isto também roda ao reexibir
// um formulário recusado.
func TestForm_CompletarEnvSecretos(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		antes     []upstream.CampoEnv
		definidas []upstream.CredencialDefinida
		querNomes []string
	}{
		"nada gravado": {
			querNomes: []string{"", ""},
		},
		"uma gravada": {
			definidas: []upstream.CredencialDefinida{{Tipo: upstream.CredencialEnv, Nome: "TOKEN"}},
			querNomes: []string{"TOKEN", "", ""},
		},
		"header gravado não vira variável": {
			definidas: []upstream.CredencialDefinida{
				{Tipo: upstream.CredencialHeader, Nome: "X-Api-Key"},
				{Tipo: upstream.CredencialBearer},
			},
			querNomes: []string{"", ""},
		},
		"o digitado não duplica o gravado": {
			antes:     []upstream.CampoEnv{{Nome: "TOKEN", Erro: "algum erro"}},
			definidas: []upstream.CredencialDefinida{{Tipo: upstream.CredencialEnv, Nome: "TOKEN"}},
			querNomes: []string{"TOKEN", "", ""},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := upstream.Form{EnvSecretos: tc.antes}
			sut.CompletarEnvSecretos(tc.definidas)

			got := make([]string, 0, len(sut.EnvSecretos))
			for _, e := range sut.EnvSecretos {
				got = append(got, e.Nome)
			}
			if !reflect.DeepEqual(got, tc.querNomes) {
				t.Fatalf("nomes = %q, quer %q", got, tc.querNomes)
			}
			for _, e := range sut.EnvSecretos {
				if e.Nome != "" && !e.Definido {
					t.Errorf("%s não está marcada como definida", e.Nome)
				}
			}
		})
	}
}

// TestForm_CompletarEnvSecretosPreservaODigitado garante que reexibir um
// formulário recusado não apaga o erro nem o valor que o admin acabou de digitar.
func TestForm_CompletarEnvSecretosPreservaODigitado(t *testing.T) {
	t.Parallel()

	sut := upstream.Form{
		EnvSecretos: []upstream.CampoEnv{{Nome: "TOKEN", Valor: cripto.Segredo("x"), Erro: "algum erro"}},
	}
	sut.CompletarEnvSecretos([]upstream.CredencialDefinida{
		{Tipo: upstream.CredencialEnv, Nome: "TOKEN"},
	})

	if sut.EnvSecretos[0].Erro != "algum erro" {
		t.Errorf("erro da linha = %q, quer preservado", sut.EnvSecretos[0].Erro)
	}
	if sut.EnvSecretos[0].Valor.Revelar() != "x" {
		t.Error("o valor digitado foi perdido ao reexibir o formulário")
	}
	if !sut.EnvSecretos[0].Definido {
		t.Error("Definido = false, quer true: a variável existe no banco")
	}
}

func TestLinhaDeComando(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		comando string
		args    []string
		quer    string
	}{
		"sem argumentos":         {comando: "npx", quer: "npx"},
		"argumentos simples":     {comando: "npx", args: []string{"-y", "servidor"}, quer: "npx -y servidor"},
		"argumento com espaço":   {comando: "node", args: []string{"C:\\Arquivos de Programas\\a.js"}, quer: `node "C:\\Arquivos de Programas\\a.js"`},
		"argumento com aspas":    {comando: "sh", args: []string{`diz "oi"`}, quer: `sh "diz \"oi\""`},
		"argumento vazio some":   {comando: "npx", args: []string{""}, quer: "npx "},
		"comando com caminho":    {comando: "/usr/bin/uvx", args: []string{"pkg"}, quer: "/usr/bin/uvx pkg"},
		"argumento com tabulaçã": {comando: "sh", args: []string{"a\tb"}, quer: "sh \"a\\tb\""},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := upstream.LinhaDeComando(tc.comando, tc.args); got != tc.quer {
				t.Errorf("LinhaDeComando() = %q, quer %q", got, tc.quer)
			}
		})
	}
}

// TestTextoDeEnv fixa a ordem: a caixa de texto é reexibida a cada edição, e uma
// ordem que muda sozinha faz o admin achar que alguém mexeu na configuração.
func TestTextoDeEnv(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		env  map[string]string
		quer string
	}{
		"vazio":     {quer: ""},
		"uma":       {env: map[string]string{"A": "1"}, quer: "A=1"},
		"ordenadas": {env: map[string]string{"C": "3", "A": "1", "B": "2"}, quer: "A=1\nB=2\nC=3"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := upstream.TextoDeEnv(tc.env); got != tc.quer {
				t.Errorf("TextoDeEnv() = %q, quer %q", got, tc.quer)
			}
		})
	}
}

// TestForm_TipoEfetivo: formulário sem tipo é HTTP, para o que já existia
// continuar significando o que significava.
func TestForm_TipoEfetivo(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		tipo string
		quer string
	}{
		"vazio é http":               {tipo: "", quer: upstream.TipoHTTP},
		"http":                       {tipo: upstream.TipoHTTP, quer: upstream.TipoHTTP},
		"stdio":                      {tipo: upstream.TipoSTDIO, quer: upstream.TipoSTDIO},
		"desconhecido cai para http": {tipo: "carta-pombo", quer: upstream.TipoHTTP},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := upstream.Form{Tipo: tc.tipo}
			if got := sut.TipoEfetivo(); got != tc.quer {
				t.Errorf("TipoEfetivo() = %q, quer %q", got, tc.quer)
			}
		})
	}
}
