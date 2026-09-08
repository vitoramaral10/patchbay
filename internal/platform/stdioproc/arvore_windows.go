//go:build windows

// A build tag é explícita, e não só o sufixo do arquivo: o sufixo é constraint
// implícita, e implícito é o que desaparece da build sem ninguém notar
// (seção 08.4 do estudo).

package stdioproc

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// nomeDeVariavelIgnoraCaixa é verdadeiro aqui: para o Windows, Path e PATH são a
// mesma variável, e um bloco de ambiente com as duas é comportamento indefinido.
const nomeDeVariavelIgnoraCaixa = true

// prepararArvore separa o filho do grupo de console do patchbay.
//
// O Windows não tem process group POSIX; o que existe é o grupo de console, que
// só serve para propagar Ctrl+C. Separar o filho garante que um Ctrl+C no
// terminal do patchbay não corra com o desligamento ordenado que o supervisor
// faz — quem mata a árvore é o Job Object, e ele obedece a uma ordem só.
func prepararArvore(cmd *exec.Cmd) {
	// CREATE_NO_WINDOW some com a janela de console que um binário GUI-menos
	// como node.exe ou python.exe abriria por padrão quando lançado sem
	// terminal próprio: sem a flag, cada upstream stdio piscaria uma janela
	// preta atrás do patchbay.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}

// Arvore é o Job Object que contém o filho e tudo que ele lançar.
//
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE é o que faz a árvore morrer mesmo se o
// patchbay for morto sem chance de rodar código: quando o processo termina, o
// sistema fecha o último handle do job, e fechar o último handle mata todo mundo
// que está dentro. É a diferença entre "matamos o neto se der tempo" e "o
// sistema operacional garante que o neto morre".
type Arvore struct {
	job windows.Handle
	pid int

	uma sync.Once
	err error
}

// adotarArvore cria o job e põe o processo dentro dele.
//
// Depois do Start e não antes: o os/exec da stdlib não expõe a thread principal
// do filho, então não há como criar o processo suspenso (CREATE_SUSPENDED) e
// retomá-lo depois da atribuição. A janela entre CreateProcess e
// AssignProcessToJobObject é a única brecha que sobra, e é por isso que esta
// chamada tem que colar no Start: um neto lançado nesses microssegundos
// escaparia do job. Falhar aqui é erro de verdade — um processo que não entrou
// no job é um processo que ninguém consegue matar por inteiro.
func adotarArvore(cmd *exec.Cmd) (*Arvore, error) {
	pid := cmd.Process.Pid

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("stdioproc: criar job object para %d: %w", pid, err)
	}

	limites := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	// unsafe.Pointer é a única forma de chamar SetInformationJobObject: a API do
	// Windows recebe um ponteiro cru e o tamanho da struct, e x/sys/windows
	// repassa isso como está. A struct é local, tem tamanho fixo e não escapa —
	// o ponteiro não sobrevive à chamada.
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limites)), // #nosec G103 -- contrato da API do Windows
		uint32(unsafe.Sizeof(limites)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("stdioproc: configurar job object de %d: %w", pid, err)
	}

	// O handle do processo vem de OpenProcess e não do os.Process: a stdlib não
	// exporta o handle que ela já tem. Abrir por PID aqui não tem a janela de
	// reuso que o mesmo padrão teria em outro supervisor: a stdlib mantém o
	// handle original do CreateProcess aberto até Wait ou Release, e enquanto
	// esse handle existir o Windows não recicla o PID para outro processo.
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("stdioproc: abrir processo %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("stdioproc: atribuir %d ao job object: %w", pid, err)
	}
	return &Arvore{job: job, pid: pid}, nil
}

// Encerrar mata todo mundo que está no job e devolve o handle. Idempotente.
//
// TerminateJobObject não tem versão educada, e é assim que tem que ser: a
// despedida do protocolo STDIO — fechar o stdin, esperar, SIGTERM, SIGKILL — já
// foi feita com o filho direto pelo transporte do go-sdk. O que resta aqui é o
// neto órfão, que não fala esse protocolo. Fechar o handle depois é obrigatório:
// enquanto ele existir, o job existe, e o job existe para ser fechado.
func (a *Arvore) Encerrar() error {
	a.uma.Do(func() {
		errTerminar := windows.TerminateJobObject(a.job, 1)
		errFechar := windows.CloseHandle(a.job)
		switch {
		case errTerminar != nil:
			a.err = fmt.Errorf("stdioproc: terminar job object de %d: %w", a.pid, errTerminar)
		case errFechar != nil:
			a.err = fmt.Errorf("stdioproc: fechar job object de %d: %w", a.pid, errFechar)
		}
	})
	return a.err
}

// Identificador descreve a árvore no log.
func (a *Arvore) Identificador() string {
	return fmt.Sprintf("job=0x%x pid=%d", uintptr(a.job), a.pid)
}
