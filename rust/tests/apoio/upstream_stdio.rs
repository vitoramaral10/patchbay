//! O upstream STDIO falso da caixa-preta: o patchbay o executa como um MCP de processo local e fala
//! com ele por stdin/stdout. O catálogo é o [`Perfil::Stdio`] de `falso_mcp.rs`.

#[path = "falso_mcp.rs"]
mod falso_mcp;

use rmcp::ServiceExt;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    let servidor = falso_mcp::Falso::new(falso_mcp::Perfil::Stdio);
    match servidor.serve(rmcp::transport::stdio()).await {
        // Sai quando o patchbay fecha o stdin.
        Ok(rodando) => drop(rodando.waiting().await),
        Err(e) => eprintln!("patchbay-upstream-falso: {e}"),
    }
}
