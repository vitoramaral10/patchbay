package configuracao

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
)

// cabecalho é o comentário que abre todo arquivo exportado.
//
// Comentário e não campo do documento: ele é para quem abre o arquivo num editor,
// e o analisador o descarta — o que mantém o ida-e-volta byte a byte, porque o
// export seguinte o escreve igual.
const cabecalho = `# patchbay — configuração exportada.
#
# Este arquivo é artefato de export/import e nunca é lido no boot: aplicá-lo é
# uma operação explícita (patchbay import), e nada aqui entra em efeito sozinho.
#
# Segredo não sai daqui. Cada credencial aparece como um slot com o nome da
# variável de ambiente de onde o import a lê; a variável não definida deixa o
# que está gravado no lugar. Para apagar uma credencial, escreva limpar: true.
#
# revisao é a trava otimista. O import compara a revisão de cada item com o
# estado do banco: item que só este arquivo mudou é aplicado, item que só o
# banco mudou fica como está, e item que os dois mudaram é reportado como
# conflito. Não edite as linhas de revisao — é a ausência de edição nelas que
# permite distinguir os três casos.

`

// Servico é o export e o import da configuração.
//
// As portas entram por construtor, e nenhuma delas é uma feature: só cmd/patchbay
// conhece as duas pontas.
type Servico struct {
	upstreams Upstreams
	segredos  SegredosDeUpstream
	endpoints Endpoints
	chaves    Chaves
	clientes  Clientes
	ambiente  Ambiente
	log       *slog.Logger
}

// Opcao configura o serviço.
type Opcao func(*Servico)

// ComChaves liga o export à lista de chaves de API. Sem ela a seção sai vazia.
func ComChaves(c Chaves) Opcao { return func(s *Servico) { s.chaves = c } }

// ComClientes liga o export à lista de clientes OAuth.
func ComClientes(c Clientes) Opcao { return func(s *Servico) { s.clientes = c } }

// ComAmbiente troca de onde o import lê o valor de um segredo referenciado. O
// padrão é o ambiente do processo.
func ComAmbiente(a Ambiente) Opcao { return func(s *Servico) { s.ambiente = a } }

// NovoServico monta o serviço com as duas portas que ele não sabe viver sem.
//
// Upstream e endpoint são obrigatórios porque são as duas seções que o import
// aplica; chave de API e cliente OAuth são registro no arquivo, e a ausência
// deles só significa uma seção a menos.
func NovoServico(
	ups Upstreams, segredos SegredosDeUpstream, ends Endpoints, log *slog.Logger, opcoes ...Opcao,
) *Servico {
	s := &Servico{
		upstreams: ups,
		segredos:  segredos,
		endpoints: ends,
		ambiente:  func(string) (string, bool) { return "", false },
		log:       log,
	}
	for _, o := range opcoes {
		o(s)
	}
	return s
}

// Exportar devolve o YAML do estado atual do banco.
//
// Todo item sai na forma normalizada, e não como a linha do banco está: é a
// forma normalizada que o import compara, e exportar qualquer outra faria o
// primeiro import depois de um export mostrar diferença que ninguém escreveu.
func (s *Servico) Exportar(ctx context.Context) ([]byte, error) {
	doc, err := s.estadoDoBanco(ctx)
	if err != nil {
		return nil, err
	}

	if s.chaves != nil {
		if doc.ChavesAPI, err = s.chaves.Listar(ctx); err != nil {
			return nil, fmt.Errorf("configuracao: exportar chaves de api: %w", err)
		}
		slices.SortFunc(doc.ChavesAPI, func(a, b ChaveAPI) int {
			return strings.Compare(a.PrefixoVisivel, b.PrefixoVisivel)
		})
	}
	if s.clientes != nil {
		if doc.ClientesOAuth, err = s.clientes.Listar(ctx); err != nil {
			return nil, fmt.Errorf("configuracao: exportar clientes oauth: %w", err)
		}
		slices.SortFunc(doc.ClientesOAuth, func(a, b ClienteOAuth) int {
			return strings.Compare(a.ClientID, b.ClientID)
		})
	}

	var buf bytes.Buffer
	buf.WriteString(cabecalho)
	enc := yaml.NewEncoder(&buf)
	// Dois espaços, que é o recuo que o yaml.v3 não usa por padrão em lista
	// aninhada e que é o que torna a composição de um endpoint legível.
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("configuracao: serializar yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("configuracao: fechar yaml: %w", err)
	}
	return buf.Bytes(), nil
}

