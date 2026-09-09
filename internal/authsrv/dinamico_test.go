package authsrv

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// repoDinamicoFake é um Repositorio mínimo para os testes de resolução de
// CIMD abaixo: só ClienteMesmoRevogado, SalvarCacheCIMD e ContarCacheCIMD, que
// é o que resolverCIMD toca. Os demais métodos existem só para satisfazer a
// interface e nunca deveriam ser chamados por estes testes — daí o panic.
type repoDinamicoFake struct {
	clientes        map[string]Cliente
	contarCacheCIMD func(ctx context.Context, desde time.Time) (int, error)
}

func (r *repoDinamicoFake) ClienteMesmoRevogado(_ context.Context, clientID string) (Cliente, error) {
	c, ok := r.clientes[clientID]
	if !ok {
		return Cliente{}, ErrClienteNaoEncontrado
	}
	return c, nil
}

func (r *repoDinamicoFake) SalvarCacheCIMD(_ context.Context, d ClienteDinamico) (Cliente, error) {
	if r.clientes == nil {
		r.clientes = map[string]Cliente{}
	}
	c := Cliente{
		ID: 1, ClientID: d.ClientID, Nome: d.Nome, Tipo: d.Tipo,
		RedirectURIs: d.RedirectURIs, Origem: d.Origem,
		CriadoEm: d.CriadoEm, ExpiraEm: d.ExpiraEm, EscopoAberto: true,
	}
	r.clientes[d.ClientID] = c
	return c, nil
}

func (r *repoDinamicoFake) ContarCacheCIMD(ctx context.Context, desde time.Time) (int, error) {
	if r.contarCacheCIMD != nil {
		return r.contarCacheCIMD(ctx, desde)
	}
	return 0, nil
}

func (r *repoDinamicoFake) ClientePorClientID(context.Context, string) (Cliente, error) {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) RegistrarClienteDinamico(
	context.Context, ClienteDinamico, string, time.Time, int, int,
) (Cliente, error) {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) GravarCodigo(context.Context, Codigo) error {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) ConsumirCodigo(context.Context, string, time.Time) (Codigo, bool, error) {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) GravarPar(context.Context, Token, Token) error {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) TokenPorHash(context.Context, string) (Token, error) {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) Rotacionar(context.Context, int64, Token, Token, time.Time) (bool, error) {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) RevogarFamilia(context.Context, string, time.Time) error {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) RevogarToken(context.Context, int64, time.Time) error {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) RegistrarUsoToken(context.Context, int64, time.Time) error {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) LimparExpirados(context.Context, time.Time) error {
	panic("não usado neste teste")
}

func (r *repoDinamicoFake) LimparCacheCIMD(context.Context, time.Time) error {
	panic("não usado neste teste")
}

var _ Repositorio = (*repoDinamicoFake)(nil)

// cimdFake é o DocumentosCIMD do teste: sem rede nenhuma, só o que
// resolverCIMD precisa decidir entre servir o cache e recusar.
type cimdFake struct {
	doc      DocumentoCIMD
	err      error
	chamadas int
}

func (c *cimdFake) Buscar(context.Context, string) (DocumentoCIMD, error) {
	c.chamadas++
	if c.err != nil {
		return DocumentoCIMD{}, c.err
	}
	return c.doc, nil
}

func servicoDeTesteCIMD(repo Repositorio, busca DocumentosCIMD, relogio func() time.Time) *Servico {
	return NovoServico(repo, nil, "https://patchbay.exemplo",
		func(slug string) string { return "endpoint:" + slug },
		slog.New(slog.DiscardHandler),
		ComCIMD(busca), ComRelogio(relogio), ComValidadeCIMD(time.Hour))
}

// TestResolverCIMD_LastKnownGoodComPrazo prova o prazo do cache vencido: uma
// busca que falha continua servindo o último documento bom logo depois do TTL
// vencer, mas para de servir 3×ValidadeCIMD depois de expira_em — servir o
// passado para sempre deixaria de ser resiliência para virar "nunca reflete
// revogação".
func TestResolverCIMD_LastKnownGoodComPrazo(t *testing.T) {
	t.Parallel()

	const identificador = "https://cliente.test/doc"
	agora := time.Now()
	relogio := func() time.Time { return agora }

	repo := &repoDinamicoFake{}
	busca := &cimdFake{doc: DocumentoCIMD{
		ClientID: identificador,
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{"http://127.0.0.1/callback"},
		},
	}}
	s := servicoDeTesteCIMD(repo, busca, relogio)

	// Primeira busca: documento bom, cria o cache com expira_em = agora + 1h.
	if _, err := s.clientePorIdentificador(context.Background(), identificador); err != nil {
		t.Fatalf("primeira busca: erro = %v, quer nil", err)
	}

	// O documento fica indisponível a partir daqui.
	busca.err = errors.New("rede fora do ar")

	// Bem depois do TTL vencer, mas dentro de 3×ValidadeCIMD: serve o cache.
	agora = agora.Add(time.Hour + time.Minute)
	if _, err := s.clientePorIdentificador(context.Background(), identificador); err != nil {
		t.Fatalf("dentro do prazo de last-known-good: erro = %v, quer nil", err)
	}

	// Passou de 3×ValidadeCIMD além do vencimento: já não serve.
	agora = agora.Add(3 * time.Hour)
	_, err := s.clientePorIdentificador(context.Background(), identificador)
	if err == nil {
		t.Fatal("depois do prazo de last-known-good: erro = nil, quer recusa")
	}
	var oerr *ErroOAuth
	if !errors.As(err, &oerr) || oerr.Codigo != ErroInvalidClient {
		t.Errorf("erro = %v, quer *ErroOAuth invalid_client", err)
	}
}

// TestResolverCIMD_TetoDeCacheNovo prova que um identificador nunca visto é
// recusado sem nenhuma busca quando o teto de documentos novos por hora já foi
// atingido.
func TestResolverCIMD_TetoDeCacheNovo(t *testing.T) {
	t.Parallel()

	repo := &repoDinamicoFake{
		contarCacheCIMD: func(context.Context, time.Time) (int, error) {
			return TetoCacheCIMDPorHora, nil
		},
	}
	busca := &cimdFake{}
	s := servicoDeTesteCIMD(repo, busca, time.Now)

	_, err := s.clientePorIdentificador(context.Background(), "https://novo.test/doc")
	if err == nil {
		t.Fatal("teto de cache atingido: erro = nil, quer recusa")
	}
	var oerr *ErroOAuth
	if !errors.As(err, &oerr) || oerr.Codigo != ErroInvalidClient {
		t.Errorf("erro = %v, quer *ErroOAuth invalid_client", err)
	}
	if busca.chamadas != 0 {
		t.Errorf("chamadas ao buscador = %d, quer 0: o teto devia recusar antes de qualquer busca",
			busca.chamadas)
	}
}
