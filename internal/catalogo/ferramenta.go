// Package catalogo normaliza a ferramenta que vem de um upstream e materializa
// a tabela de ferramentas de cada endpoint.
//
// É a fronteira de confiança do patchbay: tudo que vem de terceiro passa por
// aqui antes de chegar ao SDK, porque (*mcp.Server).AddTool entra em panic com
// dado malformado em oito pontos (mcp/server.go:273-313, verificado na v1.7.0).
// Ferramenta que não normaliza é descartada com log — nunca derruba o processo.
package catalogo

import "github.com/modelcontextprotocol/go-sdk/mcp"

// Aviso registra o que a normalização precisou mudar numa ferramenta. A UI da
// fatia 2 mostra esses avisos ao lado do nome: normalização silenciosa é
// indistinguível de servidor quebrado.
type Aviso string

// Avisos possíveis, um por regra do normalizador.
const (
	// AvisoNomeSaneado: o nome vinha fora das regras do SDK e foi corrigido.
	AvisoNomeSaneado Aviso = "nome_saneado"
	// AvisoColisaoDeNome: outro upstream do mesmo endpoint já usava o nome.
	AvisoColisaoDeNome Aviso = "colisao_de_nome"
	// AvisoSchemaDegradado: o input schema foi substituído por objeto permissivo.
	AvisoSchemaDegradado Aviso = "schema_degradado"
	// AvisoOutputDescartado: o output schema não serializa e foi removido.
	AvisoOutputDescartado Aviso = "output_descartado"
	// AvisoAnotacaoRemovida: anotação x-mcp-header removida do input schema.
	AvisoAnotacaoRemovida Aviso = "anotacao_removida"
)

// Origem é um upstream dentro de um endpoint, com o prefixo e as regras daquela
// composição e o que ele expôs no último tools/list bem-sucedido.
//
// Prefixo e Regras são por origem e não por upstream: o mesmo upstream entra em
// vários endpoints com composições diferentes, e é isto que faz cada endpoint
// ver um catálogo próprio a partir do mesmo snapshot.
type Origem struct {
	UpstreamID  int64
	Nome        string
	Prefixo     string
	Regras      []Regra
	Ferramentas []*mcp.Tool
}

// Ferramenta é uma ferramenta pronta para entrar num *mcp.Server, com a origem
// que o handler precisa para rotear a chamada.
type Ferramenta struct {
	UpstreamID   int64
	UpstreamNome string
	// NomeOriginal é o nome no upstream: é ele que vai no tools/call de saída.
	NomeOriginal string
	// Tool é a ferramenta normalizada. Já passou pelas oito validações que o
	// AddTool faz em panic.
	Tool *mcp.Tool
	// Avisos é o que a normalização mudou, em ordem estável.
	Avisos []Aviso
}

// NomeExposto é o nome que o cliente vê e chama.
func (f Ferramenta) NomeExposto() string {
	if f.Tool == nil {
		return ""
	}
	return f.Tool.Name
}

// TemAviso informa se a ferramenta carrega o aviso a.
func (f Ferramenta) TemAviso(a Aviso) bool {
	for _, x := range f.Avisos {
		if x == a {
			return true
		}
	}
	return false
}
