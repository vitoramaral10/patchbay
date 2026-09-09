// Este é o único teste do pacote que roda por dentro (package upstream e não
// upstream_test): o que ele prova é o nome dos campos do formulário, e o nome do
// campo é a costura entre o HTML e o Go que nenhum compilador confere. Um
// `env_valor` que virasse `env_value` compilaria, passaria em todo teste de
// domínio, e só apareceria como "salvei o token e ele não chegou".
package upstream

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLerForm(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		corpo string
		quer  func(*testing.T, Form)
	}{
		"upstream stdio completo": {
			corpo: strings.Join([]string{
				"tipo=stdio",
				"nome=arquivos",
				"comando=npx",
				"args=-y%0A%40escopo%2Fservidor%0AC%3A%5CArquivos+de+Programas",
				"env=NODE_ENV%3Dproduction%0ALOG_LEVEL%3Ddebug",
				"timeout_ms=5000",
				"habilitado=1",
				"env_nome=GITHUB_TOKEN",
				"env_valor=ghp_0123",
				"env_nome=OUTRO",
				"env_valor=",
			}, "&"),
			quer: func(t *testing.T, f Form) {
				t.Helper()
				if f.TipoEfetivo() != TipoSTDIO {
					t.Errorf("tipo = %q, quer %q", f.TipoEfetivo(), TipoSTDIO)
				}
				if f.Comando != "npx" {
					t.Errorf("comando = %q, quer npx", f.Comando)
				}
				if !f.Validar() {
					t.Fatalf("Validar() = false, quer true (erros: %v)", f.Erros)
				}
				querArgs := []string{"-y", "@escopo/servidor", "C:\\Arquivos de Programas"}
				if !reflect.DeepEqual(f.Args, querArgs) {
					t.Errorf("args = %q, quer %q", f.Args, querArgs)
				}
				querEnv := map[string]string{"NODE_ENV": "production", "LOG_LEVEL": "debug"}
				if !reflect.DeepEqual(f.Env, querEnv) {
					t.Errorf("env = %v, quer %v", f.Env, querEnv)
				}
				if len(f.EnvSecretos) != 2 {
					t.Fatalf("variáveis sensíveis = %d, quer 2", len(f.EnvSecretos))
				}
				if f.EnvSecretos[0].Nome != "GITHUB_TOKEN" ||
					f.EnvSecretos[0].Valor.Revelar() != "ghp_0123" {
					t.Errorf("primeira variável = %+v, quer GITHUB_TOKEN com valor", f.EnvSecretos[0])
				}
				if f.EnvSecretos[1].Nome != "OUTRO" || !f.EnvSecretos[1].Valor.Vazio() {
					t.Errorf("segunda variável = %+v, quer OUTRO sem valor", f.EnvSecretos[1])
				}
			},
		},
		"limpar viaja por nome e não por posição": {
			// O checkbox só é enviado quando marcado, então ele não pode ser um
			// array paralelo: aqui só a segunda linha está marcada.
			corpo: strings.Join([]string{
				"tipo=stdio",
				"nome=arquivos",
				"comando=npx",
				"timeout_ms=5000",
				"env_nome=A_TOKEN",
				"env_valor=",
				"env_nome=B_TOKEN",
				"env_valor=",
				"env_limpar=B_TOKEN",
			}, "&"),
			quer: func(t *testing.T, f Form) {
				t.Helper()
				if len(f.EnvSecretos) != 2 {
					t.Fatalf("variáveis = %d, quer 2", len(f.EnvSecretos))
				}
				if f.EnvSecretos[0].Limpar {
					t.Error("A_TOKEN marcada para limpar, quer intocada")
				}
				if !f.EnvSecretos[1].Limpar {
					t.Error("B_TOKEN não marcada para limpar, quer marcada")
				}
			},
		},
		"upstream http continua lendo o que sempre leu": {
			corpo: strings.Join([]string{
				"nome=notion",
				"url=https%3A%2F%2Fexemplo.com%2Fmcp",
				"timeout_ms=15000",
				"habilitado=1",
				"bearer=sk-0123",
				"header_nome=X-Api-Key",
				"header_valor=abc",
				"header_limpar=X-Api-Key",
			}, "&"),
			quer: func(t *testing.T, f Form) {
				t.Helper()
				if f.TipoEfetivo() != TipoHTTP {
					t.Errorf("tipo = %q, quer %q: formulário sem tipo é http", f.TipoEfetivo(), TipoHTTP)
				}
				if f.URL != "https://exemplo.com/mcp" {
					t.Errorf("url = %q", f.URL)
				}
				if f.Bearer.Revelar() != "sk-0123" {
					t.Error("bearer não chegou")
				}
				if len(f.Headers) != 1 || f.Headers[0].Nome != "X-Api-Key" || !f.Headers[0].Limpar {
					t.Errorf("headers = %+v, quer X-Api-Key marcado para limpar", f.Headers)
				}
				if len(f.EnvSecretos) != 0 {
					t.Errorf("variáveis sensíveis = %d, quer 0", len(f.EnvSecretos))
				}
			},
		},
		"timeout ilegível cai na faixa inválida em vez de virar 400": {
			corpo: "nome=notion&url=https%3A%2F%2Fx%2Fmcp&timeout_ms=quinze",
			quer: func(t *testing.T, f Form) {
				t.Helper()
				if f.TimeoutMS != 0 {
					t.Errorf("timeout = %d, quer 0", f.TimeoutMS)
				}
				if f.Validar() {
					t.Error("Validar() = true, quer false")
				}
				if f.Erros["timeout_ms"] == "" {
					t.Error("Erros[timeout_ms] vazio, quer a mensagem que diz qual campo está errado")
				}
			},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequest("POST", "/admin/mcps", strings.NewReader(tc.corpo))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			f, err := lerForm(r)
			if err != nil {
				t.Fatalf("lerForm() = %v, quer nil", err)
			}
			tc.quer(t, f)
		})
	}
}
