package catalogo

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Limites e formas que o SDK exige, replicados aqui porque validateToolName
// (mcp/tool.go:163) só loga o erro e registra a ferramenta de todo jeito.
const (
	// MaxNomeFerramenta é o teto de bytes de um nome de ferramenta.
	MaxNomeFerramenta = 128
	// substituto é o rune usado no lugar de um caractere não permitido.
	substituto = '_'
	// anotacaoHeader é a chave de anotação que faz o AddTool entrar em panic
	// quando aplicada a tipo não primitivo ou duplicada
	// (mcp/streamable_headers.go:277).
	anotacaoHeader = "x-mcp-header"
)

// ErrDescartada indica ferramenta que não dá para normalizar. Quem chama loga e
// segue: uma ferramenta perdida é melhor que um endpoint derrubado.
var ErrDescartada = errors.New("catalogo: ferramenta descartada")

// schemaPermissivo é o input schema que substitui um schema inutilizável. É o
// mínimo que o AddTool aceita.
func schemaPermissivo() map[string]any { return map[string]any{"type": "object"} }

// Normalizar prepara uma ferramenta de upstream para o AddTool.
//
// As regras são as da seção 08.2 do estudo: nome validado e saneado, input
// schema substituído quando inutilizável, output schema descartado quando não
// serializa, anotação de header removida. O prefixo é aplicado ao nome
// registrado, nunca ao nome chamado no upstream.
func Normalizar(origemID int64, origemNome string, prefixo string, t *mcp.Tool) (Ferramenta, error) {
	if t == nil {
		return Ferramenta{}, fmt.Errorf("%w: ferramenta nula", ErrDescartada)
	}
	if t.Name == "" {
		// Sem nome original não há o que chamar no upstream: não existe
		// correção possível, só descarte.
		return Ferramenta{}, fmt.Errorf("%w: nome original vazio", ErrDescartada)
	}

	f := Ferramenta{
		UpstreamID:   origemID,
		UpstreamNome: origemNome,
		NomeOriginal: t.Name,
	}

	nome, saneado := sanearNome(prefixo + t.Name)
	if nome == "" {
		return Ferramenta{}, fmt.Errorf("%w: nome %q não sobrevive ao saneamento", ErrDescartada, t.Name)
	}
	if saneado {
		f.Avisos = append(f.Avisos, AvisoNomeSaneado)
	}

	// Cópia rasa: o snapshot do upstream não pode ser mutado, e o AddTool
	// documenta que o *Tool não pode mudar depois da chamada.
	copia := *t
	copia.Name = nome

	entrada, degradou, removeu := normalizarEntrada(t.InputSchema)
	copia.InputSchema = entrada
	if degradou {
		f.Avisos = append(f.Avisos, AvisoSchemaDegradado)
	}
	if removeu {
		f.Avisos = append(f.Avisos, AvisoAnotacaoRemovida)
	}

	saida, descartou := normalizarSaida(t.OutputSchema)
	copia.OutputSchema = saida
	if descartou {
		f.Avisos = append(f.Avisos, AvisoOutputDescartado)
	}

	f.Tool = &copia
	return f, nil
}

// sanearNome aplica as regras de validateToolName (mcp/tool.go:163): não vazio,
// no máximo 128 bytes, e só [a-zA-Z0-9_-.].
func sanearNome(nome string) (string, bool) {
	var b strings.Builder
	b.Grow(len(nome))
	mudou := false
	for _, r := range nome {
		if runeValido(r) {
			b.WriteRune(r)
			continue
		}
		mudou = true
		b.WriteRune(substituto)
	}
	saneado := b.String()

	// O teto do SDK é em bytes; cortar em fronteira de rune evita produzir
	// UTF-8 inválido — ainda que só ASCII sobreviva ao laço acima.
	if len(saneado) > MaxNomeFerramenta {
		saneado = saneado[:MaxNomeFerramenta]
		mudou = true
	}
	// Um nome só de substitutos não identifica nada: trata como não saneável.
	if strings.Trim(saneado, string(substituto)) == "" {
		return "", true
	}
	return saneado, mudou
}

func runeValido(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' || r == '-' || r == '.'
}

