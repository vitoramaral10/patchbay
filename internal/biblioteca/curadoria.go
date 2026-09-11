package biblioteca

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BaseCuradoria é a origem: o acervo "oficial" do mcpservers.org.
//
// O caminho carrega o idioma: /pt-BR traz resumo e descrição traduzidos, e os
// valores que viram configuração (o comando, os argumentos) não são traduzidos.
const BaseCuradoria = "https://mcpservers.org/pt-BR"

const (
	// tetoDaResposta corta o corpo da origem. O teto é folga larga sobre as
	// páginas medidas, e existe porque ler sem limite o corpo de um terceiro é
	// como se enche a memória do processo.
	tetoDaResposta = 8 << 20
	// timeoutPadrao é o prazo de uma ida à origem, do connect ao último byte.
	//
	// Quarenta e cinco segundos, e não os quinze de um cliente comum, porque a
	// medição de 2026-09-09 pegou requisições avulsas passando de 40s sem
	// responder. Quem espera aqui é a varredura de fundo, que tem paciência de
	// sobra; cortar antes só trocaria uma página lenta por uma tentativa a mais.
	// O que impede a paciência de virar varredura eterna é o prazo do conjunto,
	// em PrazoDaVarredura.
	timeoutPadrao = 45 * time.Second
	// esperaEntreCuradas é a pausa entre duas páginas de detalhe.
	//
	// Dois segundos, e o número é medido, não escolhido. A origem limita taxa:
	// com 250 ms de pausa, uma varredura de verdade em 2026-09-09 trouxe **63
	// de 293** — as outras 230 vieram com HTTP 429. Com dois segundos, a mesma
	// sequência passa. São ~22 min para as ~651 páginas (29m26s medidos em
	// 2026-09-11), dentro de uma varredura de fundo que roda de doze em doze
	// horas: a pressa aqui não vale nada.
	//
	// A origem não manda Retry-After, então não há o que obedecer — o que
	// resta é ir devagar e insistir, e quem insiste é o sincronizador.
	esperaEntreCuradas = 2 * time.Second
	// agenteDeNavegador é o User-Agent que a origem aceita. Não é disfarce: o
	// robots.txt de lá libera /official, e o que se pede é a mesma página que
	// um leitor humano abre.
	agenteDeNavegador = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// Curadoria é o cliente do mcpservers.org.
type Curadoria struct {
	base    string
	cliente *http.Client
}

// NovaCuradoria monta o cliente. base vazio usa BaseCuradoria.
func NovaCuradoria(base string) *Curadoria {
	if base == "" {
		base = BaseCuradoria
	}
	return &Curadoria{
		base: strings.TrimSuffix(base, "/"),
		cliente: &http.Client{
			Timeout: timeoutPadrao,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && req.URL.Host != via[0].URL.Host {
					return fmt.Errorf("%w: redirecionamento para outro host (%s)",
						ErrOrigemIndisponivel, req.URL.Host)
				}
				if len(via) >= 5 {
					return errors.New("redirecionamentos demais")
				}
				return nil
			},
		},
	}
}

// buscar traz uma página da origem.
//
// recusado por slugDeServidorValido; o cliente ainda barra redirecionamento
// para outro host
//
//nolint:gosec // a URL é a base do pacote mais um caminho fixo, com o slug já
func (c *Curadoria) buscar(ctx context.Context, alvo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	req.Header.Set("User-Agent", agenteDeNavegador)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")

	resp, err := c.cliente.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	defer func() { _ = resp.Body.Close() }()

	corpo, err := io.ReadAll(io.LimitReader(resp.Body, tetoDaResposta))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrNaoEncontrado
	case resp.StatusCode == http.StatusTooManyRequests:
		// Separado dos demais porque a ação é outra: 429 passa se esperar, e é
		// o erro que a varredura precisa insistir em vez de descartar a página.
		return "", fmt.Errorf("%w: HTTP 429", ErrTaxaExcedida)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: HTTP %d", ErrOrigemIndisponivel, resp.StatusCode)
	}
	// O desafio de bot do Cloudflare chega com 200 e HTML no lugar da página.
	// Sem esta checagem ele viraria "formato mudou", que manda procurar o
	// defeito no lugar errado.
	if desafioDeBot(string(corpo)) {
		return "", fmt.Errorf("%w: a origem respondeu com desafio de bot", ErrOrigemIndisponivel)
	}
	return string(corpo), nil
}

