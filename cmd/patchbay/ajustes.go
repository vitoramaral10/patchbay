package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
	"github.com/vitoramaral10/patchbay/internal/platform/webui"
)

// A URL pública editável em Ajustes.
//
// Ela entra no issuer do authorization server, no redirect do OAuth de MCP e
// na proteção de Origin, todos montados no boot. Em vez de fazer cada um ler um
// valor mutável, salvar reinicia a aplicação dentro do processo: o container
// não cai, as sessões MCP abertas reconectam.

const (
	chaveURLPublica = "url_publica"
	rotaURLPublica  = webui.RotaConfiguracao + "/url-publica"
)

// urlPublicaValida aceita só esquema e host (porta opcional), sem caminho.
func urlPublicaValida(bruta string) (string, bool) {
	limpa := strings.TrimRight(strings.TrimSpace(bruta), "/")
	u, err := url.Parse(limpa)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", false
	}
	return limpa, true
}

func (a *Aplicacao) salvarURLPublica(w http.ResponseWriter, r *http.Request) {
	if a.cfg.PublicURLExplicita {
		http.Error(w, "A URL pública vem de PATCHBAY_PUBLIC_URL e não muda por aqui.", http.StatusConflict)
		return
	}
	nova, ok := urlPublicaValida(r.PostFormValue("url_publica"))
	if !ok {
		webui.Redirecionar(w, r, webui.RotaConfiguracao+"?aviso=url_invalida")
		return
	}
	cfgs := store.NovasConfiguracoes(a.st.Leitura(), a.st.Escrita())
	if err := cfgs.Gravar(r.Context(), chaveURLPublica, nova); err != nil {
		webui.ErroInterno(w, r, a.log, err)
		return
	}
	a.log.Info("URL pública alterada pela tela", "url_publica", nova)
	// O navegador volta pela URL nova: a de antes pode nem ser a que ele usa.
	webui.Redirecionar(w, r, nova+webui.RotaConfiguracao+"?aviso=url_salva")
	a.reiniciarUma.Do(func() { close(a.reiniciar) })
}

// infoURLPublica é o que a tela de Ajustes mostra.
func (a *Aplicacao) infoURLPublica() (string, bool) {
	return a.cfg.PublicURL, a.cfg.PublicURLExplicita
}
