package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/vitoramaral10/patchbay/internal/platform/versao"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// BrokerOAuth é o dono do OAuth de upstream.
//
// Uma instância de auth.AuthorizationCodeHandler e uma de FonteToken por
// upstream, para toda a vida do processo (decisão 12 do estudo). Elas nascem na
// primeira tentativa de conexão e só morrem quando o upstream é reconfigurado ou
// removido — reconfigurar precisa matá-las porque o client_id que o admin
// acabou de trocar não pode continuar valendo numa fonte já construída.
//
// A descoberta (RFC 9728 e RFC 8414), o PKCE, a ordem de registro de cliente
// (CIMD → pré-registrado → DCR) e o refresh via x/oauth2 são todos do go-sdk.
// O que este pacote acrescenta é o que o SDK não pode ter: o fluxo de
// consentimento pelo navegador de um admin que está em outra máquina, e a
// persistência cifrada do que ele produz.
type BrokerOAuth struct {
	cofre      CofreOAuth
	urlPublica string
	cliente    *http.Client
	relogio    Relogio
	log        *slog.Logger

	tempoConsentimento time.Duration
	esperaURL          time.Duration

	mu      sync.Mutex
	sessoes map[int64]*sessaoOAuth
	// porState é o registro de state de uso único: o callback só encontra a
	// tentativa que o emitiu, e uma vez.
	//
	// Em memória e não em tabela, e a diferença é deliberada. O code_verifier do
	// PKCE que emparelha com este state vive dentro do handler do SDK e não é
	// exposto; persistir o state sem ele daria uma linha que não serve para
	// nada, e persistir os dois exigiria reimplementar a troca do code por token
	// por fora da biblioteca. Como o fetcher do SDK é um callback que espera,
	// state e verifier têm exatamente o mesmo tempo de vida: a chamada.
	porState map[string]*pedidoConsentimento
}

// OpcaoOAuth ajusta o broker na construção.
type OpcaoOAuth func(*BrokerOAuth)

// ComRelogioOAuth troca o relógio do broker. O teste injeta o seu para não
// depender de tempo de verdade.
func ComRelogioOAuth(r Relogio) OpcaoOAuth {
	return func(b *BrokerOAuth) {
		if r != nil {
			b.relogio = r
		}
	}
}

// ComClienteOAuth troca o cliente HTTP das requisições de OAuth: metadata,
// registro dinâmico, troca do code e refresh.
func ComClienteOAuth(c *http.Client) OpcaoOAuth {
	return func(b *BrokerOAuth) {
		if c != nil {
			b.cliente = c
		}
	}
}

// ComTempoDeConsentimento troca quanto a tentativa espera o admin concluir o
// consentimento no provedor.
func ComTempoDeConsentimento(d time.Duration) OpcaoOAuth {
	return func(b *BrokerOAuth) {
		if d > 0 {
			b.tempoConsentimento = d
		}
	}
}

// ComEsperaDeURL troca quanto o clique em "Autorizar" espera a URL do provedor
// ficar pronta.
func ComEsperaDeURL(d time.Duration) OpcaoOAuth {
	return func(b *BrokerOAuth) {
		if d > 0 {
			b.esperaURL = d
		}
	}
}

// NovoBrokerOAuth monta o broker.
//
// urlPublica é o que o patchbay sabe de si mesmo: o TLS é do proxy reverso na
// frente dele, então nem o redirect_uri nem a URL do Client ID Metadata Document
// podem ser derivados do endereço de escuta ou do r.TLS de uma requisição.
func NovoBrokerOAuth(cofre CofreOAuth, urlPublica string, log *slog.Logger, opcoes ...OpcaoOAuth) *BrokerOAuth {
	b := &BrokerOAuth{
		cofre:              cofre,
		urlPublica:         strings.TrimRight(urlPublica, "/"),
		cliente:            &http.Client{},
		relogio:            relogioReal{},
		log:                log,
		tempoConsentimento: TempoDeConsentimentoPadrao,
		esperaURL:          EsperaURLAutorizacaoPadrao,
		sessoes:            make(map[int64]*sessaoOAuth),
		porState:           make(map[string]*pedidoConsentimento),
	}
	for _, o := range opcoes {
		o(b)
	}
	return b
}

