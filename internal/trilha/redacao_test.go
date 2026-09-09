package trilha_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/trilha"
)

// TestRedigir cobre a regra de valor: o que parece credencial sai redigido mesmo
// quando a chave do atributo é inócua.
//
// É a regra que pega o vazamento que mais acontece — não o campo chamado
// "token", mas a mensagem de erro de um upstream que repete o header recusado.
func TestRedigir(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		entrada   string
		quer      string
		querMudou bool
	}{
		"texto comum passa intacto": {
			entrada: "upstream notion ficou pronto em 42 ms", quer: "upstream notion ficou pronto em 42 ms",
		},
		"vazio passa intacto": {entrada: "", quer: ""},
		"header bearer preserva o esquema": {
			entrada:   "recusado com Authorization: Bearer abcdefghijklmnopqrst",
			quer:      "recusado com Authorization: Bearer «redigido»",
			querMudou: true,
		},
		"basic também": {
			entrada:   "Basic dXN1YXJpbzpzZW5oYQ==",
			quer:      "Basic «redigido»",
			querMudou: true,
		},
		"jwt sai inteiro": {
			entrada:   "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			quer:      "token «redigido»",
			querMudou: true,
		},
		"chave de api mantém o prefixo visível": {
			entrada:   "pbk_a1b2c3d4_SEGREDOMUITOSECRETO",
			quer:      "pbk_a1b2c3d4_«redigido»",
			querMudou: true,
		},
		"access token do as mantém o prefixo visível": {
			entrada:   "pbat_zzzz1111_SEGREDOMUITOSECRETO",
			quer:      "pbat_zzzz1111_«redigido»",
			querMudou: true,
		},
		"bearer com chave do patchbay preserva os dois": {
			entrada:   "Bearer pbk_a1b2c3d4_SEGREDOMUITOSECRETO",
			quer:      "Bearer pbk_a1b2c3d4_«redigido»",
			querMudou: true,
		},
		"valor já redigido não é redigido de novo": {
			entrada: "senha «redigido»", quer: "senha «redigido»",
		},
		"dois segredos na mesma linha": {
			entrada:   "de pbk_aaaa1111_PRIMEIROSEGREDO para pbk_bbbb2222_SEGUNDOSEGREDO",
			quer:      "de pbk_aaaa1111_«redigido» para pbk_bbbb2222_«redigido»",
			querMudou: true,
		},
		// O corpo tem piso de quatro caracteres de propósito: abaixo disso não é
		// segredo, é um identificador com underscore, e redigi-lo trocaria ruído
		// por ruído. Este caso trava esse limite.
		"corpo curto demais não é tratado como segredo": {
			entrada: "pbk_aaaa1111_UM", quer: "pbk_aaaa1111_UM",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			got, mudou := trilha.Redigir(tc.entrada)
			if got != tc.quer {
				t.Errorf("Redigir(%q) = %q, quer %q", tc.entrada, got, tc.quer)
			}
			if mudou != tc.querMudou {
				t.Errorf("Redigir(%q) mudou = %v, quer %v", tc.entrada, mudou, tc.querMudou)
			}
		})
	}
}

// TestRedigir_URLComParametroSensivel cobre a correção da revisão: um
// *url.Error do net/url embute a URL inteira na mensagem, e um parâmetro de
// query com nome sensível vazava o segredo direto para call_log.erro, para o
// log e para o SSE.
func TestRedigir_URLComParametroSensivel(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		entrada string
		quer    string
	}{
		"um parâmetro sensível, o exemplo da revisão": {
			entrada: (&url.Error{Op: "Get", URL: "https://api.x.com/mcp?api_key=S3GR3D0",
				Err: errors.New("context deadline exceeded")}).Error(),
			quer: `Get "https://api.x.com/mcp?api_key=` + trilha.Redigido + `": context deadline exceeded`,
		},
		"vários parâmetros, só os sensíveis saem": {
			entrada: "falhou em https://exemplo.invalido/mcp?foo=bar&api_key=SEGREDO&access_key=OUTRO&normal=val",
			quer:    "falhou em https://exemplo.invalido/mcp?foo=bar&api_key=" + trilha.Redigido + "&access_key=" + trilha.Redigido + "&normal=val",
		},
		"code e state de um fluxo oauth": {
			entrada: "redirect para https://exemplo.invalido/callback?code=abc123&state=xyz789",
			quer:    "redirect para https://exemplo.invalido/callback?code=" + trilha.Redigido + "&state=" + trilha.Redigido,
		},
		"assinatura de webhook": {
			entrada: "POST https://exemplo.invalido/hook?sig=deadbeef recusado",
			quer:    "POST https://exemplo.invalido/hook?sig=" + trilha.Redigido + " recusado",
		},
		"url sem parâmetro sensível não muda": {
			entrada: "GET https://exemplo.invalido/mcp?endpoint=pessoal falhou",
			quer:    "GET https://exemplo.invalido/mcp?endpoint=pessoal falhou",
		},
		"url sem query não muda": {
			entrada: "GET https://exemplo.invalido/mcp falhou",
			quer:    "GET https://exemplo.invalido/mcp falhou",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			got, _ := trilha.Redigir(tc.entrada)
			if got != tc.quer {
				t.Errorf("trilha.Redigir(%q) = %q, quer %q", tc.entrada, got, tc.quer)
			}
		})
	}
}

