//! A chave mestra do processo e o canário (`cmd/patchbay/chave.go`): de onde a chave vem, o
//! arquivo `<dados>/chave-mestra` quando o ambiente não traz nenhuma, e o subcomando
//! `chave-mestra gerar`.

use std::io::Write as _;
use std::path::Path;

use anyhow::Context as _;
use zeroize::Zeroizing;

use crate::cli::Flags;
use crate::plataforma::cripto::{self, ChaveMestra, Cofre, VAR_CHAVE_MESTRA};
use crate::plataforma::store::Store;

/// O arquivo da chave quando o ambiente não traz nenhuma.
const ARQUIVO_CHAVE_MESTRA: &str = "chave-mestra";

/// O que o operador precisa ler quando o processo se recusa a subir por causa da chave.
fn instrucao_chave_mestra() -> String {
    format!(
        "
A chave mestra cifra em repouso todo segredo que o patchbay apresenta a um MCP.
Ela vem de {VAR_CHAVE_MESTRA} quando a variável existe; senão, do
arquivo {ARQUIVO_CHAVE_MESTRA} no diretório de dados, criado no primeiro boot.
São 32 bytes em base64. Gere uma com:

    patchbay chave-mestra gerar"
    )
}

fn instrucao_canario() -> String {
    format!(
        "
O canário gravado neste banco não volta com a chave mestra em uso: ela é outra.
Todo segredo cifrado aqui ficou ilegível, então o patchbay se recusa a subir em
vez de tratar cada upstream como se o provedor tivesse revogado o acesso.

Reponha em {VAR_CHAVE_MESTRA} (ou no arquivo {ARQUIVO_CHAVE_MESTRA}) a chave com que este banco foi criado. Se ela
foi perdida de vez, o caminho é apagar o banco e recadastrar — não existe
recuperação, e é por isso que a chave é segredo de produção."
    )
}

/// A cifra de campo a partir dos bytes da chave mestra (do ambiente ou do arquivo). Bytes que não
/// são UTF-8 não são base64: a mesma recusa do Go, que lê a variável e o arquivo como bytes.
fn cofre_de(bytes: &[u8]) -> anyhow::Result<Cofre> {
    let mestra = std::str::from_utf8(bytes)
        .map_err(|_| cripto::Erro::ChaveMestraInvalida("não é base64".into()))
        .and_then(ChaveMestra::de)
        .map_err(|e| anyhow::anyhow!("{e}\n{}", instrucao_chave_mestra()))?;
    Ok(Cofre::new(&mestra))
}

/// A cifra de campo do processo. `do_ambiente` é `PATCHBAY_MASTER_KEY`, que o `main` lê e tira do
/// ambiente antes de existir outra thread (um MCP stdio herda o ambiente do patchbay). Sem ela, a
/// chave mora em `<dados>/chave-mestra`, sorteada no primeiro boot. shortcut: com a chave ao lado
/// do banco, a cifra protege o banco copiado sozinho, não o volume inteiro; quem precisa disso usa
/// a variável.
pub fn cofre_do_boot(data_dir: &str, do_ambiente: Option<&[u8]>) -> anyhow::Result<Cofre> {
    match do_ambiente.filter(|t| !t.is_empty()) {
        Some(bytes) => cofre_de(bytes),
        None => cofre_de(&chave_do_arquivo(Path::new(data_dir))?),
    }
}

/// Lê `<dados>/chave-mestra`, criando-a se não existir.
fn chave_do_arquivo(dir: &Path) -> anyhow::Result<Zeroizing<Vec<u8>>> {
    let caminho = dir.join(ARQUIVO_CHAVE_MESTRA);
    if let Some(b) = ler_chave(&caminho)? {
        return Ok(b);
    }
    criar_diretorio(dir)?;
    let texto = cripto::gerar_chave_mestra()?;
    // Grava num temporário e publica com link: o link é atômico e falha se o caminho já existe,
    // então dois boots no mesmo volume nunca leem uma chave pela metade nem sorteiam duas.
    let mut sufixo = [0u8; 6];
    getrandom::fill(&mut sufixo).map_err(|e| anyhow::anyhow!("criar chave mestra em {}: {e}", dir.display()))?;
    let tmp = dir.join(format!(".chave-mestra-{}", sufixo.iter().map(|b| format!("{b:02x}")).collect::<String>()));
    let mut conteudo = Zeroizing::new(Vec::with_capacity(texto.len() + 1));
    conteudo.extend_from_slice(texto.as_bytes());
    conteudo.push(b'\n');
    let publicada = publicar(&tmp, &caminho, &conteudo);
    // O temporário sai em todo caminho, de erro ou não (com o link, o arquivo fica pelo nome final).
    let _ = std::fs::remove_file(&tmp);
    if publicada? {
        return Ok(conteudo);
    }
    // Outro boot publicou primeiro: lê o dele, uma vez só. Se o caminho existe e não abre (link
    // simbólico pendurado), não há o que ler nem como publicar por cima.
    ler_chave(&caminho)?
        .with_context(|| format!("criar {}: o caminho já existe mas não abre como arquivo (link simbólico quebrado?)", caminho.display()))
}

