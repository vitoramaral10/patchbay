package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// timeoutGravarConcessao limita a escrita disparada por um refresh.
//
// Curto porque é uma linha só e porque quem espera é o refresh: se o banco está
// travado, insistir aqui só transforma indisponibilidade de escrita em
// indisponibilidade de token.
const timeoutGravarConcessao = 5 * time.Second

// FonteToken é a oauth2.TokenSource de um upstream.
//
// Exatamente uma por upstream para toda a vida do processo, e a unicidade da
// instância *é* o mutex que o requisito de refresh serializado pede (seção 08.5).
// A corrida que se quer evitar — duas requisições vendo 401 ao mesmo tempo e
// correndo para dar refresh com o mesmo refresh token, o que dispara detecção de
// replay em provedor com rotação de família — não existe dentro de uma
// instância: oauth2.reuseTokenSource já guarda o token atrás de um mutex. Ela
// reaparece no minuto em que alguém cria duas instâncias para o mesmo upstream,
// por exemplo uma por endpoint que o inclui. Daí a regra: o broker é o único
// dono, a fonte nasce com o upstream e morre com ele.
//
// O mutex daqui acrescenta uma coisa que o do x/oauth2 não faz: ele serializa
// também a *persistência*. Sem isso, duas gravações do mesmo refresh podiam
// chegar ao banco fora de ordem e deixar gravado o token mais velho.
//
// O que é gravado é o *oauth2.Token que a source devolve, nunca o corpo da
// resposta HTTP. O x/oauth2 já não sobrescreve RefreshToken com valor vazio numa
// requisição de refresh, então o token que sai daqui já vem mesclado; parsear a
// resposta do token endpoint por conta própria é a única forma de errar isso, e
// reintroduziria um bug que a biblioteca não tem.
//
// Invariante de lock: f.mu nunca é tomado com s.mu (de sessaoOAuth) preso.
// aoRevogar por dentro chama marcarPrecisa, que pega s.mu — e por isso Token()
// só o chama depois de soltar f.mu, nunca no meio da seção crítica.
type FonteToken struct {
	upstreamID int64
	nome       string
	base       oauth2.TokenSource
	molde      Concessao
	cofre      CofreOAuth
	relogio    Relogio
	log        *slog.Logger
	// aoRevogar é chamado quando o provedor recusa o refresh token
	// (invalid_grant). É o que faz o upstream cair em sem_consentimento em vez
	// de insistir com uma credencial morta.
	aoRevogar func()

	mu     sync.Mutex
	atual  *oauth2.Token
	morreu bool
}

// Compila-se como oauth2.TokenSource: é assim que ela entra no transporte.
var _ oauth2.TokenSource = (*FonteToken)(nil)

// novaFonteToken embrulha a source do SDK com serialização e persistência.
//
// molde carrega o que não muda entre refreshes (client_id efetivo, registro,
// token endpoint, estilo, escopos): a gravação de cada token novo reescreve a
// concessão inteira, e sem o molde ela apagaria justamente o que permite renovar
// depois de um reinício.
func novaFonteToken(
	upstreamID int64, nome string, base oauth2.TokenSource, molde Concessao,
	cofre CofreOAuth, relogio Relogio, log *slog.Logger, aoRevogar func(),
) *FonteToken {
	return &FonteToken{
		upstreamID: upstreamID,
		nome:       nome,
		base:       base,
		molde:      molde,
		cofre:      cofre,
		relogio:    relogio,
		log:        log,
		aoRevogar:  aoRevogar,
	}
}

