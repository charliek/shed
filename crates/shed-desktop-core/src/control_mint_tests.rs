//! The cold-launch mint with no broker (plan 039 criterion 13, the unit-level half): a
//! `Backend` built with [`super::control_minter_over`] enrols against an `mtls` server by
//! relaying its CSR over the SSH bootstrap seam (a fake runner standing in for `ssh`,
//! behind the real `CredentialMinter` with its `known_hosts` pin check), lists that
//! server's sheds with the issued certificate, and an existing `machine.json` and
//! credential store are used as they are.

use std::collections::HashSet;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::server::WebPkiClientVerifier;
use rustls::{RootCertStore, ServerConfig};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

use shed_app::Backend;
use shed_core::csr::{pem_encode, PEM_LABEL_CERTIFICATE};
use shed_source::minter::{CredentialMinter, Minted, Minter, MinterError, SCOPE_CREDENTIALS};
use shed_source::test_support::{BootstrapError, BootstrapRunner, Bundle, Params};
use shed_source::ServerTarget;

/// A throwaway host key: the pin only has to parse and name the endpoint.
const HOST_KEY: &str =
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKyehkOrwZvXnDGLa0WIR+scxRoxwOrQio9RtxMcttu/";
const SSH_PORT: u16 = 2222;
const USER: &str = "pat";
const DAY: u64 = 24 * 3600;

/// The server's CA: signs the server's leaf and every CSR the bootstrap relays.
struct Ca {
    cert: rcgen::Certificate,
    key: rcgen::KeyPair,
}

impl Ca {
    fn new() -> Self {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Constrained(0));
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "shed-test-ca");
        params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
        let cert = params.self_signed(&key).unwrap();
        Ca { cert, key }
    }

    /// A leaf for `localhost`: its DER and PKCS#8 key.
    fn server_leaf(&self) -> (Vec<u8>, Vec<u8>) {
        let key = rcgen::KeyPair::generate().unwrap();
        let params =
            rcgen::CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()]).unwrap();
        let cert = params.signed_by(&key, &self.cert, &self.key).unwrap();
        (cert.der().to_vec(), key.serialize_der())
    }

    /// A shed-shaped client certificate (`CN`, `OU=<scope>`, the userId attribute) for
    /// `public`, valid for a day (past every refresh window).
    fn client_cert(
        &self,
        public: &impl rcgen::PublicKeyData,
        cn: &str,
        scope: &str,
        serial: u64,
    ) -> String {
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, cn);
        params
            .distinguished_name
            .push(rcgen::DnType::OrganizationalUnitName, scope);
        // userId, 0.9.2342.19200300.100.1.1: a 0.10 server always names the user.
        params.distinguished_name.push(
            rcgen::DnType::CustomDnType(vec![0, 9, 2342, 19_200_300, 100, 1, 1]),
            rcgen::DnValue::PrintableString(USER.try_into().unwrap()),
        );
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        params.serial_number = Some(rcgen::SerialNumber::from(serial));
        let now = SystemTime::now();
        params.not_before = (now - Duration::from_secs(60)).into();
        params.not_after = (now + Duration::from_secs(DAY)).into();
        let cert = params.signed_by(public, &self.cert, &self.key).unwrap();
        pem_encode(PEM_LABEL_CERTIFICATE, cert.der())
    }

    /// Signs a CSR the way the server's CA does: only its P-256 public key is taken.
    fn sign_csr(&self, csr_base64: &str, cn: &str, scope: &str, serial: u64) -> String {
        use base64::Engine as _;
        let der = base64::engine::general_purpose::STANDARD
            .decode(csr_base64)
            .expect("the CSR is standard base64");
        // The uncompressed point of the SPKI's BIT STRING (`03 42 00 04 …`).
        let at = der
            .windows(4)
            .position(|w| w == [0x03, 0x42, 0x00, 0x04])
            .expect("a P-256 key in the CSR");
        struct RawP256(Vec<u8>);
        impl rcgen::PublicKeyData for RawP256 {
            fn der_bytes(&self) -> &[u8] {
                &self.0
            }
            fn algorithm(&self) -> &'static rcgen::SignatureAlgorithm {
                &rcgen::PKCS_ECDSA_P256_SHA256
            }
        }
        self.client_cert(
            &RawP256(der[at + 3..at + 3 + 65].to_vec()),
            cn,
            scope,
            serial,
        )
    }
}

