//go:build unix

// A build tag é explícita de propósito. O sufixo _unix.go não é constraint
// nenhuma para o Go — ao contrário de _windows.go —, e confiar no nome do
// arquivo é como um arquivo some da build em silêncio.

package stdioproc

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// nomeDeVariavelIgnoraCaixa é falso aqui: no Unix PATH e Path são duas
// variáveis diferentes.
const nomeDeVariavelIgnoraCaixa = false

// prepararArvore põe o filho num process group próprio.
//
// Sem Setpgid o filho herda o grupo do patchbay, e matar o grupo mataria o
// patchbay junto. Com ele, o grupo do filho é o próprio PID dele, e todo neto
// nasce dentro do mesmo grupo por herança.
func prepararArvore(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Arvore é o process group do filho.
type Arvore struct {
	pgid int

	uma sync.Once
	err error
}

// adotarArvore lê o grupo que o Setpgid criou.
//
// Ler em vez de assumir pgid == pid: se por algum motivo o Setpgid não valeu,
// é melhor descobrir agora do que mandar um sinal para o grupo errado depois.
func adotarArvore(cmd *exec.Cmd) (*Arvore, error) {
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return nil, fmt.Errorf("stdioproc: ler process group de %d: %w", pid, err)
	}
	if pgid == syscall.Getpgrp() {
		// O filho ficou no grupo do patchbay. Matar esse grupo derrubaria o
		// gateway inteiro, então é melhor recusar a supervisão.
		return nil, fmt.Errorf("stdioproc: processo %d ficou no process group do patchbay (%d)", pid, pgid)
	}
	return &Arvore{pgid: pgid}, nil
}

// Encerrar mata o process group inteiro. Idempotente.
//
// SIGKILL direto, sem SIGTERM antes: a despedida educada — fechar o stdin,
// esperar, SIGTERM, SIGKILL — é do protocolo STDIO e o transporte do go-sdk já
// a fez com o filho direto antes de chegar aqui. O que sobra para este método é
// justamente quem não participa desse protocolo: o neto órfão, que não tem
// contrato nenhum com o patchbay e cuja espera deixaria o desligamento sem teto.
func (a *Arvore) Encerrar() error {
	a.uma.Do(func() {
		if err := syscall.Kill(-a.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			a.err = fmt.Errorf("stdioproc: matar process group %d: %w", a.pgid, err)
		}
	})
	return a.err
}

// Identificador descreve a árvore no log.
func (a *Arvore) Identificador() string {
	return fmt.Sprintf("pgid=%d", a.pgid)
}
