package upstream

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// relogioParado é um relógio de teste: Agora é fixo e Depois nunca dispara.
//
// Nunca disparar é o certo aqui — nenhum caso deste arquivo espera por tempo, e
// um timer que dispara sozinho transformaria falha de lógica em teste
// intermitente.
type relogioParado struct{ agora time.Time }

func (r relogioParado) Agora() time.Time { return r.agora }

func (relogioParado) Depois(time.Duration) <-chan time.Time { return nil }

// fonteContada é a oauth2.TokenSource de baixo: conta quantas vezes foi chamada
// e devolve um token novo a cada chamada.
//
// É o dublê que torna a serialização observável: se o mutex da FonteToken não
// existisse, N goroutines produziriam N chamadas aqui.
type fonteContada struct {
	chamadas atomic.Int32
	erro     error
	expira   time.Time
}

func (f *fonteContada) Token() (*oauth2.Token, error) {
	n := f.chamadas.Add(1)
	if f.erro != nil {
		return nil, f.erro
	}
	return &oauth2.Token{
		AccessToken:  "acesso-" + string(rune('a'+n-1)),
		RefreshToken: "refresh-" + string(rune('a'+n-1)),
		TokenType:    "Bearer",
		Expiry:       f.expira,
	}, nil
}

// cofreEspiao registra o que foi gravado e apagado, sem tocar em banco nenhum.
type cofreEspiao struct {
	mu       sync.Mutex
	gravadas []Concessao
	apagou   int
}

func (c *cofreEspiao) ClienteOAuth(context.Context, int64) (ClienteOAuth, error) {
	return ClienteOAuth{}, nil
}

func (c *cofreEspiao) Concessao(context.Context, int64) (Concessao, bool, error) {
	return Concessao{}, false, nil
}

func (c *cofreEspiao) GravarConcessao(_ context.Context, _ int64, g Concessao) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gravadas = append(c.gravadas, g)
	return nil
}

func (c *cofreEspiao) ApagarConcessao(context.Context, int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apagou++
	return nil
}

func (c *cofreEspiao) contagem() (gravadas, apagadas int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.gravadas), c.apagou
}

func (c *cofreEspiao) ultima() Concessao {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.gravadas) == 0 {
		return Concessao{}
	}
	return c.gravadas[len(c.gravadas)-1]
}

func fonteDeTeste(t *testing.T, base oauth2.TokenSource, cofre CofreOAuth, aoRevogar func()) *FonteToken {
	t.Helper()
	return novaFonteToken(7, "notion", base,
		Concessao{ClientIDEfetivo: "cli-1", Registro: RegistroDCR, URLToken: "https://as.invalido/token"},
		cofre, relogioParado{agora: time.Unix(1_700_000_000, 0)},
		slog.New(slog.DiscardHandler), aoRevogar)
}

// TestFonteToken_RefreshSerializado é o teste que a seção 13 do estudo pede.
//
// A base daqui (fonteContada) não cacheia nada sozinha — cada chamada devolve um
// token novo, de propósito — e por isso o que ele prova não é "uma única ida ao
// token endpoint" (quem cacheia isso é o oauth2.ReuseTokenSource, coberto por
// TestFonteToken_ReuseTokenSourceUmaSoIdaAoEndpoint logo abaixo), e sim que as
// gravações nunca se sobrepõem: com N goroutines batendo ao mesmo tempo, cada
// token novo é gravado exatamente uma vez e a ordem das gravações não se
// embaralha. Sem o mutex de FonteToken, duas gravações concorrentes podiam
// chegar ao banco fora de ordem e deixar gravado o token mais velho — o mesmo
// tipo de corrida que, do lado do provedor, dispara detecção de replay em quem
// gira a família de refresh token (issue #1760 do SDK de TypeScript).
func TestFonteToken_RefreshSerializado(t *testing.T) {
	t.Parallel()

	const goroutines = 64

	base := &fonteContada{expira: time.Unix(1_700_003_600, 0)}
	cofre := &cofreEspiao{}
	sut := fonteDeTeste(t, base, cofre, nil)

	// Todas as goroutines soltas de uma vez: sem a largada comum, elas se
	// enfileirariam sozinhas e o teste passaria mesmo sem o mutex.
	largada := make(chan struct{})
	var prontas, terminadas sync.WaitGroup
	prontas.Add(goroutines)
	terminadas.Add(goroutines)

	tokens := make([]string, goroutines)
	erros := make([]error, goroutines)
	for i := range goroutines {
		go func() {
			defer terminadas.Done()
			prontas.Done()
			<-largada
			tok, err := sut.Token()
			erros[i] = err
			if tok != nil {
				tokens[i] = tok.AccessToken
			}
		}()
	}
	prontas.Wait()
	close(largada)
	terminadas.Wait()

	for i := range goroutines {
		if erros[i] != nil {
			t.Fatalf("erro da goroutine %d = %v, quer nil", i, erros[i])
		}
		if tokens[i] == "" {
			t.Fatalf("token da goroutine %d vazio, quer o access token", i)
		}
	}

	// A prova da serialização: cada token novo foi gravado exatamente uma vez, e
	// na ordem. Com duas goroutines dentro do Token ao mesmo tempo, a gravação de
	// uma passaria por cima da outra e a contagem não bateria.
	gravadas, apagadas := cofre.contagem()
	if gravadas != goroutines {
		t.Errorf("gravações = %d, quer %d (uma por token novo, sem sobreposição)", gravadas, goroutines)
	}
	if apagadas != 0 {
		t.Errorf("apagamentos = %d, quer 0", apagadas)
	}
	if u := cofre.ultima(); u.ClientIDEfetivo != "cli-1" || u.Registro != RegistroDCR {
		t.Errorf("molde da concessão = %+v, quer client_id cli-1 e registro dcr", u)
	}
}

