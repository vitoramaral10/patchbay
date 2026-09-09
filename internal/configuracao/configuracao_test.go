package configuracao_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/configuracao"
)

func TestServico_ExportarImportarIdaEVolta(t *testing.T) {
	t.Parallel()

	casos := map[string]func() *bancoFake{
		"instalação povoada":                bancoPovoado,
		"instalação vazia":                  novoBancoFake,
		"upstream sem endpoint que o use":   semEndpoints,
		"endpoint sem composição nenhuma":   semComposicao,
		"upstream desabilitado com segredo": desabilitadoComSegredo,
	}

	for nome, montar := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			b := montar()
			sut := servicoDe(t, b, nil)
			ctx := context.Background()

			primeiro, err := sut.Exportar(ctx)
			if err != nil {
				t.Fatalf("Exportar() erro = %v, quer nil", err)
			}

			plano, err := sut.Planejar(ctx, primeiro, configuracao.Opcoes{})
			if err != nil {
				t.Fatalf("Planejar() erro = %v, quer nil", err)
			}
			if plano.BancoAvancou {
				t.Errorf("BancoAvancou = true, quer false: o export acabou de sair deste banco")
			}
			if n := plano.Aplicaveis(); n != 0 {
				t.Errorf("Aplicaveis() = %d, quer 0; itens = %s", n, itensEmTexto(plano))
			}
			if n := plano.Conflitos() + plano.Erros(); n != 0 {
				t.Errorf("conflitos+erros = %d, quer 0; itens = %s", n, itensEmTexto(plano))
			}

			relatorio, err := sut.Aplicar(ctx, plano)
			if err != nil {
				t.Fatalf("Aplicar() erro = %v, quer nil", err)
			}
			if n := relatorio.Aplicados(); n != 0 {
				t.Errorf("Aplicados() = %d, quer 0", n)
			}
			if len(b.escritas) != 0 {
				t.Errorf("escritas = %v, quer nenhuma", b.escritas)
			}

			segundo, err := sut.Exportar(ctx)
			if err != nil {
				t.Fatalf("Exportar() de novo: erro = %v, quer nil", err)
			}
			if !bytes.Equal(primeiro, segundo) {
				t.Errorf("o segundo export difere do primeiro\n--- primeiro ---\n%s\n--- segundo ---\n%s",
					primeiro, segundo)
			}
		})
	}
}

func semEndpoints() *bancoFake {
	b := bancoPovoado()
	b.endpoints = nil
	return b
}

func semComposicao() *bancoFake {
	b := bancoPovoado()
	for i := range b.endpoints {
		b.endpoints[i].Item.Upstreams = nil
	}
	return b
}

func desabilitadoComSegredo() *bancoFake {
	b := bancoPovoado()
	b.upstreams[0].Item.Habilitado = false
	return b
}

func TestServico_ExportarNaoLevaSegredo(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	sut := servicoDe(t, b, nil)

	dados, err := sut.Exportar(context.Background())
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	texto := string(dados)

	for _, valor := range b.valores {
		if strings.Contains(texto, valor) {
			t.Fatalf("o YAML exportado contém um valor de credencial")
		}
	}
	// O slot precisa aparecer: sem ele, quem importa noutra máquina não descobre
	// quais variáveis de ambiente precisa exportar.
	for _, quer := range []string{
		"${PATCHBAY_SEGREDO_NOTION_BEARER}",
		"${PATCHBAY_SEGREDO_ARQUIVOS_ENV_TOKEN}",
	} {
		if !strings.Contains(texto, quer) {
			t.Errorf("YAML sem a referência %s\n%s", quer, texto)
		}
	}
}

