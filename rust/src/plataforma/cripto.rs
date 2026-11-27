//! A cifra de campo do patchbay: AES-256-GCM com chave derivada por HKDF da chave mestra.
//! Porte de `internal/platform/cripto`, bit a bit: o que o Go cifrou o Rust decifra, e o contrário.
//!
//! São duas classes de segredo (seção 08.8 do estudo prévio): o que o patchbay *apresenta* (bearer
//! de upstream, header, client secret, token OAuth) volta em claro e vai aqui; o que ele *verifica*
//! (chave de API, sessão, tokens que ele emitiu) vai em hash e não passa por este módulo.
//!
//! Formato guardado: `pbc1:` + base64url sem padding de (nonce12 ‖ texto cifrado ‖ tag16). O AAD
//! é, para cada parte de `["pbc1", tabela, coluna, id]`, o tamanho em uvarint seguido dos bytes:
//! é o que impede transplantar um valor de uma linha (ou coluna) para outra.

use std::fmt;

use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use base64::Engine;
use base64::engine::general_purpose::{GeneralPurpose, GeneralPurposeConfig, URL_SAFE_NO_PAD};
use base64::engine::{DecodePaddingMode, general_purpose};
use zeroize::{Zeroize, Zeroizing};

/// A única origem da chave mestra no ambiente.
pub const VAR_CHAVE_MESTRA: &str = "PATCHBAY_MASTER_KEY";

/// Tamanho exigido da chave mestra depois de decodificada: o material que o HKDF expande para a
/// chave de AES-256 (entrada menor que a saída não acrescenta entropia).
pub const BYTES_CHAVE_MESTRA: usize = 32;

/// O que aparece no lugar de qualquer segredo formatado ou logado.
pub const REDIGIDO: &str = "«redigido»";

const PREFIXO_V1: &str = "pbc1:";
const SAL_HKDF: &[u8] = b"patchbay:cripto:v1";
const INFO_CIFRA_V1: &[u8] = b"patchbay:cifra-de-campo:aes-256-gcm:v1";
const ROTULO_AAD_V1: &str = "pbc1";
const BYTES_NONCE: usize = 12;
const BYTES_TAG: usize = 16;

/// Os erros do módulo, com o texto dos sentinelas do Go. Nenhum carrega o valor guardado nem a
/// chave: erro de decifra vai para o log, e o log é a via mais fácil de vazar o que a cifra protege.
#[derive(Debug)]
pub enum Erro {
    /// `PATCHBAY_MASTER_KEY` vazia ou não definida.
    ChaveMestraAusente,
    /// Chave presente que não é base64 de 32 bytes; o texto diz por quê, nunca o conteúdo.
    ChaveMestraInvalida(String),
    /// Valor guardado que não tem a forma de um valor desta implementação.
    Formato(&'static str),
    /// Valor que não autentica: chave outra, valor transplantado ou adulterado (o GCM não distingue).
    Autenticacao,
    /// Pedido de cifra sem o contexto da linha.
    CampoIncompleto(String),
    /// O canário gravado não volta com a chave em uso. `None` na causa: decifrou, mas não é o texto.
    CanarioNaoConfere(Option<Box<Erro>>),
    /// Falha ao ler ou gravar o canário no armazenamento.
    Armazenamento(&'static str, anyhow::Error),
    /// O gerador aleatório do sistema falhou.
    Sorteio(&'static str, String),
}

impl fmt::Display for Erro {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Erro::ChaveMestraAusente => write!(f, "cripto: {VAR_CHAVE_MESTRA} não definida"),
            Erro::ChaveMestraInvalida(porque) => write!(f, "cripto: chave mestra inválida: {porque}"),
            Erro::Formato(porque) => write!(f, "cripto: valor cifrado com formato inválido: {porque}"),
            Erro::Autenticacao => f.write_str("cripto: valor cifrado não autentica"),
            Erro::CampoIncompleto(campo) => write!(f, "cripto: campo sem tabela, coluna ou id: {campo}"),
            Erro::CanarioNaoConfere(causa) => {
                f.write_str("cripto: o canário não confere com a chave mestra em uso: ")?;
                match causa {
                    Some(c) => write!(f, "{c}"),
                    None => f.write_str("valor decifrado inesperado"),
                }
            }
            Erro::Armazenamento(o_que, causa) => write!(f, "cripto: {o_que}: {causa:#}"),
            Erro::Sorteio(o_que, causa) => write!(f, "cripto: {o_que}: {causa}"),
        }
    }
}

impl std::error::Error for Erro {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Erro::CanarioNaoConfere(Some(c)) => Some(c.as_ref()),
            Erro::Armazenamento(_, c) => Some(c.as_ref()),
            _ => None,
        }
    }
}

