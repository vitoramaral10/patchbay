package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/authsrv"
	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/trilha"
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
		// NomesOriginais é o catálogo vivo contra o qual as regras casam — o
		// que a tela usa para avisar de uma regra que não casa com nada nele.
		for _, t := range a.gerente.Ferramentas(reg.ID) {
			if t != nil {
				o.NomesOriginais = append(o.NomesOriginais, t.Name)
			}
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

// endpointsParaOAuth responde ao authorization server: quais endpoints existem,
// para o escopo de um cliente, para o scopes_supported da metadata e para o
// resource do RFC 8707.
type endpointsParaOAuth struct {
	repo *endpoint.RepositorioSQLite
}

// Todos implementa authsrv.Endpoints.
func (a endpointsParaOAuth) Todos(ctx context.Context) ([]authsrv.EndpointRef, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]authsrv.EndpointRef, 0, len(regs))
	for _, reg := range regs {
		out = append(out, authsrv.EndpointRef{ID: reg.ID, Slug: reg.Slug, Nome: reg.Nome})
	}
	return out, nil
}

// trilhaDoEndpoint liga o gancho de captura do endpoint à fila da trilha.
//
// É a costura mais fina do arquivo de propósito: as duas features declaram o
// mesmo punhado de campos com nomes próprios, e traduzi-los aqui é o preço de
// nenhuma das duas conhecer a outra. O tipo do resultado é traduzido em vez de
// compartilhado pelo mesmo motivo.
type trilhaDoEndpoint struct {
	registrador *trilha.Registrador
}

// Observar implementa endpoint.Observador. Não bloqueia: Registrador.Observar é
// um envio não bloqueante num canal com buffer, e fila cheia descarta contando.
func (t trilhaDoEndpoint) Observar(c endpoint.Chamada) {
	t.registrador.Observar(trilha.Evento{
		Inicio:       c.Inicio,
		Duracao:      c.Duracao,
		EndpointID:   c.EndpointID,
		EndpointSlug: c.EndpointSlug,
		UpstreamID:   c.UpstreamID,
		UpstreamNome: c.UpstreamNome,
		Ferramenta:   c.Ferramenta,
		Original:     c.Original,
		Resultado:    resultadoDaTrilha(c.Resultado),
		Erro:         c.Erro,
		BytesEntrada: c.BytesEntrada,
		BytesSaida:   c.BytesSaida,
		Sessao:       c.Sessao,
		Credencial:   c.Credencial,
		Era:          c.Era,
	})
}

// resultadoDaTrilha traduz o vocabulário de desfecho de uma feature no da outra.
//
// Os textos são iguais hoje, e a tradução existe justamente para que continuem
// podendo divergir: o dia em que o endpoint precisar de um quarto desfecho, o
// compilador aponta este switch em vez de gravar um valor que o CHECK da
// migração recusa.
func resultadoDaTrilha(r endpoint.ResultadoChamada) trilha.Resultado {
	switch r {
	case endpoint.ChamadaOK:
		return trilha.ResultadoOK
	case endpoint.ChamadaTimeout:
		return trilha.ResultadoTimeout
	default:
		return trilha.ResultadoErro
	}
}

// verificadorDeBearer combina as duas credenciais que abrem /mcp/{slug}: a
// chave de API da fatia 1 e o access token do authorization server da fatia 10.
//
// O despacho é pela marca do texto, e não por tentativa e erro: cada verificador
// consulta o banco, e tentar os dois em sequência dobraria o custo do caminho
// quente para dizer a mesma coisa. A marca vem de quem emitiu, então nunca há
// ambiguidade.
//
// Os dois escrevem o escopo "endpoint:<slug>" em TokenInfo.Scopes, que é o que
// o middleware do go-sdk compara com o escopo exigido pela URL — é dele que sai
// o 403 quando um token de um endpoint é apresentado noutro, e é por isso que
// esta função não precisa olhar o aud.
func verificadorDeBearer(chave *apikey.Servico, as *authsrv.Servico) auth.TokenVerifier {
	return func(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
		switch {
		case authsrv.TemMarca(token, authsrv.MarcaAcesso):
			return as.Verificar(ctx, token, r)
		case authsrv.TemMarca(token, authsrv.MarcaRefresh):
			// Refresh token no lugar do access é erro comum de cliente, e dizer
			// isso poupa uma hora de depuração de quem integra.
			return nil, fmt.Errorf("%w: o refresh_token não vale como bearer; troque-o no /oauth/token",
				auth.ErrInvalidToken)
		default:
			// Chave de API é o padrão: ela é a credencial mais antiga e a que
			// não tem descoberta nenhuma, então erro de formato aqui vira o 401
			// dela, com o desafio que aponta para a metadata do endpoint.
			return chave.Verificar(ctx, token, r)
		}
	}
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
