//! Troca cruzada de banco e de chave entre o binário Go e o Rust (critérios da F1 no plano): o
//! banco que um cria o outro abre, sem mexer em `goose_db_version` nem no canário, e a chave que
//! um gera o outro aceita. Roda os dois binários como processos, no mesmo diretório de dados.
//!
//! ```sh
//! go build -p 2 -o /tmp/patchbay-go ./cmd/patchbay
//! PATCHBAY_GO=/tmp/patchbay-go cargo test -j 2 --test troca_com_go
//! ```
//!
//! Sem `PATCHBAY_GO` cada teste avisa e não faz nada.

use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

/// A chave mestra fixa da caixa-preta (`tests/apoio/mod.rs`).
const CHAVE: &str = "cGF0Y2hiYXktY2FpeGEtcHJldGEtY2hhdmUtZml4YSE=";
const RUST: &str = env!("CARGO_BIN_EXE_patchbay");

fn go() -> Option<String> {
    let g = std::env::var("PATCHBAY_GO").ok().filter(|g| !g.is_empty());
    if g.is_none() {
        eprintln!("[pulado] PATCHBAY_GO não definido: este teste precisa do binário Go");
    }
    g
}

/// Um diretório de cena, apagado no fim.
struct Cena(PathBuf);

impl Cena {
    fn new() -> Self {
        static N: AtomicUsize = AtomicUsize::new(0);
        let d = std::env::temp_dir().join(format!("patchbay-troca-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        Cena(d)
    }

    fn dados(&self) -> PathBuf {
        self.0.join("dados")
    }

    fn banco(&self) -> PathBuf {
        self.dados().join("patchbay.db")
    }

    fn comando(&self, bin: &str, args: &[&str], chave: Option<&str>, porta: u16) -> Command {
        let mut c = Command::new(bin);
        c.args(args).env_clear().current_dir(&self.0).stdin(Stdio::null());
        let fechada = "http://127.0.0.1:9";
        c.envs([
            ("PATH", "/usr/local/bin:/usr/bin:/bin"),
            ("HTTP_PROXY", fechada),
            ("HTTPS_PROXY", fechada),
            ("PATCHBAY_LOG_LEVEL", "debug"),
        ]);
        c.env("HOME", &self.0).env("PATCHBAY_DATA_DIR", self.dados()).env("PATCHBAY_LISTEN", format!("127.0.0.1:{porta}"));
        if let Some(k) = chave {
            c.env("PATCHBAY_MASTER_KEY", k);
        }
        c
    }

    fn rodar(&self, bin: &str, args: &[&str], chave: Option<&str>) -> Output {
        self.comando(bin, args, chave, 1).output().unwrap_or_else(|e| panic!("rodar {bin}: {e}"))
    }

    /// Sobe `serve`, espera `/saude`, manda SIGTERM e espera a saída. Devolve o código de saída e
    /// a saída de erro (o log).
    fn subir_e_parar(&self, bin: &str, chave: Option<&str>) -> (Option<i32>, String) {
        let porta = std::net::TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port();
        let log = self.0.join("log");
        let arquivo = std::fs::File::create(&log).unwrap();
        let mut filho = self.comando(bin, &["serve"], chave, porta).stdout(arquivo.try_clone().unwrap()).stderr(arquivo).spawn().unwrap();
        let prazo = Instant::now() + Duration::from_secs(10);
        let mut no_ar = false;
        while Instant::now() < prazo && !no_ar {
            if filho.try_wait().unwrap().is_some() {
                break;
            }
            no_ar = saude(porta);
            std::thread::sleep(Duration::from_millis(50));
        }
        if no_ar {
            let _ = Command::new("kill").args(["-TERM", &filho.id().to_string()]).status();
        }
        let prazo = Instant::now() + Duration::from_secs(15);
        let status = loop {
            if let Some(s) = filho.try_wait().unwrap() {
                break s;
            }
            if Instant::now() > prazo {
                let _ = filho.kill();
                break filho.wait().unwrap();
            }
            std::thread::sleep(Duration::from_millis(50));
        };
        (status.code(), std::fs::read_to_string(&log).unwrap())
    }

    /// Sobe e para, e exige que tenha subido e saído limpo.
    fn subir_ok(&self, bin: &str, chave: Option<&str>) {
        let (codigo, log) = self.subir_e_parar(bin, chave);
        assert_eq!(codigo, Some(0), "{bin} serve:\n{log}");
        assert!(log.contains("\"msg\":\"patchbay escutando\""), "{bin} serve:\n{log}");
    }
}

impl Drop for Cena {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// `GET /saude` cru, sem cliente HTTP.
fn saude(porta: u16) -> bool {
    use std::io::{Read, Write};
    let Ok(mut s) = std::net::TcpStream::connect(("127.0.0.1", porta)) else { return false };
    let _ = s.write_all(b"GET /saude HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
    let mut r = String::new();
    let _ = s.read_to_string(&mut r);
    r.starts_with("HTTP/1.1 200") && r.ends_with("\r\n\r\nok\n")
}

fn ler(banco: &Path) -> rusqlite::Connection {
    rusqlite::Connection::open_with_flags(banco, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY).unwrap()
}

/// `goose_db_version` inteira, instante incluso.
fn versoes(banco: &Path) -> Vec<(i64, i64, i64, String)> {
    let c = ler(banco);
    let mut q = c.prepare("SELECT id, version_id, is_applied, tstamp FROM goose_db_version ORDER BY id").unwrap();
    q.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?))).unwrap().map(Result::unwrap).collect()
}

/// Todo objeto do esquema com o SQL de criação.
fn esquema(banco: &Path) -> Vec<(String, String, Option<String>)> {
    let c = ler(banco);
    let mut q = c.prepare("SELECT type, name, sql FROM sqlite_master ORDER BY type, name").unwrap();
    q.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?))).unwrap().map(Result::unwrap).collect()
}

