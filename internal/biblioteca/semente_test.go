package biblioteca_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

func TestSementeIdaEVolta(t *testing.T) {
	t.Parallel()

	itens := append(itensDeTeste(), biblioteca.Item{
		Nome: "mcpservers.org/com-endpoints", Titulo: "Com Endpoints",
		Transporte: biblioteca.TransporteHTTP,
		URL:        "https://a.test/mcp",
		Endpoints:  []string{"https://a.test/mcp", "https://b.test/mcp"},
	})

	gerado := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := biblioteca.GravarSemente(&buf, itens, gerado); err != nil {
		t.Fatalf("GravarSemente: erro = %v, quer nil", err)
	}
	// Comprimida: o catálogo inteiro vai dentro do binário, e texto puro
	// custaria megabytes por release.
	if buf.Len() == 0 || buf.Bytes()[0] != 0x1f {
		t.Fatalf("a semente não saiu comprimida (%d bytes)", buf.Len())
	}

	// Descomprime e decodifica pelo lado de fora, sem acesso ao tipo interno
	// arquivoDeSemente, só para provar que o formato grava e lê o campo novo
	// (D-07): a semente atual, gerada sem Endpoints, continua decodificando —
	// é o que TestSementeVersionadaEValida garante em separado.
	z, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("abrir gzip: erro = %v, quer nil", err)
	}
	var lido struct {
		GeradoEm time.Time         `json:"geradoEm"`
		Itens    []biblioteca.Item `json:"itens"`
	}
	if err := json.NewDecoder(z).Decode(&lido); err != nil {
		t.Fatalf("decodificar semente: erro = %v, quer nil", err)
	}
	if !lido.GeradoEm.Equal(gerado) {
		t.Errorf("GeradoEm = %v, quer %v", lido.GeradoEm, gerado)
	}
	if len(lido.Itens) != len(itens) {
		t.Fatalf("Itens = %d, quer %d", len(lido.Itens), len(itens))
	}
	ultimo := lido.Itens[len(lido.Itens)-1]
	if len(ultimo.Endpoints) != 2 || ultimo.Endpoints[0] != "https://a.test/mcp" ||
		ultimo.Endpoints[1] != "https://b.test/mcp" {
		t.Fatalf("Endpoints = %v, quer [a b] na ordem", ultimo.Endpoints)
	}
}

// TestPrimeiroBootUsaASemente: instalação nova não pode nascer vazia. A
// primeira varredura leva cerca de 30 minutos (29m26s medidos em 2026-09-11),
// e é a semente que evita a tela ficar esse tempo dizendo "o catálogo ainda
// está sendo baixado".
func TestPrimeiroBootUsaASemente(t *testing.T) {
	t.Parallel()

	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()

	repo := repoDeTeste(t)
	gerado := time.Now().Add(-72 * time.Hour)
	// Origem que não responde: o que se prova é a semente, não a varredura.
	fora := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora", http.StatusBadGateway)
	})
	sinc := biblioteca.NovoSincronizador(
		biblioteca.NovaCuradoria(fora.URL), repo,
		slog.New(slog.DiscardHandler),
		biblioteca.ComEsperaEntreTentativas(0),
		biblioteca.ComSemente(itensDeTeste(), gerado),
	)

	pronto := make(chan struct{})
	sinc.Observar(func() { close(pronto) })
	go sinc.Manter(ctx)

	// Manter semeia e só então tenta varrer; a varredura vai falhar, e é o
	// sinal dela que diz que a semeadura já aconteceu.
	select {
	case <-pronto:
	case <-time.After(10 * time.Second):
		t.Fatal("a rotina não chegou a tentar a varredura")
	}

	_, total, err := repo.Buscar(context.Background(), biblioteca.Filtro{}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	if total != len(itensDeTeste()) {
		t.Fatalf("total = %d, quer %d: a semente não entrou", total, len(itensDeTeste()))
	}

	// A data é a da geração, não a de agora. Gravar "agora" deixaria a
	// instalação nova doze horas com o catálogo do dia do release achando que
	// está fresco — e a tela mentiria sobre a idade.
	estado, err := repo.Sincronizacao(context.Background())
	if err != nil {
		t.Fatalf("Sincronizacao: erro = %v, quer nil", err)
	}
	if estado.Idade() < 48*time.Hour {
		t.Errorf("idade = %s, quer a da semente (72h): a tela mentiria sobre a idade", estado.Idade())
	}
}