/// Um valor em claro que nunca chega ao log nem a uma mensagem de erro: `Debug` e `Display`
/// imprimem [`REDIGIDO`], e ler o valor exige dizer [`Segredo::revelar`] (grep-ável numa revisão).
///
/// São bytes, como a `string` do Go: o que o Go cifrou (um segredo vindo de variável de ambiente
/// pode não ser UTF-8) volta igual. Quem precisa de texto (token OAuth) converte e decide o que
/// fazer com o que não for; header e variável de ambiente aceitam bytes.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct Segredo(Vec<u8>);

impl Segredo {
    pub fn new(valor: impl Into<Vec<u8>>) -> Self {
        Segredo(valor.into())
    }

    /// O valor em claro. É a única forma de sair do tipo.
    pub fn revelar(&self) -> &[u8] {
        &self.0
    }
}

impl Drop for Segredo {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

impl fmt::Debug for Segredo {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(REDIGIDO)
    }
}

impl fmt::Display for Segredo {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(REDIGIDO)
    }
}

/// O material de 32 bytes de onde toda chave de cifra é derivada; se redige como [`Segredo`].
pub struct ChaveMestra([u8; BYTES_CHAVE_MESTRA]);

impl Drop for ChaveMestra {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

impl fmt::Debug for ChaveMestra {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(REDIGIDO)
    }
}

impl fmt::Display for ChaveMestra {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(REDIGIDO)
    }
}

/// As quatro variantes que o Go aceita, na mesma ordem (Std, RawStd, URL, RawURL). Como o
/// `encoding/base64` do Go, aceitam bits de folga no último grupo.
fn codificacoes() -> [GeneralPurpose; 4] {
    let com =
        GeneralPurposeConfig::new().with_decode_allow_trailing_bits(true).with_decode_padding_mode(DecodePaddingMode::RequireCanonical);
    let sem = com.with_decode_padding_mode(DecodePaddingMode::RequireNone);
    [
        GeneralPurpose::new(&base64::alphabet::STANDARD, com),
        GeneralPurpose::new(&base64::alphabet::STANDARD, sem),
        GeneralPurpose::new(&base64::alphabet::URL_SAFE, com),
        GeneralPurpose::new(&base64::alphabet::URL_SAFE, sem),
    ]
}

impl ChaveMestra {
    /// Decodifica o texto de `PATCHBAY_MASTER_KEY`. As quatro variantes de base64 são aceitas
    /// porque a diferença é invisível para quem copiou a chave de um gerenciador de segredo.
    pub fn de(texto: &str) -> Result<ChaveMestra, Erro> {
        let texto = texto.trim();
        if texto.is_empty() {
            return Err(Erro::ChaveMestraAusente);
        }
        // O decodificador do Go ignora \r e \n no meio do texto (chave colada com quebra).
        let limpo: Zeroizing<String> = Zeroizing::new(texto.chars().filter(|c| !matches!(c, '\r' | '\n')).collect());
        let Some(mut bruta) = codificacoes().iter().find_map(|c| c.decode(&limpo).ok()) else {
            return Err(Erro::ChaveMestraInvalida("não é base64".into()));
        };
        let tamanho = bruta.len();
        let chave = <[u8; BYTES_CHAVE_MESTRA]>::try_from(bruta.as_slice()).ok().map(ChaveMestra);
        bruta.zeroize();
        // O tamanho entra na mensagem; o conteúdo, nunca.
        chave.ok_or_else(|| Erro::ChaveMestraInvalida(format!("{tamanho} bytes decodificados, quer {BYTES_CHAVE_MESTRA}")))
    }
}

