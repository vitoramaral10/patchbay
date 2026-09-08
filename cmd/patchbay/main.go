// Command patchbay é o gateway MCP self-hosted: um endpoint agrega vários
// servidores MCP upstream e os serve como se fossem um só.
//
// Subcomandos:
//
//	patchbay serve   sobe o gateway
//	patchbay seed    cria endpoint, upstream e chave de desenvolvimento
//	patchbay versao  imprime a versão
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vitoramaral10/patchbay/internal/platform/versao"
)

func main() {
	if err := executar(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "patchbay: %v\n", err)
		os.Exit(1)
	}
}

// executar despacha o subcomando. É a borda: é aqui que erro vira texto e
// código de saída, e em nenhum lugar mais.
func executar(args []string, saida *os.File) error {
	// Ctrl+C e SIGTERM cancelam o contexto raiz; todo componente desliga a
	// partir dele.
	ctx, parar := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer parar()

	comando := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		comando, args = args[0], args[1:]
	}

	switch comando {
	case "serve":
		cfg, err := lerConfig(comando, args)
		if err != nil {
			return err
		}
		return servir(ctx, cfg, novoLogger(cfg, os.Stderr))

	case "seed":
		return comandoSeed(ctx, args, saida)

	case "versao", "version":
		_, _ = fmt.Fprintf(saida, "patchbay %s\n", versao.Numero)
		return nil

	case "ajuda", "help":
		imprimirAjuda(saida)
		return nil

	default:
		imprimirAjuda(os.Stderr)
		return fmt.Errorf("subcomando desconhecido: %s", comando)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func imprimirAjuda(saida *os.File) {
	_, _ = fmt.Fprint(saida, `uso: patchbay <subcomando> [flags]

subcomandos:
  serve    sobe o gateway (padrão)
  seed     cria endpoint, upstream HTTP e chave de API de desenvolvimento
  versao   imprime a versão
  ajuda    imprime esta mensagem

flags comuns (todas com variável de ambiente equivalente):
  -listen       endereço de escuta            PATCHBAY_LISTEN
  -data-dir     diretório do banco SQLite     PATCHBAY_DATA_DIR
  -public-url   URL pública do patchbay       PATCHBAY_PUBLIC_URL
  -log-level    debug|info|warn|error         PATCHBAY_LOG_LEVEL
  -log-texto    log em texto em vez de JSON   PATCHBAY_LOG_TEXTO
`)
}