// Token devolve o access token em vigor, renovando quando ele venceu.
//
// Sem contexto na assinatura porque é isso que a interface do x/oauth2 é. A
// gravação usa um contexto próprio com prazo curto: ela não pode herdar o da
// requisição que por acaso disparou o refresh, senão um cliente desistindo no
// meio deixaria o token novo em memória e o velho no banco.
//
// aoRevogar é chamado fora de f.mu, de propósito: ele chama de volta
// sessaoOAuth.marcarPrecisa, que pega s.mu, e s.mu.Preparar consulta f.Morreu()
// por conta própria. Chamar aoRevogar com f.mu preso seria a metade que falta de
// um ABBA — esta goroutine segurando f.mu e esperando s.mu, a de Preparar
// segurando s.mu e esperando f.mu.
func (f *FonteToken) Token() (*oauth2.Token, error) {
	f.mu.Lock()

	if f.morreu {
		f.mu.Unlock()
		// Depois de invalid_grant, insistir só produz mais uma recusa: o que
		// falta é consentimento, e quem o pede é a UI.
		//
		// O erro carrega um *oauth2.RetrieveError com invalid_grant junto com a
		// sentinela, e isso é deliberado: é a forma que o transporte do go-sdk
		// reconhece como "siga sem Authorization" (mcp/streamable.go:2397). Com
		// qualquer outro erro ele aborta a requisição, o 401 nunca chega, e o
		// fluxo de autorização que a UI acabou de pedir não seria disparado.
		return nil, fmt.Errorf("upstream %s: %w: %w", f.nome,
			ErrSemConsentimento, &oauth2.RetrieveError{ErrorCode: "invalid_grant"})
	}

	tok, err := f.base.Token()
	if err != nil {
		if f.refreshRecusado(err) {
			f.morreu = true
			f.esquecerConcessao()
			f.mu.Unlock()
			if f.aoRevogar != nil {
				f.aoRevogar()
			}
			// err aqui é seguro de embrulhar cru: refreshRecusado só devolve
			// verdadeiro com um *oauth2.RetrieveError de ErrorCode "invalid_grant"
			// preenchido, e com o código preenchido o x/oauth2 imprime só código,
			// descrição e URI (token.go:212) — nunca o corpo bruto da resposta, que
			// é onde um provedor que ecoa a requisição poria o refresh token
			// enviado.
			return nil, fmt.Errorf("upstream %s: refresh recusado pelo provedor: %w: %w",
				f.nome, ErrSemConsentimento, err)
		}
		f.mu.Unlock()
		// %s com mensagemDeFalha, e não %w com o erro cru: este ramo nunca é
		// invalid_grant (refreshRecusado já teria interceptado), e por isso pode
		// ser justamente o token endpoint respondendo algo que o x/oauth2 não
		// reconhece como erro OAuth — o caso em que RetrieveError imprime o corpo
		// bruto da resposta (token.go:212), que pode ecoar a credencial enviada.
		return nil, fmt.Errorf("upstream %s: obter token: %s", f.nome, mensagemDeFalha(err))
	}

	f.persistirSeNovo(tok)
	f.mu.Unlock()
	return tok, nil
}

// PertoDeExpirar informa se o token em vigor vence dentro de margem.
//
// Só olha o que já está em mão: não chama a source, porque a pergunta é
// justamente se vale a pena chamá-la.
func (f *FonteToken) PertoDeExpirar(margem time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.morreu {
		return false
	}
	if f.atual == nil {
		// Nada em mão ainda: pedir o token é o que descobre se há um.
		return true
	}
	return tokenDe(f.atual).PertoDeExpirar(f.relogio.Agora(), margem)
}

// Renovar é a renovação proativa que a supervisão dispara.
//
// Devolve nil quando não havia nada a fazer: o token está longe de expirar, ou a
// concessão já morreu e quem resolve é o admin.
func (f *FonteToken) Renovar(margem time.Duration) error {
	if !f.PertoDeExpirar(margem) {
		return nil
	}
	_, err := f.Token()
	return err
}

// Morreu informa se o provedor já recusou o refresh desta fonte.
func (f *FonteToken) Morreu() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.morreu
}

// persistirSeNovo grava a concessão quando o token mudou.
//
// A comparação é por access token e por prazo: reusar o mesmo token não precisa
// tocar o disco, e um refresh a cada requisição de saída faria o SQLite ver uma
// escrita por chamada de ferramenta.
func (f *FonteToken) persistirSeNovo(tok *oauth2.Token) {
	if f.atual != nil &&
		f.atual.AccessToken == tok.AccessToken &&
		f.atual.RefreshToken == tok.RefreshToken &&
		f.atual.Expiry.Equal(tok.Expiry) {
		return
	}
	primeiro := f.atual == nil
	f.atual = tok

	c := f.molde
	c.Token = tokenDe(tok)
	if !primeiro {
		c.RefreshEm = f.relogio.Agora()
	}

	ctx, cancelar := context.WithTimeout(context.Background(), timeoutGravarConcessao)
	defer cancelar()
	if err := f.cofre.GravarConcessao(ctx, f.upstreamID, c); err != nil {
		// Erro de gravação não invalida o token que já está em mão: a sessão
		// continua funcionando e o que se perde é a sobrevivência ao reinício.
		// Retornar aqui trocaria "reinício vai pedir consentimento de novo" por
		// "o upstream para agora", que é pior.
		f.log.Error("token de upstream renovado mas não gravado",
			"upstream", f.nome, "upstream_id", f.upstreamID, "erro", err)
		return
	}
	if !primeiro {
		// Sem valor nenhum do token no log, nem o prefixo: o campo é o instante.
		f.log.Info("token de upstream renovado",
			"upstream", f.nome, "upstream_id", f.upstreamID, "expira_em", tok.Expiry)
	}
}

