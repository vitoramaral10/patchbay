package upstream_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// O upstream STDIO só se testa com processo de verdade, e o único binário que
// existe em toda máquina que roda `go test` é o próprio binário de teste. Então
// ele se reexecuta: TestMain olha a variável de papel antes de a suíte começar
// e, quando ela está definida, o processo vira a peça do cenário.
//
// Os três papéis cobrem os três caminhos da fatia: um servidor MCP que responde
// (pronto, e restart quando ele morre), um processo mudo que nunca responde ao
// initialize (hang), e um neto que sobrevive ao pai (morte de árvore).
//
// Nenhuma destas variáveis começa com PATCHBAY_ de propósito: elas viajam para
// o processo filho por cfg.Env, o mesmo caminho de uma variável de upstream de
// verdade, e abrirProcesso filtra qualquer prefixo PATCHBAY_ do ambiente do
// filho (item 1). Um nome de coordenação de teste com esse prefixo seria
// filtrado igual, o filho cairia no branch default do TestMain e reexecutaria
// a suíte inteira dentro de si mesmo — o que é exatamente o que acontecia
// antes deste comentário existir.
const (
	envPapel = "UPSTREAM_TESTE_PAPEL"
	// envNeto, quando definido, manda o processo lançar um neto e diz onde ele
	// publica o endereço do socket de vida.
	envNeto = "UPSTREAM_TESTE_NETO"
	// envNetoHerdaStderr, quando definido, manda o neto herdar o stderr do
	// pai em vez de ficar com o dele nulo — o cenário real de `npx` lançando
	// `node` sem redirecionar nada, que segura o pipe de stderr do processo
	// direto aberto mesmo depois de ele morrer.
	envNetoHerdaStderr = "UPSTREAM_TESTE_NETO_STDERR"
	// envPIDs, quando definido, é o arquivo em que cada processo anota o próprio
	// PID ao subir. É como o teste de restart vê que o processo é outro.
	envPIDs = "UPSTREAM_TESTE_PIDS"
	// envAmbiente, quando definido, é o arquivo em que o processo grava o
	// próprio ambiente ao subir — o teste do item 1 lê esse arquivo para
	// provar que PATCHBAY_MASTER_KEY não atravessou.
	envAmbiente = "UPSTREAM_TESTE_AMBIENTE"

	papelServidor = "servidor"
	papelMudo     = "mudo"
	papelNeto     = "neto"
	// papelTeimoso completa o initialize e serve normalmente, mas ignora o
	// EOF do stdin: só sai morto, pela árvore. É o processo que exercita o
	// caminho de graça de Encerrar (espera, Warn, coleta assíncrona) em vez
	// do caminho feliz de um processo que sai sozinho quando o cliente some.
	papelTeimoso = "teimoso"
	// papelAmbiente escreve o próprio ambiente num arquivo antes de servir.
	papelAmbiente = "ambiente"

	// ferramentaSuicida existe para o teste derrubar o processo de dentro, sem
	// depender de sinal nem de relógio: a chamada não volta, o processo morre, e
	// o supervisor tem que perceber e reiniciar.
	ferramentaSuicida = "patchbay_encerrar_processo"
	ferramentaEco     = "patchbay_eco"
)

func TestMain(m *testing.M) {
	switch os.Getenv(envPapel) {
	case papelServidor:
		os.Exit(rodarServidorMCP())
	case papelMudo:
		os.Exit(rodarMudo())
	case papelNeto:
		os.Exit(rodarNeto())
	case papelTeimoso:
		os.Exit(rodarTeimoso())
	case papelAmbiente:
		os.Exit(rodarAmbiente())
	default:
		os.Exit(m.Run())
	}
}

