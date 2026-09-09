package authsrv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// TestDestinoPublico é a regra de IP do guarda de SSRF, exercitada sobre
// endereços fabricados.
//
// Table-driven sobre endereço e não sobre servidor de teste de propósito: é a
// única forma de cobrir 169.254.169.254 e as faixas privadas sem ter uma
// máquina com esses endereços — e é justamente esse alvo que a adoção de CIMD
// põe ao alcance de quem escolhe o client_id.
func TestDestinoPublico(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		ip       string
		querErro bool
	}{
		"unicast global v4":        {ip: "8.8.8.8"},
		"unicast global v6":        {ip: "2606:4700::1111"},
		"loopback v4":              {ip: "127.0.0.1", querErro: true},
		"loopback v4 fora do .0.1": {ip: "127.99.42.7", querErro: true},
		"loopback v6":              {ip: "::1", querErro: true},
		"metadata de nuvem":        {ip: "169.254.169.254", querErro: true},
		"link-local v6":            {ip: "fe80::1", querErro: true},
		"privada 10/8":             {ip: "10.0.0.1", querErro: true},
		"privada 172.16/12":        {ip: "172.16.5.4", querErro: true},
		"privada 192.168/16":       {ip: "192.168.1.1", querErro: true},
		"unique local v6":          {ip: "fd00::1", querErro: true},
		"cgnat":                    {ip: "100.64.0.1", querErro: true},
		"benchmark 198.18/15":      {ip: "198.19.0.1", querErro: true},
		"reservada 240/4":          {ip: "240.0.0.1", querErro: true},
		"não especificado":         {ip: "0.0.0.0", querErro: true},
		"multicast":                {ip: "224.0.0.1", querErro: true},
		"documentação v6":          {ip: "2001:db8::1", querErro: true},
		"nat64 atravessa para v4":  {ip: "64:ff9b::808:808", querErro: true},
		"nat64 de tradutor local":  {ip: "64:ff9b:1::a00:1", querErro: true},
		// ::ffff:127.0.0.1 é o loopback escrito como IPv6: sem o Unmap, ele
		// escaparia de IsLoopback e o guarda deixaria passar.
		"loopback mapeado em v6": {ip: "::ffff:127.0.0.1", querErro: true},
		"privada mapeada em v6":  {ip: "::ffff:10.0.0.1", querErro: true},
		// IPv4-compatible, 6to4 e Teredo carregam ou encapsulam um endereço
		// IPv4 dentro de um IPv6, sem passar pelo Unmap — a mesma classe de
		// risco do IPv4 mapeado, só que numa forma que o Unmap não desfaz.
		"ipv4-compatible carrega 127.0.0.1": {ip: "::7f00:1", querErro: true},
		"6to4 de endereço privado":          {ip: "2002:a00:1::", querErro: true},
		"teredo":                            {ip: "2001:0:0:0:0:0:a00:1", querErro: true},
		"rede 0/8":                          {ip: "0.1.2.3", querErro: true},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			ip, err := netip.ParseAddr(tc.ip)
			if err != nil {
				t.Fatalf("parsear %q: erro = %v, quer nil", tc.ip, err)
			}
			err = destinoPublico(ip)
			if tc.querErro {
				if !errors.Is(err, ErrDestinoBloqueado) {
					t.Errorf("destinoPublico(%s) = %v, quer ErrDestinoBloqueado", tc.ip, err)
				}
				return
			}
			if err != nil {
				t.Errorf("destinoPublico(%s) = %v, quer nil", tc.ip, err)
			}
		})
	}
}

