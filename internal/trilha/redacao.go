package trilha

import (
	"regexp"
	"strings"
)

// Redigido é a marca que substitui um valor sensível. É o mesmo texto que
// cripto.Segredo já usa, de propósito: quem lê o log vê uma marca só, venha ela
// do tipo que se autorredige ou desta redação de última hora.
const Redigido = "«redigido»"

// chavesSensiveis são as chaves cujo valor sai redigido sempre, sem olhar o
// conteúdo. O casamento é por *substring* em minúscula, então "authorization"
// pega "authorization_header" e "token" pega "access_token", "refresh_token" e
// "id_token" sem precisar listá-los.
//
// "key" solto não entra: ele casaria com "api_key_id", que é um número de linha
// e é justamente o que permite achar a chave na tela. "chave" entra, e o preço
// conhecido é que um `slog` com a chave "chave" carregando o *nome* de uma chave
// de API também sai redigido — a falha na direção certa.
var chavesSensiveis = []string{
	"authorization",
	"autorizacao",
	"token",
	"secret",
	"segredo",
	"senha",
	"password",
	"chave",
	"cookie",
	"credencial",
	"bearer",
	"verifier",
	"pkce",
}

// Marcas dos segredos que o próprio patchbay emite, na forma
// <marca>_<identificador>_<segredo>: chave de API (pbk), access e refresh token
// do authorization server (pbat, pbrt), código de autorização (pbac) e segredo
// de cliente (pbcs).
//
// A redação preserva as duas primeiras partes — que são exatamente o "prefixo
// visível" que a UI mostra — e apaga a terceira. É o que a seção 11 pede: o
// valor sai redigido *com o prefixo visível*, para o admin saber de qual
// credencial o log estava falando sem que o log a entregue.
var rePatchbay = regexp.MustCompile(`\b(pb(?:k|at|rt|ac|cs)_[a-z0-9]{2,32})_[A-Za-z0-9._~+/=-]{4,}`)

// reJWT casa um JSON Web Token pelas três partes base64url. O header de um JWT
// é público, mas a assinatura e o payload não são, e um token cortado ao meio
// continua sendo material para quem coleta — a marca substitui o valor inteiro.
var reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*`)

// reEsquema casa o header de autorização inteiro, com o esquema preservado: um
// log que diz só «redigido» esconde a informação útil de que o cliente mandou
// Basic onde o servidor esperava Bearer.
var reEsquema = regexp.MustCompile(`(?i)\b(bearer|basic|dpop)\s+[A-Za-z0-9._~+/=-]{8,}`)

// reMarcaTerceiro casa os prefixos que a indústria usa como assinatura de tipo
// de credencial: GitHub (ghp_/gho_/github_pat_), Anthropic (sk-ant-), Slack
// (xoxb-/xoxp-), GitLab (glpat-) e o access token OAuth do Google (ya29.).
//
// O valor sai redigido inteiro, ao contrário de rePatchbay: aqui não há um
// "prefixo visível" a preservar — o próprio prefixo da marca já diz de que
// provedor era o segredo, e é essa a informação que vale manter.
var reMarcaTerceiro = regexp.MustCompile(
	`\b(?:ghp_|gho_|github_pat_|sk-ant-|xoxb-|xoxp-|glpat-|ya29\.)[A-Za-z0-9_.~+/=-]{4,}`)

// reChaveOpenAI casa uma chave no formato "sk-" da OpenAI, e de provedores que
// copiaram o formato. O prefixo sozinho ("sk-") é comum demais para ser
// assinatura por si só; o corpo de vinte ou mais caracteres é o que distingue
// uma chave de API de qualquer outra string que comece com essas duas letras.
var reChaveOpenAI = regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)

// reChaveAWS casa um Access Key ID da AWS: AKIA seguido de exatamente dezesseis
// caracteres alfanuméricos maiúsculos, o formato fixo que a AWS documenta.
var reChaveAWS = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)

// parametrosURLSensiveis são os parâmetros de query redigidos dentro de uma
// URL, além do que ChaveSensivel já cobre pelo nome: código de autorização e
// state de um fluxo OAuth, assinatura de webhook, e as variações mais comuns
// do nome de uma chave de API que ChaveSensivel deixa passar de propósito
// (ver o comentário de chavesSensiveis sobre "key" solto).
var parametrosURLSensiveis = []string{"api_key", "apikey", "access_key", "code", "state", "sig", "signature"}

// paramDeURLSensivel informa se o nome de um parâmetro de query pede redação
// do valor.
func paramDeURLSensivel(nome string) bool {
	if ChaveSensivel(nome) {
		return true
	}
	n := strings.ToLower(nome)
	for _, s := range parametrosURLSensiveis {
		if n == s {
			return true
		}
	}
	return false
}

// reURLComQuery casa uma URL com query string dentro de um texto qualquer:
// esquema, autoridade e caminho até o primeiro "?", e a query depois dele.
//
// Existe porque um *url.Error do net/url embute a URL inteira na mensagem —
// "Get \"https://api.x.com/mcp?api_key=SEGREDO\": context deadline exceeded" —
// e essa mensagem chega inteira a Evento.Erro. Sem isto, o segredo que ia num
// parâmetro de query (e não num header) atravessava a redação inteiro.
var reURLComQuery = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>?]+\?[^\s"'<>]*`)

// reParamDeQuery casa um parâmetro "?nome=valor" ou "&nome=valor" dentro do
// texto que reURLComQuery já isolou como sendo uma URL.
var reParamDeQuery = regexp.MustCompile(`([?&])([A-Za-z0-9_.\-]+)=([^&]*)`)

