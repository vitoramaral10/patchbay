package catalogo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Vinculo é uma linha da composição endpoint↔upstream.
type Vinculo struct {
	UpstreamID   int64
	UpstreamNome string
	Prefixo      string
}

// Composicao é o que o Servico precisa da persistência.
type Composicao interface {
	// Vinculos devolve os upstreams de um endpoint, na ordem da composição.
	Vinculos(ctx context.Context, endpointID int64) ([]Vinculo, error)
}

// Descobridor é o que o Servico precisa do gerente de upstreams: o último
// tools/list bem-sucedido, já em memória.
//
// A assinatura é deliberadamente sem erro e sem contexto: materializar um
// endpoint lê um snapshot, nunca fala com o upstream. Se falasse, o hang de um
// upstream viraria latência do endpoint (decisão 4 do estudo).
type Descobridor interface {
	Ferramentas(upstreamID int64) []*mcp.Tool
}

// Servico materializa o catálogo de um endpoint.
type Servico struct {
	comp Composicao
	desc Descobridor
	log  *slog.Logger
}

// NovoServico monta o serviço de catálogo.
func NovoServico(comp Composicao, desc Descobridor, log *slog.Logger) *Servico {
	return &Servico{comp: comp, desc: desc, log: log}
}

// Materializar devolve as ferramentas do endpoint, normalizadas e sem colisão
// de nome.
//
// Endpoint sem nenhum upstream pronto devolve lista vazia e nenhum erro: erro
// no tools/list faz o cliente marcar o servidor inteiro como quebrado, e lista
// vazia é degradação legível (seção 11).
func (s *Servico) Materializar(ctx context.Context, endpointID int64) ([]Ferramenta, error) {
	vinculos, err := s.comp.Vinculos(ctx, endpointID)
	if err != nil {
		return nil, fmt.Errorf("catalogo: composição do endpoint %d: %w", endpointID, err)
	}

	origens := make([]Origem, 0, len(vinculos))
	for _, v := range vinculos {
		origens = append(origens, Origem{
			UpstreamID:  v.UpstreamID,
			Nome:        v.UpstreamNome,
			Prefixo:     v.Prefixo,
			Ferramentas: s.desc.Ferramentas(v.UpstreamID),
		})
	}
	return Materializar(s.log, origens), nil
}
