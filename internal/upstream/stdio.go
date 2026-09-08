package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/platform/stdioproc"
)

// esperaDeSaida é quanto o supervisor espera o processo sair sozinho depois de o
// stdin ter sido fechado, antes de matar a árvore.
//
// Fixa e curta de propósito: o adeus do protocolo STDIO já aconteceu quando
// chegamos aqui, e esperar mais que isso é atrasar o desligamento do patchbay
// por um servidor que decidiu não sair.
const esperaDeSaida = 3 * time.Second

// esperaDeColeta limita o Wait depois de a árvore ter sido morta. O processo já
// não existe; o que pode faltar é o pipe de stderr fechar, e o WaitDelay do
// exec já é o teto disso.
const esperaDeColeta = stdioproc.EsperaDePipe + time.Second

// processoUpstream é a alça do processo STDIO de um upstream.
//
// Um por upstream, compartilhado por todas as sessões de cliente (decisão da
// seção 08.4). Nunca um por sessão: spawn por sessão é literalmente o bug que
// vazava um processo vivo por reconexão até esgotar os PIDs da máquina, e é a
// razão de o gateway anterior ter sido abandonado.
type processoUpstream struct {
	cmd    *exec.Cmd
	arvore *stdioproc.Arvore
	nome   string
	log    *slog.Logger

	uma sync.Once
}

// abrirProcesso lança o processo do upstream e devolve o transporte MCP ligado
// aos pipes dele.
//
// A árvore é adotada **antes** de qualquer byte de MCP trafegar. É o que faz o
// watchdog ter o que matar quando o initialize pendura: se o processo só fosse
// iniciado lá dentro do Connect do SDK, o supervisor descobriria o timeout sem
// nenhuma alça para o processo que ficou preso.
func (g *Gerente) abrirProcesso(ctx context.Context, cfg Config) (mcp.Transport, *processoUpstream, error) {
	ambiente, err := g.ambienteDe(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	registro := g.log.With("upstream", cfg.Nome, "upstream_id", cfg.ID)
	cmd, err := stdioproc.Preparar(stdioproc.Especificacao{
		Comando:  cfg.Comando,
		Args:     cfg.Args,
		Ambiente: ambiente,
		// PATCHBAY_ inteiro, e não só a chave mestra: qualquer variável
		// interna do patchbay (PATCHBAY_DATA_DIR, e o que vier depois) é
		// contexto do próprio processo, não do upstream que ele supervisiona.
		NaoHerdar: []string{"PATCHBAY_"},
		// O stderr do servidor é diagnóstico, não protocolo: em debug porque um
		// servidor tagarela não pode encher o log de produção sozinho.
		Erros: func(linha string) { registro.Debug("stderr do upstream stdio", "linha", linha) },
	})
	if err != nil {
		return nil, nil, fmt.Errorf("upstream %s: %w", cfg.Nome, err)
	}

	// Os dois pipes precisam existir antes do Start; depois dele o exec recusa.
	saida, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("upstream %s: abrir stdout do processo: %w", cfg.Nome, err)
	}
	// Fechamento armado até o Start dar certo: se StdinPipe falhar daqui a
	// pouco, ninguém mais chamaria Start nem Wait — os dois que normalmente
	// fecham o lado de leitura deste pipe —, e ele vazaria até o coletor de
	// lixo achar o *exec.Cmd por acaso.
	fecharSaida := true
	defer func() {
		if fecharSaida {
			_ = saida.Close()
		}
	}()

	entrada, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("upstream %s: abrir stdin do processo: %w", cfg.Nome, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("upstream %s: iniciar %s: %w", cfg.Nome, cfg.Comando, err)
	}
	fecharSaida = false

	arvore, err := stdioproc.Adotar(cmd)
	if err != nil {
		// Processo que não entrou na árvore é processo que ninguém consegue
		// matar por inteiro depois. Melhor derrubá-lo agora, enquanto ele ainda
		// não teve tempo de lançar neto nenhum.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("upstream %s: %w", cfg.Nome, err)
	}

	p := &processoUpstream{cmd: cmd, arvore: arvore, nome: cfg.Nome, log: registro}
	registro.Info("processo de upstream stdio iniciado",
		"comando", cfg.Comando, "pid", cmd.Process.Pid, "arvore", arvore.Identificador())

	// io.NopCloser no stdout pela mesma razão que o CommandTransport do SDK usa:
	// a conexão se fecha fechando o stdin, e fechar o stdout por baixo do Wait
	// só produz "file already closed" no lugar do erro de verdade.
	//
	// Isto é seguro porque ioConn.Read do go-sdk não bloqueia lendo do pipe
	// para sempre: ele seleciona entre o Read de verdade e o canal t.closed,
	// então dar Close() no mcp.IOTransport (o que a sessão faz ao desistir de
	// um Connect preso) destrava o leitor mesmo sem o stdout do filho ter
	// mandado EOF. É contrato de uma dependência externa e não deste pacote —
	// uma atualização futura do go-sdk pode mudar esse Read sem aviso nenhum
	// aqui, e é por isso que este comentário existe.
	return &mcp.IOTransport{Reader: io.NopCloser(saida), Writer: entrada}, p, nil
}

