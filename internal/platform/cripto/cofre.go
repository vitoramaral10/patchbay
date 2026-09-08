package cripto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// Formato de armazenamento.
//
//	pbc1:<base64url sem padding de (nonce ‖ selado)>
//
// O prefixo de versão é o que permite rotação: um dia "pbc2" pode significar
// outra função de derivação ou outra cifra, e Decifrar despacha pelo prefixo
// enquanto as linhas antigas continuam legíveis. Sem ele, trocar de algoritmo
// exigiria reescrever todas as linhas de uma vez — que é exatamente a operação
// que não dá para fazer com o processo no ar.
//
// O nonce viaja dentro do valor, e não numa coluna própria como a tabela da
// seção 08.8 desenhou. Coluna separada permite gravar valor e nonce de linhas
// diferentes, e um par trocado por acidente é indistinguível de chave errada na
// hora de diagnosticar. Ele pertence ao texto cifrado, então mora com ele.
const (
	prefixoV1 = "pbc1:"

	// Contexto da derivação. Salt e info são fixos e versionados: derivar duas
	// chaves diferentes do mesmo segredo é o serviço que o HKDF presta, e é o
	// que mantém a cifra de campo separada de qualquer outra chave futura.
	salHKDF     = "patchbay:cripto:v1"
	infoCifraV1 = "patchbay:cifra-de-campo:aes-256-gcm:v1"

	// rotuloAADV1 entra no dado autenticado para que um valor da versão 1 não
	// possa ser aceito como se fosse de outra versão do formato.
	rotuloAADV1 = "pbc1"

	bytesChaveAES = 32
)

// Erros sentinela da cifra.
var (
	// ErrFormato indica valor guardado que não tem a forma de um valor cifrado
	// desta implementação — prefixo desconhecido, base64 quebrado, curto demais.
	ErrFormato = errors.New("cripto: valor cifrado com formato inválido")

	// ErrAutenticacao indica valor que não autentica: ou a chave mestra é outra,
	// ou o valor foi transplantado de outra linha, ou o texto foi adulterado.
	// GCM não distingue os três casos, e não deveria.
	ErrAutenticacao = errors.New("cripto: valor cifrado não autentica")

	// ErrCampoIncompleto indica pedido de cifra sem o contexto da linha.
	ErrCampoIncompleto = errors.New("cripto: campo sem tabela, coluna ou id")
)

// Campo é de onde o valor cifrado vem: a tabela, a coluna e o identificador da
// linha.
//
// Ele entra como dado autenticado adicional (AAD) do GCM, e é isso que impede
// transplante: um valor cifrado copiado da linha do upstream A para a linha do
// upstream B decifra com a chave certa mas falha a autenticação, porque o AAD
// gravado não bate com o AAD do lugar onde ele apareceu. Sem AAD, quem tem
// acesso de escrita ao banco — e não à chave — consegue apontar a credencial de
// um upstream para outro.
type Campo struct {
	// Tabela é o nome da tabela SQL.
	Tabela string
	// Coluna é o nome da coluna que guarda o valor cifrado.
	Coluna string
	// ID identifica a linha. Pode ser a chave primária ou a chave natural; o
	// que importa é ser estável e único dentro da coluna.
	ID string
}

func (c Campo) validar() error {
	if c.Tabela == "" || c.Coluna == "" || c.ID == "" {
		return fmt.Errorf("%w: %q/%q/%q", ErrCampoIncompleto, c.Tabela, c.Coluna, c.ID)
	}
	return nil
}

// aad monta o dado autenticado adicional.
//
// Cada parte vai precedida do próprio tamanho, e não separada por um caractere:
// com separador, ("ab", "c") e ("a", "bc") produzem o mesmo AAD, e o teto que o
// AAD levanta contra transplante desce junto.
func (c Campo) aad() []byte {
	partes := [...]string{rotuloAADV1, c.Tabela, c.Coluna, c.ID}
	tamanho := 0
	for _, p := range partes {
		tamanho += len(p) + binary.MaxVarintLen64
	}

	b := make([]byte, 0, tamanho)
	for _, p := range partes {
		b = binary.AppendUvarint(b, uint64(len(p)))
		b = append(b, p...)
	}
	return b
}

