package trilha_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/trilha"
)

// telasDeTeste sobe as três rotas de observabilidade num servidor de verdade.
//
// Sem o portão de sessão de admin: quem protege /admin/ é o mux de main, e este
// teste é sobre o que as telas respondem, não sobre quem entra nelas.
type telasDeTeste struct {
	url  string
	repo *trilha.RepositorioSQLite
	hub  *trilha.Hub
}

func subirTelas(t *testing.T) telasDeTeste {
	t.Helper()

	repo := repositorioDeTeste(t)
	hub := trilha.NovoHub()
	reg := trilha.NovoRegistrador(repo, semLog(), trilha.ComHub(hub))

	mux := http.NewServeMux()
	trilha.NovoAdmin(repo, reg, hub, semLog()).Rotas(mux)

	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		// Encerrar antes de Close: o Close espera pelas requisições em curso, e
		// um stream de SSE só termina quando o hub solta o assinante ou o
		// cliente desiste. É a mesma ordem que main usa no desligamento.
		hub.Encerrar()
		ts.Close()
	})
	return telasDeTeste{url: ts.URL, repo: repo, hub: hub}
}

func abrir(t *testing.T, url string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: erro = %v, quer nil", url, err)
	}
	defer func() { _ = res.Body.Close() }()

	corpo, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ler corpo de %s: erro = %v, quer nil", url, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, quer 200", url, res.StatusCode)
	}
	return string(corpo)
}

// TestTelaTrilha_Filtra é o critério "a tela filtra", pela borda HTTP: o recorte
// vem da query string e o HTML mostra só o que casa.
func TestTelaTrilha_Filtra(t *testing.T) {
	t.Parallel()

	telas := subirTelas(t)
	gravarAmostra(t, telas.repo)

	casos := map[string]struct {
		query     string
		querTer   []string
		querNaoTe []string
	}{
		"sem filtro mostra tudo": {
			query:   "",
			querTer: []string{"notion_buscar", "github_issues", "notion_criar", "github_pr"},
		},
		"por endpoint": {
			query:     "?endpoint=pessoal",
			querTer:   []string{"notion_buscar", "github_issues"},
			querNaoTe: []string{"notion_criar", "github_pr"},
		},
		"por upstream": {
			query:     "?upstream=notion",
			querTer:   []string{"notion_buscar", "notion_criar"},
			querNaoTe: []string{"github_issues", "github_pr"},
		},
		"por resultado": {
			query:     "?resultado=timeout",
			querTer:   []string{"notion_criar"},
			querNaoTe: []string{"notion_buscar", "github_pr"},
		},
		"por ferramenta": {
			query:     "?ferramenta=issues",
			querTer:   []string{"github_issues"},
			querNaoTe: []string{"notion_buscar"},
		},
		"por período": {
			query:     "?periodo=15m",
			querTer:   []string{"notion_buscar", "github_issues"},
			querNaoTe: []string{"github_pr"},
		},
		"combinação sem resultado explica que é o filtro": {
			query:     "?endpoint=pessoal&resultado=timeout",
			querTer:   []string{"Nada casa com este filtro", "Limpar filtro"},
			querNaoTe: []string{"notion_buscar"},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			html := abrir(t, telas.url+webui.RotaTrilha+tc.query)
			for _, esperado := range tc.querTer {
				if !strings.Contains(html, esperado) {
					t.Errorf("a tela não mostra %q com %q", esperado, tc.query)
				}
			}
			// A assertiva negativa olha só o corpo da tabela. Os seletores do
			// filtro oferecem *todos* os valores conhecidos da trilha, de
			// propósito — filtrar por algo que não está na lista seria adivinhar
			// —, então procurar o nome no HTML inteiro acusaria o <option>.
			linhas := corpoDaTabela(html)
			for _, proibido := range tc.querNaoTe {
				if strings.Contains(linhas, proibido) {
					t.Errorf("a tabela mostra %q com %q, e não devia", proibido, tc.query)
				}
			}
		})
	}
}