// ComOAuth liga o gerente ao broker de OAuth de upstream.
//
// Sem ele, um upstream com modo_credencial = oauth não sai de degradado: não há
// quem monte o token, e falhar dizendo isso é melhor que conectar sem
// autorização e receber 401 sem explicação.
func ComOAuth(b *BrokerOAuth) Opcao {
	return func(g *Gerente) { g.oauth = b }
}

// RedirectURI é o redirect_uri que o patchbay registra no provedor.
//
// É contrato: o valor gravado no cadastro do cliente OAuth do provedor tem que
// ser byte a byte este, e mudar a URL pública depois invalida todo consentimento
// existente.
func (b *BrokerOAuth) RedirectURI() string {
	return b.urlPublica + webui.RotaCallbackOAuthUpstream
}

// URLMetadataCliente é a URL do Client ID Metadata Document, que no CIMD é o
// próprio client_id.
//
// Vazia quando a URL pública não é HTTPS: o SDK exige HTTPS não-raiz
// (auth/authorization_code.go:231), e com razão — um client_id que é uma URL só
// vale como identidade se ninguém no caminho puder trocar o documento. Em
// desenvolvimento sobre http isso significa que a ordem cai direto em
// pré-registrado ou DCR, que é o comportamento certo e não um degrade
// silencioso: ele aparece no log do boot.
func (b *BrokerOAuth) URLMetadataCliente() string {
	if !strings.HasPrefix(b.urlPublica, "https://") {
		return ""
	}
	return b.urlPublica + webui.RotaMetadataClienteUpstream
}

// pedidoConsentimento é uma tentativa de consentimento em curso.
//
// Nasce no clique em "Autorizar" e morre quando o callback chega, quando o prazo
// vence, ou quando um clique novo a substitui. Uso único em todos os casos.
type pedidoConsentimento struct {
	upstreamID int64
	// criadoEm é quando o clique em "Autorizar" abriu este pedido. É o que
	// pendenteValido confere contra b.tempoConsentimento: sem prazo próprio, um
	// pedido cujo callback nunca chega — upstream fora do ar no momento do
	// clique, por exemplo — ficaria pendurado para sempre, e ConsentimentoPedido
	// continuaria estendendo o watchdog e Preparar continuaria deixando passar
	// uma tentativa que nunca completa.
	criadoEm time.Time
	// state é preenchido pelo fetcher, que é quem vê a URL montada pelo SDK.
	state string
	// urlPronta leva a URL de autorização para a requisição do admin que está
	// esperando por ela. Bufferizado: quem publica nunca pode bloquear.
	urlPronta chan string
	// resposta leva o que o callback recebeu do provedor.
	resposta chan respostaConsentimento
	// cancelado é fechado quando um clique novo substitui este pedido.
	cancelado chan struct{}
}

type respostaConsentimento struct {
	codigo string
	iss    string
	// erroAS é o parâmetro error do callback (access_denied, etc.). Não é
	// segredo e é o que a tela precisa dizer ao admin.
	erroAS string
}

// sessaoOAuth é o OAuth de um upstream: um handler, uma fonte de token, um
// pedido de consentimento por vez.
//
// Invariante de lock: f.mu (de FonteToken) nunca é tomado com s.mu preso. O
// caminho contrário — s.mu tomado com f.mu preso — não existe neste arquivo, e
// é isso que evita a ABBA entre esta sessão e a fonte de token dela: Preparar
// só consulta fonte.Morreu() depois de soltar s.mu, e aoRevogar (que pega
// s.mu por dentro de marcarPrecisa) só é chamado por FonteToken.Token() depois
// de soltar f.mu.
type sessaoOAuth struct {
	broker *BrokerOAuth
	id     int64
	nome   string

	// pedidos acorda a supervisão quando o admin pede autorização. Bufferizado
	// em 1: dois cliques seguidos não precisam de duas voltas.
	pedidos chan struct{}
	// parar é fechado quando a sessão é descartada, para soltar um fetcher que
	// esteja esperando pelo consentimento.
	parar chan struct{}

	mu       sync.Mutex
	handler  *auth.AuthorizationCodeHandler
	fonte    *FonteToken
	cliente  ClienteOAuth
	urlCIMD  string
	pendente *pedidoConsentimento
	// precisa é verdadeiro quando o fetcher já disse que falta consentimento. É
	// o que faz o gerente escolher sem_consentimento em vez de degradado sem
	// depender de o erro sobreviver a todas as camadas de embrulho do SDK.
	precisa bool
}

