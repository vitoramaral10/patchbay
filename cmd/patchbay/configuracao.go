package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/vitoramaral10/patchbay/internal/apikey"
	"github.com/vitoramaral10/patchbay/internal/authsrv"
	"github.com/vitoramaral10/patchbay/internal/catalogo"
	"github.com/vitoramaral10/patchbay/internal/configuracao"
	"github.com/vitoramaral10/patchbay/internal/endpoint"
	"github.com/vitoramaral10/patchbay/internal/platform/cripto"
	"github.com/vitoramaral10/patchbay/internal/upstream"
)

// As costuras do export/import de YAML.
//
// internal/configuracao atravessa quatro features (upstream, endpoint, apikey e
// authsrv) e não importa nenhuma: ele declara as portas e recebe estes
// adaptadores. É a mesma regra de adaptadores.go — só main conhece as duas
// pontas —, e é o que permite ao pacote de configuração falar em nome de upstream
// enquanto os repositórios continuam falando em id.

// upstreamsDaConfiguracao implementa configuracao.Upstreams.
//
// gerente pode ser nil: o subcomando de linha de comando abre o banco sem
// supervisão nenhuma, como o seed. Quando ele existe (import pela UI), a mudança
// entra em vigor sem esperar o próximo boot.
type upstreamsDaConfiguracao struct {
	repo           *upstream.RepositorioSQLite
	gerente        *upstream.Gerente
	rematerializar func(context.Context) error
	log            *slog.Logger
}

// Listar devolve os upstreams com os slots de credencial e nenhum valor.
func (a upstreamsDaConfiguracao) Listar(ctx context.Context) ([]configuracao.UpstreamNoBanco, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]configuracao.UpstreamNoBanco, 0, len(regs))
	for _, reg := range regs {
		// CredenciaisDefinidas e não Credenciais: o export precisa saber que
		// existe um bearer, e não pode — nem quer — decifrá-lo.
		definidas, err := a.repo.CredenciaisDefinidas(ctx, reg.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, configuracao.UpstreamNoBanco{ID: reg.ID, Item: itemDeUpstream(reg, definidas)})
	}
	return out, nil
}

// Conferir roda a validação do formulário da feature e recusa a troca de
// transporte.
//
// A troca de tipo é barrada aqui porque o repositório não a aplica de propósito
// (repositorio_sqlite.go: tipo é escolhido na criação): sem esta recusa, o import
// diria "atualizado" e o tipo continuaria o mesmo, e o export seguinte mostraria
// a mesma diferença para sempre.
func (a upstreamsDaConfiguracao) Conferir(ctx context.Context, u configuracao.Upstream) error {
	form := formDeUpstream(u)
	if !form.Validar() {
		return errosDeFormulario(form.Erros)
	}
	reg, existe, err := a.porNome(ctx, u.Nome)
	if err != nil {
		return err
	}
	if existe && reg.Tipo != form.TipoEfetivo() {
		return fmt.Errorf(
			"o upstream %s é %s no banco e %s no arquivo; trocar o transporte não é editar o upstream, "+
				"é substituí-lo — remova e cadastre outro", u.Nome, reg.Tipo, form.TipoEfetivo())
	}
	return nil
}

// Criar grava o upstream e o põe no ar quando há supervisão.
func (a upstreamsDaConfiguracao) Criar(ctx context.Context, u configuracao.Upstream) (int64, error) {
	form := formDeUpstream(u)
	if !form.Validar() {
		return 0, errosDeFormulario(form.Erros)
	}
	id, err := a.repo.Criar(ctx, form)
	if err != nil {
		return 0, err
	}
	a.aplicarNoAr(ctx, id)
	return id, nil
}

// Atualizar grava a configuração sem tocar em credencial nenhuma: o formulário vai
// com os campos de segredo vazios, e vazio significa "mantém o que está gravado".
func (a upstreamsDaConfiguracao) Atualizar(ctx context.Context, id int64, u configuracao.Upstream) error {
	form := formDeUpstream(u)
	form.ID = id
	if !form.Validar() {
		return errosDeFormulario(form.Erros)
	}
	if err := a.repo.Atualizar(ctx, id, form); err != nil {
		return err
	}
	a.aplicarNoAr(ctx, id)
	return nil
}

// Remover apaga o upstream do banco e o tira da supervisão.
func (a upstreamsDaConfiguracao) Remover(ctx context.Context, id int64) error {
	if err := a.repo.Remover(ctx, id); err != nil {
		return err
	}
	if a.gerente != nil {
		if err := a.gerente.Remover(ctx, id); err != nil {
			a.log.Error("upstream apagado do banco mas não saiu da supervisão",
				"upstream_id", id, "erro", err)
		}
	}
	a.sincronizar(ctx)
	return nil
}