// estadoDoBanco monta o documento das duas seções aplicáveis, já com as revisões
// por item e a do documento. É a base do export e o lado "banco" da mescla.
func (s *Servico) estadoDoBanco(ctx context.Context) (Documento, error) {
	ups, err := s.upstreams.Listar(ctx)
	if err != nil {
		return Documento{}, fmt.Errorf("configuracao: listar upstreams: %w", err)
	}
	ends, err := s.endpoints.Listar(ctx)
	if err != nil {
		return Documento{}, fmt.Errorf("configuracao: listar endpoints: %w", err)
	}

	doc := Documento{
		Versao:    VersaoAtual,
		Upstreams: make([]Upstream, 0, len(ups)),
		Endpoints: make([]Endpoint, 0, len(ends)),
	}
	for _, u := range ups {
		item := u.Item.normalizado()
		item.Revisao = item.resumo()
		item.Segredos = comReferenciaCanonica(item.Nome, item.Segredos)
		doc.Upstreams = append(doc.Upstreams, item)
	}
	for _, e := range ends {
		item := e.Item.normalizado()
		item.Revisao = item.resumo()
		doc.Endpoints = append(doc.Endpoints, item)
	}
	slices.SortFunc(doc.Upstreams, func(a, b Upstream) int { return strings.Compare(a.Nome, b.Nome) })
	slices.SortFunc(doc.Endpoints, func(a, b Endpoint) int { return strings.Compare(a.Slug, b.Slug) })
	doc.Revisao = resumoDoDocumento(doc.Upstreams, doc.Endpoints)
	return doc, nil
}

// comReferenciaCanonica preenche o `valor` de cada slot com a variável de
// ambiente correspondente. O slot que já veio do banco nunca tem valor: o valor
// é a instrução de onde lê-lo, e ela é escrita pelo export.
func comReferenciaCanonica(upstream string, segredos []Segredo) []Segredo {
	out := make([]Segredo, 0, len(segredos))
	for _, s := range segredos {
		s.Valor = referenciaCanonica(upstream, s)
		s.Limpar = false
		out = append(out, s)
	}
	return semVazio(out)
}

// Opcoes são as escolhas de quem importa.
type Opcoes struct {
	// RemoverAusentes apaga o que existe no banco e não está no arquivo. Fora
	// dela, ausência nunca apaga: um arquivo exportado de outra instalação — ou
	// editado à mão com um pedaço só da configuração — apagaria o resto sem
	// ninguém ter pedido.
	RemoverAusentes bool
}

// Planejar lê o YAML e devolve o que o import faria, sem escrever nada.
//
// As Opcoes entram aqui, e não só no Aplicar, porque --remover-ausentes muda o
// plano: sem ela o item ausente é reportado, com ela ele é uma remoção. Um plano
// que não dependesse da flag seria um plano que não descreve o que vai acontecer.
func (s *Servico) Planejar(ctx context.Context, dados []byte, o Opcoes) (Plano, error) {
	doc, err := Ler(dados)
	if err != nil {
		return Plano{}, err
	}

	banco, err := s.estadoDoBanco(ctx)
	if err != nil {
		return Plano{}, err
	}

	p := Plano{
		Versao:          doc.Versao,
		Revisao:         doc.Revisao,
		RevisaoAtual:    banco.Revisao,
		BancoAvancou:    doc.Revisao != "" && doc.Revisao != banco.Revisao,
		RemoverAusentes: o.RemoverAusentes,
	}

	upsNoBanco, err := s.upstreams.Listar(ctx)
	if err != nil {
		return Plano{}, fmt.Errorf("configuracao: listar upstreams: %w", err)
	}
	endsNoBanco, err := s.endpoints.Listar(ctx)
	if err != nil {
		return Plano{}, fmt.Errorf("configuracao: listar endpoints: %w", err)
	}

	citadoPor := upstreamsCitadosPorEndpoints(doc.Endpoints)
	itensUp, idsPorNome, removidos := s.planejarUpstreams(ctx, doc, banco, upsNoBanco, citadoPor, o)
	p.Itens = append(p.Itens, itensUp...)
	p.Itens = append(p.Itens, s.planejarSegredos(doc, idsPorNome)...)
	p.Itens = append(p.Itens, s.planejarEndpoints(ctx, doc, banco, endsNoBanco, removidos, o)...)
	p.Itens = append(p.Itens, itensInformativos(doc)...)
	return p, nil
}

