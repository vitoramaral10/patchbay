package endpoint_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/endpoint"
)

// relogioFalso põe a janela de graça sob controle do teste: "passou o prazo"
// vira uma chamada de método, não cinco minutos de espera.
type relogioFalso struct {
	mu    sync.Mutex
	agora time.Time
}

func novoRelogioFalso() *relogioFalso {
	return &relogioFalso{agora: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

func (r *relogioFalso) Agora() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agora
}

func (r *relogioFalso) avancar(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agora = r.agora.Add(d)
}

func chamar(t *testing.T, sessao *mcp.ClientSession, nome string) *mcp.CallToolResult {
	t.Helper()

	res, err := sessao.CallTool(context.Background(), &mcp.CallToolParams{Name: nome})
	if err != nil {
		t.Fatalf("tools/call %s: erro = %v, quer nil", nome, err)
	}
	return res
}

func textoDe(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	var b strings.Builder
	for _, c := range res.Content {
		if texto, ok := c.(*mcp.TextContent); ok {
			b.WriteString(texto.Text)
		}
	}
	return b.String()
}

// TestServidores_LapideRespondeAteAJanelaVencer é o comportamento que a seção
// 08.2 pede: a ferramenta que sai do catálogo continua registrada por uma janela
// de graça e explica que saiu, em vez de o cliente receber unknown tool e
// concluir que o servidor inteiro está quebrado.
func TestServidores_LapideRespondeAteAJanelaVencer(t *testing.T) {
	t.Parallel()

	const janela = 5 * time.Minute
	rel := novoRelogioFalso()
	sut, _, cat := montarCom(t,
		[]endpoint.Opcao{endpoint.ComJanelaDeGraca(janela), endpoint.ComRelogio(rel)},
		endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, "alfa", "beta")
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	// O upstream de alfa degradou: o catálogo agora só traz beta.
	cat.definir("beta")
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"alfa", "beta"}) {
		t.Fatalf("ferramentas = %v, quer [alfa beta]; alfa devia continuar como lápide", nomes)
	}
	if lapides := sut.Lapides(slugDeTeste); !slices.Equal(lapides, []string{"alfa"}) {
		t.Errorf("lápides = %v, quer [alfa]", lapides)
	}
	// A contagem da tela é de ferramenta que funciona: lápide não conta.
	if n := sut.Contagem(slugDeTeste); n != 1 {
		t.Errorf("contagem = %d, quer 1; lápide não é ferramenta que funciona", n)
	}

	res := chamar(t, sessao, "alfa")
	if !res.IsError {
		t.Error("chamada da lápide = sucesso, quer erro de ferramenta")
	}
	if texto := textoDe(t, res); !strings.Contains(texto, "alfa") || !strings.Contains(texto, "tools/list") {
		t.Errorf("texto da lápide = %q, quer o nome da ferramenta e o que fazer a seguir", texto)
	}

	// Vencida a janela, a varredura recolhe e o tools/list fica honesto.
	rel.avancar(janela + time.Second)
	sut.VarrerLapides()

	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"beta"}) {
		t.Errorf("ferramentas = %v, quer [beta] depois da janela", nomes)
	}
	if lapides := sut.Lapides(slugDeTeste); len(lapides) != 0 {
		t.Errorf("lápides = %v, quer nenhuma depois da janela", lapides)
	}
}

// TestServidores_FerramentaQueVoltaAntesDaJanelaVoltaDeVerdade: o vai-e-vem de um
// upstream que oscila é o caso normal, não a exceção. A ferramenta que volta tem
// que voltar funcionando — se a lápide sobrevivesse, o cliente passaria a receber
// "ela saiu" sobre uma ferramenta que está de pé.
func TestServidores_FerramentaQueVoltaAntesDaJanelaVoltaDeVerdade(t *testing.T) {
	t.Parallel()

	rel := novoRelogioFalso()
	sut, _, cat := montarCom(t,
		[]endpoint.Opcao{endpoint.ComJanelaDeGraca(5 * time.Minute), endpoint.ComRelogio(rel)},
		endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, "alfa")
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	cat.definir()
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar com upstream degradado: erro = %v, quer nil", err)
	}
	if res := chamar(t, sessao, "alfa"); !res.IsError {
		t.Fatal("chamada da lápide = sucesso, quer erro de ferramenta")
	}

	rel.avancar(time.Minute)
	cat.definir("alfa")
	if err := sut.Sincronizar(context.Background()); err != nil {
		t.Fatalf("Sincronizar com upstream de volta: erro = %v, quer nil", err)
	}

	if lapides := sut.Lapides(slugDeTeste); len(lapides) != 0 {
		t.Errorf("lápides = %v, quer nenhuma; a ferramenta voltou", lapides)
	}
	res := chamar(t, sessao, "alfa")
	if res.IsError {
		t.Errorf("chamada = erro (%q), quer sucesso: a lápide sobreviveu à volta", textoDe(t, res))
	}
	// Passar da janela agora não pode remover nada: não existe lápide pendente.
	rel.avancar(10 * time.Minute)
	sut.VarrerLapides()
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"alfa"}) {
		t.Errorf("ferramentas = %v, quer [alfa]", nomes)
	}
}

