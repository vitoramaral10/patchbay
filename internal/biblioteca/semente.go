package biblioteca

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// sementeEmbutida é o catálogo que viaja dentro do binário.
//
// Ela existe por um motivo só: instalação nova não nascer vazia. A primeira
// varredura leva de 20 a 35 minutos — 22 páginas de índice mais uma página de
// detalhe por servidor da lista oficial —, e durante esse tempo a tela dizia
// "o catálogo ainda está sendo baixado" e não servia para nada.
//
// **Não é o catálogo embutido que foi recusado em 2026-09-09.** Aquele era a
// única fonte, e envelhecia junto com o release. Este é semente: ele entra no
// banco no primeiro boot com a data em que foi gerado, e é justamente essa data
// que faz o sincronizador considerá-lo vencido e sair varrendo na hora. Meia
// hora depois de subir, o que está na tela veio da rede.
//
//go:embed semente.json.gz
var sementeEmbutida []byte

// tetoDaSemente corta a descompressão. A semente é nossa e conhecida, mas
// descomprimir sem limite um arquivo qualquer é como se enche a memória do
// processo — e o arquivo pode ser trocado por quem monta o binário.
const tetoDaSemente = 64 << 20

// arquivoDeSemente é a forma do que está gravado.
type arquivoDeSemente struct {
	// GeradoEm é quando a varredura que produziu isto terminou. É a idade que a
	// tela vai mostrar, e é o que faz o sincronizador varrer no primeiro boot em
	// vez de esperar doze horas.
	GeradoEm time.Time `json:"geradoEm"`
	Itens    []Item    `json:"itens"`
}

// Semente devolve o catálogo embutido e a data em que ele foi gerado.
//
// Semente vazia não é erro: o repositório pode ser construído sem ela, e nesse
// caso a instalação nova simplesmente espera a primeira varredura, como fazia
// antes.
func Semente() ([]Item, time.Time, error) { return lerSemente(sementeEmbutida) }

// lerSemente é o miolo, separado de Semente para o teste poder alimentá-lo sem
// depender do arquivo que está embutido neste build.
func lerSemente(bruta []byte) ([]Item, time.Time, error) {
	if len(bruta) == 0 {
		return nil, time.Time{}, nil
	}
	z, err := gzip.NewReader(bytes.NewReader(bruta))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("biblioteca: abrir semente: %w", err)
	}
	defer func() { _ = z.Close() }()

	bruto, err := io.ReadAll(io.LimitReader(z, tetoDaSemente))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("biblioteca: ler semente: %w", err)
	}
	var arq arquivoDeSemente
	if err := json.Unmarshal(bruto, &arq); err != nil {
		return nil, time.Time{}, fmt.Errorf("biblioteca: semente ilegível: %w", err)
	}
	return arq.Itens, arq.GeradoEm, nil
}

// GravarSemente escreve um catálogo no formato da semente.
//
// Usado pelo subcomando que a regenera. Fica aqui, e não no comando, para o
// formato ter um dono só: quem lê e quem escreve mudam juntos.
func GravarSemente(w io.Writer, itens []Item, geradoEm time.Time) error {
	z := gzip.NewWriter(w)
	if err := json.NewEncoder(z).Encode(arquivoDeSemente{GeradoEm: geradoEm, Itens: itens}); err != nil {
		return fmt.Errorf("biblioteca: escrever semente: %w", err)
	}
	if err := z.Close(); err != nil {
		return fmt.Errorf("biblioteca: fechar semente: %w", err)
	}
	return nil
}
