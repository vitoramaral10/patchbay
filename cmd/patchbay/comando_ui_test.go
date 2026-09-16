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
// instalação": a linha que a documentação do servidor publica entra pela caixa
// na tela de MCPs e sai como Authorization na conexão com o servidor MCP de
// verdade.
//
// É o mesmo par de afirmações do cadastro à mão, e é por isso que vale ter os
// dois: o token nunca reaparece em tela nenhuma, e chega inteiro ao upstream. O
// caminho de colar não pode ter atalho que perca uma das duas — o formulário só
// esconde o segredo porque o Criar o cifra, e é esse Criar que este caminho tem
// que estar usando.
func TestUI_ComandoColadoChegaAoUpstream(t *testing.T) {
	t.Parallel()

	alvo, headersRecebidos := upstreamComEspiao(t)
	u := subirUI(t)
	u.setup(t)

	comando := `claude mcp add --transport http --scope user vindo-do-comando ` +
		alvo + ` --header "Authorization: Bearer ` + tokenNoComando + `"` +
		` --header "X-Api-Key: ` + headerNaTela + `"`

	// Um POST só: colar cria.
	res := u.enviarForm(t, webui.RotaUpstreams+"/colar", url.Values{"comando": {comando}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("colar: status = %d, quer %d (corpo: %q)", res.StatusCode, http.StatusOK, corpo(t, res))
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

	// E o upstream recebeu as duas credenciais na conexão.
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
