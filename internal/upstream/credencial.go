package upstream

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// Tipos de credencial estática que o patchbay apresenta a um upstream HTTP.
const (
	// CredencialBearer vira o header Authorization: Bearer <valor>.
	CredencialBearer = "bearer"
	// CredencialHeader vira um header estático de nome escolhido pelo admin.
	CredencialHeader = "header"
)

// tabelaSegredo e colunaSegredo entram no AAD da cifra. São constantes e não
// literais espalhados porque o AAD gravado precisa ser byte a byte o mesmo do
// AAD conferido — um typo aqui só apareceria como "não autentica" em produção.
const (
	tabelaSegredo = "upstream_secret"
	colunaSegredo = "valor_cifrado"
)

// Credencial é uma credencial estática com o valor em claro. Só existe em
// memória, no caminho entre o banco e o header da requisição de saída.
type Credencial struct {
	Tipo  string
	Nome  string
	Valor cripto.Segredo
}

// CredencialDefinida descreve uma credencial gravada sem o valor. É o que a UI
// vê: valor de credencial nunca é re-exibido, só "definido" ou "não definido".
type CredencialDefinida struct {
	Tipo string
	Nome string
}

// Cifrador é o mínimo da cifra de campo que este pacote consome.
//
// Declarado aqui, no consumidor: o repositório não escolhe algoritmo, e o teste
// troca a implementação sem tocar em AES.
type Cifrador interface {
	Cifrar(campo cripto.Campo, claro cripto.Segredo) (string, error)
	Decifrar(campo cripto.Campo, guardado string) (cripto.Segredo, error)
}

// LerCredenciais devolve as credenciais em claro de um upstream.
//
// O gerente a chama na hora de abrir a sessão, e não guarda o resultado: assim
// trocar o bearer pela UI vale na reconexão seguinte sem nenhum cache a
// invalidar, e um segredo passa o mínimo de tempo possível em memória.
type LerCredenciais func(ctx context.Context, upstreamID int64) ([]Credencial, error)

// ComCredenciais liga o gerente à origem das credenciais estáticas.
func ComCredenciais(fn LerCredenciais) Opcao {
	return func(g *Gerente) { g.credenciais = fn }
}

// campoDe monta o contexto da cifra de uma credencial.
//
// O identificador é a chave natural (upstream, tipo, nome) e não o id da linha:
// ele amarra o valor ao slot em que ele serve, então nem um id reaproveitado nem
// um UPDATE que troque upstream_id conseguem mover a credencial de lugar.
func campoDe(upstreamID int64, tipo, nome string) cripto.Campo {
	return cripto.Campo{
		Tabela: tabelaSegredo,
		Coluna: colunaSegredo,
		ID:     strconv.FormatInt(upstreamID, 10) + "/" + tipo + "/" + nome,
	}
}

// clienteDe devolve o cliente HTTP daquele upstream, com as credenciais
// estáticas injetadas quando existem.
//
// Um cliente por conexão e não um global: as credenciais são de um upstream só,
// e um RoundTripper compartilhado mandaria o bearer de um servidor para outro.
func (g *Gerente) clienteDe(ctx context.Context, cfg Config) (*http.Client, error) {
	if g.credenciais == nil {
		return g.cliente, nil
	}

	ctxLeitura, cancelar := context.WithTimeout(ctx, cfg.Timeout)
	defer cancelar()

	creds, err := g.credenciais(ctxLeitura, cfg.ID)
	if err != nil {
		// A mensagem diz qual upstream falhou, nunca o que ele guarda.
		return nil, fmt.Errorf("upstream %s: ler credenciais: %w", cfg.Nome, err)
	}
	if len(creds) == 0 {
		return g.cliente, nil
	}

	base := g.cliente.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copia := *g.cliente
	copia.Transport = &transporteComCredenciais{base: base, credenciais: creds}
	return &copia, nil
}

// transporteComCredenciais injeta bearer e headers estáticos em toda requisição
// de saída para aquele upstream.
//
// Header e não query string: credencial em query string vaza em log de proxy,
// em histórico e em Referer — e é justamente o vazamento que a cifra em repouso
// não teria como impedir depois. Este RoundTripper também não loga nada: um dump
// de requisição aqui reintroduziria o segredo no slog por outra porta.
type transporteComCredenciais struct {
	base        http.RoundTripper
	credenciais []Credencial
}

func (t *transporteComCredenciais) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone: um RoundTripper não pode alterar a requisição que recebe, e o SDK
	// reusa a requisição entre tentativas.
	clone := req.Clone(req.Context())
	for _, c := range t.credenciais {
		switch c.Tipo {
		case CredencialBearer:
			clone.Header.Set("Authorization", "Bearer "+c.Valor.Revelar())
		case CredencialHeader:
			clone.Header.Set(c.Nome, c.Valor.Revelar())
		}
	}
	return t.base.RoundTrip(clone)
}

// NomeDeHeaderValido aceita o que a RFC 9110 chama de token, que é o conjunto de
// caracteres permitidos num nome de header.
//
// Existe para que a UI recuse na hora, com mensagem: um nome inválido só
// apareceria como requisição malformada muito depois, no upstream.
func NomeDeHeaderValido(nome string) bool {
	if nome == "" {
		return false
	}
	const especiais = "!#$%&'*+-.^_`|~"
	for _, r := range nome {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(especiais, r):
		default:
			return false
		}
	}
	return true
}

// ValorDeHeaderValido recusa controle e quebra de linha, que é o que permitiria
// injetar um segundo header a partir de um valor.
func ValorDeHeaderValido(valor string) bool {
	for _, r := range valor {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
