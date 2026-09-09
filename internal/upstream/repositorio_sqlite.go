package upstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Erros sentinela do CRUD.
var (
	// ErrNaoEncontrado indica upstream que não existe no banco.
	ErrNaoEncontrado = errors.New("upstream: não encontrado")
	// ErrNomeEmUso indica nome já cadastrado.
	ErrNomeEmUso = errors.New("upstream: nome já em uso")
)

// RepositorioSQLite lê e escreve upstreams nos dois pools.
type RepositorioSQLite struct {
	leitura  *sql.DB
	escrita  *sql.DB
	cifrador Cifrador
}

// NovoRepositorioSQLite monta o repositório de upstreams.
//
// O cifrador entra por construtor porque as credenciais estáticas do upstream
// são a classe de segredo que precisa voltar em claro (seção 08.8): sem ele o
// repositório não teria como gravar bearer nem header.
func NovoRepositorioSQLite(leitura, escrita *sql.DB, cifrador Cifrador) *RepositorioSQLite {
	return &RepositorioSQLite{leitura: leitura, escrita: escrita, cifrador: cifrador}
}

const colunas = `id, nome, tipo, url, comando, args, env, timeout_ms, habilitado, ultimo_erro, modo_credencial`

func lerRegistro(scan func(...any) error) (Registro, error) {
	var (
		r          Registro
		args, ambi string
		habilitado int
	)
	if err := scan(&r.ID, &r.Nome, &r.Tipo, &r.URL, &r.Comando, &args, &ambi,
		&r.TimeoutMS, &habilitado, &r.UltimoErro, &r.Modo); err != nil {
		return Registro{}, err
	}
	var err error
	if r.Args, err = decodificarArgs(args); err != nil {
		return Registro{}, fmt.Errorf("upstream %s: %w", r.Nome, err)
	}
	if r.Env, err = decodificarEnv(ambi); err != nil {
		return Registro{}, fmt.Errorf("upstream %s: %w", r.Nome, err)
	}
	r.Habilitado = habilitado == 1
	return r, nil
}

