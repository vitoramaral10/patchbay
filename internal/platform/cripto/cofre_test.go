package cripto_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// campo é o contexto usado por quase todo caso: uma linha concreta de uma
// coluna concreta.
var campo = cripto.Campo{Tabela: "upstream_secret", Coluna: "valor_cifrado", ID: "1/bearer/"}

func cofreDeTeste(t *testing.T) *cripto.Cofre {
	t.Helper()

	texto, err := cripto.GerarChaveMestra()
	if err != nil {
		t.Fatalf("GerarChaveMestra: erro = %v, quer nil", err)
	}
	mestra, err := cripto.ChaveMestraDe(texto)
	if err != nil {
		t.Fatalf("ChaveMestraDe: erro = %v, quer nil", err)
	}
	cofre, err := cripto.NovoCofre(mestra)
	if err != nil {
		t.Fatalf("NovoCofre: erro = %v, quer nil", err)
	}
	return cofre
}

func TestCofre_RoundTrip(t *testing.T) {
	t.Parallel()

	casos := map[string]cripto.Segredo{
		"valor vazio":            "",
		"token opaco":            "sk-ant-0123456789abcdef",
		"acentuação e emoji":     "senha-com-ção-e-🔐",
		"valor com bytes altos":  cripto.Segredo(strings.Repeat("\xf0\x9f\x94\x90", 64)),
		"valor longo":            cripto.Segredo(strings.Repeat("a", 8192)),
		"valor com quebra':\n\t": "linha1\nlinha2\t",
	}

	for nome, claro := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut := cofreDeTeste(t)
			guardado, err := sut.Cifrar(campo, claro)
			if err != nil {
				t.Fatalf("Cifrar: erro = %v, quer nil", err)
			}
			if strings.Contains(guardado, claro.Revelar()) && !claro.Vazio() {
				t.Fatal("o valor em claro apareceu dentro do texto cifrado")
			}

			volta, err := sut.Decifrar(campo, guardado)
			if err != nil {
				t.Fatalf("Decifrar: erro = %v, quer nil", err)
			}
			if volta != claro {
				t.Errorf("valor decifrado = %q, quer %q", volta.Revelar(), claro.Revelar())
			}
		})
	}
}

// TestCofre_AADErradaFalha é a prova de que um valor cifrado não pode ser
// transplantado de uma linha para outra: quem tem escrita no banco, mas não a
// chave, não consegue apontar a credencial de um upstream para outro.
func TestCofre_AADErradaFalha(t *testing.T) {
	t.Parallel()

	origem := cripto.Campo{Tabela: "upstream_secret", Coluna: "valor_cifrado", ID: "1/bearer/"}

	casos := map[string]cripto.Campo{
		"outra linha":  {Tabela: "upstream_secret", Coluna: "valor_cifrado", ID: "2/bearer/"},
		"outro tipo":   {Tabela: "upstream_secret", Coluna: "valor_cifrado", ID: "1/header/X-Api-Key"},
		"outra coluna": {Tabela: "upstream_secret", Coluna: "outra_coluna", ID: "1/bearer/"},
		"outra tabela": {Tabela: "upstream_oauth", Coluna: "valor_cifrado", ID: "1/bearer/"},
		// Sem prefixo de tamanho no AAD, ("a","bc") e ("ab","c") colidiriam.
		"fronteira deslocada": {Tabela: "upstream_secre", Coluna: "tvalor_cifrado", ID: "1/bearer/"},
	}

	sut := cofreDeTeste(t)
	guardado, err := sut.Cifrar(origem, "segredo-da-linha-1")
	if err != nil {
		t.Fatalf("Cifrar: erro = %v, quer nil", err)
	}

	for nome, destino := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			_, err := sut.Decifrar(destino, guardado)
			if !errors.Is(err, cripto.ErrAutenticacao) {
				t.Fatalf("erro = %v, quer %v", err, cripto.ErrAutenticacao)
			}
		})
	}
}

// TestCofre_NonceUnico: em GCM, repetir nonce com a mesma chave destrói a
// confidencialidade dos dois valores e revela a chave de autenticação.
func TestCofre_NonceUnico(t *testing.T) {
	t.Parallel()

	const voltas = 500
	sut := cofreDeTeste(t)

	vistos := make(map[string]bool, voltas)
	for i := range voltas {
		guardado, err := sut.Cifrar(campo, "o mesmo valor, sempre")
		if err != nil {
			t.Fatalf("Cifrar na volta %d: erro = %v, quer nil", i, err)
		}
		nonce := nonceDe(t, guardado)
		if vistos[nonce] {
			t.Fatalf("nonce repetido na volta %d", i)
		}
		vistos[nonce] = true
	}
	if len(vistos) != voltas {
		t.Errorf("nonces distintos = %d, quer %d", len(vistos), voltas)
	}
}

// TestCofre_FormatoVersionado trava a forma de armazenamento: é ela que permite
// rotação de algoritmo sem reescrever todas as linhas de uma vez.
func TestCofre_FormatoVersionado(t *testing.T) {
	t.Parallel()

	sut := cofreDeTeste(t)
	guardado, err := sut.Cifrar(campo, "x")
	if err != nil {
		t.Fatalf("Cifrar: erro = %v, quer nil", err)
	}

	corpo, temPrefixo := strings.CutPrefix(guardado, "pbc1:")
	if !temPrefixo {
		t.Fatalf("valor guardado = %q, quer começar com \"pbc1:\"", guardado)
	}
	selado, err := base64.RawURLEncoding.DecodeString(corpo)
	if err != nil {
		t.Fatalf("corpo não é base64url sem padding: %v", err)
	}
	// 12 bytes de nonce + 16 de tag do GCM.
	if len(selado) < 12+16 {
		t.Errorf("bytes selados = %d, quer ao menos %d", len(selado), 12+16)
	}
}

