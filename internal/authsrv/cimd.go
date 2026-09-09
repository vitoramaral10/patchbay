package authsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/sync/singleflight"
)

// TipoPrereg, TipoDCR e TipoCIMD são as três origens de registro possíveis de
// um cliente, e o valor da coluna oauth_client.tipo.
const (
	// TipoDCR é o cliente que se registrou sozinho por RFC 7591. Persiste: a
	// resposta do registro entregou um client_id que o cliente guardou.
	TipoDCR = "dcr"
	// TipoCIMD é o *cache* de um Client ID Metadata Document. Não é registro: a
	// linha existe só para não buscar o documento a cada requisição, tem TTL em
	// expira_em, e sumir dela não invalida nada do lado do cliente.
	TipoCIMD = "cimd_cache"
)

// Limites da busca do documento de CIMD.
//
// São todos apertados de propósito. Este é o único lugar do patchbay em que ele
// faz uma requisição de saída para uma URL escolhida por quem chama, e o
// processo que a faz é o mesmo que tem o banco e a chave mestra em mãos — é a
// superfície de ataque que a spec MCP criou ao trocar DCR por CIMD (seção 14 do
// estudo).
const (
	// ValidadeCIMD é o TTL do documento em cache.
	//
	// Uma hora: o documento é a allowlist de redirect de um cliente, então
	// cachear demais atrasa a revogação que o dono do cliente fizer no próprio
	// documento; cachear de menos transforma cada consentimento num fetch de
	// saída. Não é lido do Cache-Control da resposta de propósito — deixar o
	// alvo escolher por quanto tempo ele fica em cache dá a ele controle sobre
	// uma tabela do meu banco.
	ValidadeCIMD = time.Hour
	// TimeoutCIMD é o teto do fetch inteiro, resolução de DNS incluída.
	TimeoutCIMD = 5 * time.Second
	// TamanhoMaximoCIMD é o teto do corpo lido. Um documento legítimo tem
	// alguns KiB; o teto existe para que um alvo que responda um stream infinito
	// não coma a memória do processo.
	TamanhoMaximoCIMD = 64 << 10
	// redirectsMaximosCIMD é zero por escrito: seguir redirect é o jeito
	// clássico de furar um guarda de SSRF, porque a URL validada e a URL
	// buscada deixam de ser a mesma.
	redirectsMaximosCIMD = 0
	// janelaCacheNegativoCIMD é quanto tempo uma falha de busca fica
	// registrada, por identificador. Sem ela, um client_id cujo domínio está
	// fora do ar renova a tentativa a cada autorização que chegar — na mesma
	// velocidade de quem está tentando o fluxo de verdade.
	janelaCacheNegativoCIMD = 30 * time.Second
	// TetoCacheCIMDPorHora é quantos documentos de CIMD *novos* (identificador
	// nunca visto) este AS busca e cacheia por hora. Mesma ordem do teto de
	// DCR: sem ele, quem escolhe o client_id — que em CIMD é uma URL — força o
	// processo a fazer uma requisição de saída nova por identificador
	// diferente, sem limite.
	TetoCacheCIMDPorHora = 40
)

// Erros sentinela da busca de CIMD.
var (
	// ErrDestinoBloqueado indica endereço que o guarda de SSRF não deixa
	// alcançar.
	ErrDestinoBloqueado = errors.New("authsrv: destino bloqueado pelo guarda de saída")
	// ErrDocumentoCIMD indica documento ausente, malformado ou que não fecha
	// com a URL que o identifica.
	ErrDocumentoCIMD = errors.New("authsrv: documento de client id inválido")
	// ErrRedirectRecusado indica que o alvo respondeu com redirecionamento.
	ErrRedirectRecusado = errors.New("authsrv: o documento de client id não pode redirecionar")
	// ErrCorpoExcedido indica corpo de resposta maior que TamanhoMaximoCIMD.
	ErrCorpoExcedido = errors.New("authsrv: documento de client id passou do tamanho máximo")
)