// Encerrar recolhe o processo e tudo o que ele lançou, dando antes a chance de
// ele sair sozinho. É o caminho do desligamento ordenado.
//
// A ordem é o ponto inteiro da fatia: primeiro o filho direto sai sozinho (o
// stdin dele já foi fechado por quem chamou), e só então a árvore é morta —
// mesmo que ele já tenha saído. O que a árvore recolhe é o **neto**: `npx` lança
// `node`, `uvx` lança `python`, e é o neto que (*os.Process).Kill deixaria vivo
// para sempre.
func (p *processoUpstream) Encerrar() { p.encerrar(esperaDeSaida) }

// EncerrarAgora mata a árvore sem esperar nada. É o caminho do hang.
//
// Um processo que não respondeu ao initialize dentro do timeout já gastou toda a
// paciência que tinha; esperar mais três segundos por uma saída ordenada seria
// atrasar a ida para degradado em troca de nada. E aqui matar tem um efeito
// extra: o Connect preso lá em cima destrava sozinho quando o stdout do filho
// fecha — no STDIO, ao contrário do HTTP, o abandono da issue #1189 não deixa
// resíduo, porque o supervisor é dono do outro lado do cano.
func (p *processoUpstream) EncerrarAgora() { p.encerrar(0) }

func (p *processoUpstream) encerrar(graca time.Duration) {
	p.uma.Do(func() {
		// Capturado antes de Encerrar(): o handle que ele fecha (o Job Object
		// no Windows) não muda o texto do identificador, mas prender a
		// leitura a um ponto anterior ao fechamento é o que continua correto
		// se Encerrar() um dia passar a zerar o estado depois de fechar.
		identificador := p.arvore.Identificador()

		saiu := make(chan error, 1)
		go func() { saiu <- p.cmd.Wait() }()

		recolhido := false
		if graca > 0 {
			select {
			case err := <-saiu:
				p.registrarSaida(err)
				recolhido = true
			case <-time.After(graca):
				// "Não confirmou saída" e não "não saiu": o que se sabe aqui
				// é só que o Wait não voltou dentro da espera — o processo
				// pode muito bem já ter morrido com um descendente segurando
				// o pipe, e é exatamente isso que registrarSaida esclarece
				// se o Wait vier a voltar depois, lá embaixo.
				p.log.Warn("processo de upstream stdio não confirmou saída depois do stdin fechado",
					"espera", graca, "pid", p.cmd.Process.Pid)
			}
		}

		if err := p.arvore.Encerrar(); err != nil {
			p.log.Warn("falha ao matar a árvore do processo de upstream stdio",
				"arvore", identificador, "erro", err)
		}

		// A garantia deste método é "a árvore está morta", e ela acabou de ser
		// dada. Recolher o zumbi corre por fora: o que pode faltar é um pipe
		// herdado por neto fechar, e segurar o supervisor por isso seria pagar
		// latência de desligamento por contabilidade.
		if recolhido {
			return
		}
		go func() {
			select {
			case err := <-saiu:
				p.registrarSaida(err)
			case <-time.After(esperaDeColeta):
				p.log.Error("processo de upstream stdio não foi recolhido",
					"pid", p.cmd.Process.Pid, "arvore", identificador)
			}
		}()
	})
}

// registrarSaida conta como o processo terminou.
//
// Saída com código diferente de zero é informação de diagnóstico, não erro do
// patchbay: o supervisor já vai tratar a queda como falha e agendar o backoff.
func (p *processoUpstream) registrarSaida(err error) {
	var saida *exec.ExitError
	switch {
	case err == nil:
		p.log.Info("processo de upstream stdio saiu", "codigo", 0)
	case errors.As(err, &saida):
		p.log.Warn("processo de upstream stdio saiu com erro", "codigo", saida.ExitCode())
	case errors.Is(err, exec.ErrWaitDelay):
		// O processo saiu — Wait só não voltou na hora porque um descendente
		// (o neto que `npx` deixa para trás, por exemplo) ainda segurava o
		// pipe de stderr depois do WaitDelay. Não é falha de recolhimento: é
		// exatamente o caso que WaitDelay existe para destravar.
		p.log.Info("processo de upstream stdio saiu; um descendente ainda segurava o pipe de stderr")
	default:
		p.log.Warn("falha ao recolher o processo de upstream stdio", "erro", err)
	}
}

// ambienteDe monta o ambiente do processo: o do patchbay, mais as variáveis
// configuradas, mais as variáveis sensíveis decifradas na hora.
//
// As sensíveis são lidas do banco a cada início de processo e não ficam em
// lugar nenhum além do bloco de ambiente do filho: trocar uma pela tela vale no
// próximo restart, sem cache a invalidar.
func (g *Gerente) ambienteDe(ctx context.Context, cfg Config) ([]string, error) {
	extras := make(map[string]string, len(cfg.Env))
	for chave, valor := range cfg.Env {
		extras[chave] = valor
	}

	if g.credenciais != nil {
		ctxLeitura, cancelar := context.WithTimeout(ctx, cfg.Timeout)
		defer cancelar()

		creds, err := g.credenciais(ctxLeitura, cfg.ID)
		if err != nil {
			// A mensagem diz qual upstream falhou, nunca o que ele guarda.
			return nil, fmt.Errorf("upstream %s: ler variáveis sensíveis: %w", cfg.Nome, err)
		}
		for _, c := range creds {
			if c.Tipo == CredencialEnv {
				extras[c.Nome] = c.Valor.Revelar()
			}
		}
	}
	return stdioproc.AmbienteHerdado(extras), nil
}
