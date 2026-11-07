package upstream

import (
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
)

// As formas livres do colar, além dos dois dialetos de comando.
//
// A documentação de um servidor MCP publica a instalação de quatro jeitos, e
// quem configura não deveria precisar saber qual é qual:
//
//	https://mcp.exemplo.com/mcp                         ← só a URL
//	npx -y @modelcontextprotocol/server-filesystem /tmp ← só o processo
//	{"mcpServers": {"fs": {"command": "npx", ...}}}     ← o JSON do Claude Desktop/Cursor
//	claude mcp add ... / npx add-mcp ...                ← os dialetos de comando.go
//
// As três primeiras viram tokens do `claude mcp add` e passam pelo mesmo
// montarDoClaude: uma validação só, uma regra de header e de env só.

// lancadores são os executáveis que abrem um MCP de processo local. A linha
// que começa por um deles é o processo inteiro, não um comando de instalação.
var lancadores = map[string]bool{
	"npx": true, "uvx": true, "bunx": true, "docker": true, "podman": true,
	"node": true, "python": true, "python3": true, "deno": true, "uv": true,
	"pipx": true, "go": true,
}

// lerFormaLivre reconhece URL, processo e JSON. ok=false devolve a decisão aos
// dialetos de comando.
func lerFormaLivre(texto string) (imp Importacao, ok bool, err error) {
	limpo := strings.TrimSpace(texto)
	switch {
	case strings.HasPrefix(limpo, "{"):
		tokens, err := tokensDoJSON(limpo)
		if err != nil {
			return Importacao{}, true, err
		}
		imp, err := montarDoClaude(tokens)
		return imp, true, err
	case !strings.ContainsAny(limpo, " \t\r\n") &&
		(strings.HasPrefix(limpo, "http://") || strings.HasPrefix(limpo, "https://")):
		// shortcut: URL pura é Streamable HTTP; SSE antigo se cola com
		// --transport sse ou pelo JSON com "type": "sse".
		transporte := TipoHTTP
		if strings.HasSuffix(strings.TrimRight(limpo, "/"), "/sse") {
			transporte = TipoSSE
		}
		imp, err := montarDoClaude([]string{"--transport", transporte, nomeDaURL(limpo), limpo})
		return imp, true, err
	}

	tokens, err := tokenizarComando(limpo)
	if err != nil || len(tokens) == 0 || !lancadores[tokens[0]] {
		return Importacao{}, false, nil
	}
	if slices.Contains(tokens, "add-mcp") { // `npx add-mcp` é dialeto, não processo
		return Importacao{}, false, nil
	}
	imp, err = montarDoClaude(append([]string{nomeDoProcesso(tokens), "--"}, tokens...))
	return imp, true, err
}