// aplicarNoAr traduz a intenção gravada (habilitado) em supervisão, como a tela de
// upstream faz. Sem gerente — no subcomando de CLI — não há nada a aplicar: o
// banco é a fonte de verdade e o próximo boot lê dele.
func (a upstreamsDaConfiguracao) aplicarNoAr(ctx context.Context, id int64) {
	if a.gerente == nil {
		return
	}
	reg, err := a.repo.Obter(ctx, id)
	if err != nil {
		a.log.Error("upstream gravado mas não relido para aplicar", "upstream_id", id, "erro", err)
		return
	}
	if reg.Habilitado {
		err = a.gerente.Aplicar(ctx, reg.Config())
	} else {
		err = a.gerente.Remover(ctx, reg.ID)
	}
	if err != nil {
		a.log.Error("upstream gravado mas não aplicado em tempo de execução",
			"upstream", reg.Nome, "upstream_id", reg.ID, "erro", err)
	}
	a.sincronizar(ctx)
}

func (a upstreamsDaConfiguracao) sincronizar(ctx context.Context) {
	if a.rematerializar == nil {
		return
	}
	if err := a.rematerializar(ctx); err != nil {
		a.log.Error("falha ao rematerializar endpoints após import", "erro", err)
	}
}

func (a upstreamsDaConfiguracao) porNome(ctx context.Context, nome string) (upstream.Registro, bool, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return upstream.Registro{}, false, err
	}
	for _, reg := range regs {
		if reg.Nome == nome {
			return reg, true, nil
		}
	}
	return upstream.Registro{}, false, nil
}

// segredosDaConfiguracao implementa configuracao.SegredosDeUpstream.
type segredosDaConfiguracao struct {
	repo *upstream.RepositorioSQLite
}

// Definir grava a credencial cifrada no slot.
func (a segredosDaConfiguracao) Definir(
	ctx context.Context, upstreamID int64, tipo, nome string, valor cripto.Segredo,
) error {
	return a.repo.DefinirCredencial(ctx, upstreamID, tipo, nome, valor)
}

// Apagar remove a credencial do slot.
func (a segredosDaConfiguracao) Apagar(ctx context.Context, upstreamID int64, tipo, nome string) error {
	return a.repo.ApagarCredencial(ctx, upstreamID, tipo, nome)
}

// endpointsDaConfiguracao implementa configuracao.Endpoints.
//
// Precisa dos dois repositórios porque a composição referencia upstream por nome
// no arquivo e por id no banco: a tradução é trabalho de adaptador, não das duas
// features.
type endpointsDaConfiguracao struct {
	repo       *endpoint.RepositorioSQLite
	upstreams  *upstream.RepositorioSQLite
	servidores *endpoint.Servidores
	log        *slog.Logger
}

// Listar devolve os endpoints com a composição, já traduzida para nomes.
func (a endpointsDaConfiguracao) Listar(ctx context.Context) ([]configuracao.EndpointNoBanco, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return nil, err
	}
	nomes, err := a.nomesPorID(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]configuracao.EndpointNoBanco, 0, len(regs))
	for _, reg := range regs {
		composicao, err := a.repo.ComposicaoDe(ctx, reg.ID)
		if err != nil {
			return nil, err
		}
		item := configuracao.Endpoint{
			Slug:       reg.Slug,
			Nome:       reg.Nome,
			Descricao:  reg.Descricao,
			Instrucoes: reg.Instrucoes,
		}
		for _, c := range composicao {
			nome, ok := nomes[c.UpstreamID]
			if !ok {
				// Só acontece com linha órfã de composição, que a cascata do
				// banco não deixa existir. Exportar um vínculo sem nome faria o
				// import recusar o arquivo inteiro, então ele sai de fora e o
				// log diz que saiu.
				a.log.Warn("vínculo de composição sem upstream correspondente",
					"endpoint", reg.Slug, "upstream_id", c.UpstreamID)
				continue
			}
			item.Upstreams = append(item.Upstreams, configuracao.Vinculo{
				Nome:    nome,
				Prefixo: c.Prefixo,
				Regras:  regrasDaConfiguracao(c.Regras),
			})
		}
		out = append(out, configuracao.EndpointNoBanco{ID: reg.ID, Item: item})
	}
	return out, nil
}

// Conferir roda a validação do formulário de endpoint sem escrever nada.
//
// Os ids usados aqui são sintéticos: Validar só os usa como chave dos mapas de
// prefixo e de regras, e um upstream que este mesmo import ainda vai criar não
// tem id para oferecer.
func (a endpointsDaConfiguracao) Conferir(ctx context.Context, e configuracao.Endpoint) error {
	existe := true
	if _, err := a.porSlug(ctx, e.Slug); err != nil {
		if !errors.Is(err, endpoint.ErrNaoEncontrado) {
			return err
		}
		existe = false
	}
	ids := make(map[string]int64, len(e.Upstreams))
	for i, v := range e.Upstreams {
		ids[v.Nome] = int64(i + 1)
	}
	form := formDeEndpoint(e, ids, existe)
	if !form.Validar() {
		return errosDeFormulario(form.Erros)
	}
	return nil
}

