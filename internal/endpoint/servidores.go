package endpoint

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/platform/versao"
)

// Servidores guarda um *mcp.Server por endpoint e mantém o catálogo de cada um
// atualizado.
type Servidores struct {
	repo Repositorio
	cat  Catalogo
	exec Executor
	log  *slog.Logger

	mu      sync.RWMutex
	porSlug map[string]*vivo
}

// vivo é um endpoint no ar.
//
// Cada endpoint tem o *seu* StreamableHTTPHandler, e não um compartilhado por
// toda a rota /mcp/{endpoint}. O handler do go-sdk v1.7.0 resolve a sessão só
// por Mcp-Session-Id + TokenInfo.UserID (mcp/streamable.go:559-574 e :633-645) e
// nunca olha o path: com um handler só, uma chave com escopo em dois endpoints
// abriria sessão em /mcp/a e reusaria o mesmo Mcp-Session-Id em /mcp/b — o
// middleware validaria o escopo de b, e quem atenderia seria o *mcp.Server de a.
// Um handler por endpoint dá a cada um a sua tabela de sessões, e o isolamento
// deixa de depender de o SDK olhar o path.
type vivo struct {
	// materializacao serializa cálculo e aplicação do catálogo deste endpoint.
	//
	// AoMudar é disparado por uma goroutine de supervisão por upstream, então
	// dois upstreams que ficam prontos quase juntos rematerializam em paralelo.
	// Sem este mutex, o snapshot de "quais ferramentas existem agora" é calculado
	// por um e aplicado depois do outro: sobra ferramenta morta registrada, ou
	// sai ferramenta viva.
	materializacao sync.Mutex

	reg      Registro
	servidor *mcp.Server
	handler  http.Handler
	expostos []string // nomes registrados, para saber o que remover
}

// NovoServidores monta o registro de endpoints.
func NovoServidores(repo Repositorio, cat Catalogo, exec Executor, log *slog.Logger) *Servidores {
	return &Servidores{
		repo:    repo,
		cat:     cat,
		exec:    exec,
		log:     log,
		porSlug: make(map[string]*vivo),
	}
}

// Sincronizar acerta o conjunto de endpoints no ar com o banco e rematerializa o
// catálogo de todos.
//
// É chamada no boot, a cada mudança de catálogo de upstream e depois de toda
// escrita da UI. Endpoint que já existe mantém a mesma instância de *mcp.Server
// para que as sessões retidas dos clientes sobrevivam à rematerialização.
//
// Rematerializa todos, e não só o endpoint que mudou, porque um upstream compõe
// vários endpoints: o conjunto afetado por uma mudança de upstream é "todo
// endpoint que o inclui", e descobrir isso custa a mesma consulta que
// rematerializar. O preço é um tools/list_changed a mais em endpoint que não
// mudou, e o SDK já faz debounce dessa notificação (mcp/server.go:699).
func (s *Servidores) Sincronizar(ctx context.Context) error {
	regs, err := s.repo.Todos(ctx)
	if err != nil {
		return fmt.Errorf("endpoint: listar: %w", err)
	}

	vivos, aposentados := s.reconciliar(regs)
	for _, ap := range aposentados {
		s.fecharSessoes(ap.servidor, ap.slug, ap.motivo)
	}

	var erros []error
	for _, v := range vivos {
		if err := s.rematerializar(ctx, v); err != nil {
			erros = append(erros, err)
		}
	}
	if len(erros) > 0 {
		return fmt.Errorf("endpoint: rematerializar: %w", erros[0])
	}
	return nil
}

// aposentado é um *mcp.Server que saiu de serviço e cujas sessões precisam ser
// fechadas fora do lock.
type aposentado struct {
	servidor *mcp.Server
	slug     string
	motivo   string
}