// Ler analisa o YAML e devolve o documento normalizado.
//
// Exportada porque a UI precisa recusar um arquivo colado antes de montar plano
// nenhum, e porque a mensagem de recusa é a mesma nos dois caminhos.
func Ler(dados []byte) (Documento, error) {
	var doc Documento
	dec := yaml.NewDecoder(bytes.NewReader(dados))
	// Campo desconhecido é recusado: `timeout` no lugar de `timeout_ms` seria um
	// import que "funciona" e não muda o timeout, e o dono descobriria isso na
	// próxima vez que o upstream travasse.
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return Documento{}, fmt.Errorf("%w: %w", ErrYAMLInvalido, err)
	}
	if doc.Versao != VersaoAtual {
		return Documento{}, fmt.Errorf("%w: o arquivo diz %d e este patchbay lê %d",
			ErrVersaoDesconhecida, doc.Versao, VersaoAtual)
	}

	for i := range doc.Upstreams {
		doc.Upstreams[i] = doc.Upstreams[i].normalizado()
	}
	for i := range doc.Endpoints {
		doc.Endpoints[i] = doc.Endpoints[i].normalizado()
	}
	if err := conferirIdentidades(doc); err != nil {
		return Documento{}, err
	}
	return doc, nil
}

// conferirIdentidades recusa o documento com identidade repetida.
//
// Erro de documento e não item de plano: com dois upstreams de mesmo nome não há
// como saber qual das duas definições o dono quis, e aplicar uma das duas em
// silêncio é a pior das três opções.
func conferirIdentidades(doc Documento) error {
	vistos := make(map[string]bool, len(doc.Upstreams))
	for _, u := range doc.Upstreams {
		if vistos[u.Nome] {
			return fmt.Errorf("%w: o upstream %q aparece duas vezes", ErrNomeRepetido, u.Nome)
		}
		vistos[u.Nome] = true
	}
	slugs := make(map[string]bool, len(doc.Endpoints))
	for _, e := range doc.Endpoints {
		if slugs[e.Slug] {
			return fmt.Errorf("%w: o endpoint %q aparece duas vezes", ErrNomeRepetido, e.Slug)
		}
		slugs[e.Slug] = true
	}
	return nil
}

// planejarUpstreams decide o que fazer com cada upstream, e devolve também o
// mapa nome→id (para os segredos) e o conjunto de nomes que sairiam do banco
// (para barrar endpoint que ainda os referencia).
func (s *Servico) planejarUpstreams(
	ctx context.Context, doc, banco Documento, noBanco []UpstreamNoBanco, citadoPor map[string]string, o Opcoes,
) (itens []Item, idsPorNome map[string]int64, removidos map[string]bool) {
	idsPorNome = make(map[string]int64, len(noBanco))
	for _, u := range noBanco {
		idsPorNome[u.Item.normalizado().Nome] = u.ID
	}
	porNome := make(map[string]Upstream, len(banco.Upstreams))
	for _, u := range banco.Upstreams {
		porNome[u.Nome] = u
	}

	itens = make([]Item, 0, len(doc.Upstreams))
	noArquivo := make(map[string]bool, len(doc.Upstreams))
	for _, desejado := range doc.Upstreams {
		noArquivo[desejado.Nome] = true
		item := Item{Tipo: ItemUpstream, Nome: desejado.Nome, id: idsPorNome[desejado.Nome]}

		if motivo := motivoDeUpstreamInvalido(desejado); motivo != "" {
			item.Operacao, item.Motivo = OperacaoErro, motivo
			itens = append(itens, item)
			continue
		}

		atual, existe := porNome[desejado.Nome]
		item.Operacao, item.Motivo = decidir(desejado.Revisao, desejado.resumo(), atual.resumo(), existe)
		if item.Operacao == OperacaoConflito {
			item.Divergencias = divergenciasDeUpstream(desejado, atual)
		}
		if item.Aplicavel() {
			if err := s.upstreams.Conferir(ctx, desejado); err != nil {
				item.Operacao, item.Motivo, item.Divergencias = OperacaoErro, err.Error(), nil
			} else {
				semSegredo := desejado
				semSegredo.Segredos = nil
				item.upstream = &semSegredo
			}
		}
		itens = append(itens, item)
	}

	removidos = make(map[string]bool)
	for _, u := range banco.Upstreams {
		if noArquivo[u.Nome] {
			continue
		}
		item := Item{Tipo: ItemUpstream, Nome: u.Nome, id: idsPorNome[u.Nome]}
		switch {
		case !o.RemoverAusentes:
			item.Operacao = OperacaoAusente
			item.Motivo = "existe no banco e não no arquivo"
		case citadoPor[u.Nome] != "":
			// A composição do endpoint só é reescrita quando o próprio endpoint é
			// aplicado — e um endpoint em erro, em conflito ou sem mudança não
			// reescreve nada. Sem esta recusa, remover o upstream cascatearia por
			// ON DELETE CASCADE e apagaria o vínculo de baixo do endpoint, que o
			// plano nunca alertou como afetado.
			item.Operacao = OperacaoErro
			item.Motivo = "o endpoint " + citadoPor[u.Nome] +
				" ainda cita este upstream no arquivo; tire o vínculo de lá antes de remover o upstream"
		default:
			item.Operacao = OperacaoRemover
			removidos[u.Nome] = true
		}
		itens = append(itens, item)
	}
	return itens, idsPorNome, removidos
}

