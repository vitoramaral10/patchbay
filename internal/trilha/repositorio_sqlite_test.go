package trilha_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/trilha"
)

func repositorioDeTeste(t *testing.T) *trilha.RepositorioSQLite {
	t.Helper()

	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})
	return trilha.NovoRepositorioSQLite(st.Leitura(), st.Escrita())
}

// base é o instante de referência dos testes de filtro. Fixo e não time.Now()
// dentro de cada caso: o recorte por período tem de ser reproduzível.
func base() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func eventoEm(quando time.Time, e trilha.Evento) trilha.Evento {
	e.Inicio = quando
	if e.Resultado == "" {
		e.Resultado = trilha.ResultadoOK
	}
	return e
}

// gravarAmostra grava um conjunto conhecido e devolve o instante de referência.
func gravarAmostra(t *testing.T, repo *trilha.RepositorioSQLite) time.Time {
	t.Helper()

	agora := base()
	eventos := []trilha.Evento{
		eventoEm(agora.Add(-time.Minute), trilha.Evento{
			EndpointSlug: "pessoal", UpstreamNome: "notion",
			Ferramenta: "notion_buscar", Original: "buscar",
			Duracao: 12 * time.Millisecond, BytesEntrada: 30, BytesSaida: 400,
			Sessao: "aaaa1111", Credencial: "apikey:1", Era: "2025-11-25",
		}),
		eventoEm(agora.Add(-2*time.Minute), trilha.Evento{
			EndpointSlug: "pessoal", UpstreamNome: "github",
			Ferramenta: "github_issues", Original: "issues",
			Resultado: trilha.ResultadoErro, Erro: "upstream github: 500",
			Duracao: 900 * time.Millisecond,
		}),
		eventoEm(agora.Add(-90*time.Minute), trilha.Evento{
			EndpointSlug: "trabalho", UpstreamNome: "notion",
			Ferramenta: "notion_criar", Original: "criar",
			Resultado: trilha.ResultadoTimeout, Erro: "prazo esgotado",
			Duracao: 15 * time.Second,
		}),
		eventoEm(agora.Add(-30*time.Hour), trilha.Evento{
			EndpointSlug: "trabalho", UpstreamNome: "github",
			Ferramenta: "github_pr", Original: "pr",
			Duracao: 40 * time.Millisecond,
		}),
	}
	if err := repo.Gravar(context.Background(), eventos); err != nil {
		t.Fatalf("gravar amostra: erro = %v, quer nil", err)
	}
	return agora
}

// TestRepositorioSQLite_GravarEmLoteELer prova a ida e a volta: o lote inteiro
// entra numa transação e volta com todos os campos intactos.
func TestRepositorioSQLite_GravarEmLoteELer(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	agora := base()

	quer := eventoEm(agora, trilha.Evento{
		EndpointID: 7, EndpointSlug: "pessoal",
		UpstreamID: 3, UpstreamNome: "notion",
		Ferramenta: "notion_buscar", Original: "buscar",
		Resultado: trilha.ResultadoOK, Erro: "",
		Duracao: 123 * time.Millisecond, BytesEntrada: 42, BytesSaida: 4096,
		Sessao: "abcdef0123456789", Credencial: "apikey:9", Era: "2026-07-28",
	})
	if err := repo.Gravar(context.Background(), []trilha.Evento{quer}); err != nil {
		t.Fatalf("Gravar: erro = %v, quer nil", err)
	}

	eventos, temMais, err := repo.Listar(context.Background(), trilha.Filtro{})
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	if temMais {
		t.Error("temMais = true, quer false com uma linha só")
	}
	if len(eventos) != 1 {
		t.Fatalf("linhas = %d, quer 1", len(eventos))
	}

	got := eventos[0]
	if got.ID == 0 {
		t.Error("ID = 0, quer o id da linha")
	}
	if !got.Inicio.Equal(quer.Inicio) {
		t.Errorf("Inicio = %v, quer %v (o ts é gravado em milissegundos)", got.Inicio, quer.Inicio)
	}
	if got.Duracao != quer.Duracao {
		t.Errorf("Duracao = %v, quer %v", got.Duracao, quer.Duracao)
	}
	for _, c := range []struct {
		campo     string
		got, quer any
	}{
		{"EndpointID", got.EndpointID, quer.EndpointID},
		{"EndpointSlug", got.EndpointSlug, quer.EndpointSlug},
		{"UpstreamID", got.UpstreamID, quer.UpstreamID},
		{"UpstreamNome", got.UpstreamNome, quer.UpstreamNome},
		{"Ferramenta", got.Ferramenta, quer.Ferramenta},
		{"Original", got.Original, quer.Original},
		{"Resultado", got.Resultado, quer.Resultado},
		{"BytesEntrada", got.BytesEntrada, quer.BytesEntrada},
		{"BytesSaida", got.BytesSaida, quer.BytesSaida},
		{"Sessao", got.Sessao, quer.Sessao},
		{"Credencial", got.Credencial, quer.Credencial},
		{"Era", got.Era, quer.Era},
	} {
		if c.got != c.quer {
			t.Errorf("%s = %v, quer %v", c.campo, c.got, c.quer)
		}
	}
}

