//! Test-only TLS servers: a real in-process mtls listener and a raw-socket alert
//! emitter. Compiled only under `cfg(test)`.
//!
//! Two shapes, because the classifier ([`crate::authfail`]) has to be right about
//! errors produced by a **Go** server it cannot host here:
//!
//! * [`TlsServer`] is a genuine rustls listener that demands a client certificate
//!   (`WebPkiClientVerifier`), speaks HTTP/1.1 well enough for reqwest, and can be
//!   pinned to TLS 1.2 or 1.3. It proves the whole client path end to end —
//!   handshake, certificate presentation, rotation across a forced reconnect.
//! * [`spawn_alert_server`] answers a ClientHello with one raw TLS alert record of
//!   a chosen description. TLS alerts are protocol constants, so the rendering a
//!   rustls CLIENT produces for "the peer sent alert N" is identical whether N came
//!   from rustls or from Go's crypto/tls — which is what lets the alert-shape
//!   findings recorded in `authfail.rs` cover the production Go server, including
//!   the TLS 1.2 no-certificate case (alert 40) that a rustls server never emits.

use std::io;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::server::WebPkiClientVerifier;
use rustls::{RootCertStore, ServerConfig};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::task::JoinHandle;

/// A throwaway CA that issues the test server's leaf and the client certificates.
pub struct TestCa {
    cert: rcgen::Certificate,
    key: rcgen::KeyPair,
}

/// One issued certificate plus the PKCS#8 key it belongs to.
pub struct IssuedCert {
    pub cert_pem: String,
    pub key_pkcs8_der: Vec<u8>,
    pub der: Vec<u8>,
}

impl TestCa {
    pub fn new(name: &str) -> Self {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Constrained(0));
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, name);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let cert = params.self_signed(&key).unwrap();
        Self { cert, key }
    }

    pub fn cert_der(&self) -> CertificateDer<'static> {
        self.cert.der().clone()
    }

    /// Issue a server leaf for `localhost` (SAN + IP, so any dial name works).
    pub fn server_cert(&self) -> IssuedCert {
        let mut params =
            rcgen::CertificateParams::new(vec!["localhost".to_string(), "127.0.0.1".to_string()])
                .unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "shed-test-server");
        params.use_authority_key_identifier_extension = true;
        self.issue(params)
    }

    /// Issue a client certificate with a shed-shaped subject
    /// (`CN=<ssh-fingerprint> OU=<scope> O=<kind>`) and an explicit validity
    /// window, so an EXPIRED credential can be produced on demand.
    pub fn client_cert(
        &self,
        cn: &str,
        scope: &str,
        validity: (SystemTime, SystemTime),
    ) -> IssuedCert {
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, cn);
        params
            .distinguished_name
            .push(rcgen::DnType::OrganizationalUnitName, scope);
        params
            .distinguished_name
            .push(rcgen::DnType::OrganizationName, "cli");
        params.use_authority_key_identifier_extension = true;
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        params.not_before = validity.0.into();
        params.not_after = validity.1.into();
        self.issue(params)
    }

    /// Sign a CSR the way the shed-server's CA does: take ONLY the public key out
    /// of it and compose the subject from server-side knowledge — every field the
    /// CSR requested is discarded (plan 001 D4). Returns the leaf PEM + serial.
    ///
    /// The public key is lifted out of the CSR's SubjectPublicKeyInfo by locating
    /// the P-256 `BIT STRING` (`03 42 00` + the 65-byte uncompressed point) rather
    /// than with an X.509 parser: rcgen's own CSR reader needs its `x509-parser`
    /// feature, which would pull a parser into this workspace's lock for test
    /// convenience alone. The extraction is self-checking — a wrong key produces a
    /// certificate whose pairing `certified_key_from_pem` rejects.
    pub fn sign_csr(
        &self,
        csr_der: &[u8],
        cn: &str,
        scope: &str,
        serial: u64,
        validity: (SystemTime, SystemTime),
    ) -> (String, String) {
        let point = p256_public_point(csr_der).expect("P-256 SPKI in CSR");
        struct RawP256(Vec<u8>);
        impl rcgen::PublicKeyData for RawP256 {
            fn der_bytes(&self) -> &[u8] {
                &self.0
            }
            fn algorithm(&self) -> &'static rcgen::SignatureAlgorithm {
                &rcgen::PKCS_ECDSA_P256_SHA256
            }
        }
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, cn);
        params
            .distinguished_name
            .push(rcgen::DnType::OrganizationalUnitName, scope);
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        params.serial_number = Some(rcgen::SerialNumber::from(serial));
        params.not_before = validity.0.into();
        params.not_after = validity.1.into();
        let cert = params
            .signed_by(&RawP256(point), &self.cert, &self.key)
            .unwrap();
        (
            crate::csr::pem_encode(crate::csr::PEM_LABEL_CERTIFICATE, cert.der()),
            format!("{serial:x}"),
        )
    }

    fn issue(&self, params: rcgen::CertificateParams) -> IssuedCert {
        let key = rcgen::KeyPair::generate().unwrap();
        let cert = params.signed_by(&key, &self.cert, &self.key).unwrap();
        IssuedCert {
            // rcgen's own `pem` feature is deliberately off (see csr.rs), so the
            // fixture uses shed-core's PEM writer — which also exercises it.
            cert_pem: crate::csr::pem_encode(crate::csr::PEM_LABEL_CERTIFICATE, cert.der()),
            key_pkcs8_der: key.serialize_der(),
            der: cert.der().to_vec(),
        }
    }
}

