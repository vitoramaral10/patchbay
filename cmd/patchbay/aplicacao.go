package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/vitoramaral10/patchbay/internal/admin"
	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// timeoutSincronizacao limita a rematerialização disparada por mudança de
// catálogo de upstream. É escrita nenhuma e leitura curta; se passar disso, o
// banco está travado e insistir só piora.
const timeoutSincronizacao = 10 * time.Second

// intervaloLimpezaSessao é a frequência da varredura de sessões de admin
// vencidas. Sessão vencida já não autentica nada — a varredura só evita que a
// tabela cresça para sempre.
const intervaloLimpezaSessao = time.Hour

// Aplicacao é o grafo de dependências montado. Só este arquivo o conhece
// inteiro — as features não se importam entre si.
type Aplicacao struct {
	cfg   Config
	log   *slog.Logger
	st    *store.Store
	chave *apikey.Servico
	adm   *admin.Servico

	gerente   *upstream.Gerente
	endpoints *endpoint.Servidores

	repoUpstream *upstream.RepositorioSQLite
	repoEndpoint *endpoint.RepositorioSQLite
	repoChave    *apikey.RepositorioSQLite

	admHTTP    *admin.HTTP
	adminUp    *upstream.Admin
	adminEnd   *endpoint.Admin
	adminChave *apikey.Admin

	mu         sync.Mutex
	observados []func()

	wg sync.WaitGroup
}

// montar abre o banco, aplica as migrações e liga os componentes.
//
// O cofre entra por parâmetro e não pela Config: a Config é impressa em log de
// boot e vai inteira para os testes, e a chave mestra não pode passar por lá.
func montar(ctx context.Context, cfg Config, cofre *cripto.Cofre, log *slog.Logger) (*Aplicacao, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("criar diretório de dados %s: %w", cfg.DataDir, err)
	}
	st, err := store.Abrir(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}

	a := &Aplicacao{cfg: cfg, log: log, st: st}
	leitura, escrita := st.Leitura(), st.Escrita()

	// Antes de qualquer componente: se o canário não confere, a chave mestra
	// mudou e nada do que está cifrado neste banco volta.
	if err := verificarCanario(ctx, cofre, leitura, escrita, log); err != nil {
		_ = st.Close()
		return nil, err
	}

	a.repoUpstream = upstream.NovoRepositorioSQLite(leitura, escrita, cofre)
	a.repoEndpoint = endpoint.NovoRepositorioSQLite(leitura, escrita)
	a.repoChave = apikey.NovoRepositorioSQLite(leitura, escrita)

	a.chave = apikey.NovoServico(a.repoChave, log.With("componente", "apikey"))
	a.adm = admin.NovoServico(
		admin.NovoRepositorioSQLite(leitura, escrita),
		log.With("componente", "admin"),
	)

	cfgs, err := upstream.Habilitados(ctx, leitura)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	a.gerente = upstream.NovoGerente(
		log.With("componente", "upstream"), cfgs,
		upstream.AoMudar(a.sincronizar),
		// As credenciais estáticas são lidas do banco a cada conexão, e não
		// guardadas na Config: assim trocar o bearer pela tela vale na
		// reconexão seguinte, sem cache a invalidar e sem segredo passeando
		// pela estrutura que alimenta a UI.
		upstream.ComCredenciais(a.repoUpstream.Credenciais),
	)

	cat := catalogo.NovoServico(
		catalogo.NovaComposicaoSQLite(leitura),
		a.gerente,
		log.With("componente", "catalogo"),
	)
	a.endpoints = endpoint.NovoServidores(
		a.repoEndpoint,
		cat,
		a.gerente,
		log.With("componente", "endpoint"),
	)

	a.admHTTP = admin.NovoHTTP(a.adm, cfg.PublicURL, log.With("componente", "admin_http"))
	a.adminUp = upstream.NovoAdmin(
		a.repoUpstream, a.gerente,
		a.endpoints.Sincronizar, nomeExpostoDe,
		log.With("componente", "admin_upstream"),
	)
	a.adminEnd = endpoint.NovoAdmin(
		a.repoEndpoint, a.endpoints,
		upstreamsParaEndpoint{repo: a.repoUpstream, gerente: a.gerente},
		cfg.PublicURL, log.With("componente", "admin_endpoint"),
	)
	a.adminChave = apikey.NovoAdmin(
		a.repoChave,
		endpointsParaChave{repo: a.repoEndpoint},
		cfg.PublicURL, log.With("componente", "admin_chave"),
	)

	// Materializa o que der para materializar antes de escutar: o endpoint sobe
	// servindo catálogo vazio se nenhum upstream conectou, nunca travando.
	if err := a.endpoints.Sincronizar(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	log.Info("endpoints materializados no boot", "endpoints", a.endpoints.Slugs())
	return a, nil
}

// Observar registra uma função chamada depois de cada sincronização de
// endpoints.
func (a *Aplicacao) Observar(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observados = append(a.observados, fn)
}

// Iniciar sobe as goroutines de fundo: a supervisão dos upstreams, o gravador de
// "último uso" das chaves e a limpeza de sessão de admin. Todas morrem com o ctx.
func (a *Aplicacao) Iniciar(ctx context.Context) {
	a.gerente.Iniciar(ctx)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.chave.GravarUsos(ctx)
	}()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.limparSessoes(ctx)
	}()

	// A lápide de ferramenta vence sozinha, e a rematerialização só acontece
	// quando algo muda: sem esta varredura, a ferramenta que saiu ficaria no
	// tools/list explicando que saiu para sempre.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.endpoints.VigiarLapides(ctx)
	}()
}

