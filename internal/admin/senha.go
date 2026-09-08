package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Parametros são os custos do argon2id.
//
// argon2id e não bcrypt: bcrypt tem teto de 72 bytes e não tem custo de memória,
// então não resiste a GPU de forma comparável (seção 10 do estudo).
type Parametros struct {
	// Tempo é o número de passes (t).
	Tempo uint32
	// Memoria é o custo de memória em KiB (m).
	Memoria uint32
	// Threads é o paralelismo (p).
	Threads uint8
	// Tamanho é o tamanho da chave derivada, em bytes.
	Tamanho uint32
	// BytesSal é o tamanho do sal sorteado, em bytes.
	BytesSal uint32
}

// ParametrosPadrao segue a recomendação do OWASP para argon2id: 64 MiB, dois
// passes, quatro lanes.
var ParametrosPadrao = Parametros{
	Tempo:    2,
	Memoria:  64 * 1024,
	Threads:  4,
	Tamanho:  32,
	BytesSal: 16,
}

// Valido recusa parâmetro que produziria hash barato por acidente de
// configuração.
func (p Parametros) Valido() bool {
	return p.Tempo >= 1 && p.Memoria >= 8*1024 && p.Threads >= 1 &&
		p.Tamanho >= 16 && p.BytesSal >= 8
}

const versaoArgon2 = 19

// HashSenha deriva o hash de armazenamento de uma senha.
//
// A saída é a string PHC (`$argon2id$v=19$m=...,t=...,p=...$sal$hash`): os
// parâmetros viajam junto com o hash, então trocá-los no futuro não invalida as
// senhas já gravadas — a verificação usa os parâmetros da linha, não os do
// código.
func HashSenha(senha string, p Parametros) (string, error) {
	if !p.Valido() {
		return "", fmt.Errorf("admin: parâmetros de argon2id inválidos: %+v", p)
	}
	sal := make([]byte, p.BytesSal)
	if _, err := rand.Read(sal); err != nil {
		return "", fmt.Errorf("admin: sortear sal: %w", err)
	}
	chave := argon2.IDKey([]byte(senha), sal, p.Tempo, p.Memoria, p.Threads, p.Tamanho)

	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		versaoArgon2, p.Memoria, p.Tempo, p.Threads,
		b64.EncodeToString(sal), b64.EncodeToString(chave)), nil
}

// VerificarSenha confere uma senha contra o hash gravado.
//
// Devolve erro só quando o hash está malformado — senha errada é (false, nil).
// A comparação é de tempo constante.
func VerificarSenha(senha, codificado string) (bool, error) {
	p, sal, esperado, err := decodificar(codificado)
	if err != nil {
		return false, err
	}
	obtido := argon2.IDKey([]byte(senha), sal, p.Tempo, p.Memoria, p.Threads, uint32(len(esperado)))
	return subtle.ConstantTimeCompare(obtido, esperado) == 1, nil
}

func decodificar(codificado string) (p Parametros, sal, chave []byte, err error) {
	partes := strings.Split(codificado, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", sal, hash]
	if len(partes) != 6 || partes[0] != "" || partes[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: prefixo", ErrHashInvalido)
	}

	var versao int
	if _, err := fmt.Sscanf(partes[2], "v=%d", &versao); err != nil {
		return p, nil, nil, fmt.Errorf("%w: versão: %w", ErrHashInvalido, err)
	}
	if versao != versaoArgon2 {
		return p, nil, nil, fmt.Errorf("%w: versão %d não suportada", ErrHashInvalido, versao)
	}
	if _, err := fmt.Sscanf(partes[3], "m=%d,t=%d,p=%d", &p.Memoria, &p.Tempo, &p.Threads); err != nil {
		return p, nil, nil, fmt.Errorf("%w: custos: %w", ErrHashInvalido, err)
	}

	b64 := base64.RawStdEncoding
	if sal, err = b64.DecodeString(partes[4]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: sal: %w", ErrHashInvalido, err)
	}
	if chave, err = b64.DecodeString(partes[5]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: hash: %w", ErrHashInvalido, err)
	}
	if len(sal) == 0 || len(chave) == 0 {
		return p, nil, nil, fmt.Errorf("%w: sal ou hash vazio", ErrHashInvalido)
	}
	p.BytesSal = uint32(len(sal))
	p.Tamanho = uint32(len(chave))
	return p, sal, chave, nil
}