/// Sorteia uma chave mestra nova e devolve o texto que vai em `PATCHBAY_MASTER_KEY`.
pub fn gerar_chave_mestra() -> Result<Zeroizing<String>, Erro> {
    let mut bruta = Zeroizing::new([0u8; BYTES_CHAVE_MESTRA]);
    getrandom::fill(bruta.as_mut()).map_err(|e| Erro::Sorteio("sortear chave mestra", e.to_string()))?;
    Ok(Zeroizing::new(general_purpose::STANDARD.encode(bruta.as_ref())))
}

/// De onde o valor cifrado vem: tabela, coluna e identificador estável da linha. Entra no AAD.
#[derive(Debug, Clone, Copy)]
pub struct Campo<'a> {
    pub tabela: &'a str,
    pub coluna: &'a str,
    pub id: &'a str,
}

impl Campo<'_> {
    fn validar(&self) -> Result<(), Erro> {
        if self.tabela.is_empty() || self.coluna.is_empty() || self.id.is_empty() {
            return Err(Erro::CampoIncompleto(format!("{:?}/{:?}/{:?}", self.tabela, self.coluna, self.id)));
        }
        Ok(())
    }

    /// Cada parte precedida do próprio tamanho em uvarint (o `binary.AppendUvarint` do Go): com
    /// separador, ("ab", "c") e ("a", "bc") dariam o mesmo AAD.
    fn aad(&self) -> Vec<u8> {
        let mut b = Vec::new();
        for parte in [ROTULO_AAD_V1, self.tabela, self.coluna, self.id] {
            let mut n = parte.len() as u64;
            while n >= 0x80 {
                b.push((n as u8) | 0x80);
                n >>= 7;
            }
            b.push(n as u8);
            b.extend_from_slice(parte.as_bytes());
        }
        b
    }
}

/// Cifra e decifra valores de campo com a chave derivada da mestra. Seguro para uso concorrente.
pub struct Cofre {
    v1: Aes256Gcm,
}

impl Cofre {
    /// Deriva a chave de cifra da mestra por HKDF-SHA256: a mestra nunca é usada direto como chave.
    pub fn new(mestra: &ChaveMestra) -> Cofre {
        let mut derivada = [0u8; 32];
        hkdf::Hkdf::<sha2::Sha256>::new(Some(SAL_HKDF), &mestra.0)
            .expand(INFO_CIFRA_V1, &mut derivada)
            .expect("32 bytes cabem no HKDF-SHA256");
        let v1 = Aes256Gcm::new(&derivada.into());
        derivada.zeroize();
        Cofre { v1 }
    }

    /// O valor pronto para gravar na coluna. O nonce é sorteado a cada chamada: repetir nonce com
    /// a mesma chave em GCM destrói a confidencialidade e revela a chave de autenticação.
    pub fn cifrar(&self, campo: Campo<'_>, claro: &Segredo) -> Result<String, Erro> {
        campo.validar()?;
        let mut nonce = [0u8; BYTES_NONCE];
        getrandom::fill(&mut nonce).map_err(|e| Erro::Sorteio("sortear nonce", e.to_string()))?;
        let aad = campo.aad();
        let selado = self
            .v1
            .encrypt(Nonce::from_slice(&nonce), Payload { msg: claro.revelar(), aad: &aad })
            .expect("o GCM só recusa entrada de 64 GiB");
        let mut tudo = Vec::with_capacity(BYTES_NONCE + selado.len());
        tudo.extend_from_slice(&nonce);
        tudo.extend_from_slice(&selado);
        Ok(format!("{PREFIXO_V1}{}", URL_SAFE_NO_PAD.encode(tudo)))
    }

    /// O valor em claro de uma coluna.
    pub fn decifrar(&self, campo: Campo<'_>, guardado: &str) -> Result<Segredo, Erro> {
        campo.validar()?;
        let corpo = guardado.strip_prefix(PREFIXO_V1).ok_or(Erro::Formato("versão de formato desconhecida"))?;
        // O RawURLEncoding do Go: sem padding, bits de folga aceitos, \r e \n ignorados.
        let [.., raw_url] = codificacoes();
        let corpo: String = corpo.chars().filter(|c| !matches!(c, '\r' | '\n')).collect();
        let selado = raw_url.decode(corpo).map_err(|_| Erro::Formato("base64"))?;
        if selado.len() < BYTES_NONCE + BYTES_TAG {
            return Err(Erro::Formato("corpo curto demais"));
        }
        let (nonce, resto) = selado.split_at(BYTES_NONCE);
        let aad = campo.aad();
        let claro = self.v1.decrypt(Nonce::from_slice(nonce), Payload { msg: resto, aad: &aad }).map_err(|_| Erro::Autenticacao)?;
        Ok(Segredo(claro))
    }
}

