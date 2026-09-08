package main

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// composicao é o que a tela de endpoint grava para um upstream: se ele entra,
// com que prefixo e com que regras.
type composicao struct {
	upstreamID int64
	prefixo    string
	regras     string
}

func camposDoEndpoint(slug, nome string, itens ...composicao) url.Values {
	campos := url.Values{"slug": {slug}, "nome": {nome}}
	for _, item := range itens {
		id := strconv.FormatInt(item.upstreamID, 10)
		campos.Add("upstream", id)
		campos.Set(endpoint.ChavePrefixo(item.upstreamID), item.prefixo)
		campos.Set(endpoint.ChaveRegras(item.upstreamID), item.regras)
	}
	return campos
}

func (u uiDeTeste) criarEndpointComposto(t *testing.T, slug, nome string, itens ...composicao) int64 {
	t.Helper()

	res := u.enviarForm(t, webui.RotaEndpoints, camposDoEndpoint(slug, nome, itens...))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("criar endpoint %s: status = %d, quer %d (corpo: %q)",
			slug, res.StatusCode, http.StatusOK, corpo(t, res))
	}
	return idDoDestino(t, res)
}

func (u uiDeTeste) editarComposicao(t *testing.T, id int64, slug, nome string, itens ...composicao) {
	t.Helper()

	campos := camposDoEndpoint(slug, nome, itens...)
	campos.Del("slug") // o slug é contrato e não entra no UPDATE.
	res := u.enviarForm(t, webui.RotaEndpoints+"/"+strconv.FormatInt(id, 10), campos)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("editar endpoint %d: status = %d, quer %d (corpo: %q)",
			id, res.StatusCode, http.StatusOK, corpo(t, res))
	}
}

// TestUI_ComposicaoFina é a demonstração da fatia 4: um upstream, dois endpoints
// com composições diferentes, e a edição da composição valendo na hora, sem
// reiniciar o processo e sem derrubar a sessão do cliente MCP.
func TestUI_ComposicaoFina(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamFalso(t)
	upstreamID := u.criarUpstream(t, "falso", alvo)

	// pessoal leva tudo com prefixo; trabalho leva só somar, renomeado.
	pessoalID := u.criarEndpointComposto(t, "pessoal", "Pessoal", composicao{
		upstreamID: upstreamID, prefixo: "nt_",
	})
	u.criarEndpointComposto(t, "trabalho", "Trabalho", composicao{
		upstreamID: upstreamID,
		prefixo:    "wk.",
		regras:     "renomear somar soma\nincluir somar\nexcluir *\n",
	})

	chavePessoal := u.criarChave(t, "notebook", pessoalID)

	esperarFerramentas(t, u, "pessoal", 2)
	esperarFerramentas(t, u, "trabalho", 1)

	sessao := conectarMCP(t, u, "pessoal", chavePessoal)
	quer := []string{"nt_" + nomeNormalizado, "nt_somar"}
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, quer) {
		t.Fatalf("ferramentas de pessoal = %v, quer %v", nomes, quer)
	}
	if nomes := u.app.endpoints.Expostos("trabalho"); !slices.Equal(nomes, []string{"wk.soma"}) {
		t.Errorf("ferramentas de trabalho = %v, quer [wk.soma]; o mesmo upstream compõe os dois", nomes)
	}

	// Editar a composição de pessoal vale na hora, na mesma sessão MCP: o filtro
	// tira uma ferramenta e o prefixo muda, sem nenhum reinício.
	servidorAntes := u.app.endpoints.Servidor("pessoal")
	u.editarComposicao(t, pessoalID, "pessoal", "Pessoal", composicao{
		upstreamID: upstreamID,
		prefixo:    "p.",
		regras:     "excluir eco*\n",
	})
	esperarFerramentas(t, u, "pessoal", 1)

	if nomes := u.app.endpoints.Expostos("pessoal"); !slices.Equal(nomes, []string{"p.somar"}) {
		t.Errorf("expostos depois da edição = %v, quer [p.somar]", nomes)
	}
	// O que a sessão vê é o catálogo novo mais as lápides do antigo: trocar o
	// prefixo troca o nome exposto, que é contrato, e a janela de graça da fatia
	// 3 existe justamente para o cliente que ainda não relistou não receber
	// unknown tool.
	querNaSessao := []string{"nt_" + nomeNormalizado, "nt_somar", "p.somar"}
	if nomes := nomesDeFerramenta(t, sessao); !slices.Equal(nomes, querNaSessao) {
		t.Errorf("ferramentas depois da edição = %v, quer %v", nomes, querNaSessao)
	}
	querLapides := []string{"nt_" + nomeNormalizado, "nt_somar"}
	if nomes := u.app.endpoints.Lapides("pessoal"); !slices.Equal(nomes, querLapides) {
		t.Errorf("lápides = %v, quer %v", nomes, querLapides)
	}
	if u.app.endpoints.Servidor("pessoal") != servidorAntes {
		t.Error("mudar a composição recriou o *mcp.Server; a sessão do cliente teria caído")
	}
	// trabalho não foi tocado: composição é por endpoint.
	if nomes := u.app.endpoints.Expostos("trabalho"); !slices.Equal(nomes, []string{"wk.soma"}) {
		t.Errorf("ferramentas de trabalho = %v, quer [wk.soma]; editar pessoal mexeu no outro endpoint", nomes)
	}

	// A composição gravada volta na tela de edição, para poder ser editada de
	// novo sem ser reescrita do zero.
	tela := corpo(t, u.abrir(t, webui.RotaEndpoints+"/"+strconv.FormatInt(pessoalID, 10)+"/editar"))
	if !strings.Contains(tela, `value="p."`) {
		t.Error("a tela de edição não reexibe o prefixo gravado")
	}
	if !strings.Contains(tela, "excluir eco*") {
		t.Error("a tela de edição não reexibe as regras gravadas")
	}
}

