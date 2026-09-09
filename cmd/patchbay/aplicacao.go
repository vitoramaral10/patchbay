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
	"github.com/vitoramaral10/patchbay/internal/authsrv"
	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/configuracao"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
	"github.com/vitoramaral10/patchbay/internal/trilha"
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
	oauth *authsrv.Servico

	gerente   *upstream.Gerente
	oauthUp   *upstream.BrokerOAuth
	endpoints *endpoint.Servidores

	// hub e registrador são a observabilidade: o fan-out do log ao vivo e a
	// fila que grava a trilha fora do caminho da latência.
	hub         *trilha.Hub
	registrador *trilha.Registrador
	// pararTrilha é o sinal próprio do consumidor da trilha — disparado só
	// depois de srv.Shutdown() retornar, nunca no cancelamento do ctx do
	// serviço (que chega antes, enquanto requisições ainda estão em curso).
	// Ver PararConsumoDaTrilha.
	pararTrilha    chan struct{}
	pararTrilhaUma sync.Once

	repoUpstream *upstream.RepositorioSQLite
	repoEndpoint *endpoint.RepositorioSQLite
	repoChave    *apikey.RepositorioSQLite
	repoOAuth    *authsrv.RepositorioSQLite
	repoTrilha   *trilha.RepositorioSQLite

	admHTTP     *admin.HTTP
	oauthHTTP   *authsrv.HTTP
	adminUp     *upstream.Admin
	adminBib    *biblioteca.Admin
	sincBib     *biblioteca.Sincronizador
	adminEnd    *endpoint.Admin
	adminChave  *apikey.Admin
	adminOAuth  *authsrv.Admin
	adminConfig *configuracao.Admin
	adminTrilha *trilha.Admin

	mu         sync.Mutex
	observados []func()

	wg sync.WaitGroup
}

// OpcaoApp ajusta o grafo montado.
//
// Existe pelos dois componentes que fazem requisição de saída, e que o teste
// ponta a ponta precisa apontar para um servidor em processo: o buscador de
// documentos de CIMD, cuja URL vem de terceiro e que o guarda de SSRF —
// corretamente — recusaria em teste; e a biblioteca, que varre o registry
// oficial e sem isto faria o teste depender da internet.
type OpcaoApp func(*opcoesApp)

type opcoesApp struct {
	cimd                authsrv.DocumentosCIMD
	origemBiblioteca    string
	curadoriaBiblioteca string
	intervaloBiblioteca time.Duration
}

// ComBuscadorCIMD troca o buscador de documentos de CIMD.
func ComBuscadorCIMD(b authsrv.DocumentosCIMD) OpcaoApp {
	return func(o *opcoesApp) { o.cimd = b }
}

// ComOrigemDaBiblioteca troca a base de onde a biblioteca varre o catálogo.
//
// Só o teste usa: em produção a base é o registry oficial, fixa no pacote. Não
// é configuração — apontar a biblioteca para outro lugar em produção seria
// cadastrar upstream a partir de uma lista que ninguém revisou.
func ComOrigemDaBiblioteca(base string) OpcaoApp {
	return func(o *opcoesApp) { o.origemBiblioteca = base }
}

// ComCuradoriaDaBiblioteca troca a base da segunda origem da biblioteca, a
// lista curada de servidores remotos.
//
// Só o teste usa, pela mesma razão de ComOrigemDaBiblioteca: sem isto a suíte
// sairia para o mcpservers.org a cada execução.
func ComCuradoriaDaBiblioteca(base string) OpcaoApp {
	return func(o *opcoesApp) { o.curadoriaBiblioteca = base }
}

// ComIntervaloDaBiblioteca troca de quanto em quanto tempo o catálogo local é
// refeito.
//
// Só o teste usa, e por um motivo específico: sem isto, o teste que sobe o
// patchbay inteiro esperaria doze horas para ver a segunda varredura. Em
// produção o intervalo é constante do pacote.
func ComIntervaloDaBiblioteca(d time.Duration) OpcaoApp {
	return func(o *opcoesApp) { o.intervaloBiblioteca = d }
}