func TestServico_PlanejarRecusaDocumento(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		yaml   string
		querEr error
		texto  string
	}{
		"yaml que nem analisa": {
			yaml:   "isto: [não fecha\n",
			querEr: configuracao.ErrYAMLInvalido,
		},
		"versão desconhecida": {
			yaml:   "versao: 99\n",
			querEr: configuracao.ErrVersaoDesconhecida,
			texto:  "este patchbay lê 1",
		},
		"sem versão nenhuma": {
			yaml:   "upstreams: []\n",
			querEr: configuracao.ErrVersaoDesconhecida,
		},
		"campo que não existe no schema": {
			yaml:   "versao: 1\nupstreams:\n  - nome: a\n    timeout: 15\n",
			querEr: configuracao.ErrYAMLInvalido,
			texto:  "timeout",
		},
		"upstream repetido": {
			yaml: "versao: 1\nupstreams:\n" +
				"  - {nome: a, tipo: http, url: 'https://x/mcp', timeout_ms: 1000, habilitado: true}\n" +
				"  - {nome: a, tipo: http, url: 'https://y/mcp', timeout_ms: 1000, habilitado: true}\n",
			querEr: configuracao.ErrNomeRepetido,
			texto:  `"a"`,
		},
		"endpoint repetido": {
			yaml: "versao: 1\nendpoints:\n" +
				"  - {slug: p, nome: P}\n  - {slug: p, nome: P2}\n",
			querEr: configuracao.ErrNomeRepetido,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			b := bancoPovoado()
			sut := servicoDe(t, b, nil)

			_, err := sut.Planejar(context.Background(), []byte(tc.yaml), configuracao.Opcoes{})
			if !errors.Is(err, tc.querEr) {
				t.Fatalf("Planejar() erro = %v, quer %v", err, tc.querEr)
			}
			if tc.texto != "" && !strings.Contains(err.Error(), tc.texto) {
				t.Errorf("mensagem = %q, quer conter %q", err.Error(), tc.texto)
			}
			if len(b.escritas) != 0 {
				t.Errorf("escritas = %v, quer nenhuma: planejar não escreve", b.escritas)
			}
		})
	}
}

func TestServico_PlanejarMarcaItemInvalido(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		muda  func(*configuracao.Documento)
		tipo  string
		nome  string
		texto string
	}{
		"segredo em claro no arquivo": {
			muda: func(d *configuracao.Documento) {
				d.Upstreams[1].Segredos[0].Valor = "token-de-verdade"
			},
			tipo: configuracao.ItemUpstream, nome: "notion",
			texto: "segredo em claro",
		},
		"variável em env e em segredos ao mesmo tempo": {
			muda: func(d *configuracao.Documento) {
				if d.Upstreams[0].Env == nil {
					d.Upstreams[0].Env = map[string]string{}
				}
				d.Upstreams[0].Env["TOKEN"] = "duplicado-em-claro"
			},
			tipo: configuracao.ItemUpstream, nome: "arquivos",
			texto: "TOKEN",
		},
		"tipo de transporte não suportado": {
			muda:  func(d *configuracao.Documento) { d.Upstreams[1].Tipo = "sse" },
			tipo:  configuracao.ItemUpstream,
			nome:  "notion",
			texto: `tipo "sse"`,
		},
		"timeout zerado": {
			muda:  func(d *configuracao.Documento) { d.Upstreams[1].TimeoutMS = 0 },
			tipo:  configuracao.ItemUpstream,
			nome:  "notion",
			texto: "timeout_ms",
		},
		"composição cita upstream inexistente": {
			muda: func(d *configuracao.Documento) {
				d.Endpoints[1].Upstreams[0].Nome = "fantasma"
			},
			tipo: configuracao.ItemEndpoint, nome: "pessoal",
			texto: "fantasma",
		},
		"ação de regra desconhecida": {
			muda: func(d *configuracao.Documento) {
				d.Endpoints[1].Upstreams[0].Regras = []configuracao.Regra{{Acao: "apagar", Padrao: "x"}}
			},
			tipo: configuracao.ItemEndpoint, nome: "pessoal",
			texto: `ação "apagar"`,
		},
		"renomear sem nome novo": {
			muda: func(d *configuracao.Documento) {
				d.Endpoints[1].Upstreams[0].Regras = []configuracao.Regra{
					{Acao: configuracao.RegraRenomear, Padrao: "x"},
				}
			},
			tipo: configuracao.ItemEndpoint, nome: "pessoal",
			texto: "renomear exige renome",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			b := bancoPovoado()
			sut := servicoDe(t, b, nil)
			ctx := context.Background()

			dados, err := sut.Exportar(ctx)
			if err != nil {
				t.Fatalf("Exportar() erro = %v, quer nil", err)
			}
			doc := documentoDe(t, dados)
			tc.muda(&doc)

			plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
			if err != nil {
				t.Fatalf("Planejar() erro = %v, quer nil", err)
			}
			item := itemDoPlano(t, plano, tc.tipo, tc.nome)
			if item.Operacao != configuracao.OperacaoErro {
				t.Fatalf("Operacao = %s, quer %s (motivo = %q)",
					item.Operacao, configuracao.OperacaoErro, item.Motivo)
			}
			if !strings.Contains(item.Motivo, tc.texto) {
				t.Errorf("motivo = %q, quer conter %q", item.Motivo, tc.texto)
			}

			if _, err := sut.Aplicar(ctx, plano); err != nil {
				t.Fatalf("Aplicar() erro = %v, quer nil: item em erro não é falha de aplicação", err)
			}
			if len(b.escritas) != 0 {
				t.Errorf("escritas = %v, quer nenhuma", b.escritas)
			}
		})
	}
}

