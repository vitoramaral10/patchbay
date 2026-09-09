package biblioteca

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RepositorioSQLite é a cópia local do catálogo.
//
// Dois pools porque o store separa leitura de escrita: a tela lê a cada
// abertura, e quem escreve é só a sincronização, uma vez por ciclo.
type RepositorioSQLite struct {
	leitura *sql.DB
	escrita *sql.DB
}

// NovoRepositorio monta o repositório sobre os pools do store.
func NovoRepositorio(leitura, escrita *sql.DB) *RepositorioSQLite {
	return &RepositorioSQLite{leitura: leitura, escrita: escrita}
}

// Sincronizacao é o estado da última varredura da origem.
//
// A tela mostra isto junto da lista porque catálogo guardado tem idade, e idade
// escondida é o que faz o admin procurar um servidor que existe e concluir que o
// patchbay está quebrado.
type Sincronizacao struct {
	// ConcluidaEm é quando a última varredura terminou inteira. Zero quando
	// nenhuma terminou — instalação nova, com a primeira ainda rodando.
	ConcluidaEm time.Time
	// TentadaEm é quando a última tentativa começou, tenha ela terminado ou não.
	TentadaEm time.Time
	// Servidores é quantos ficaram gravados na última varredura completa.
	Servidores int
	// Erro é o motivo da última tentativa que não terminou. Vazio quando deu
	// certo.
	Erro string
}

// Nunca diz se o catálogo ainda não existe.
func (s Sincronizacao) Nunca() bool { return s.ConcluidaEm.IsZero() }

// Idade é há quanto tempo o catálogo local foi montado.
func (s Sincronizacao) Idade() time.Duration {
	if s.Nunca() {
		return 0
	}
	return time.Since(s.ConcluidaEm)
}

// colunas é a projeção usada em toda leitura, para as duas consultas lerem os
// campos na mesma ordem.
const colunas = `nome, titulo, descricao, versao, transporte, url, comando, args,
	pede_credencial, site, autenticacao, curado`

// Filtro é o que a tela pede ao catálogo.
//
// Struct e não mais parâmetros soltos porque já são dois critérios que andam
// juntos em toda consulta — o termo e o recorte de origem —, e um terceiro
// parâmetro string ao lado de dois ints é onde a chamada errada passa a
// compilar.
type Filtro struct {
	// Termo é a busca livre, sobre nome, título e descrição.
	Termo string
	// SoCurados deixa passar só o que está na lista de remotos do
	// mcpservers.org — a curadoria feita a dedo, e o sinal de confiança mais
	// forte que a biblioteca tem.
	SoCurados bool
}

// Vazio diz se o filtro deixa passar o catálogo inteiro.
func (f Filtro) Vazio() bool {
	return strings.TrimSpace(f.Termo) == "" && !f.SoCurados
}

// Buscar devolve uma página do catálogo local, filtrada pelo termo.
//
// O total volta junto porque a tela precisa dele para paginar: diferente do
// cursor opaco da origem, aqui o catálogo é nosso e dá para dizer quantos são.
//
// A busca é sobre a coluna busca — nome, título e descrição já juntos e em
// minúsculas —, exigindo todos os pedaços do termo em qualquer ordem. Substring
// e não prefixo: quem procura "jira" precisa achar o Atlassian, cuja descrição
// cita Jira e cujo nome não.
func (r *RepositorioSQLite) Buscar(
	ctx context.Context, f Filtro, limite, deslocamento int,
) ([]Item, int, error) {
	onde, args := filtroDe(f)

	var total int
	if err := r.leitura.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM biblioteca_servidor"+onde, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("biblioteca: contar: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// Curados primeiro, depois por nome.
	//
	// A ordem precisa ser estável — sem ORDER BY, duas páginas seguidas podem
	// repetir e pular linhas — e o nome sozinho enterrava o que importa: medido
	// numa varredura de verdade em 2026-09-09, buscar "neon" trazia
	// "br.com.nineoneninetwo/9192" antes do Neon, porque "neon" está dentro de
	// "nineoneninetwo" e o b vem antes do m de "mcpservers.org/neon".
	//
	// Relevância de texto exigiria pontuação que o LIKE não dá. Curadoria é a
	// aproximação honesta: são algumas centenas escolhidas a dedo contra dezenas
	// de milhares auto-publicadas, e quem busca quase sempre quer uma delas.
	// O gosec marca as duas consultas acima e abaixo como SQL montado por
	// concatenação. Os pedaços concatenados são constantes deste arquivo:
	// colunas é literal, e onde vem de filtroDe, que só emite "busca LIKE ?"
	// repetido. O termo do admin nunca entra na string — ele viaja em args,
	// como placeholder.
	//
	//nolint:gosec // ver o parágrafo acima: nada de fora entra na consulta
	linhas, err := r.leitura.QueryContext(ctx,
		"SELECT "+colunas+" FROM biblioteca_servidor"+onde+
			" ORDER BY curado DESC, nome LIMIT ? OFFSET ?",
		append(args, limite, deslocamento)...)
	if err != nil {
		return nil, 0, fmt.Errorf("biblioteca: listar: %w", err)
	}
	defer func() { _ = linhas.Close() }()

	itens := make([]Item, 0, limite)
	for linhas.Next() {
		item, err := lerLinha(linhas)
		if err != nil {
			return nil, 0, err
		}
		itens = append(itens, item)
	}
	if err := linhas.Err(); err != nil {
		return nil, 0, fmt.Errorf("biblioteca: listar: %w", err)
	}
	return itens, total, nil
}