func TestCofre_ValorGuardadoInvalido(t *testing.T) {
	t.Parallel()

	sut := cofreDeTeste(t)
	valido, err := sut.Cifrar(campo, "x")
	if err != nil {
		t.Fatalf("Cifrar: erro = %v, quer nil", err)
	}

	casos := map[string]struct {
		guardado string
		quer     error
	}{
		"vazio":                  {guardado: "", quer: cripto.ErrFormato},
		"sem prefixo de versão":  {guardado: strings.TrimPrefix(valido, "pbc1:"), quer: cripto.ErrFormato},
		"versão desconhecida":    {guardado: "pbc9:" + strings.TrimPrefix(valido, "pbc1:"), quer: cripto.ErrFormato},
		"base64 quebrado":        {guardado: "pbc1:não é base64", quer: cripto.ErrFormato},
		"corpo curto demais":     {guardado: "pbc1:AAAA", quer: cripto.ErrFormato},
		"texto cifrado alterado": {guardado: corromperMeio(t, valido), quer: cripto.ErrAutenticacao},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			_, err := sut.Decifrar(campo, tc.guardado)
			if !errors.Is(err, tc.quer) {
				t.Fatalf("erro = %v, quer %v", err, tc.quer)
			}
		})
	}
}

// TestCofre_ChaveTrocadaNaoDecifra é o mesmo modo de falha que o canário detecta
// no boot, um nível abaixo: chave outra, valor ilegível.
func TestCofre_ChaveTrocadaNaoDecifra(t *testing.T) {
	t.Parallel()

	antigo := cofreDeTeste(t)
	novo := cofreDeTeste(t)

	guardado, err := antigo.Cifrar(campo, "token-do-notion")
	if err != nil {
		t.Fatalf("Cifrar: erro = %v, quer nil", err)
	}
	if _, err := novo.Decifrar(campo, guardado); !errors.Is(err, cripto.ErrAutenticacao) {
		t.Fatalf("erro = %v, quer %v", err, cripto.ErrAutenticacao)
	}
}

func TestCofre_CampoIncompletoRecusado(t *testing.T) {
	t.Parallel()

	casos := map[string]cripto.Campo{
		"sem tabela": {Coluna: "valor_cifrado", ID: "1"},
		"sem coluna": {Tabela: "upstream_secret", ID: "1"},
		"sem id":     {Tabela: "upstream_secret", Coluna: "valor_cifrado"},
	}

	sut := cofreDeTeste(t)
	for nome, c := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if _, err := sut.Cifrar(c, "x"); !errors.Is(err, cripto.ErrCampoIncompleto) {
				t.Errorf("erro de Cifrar = %v, quer %v", err, cripto.ErrCampoIncompleto)
			}
			if _, err := sut.Decifrar(c, "pbc1:AAAA"); !errors.Is(err, cripto.ErrCampoIncompleto) {
				t.Errorf("erro de Decifrar = %v, quer %v", err, cripto.ErrCampoIncompleto)
			}
		})
	}
}

// TestCofre_ErroNaoCarregaOValor: a mensagem de erro vai para o log, e o log é a
// via mais fácil de vazar o que a cifra em repouso protege.
func TestCofre_ErroNaoCarregaOValor(t *testing.T) {
	t.Parallel()

	const segredo = "sk-nao-pode-aparecer-em-erro"
	antigo := cofreDeTeste(t)
	novo := cofreDeTeste(t)

	guardado, err := antigo.Cifrar(campo, segredo)
	if err != nil {
		t.Fatalf("Cifrar: erro = %v, quer nil", err)
	}
	_, err = novo.Decifrar(campo, guardado)
	if err == nil {
		t.Fatal("erro = nil, quer falha de autenticação")
	}
	if texto := err.Error(); strings.Contains(texto, segredo) || strings.Contains(texto, guardado) {
		t.Errorf("mensagem de erro = %q, quer sem o valor guardado nem o segredo", texto)
	}
}

func nonceDe(t *testing.T, guardado string) string {
	t.Helper()

	selado, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(guardado, "pbc1:"))
	if err != nil {
		t.Fatalf("decodificar valor guardado: erro = %v, quer nil", err)
	}
	if len(selado) < 12 {
		t.Fatalf("bytes selados = %d, quer ao menos 12", len(selado))
	}
	return string(selado[:12])
}

// corromperMeio decodifica o corpo selado e flipa um bit no meio dele, para
// adulterar o texto cifrado de verdade.
//
// Alternar só o último caractere base64 (como a versão anterior deste teste
// fazia) mexe apenas nos dois bits de folga do último grupo de 6 bits: em
// ~8% das cifragens esses bits caem sobre padding que não decodifica em
// nenhum byte, e às vezes 'A' e 'B' decodificam nos mesmos bytes, deixando o
// teste passar ou falhar ao acaso. Corromper um byte do meio do ciphertext
// garante uma falha de autenticação do GCM sempre.
func corromperMeio(t *testing.T, guardado string) string {
	t.Helper()

	corpo, temPrefixo := strings.CutPrefix(guardado, "pbc1:")
	if !temPrefixo {
		t.Fatalf("valor guardado = %q, quer começar com \"pbc1:\"", guardado)
	}
	selado, err := base64.RawURLEncoding.DecodeString(corpo)
	if err != nil {
		t.Fatalf("decodificar valor guardado: erro = %v, quer nil", err)
	}
	if len(selado) == 0 {
		t.Fatal("corpo selado vazio; nada para corromper")
	}
	selado[len(selado)/2] ^= 0x01
	return "pbc1:" + base64.RawURLEncoding.EncodeToString(selado)
}