/// The 65-byte uncompressed P-256 point inside a DER SubjectPublicKeyInfo:
/// `BIT STRING` of length 0x42 with 0 unused bits, whose content starts with the
/// uncompressed-point tag `0x04`.
fn p256_public_point(der: &[u8]) -> Option<Vec<u8>> {
    der.windows(4)
        .position(|w| w == [0x03, 0x42, 0x00, 0x04])
        .map(|i| der[i + 3..i + 3 + 65].to_vec())
}

/// Which TLS versions the test listener offers.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum TlsVersion {
    V12,
    V13,
    Any,
}

/// A running in-process HTTPS server that requires a client certificate.
pub struct TlsServer {
    pub addr: std::net::SocketAddr,
    pub pin: String,
    /// Number of completed TLS handshakes — i.e. how many times a connection was
    /// actually dialed rather than served from reqwest's pool.
    pub handshakes: Arc<AtomicUsize>,
    /// The client-certificate serials seen, in arrival order (hex of the DER's
    /// serial as rendered by the CN we put in — see [`Self::client_cns`]).
    pub seen_cns: Arc<std::sync::Mutex<Vec<String>>>,
    /// The posture this listener enforces per request — flippable at runtime by
    /// [`Self::set_mode`], which is the whole point of [`spawn_flip_server`].
    pub mode: Arc<std::sync::Mutex<Option<ServerAuthMode>>>,
    task: JoinHandle<()>,
}

impl Drop for TlsServer {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl TlsServer {
    pub fn client_cns(&self) -> Vec<String> {
        self.seen_cns.lock().unwrap().clone()
    }

    /// Flip the server's auth posture — the operator action plan 001 D5's
    /// migration story is about.
    pub fn set_mode(&self, mode: ServerAuthMode) {
        *self.mode.lock().unwrap() = Some(mode);
    }

    pub fn handshake_count(&self) -> usize {
        self.handshakes.load(Ordering::SeqCst)
    }

