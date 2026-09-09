package trilha

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// intervaloBatida é a frequência do comentário de manutenção do SSE.
//
// Existe por causa dos intermediários: um proxy reverso que não vê byte nenhum
// por um minuto fecha a conexão, e o cliente reconecta em loop. Um comentário
// (":") é ignorado pelo EventSource e mantém a conexão viva.
const intervaloBatida = 25 * time.Second

// maximoLinhasAoVivo é quantas linhas a tela de log ao vivo mantém no DOM antes
// de o htmx começar a jogar as antigas fora. Sem teto, uma aba deixada aberta a
// noite inteira cresce até o navegador engasgar.
const maximoLinhasAoVivo = 300

// Admin é a borda HTTP da observabilidade: a trilha filtrável e o log ao vivo.
type Admin struct {
	repo *RepositorioSQLite
	reg  *Registrador
	hub  *Hub
	log  *slog.Logger
}

// NovoAdmin monta as telas de observabilidade.
//
// O Registrador entra porque o contador de descartes vive em memória, não no
// banco: ele conta o que *não* foi gravado, e por definição não há linha para
// consultar.
func NovoAdmin(repo *RepositorioSQLite, reg *Registrador, hub *Hub, log *slog.Logger) *Admin {
	return &Admin{repo: repo, reg: reg, hub: hub, log: log}
}

// Rotas registra as telas. Todas exigem sessão de admin.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaTrilha, a.trilha)
	mux.HandleFunc("GET "+webui.RotaLogsAoVivo, a.aoVivo)
	mux.HandleFunc("GET "+webui.RotaLogsFluxo, a.fluxo)
}

// trilha é a tela filtrável de chamadas.
func (a *Admin) trilha(w http.ResponseWriter, r *http.Request) {
	f := LerFiltro(r.URL.Query())

	eventos, temMais, err := a.repo.Listar(r.Context(), f)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	opcoes, err := a.repo.Opcoes(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	resumo, err := a.repo.Resumo(r.Context(), JanelaResumoPadrao)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	webui.Renderizar(w, r, http.StatusOK, a.log, TelaTrilha(Pagina{
		Eventos:  eventos,
		Filtro:   f,
		Opcoes:   opcoes,
		Resumo:   resumo,
		TemMais:  temMais,
		Descarte: a.descarte(),
	}))
}

// aoVivo é a tela do log ao vivo. Ela não busca nada: o conteúdo chega pelo SSE.
func (a *Admin) aoVivo(w http.ResponseWriter, r *http.Request) {
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaAoVivo(a.descarte(), maximoLinhasAoVivo))
}

// descarte junta os dois resíduos que a tela precisa mostrar: o da fila de
// gravação e o da fila de cada assinante de SSE.
func (a *Admin) descarte() Descarte {
	var d Descarte
	if a.reg != nil {
		d.Eventos = a.reg.Descartes()
		d.Gravados = a.reg.Gravados()
		d.FalhasGravacao = a.reg.FalhasGravacao()
	}
	if a.hub != nil {
		d.Mensagens = a.hub.Perdidas()
		d.Assinantes = a.hub.Assinantes()
	}
	return d
}

