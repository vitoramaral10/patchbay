//! Executor de migrações compatível com a goose 3.28 que o Go usa: a mesma tabela
//! `goose_db_version`, a linha zero na criação, uma linha por migração aplicada, e os mesmos `.sql`
//! de `internal/platform/store/migracoes/` (uma cópia só até a F15). O Rust sobe num banco que o Go
//! migrou sem mexer no que já está lá, e o Go sobe num banco que o Rust criou.
//!
//! O que se reproduz da goose (`provider.go:up`, `provider_run.go`, `gooseutil/resolve.go`):
//! - tabela ausente: `CREATE TABLE` e `INSERT (0, 1)` numa transação;
//! - versão do arquivo menor que a maior do banco e não aplicada: erro (migração "faltando");
//! - versão do banco maior que as do binário: não é erro (o Go antigo sobe no banco do Rust novo);
//! - cada migração na sua transação, com o `INSERT` da versão dentro dela; com
//!   `-- +goose NO TRANSACTION` (a 00017), direto na conexão, porque `PRAGMA foreign_keys` não
//!   funciona dentro de transação.
//!
//! Diferença de implementação, não de efeito: a goose divide a seção Up em comandos e executa um a
//! um; aqui a seção Up vai inteira para o SQLite, que executa os comandos em sequência na mesma
//! conexão. As anotações `-- +goose StatementBegin/End` são comentários SQL e não mudam nada.

use anyhow::Context as _;
use sqlx::SqlitePool;

/// Uma migração embutida: a versão (o número do nome do arquivo) e o texto.
pub struct Migracao {
    pub versao: i64,
    pub arquivo: &'static str,
    pub sql: &'static str,
}

macro_rules! migracao {
    ($versao:literal, $arquivo:literal) => {
        Migracao {
            versao: $versao,
            arquivo: $arquivo,
            sql: include_str!(concat!(env!("CARGO_MANIFEST_DIR"), "/../internal/platform/store/migracoes/", $arquivo)),
        }
    };
}

/// As migrações do Go, em ordem. Um teste confere que a lista é o diretório inteiro.
pub const MIGRACOES: &[Migracao] = &[
    migracao!(1, "00001_esqueleto.sql"),
    migracao!(2, "00002_admin_ui.sql"),
    migracao!(4, "00004_segredos.sql"),
    migracao!(5, "00005_oauth_as.sql"),
    migracao!(6, "00006_composicao_fina.sql"),
    migracao!(7, "00007_upstream_stdio.sql"),
    migracao!(8, "00008_oauth_upstream.sql"),
    migracao!(10, "00010_authsrv_clientes_dinamicos.sql"),
    migracao!(11, "00011_trilha.sql"),
    migracao!(12, "00012_sonda.sql"),
    migracao!(13, "00013_biblioteca.sql"),
    migracao!(14, "00014_biblioteca_curadoria.sql"),
    migracao!(15, "00015_biblioteca_so_oficiais.sql"),
    migracao!(16, "00016_biblioteca_endpoints.sql"),
    migracao!(17, "00017_upstream_sem_auth.sql"),
    migracao!(18, "00018_upstream_oauth_loopback.sql"),
];

impl Migracao {
    /// Se a migração pede `-- +goose NO TRANSACTION`.
    fn sem_transacao(&self) -> bool {
        self.sql.lines().any(|l| l.trim() == "-- +goose NO TRANSACTION")
    }

    /// A seção Up: das linhas depois de `-- +goose Up` até `-- +goose Down` (ou o fim).
    fn up(&self) -> anyhow::Result<&'static str> {
        let sql = self.sql;
        let linha = |marca: &str| {
            let mut pos = 0;
            for l in sql.split_inclusive('\n') {
                if l.trim() == marca {
                    return Some((pos, pos + l.len()));
                }
                pos += l.len();
            }
            None
        };
        let (_, inicio) = linha("-- +goose Up").with_context(|| format!("{}: sem '-- +goose Up'", self.arquivo))?;
        let fim = linha("-- +goose Down").map_or(sql.len(), |(i, _)| i);
        anyhow::ensure!(fim >= inicio, "{}: '-- +goose Down' antes de '-- +goose Up'", self.arquivo);
        Ok(&sql[inicio..fim])
    }
}