// rodarServidorMCP é um servidor MCP STDIO mínimo, feito com o próprio go-sdk.
//
// Com o SDK dos dois lados, e não com JSON-RPC escrito à mão: o que este teste
// precisa provar é o ciclo de vida do processo, e um handshake caseiro só
// acrescentaria uma segunda coisa que pode estar errada.
func rodarServidorMCP() int {
	if err := anotarPID(); err != nil {
		return 1
	}
	if err := lancarNeto(); err != nil {
		return 1
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "patchbay-fixture-stdio", Version: "0"}, nil)
	// O schema de entrada é obrigatório: sem ele o AddTool entra em panic, que é
	// o mesmo comportamento que o normalizador do catálogo existe para conter.
	vazio := &jsonschema.Schema{Type: "object"}
	srv.AddTool(&mcp.Tool{Name: ferramentaEco, Description: "devolve o que recebeu", InputSchema: vazio},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "eco"}}}, nil
		})
	srv.AddTool(&mcp.Tool{Name: ferramentaSuicida, Description: "mata o processo do servidor", InputSchema: vazio},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			os.Exit(0)
			return nil, nil
		})

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return 1
	}
	return 0
}

// rodarTeimoso serve normalmente — mesmas ferramentas de rodarServidorMCP —
// mas não sai quando o srv.Run volta por causa do EOF do stdin: ele ignora o
// retorno e fica de pé, só saindo morto pela árvore.
//
// É o que exercita o caminho de graça de Encerrar: a despedida ordenada (fechar
// o stdin, esperar esperaDeSaida) não é o que derruba este processo, e é
// exatamente por isso que o supervisor precisa do Warn e da coleta assíncrona
// para não travar no desligamento.
func rodarTeimoso() int {
	if err := anotarPID(); err != nil {
		return 1
	}
	if err := lancarNeto(); err != nil {
		return 1
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "patchbay-fixture-stdio-teimoso", Version: "0"}, nil)
	vazio := &jsonschema.Schema{Type: "object"}
	srv.AddTool(&mcp.Tool{Name: ferramentaEco, Description: "devolve o que recebeu", InputSchema: vazio},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "eco"}}}, nil
		})

	go func() { _ = srv.Run(context.Background(), &mcp.StdioTransport{}) }()
	// Um select vazio faria o detector de deadlock derrubar o processo, e o
	// teste passaria a medir um processo que sai sozinho — o oposto do que
	// este papel existe para provar. Com um timer pendente ele fica de pé até
	// alguém encerrá-lo.
	<-time.After(10 * time.Minute)
	return 0
}

// rodarAmbiente grava o próprio ambiente num arquivo antes de servir. É o
// teste do item 1: prova que PATCHBAY_MASTER_KEY (e qualquer outra variável
// interna do patchbay) não atravessa para o processo do upstream stdio.
func rodarAmbiente() int {
	if err := publicarAmbiente(); err != nil {
		return 1
	}
	return rodarServidorMCP()
}

func publicarAmbiente() error {
	caminho := os.Getenv(envAmbiente)
	if caminho == "" {
		return nil
	}
	tmp := caminho + ".parcial"
	if err := os.WriteFile(tmp, []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, caminho)
}

// rodarMudo sobe, lança o neto e nunca responde a nada. É o upstream pendurado.
func rodarMudo() int {
	if err := anotarPID(); err != nil {
		return 1
	}
	if err := lancarNeto(); err != nil {
		return 1
	}
	// Um select vazio faria o detector de deadlock derrubar o processo, e o
	// teste passaria a medir um processo que morre sozinho em vez de um que
	// pendura. Com um timer pendente ele fica de pé até alguém encerrá-lo.
	<-time.After(10 * time.Minute)
	return 0
}

// rodarNeto abre um socket de escuta e publica o endereço num arquivo.
//
// O socket é o sinal de vida do neto: enquanto o processo existe, uma conexão
// aberta contra ele fica de pé; quando ele morre, o sistema fecha o socket e
// quem lia recebe o fim. É a única prova de morte de processo que funciona igual
// nos dois sistemas — no Unix os.FindProcess sempre devolve sucesso
// (golang/go#34396) e no Windows não há equivalente de sinal 0.
func rodarNeto() int {
	ouvinte, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1
	}
	caminho := os.Getenv(envNeto)
	tmp := caminho + ".parcial"
	if err := os.WriteFile(tmp, []byte(ouvinte.Addr().String()), 0o600); err != nil {
		return 1
	}
	if err := os.Rename(tmp, caminho); err != nil {
		return 1
	}
	for {
		c, err := ouvinte.Accept()
		if err != nil {
			return 0
		}
		// Guardar a conexão é obrigatório: o net.Conn tem finalizador, e uma
		// conexão sem referência viva seria fechada pelo coletor de lixo — o
		// teste veria o socket cair e concluiria que o neto morreu.
		conexoesDoNeto = append(conexoesDoNeto, c)
	}
}