// TestRedigir_MarcasDeTerceiro cobre as assinaturas de credencial de provedor
// externo que a revisão pediu: o prefixo é conhecido mesmo sem o patchbay
// conhecer o formato completo de cada provedor.
func TestRedigir_MarcasDeTerceiro(t *testing.T) {
	t.Parallel()

	casos := map[string]string{
		"github token clássico": "ghp_NAOEUMSEGREDODEVERDADE0000",
		"github oauth token":    "gho_NAOEUMSEGREDODEVERDADE0000",
		"github pat":            "github_pat_NAOEUMSEGREDODEVERDADE0000",
		"anthropic":             "sk-ant-NAOEUMSEGREDODEVERDADE0000",
		"openai":                "sk-NAOEUMSEGREDODEVERDADE00000000000",
		"slack bot token":       "xoxb-NAOEUMSEGREDODEVERDADE0000",
		"slack user token":      "xoxp-NAOEUMSEGREDODEVERDADE0000",
		"gitlab":                "glpat-NAOEUMSEGREDODEVERDADE",
		"google oauth access":   "ya29.NAOEUMSEGREDODEVERDADE0000",
		"aws access key id":     "AKIAZZZZZZZZZZZZZZZZ",
	}

	for nome, segredo := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			entrada := "upstream recusou com a credencial " + segredo
			got, mudou := trilha.Redigir(entrada)
			if !mudou {
				t.Fatalf("trilha.Redigir(%q) não mudou nada, quer a marca redigida", entrada)
			}
			if strings.Contains(got, segredo) {
				t.Errorf("trilha.Redigir(%q) = %q, ainda contém o segredo em claro", entrada, got)
			}
			if !strings.Contains(got, trilha.Redigido) {
				t.Errorf("trilha.Redigir(%q) = %q, quer conter %q", entrada, got, trilha.Redigido)
			}
		})
	}

	t.Run("chave sk- curta demais não é tratada como segredo", func(t *testing.T) {
		t.Parallel()

		entrada := "modo sk-modo-de-teste ligado"
		got, mudou := trilha.Redigir(entrada)
		if mudou || got != entrada {
			t.Errorf("trilha.Redigir(%q) = %q, mudou = %v, quer intacto", entrada, got, mudou)
		}
	})
}

// TestChaveSensivel_e_RedigirPar cobre a regra de chave: valor de chave sensível
// sai redigido sem olhar o conteúdo.
func TestChaveSensivel_e_RedigirPar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		chave       string
		valor       string
		querSensvel bool
		quer        string
	}{
		"authorization":                     {chave: "authorization", valor: "qualquer-coisa", querSensvel: true, quer: "«redigido»"},
		"access_token pega por token":       {chave: "access_token", valor: "opaco", querSensvel: true, quer: "«redigido»"},
		"refresh_token pega por token":      {chave: "refresh_token", valor: "opaco", querSensvel: true, quer: "«redigido»"},
		"client_secret pega por secret":     {chave: "client_secret", valor: "opaco", querSensvel: true, quer: "«redigido»"},
		"senha":                             {chave: "senha", valor: "1234", querSensvel: true, quer: "«redigido»"},
		"password":                          {chave: "password", valor: "1234", querSensvel: true, quer: "«redigido»"},
		"cookie":                            {chave: "cookie", valor: "sessao=abc", querSensvel: true, quer: "«redigido»"},
		"chave":                             {chave: "chave", valor: "notebook", querSensvel: true, quer: "«redigido»"},
		"pkce_verifier":                     {chave: "pkce_verifier", valor: "opaco", querSensvel: true, quer: "«redigido»"},
		"credencial":                        {chave: "credencial", valor: "opaco", querSensvel: true, quer: "«redigido»"},
		"maiúscula não escapa":              {chave: "Authorization", valor: "opaco", querSensvel: true, quer: "«redigido»"},
		"api_key_id não é sensível":         {chave: "api_key_id", valor: "17", quer: "17"},
		"endpoint não é sensível":           {chave: "endpoint", valor: "pessoal", quer: "pessoal"},
		"upstream não é sensível":           {chave: "upstream", valor: "notion", quer: "notion"},
		"duracao_ms não é sensível":         {chave: "duracao_ms", valor: "42", quer: "42"},
		"chave sensível preserva o prefixo": {chave: "token", valor: "pbat_zzzz1111_SEGREDO", querSensvel: true, quer: "pbat_zzzz1111_«redigido»"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := trilha.ChaveSensivel(tc.chave); got != tc.querSensvel {
				t.Errorf("ChaveSensivel(%q) = %v, quer %v", tc.chave, got, tc.querSensvel)
			}
			got, _ := trilha.RedigirPar(tc.chave, tc.valor)
			if got != tc.quer {
				t.Errorf("RedigirPar(%q, %q) = %q, quer %q", tc.chave, tc.valor, got, tc.quer)
			}
		})
	}
}

