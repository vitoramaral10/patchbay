package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestURLPublicaValida(t *testing.T) {
	for bruta, quer := range map[string]string{
		"https://patchbay.exemplo.com/": "https://patchbay.exemplo.com",
		"http://192.168.0.10:8787":      "http://192.168.0.10:8787",
		"https://x.com/admin":           "",
		"ftp://x.com":                   "",
		"patchbay.exemplo.com":          "",
		"https://u:s@x.com":             "",
	} {
		got, ok := urlPublicaValida(bruta)
		if got != quer || ok != (quer != "") {
			t.Errorf("%q → %q %v, quer %q", bruta, got, ok, quer)
		}
	}
}

// Vários boots ao mesmo tempo no mesmo volume leem a mesma chave, inteira.
func TestChaveDoArquivo_BootsConcorrentes(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	chaves := make(chan string, n)
	erros := make(chan error, n)
	for range n {
		go func() {
			k, err := chaveDoArquivo(dir)
			chaves <- k
			erros <- err
		}()
	}
	var primeira string
	for range n {
		if err := <-erros; err != nil {
			t.Fatal(err)
		}
		k := <-chaves
		if primeira == "" {
			primeira = k
		}
		if k != primeira || len(k) != 44 {
			t.Fatalf("chave divergente ou truncada: %q vs %q", k, primeira)
		}
	}
	info, err := os.Stat(filepath.Join(dir, arquivoChaveMestra))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("arquivo: %v, perm %v", err, info.Mode().Perm())
	}
}
