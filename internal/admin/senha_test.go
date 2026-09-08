package admin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/admin"
)

// baratos são os custos usados no teste. O objetivo aqui é o formato e a
// comparação, não a resistência a GPU — pagar 64 MiB por caso tornaria a suíte
// inútil de rodar.
var baratos = admin.Parametros{Tempo: 1, Memoria: 8 * 1024, Threads: 1, Tamanho: 16, BytesSal: 8}

func TestHashSenha_FormatoPHC(t *testing.T) {
	t.Parallel()

	hash, err := admin.HashSenha("senha-de-doze-caracteres", baratos)
	if err != nil {
		t.Fatalf("HashSenha: erro = %v, quer nil", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("hash = %q, quer prefixo com os custos em vigor", hash)
	}
	if n := strings.Count(hash, "$"); n != 5 {
		t.Errorf("separadores = %d, quer 5 (algoritmo, versão, custos, sal, hash)", n)
	}
}

func TestHashSenha_SalDiferentePorChamada(t *testing.T) {
	t.Parallel()

	primeiro, err := admin.HashSenha("mesma-senha-aqui", baratos)
	if err != nil {
		t.Fatalf("HashSenha: erro = %v, quer nil", err)
	}
	segundo, err := admin.HashSenha("mesma-senha-aqui", baratos)
	if err != nil {
		t.Fatalf("HashSenha: erro = %v, quer nil", err)
	}
	if primeiro == segundo {
		t.Error("dois hashes da mesma senha são iguais, quer sal sorteado por chamada")
	}
}

func TestVerificarSenha(t *testing.T) {
	t.Parallel()

	valido, err := admin.HashSenha("senha-certa-mesmo", baratos)
	if err != nil {
		t.Fatalf("HashSenha: erro = %v, quer nil", err)
	}

	casos := map[string]struct {
		senha      string
		hash       string
		querOK     bool
		querErroDe error
	}{
		"aceita a senha certa":            {senha: "senha-certa-mesmo", hash: valido, querOK: true},
		"recusa a senha errada":           {senha: "senha-errada-aqui", hash: valido},
		"recusa senha vazia":              {senha: "", hash: valido},
		"erra em hash sem prefixo":        {senha: "x", hash: "argon2id$m=1$a$b", querErroDe: admin.ErrHashInvalido},
		"erra em hash de outro algoritmo": {senha: "x", hash: "$argon2i$v=19$m=8192,t=1,p=1$YWJj$YWJj", querErroDe: admin.ErrHashInvalido},
		"erra em versão não suportada":    {senha: "x", hash: "$argon2id$v=18$m=8192,t=1,p=1$YWJj$YWJj", querErroDe: admin.ErrHashInvalido},
		"erra em base64 quebrado":         {senha: "x", hash: "$argon2id$v=19$m=8192,t=1,p=1$!!!$YWJj", querErroDe: admin.ErrHashInvalido},
		"erra em custos ilegíveis":        {senha: "x", hash: "$argon2id$v=19$memoria$YWJj$YWJj", querErroDe: admin.ErrHashInvalido},
		"erra em hash vazio":              {senha: "x", hash: "", querErroDe: admin.ErrHashInvalido},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			ok, err := admin.VerificarSenha(tc.senha, tc.hash)
			if tc.querErroDe != nil {
				if !errors.Is(err, tc.querErroDe) {
					t.Fatalf("erro = %v, quer %v", err, tc.querErroDe)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro = %v, quer nil", err)
			}
			if ok != tc.querOK {
				t.Errorf("ok = %v, quer %v", ok, tc.querOK)
			}
		})
	}
}

func TestHashSenha_RecusaParametroBarato(t *testing.T) {
	t.Parallel()

	// Configuração que produziria hash barato por acidente precisa falhar alto, e
	// não gravar em silêncio uma senha mal protegida.
	if _, err := admin.HashSenha("qualquer", admin.Parametros{}); err == nil {
		t.Error("erro = nil, quer recusa de parâmetros inválidos")
	}
}