// normalizarEntrada devolve um input schema que o AddTool aceita.
//
// Cobre os cinco caminhos de panic do input (mcp/server.go:281-297): schema
// nulo, ponteiro tipado nulo, type diferente de "object", schema que não
// serializa, e mapa sem type "object". Devolve também se removeu anotação
// x-mcp-header, que é o sexto caminho (mcp/server.go:313).
func normalizarEntrada(bruto any) (schema any, degradou, removeuAnotacao bool) {
	if bruto == nil {
		return schemaPermissivo(), true, false
	}
	// Ponteiro tipado nulo: a interface não é nil, mas o AddTool entra em panic.
	if s, ok := bruto.(*jsonschema.Schema); ok {
		if s == nil {
			return schemaPermissivo(), true, false
		}
		if s.Type != "object" {
			return schemaPermissivo(), true, false
		}
		// Um *jsonschema.Schema válido não carrega anotação livre; passa direto.
		return s, false, false
	}

	var m map[string]any
	if err := remarshal(bruto, &m); err != nil {
		return schemaPermissivo(), true, false
	}
	if tipo, _ := m["type"].(string); tipo != "object" {
		return schemaPermissivo(), true, false
	}

	// A anotação x-mcp-header liga um parâmetro da ferramenta a um header HTTP
	// do transporte. Num gateway ela não tem sentido — o header seria o do
	// patchbay, não o do cliente — e uma anotação malformada é panic. Remover é
	// mais honesto que descartar a ferramenta inteira.
	removeuAnotacao = removerAnotacaoHeader(m)
	return m, false, removeuAnotacao
}

// normalizarSaida descarta o output schema que não serializa ou que é ponteiro
// tipado nulo (mcp/server.go:302-309). O output schema é opcional no protocolo,
// então descartar não quebra nada.
func normalizarSaida(bruto any) (schema any, descartou bool) {
	if bruto == nil {
		return nil, false
	}
	if s, ok := bruto.(*jsonschema.Schema); ok {
		if s == nil {
			return nil, true
		}
		return s, false
	}
	var qualquer any
	if err := remarshal(bruto, &qualquer); err != nil {
		return nil, true
	}
	return bruto, false
}

// removerAnotacaoHeader varre properties recursivamente e apaga a anotação.
func removerAnotacaoHeader(schema map[string]any) bool {
	removeu := false
	if _, existe := schema[anotacaoHeader]; existe {
		delete(schema, anotacaoHeader)
		removeu = true
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return removeu
	}
	for _, v := range props {
		sub, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if removerAnotacaoHeader(sub) {
			removeu = true
		}
	}
	return removeu
}

// remarshal repete o que o SDK faz para checar se um schema serializa
// (mcp/util.go): JSON de ida e volta.
func remarshal(de any, para any) error {
	bytes, err := json.Marshal(de)
	if err != nil {
		return fmt.Errorf("serializar: %w", err)
	}
	if err := json.Unmarshal(bytes, para); err != nil {
		return fmt.Errorf("desserializar: %w", err)
	}
	return nil
}

// Materializar normaliza todas as origens de um endpoint e resolve colisão de
// nome, devolvendo a lista pronta para registrar.
//
// A saída é determinística: a ordem das origens é a ordem da composição e as
// ferramentas de cada origem entram na ordem em que o upstream as listou. Isso
// é o que faz o nome exposto ser estável entre rematerializações — e o nome
// exposto é contrato, porque o cliente pode tê-lo em cache de prompt.
func Materializar(log *slog.Logger, origens []Origem) []Ferramenta {
	vistos := make(map[string]bool)
	var out []Ferramenta

	for _, o := range origens {
		for _, bruta := range o.Ferramentas {
			f, err := Normalizar(o.UpstreamID, o.Nome, o.Prefixo, bruta)
			if err != nil {
				log.Warn("ferramenta descartada na normalização",
					"upstream", o.Nome, "ferramenta", nomeSeguro(bruta), "erro", err)
				continue
			}
			if nome, colidiu := desambiguar(f.NomeExposto(), vistos); colidiu {
				if nome == "" {
					log.Warn("ferramenta descartada por colisão insolúvel de nome",
						"upstream", o.Nome, "ferramenta", f.NomeExposto())
					continue
				}
				log.Info("nome de ferramenta desambiguado",
					"upstream", o.Nome, "de", f.NomeExposto(), "para", nome)
				f.Tool.Name = nome
				f.Avisos = append(f.Avisos, AvisoColisaoDeNome)
			}
			vistos[f.NomeExposto()] = true
			out = append(out, f)
		}
	}
	return out
}

// desambiguar acha o primeiro sufixo livre para um nome já usado.
func desambiguar(nome string, vistos map[string]bool) (string, bool) {
	if !vistos[nome] {
		return nome, false
	}
	for i := 2; i < 1000; i++ {
		sufixo := fmt.Sprintf("_%d", i)
		base := nome
		if len(base)+len(sufixo) > MaxNomeFerramenta {
			base = base[:MaxNomeFerramenta-len(sufixo)]
		}
		if candidato := base + sufixo; !vistos[candidato] {
			return candidato, true
		}
	}
	return "", true
}

func nomeSeguro(t *mcp.Tool) string {
	if t == nil {
		return "<nula>"
	}
	return t.Name
}