func TestValidarURLCIMD(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		url      string
		querErro bool
	}{
		"https com caminho":       {url: "https://claude.ai/.well-known/oauth-client"},
		"http é recusado":         {url: "http://claude.ai/.well-known/oauth-client", querErro: true},
		"esquema file é recusado": {url: "file:///etc/passwd", querErro: true},
		"sem caminho é recusado":  {url: "https://claude.ai", querErro: true},
		"só a barra é recusado":   {url: "https://claude.ai/", querErro: true},
		"com fragmento":           {url: "https://claude.ai/doc#x", querErro: true},
		"com userinfo":            {url: "https://mau@claude.ai/doc", querErro: true},
		// A forma de um IP literal passa aqui: quem recusa faixa privada é o
		// buscador, em conferirLiteral — ver
		// TestBuscadorCIMD_GuardaDeDestinoLigado.
		"ip literal tem forma válida": {url: "https://10.0.0.1/doc"},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			err := ValidarURLCIMD(tc.url)
			if tc.querErro != (err != nil) {
				t.Errorf("ValidarURLCIMD(%q) = %v, quer erro = %v", tc.url, err, tc.querErro)
			}
		})
	}
}

// documentoValido é o corpo que um cliente legítimo publica.
func documentoValido(clientID string) DocumentoCIMD {
	return DocumentoCIMD{
		ClientID: clientID,
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{
			ClientName:              "Claude Code de teste",
			RedirectURIs:            []string{"http://localhost/callback", "http://127.0.0.1/callback"},
			TokenEndpointAuthMethod: "none",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
		},
	}
}

// servirCIMD sobe um servidor TLS em processo com o handler dado e devolve o
// buscador apontado para ele.
//
// Duas opções entram: o transporte, para o buscador confiar no certificado que o
// httptest emitiu, e o destino permitido, porque o httptest escuta em 127.0.0.1
// — exatamente o que destinoPublico bloqueia. O resto do guarda continua vivo:
// esquema, redirect, Content-Type e teto de corpo são exercitados de verdade.
func servirCIMD(t *testing.T, h http.HandlerFunc, extras ...OpcaoCIMD) (*BuscadorCIMD, string) {
	t.Helper()

	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)

	transporte, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("o cliente do httptest não usa *http.Transport")
	}
	opcoes := append([]OpcaoCIMD{
		ComTransporteCIMD(transporte),
		ComDestinoCIMD(func(netip.Addr) error { return nil }),
	}, extras...)
	buscador := NovoBuscadorCIMD(slog.New(slog.DiscardHandler), opcoes...)
	return buscador, ts.URL + "/.well-known/oauth-client"
}

func escreverJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("escrever documento: erro = %v, quer nil", err)
	}
}

