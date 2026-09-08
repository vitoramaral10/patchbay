package stdioproc

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Estes testes são de caixa branca (package stdioproc, não stdioproc_test) de
// propósito: o que eles provam é a lógica de reconstrução de linha do
// coletor, e ela precisa de controle exato sobre onde cada Write corta o
// fluxo de bytes — algo que um processo real, do outro lado de um cano do
// sistema operacional, não garante. O SO pode entregar tudo de uma vez ou em
// pedaços, e o resultado final tem que ser o mesmo dos dois jeitos; aqui o
// teste escolhe o corte, em vez de torcer para o cano cooperar.

func coletar(t *testing.T) (*escritorDeLinhas, *[]string) {
	t.Helper()
	var linhas []string
	e := &escritorDeLinhas{escrever: func(s string) { linhas = append(linhas, s) }}
	return e, &linhas
}

func TestEscritorDeLinhas_DivideEntreDoisWrite(t *testing.T) {
	t.Parallel()

	e, linhas := coletar(t)
	escrever(t, e, "linha-")
	escrever(t, e, "dividida\n")

	quer := []string{"linha-dividida"}
	if !reflect.DeepEqual(*linhas, quer) {
		t.Errorf("linhas = %q, quer %q", *linhas, quer)
	}
}

func TestEscritorDeLinhas_FimDeLinhaDoWindows(t *testing.T) {
	t.Parallel()

	e, linhas := coletar(t)
	escrever(t, e, "linha-crlf\r\n")

	quer := []string{"linha-crlf"}
	if !reflect.DeepEqual(*linhas, quer) {
		t.Errorf("linhas = %q, quer %q", *linhas, quer)
	}
}

func TestEscritorDeLinhas_LinhaVaziaNaoEmiteNada(t *testing.T) {
	t.Parallel()

	e, linhas := coletar(t)
	escrever(t, e, "\n")
	escrever(t, e, "\r\n")

	if len(*linhas) != 0 {
		t.Errorf("linhas = %q, quer nenhuma", *linhas)
	}
}

// TestEscritorDeLinhas_CorteEResincronizacao é o item 2b: uma linha que passa
// do teto antes do \n chegar tem que ser truncada com o sufixo, e o que vier
// depois do \n dela tem que ser a linha seguinte inteira — não o resto do que
// já foi truncado colado no começo dela, que era o bug antigo.
func TestEscritorDeLinhas_CorteEResincronizacao(t *testing.T) {
	t.Parallel()

	e, linhas := coletar(t)

	// Uma linha sem \n que já nasce maior que o teto: dispara a truncagem
	// mesmo sem o \n ter chegado ainda.
	marcador := "INICIO-"
	longa := marcador + strings.Repeat("a", limiteDeLinha+50)
	escrever(t, e, longa)

	// O resto da linha longa, que o coletor tem que descartar até o \n dela.
	escrever(t, e, strings.Repeat("b", 30)+"\n")

	// E a linha seguinte, que precisa chegar intacta.
	escrever(t, e, "depois\n")

	if len(*linhas) != 2 {
		t.Fatalf("linhas emitidas = %d, quer 2 (a truncada e a seguinte); got %q", len(*linhas), *linhas)
	}
	if !strings.HasPrefix((*linhas)[0], marcador) {
		t.Errorf("primeira linha = %q, quer prefixo %q preservado", (*linhas)[0], marcador)
	}
	if !strings.HasSuffix((*linhas)[0], sufixoTruncado) {
		t.Errorf("primeira linha = %q, quer sufixo %q", (*linhas)[0], sufixoTruncado)
	}
	if len((*linhas)[0]) != limiteDeLinha+len(sufixoTruncado) {
		t.Errorf("tamanho da primeira linha = %d, quer %d (teto + sufixo)",
			len((*linhas)[0]), limiteDeLinha+len(sufixoTruncado))
	}
	if (*linhas)[1] != "depois" {
		t.Errorf("segunda linha = %q, quer %q: a ressincronização colou o resto descartado nela", (*linhas)[1], "depois")
	}
}

// TestEscritorDeLinhas_TetoDeLinhasPorProcesso é o item 2a: passado o teto, o
// coletor para de emitir linha a linha e resume o resto numa única linha de
// aviso.
func TestEscritorDeLinhas_TetoDeLinhasPorProcesso(t *testing.T) {
	t.Parallel()

	e, linhas := coletar(t)

	const total = limiteDeLinhasPorProcesso + 50
	for i := range total {
		escrever(t, e, "linha-"+strconv.Itoa(i)+"\n")
	}

	// As primeiras limiteDeLinhasPorProcesso, intactas...
	if len(*linhas) != limiteDeLinhasPorProcesso+1 {
		t.Fatalf("linhas emitidas = %d, quer %d (o teto + um aviso de resumo)",
			len(*linhas), limiteDeLinhasPorProcesso+1)
	}
	for i := range limiteDeLinhasPorProcesso {
		quer := "linha-" + strconv.Itoa(i)
		if (*linhas)[i] != quer {
			t.Fatalf("linha %d = %q, quer %q", i, (*linhas)[i], quer)
		}
	}
	// ...e só então o resumo — uma única vez, na primeira linha suprimida: as
	// 49 seguintes só contam, sem repetir o aviso a cada uma (intervaloDeAvisoDeSupressao
	// é o que decide a próxima repetição, bem mais adiante que 50).
	ultima := (*linhas)[limiteDeLinhasPorProcesso]
	if !strings.Contains(ultima, "suprimidas") {
		t.Errorf("linha de resumo = %q, quer mencionar linhas suprimidas", ultima)
	}
}

func escrever(t *testing.T, e *escritorDeLinhas, s string) {
	t.Helper()
	if _, err := e.Write([]byte(s)); err != nil {
		t.Fatalf("Write(%q) = %v, quer nil", s, err)
	}
}
