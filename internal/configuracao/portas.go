package configuracao

import (
	"context"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// As portas deste arquivo são o que este pacote consome das outras features.
//
// Declaradas aqui, no consumidor, e implementadas em cmd/patchbay: o export
// atravessa upstream, endpoint, apikey e authsrv, e feature não importa feature.
// O que passa pela fronteira são os tipos deste pacote — os mesmos que viram
// YAML —, então nenhuma das quatro features precisa saber que existe um formato
// de arquivo.
//
// São interfaces de CRUD e não de um método só porque o assunto é um: criar,
// atualizar e remover o mesmo item. Fatiá-las em três não daria a nenhum
// consumidor uma dependência menor, e faria o Servico receber nove parâmetros.

// UpstreamNoBanco é um upstream lido do banco, com o id que só o banco conhece.
type UpstreamNoBanco struct {
	ID   int64
	Item Upstream
}

// EndpointNoBanco é um endpoint lido do banco.
type EndpointNoBanco struct {
	ID   int64
	Item Endpoint
}

// Upstreams é o que este pacote precisa da feature de upstream.
//
// Criar e Atualizar recebem o item já sem os segredos: credencial tem porta
// própria, porque ela é aplicada mesmo quando a configuração do upstream não
// muda — e não é aplicada quando a variável de ambiente não está definida.
type Upstreams interface {
	// Listar devolve os upstreams cadastrados, com os slots de credencial que
	// existem e sem nenhum valor de credencial.
	Listar(ctx context.Context) ([]UpstreamNoBanco, error)
	// Conferir roda a validação da própria feature sem escrever nada. É o que
	// faz o --dry-run recusar antes de aplicar o que o formulário recusaria.
	Conferir(ctx context.Context, u Upstream) error
	Criar(ctx context.Context, u Upstream) (int64, error)
	Atualizar(ctx context.Context, id int64, u Upstream) error
	Remover(ctx context.Context, id int64) error
}

// SegredosDeUpstream é a escrita de credencial estática, slot por slot.
//
// Porta separada de Upstreams porque o ciclo de vida é outro: o valor vem do
// ambiente do processo, não do arquivo, e "não vem" é um resultado normal que
// deixa o que está gravado no lugar.
type SegredosDeUpstream interface {
	Definir(ctx context.Context, upstreamID int64, tipo, nome string, valor cripto.Segredo) error
	Apagar(ctx context.Context, upstreamID int64, tipo, nome string) error
}

// Endpoints é o que este pacote precisa da feature de endpoint.
//
// A composição referencia upstream por nome, e é quem implementa a porta que
// traduz nome em id: o id é interno e não viaja no arquivo. Nome que não existe
// nem no banco nem no YAML é barrado antes, no plano.
type Endpoints interface {
	Listar(ctx context.Context) ([]EndpointNoBanco, error)
	Conferir(ctx context.Context, e Endpoint) error
	Criar(ctx context.Context, e Endpoint) (int64, error)
	Atualizar(ctx context.Context, id int64, e Endpoint) error
	Remover(ctx context.Context, id int64) error
}

// Chaves é a leitura das chaves de API para o export. Só leitura: o import não
// emite chave (ver Documento.ChavesAPI).
type Chaves interface {
	Listar(ctx context.Context) ([]ChaveAPI, error)
}

// Clientes é a leitura dos clientes OAuth do authorization server para o export.
type Clientes interface {
	Listar(ctx context.Context) ([]ClienteOAuth, error)
}

// Ambiente é de onde o import lê o valor de um segredo referenciado.
//
// Injetado e não os.LookupEnv direto para que o teste não dependa do ambiente da
// máquina, e para que um caminho futuro (um gerenciador de segredo, por exemplo)
// entre sem tocar no resto.
type Ambiente func(nome string) (string, bool)

// ehReferencia informa se o valor está na forma `${NOME}`.
func ehReferencia(valor string) bool {
	dentro, ok := strings.CutPrefix(valor, "${")
	if !ok {
		return false
	}
	dentro, ok = strings.CutSuffix(dentro, "}")
	return ok && dentro != "" && !strings.ContainsAny(dentro, "${} \t")
}

// nomeDaReferencia extrai o nome da variável de `${NOME}`.
func nomeDaReferencia(valor string) (string, bool) {
	if !ehReferencia(valor) {
		return "", false
	}
	return valor[2 : len(valor)-1], true
}

// referenciaCanonica é o nome de variável de ambiente que o export escreve para
// um slot de credencial.
//
// Previsível de propósito: quem exporta numa máquina e importa noutra precisa
// saber quais variáveis exportar sem abrir o arquivo item por item. O nome do
// upstream e o do header entram em caixa alta com o que não é identificador
// virando `_`, que é o alfabeto que um shell aceita sem escape.
func referenciaCanonica(upstream string, s Segredo) string {
	partes := []string{"PATCHBAY", "SEGREDO", identificador(upstream), identificador(s.Tipo)}
	if s.Nome != "" {
		partes = append(partes, identificador(s.Nome))
	}
	return "${" + strings.Join(partes, "_") + "}"
}

// identificador transforma texto livre no alfabeto de nome de variável de
// ambiente: letras em caixa alta, dígitos e `_`.
func identificador(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
