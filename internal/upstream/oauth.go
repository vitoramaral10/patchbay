package upstream

import (
	"context"
	"errors"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// Modos de credencial de um upstream que fala HTTP (Streamable ou SSE).
//
// São excludentes: no modo oauth o Authorization é montado pelo token da
// concessão, e um bearer estático gravado ao lado só produziria dois headers de
// autorização com o servidor escolhendo um em silêncio.
const (
	// ModoEstatica é bearer e headers colados pelo admin, sem consentimento.
	ModoEstatica = "estatica"
	// ModoOAuth é authorization code com PKCE contra o AS do upstream.
	ModoOAuth = "oauth"
)

// Formas de registro de cliente OAuth, na ordem em que o go-sdk as tenta:
// Client ID Metadata Document, cliente pré-registrado, Dynamic Client
// Registration (auth/authorization_code.go:526).
//
// Guardar por qual delas o client_id veio não é luxo de auditoria: "de onde saiu
// este client_id" é a primeira pergunta de todo diagnóstico de OAuth, e ela
// deixa de ter resposta no log assim que o processo reinicia.
const (
	RegistroCIMD          = "cimd"
	RegistroPreRegistrado = "preregistrado"
	RegistroDCR           = "dcr"
)

// Erros sentinela do OAuth de upstream.
var (
	// ErrSemConsentimento indica upstream OAuth que não tem token utilizável e
	// depende de um clique do admin para conseguir um. É o erro que leva a
	// EstadoSemConsentimento.
	ErrSemConsentimento = errors.New("upstream: sem consentimento OAuth")
	// ErrConsentimentoDesconhecido indica callback com state que não pertence a
	// nenhuma tentativa em curso — ou que já foi consumido. State é de uso
	// único.
	ErrConsentimentoDesconhecido = errors.New("upstream: consentimento não encontrado ou já usado")
	// ErrConsentimentoRecusado indica que o AS devolveu erro no callback, em vez
	// de um code (access_denied, quando o admin clica em negar).
	ErrConsentimentoRecusado = errors.New("upstream: consentimento recusado no provedor")
	// ErrNaoEhOAuth indica operação de consentimento pedida a um upstream que
	// não está no modo oauth.
	ErrNaoEhOAuth = errors.New("upstream: upstream não usa OAuth")
	// ErrConsentimentoDemorou indica que a URL de autorização não ficou pronta
	// no prazo que a requisição do admin espera.
	ErrConsentimentoDemorou = errors.New("upstream: autorização não ficou pronta no prazo")
)

// Prazos do consentimento.
const (
	// TempoDeConsentimentoPadrao é quanto a tentativa de conexão espera o admin
	// concluir o consentimento no provedor.
	//
	// Minutos e não segundos porque o que está sendo esperado é uma pessoa
	// escolhendo uma conta, digitando uma senha e lendo uma tela de permissões.
	TempoDeConsentimentoPadrao = 10 * time.Minute
	// EsperaURLAutorizacaoPadrao é quanto o clique em "Autorizar" espera a URL
	// do provedor ficar pronta.
	//
	// Ela só existe depois de a supervisão reconectar, tomar o 401 e completar a
	// descoberta RFC 9728/8414 (e, no caminho do DCR, registrar o cliente).
	// Segundos, portanto — e o fim do prazo é um aviso na tela pedindo para
	// clicar de novo, nunca um erro sem explicação.
	EsperaURLAutorizacaoPadrao = 20 * time.Second
	// MargemDeRenovacaoPadrao é com quanta antecedência a supervisão renova o
	// token.
	//
	// A renovação é proativa e roda na supervisão porque refresh no caminho da
	// requisição do cliente é justamente o que a decisão 4 do estudo proíbe: o
	// transporte pede o token a cada requisição de saída, e se ele estiver
	// vencido nessa hora o tools/call do cliente paga a ida ao token endpoint.
	MargemDeRenovacaoPadrao = 2 * time.Minute
	// IntervaloDeRenovacaoPadrao é de quanto em quanto tempo a supervisão
	// verifica se o token está perto de expirar.
	IntervaloDeRenovacaoPadrao = 30 * time.Second
)

// Token é o token OAuth de um upstream como ele é persistido.
//
// Os dois campos de segredo são cripto.Segredo: eles nunca podem chegar ao log
// nem a uma mensagem de erro, e o tipo garante isso mesmo quando alguém formata
// a struct inteira com %v. O JSON, por outro lado, sai com o valor em claro —
// e é justamente por isso que ele nunca é gravado: cada campo vai para a sua
// coluna cifrada.
type Token struct {
	Acesso  cripto.Segredo
	Refresh cripto.Segredo
	// Tipo é o token_type do provedor, normalmente "Bearer".
	Tipo string
	// Expira é quando o access token vence. Zero é "sem prazo declarado".
	Expira time.Time
}

// Valido informa se há access token.
func (t Token) Valido() bool { return !t.Acesso.Vazio() }

// PertoDeExpirar informa se falta menos que margem para o token vencer.
//
// Token sem prazo declarado nunca está perto de expirar: o provedor não disse
// nada, e inventar um prazo faria o patchbay pedir refresh a cada verificação.
func (t Token) PertoDeExpirar(agora time.Time, margem time.Duration) bool {
	if t.Expira.IsZero() {
		return false
	}
	return !agora.Add(margem).Before(t.Expira)
}

// ClienteOAuth é o cliente pré-registrado que o admin colou no formulário.
//
// Pré-registro é caminho de primeira classe e não recuperação de erro (decisão
// 12 do estudo): o Google não anuncia registration_endpoint nem
// client_id_metadata_document_supported, então sem este campo ele simplesmente
// não funciona.
type ClienteOAuth struct {
	ClientID string
	Segredo  cripto.Segredo
	// Issuer é o issuer do AS a que estas credenciais pertencem. Vazio desliga a
	// conferência; preenchido, o SDK recusa usá-las com outro AS (SEP-2352).
	Issuer string
}

// Definido informa se há cliente pré-registrado configurado.
func (c ClienteOAuth) Definido() bool { return c.ClientID != "" }

// Concessao é o resultado de um consentimento concluído: o token mais o mínimo
// para renová-lo sem refazer a descoberta.
//
// url_token, estilo e escopos entram aqui porque o token endpoint só é conhecido
// depois do RFC 8414, e refazer a descoberta exigiria um novo 401 do upstream —
// o que empurraria o refresh de volta para o caminho da requisição do cliente.
type Concessao struct {
	// ClientIDEfetivo é o client_id que o consentimento usou de fato.
	ClientIDEfetivo string
	// Registro é por qual dos três caminhos ele veio.
	Registro string
	URLToken string
	// Estilo é o oauth2.AuthStyle escolhido pelo SDK a partir do
	// token_endpoint_auth_methods_supported do AS.
	Estilo  int
	Escopos []string
	Token   Token
	// RefreshEm é quando foi o último refresh bem-sucedido. Zero no
	// consentimento inicial.
	RefreshEm time.Time
}

// EstadoOAuth é o que a tela mostra de OAuth sem decifrar nada.
//
// Nenhum campo aqui é segredo, de propósito: a tela de um upstream tem que
// continuar abrindo depois de uma troca de chave mestra, e é durante esse
// diagnóstico que ela é mais necessária.
type EstadoOAuth struct {
	// ClientID é o informado pelo admin. Vazio significa que a ordem do SDK
	// (CIMD → pré-registrado → DCR) vai cair em CIMD ou DCR.
	ClientID string
	// SegredoDefinido diz se há client_secret gravado, sem revelá-lo.
	SegredoDefinido bool
	Issuer          string
	// ClientIDEfetivo e Registro descrevem a concessão em vigor.
	ClientIDEfetivo string
	Registro        string
	// Consentido diz se existe access token gravado.
	Consentido bool
	ExpiraEm   time.Time
	RefreshEm  time.Time
}

// RotuloDoRegistro traduz o registro no texto da tela.
func RotuloDoRegistro(registro string) string {
	switch registro {
	case RegistroCIMD:
		return "Client ID Metadata Document"
	case RegistroPreRegistrado:
		return "cliente pré-registrado"
	case RegistroDCR:
		return "registro dinâmico (DCR)"
	default:
		return "—"
	}
}

// CofreOAuth é o mínimo que o broker de OAuth precisa do banco.
//
// Declarado aqui, no consumidor: o broker não sabe que existe SQLite, e o teste
// troca a implementação sem abrir arquivo nenhum. Quatro métodos porque são as
// quatro coisas que um consentimento faz com o disco — ler o cliente que o admin
// configurou, ler a concessão que existe, gravar a que acabou de nascer, e
// apagar a que o provedor revogou.
type CofreOAuth interface {
	ClienteOAuth(ctx context.Context, upstreamID int64) (ClienteOAuth, error)
	Concessao(ctx context.Context, upstreamID int64) (Concessao, bool, error)
	GravarConcessao(ctx context.Context, upstreamID int64, c Concessao) error
	ApagarConcessao(ctx context.Context, upstreamID int64) error
}