// reconciliar acerta o mapa com o banco sob o lock e devolve o que rematerializar
// e o que aposentar.
func (s *Servidores) reconciliar(regs []Registro) (vivos []*vivo, aposentados []aposentado) {
	s.mu.Lock()
	defer s.mu.Unlock()

	presentes := make(map[string]bool, len(regs))
	for _, reg := range regs {
		presentes[reg.Slug] = true

		v, existe := s.porSlug[reg.Slug]
		if !existe {
			s.porSlug[reg.Slug] = s.novoVivo(reg)
			s.log.Info("endpoint no ar", "endpoint", reg.Slug)
			continue
		}
		if reg.identidadeMudou(v.reg) {
			// Nome, descrição e instruções entram no *mcp.Server na construção e
			// são lidos do struct de opções no initialize (mcp/server.go:906 e
			// :1987): o SDK não oferece como trocá-los numa instância viva. Por
			// isso editar esses três campos recria o servidor e fecha as sessões
			// daquele endpoint — o cliente reconecta e passa a ver o texto novo.
			// Mudar a composição não passa por aqui: aquilo é tools/list_changed.
			aposentados = append(aposentados, aposentado{
				servidor: v.servidor, slug: v.reg.Slug,
				motivo: "identidade do endpoint editada",
			})
			v.servidor = s.novoServidorMCP(reg)
			v.handler = s.novoHandler(v.servidor)
			v.expostos = nil
		}
		v.reg = reg
	}

	// Endpoint apagado do banco sai do ar. A janela de graça de ferramenta
	// removida (lápide) é da fatia 3 e vale para ferramenta, não para endpoint.
	for slug, v := range s.porSlug {
		if presentes[slug] {
			continue
		}
		delete(s.porSlug, slug)
		aposentados = append(aposentados, aposentado{
			servidor: v.servidor, slug: slug, motivo: "endpoint removido",
		})
		s.log.Info("endpoint fora do ar", "endpoint", slug)
	}

	vivos = slices.Collect(maps.Values(s.porSlug))
	slices.SortFunc(vivos, func(a, b *vivo) int { return cmp.Compare(a.reg.Slug, b.reg.Slug) })
	return vivos, aposentados
}

func (s *Servidores) novoVivo(reg Registro) *vivo {
	srv := s.novoServidorMCP(reg)
	return &vivo{reg: reg, servidor: srv, handler: s.novoHandler(srv)}
}

func (s *Servidores) novoServidorMCP(reg Registro) *mcp.Server {
	return mcp.NewServer(
		&mcp.Implementation{
			Name:    "patchbay/" + reg.Slug,
			Version: versao.Numero,
			Title:   reg.Titulo(),
		},
		&mcp.ServerOptions{
			Logger:       s.log.With("componente", "servidor_mcp", "endpoint", reg.Slug),
			Instructions: reg.Instrucoes,
		},
	)
}

// fecharSessoes encerra as sessões de um servidor aposentado.
//
// Sem isto, trocar ou remover um endpoint deixaria a sessão viva pendurada no
// mapa do handler antigo, com o transporte aberto e o cliente achando que ainda
// fala com um endpoint que não existe. Fechar a ServerSession também a tira da
// tabela do StreamableHTTPHandler, que é quem a registrou.
func (s *Servidores) fecharSessoes(srv *mcp.Server, slug, motivo string) {
	if srv == nil {
		return
	}
	encerradas := 0
	for sessao := range srv.Sessions() {
		if err := sessao.Close(); err != nil {
			s.log.Debug("erro ao fechar sessão de endpoint",
				"endpoint", slug, "erro", err)
		}
		encerradas++
	}
	if encerradas > 0 {
		s.log.Info("sessões de endpoint encerradas",
			"endpoint", slug, "sessoes", encerradas, "motivo", motivo)
	}
}