/// Grava o conteúdo no temporário e o publica com `hard_link`; `false` quando o destino já existe.
fn publicar(tmp: &Path, caminho: &Path, conteudo: &[u8]) -> anyhow::Result<bool> {
    let mut opcoes = std::fs::OpenOptions::new();
    opcoes.write(true).create_new(true);
    #[cfg(unix)]
    std::os::unix::fs::OpenOptionsExt::mode(&mut opcoes, 0o600);
    let dir = caminho.parent().unwrap_or(Path::new("."));
    let mut arquivo = opcoes.open(tmp).with_context(|| format!("criar chave mestra em {}", dir.display()))?;
    arquivo.write_all(conteudo).and_then(|()| arquivo.sync_all()).context("gravar chave mestra")?;
    drop(arquivo);
    match std::fs::hard_link(tmp, caminho) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => return Ok(false),
        Err(e) => return Err(e).with_context(|| format!("criar {}", caminho.display())),
    }
    // O link só sobrevive a uma queda de energia depois do fsync do diretório.
    #[cfg(unix)]
    std::fs::File::open(dir).and_then(|d| d.sync_all()).with_context(|| format!("gravar chave mestra: fsync de {}", dir.display()))?;
    Ok(true)
}

/// O conteúdo do arquivo da chave, ou `None` se ele não existe. Avisa, sem recusar, quando a
/// permissão é mais aberta que 0600: recusar derrubaria instalação que já funciona.
fn ler_chave(caminho: &Path) -> anyhow::Result<Option<Zeroizing<Vec<u8>>>> {
    let bytes = match std::fs::read(caminho) {
        Ok(b) => Zeroizing::new(b),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e).with_context(|| format!("ler {}", caminho.display())),
    };
    #[cfg(unix)]
    if let Ok(m) = std::fs::metadata(caminho) {
        use std::os::unix::fs::PermissionsExt;
        let modo = m.permissions().mode() & 0o777;
        if modo & 0o077 != 0 {
            tracing::warn!(
                arquivo = %caminho.display(),
                permissao = format!("{modo:04o}"),
                "o arquivo da chave mestra tem permissão mais aberta que 0600; restrinja com chmod 600"
            );
        }
    }
    Ok(Some(bytes))
}

/// `os.MkdirAll(dir, 0o750)`.
pub fn criar_diretorio(dir: &Path) -> anyhow::Result<()> {
    let mut b = std::fs::DirBuilder::new();
    b.recursive(true);
    #[cfg(unix)]
    std::os::unix::fs::DirBuilderExt::mode(&mut b, 0o750);
    b.create(dir).with_context(|| format!("criar diretório de dados {}", dir.display()))
}

/// Grava o canário no primeiro boot e confere nos seguintes. Falha dura: o processo não sobe.
pub async fn verificar_canario(cofre: &Cofre, st: &Store) -> anyhow::Result<()> {
    let criado =
        cripto::verificar_canario(cofre, &st.configuracoes()).await.map_err(|e| anyhow::anyhow!("{e}\n{}", instrucao_canario()))?;
    if criado {
        tracing::info!("canário da chave mestra gravado no primeiro boot");
    }
    Ok(())
}

/// O subcomando que gera uma chave nova: o único caminho do binário que não exige a chave já
/// existir (o ovo e a galinha da primeira instalação).
pub fn comando_chave_mestra(args: &[String]) -> anyhow::Result<()> {
    let (verbo, args) = match args.split_first() {
        Some((v, resto)) if !v.starts_with('-') => (v.as_str(), resto),
        _ => ("gerar", args),
    };
    anyhow::ensure!(verbo == "gerar", "chave-mestra: verbo desconhecido: {verbo} (use: gerar)");
    Flags::new("patchbay chave-mestra gerar")
        .analisar(args)
        .map_err(|e| if e.is::<crate::cli::PedidoDeAjuda>() { e } else { e.context("chave-mestra") })?;
    let chave = cripto::gerar_chave_mestra()?;
    // A chave sozinha na saída padrão, para `export PATCHBAY_MASTER_KEY=$(patchbay chave-mestra
    // gerar)` funcionar; a explicação vai para a saída de erro, que não entra na substituição.
    println!("{}", chave.as_str());
    eprint!(
        "
Chave mestra de 32 bytes em base64, impressa uma única vez: ela não é guardada
em lugar nenhum pelo patchbay. Exporte em {VAR_CHAVE_MESTRA} e guarde onde você guarda
segredo de produção.
"
    );
    Ok(())
}

