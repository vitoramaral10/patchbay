package apikey

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Repositorio é o que o Servico precisa da persistência. A interface é
// declarada aqui, no consumidor, e implementada por RepositorioSQLite.
type Repositorio interface {
	// PorHash devolve a chave cujo hash de armazenamento é hash.
	// Devolve ErrNaoEncontrada quando não existe.
	PorHash(ctx context.Context, hash string) (Chave, error)
	// RegistrarUso grava o instante do último uso de uma chave.
	RegistrarUso(ctx context.Context, id int64, quando time.Time) error
}

// Servico verifica a chave apresentada por um cliente.
type Servico struct {
	repo  Repositorio
	log   *slog.Logger
	agora func() time.Time
	usos  chan registroUso
}

type registroUso struct {
	id     int64
	quando time.Time
}

// capacidadeUsos é o tamanho da fila de "último uso". Cheia, a fila descarta:
// gravar o uso é telemetria, e telemetria não pode segurar o caminho da
// requisição nem competir pelo único escritor do SQLite.
const capacidadeUsos = 256

// NovoServico monta o serviço de verificação.
func NovoServico(repo Repositorio, log *slog.Logger) *Servico {
	return &Servico{
		repo:  repo,
		log:   log,
		agora: time.Now,
		usos:  make(chan registroUso, capacidadeUsos),
	}
}

// Verificar tem a assinatura de auth.TokenVerifier do go-sdk e é o que se
// entrega a auth.RequireBearerToken.
//
// O escopo devolvido é um por endpoint que a chave alcança; a decisão de
// permitir ou recusar o endpoint da requisição é do middleware, que compara
// esses escopos com o escopo exigido — e é dele que sai o 403.
func (s *Servico) Verificar(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if _, err := PrefixoVisivelDe(token); err != nil {
		// Formato errado não distingue de chave inexistente para quem chama:
		// as duas respostas são 401.
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, ErrFormato)
	}

	chave, err := s.repo.PorHash(ctx, Hash(token))
	switch {
	case errors.Is(err, ErrNaoEncontrada):
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, ErrNaoEncontrada)
	case err != nil:
		// Falha de infraestrutura vira 500, não 401: negar acesso por causa de
		// um disco cheio esconderia o problema real. O middleware do go-sdk
		// escreve err.Error() no corpo da resposta (auth/auth.go:117), então o
		// detalhe fica no log e o cliente recebe texto genérico.
		s.log.Error("falha ao consultar chave de api", "erro", err)
		return nil, errInterno
	case chave.Revogada():
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, ErrRevogada)
	}

	s.marcarUso(chave.ID)

	return &auth.TokenInfo{
		Scopes: Escopos(chave.Endpoints),
		UserID: fmt.Sprintf("apikey:%d", chave.ID),
		// Sem expiração: a chave vale até ser revogada. Por isso
		// RequireBearerTokenOptions.AllowMissingExpiration é obrigatório
		// aqui — sem ela o middleware recusa toda chave com 401.
	}, nil
}

// errInterno é o que o cliente vê quando a falha é do patchbay, não da chave.
var errInterno = errors.New("erro interno ao verificar a credencial")

// marcarUso enfileira a gravação do último uso, sem bloquear.
func (s *Servico) marcarUso(id int64) {
	select {
	case s.usos <- registroUso{id: id, quando: s.agora()}:
	default:
		s.log.Debug("fila de último uso cheia, registro descartado", "api_key_id", id)
	}
}

// GravarUsos drena a fila de "último uso" até ctx ser cancelado.
//
// É a única escrita que o caminho da requisição provoca, e ela acontece fora
// dele: quem chama é dono da goroutine e o cancelamento do ctx a encerra.
func (s *Servico) GravarUsos(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-s.usos:
			if err := s.repo.RegistrarUso(ctx, u.id, u.quando); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Warn("não gravou último uso da chave", "api_key_id", u.id, "erro", err)
			}
		}
	}
}
