//! O binário patchbay em Rust. Na F0 ele só imprime a versão: a fatia é a caixa-preta
//! (`tests/caixa_preta.rs`), que roda contra o binário Go até as fatias seguintes portarem o resto.

fn main() {
    println!("patchbay {}", env!("CARGO_PKG_VERSION"));
}
