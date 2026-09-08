// Package cripto é a cifra de campo do patchbay: AES-256-GCM com chave
// derivada por HKDF da chave mestra.
//
// Ele existe por causa da separação da seção 08.8 do estudo prévio. São duas
// classes de segredo, e confundi-las é irreversível: o que o patchbay
// *apresenta* (bearer de upstream, header estático, client secret, token OAuth)
// precisa voltar em claro e por isso vai em cifra simétrica reversível; o que
// ele *verifica* (chave de API, sessão de admin, code e token que ele mesmo
// emitiu) nunca precisa voltar e vai em hash. Guardar a segunda classe de forma
// reversível cria um cofre de credencial alheia sem necessidade nenhuma;
// guardar a primeira como hash simplesmente não funciona.
//
// A chave mestra vem exclusivamente de PATCHBAY_MASTER_KEY (decisão da seção
// 12): nenhum arquivo de chave em disco, nenhuma entrada pela UI. Sem ela o
// processo não sobe. O canário (canario.go) é o que transforma "a chave mudou"
// de corrupção silenciosa em indisponibilidade explícita.
package cripto

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// VarChaveMestra é a única origem da chave mestra.
const VarChaveMestra = "PATCHBAY_MASTER_KEY"

// BytesChaveMestra é o tamanho exigido da chave mestra depois de decodificada.
//
// 32 bytes porque é o material que o HKDF expande para a chave de AES-256 —
// entrada menor que a saída não acrescenta entropia, só esconde que ela falta.
const BytesChaveMestra = 32

// Redigido é o que aparece no lugar de qualquer segredo formatado ou logado.
const Redigido = "«redigido»"

// Erros sentinela do pacote.
var (
	// ErrChaveMestraAusente indica PATCHBAY_MASTER_KEY vazia ou não definida.
	ErrChaveMestraAusente = errors.New("cripto: " + VarChaveMestra + " não definida")
	// ErrChaveMestraInvalida indica chave presente mas que não é base64 de 32 bytes.
	ErrChaveMestraInvalida = errors.New("cripto: chave mestra inválida")
)

// Segredo é um valor em claro que nunca pode chegar ao log nem a uma mensagem
// de erro.
//
// O tipo existe para que isso não dependa de disciplina: String, GoString e
// LogValue devolvem a marca de redação, então %v, %s, %#v e slog imprimem
// «redigido» mesmo quando alguém esquece. Ler o valor de verdade exige dizer o
// nome — Revelar — e essa chamada é grep-ável numa revisão.
type Segredo string

// String satisfaz fmt.Stringer com a marca de redação.
func (s Segredo) String() string { return Redigido }

// GoString satisfaz fmt.GoStringer, que é o que %#v usa.
func (s Segredo) GoString() string { return Redigido }

// LogValue satisfaz slog.LogValuer: o valor nunca chega ao handler.
func (s Segredo) LogValue() slog.Value { return slog.StringValue(Redigido) }

// Revelar devolve o valor em claro. É a única forma de sair do tipo.
func (s Segredo) Revelar() string { return string(s) }

// Vazio informa se não há valor nenhum.
func (s Segredo) Vazio() bool { return s == "" }

// ChaveMestra é o material de 32 bytes de onde toda chave de cifra é derivada.
//
// É struct e não []byte pelo mesmo motivo de Segredo: o tipo carrega a redação
// junto, e não existe caminho acidental do conteúdo para o log.
type ChaveMestra struct {
	bruta []byte
}

// String, GoString e LogValue redigem a chave mestra.
func (c ChaveMestra) String() string       { return Redigido }
func (c ChaveMestra) GoString() string     { return Redigido }
func (c ChaveMestra) LogValue() slog.Value { return slog.StringValue(Redigido) }

// Definida informa se a chave tem material.
func (c ChaveMestra) Definida() bool { return len(c.bruta) == BytesChaveMestra }

// ChaveMestraDe decodifica o texto de PATCHBAY_MASTER_KEY.
//
// Recebe o texto em vez de ler o ambiente sozinha para que a validação seja
// testável em paralelo: t.Setenv e t.Parallel não convivem.
//
// O formato documentado é base64 de 32 bytes. As quatro variantes de base64 são
// aceitas porque a diferença entre elas é invisível para quem copiou a chave de
// um gerenciador de segredo, e recusar por causa de padding transformaria um
// erro de digitação num incidente de indisponibilidade.
func ChaveMestraDe(texto string) (ChaveMestra, error) {
	texto = strings.TrimSpace(texto)
	if texto == "" {
		return ChaveMestra{}, ErrChaveMestraAusente
	}

	bruta, ok := decodificarBase64(texto)
	if !ok {
		return ChaveMestra{}, fmt.Errorf("%w: não é base64", ErrChaveMestraInvalida)
	}
	if len(bruta) != BytesChaveMestra {
		// O tamanho entra na mensagem; o conteúdo, nunca.
		return ChaveMestra{}, fmt.Errorf("%w: %d bytes decodificados, quer %d",
			ErrChaveMestraInvalida, len(bruta), BytesChaveMestra)
	}
	return ChaveMestra{bruta: bruta}, nil
}

// GerarChaveMestra sorteia uma chave mestra nova e devolve o texto que vai em
// PATCHBAY_MASTER_KEY.
func GerarChaveMestra() (string, error) {
	bruta := make([]byte, BytesChaveMestra)
	if _, err := rand.Read(bruta); err != nil {
		return "", fmt.Errorf("cripto: sortear chave mestra: %w", err)
	}
	return base64.StdEncoding.EncodeToString(bruta), nil
}

func decodificarBase64(texto string) ([]byte, bool) {
	codificacoes := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, c := range codificacoes {
		if b, err := c.DecodeString(texto); err == nil {
			return b, true
		}
	}
	return nil, false
}
