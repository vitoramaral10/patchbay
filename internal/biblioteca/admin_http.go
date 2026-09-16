package biblioteca

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// PorTela é quantos servidores a tela mostra por página.
//
// Trinta e seis porque a grade é de uma, duas ou três colunas: o número fecha
// linha em qualquer uma delas. Não tem relação com o tamanho da página da
// origem — aquilo é a varredura, isto é leitura de banco local.
const PorTela = 36

// Admin é a borda HTTP da biblioteca.
//
// Lê do catálogo local e nada mais. A única escrita que ela oferece é pedir uma
// varredura fora de hora, e mesmo essa não escreve aqui: ela acorda o
// sincronizador. Quem cria upstream é o formulário de upstream — a biblioteca
// só monta o link que chega lá preenchido, e é isso que também mantém de pé a
// regra de arquitetura de uma feature não importar outra.
type Admin struct {
	repo *RepositorioSQLite
	sinc *Sincronizador
	log  *slog.Logger
}

// NovoAdmin monta a borda sobre o catálogo local e o sincronizador.
func NovoAdmin(repo *RepositorioSQLite, sinc *Sincronizador, log *slog.Logger) *Admin {
	return &Admin{repo: repo, sinc: sinc, log: log}
}

// Rotas registra as telas. Exigem sessão de admin, como o resto do painel.
//
// O nome do servidor vai no fim do caminho, e com reticências, porque nome do
// catálogo tem barra no meio (mcpservers.org/<slug>): num segmento simples o
// mux cortaria no meio do nome. É também por isso que "adicionar" vem antes do
// nome, e não depois — curinga com reticências só existe no último segmento.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaBiblioteca, a.listar)
	mux.HandleFunc("GET "+webui.RotaBiblioteca+"/adicionar/{nome...}", a.adicionar)
	mux.HandleFunc("POST "+webui.RotaBiblioteca+"/atualizar", a.atualizar)
}

// listar mostra uma página do catálogo local, filtrada pelo ?q=.
//
// O termo e a página vivem na URL e não em sessão: uma busca é linkável e
// sobrevive ao recarregar, que é a mesma razão pela qual a trilha filtra por
// GET. O htmx só acelera o que já funciona sem ele — com JavaScript desligado,
// o formulário continua sendo um GET comum que recarrega a página inteira.
func (a *Admin) listar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filtro := Filtro{
		Termo: strings.TrimSpace(q.Get("q")),
	}
	pagina := paginaDaQuery(q.Get("p"))
	htmx := r.Header.Get("HX-Request") == "true"

	dados := Pagina{
		Filtro:  filtro,
		Numero:  pagina,
		EmCurso: a.sinc.EmCurso(),
	}
	if estado, err := a.repo.Sincronizacao(r.Context()); err != nil {
		a.log.Warn("não foi possível ler o estado da biblioteca", "erro", err)
	} else {
		dados.Estado = estado
	}

	itens, total, err := a.repo.Buscar(r.Context(), filtro, PorTela, (pagina-1)*PorTela)
	if err != nil {
		// Catálogo ilegível é defeito do patchbay, não de terceiro: aqui o 500
		// é honesto.
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	// Página além do fim: sem isto, ?p=99 numa busca com resultados mostraria
	// "nenhum servidor com esse termo", que é a mensagem errada — há
	// resultados, o que não existe é aquela página. Acontece com link velho e
	// com quem digita na URL, e cair na última página é o que o admin espera.
	if len(itens) == 0 && total > 0 {
		pagina = (total + PorTela - 1) / PorTela
		dados.Numero = pagina
		itens, total, err = a.repo.Buscar(r.Context(), filtro, PorTela, (pagina-1)*PorTela)
		if err != nil {
			webui.ErroInterno(w, r, a.log, err)
			return
		}
	}
	dados.Itens, dados.Total = itens, total

	if htmx {
		webui.Renderizar(w, r, http.StatusOK, a.log, Resultados(dados))
		return
	}
	dados.Alerta = webui.Avisos(r, avisos)
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaBiblioteca(dados))
}

// adicionar leva o servidor escolhido para o formulário de upstream preenchido.
//
// GET e não POST: nada muda no patchbay — lê-se o catálogo local e
// redireciona-se. O que muda estado é o formulário do outro lado, que o admin
// ainda vai revisar e salvar.
func (a *Admin) adicionar(w http.ResponseWriter, r *http.Request) {
	nome := r.PathValue("nome")

	item, err := a.repo.Um(r.Context(), nome)
	if err != nil {
		if !errors.Is(err, ErrNaoEncontrado) {
			webui.ErroInterno(w, r, a.log, err)
			return
		}
		webui.Redirecionar(w, r, webui.RotaBiblioteca+"?aviso=sumiu")
		return
	}
	if idx, ok := indiceDoEndpoint(r, item); ok {
		item.URL = item.Endpoints[idx]
	}
	webui.Redirecionar(w, r, rotaDeCadastro(item))
}

// indiceDoEndpoint lê o índice do endpoint escolhido no cartão (D-07), quando
// válido.
//
// Query ausente ou índice fora da faixa não é erro: cai no comportamento
// atual, que usa a URL do item — o mesmo link "Adicionar" de sempre.
func indiceDoEndpoint(r *http.Request, item Item) (int, bool) {
	v := r.URL.Query().Get("endpoint")
	if v == "" {
		return 0, false
	}
	idx, err := strconv.Atoi(v)
	if err != nil || idx < 0 || idx >= len(item.Endpoints) {
		return 0, false
	}
	return idx, true
}

