package webui

// O espaço de URLs da UI de administração.
//
// Fica no layout, e não em cada feature, porque é o shell que precisa de todos
// eles para desenhar a navegação — e porque um caminho escrito duas vezes é um
// caminho que uma das duas vezes está errado. É URL de tela, não modelo de
// domínio: platform continua sem saber o que é um upstream.
const (
	// RotaPainel é a raiz da UI e o destino do login bem-sucedido.
	RotaPainel = "/admin/"
	// RotaUpstreams é a lista de upstreams.
	RotaUpstreams = "/admin/upstreams"
	// RotaEndpoints é a lista de endpoints.
	RotaEndpoints = "/admin/endpoints"
	// RotaChaves é a lista de chaves de API.
	RotaChaves = "/admin/chaves"
	// RotaClientesOAuth é a lista de clientes OAuth do authorization server.
	RotaClientesOAuth = "/admin/oauth"
	// RotaLogin é o formulário de entrada.
	RotaLogin = "/admin/login"
	// RotaSetup é o formulário de criação do admin único; deixa de existir
	// depois que o admin existe.
	RotaSetup = "/admin/setup"
	// RotaSair encerra a sessão.
	RotaSair = "/admin/sair"
)

// Seções da navegação, usadas para marcar o item ativo.
const (
	SecaoPainel    = "painel"
	SecaoUpstreams = "upstreams"
	SecaoEndpoints = "endpoints"
	SecaoChaves    = "chaves"
	SecaoOAuth     = "oauth"
)

type itemNav struct {
	Rota   string
	Rotulo string
	Secao  string
}

var navegacao = []itemNav{
	{Rota: RotaPainel, Rotulo: "Painel", Secao: SecaoPainel},
	{Rota: RotaUpstreams, Rotulo: "Upstreams", Secao: SecaoUpstreams},
	{Rota: RotaEndpoints, Rotulo: "Endpoints", Secao: SecaoEndpoints},
	{Rota: RotaChaves, Rotulo: "Chaves de API", Secao: SecaoChaves},
	{Rota: RotaClientesOAuth, Rotulo: "Clientes OAuth", Secao: SecaoOAuth},
}
