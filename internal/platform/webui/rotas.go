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
	// RotaConfiguracao é o export e o import do YAML versionável.
	RotaConfiguracao = "/admin/configuracao"
	// RotaTrilha é a trilha filtrável de chamadas de ferramenta.
	RotaTrilha = "/admin/trilha"
	// RotaLogsAoVivo é a tela de log ao vivo.
	RotaLogsAoVivo = "/admin/logs/ao-vivo"
	// RotaLogsFluxo é o stream de SSE que alimenta a tela de log ao vivo. É
	// rota separada da tela porque uma responde text/html e a outra
	// text/event-stream, e um handler que decide isso pelo Accept é um handler
	// que quebra quando o navegador muda de opinião.
	RotaLogsFluxo = "/admin/logs/ao-vivo/fluxo"
	// RotaCallbackOAuthUpstream é onde o authorization server de um upstream
	// devolve o navegador do admin depois do consentimento.
	//
	// Mora sob /admin/upstreams e não sob /admin/oauth porque /admin/oauth já é
	// a lista de clientes do authorization server *do patchbay*: o mesmo prefixo
	// significaria dois papéis opostos — o patchbay como cliente de um provedor
	// e o patchbay como servidor de seus clientes — e o caminho literal ainda
	// sombrearia o /admin/oauth/{id} daquela tela.
	//
	// É contrato com o provedor: o redirect_uri registrado lá é este caminho
	// sobre a URL pública, e mudá-lo depois quebra todo consentimento existente.
	RotaCallbackOAuthUpstream = RotaUpstreams + "/oauth/callback"
	// RotaMetadataClienteUpstream é o Client ID Metadata Document do patchbay
	// como cliente OAuth (SEP-991).
	//
	// Público, sem sessão: quem o lê é o authorization server do upstream, do
	// lado de fora. O caminho não é a raiz porque o SDK exige URL HTTPS
	// não-raiz para usar CIMD como client_id.
	RotaMetadataClienteUpstream = "/oauth/patchbay-cliente.json"
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
	// SecaoConfiguracao é o export/import de YAML.
	SecaoConfiguracao = "configuracao"
	SecaoTrilha       = "trilha"
	SecaoLogs         = "logs"
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
	{Rota: RotaConfiguracao, Rotulo: "Configuração", Secao: SecaoConfiguracao},
	{Rota: RotaTrilha, Rotulo: "Trilha", Secao: SecaoTrilha},
	{Rota: RotaLogsAoVivo, Rotulo: "Logs", Secao: SecaoLogs},
}
