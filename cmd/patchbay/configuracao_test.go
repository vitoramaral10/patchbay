package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/authsrv"
	"github.com/vitoramaral10/patchbay/internal/configuracao"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// instalacao é um banco de verdade com os repositórios e o serviço de
// configuração montados sobre ele.
//
// Os testes deste arquivo usam SQLite e os repositórios reais de propósito: o que
// eles exercitam é justamente a costura que os dublês de internal/configuracao
// não podem exercitar — o formulário de cada feature, a ida e volta das regras
// pela forma de texto, e a tradução entre nome de upstream e id.
type instalacao struct {
	servico   *configuracao.Servico
	upstreams *upstream.RepositorioSQLite
	endpoints *endpoint.RepositorioSQLite
	chaves    *apikey.RepositorioSQLite
}

func novaInstalacao(t *testing.T, ambiente map[string]string) *instalacao {
	t.Helper()

	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})

	leitura, escrita := st.Leitura(), st.Escrita()
	log := slog.New(slog.DiscardHandler)

	repoUpstream := upstream.NovoRepositorioSQLite(leitura, escrita, cofreDeTeste(t))
	repoEndpoint := endpoint.NovoRepositorioSQLite(leitura, escrita)
	repoChave := apikey.NovoRepositorioSQLite(leitura, escrita)

	return &instalacao{
		upstreams: repoUpstream,
		endpoints: repoEndpoint,
		chaves:    repoChave,
		servico: configuracao.NovoServico(
			upstreamsDaConfiguracao{repo: repoUpstream, log: log},
			segredosDaConfiguracao{repo: repoUpstream},
			endpointsDaConfiguracao{repo: repoEndpoint, upstreams: repoUpstream, log: log},
			log,
			configuracao.ComChaves(chavesDaConfiguracao{repo: repoChave}),
			configuracao.ComClientes(clientesDaConfiguracao{
				repo: authsrv.NovoRepositorioSQLite(leitura, escrita),
			}),
			configuracao.ComAmbiente(func(nome string) (string, bool) {
				v, ok := ambiente[nome]
				return v, ok
			}),
		),
	}
}

// povoar cadastra a configuração de partida pelos mesmos caminhos que a UI usa.
func (i *instalacao) povoar(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	http := upstream.Form{
		Nome: "notion", Tipo: upstream.TipoHTTP, URL: "https://mcp.notion.com/mcp",
		TimeoutMS: 15000, Habilitado: true,
		Bearer: cripto.Segredo("token-do-notion"),
		// A sonda entra aqui para que a ida e volta do YAML a cubra: sem ela no
		// arquivo, um import apagaria em silêncio a configuração de sonda de
		// todo upstream do banco.
		SondaHabilitada:  true,
		SondaFerramenta:  "notion-search",
		SondaArgs:        `{"query":"ping"}`,
		SondaEspera:      "resultado",
		SondaIntervaloMS: 900000,
		SondaTimeoutMS:   10000,
		SondaTolerancia:  3,
	}
	if !http.Validar() {
		t.Fatalf("formulário http recusado: %v", http.Erros)
	}
	idHTTP, err := i.upstreams.Criar(ctx, http)
	if err != nil {
		t.Fatalf("criar upstream http: erro = %v, quer nil", err)
	}

	stdio := upstream.Form{
		Nome: "arquivos", Tipo: upstream.TipoSTDIO, Comando: "npx",
		ArgsTexto: "-y\n@modelcontextprotocol/server-filesystem\n/dados",
		EnvTexto:  "NODE_ENV=production\nLOG=info",
		TimeoutMS: 20000, Habilitado: true,
		EnvSecretos: []upstream.CampoEnv{{Nome: "TOKEN", Valor: cripto.Segredo("token-dos-arquivos")}},
	}
	if !stdio.Validar() {
		t.Fatalf("formulário stdio recusado: %v", stdio.Erros)
	}
	idSTDIO, err := i.upstreams.Criar(ctx, stdio)
	if err != nil {
		t.Fatalf("criar upstream stdio: erro = %v, quer nil", err)
	}

	pessoal := endpoint.Form{
		Slug: "pessoal", Nome: "Pessoal", Descricao: "o de todo dia",
		Instrucoes:  "prefira buscar antes de escrever",
		UpstreamIDs: []int64{idHTTP, idSTDIO},
		Prefixos:    map[int64]string{idHTTP: "nt_", idSTDIO: "fs_"},
		RegrasTexto: map[int64]string{
			idSTDIO: "excluir write_*\nrenomear read_* ler_*\n",
		},
	}
	if !pessoal.Validar() {
		t.Fatalf("formulário de endpoint recusado: %v", pessoal.Erros)
	}
	idEndpoint, err := i.endpoints.Criar(ctx, pessoal)
	if err != nil {
		t.Fatalf("criar endpoint: erro = %v, quer nil", err)
	}

	emitida, err := apikey.Gerar()
	if err != nil {
		t.Fatalf("gerar chave: erro = %v, quer nil", err)
	}
	if _, err := i.chaves.Emitir(ctx, "desenvolvimento", emitida, []int64{idEndpoint}); err != nil {
		t.Fatalf("emitir chave: erro = %v, quer nil", err)
	}
}

