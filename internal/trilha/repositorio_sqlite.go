package trilha

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Relogio é o que o cache de Opcoes usa para decidir se os 60 s do TTL já
// venceram. Só este método, e não time.Now direto: um teste que precisa do
// TTL vencido não pode depender do relógio de verdade.
type Relogio interface{ Agora() time.Time }

type relogioReal struct{}

func (relogioReal) Agora() time.Time { return time.Now() }

// ttlOpcoes é quanto tempo o resultado de Opcoes fica em cache. Os três
// SELECT DISTINCT que ela roda escaneiam o índice inteiro, e a tela de trilha
// chama Opcoes a cada carregamento — inclusive a cada F5 de quem está com a
// tela aberta ao lado do log ao vivo. Um minuto de atraso para um endpoint ou
// ferramenta novos aparecerem no seletor é barato contra escanear a tabela a
// cada requisição.
const ttlOpcoes = 60 * time.Second

// RepositorioSQLite é a trilha na tabela call_log.
//
// Dois pools, como o resto do patchbay: a tela lê pelo pool sem limite e o
// escritor único grava. A trilha é a escrita mais frequente do sistema, e é essa
// separação que impede o log de serializar o gateway (seção 08.8).
type RepositorioSQLite struct {
	leitura *sql.DB
	escrita *sql.DB

	relogio Relogio

	muOpcoes    sync.Mutex
	opcoesCache Opcoes
	opcoesEm    time.Time
}

// OpcaoRepo ajusta o RepositorioSQLite na construção.
type OpcaoRepo func(*RepositorioSQLite)

// ComRelogio troca o relógio do cache de Opcoes. Uso de teste: sem isto, um
// teste do TTL teria de esperar 60 s de verdade.
func ComRelogio(r Relogio) OpcaoRepo {
	return func(rp *RepositorioSQLite) { rp.relogio = r }
}

// NovoRepositorioSQLite monta o acesso à trilha sobre os dois pools.
func NovoRepositorioSQLite(leitura, escrita *sql.DB, opcoes ...OpcaoRepo) *RepositorioSQLite {
	r := &RepositorioSQLite{leitura: leitura, escrita: escrita, relogio: relogioReal{}}
	for _, o := range opcoes {
		o(r)
	}
	return r
}

const sqlInserir = `
INSERT INTO call_log (
    ts, endpoint_id, endpoint_slug, upstream_id, upstream_nome,
    ferramenta, ferramenta_original, resultado, erro, duracao_ms,
    bytes_entrada, bytes_saida, sessao, credencial, era, origem
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Gravar insere o lote inteiro numa transação.
//
// Uma transação por lote, e não por linha: em WAL cada COMMIT é um fsync, e o
// que amortiza o custo do escritor único é justamente agrupar. O statement é
// preparado uma vez e reusado no lote.
func (r *RepositorioSQLite) Gravar(ctx context.Context, eventos []Evento) error {
	if len(eventos) == 0 {
		return nil
	}
	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("trilha: abrir transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, sqlInserir)
	if err != nil {
		return fmt.Errorf("trilha: preparar inserção: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range eventos {
		_, err := stmt.ExecContext(ctx,
			e.Inicio.UTC().UnixMilli(), e.EndpointID, e.EndpointSlug, e.UpstreamID, e.UpstreamNome,
			e.Ferramenta, e.Original, string(e.Resultado), e.Erro, e.DuracaoMS(),
			e.BytesEntrada, e.BytesSaida, e.Sessao, e.Credencial, e.Era, string(e.Origem.OuCliente()))
		if err != nil {
			return fmt.Errorf("trilha: gravar chamada de %s: %w", e.Ferramenta, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("trilha: confirmar lote de %d: %w", len(eventos), err)
	}
	return nil
}

// Podar apaga até limite linhas mais antigas que antesDe.
//
// O DELETE tem LIMIT por subconsulta de id, e não um `DELETE ... LIMIT`: o
// modernc.org/sqlite não é compilado com SQLITE_ENABLE_UPDATE_DELETE_LIMIT, e a
// subconsulta é a forma portátil de limitar. O índice em ts é o que a torna
// barata.
func (r *RepositorioSQLite) Podar(ctx context.Context, antesDe time.Time, limite int) (int64, error) {
	if limite <= 0 {
		return 0, nil
	}
	res, err := r.escrita.ExecContext(ctx, `
