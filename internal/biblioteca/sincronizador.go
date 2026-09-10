package biblioteca

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// IntervaloDeSincronizacao é de quanto em quanto tempo o catálogo local é
// refeito.
//
// Doze horas porque o registry é um catálogo de terceiro que cresce aos poucos:
// servidor novo aparecendo meio dia depois é aceitável, e uma varredura custa
// ~300 requisições e ~16 minutos contra um serviço que não é nosso (medido em
// 2026-09-09: 28.067 servidores gravados em 15m57s). Quem precisa de agora tem o
// botão de atualizar na tela.
const IntervaloDeSincronizacao = 12 * time.Hour

// PrazoDaVarredura é quanto tempo uma varredura inteira pode levar.
//
// Existe porque a paciência por página é grande de propósito (45s), e 297
// páginas de paciência somariam horas se a origem entrasse num dia ruim. Trinta
// Uma hora cobre as duas origens somadas, com folga para as tentativas: 15m57s
// medidos na varredura completa do registry em 2026-09-09, mais ~10 minutos das
// 293 páginas de detalhe do mcpservers.org — que vão a dois segundos uma da
// outra porque a origem limita taxa. E ainda assim é um fim: passou disso, a
// varredura vira falha registrada, o catálogo anterior continua servindo, e a
// próxima tenta de novo.
const PrazoDaVarredura = time.Hour

// pisoDaCuradoria é a fração das páginas de detalhe que precisa dar certo para
// a curadoria contar.
//
// Página avulsa falhando é aceitável — são 293 requisições a um site de
// terceiro, e desistir por causa de uma jogaria fora a varredura inteira. Mas
// uma varredura em que quase tudo falhou marcaria como não-curado quem é curado,
// e o filtro "só curados" ficaria vazio sem ninguém entender por quê. Metade é
// o ponto em que o resultado deixa de ser confiável.
const pisoDaCuradoria = 0.5

const (
	// tentativasPorPagina é quantas vezes uma página é pedida antes de a
	// varredura desistir. Medido em 2026-09-09: o registry responde a maioria
	// das páginas em ~1,4s, mas requisições avulsas chegaram a passar de 40
	// segundos sem responder. Desistir na primeira falha jogaria fora meia
	// varredura por causa de um soluço.
	tentativasPorPagina = 3
	// esperaEntreTentativas é quanto se espera antes de repetir uma página.
	esperaEntreTentativas = 3 * time.Second
	// tetoDePaginas fecha a porta do cursor que nunca termina. Com 100 por
	// página, mil páginas são 100 mil servidores — muito acima dos 29.610
	// medidos, e ainda assim um fim.
	tetoDePaginas = 1000
)

// Sincronizador mantém a cópia local do catálogo.
//
// É a única coisa neste pacote que escreve. A tela só lê, e é justamente essa
// separação que faz a busca digitada não tocar a rede.
type Sincronizador struct {
	origem    *Origem
	curadoria *Curadoria
	repo      *RepositorioSQLite
	log       *slog.Logger
	intervalo time.Duration
	espera    time.Duration
	// esperaCurada é a pausa entre duas páginas de detalhe da curadoria.
	esperaCurada time.Duration
	// semente é de onde sai o catálogo do primeiro boot. Trocável só pelo
	// teste; em produção é sempre o arquivo embutido.
	semente func() ([]Item, time.Time, error)
	emCurso atomic.Bool

	// mu protege os dois campos abaixo, escritos por Manter e por Observar e
	// lidos por Disparar — ou seja, por goroutines diferentes.
	mu sync.Mutex
	// observadores é quem quer saber que uma varredura terminou. Existe para o
	// teste; em produção a lista fica vazia.
	observadores []func()
	// fundo é o contexto que Manter recebeu, guardado para Disparar poder rodar
	// uma varredura fora de hora.
	//
	// Guardar contexto num campo é exceção, e esta é a exceção clássica: a
	// varredura pedida pelo botão leva minutos e não pode herdar o prazo da
	// requisição que a pediu — ela morreria no redirecionamento. O contexto
	// certo é o da aplicação, e quem o tem é o loop de fundo.
	//
	// Nasce em context.Background() só para Disparar nunca ter um caminho nulo.
	// Em produção Manter troca por ele antes de o servidor HTTP aceitar a
	// primeira requisição, então esse valor inicial não chega a ser usado.
	//
	//nolint:containedctx // deliberado: ver o parágrafo acima
	fundo context.Context
}

