// Package biblioteca é a tela que lê o catálogo oficial de servidores MCP e
// transforma um deles num upstream preenchido.
//
// Existe porque cadastrar um upstream à mão é acertar URL, transporte e modo de
// credencial na primeira tentativa — três campos em que errar não dá erro de
// formulário, dá um upstream degradado horas depois. A biblioteca troca isso por
// escolher um nome de uma lista.
//
// # Duas origens, e por quê
//
// A biblioteca lê **duas** fontes e as mescla. Cada uma ganha no que a outra
// perde, e isso foi medido, não suposto (2026-09-09):
//
// O **registry oficial** (registry.modelcontextprotocol.io) dá alcance: 29.843
// servidores, dos quais 11.769 só existem como pacote instalável — é dele que
// vem todo servidor de processo local. Mas o esquema dele **não tem campo de
// autenticação**: com.notion/mcp, que exige OAuth, chega sem nenhum sinal disso.
// E dois terços do que há lá são namespaces io.github.*, ou seja, conta de
// GitHub, não domínio de fornecedor.
//
// O **mcpservers.org** dá curadoria, em duas listas. A de remotos tem 293
// servidores escolhidos a dedo.
// Numa amostra de 25 deles, 25 declaram a forma de autenticação (22 são OAuth),
// o resumo vem em português, e — o que decidiu a questão — **só 3 dos 25 existem
// no registry**. Neon, MDN, Pendo, Blackbaud, Candid e Unthread são remotos
// conhecidos que simplesmente não estão lá.
//
// A segunda lista dele, /official, tem 647 servidores de processo local. Ela
// entra pelo que é — nomes que alguém chamou de oficiais —, porque não declara
// transporte, autenticação nem URL (0 de 10 amostrados): o comando existe só
// como trecho de README, e só 4 de 14 saíram aproveitáveis. O acervo maior do
// mesmo site (/all, 12.173) fica de fora: ele nem contém os remotos —
// /servers/notion responde 404 — e custaria ~7 horas por varredura para repetir,
// sem estrutura, a cauda longa que o registry já entrega em packages[].
//
// A chave de junção é a **URL do endpoint**: as duas publicam o mesmo endereço
// para o mesmo servidor, e casar por ele é exato — casar por nome não seria,
// porque "Notion" de um lado é "com.notion/mcp" do outro. Quem casa fica com a
// identidade técnica do registry e com o texto e a autenticação da curadoria;
// ver mesclar, em sincronizador.go.
//
// Os oficiais, que não têm URL, casam pela linha de comando com a versão do
// pacote ignorada — o registry pina e o mcpservers.org não.
//
// A primeira versão desta tela lia só o mcpservers.org, a segunda só o registry.
// Nenhuma das duas bastava.
//
// # A semente
//
// A primeira varredura leva perto de uma hora, e a tela passava esse tempo sem
// servir para nada. Por isso o binário carrega um catálogo versionado — ver
// semente.go —, que entra no banco no primeiro boot com a data em que foi
// gerado. Essa data é o que faz o sincronizador considerá-lo vencido e varrer em
// seguida: a semente é ponto de partida, nunca o catálogo em vigor.
//
// # O catálogo é copiado, não lido ao vivo
//
// A primeira versão desta tela ia à origem a cada abertura e a cada tecla
// digitada, sem guardar nada. Isso caiu em 2026-09-09, contra a medição: são
// 29.610 servidores em páginas de 100, a varredura inteira leva ~16 minutos, e
// requisições avulsas ao registry chegaram a passar de 40 segundos sem
// responder. A tela herdava a latência e a instabilidade de um terceiro.
//
// Agora existe cópia local em SQLite, refeita de tempos em tempos pelo
// Sincronizador — a única parte deste pacote que fala com a rede. A tela lê o
// banco: medido com 30 mil linhas, uma busca leva de 30 a 96 ms, contra os
// segundos (e os 40 que estouravam) da leitura ao vivo. E continua funcionando
// com a internet fora.
//
// O que se paga por isso, e está escrito na tela: **o catálogo tem idade**. Um
// servidor publicado hoje não aparece até a próxima varredura. A idade fica
// visível acima da lista, junto com o botão que refaz a varredura na hora —
// esconder a idade é o que faria o admin procurar um servidor que existe e
// concluir que o patchbay está quebrado.
//
// Varredura que volta vazia não apaga o que existe, e varredura que falha no
// meio não toca no catálogo: a cópia anterior continua servindo, com a idade
// dizendo o que ela é.
//
// # O modo de credencial só vai quando alguém declarou
//
// O link que abre o formulário de upstream leva modo=oauth **só** quando a
// curadoria declarou a autenticação daquele servidor. Para quem vem só do
// registry, o campo não existe em lugar nenhum, e o formulário fica no padrão
// dele: adivinhar OAuth a partir do nada produziria um fluxo de consentimento
// que não fecha, e adivinhar estática produziria um upstream que nasce com 401.
//
// Quando o registry declara headers obrigatórios num remote, a tela diz que o
// servidor pede credencial — é o único sinal que ele dá, e ele é dito como
// aviso, não convertido em configuração.
package biblioteca

