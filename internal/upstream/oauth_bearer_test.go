package upstream_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestAdmin_BearerGravadoRecusaEntradaNoModoOAuth cobre a correção da revisão:
// um upstream que já tem bearer gravado não pode entrar em modo oauth pela
// borda HTTP, mesmo sem o teste setar BearerDefinido à mão — é exatamente o
// caminho (admin_http.go: criar e atualizar chamando Validar antes de
// CompletarCredenciais) que deixava a validação achar BearerDefinido = false.
func TestAdmin_BearerGravadoRecusaEntradaNoModoOAuth(t *testing.T) {
	t.Parallel()

	as := novoASFalso(t)
	recurso := novoRecursoProtegido(t, as, false, "buscar")

	a := novoAmbiente(t, func(string) []upstream.Form {
		return []upstream.Form{{
			Nome: "com-bearer", Tipo: upstream.TipoHTTP, URL: recurso.URLMCP,
			TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
			Bearer: "sk-gravado-antes-do-oauth",
		}}
	}, opcoesAmbiente{})

	const id int64 = 1
	cliente := clienteSemSeguir()
	rota := a.admin.URL + webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10)

	resp, err := cliente.PostForm(rota, url.Values{
		"nome":       {"com-bearer"},
		"url":        {recurso.URLMCP},
		"timeout_ms": {strconv.FormatInt(upstream.TimeoutPadraoMS, 10)},
		"habilitado": {"1"},
		"modo":       {upstream.ModoOAuth},
		// Sem bearer_limpar: o admin só trocou o modo, sem limpar o bearer — é
		// o clique que a validação tem que recusar.
	})
	if err != nil {
		t.Fatalf("POST atualizar: erro = %v, quer nil", err)
	}
	defer func() { _ = resp.Body.Close() }()
	corpo, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ler corpo: erro = %v, quer nil", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer %d (corpo: %s)", resp.StatusCode, http.StatusUnprocessableEntity, corpo)
	}
	if !strings.Contains(string(corpo), "Limpe o bearer") {
		t.Errorf("corpo não contém o erro de modo esperado; corpo = %s", corpo)
	}

	// E nada mudou no banco: o modo continua estática e o bearer continua
	// gravado.
	reg, err := a.repo.Obter(context.Background(), id)
	if err != nil {
		t.Fatalf("obter: erro = %v, quer nil", err)
	}
	if reg.ModoEfetivo() != upstream.ModoEstatica {
		t.Errorf("modo = %q, quer %q (a atualização recusada não pode ter sido gravada)",
			reg.ModoEfetivo(), upstream.ModoEstatica)
	}
	definidas, err := a.repo.CredenciaisDefinidas(context.Background(), id)
	if err != nil {
		t.Fatalf("credenciais definidas: erro = %v, quer nil", err)
	}
	temBearer := false
	for _, d := range definidas {
		if d.Tipo == upstream.CredencialBearer {
			temBearer = true
		}
	}
	if !temBearer {
		t.Error("bearer não está mais gravado depois da atualização recusada")
	}
}