func TestServico_DryRunNaoEscreve(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	sut := servicoDe(t, b, map[string]string{"PATCHBAY_SEGREDO_NOTION_BEARER": "novo-token"})
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	doc := documentoDe(t, dados)
	doc.Upstreams[1].TimeoutMS = 30000
	doc.Upstreams = append(doc.Upstreams, configuracao.Upstream{
		Nome: "novo", Tipo: configuracao.TipoHTTP, URL: "https://novo/mcp",
		TimeoutMS: 5000, Habilitado: true,
	})

	plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	// Planejar já leu o arquivo, decidiu tudo e resolveu o segredo do ambiente —
	// e não pode ter escrito nada. É essa separação que faz o --dry-run valer
	// como ensaio do import de verdade, e não como um segundo caminho de código.
	if len(b.escritas) != 0 {
		t.Fatalf("escritas = %v, quer nenhuma", b.escritas)
	}
	if n := plano.Aplicaveis(); n != 3 {
		t.Errorf("Aplicaveis() = %d, quer 3 (atualizar notion, criar novo, definir bearer); itens = %s",
			n, itensEmTexto(plano))
	}

	var saida bytes.Buffer
	if err := plano.Escrever(&saida); err != nil {
		t.Fatalf("Escrever() erro = %v, quer nil", err)
	}
	for _, quer := range []string{"criar", "atualizar", "novo", "notion"} {
		if !strings.Contains(saida.String(), quer) {
			t.Errorf("plano impresso sem %q:\n%s", quer, saida.String())
		}
	}

	// O mesmo plano aplicado escreve — e é a diferença entre os dois que o
	// --dry-run promete.
	if _, err := sut.Aplicar(ctx, plano); err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}
	if len(b.escritas) != 3 {
		t.Errorf("escritas = %v, quer 3", b.escritas)
	}
}

