package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// Os adaptadores deste arquivo são as costuras entre features.
//
// Cada feature declara a interface mínima do que consome (endpoint.Upstreams,
// apikey.Endpoints, upstream.NomeExpostoDe) e nenhuma importa a outra. Quem
// conhece as duas pontas é este pacote — é a única coisa que main faz de
// diferente de todo mundo.

// upstreamsParaEndpoint responde à tela de composição de endpoint: quais
// upstreams existem, em que estado estão e quantas ferramentas cada um traz.
type upstreamsParaEndpoint struct {
	repo    *upstream.RepositorioSQLite
	gerente *upstream.Gerente
}

// Opcoes implementa endpoint.Upstreams.
func (a upstreamsParaEndpoint) Opcoes(ctx context.Context) ([]endpoint.UpstreamOpcao, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]endpoint.UpstreamOpcao, 0, len(regs))
	for _, reg := range regs {
		o := endpoint.UpstreamOpcao{ID: reg.ID, Nome: reg.Nome, Habilitado: reg.Habilitado}
		// O estado e a contagem vêm da memória, não do banco: o banco guarda a
		// intenção do admin (habilitado), e o estado da máquina vive no gerente.
		if s, ok := a.gerente.Situacao(reg.ID); ok {
			o.Estado, o.Ferramentas = string(s.Estado), s.Ferramentas
		}
		out = append(out, o)
	}
	return out, nil
}

// endpointsParaChave responde à tela de escopo de chave de API.
type endpointsParaChave struct {
	repo *endpoint.RepositorioSQLite
}

// Opcoes implementa apikey.Endpoints.
func (a endpointsParaChave) Opcoes(ctx context.Context) ([]apikey.EndpointOpcao, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]apikey.EndpointOpcao, 0, len(regs))
	for _, reg := range regs {
		out = append(out, apikey.EndpointOpcao{ID: reg.ID, Slug: reg.Slug, Nome: reg.Nome})
	}
	return out, nil
}

// nomeExpostoDe traduz uma ferramenta bruta no nome que o cliente veria.
//
// Sem prefixo: o prefixo é da composição, e a tela de detalhe do upstream mostra
// o upstream isolado. Ferramenta que não normaliza aparece com o nome original e
// o aviso de descarte, porque some do catálogo e o admin precisa saber por quê.
func nomeExpostoDe(t *mcp.Tool) (string, []string) {
	f, err := catalogo.Normalizar(0, "", "", t)
	if err != nil {
		return t.Name, []string{"descartada"}
	}
	avisos := make([]string, 0, len(f.Avisos))
	for _, a := range f.Avisos {
		avisos = append(avisos, string(a))
	}
	return f.NomeExposto(), avisos
}