// conexoesDoNeto só existe no processo-neto, para segurar as conexões aceitas.
var conexoesDoNeto []net.Conn

// lancarNeto cria o processo neto e não o espera: quando o pai morre, o neto
// fica órfão. É o que `npx` faz com `node`, e é o processo que (*os.Process).Kill
// deixaria vivo para sempre.
func lancarNeto() error {
	caminho := os.Getenv(envNeto)
	if caminho == "" {
		return nil
	}
	neto := exec.Command(os.Args[0]) // #nosec G204 -- é o próprio binário de teste
	neto.Env = append(os.Environ(), envPapel+"="+papelNeto, envNeto+"="+caminho)
	// Stdin e stdout ficam nulos sempre: o neto não pode herdar o cano de MCP
	// do pai, senão o fim da sessão dependeria de ele fechar também.
	//
	// O stderr é a exceção controlada por envNetoHerdaStderr: herdar o do pai
	// é o cenário real de `npx` lançando `node` sem redirecionar nada, e é o
	// que mantém o pipe de stderr do processo direto aberto mesmo depois de
	// ele morrer — o caso que Encerrar() precisa não travar por causa dele.
	if os.Getenv(envNetoHerdaStderr) != "" {
		neto.Stderr = os.Stderr
	}
	return neto.Start()
}