// TestRepositorioSQLite_LoteVazioNaoAbreTransacao: o consumidor chama descarga
// no tique mesmo sem nada, e isso não pode custar uma transação no escritor
// único.
func TestRepositorioSQLite_LoteVazioNaoAbreTransacao(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	if err := repo.Gravar(context.Background(), nil); err != nil {
		t.Fatalf("Gravar(nil): erro = %v, quer nil", err)
	}
}

// TestRepositorioSQLite_Filtros é o critério "a tela filtra", nos cinco eixos.
func TestRepositorioSQLite_Filtros(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	agora := gravarAmostra(t, repo)

	casos := map[string]struct {
		filtro trilha.Filtro
		quer   []string // ferramentas esperadas, da mais nova para a mais antiga
	}{
		"sem filtro traz tudo, mais nova antes": {
			filtro: trilha.Filtro{},
			quer:   []string{"notion_buscar", "github_issues", "notion_criar", "github_pr"},
		},
		"por endpoint": {
			filtro: trilha.Filtro{Endpoint: "pessoal"},
			quer:   []string{"notion_buscar", "github_issues"},
		},
		"por upstream": {
			filtro: trilha.Filtro{Upstream: "notion"},
			quer:   []string{"notion_buscar", "notion_criar"},
		},
		"por endpoint e upstream juntos": {
			filtro: trilha.Filtro{Endpoint: "trabalho", Upstream: "github"},
			quer:   []string{"github_pr"},
		},
		"por resultado ok": {
			filtro: trilha.Filtro{Resultado: trilha.ResultadoOK},
			quer:   []string{"notion_buscar", "github_pr"},
		},
		"por resultado erro": {
			filtro: trilha.Filtro{Resultado: trilha.ResultadoErro},
			quer:   []string{"github_issues"},
		},
		"por resultado timeout": {
			filtro: trilha.Filtro{Resultado: trilha.ResultadoTimeout},
			quer:   []string{"notion_criar"},
		},
		"por pedaço do nome exposto": {
			filtro: trilha.Filtro{Ferramenta: "issues"},
			quer:   []string{"github_issues"},
		},
		"por pedaço do prefixo": {
			filtro: trilha.Filtro{Ferramenta: "notion_"},
			quer:   []string{"notion_buscar", "notion_criar"},
		},
		"pelo nome original, que não é o exposto": {
			filtro: trilha.Filtro{Ferramenta: "criar"},
			quer:   []string{"notion_criar"},
		},
		"underscore não é curinga": {
			// Sem o ESCAPE, "b_scar" casaria "buscar" pelo curinga do LIKE.
			filtro: trilha.Filtro{Ferramenta: "b_scar"},
			quer:   nil,
		},
		"porcento não é curinga": {
			filtro: trilha.Filtro{Ferramenta: "%"},
			quer:   nil,
		},
		"período de 15 minutos": {
			filtro: trilha.Filtro{Desde: agora.Add(-15 * time.Minute)},
			quer:   []string{"notion_buscar", "github_issues"},
		},
		"período de 24 horas": {
			filtro: trilha.Filtro{Desde: agora.Add(-24 * time.Hour)},
			quer:   []string{"notion_buscar", "github_issues", "notion_criar"},
		},
		"janela fechada no meio": {
			filtro: trilha.Filtro{
				Desde: agora.Add(-100 * time.Minute),
				Ate:   agora.Add(-80 * time.Minute),
			},
			quer: []string{"notion_criar"},
		},
		"resultado desconhecido é ignorado em vez de esvaziar a tela": {
			filtro: trilha.Filtro{Resultado: trilha.Resultado("inventado")},
			quer:   []string{"notion_buscar", "github_issues", "notion_criar", "github_pr"},
		},
		"combinação sem resultado": {
			filtro: trilha.Filtro{Endpoint: "pessoal", Resultado: trilha.ResultadoTimeout},
			quer:   nil,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			eventos, _, err := repo.Listar(context.Background(), tc.filtro)
			if err != nil {
				t.Fatalf("Listar: erro = %v, quer nil", err)
			}
			got := ferramentasDe(eventos)
			if !slices.Equal(got, tc.quer) {
				t.Errorf("ferramentas = %v, quer %v", got, tc.quer)
			}
		})
	}
}

