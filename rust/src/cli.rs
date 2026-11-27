//! As flags dos subcomandos com a semântica do pacote `flag` do Go, que é o contrato de quem já
//! usa o binário: `-x`, `--x`, `-x=v`, `-x v`; booleano só aceita valor com `=`; para no primeiro
//! argumento que não é flag e em `--`; `-h`/`-help` imprime o uso e sai com 2. Os textos de erro e
//! de uso são os do Go (`flag provided but not defined: -x`, `Usage of patchbay serve:`).

use std::collections::BTreeMap;
use std::fmt;

/// O pedido de ajuda (`-h`): o processo sai com 2 sem imprimir erro, como o `flag.ErrHelp`.
#[derive(Debug)]
pub struct PedidoDeAjuda;

impl fmt::Display for PedidoDeAjuda {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("flag: help requested")
    }
}

impl std::error::Error for PedidoDeAjuda {}

#[derive(Clone, Copy, PartialEq)]
enum Tipo {
    Texto,
    Booleano,
    Inteiro,
}

struct Definicao {
    tipo: Tipo,
    padrao: String,
    uso: &'static str,
}

/// Um conjunto de flags, como o `flag.FlagSet` com `ContinueOnError`.
pub struct Flags {
    nome: String,
    definicoes: BTreeMap<&'static str, Definicao>,
    valores: BTreeMap<&'static str, String>,
}

impl Flags {
    pub fn new(nome: impl Into<String>) -> Self {
        Flags { nome: nome.into(), definicoes: BTreeMap::new(), valores: BTreeMap::new() }
    }

    fn definir(&mut self, nome: &'static str, tipo: Tipo, padrao: String, uso: &'static str) {
        self.valores.insert(nome, padrao.clone());
        self.definicoes.insert(nome, Definicao { tipo, padrao, uso });
    }

    pub fn texto(&mut self, nome: &'static str, padrao: impl Into<String>, uso: &'static str) {
        self.definir(nome, Tipo::Texto, padrao.into(), uso);
    }

    pub fn booleano(&mut self, nome: &'static str, padrao: bool, uso: &'static str) {
        self.definir(nome, Tipo::Booleano, padrao.to_string(), uso);
    }

    pub fn inteiro(&mut self, nome: &'static str, padrao: i64, uso: &'static str) {
        self.definir(nome, Tipo::Inteiro, padrao.to_string(), uso);
    }

    /// O valor de uma flag de texto (definida; o contrário é erro de programação).
    pub fn ler_texto(&self, nome: &str) -> String {
        self.valores[nome].clone()
    }

    pub fn ler_booleano(&self, nome: &str) -> bool {
        self.valores[nome] == "true"
    }

    pub fn ler_inteiro(&self, nome: &str) -> i64 {
        self.valores[nome].parse().expect("validado na análise")
    }

    /// Analisa `args` e devolve o que sobrou depois da última flag. No erro, imprime a mensagem
    /// e o uso na saída de erro, como o Go, e devolve o erro para quem chamou embrulhar.
    pub fn analisar(&mut self, args: &[String]) -> anyhow::Result<Vec<String>> {
        let mut resto = args;
        while let Some(s) = resto.first() {
            if s.len() < 2 || !s.starts_with('-') {
                break;
            }
            let menos = if s.as_bytes()[1] == b'-' { 2 } else { 1 };
            if s == "--" {
                resto = &resto[1..];
                break;
            }
            let nome = &s[menos..];
            if nome.is_empty() || nome.starts_with(['-', '=']) {
                return Err(self.falha(format!("bad flag syntax: {s}")));
            }
            resto = &resto[1..];
            let (nome, valor) = match nome.split_once('=') {
                Some((n, v)) => (n, Some(v.to_string())),
                None => (nome, None),
            };
            let Some((&nome, def)) = self.definicoes.get_key_value(nome) else {
                if nome == "help" || nome == "h" {
                    eprint!("{}", self.uso());
                    return Err(PedidoDeAjuda.into());
                }
                return Err(self.falha(format!("flag provided but not defined: -{nome}")));
            };
            let valor = match (def.tipo, valor) {
                (Tipo::Booleano, None) => "true".to_string(),
                (Tipo::Booleano, Some(v)) => match booleano_go(&v) {
                    Some(b) => b.to_string(),
                    None => return Err(self.falha(format!("invalid boolean value {v:?} for -{nome}: parse error"))),
                },
                (tipo, valor) => {
                    let v = match valor {
                        Some(v) => v,
                        None => match resto.split_first() {
                            Some((v, r)) => {
                                resto = r;
                                v.clone()
                            }
                            None => return Err(self.falha(format!("flag needs an argument: -{nome}"))),
                        },
                    };
                    if tipo == Tipo::Inteiro {
                        // shortcut: o Go lê com base 0 (aceita 0x, 0o, _); aqui só decimal, que é o
                        // que as flags do patchbay (timeout em ms) usam.
                        match v.parse::<i64>() {
                            Ok(n) => n.to_string(),
                            Err(e) => {
                                let porque =
                                    if matches!(e.kind(), std::num::IntErrorKind::PosOverflow | std::num::IntErrorKind::NegOverflow) {
                                        "value out of range"
                                    } else {
                                        "parse error"
                                    };
                                return Err(self.falha(format!("invalid value {v:?} for flag -{nome}: {porque}")));
                            }
                        }
                    } else {
                        v
                    }
                }
            };
            self.valores.insert(nome, valor);
        }
        Ok(resto.to_vec())
    }