// DocumentoCIMD é o Client ID Metadata Document: os metadados de cliente do
// RFC 7591 publicados pelo próprio cliente numa URL https, mais o client_id que
// tem de ser igual a essa URL.
//
// Os metadados vêm da struct do go-sdk e não de uma cópia local: são os mesmos
// campos do corpo de DCR, e reusar a struct é o que garante que o nome de campo
// que eu leio é o nome de campo que o cliente escreve.
type DocumentoCIMD struct {
	oauthex.ClientRegistrationMetadata
	// ClientID precisa ser exatamente a URL de onde o documento foi buscado.
	// É a amarração que impede publicar, numa URL sua, um documento que se
	// apresenta como cliente de outro.
	ClientID string `json:"client_id"`
}

// DocumentosCIMD é o que o Servico precisa da busca de CIMD, declarado aqui no
// consumidor: uma busca, com os guard-rails já aplicados.
//
// É interface e não o struct concreto por dois motivos: o teste ponta a ponta
// serve o documento de um httptest em processo, e é aqui que a fatia 11 se
// desliga inteira — Servico com este campo nil não anuncia CIMD na metadata e
// não faz requisição de saída nenhuma.
type DocumentosCIMD interface {
	// Buscar devolve o documento publicado em identificador.
	Buscar(ctx context.Context, identificador string) (DocumentoCIMD, error)
}

// BuscadorCIMD busca documentos de CIMD com os guard-rails da seção 14 do
// estudo: só https, sem seguir redirect, faixas privadas bloqueadas antes e
// depois da resolução de DNS, Content-Type exigido, corpo limitado, timeout
// curto e single-flight.
type BuscadorCIMD struct {
	cliente  *http.Client
	permitir func(netip.Addr) error
	log      *slog.Logger
	grupo    singleflight.Group
	agora    func() time.Time

	// base é o transporte de onde TLS e proxy são herdados. Só o teste o troca;
	// em produção fica nil e o buscador clona o transporte padrão.
	base *http.Transport

	// muFalhas e falhas são o cache negativo: a última falha de busca de cada
	// identificador, por janelaCacheNegativoCIMD. Ao lado do singleflight e não
	// dentro dele, porque o singleflight só junta requisições concorrentes — a
	// falha registrada aqui é o que evita a próxima requisição, que chega
	// depois que a primeira já terminou.
	muFalhas sync.Mutex
	falhas   map[string]falhaCacheCIMD
}

// falhaCacheCIMD é uma entrada do cache negativo.
type falhaCacheCIMD struct {
	erro  error
	ateEm time.Time
}

// Garante que o buscador satisfaz o contrato que o Servico consome.
var _ DocumentosCIMD = (*BuscadorCIMD)(nil)

// OpcaoCIMD ajusta o buscador na construção.
type OpcaoCIMD func(*BuscadorCIMD)

// ComTransporteCIMD troca o transporte de onde o buscador herda TLS e proxy.
//
// Existe pelo teste: um httptest.NewTLSServer emite certificado próprio, e sem
// o pool de raízes dele nenhum documento servido em processo seria buscável. O
// buscador continua instalando o próprio DialContext por cima do que vier
// daqui, então o guarda de destino e o teto de redirect valem igual.
func ComTransporteCIMD(base *http.Transport) OpcaoCIMD {
	return func(b *BuscadorCIMD) { b.base = base }
}

// ComDestinoCIMD troca a regra de "este endereço pode ser alcançado".
//
// É a única forma de relaxar o guarda de SSRF, e existe pelo teste: o httptest
// escuta em 127.0.0.1, que é exatamente o que destinoPublico bloqueia. Nenhum
// caminho de produção chama esta opção — quem monta o grafo (cmd/patchbay) usa
// NovoBuscadorCIMD sem nenhuma opção.
func ComDestinoCIMD(permitir func(netip.Addr) error) OpcaoCIMD {
	return func(b *BuscadorCIMD) { b.permitir = permitir }
}

