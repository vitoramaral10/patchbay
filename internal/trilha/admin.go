package trilha

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tamanhos de página. O padrão cabe numa tela sem rolagem infinita, e o teto
// existe porque ?por_pagina= vem da URL: sem teto, um número grande na query
// vira um scan da tabela inteira renderizado em HTML.
const (
	PorPaginaPadrao = 50
	PorPaginaMaximo = 200
)

// JanelaResumoPadrao é a janela dos contadores do painel da trilha.
const JanelaResumoPadrao = time.Hour

// Periodo é o recorte de tempo do filtro, na forma que a URL carrega.
type Periodo string

// Períodos oferecidos. Vazio é "tudo o que a retenção guardou".
const (
	PeriodoTudo    Periodo = ""
	Periodo15Min   Periodo = "15m"
	Periodo1Hora   Periodo = "1h"
	Periodo6Horas  Periodo = "6h"
	Periodo24Horas Periodo = "24h"
	Periodo7Dias   Periodo = "7d"
)

// periodos é a lista na ordem em que a tela oferece, com o rótulo e a duração.
var periodos = []struct {
	Valor   Periodo
	Rotulo  string
	Duracao time.Duration
}{
	{PeriodoTudo, "Tudo", 0},
	{Periodo15Min, "Últimos 15 min", 15 * time.Minute},
	{Periodo1Hora, "Última hora", time.Hour},
	{Periodo6Horas, "Últimas 6 h", 6 * time.Hour},
	{Periodo24Horas, "Últimas 24 h", 24 * time.Hour},
	{Periodo7Dias, "Últimos 7 dias", 7 * 24 * time.Hour},
}

// Duracao devolve a janela do período e se ele é conhecido. Período
// desconhecido cai em "tudo": a tela nunca fica vazia por causa de uma URL
// digitada errado.
func (p Periodo) Duracao() (time.Duration, bool) {
	for _, x := range periodos {
		if x.Valor == p {
			return x.Duracao, true
		}
	}
	return 0, false
}

// Rotulo é como o período aparece no seletor.
func (p Periodo) Rotulo() string {
	for _, x := range periodos {
		if x.Valor == p {
			return x.Rotulo
		}
	}
	return "Tudo"
}

// Periodos devolve as opções do seletor, em ordem.
func Periodos() []struct {
	Valor   Periodo
	Rotulo  string
	Duracao time.Duration
} {
	return periodos
}

// Filtro é o recorte que a tela de trilha aplica.
//
// Os cinco eixos que a seção 11 pede — endpoint, upstream, ferramenta, resultado
// e período — mais a paginação por cursor. Tudo vem da query string, e nada
// dele entra em SQL por interpolação.
type Filtro struct {
	Endpoint   string
	Upstream   string
	Ferramenta string
	Resultado  Resultado
	// Origem separa a chamada de cliente da sondagem funcional. Vazia é
	// "todas", e é o padrão de propósito: um filtro que esconde linhas sem
	// dizer é a trilha mentindo por omissão. Quem não quer ver a sonda a
	// escolhe no seletor; os contadores do painel, esses, já contam só cliente.
	Origem  Origem
	Periodo Periodo

	// Desde e Ate são o recorte absoluto. Normalizado deriva Desde do Periodo
	// quando ele não veio preenchido; o teste preenche direto para não depender
	// do relógio.
	Desde time.Time
	Ate   time.Time

	// CursorTS e CursorID são o par (ts, id) da última linha da página
	// anterior, juntos e não cada um sozinho: ts sozinho empata dentro do
	// mesmo milissegundo sob rajada, e id sozinho não segue o
	// ORDER BY ts DESC, id DESC que RepositorioSQLite.Listar usa. Zero é "sem
	// cursor": a primeira página.
	CursorTS int64
	CursorID int64

	PorPagina int
}

// TemCursor informa se o filtro carrega o cursor de uma página seguinte.
func (f Filtro) TemCursor() bool { return f.CursorID != 0 }

// Normalizado devolve o filtro com os limites aplicados: tamanho de página
// dentro do teto, resultado desconhecido descartado, Desde derivado do
// período e cursor negativo descartado.
func (f Filtro) Normalizado() Filtro {
	switch {
	case f.PorPagina <= 0:
		f.PorPagina = PorPaginaPadrao
	case f.PorPagina > PorPaginaMaximo:
		f.PorPagina = PorPaginaMaximo
	}
	if f.Resultado != "" && !f.Resultado.Valido() {
		f.Resultado = ""
	}
	if f.Origem != "" && !f.Origem.Valida() {
		f.Origem = ""
	}
	if _, conhecido := f.Periodo.Duracao(); !conhecido {
		f.Periodo = PeriodoTudo
	}
	if f.Desde.IsZero() {
		if d, _ := f.Periodo.Duracao(); d > 0 {
			f.Desde = time.Now().Add(-d)
		}
	}
	if f.CursorID < 0 {
		f.CursorID = 0
	}
	return f
}

