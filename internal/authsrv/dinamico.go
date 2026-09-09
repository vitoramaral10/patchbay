package authsrv

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// clientePorIdentificador resolve o client_id de um pedido de autorização.
//
// São dois espaços de identificador convivendo no mesmo parâmetro: o que este
// AS emitiu (prereg pela UI ou DCR), que é uma linha de oauth_client; e a URL
// https de um Client ID Metadata Document, que não é registro nenhum — é um
// documento publicado pelo cliente, que este AS busca e cacheia.
func (s *Servico) clientePorIdentificador(ctx context.Context, clientID string) (Cliente, error) {
	if s.cimd == nil || !ehIdentificadorCIMD(clientID) {
		return s.repo.ClientePorClientID(ctx, clientID)
	}
	return s.resolverCIMD(ctx, clientID)
}

// resolverCIMD devolve o cliente descrito pelo documento publicado em
// identificador, buscando-o quando o cache não serve.
//
// O cache é a linha de oauth_client com tipo cimd_cache e expira_em preenchido.
// Regras dele importam:
//
//   - Erro de busca não impede servir o último documento bom, mas só até
//     3×ValidadeCIMD depois de expira_em — o prazo do last-known-good. O
//     documento é a allowlist de redirect de um cliente que já funcionava:
//     derrubar a conexão dele porque a CDN do dono piscou seria trocar uma
//     indisponibilidade de terceiro por uma minha. Mas servir para sempre um
//     documento que pode ter mudado — ou que o dono do domínio já não controla
//     mais — trocaria "resiliente a uma falha" por "nunca reflete revogação".
//   - Cliente revogado pelo admin não é rebuscado. Sem isso, revogar um cliente
//     de CIMD na tela duraria até o TTL vencer e o documento voltar sozinho.
//   - Documento nunca visto (sem cache algum) entra no teto de
//     TetoCacheCIMDPorHora antes de qualquer requisição de saída: é o que
//     impede que a escolha do client_id — uma URL, em CIMD — vire uma forma de
//     fazer o processo buscar documentos novos sem limite.
func (s *Servico) resolverCIMD(ctx context.Context, identificador string) (Cliente, error) {
	agora := s.agora()

	cache, err := s.repo.ClienteMesmoRevogado(ctx, identificador)
	temCache := err == nil
	switch {
	case temCache && cache.Revogado():
		return Cliente{}, ErrClienteNaoEncontrado
	case temCache && agora.Before(cache.ExpiraEm):
		return cache, nil
	case err != nil && !errors.Is(err, ErrClienteNaoEncontrado):
		return Cliente{}, erroInterno(err)
	}

	if !temCache {
		total, err := s.repo.ContarCacheCIMD(ctx, agora.Add(-time.Hour))
		if err != nil {
			return Cliente{}, erroInterno(err)
		}
		if total >= TetoCacheCIMDPorHora {
			s.log.Warn("cache de cimd recusado por teto", "total", total)
			return Cliente{}, &ErroOAuth{
				Codigo:      ErroInvalidClient,
				Descricao:   "documentos de client id novos demais nesta janela; tente de novo mais tarde",
				Status:      http.StatusTooManyRequests,
				SemRedirect: true,
			}
		}
	}

	doc, err := s.cimd.Buscar(ctx, identificador)
	if err != nil {
		// last-known-good só até 3×ValidadeCIMD depois do vencimento: além
		// disso, um documento que pode ter mudado há muito não é mais "o mesmo
		// cliente que já funcionava" — é servir o passado como se fosse agora.
		if temCache && agora.Before(cache.ExpiraEm.Add(3*s.validadeCIMD)) {
			s.log.Warn("documento de client id não pôde ser rebuscado; servindo o último bom",
				"client_id", identificador, "erro", err)
			return cache, nil
		}
		return Cliente{}, &ErroOAuth{
			Codigo:      ErroInvalidClient,
			Descricao:   "o documento de client_id não pôde ser buscado ou não é válido",
			Status:      http.StatusUnauthorized,
			Causa:       err,
			SemRedirect: true,
		}
	}

	cliente, err := s.repo.SalvarCacheCIMD(ctx, ClienteDinamico{
		ClientID:     identificador,
		Nome:         doc.Nome(),
		Tipo:         TipoCIMD,
		RedirectURIs: doc.RedirectURIs,
		Origem:       hospedeiroDe(identificador),
		CriadoEm:     agora,
		ExpiraEm:     agora.Add(s.validadeCIMD),
	})
	if err != nil {
		return Cliente{}, erroInterno(err)
	}
	return cliente, nil
}

// PermiteRedirect informa se uri está na allowlist do cliente.
//
// Duas regras de comparação convivem, e é o tipo do cliente que escolhe qual
// vale:
//
//   - Exata, caractere a caractere, para todo cliente. É o padrão, e é ela que
//     impede que o AS vire redirecionador aberto — comparação por prefixo é a
//     falha clássica que entrega o código de autorização a quem registrou um
//     caminho parecido.
//   - Ignorando a porta, para http em loopback — mas só em TipoDCR e TipoCIMD.
//     É o RFC 8252 §7.3: o cliente nativo escuta numa porta efêmera que ele só
//     descobre ao abrir o listener, então a porta não pode fazer parte do que
//     foi cadastrado. É o que o Claude Code exige, e é por isso que ele
//     declara http://localhost/callback e http://127.0.0.1/callback no próprio
//     documento de CIMD. Um cliente TipoPrereg volta à comparação exata: foi o
//     admin quem escolheu aquela porta na tela, e nada nele diz que é um
//     cliente nativo com listener efêmero — dar-lhe a mesma folga seria abrir
//     mão da porta sem o motivo que a justifica.
func (c Cliente) PermiteRedirect(uri string) bool {
	loopbackLivreDePorta := c.Tipo == TipoDCR || c.Tipo == TipoCIMD
	for _, permitida := range c.RedirectURIs {
		if permitida == uri {
			return true
		}
		if loopbackLivreDePorta && casaLoopbackSemPorta(permitida, uri) {
			return true
		}
	}
	return false
}

// casaLoopbackSemPorta compara duas redirect_uri de loopback ignorando a porta.
//
// Tudo o mais é comparado exatamente: esquema, hostname, caminho e query. E o
// hostname é comparado por igualdade, não por "os dois são loopback" —
// 127.0.0.1 e localhost são nomes diferentes, e conflatá-los deixaria um cliente
// que cadastrou só um receber o código no outro. Não custa nada exigir os dois:
// quem precisa dos dois declara os dois, como o Claude Code faz.
func casaLoopbackSemPorta(permitida, pedida string) bool {
	a, erroA := url.Parse(permitida)
	b, erroB := url.Parse(pedida)
	if erroA != nil || erroB != nil {
		return false
	}
	switch {
	case a.Scheme != "http" || b.Scheme != "http":
		return false
	case a.User != nil || b.User != nil:
		return false
	case !ehLoopback(a.Hostname()) || !ehLoopback(b.Hostname()):
		return false
	case !strings.EqualFold(a.Hostname(), b.Hostname()):
		return false
	}
	return a.Path == b.Path && a.RawQuery == b.RawQuery && a.Fragment == "" && b.Fragment == ""
}
