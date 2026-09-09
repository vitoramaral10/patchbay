package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

const tokenNoComando = "sk-token-que-veio-no-comando"

// TestUI_ComandoColadoChegaAoUpstream fecha o ciclo do "colar o comando de
// instalação": a linha que a documentação do servidor publica entra pela tela e
// sai como Authorization na conexão com o servidor MCP de verdade.
//
// É o mesmo par de afirmações do cadastro à mão, e é por isso que vale ter os
// dois: o token nunca reaparece em tela nenhuma, e chega inteiro ao upstream. O
// caminho novo não pode ter atalho que perca uma das duas — o formulário só
// esconde o segredo porque o Criar o cifra, e é esse Criar que este fluxo tem
// que estar usando.
func TestUI_ComandoColadoChegaAoUpstream(t *testing.T) {
	t.Parallel()

	alvo, headersRecebidos := upstreamComEspiao(t)
	u := subirUI(t)
	u.setup(t)

	comando := `claude mcp add --transport http --scope user vindo-do-comando ` +
		alvo + ` --header "Authorization: Bearer ` + tokenNoComando + `"` +
		` --header "X-Api-Key: ` + headerNaTela + `"`

	// 1. Conferência: mostra o cadastro, sem gravar e sem revelar credencial.
	conferencia := u.enviarForm(t, webui.RotaUpstreams+"/importar", url.Values{"comando": {comando}})
	if conferencia.StatusCode != http.StatusOK {
		t.Fatalf("conferência: status = %d, quer %d", conferencia.StatusCode, http.StatusOK)
	}
	tela := corpo(t, conferencia)
	if !strings.Contains(tela, "vindo-do-comando") || !strings.Contains(tela, "X-Api-Key") {
		t.Errorf("a conferência não descreve o que seria criado; corpo = %q", tela)
	}
	for _, segredo := range []string{tokenNoComando, headerNaTela} {
		if strings.Contains(tela, segredo) {
			t.Errorf("a conferência revelou o segredo %q", segredo)
		}
	}

	// 2. Criar, pela pendência que a conferência devolveu.
	res := u.enviarForm(t, webui.RotaUpstreams+"/importar/aplicar", url.Values{
		"pendencia": {pendenciaDaTela(t, tela)},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("aplicar: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
	}
	id := idDoDestino(t, res)

	// A tela de detalhe diz "definido", nunca o valor — como no cadastro à mão.
	detalhe := corpo(t, u.abrir(t, webui.RotaUpstreams+"/"+strconv.FormatInt(id, 10)))
	for _, segredo := range []string{tokenNoComando, headerNaTela} {
		if strings.Contains(detalhe, segredo) {
			t.Errorf("tela de detalhe reexibiu o segredo %q", segredo)
		}
	}
	if !strings.Contains(detalhe, "X-Api-Key") {
		t.Error("tela de detalhe não lista o header que veio no comando")
	}

	// 3. E o upstream recebeu as duas credenciais na conexão.
	esperarUpstreamPronto(t, u, id)
	recebido := headersRecebidos()
	if recebido == nil {
		t.Fatal("nenhuma requisição chegou ao upstream")
	}
	if got, quer := recebido.Get("Authorization"), "Bearer "+tokenNoComando; got != quer {
		t.Errorf("Authorization = %q, quer %q", got, quer)
	}
	if got := recebido.Get("X-Api-Key"); got != headerNaTela {
		t.Errorf("X-Api-Key = %q, quer %q", got, headerNaTela)
	}
}

// pendenciaDaTela tira da conferência o identificador que os botões dela
// reenviam. Ele é o único pedaço do comando que volta ao navegador.
func pendenciaDaTela(t *testing.T, tela string) string {
	t.Helper()

	const marca = `name="pendencia" value="`
	i := strings.Index(tela, marca)
	if i < 0 {
		t.Fatalf("a conferência não traz o campo de pendência; corpo = %q", tela)
	}
	resto := tela[i+len(marca):]
	fim := strings.IndexByte(resto, '"')
	if fim <= 0 {
		t.Fatalf("campo de pendência malformado; corpo = %q", tela)
	}
	return resto[:fim]
}
