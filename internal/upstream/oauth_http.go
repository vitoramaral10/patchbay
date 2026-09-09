package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

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

	a.log.Info("autorização de upstream iniciada", "upstream", reg.Nome, "upstream_id", reg.ID)
	// Redirecionar e não renderizar um link: o clique já é a decisão, e uma tela
	// intermediária só acrescentaria um passo.
	webui.Redirecionar(w, r, destino)
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