    pub fn base_url(&self) -> String {
        format!("https://localhost:{}", self.addr.port())
    }
}

/// How a [`spawn_server`] listener behaves. `Default` is the plain
/// certificate-requiring keep-alive listener.
#[derive(Clone, Debug)]
pub struct ServerOpts {
    pub version: TlsVersion,
    /// Send `Connection: close` and drop the connection after one response — a
    /// FORCED reconnect, so the next request re-handshakes.
    ///
    /// Leave it OFF for anything asserting on pooled-connection behavior: a
    /// closing server hides every bug that only exists because a connection (and
    /// with it, a client identity) is reused.
    pub close_after_each: bool,
    /// Per-request authorization posture. `None` authorizes everything that got
    /// through the handshake.
    pub auth_mode: Option<ServerAuthMode>,
    /// Complete the handshake even when the client presents no certificate
    /// (required for a listener that serves the token posture too).
    pub allow_unauthenticated: bool,
    /// Wait this long after ACCEPTing the TCP connection before starting the TLS
    /// handshake. It opens a deterministic window between the client's
    /// ClientHello and the server's CertificateRequest — the seam
    /// `tls::certificate_installed_mid_handshake_is_presented_on_tls12` needs.
    pub handshake_delay: Duration,
}

impl Default for ServerOpts {
    fn default() -> Self {
        Self {
            version: TlsVersion::Any,
            close_after_each: false,
            auth_mode: None,
            allow_unauthenticated: false,
            handshake_delay: Duration::ZERO,
        }
    }
}

/// Start an HTTPS listener that requires a client certificate signed by `ca`.
///
/// `close_after_each` makes the server send `Connection: close` and drop the
/// connection after one response — the "forced reconnect" the rotation test needs
/// so the next request re-handshakes and presents the NEW certificate.
pub async fn spawn_mtls_server(
    ca: &TestCa,
    version: TlsVersion,
    close_after_each: bool,
) -> TlsServer {
    spawn_server(
        ca,
        ServerOpts {
            version,
            close_after_each,
            ..ServerOpts::default()
        },
    )
    .await
}

/// What a [`spawn_flip_server`] listener demands of a request.
#[derive(Clone, PartialEq, Eq, Debug)]
pub enum ServerAuthMode {
    /// mtls: a client certificate is required; a request without one gets 401.
    Mtls,
    /// mtls with a LIVE allowlist, i.e. the Go middleware's per-request
    /// re-validation: the handshake accepts any CA-issued certificate, and each
    /// request is authorized only while the CN it presented is still listed.
    /// Swapping the list mid-connection revokes an identity on a POOLED
    /// connection — the case `Connection: close` (server side) and the
    /// fresh-transport retry (client side) exist for.
    MtlsAllow(Vec<String>),
    /// token: a `Authorization: Bearer <expected>` header is required; any
    /// certificate presented is IGNORED (parity with the Go middleware, which
    /// does not read the header in mtls mode and does not read the certificate in
    /// token mode).
    Token(String),
}

/// A listener that can be FLIPPED between token and mtls at runtime — the
/// client-side half of plan 001 D5's migration story.
///
/// Client certificates are `allow_unauthenticated` here, so one live listener can
/// serve both postures without a rebind. That is a deliberate simplification of
/// the real server (which would reject a certificate-less peer during the
/// handshake in mtls mode): what this exercises is the CLIENT's adaptation —
/// 401 → re-mint → adopt whatever shape came back → retry — not the server's
/// handshake enforcement, which `spawn_mtls_server` covers with a genuine
/// `RequireAndVerifyClientCert` listener.
///
/// `close_after_each` forces a reconnect per request. Pass `false` to exercise
/// the POOLED path, where a rejected identity would otherwise be re-presented on
/// the retry.
pub async fn spawn_flip_server(
    ca: &TestCa,
    mode: ServerAuthMode,
    close_after_each: bool,
) -> TlsServer {
    spawn_server(
        ca,
        ServerOpts {
            close_after_each,
            auth_mode: Some(mode),
            allow_unauthenticated: true,
            ..ServerOpts::default()
        },
    )
    .await
}

/// The general listener constructor: [`spawn_mtls_server`] and
/// [`spawn_flip_server`] are the two named shapes of it.
pub async fn spawn_server(ca: &TestCa, opts: ServerOpts) -> TlsServer {
    let leaf = ca.server_cert();
    let mut roots = RootCertStore::empty();
    roots.add(ca.cert_der()).unwrap();
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut verifier =
        WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider.clone());
    if opts.allow_unauthenticated {
        verifier = verifier.allow_unauthenticated();
    }
    let versions: &[&'static rustls::SupportedProtocolVersion] = match opts.version {
        TlsVersion::V12 => &[&rustls::version::TLS12],
        TlsVersion::V13 => &[&rustls::version::TLS13],
        TlsVersion::Any => &[&rustls::version::TLS12, &rustls::version::TLS13],
    };
    let config = ServerConfig::builder_with_provider(provider)
        .with_protocol_versions(versions)
        .unwrap()
        .with_client_cert_verifier(verifier.build().unwrap())
        .with_single_cert(
            vec![CertificateDer::from(leaf.der.clone())],
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(leaf.key_pkcs8_der.clone())),
        )
        .unwrap();
    spawn_with_config(config, crate::tls::fingerprint(&leaf.der), opts).await
}

