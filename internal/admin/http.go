package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"log/slog"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// NomeCookie é o cookie de sessão do admin.
const NomeCookie = "patchbay_sessao_admin"

// HTTP serve o setup do primeiro acesso, o login, o logout e o portão que
// protege as demais rotas de administração.
type HTTP struct {
	s      *Servico
	log    *slog.Logger
	seguro bool
}

// NovoHTTP monta a borda HTTP do admin.
//
// seguro liga o atributo Secure do cookie e é derivado da URL pública: o TLS é
// terminado por proxy reverso na frente do patchbay (decisão de 2026-09-08), e
// por isso quem sabe se a conexão do navegador é https é a configuração, não o
// r.TLS da requisição.
func NovoHTTP(s *Servico, urlPublica string, log *slog.Logger) *HTTP {
	return &HTTP{s: s, log: log, seguro: strings.HasPrefix(urlPublica, "https://")}
}

// Rotas registra o que não exige sessão: setup, login e logout.
func (h *HTTP) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaSetup, h.setupForm)
	mux.HandleFunc("POST "+webui.RotaSetup, h.setupGravar)
	mux.HandleFunc("GET "+webui.RotaLogin, h.loginForm)
	mux.HandleFunc("POST "+webui.RotaLogin, h.loginEntrar)
	mux.HandleFunc("POST "+webui.RotaSair, h.sair)
}

type chaveContexto struct{}

// DoContexto devolve o admin da sessão que Proteger colocou no contexto.
func DoContexto(ctx context.Context) (Admin, bool) {
	a, ok := ctx.Value(chaveContexto{}).(Admin)
	return a, ok
}

// Proteger é o portão de toda rota de UI.
//
// Sem admin cadastrado, tudo vai para o setup: é o primeiro acesso, e uma tela
// de login para um usuário que não existe é um beco. Com admin e sem sessão, vai
// para o login carregando o destino, para que o clique original não se perca.
func (h *HTTP) Proteger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		existe, err := h.s.Existe(r.Context())
		if err != nil {
			webui.ErroInterno(w, r, h.log, err)
			return
		}
		if !existe {
			webui.Redirecionar(w, r, webui.RotaSetup)
			return
		}

		sessao, err := h.s.Sessao(r.Context(), h.tokenDaRequisicao(r))
		switch {
		case errors.Is(err, ErrSessao):
			h.expirarCookie(w)
			webui.Redirecionar(w, r, webui.RotaLogin+"?destino="+url.QueryEscape(h.destinoDeVolta(r)))
			return
		case err != nil:
			webui.ErroInterno(w, r, h.log, err)
			return
		}

		ctx := context.WithValue(r.Context(), chaveContexto{}, sessao.Admin)
		// O nome também vai para o contexto do layout: as telas de upstream,
		// endpoint e chave desenham a barra com ele e nenhuma delas pode importar
		// esta feature.
		ctx = webui.ComUsuario(ctx, sessao.Admin.Usuario)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// destinoDeVolta é o caminho que a tela de login guarda para voltar depois do
// login.
//
// A query viaja junto no caso geral — é o que devolve o admin à página exata
// que ele pediu, paginação inclusa. O callback OAuth de upstream é a exceção:
// a query dele é code e state de um authorization server, e gravá-la em
// ?destino= poria um code de autorização — de uso único e às vezes de vida
// curta — dentro do formulário de login, visível na URL e reenviado ao
// provedor depois do login, quando ele já não vale nada ou já foi consumido.
// Só o caminho garante que Entregar recuse esse callback tardio como state
// desconhecido, em vez de reagir a um code requentado.
func (h *HTTP) destinoDeVolta(r *http.Request) string {
	if r.URL.Path == webui.RotaCallbackOAuthUpstream {
		return r.URL.Path
	}
	return r.URL.RequestURI()
}

func (h *HTTP) tokenDaRequisicao(r *http.Request) string {
	c, err := r.Cookie(NomeCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// --- setup do primeiro acesso ---

func (h *HTTP) setupForm(w http.ResponseWriter, r *http.Request) {
	existe, err := h.s.Existe(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, h.log, err)
		return
	}
	if existe {
		// A rota deixa de existir depois do primeiro acesso: com admin
		// cadastrado, /admin/setup é só o caminho do login.
		webui.Redirecionar(w, r, webui.RotaLogin)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, h.log, TelaSetup(FormSetup{}))
}

func (h *HTTP) setupGravar(w http.ResponseWriter, r *http.Request) {
	existe, err := h.s.Existe(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, h.log, err)
		return
	}
	if existe {
		webui.Redirecionar(w, r, webui.RotaLogin)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}

	form := FormSetup{
		Usuario:   strings.TrimSpace(r.PostFormValue("usuario")),
		Senha:     r.PostFormValue("senha"),
		Confirmar: r.PostFormValue("confirmar"),
	}
	if form.Senha != form.Confirmar {
		form.ErroSenha = "As duas senhas não são iguais."
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, h.log, TelaSetup(form))
		return
	}

	admin, err := h.s.Criar(r.Context(), form.Usuario, form.Senha)
	switch {
	case errors.Is(err, ErrUsuarioVazio):
		form.ErroUsuario = "Escolha um nome de usuário."
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, h.log, TelaSetup(form))
		return
	case errors.Is(err, ErrSenhaCurta):
		form.ErroSenha = "A senha precisa de pelo menos 12 caracteres."
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, h.log, TelaSetup(form))
		return
	case errors.Is(err, ErrJaExiste):
		webui.Redirecionar(w, r, webui.RotaLogin)
		return
	case err != nil:
		webui.ErroInterno(w, r, h.log, err)
		return
	}

	// Entra já autenticado: pedir a senha que a pessoa acabou de escolher é
	// atrito sem ganho de segurança nenhum.
	if err := h.abrirSessao(w, r, admin); err != nil {
		webui.ErroInterno(w, r, h.log, err)
		return
	}
	webui.Redirecionar(w, r, webui.RotaPainel+"?aviso=setup")
}

