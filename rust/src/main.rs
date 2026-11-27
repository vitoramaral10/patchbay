//! O binário patchbay em Rust: o gateway MCP self-hosted, portado do Go em fatias (plano em
//! `docs/estudos/2026-11-27-migracao-rust.md`). Na F1 existe a plataforma: configuração, chave
//! mestra e canário, banco com as migrações do Go, log no formato do Go, `/saude`, desligamento e
//! reinício interno, e os subcomandos que dependem só disso.
//!
//! `serve`, `seed`, `export` e `import` exigem a chave mestra (de `PATCHBAY_MASTER_KEY` ou de
//! `<dados>/chave-mestra`): sem ela não há como ler nem gravar segredo em repouso.

mod app;
mod chave;
mod cli;
mod config;
mod plataforma;
mod seed;

use std::process::ExitCode;

use plataforma::cripto::VAR_CHAVE_MESTRA;
use zeroize::Zeroizing;

/// Número, commit e data do build, como os `-ldflags` do goreleaser no Go.
fn versao() -> String {
    format!(
        "{} (commit {}, build {})",
        option_env!("PATCHBAY_VERSAO").unwrap_or("dev"),
        option_env!("PATCHBAY_COMMIT").unwrap_or("desconhecido"),
        option_env!("PATCHBAY_DATA_BUILD").unwrap_or("desconhecida"),
    )
}

const AJUDA: &str = "uso: patchbay <subcomando> [flags]

subcomandos:
  serve                sobe o gateway (padrão)
  seed                 cria endpoint, upstream HTTP e chave de API de desenvolvimento
  biblioteca-semente   regrava o catálogo embutido varrendo o mcpservers.org   [-o arquivo]
  export               escreve a configuração versionável em YAML   [-o arquivo] [--forcar]
  import               aplica um YAML   arquivo [--dry-run] [--remover-ausentes]
  chave-mestra gerar   sorteia a chave de PATCHBAY_MASTER_KEY
  versao               imprime a versão
  ajuda                imprime esta mensagem

flags comuns (todas com variável de ambiente equivalente):
  -listen       endereço de escuta            PATCHBAY_LISTEN
  -data-dir     diretório do banco SQLite     PATCHBAY_DATA_DIR
  -public-url   URL pública do patchbay       PATCHBAY_PUBLIC_URL
  -log-level    debug|info|warn|error         PATCHBAY_LOG_LEVEL
  -log-texto    log em texto em vez de JSON   PATCHBAY_LOG_TEXTO

variável obrigatória em serve e seed (não tem flag equivalente, de propósito:
argumento de processo aparece em ps e em histórico de shell):
  PATCHBAY_MASTER_KEY   chave mestra de cifra, 32 bytes em base64
";

fn main() -> ExitCode {
    // Antes de qualquer outra coisa: o processo deixa de ser "dumpable". Com isso o
    // /proc/<pid>/environ (que guarda o bloco de ambiente original, chave mestra inclusa) e o
    // /proc/<pid>/mem passam a ser do root, e ptrace e core dump ficam bloqueados para outro
    // processo do mesmo uid, como um MCP stdio de terceiro. O filho que o patchbay executa volta a
    // ser dumpable no execve, então isso não vaza para ele.
    #[cfg(target_os = "linux")]
    // SAFETY: prctl com argumentos inteiros não toca memória do processo.
    unsafe {
        libc::prctl(libc::PR_SET_DUMPABLE, 0, 0, 0, 0)
    };
    // A chave sai do ambiente antes de existir qualquer outra thread, com os bytes zerados no
    // próprio bloco do ambiente: só tirar a variável (unsetenv) deixaria o valor na memória e no
    // /proc/<pid>/environ, e um MCP stdio herda o ambiente do patchbay.
    let chave = tirar_do_ambiente(VAR_CHAVE_MESTRA);
    let args: Vec<String> = std::env::args_os().skip(1).map(|a| a.to_string_lossy().into_owned()).collect();
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build().expect("runtime do tokio");
    match rt.block_on(executar(&args, chave.as_deref().map(Vec::as_slice))) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) if e.is::<cli::PedidoDeAjuda>() => ExitCode::from(2),
        Err(e) => {
            // A borda: é aqui que erro vira texto e código de saída, e em nenhum lugar mais.
            eprintln!("patchbay: {e:#}");
            ExitCode::FAILURE
        }
    }
}

