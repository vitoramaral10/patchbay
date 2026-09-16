package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A descoberta de "este MCP fala OAuth", para o cadastro que chega por comando
// colado.
//
// O formulário tem o campo de modo e o catálogo tem a autenticação declarada
// (biblioteca.Item.ModoDeCredencial), mas o comando de instalação não tem nem
// um nem outro: `npx add-mcp https://algum/mcp` não diz uma palavra sobre
// autenticação, e até 2026-09-16 todo MCP remoto colado nascia em modo
// estática — o padrão de Form.ModoEfetivo. Para um servidor OAuth isso é um
// cadastro errado que só se manifesta depois, como 401 em laço sem nenhum
// botão de Autorizar na tela, porque o modo estática nunca chega ao fluxo de
// consentimento.

// PrazoDeDeteccao é o teto da descoberta inteira, incluindo as duas tentativas.
//
// Curto de propósito: ela roda dentro do POST que cria o MCP, e o colar é de um
// passo só desde 2026-09-16. Estourar o prazo não é erro — é o cadastro sair em
// modo estática, exatamente como saía antes desta detecção existir.
const PrazoDeDeteccao = 4 * time.Second

// limiteDoMetadado é quanto do documento o detector lê. O documento do RFC 9728
// tem algumas centenas de bytes; o teto existe porque a URL é de terceiro, e
// corpo sem fim do outro lado não pode virar memória daqui.
const limiteDoMetadado = 64 << 10

// DetectorDeOAuth responde se a URL de um MCP remoto se declara recurso
// protegido por OAuth.
//
// Tipo próprio, e não um método, para o teste poder responder sem rede: o
// caminho do colar é HTTP de ponta a ponta, e sem isto todo teste dele
// dependeria de um servidor de metadados de verdade.
type DetectorDeOAuth func(ctx context.Context, urlDoMCP string) bool

// novoDetectorPorMetadados lê o Protected Resource Metadata do RFC 9728.
//
// Metadado publicado e não o desafio do 401: as duas coisas são evidência, mas
// só esta se obtém com um GET sem corpo e sem efeito nenhum do outro lado. O
// 401 exigiria falar MCP — montar um initialize de verdade, com o transporte
// certo para HTTP e para SSE — dentro de um handler cuja única pergunta é "que
// modo gravar".
//
// O preço é conhecido: servidor que só anuncia OAuth pelo WWW-Authenticate,
// sem publicar o documento, continua nascendo em modo estática. Esse caso se
// conserta na tela, trocando o modo; o contrário — adivinhar OAuth de um
// servidor que só quer um bearer estático — produziria um cadastro que o admin
// não consegue completar.
func novoDetectorPorMetadados(cliente *http.Client) DetectorDeOAuth {
	return func(ctx context.Context, urlDoMCP string) bool {
		for _, candidata := range metadadosProtegidosDe(urlDoMCP) {
			if publicaAuthorizationServer(ctx, cliente, candidata) {
				return true
			}
		}
		return false
	}
}

// metadadosProtegidosDe monta as URLs do documento, na ordem em que o RFC 9728
// §3.1 manda tentar.
//
// O caminho do recurso entra depois do .well-known, e não antes: para
// https://host/mcp o documento é https://host/.well-known/oauth-protected-resource/mcp.
// A raiz vem como segunda tentativa porque servidor que serve o MCP em /mcp mas
// publica o documento na raiz existe — a Canva é um — e uma tentativa a mais
// custa um GET dentro do mesmo prazo.
func metadadosProtegidosDe(bruta string) []string {
	u, err := url.Parse(strings.TrimSpace(bruta))
	if err != nil || u.Host == "" {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil
	}
	raiz := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	caminho := strings.TrimSuffix(u.EscapedPath(), "/")
	if caminho == "" {
		return []string{raiz}
	}
	return []string{raiz + caminho, raiz}
}

// publicaAuthorizationServer diz se o endereço serve um documento de recurso
// protegido utilizável.
//
// A exigência é authorization_servers não vazio, e não apenas o 200: um
// servidor que responde a qualquer caminho com uma página de HTML — ou com o
// próprio index — passaria por qualquer teste mais frouxo, e o resultado seria
// um MCP cadastrado em OAuth sem ter para onde mandar o admin consentir.
func publicaAuthorizationServer(ctx context.Context, cliente *http.Client, endereco string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endereco, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")

	res, err := cliente.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, limiteDoMetadado))
		_ = res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		return false
	}

	var doc struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, limiteDoMetadado)).Decode(&doc); err != nil {
		return false
	}
	return len(doc.AuthorizationServers) > 0
}

// avisoModoOAuthDetectado é o que a tela do MCP recém-criado diz sobre a
// decisão. Vai pela guarda de notas, junto com os outros ajustes do comando:
// modo escolhido sem o admin digitar precisa aparecer para ele, senão a
// detecção vira mágica que ninguém consegue auditar depois.
const avisoModoOAuthDetectado = "O servidor publica metadados de recurso protegido (RFC 9728), " +
	"então cadastrei em modo OAuth em vez de credencial estática. " +
	"Clique em Autorizar para consentir."

// ajustarModoPorDescoberta grava o modo oauth quando o servidor se declara
// recurso protegido, e devolve o aviso para a tela. Devolve vazio quando não
// mexeu em nada.
//
// As guardas dizem quando ela se cala, e cada uma tem motivo:
//
//   - Sem broker de OAuth, o modo oauth não existe nesta instalação: gravá-lo
//     produziria um upstream cujo botão Autorizar responde que a autorização
//     está indisponível.
//   - Modo já escolhido é escolha de quem cadastrou, e não palpite a corrigir.
//   - STDIO não tem modo de credencial nenhum.
//   - Bearer ou header vindos no comando são credencial estática explícita.
//     Trocar para oauth ali não só ignoraria o que o admin colou: validarOAuth
//     recusa as duas coisas juntas, e o cadastro morreria na validação com uma
//     mensagem sobre um modo que ninguém pediu.
func (a *Admin) ajustarModoPorDescoberta(ctx context.Context, f *Form) string {
	if a.oauth == nil || a.detectarOAuth == nil {
		return ""
	}
	if f.STDIO() || f.Modo != "" {
		return ""
	}
	if !f.Bearer.Vazio() || len(f.Headers) > 0 {
		return ""
	}

	ctxDeteccao, cancelar := context.WithTimeout(ctx, PrazoDeDeteccao)
	defer cancelar()
	if !a.detectarOAuth(ctxDeteccao, f.URL) {
		return ""
	}

	f.Modo = ModoOAuth
	return avisoModoOAuthDetectado
}
