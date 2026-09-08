// Package webui é a infraestrutura da UI de administração: os arquivos
// estáticos embutidos e os componentes de layout que toda tela reusa.
//
// É platform e não feature: não conhece upstream, endpoint nem chave de API.
// Cada feature traz os próprios templates e os embrulha em Pagina.
//
// htmx e a extensão de SSE vêm vendorizados com a versão no nome do arquivo, e
// não de CDN: o patchbay é entregue como binário único e uma UI que depende de
// rede externa para funcionar não é binário único.
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
)

// Prefixo é o caminho em que os arquivos estáticos são servidos.
const Prefixo = "/static/"

// Versões vendorizadas, fixadas: bump é edição de arquivo e de constante junto,
// nunca resolução em tempo de execução.
const (
	arquivoHTMX    = "vendor/htmx-2.0.7.min.js"
	arquivoHTMXSSE = "vendor/htmx-ext-sse-2.2.4.js"
	arquivoCSS     = "css/patchbay.css"
)

//go:embed estatico
var arquivos embed.FS

// raiz é o embed.FS com a pasta estatico/ como raiz.
var raiz = precisa(fs.Sub(arquivos, "estatico"))

// Impressao identifica o conjunto de estáticos desta build.
//
// Entra como query nas URLs de <link> e <script> para que o cache do navegador
// possa ser immutable: sem ela, ou o CSS novo não chega, ou todo carregamento
// paga uma revalidação.
var Impressao = impressaoDigital()

// URL devolve o caminho público de um arquivo estático, já com a impressão.
func URL(caminho string) string { return Prefixo + caminho + "?v=" + Impressao }

// URLCSS, URLHTMX e URLHTMXSSE são os três estáticos que o layout carrega.
func URLCSS() string     { return URL(arquivoCSS) }
func URLHTMX() string    { return URL(arquivoHTMX) }
func URLHTMXSSE() string { return URL(arquivoHTMXSSE) }

// Estaticos devolve o handler de Prefixo.
//
// Cache longo e immutable porque a URL carrega a impressão do conteúdo: build
// nova muda a URL, então nunca existe versão velha em cache sendo servida.
func Estaticos() http.Handler {
	servidor := http.FileServerFS(raiz)
	return http.StripPrefix(strings.TrimSuffix(Prefixo, "/"), http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			servidor.ServeHTTP(w, r)
		}))
}

// impressaoDigital resume o conteúdo de todos os estáticos num rótulo curto.
func impressaoDigital() string {
	soma := sha256.New()
	nomes, err := listar(raiz)
	if err != nil {
		// Falha aqui significa embed quebrado, que é erro de programação
		// detectado na primeira requisição; degradar para um rótulo fixo é
		// melhor que um panic na inicialização por causa de cache.
		return "dev"
	}
	for _, nome := range nomes {
		_, _ = io.WriteString(soma, nome)
		f, err := raiz.Open(nome)
		if err != nil {
			continue
		}
		_, _ = io.Copy(soma, f)
		_ = f.Close()
	}
	return base64.RawURLEncoding.EncodeToString(soma.Sum(nil))[:12]
}

func listar(sistema fs.FS) ([]string, error) {
	var nomes []string
	err := fs.WalkDir(sistema, ".", func(caminho string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			nomes = append(nomes, caminho)
		}
		return nil
	})
	slices.Sort(nomes)
	return nomes, err
}

func precisa[T any](v T, err error) T {
	if err != nil {
		// Só acontece com embed mal declarado: erro de programação, e falhar na
		// inicialização é o comportamento certo.
		panic(err)
	}
	return v
}
