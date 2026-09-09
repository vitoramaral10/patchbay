package upstream

import (
	"context"
	"fmt"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// A escrita de credencial slot a slot, fora do formulário.
//
// O formulário é a forma certa para a UI de upstream — ele carrega "em branco
// mantém, limpar apaga" junto com o resto da configuração —, mas o import de YAML
// tem um ciclo de vida diferente: o valor vem do ambiente do processo, e uma
// credencial pode precisar ser gravada num upstream cuja configuração o import
// deixou intocada. Passar por Atualizar ali obrigaria a reescrever a linha
// inteira do upstream (e a zerar ultimo_erro) só para trocar um bearer.
//
// Arquivo próprio para que a fatia 13 não conflite no merge com as frentes que
// estão editando admin.go e repositorio_sqlite.go em paralelo.

// DefinirCredencial grava ou substitui uma credencial estática de um upstream.
//
// O valor é cifrado com o AAD do slot, como em qualquer outro caminho: é isso que
// impede que a credencial de um upstream sirva na linha de outro.
func (r *RepositorioSQLite) DefinirCredencial(
	ctx context.Context, upstreamID int64, tipo, nome string, valor cripto.Segredo,
) error {
	if err := conferirSlot(tipo, nome); err != nil {
		return err
	}
	if valor.Vazio() {
		// Valor vazio significaria gravar credencial que não autentica nada. Quem
		// quer apagar chama ApagarCredencial — a ambiguidade entre "vazio" e
		// "apagar" é justamente o que a UI resolve com um botão separado.
		return fmt.Errorf("upstream: credencial %s/%s de %d sem valor", tipo, nome, upstreamID)
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upstream: abrir transação de credencial: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := r.gravarCredencial(ctx, tx, upstreamID, tipo, nome, valor); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upstream: confirmar credencial %s/%s de %d: %w", tipo, nome, upstreamID, err)
	}
	return nil
}

// ApagarCredencial remove uma credencial estática. Apagar o que não existe não é
// erro: o resultado desejado ("este slot está vazio") já vale.
func (r *RepositorioSQLite) ApagarCredencial(
	ctx context.Context, upstreamID int64, tipo, nome string,
) error {
	if err := conferirSlot(tipo, nome); err != nil {
		return err
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upstream: abrir transação de credencial: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := apagarCredencial(ctx, tx, upstreamID, tipo, nome); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upstream: confirmar remoção de %s/%s de %d: %w", tipo, nome, upstreamID, err)
	}
	return nil
}

// conferirSlot recusa o que o CHECK da tabela recusaria, com mensagem que diz o
// que está errado.
//
// Antes do SQL e não depois: uma violação de CHECK chega como "constraint failed",
// que não diz a quem escreveu o arquivo YAML qual linha corrigir.
func conferirSlot(tipo, nome string) error {
	switch tipo {
	case CredencialBearer:
		if nome != "" {
			return fmt.Errorf("upstream: o bearer é um por upstream e não leva nome (%q)", nome)
		}
	case CredencialHeader:
		if !NomeDeHeaderValido(nome) {
			return fmt.Errorf("upstream: nome de header inválido: %q", nome)
		}
		if strings.EqualFold(nome, "authorization") {
			return fmt.Errorf("upstream: Authorization é montado pelo slot %s", CredencialBearer)
		}
	case CredencialEnv:
		if !NomeDeVariavelValido(nome) {
			return fmt.Errorf("upstream: nome de variável de ambiente inválido: %q", nome)
		}
	default:
		return fmt.Errorf("upstream: tipo de credencial desconhecido: %q", tipo)
	}
	return nil
}