// sessao devolve a sessão de OAuth de um upstream, criando o slot se preciso.
func (b *BrokerOAuth) sessao(cfg Config) *sessaoOAuth {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessoes[cfg.ID]; ok {
		return s
	}
	s := &sessaoOAuth{
		broker:  b,
		id:      cfg.ID,
		nome:    cfg.Nome,
		pedidos: make(chan struct{}, 1),
		parar:   make(chan struct{}),
	}
	b.sessoes[cfg.ID] = s
	return s
}

// Esquecer descarta a sessão de um upstream: handler, fonte de token e pedido em
// curso.
//
// É o que o gerente chama ao reconfigurar ou remover. Sem isto, trocar o
// client_id pela tela não teria efeito nenhum até o próximo boot — a fonte
// construída com o valor antigo continuaria servindo.
func (b *BrokerOAuth) Esquecer(id int64) {
	b.mu.Lock()
	s, ok := b.sessoes[id]
	if ok {
		delete(b.sessoes, id)
		for state, p := range b.porState {
			if p.upstreamID == id {
				delete(b.porState, state)
			}
		}
	}
	b.mu.Unlock()
	if ok {
		close(s.parar)
	}
}

// Pedidos é o canal em que a supervisão espera por um pedido de autorização.
//
// Nulo para upstream que não usa OAuth, e um canal nulo num select bloqueia para
// sempre — que é exatamente o comportamento desejado: aquele braço do select
// simplesmente não existe.
func (b *BrokerOAuth) Pedidos(cfg Config) <-chan struct{} {
	if cfg.Modo != ModoOAuth {
		return nil
	}
	return b.sessao(cfg).pedidos
}

// PrecisaConsentimento informa se o upstream está esperando um clique em
// "Autorizar".
func (b *BrokerOAuth) PrecisaConsentimento(id int64) bool {
	b.mu.Lock()
	s, ok := b.sessoes[id]
	b.mu.Unlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.precisa
}

// ConsentimentoPedido informa se há uma tentativa de consentimento aguardando o
// navegador do admin.
//
// O gerente usa isto para estender o prazo do watchdog: a tentativa em curso
// inclui esperar uma pessoa clicar em "permitir" no provedor, e cortá-la no
// timeout de operação transformaria todo consentimento num connect abandonado —
// cinco deles desabilitariam o upstream sozinho.
//
// Um pedido vencido — o prazo de consentimento passou sem o callback chegar —
// conta como se não existisse: descartá-lo aqui é o que impede o watchdog de
// ficar estendido para sempre por uma tentativa que já morreu.
func (b *BrokerOAuth) ConsentimentoPedido(id int64) bool {
	b.mu.Lock()
	s, ok := b.sessoes[id]
	b.mu.Unlock()
	if !ok {
		return false
	}
	return s.consentimentoEmCurso()
}

