package authsrv

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Repositorio é o que o Servico precisa da persistência, declarado aqui no
// consumidor e implementado por RepositorioSQLite.
type Repositorio interface {
	// ClientePorClientID devolve o cliente ativo. ErrClienteNaoEncontrado
	// quando não existe ou foi revogado.
	ClientePorClientID(ctx context.Context, clientID string) (Cliente, error)

	// GravarCodigo grava um código de autorização recém-emitido.
	GravarCodigo(ctx context.Context, c Codigo) error
	// ConsumirCodigo marca o código como usado numa escrita condicional e
	// devolve a linha. O segundo retorno é falso quando o código já estava
	// usado — que é a detecção de replay do código, e obriga a revogar a
	// família que ele emitiu.
	ConsumirCodigo(ctx context.Context, hash string, agora time.Time) (Codigo, bool, error)

	// GravarPar grava o access e o refresh emitidos juntos, numa transação.
	GravarPar(ctx context.Context, acesso, refresh Token) error
	// TokenPorHash devolve o token pelo hash de armazenamento.
	TokenPorHash(ctx context.Context, hash string) (Token, error)
	// Rotacionar marca o refresh antigo como substituído e grava o par novo,
	// tudo numa transação. Devolve falso quando o antigo já tinha sido
	// substituído ou revogado entre a leitura e a escrita — a corrida que a
	// detecção de replay não pode perder.
	Rotacionar(ctx context.Context, antigoID int64, acesso, refresh Token, agora time.Time) (bool, error)

	// RevogarFamilia revoga todos os tokens de uma família e queima os códigos
	// dela. É a resposta a replay.
	RevogarFamilia(ctx context.Context, familiaID string, agora time.Time) error
	// RevogarToken revoga um token só (RFC 7009 sobre um access token).
	RevogarToken(ctx context.Context, id int64, agora time.Time) error
	// RegistrarUsoToken grava o instante do último uso de um token.
	RegistrarUsoToken(ctx context.Context, id int64, quando time.Time) error
	// LimparExpirados apaga código e token vencidos há mais de uma janela.
	LimparExpirados(ctx context.Context, antesDe time.Time) error
}

// Servico é a regra do authorization server.
type Servico struct {
	repo       Repositorio
	endpoints  Endpoints
	urlPublica string
	escopo     func(slug string) string
	log        *slog.Logger
	agora      func() time.Time

	validadeCodigo  time.Duration
	validadeAcesso  time.Duration
	validadeRefresh time.Duration

	usos chan usoToken
}

type usoToken struct {
	id     int64
	quando time.Time
}

// capacidadeUsos é a fila de "último uso" de token. Cheia, descarta: gravar o
// uso é telemetria, e telemetria não segura o caminho da requisição nem compete
// pelo escritor único do SQLite.
const capacidadeUsos = 256

// Opcao ajusta o serviço na construção.
type Opcao func(*Servico)

// ComRelogio troca a fonte de tempo. É o que o teste usa para envelhecer um
// token sem esperar pelo relógio.
func ComRelogio(agora func() time.Time) Opcao {
	return func(s *Servico) { s.agora = agora }
}

// ComValidades troca as três durações de credencial.
func ComValidades(codigo, acesso, refresh time.Duration) Opcao {
	return func(s *Servico) {
		s.validadeCodigo, s.validadeAcesso, s.validadeRefresh = codigo, acesso, refresh
	}
}

// NovoServico monta o authorization server.
//
// escopo é injetado porque o nome do escopo de um endpoint é contrato
// compartilhado com a feature de chave de API, e feature não importa feature:
// quem conhece as duas pontas é main.
func NovoServico(
	repo Repositorio, endpoints Endpoints,
	urlPublica string, escopo func(slug string) string,
	log *slog.Logger, opcoes ...Opcao,
) *Servico {
	s := &Servico{
		repo:            repo,
		endpoints:       endpoints,
		urlPublica:      strings.TrimRight(urlPublica, "/"),
		escopo:          escopo,
		log:             log,
		agora:           time.Now,
		validadeCodigo:  ValidadeCodigo,
		validadeAcesso:  ValidadeAcesso,
		validadeRefresh: ValidadeRefresh,
		usos:            make(chan usoToken, capacidadeUsos),
	}
	for _, o := range opcoes {
		o(s)
	}
	return s
}