func desafioDeBot(s string) bool {
	inicio := s
	if len(inicio) > 600 {
		inicio = inicio[:600]
	}
	return strings.Contains(inicio, "Just a moment")
}

// As expressões que traduzem a página da origem.
//
// São regulares e não um parser de HTML de verdade porque o que se lê são
// poucos pedaços de marcação estáveis, e uma árvore inteira para isso custaria
// uma dependência nova. Cada uma está amarrada a um pedaço nomeado da página, e
// quando uma delas para de casar o pacote devolve ErrFormatoDaOrigem — nunca um
// item pela metade, que viraria um cadastro errado.
var (
	// Os campos da página de detalhe.
	reTituloCurado = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
	// reDescricaoImediata pega o parágrafo que segue o título. A página de
	// verdade sempre deixa marcação entre os dois — um <span> de selo,
	// um </div> de fechamento — por isso a busca não exige que o <p> venha
	// colado no </h1>: ela varre até 300 bytes à frente à procura dele, e
	// desiste sem erro se não achar (a descrição é opcional).
	reDescricaoImediata = regexp.MustCompile(`(?s)^.{0,300}?<p[^>]*>(.*?)</p>`)
	// reSobreCurado é o plano B quando não há parágrafo logo após o título.
	reSobreCurado = regexp.MustCompile(`(?s)<h2[^>]*>\s*(?:Sobre|About) [^<]*</h2>.{0,400}?<p[^>]*>(.*?)</p>`)

	// reSiteCurado é o link de destaque perto da descrição — repositório ou
	// página do projeto. A origem sempre o marca com target="_blank"; é o que
	// distingue esse link dos outros (menu, rodapé) que a página também tem.
	reSiteCurado = regexp.MustCompile(`<a\s+href="(https://[^"]+)"[^>]*target="_blank"`)

	reMarcacao = regexp.MustCompile(`<[^>]+>`)
	reEspacos  = regexp.MustCompile(`\s+`)
)

// janelaDoSite é o quanto se varre depois da descrição à procura do link do
// site. 4 KB, medido nas páginas de amostra: o link do repositório está bem
// antes disso, e o teto existe para não pegar um link qualquer lá embaixo na
// página.
const janelaDoSite = 4096

// janelaDoRotulo é o quanto se olha para trás de um endereço à procura do
// rótulo que o apresenta ("Endpoint", "Conecte-se em"). 120 caracteres, medido
// nas amostras: cabe a célula de tabela e a frase que antecede o <code>, e não
// alcança o parágrafo anterior, que falaria de outra coisa.
const janelaDoRotulo = 120

// autenticacaoDe traduz a frase da origem numa das três formas conhecidas.
//
// O padrão é token, e não aberta: errar para "precisa de token" faz a tela pedir
// uma credencial que talvez não seja necessária — chato. Errar para "aberta" faz
// o admin cadastrar sem credencial um servidor que exige uma, e o upstream nasce
// degradado com 401. Entre os dois erros, o primeiro é o barato.
//
// Quem chama daqui (lerOficial) só aproveita o AutOAuth: o acervo /servers/ não
// tem campo de autenticação, e o que se lê é prosa de README. "oauth" escrito
// por extenso é afirmação; a ausência dele não é — por isso as outras duas
// respostas viram "ninguém declarou" (D-02), e não um palpite.
func autenticacaoDe(frase string) string {
	f := strings.ToLower(frase)
	switch {
	case strings.Contains(f, "oauth"):
		return AutOAuth
	case strings.Contains(f, "aberto"), strings.Contains(f, "sem autentica"),
		strings.Contains(f, "no auth"), strings.Contains(f, "open"), strings.Contains(f, "none"):
		return AutAberta
	default:
		return AutToken
	}
}

