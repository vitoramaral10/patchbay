package configuracao_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/vitoramaral10/patchbay/internal/configuracao"
	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// bancoFake é o banco inteiro num struct: as quatro portas de configuracao
// implementadas por um dublê só.
//
// Um dublê e não quatro porque o assunto dos testes é a mescla, e ela cruza as
// seções: um upstream criado num item precisa estar visível para o item de
// segredo e para o vínculo de um endpoint no mesmo Aplicar. Quatro fakes
// independentes obrigariam o teste a costurá-los à mão em cada caso.
type bancoFake struct {
	upstreams []configuracao.UpstreamNoBanco
	endpoints []configuracao.EndpointNoBanco
	chaves    []configuracao.ChaveAPI
	clientes  []configuracao.ClienteOAuth

	// slots é o que existe gravado por upstream, e valores é o valor em claro de
	// cada slot. Separados porque é exatamente a distinção do banco real: a UI e
	// o export leem os slots, e só o caminho da conexão lê os valores.
	slots   map[int64][]configuracao.Segredo
	valores map[string]string

	// escritas é a trilha de toda escrita, em ordem. É o que faz o teste de
	// --dry-run afirmar "nada foi escrito" em vez de "o resultado parece igual".
	escritas []string

	erroConferirUpstream error
	erroCriarUpstream    error
	erroAtualizarEnd     error

	proximoUpstream int64
	proximoEndpoint int64
}

func novoBancoFake() *bancoFake {
	return &bancoFake{
		slots:           map[int64][]configuracao.Segredo{},
		valores:         map[string]string{},
		proximoUpstream: 100,
		proximoEndpoint: 200,
	}
}

func (b *bancoFake) registrar(formato string, args ...any) {
	b.escritas = append(b.escritas, fmt.Sprintf(formato, args...))
}

// --- configuracao.Upstreams ---

func (b *bancoFake) Listar(ctx context.Context) ([]configuracao.UpstreamNoBanco, error) {
	_ = ctx
	out := make([]configuracao.UpstreamNoBanco, 0, len(b.upstreams))
	for _, u := range b.upstreams {
		u.Item.Segredos = slices.Clone(b.slots[u.ID])
		out = append(out, u)
	}
	return out, nil
}

func (b *bancoFake) Conferir(ctx context.Context, u configuracao.Upstream) error {
	_, _ = ctx, u
	return b.erroConferirUpstream
}

func (b *bancoFake) Criar(ctx context.Context, u configuracao.Upstream) (int64, error) {
	_ = ctx
	if b.erroCriarUpstream != nil {
		return 0, b.erroCriarUpstream
	}
	b.proximoUpstream++
	id := b.proximoUpstream
	b.upstreams = append(b.upstreams, configuracao.UpstreamNoBanco{ID: id, Item: u})
	b.registrar("criar upstream %s", u.Nome)
	return id, nil
}

func (b *bancoFake) Atualizar(ctx context.Context, id int64, u configuracao.Upstream) error {
	_ = ctx
	for i := range b.upstreams {
		if b.upstreams[i].ID == id {
			b.upstreams[i].Item = u
			b.registrar("atualizar upstream %s", u.Nome)
			return nil
		}
	}
	return fmt.Errorf("upstream %d não existe", id)
}

func (b *bancoFake) Remover(ctx context.Context, id int64) error {
	_ = ctx
	for i, u := range b.upstreams {
		if u.ID == id {
			b.upstreams = slices.Delete(b.upstreams, i, i+1)
			delete(b.slots, id)
			b.registrar("remover upstream %s", u.Item.Nome)
			return nil
		}
	}
	return fmt.Errorf("upstream %d não existe", id)
}

// --- configuracao.SegredosDeUpstream ---

func (b *bancoFake) Definir(
	ctx context.Context, upstreamID int64, tipo, nome string, valor cripto.Segredo,
) error {
	_ = ctx
	slot := configuracao.Segredo{Tipo: tipo, Nome: nome}
	if !slices.ContainsFunc(b.slots[upstreamID], func(s configuracao.Segredo) bool {
		return s.Tipo == tipo && s.Nome == nome
	}) {
		b.slots[upstreamID] = append(b.slots[upstreamID], slot)
	}
	b.valores[chaveDeSlot(upstreamID, tipo, nome)] = valor.Revelar()
	b.registrar("definir segredo %d/%s/%s", upstreamID, tipo, nome)
	return nil
}

