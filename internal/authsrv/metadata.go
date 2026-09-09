package authsrv

import (
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Caminhos públicos do authorization server. Ficam aqui, e não em webui, porque
// são contrato de protocolo e não URL de tela: o sufixo well-known é fixado
// pelo RFC 8414 e pelo RFC 9728, e o cliente monta essas URLs sozinho.
const (
	// RotaMetadataAS é o documento RFC 8414. O caminho é obrigatório: o cliente
	// o deriva do issuer, nunca o descobre.
	RotaMetadataAS = "/.well-known/oauth-authorization-server"
	// RotaMetadataRecurso é a metadata RFC 9728 de um endpoint, no caminho com
	// sufixo. Cada endpoint é um protected resource distinto (seção 07): é isso
	// que faz o aud do token carregar o endpoint e a verificação virar uma
	// comparação de string.
	RotaMetadataRecurso = "/.well-known/oauth-protected-resource/mcp/{endpoint}"
	// RotaMetadataRecursoRaiz é o fallback do RFC 9728 na raiz, sem sufixo de
	// endpoint. O RFC exige que o cliente suporte os dois mecanismos de
	// descoberta; este é o que responde quando ele bate ali direto, sem ainda
	// saber qual slug quer.
	RotaMetadataRecursoRaiz = "/.well-known/oauth-protected-resource"
	// RotaAutorizar é o authorize endpoint. Fica atrás da sessão de admin.
	RotaAutorizar = "/oauth/authorize"
	// RotaToken é o token endpoint.
	//
	//nolint:gosec // G101: é o caminho da rota, não uma credencial embutida
	RotaToken = "/oauth/token"
	// RotaRevogar é o revocation endpoint (RFC 7009).
	RotaRevogar = "/oauth/revoke"
	// RotaRegistrar é o registration endpoint do RFC 7591 (DCR). Aberto por
	// desenho — é o que "dynamic" quer dizer —, com teto por origem e por hora.
	RotaRegistrar = "/oauth/register"
)

// MetadataServidor é o documento de metadata do authorization server
// (RFC 8414 §2), na forma que o patchbay publica.
//
// É struct própria e não oauthex.AuthServerMeta porque a do SDK marca jwks_uri
// sem omitempty — e um "jwks_uri": "" num documento público é um campo que
// promete um JWKS que não existe. O access token deste AS é opaco: não há
// chave pública a publicar. O teste de metadata decodifica o que sai daqui
// dentro de oauthex.AuthServerMeta, que é o que garante que os nomes de campo
// continuam sendo os que o cliente lê.
//
// client_id_metadata_document_supported e registration_endpoint são a fatia 11.
// O claude.ai só escolhe CIMD se enxergar o primeiro como true *e* "none" em
// token_endpoint_auth_methods_supported; faltando um dos dois, ele cai para DCR
// e registra um cliente novo a cada conexão fresca. Os dois caminhos ficam
// anunciados de propósito — CIMD porque é o que a spec 2026-07-28 quer, DCR
// porque há cliente que só tem ele.
type MetadataServidor struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RevocationEndpoint                         string   `json:"revocation_endpoint,omitempty"`
	RegistrationEndpoint                       string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                            []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	ResponseModesSupported                     []string `json:"response_modes_supported,omitempty"`
	GrantTypesSupported                        []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	RevocationEndpointAuthMethodsSupported     []string `json:"revocation_endpoint_auth_methods_supported,omitempty"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported,omitempty"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported,omitempty"`
	ServiceDocumentation                       string   `json:"service_documentation,omitempty"`
}

// metodosAutenticacaoCliente são os métodos aceitos no token e no revocation
// endpoint.
//
// "none" precisa estar na lista: o cliente CIMD do claude.ai autentica como
// cliente público, e sem esse valor anunciado o Claude nem tenta CIMD. Os dois
// client_secret_* existem para o cliente confidencial cadastrado pela UI.
var metodosAutenticacaoCliente = []string{"none", "client_secret_post", "client_secret_basic"}

// Metadata monta o documento RFC 8414 com os escopos dos endpoints existentes.
func (s *Servico) Metadata(escopos []string) MetadataServidor {
	return MetadataServidor{
		Issuer:                s.urlPublica,
		AuthorizationEndpoint: s.urlPublica + RotaAutorizar,
		TokenEndpoint:         s.urlPublica + RotaToken,
		RevocationEndpoint:    s.urlPublica + RotaRevogar,
		RegistrationEndpoint:  s.urlPublica + RotaRegistrar,
		ScopesSupported:       escopos,
		// O par que decide entre CIMD e DCR no claude.ai: este campo true e
		// "none" em token_endpoint_auth_methods_supported, que
		// metodosAutenticacaoCliente já traz.
		ClientIDMetadataDocumentSupported: s.cimd != nil,
		// Só code: o OAuth 2.1 remove implicit, e client_credentials não serve
		// ao Claude, que exige consentimento de um usuário.
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:      metodosAutenticacaoCliente,
		RevocationEndpointAuthMethodsSupported: metodosAutenticacaoCliente,
		// S256 e nada mais. "plain" não é PKCE, é teatro: o desafio viaja igual
		// ao verificador. O claude.ai exige ver exatamente esta lista.
		CodeChallengeMethodsSupported: []string{"S256"},
		// RFC 9207: o iss volta na resposta de autorização e o cliente valida.
		// Passou de opcional a SHOULD em 2026-07-28, contra mix-up attack.
		AuthorizationResponseIssParameterSupported: true,
	}
}

// MetadataRecurso monta a metadata RFC 9728 de um endpoint.
//
// authorization_servers tem uma entrada só, de propósito: o Claude usa apenas a
// primeira e não faz fallback, então uma segunda entrada seria decoração que
// ninguém lê — e que daria a impressão errada de redundância.
func (s *Servico) MetadataRecurso(e EndpointRef) *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               s.Recurso(e.Slug),
		AuthorizationServers:   []string{s.urlPublica},
		ScopesSupported:        []string{s.escopo(e.Slug)},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "patchbay · " + e.Rotulo(),
	}
}

// MetadataRecursoRaiz monta o fallback do RFC 9728 na raiz.
//
// SUPOSIÇÃO: o estudo prévio pede o fallback na raiz mas não detalha o corpo do
// documento. Sem um endpoint específico para nomear, o resource aponta a
// própria base pública do patchbay — e não o mcp/{slug} de um endpoint — e
// scopes_supported agrega o escopo de todos os endpoints existentes. É o
// mesmo formato do RFC 9728, só que genérico: quem bate aqui ainda não sabe
// qual endpoint quer.
func (s *Servico) MetadataRecursoRaiz(escopos []string) *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               s.urlPublica,
		AuthorizationServers:   []string{s.urlPublica},
		ScopesSupported:        escopos,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "patchbay",
	}
}
