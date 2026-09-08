package stdioproc_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/stdioproc"
)

// O teste do neto sobrevivente precisa de três processos, e o único binário que
// existe em toda máquina que roda `go test` é o próprio binário de teste. Então
// ele se reexecuta: TestMain olha a variável de papel antes de qualquer teste
// rodar e, quando ela está definida, o processo vira a peça do cenário em vez de
// rodar a suíte.
const (
	envPapel  = "PATCHBAY_STDIOPROC_PAPEL"
	envArtefa = "PATCHBAY_STDIOPROC_PORTA"

	papelPai    = "pai"
	papelNeto   = "neto"
	papelStderr = "stderr"
)

func TestMain(m *testing.M) {
	switch os.Getenv(envPapel) {
	case papelPai:
		os.Exit(rodarPai())
	case papelNeto:
		os.Exit(rodarNeto())
	case papelStderr:
		os.Exit(rodarStderr())
	default:
		os.Exit(m.Run())
	}
}

// rodarStderr escreve duas linhas conhecidas no stderr e sai. É o suficiente
// para provar que Especificacao.Erros está ligado ao stderr de verdade do
// processo filho — os casos de borda do coletor (linha dividida, \r\n, corte
// em 4096, teto de linhas) são testados direto na unidade em escritor_test.go,
// onde o teste decide exatamente como os bytes chegam ao Write, em vez de
// torcer para o SO entregar o cano do jeito que o cenário precisa.
func rodarStderr() int {
	_, _ = os.Stderr.WriteString("primeira linha\n")
	_, _ = os.Stderr.WriteString("segunda linha\n")
	return 0
}

// rodarPai lança o neto e depois não faz mais nada.
//
// Não sair sozinho é o ponto: quem tem que encerrá-lo é o supervisor. E o neto
// é lançado sem Wait, para ficar órfão quando o pai morrer — que é exatamente o
// que acontece com `npx` lançando `node` e morrendo no meio.
func rodarPai() int {
	neto := exec.Command(os.Args[0]) // #nosec G204 -- é o próprio binário de teste
	neto.Env = append(os.Environ(),
		envPapel+"="+papelNeto,
		envArtefa+"="+os.Getenv(envArtefa),
	)
	// Stdin, stdout e stderr ficam nulos: o neto não pode herdar cano nenhum do
	// pai, senão o Wait do pai esperaria por ele e o teste mediria outra coisa.
	if err := neto.Start(); err != nil {
		return 1
	}
	// Um select vazio faria o detector de deadlock do Go derrubar o processo na
	// hora, e o teste passaria a medir um pai que morre sozinho. Com um timer
	// pendente o processo fica de pé até alguém encerrá-lo — que é o cenário.
	<-time.After(10 * time.Minute)
	return 0
}