// Todos devolve os upstreams cadastrados, em ordem de nome.
func (r *RepositorioSQLite) Todos(ctx context.Context) ([]Registro, error) {
	rows, err := r.leitura.QueryContext(ctx, `SELECT `+colunas+` FROM upstream ORDER BY nome`)
	if err != nil {
		return nil, fmt.Errorf("upstream: selecionar todos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Registro
	for rows.Next() {
		reg, err := lerRegistro(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("upstream: ler linha: %w", err)
		}
		out = append(out, reg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar todos: %w", err)
	}
	return out, nil
}

// Obter devolve um upstream pelo id.
func (r *RepositorioSQLite) Obter(ctx context.Context, id int64) (Registro, error) {
	reg, err := lerRegistro(r.leitura.QueryRowContext(ctx,
		`SELECT `+colunas+` FROM upstream WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Registro{}, ErrNaoEncontrado
	}
	if err != nil {
		return Registro{}, fmt.Errorf("upstream: selecionar %d: %w", id, err)
	}
	return reg, nil
}

// Criar grava um upstream HTTP e as credenciais estáticas do formulário.
//
// Uma transação só: um upstream criado sem o bearer que o admin acabou de
// digitar entraria em supervisão, falharia com 401 e pareceria erro do
// provedor.
func (r *RepositorioSQLite) Criar(ctx context.Context, f Form) (int64, error) {
	args, ambi, err := codificarProcesso(f)
	if err != nil {
		return 0, err
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("upstream: abrir transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO upstream (nome, tipo, url, comando, args, env, timeout_ms, habilitado,
                      modo_credencial, criado_em)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id`,
		f.Nome, f.TipoEfetivo(), f.URL, f.Comando, args, ambi,
		f.TimeoutMS, booleanoSQL(f.Habilitado), f.ModoEfetivo(), time.Now().Unix()).Scan(&id)
	if err != nil {
		if nomeEmUso(ctx, r.leitura, f.Nome, 0) {
			return 0, ErrNomeEmUso
		}
		return 0, fmt.Errorf("upstream: gravar %s: %w", f.Nome, err)
	}
	if err := r.aplicarCredenciais(ctx, tx, id, f); err != nil {
		return 0, err
	}
	if err := r.aplicarOAuth(ctx, tx, id, f); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("upstream: confirmar criação de %s: %w", f.Nome, err)
	}
	return id, nil
}

// Atualizar grava nome, destino (URL ou processo), timeout e a intenção de
// habilitar.
//
// tipo não está na lista de propósito: ele é escolhido na criação e não muda
// depois. Trocar o transporte de um upstream vivo não é editar o upstream, é
// trocá-lo por outro — as ferramentas, as credenciais e o modo de falha são
// outros —, e deixar o campo editável faria um clique errado parecer uma
// reconfiguração quando é uma substituição.
//
// ultimo_erro é zerado junto: ele é texto para a UI descrever a última falha, e
// depois de uma reconfiguração a falha antiga já não descreve nada.
func (r *RepositorioSQLite) Atualizar(ctx context.Context, id int64, f Form) error {
	args, ambi, err := codificarProcesso(f)
	if err != nil {
		return err
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upstream: abrir transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE upstream
   SET nome = ?, url = ?, comando = ?, args = ?, env = ?,
       timeout_ms = ?, habilitado = ?, modo_credencial = ?, ultimo_erro = ''
 WHERE id = ?`,
		f.Nome, f.URL, f.Comando, args, ambi,
		f.TimeoutMS, booleanoSQL(f.Habilitado), f.ModoEfetivo(), id)
	if err != nil {
		if nomeEmUso(ctx, r.leitura, f.Nome, id) {
			return ErrNomeEmUso
		}
		return fmt.Errorf("upstream: atualizar %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNaoEncontrado
	}
	if err := r.aplicarCredenciais(ctx, tx, id, f); err != nil {
		return err
	}
	if err := r.aplicarOAuth(ctx, tx, id, f); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upstream: confirmar atualização de %d: %w", id, err)
	}
	return nil
}

// Remover apaga o upstream. As linhas de composição saem por ON DELETE CASCADE.
func (r *RepositorioSQLite) Remover(ctx context.Context, id int64) error {
	res, err := r.escrita.ExecContext(ctx, `DELETE FROM upstream WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("upstream: apagar %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNaoEncontrado
	}
	return nil
}

// ContagemDeEndpoints devolve em quantos endpoints cada upstream entra.
func (r *RepositorioSQLite) ContagemDeEndpoints(ctx context.Context) (map[int64]int, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT upstream_id, COUNT(*) FROM endpoint_upstream GROUP BY upstream_id`)
	if err != nil {
		return nil, fmt.Errorf("upstream: contar endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]int)
	for rows.Next() {
		var (
			id int64
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("upstream: ler contagem de endpoints: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar contagem de endpoints: %w", err)
	}
	return out, nil
}

// EndpointsDo devolve os slugs dos endpoints que incluem o upstream.
//
// A tela mostra essa lista porque remover ou desabilitar um upstream muda o
// tools/list de todo endpoint que o inclui, e sem a lista o admin não sabe quais.
func (r *RepositorioSQLite) EndpointsDo(ctx context.Context, id int64) ([]string, error) {
	rows, err := r.leitura.QueryContext(ctx, `
SELECT e.slug
  FROM endpoint_upstream eu
  JOIN endpoint e ON e.id = eu.endpoint_id
 WHERE eu.upstream_id = ?
 ORDER BY e.slug`, id)
	if err != nil {
		return nil, fmt.Errorf("upstream: selecionar endpoints de %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("upstream: ler endpoint de %d: %w", id, err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar endpoints de %d: %w", id, err)
	}
	return out, nil
}

// codificarProcesso serializa os campos de processo do formulário.
//
// Num upstream HTTP eles saem vazios, e é a representação certa: o tipo é
// escolhido na criação e não muda depois, então um upstream HTTP nunca teve
// comando nenhum para preservar.
func codificarProcesso(f Form) (args, ambiente string, err error) {
	if args, err = codificarArgs(f.Args); err != nil {
		return "", "", err
	}
	if ambiente, err = codificarEnv(f.Env); err != nil {
		return "", "", err
	}
	return args, ambiente, nil
}

func booleanoSQL(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nomeEmUso(ctx context.Context, leitura *sql.DB, nome string, exceto int64) bool {
	var n int
	if err := leitura.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upstream WHERE nome = ? AND id <> ?`, nome, exceto).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