#[cfg(test)]
mod testes {
    use super::*;
    use crate::plataforma::store::DirTemporario;

    /// Sem a variável, a chave nasce em `<dados>/chave-mestra` (0600) e volta igual nos boots
    /// seguintes; com a variável, o arquivo nem é criado.
    #[test]
    fn chave_do_arquivo_nasce_uma_vez() {
        let d = DirTemporario::new();
        let dados = d.0.join("dados");
        let primeira = chave_do_arquivo(&dados).unwrap();
        assert_eq!(chave_do_arquivo(&dados).unwrap(), primeira);
        let caminho = dados.join(ARQUIVO_CHAVE_MESTRA);
        assert!(primeira.ends_with(b"\n") && primeira.len() == 45, "a chave e a quebra de linha");
        assert_eq!(std::fs::read(&caminho).unwrap(), primeira.as_slice());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(std::fs::metadata(&caminho).unwrap().permissions().mode() & 0o777, 0o600);
        }
        assert_eq!(nomes(&dados), [ARQUIVO_CHAVE_MESTRA], "o temporário sai");

        let outro = d.0.join("outro");
        cofre_do_boot(outro.to_str().unwrap(), Some(&primeira)).unwrap();
        assert!(!outro.exists());
    }

    fn nomes(dir: &Path) -> Vec<String> {
        let mut v: Vec<String> = std::fs::read_dir(dir).unwrap().map(|e| e.unwrap().file_name().into_string().unwrap()).collect();
        v.sort();
        v
    }

    #[test]
    fn chave_invalida_explica_como_gerar() {
        let e = cofre_do_boot("/nao/usado", Some(b"curta")).err().unwrap().to_string();
        assert!(e.starts_with("cripto: chave mestra inválida: "), "{e}");
        assert!(e.ends_with("patchbay chave-mestra gerar"), "{e}");
    }

    /// Item 3 da revisão: valor que não é UTF-8 é chave inválida, com a mensagem do Go, e não
    /// cai para o arquivo (que sortearia uma chave nova em silêncio).
    #[test]
    fn chave_nao_utf8_e_invalida() {
        let d = DirTemporario::new();
        let e = cofre_do_boot(d.0.to_str().unwrap(), Some(b"\xff\xfe")).err().unwrap().to_string();
        assert!(e.starts_with("cripto: chave mestra inválida: não é base64\n"), "{e}");
        assert!(nomes(&d.0).is_empty(), "criou arquivo de chave");
    }

    /// Item 4: link simbólico pendurado no lugar do arquivo. Antes, recursão até estourar a
    /// pilha, deixando um temporário por volta; agora, um erro claro e nada sobrando.
    #[cfg(unix)]
    #[test]
    fn link_pendurado_falha_sem_recursao_nem_sobra() {
        let d = DirTemporario::new();
        std::os::unix::fs::symlink(d.0.join("nao-existe"), d.0.join(ARQUIVO_CHAVE_MESTRA)).unwrap();
        let e = format!("{:#}", chave_do_arquivo(&d.0).unwrap_err());
        assert!(e.contains("link simbólico quebrado"), "{e}");
        assert_eq!(nomes(&d.0), [ARQUIVO_CHAVE_MESTRA], "sobrou temporário");
    }

    /// Item 8: arquivo de chave mais aberto que 0600 é lido, com aviso no log.
    #[cfg(unix)]
    #[test]
    fn permissao_aberta_avisa_sem_recusar() {
        use std::os::unix::fs::PermissionsExt;
        let d = DirTemporario::new();
        let chave = chave_do_arquivo(&d.0).unwrap();
        let caminho = d.0.join(ARQUIVO_CHAVE_MESTRA);
        let log = crate::plataforma::log::capturar(|| assert_eq!(chave_do_arquivo(&d.0).unwrap(), chave));
        assert!(log.is_empty(), "avisou com 0600: {log}");
        std::fs::set_permissions(&caminho, std::fs::Permissions::from_mode(0o644)).unwrap();
        let log = crate::plataforma::log::capturar(|| assert_eq!(chave_do_arquivo(&d.0).unwrap(), chave));
        assert!(log.contains("\"level\":\"WARN\"") && log.contains("\"permissao\":\"0644\""), "{log}");
    }
}