func TestServico_MesclaComBancoAvancado(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	// Um terceiro upstream para o caso "só o banco mudou": ele precisa existir no
	// export para carregar uma revisão.
	b.upstreams = append(b.upstreams, configuracao.UpstreamNoBanco{ID: 3, Item: configuracao.Upstream{
		Nome: "calendario", Tipo: configuracao.TipoHTTP, URL: "https://cal/mcp",
		TimeoutMS: 9000, Habilitado: true,
	}})
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	doc := documentoDe(t, dados)

	// Lado do arquivo: notion muda só aqui, arquivos muda dos dois lados.
	for i := range doc.Upstreams {
		switch doc.Upstreams[i].Nome {
		case "notion":
			doc.Upstreams[i].TimeoutMS = 31000
		case "arquivos":
			doc.Upstreams[i].TimeoutMS = 32000
		}
	}
	// Lado do banco: arquivos e calendario mudam depois do export.
	for i := range b.upstreams {
		switch b.upstreams[i].Item.Nome {
		case "arquivos":
			b.upstreams[i].Item.TimeoutMS = 44000
		case "calendario":
			b.upstreams[i].Item.Habilitado = false
		}
	}

	plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	if !plano.BancoAvancou {
		t.Errorf("BancoAvancou = false, quer true")
	}

	querOperacao := map[string]configuracao.Operacao{
		"notion":     configuracao.OperacaoAtualizar,
		"arquivos":   configuracao.OperacaoConflito,
		"calendario": configuracao.OperacaoSemMudanca,
	}
	for nome, quer := range querOperacao {
		if got := itemDoPlano(t, plano, configuracao.ItemUpstream, nome).Operacao; got != quer {
			t.Errorf("operação de %s = %s, quer %s", nome, got, quer)
		}
	}

	conflito := itemDoPlano(t, plano, configuracao.ItemUpstream, "arquivos")
	if len(conflito.Divergencias) == 0 {
		t.Fatalf("conflito sem divergências: o dono não teria como decidir")
	}
	d := conflito.Divergencias[0]
	if d.Campo != "timeout_ms" || d.NoYAML != "32000" || d.NoBanco != "44000" {
		t.Errorf("divergência = %+v, quer timeout_ms 32000/44000", d)
	}

	relatorio, err := sut.Aplicar(ctx, plano)
	if err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}
	if n := relatorio.Aplicados(); n != 1 {
		t.Fatalf("Aplicados() = %d, quer 1; escritas = %v", n, b.escritas)
	}
	// O conflito não impediu o item que não conflitou de entrar, e não sobrescreveu
	// o que o banco mudou sozinho.
	if got := upstreamDoBanco(t, b, "notion").TimeoutMS; got != 31000 {
		t.Errorf("timeout de notion = %d, quer 31000", got)
	}
	if got := upstreamDoBanco(t, b, "arquivos").TimeoutMS; got != 44000 {
		t.Errorf("timeout de arquivos = %d, quer 44000: o conflito não pode ter aplicado o arquivo", got)
	}
	if upstreamDoBanco(t, b, "calendario").Habilitado {
		t.Errorf("calendario voltou a habilitado: o arquivo não mudou esse item")
	}
}

// TestServico_ConflitoSoNaSonda prova que a sonda entra em
// divergenciasDeUpstream: um conflito causado só por ela precisa listar o
// campo "sonda", e não sair de mãos vazias por o dono não ter onde ler o
// motivo do conflito.
func TestServico_ConflitoSoNaSonda(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	doc := documentoDe(t, dados)

	// Os dois lados ligam a sonda de "notion" depois do export, com
	// ferramentas diferentes: nenhum outro campo muda.
	for i := range doc.Upstreams {
		if doc.Upstreams[i].Nome == "notion" {
			doc.Upstreams[i].Sonda = &configuracao.SondaDoUpstream{
				Habilitada: true, Ferramenta: "buscar",
				IntervaloMS: 900_000, TimeoutMS: 15_000, Tolerancia: 2,
			}
		}
	}
	for i := range b.upstreams {
		if b.upstreams[i].Item.Nome == "notion" {
			b.upstreams[i].Item.Sonda = &configuracao.SondaDoUpstream{
				Habilitada: true, Ferramenta: "pesquisar_paginas",
				IntervaloMS: 900_000, TimeoutMS: 15_000, Tolerancia: 2,
			}
		}
	}

	plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}

	item := itemDoPlano(t, plano, configuracao.ItemUpstream, "notion")
	if item.Operacao != configuracao.OperacaoConflito {
		t.Fatalf("operação de notion = %s, quer %s", item.Operacao, configuracao.OperacaoConflito)
	}

	achou := false
	for _, d := range item.Divergencias {
		if d.Campo == "sonda" {
			achou = true
			if !strings.Contains(d.NoYAML, "buscar") || !strings.Contains(d.NoBanco, "pesquisar_paginas") {
				t.Errorf("divergência de sonda = %+v, quer as duas ferramentas", d)
			}
		}
	}
	if !achou {
		t.Errorf("divergências de notion = %+v, quer o campo \"sonda\" entre elas", item.Divergencias)
	}
}

