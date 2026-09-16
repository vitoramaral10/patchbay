package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// RotasPublicas registra o que o authorization server do upstream precisa
// alcançar de fora, sem sessão de admin.
//
// É só o Client ID Metadata Document. Ele não é segredo — é a declaração pública
// de quem o patchbay é como cliente OAuth — e exigir sessão aqui simplesmente
// impediria o provedor de lê-lo.
func (a *Admin) RotasPublicas(mux *http.ServeMux) {
	mux.HandleFunc("GET "+webui.RotaMetadataClienteUpstream, a.metadataCliente)
}

// metadataCliente serve o Client ID Metadata Document (SEP-991).
//
// No CIMD o client_id *é* esta URL, e o documento precisa repeti-lo dentro de si:
// é assim que o AS confirma que a URL que recebeu como client_id descreve mesmo
// este cliente, e não um documento hospedado em outro lugar. Por isso o campo sai
// junto do resto da metadata de registro em vez de a estrutura do SDK bastar.
func (a *Admin) metadataCliente(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.NotFound(w, r)
		return
	}
	u := a.oauth.URLMetadataCliente()
	if u == "" {
		// Sem HTTPS o CIMD não vale como identidade e o SDK nem o aceita.
		// Devolver 404 é honesto: o documento não existe nesta instalação.
		http.NotFound(w, r)
		return
	}

	doc := struct {
		ClientID string `json:"client_id"`
		*oauthex.ClientRegistrationMetadata
	}{
		ClientID:                   u,
		ClientRegistrationMetadata: a.oauth.metadataDeRegistro(),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		a.log.Error("falha ao servir metadata de cliente OAuth", "erro", err)
	}
}

// autorizar é o botão "Autorizar" da tela do upstream.
//
// Ele não monta a URL de autorização: quem a monta é o go-sdk, dentro do fluxo
// que a supervisão dispara, depois da descoberta RFC 9728/8414 e do registro de
// cliente. Este handler pede o consentimento, espera a URL aparecer e redireciona
// o navegador do admin para lá. É a ponte entre "o patchbay roda como serviço" e
// "o consentimento é dado pelo navegador do admin, que está em outra máquina".
func (a *Admin) autorizar(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	if a.oauth == nil || !reg.UsaOAuth() {
		a.avisar(w, r, reg.ID, "oauth_indisponivel")
		return
	}
	if !reg.Habilitado {
		a.avisar(w, r, reg.ID, "oauth_desabilitado")
		return
	}

	// O estado de OAuth, e não o cliente: aqui só interessa se este upstream usa
	// redirect de loopback, e EstadoOAuth responde isso sem decifrar nada.
	estado, err := a.repo.EstadoOAuth(r.Context(), reg.ID)
	if err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}

	destino, err := a.oauth.Pedir(r.Context(), reg.Config())
	if err != nil {
		// Sem detalhe do provedor na tela e sem token em log nenhum: o que o
		// admin precisa saber é que a autorização não começou e que clicar de
		// novo é o caminho.
		a.log.Warn("autorização de upstream não começou",
			"upstream", reg.Nome, "upstream_id", reg.ID, "erro", err)
		a.avisar(w, r, reg.ID, erroDeConsentimento(err))
		return
	}

	a.log.Info("autorização de upstream iniciada",
		"upstream", reg.Nome, "upstream_id", reg.ID,
		"loopback", estado.RedirectLoopback != "")

	if estado.RedirectLoopback != "" {
		// Provedor que só aceita loopback: o navegador vai ser devolvido a um
		// endereço da máquina de quem está autorizando, onde ninguém escuta. A
		// página falha, e o que interessa fica na barra de endereços. Esta tela
		// é o único lugar onde esse passo pode ser explicado antes de ele
		// acontecer — depois, o admin está olhando um erro de conexão.
		webui.Renderizar(w, r, http.StatusOK, a.log, TelaEntregaLoopback(DadosLoopback{
			UpstreamID:   reg.ID,
			UpstreamNome: reg.Nome,
			URLProvedor:  destino,
			Redirect:     estado.RedirectLoopback,
		}))
		return
	}

	// Redirecionar e não renderizar um link: o clique já é a decisão, e uma tela
	// intermediária só acrescentaria um passo.
	webui.Redirecionar(w, r, destino)
}

// DadosLoopback alimenta a tela de entrega manual do provedor loopback-only.
//
// URLProvedor é a URL de autorização já montada pelo SDK. Ela carrega o state e
// o code_challenge, não uma credencial: é a mesma URL para onde o navegador
// seria redirecionado no fluxo normal.
type DadosLoopback struct {
	UpstreamID   int64
	UpstreamNome string
	URLProvedor  string
	Redirect     string
	// Colado e Erro reexibem a tentativa recusada, para corrigir sem refazer o
	// consentimento no provedor.
	Colado string
	Erro   string
}

