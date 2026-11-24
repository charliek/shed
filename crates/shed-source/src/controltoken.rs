//! The control-token provider — a faithful Rust port of
//! the retired Go daemon's `controltoken.go`. It resolves servers through an injected
//! [`ServerResolver`] (plan 039 §3.7): this crate has no YAML parser, so the embedder
//! passes the reader (the broker's is its saphyr-backed `load_discovered_servers`).
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

use crate::minter::{Minter, RelayedCredential, SCOPE_CONTROL};
use crate::target::ServerTarget;
use crate::time::rfc3339_utc;

/// The shed CLI config (`~/.shed/config.yaml`) the control-token provider resolves
/// servers from in single-server mode (mirror `config.go:DefaultDiscoverySource`).
/// The single canonical definition: the broker's `config` re-exports it, because its
/// discovery `apply_defaults` uses it too.
pub const DEFAULT_DISCOVERY_SOURCE: &str = "~/.shed/config.yaml";

/// Reads the shed CLI config at a (tilde-expanded) path and returns one
/// [`ServerTarget`] per registered server; `Err(message)` when the file cannot be read
/// or parsed. A missing file is not an error (an empty list).
///
/// Injected into [`ControlTokenProvider::new`] because the reader is not this crate's:
/// the broker passes its saphyr-backed `discovery::load_discovered_servers`. Whatever
/// is passed must keep that reader's rule that an entry without `ssh_port` reads as
/// port 0 ("no ssh endpoint"), never 22, or a control credential would be minted
/// against a port the config never named.
pub type ServerResolver = Arc<dyn Fn(&str) -> Result<Vec<ServerTarget>, String> + Send + Sync>;

// ---------------------------------------------------------------------------
// Control-token minter seam
// ---------------------------------------------------------------------------
//
// The `token.get` seam lives here, in the broker core, for any embedder to consume;
// [`ControlTokenProvider`] below is its production implementation.

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
    /// Reads `source_path` ([`ServerResolver`]).
    resolve_servers: ServerResolver,
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
    /// `resolve_servers` reads the shed CLI config at `source_path` (tilde-expanded
    /// here first); see [`ServerResolver`].
    pub fn new(
        minter: Arc<dyn Minter>,
        source_path: &str,
        resolve_servers: ServerResolver,
    ) -> Self {
        Self {
            minter,
            source_path: crate::paths::expand_tilde(source_path),
            resolve_servers,
            tokens: Mutex::new(HashMap::new()),
            next_flight: AtomicU64::new(0),
        }
    }

    /// Look the server up by name in the shed CLI config. Minting requires both an SSH
    /// endpoint (to bootstrap over) and a secure (https) server — an open http server
    /// needs no token and can't be minted for. Rejects **before any mint**, checking the
    /// ssh-endpoint gate BEFORE the secure gate (mirror `controltoken.go:resolve`).
    fn resolve(&self, name: &str) -> Result<ServerTarget, String> {
        let targets = (self.resolve_servers)(&self.source_path)
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
#[path = "controltoken_tests.rs"]
mod tests;