// servidorJSON é uma entrada de mcpServers, nos nomes que Claude Desktop,
// Cursor, VS Code e Windsurf usam.
type servidorJSON struct {
	Type      string            `json:"type"`
	Transport string            `json:"transport"`
	URL       string            `json:"url"`
	ServerURL string            `json:"serverUrl"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Headers   map[string]string `json:"headers"`
}

func (s servidorJSON) vazio() bool {
	return s.URL == "" && s.ServerURL == "" && s.Command == ""
}

// tokensDoJSON aceita {"mcpServers": {...}}, {"servers": {...}} (VS Code), o
// mapa nome → servidor sem embrulho, ou um servidor solto.
func tokensDoJSON(texto string) ([]string, error) {
	var bruto map[string]json.RawMessage
	if err := json.Unmarshal([]byte(texto), &bruto); err != nil {
		return nil, errors.New("O texto começa como JSON, mas não é um JSON válido. " +
			"Confira se copiou o bloco inteiro, com as chaves de abertura e fechamento.")
	}
	for _, chave := range []string{"mcpServers", "servers", "mcp"} {
		if dentro, ok := bruto[chave]; ok {
			if err := json.Unmarshal(dentro, &bruto); err != nil {
				return nil, errors.New("A chave " + chave + " do JSON não é um objeto de servidores.")
			}
			break
		}
	}

	var solto servidorJSON
	if err := json.Unmarshal([]byte(texto), &solto); err == nil && !solto.vazio() {
		return tokensDoServidor("", solto)
	}

	nomes := make([]string, 0, len(bruto))
	for nome := range bruto {
		nomes = append(nomes, nome)
	}
	sort.Strings(nomes)
	var achados []string
	var srv servidorJSON
	for _, nome := range nomes {
		var s servidorJSON
		if json.Unmarshal(bruto[nome], &s) == nil && !s.vazio() {
			achados = append(achados, nome)
			srv = s
		}
	}
	switch len(achados) {
	case 0:
		return nil, errors.New("Não achei nenhum servidor neste JSON: cada um precisa de " +
			"\"url\" ou de \"command\".")
	case 1:
		return tokensDoServidor(achados[0], srv)
	default:
		// shortcut: um MCP por colagem; aceitar vários pede uma tela de lote.
		return nil, errors.New("Este JSON traz " + strings.Join(achados, ", ") +
			". Cole um servidor por vez.")
	}
}

func tokensDoServidor(nome string, s servidorJSON) ([]string, error) {
	destino := s.URL
	if destino == "" {
		destino = s.ServerURL
	}
	var tokens []string
	if destino != "" {
		if nome == "" {
			nome = nomeDaURL(destino)
		}
		tipo := strings.ToLower(s.Type + s.Transport)
		if strings.Contains(tipo, "sse") {
			tokens = append(tokens, "--transport", TipoSSE)
		} else {
			tokens = append(tokens, "--transport", TipoHTTP)
		}
		for _, k := range chavesOrdenadas(s.Headers) {
			tokens = append(tokens, "--header", k+": "+s.Headers[k])
		}
		return append(append(tokens, nome), destino), nil
	}

	processo := append([]string{s.Command}, s.Args...)
	if nome == "" {
		nome = nomeDoProcesso(processo)
	}
	for _, k := range chavesOrdenadas(s.Env) {
		tokens = append(tokens, "--env", k+"="+s.Env[k])
	}
	return append(append(tokens, nome, "--"), processo...), nil
}

func chavesOrdenadas(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// nomeDaURL tira o nome do host: mcp.canva.com → canva, api.githubcopilot.com
// → githubcopilot.
func nomeDaURL(bruta string) string {
	u, err := url.Parse(bruta)
	if err != nil || u.Hostname() == "" {
		return "mcp"
	}
	partes := strings.Split(u.Hostname(), ".")
	if len(partes) >= 2 {
		partes = partes[:len(partes)-1] // tira o TLD
	}
	for i := len(partes) - 1; i >= 0; i-- {
		if p := partes[i]; p != "mcp" && p != "api" && p != "www" && p != "com" && p != "" {
			return limparNome(p)
		}
	}
	return limparNome(partes[0])
}

// nomeDoProcesso tira o nome do pacote: npx -y @scope/server-github@1.2 →
// github; uvx mcp-server-fetch → fetch; docker run -i ghcr.io/x/y:tag → y.
func nomeDoProcesso(tokens []string) string {
	for _, t := range tokens[1:] {
		if strings.HasPrefix(t, "-") || t == "run" || t == "--from" || strings.Contains(t, "=") {
			continue
		}
		base := path.Base(strings.TrimPrefix(t, "@"))
		if i := strings.LastIndexAny(base, "@:"); i > 0 {
			base = base[:i]
		}
		for _, p := range []string{"mcp-server-", "server-", "mcp-"} {
			base = strings.TrimPrefix(base, p)
		}
		for _, s := range []string{"-mcp-server", "-mcp", "_mcp", ".py", ".js"} {
			base = strings.TrimSuffix(base, s)
		}
		if n := limparNome(base); n != "" {
			return n
		}
	}
	return limparNome(tokens[0])
}

func limparNome(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}