// TestServidores_CatalogoParcialContinuaServindo é o critério da fatia: endpoint
// com dois upstreams e um degradado serve as ferramentas do que está de pé, e o
// tools/list vazio é resposta legítima quando nenhum está — nunca erro, porque
// erro no tools/list faz o cliente marcar o endpoint inteiro como quebrado.
func TestServidores_CatalogoParcialContinuaServindo(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		catalogo []string
		quer     []string
	}{
		"os dois upstreams prontos": {
			catalogo: []string{"notion_buscar", "drive_listar"},
			quer:     []string{"drive_listar", "notion_buscar"},
		},
		"um upstream degradado, o outro continua servindo": {
			catalogo: []string{"notion_buscar"},
			quer:     []string{"notion_buscar"},
		},
		"nenhum upstream pronto devolve lista vazia": {
			catalogo: nil,
			quer:     []string{},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut, _, _ := montarCom(t,
				[]endpoint.Opcao{endpoint.ComJanelaDeGraca(0), endpoint.ComRelogio(novoRelogioFalso())},
				endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"}, tc.catalogo...)
			sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

			if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, tc.quer) {
				t.Fatalf("ferramentas = %v, quer %v", nomes, tc.quer)
			}
			if len(tc.quer) == 0 {
				return
			}
			if res := chamar(t, sessao, tc.quer[0]); res.IsError {
				t.Errorf("chamada de %s = erro (%q), quer sucesso", tc.quer[0], textoDe(t, res))
			}
		})
	}
}

// TestServidores_UpstreamQueDegradaEVoltaRepoeOCatalogo percorre a sequência que
// o admin vê: o upstream cai, o endpoint continua no ar servindo o resto, e a
// volta rematerializa sem que ninguém reinicie nada.
func TestServidores_UpstreamQueDegradaEVoltaRepoeOCatalogo(t *testing.T) {
	t.Parallel()

	sut, _, cat := montarCom(t,
		[]endpoint.Opcao{endpoint.ComJanelaDeGraca(0), endpoint.ComRelogio(novoRelogioFalso())},
		endpoint.Registro{ID: 1, Slug: slugDeTeste, Nome: "Pessoal"},
		"notion_buscar", "drive_listar")
	sessao := ligarCliente(t, sut.Servidor(slugDeTeste), nil)

	sincronizar := func(nomes ...string) {
		t.Helper()
		cat.definir(nomes...)
		if err := sut.Sincronizar(context.Background()); err != nil {
			t.Fatalf("Sincronizar: erro = %v, quer nil", err)
		}
	}

	// O upstream do drive degrada: as ferramentas dele saem, as do notion ficam.
	sincronizar("notion_buscar")
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"notion_buscar"}) {
		t.Fatalf("ferramentas com um upstream degradado = %v, quer [notion_buscar]", nomes)
	}
	if res := chamar(t, sessao, "notion_buscar"); res.IsError {
		t.Errorf("chamada = erro (%q), quer sucesso: o catálogo parcial tem que funcionar", textoDe(t, res))
	}

	// Os dois degradam: lista vazia, e a sessão do cliente continua de pé.
	sincronizar()
	if nomes := nomesDoServidor(t, sessao); len(nomes) != 0 {
		t.Fatalf("ferramentas sem upstream pronto = %v, quer nenhuma", nomes)
	}

	// O drive volta primeiro.
	sincronizar("drive_listar")
	if nomes := nomesDoServidor(t, sessao); !slices.Equal(nomes, []string{"drive_listar"}) {
		t.Fatalf("ferramentas depois da volta parcial = %v, quer [drive_listar]", nomes)
	}
	if res := chamar(t, sessao, "drive_listar"); res.IsError {
		t.Errorf("chamada depois da volta = erro (%q), quer sucesso", textoDe(t, res))
	}
}
