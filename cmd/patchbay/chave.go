package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

// instrucaoChaveMestra é o que o operador precisa ler quando o processo se
// recusa a subir por causa da chave.
const instrucaoChaveMestra = `
A chave mestra cifra em repouso todo segredo que o patchbay apresenta a um MCP.
Ela vem de ` + cripto.VarChaveMestra + ` quando a variável existe; senão, do
arquivo ` + arquivoChaveMestra + ` no diretório de dados, criado no primeiro boot.
São 32 bytes em base64. Gere uma com:

    patchbay chave-mestra gerar`

// arquivoChaveMestra guarda a chave quando o ambiente não traz nenhuma.
const arquivoChaveMestra = "chave-mestra"

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

// cofreDoBoot devolve a cifra de campo do processo.
//
// PATCHBAY_MASTER_KEY, quando existe, vence (é o deploy da v1, e quem quer a
// chave fora do disco dos dados). Sem ela, a chave mora em
// <dados>/chave-mestra, sorteada no primeiro boot: instalar não pede passo
// nenhum. shortcut: com a chave ao lado do banco, a cifra protege o banco
// copiado sozinho, não o volume inteiro; quem precisa disso usa a variável.
//
// A variável sai do ambiente assim que é lida: um MCP stdio herda o ambiente
// do patchbay, e esta é a segunda camada depois do filtro de prefixo.
func cofreDoBoot(dataDir string) (*cripto.Cofre, error) {
	texto := os.Getenv(cripto.VarChaveMestra)
	_ = os.Unsetenv(cripto.VarChaveMestra)
	if texto == "" {
		var err error
		if texto, err = chaveDoArquivo(dataDir); err != nil {
			return nil, err
		}
	}
	return cofreDe(texto)
}

// chaveDoArquivo lê <dados>/chave-mestra, criando-a se não existir.
func chaveDoArquivo(dataDir string) (string, error) {
	caminho := filepath.Join(dataDir, arquivoChaveMestra)
	if b, err := os.ReadFile(caminho); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("ler %s: %w", caminho, err)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("criar diretório de dados %s: %w", dataDir, err)
	}
	texto, err := cripto.GerarChaveMestra()
	if err != nil {
		return "", err
	}
	// Grava num temporário e publica com link: o link é atômico e falha se o
	// arquivo já existe, então dois boots no mesmo volume nunca leem uma chave
	// pela metade nem sorteiam duas.
	tmp, err := os.CreateTemp(dataDir, ".chave-mestra-*")
	if err != nil {
		return "", fmt.Errorf("criar chave mestra em %s: %w", dataDir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(texto + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("gravar chave mestra: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("gravar chave mestra: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("gravar chave mestra: %w", err)
	}
	if err := os.Link(tmp.Name(), caminho); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return chaveDoArquivo(dataDir) // outro boot publicou primeiro
		}
		return "", fmt.Errorf("criar %s: %w", caminho, err)
	}
	return texto, nil
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

Reponha em ` + cripto.VarChaveMestra + ` (ou no arquivo ` + arquivoChaveMestra + `) a chave com que este banco foi criado. Se ela
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
