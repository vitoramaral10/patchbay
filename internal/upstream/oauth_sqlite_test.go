package upstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestForm_ValidarOAuth cobre o que a tela recusa antes de gravar.
//
// O caso que importa é o primeiro: bearer estático junto com OAuth monta dois
// Authorization, o servidor escolhe um sem dizer qual, e o sintoma é 401
// intermitente que ninguém liga a um formulário salvo semanas antes.
func TestForm_ValidarOAuth(t *testing.T) {
	t.Parallel()

	base := func() upstream.Form {
		return upstream.Form{
			Nome: "notion", Tipo: upstream.TipoHTTP, URL: "https://exemplo.com/mcp",
			TimeoutMS: upstream.TimeoutPadraoMS, Modo: upstream.ModoOAuth,
		}
	}

	casos := map[string]struct {
		ajuste    func(*upstream.Form)
		querPassa bool
		querErro  string
	}{
		"oauth sem cliente informado cai em CIMD ou DCR": {
			ajuste:    func(*upstream.Form) {},
			querPassa: true,
		},
		"oauth com cliente pré-registrado": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID = "cliente-123"
				f.OAuthSegredo = "segredo-123"
			},
			querPassa: true,
		},
		"bearer estático junto com oauth": {
			ajuste:   func(f *upstream.Form) { f.Bearer = "sk-0123" },
			querErro: "modo",
		},
		"bearer já gravado junto com oauth": {
			ajuste:   func(f *upstream.Form) { f.BearerDefinido = true },
			querErro: "modo",
		},
		"bearer gravado mas marcado para limpar": {
			ajuste: func(f *upstream.Form) {
				f.BearerDefinido, f.BearerLimpar = true, true
			},
			querPassa: true,
		},
		"stdio não usa oauth": {
			ajuste: func(f *upstream.Form) {
				f.Tipo, f.Comando, f.URL = upstream.TipoSTDIO, "npx", ""
			},
			querErro: "modo",
		},
		"client_id com quebra de linha": {
			ajuste:   func(f *upstream.Form) { f.OAuthClientID = "cli\r\nX: y" },
			querErro: "oauth_client_id",
		},
		"client_secret com caractere de controle": {
			ajuste:   func(f *upstream.Form) { f.OAuthSegredo = "seg\nredo" },
			querErro: "oauth_segredo",
		},
		"issuer que não é URL": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthIssuer = "cli", "accounts.google.com"
			},
			querErro: "oauth_issuer",
		},
		"issuer sem client_id não protege nada": {
			ajuste:   func(f *upstream.Form) { f.OAuthIssuer = "https://accounts.google.com" },
			querErro: "oauth_issuer",
		},
		"issuer com client_id": {
			ajuste: func(f *upstream.Form) {
				f.OAuthClientID, f.OAuthIssuer = "cli", "https://accounts.google.com"
			},
			querPassa: true,
		},
		"modo estático segue aceitando bearer": {
			ajuste: func(f *upstream.Form) {
				f.Modo, f.Bearer = upstream.ModoEstatica, "sk-0123"
			},
			querPassa: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			f := base()
			tc.ajuste(&f)
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

// TestRepositorio_OAuthCicloDeVida percorre o que o banco guarda de OAuth: o
// cliente informado, a concessão que o consentimento produz, a revogação e a
// troca de modo.
func TestRepositorio_OAuthCicloDeVida(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, st := repositorioDeTeste(t)

	f := upstream.Form{
		Nome: "notion", Tipo: upstream.TipoHTTP, URL: "https://exemplo.com/mcp",
		TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
		Modo: upstream.ModoOAuth, OAuthClientID: "cli-1",
		OAuthSegredo: "segredo-do-provedor", OAuthIssuer: "https://as.exemplo",
	}
	id, err := repo.Criar(ctx, f)
	if err != nil {
		t.Fatalf("criar: erro = %v, quer nil", err)
	}

	// O cliente volta em claro para o broker, e cifrado no disco.
	cliente, err := repo.ClienteOAuth(ctx, id)
	if err != nil {
		t.Fatalf("cliente OAuth: erro = %v, quer nil", err)
	}
	if cliente.ClientID != "cli-1" || cliente.Segredo.Revelar() != "segredo-do-provedor" {
		t.Errorf("cliente = %+v, quer cli-1 com o segredo em claro", cliente)
	}
	if cliente.Issuer != "https://as.exemplo" {
		t.Errorf("issuer = %q, quer https://as.exemplo", cliente.Issuer)
	}
	if v := colunaOAuthCrua(t, st, id, "client_secret_cifrado"); v == "segredo-do-provedor" {
		t.Error("client_secret gravado em claro")
	}

	// Sem consentimento não há concessão.
	if _, ok, err := repo.Concessao(ctx, id); err != nil || ok {
		t.Fatalf("concessão antes do consentimento: ok = %v, erro = %v, quer false e nil", ok, err)
	}

	// O consentimento grava a concessão.
	expira := time.Now().Add(time.Hour).Truncate(time.Second)
	refreshEm := time.Now().Add(-time.Minute).Truncate(time.Second)
	concessao := upstream.Concessao{
		ClientIDEfetivo: "cli-1", Registro: upstream.RegistroPreRegistrado,
		URLToken: "https://as.exemplo/token", Estilo: 1,
		Escopos:   []string{"leitura", "offline_access"},
		RefreshEm: refreshEm,
		Token: upstream.Token{
			Acesso: "acesso-1", Refresh: "refresh-1", Tipo: "Bearer", Expira: expira,
		},
	}
	if err := repo.GravarConcessao(ctx, id, concessao); err != nil {
		t.Fatalf("gravar concessão: erro = %v, quer nil", err)
	}

	lida, ok, err := repo.Concessao(ctx, id)
	if err != nil || !ok {
		t.Fatalf("concessão: ok = %v, erro = %v, quer true e nil", ok, err)
	}
	if lida.Token.Acesso.Revelar() != "acesso-1" || lida.Token.Refresh.Revelar() != "refresh-1" {
		t.Errorf("token lido = %+v, quer acesso-1 e refresh-1", lida.Token)
	}
	if !lida.Token.Expira.Equal(expira) {
		t.Errorf("expira = %v, quer %v", lida.Token.Expira, expira)
	}
	if len(lida.Escopos) != 2 {
		t.Errorf("escopos = %v, quer dois", lida.Escopos)
	}

	// A revogação apaga o token e mantém o que o admin configurou: obrigar a
	// colar client_id e segredo de novo por causa de uma revogação do provedor
	// seria castigo sem motivo.
	if err := repo.ApagarConcessao(ctx, id); err != nil {
		t.Fatalf("apagar concessão: erro = %v, quer nil", err)
	}
	if _, ok, _ := repo.Concessao(ctx, id); ok {
		t.Error("concessão ainda existe depois de apagada")
	}
	estado, err := repo.EstadoOAuth(ctx, id)
	if err != nil {
		t.Fatalf("estado: erro = %v, quer nil", err)
	}
	if !estado.SegredoDefinido || estado.ClientID != "cli-1" {
		t.Errorf("estado depois da revogação = %+v, quer o cliente preservado", estado)
	}
	if estado.Consentido {
		t.Error("consentido = true, quer false depois da revogação")
	}
	if estado.ClientIDEfetivo != "cli-1" || estado.Registro != upstream.RegistroPreRegistrado {
		t.Errorf("estado = %+v, quer o registro preservado para o diagnóstico", estado)
	}
	if !estado.RefreshEm.IsZero() {
		t.Errorf("último refresh = %v, quer zero depois da revogação (não há mais concessão nenhuma)",
			estado.RefreshEm)
	}

	// Trocar o client_id invalida a concessão: um token emitido para outro
	// cliente não vale para este, e mantê-lo produziria um 401 que a tela
	// descreveria como "consentido".
	if err := repo.GravarConcessao(ctx, id, concessao); err != nil {
		t.Fatalf("regravar concessão: erro = %v, quer nil", err)
	}
	f.OAuthClientID = "cli-2"
	if err := repo.Atualizar(ctx, id, f); err != nil {
		t.Fatalf("atualizar: erro = %v, quer nil", err)
	}
	if _, ok, _ := repo.Concessao(ctx, id); ok {
		t.Error("concessão sobreviveu à troca de client_id")
	}

	// Sair do modo OAuth apaga a linha inteira: guardar credencial de um fluxo
	// que já não roda é cofre de credencial alheia sem necessidade.
	if err := repo.GravarConcessao(ctx, id, concessao); err != nil {
		t.Fatalf("regravar concessão: erro = %v, quer nil", err)
	}
	f.Modo = upstream.ModoEstatica
	f.Bearer = cripto.Segredo("sk-estatico")
	if err := repo.Atualizar(ctx, id, f); err != nil {
		t.Fatalf("atualizar para estática: erro = %v, quer nil", err)
	}
	estado, err = repo.EstadoOAuth(ctx, id)
	if err != nil {
		t.Fatalf("estado: erro = %v, quer nil", err)
	}
	if estado.ClientID != "" || estado.SegredoDefinido || estado.Consentido {
		t.Errorf("estado depois de sair do OAuth = %+v, quer tudo vazio", estado)
	}
	reg, err := repo.Obter(ctx, id)
	if err != nil {
		t.Fatalf("obter: erro = %v, quer nil", err)
	}
	if reg.ModoEfetivo() != upstream.ModoEstatica {
		t.Errorf("modo = %q, quer %q", reg.ModoEfetivo(), upstream.ModoEstatica)
	}
}

// TestRepositorio_ConcessaoNaoAutenticaEmOutraColuna prova o AAD por coluna: um
// access token transplantado para a coluna de refresh não decifra, mesmo com a
// chave mestra certa.
//
// Sem a coluna no dado autenticado, quem tem escrita no banco e não tem a chave
// conseguiria trocar os dois valores de lugar.
func TestRepositorio_ConcessaoNaoAutenticaEmOutraColuna(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, st := repositorioDeTeste(t)

	id, err := repo.Criar(ctx, upstream.Form{
		Nome: "notion", Tipo: upstream.TipoHTTP, URL: "https://exemplo.com/mcp",
		TimeoutMS: upstream.TimeoutPadraoMS, Modo: upstream.ModoOAuth,
	})
	if err != nil {
		t.Fatalf("criar: erro = %v, quer nil", err)
	}
	if err := repo.GravarConcessao(ctx, id, upstream.Concessao{
		ClientIDEfetivo: "cli", Registro: upstream.RegistroDCR,
		URLToken: "https://as.exemplo/token",
		Token:    upstream.Token{Acesso: "acesso-1", Refresh: "refresh-1"},
	}); err != nil {
		t.Fatalf("gravar concessão: erro = %v, quer nil", err)
	}

	// Transplante: o valor do access vai para a coluna do refresh.
	acesso := colunaOAuthCrua(t, st, id, "access_token_cifrado")
	if _, err := st.Escrita().ExecContext(ctx,
		`UPDATE upstream_oauth SET refresh_token_cifrado = ? WHERE upstream_id = ?`,
		acesso, id); err != nil {
		t.Fatalf("transplantar: erro = %v, quer nil", err)
	}

	if _, _, err := repo.Concessao(ctx, id); err == nil {
		t.Fatal("erro = nil, quer recusa de autenticação do valor transplantado")
	}
}

// colunaOAuthCrua lê uma coluna de upstream_oauth direto do SQLite, sem passar
// pelo repositório: é o único jeito de provar que o que está gravado não é o
// valor em claro.
func colunaOAuthCrua(t *testing.T, st *store.Store, upstreamID int64, coluna string) string {
	t.Helper()

	var valor string
	err := st.Leitura().QueryRowContext(context.Background(),
		`SELECT `+coluna+` FROM upstream_oauth WHERE upstream_id = ?`, upstreamID).Scan(&valor)
	if err != nil {
		t.Fatalf("ler coluna crua %s: erro = %v, quer nil", coluna, err)
	}
	return valor
}