/// Aplica as migrações pendentes no pool de escrita (DDL não compete com outra conexão). Quem
/// chama segura a trava entre processos ([`super::store::Store::abrir`]); mesmo assim, cada passo
/// em transação reconfere dentro do `BEGIN IMMEDIATE` se outro já o fez.
pub async fn migrar(escrita: &SqlitePool, migracoes: &[Migracao]) -> anyhow::Result<()> {
    let mut conn = escrita.acquire().await?;
    garantir_tabela_de_versoes(&mut conn).await?;

    // Todas as linhas, como o ListMigrations da goose (is_applied não entra na conta).
    let no_banco: Vec<i64> = sqlx::query_scalar("SELECT version_id FROM goose_db_version ORDER BY id DESC").fetch_all(&mut *conn).await?;
    anyhow::ensure!(!no_banco.is_empty(), "missing zero version migration");
    let maxima = no_banco.iter().copied().max().unwrap_or(0);
    let do_binario = migracoes.iter().map(|m| m.versao).max().unwrap_or(0);
    if maxima > do_binario {
        // Não é erro (o rollback para um binário anterior depende disso), mas é o sinal de que
        // um binário mais novo passou por este banco.
        tracing::warn!(
            versao_do_banco = maxima,
            versao_do_binario = do_binario,
            "o banco está numa versão de esquema à frente deste binário; subindo assim mesmo"
        );
    }
    let pendentes: Vec<&Migracao> = migracoes.iter().filter(|m| !no_banco.contains(&m.versao)).collect();
    let faltando: Vec<String> = pendentes.iter().filter(|m| m.versao < maxima).map(|m| m.versao.to_string()).collect();
    if !faltando.is_empty() {
        let (s, v) = if faltando.len() > 1 { ("s", "versions") } else { ("", "version") };
        anyhow::bail!(
            "detected {} missing (out-of-order) migration{s} lower than database version ({maxima}): {v} {}",
            faltando.len(),
            faltando.join(",")
        );
    }

    for m in pendentes {
        aplicar(&mut conn, m).await.with_context(|| format!("failed to run SQL migration {}", m.arquivo))?;
    }
    Ok(())
}

/// Aplica uma migração e grava a versão; `false` quando outro já a tinha aplicado.
async fn aplicar(conn: &mut sqlx::SqliteConnection, m: &Migracao) -> anyhow::Result<bool> {
    let up = m.up()?;
    if m.sem_transacao() {
        // Sem transação não há onde reconferir com a trava de escrita: o que impede a segunda
        // aplicação é a trava entre processos de quem chama.
        if ja_aplicada(conn, m.versao).await? {
            return Ok(false);
        }
        sqlx::raw_sql(up).execute(&mut *conn).await?;
        inserir_versao(conn, m.versao).await?;
        return Ok(true);
    }
    let mut tx = sqlx::Connection::begin_with(&mut *conn, "BEGIN IMMEDIATE").await?;
    if ja_aplicada(&mut tx, m.versao).await? {
        tx.rollback().await?;
        return Ok(false);
    }
    sqlx::raw_sql(up).execute(&mut *tx).await?;
    inserir_versao(&mut tx, m.versao).await?;
    tx.commit().await?;
    Ok(true)
}

/// Cria `goose_db_version` com a linha zero se ela não existe, reconferindo dentro do
/// `BEGIN IMMEDIATE`; tenta 3 vezes de novo, 1 s entre elas, como o `tryEnsureVersionTable` da goose.
async fn garantir_tabela_de_versoes(conn: &mut sqlx::SqliteConnection) -> anyhow::Result<()> {
    let mut tentativa = 0;
    loop {
        let r: sqlx::Result<()> = async {
            let mut tx = sqlx::Connection::begin_with(&mut *conn, "BEGIN IMMEDIATE").await?;
            let existe: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'goose_db_version')")
                    .fetch_one(&mut *tx)
                    .await?;
            if !existe {
                // O texto da goose (internal/dialects/sqlite3.go), espaços inclusive: é o que fica
                // em sqlite_master e no despejo do esquema.
                sqlx::raw_sql(
                    "CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	)",
                )
                .execute(&mut *tx)
                .await?;
                inserir_versao(&mut tx, 0).await?;
            }
            tx.commit().await
        }
        .await;
        match r {
            Ok(()) => return Ok(()),
            Err(e) if tentativa < 3 => {
                tentativa += 1;
                tracing::debug!(erro = %e, tentativa, "criar a tabela de versões; tentando de novo");
                tokio::time::sleep(std::time::Duration::from_secs(1)).await;
            }
            Err(e) => return Err(e).context("create version table"),
        }
    }
}