// TestFonteToken_ReuseTokenSourceUmaSoIdaAoEndpoint é o cenário de produção de
// verdade: conf.TokenSource, do x/oauth2, já embrulha a source num
// oauth2.ReuseTokenSource, que cacheia o token atrás do próprio mutex antes de a
// FonteToken entrar em cena.
//
// N chamadas concorrentes com o token de partida vencido produzem uma única ida
// ao token endpoint — quem serializa isso é o ReuseTokenSource, não a FonteToken
// — e uma única gravação: as chamadas que chegam depois da primeira leem o token
// já cacheado, e persistirSeNovo não regrava o que não mudou.
func TestFonteToken_ReuseTokenSourceUmaSoIdaAoEndpoint(t *testing.T) {
	t.Parallel()

	const goroutines = 64

	// oauth2.Token.Valid() confere contra o relógio de verdade — é a
	// biblioteca padrão, não o relogioParado que só a FonteToken enxerga — e por
	// isso os dois prazos aqui são relativos a time.Now(), e não ao "agora" fixo
	// que fonteDeTeste usa para o resto do arquivo.
	vencido := &oauth2.Token{AccessToken: "vencido", Expiry: time.Now().Add(-time.Hour)}
	contada := &fonteContada{expira: time.Now().Add(time.Hour)}
	base := oauth2.ReuseTokenSource(vencido, contada)
	cofre := &cofreEspiao{}
	sut := fonteDeTeste(t, base, cofre, nil)

	largada := make(chan struct{})
	var prontas, terminadas sync.WaitGroup
	prontas.Add(goroutines)
	terminadas.Add(goroutines)

	erros := make([]error, goroutines)
	for i := range goroutines {
		go func() {
			defer terminadas.Done()
			prontas.Done()
			<-largada
			_, erros[i] = sut.Token()
		}()
	}
	prontas.Wait()
	close(largada)
	terminadas.Wait()

	for i, err := range erros {
		if err != nil {
			t.Fatalf("erro da goroutine %d = %v, quer nil", i, err)
		}
	}

	if n := contada.chamadas.Load(); n != 1 {
		t.Errorf("idas ao token endpoint = %d, quer 1 (oauth2.ReuseTokenSource já cacheia)", n)
	}
	if gravadas, apagadas := cofre.contagem(); gravadas != 1 || apagadas != 0 {
		t.Errorf("gravações = %d, apagamentos = %d, quer 1 e 0 (token cacheado não é regravado)",
			gravadas, apagadas)
	}
}

// TestFonteToken_ReusaTokenValido prova que a fonte não grava duas vezes o mesmo
// token: a fonte de baixo devolvendo o mesmo valor não gera escrita nenhuma.
//
// Sem isso, o SQLite veria uma escrita por requisição de saída — o transporte
// pede o token a cada uma.
func TestFonteToken_ReusaTokenValido(t *testing.T) {
	t.Parallel()

	base := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: "acesso-fixo", TokenType: "Bearer", Expiry: time.Unix(1_700_003_600, 0),
	})
	cofre := &cofreEspiao{}
	sut := fonteDeTeste(t, base, cofre, nil)

	for range 5 {
		if _, err := sut.Token(); err != nil {
			t.Fatalf("erro = %v, quer nil", err)
		}
	}

	if gravadas, _ := cofre.contagem(); gravadas != 1 {
		t.Errorf("gravações = %d, quer 1 (só o primeiro token é novo)", gravadas)
	}
}