// TestTelaTrilha_MostraOContadorDeDescartes é o critério explícito da seção 11:
// se a trilha descartar linhas por rajada, o número aparece na tela.
func TestTelaTrilha_MostraOContadorDeDescartes(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	hub := trilha.NovoHub()
	// Consumidor nunca iniciado e fila de um: o segundo evento é descartado.
	reg := trilha.NovoRegistrador(repo, semLog(), trilha.ComCapacidade(1), trilha.ComHub(hub))

	mux := http.NewServeMux()
	trilha.NovoAdmin(repo, reg, hub, semLog()).Rotas(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Antes de qualquer descarte o contador já está na tela, zerado: um número
	// que só existe quando há problema é um número que ninguém aprende a ler.
	html := abrir(t, ts.URL+webui.RotaTrilha)
	if !strings.Contains(html, "Descartes da trilha") {
		t.Fatal("a tela não mostra o contador de descartes")
	}
	if strings.Contains(html, "A trilha perdeu linhas") {
		t.Error("a tela avisa de perda sem ter havido nenhuma")
	}

	for range 5 {
		reg.Observar(evento("somar"))
	}
	if got := reg.Descartes(); got != 4 {
		t.Fatalf("Descartes() = %d, quer 4", got)
	}

	html = abrir(t, ts.URL+webui.RotaTrilha)
	if !strings.Contains(html, "A trilha perdeu linhas") {
		t.Error("a tela não avisa que a trilha perdeu linhas")
	}
	if !strings.Contains(html, ">4<") {
		t.Errorf("a tela não mostra o número 4 de descartes:\n%s", recorte(html, "Descartes da trilha", 400))
	}
}

// TestFluxoSSE_EntregaAosDoisAssinantes prova o fan-out pela borda HTTP: duas
// conexões abertas recebem a mesma linha, e o formato é o que a extensão sse do
// htmx espera (event: <tipo>, data: <fragmento>).
func TestFluxoSSE_EntregaAosDoisAssinantes(t *testing.T) {
	t.Parallel()

	telas := subirTelas(t)

	primeira := conectarFluxo(t, telas.url)
	segunda := conectarFluxo(t, telas.url)
	esperar(t, func() bool { return telas.hub.Assinantes() == 2 },
		"as duas conexões de SSE não chegaram a assinar o hub")

	telas.hub.Publicar(trilha.Mensagem{
		Tipo: trilha.TipoChamada,
		Chamada: trilha.Evento{
			Inicio: time.Now(), Duracao: 12 * time.Millisecond,
			EndpointSlug: "pessoal", UpstreamNome: "notion",
			Ferramenta: "notion_buscar", Resultado: trilha.ResultadoOK,
		},
	})

	for nome, fluxo := range map[string]*fluxoDeTeste{"primeira": primeira, "segunda": segunda} {
		evento := fluxo.proximoEvento(t)
		if evento.tipo != "chamada" {
			t.Errorf("%s: event = %q, quer %q", nome, evento.tipo, "chamada")
		}
		if !strings.Contains(evento.dados, "notion_buscar") {
			t.Errorf("%s: dados = %q, quer conter a ferramenta", nome, evento.dados)
		}
		if !strings.Contains(evento.dados, "<li") {
			t.Errorf("%s: dados = %q, quer um fragmento de HTML", nome, evento.dados)
		}
	}
}

// TestFluxoSSE_AssinanteLentoNaoTravaOsOutros: a conexão que parou de ler perde
// as linhas dela e só as dela. Pela borda HTTP, e não só pelo hub, porque é aqui
// que a contrapressão do TCP apareceria se ela existisse.
//
// A rápida é drenada por uma goroutine *durante* a publicação, e não só
// depois: sem isso o canal dela também enche (o teto de Hub é por assinante,
// não só para quem não lê nada) e o próprio teste perderia o "marco final" que
// ele quer conferir — não por causa da lenta, mas por não estar lendo rápido o
// bastante.
func TestFluxoSSE_AssinanteLentoNaoTravaOsOutros(t *testing.T) {
	t.Parallel()

	telas := subirTelas(t)

	lenta := conectarFluxo(t, telas.url) // aberta e nunca lida
	_ = lenta
	rapida := conectarFluxo(t, telas.url)
	esperar(t, func() bool { return telas.hub.Assinantes() == 2 },
		"as duas conexões de SSE não chegaram a assinar o hub")

	// A goroutine de dreno é uma só, por toda a vida da conexão: ler de um
	// bufio.Reader de duas goroutines ao mesmo tempo corromperia o stream. Ela
	// sai sozinha quando a conexão fecha, no t.Cleanup de conectarFluxo.
	eventosDaRapida := make(chan eventoSSE, trilha.CapacidadeAssinante*4)
	leituraAcabou := make(chan error, 1)
	go func() {
		for {
			linha, err := rapida.leitor.ReadString('\n')
			if err != nil {
				leituraAcabou <- err
				return
			}
			linha = strings.TrimRight(linha, "\r\n")
			eventosDaRapida <- eventoSSE{dados: linha}
		}
	}()

	// Muito acima da capacidade por assinante: a fila da lenta transborda, e a
	// da rápida também — o burst em si é mais rápido que qualquer consumidor
	// de rede consegue escoar, então o hub descarta para as duas durante o
	// pico. O que se prova não é que nenhuma linha se perde para a rápida
	// (o teto é por assinante, e um burst que o excede sempre derruba
	// algumas) — é que ela não fica para trás para sempre: o marco final é
	// republicado até ela confirmar, e tem de chegar dentro do teto do teste.
	publicou := make(chan struct{})
	pararMarco := make(chan struct{})
	go func() {
		defer close(publicou)
		for i := 0; i < trilha.CapacidadeAssinante*3; i++ {
			telas.hub.Publicar(mensagemDeLog("linha de teste"))
		}
		tique := time.NewTicker(2 * time.Millisecond)
		defer tique.Stop()
		for {
			telas.hub.Publicar(mensagemDeLog("marco final"))
			select {
			case <-pararMarco:
				return
			case <-tique.C:
			}
		}
	}()

	achouMarco := false
	total := 0
	limite := time.After(tetoDeEspera)
	for !achouMarco {
		select {
		case l := <-eventosDaRapida:
			total++
			if strings.Contains(l.dados, "marco final") {
				achouMarco = true
			}
		case err := <-leituraAcabou:
			t.Fatalf("leitura da rápida terminou (linhas vistas: %d): erro = %v", total, err)
		case <-limite:
			t.Fatalf("o marco final não chegou à assinante rápida, que estava sendo drenada (linhas vistas: %d), perdidas=%d",
				total, telas.hub.Perdidas())
		}
	}
	close(pararMarco)
	<-publicou

	if telas.hub.Perdidas() == 0 {
		t.Error("Perdidas() = 0, quer mais de zero")
	}
}

// TestFluxoSSE_EncerrarFechaAConexao prova o caminho de desligamento pela borda:
// o corpo termina em EOF em vez de ficar pendurado.
func TestFluxoSSE_EncerrarFechaAConexao(t *testing.T) {
	t.Parallel()

	telas := subirTelas(t)
	fluxo := conectarFluxo(t, telas.url)
	esperar(t, func() bool { return telas.hub.Assinantes() == 1 },
		"a conexão de SSE não chegou a assinar o hub")

	telas.hub.Encerrar()

	fim := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(fluxo.corpo)
		fim <- err
	}()
	select {
	case err := <-fim:
		if err != nil {
			t.Errorf("ler até o fim: erro = %v, quer nil (EOF limpo)", err)
		}
	case <-time.After(tetoDeEspera):
		t.Fatal("a conexão de SSE não fechou depois de Encerrar")
	}
}