// ---------- canário ----------

/// A chave da linha de `settings` que guarda o canário.
pub const CHAVE_CANARIO: &str = "canario_chave_mestra";

/// O valor conhecido que o canário cifra. Não é segredo: o que importa é ser fixo.
const TEXTO_CANARIO: &str = "patchbay: canário da chave mestra";

const CAMPO_CANARIO: Campo<'static> = Campo { tabela: "settings", coluna: "valor", id: CHAVE_CANARIO };

/// O mínimo do armazenamento de configuração que o canário usa; quem tem o SQLite implementa.
pub trait Configuracoes {
    /// O valor da chave; `None` quando ela não existe, que não é erro.
    fn ler(&self, chave: &str) -> impl Future<Output = anyhow::Result<Option<String>>> + Send;
    /// Cria a chave só se ela não existe (`INSERT ... ON CONFLICT DO NOTHING`); a que já existe
    /// fica como está.
    fn gravar_se_ausente(&self, chave: &str, valor: &str) -> impl Future<Output = anyhow::Result<()>> + Send;
}

/// Grava o canário no primeiro boot e o confere em todos os seguintes; `true` quando acabou de
/// gravar. Falha dura de propósito: quem chama não sobe (indisponibilidade em vez de corrupção
/// silenciosa, seção 14).
pub async fn verificar_canario(cofre: &Cofre, cfgs: &impl Configuracoes) -> Result<bool, Erro> {
    let guardado = cfgs.ler(CHAVE_CANARIO).await.map_err(|e| Erro::Armazenamento("ler canário", e))?;
    let (guardado, criado) = match guardado {
        Some(g) => (g, false),
        None => {
            // Dois boots no mesmo banco vazio: o primeiro canário gravado vence, e o outro é
            // conferido como se já estivesse lá (com chave diferente, este boot recusa).
            let cifrado = cofre.cifrar(CAMPO_CANARIO, &Segredo::new(TEXTO_CANARIO))?;
            cfgs.gravar_se_ausente(CHAVE_CANARIO, &cifrado).await.map_err(|e| Erro::Armazenamento("gravar canário", e))?;
            let ficou = cfgs.ler(CHAVE_CANARIO).await.map_err(|e| Erro::Armazenamento("ler canário", e))?;
            let ficou = ficou.ok_or_else(|| Erro::Armazenamento("gravar canário", anyhow::anyhow!("o canário sumiu depois de gravado")))?;
            let nosso = ficou == cifrado;
            (ficou, nosso)
        }
    };
    let claro = cofre.decifrar(CAMPO_CANARIO, &guardado).map_err(|e| Erro::CanarioNaoConfere(Some(Box::new(e))))?;
    if claro.revelar() != TEXTO_CANARIO.as_bytes() {
        return Err(Erro::CanarioNaoConfere(None));
    }
    Ok(criado)
}

/// Os 19 testes de `internal/platform/cripto` (cripto_test, cofre_test e canario_test), com os
/// mesmos casos.
#[cfg(test)]
mod testes {
    use std::collections::{BTreeMap, HashSet};
    use std::sync::Mutex;

    use super::*;

    fn std64(b: &[u8]) -> String {
        general_purpose::STANDARD.encode(b)
    }

    // ---- cripto_test.go ----

    #[test]
    fn chave_mestra_de() {
        let trinta_e_dois = std64(&[0x2a; 32]);
        fn ausente(e: &Erro) -> bool {
            matches!(e, Erro::ChaveMestraAusente)
        }
        fn invalida(e: &Erro) -> bool {
            matches!(e, Erro::ChaveMestraInvalida(_))
        }
        type Quer = Option<fn(&Erro) -> bool>;
        let casos: Vec<(&str, String, Quer)> = vec![
            ("vazia", "".into(), Some(ausente)),
            ("só espaço", "   \n\t ".into(), Some(ausente)),
            ("não é base64", "isto não é base64!!".into(), Some(invalida)),
            ("curta demais", std64(&[1; 16]), Some(invalida)),
            ("longa demais", std64(&[1; 64]), Some(invalida)),
            ("base64 padrão", trinta_e_dois.clone(), None),
            ("base64 sem padding", general_purpose::STANDARD_NO_PAD.encode([0x2a; 32]), None),
            ("base64url", general_purpose::URL_SAFE.encode([0xfb; 32]), None),
            ("com espaço em volta", format!("  {trinta_e_dois}\n"), None),
            ("32 bytes crus não é chave", "k".repeat(32), Some(invalida)),
        ];
        for (nome, texto, quer) in casos {
            match (ChaveMestra::de(&texto), quer) {
                (Ok(_), None) => {}
                (Err(e), Some(q)) => assert!(q(&e), "{nome}: erro = {e}"),
                (r, q) => panic!("{nome}: resultado = {:?}, quer erro = {}", r.err(), q.is_some()),
            }
        }
    }

