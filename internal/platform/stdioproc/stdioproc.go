// Package stdioproc é o ciclo de vida de um processo filho cuja árvore inteira
// precisa morrer junto com ele.
//
// É o único pacote do módulo que fala com o sistema operacional em nível de
// syscall: process group no Linux e no macOS, Job Object no Windows (seção 08.4
// do estudo). Manter isso fora de internal/upstream é o que permite testar o
// supervisor com um processo que pendura de propósito, sem MCP no meio.
//
// O que ele resolve é o vazamento de PID que derrubou o gateway anterior:
// (*os.Process).Kill mata o filho direto e deixa o neto vivo. Um servidor MCP
// STDIO real quase sempre tem neto — `npx` lança `node`, `uvx` lança `python` —,
// então matar só o filho é indistinguível de não matar nada.
//
// A separação em duas funções (Preparar antes de Start, Adotar depois) existe
// porque quem chama Start é o transporte do go-sdk, não este pacote: o
// mcp.CommandTransport recebe um *exec.Cmd pronto e o inicia sozinho.
package stdioproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// EsperaDePipe é quanto Wait tolera de I/O pendente depois de o processo sair.
//
// Sem ela, um neto que herdou o stderr do pai segura o Wait para sempre: o
// processo morreu, o pipe não fechou, e o supervisor fica preso num filho que
// já não existe. É o WaitDelay que a seção 08.4 exige.
const EsperaDePipe = 2 * time.Second

// ErrComandoVazio indica especificação sem programa para executar.
var ErrComandoVazio = errors.New("stdioproc: comando vazio")

// Especificacao é o processo que o admin pediu.
//
// Não há diretório de trabalho: o estudo não prevê a coluna, e herdar o
// diretório do patchbay é a escolha previsível — um caminho relativo digitado na
// tela resolve no mesmo lugar em que o processo está rodando.
type Especificacao struct {
	// Comando é o programa. Vai para exec.LookPath, então nome simples resolve
	// pelo PATH do processo patchbay.
	Comando string
	// Args são os argumentos, já separados. Nada de linha de comando única
	// partida por espaço aqui: quebrar string de shell é onde nasce injeção.
	Args []string
	// Ambiente é o ambiente completo do filho, no formato "CHAVE=valor".
	Ambiente []string
	// NaoHerdar lista prefixos (ou nomes completos) de variável que nunca
	// podem atravessar para o ambiente do filho, mesmo que Ambiente os
	// contenha. É a defesa contra vazar PATCHBAY_MASTER_KEY e as demais
	// variáveis internas do patchbay (PATCHBAY_DATA_DIR, por exemplo) para um
	// servidor MCP de terceiro: quem monta Ambiente pode herdar o processo
	// inteiro por conveniência (é o caso de AmbienteHerdado), e este filtro
	// roda de novo bem no ponto em que o bloco vira o ambiente real do
	// processo filho — mesmo que algum caminho futuro monte Ambiente sem
	// passar por AmbienteHerdado.
	NaoHerdar []string
	// Erros recebe o stderr do filho, linha a linha. Nulo descarta.
	Erros func(linha string)
}

// Preparar monta o *exec.Cmd já configurado para nascer numa árvore própria.
//
// Tudo o que precisa valer antes do Start acontece aqui — SysProcAttr é lido
// pelo exec no momento da criação do processo e mudá-lo depois não tem efeito
// nenhum.
func Preparar(esp Especificacao) (*exec.Cmd, error) {
	if strings.TrimSpace(esp.Comando) == "" {
		return nil, ErrComandoVazio
	}
	caminho, err := exec.LookPath(esp.Comando)
	if err != nil {
		return nil, fmt.Errorf("stdioproc: localizar %s: %w", esp.Comando, err)
	}

	// #nosec G204 -- comando e argumentos são configuração de administrador,
	// que é exatamente o que esta feature existe para executar. Não há shell no
	// meio: os argumentos vão como vetor, nunca como string a ser interpretada.
	//
	// exec.Command e não exec.CommandContext de propósito: o CommandContext mata
	// só o processo direto quando o contexto vence, que é exatamente a falha que
	// este pacote existe para corrigir — o neto sobreviveria. Quem encerra é o
	// supervisor, pela árvore, num ponto em que ele sabe que a sessão MCP já se
	// despediu.
	cmd := exec.Command(caminho, esp.Args...) //nolint:noctx // o ciclo de vida é da árvore, não do contexto
	cmd.Env = filtrarAmbiente(esp.Ambiente, esp.NaoHerdar)
	// Stdin e Stdout ficam para o transporte do go-sdk, que abre os dois pipes
	// no Connect. Stderr é nosso: servidor MCP registra diagnóstico ali, e sem
	// isto um upstream que morre na primeira linha morre em silêncio.
	if esp.Erros != nil {
		cmd.Stderr = &escritorDeLinhas{escrever: esp.Erros}
	}
	// Com Stderr sendo um io.Writer, o exec cria uma goroutine de cópia e o Wait
	// espera por ela. Um neto que herdou o stderr mantém o pipe aberto depois de
	// o pai morrer, e o Wait ficaria preso — WaitDelay é o teto disso.
	cmd.WaitDelay = EsperaDePipe
	prepararArvore(cmd)
	return cmd, nil
}

