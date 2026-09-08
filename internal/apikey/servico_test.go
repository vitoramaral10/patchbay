package apikey_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/vitoramaral10/patchbay/internal/apikey"
)

// RepositorioFake é o dublê do repositório, escrito à mão sobre a interface
// pequena que o Servico declara.
type RepositorioFake struct {
	chave apikey.Chave
	err   error

	usos chan int64
}

func (r *RepositorioFake) PorHash(_ context.Context, _ string) (apikey.Chave, error) {
	if r.err != nil {
		return apikey.Chave{}, r.err
	}
	return r.chave, nil
}

func (r *RepositorioFake) RegistrarUso(_ context.Context, id int64, _ time.Time) error {
	if r.usos != nil {
		r.usos <- id
	}
	return nil
}

func logDescartado() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

const chaveDeTeste = "pbk_aaaabbbb_um-segredo-qualquer-com-tamanho-suficiente"

func TestServico_Verificar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		token       string
		chave       apikey.Chave
		erroRepo    error
		querEscopos []string
		// querInvalido diz se o erro tem que desaguar em 401 pelo middleware do
		// go-sdk, que é quem traduz auth.ErrInvalidToken.
		querInvalido bool
		querErro     bool
	}{
		"chave válida devolve um escopo por endpoint": {
			token:       chaveDeTeste,
			chave:       apikey.Chave{ID: 1, Nome: "dev", Endpoints: []string{"pessoal", "trabalho"}},
			querEscopos: []string{"endpoint:pessoal", "endpoint:trabalho"},
		},
		"chave sem endpoint nenhum não dá escopo nenhum": {
			token: chaveDeTeste,
			chave: apikey.Chave{ID: 2, Nome: "orfa"},
		},
		"chave inexistente é token inválido": {
			token:        chaveDeTeste,
			erroRepo:     apikey.ErrNaoEncontrada,
			querInvalido: true,
			querErro:     true,
		},
		"chave revogada é token inválido": {
			token: chaveDeTeste,
			chave: apikey.Chave{
				ID:         3,
				Endpoints:  []string{"pessoal"},
				RevogadaEm: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			querInvalido: true,
			querErro:     true,
		},
		"formato inválido é token inválido sem consultar o banco": {
			token:        "isso-nao-e-uma-chave",
			erroRepo:     errors.New("o repositório não devia ser consultado"),
			querInvalido: true,
			querErro:     true,
		},
		"falha de infraestrutura não vira token inválido": {
			token:    chaveDeTeste,
			erroRepo: errors.New("disco cheio"),
			querErro: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			repo := &RepositorioFake{chave: tc.chave, err: tc.erroRepo}
			sut := apikey.NovoServico(repo, logDescartado())

			info, err := sut.Verificar(context.Background(), tc.token, nil)

			if tc.querErro {
				if err == nil {
					t.Fatalf("erro = nil, quer erro")
				}
				if invalido := errors.Is(err, auth.ErrInvalidToken); invalido != tc.querInvalido {
					t.Fatalf("errors.Is(err, auth.ErrInvalidToken) = %v, quer %v (erro: %v)",
						invalido, tc.querInvalido, err)
				}
				if info != nil {
					t.Errorf("token info = %v, quer nil", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if !slices.Equal(info.Scopes, tc.querEscopos) {
				t.Errorf("escopos = %v, quer %v", info.Scopes, tc.querEscopos)
			}
			// UserID amarra a sessão retida a esta credencial: sem ele o SDK não
			// tem como recusar reuso de Mcp-Session-Id por outra chave
			// (mcp/streamable.go:567).
			if info.UserID == "" {
				t.Error("user id vazio, quer identificar a chave")
			}
			if !info.Expiration.IsZero() {
				t.Error("expiração preenchida; a chave de api vale até ser revogada")
			}
		})
	}
}

// TestServico_VerificarNoMiddleware exercita a tradução que o middleware do
// go-sdk faz: sem chave e chave inválida viram 401 com WWW-Authenticate, chave
// sem escopo para o endpoint vira 403, e chave com escopo passa.
func TestServico_VerificarNoMiddleware(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		autorizacao string
		endpoints   []string
		erroRepo    error
		querStatus  int
		querDesafio bool
	}{
		"sem header de autorização": {
			endpoints:   []string{"pessoal"},
			querStatus:  http.StatusUnauthorized,
			querDesafio: true,
		},
		"header sem o esquema bearer": {
			autorizacao: "Basic " + chaveDeTeste,
			endpoints:   []string{"pessoal"},
			querStatus:  http.StatusUnauthorized,
			querDesafio: true,
		},
		"chave inexistente": {
			autorizacao: "Bearer " + chaveDeTeste,
			erroRepo:    apikey.ErrNaoEncontrada,
			querStatus:  http.StatusUnauthorized,
			querDesafio: true,
		},
		"chave sem escopo para o endpoint": {
			autorizacao: "Bearer " + chaveDeTeste,
			endpoints:   []string{"trabalho"},
			querStatus:  http.StatusForbidden,
			querDesafio: true,
		},
		"chave com escopo para o endpoint": {
			autorizacao: "Bearer " + chaveDeTeste,
			endpoints:   []string{"pessoal", "trabalho"},
			querStatus:  http.StatusOK,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			repo := &RepositorioFake{
				chave: apikey.Chave{ID: 9, Endpoints: tc.endpoints},
				err:   tc.erroRepo,
			}
			sut := apikey.NovoServico(repo, logDescartado())

			middleware := auth.RequireBearerToken(sut.Verificar, &auth.RequireBearerTokenOptions{
				Scopes:                 []string{apikey.Escopo("pessoal")},
				AllowMissingExpiration: true,
			})
			handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodPost, "/mcp/pessoal", nil)
			if tc.autorizacao != "" {
				req.Header.Set("Authorization", tc.autorizacao)
			}
			res := httptest.NewRecorder()

			handler.ServeHTTP(res, req)

			if res.Code != tc.querStatus {
				t.Fatalf("status = %d, quer %d (corpo: %q)", res.Code, tc.querStatus, res.Body.String())
			}
			desafio := res.Header().Get("WWW-Authenticate")
			if tc.querDesafio && desafio == "" {
				t.Error("WWW-Authenticate ausente, quer o desafio Bearer")
			}
			if !tc.querDesafio && desafio != "" {
				t.Errorf("WWW-Authenticate = %q, quer vazio", desafio)
			}
		})
	}
}

// TestServico_GravarUsos prova que o último uso é gravado fora do caminho da
// requisição: Verificar volta na hora, e a gravação chega pela goroutine que
// GravarUsos é dona.
func TestServico_GravarUsos(t *testing.T) {
	t.Parallel()

	repo := &RepositorioFake{
		chave: apikey.Chave{ID: 42, Endpoints: []string{"pessoal"}},
		usos:  make(chan int64, 1),
	}
	sut := apikey.NovoServico(repo, logDescartado())

	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()

	pronto := make(chan struct{})
	go func() {
		defer close(pronto)
		sut.GravarUsos(ctx)
	}()

	if _, err := sut.Verificar(ctx, chaveDeTeste, nil); err != nil {
		t.Fatalf("erro = %v, quer nil", err)
	}

	select {
	case id := <-repo.usos:
		if id != 42 {
			t.Errorf("id gravado = %d, quer 42", id)
		}
	case <-ctx.Done():
		t.Fatal("contexto cancelado antes de gravar o uso")
	}

	cancelar()
	<-pronto
}
