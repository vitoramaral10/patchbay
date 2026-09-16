package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/biblioteca"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// TestCatalogoDoRegistryEDescartadoNaAtualizacao prova a migração 00015 (D-04):
// um banco que já tinha catálogo no formato do registry, ao subir o binário
// atualizado, descarta esse catálogo inteiro e recebe a semente nova — antes
// de qualquer varredura — sem perder as conexões já cadastradas.
//
// O teste encena "banco de antes da 00015" sem reescrever a migração: sobe o
// schema inteiro (00015 e o que vier depois dela incluídas) uma vez, grava
// linhas no formato antigo por cima, e depois desfaz tudo que é >= 15 — tanto
// o registro de versão do goose quanto o schema que essas migrações criaram
// (ver a lista logo abaixo, no corpo do teste). No boot seguinte o goose acha
// que a 00015 (e o resto) nunca rodou e as aplica de novo — e é essa segunda
// aplicação da 00015, sobre um banco com lixo do registry, que prova o
// descarte.
func TestCatalogoDoRegistryEDescartadoNaAtualizacao(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cofre := cofreDeTeste(t)
	ctx := context.Background()

	// Sobe o schema inteiro uma vez, no formato de hoje (00015 já aplicada).
	st, err := store.Abrir(ctx, dir)
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}

	repoBib := biblioteca.NovoRepositorio(st.Leitura(), st.Escrita())
	itensDoRegistry := []biblioteca.Item{
		{
			Nome: "com.notion/mcp", Titulo: "Notion", Descricao: "MCP do Notion",
			Transporte: "http", URL: "https://mcp.notion.com/mcp",
		},
		{
			Nome: "io.github.fulano/x", Titulo: "X do Fulano", Descricao: "servidor stdio",
			Transporte: "stdio", Comando: "x-mcp", Args: []string{"--stdio"},
		},
	}
	if err := repoBib.Substituir(ctx, itensDoRegistry, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("gravar catálogo do registry: erro = %v, quer nil", err)
	}

	// Uma conexão já cadastrada, pelo mesmo caminho público que a UI usa.
	repoUp := upstream.NovoRepositorioSQLite(st.Leitura(), st.Escrita(), cofre)
	idUpstream, err := repoUp.Criar(ctx, upstream.Form{
		Nome: "notion", Tipo: upstream.TipoHTTP, URL: "https://mcp.notion.com/mcp",
		TimeoutMS: 15000, Habilitado: true,
	})
	if err != nil {
		t.Fatalf("cadastrar upstream: erro = %v, quer nil", err)
	}

	// Encena o estado anterior à 00015 de verdade, e não só o registro de
	// versão: apagar só a linha version_id = 15 do goose_db_version deixa uma
	// lacuna abaixo da versão mais alta aplicada (hoje 16, da 00016), e o goose
	// recusa isso como migração fora de ordem ("detected 1 missing
	// (out-of-order) migration lower than database version"). É preciso voltar
	// o esquema inteiro ao estado da 00014: apagar o registro de toda migração
	// >= 15 e desfazer o que cada uma criou.
	//
	// Lista do que desfazer, versão por versão — toda migração nova acima da
	// 00015 entra aqui:
	//   - 00015: só apaga linhas e zera estado, não cria objeto (o DROP INDEX
	//     dela já é IF EXISTS); nada a desfazer no schema.
	//   - 00016: ALTER TABLE ... ADD COLUMN endpoints; desfeito abaixo com DROP
	//     COLUMN (driver modernc.org/sqlite v1.58, SQLite recente — a própria
	//     00014 já usa DROP COLUMN no Down).
	if _, err := st.Escrita().ExecContext(ctx,
		"DELETE FROM goose_db_version WHERE version_id >= 15"); err != nil {
		t.Fatalf("desfazer o registro das migrações >= 15: erro = %v, quer nil", err)
	}
	if _, err := st.Escrita().ExecContext(ctx,
		"ALTER TABLE biblioteca_servidor DROP COLUMN endpoints"); err != nil {
		t.Fatalf("desfazer a coluna endpoints (00016): erro = %v, quer nil", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("fechar o banco de partida: erro = %v, quer nil", err)
	}

	// Semente nova, só com o formato mcpservers.org/<slug> — o que a migração
	// precisa entregar antes de qualquer varredura.
	geradoEm := time.Now().Add(-time.Hour)
	sementeNova := []biblioteca.Item{
		{
			Nome: "mcpservers.org/linear", Titulo: "Linear", Descricao: "Gestão de tarefas",
			Transporte: "http", URL: "https://mcp.linear.app/sse", Autenticacao: "oauth",
		},
		{
			Nome: "mcpservers.org/sentry", Titulo: "Sentry", Descricao: "Rastreamento de erro",
			Transporte: "http", URL: "https://mcp.sentry.dev/mcp", Autenticacao: "token",
		},
	}

	cfg := Config{
		Listen:    "127.0.0.1:0",
		DataDir:   dir,
		PublicURL: "http://127.0.0.1:8787",
		NivelLog:  slog.LevelError,
	}
	log := slog.New(slog.DiscardHandler)
	ctxApp, cancelar := context.WithCancel(context.Background())

	app, err := montar(ctxApp, cfg, cofre, log,
		ComSementeDaBiblioteca(sementeNova, geradoEm),
		// Servidor mudo: a varredura que a subida dispara em seguida falha, e a
		// asserção abaixo — logo após o boot — precisa valer antes dela ter
		// qualquer chance de mexer no catálogo.
		ComCuradoriaDaBiblioteca(curadoriaMudaDeTeste(t)),
	)
	if err != nil {
		t.Fatalf("montar: erro = %v, quer nil", err)
	}
	// Cancelar antes de fechar, na mesma função de limpeza: os laços de fundo só
	// devolvem o wg.Wait() de dentro de Fechar depois que o ctx morre. Duas
	// chamadas a t.Cleanup fariam LIFO chamar Fechar primeiro, e Fechar travaria
	// esperando um contexto que só seria cancelado depois.
	t.Cleanup(func() {
		cancelar()
		if err := app.Fechar(); err != nil {
			t.Errorf("fechar aplicação: erro = %v, quer nil", err)
		}
	})

	// Sinal de que uma varredura terminou — com sucesso ou não. É o que a nota
	// (g) da tarefa pede: a asserção abaixo é estável mesmo que a varredura
	// contra o servidor mudo falhe, porque semear roda antes dela em Manter, e
	// RegistrarFalha nunca toca no catálogo.
	varreu := make(chan struct{}, 1)
	app.sincBib.Observar(func() {
		select {
		case varreu <- struct{}{}:
		default:
		}
	})
	app.Iniciar(ctxApp)

	select {
	case <-varreu:
	case <-time.After(10 * time.Second):
		t.Fatal("a varredura depois do boot não terminou a tempo")
	}

	repoBibDepois := biblioteca.NovoRepositorio(app.st.Leitura(), app.st.Escrita())
	itens, total, err := repoBibDepois.Buscar(ctx, biblioteca.Filtro{}, 100, 0)
	if err != nil {
		t.Fatalf("buscar catálogo: erro = %v, quer nil", err)
	}
	if total != len(sementeNova) {
		t.Fatalf("catálogo tem %d item(ns), quer %d (só a semente nova)", total, len(sementeNova))
	}
	for _, item := range itens {
		if !strings.HasPrefix(item.Nome, "mcpservers.org/") {
			t.Errorf("item %q sobreviveu à atualização, formato do registry descartado", item.Nome)
		}
	}
	nomes := make(map[string]bool, len(itens))
	for _, item := range itens {
		nomes[item.Nome] = true
	}
	for _, quer := range sementeNova {
		if !nomes[quer.Nome] {
			t.Errorf("semente nova sem %q no catálogo", quer.Nome)
		}
	}

	// A conexão cadastrada antes da atualização continua íntegra.
	registro, err := app.repoUpstream.Obter(ctx, idUpstream)
	if err != nil {
		t.Fatalf("reler upstream depois da atualização: erro = %v, quer nil", err)
	}
	if registro.Nome != "notion" || registro.URL != "https://mcp.notion.com/mcp" {
		t.Errorf("upstream = %+v, quer nome=notion url=https://mcp.notion.com/mcp", registro)
	}
}