// Um lê um servidor pelo nome.
func (r *RepositorioSQLite) Um(ctx context.Context, nome string) (Item, error) {
	if !nomeValido(nome) {
		return Item{}, ErrNaoEncontrado
	}
	linha := r.leitura.QueryRowContext(ctx,
		"SELECT "+colunas+" FROM biblioteca_servidor WHERE nome = ?", nome)
	item, err := lerLinha(linha)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNaoEncontrado
	}
	return item, err
}

// escaneavel é o que sql.Row e sql.Rows têm em comum, para lerLinha servir aos
// dois caminhos de leitura.
type escaneavel interface{ Scan(dest ...any) error }

func lerLinha(l escaneavel) (Item, error) {
	var i Item
	var args string
	var pede, curado int
	if err := l.Scan(&i.Nome, &i.Titulo, &i.Descricao, &i.Versao,
		&i.Transporte, &i.URL, &i.Comando, &args, &pede, &i.Site,
		&i.Autenticacao, &curado); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Item{}, err
		}
		return Item{}, fmt.Errorf("biblioteca: ler servidor: %w", err)
	}
	i.PedeCredencial = pede != 0
	i.Curado = curado != 0
	if args != "" {
		if err := json.Unmarshal([]byte(args), &i.Args); err != nil {
			// Args ilegível é linha que este código gravou e não sabe mais ler.
			// Devolver o item sem argumentos abriria um formulário com o comando
			// certo e a execução incompleta, que é o erro caro.
			return Item{}, fmt.Errorf("biblioteca: args de %s: %w", i.Nome, err)
		}
	}
	return i, nil
}

