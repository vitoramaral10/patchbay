package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// Config é a configuração de bootstrap, lida uma única vez aqui e passada
// adiante como struct. Nada de os.Getenv espalhado pelo resto do código.
//
// São flags e variáveis de ambiente, sem biblioteca: koanf e viper resolvem
// camadas de configuração que a proposta 08.9 do estudo elimina — o YAML é
// artefato de export/import e nunca é lido no boot.
type Config struct {
	Listen     string
	DataDir    string
	PublicURL  string
	NivelLog   slog.Level
	LogEmTexto bool
}

// Padrões de desenvolvimento: só loopback, para que a instalação nova não fique
// exposta antes de existir autenticação de admin (fatia 2).
const (
	listenPadrao  = "127.0.0.1:8787"
	dataDirPadrao = "./dados"
)

// registrarFlags declara as flags de configuração num FlagSet que pode já ter
// as flags de um subcomando, e devolve o que resolve tudo depois do Parse.
func registrarFlags(fs *flag.FlagSet) (c *Config, resolver func() error) {
	c = &Config{}
	var nivel string

	fs.StringVar(&c.Listen, "listen", ambiente("PATCHBAY_LISTEN", listenPadrao),
		"endereço de escuta (PATCHBAY_LISTEN)")
	fs.StringVar(&c.DataDir, "data-dir", ambiente("PATCHBAY_DATA_DIR", dataDirPadrao),
		"diretório do banco SQLite (PATCHBAY_DATA_DIR)")
	fs.StringVar(&c.PublicURL, "public-url", ambiente("PATCHBAY_PUBLIC_URL", ""),
		"URL pública do patchbay, sem barra final (PATCHBAY_PUBLIC_URL)")
	fs.StringVar(&nivel, "log-level", ambiente("PATCHBAY_LOG_LEVEL", "info"),
		"nível de log: debug, info, warn, error (PATCHBAY_LOG_LEVEL)")
	fs.BoolVar(&c.LogEmTexto, "log-texto", ambiente("PATCHBAY_LOG_TEXTO", "") != "",
		"log em texto em vez de JSON (PATCHBAY_LOG_TEXTO)")

	return c, func() error {
		if err := c.NivelLog.UnmarshalText([]byte(nivel)); err != nil {
			return fmt.Errorf("config: nível de log %q: %w", nivel, err)
		}
		return c.normalizar()
	}
}

// lerConfig monta a configuração de um subcomando que só tem as flags comuns.
func lerConfig(comando string, args []string) (Config, error) {
	fs := flag.NewFlagSet("patchbay "+comando, flag.ContinueOnError)
	c, resolver := registrarFlags(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := resolver(); err != nil {
		return Config{}, err
	}
	return *c, nil
}

func (c *Config) normalizar() error {
	if c.Listen == "" {
		return errors.New("config: endereço de escuta vazio")
	}
	if c.DataDir == "" {
		return errors.New("config: diretório de dados vazio")
	}
	if c.PublicURL == "" {
		// Derivar da escuta é o certo para desenvolvimento e errado atrás de
		// proxy: o authorization server da fatia 10 anuncia o issuer com esta
		// URL, então lá ela passa a ser obrigatória.
		c.PublicURL = "http://" + c.Listen
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: URL pública inválida: %q", c.PublicURL)
	}
	return nil
}

func ambiente(chave, padrao string) string {
	if v := os.Getenv(chave); v != "" {
		return v
	}
	return padrao
}

// novoLogger monta o log estruturado. JSON por padrão porque o destino é um
// serviço 24×7; texto é conveniência de desenvolvimento.
func novoLogger(c Config, saida *os.File) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.NivelLog}
	if c.LogEmTexto {
		return slog.New(slog.NewTextHandler(saida, opts))
	}
	return slog.New(slog.NewJSONHandler(saida, opts))
}