// OpcaoSincronizador ajusta o sincronizador na construção.
type OpcaoSincronizador func(*Sincronizador)

// ComIntervalo troca de quanto em quanto tempo a varredura roda. Só o teste
// usa: em produção o intervalo é constante do pacote, porque não é uma escolha
// que o admin precise fazer.
func ComIntervalo(d time.Duration) OpcaoSincronizador {
	return func(s *Sincronizador) {
		if d > 0 {
			s.intervalo = d
		}
	}
}

// ComEsperaEntreTentativas troca quanto se espera antes de repetir uma página
// que falhou. Só o teste usa: sem isto, provar que a varredura insiste custaria
// segundos de relógio de verdade a cada execução da suíte.
func ComEsperaEntreTentativas(d time.Duration) OpcaoSincronizador {
	return func(s *Sincronizador) {
		if d >= 0 {
			s.espera = d
			s.esperaCurada = d
		}
	}
}

// ComSemente troca o catálogo do primeiro boot. Só o teste usa: em produção a
// semente é o arquivo embutido no binário.
func ComSemente(itens []Item, geradoEm time.Time) OpcaoSincronizador {
	return func(s *Sincronizador) {
		s.semente = func() ([]Item, time.Time, error) { return itens, geradoEm, nil }
	}
}

// NovoSincronizador monta a rotina sobre a origem e o repositório.
func NovoSincronizador(
	origem *Origem, curadoria *Curadoria, repo *RepositorioSQLite, log *slog.Logger,
	opcoes ...OpcaoSincronizador,
) *Sincronizador {
	s := &Sincronizador{
		origem:       origem,
		curadoria:    curadoria,
		repo:         repo,
		log:          log,
		intervalo:    IntervaloDeSincronizacao,
		espera:       esperaEntreTentativas,
		esperaCurada: esperaEntreCuradas,
		semente:      Semente,
		fundo:        context.Background(),
	}
	for _, o := range opcoes {
		o(s)
	}
	return s
}

// Manter roda a varredura até o ctx ser cancelado.
//
// A primeira acontece no boot só quando o catálogo local está vencido ou não
// existe. Varrer a cada boot puniria quem reinicia o patchbay com uma varredura
// de minutos contra um serviço de terceiro, sem nada a ganhar: o catálogo que
// está no banco continua valendo.
func (s *Sincronizador) Manter(ctx context.Context) {
	s.mu.Lock()
	s.fundo = ctx
	s.mu.Unlock()

	// Semeou agora, varre agora — sem consultar a idade. A semente é parcial de
	// propósito (só os curados) e pode ter sido gerada hoje, no corte da versão:
	// deixá-la decidir pela idade faria a instalação nova passar doze horas com
	// algumas centenas de servidores achando que o catálogo está completo.
	if s.semear(ctx) || s.precisaAgora(ctx) {
		s.tentar(ctx)
	}
	t := time.NewTicker(s.intervalo)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tentar(ctx)
		}
	}
}

// semear grava o catálogo embutido quando o banco ainda não tem nenhum.
//
// Só na instalação nova: se alguma varredura já terminou, o que está no banco é
// mais novo que a semente por definição, e sobrescrevê-lo seria andar para trás.
//
// A data gravada é a da geração da semente, não a de agora. É o que faz a tela
// dizer a idade de verdade e o que faz precisaAgora mandar varrer em seguida —
// gravar "agora" deixaria a instalação nova doze horas com um catálogo do dia do
// release, achando que está fresco.
// Devolve true quando semeou, e quem chama usa isso para varrer em seguida.
func (s *Sincronizador) semear(ctx context.Context) bool {
	estado, err := s.repo.Sincronizacao(ctx)
	if err != nil || !estado.Nunca() {
		return false
	}
	itens, geradoEm, err := s.semente()
	if err != nil {
		s.log.Warn("semente da biblioteca ilegível", "erro", err)
		return false
	}
	if len(itens) == 0 {
		return false
	}
	if err := s.repo.Substituir(ctx, itens, geradoEm); err != nil {
		s.log.Warn("não foi possível gravar a semente da biblioteca", "erro", err)
		return false
	}
	s.log.Info("biblioteca semeada a partir do catálogo embutido",
		"servidores", len(itens), "gerado_em", geradoEm)
	return true
}

