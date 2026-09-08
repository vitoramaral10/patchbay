package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// OpcoesSeed são os parâmetros do seed de desenvolvimento.
type OpcoesSeed struct {
	Endpoint    string
	Upstream    string
	UpstreamURL string
	NomeChave   string
	TimeoutMS   int64
}

// ResultadoSeed é o que o seed criou. A chave em claro aparece uma única vez:
// ela é guardada como hash e não tem como voltar.
type ResultadoSeed struct {
	EndpointID   int64
	EndpointSlug string
	UpstreamID   int64
	ChaveClaro   string
	ChavePrefixo string
}

// semear cria um endpoint e um upstream HTTP sem auth, compõe os dois e emite
// uma chave de API com escopo naquele endpoint.
//
// É seed de desenvolvimento, não migração: uma migração de seed entraria em
// toda instalação para sempre, e a partir da fatia 2 tudo isso passa a ser
// feito pela UI. Roda em transação: seed pela metade é pior que seed nenhum.
func semear(ctx context.Context, escrita *sql.DB, o OpcoesSeed) (ResultadoSeed, error) {
	if o.UpstreamURL == "" {
		return ResultadoSeed{}, errors.New("seed: --upstream-url é obrigatório")
	}
	if o.TimeoutMS <= 0 {
		return ResultadoSeed{}, errors.New("seed: --timeout-ms precisa ser positivo")
	}

	emitida, err := apikey.Gerar()
	if err != nil {
		return ResultadoSeed{}, err
	}

	tx, err := escrita.BeginTx(ctx, nil)
	if err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: abrir transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	agora := time.Now().Unix()

	var upstreamID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO upstream (nome, tipo, url, timeout_ms, habilitado, criado_em)
VALUES (?, ?, ?, ?, 1, ?)
ON CONFLICT (nome) DO UPDATE SET
    tipo = excluded.tipo,
    url = excluded.url,
    timeout_ms = excluded.timeout_ms,
    habilitado = 1,
    ultimo_erro = ''
RETURNING id`,
		o.Upstream, upstream.TipoHTTP, o.UpstreamURL, o.TimeoutMS, agora).Scan(&upstreamID)
	if err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: gravar upstream %s: %w", o.Upstream, err)
	}

	var endpointID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO endpoint (slug, descricao, criado_em)
VALUES (?, ?, ?)
ON CONFLICT (slug) DO UPDATE SET descricao = excluded.descricao
RETURNING id`,
		o.Endpoint, "endpoint de desenvolvimento criado por patchbay seed", agora).Scan(&endpointID)
	if err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: gravar endpoint %s: %w", o.Endpoint, err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO endpoint_upstream (endpoint_id, upstream_id, prefixo, ordem)
VALUES (?, ?, '', 0)
ON CONFLICT (endpoint_id, upstream_id) DO NOTHING`, endpointID, upstreamID); err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: compor endpoint com upstream: %w", err)
	}

	var chaveID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO api_key (nome, hash, prefixo_visivel, criado_em)
VALUES (?, ?, ?, ?)
RETURNING id`,
		o.NomeChave, emitida.Hash, emitida.PrefixoVisivel, agora).Scan(&chaveID)
	if err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: gravar chave de api: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO api_key_endpoint (api_key_id, endpoint_id) VALUES (?, ?)`,
		chaveID, endpointID); err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: dar escopo à chave: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ResultadoSeed{}, fmt.Errorf("seed: confirmar transação: %w", err)
	}

	return ResultadoSeed{
		EndpointID:   endpointID,
		EndpointSlug: o.Endpoint,
		UpstreamID:   upstreamID,
		ChaveClaro:   emitida.Claro,
		ChavePrefixo: emitida.PrefixoVisivel,
	}, nil
}

// comandoSeed é a borda: lê as flags, chama semear e imprime o que um humano
// precisa para conectar.
func comandoSeed(ctx context.Context, args []string, saida io.Writer) error {
	fs := flag.NewFlagSet("patchbay seed", flag.ContinueOnError)
	o := OpcoesSeed{}
	fs.StringVar(&o.Endpoint, "endpoint", "pessoal", "slug do endpoint a criar")
	fs.StringVar(&o.Upstream, "upstream", "exemplo", "nome do upstream a criar")
	fs.StringVar(&o.UpstreamURL, "upstream-url", "", "URL do upstream MCP Streamable HTTP sem auth")
	fs.StringVar(&o.NomeChave, "nome-chave", "desenvolvimento", "nome da chave de API emitida")
	fs.Int64Var(&o.TimeoutMS, "timeout-ms", 15000, "timeout de toda operação daquele upstream")

	// As flags de configuração comuns entram no mesmo FlagSet, para que o seed
	// aceite -data-dir e -public-url igual ao serve.
	cfg, resolver := registrarFlags(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if err := resolver(); err != nil {
		return err
	}
	log := novoLogger(*cfg, os.Stderr)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("criar diretório de dados %s: %w", cfg.DataDir, err)
	}
	st, err := store.Abrir(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("falha ao fechar o banco", "erro", err)
		}
	}()

	r, err := semear(ctx, st.Escrita(), o)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(saida, `seed aplicado em %s

endpoint: %s (id %d)
upstream: %s -> %s (id %d)
chave de api: %s   (prefixo visível: %s)

A chave aparece uma única vez: ela é guardada como hash. Para registrar no
Claude Code:

  claude mcp add --transport http %s %s/mcp/%s --header "Authorization: Bearer %s"
`,
		st.Caminho(),
		r.EndpointSlug, r.EndpointID,
		o.Upstream, o.UpstreamURL, r.UpstreamID,
		r.ChaveClaro, r.ChavePrefixo,
		"patchbay-"+r.EndpointSlug, cfg.PublicURL, r.EndpointSlug, r.ChaveClaro)
	return nil
}
