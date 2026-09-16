package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// A borda HTTP do "colar o comando de instalação".
//
// Uma rota, um POST, um MCP criado: a caixa de colar mora na própria tela de
// MCPs, e o que ela recebe vira cadastro sem passo intermediário. Até
// 2026-09-16 eram três rotas — colar, conferir, criar —, com uma tela de resumo
// no meio; ela saiu por decisão do dono, que queria colar e salvar.
//
// O que a conferência protegia, dito aqui para não ser redescoberto num
// incidente: um comando STDIO (`npx -y algum-mcp`) vira processo filho do
// patchbay, lançado com o usuário do processo e reiniciado pela supervisão
// sempre que cair. Antes havia uma tela avisando disso antes de gravar; agora o
// que existe é o alerta na própria caixa de colar. Quem cola assume o que o
// comando executa.
//
// O que não mudou: quem grava continua sendo o caminho do formulário — mesma
// validação, mesmo Criar (que cifra bearer, headers e variáveis na mesma
// transação do INSERT) e mesmo hot-apply. Se este handler tivesse escrita
// própria, seria por ela que um campo novo entraria no banco sem entrar na
// supervisão.

// tamanhoMaximoDoCorpo é o teto do POST que recebe o comando.
//
// Folgado em relação ao LimiteDoComando de propósito: o corpo chega
// url-encoded, e cada espaço ou aspa do comando vira três bytes. O limite que
// vale para o admin é o do comando; este só impede que o handler leia um corpo
// absurdo em memória antes de chegar lá.
const tamanhoMaximoDoCorpo = 4 * LimiteDoComando

// Os limites da guarda de notas.
const (
	// validadeDasNotas é quanto tempo as notas de uma importação esperam pelo
	// GET da tela de detalhe. Curto: é o intervalo de um redirecionamento, e o
	// que passar disso é aba que ninguém abriu.
	validadeDasNotas = 5 * time.Minute
	// maxNotas é o teto de importações esperando para ser lidas. A UI é de um
	// administrador; oito é folga, e o teto existe para que um redirecionamento
	// perdido não vire crescimento de memória sem fim.
	maxNotas = 8
)

// guardaDeNotas leva o que o patchbay ajustou do comando até a tela do MCP
// recém-criado.
//
// Existe porque a criação passou a ser direta: o que a tela de conferência
// dizia antes de gravar — "o --scope foi ignorado", "as variáveis foram para o
// bloco cifrado" — não tem mais onde aparecer, e mandar isso só para o log
// faria a tela mentir por omissão sobre o que ela acabou de gravar.
//
// Em memória e não no banco: é recado de uma navegação, e perdê-lo num restart
// não custa mais que o admin reler os mesmos fatos na tela de detalhe. Não
// guarda credencial — Avisos nomeia header e variável, nunca valor —, e ainda
// assim tem prazo e teto, porque estado de tela que só cresce é vazamento.
type guardaDeNotas struct {
	mu    sync.Mutex
	itens map[string]notasPendentes
}

type notasPendentes struct {
	avisos []string
	em     time.Time
}

func novaGuardaDeNotas() *guardaDeNotas {
	return &guardaDeNotas{itens: make(map[string]notasPendentes, maxNotas)}
}

// guardar registra as notas e devolve o identificador que vai na URL. Sem
// avisos, devolve vazio: não há o que levar, e um identificador para nota
// nenhuma só sujaria a URL.
//
// O identificador é aleatório de 128 bits e não um contador: ele viaja na query
// do redirecionamento, e um número previsível deixaria uma aba ler a nota de
// outra.
func (g *guardaDeNotas) guardar(avisos []string) (string, error) {
	if len(avisos) == 0 {
		return "", nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])

	g.mu.Lock()
	defer g.mu.Unlock()
	g.limpar(time.Now())
	for len(g.itens) >= maxNotas {
		g.removerMaisVelho()
	}
	g.itens[id] = notasPendentes{avisos: avisos, em: time.Now()}
	return id, nil
}

// tomar devolve as notas e as retira da guarda: elas valem para uma exibição.
func (g *guardaDeNotas) tomar(id string) []string {
	if id == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limpar(time.Now())
	item, ok := g.itens[id]
	if !ok {
		return nil
	}
	delete(g.itens, id)
	return item.avisos
}