// Cofre cifra e decifra valores de campo com a chave derivada da chave mestra.
//
// É seguro para uso concorrente: cipher.AEAD é imutável depois de construído e
// o nonce é sorteado por chamada.
type Cofre struct {
	v1 cipher.AEAD
}

// NovoCofre deriva a chave de cifra da chave mestra por HKDF-SHA256.
//
// A derivação existe para que a chave mestra nunca seja usada diretamente como
// chave de AES: com HKDF, acrescentar um segundo uso (outra cifra, um MAC) é
// trocar o info, e não reutilizar a mesma chave em dois algoritmos.
func NovoCofre(mestra ChaveMestra) (*Cofre, error) {
	if !mestra.Definida() {
		return nil, ErrChaveMestraAusente
	}

	derivada := make([]byte, bytesChaveAES)
	leitor := hkdf.New(sha256.New, mestra.bruta, []byte(salHKDF), []byte(infoCifraV1))
	if _, err := io.ReadFull(leitor, derivada); err != nil {
		return nil, fmt.Errorf("cripto: derivar chave de cifra: %w", err)
	}

	bloco, err := aes.NewCipher(derivada)
	if err != nil {
		return nil, fmt.Errorf("cripto: montar AES-256: %w", err)
	}
	gcm, err := cipher.NewGCM(bloco)
	if err != nil {
		return nil, fmt.Errorf("cripto: montar GCM: %w", err)
	}
	return &Cofre{v1: gcm}, nil
}

// Cifrar devolve o valor pronto para gravar na coluna.
//
// O nonce é sorteado a cada chamada: em GCM, repetir nonce com a mesma chave
// destrói a confidencialidade dos dois valores e revela a chave de
// autenticação. Nunca é derivado do conteúdo nem contado a partir de um estado.
func (c *Cofre) Cifrar(campo Campo, claro Segredo) (string, error) {
	if err := campo.validar(); err != nil {
		return "", err
	}

	nonce := make([]byte, c.v1.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cripto: sortear nonce: %w", err)
	}

	// Seal escreve o selado logo depois do nonce, no mesmo buffer: a saída já
	// sai na forma nonce ‖ texto cifrado ‖ tag.
	selado := c.v1.Seal(nonce, nonce, []byte(claro), campo.aad())
	return prefixoV1 + base64.RawURLEncoding.EncodeToString(selado), nil
}

// Decifrar devolve o valor em claro de uma coluna.
//
// A mensagem de erro nunca carrega o valor guardado nem parte dele: um erro de
// decifra vai para o log, e o log é a via mais fácil de vazar exatamente o que
// a cifra em repouso protege (seção 11).
func (c *Cofre) Decifrar(campo Campo, guardado string) (Segredo, error) {
	if err := campo.validar(); err != nil {
		return "", err
	}

	corpo, ok := strings.CutPrefix(guardado, prefixoV1)
	if !ok {
		return "", fmt.Errorf("%w: versão de formato desconhecida", ErrFormato)
	}
	selado, err := base64.RawURLEncoding.DecodeString(corpo)
	if err != nil {
		return "", fmt.Errorf("%w: base64", ErrFormato)
	}
	if len(selado) < c.v1.NonceSize()+c.v1.Overhead() {
		return "", fmt.Errorf("%w: corpo curto demais", ErrFormato)
	}

	nonce, resto := selado[:c.v1.NonceSize()], selado[c.v1.NonceSize():]
	claro, err := c.v1.Open(nil, nonce, resto, campo.aad())
	if err != nil {
		return "", ErrAutenticacao
	}
	return Segredo(claro), nil
}
