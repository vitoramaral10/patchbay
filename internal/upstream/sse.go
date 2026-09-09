package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// O transporte SSE legado (fatia 14).
//
// É o HTTP+SSE da revisão 2024-11-05 do MCP: um GET que fica pendurado servindo
// os eventos do servidor e um POST por mensagem do cliente, no endereço que o
// primeiro evento anuncia. O go-sdk já o traz pronto em mcp.SSEClientTransport,
// e daqui para baixo a máquina de estados é a mesma do Streamable HTTP e do
// STDIO: mesmo watchdog, mesmo backoff, mesmo teto de abandonos, mesma tela.
//
// As credenciais são as mesmas do HTTP — bearer, headers estáticos e OAuth —
// porque do ponto de vista de quem autentica não há diferença: as duas coisas
// são requisição HTTP com Authorization. O que muda é só o desenho do canal.

// transporteSSE desamarra o stream do contexto da chamada de Connect.
//
// É a diferença que faz o SSE legado funcionar dentro deste supervisor. O
// SSEClientTransport do SDK abre o GET pendurado com o contexto que recebe no
// Connect (mcp/sse.go:371) e a sessão inteira morre junto com ele. O supervisor
// cancela o contexto do Connect assim que ele volta — é o watchdog da issue
// #1189, e no Streamable HTTP isso não faz diferença porque lá a conexão já
// nasce destacada da chamada. Aqui faria: a sessão cairia no primeiro
// tools/list, com EOF, e o upstream ficaria oscilando entre pronto e degradado
// sem que nada no log explicasse por quê.
//
// O prazo do watchdog continua valendo enquanto a conexão está nascendo: se o
// contexto da chamada morre antes de o Connect voltar, a conexão morre junto.
// Depois disso ela só termina por Close — que é o que o supervisor chama no
// defer, inclusive no desligamento.
type transporteSSE struct {
	base *mcp.SSEClientTransport

	mu       sync.Mutex
	cancelar context.CancelFunc
}

func (t *transporteSSE) Connect(ctx context.Context) (mcp.Connection, error) {
	vida, cancelar := context.WithCancel(context.WithoutCancel(ctx))
	t.mu.Lock()
	t.cancelar = cancelar
	t.mu.Unlock()

	// Enquanto o Connect não volta, o contexto da chamada ainda manda.
	nasceu := make(chan struct{})
	go func() {
		select {
		case <-nasceu:
		case <-ctx.Done():
			cancelar()
		}
	}()

	conexao, err := t.base.Connect(vida)
	close(nasceu)
	if err != nil {
		cancelar()
		return nil, fmt.Errorf("conectar por sse: %w", err)
	}
	return &conexaoSSE{Connection: conexao, cancelar: cancelar}, nil
}

// Abandonar corta a conexão SSE que esta tentativa deixou pendurada.
//
// Existe para o watchdog do supervisor (gerente.go): quando ele desiste de
// esperar cliente.Connect por timeout, o transporte já pode ter aberto o GET
// pendurado do SSE — Connect voltou sem erro — mas ninguém chamou Close nele,
// porque o próprio cliente.Connect ainda está travado numa etapa seguinte
// (a negociação initialize) e nunca devolveu a sessão. Sem isto, o goroutine
// de leitura do stream e o socket ficariam presos até o processo reiniciar.
// Idempotente e seguro de chamar de qualquer goroutine, inclusive antes de
// Connect terminar: cancelar vida faz t.base.Connect voltar com erro, e depois
// disso mais uma chamada não tem efeito porque cancelar já é no-op repetido.
func (t *transporteSSE) Abandonar() {
	t.mu.Lock()
	cancelar := t.cancelar
	t.mu.Unlock()
	if cancelar != nil {
		cancelar()
	}
}

// conexaoSSE é a conexão do SDK com o cancelamento da própria vida junto.
type conexaoSSE struct {
	mcp.Connection
	cancelar context.CancelFunc
}