// Adotar põe o processo já iniciado sob controle do sistema operacional.
//
// Precisa ser chamado imediatamente depois do Start: no Windows, a janela entre
// CreateProcess e AssignProcessToJobObject é por onde um neto escaparia do job
// (seção 08.4). Não dá para fechar a janela inteira com o os/exec da stdlib —
// ela não expõe a thread principal do filho, então não há como criar suspenso e
// retomar depois da atribuição —, e por isso a chamada tem que colar no Start.
func Adotar(cmd *exec.Cmd) (*Arvore, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("stdioproc: adotar processo que não foi iniciado")
	}
	return adotarArvore(cmd)
}

// Iniciar é Preparar + Start + Adotar, para quem não precisa entregar o
// *exec.Cmd a um transporte de terceiro.
//
// Existe para o teste do supervisor rodar sem MCP no meio, que é a razão de este
// pacote estar separado de internal/upstream.
func Iniciar(esp Especificacao) (*exec.Cmd, *Arvore, error) {
	cmd, err := Preparar(esp)
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("stdioproc: iniciar %s: %w", esp.Comando, err)
	}
	arvore, err := Adotar(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, err
	}
	return cmd, arvore, nil
}

// AmbienteHerdado monta o ambiente do filho a partir do ambiente do patchbay,
// com as variáveis configuradas por cima.
//
// Herdar e não partir do zero: um servidor MCP lançado por `npx` ou `uvx`
// precisa de PATH, HOME e, no Windows, de SystemRoot e APPDATA. Ambiente vazio
// quebra o caso mais comum logo na primeira execução, e o erro que aparece na
// tela ("executable file not found") não diz nada sobre a causa.
func AmbienteHerdado(extras map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extras))
	sobrescritas := make(map[string]bool, len(extras))
	for chave := range extras {
		sobrescritas[chaveNormalizada(chave)] = true
	}
	for _, par := range base {
		nome, _, ok := strings.Cut(par, "=")
		if ok && sobrescritas[chaveNormalizada(nome)] {
			continue
		}
		out = append(out, par)
	}
	for chave, valor := range extras {
		out = append(out, chave+"="+valor)
	}
	return out
}

// chaveNormalizada aplica a regra de comparação de nome de variável do sistema.
// No Windows o nome não diferencia maiúscula de minúscula, e duas entradas para
// "Path" e "PATH" no mesmo bloco de ambiente é comportamento indefinido.
func chaveNormalizada(nome string) string {
	if nomeDeVariavelIgnoraCaixa {
		return strings.ToUpper(nome)
	}
	return nome
}

// filtrarAmbiente remove do bloco de ambiente toda variável cujo nome comece
// por um dos prefixos de naoHerdar. prefixos vazio devolve ambiente sem cópia,
// para o caso comum (upstream stdio sem NaoHerdar configurado) não pagar
// alocação por nada.
func filtrarAmbiente(ambiente []string, naoHerdar []string) []string {
	if len(naoHerdar) == 0 {
		return ambiente
	}
	out := make([]string, 0, len(ambiente))
	for _, par := range ambiente {
		nome, _, _ := strings.Cut(par, "=")
		if temPrefixo(nome, naoHerdar) {
			continue
		}
		out = append(out, par)
	}
	return out
}

// temPrefixo compara com a mesma regra de caixa do sistema operacional: no
// Windows PATCHBAY_MASTER_KEY e patchbay_master_key são a mesma variável.
func temPrefixo(nome string, prefixos []string) bool {
	nome = chaveNormalizada(nome)
	for _, p := range prefixos {
		if strings.HasPrefix(nome, chaveNormalizada(p)) {
			return true
		}
	}
	return false
}