// texto tira marcação, resolve entidade e normaliza espaço.
func texto(s string) string {
	s = reMarcacao.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(reEspacos.ReplaceAllString(s, " "))
}

// caminhoOficiais é a lista de "servidores MCP oficiais" do mcpservers.org.
//
// Estes vivem em /servers/<slug> e são, quase todos, processo local. Medido em
// 2026-09-09, o que eles declaram é bem menos do que os remotos que a mudança
// anterior também varria:
//
//   - **nenhum** tem <dt>Transporte</dt>, <dt>Autenticação</dt> nem URL no bloco
//     de conexão (0 de 10 amostrados);
//   - o comando existe só como trecho de copiar-e-colar do README, e numa
//     amostra de 14 apenas 4 eram aproveitáveis: 9 não tinham bloco algum e 1
//     trazia caminho de exemplo (C:\PATH\TO\PARENT\FOLDER).
//
// Por isso este acervo entra pelo que ele é — uma lista de nomes que alguém
// chamou de oficiais —, com ou sem comando aproveitável (D-03). O acervo maior
// do mesmo site (/all, 12.173) fica de fora: seriam 406 páginas de índice mais
// 12.173 de detalhe, ~7 horas por varredura, para a mesma qualidade de dado,
// sem estrutura nenhuma a mais.
const caminhoOficiais = "official"

// TetoDePaginasOficiais fecha a paginação de /official.
//
// Medido em 22 páginas de 30 em 2026-09-09. O teto é folga com fim: paginação
// de terceiro que passa a devolver sempre a mesma página viraria varredura
// eterna sem ele.
const TetoDePaginasOficiais = 60

