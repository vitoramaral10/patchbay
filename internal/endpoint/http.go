package endpoint

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Rota é o padrão de ServeMux servido por este pacote. O slug entra na URL e é
// contrato (seção 10).
const Rota = "/mcp/{endpoint}"

// SessionTimeoutPadrao recicla sessão ociosa. A sessão é retida (não
// Stateless), então o gateway carrega estado em memória e sessão que ninguém
// fecha vaza (decisão 14).
const SessionTimeoutPadrao = 30 * time.Minute

// parametrosProibidos são os nomes que um cliente tentaria usar para mandar a
// credencial na query string.
//
// Chave de API em query string vem desligada (seção 11): vaza em log de proxy,
// em histórico de navegador e em referer. Recusar com o motivo escrito é
// melhor que ignorar em silêncio — ignorar faz o cliente insistir.
var parametrosProibidos = []string{"api_key", "apikey", "key", "token", "access_token"}

// Autorizacao é o que este pacote precisa da credencial: verificar o token e
// saber qual escopo um endpoint exige.
//
// As duas metades vêm de fora porque quem emite a credencial é outra feature, e
// feature não importa feature: só main conhece o grafo.
type Autorizacao struct {
	// Verificar tem a assinatura de auth.TokenVerifier do go-sdk.
	Verificar auth.TokenVerifier
	// Escopo devolve o escopo que dá acesso a um endpoint.
	Escopo func(slug string) string
}

// novoHandler monta o StreamableHTTPHandler de um único endpoint.
//
// getServer devolve sempre o mesmo *mcp.Server porque este handler serve só este
// endpoint: é o que separa as tabelas de sessão de dois endpoints (ver o
// comentário do tipo vivo).
func (s *Servidores) novoHandler(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Logger:         s.log.With("componente", "streamable_http"),
			SessionTimeout: SessionTimeoutPadrao,
			// DisableLocalhostProtection fica no default (falso): requisição que
			// chega por endereço de loopback com Host não-loopback é recusada com
			// 403, que é a proteção contra DNS rebinding
			// (mcp/streamable.go:326-334).
		})
}

// handlerDoSlug devolve o handler MCP daquele endpoint.
func (s *Servidores) handlerDoSlug(slug string) (http.Handler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.porSlug[slug]
	if !ok {
		return nil, false
	}
	return v.handler, true
}

// Handler monta o handler HTTP de /mcp/{endpoint}.
//
// A cadeia é: resolve o endpoint do path → rejeita credencial na query string →
// auth.RequireBearerToken com o escopo daquele endpoint → o handler streamable
// daquele endpoint.
func (s *Servidores) Handler(autz Autorizacao, urlPublica string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("endpoint")
		streamable, ok := s.handlerDoSlug(slug)
		if !ok {
			http.Error(w, "endpoint não encontrado", http.StatusNotFound)
			return
		}
		if nome, achou := credencialNaQuery(r); achou {
			http.Error(w,
				"credencial em query string está desligada ("+nome+"): use o header Authorization: Bearer",
				http.StatusBadRequest)
			return
		}

		// O escopo exigido é o do endpoint da URL. O middleware do go-sdk
		// compara com os escopos que o verificador devolveu e é dele que sai o
		// 403 "insufficient scope" (auth/auth.go:153).
		middleware := auth.RequireBearerToken(autz.Verificar, &auth.RequireBearerTokenOptions{
			Scopes: []string{autz.Escopo(slug)},
			// A chave de API não expira: ela vale até ser revogada. Sem esta
			// opção o middleware recusa toda chave com "token missing
			// expiration" (auth/auth.go:164).
			AllowMissingExpiration: true,
			ResourceMetadataURL:    metadataDoRecurso(urlPublica, slug),
		})
		middleware(streamable).ServeHTTP(w, r)
	})
}

// metadataDoRecurso monta a URL da metadata RFC 9728 daquele endpoint.
//
// Cada endpoint é um protected resource distinto, com metadata no caminho com
// sufixo — é isso que faz o aud do token carregar o endpoint (seção 07). O
// handler que serve esse JSON é da fatia 10; aqui só o anúncio no
// WWW-Authenticate já existe, porque ele é o que diz ao cliente onde procurar.
func metadataDoRecurso(urlPublica, slug string) string {
	if urlPublica == "" {
		return ""
	}
	return urlPublica + "/.well-known/oauth-protected-resource/mcp/" + slug
}

func credencialNaQuery(r *http.Request) (string, bool) {
	q := r.URL.Query()
	for _, nome := range parametrosProibidos {
		if q.Has(nome) {
			return nome, true
		}
	}
	return "", false
}