// TestFluxoSSE_CRLFNaoInjetaLinhaDeEvento prova que um \r ou \r\n embutido num
// campo de terceiro (o nome de uma ferramenta, por exemplo) não consegue
// forjar uma linha "event:" extra no stream: a especificação de SSE aceita \r
// sozinho como fim de campo, e sem normalizar antes do split o cliente veria
// um segundo evento que o servidor nunca quis mandar.
func TestFluxoSSE_CRLFNaoInjetaLinhaDeEvento(t *testing.T) {
	t.Parallel()

	telas := subirTelas(t)
	fluxo := conectarFluxo(t, telas.url)
	esperar(t, func() bool { return telas.hub.Assinantes() == 1 },
		"a conexão de SSE não chegou a assinar o hub")

	telas.hub.Publicar(trilha.Mensagem{
		Tipo: trilha.TipoChamada,
		Chamada: trilha.Evento{
			Inicio: time.Now(), EndpointSlug: "pessoal", UpstreamNome: "notion",
			Ferramenta: "x\r\revent: log\rdata: forjado", Resultado: trilha.ResultadoOK,
		},
	})

	linhas := lerBlocoSSE(t, fluxo.leitor)
	n := 0
	for _, l := range linhas {
		if strings.HasPrefix(l, "event: ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("linhas \"event: \" no bloco = %d, quer 1 (a ferramenta não pode forjar uma segunda): %v", n, linhas)
	}
}

