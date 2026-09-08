package apikey_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/apikey"
)

func TestGerar(t *testing.T) {
	t.Parallel()

	a, err := apikey.Gerar()
	if err != nil {
		t.Fatalf("erro = %v, quer nil", err)
	}
	b, err := apikey.Gerar()
	if err != nil {
		t.Fatalf("erro = %v, quer nil", err)
	}

	if a.Claro == b.Claro {
		t.Error("duas chaves geradas são iguais")
	}
	if !strings.HasPrefix(a.Claro, apikey.Marca+"_") {
		t.Errorf("chave = %q, quer prefixo %q", a.Claro, apikey.Marca+"_")
	}
	if !strings.HasPrefix(a.Claro, a.PrefixoVisivel+"_") {
		t.Errorf("prefixo visível %q não é prefixo da chave %q", a.PrefixoVisivel, a.Claro)
	}
	if strings.Contains(a.Hash, a.Claro) {
		t.Error("o hash contém a chave em claro")
	}
	if a.Hash != apikey.Hash(a.Claro) {
		t.Error("hash devolvido por Gerar não bate com Hash da chave em claro")
	}
	// O segredo tem que sobrar depois do prefixo: sem isso o hash seria do
	// prefixo, que é público.
	if len(a.Claro)-len(a.PrefixoVisivel) < 40 {
		t.Errorf("segredo com %d caracteres, quer pelo menos 40",
			len(a.Claro)-len(a.PrefixoVisivel))
	}
}

func TestHash(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		a, b      string
		querIgual bool
	}{
		"mesma chave dá o mesmo hash":          {a: "pbk_abc_def", b: "pbk_abc_def", querIgual: true},
		"chaves diferentes dão hash diferente": {a: "pbk_abc_def", b: "pbk_abc_deg"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if igual := apikey.Hash(tc.a) == apikey.Hash(tc.b); igual != tc.querIgual {
				t.Fatalf("hashes iguais = %v, quer %v", igual, tc.querIgual)
			}
			if len(apikey.Hash(tc.a)) != 64 {
				t.Errorf("tamanho do hash = %d, quer 64 (sha-256 em hex)", len(apikey.Hash(tc.a)))
			}
		})
	}
}

func TestPrefixoVisivelDe(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		claro   string
		quer    string
		querErr error
	}{
		"chave bem formada": {claro: "pbk_aaaabbbb_segredo", quer: "pbk_aaaabbbb"},
		// O segredo é base64url: '_' e '-' aparecem nele, e o prefixo continua
		// sendo os dois primeiros segmentos.
		"segredo com separador dentro": {claro: "pbk_aaaabbbb_seg_re-do", quer: "pbk_aaaabbbb"},
		"sem segredo":                  {claro: "pbk_aaaabbbb_", querErr: apikey.ErrFormato},
		"sem identificador":            {claro: "pbk__segredo", querErr: apikey.ErrFormato},
		"sem marca":                    {claro: "xyz_aaaa_segredo", querErr: apikey.ErrFormato},
		"sem separador":                {claro: "pbksemseparador", querErr: apikey.ErrFormato},
		"vazia":                        {claro: "", querErr: apikey.ErrFormato},
		"só a marca":                   {claro: "pbk_", querErr: apikey.ErrFormato},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			prefixo, err := apikey.PrefixoVisivelDe(tc.claro)
			if !errors.Is(err, tc.querErr) {
				t.Fatalf("erro = %v, quer %v", err, tc.querErr)
			}
			if prefixo != tc.quer {
				t.Errorf("prefixo = %q, quer %q", prefixo, tc.quer)
			}
		})
	}
}

func TestEscopos(t *testing.T) {
	t.Parallel()

	got := apikey.Escopos([]string{"pessoal", "trabalho"})
	quer := []string{"endpoint:pessoal", "endpoint:trabalho"}

	if len(got) != len(quer) {
		t.Fatalf("escopos = %v, quer %v", got, quer)
	}
	for i := range quer {
		if got[i] != quer[i] {
			t.Errorf("escopos[%d] = %q, quer %q", i, got[i], quer[i])
		}
	}
}
