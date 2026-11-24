//! shed-core — the pure Rust shed-server protocol client + wire DTOs.
//!
//! Clients link this crate directly. Decoders in `models` must
//! reproduce the defensive semantics documented there exactly.

pub mod authfail;
pub mod certuser;
pub mod config;
pub mod create;
pub mod csr;
pub mod http;
pub mod identityfile;
pub mod models;
pub mod sse;
pub mod terminal;
#[cfg(test)]
mod testtls;
pub mod tls;
pub mod token;

/// M0 canary health check. Async so an async-over-tokio caller path is proven
/// early.
pub async fn ping(echo: String) -> String {
    format!("shed-core ok: {echo}")
}

#[cfg(test)]
mod tests {
    #[tokio::test]
    async fn ping_echoes() {
        assert_eq!(super::ping("hi".into()).await, "shed-core ok: hi");
    }
}
