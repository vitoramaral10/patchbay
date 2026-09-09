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
// Três rotas, e a do meio não escreve nada: colar, conferir, criar. A tela de
// conferência existe porque o comando pode lançar um processo local, e um
// `npx -y algo-qualquer` colado de um README que ninguém leu não pode virar
// processo filho do patchbay sem alguém ver o que ele executa. É a mesma forma
// do import de YAML — plano antes de aplicar —, pelo mesmo motivo.
//
// O que muda em relação ao import de YAML é onde o texto espera entre as duas
// telas. Lá ele volta num campo escondido; aqui não pode, porque o comando
// carrega o token literal — `--header "Authorization: Bearer ..."` — e este
// projeto não devolve segredo para a tela. Então a conferência guarda o texto
// aqui dentro e leva para o navegador só um identificador opaco.
//
// A caixa de colar é a exceção, e deliberada: quando a leitura falha, o comando
// volta para ela porque o erro mais comum é o token que ainda é o exemplo da
// documentação, e corrigi-lo exige tê-lo ali para editar. Campo de entrada que
// o admin acabou de digitar é outra coisa que resumo renderizado de volta.

// tamanhoMaximoDoCorpo é o teto do POST das rotas que recebem o comando.
//
// Folgado em relação ao LimiteDoComando de propósito: o corpo chega
// url-encoded, e cada espaço ou aspa do comando vira três bytes. O limite que
// vale para o admin é o do comando; este só impede que o handler leia um corpo
// absurdo em memória antes de chegar lá.
const tamanhoMaximoDoCorpo = 4 * LimiteDoComando

// Os limites da guarda de conferências pendentes.
const (
	// validadeDaConferencia é quanto tempo o comando espera pelo clique. Curto
	// porque o que espera é uma credencial em claro na memória do processo, e
	// longo o bastante para quem foi conferir a URL na documentação antes de
	// confirmar.
	validadeDaConferencia = 15 * time.Minute
	// maxConferencias é o teto de telas abertas ao mesmo tempo. A UI é de um
	// administrador; oito é folga, e o teto existe para que uma aba abandonada
	// não vire um vazamento de memória com token dentro.
	maxConferencias = 8
)

// guardaDeComandos guarda o comando colado entre a tela de conferência e o
// clique que cria.
//
// Em memória e não no banco: é estado de uma tela aberta, com credencial em
// claro dentro, e nada disso deve sobreviver a um restart. Perder a conferência
// num restart custa um colar a mais; gravá-la custaria um segredo em claro numa
// tabela que ninguém lembraria de limpar.
type guardaDeComandos struct {
	mu    sync.Mutex
	itens map[string]comandoPendente
}

type comandoPendente struct {
	texto string
	em    time.Time
}

func novaGuardaDeComandos() *guardaDeComandos {
	return &guardaDeComandos{itens: make(map[string]comandoPendente, maxConferencias)}
}

// guardar registra o comando e devolve o identificador que vai para a tela.
//
// O identificador é aleatório de 128 bits e não um contador: ele viaja no HTML
// e volta num POST, e um número previsível deixaria uma aba adivinhar a
// conferência de outra.
func (g *guardaDeComandos) guardar(texto string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])

	g.mu.Lock()
	defer g.mu.Unlock()
	g.limpar(time.Now())
	for len(g.itens) >= maxConferencias {
		g.removerMaisVelho()
	}
	g.itens[id] = comandoPendente{texto: texto, em: time.Now()}
	return id, nil
}

// tomar devolve o comando e o retira da guarda.
//
// De uma vez só: o identificador vale para um clique, e o segundo clique no
// mesmo botão não pode tentar criar de novo o que já foi criado.
func (g *guardaDeComandos) tomar(id string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limpar(time.Now())
	item, ok := g.itens[id]
	if !ok {
		return "", false
	}
	delete(g.itens, id)
	return item.texto, true
}

// limpar descarta o que passou da validade. Roda dentro do mutex, nas duas
// operações: sem goroutine de varredura, um comando expirado só sai quando
// alguém encosta na guarda — e é exatamente aí que ele precisa ter saído.
func (g *guardaDeComandos) limpar(agora time.Time) {
	for id, item := range g.itens {
		if agora.Sub(item.em) > validadeDaConferencia {
			delete(g.itens, id)
		}
	}
}

func (g *guardaDeComandos) removerMaisVelho() {
	var maisVelho string
	var em time.Time
	for id, item := range g.itens {
		if maisVelho == "" || item.em.Before(em) {
			maisVelho, em = id, item.em
		}
	}
	delete(g.itens, maisVelho)
}