// Emissor devolve o issuer anunciado no documento RFC 8414. É também o valor do
// parâmetro iss do RFC 9207 na resposta de autorização.
func (s *Servico) Emissor() string { return s.urlPublica }

// Recurso devolve a URL canônica de um endpoint — o resource do RFC 8707, o
// aud do token e o campo resource da metadata RFC 9728, que são o mesmo texto.
//
// O slug entra aqui e por isso é contrato: renomear um slug invalida em
// silêncio toda credencial daquele endpoint.
func (s *Servico) Recurso(slug string) string { return s.urlPublica + "/mcp/" + slug }

// slugDoRecurso faz o caminho inverso, com a normalização do RFC 8707 §2: uma
// barra final é tolerada, qualquer outra diferença não.
func (s *Servico) slugDoRecurso(recurso string) (string, bool) {
	prefixo := s.urlPublica + "/mcp/"
	resto, ok := strings.CutPrefix(strings.TrimSuffix(recurso, "/"), prefixo)
	if !ok || resto == "" || strings.ContainsAny(resto, "/?#") {
		return "", false
	}
	return resto, true
}

// Escopos devolve o escopo de cada endpoint existente, para o scopes_supported
// da metadata.
func (s *Servico) Escopos(ctx context.Context) ([]string, error) {
	refs, err := s.endpoints.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, s.escopo(r.Slug))
	}
	return out, nil
}

// Endpoint devolve o endpoint de um slug.
func (s *Servico) Endpoint(ctx context.Context, slug string) (EndpointRef, error) {
	refs, err := s.endpoints.Todos(ctx)
	if err != nil {
		return EndpointRef{}, err
	}
	for _, r := range refs {
		if r.Slug == slug {
			return r, nil
		}
	}
	return EndpointRef{}, ErrEndpointNaoEncontrado
}

// --- authorize ---

// PedidoAutorizacao são os parâmetros de /oauth/authorize, como chegaram.
type PedidoAutorizacao struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	Escopo              string
	State               string
}

// Autorizacao é o pedido já validado: é o que a tela de consentimento mostra e
// o que a emissão do código grava.
type Autorizacao struct {
	Pedido   PedidoAutorizacao
	Cliente  Cliente
	Endpoint EndpointRef
	Escopo   string
}

// tamanhoMaximoParametro limita os campos que voltam para o navegador na tela
// de consentimento e na query do redirect. Sem teto, um state de 1 MB vira uma
// página de 1 MB e um redirect que nenhum proxy aceita.
const tamanhoMaximoParametro = 512