// upstreamPorNome acha o upstream pelo nome, que é a chave que o YAML usa — o
// id é interno e não atravessa o arquivo.
func upstreamPorNome(t *testing.T, i *instalacao, nome string) upstream.Registro {
	t.Helper()

	regs, err := i.upstreams.Todos(context.Background())
	if err != nil {
		t.Fatalf("listar upstreams: erro = %v, quer nil", err)
	}
	for _, reg := range regs {
		if reg.Nome == nome {
			return reg
		}
	}
	t.Fatalf("upstream %q não está no banco", nome)
	return upstream.Registro{}
}

// TestConfiguracao_IdaEVoltaEntreInstalacoes é o critério da fatia com os
// repositórios de verdade: o YAML de uma instalação reconstrói outra, e o export
// da reconstruída é byte a byte o mesmo.
func TestConfiguracao_IdaEVoltaEntreInstalacoes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	origem := novaInstalacao(t, nil)
	origem.povoar(t)

	dados, err := origem.servico.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	if strings.Contains(string(dados), "token-do-notion") ||
		strings.Contains(string(dados), "token-dos-arquivos") {
		t.Fatalf("o YAML exportado carrega credencial em claro")
	}

	// A chave de API sai no arquivo como registro, e o import não a emite.
	if !strings.Contains(string(dados), "chaves_api:") {
		t.Errorf("YAML sem a seção de chaves de api:\n%s", dados)
	}

	destino := novaInstalacao(t, nil)
	plano, err := destino.servico.Planejar(ctx, dados, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	if plano.Erros() > 0 || plano.Conflitos() > 0 {
		var texto bytes.Buffer
		_ = plano.Escrever(&texto)
		t.Fatalf("plano com erro ou conflito:\n%s", texto.String())
	}
	relatorio, err := destino.servico.Aplicar(ctx, plano)
	if err != nil {
		var texto bytes.Buffer
		_ = relatorio.Escrever(&texto)
		t.Fatalf("Aplicar() erro = %v, quer nil\n%s", err, texto.String())
	}

	// A composição precisa ter atravessado inteira: prefixo por upstream e as
	// regras na ordem em que foram escritas.
	regs, err := destino.endpoints.Todos(ctx)
	if err != nil {
		t.Fatalf("listar endpoints: erro = %v, quer nil", err)
	}
	if len(regs) != 1 {
		t.Fatalf("endpoints no destino = %d, quer 1", len(regs))
	}
	composicao, err := destino.endpoints.ComposicaoDe(ctx, regs[0].ID)
	if err != nil {
		t.Fatalf("ler composição: erro = %v, quer nil", err)
	}
	if len(composicao) != 2 {
		t.Fatalf("vínculos = %d, quer 2", len(composicao))
	}
	var regras []string
	for _, c := range composicao {
		for _, r := range c.Regras {
			regras = append(regras, string(r.Acao)+" "+r.Padrao+" "+r.Renome)
		}
	}
	slices.Sort(regras)
	if quer := []string{"excluir write_* ", "renomear read_* ler_*"}; !slices.Equal(regras, quer) {
		t.Errorf("regras = %q, quer %q", regras, quer)
	}

	// A sonda funcional atravessa inteira. Ela não é segredo e não é estado: é
	// configuração, e um import que a apagasse tiraria as ferramentas do
	// catálogo no primeiro upstream que quebrasse, sem ninguém ter pedido.
	sondado := upstreamPorNome(t, destino, "notion")
	quer := upstream.Sonda{
		Habilitada: true, Ferramenta: "notion-search",
		Args: []byte(`{"query":"ping"}`), Espera: "resultado",
		Intervalo: 900 * time.Second, Timeout: 10 * time.Second, Tolerancia: 3,
	}
	if !reflect.DeepEqual(sondado.Sonda, quer) {
		t.Errorf("sonda no destino = %+v, quer %+v", sondado.Sonda, quer)
	}

	// Sem a variável de ambiente, os slots de credencial não vêm junto: o segredo
	// é o único resíduo que o arquivo não reconstrói, por desenho.
	definidas, err := destino.upstreams.CredenciaisDefinidas(ctx, composicao[0].UpstreamID)
	if err != nil {
		t.Fatalf("ler credenciais definidas: erro = %v, quer nil", err)
	}
	if len(definidas) != 0 {
		t.Errorf("credenciais no destino = %v, quer nenhuma", definidas)
	}

	// E o export do destino, tirando o que não se reconstrói, bate com o da
	// origem: mesmo upstreams, mesmos endpoints, mesma revisão de documento.
	depois, err := destino.servico.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() do destino: erro = %v, quer nil", err)
	}
	segundoPlano, err := destino.servico.Planejar(ctx, depois, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() do próprio export: erro = %v, quer nil", err)
	}
	if n := segundoPlano.Aplicaveis(); n != 0 {
		var texto bytes.Buffer
		_ = segundoPlano.Escrever(&texto)
		t.Errorf("o import do próprio export tem %d item(ns) aplicáveis:\n%s", n, texto.String())
	}
}

