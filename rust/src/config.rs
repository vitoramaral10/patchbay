//! A configuração de bootstrap (`cmd/patchbay/config.go`): flags e variáveis de ambiente lidas uma
//! vez e passadas adiante como struct. O YAML é artefato de export/import e nunca é lido no boot.

use crate::cli::Flags;
use crate::plataforma::log;

/// Padrões de desenvolvimento: só loopback, para a instalação nova não ficar exposta.
const LISTEN_PADRAO: &str = "127.0.0.1:8787";
const DATA_DIR_PADRAO: &str = "./dados";

#[derive(Debug, Clone)]
pub struct Config {
    pub listen: String,
    pub data_dir: String,
    pub public_url: String,
    /// Se a URL veio de flag ou ambiente. Sem ela, vale a gravada em Ajustes (`url_publica`), e na
    /// falta dessa, a da escuta.
    pub public_url_explicita: bool,
    /// O nível numérico do slog.
    pub nivel_log: i64,
    pub log_em_texto: bool,
}

/// O valor da variável, ou o padrão quando ela não existe ou está vazia.
fn ambiente(chave: &str, padrao: &str) -> String {
    std::env::var(chave).ok().filter(|v| !v.is_empty()).unwrap_or_else(|| padrao.to_string())
}

/// Declara as flags de configuração num conjunto que pode já ter as flags de um subcomando.
pub fn registrar(f: &mut Flags) {
    f.texto("listen", ambiente("PATCHBAY_LISTEN", LISTEN_PADRAO), "endereço de escuta (PATCHBAY_LISTEN)");
    f.texto("data-dir", ambiente("PATCHBAY_DATA_DIR", DATA_DIR_PADRAO), "diretório do banco SQLite (PATCHBAY_DATA_DIR)");
    f.texto("public-url", ambiente("PATCHBAY_PUBLIC_URL", ""), "URL pública do patchbay, sem barra final (PATCHBAY_PUBLIC_URL)");
    f.texto("log-level", ambiente("PATCHBAY_LOG_LEVEL", "info"), "nível de log: debug, info, warn, error (PATCHBAY_LOG_LEVEL)");
    f.booleano("log-texto", !ambiente("PATCHBAY_LOG_TEXTO", "").is_empty(), "log em texto em vez de JSON (PATCHBAY_LOG_TEXTO)");
}

/// Resolve a configuração depois da análise.
pub fn resolver(f: &Flags) -> anyhow::Result<Config> {
    let nivel = f.ler_texto("log-level");
    let nivel_log = log::ler_nivel(&nivel).map_err(|e| anyhow::anyhow!("config: nível de log {nivel:?}: {e}"))?;
    let mut c = Config {
        listen: f.ler_texto("listen"),
        data_dir: f.ler_texto("data-dir"),
        public_url: f.ler_texto("public-url"),
        public_url_explicita: false,
        nivel_log,
        log_em_texto: f.ler_booleano("log-texto"),
    };
    c.normalizar()?;
    Ok(c)
}

/// A configuração de um subcomando que só tem as flags comuns.
pub fn ler(comando: &str, args: &[String]) -> anyhow::Result<Config> {
    let mut f = Flags::new(format!("patchbay {comando}"));
    registrar(&mut f);
    f.analisar(args).map_err(|e| if e.is::<crate::cli::PedidoDeAjuda>() { e } else { e.context("config") })?;
    resolver(&f)
}

impl Config {
    fn normalizar(&mut self) -> anyhow::Result<()> {
        anyhow::ensure!(!self.listen.is_empty(), "config: endereço de escuta vazio");
        anyhow::ensure!(!self.data_dir.is_empty(), "config: diretório de dados vazio");
        self.public_url_explicita = !self.public_url.is_empty();
        if self.public_url.is_empty() {
            // Derivar da escuta serve em desenvolvimento e é errado atrás de proxy.
            self.public_url = format!("http://{}", self.listen);
        }
        self.public_url = self.public_url.trim_end_matches('/').to_string();
        anyhow::ensure!(url_com_esquema_e_host(&self.public_url), "config: URL pública inválida: {:?}", self.public_url);
        Ok(())
    }
}

/// O que o `url.Parse` do Go aceita com esquema e host não vazios: `esquema://[usuário@]host...`,
/// esquema começando por letra e só com letra, dígito, `+`, `-` ou `.`.
pub fn url_com_esquema_e_host(u: &str) -> bool {
    let Some((esquema, resto)) = u.split_once("://") else { return false };
    let esquema_ok = esquema.chars().next().is_some_and(|c| c.is_ascii_alphabetic())
        && esquema.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '-' | '.'));
    let autoridade = resto.split(['/', '?', '#']).next().unwrap_or("");
    let host = autoridade.rsplit_once('@').map_or(autoridade, |(_, h)| h);
    esquema_ok && !host.is_empty() && !u.chars().any(|c| c.is_control() || c == ' ')
}

#[cfg(test)]
mod testes {
    use super::*;

    #[test]
    fn url_publica_como_o_go() {
        for (u, ok) in [
            ("http://127.0.0.1:8787", true),
            ("https://patchbay.exemplo.com", true),
            ("http://:8787", true),
            ("http://", false),
            ("localhost:8787", false),
            ("1http://x", false),
            ("http://a b", false),
        ] {
            assert_eq!(url_com_esquema_e_host(u), ok, "{u}");
        }
    }
}