func anotarPID() error {
	caminho := os.Getenv(envPIDs)
	if caminho == "" {
		return nil
	}
	f, err := os.OpenFile(caminho, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- caminho é do t.TempDir
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	return err
}

// TestGerente_STDIOMataAArvoreAoEncerrar é o critério da fatia no caminho do
// desligamento ordenado: quando o gerente encerra, o processo do upstream morre
// e o **neto** dele morre junto.
//
// É o único teste que reproduz o vazamento de PID do gateway anterior, e por
// isso ele sobe três processos de verdade em vez de dublê.
func TestGerente_STDIOMataAArvoreAoEncerrar(t *testing.T) {
	t.Parallel()

	cenario := novoCenarioSTDIO(t, papelServidor, comNeto)
	sut := cenario.subir(t, upstream.ComIntervaloTentativa(50*time.Millisecond))

	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	conexao := ligarNoNeto(t, cenario.enderecoDoNeto(t))

	cenario.encerrar(sut)

	esperarConexaoCair(t, conexao,
		"o neto sobreviveu ao encerramento do upstream: a árvore não foi morta")
}

// TestGerente_STDIOQueNaoRespondeAoInitializeViraDegradado é a detecção de hang.
//
// Um processo vivo e mudo não morre sozinho, então liveness de PID não o
// detecta: quem detecta é o timeout. E como aqui o supervisor é dono do outro
// lado do cano, o abandono da issue #1189 não deixa resíduo — a árvore é morta,
// inclusive o neto.
func TestGerente_STDIOQueNaoRespondeAoInitializeViraDegradado(t *testing.T) {
	t.Parallel()

	cenario := novoCenarioSTDIO(t, papelMudo, comNeto)
	rel := novoRelogioFalso()
	sut := cenario.subir(t,
		upstream.ComRelogio(rel),
		upstream.ComBackoff(upstream.BackoffFixo(time.Minute)),
	)

	conexao := ligarNoNeto(t, cenario.enderecoDoNeto(t))

	// O supervisor pedir backoff é o sinal de que ele tratou o mudo como falha.
	// Com o relógio falso a espera nunca vence, então só existe uma tentativa e
	// o cenário não fica reiniciando processo durante o teste.
	if espera := rel.esperarPedido(t); espera <= 0 {
		t.Fatalf("espera do backoff = %v, quer positiva", espera)
	}

	s, ok := sut.Situacao(1)
	if !ok {
		t.Fatal("upstream saiu da supervisão, quer supervisionado")
	}
	if s.Estado != upstream.EstadoDegradado {
		t.Errorf("estado = %q, quer %q", s.Estado, upstream.EstadoDegradado)
	}
	if s.UltimoErro == "" {
		t.Error("último erro vazio, quer o motivo para a UI mostrar")
	}
	if s.ProximaEm.IsZero() {
		t.Error("próxima tentativa zerada, quer o backoff agendado na tela")
	}
	if got := sut.Ferramentas(1); len(got) != 0 {
		t.Errorf("ferramentas = %d, quer 0: upstream mudo não publica catálogo", len(got))
	}

	esperarConexaoCair(t, conexao,
		"o neto do processo pendurado continuou vivo: o hang não recolheu a árvore")
}

// TestGerente_STDIOReiniciaQuandoOProcessoMorre prova o restart pela mesma
// máquina de estados da fatia 3: o processo morre sozinho, a sessão cai, o
// upstream vai a degradado e a próxima tentativa sobe um processo novo.
func TestGerente_STDIOReiniciaQuandoOProcessoMorre(t *testing.T) {
	t.Parallel()

	cenario := novoCenarioSTDIO(t, papelServidor)
	sut := cenario.subir(t, upstream.ComIntervaloTentativa(20*time.Millisecond))

	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	primeiro := esperarPIDs(t, cenario.arquivoPIDs, 1)

	// A ferramenta mata o processo de dentro: sem sinal, sem relógio, e sem
	// depender de o teste conseguir o PID antes de o processo mudar.
	if _, err := sut.Chamar(context.Background(), 1, ferramentaSuicida, nil); err == nil {
		t.Fatal("Chamar() = nil, quer o erro da chamada que matou o processo")
	}

	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	pids := esperarPIDs(t, cenario.arquivoPIDs, 2)

	if pids[0] != primeiro[0] {
		t.Fatalf("primeiro pid mudou de %q para %q", primeiro[0], pids[0])
	}
	if pids[1] == pids[0] {
		t.Fatalf("os dois pids são %q: o supervisor não subiu um processo novo", pids[0])
	}

	// E o processo novo serve de verdade, não só existe.
	res, err := sut.Chamar(context.Background(), 1, ferramentaEco, nil)
	if err != nil {
		t.Fatalf("Chamar() depois do restart = %v, quer nil", err)
	}
	if len(res.Content) == 0 {
		t.Error("conteúdo vazio, quer a resposta do processo reiniciado")
	}
}

// TestGerente_STDIOEncerrarPeloCaminhoDeGracaAindaMataAArvore é o item 4a: um
// servidor que completa o initialize e depois ignora o EOF do stdin (só sai
// morto) força o caminho de graça de Encerrar — a espera de esperaDeSaida, o
// Warn de "não confirmou saída", a coleta assíncrona — em vez do caminho feliz
// em que o processo já saiu quando o Wait é conferido. Mesmo por esse caminho,
// a árvore inteira tem que morrer.
func TestGerente_STDIOEncerrarPeloCaminhoDeGracaAindaMataAArvore(t *testing.T) {
	t.Parallel()

	cenario := novoCenarioSTDIO(t, papelTeimoso, comNeto)
	sut := cenario.subir(t, upstream.ComIntervaloTentativa(50*time.Millisecond))

	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	conexao := ligarNoNeto(t, cenario.enderecoDoNeto(t))

	inicio := time.Now()
	cenario.encerrar(sut)
	decorrido := time.Since(inicio)

	// O processo nunca sai sozinho: qualquer encerramento rápido demais seria
	// prova de que o teste não passou pelo caminho de graça, e sim por algum
	// atalho que não exercita o Warn nem a coleta assíncrona.
	if decorrido < time.Second {
		t.Errorf("Encerrar() voltou em %v; rápido demais para ter esperado o processo teimoso", decorrido)
	}

	esperarConexaoCair(t, conexao,
		"o neto do processo teimoso continuou vivo: o caminho de graça não recolheu a árvore")
}

// TestGerente_STDIOEncerrarComNetoQueHerdaStderrNaoPendura é o item 4b: o
// cenário real de `npx` lançando `node` sem redirecionar nada. O neto herda o
// stderr do processo direto e mantém o pipe aberto mesmo depois de o pai
// morrer — e é exatamente o que o WaitDelay de stdioproc existe para não
// deixar pendurar Encerrar() para sempre.
func TestGerente_STDIOEncerrarComNetoQueHerdaStderrNaoPendura(t *testing.T) {
	t.Parallel()

	cenario := novoCenarioSTDIO(t, papelTeimoso, comNeto, comNetoStderrHerdado)
	sut := cenario.subir(t, upstream.ComIntervaloTentativa(50*time.Millisecond))

	esperarEstado(t, sut, 1, upstream.EstadoPronto)
	conexao := ligarNoNeto(t, cenario.enderecoDoNeto(t))

	inicio := time.Now()
	cenario.encerrar(sut)
	decorrido := time.Since(inicio)

	// Generoso de propósito: esperaDeSaida (3s) mais EsperaDePipe (2s) mais
	// folga para máquina carregada. O que importa é que isto tem um teto —
	// sem o WaitDelay de stdioproc, o pipe herdado pelo neto penduraria o
	// Wait para sempre, e este teste passaria de rodar.
	const teto = 15 * time.Second
	if decorrido > teto {
		t.Errorf("Encerrar() = %v, quer no máximo %v: o pipe herdado pelo neto pendurou o desligamento",
			decorrido, teto)
	}

	esperarConexaoCair(t, conexao,
		"o neto que herda o stderr continuou vivo: Encerrar() não matou a árvore inteira")
}

// TestGerente_STDIONaoHerdaVariavelInternaDoPatchbay é o teste de ponta a
// ponta do item 1: PATCHBAY_MASTER_KEY, e qualquer outra variável PATCHBAY_*
// do processo do patchbay, não pode atravessar para o ambiente de um upstream
// stdio de terceiro — é o vazamento que anularia a cifra em repouso.
//
//nolint:paralleltest // t.Setenv muta o ambiente do processo; não convive com t.Parallel (mesma razão de cripto.ChaveMestraDe).
func TestGerente_STDIONaoHerdaVariavelInternaDoPatchbay(t *testing.T) {
	t.Setenv(cripto.VarChaveMestra, "nao-pode-vazar-para-o-upstream")
	t.Setenv("PATCHBAY_DATA_DIR", "nao-pode-vazar-tambem")

	cenario := novoCenarioSTDIO(t, papelAmbiente)
	sut := cenario.subir(t)

	esperarEstado(t, sut, 1, upstream.EstadoPronto)

	ambiente := esperarArquivo(t, cenario.arquivoAmbiente)
	if strings.Contains(ambiente, "nao-pode-vazar") {
		t.Fatal("o valor de uma variável interna do patchbay vazou para o ambiente do upstream stdio")
	}
	for _, proibida := range []string{cripto.VarChaveMestra + "=", "PATCHBAY_DATA_DIR="} {
		if strings.Contains(ambiente, proibida) {
			t.Errorf("o ambiente do processo do upstream contém %q, quer filtrado", proibida)
		}
	}
}

// --- cenário -----------------------------------------------------------------

type cenarioSTDIO struct {
	cfg             upstream.Config
	arquivoNeto     string
	arquivoPIDs     string
	arquivoAmbiente string
	cancelarBase    context.CancelFunc
}

type opcaoCenario func(*cenarioSTDIO, map[string]string)

// comNeto manda o processo lançar um neto que sobrevive a ele.
func comNeto(c *cenarioSTDIO, env map[string]string) {
	env[envNeto] = c.arquivoNeto
}

// comNetoStderrHerdado manda o neto herdar o stderr do pai em vez do dele
// próprio, nulo. Só faz sentido combinado com comNeto.
func comNetoStderrHerdado(_ *cenarioSTDIO, env map[string]string) {
	env[envNetoHerdaStderr] = "1"
}

func novoCenarioSTDIO(t *testing.T, papel string, opcoes ...opcaoCenario) *cenarioSTDIO {
	t.Helper()

	dir := t.TempDir()
	c := &cenarioSTDIO{
		arquivoNeto:     filepath.Join(dir, "endereco-do-neto"),
		arquivoPIDs:     filepath.Join(dir, "pids"),
		arquivoAmbiente: filepath.Join(dir, "ambiente-do-processo"),
	}
	env := map[string]string{
		envPapel:    papel,
		envPIDs:     c.arquivoPIDs,
		envAmbiente: c.arquivoAmbiente,
	}
	for _, o := range opcoes {
		o(c, env)
	}
	c.cfg = upstream.Config{
		ID: 1, Nome: "patchbay-fixture-stdio", Tipo: upstream.TipoSTDIO,
		Comando: os.Args[0],
		Env:     env,
		Timeout: 3 * time.Second,
	}
	return c
}

func (c *cenarioSTDIO) subir(t *testing.T, opcoes ...upstream.Opcao) *upstream.Gerente {
	t.Helper()

	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{c.cfg}, opcoes...)
	ctx, cancelar := context.WithCancel(context.Background())
	c.cancelarBase = cancelar
	sut.Iniciar(ctx)
	// O Cleanup também encerra: um teste que falha no meio não pode deixar
	// processo de fixture rodando na máquina de quem rodou a suíte.
	t.Cleanup(func() { c.encerrar(sut) })
	return sut
}