// lerBlocoSSE lê um bloco de SSE inteiro, tratando \r, \r\n e \n como fim de
// linha — a especificação de SSE aceita os três. É o que faz este teste
// enxergar a injeção que um bufio.Reader.ReadString('\n') comum não veria,
// porque só ele para em \n.
func lerBlocoSSE(t *testing.T, r *bufio.Reader) []string {
	t.Helper()

	lido := make(chan []string, 1)
	falha := make(chan error, 1)
	go func() {
		var linhas []string
		var atual strings.Builder
		for {
			b, err := r.ReadByte()
			if err != nil {
				falha <- err
				return
			}
			fimDeLinha := b == '\n'
			if b == '\r' {
				fimDeLinha = true
				if pb, err := r.Peek(1); err == nil && len(pb) == 1 && pb[0] == '\n' {
					_, _ = r.ReadByte()
				}
			}
			if !fimDeLinha {
				atual.WriteByte(b)
				continue
			}
			l := atual.String()
			atual.Reset()
			if l == "" {
				lido <- linhas
				return
			}
			linhas = append(linhas, l)
		}
	}()

	select {
	case l := <-lido:
		return l
	case err := <-falha:
		t.Fatalf("ler bloco de SSE: erro = %v, quer nil", err)
		return nil
	case <-time.After(tetoDeEspera):
		t.Fatal("nenhum bloco de SSE chegou")
		return nil
	}
}

// fluxoDeTeste é uma conexão de SSE aberta, com o leitor de linhas por cima.
type fluxoDeTeste struct {
	corpo  io.ReadCloser
	leitor *bufio.Reader
}

type eventoSSE struct {
	tipo  string
	dados string
}

// conectarFluxo abre uma conexão de SSE e espera o cabeçalho.
func conectarFluxo(t *testing.T, base string) *fluxoDeTeste {
	t.Helper()

	ctx, cancelar := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+webui.RotaLogsFluxo, http.NoBody)
	if err != nil {
		cancelar()
		t.Fatalf("montar requisição: erro = %v, quer nil", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancelar()
		t.Fatalf("GET %s: erro = %v, quer nil", webui.RotaLogsFluxo, err)
	}
	t.Cleanup(func() {
		cancelar()
		_ = res.Body.Close()
	})

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, quer 200", res.StatusCode)
	}
	if tipo := res.Header.Get("Content-Type"); tipo != "text/event-stream" {
		t.Fatalf("Content-Type = %q, quer %q", tipo, "text/event-stream")
	}
	return &fluxoDeTeste{corpo: res.Body, leitor: bufio.NewReader(res.Body)}
}

// proximoEvento lê até o fim do próximo evento, pulando os comentários de
// batida.
func (f *fluxoDeTeste) proximoEvento(t *testing.T) eventoSSE {
	t.Helper()

	lido := make(chan eventoSSE, 1)
	erro := make(chan error, 1)
	go func() {
		var atual eventoSSE
		for {
			linha, err := f.leitor.ReadString('\n')
			if err != nil {
				erro <- err
				return
			}
			linha = strings.TrimRight(linha, "\r\n")
			switch {
			case strings.HasPrefix(linha, ":"):
				// batida de manutenção
			case strings.HasPrefix(linha, "event: "):
				atual.tipo = strings.TrimPrefix(linha, "event: ")
			case strings.HasPrefix(linha, "data: "):
				atual.dados += strings.TrimPrefix(linha, "data: ")
			case linha == "" && atual.tipo != "":
				lido <- atual
				return
			}
		}
	}()

	select {
	case e := <-lido:
		return e
	case err := <-erro:
		t.Fatalf("ler do stream de SSE: erro = %v, quer nil", err)
		return eventoSSE{}
	case <-time.After(tetoDeEspera):
		t.Fatalf("nenhum evento de SSE chegou em %v", tetoDeEspera)
		return eventoSSE{}
	}
}

// corpoDaTabela devolve só o <tbody> da trilha, que é onde as linhas moram.
// Fora dele estão o cabeçalho e os seletores do filtro, que citam nomes de
// endpoint, upstream e ferramenta por desenho.
func corpoDaTabela(html string) string {
	i := strings.Index(html, "<tbody")
	if i < 0 {
		return ""
	}
	fim := strings.Index(html[i:], "</tbody>")
	if fim < 0 {
		return html[i:]
	}
	return html[i : i+fim]
}

// recorte devolve um pedaço do HTML em torno de uma marca, para a mensagem de
// falha caber na tela.
func recorte(html, marca string, tamanho int) string {
	i := strings.Index(html, marca)
	if i < 0 {
		return "(marca não encontrada)"
	}
	fim := min(i+tamanho, len(html))
	return html[i:fim]
}