// TestRepositorioSQLite_Paginacao prova que a página seguinte existe sem COUNT
// nem OFFSET — o cursor é o (ts, id) da última linha vista — e que não repete
// nem pula linha.
func TestRepositorioSQLite_Paginacao(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	gravarAmostra(t, repo)

	primeira, temMais, err := repo.Listar(context.Background(), trilha.Filtro{PorPagina: 3})
	if err != nil {
		t.Fatalf("Listar página 1: erro = %v, quer nil", err)
	}
	if !temMais {
		t.Error("temMais = false na página 1, quer true (são quatro linhas em páginas de três)")
	}
	if len(primeira) != 3 {
		t.Fatalf("linhas na página 1 = %d, quer 3", len(primeira))
	}

	ultima := primeira[len(primeira)-1]
	segunda, temMais, err := repo.Listar(context.Background(), trilha.Filtro{
		PorPagina: 3,
		CursorTS:  ultima.Inicio.UTC().UnixMilli(),
		CursorID:  ultima.ID,
	})
	if err != nil {
		t.Fatalf("Listar página 2: erro = %v, quer nil", err)
	}
	if temMais {
		t.Error("temMais = true na página 2, quer false")
	}
	if len(segunda) != 1 {
		t.Fatalf("linhas na página 2 = %d, quer 1", len(segunda))
	}

	todas := append(ferramentasDe(primeira), ferramentasDe(segunda)...)
	quer := []string{"notion_buscar", "github_issues", "notion_criar", "github_pr"}
	if !slices.Equal(todas, quer) {
		t.Errorf("páginas concatenadas = %v, quer %v", todas, quer)
	}
}

// TestRepositorioSQLite_PaginacaoPorCursorNaoRepiteNemPula prova o motivo da
// troca de OFFSET por keyset: linhas gravadas no mesmo milissegundo — o caso
// comum sob rajada — não podem repetir nem sumir quando caem na fronteira de
// duas páginas.
func TestRepositorioSQLite_PaginacaoPorCursorNaoRepiteNemPula(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	mesmoInstante := base()
	var lote []trilha.Evento
	for i := range 5 {
		lote = append(lote, eventoEm(mesmoInstante, trilha.Evento{
			EndpointSlug: "pessoal", Ferramenta: fmt.Sprintf("ferramenta-%d", i),
		}))
	}
	if err := repo.Gravar(context.Background(), lote); err != nil {
		t.Fatalf("Gravar: erro = %v, quer nil", err)
	}

	primeira, temMais, err := repo.Listar(context.Background(), trilha.Filtro{PorPagina: 3})
	if err != nil {
		t.Fatalf("Listar página 1: erro = %v, quer nil", err)
	}
	if !temMais {
		t.Fatal("temMais = false, quer true (cinco linhas em páginas de três)")
	}
	if len(primeira) != 3 {
		t.Fatalf("linhas na página 1 = %d, quer 3", len(primeira))
	}

	ultima := primeira[len(primeira)-1]
	segunda, temMais, err := repo.Listar(context.Background(), trilha.Filtro{
		PorPagina: 3, CursorTS: ultima.Inicio.UTC().UnixMilli(), CursorID: ultima.ID,
	})
	if err != nil {
		t.Fatalf("Listar página 2: erro = %v, quer nil", err)
	}
	if temMais {
		t.Error("temMais = true na página 2, quer false")
	}
	if len(segunda) != 2 {
		t.Fatalf("linhas na página 2 = %d, quer 2", len(segunda))
	}

	vistos := make(map[int64]bool)
	for _, e := range append(slices.Clone(primeira), segunda...) {
		if vistos[e.ID] {
			t.Errorf("id %d apareceu em mais de uma página", e.ID)
		}
		vistos[e.ID] = true
	}
	if len(vistos) != 5 {
		t.Errorf("total de linhas vistas = %d, quer 5 (nenhuma pode ter sido pulada)", len(vistos))
	}
}

