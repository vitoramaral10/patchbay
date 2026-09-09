package biblioteca_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
)

// paginaFalsa monta uma resposta do registry com um servidor remoto, e o cursor
// que leva à seguinte. Cursor vazio é a última página.
func paginaFalsa(nome, proximo string) string {
	cursor := ""
	if proximo != "" {
		cursor = fmt.Sprintf(`"nextCursor":%q`, proximo)
	}
	return fmt.Sprintf(`{"servers":[{"server":{
		"name":%q,"title":%q,"version":"1.0.0","description":"servidor de teste",
		"remotes":[{"type":"streamable-http","url":"https://%s.test/mcp"}]
	}}],"metadata":{%s}}`, nome, nome, "exemplo", cursor)
}

// registryDeMentira serve três páginas encadeadas por cursor.
func registryDeMentira(t *testing.T) origemDeMentira {
	t.Helper()

	return servir(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(paginaFalsa("com.exemplo/um", "c2")))
		case "c2":
			_, _ = w.Write([]byte(paginaFalsa("com.exemplo/dois", "c3")))
		default:
			_, _ = w.Write([]byte(paginaFalsa("com.exemplo/tres", "")))
		}
	})
}

// sincronizadorDeTeste monta a rotina com a curadoria muda: quem está sob teste
// aqui é a varredura do registry, e uma curadoria que publica um servidor
// qualquer mantém a varredura acima do piso sem interferir na mesclagem.
func sincronizadorDeTeste(
	t *testing.T, base string, repo *biblioteca.RepositorioSQLite,
) *biblioteca.Sincronizador {
	t.Helper()
	return sincronizadorCom(t, base, curadoriaMuda(t), repo)
}

func sincronizadorCom(
	t *testing.T, base, curada string, repo *biblioteca.RepositorioSQLite,
) *biblioteca.Sincronizador {
	t.Helper()

	return biblioteca.NovoSincronizador(
		biblioteca.NovaOrigem(base), biblioteca.NovaCuradoria(curada),
		repo, slog.New(slog.DiscardHandler),
		biblioteca.ComEsperaEntreTentativas(0),
	)
}

func TestVarreduraPercorreTodasAsPaginas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := repoDeTeste(t)
	ts := registryDeMentira(t)

	if err := sincronizadorDeTeste(t, ts.URL, repo).Sincronizar(ctx); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	// Parar na primeira página é exatamente a queixa que originou esta mudança:
	// o catálogo tem de vir inteiro, seguindo o cursor até ele acabar.
	_, total, err := repo.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0)
	if err != nil {
		t.Fatalf("Buscar: erro = %v, quer nil", err)
	}
	// Três do registry, uma por página, mais o servidor que a curadoria muda
	// publica: a varredura junta as duas origens.
	if total != 4 {
		t.Fatalf("servidores gravados = %d, quer 4 (3 do registry + 1 da curadoria)", total)
	}
	if idas := ts.idas.Load(); idas != 3 {
		t.Errorf("idas à origem = %d, quer 3", idas)
	}

	estado, err := repo.Sincronizacao(ctx)
	if err != nil {
		t.Fatalf("Sincronizacao: erro = %v, quer nil", err)
	}
	if estado.Nunca() || estado.Servidores != 4 || estado.Erro != "" {
		t.Fatalf("estado = %+v, quer varredura completa de 4 sem erro", estado)
	}
}

func TestVarreduraInsisteNaPaginaQueFalhou(t *testing.T) {
	t.Parallel()

	var idas atomic.Int64
	ts := servir(t, func(w http.ResponseWriter, r *http.Request) {
		// A primeira ida cai. Desistir aí jogaria fora a varredura inteira por
		// causa de um soluço de rede — e o registry medido em 2026-09-09 tem
		// deles.
		if idas.Add(1) == 1 {
			http.Error(w, "instável", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(paginaFalsa("com.exemplo/um", "")))
	})

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := sincronizadorDeTeste(t, ts.URL, repo).Sincronizar(ctx); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}
	if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 2 {
		t.Fatalf("servidores = %d, quer 2: a varredura desistiu na primeira falha", total)
	}
}