import (
	"errors"
	"fmt"
	"strings"
)

// Transportes possíveis. São os mesmos rótulos que o formulário de upstream usa
// em ?tipo=, de propósito: o valor viaja daqui para lá sem tradução, e uma
// tabela de conversão no meio é onde os dois lados divergem.
const (
	TransporteHTTP  = "http"
	TransporteSSE   = "sse"
	TransporteSTDIO = "stdio"
)

// Formas de autenticação que a curadoria declara.
//
// Não é o modo de credencial do upstream — é o que o servidor exige. Só o
// mcpservers.org publica isto: o esquema do registry oficial não tem campo de
// autenticação, e por isso um servidor que vem só de lá chega com Autenticacao
// vazia e o formulário fica no padrão dele.
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
	// ErrOrigemIndisponivel é rede fora, tempo esgotado ou a origem respondendo
	// o que não devia. É o erro que a tela traduz em "não consegui falar com o
	// registry".
	ErrOrigemIndisponivel = errors.New("biblioteca: origem indisponível")
	// ErrFormatoDaOrigem é a resposta tendo chegado e não ser o que este pacote
	// sabe ler — o esquema do registry mudou. É separado de
	// ErrOrigemIndisponivel porque a ação é outra: um pede para tentar de novo,
	// o outro pede um commit aqui.
	ErrFormatoDaOrigem = errors.New("biblioteca: formato da origem mudou")
	// ErrTaxaExcedida é a origem ter respondido 429.
	//
	// Separado de ErrOrigemIndisponivel porque a ação é outra: aqui esperar
	// resolve, e a varredura insiste na mesma página em vez de descartá-la. É
	// ErrOrigemIndisponivel para quem só quer saber se deu ou não deu.
	ErrTaxaExcedida = fmt.Errorf("%w: taxa excedida", ErrOrigemIndisponivel)
	// ErrNaoEncontrado é o servidor não existir mais na origem.
	ErrNaoEncontrado = errors.New("biblioteca: servidor não está mais no catálogo")
)

