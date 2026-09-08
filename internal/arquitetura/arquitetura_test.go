// Package arquitetura_test guarda o teste que faz a regra de dependência da
// seção 10 do estudo quebrar o build.
//
// Não existe código de produção neste diretório de propósito: a regra é uma
// propriedade do módulo, não de um pacote.
package arquitetura_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const modulo = "github.com/vitoramaral10/patchbay"

// toleradas são as arestas feature→feature que o estudo aceita: importar só os
// tipos de outra feature, quando não cria ciclo.
//
// endpoint importa catalogo pelo tipo catalogo.Ferramenta, mas declara
// endpoint.Catalogo no próprio pacote em vez de usar o *catalogo.Servico. Toda
// aresta nova aqui precisa da mesma justificativa escrita.
var toleradas = map[string][]string{
	"endpoint": {"catalogo"},
}

func TestFeatureNaoImportaFeature(t *testing.T) {
	t.Parallel()

	pacotes := listarPacotes(t)

	for _, pacote := range pacotes {
		feature, ok := nomeDaFeature(pacote)
		if !ok {
			continue
		}
		t.Run(feature, func(t *testing.T) {
			t.Parallel()

			for _, importado := range importesInternos(t, pacote) {
				outra, ok := nomeDaFeature(importado)
				if !ok || outra == feature {
					continue
				}
				if slices.Contains(toleradas[feature], outra) {
					continue
				}
				t.Errorf("internal/%s importa internal/%s (%s); feature não importa feature",
					feature, outra, importado)
			}
		})
	}
}

func TestPlatformNaoImportaFeature(t *testing.T) {
	t.Parallel()

	for _, pacote := range listarPacotes(t) {
		if !strings.HasPrefix(pacote, modulo+"/internal/platform/") {
			continue
		}
		t.Run(strings.TrimPrefix(pacote, modulo+"/internal/platform/"), func(t *testing.T) {
			t.Parallel()

			for _, importado := range importesInternos(t, pacote) {
				if feature, ok := nomeDaFeature(importado); ok {
					t.Errorf("%s importa internal/%s; platform é infraestrutura e não conhece feature",
						pacote, feature)
				}
			}
		})
	}
}

// nomeDaFeature devolve o nome da feature de um pacote sob internal/, ou false
// se o pacote não é uma feature (platform e tudo fora de internal/).
func nomeDaFeature(pacote string) (string, bool) {
	resto, ok := semPrefixo(pacote, modulo+"/internal/")
	if !ok {
		return "", false
	}
	if strings.HasPrefix(resto, "platform/") || resto == "platform" {
		return "", false
	}
	if i := strings.IndexByte(resto, '/'); i >= 0 {
		resto = resto[:i]
	}
	// O próprio diretório do teste de arquitetura não é feature.
	if resto == "arquitetura" {
		return "", false
	}
	return resto, true
}

func semPrefixo(s, prefixo string) (string, bool) {
	if !strings.HasPrefix(s, prefixo) {
		return "", false
	}
	return s[len(prefixo):], true
}

func listarPacotes(t *testing.T) []string {
	t.Helper()
	return rodarGoList(t, "{{.ImportPath}}", modulo+"/...")
}

// importesInternos devolve os pacotes do próprio módulo que pacote importa,
// direta ou indiretamente. Deps e não Imports: importar em cadeia por um pacote
// intermediário burla a regra do mesmo jeito.
func importesInternos(t *testing.T, pacote string) []string {
	t.Helper()
	var out []string
	for _, dep := range rodarGoList(t, "{{join .Deps \"\\n\"}}", pacote) {
		if strings.HasPrefix(dep, modulo+"/") {
			out = append(out, dep)
		}
	}
	return out
}

func rodarGoList(t *testing.T, formato string, alvo string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", formato, alvo)
	saida, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -f %q %s: erro = %v\n%s", formato, alvo, err, saida)
	}
	var out []string
	for _, linha := range strings.Split(strings.ReplaceAll(string(saida), "\r\n", "\n"), "\n") {
		if linha = strings.TrimSpace(linha); linha != "" {
			out = append(out, linha)
		}
	}
	return out
}