func TestServico_RemoverAusentesSoComFlag(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		remover     bool
		querUp      configuracao.Operacao
		querEnd     configuracao.Operacao
		querEscrita int
	}{
		"sem a flag, ausente é só reportado": {
			remover: false,
			querUp:  configuracao.OperacaoAusente,
			querEnd: configuracao.OperacaoAusente,
		},
		"com a flag, ausente é removido": {
			remover:     true,
			querUp:      configuracao.OperacaoRemover,
			querEnd:     configuracao.OperacaoRemover,
			querEscrita: 2,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			b := bancoPovoado()
			sut := servicoDe(t, b, nil)
			ctx := context.Background()

			dados, err := sut.Exportar(ctx)
			if err != nil {
				t.Fatalf("Exportar() erro = %v, quer nil", err)
			}
			doc := documentoDe(t, dados)
			// O arquivo perde o upstream "arquivos" e o endpoint "leitura". O
			// endpoint "pessoal" também perde o vínculo, senão ele passaria a
			// citar um upstream que este import removeria.
			doc.Upstreams = semUpstream(doc.Upstreams, "arquivos")
			doc.Endpoints = semEndpoint(doc.Endpoints, "leitura")
			for i := range doc.Endpoints {
				doc.Endpoints[i].Upstreams = semVinculo(doc.Endpoints[i].Upstreams, "arquivos")
			}

			plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{RemoverAusentes: tc.remover})
			if err != nil {
				t.Fatalf("Planejar() erro = %v, quer nil", err)
			}
			if got := itemDoPlano(t, plano, configuracao.ItemUpstream, "arquivos").Operacao; got != tc.querUp {
				t.Errorf("operação do upstream ausente = %s, quer %s", got, tc.querUp)
			}
			if got := itemDoPlano(t, plano, configuracao.ItemEndpoint, "leitura").Operacao; got != tc.querEnd {
				t.Errorf("operação do endpoint ausente = %s, quer %s", got, tc.querEnd)
			}

			if _, err := sut.Aplicar(ctx, plano); err != nil {
				t.Fatalf("Aplicar() erro = %v, quer nil", err)
			}
			// O endpoint "pessoal" também é atualizado quando o vínculo sai, e
			// isso conta como escrita nos dois casos: a comparação é do que
			// sobrou do que a flag decide.
			removidas := 0
			for _, e := range b.escritas {
				if strings.HasPrefix(e, "remover ") {
					removidas++
				}
			}
			if removidas != tc.querEscrita {
				t.Errorf("remoções = %d, quer %d; escritas = %v", removidas, tc.querEscrita, b.escritas)
			}
		})
	}
}

// TestServico_RemoverUpstreamAindaCitadoNoArquivoNaoAplica prova que remover um
// upstream não cascateia por baixo de um endpoint que continua no arquivo
// citando-o: a remoção vira erro, e nem o upstream nem o vínculo saem do banco.
//
// Sem esta recusa, o upstream sairia mesmo assim (o item dele não depende do
// item do endpoint), e o ON DELETE CASCADE apagaria o vínculo do endpoint
// "pessoal" por baixo — um efeito colateral que o relatório nunca menciona.
func TestServico_RemoverUpstreamAindaCitadoNoArquivoNaoAplica(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	doc := documentoDe(t, dados)
	// O arquivo perde o upstream "arquivos" dos upstreams cadastrados, mas o
	// endpoint "pessoal" continua citando-o na composição — um arquivo editado
	// pela metade, exatamente o caso que a remoção precisa recusar.
	doc.Upstreams = semUpstream(doc.Upstreams, "arquivos")

	plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{RemoverAusentes: true})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	item := itemDoPlano(t, plano, configuracao.ItemUpstream, "arquivos")
	if item.Operacao != configuracao.OperacaoErro {
		t.Fatalf("Operacao = %s, quer %s (motivo = %q)", item.Operacao, configuracao.OperacaoErro, item.Motivo)
	}
	if !strings.Contains(item.Motivo, "pessoal") {
		t.Errorf("motivo = %q, quer nomear o endpoint pessoal", item.Motivo)
	}

	if _, err := sut.Aplicar(ctx, plano); err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}
	// upstreamDoBanco já falha o teste se o upstream tiver saído do banco.
	upstreamDoBanco(t, b, "arquivos")
	pessoal := endpointDoBanco(t, b, "pessoal")
	var aindaTemArquivos bool
	for _, v := range pessoal.Upstreams {
		if v.Nome == "arquivos" {
			aindaTemArquivos = true
		}
	}
	if !aindaTemArquivos {
		t.Errorf("o vínculo de pessoal com arquivos sumiu: a remoção recusada cascateou mesmo assim")
	}
}

