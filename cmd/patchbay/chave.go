package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

// instrucaoChaveMestra é o que o operador precisa ler quando o processo se
// recusa a subir por causa da chave.
//
// A mensagem vai por inteiro na saída de erro, e não só no README, porque quem
// encontra este erro está no meio de um deploy que acabou de falhar.
const instrucaoChaveMestra = `
A chave mestra cifra em repouso todo segredo que o patchbay apresenta a um
upstream (bearer, header estático e, adiante, os tokens de OAuth). Ela vem
exclusivamente da variável de ambiente ` + cripto.VarChaveMestra + `:
são 32 bytes em base64, sem arquivo de chave em disco e sem entrada pela UI.

Gere uma com:

    patchbay chave-mestra gerar

e exporte o resultado em ` + cripto.VarChaveMestra + ` antes de subir o patchbay.
Guarde-a onde você guarda segredo de produção: perdê-la torna ilegível tudo o
que já foi cifrado, e trocá-la faz o patchbay se recusar a subir.`

// cofreDe monta a cifra de campo a partir do texto da chave mestra.
func cofreDe(texto string) (*cripto.Cofre, error) {
	mestra, err := cripto.ChaveMestraDe(texto)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, instrucaoChaveMestra)
	}
	cofre, err := cripto.NovoCofre(mestra)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, instrucaoChaveMestra)
	}
	return cofre, nil
}

// cofreDoAmbiente é o caminho de boot: lê PATCHBAY_MASTER_KEY e nada mais.
func cofreDoAmbiente() (*cripto.Cofre, error) {
	return cofreDe(os.Getenv(cripto.VarChaveMestra))
}

// verificarCanario grava o canário no primeiro boot e confere nos seguintes.
//
// Falha dura: o processo não sobe. É a escolha da seção 14 — indisponibilidade
// em vez de corrupção silenciosa. Subir "funcionando" com a chave errada
// transformaria todo upstream autenticado em falha de credencial, como se todos
// os provedores tivessem revogado o acesso no mesmo minuto.
func verificarCanario(
	ctx context.Context, cofre *cripto.Cofre, leitura, escrita *sql.DB, log *slog.Logger,
) error {
	criado, err := cripto.VerificarCanario(ctx, cofre, store.NovasConfiguracoes(leitura, escrita))
	if err != nil {
		return fmt.Errorf("%w\n%s", err, instrucaoCanario)
	}
	if criado {
		log.Info("canário da chave mestra gravado no primeiro boot")
	}
	return nil
}

const instrucaoCanario = `
O canário gravado neste banco não volta com a chave mestra em uso: ela é outra.
Todo segredo cifrado aqui ficou ilegível, então o patchbay se recusa a subir em
vez de tratar cada upstream como se o provedor tivesse revogado o acesso.

Reponha em ` + cripto.VarChaveMestra + ` a chave com que este banco foi criado. Se ela
foi perdida de vez, o caminho é apagar o banco e recadastrar — não existe
recuperação, e é por isso que a chave é segredo de produção.`

// comandoChaveMestra é a borda do subcomando que gera uma chave nova.
//
// Ele é o único caminho do binário que não exige a chave já existir — é o que
// resolve o ovo e a galinha da primeira instalação.
func comandoChaveMestra(args []string, saida io.Writer) error {
	verbo := "gerar"
	if len(args) > 0 && !isFlag(args[0]) {
		verbo, args = args[0], args[1:]
	}
	if verbo != "gerar" {
		return fmt.Errorf("chave-mestra: verbo desconhecido: %s (use: gerar)", verbo)
	}

	fs := flag.NewFlagSet("patchbay chave-mestra gerar", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("chave-mestra: %w", err)
	}

	chave, err := cripto.GerarChaveMestra()
	if err != nil {
		return err
	}

	// A chave sai na saída padrão e sozinha na linha, para que `export
	// PATCHBAY_MASTER_KEY=$(patchbay chave-mestra gerar)` funcione; a explicação
	// vai para a saída de erro, que não entra na substituição.
	_, _ = fmt.Fprintln(saida, chave)
	_, _ = fmt.Fprintf(os.Stderr, `
Chave mestra de 32 bytes em base64, impressa uma única vez: ela não é guardada
em lugar nenhum pelo patchbay. Exporte em %s e guarde onde você guarda
segredo de produção.
`, cripto.VarChaveMestra)
	return nil
}
