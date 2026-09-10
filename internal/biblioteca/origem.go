package biblioteca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// BaseRegistry é a origem em produção: o registry oficial do Model Context
// Protocol.
//
// É API JSON de verdade, e não HTML de terceiro lido por regex — a razão da
// troca está no comentário do pacote. Não há robots.txt a contornar nem desafio
// de bot no caminho.
const BaseRegistry = "https://registry.modelcontextprotocol.io"

// PorPagina é quantos servidores a tela pede por vez.
//
// Cem é o teto da origem: limit=500 responde 422. Pedir o máximo é o que faz a
// tela paginar pouco num catálogo de quase trinta mil.
const PorPagina = 100

const (
	// tetoDaResposta corta o corpo da origem. Uma página de cem servidores fica
	// em ~80 kB; o teto é folga, e existe porque ler sem limite o corpo de um
	// terceiro é como se enche a memória do processo.
	tetoDaResposta = 8 << 20
	// timeoutPadrao é o prazo de uma ida à origem, do connect ao último byte.
	//
	// Quarenta e cinco segundos, e não os quinze de um cliente comum, porque a
	// medição de 2026-09-09 pegou requisições ao registry passando de 40s sem
	// responder. Quem espera aqui é a varredura de fundo, que tem paciência de
	// sobra; cortar antes só trocaria uma página lenta por uma tentativa a mais.
	// O que impede a paciência de virar varredura eterna é o prazo do conjunto,
	// em PrazoDaVarredura.
	timeoutPadrao = 45 * time.Second
	// agente identifica o patchbay para a origem. Ao contrário do cliente
	// anterior, que precisava se passar por navegador para o mcpservers.org
	// responder, aqui não há motivo para disfarce: é uma API pública pedindo
	// JSON.
	agente = "patchbay (+https://github.com/vitoramaral10/patchbay)"
	// caminhoServidores é o único endpoint que este pacote usa.
	caminhoServidores = "/v0/servers"
)

// Origem é o cliente do registry.
//
// Não guarda nada: quem guarda é o repositório, e quem escreve nele é o
// Sincronizador. A tela nunca passa por aqui — é justamente por isso que a
// busca digitada não toca a rede.
type Origem struct {
	base    string
	cliente *http.Client
}

