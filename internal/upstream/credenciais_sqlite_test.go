package upstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

func repositorioDeTeste(t *testing.T) (*upstream.RepositorioSQLite, *store.Store) {
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

	texto, err := cripto.GerarChaveMestra()
	if err != nil {
		t.Fatalf("gerar chave mestra: erro = %v, quer nil", err)
	}
	mestra, err := cripto.ChaveMestraDe(texto)
	if err != nil {
		t.Fatalf("ler chave mestra: erro = %v, quer nil", err)
	}
	cofre, err := cripto.NovoCofre(mestra)
	if err != nil {
		t.Fatalf("montar cofre: erro = %v, quer nil", err)
	}
	return upstream.NovoRepositorioSQLite(st.Leitura(), st.Escrita(), cofre), st
}

func formBase(nome string) upstream.Form {
	return upstream.Form{
		Nome: nome, URL: "https://exemplo.com/mcp",
		TimeoutMS: upstream.TimeoutPadraoMS, Habilitado: true,
	}
}

// colunaCrua lê valor_cifrado direto do SQLite, sem passar pelo repositório: é o
// único jeito de provar que o que está gravado não é o valor em claro.
func colunaCrua(t *testing.T, st *store.Store, upstreamID int64, tipo, nome string) string {
	t.Helper()

	var valor string
	err := st.Leitura().QueryRowContext(context.Background(),
		`SELECT valor_cifrado FROM upstream_secret WHERE upstream_id = ? AND tipo = ? AND nome = ?`,
		upstreamID, tipo, nome).Scan(&valor)
	if err != nil {
		t.Fatalf("ler coluna crua de %s/%s: erro = %v, quer nil", tipo, nome, err)
	}
	return valor
}

// TestRepositorio_CredencialFicaCifradaNoBanco é o requisito da fatia lido do
// lado do disco: quem abre o arquivo .db não encontra a credencial.
func TestRepositorio_CredencialFicaCifradaNoBanco(t *testing.T) {
	t.Parallel()

	const (
		bearer = "sk-notion-0123456789-em-claro"
		chave  = "valor-do-header-em-claro"
	)

	sut, st := repositorioDeTeste(t)
	ctx := context.Background()

	f := formBase("notion")
	f.Bearer = bearer
	f.Headers = []upstream.CampoHeader{{Nome: "X-Api-Key", Valor: chave}}

	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	casos := map[string]struct {
		tipo, nome, claro string
	}{
		"bearer": {tipo: upstream.CredencialBearer, nome: "", claro: bearer},
		"header": {tipo: upstream.CredencialHeader, nome: "X-Api-Key", claro: chave},
	}
	for nome, tc := range casos {
		guardado := colunaCrua(t, st, id, tc.tipo, tc.nome)
		if strings.Contains(guardado, tc.claro) {
			t.Errorf("%s: coluna crua contém o valor em claro", nome)
		}
		if !strings.HasPrefix(guardado, "pbc1:") {
			t.Errorf("%s: coluna crua = %q, quer o formato versionado", nome, guardado)
		}
	}

	// E o caminho de volta devolve exatamente o que entrou.
	creds, err := sut.Credenciais(ctx, id)
	if err != nil {
		t.Fatalf("Credenciais: erro = %v, quer nil", err)
	}
	if len(creds) != 2 {
		t.Fatalf("credenciais = %d, quer 2", len(creds))
	}
	// A ordem é estável: bearer antes de header.
	if creds[0].Tipo != upstream.CredencialBearer || creds[0].Valor.Revelar() != bearer {
		t.Errorf("credencial[0] = %s/%s, quer o bearer em claro", creds[0].Tipo, creds[0].Nome)
	}
	if creds[1].Nome != "X-Api-Key" || creds[1].Valor.Revelar() != chave {
		t.Errorf("credencial[1] = %s/%s, quer X-Api-Key em claro", creds[1].Tipo, creds[1].Nome)
	}
}