// fluxo é o stream de SSE.
//
// Escreve fragmento de HTML e não JSON: quem consome é a extensão sse do htmx,
// que troca o conteúdo do alvo pelo que vem em `data:`. Montar o HTML aqui, com
// templ, é o que garante o escape de tudo que veio de terceiro — um `data:` com
// JSON exigiria um render em JavaScript escrito à mão, que é justamente o que a
// decisão 08.10 recusa.
func (a *Admin) fluxo(w http.ResponseWriter, r *http.Request) {
	if a.hub == nil {
		http.Error(w, "log ao vivo indisponível", http.StatusServiceUnavailable)
		return
	}
	controle := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Desliga o buffer de proxies que entendem a dica; sem ela o stream chega em
	// blocos e o "ao vivo" atrasa minutos.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := controle.Flush(); err != nil {
		// Sem flush não existe SSE: o cliente ficaria esperando um corpo que o
		// servidor está segurando.
		a.log.Debug("stream de log sem flush", "erro", err)
		return
	}

	mensagens, cancelar := a.hub.Assinar()
	defer cancelar()

	batida := time.NewTicker(intervaloBatida)
	defer batida.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case <-batida.C:
			// Deadline antes de cada escrita, e não uma vez só na abertura:
			// uma conexão que o cliente largou sem fechar (rede caiu, proxy
			// travou) prenderia esta goroutine no Write até o SO desistir —
			// sem prazo próprio, é o mesmo problema que a batida existe para
			// evitar do outro lado da conexão.
			if err := controle.SetWriteDeadline(time.Now().Add(intervaloBatida)); err != nil {
				a.log.Debug("stream de log sem deadline de escrita", "erro", err)
				return
			}
			if _, err := io.WriteString(w, ": batida\n\n"); err != nil {
				return
			}
			if err := controle.Flush(); err != nil {
				return
			}

		case m, aberto := <-mensagens:
			if !aberto {
				// Hub encerrado: o processo está desligando.
				return
			}
			if err := controle.SetWriteDeadline(time.Now().Add(intervaloBatida)); err != nil {
				a.log.Debug("stream de log sem deadline de escrita", "erro", err)
				return
			}
			if err := a.escrever(ctx, w, m); err != nil {
				// Cliente foi embora ou o processo está fechando. Não é achado:
				// SSE termina assim na maioria das vezes.
				a.log.Debug("stream de log encerrado", "erro", err)
				return
			}
			if err := controle.Flush(); err != nil {
				return
			}
		}
	}
}

// escrever serializa uma mensagem no formato do SSE.
func (a *Admin) escrever(ctx context.Context, w io.Writer, m Mensagem) error {
	//nolint:contextcheck // o construtor de componente templ é fábrica pura; o
	// contexto entra no Render logo abaixo. É o mesmo falso positivo que o
	// .golangci.yml já documenta para webui.Renderizar.
	componente := componenteDe(m)
	if componente == nil {
		return nil
	}
	var buf bytes.Buffer
	if err := componente.Render(ctx, &buf); err != nil {
		return fmt.Errorf("trilha: renderizar linha de %s: %w", m.Tipo, err)
	}

	var saida strings.Builder
	saida.WriteString("event: ")
	saida.WriteString(string(m.Tipo))
	saida.WriteByte('\n')
	// Normaliza \r\n e \r solto para \n antes de partir em linhas: a
	// especificação de SSE aceita os três como fim de campo, então um valor de
	// terceiro com \r embutido (um nome de ferramenta, por exemplo) forjaria
	// uma linha "event:"/"data:" extra se chegasse cru ao cliente. Depois desta
	// normalização não sobra nenhum \r na saída, então toda linha nasce com o
	// prefixo "data: " abaixo — inclusive a que continha o \r.
	texto := strings.ReplaceAll(buf.String(), "\r\n", "\n")
	texto = strings.ReplaceAll(texto, "\r", "\n")
	// Uma linha `data:` por linha do HTML: o campo do SSE não aceita quebra, e o
	// cliente rejunta as linhas com \n.
	for linha := range strings.SplitSeq(texto, "\n") {
		saida.WriteString("data: ")
		saida.WriteString(linha)
		saida.WriteByte('\n')
	}
	saida.WriteByte('\n')

	if _, err := io.WriteString(w, saida.String()); err != nil {
		return fmt.Errorf("trilha: escrever evento de %s: %w", m.Tipo, err)
	}
	return nil
}

// componenteDe escolhe o fragmento de cada tipo de mensagem.
func componenteDe(m Mensagem) templ.Component {
	switch m.Tipo {
	case TipoLog:
		return LinhaDeLog(m.Log)
	case TipoChamada:
		return LinhaDeChamada(m.Chamada)
	default:
		return nil
	}
}