// Criar grava o endpoint e a composição.
func (a endpointsDaConfiguracao) Criar(ctx context.Context, e configuracao.Endpoint) (int64, error) {
	form, err := a.formResolvido(ctx, e, false)
	if err != nil {
		return 0, err
	}
	id, err := a.repo.Criar(ctx, form)
	if err != nil {
		return 0, err
	}
	a.sincronizar(ctx)
	return id, nil
}

// Atualizar grava nome, descrição, instruções e a composição inteira. O slug não
// entra: ele é contrato e não tem UPDATE, nem aqui nem na tela.
func (a endpointsDaConfiguracao) Atualizar(ctx context.Context, id int64, e configuracao.Endpoint) error {
	form, err := a.formResolvido(ctx, e, true)
	if err != nil {
		return err
	}
	form.ID = id
	if err := a.repo.Atualizar(ctx, id, form); err != nil {
		return err
	}
	a.sincronizar(ctx)
	return nil
}

// Remover apaga o endpoint. A composição e o escopo de chave saem por cascata.
func (a endpointsDaConfiguracao) Remover(ctx context.Context, id int64) error {
	if err := a.repo.Remover(ctx, id); err != nil {
		return err
	}
	a.sincronizar(ctx)
	return nil
}

func (a endpointsDaConfiguracao) sincronizar(ctx context.Context) {
	if a.servidores == nil {
		return
	}
	if err := a.servidores.Sincronizar(ctx); err != nil {
		a.log.Error("falha ao rematerializar endpoints após import", "erro", err)
	}
}

// formResolvido traduz os nomes de upstream do arquivo nos ids do banco.
func (a endpointsDaConfiguracao) formResolvido(
	ctx context.Context, e configuracao.Endpoint, slugFixo bool,
) (endpoint.Form, error) {
	regs, err := a.upstreams.Todos(ctx)
	if err != nil {
		return endpoint.Form{}, err
	}
	ids := make(map[string]int64, len(regs))
	for _, reg := range regs {
		ids[reg.Nome] = reg.ID
	}
	for _, v := range e.Upstreams {
		if _, ok := ids[v.Nome]; !ok {
			return endpoint.Form{}, fmt.Errorf(
				"a composição de %s cita o upstream %s, que não existe no banco", e.Slug, v.Nome)
		}
	}
	form := formDeEndpoint(e, ids, slugFixo)
	if !form.Validar() {
		return endpoint.Form{}, errosDeFormulario(form.Erros)
	}
	return form, nil
}

func (a endpointsDaConfiguracao) porSlug(ctx context.Context, slug string) (endpoint.Registro, error) {
	regs, err := a.repo.Todos(ctx)
	if err != nil {
		return endpoint.Registro{}, err
	}
	for _, reg := range regs {
		if reg.Slug == slug {
			return reg, nil
		}
	}
	return endpoint.Registro{}, endpoint.ErrNaoEncontrado
}

func (a endpointsDaConfiguracao) nomesPorID(ctx context.Context) (map[int64]string, error) {
	regs, err := a.upstreams.Todos(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(regs))
	for _, reg := range regs {
		out[reg.ID] = reg.Nome
	}
	return out, nil
}

// chavesDaConfiguracao implementa configuracao.Chaves. Só leitura: a chave é
// guardada por hash, e o import não emite nenhuma.
type chavesDaConfiguracao struct {
	repo *apikey.RepositorioSQLite
}

// Listar devolve as chaves emitidas, sem segredo nenhum.
func (a chavesDaConfiguracao) Listar(ctx context.Context) ([]configuracao.ChaveAPI, error) {
	chaves, err := a.repo.Todas(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]configuracao.ChaveAPI, 0, len(chaves))
	for _, c := range chaves {
		out = append(out, configuracao.ChaveAPI{
			Nome:           c.Nome,
			PrefixoVisivel: c.PrefixoVisivel,
			Endpoints:      slices.Clone(c.Endpoints),
			Revogada:       c.Revogada(),
		})
	}
	return out, nil
}

// clientesDaConfiguracao implementa configuracao.Clientes.
type clientesDaConfiguracao struct {
	repo *authsrv.RepositorioSQLite
}