// precisaAgora decide se o catálogo no banco já não serve.
func (s *Sincronizador) precisaAgora(ctx context.Context) bool {
	estado, err := s.repo.Sincronizacao(ctx)
	if err != nil {
		// Não dá para saber a idade: varre. Uma varredura a mais custa tempo;
		// não varrer deixaria a tela vazia sem ninguém para consertar.
		s.log.Warn("não foi possível ler o estado da biblioteca", "erro", err)
		return true
	}
	return estado.Nunca() || estado.Idade() >= s.intervalo
}

// EmCurso diz se uma varredura está rodando agora, para a tela não oferecer
// disparar outra.
func (s *Sincronizador) EmCurso() bool { return s.emCurso.Load() }

// Observar registra quem quer saber que uma varredura terminou, com ou sem
// sucesso.
//
// Existe para o teste esperar por sinal em vez de pelo relógio, que é regra do
// projeto: a varredura é assíncrona por desenho, e sem isto provar que ela
// terminou seria dormir e torcer. Em produção ninguém observa.
func (s *Sincronizador) Observar(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observadores = append(s.observadores, f)
}

func (s *Sincronizador) avisar() {
	s.mu.Lock()
	observadores := make([]func(), len(s.observadores))
	copy(observadores, s.observadores)
	s.mu.Unlock()
	for _, f := range observadores {
		f()
	}
}

// Disparar pede uma varredura fora do horário, sem esperar por ela.
//
// Devolve false quando já havia uma rodando. Não enfileira: duas varreduras seguidas dariam o mesmo resultado,
// e a segunda só serviria para dobrar o peso sobre a origem.
func (s *Sincronizador) Disparar() bool {
	s.mu.Lock()
	ctx := s.fundo
	s.mu.Unlock()
	if s.emCurso.Load() {
		return false
	}
	go s.tentar(ctx)
	return true
}

// ErrJaEmCurso é uma varredura ter sido pedida enquanto outra roda.
var ErrJaEmCurso = errors.New("biblioteca: já há uma sincronização em curso")

// Sincronizar faz uma varredura agora e espera por ela.
//
// É o miolo, exportado porque o teste precisa de uma varredura determinística:
// sem isto ele esperaria pelo relógio para saber se a goroutine de fundo já
// tinha terminado, que é justamente o tipo de espera que o projeto não aceita.
//
// O estado fica gravado dos dois lados: sucesso troca o catálogo e zera o erro,
// falha registra o motivo e não toca no catálogo — a cópia anterior continua
// servindo, com a idade dizendo o que ela é.
func (s *Sincronizador) Sincronizar(ctx context.Context) error {
	if !s.emCurso.CompareAndSwap(false, true) {
		return ErrJaEmCurso
	}
	defer func() {
		s.emCurso.Store(false)
		s.avisar()
	}()

	// Dois contextos, e a diferença importa. O da varredura tem prazo; o de fora
	// é o da aplicação, e é só ele que diz "o patchbay está parando".
	//
	// Confundir os dois faria a varredura que estourou o prazo ser lida como
	// desligamento — e desligamento não registra falha, então a tela nunca
	// contaria que a última tentativa não terminou.
	ctxVarredura, cancelar := context.WithTimeout(ctx, PrazoDaVarredura)
	defer cancelar()

	itens, err := s.varrer(ctxVarredura)
	if err == nil {
		err = s.repo.Substituir(ctxVarredura, itens, time.Now())
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		// Desligando: não é falha do catálogo, e gravá-la faria a próxima
		// abertura da tela acusar um erro que foi o próprio patchbay parando.
		return err
	}
	// A gravação da falha usa o contexto de fora: se o que estourou foi o prazo
	// da varredura, o contexto dela já está cancelado e não gravaria nada.
	if erroAoGravar := s.repo.RegistrarFalha(ctx, time.Now(), err); erroAoGravar != nil {
		s.log.Warn("não foi possível registrar a falha da biblioteca", "erro", erroAoGravar)
	}
	return err
}

// tentar é a varredura do loop de fundo: a mesma coisa, com o resultado no log
// em vez de devolvido a ninguém.
func (s *Sincronizador) tentar(ctx context.Context) {
	inicio := time.Now()
	if err := s.Sincronizar(ctx); err != nil {
		if ctx.Err() == nil && !errors.Is(err, ErrJaEmCurso) {
			s.log.Warn("a sincronização da biblioteca não terminou",
				"origem", s.origem.base, "erro", err, "duracao", time.Since(inicio))
		}
		return
	}
	s.log.Info("biblioteca sincronizada", "duracao", time.Since(inicio))
}

