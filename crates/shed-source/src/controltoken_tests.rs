//! The control-token provider ([`crate::controltoken`]), resolving servers from a real
//! shed CLI config through [`crate::discovery::resolver`], the resolver production
//! passes (plan 039 PR B; these moved here from the retired broker, which resolved
//! through its own reader).

use crate::controltoken::*;
use crate::minter::{Minted, Minter, MinterError, RelayedCredential};
use crate::target::ServerTarget;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

/// The provider as production builds it: resolving through this crate's discovery.
fn provider(minter: Arc<dyn Minter>, source_path: &str) -> ControlTokenProvider {
    ControlTokenProvider::new(minter, source_path, crate::discovery::resolver())
}

fn write_config(content: &str) -> (tempfile::TempDir, String) {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("config.yaml");
    std::fs::write(&path, content).unwrap();
    (dir, path.to_string_lossy().into_owned())
}

const PROD_SECURE: &str = "\
servers:
  prod:
    api_url: https://prod.example:8443
    host: prod.example
    ssh_port: 2222
";

/// A `Minter` returning canned tokens in sequence (repeating the last) + call count.
struct FakeMinter {
    results: Mutex<Vec<String>>,
    calls: AtomicUsize,
    /// Every (scope, csr) the provider submitted — how the tests see that `token.get`
    /// sends NO `csr=` argument.
    csrs: Mutex<Vec<(String, String)>>,
}
impl FakeMinter {
    fn new(tokens: &[&str]) -> Arc<Self> {
        Arc::new(Self {
            results: Mutex::new(tokens.iter().map(|s| s.to_string()).collect()),
            calls: AtomicUsize::new(0),
            csrs: Mutex::new(Vec::new()),
        })
    }
}
#[async_trait::async_trait]
impl Minter for FakeMinter {
    async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
        unreachable!("the control paths never take the key-generating mint")
    }
    async fn mint_relayed(
        &self,
        _t: &ServerTarget,
        scope: &str,
        csr_base64: &str,
    ) -> Result<RelayedCredential, MinterError> {
        let i = self.calls.fetch_add(1, Ordering::SeqCst);
        self.csrs
            .lock()
            .unwrap()
            .push((scope.to_string(), csr_base64.to_string()));
        let toks = self.results.lock().unwrap();
        let idx = i.min(toks.len() - 1);
        Ok(RelayedCredential {
            token: toks[idx].clone(),
            ..RelayedCredential::default()
        })
    }
}