// atualizar pede uma varredura fora de hora.
//
// POST porque dispara trabalho: um GET que varre o mcpservers.org seria
// varredura a cada prefetch de navegador. A resposta é imediata e a varredura
// continua no fundo — ela leva minutos, e prender a requisição só faria o
// navegador desistir no meio.
func (a *Admin) atualizar(w http.ResponseWriter, r *http.Request) {
	aviso := "atualizando"
	if !a.sinc.Disparar() {
		aviso = "ja-atualizando"
	}
	webui.Redirecionar(w, r, webui.RotaBiblioteca+"?aviso="+aviso)
}

// paginaDaQuery lê o número da página, tolerando o que não é número.
//
// Página fora de faixa não é erro para o admin: ele chegou aqui por um link ou
// pela URL, e cair no começo é o comportamento previsível.
func paginaDaQuery(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return n
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
// navegador, log de proxy e Referer. O modo de credencial entra só quando a
// página de detalhe declarou a autenticação — sem essa declaração, mandar um
// palpite seria pior do que deixar o formulário no padrão dele.
func rotaDeCadastro(i Item) string {
	q := url.Values{
		"tipo": {i.Transporte},
		"nome": {i.Titulo},
	}
	// O modo só entra quando a página de detalhe declarou a autenticação.
	// Servidor sem declaração não leva palpite: o formulário fica no padrão
	// dele e o admin escolhe.
	if modo := i.ModoDeCredencial(); modo != "" {
		q.Set("modo", modo)
	}
	// Sem comando publicado, nada de comando/arg na query: o formulário fica
	// no padrão dele, e o admin escolhe.
	if i.Remoto() {
		q.Set("url", i.URL)
	} else if i.Comando != "" {
		q.Set("comando", i.Comando)
		for _, a := range i.Args {
			q.Add("arg", a)
		}
	}
	return webui.RotaUpstreams + "/novo?" + q.Encode()
}

// avisos são as mensagens que as ações devolvem pela URL.
var avisos = map[string]webui.Alerta{
	"sumiu": {
		Tom:    webui.TomAlerta,
		Titulo: "Esse servidor não está mais no catálogo.",
		Texto: "Ele saiu da lista entre esta tela carregar e você clicar — quase sempre " +
			"porque uma sincronização entrou no meio. Busque de novo, ou cadastre o MCP à mão.",
	},
	"atualizando": {
		Tom:    webui.TomInfo,
		Titulo: "A atualização do catálogo começou.",
		Texto: "Ela roda no fundo e leva de 20 a 35 minutos: são 22 páginas de índice e " +
			"cerca de 650 de detalhe, na lista oficial do mcpservers.org. A lista abaixo " +
			"continua sendo a anterior até ela terminar.",
	},
	"ja-atualizando": {
		Tom:    webui.TomInfo,
		Titulo: "Já tem uma atualização rodando.",
		Texto: "Duas seguidas dariam o mesmo resultado, então esta foi ignorada. " +
			"Recarregue daqui a alguns minutos.",
	},
}

// Pagina é o que a tela precisa saber.
type Pagina struct {
	// Itens é a página do catálogo local.
	Itens []Item
	// Filtro é o recorte pedido: o termo digitado.
	Filtro Filtro
	// Total é quantos servidores casam com o termo, no catálogo inteiro.
	Total int
	// Numero é a página que está sendo mostrada, contando de 1.
	Numero int
	// Estado é a idade do catálogo e o que houve na última varredura.
	Estado Sincronizacao
	// EmCurso é uma varredura estar rodando agora.
	EmCurso bool
	// Alerta é o resultado da ação anterior.
	Alerta *webui.Alerta
}

// Paginas é quantas páginas o filtro atual tem.
func (p Pagina) Paginas() int {
	if p.Total == 0 {
		return 0
	}
	return int(math.Ceil(float64(p.Total) / float64(PorTela)))
}

// TemProxima e TemAnterior decidem se os links de navegação aparecem. Link que
// não leva a lugar nenhum é ruído.
func (p Pagina) TemProxima() bool { return p.Numero < p.Paginas() }
func (p Pagina) TemAnterior() bool {
	return p.Numero > 1 && p.Paginas() > 0
}

// Proxima e Anterior são os links, preservando a busca.
func (p Pagina) Proxima() string { return p.rota(p.Numero + 1) }
func (p Pagina) Anterior() string {
	return p.rota(p.Numero - 1)
}

func (p Pagina) rota(numero int) string {
	q := url.Values{}
	if p.Filtro.Termo != "" {
		q.Set("q", p.Filtro.Termo)
	}
	if numero > 1 {
		q.Set("p", strconv.Itoa(numero))
	}
	if len(q) == 0 {
		return webui.RotaBiblioteca
	}
	return webui.RotaBiblioteca + "?" + q.Encode()
}

// CatalogoVazio distingue "a busca não achou" de "o catálogo ainda não existe".
//
// São situações diferentes e a saída do admin é outra em cada uma: uma pede
// outro termo, a outra pede esperar a primeira varredura terminar. A mesma
// mensagem para as duas faria a instalação nova parecer defeito.
func (p Pagina) CatalogoVazio() bool { return p.Estado.Nunca() }