// limparSessoes varre as sessões vencidas até o ctx ser cancelado.
func (a *Aplicacao) limparSessoes(ctx context.Context) {
	tique := time.NewTicker(intervaloLimpezaSessao)
	defer tique.Stop()
	for {
		if err := a.adm.LimparSessoes(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("falha ao limpar sessões de admin", "erro", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tique.C:
		}
	}
}

// Handler monta o roteamento HTTP.
//
// Três espaços de URL: /mcp/{slug} é o transporte MCP autenticado por chave de
// API, /static/ são os arquivos da UI e /admin/ é a administração atrás da sessão
// de admin.
func (a *Aplicacao) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(endpoint.Rota, a.endpoints.Handler(
		endpoint.Autorizacao{Verificar: a.chave.Verificar, Escopo: apikey.Escopo},
		a.cfg.PublicURL,
		a.log,
	))
	mux.HandleFunc("GET /saude", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET "+webui.Prefixo, webui.Estaticos())

	// Setup, login e logout ficam fora do portão: eles são o portão.
	a.admHTTP.Rotas(mux)

	// Tudo o mais sob /admin/ exige sessão. O padrão com barra é menos específico
	// que os literais acima, então o ServeMux resolve o login para o handler de
	// login e o resto para cá.
	protegido := http.NewServeMux()
	protegido.HandleFunc("GET "+webui.RotaPainel, a.painel)
	a.adminUp.Rotas(protegido)
	a.adminEnd.Rotas(protegido)
	a.adminChave.Rotas(protegido)
	mux.Handle(webui.RotaPainel, a.admHTTP.Proteger(protegido))

	// Proteção de Origin nativa do net/http: requisição de navegador
	// cross-origin com método não seguro é recusada com 403. Cliente que não é
	// navegador não manda Origin nem Sec-Fetch-Site e passa direto, que é o
	// comportamento certo para o transporte MCP — e é também o que faz o htmx
	// funcionar sem token de formulário, porque ele manda Sec-Fetch-Site:
	// same-origin como qualquer fetch da própria página.
	protecao := http.NewCrossOriginProtection()
	for _, origem := range []string{a.cfg.PublicURL} {
		if err := protecao.AddTrustedOrigin(origem); err != nil {
			a.log.Warn("origem confiável recusada", "origem", origem, "erro", err)
		}
	}
	return protecao.Handler(mux)
}

// painel é a tela inicial da administração.
func (a *Aplicacao) painel(w http.ResponseWriter, r *http.Request) {
	d := dadosPainel{URLPublica: a.cfg.PublicURL}

	ups, err := a.repoUpstream.Todos(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	d.Upstreams = len(ups)
	for _, u := range ups {
		s, sob := a.gerente.Situacao(u.ID)
		switch {
		case !sob:
			// Desabilitado ou fora da supervisão: nem pronto, nem degradado.
		case s.Estado == upstream.EstadoPronto:
			d.UpstreamsProntos++
		case s.Estado == upstream.EstadoDegradado, s.Estado == upstream.EstadoSondaFalhou,
			s.Estado == upstream.EstadoDesabilitado, s.Estado == upstream.EstadoSemConsentimento:
			d.UpstreamsRuins++
		}
	}

	slugs := a.endpoints.Slugs()
	d.Endpoints = len(slugs)
	for _, slug := range slugs {
		d.Ferramentas += a.endpoints.Contagem(slug)
	}

	chaves, err := a.repoChave.Todas(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	d.Chaves = len(chaves)
	for _, c := range chaves {
		if !c.Revogada() {
			d.ChavesAtivas++
		}
	}

	webui.Renderizar(w, r, http.StatusOK, a.log, telaPainel(d, webui.Avisos(r, avisosDoPainel)))
}

var avisosDoPainel = map[string]webui.Alerta{
	"setup": {
		Tom:    webui.TomSucesso,
		Titulo: "Administrador criado e sessão aberta.",
		Texto:  "O próximo passo é cadastrar um upstream.",
	},
}

// Fechar espera as goroutines de fundo e fecha o banco.
func (a *Aplicacao) Fechar() error {
	a.gerente.Aguardar()
	a.wg.Wait()
	return a.st.Close()
}

// sincronizar rematerializa os endpoints. É o que o gerente chama quando o
// catálogo de algum upstream muda.
func (a *Aplicacao) sincronizar(ctx context.Context) {
	ctxSync, cancelar := context.WithTimeout(ctx, timeoutSincronizacao)
	defer cancelar()

	if err := a.endpoints.Sincronizar(ctxSync); err != nil {
		a.log.Error("falha ao rematerializar endpoints", "erro", err)
	}

	a.mu.Lock()
	observados := make([]func(), len(a.observados))
	copy(observados, a.observados)
	a.mu.Unlock()
	for _, fn := range observados {
		fn()
	}
}

// servir sobe o servidor HTTP e desliga limpo no cancelamento do ctx.
func servir(ctx context.Context, cfg Config, log *slog.Logger) error {
	cofre, err := cofreDoAmbiente()
	if err != nil {
		return err
	}
	a, err := montar(ctx, cfg, cofre, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := a.Fechar(); err != nil {
			log.Error("falha ao fechar a aplicação", "erro", err)
		}
	}()

	a.Iniciar(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	erros := make(chan error, 1)
	go func() {
		log.Info("patchbay escutando",
			"listen", cfg.Listen, "url_publica", cfg.PublicURL, "banco", a.st.Caminho(),
			"administracao", cfg.PublicURL+webui.RotaPainel)
		erros <- srv.ListenAndServe()
	}()

	select {
	case err := <-erros:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("servir: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("desligando")
		ctxDesligar, cancelar := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelar()
		if err := srv.Shutdown(ctxDesligar); err != nil {
			return fmt.Errorf("desligar: %w", err)
		}
		return nil
	}
}