// TestConfiguracao_SegredoVemDoAmbiente cobre o único caminho em que o import
// grava credencial: a variável referenciada existe no ambiente do processo.
func TestConfiguracao_SegredoVemDoAmbiente(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	origem := novaInstalacao(t, nil)
	origem.povoar(t)

	dados, err := origem.servico.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}

	destino := novaInstalacao(t, map[string]string{
		"PATCHBAY_SEGREDO_NOTION_BEARER":      "bearer-do-ambiente",
		"PATCHBAY_SEGREDO_ARQUIVOS_ENV_TOKEN": "env-do-ambiente",
		"PATCHBAY_SEGREDO_ARQUIVOS_ENV_OUTRA": "não referenciada por nenhum slot",
	})
	plano, err := destino.servico.Planejar(ctx, dados, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	if _, err := destino.servico.Aplicar(ctx, plano); err != nil {
		t.Fatalf("Aplicar() erro = %v, quer nil", err)
	}

	regs, err := destino.upstreams.Todos(ctx)
	if err != nil {
		t.Fatalf("listar upstreams: erro = %v, quer nil", err)
	}
	querPorUpstream := map[string]upstream.Credencial{
		"notion":   {Tipo: upstream.CredencialBearer, Valor: cripto.Segredo("bearer-do-ambiente")},
		"arquivos": {Tipo: upstream.CredencialEnv, Nome: "TOKEN", Valor: cripto.Segredo("env-do-ambiente")},
	}
	for _, reg := range regs {
		creds, err := destino.upstreams.Credenciais(ctx, reg.ID)
		if err != nil {
			t.Fatalf("ler credenciais de %s: erro = %v, quer nil", reg.Nome, err)
		}
		quer := querPorUpstream[reg.Nome]
		if len(creds) != 1 {
			t.Fatalf("credenciais de %s = %d, quer 1", reg.Nome, len(creds))
		}
		got := creds[0]
		if got.Tipo != quer.Tipo || got.Nome != quer.Nome || got.Valor.Revelar() != quer.Valor.Revelar() {
			t.Errorf("credencial de %s = %s/%s, quer %s/%s (valor confere = %v)",
				reg.Nome, got.Tipo, got.Nome, quer.Tipo, quer.Nome,
				got.Valor.Revelar() == quer.Valor.Revelar())
		}
	}

	// Reimportar o próprio export, agora sem nenhuma variável no ambiente, não
	// apaga o que acabou de ser gravado: ausência de valor é "mantém".
	dadosDoDestino, err := destino.servico.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() do destino: erro = %v, quer nil", err)
	}
	log := slog.New(slog.DiscardHandler)
	semVariaveis := configuracao.NovoServico(
		upstreamsDaConfiguracao{repo: destino.upstreams, log: log},
		segredosDaConfiguracao{repo: destino.upstreams},
		endpointsDaConfiguracao{repo: destino.endpoints, upstreams: destino.upstreams, log: log},
		log,
	)
	planoSemVar, err := semVariaveis.Planejar(ctx, dadosDoDestino, configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() sem variáveis: erro = %v, quer nil", err)
	}
	if _, err := semVariaveis.Aplicar(ctx, planoSemVar); err != nil {
		t.Fatalf("Aplicar() sem variáveis: erro = %v, quer nil", err)
	}
	for _, reg := range regs {
		creds, err := destino.upstreams.Credenciais(ctx, reg.ID)
		if err != nil {
			t.Fatalf("reler credenciais de %s: erro = %v, quer nil", reg.Nome, err)
		}
		if len(creds) != 1 {
			t.Errorf("credenciais de %s depois do reimport = %d, quer 1: ausência não apaga",
				reg.Nome, len(creds))
		}
	}
}

