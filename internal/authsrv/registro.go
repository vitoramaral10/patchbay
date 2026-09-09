package authsrv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Códigos de erro do RFC 7591 §3.2.2, que são os únicos que o registration
// endpoint pode devolver além dos do RFC 6749.
const (
	ErroInvalidRedirectURI    = "invalid_redirect_uri"
	ErroInvalidClientMetadata = "invalid_client_metadata"
)

// Tetos do registro dinâmico.
//
// O DCR é um endpoint de escrita aberto: qualquer um na internet pode criar uma
// linha em oauth_client. Sem teto, a tabela cresce até o disco acabar, e a lista
// da UI vira ruído em que o cliente de verdade não se acha. Os números são
// generosos para uso legítimo — um cliente registra uma vez e guarda o
// client_id — e mesquinhos para inflação.
const (
	// LimiteRegistroPorMinuto é o balde de fichas por IP no /oauth/register.
	//
	// Maior que o teto por hora de propósito: quem gasta ficha aqui inclui o
	// cliente cujos metadados foram recusados, e recusa não cria linha. Apertar
	// este número até o teto por hora faria um cliente com um campo errado gastar
	// a cota de registro que ele nunca usou.
	LimiteRegistroPorMinuto = 12
	// TetoRegistroPorOrigem é quantos registros uma mesma origem consegue criar
	// por hora.
	TetoRegistroPorOrigem = 5
	// TetoRegistroPorHora é o teto agregado, de todas as origens somadas. Existe
	// porque o teto por origem sozinho só obriga o atacante a variar de IP.
	TetoRegistroPorHora = 40
	// JanelaRegistro é a janela dos dois tetos acima.
	JanelaRegistro = time.Hour
)

// Limites de forma dos metadados de cliente, valendo igual para DCR e para
// CIMD: os dois carregam a mesma struct, vinda de fora, e nenhum dos dois campos
// tem tamanho definido pelo RFC 7591.
const (
	// MaximoRedirectsPorCliente é quantas redirect_uri um cliente declara. O
	// Claude Code declara duas (localhost e 127.0.0.1); oito cobre folgado
	// qualquer cliente legítimo.
	MaximoRedirectsPorCliente = 8
	// tamanhoMaximoNome corta o client_name, que é texto de terceiro exibido na
	// tela de consentimento e na lista de clientes.
	tamanhoMaximoNome = 120
)

// ErrRegistroExcedido indica teto de registros dinâmicos atingido.
var ErrRegistroExcedido = errors.New("authsrv: teto de registros dinâmicos atingido na janela")

// grantsSuportados e responsesSuportados são o que este AS implementa. Um
// cliente que peça outra coisa é recusado no registro em vez de descobrir na
// hora do fluxo que o grant não existe.
var (
	grantsSuportados     = []string{"authorization_code", "refresh_token"}
	responsesSuportados  = []string{"code"}
	autenticacoesCliente = []string{"none", "client_secret_post", "client_secret_basic"}
)

// ClienteRegistrado é o resultado de um registro dinâmico: o cliente gravado
// mais o segredo em claro, quando houve.
type ClienteRegistrado struct {
	Cliente
	// Segredo só existe nesta resposta. O que fica no banco é o hash — a mesma
	// decisão do cliente cadastrado pela UI e da chave de API, e a razão pela
	// qual o RFC 7591 §3.2.1 não promete reexibição.
	Segredo string
}

// Registrar atende o Dynamic Client Registration do RFC 7591.
//
// O DCR está deprecado na spec MCP 2026-07-28 em favor de CIMD, e continua
// aqui porque a remoção mais cedo possível é a primeira revisão publicada em ou
// depois de 2027-07-28 — e porque há cliente que só sabe fazer DCR.
//
// origem é o IP de quem pediu, e serve de chave dos tetos e de coluna na tela.
func (s *Servico) Registrar(
	ctx context.Context, meta oauthex.ClientRegistrationMetadata, origem string,
) (ClienteRegistrado, error) {
	agora := s.agora()

	if err := validarMetadados(&meta); err != nil {
		return ClienteRegistrado{}, err
	}

	clientID, err := gerarClientID()
	if err != nil {
		return ClienteRegistrado{}, erroInterno(err)
	}

	// Confidencial só quando o cliente pediu autenticação com segredo. O padrão
	// do RFC 7591 §2 é client_secret_basic quando o campo vem vazio, e é ele
	// que vale — inventar "none" no lugar do padrão faria o cliente montar um
	// Basic que o token endpoint recusaria.
	metodo := meta.TokenEndpointAuthMethod
	if metodo == "" {
		metodo = "client_secret_basic"
	}
	var claro, prefixo, hash string
	if metodo != "none" {
		if claro, prefixo, hash, err = gerarSegredo(); err != nil {
			return ClienteRegistrado{}, erroInterno(err)
		}
	}

	nome := strings.TrimSpace(meta.ClientName)
	if nome == "" {
		nome = "cliente registrado por DCR"
	}
	// O teto é conferido dentro da mesma transação da gravação (ver o doc de
	// RegistrarClienteDinamico): gerar a credencial antes de saber se o teto
	// passa é barato, é só entropia local, e é o preço de fechar a corrida
	// entre contar e gravar.
	cliente, err := s.repo.RegistrarClienteDinamico(ctx, ClienteDinamico{
		ClientID:       clientID,
		Nome:           recortar(nome, tamanhoMaximoNome),
		Tipo:           TipoDCR,
		Confidencial:   metodo != "none",
		SegredoHash:    hash,
		SegredoPrefixo: prefixo,
		RedirectURIs:   meta.RedirectURIs,
		Origem:         origem,
		CriadoEm:       agora,
	}, origem, agora.Add(-JanelaRegistro), TetoRegistroPorOrigem, TetoRegistroPorHora)
	switch {
	case errors.Is(err, ErrRegistroExcedido):
		s.log.Warn("registro dinâmico recusado por teto", "origem", origem)
		return ClienteRegistrado{}, &ErroOAuth{
			Codigo:    ErroInvalidClientMetadata,
			Descricao: "registros demais nesta janela; tente de novo mais tarde",
			Status:    http.StatusTooManyRequests,
			Causa:     ErrRegistroExcedido,
		}
	case err != nil:
		return ClienteRegistrado{}, erroInterno(err)
	}

	// O segredo não entra no log nem em Debug: a tela de log é a via mais fácil
	// de vazar exatamente o que o hash em repouso protege.
	s.log.Info("cliente registrado por dcr",
		"client_id", cliente.ClientID, "nome", cliente.Nome,
		"confidencial", cliente.Confidencial, "origem", origem,
		"redirect_uris", len(cliente.RedirectURIs))

	return ClienteRegistrado{Cliente: cliente, Segredo: claro}, nil
}