func semUpstream(us []configuracao.Upstream, nome string) []configuracao.Upstream {
	out := us[:0:0]
	for _, u := range us {
		if u.Nome != nome {
			out = append(out, u)
		}
	}
	return out
}

func semEndpoint(es []configuracao.Endpoint, slug string) []configuracao.Endpoint {
	out := es[:0:0]
	for _, e := range es {
		if e.Slug != slug {
			out = append(out, e)
		}
	}
	return out
}

func semVinculo(vs []configuracao.Vinculo, nome string) []configuracao.Vinculo {
	out := vs[:0:0]
	for _, v := range vs {
		if v.Nome != nome {
			out = append(out, v)
		}
	}
	return out
}

func TestServico_Segredos(t *testing.T) {
	t.Parallel()

	const slotBearer = "1/bearer/"

	casos := map[string]struct {
		ambiente  map[string]string
		muda      func(*configuracao.Documento)
		querOp    configuracao.Operacao
		querValor string
		querSumiu bool
	}{
		"slot exportado sem a variável definida mantém o gravado": {
			querOp:    configuracao.OperacaoSemMudanca,
			querValor: "token-do-notion",
		},
		"slot ausente do arquivo não apaga o gravado": {
			muda:      func(d *configuracao.Documento) { d.Upstreams[1].Segredos = nil },
			querValor: "token-do-notion",
		},
		"variável definida grava o valor novo": {
			ambiente:  map[string]string{"PATCHBAY_SEGREDO_NOTION_BEARER": "token-novo"},
			querOp:    configuracao.OperacaoAtualizar,
			querValor: "token-novo",
		},
		"limpar apaga o gravado": {
			muda:      func(d *configuracao.Documento) { d.Upstreams[1].Segredos[0].Limpar = true },
			querOp:    configuracao.OperacaoRemover,
			querSumiu: true,
		},
		"referência a outra variável lê de onde o arquivo mandou": {
			ambiente: map[string]string{"MEU_TOKEN": "token-de-outra-var"},
			muda: func(d *configuracao.Documento) {
				d.Upstreams[1].Segredos[0].Valor = "${MEU_TOKEN}"
			},
			querOp:    configuracao.OperacaoAtualizar,
			querValor: "token-de-outra-var",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			b := bancoPovoado()
			sut := servicoDe(t, b, tc.ambiente)
			ctx := context.Background()

			dados, err := sut.Exportar(ctx)
			if err != nil {
				t.Fatalf("Exportar() erro = %v, quer nil", err)
			}
			doc := documentoDe(t, dados)
			if tc.muda != nil {
				tc.muda(&doc)
			}

			plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
			if err != nil {
				t.Fatalf("Planejar() erro = %v, quer nil", err)
			}
			if tc.querOp != "" {
				item := itemDoPlano(t, plano, configuracao.ItemSegredo, "notion · bearer")
				if item.Operacao != tc.querOp {
					t.Errorf("operação do segredo = %s, quer %s (motivo = %q)",
						item.Operacao, tc.querOp, item.Motivo)
				}
			}

			if _, err := sut.Aplicar(ctx, plano); err != nil {
				t.Fatalf("Aplicar() erro = %v, quer nil", err)
			}
			valor, existe := b.valores[slotBearer]
			switch {
			case tc.querSumiu && existe:
				t.Errorf("o bearer continua gravado com %q, quer apagado", valor)
			case !tc.querSumiu && valor != tc.querValor:
				t.Errorf("bearer gravado = %q, quer %q", valor, tc.querValor)
			}
		})
	}
}