async fn spawn_with_config(config: ServerConfig, pin: String, opts: ServerOpts) -> TlsServer {
    let close_after_each = opts.close_after_each;
    let auth_mode = opts.auth_mode.clone();
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(config));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handshakes = Arc::new(AtomicUsize::new(0));
    let seen_cns = Arc::new(std::sync::Mutex::new(Vec::new()));
    let mode = Arc::new(std::sync::Mutex::new(auth_mode));
    let task = {
        let handshakes = handshakes.clone();
        let seen_cns = seen_cns.clone();
        let mode = mode.clone();
        tokio::spawn(async move {
            loop {
                let Ok((sock, _)) = listener.accept().await else {
                    return;
                };
                let acceptor = acceptor.clone();
                let handshakes = handshakes.clone();
                let seen_cns = seen_cns.clone();
                let mode = mode.clone();
                let handshake_delay = opts.handshake_delay;
                tokio::spawn(async move {
                    if !handshake_delay.is_zero() {
                        // The client's ClientHello is already on the wire; every
                        // later handshake message waits for us.
                        tokio::time::sleep(handshake_delay).await;
                    }
                    let Ok(mut tls) = acceptor.accept(sock).await else {
                        return;
                    };
                    handshakes.fetch_add(1, Ordering::SeqCst);
                    let mut peer_cn = None;
                    if let Some(certs) = tls.get_ref().1.peer_certificates() {
                        if let Some(first) = certs.first() {
                            let cn = common_name(first);
                            seen_cns.lock().unwrap().push(cn.clone());
                            peer_cn = Some(cn);
                        }
                    }
                    loop {
                        let mut buf = [0u8; 4096];
                        let head = match tls.read(&mut buf).await {
                            Ok(0) | Err(_) => return,
                            Ok(n) => String::from_utf8_lossy(&buf[..n]).to_string(),
                        };
                        let status = match &*mode.lock().unwrap() {
                            None => None,
                            Some(ServerAuthMode::Mtls) => {
                                // A certificate is the credential; the header is
                                // never read (Go middleware parity).
                                peer_cn.is_none().then_some(401)
                            }
                            Some(ServerAuthMode::MtlsAllow(allowed)) => {
                                // Re-derived per request off the certificate this
                                // CONNECTION presented at its handshake — which is
                                // exactly why revocation lands on a pooled
                                // connection and cannot be fixed by re-sending on
                                // it.
                                match &peer_cn {
                                    Some(cn) if allowed.contains(cn) => None,
                                    _ => Some(401),
                                }
                            }
                            Some(ServerAuthMode::Token(expected)) => {
                                let want = format!("authorization: bearer {expected}");
                                (!head.to_lowercase().contains(&want)).then_some(401)
                            }
                        };
                        // An SSE route answers with an event stream that ENDS
                        // (clean EOF), so a client's stream loop terminates
                        // instead of waiting out its idle timer.
                        let sse_body = if status.is_none() {
                            sse_body_for(&head)
                        } else {
                            None
                        };
                        let sse = sse_body.is_some();
                        let body = match (status, sse_body) {
                            (Some(code), _) => format!("{{\"status\":{code}}}"),
                            (None, Some(sse)) => sse.to_string(),
                            (None, None) => "{\"ok\":true}".to_string(),
                        };
                        let code = status.unwrap_or(200);
                        // An SSE response is terminated by closing, like the real
                        // server's stream teardown.
                        let close = close_after_each || sse;
                        let conn = if close { "close" } else { "keep-alive" };
                        let ctype = if sse {
                            "text/event-stream"
                        } else {
                            "application/json"
                        };
                        let resp = format!(
                            "HTTP/1.1 {code} X\r\nContent-Type: {ctype}\r\nContent-Length: {}\r\nConnection: {conn}\r\n\r\n{body}",
                            body.len()
                        );
                        if tls.write_all(resp.as_bytes()).await.is_err() {
                            return;
                        }
                        let _ = tls.flush().await;
                        if close {
                            let _ = tls.shutdown().await;
                            return;
                        }
                    }
                });
            }
        })
    };
    TlsServer {
        addr,
        pin,
        handshakes,
        seen_cns,
        mode,
        task,
    }
}

