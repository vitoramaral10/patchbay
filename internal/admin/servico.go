package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DuracaoSessaoPadrao é a validade absoluta de uma sessão de admin.
//
// Absoluta e não deslizante: renovar a cada requisição seria uma escrita por
// requisição competindo pelo escritor único do SQLite, e a UI é usada por uma
// pessoa em rajadas curtas. Doze horas cobrem um dia de trabalho.
const DuracaoSessaoPadrao = 12 * time.Hour

const bytesTokenSessao = 32

// Repositorio é o que o Servico precisa da persistência, declarado aqui no
// consumidor.
type Repositorio interface {
	// Existe informa se o admin único já foi cadastrado.
	Existe(ctx context.Context) (bool, error)
	// Criar grava o admin único. Devolve ErrJaExiste se já houver um.
	Criar(ctx context.Context, usuario, senhaHash string, agora time.Time) (Admin, error)
	// PorUsuario devolve o admin e o hash da senha. ErrCredencial se não achar.
	PorUsuario(ctx context.Context, usuario string) (Admin, string, error)
	// CriarSessao grava uma sessão pelo hash do token.
	CriarSessao(ctx context.Context, hash string, adminID int64, criadaEm, expiraEm time.Time) error
	// SessaoPorHash devolve a sessão viva. ErrSessao se não existir ou vencer.
	SessaoPorHash(ctx context.Context, hash string, agora time.Time) (Sessao, error)
	// ApagarSessao encerra uma sessão.
	ApagarSessao(ctx context.Context, hash string) error
	// ApagarSessoesExpiradas limpa o que venceu.
	ApagarSessoesExpiradas(ctx context.Context, agora time.Time) (int64, error)
}

// Servico é a regra do admin: cadastro no primeiro acesso, autenticação e
// sessão.
type Servico struct {
	repo    Repositorio
	log     *slog.Logger
	params  Parametros
	duracao time.Duration
	agora   func() time.Time

	// O hash de mentira é derivado uma vez, sob Once: sem isso, dois logins
	// concorrentes escreveriam o campo ao mesmo tempo.
	umaVez   sync.Once
	hashFake string
}

// Opcao ajusta o serviço na construção.
type Opcao func(*Servico)

// ComParametros troca os custos do argon2id. É o que o teste usa para não pagar
// 64 MiB por caso.
func ComParametros(p Parametros) Opcao {
	return func(s *Servico) { s.params = p }
}

// ComDuracaoSessao troca a validade da sessão.
func ComDuracaoSessao(d time.Duration) Opcao {
	return func(s *Servico) { s.duracao = d }
}

// ComRelogio troca a fonte de tempo.
func ComRelogio(agora func() time.Time) Opcao {
	return func(s *Servico) { s.agora = agora }
}

// NovoServico monta o serviço de admin.
func NovoServico(repo Repositorio, log *slog.Logger, opcoes ...Opcao) *Servico {
	s := &Servico{
		repo:    repo,
		log:     log,
		params:  ParametrosPadrao,
		duracao: DuracaoSessaoPadrao,
		agora:   time.Now,
	}
	for _, o := range opcoes {
		o(s)
	}
	return s
}

// Existe informa se o setup do primeiro acesso já foi feito.
func (s *Servico) Existe(ctx context.Context) (bool, error) {
	existe, err := s.repo.Existe(ctx)
	if err != nil {
		return false, fmt.Errorf("admin: consultar existência: %w", err)
	}
	return existe, nil
}

// Criar cadastra o admin único. Só funciona no primeiro acesso.
func (s *Servico) Criar(ctx context.Context, usuario, senha string) (Admin, error) {
	usuario = strings.TrimSpace(usuario)
	if usuario == "" {
		return Admin{}, ErrUsuarioVazio
	}
	if len([]rune(senha)) < MinimoSenha {
		return Admin{}, fmt.Errorf("%w: mínimo de %d caracteres", ErrSenhaCurta, MinimoSenha)
	}

	hash, err := HashSenha(senha, s.params)
	if err != nil {
		return Admin{}, err
	}
	a, err := s.repo.Criar(ctx, usuario, hash, s.agora())
	if err != nil {
		return Admin{}, err
	}
	s.log.Info("administrador cadastrado no primeiro acesso", "usuario", a.Usuario)
	return a, nil
}