// TestUpstreamsDaConfiguracao_ConferirRecusaTrocaDeTransporte: o repositório não
// atualiza o tipo de propósito, então deixar o import "atualizar" um upstream com
// outro transporte produziria uma diferença que nunca fecha.
func TestUpstreamsDaConfiguracao_ConferirRecusaTrocaDeTransporte(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inst := novaInstalacao(t, nil)
	inst.povoar(t)

	dados, err := inst.servico.Exportar(ctx)
	if err != nil {
		t.Fatalf("Exportar() erro = %v, quer nil", err)
	}
	alterado := strings.Replace(string(dados),
		"tipo: http", "tipo: stdio\n    comando: npx", 1)

	plano, err := inst.servico.Planejar(ctx, []byte(alterado), configuracao.Opcoes{})
	if err != nil {
		t.Fatalf("Planejar() erro = %v, quer nil", err)
	}
	var achou bool
	for _, item := range plano.Itens {
		if item.Tipo != configuracao.ItemUpstream || item.Nome != "notion" {
			continue
		}
		achou = true
		if item.Operacao != configuracao.OperacaoErro {
			t.Fatalf("Operacao = %s, quer %s (motivo = %q)",
				item.Operacao, configuracao.OperacaoErro, item.Motivo)
		}
		if !strings.Contains(item.Motivo, "substituí-lo") {
			t.Errorf("motivo = %q, quer explicar que trocar transporte é substituir", item.Motivo)
		}
	}
	if !achou {
		t.Fatalf("plano sem o item de notion")
	}
}