// TestFonteToken_RefreshRecusadoViraSemConsentimento cobre a revogação: o
// provedor devolve invalid_grant, a concessão é apagada, quem observa é avisado,
// e a fonte para de tentar.
//
// Parar de tentar é o ponto. Insistir contra um refresh token revogado é o laço
// que a fatia proíbe: o que falta é um humano autorizando de novo, e mais uma
// requisição não produz um.
func TestFonteToken_RefreshRecusadoViraSemConsentimento(t *testing.T) {
	t.Parallel()

	base := &fonteContada{erro: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}}
	cofre := &cofreEspiao{}
	avisos := 0
	sut := fonteDeTeste(t, base, cofre, func() { avisos++ })

	_, err := sut.Token()
	if !errors.Is(err, ErrSemConsentimento) {
		t.Fatalf("erro = %v, quer %v", err, ErrSemConsentimento)
	}
	if !sut.Morreu() {
		t.Error("fonte não morreu, quer morta depois de invalid_grant")
	}
	if avisos != 1 {
		t.Errorf("avisos de revogação = %d, quer 1", avisos)
	}
	if _, apagadas := cofre.contagem(); apagadas != 1 {
		t.Errorf("apagamentos = %d, quer 1", apagadas)
	}

	// Segunda chamada: nem chega à fonte de baixo.
	for range 10 {
		if _, err := sut.Token(); !errors.Is(err, ErrSemConsentimento) {
			t.Fatalf("erro = %v, quer %v", err, ErrSemConsentimento)
		}
	}
	if n := base.chamadas.Load(); n != 1 {
		t.Errorf("idas ao token endpoint = %d, quer 1 (sem laço depois da revogação)", n)
	}
	if avisos != 1 {
		t.Errorf("avisos de revogação = %d, quer 1 (o aviso não se repete)", avisos)
	}
}

// TestFonteToken_ErroTransitorioNaoMataAConcessao separa "a rede caiu" de "o
// provedor não aceita mais este refresh token".
//
// Tratar os dois igual faria uma queda de rede exigir reautorização manual de
// todo upstream, que é o oposto do que a fatia entrega.
func TestFonteToken_ErroTransitorioNaoMataAConcessao(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		erro       error
		querMorreu bool
	}{
		"rede indisponível": {
			erro: errors.New("dial tcp: connection refused"),
		},
		"provedor com erro interno": {
			erro: &oauth2.RetrieveError{ErrorCode: "server_error"},
		},
		"refresh token revogado": {
			erro: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, querMorreu: true,
		},
		"cliente não existe mais no provedor": {
			erro: &oauth2.RetrieveError{ErrorCode: "invalid_client"}, querMorreu: true,
		},
		"cliente sem autorização para este grant": {
			erro: &oauth2.RetrieveError{ErrorCode: "unauthorized_client"}, querMorreu: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			cofre := &cofreEspiao{}
			sut := fonteDeTeste(t, &fonteContada{erro: tc.erro}, cofre, nil)

			_, err := sut.Token()
			if err == nil {
				t.Fatal("erro = nil, quer erro")
			}
			if got := errors.Is(err, ErrSemConsentimento); got != tc.querMorreu {
				t.Errorf("é sem consentimento = %v, quer %v (erro = %v)", got, tc.querMorreu, err)
			}
			if got := sut.Morreu(); got != tc.querMorreu {
				t.Errorf("morreu = %v, quer %v", got, tc.querMorreu)
			}
		})
	}
}

// TestFonteToken_PertoDeExpirar é o gatilho da renovação proativa, com o relógio
// injetado.
func TestFonteToken_PertoDeExpirar(t *testing.T) {
	t.Parallel()

	agora := time.Unix(1_700_000_000, 0)

	casos := map[string]struct {
		expira time.Time
		margem time.Duration
		quer   bool
	}{
		"token folgado":                    {expira: agora.Add(time.Hour), margem: time.Minute},
		"token dentro da margem":           {expira: agora.Add(30 * time.Second), margem: time.Minute, quer: true},
		"token vencido":                    {expira: agora.Add(-time.Second), margem: time.Minute, quer: true},
		"token sem prazo nunca vence":      {expira: time.Time{}, margem: time.Hour},
		"exatamente na borda já renova":    {expira: agora.Add(time.Minute), margem: time.Minute, quer: true},
		"margem zero só pega o já vencido": {expira: agora.Add(time.Second), margem: 0},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			base := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "a", Expiry: tc.expira})
			sut := novaFonteToken(1, "up", base, Concessao{}, &cofreEspiao{},
				relogioParado{agora: agora}, slog.New(slog.DiscardHandler), nil)
			if _, err := sut.Token(); err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}

			if got := sut.PertoDeExpirar(tc.margem); got != tc.quer {
				t.Errorf("perto de expirar = %v, quer %v", got, tc.quer)
			}
		})
	}
}
