package catalogo

import (
	"errors"
	"fmt"
	"strings"
)

// Acao é o que uma regra faz com a ferramenta cujo nome original casa com o
// padrão.
type Acao string

// As três ações da composição fina.
const (
	// AcaoIncluir mantém a ferramenta no endpoint.
	AcaoIncluir Acao = "incluir"
	// AcaoExcluir tira a ferramenta do endpoint.
	AcaoExcluir Acao = "excluir"
	// AcaoRenomear troca o nome-base antes de o prefixo entrar.
	AcaoRenomear Acao = "renomear"
)

// AvisoRenomeada marca a ferramenta cujo nome-base veio de uma regra, e não do
// upstream. Aparece na UI porque um nome que não existe em lugar nenhum do
// upstream é indistinguível de bug quando ninguém avisa.
const AvisoRenomeada Aviso = "renomeada"

// MaxPrefixo é o teto de um prefixo de composição.
//
// O orçamento de nome é o do SDK (MaxNomeFerramenta): prefixo comprido come o
// nome da ferramenta no corte, e nome cortado é o que faz duas ferramentas
// diferentes virarem a mesma no cliente.
const MaxPrefixo = 32

// MaxRenome é o teto de tamanho de um nome novo escrito numa regra renomear.
//
// Teto próprio e não o do prefixo: o renome é o nome-base inteiro, não um
// pedaço curto colado na frente dele, e usar MaxPrefixo aqui recusaria nome de
// ferramenta legítimo.
const MaxRenome = 64

// MaxRegras é o teto de regras por vínculo endpoint↔upstream.
//
// Uma composição sem teto é uma tela sem teto: o textarea de regras vira
// ilegível bem antes de qualquer limite técnico, e 200 já é mais regra do que
// qualquer composição real deste gateway escreveu até aqui.
const MaxRegras = 200

// ErrRegraInvalida indica linha de regra que não dá para interpretar. A borda
// mostra o texto para o admin corrigir; o banco nunca recebe regra inválida.
var ErrRegraInvalida = errors.New("catalogo: regra inválida")

// ErroDeRegra é a linha que não deu para interpretar, com o número dela.
//
// Tipado e não só uma string embrulhada porque a tela precisa do número da linha
// e do motivo separados do prefixo do pacote: mensagem de erro de formulário é
// texto para uma pessoa, não trilha de propagação.
type ErroDeRegra struct {
	Linha   int
	Detalhe string
}

// Error implementa error.
func (e *ErroDeRegra) Error() string {
	return fmt.Sprintf("catalogo: regra inválida na linha %d: %s", e.Linha, e.Detalhe)
}

// Is faz errors.Is(err, ErrRegraInvalida) valer para este tipo.
func (e *ErroDeRegra) Is(alvo error) bool { return alvo == ErrRegraInvalida }

// Mensagem é o texto que vai para o campo do formulário.
func (e *ErroDeRegra) Mensagem() string {
	return fmt.Sprintf("Linha %d: %s", e.Linha, e.Detalhe)
}

// Regra é uma linha da composição fina de um endpoint com um upstream.
//
// O padrão casa contra o nome original da ferramenta no upstream — nunca contra
// o nome já prefixado. Filtrar pelo nome exposto faria o prefixo mudar o efeito
// do filtro, e mexer no prefixo passaria a apagar ferramenta em silêncio.
type Regra struct {
	Acao   Acao
	Padrao string
	// Renome é o nome-base novo, e só vale com AcaoRenomear. Um `*` no renome é
	// substituído pelo que o `*` de mesma posição capturou no padrão.
	Renome string
}