// TestHandlerLog_RedigeNoDestinoEnaTela é o critério da fatia: segredo nunca
// aparece em log. A assertiva é dupla de propósito — o mesmo caso é checado no
// texto que o handler de baixo escreveu (o arquivo, o stderr) e na linha que foi
// para o hub do log ao vivo. Redigir só na tela deixaria o vazamento no destino
// que ninguém revisa.
func TestHandlerLog_RedigeNoDestinoEnaTela(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		// registrar faz a chamada de log do caso.
		registrar func(*slog.Logger)
		// proibidos nunca podem aparecer, nem no destino nem na tela.
		proibidos []string
		// exigidos precisam aparecer nos dois: é o que impede que a redação
		// tenha apagado a linha inteira e "passado" por acidente.
		exigidos []string
	}{
		"atributo de chave sensível": {
			registrar: func(l *slog.Logger) {
				l.Info("upstream recusou", "upstream", "notion", "authorization", "Bearer SEGREDOABSOLUTO")
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"upstream recusou", "notion", "«redigido»"},
		},
		"segredo escorrido para a mensagem": {
			registrar: func(l *slog.Logger) {
				l.Warn("falhou com Authorization: Bearer SEGREDOABSOLUTO")
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"«redigido»"},
		},
		"segredo dentro de erro embrulhado": {
			registrar: func(l *slog.Logger) {
				l.Error("chamada falhou", "erro", errors.New("401: pbat_zzzz1111_SEGREDOABSOLUTO inválido"))
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"pbat_zzzz1111_«redigido»"},
		},
		"aninhado em slog.Group por chave de dentro": {
			registrar: func(l *slog.Logger) {
				l.Info("cabeçalhos de saída",
					slog.Group("http", "host", "api.notion.com", "authorization", "Bearer SEGREDOABSOLUTO"))
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"api.notion.com", "«redigido»"},
		},
		"aninhado em slog.Group cuja chave de fora é a sensível": {
			registrar: func(l *slog.Logger) {
				l.Info("token trocado",
					slog.Group("token", "tipo", "bearer", "valor", "SEGREDOABSOLUTO"))
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"token trocado"},
		},
		"grupo dentro de grupo": {
			registrar: func(l *slog.Logger) {
				l.Info("fluxo",
					slog.Group("oauth", slog.Group("segredo", "valor", "SEGREDOABSOLUTO")))
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"fluxo"},
		},
		"aberto por WithGroup": {
			registrar: func(l *slog.Logger) {
				l.WithGroup("saida").Info("montou requisição", "authorization", "Bearer SEGREDOABSOLUTO")
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"montou requisição", "«redigido»"},
		},
		"fixado por With": {
			registrar: func(l *slog.Logger) {
				l.With("token", "SEGREDOABSOLUTO").Info("componente subiu", "componente", "upstream")
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"componente subiu", "upstream", "«redigido»"},
		},
		"cripto.Segredo se autorredige e continua redigido": {
			registrar: func(l *slog.Logger) {
				l.Info("credencial carregada", "valor", cripto.Segredo("SEGREDOABSOLUTO"))
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			exigidos:  []string{"credencial carregada", "«redigido»"},
		},
		"linha sem segredo nenhum atravessa inteira": {
			registrar: func(l *slog.Logger) {
				l.Info("endpoint materializado", "endpoint", "pessoal", "ferramentas", 7)
			},
			exigidos: []string{"endpoint materializado", "pessoal", "7"},
		},
		"slog.Any de struct com campo sensível pelo nome": {
			registrar: func(l *slog.Logger) {
				l.Info("configuração carregada", "cfg", struct{ Nome, Token string }{
					Nome: "upstream-x", Token: "SEGREDOABSOLUTO",
				})
			},
			proibidos: []string{"SEGREDOABSOLUTO"},
			// A chave do atributo ("cfg") não é sensível; é o campo Token de
			// dentro do struct que exige a varredura de KindAny em log.go.
			exigidos: []string{"configuração carregada", "Nome:upstream-x", "Token:«redigido»"},
		},
		"slog.Any de mapa com valor no formato de marca de terceiro": {
			registrar: func(l *slog.Logger) {
				l.Info("cabeçalhos de saída", "headers", map[string]string{
					"X-Api-Key": "ghp_NAOEUMSEGREDODEVERDADE0000",
				})
			},
			proibidos: []string{"ghp_NAOEUMSEGREDODEVERDADE0000"},
			exigidos:  []string{"cabeçalhos de saída", "«redigido»"},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			var destino bytes.Buffer
			hub := trilha.NovoHub()
			mensagens, cancelar := hub.Assinar()
			t.Cleanup(cancelar)

			base := slog.NewJSONHandler(&destino, &slog.HandlerOptions{Level: slog.LevelDebug})
			log := slog.New(trilha.NovoHandlerLog(base, hub))

			tc.registrar(log)

			naTela := textoDaLinha(t, mensagens)
			noDestino := destino.String()

			for _, proibido := range tc.proibidos {
				if strings.Contains(noDestino, proibido) {
					t.Errorf("o log escrito contém %q; ele nunca pode sair do processo\n%s", proibido, noDestino)
				}
				if strings.Contains(naTela, proibido) {
					t.Errorf("a linha do log ao vivo contém %q\n%s", proibido, naTela)
				}
			}
			for _, exigido := range tc.exigidos {
				if !strings.Contains(noDestino, exigido) {
					t.Errorf("o log escrito não contém %q\n%s", exigido, noDestino)
				}
				if !strings.Contains(naTela, exigido) {
					t.Errorf("a linha do log ao vivo não contém %q\n%s", exigido, naTela)
				}
			}
		})
	}
}