// Item é um servidor do catálogo, já traduzido no que o cadastro precisa.
//
// Diferente da versão que lia HTML, aqui não há segunda requisição para
// descobrir endpoint e transporte: a listagem do registry já traz remotes[] e
// packages[], então o que a tela mostra e o que o formulário recebe saem da
// mesma resposta. Um item sem forma de conexão utilizável nunca é montado — ver
// itemDe, em origem.go.
type Item struct {
	// Nome é o identificador no registry, em DNS invertido: "com.notion/mcp".
	// É ele que volta pela URL quando o admin clica em adicionar.
	Nome string
	// Titulo é o nome de exibição. Cai para Nome quando a origem não declara um.
	Titulo string
	// Descricao é a linha do catálogo.
	Descricao string
	// Versao é a versão publicada, mostrada para o admin saber o que está vendo.
	Versao string

	// Transporte é http, sse ou stdio.
	Transporte string
	// URL é o endpoint, nos transportes remotos. Vazio no stdio.
	URL string
	// Comando e Args são a execução, no stdio. Vazios nos remotos.
	Comando string
	Args    []string

	// Autenticacao é o que o servidor exige: AutOAuth, AutToken ou AutAberta.
	// Vazia quando ninguém declarou — o registry não tem esse campo, e só a
	// curadoria do mcpservers.org o publica.
	Autenticacao string
	// PedeCredencial é alguém ter declarado que o servidor exige credencial:
	// header obrigatório no remote do registry, ou autenticação não-aberta na
	// curadoria.
	PedeCredencial bool
	// Curado é o servidor estar na lista de remotos do mcpservers.org, que é
	// escolhida a dedo. É o sinal de confiança mais forte que a biblioteca tem:
	// o registry aceita quem provar o namespace, e dois terços do que há lá são
	// contas de GitHub.
	Curado bool
	// Site é o repositório ou a página do servidor, quando a origem declara um.
	// É o "ver na origem" da tela, e pode ser vazio.
	Site string
}

// ModoDeCredencial traduz a autenticação declarada no modo que o formulário de
// upstream entende.
//
// De mão única e conservadora: só oauth vira oauth. Estática com bearer em
// branco é um formulário que o admin completa; oauth errado é um fluxo de
// consentimento que não fecha. Vazio significa "ninguém declarou" e devolve
// vazio, para o link não carregar palpite nenhum.
func (i Item) ModoDeCredencial() string {
	if i.Autenticacao == AutOAuth {
		return "oauth"
	}
	return ""
}

// Remoto diz se o item vira upstream de rede, em vez de processo local.
func (i Item) Remoto() bool { return i.Transporte != TransporteSTDIO }

// Namespace é a parte do nome antes da barra: "com.notion", "io.github.fulano".
func (i Item) Namespace() string {
	if barra := strings.IndexByte(i.Nome, '/'); barra > 0 {
		return i.Nome[:barra]
	}
	return i.Nome
}

// DominioVerificado diz se o servidor foi publicado sob o domínio de quem o faz.
//
// É o mais perto de "oficial" que o registry permite afirmar, e é afirmação de
// fato, não julgamento: a documentação do registry exige que "para publicar em
// com.example/server, o publicador prove que é dono do domínio example.com".
// Então com.notion/mcp existe porque alguém provou controlar notion.com.
//
// Os namespaces de foundry — io.github.*, io.gitlab.* — provam a conta no
// serviço, não o domínio do fornecedor. Isso deixa de fora o servidor que uma
// empresa publica pela própria organização no GitHub, e o filtro da tela diz
// isso com todas as letras: esconder sem explicar seria pior do que não filtrar.
func (i Item) DominioVerificado() bool {
	ns := i.Namespace()
	for _, foundry := range namespacesDeFoundry {
		if strings.HasPrefix(ns, foundry) {
			return false
		}
	}
	return strings.Contains(ns, ".")
}

// namespacesDeFoundry são os prefixos em que o registry verifica a conta num
// serviço de hospedagem, e não o domínio de quem publica.
//
// Vive aqui e na consulta do repositório (ver filtroDe): são os dois lugares
// onde a mesma regra precisa valer, um para desenhar o selo e outro para
// filtrar no banco. Mudou aqui, muda lá — o teste
// TestFiltroDeOficiaisCasaComOSelo é quem cobra isso.
var namespacesDeFoundry = []string{"io.github.", "io.gitlab.", "io.modelcontextprotocol.anonymous"}

// Resultado é uma página do catálogo.
//
// O cursor é opaco de propósito: ele é o que a origem devolveu em
// metadata.nextCursor, e o patchbay não o interpreta — só o devolve na próxima
// requisição. Vazio significa que esta é a última página.
type Resultado struct {
	Itens         []Item
	ProximoCursor string
}
