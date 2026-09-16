// Command patchbay é o gateway MCP self-hosted: um endpoint agrega vários
// servidores MCP upstream e os serve como se fossem um só.
//
// Subcomandos:
//
//	patchbay serve                sobe o gateway
//	patchbay seed                 cria endpoint, upstream e chave de desenvolvimento
//	patchbay export               escreve a configuração em YAML
//	patchbay import arquivo.yaml  aplica um YAML, com --dry-run e mescla
//	patchbay chave-mestra gerar   sorteia a chave de PATCHBAY_MASTER_KEY
//	patchbay versao               imprime a versão
//
// serve, seed, export e import exigem PATCHBAY_MASTER_KEY: sem ela não há como
// ler nem gravar segredo em repouso, e subir sem cifra seria a corrupção
// silenciosa que a seção 14 do estudo recusa.
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

	case "export":
		return comandoExport(ctx, args, saida)

	case "import":
		return comandoImport(ctx, args, saida)

	case "biblioteca-semente":
		return comandoBibliotecaSemente(ctx, args, saida)

	case "chave-mestra":
		return comandoChaveMestra(args, saida)

	case "versao", "version":
		_, _ = fmt.Fprintf(saida, "patchbay %s\n", versao.String())
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
  serve                sobe o gateway (padrão)
  seed                 cria endpoint, upstream HTTP e chave de API de desenvolvimento
  biblioteca-semente   regrava o catálogo embutido varrendo o mcpservers.org   [-o arquivo]
  export               escreve a configuração versionável em YAML   [-o arquivo] [--forcar]
  import               aplica um YAML   arquivo [--dry-run] [--remover-ausentes]
  chave-mestra gerar   sorteia a chave de PATCHBAY_MASTER_KEY
  versao               imprime a versão
  ajuda                imprime esta mensagem

flags comuns (todas com variável de ambiente equivalente):
  -listen       endereço de escuta            PATCHBAY_LISTEN
  -data-dir     diretório do banco SQLite     PATCHBAY_DATA_DIR
  -public-url   URL pública do patchbay       PATCHBAY_PUBLIC_URL
  -log-level    debug|info|warn|error         PATCHBAY_LOG_LEVEL
  -log-texto    log em texto em vez de JSON   PATCHBAY_LOG_TEXTO

variável obrigatória em serve e seed (não tem flag equivalente, de propósito:
argumento de processo aparece em ps e em histórico de shell):
  PATCHBAY_MASTER_KEY   chave mestra de cifra, 32 bytes em base64
`)
}
