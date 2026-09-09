// Package biblioteca é a tela que lê o catálogo de servidores MCP remotos do
// mcpservers.org e transforma um deles num upstream preenchido.
//
// Existe porque cadastrar um upstream à mão é acertar URL, transporte e modo de
// credencial na primeira tentativa — três campos em que errar não dá erro de
// formulário, dá um upstream degradado horas depois. A biblioteca troca isso por
// escolher um nome de uma lista.
//
// # Nada é guardado
//
// Não há catálogo embutido no binário nem tabela no banco: toda vez que a tela
// abre, ela busca a lista na origem. É decisão do dono (2026-09-09), e o que se
// ganha com ela é que a biblioteca nunca mostra um servidor que saiu do ar nem
// esconde um que entrou — não existe versão velha para ficar velha.
//
// O preço, explícito: **sem rede para o mcpservers.org, a tela não funciona**.
// Ela diz isso com todas as letras e aponta para o cadastro à mão, em vez de
// mostrar uma lista vazia que parece defeito. O resto do patchbay não depende
// disto: o gateway sobe, serve e roteia igual com a origem fora do ar.
//
// # Por que a busca é filtrada aqui e não delegada
//
// A origem tem busca própria (/search?query=), e ela é renderizada no servidor.
// Só que ela casa **apenas pelo nome** do servidor remoto: medido em 2026-09-09,
// "jira" devolve zero remotos — não acha o Atlassian, cujo resumo é literalmente
// "Jira, Confluence, Compass" —, e "database" e "kubernetes" também devolvem
// zero. Delegar a busca deixaria a tela pior do que ela precisa ser.
//
// Então a tela pede a lista de remotos da origem (uma requisição) e filtra o que
// veio, sobre nome e resumo. Os dados continuam sendo, byte a byte, o que a
// origem respondeu naquele momento — o que muda é só onde a comparação de texto
// roda.
//
// # A tradução do HTML
//
// A origem não tem API: o robots.txt bloqueia /api/, e o que sobra é o HTML das
// páginas públicas. Ler HTML de terceiro é frágil por natureza, e o pacote assume
// isso em vez de fingir o contrário — ver origem.go, onde cada expressão está
// amarrada a um pedaço da página e a falha de extração vira erro visível, nunca
// um item pela metade.
package biblioteca

import "errors"

// Transportes possíveis. São os mesmos rótulos que o formulário de upstream usa
// em ?tipo=, de propósito: o valor viaja daqui para lá sem tradução, e uma
// tabela de conversão no meio é onde os dois lados divergem.
const (
	TransporteHTTP = "http"
	TransporteSSE  = "sse"
)

// Formas de autenticação que a origem declara.
//
// Não é o modo de credencial do upstream — é o que o servidor exige. A tradução
// para o modo está em ModoDeCredencial, e é de mão única: oauth vira oauth, todo
// o resto vira estática. Estática com bearer em branco é um formulário que o
// admin completa; oauth errado é um fluxo de consentimento que não fecha.
const (
	// AutOAuth é consentimento por navegador.
	AutOAuth = "oauth"
	// AutToken é chave ou token colado pelo admin.
	AutToken = "token"
	// AutAberta é servidor sem autenticação nenhuma.
	AutAberta = "aberta"
)

// Erros sentinela do pacote.
var (
	// ErrOrigemIndisponivel é rede fora, tempo esgotado, ou a origem
	// respondendo com desafio de bot. É o erro que a tela traduz em "não
	// consegui falar com o mcpservers.org".
	ErrOrigemIndisponivel = errors.New("biblioteca: origem indisponível")
	// ErrFormatoDaOrigem é a página tendo chegado, e o que estava nela não ser
	// o que este pacote sabe ler — quase sempre o site mudou de marcação. É
	// separado de ErrOrigemIndisponivel porque a ação é outra: um pede para
	// tentar de novo, o outro pede um commit aqui.
	ErrFormatoDaOrigem = errors.New("biblioteca: formato da origem mudou")
	// ErrNaoEncontrado é o slug não existir mais na origem.
	ErrNaoEncontrado = errors.New("biblioteca: servidor não está mais no catálogo")
)

// Item é a linha da lista: o que a página índice da origem sabe dizer.
//
// Não tem URL nem transporte de propósito — a página índice não os traz, e
// inventá-los aqui seria um botão "adicionar" que leva a um formulário errado.
// Eles vêm do Detalhe, buscado quando o admin escolhe um servidor.
type Item struct {
	// Slug identifica o servidor na origem.
	Slug string
	// Nome é como o servidor se chama.
	Nome string
	// Resumo é a linha curta, já no idioma da origem que pedimos (pt-BR).
	Resumo string
}

// Detalhe é a página de um servidor: o que o cadastro precisa.
type Detalhe struct {
	Item
	// Descricao é o parágrafo "Sobre".
	Descricao string
	// URL é o endpoint MCP.
	URL string
	// Transporte é http ou sse.
	Transporte string
	// Autenticacao é oauth, token ou aberta.
	Autenticacao string
	// Docs é a documentação oficial do servidor, quando a origem a declara.
	Docs string
}

// ModoDeCredencial traduz a autenticação declarada no modo que o formulário de
// upstream entende.
func (d Detalhe) ModoDeCredencial() string {
	if d.Autenticacao == AutOAuth {
		return "oauth"
	}
	return "estatica"
}