    #[test]
    fn chave_mestra_de_erro_nao_carrega_a_chave() {
        let quase_certa = "bm9wZSBhaW5kYSBuYW8gc2FvIDMyIGJ5dGVz";
        let e = ChaveMestra::de(quase_certa).expect_err("quer chave inválida");
        assert!(!e.to_string().contains(quase_certa), "mensagem = {e}");
    }

    #[test]
    fn gerar_chave_mestra_serve_de_entrada_para_o_cofre() {
        let mut vistas = HashSet::new();
        for i in 0..50 {
            let texto = gerar_chave_mestra().unwrap();
            assert!(vistas.insert(texto.to_string()), "chave repetida na volta {i}");
            Cofre::new(&ChaveMestra::de(&texto).unwrap());
        }
    }

    /// No Go, `NovoCofre(ChaveMestra{})` recusa a chave sem material. Aqui o tipo não deixa existir
    /// chave indefinida: o único construtor é [`ChaveMestra::de`], e ele recusa o vazio.
    #[test]
    fn novo_cofre_recusa_chave_indefinida() {
        assert!(matches!(ChaveMestra::de(""), Err(Erro::ChaveMestraAusente)));
    }

    #[test]
    fn segredo_nunca_sai_em_claro() {
        let valor = "sk-ant-valor-que-nao-pode-vazar";
        let segredo = Segredo::new(valor);
        #[derive(Debug)]
        #[allow(dead_code)] // só o Debug lê o campo
        struct Dentro {
            token: Segredo,
        }
        let formatos = [
            ("{}", format!("{segredo}")),
            ("{:?}", format!("{segredo:?}")),
            ("{:#?}", format!("{segredo:#?}")),
            ("{:>40}", format!("{segredo:>40}")),
            ("dentro de struct", format!("{:?}", Dentro { token: segredo.clone() })),
        ];
        for (nome, saida) in formatos {
            assert!(!saida.contains(valor), "{nome} = {saida}");
            assert!(saida.contains(REDIGIDO), "{nome} = {saida}");
        }
        assert_eq!(segredo.revelar(), valor.as_bytes());
    }

    #[test]
    fn chave_mestra_nunca_sai_em_claro() {
        let chave = ChaveMestra::de("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=").unwrap();
        for saida in [format!("{chave}"), format!("{chave:?}"), format!("{chave:#?}")] {
            assert!(saida.contains(REDIGIDO), "formatação = {saida}");
        }
    }

    /// A redação no caminho que mais importa: o log, pela camada no formato do Go.
    #[test]
    fn segredo_no_log() {
        let valor = "bearer-que-nao-pode-ir-para-o-log";
        let saida = crate::plataforma::log::capturar(|| {
            let chave = ChaveMestra::de(&std64(&[7; 32])).unwrap();
            tracing::info!(token = %Segredo::new(valor), chave_mestra = ?chave, "credencial aplicada");
        });
        assert!(!saida.contains(valor), "log = {saida}");
        assert!(saida.contains(REDIGIDO), "log = {saida}");
    }

    // ---- cofre_test.go ----

    const CAMPO: Campo<'static> = Campo { tabela: "upstream_secret", coluna: "valor_cifrado", id: "1/bearer/" };

    fn cofre_de_teste() -> Cofre {
        Cofre::new(&ChaveMestra::de(&gerar_chave_mestra().unwrap()).unwrap())
    }

    fn selado(guardado: &str) -> Vec<u8> {
        URL_SAFE_NO_PAD.decode(guardado.strip_prefix("pbc1:").expect("prefixo pbc1:")).expect("base64url sem padding")
    }

