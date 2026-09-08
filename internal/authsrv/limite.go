package authsrv

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Limites padrão das rotas do AS, por chave e por minuto.
//
// O AS é o único lugar do patchbay onde um desconhecido pode fazer o processo
// consultar o banco em laço: /token compara hash de código e de refresh, e
// /authorize resolve client_id. Sem teto, ele vira oráculo — dá para medir quais
// client_id existem e para varrer código por força bruta enquanto a janela de
// cinco minutos não fecha. Os números são generosos de propósito: um cliente
// legítimo faz um punhado de requisições por conexão, e apertar isso trocaria um
// ataque improvável por uma falha real de conexão.
const (
	LimiteTokenPorMinuto     = 60
	LimiteAutorizarPorMinuto = 30
)

// limitador é um balde de fichas por chave, com reposição contínua.
//
// Próprio e não biblioteca: são trinta linhas com relógio injetado, e o que uma
// dependência traria a mais — políticas, distribuição, métricas — não é o que
// falta aqui.
type limitador struct {
	mu     sync.Mutex
	baldes map[string]*balde
	teto   float64
	janela time.Duration
	agora  func() time.Time
}

type balde struct {
	fichas   float64
	ultimoEm time.Time
}

// limiteMaximoDeChaves é o teto de chaves rastreadas. Passar disso significa
// varredura de IPs ou de client_id; a tabela é zerada em vez de crescer, o que
// custa uma janela de tolerância a quem estava usando o serviço e devolve a
// memória a quem paga por ela.
const limiteMaximoDeChaves = 4096

func novoLimitador(porJanela int, janela time.Duration, agora func() time.Time) *limitador {
	return &limitador{
		baldes: make(map[string]*balde),
		teto:   float64(porJanela),
		janela: janela,
		agora:  agora,
	}
}

// permitir consome uma ficha da chave e informa se a requisição passa.
func (l *limitador) permitir(chave string) bool {
	agora := l.agora()

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.baldes) > limiteMaximoDeChaves {
		l.baldes = make(map[string]*balde)
	}

	b, ok := l.baldes[chave]
	if !ok {
		l.baldes[chave] = &balde{fichas: l.teto - 1, ultimoEm: agora}
		return true
	}

	decorrido := agora.Sub(b.ultimoEm)
	if decorrido > 0 {
		b.fichas += l.teto * (float64(decorrido) / float64(l.janela))
		if b.fichas > l.teto {
			b.fichas = l.teto
		}
		b.ultimoEm = agora
	}
	if b.fichas < 1 {
		return false
	}
	b.fichas--
	return true
}

// chaveDoCliente identifica quem está batendo na porta.
//
// O IP vem do RemoteAddr e nunca de X-Forwarded-For: o patchbay fica atrás de um
// proxy reverso que ele não configura, e confiar num header que o cliente
// escreve transformaria o limitador em decoração — basta variar o header para
// zerar o balde.
func chaveDoCliente(r *http.Request, sufixo string) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if sufixo == "" {
		return ip
	}
	return ip + "|" + sufixo
}