#[tokio::test]
async fn always_mints_fresh() {
    let (_d, cfg) = write_config(PROD_SECURE);
    let fm = FakeMinter::new(&["ctl-1", "ctl-2"]);
    let p = provider(fm.clone(), &cfg);

    assert_eq!(p.mint_control("prod").await.unwrap().token, "ctl-1");
    // A second sequential call re-mints — no completed-cache short-circuit.
    assert_eq!(p.mint_control("prod").await.unwrap().token, "ctl-2");
    assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn errors_before_any_mint() {
    let (_d, cfg) = write_config(
        "\
servers:
  open-no-ssh:
    host: open1.example
    http_port: 8080
  open-http:
    host: open2.example
    http_port: 8080
    ssh_port: 2222
",
    );
    let fm = FakeMinter::new(&["x"]);
    let p = provider(fm.clone(), &cfg);

    // unknown / no-ssh-endpoint / open-http-with-ssh all error before minting.
    assert!(p.mint_control("missing").await.is_err());
    assert!(p.mint_control("open-no-ssh").await.is_err());
    assert!(p.mint_control("open-http").await.is_err());
    assert_eq!(fm.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn error_strings_match_go() {
    let (_d, cfg) = write_config(
        "\
servers:
  open-no-ssh:
    host: open1.example
    http_port: 8080
  open-http:
    host: open2.example
    http_port: 8080
    ssh_port: 2222
",
    );
    let p = provider(FakeMinter::new(&["x"]), &cfg);
    assert_eq!(
        p.mint_control("missing").await.unwrap_err(),
        "unknown server \"missing\""
    );
    assert_eq!(
        p.mint_control("open-no-ssh").await.unwrap_err(),
        "server \"open-no-ssh\" has no ssh endpoint to mint a control credential over"
    );
    assert_eq!(
            p.mint_control("open-http").await.unwrap_err(),
            "server \"open-http\" is not a secure (https) server; control-credential minting is unavailable"
        );
}

const MTLS_SERVERS: &str = "\
servers:
  certs:
    api_url: https://certs.example:8443
    host: certs.example
    ssh_port: 2222
    auth_mode: mtls
  certs-shouty:
    api_url: https://certs2.example:8443
    host: certs2.example
    ssh_port: 2222
    auth_mode: MTLS
  certs-no-ssh:
    api_url: https://certs3.example:8443
    host: certs3.example
    auth_mode: mtls
";

/// `token.get` against a server that issues certificates fails with the explicit
/// upgrade instruction, before any SSH round trip. Rejected AFTER the ssh-endpoint and
/// https gates, so a misconfigured entry still gets the more specific diagnosis first.
#[tokio::test]
async fn token_get_on_an_mtls_server_says_upgrade_the_app() {
    let (_d, cfg) = write_config(MTLS_SERVERS);
    let fm = FakeMinter::new(&["x"]);
    let p = provider(fm.clone(), &cfg);

    assert_eq!(
        p.mint_control("certs").await.unwrap_err(),
        "server \"certs\" issues client certificates (auth.mode: mtls); \
             this shed-desktop is too old to use one — upgrade the app"
    );
    // Case-insensitive, matching Go's EqualFold.
    assert!(p
        .mint_control("certs-shouty")
        .await
        .unwrap_err()
        .contains("upgrade the app"));
    // The ssh-endpoint gate still wins — it names the more specific problem.
    assert!(p
        .mint_control("certs-no-ssh")
        .await
        .unwrap_err()
        .contains("no ssh endpoint"));

    assert_eq!(fm.calls.load(Ordering::SeqCst), 0, "no SSH round trip");
}

/// A `Minter` that records the relayed CSR + scope and answers with a scripted bundle.
struct RelayMinter {
    reply: RelayedCredential,
    seen: Mutex<Vec<(String, String)>>, // (scope, csr)
    keygen_calls: AtomicUsize,
}
impl RelayMinter {
    fn new(reply: RelayedCredential) -> Arc<Self> {
        Arc::new(Self {
            reply,
            seen: Mutex::new(Vec::new()),
            keygen_calls: AtomicUsize::new(0),
        })
    }
}
#[async_trait::async_trait]
impl Minter for RelayMinter {
    async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
        // The key-generating path must never be taken by the relay.
        self.keygen_calls.fetch_add(1, Ordering::SeqCst);
        Ok(Minted::token("should-not-be-used", None))
    }
    async fn mint_relayed(
        &self,
        _t: &ServerTarget,
        scope: &str,
        csr_base64: &str,
    ) -> Result<RelayedCredential, MinterError> {
        self.seen
            .lock()
            .unwrap()
            .push((scope.to_string(), csr_base64.to_string()));
        Ok(self.reply.clone())
    }
}

/// The relay passes the caller's CSR through verbatim on the CONTROL scope, never
/// generates a keypair, and maps an mtls bundle onto the certificate fields.
#[tokio::test]
async fn credential_get_relays_the_callers_csr() {
    let (_d, cfg) = write_config(PROD_SECURE);
    let rm = RelayMinter::new(RelayedCredential {
        is_mtls: true,
        token: String::new(),
        client_cert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n".into(),
        cert_serial: "0a1b".into(),
        expiry: Some(1_893_456_000),
    });
    let p = provider(rm.clone(), &cfg);

    let got = p
        .mint_control_credential("prod", "QUJDREVG==")
        .await
        .unwrap();
    assert_eq!(got.auth_mode, "mtls");
    assert_eq!(got.cert_serial, "0a1b");
    assert!(got.client_cert.contains("BEGIN CERTIFICATE"));
    assert_eq!(got.token, "", "an mtls credential carries no bearer");
    assert_eq!(got.expires_at.as_deref(), Some("2030-01-01T00:00:00Z"));

    assert_eq!(
        *rm.seen.lock().unwrap(),
        vec![("control".to_string(), "QUJDREVG==".to_string())],
        "the CSR must cross verbatim on the control scope"
    );
    assert_eq!(
        rm.keygen_calls.load(Ordering::SeqCst),
        0,
        "the relay must not mint with a key of its own"
    );

    // Debug never renders the token.
    assert!(format!("{got:?}").contains("<redacted>"));
}

/// The same call against a TOKEN-mode server degrades to today's behavior: the server
/// ignores the CSR and the bearer comes back, so a mode flip needs no app change.
#[tokio::test]
async fn credential_get_degrades_to_a_token_bundle() {
    let (_d, cfg) = write_config(PROD_SECURE);
    let rm = RelayMinter::new(RelayedCredential {
        is_mtls: false,
        token: "ctl-tok".into(),
        client_cert: String::new(),
        cert_serial: String::new(),
        expiry: None,
    });
    let p = provider(rm, &cfg);

    let got = p.mint_control_credential("prod", "QUJD").await.unwrap();
    assert_eq!(got.auth_mode, "token");
    assert_eq!(got.token, "ctl-tok");
    assert_eq!(got.client_cert, "");
    assert_eq!(got.expires_at, None);
}

/// The relay is NOT mode-gated — it is the path that works against either mode — but
/// the resolve-time gates still apply, before any SSH round trip.
#[tokio::test]
async fn credential_get_is_not_mode_gated_but_still_resolves() {
    let (_d, cfg) = write_config(MTLS_SERVERS);
    let rm = RelayMinter::new(RelayedCredential {
        is_mtls: true,
        client_cert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n".into(),
        ..RelayedCredential::default()
    });
    let p = provider(rm.clone(), &cfg);

    // An mtls-recorded server IS served here, unlike `token.get`.
    let got = p.mint_control_credential("certs", "QUJD").await.unwrap();
    assert_eq!(got.auth_mode, "mtls");

    assert!(p
        .mint_control_credential("certs-no-ssh", "QUJD")
        .await
        .unwrap_err()
        .contains("no ssh endpoint"));
    assert!(p
        .mint_control_credential("missing", "QUJD")
        .await
        .unwrap_err()
        .contains("unknown server"));
}

/// `token.get` sends NO `csr=` argument — the wire divergence the Go-vs-Rust
/// differential harness caught. The reply cannot carry a certificate, so requesting
/// one is pointless against a token server and actively wrong against an mtls one
/// (it would mint a certificate for a key THIS process holds).
#[tokio::test]
async fn token_get_sends_no_csr() {
    let (_d, cfg) = write_config(PROD_SECURE);
    let fm = FakeMinter::new(&["ctl-1"]);
    let p = provider(fm.clone(), &cfg);
    p.mint_control("prod").await.unwrap();
    assert_eq!(
        *fm.csrs.lock().unwrap(),
        vec![("control".to_string(), String::new())],
        "token.get must run a CSR-free control bootstrap"
    );
}

/// A server that answers with a certificate despite a token-recorded entry (the STALE
/// entry case) still produces the explicit upgrade error, not an empty token.
#[tokio::test]
async fn token_get_rejects_a_certificate_the_server_returned() {
    let (_d, cfg) = write_config(PROD_SECURE); // recorded WITHOUT auth_mode
    let rm = RelayMinter::new(RelayedCredential {
        is_mtls: true,
        client_cert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n".into(),
        ..RelayedCredential::default()
    });
    let p = provider(rm, &cfg);
    let err = p.mint_control("prod").await.unwrap_err();
    assert!(err.contains("upgrade the app"), "{err}");
}

/// Concurrent `token.get` asks for one server coalesce onto a single SSH round trip
/// (Go's `singleflight.Group`), while `credential.get` deliberately does NOT — two
/// requests carry two different CSRs, so one answer cannot serve both.
#[tokio::test]
async fn token_get_coalesces_but_credential_get_does_not() {
    struct GatedRelay {
        calls: AtomicUsize,
        release: tokio::sync::Semaphore,
    }
    #[async_trait::async_trait]
    impl Minter for GatedRelay {
        async fn mint(&self, _t: &ServerTarget, _s: &str) -> Result<Minted, MinterError> {
            unreachable!()
        }
        async fn mint_relayed(
            &self,
            _t: &ServerTarget,
            _scope: &str,
            _csr: &str,
        ) -> Result<RelayedCredential, MinterError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let _p = self.release.acquire().await.unwrap();
            Ok(RelayedCredential {
                token: "ctl".into(),
                ..RelayedCredential::default()
            })
        }
    }
    let (_d, cfg) = write_config(PROD_SECURE);
    let gm = Arc::new(GatedRelay {
        calls: AtomicUsize::new(0),
        release: tokio::sync::Semaphore::new(0),
    });
    let p = Arc::new(provider(gm.clone(), &cfg));

    let mut handles = Vec::new();
    for _ in 0..6 {
        let p = p.clone();
        handles.push(tokio::spawn(async move { p.mint_control("prod").await }));
    }
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    gm.release.add_permits(6);
    for h in handles {
        assert_eq!(h.await.unwrap().unwrap().token, "ctl");
    }
    assert_eq!(gm.calls.load(Ordering::SeqCst), 1, "one SSH round trip");

    // The flight is dropped on completion: the NEXT ask mints fresh.
    gm.release.add_permits(1);
    p.mint_control("prod").await.unwrap();
    assert_eq!(gm.calls.load(Ordering::SeqCst), 2, "never a cache");

    // credential.get does not coalesce.
    let mut handles = Vec::new();
    for i in 0..3 {
        let p = p.clone();
        handles.push(tokio::spawn(async move {
            p.mint_control_credential("prod", &format!("CSR{i}")).await
        }));
    }
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    gm.release.add_permits(3);
    for h in handles {
        h.await.unwrap().unwrap();
    }
    assert_eq!(
        gm.calls.load(Ordering::SeqCst),
        5,
        "each credential.get carries its own CSR and must get its own mint"
    );
}

// The `load_discovered_servers` unit + golden tests moved to `discovery.rs`
// alongside the hoisted function (the ssh_port=0-vs-22 + empty-host + sort +
// malformed divergences it pins).

#[test]
fn debug_redacts_token() {
    let secret = "super-secret-control-token-value";
    let minted = MintedControlToken {
        token: secret.to_string(),
        expires_at: Some("2026-07-15T00:00:00Z".to_string()),
    };
    let debug_str = format!("{minted:?}");
    assert!(
        !debug_str.contains(secret),
        "Debug output leaked the token: {debug_str}"
    );
    assert!(debug_str.contains("<redacted>"));
    assert!(debug_str.contains("2026-07-15T00:00:00Z"));
}
