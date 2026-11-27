//! A aplicação no ar (`cmd/patchbay/aplicacao.go`): monta o que existe (na F1, a plataforma:
//! banco, migrações, canário), serve HTTP e desliga limpo no SIGTERM/Ctrl+C. Salvar a URL pública
//! na tela de ajustes reinicia a aplicação dentro do processo: o container não cai.

use std::sync::Arc;
use std::time::Duration;

use anyhow::Context as _;
use axum::Router;
use axum::http::{StatusCode, header};
use axum::response::IntoResponse;
use axum::routing::get;
use hyper_util::rt::{TokioIo, TokioTimer};
use hyper_util::server::graceful::GracefulShutdown;
use hyper_util::service::TowerToHyperService;
use tokio::net::TcpListener;
use tokio::sync::{Notify, watch};

use crate::chave;
use crate::config::Config;
use crate::plataforma::cripto::{Cofre, Configuracoes as _};
use crate::plataforma::store::Store;

/// A chave de `settings` da URL pública editável em Ajustes.
const CHAVE_URL_PUBLICA: &str = "url_publica";
/// A rota do painel, que o log de boot anuncia.
const ROTA_PAINEL: &str = "/admin/";
/// O `ReadHeaderTimeout` do Go, e o prazo do desligamento.
const TEMPO_CABECALHO: Duration = Duration::from_secs(10);
const TEMPO_DESLIGAR: Duration = Duration::from_secs(10);
/// A pausa depois de um erro de accept, como no `net/http` do Go.
const ESPERA_ACCEPT_MINIMA: Duration = Duration::from_millis(5);
const ESPERA_ACCEPT_MAXIMA: Duration = Duration::from_secs(1);
const INTERVALO_AVISO_ACCEPT: Duration = Duration::from_secs(10);

/// Uma volta da aplicação: a configuração efetiva e o banco. O cofre entra quando a primeira
/// feature que decifra chegar (F4).
pub struct Aplicacao {
    pub cfg: Config,
    pub st: Store,
    /// `notify_one` pede outra volta do laço de [`servir`]: o servidor desliga, a aplicação fecha
    /// e é montada de novo lendo o banco (é como a URL pública salva em Ajustes passa a valer; a
    /// tela de ajustes chega na F12).
    pub reiniciar: Arc<Notify>,
}

impl Aplicacao {
    /// As rotas. Na F1 só `/saude`; o resto vem com as fatias seguintes.
    fn rotas(&self) -> Router {
        Router::new()
            .route("/saude", get(|| async { ([(header::CONTENT_TYPE, "text/plain; charset=utf-8")], "ok\n") }))
            .fallback(nao_encontrado)
    }
}

/// O 404 do `http.ServeMux` do Go, com os cabeçalhos do `http.Error`.
async fn nao_encontrado() -> impl IntoResponse {
    (
        StatusCode::NOT_FOUND,
        [(header::CONTENT_TYPE, "text/plain; charset=utf-8"), (header::X_CONTENT_TYPE_OPTIONS, "nosniff")],
        "404 page not found\n",
    )
}

/// Abre o banco, aplica as migrações e confere o canário. O cofre entra por parâmetro e não pela
/// `Config`: a configuração é impressa em log e vai inteira para os testes.
pub async fn montar(mut cfg: Config, cofre: Arc<Cofre>) -> anyhow::Result<Aplicacao> {
    chave::criar_diretorio(std::path::Path::new(&cfg.data_dir))?;
    let st = Store::abrir(std::path::Path::new(&cfg.data_dir)).await?;
    let r = async {
        if !cfg.public_url_explicita
            && let Some(gravada) = st.configuracoes().ler(CHAVE_URL_PUBLICA).await?
        {
            cfg.public_url = gravada;
        }
        // Antes de qualquer componente: se o canário não confere, nada do que está cifrado volta.
        chave::verificar_canario(&cofre, &st).await
    }
    .await;
    if let Err(e) = r {
        st.fechar().await;
        return Err(e);
    }
    Ok(Aplicacao { cfg, st, reiniciar: Arc::new(Notify::new()) })
}

/// Sobe o servidor HTTP e desliga limpo no SIGTERM ou Ctrl+C. A chave é lida uma vez: reler no
/// reinício sortearia uma chave nova em `<dados>/chave-mestra`.
pub async fn servir(cfg: Config, chave_do_ambiente: Option<&[u8]>) -> anyhow::Result<()> {
    let cofre = Arc::new(chave::cofre_do_boot(&cfg.data_dir, chave_do_ambiente)?);
    let (avisar, desligar) = watch::channel(false);
    tokio::spawn(async move {
        sinal().await;
        let _ = avisar.send(true);
    });
    servir_ate(cfg, cofre, desligar, |_| ()).await
}

