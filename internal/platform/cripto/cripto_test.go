package cripto_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// TestChaveMestraDe cobre o portão do boot: sem chave, ou com chave que não é
// base64 de 32 bytes, o processo não pode subir.
func TestChaveMestraDe(t *testing.T) {
	t.Parallel()

	trintaEDois := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))

	casos := map[string]struct {
		texto string
		quer  error
	}{
		"vazia":                     {texto: "", quer: cripto.ErrChaveMestraAusente},
		"só espaço":                 {texto: "   \n\t ", quer: cripto.ErrChaveMestraAusente},
		"não é base64":              {texto: "isto não é base64!!", quer: cripto.ErrChaveMestraInvalida},
		"curta demais":              {texto: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)), quer: cripto.ErrChaveMestraInvalida},
		"longa demais":              {texto: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)), quer: cripto.ErrChaveMestraInvalida},
		"base64 padrão":             {texto: trintaEDois},
		"base64 sem padding":        {texto: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))},
		"base64url":                 {texto: base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))},
		"com espaço em volta":       {texto: "  " + trintaEDois + "\n"},
		"32 bytes crus não é chave": {texto: strings.Repeat("k", 32), quer: cripto.ErrChaveMestraInvalida},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			chave, err := cripto.ChaveMestraDe(tc.texto)
			if !errors.Is(err, tc.quer) {
				t.Fatalf("erro = %v, quer %v", err, tc.quer)
			}
			if tc.quer == nil && !chave.Definida() {
				t.Error("chave não ficou definida com entrada válida")
			}
		})
	}
}

// TestChaveMestraDe_ErroNaoCarregaAChave: a mensagem de "chave inválida" vai
// para a saída de erro do processo, que costuma virar log de deploy.
func TestChaveMestraDe_ErroNaoCarregaAChave(t *testing.T) {
	t.Parallel()

	const quaseCerta = "bm9wZSBhaW5kYSBuYW8gc2FvIDMyIGJ5dGVz"
	_, err := cripto.ChaveMestraDe(quaseCerta)
	if err == nil {
		t.Fatal("erro = nil, quer chave inválida")
	}
	if strings.Contains(err.Error(), quaseCerta) {
		t.Errorf("mensagem = %q, quer sem o texto da chave", err.Error())
	}
}

func TestGerarChaveMestra_ServeDeEntradaParaOCofre(t *testing.T) {
	t.Parallel()

	vistas := make(map[string]bool, 50)
	for i := range 50 {
		texto, err := cripto.GerarChaveMestra()
		if err != nil {
			t.Fatalf("GerarChaveMestra na volta %d: erro = %v, quer nil", i, err)
		}
		if vistas[texto] {
			t.Fatalf("chave repetida na volta %d", i)
		}
		vistas[texto] = true

		chave, err := cripto.ChaveMestraDe(texto)
		if err != nil {
			t.Fatalf("ChaveMestraDe da chave gerada: erro = %v, quer nil", err)
		}
		if _, err := cripto.NovoCofre(chave); err != nil {
			t.Fatalf("NovoCofre: erro = %v, quer nil", err)
		}
	}
}

func TestNovoCofre_RecusaChaveIndefinida(t *testing.T) {
	t.Parallel()

	if _, err := cripto.NovoCofre(cripto.ChaveMestra{}); !errors.Is(err, cripto.ErrChaveMestraAusente) {
		t.Fatalf("erro = %v, quer %v", err, cripto.ErrChaveMestraAusente)
	}
}

// TestSegredo_NuncaSaiEmClaro é a garantia que não depende de disciplina: o tipo
// redige a si mesmo em toda formatação e no slog.
func TestSegredo_NuncaSaiEmClaro(t *testing.T) {
	t.Parallel()

	const valor = "sk-ant-valor-que-nao-pode-vazar"
	segredo := cripto.Segredo(valor)

	// Passa por any de propósito: é assim que o valor chega ao fmt e ao slog no
	// código de verdade, e é o caminho em que a redação precisa valer.
	var comoAny any = segredo

	formatos := map[string]string{
		"%v":  fmt.Sprintf("%v", segredo),
		"%s":  fmt.Sprintf("%s", comoAny),
		"%q":  fmt.Sprintf("%q", segredo),
		"%#v": fmt.Sprintf("%#v", segredo),
		"%+v": fmt.Sprintf("%+v", segredo),
		"dentro de struct": fmt.Sprintf("%v", struct {
			Token cripto.Segredo
		}{segredo}),
	}
	for nome, saida := range formatos {
		if strings.Contains(saida, valor) {
			t.Errorf("%s = %q, quer sem o valor em claro", nome, saida)
		}
		if !strings.Contains(saida, cripto.Redigido) {
			t.Errorf("%s = %q, quer a marca de redação", nome, saida)
		}
	}

	if segredo.Revelar() != valor {
		t.Errorf("Revelar() = %q, quer %q", segredo.Revelar(), valor)
	}
}

func TestChaveMestra_NuncaSaiEmClaro(t *testing.T) {
	t.Parallel()

	const texto = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	chave, err := cripto.ChaveMestraDe(texto)
	if err != nil {
		t.Fatalf("ChaveMestraDe: erro = %v, quer nil", err)
	}

	var comoAny any = chave
	for _, saida := range []string{
		fmt.Sprintf("%v", chave),
		fmt.Sprintf("%s", comoAny),
		fmt.Sprintf("%#v", chave),
	} {
		if !strings.Contains(saida, cripto.Redigido) {
			t.Errorf("formatação = %q, quer a marca de redação", saida)
		}
	}
}

// TestSegredo_NoSlog prova a redação no caminho que mais importa: o log.
func TestSegredo_NoSlog(t *testing.T) {
	t.Parallel()

	const valor = "bearer-que-nao-pode-ir-para-o-log"

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Info("credencial aplicada",
		"token", cripto.Segredo(valor),
		"chave_mestra", func() cripto.ChaveMestra {
			c, _ := cripto.ChaveMestraDe(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
			return c
		}(),
	)

	if saida := buf.String(); strings.Contains(saida, valor) {
		t.Errorf("log = %q, quer sem o valor em claro", saida)
	}
	if !strings.Contains(buf.String(), cripto.Redigido) {
		t.Errorf("log = %q, quer a marca de redação", buf.String())
	}
}