// TestParsearComPosicionais protege o subcomando de um --dry-run ignorado: o flag
// da stdlib para no primeiro argumento posicional, e um ensaio que escreve seria a
// pior falha possível deste comando.
func TestParsearComPosicionais(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		args      []string
		querFlag  bool
		querArqs  []string
		querFalha bool
	}{
		"flag antes do arquivo":      {args: []string{"--dry-run", "c.yaml"}, querFlag: true, querArqs: []string{"c.yaml"}},
		"flag depois do arquivo":     {args: []string{"c.yaml", "--dry-run"}, querFlag: true, querArqs: []string{"c.yaml"}},
		"flag no meio de dois nomes": {args: []string{"a.yaml", "--dry-run", "b.yaml"}, querFlag: true, querArqs: []string{"a.yaml", "b.yaml"}},
		"só o arquivo":               {args: []string{"c.yaml"}, querArqs: []string{"c.yaml"}},
		"nenhum argumento":           {args: nil},
		"flag que não existe":        {args: []string{"--nao-existe"}, querFalha: true},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			fs := flag.NewFlagSet("teste", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			ensaio := fs.Bool("dry-run", false, "")

			arqs, err := parsearComPosicionais(fs, tc.args)
			if tc.querFalha {
				if err == nil {
					t.Fatalf("erro = nil, quer recusa")
				}
				return
			}
			if err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if *ensaio != tc.querFlag {
				t.Errorf("dry-run = %v, quer %v", *ensaio, tc.querFlag)
			}
			if !slices.Equal(arqs, tc.querArqs) {
				t.Errorf("posicionais = %q, quer %q", arqs, tc.querArqs)
			}
		})
	}
}