// Listar devolve os clientes OAuth por metadados, sem segredo nenhum.
func (a clientesDaConfiguracao) Listar(ctx context.Context) ([]configuracao.ClienteOAuth, error) {
	clientes, err := a.repo.TodosClientes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]configuracao.ClienteOAuth, 0, len(clientes))
	for _, c := range clientes {
		slugs := make([]string, 0, len(c.Endpoints))
		for _, e := range c.Endpoints {
			slugs = append(slugs, e.Slug)
		}
		out = append(out, configuracao.ClienteOAuth{
			ClientID:     c.ClientID,
			Nome:         c.Nome,
			Tipo:         c.Tipo,
			Confidencial: c.Confidencial,
			RedirectURIs: slices.Clone(c.RedirectURIs),
			Endpoints:    slugs,
			Revogado:     c.Revogado(),
		})
	}
	return out, nil
}

// itemDeUpstream traduz a linha do banco no item do arquivo.
func itemDeUpstream(reg upstream.Registro, definidas []upstream.CredencialDefinida) configuracao.Upstream {
	item := configuracao.Upstream{
		Nome:       reg.Nome,
		Tipo:       reg.Tipo,
		URL:        reg.URL,
		Comando:    reg.Comando,
		Args:       slices.Clone(reg.Args),
		Env:        maps.Clone(reg.Env),
		TimeoutMS:  reg.TimeoutMS,
		Habilitado: reg.Habilitado,
	}
	for _, d := range definidas {
		item.Segredos = append(item.Segredos, configuracao.Segredo{Tipo: d.Tipo, Nome: d.Nome})
	}
	return item
}

// formDeUpstream monta o formulário da feature a partir do item do arquivo.
//
// Os campos de credencial ficam vazios de propósito: vazio significa "mantém o que
// está gravado", e o segredo do import entra pela porta própria, slot a slot.
func formDeUpstream(u configuracao.Upstream) upstream.Form {
	return upstream.Form{
		Nome:       u.Nome,
		Tipo:       u.Tipo,
		URL:        u.URL,
		TimeoutMS:  u.TimeoutMS,
		Habilitado: u.Habilitado,
		Comando:    u.Comando,
		ArgsTexto:  strings.Join(u.Args, "\n"),
		EnvTexto:   textoDeEnv(u.Env),
	}
}

// textoDeEnv escreve o mapa como a caixa NOME=valor por linha que o formulário lê,
// em ordem de nome para que o mesmo mapa dê sempre o mesmo texto.
func textoDeEnv(env map[string]string) string {
	nomes := slices.Sorted(maps.Keys(env))
	linhas := make([]string, 0, len(nomes))
	for _, nome := range nomes {
		linhas = append(linhas, nome+"="+env[nome])
	}
	return strings.Join(linhas, "\n")
}

// formDeEndpoint monta o formulário de endpoint a partir do item do arquivo.
//
// As regras atravessam como texto na forma canônica de uma linha por regra: é o
// formato que catalogo.AnalisarRegras lê, e é o que evita ter dois analisadores de
// regra no projeto para divergirem depois.
func formDeEndpoint(e configuracao.Endpoint, ids map[string]int64, slugFixo bool) endpoint.Form {
	form := endpoint.Form{
		Slug:        e.Slug,
		Nome:        e.Nome,
		Descricao:   e.Descricao,
		Instrucoes:  e.Instrucoes,
		SlugFixo:    slugFixo,
		Prefixos:    make(map[int64]string, len(e.Upstreams)),
		RegrasTexto: make(map[int64]string, len(e.Upstreams)),
	}
	for _, v := range e.Upstreams {
		id, ok := ids[v.Nome]
		if !ok {
			continue
		}
		form.UpstreamIDs = append(form.UpstreamIDs, id)
		form.Prefixos[id] = v.Prefixo
		form.RegrasTexto[id] = configuracao.TextoDeRegras(v.Regras)
	}
	return form
}

// regrasDaConfiguracao traduz as regras da composição para o item do arquivo.
func regrasDaConfiguracao(regras []catalogo.Regra) []configuracao.Regra {
	out := make([]configuracao.Regra, 0, len(regras))
	for _, r := range regras {
		out = append(out, configuracao.Regra{
			Acao:   string(r.Acao),
			Padrao: r.Padrao,
			Renome: r.Renome,
		})
	}
	return out
}

// errosDeFormulario junta os erros de campo numa mensagem só.
//
// Em ordem de campo porque a mensagem entra no relatório do import, e um
// relatório que muda de ordem entre execuções não serve para comparar duas
// tentativas.
func errosDeFormulario(erros map[string]string) error {
	if len(erros) == 0 {
		return errors.New("formulário recusado sem erro declarado")
	}
	partes := make([]string, 0, len(erros))
	for _, campo := range slices.Sorted(maps.Keys(erros)) {
		partes = append(partes, campo+": "+erros[campo])
	}
	return errors.New(strings.Join(partes, " / "))
}
