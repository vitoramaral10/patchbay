// Package versao carrega a versão do patchbay para quem precisa se anunciar:
// o binário na linha de comando e o Implementation do MCP nos dois papéis.
package versao

// Numero é a versão em vigor. O goreleaser passa a sobrescrevê-la por ldflags
// quando a fatia de empacotamento entrar (fatia 16).
var Numero = "0.1.0-fatia1"