// TestRepositorioSQLite_FiltroPorEndpointUsaIndice confere, via
// EXPLAIN QUERY PLAN, que o filtro por endpoint_slug usa o índice da migração
// 00011 — e não um scan da tabela inteira, que é o que a seção 11 promete para
// uma trilha que cresce por meses.
func TestRepositorioSQLite_FiltroPorEndpointUsaIndice(t *testing.T) {
	t.Parallel()

	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})

	linhas, err := st.Leitura().QueryContext(context.Background(),
		`EXPLAIN QUERY PLAN SELECT id FROM call_log WHERE endpoint_slug = ? ORDER BY ts DESC, id DESC LIMIT 10`,
		"pessoal")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: erro = %v, quer nil", err)
	}
	defer func() { _ = linhas.Close() }()

	var plano strings.Builder
	for linhas.Next() {
		var id, pai, notUsed int
		var detalhe string
		if err := linhas.Scan(&id, &pai, &notUsed, &detalhe); err != nil {
			t.Fatalf("ler plano: erro = %v, quer nil", err)
		}
		plano.WriteString(detalhe)
		plano.WriteByte('\n')
	}
	if err := linhas.Err(); err != nil {
		t.Fatalf("iterar plano: erro = %v, quer nil", err)
	}
	t.Logf("plano de consulta:\n%s", plano.String())
	if !strings.Contains(plano.String(), "idx_call_log_endpoint_ts") {
		t.Errorf("plano de consulta não usa o índice de endpoint_slug:\n%s", plano.String())
	}
}

// relogioFake é o dublê de trilha.Relogio, para o teste do TTL de Opcoes não
// depender de esperar 60 s de verdade.
type relogioFake struct{ agora time.Time }

func (r *relogioFake) Agora() time.Time { return r.agora }

// TestRepositorioSQLite_OpcoesUsaCacheDentroDoTTL prova o cache da seção 11:
// dentro do TTL, uma segunda chamada não consulta o banco — uma linha gravada
// depois da primeira chamada não aparece até o relógio avançar.
func TestRepositorioSQLite_OpcoesUsaCacheDentroDoTTL(t *testing.T) {
	t.Parallel()

	st, err := store.Abrir(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})

	relogio := &relogioFake{agora: time.Now()}
	repo := trilha.NovoRepositorioSQLite(st.Leitura(), st.Escrita(), trilha.ComRelogio(relogio))

	if err := repo.Gravar(context.Background(), []trilha.Evento{
		eventoEm(base(), trilha.Evento{EndpointSlug: "pessoal", Ferramenta: "somar", Resultado: trilha.ResultadoOK}),
	}); err != nil {
		t.Fatalf("Gravar: erro = %v, quer nil", err)
	}

	primeira, err := repo.Opcoes(context.Background())
	if err != nil {
		t.Fatalf("Opcoes: erro = %v, quer nil", err)
	}
	if !slices.Contains(primeira.Endpoints, "pessoal") {
		t.Fatalf("Endpoints = %v, quer conter %q", primeira.Endpoints, "pessoal")
	}

	// Uma linha nova entra depois da primeira chamada: se a segunda consultasse
	// o banco, ela apareceria — e é isso que a assertiva de baixo nega.
	if err := repo.Gravar(context.Background(), []trilha.Evento{
		eventoEm(base(), trilha.Evento{EndpointSlug: "trabalho", Ferramenta: "outra", Resultado: trilha.ResultadoOK}),
	}); err != nil {
		t.Fatalf("Gravar: erro = %v, quer nil", err)
	}

	dentroDoTTL, err := repo.Opcoes(context.Background())
	if err != nil {
		t.Fatalf("Opcoes: erro = %v, quer nil", err)
	}
	if slices.Contains(dentroDoTTL.Endpoints, "trabalho") {
		t.Error("Opcoes consultou o banco dentro do TTL: a linha nova já apareceu")
	}

	relogio.agora = relogio.agora.Add(61 * time.Second)
	depoisDoTTL, err := repo.Opcoes(context.Background())
	if err != nil {
		t.Fatalf("Opcoes: erro = %v, quer nil", err)
	}
	if !slices.Contains(depoisDoTTL.Endpoints, "trabalho") {
		t.Error("Opcoes não voltou a consultar o banco depois do TTL vencer")
	}
}

