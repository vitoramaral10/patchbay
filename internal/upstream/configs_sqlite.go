package upstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const sqlHabilitados = `
SELECT id, nome, tipo, url, comando, args, env, timeout_ms, modo_credencial,
       sonda_habilitada, sonda_ferramenta, sonda_args, sonda_espera,
       sonda_intervalo_ms, sonda_timeout_ms, sonda_tolerancia
  FROM upstream
 WHERE habilitado = 1
 ORDER BY nome`

// Habilitados lê do banco os upstreams que o admin quer no ar.
//
// habilitado é a intenção do admin e é o único estado persistido de um
// upstream: o estado da máquina vive em memória, e reiniciar o patchbay sempre
// recomeça em novo → conectando.
func Habilitados(ctx context.Context, leitura *sql.DB) ([]Config, error) {
	rows, err := leitura.QueryContext(ctx, sqlHabilitados)
	if err != nil {
		return nil, fmt.Errorf("upstream: selecionar habilitados: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Config
	for rows.Next() {
		var (
			c          Config
			args, ambi string
			timeoutMS  int64
			sonda      sondaBruta
		)
		if err := rows.Scan(&c.ID, &c.Nome, &c.Tipo, &c.URL, &c.Comando, &args, &ambi,
			&timeoutMS, &c.Modo,
			&sonda.habilitada, &sonda.ferramenta, &sonda.args, &sonda.espera,
			&sonda.intervaloMS, &sonda.timeoutMS, &sonda.tolerancia); err != nil {
			return nil, fmt.Errorf("upstream: ler linha: %w", err)
		}
		if c.Args, err = decodificarArgs(args); err != nil {
			return nil, fmt.Errorf("upstream %s: %w", c.Nome, err)
		}
		if c.Env, err = decodificarEnv(ambi); err != nil {
			return nil, fmt.Errorf("upstream %s: %w", c.Nome, err)
		}
		c.Timeout = time.Duration(timeoutMS) * time.Millisecond
		c.Sonda = sonda.sonda()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream: iterar habilitados: %w", err)
	}
	return out, nil
}

// sondaBruta são as colunas de sonda do jeito que o SQLite as devolve.
//
// Um tipo em vez de sete variáveis soltas em cada Scan: as colunas da sonda são
// lidas em dois lugares (Habilitados e o CRUD), e repetir a ordem delas à mão é
// como um campo novo entra num caminho e não no outro.
type sondaBruta struct {
	habilitada  int
	ferramenta  string
	args        string
	espera      string
	intervaloMS int64
	timeoutMS   int64
	tolerancia  int
}

// sonda traduz as colunas na configuração que a supervisão consome.
func (b sondaBruta) sonda() Sonda {
	return Sonda{
		Habilitada: b.habilitada == 1,
		Ferramenta: b.ferramenta,
		Args:       ArgsDeSonda(b.args),
		Espera:     b.espera,
		Intervalo:  time.Duration(b.intervaloMS) * time.Millisecond,
		Timeout:    time.Duration(b.timeoutMS) * time.Millisecond,
		Tolerancia: b.tolerancia,
	}
}

// Args e env viajam como JSON numa coluna de texto, que é a forma que o schema
// da seção 08.8 fixou. JSON e não string separada por espaço: argumento de
// processo com espaço dentro é normal (um caminho do Windows, por exemplo), e
// qualquer separador escolhido aqui viraria uma regra de escape para o admin
// descobrir sozinho.

func decodificarArgs(bruto string) ([]string, error) {
	if bruto == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(bruto), &out); err != nil {
		return nil, fmt.Errorf("decodificar args: %w", err)
	}
	return out, nil
}

func decodificarEnv(bruto string) (map[string]string, error) {
	if bruto == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(bruto), &out); err != nil {
		return nil, fmt.Errorf("decodificar env: %w", err)
	}
	return out, nil
}

func codificarArgs(args []string) (string, error) {
	if args == nil {
		args = []string{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("upstream: codificar args: %w", err)
	}
	return string(b), nil
}

func codificarEnv(env map[string]string) (string, error) {
	if env == nil {
		env = map[string]string{}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("upstream: codificar env: %w", err)
	}
	return string(b), nil
}