// Varrer lê as origens e devolve o catálogo mesclado, sem gravar nada.
//
// Exportada para o subcomando que regenera a semente embutida. Não toca no
// repositório — é a única operação do sincronizador que não toca —, então um
// Sincronizador construído só para isto pode receber repo nulo.
func (s *Sincronizador) Varrer(ctx context.Context) ([]Item, error) {
	ctxVarredura, cancelar := context.WithTimeout(ctx, PrazoDaVarredura)
	defer cancelar()
	return s.varrer(ctxVarredura)
}

// varrer lê as duas origens e devolve o catálogo mesclado.
//
// O registry é a base — é dele que vêm o alcance e os servidores de processo
// local. A curadoria do mcpservers.org entra por cima, e a chave de junção é a
// **URL do endpoint**: as duas origens publicam o mesmo endereço para o mesmo
// servidor, e casar por ele é exato, ao contrário de casar por nome.
//
// O que a curadoria acrescenta a quem já estava lá: a autenticação (que o
// esquema do registry não tem), o resumo em português e a marca de curado. O que
// ela acrescenta sozinha: os remotos que o registry não conhece — medido em
// 2026-09-09, 22 de 25 amostrados.
func (s *Sincronizador) varrer(ctx context.Context) ([]Item, error) {
	doRegistry, err := s.varrerRegistry(ctx)
	if err != nil {
		return nil, err
	}
	curados, err := s.varrerCuradoria(ctx)
	if err != nil {
		return nil, err
	}
	oficiais, err := s.varrerOficiais(ctx)
	if err != nil {
		return nil, err
	}
	return mesclar(doRegistry, curados, oficiais), nil
}

// varrerOficiais lê a lista /official do mcpservers.org: 647 servidores em 22
// páginas de índice, mais uma página de detalhe cada.
//
// Tolerante por natureza, e não por acidente: a maioria das páginas **não** tem
// comando aproveitável (numa amostra de 14, só 4 tinham), então página recusada
// é o caso comum e não pode contar como falha. O que derruba a varredura aqui é
// o índice não vir — sem ele não há o que ler.
func (s *Sincronizador) varrerOficiais(ctx context.Context) ([]Item, error) {
	slugs, err := s.curadoria.SlugsOficiais(ctx)
	if err != nil {
		return nil, fmt.Errorf("oficiais: %w", err)
	}
	itens := make([]Item, 0, len(slugs)/3)
	var semComando, indisponiveis int
	for i, slug := range slugs {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s.esperaCurada):
			}
		}
		item, err := s.oficialComTentativas(ctx, slug)
		switch {
		case err == nil:
			itens = append(itens, item)
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, ErrFormatoDaOrigem):
			// Sem comando aproveitável: o normal.
			semComando++
		default:
			indisponiveis++
		}
	}
	s.log.Info("oficiais lidos",
		"aproveitados", len(itens), "sem_comando", semComando,
		"indisponiveis", indisponiveis, "total", len(slugs))
	return itens, nil
}

func (s *Sincronizador) oficialComTentativas(ctx context.Context, slug string) (Item, error) {
	var ultimo error
	for tentativa := range tentativasPorPagina {
		if tentativa > 0 {
			select {
			case <-ctx.Done():
				return Item{}, ctx.Err()
			case <-time.After(s.esperaCurada * time.Duration(1<<tentativa)):
			}
		}
		item, err := s.curadoria.Oficial(ctx, slug)
		if err == nil {
			return item, nil
		}
		if !errors.Is(err, ErrTaxaExcedida) || ctx.Err() != nil {
			return Item{}, err
		}
		ultimo = err
	}
	return Item{}, ultimo
}