// rematerializar troca o conjunto de ferramentas de um endpoint.
//
// Todo o corpo roda sob v.materializacao: ler a composição, normalizar, registrar
// e remover o que sobrou são um só passo. Fora do mutex, dois chamadores
// concorrentes calculariam listas diferentes e a última escrita venceria.
func (s *Servidores) rematerializar(ctx context.Context, v *vivo) error {
	v.materializacao.Lock()
	defer v.materializacao.Unlock()

	s.mu.RLock()
	reg, srv, antigos := v.reg, v.servidor, v.expostos
	s.mu.RUnlock()

	ferramentas, err := s.cat.Materializar(ctx, reg.ID)
	if err != nil {
		return fmt.Errorf("endpoint %s: %w", reg.Slug, err)
	}

	novos := make([]string, 0, len(ferramentas))
	for _, f := range ferramentas {
		if s.registrar(srv, f) {
			novos = append(novos, f.NomeExposto())
		}
	}

	// O que saiu do catálogo é removido. AddTool e RemoveTools passam por
	// changeAndNotify, que dispara tools/list_changed com debounce
	// (mcp/server.go:699).
	if sumiram := diferenca(antigos, novos); len(sumiram) > 0 {
		srv.RemoveTools(sumiram...)
		s.log.Info("ferramentas removidas do endpoint",
			"endpoint", reg.Slug, "ferramentas", sumiram)
	}

	s.mu.Lock()
	// Só grava se o servidor não foi trocado embaixo por uma recriação: nesse
	// caso o conjunto calculado é de outra instância e não descreve esta.
	if v.servidor == srv {
		v.expostos = novos
	}
	s.mu.Unlock()

	s.log.Info("endpoint materializado", "endpoint", reg.Slug, "ferramentas", len(novos))
	return nil
}

// registrar chama AddTool com recover individual.
//
// Cinturão além do suspensório: o normalizador já cobre os caminhos de panic
// conhecidos do AddTool (mcp/server.go:281-313), mas se um caminho novo
// aparecer num bump do SDK o custo é uma ferramenta, não o processo.
func (s *Servidores) registrar(srv *mcp.Server, f catalogo.Ferramenta) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			s.log.Error("AddTool entrou em panic com ferramenta normalizada",
				"ferramenta", f.NomeExposto(), "upstream", f.UpstreamNome, "panic", r)
		}
	}()
	srv.AddTool(f.Tool, s.encaminhar(f))
	return true
}

// encaminhar devolve o handler que executa a ferramenta no upstream.
//
// O handler fecha sobre o upstream e o nome original, então a resolução do nome
// não é uma busca: prefixo e renome custam zero por chamada e colisão de nome é
// impossível por construção.
func (s *Servidores) encaminhar(f catalogo.Ferramenta) mcp.ToolHandler {
	upstreamID, nomeOriginal, upstreamNome := f.UpstreamID, f.NomeOriginal, f.UpstreamNome
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args json.RawMessage
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}
		res, err := s.exec.Chamar(ctx, upstreamID, nomeOriginal, args)
		if err != nil {
			// Erro de ferramenta, não erro de protocolo: o cliente precisa
			// saber que esta chamada falhou sem concluir que o endpoint todo
			// está quebrado.
			s.log.Warn("chamada de ferramenta falhou",
				"upstream", upstreamNome, "ferramenta", nomeOriginal, "erro", err)
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{
					Text: fmt.Sprintf("patchbay: upstream %q não atendeu a chamada de %q", upstreamNome, nomeOriginal),
				}},
			}, nil
		}
		return res, nil
	}
}

// Servidor devolve o *mcp.Server do slug, ou nil se o endpoint não existe.
func (s *Servidores) Servidor(slug string) *mcp.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.porSlug[slug]
	if !ok {
		return nil
	}
	return v.servidor
}

// Existe informa se o slug está no ar.
func (s *Servidores) Existe(slug string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.porSlug[slug]
	return ok
}

// Contagem devolve quantas ferramentas o endpoint está expondo agora.
//
// É o número que a seção 11 exige ao lado de cada endpoint: um endpoint que passa
// de 40 ferramentas degrada todos os clientes de uma vez, e sem o número na tela
// isso acontece sem ninguém ver.
func (s *Servidores) Contagem(slug string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.porSlug[slug]
	if !ok {
		return 0
	}
	return len(v.expostos)
}

// Expostos devolve os nomes que o endpoint expõe agora, em ordem.
func (s *Servidores) Expostos(slug string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.porSlug[slug]
	if !ok {
		return nil
	}
	return slices.Sorted(slices.Values(v.expostos))
}

// Slugs devolve os endpoints no ar, em ordem.
func (s *Servidores) Slugs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.porSlug))
}

func diferenca(antes, depois []string) []string {
	agora := make(map[string]bool, len(depois))
	for _, n := range depois {
		agora[n] = true
	}
	var sumiram []string
	for _, n := range antes {
		if !agora[n] {
			sumiram = append(sumiram, n)
		}
	}
	return sumiram
}
