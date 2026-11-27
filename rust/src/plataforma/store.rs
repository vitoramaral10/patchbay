//! O SQLite que é a única fonte de verdade do patchbay, no mesmo arquivo e com o mesmo esquema do
//! Go (`internal/platform/store`): dois pools sobre `patchbay.db`, leitura e escrita com uma
//! conexão só (o SQLite aceita um escritor por vez mesmo em WAL; serializar aqui troca
//! `SQLITE_BUSY` por espera na fila), e os mesmos pragmas.

use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::Context as _;
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePool, SqlitePoolOptions, SqliteSynchronous};

use super::migracoes::{self, MIGRACOES};

/// O nome do arquivo de banco dentro do diretório de dados.
pub const NOME_ARQUIVO: &str = "patchbay.db";

/// O teto do pool de leitura. O Go não limita; aqui é preciso um número, e o SQLite em WAL não
/// ganha nada com mais leitores simultâneos que isto num serviço de uma pessoa.
const LEITORES: u32 = 16;

/// Quanto um pedido de conexão espera na fila do pool. O `database/sql` do Go espera sem prazo
/// (até o contexto da requisição); o padrão do sqlx, 30 s, devolveria `PoolTimedOut` a um leitor
/// atrás de uma escrita longa. Um dia é "sem teto" na prática sem estourar a conta do `Instant`.
const ESPERA_POR_CONEXAO: Duration = Duration::from_secs(24 * 60 * 60);

/// O arquivo de trava das migrações, ao lado do banco (como o `-wal` e o `-shm` do SQLite).
const TRAVA_MIGRACAO: &str = "patchbay.db-trava-migracao";

/// Os dois pools sobre o mesmo arquivo. A leitura sai crua ([`Store::leitura`]); a escrita só por
/// [`Store::transacao_escrita`], que já abre com `BEGIN IMMEDIATE`: um `begin()` comum no pool de
/// escrita abriria transação adiada, e o upgrade de trava no meio dela é o `SQLITE_BUSY` que o
/// `_txlock=immediate` do Go evita.
pub struct Store {
    leitura: SqlitePool,
    escrita: SqlitePool,
    caminho: PathBuf,
}

impl Store {
    /// Abre o banco em `dir`, aplica as migrações embutidas e devolve os dois pools prontos.
    /// O diretório precisa existir.
    pub async fn abrir(dir: &Path) -> anyhow::Result<Store> {
        if dir.as_os_str().is_empty() {
            anyhow::bail!("store: diretório de dados vazio");
        }
        let caminho = dir.join(NOME_ARQUIVO);
        // Duas instâncias subindo juntas no mesmo diretório abrem e migram uma de cada vez: no
        // banco novo, passar o journal para WAL pede trava exclusiva e falha com "database is
        // locked" sem esperar o busy_timeout; e a 00017 roda fora de transação, e só a trava entre
        // processos impede que ela rode duas vezes.
        let trava = travar_migracao(dir).await.context("store: aplicar migrações")?;
        let escrita = abrir_pool(&caminho, true).await.context("store: pool de escrita")?;
        let leitura = match abrir_pool(&caminho, false).await {
            Ok(p) => p,
            Err(e) => {
                escrita.close().await;
                return Err(e.context("store: pool de leitura"));
            }
        };
        let st = Store { leitura, escrita, caminho };
        let r = migracoes::migrar(&st.escrita, MIGRACOES).await;
        drop(trava);
        if let Err(e) = r {
            st.fechar().await;
            return Err(e.context("store: aplicar migrações"));
        }
        Ok(st)
    }

    /// O pool de leitura.
    #[cfg_attr(not(test), expect(dead_code, reason = "as features (F2 em diante) recebem os pools por aqui"))]
    pub fn leitura(&self) -> &SqlitePool {
        &self.leitura
    }