// Validar confere o pedido de autorização inteiro.
//
// A ordem é a do RFC 6749 §4.1.2.1 e importa: enquanto client_id e redirect_uri
// não estiverem validados, nenhum erro pode virar redirecionamento — por isso
// esses dois erros vêm marcados com SemRedirect.
func (s *Servico) Validar(ctx context.Context, p PedidoAutorizacao) (Autorizacao, error) {
	if p.ClientID == "" {
		return Autorizacao{}, &ErroOAuth{
			Codigo: ErroInvalidRequest, Descricao: "client_id ausente",
			Status: http.StatusBadRequest, SemRedirect: true,
		}
	}
	cliente, err := s.repo.ClientePorClientID(ctx, p.ClientID)
	switch {
	case errors.Is(err, ErrClienteNaoEncontrado):
		return Autorizacao{}, &ErroOAuth{
			Codigo: ErroInvalidClient, Descricao: "client_id desconhecido",
			Status: http.StatusUnauthorized, SemRedirect: true,
		}
	case err != nil:
		return Autorizacao{}, erroInterno(err)
	}

	if p.RedirectURI == "" {
		return Autorizacao{}, &ErroOAuth{
			Codigo: ErroInvalidRequest, Descricao: "redirect_uri ausente",
			Status: http.StatusBadRequest, SemRedirect: true,
		}
	}
	if !cliente.PermiteRedirect(p.RedirectURI) {
		return Autorizacao{}, &ErroOAuth{
			Codigo: ErroInvalidRequest, Descricao: "redirect_uri não está na allowlist do cliente",
			Status: http.StatusBadRequest, SemRedirect: true,
		}
	}

	// Daqui para baixo o erro volta pelo redirect, com state e iss.
	switch {
	case p.ResponseType == "":
		return Autorizacao{}, erroOAuth(ErroInvalidRequest, "response_type ausente")
	case p.ResponseType != "code":
		return Autorizacao{}, erroOAuth(ErroUnsupportedResponseType,
			"só response_type=code é suportado")
	}

	// PKCE S256 obrigatório, sem exceção e sem "plain": o OAuth 2.1 exige PKCE
	// de todo cliente, e "plain" transmite o verificador no lugar do desafio.
	switch {
	case p.CodeChallenge == "":
		return Autorizacao{}, erroOAuth(ErroInvalidRequest, "code_challenge é obrigatório (PKCE S256)")
	case p.CodeChallengeMethod == "" || p.CodeChallengeMethod != "S256":
		return Autorizacao{}, erroOAuth(ErroInvalidRequest, "code_challenge_method precisa ser S256")
	case !desafioBemFormado(p.CodeChallenge):
		return Autorizacao{}, erroOAuth(ErroInvalidRequest, "code_challenge malformado")
	}

	if p.State == "" {
		return Autorizacao{}, erroOAuth(ErroInvalidRequest, "state é obrigatório")
	}
	if len(p.State) > tamanhoMaximoParametro {
		return Autorizacao{}, erroOAuth(ErroInvalidRequest, "state longo demais")
	}

	// resource do RFC 8707 é obrigatório: é ele que amarra o token a um
	// endpoint, e sem ele o token valeria no gateway inteiro — o isolamento
	// entre endpoints, que é a razão de existir do produto, sumiria.
	if p.Resource == "" {
		return Autorizacao{}, erroOAuth(ErroInvalidTarget,
			"resource é obrigatório e precisa ser a URL canônica de um endpoint")
	}
	slug, ok := s.slugDoRecurso(p.Resource)
	if !ok {
		return Autorizacao{}, erroOAuth(ErroInvalidTarget,
			"resource não é a URL canônica de um endpoint deste patchbay")
	}
	endpoint, ok := cliente.EndpointPorSlug(slug)
	if !ok {
		return Autorizacao{}, erroOAuth(ErroInvalidTarget,
			"este cliente não tem acesso ao endpoint pedido em resource")
	}

	escopo := s.escopo(endpoint.Slug)
	if p.Escopo != "" && !escopoContido(p.Escopo, escopo) {
		return Autorizacao{}, erroOAuth(ErroInvalidScope,
			"o único escopo deste resource é "+escopo)
	}

	return Autorizacao{Pedido: p, Cliente: cliente, Endpoint: endpoint, Escopo: escopo}, nil
}

// EmitirCodigo sorteia o código de autorização e o grava por hash.
//
// O código é guardado como hash pelo mesmo motivo da chave de API: ele é uma
// credencial que o patchbay verifica, e nunca precisa voltar em claro. Ele nasce
// amarrado a cliente, redirect, desafio PKCE e resource — os quatro que a troca
// vai reconferir.
func (s *Servico) EmitirCodigo(ctx context.Context, a Autorizacao) (string, error) {
	claro, err := sortear(MarcaCodigo)
	if err != nil {
		return "", erroInterno(err)
	}
	fam, err := familia()
	if err != nil {
		return "", erroInterno(err)
	}
	agora := s.agora()
	cod := Codigo{
		Hash:          Hash(claro),
		FamiliaID:     fam,
		ClienteID:     a.Cliente.ID,
		EndpointID:    a.Endpoint.ID,
		Resource:      s.Recurso(a.Endpoint.Slug),
		Escopo:        a.Escopo,
		RedirectURI:   a.Pedido.RedirectURI,
		CodeChallenge: a.Pedido.CodeChallenge,
		CriadoEm:      agora,
		ExpiraEm:      agora.Add(s.validadeCodigo),
	}
	if err := s.repo.GravarCodigo(ctx, cod); err != nil {
		return "", erroInterno(err)
	}
	s.log.Info("consentimento concedido",
		"client_id", a.Cliente.ClientID, "endpoint", a.Endpoint.Slug, "familia_id", fam)
	return claro, nil
}

