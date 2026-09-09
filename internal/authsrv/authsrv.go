// Package authsrv é o authorization server OAuth 2.1 que o patchbay embute
// para os seus próprios clientes — o claude.ai, o Claude Code e qualquer outro
// cliente MCP remoto.
//
// É código próprio e não biblioteca, pela conta da seção 09 do estudo prévio: o
// subconjunto que a spec MCP exige é um grant só (authorization_code com PKCE
// S256 obrigatório) mais refresh, e o parâmetro resource do RFC 8707 — que é o
// que faz um token valer para um endpoint só — não existe nas estruturas do
// zitadel/oidc nem do fosite. O que compra de volta a maturidade da biblioteca
// recusada é o teste: o AS é exercitado pelo cliente OAuth real do go-sdk
// (auth.AuthorizationCodeHandler), não por um cliente escrito à mão que
// concordaria com o meu erro de entendimento.
//
// Tudo o que este pacote guarda é credencial que ele verifica — código, access
// token, refresh token, segredo de cliente. Nenhuma volta em claro: vai hash,
// nunca cifra reversível (decisão 12). Por isso este pacote não conhece a chave
// mestra e não depende da fatia de segredos.
package authsrv

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Marcas dos segredos emitidos por este pacote. Existem para que um valor solto
// num log ou num arquivo de configuração alheio seja reconhecível — e para que
// o verificador de bearer saiba, sem consultar o banco, se o que chegou é chave
// de API ou token deste AS.
const (
	// MarcaAcesso prefixa todo access token.
	MarcaAcesso = "pbat"
	// MarcaRefresh prefixa todo refresh token.
	MarcaRefresh = "pbrt"
	// MarcaCodigo prefixa todo código de autorização.
	MarcaCodigo = "pbac"
	// MarcaSegredo prefixa todo segredo de cliente confidencial.
	MarcaSegredo = "pbcs"
)

// Durações padrão das credenciais emitidas.
const (
	// ValidadeCodigo é curta porque o código atravessa o navegador: ele vive o
	// tempo de um redirect e de uma troca. Dez minutos é o teto do RFC 6749.
	ValidadeCodigo = 5 * time.Minute
	// ValidadeAcesso é curta de propósito: o access token é o que vaza com mais
	// facilidade, e a revogação de família só vale a partir do próximo refresh.
	ValidadeAcesso = time.Hour
	// ValidadeRefresh é longa porque é ela que evita pedir consentimento de novo
	// a cada dia de uso. A rotação é o que a torna aceitável.
	ValidadeRefresh = 30 * 24 * time.Hour
)

// Tipos de token gravados em oauth_token.tipo.
const (
	TipoAcesso  = "access"
	TipoRefresh = "refresh"
)

// Erros sentinela do pacote. São os de persistência e de estado; o erro que
// chega ao cliente OAuth é sempre um *ErroOAuth (ver erros.go).
var (
	// ErrClienteNaoEncontrado indica client_id desconhecido ou revogado.
	ErrClienteNaoEncontrado = errors.New("authsrv: cliente não encontrado")
	// ErrClienteEmUso indica client_id já cadastrado.
	ErrClienteEmUso = errors.New("authsrv: client_id já cadastrado")
	// ErrCodigoNaoEncontrado indica código inexistente ou já expirado.
	ErrCodigoNaoEncontrado = errors.New("authsrv: código de autorização não encontrado")
	// ErrTokenNaoEncontrado indica token inexistente.
	ErrTokenNaoEncontrado = errors.New("authsrv: token não encontrado")
	// ErrEndpointNaoEncontrado indica slug que não existe.
	ErrEndpointNaoEncontrado = errors.New("authsrv: endpoint não encontrado")
	// ErrSemEndpoint indica cliente pedido sem nenhum endpoint no escopo.
	ErrSemEndpoint = errors.New("authsrv: cliente sem endpoint no escopo")
	// ErrSemRedirect indica cliente pedido sem nenhuma redirect_uri.
	ErrSemRedirect = errors.New("authsrv: cliente sem redirect_uri")
)

// RedirectClaudeAI é a URI fixa das superfícies hospedadas da Anthropic —
// claude.ai web, Desktop, mobile e Cowork. A comparação com ela é exata; o
// match de loopback ignorando a porta, que o Claude Code exige, é da fatia 11.
const RedirectClaudeAI = "https://claude.ai/api/mcp/auth_callback"

// EndpointRef é o que este pacote precisa saber de um endpoint: o identificador
// que amarra o token e o slug que entra na URL canônica do resource.
//
// Declarado aqui, no consumidor: quem é dono de endpoint é outra feature.
type EndpointRef struct {
	ID   int64
	Slug string
	Nome string
}

// Rotulo é como o endpoint aparece na tela de consentimento e no formulário.
func (e EndpointRef) Rotulo() string {
	if e.Nome != "" {
		return e.Nome
	}
	return e.Slug
}

// Endpoints é o que este pacote precisa da feature de endpoint.
type Endpoints interface {
	// Todos devolve os endpoints existentes, ordenados por slug.
	Todos(ctx context.Context) ([]EndpointRef, error)
}