DELETE FROM call_log
 WHERE id IN (SELECT id FROM call_log WHERE ts < ? ORDER BY ts LIMIT ?)`,
		antesDe.UTC().UnixMilli(), limite)
	if err != nil {
		return 0, fmt.Errorf("trilha: apagar linhas anteriores a %s: %w",
			antesDe.UTC().Format(time.RFC3339), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("trilha: contar linhas apagadas: %w", err)
	}
	return n, nil
}

const colunas = `
    id, ts, endpoint_id, endpoint_slug, upstream_id, upstream_nome,
    ferramenta, ferramenta_original, resultado, erro, duracao_ms,
    bytes_entrada, bytes_saida, sessao, credencial, era, origem`

// Listar devolve a página de eventos que casa com o filtro, mais nova antes.
//
// O segundo retorno diz se existe página seguinte. Vem de pedir uma linha além
// do tamanho da página, e não de um COUNT(*): a contagem exata varreria a tabela
// inteira a cada troca de filtro para produzir um número que a tela usa só para
// habilitar um botão.
func (r *RepositorioSQLite) Listar(ctx context.Context, f Filtro) ([]Evento, bool, error) {
	f = f.Normalizado()

	var (
		onde strings.Builder
		args []any
	)
	onde.WriteString(" WHERE 1 = 1")
	if f.Endpoint != "" {
		onde.WriteString(" AND endpoint_slug = ?")
		args = append(args, f.Endpoint)
	}
	if f.Upstream != "" {
		onde.WriteString(" AND upstream_nome = ?")
		args = append(args, f.Upstream)
	}
	if f.Ferramenta != "" {
		// Casamento por conteúdo, e não igualdade: o admin lembra do sufixo da
		// ferramenta com mais frequência do que do prefixo que a composição
		// colou nela. O ESCAPE existe porque _ e % são curinga do LIKE e são
		// caracteres legítimos de nome de ferramenta.
		onde.WriteString(` AND (ferramenta LIKE ? ESCAPE '\' OR ferramenta_original LIKE ? ESCAPE '\')`)
		padrao := "%" + escaparLike(f.Ferramenta) + "%"
		args = append(args, padrao, padrao)
	}
	if f.Resultado != "" {
		onde.WriteString(" AND resultado = ?")
		args = append(args, string(f.Resultado))
	}
	if f.Origem != "" {
		onde.WriteString(" AND origem = ?")
		args = append(args, string(f.Origem))
	}
	if !f.Desde.IsZero() {
		onde.WriteString(" AND ts >= ?")
		args = append(args, f.Desde.UTC().UnixMilli())
	}
	if !f.Ate.IsZero() {
		onde.WriteString(" AND ts <= ?")
		args = append(args, f.Ate.UTC().UnixMilli())
	}
	if f.TemCursor() {
		// Paginação por keyset, e não por OFFSET: um OFFSET grande faz o
		// SQLite contar e descartar cada linha das páginas já vistas antes de
		// chegar na atual, um custo que cresce com a página. A comparação de
		// tupla "(ts, id) < (?, ?)" pula direto para depois da última linha
		// vista, e o índice em ts resolve sem escanear o que já passou.
		onde.WriteString(" AND (ts, id) < (?, ?)")
		args = append(args, f.CursorTS, f.CursorID)
	}

	// id como critério de desempate: sob rajada várias linhas caem no mesmo
	// milissegundo, e sem ele a paginação repetiria ou pularia linha.
	//
	//nolint:gosec // G202: todo pedaço concatenado é constante deste arquivo —
	// onde.String() é montado só com os literais acima, e cada valor do filtro
	// entra por placeholder, nunca no texto. É o preço de um WHERE de cinco
	// eixos opcionais: a alternativa (`? = '' OR coluna = ?` para cada um) é SQL
	// estático e engana o planejador, que perde o índice de ts.
	consulta := "SELECT" + colunas + " FROM call_log" + onde.String() +
		" ORDER BY ts DESC, id DESC LIMIT ?"
	args = append(args, f.PorPagina+1)

	rows, err := r.leitura.QueryContext(ctx, consulta, args...)
	if err != nil {
		return nil, false, fmt.Errorf("trilha: selecionar chamadas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	eventos := make([]Evento, 0, f.PorPagina)
	for rows.Next() {
		e, err := lerEvento(rows)
		if err != nil {
			return nil, false, err
		}
		eventos = append(eventos, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("trilha: iterar chamadas: %w", err)
	}

	temMais := len(eventos) > f.PorPagina
	if temMais {
		eventos = eventos[:f.PorPagina]
	}
	return eventos, temMais, nil
}

func lerEvento(rows *sql.Rows) (Evento, error) {
	var (
		e         Evento
		ts        int64
		duracaoMS int64
		resultado string
		origem    string
	)
	err := rows.Scan(
		&e.ID, &ts, &e.EndpointID, &e.EndpointSlug, &e.UpstreamID, &e.UpstreamNome,
		&e.Ferramenta, &e.Original, &resultado, &e.Erro, &duracaoMS,
		&e.BytesEntrada, &e.BytesSaida, &e.Sessao, &e.Credencial, &e.Era, &origem)
	if err != nil {
		return Evento{}, fmt.Errorf("trilha: ler chamada: %w", err)
	}
	e.Inicio = time.UnixMilli(ts).UTC()
	e.Duracao = time.Duration(duracaoMS) * time.Millisecond
	e.Resultado = Resultado(resultado)
	e.Origem = Origem(origem)
	return e, nil
}

// Resumo conta as chamadas de uma janela recente para os contadores do painel.
//
// Só as de cliente: a sondagem funcional é diagnóstico, não tráfego, e
// misturá-la aqui inflaria a taxa por minuto e o contador de erros com o custo
// da própria observação. Uma sonda a cada 15 min num gateway parado faria o
// painel dizer "0,07 chamadas/min" sobre um sistema que ninguém usou, e uma
// sonda quebrada faria "100% de erro" sobre chamadas de cliente que nunca
// existiram. Quem quer ver a sonda filtra por origem na tabela abaixo.
func (r *RepositorioSQLite) Resumo(ctx context.Context, janela time.Duration) (Resumo, error) {
	desde := time.Now().Add(-janela)
	var res Resumo
	res.Janela = janela
	err := r.leitura.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN resultado = 'erro'    THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN resultado = 'timeout' THEN 1 ELSE 0 END), 0),
       COALESCE(MAX(duracao_ms), 0)
  FROM call_log
 WHERE ts >= ? AND origem = 'cliente'`, desde.UTC().UnixMilli()).
		Scan(&res.Chamadas, &res.Erros, &res.Timeouts, &res.PiorDuracaoMS)
	if err != nil {
		return Resumo{}, fmt.Errorf("trilha: resumir chamadas: %w", err)
	}
	return res, nil
}

// limiteOpcoes é o teto de valores distintos oferecidos em cada filtro. Um
// gateway pessoal não passa disso, e o teto impede que uma tabela grande
// transforme a montagem da tela num scan sem fim.
const limiteOpcoes = 200

// As três consultas dos seletores, cada uma escrita por extenso.
//
// Por extenso e não montada com o nome da coluna interpolado: não existe
// placeholder para identificador em SQL, então a única defesa de uma consulta
// montada seria a origem do texto — e "confie em mim, esta string é constante" é
// exatamente o que o revisor não consegue verificar de relance.
const (
	sqlEndpointsDistintos = `
SELECT DISTINCT endpoint_slug FROM call_log
 WHERE endpoint_slug <> '' ORDER BY endpoint_slug LIMIT ?`

	sqlUpstreamsDistintos = `
SELECT DISTINCT upstream_nome FROM call_log
 WHERE upstream_nome <> '' ORDER BY upstream_nome LIMIT ?`

	sqlFerramentasDistintas = `
SELECT DISTINCT ferramenta FROM call_log
 WHERE ferramenta <> '' ORDER BY ferramenta LIMIT ?`
)

// Opcoes devolve os valores distintos que os seletores da tela oferecem, em
// cache por ttlOpcoes.
//
// Sai da própria trilha, e não das tabelas de endpoint e de upstream: o que
// interessa filtrar é o que aparece na trilha, incluindo o endpoint que já foi
// apagado — a linha dele continua ali de propósito (migração 00011).
func (r *RepositorioSQLite) Opcoes(ctx context.Context) (Opcoes, error) {
	r.muOpcoes.Lock()
	if !r.opcoesEm.IsZero() && r.relogio.Agora().Sub(r.opcoesEm) < ttlOpcoes {
		o := r.opcoesCache
		r.muOpcoes.Unlock()
		return o, nil
	}
	r.muOpcoes.Unlock()

	var o Opcoes
	var err error
	if o.Endpoints, err = r.distintos(ctx, sqlEndpointsDistintos, "endpoint"); err != nil {
		return Opcoes{}, err
	}
	if o.Upstreams, err = r.distintos(ctx, sqlUpstreamsDistintos, "upstream"); err != nil {
		return Opcoes{}, err
	}
	if o.Ferramentas, err = r.distintos(ctx, sqlFerramentasDistintas, "ferramenta"); err != nil {
		return Opcoes{}, err
	}

	r.muOpcoes.Lock()
	r.opcoesCache, r.opcoesEm = o, r.relogio.Agora()
	r.muOpcoes.Unlock()
	return o, nil
}

// distintos roda uma das consultas acima. consulta é sempre uma constante deste
// arquivo; coluna só entra na mensagem de erro.
func (r *RepositorioSQLite) distintos(ctx context.Context, consulta, coluna string) ([]string, error) {
	rows, err := r.leitura.QueryContext(ctx, consulta, limiteOpcoes)
	if err != nil {
		return nil, fmt.Errorf("trilha: valores distintos de %s: %w", coluna, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("trilha: ler %s: %w", coluna, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trilha: iterar %s: %w", coluna, err)
	}
	return out, nil
}

// escaparLike neutraliza os curingas do LIKE no texto que o admin digitou.
func escaparLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	return strings.ReplaceAll(s, "_", `\_`)
}