    #[test]
    fn cofre_ida_e_volta() {
        let casos = [
            ("valor vazio", String::new()),
            ("token opaco", "sk-ant-0123456789abcdef".into()),
            ("acentuação e emoji", "senha-com-ção-e-🔐".into()),
            ("valor com bytes altos", "\u{1F510}".repeat(64)),
            ("valor longo", "a".repeat(8192)),
            ("valor com quebra", "linha1\nlinha2\t".into()),
        ];
        for (nome, claro) in casos {
            let sut = cofre_de_teste();
            let guardado = sut.cifrar(CAMPO, &Segredo::new(claro.clone())).unwrap();
            assert!(claro.is_empty() || !guardado.contains(&claro), "{nome}: o claro apareceu no cifrado");
            assert_eq!(sut.decifrar(CAMPO, &guardado).unwrap().revelar(), claro.as_bytes(), "{nome}");
        }
    }

    #[test]
    fn cofre_aad_errada_falha() {
        let sut = cofre_de_teste();
        let guardado = sut.cifrar(CAMPO, &Segredo::new("segredo-da-linha-1")).unwrap();
        let casos = [
            ("outra linha", Campo { id: "2/bearer/", ..CAMPO }),
            ("outro tipo", Campo { id: "1/header/X-Api-Key", ..CAMPO }),
            ("outra coluna", Campo { coluna: "outra_coluna", ..CAMPO }),
            ("outra tabela", Campo { tabela: "upstream_oauth", ..CAMPO }),
            // Sem prefixo de tamanho no AAD, ("a","bc") e ("ab","c") colidiriam.
            ("fronteira deslocada", Campo { tabela: "upstream_secre", coluna: "tvalor_cifrado", id: "1/bearer/" }),
        ];
        for (nome, destino) in casos {
            assert!(matches!(sut.decifrar(destino, &guardado), Err(Erro::Autenticacao)), "{nome}");
        }
    }

    #[test]
    fn cofre_nonce_unico() {
        let sut = cofre_de_teste();
        let mut vistos = HashSet::new();
        for i in 0..500 {
            let guardado = sut.cifrar(CAMPO, &Segredo::new("o mesmo valor, sempre")).unwrap();
            assert!(vistos.insert(selado(&guardado)[..12].to_vec()), "nonce repetido na volta {i}");
        }
    }

    #[test]
    fn cofre_formato_versionado() {
        let guardado = cofre_de_teste().cifrar(CAMPO, &Segredo::new("x")).unwrap();
        // 12 bytes de nonce + 16 de tag do GCM (+ 1 do "x").
        assert_eq!(selado(&guardado).len(), 12 + 16 + 1);
    }