// Autorizacao devolve o handler de OAuth daquele upstream, construindo-o na
// primeira vez.
//
// Roda na goroutine de supervisão, antes de abrir a sessão: é I/O de banco e de
// rede, e é por isso que não pode acontecer no caminho da requisição do cliente.
func (b *BrokerOAuth) Autorizacao(ctx context.Context, cfg Config) (auth.OAuthHandler, error) {
	s := b.sessao(cfg)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handler != nil {
		return s.handler, nil
	}

	cliente, err := b.cofre.ClienteOAuth(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: ler cliente OAuth: %w", cfg.Nome, err)
	}
	concessao, temConcessao, err := b.cofre.Concessao(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: ler concessão OAuth: %w", cfg.Nome, err)
	}

	s.cliente = cliente
	s.urlCIMD = b.URLMetadataCliente()

	conf := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              b.RedirectURI(),
		AuthorizationCodeFetcher: s.buscarCodigo,
		// O patchbay guarda o refresh token cifrado em repouso, então pedir
		// offline_access é honesto — e sem ele todo vencimento de access token
		// viraria um clique em "Autorizar".
		RequestRefreshToken: true,
		Client:              b.cliente,
		NewTokenSource:      s.novaFonte,
	}
	if s.urlCIMD != "" {
		conf.ClientIDMetadataDocumentConfig = &auth.ClientIDMetadataDocumentConfig{URL: s.urlCIMD}
	}
	switch {
	case cliente.Definido():
		pre := &oauthex.ClientCredentials{ClientID: cliente.ClientID, Issuer: cliente.Issuer}
		if !cliente.Segredo.Vazio() {
			pre.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: cliente.Segredo.Revelar()}
		}
		conf.PreregisteredClient = pre
	case concessao.Registro == RegistroDCR && concessao.ClientIDEfetivo != "":
		// O client_id que o registro dinâmico emitiu vale de novo: é isso que
		// "persistir o client_id obtido por DCR" compra. Sem reusá-lo, cada
		// reautorização registraria mais um cliente no provedor, e o cadastro de
		// lá viraria uma lista de clientes órfãos que ninguém sabe apagar.
		//
		// Entra como pré-registrado porque é o que ele é agora: um cliente que já
		// existe no provedor. O registro continua contando como dcr, porque foi
		// de lá que ele veio — registroDe compara com o que o admin informou, e o
		// admin não informou nada.
		conf.PreregisteredClient = &oauthex.ClientCredentials{ClientID: concessao.ClientIDEfetivo}
	}
	conf.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
		Metadata: b.metadataDeRegistro(),
	}

	if temConcessao && concessao.Token.Valido() && concessao.URLToken != "" {
		//nolint:contextcheck // a fonte de token vive além desta chamada: o
		// contexto dela é de fundo de propósito, e amarrá-lo ao ctx daqui faria
		// todo refresh posterior falhar com "context canceled".
		s.fonte = b.fonteDaConcessao(s, cliente, concessao)
		conf.InitialTokenSource = s.fonte
	}

	h, err := auth.NewAuthorizationCodeHandler(conf)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: montar cliente OAuth: %w", cfg.Nome, err)
	}
	s.handler = h
	return h, nil
}

// Preparar garante que existe autorização utilizável antes de a supervisão
// gastar uma tentativa de conexão.
//
// É o portão que elimina o laço. Sem ele, um upstream sem consentimento
// reconectaria a cada volta do backoff só para tomar 401 — e no caminho do
// registro dinâmico cada uma dessas voltas registraria um cliente novo no
// provedor, acumulando lá dezenas de registros que nunca serão usados.
//
// Passa em dois casos: quando o admin acabou de pedir autorização (e aí a
// tentativa existe justamente para completar o fluxo), e quando há fonte de token
// viva. Fonte morta — refresh recusado com invalid_grant — não passa: o que falta
// é um humano, e mais uma requisição não produz um.
//
// fonte.Morreu() é consultado com s.mu solto: ela pega f.mu por dentro, e
// FonteToken.Token() pode estar no meio de um refresh, segurando f.mu, prestes a
// chamar aoRevogar — que pega s.mu de volta. Segurar as duas ao mesmo tempo aqui
// seria a metade que falta para um ABBA: esta goroutine esperando f.mu com s.mu
// preso, a do refresh esperando s.mu com f.mu preso.
func (b *BrokerOAuth) Preparar(ctx context.Context, cfg Config) error {
	s := b.sessao(cfg)
	if _, err := b.Autorizacao(ctx, cfg); err != nil {
		return err
	}

	if s.consentimentoEmCurso() {
		return nil
	}

	s.mu.Lock()
	fonte := s.fonte
	s.mu.Unlock()
	if fonte != nil && !fonte.Morreu() {
		return nil
	}

	s.mu.Lock()
	s.precisa = true
	s.mu.Unlock()
	return fmt.Errorf("upstream %s: %w", cfg.Nome, ErrSemConsentimento)
}