// Cliente é um cliente OAuth registrado.
type Cliente struct {
	ID           int64
	ClientID     string
	Nome         string
	Tipo         string
	Confidencial bool
	RedirectURIs []string
	Endpoints    []EndpointRef
	CriadoEm     time.Time
	RevogadoEm   time.Time

	// EscopoAberto marca o cliente que pode *pedir* qualquer endpoint, em vez de
	// ter uma lista escolhida pelo admin. É o caso de todo registro dinâmico —
	// DCR e CIMD —, porque ninguém escolheu endpoint por ele. Endpoints vem
	// preenchido com todos os existentes quando isto é verdadeiro, então o resto
	// do pacote não precisa saber da diferença.
	EscopoAberto bool
	// Origem é de onde o registro veio: o IP remoto no DCR, o hostname do
	// documento no CIMD, vazio no cadastro pela UI.
	Origem string
	// ExpiraEm é o TTL do cache de um documento de CIMD. Zero para quem não é
	// cache: registro não expira, é revogado.
	ExpiraEm time.Time

	// segredoHash e SegredoPrefixo seguem a mesma regra da chave de API: o
	// segredo aparece em claro uma vez, na criação, e depois só o prefixo — que
	// é o que permite a UI dizer qual credencial é qual sem guardá-la.
	segredoHash    string
	SegredoPrefixo string
}

// Revogado informa se o cliente foi revogado.
func (c Cliente) Revogado() bool { return !c.RevogadoEm.IsZero() }

// PermiteRedirect e a regra de loopback do RFC 8252 estão em dinamico.go, junto
// da resolução de CIMD que as tornou necessárias.

// EndpointPorSlug devolve o endpoint do escopo do cliente pelo slug.
func (c Cliente) EndpointPorSlug(slug string) (EndpointRef, bool) {
	for _, e := range c.Endpoints {
		if e.Slug == slug {
			return e, true
		}
	}
	return EndpointRef{}, false
}

// Codigo é o código de autorização como ele existe no banco, sem o texto claro.
type Codigo struct {
	Hash          string
	FamiliaID     string
	ClienteID     int64
	EndpointID    int64
	Resource      string
	Escopo        string
	RedirectURI   string
	CodeChallenge string
	CriadoEm      time.Time
	ExpiraEm      time.Time
	UsadoEm       time.Time
}

// Token é um access ou refresh token como ele existe no banco, sem o texto
// claro.
type Token struct {
	ID             int64
	Hash           string
	Tipo           string
	FamiliaID      string
	ClienteID      int64
	EndpointID     int64
	Resource       string
	Escopo         string
	SubstituidoPor int64
	CriadoEm       time.Time
	ExpiraEm       time.Time
	RevogadoEm     time.Time
	UltimoUsoEm    time.Time

	// ClienteClientID e ClienteRevogado vêm do JOIN da consulta por hash: a
	// verificação de bearer precisa saber, na mesma ida ao banco, se o cliente
	// dono do token ainda existe. Revogar um cliente sem isso deixaria os
	// tokens dele valendo até expirarem.
	ClienteClientID string
	ClienteRevogado bool
}

// Concessao é o que o token endpoint devolve ao cliente.
type Concessao struct {
	AccessToken  string
	RefreshToken string
	ExpiraEm     time.Time
	Escopo       string
	Resource     string
}

// bytesSegredo é a entropia de todo segredo sorteado aqui. 256 bits: como não
// há dicionário a encarecer, SHA-256 basta como hash de armazenamento — o mesmo
// raciocínio da chave de API.
const bytesSegredo = 32

// bytesIdentificador rende oito caracteres base32 no prefixo visível do segredo
// de cliente.
const bytesIdentificador = 5

// sortear devolve um segredo em claro com a marca pedida.
func sortear(marca string) (string, error) {
	bruto := make([]byte, bytesSegredo)
	if _, err := rand.Read(bruto); err != nil {
		return "", fmt.Errorf("authsrv: sortear segredo %s: %w", marca, err)
	}
	return marca + "_" + base64.RawURLEncoding.EncodeToString(bruto), nil
}

// Hash é o hash de armazenamento de qualquer credencial deste pacote.
func Hash(claro string) string {
	soma := sha256.Sum256([]byte(claro))
	return hex.EncodeToString(soma[:])
}

// TemMarca informa se o texto tem a forma de uma credencial daquela marca. É o
// que permite ao verificador de bearer escolher entre chave de API e token do
// AS sem consultar o banco duas vezes.
func TemMarca(claro, marca string) bool {
	return strings.HasPrefix(claro, marca+"_") && len(claro) > len(marca)+1
}

// familia sorteia o identificador de uma família de tokens.
//
// A família nasce com o código de autorização e não com o primeiro refresh: se
// nascesse depois, o par emitido pela troca do código ficaria fora dela e a
// revogação por replay deixaria um access token vivo.
func familia() (string, error) {
	bruto := make([]byte, 16)
	if _, err := rand.Read(bruto); err != nil {
		return "", fmt.Errorf("authsrv: sortear família: %w", err)
	}
	return hex.EncodeToString(bruto), nil
}