// SlugsOficiais percorre a paginação de /official e devolve os identificadores.
//
// Diferente dos remotos, aqui o índice pagina: 651 servidores de 30 em 30
// (medido em 2026-09-11). A última página é descoberta pelos próprios links de
// paginação, e não chutada.
func (c *Curadoria) SlugsOficiais(ctx context.Context) ([]string, error) {
	primeira, err := c.paginaOficial(ctx, 1)
	if err != nil {
		return nil, err
	}
	slugs, visto := slugsDeServidor(primeira), map[string]bool{}
	for _, s := range slugs {
		visto[s] = true
	}
	ultima := ultimaPagina(primeira)
	if ultima > TetoDePaginasOficiais {
		ultima = TetoDePaginasOficiais
	}
	for p := 2; p <= ultima; p++ {
		pagina, err := c.paginaOficial(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("página %d: %w", p, err)
		}
		for _, s := range slugsDeServidor(pagina) {
			if visto[s] {
				continue
			}
			visto[s] = true
			slugs = append(slugs, s)
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("%w: nenhum servidor na lista de oficiais", ErrFormatoDaOrigem)
	}
	return slugs, nil
}

func (c *Curadoria) paginaOficial(ctx context.Context, n int) (string, error) {
	alvo, err := url.JoinPath(c.base, caminhoOficiais)
	if err != nil {
		return "", fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	if n > 1 {
		alvo += "?page=" + strconv.Itoa(n)
	}
	return c.buscar(ctx, alvo)
}

// Oficial lê a página de um servidor do acervo /servers/.
//
// Devolve o erro de buscar (ErrOrigemIndisponivel, ErrNaoEncontrado ou
// ErrTaxaExcedida) quando a requisição falha, e só devolve ErrFormatoDaOrigem
// quando a página chega mas falta o essencial — título. Sem comando
// aproveitável no README, que é o caso da maioria, o item entra do mesmo
// jeito, como stdio com Comando vazio (D-03): um cartão com "adicionar" que
// abre um formulário sem comando é melhor do que o servidor não aparecer.
func (c *Curadoria) Oficial(ctx context.Context, slug string) (Item, error) {
	if !slugDeServidorValido(slug) {
		return Item{}, ErrNaoEncontrado
	}
	alvo, err := url.JoinPath(c.base, "servers", slug)
	if err != nil {
		return Item{}, fmt.Errorf("%w: base inválida: %w", ErrOrigemIndisponivel, err)
	}
	pagina, err := c.buscar(ctx, alvo)
	if err != nil {
		return Item{}, err
	}
	return lerOficial(slug, pagina)
}

// As expressões do acervo /servers/.
var (
	// reCartaoOficial casa o link de um servidor no índice. O slug pode ter
	// uma barra no meio (AudienseCo/mcp-audiense-insights) e maiúsculas.
	reCartaoOficial = regexp.MustCompile(`href="[^"]*?/servers/([^"?#]+)"`)
	// rePaginaOficial casa os links de paginação, para descobrir a última.
	rePaginaOficial = regexp.MustCompile(`/official\?page=(\d+)`)
	// reSlugDeServidor é a forma de um slug do acervo /servers/.
	reSlugDeServidor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}(?:/[A-Za-z0-9][A-Za-z0-9._-]{0,80})?$`)

	// reComandoDoSnippet tira o comando e, quando existem, os argumentos do
	// trecho de copiar-e-colar que a página mostra. É JSON dentro do HTML,
	// escapado, e por isso a leitura acontece depois de resolver as entidades.
	//
	// Os "args" são opcionais (D-03: "command" sozinho é snippet válido) e, se
	// vierem, têm de vir depois do comando — é assim que o site escreve. Args
	// antes do comando não casa, e não casar é o resultado certo: o que sai
	// daqui vira linha de comando de um processo, e meio acerto ali é um
	// upstream que não sobe.
	reComandoDoSnippet = regexp.MustCompile(
		`(?s)"command"\s*:\s*"([^"]+)"(?:\s*,\s*"args"\s*:\s*\[([^\])]{0,400})\])?`)
	reArgDoSnippet = regexp.MustCompile(`"([^"]*)"`)

	// reFimDaRegiao marca onde a página para de falar do servidor dela e passa
	// a listar outros — é onde começa o bloco "Servidores relacionados", e é o
	// primeiro link para outra página de servidor do site.
	//
	// O acervo aparece em duas formas de caminho, a de /servers/ e a da lista
	// de remotos, que prefixa o mesmo final; o padrão cobre as duas sem
	// escrever o nome da segunda por extenso, que saiu do pacote (CA-02).
	//
	// Ler além daqui é o defeito que a página do 1Password mostrou: o vizinho
	// publica endpoint rotulado, e sem o corte o endereço dele viraria o deste.
	reFimDaRegiao = regexp.MustCompile(`href="[^"]*/(?:remote-)?(?:mcp-)?servers/`)

	// reSinalDeRemoto é como a origem diz, na descrição, que o servidor é de
	// rede e não processo local. Sem essa frase e sem endereço rotulado, um
	// https qualquer no meio do README não é convite para conectar (D-02).
	reSinalDeRemoto = regexp.MustCompile(
		`(?i)servidor[^.<]{0,24}remoto|remote mcp server|streamable http|http (streamable|transmiss)`)
	// reRotuloDeConexao é o que a origem escreve imediatamente antes de um
	// endereço de conexão, nas duas línguas em que ela publica.
	reRotuloDeConexao = regexp.MustCompile(
		`(?i)endpoint|conecte-se em|connect to|adicione|streamable http|http transmissível|url de conexão|connection urls?`)
	// reEnderecoHTTPS pega qualquer https da região; quem filtra é o caminho.
	reEnderecoHTTPS = regexp.MustCompile(`https://[^\s"'<>)\]]+`)
	// reCaminhoDeMCP é a forma do caminho de um endpoint de MCP. Endereço que
	// não termina assim fica de fora — o falso negativo é o erro barato: o item
	// entra como os outros, sem comando (D-02).
	reCaminhoDeMCP = regexp.MustCompile(`/(?:mcp|sse)/?$`)
	// reCaminhoDeSSE separa os dois transportes remotos.
	reCaminhoDeSSE = regexp.MustCompile(`/sse/?$`)
	// reEnderecoNegado é a frase que desmente o endereço que ela cita. A
	// página do Apify é o caso medido: o rótulo "endpoint" apresenta o SSE
	// legado justamente para anunciar que ele saiu do ar, e o endereço vivo
	// não tem caminho de MCP. Sem esta guarda, o rótulo faz o endereço morto
	// passar por endpoint (D-02).
	reEnderecoNegado = regexp.MustCompile(
		`(?i)removido|removed|legado|legacy|deprecated|descontinuado|não está mais|no longer`)

	// A tabela de endpoints da página de detalhe. Quando o fornecedor publica
	// mais de um endereço — a Cloudflare publica 17 —, ele não escreve rótulo
	// nenhum: monta uma tabela e põe os endereços numa coluna. O nome da coluna
	// é o rótulo, e vale para todas as linhas de uma vez (D-02 emenda 2).
	reTabela            = regexp.MustCompile(`(?is)<table[^>]*>(.*?)</table>`)
	reCabecalhoDaTabela = regexp.MustCompile(`(?is)<thead[^>]*>(.*?)</thead>`)
	reCelulaDeCabecalho = regexp.MustCompile(`(?is)<th[^>]*>(.*?)</th>`)
	reLinhaDaTabela     = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	reCelulaDaTabela    = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	// reColunaDeEndpoint é o nome de coluna que anuncia endereço de conexão,
	// nas duas línguas em que a origem publica ("URL do Servidor", "Endpoint").
	// Coluna de nome ou de descrição não casa, e é isso que impede o link do
	// repositório, que mora na coluna de nome, de virar endpoint.
	reColunaDeEndpoint = regexp.MustCompile(`(?i)url|endpoint`)
	// reLinhaRecomendada é como a origem marca, entre vários endpoints, aquele
	// por onde ela quer que se comece.
	reLinhaRecomendada = regexp.MustCompile(`(?i)recomendad[oa]|recommended`)

	// rePlaceholder são os marcadores que o README deixa para a pessoa trocar.
	// Um comando com isso dentro não sobe, e cadastrá-lo assim seria entregar um
	// upstream quebrado com cara de pronto.
	replaceholderNoArg = regexp.MustCompile(`(?i)PATH[/\\]TO|YOUR[_ ]|<[A-Z_]{3,}>|CAMINHO[_/ ]|/path/to|SEU[_ ]`)
)

