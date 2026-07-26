//! Test-only raw TLS alert emitter: answers any ClientHello with a single
//! fatal alert record of a chosen description, counting accepted connections.
//!
//! A miniature of shed-core's `testtls::spawn_alert_server` (which is
//! `cfg(test)`-private to that crate). TLS alerts are protocol constants, so
//! the rendering a rustls CLIENT produces — `received fatal alert: …`, the
//! shape [`shed_core::authfail::is_auth_shaped_message`] classifies — is
//! identical whether the alert came from this emitter or from the production
//! Go server refusing a client certificate.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// TLS alert 116: the TLS 1.3 "certificate required" a `RequireAndVerify`
/// server sends when no client certificate is presented.
pub const CERTIFICATE_REQUIRED: u8 = 116;

/// The spawned emitter. The listener task runs until the test's runtime drops.
pub struct AlertServer {
    addr: std::net::SocketAddr,
    accepts: Arc<AtomicUsize>,
}

impl AlertServer {
    pub async fn spawn(description: u8) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let accepts = Arc::new(AtomicUsize::new(0));
        let counter = accepts.clone();
        tokio::spawn(async move {
            while let Ok((mut sock, _)) = listener.accept().await {
                counter.fetch_add(1, Ordering::SeqCst);
                tokio::spawn(async move {
                    let mut buf = [0u8; 4096];
                    let _ = sock.read(&mut buf).await;
                    // record: alert(21), TLS 1.2 record version, len 2, fatal(2), desc
                    let alert = [0x15, 0x03, 0x03, 0x00, 0x02, 0x02, description];
                    let _ = sock.write_all(&alert).await;
                    let _ = sock.flush().await;
                    // Give the peer a beat to read the alert before the socket drops.
                    tokio::time::sleep(Duration::from_millis(50)).await;
                });
            }
        });
        Self { addr, accepts }
    }

    /// An `https://` base URL for the emitter — TLS is exactly what it speaks
    /// (one alert's worth of it).
    pub fn base_url(&self) -> String {
        format!("https://{}", self.addr)
    }

    /// How many connections were accepted — the observable "send" count for
    /// asserting retry bounds.
    pub fn accepts(&self) -> usize {
        self.accepts.load(Ordering::SeqCst)
    }
}
