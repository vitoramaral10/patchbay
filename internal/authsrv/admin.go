package authsrv

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TipoPrereg é o registro feito à mão pela UI. É caminho de primeira classe e
// não fallback de erro: CIMD e DCR são a fatia 11, e mesmo depois dela vai
// existir cliente que só tem credencial colada à mão.
const TipoPrereg = "prereg"

var base32Cliente = base32.StdEncoding.WithPadding(base32.NoPadding)

// FormCliente é o formulário de cadastro de cliente OAuth.
//
// Só criação: nem o escopo nem a allowlist de redirect se editam. Mudar
// qualquer um dos dois muda em silêncio o que um cliente lá fora alcança;
// revogar e cadastrar outro deixa a mudança visível dos dois lados.
type FormCliente struct {
	Nome string
	// RedirectTexto é o textarea cru, uma URI por linha, guardado para reexibir
	// exatamente o que a pessoa digitou quando a validação recusa.
	RedirectTexto string
	RedirectURIs  []string
	Confidencial  bool
	EndpointIDs   []int64
	Erros         map[string]string
}

// Validar preenche Erros e informa se o formulário passa.
func (f *FormCliente) Validar() bool {
	f.Erros = map[string]string{}
	f.Nome = strings.TrimSpace(f.Nome)

	if f.Nome == "" {
		f.Erros["nome"] = "Dê um nome ao cliente: é como você vai saber qual revogar."
	}

	f.RedirectURIs = nil
	for _, linha := range strings.Split(f.RedirectTexto, "\n") {
		uri := strings.TrimSpace(linha)
		if uri == "" {
			continue
		}
		if motivo := motivoRedirectInvalido(uri); motivo != "" {
			f.Erros["redirect"] = uri + ": " + motivo
			continue
		}
		f.RedirectURIs = append(f.RedirectURIs, uri)
	}
	if len(f.RedirectURIs) == 0 && f.Erros["redirect"] == "" {
		f.Erros["redirect"] = "Informe ao menos uma redirect_uri. A comparação é exata, caractere a caractere " +
			"— só um cliente registrado por DCR ou por CIMD tem a porta do loopback livre."
	}
	if len(f.EndpointIDs) == 0 {
		f.Erros["endpoint"] = "Escolha ao menos um endpoint. Cliente sem escopo não consegue pedir token nenhum."
	}
	return len(f.Erros) == 0
}

// motivoRedirectInvalido devolve o motivo da recusa, ou vazio se a URI serve.
//
// As regras são as do RFC 6749 §3.1.2 mais o RFC 8252 §7.3: absoluta, sem
// fragmento, e http só em loopback — o único caso em que o segredo não viaja em
// claro pela rede é a máquina do próprio usuário.
func motivoRedirectInvalido(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return "não é uma URI válida"
	}
	switch {
	case u.Scheme == "":
		return "precisa ser absoluta, com esquema"
	case u.Fragment != "" || strings.Contains(uri, "#"):
		return "não pode ter fragmento"
	case u.Scheme == "https":
		return ""
	case u.Scheme == "http":
		if ehLoopback(u.Hostname()) {
			return ""
		}
		return "http só é aceito em loopback (127.0.0.1, [::1] ou localhost)"
	default:
		// Esquema próprio de app nativo (com.exemplo.app:/callback) é legítimo
		// no RFC 8252, e recusá-lo fecharia a porta para cliente desktop.
		if strings.Contains(u.Scheme, ".") {
			return ""
		}
		return "esquema não aceito: use https, http em loopback, ou um esquema próprio com ponto"
	}
}

func ehLoopback(host string) bool {
	// EqualFold porque hostname é case-insensitive (localhost, LocalHost e
	// LOCALHOST são o mesmo host); os dois literais de IP não têm letra que
	// mude de caixa, então a comparação exata neles já basta.
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// EndpointOpcao é um endpoint oferecido no escopo de um cliente.
type EndpointOpcao struct {
	EndpointRef
	Escolhido bool
}

// ClienteCriado é o resultado do cadastro.
//
// Segredo só existe nesta tela, como a chave de API: ele é guardado por hash, e
// prometer reexibição obrigaria a guardar reversível — um cofre de credencial
// sem necessidade (seção 08.8).
type ClienteCriado struct {
	Cliente
	Segredo string
}

// Sessao é uma família de tokens vista da tela: o que o admin chama de "sessão"
// daquele cliente.
//
// A família é a unidade certa de revogação porque é a unidade da rotação: matar
// um refresh sozinho deixaria o access vivo por até uma hora, e o cliente
// tentaria renovar com um token que o AS trata como replay.
type Sessao struct {
	FamiliaID    string
	EndpointSlug string
	Resource     string
	CriadaEm     time.Time
	ExpiraEm     time.Time
	UltimoUsoEm  time.Time
	Ativos       int
	Total        int
}

// Ativa informa se a família ainda tem algum token válido.
func (s Sessao) Ativa() bool { return s.Ativos > 0 }

// Curta é o identificador da família como ele aparece na tela: doze caracteres
// bastam para o admin distinguir uma linha da outra.
func (s Sessao) Curta() string {
	if len(s.FamiliaID) <= 12 {
		return s.FamiliaID
	}
	return s.FamiliaID[:12]
}

// gerarClientID sorteia o identificador público de um cliente.
//
// Público e não secreto: ele viaja na query do authorize e aparece no log de
// qualquer proxy. O que ele precisa ser é não-adivinhável o bastante para não
// dar a ninguém a lista de clientes cadastrados.
func gerarClientID() (string, error) {
	bruto := make([]byte, 10)
	if _, err := rand.Read(bruto); err != nil {
		return "", fmt.Errorf("authsrv: sortear client_id: %w", err)
	}
	return "pbc_" + strings.ToLower(base32Cliente.EncodeToString(bruto)), nil
}

// gerarSegredo sorteia o segredo de um cliente confidencial e devolve o claro,
// o prefixo visível e o hash de armazenamento.
func gerarSegredo() (claro, prefixo, hash string, err error) {
	ident := make([]byte, bytesIdentificador)
	if _, err := rand.Read(ident); err != nil {
		return "", "", "", fmt.Errorf("authsrv: sortear identificador de segredo: %w", err)
	}
	bruto, err := sortear(MarcaSegredo)
	if err != nil {
		return "", "", "", err
	}
	prefixo = MarcaSegredo + "_" + strings.ToLower(base32Cliente.EncodeToString(ident))
	// O prefixo entra no próprio segredo para que o que a UI mostra depois seja
	// literalmente o começo do que o cliente guardou.
	claro = prefixo + "_" + strings.TrimPrefix(bruto, MarcaSegredo+"_")
	return claro, prefixo, Hash(claro), nil
}