// redigirURL redige o valor de todo parâmetro de query sensível de uma URL
// dentro do texto, preservando esquema, autoridade, caminho e os demais
// parâmetros — que continuam sendo diagnóstico legítimo (qual host, qual
// rota falhou).
func redigirURL(valor string) (string, bool) {
	mudou := false
	fora := reURLComQuery.ReplaceAllStringFunc(valor, func(url string) string {
		return reParamDeQuery.ReplaceAllStringFunc(url, func(par string) string {
			m := reParamDeQuery.FindStringSubmatch(par)
			prefixo, chave, val := m[1], m[2], m[3]
			if val == "" || val == Redigido || !paramDeURLSensivel(chave) {
				return par
			}
			mudou = true
			return prefixo + chave + "=" + Redigido
		})
	})
	return fora, mudou
}

// reCampoSensivel casa "chave: valor" ou "chave=valor" dentro de um texto
// renderizado por struct ou mapa — a forma que "%+v" produz.
//
// É o complemento de ChaveSensivel para quando a chave sensível nunca passa
// pelo slog como chave de atributo: um slog.Any("cfg", struct{ Token string
// }{...}) só expõe "cfg" ao HandlerLog, e é dentro do texto renderizado que
// "Token:valor" mora.
var reCampoSensivel = regexp.MustCompile(
	`(?i)\b(\w*(?:` + strings.Join(chavesSensiveis, "|") + `)\w*)(\s*[:=]\s*)([^\s}\],]+)`)

// RedigirCampos redige o valor de todo campo "chave: valor" ou "chave=valor"
// de um texto cuja chave é sensível. Usado por HandlerLog para varrer o texto
// de um slog.Any de struct ou mapa, que a redação por chave de atributo não
// alcança (ver redigirAttr em log.go).
func RedigirCampos(texto string) (string, bool) {
	mudou := false
	fora := reCampoSensivel.ReplaceAllStringFunc(texto, func(m string) string {
		sub := reCampoSensivel.FindStringSubmatch(m)
		chave, sep, valor := sub[1], sub[2], sub[3]
		if valor == Redigido {
			return m
		}
		mudou = true
		return chave + sep + Redigido
	})
	return fora, mudou
}

// ChaveSensivel informa se a chave de um atributo de log pede redação
// incondicional do valor.
func ChaveSensivel(chave string) bool {
	c := strings.ToLower(chave)
	for _, s := range chavesSensiveis {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

// Redigir apaga de valor o que parece credencial, e informa se mudou algo.
//
// Roda mesmo quando a chave é inócua: o vazamento que mais acontece não é o
// campo chamado "token", é a mensagem de erro de um upstream que repete o header
// que ele recusou. Por isso a regra de valor é independente da regra de chave.
//
// O limite conhecido: um header estático de upstream é texto livre — nada
// impede um provedor de emitir uma credencial que não segue nenhum dos
// formatos abaixo (marca conhecida, JWT, parâmetro de URL, esquema de
// autorização). Para esse caso não há como a redação por conteúdo saber que
// aquele valor era um segredo; só a redação por chave (ChaveSensivel) continua
// valendo. Ver o mesmo limite documentado em HandlerLog (log.go) e no
// subtítulo de /admin/logs/ao-vivo (admin.templ).
func Redigir(valor string) (string, bool) {
	if valor == "" || strings.Contains(valor, Redigido) {
		return valor, false
	}
	fora := rePatchbay.ReplaceAllString(valor, "${1}_"+Redigido)
	fora = reMarcaTerceiro.ReplaceAllString(fora, Redigido)
	fora = reChaveOpenAI.ReplaceAllString(fora, Redigido)
	fora = reChaveAWS.ReplaceAllString(fora, Redigido)
	fora = reJWT.ReplaceAllString(fora, Redigido)
	if novo, mudouURL := redigirURL(fora); mudouURL {
		fora = novo
	}
	// O esquema só corre depois, e só se nada foi redigido ainda: senão
	// "Bearer pbk_abc_segredo" perderia o prefixo visível que a regra de cima
	// acabou de preservar.
	if !strings.Contains(fora, Redigido) {
		fora = reEsquema.ReplaceAllString(fora, "${1} "+Redigido)
	}
	return fora, fora != valor
}

// RedigirSensivel apaga o valor de um atributo que já se sabe sensível, sem
// olhar o conteúdo.
//
// Existe separada de RedigirPar porque a sensibilidade não vem sempre da chave
// do próprio atributo: dentro de um slog.Group("token", "valor", x), quem é
// sensível é a chave de fora, e o valor a esconder é o de dentro. Uma função que
// redecidisse pela chave recebida desredigiria exatamente esse caso.
func RedigirSensivel(valor string) (string, bool) {
	if valor == "" || strings.Contains(valor, Redigido) {
		return valor, false
	}
	// Tenta as regras de valor primeiro: elas sabem preservar o prefixo visível,
	// e um "authorization: Bearer pbk_abc_x" é mais útil redigido como
	// "Bearer pbk_abc_«redigido»" do que como "«redigido»".
	if fora, mudou := Redigir(valor); mudou {
		return fora, true
	}
	return Redigido, true
}

// RedigirPar aplica as duas regras a um par de log: chave sensível redige o
// valor inteiro (preservando o prefixo visível quando ele existe), chave comum
// passa só pela regra de valor.
func RedigirPar(chave, valor string) (string, bool) {
	if !ChaveSensivel(chave) {
		return Redigir(valor)
	}
	return RedigirSensivel(valor)
}
