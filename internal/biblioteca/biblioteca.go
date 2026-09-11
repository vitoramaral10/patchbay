// Package biblioteca é a tela que lê o catálogo oficial de servidores MCP e
// transforma um deles num upstream preenchido.
//
// Existe porque cadastrar um upstream à mão é acertar URL, transporte e modo de
// credencial na primeira tentativa — três campos em que errar não dá erro de
// formulário, dá um upstream degradado horas depois. A biblioteca troca isso por
// escolher um nome de uma lista.
//
// # Origem única: a lista oficial do mcpservers.org
//
// A biblioteca lê só https://mcpservers.org/pt-BR/official: um índice paginado
// (652 servidores em 22 páginas, medido em 2026-09-11) e, para cada servidor
// listado, a página de detalhe. O nome do item é "mcpservers.org/<slug>" — sem
// namespace de fornecedor nem barra dupla, ao contrário do formato de origens
// já descartadas (ver REMOVED em proposal.md).
//
// Nem toda página de detalhe publica um comando de instalação limpo, e o item
// entra assim mesmo, sem comando, com nome, descrição e site preenchidos, para
// "Adicionar" abrir o formulário com o que existe — ver lerOficial, em
// curadoria.go. Quando a descrição ou o bloco de conexão da página expõe uma
// URL de MCP (HTTP streamable ou SSE), o item entra como remoto, com a URL
// preenchida e sem comando — ver D-02 em design.md.
//
// Página de detalhe que não pôde ser lida — 5xx, 404 ou 200 sem título — é
// contada como indisponível e a varredura segue; acima de 10% de detalhes
// indisponíveis num índice, ela falha e preserva o catálogo anterior.
//
// # A semente
//
// A primeira varredura leva de 20 a 35 minutos, e a tela passava esse tempo sem
// servir para nada. Por isso o binário carrega um catálogo versionado — ver
// semente.go —, regerado pela task `biblioteca:semente`, que entra no banco no
// primeiro boot com a data em que foi gerado. Essa data é o que faz o
// sincronizador considerá-lo vencido e varrer em seguida: a semente é ponto de
// partida, nunca o catálogo em vigor. Instalação que já tinha catálogo de uma
// origem anterior tem esse catálogo descartado pela migração 00015, antes desse
// primeiro boot.
//
// # O catálogo é copiado, não lido ao vivo
//
// Existe cópia local em SQLite, refeita de tempos em tempos pelo
// Sincronizador — a única parte deste pacote que fala com a rede, respeitando a
// pausa de 2 s entre páginas que a origem exige e concluindo dentro do prazo de
// 1 h (PrazoDaVarredura). A tela lê o banco, nunca a origem: e continua
// funcionando com a internet fora.
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
// # O modo de credencial só vai quando a origem declarou
//
// O link que abre o formulário de upstream leva modo=oauth **só** quando a
// página de detalhe declarou a autenticação daquele servidor. Sem essa
// declaração o formulário fica no padrão dele: adivinhar OAuth a partir do nada
// produziria um fluxo de consentimento que não fecha, e adivinhar estática
// produziria um upstream que nasce com 401.
package biblioteca

import (
	"errors"
	"fmt"
	"regexp"
)

// Transportes possíveis. São os mesmos rótulos que o formulário de upstream usa
// em ?tipo=, de propósito: o valor viaja daqui para lá sem tradução, e uma
// tabela de conversão no meio é onde os dois lados divergem.
const (
	TransporteHTTP  = "http"
	TransporteSSE   = "sse"
	TransporteSTDIO = "stdio"
)

// Formas de autenticação que a página de detalhe pode declarar.
//
// Não é o modo de credencial do upstream — é o que o servidor exige. Nem toda
// página de detalhe cita autenticação, e por isso um item pode chegar com
// Autenticacao vazia e o formulário fica no padrão dele.
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
	// mcpservers.org".
	ErrOrigemIndisponivel = errors.New("biblioteca: origem indisponível")
	// ErrFormatoDaOrigem é a resposta tendo chegado e não ser o que este pacote
	// sabe ler — a marcação do mcpservers.org mudou. É separado de
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
// A listagem do índice não basta: endpoint, transporte e autenticação, quando
// existem, só aparecem na página de detalhe, e é por isso que a varredura faz
// uma segunda requisição por servidor — ver lerOficial, em curadoria.go. Um
// item sem comando aproveitável entra do mesmo jeito, como stdio com Comando
// vazio (D-03): recusar o item inteiro custaria mais do que um formulário que
// o admin completa.
type Item struct {
	// Nome é o identificador no catálogo: "mcpservers.org/<slug>". É ele que
	// volta pela URL quando o admin clica em adicionar.
	Nome string
	// Titulo é o nome de exibição. Cai para Nome quando a origem não declara um.
	Titulo string
	// Descricao é a linha do catálogo.
	Descricao string

	// Transporte é http, sse ou stdio.
	Transporte string
	// URL é o endpoint, nos transportes remotos. Vazio no stdio.
	URL string
	// Endpoints são todos os endereços que a página publicou numa tabela de
	// endpoints, na ordem em que aparecem (D-07). A Cloudflare publica 17;
	// quase todo mundo publica um só, e a maioria não publica nenhum, e aí
	// fica vazio. Quando não é vazio, URL é um deles — é o escolhido, a linha
	// marcada "recomendado" ou a primeira.
	Endpoints []string
	// Comando e Args são a execução, no stdio. Vazios nos remotos.
	Comando string
	Args    []string

	// Autenticacao é o que o servidor exige: AutOAuth, AutToken ou AutAberta.
	// Vazia quando ninguém declarou — só entra quando a página de detalhe
	// cita a forma de autenticação (ver D-02 em design.md).
	Autenticacao string
	// PedeCredencial é a página de detalhe ter declarado que o servidor exige
	// credencial: autenticação não-aberta.
	PedeCredencial bool
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

// padraoDoNome é a forma de um nome do catálogo: um primeiro segmento, barra, e
// mais um ou dois. Maiúscula entra porque a origem publica assim
// (mcpservers.org/AudienseCo/mcp-audiense-insights) — o slug do acervo pode ter
// uma barra no meio, e encurtá-lo criaria colisão entre dois servidores da mesma
// organização.
const padraoDoNome = "^[A-Za-z0-9][A-Za-z0-9._-]{0,120}(?:/[A-Za-z0-9][A-Za-z0-9._-]{0,120}){1,2}$"

var reNome = regexp.MustCompile(padraoDoNome)

// nomeValido barra o que nunca poderia ser um nome da origem antes de virar
// busca. É higiene de borda: o nome chega pela URL da tela.
func nomeValido(s string) bool { return reNome.MatchString(s) }
