// Package store abre o SQLite que é a única fonte de verdade do patchbay e
// aplica as migrações embutidas.
//
// São dois pools sobre o mesmo arquivo, como a seção 08.8 do estudo prévio
// descreve: leitura sem limite de conexões e escrita com uma única conexão.
// SQLite aceita um escritor por vez mesmo em WAL; serializar a escrita no Go
// troca SQLITE_BUSY por espera na fila do database/sql.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	// Driver SQLite puro Go: sem cgo, o cross-compile Windows→Linux e a imagem
	// distroless continuam funcionando.
	_ "modernc.org/sqlite"
)

// NomeArquivo é o nome do arquivo de banco dentro do diretório de dados.
const NomeArquivo = "patchbay.db"

// ErrDiretorioVazio indica que Abrir foi chamado sem diretório de dados.
var ErrDiretorioVazio = errors.New("store: diretório de dados vazio")

// Store guarda os dois pools sobre o mesmo arquivo SQLite.
//
// Leitura e Escrita devolvem *sql.DB para que cada feature declare o que
// precisa sem importar este pacote em assinatura de construtor.
type Store struct {
	leitura *sql.DB
	escrita *sql.DB
	caminho string
}

// Abrir abre o banco em dir, aplica as migrações embutidas e devolve os dois
// pools prontos. O diretório precisa existir.
func Abrir(ctx context.Context, dir string) (*Store, error) {
	if dir == "" {
		return nil, ErrDiretorioVazio
	}
	caminho := filepath.Join(dir, NomeArquivo)

	escrita, err := abrirPool(ctx, caminho, true)
	if err != nil {
		return nil, fmt.Errorf("store: pool de escrita: %w", err)
	}
	leitura, err := abrirPool(ctx, caminho, false)
	if err != nil {
		_ = escrita.Close()
		return nil, fmt.Errorf("store: pool de leitura: %w", err)
	}

	s := &Store{leitura: leitura, escrita: escrita, caminho: caminho}
	if err := Migrar(ctx, escrita); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Leitura devolve o pool de leitura, sem limite de conexões.
func (s *Store) Leitura() *sql.DB { return s.leitura }

// Escrita devolve o pool de escrita, com uma única conexão.
func (s *Store) Escrita() *sql.DB { return s.escrita }

// Caminho devolve o arquivo de banco em uso.
func (s *Store) Caminho() string { return s.caminho }

// Close fecha os dois pools.
func (s *Store) Close() error {
	return errors.Join(s.leitura.Close(), s.escrita.Close())
}

// abrirPool monta o DSN com os pragmas da seção 08.8 e ajusta os limites do
// pool conforme ele seja o de escrita ou o de leitura.
func abrirPool(ctx context.Context, caminho string, escritor bool) (*sql.DB, error) {
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_synchronous", "NORMAL")
	q.Set("_foreign_keys", "1")
	if escritor {
		// BEGIN IMMEDIATE: evita upgrade de lock no meio da transação.
		q.Set("_txlock", "immediate")
	}
	dsn := "file:" + filepath.ToSlash(caminho) + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrir %s: %w", caminho, err)
	}
	if escritor {
		db.SetMaxOpenConns(1)
	}
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s: %w", caminho, err)
	}
	return db, nil
}
