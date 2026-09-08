// Package apikey emite e verifica a chave de API por cliente, com escopo de
// endpoints.
//
// A chave é uma credencial que o patchbay verifica, nunca uma que ele
// apresenta: é guardada como hash e não volta em claro. Quem confunde as duas
// classes de segredo (seção 08.8 do estudo prévio) cria um cofre de credencial
// alheia sem necessidade.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Marca é o prefixo textual de toda chave emitida pelo patchbay. Existe para
// que a chave seja reconhecível num log ou num arquivo de configuração alheio.
const Marca = "pbk"

const (
	bytesIdentificador = 5  // 8 caracteres base32 no prefixo visível
	bytesSegredo       = 32 // 256 bits de entropia
)

// Erros sentinela do pacote.
var (
	// ErrNaoEncontrada indica que nenhuma chave ativa casa com o hash apresentado.
	ErrNaoEncontrada = errors.New("apikey: chave não encontrada")
	// ErrRevogada indica que a chave existe mas foi revogada.
	ErrRevogada = errors.New("apikey: chave revogada")
	// ErrSemEscopo indica que a chave não dá acesso ao endpoint pedido.
	ErrSemEscopo = errors.New("apikey: chave sem escopo para o endpoint")
	// ErrFormato indica que o texto apresentado não tem a forma de uma chave.
	ErrFormato = errors.New("apikey: formato inválido")
)

// Chave é o registro de uma chave emitida, sem o segredo.
type Chave struct {
	ID             int64
	Nome           string
	PrefixoVisivel string
	Endpoints      []string // slugs a que a chave dá acesso
	CriadaEm       time.Time
	RevogadaEm     time.Time
	UltimoUsoEm    time.Time
}

// Revogada informa se a chave já foi revogada.
func (c Chave) Revogada() bool { return !c.RevogadaEm.IsZero() }

// Emitida é o resultado de Gerar: o texto claro aparece uma única vez, na
// criação, e depois só o prefixo visível.
type Emitida struct {
	Claro          string
	PrefixoVisivel string
	Hash           string
}

var base32Chave = base32.StdEncoding.WithPadding(base32.NoPadding)

// Gerar sorteia uma chave nova.
//
// A forma é pbk_<identificador>_<segredo>: o prefixo visível é tudo até o
// último separador e é o que a UI guarda para dizer qual chave é qual.
func Gerar() (Emitida, error) {
	ident := make([]byte, bytesIdentificador)
	if _, err := rand.Read(ident); err != nil {
		return Emitida{}, fmt.Errorf("apikey: sortear identificador: %w", err)
	}
	segredo := make([]byte, bytesSegredo)
	if _, err := rand.Read(segredo); err != nil {
		return Emitida{}, fmt.Errorf("apikey: sortear segredo: %w", err)
	}

	prefixo := Marca + "_" + strings.ToLower(base32Chave.EncodeToString(ident))
	claro := prefixo + "_" + base64.RawURLEncoding.EncodeToString(segredo)
	return Emitida{Claro: claro, PrefixoVisivel: prefixo, Hash: Hash(claro)}, nil
}

// Hash devolve o hash de armazenamento de uma chave em claro.
//
// SHA-256 e não argon2id de propósito: a chave é sorteada com 256 bits de
// entropia, então não existe ataque de dicionário a encarecer — argon2id fica
// reservado à senha do admin, que é escolhida por uma pessoa.
func Hash(claro string) string {
	soma := sha256.Sum256([]byte(claro))
	return hex.EncodeToString(soma[:])
}

// PrefixoVisivelDe extrai o prefixo visível de uma chave em claro, sem validar
// o segredo. Serve para logar qual chave falhou sem logar a chave.
func PrefixoVisivelDe(claro string) (string, error) {
	// SplitN em três e não busca pelo último separador: o segredo é base64url e
	// pode conter '_', então o último separador da string cai dentro dele com
	// alguma frequência — e o "prefixo" logado deixaria de bater com o
	// prefixo_visivel gravado no banco, que é o que a UI mostra.
	partes := strings.SplitN(claro, "_", 3)
	if len(partes) != 3 || partes[0] != Marca || partes[1] == "" || partes[2] == "" {
		return "", ErrFormato
	}
	return partes[0] + "_" + partes[1], nil
}

// Escopo é o nome do escopo que representa acesso a um endpoint.
//
// É o mesmo texto nos dois lados da verificação: o que a chave carrega em
// TokenInfo.Scopes e o que o middleware do go-sdk exige por requisição.
func Escopo(slug string) string { return "endpoint:" + slug }

// Escopos traduz os slugs de uma chave para os escopos do middleware.
func Escopos(slugs []string) []string {
	escopos := make([]string, 0, len(slugs))
	for _, s := range slugs {
		escopos = append(escopos, Escopo(s))
	}
	return escopos
}