// TestUI_DesmarcarERemarcarUpstreamNaoTrazRegraAntiga: as regras não são filhas
// de endpoint_upstream, então não saem por cascata quando a composição é
// limpa — é por isso que repositorio_sqlite.go:~221-227 tem um DELETE FROM
// endpoint_tool_rule explícito. Desmarcar o upstream e remarcá-lo sem escrever
// regra nenhuma não pode trazer de volta o filtro antigo.
func TestUI_DesmarcarERemarcarUpstreamNaoTrazRegraAntiga(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamFalso(t)
	upstreamID := u.criarUpstream(t, "falso", alvo)

	id := u.criarEndpointComposto(t, "pessoal", "Pessoal", composicao{
		upstreamID: upstreamID,
		regras:     "excluir eco*\n",
	})
	esperarFerramentas(t, u, "pessoal", 1)

	// Desmarca o upstream: nenhum campo "upstream" no POST, a composição fica
	// vazia.
	res := u.enviarForm(t, webui.RotaEndpoints+"/"+strconv.FormatInt(id, 10),
		url.Values{"nome": {"Pessoal"}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("desmarcar upstream: status = %d, quer %d (corpo: %q)",
			res.StatusCode, http.StatusOK, corpo(t, res))
	}
	esperarFerramentas(t, u, "pessoal", 0)

	// Remarca sem escrever regra nenhuma: se o DELETE explícito de
	// endpoint_tool_rule não rodasse, o filtro "excluir eco*" voltaria sozinho
	// e o endpoint ficaria com 1 ferramenta em vez das 2 do upstream inteiro.
	u.editarComposicao(t, id, "pessoal", "Pessoal", composicao{upstreamID: upstreamID})
	esperarFerramentas(t, u, "pessoal", 2)
}

// TestUI_RegraInvalidaNaoGrava: a linha que não dá para interpretar volta como
// erro de campo, com o número da linha, e a composição no ar não muda.
func TestUI_RegraInvalidaNaoGrava(t *testing.T) {
	t.Parallel()

	u := subirUI(t)
	u.setup(t)

	alvo := upstreamFalso(t)
	upstreamID := u.criarUpstream(t, "falso", alvo)
	id := u.criarEndpointComposto(t, "pessoal", "Pessoal", composicao{upstreamID: upstreamID})
	esperarFerramentas(t, u, "pessoal", 2)

	res := u.enviarForm(t, webui.RotaEndpoints+"/"+strconv.FormatInt(id, 10),
		camposDoEndpoint("", "Pessoal", composicao{
			upstreamID: upstreamID,
			regras:     "excluir somar\napagar tudo\n",
		}))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, quer %d", res.StatusCode, http.StatusUnprocessableEntity)
	}
	if texto := corpo(t, res); !strings.Contains(texto, "Linha 2") {
		t.Errorf("a tela não disse qual linha está errada; corpo: %q", texto)
	}
	// A regra válida da primeira linha também não vale: ou grava tudo, ou nada.
	if n := u.app.endpoints.Contagem("pessoal"); n != 2 {
		t.Errorf("contagem = %d, quer 2; um formulário recusado mudou a composição no ar", n)
	}
}
