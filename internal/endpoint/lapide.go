package endpoint

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
)

// IntervaloVarreduraLapide é a frequência com que as lápides vencidas são
// recolhidas.
//
// Não precisa ser fina: a lápide vencida é uma ferramenta a mais no tools/list
// por alguns segundos, e uma varredura por segundo custaria mais que o que
// evita. Quem recolhe na hora certa é a rematerialização, que já roda a cada
// mudança de catálogo.
const IntervaloVarreduraLapide = 30 * time.Second

// Relogio é o mínimo do tempo que este pacote consome: só o instante atual,
// para decidir qual lápide venceu.
//
// Declarado aqui, no consumidor, e injetado, para que o teste da janela de
// graça não espere cinco minutos de relógio de verdade.
type Relogio interface {
	Agora() time.Time
}

type relogioReal struct{}

func (relogioReal) Agora() time.Time { return time.Now() }

// Opcao ajusta o registro de endpoints na construção.
type Opcao func(*Servidores)

// ComJanelaDeGraca troca quanto tempo uma ferramenta removida continua
// registrada como lápide. Zero ou negativo desliga a lápide: a ferramenta sai
// do catálogo na hora, e o cliente que ainda não relistou recebe unknown tool.
func ComJanelaDeGraca(d time.Duration) Opcao {
	return func(s *Servidores) { s.janela = d }
}

// ComRelogio troca o relógio que decide qual lápide venceu.
func ComRelogio(r Relogio) Opcao {
	return func(s *Servidores) {
		if r != nil {
			s.relogio = r
		}
	}
}

// lapide é uma ferramenta que saiu do catálogo e ainda responde.
type lapide struct {
	// upstream é de onde ela vinha, para a mensagem dizer o que sumiu.
	upstream string
	// expiraEm é quando ela sai do tools/list de vez.
	expiraEm time.Time
}

// aplicarLapides troca por lápide o que saiu do catálogo e recolhe as vencidas.
//
// Roda dentro de v.materializacao, como o resto de rematerializar: o conjunto de
// lápides é calculado e aplicado num passo só, e todo escritor deste mapa passa
// por aqui ou pela varredura, que pega o mesmo mutex.
func (s *Servidores) aplicarLapides(srv *mcp.Server, v *vivo, slug string, antigos []string, origensAntigas map[string]string, novos []string) {
	sumiram := diferenca(antigos, novos)

	// Janela desligada: comportamento antigo, a ferramenta sai na hora.
	if s.janela <= 0 {
		if len(sumiram) > 0 {
			srv.RemoveTools(sumiram...)
			s.log.Info("ferramentas removidas do endpoint",
				"endpoint", slug, "ferramentas", sumiram)
		}
		return
	}

	s.mu.RLock()
	pendentes := maps.Clone(v.lapides)
	s.mu.RUnlock()
	if pendentes == nil {
		pendentes = make(map[string]lapide)
	}

	// Ferramenta que voltou deixa de ser lápide: o AddTool da de verdade já
	// sobrescreveu o registro com o schema e o handler certos.
	voltaram := 0
	for _, nome := range novos {
		if _, era := pendentes[nome]; era {
			delete(pendentes, nome)
			voltaram++
		}
	}

	agora := s.relogio.Agora()
	var novas []string
	for _, nome := range sumiram {
		if _, ja := pendentes[nome]; ja {
			continue
		}
		// A origem vem da materialização anterior: é de lá que a ferramenta
		// saiu, e é esse upstream que a mensagem precisa nomear.
		upstreamNome := origensAntigas[nome]
		if !s.registrarLapide(srv, nome, upstreamNome) {
			// Não deu para registrar a lápide: melhor tirar a ferramenta do que
			// deixar registrado um handler que aponta para uma sessão morta.
			srv.RemoveTools(nome)
			continue
		}
		pendentes[nome] = lapide{upstream: upstreamNome, expiraEm: agora.Add(s.janela)}
		novas = append(novas, nome)
	}

	vencidas := vencidasEm(pendentes, agora)
	for _, nome := range vencidas {
		delete(pendentes, nome)
	}
	if len(vencidas) > 0 {
		srv.RemoveTools(vencidas...)
	}

	s.mu.Lock()
	// Só grava se o servidor não foi trocado embaixo por uma recriação: nesse
	// caso as lápides calculadas são de outra instância.
	if v.servidor == srv {
		v.lapides = pendentes
	}
	s.mu.Unlock()

	if len(novas) > 0 {
		s.log.Info("ferramentas com lápide no endpoint",
			"endpoint", slug, "ferramentas", novas, "janela_s", int(s.janela.Seconds()))
	}
	if len(vencidas) > 0 {
		s.log.Info("ferramentas removidas do endpoint",
			"endpoint", slug, "ferramentas", vencidas)
	}
	if voltaram > 0 {
		s.log.Info("ferramentas voltaram antes da janela de graça",
			"endpoint", slug, "ferramentas", voltaram)
	}
}