func slugsDeServidor(pagina string) []string {
	achados := reCartaoOficial.FindAllStringSubmatch(pagina, -1)
	visto := make(map[string]bool, len(achados))
	slugs := make([]string, 0, len(achados))
	for _, a := range achados {
		s := a[1]
		if visto[s] || !slugDeServidorValido(s) {
			continue
		}
		visto[s] = true
		slugs = append(slugs, s)
	}
	return slugs
}

// ultimaPagina lê o maior número que os links de paginação citam.
//
// A página mostra os vizinhos e a última — é dela que sai o total, e é por isso
// que este código não precisa adivinhar quantas páginas existem.
func ultimaPagina(pagina string) int {
	maior := 1
	for _, m := range rePaginaOficial.FindAllStringSubmatch(pagina, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maior {
			maior = n
		}
	}
	return maior
}

func slugDeServidorValido(s string) bool { return reSlugDeServidor.MatchString(s) }

// lerOficial tira da página de um servidor do acervo /servers/ o que dá para
// cadastrar.
//
// Nome, título e site são o mínimo — título é obrigatório, o resto é
// oportunista. Comando é o campo raro (D-03): a maioria das páginas não tem
// um aproveitável, e a partir desta tarefa isso deixou de ser recusa — o item
// entra assim mesmo, sem comando, porque um cartão com nome, descrição e link
// para o repositório ainda é útil para quem está escolhendo o que instalar.
func lerOficial(slug, pagina string) (Item, error) {
	i := Item{
		// A barra do slug sobrevive no nome, e o nome fica com três segmentos
		// (mcpservers.org/AudienseCo/mcp-audiense-insights). É de propósito: o
		// nome é identidade, e encurtá-lo criaria colisão entre dois servidores
		// da mesma organização.
		Nome:       "mcpservers.org/" + slug,
		Transporte: TransporteSTDIO,
	}
	tm := reTituloCurado.FindStringSubmatchIndex(pagina)
	if tm == nil {
		return Item{}, fmt.Errorf("%w: %s sem nome", ErrFormatoDaOrigem, slug)
	}
	i.Titulo = texto(pagina[tm[2]:tm[3]])
	if i.Titulo == "" {
		return Item{}, fmt.Errorf("%w: %s sem nome", ErrFormatoDaOrigem, slug)
	}

	// fimDaDescricao começa no fim do título e avança se um parágrafo for
	// achado — é a partir dali que a busca pelo site começa.
	fimDaDescricao := tm[1]
	if dm := reDescricaoImediata.FindStringSubmatchIndex(pagina[tm[1]:]); dm != nil {
		i.Descricao = texto(pagina[tm[1]+dm[2] : tm[1]+dm[3]])
		fimDaDescricao = tm[1] + dm[1]
	}
	if i.Descricao == "" {
		if sm := reSobreCurado.FindStringSubmatchIndex(pagina); sm != nil {
			i.Descricao = texto(pagina[sm[2]:sm[3]])
			fimDaDescricao = sm[1]
		}
	}
	i.Site = siteApos(pagina, fimDaDescricao)

	// Uma minoria destes "oficiais" é serviço de rede, e a página diz isso em
	// prosa: descrição de servidor remoto mais um endereço de MCP. Quando as
	// duas coisas estão lá, o item é remoto e a URL é a forma de conexão —
	// então o snippet de instalação do README, se existir, não entra: comando e
	// argumentos são do stdio (D-02).
	regiao := regiaoDoServidor(pagina, tm[1])
	// A tabela de endpoints vem antes do rótulo e da descrição: uma coluna
	// chamada "URL do Servidor" é a declaração mais explícita que a página sabe
	// dar do que é endereço de conexão — e do que não é, já que o que está nas
	// outras colunas fica de fora (D-02 emenda 2).
	if escolhido, todos, ok := endpointsDaTabela(regiao); ok {
		i.Transporte, i.URL, i.Endpoints = transporteDoEndereco(escolhido), escolhido, todos
		if autenticacaoDe(regiao) == AutOAuth {
			i.Autenticacao, i.PedeCredencial = AutOAuth, true
		}
		return i, nil
	}
	if endereco, transporte, ok := conexaoRemota(regiao, i.Descricao); ok {
		i.Transporte, i.URL = transporte, endereco
		if autenticacaoDe(regiao) == AutOAuth {
			i.Autenticacao, i.PedeCredencial = AutOAuth, true
		}
		return i, nil
	}

	if comando, args, ok := execucaoDoSnippet(pagina); ok {
		i.Comando, i.Args = comando, args
	}
	return i, nil
}