/// O laço de reinício: cada volta monta a aplicação e serve até desligar ou pedir reinício.
/// `ao_subir` recebe cada aplicação montada (é por onde o teste pede o reinício).
async fn servir_ate(
    cfg: Config,
    cofre: Arc<Cofre>,
    mut desligar: watch::Receiver<bool>,
    mut ao_subir: impl FnMut(&Aplicacao),
) -> anyhow::Result<()> {
    loop {
        let a = montar(cfg.clone(), cofre.clone()).await?;
        ao_subir(&a);
        let r = servir_uma_vez(&a, &mut desligar).await;
        a.st.fechar().await;
        match r? {
            Fim::Reiniciar => tracing::info!("reiniciando a aplicação para aplicar os ajustes"),
            Fim::Desligar => return Ok(()),
        }
    }
}

enum Fim {
    Reiniciar,
    Desligar,
}

async fn servir_uma_vez(a: &Aplicacao, desligar: &mut watch::Receiver<bool>) -> anyhow::Result<Fim> {
    tracing::info!(
        listen = a.cfg.listen.as_str(),
        url_publica = a.cfg.public_url.as_str(),
        banco = %a.st.caminho().display(),
        administracao = format!("{}{ROTA_PAINEL}", a.cfg.public_url),
        "patchbay escutando"
    );
    let ouvinte = escutar(&a.cfg.listen).await.context("servir")?;
    let app = a.rotas();
    let mut http1 = hyper::server::conn::http1::Builder::new();
    http1.timer(TokioTimer::new()).header_read_timeout(TEMPO_CABECALHO);
    let conexoes = GracefulShutdown::new();
    // Erro de accept (EMFILE, ENFILE...) deixa a conexão na fila e o socket pronto: sem pausa o
    // laço gira a 100% de CPU. Pausa de 5 ms dobrando até 1 s, zerada no primeiro accept bom (o
    // tempDelay do net/http do Go), e o aviso no log no máximo a cada 10 s.
    let mut espera = Duration::ZERO;
    let mut ultimo_aviso: Option<tokio::time::Instant> = None;
    let pausa = tokio::time::sleep(Duration::ZERO);
    tokio::pin!(pausa);
    let mut pausado = false;
    let fim = loop {
        tokio::select! {
            r = ouvinte.accept(), if !pausado => match r {
                Ok((tcp, _)) => {
                    espera = Duration::ZERO;
                    let servico = TowerToHyperService::new(app.clone());
                    let conexao = conexoes.watch(http1.serve_connection(TokioIo::new(tcp), servico));
                    tokio::spawn(async move {
                        if let Err(e) = conexao.await {
                            tracing::debug!(erro = %e, "conexão HTTP");
                        }
                    });
                }
                Err(e) => {
                    espera = if espera.is_zero() { ESPERA_ACCEPT_MINIMA } else { (espera * 2).min(ESPERA_ACCEPT_MAXIMA) };
                    let agora = tokio::time::Instant::now();
                    if ultimo_aviso.is_none_or(|t| agora - t >= INTERVALO_AVISO_ACCEPT) {
                        ultimo_aviso = Some(agora);
                        tracing::warn!(erro = %e, nova_tentativa_em_ms = espera.as_millis() as u64, "aceitar conexão");
                    }
                    pausa.as_mut().reset(agora + espera);
                    pausado = true;
                }
            },
            () = &mut pausa, if pausado => pausado = false,
            () = a.reiniciar.notified() => break Fim::Reiniciar,
            _ = desligar.wait_for(|d| *d) => {
                tracing::info!("desligando");
                break Fim::Desligar;
            }
        }
    };
    drop(ouvinte);
    let terminou = tokio::time::timeout(TEMPO_DESLIGAR, conexoes.shutdown()).await.is_ok();
    match fim {
        Fim::Desligar if !terminou => anyhow::bail!("desligar: conexões ainda abertas depois de {} s", TEMPO_DESLIGAR.as_secs()),
        f => Ok(f),
    }
}

/// O `net.Listen("tcp", endereço)` do Go: `:porta` escuta em todas as interfaces, IPv6 e IPv4
/// (com queda para só IPv4 onde não há IPv6, como em container sem IPv6).
async fn escutar(endereco: &str) -> anyhow::Result<TcpListener> {
    let r = match endereco.strip_prefix(':') {
        Some(porta) => match TcpListener::bind(format!("[::]:{porta}")).await {
            Ok(o) => Ok(o),
            Err(_) => TcpListener::bind(format!("0.0.0.0:{porta}")).await,
        },
        None => TcpListener::bind(endereco).await,
    };
    r.with_context(|| format!("escutar em {endereco}"))
}

