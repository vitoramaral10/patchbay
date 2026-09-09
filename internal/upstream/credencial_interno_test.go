// Este arquivo prova, por dentro (package upstream), a defesa de clienteDe: um
// bearer estático gravado não pode sobrepor o Authorization que o OAuth monta,
// mesmo que a linha ainda exista no banco.
package upstream

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// TestClienteDe_FiltraBearerEmModoOAuth prova a defesa em profundidade: mesmo
// que LerCredenciais devolva um bearer gravado — uma linha que aplicarOAuth já
// devia ter apagado ao entrar no modo oauth, mas que pode sobreviver por uma
// gravação direta no banco ou por um esquema anterior a essa transação — o
// transporte de saída de um upstream em modo oauth nunca o usa.
func TestClienteDe_FiltraBearerEmModoOAuth(t *testing.T) {
	t.Parallel()

	cfg := Config{ID: 1, Nome: "oauth-com-bearer-antigo", Tipo: TipoHTTP, Timeout: time.Second, Modo: ModoOAuth}
	g := NovoGerente(slog.New(slog.DiscardHandler), nil,
		ComCredenciais(func(context.Context, int64) ([]Credencial, error) {
			return []Credencial{
				{Tipo: CredencialBearer, Valor: "sk-antigo-que-nao-devia-valer"},
				{Tipo: CredencialHeader, Nome: "X-Api-Key", Valor: "abc"},
			}, nil
		}),
	)

	cliente, err := g.clienteDe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("clienteDe: erro = %v, quer nil", err)
	}
	transporte, ok := cliente.Transport.(*transporteComCredenciais)
	if !ok {
		t.Fatalf("transporte = %T, quer *transporteComCredenciais (o header estático continua valendo)",
			cliente.Transport)
	}
	for _, c := range transporte.credenciais {
		if c.Tipo == CredencialBearer {
			t.Errorf("bearer presente no transporte em modo oauth: %+v", c)
		}
	}
}

// TestClienteDe_BearerContinuaEmModoEstatica é o controle: fora do modo oauth o
// bearer segue valendo, para não confundir "filtrado" com "bearer quebrado".
func TestClienteDe_BearerContinuaEmModoEstatica(t *testing.T) {
	t.Parallel()

	cfg := Config{ID: 2, Nome: "estatica-com-bearer", Tipo: TipoHTTP, Timeout: time.Second}
	g := NovoGerente(slog.New(slog.DiscardHandler), nil,
		ComCredenciais(func(context.Context, int64) ([]Credencial, error) {
			return []Credencial{{Tipo: CredencialBearer, Valor: "sk-valido"}}, nil
		}),
	)

	cliente, err := g.clienteDe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("clienteDe: erro = %v, quer nil", err)
	}
	transporte, ok := cliente.Transport.(*transporteComCredenciais)
	if !ok {
		t.Fatalf("transporte = %T, quer *transporteComCredenciais", cliente.Transport)
	}
	achou := false
	for _, c := range transporte.credenciais {
		if c.Tipo == CredencialBearer {
			achou = true
		}
	}
	if !achou {
		t.Error("bearer ausente do transporte em modo estática, quer presente")
	}
}
