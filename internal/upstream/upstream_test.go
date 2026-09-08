package upstream_test

import (
	"errors"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

func TestConfig_Validar(t *testing.T) {
	t.Parallel()

	valida := upstream.Config{
		ID:      1,
		Nome:    "notion",
		Tipo:    upstream.TipoHTTP,
		URL:     "https://exemplo.invalido/mcp",
		Timeout: 15 * time.Second,
	}

	casos := map[string]struct {
		ajuste  func(*upstream.Config)
		querErr error
	}{
		"upstream http completo": {ajuste: func(*upstream.Config) {}},
		"sem nome": {
			ajuste: func(c *upstream.Config) { c.Nome = "" },
			// Nome vazio não tem sentinela própria: é erro de configuração e a
			// única saída é a mensagem.
			querErr: nil,
		},
		"sem url": {
			ajuste: func(c *upstream.Config) { c.URL = "" },
		},
		"timeout zerado": {
			ajuste: func(c *upstream.Config) { c.Timeout = 0 },
		},
		"tipo stdio ainda não suportado": {
			ajuste:  func(c *upstream.Config) { c.Tipo = upstream.TipoSTDIO },
			querErr: upstream.ErrTipoNaoSuportado,
		},
		"tipo sse ainda não suportado": {
			ajuste:  func(c *upstream.Config) { c.Tipo = upstream.TipoSSE },
			querErr: upstream.ErrTipoNaoSuportado,
		},
		"tipo desconhecido": {
			ajuste:  func(c *upstream.Config) { c.Tipo = "carta-pombo" },
			querErr: upstream.ErrTipoNaoSuportado,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			cfg := valida
			tc.ajuste(&cfg)
			err := cfg.Validar()

			if nome == "upstream http completo" {
				if err != nil {
					t.Fatalf("erro = %v, quer nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("erro = nil, quer erro")
			}
			if tc.querErr != nil && !errors.Is(err, tc.querErr) {
				t.Fatalf("erro = %v, quer %v", err, tc.querErr)
			}
		})
	}
}