/// Resolve no SIGTERM ou Ctrl+C.
async fn sinal() {
    #[cfg(unix)]
    if let Ok(mut term) = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = term.recv() => {}
        }
        return;
    }
    let _ = tokio::signal::ctrl_c().await;
}

#[cfg(test)]
mod testes {
    use std::sync::Mutex;

    use super::*;
    use crate::plataforma::cripto::{ChaveMestra, gerar_chave_mestra};
    use crate::plataforma::store::DirTemporario;

    fn cfg(dados: &std::path::Path, listen: &str) -> Config {
        Config {
            listen: listen.to_string(),
            data_dir: dados.display().to_string(),
            public_url: String::new(),
            public_url_explicita: false,
            nivel_log: 0,
            log_em_texto: false,
        }
    }

    /// O reinício interno: a volta seguinte relê a URL pública gravada em Ajustes, e o processo
    /// continua respondendo `/saude` na mesma porta; o desligamento encerra o laço sem erro.
    #[tokio::test]
    async fn reinicio_rele_o_banco_e_desligamento_encerra() {
        let d = DirTemporario::new();
        let porta = std::net::TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port();
        let mut c = cfg(&d.0.join("dados"), &format!("127.0.0.1:{porta}"));
        c.public_url = format!("http://127.0.0.1:{porta}");
        let cofre = Arc::new(Cofre::new(&ChaveMestra::de(&gerar_chave_mestra().unwrap()).unwrap()));
        let (avisar, desligar) = watch::channel(false);
        type Voltas = Arc<Mutex<Vec<(String, Arc<Notify>)>>>;
        let voltas: Voltas = Arc::default();
        let v = voltas.clone();
        let tarefa =
            tokio::spawn(servir_ate(c, cofre, desligar, move |a| v.lock().unwrap().push((a.cfg.public_url.clone(), a.reiniciar.clone()))));

        let saude = async || {
            for _ in 0..100 {
                if let Ok(mut s) = tokio::net::TcpStream::connect(("127.0.0.1", porta)).await {
                    use tokio::io::{AsyncReadExt, AsyncWriteExt};
                    s.write_all(b"GET /saude HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n").await.unwrap();
                    let mut r = String::new();
                    s.read_to_string(&mut r).await.unwrap();
                    return r;
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
            panic!("não subiu");
        };
        let r = saude().await;
        assert!(r.starts_with("HTTP/1.1 200") && r.ends_with("\r\n\r\nok\n"), "{r}");
        assert!(r.contains("content-type: text/plain; charset=utf-8"), "{r}");

        // A tela de ajustes grava e pede o reinício; a volta seguinte lê a URL gravada.
        let st = Store::abrir(&d.0.join("dados")).await.unwrap();
        st.configuracoes().gravar(CHAVE_URL_PUBLICA, "https://gravada.exemplo").await.unwrap();
        st.fechar().await;
        let primeira = voltas.lock().unwrap()[0].1.clone();
        primeira.notify_one();
        for _ in 0..100 {
            if voltas.lock().unwrap().len() == 2 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(voltas.lock().unwrap().len(), 2, "não reiniciou");
        assert!(saude().await.ends_with("ok\n"));

        avisar.send(true).unwrap();
        tokio::time::timeout(Duration::from_secs(5), tarefa).await.expect("desligou").unwrap().expect("sem erro");
        let urls: Vec<String> = voltas.lock().unwrap().iter().map(|(u, _)| u.clone()).collect();
        assert_eq!(urls, [format!("http://127.0.0.1:{porta}"), "https://gravada.exemplo".into()]);
    }

    /// A URL explícita (flag ou ambiente) vence a gravada em Ajustes; sem ela, a gravada vence a
    /// derivada da escuta.
    #[tokio::test]
    async fn url_gravada_vence_a_da_escuta() {
        let d = DirTemporario::new();
        let cofre = Arc::new(Cofre::new(&ChaveMestra::de(&gerar_chave_mestra().unwrap()).unwrap()));
        let mut c = cfg(&d.0, "127.0.0.1:1");
        c.public_url = "http://127.0.0.1:1".into();
        c.public_url_explicita = true;
        let a = montar(c.clone(), cofre.clone()).await.unwrap();
        a.st.configuracoes().gravar(CHAVE_URL_PUBLICA, "https://gravada.exemplo").await.unwrap();
        a.st.fechar().await;
        assert_eq!(montar(c.clone(), cofre.clone()).await.unwrap().cfg.public_url, "http://127.0.0.1:1");
        c.public_url_explicita = false;
        assert_eq!(montar(c, cofre).await.unwrap().cfg.public_url, "https://gravada.exemplo");
    }
}