// montar abre o banco, aplica as migrações e liga os componentes.
//
// O cofre entra por parâmetro e não pela Config: a Config é impressa em log de
// boot e vai inteira para os testes, e a chave mestra não pode passar por lá.
func montar(
	ctx context.Context, cfg Config, cofre *cripto.Cofre, log *slog.Logger, opcoes ...OpcaoApp,
) (*Aplicacao, error) {
	var opc opcoesApp
	for _, o := range opcoes {
		o(&opc)
	}
	if opc.cimd == nil {
		opc.cimd = authsrv.NovoBuscadorCIMD(log.With("componente", "authsrv_cimd"))
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("criar diretório de dados %s: %w", cfg.DataDir, err)
	}

	// O hub do log ao vivo nasce antes de todo componente, e o logger é
	// reembrulhado aqui: daqui para baixo *todo* log do processo passa pela
	// redação de segredo e é replicado para a tela. Redigir só na tela deixaria o
	// vazamento no destino que ninguém revisa (seção 11).
	hub := trilha.NovoHub()
	log = slog.New(trilha.NovoHandlerLog(log.Handler(), hub))

	st, err := store.Abrir(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}

	a := &Aplicacao{cfg: cfg, log: log, st: st, hub: hub, pararTrilha: make(chan struct{})}
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
	a.repoOAuth = authsrv.NovoRepositorioSQLite(leitura, escrita)
	a.repoTrilha = trilha.NovoRepositorioSQLite(leitura, escrita)

	// A fila da trilha é montada antes dos endpoints porque é ela que o gancho de
	// captura recebe. Nada aqui é opcional por configuração: a trilha é a única
	// forma de responder "o que esse cliente andou chamando", e um gateway sem
	// ela mente por omissão.
	a.registrador = trilha.NovoRegistrador(
		a.repoTrilha, log.With("componente", "trilha"),
		trilha.ComHub(hub),
	)

	a.chave = apikey.NovoServico(a.repoChave, log.With("componente", "apikey"))
	a.adm = admin.NovoServico(
		admin.NovoRepositorioSQLite(leitura, escrita),
		log.With("componente", "admin"),
	)
	// O escopo é injetado porque o nome "endpoint:<slug>" é contrato dos dois
	// verificadores de bearer, e nenhuma feature pode importar a outra.
	a.oauth = authsrv.NovoServico(
		a.repoOAuth, endpointsParaOAuth{repo: a.repoEndpoint},
		cfg.PublicURL, apikey.Escopo,
		log.With("componente", "authsrv"),
		// Passar o buscador é o que faz a metadata anunciar
		// client_id_metadata_document_supported — e é esse anúncio, junto com
		// "none" em token_endpoint_auth_methods_supported, que faz o claude.ai
		// escolher CIMD em vez de registrar um cliente novo por DCR a cada
		// conexão fresca.
		authsrv.ComCIMD(opc.cimd),
	)

	cfgs, err := upstream.Habilitados(ctx, leitura)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	// O broker de OAuth de upstream nasce antes do gerente porque é ele que a
	// supervisão consulta para montar o Authorization. Uma instância por
	// processo: é ela que garante uma TokenSource por upstream, e duas instâncias
	// reintroduziriam a corrida de refresh que o mutex de dentro dela evita.
	a.oauthUp = upstream.NovoBrokerOAuth(
		a.repoUpstream, cfg.PublicURL, log.With("componente", "oauth_upstream"))
	if a.oauthUp.URLMetadataCliente() == "" {
		// Visível, e não silencioso: sem HTTPS a ordem de registro de cliente cai
		// direto em pré-registrado ou DCR, e quem for diagnosticar "por que não
		// usou CIMD" precisa achar a resposta no log do boot.
		log.Info("client id metadata document desligado",
			"motivo", "a URL pública não é https", "url_publica", cfg.PublicURL)
	}

	a.gerente = upstream.NovoGerente(
		log.With("componente", "upstream"), cfgs,
		upstream.AoMudar(a.sincronizar),
		upstream.ComOAuth(a.oauthUp),
		// As credenciais estáticas são lidas do banco a cada conexão, e não
		// guardadas na Config: assim trocar o bearer pela tela vale na
		// reconexão seguinte, sem cache a invalidar e sem segredo passeando
		// pela estrutura que alimenta a UI.
		upstream.ComCredenciais(a.repoUpstream.Credenciais),
		// A sonda funcional deixa rastro na mesma trilha das chamadas de
		// cliente, marcada como origem sonda. É a única costura da fatia 9 com
		// a 12, e ela cabe aqui pelo mesmo motivo da captura do endpoint:
		// nenhuma das duas features conhece a outra.
		upstream.ComObservadorDeSonda(trilhaDaSonda{registrador: a.registrador}),
		// Mesma costura, para a evidência que fica só em memória: sem redator
		// injetado, Pedido e Resposta chegariam crus a SituacaoSonda.
		upstream.ComRedator(redatorDeSonda),
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
		// O gancho de captura da trilha. É a única costura entre a fatia 12 e o
		// caminho da requisição, e o que ele faz por chamada é um envio não
		// bloqueante num canal com buffer.
		endpoint.ComObservador(trilhaDoEndpoint{registrador: a.registrador}),
	)

	a.admHTTP = admin.NovoHTTP(a.adm, cfg.PublicURL, log.With("componente", "admin_http"))
	a.adminUp = upstream.NovoAdmin(
		a.repoUpstream, a.gerente, a.oauthUp,
		a.endpoints.Sincronizar, nomeExpostoDe,
		log.With("componente", "admin_upstream"),
	)
	// A biblioteca tem cópia local de duas origens — o registry oficial e a
	// lista curada do mcpservers.org —, mesclada por uma goroutine de fundo. A
	// tela lê o banco e nunca a rede: é o que faz a busca digitada responder na
	// hora, e o que mantém a tela de pé com a internet fora.
	//
	// O resto do gateway não sabe que ela existe — nem o boot depende dela: a
	// primeira varredura acontece depois, em Iniciar, e enquanto ela não termina
	// a tela diz que o catálogo ainda está vindo.
	a.sincBib = biblioteca.NovoSincronizador(
		biblioteca.NovaOrigem(opc.origemBiblioteca),
		biblioteca.NovaCuradoria(opc.curadoriaBiblioteca),
		biblioteca.NovoRepositorio(leitura, escrita),
		log.With("componente", "biblioteca_sync"),
		biblioteca.ComIntervalo(opc.intervaloBiblioteca),
	)
	a.adminBib = biblioteca.NovoAdmin(
		biblioteca.NovoRepositorio(leitura, escrita),
		a.sincBib,
		log.With("componente", "admin_biblioteca"),
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
	// O export/import de YAML recebe as quatro features pelos adaptadores de
	// configuracao.go, e o gerente junto: um import feito pela tela precisa valer
	// no ar, sem esperar o próximo boot. O subcomando de linha de comando monta o
	// mesmo serviço sem gerente nenhum — lá não há supervisão a atualizar.
	logConfig := log.With("componente", "configuracao")
	a.adminConfig = configuracao.NovoAdmin(
		configuracao.NovoServico(
			upstreamsDaConfiguracao{
				repo: a.repoUpstream, gerente: a.gerente,
				rematerializar: a.endpoints.Sincronizar, log: logConfig,
			},
			segredosDaConfiguracao{repo: a.repoUpstream},
			endpointsDaConfiguracao{
				repo: a.repoEndpoint, upstreams: a.repoUpstream,
				servidores: a.endpoints, log: logConfig,
			},
			logConfig,
			configuracao.ComChaves(chavesDaConfiguracao{repo: a.repoChave}),
			configuracao.ComClientes(clientesDaConfiguracao{repo: a.repoOAuth}),
			// O import lê segredo do ambiente do processo. Em serve isso é o
			// ambiente com que o patchbay subiu — a chave mestra já saiu de lá
			// no boot (cofreDoAmbiente).
			configuracao.ComAmbiente(os.LookupEnv),
		),
		logConfig,
	)

	a.oauthHTTP = authsrv.NovoHTTP(a.oauth, log.With("componente", "authsrv_http"))
	a.adminOAuth = authsrv.NovoAdmin(
		a.repoOAuth, endpointsParaOAuth{repo: a.repoEndpoint},
		cfg.PublicURL, log.With("componente", "admin_oauth"),
	)
	a.adminTrilha = trilha.NovoAdmin(
		a.repoTrilha, a.registrador, hub,
		log.With("componente", "admin_trilha"),
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

// Iniciar sobe as goroutines de fundo: a supervisão dos upstreams, os dois
// gravadores de "último uso" (chave de API e token OAuth), a limpeza de estado
// vencido, a varredura de lápides e — da fatia 12 — o consumidor da trilha, a
// retenção e o desligamento do hub de SSE. Todas morrem com o ctx.
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
		a.oauth.GravarUsos(ctx)
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

	// O consumidor único da trilha. Um só, porque o SQLite aceita um escritor
	// por vez: dois competiriam pela mesma conexão para gravar a mesma tabela.
	//
	// context.Background() e não ctx: o consumidor para pelo sinal próprio
	// (a.pararTrilha, fechado só depois de srv.Shutdown() retornar), não pelo
	// cancelamento de ctx — que chega antes, enquanto requisições ainda em
	// curso podem chamar Observar. Se as gravações em andamento recebessem um
	// ctx já cancelado nessa janela, elas falhariam por causa do mesmo sinal
	// que não deveria afetá-las.
	a.wg.Add(1)
	//nolint:gosec,contextcheck // G118 e contextcheck pedem os dois para
	// propagar ctx aqui; context.Background() é a escolha certa, não um
	// esquecimento — ver o comentário acima desta goroutine.
	go func() {
		defer a.wg.Done()
		a.registrador.Consumir(context.Background(), a.pararTrilha)
	}()

	// A retenção. Em lotes pequenos, de propósito: um DELETE sem limite seria
	// uma transação de tamanho imprevisível no único escritor.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.registrador.Varrer(ctx)
	}()

	// A cópia local do catálogo de servidores MCP. A primeira varredura só
	// acontece se o que está no banco estiver vencido, então reiniciar o
	// patchbay não custa trezentas requisições ao registry.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.sincBib.Manter(ctx)
	}()

	// Solta os handlers de SSE no desligamento. Sem isto o Shutdown esperaria o
	// prazo inteiro por conexões que, por desenho, só terminam quando o cliente
	// desiste.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		<-ctx.Done()
		a.hub.Encerrar()
	}()
}