// upstreamsCitadosPorEndpoints mapeia cada upstream citado numa composição do
// arquivo ao slug do primeiro endpoint que o cita.
//
// Calculado antes de planejar upstream ou endpoint porque a decisão de remover
// um upstream depende do arquivo inteiro, e não só do que aquele upstream faz:
// um endpoint que ainda cita o upstream impede a remoção mesmo que o próprio
// item do endpoint termine em erro por outro motivo.
func upstreamsCitadosPorEndpoints(endpoints []Endpoint) map[string]string {
	out := make(map[string]string)
	for _, e := range endpoints {
		for _, v := range e.Upstreams {
			if _, existe := out[v.Nome]; !existe {
				out[v.Nome] = e.Slug
			}
		}
	}
	return out
}

// planejarSegredos transforma cada slot do arquivo numa intenção de escrita.
//
// Independente do veredito da configuração do upstream: uma credencial nova é
// aplicada mesmo que a configuração daquele upstream tenha conflitado, porque as
// duas coisas não se contradizem — e um bearer que não entra por causa de um
// timeout divergente seria um 401 que ninguém explica.
//
// Segredo nunca conflita: não há como comparar o valor gravado com o valor
// referenciado sem decifrar os dois, e referência resolvida é um pedido
// explícito de sobrescrever.
func (s *Servico) planejarSegredos(doc Documento, idsPorNome map[string]int64) []Item {
	var itens []Item
	for _, u := range doc.Upstreams {
		if motivoDeUpstreamInvalido(u) != "" {
			continue
		}
		for _, slot := range u.Segredos {
			item := Item{
				Tipo: ItemSegredo,
				Nome: u.Nome + " · " + rotuloDoSlot(slot),
			}
			alvo := &slotDeSegredo{
				upstreamID:   idsPorNome[u.Nome],
				upstreamNome: u.Nome,
				tipo:         slot.Tipo,
				nome:         slot.Nome,
			}
			switch {
			case slot.Limpar:
				item.Operacao, item.Motivo = OperacaoRemover, "limpar: true apaga a credencial gravada"
				alvo.limpar = true
			default:
				nome, _ := nomeDaReferencia(slot.Valor)
				valor, definida := s.ambiente(nome)
				if !definida {
					item.Operacao = OperacaoSemMudanca
					item.Motivo = variavelAusente(nome)
					itens = append(itens, item)
					continue
				}
				item.Operacao = OperacaoAtualizar
				item.Motivo = "valor lido de " + nome
				alvo.valor = cripto.Segredo(valor)
			}
			item.segredo = alvo
			itens = append(itens, item)
		}
	}
	return itens
}

// variavelAusente explica por que o slot ficou como estava. Nomeia a variável e
// nunca o valor — é a informação de que o operador precisa para resolver.
func variavelAusente(nome string) string {
	if nome == "" {
		return "sem referência a variável de ambiente; a credencial gravada fica como está"
	}
	return nome + " não está definida no ambiente; a credencial gravada fica como está"
}

