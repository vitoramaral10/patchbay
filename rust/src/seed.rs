//! `patchbay seed` (`cmd/patchbay/seed.go`) até onde a F1 alcança: as flags, o portão da chave
//! mestra, o banco e o canário, e a validação das opções. Gravar endpoint, upstream e chave de API
//! depende de `apikey` (F3) e entra com a F6.

use std::path::Path;

use crate::cli::{Flags, PedidoDeAjuda};
use crate::plataforma::{log, store::Store};
use crate::{chave, config};

pub async fn comando_seed(args: &[String], chave_do_ambiente: Option<&[u8]>) -> anyhow::Result<()> {
    let mut f = Flags::new("patchbay seed");
    f.texto("endpoint", "pessoal", "slug do endpoint a criar");
    f.texto("upstream", "exemplo", "nome do upstream a criar");
    f.texto("upstream-url", "", "URL do upstream MCP Streamable HTTP sem auth");
    f.texto("nome-chave", "desenvolvimento", "nome da chave de API emitida");
    f.inteiro("timeout-ms", 15000, "timeout de toda operação daquele upstream");
    // As flags comuns no mesmo conjunto, para o seed aceitar -data-dir e -public-url como o serve.
    config::registrar(&mut f);
    f.analisar(args).map_err(|e| if e.is::<PedidoDeAjuda>() { e } else { e.context("seed") })?;
    let cfg = config::resolver(&f)?;
    log::iniciar(cfg.nivel_log, cfg.log_em_texto);

    // O seed toca o mesmo banco que o serve e passa pelo mesmo portão: sem chave não abre, e com a
    // chave errada o canário barra antes de qualquer escrita.
    let cofre = chave::cofre_do_boot(&cfg.data_dir, chave_do_ambiente)?;
    chave::criar_diretorio(Path::new(&cfg.data_dir))?;
    let st = Store::abrir(Path::new(&cfg.data_dir)).await?;
    let r = async {
        chave::verificar_canario(&cofre, &st).await?;
        anyhow::ensure!(!f.ler_texto("upstream-url").is_empty(), "seed: --upstream-url é obrigatório");
        anyhow::ensure!(f.ler_inteiro("timeout-ms") > 0, "seed: --timeout-ms precisa ser positivo");
        anyhow::bail!("seed: ainda não portado para Rust (endpoint, upstream e chave de API entram com a F6)")
    }
    .await;
    st.fechar().await;
    r
}
