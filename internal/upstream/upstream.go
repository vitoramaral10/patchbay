// Package upstream é dono do ciclo de vida da sessão MCP de cada servidor
// configurado.
//
// Nenhuma operação de upstream acontece no caminho da requisição do cliente
// (decisão 4 do estudo): a conexão e a descoberta rodam em goroutine de
// supervisão, e o endpoint serve o snapshot que estiver materializado. Se a
// conexão fosse aberta na requisição, o hang do upstream viraria latência do
// endpoint.
//
// A máquina de estados é a da seção 05: nenhum estado de erro é persistido, e
// voltar de degradado passa obrigatoriamente por uma sessão nova. O backoff com
// jitter e teto, o watchdog de conexão com contador de abandonos e a
// desabilitação automática ao passar do teto estão aqui; sonda_falhou e
// sem_consentimento são estados declarados que só as fatias 9 e 7 sabem entrar.
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

// Os sete estados da seção 05.
//
// Cinco são dirigidos por este pacote. EstadoSondaFalhou e
// EstadoSemConsentimento estão declarados porque o vocabulário é contrato — a
// UI, o log e o export falam dele —, mas quem os alcança são a sonda funcional
// (fatia 9) e o consentimento OAuth de upstream (fatia 7). Declará-los agora
// evita que os dois inventem um nome diferente para o mesmo estado depois.
const (
	// EstadoNovo é onde todo upstream habilitado começa, em todo boot.
	EstadoNovo Estado = "novo"
	// EstadoConectando é a tentativa em curso, com o watchdog armado.
	EstadoConectando Estado = "conectando"
	// EstadoPronto é sessão aberta e tools/list respondido.
	EstadoPronto Estado = "pronto"
	// EstadoDegradado é "não consigo falar com ele": as ferramentas saem do
	// catálogo e a reconexão está agendada no backoff.
	EstadoDegradado Estado = "degradado"
	// EstadoDesabilitado é a supervisão desligada por autoproteção, ao passar do
	// teto de abandonos. A intenção do admin de desabilitar não passa por aqui:
	// ela tira o upstream da supervisão inteira.
	EstadoDesabilitado Estado = "desabilitado"
	// EstadoSondaFalhou é "falo com ele, ele lista ferramentas, e a chamada de
	// verdade não funciona". Fatia 9.
	EstadoSondaFalhou Estado = "sonda_falhou"
	// EstadoSemConsentimento é refresh de OAuth recusado. Fatia 7.
	EstadoSemConsentimento Estado = "sem_consentimento"
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
//
// Tudo aqui vive em memória. O banco guarda a intenção do admin (habilitado) e
// mais nada da máquina: persistir estado de erro é o que travava a reconexão no
// MetaMCP, porque o estado passa a ser uma decisão gravada em vez da leitura de
// uma tentativa.
type Situacao struct {
	Config      Config
	Estado      Estado
	UltimoErro  string
	Ferramentas int
	// TentativaEm é quando a tentativa em curso (ou a última) começou.
	TentativaEm time.Time
	// ProximaEm é quando o backoff libera a próxima tentativa. Zero quando não
	// há nenhuma agendada — pronto, ou desabilitado por autoproteção.
	//
	// Está na tela porque sem ela o admin fica clicando em reconectar sem saber
	// que a reconexão já está marcada (seção 11).
	ProximaEm time.Time
	// Falhas é o número de tentativas consecutivas sem chegar a pronto. É o
	// expoente do backoff.
	Falhas int
	// Abandonos é quantos connects consecutivos foram abandonados por timeout
	// do watchdog desde a última vez que a sessão chegou a pronto. É o número
	// que abandonosEstouraram compara contra o teto: um upstream saudável com
	// um timeout de connect esporádico a cada poucos dias não pode ficar
	// desabilitado para sempre por um contador que nunca zera.
	//
	// Cada abandono deixa duas goroutines presas, não uma: a do Connect que
	// nunca retorna, e o coletor que aguarda `<-pronto` para fechar a sessão
	// se ela chegar tarde. As duas só somem no próximo boot (seção 08.3).
	Abandonos int
	// AbandonosTotais é o total acumulado de connects abandonados desde o
	// boot (ou desde o último Aplicar). Ao contrário de Abandonos, nunca zera
	// sozinho ao chegar a pronto — só definir o apaga —, e é por isso que a
	// tela mostra os dois: um para saber se está perto do teto agora, outro
	// para ver o resíduo de goroutines acumulado ao longo do tempo.
	AbandonosTotais int
	// Motivo diz por que a supervisão se desligou sozinha. Vazio enquanto ela
	// está no ar: upstream que se desabilita em silêncio é indistinguível de
	// upstream que alguém apagou.
	Motivo string
}