// planejarEndpoints decide o que fazer com cada endpoint.
func (s *Servico) planejarEndpoints(
	ctx context.Context, doc, banco Documento, noBanco []EndpointNoBanco,
	removidos map[string]bool, o Opcoes,
) []Item {
	idsPorSlug := make(map[string]int64, len(noBanco))
	for _, e := range noBanco {
		idsPorSlug[e.Item.normalizado().Slug] = e.ID
	}
	porSlug := make(map[string]Endpoint, len(banco.Endpoints))
	for _, e := range banco.Endpoints {
		porSlug[e.Slug] = e
	}
	// O conjunto de upstreams que existirão depois do import: o que o arquivo
	// traz mais o que o banco já tem, menos o que este import removeria.
	disponiveis := make(map[string]bool, len(doc.Upstreams)+len(banco.Upstreams))
	for _, u := range banco.Upstreams {
		disponiveis[u.Nome] = !removidos[u.Nome]
	}
	for _, u := range doc.Upstreams {
		if motivoDeUpstreamInvalido(u) == "" {
			disponiveis[u.Nome] = true
		}
	}

	itens := make([]Item, 0, len(doc.Endpoints))
	noArquivo := make(map[string]bool, len(doc.Endpoints))
	for _, desejado := range doc.Endpoints {
		noArquivo[desejado.Slug] = true
		item := Item{Tipo: ItemEndpoint, Nome: desejado.Slug, id: idsPorSlug[desejado.Slug]}

		motivo := motivoDeEndpointInvalido(desejado)
		if motivo == "" {
			motivo = motivoDeVinculoSemUpstream(desejado, disponiveis)
		}
		if motivo != "" {
			item.Operacao, item.Motivo = OperacaoErro, motivo
			itens = append(itens, item)
			continue
		}

		atual, existe := porSlug[desejado.Slug]
		item.Operacao, item.Motivo = decidir(desejado.Revisao, desejado.resumo(), atual.resumo(), existe)
		if item.Operacao == OperacaoConflito {
			item.Divergencias = divergenciasDeEndpoint(desejado, atual)
		}
		if item.Aplicavel() {
			if err := s.endpoints.Conferir(ctx, desejado); err != nil {
				item.Operacao, item.Motivo, item.Divergencias = OperacaoErro, err.Error(), nil
			} else {
				copia := desejado
				item.endpoint = &copia
			}
		}
		itens = append(itens, item)
	}

	for _, e := range banco.Endpoints {
		if noArquivo[e.Slug] {
			continue
		}
		item := Item{Tipo: ItemEndpoint, Nome: e.Slug, id: idsPorSlug[e.Slug]}
		if o.RemoverAusentes {
			item.Operacao = OperacaoRemover
		} else {
			item.Operacao = OperacaoAusente
			item.Motivo = "existe no banco e não no arquivo"
		}
		itens = append(itens, item)
	}
	return itens
}

// motivoDeVinculoSemUpstream recusa composição que cita upstream que não vai
// existir. Barrado no plano e não na aplicação porque é justamente o tipo de erro
// que o --dry-run existe para mostrar.
func motivoDeVinculoSemUpstream(e Endpoint, disponiveis map[string]bool) string {
	for _, v := range e.Upstreams {
		if !disponiveis[v.Nome] {
			return "a composição cita o upstream " + v.Nome +
				", que não está no arquivo nem no banco (ou está sendo removido)"
		}
	}
	return ""
}

// itensInformativos são as duas seções que o export registra e o import não
// aplica.
func itensInformativos(doc Documento) []Item {
	itens := make([]Item, 0, len(doc.ChavesAPI)+len(doc.ClientesOAuth))
	for _, c := range doc.ChavesAPI {
		itens = append(itens, Item{
			Tipo: ItemChaveAPI, Nome: c.Nome, Operacao: OperacaoInformativo,
			Motivo: "chave de API é guardada por hash; o import não a emite — use a UI",
		})
	}
	for _, c := range doc.ClientesOAuth {
		itens = append(itens, Item{
			Tipo: ItemClienteOAuth, Nome: c.Nome, Operacao: OperacaoInformativo,
			Motivo: "cliente OAuth guarda o segredo por hash; o import não o cadastra — use a UI",
		})
	}
	return itens
}

// decidir é a mescla de três vias.
//
// A base é a revisão que o próprio item do arquivo carrega, e é isso que dispensa
// guardar o estado do último export em lugar nenhum: quem edita o item mexe nos
// campos e não na revisão, então "o resumo do arquivo bate com a revisão que ele
// carrega" significa exatamente "este item não foi editado".
//
// Item sem revisão é arquivo escrito à mão, sem trava: a intenção declarada vale,
// e o resultado é atualizar.
func decidir(base, noYAML, noBanco string, existeNoBanco bool) (Operacao, string) {
	switch {
	case !existeNoBanco:
		return OperacaoCriar, ""
	case noYAML == noBanco:
		return OperacaoSemMudanca, ""
	case base == "":
		return OperacaoAtualizar, "item sem revisão: o arquivo vale como intenção"
	case base == noBanco:
		return OperacaoAtualizar, ""
	case base == noYAML:
		return OperacaoSemMudanca, "o banco mudou depois do export e o arquivo não; o banco fica"
	default:
		return OperacaoConflito, "o arquivo e o banco mudaram desde o export; nada foi aplicado neste item"
	}
}

