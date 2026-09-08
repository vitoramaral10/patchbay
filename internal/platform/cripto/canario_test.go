package cripto_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// configFake é a tabela settings em memória. Escrito à mão, como todo dublê
// deste repositório: o contrato tem dois métodos.
type configFake struct {
	mu     sync.Mutex
	linhas map[string]string
	erro   error
}

func novaConfigFake() *configFake { return &configFake{linhas: map[string]string{}} }

func (c *configFake) Ler(_ context.Context, chave string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.erro != nil {
		return "", false, c.erro
	}
	v, ok := c.linhas[chave]
	return v, ok, nil
}

func (c *configFake) Gravar(_ context.Context, chave, valor string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.erro != nil {
		return c.erro
	}
	c.linhas[chave] = valor
	return nil
}

func TestVerificarCanario_PrimeiroBootGravaEOsSeguintesConferem(t *testing.T) {
	t.Parallel()

	sut := cofreDeTeste(t)
	cfgs := novaConfigFake()
	ctx := context.Background()

	criado, err := cripto.VerificarCanario(ctx, sut, cfgs)
	if err != nil {
		t.Fatalf("primeiro boot: erro = %v, quer nil", err)
	}
	if !criado {
		t.Error("primeiro boot: criado = false, quer true")
	}

	guardado, existe, err := cfgs.Ler(ctx, cripto.ChaveCanario)
	if err != nil || !existe {
		t.Fatalf("canário gravado: existe = %v, erro = %v, quer true e nil", existe, err)
	}
	if !strings.HasPrefix(guardado, "pbc1:") {
		t.Errorf("canário guardado = %q, quer cifrado no formato versionado", guardado)
	}

	for volta := range 3 {
		criado, err := cripto.VerificarCanario(ctx, sut, cfgs)
		if err != nil {
			t.Fatalf("boot seguinte %d: erro = %v, quer nil", volta, err)
		}
		if criado {
			t.Errorf("boot seguinte %d: criado = true, quer false", volta)
		}
	}
}

// TestVerificarCanario_ChaveTrocadaFalha é o teste que a fatia existe para ter:
// a chave mestra mudou entre dois boots, e o processo tem de se recusar a subir.
func TestVerificarCanario_ChaveTrocadaFalha(t *testing.T) {
	t.Parallel()

	cfgs := novaConfigFake()
	ctx := context.Background()

	antigo := cofreDeTeste(t)
	if _, err := cripto.VerificarCanario(ctx, antigo, cfgs); err != nil {
		t.Fatalf("boot com a chave original: erro = %v, quer nil", err)
	}

	novo := cofreDeTeste(t)
	_, err := cripto.VerificarCanario(ctx, novo, cfgs)
	if !errors.Is(err, cripto.ErrCanarioNaoConfere) {
		t.Fatalf("erro = %v, quer %v", err, cripto.ErrCanarioNaoConfere)
	}
	// A causa continua embrulhada, para o diagnóstico não depender do texto.
	if !errors.Is(err, cripto.ErrAutenticacao) {
		t.Errorf("erro = %v, quer embrulhar também %v", err, cripto.ErrAutenticacao)
	}
	// E o canário gravado continua o do dono da chave original: um boot recusado
	// não pode sobrescrever o canário, senão a chave certa deixaria de voltar.
	guardado, _, _ := cfgs.Ler(ctx, cripto.ChaveCanario)
	if _, err := antigo.Decifrar(
		cripto.Campo{Tabela: "settings", Coluna: "valor", ID: cripto.ChaveCanario}, guardado,
	); err != nil {
		t.Errorf("canário deixou de decifrar com a chave original: erro = %v, quer nil", err)
	}
}

func TestVerificarCanario_ValorCorrompidoFalha(t *testing.T) {
	t.Parallel()

	casos := map[string]string{
		"lixo em claro":          "isto nunca foi cifrado",
		"prefixo desconhecido":   "pbc9:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"vazio":                  "",
		"cifrado de outra linha": "",
	}

	sut := cofreDeTeste(t)
	// O caso "cifrado de outra linha" é um valor legítimo desta mesma chave, mas
	// gravado com outro contexto: o AAD é o que o recusa.
	outroCampo := cripto.Campo{Tabela: "settings", Coluna: "valor", ID: "outra_chave"}
	transplantado, err := sut.Cifrar(outroCampo, "patchbay: canário da chave mestra")
	if err != nil {
		t.Fatalf("Cifrar: erro = %v, quer nil", err)
	}
	casos["cifrado de outra linha"] = transplantado

	for nome, guardado := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			cfgs := novaConfigFake()
			if err := cfgs.Gravar(context.Background(), cripto.ChaveCanario, guardado); err != nil {
				t.Fatalf("Gravar: erro = %v, quer nil", err)
			}
			_, err := cripto.VerificarCanario(context.Background(), sut, cfgs)
			if !errors.Is(err, cripto.ErrCanarioNaoConfere) {
				t.Fatalf("erro = %v, quer %v", err, cripto.ErrCanarioNaoConfere)
			}
		})
	}
}

func TestVerificarCanario_PropagaFalhaDoArmazenamento(t *testing.T) {
	t.Parallel()

	errDisco := errors.New("disco cheio")
	cfgs := novaConfigFake()
	cfgs.erro = errDisco

	_, err := cripto.VerificarCanario(context.Background(), cofreDeTeste(t), cfgs)
	if !errors.Is(err, errDisco) {
		t.Fatalf("erro = %v, quer %v", err, errDisco)
	}
}