// rodarNeto abre um socket de escuta e publica a porta num arquivo.
//
// O socket é o sinal de vida: enquanto o processo existe, uma conexão aberta
// contra ele fica de pé; quando ele morre, o sistema operacional fecha o socket
// e quem estava lendo recebe o fim. É a única prova de morte de processo que
// funciona igual no Windows e no Unix — no Unix os.FindProcess sempre devolve
// sucesso (golang/go#34396) e no Windows não existe equivalente de sinal 0.
func rodarNeto() int {
	ouvinte, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1
	}
	if err := publicarPorta(os.Getenv(envArtefa), ouvinte.Addr().String()); err != nil {
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

// publicarPorta grava e renomeia, para o teste nunca ler um arquivo pela metade.
func publicarPorta(caminho, endereco string) error {
	if caminho == "" {
		return errors.New("sem caminho de artefato")
	}
	tmp := caminho + ".parcial"
	if err := os.WriteFile(tmp, []byte(endereco), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, caminho)
}

// TestArvore_EncerrarMataONetoSobrevivente é o teste do vazamento de PID: um
// processo neto que sobreviveu ao pai tem que morrer junto com a árvore.
//
// É o único caso que reproduz o modo de falha do gateway anterior, e é por isso
// que ele sobe processo de verdade em vez de dublê.
func TestArvore_EncerrarMataONetoSobrevivente(t *testing.T) {
	t.Parallel()

	_, arvore, endereco, esperar := subirArvoreComNeto(t)
	conexao := ligarNoNeto(t, endereco)

	if err := arvore.Encerrar(); err != nil {
		t.Fatalf("Encerrar() = %v, quer nil", err)
	}

	esperarConexaoCair(t, conexao, "o neto continuou vivo depois de a árvore ser encerrada")
	esperarSaida(t, esperar, "o pai continuou vivo depois de a árvore ser encerrada")
}

// TestArvore_MatarSoOFilhoDeixaONetoVivo é o caso de controle.
//
// Sem ele o teste acima passaria mesmo que o neto estivesse morrendo por outro
// motivo — um cano fechado, o processo de teste terminando — e a árvore não
// estaria provando nada. Aqui (*os.Process).Kill mata só o filho direto, e o
// neto tem que continuar de pé: é literalmente o bug.
func TestArvore_MatarSoOFilhoDeixaONetoVivo(t *testing.T) {
	t.Parallel()

	cmd, arvore, endereco, esperar := subirArvoreComNeto(t)
	conexao := ligarNoNeto(t, endereco)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill() = %v, quer nil", err)
	}
	esperarSaida(t, esperar, "o filho direto não morreu com Kill")

	// Meio segundo é folga: se o neto fosse morrer junto, morreria no mesmo
	// instante em que o pai — não meio segundo depois.
	if err := lerAteCair(conexao, 500*time.Millisecond); err == nil {
		t.Fatal("o neto morreu junto com o pai; o caso de controle deixou de controlar coisa nenhuma")
	}

	if err := arvore.Encerrar(); err != nil {
		t.Fatalf("Encerrar() = %v, quer nil", err)
	}
	esperarConexaoCair(t, conexao, "o neto sobreviveu à árvore")
}

// subirArvoreComNeto devolve, além do cmd e da árvore, uma função de espera
// memoizada: (*exec.Cmd).Wait não pode ser chamado duas vezes, e tanto o
// corpo do teste (via esperarSaida) quanto o t.Cleanup precisam esperar a
// mesma saída sem violar essa regra.
func subirArvoreComNeto(t *testing.T) (cmd *exec.Cmd, arvore *stdioproc.Arvore, endereco string, esperar func() error) {
	t.Helper()

	artefato := filepath.Join(t.TempDir(), "porta-do-neto")
	cmd, arvore, err := stdioproc.Iniciar(stdioproc.Especificacao{
		Comando: os.Args[0],
		Ambiente: append(os.Environ(),
			envPapel+"="+papelPai,
			envArtefa+"="+artefato,
		),
	})
	if err != nil {
		t.Fatalf("Iniciar() = %v, quer nil", err)
	}

	var umaEspera sync.Once
	var erroEspera error
	esperar = func() error {
		umaEspera.Do(func() { erroEspera = cmd.Wait() })
		return erroEspera
	}

	t.Cleanup(func() {
		_ = arvore.Encerrar()
		_ = esperar()
	})

	return cmd, arvore, esperarArquivo(t, artefato), esperar
}

// esperarArquivo espera o artefato que o neto publica.
func esperarArquivo(t *testing.T, caminho string) string {
	t.Helper()

	tique := time.NewTicker(10 * time.Millisecond)
	defer tique.Stop()
	limite := time.After(20 * time.Second)
	for {
		if b, err := os.ReadFile(caminho); err == nil && len(b) > 0 { // #nosec G304 -- caminho é do t.TempDir
			return strings.TrimSpace(string(b))
		}
		select {
		case <-tique.C:
		case <-limite:
			t.Fatalf("o neto não publicou %s em 20s", caminho)
			return ""
		}
	}
}

