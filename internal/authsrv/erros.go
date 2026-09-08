package authsrv

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Códigos de erro do RFC 6749 §4.1.2.1 e §5.2, mais o invalid_target do
// RFC 8707 §2.
//
// O texto exato importa: o claude.ai decide reautenticar a partir do código, e
// não da mensagem. Um refresh expirado que devolva "expired_token" em vez de
// invalid_grant faz o cliente desistir em vez de refazer o consentimento.
const (
	ErroInvalidRequest          = "invalid_request"
	ErroInvalidClient           = "invalid_client"
	ErroInvalidGrant            = "invalid_grant"
	ErroUnauthorizedClient      = "unauthorized_client"
	ErroUnsupportedGrantType    = "unsupported_grant_type"
	ErroUnsupportedResponseType = "unsupported_response_type"
	ErroInvalidScope            = "invalid_scope"
	ErroAccessDenied            = "access_denied"
	ErroServerError             = "server_error"
	ErroTemporarilyUnavailable  = "temporarily_unavailable"
	ErroInvalidTarget           = "invalid_target"
)

// ErroOAuth é o erro que chega ao cliente OAuth, na forma do RFC 6749.
//
// Status é o código HTTP do token endpoint; no authorize endpoint ele é
// ignorado, porque lá o erro viaja na query do redirect.
type ErroOAuth struct {
	Codigo    string
	Descricao string
	Status    int
	// Causa fica só no log. Nunca vai para a resposta: a descrição do RFC 6749
	// é para o desenvolvedor do cliente, não para o operador do AS, e detalhe
	// interno ali vira oráculo.
	Causa error
	// SemRedirect marca o erro que o authorize endpoint NÃO pode devolver por
	// redirecionamento (RFC 6749 §4.1.2.1): client_id desconhecido e
	// redirect_uri fora da allowlist. Redirecionar nesses dois casos é entregar
	// o erro — e, na variante com code, a própria credencial — a um destino que
	// o AS não reconhece.
	SemRedirect bool
}

func (e *ErroOAuth) Error() string {
	if e.Causa != nil {
		return fmt.Sprintf("authsrv: %s: %s: %v", e.Codigo, e.Descricao, e.Causa)
	}
	return fmt.Sprintf("authsrv: %s: %s", e.Codigo, e.Descricao)
}

// Unwrap expõe a causa a errors.Is/As sem colocá-la na resposta.
func (e *ErroOAuth) Unwrap() error { return e.Causa }

// erroOAuth monta um erro com status 400, que é o padrão do RFC 6749 §5.2.
func erroOAuth(codigo, descricao string) *ErroOAuth {
	return &ErroOAuth{Codigo: codigo, Descricao: descricao, Status: http.StatusBadRequest}
}

// erroInterno é a falha do patchbay, não do cliente: 500 e o detalhe só no log.
func erroInterno(causa error) *ErroOAuth {
	return &ErroOAuth{
		Codigo:    ErroServerError,
		Descricao: "falha interna do authorization server",
		Status:    http.StatusInternalServerError,
		Causa:     causa,
	}
}

// comoErroOAuth traduz qualquer erro no que o cliente deve ver.
func comoErroOAuth(err error) *ErroOAuth {
	var oerr *ErroOAuth
	if errors.As(err, &oerr) {
		return oerr
	}
	return erroInterno(err)
}

// respostaErro escreve o corpo JSON do RFC 6749 §5.2.
//
// Cache-Control: no-store em toda resposta do token endpoint, de erro
// inclusive: a resposta carrega credencial no caminho feliz e o estado do
// grant no caminho triste, e nenhum dos dois pode ficar em cache de proxy.
func respostaErro(w http.ResponseWriter, log *slog.Logger, err *ErroOAuth) {
	if err.Status >= http.StatusInternalServerError {
		log.Error("falha no authorization server", "erro_oauth", err.Codigo, "erro", err)
	} else {
		log.Debug("requisição oauth recusada", "erro_oauth", err.Codigo, "descricao", err.Descricao)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if err.Codigo == ErroInvalidClient {
		// RFC 6749 §5.2: 401 quando a autenticação do cliente falhou, com o
		// desafio correspondente ao esquema que ele tentou usar.
		w.Header().Set("WWW-Authenticate", `Basic realm="patchbay"`)
	}

	status := err.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)

	corpo := map[string]string{"error": err.Codigo}
	if err.Descricao != "" {
		corpo["error_description"] = err.Descricao
	}
	if erro := json.NewEncoder(w).Encode(corpo); erro != nil {
		log.Error("não escreveu o corpo do erro oauth", "erro", erro)
	}
}