    /// Uma transação na conexão única de escrita, aberta com `BEGIN IMMEDIATE` (o
    /// `_txlock=immediate` do Go). É o único caminho de escrita, inclusive de um comando só.
    #[cfg_attr(not(test), expect(dead_code, reason = "as features (F2 em diante) escrevem por aqui"))]
    pub async fn transacao_escrita(&self) -> sqlx::Result<sqlx::Transaction<'static, sqlx::Sqlite>> {
        self.escrita.begin_with("BEGIN IMMEDIATE").await
    }

    /// O pool de escrita cru, para os testes do executor de migrações.
    #[cfg(test)]
    pub(crate) fn pool_de_escrita(&self) -> &SqlitePool {
        &self.escrita
    }

    /// O arquivo de banco em uso.
    pub fn caminho(&self) -> &Path {
        &self.caminho
    }

    /// Fecha os dois pools (o último a fechar faz o checkpoint do WAL, como no Go).
    pub async fn fechar(&self) {
        self.leitura.close().await;
        self.escrita.close().await;
    }

    /// O acesso à tabela `settings`.
    pub fn configuracoes(&self) -> Configuracoes {
        Configuracoes { leitura: self.leitura.clone(), escrita: self.escrita.clone() }
    }
}

/// Os pragmas da seção 08.8, os mesmos do DSN do Go: WAL, `busy_timeout=5000`,
/// `synchronous=NORMAL`, `foreign_keys=1`.
async fn abrir_pool(caminho: &Path, escritor: bool) -> anyhow::Result<SqlitePool> {
    let opcoes = SqliteConnectOptions::new()
        .filename(caminho)
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .busy_timeout(Duration::from_millis(5000))
        .synchronous(SqliteSynchronous::Normal)
        .foreign_keys(true);
    SqlitePoolOptions::new()
        .max_connections(if escritor { 1 } else { LEITORES })
        .min_connections(0)
        .idle_timeout(Duration::from_secs(5 * 60))
        .acquire_timeout(ESPERA_POR_CONEXAO)
        // connect_with abre a primeira conexão: é o PingContext do Go.
        .connect_with(opcoes)
        .await
        .with_context(|| format!("abrir {}", caminho.display()))
}

/// Trava exclusiva entre processos (`flock`) no arquivo de trava do diretório; solta no drop.
/// shortcut: fora do Unix não trava, e a corrida de duas instâncias no primeiro boot fica só com a
/// reconferência dentro da transação (que não cobre a 00017); a imagem é Linux.
async fn travar_migracao(dir: &Path) -> anyhow::Result<std::fs::File> {
    let caminho = dir.join(TRAVA_MIGRACAO);
    tokio::task::spawn_blocking(move || {
        let arquivo = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(&caminho)
            .with_context(|| format!("abrir {}", caminho.display()))?;
        #[cfg(unix)]
        {
            use std::os::fd::AsRawFd;
            // SAFETY: o descritor é do arquivo aberto acima e vive até o fim desta função.
            if unsafe { libc::flock(arquivo.as_raw_fd(), libc::LOCK_EX) } != 0 {
                return Err(std::io::Error::last_os_error()).with_context(|| format!("travar {}", caminho.display()));
            }
        }
        Ok(arquivo)
    })
    .await?
}

/// O acesso à tabela `settings`: a configuração do processo que vive no banco, em chave/valor (o
/// canário da chave mestra, a URL pública, a geração do import de YAML).
#[derive(Clone)]
pub struct Configuracoes {
    leitura: SqlitePool,
    escrita: SqlitePool,
}

impl Configuracoes {
    /// Cria ou substitui o valor de uma chave.
    #[cfg_attr(not(test), expect(dead_code, reason = "a tela de ajustes (F12) grava a URL pública"))]
    pub async fn gravar(&self, chave: &str, valor: &str) -> anyhow::Result<()> {
        self.inserir(
            "INSERT INTO settings (chave, valor, atualizado_em)
VALUES (?, ?, ?)
ON CONFLICT (chave) DO UPDATE SET
    valor = excluded.valor,
    atualizado_em = excluded.atualizado_em",
            chave,
            valor,
        )
        .await
    }

    async fn inserir(&self, sql: &'static str, chave: &str, valor: &str) -> anyhow::Result<()> {
        let r: sqlx::Result<()> = async {
            let mut tx = self.escrita.begin_with("BEGIN IMMEDIATE").await?;
            sqlx::query(sql).bind(chave).bind(valor).bind(jiff::Timestamp::now().as_second()).execute(&mut *tx).await?;
            tx.commit().await
        }
        .await;
        r.with_context(|| format!("store: gravar configuração {chave}"))
    }
}

