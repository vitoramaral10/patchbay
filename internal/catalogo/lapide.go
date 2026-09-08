package catalogo

import (
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// JanelaDeGracaPadrao é quanto tempo uma ferramenta removida continua
// registrada como lápide.
//
// Cinco minutos porque a janela precisa cobrir o intervalo entre o
// tools/list_changed e a relistagem do cliente, e um cliente que guarda a lista
// em cache de prompt pode demorar uma conversa inteira para relistar. Mais que
// isso e a lápide vira ruído no catálogo; menos e ela não cobre o vai-e-vem de
// um upstream que oscila.
const JanelaDeGracaPadrao = 5 * time.Minute

// AvisoLapide marca a ferramenta que só existe para explicar que saiu.
const AvisoLapide Aviso = "lapide"

// Lapide monta a ferramenta que ocupa o lugar de uma que saiu do catálogo.
//
// Sem ela, o cliente que ainda não relistou recebe unknown tool
// (mcp/server.go:955-960) e conclui que o servidor está quebrado — quando a
// causa foi uma mudança de configuração ou um upstream degradado. O schema é o
// permissivo de propósito: a lápide precisa aceitar exatamente os argumentos que
// a ferramenta de verdade aceitava, para que a chamada chegue até ela e possa
// ser respondida com a explicação em vez de com um erro de validação.
func Lapide(nomeExposto, upstreamNome string) *mcp.Tool {
	return &mcp.Tool{
		Name:        nomeExposto,
		Description: TextoDeLapide(nomeExposto, upstreamNome),
		InputSchema: schemaPermissivo(),
	}
}

// TextoDeLapide é a mensagem que a lápide devolve como erro de ferramenta.
//
// Diz o que aconteceu e o que fazer, porque a mensagem chega a um modelo que
// vai decidir o próximo passo com ela: "ferramenta desconhecida" faz o cliente
// desistir do endpoint inteiro, e "relista e tenta de novo" faz ele se corrigir.
func TextoDeLapide(nomeExposto, upstreamNome string) string {
	origem := "do catálogo deste endpoint"
	if upstreamNome != "" {
		origem = fmt.Sprintf("do upstream %q", upstreamNome)
	}
	return fmt.Sprintf(
		"A ferramenta %q saiu %s e não atende mais. O endpoint continua no ar: relista as ferramentas (tools/list) e use o catálogo novo.",
		nomeExposto, origem)
}

// ResultadoDeLapide é o que a chamada de uma ferramenta com lápide devolve.
//
// Erro de ferramenta e não erro de protocolo: o cliente precisa saber que esta
// chamada não vai funcionar sem concluir que o endpoint está quebrado.
func ResultadoDeLapide(nomeExposto, upstreamNome string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{
			Text: TextoDeLapide(nomeExposto, upstreamNome),
		}},
	}
}