// regiaoDoServidor recorta o trecho da página que fala do servidor dela: do fim
// do título até o primeiro link para outro servidor do acervo, com as entidades
// já resolvidas — o que vem depois é vitrine de vizinhos.
func regiaoDoServidor(pagina string, pos int) string {
	if pos < 0 || pos > len(pagina) {
		return ""
	}
	resto := pagina[pos:]
	if m := reFimDaRegiao.FindStringIndex(resto); m != nil {
		resto = resto[:m[0]]
	}
	return html.UnescapeString(resto)
}

// conexaoRemota acha na região o endereço pelo qual se conecta ao servidor, e o
// transporte que ele implica.
//
// São duas condições, e as duas precisam valer (D-02): o servidor tem de se
// anunciar como remoto — na descrição, ou pelo próprio endereço vir apresentado
// por um rótulo de conexão — e o endereço tem de ter caminho de MCP. Sem as
// duas não há remoto, e o item segue como os demais, sem comando: o README que
// mostra `url = "https://…/mcp"` dentro de um exemplo de configuração está
// documentando outra coisa, e cadastrá-lo como endpoint criaria um upstream que
// aponta para lugar nenhum.
func conexaoRemota(regiao, descricao string) (endereco, transporte string, ok bool) {
	seDizRemoto := reSinalDeRemoto.MatchString(descricao)
	for _, m := range reEnderecoHTTPS.FindAllStringIndex(regiao, -1) {
		// A pontuação da frase gruda no fim do endereço quando ele vem solto no
		// texto ("…/mcp."); o caminho é o que decide, então ela sai antes.
		u := strings.TrimRight(regiao[m[0]:m[1]], ".,;:")
		if !reCaminhoDeMCP.MatchString(u) {
			continue
		}
		antes, depois := vizinhanca(regiao, m[0], m[1])
		// A guarda de negação vem antes de qualquer coisa e vale para os dois
		// caminhos, o do rótulo e o da descrição: endereço desmentido não é
		// endereço. Descartar um candidato não encerra a busca — a região pode
		// citar o endpoint velho e o novo, e o próximo candidato ainda vale.
		if reEnderecoNegado.MatchString(antes) || reEnderecoNegado.MatchString(depois) {
			continue
		}
		rotulado := reRotuloDeConexao.MatchString(antes)
		if !rotulado && !(seDizRemoto && strings.Contains(descricao, u)) {
			continue
		}
		if reCaminhoDeSSE.MatchString(u) {
			return u, TransporteSSE, true
		}
		return u, TransporteHTTP, true
	}
	return "", "", false
}