// Vazio informa se nenhum eixo do filtro está preenchido — o que muda o texto do
// estado vazio de "nada casa com este filtro" para "a trilha está vazia".
func (f Filtro) Vazio() bool {
	return f.Endpoint == "" && f.Upstream == "" && f.Ferramenta == "" &&
		f.Resultado == "" && f.Origem == "" && f.Periodo == PeriodoTudo
}

// Query monta a query string do filtro com o cursor de uma linha — o link de
// "mais antigas" usa isso para pedir a página seguinte sem perder o recorte.
// cursorID zero omite o cursor, o que produz a URL da primeira página.
func (f Filtro) Query(cursorTS, cursorID int64) string {
	q := url.Values{}
	if f.Endpoint != "" {
		q.Set("endpoint", f.Endpoint)
	}
	if f.Upstream != "" {
		q.Set("upstream", f.Upstream)
	}
	if f.Ferramenta != "" {
		q.Set("ferramenta", f.Ferramenta)
	}
	if f.Resultado != "" {
		q.Set("resultado", string(f.Resultado))
	}
	if f.Origem != "" {
		q.Set("origem", string(f.Origem))
	}
	if f.Periodo != PeriodoTudo {
		q.Set("periodo", string(f.Periodo))
	}
	if cursorID != 0 {
		q.Set("cursor_ts", strconv.FormatInt(cursorTS, 10))
		q.Set("cursor_id", strconv.FormatInt(cursorID, 10))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// LerFiltro monta o filtro a partir da query string.
func LerFiltro(q url.Values) Filtro {
	cursorTS, _ := strconv.ParseInt(q.Get("cursor_ts"), 10, 64)
	cursorID, _ := strconv.ParseInt(q.Get("cursor_id"), 10, 64)
	return Filtro{
		Endpoint:   strings.TrimSpace(q.Get("endpoint")),
		Upstream:   strings.TrimSpace(q.Get("upstream")),
		Ferramenta: strings.TrimSpace(q.Get("ferramenta")),
		Resultado:  Resultado(strings.TrimSpace(q.Get("resultado"))),
		Origem:     Origem(strings.TrimSpace(q.Get("origem"))),
		Periodo:    Periodo(strings.TrimSpace(q.Get("periodo"))),
		CursorTS:   cursorTS,
		CursorID:   cursorID,
	}.Normalizado()
}

// Opcoes são os valores distintos que os seletores da tela oferecem.
type Opcoes struct {
	Endpoints   []string
	Upstreams   []string
	Ferramentas []string
}

// Resumo são os contadores de uma janela recente. Só de chamada de cliente: a
// sondagem funcional é o custo da observação, não tráfego (ver
// RepositorioSQLite.Resumo).
type Resumo struct {
	Janela        time.Duration
	Chamadas      int64
	Erros         int64
	Timeouts      int64
	PiorDuracaoMS int64
}

// PorMinuto é a taxa de chamadas na janela, com uma casa decimal.
//
// Derivada e não contada: um contador por minuto exigiria estado de janela
// deslizante em memória para dizer o mesmo que uma divisão diz.
func (r Resumo) PorMinuto() string {
	minutos := r.Janela.Minutes()
	if minutos <= 0 {
		return "0"
	}
	return strconv.FormatFloat(float64(r.Chamadas)/minutos, 'f', 1, 64)
}

// RotuloJanela descreve a janela dos contadores em texto.
func (r Resumo) RotuloJanela() string {
	switch {
	case r.Janela >= 24*time.Hour:
		return "nas últimas " + strconv.Itoa(int(r.Janela.Hours())) + " h"
	case r.Janela >= time.Hour:
		return "na última hora"
	default:
		return "nos últimos " + strconv.Itoa(int(r.Janela.Minutes())) + " min"
	}
}

// Pagina é o que a tela de trilha renderiza.
type Pagina struct {
	Eventos  []Evento
	Filtro   Filtro
	Opcoes   Opcoes
	Resumo   Resumo
	TemMais  bool
	Descarte Descarte
}

// Descarte é o resíduo assumido da fatia, e ele aparece na tela sempre — não só
// quando é maior que zero. Um contador que só existe quando há problema é um
// contador que ninguém aprende a ler.
type Descarte struct {
	// Eventos é quantas chamadas não entraram na trilha por fila cheia.
	Eventos uint64
	// Gravados é quantas entraram, para o número acima ter escala.
	Gravados uint64
	// FalhasGravacao é quantas chamadas saíram da fila e o banco recusou
	// gravar — separado de Eventos porque é outro problema: a fila não estava
	// cheia, foi o banco que recusou.
	FalhasGravacao uint64
	// Mensagens é quantas linhas do log ao vivo não couberam na fila de algum
	// assinante de SSE.
	Mensagens uint64
	// Assinantes é quantas telas de log ao vivo estão abertas agora.
	Assinantes int
}

// Houve informa se algo foi descartado ou falhou ao gravar desde o boot.
func (d Descarte) Houve() bool { return d.Eventos > 0 || d.Mensagens > 0 || d.FalhasGravacao > 0 }