// Aplicar escreve o plano no banco, item a item.
//
// Cada item é uma transação própria — a do repositório da feature —, e um item que
// falha não desfaz os anteriores. É a escolha deliberada da fatia: uma transação
// única sobre a configuração inteira transformaria um nome de header inválido no
// décimo upstream em "nada foi importado", e o dono repetiria o import inteiro
// para descobrir o mesmo erro.
//
// A ordem é por fase e não a do plano: upstream antes de endpoint na criação,
// endpoint antes de upstream na remoção.
func (s *Servico) Aplicar(ctx context.Context, p Plano) (Relatorio, error) {
	fila := make([]Item, 0, len(p.Itens))
	var rel Relatorio
	for _, i := range p.Itens {
		if i.Aplicavel() {
			fila = append(fila, i)
			continue
		}
		rel.Ignorados = append(rel.Ignorados, i)
	}
	slices.SortStableFunc(fila, func(a, b Item) int { return a.fase() - b.fase() })

	// Segredo de upstream criado neste mesmo import não tem id no plano: ele
	// nasce aqui.
	criados := make(map[string]int64)
	for _, item := range fila {
		if err := ctx.Err(); err != nil {
			return rel, fmt.Errorf("configuracao: import interrompido: %w", err)
		}
		res := Resultado{Item: item}
		if err := s.aplicarItem(ctx, item, criados); err != nil {
			res.Erro = err.Error()
			s.log.Warn("item do import não foi aplicado",
				"tipo", item.Tipo, "nome", item.Nome, "operacao", string(item.Operacao), "erro", err)
		}
		rel.Resultados = append(rel.Resultados, res)
	}

	if rel.Falhas() > 0 {
		return rel, fmt.Errorf("%w: %d de %d item(ns) falharam",
			ErrAplicacaoParcial, rel.Falhas(), len(rel.Resultados))
	}
	return rel, nil
}

func (s *Servico) aplicarItem(ctx context.Context, item Item, criados map[string]int64) error {
	switch {
	case item.Tipo == ItemUpstream && item.Operacao == OperacaoRemover:
		return s.upstreams.Remover(ctx, item.id)
	case item.Tipo == ItemUpstream && item.Operacao == OperacaoCriar:
		id, err := s.upstreams.Criar(ctx, *item.upstream)
		if err != nil {
			return err
		}
		criados[item.upstream.Nome] = id
		return nil
	case item.Tipo == ItemUpstream:
		return s.upstreams.Atualizar(ctx, item.id, *item.upstream)

	case item.Tipo == ItemSegredo:
		return s.aplicarSegredo(ctx, item, criados)

	case item.Tipo == ItemEndpoint && item.Operacao == OperacaoRemover:
		return s.endpoints.Remover(ctx, item.id)
	case item.Tipo == ItemEndpoint && item.Operacao == OperacaoCriar:
		_, err := s.endpoints.Criar(ctx, *item.endpoint)
		return err
	case item.Tipo == ItemEndpoint:
		return s.endpoints.Atualizar(ctx, item.id, *item.endpoint)

	default:
		// Item aplicável de tipo que este pacote não escreve é erro de
		// programação: o plano só marca Aplicavel os três tipos acima.
		return fmt.Errorf("configuracao: tipo de item sem aplicação: %s", item.Tipo)
	}
}

func (s *Servico) aplicarSegredo(ctx context.Context, item Item, criados map[string]int64) error {
	alvo := item.segredo
	id := alvo.upstreamID
	if id == 0 {
		id = criados[alvo.upstreamNome]
	}
	if id == 0 {
		return fmt.Errorf("configuracao: o upstream %s não existe para gravar %s/%s",
			alvo.upstreamNome, alvo.tipo, alvo.nome)
	}
	if alvo.limpar {
		return s.segredos.Apagar(ctx, id, alvo.tipo, alvo.nome)
	}
	return s.segredos.Definir(ctx, id, alvo.tipo, alvo.nome, alvo.valor)
}
