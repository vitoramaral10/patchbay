package cripto

import (
	"context"
	"errors"
	"fmt"
)

// ChaveCanario é a chave da linha de settings que guarda o canário.
const ChaveCanario = "canario_chave_mestra"

// textoCanario é o valor conhecido que o canário cifra. O conteúdo não é
// segredo: o que importa é ele ser fixo, para que decifrar e comparar tenha
// resposta binária.
const textoCanario = "patchbay: canário da chave mestra"

// campoCanario é o contexto do canário no AAD.
var campoCanario = Campo{Tabela: "settings", Coluna: "valor", ID: ChaveCanario}

// ErrCanarioNaoConfere indica que o canário gravado não volta com a chave
// mestra em uso.
//
// É o risco "a chave mestra é ponto único de falha do estado" (seção 14): se a
// variável mudar — deploy novo, compose editado, secret rotacionado —, todo
// segredo em repouso fica ilegível de uma vez. O modo de falha ruim seria subir
// "funcionando" e transformar cada upstream em sem_consentimento, como se todos
// os provedores tivessem revogado o acesso no mesmo minuto. Por isso o processo
// não sobe: indisponibilidade é preferível a corrupção silenciosa.
var ErrCanarioNaoConfere = errors.New("cripto: o canário não confere com a chave mestra em uso")

// Configuracoes é o mínimo do armazenamento de configuração que o canário usa.
//
// Declarada aqui, no consumidor, e implementada por quem tem o SQLite: assim o
// canário é exercitado em teste sem banco, e este pacote continua sem saber o
// que é uma transação.
type Configuracoes interface {
	// Ler devolve o valor da chave. O segundo retorno é falso quando ela não
	// existe, que não é erro.
	Ler(ctx context.Context, chave string) (valor string, existe bool, err error)
	// Gravar cria ou substitui o valor da chave.
	Gravar(ctx context.Context, chave, valor string) error
}

// VerificarCanario grava o canário no primeiro boot e o confere em todos os
// seguintes. O primeiro retorno diz se ele acabou de ser gravado.
//
// Falha dura de propósito: quem chama traduz o erro em processo que não sobe.
func VerificarCanario(ctx context.Context, cofre *Cofre, cfgs Configuracoes) (bool, error) {
	guardado, existe, err := cfgs.Ler(ctx, ChaveCanario)
	if err != nil {
		return false, fmt.Errorf("cripto: ler canário: %w", err)
	}

	if !existe {
		cifrado, err := cofre.Cifrar(campoCanario, textoCanario)
		if err != nil {
			return false, fmt.Errorf("cripto: cifrar canário: %w", err)
		}
		if err := cfgs.Gravar(ctx, ChaveCanario, cifrado); err != nil {
			return false, fmt.Errorf("cripto: gravar canário: %w", err)
		}
		return true, nil
	}

	claro, err := cofre.Decifrar(campoCanario, guardado)
	if err != nil {
		// A causa vai embrulhada para o diagnóstico, mas o que o operador
		// precisa ler primeiro é que a chave mudou.
		return false, fmt.Errorf("%w: %w", ErrCanarioNaoConfere, err)
	}
	if claro.Revelar() != textoCanario {
		return false, fmt.Errorf("%w: valor decifrado inesperado", ErrCanarioNaoConfere)
	}
	return false, nil
}