// ComRelogioCIMD troca a fonte de tempo do cache negativo. Existe pelo teste:
// esperar 30 segundos de verdade para provar que a janela vence seria um teste
// de 30 segundos.
func ComRelogioCIMD(agora func() time.Time) OpcaoCIMD {
	return func(b *BuscadorCIMD) { b.agora = agora }
}

// NovoBuscadorCIMD monta o buscador com o guarda de saída ligado.
func NovoBuscadorCIMD(log *slog.Logger, opcoes ...OpcaoCIMD) *BuscadorCIMD {
	b := &BuscadorCIMD{
		log:      log,
		permitir: destinoPublico,
		agora:    time.Now,
		falhas:   make(map[string]falhaCacheCIMD),
	}
	for _, o := range opcoes {
		o(b)
	}

	base := b.base
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // é *http.Transport na stdlib
	}
	transporte := base.Clone()
	// Sem proxy: um proxy configurado no ambiente receberia a URL inteira e
	// faria a conexão por mim, e o Control abaixo passaria a conferir o
	// endereço do proxy em vez do endereço do alvo — o guarda viraria enfeite.
	transporte.Proxy = nil
	discador := &net.Dialer{Timeout: TimeoutCIMD, Control: b.conferirEndereco}
	transporte.DialContext = discador.DialContext

	b.cliente = &http.Client{
		Transport: transporte,
		CheckRedirect: func(_ *http.Request, anteriores []*http.Request) error {
			if len(anteriores) > redirectsMaximosCIMD {
				return ErrRedirectRecusado
			}
			return nil
		},
		Timeout: TimeoutCIMD,
	}
	return b
}

// conferirEndereco é o hook de net.Dialer.Control: roda com o endereço já
// resolvido, imediatamente antes de conectar.
//
// É este hook, e não uma consulta de DNS feita antes, que fecha o DNS
// rebinding: entre resolver e conectar não sobra janela nenhuma para o nome
// passar a apontar para 169.254.169.254.
func (b *BuscadorCIMD) conferirEndereco(_, endereco string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(endereco)
	if err != nil {
		return fmt.Errorf("%w: endereço %q ilegível", ErrDestinoBloqueado, endereco)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q não é um endereço IP", ErrDestinoBloqueado, host)
	}
	return b.permitir(ip.Unmap())
}

// conferirLiteral é o "antes da resolução de DNS": quando o host do
// identificador já é um endereço IP, a recusa acontece sem abrir socket nenhum.
//
// Não fica em ValidarURLCIMD porque a regra de destino é do buscador — é ela que
// o teste substitui —, e uma função de pacote não teria como consultá-la.
func (b *BuscadorCIMD) conferirLiteral(identificador string) error {
	u, err := url.Parse(identificador)
	if err != nil {
		return fmt.Errorf("%w: %q não é uma URL", ErrDocumentoCIMD, identificador)
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		// Não é literal: quem confere é o Control do discador, depois do DNS.
		return nil
	}
	return b.permitir(ip.Unmap())
}

// Buscar traz o documento de CIMD publicado em identificador.
//
// O single-flight é por identificador: um documento pedido por dez requisições
// ao mesmo tempo — o que acontece quando o cache acabou de vencer — rende uma
// requisição de saída, não dez.
func (b *BuscadorCIMD) Buscar(ctx context.Context, identificador string) (DocumentoCIMD, error) {
	if err := ValidarURLCIMD(identificador); err != nil {
		return DocumentoCIMD{}, err
	}
	if err := b.conferirLiteral(identificador); err != nil {
		return DocumentoCIMD{}, err
	}
	if err, negativado := b.erroCacheado(identificador); negativado {
		return DocumentoCIMD{}, err
	}

	// O ctx do chamador sai do caminho: a busca é preenchimento de cache
	// compartilhado, e amarrá-la a quem chegou primeiro faria o cancelamento
	// dele virar erro para todos os outros que esperavam no single-flight.
	ctxBusca, cancelar := context.WithTimeout(context.WithoutCancel(ctx), TimeoutCIMD)
	defer cancelar()

	bruto, err, _ := b.grupo.Do(identificador, func() (any, error) {
		doc, err := b.buscar(ctxBusca, identificador)
		if err != nil {
			b.registrarFalha(identificador, err)
			return nil, err
		}
		b.limparFalha(identificador)
		return doc, nil
	})
	if err != nil {
		return DocumentoCIMD{}, err
	}
	doc, ok := bruto.(DocumentoCIMD)
	if !ok {
		return DocumentoCIMD{}, fmt.Errorf("%w: resultado de tipo inesperado", ErrDocumentoCIMD)
	}
	return doc, nil
}

