//! O binário Rust como processo, no que só aparece de fora: o que outro processo do mesmo usuário
//! enxerga dele, o comportamento sem descritores livres e duas instâncias subindo juntas.

use std::net::TcpStream;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

const CHAVE: &str = "cGF0Y2hiYXktY2FpeGEtcHJldGEtY2hhdmUtZml4YSE=";
const RUST: &str = env!("CARGO_BIN_EXE_patchbay");

struct Dir(PathBuf);

impl Dir {
    fn new() -> Self {
        static N: AtomicUsize = AtomicUsize::new(0);
        let d = std::env::temp_dir().join(format!("patchbay-processo-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        Dir(d)
    }
}

impl Drop for Dir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn porta_livre() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port()
}

/// `serve` com o ambiente limpo; `prefixo` vai antes do binário num `sh -c` (para o `ulimit`).
fn subir(dir: &Path, porta: u16, log: &str, prefixo: &str) -> Child {
    let arquivo = std::fs::File::create(dir.join(log)).unwrap();
    Command::new("sh")
        .arg("-c")
        .arg(format!("{prefixo} exec \"$0\" serve"))
        .arg(RUST)
        .env_clear()
        .env("PATH", "/usr/bin:/bin")
        .env("PATCHBAY_DATA_DIR", dir.join("dados"))
        .env("PATCHBAY_LISTEN", format!("127.0.0.1:{porta}"))
        .env("PATCHBAY_MASTER_KEY", CHAVE)
        .stdin(Stdio::null())
        .stdout(arquivo.try_clone().unwrap())
        .stderr(arquivo)
        .spawn()
        .unwrap()
}

fn saude(porta: u16) -> bool {
    use std::io::{Read, Write};
    let Ok(mut s) = TcpStream::connect(("127.0.0.1", porta)) else { return false };
    let _ = s.set_read_timeout(Some(Duration::from_secs(2)));
    let _ = s.write_all(b"GET /saude HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
    let mut r = String::new();
    let _ = s.read_to_string(&mut r);
    r.ends_with("\r\n\r\nok\n")
}

fn esperar_saude(porta: u16, segundos: u64) -> bool {
    let prazo = Instant::now() + Duration::from_secs(segundos);
    while Instant::now() < prazo {
        if saude(porta) {
            return true;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    false
}

/// SIGTERM e a espera da saída limpa.
fn parar(mut filho: Child) -> Option<i32> {
    let _ = Command::new("kill").args(["-TERM", &filho.id().to_string()]).status();
    let prazo = Instant::now() + Duration::from_secs(15);
    while Instant::now() < prazo {
        if let Some(s) = filho.try_wait().unwrap() {
            return s.code();
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    let _ = filho.kill();
    filho.wait().unwrap().code()
}

/// Item 1 da revisão: outro processo do mesmo uid (um MCP stdio de terceiro, por exemplo) não lê
/// a chave mestra em `/proc/<pid>/environ` do patchbay no ar.
#[cfg(target_os = "linux")]
#[test]
fn chave_mestra_fora_do_proc_environ() {
    let d = Dir::new();
    let porta = porta_livre();
    let filho = subir(&d.0, porta, "log", "");
    assert!(esperar_saude(porta, 10), "não subiu");
    let pid = filho.id();
    let lido = Command::new("cat").arg(format!("/proc/{pid}/environ")).env("LC_ALL", "C").output().unwrap();
    let do_mem = Command::new("dd").args([&format!("if=/proc/{pid}/mem"), "bs=1", "count=1", "status=none"]).output().unwrap();
    assert_eq!(parar(filho), Some(0));
    let texto = String::from_utf8_lossy(&lido.stdout);
    assert!(!texto.contains(CHAVE), "a chave mestra está no /proc/{pid}/environ");
    // Não dumpable: o arquivo passa a ser do root, e a leitura nem abre.
    assert!(!lido.status.success(), "/proc/{pid}/environ abriu: {texto:?}");
    assert!(String::from_utf8_lossy(&lido.stderr).contains("Permission denied"), "{lido:?}");
    assert!(!do_mem.status.success(), "/proc/{pid}/mem abriu");
}

fn tempo_de_cpu(pid: u32) -> u64 {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).unwrap();
    // Os campos depois do nome entre parênteses: utime é o 14º e stime o 15º da linha toda.
    let depois = &stat[stat.rfind(')').unwrap() + 2..];
    let campos: Vec<&str> = depois.split(' ').collect();
    campos[11].parse::<u64>().unwrap() + campos[12].parse::<u64>().unwrap()
}

/// Item 2: sem descritor livre (EMFILE), o accept espera em vez de girar a 100% de CPU, o aviso
/// sai poucas vezes, e o servidor volta a atender quando os descritores voltam.
#[cfg(target_os = "linux")]
#[test]
fn accept_sem_descritor_nao_gira() {
    let d = Dir::new();
    let porta = porta_livre();
    let filho = subir(&d.0, porta, "log", "ulimit -n 64 &&");
    assert!(esperar_saude(porta, 10), "não subiu");
    let pid = filho.id();
    // Mais conexões que descritores: as que sobram ficam na fila do kernel, e o accept dá EMFILE.
    let abertas: Vec<TcpStream> = (0..150).filter_map(|_| TcpStream::connect(("127.0.0.1", porta)).ok()).collect();
    assert!(abertas.len() > 64, "só {} conexões", abertas.len());
    std::thread::sleep(Duration::from_millis(500));
    let antes = tempo_de_cpu(pid);
    std::thread::sleep(Duration::from_secs(2));
    let gasto = tempo_de_cpu(pid) - antes;
    let log = std::fs::read_to_string(d.0.join("log")).unwrap();
    let avisos = log.lines().filter(|l| l.contains("\"msg\":\"aceitar conexão\"")).count();
    drop(abertas);
    let voltou = esperar_saude(porta, 10);
    assert_eq!(parar(filho), Some(0));
    // Em 2 s, girando, seriam ~200 tiques de CPU (a 100 Hz).
    assert!(gasto < 40, "{gasto} tiques de CPU em 2 s sem descritor livre");
    assert!(log.contains("Too many open files"), "não chegou a faltar descritor:\n{log}");
    assert!((1..=2).contains(&avisos), "{avisos} avisos de accept em ~2,5 s");
    assert!(voltou, "não voltou a atender depois de liberar os descritores");
}

/// Item 5: duas instâncias subindo juntas no mesmo diretório vazio sobem as duas, e o banco fica
/// com uma linha por versão e um canário só.
#[test]
fn duas_instancias_juntas_migram_uma_vez() {
    for _ in 0..3 {
        let d = Dir::new();
        let (p1, p2) = (porta_livre(), porta_livre());
        let (a, b) = (subir(&d.0, p1, "log-a", ""), subir(&d.0, p2, "log-b", ""));
        let (ok_a, ok_b) = (esperar_saude(p1, 15), esperar_saude(p2, 15));
        let (sa, sb) = (parar(a), parar(b));
        let logs =
            format!("{}\n{}", std::fs::read_to_string(d.0.join("log-a")).unwrap(), std::fs::read_to_string(d.0.join("log-b")).unwrap());
        assert!(ok_a && ok_b && sa == Some(0) && sb == Some(0), "uma não subiu:\n{logs}");
        let c = rusqlite::Connection::open_with_flags(d.0.join("dados/patchbay.db"), rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY).unwrap();
        let versoes: Vec<i64> = c
            .prepare("SELECT version_id FROM goose_db_version ORDER BY id")
            .unwrap()
            .query_map([], |r| r.get(0))
            .unwrap()
            .map(Result::unwrap)
            .collect();
        assert_eq!(versoes, [0, 1, 2, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15, 16, 17, 18]);
        let canarios: i64 = c.query_row("SELECT count(*) FROM settings WHERE chave = 'canario_chave_mestra'", [], |r| r.get(0)).unwrap();
        assert_eq!(canarios, 1);
        assert_eq!(logs.matches("canário da chave mestra gravado no primeiro boot").count(), 1, "{logs}");
    }
}
