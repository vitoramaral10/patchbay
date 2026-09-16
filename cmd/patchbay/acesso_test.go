package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vitoramaral10/patchbay/internal/trilha"
)

// TestLogDeAcesso cobre o que faltava no diagnóstico do primeiro conector: um
// 404 e um redirecionamento precisam deixar linha no log.
func TestLogDeAcesso(t *testing.T) {
	t.Parallel()

	casos := map[string]struct {
		alvo        string
		status      int
		querNaLinha []string
	}{
		"404 aparece": {
			alvo:        "/authorize?client_id=pbc_teste",
			status:      http.StatusNotFound,
			querNaLinha: []string{"caminho=/authorize", "status=404", `query="client_id=pbc_teste"`},
		},
		"redirecionamento aparece": {
			alvo:        "/oauth/authorize",
			status:      http.StatusSeeOther,
			querNaLinha: []string{"caminho=/oauth/authorize", "status=303"},
		},
	}

	for nome, tc := range casos {
		t.Run(nome, func(t *testing.T) {
			t.Parallel()

			var saida bytes.Buffer
			log := slog.New(slog.NewTextHandler(&saida, &slog.HandlerOptions{Level: slog.LevelDebug}))
			h := registrarAcesso(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}), log)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.alvo, nil))

			linha := saida.String()
			if !strings.Contains(linha, "acesso http") {
				t.Fatalf("log = %q, quer a linha de acesso", linha)
			}
			for _, quer := range tc.querNaLinha {
				if !strings.Contains(linha, quer) {
					t.Errorf("log = %q, quer conter %q", linha, quer)
				}
			}
			if rec.Code != tc.status {
				t.Errorf("status repassado = %d, quer %d", rec.Code, tc.status)
			}
		})
	}
}

// TestLogDeAcessoRedigeAQuery: o authorize endpoint carrega code e state na
// query, e o log de acesso não pode virar a porta dos fundos da redação.
func TestLogDeAcessoRedigeAQuery(t *testing.T) {
	t.Parallel()

	var saida bytes.Buffer
	log := slog.New(slog.NewTextHandler(&saida, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := registrarAcesso(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), log)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/oauth/callback?code=segredo-do-codigo&state=segredo-do-state&client_id=pbc_teste", nil))

	linha := saida.String()
	for _, proibido := range []string{"segredo-do-codigo", "segredo-do-state"} {
		if strings.Contains(linha, proibido) {
			t.Errorf("log = %q, vazou %q", linha, proibido)
		}
	}
	if !strings.Contains(linha, trilha.Redigido) {
		t.Errorf("log = %q, quer a marca de redação", linha)
	}
	// O que não é sensível continua na linha: é ele que diz qual cliente era.
	if !strings.Contains(linha, "client_id=pbc_teste") {
		t.Errorf("log = %q, quer preservar o client_id", linha)
	}
}

// TestLogDeAcessoNaoEnvolveForaDeDebug garante que o caminho quente do
// transporte MCP recebe o http.ResponseWriter original quando o log está em
// info: nada de espião entre o SSE e a conexão.
func TestLogDeAcessoNaoEnvolveForaDeDebug(t *testing.T) {
	t.Parallel()

	var saida bytes.Buffer
	log := slog.New(slog.NewTextHandler(&saida, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var envolveu bool
	h := registrarAcesso(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, envolveu = w.(*espiaDeResposta)
	}), log)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/saude", nil))

	if envolveu {
		t.Error("handler recebeu o espião fora do nível debug")
	}
	if saida.Len() != 0 {
		t.Errorf("log = %q, quer vazio fora do nível debug", saida.String())
	}
}

// TestEspiaDeRespostaEscoa: o espião não pode cortar o flush, ou o SSE do
// transporte MCP para de escoar assim que alguém liga o log em debug.
func TestEspiaDeRespostaEscoa(t *testing.T) {
	t.Parallel()

	var saida bytes.Buffer
	log := slog.New(slog.NewTextHandler(&saida, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := registrarAcesso(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("parcial"))
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter do espião não é http.Flusher")
			return
		}
		f.Flush()
	}), log)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp/pessoal", nil))

	if !rec.Flushed {
		t.Error("flush não chegou ao ResponseWriter de baixo")
	}
	// Write sem WriteHeader é 200 implícito, e é isso que a linha tem de dizer.
	if !strings.Contains(saida.String(), "status=200") {
		t.Errorf("log = %q, quer status=200", saida.String())
	}
}