// encerrar desliga a supervisão e espera. Quando volta, a promessa da fatia é
// que não existe mais processo de upstream vivo.
func (c *cenarioSTDIO) encerrar(sut *upstream.Gerente) {
	c.cancelarBase()
	sut.Aguardar()
}

func (c *cenarioSTDIO) enderecoDoNeto(t *testing.T) string {
	t.Helper()
	return esperarArquivo(t, c.arquivoNeto)
}

// --- espera ------------------------------------------------------------------

// prazo é generoso de propósito: subir um processo, negociar MCP e listar
// ferramentas custa mais numa máquina carregada, e um teste que falha por isso
// mede a máquina em vez do código.
const prazo = 30 * time.Second

// intervaloDeSondagem é a granularidade de quem espera por artefato de outro
// processo. Não há canal para escutar do outro lado de um os.Exit.
const intervaloDeSondagem = 10 * time.Millisecond

func esperarArquivo(t *testing.T, caminho string) string {
	t.Helper()

	tique := time.NewTicker(intervaloDeSondagem)
	defer tique.Stop()
	limite := time.After(prazo)
	for {
		if b, err := os.ReadFile(caminho); err == nil && len(b) > 0 { // #nosec G304 -- caminho é do t.TempDir
			return strings.TrimSpace(string(b))
		}
		select {
		case <-tique.C:
		case <-limite:
			t.Fatalf("o processo de fixture não publicou %s em %v", caminho, prazo)
			return ""
		}
	}
}