// registrarLapide troca a ferramenta pelo handler que explica que ela saiu.
//
// Mesmo recover individual do registrar: um AddTool que entre em panic aqui
// custaria uma ferramenta, nunca o processo.
func (s *Servidores) registrarLapide(srv *mcp.Server, nome, upstreamNome string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			s.log.Error("AddTool entrou em panic com lápide de ferramenta",
				"ferramenta", nome, "upstream", upstreamNome, "panic", r)
		}
	}()
	srv.AddTool(catalogo.Lapide(nome, upstreamNome),
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return catalogo.ResultadoDeLapide(nome, upstreamNome), nil
		})
	return true
}

// VarrerLapides recolhe as lápides vencidas de todos os endpoints no ar.
//
// Exportada porque é o gatilho do teste da janela de graça: com o relógio
// injetado, "passou o prazo" vira uma chamada, não uma espera.
func (s *Servidores) VarrerLapides() {
	s.mu.RLock()
	vivos := slices.Collect(maps.Values(s.porSlug))
	s.mu.RUnlock()

	for _, v := range vivos {
		s.varrerLapidesDe(v)
	}
}

func (s *Servidores) varrerLapidesDe(v *vivo) {
	v.materializacao.Lock()
	defer v.materializacao.Unlock()

	s.mu.RLock()
	srv, slug := v.servidor, v.reg.Slug
	pendentes := maps.Clone(v.lapides)
	s.mu.RUnlock()

	vencidas := vencidasEm(pendentes, s.relogio.Agora())
	if len(vencidas) == 0 {
		return
	}
	srv.RemoveTools(vencidas...)

	s.mu.Lock()
	if v.servidor == srv {
		for _, nome := range vencidas {
			delete(v.lapides, nome)
		}
	}
	s.mu.Unlock()

	s.log.Info("lápides de ferramenta recolhidas", "endpoint", slug, "ferramentas", vencidas)
}

// VigiarLapides recolhe as lápides vencidas até o ctx ser cancelado. É a
// goroutine de fundo que main sobe; quem chama é dono dela.
func (s *Servidores) VigiarLapides(ctx context.Context) {
	if s.janela <= 0 {
		return
	}
	tique := time.NewTicker(IntervaloVarreduraLapide)
	defer tique.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tique.C:
			s.VarrerLapides()
		}
	}
}

// Lapides devolve os nomes que o endpoint ainda serve só para explicar que
// saíram, em ordem.
func (s *Servidores) Lapides(slug string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.porSlug[slug]
	if !ok {
		return nil
	}
	return slices.Sorted(maps.Keys(v.lapides))
}

// vencidasEm devolve, em ordem, as lápides cujo prazo já passou.
func vencidasEm(pendentes map[string]lapide, agora time.Time) []string {
	var out []string
	for nome, l := range pendentes {
		if !agora.Before(l.expiraEm) {
			out = append(out, nome)
		}
	}
	slices.Sort(out)
	return out
}
