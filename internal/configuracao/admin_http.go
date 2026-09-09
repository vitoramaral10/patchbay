package configuracao

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// tamanhoMaximoDoYAML é o teto do corpo aceito no formulário de import.
//
// Existe porque o handler lê o corpo inteiro em memória antes de analisar. Um
// megabyte é muitas vezes a maior configuração que este gateway já teve, e o
// limite transforma um corpo absurdo em 413 em vez de em memória do processo.
const tamanhoMaximoDoYAML = 1 << 20

// Admin é a borda HTTP do export e do import.
//
// Três rotas e nenhuma delas escreve sem passar pelo plano: baixar, planejar e
// aplicar. Aplicar refaz o plano contra o banco de agora — entre ver a tela e
// clicar o botão pode ter passado um dia, e aplicar um plano velho seria aplicar
// uma decisão tomada sobre um estado que já não existe.
type Admin struct {
	servico *Servico
	log     *slog.Logger
}

// NovoAdmin monta a borda da tela de configuração.
func NovoAdmin(s *Servico, log *slog.Logger) *Admin {
	return &Admin{servico: s, log: log}
}

// Rotas registra a tela no mux protegido por sessão de admin.
func (a *Admin) Rotas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaConfiguracao, a.tela)
	mux.HandleFunc("GET "+RotaExportar, a.exportar)
	mux.HandleFunc("POST "+RotaPlano, a.planejar)
	mux.HandleFunc("POST "+RotaAplicar, a.aplicar)
}

func (a *Admin) tela(w http.ResponseWriter, r *http.Request) {
	aviso := webui.Avisos(r, avisosDaConfiguracao)
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaConfiguracao(DadosTela{Alerta: aviso}))
}

var avisosDaConfiguracao = map[string]webui.Alerta{
	"vazio": {
		Tom:    webui.TomAlerta,
		Titulo: "Cole o YAML antes de pedir o plano.",
		Texto:  "O campo veio em branco, e não há o que planejar.",
	},
}

// exportar entrega o YAML como arquivo.
//
// Content-Disposition com nome fixo: o arquivo vai para o repositório de
// configuração de quem administra, e um nome com data mudaria o caminho a cada
// download — o que estragaria justamente o diff que ele existe para produzir.
func (a *Admin) exportar(w http.ResponseWriter, r *http.Request) {
	dados, err := a.servico.Exportar(r.Context())
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="patchbay.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := w.Write(dados); err != nil {
		a.log.Error("falha ao enviar o yaml exportado", "erro", err)
	}
}

func (a *Admin) planejar(w http.ResponseWriter, r *http.Request) {
	texto, opcoes, ok := a.lerFormulario(w, r)
	if !ok {
		return
	}

	plano, err := a.servico.Planejar(r.Context(), []byte(texto), opcoes)
	if err != nil {
		a.log.Info("yaml recusado no import pela ui", "erro", err)
		tela := DadosTela{Texto: texto, RemoverAusentes: opcoes.RemoverAusentes, Erro: mensagemDeRecusa(err)}
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, a.log, TelaConfiguracao(tela))
		return
	}
	webui.Renderizar(w, r, http.StatusOK, a.log, TelaPlano(plano, texto))
}

func (a *Admin) aplicar(w http.ResponseWriter, r *http.Request) {
	texto, opcoes, ok := a.lerFormulario(w, r)
	if !ok {
		return
	}

	// O plano é refeito aqui de propósito: é ele que decide o que escrever, e
	// refazê-lo contra o banco de agora é o que impede um clique adiado de
	// aplicar uma decisão tomada sobre um estado que já mudou.
	plano, err := a.servico.Planejar(r.Context(), []byte(texto), opcoes)
	if err != nil {
		a.log.Info("yaml recusado no import pela ui", "erro", err)
		tela := DadosTela{Texto: texto, RemoverAusentes: opcoes.RemoverAusentes, Erro: mensagemDeRecusa(err)}
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, a.log, TelaConfiguracao(tela))
		return
	}

	relatorio, err := a.servico.Aplicar(r.Context(), plano)
	if err != nil && !errors.Is(err, ErrAplicacaoParcial) {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	a.log.Info("import de configuração aplicado pela ui",
		"aplicados", relatorio.Aplicados(), "falhas", relatorio.Falhas(),
		"nao_aplicados", len(relatorio.Ignorados))

	status := http.StatusOK
	if relatorio.Falhas() > 0 {
		status = http.StatusUnprocessableEntity
	}
	webui.Renderizar(w, r, status, a.log, TelaRelatorio(relatorio))
}

// lerFormulario devolve o YAML colado e as opções, ou responde à requisição.
func (a *Admin) lerFormulario(w http.ResponseWriter, r *http.Request) (string, Opcoes, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, tamanhoMaximoDoYAML)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "O arquivo colado é grande demais ou o formulário veio malformado.",
			http.StatusRequestEntityTooLarge)
		return "", Opcoes{}, false
	}
	texto := r.PostFormValue("yaml")
	if texto == "" {
		webui.Redirecionar(w, r, webui.RotaConfiguracao+"?aviso=vazio")
		return "", Opcoes{}, false
	}
	return texto, Opcoes{RemoverAusentes: r.PostFormValue("remover_ausentes") != ""}, true
}

// mensagemDeRecusa traduz o erro do analisador em texto de formulário.
//
// A mensagem do analisador vai para a tela porque ela descreve o arquivo que a
// pessoa acabou de colar — não é detalhe interno do servidor, é o motivo de a
// linha 12 não valer. Erro que não é do arquivo vira texto genérico, como em
// qualquer outra borda: a tela não descreve o interior do servidor.
func mensagemDeRecusa(err error) string {
	switch {
	case errors.Is(err, ErrVersaoDesconhecida):
		return err.Error()
	case errors.Is(err, ErrNomeRepetido):
		return err.Error()
	case errors.Is(err, ErrYAMLInvalido):
		return "Não foi possível ler o YAML: " + err.Error()
	default:
		// Erro de leitura do banco, e não do arquivo: a tela não descreve o
		// interior do servidor.
		return "Não foi possível montar o plano agora. Tente de novo em alguns instantes."
	}
}
