package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/vitoramaral10/patchbay/internal/trilha"
)

// registrarAcesso envolve o handler HTTP com o log de acesso: método, caminho,
// status e duração de cada requisição.
//
// Existe porque sem ele um 404 e um redirecionamento não deixam rastro nenhum
// — nem em debug —, e todo diagnóstico de rota vira adivinhação com curl de
// fora. Foi exatamente o que custou o primeiro conector real em 2026-09-16: o
// cliente batia em caminhos que o patchbay não servia e a resposta não aparecia
// em lugar algum.
//
// Só em debug, e a decisão é medida: o nível de informação é baixo por
// requisição e o volume é alto — uma linha por requisição em info encheria o
// log de quem só quer ver o que o gateway fez. O custo de ligar é um
// PATCHBAY_LOG_LEVEL=debug.
//
// A query passa por trilha.RedigirQuery antes de entrar na linha: o caminho de
// /oauth/authorize carrega code e state, e o log de acesso não pode ser a porta
// dos fundos da redação que a trilha já faz.
func registrarAcesso(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A checagem antecipada evita tanto o custo do espião quanto o do
		// wrapper de ResponseWriter quando o nível não é debug — e deixa o
		// transporte MCP receber o w original no caminho quente.
		if !log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}

		inicio := time.Now()
		espiao := &espiaDeResposta{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(espiao, r)

		query, _ := trilha.RedigirQuery(r.URL.RawQuery)
		// Uma requisição de streaming só produz a linha quando o stream fecha:
		// a duração de um SSE do transporte MCP é a vida da sessão, não o tempo
		// de resposta.
		log.LogAttrs(r.Context(), slog.LevelDebug, "acesso http",
			slog.String("metodo", r.Method),
			slog.String("caminho", r.URL.Path),
			slog.String("query", query),
			slog.Int("status", espiao.status),
			slog.Int64("ms", time.Since(inicio).Milliseconds()),
		)
	})
}

// espiaDeResposta guarda o status escrito, que o http.ResponseWriter não
// devolve.
type espiaDeResposta struct {
	http.ResponseWriter
	status   int
	escreveu bool
}

func (e *espiaDeResposta) WriteHeader(status int) {
	if !e.escreveu {
		e.status = status
		e.escreveu = true
	}
	e.ResponseWriter.WriteHeader(status)
}

func (e *espiaDeResposta) Write(b []byte) (int, error) {
	e.escreveu = true
	return e.ResponseWriter.Write(b)
}

// Unwrap entrega o writer de baixo ao http.ResponseController, que é como
// Hijack, Flush e os deadlines atravessam o espião sem que ele precise
// implementar cada interface.
func (e *espiaDeResposta) Unwrap() http.ResponseWriter { return e.ResponseWriter }

// Flush existe além do Unwrap porque nem todo código faz o flush pelo
// ResponseController: quem ainda faz w.(http.Flusher) precisa achar o método
// aqui, ou o SSE do transporte MCP para de escoar assim que o log de acesso
// liga.
func (e *espiaDeResposta) Flush() {
	//nolint:errcheck // Flush sem suporte embaixo não é erro que o log trate
	_ = http.NewResponseController(e.ResponseWriter).Flush()
}
