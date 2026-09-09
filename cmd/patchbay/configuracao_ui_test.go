package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/configuracao"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// TestUI_ConfiguracaoExportaEImporta percorre a tela inteira: baixar o YAML,
// colá-lo com uma edição, ver o plano e aplicar.
//
// O caminho da UI é diferente do da linha de comando num ponto que importa: aqui
// o gerente está no ar, e o import precisa valer sem esperar o próximo boot.
func TestUI_ConfiguracaoExportaEImporta(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)
	idUp := u.criarUpstream(t, "exemplo", "https://exemplo.invalido/mcp")
	u.criarEndpoint(t, "pessoal", "Pessoal", idUp)

	// A navegação leva à tela, e a tela oferece o download.
	res := u.abrir(t, webui.RotaConfiguracao)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("abrir configuração: status = %d, quer %d", res.StatusCode, http.StatusOK)
	}
	if texto := corpo(t, res); !strings.Contains(texto, configuracao.RotaExportar) {
		t.Errorf("tela sem o link de export:\n%s", texto)
	}

	baixado := u.abrir(t, configuracao.RotaExportar)
	if baixado.StatusCode != http.StatusOK {
		t.Fatalf("exportar: status = %d, quer %d", baixado.StatusCode, http.StatusOK)
	}
	if disp := baixado.Header.Get("Content-Disposition"); !strings.Contains(disp, "patchbay.yaml") {
		t.Errorf("Content-Disposition = %q, quer o nome do arquivo", disp)
	}
	yamlOriginal := corpo(t, baixado)
	if !strings.Contains(yamlOriginal, "nome: exemplo") {
		t.Fatalf("YAML exportado sem o upstream criado:\n%s", yamlOriginal)
	}

	// Uma edição de verdade no arquivo, como quem versiona a configuração.
	editado := strings.Replace(yamlOriginal, "timeout_ms: 5000", "timeout_ms: 9000", 1)
	if editado == yamlOriginal {
		t.Fatalf("o YAML não trazia o timeout esperado:\n%s", yamlOriginal)
	}

	plano := u.enviarForm(t, configuracao.RotaPlano, url.Values{"yaml": {editado}})
	if plano.StatusCode != http.StatusOK {
		t.Fatalf("plano: status = %d, quer %d (corpo: %q)", plano.StatusCode, http.StatusOK, corpo(t, plano))
	}
	textoPlano := corpo(t, plano)
	for _, quer := range []string{string(configuracao.OperacaoAtualizar), "exemplo"} {
		if !strings.Contains(textoPlano, quer) {
			t.Errorf("plano sem %q:\n%s", quer, textoPlano)
		}
	}

	// Nada foi escrito ao ver o plano: é a promessa do passo intermediário.
	reg, err := u.app.repoUpstream.Obter(t.Context(), idUp)
	if err != nil {
		t.Fatalf("reler upstream: erro = %v, quer nil", err)
	}
	if reg.TimeoutMS != 5000 {
		t.Fatalf("timeout depois do plano = %d, quer 5000: ver o plano não escreve", reg.TimeoutMS)
	}

	aplicado := u.enviarForm(t, configuracao.RotaAplicar, url.Values{"yaml": {editado}})
	if aplicado.StatusCode != http.StatusOK {
		t.Fatalf("aplicar: status = %d, quer %d (corpo: %q)",
			aplicado.StatusCode, http.StatusOK, corpo(t, aplicado))
	}
	if texto := corpo(t, aplicado); !strings.Contains(texto, "aplicado") {
		t.Errorf("relatório sem a linha de aplicação:\n%s", texto)
	}

	reg, err = u.app.repoUpstream.Obter(t.Context(), idUp)
	if err != nil {
		t.Fatalf("reler upstream: erro = %v, quer nil", err)
	}
	if reg.TimeoutMS != 9000 {
		t.Errorf("timeout depois do import = %d, quer 9000", reg.TimeoutMS)
	}
	// E o gerente recebeu a configuração nova sem reiniciar o processo.
	if s, sob := u.app.gerente.Situacao(idUp); !sob || s.Config.Timeout.Milliseconds() != 9000 {
		t.Errorf("timeout na supervisão = %v (sob supervisão = %v), quer 9s",
			s.Config.Timeout, sob)
	}
}

// TestUI_ConfiguracaoRecusaYAMLInvalido: a tela devolve o motivo no campo, com o
// que a pessoa colou de volta, em vez de um 500 sem explicação.
func TestUI_ConfiguracaoRecusaYAMLInvalido(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	casos := map[string]struct {
		yaml  string
		texto string
	}{
		"versão desconhecida": {yaml: "versao: 42\n", texto: "este patchbay lê 1"},
		"campo que não existe": {
			yaml:  "versao: 1\nupstreams:\n  - nome: a\n    timeout: 9\n",
			texto: "Não foi possível ler o YAML",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			// Os subtestes dividem a mesma UI e a mesma sessão: o POST de plano
			// não escreve nada, então rodar em paralelo é seguro — e é o que a
			// recusa precisa provar, que ela é resposta e não estado.
			t.Parallel()

			res := u.enviarForm(t, configuracao.RotaPlano, url.Values{"yaml": {tc.yaml}})
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusUnprocessableEntity)
			}
			texto := corpo(t, res)
			if !strings.Contains(texto, tc.texto) {
				t.Errorf("tela sem o motivo %q:\n%s", tc.texto, texto)
			}
			if !strings.Contains(texto, "versao") {
				t.Errorf("tela não reexibiu o que foi colado:\n%s", texto)
			}
		})
	}
}