impl super::cripto::Configuracoes for Configuracoes {
    async fn ler(&self, chave: &str) -> anyhow::Result<Option<String>> {
        sqlx::query_scalar("SELECT valor FROM settings WHERE chave = ?")
            .bind(chave)
            .fetch_optional(&self.leitura)
            .await
            .with_context(|| format!("store: ler configuração {chave}"))
    }

    async fn gravar_se_ausente(&self, chave: &str, valor: &str) -> anyhow::Result<()> {
        self.inserir("INSERT INTO settings (chave, valor, atualizado_em) VALUES (?, ?, ?) ON CONFLICT (chave) DO NOTHING", chave, valor)
            .await
    }
}

/// Um diretório temporário próprio, apagado no `Drop`. Só para testes.
#[cfg(test)]
pub struct DirTemporario(pub PathBuf);

#[cfg(test)]
impl DirTemporario {
    pub fn new() -> Self {
        use std::sync::atomic::{AtomicUsize, Ordering};
        static N: AtomicUsize = AtomicUsize::new(0);
        let d = std::env::temp_dir().join(format!("patchbay-teste-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst)));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        DirTemporario(d)
    }
}

#[cfg(test)]
impl Drop for DirTemporario {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Os testes de `internal/platform/store` (store_test e configuracoes_test; o de migracoes_test
/// fica em `migracoes.rs`), com os mesmos casos.
#[cfg(test)]
mod testes {
    use sqlx::Row as _;

    use super::*;
    use crate::plataforma::cripto::Configuracoes as _;

    async fn abrir() -> (Store, DirTemporario) {
        let d = DirTemporario::new();
        (Store::abrir(&d.0).await.expect("abrir"), d)
    }

    async fn existe_tabela(st: &Store, tabela: &str) -> bool {
        sqlx::query_scalar::<_, String>("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?")
            .bind(tabela)
            .fetch_optional(st.leitura())
            .await
            .unwrap()
            .is_some()
    }

    #[tokio::test]
    async fn abrir_recusa_diretorio_vazio() {
        let e = Store::abrir(Path::new("")).await.err().expect("quer erro");
        assert_eq!(e.to_string(), "store: diretório de dados vazio");
    }

    #[tokio::test]
    async fn abrir_aplica_migracoes() {
        let (st, _d) = abrir().await;
        for t in ["upstream", "endpoint", "endpoint_upstream", "api_key", "api_key_endpoint"] {
            assert!(existe_tabela(&st, t).await, "tabela {t}");
        }
    }

    #[tokio::test]
    async fn abrir_idempotente() {
        let d = DirTemporario::new();
        for i in 0..2 {
            let st = Store::abrir(&d.0).await.unwrap_or_else(|e| panic!("abrir na volta {i}: {e:#}"));
            st.fechar().await;
        }
    }

    #[tokio::test]
    async fn abrir_pragmas_em_vigor() {
        let (st, _d) = abrir().await;
        for (pragma, quer) in [("journal_mode", "wal"), ("busy_timeout", "5000"), ("synchronous", "1"), ("foreign_keys", "1")] {
            // O valor do pragma vem da tabela do teste, não de entrada.
            let sql = sqlx::AssertSqlSafe(format!("PRAGMA {pragma}"));
            let fila = sqlx::query(sql).fetch_one(st.leitura()).await.unwrap();
            let valor = fila.try_get::<String, _>(0).or_else(|_| fila.try_get::<i64, _>(0).map(|n| n.to_string())).unwrap();
            assert_eq!(valor, quer, "PRAGMA {pragma}");
        }
    }

    /// O pool de escrita serializa: é o que troca SQLITE_BUSY por espera na fila.
    #[tokio::test]
    async fn pools_escritor_unico() {
        let (st, _d) = abrir().await;
        assert_eq!(st.escrita.options().get_max_connections(), 1);
        assert_ne!(st.leitura().options().get_max_connections(), 1);
    }

    /// A integridade referencial está ligada: sem ela, composição órfã passaria em silêncio.
    #[tokio::test]
    async fn chave_estrangeira() {
        let (st, _d) = abrir().await;
        let mut tx = st.transacao_escrita().await.unwrap();
        let r = sqlx::query("INSERT INTO endpoint_upstream (endpoint_id, upstream_id) VALUES (999, 999)").execute(&mut *tx).await;
        assert!(r.is_err(), "quer violação de chave estrangeira");
    }

    #[tokio::test]
    async fn configuracoes_chave_inexistente_nao_e_erro() {
        let (st, _d) = abrir().await;
        assert_eq!(st.configuracoes().ler("nunca_gravada").await.unwrap(), None);
    }

    #[tokio::test]
    async fn configuracoes_gravar_e_ler_de_volta() {
        let (st, _d) = abrir().await;
        let sut = st.configuracoes();
        sut.gravar("canario_chave_mestra", "pbc1:primeiro").await.unwrap();
        // Regravar substitui: o canário do primeiro boot é único por banco.
        sut.gravar("canario_chave_mestra", "pbc1:segundo").await.unwrap();
        assert_eq!(sut.ler("canario_chave_mestra").await.unwrap().as_deref(), Some("pbc1:segundo"));
    }

    /// A migração 00004 entrou.
    #[tokio::test]
    async fn migracoes_tabelas_da_fatia_6() {
        let (st, _d) = abrir().await;
        for t in ["settings", "upstream_secret"] {
            assert!(existe_tabela(&st, t).await, "tabela {t}");
        }
    }

    /// O critério da seção 2.2 do plano: um banco criado e migrado pelo Go, com um segredo de cada
    /// tipo gravado pelos repositórios do Go (`tests/fixtures/gerar_segredos_go.go`). O Rust abre
    /// sem mexer em `goose_db_version`, o canário confere e todo segredo decifra com o AAD de cada
    /// coluna (`upstream_secret`: `<upstream>/<tipo>/<nome>`; `upstream_oauth`: `<upstream>`).
    #[tokio::test]
    async fn fixture_do_go_abre_e_decifra() {
        use crate::plataforma::cripto::{Campo, ChaveMestra, Cofre, verificar_canario};

        let d = DirTemporario::new();
        std::fs::copy(concat!(env!("CARGO_MANIFEST_DIR"), "/tests/fixtures/segredos-go.db"), d.0.join(NOME_ARQUIVO)).unwrap();
        let versoes = async |st: &Store| -> Vec<(i64, i64, i64, String)> {
            sqlx::query_as("SELECT id, version_id, is_applied, tstamp FROM goose_db_version ORDER BY id")
                .fetch_all(st.leitura())
                .await
                .unwrap()
        };
        let antes = {
            let opcoes = SqliteConnectOptions::new().filename(d.0.join(NOME_ARQUIVO)).read_only(true);
            let p = SqlitePoolOptions::new().connect_with(opcoes).await.unwrap();
            let v =
                sqlx::query_as("SELECT id, version_id, is_applied, tstamp FROM goose_db_version ORDER BY id").fetch_all(&p).await.unwrap();
            p.close().await;
            v
        };
        let st = Store::abrir(&d.0).await.expect("o Rust abre o banco do Go");
        assert_eq!(versoes(&st).await, antes, "goose_db_version mudou");

        // A chave mestra fixa da caixa-preta (tests/apoio/mod.rs).
        let cofre = Cofre::new(&ChaveMestra::de("cGF0Y2hiYXktY2FpeGEtcHJldGEtY2hhdmUtZml4YSE=").unwrap());
        assert!(!verificar_canario(&cofre, &st.configuracoes()).await.expect("canário do Go confere"), "regravou o canário");

        let segredos: Vec<(i64, String, String, String)> =
            sqlx::query_as("SELECT upstream_id, tipo, nome, valor_cifrado FROM upstream_secret ORDER BY id")
                .fetch_all(st.leitura())
                .await
                .unwrap();
        let mut claros = Vec::new();
        for (up, tipo, nome, cifrado) in &segredos {
            let id = format!("{up}/{tipo}/{nome}");
            let claro = cofre.decifrar(Campo { tabela: "upstream_secret", coluna: "valor_cifrado", id: &id }, cifrado).unwrap();
            claros.push((id, String::from_utf8(claro.revelar().to_vec()).unwrap()));
        }
        let (up, segredo, acesso, refresh): (i64, String, String, String) =
            sqlx::query_as("SELECT upstream_id, client_secret_cifrado, access_token_cifrado, refresh_token_cifrado FROM upstream_oauth")
                .fetch_one(st.leitura())
                .await
                .unwrap();
        let id = up.to_string();
        for (coluna, cifrado) in [("client_secret_cifrado", segredo), ("access_token_cifrado", acesso), ("refresh_token_cifrado", refresh)]
        {
            let claro = cofre.decifrar(Campo { tabela: "upstream_oauth", coluna, id: &id }, &cifrado).unwrap();
            claros.push((format!("{id}/{coluna}"), String::from_utf8(claro.revelar().to_vec()).unwrap()));
        }
        let quer = [
            ("1/bearer/", "bearer-gravado-pelo-go"),
            ("1/header/X-Api-Key", "header-gravado-pelo-go"),
            ("2/env/TOKEN_DO_GO", "env-gravado-pelo-go"),
            ("3/client_secret_cifrado", "client-secret-gravado-pelo-go"),
            ("3/access_token_cifrado", "access-gravado-pelo-go"),
            ("3/refresh_token_cifrado", "refresh-gravado-pelo-go"),
        ];
        assert_eq!(claros, quer.map(|(a, b)| (a.to_string(), b.to_string())));
        st.fechar().await;
    }

    /// Item 6 da revisão: a transação de escrita já nasce com a trava de escrita. Com `BEGIN`
    /// adiado, outra conexão conseguiria a trava antes do primeiro comando.
    #[tokio::test]
    async fn transacao_de_escrita_nasce_immediate() {
        let (st, _d) = abrir().await;
        let tx = st.transacao_escrita().await.unwrap();
        let opcoes = SqliteConnectOptions::new().filename(st.caminho()).busy_timeout(Duration::ZERO);
        let mut outra = sqlx::ConnectOptions::connect(&opcoes).await.unwrap();
        let r = sqlx::raw_sql("BEGIN IMMEDIATE").execute(&mut outra).await;
        assert!(r.is_err_and(|e| e.to_string().contains("locked")), "a outra conexão pegou a trava de escrita");
        tx.rollback().await.unwrap();
        sqlx::raw_sql("BEGIN IMMEDIATE; ROLLBACK").execute(&mut outra).await.expect("livre depois do rollback");
    }

    /// Item 9: nenhum pedido de conexão leva `PoolTimedOut` aos 30 s do padrão do sqlx.
    #[tokio::test]
    async fn espera_por_conexao_sem_teto_pratico() {
        let (st, _d) = abrir().await;
        assert_eq!(st.leitura.options().get_acquire_timeout(), ESPERA_POR_CONEXAO);
        assert_eq!(st.escrita.options().get_acquire_timeout(), ESPERA_POR_CONEXAO);
        assert!(ESPERA_POR_CONEXAO >= Duration::from_secs(3600));
    }

    /// Item 10: `gravar_se_ausente` não sobrescreve; `gravar` sim.
    #[tokio::test]
    async fn gravar_se_ausente_mantem_o_primeiro() {
        let (st, _d) = abrir().await;
        let sut = st.configuracoes();
        sut.gravar_se_ausente("canario_chave_mestra", "pbc1:primeiro").await.unwrap();
        sut.gravar_se_ausente("canario_chave_mestra", "pbc1:segundo").await.unwrap();
        assert_eq!(sut.ler("canario_chave_mestra").await.unwrap().as_deref(), Some("pbc1:primeiro"));
    }

    /// Item 5: duas aberturas simultâneas no banco vazio (cada uma com sua trava, como dois
    /// processos) terminam com uma linha por versão, nenhuma repetida.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn duas_aberturas_juntas_migram_uma_vez() {
        for _ in 0..5 {
            let d = DirTemporario::new();
            let (a, b) = tokio::join!(Store::abrir(&d.0), Store::abrir(&d.0));
            let (a, b) = (a.expect("a"), b.expect("b"));
            let versoes: Vec<i64> =
                sqlx::query_scalar("SELECT version_id FROM goose_db_version ORDER BY id").fetch_all(a.leitura()).await.unwrap();
            let quer: Vec<i64> = std::iter::once(0).chain(MIGRACOES.iter().map(|m| m.versao)).collect();
            assert_eq!(versoes, quer);
            a.fechar().await;
            b.fechar().await;
        }
    }
}