// varrerCuradoria lê a lista de remotos do mcpservers.org, uma página de detalhe
// por servidor.
//
// Sequencial e com pausa: quatro requisições em paralelo faziam ~30% virarem
// desafio de bot naquele Cloudflare. Aqui a pressa não vale nada — isto roda de
// doze em doze horas, no fundo.
func (s *Sincronizador) varrerCuradoria(ctx context.Context) ([]Item, error) {
	slugs, err := s.curadoria.Slugs(ctx)
	if err != nil {
		return nil, fmt.Errorf("curadoria: %w", err)
	}
	itens := make([]Item, 0, len(slugs))
	var falhas int
	for i, slug := range slugs {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s.esperaCurada):
			}
		}
		item, err := s.curadaComTentativas(ctx, slug)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Página avulsa que não deu: o servidor fica de fora da curadoria e
			// a varredura segue. Ele ainda pode existir pelo registry.
			falhas++
			s.log.Debug("servidor curado não pôde ser lido", "slug", slug, "erro", err)
			continue
		}
		itens = append(itens, item)
	}
	if len(itens) < int(float64(len(slugs))*pisoDaCuradoria) {
		return nil, fmt.Errorf("%w: curadoria trouxe só %d de %d servidores (%d falhas)",
			ErrOrigemIndisponivel, len(itens), len(slugs), falhas)
	}
	if falhas > 0 {
		s.log.Info("curadoria lida com falhas parciais",
			"lidos", len(itens), "falhas", falhas, "total", len(slugs))
	}
	return itens, nil
}

// varrerRegistry percorre o catálogo inteiro do registry, página por página.
func (s *Sincronizador) varrerRegistry(ctx context.Context) ([]Item, error) {
	var itens []Item
	visto := map[string]bool{}
	cursor := ""

	for pagina := range tetoDePaginas {
		res, err := s.pedirComTentativas(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("página %d: %w", pagina+1, err)
		}
		for _, i := range res.Itens {
			// A origem pagina por cursor, e cursor que repete devolveria o mesmo
			// servidor duas vezes — que aqui viraria erro de chave primária no
			// meio da transação, longe da causa.
			if visto[i.Nome] {
				continue
			}
			visto[i.Nome] = true
			itens = append(itens, i)
		}
		if res.ProximoCursor == "" {
			return itens, nil
		}
		if res.ProximoCursor == cursor {
			return nil, fmt.Errorf("%w: a origem repetiu o cursor %q",
				ErrFormatoDaOrigem, cursor)
		}
		cursor = res.ProximoCursor
	}
	return nil, fmt.Errorf("%w: a origem não terminou em %d páginas",
		ErrFormatoDaOrigem, tetoDePaginas)
}

// pedirComTentativas insiste numa página antes de desistir da varredura.
//
// Só em indisponibilidade: esquema mudado não melhora tentando de novo, e
// repetir seria peso na origem sem chance de sucesso.
func (s *Sincronizador) pedirComTentativas(ctx context.Context, cursor string) (Resultado, error) {
	var ultimo error
	for tentativa := range tentativasPorPagina {
		if tentativa > 0 {
			select {
			case <-ctx.Done():
				return Resultado{}, ctx.Err()
			case <-time.After(s.espera):
			}
		}
		res, err := s.origem.Listar(ctx, "", cursor)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, ErrOrigemIndisponivel) || ctx.Err() != nil {
			return Resultado{}, err
		}
		ultimo = err
	}
	return Resultado{}, ultimo
}

// curadaComTentativas insiste numa página de detalhe enquanto a origem estiver
// limitando taxa.
//
// Só em 429, e por isso ele é erro próprio: o resto — página que sumiu, formato
// que mudou — não melhora esperando, e repetir seria peso na origem sem chance
// de sucesso. A espera cresce a cada tentativa porque um 429 costuma significar
// que o balde de taxa vai demorar mais que a pausa normal para encher.
func (s *Sincronizador) curadaComTentativas(ctx context.Context, slug string) (Item, error) {
	var ultimo error
	for tentativa := range tentativasPorPagina {
		if tentativa > 0 {
			select {
			case <-ctx.Done():
				return Item{}, ctx.Err()
			case <-time.After(s.esperaCurada * time.Duration(1<<tentativa)):
			}
		}
		item, err := s.curadoria.Um(ctx, slug)
		if err == nil {
			return item, nil
		}
		if !errors.Is(err, ErrTaxaExcedida) || ctx.Err() != nil {
			return Item{}, err
		}
		ultimo = err
	}
	return Item{}, ultimo
}

