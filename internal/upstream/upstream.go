// Package upstream é dono do ciclo de vida da sessão MCP de cada servidor
// configurado.
//
// Nenhuma operação de upstream acontece no caminho da requisição do cliente
// (decisão 4 do estudo): a conexão e a descoberta rodam em goroutine de
// supervisão, e o endpoint serve o snapshot que estiver materializado. Se a
// conexão fosse aberta na requisição, o hang do upstream viraria latência do
// endpoint.
//
// A máquina de estados completa — backoff com jitter e teto, contador de
// connects abandonados, desabilitação automática, sonda — é da fatia 3. O que
// existe aqui não a contraria: nenhum estado de erro é persistido, e voltar de
// degradado passa obrigatoriamente por uma sessão nova.
package upstream

import (
	"errors"
	"fmt"
	"time"
)

// Tipos de upstream. Só http é servido na fatia 1; sse e stdio existem no
// schema porque a forma é contrato (seções 08.4 e 08.8).
const (
	TipoHTTP  = "http"
	TipoSSE   = "sse"
	TipoSTDIO = "stdio"
)

// Erros sentinela do pacote.
var (
	// ErrDesconhecido indica upstream que não está sob supervisão.
	ErrDesconhecido = errors.New("upstream: desconhecido")
	// ErrIndisponivel indica upstream sem sessão pronta no momento da chamada.
	ErrIndisponivel = errors.New("upstream: indisponível")
	// ErrTipoNaoSuportado indica tipo de transporte ainda não implementado.
	ErrTipoNaoSuportado = errors.New("upstream: tipo de transporte não suportado")
)

// Estado é a posição de um upstream na máquina de estados. Vive em memória: o
// SQLite guarda a intenção do admin (habilitado) e o último erro como texto
// para a UI, nunca o estado. Persistir ERROR é o que travava a reconexão no
// MetaMCP.
type Estado string

// Estados da fatia 1. A fatia 3 acrescenta sem_consentimento, sonda_falhou e
// desabilitado_automaticamente.
const (
	EstadoNovo       Estado = "novo"
	EstadoConectando Estado = "conectando"
	EstadoPronto     Estado = "pronto"
	EstadoDegradado  Estado = "degradado"
)

// Config é a configuração de um upstream, como ela sai do banco.
type Config struct {
	ID      int64
	Nome    string
	Tipo    string
	URL     string
	Timeout time.Duration
}

// Validar recusa configuração que o gerente não sabe supervisionar.
func (c Config) Validar() error {
	if c.Nome == "" {
		return errors.New("upstream: nome vazio")
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("upstream %s: timeout precisa ser positivo", c.Nome)
	}
	switch c.Tipo {
	case TipoHTTP:
		if c.URL == "" {
			return fmt.Errorf("upstream %s: url vazia", c.Nome)
		}
		return nil
	case TipoSSE, TipoSTDIO:
		return fmt.Errorf("%w: %s", ErrTipoNaoSuportado, c.Tipo)
	default:
		return fmt.Errorf("%w: %s", ErrTipoNaoSuportado, c.Tipo)
	}
}

// Situacao é o retrato de um upstream para quem observa de fora (UI, log).
type Situacao struct {
	Config      Config
	Estado      Estado
	UltimoErro  string
	Ferramentas int
	TentativaEm time.Time
}
