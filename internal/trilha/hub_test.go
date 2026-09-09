package trilha_test

import (
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/trilha"
)

// tetoDeEspera é quanto o teste espera por um sinal antes de declarar travamento.
// Generoso de propósito: ele existe para transformar deadlock em falha legível,
// não para medir desempenho. Compartilhado com os outros arquivos de teste
// deste pacote.
const tetoDeEspera = 10 * time.Second

func mensagemDeLog(texto string) trilha.Mensagem {
	return trilha.Mensagem{
		Tipo: trilha.TipoLog,
		Log:  trilha.LinhaLog{Instante: time.Now(), Nivel: "INFO", Mensagem: texto},
	}
}

// TestHub_EntregaAosDoisAssinantes: duas telas abertas veem o mesmo evento.
func TestHub_EntregaAosDoisAssinantes(t *testing.T) {
	t.Parallel()

	sut := trilha.NovoHub()
	primeira, fecharPrimeira := sut.Assinar()
	t.Cleanup(fecharPrimeira)
	segunda, fecharSegunda := sut.Assinar()
	t.Cleanup(fecharSegunda)

	if got := sut.Assinantes(); got != 2 {
		t.Fatalf("Assinantes() = %d, quer 2", got)
	}

	sut.Publicar(mensagemDeLog("endpoint materializado"))

	for nome, canal := range map[string]<-chan trilha.Mensagem{
		"primeira": primeira,
		"segunda":  segunda,
	} {
		m := receber(t, canal, nome)
		if m.Log.Mensagem != "endpoint materializado" {
			t.Errorf("%s recebeu %q, quer %q", nome, m.Log.Mensagem, "endpoint materializado")
		}
	}
}

// TestHub_AssinanteLentoNaoTravaOsOutros é a garantia que sustenta publicar do
// lado do produtor: a tela que parou de ler perde as linhas dela, e só as dela.
//
// A lenta nunca lê nada durante o teste. Se Publicar bloqueasse, a rápida não
// veria a mensagem seguinte e o teto de espera transformaria o travamento em
// falha legível.
func TestHub_AssinanteLentoNaoTravaOsOutros(t *testing.T) {
	t.Parallel()

	sut := trilha.NovoHub()
	lenta, fecharLenta := sut.Assinar()
	t.Cleanup(fecharLenta)
	rapida, fecharRapida := sut.Assinar()
	t.Cleanup(fecharRapida)

	// Bem acima da capacidade por assinante: a fila da lenta tem de encher e
	// transbordar várias vezes.
	const publicadas = trilha.CapacidadeAssinante * 3

	publicou := make(chan struct{})
	go func() {
		defer close(publicou)
		for i := 0; i < publicadas; i++ {
			sut.Publicar(mensagemDeLog("linha"))
			// A rápida lê a cada publicação; a lenta, nenhuma.
			receberSemEsperar(rapida)
		}
	}()

	select {
	case <-publicou:
	case <-time.After(tetoDeEspera):
		t.Fatal("Publicar bloqueou por causa do assinante que não lê")
	}

	if sut.Perdidas() == 0 {
		t.Error("Perdidas() = 0, quer mais de zero: a fila da lenta tinha de ter transbordado")
	}
	// A lenta ficou com exatamente a capacidade dela, e nada além.
	if got := len(lenta); got != trilha.CapacidadeAssinante {
		t.Errorf("fila da lenta = %d, quer %d (o teto por assinante)", got, trilha.CapacidadeAssinante)
	}
}

// TestHub_CancelarParaDeReceber: fechar a tela tira o assinante do fan-out.
func TestHub_CancelarParaDeReceber(t *testing.T) {
	t.Parallel()

	sut := trilha.NovoHub()
	canal, cancelar := sut.Assinar()

	cancelar()
	if got := sut.Assinantes(); got != 0 {
		t.Fatalf("Assinantes() = %d, quer 0 depois do cancelamento", got)
	}
	// Idempotente: um handler que sai por dois caminhos chama o cancelamento
	// duas vezes, e fechar o canal de novo entraria em panic.
	cancelar()

	sut.Publicar(mensagemDeLog("depois do cancelamento"))

	if _, aberto := <-canal; aberto {
		t.Error("canal do assinante cancelado ainda entrega mensagem, quer fechado")
	}
}

// TestHub_EncerrarSoltaOsHandlers prova o caminho de desligamento: sem ele o
// http.Server.Shutdown esperaria o prazo inteiro por conexões de SSE.
func TestHub_EncerrarSoltaOsHandlers(t *testing.T) {
	t.Parallel()

	sut := trilha.NovoHub()
	primeira, _ := sut.Assinar()
	segunda, _ := sut.Assinar()

	sut.Encerrar()
	// Duas vezes é seguro: quem desliga pode ser chamado de mais de um caminho.
	sut.Encerrar()

	for nome, canal := range map[string]<-chan trilha.Mensagem{
		"primeira": primeira,
		"segunda":  segunda,
	} {
		if _, aberto := <-canal; aberto {
			t.Errorf("canal de %s continua aberto depois de Encerrar", nome)
		}
	}

	// Quem assinar depois recebe um canal já fechado, e sai pelo mesmo caminho.
	tardia, cancelar := sut.Assinar()
	cancelar()
	if _, aberto := <-tardia; aberto {
		t.Error("assinatura feita depois de Encerrar veio aberta, quer fechada")
	}
	if got := sut.Assinantes(); got != 0 {
		t.Errorf("Assinantes() = %d, quer 0", got)
	}
}

func receber(t *testing.T, canal <-chan trilha.Mensagem, nome string) trilha.Mensagem {
	t.Helper()

	select {
	case m, aberto := <-canal:
		if !aberto {
			t.Fatalf("canal de %s fechado, quer uma mensagem", nome)
		}
		return m
	case <-time.After(tetoDeEspera):
		t.Fatalf("%s não recebeu nada em %v", nome, tetoDeEspera)
		return trilha.Mensagem{}
	}
}

func receberSemEsperar(canal <-chan trilha.Mensagem) {
	select {
	case <-canal:
	default:
	}
}