// erroCacheado devolve o erro da última falha de busca de identificador,
// enquanto ela ainda estiver dentro de janelaCacheNegativoCIMD.
func (b *BuscadorCIMD) erroCacheado(identificador string) (error, bool) {
	b.muFalhas.Lock()
	defer b.muFalhas.Unlock()

	f, ok := b.falhas[identificador]
	if !ok || !b.agora().Before(f.ateEm) {
		return nil, false
	}
	return f.erro, true
}

// registrarFalha guarda o erro de uma busca malsucedida, por
// janelaCacheNegativoCIMD.
func (b *BuscadorCIMD) registrarFalha(identificador string, err error) {
	b.muFalhas.Lock()
	defer b.muFalhas.Unlock()

	if b.falhas == nil {
		b.falhas = make(map[string]falhaCacheCIMD)
	}
	b.falhas[identificador] = falhaCacheCIMD{erro: err, ateEm: b.agora().Add(janelaCacheNegativoCIMD)}
}

// limparFalha apaga o cache negativo de identificador depois de uma busca bem
// sucedida: um documento voltando a responder não deve continuar recusado até
// a janela vencer sozinha.
func (b *BuscadorCIMD) limparFalha(identificador string) {
	b.muFalhas.Lock()
	defer b.muFalhas.Unlock()
	delete(b.falhas, identificador)
}