// --- token ---

// PedidoToken são os parâmetros de /oauth/token, como chegaram.
type PedidoToken struct {
	GrantType    string
	Codigo       string
	RedirectURI  string
	CodeVerifier string
	RefreshToken string
	Resource     string
	Escopo       string
}

// Trocar executa o grant authorization_code.
func (s *Servico) Trocar(ctx context.Context, cliente Cliente, p PedidoToken) (Concessao, error) {
	if p.Codigo == "" {
		return Concessao{}, erroOAuth(ErroInvalidRequest, "code ausente")
	}
	agora := s.agora()

	cod, consumiu, err := s.repo.ConsumirCodigo(ctx, Hash(p.Codigo), agora)
	switch {
	case errors.Is(err, ErrCodigoNaoEncontrado):
		return Concessao{}, erroOAuth(ErroInvalidGrant, "código inválido ou expirado")
	case err != nil:
		return Concessao{}, erroInterno(err)
	}

	// Reuso de código: a troca única é o que impede um código interceptado de
	// valer duas vezes, e a resposta correta é queimar tudo o que a primeira
	// troca emitiu — não só recusar a segunda.
	if !consumiu {
		s.revogarFamilia(ctx, cod.FamiliaID, agora, "código de autorização reutilizado")
		return Concessao{}, erroOAuth(ErroInvalidGrant, "código já utilizado")
	}
	if !agora.Before(cod.ExpiraEm) {
		return Concessao{}, erroOAuth(ErroInvalidGrant, "código expirado")
	}
	if cod.ClienteID != cliente.ID {
		s.revogarFamilia(ctx, cod.FamiliaID, agora, "código apresentado por outro cliente")
		return Concessao{}, erroOAuth(ErroInvalidGrant, "código não pertence a este cliente")
	}
	if p.RedirectURI != cod.RedirectURI {
		return Concessao{}, erroOAuth(ErroInvalidGrant,
			"redirect_uri diferente da usada na autorização")
	}
	if p.Resource != "" && p.Resource != cod.Resource {
		return Concessao{}, erroOAuth(ErroInvalidTarget, "resource diferente do autorizado")
	}
	if err := conferirPKCE(p.CodeVerifier, cod.CodeChallenge); err != nil {
		return Concessao{}, err
	}

	acesso, refresh, concessao, err := s.novoPar(cod.FamiliaID, cliente.ID, cod.EndpointID,
		cod.Resource, cod.Escopo, agora)
	if err != nil {
		return Concessao{}, err
	}
	if err := s.repo.GravarPar(ctx, acesso, refresh); err != nil {
		return Concessao{}, erroInterno(err)
	}
	s.log.Info("token emitido por authorization_code",
		"client_id", cliente.ClientID, "resource", cod.Resource, "familia_id", cod.FamiliaID)
	return concessao, nil
}