// endpointsDaTabela lê os endereços de conexão que a região publica numa tabela
// de endpoints: todos eles, na ordem da página, mais o escolhido para ser a URL
// do item.
//
// A âncora é o cabeçalho, não o endereço: só entra a coluna cujo <th> se chama
// de URL ou de endpoint, e só as <td> dela. É o que basta para a página da
// Cloudflare — "Nome do Servidor | Descrição | URL do Servidor", 17 linhas, sem
// rótulo de conexão em lugar nenhum — entrar inteira sem que os links de
// repositório da coluna de nome entrem junto.
//
// O escolhido é o da primeira linha que se diz recomendada; sem nenhuma, o da
// primeira linha. Quem escolhe é a página, não a ordem alfabética: a Cloudflare
// marca "(recomendado)" no servidor Code Mode, que é o de acesso amplo.
func endpointsDaTabela(regiao string) (escolhido string, endpoints []string, ok bool) {
	for _, tabela := range reTabela.FindAllStringSubmatch(regiao, -1) {
		coluna := colunaDeEndpoint(tabela[1])
		if coluna < 0 {
			continue
		}
		var achados []string
		var recomendado string
		for _, linha := range reLinhaDaTabela.FindAllStringSubmatch(tabela[1], -1) {
			// A linha do <thead> tem <th> e nenhum <td>, então cai aqui e sai.
			celulas := reCelulaDaTabela.FindAllStringSubmatch(linha[1], -1)
			if coluna >= len(celulas) {
				continue
			}
			endereco, achou := enderecoDaCelula(celulas[coluna][1])
			if !achou {
				continue
			}
			if recomendado == "" && reLinhaRecomendada.MatchString(texto(linha[1])) {
				recomendado = endereco
			}
			achados = append(achados, endereco)
		}
		if len(achados) == 0 {
			continue
		}
		if recomendado == "" {
			recomendado = achados[0]
		}
		return recomendado, achados, true
	}
	return "", nil, false
}

// colunaDeEndpoint devolve o índice da coluna de endereços, ou -1 quando a
// tabela não tem uma. Tabela sem <thead> não tem rótulo de coluna e, por isso,
// não ancora nada.
func colunaDeEndpoint(tabela string) int {
	cabecalho := reCabecalhoDaTabela.FindStringSubmatch(tabela)
	if cabecalho == nil {
		return -1
	}
	for n, celula := range reCelulaDeCabecalho.FindAllStringSubmatch(cabecalho[1], -1) {
		if reColunaDeEndpoint.MatchString(texto(celula[1])) {
			return n
		}
	}
	return -1
}