// TestRepositorioSQLite_PodarApagaSoOVencido é o critério da retenção.
func TestRepositorioSQLite_PodarApagaSoOVencido(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	agora := gravarAmostra(t, repo)

	// Corte de 24 h: só a linha de 30 h atrás está vencida.
	apagadas, err := repo.Podar(context.Background(), agora.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("Podar: erro = %v, quer nil", err)
	}
	if apagadas != 1 {
		t.Errorf("apagadas = %d, quer 1", apagadas)
	}

	eventos, _, err := repo.Listar(context.Background(), trilha.Filtro{})
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	quer := []string{"notion_buscar", "github_issues", "notion_criar"}
	if got := ferramentasDe(eventos); !slices.Equal(got, quer) {
		t.Errorf("restaram %v, quer %v", got, quer)
	}

	// Repetir não apaga mais nada: a poda é idempotente para o mesmo corte.
	apagadas, err = repo.Podar(context.Background(), agora.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("Podar de novo: erro = %v, quer nil", err)
	}
	if apagadas != 0 {
		t.Errorf("apagadas na segunda passada = %d, quer 0", apagadas)
	}
}

// TestRepositorioSQLite_PodarRespeitaOLimite prova que o DELETE é limitado: é o
// que impede a varredura de virar uma transação de tamanho imprevisível no único
// escritor do SQLite.
func TestRepositorioSQLite_PodarRespeitaOLimite(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	agora := base()

	lote := make([]trilha.Evento, 0, 10)
	for i := range 10 {
		lote = append(lote, eventoEm(agora.Add(-time.Duration(48+i)*time.Hour),
			trilha.Evento{EndpointSlug: "pessoal", Ferramenta: "velha"}))
	}
	if err := repo.Gravar(context.Background(), lote); err != nil {
		t.Fatalf("Gravar: erro = %v, quer nil", err)
	}

	apagadas, err := repo.Podar(context.Background(), agora.Add(-24*time.Hour), 4)
	if err != nil {
		t.Fatalf("Podar: erro = %v, quer nil", err)
	}
	if apagadas != 4 {
		t.Errorf("apagadas = %d, quer 4 (o limite do lote)", apagadas)
	}

	eventos, _, err := repo.Listar(context.Background(), trilha.Filtro{PorPagina: 100})
	if err != nil {
		t.Fatalf("Listar: erro = %v, quer nil", err)
	}
	if len(eventos) != 6 {
		t.Errorf("restaram %d linhas, quer 6", len(eventos))
	}
}

// TestRepositorioSQLite_ResumoEOpcoes cobre os contadores do painel e os valores
// dos seletores.
func TestRepositorioSQLite_ResumoEOpcoes(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	gravarAmostra(t, repo)

	resumo, err := repo.Resumo(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Resumo: erro = %v, quer nil", err)
	}
	// Na última hora só as duas primeiras linhas contam.
	if resumo.Chamadas != 2 {
		t.Errorf("Chamadas = %d, quer 2", resumo.Chamadas)
	}
	if resumo.Erros != 1 {
		t.Errorf("Erros = %d, quer 1", resumo.Erros)
	}
	if resumo.Timeouts != 0 {
		t.Errorf("Timeouts = %d, quer 0 (o timeout foi há 90 min)", resumo.Timeouts)
	}
	if resumo.PiorDuracaoMS != 900 {
		t.Errorf("PiorDuracaoMS = %d, quer 900", resumo.PiorDuracaoMS)
	}

	opcoes, err := repo.Opcoes(context.Background())
	if err != nil {
		t.Fatalf("Opcoes: erro = %v, quer nil", err)
	}
	if quer := []string{"pessoal", "trabalho"}; !slices.Equal(opcoes.Endpoints, quer) {
		t.Errorf("Endpoints = %v, quer %v", opcoes.Endpoints, quer)
	}
	if quer := []string{"github", "notion"}; !slices.Equal(opcoes.Upstreams, quer) {
		t.Errorf("Upstreams = %v, quer %v", opcoes.Upstreams, quer)
	}
	quer := []string{"github_issues", "github_pr", "notion_buscar", "notion_criar"}
	if !slices.Equal(opcoes.Ferramentas, quer) {
		t.Errorf("Ferramentas = %v, quer %v", opcoes.Ferramentas, quer)
	}
}

// TestRepositorioSQLite_ResumoSemLinhasNaoQuebra: banco novo devolve zeros, não
// erro de NULL.
func TestRepositorioSQLite_ResumoSemLinhasNaoQuebra(t *testing.T) {
	t.Parallel()

	repo := repositorioDeTeste(t)
	resumo, err := repo.Resumo(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Resumo: erro = %v, quer nil", err)
	}
	if resumo.Chamadas != 0 || resumo.Erros != 0 || resumo.Timeouts != 0 || resumo.PiorDuracaoMS != 0 {
		t.Errorf("Resumo = %+v, quer tudo zero", resumo)
	}
}

func ferramentasDe(eventos []trilha.Evento) []string {
	var out []string
	for _, e := range eventos {
		out = append(out, e.Ferramenta)
	}
	return out
}