// Renovar executa o grant refresh_token, com rotação e detecção de replay.
//
// A rotação é exigência do OAuth 2.1 para cliente público — que é o caso de todo
// cliente de CIMD e de DCR, e do claude.ai. A detecção de replay é o que a torna
// útil: sem revogar a família ao ver um refresh já rotacionado, a rotação só
// troca o token de lugar.
func (s *Servico) Renovar(ctx context.Context, cliente Cliente, p PedidoToken) (Concessao, error) {
	if p.RefreshToken == "" {
		return Concessao{}, erroOAuth(ErroInvalidRequest, "refresh_token ausente")
	}
	agora := s.agora()

	antigo, err := s.repo.TokenPorHash(ctx, Hash(p.RefreshToken))
	switch {
	case errors.Is(err, ErrTokenNaoEncontrado):
		return Concessao{}, erroOAuth(ErroInvalidGrant, "refresh_token inválido")
	case err != nil:
		return Concessao{}, erroInterno(err)
	}
	if antigo.Tipo != TipoRefresh {
		return Concessao{}, erroOAuth(ErroInvalidGrant, "o token apresentado não é um refresh_token")
	}
	if antigo.ClienteID != cliente.ID {
		s.revogarFamilia(ctx, antigo.FamiliaID, agora, "refresh_token apresentado por outro cliente")
		return Concessao{}, erroOAuth(ErroInvalidGrant, "refresh_token não pertence a este cliente")
	}
	// Já rotacionado ou já revogado: é replay. Toda a família cai — inclusive o
	// refresh que o atacante (ou o cliente legítimo) tem em mãos agora.
	if antigo.SubstituidoPor != 0 || !antigo.RevogadoEm.IsZero() {
		s.revogarFamilia(ctx, antigo.FamiliaID, agora, "refresh_token reapresentado depois de rotacionado")
		return Concessao{}, erroOAuth(ErroInvalidGrant, "refresh_token já utilizado; a sessão foi revogada")
	}
	if !agora.Before(antigo.ExpiraEm) {
		return Concessao{}, erroOAuth(ErroInvalidGrant, "refresh_token expirado")
	}
	if p.Escopo != "" && !escopoContido(p.Escopo, antigo.Escopo) {
		return Concessao{}, erroOAuth(ErroInvalidScope, "o refresh não pode ampliar o escopo concedido")
	}
	if p.Resource != "" && p.Resource != antigo.Resource {
		return Concessao{}, erroOAuth(ErroInvalidTarget, "resource diferente do concedido")
	}

	acesso, refresh, concessao, err := s.novoPar(antigo.FamiliaID, cliente.ID, antigo.EndpointID,
		antigo.Resource, antigo.Escopo, agora)
	if err != nil {
		return Concessao{}, err
	}
	rotacionou, err := s.repo.Rotacionar(ctx, antigo.ID, acesso, refresh, agora)
	if err != nil {
		return Concessao{}, erroInterno(err)
	}
	if !rotacionou {
		// Duas renovações concorrentes com o mesmo refresh: uma ganhou a
		// escrita condicional, a outra chega aqui. É replay do ponto de vista
		// do AS, e o tratamento é o mesmo.
		s.revogarFamilia(ctx, antigo.FamiliaID, agora, "refresh_token rotacionado concorrentemente")
		return Concessao{}, erroOAuth(ErroInvalidGrant, "refresh_token já utilizado; a sessão foi revogada")
	}
	s.log.Info("token renovado por rotação",
		"client_id", cliente.ClientID, "resource", antigo.Resource, "familia_id", antigo.FamiliaID)
	return concessao, nil
}

// novoPar sorteia o access e o refresh de uma família.
func (s *Servico) novoPar(
	familiaID string, clienteID, endpointID int64,
	resource, escopo string, agora time.Time,
) (acesso, refresh Token, c Concessao, err error) {
	acessoClaro, err := sortear(MarcaAcesso)
	if err != nil {
		return Token{}, Token{}, Concessao{}, erroInterno(err)
	}
	refreshClaro, err := sortear(MarcaRefresh)
	if err != nil {
		return Token{}, Token{}, Concessao{}, erroInterno(err)
	}

	expiraAcesso := agora.Add(s.validadeAcesso)
	base := Token{
		FamiliaID:  familiaID,
		ClienteID:  clienteID,
		EndpointID: endpointID,
		Resource:   resource,
		Escopo:     escopo,
		CriadoEm:   agora,
	}
	acesso = base
	acesso.Hash, acesso.Tipo, acesso.ExpiraEm = Hash(acessoClaro), TipoAcesso, expiraAcesso
	refresh = base
	refresh.Hash, refresh.Tipo, refresh.ExpiraEm = Hash(refreshClaro), TipoRefresh, agora.Add(s.validadeRefresh)

	return acesso, refresh, Concessao{
		AccessToken:  acessoClaro,
		RefreshToken: refreshClaro,
		ExpiraEm:     expiraAcesso,
		Escopo:       escopo,
		Resource:     resource,
	}, nil
}

// revogarFamilia queima a família inteira e registra o motivo.
//
// Não devolve erro de propósito: o chamador já vai recusar a requisição, e uma
// falha de escrita aqui não pode virar 500 que esconde o invalid_grant que o
// cliente precisa ver para reautenticar.
func (s *Servico) revogarFamilia(ctx context.Context, familiaID string, agora time.Time, motivo string) {
	s.log.Warn("família de tokens revogada", "familia_id", familiaID, "motivo", motivo)
	if err := s.repo.RevogarFamilia(ctx, familiaID, agora); err != nil {
		s.log.Error("falha ao revogar família de tokens", "familia_id", familiaID, "erro", err)
	}
}

