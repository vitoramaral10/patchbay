package biblioteca

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// IntervaloDeSincronizacao é de quanto em quanto tempo o catálogo local é
// refeito.
//
// Doze horas porque a origem é um acervo de terceiro que cresce aos poucos:
// servidor novo aparecendo meio dia depois é aceitável, e uma varredura custa
// cerca de 30 minutos contra um site que não é nosso — ~651 páginas de detalhe
// a dois segundos uma da outra, que é a pausa que a origem exige (ver
// esperaEntreCuradas), mais as 22 páginas de índice (medido em 2026-09-11).
// Quem precisa de agora tem o botão de atualizar na tela.
const IntervaloDeSincronizacao = 12 * time.Hour

// PrazoDaVarredura é quanto tempo uma varredura inteira pode levar.
//
// Existe porque a paciência por página é grande de propósito (45s), e ~651
// páginas de paciência somariam horas se a origem entrasse num dia ruim. Uma
// hora cobre o acervo inteiro com folga para as tentativas: as páginas de
// detalhe vão a dois segundos uma da outra porque a origem limita taxa, o que
// deu 29m26s na medição de 2026-09-11. E ainda assim é um fim: passou disso, a
// varredura vira falha registrada, o catálogo anterior continua servindo, e a
// próxima tenta de novo.
const PrazoDaVarredura = time.Hour

// tentativasPorPagina é quantas vezes uma página de detalhe é pedida antes de a
// varredura desistir dela.
//
// Só em taxa excedida: a origem limita taxa (ver esperaEntreCuradas), e desistir
// no primeiro 429 jogaria fora um servidor por causa de um balde que ainda não
// encheu. O resto — página que sumiu, marcação que mudou — não melhora
// esperando.
const tentativasPorPagina = 3

// Sincronizador mantém a cópia local do catálogo.
//
// É a única coisa neste pacote que escreve. A tela só lê, e é justamente essa
// separação que faz a busca digitada não tocar a rede.
type Sincronizador struct {
	curadoria *Curadoria
	repo      *RepositorioSQLite
	log       *slog.Logger
	intervalo time.Duration
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
	curadoria *Curadoria, repo *RepositorioSQLite, log *slog.Logger,
	opcoes ...OpcaoSincronizador,
) *Sincronizador {
	s := &Sincronizador{
		curadoria:    curadoria,
		repo:         repo,
		log:          log,
		intervalo:    IntervaloDeSincronizacao,
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

	// Semeou agora, varre agora — sem consultar a idade. A semente pode ter sido
	// gerada dias antes, no corte da versão: deixá-la decidir pela idade faria a
	// instalação nova passar doze horas achando que o catálogo do release está
	// fresco, quando não está.
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
				"erro", err, "duracao", time.Since(inicio))
		}
		return
	}
	s.log.Info("biblioteca sincronizada", "duracao", time.Since(inicio))
}

// Varrer lê a origem e devolve o catálogo, sem gravar nada.
//
// Exportada para o subcomando que regenera a semente embutida. Não toca no
// repositório — é a única operação do sincronizador que não toca —, então um
// Sincronizador construído só para isto pode receber repo nulo.
func (s *Sincronizador) Varrer(ctx context.Context) ([]Item, error) {
	ctxVarredura, cancelar := context.WithTimeout(ctx, PrazoDaVarredura)
	defer cancelar()
	return s.varrer(ctxVarredura)
}

// varrer lê a lista /official do mcpservers.org: 651 servidores em 22 páginas
// de índice (medido em 2026-09-11), mais uma página de detalhe cada.
//
// Tolerante por natureza, e não por acidente: a maioria das páginas **não** tem
// comando aproveitável (numa amostra de 14, só 4 tinham), então página recusada
// é o caso comum e não pode contar como falha. O que derruba a varredura aqui é
// o índice não vir — sem ele não há o que ler, e devolver lista vazia como
// sucesso apagaria o catálogo inteiro.
func (s *Sincronizador) varrer(ctx context.Context) ([]Item, error) {
	slugs, err := s.curadoria.SlugsOficiais(ctx)
	if err != nil {
		return nil, fmt.Errorf("oficiais: %w", err)
	}
	itens := make([]Item, 0, len(slugs)/3)
	// O nome é chave primária no banco, então um nome repetido derrubaria a
	// varredura inteira com erro de constraint, longe da causa: ninguém o
	// encontraria depurando o INSERT.
	//
	// Hoje ele não filtra nada — SlugsOficiais já devolve slug único e o nome
	// sai do slug. É segunda linha de propósito, para quando a leitura do
	// índice ou a forma do nome mudarem; quem cobra o comportamento de ponta a
	// ponta é TestIndiceQueRepeteOServidorNaoDuplicaOItem.
	nomes := make(map[string]bool, len(slugs))
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
			if nomes[item.Nome] {
				continue
			}
			nomes[item.Nome] = true
			itens = append(itens, item)
			if item.Comando == "" {
				// O normal (D-03): a maioria não tem comando aproveitável, e
				// isso deixou de ser recusa — o item entra sem comando.
				semComando++
			}
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			// ErrFormatoDaOrigem só sobra aqui para página sem título — de
			// verdade fora do padrão, e não mais a falta de comando, que
			// agora entra no catálogo (D-03).
			indisponiveis++
		}
	}
	// Piso de 10%: um detalhe fora do ar é tolerado (D-03 é sobre a maioria não
	// ter comando, não sobre a origem cair), mas passado o piso o catálogo
	// resultante já não representa o acervo — melhor manter o anterior do que
	// gravar um recorte que só existe porque a origem estava com problema.
	// Aritmética inteira sem arredondar: 10% exatos não falha, só o que passa
	// disso.
	if total := len(slugs); total > 0 && indisponiveis*10 > total {
		return nil, fmt.Errorf("%w: %d de %d detalhes indisponíveis",
			ErrOrigemIndisponivel, indisponiveis, total)
	}
	s.log.Info("oficiais lidos",
		"aproveitados", len(itens), "sem_comando", semComando,
		"indisponiveis", indisponiveis, "total", len(slugs))
	return itens, nil
}

// oficialComTentativas insiste numa página de detalhe enquanto a origem estiver
// limitando taxa.
//
// Só em 429, e por isso ele é erro próprio: o resto — página que sumiu, formato
// que mudou — não melhora esperando, e repetir seria peso na origem sem chance
// de sucesso. A espera cresce a cada tentativa porque um 429 costuma significar
// que o balde de taxa vai demorar mais que a pausa normal para encher.
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