// fonteDaConcessao reconstrói a fonte de token a partir do que está gravado.
//
// É o que faz um reinício não pedir consentimento de novo: com o token endpoint,
// o estilo de autenticação e os escopos persistidos, o refresh token gravado
// volta a ser utilizável sem nenhuma descoberta — e portanto sem depender de um
// 401 do upstream, que é o que jogaria o refresh para o caminho da requisição.
func (b *BrokerOAuth) fonteDaConcessao(s *sessaoOAuth, cliente ClienteOAuth, c Concessao) *FonteToken {
	conf := &oauth2.Config{
		ClientID: c.ClientIDEfetivo,
		Endpoint: oauth2.Endpoint{
			TokenURL:  c.URLToken,
			AuthStyle: oauth2.AuthStyle(c.Estilo),
		},
		RedirectURL: b.RedirectURI(),
		Scopes:      c.Escopos,
	}
	if c.Registro == RegistroPreRegistrado && !cliente.Segredo.Vazio() {
		conf.ClientSecret = cliente.Segredo.Revelar()
	}

	// Contexto de fundo, e não o da requisição que por acaso disparou a
	// construção: o x/oauth2 guarda o contexto que recebe e o reusa em todo
	// refresh seguinte, então amarrá-lo a uma requisição faria todo refresh
	// posterior falhar com "context canceled".
	ctxFundo := context.WithValue(context.Background(), oauth2.HTTPClient, b.cliente)
	inicial := tokenOAuth2De(c.Token)

	molde := c
	molde.Token = Token{}
	f := novaFonteToken(s.id, s.nome, conf.TokenSource(ctxFundo, inicial), molde,
		b.cofre, b.relogio, b.log, func() { s.marcarPrecisa() })
	// O token gravado já é o token em mão: sem isto a primeira chamada gravaria
	// de novo o que acabou de ser lido.
	f.atual = inicial
	return f
}

// metadataDeRegistro é o que o patchbay declara de si, tanto no DCR quanto no
// Client ID Metadata Document.
//
// token_endpoint_auth_method "none": o patchbay é cliente público neste fluxo,
// porque a segurança dele vem do PKCE e do redirect_uri exato, não de um segredo
// que um AS qualquer teria emitido. Cliente confidencial acontece só no caminho
// pré-registrado, em que o segredo é o que o admin recebeu do provedor.
func (b *BrokerOAuth) metadataDeRegistro() *oauthex.ClientRegistrationMetadata {
	return &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            []string{b.RedirectURI()},
		ClientName:              "patchbay",
		ClientURI:               b.urlPublica,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		SoftwareID:              "patchbay",
		SoftwareVersion:         versao.Numero,
	}
}