// Aplicar decide se a ferramenta entra no endpoint e com que nome-base.
//
// A ordem é fixa e é ela que torna a composição previsível:
//
//  1. filtro — percorre as regras na ordem e a primeira incluir/excluir que casa
//     decide. Ferramenta que não casa com nenhuma regra entra;
//  2. renome — percorre de novo e a primeira regra renomear que casa troca o
//     nome-base;
//  3. prefixo — aplicado depois, na normalização, ao nome-base já decidido.
//
// Duas passadas em vez de uma lista única de efeitos porque filtro e renome
// respondem perguntas diferentes: "esta ferramenta entra?" e "com que nome?". Um
// único laço faria a posição de uma regra de renome mudar quem entra.
//
// O padrão é sempre incluir, e a lista de permissão se escreve fechando o
// conjunto com `excluir *` no fim. A alternativa — "existe uma regra incluir,
// logo o padrão vira excluir" — é a que faz uma exceção escrita antes de um
// excluir amplo apagar em silêncio todo o resto do catálogo. Aqui a regra que
// apaga tudo é uma linha que a pessoa escreveu e vê.
func Aplicar(regras []Regra, nomeOriginal string) (base string, entra bool, renomeou bool) {
	entra = true
	for _, r := range regras {
		if r.Acao != AcaoIncluir && r.Acao != AcaoExcluir {
			continue
		}
		if _, ok := casar(r.Padrao, nomeOriginal); ok {
			entra = r.Acao == AcaoIncluir
			break
		}
	}
	if !entra {
		return "", false, false
	}

	for _, r := range regras {
		if r.Acao != AcaoRenomear {
			continue
		}
		capturas, ok := casar(r.Padrao, nomeOriginal)
		if !ok {
			continue
		}
		// Renome que resolve para vazio é ignorado: sobraria só o prefixo, e
		// duas ferramentas viram a mesma. Perder o renome é mais barato que
		// perder a identidade da ferramenta.
		if novo := aplicarRenome(r.Renome, capturas); novo != "" {
			return novo, true, novo != nomeOriginal
		}
		break
	}
	return nomeOriginal, true, false
}

// Casa informa se o padrão da regra casa com o nome original. É o que a tela
// usa para avisar de uma regra que não casou nenhuma ferramenta do catálogo
// vivo do upstream — Aplicar decide o resultado da composição inteira, e aqui
// o interesse é só numa regra isolada.
func (r Regra) Casa(nomeOriginal string) bool {
	_, ok := casar(r.Padrao, nomeOriginal)
	return ok
}

// casar aplica o glob do padrão ao nome e devolve o que cada `*` capturou.
//
// Só `*` é metacaractere, e ele casa qualquer sequência, inclusive vazia. Não é
// regex de propósito: o padrão é escrito num campo de formulário por quem
// administra o gateway, e regex ali é a diferença entre "excluí as de escrita" e
// uma hora de depuração.
func casar(padrao, nome string) ([]string, bool) {
	partes := strings.Split(padrao, "*")
	if len(partes) == 1 {
		return nil, padrao == nome
	}

	resto, ok := strings.CutPrefix(nome, partes[0])
	if !ok {
		return nil, false
	}
	capturas := make([]string, 0, len(partes)-1)
	for _, meio := range partes[1 : len(partes)-1] {
		i := strings.Index(resto, meio)
		if i < 0 {
			return nil, false
		}
		capturas = append(capturas, resto[:i])
		resto = resto[i+len(meio):]
	}
	fim := partes[len(partes)-1]
	if !strings.HasSuffix(resto, fim) {
		return nil, false
	}
	return append(capturas, resto[:len(resto)-len(fim)]), true
}

// aplicarRenome troca cada `*` do renome pela captura de mesma posição. `*` sem
// captura correspondente vira vazio.
func aplicarRenome(renome string, capturas []string) string {
	if !strings.Contains(renome, "*") {
		return renome
	}
	var b strings.Builder
	usadas := 0
	for _, r := range renome {
		if r != '*' {
			b.WriteRune(r)
			continue
		}
		if usadas < len(capturas) {
			b.WriteString(capturas[usadas])
		}
		usadas++
	}
	return b.String()
}