/// The event stream this listener serves on the two SSE routes, or `None` for
/// every other request (which gets the plain JSON body).
///
/// Both streams are short and self-terminating: the point of serving them over a
/// real mtls listener is the AUTH path around the stream (reject → re-mint →
/// reconnect), not the SSE parser, which the httpmock-based tests already cover
/// frame by frame.
fn sse_body_for(head: &str) -> Option<&'static str> {
    let line = head.lines().next().unwrap_or_default();
    if line.starts_with("GET /api/rc/events") {
        return Some(
            ": ok\n\n\
             event: activity.changed\n\
             data: {\"shed\":\"proj\",\"slug\":\"cdx777\",\"activity\":\"working\",\"state\":\"ready\"}\n\n",
        );
    }
    if line.starts_with("POST /api/sheds ") {
        return Some(
            "event: progress\ndata: {\"message\":\"building\"}\n\n\
             event: complete\ndata: {\"name\":\"folio\",\"status\":\"running\"}\n\n",
        );
    }
    None
}

/// Pull the CN out of a DER certificate without an X.509 parser: the subject RDN
/// values are the last printable strings before the extensions, and the test
/// certificates put a unique marker in the CN. Scanning for the marker keeps this
/// helper dependency-free (no x509-parser in shed-core).
fn common_name(der: &CertificateDer<'_>) -> String {
    let bytes = der.as_ref();
    let mut out = String::new();
    let mut i = 0;
    while i + 2 < bytes.len() {
        // UTF8String (0x0c) or PrintableString (0x13) with a short length.
        if (bytes[i] == 0x0c || bytes[i] == 0x13) && bytes[i + 1] as usize + i + 2 <= bytes.len() {
            let len = bytes[i + 1] as usize;
            if let Ok(s) = std::str::from_utf8(&bytes[i + 2..i + 2 + len]) {
                if s.starts_with("SHA256:") {
                    out = s.to_string();
                }
            }
            i += 2 + len;
        } else {
            i += 1;
        }
    }
    out
}

/// Accept one TCP connection, read the ClientHello, and answer with a single
/// FATAL TLS alert record of `description`.
///
/// This is how the alert renderings in [`crate::authfail`] were derived for alert
/// codes a rustls server will not produce (notably 40, `handshake_failure`, which
/// is what a Go TLS 1.2 server sends when no client certificate is presented).
pub async fn spawn_alert_server(description: u8) -> (std::net::SocketAddr, JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        while let Ok((mut sock, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut buf = [0u8; 4096];
                let _: io::Result<usize> = sock.read(&mut buf).await;
                // record: alert(21), TLS 1.2 record version, len 2, fatal(2), desc
                let alert = [0x15, 0x03, 0x03, 0x00, 0x02, 0x02, description];
                let _ = sock.write_all(&alert).await;
                let _ = sock.flush().await;
                tokio::time::sleep(Duration::from_millis(50)).await;
            });
        }
    });
    (addr, task)
}

/// `(not_before, not_after)` spanning now — a valid certificate.
pub fn valid_window() -> (SystemTime, SystemTime) {
    (
        SystemTime::now() - Duration::from_secs(3600),
        SystemTime::now() + Duration::from_secs(3600),
    )
}

/// `(not_before, not_after)` entirely in the past — an expired certificate.
pub fn expired_window() -> (SystemTime, SystemTime) {
    (
        SystemTime::now() - Duration::from_secs(7200),
        SystemTime::now() - Duration::from_secs(3600),
    )
}