func TestEsquemaMudadoNaoEInsistido(t *testing.T) {
	t.Parallel()

	var idas atomic.Int64
	ts := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		idas.Add(1)
		_, _ = w.Write([]byte("isto não é json"))
	})

	err := sincronizadorDeTeste(t, ts.URL, repoDeTeste(t)).Sincronizar(context.Background())
	if !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("erro = %v, quer ErrFormatoDaOrigem", err)
	}
	// Repetir não melhora esquema mudado: seria peso na origem sem chance de
	// sucesso.
	if n := idas.Load(); n != 1 {
		t.Errorf("idas à origem = %d, quer 1", n)
	}
}

func TestCursorQueSeRepeteNaoViraVarreduraSemFim(t *testing.T) {
	t.Parallel()

	ts := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		// Sempre o mesmo cursor: sem a guarda, a varredura giraria para sempre.
		_, _ = w.Write([]byte(paginaFalsa("com.exemplo/um", "sempre-o-mesmo")))
	})

	err := sincronizadorDeTeste(t, ts.URL, repoDeTeste(t)).Sincronizar(context.Background())
	if !errors.Is(err, biblioteca.ErrFormatoDaOrigem) {
		t.Fatalf("erro = %v, quer ErrFormatoDaOrigem", err)
	}
}

func TestVarreduraQueFalhaNaoDerrubaOCatalogoAnterior(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := repo.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}

	fora := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora do ar", http.StatusBadGateway)
	})
	if err := sincronizadorDeTeste(t, fora.URL, repo).Sincronizar(ctx); err == nil {
		t.Fatal("Sincronizar: erro = nil, quer a falha da origem")
	}

	// Dado velho e útil vale mais do que tela vazia: a cópia anterior continua
	// servindo, e a tela mostra a idade dela junto com o erro.
	if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3", total)
	}
	estado, _ := repo.Sincronizacao(ctx)
	if estado.Erro == "" {
		t.Error("a falha não ficou visível para a tela")
	}
}

