package webui

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
)

// Redirecionar manda o navegador para destino depois de uma ação.
//
// 303 e não 302: o método da próxima requisição precisa virar GET, senão o
// recarregar da página reenvia o POST. Quando a requisição veio do htmx, o
// redirecionamento vai no header HX-Redirect — htmx segue um 303 dentro do
// próprio XHR e trocaria o fragmento, não a página.
func Redirecionar(w http.ResponseWriter, r *http.Request, destino string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", destino)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, destino, http.StatusSeeOther)
}

// Renderizar escreve um componente templ como resposta HTML.
//
// O status vai antes do corpo porque templ escreve direto no ResponseWriter: se
// a renderização falhar no meio, o cabeçalho já foi enviado e não há o que
// corrigir — só logar.
func Renderizar(w http.ResponseWriter, r *http.Request, status int, log *slog.Logger, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A UI não tem conteúdo cacheável: toda tela reflete estado mutável.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		log.Error("falha ao renderizar tela", "caminho", r.URL.Path, "erro", err)
	}
}

// ErroInterno responde 500 com texto genérico e deixa o detalhe no log.
//
// Nunca devolve err.Error() no corpo: a mensagem carrega query, caminho de
// arquivo e nome de host.
func ErroInterno(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	log.Error("falha ao atender rota de administração",
		"caminho", r.URL.Path, "metodo", r.Method, "erro", err)
	http.Error(w, "Não foi possível concluir a operação. Tente de novo em alguns instantes.",
		http.StatusInternalServerError)
}

// Avisos traduz o código de ?aviso= no alerta que a tela mostra.
//
// O resultado da ação viaja na URL, e não em cookie de flash nem em sessão: uma
// tabela de mensagens a mais no SQLite para dizer "criado com sucesso" é estado
// mutável sem motivo, e o escritor do banco é único.
func Avisos(r *http.Request, mensagens map[string]Alerta) *Alerta {
	codigo := r.URL.Query().Get("aviso")
	if codigo == "" {
		return nil
	}
	a, ok := mensagens[codigo]
	if !ok {
		return nil
	}
	return &a
}

type chaveUsuario struct{}

// ComUsuario grava no contexto quem está autenticado, para o shell mostrar no
// canto e oferecer o "sair".
//
// Fica aqui, no layout, e não na feature de admin: as telas de upstream, endpoint
// e chave precisam do nome para desenhar a barra, e nenhuma delas pode importar a
// feature de admin — feature não importa feature.
func ComUsuario(ctx context.Context, usuario string) context.Context {
	return context.WithValue(ctx, chaveUsuario{}, usuario)
}

// UsuarioDoContexto devolve o usuário autenticado, ou vazio.
func UsuarioDoContexto(ctx context.Context) string {
	u, _ := ctx.Value(chaveUsuario{}).(string)
	return u
}
