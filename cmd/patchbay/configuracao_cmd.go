package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/authsrv"
	"github.com/vitoramaral10/patchbay/internal/configuracao"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// Os dois subcomandos de configuração versionável: `patchbay export` e
// `patchbay import`.
//
// Os dois abrem o banco como o seed abre — mesmo portão de chave mestra, mesmo
// canário — e não sobem supervisão nenhuma: o YAML nunca é lido no boot (seção
// 08.9), e um import que precisasse do gateway no ar não seria a ferramenta de
// reconstruir uma instalação a partir do arquivo.
//
// Consequência a saber: importar com o patchbay rodando escreve no banco, mas o
// processo em execução não relê a configuração sozinho — é o mesmo comportamento
// do seed. Para aplicar no ar, use os botões da tela de configuração.

// permissaoArquivoExportado é 0600 por precaução, não por necessidade: o YAML não
// carrega segredo nenhum, mas ele descreve a topologia inteira do gateway, e o
// padrão de 0644 num diretório compartilhado entregaria isso de graça.
const permissaoArquivoExportado = 0o600

// comandoExport imprime o YAML do estado atual.
func comandoExport(ctx context.Context, args []string, saida io.Writer) error {
	fs := flag.NewFlagSet("patchbay export", flag.ContinueOnError)
	var destino string
	var forcar bool
	fs.StringVar(&destino, "o", "", "arquivo de destino; sem ele o YAML vai para a saída padrão")
	fs.BoolVar(&forcar, "forcar", false, "sobrescreve o arquivo de destino se ele já existir")
	cfg, resolver := registrarFlags(fs)
	if _, err := parsearComPosicionais(fs, args); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if err := resolver(); err != nil {
		return err
	}
	log := novoLogger(*cfg, os.Stderr)

	// Recusar antes de tocar no banco: um -o que aponta para o arquivo errado é
	// engano de quem digitou o comando, e descobrir isso só depois de abrir o
	// banco não pouparia a sobrescrita nenhuma.
	if destino != "" && !forcar {
		if _, err := os.Stat(destino); err == nil {
			return fmt.Errorf("export: %s já existe; use --forcar para sobrescrever", destino)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("export: conferir %s: %w", destino, err)
		}
	}

	servico, fechar, err := servicoDeConfiguracao(ctx, *cfg, log)
	if err != nil {
		return err
	}
	defer fechar()

	dados, err := servico.Exportar(ctx)
	if err != nil {
		return err
	}
	if destino == "" {
		_, err = saida.Write(dados)
		return err
	}
	if err := os.WriteFile(destino, dados, permissaoArquivoExportado); err != nil {
		return fmt.Errorf("export: gravar %s: %w", destino, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "configuração exportada em %s\n", destino)
	return nil
}

// comandoImport planeja e, fora do --dry-run, aplica.
//
// O plano é impresso sempre, inclusive no import de verdade: quem lê a saída
// precisa ver o que foi decidido item a item, e não só o total.
func comandoImport(ctx context.Context, args []string, saida io.Writer) error {
	fs := flag.NewFlagSet("patchbay import", flag.ContinueOnError)
	var (
		ensaio          bool
		removerAusentes bool
	)
	fs.BoolVar(&ensaio, "dry-run", false, "mostra o plano e não escreve nada")
	fs.BoolVar(&removerAusentes, "remover-ausentes", false,
		"apaga o que existe no banco e não está no arquivo")
	cfg, resolver := registrarFlags(fs)

	posicionais, err := parsearComPosicionais(fs, args)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if len(posicionais) != 1 {
		return errors.New("import: informe um arquivo YAML — uso: patchbay import arquivo.yaml [--dry-run]")
	}
	if err := resolver(); err != nil {
		return err
	}
	log := novoLogger(*cfg, os.Stderr)

	// O caminho é o argumento do subcomando: quem o escolhe é quem rodou o
	// binário, e ele já tem acesso de leitura ao que o processo tem. Restringir a
	// um diretório aqui não protegeria nada e impediria o uso normal — importar
	// um arquivo de qualquer lugar do disco.
	dados, err := os.ReadFile(posicionais[0]) // #nosec G304,G703 -- caminho vem do argumento do operador
	if err != nil {
		return fmt.Errorf("import: ler %s: %w", posicionais[0], err)
	}

	servico, fechar, err := servicoDeConfiguracao(ctx, *cfg, log)
	if err != nil {
		return err
	}
	defer fechar()

	plano, err := servico.Planejar(ctx, dados, configuracao.Opcoes{RemoverAusentes: removerAusentes})
	if err != nil {
		return err
	}
	if err := plano.Escrever(saida); err != nil {
		return err
	}

	if ensaio {
		_, _ = fmt.Fprintf(saida,
			"--dry-run: nada foi escrito. Rode sem a flag para aplicar os %d item(ns) aplicáveis.\n",
			plano.Aplicaveis())
		// Conflito e erro não escrevem nada mesmo fora do --dry-run, mas quem
		// chama este comando de um pipeline de CI precisa de um código de saída
		// que diga "este arquivo tem pendência" — e nada aqui além do texto
		// impresso distinguiria esse ensaio de um YAML limpo.
		if n := plano.Erros() + plano.Conflitos(); n > 0 {
			return fmt.Errorf("import --dry-run: %d item(ns) em erro ou conflito", n)
		}
		return nil
	}

	relatorio, err := servico.Aplicar(ctx, plano)
	if erroDeEscrita := relatorio.Escrever(saida); erroDeEscrita != nil {
		return erroDeEscrita
	}
	if plano.Conflitos() > 0 {
		_, _ = fmt.Fprintf(saida,
			"%d item(ns) em conflito não foram aplicados: o arquivo e o banco mudaram desde o export.\n"+
				"Decida item a item — edite o arquivo com o valor que vale e importe de novo, ou\n"+
				"exporte de novo para partir do estado do banco.\n", plano.Conflitos())
	}
	if err != nil && !errors.Is(err, configuracao.ErrAplicacaoParcial) {
		return err
	}
	// Conflito e erro de item não abortam o import — a mescla aplica o que dá e
	// deixa o resto para o dono decidir —, mas o código de saída precisa avisar
	// quem chama de um terminal de CI: "aplicado sem pendência" e "aplicado com
	// conflito ou erro" não podem devolver o mesmo 0.
	if n := plano.Erros() + plano.Conflitos() + relatorio.Falhas(); n > 0 {
		return fmt.Errorf("import: %d item(ns) não foram aplicados por erro ou conflito", n)
	}
	return nil
}

// servicoDeConfiguracao abre o banco e monta o serviço de export/import sem
// supervisão de upstream.
//
// Devolve o fechamento em vez de aceitar um defer de fora para que o chamador não
// precise conhecer o store: é a mesma razão de as features receberem *sql.DB e
// não *store.Store.
func servicoDeConfiguracao(
	ctx context.Context, cfg Config, log *slog.Logger,
) (*configuracao.Servico, func(), error) {
	cofre, err := cofreDoAmbiente()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("criar diretório de dados %s: %w", cfg.DataDir, err)
	}
	st, err := store.Abrir(ctx, cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	fechar := func() {
		if err := st.Close(); err != nil {
			log.Error("falha ao fechar o banco", "erro", err)
		}
	}

	leitura, escrita := st.Leitura(), st.Escrita()
	if err := verificarCanario(ctx, cofre, leitura, escrita, log); err != nil {
		fechar()
		return nil, nil, err
	}

	repoUpstream := upstream.NovoRepositorioSQLite(leitura, escrita, cofre)
	repoEndpoint := endpoint.NovoRepositorioSQLite(leitura, escrita)

	servico := configuracao.NovoServico(
		upstreamsDaConfiguracao{repo: repoUpstream, log: log},
		segredosDaConfiguracao{repo: repoUpstream},
		endpointsDaConfiguracao{repo: repoEndpoint, upstreams: repoUpstream, log: log},
		log.With("componente", "configuracao"),
		configuracao.ComChaves(chavesDaConfiguracao{repo: apikey.NovoRepositorioSQLite(leitura, escrita)}),
		configuracao.ComClientes(clientesDaConfiguracao{repo: authsrv.NovoRepositorioSQLite(leitura, escrita)}),
		configuracao.ComAmbiente(os.LookupEnv),
	)
	return servico, fechar, nil
}

// parsearComPosicionais analisa as flags aceitando argumento posicional em
// qualquer posição.
//
// O flag da stdlib para de analisar no primeiro argumento que não é flag, então
// `patchbay import arquivo.yaml --dry-run` deixaria --dry-run como argumento
// posicional e a flag valeria false — um --dry-run silenciosamente ignorado é o
// pior resultado possível deste subcomando. O laço tira o posicional e analisa o
// resto até não sobrar nenhum.
func parsearComPosicionais(fs *flag.FlagSet, args []string) ([]string, error) {
	var posicionais []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return posicionais, nil
		}
		posicionais = append(posicionais, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