// mesclar junta o catálogo do registry com a curadoria do mcpservers.org.
//
// A chave é a URL do endpoint, normalizada: as duas origens publicam o mesmo
// endereço para o mesmo servidor, e casar por ele é exato. Casar por nome não
// seria — "Notion" no mcpservers.org é "com.notion/mcp" no registry.
//
// Quando os dois têm o servidor, cada lado ganha no que ele garante:
//
//   - identidade técnica é do registry (nome, namespace, versão). É o que ele
//     verifica ao aceitar a publicação, e é o que o filtro de domínio lê.
//   - texto para gente e autenticação são da curadoria. O título e o resumo de
//     lá são escritos por uma pessoa e traduzidos; a autenticação **só existe**
//     lá. O transporte também, porque a página de lá descreve exatamente aquele
//     endereço.
//
// Os oficiais entram por último e por outro caminho: eles são processo local e
// não têm URL, então casam pela **linha de comando**, com a versão do pacote
// ignorada — o registry pina (@1.2.3) e o mcpservers.org não, e sem normalizar
// isso o mesmo servidor viraria dois cartões.
//
// Servidor que só a curadoria tem entra inteiro, com identidade própria — ver
// lerCurado. Servidor que só o registry tem passa intacto, sem marca de curado.
func mesclar(doRegistry, curados, oficiais []Item) []Item {
	porURL := make(map[string]int, len(doRegistry))
	for i, it := range doRegistry {
		if it.Remoto() && it.URL != "" {
			porURL[chaveDeEndpoint(it.URL)] = i
		}
	}

	saida := make([]Item, len(doRegistry), len(doRegistry)+len(curados))
	copy(saida, doRegistry)

	// O nome é chave primária no banco, então uma colisão derrubaria a
	// varredura inteira com erro de constraint, longe da causa. Ela é
	// improvável — exigiria o registry aceitar um servidor no namespace
	// "mcpservers.org" —, e é exatamente por ser improvável que precisa ser
	// tratada aqui: ninguém a encontraria depurando o INSERT.
	nomes := make(map[string]bool, len(doRegistry)+len(curados))
	for _, it := range doRegistry {
		nomes[it.Nome] = true
	}

	for _, c := range curados {
		i, achou := porURL[chaveDeEndpoint(c.URL)]
		if !achou {
			if nomes[c.Nome] {
				continue
			}
			nomes[c.Nome] = true
			saida = append(saida, c)
			continue
		}
		base := saida[i]
		base.Curado = true
		base.Autenticacao = c.Autenticacao
		base.PedeCredencial = c.PedeCredencial
		base.Transporte = c.Transporte
		if c.Titulo != "" {
			base.Titulo = c.Titulo
		}
		if c.Descricao != "" {
			base.Descricao = c.Descricao
		}
		saida[i] = base
	}

	// Os oficiais são stdio: casam pela execução, não pela URL.
	porExecucao := make(map[string]int, len(saida))
	for i, it := range saida {
		if !it.Remoto() {
			porExecucao[chaveDeExecucao(it)] = i
		}
	}
	for _, o := range oficiais {
		if i, achou := porExecucao[chaveDeExecucao(o)]; achou {
			// Já existe pelo registry, com identificador e argumentos
			// declarados — que são melhores que o trecho de README de onde
			// estes saem. O que o oficial acrescenta é só a marca.
			saida[i].Curado = true
			continue
		}
		if nomes[o.Nome] {
			continue
		}
		nomes[o.Nome] = true
		saida = append(saida, o)
	}
	return saida
}

// chaveDeExecucao normaliza a linha de comando de um servidor de processo local
// para a comparação.
//
// A versão do pacote sai fora: o registry pina (@modelcontextprotocol/x@1.2.3) e
// o mcpservers.org publica sem versão, e tratar os dois como servidores
// diferentes duplicaria o cartão na tela.
func chaveDeExecucao(i Item) string {
	partes := make([]string, 0, len(i.Args)+1)
	partes = append(partes, strings.ToLower(i.Comando))
	for _, a := range i.Args {
		partes = append(partes, strings.ToLower(semVersao(a)))
	}
	return strings.Join(partes, " ")
}

// semVersao tira o sufixo @versão de um identificador de pacote, preservando o
// @ inicial do escopo npm (@org/pacote).
func semVersao(arg string) string {
	if i := strings.LastIndexByte(arg, '@'); i > 0 {
		return arg[:i]
	}
	return arg
}

// chaveDeEndpoint normaliza uma URL para a comparação.
//
// Minúsculas e sem barra final: as duas origens escrevem o mesmo endereço com
// diferenças que não mudam para onde ele aponta, e tratá-las como servidores
// diferentes duplicaria o cartão na tela.
func chaveDeEndpoint(u string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u), "/"))
}