func (f *FonteToken) esquecerConcessao() {
	ctx, cancelar := context.WithTimeout(context.Background(), timeoutGravarConcessao)
	defer cancelar()
	if err := f.cofre.ApagarConcessao(ctx, f.upstreamID); err != nil {
		f.log.Error("concessão revogada mas não apagada do banco",
			"upstream", f.nome, "upstream_id", f.upstreamID, "erro", err)
	}
}

// refreshRecusado distingue "o refresh não vai voltar a funcionar sozinho" de
// "a rede caiu".
//
// invalid_grant é a resposta que a RFC 6749 §5.2 reserva para refresh token
// expirado, revogado ou já usado. invalid_client e unauthorized_client entram
// pelo mesmo motivo, não pelo do token: são o provedor dizendo que o próprio
// cliente não vale mais — o registro dinâmico foi apagado do lado de lá, ou o
// client_secret de um cliente pré-registrado girou — e um cliente que não
// existe mais não autentica refresh nenhum, por mais válido que ele seja.
// Os três casos têm o mesmo desfecho: insistir não resolve, e o que falta é um
// humano autorizando de novo (o que, para DCR, é um cliente novo). Qualquer
// outro erro é transitório até prova em contrário, e tratá-lo como revogação
// faria uma queda de rede exigir reautorização manual.
func (f *FonteToken) refreshRecusado(err error) bool {
	var recusa *oauth2.RetrieveError
	if !errors.As(err, &recusa) {
		return false
	}
	switch recusa.ErrorCode {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return true
	default:
		return false
	}
}

// mensagemDeFalha é o texto de um erro de OAuth pronto para ir à tela e ao log.
//
// Ele existe por uma porta estreita e real: quando um token endpoint responde
// algo que o x/oauth2 não consegue interpretar como erro OAuth, o
// *oauth2.RetrieveError imprime o **corpo bruto da resposta** junto com o status
// (token.go:212). Um provedor que ecoa o que recebeu — e alguns ecoam, numa
// página de erro de proxy ou num dump de requisição — poria o refresh token
// enviado dentro da mensagem, e essa mensagem vai para ultimo_erro, para a tela
// do upstream e para o slog.
//
// Com error_code preenchido a mensagem já é só o código, a descrição e a URI do
// erro: nada disso é credencial, e os três ajudam a diagnosticar. É por isso que
// mora aqui e não em gerente.go: o que ela protege é especificamente a
// serialização de *oauth2.RetrieveError, o mesmo motivo de refreshRecusado.
func mensagemDeFalha(causa error) string {
	var recusa *oauth2.RetrieveError
	if !errors.As(causa, &recusa) || recusa.ErrorCode != "" {
		return causa.Error()
	}
	status := "sem resposta"
	if recusa.Response != nil {
		status = recusa.Response.Status
	}
	return fmt.Sprintf("o token endpoint respondeu %s, e a resposta não é um erro "+
		"OAuth reconhecível; o corpo dela fica de fora daqui porque pode ecoar a "+
		"credencial enviada", status)
}

// tokenDe traduz o token do x/oauth2 no que é persistido.
func tokenDe(tok *oauth2.Token) Token {
	return Token{
		Acesso:  cripto.Segredo(tok.AccessToken),
		Refresh: cripto.Segredo(tok.RefreshToken),
		Tipo:    tok.TokenType,
		Expira:  tok.Expiry,
	}
}

// tokenOAuth2De faz o caminho de volta, para reconstruir a source no boot.
func tokenOAuth2De(t Token) *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  t.Acesso.Revelar(),
		RefreshToken: t.Refresh.Revelar(),
		TokenType:    t.Tipo,
		Expiry:       t.Expira,
	}
}