// --- login e logout ---

func (h *HTTP) loginForm(w http.ResponseWriter, r *http.Request) {
	existe, err := h.s.Existe(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, h.log, err)
		return
	}
	if !existe {
		webui.Redirecionar(w, r, webui.RotaSetup)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, h.log, TelaLogin(FormLogin{
		Destino: destinoSeguro(r.URL.Query().Get("destino")),
		Expirou: r.URL.Query().Get("aviso") == "expirou",
	}))
}

func (h *HTTP) loginEntrar(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulário inválido", http.StatusBadRequest)
		return
	}
	form := FormLogin{
		Usuario: strings.TrimSpace(r.PostFormValue("usuario")),
		Destino: destinoSeguro(r.PostFormValue("destino")),
	}

	admin, err := h.s.Autenticar(r.Context(), form.Usuario, r.PostFormValue("senha"))
	switch {
	case errors.Is(err, ErrCredencial):
		// O log registra a tentativa sem a senha e sem dizer qual metade errou.
		h.log.Warn("tentativa de login recusada", "usuario", form.Usuario)
		form.Erro = "Usuário ou senha inválidos."
		webui.Renderizar(w, r, http.StatusUnauthorized, h.log, TelaLogin(form))
		return
	case err != nil:
		webui.ErroInterno(w, r, h.log, err)
		return
	}

	if err := h.abrirSessao(w, r, admin); err != nil {
		webui.ErroInterno(w, r, h.log, err)
		return
	}
	h.log.Info("login de admin", "usuario", admin.Usuario)
	webui.Redirecionar(w, r, form.destinoOuPainel())
}

func (h *HTTP) sair(w http.ResponseWriter, r *http.Request) {
	if err := h.s.Encerrar(r.Context(), h.tokenDaRequisicao(r)); err != nil {
		// Não impede o logout: o cookie sai de todo jeito, e sessão órfã no
		// banco morre na limpeza por expiração.
		h.log.Warn("não apagou a sessão do banco no logout", "erro", err)
	}
	h.expirarCookie(w)
	webui.Redirecionar(w, r, webui.RotaLogin)
}

func (h *HTTP) abrirSessao(w http.ResponseWriter, r *http.Request, a Admin) error {
	token, expiraEm, err := h.s.AbrirSessao(r.Context(), a.ID)
	if err != nil {
		return err
	}
	// G124 pede Secure sempre. Aqui ele vem da URL pública porque o TLS é
	// terminado por proxy reverso na frente do patchbay (seção 12 do estudo):
	// forçar Secure quebraria o acesso por http em loopback, que é o modo de
	// desenvolvimento e o primeiro acesso de toda instalação.
	//nolint:gosec // Secure derivado da URL pública; ver o comentário acima
	http.SetCookie(w, &http.Cookie{
		Name:  NomeCookie,
		Value: token,
		Path:  "/",
		// HttpOnly: nenhum JS da UI precisa do token, e um XSS não deve poder
		// exfiltrá-lo. SameSite=Lax: o cookie não viaja em POST cross-site, que
		// é a segunda barreira além da proteção de Origin do net/http.
		HttpOnly: true,
		Secure:   h.seguro,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiraEm,
	})
	return nil
}

func (h *HTTP) expirarCookie(w http.ResponseWriter) {
	//nolint:gosec // mesmo motivo de abrirSessao: Secure vem da URL pública
	http.SetCookie(w, &http.Cookie{
		Name:     NomeCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.seguro,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// destinoSeguro só aceita caminho absoluto do próprio patchbay.
//
// Sem isso, ?destino=https://exemplo.invalido transforma a tela de login num
// redirecionador aberto — o vetor clássico de phishing em cima de um painel
// legítimo.
func destinoSeguro(destino string) string {
	if destino == "" || !strings.HasPrefix(destino, "/") || strings.HasPrefix(destino, "//") {
		return ""
	}
	u, err := url.Parse(destino)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	return u.RequestURI()
}
