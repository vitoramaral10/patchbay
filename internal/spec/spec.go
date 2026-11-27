// Package spec acompanha a revisão mais nova da MCP publicada no site oficial.
//
// A tela de MCPs compara a revisão que cada upstream negociou com a mais nova;
// sem esta consulta, "mais nova" seria só a que a go-sdk do patchbay conhece, e
// uma spec nova passaria despercebida até alguém atualizar o patchbay.
package spec

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// SitemapOficial é o sitemap do site da MCP: cada revisão publicada tem as
// páginas em /specification/AAAA-MM-DD/.
const SitemapOficial = "https://modelcontextprotocol.io/sitemap.xml"

// IntervaloPadrao é de quanto em quanto tempo o sitemap é relido. Spec sai
// poucas vezes por ano; meio dia basta para avisar no mesmo dia.
const IntervaloPadrao = 12 * time.Hour

var reRevisao = regexp.MustCompile(`/specification/(\d{4}-\d{2}-\d{2})/`)

// limiteDoCorpo protege contra resposta gigante: o sitemap real tem poucos kB.
const limiteDoCorpo = 4 << 20

// Vigia guarda a última revisão publicada que conseguiu ler.
type Vigia struct {
	url string
	cli *http.Client
	log *slog.Logger

	mu        sync.RWMutex
	publicada string
	lidaEm    time.Time
}

// Nova cria a vigia do sitemap em url. url vazia desliga a consulta: a vigia
// responde "" para sempre e Rodar volta na hora.
func Nova(url string, log *slog.Logger) *Vigia {
	return &Vigia{url: url, cli: &http.Client{Timeout: 30 * time.Second}, log: log}
}

// Publicada devolve a revisão mais nova publicada e quando foi lida; "" se
// ainda não leu (ou a consulta está desligada).
func (v *Vigia) Publicada() (string, time.Time) {
	if v == nil {
		return "", time.Time{}
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.publicada, v.lidaEm
}

// Rodar lê agora e depois a cada intervalo, até ctx acabar. Falha só vai ao
// log: a tela continua com a última leitura boa, ou com a revisão da go-sdk.
func (v *Vigia) Rodar(ctx context.Context, intervalo time.Duration) {
	if v == nil || v.url == "" {
		return
	}
	t := time.NewTicker(intervalo)
	defer t.Stop()
	for {
		if err := v.Consultar(ctx); err != nil && ctx.Err() == nil {
			v.log.Warn("não consegui ler a revisão publicada da MCP", "url", v.url, "erro", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Consultar lê o sitemap uma vez e guarda a maior revisão encontrada.
func (v *Vigia) Consultar(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return err
	}
	res, err := v.cli.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("sitemap respondeu %d", res.StatusCode)
	}
	corpo, err := io.ReadAll(io.LimitReader(res.Body, limiteDoCorpo))
	if err != nil {
		return err
	}
	mais := ""
	for _, m := range reRevisao.FindAllStringSubmatch(string(corpo), -1) {
		// Data de verdade: um caminho estranho no sitemap não pode virar a
		// "revisão 9999-99-99" e acusar todo upstream de desatualizado.
		if _, err := time.Parse(time.DateOnly, m[1]); err == nil && m[1] > mais {
			mais = m[1]
		}
	}
	if mais == "" {
		return fmt.Errorf("nenhuma revisão no sitemap")
	}
	v.mu.Lock()
	v.publicada, v.lidaEm = mais, time.Now()
	v.mu.Unlock()
	return nil
}