/// An HTTPS server that requires a client certificate the CA issued, answering every
/// request with an empty shed list; it records each handshake's client CN.
struct MtlsServer {
    port: u16,
    pin: String,
    client_cns: Arc<Mutex<Vec<String>>>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for MtlsServer {
    fn drop(&mut self) {
        self.task.abort();
    }
}

async fn mtls_server(ca: &Ca) -> MtlsServer {
    let (leaf, key) = ca.server_leaf();
    let mut roots = RootCertStore::empty();
    roots
        .add(CertificateDer::from(ca.cert.der().to_vec()))
        .unwrap();
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider.clone())
        .build()
        .unwrap();
    let config = ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_client_cert_verifier(verifier)
        .with_single_cert(
            vec![CertificateDer::from(leaf.clone())],
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key)),
        )
        .unwrap();
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(config));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let client_cns = Arc::new(Mutex::new(Vec::new()));
    let cns = client_cns.clone();
    let task = tokio::spawn(async move {
        loop {
            let Ok((sock, _)) = listener.accept().await else {
                return;
            };
            let acceptor = acceptor.clone();
            let cns = cns.clone();
            tokio::spawn(async move {
                let Ok(mut tls) = acceptor.accept(sock).await else {
                    return;
                };
                let cn = tls
                    .get_ref()
                    .1
                    .peer_certificates()
                    .and_then(|c| c.first())
                    .map(|c| common_name(c.as_ref()))
                    .unwrap_or_default();
                cns.lock().unwrap().push(cn);
                let mut buf = Vec::new();
                let mut chunk = [0u8; 4096];
                while !buf.windows(4).any(|w| w == b"\r\n\r\n") {
                    match tls.read(&mut chunk).await {
                        Ok(0) | Err(_) => return,
                        Ok(n) => buf.extend_from_slice(&chunk[..n]),
                    }
                }
                let body = r#"{"sheds":[]}"#;
                let reply = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                );
                let _ = tls.write_all(reply.as_bytes()).await;
                let _ = tls.shutdown().await;
            });
        }
    });
    MtlsServer {
        port,
        pin: shed_core::tls::fingerprint(&leaf),
        client_cns,
        task,
    }
}

/// The subject's CN in a certificate's DER: the last `06 03 55 04 03` (the issuer's
/// comes first), then a string.
fn common_name(der: &[u8]) -> String {
    let at = der
        .windows(5)
        .rposition(|w| w == [0x06, 0x03, 0x55, 0x04, 0x03])
        .expect("a CN");
    let len = der[at + 6] as usize;
    String::from_utf8_lossy(&der[at + 7..at + 7 + len]).into_owned()
}

/// Stands in for `ssh … _bootstrap`: records what it was asked and has the CA sign the
/// relayed CSR, as an `auth.mode: mtls` server does.
struct FakeBootstrap {
    ca: Arc<Ca>,
    calls: Mutex<Vec<Params>>,
}

#[async_trait::async_trait]
impl BootstrapRunner for FakeBootstrap {
    async fn run(&self, params: &Params) -> Result<Bundle, BootstrapError> {
        self.calls.lock().unwrap().push(params.clone());
        let serial = self.calls.lock().unwrap().len() as u64 + 0x10;
        let cert = self
            .ca
            .sign_csr(&params.csr, "SHA256:desktop", &params.scope, serial);
        let expires = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64
            + DAY as i64;
        Ok(Bundle {
            auth_mode: "mtls".into(),
            http_port: 0,
            https_port: 18443,
            tls_cert_fingerprint: String::new(),
            token: String::new(),
            client_cert: cert,
            scope: params.scope.clone(),
            token_id: String::new(),
            cert_serial: format!("{serial:x}"),
            expires_at: Some(expires),
        })
    }
}

/// A home with the shed CLI's config naming the server and its pinned SSH endpoint.
fn home(server: &MtlsServer) -> (tempfile::TempDir, String, String) {
    home_with(server, "servers:\n")
}