// NovaOrigem monta o cliente. base vazio usa BaseRegistry.
func NovaOrigem(base string) *Origem {
	if base == "" {
		base = BaseRegistry
	}
	return &Origem{
		base: strings.TrimSuffix(base, "/"),
		cliente: &http.Client{
			Timeout: timeoutPadrao,
			// Nunca seguir redirecionamento para fora do host da origem: a
			// biblioteca chama um endpoint conhecido, e um 302 para outro lugar
			// é coisa que só interessa a quem quer que o patchbay busque outra
			// coisa.
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

// Listar devolve uma página do catálogo, já filtrada pelo termo.
//
// O filtro é da origem, não daqui: ?search= casa em nome e descrição, que é o
// que faz "jira" achar o Atlassian. A versão anterior filtrava localmente
// porque a busca do mcpservers.org só casava pelo nome — e podia, porque eram
// 293 itens. Com quase trinta mil, baixar tudo para filtrar em memória não é
// opção.
//
// cursor vazio é a primeira página. O cursor devolvido em Resultado é opaco:
// vem da origem e volta para ela sem ser interpretado.
func (o *Origem) Listar(ctx context.Context, termo, cursor string) (Resultado, error) {
	valores := url.Values{
		"limit":   {fmt.Sprint(PorPagina)},
		"version": {"latest"},
	}
	if t := strings.TrimSpace(termo); t != "" {
		valores.Set("search", t)
	}
	if cursor != "" {
		valores.Set("cursor", cursor)
	}
	resposta, err := o.pedir(ctx, valores)
	if err != nil {
		return Resultado{}, err
	}
	res := Resultado{
		Itens:         make([]Item, 0, len(resposta.Servers)),
		ProximoCursor: resposta.Metadata.NextCursor,
	}
	for _, entrada := range resposta.Servers {
		if item, ok := itemDe(entrada.Server); ok {
			res.Itens = append(res.Itens, item)
		}
	}
	return res, nil
}

// respostaAPI é o recorte do esquema do registry que este pacote lê.
//
// Só os campos usados: o esquema é grande, e declarar o que não se usa faz o
// compilador deixar passar mudança em campo que ninguém lê. O que falta aqui é
// ignorado pelo decoder, de propósito — campo novo na origem não quebra a tela.
type respostaAPI struct {
	Servers []struct {
		Server servidorAPI `json:"server"`
	} `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
	} `json:"metadata"`
}

type servidorAPI struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
	WebsiteURL  string `json:"websiteUrl"`
	Repository  struct {
		URL string `json:"url"`
	} `json:"repository"`
	Remotes []struct {
		Type    string `json:"type"`
		URL     string `json:"url"`
		Headers []struct {
			Name       string `json:"name"`
			IsRequired bool   `json:"isRequired"`
			IsSecret   bool   `json:"isSecret"`
		} `json:"headers"`
	} `json:"remotes"`
	Packages []pacoteAPI `json:"packages"`
}

type pacoteAPI struct {
	RegistryType string `json:"registryType"`
	Identifier   string `json:"identifier"`
	Version      string `json:"version"`
	RuntimeHint  string `json:"runtimeHint"`
	Transport    struct {
		Type string `json:"type"`
	} `json:"transport"`
	RuntimeArguments []argumentoAPI `json:"runtimeArguments"`
	PackageArguments []argumentoAPI `json:"packageArguments"`
}

type argumentoAPI struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// chaveServers é a chave que toda resposta do endpoint traz, mesmo quando a
// busca não achou nada.
const chaveServers = "\"servers\""

// pedir faz uma requisição ao endpoint de servidores.
//
// O gosec marca a chamada abaixo como SSRF porque a URL é variável e não
// literal. Ela não vem de fora: é a base — constante do pacote em produção, e
// um httptest no teste — mais um caminho fixo, e os únicos pedaços variáveis (o
// termo e o cursor) entram como query já escapada por url.Values. O cliente
// ainda recusa redirecionamento que troque de host, então nem a resposta da
// origem consegue mover a requisição para outro lugar.
//
//nolint:gosec // ver o parágrafo acima: a URL não é entrada de terceiro
func (o *Origem) pedir(ctx context.Context, valores url.Values) (respostaAPI, error) {
	var vazia respostaAPI
	alvo := o.base + caminhoServidores + "?" + valores.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
	if err != nil {
		return vazia, fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	req.Header.Set("User-Agent", agente)
	req.Header.Set("Accept", "application/json")

	resp, err := o.cliente.Do(req)
	if err != nil {
		return vazia, fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	defer func() { _ = resp.Body.Close() }()

	corpo, err := io.ReadAll(io.LimitReader(resp.Body, tetoDaResposta))
	if err != nil {
		return vazia, fmt.Errorf("%w: %w", ErrOrigemIndisponivel, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Inclusive o 404: aqui ele é o endpoint ter sumido, não um servidor
		// não existir. Servidor que não existe é uma busca com zero
		// resultados, e essa chega com 200.
		return vazia, fmt.Errorf("%w: HTTP %d", ErrOrigemIndisponivel, resp.StatusCode)
	}

	var resposta respostaAPI
	if err := json.Unmarshal(corpo, &resposta); err != nil {
		return vazia, fmt.Errorf("%w: %w", ErrFormatoDaOrigem, err)
	}
	// Resposta sem a chave servers é o esquema ter mudado; resposta com
	// servers vazio é busca sem resultado, e isso é legítimo. Distinguir os
	// dois é o que impede "a origem mudou" aparecer para quem só digitou um
	// termo que não existe.
	if resposta.Servers == nil && !strings.Contains(string(corpo), chaveServers) {
		return vazia, fmt.Errorf("%w: resposta sem a lista de servidores", ErrFormatoDaOrigem)
	}
	return resposta, nil
}

// itemDe traduz um servidor da origem no que a tela e o formulário usam.
//
// Devolve false quando o servidor não tem forma de conexão que o patchbay saiba
// cadastrar. Descartar é deliberado: um cartão com botão "adicionar" que abre
// um formulário vazio é pior do que o servidor não aparecer, porque o admin só
// descobre no fim.
func itemDe(s servidorAPI) (Item, bool) {
	if s.Name == "" {
		return Item{}, false
	}
	item := Item{
		Nome:      s.Name,
		Titulo:    s.Title,
		Descricao: s.Description,
		Versao:    s.Version,
		Site:      s.WebsiteURL,
	}
	if item.Titulo == "" {
		item.Titulo = s.Name
	}
	if item.Site == "" {
		item.Site = s.Repository.URL
	}

	// Remoto ganha do pacote: é o upstream que o gateway alcança pela rede, sem
	// nada instalado ao lado. Só se não houver remoto utilizável é que o
	// processo local entra.
	for _, r := range s.Remotes {
		transporte, ok := transporteDe(r.Type)
		if !ok {
			continue
		}
		// Endpoint em texto claro carregaria o bearer do upstream pela rede sem
		// cifra. Quem realmente precisar cadastra à mão, de olhos abertos.
		if !strings.HasPrefix(r.URL, "https://") {
			continue
		}
		item.Transporte, item.URL = transporte, r.URL
		for _, h := range r.Headers {
			if h.IsRequired || h.IsSecret {
				item.PedeCredencial = true
			}
		}
		return item, true
	}

	for _, p := range s.Packages {
		comando, args, ok := execucaoDe(p)
		if !ok {
			continue
		}
		item.Transporte, item.Comando, item.Args = TransporteSTDIO, comando, args
		return item, true
	}
	return Item{}, false
}

// transporteDe traduz o tipo de remote da origem no rótulo do patchbay.
//
// Tipo desconhecido não vira http por padrão: um transporte que este código não
// conhece, cadastrado como http, é um upstream que falha no primeiro handshake.
func transporteDe(tipo string) (string, bool) {
	switch tipo {
	case "streamable-http":
		return TransporteHTTP, true
	case "sse":
		return TransporteSSE, true
	default:
		return "", false
	}
}

// runtimes são os executores que recebem o identificador do pacote como
// argumento, quando a origem não declara runtimeHint.
//
// Só estes três porque neles a linha de comando é previsível: npx pacote, uvx
// pacote, dnx pacote. Fora daqui — oci, mcpb — o comando depende de flags que a
// origem não declara (imagem de container quer -i, --rm, montagem), e montá-lo
// aqui seria adivinhar. Sem runtimeHint, esses pacotes são descartados.
var runtimes = map[string]string{
	"npm":   "npx",
	"pypi":  "uvx",
	"nuget": "dnx",
}

// execucaoDe monta o comando de um servidor local a partir do que a origem
// declara.
//
// Nada é inventado além de uma coisa, dita aqui: o -y do npx. Sem ele o npx
// pergunta antes de baixar, e um upstream stdio roda sem terminal — a pergunta
// não chega a ninguém, vira processo pendurado. Isso é como o executor se
// comporta, não um palpite sobre o servidor. Se a origem já declarou o -y, ele
// não é repetido.
//
// O resto sai do que veio: argumentos posicionais declarados, o identificador
// do pacote com a versão publicada, e os posicionais do pacote. Argumento
// nomeado e variável de ambiente ficam de fora — pedem valor que só o admin
// tem, e o formulário é onde ele os preenche.
func execucaoDe(p pacoteAPI) (string, []string, bool) {
	if p.Transport.Type != "" && p.Transport.Type != "stdio" {
		return "", nil, false
	}
	if p.Identifier == "" {
		return "", nil, false
	}
	comando := p.RuntimeHint
	if comando == "" {
		comando = runtimes[p.RegistryType]
	}
	if comando == "" {
		return "", nil, false
	}

	args := posicionais(p.RuntimeArguments)
	if comando == "npx" && !contem(args, "-y") && !contem(args, "--yes") {
		args = append(args, "-y")
	}
	alvo := p.Identifier
	if p.Version != "" && p.RegistryType == "npm" {
		alvo += "@" + p.Version
	}
	args = append(args, alvo)
	return comando, append(args, posicionais(p.PackageArguments)...), true
}

func posicionais(args []argumentoAPI) []string {
	saida := make([]string, 0, len(args))
	for _, a := range args {
		if a.Type == "positional" && a.Value != "" {
			saida = append(saida, a.Value)
		}
	}
	return saida
}

func contem(lista []string, v string) bool {
	for _, x := range lista {
		if x == v {
			return true
		}
	}
	return false
}

// padraoDoNome é a forma de um nome do registry: namespace em DNS invertido,
// barra, nome. Maiúscula entra porque a origem publica assim
// (io.github.MrRefactoring/...).
// Dois ou três segmentos: "com.notion/mcp" do registry, e também
// "mcpservers.org/AudienseCo/mcp-audiense-insights", porque o slug do acervo
// /servers/ pode ter uma barra no meio e encurtá-lo criaria colisão entre dois
// servidores da mesma organização.
const padraoDoNome = "^[A-Za-z0-9][A-Za-z0-9._-]{0,120}(?:/[A-Za-z0-9][A-Za-z0-9._-]{0,120}){1,2}$"

var reNome = regexp.MustCompile(padraoDoNome)

// nomeValido barra o que nunca poderia ser um nome da origem antes de virar
// busca. É higiene de borda: o nome chega pela URL da tela.
func nomeValido(s string) bool { return reNome.MatchString(s) }