// TestRepositorio_ValorEmBrancoMantemEOLimparApaga trava a semântica que a tela
// promete: campo vazio não é ordem de apagar.
func TestRepositorio_ValorEmBrancoMantemEOLimparApaga(t *testing.T) {
	t.Parallel()

	sut, _ := repositorioDeTeste(t)
	ctx := context.Background()

	f := formBase("notion")
	f.Bearer = "primeiro-bearer"
	f.Headers = []upstream.CampoHeader{{Nome: "X-Api-Key", Valor: "primeiro-header"}}
	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	// Uma edição que só mexeu no timeout: nada de credencial no formulário.
	manter := formBase("notion")
	manter.TimeoutMS = 20000
	manter.Headers = []upstream.CampoHeader{{Nome: "X-Api-Key", Definido: true}}
	if err := sut.Atualizar(ctx, id, manter); err != nil {
		t.Fatalf("Atualizar mantendo: erro = %v, quer nil", err)
	}
	creds, err := sut.Credenciais(ctx, id)
	if err != nil {
		t.Fatalf("Credenciais: erro = %v, quer nil", err)
	}
	if len(creds) != 2 {
		t.Fatalf("credenciais depois de manter = %d, quer 2", len(creds))
	}
	if creds[0].Valor.Revelar() != "primeiro-bearer" {
		t.Error("o bearer mudou numa edição que não o tocou")
	}

	// Trocar o bearer e apagar o header.
	trocar := formBase("notion")
	trocar.Bearer = "segundo-bearer"
	trocar.Headers = []upstream.CampoHeader{{Nome: "X-Api-Key", Definido: true, Limpar: true}}
	if err := sut.Atualizar(ctx, id, trocar); err != nil {
		t.Fatalf("Atualizar trocando: erro = %v, quer nil", err)
	}
	creds, err = sut.Credenciais(ctx, id)
	if err != nil {
		t.Fatalf("Credenciais: erro = %v, quer nil", err)
	}
	if len(creds) != 1 {
		t.Fatalf("credenciais depois de limpar o header = %d, quer 1", len(creds))
	}
	if creds[0].Valor.Revelar() != "segundo-bearer" {
		t.Errorf("bearer = %q, quer %q", creds[0].Valor.Revelar(), "segundo-bearer")
	}

	// E limpar o bearer também.
	limpar := formBase("notion")
	limpar.BearerLimpar = true
	if err := sut.Atualizar(ctx, id, limpar); err != nil {
		t.Fatalf("Atualizar limpando: erro = %v, quer nil", err)
	}
	if creds, err := sut.Credenciais(ctx, id); err != nil || len(creds) != 0 {
		t.Fatalf("credenciais = %v (erro = %v), quer nenhuma", creds, err)
	}
}

// TestRepositorio_CredenciaisDefinidasNaoDecifram: a tela precisa abrir mesmo
// com a chave mestra trocada, senão o diagnóstico vira um 500.
func TestRepositorio_CredenciaisDefinidasNaoDecifram(t *testing.T) {
	t.Parallel()

	sut, _ := repositorioDeTeste(t)
	ctx := context.Background()

	f := formBase("notion")
	f.Bearer = "um-bearer"
	f.Headers = []upstream.CampoHeader{{Nome: "X-Api-Key", Valor: "um-header"}}
	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	definidas, err := sut.CredenciaisDefinidas(ctx, id)
	if err != nil {
		t.Fatalf("CredenciaisDefinidas: erro = %v, quer nil", err)
	}
	quer := []upstream.CredencialDefinida{
		{Tipo: upstream.CredencialBearer},
		{Tipo: upstream.CredencialHeader, Nome: "X-Api-Key"},
	}
	if len(definidas) != len(quer) {
		t.Fatalf("definidas = %v, quer %v", definidas, quer)
	}
	for i := range quer {
		if definidas[i] != quer[i] {
			t.Errorf("definidas[%d] = %v, quer %v", i, definidas[i], quer[i])
		}
	}
}

// TestRepositorio_RemoverUpstreamLevaAsCredenciais: sem o cascade, o segredo
// sobreviveria ao upstream que ele autenticava.
func TestRepositorio_RemoverUpstreamLevaAsCredenciais(t *testing.T) {
	t.Parallel()

	sut, st := repositorioDeTeste(t)
	ctx := context.Background()

	f := formBase("notion")
	f.Bearer = "um-bearer"
	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}
	if err := sut.Remover(ctx, id); err != nil {
		t.Fatalf("Remover: erro = %v, quer nil", err)
	}

	var n int
	if err := st.Leitura().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upstream_secret WHERE upstream_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("contar credenciais: erro = %v, quer nil", err)
	}
	if n != 0 {
		t.Errorf("credenciais órfãs = %d, quer 0", n)
	}
}

// TestRepositorio_CredencialDeOutraChaveNaoVolta é o canário visto do lado da
// credencial: chave trocada dá erro explícito, nunca valor errado em silêncio.
func TestRepositorio_CredencialDeOutraChaveNaoVolta(t *testing.T) {
	t.Parallel()

	sut, st := repositorioDeTeste(t)
	ctx := context.Background()

	f := formBase("notion")
	f.Bearer = "um-bearer"
	id, err := sut.Criar(ctx, f)
	if err != nil {
		t.Fatalf("Criar: erro = %v, quer nil", err)
	}

	outroTexto, err := cripto.GerarChaveMestra()
	if err != nil {
		t.Fatalf("gerar chave mestra: erro = %v, quer nil", err)
	}
	outraChave, err := cripto.ChaveMestraDe(outroTexto)
	if err != nil {
		t.Fatalf("ler chave mestra: erro = %v, quer nil", err)
	}
	outroCofre, err := cripto.NovoCofre(outraChave)
	if err != nil {
		t.Fatalf("montar cofre: erro = %v, quer nil", err)
	}
	comOutraChave := upstream.NovoRepositorioSQLite(st.Leitura(), st.Escrita(), outroCofre)

	_, err = comOutraChave.Credenciais(ctx, id)
	if !errors.Is(err, cripto.ErrAutenticacao) {
		t.Fatalf("erro = %v, quer %v", err, cripto.ErrAutenticacao)
	}
}