// TestBuscadorCIMD_Buscar cobre o caminho feliz e cada recusa do guarda de
// SSRF que depende de uma resposta de verdade.
func TestBuscadorCIMD_Buscar(t *testing.T) {
	t.Parallel()

	t.Run("documento bom é aceito", func(t *testing.T) {
		t.Parallel()

		var alvo string
		sut, url := servirCIMD(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") != "application/json" {
				t.Errorf("Accept = %q, quer application/json", r.Header.Get("Accept"))
			}
			escreverJSON(t, w, documentoValido(alvo))
		})
		alvo = url

		doc, err := sut.Buscar(context.Background(), url)
		if err != nil {
			t.Fatalf("Buscar: erro = %v, quer nil", err)
		}
		switch {
		case doc.ClientID != url:
			t.Errorf("client_id = %q, quer %q", doc.ClientID, url)
		case len(doc.RedirectURIs) != 2:
			t.Errorf("redirect_uris = %v, quer 2 entradas", doc.RedirectURIs)
		case doc.Nome() != "Claude Code de teste":
			t.Errorf("Nome() = %q, quer o client_name", doc.Nome())
		}
	})

	t.Run("single-flight rende uma requisição de saída", func(t *testing.T) {
		t.Parallel()

		// Não é sobre concorrência: é sobre a busca não acontecer duas vezes
		// para o mesmo identificador dentro da mesma chamada. A prova de que o
		// cache evita a segunda ida está no teste ponta a ponta, que conta
		// visitas ao servidor de documento.
		var alvo string
		var visitas int
		sut, url := servirCIMD(t, func(w http.ResponseWriter, _ *http.Request) {
			visitas++
			escreverJSON(t, w, documentoValido(alvo))
		})
		alvo = url

		if _, err := sut.Buscar(context.Background(), url); err != nil {
			t.Fatalf("Buscar: erro = %v, quer nil", err)
		}
		if visitas != 1 {
			t.Errorf("visitas = %d, quer 1", visitas)
		}
	})

	// As recusas. Cada caso é um jeito conhecido de furar um guarda de SSRF, ou
	// um documento que não fecha com a URL de onde veio.
	recusas := map[string]struct {
		handler      http.HandlerFunc
		querEsteErro error
	}{
		"redirecionamento não é seguido": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				// O destino é interno de propósito: seguir o 302 é como a URL
				// validada deixa de ser a URL buscada.
				http.Redirect(w, r, "https://169.254.169.254/latest/meta-data", http.StatusFound)
			},
			// Sentinela específica, e não só ErrDocumentoCIMD: é o que prova
			// que a recusa veio mesmo do CheckRedirect, e não de alguma outra
			// falha que por acaso também usasse o mesmo guarda-chuva.
			querEsteErro: ErrRedirectRecusado,
		},
		"corpo maior que o teto": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"client_id":"`))
				_, _ = w.Write([]byte(strings.Repeat("a", TamanhoMaximoCIMD+1)))
				_, _ = w.Write([]byte(`"}`))
			},
			querEsteErro: ErrCorpoExcedido,
		},
		"content-type de html": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(`{"client_id":"x"}`))
			},
			querEsteErro: ErrDocumentoCIMD,
		},
		"sem content-type": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header()["Content-Type"] = nil
				_, _ = w.Write([]byte(`{}`))
			},
			querEsteErro: ErrDocumentoCIMD,
		},
		"status 404": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.NotFound(w, nil)
			},
			querEsteErro: ErrDocumentoCIMD,
		},
		"corpo que não é json": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`<html>`))
			},
			querEsteErro: ErrDocumentoCIMD,
		},
	}

	for nome, tc := range recusas {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			sut, url := servirCIMD(t, tc.handler)
			_, err := sut.Buscar(context.Background(), url)
			if !errors.Is(err, tc.querEsteErro) {
				t.Errorf("Buscar = %v, quer %v", err, tc.querEsteErro)
			}
		})
	}
}

// TestBuscadorCIMD_CacheNegativo prova que uma falha de busca fica registrada
// por identificador: uma segunda autorização dentro da janela não bate no
// documento de novo, e uma depois da janela bate.
func TestBuscadorCIMD_CacheNegativo(t *testing.T) {
	t.Parallel()

	agora := time.Now()
	relogio := func() time.Time { return agora }

	var visitas int
	sut, url := servirCIMD(t, func(w http.ResponseWriter, _ *http.Request) {
		visitas++
		http.NotFound(w, nil)
	}, ComRelogioCIMD(relogio))

	if _, err := sut.Buscar(context.Background(), url); !errors.Is(err, ErrDocumentoCIMD) {
		t.Fatalf("primeira busca: erro = %v, quer ErrDocumentoCIMD", err)
	}
	if _, err := sut.Buscar(context.Background(), url); !errors.Is(err, ErrDocumentoCIMD) {
		t.Fatalf("segunda busca dentro da janela: erro = %v, quer ErrDocumentoCIMD", err)
	}
	if visitas != 1 {
		t.Errorf("visitas ao documento = %d, quer 1: a segunda busca devia ter vindo do cache negativo", visitas)
	}

	agora = agora.Add(janelaCacheNegativoCIMD + time.Second)
	if _, err := sut.Buscar(context.Background(), url); !errors.Is(err, ErrDocumentoCIMD) {
		t.Fatalf("busca depois da janela: erro = %v, quer ErrDocumentoCIMD", err)
	}
	if visitas != 2 {
		t.Errorf("visitas ao documento = %d, quer 2: a janela vencida devia ter rebuscado", visitas)
	}
}