// --- revogação (RFC 7009) ---

// Revogar atende o revocation endpoint.
//
// Token desconhecido não é erro (RFC 7009 §2.2): o resultado desejado — "este
// token não abre mais nada" — já vale. Revogar um refresh derruba a família
// inteira, que é o "SHOULD invalidate related tokens" do §2.1.
func (s *Servico) Revogar(ctx context.Context, cliente Cliente, claro string) error {
	if claro == "" {
		return nil
	}
	tok, err := s.repo.TokenPorHash(ctx, Hash(claro))
	switch {
	case errors.Is(err, ErrTokenNaoEncontrado):
		return nil
	case err != nil:
		return erroInterno(err)
	}
	if tok.ClienteID != cliente.ID {
		// Token de outro cliente: silêncio. Dizer "existe mas não é seu"
		// transforma o endpoint num oráculo de tokens alheios.
		return nil
	}

	agora := s.agora()
	if tok.Tipo == TipoRefresh {
		s.revogarFamilia(ctx, tok.FamiliaID, agora, "revogação pedida pelo cliente")
		return nil
	}
	if err := s.repo.RevogarToken(ctx, tok.ID, agora); err != nil {
		return erroInterno(err)
	}
	s.log.Info("access token revogado pelo cliente", "client_id", cliente.ClientID)
	return nil
}

// --- verificação do bearer em /mcp/{slug} ---

// Verificar tem a assinatura de auth.TokenVerifier do go-sdk: é o que se entrega
// a auth.RequireBearerToken, ao lado do verificador de chave de API.
//
// O escopo devolvido é o do endpoint gravado no token, e é o middleware quem
// compara com o escopo exigido pela URL — é de lá que sai o 403 quando um token
// de um endpoint é apresentado noutro. Fazer essa recusa aqui devolveria 401, e
// 401 diz ao cliente "reautentique", que é o conselho errado.
func (s *Servico) Verificar(ctx context.Context, claro string, _ *http.Request) (*auth.TokenInfo, error) {
	if !TemMarca(claro, MarcaAcesso) {
		return nil, fmt.Errorf("%w: não é um access token do patchbay", auth.ErrInvalidToken)
	}

	tok, err := s.repo.TokenPorHash(ctx, Hash(claro))
	switch {
	case errors.Is(err, ErrTokenNaoEncontrado):
		return nil, fmt.Errorf("%w: token desconhecido", auth.ErrInvalidToken)
	case err != nil:
		// Falha de infraestrutura é 500, não 401: negar acesso por disco cheio
		// esconderia o problema real atrás de um "reautentique".
		s.log.Error("falha ao consultar access token", "erro", err)
		return nil, errInterno
	case tok.Tipo != TipoAcesso:
		return nil, fmt.Errorf("%w: o token apresentado não é um access token", auth.ErrInvalidToken)
	case !tok.RevogadoEm.IsZero():
		return nil, fmt.Errorf("%w: token revogado", auth.ErrInvalidToken)
	case tok.ClienteRevogado:
		return nil, fmt.Errorf("%w: cliente revogado", auth.ErrInvalidToken)
	}

	agora := s.agora()
	if !agora.Before(tok.ExpiraEm) {
		return nil, fmt.Errorf("%w: token expirado", auth.ErrInvalidToken)
	}
	s.marcarUso(tok.ID, agora)

	return &auth.TokenInfo{
		Scopes:     strings.Fields(tok.Escopo),
		Expiration: tok.ExpiraEm,
		UserID:     "oauth:" + tok.ClienteClientID,
		// O aud fica disponível para a trilha de chamadas da fatia 12; a
		// autorização em si já saiu do escopo.
		Extra: map[string]any{"aud": tok.Resource},
	}, nil
}

// errInterno é o que o cliente vê quando a falha é do patchbay, não do token.
var errInterno = errors.New("erro interno ao verificar a credencial")

func (s *Servico) marcarUso(id int64, quando time.Time) {
	select {
	case s.usos <- usoToken{id: id, quando: quando}:
	default:
		s.log.Debug("fila de último uso de token cheia, registro descartado", "oauth_token_id", id)
	}
}

