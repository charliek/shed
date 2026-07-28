//! The control-token provider — a faithful Rust port of
//! `cmd/shed-host-agent/controltoken.go`. The `ServerTarget` / `load_discovered_servers`
//! slice its resolve path needs now lives in the always-on [`crate::discovery`] module
//! (hoisted out of here so the headless supervisor path can use it too).
//!
//! [`ControlTokenProvider`] mints and caches CONTROL-scoped tokens for named servers on
//! the desktop's behalf (the `token.get` UDS request). Minting is BROAD: it can mint for
//! any server in the shed CLI config the agent's SSH key is allowlisted on — not only the
//! discovery-scoped servers it brokers credentials for. It **always mints fresh** (never
//! serves a per-server source's completed cache) because a restarted server silently
//! invalidates control tokens with no signal to the agent.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use futures_util::future::{BoxFuture, Shared};
use futures_util::FutureExt as _;

use crate::discovery::{load_discovered_servers, ServerTarget};
use crate::minter::{Minter, RelayedCredential, SCOPE_CONTROL};
use crate::status::rfc3339_utc;

/// The shed CLI config (`~/.shed/config.yaml`) the control-token provider resolves
/// servers from in single-server mode (mirror `config.go:DefaultDiscoverySource`).
/// Re-exported from `config` (the single canonical definition the discovery
/// `apply_defaults` also uses) so `main.rs`'s `controltoken::DEFAULT_DISCOVERY_SOURCE`
/// reference stays stable.
pub use crate::config::DEFAULT_DISCOVERY_SOURCE;

// ---------------------------------------------------------------------------
// Control-token minter seam
// ---------------------------------------------------------------------------
//
// The `token.get` seam lives here (the broker core), not in the daemon's
// `desktop` module: the desktop UDS server (`shed-host-agent`) and any future
// embedder both consume it, and [`ControlTokenProvider`] below is its production
// implementation. The bin's `DesktopServer` re-imports these from `shed_broker`.

/// A minted control-scoped token: the token plus an optional RFC3339 expiry
/// (`None` = non-expiring / unknown, which omits `expires_at` in the reply).
pub struct MintedControlToken {
    pub token: String,
    pub expires_at: Option<String>,
}

impl std::fmt::Debug for MintedControlToken {
    /// Redacts the live bearer token — only `expires_at` is printed as-is.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MintedControlToken")
            .field("token", &"<redacted>")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

/// Mints CONTROL-scoped tokens on the app's behalf (answers `token.get`). The
/// production implementation is [`ControlTokenProvider`]; the injection seam lets
/// the desktop server (and tests) swap in a stand-in.
#[async_trait::async_trait]
pub trait ControlTokenMinter: Send + Sync {
    /// Mint a control token for `server`. `Err(msg)` fails the `token.get` closed —
    /// the reply carries the message and no token.
    async fn mint_control(&self, server: &str) -> Result<MintedControlToken, String>;
}

/// The message a desktop that speaks mtls gets when it asks for a control token from a
/// server that issues certificates instead. It names the ACTION (`upgrade the app`)
/// because the failure is a capability gap in the caller, not a misconfiguration:
/// `token.get` has no field that can carry a certificate. Byte-mirrors Go's
/// `controltoken.go:errDesktopTooOldForMTLS`.
fn err_desktop_too_old_for_mtls(server: &str) -> String {
    format!(
        "server \"{server}\" issues client certificates (auth.mode: mtls); \
         this shed-desktop is too old to use one — upgrade the app"
    )
}

/// A CONTROL-scoped credential minted for a CALLER-SUPPLIED CSR — the payload of the
/// desktop `credential.response` message.
///
/// It carries no private key, and that is the entire point of the message pair it backs:
/// the desktop app generates its own keypair, sends only the CSR across the UDS, and keeps
/// the private half in its own process (plan 001 D6's ownership table). The broker is a
/// RELAY here, not a credential holder.
#[derive(Clone, PartialEq, Eq)]
pub struct MintedControlCredential {
    /// `"token"` or `"mtls"` — the shape the server chose.
    pub auth_mode: String,
    /// The bearer token; set in token mode only.
    pub token: String,
    /// The PEM leaf issued for the submitted CSR; set in mtls mode only.
    pub client_cert: String,
    /// The issued certificate's serial in lower-case hex; mtls mode only.
    pub cert_serial: String,
    /// RFC3339 UTC, whole seconds. `None` when the server reported no expiry.
    pub expires_at: Option<String>,
}

impl std::fmt::Debug for MintedControlCredential {
    /// Redacts the live bearer token. The certificate is public material but is elided
    /// too — it is long, and its serial identifies it.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MintedControlCredential")
            .field("auth_mode", &self.auth_mode)
            .field("token", &"<redacted>")
            .field("client_cert_len", &self.client_cert.len())
            .field("cert_serial", &self.cert_serial)
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

/// Mints a CONTROL-scoped credential for a CSR the caller generated (answers the desktop
/// `credential.get`). The production implementation is [`ControlTokenProvider`].
#[async_trait::async_trait]
pub trait ControlCredentialMinter: Send + Sync {
    /// Run a control-scope bootstrap for `server`, passing `csr_base64` through VERBATIM
    /// (an empty string sends no `csr=` argument at all, which is the legacy token-only
    /// request), and return whichever credential the server issued.
    ///
    /// This path deliberately does NOT generate a keypair: a certificate issued for a key
    /// the broker holds would be useless to the app that has to present it.
    async fn mint_control_credential(
        &self,
        server: &str,
        csr_base64: &str,
    ) -> Result<MintedControlCredential, String>;
}

/// Answers `token.get` by minting CONTROL tokens over SSH (mirror
/// `controltoken.go:controlTokenProvider`).
pub struct ControlTokenProvider {
    minter: Arc<dyn Minter>,
    /// The shed CLI config to resolve servers from (tilde-expanded).
    source_path: String,
    /// Coalesces concurrent CSR-FREE mints for the same server (Go's
    /// `singleflight.Group`). Deliberately NOT applied to the certificate path: two
    /// `credential.get` requests carry two DIFFERENT CSRs, so sharing one answer between
    /// them would hand an app a certificate for a key it does not hold.
    tokens: Mutex<HashMap<String, (u64, TokenFlight)>>,
    /// Identifies each in-flight mint so a finishing caller removes ITS OWN entry and
    /// never evicts a newer flight that started in the gap.
    next_flight: AtomicU64,
}

/// One in-flight CSR-free control mint, shared by every caller that joins it.
///
/// A `Shared` future rather than a spawned task: the provider is not held as an `Arc`
/// (the daemon boxes it behind `dyn ControlTokenMinter`), so there is no `'static` handle
/// to spawn with — and a shared future needs none. The trade is that progress happens on
/// whichever caller is polling, so if every waiter goes away the mint is abandoned rather
/// than completing in the background. That is the right shape here: nothing caches the
/// result, so an abandoned mint costs an SSH round-trip and nothing else.
type TokenFlight = Shared<BoxFuture<'static, Result<RelayedCredential, String>>>;

impl ControlTokenProvider {
    pub fn new(minter: Arc<dyn Minter>, source_path: &str) -> Self {
        Self {
            minter,
            source_path: crate::config::expand_tilde(source_path),
            tokens: Mutex::new(HashMap::new()),
            next_flight: AtomicU64::new(0),
        }
    }