// enderecoDaCelula tira de uma célula da coluna de endereços o endpoint que ela
// publica, se publicar um.
//
// A guarda de negação vale célula a célula, como no caminho do rótulo: a linha
// que existe para dizer que aquele endereço saiu do ar não entrega endpoint.
func enderecoDaCelula(celula string) (string, bool) {
	if reEnderecoNegado.MatchString(texto(celula)) {
		return "", false
	}
	for _, bruto := range reEnderecoHTTPS.FindAllString(celula, -1) {
		// A pontuação da frase gruda no fim do endereço quando ele vem solto no
		// texto da célula; o caminho é o que decide, então ela sai antes.
		endereco := strings.TrimRight(bruto, ".,;:")
		if !reCaminhoDeMCP.MatchString(endereco) || ehRepositorio(endereco) {
			continue
		}
		return endereco, true
	}
	return "", false
}

// hospedeirosDeRepositorio são os hospedeiros de código. Um endereço deles
// nunca é endpoint, por mais que o caminho termine em /mcp: github.com/x/mcp é
// o repositório do servidor, não o servidor (D-02). Cadastrá-lo criaria um
// upstream apontando para uma página de HTML.
var hospedeirosDeRepositorio = map[string]bool{
	"github.com":    true,
	"gitlab.com":    true,
	"bitbucket.org": true,
}

// ehRepositorio diz se o endereço é de código-fonte. Endereço que não dá para
// analisar conta como repositório — quem não sabe de quem é o host não tem como
// oferecê-lo para conexão.
func ehRepositorio(endereco string) bool {
	u, err := url.Parse(endereco)
	if err != nil {
		return true
	}
	return hospedeirosDeRepositorio[strings.ToLower(u.Hostname())]
}

// transporteDoEndereco decide entre os dois transportes remotos pelo caminho do
// endereço.
func transporteDoEndereco(endereco string) string {
	if reCaminhoDeSSE.MatchString(endereco) {
		return TransporteSSE
	}
	return TransporteHTTP
}

// vizinhanca devolve os janelaDoRotulo caracteres de cada lado do endereço que
// ocupa [inicio, fim) na região — o que o apresenta e o que a frase diz dele
// em seguida.
func vizinhanca(regiao string, inicio, fim int) (antes, depois string) {
	a := inicio - janelaDoRotulo
	if a < 0 {
		a = 0
	}
	d := fim + janelaDoRotulo
	if d > len(regiao) {
		d = len(regiao)
	}
	return regiao[a:inicio], regiao[fim:d]
}

// siteApos procura o link de destaque nos janelaDoSite bytes seguintes a pos.
// Sem link nessa faixa, o site fica vazio — sem erro, porque nem toda página
// tem um.
func siteApos(pagina string, pos int) string {
	if pos < 0 || pos > len(pagina) {
		return ""
	}
	fim := pos + janelaDoSite
	if fim > len(pagina) {
		fim = len(pagina)
	}
	m := reSiteCurado.FindStringSubmatch(pagina[pos:fim])
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// execucaoDoSnippet lê o comando do trecho de configuração da página.
//
// Recusa o que tem marcador de exemplo dentro: "C:/PATH/TO/PARENT/FOLDER" e
// "YOUR_API_KEY" são pedidos para a pessoa trocar, e cadastrá-los como se
// fossem configuração entrega um upstream quebrado com cara de pronto. Um
// comando recusado (ou nenhum snippet) não é erro — quem chama trata o
// terceiro retorno como "sem comando aproveitável" (D-03).
func execucaoDoSnippet(pagina string) (string, []string, bool) {
	m := reComandoDoSnippet.FindStringSubmatch(html.UnescapeString(pagina))
	if m == nil {
		return "", nil, false
	}
	comando := strings.TrimSpace(m[1])
	if comando == "" || replaceholderNoArg.MatchString(comando) {
		return "", nil, false
	}
	// m[2] fica vazio tanto quando "args" não veio no snippet quanto quando
	// veio como lista vazia — nos dois casos o comando sozinho já é válido.
	var args []string
	for _, a := range reArgDoSnippet.FindAllStringSubmatch(m[2], -1) {
		v := strings.TrimSpace(a[1])
		if v == "" {
			continue
		}
		if replaceholderNoArg.MatchString(v) {
			return "", nil, false
		}
		args = append(args, v)
	}
	return comando, args, true
}