func TestServico_AplicarNaoAbortaNoPrimeiroErro(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	b.erroAtualizarEnd = errFake
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	doc := documentoDe(t, dados)
	// Um endpoint que falha ao gravar e dois upstreams que gravam bem: o
	// relatório precisa dizer os três, e os dois bons precisam ter entrado.
	for i := range doc.Endpoints {
		doc.Endpoints[i].Descricao = "mudou"
	}
	for i := range doc.Upstreams {
		doc.Upstreams[i].TimeoutMS = 25000
	}

	plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}

	relatorio, err := sut.Aplicar(ctx, plano)
	if !errors.Is(err, configuracao.ErrAplicacaoParcial) {
		t.Fatalf("Aplicar() erro = %v, quer %v", err, configuracao.ErrAplicacaoParcial)
	}
	if relatorio.Aplicados() != 2 || relatorio.Falhas() != 2 {
		t.Errorf("aplicados/falhas = %d/%d, quer 2/2; escritas = %v",
			relatorio.Aplicados(), relatorio.Falhas(), b.escritas)
	}
	for _, nome := range []string{"notion", "arquivos"} {
		if got := upstreamDoBanco(t, b, nome).TimeoutMS; got != 25000 {
			t.Errorf("timeout de %s = %d, quer 25000: item bom não pode ser desfeito por item ruim",
				nome, got)
		}
	}

	var saida bytes.Buffer
	if err := relatorio.Escrever(&saida); err != nil {
		t.Fatalf("Escrever() erro = %v, quer nil", err)
	}
	if !strings.Contains(saida.String(), "falhou") {
		t.Errorf("relatório sem a linha de falha:\n%s", saida.String())
	}
}

func TestServico_ImportaEmInstalacaoVazia(t *testing.T) {
	t.Parallel()

	origem := bancoPovoado()
	sut := servicoDe(t, origem, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}

	// Outra instalação, banco vazio: é o caso de reconstruir a instância a partir
	// do arquivo, que é a razão de a fatia existir.
	destino := novoBancoFake()
	outro := servicoDe(t, destino, nil)

	plano, err := outro.Planejar(ctx, dados, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	if _, err := outro.Aplicar(ctx, plano); err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}

	// Um endpoint criado precisa do upstream que ele compõe já criado: é a ordem
	// de fase do plano que garante isso, e é ela que este caso protege.
	if len(destino.upstreams) != 2 || len(destino.endpoints) != 2 {
		t.Fatalf("destino = %d upstreams e %d endpoints, quer 2 e 2",
			len(destino.upstreams), len(destino.endpoints))
	}
	pessoal := endpointDoBanco(t, destino, "pessoal")
	if len(pessoal.Upstreams) != 2 {
		t.Fatalf("composição de %s = %d vínculos, quer 2", pessoal.Slug, len(pessoal.Upstreams))
	}
	if pessoal.Upstreams[0].Nome != "arquivos" || pessoal.Upstreams[0].Prefixo != "fs_" {
		t.Errorf("primeiro vínculo = %+v, quer arquivos com prefixo fs_", pessoal.Upstreams[0])
	}
	if n := len(pessoal.Upstreams[0].Regras); n != 2 {
		t.Errorf("regras do vínculo = %d, quer 2", n)
	}

	// E o segundo import do mesmo arquivo não muda mais nada.
	segundo, err := outro.Planejar(ctx, dados, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() de novo: erro = %v, quer nil", err)
	}
	if n := segundo.Aplicaveis(); n != 0 {
		t.Errorf("Aplicaveis() no segundo import = %d, quer 0; itens = %s", n, itensEmTexto(segundo))
	}
}

