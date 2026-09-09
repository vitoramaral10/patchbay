package authsrv

import (
	"context"
	"testing"
	"time"

	"github.com/vitoramaral10/patchbay/internal/platform/store"
)

// TestTodosClientesNaoCarregaSegredoHash prova a fatia 11 (item 11 da revisão):
// a listagem não traz o hash do segredo de ninguém para a memória do processo,
// mesmo quando o cliente é confidencial. Só a leitura por id — a tela de
// detalhe e a autenticação — carrega a coluna inteira.
func TestTodosClientesNaoCarregaSegredoHash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.Abrir(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("abrir banco: erro = %v, quer nil", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("fechar banco: erro = %v, quer nil", err)
		}
	})

	var endpointID int64
	if err := st.Escrita().QueryRowContext(ctx,
		`INSERT INTO endpoint (slug, nome, descricao, criado_em) VALUES ('pessoal', 'Pessoal', '', 0)
		 RETURNING id`).Scan(&endpointID); err != nil {
		t.Fatalf("criar endpoint: erro = %v, quer nil", err)
	}

	r := NovoRepositorioSQLite(st.Leitura(), st.Escrita())
	criado, err := r.CriarCliente(ctx, FormCliente{
		Nome:         "serviço confidencial de teste",
		Confidencial: true,
		RedirectURIs: []string{"https://servico.test/cb"},
		EndpointIDs:  []int64{endpointID},
	}, "pbc_teste_segredo_hash", "pbcs_teste", Hash("segredo-de-teste"), time.Now())
	if err != nil {
		t.Fatalf("cadastrar cliente: erro = %v, quer nil", err)
	}

	// A leitura por id continua trazendo o hash: é ela que autentica.
	porID, err := r.ClientePorID(ctx, criado.ID)
	if err != nil {
		t.Fatalf("ClientePorID: erro = %v, quer nil", err)
	}
	if porID.segredoHash == "" {
		t.Fatal("ClientePorID veio sem segredo_hash: a autenticação deixaria de funcionar")
	}

	todos, err := r.TodosClientes(ctx)
	if err != nil {
		t.Fatalf("TodosClientes: erro = %v, quer nil", err)
	}
	var achou bool
	for _, c := range todos {
		if c.ID != criado.ID {
			continue
		}
		achou = true
		if c.segredoHash != "" {
			t.Error("TodosClientes veio com segredo_hash preenchido: a listagem não devia carregar essa coluna")
		}
		if c.SegredoPrefixo == "" {
			t.Error("TodosClientes sem segredo_prefixo: a tela precisa dele para dizer qual credencial é qual")
		}
	}
	if !achou {
		t.Fatal("cliente cadastrado não apareceu em TodosClientes")
	}
}