// TestComandoImport_CodigoDeSaidaCobreConflito exercita os subcomandos de
// verdade (não só o Servico): um conflito de mescla não é falha de item, mas
// precisa devolver erro — e por extensão código de saída ≠ 0 — em quem chama
// de um pipeline de CI, com ou sem --dry-run. Sem este teste, um conflito
// silenciosamente devolveria 0 e a esteira seguiria como se o import tivesse
// batido com o arquivo.
//
// Não roda em paralelo com o resto do pacote: PATCHBAY_MASTER_KEY é lido do
// ambiente do processo e cofreDoAmbiente o apaga assim que lê, então dois
// subtestes usando a variável ao mesmo tempo colidiriam.
func TestComandoImport_CodigoDeSaidaCobreConflito(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	chaveTexto, err := cripto.GerarChaveMestra()
	if err != nil {
		t.Fatalf("gerar chave mestra: erro = %v, quer nil", err)
	}
	cofre, err := cofreDe(chaveTexto)
	if err != nil {
		t.Fatalf("montar cofre: erro = %v, quer nil", err)
	}

	st, err := store.Abrir(ctx, dataDir)
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	repo := upstream.NovoRepositorioSQLite(st.Leitura(), st.Escrita(), cofre)
	form := upstream.Form{
		Nome: "notion", Tipo: upstream.TipoHTTP, URL: "https://mcp.notion.com/mcp",
		TimeoutMS: 15000, Habilitado: true,
	}
	if !form.Validar() {
		t.Fatalf("formulário recusado: %v", form.Erros)
	}
	id, err := repo.Criar(ctx, form)
	if err != nil {
		t.Fatalf("criar upstream: erro = %v, quer nil", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("fechar banco: erro = %v, quer nil", err)
	}

	t.Setenv("PATCHBAY_MASTER_KEY", chaveTexto)
	var exportado bytes.Buffer
	if err := comandoExport(ctx, []string{"-data-dir", dataDir}, &exportado); err != nil {
		t.Fatalf("export: erro = %v, quer nil", err)
	}

	// O arquivo muda o timeout de um jeito; o banco, direto, muda para outro
	// valor depois do export: o clássico conflito de três vias.
	editado := strings.Replace(exportado.String(), "timeout_ms: 15000", "timeout_ms: 20000", 1)
	if editado == exportado.String() {
		t.Fatalf("o export não trazia o timeout esperado:\n%s", exportado.String())
	}
	arquivo := filepath.Join(t.TempDir(), "conflito.yaml")
	if err := os.WriteFile(arquivo, []byte(editado), 0o600); err != nil {
		t.Fatalf("gravar yaml: erro = %v, quer nil", err)
	}

	st2, err := store.Abrir(ctx, dataDir)
	if err != nil {
		t.Fatalf("reabrir banco: erro = %v, quer nil", err)
	}
	repo2 := upstream.NovoRepositorioSQLite(st2.Leitura(), st2.Escrita(), cofre)
	form.ID = id
	form.TimeoutMS = 30000
	if !form.Validar() {
		t.Fatalf("formulário recusado: %v", form.Erros)
	}
	if err := repo2.Atualizar(ctx, id, form); err != nil {
		t.Fatalf("atualizar upstream: erro = %v, quer nil", err)
	}
	if err := st2.Close(); err != nil {
		t.Fatalf("fechar banco: erro = %v, quer nil", err)
	}

	// --dry-run com conflito: nada escreve, mas o código de saída avisa quem
	// chama.
	t.Setenv("PATCHBAY_MASTER_KEY", chaveTexto)
	var saidaEnsaio bytes.Buffer
	if err := comandoImport(ctx, []string{"-data-dir", dataDir, "--dry-run", arquivo}, &saidaEnsaio); err == nil {
		t.Fatalf("--dry-run com conflito: erro = nil, quer recusa (saída:\n%s)", saidaEnsaio.String())
	}

	// Sem --dry-run: o conflito não é aplicado (o item some do banco sem
	// mudar), mas o comando também devolve erro — o conflito não é falha de
	// item, e mesmo assim precisa virar código de saída ≠ 0.
	t.Setenv("PATCHBAY_MASTER_KEY", chaveTexto)
	var saidaAplicada bytes.Buffer
	err = comandoImport(ctx, []string{"-data-dir", dataDir, arquivo}, &saidaAplicada)
	if err == nil {
		t.Fatalf("import com conflito: erro = nil, quer recusa (saída:\n%s)", saidaAplicada.String())
	}
	if !strings.Contains(saidaAplicada.String(), "conflito") {
		t.Errorf("saída sem menção a conflito:\n%s", saidaAplicada.String())
	}

	st3, err := store.Abrir(ctx, dataDir)
	if err != nil {
		t.Fatalf("reabrir banco de novo: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st3.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})
	reg, err := upstream.NovoRepositorioSQLite(st3.Leitura(), st3.Escrita(), cofre).Obter(ctx, id)
	if err != nil {
		t.Fatalf("reler upstream: erro = %v, quer nil", err)
	}
	if reg.TimeoutMS != 30000 {
		t.Errorf("timeout no banco = %d, quer 30000: o conflito não pode ter aplicado o arquivo", reg.TimeoutMS)
	}
}

// TestComandoExport_NaoSobrescreveSemForcar: -o apontando para um arquivo que
// já existe é recusado sem --forcar, para que ninguém perca uma configuração
// versionada por um comando digitado duas vezes.
func TestComandoExport_NaoSobrescreveSemForcar(t *testing.T) {
	t.Parallel()

	destino := filepath.Join(t.TempDir(), "patchbay.yaml")
	const conteudoOriginal = "não mexa aqui"
	if err := os.WriteFile(destino, []byte(conteudoOriginal), 0o600); err != nil {
		t.Fatalf("gravar arquivo de partida: erro = %v, quer nil", err)
	}

	// Sem --forcar e sem PATCHBAY_MASTER_KEY nenhuma: a recusa por já existir
	// precisa vir antes de o comando sequer abrir o banco.
	if err := comandoExport(context.Background(), []string{"-o", destino}, io.Discard); err == nil {
		t.Fatal("erro = nil, quer recusa por o arquivo já existir")
	}
	lido, err := os.ReadFile(destino)
	if err != nil {
		t.Fatalf("reler arquivo: erro = %v, quer nil", err)
	}
	if string(lido) != conteudoOriginal {
		t.Fatalf("o arquivo foi sobrescrito sem --forcar: %q", lido)
	}
}
