package biblioteca

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// Admin é a borda HTTP da biblioteca.
//
// Não escreve nada: não há POST aqui, não há tabela, não há arquivo. Quem cria
// upstream é o formulário de upstream — a biblioteca só monta o link que chega
// lá preenchido, e é isso que também mantém de pé a regra de arquitetura de uma
// feature não importar outra.
type Admin struct {
	origem *Origem
	log    *slog.Logger
}

// NovoAdmin monta a borda sobre um cliente da origem.
func NovoAdmin(origem *Origem, log *slog.Logger) *Admin {
	return &Admin{origem: origem, log: log}
}

// Rotas registra as telas. Exigem sessão de admin, como o resto do painel.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaBiblioteca, a.listar)
	mux.HandleFunc("GET "+webui.RotaBiblioteca+"/{slug}/adicionar", a.adicionar)
}

// listar mostra o que a origem publica agora, filtrado pelo ?q=.
//
// O termo vive na URL e não em sessão: uma busca é linkável e sobrevive ao
// recarregar, que é a mesma razão pela qual a trilha filtra por GET. O htmx só
// acelera o que já funciona sem ele — com JavaScript desligado, o formulário
// continua sendo um GET comum que recarrega a página inteira.
func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	termo := strings.TrimSpace(r.URL.Query().Get("q"))
	htmx := r.Header.Get("HX-Request") == "true"

	todos, err := a.origem.Listar(r.Context())
	if err != nil {
		// Origem fora não é erro do patchbay: 200 com a explicação na tela, e não
		// um 502 que o admin lê como "o gateway quebrou". O log fica com o
		// motivo exato.
		a.log.Warn("não foi possível ler o catálogo da origem",
			"origem", a.origem.base, "erro", err)
		falha := falhaDe(err)
		if htmx {
			webui.Renderizar(w, r, http.StatusOK, a.log, Resultados(nil, termo, 0, falha))
			return
		}
		webui.Renderizar(w, r, http.StatusOK, a.log, TelaBiblioteca(Pagina{
			Termo:  termo,
			Falha:  falha,
			Alerta: webui.Avisos(r, avisos),
		}))
		return
	}

	achados := Buscar(todos, termo)
	// Requisição do htmx recebe só a lista. A página inteira dentro do alvo
	// aninharia um <html> dentro do <body> a cada tecla digitada.
	if htmx {
		webui.Renderizar(w, r, http.StatusOK, a.log, Resultados(achados, termo, len(todos), ""))
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaBiblioteca(Pagina{
		Itens:  achados,
		Termo:  termo,
		Total:  len(todos),
		Alerta: webui.Avisos(r, avisos),
	}))
}

// adicionar busca a página do servidor escolhido e manda o admin para o
// formulário de upstream preenchido.
//
// A ida à origem acontece aqui, e não ao montar a lista, porque é uma por
// clique em vez de uma por servidor listado: a página de listagem não traz URL
// nem transporte, e buscá-los para os 293 só para desenhar a tela seriam 293
// requisições ao site a cada abertura.
//
// GET e não POST: nada muda no patchbay: busca-se uma página e redireciona-se.
// O que muda estado é o formulário do outro lado, que o admin ainda vai revisar
// e salvar.
func (a *Admin) adicionar(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	detalhe, err := a.origem.Detalhe(r.Context(), slug)
	if err != nil {
		a.log.Warn("não foi possível ler o servidor na origem",
			"slug", slug, "origem", a.origem.base, "erro", err)
		webui.Redirecionar(w, r, webui.RotaBiblioteca+"?aviso="+avisoDe(err))
		return
	}
	webui.Redirecionar(w, r, rotaDeCadastro(detalhe))
}

// rotaDeCadastro é o formulário de upstream novo, já preenchido com o servidor
// escolhido.
//
// Os nomes dos parâmetros são contrato com internal/upstream, e a regra de
// arquitetura impede importar aquele pacote para pegá-los de uma constante
// compartilhada. Quem guarda o contrato é
// TestAdicionarDaBibliotecaAbreFormularioPreenchido, em cmd/patchbay: ele segue
// este caminho e confere que o formulário volta preenchido.
//
// Nenhum campo de credencial entra aqui: query vaza para histórico do
// navegador, log de proxy e Referer.
func rotaDeCadastro(d Detalhe) string {
	q := url.Values{
		"tipo": {d.Transporte},
		"nome": {d.Nome},
		"url":  {d.URL},
		"modo": {d.ModoDeCredencial()},
	}
	return webui.RotaUpstreams + "/novo?" + q.Encode()
}

// Falha é o que a tela diz quando a origem não respondeu como devia. Vazio é
// tudo certo.
type Falha string

// As duas falhas que a tela distingue, porque a ação do admin é diferente em
// cada uma.
const (
	// FalhaIndisponivel é rede fora, tempo esgotado ou desafio de bot: tentar de
	// novo pode resolver.
	FalhaIndisponivel Falha = "indisponivel"
	// FalhaFormato é a página ter chegado e não ser mais o que o patchbay sabe
	// ler: tentar de novo não resolve, o site mudou.
	FalhaFormato Falha = "formato"
)

func falhaDe(err error) Falha {
	if errors.Is(err, ErrFormatoDaOrigem) {
		return FalhaFormato
	}
	return FalhaIndisponivel
}

func avisoDe(err error) string {
	switch {
	case errors.Is(err, ErrNaoEncontrado):
		return "sumiu"
	case errors.Is(err, ErrFormatoDaOrigem):
		return "formato"
	default:
		return "indisponivel"
	}
}

// avisos são as mensagens que o adicionar devolve pela URL quando não deu.
var avisos = map[string]webui.Alerta{
	"sumiu": {
		Tom:    webui.TomAlerta,
		Titulo: "Esse servidor não está mais no catálogo.",
		Texto: "A origem respondeu que a página não existe — ele deve ter saído da lista " +
			"desde que esta tela carregou. Busque de novo, ou cadastre o upstream à mão.",
	},
	"indisponivel": {
		Tom:    webui.TomPerigo,
		Titulo: "Não foi possível falar com o mcpservers.org.",
		Texto: "A biblioteca lê o catálogo direto da origem a cada uso, então ela precisa " +
			"de saída para a internet. O cadastro de upstream à mão continua funcionando.",
	},
	"formato": {
		Tom:    webui.TomPerigo,
		Titulo: "A origem respondeu num formato que o patchbay não sabe ler.",
		Texto: "A página chegou, mas a marcação mudou — tentar de novo não resolve. " +
			"O log do processo tem o detalhe. Cadastre o upstream à mão por enquanto.",
	},
}

// Pagina é o que a tela precisa saber.
type Pagina struct {
	// Itens é o recorte já filtrado.
	Itens []Item
	// Termo é o que está na caixa de busca.
	Termo string
	// Total é quantos a origem publica agora, para a tela dizer "12 de 293".
	Total int
	// Falha é o motivo de não haver lista. Vazio quando a origem respondeu.
	Falha Falha
	// Alerta é o resultado da ação anterior.
	Alerta *webui.Alerta
}
