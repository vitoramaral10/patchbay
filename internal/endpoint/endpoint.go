// Package endpoint mantém um *mcp.Server vivo por endpoint e o serve em
// /mcp/{slug}.
//
// É o único ponto que pode disparar tools/list_changed: a notificação sai de
// AddTool/RemoveTools (mcp/server.go:319, mcp/server.go:571), e essas são
// operações de instância. Por isso o *mcp.Server de um endpoint é criado uma
// vez e atualizado no lugar — recriá-lo derrubaria as sessões retidas.
package endpoint

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

// Erros sentinela do pacote.
var (
	// ErrNaoEncontrado indica endpoint que não existe no banco.
	ErrNaoEncontrado = errors.New("endpoint: não encontrado")
	// ErrSlugEmUso indica tentativa de criar endpoint com slug já cadastrado.
	ErrSlugEmUso = errors.New("endpoint: slug já em uso")
)

// Registro é o endpoint como ele existe no banco. O slug é contrato: ele entra
// na URL e no resource do token, e renomeá-lo invalida silenciosamente a
// credencial de todo cliente daquele endpoint — por isso não existe UPDATE de
// slug, nem no repositório nem na tela.
type Registro struct {
	ID         int64
	Slug       string
	Nome       string
	Descricao  string
	Instrucoes string
}

// Titulo é o rótulo humano do endpoint, com o slug como reserva para que a UI e
// o initialize do MCP nunca mostrem um endpoint sem nome.
func (r Registro) Titulo() string {
	if r.Nome != "" {
		return r.Nome
	}
	return r.Slug
}

// identidadeMudou informa se os campos que só entram no *mcp.Server na
// construção mudaram. Ver o comentário em Servidores.reconciliar.
func (r Registro) identidadeMudou(outro Registro) bool {
	return r.Nome != outro.Nome ||
		r.Descricao != outro.Descricao ||
		r.Instrucoes != outro.Instrucoes
}

// Catalogo é o que este pacote precisa do catálogo. A interface é declarada
// aqui, no consumidor, com o único método usado.
type Catalogo interface {
	Materializar(ctx context.Context, endpointID int64) ([]catalogo.Ferramenta, error)
}

// Executor é o que este pacote precisa do gerente de upstreams: executar a
// chamada numa sessão já aberta.
type Executor interface {
	Chamar(ctx context.Context, upstreamID int64, nome string, args json.RawMessage) (*mcp.CallToolResult, error)
}

// Repositorio é o que este pacote precisa da persistência para servir MCP.
type Repositorio interface {
	Todos(ctx context.Context) ([]Registro, error)
}
