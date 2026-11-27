//go:build ignore

// Gera tests/fixtures/segredos-go.db: um banco criado e migrado pelo Go (goose), com o canário e
// um segredo de cada tipo cifrado pelos repositórios de produção do Go, na chave mestra fixa da
// caixa-preta. O Rust decifra todos (src/plataforma/cripto.rs, teste fixture_do_go).
//
// Não é código de produção nem pacote do módulo (build tag ignore). Para regravar, da raiz:
//
//	go run ./rust/tests/fixtures/gerar_segredos_go.go rust/tests/fixtures/segredos-go.db
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// A mesma de rust/tests/apoio/mod.rs (CHAVE_MESTRA).
const chaveMestra = "cGF0Y2hiYXktY2FpeGEtcHJldGEtY2hhdmUtZml4YSE="

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "uso: go run gerar_segredos_go.go <saida.db>")
		os.Exit(2)
	}
	if err := gerar(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "gerar:", err)
		os.Exit(1)
	}
}

func gerar(ctx context.Context, saida string) error {
	dir, err := os.MkdirTemp("", "segredos-go-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	st, err := store.Abrir(ctx, dir)
	if err != nil {
		return err
	}
	defer st.Close()

	mestra, err := cripto.ChaveMestraDe(chaveMestra)
	if err != nil {
		return err
	}
	cofre, err := cripto.NovoCofre(mestra)
	if err != nil {
		return err
	}
	if _, err := cripto.VerificarCanario(ctx, cofre, store.NovasConfiguracoes(st.Leitura(), st.Escrita())); err != nil {
		return err
	}

	repo := upstream.NovoRepositorioSQLite(st.Leitura(), st.Escrita(), cofre)
	criar := func(f upstream.Form) (int64, error) {
		if !f.Validar() {
			return 0, fmt.Errorf("form %s inválido: %v", f.Nome, f.Erros)
		}
		return repo.Criar(ctx, f)
	}

	// 1: HTTP com bearer e header.
	http, err := criar(upstream.Form{Nome: "http-com-segredos", Tipo: "http", URL: "http://127.0.0.1:9/mcp",
		TimeoutMS: 1000, Habilitado: true, Modo: "estatica"})
	if err != nil {
		return err
	}
	if err := repo.DefinirCredencial(ctx, http, "bearer", "", "bearer-gravado-pelo-go"); err != nil {
		return err
	}
	if err := repo.DefinirCredencial(ctx, http, "header", "X-Api-Key", "header-gravado-pelo-go"); err != nil {
		return err
	}

	// 2: STDIO com variável de ambiente secreta.
	stdio, err := criar(upstream.Form{Nome: "stdio-com-env", Tipo: "stdio", Comando: "/bin/true",
		TimeoutMS: 1000, Habilitado: true, Modo: "estatica"})
	if err != nil {
		return err
	}
	if err := repo.DefinirCredencial(ctx, stdio, "env", "TOKEN_DO_GO", "env-gravado-pelo-go"); err != nil {
		return err
	}

	// 3: OAuth com client secret e concessão (access e refresh).
	oauth, err := criar(upstream.Form{Nome: "oauth-com-concessao", Tipo: "http", URL: "http://127.0.0.1:9/mcp",
		TimeoutMS: 1000, Habilitado: true, Modo: "oauth",
		OAuthClientID: "cliente-do-go", OAuthSegredo: "client-secret-gravado-pelo-go"})
	if err != nil {
		return err
	}
	if err := repo.GravarConcessao(ctx, oauth, upstream.Concessao{
		ClientIDEfetivo: "cliente-do-go", Registro: "preregistrado", URLToken: "http://127.0.0.1:9/token",
		Estilo: 1, Escopos: []string{"mcp"},
		Token: upstream.Token{Acesso: "access-gravado-pelo-go", Refresh: "refresh-gravado-pelo-go", Tipo: "Bearer"},
	}); err != nil {
		return err
	}

	// Um arquivo só, sem -wal: é o que vai para o repositório.
	_ = os.Remove(saida)
	abs, err := filepath.Abs(saida)
	if err != nil {
		return err
	}
	return vacuumInto(ctx, st.Escrita(), abs)
}

func vacuumInto(ctx context.Context, db *sql.DB, destino string) error {
	_, err := db.ExecContext(ctx, "VACUUM INTO ?", destino)
	return err
}
