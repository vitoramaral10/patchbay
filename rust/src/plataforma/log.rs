//! O log do processo no formato do `log/slog` do Go: uma linha JSON por evento
//! (`{"time":…,"level":"INFO","msg":…,<campos>}`) ou, com `PATCHBAY_LOG_TEXTO`, o formato do
//! `TextHandler` (`time=… level=INFO msg=… chave=valor`). Quem lê o log de produção (e a caixa-preta)
//! não distingue os dois binários.
//!
//! Só entra o que o próprio patchbay loga (alvo `patchbay::…`): o Go não repassa o log interno do
//! driver SQLite nem do HTTP, e o sqlx loga cada consulta.

use std::fmt::{self, Write as _};
use std::io::Write;
use std::sync::{Arc, Mutex};

use tracing::field::{Field, Visit};
use tracing::{Event, Level, Metadata, Subscriber};
use tracing_subscriber::layer::{Context, Layer, SubscriberExt};

/// O nível numérico do slog (debug -4, info 0, warn 4, error 8).
pub fn nivel_go(l: &Level) -> i64 {
    match *l {
        Level::TRACE => -8,
        Level::DEBUG => -4,
        Level::INFO => 0,
        Level::WARN => 4,
        Level::ERROR => 8,
    }
}

/// `slog.Level.UnmarshalText`: nome sem caixa, com deslocamento opcional (`info+2`, `DEBUG-4`).
pub fn ler_nivel(s: &str) -> Result<i64, String> {
    let erro = |e: &str| format!("slog: level string {s:?}: {e}");
    let (nome, deslocamento) = match s.find(['+', '-']) {
        Some(i) => {
            let d = s[i..].parse::<i64>().map_err(|_| erro(&format!("strconv.Atoi: parsing {:?}: invalid syntax", &s[i..])))?;
            (&s[..i], d)
        }
        None => (s, 0),
    };
    let base = match nome.to_uppercase().as_str() {
        "DEBUG" => -4,
        "INFO" => 0,
        "WARN" => 4,
        "ERROR" => 8,
        _ => return Err(erro("unknown name")),
    };
    Ok(base + deslocamento)
}

/// O nome do nível como o slog imprime (`INFO`, `WARN+2`...).
fn nome_do_nivel(n: i64) -> String {
    let (base, d) = match n {
        ..0 => ("DEBUG", n + 4),
        0..4 => ("INFO", n),
        4..8 => ("WARN", n - 4),
        _ => ("ERROR", n - 8),
    };
    if d == 0 { base.to_string() } else { format!("{base}{d:+}") }
}

/// Instala o log do processo na saída de erro. Chamado uma vez, depois de ler a configuração.
pub fn iniciar(nivel: i64, em_texto: bool) {
    let camada = CamadaGo { nivel, em_texto, saida: Arc::new(Mutex::new(Box::new(std::io::stderr()))) };
    let _ = tracing::subscriber::set_global_default(tracing_subscriber::registry().with(camada));
}

type Saida = Arc<Mutex<Box<dyn Write + Send>>>;

struct CamadaGo {
    nivel: i64,
    em_texto: bool,
    saida: Saida,
}

/// Um valor de campo com o tipo que o slog distingue na saída.
enum Valor {
    Texto(String),
    Numero(String),
    Booleano(bool),
}

#[derive(Default)]
struct Campos {
    msg: String,
    campos: Vec<(&'static str, Valor)>,
}

impl Campos {
    fn por(&mut self, campo: &Field, v: Valor) {
        match (campo.name(), v) {
            ("message", Valor::Texto(t)) => self.msg = t,
            (nome, v) => self.campos.push((nome, v)),
        }
    }
}

impl Visit for Campos {
    fn record_str(&mut self, campo: &Field, v: &str) {
        self.por(campo, Valor::Texto(v.to_string()));
    }
    fn record_i64(&mut self, campo: &Field, v: i64) {
        self.por(campo, Valor::Numero(v.to_string()));
    }
    fn record_u64(&mut self, campo: &Field, v: u64) {
        self.por(campo, Valor::Numero(v.to_string()));
    }
    fn record_f64(&mut self, campo: &Field, v: f64) {
        self.por(campo, Valor::Numero(v.to_string()));
    }
    fn record_bool(&mut self, campo: &Field, v: bool) {
        self.por(campo, Valor::Booleano(v));
    }
    fn record_error(&mut self, campo: &Field, v: &(dyn std::error::Error + 'static)) {
        self.por(campo, Valor::Texto(v.to_string()));
    }
    fn record_debug(&mut self, campo: &Field, v: &dyn fmt::Debug) {
        self.por(campo, Valor::Texto(format!("{v:?}")));
    }
}

impl<S: Subscriber> Layer<S> for CamadaGo {
    fn enabled(&self, meta: &Metadata<'_>, _: Context<'_, S>) -> bool {
        let nosso = meta.target() == "patchbay" || meta.target().starts_with("patchbay::");
        nosso && nivel_go(meta.level()) >= self.nivel
    }