// esperarPIDs espera o arquivo de PIDs ter pelo menos n linhas.
func esperarPIDs(t *testing.T, caminho string, n int) []string {
	t.Helper()

	tique := time.NewTicker(intervaloDeSondagem)
	defer tique.Stop()
	limite := time.After(prazo)
	var pids []string
	for {
		if b, err := os.ReadFile(caminho); err == nil { // #nosec G304 -- caminho é do t.TempDir
			pids = nil
			for _, linha := range strings.Split(string(b), "\n") {
				if linha = strings.TrimSpace(linha); linha != "" {
					pids = append(pids, linha)
				}
			}
			if len(pids) >= n {
				return pids
			}
		}
		select {
		case <-tique.C:
		case <-limite:
			t.Fatalf("processos anotados = %v depois de %v, quer pelo menos %d", pids, prazo, n)
			return nil
		}
	}
}

func ligarNoNeto(t *testing.T, endereco string) net.Conn {
	t.Helper()

	c, err := net.DialTimeout("tcp", endereco, prazo)
	if err != nil {
		t.Fatalf("ligar no neto em %s: %v", endereco, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// esperarConexaoCair bloqueia até o socket do neto morrer com ele. É a prova de
// morte de processo, e ela é um sinal de verdade: nada de dormir e torcer.
func esperarConexaoCair(t *testing.T, c net.Conn, mensagem string) {
	t.Helper()

	if err := c.SetReadDeadline(time.Now().Add(prazo)); err != nil {
		t.Fatalf("prazo de leitura: %v", err)
	}
	buf := make([]byte, 1)
	_, err := c.Read(buf)

	var expirou net.Error
	if errors.As(err, &expirou) && expirou.Timeout() {
		t.Fatalf("%s (a conexão continuou de pé por %v)", mensagem, prazo)
	}
}