/// [`home`] with the config's `servers:` line written as `servers` (a leading document
/// marker, a trailing comment: forms the core reader reads).
fn home_with(server: &MtlsServer, servers: &str) -> (tempfile::TempDir, String, String) {
    let dir = tempfile::tempdir().unwrap();
    let config = dir.path().join("config.yaml");
    std::fs::write(
        &config,
        format!(
            "{servers}    prod:\n        host: 127.0.0.1\n        ssh_port: {SSH_PORT}\n        api_url: https://localhost:{}\n        tls_cert_fingerprint: {}\n        auth_mode: mtls\n",
            server.port, server.pin
        ),
    )
    .unwrap();
    let known_hosts = dir.path().join("known_hosts");
    std::fs::write(&known_hosts, format!("[127.0.0.1]:{SSH_PORT} {HOST_KEY}\n")).unwrap();
    (
        dir,
        config.to_string_lossy().into_owned(),
        known_hosts.to_string_lossy().into_owned(),
    )
}

/// A cold launch with no broker: the Backend has no certificate, so its first request
/// enrols through the source minter, which relays the Backend's own CSR in the control
/// scope over the SSH bootstrap (pinned endpoint, nothing else), and the server sees the
/// issued certificate. The certificate is kept: the next listing does not mint again.
#[tokio::test]
async fn a_cold_launch_enrols_over_the_ssh_bootstrap_and_lists_sheds() {
    cold_launch_enrols("servers:\n").await;
}

/// The same cold launch from a config with a leading `---` document marker, and from
/// one whose `servers:` line carries a comment: the core reader reads both, so the
/// explicit `ssh_port` is the endpoint and the enrolment runs (review of plan 039 PR B:
/// the line scanner this replaced read port 0 there, "no ssh endpoint").
#[tokio::test]
async fn a_cold_launch_reads_a_document_marker_and_a_commented_servers_line() {
    cold_launch_enrols("---\nservers:\n").await;
    cold_launch_enrols("# shed\nservers: # the shed servers\n").await;
}

async fn cold_launch_enrols(servers: &str) {
    let ca = Arc::new(Ca::new());
    let server = mtls_server(&ca).await;
    let (_dir, config, known_hosts) = home_with(&server, servers);
    let runner = Arc::new(FakeBootstrap {
        ca: ca.clone(),
        calls: Mutex::new(Vec::new()),
    });
    let ssh: Arc<dyn Minter> =
        Arc::new(CredentialMinter::with_runner(&known_hosts, runner.clone()));
    let minter = super::control_minter_over(ssh, &config);
    assert!(minter.supports_mtls());
    let backend = Backend::from_env_parts_with_credentials(
        false,
        None,
        std::path::Path::new(&config),
        Some(&minter),
        None,
        &HashSet::new(),
    );

    let r = backend.refresh().await;
    assert!(r.host_errors.is_empty(), "{:?}", r.host_errors);
    assert!(r.sheds.is_empty());
    {
        let calls = runner.calls.lock().unwrap();
        assert_eq!(calls.len(), 1, "one enrolment");
        let p = &calls[0];
        assert_eq!((p.host.as_str(), p.port), ("127.0.0.1", SSH_PORT));
        assert_eq!(p.scope, "control");
        assert_eq!(p.known_hosts_path, known_hosts);
        assert!(!p.csr.is_empty(), "the Backend's CSR is relayed");
    }
    assert_eq!(
        server.client_cns.lock().unwrap().as_slice(),
        ["SHA256:desktop"],
        "the server saw the issued certificate"
    );

    let r = backend.refresh().await;
    assert!(r.host_errors.is_empty(), "{:?}", r.host_errors);
    assert_eq!(runner.calls.lock().unwrap().len(), 1, "no second mint");
    assert_eq!(server.client_cns.lock().unwrap().len(), 2);
}