// DadosImportar alimenta a tela de colar o comando.
type DadosImportar struct {
	// Texto é o comando colado, de volta na caixa para ser corrigido.
	Texto string
	// Erro é a recusa, em texto de tela. Nunca contém credencial: quem monta a
	// mensagem é o analisador, e ele nomeia o header sem repetir o valor.
	Erro string
}

func (a *Admin) formImportar(w http.ResponseWriter, r *http.Request) {
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaImportar(DadosImportar{}))
}

// lerComando analisa o comando colado e mostra o que seria criado.
//
// Atende também o "voltar e editar" da tela de conferência, que chega com o
// identificador da pendência e o pedido de edição: é o caminho de volta sem
// obrigar a colar tudo de novo.
func (a *Admin) lerComando(w http.ResponseWriter, r *http.Request) {
	texto, ok := a.textoDoPedido(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("editar") != "" {
		webui.Renderizar(w, r, http.StatusOK, a.log, TelaImportar(DadosImportar{Texto: texto}))
		return
	}

	imp, err := LerComandoDeInstalacao(texto)
	if err != nil {
		a.recusar(w, r, texto, err)
		return
	}
	pendencia, err := a.comandos.guardar(texto)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaConferirComando(imp, pendencia))
}

// importar grava o MCP descrito pelo comando.
//
// Daqui para baixo é o caminho de criação do formulário, sem atalho: mesma
// validação, mesmo Criar (que cifra bearer, headers e variáveis na mesma
// transação do INSERT) e mesmo hot-apply. Se este handler tivesse uma escrita
// própria, seria por ela que um campo novo entraria no banco sem entrar na
// supervisão.
//
// O comando é reanalisado aqui, e não recebido pronto da tela: o que grava é
// sempre o texto. Um resumo devolvido pelo navegador seria uma segunda fonte da
// verdade, e a diferença entre as duas apareceria como MCP cadastrado diferente
// do que a conferência mostrou.
func (a *Admin) importar(w http.ResponseWriter, r *http.Request) {
	texto, ok := a.textoDoPedido(w, r)
	if !ok {
		return
	}
	imp, err := LerComandoDeInstalacao(texto)
	if err != nil {
		a.recusar(w, r, texto, err)
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
		// url.Parse. A mensagem do campo é melhor que uma genérica, e a caixa
		// de colar é o lugar dela — o formulário cheio não teria como receber o
		// segredo de volta.
		a.recusar(w, r, texto, errors.New(primeiroErro(form)))
		return
	}

	id, err := a.repo.Criar(r.Context(), form)
	switch {
	case errors.Is(err, ErrNomeEmUso):
		a.recusar(w, r, texto, errors.New("Já existe um MCP chamado "+resumir(form.Nome)+
			". Troque o nome no comando e cole de novo."))
		return
	case err != nil:
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	form.ID = id
	a.aplicarNoAr(r.Context(), registroDoForm(id, form))
	a.log.Info("upstream criado por comando colado",
		"upstream", form.Nome, "upstream_id", id, "tipo", form.TipoEfetivo())
	webui.Redirecionar(w, r, webui.RotaUpstreams+"/"+strconv.FormatInt(id, 10)+"?aviso=importado")
}

// recusar devolve a tela de colar com o comando e o motivo.
func (a *Admin) recusar(w http.ResponseWriter, r *http.Request, texto string, err error) {
	a.log.Info("comando de instalação recusado pela ui", "erro", err)
	webui.Renderizar(w, r, http.StatusUnprocessableEntity, a.log,
		TelaImportar(DadosImportar{Texto: texto, Erro: err.Error()}))
}

// textoDoPedido devolve o comando desta requisição, venha ele da caixa de colar
// ou da conferência pendente, ou responde à requisição.
func (a *Admin) textoDoPedido(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, tamanhoMaximoDoCorpo)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "O comando colado é grande demais ou o formulário veio malformado.",
			http.StatusRequestEntityTooLarge)
		return "", false
	}

	if id := r.PostFormValue("pendencia"); id != "" {
		texto, ok := a.comandos.tomar(id)
		if !ok {
			// Expirou, o processo reiniciou, ou o botão foi clicado duas vezes.
			// Nos três casos o texto sumiu de vez, e dizer isso é melhor que uma
			// tela em branco.
			webui.Renderizar(w, r, http.StatusUnprocessableEntity, a.log,
				TelaImportar(DadosImportar{Erro: "Esta conferência não vale mais — " +
					"ou já foi usada, ou passou do tempo. Cole o comando de novo."}))
			return "", false
		}
		return texto, true
	}

	texto := r.PostFormValue("comando")
	if strings.TrimSpace(texto) == "" {
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, a.log,
			TelaImportar(DadosImportar{Erro: ErrComandoVazio.Error()}))
		return "", false
	}
	return texto, true
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