func TestServico_ItensInformativosNaoSaoAplicados(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}

	destino := novoBancoFake()
	outro := servicoDe(t, destino, nil)
	plano, err := outro.Planejar(ctx, dados, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}

	for _, tipo := range []string{configuracao.ItemChaveAPI, configuracao.ItemClienteOAuth} {
		nome := "desenvolvimento"
		if tipo == configuracao.ItemClienteOAuth {
			nome = "Claude Desktop"
		}
		item := itemDoPlano(t, plano, tipo, nome)
		if item.Operacao != configuracao.OperacaoInformativo {
			t.Errorf("operação de %s = %s, quer %s", tipo, item.Operacao, configuracao.OperacaoInformativo)
		}
		if item.Aplicavel() {
			t.Errorf("%s marcado como aplicável", tipo)
		}
	}

	if _, err := outro.Aplicar(ctx, plano); err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}
	if len(destino.chaves) != 0 || len(destino.clientes) != 0 {
		t.Errorf("o import criou chave ou cliente: %d/%d", len(destino.chaves), len(destino.clientes))
	}
}

func TestLer_AceitaArquivoEscritoAMao(t *testing.T) {
	t.Parallel()

	// Sem nenhuma linha de revisao: é o arquivo que alguém escreve do zero, e ele
	// precisa valer como intenção — a trava otimista não pode virar um pedágio
	// para quem nunca exportou.
	const escrito = `versao: 1
upstreams:
  - nome: notion
    tipo: http
    url: https://mcp.notion.com/mcp
    timeout_ms: 60000
    habilitado: true
`

	b := bancoPovoado()
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	plano, err := sut.Planejar(ctx, []byte(escrito), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	if plano.BancoAvancou {
		t.Errorf("BancoAvancou = true, quer false: arquivo sem revisão não dispara a trava")
	}
	item := itemDoPlano(t, plano, configuracao.ItemUpstream, "notion")
	if item.Operacao != configuracao.OperacaoAtualizar {
		t.Fatalf("Operacao = %s, quer %s", item.Operacao, configuracao.OperacaoAtualizar)
	}
	if !strings.Contains(item.Motivo, "sem revisão") {
		t.Errorf("motivo = %q, quer explicar a ausência de revisão", item.Motivo)
	}

	// E o que ficou de fora do arquivo continua no banco: um arquivo parcial não
	// apaga nada sem --remover-ausentes.
	if _, err := sut.Aplicar(ctx, plano); err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}
	if len(b.upstreams) != 2 || len(b.endpoints) != 2 {
		t.Errorf("banco = %d upstreams e %d endpoints, quer 2 e 2",
			len(b.upstreams), len(b.endpoints))
	}
	if got := upstreamDoBanco(t, b, "notion").TimeoutMS; got != 60000 {
		t.Errorf("timeout de notion = %d, quer 60000", got)
	}
}

func TestServico_ConferirBloqueiaAntesDeEscrever(t *testing.T) {
	t.Parallel()

	b := bancoPovoado()
	b.erroConferirUpstream = errors.New("url: URL inválida")
	sut := servicoDe(t, b, nil)
	ctx := context.Background()

	dados, err := sut.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	doc := documentoDe(t, dados)
	doc.Upstreams[1].URL = "isto-não-é-url"

	plano, err := sut.Planejar(ctx, yamlDe(t, doc), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	item := itemDoPlano(t, plano, configuracao.ItemUpstream, "notion")
	if item.Operacao != configuracao.OperacaoErro {
		t.Fatalf("Operacao = %s, quer %s", item.Operacao, configuracao.OperacaoErro)
	}
	if !strings.Contains(item.Motivo, "URL inválida") {
		t.Errorf("motivo = %q, quer a mensagem da própria feature", item.Motivo)
	}
	if len(b.escritas) != 0 {
		t.Errorf("escritas = %v, quer nenhuma", b.escritas)
	}
}
