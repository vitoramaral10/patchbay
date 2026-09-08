// Package admin é dono do administrador único da UI web: a senha em argon2id, a
// sessão por cookie e o portão que protege toda rota de administração.
//
// Multiusuário e RBAC são não-objetivos da v1 (seção 12 do estudo), e isso não é
// simplificação temporária: o "usuário" do authorization server da fatia 10 é
// este admin, e o consentimento de OAuth de cliente acontece atrás desta mesma
// sessão. O CHECK (id = 1) na tabela faz de "único" um invariante do banco.
package admin

import (
	"errors"
	"time"
)

// Erros sentinela do pacote.
var (
	// ErrSemAdmin indica que ninguém foi cadastrado ainda: a UI redireciona
	// para o setup do primeiro acesso.
	ErrSemAdmin = errors.New("admin: nenhum administrador cadastrado")
	// ErrJaExiste indica tentativa de rodar o setup depois do primeiro acesso.
	ErrJaExiste = errors.New("admin: administrador já cadastrado")
	// ErrCredencial indica usuário ou senha errados. É um erro só de propósito:
	// distinguir os dois entrega ao atacante qual metade acertou.
	ErrCredencial = errors.New("admin: usuário ou senha inválidos")
	// ErrSessao indica cookie ausente, desconhecido ou expirado.
	ErrSessao = errors.New("admin: sessão inválida ou expirada")
	// ErrSenhaCurta indica senha abaixo do mínimo.
	ErrSenhaCurta = errors.New("admin: senha curta")
	// ErrUsuarioVazio indica nome de usuário em branco.
	ErrUsuarioVazio = errors.New("admin: usuário vazio")
	// ErrHashInvalido indica hash de senha que não está no formato esperado —
	// banco corrompido ou editado à mão, não entrada de usuário.
	ErrHashInvalido = errors.New("admin: hash de senha em formato inválido")
)

// MinimoSenha é o piso de caracteres da senha do admin.
//
// Doze e não oito: a senha é a única credencial deste sistema escolhida por uma
// pessoa, ela abre o painel que guarda as credenciais de todos os upstreams, e
// não existe segundo fator nem bloqueio por tentativa na v1.
const MinimoSenha = 12

// Admin é o administrador único, sem a senha.
type Admin struct {
	ID       int64
	Usuario  string
	CriadoEm time.Time
}

// Sessao é uma sessão viva, como ela sai do banco.
type Sessao struct {
	Admin    Admin
	CriadaEm time.Time
	ExpiraEm time.Time
}