// Pedir inicia um consentimento e devolve a URL do provedor para onde o
// navegador do admin deve ir.
//
// Ela não é montada aqui: quem a monta é o SDK, dentro do Authorize, depois da
// descoberta e do registro de cliente. Este método registra o pedido, acorda a
// supervisão e espera a URL aparecer — é a ponte entre "o consentimento é dado
// pelo navegador do admin" e "o fluxo roda no processo do servidor".
func (b *BrokerOAuth) Pedir(ctx context.Context, cfg Config) (string, error) {
	if cfg.Modo != ModoOAuth {
		return "", fmt.Errorf("%w: %s", ErrNaoEhOAuth, cfg.Nome)
	}
	s := b.sessao(cfg)

	p := &pedidoConsentimento{
		upstreamID: cfg.ID,
		criadoEm:   b.relogio.Agora(),
		urlPronta:  make(chan string, 1),
		resposta:   make(chan respostaConsentimento, 1),
		cancelado:  make(chan struct{}),
	}

	s.mu.Lock()
	antigo := s.pendente
	s.pendente = p
	s.precisa = false
	s.mu.Unlock()
	if antigo != nil {
		// O clique novo manda: soltar o fetcher antigo evita que ele fique
		// segurando a tentativa até o prazo de consentimento vencer.
		close(antigo.cancelado)
	}

	s.acordar()

	select {
	case u := <-p.urlPronta:
		return u, nil
	case <-ctx.Done():
		s.limparPendente(p)
		return "", fmt.Errorf("upstream %s: pedir autorização: %w", cfg.Nome, ctx.Err())
	case <-b.relogio.Depois(b.esperaURL):
		// O pedido fica de pé: a supervisão pode estar terminando a tentativa
		// anterior, e o próximo clique aproveita a URL que aparecer.
		return "", fmt.Errorf("upstream %s: %w", cfg.Nome, ErrConsentimentoDemorou)
	}
}

// Entregar é o callback: leva o que o provedor devolveu para a tentativa que o
// emitiu.
//
// Devolve o upstream a que o state pertencia, para a UI saber para onde
// redirecionar. State desconhecido é recusado sem dizer mais nada — é a
// conferência de CSRF do fluxo, e um state que não está no registro é ou uma
// tentativa já consumida, ou uma requisição que ninguém pediu.
func (b *BrokerOAuth) Entregar(state, codigo, iss, erroAS string) (int64, error) {
	if state == "" {
		return 0, ErrConsentimentoDesconhecido
	}
	b.mu.Lock()
	p, ok := b.porState[state]
	if ok {
		delete(b.porState, state)
	}
	b.mu.Unlock()
	if !ok {
		return 0, ErrConsentimentoDesconhecido
	}

	select {
	case p.resposta <- respostaConsentimento{codigo: codigo, iss: iss, erroAS: erroAS}:
	default:
		// Ninguém esperando: a tentativa já desistiu pelo prazo.
		return p.upstreamID, ErrConsentimentoDesconhecido
	}
	if erroAS != "" {
		return p.upstreamID, fmt.Errorf("%w: %s", ErrConsentimentoRecusado, erroAS)
	}
	return p.upstreamID, nil
}

// Renovar é a renovação proativa que a supervisão dispara.
func (b *BrokerOAuth) Renovar(id int64, margem time.Duration) error {
	b.mu.Lock()
	s, ok := b.sessoes[id]
	b.mu.Unlock()
	if !ok {
		return nil
	}
	s.mu.Lock()
	f := s.fonte
	s.mu.Unlock()
	if f == nil {
		return nil
	}
	return f.Renovar(margem)
}

// Fonte devolve a fonte de token de um upstream, ou nulo. Existe para o teste
// poder afirmar que ela é a mesma instância entre reconexões.
func (b *BrokerOAuth) Fonte(id int64) *FonteToken {
	b.mu.Lock()
	s, ok := b.sessoes[id]
	b.mu.Unlock()
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fonte
}

// --- o lado da sessão -------------------------------------------------------

func (s *sessaoOAuth) acordar() {
	select {
	case s.pedidos <- struct{}{}:
	default:
	}
}

func (s *sessaoOAuth) marcarPrecisa() {
	s.mu.Lock()
	s.precisa = true
	s.mu.Unlock()
}

func (s *sessaoOAuth) limparPendente(p *pedidoConsentimento) {
	s.mu.Lock()
	if s.pendente == p {
		s.pendente = nil
	}
	// state é escrito por buscarCodigo sob s.mu, e por isso só pode ser lido
	// aqui sob o mesmo lock: as duas goroutines rodam em paralelo — esta pelo
	// ctx.Done() de Pedir, aquela pela supervisão — e sem o lock a leitura seria
	// uma corrida de dado sobre o mesmo campo.
	state := p.state
	s.mu.Unlock()
	if state != "" {
		s.broker.mu.Lock()
		if atual, ok := s.broker.porState[state]; ok && atual == p {
			delete(s.broker.porState, state)
		}
		s.broker.mu.Unlock()
	}
}