    /// Look the server up by name in the shed CLI config. Minting requires both an SSH
    /// endpoint (to bootstrap over) and a secure (https) server — an open http server
    /// needs no token and can't be minted for. Rejects **before any mint**, checking the
    /// ssh-endpoint gate BEFORE the secure gate (mirror `controltoken.go:resolve`).
    fn resolve(&self, name: &str) -> Result<ServerTarget, String> {
        let targets = load_discovered_servers(&self.source_path)
            .map_err(|e| format!("reading server config: {e}"))?;
        for t in targets {
            if t.name == name {
                if t.ssh_host.is_empty() || t.ssh_port == 0 {
                    return Err(format!(
                        "server \"{name}\" has no ssh endpoint to mint a control credential over"
                    ));
                }
                if !t.is_secure() {
                    return Err(format!(
                        "server \"{name}\" is not a secure (https) server; control-credential minting is unavailable"
                    ));
                }
                return Ok(t);
            }
        }
        Err(format!("unknown server \"{name}\""))
    }

    /// Run ONE CSR-free control bootstrap for `name`, coalescing callers that overlap it
    /// (Go's `p.tokens.Do`). The entry is dropped as soon as this flight completes, so the
    /// NEXT ask mints fresh — nothing here is a cache.
    async fn mint_token_singleflight(
        &self,
        name: &str,
        target: &ServerTarget,
    ) -> Result<RelayedCredential, String> {
        let (id, flight) = {
            let mut inflight = self.tokens.lock().unwrap();
            match inflight.get(name) {
                Some((id, f)) => (*id, f.clone()),
                None => {
                    let id = self.next_flight.fetch_add(1, Ordering::Relaxed);
                    let minter = self.minter.clone();
                    let target = target.clone();
                    let f: TokenFlight = async move {
                        minter
                            // The CSR is deliberately EMPTY. `token.get` is frozen at
                            // bearer tokens — its reply has no field that could carry a
                            // certificate — so asking for one would be pointless work
                            // against a token server and, against an mtls server, would
                            // mint a certificate for a key THIS process holds and the app
                            // could never present. The explicit upgrade error is the
                            // better answer, and the caller produces it.
                            .mint_relayed(&target, SCOPE_CONTROL, "")
                            .await
                            .map_err(|e| e.message().to_string())
                    }
                    .boxed()
                    .shared();
                    inflight.insert(name.to_string(), (id, f.clone()));
                    (id, f)
                }
            }
        };
        let out = flight.await;
        {
            let mut inflight = self.tokens.lock().unwrap();
            if inflight.get(name).is_some_and(|(got, _)| *got == id) {
                inflight.remove(name);
            }
        }
        out
    }
}

#[async_trait::async_trait]
impl ControlTokenMinter for ControlTokenProvider {
    /// Mint a control token for `server`, always FRESH (the per-server source's completed
    /// cache is never served — a restarted server silently invalidates control tokens).
    /// `Err(message)` fails the `token.get` closed with the message (mirror
    /// `controltoken.go:Token` + `handleTokenGet`'s `resp.Error = err.Error()`).
    async fn mint_control(&self, server: &str) -> Result<MintedControlToken, String> {
        let target = self.resolve(server)?;
        // A server recorded as issuing CERTIFICATES has no control TOKEN to hand out
        // (plan 001 D2: in mtls mode nothing mints bearer tokens and the middleware
        // ignores `Authorization`), and this message has no field that could carry one.
        // Rejecting on the RECORDED mode, before any SSH round trip, is the one place
        // stale knowledge is worth acting on: the alternative is a mint that succeeds,
        // returns a certificate this reply cannot express, and surfaces as an unexplained
        // 401 in the app much later. A desktop new enough to use a certificate asks
        // through `credential.get` ([`ControlCredentialMinter`]) and never reaches here.
        if target.is_mtls() {
            return Err(err_desktop_too_old_for_mtls(server));
        }
        let bundle = self.mint_token_singleflight(server, &target).await?;
        // ...and again on what the server ACTUALLY answered with, which is the real check:
        // the recorded mode above is only a shortcut that saves the round trip.
        if bundle.is_mtls {
            return Err(err_desktop_too_old_for_mtls(server));
        }
        Ok(MintedControlToken {
            token: bundle.token,
            // Go: exp.UTC().Format(time.RFC3339) only when !exp.IsZero(); a None expiry
            // omits `token.response.expires_at`.
            expires_at: bundle.expiry.map(rfc3339_utc),
        })
    }
}

#[async_trait::async_trait]
impl ControlCredentialMinter for ControlTokenProvider {
    /// Always FRESH and never cached — for the same reason [`Self::mint_control`] is (a
    /// restarted server silently invalidates control credentials), plus a stronger one:
    /// the result belongs to the CALLER's keypair, so caching it would mean handing one
    /// app's credential to whoever asked next.
    ///
    /// Note what is deliberately absent: no `is_mtls` pre-check. This path works against a
    /// server in EITHER mode, which is exactly what makes a mode flip invisible to the
    /// app — it always sends a CSR, and whichever credential comes back is the one that
    /// mode issues (plan 001 D4's compat matrix, client side).
    async fn mint_control_credential(
        &self,
        server: &str,
        csr_base64: &str,
    ) -> Result<MintedControlCredential, String> {
        let target = self.resolve(server)?;
        let relayed = self
            .minter
            .mint_relayed(&target, SCOPE_CONTROL, csr_base64)
            .await
            .map_err(|e| e.message().to_string())?;
        Ok(MintedControlCredential {
            auth_mode: if relayed.is_mtls {
                AUTH_MODE_MTLS.to_string()
            } else {
                AUTH_MODE_TOKEN.to_string()
            },
            token: if relayed.is_mtls {
                String::new()
            } else {
                relayed.token
            },
            client_cert: relayed.client_cert,
            cert_serial: relayed.cert_serial,
            expires_at: relayed.expiry.map(rfc3339_utc),
        })
    }
}

/// The `auth_mode` literals carried on the wire (mirror `sdk.AuthModeToken`/`MTLS`).
const AUTH_MODE_TOKEN: &str = "token";
const AUTH_MODE_MTLS: &str = "mtls";

#[cfg(test)]
mod tests {
    use super::*;
    use crate::minter::{Minted, MinterError};
    use std::sync::atomic::{AtomicUsize, Ordering};

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
        let p = ControlTokenProvider::new(fm.clone(), &cfg);

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
        let p = ControlTokenProvider::new(fm.clone(), &cfg);

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
        let p = ControlTokenProvider::new(FakeMinter::new(&["x"]), &cfg);
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
        let p = ControlTokenProvider::new(fm.clone(), &cfg);

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
        let p = ControlTokenProvider::new(rm.clone(), &cfg);

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
        let p = ControlTokenProvider::new(rm, &cfg);

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
        let p = ControlTokenProvider::new(rm.clone(), &cfg);

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
        let p = ControlTokenProvider::new(fm.clone(), &cfg);
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
        let p = ControlTokenProvider::new(rm, &cfg);
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
        let p = Arc::new(ControlTokenProvider::new(gm.clone(), &cfg));

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
}