/// A server with no explicit `ssh_port` has no SSH endpoint: nothing is minted, and the
/// listing fails with the server's refusal rather than a downgrade.
#[tokio::test]
async fn an_entry_without_an_ssh_port_never_runs_the_bootstrap() {
    let ca = Arc::new(Ca::new());
    let server = mtls_server(&ca).await;
    let (dir, _, known_hosts) = home(&server);
    let config = dir.path().join("noport.yaml");
    std::fs::write(
        &config,
        format!(
            "servers:\n    prod:\n        host: 127.0.0.1\n        api_url: https://localhost:{}\n        tls_cert_fingerprint: {}\n        auth_mode: mtls\n",
            server.port, server.pin
        ),
    )
    .unwrap();
    let runner = Arc::new(FakeBootstrap {
        ca,
        calls: Mutex::new(Vec::new()),
    });
    let ssh: Arc<dyn Minter> =
        Arc::new(CredentialMinter::with_runner(&known_hosts, runner.clone()));
    let minter = super::control_minter_over(ssh, &config.to_string_lossy());
    let backend = Backend::from_env_parts_with_credentials(
        false,
        None,
        &config,
        Some(&minter),
        None,
        &HashSet::new(),
    );
    let r = backend.refresh().await;
    assert_eq!(r.host_errors.len(), 1);
    assert!(runner.calls.lock().unwrap().is_empty());
}

/// A `machine.json` and a credentials-scope store left by the embedded broker are used
/// as they are: the identity is read, not rewritten, and the stored certificate is
/// presented without a mint. Both keep the broker's paths under the state directory.
#[tokio::test]
async fn an_existing_identity_and_credential_store_are_used_as_they_are() {
    use shed_source::credstore::CredStore;
    use shed_source::machine::{load_or_create_at, machine_state_path, LoadOutcome};

    let state = shed_source::paths::state_dir();
    assert_eq!(
        machine_state_path().strip_prefix(&state).unwrap(),
        std::path::Path::new("broker/machine.json")
    );
    assert_eq!(
        CredStore::for_scope(SCOPE_CREDENTIALS)
            .root()
            .strip_prefix(&state)
            .unwrap(),
        std::path::Path::new("broker/creds/credentials")
    );

    let dir = tempfile::tempdir().unwrap();
    let broker = dir.path().join("broker");
    std::fs::create_dir_all(&broker).unwrap();
    let machine = broker.join("machine.json");
    let written = r#"{"schema":1,"id":"0123456789abcdef0123456789abcdef","name":"desk","created_at":"2026-01-01T00:00:00Z"}"#;
    std::fs::write(&machine, written).unwrap();
    let loaded = load_or_create_at(&machine).unwrap();
    assert_eq!(loaded.outcome, LoadOutcome::Existing);
    assert_eq!(loaded.identity.id, "0123456789abcdef0123456789abcdef");
    assert_eq!(loaded.identity.name, "desk");
    assert_eq!(std::fs::read_to_string(&machine).unwrap(), written);

    // The broker's stored credential: a certificate and its key.
    let ca = Ca::new();
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let cert = ca.client_cert(&key, "SHA256:broker", SCOPE_CREDENTIALS, 0x2a);
    let store = CredStore::new(broker.join("creds").join(SCOPE_CREDENTIALS));
    store
        .write(
            "prod",
            &cert,
            &pem_encode("PRIVATE KEY", &key.serialize_der()),
        )
        .unwrap();

    struct NeverMint(AtomicUsize);
    #[async_trait::async_trait]
    impl Minter for NeverMint {
        async fn mint(&self, _: &ServerTarget, _: &str) -> Result<Minted, MinterError> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Err(MinterError::new("no mint expected".into()))
        }
    }
    let never = Arc::new(NeverMint(AtomicUsize::new(0)));
    let target = ServerTarget {
        name: "prod".into(),
        url: "https://prod.example.test:18443".into(),
        token: String::new(),
        tls_fingerprint: "sha256:00".into(),
        ssh_host: "prod.example.test".into(),
        ssh_port: SSH_PORT,
        auth_mode: "mtls".into(),
        user: String::new(),
        identity_file: String::new(),
    };
    let source = shed_source::minter::new_credential_source_with_store(
        never.clone(),
        target,
        SCOPE_CREDENTIALS,
        Some(store),
    );
    source.token_for_request().await.unwrap();
    assert!(source.holds_certificate());
    let held = source
        .cert_resolver()
        .current()
        .expect("the stored certificate is armed");
    assert_eq!(common_name(held.cert[0].as_ref()), "SHA256:broker");
    assert_eq!(
        never.0.load(Ordering::SeqCst),
        0,
        "presented without a mint"
    );
}
