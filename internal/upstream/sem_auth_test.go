package upstream_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestForm_ModoNenhum cobre a opção que faltava na tela: cadastrar um MCP que
// não pede credencial nenhuma.
func TestForm_ModoNenhum(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		ajuste    func(*upstream.Form)
		querModo  string
		querPassa bool
		querErro  string
	}{
		"http sem autenticação": {
			ajuste:    func(f *upstream.Form) { f.Modo = upstream.ModoNenhum },
			querModo:  upstream.ModoNenhum,
			querPassa: true,
		},
		"sse sem autenticação": {
			ajuste: func(f *upstream.Form) {
				f.Tipo, f.Modo = upstream.TipoSSE, upstream.ModoNenhum
			},
			querModo:  upstream.ModoNenhum,
			querPassa: true,
		},
		"modo vazio continua estático": {
			ajuste:    func(f *upstream.Form) {},
			querModo:  upstream.ModoEstatica,
			querPassa: true,
		},
		"stdio não tem modo de credencial": {
			ajuste: func(f *upstream.Form) {
				f.Tipo, f.Comando, f.URL = upstream.TipoSTDIO, "npx", ""
				f.Modo = upstream.ModoNenhum
			},
			querModo: upstream.ModoEstatica,
			querErro: "modo",
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			f := upstream.Form{
				Nome: "aberto", Tipo: upstream.TipoHTTP, URL: "https://exemplo.com/mcp",
				TimeoutMS: upstream.TimeoutPadraoMS,
			}
			tc.ajuste(&f)

			if got := f.ModoEfetivo(); got != tc.querModo {
				t.Errorf("ModoEfetivo = %q, quer %q", got, tc.querModo)
			}
			passou := f.Validar()
			if passou != tc.querPassa {
				t.Fatalf("passou = %v, quer %v (erros = %v)", passou, tc.querPassa, f.Erros)
			}
			if tc.querErro != "" && f.Erros[tc.querErro] == "" {
				t.Errorf("erros = %v, quer a chave %q", f.Erros, tc.querErro)
			}
		})
	}
}

// TestRepositorio_ModoNenhumApagaOQueIriaNoHeader prova que "sem autenticação" é
// uma declaração que vale no banco, e não só uma etiqueta na tela: o bearer e os
// headers do modo anterior saem na mesma transação que grava o modo.
func TestRepositorio_ModoNenhumApagaOQueIriaNoHeader(t *testing.T) {
	t.Parallel()

	sut, _ := repositorioDeTeste(t)
	ctx := context.Background()

	f := formBase("interno")
	f.Bearer = "sk-0123456789"
	f.Headers = []upstream.CampoHeader{{Nome: "X-Api-Key", Valor: "chave"}}
	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	aberto := formBase("interno")
	aberto.Modo = upstream.ModoNenhum
	aberto.BearerDefinido = true
	if err := sut.Atualizar(ctx, id, aberto); err != nil {
		t.Fatalf("Atualizar: erro = %v, quer nil", err)
	}

	creds, err := sut.Credenciais(ctx, id)
	if err != nil {
		t.Fatalf("Credenciais: erro = %v, quer nil", err)
	}
	if len(creds) != 0 {
		t.Errorf("credenciais = %d, quer 0 depois de salvar sem autenticação", len(creds))
	}

	reg, err := sut.Obter(ctx, id)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if !reg.SemAutenticacao() {
		t.Errorf("modo gravado = %q, quer %q", reg.ModoEfetivo(), upstream.ModoNenhum)
	}
}

// TestGerente_ModoNenhumNaoMandaAuthorization é o requisito visto do lado do
// servidor: um MCP declarado aberto não recebe header de credencial, nem mesmo
// quando sobrou linha gravada no banco — gravação direta, corrida entre abas.
func TestGerente_ModoNenhumNaoMandaAuthorization(t *testing.T) {
	t.Parallel()

	alvo, espia := servidorEspiao(t)

	cfg := upstream.Config{
		ID: 7, Nome: "aberto", Tipo: upstream.TipoHTTP,
		URL: alvo, Timeout: 5 * time.Second, Modo: upstream.ModoNenhum,
	}
	if !cfg.SemAutenticacao() {
		t.Fatalf("SemAutenticacao = false, quer true para %q", cfg.Modo)
	}

	mudou := make(chan struct{}, 64)
	sut := upstream.NovoGerente(slog.New(slog.DiscardHandler), []upstream.Config{cfg},
		upstream.ComIntervaloTentativa(20*time.Millisecond),
		upstream.AoMudar(func(context.Context) {
			select {
			case mudou <- struct{}{}:
			default:
			}
		}),
		upstream.ComCredenciais(func(context.Context, int64) ([]upstream.Credencial, error) {
			t.Error("credenciais lidas para um MCP declarado sem autenticação")
			return []upstream.Credencial{
				{Tipo: upstream.CredencialBearer, Valor: "sk-sobrevivente"},
			}, nil
		}),
	)

	ctx, cancelar := context.WithCancel(context.Background())
	sut.Iniciar(ctx)
	t.Cleanup(func() {
		cancelar()
		sut.Aguardar()
	})
	esperarPronto(t, sut, cfg.ID, mudou)

	recebido := espia.headers()
	if recebido == nil {
		t.Fatal("nenhuma requisição chegou ao upstream")
	}
	if v := recebido.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, quer vazio", v)
	}
}

// TestTelaForm_OfereceSemAutenticacao é o bug que abriu esta fatia: a opção não
// existia na tela, e quem chegava num MCP aberto procurava o que preencher. O
// seletor tem de aparecer inclusive sem broker de OAuth.
func TestTelaForm_OfereceSemAutenticacao(t *testing.T) {
	t.Parallel()

	for nome, disponivel := range map[string]bool{
		"com broker de OAuth": true,
		"sem broker de OAuth": false,
	} {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			f := upstream.Form{
				Nome: "aberto", Tipo: upstream.TipoHTTP, URL: "https://exemplo.com/mcp",
				TimeoutMS: upstream.TimeoutPadraoMS, OAuthDisponivel: disponivel,
			}
			var sb strings.Builder
			if err := upstream.TelaForm(f).Render(context.Background(), &sb); err != nil {
				t.Fatalf("render: erro = %v, quer nil", err)
			}
			html := sb.String()

			if !strings.Contains(html, `value="`+upstream.ModoNenhum+`"`) {
				t.Error("a tela não oferece a opção sem autenticação")
			}
			if !strings.Contains(html, "Sem autenticação") {
				t.Error("a tela não nomeia a opção sem autenticação")
			}
			if temOAuth := strings.Contains(html, `value="`+upstream.ModoOAuth+`"`); temOAuth != disponivel {
				t.Errorf("opção de OAuth na tela = %v, quer %v", temOAuth, disponivel)
			}
		})
	}
}