    #[test]
    fn cofre_valor_guardado_invalido() {
        let sut = cofre_de_teste();
        let valido = sut.cifrar(CAMPO, &Segredo::new("x")).unwrap();
        let corpo = valido.strip_prefix("pbc1:").unwrap();
        // Flipa um bit no meio do selado: alterar só o último caractere base64 mexe às vezes em
        // bits de folga e deixaria o teste ao acaso (o comentário do Go explica).
        let mut b = selado(&valido);
        let meio = b.len() / 2;
        b[meio] ^= 0x01;
        let alterado = format!("pbc1:{}", URL_SAFE_NO_PAD.encode(b));
        fn formato(e: &Erro) -> bool {
            matches!(e, Erro::Formato(_))
        }
        fn autenticacao(e: &Erro) -> bool {
            matches!(e, Erro::Autenticacao)
        }
        type Caso = (&'static str, String, fn(&Erro) -> bool);
        let casos: [Caso; 6] = [
            ("vazio", String::new(), formato),
            ("sem prefixo de versão", corpo.to_string(), formato),
            ("versão desconhecida", format!("pbc9:{corpo}"), formato),
            ("base64 quebrado", "pbc1:não é base64".into(), formato),
            ("corpo curto demais", "pbc1:AAAA".into(), formato),
            ("texto cifrado alterado", alterado, autenticacao),
        ];
        for (nome, guardado, quer) in casos {
            let e = sut.decifrar(CAMPO, &guardado).expect_err(nome);
            assert!(quer(&e), "{nome}: erro = {e}");
        }
    }

    #[test]
    fn cofre_chave_trocada_nao_decifra() {
        let guardado = cofre_de_teste().cifrar(CAMPO, &Segredo::new("token-do-notion")).unwrap();
        assert!(matches!(cofre_de_teste().decifrar(CAMPO, &guardado), Err(Erro::Autenticacao)));
    }

    #[test]
    fn cofre_campo_incompleto_recusado() {
        let sut = cofre_de_teste();
        for (nome, c) in [
            ("sem tabela", Campo { tabela: "", ..CAMPO }),
            ("sem coluna", Campo { coluna: "", ..CAMPO }),
            ("sem id", Campo { id: "", ..CAMPO }),
        ] {
            assert!(matches!(sut.cifrar(c, &Segredo::new("x")), Err(Erro::CampoIncompleto(_))), "{nome}: cifrar");
            assert!(matches!(sut.decifrar(c, "pbc1:AAAA"), Err(Erro::CampoIncompleto(_))), "{nome}: decifrar");
        }
    }

    #[test]
    fn cofre_erro_nao_carrega_o_valor() {
        let segredo = "sk-nao-pode-aparecer-em-erro";
        let guardado = cofre_de_teste().cifrar(CAMPO, &Segredo::new(segredo)).unwrap();
        let texto = cofre_de_teste().decifrar(CAMPO, &guardado).expect_err("quer falha de autenticação").to_string();
        assert!(!texto.contains(segredo) && !texto.contains(&guardado), "mensagem = {texto}");
    }

    // ---- canario_test.go ----

    /// A tabela settings em memória, escrita à mão como todo dublê do repositório.
    #[derive(Default)]
    struct ConfigFalsa {
        linhas: Mutex<BTreeMap<String, String>>,
        erro: Option<&'static str>,
        /// O canário que outro boot grava entre a nossa leitura e a nossa escrita.
        intruso: Mutex<Option<String>>,
    }

    impl Configuracoes for ConfigFalsa {
        async fn ler(&self, chave: &str) -> anyhow::Result<Option<String>> {
            if let Some(e) = self.erro {
                anyhow::bail!(e);
            }
            Ok(self.linhas.lock().unwrap().get(chave).cloned())
        }

        async fn gravar_se_ausente(&self, chave: &str, valor: &str) -> anyhow::Result<()> {
            if let Some(e) = self.erro {
                anyhow::bail!(e);
            }
            let mut linhas = self.linhas.lock().unwrap();
            if let Some(outro) = self.intruso.lock().unwrap().take() {
                linhas.insert(chave.into(), outro);
            }
            linhas.entry(chave.into()).or_insert_with(|| valor.into());
            Ok(())
        }
    }

    #[tokio::test]
    async fn canario_primeiro_boot_grava_e_os_seguintes_conferem() {
        let sut = cofre_de_teste();
        let cfgs = ConfigFalsa::default();
        assert!(verificar_canario(&sut, &cfgs).await.unwrap(), "primeiro boot: criado = false");
        let guardado = cfgs.ler(CHAVE_CANARIO).await.unwrap().expect("canário gravado");
        assert!(guardado.starts_with("pbc1:"), "canário guardado = {guardado}");
        for volta in 0..3 {
            assert!(!verificar_canario(&sut, &cfgs).await.unwrap(), "boot seguinte {volta}: criado = true");
        }
    }

    #[tokio::test]
    async fn canario_chave_trocada_falha() {
        let cfgs = ConfigFalsa::default();
        let antigo = cofre_de_teste();
        verificar_canario(&antigo, &cfgs).await.unwrap();
        let e = verificar_canario(&cofre_de_teste(), &cfgs).await.expect_err("chave trocada");
        // A causa continua embrulhada, para o diagnóstico não depender do texto.
        assert!(matches!(&e, Erro::CanarioNaoConfere(Some(c)) if matches!(**c, Erro::Autenticacao)), "erro = {e}");
        // Um boot recusado não sobrescreve o canário: a chave certa continua voltando.
        let guardado = cfgs.ler(CHAVE_CANARIO).await.unwrap().unwrap();
        antigo.decifrar(CAMPO_CANARIO, &guardado).expect("canário deixou de decifrar com a chave original");
    }

    #[tokio::test]
    async fn canario_valor_corrompido_falha() {
        let sut = cofre_de_teste();
        // Um valor legítimo desta mesma chave, gravado com outro contexto: o AAD o recusa.
        let transplantado = sut.cifrar(Campo { id: "outra_chave", ..CAMPO_CANARIO }, &Segredo::new(TEXTO_CANARIO)).unwrap();
        for (nome, guardado) in [
            ("lixo em claro", "isto nunca foi cifrado".to_string()),
            ("prefixo desconhecido", "pbc9:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA".into()),
            ("vazio", String::new()),
            ("cifrado de outra linha", transplantado),
        ] {
            let cfgs = ConfigFalsa::default();
            cfgs.gravar_se_ausente(CHAVE_CANARIO, &guardado).await.unwrap();
            let r = verificar_canario(&sut, &cfgs).await;
            assert!(matches!(r, Err(Erro::CanarioNaoConfere(_))), "{nome}: {r:?}");
        }
    }

    #[tokio::test]
    async fn canario_propaga_falha_do_armazenamento() {
        let cfgs = ConfigFalsa { erro: Some("disco cheio"), ..ConfigFalsa::default() };
        let e = verificar_canario(&cofre_de_teste(), &cfgs).await.expect_err("quer a falha do disco");
        assert!(matches!(&e, Erro::Armazenamento(_, c) if c.to_string() == "disco cheio"), "erro = {e}");
    }

    /// Item 10 da revisão: dois boots no banco vazio. O canário do outro chega primeiro; com a
    /// mesma chave este boot confere o dele (sem se dizer criador), com outra chave recusa, e
    /// nenhum dos dois sobrescreve o que ficou.
    #[tokio::test]
    async fn canario_corrida_no_primeiro_boot() {
        let sut = cofre_de_teste();
        let do_outro = sut.cifrar(CAMPO_CANARIO, &Segredo::new(TEXTO_CANARIO)).unwrap();
        let cfgs = ConfigFalsa { intruso: Mutex::new(Some(do_outro.clone())), ..ConfigFalsa::default() };
        assert!(!verificar_canario(&sut, &cfgs).await.unwrap(), "disse que criou o canário do outro");
        assert_eq!(cfgs.ler(CHAVE_CANARIO).await.unwrap().as_deref(), Some(do_outro.as_str()));

        let de_outra_chave = cofre_de_teste().cifrar(CAMPO_CANARIO, &Segredo::new(TEXTO_CANARIO)).unwrap();
        let cfgs = ConfigFalsa { intruso: Mutex::new(Some(de_outra_chave.clone())), ..ConfigFalsa::default() };
        let e = verificar_canario(&sut, &cfgs).await.expect_err("chave diferente da do canário que ficou");
        assert!(matches!(e, Erro::CanarioNaoConfere(_)), "{e}");
        assert_eq!(cfgs.ler(CHAVE_CANARIO).await.unwrap().as_deref(), Some(de_outra_chave.as_str()));
    }

    /// Item 7: como o Go, o claro é bytes (o que não é UTF-8 volta igual) e o base64 guardado
    /// aceita quebras de linha no meio.
    #[test]
    fn decifrar_bytes_e_quebras_como_o_go() {
        let sut = cofre_de_teste();
        let bytes = vec![0xff, 0xfe, 0x00, b'a'];
        let guardado = sut.cifrar(CAMPO, &Segredo::new(bytes.clone())).unwrap();
        assert_eq!(sut.decifrar(CAMPO, &guardado).unwrap().revelar(), bytes.as_slice());
        let corpo = guardado.strip_prefix("pbc1:").unwrap();
        let quebrado = format!("pbc1:{}\r\n{}\n", &corpo[..10], &corpo[10..]);
        assert_eq!(sut.decifrar(CAMPO, &quebrado).unwrap().revelar(), bytes.as_slice());
    }

    /// O AAD em uvarint, conferido contra o `binary.AppendUvarint` do Go num tamanho de dois bytes.
    #[test]
    fn aad_em_uvarint() {
        let longo = "x".repeat(300);
        let aad = Campo { tabela: "t", coluna: "c", id: &longo }.aad();
        assert_eq!(&aad[..10], b"\x04pbc1\x01t\x01c\xac");
        assert_eq!(aad[10], 0x02);
        assert_eq!(aad.len(), 11 + 300);
    }
}