// consentimentoEmCurso informa se há um pedido de consentimento vivo,
// descartando primeiro o que passou do prazo — e limpando o registro de state
// junto, para que um callback atrasado não encontre um state que já devia ter
// sumido.
func (s *sessaoOAuth) consentimentoEmCurso() bool {
	s.mu.Lock()
	p := s.pendente
	if p == nil {
		s.mu.Unlock()
		return false
	}
	if s.broker.relogio.Agora().Sub(p.criadoEm) <= s.broker.tempoConsentimento {
		s.mu.Unlock()
		return true
	}
	s.pendente = nil
	s.precisa = true
	state := p.state
	s.mu.Unlock()

	if state != "" {
		s.broker.mu.Lock()
		if atual, ok := s.broker.porState[state]; ok && atual == p {
			delete(s.broker.porState, state)
		}
		s.broker.mu.Unlock()
	}
	return false
}

// buscarCodigo é o auth.AuthorizationCodeFetcher do patchbay.
//
// O SDK espera que ela leve o admin à URL de autorização e devolva o code. Aqui
// o admin está noutra máquina, então ela publica a URL para a requisição que
// clicou em "Autorizar" e espera o callback HTTP chegar. É por isso que ela
// bloqueia, e é por isso que ela tem que bloquear: o code_verifier do PKCE e o
// state desta tentativa vivem dentro do handler do SDK, e um code obtido com o
// challenge de uma tentativa não pode ser trocado na seguinte.
//
// Sem pedido do admin em curso ela recusa na hora, com ErrSemConsentimento. É o
// que impede o laço: um upstream com token revogado não fica reconectando de
// cinco em cinco minutos para tomar 401 — ele para em sem_consentimento e espera
// o clique, porque o que falta é uma pessoa, não uma tentativa.
func (s *sessaoOAuth) buscarCodigo(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	s.mu.Lock()
	p := s.pendente
	if p == nil {
		s.precisa = true
		s.mu.Unlock()
		s.broker.log.Info("upstream aguardando consentimento OAuth",
			"upstream", s.nome, "upstream_id", s.id)
		return nil, fmt.Errorf("upstream %s: %w", s.nome, ErrSemConsentimento)
	}
	s.mu.Unlock()

	state := stateDaURL(args.URL)
	if state == "" {
		return nil, fmt.Errorf("upstream %s: URL de autorização sem state", s.nome)
	}

	s.mu.Lock()
	p.state = state
	s.mu.Unlock()
	s.broker.mu.Lock()
	s.broker.porState[state] = p
	s.broker.mu.Unlock()
	defer s.limparPendente(p)

	select {
	case p.urlPronta <- args.URL:
	default:
	}

	select {
	case r := <-p.resposta:
		if r.erroAS != "" {
			s.marcarPrecisa()
			return nil, fmt.Errorf("upstream %s: %w: %s", s.nome, ErrConsentimentoRecusado, r.erroAS)
		}
		// State devolvido é o mesmo que o SDK gerou: quem o conferiu foi o
		// registro de uso único acima, e devolvê-lo daqui é o que faz a
		// conferência do próprio SDK passar sem inventar um segundo state.
		return &auth.AuthorizationResult{Code: r.codigo, State: state, Iss: r.iss}, nil
	case <-p.cancelado:
		s.marcarPrecisa()
		return nil, fmt.Errorf("upstream %s: %w: pedido substituído", s.nome, ErrSemConsentimento)
	case <-s.parar:
		return nil, fmt.Errorf("upstream %s: %w: upstream reconfigurado", s.nome, ErrSemConsentimento)
	case <-s.broker.relogio.Depois(s.broker.tempoConsentimento):
		s.marcarPrecisa()
		return nil, fmt.Errorf("upstream %s: %w: consentimento não concluído no prazo",
			s.nome, ErrSemConsentimento)
	case <-ctx.Done():
		s.marcarPrecisa()
		return nil, fmt.Errorf("upstream %s: %w: %w", s.nome, ErrSemConsentimento, ctx.Err())
	}
}