// colarRetornoOAuth recebe a URL que ficou na barra de endereços do admin
// depois de o provedor devolver o navegador ao loopback.
//
// O que ele extrai é o mesmo que o callback extrairia da query — state, code,
// iss, error — e entrega pelo mesmo Entregar. Daqui para baixo os dois caminhos
// são um só: a troca por token e a gravação cifrada acontecem na supervisão, com
// o redirect_uri de loopback que o provedor registrou.
//
// Aceita a URL inteira ou só a query: quem copia da barra traz tudo, quem copia
// da mensagem de erro de um navegador às vezes traz só um pedaço.
func (a *Admin) colarRetornoOAuth(w http.ResponseWriter, r *http.Request) {
	reg, ok := a.upstreamDaRota(w, r)
	if !ok {
		return
	}
	if a.oauth == nil || !reg.UsaOAuth() {
		a.avisar(w, r, reg.ID, "oauth_indisponivel")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, tamanhoMaximoDoCorpo)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "O texto colado é grande demais ou o formulário veio malformado.",
			http.StatusRequestEntityTooLarge)
		return
	}

	colado := strings.TrimSpace(r.PostFormValue("retorno"))
	recusar := func(motivo string) {
		a.log.Info("entrega manual de OAuth recusada",
			"upstream", reg.Nome, "upstream_id", reg.ID, "motivo", motivo)
		estado, err := a.repo.EstadoOAuth(r.Context(), reg.ID)
		if err != nil {
			webui.ErroInterno(w, r, a.log, err)
			return
		}
		webui.Renderizar(w, r, http.StatusUnprocessableEntity, a.log,
			TelaEntregaLoopback(DadosLoopback{
				UpstreamID:   reg.ID,
				UpstreamNome: reg.Nome,
				URLProvedor:  r.PostFormValue("provedor"),
				Redirect:     estado.RedirectLoopback,
				Colado:       colado,
				Erro:         motivo,
			}))
	}

	if colado == "" {
		recusar("Cole a URL que ficou na barra de endereços depois de autorizar.")
		return
	}
	q, err := queryDoRetorno(colado)
	if err != nil {
		recusar(err.Error())
		return
	}

	upstreamID, err := a.oauth.Entregar(
		q.Get("state"), q.Get("code"), q.Get("iss"), q.Get("error"))
	switch {
	case errors.Is(err, ErrConsentimentoDesconhecido):
		// O state não bate com tentativa nenhuma em curso: ou o prazo venceu, ou
		// esta URL já foi entregue, ou ela é de outro consentimento. Nos três
		// casos o caminho é recomeçar pelo botão Autorizar.
		recusar("Esta URL não corresponde a uma autorização em curso — ou ela já foi " +
			"usada, ou passou do tempo. Comece de novo pelo botão Autorizar.")
		return
	case err != nil:
		a.log.Warn("entrega manual de OAuth recusada pelo provedor",
			"upstream_id", upstreamID, "erro", err)
		a.avisar(w, r, reg.ID, erroDeConsentimento(err))
		return
	}

	a.log.Info("entrega manual de OAuth aceita", "upstream", reg.Nome, "upstream_id", reg.ID)
	a.avisar(w, r, reg.ID, "autorizado")
}

// queryDoRetorno tira os parâmetros do que o admin colou.
//
// Três formas, porque são as três que aparecem na prática: a URL inteira da
// barra de endereços, a query com o `?` na frente, e a query crua. O que não
// pode é adivinhar — texto sem `code` nem `error` é recusado com a instrução,
// em vez de virar um state vazio que o broker recusaria sem dizer por quê.
func queryDoRetorno(colado string) (url.Values, error) {
	bruta := colado
	if i := strings.IndexByte(bruta, '?'); i >= 0 {
		bruta = bruta[i+1:]
	}
	if i := strings.IndexByte(bruta, '#'); i >= 0 {
		bruta = bruta[:i]
	}
	q, err := url.ParseQuery(bruta)
	if err != nil {
		return nil, errors.New("Não consegui ler esta URL. Cole-a inteira, como ela " +
			"aparece na barra de endereços — de http://127.0.0.1 até o fim.")
	}
	if q.Get("code") == "" && q.Get("error") == "" {
		return nil, errors.New("A URL colada não traz nem code nem error. Confira se " +
			"você copiou a barra de endereços depois de autorizar no provedor, e não antes.")
	}
	return q, nil
}

// callback é onde o authorization server do upstream devolve o navegador do
// admin.
//
// Fica atrás da sessão de admin de propósito, junto com o resto de /admin: quem
// volta aqui é o navegador de quem clicou em "Autorizar", e o code que ele traz
// vale uma credencial de longa duração de um provedor de terceiro.
//
// A conferência de state é o registro de uso único do broker: um state que não
// está lá é ou uma tentativa já consumida, ou uma requisição que ninguém pediu, e
// nos dois casos a resposta é a mesma. O code_challenge do PKCE emparelhado com
// esse state vive dentro do handler do SDK, que é quem faz a troca.
func (a *Admin) callback(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()

	upstreamID, err := a.oauth.Entregar(
		q.Get("state"), q.Get("code"), q.Get("iss"), q.Get("error"))
	if err != nil && errors.Is(err, ErrConsentimentoDesconhecido) {
		// Sem upstream a que voltar: a lista é o destino honesto.
		a.log.Warn("callback de OAuth de upstream com state desconhecido",
			"caminho", r.URL.Path)
		webui.Redirecionar(w, r, webui.RotaUpstreams+"?aviso=consentimento_invalido")
		return
	}
	if err != nil {
		a.log.Warn("callback de OAuth de upstream recusado",
			"upstream_id", upstreamID, "erro", err)
		a.avisar(w, r, upstreamID, erroDeConsentimento(err))
		return
	}

	// O code foi entregue à tentativa que espera por ele; a troca por token e a
	// gravação cifrada acontecem na supervisão, não nesta requisição. A tela diz
	// isso: o estado do upstream se atualiza a cada recarga.
	a.avisar(w, r, upstreamID, "autorizado")
}

// avisar volta para a tela do upstream com um código de aviso.
func (a *Admin) avisar(w http.ResponseWriter, r *http.Request, upstreamID int64, codigo string) {
	destino := webui.RotaUpstreams
	if upstreamID != 0 {
		destino += "/" + strconv.FormatInt(upstreamID, 10)
	}
	if codigo != "" {
		destino += "?aviso=" + codigo
	}
	webui.Redirecionar(w, r, destino)
}