// textoDaLinha lê a única mensagem publicada e a devolve como texto plano, para
// a assertiva não depender da forma do HTML.
func textoDaLinha(t *testing.T, mensagens <-chan trilha.Mensagem) string {
	t.Helper()

	var m trilha.Mensagem
	select {
	case m = <-mensagens:
	default:
		t.Fatal("nada foi publicado no hub: o handler tem de replicar toda linha")
	}
	if m.Tipo != trilha.TipoLog {
		t.Fatalf("Tipo = %q, quer %q", m.Tipo, trilha.TipoLog)
	}

	var b strings.Builder
	b.WriteString(m.Log.Nivel)
	b.WriteByte(' ')
	b.WriteString(m.Log.Mensagem)
	for _, a := range m.Log.Atributos {
		b.WriteByte(' ')
		b.WriteString(a.Chave)
		b.WriteByte('=')
		b.WriteString(a.Valor)
	}
	return b.String()
}

// TestHandlerLog_RespeitaONivelDoHandlerDeBaixo garante que embrulhar não liga o
// debug do mundo: quem decide o nível continua sendo o handler que main montou.
func TestHandlerLog_RespeitaONivelDoHandlerDeBaixo(t *testing.T) {
	t.Parallel()

	var destino bytes.Buffer
	hub := trilha.NovoHub()
	mensagens, cancelar := hub.Assinar()
	t.Cleanup(cancelar)

	base := slog.NewJSONHandler(&destino, &slog.HandlerOptions{Level: slog.LevelWarn})
	sut := trilha.NovoHandlerLog(base, hub)

	if sut.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) = true, quer false: o nível é do handler de baixo")
	}
	slog.New(sut).Info("não devia sair")

	if destino.Len() != 0 {
		t.Errorf("log escrito = %q, quer vazio", destino.String())
	}
	select {
	case m := <-mensagens:
		t.Errorf("hub recebeu %q, quer nada", m.Log.Mensagem)
	default:
	}
}

// TestHandlerLog_SemHubContinuaRedigindo: a redação não depende de haver tela
// aberta.
func TestHandlerLog_SemHubContinuaRedigindo(t *testing.T) {
	t.Parallel()

	var destino bytes.Buffer
	log := slog.New(trilha.NovoHandlerLog(
		slog.NewTextHandler(&destino, nil), nil))

	log.Info("subiu", "authorization", "Bearer SEGREDOABSOLUTO")

	if strings.Contains(destino.String(), "SEGREDOABSOLUTO") {
		t.Errorf("log = %q, quer o valor redigido", destino.String())
	}
}
