package apikey

import (
	"context"
	"errors"
	"strings"
)

// Erros sentinela do CRUD.
var (
	// ErrNenhumEndpoint indica chave pedida sem escopo em endpoint nenhum.
	ErrNenhumEndpoint = errors.New("apikey: chave sem endpoint no escopo")
)

// Form é o formulário de criação de chave.
//
// Só criação: o escopo de uma chave não se edita. Trocar o escopo de uma chave já
// entregue mudaria em silêncio o que um cliente lá fora pode fazer; revogar e
// emitir outra deixa a mudança visível dos dois lados.
type Form struct {
	Nome        string
	EndpointIDs []int64
	Erros       map[string]string
}

// Validar preenche Erros e informa se o formulário passa.
func (f *Form) Validar() bool {
	f.Erros = map[string]string{}
	f.Nome = strings.TrimSpace(f.Nome)

	if f.Nome == "" {
		f.Erros["nome"] = "Dê um nome à chave: é como você vai saber qual cliente revogar."
	}
	if len(f.EndpointIDs) == 0 {
		f.Erros["endpoint"] = "Escolha ao menos um endpoint. Chave sem escopo não abre nada."
	}
	return len(f.Erros) == 0
}

// EndpointOpcao é um endpoint oferecido no escopo de uma chave.
type EndpointOpcao struct {
	ID        int64
	Slug      string
	Nome      string
	Escolhido bool
}

// Rotulo é como o endpoint aparece na lista de escopo.
func (o EndpointOpcao) Rotulo() string {
	if o.Nome != "" {
		return o.Nome
	}
	return o.Slug
}

// Endpoints é o que a tela de escopo precisa saber dos endpoints.
//
// Declarada aqui, no consumidor: quem é dono de endpoint é outra feature.
type Endpoints interface {
	Opcoes(ctx context.Context) ([]EndpointOpcao, error)
}

// Criada é o resultado de emitir uma chave.
//
// Claro só existe nesta tela: a chave é guardada como hash, e prometer reexibição
// obrigaria a guardar reversível — um cofre de credencial sem necessidade
// nenhuma (seção 08.8).
type Criada struct {
	Chave
	Claro string
	// Comandos é um "claude mcp add" pronto por endpoint do escopo.
	Comandos []Comando
}

// Comando é a linha pronta para registrar um endpoint num cliente MCP.
type Comando struct {
	Slug  string
	Linha string
}
