package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// AutorizarSemDesafio roda o fluxo de autorização por conta própria quando o
// admin pediu consentimento e ainda não existe token vivo.
//
// O caminho normal do go-sdk só autoriza quando o upstream responde 401: é o
// Authorize do handler, chamado pelo transporte ao tomar o desafio. Há servidor
// que não desafia no handshake — o MCP do Google Drive responde initialize e
// tools/list sem credencial e só recusa no tools/call, dentro do resultado. Com
// ele o 401 nunca chega, o Authorize nunca roda, a URL de consentimento nunca é
// montada, e o clique em "Autorizar" espera até o prazo e desiste.
//
// Aqui o Authorize é chamado com um desafio sintético, sem WWW-Authenticate: o
// SDK então descobre o authorization server pela metadata de recurso protegido
// (RFC 9728) da própria URL do upstream, exatamente como faria depois de um 401
// de verdade. Tudo o mais — registro de cliente, PKCE, fetcher do código, troca
// por token e gravação da concessão — é o mesmo fluxo.
//
// Só roda com pedido do admin em curso: sem ele não há navegador esperando a URL,
// e disparar a descoberta sozinho seria requisição ao provedor sem motivo.
func (b *BrokerOAuth) AutorizarSemDesafio(ctx context.Context, cfg Config) error {
	if cfg.Modo != ModoOAuth || !b.ConsentimentoPedido(cfg.ID) {
		return nil
	}
	s := b.sessao(cfg)
	s.mu.Lock()
	fonte := s.fonte
	s.mu.Unlock()
	if fonte != nil && !fonte.Morreu() {
		return nil
	}

	h, err := b.Autorizacao(ctx, cfg)
	if err != nil {
		return err
	}
	handler, ok := h.(*auth.AuthorizationCodeHandler)
	if !ok {
		return fmt.Errorf("upstream %s: handler de OAuth inesperado %T", cfg.Nome, h)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("upstream %s: montar requisição de autorização: %w", cfg.Nome, err)
	}
	desafio := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
	if err := handler.Authorize(ctx, req, desafio); err != nil {
		return fmt.Errorf("upstream %s: autorizar sem desafio: %w", cfg.Nome, err)
	}
	return nil
}