func (b *bancoFake) Apagar(ctx context.Context, upstreamID int64, tipo, nome string) error {
	_ = ctx
	b.slots[upstreamID] = slices.DeleteFunc(b.slots[upstreamID], func(s configuracao.Segredo) bool {
		return s.Tipo == tipo && s.Nome == nome
	})
	delete(b.valores, chaveDeSlot(upstreamID, tipo, nome))
	b.registrar("apagar segredo %d/%s/%s", upstreamID, tipo, nome)
	return nil
}

func chaveDeSlot(upstreamID int64, tipo, nome string) string {
	return fmt.Sprintf("%d/%s/%s", upstreamID, tipo, nome)
}

// --- configuracao.Endpoints ---

// endpointsFake existe só para dar à mesma struct um segundo Listar: as duas
// portas têm o método de mesmo nome e assinatura diferente, e um tipo não pode
// implementar as duas.
type endpointsFake struct{ b *bancoFake }

func (e endpointsFake) Listar(ctx context.Context) ([]configuracao.EndpointNoBanco, error) {
	_ = ctx
	return slices.Clone(e.b.endpoints), nil
}

func (e endpointsFake) Conferir(ctx context.Context, ep configuracao.Endpoint) error {
	_, _ = ctx, ep
	return nil
}

func (e endpointsFake) Criar(ctx context.Context, ep configuracao.Endpoint) (int64, error) {
	_ = ctx
	e.b.proximoEndpoint++
	id := e.b.proximoEndpoint
	e.b.endpoints = append(e.b.endpoints, configuracao.EndpointNoBanco{ID: id, Item: ep})
	e.b.registrar("criar endpoint %s", ep.Slug)
	return id, nil
}

func (e endpointsFake) Atualizar(ctx context.Context, id int64, ep configuracao.Endpoint) error {
	_ = ctx
	if e.b.erroAtualizarEnd != nil {
		return e.b.erroAtualizarEnd
	}
	for i := range e.b.endpoints {
		if e.b.endpoints[i].ID == id {
			e.b.endpoints[i].Item = ep
			e.b.registrar("atualizar endpoint %s", ep.Slug)
			return nil
		}
	}
	return fmt.Errorf("endpoint %d não existe", id)
}

func (e endpointsFake) Remover(ctx context.Context, id int64) error {
	_ = ctx
	for i, ep := range e.b.endpoints {
		if ep.ID == id {
			e.b.endpoints = slices.Delete(e.b.endpoints, i, i+1)
			e.b.registrar("remover endpoint %s", ep.Item.Slug)
			return nil
		}
	}
	return fmt.Errorf("endpoint %d não existe", id)
}

// --- portas de leitura ---

type chavesFake struct{ b *bancoFake }

func (c chavesFake) Listar(ctx context.Context) ([]configuracao.ChaveAPI, error) {
	_ = ctx
	return slices.Clone(c.b.chaves), nil
}

type clientesFake struct{ b *bancoFake }

func (c clientesFake) Listar(ctx context.Context) ([]configuracao.ClienteOAuth, error) {
	_ = ctx
	return slices.Clone(c.b.clientes), nil
}

// servicoDe monta o serviço sobre o banco falso, com o ambiente que o caso pedir.
func servicoDe(t *testing.T, b *bancoFake, ambiente map[string]string) *configuracao.Servico {
	t.Helper()
	return configuracao.NovoServico(
		b, b, endpointsFake{b: b},
		slog.New(slog.DiscardHandler),
		configuracao.ComChaves(chavesFake{b: b}),
		configuracao.ComClientes(clientesFake{b: b}),
		configuracao.ComAmbiente(func(nome string) (string, bool) {
			v, ok := ambiente[nome]
			return v, ok
		}),
	)
}

