package configuracao

import (
	"slices"
	"strconv"
	"strings"
)

// As funções deste arquivo transformam "os resumos não batem" em "estes campos
// não batem".
//
// O resumo decide a mescla; a divergência é o que o dono lê para decidir um
// conflito. São dois trabalhos diferentes: o resumo precisa ser exato e opaco, e
// a lista de campos precisa ser legível — comparar campo a campo para decidir a
// mescla faria cada campo novo do schema virar uma linha a esquecer.

// divergenciasDeUpstream lista os campos em que o YAML e o banco discordam.
func divergenciasDeUpstream(noYAML, noBanco Upstream) []Divergencia {
	a, b := noYAML.normalizado(), noBanco.normalizado()
	campos := []struct {
		nome         string
		emA, emB     string
		saiSeIguais  bool
		soParaTipoDe string
	}{
		{nome: "tipo", emA: a.Tipo, emB: b.Tipo},
		{nome: "url", emA: a.URL, emB: b.URL, soParaTipoDe: TipoHTTP},
		{nome: "comando", emA: a.Comando, emB: b.Comando, soParaTipoDe: TipoSTDIO},
		{nome: "args", emA: listaEmTexto(a.Args), emB: listaEmTexto(b.Args), soParaTipoDe: TipoSTDIO},
		{nome: "env", emA: mapaEmTexto(a.Env), emB: mapaEmTexto(b.Env), soParaTipoDe: TipoSTDIO},
		{nome: "timeout_ms", emA: strconv.FormatInt(a.TimeoutMS, 10), emB: strconv.FormatInt(b.TimeoutMS, 10)},
		{nome: "habilitado", emA: strconv.FormatBool(a.Habilitado), emB: strconv.FormatBool(b.Habilitado)},
	}

	var out []Divergencia
	for _, c := range campos {
		// Campo do outro transporte não entra: um upstream http divergindo em
		// "comando" seria ruído em toda linha de conflito.
		if c.soParaTipoDe != "" && a.Tipo != c.soParaTipoDe && b.Tipo != c.soParaTipoDe {
			continue
		}
		if c.emA != c.emB {
			out = append(out, Divergencia{Campo: c.nome, NoYAML: c.emA, NoBanco: c.emB})
		}
	}
	return out
}

// divergenciasDeEndpoint lista os campos em que o YAML e o banco discordam. A
// composição entra como uma linha por upstream: o dono decide o conflito olhando
// o prefixo e as regras daquele vínculo, não um resumo do endpoint inteiro.
func divergenciasDeEndpoint(noYAML, noBanco Endpoint) []Divergencia {
	a, b := noYAML.normalizado(), noBanco.normalizado()

	var out []Divergencia
	for _, c := range []struct{ nome, emA, emB string }{
		{"nome", a.Nome, b.Nome},
		{"descricao", a.Descricao, b.Descricao},
		{"instrucoes", a.Instrucoes, b.Instrucoes},
	} {
		if c.emA != c.emB {
			out = append(out, Divergencia{Campo: c.nome, NoYAML: c.emA, NoBanco: c.emB})
		}
	}

	porNome := func(vs []Vinculo) map[string]Vinculo {
		m := make(map[string]Vinculo, len(vs))
		for _, v := range vs {
			m[v.Nome] = v
		}
		return m
	}
	emA, emB := porNome(a.Upstreams), porNome(b.Upstreams)
	nomes := make([]string, 0, len(emA)+len(emB))
	for nome := range emA {
		nomes = append(nomes, nome)
	}
	for nome := range emB {
		if _, ok := emA[nome]; !ok {
			nomes = append(nomes, nome)
		}
	}
	slices.Sort(nomes)

	for _, nome := range nomes {
		va, temA := emA[nome]
		vb, temB := emB[nome]
		switch {
		case temA && !temB:
			out = append(out, Divergencia{Campo: "upstream " + nome, NoYAML: vinculoEmTexto(va), NoBanco: ""})
		case !temA && temB:
			out = append(out, Divergencia{Campo: "upstream " + nome, NoYAML: "", NoBanco: vinculoEmTexto(vb)})
		default:
			if ta, tb := vinculoEmTexto(va), vinculoEmTexto(vb); ta != tb {
				out = append(out, Divergencia{Campo: "upstream " + nome, NoYAML: ta, NoBanco: tb})
			}
		}
	}
	return out
}

// vinculoEmTexto resume um vínculo numa linha: prefixo e regras, que é tudo o que
// a composição fina guarda.
func vinculoEmTexto(v Vinculo) string {
	partes := make([]string, 0, len(v.Regras)+1)
	partes = append(partes, "prefixo="+v.Prefixo)
	for _, r := range v.Regras {
		partes = append(partes, r.Linha())
	}
	return strings.Join(partes, "; ")
}

func listaEmTexto(v []string) string { return strings.Join(v, " ") }

// mapaEmTexto escreve o mapa em ordem de chave, para que a mesma configuração
// produza sempre o mesmo texto.
func mapaEmTexto(m map[string]string) string {
	chaves := make([]string, 0, len(m))
	for k := range m {
		chaves = append(chaves, k)
	}
	slices.Sort(chaves)
	partes := make([]string, 0, len(chaves))
	for _, k := range chaves {
		partes = append(partes, k+"="+m[k])
	}
	return strings.Join(partes, " ")
}