// novaFonte é o hook NewTokenSource do SDK, e é o ponto de persistência correto.
//
// Ele recebe o *oauth2.Config já resolvido — com o client_id que a ordem CIMD →
// pré-registrado → DCR escolheu, o token endpoint descoberto, o estilo de
// autenticação que o AS anunciou e os escopos concedidos — e o *oauth2.Token
// mesclado pela biblioteca. É tudo o que precisa ser gravado, e nada disso vem
// do corpo da resposta HTTP: parsear o token endpoint por conta própria é a
// única forma de reintroduzir o bug de sobrescrever refresh_token vazio que o
// x/oauth2 já não tem.
func (s *sessaoOAuth) novaFonte(ctx context.Context, conf *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	molde := Concessao{
		ClientIDEfetivo: conf.ClientID,
		Registro:        s.registroDe(conf.ClientID),
		URLToken:        conf.Endpoint.TokenURL,
		Estilo:          int(conf.Endpoint.AuthStyle),
		Escopos:         conf.Scopes,
	}

	f := novaFonteToken(s.id, s.nome, conf.TokenSource(ctx, tok), molde,
		s.broker.cofre, s.broker.relogio, s.broker.log, func() { s.marcarPrecisa() })

	// Grava agora, e não na primeira chamada de Token: o consentimento acabou de
	// acontecer e é o único momento em que o refresh token pode chegar. Perdê-lo
	// aqui custaria um clique novo do admin.
	f.mu.Lock()
	//nolint:contextcheck // a gravação da concessão tem prazo próprio: herdar o
	// ctx do consentimento faria o refresh token — a única coisa irrecuperável
	// aqui — se perder se a requisição do admin terminasse primeiro.
	f.persistirSeNovo(tok)
	f.mu.Unlock()

	s.mu.Lock()
	s.fonte = f
	s.precisa = false
	s.mu.Unlock()

	s.broker.log.Info("consentimento OAuth de upstream concluído",
		"upstream", s.nome, "upstream_id", s.id,
		"registro", molde.Registro, "client_id", molde.ClientIDEfetivo,
		"escopos", strings.Join(molde.Escopos, " "))
	return f, nil
}

// registroDe diz por qual dos três caminhos o client_id em uso veio.
//
// Por comparação e não por um campo do SDK, porque o SDK não devolve essa
// informação: os três candidatos são conhecidos aqui e são distintos entre si —
// a URL do CIMD, o client_id que o admin colou, e qualquer outro valor, que só
// pode ter saído do registration_endpoint. É assim que o client_id emitido por
// DCR fica gravado sem que nada precise ser inferido depois.
func (s *sessaoOAuth) registroDe(clientID string) string {
	switch {
	case s.urlCIMD != "" && clientID == s.urlCIMD:
		return RegistroCIMD
	case s.cliente.ClientID != "" && clientID == s.cliente.ClientID:
		return RegistroPreRegistrado
	default:
		return RegistroDCR
	}
}

// stateDaURL extrai o state da URL de autorização montada pelo SDK.
//
// Ler o state de lá em vez de sortear um próprio é o que mantém uma verdade só:
// o SDK confere o state que o fetcher devolve contra o que ele gerou, e um
// segundo state significaria duas conferências que podem discordar.
func stateDaURL(bruta string) string {
	u, err := url.Parse(bruta)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}

// erroDeConsentimento traduz o que veio do broker no que a tela mostra.
func erroDeConsentimento(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrConsentimentoDesconhecido):
		return "consentimento_invalido"
	case errors.Is(err, ErrConsentimentoRecusado):
		return "consentimento_recusado"
	case errors.Is(err, ErrConsentimentoDemorou):
		return "consentimento_demorou"
	default:
		return "consentimento_falhou"
	}
}