/// Lê a variável em bytes (valor que não é UTF-8 chega como está e é recusado como chave
/// inválida, como no Go), zera o valor em cada entrada `NOME=` do bloco do ambiente e a remove.
/// Só pode ser chamada com uma thread só.
fn tirar_do_ambiente(nome: &str) -> Option<Zeroizing<Vec<u8>>> {
    let valor = Zeroizing::new(std::env::var_os(nome)?.into_encoded_bytes());
    #[cfg(target_os = "linux")]
    {
        unsafe extern "C" {
            static mut environ: *mut *mut std::ffi::c_char;
        }
        let prefixo = format!("{nome}=");
        // SAFETY: uma thread só (o runtime nasce depois); cada entrada de `environ` é uma string C
        // gravável terminada em zero, e só se escreve dentro dela.
        unsafe {
            let mut e = environ;
            while !e.is_null() && !(*e).is_null() {
                let entrada = (*e).cast::<u8>();
                let n = libc::strlen(*e);
                if std::slice::from_raw_parts(entrada, n).starts_with(prefixo.as_bytes()) {
                    for i in prefixo.len()..n {
                        std::ptr::write_volatile(entrada.add(i), 0);
                    }
                }
                e = e.add(1);
            }
        }
    }
    // shortcut: fora do Linux só se tira a variável, sem zerar o bloco; a imagem é Linux.
    // SAFETY: uma thread só, como acima.
    unsafe { std::env::remove_var(nome) };
    Some(valor)
}

async fn executar(args: &[String], chave: Option<&[u8]>) -> anyhow::Result<()> {
    let (comando, args) = match args.split_first() {
        Some((c, resto)) if !c.starts_with('-') => (c.as_str(), resto),
        _ => ("serve", args),
    };
    match comando {
        "serve" => {
            let cfg = config::ler(comando, args)?;
            plataforma::log::iniciar(cfg.nivel_log, cfg.log_em_texto);
            app::servir(cfg, chave).await
        }
        "seed" => seed::comando_seed(args, chave).await,
        "export" | "import" => anyhow::bail!("{comando}: ainda não portado para Rust (entra com a F12)"),
        "biblioteca-semente" => anyhow::bail!("{comando}: ainda não portado para Rust (entra com a F13)"),
        "chave-mestra" => chave::comando_chave_mestra(args),
        "versao" | "version" => {
            println!("patchbay {}", versao());
            Ok(())
        }
        "ajuda" | "help" => {
            print!("{AJUDA}");
            Ok(())
        }
        _ => {
            eprint!("{AJUDA}");
            anyhow::bail!("subcomando desconhecido: {comando}")
        }
    }
}

#[cfg(test)]
mod testes {
    /// Os bytes do valor somem do bloco do ambiente, não só a variável. O `setenv` da glibc nunca
    /// libera a string que alocou, então o ponteiro antigo continua lendo memória válida.
    #[cfg(all(target_os = "linux", target_env = "gnu"))]
    #[test]
    fn tirar_do_ambiente_zera_o_valor() {
        let nome = "PATCHBAY_TESTE_TIRAR_DO_AMBIENTE";
        // SAFETY: nenhum outro teste deste binário lê ou grava esta variável.
        unsafe { std::env::set_var(nome, "valor-que-some") };
        let c = std::ffi::CString::new(nome).unwrap();
        let p = unsafe { libc::getenv(c.as_ptr()) };
        assert!(!p.is_null());
        let lido = super::tirar_do_ambiente(nome).unwrap();
        assert_eq!(lido.as_slice(), b"valor-que-some");
        assert!(std::env::var_os(nome).is_none(), "a variável continua definida");
        let antigo = unsafe { std::slice::from_raw_parts(p.cast::<u8>(), 14) };
        assert_eq!(antigo, [0u8; 14], "o valor ficou no bloco do ambiente");
        assert!(super::tirar_do_ambiente(nome).is_none());
    }
}
