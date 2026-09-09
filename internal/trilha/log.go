package trilha

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// HandlerLog é o slog.Handler que redige segredo e replica a linha para o hub
// do log ao vivo.
//
// Ele envolve o handler de verdade e redige *antes* de delegar: a redação vale
// para o arquivo, para o stderr e para a tela pelo mesmo caminho. Redigir só na
// tela deixaria o vazamento no destino que ninguém revisa — e a tela de log é a
// via mais fácil de vazar exatamente o que a cifra em repouso protege
// (seção 11).
//
// O limite conhecido: a redação por conteúdo (trilha.Redigir) reconhece
// formato — JWT, marca de provedor conhecida (ghp_, sk-ant-, AKIA...),
// parâmetro de URL, esquema de autorização — e a redação por chave
// (ChaveSensivel) reconhece nome de atributo. Um header estático de upstream é
// texto livre: se o valor não casa com nenhum formato conhecido e a chave que
// o carrega não é óbvia, ele atravessa. Isto não promete redigir todo segredo
// possível — promete redigir os que têm forma reconhecível ou nome sensato, que
// é a maioria do que escapa de um `log.Printf` ou de uma mensagem de erro de
// upstream.
type HandlerLog struct {
	base slog.Handler
	hub  *Hub

	// grupos são os grupos abertos por WithGroup, para prefixar a chave na
	// linha que vai para a tela. O handler de baixo já faz isso do lado dele.
	grupos []string
	// fixos são os atributos acumulados por WithAttrs, já redigidos e já com o
	// prefixo de grupo aplicado.
	fixos []Atributo
}

// NovoHandlerLog envolve base. Com hub nil ele continua redigindo — o que se
// perde é só o log ao vivo.
func NovoHandlerLog(base slog.Handler, hub *Hub) *HandlerLog {
	return &HandlerLog{base: base, hub: hub}
}

// Enabled delega: quem decide o nível é o handler de baixo, com a opção que main
// montou.
func (h *HandlerLog) Enabled(ctx context.Context, nivel slog.Level) bool {
	return h.base.Enabled(ctx, nivel)
}

// Handle redige a mensagem e os atributos, entrega ao handler de baixo e publica
// a linha no hub.
func (h *HandlerLog) Handle(ctx context.Context, r slog.Record) error {
	mensagem, _ := Redigir(r.Message)

	nova := slog.NewRecord(r.Time, r.Level, mensagem, r.PC)
	prefixo := strings.Join(h.grupos, ".")
	daChamada := make([]Atributo, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		redigido := redigirAttr(a, false)
		nova.AddAttrs(redigido)
		daChamada = achatar(prefixo, redigido, daChamada)
		return true
	})

	err := h.base.Handle(ctx, nova)

	if h.hub != nil {
		atributos := make([]Atributo, 0, len(h.fixos)+len(daChamada))
		atributos = append(atributos, h.fixos...)
		atributos = append(atributos, daChamada...)
		h.hub.Publicar(Mensagem{Tipo: TipoLog, Log: LinhaLog{
			Instante:  r.Time,
			Nivel:     r.Level.String(),
			Mensagem:  mensagem,
			Atributos: atributos,
		}})
	}
	return err
}

// WithAttrs devolve um handler novo com os atributos já redigidos.
func (h *HandlerLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	redigidos := make([]slog.Attr, 0, len(attrs))
	fixos := slices.Clone(h.fixos)
	prefixo := strings.Join(h.grupos, ".")
	for _, a := range attrs {
		red := redigirAttr(a, false)
		redigidos = append(redigidos, red)
		fixos = achatar(prefixo, red, fixos)
	}
	return &HandlerLog{
		base:   h.base.WithAttrs(redigidos),
		hub:    h.hub,
		grupos: slices.Clone(h.grupos),
		fixos:  fixos,
	}
}

// WithGroup abre um grupo. Nome vazio devolve o próprio handler, como o contrato
// do slog.Handler exige.
func (h *HandlerLog) WithGroup(nome string) slog.Handler {
	if nome == "" {
		return h
	}
	return &HandlerLog{
		base:   h.base.WithGroup(nome),
		hub:    h.hub,
		grupos: append(slices.Clone(h.grupos), nome),
		fixos:  slices.Clone(h.fixos),
	}
}

// redigirAttr redige um atributo, descendo em slog.Group.
//
// herdado propaga a sensibilidade para dentro do grupo: com
// slog.Group("token", "valor", x), a chave sensível é a de fora e o valor a
// esconder é o de dentro — sem a propagação, agrupar um segredo o desredigiria.
func redigirAttr(a slog.Attr, herdado bool) slog.Attr {
	sensivel := herdado || ChaveSensivel(a.Key)

	// Resolve antes de olhar: é aqui que um slog.LogValuer como cripto.Segredo
	// vira o texto que ele quer mostrar, e é esse texto que precisa passar pela
	// redação — não o struct.
	valor := a.Value.Resolve()

	if valor.Kind() == slog.KindGroup {
		grupo := valor.Group()
		dentro := make([]slog.Attr, 0, len(grupo))
		for _, g := range grupo {
			dentro = append(dentro, redigirAttr(g, sensivel))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(dentro...)}
	}

	texto := valor.String()
	if valor.Kind() == slog.KindAny {
		// "%+v" e não valor.String(): para KindAny, Value.String() delega em
		// fmt.Sprint, que não mostra nome de campo de struct (só de mapa). Sem
		// o nome do campo, um slog.Any("cfg", struct{ Token string }{...})
		// vira "{segredo}" sem "Token:" na frente, e reCampoSensivel (abaixo)
		// não tem chave nenhuma para casar.
		texto = fmt.Sprintf("%+v", valor.Any())
	}

	var (
		fora  string
		mudou bool
	)
	if sensivel {
		// RedigirSensivel e não RedigirPar: a sensibilidade pode ter vindo da
		// chave de um grupo acima, e RedigirPar a redecidiria pela chave deste
		// atributo — que num slog.Group("token", "valor", x) é só "valor".
		fora, mudou = RedigirSensivel(texto)
	} else {
		fora, mudou = Redigir(texto)
		if valor.Kind() == slog.KindAny {
			// Chave sensível *dentro* do valor — um campo de struct ou mapa
			// que o slog nunca viu como chave de atributo, só Redigir (que
			// olha conteúdo, não nome) já rodou acima.
			if campos, mudouCampos := RedigirCampos(fora); mudouCampos {
				fora, mudou = campos, true
			}
		}
	}
	if !mudou {
		// Nada a esconder: devolve o valor original, com o tipo original. Passar
		// tudo por String() jogaria fora o número e o booleano do JSON.
		return slog.Attr{Key: a.Key, Value: valor}
	}
	return slog.String(a.Key, fora)
}

// achatar acrescenta a out o atributo já redigido, com a chave prefixada pelos
// grupos, descendo em slog.Group.
func achatar(prefixo string, a slog.Attr, out []Atributo) []Atributo {
	chave := a.Key
	if prefixo != "" {
		chave = prefixo + "." + a.Key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			out = achatar(chave, g, out)
		}
		return out
	}
	return append(out, Atributo{Chave: chave, Valor: a.Value.String()})
}
