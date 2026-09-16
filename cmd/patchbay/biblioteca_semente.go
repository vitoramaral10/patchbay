package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// caminhoDaSemente é onde o go:embed a procura.
const caminhoDaSemente = "internal/biblioteca/semente.json.gz"

// comandoBibliotecaSemente varre a origem e regrava o catálogo embutido.
//
// Roda na máquina de quem constrói o release, não no gateway: é uma varredura
// completa da lista oficial do mcpservers.org — o índice paginado mais uma
// página de detalhe por servidor —, e leva cerca de 30 minutos (29m26s
// medidos em 2026-09-11). O resultado vai versionado, e é ele que faz
// instalação nova nascer com catálogo em vez de esperar a primeira varredura.
//
// A data gravada é a de agora, e é a idade que a tela vai mostrar em toda
// instalação nova até a primeira varredura terminar. Regerar a semente perto do
// corte de versão é o que a mantém honesta.
func comandoBibliotecaSemente(ctx context.Context, args []string, saida *os.File) error {
	fs := flag.NewFlagSet("biblioteca-semente", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	destino := fs.String("o", caminhoDaSemente, "arquivo de saída")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// repo nulo de propósito: Varrer não grava nada. Ver o comentário dela.
	sinc := biblioteca.NovoSincronizador(biblioteca.NovaCuradoria(""), nil, log)

	_, _ = fmt.Fprintln(saida, "varrendo a origem — isto leva cerca de 30 minutos")
	inicio := time.Now()
	itens, err := sinc.Varrer(ctx)
	if err != nil {
		return fmt.Errorf("varrer: %w", err)
	}
	if len(itens) == 0 {
		return fmt.Errorf("a varredura não trouxe nenhum servidor")
	}

	if err := os.MkdirAll(filepath.Dir(*destino), 0o750); err != nil {
		return fmt.Errorf("criar diretório da semente: %w", err)
	}
	// Arquivo temporário e rename: um Ctrl+C no meio da gravação deixaria uma
	// semente truncada no lugar da boa, e o go:embed a levaria para o binário.
	tmp := *destino + ".novo"
	// O gosec marca a abertura abaixo porque o caminho é variável. Ele vem da
	// flag -o deste comando, escolhida por quem constrói o release na própria
	// máquina — é o mesmo nível de confiança de qualquer argumento de linha de
	// comando, e recusá-lo aqui só tiraria a possibilidade de gerar a semente
	// noutro lugar para conferir antes de trocar a versionada.
	//nolint:gosec // caminho vem da flag do próprio operador
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("abrir semente: %w", err)
	}
	if err := biblioteca.GravarSemente(f, itens, time.Now().UTC()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fechar semente: %w", err)
	}
	if err := os.Rename(tmp, *destino); err != nil {
		return fmt.Errorf("mover semente: %w", err)
	}

	info, err := os.Stat(*destino)
	if err != nil {
		return fmt.Errorf("conferir semente: %w", err)
	}
	_, _ = fmt.Fprintf(saida, "semente gravada em %s: %d servidores, %d kB, em %s\n",
		*destino, len(itens), info.Size()/1024, time.Since(inicio).Round(time.Second))
	return nil
}
