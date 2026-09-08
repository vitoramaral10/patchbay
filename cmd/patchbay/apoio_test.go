package main

import (
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// cofreDeTeste monta a cifra de campo com uma chave mestra efêmera.
//
// Uma chave por teste, sorteada: nenhum teste depende de PATCHBAY_MASTER_KEY do
// ambiente — o que também impede que um teste passe por acidente com a chave de
// desenvolvimento de quem está rodando.
func cofreDeTeste(t *testing.T) *cripto.Cofre {
	t.Helper()

	texto, err := cripto.GerarChaveMestra()
	if err != nil {
		t.Fatalf("gerar chave mestra: erro = %v, quer nil", err)
	}
	cofre, err := cofreDe(texto)
	if err != nil {
		t.Fatalf("montar cofre: erro = %v, quer nil", err)
	}
	return cofre
}