// limparSessoes varre o que venceu até o ctx ser cancelado: sessão de admin,
// código de autorização e token do AS. Nada disso autentica mais nada — a
// varredura só evita que as tabelas cresçam para sempre.
func (a *Aplicacao) limparSessoes(ctx context.Context) {
	tique := time.NewTicker(intervaloLimpezaSessao)
	defer tique.Stop()
	for {
		if err := a.adm.LimparSessoes(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("falha ao limpar sessões de admin", "erro", err)
		}
		if err := a.oauth.Limpar(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("falha ao limpar estado vencido do authorization server", "erro", err)
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
// Quatro espaços de URL: /mcp/{slug} é o transporte MCP, autenticado por chave
// de API ou por token do authorization server; /.well-known/ e /oauth/ são o
// authorization server; /static/ são os arquivos da UI; e /admin/ é a
// administração atrás da sessão de admin.
func (a *Aplicacao) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(endpoint.Rota, a.endpoints.Handler(
		endpoint.Autorizacao{
			Verificar: verificadorDeBearer(a.chave, a.oauth),
			Escopo:    apikey.Escopo,
		},
		a.cfg.PublicURL,
		a.log,
	))

	// Metadata, token e revogação são o protocolo: não passam por sessão de
	// admin, porque quem autentica ali é o cliente OAuth.
	a.oauthHTTP.Rotas(mux)
	// O Client ID Metadata Document do patchbay como cliente OAuth de upstream:
	// quem o lê é o authorization server do provedor, de fora, e exigir sessão de
	// admin aqui só o impediria de ler.
	a.adminUp.RotasPublicas(mux)
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
	a.adminBib.Rotas(protegido)
	a.adminEnd.Rotas(protegido)
	a.adminChave.Rotas(protegido)
	a.adminOAuth.Rotas(protegido)
	a.adminConfig.Rotas(protegido)
	a.adminTrilha.Rotas(protegido)
	mux.Handle(webui.RotaPainel, a.admHTTP.Proteger(protegido))

	// O authorize endpoint é o único do AS que exige sessão de admin: o
	// "usuário" deste authorization server é o administrador do patchbay, e o
	// mesmo portão da UI serve aqui — sem sessão ele redireciona para o login
	// carregando o destino, então o clique do consentimento não se perde.
	autorizar := http.NewServeMux()
	a.oauthHTTP.RotasAutorizacao(autorizar)
	mux.Handle(authsrv.RotaAutorizar, a.admHTTP.Proteger(autorizar))

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
	// O token e o revocation endpoint saem da proteção: são API de protocolo,
	// chamados de outra origem por desenho, e a defesa deles é a autenticação de
	// cliente mais o PKCE — não o Origin. Recusá-los com 403 quebraria um
	// cliente MCP que rode no navegador.
	for _, padrao := range authsrv.PadroesSemProtecaoDeOrigem {
		protecao.AddInsecureBypassPattern(padrao)
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
		d.Lapides += len(a.endpoints.Lapides(slug))
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

// PararConsumoDaTrilha sinaliza ao consumidor da trilha que pode drenar o que
// sobrou na fila e voltar. Idempotente: chamar mais de uma vez não entra em
// pânico (sync.Once fechando o canal).
//
// Chame só depois de ter certeza de que nenhuma requisição em curso vai mais
// chamar Observar — no processo real, depois de srv.Shutdown() retornar (ver
// servir); em teste, depois que o servidor de teste já garantiu o mesmo.
// Chamar cedo demais reabre o furo que esta função existe para fechar: uma
// chamada que termina depois cai numa fila que ninguém mais drena.
func (a *Aplicacao) PararConsumoDaTrilha() {
	a.pararTrilhaUma.Do(func() { close(a.pararTrilha) })
}

// Fechar espera as goroutines de fundo e fecha o banco.
//
// PararConsumoDaTrilha aqui é a rede de segurança: quem já passou pelo fluxo
// completo de servir() a chamou antes, e esta chamada é um no-op idempotente.
// Quem monta a Aplicacao fora desse fluxo (teste que sobe e derruba pela
// tela) nunca a chama sozinho, e sem isto a.wg.Wait() abaixo travaria para
// sempre esperando o consumidor.
func (a *Aplicacao) Fechar() error {
	a.PararConsumoDaTrilha()
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
		// a.log e não log: daqui em diante o logger é o que redige segredo e
		// replica para a tela de log ao vivo.
		a.log.Info("patchbay escutando",
			"listen", cfg.Listen, "url_publica", cfg.PublicURL, "banco", a.st.Caminho(),
			"administracao", cfg.PublicURL+webui.RotaPainel)
		erros <- srv.ListenAndServe()
	}()

	select {
	case err := <-erros:
		// O servidor saiu por conta própria (crash de listener, por exemplo):
		// nenhuma requisição pode mais estar em curso, então já é seguro parar
		// o consumidor da trilha.
		a.PararConsumoDaTrilha()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("servir: %w", err)
		}
		return nil
	case <-ctx.Done():
		a.log.Info("desligando")
		ctxDesligar, cancelar := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelar()
		erroDesligar := srv.Shutdown(ctxDesligar)
		// Só depois de Shutdown retornar: é a garantia de que toda requisição
		// em curso — e toda chamada a Observar que ela ainda pudesse fazer —
		// terminou. Chamar isto no ctx.Done() de cima perderia justamente essa
		// garantia (ver o comentário de Consumir em trilha/registrador.go).
		a.PararConsumoDaTrilha()
		if erroDesligar != nil {
			return fmt.Errorf("desligar: %w", erroDesligar)
		}
		return nil
	}
}