func (c *conexaoSSE) Close() error {
	defer c.cancelar()
	return c.Connection.Close()
}

// transporteOAuthSSE injeta o bearer do OAuth nas requisições do transporte SSE
// e roda o fluxo de autorização quando o servidor recusa.
//
// Existe porque mcp.SSEClientTransport não tem campo OAuthHandler — só o
// StreamableClientTransport tem (mcp/streamable.go:1961). Sem isto, um upstream
// SSE com OAuth ficaria sem nenhum Authorization e o modo simplesmente não
// funcionaria para ele; reimplementar o transporte SSE inteiro para ganhar um
// campo seria trocar cinquenta linhas de RoundTripper por um transporte
// mantido à mão.
//
// A autorização só é tentada em requisição sem corpo, que é o GET que abre o
// stream — e é justamente onde a descoberta RFC 9728 acontece, porque é a
// primeira requisição da sessão. Num POST de mensagem o corpo já foi consumido e
// repeti-lo não é seguro; o 401 sobe, a sessão morre, e a reconexão passa pelo
// GET de novo. É o mesmo caminho que o "não existe transição direta degradado →
// pronto" já obriga.
type transporteOAuthSSE struct {
	base    http.RoundTripper
	handler auth.OAuthHandler
	nome    string
}

// comOAuthSSE devolve uma cópia do cliente com o OAuth no transporte.
//
// Cópia e não o cliente compartilhado: o token é de um upstream só, e um
// RoundTripper compartilhado mandaria o Authorization de um servidor para outro.
func comOAuthSSE(cliente *http.Client, handler auth.OAuthHandler, nome string) *http.Client {
	base := cliente.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copia := *cliente
	copia.Transport = &transporteOAuthSSE{base: base, handler: handler, nome: nome}
	return &copia
}

func (t *transporteOAuthSSE) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone: um RoundTripper não pode alterar a requisição que recebe.
	clone := req.Clone(req.Context())
	if err := t.autorizar(clone); err != nil {
		return nil, err
	}

	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	recusou := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
	if !recusou || req.Body != nil {
		return resp, nil
	}

	// Authorize fecha o corpo da resposta que recebe, por contrato da interface.
	if err := t.handler.Authorize(req.Context(), clone, resp); err != nil {
		return nil, fmt.Errorf("upstream %s: autorizar sessão SSE: %w", t.nome, err)
	}

	segunda := req.Clone(req.Context())
	if err := t.autorizar(segunda); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(segunda)
}

// autorizar põe o Authorization da fonte de token na requisição.
//
// invalid_grant é tolerado e a requisição segue sem header, do mesmo jeito que o
// StreamableClientTransport faz (mcp/streamable.go:2397): o refresh token morreu,
// e o que resolve é o 401 que vem em seguida disparar o fluxo de autorização —
// não abortar a requisição com um erro que ninguém sabe interpretar.
func (t *transporteOAuthSSE) autorizar(req *http.Request) error {
	fonte, err := t.handler.TokenSource(req.Context())
	if err != nil {
		return fmt.Errorf("upstream %s: fonte de token: %w", t.nome, err)
	}
	if fonte == nil {
		return nil
	}
	tok, err := fonte.Token()
	if err != nil {
		var recusa *oauth2.RetrieveError
		if errors.As(err, &recusa) && recusa.ErrorCode == "invalid_grant" {
			return nil
		}
		if errors.Is(err, ErrSemConsentimento) {
			return nil
		}
		// %s com mensagemDeFalha, e não %w com o erro cru: este ramo já excluiu
		// invalid_grant acima, então pode ser o token endpoint respondendo algo
		// que o x/oauth2 não reconhece como erro OAuth — o caso em que
		// RetrieveError imprime o corpo bruto da resposta (token.go:212), que
		// pode ecoar a credencial enviada.
		return fmt.Errorf("upstream %s: obter token: %s", t.nome, mensagemDeFalha(err))
	}
	if tok != nil {
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	return nil
}