    fn falha(&self, msg: String) -> anyhow::Error {
        eprintln!("{msg}");
        eprint!("{}", self.uso());
        anyhow::anyhow!(msg)
    }

    /// O texto do `flag.PrintDefaults`, com o cabeçalho `Usage of <nome>:`.
    fn uso(&self) -> String {
        let mut s = format!("Usage of {}:\n", self.nome);
        for (nome, d) in &self.definicoes {
            let mut linha = format!("  -{nome}");
            match d.tipo {
                Tipo::Texto => linha += " string",
                Tipo::Inteiro => linha += " int",
                Tipo::Booleano => {}
            }
            // Flag de uma letra sem tipo fica na mesma linha do uso, como no Go.
            linha += if linha.len() <= 4 { "\t" } else { "\n    \t" };
            linha += &d.uso.replace('\n', "\n    \t");
            let zero = match d.tipo {
                Tipo::Texto => "",
                Tipo::Booleano => "false",
                Tipo::Inteiro => "0",
            };
            if d.padrao != zero {
                if d.tipo == Tipo::Texto {
                    linha += &format!(" (default {:?})", d.padrao);
                } else {
                    linha += &format!(" (default {})", d.padrao);
                }
            }
            s += &linha;
            s.push('\n');
        }
        s
    }
}

/// `strconv.ParseBool`.
fn booleano_go(v: &str) -> Option<bool> {
    match v {
        "1" | "t" | "T" | "TRUE" | "true" | "True" => Some(true),
        "0" | "f" | "F" | "FALSE" | "false" | "False" => Some(false),
        _ => None,
    }
}

#[cfg(test)]
mod testes {
    use super::*;

    fn flags() -> Flags {
        let mut f = Flags::new("patchbay teste");
        f.texto("listen", "127.0.0.1:8787", "endereço de escuta (PATCHBAY_LISTEN)");
        f.booleano("log-texto", false, "log em texto em vez de JSON (PATCHBAY_LOG_TEXTO)");
        f.inteiro("timeout-ms", 15000, "timeout de toda operação daquele upstream");
        f.texto("o", "", "arquivo de destino");
        f
    }

    fn args(a: &[&str]) -> Vec<String> {
        a.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn formas_aceitas_como_no_go() {
        let mut f = flags();
        let resto =
            f.analisar(&args(&["--listen", ":9", "-log-texto", "-timeout-ms=3000", "-o=x.yaml", "arquivo", "-listen", "y"])).unwrap();
        assert_eq!(f.ler_texto("listen"), ":9");
        assert!(f.ler_booleano("log-texto"));
        assert_eq!(f.ler_inteiro("timeout-ms"), 3000);
        assert_eq!(f.ler_texto("o"), "x.yaml");
        assert_eq!(resto, args(&["arquivo", "-listen", "y"]), "para no primeiro posicional");

        let mut f = flags();
        assert_eq!(f.analisar(&args(&["--", "-listen"])).unwrap(), args(&["-listen"]));
        let mut f = flags();
        assert_eq!(f.analisar(&args(&["-", "x"])).unwrap(), args(&["-", "x"]), "- sozinho é posicional");
        let mut f = flags();
        f.analisar(&args(&["-log-texto=false"])).unwrap();
        assert!(!f.ler_booleano("log-texto"));
    }

    #[test]
    fn erros_com_o_texto_do_go() {
        let erro = |a: &[&str]| flags().analisar(&args(a)).unwrap_err().to_string();
        assert_eq!(erro(&["-nada"]), "flag provided but not defined: -nada");
        assert_eq!(erro(&["---x"]), "bad flag syntax: ---x");
        assert_eq!(erro(&["-=x"]), "bad flag syntax: -=x");
        assert_eq!(erro(&["-listen"]), "flag needs an argument: -listen");
        assert_eq!(erro(&["-log-texto=talvez"]), r#"invalid boolean value "talvez" for -log-texto: parse error"#);
        assert_eq!(erro(&["-timeout-ms", "abc"]), r#"invalid value "abc" for flag -timeout-ms: parse error"#);
        assert_eq!(
            erro(&["-timeout-ms=99999999999999999999"]),
            r#"invalid value "99999999999999999999" for flag -timeout-ms: value out of range"#
        );
        assert!(flags().analisar(&args(&["-h"])).unwrap_err().is::<PedidoDeAjuda>());
        assert!(flags().analisar(&args(&["--help"])).unwrap_err().is::<PedidoDeAjuda>());
    }

    #[test]
    fn uso_como_o_print_defaults() {
        assert_eq!(
            flags().uso(),
            "Usage of patchbay teste:
  -listen string
    \tendereço de escuta (PATCHBAY_LISTEN) (default \"127.0.0.1:8787\")
  -log-texto
    \tlog em texto em vez de JSON (PATCHBAY_LOG_TEXTO)
  -o string
    \tarquivo de destino
  -timeout-ms int
    \ttimeout de toda operação daquele upstream (default 15000)
"
        );
    }
}
