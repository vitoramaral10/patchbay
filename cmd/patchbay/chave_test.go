package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// TestCofreDe_ChaveAusenteOuInvalidaRecusadaNoBoot: sem chave mestra, ou com
// chave que não é 32 bytes em base64, o processo não sobe. É o portão que
// serve e seed atravessam antes de tocar o banco.
func TestCofreDe_ChaveAusenteOuInvalidaRecusadaNoBoot(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		texto string
		quer  error
	}{
		"variável não definida":  {texto: "", quer: cripto.ErrChaveMestraAusente},
		"chave curta":            {texto: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16)), quer: cripto.ErrChaveMestraInvalida},
		"chave que não é base64": {texto: "chave-de-trinta-e-dois-caracteres", quer: cripto.ErrChaveMestraInvalida},
		"chave válida":           {texto: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			cofre, err := cofreDe(tc.texto)
			if !errors.Is(err, tc.quer) {
				t.Fatalf("erro = %v, quer %v", err, tc.quer)
			}
			if tc.quer == nil {
				if cofre == nil {
					t.Fatal("cofre = nil, quer a cifra montada")
				}
				return
			}
			// A recusa precisa dizer o que fazer: quem lê isto está no meio de
			// um deploy que acabou de falhar.
			for _, trecho := range []string{cripto.VarChaveMestra, "patchbay chave-mestra gerar"} {
				if !strings.Contains(err.Error(), trecho) {
					t.Errorf("mensagem = %q, quer conter %q", err.Error(), trecho)
				}
			}
		})
	}
}

// TestComandoChaveMestra_GeraChaveUsavel prova o caminho da primeira instalação:
// o único subcomando que roda sem a chave é o que a produz.
func TestComandoChaveMestra_GeraChaveUsavel(t *testing.T) {
	t.Parallel()

	var saida bytes.Buffer
	if err := comandoChaveMestra(nil, &saida); err != nil {
		t.Fatalf("comandoChaveMestra: erro = %v, quer nil", err)
	}

	// Uma linha só na saída padrão, para servir a `export VAR=$(...)`.
	linhas := strings.Fields(saida.String())
	if len(linhas) != 1 {
		t.Fatalf("saída = %q, quer só a chave", saida.String())
	}
	if _, err := cofreDe(linhas[0]); err != nil {
		t.Fatalf("a chave gerada não é aceita no boot: erro = %v, quer nil", err)
	}
}

func TestComandoChaveMestra_VerboDesconhecido(t *testing.T) {
	t.Parallel()

	var saida bytes.Buffer
	if err := comandoChaveMestra([]string{"mostrar"}, &saida); err == nil {
		t.Fatal("erro = nil, quer recusa do verbo desconhecido")
	}
	if saida.Len() != 0 {
		t.Errorf("saída = %q, quer vazia", saida.String())
	}
}

// TestMontar_CanarioComChaveTrocadaNaoSobe é o teste que a seção 14 pede: o
// mesmo banco, uma chave mestra diferente, e o processo se recusando a subir em
// vez de tratar cada upstream como se o provedor tivesse revogado o acesso.
func TestMontar_CanarioComChaveTrocadaNaoSobe(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
		PublicURL: "http://127.0.0.1:8787",
		NivelLog:  slog.LevelError,
	}
	log := slog.New(slog.DiscardHandler)
	ctx, cancelar := context.WithCancel(context.Background())
	t.Cleanup(cancelar)

	// Primeiro boot: grava o canário e sobe.
	app, err := montar(ctx, cfg, cofreDeTeste(t), log)
	if err != nil {
		t.Fatalf("primeiro boot: erro = %v, quer nil", err)
	}
	if err := app.Fechar(); err != nil {
		t.Fatalf("fechar o primeiro boot: erro = %v, quer nil", err)
	}

	// Segundo boot com a chave trocada: falha dura.
	if _, err := montar(ctx, cfg, cofreDeTeste(t), log); err == nil {
		t.Fatal("erro = nil, quer recusa por canário que não confere")
	} else {
		if !errors.Is(err, cripto.ErrCanarioNaoConfere) {
			t.Fatalf("erro = %v, quer %v", err, cripto.ErrCanarioNaoConfere)
		}
		if !strings.Contains(err.Error(), cripto.VarChaveMestra) {
			t.Errorf("mensagem = %q, quer dizer qual variável repor", err.Error())
		}
	}
}