async fn ja_aplicada(conn: &mut sqlx::SqliteConnection, versao: i64) -> sqlx::Result<bool> {
    sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM goose_db_version WHERE version_id = ?)").bind(versao).fetch_one(conn).await
}

async fn inserir_versao(conn: &mut sqlx::SqliteConnection, versao: i64) -> sqlx::Result<()> {
    sqlx::query("INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, ?)")
        .bind(versao)
        .bind(true)
        .execute(conn)
        .await
        .map(|_| ())
}

#[cfg(test)]
mod testes {
    use super::*;
    use crate::plataforma::store::{DirTemporario, NOME_ARQUIVO, Store};

    /// A lista embutida é o diretório do Go inteiro: uma migração nova lá sem entrada aqui
    /// faria o Rust parar num esquema mais velho que o do Go.
    #[test]
    fn lista_igual_ao_diretorio_do_go() {
        let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../internal/platform/store/migracoes");
        let mut arquivos: Vec<String> = std::fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().into_string().unwrap())
            .filter(|n| n.ends_with(".sql"))
            .collect();
        arquivos.sort();
        let embutidos: Vec<&str> = MIGRACOES.iter().map(|m| m.arquivo).collect();
        assert_eq!(arquivos, embutidos);
        for m in MIGRACOES {
            assert_eq!(m.arquivo[..5].parse::<i64>().unwrap(), m.versao, "{}", m.arquivo);
            m.up().unwrap();
        }
        let sem: Vec<i64> = MIGRACOES.iter().filter(|m| m.sem_transacao()).map(|m| m.versao).collect();
        assert_eq!(sem, [17], "só a 00017 é NO TRANSACTION");
    }

    /// Porte de `TestMigracao00017_PreservaFilhasEAceitaModoNenhum`: a 00017 reconstrói a tabela
    /// upstream (mãe de quatro filhas com ON DELETE CASCADE), e isso só falha em banco com dado.
    /// Para no 16, povoa upstream e as filhas, e só então aplica a 17.
    #[tokio::test]
    async fn migracao_00017_preserva_filhas_e_aceita_modo_nenhum() {
        let d = DirTemporario::new();
        // O pool de escrita do Store, sem migrar: abre, e o Store::abrir completo vem depois.
        let opcoes = sqlx::sqlite::SqliteConnectOptions::new().filename(d.0.join(NOME_ARQUIVO)).create_if_missing(true).foreign_keys(true);
        let db = sqlx::sqlite::SqlitePoolOptions::new().max_connections(1).connect_with(opcoes).await.unwrap();
        let ate_16 = MIGRACOES.iter().position(|m| m.versao == 17).unwrap();
        migrar(&db, &MIGRACOES[..ate_16]).await.expect("até a 16");

        for c in [
            "INSERT INTO upstream (id, nome, tipo, url, timeout_ms, habilitado, criado_em, modo_credencial)
             VALUES (1, 'alvo', 'http', 'https://exemplo.com/mcp', 15000, 1, 0, 'estatica')",
            "INSERT INTO endpoint (id, slug, criado_em) VALUES (1, 'principal', 0)",
            "INSERT INTO endpoint_upstream (endpoint_id, upstream_id) VALUES (1, 1)",
            "INSERT INTO upstream_secret (upstream_id, tipo, nome, valor_cifrado, criado_em, atualizado_em)
             VALUES (1, 'bearer', '', 'cifrado', 0, 0)",
            "INSERT INTO upstream_oauth (upstream_id, criado_em, atualizado_em) VALUES (1, 0, 0)",
        ] {
            sqlx::query(c).execute(&db).await.unwrap_or_else(|e| panic!("povoar ({c}): {e}"));
        }

        migrar(&db, MIGRACOES).await.expect("o resto");

        let contar = async |sql: &'static str| -> i64 { sqlx::query_scalar(sql).fetch_one(&db).await.unwrap() };
        // As filhas continuam lá: com foreign_keys ligado no DROP TABLE, a cascata as levaria.
        for (t, sql) in [
            ("endpoint_upstream", "SELECT count(*) FROM endpoint_upstream WHERE upstream_id = 1"),
            ("upstream_secret", "SELECT count(*) FROM upstream_secret WHERE upstream_id = 1"),
            ("upstream_oauth", "SELECT count(*) FROM upstream_oauth WHERE upstream_id = 1"),
        ] {
            assert_eq!(contar(sql).await, 1, "{t}");
        }
        // O upstream sobreviveu com o modo que tinha.
        let modo: String = sqlx::query_scalar("SELECT modo_credencial FROM upstream WHERE id = 1").fetch_one(&db).await.unwrap();
        assert_eq!(modo, "estatica");
        // O CHECK novo aceita nenhum, e continua recusando o que ninguém entende.
        sqlx::query("UPDATE upstream SET modo_credencial = 'nenhum' WHERE id = 1").execute(&db).await.expect("gravar modo nenhum");
        assert!(sqlx::query("UPDATE upstream SET modo_credencial = 'inventado' WHERE id = 1").execute(&db).await.is_err(), "quer o CHECK");
        // E a cascata volta a funcionar: as filhas resolvem de novo para a tabela nova.
        sqlx::query("DELETE FROM upstream WHERE id = 1").execute(&db).await.unwrap();
        assert_eq!(contar("SELECT count(*) FROM upstream_secret").await, 0, "cascata depois da troca");
        db.close().await;

        // E o banco migrado em duas etapas abre como qualquer outro.
        Store::abrir(&d.0).await.expect("abrir depois").fechar().await;
    }

    /// A tabela de versões fica como a goose a deixaria: a linha zero e uma por migração, em
    /// ordem; reabrir não acrescenta nada.
    #[tokio::test]
    async fn versoes_como_a_goose() {
        let d = DirTemporario::new();
        for _ in 0..2 {
            Store::abrir(&d.0).await.unwrap().fechar().await;
        }
        let st = Store::abrir(&d.0).await.unwrap();
        let linhas: Vec<(i64, i64, i64)> =
            sqlx::query_as("SELECT id, version_id, is_applied FROM goose_db_version ORDER BY id").fetch_all(st.leitura()).await.unwrap();
        let quer: Vec<(i64, i64, i64)> =
            std::iter::once(0).chain(MIGRACOES.iter().map(|m| m.versao)).enumerate().map(|(i, v)| (i as i64 + 1, v, 1)).collect();
        assert_eq!(linhas, quer);
        st.fechar().await;
    }

    /// Versão do banco maior que as do binário não é erro (e avisa no log); versão menor que
    /// falta, é.
    #[test]
    fn versao_do_banco_a_frente_e_faltando() {
        let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap();
        let d = DirTemporario::new();
        let st = rt.block_on(Store::abrir(&d.0)).unwrap();
        let p = st.pool_de_escrita().clone();
        let exec = |sql: &'static str| rt.block_on(sqlx::query(sql).execute(&p)).unwrap();
        exec("INSERT INTO goose_db_version (version_id, is_applied) VALUES (19, 1)");
        let log = crate::plataforma::log::capturar(|| rt.block_on(migrar(&p, MIGRACOES)).expect("banco à frente do binário"));
        assert!(log.contains(r#""level":"WARN","msg":"o banco está numa versão de esquema à frente deste binário; subindo assim mesmo","versao_do_banco":19,"versao_do_binario":18"#), "{log}");
        exec("DELETE FROM goose_db_version WHERE version_id IN (5, 6)");
        let e = rt.block_on(migrar(&p, MIGRACOES)).expect_err("faltando");
        assert_eq!(e.to_string(), "detected 2 missing (out-of-order) migrations lower than database version (19): versions 5,6");
        rt.block_on(st.fechar());
    }

    /// Item 5 da revisão: a migração que outro processo aplicou entre a listagem e o
    /// `BEGIN IMMEDIATE` não roda de novo, nem a 00015, que é só DML e rodaria sem erro; a 00017
    /// (sem transação) também reconfere.
    #[tokio::test]
    async fn reconfere_antes_de_aplicar() {
        let d = DirTemporario::new();
        let st = Store::abrir(&d.0).await.unwrap();
        let mut conn = st.pool_de_escrita().acquire().await.unwrap();
        for v in [15, 17] {
            let m = MIGRACOES.iter().find(|m| m.versao == v).unwrap();
            assert!(!aplicar(&mut conn, m).await.unwrap(), "aplicou a {v} de novo");
            let n: i64 = sqlx::query_scalar("SELECT count(*) FROM goose_db_version WHERE version_id = ?")
                .bind(v)
                .fetch_one(&mut *conn)
                .await
                .unwrap();
            assert_eq!(n, 1, "versão {v}");
        }
        drop(conn);
        st.fechar().await;
    }
}