// AnalisarRegras lê o texto do formulário e devolve as regras na ordem escrita.
//
// Uma regra por linha, campos separados por espaço: `<ação> <padrão> [renome]`.
// Linha vazia e linha começada por `#` são ignoradas. Texto e não uma tabela na
// tela porque a ordem das regras é o que decide o resultado, e reordenar linhas
// num textarea é a operação mais barata que existe — em HTML sem JavaScript,
// reordenar uma lista de campos não é.
func AnalisarRegras(texto string) ([]Regra, error) {
	var out []Regra
	for i, linha := range strings.Split(strings.ReplaceAll(texto, "\r\n", "\n"), "\n") {
		linha = strings.TrimSpace(linha)
		if linha == "" || strings.HasPrefix(linha, "#") {
			continue
		}
		r, detalhe := analisarLinha(linha)
		if detalhe != "" {
			return nil, &ErroDeRegra{Linha: i + 1, Detalhe: detalhe}
		}
		out = append(out, r)
	}
	return out, nil
}

// analisarLinha devolve a regra ou o motivo, em texto para uma pessoa, de a
// linha não valer.
func analisarLinha(linha string) (Regra, string) {
	campos := strings.Fields(linha)
	if len(campos) < 2 {
		return Regra{}, `escreva "<ação> <padrão>" — por exemplo: excluir write_*`
	}
	if len(campos) > 3 {
		return Regra{}, "sobrou texto depois do terceiro campo"
	}

	r := Regra{Acao: Acao(campos[0]), Padrao: campos[1]}
	switch r.Acao {
	case AcaoIncluir, AcaoExcluir:
		if len(campos) == 3 {
			return Regra{}, string(r.Acao) + " não leva um terceiro campo"
		}
	case AcaoRenomear:
		if len(campos) != 3 {
			return Regra{}, "renomear exige o nome novo no terceiro campo"
		}
		r.Renome = campos[2]
		if detalhe := renomeInvalido(r.Renome, r.Padrao); detalhe != "" {
			return Regra{}, detalhe
		}
	default:
		return Regra{}, fmt.Sprintf("ação %q desconhecida; use incluir, excluir ou renomear", campos[0])
	}
	return r, ""
}

// TextoDeRegras devolve a forma canônica das regras, uma por linha. É o que a
// tela reexibe e o que o export de YAML da fatia 13 vai escrever.
func TextoDeRegras(regras []Regra) string {
	var b strings.Builder
	for _, r := range regras {
		b.WriteString(string(r.Acao))
		b.WriteByte(' ')
		b.WriteString(r.Padrao)
		if r.Acao == AcaoRenomear {
			b.WriteByte(' ')
			b.WriteString(r.Renome)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renomeInvalido devolve o motivo, em texto para uma pessoa, de o nome novo de
// uma regra renomear não valer — ou "" se valer.
//
// O alfabeto é o de runeValido mais `*`: o renome vira nome exposto sem passar
// pelo saneamento do SDK antes da desambiguação (Materializar aplica o renome
// antes do saneamento), e nome exposto é contrato — saná-lo em silêncio faria o
// admin achar que configurou um nome que não é o que o cliente vê.
//
// O teto de estrelas evita a captura sem origem: `renomear * ///` tem zero
// grupos no padrão e um `*` sobrando no renome sem captura correspondente
// viraria string vazia (aplicarRenome), esvaziando o nome de toda ferramenta
// que casar com o padrão.
func renomeInvalido(renome, padrao string) string {
	if len(renome) > MaxRenome {
		return fmt.Sprintf("o nome novo passa de %d caracteres", MaxRenome)
	}
	for _, r := range renome {
		if r != '*' && !runeValido(r) {
			return fmt.Sprintf("o nome novo só aceita letras, números, %q, %q, %q e %q", "_", "-", ".", "*")
		}
	}
	if strings.Count(renome, "*") > strings.Count(padrao, "*") {
		return "o nome novo tem mais * do que o padrão consegue capturar"
	}
	return ""
}

// PrefixoValido informa se o prefixo sobrevive à normalização de nome sem ser
// alterado. Prefixo saneado em silêncio faria o nome exposto — que é contrato —
// não ser o que o admin digitou.
func PrefixoValido(prefixo string) bool {
	if len(prefixo) > MaxPrefixo {
		return false
	}
	for _, r := range prefixo {
		if !runeValido(r) {
			return false
		}
	}
	return true
}