    fn on_event(&self, evento: &Event<'_>, _: Context<'_, S>) {
        let mut c = Campos::default();
        evento.record(&mut c);
        let nivel = nome_do_nivel(nivel_go(evento.metadata().level()));
        let agora = jiff::ZonedDateTime::now();
        let linha = if self.em_texto { texto(&agora, &nivel, &c) } else { json(&agora, &nivel, &c) };
        if let Ok(mut s) = self.saida.lock() {
            let _ = s.write_all(linha.as_bytes());
        }
    }
}

fn json(agora: &jiff::ZonedDateTime, nivel: &str, c: &Campos) -> String {
    let mut s = String::from("{\"time\":\"");
    s += &instante(agora, false);
    s += "\",\"level\":";
    string_json(&mut s, nivel);
    s += ",\"msg\":";
    string_json(&mut s, &c.msg);
    for (k, v) in &c.campos {
        s.push(',');
        string_json(&mut s, k);
        s.push(':');
        match v {
            Valor::Texto(t) => string_json(&mut s, t),
            Valor::Numero(n) => s += n,
            Valor::Booleano(b) => s += if *b { "true" } else { "false" },
        }
    }
    s += "}\n";
    s
}

fn texto(agora: &jiff::ZonedDateTime, nivel: &str, c: &Campos) -> String {
    let mut s = format!("time={} level={nivel} msg=", instante(agora, true));
    valor_texto(&mut s, &c.msg);
    for (k, v) in &c.campos {
        s.push(' ');
        valor_texto(&mut s, k);
        s.push('=');
        match v {
            Valor::Texto(t) => valor_texto(&mut s, t),
            Valor::Numero(n) => s += n,
            Valor::Booleano(b) => s += if *b { "true" } else { "false" },
        }
    }
    s.push('\n');
    s
}

/// RFC 3339 no fuso local como o Go: no JSON, `RFC3339Nano` (fração sem zeros à direita, omitida
/// se zero); no texto, milissegundos com três dígitos sempre. Fuso zero vira `Z`.
fn instante(t: &jiff::ZonedDateTime, milis: bool) -> String {
    let mut s = t.strftime("%Y-%m-%dT%H:%M:%S").to_string();
    let nanos = t.subsec_nanosecond();
    if milis {
        let _ = write!(s, ".{:03}", nanos / 1_000_000);
    } else if nanos != 0 {
        let fracao = format!("{nanos:09}");
        s.push('.');
        s += fracao.trim_end_matches('0');
    }
    let seg = t.offset().seconds();
    if seg == 0 {
        s.push('Z');
    } else {
        let a = seg.unsigned_abs();
        let _ = write!(s, "{}{:02}:{:02}", if seg < 0 { '-' } else { '+' }, a / 3600, a / 60 % 60);
    }
    s
}

/// A string JSON do slog: escapa `"`, `\`, `\n`, `\r`, `\t` curtos, outros controles em `\u00XX`,
/// e U+2028/U+2029; `<`, `>` e `&` passam como estão (diferente do `encoding/json`).
fn string_json(s: &mut String, v: &str) {
    s.push('"');
    for ch in v.chars() {
        match ch {
            '"' => s.push_str("\\\""),
            '\\' => s.push_str("\\\\"),
            '\n' => s.push_str("\\n"),
            '\r' => s.push_str("\\r"),
            '\t' => s.push_str("\\t"),
            '\u{2028}' | '\u{2029}' => {
                let _ = write!(s, "\\u{:04x}", ch as u32);
            }
            c if (c as u32) < 0x20 => {
                let _ = write!(s, "\\u00{:02x}", c as u32);
            }
            c => s.push(c),
        }
    }
    s.push('"');
}

/// O valor do `TextHandler`: entre aspas (como `strconv.Quote`) se vazio ou se tem espaço, `=`,
/// aspas ou caractere de controle.
fn valor_texto(s: &mut String, v: &str) {
    let aspas = v.is_empty() || v.chars().any(|c| c == ' ' || c == '=' || c == '"' || c.is_whitespace() || c.is_control());
    if aspas {
        // shortcut: o Debug do Rust escapa controle como \u{1b} e o strconv.Quote do Go como \x1b;
        // só difere em valor com caractere de controle, que o patchbay não loga.
        let _ = write!(s, "{v:?}");
    } else {
        s.push_str(v);
    }
}

/// Roda `f` com o log no formato JSON em nível debug e devolve o que saiu. Só para testes.
#[cfg(test)]
pub fn capturar(f: impl FnOnce()) -> String {
    #[derive(Clone)]
    struct Buffer(Arc<Mutex<Vec<u8>>>);
    impl Write for Buffer {
        fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    let buffer = Buffer(Arc::default());
    let camada = CamadaGo { nivel: -4, em_texto: false, saida: Arc::new(Mutex::new(Box::new(buffer.clone()))) };
    tracing::subscriber::with_default(tracing_subscriber::registry().with(camada), f);
    String::from_utf8(buffer.0.lock().unwrap().clone()).unwrap()
}

#[cfg(test)]
mod testes {
    use super::*;

    #[test]
    fn niveis_como_o_slog() {
        assert_eq!(ler_nivel("debug"), Ok(-4));
        assert_eq!(ler_nivel("Warn"), Ok(4));
        assert_eq!(ler_nivel("info+2"), Ok(2));
        assert_eq!(ler_nivel("ERROR-1"), Ok(7));
        assert_eq!(ler_nivel("verbose").unwrap_err(), r#"slog: level string "verbose": unknown name"#);
        assert_eq!(ler_nivel("info+x").unwrap_err(), r#"slog: level string "info+x": strconv.Atoi: parsing "+x": invalid syntax"#);
        assert_eq!(nome_do_nivel(2), "INFO+2");
        assert_eq!(nome_do_nivel(-4), "DEBUG");
        assert_eq!(nome_do_nivel(8), "ERROR");
    }

    #[test]
    fn linha_json_como_a_do_go() {
        let saida = capturar(|| tracing::info!(listen = "127.0.0.1:8787", n = 3, ok = true, erro = %"a\"b<c>\n", "patchbay escutando"));
        let (inicio, resto) = saida.split_once("\",\"level\"").unwrap();
        assert!(inicio.starts_with("{\"time\":\"20"), "{saida}");
        assert_eq!(
            resto,
            r#":"INFO","msg":"patchbay escutando","listen":"127.0.0.1:8787","n":3,"ok":true,"erro":"a\"b<c>\n"}"#.to_string() + "\n"
        );
    }

    #[test]
    fn instantes_nos_dois_formatos() {
        let t: jiff::ZonedDateTime = "2026-11-27T19:43:10.120000000-03:00[-03:00]".parse().unwrap();
        assert_eq!(instante(&t, false), "2026-11-27T19:43:10.12-03:00");
        assert_eq!(instante(&t, true), "2026-11-27T19:43:10.120-03:00");
        let utc: jiff::ZonedDateTime = "2026-11-27T22:43:10Z[UTC]".parse().unwrap();
        assert_eq!(instante(&utc, false), "2026-11-27T22:43:10Z");
    }

    #[test]
    fn valores_do_texto() {
        let mut s = String::new();
        valor_texto(&mut s, "canário gravado");
        s.push(' ');
        valor_texto(&mut s, "simples");
        s.push(' ');
        valor_texto(&mut s, "");
        assert_eq!(s, r#""canário gravado" simples """#);
    }
}
