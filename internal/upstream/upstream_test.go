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
		valida  bool
		querErr error
	}{
		"upstream http completo": {ajuste: func(*upstream.Config) {}, valida: true},
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
		"tipo stdio sem comando": {
			ajuste: func(c *upstream.Config) { c.Tipo = upstream.TipoSTDIO },
			// Comando vazio não tem sentinela própria: é erro de configuração e
			// a única saída é a mensagem.
			querErr: nil,
		},
		"tipo stdio com comando só de espaço": {
			ajuste: func(c *upstream.Config) {
				c.Tipo, c.Comando = upstream.TipoSTDIO, "  \t "
			},
		},
		"upstream sse completo": {
			ajuste: func(c *upstream.Config) {
				c.Tipo, c.URL = upstream.TipoSSE, "https://exemplo.invalido/sse"
			},
			valida: true,
		},
		"tipo sse sem url": {
			ajuste: func(c *upstream.Config) { c.Tipo, c.URL = upstream.TipoSSE, "" },
		},
		"modo oauth em http é válido": {
			ajuste: func(c *upstream.Config) { c.Modo = upstream.ModoOAuth },
			valida: true,
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

			if tc.valida {
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

// TestConfig_ValidarSTDIO cobre o que o supervisor de processo aceita.
//
// URL vazia não é erro num upstream stdio, e comando vazio não é erro num
// upstream HTTP: o campo que importa é o do transporte configurado, e validar os
// dois juntos recusaria configuração perfeitamente válida.
func TestConfig_ValidarSTDIO(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		cfg  upstream.Config
		quer bool
	}{
		"comando simples": {
			cfg: upstream.Config{
				Nome: "arquivos", Tipo: upstream.TipoSTDIO,
				Comando: "npx", Timeout: time.Second,
			},
			quer: true,
		},
		"comando com argumentos e ambiente": {
			cfg: upstream.Config{
				Nome: "arquivos", Tipo: upstream.TipoSTDIO,
				Comando: "npx",
				Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "C:\\dados"},
				Env:     map[string]string{"NODE_ENV": "production"},
				Timeout: time.Second,
			},
			quer: true,
		},
		"sem url e sem comando": {
			cfg: upstream.Config{
				Nome: "arquivos", Tipo: upstream.TipoSTDIO, Timeout: time.Second,
			},
		},
		"sem nome": {
			cfg: upstream.Config{
				Tipo: upstream.TipoSTDIO, Comando: "npx", Timeout: time.Second,
			},
		},
		"timeout zerado": {
			cfg: upstream.Config{
				Nome: "arquivos", Tipo: upstream.TipoSTDIO, Comando: "npx",
			},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.Validar()
			if tc.quer && err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if !tc.quer && err == nil {
				t.Fatal("erro = nil, quer erro")
			}
		})
	}
}