// escritorDeLinhas transforma o stderr do filho em chamadas linha a linha.
//
// Linha e não bloco porque o destino é log estruturado: um Write pode trazer
// meia linha ou três, e emitir o buffer cru produz registro cortado no meio.
type escritorDeLinhas struct {
	escrever func(string)

	restante []byte
	// descartando é true logo depois de uma linha ter sido cortada em
	// limiteDeLinha sem o \n ainda ter chegado. Enquanto durar, o que chegar
	// é o resto de uma linha que já foi truncada e emitida — não o começo da
	// próxima —, e o \n que fecha essa linha é o único sinal que tira o
	// coletor deste modo.
	descartando bool
	// emitidas e suprimidas são o teto de linhas por processo (item 2a): um
	// servidor tagarela de verdade — um loop que nunca para de escrever —
	// não pode fazer o log de produção crescer sem limite.
	emitidas   int
	suprimidas int
}

// limiteDeLinha corta linha absurda antes de ela virar registro de log absurdo.
// Um servidor que despeja um stack trace de um megabyte no stderr não pode
// decidir o tamanho do nosso log.
const limiteDeLinha = 4096

// sufixoTruncado marca, na própria linha, que ela veio cortada — sem isto o
// log mostra uma linha "completa" que na verdade parou no meio da frase.
const sufixoTruncado = " …(truncado)"

// limiteDeLinhasPorProcesso é quantas linhas de stderr por processo viram
// registro de log. As demais são contadas e resumidas, nunca escritas uma a
// uma: emitir todas seria o próprio problema que o teto existe para evitar.
const limiteDeLinhasPorProcesso = 200

// intervaloDeAvisoDeSupressao é a cada quantas linhas suprimidas o coletor
// repete o aviso. Um processo pendurado por horas escrevendo sem parar não
// pode deixar a supressão em silêncio depois do primeiro aviso.
const intervaloDeAvisoDeSupressao = 1000

func (e *escritorDeLinhas) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if e.descartando {
			i := indiceDeQuebra(p)
			if i < 0 {
				// A linha longa ainda não terminou: nada para guardar, só
				// esperar o \n que encerra o modo de descarte.
				return total, nil
			}
			e.descartando = false
			p = p[i+1:]
			continue
		}

		e.restante = append(e.restante, p...)
		p = nil

		for {
			i := indiceDeQuebra(e.restante)
			if i < 0 {
				break
			}
			e.emitirLinha(e.restante[:i], false)
			e.restante = e.restante[i+1:]
		}

		if len(e.restante) > limiteDeLinha {
			// A linha ainda não fechou e já passou do teto: truncar agora e
			// descartar o resto até o próximo \n é o que evita que o buffer
			// cresça sem limite com um processo que nunca manda quebra de
			// linha, e é o que impede o resto de colar na linha seguinte.
			e.emitirLinha(e.restante[:limiteDeLinha], true)
			e.restante = nil
			e.descartando = true
		}
	}
	return total, nil
}

// emitirLinha aplica o corte de \r final e o teto de tamanho antes de decidir
// se a linha vira registro de log.
func (e *escritorDeLinhas) emitirLinha(linha []byte, forcarTruncado bool) {
	texto := strings.TrimRight(string(linha), "\r")
	truncada := forcarTruncado || len(texto) > limiteDeLinha
	if truncada {
		if len(texto) > limiteDeLinha {
			texto = texto[:limiteDeLinha]
		}
		texto += sufixoTruncado
	}
	if !truncada && strings.TrimSpace(texto) == "" {
		return
	}
	e.contarEEmitir(texto)
}

// contarEEmitir aplica o teto de linhas por processo: as primeiras
// limiteDeLinhasPorProcesso viram log normalmente, e as que vierem depois só
// contam, com um aviso de resumo a cada intervaloDeAvisoDeSupressao.
func (e *escritorDeLinhas) contarEEmitir(texto string) {
	if e.emitidas >= limiteDeLinhasPorProcesso {
		e.suprimidas++
		if e.suprimidas == 1 || e.suprimidas%intervaloDeAvisoDeSupressao == 0 {
			e.escrever(fmt.Sprintf("stderr: suprimidas %d linha(s) além do teto de %d por processo",
				e.suprimidas, limiteDeLinhasPorProcesso))
		}
		return
	}
	e.emitidas++
	e.escrever(texto)
}

func indiceDeQuebra(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}