// TestBuscadorCIMD_GuardaDeDestinoLigado prova que a regra de produção recusa o
// próprio httptest.
//
// É o teste que fecha o resto do arquivo: todos os outros casos rodam com
// ComDestinoCIMD relaxado, e sem este eu não saberia se o guarda de verdade está
// ligado por padrão.
func TestBuscadorCIMD_GuardaDeDestinoLigado(t *testing.T) {
	t.Parallel()

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		escreverJSON(t, w, documentoValido("irrelevante"))
	}))
	t.Cleanup(ts.Close)

	transporte, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("o cliente do httptest não usa *http.Transport")
	}
	// Sem ComDestinoCIMD: vale destinoPublico, e o httptest escuta em 127.0.0.1.
	sut := NovoBuscadorCIMD(slog.New(slog.DiscardHandler), ComTransporteCIMD(transporte))

	_, err := sut.Buscar(context.Background(), ts.URL+"/.well-known/oauth-client")
	if !errors.Is(err, ErrDestinoBloqueado) {
		t.Fatalf("Buscar em loopback = %v, quer ErrDestinoBloqueado", err)
	}
}

func TestDocumentoCIMD_Validar(t *testing.T) {
	t.Parallel()

	const url = "https://claude.ai/.well-known/oauth-client"

	casos := map[string]struct {
		muda     func(*DocumentoCIMD)
		querErro bool
	}{
		"documento bom": {muda: func(*DocumentoCIMD) {}},
		"client_id de outro cliente": {
			muda:     func(d *DocumentoCIMD) { d.ClientID = "https://outro.test/doc" },
			querErro: true,
		},
		"client_id com barra final a mais": {
			muda:     func(d *DocumentoCIMD) { d.ClientID = url + "/" },
			querErro: true,
		},
		"sem redirect_uris": {
			muda:     func(d *DocumentoCIMD) { d.RedirectURIs = nil },
			querErro: true,
		},
		"redirect_uri em http fora de loopback": {
			muda:     func(d *DocumentoCIMD) { d.RedirectURIs = []string{"http://mau.test/callback"} },
			querErro: true,
		},
		"redirect_uris demais": {
			muda: func(d *DocumentoCIMD) {
				d.RedirectURIs = make([]string, MaximoRedirectsPorCliente+1)
				for i := range d.RedirectURIs {
					d.RedirectURIs[i] = "https://claude.ai/cb"
				}
			},
			querErro: true,
		},
		"grant que o AS não implementa": {
			muda:     func(d *DocumentoCIMD) { d.GrantTypes = []string{"client_credentials"} },
			querErro: true,
		},
		"response_type token": {
			muda:     func(d *DocumentoCIMD) { d.ResponseTypes = []string{"token"} },
			querErro: true,
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			doc := documentoValido(url)
			tc.muda(&doc)
			err := doc.Validar(url)
			if tc.querErro != (err != nil) {
				t.Errorf("Validar = %v, quer erro = %v", err, tc.querErro)
			}
		})
	}
}

func TestEhIdentificadorCIMD(t *testing.T) {
	t.Parallel()

	casos := map[string]bool{
		"https://claude.ai/.well-known/oauth-client": true,
		"pbc_abcdefghij":       false,
		"http://claude.ai/doc": false,
		"":                     false,
	}
	for entrada, quer := range casos {
		t.Run(entrada, func(t *testing.T) {
			t.Parallel()

			if got := ehIdentificadorCIMD(entrada); got != quer {
				t.Errorf("ehIdentificadorCIMD(%q) = %v, quer %v", entrada, got, quer)
			}
		})
	}
}

func TestRecortar(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		texto  string
		limite int
		quer   string
	}{
		"cabe inteiro":             {texto: "curto", limite: 10, quer: "curto"},
		"corta no limite":          {texto: "abcdefghij", limite: 5, quer: "abcde"},
		"não corta rune pelo meio": {texto: "açúcar", limite: 2, quer: "a"},
		"limite zero":              {texto: "abc", limite: 0, quer: ""},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			if got := recortar(tc.texto, tc.limite); got != tc.quer {
				t.Errorf("recortar(%q, %d) = %q, quer %q", tc.texto, tc.limite, got, tc.quer)
			}
		})
	}
}