// bancoPovoado é o estado de partida da maioria dos casos: dois upstreams (um de
// cada transporte), dois endpoints com composição fina, uma chave e um cliente.
func bancoPovoado() *bancoFake {
	b := novoBancoFake()
	b.upstreams = []configuracao.UpstreamNoBanco{
		{ID: 1, Item: configuracao.Upstream{
			Nome:       "notion",
			Tipo:       configuracao.TipoHTTP,
			URL:        "https://mcp.notion.com/mcp",
			TimeoutMS:  15000,
			Habilitado: true,
		}},
		{ID: 2, Item: configuracao.Upstream{
			Nome:       "arquivos",
			Tipo:       configuracao.TipoSTDIO,
			Comando:    "npx",
			Args:       []string{"-y", "@modelcontextprotocol/server-filesystem", "/dados"},
			Env:        map[string]string{"NODE_ENV": "production", "LOG": "info"},
			TimeoutMS:  20000,
			Habilitado: true,
		}},
	}
	b.slots[1] = []configuracao.Segredo{{Tipo: configuracao.SegredoBearer}}
	b.valores[chaveDeSlot(1, configuracao.SegredoBearer, "")] = "token-do-notion"
	b.slots[2] = []configuracao.Segredo{{Tipo: configuracao.SegredoEnv, Nome: "TOKEN"}}
	b.valores[chaveDeSlot(2, configuracao.SegredoEnv, "TOKEN")] = "token-dos-arquivos"

	b.endpoints = []configuracao.EndpointNoBanco{
		{ID: 10, Item: configuracao.Endpoint{
			Slug:       "pessoal",
			Nome:       "Pessoal",
			Descricao:  "o endpoint de todo dia",
			Instrucoes: "prefira a busca antes de escrever",
			Upstreams: []configuracao.Vinculo{
				{Nome: "notion", Prefixo: "nt_"},
				{Nome: "arquivos", Prefixo: "fs_", Regras: []configuracao.Regra{
					{Acao: configuracao.RegraExcluir, Padrao: "write_*"},
					{Acao: configuracao.RegraRenomear, Padrao: "read_*", Renome: "ler_*"},
				}},
			},
		}},
		{ID: 11, Item: configuracao.Endpoint{
			Slug:      "leitura",
			Nome:      "Somente leitura",
			Upstreams: []configuracao.Vinculo{{Nome: "notion"}},
		}},
	}
	b.chaves = []configuracao.ChaveAPI{
		{Nome: "desenvolvimento", PrefixoVisivel: "pbk_aaaabbbb", Endpoints: []string{"pessoal"}},
	}
	b.clientes = []configuracao.ClienteOAuth{
		{
			ClientID: "pbc_zzzz", Nome: "Claude Desktop", Tipo: "prereg", Confidencial: true,
			RedirectURIs: []string{"http://127.0.0.1:33418/callback"},
			Endpoints:    []string{"pessoal"},
		},
	}
	return b
}

// documentoDe analisa o YAML exportado para que o caso o edite em Go em vez de
// por substituição de texto: editar YAML com strings.Replace testaria o
// substituidor, não a mescla.
func documentoDe(t *testing.T, dados []byte) configuracao.Documento {
	t.Helper()
	var doc configuracao.Documento
	if err := yaml.Unmarshal(dados, &doc); err != nil {
		t.Fatalf("analisar yaml exportado: erro = %v", err)
	}
	return doc
}

// yamlDe serializa o documento de volta, como faria quem editou o arquivo à mão:
// os campos mudam e as linhas de revisao ficam como estavam.
func yamlDe(t *testing.T, doc configuracao.Documento) []byte {
	t.Helper()
	dados, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("serializar documento: erro = %v", err)
	}
	return dados
}

// itemDoPlano acha o item de um tipo e nome. Falha o teste se não existir: o
// plano é o contrato deste pacote, e um item que sumiu é o achado, não um nil a
// tratar.
func itemDoPlano(t *testing.T, p configuracao.Plano, tipo, nome string) configuracao.Item {
	t.Helper()
	for _, i := range p.Itens {
		if i.Tipo == tipo && i.Nome == nome {
			return i
		}
	}
	t.Fatalf("plano sem item %s %q; itens = %s", tipo, nome, itensEmTexto(p))
	return configuracao.Item{}
}

func itensEmTexto(p configuracao.Plano) string {
	partes := make([]string, 0, len(p.Itens))
	for _, i := range p.Itens {
		partes = append(partes, fmt.Sprintf("%s/%s=%s", i.Tipo, i.Nome, i.Operacao))
	}
	return strings.Join(partes, ", ")
}

// upstreamDoBanco devolve o item gravado, para o teste conferir o que a aplicação
// escreveu de verdade.
func upstreamDoBanco(t *testing.T, b *bancoFake, nome string) configuracao.Upstream {
	t.Helper()
	for _, u := range b.upstreams {
		if u.Item.Nome == nome {
			return u.Item
		}
	}
	t.Fatalf("banco sem upstream %q", nome)
	return configuracao.Upstream{}
}

// endpointDoBanco devolve o endpoint gravado pelo slug.
func endpointDoBanco(t *testing.T, b *bancoFake, slug string) configuracao.Endpoint {
	t.Helper()
	for _, e := range b.endpoints {
		if e.Item.Slug == slug {
			return e.Item
		}
	}
	t.Fatalf("banco sem endpoint %q", slug)
	return configuracao.Endpoint{}
}

var errFake = errors.New("falha de mentira")