func (b *BuscadorCIMD) buscar(ctx context.Context, identificador string) (DocumentoCIMD, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, identificador, nil)
	if err != nil {
		return DocumentoCIMD{}, fmt.Errorf("%w: montar requisição: %w", ErrDocumentoCIMD, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "patchbay-authsrv")

	res, err := b.cliente.Do(req)
	if err != nil {
		return DocumentoCIMD{}, fmt.Errorf("%w: buscar %s: %w", ErrDocumentoCIMD, identificador, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return DocumentoCIMD{}, fmt.Errorf("%w: %s respondeu %d", ErrDocumentoCIMD, identificador, res.StatusCode)
	}
	if err := conferirJSON(res.Header.Get("Content-Type")); err != nil {
		return DocumentoCIMD{}, fmt.Errorf("%w: %s: %w", ErrDocumentoCIMD, identificador, err)
	}

	// Um byte além do teto: é o que permite distinguir "documento do tamanho
	// exato do limite" de "documento truncado", em vez de decodificar um JSON
	// cortado e reclamar da sintaxe.
	corpo, err := io.ReadAll(io.LimitReader(res.Body, TamanhoMaximoCIMD+1))
	if err != nil {
		return DocumentoCIMD{}, fmt.Errorf("%w: ler %s: %w", ErrDocumentoCIMD, identificador, err)
	}
	if len(corpo) > TamanhoMaximoCIMD {
		return DocumentoCIMD{}, fmt.Errorf("%w: %w: %s passou de %d bytes",
			ErrDocumentoCIMD, ErrCorpoExcedido, identificador, TamanhoMaximoCIMD)
	}

	var doc DocumentoCIMD
	if err := json.Unmarshal(corpo, &doc); err != nil {
		return DocumentoCIMD{}, fmt.Errorf("%w: %s não é JSON: %w", ErrDocumentoCIMD, identificador, err)
	}
	if err := doc.Validar(identificador); err != nil {
		return DocumentoCIMD{}, err
	}
	b.log.Info("documento de client id buscado", "client_id", identificador,
		"redirect_uris", len(doc.RedirectURIs))
	return doc, nil
}

// Validar confere o documento contra a URL de onde ele veio.
func (d DocumentoCIMD) Validar(identificador string) error {
	if d.ClientID != identificador {
		// A comparação é exata. Normalizar aqui — tolerar barra final, aceitar
		// caixa diferente no caminho — abriria justamente a folga que permite
		// publicar um documento se apresentando como outro cliente.
		return fmt.Errorf("%w: o client_id do documento (%q) não é a URL de onde ele veio (%q)",
			ErrDocumentoCIMD, d.ClientID, identificador)
	}
	if len(d.RedirectURIs) == 0 {
		return fmt.Errorf("%w: sem redirect_uris", ErrDocumentoCIMD)
	}
	if len(d.RedirectURIs) > MaximoRedirectsPorCliente {
		return fmt.Errorf("%w: %d redirect_uris passa do teto de %d",
			ErrDocumentoCIMD, len(d.RedirectURIs), MaximoRedirectsPorCliente)
	}
	for _, uri := range d.RedirectURIs {
		if motivo := motivoRedirectInvalido(uri); motivo != "" {
			return fmt.Errorf("%w: redirect_uri %q: %s", ErrDocumentoCIMD, uri, motivo)
		}
	}
	if err := conferirGrants(d.GrantTypes, d.ResponseTypes); err != nil {
		return fmt.Errorf("%w: %w", ErrDocumentoCIMD, err)
	}
	return nil
}

// Nome é como o documento se apresenta na tela de consentimento.
//
// O client_name vem de quem publicou o documento e não é verificado por
// ninguém: é rótulo, e é por isso que a tela mostra o hostname do client_id ao
// lado dele — o hostname é a única parte que o dono do domínio controla.
func (d DocumentoCIMD) Nome() string {
	if nome := strings.TrimSpace(d.ClientName); nome != "" {
		return recortar(nome, tamanhoMaximoNome)
	}
	u, err := url.Parse(d.ClientID)
	if err != nil || u.Host == "" {
		return d.ClientID
	}
	return u.Host
}

// ehIdentificadorCIMD informa se um client_id é uma URL de CIMD em vez de um
// identificador emitido por este AS.
//
// O critério é o esquema: todo client_id que este AS emite começa com "pbc_",
// e a spec exige que o identificador de CIMD seja uma URL https. Não há como
// um confundir-se com o outro.
func ehIdentificadorCIMD(clientID string) bool {
	return strings.HasPrefix(clientID, "https://")
}

// ValidarURLCIMD confere a *forma* do identificador antes de qualquer
// requisição. A regra de destino — que faixas de IP podem ser alcançadas — é do
// buscador, em conferirLiteral e no Control do discador.
func ValidarURLCIMD(identificador string) error {
	u, err := url.Parse(identificador)
	if err != nil {
		return fmt.Errorf("%w: %q não é uma URL", ErrDocumentoCIMD, identificador)
	}
	switch {
	case u.Scheme != "https":
		// Só https, sem exceção nem para loopback: o documento é a allowlist de
		// redirect de um cliente, e buscá-lo em claro deixaria qualquer um no
		// caminho reescrever para onde o código de autorização vai.
		return fmt.Errorf("%w: %q não é https", ErrDocumentoCIMD, identificador)
	case u.Host == "":
		return fmt.Errorf("%w: %q não tem host", ErrDocumentoCIMD, identificador)
	case u.User != nil:
		return fmt.Errorf("%w: %q tem userinfo", ErrDocumentoCIMD, identificador)
	case u.Fragment != "" || strings.Contains(identificador, "#"):
		return fmt.Errorf("%w: %q tem fragmento", ErrDocumentoCIMD, identificador)
	case u.Path == "" || u.Path == "/":
		// A mesma exigência que o cliente do go-sdk faz do lado dele
		// (auth/authorization_code.go:188): identificador de CIMD é URL https
		// com caminho, nunca a raiz de um domínio.
		return fmt.Errorf("%w: %q não tem caminho", ErrDocumentoCIMD, identificador)
	}
	return nil
}

// faixasBloqueadas são as sub-redes que netip.Addr não classifica sozinho e que
// não podem ser alvo de uma requisição de saída deste processo.
//
// Não há entrada para IPv4 mapeado em IPv6 (::ffff:0:0/96): o Unmap já roda em
// conferirEndereco e em conferirLiteral antes de qualquer verificação, então um
// endereço nessa forma já chega aqui como IPv4 puro — uma entrada aqui para ele
// seria morta, nunca alcançada.
var faixasBloqueadas = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // atribuição de protocolo IETF
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmark (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reservado
	netip.MustParsePrefix("2001:db8::/32"),   // documentação
	netip.MustParsePrefix("0.0.0.0/8"),       // "esta rede" (RFC 791 §3.2)
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 bem-conhecido — atravessa para IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 de tradutor local — idem
	// Os três abaixo são mecanismos de transição que encapsulam ou carregam um
	// endereço IPv4 dentro de um endereço IPv6: sem eles, o mesmo desvio que
	// ::ffff:x.x.x.x faria antes do Unmap continuaria aberto, só que por uma
	// forma que o Unmap não desfaz.
	netip.MustParsePrefix("::/96"),     // IPv4-compatible (RFC 4291, obsoleto)
	netip.MustParsePrefix("2002::/16"), // 6to4
	netip.MustParsePrefix("2001::/32"), // Teredo
}

// destinoPublico é a regra de produção: só endereço unicast global.
//
// A lista é de recusa e não de permissão porque a internet pública é o caso
// normal aqui — o documento de CIMD de um cliente de verdade está num domínio
// público. O que precisa ser enumerado é o que fica *dentro*: 169.254.169.254 e
// o resto do link-local, as faixas privadas, o loopback e o unique-local IPv6.
func destinoPublico(ip netip.Addr) error {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid():
		return fmt.Errorf("%w: endereço inválido", ErrDestinoBloqueado)
	case ip.IsLoopback():
		return fmt.Errorf("%w: %s é loopback", ErrDestinoBloqueado, ip)
	case ip.IsUnspecified():
		return fmt.Errorf("%w: %s não é endereço de destino", ErrDestinoBloqueado, ip)
	case ip.IsPrivate():
		// Cobre RFC 1918 no IPv4 e fc00::/7 (unique local) no IPv6.
		return fmt.Errorf("%w: %s é de faixa privada", ErrDestinoBloqueado, ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 e fe80::/10 — é aqui que mora o serviço de metadata de
		// nuvem, que é o alvo clássico de SSRF.
		return fmt.Errorf("%w: %s é link-local", ErrDestinoBloqueado, ip)
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return fmt.Errorf("%w: %s é multicast", ErrDestinoBloqueado, ip)
	}
	for _, faixa := range faixasBloqueadas {
		if faixa.Contains(ip) {
			return fmt.Errorf("%w: %s está em %s", ErrDestinoBloqueado, ip, faixa)
		}
	}
	return nil
}

// conferirJSON exige que a resposta se declare JSON.
//
// Exigir o Content-Type é guard-rail de SSRF e não preciosismo de protocolo: um
// alvo interno que responda HTML, texto ou um protocolo binário para um GET
// deixa de ser confundível com um documento de CIMD, e o erro sai antes de o
// corpo ser interpretado.
func conferirJSON(bruto string) error {
	if bruto == "" {
		return errors.New("resposta sem Content-Type")
	}
	tipo, _, err := mime.ParseMediaType(bruto)
	if err != nil {
		return fmt.Errorf("Content-Type malformado: %w", err)
	}
	if tipo != "application/json" && !strings.HasSuffix(tipo, "+json") {
		return fmt.Errorf("Content-Type %s não é JSON", tipo)
	}
	return nil
}