// TestSementeNaoSobrescreveCatalogoJaVarrido: a semente é do dia do release, e o
// que está no banco veio da rede. Andar para trás no reinício seria trocar dado
// novo por dado velho sem ninguém pedir.
func TestSementeNaoSobrescreveCatalogoJaVarrido(t *testing.T) {
	t.Parallel()

	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()

	repo := repoDeTeste(t)
	daRede := []biblioteca.Item{{
		Nome: "com.daRede/mcp", Titulo: "Veio da rede", Transporte: biblioteca.TransporteHTTP,
		URL: "https://darede.test/mcp",
	}}
	// Vencido de propósito: assim a rotina vai tentar varrer, e é o sinal
	// dessa tentativa que torna o teste determinístico. Com catálogo fresco ela
	// não faria nada e não haveria o que esperar.
	if err := repo.Substituir(context.Background(), daRede, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	fora := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora", http.StatusBadGateway)
	})
	sinc := biblioteca.NovoSincronizador(
		biblioteca.NovaCuradoria(fora.URL), repo,
		slog.New(slog.DiscardHandler),
		biblioteca.ComEsperaEntreTentativas(0),
		biblioteca.ComSemente(itensDeTeste(), time.Now().Add(-72*time.Hour)),
	)
	pronto := make(chan struct{})
	sinc.Observar(func() { close(pronto) })
	go sinc.Manter(ctx)
	select {
	case <-pronto:
	case <-time.After(10 * time.Second):
		t.Fatal("a rotina não rodou")
	}

	if _, err := repo.Um(context.Background(), "com.daRede/mcp"); err != nil {
		t.Fatalf("o que veio da rede sumiu: %v", err)
	}
	if _, total, _ := repo.Buscar(context.Background(), biblioteca.Filtro{}, 10, 0); total != 1 {
		t.Errorf("total = %d, quer 1: a semente sobrescreveu o catálogo varrido", total)
	}
}

// TestSementeVersionadaEValida lê a semente que está de fato embutida neste
// build.
//
// É guarda de release: uma semente corrompida ou gerada errado quebraria **toda
// instalação nova**, e o defeito só apareceria na máquina de quem instalou. Aqui
// ele aparece no `go test`.
func TestSementeVersionadaEValida(t *testing.T) {
	t.Parallel()

	itens, geradoEm, err := biblioteca.Semente()
	if err != nil {
		t.Fatalf("Semente: erro = %v, quer nil", err)
	}
	if len(itens) == 0 {
		t.Fatal("semente embutida vazia: CA-08 exige ao menos um item")
	}
	if geradoEm.IsZero() {
		t.Error("semente sem data: a tela mostraria idade errada e a varredura não seria disparada")
	}

	semComando, semDescricao, semSite := 0, 0, 0
	for _, i := range itens {
		if i.Nome == "" || i.Titulo == "" || i.Transporte == "" {
			t.Fatalf("item incompleto na semente: %+v", i)
		}
		// A semente só pode trazer a lista oficial: nome do registry (com.*,
		// io.github.*) nela seria a semente antiga, não a regerada por T-13.
		if !strings.HasPrefix(i.Nome, "mcpservers.org/") {
			t.Errorf("item com nome fora do padrão mcpservers.org/: %q", i.Nome)
		}
		if !i.Remoto() && i.Comando == "" {
			semComando++
		}
		if i.Descricao == "" {
			semDescricao++
		}
		if i.Site == "" {
			semSite++
		}
	}
	t.Logf("semente: %d servidores, gerada em %s, %d sem comando, %d sem descrição, %d sem site",
		len(itens), geradoEm.Format("2006-01-02"), semComando, semDescricao, semSite)
}

// TestSementeFrescaAindaAssimVarre é o guarda de um defeito que quase foi para
// produção.
//
// A semente é gerada no corte da versão, então numa instalação feita no mesmo
// dia ela tem horas de idade. Se a decisão de varrer olhasse só a idade, essa
// instalação passaria doze horas com os 651 da lista oficial que a semente
// traz — achando que o catálogo está completo, porque a tela diria "atualizado
// há 2 h". Semeou, varre.
func TestSementeFrescaAindaAssimVarre(t *testing.T) {
	t.Parallel()

	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()

	repo := repoDeTeste(t)
	// O acervo oficial de mentira: um índice que existe e a página de detalhe do
	// servidor que ele lista. É o que a varredura tem para trazer.
	origem := curadoriaDeMentira(t, nil)

	sinc := biblioteca.NovoSincronizador(
		biblioteca.NovaCuradoria(origem), repo,
		slog.New(slog.DiscardHandler),
		biblioteca.ComEsperaEntreTentativas(0),
		// Semente de agora mesmo: pela idade, ela não precisaria de varredura.
		biblioteca.ComSemente(itensDeTeste(), time.Now()),
	)
	pronto := make(chan struct{}, 2)
	sinc.Observar(func() { pronto <- struct{}{} })
	go sinc.Manter(ctx)

	select {
	case <-pronto:
	case <-time.After(15 * time.Second):
		t.Fatal("semeou e não varreu: a instalação nova ficaria com o catálogo parcial")
	}

	// O que está no banco agora veio da rede, não da semente: o nome carrega a
	// origem, e a semente não tem esse servidor.
	if _, err := repo.Um(context.Background(), "mcpservers.org/exemplo-oficial"); err != nil {
		t.Fatalf("o catálogo varrido não substituiu a semente: %v", err)
	}
}