// limpar descarta o que passou da validade. Roda dentro do mutex, nas duas
// operações: sem goroutine de varredura, uma nota expirada só sai quando alguém
// encosta na guarda — e é exatamente aí que ela precisa ter saído.
func (g *guardaDeNotas) limpar(agora time.Time) {
	for id, item := range g.itens {
		if agora.Sub(item.em) > validadeDasNotas {
			delete(g.itens, id)
		}
	}
}

func (g *guardaDeNotas) removerMaisVelho() {
	var maisVelho string
	var em time.Time
	for id, item := range g.itens {
		if maisVelho == "" || item.em.Before(em) {
			maisVelho, em = id, item.em
		}
	}
	delete(g.itens, maisVelho)
}

// DadosColar alimenta a caixa de colar na tela de MCPs.
type DadosColar struct {
	// Texto é o comando colado, de volta na caixa para ser corrigido. É a
	// exceção deliberada à regra de não devolver entrada para a tela: o erro
	// mais comum é o token que ainda é o exemplo da documentação, e corrigi-lo
	// exige tê-lo ali para editar.
	Texto string
	// Erro é a recusa, em texto de tela. Nunca contém credencial: quem monta a
	// mensagem é o analisador, e ele nomeia o header sem repetir o valor.
	Erro string
}

// colar lê o comando colado e cria o MCP, num passo só.
//
// O comando é analisado aqui e não recebido pronto da tela: o que grava é
// sempre o texto. Um resumo montado no navegador seria uma segunda fonte da
// verdade, e a diferença entre as duas apareceria como MCP cadastrado diferente
// do que a tela mostrou.
func (a *Admin) colar(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, tamanhoMaximoDoCorpo)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "O comando colado é grande demais ou o formulário veio malformado.",
			http.StatusRequestEntityTooLarge)
		return
	}

	texto := r.PostFormValue("comando")
	if strings.TrimSpace(texto) == "" {
		a.recusarColagem(w, r, "", ErrComandoVazio)
		return
	}

	imp, err := LerComandoDeInstalacao(texto)
	if err != nil {
		a.recusarColagem(w, r, texto, err)
		return
	}

	form := imp.Form
	if err := a.completarForm(r.Context(), &form); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	if !form.Validar() {
		// O analisador já recusa o que sabe recusar, então chegar aqui é o caso
		// raro: nome só de espaços, URL que passa pelo prefixo e não pelo
		// url.Parse. A mensagem do campo é melhor que uma genérica.
		a.recusarColagem(w, r, texto, errors.New(primeiroErro(form)))
		return
	}

	id, err := a.repo.Criar(r.Context(), form)
	switch {
	case errors.Is(err, ErrNomeEmUso):
		a.recusarColagem(w, r, texto, errors.New("Já existe um MCP chamado "+resumir(form.Nome)+
			". Troque o nome no comando e cole de novo."))
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	form.ID = id
	a.aplicarNoAr(r.Context(), registroDoForm(id, form))
	a.log.Info("upstream criado por comando colado",
		"upstream", form.Nome, "upstream_id", id, "tipo", form.TipoEfetivo(),
		"avisos", len(imp.Avisos))

	destino := webui.RotaUpstreams + "/" + strconv.FormatInt(id, 10) + "?aviso=importado"
	notas, err := a.notas.guardar(imp.Avisos)
	if err != nil {
		// Sem entropia para o identificador, o MCP já está criado e aplicado: o
		// que se perde é a nota, não a criação.
		a.log.Warn("não deu para guardar as notas da importação", "upstream_id", id, "erro", err)
	}
	if notas != "" {
		destino += "&notas=" + notas
	}
	webui.Redirecionar(w, r, destino)
}

// recusarColagem devolve a lista de MCPs com o comando de volta na caixa e o
// motivo em cima dela.
func (a *Admin) recusarColagem(w http.ResponseWriter, r *http.Request, texto string, err error) {
	a.log.Info("comando de instalação recusado pela ui", "erro", err)
	a.renderizarLista(w, r, http.StatusUnprocessableEntity,
		DadosColar{Texto: texto, Erro: err.Error()})
}

// primeiroErro escolhe uma mensagem entre as do formulário recusado.
//
// Ordenado por chave para que a mesma recusa produza sempre a mesma frase: com
// a ordem do mapa, o admin veria uma mensagem diferente a cada tentativa sobre
// o mesmo comando errado.
func primeiroErro(f Form) string {
	chaves := make([]string, 0, len(f.Erros))
	for k := range f.Erros {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	if len(chaves) == 0 {
		return "O comando foi lido, mas o cadastro não passou na validação."
	}
	return "O comando foi lido, mas o cadastro não passou: " + f.Erros[chaves[0]]
}