// MetodoAutenticacao devolve o token_endpoint_auth_method efetivo do cliente,
// que é o que a resposta do registro precisa ecoar: o cliente do go-sdk decide
// como autenticar no token endpoint a partir dele
// (auth/authorization_code.go:502).
func (c Cliente) MetodoAutenticacao() string {
	if c.Confidencial {
		return "client_secret_basic"
	}
	return "none"
}

// RespostaRegistro monta o corpo do RFC 7591 §3.2.1.
//
// A struct é a do go-sdk, e não uma cópia: é ela que o cliente decodifica, e
// reusá-la é o que garante que os nomes de campo continuam sendo os que ele lê.
func (c ClienteRegistrado) RespostaRegistro() *oauthex.ClientRegistrationResponse {
	return &oauthex.ClientRegistrationResponse{
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{
			RedirectURIs:            c.RedirectURIs,
			TokenEndpointAuthMethod: c.MetodoAutenticacao(),
			GrantTypes:              slices.Clone(grantsSuportados),
			ResponseTypes:           slices.Clone(responsesSuportados),
			ClientName:              c.Nome,
		},
		ClientID:         c.ClientID,
		ClientSecret:     c.Segredo,
		ClientIDIssuedAt: c.CriadoEm,
		// Segredo sem expiração: quem o rotaciona é o admin, revogando o cliente
		// na tela. Prometer uma expiração que nada implementa seria pior.
	}
}

// validarMetadados confere o que vale igual para DCR e para CIMD e normaliza o
// que passa.
func validarMetadados(meta *oauthex.ClientRegistrationMetadata) error {
	switch {
	case len(meta.RedirectURIs) == 0:
		return &ErroOAuth{
			Codigo:    ErroInvalidRedirectURI,
			Descricao: "redirect_uris é obrigatório e não pode ser vazio",
			Status:    http.StatusBadRequest,
		}
	case len(meta.RedirectURIs) > MaximoRedirectsPorCliente:
		return &ErroOAuth{
			Codigo: ErroInvalidRedirectURI,
			Descricao: fmt.Sprintf("redirect_uris passa do teto de %d entradas",
				MaximoRedirectsPorCliente),
			Status: http.StatusBadRequest,
		}
	}
	for _, uri := range meta.RedirectURIs {
		if motivo := motivoRedirectInvalido(uri); motivo != "" {
			return &ErroOAuth{
				Codigo:    ErroInvalidRedirectURI,
				Descricao: "redirect_uri " + uri + ": " + motivo,
				Status:    http.StatusBadRequest,
			}
		}
	}
	if err := conferirGrants(meta.GrantTypes, meta.ResponseTypes); err != nil {
		return &ErroOAuth{
			Codigo: ErroInvalidClientMetadata, Descricao: err.Error(),
			Status: http.StatusBadRequest,
		}
	}
	if m := meta.TokenEndpointAuthMethod; m != "" && !slices.Contains(autenticacoesCliente, m) {
		return &ErroOAuth{
			Codigo: ErroInvalidClientMetadata,
			Descricao: "token_endpoint_auth_method " + m + " não é suportado; use " +
				strings.Join(autenticacoesCliente, ", "),
			Status: http.StatusBadRequest,
		}
	}
	return nil
}

// conferirGrants recusa cliente que declare grant ou response_type que este AS
// não implementa.
//
// Campo vazio é o padrão do RFC 7591 §2 — authorization_code e code —, e o
// padrão é justamente o que este AS suporta.
func conferirGrants(grants, responses []string) error {
	for _, g := range grants {
		if !slices.Contains(grantsSuportados, g) {
			return fmt.Errorf("grant_type %s não é suportado; só %s",
				g, strings.Join(grantsSuportados, " e "))
		}
	}
	for _, r := range responses {
		if !slices.Contains(responsesSuportados, r) {
			return fmt.Errorf("response_type %s não é suportado; só code", r)
		}
	}
	return nil
}

// recortar corta um texto de terceiro no limite, em fronteira de rune.
func recortar(texto string, limite int) string {
	if len(texto) <= limite {
		return texto
	}
	corte := limite
	for corte > 0 && !utf8Inicio(texto[corte]) {
		corte--
	}
	return texto[:corte]
}

// utf8Inicio informa se o byte começa um rune — todo byte de continuação de
// UTF-8 tem os dois bits mais significativos em 10.
func utf8Inicio(b byte) bool { return b&0xC0 != 0x80 }
