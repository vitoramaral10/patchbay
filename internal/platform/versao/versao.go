// Package versao carrega a versão do patchbay para quem precisa se anunciar:
// o binário na linha de comando e o Implementation do MCP nos dois papéis.
package versao

// Numero, Commit e Data vêm em branco em build local (`go build`) e são
// sobrescritos por ldflags no release do goreleaser (.goreleaser.yaml), que
// injeta a tag, o hash curto do commit e a data do build em UTC. Nenhum dos
// três é lido de volta em outro lugar do código — servem só para quem olha o
// binário de fora (`patchbay versao`) saber exatamente o que está rodando.
var (
	Numero = "dev"
	Commit = "desconhecido"
	Data   = "desconhecida"
)

// String monta a linha completa que `patchbay versao` imprime.
func String() string {
	return Numero + " (commit " + Commit + ", build " + Data + ")"
}