// filtroDe monta o WHERE da busca. Filtro vazio devolve tudo.
//
// Cada pedaço do termo vira um LIKE com ESCAPE: sem ele, um termo com % ou _
// viraria curinga, e quem digitasse "100%" receberia o catálogo inteiro.
func filtroDe(f Filtro) (string, []any) {
	condicoes := make([]string, 0, 8)
	args := make([]any, 0, 8)

	for _, c := range strings.Fields(strings.ToLower(strings.TrimSpace(f.Termo))) {
		condicoes = append(condicoes, `busca LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escaparLike(c)+"%")
	}

	if f.SoCurados {
		// Coluna gravada na sincronização, e não regra derivada do nome: a
		// curadoria vem de outra origem, e não há como inferi-la daqui.
		condicoes = append(condicoes, "curado = 1")
	}

	if len(condicoes) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(condicoes, " AND "), args
}

func escaparLike(s string) string {
	r := strings.NewReplacer(`\`, `\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}

// Substituir troca o catálogo inteiro pelo que a varredura trouxe.
//
// Troca e não mescla, numa transação só. Mesclar deixaria para sempre o
// servidor que saiu do registry — e um servidor que saiu de lá saiu por algum
// motivo, quase sempre porque o endpoint morreu. Com a troca inteira, o
// catálogo local é sempre uma foto de uma varredura, nunca a soma de várias.
//
// Como é uma transação só, quem estiver lendo a tela durante a troca continua
// vendo a foto anterior inteira, e passa a ver a nova inteira. Não existe
// instante em que a biblioteca aparece vazia.
func (r *RepositorioSQLite) Substituir(ctx context.Context, itens []Item, quando time.Time) error {
	// Varredura que voltou vazia não apaga o catálogo. Zero servidor é sempre
	// defeito — a origem mudou de esquema, ou respondeu página vazia por engano
	// —, e trocar uma foto boa por uma vazia deixaria a tela pior do que a
	// origem estar fora do ar.
	if len(itens) == 0 {
		return fmt.Errorf("%w: a varredura não trouxe nenhum servidor", ErrFormatoDaOrigem)
	}

	tx, err := r.escrita.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("biblioteca: abrir transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM biblioteca_servidor"); err != nil {
		return fmt.Errorf("biblioteca: limpar catálogo: %w", err)
	}
	inserir, err := tx.PrepareContext(ctx, `
		INSERT INTO biblioteca_servidor
			(nome, titulo, descricao, versao, transporte, url, comando, args,
			 pede_credencial, site, autenticacao, curado, busca)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("biblioteca: preparar inserção: %w", err)
	}
	defer func() { _ = inserir.Close() }()

	gravados := 0
	for _, i := range itens {
		args, err := json.Marshal(i.Args)
		if err != nil {
			return fmt.Errorf("biblioteca: args de %s: %w", i.Nome, err)
		}
		if _, err := inserir.ExecContext(ctx,
			i.Nome, i.Titulo, i.Descricao, i.Versao, i.Transporte, i.URL,
			i.Comando, string(args), booleano(i.PedeCredencial), i.Site,
			i.Autenticacao, booleano(i.Curado), textoDeBusca(i),
		); err != nil {
			return fmt.Errorf("biblioteca: gravar %s: %w", i.Nome, err)
		}
		gravados++
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE biblioteca_sincronizacao
		   SET concluida_em = ?, tentada_em = ?, servidores = ?, erro = ''
		 WHERE id = 1`,
		quando.Unix(), quando.Unix(), gravados,
	); err != nil {
		return fmt.Errorf("biblioteca: gravar sincronização: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("biblioteca: concluir transação: %w", err)
	}
	return nil
}

// RegistrarFalha guarda que a varredura não terminou, sem tocar no catálogo.
//
// O catálogo anterior continua servindo: ele tem idade, e a idade é mostrada na
// tela junto com este erro. Apagar o que já existe porque a origem caiu seria
// trocar dado velho e útil por tela vazia.
func (r *RepositorioSQLite) RegistrarFalha(ctx context.Context, quando time.Time, motivo error) error {
	if _, err := r.escrita.ExecContext(ctx, `
		UPDATE biblioteca_sincronizacao
		   SET tentada_em = ?, erro = ?
		 WHERE id = 1`,
		quando.Unix(), motivo.Error(),
	); err != nil {
		return fmt.Errorf("biblioteca: gravar falha de sincronização: %w", err)
	}
	return nil
}

// Sincronizacao lê o estado da última varredura.
func (r *RepositorioSQLite) Sincronizacao(ctx context.Context) (Sincronizacao, error) {
	var concluida, tentada int64
	var s Sincronizacao
	err := r.leitura.QueryRowContext(ctx, `
		SELECT concluida_em, tentada_em, servidores, erro
		  FROM biblioteca_sincronizacao WHERE id = 1`,
	).Scan(&concluida, &tentada, &s.Servidores, &s.Erro)
	if errors.Is(err, sql.ErrNoRows) {
		// A linha nasce na migração. Não existir é banco de outra versão, e
		// tratar como "nunca sincronizou" é a leitura certa: a tela dirá que o
		// catálogo ainda vem.
		return Sincronizacao{}, nil
	}
	if err != nil {
		return Sincronizacao{}, fmt.Errorf("biblioteca: ler sincronização: %w", err)
	}
	if concluida > 0 {
		s.ConcluidaEm = time.Unix(concluida, 0)
	}
	if tentada > 0 {
		s.TentadaEm = time.Unix(tentada, 0)
	}
	return s, nil
}

// textoDeBusca junta o que a busca varre, em minúsculas.
//
// Montado na gravação e não na consulta: é uma vez por sincronização em vez de
// uma vez por linha por busca digitada.
func textoDeBusca(i Item) string {
	return strings.ToLower(i.Nome + " " + i.Titulo + " " + i.Descricao)
}

func booleano(b bool) int {
	if b {
		return 1
	}
	return 0
}