// Autenticar confere usuário e senha.
//
// Usuário inexistente e senha errada devolvem o mesmo erro e pagam o mesmo custo
// de argon2id: sem o hash de mentira, o tempo de resposta diria se o usuário
// existe.
func (s *Servico) Autenticar(ctx context.Context, usuario, senha string) (Admin, error) {
	a, hash, err := s.repo.PorUsuario(ctx, strings.TrimSpace(usuario))
	if errors.Is(err, ErrCredencial) {
		if _, verr := VerificarSenha(senha, s.hashDeMentira()); verr != nil {
			s.log.Debug("hash de mentira inválido", "erro", verr)
		}
		return Admin{}, ErrCredencial
	}
	if err != nil {
		return Admin{}, err
	}

	ok, err := VerificarSenha(senha, hash)
	if err != nil {
		// Hash malformado é banco corrompido, não senha errada: 500, não 401.
		return Admin{}, err
	}
	if !ok {
		return Admin{}, ErrCredencial
	}
	return a, nil
}

// hashDeMentira devolve um hash com os parâmetros em vigor, calculado uma vez.
//
// Não é segredo nenhum: existe só para que a verificação de um usuário
// inexistente custe o mesmo que a de um existente.
func (s *Servico) hashDeMentira() string {
	s.umaVez.Do(func() {
		h, err := HashSenha("senha-que-nunca-vai-existir", s.params)
		if err != nil {
			s.log.Error("não derivou o hash de mentira", "erro", err)
			return
		}
		s.hashFake = h
	})
	return s.hashFake
}

// AbrirSessao cria uma sessão e devolve o token em claro, que só existe aqui e
// no cookie: o banco guarda o hash.
func (s *Servico) AbrirSessao(ctx context.Context, adminID int64) (token string, expiraEm time.Time, err error) {
	bruto := make([]byte, bytesTokenSessao)
	if _, err := rand.Read(bruto); err != nil {
		return "", time.Time{}, fmt.Errorf("admin: sortear token de sessão: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(bruto)

	agora := s.agora()
	expiraEm = agora.Add(s.duracao)
	if err := s.repo.CriarSessao(ctx, HashToken(token), adminID, agora, expiraEm); err != nil {
		return "", time.Time{}, err
	}
	return token, expiraEm, nil
}

// Sessao resolve o token do cookie numa sessão viva.
func (s *Servico) Sessao(ctx context.Context, token string) (Sessao, error) {
	if token == "" {
		return Sessao{}, ErrSessao
	}
	return s.repo.SessaoPorHash(ctx, HashToken(token), s.agora())
}

// Encerrar apaga a sessão do token. Token desconhecido não é erro: o resultado
// de "sair" é sempre estar fora.
func (s *Servico) Encerrar(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.repo.ApagarSessao(ctx, HashToken(token))
}

// LimparSessoes apaga as sessões vencidas.
func (s *Servico) LimparSessoes(ctx context.Context) error {
	n, err := s.repo.ApagarSessoesExpiradas(ctx, s.agora())
	if err != nil {
		return err
	}
	if n > 0 {
		s.log.Debug("sessões de admin expiradas removidas", "removidas", n)
	}
	return nil
}

// HashToken é o hash de armazenamento de um token de sessão.
//
// SHA-256 e não argon2id, pelo mesmo motivo da chave de API: o token tem 256
// bits sorteados, não existe dicionário a encarecer.
func HashToken(token string) string {
	soma := sha256.Sum256([]byte(token))
	return hex.EncodeToString(soma[:])
}