// GravarUsos drena a fila de "último uso" até ctx ser cancelado. Quem chama é
// dono da goroutine.
func (s *Servico) GravarUsos(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-s.usos:
			if err := s.repo.RegistrarUsoToken(ctx, u.id, u.quando); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Warn("não gravou último uso do token", "oauth_token_id", u.id, "erro", err)
			}
		}
	}
}

// Limpar apaga códigos e tokens vencidos. Vencido já não autoriza nada — a
// varredura só evita que as tabelas cresçam para sempre.
func (s *Servico) Limpar(ctx context.Context) error {
	return s.repo.LimparExpirados(ctx, s.agora().Add(-s.validadeRefresh))
}

// --- autenticação de cliente no token e no revocation endpoint ---

// AutenticarCliente resolve e confere o cliente de uma requisição de token ou
// de revogação. O segredo vem vazio para cliente público, que é o método "none"
// anunciado na metadata.
func (s *Servico) AutenticarCliente(ctx context.Context, clientID, segredo string) (Cliente, error) {
	if clientID == "" {
		return Cliente{}, &ErroOAuth{
			Codigo: ErroInvalidClient, Descricao: "client_id ausente",
			Status: http.StatusUnauthorized,
		}
	}
	cliente, err := s.repo.ClientePorClientID(ctx, clientID)
	switch {
	case errors.Is(err, ErrClienteNaoEncontrado):
		return Cliente{}, &ErroOAuth{
			Codigo: ErroInvalidClient, Descricao: "cliente desconhecido",
			Status: http.StatusUnauthorized,
		}
	case err != nil:
		return Cliente{}, erroInterno(err)
	}

	if !cliente.Confidencial {
		// Cliente público: não tem segredo a conferir. Mandar um segredo aqui é
		// erro de configuração do cliente, e aceitar em silêncio esconderia que
		// ele acha que é confidencial.
		if segredo != "" {
			return Cliente{}, &ErroOAuth{
				Codigo: ErroInvalidClient, Descricao: "este cliente é público e não tem segredo",
				Status: http.StatusUnauthorized,
			}
		}
		return cliente, nil
	}

	// Comparação em tempo constante sobre o hash: o hash tem tamanho fixo, então
	// nem o tempo nem o tamanho vazam informação sobre o segredo.
	esperado, apresentado := []byte(cliente.segredoHash), []byte(Hash(segredo))
	if segredo == "" || subtle.ConstantTimeCompare(esperado, apresentado) != 1 {
		return Cliente{}, &ErroOAuth{
			Codigo: ErroInvalidClient, Descricao: "client_secret inválido",
			Status: http.StatusUnauthorized,
		}
	}
	return cliente, nil
}

// --- apoio ---

// conferirPKCE compara o verificador apresentado com o desafio gravado.
func conferirPKCE(verificador, desafio string) error {
	if verificador == "" {
		return erroOAuth(ErroInvalidRequest, "code_verifier ausente")
	}
	// RFC 7636 §4.1: entre 43 e 128 caracteres.
	if len(verificador) < 43 || len(verificador) > 128 {
		return erroOAuth(ErroInvalidGrant, "code_verifier fora do tamanho do RFC 7636")
	}
	soma := sha256.Sum256([]byte(verificador))
	calculado := base64.RawURLEncoding.EncodeToString(soma[:])
	if subtle.ConstantTimeCompare([]byte(calculado), []byte(desafio)) != 1 {
		return erroOAuth(ErroInvalidGrant, "code_verifier não corresponde ao code_challenge")
	}
	return nil
}

// desafioBemFormado confere a forma do code_challenge S256: 43 caracteres
// base64url sem padding, que é o comprimento fixo de um SHA-256.
func desafioBemFormado(desafio string) bool {
	if len(desafio) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(desafio)
	return err == nil
}

// escopoContido informa se todo escopo pedido está no concedido.
func escopoContido(pedido, concedido string) bool {
	tem := strings.Fields(concedido)
	for _, p := range strings.Fields(pedido) {
		encontrou := false
		for _, c := range tem {
			if c == p {
				encontrou = true
				break
			}
		}
		if !encontrou {
			return false
		}
	}
	return true
}