func TestVarreduraQueEstouraOPrazoRegistraFalha(t *testing.T) {
	t.Parallel()

	// Origem que nunca termina a página. Com o prazo da varredura em zero, o
	// efeito é o mesmo de uma origem lenta demais — e o que se prova é que isso
	// não é confundido com o patchbay desligando: a falha tem de ficar
	// registrada, senão a tela nunca conta que a última tentativa não terminou.
	lenta := servir(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancelar := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelar()

	repo := repoDeTeste(t)
	if err := repo.Substituir(context.Background(), itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}
	if err := sincronizadorDeTeste(t, lenta.URL, repo).Sincronizar(ctx); err == nil {
		t.Fatal("Sincronizar: erro = nil, quer o prazo estourado")
	}

	// Contexto de fora cancelado é desligamento: aí a falha não é registrada, de
	// propósito. Aqui o catálogo anterior é o que precisa ter sobrevivido.
	if _, total, _ := repo.Buscar(context.Background(), biblioteca.Filtro{Termo: ""}, 10, 0); total != 3 {
		t.Fatalf("total = %d, quer 3: a varredura interrompida apagou o catálogo", total)
	}
}

// TestMesclagemJuntaAsDuasOrigens é o teste da razão de existirem duas.
//
// O registry publica o Notion sem dizer que ele é OAuth — o esquema não tem esse
// campo. A curadoria diz. Depois da mesclagem, o item precisa ter a identidade
// do registry e a autenticação da curadoria; e o servidor que só a curadoria
// conhece precisa aparecer, porque medido em 2026-09-09 esse é o caso de 22 em
// cada 25.
func TestMesclagemJuntaAsDuasOrigens(t *testing.T) {
	t.Parallel()

	registry := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"servers":[{"server":{
			"name":"com.notion/mcp","title":"notion mcp","version":"1.0.1",
			"description":"Official Notion MCP server",
			"remotes":[{"type":"streamable-http","url":"https://mcp.notion.com/mcp"}]
		}}],"metadata":{}}`))
	})
	curada := curadoriaDeMentira(t, []servidorCurado{
		{Slug: "notion", Nome: "Notion", Resumo: "Notas, bases de dados e páginas",
			// A mesma URL, escrita com barra final: a chave de junção normaliza,
			// senão o mesmo servidor viraria dois cartões.
			URL: "https://mcp.notion.com/mcp/", Transporte: "Streamable HTTP", Autenticacao: "OAuth"},
		{Slug: "neon", Nome: "Neon", Resumo: "Postgres serverless",
			URL: "https://mcp.neon.tech/mcp", Transporte: "Streamable HTTP", Autenticacao: "OAuth"},
	})

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := sincronizadorCom(t, registry.URL, curada, repo).Sincronizar(ctx); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil", err)
	}

	// O Notion casou pela URL: um item só, com os dois lados.
	notion, err := repo.Um(ctx, "com.notion/mcp")
	if err != nil {
		t.Fatalf("Um(com.notion/mcp): erro = %v, quer nil — a mesclagem perdeu a identidade do registry", err)
	}
	if notion.Autenticacao != biblioteca.AutOAuth {
		t.Errorf("Autenticacao = %q, quer oauth: a curadoria não entrou", notion.Autenticacao)
	}
	if !notion.Curado {
		t.Error("Curado = false: o casamento por URL não marcou")
	}
	if notion.Descricao != "Notas, bases de dados e páginas" {
		t.Errorf("Descricao = %q, quer a da curadoria (escrita por gente, em português)", notion.Descricao)
	}

	// O Neon não existe no registry e precisa aparecer assim mesmo.
	neon, err := repo.Um(ctx, "mcpservers.org/neon")
	if err != nil {
		t.Fatalf("Um(mcpservers.org/neon): erro = %v, quer nil", err)
	}
	if !neon.Curado || neon.Autenticacao != biblioteca.AutOAuth {
		t.Errorf("neon = %+v, quer curado e oauth", neon)
	}

	// E não pode ter duplicado o Notion.
	_, total, _ := repo.Buscar(ctx, biblioteca.Filtro{}, 100, 0)
	if total != 2 {
		t.Errorf("total = %d, quer 2: a normalização da URL não casou os dois lados", total)
	}
}

// TestCuradoriaForaDoArDerrubaAVarredura: a curadoria não é enfeite. Sem ela, o
// catálogo perderia a autenticação de todo mundo e o filtro de curados ficaria
// vazio — pior do que manter a cópia anterior, que ao menos tem idade visível.
func TestCuradoriaForaDoArDerrubaAVarredura(t *testing.T) {
	t.Parallel()

	registry := registryDeMentira(t)
	fora := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fora do ar", http.StatusBadGateway)
	}))
	t.Cleanup(fora.Close)

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := repo.Substituir(ctx, itensDeTeste(), time.Now()); err != nil {
		t.Fatalf("Substituir: erro = %v, quer nil", err)
	}
	if err := sincronizadorCom(t, registry.URL, fora.URL, repo).Sincronizar(ctx); err == nil {
		t.Fatal("Sincronizar: erro = nil, quer a falha da curadoria")
	}
	if _, total, _ := repo.Buscar(ctx, biblioteca.Filtro{}, 10, 0); total != 3 {
		t.Errorf("total = %d, quer 3: o catálogo anterior foi perdido", total)
	}
}

// TestMesclagemNaoDeixaNomeColidir: o nome é chave primária, e um nome repetido
// derrubaria a varredura inteira com erro de constraint — longe da causa, e
// improvável o bastante para ninguém procurar ali.
func TestMesclagemNaoDeixaNomeColidir(t *testing.T) {
	t.Parallel()

	// O registry publica um servidor no namespace mcpservers.org, que é
	// exatamente o nome que a curadoria gera para quem só existe lá.
	registry := servir(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"servers":[{"server":{
			"name":"mcpservers.org/neon","title":"Homônimo","version":"1.0.0",
			"remotes":[{"type":"streamable-http","url":"https://outro.test/mcp"}]
		}}],"metadata":{}}`))
	})
	curada := curadoriaDeMentira(t, []servidorCurado{
		{Slug: "neon", Nome: "Neon", Resumo: "Postgres serverless",
			URL: "https://mcp.neon.tech/mcp", Transporte: "Streamable HTTP", Autenticacao: "OAuth"},
	})

	ctx := context.Background()
	repo := repoDeTeste(t)
	if err := sincronizadorCom(t, registry.URL, curada, repo).Sincronizar(ctx); err != nil {
		t.Fatalf("Sincronizar: erro = %v, quer nil — a colisão derrubou a varredura", err)
	}
	_, total, _ := repo.Buscar(ctx, biblioteca.Filtro{}, 10, 0)
	if total != 1 {
		t.Fatalf("total = %d, quer 1: o homônimo entrou duas vezes", total)
	}
}
