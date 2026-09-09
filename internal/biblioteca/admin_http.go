package biblioteca

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP da biblioteca.
//
// Só lê: não há POST nenhum aqui. Quem cria upstream é o formulário de
// upstream, e a biblioteca só monta o link que chega lá preenchido — o que
// também é o que mantém a regra de arquitetura de pé, já que uma feature não
// importa outra.
type Admin struct {
	catalogo *Catalogo
	log      *slog.Logger
}

// NovoAdmin monta a borda sobre um catálogo já carregado.
func NovoAdmin(catalogo *Catalogo, log *slog.Logger) *Admin {
	return &Admin{catalogo: catalogo, log: log}
}

// Rotas registra a tela. Exige sessão de admin, como as demais telas do painel.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaBiblioteca, a.listar)
}

// listar mostra o catálogo, filtrado pelo ?q=.
//
// O termo vive na URL e não em sessão: uma busca é linkável e sobrevive ao
// recarregar, que é a mesma razão pela qual a trilha filtra por GET. O htmx só
// acelera o que já funciona sem ele — com JavaScript desligado, o formulário
// continua sendo um GET comum que recarrega a página inteira.
func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	termo := strings.TrimSpace(r.URL.Query().Get("q"))
	achados := a.catalogo.Buscar(termo)

	// Requisição do htmx recebe só a lista. A página inteira dentro do alvo
	// aninharia um <html> dentro do <body> a cada tecla digitada.
	if r.Header.Get("HX-Request") == "true" {
		webui.Renderizar(w, r, http.StatusOK, a.log, Resultados(achados, termo, a.catalogo.Tamanho()))
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaBiblioteca(Pagina{
		Itens: achados,
		Termo: termo,
		Total: a.catalogo.Tamanho(),
	}))
}

// Pagina é o que a tela precisa saber.
type Pagina struct {
	// Itens é o recorte já filtrado.
	Itens []Item
	// Termo é o que está na caixa de busca.
	Termo string
	// Total é o tamanho do catálogo inteiro, para a tela dizer "12 de 293" sem
	// recontar.
	Total int
}