func ligarNoNeto(t *testing.T, endereco string) net.Conn {
	t.Helper()

	c, err := net.DialTimeout("tcp", endereco, 10*time.Second)
	if err != nil {
		t.Fatalf("ligar no neto em %s: %v", endereco, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// esperarConexaoCair bloqueia até o socket do neto morrer com ele.
func esperarConexaoCair(t *testing.T, c net.Conn, mensagem string) {
	t.Helper()
	if err := lerAteCair(c, 20*time.Second); err != nil {
		t.Fatalf("%s: %v", mensagem, err)
	}
}

// lerAteCair devolve nil quando a leitura termina — o que só acontece quando o
// processo do outro lado deixa de existir.
func lerAteCair(c net.Conn, prazo time.Duration) error {
	if err := c.SetReadDeadline(time.Now().Add(prazo)); err != nil {
		return err
	}
	buf := make([]byte, 1)
	_, err := c.Read(buf)
	var expirou net.Error
	if errors.As(err, &expirou) && expirou.Timeout() {
		return errors.New("a conexão com o neto continuou de pé")
	}
	return nil
}

func esperarSaida(t *testing.T, esperar func() error, mensagem string) {
	t.Helper()

	saiu := make(chan struct{})
	go func() {
		_ = esperar()
		close(saiu)
	}()
	select {
	case <-saiu:
	case <-time.After(20 * time.Second):
		t.Fatal(mensagem)
	}
}

func TestPreparar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		esp     stdioproc.Especificacao
		querErr error
		querNil bool
	}{
		"comando existente": {
			esp: stdioproc.Especificacao{Comando: os.Args[0]},
		},
		"comando vazio": {
			esp:     stdioproc.Especificacao{Comando: "   "},
			querErr: stdioproc.ErrComandoVazio,
			querNil: true,
		},
		"comando que não existe no PATH": {
			esp:     stdioproc.Especificacao{Comando: "patchbay-comando-que-nao-existe"},
			querErr: exec.ErrNotFound,
			querNil: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			cmd, err := stdioproc.Preparar(tc.esp)
			if tc.querErr != nil {
				if !errors.Is(err, tc.querErr) {
					t.Fatalf("erro = %v, quer %v", err, tc.querErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if cmd.SysProcAttr == nil {
				t.Error("SysProcAttr = nil, quer a árvore configurada antes do Start")
			}
			if cmd.WaitDelay != stdioproc.EsperaDePipe {
				t.Errorf("WaitDelay = %v, quer %v", cmd.WaitDelay, stdioproc.EsperaDePipe)
			}
		})
	}
}

// TestPreparar_NaoHerdarFiltraPrefixo é o teste do item 1: uma variável
// PATCHBAY_* presente em Ambiente não pode atravessar para o processo filho,
// porque é exatamente onde a chave mestra vazaria para um upstream stdio de
// terceiro.
func TestPreparar_NaoHerdarFiltraPrefixo(t *testing.T) {
	t.Parallel()

	cmd, err := stdioproc.Preparar(stdioproc.Especificacao{
		Comando: os.Args[0],
		Ambiente: append(os.Environ(),
			"PATCHBAY_MASTER_KEY=segredo-que-nao-pode-vazar",
			"PATCHBAY_DATA_DIR=/dados",
			"patchbay_minuscula=tambem-nao-pode-vazar",
			"OUTRA_VARIAVEL=fica",
		),
		NaoHerdar: []string{"PATCHBAY_"},
	})
	if err != nil {
		t.Fatalf("Preparar() = %v, quer nil", err)
	}

	for _, par := range cmd.Env {
		nome, _, _ := strings.Cut(par, "=")
		if strings.HasPrefix(nomeCanonico(nome), nomeCanonico("PATCHBAY_")) {
			t.Errorf("variável com prefixo filtrado atravessou: %s", par)
		}
	}
	if _, ok := valorDe(cmd.Env, "OUTRA_VARIAVEL"); !ok {
		t.Error("OUTRA_VARIAVEL sumiu: o filtro não pode remover o que não tem o prefixo")
	}
}

// TestEspecificacao_ErrosRecebeSTDERRDoFilho prova a ligação de ponta a ponta
// entre o stderr real do processo filho e o callback Erros — os casos de
// borda do coletor (divisão de linha, \r\n, corte, teto) são testados direto
// na unidade em escritor_test.go.
func TestEspecificacao_ErrosRecebeSTDERRDoFilho(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var linhas []string
	cmd, arvore, err := stdioproc.Iniciar(stdioproc.Especificacao{
		Comando:  os.Args[0],
		Ambiente: append(os.Environ(), envPapel+"="+papelStderr),
		Erros: func(linha string) {
			mu.Lock()
			defer mu.Unlock()
			linhas = append(linhas, linha)
		},
	})
	if err != nil {
		t.Fatalf("Iniciar() = %v, quer nil", err)
	}
	t.Cleanup(func() { _ = arvore.Encerrar() })

	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() = %v, quer nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	quer := []string{"primeira linha", "segunda linha"}
	if !reflect.DeepEqual(linhas, quer) {
		t.Errorf("linhas recebidas = %q, quer %q", linhas, quer)
	}
}

// TestAmbienteHerdado prova as três propriedades do bloco de ambiente do filho:
// o que o patchbay tem continua lá, o que foi configurado entra, e o que foi
// configurado por cima de um herdado não vira duas entradas para o mesmo nome.
//
// Sem t.Setenv de propósito: o que ele quer provar é herança do ambiente real do
// processo, e mexer nesse ambiente proibiria o teste de rodar em paralelo.
func TestAmbienteHerdado(t *testing.T) {
	t.Parallel()

	// Uma variável que existe com certeza, seja qual for a máquina — exceto,
	// em teoria, num processo com ambiente vazio de propósito. Aí não há
	// nada para provar herança, e o teste sai em vez de indexar um slice
	// vazio.
	ambiente := os.Environ()
	if len(ambiente) == 0 {
		t.Skip("processo sem nenhuma variável de ambiente; nada para provar herança")
	}
	herdada, _, _ := strings.Cut(ambiente[0], "=")

	casos := map[string]struct {
		extras map[string]string
	}{
		"sem extras":                 {extras: nil},
		"extras novos":               {extras: map[string]string{"PATCHBAY_TESTE_NOVA": "1", "PATCHBAY_TESTE_OUTRA": ""}},
		"extra sobre um já herdado":  {extras: map[string]string{herdada: "sobrescrito"}},
		"extra com caixa trocada":    {extras: map[string]string{strings.ToLower(herdada): "sobrescrito"}},
		"extra com igual no valor":   {extras: map[string]string{"PATCHBAY_TESTE_URL": "a=b"}},
		"extra com espaço no valor":  {extras: map[string]string{"PATCHBAY_TESTE_DIR": "C:\\Arquivos de Programas"}},
		"extra com acento no valor":  {extras: map[string]string{"PATCHBAY_TESTE_ACENTO": "ação"}},
		"muitos extras não duplicam": {extras: map[string]string{"A_PATCHBAY": "1", "B_PATCHBAY": "2", "C_PATCHBAY": "3"}},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			base := os.Environ()
			got := stdioproc.AmbienteHerdado(tc.extras)

			// 1. Nenhum nome aparece duas vezes. É a propriedade que importa:
			// dois valores para o mesmo nome é comportamento indefinido, e no
			// Windows a comparação ignora a caixa.
			vistos := map[string]int{}
			for _, linha := range got {
				nome, _, ok := strings.Cut(linha, "=")
				if !ok {
					t.Errorf("entrada sem =: %q", linha)
					continue
				}
				vistos[nomeCanonico(nome)]++
			}
			for nome, vezes := range vistos {
				if vezes > 1 {
					t.Errorf("%s aparece %d vezes, quer 1", nome, vezes)
				}
			}

			// 2. Cada extra está lá com o valor pedido.
			for chave, valor := range tc.extras {
				if achado, ok := valorDe(got, chave); !ok || achado != valor {
					t.Errorf("%s = %q (presente=%v), quer %q", chave, achado, ok, valor)
				}
			}

			// 3. O que o patchbay tinha e não foi sobrescrito continua lá.
			for _, par := range base {
				nome, valor, _ := strings.Cut(par, "=")
				if _, sobrescrito := extraDe(tc.extras, nome); sobrescrito {
					continue
				}
				if achado, ok := valorDe(got, nome); !ok || achado != valor {
					t.Errorf("herdada %s = %q (presente=%v), quer %q", nome, achado, ok, valor)
				}
			}
		})
	}
}

// nomeCanonico aplica a mesma regra de comparação que o sistema operacional usa
// para nome de variável de ambiente.
func nomeCanonico(nome string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(nome)
	}
	return nome
}

func extraDe(extras map[string]string, nome string) (string, bool) {
	for chave, valor := range extras {
		if nomeCanonico(chave) == nomeCanonico(nome) {
			return valor, true
		}
	}
	return "", false
}

func valorDe(ambiente []string, chave string) (string, bool) {
	for _, linha := range ambiente {
		if nome, valor, ok := strings.Cut(linha, "="); ok && nomeCanonico(nome) == nomeCanonico(chave) {
			return valor, true
		}
	}
	return "", false
}