fn canario(banco: &Path) -> String {
    ler(banco).query_row("SELECT valor FROM settings WHERE chave = 'canario_chave_mestra'", [], |r| r.get(0)).unwrap()
}

fn sem_tstamp(v: Vec<(i64, i64, i64, String)>) -> Vec<(i64, i64, i64)> {
    v.into_iter().map(|(a, b, c, _)| (a, b, c)).collect()
}

/// O banco que o Go criou e migrou: o Rust sobe nele sem tocar em `goose_db_version` nem no
/// canário, e o Go sobe de novo depois.
#[test]
fn banco_do_go_sobe_no_rust_sem_mudar_versoes() {
    let Some(go) = go() else { return };
    let cena = Cena::new();
    cena.subir_ok(&go, Some(CHAVE));
    let (v, c) = (versoes(&cena.banco()), canario(&cena.banco()));
    cena.subir_ok(RUST, Some(CHAVE));
    assert_eq!(versoes(&cena.banco()), v, "o Rust mexeu em goose_db_version");
    assert_eq!(canario(&cena.banco()), c, "o Rust regravou o canário");
    cena.subir_ok(&go, Some(CHAVE));
    assert_eq!(versoes(&cena.banco()), v);
}

/// O banco que o Rust criou tem o mesmo esquema e as mesmas versões do criado pelo Go, e o Go sobe
/// nele com o canário válido (com canário inválido ele se recusaria).
#[test]
fn banco_novo_do_rust_sobe_no_go() {
    let Some(go) = go() else { return };
    let (do_rust, do_go) = (Cena::new(), Cena::new());
    do_rust.subir_ok(RUST, Some(CHAVE));
    do_go.subir_ok(&go, Some(CHAVE));
    assert_eq!(esquema(&do_rust.banco()), esquema(&do_go.banco()), "esquema diferente");
    let v = versoes(&do_rust.banco());
    assert_eq!(sem_tstamp(v.clone()), sem_tstamp(versoes(&do_go.banco())), "versões diferentes");
    let c = canario(&do_rust.banco());
    do_rust.subir_ok(&go, Some(CHAVE));
    assert_eq!(versoes(&do_rust.banco()), v, "o Go mexeu em goose_db_version");
    assert_eq!(canario(&do_rust.banco()), c, "o Go regravou o canário");
}

/// A chave que um binário gera o outro aceita, nos dois sentidos, sobre o mesmo banco.
#[test]
fn chave_gerada_por_um_vale_no_outro() {
    let Some(go) = go() else { return };
    for (gera, primeiro, segundo) in [(RUST, go.as_str(), RUST), (go.as_str(), RUST, go.as_str())] {
        let cena = Cena::new();
        let s = cena.rodar(gera, &["chave-mestra", "gerar"], None);
        assert_eq!(s.status.code(), Some(0));
        let chave = String::from_utf8(s.stdout).unwrap();
        let chave = chave.trim_end_matches('\n');
        assert!(!chave.contains('\n') && chave.len() == 44, "chave de {gera}: {chave:?}");
        cena.subir_ok(primeiro, Some(chave));
        cena.subir_ok(segundo, Some(chave));
    }
}

/// Sem `PATCHBAY_MASTER_KEY`, a chave nasce em `<dados>/chave-mestra`; o outro binário lê o mesmo
/// arquivo e o canário confere.
#[test]
fn chave_no_arquivo_vale_nos_dois() {
    let Some(go) = go() else { return };
    for (primeiro, segundo) in [(RUST, go.as_str()), (go.as_str(), RUST)] {
        let cena = Cena::new();
        cena.subir_ok(primeiro, None);
        let arquivo = std::fs::read_to_string(cena.dados().join("chave-mestra")).unwrap();
        cena.subir_ok(segundo, None);
        assert_eq!(std::fs::read_to_string(cena.dados().join("chave-mestra")).unwrap(), arquivo);
    }
}

/// Com a chave trocada, os dois se recusam a subir com a mesma mensagem, sobre banco de qualquer um.
#[test]
fn chave_trocada_recusada_igual_nos_dois() {
    let Some(go) = go() else { return };
    let outra = Cena::new().rodar(RUST, &["chave-mestra", "gerar"], None).stdout;
    let outra = String::from_utf8(outra).unwrap();
    for criador in [RUST, go.as_str()] {
        let cena = Cena::new();
        cena.subir_ok(criador, Some(CHAVE));
        let recusa = |bin: &str| {
            let (codigo, log) = cena.subir_e_parar(bin, Some(outra.trim()));
            assert_eq!(codigo, Some(1), "{bin} subiu com a chave errada:\n{log}");
            // A mensagem final, sem as linhas de log que o Go escreve antes.
            log[log.find("patchbay: ").unwrap_or_else(|| panic!("{bin}: sem mensagem:\n{log}"))..].to_string()
        };
        let (do_go, do_rust) = (recusa(&go), recusa(RUST));
        assert!(do_go.starts_with("patchbay: cripto: o canário não confere"), "{do_go}");
        assert_eq!(do_rust, do_go);
    }
}
