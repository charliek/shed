//! The SSH credential minter — a faithful Rust port of `cmd/shed-host-agent/credmint.go`.
//!
//! [`CredentialMinter`] bootstraps a scoped CREDENTIAL over a server's SSH `_bootstrap`
//! channel (via [`crate::bootstrap`]), after a fast-fail `known_hosts` pin pre-check.
//! [`CredentialSource`] is the `sdk.TokenProvider` analog: it caches the minted
//! credential and re-mints on demand (near expiry, or after a 401 via
//! [`CredentialSource::invalidate`]) and proactively ([`CredentialSource::refresh`]). A
//! host-key pin mismatch is **TERMINAL** — a possible MITM — so the source fails closed
//! and never serves a credential for that server again.
//!
//! # Two credential shapes, one source (plan 001 D2/D5)
//!
//! Every mint is CSR-first: a fresh P-256 keypair is generated per attempt and its CSR
//! rides the `_bootstrap` request line, so the SERVER decides which credential comes
//! back. A `token`-mode server ignores the `csr=` argument and returns a bearer token
//! exactly as it always did; an `mtls`-mode server signs it and returns a certificate.
//! The client never has to know the mode in advance, which is what makes a server-side
//! mode flip a non-event here — the very next mint simply returns the other shape.
//!
//! In mtls state there is NO bearer to send: [`CredentialSource::token`] resolves to the
//! empty string (the bus/egress header guards then send no `Authorization`), and the
//! credential is presented through the [`shed_core::tls::ClientCertResolver`] this source
//! owns and writes on every rotation. The transports install that resolver ONCE at build
//! time, so a rotation — and even a mode flip — never rebuilds a `reqwest::Client`.
//!
//! **Concurrency note:** Go uses a `sync.Mutex` + a goroutine + a `chan struct{}`
//! done-signal for single-flight. This port preserves the *behavioral* invariant —
//! one mint per burst of overlapping callers, all receiving that mint's own result;
//! the mint runs OFF the state lock; a terminal latch is never retried — using an
//! async `Minter::mint`, a `std::sync::Mutex` for the small state, a spawned mint task,
//! and a `tokio::sync::watch` join. The mutex guard is always dropped before awaiting
//! the join (Go releases `s.mu` before `<-call.done`).

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use rustls::sign::CertifiedKey;
use shed_core::certuser::{cert_user_pem, is_user_name, CertUser};
use shed_core::csr::{pem_encode, ClientKeyPair, PEM_LABEL_PRIVATE_KEY};
use shed_core::tls::{certified_key_from_pem, ClientCertResolver};
use shed_core::token::{display_server_name, held_user_refusal, next_refusal_backoff};
use ssh_key::known_hosts::{HostPatterns, KnownHosts};
use tokio::sync::watch;

use crate::bootstrap::{BootstrapRunner, Params, SystemSshRunner};
use crate::config::expand_tilde;
use crate::credstore::CredStore;
use crate::discovery::ServerTarget;
use crate::status::now_unix;

/// Token scopes the host-agent mints over SSH: `credentials` for its own bus
/// brokering, `control` for a `token.get` on the desktop's behalf (`credmint.go:48`).
/// `credentials` is the bus token provider's scope (wired by the supervisor's
/// credentials-scope `CredentialSource`); `control` is the egress side task + `token.get`.
pub const SCOPE_CREDENTIALS: &str = "credentials";
pub const SCOPE_CONTROL: &str = "control";

/// How long before expiry an on-demand token re-mints (`tokenRefreshWindow` = 2h).
const TOKEN_REFRESH_WINDOW: Duration = Duration::from_secs(2 * 3600);
/// Proactive-refresh delay before any token has been minted (`defaultRefreshDelay` = 1h).
const DEFAULT_REFRESH_DELAY: Duration = Duration::from_secs(3600);
const MIN_REFRESH_DELAY: Duration = Duration::from_secs(60);
const MAX_REFRESH_DELAY: Duration = Duration::from_secs(12 * 3600);
/// De-synchronizes a fleet re-minting together: the delay is spread ±this of its base.
const JITTER_FRACTION: f64 = 0.25;

/// The client certificate an mtls-mode server issued for the CSR this process submitted,
/// paired with the private key it was issued for.
///
/// The key never leaves the process that generated it: only the CSR (the public half)
/// crosses the SSH channel, and only this struct and the store on disk ever hold the
/// private half (plan 001 D6).
#[derive(Clone, PartialEq, Eq)]
pub struct MintedCertificate {
    /// PEM leaf, exactly as the bundle's `client_cert` delivered it.
    pub cert_pem: String,
    /// PKCS#8 DER of the key the CSR was built from.
    pub key_pkcs8_der: Vec<u8>,
    /// Lower-case hex serial. Opaque to the agent — logs and rotation proofs.
    pub serial: String,
}

/// Redacted Debug: a minted certificate carries its private key, which must never render
/// into a log line, an error payload, or a `{:?}` of an enclosing struct.
impl std::fmt::Debug for MintedCertificate {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MintedCertificate")
            .field("serial", &self.serial)
            .field("cert_pem_len", &self.cert_pem.len())
            .field("key", &"<redacted>")
            .finish()
    }
}

/// What a mint returned — the SERVER's choice of credential shape, not the client's.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MintedCredential {
    Token(String),
    Certificate(MintedCertificate),
}

/// A minted credential plus its expiry as unix seconds (`None` = non-expiring / the
/// server returned none — Go's zero `time.Time`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Minted {
    pub credential: MintedCredential,
    pub expiry: Option<i64>,
}

impl Minted {
    /// A bearer-token mint (the shape a `token`-mode server returns).
    pub fn token(token: impl Into<String>, expiry: Option<i64>) -> Self {
        Self {
            credential: MintedCredential::Token(token.into()),
            expiry,
        }
    }

    /// The bearer token this mint carries, or `None` when it carried a certificate.
    pub fn bearer(&self) -> Option<&str> {
        match &self.credential {
            MintedCredential::Token(t) => Some(t),
            MintedCredential::Certificate(_) => None,
        }
    }
}

/// A mint failure. `message` is the human string a caller surfaces (e.g. into
/// `token.response.error`); `terminal` marks the host-key-mismatch that must be latched
/// and never retried (the Rust analog of `errors.Is(err, ErrHostKeyMismatch)`).
#[derive(Debug, Clone)]
pub struct MinterError {
    message: String,
    terminal: bool,
}

impl MinterError {
    pub(crate) fn new(message: String) -> Self {
        Self {
            message,
            terminal: false,
        }
    }
    fn terminal(message: String) -> Self {
        Self {
            message,
            terminal: true,
        }
    }
    pub fn is_host_key_mismatch(&self) -> bool {
        self.terminal
    }
    pub fn message(&self) -> &str {
        &self.message
    }
}

impl std::fmt::Display for MinterError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

/// A credential minted for a CSR SOMEONE ELSE generated — the raw bundle fields, with no
/// private key, because the key belongs to the caller that sent the CSR.
///
/// It is deliberately a different type from [`Minted`]: [`Minted`] pairs a certificate
/// with the key it was issued for and is safe to arm on a transport, whereas this one
/// cannot be presented by anybody but the original requester. Keeping them apart makes it
/// impossible to accidentally adopt a relayed credential as the agent's own.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RelayedCredential {
    /// `true` when the server issued a certificate rather than a bearer token.
    pub is_mtls: bool,
    pub token: String,
    pub client_cert: String,
    pub cert_serial: String,
    pub expiry: Option<i64>,
}

/// The subset of [`CredentialMinter`] that [`CredentialSource`] needs (mirror Go's
/// `minter` interface); a trait so tests can inject a fake without a live SSH server.
#[async_trait::async_trait]
pub trait Minter: Send + Sync {
    async fn mint(&self, target: &ServerTarget, scope: &str) -> Result<Minted, MinterError>;

    /// Bootstrap with a CSR the CALLER generated, returning the credential verbatim.
    ///
    /// The default implementation refuses: relaying is a capability of the real SSH minter,
    /// and a test fake that has not opted in should fail loudly rather than silently answer
    /// a request whose whole point is the caller's own key.
    async fn mint_relayed(
        &self,
        target: &ServerTarget,
        scope: &str,
        csr_base64: &str,
    ) -> Result<RelayedCredential, MinterError> {
        let _ = (target, scope, csr_base64);
        Err(MinterError::new(
            "this minter cannot relay a client-supplied CSR".into(),
        ))
    }
}

/// Bootstraps the host-agent's own scoped token over a server's SSH `_bootstrap`
/// channel via the injectable [`BootstrapRunner`] (mirror `credmint.go:CredentialMinter`).
pub struct CredentialMinter {
    known_hosts_path: String,
    runner: Arc<dyn BootstrapRunner>,
}

impl CredentialMinter {
    /// Build a minter from the known_hosts file that pins server host keys
    /// (tilde-expanded). The SSH identity is resolved by the system ssh client, not
    /// read from a fixed key file.
    pub fn new(known_hosts_path: &str) -> Self {
        Self {
            known_hosts_path: expand_tilde(known_hosts_path),
            runner: Arc::new(SystemSshRunner::new()),
        }
    }

    /// A minter with an injected bootstrap runner (mirror Go's `bootstrapRun` field) —
    /// tests drive minting without spawning ssh.
    #[cfg(test)]
    fn with_runner(known_hosts_path: &str, runner: Arc<dyn BootstrapRunner>) -> Self {
        Self {
            known_hosts_path: expand_tilde(known_hosts_path),
            runner,
        }
    }
}

#[async_trait::async_trait]
impl Minter for CredentialMinter {
    /// Bootstrap a fresh credential of `scope` for `target`. ssh verifies the server's
    /// host key against the pin already in known_hosts, so this never trusts an unpinned
    /// server. Mirrors `credmint.go:Mint` (whose Go sibling is `sdk/bootstrap.RunCredential`).
    ///
    /// The CSR is sent UNCONDITIONALLY, to every server, because the agent cannot know
    /// the server's mode in advance and the mode can change under it. That is safe in all
    /// three directions: a pre-mtls server only ever read request position 1 and ignores
    /// the extra argument; a `token`-mode server accepts and then deliberately ignores
    /// `csr=` for exactly that legacy parity; an `mtls`-mode server signs it. Generating
    /// the keypair costs a P-256 keygen (microseconds) and buys a single code path that
    /// never has to predict the answer.
    async fn mint(&self, target: &ServerTarget, scope: &str) -> Result<Minted, MinterError> {
        // Pre-check the pin — NOT a safety latch (a missing pin is non-terminal
        // downstream), but it buys an actionable "run shed server add" error instead of
        // ssh's opaque "Host key verification failed", and skips a doomed ssh spawn.
        known_hosts_pinned(&self.known_hosts_path, &target.ssh_host, target.ssh_port)
            .map_err(MinterError::new)?;

        let keypair = ClientKeyPair::generate().map_err(|e| {
            MinterError::new(format!(
                "generating a client key for \"{}\": {e}",
                target.name
            ))
        })?;
        let params = Params {
            host: target.ssh_host.clone(),
            port: target.ssh_port,
            known_hosts_path: self.known_hosts_path.clone(),
            scope: scope.to_string(),
            client_kind: "host-agent".to_string(),
            csr: keypair.csr_base64(),
            // The entry's own key, when it names one (plan 037 A8): ssh offers it and
            // no key that is only in the agent; the runner checks the file first.
            identity_file: target.identity_file.clone(),
        };
        match self.runner.run(&params).await {
            Ok(bundle) if bundle.is_mtls() => Ok(Minted {
                credential: MintedCredential::Certificate(MintedCertificate {
                    cert_pem: bundle.client_cert,
                    // The certificate is paired with THIS attempt's key. A bundle whose
                    // certificate does not match it is refused when the source adopts it
                    // (`certified_key_from_pem` verifies the pairing) rather than being
                    // armed and failing every later handshake with no explanation.
                    key_pkcs8_der: keypair.key_pkcs8_der().to_vec(),
                    serial: bundle.cert_serial,
                }),
                expiry: bundle.expires_at,
            }),
            Ok(bundle) => Ok(Minted {
                credential: MintedCredential::Token(bundle.token),
                expiry: bundle.expires_at,
            }),
            Err(e) => {
                let terminal = e.is_host_key_mismatch();
                // Go: fmt.Errorf("bootstrapping %s token for %q: %w", scope, name, err).
                // Use an explicit quoted-name format (NOT {:?}) to byte-match Go's %q.
                let msg = format!("bootstrapping {scope} token for \"{}\": {e}", target.name);
                Err(if terminal {
                    MinterError::terminal(msg)
                } else {
                    MinterError::new(msg)
                })
            }
        }
    }

    /// Relay a CSR the caller generated: run the same bootstrap exchange, but with the
    /// SUPPLIED `csr_base64` and no keypair of our own, and hand back the bundle fields
    /// unchanged.
    ///
    /// The CSR is passed through VERBATIM rather than re-encoded or re-validated for
    /// content: this process cannot check a public key it did not generate against a
    /// private key it does not have, and `bootstrap::validate` still enforces the one
    /// property that matters locally — that the value is a single standard-base64argv
    /// token that cannot inject an ssh option or split into a second request argument.
    /// The SERVER does the real CSR validation, which is where it belongs.
    async fn mint_relayed(
        &self,
        target: &ServerTarget,
        scope: &str,
        csr_base64: &str,
    ) -> Result<RelayedCredential, MinterError> {
        known_hosts_pinned(&self.known_hosts_path, &target.ssh_host, target.ssh_port)
            .map_err(MinterError::new)?;

        let params = Params {
            host: target.ssh_host.clone(),
            port: target.ssh_port,
            known_hosts_path: self.known_hosts_path.clone(),
            scope: scope.to_string(),
            client_kind: "host-agent".to_string(),
            csr: csr_base64.to_string(),
            identity_file: target.identity_file.clone(),
        };
        match self.runner.run(&params).await {
            Ok(bundle) => Ok(RelayedCredential {
                is_mtls: bundle.is_mtls(),
                token: bundle.token,
                client_cert: bundle.client_cert,
                cert_serial: bundle.cert_serial,
                expiry: bundle.expires_at,
            }),
            Err(e) => {
                let terminal = e.is_host_key_mismatch();
                let msg = format!("bootstrapping {scope} token for \"{}\": {e}", target.name);
                Err(if terminal {
                    MinterError::terminal(msg)
                } else {
                    MinterError::new(msg)
                })
            }
        }
    }
}

/// Report whether the known_hosts file has a usable (non-marker) host-key entry pinning
/// `host:port` — the same trust anchor `shed server add` wrote. A presence predicate:
/// ssh re-verifies the pin authoritatively during the exchange, so only existence
/// matters here. Returns `Ok(())` when one is present; an `Err(message)` when the file
/// is unreadable, unparseable, or has no entry. Mirrors `credmint.go:knownHostsPinned`
/// (via the vetted `ssh-key` known_hosts parser rather than a hand-rolled one).
fn known_hosts_pinned(known_hosts_path: &str, host: &str, port: u16) -> Result<(), String> {
    let data = std::fs::read_to_string(known_hosts_path)
        .map_err(|e| format!("reading known_hosts {known_hosts_path}: {e}"))?;
    // The stored form: "[host]:port" for a non-22 port, bare "host" for port 22 —
    // matching how OpenSSH (and shed) records it.
    let want = normalize(host, port);
    for entry in KnownHosts::new(&data) {
        let entry = entry.map_err(|e| format!("parsing known_hosts {known_hosts_path}: {e}"))?;
        // Skip marked lines: a @revoked key must never be a pin, a @cert-authority line
        // is a CA not a host-key pin (Go skips any non-empty marker).
        if entry.marker().is_some() {
            continue;
        }
        if let HostPatterns::Patterns(patterns) = entry.host_patterns() {
            if patterns.iter().any(|h| h == &want) {
                return Ok(());
            }
        }
    }
    Err(format!(
        "no host key pinned for {want} in {known_hosts_path} (run `shed server add` first)"
    ))
}

/// The `knownhosts.Normalize(net.JoinHostPort(host, port))` form: bare `host` for port
/// 22, else `[host]:port`.
fn normalize(host: &str, port: u16) -> String {
    if port == 22 {
        host.to_string()
    } else {
        format!("[{host}]:{port}")
    }
}

/// The credential a source currently holds.
///
/// The two variants are what the two server modes issue; a source moves between them
/// whenever the server's mode changes under it, with no coordination from any caller.
enum Held {
    Token(String),
    Certificate {
        /// The rustls signing identity written through to the transport's resolver.
        certified: Arc<CertifiedKey>,
        /// The PEM pair, kept so a rotation can be persisted verbatim.
        cert_pem: String,
        key_pem: String,
        serial: String,
    },
}

impl Held {
    /// The `Authorization: Bearer` value, or `None` when the credential is a certificate.
    ///
    /// The mode gate is load-bearing, not tidiness: in mtls mode the server never reads
    /// the header, so a client that kept sending a stale bearer alongside its certificate
    /// would be shipping a live credential to an endpoint that ignores it — pure downside.
    fn bearer(&self) -> Option<String> {
        match self {
            Held::Token(t) => Some(t.clone()),
            Held::Certificate { .. } => None,
        }
    }

    /// The identity to present at the next handshake, if any.
    fn certified(&self) -> Option<Arc<CertifiedKey>> {
        match self {
            Held::Token(_) => None,
            Held::Certificate { certified, .. } => Some(certified.clone()),
        }
    }

    /// Does this credential have anything to present?
    ///
    /// An EMPTY token is not usable — the pre-mtls source keyed its cache on
    /// `token != ""`, so a mint that reported success with nothing to send re-minted on
    /// the next call rather than being cached forever. That behavior is preserved
    /// exactly; a certificate is always usable (it was pair-verified before adoption).
    fn usable(&self) -> bool {
        match self {
            Held::Token(t) => !t.is_empty(),
            Held::Certificate { .. } => true,
        }
    }
}

/// The mutable state of a [`CredentialSource`], guarded by a single mutex.
struct SourceState {
    cred: Option<Held>,
    expiry: Option<i64>,
    /// The server REFUSED the held credential (a 401 or a TLS certificate
    /// alert) and no replacement has been minted yet. While set, `obtain`
    /// skips its cache short-circuit — every call attempts a mint — but the
    /// credential itself STAYS held and armed on the transport: a rejection
    /// can be transient (a mid-rotation allowlist refresh), and presenting
    /// something the server might still accept beats presenting nothing
    /// (mirror Go `clienttoken.Source.Reject`). Cleared by a successful mint.
    rejected: bool,
    /// Latched host-key-mismatch error — once set, the source fails closed forever.
    terminal_err: Option<String>,
    /// The named user this source's certificates must be issued to (plan 037 §3.2.2):
    /// the target's `user` when the config records one; else the user of the hydrated
    /// certificate; else the first named certificate minted, once it is stored. Changes
    /// only from `None` to a name, under this lock, together with adopting that
    /// certificate, inside the single flight.
    held_user: Option<String>,
    /// The in-flight mint's join handle, if one is running (single-flight). Held until
    /// the mint's store write has completed, so two adoptions are written in the order
    /// they were adopted.
    inflight: Option<watch::Receiver<Option<Arc<MintOutcome>>>>,
    /// The last held-user refusal ([`ObtainError::held_user`]), answered again to every
    /// call while its wait ([`Self::refusal_until`]) runs. `None` once a credential is
    /// adopted.
    refusal: Option<ObtainError>,
    /// The wait the last held-user refusal started (shed-core's [`next_refusal_backoff`]:
    /// 1 s, doubling per consecutive refusal, up to 30 s); `None` once a credential is
    /// adopted, so the next refusal waits the floor again.
    refusal_backoff: Option<Duration>,
    /// Until when no mint is started after a held-user refusal (plan 037 A25), on the
    /// monotonic clock ([`CredentialSource::now_mono`]); `None` once a credential is
    /// adopted. Without it every answered request minted again while the held
    /// certificate was stale or rejected, and the server issued a certificate for the
    /// other user each time, to be thrown away.
    refusal_until: Option<Instant>,
}

/// The result a joined caller reads: `(bearer, expiry)` or why there is none. The bearer
/// is `None` in mtls state — the credential is presented by the TLS resolver instead, and
/// the distinction is what lets `token.get` refuse explicitly rather than hand back "".
type MintOutcome = Result<MintOutcomeOk, ObtainError>;
/// The success half of a [`MintOutcome`]: `(bearer, expiry)`.
type MintOutcomeOk = (Option<String>, Option<i64>);

/// Why an obtain produced no credential: the message, and whether it is the refusal of
/// a certificate the mint this call made or joined was issued ([`Self::refused`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ObtainError {
    pub message: String,
    /// The certificate this call's mint was issued was refused: for its user (the
    /// held-user rule, plan 037 §3.2.2), or because the certificate that would have
    /// established the first named user could not be stored. `message` is then the
    /// refusal, built from names already checked, never text a minter produced. Belongs
    /// to this call only: a request explains its own refusal with it, never another's.
    /// The one exception is the held-user rule's wait (plan 037 A25): until it has passed
    /// no mint is started, and a call that would have minted gets the refusal that started
    /// the wait, which is still why the source has nothing newer.
    pub refused: bool,
    /// Set only with [`Self::refused`], when the refusal is the held-user rule's
    /// ([`held_user_refusal`]: a certificate for another named user, or one that names no
    /// user), not an unreadable certificate or one that could not be stored. With nothing
    /// held to present, a request is then not sent at all (plan 037 A23): the server
    /// issues certificates, so it could not authenticate it.
    pub held_user: bool,
}

impl ObtainError {
    fn plain(message: String) -> Self {
        Self {
            message,
            refused: false,
            held_user: false,
        }
    }

    fn refusal(message: String) -> Self {
        Self {
            message,
            refused: true,
            held_user: false,
        }
    }

    fn held_user_refusal(message: String) -> Self {
        Self {
            message,
            refused: true,
            held_user: true,
        }
    }
}

/// Why a mint attempt produced no usable credential — kept as a two-armed enum only so the
/// terminal host-key latch stays keyed on the MINTER's error and can never be triggered by
/// an unusable-credential message that happens to read like one.
enum MintFailure {
    Minter(MinterError),
    Unusable(String),
    /// A certificate the held-user rule refuses (or whose user cannot be read): like
    /// `Unusable`, plus a positive check of what stays held.
    Refused(IssuedRefusal),
}

/// Why [`issued_user`] refuses a certificate: the message, and whether it is the
/// held-user rule's refusal ([`held_user_refusal`]) rather than an unreadable
/// certificate ([`ObtainError::held_user`]).
struct IssuedRefusal {
    message: String,
    held_user: bool,
}

/// What [`CredentialSource::do_mint`] decided under the state lock.
enum MintStep {
    /// Nothing adopted; the outcome is final.
    Done(MintOutcome),
    /// Adopted and armed already; `write` is the best-effort store write still to do.
    Adopted {
        outcome: MintOutcome,
        write: Option<(String, String)>,
    },
    /// The first named certificate: written first, then adopted (holding `name`) only
    /// when the write succeeded.
    Establish {
        held: Held,
        expiry: Option<i64>,
        name: String,
    },
}

/// Turn a freshly minted credential into the value the state machine stores, failing
/// closed on every shape that could not authenticate.
///
/// The one real check is the certificate/key pairing. It is not a TRUST check — the
/// certificate arrived over the host-key-pinned SSH channel and its trustworthiness is
/// established by the server's own CA at handshake time — but a correctness one: a
/// mismatched pair otherwise surfaces much later as an opaque handshake failure against a
/// server that looks broken. (Go's `sdk/bootstrap.RunCredential` runs the equivalent
/// `matchesIssuedCert` check at the same point in its flow.)
///
/// An empty TOKEN is deliberately not rejected here — see [`Held::usable`] for the
/// pre-existing "cache nothing, re-mint next call" behavior it preserves.
fn adopt(minted: Minted, server: &str) -> Result<(Held, Option<i64>), String> {
    match minted.credential {
        MintedCredential::Token(t) => Ok((Held::Token(t), minted.expiry)),
        MintedCredential::Certificate(c) => {
            let certified = certified_key_from_pem(&c.cert_pem, &c.key_pkcs8_der).map_err(|e| {
                format!("client certificate issued for \"{server}\" is unusable: {e}")
            })?;
            Ok((
                Held::Certificate {
                    certified: Arc::new(certified),
                    cert_pem: c.cert_pem,
                    key_pem: pem_encode(PEM_LABEL_PRIVATE_KEY, &c.key_pkcs8_der),
                    serial: c.serial,
                },
                minted.expiry,
            ))
        }
    }
}

/// The held-user check of a certificate just minted or loaded (plan 037 §3.2.2): the user
/// it names, or why it must not be used. A token has no user (`Ok(None)`).
///
/// Refused: a certificate whose user cannot be read, one that names no user, and one for a
/// named user other than `held`. The refusal text is shed-core's
/// [`held_user_refusal`] (the desktop shows one message for the app's control path and for
/// this source), or a fixed reason for an unreadable certificate.
fn issued_user(
    held: &Held,
    held_user: Option<&str>,
    server: &str,
) -> Result<Option<CertUser>, IssuedRefusal> {
    let Held::Certificate { cert_pem, .. } = held else {
        return Ok(None);
    };
    let user = cert_user_pem(cert_pem).map_err(|e| IssuedRefusal {
        message: format!(
            "client certificate issued for {} is unusable: {e}",
            display_server_name(server)
        ),
        held_user: false,
    })?;
    match held_user_refusal(server, held_user, &user) {
        Some(message) => Err(IssuedRefusal {
            message,
            held_user: true,
        }),
        None => Ok(Some(user)),
    }
}

/// Why a stored certificate is not presented at start, or `Ok` with the user it names.
/// The same rule as [`issued_user`], with the config's `user` as the only held user (a
/// stored certificate is what would set one otherwise).
fn stored_user(cert_pem: &str, config_user: Option<&str>) -> Result<CertUser, String> {
    let user = match cert_user_pem(cert_pem) {
        Ok(u) => u,
        Err(_) => return Err("its user cannot be read".to_string()),
    };
    match (&user, config_user) {
        (CertUser::Unbound, _) => {
            Err("it names no user (issued by a shed-server older than 0.10.0)".to_string())
        }
        (CertUser::Named(name), Some(held)) if name != held => Err(if is_user_name(held) {
            format!("it is for user \"{name}\" but this entry is held to \"{held}\"")
        } else {
            format!("it is for user \"{name}\" but this entry's user is not a valid user name")
        }),
        _ => Ok(user),
    }
}

/// An `sdk.TokenProvider` backed by the SSH credential minter (mirror
/// `credmint.go:credentialSource`). Construct via [`new_credential_source`]; hold it as
/// `Arc<CredentialSource>` (the obtain methods spawn the mint task, which needs an owned
/// clone).
///
/// # The held user (plan 037 §3.2.2)
///
/// Every certificate this source adopts or presents must be issued to the user it holds
/// ([`SourceState::held_user`]) or to the implicit user. A minted certificate for another
/// named user, or one that names no user, is refused like any unusable credential: nothing
/// is persisted and the held, armed credential stays. Such a refusal also starts a wait
/// (plan 037 A25, shed-core's [`next_refusal_backoff`]: 1 s, doubling per consecutive
/// refusal to 30 s, reset by an adopted credential, on the monotonic clock): until it has
/// passed no mint is started, and every call that would mint gets the same refusal, so the
/// server is not asked to issue another user's certificate on every request while the held
/// one is stale or was rejected. A stored certificate that fails the
/// same check is not presented at start (the source mints instead and
/// [`Self::hydration_note`] says why). While a name is held, a certificate for the implicit
/// user is adopted in memory but NOT written to the store, so the stored named certificate
/// stays the one a restart reads, and a restart cannot forget the name.
///
/// # The store (plan 037 §3.2.2)
///
/// A mint's store write is part of its single flight: no second mint starts until the
/// first one's write has completed, so writes land in the order the certificates were
/// adopted. The certificate that ESTABLISHES the first named user is written before it is
/// adopted, and when that write fails it is not adopted at all (the mint is a refusal that
/// says so): otherwise a restart would read the older certificate and forget the name.
/// Every other write is best-effort (the user it is for is already held, by config or by
/// a stored certificate). A source built for a watcher group never writes once that group
/// was cancelled ([`new_group_credential_source`]).
pub struct CredentialSource {
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: String,
    /// The handshake-time client-certificate source installed on the transports this
    /// source backs. Created here, handed to the `reqwest::Client` at build time, and
    /// rewritten on every mint — the seam that makes rotation and mode flips invisible to
    /// the client (plan 001 D5).
    cert_resolver: Arc<ClientCertResolver>,
    /// Where an issued certificate is persisted, when this scope persists at all.
    store: Option<CredStore>,
    /// Why the stored certificate was not presented at start, if it was not
    /// ([`Self::hydration_note`]).
    hydration_note: Option<String>,
    /// The watcher group's cancel signal, for a source built for one: once it reads
    /// `true` the source never writes to the store again (checked under the store's lock).
    fence: Option<watch::Receiver<bool>>,
    state: Mutex<SourceState>,
    /// The monotonic clock the refusal wait is measured on ([`SourceState::refusal_until`]):
    /// [`Instant::now`], replaced only by tests ([`Self::with_mono_clock`]).
    now_mono: Arc<dyn Fn() -> Instant + Send + Sync>,
    /// The wall clock, unix seconds, that a credential's expiry is judged by: when it is
    /// stale, when the refresh loop runs, and when a held certificate has expired.
    /// [`now_unix`], replaced only by tests ([`Self::with_wall_clock`]).
    now_unix: Arc<dyn Fn() -> i64 + Send + Sync>,
    /// Test seam: when set, the next store write first signals the first `Notify` and
    /// waits on the second (the single flight is still held while it waits).
    #[cfg(test)]
    pause_before_write: Mutex<Option<(Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>)>>,
}

/// Build a per-server credential source for `scope`.
///
/// The `credentials` scope — the agent's OWN bus credential, the one that must survive a
/// restart without an SSH round trip — gets the durable store under the agent's state
/// directory. Every other scope is in-memory only: the `control` scope exists to answer
/// one request at a time and is always minted fresh, so persisting it would create a
/// second copy of a private key with nothing reading it.
pub fn new_credential_source(
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: &str,
) -> Arc<CredentialSource> {
    new_credential_source_with_store(minter, target, scope, store_for_scope(scope))
}

/// The store a scope persists to: the durable one for `credentials`, none otherwise.
fn store_for_scope(scope: &str) -> Option<CredStore> {
    #[cfg(test)]
    if let Some(root) = TEST_CREDENTIALS_ROOT.with(|r| r.borrow().clone()) {
        return (scope == SCOPE_CREDENTIALS).then(|| CredStore::new(root));
    }
    (scope == SCOPE_CREDENTIALS).then(|| CredStore::for_scope(scope))
}

#[cfg(test)]
thread_local! {
    /// Test seam: when set, [`store_for_scope`] roots the durable store here instead of
    /// under the production state directory. Per thread, so it reaches only the test that
    /// set it (a `#[tokio::test]` builds its sources on its own thread).
    static TEST_CREDENTIALS_ROOT: std::cell::RefCell<Option<std::path::PathBuf>> =
        const { std::cell::RefCell::new(None) };
}

/// Root the production constructors' durable store at `root` on this thread until the
/// guard drops (tests of the production wiring, which must not touch the real state
/// directory).
#[cfg(test)]
pub(crate) fn root_credentials_store_at(root: &std::path::Path) -> TestCredentialsRoot {
    TEST_CREDENTIALS_ROOT.with(|r| *r.borrow_mut() = Some(root.to_path_buf()));
    TestCredentialsRoot
}

/// Clears [`root_credentials_store_at`]'s override on drop.
#[cfg(test)]
pub(crate) struct TestCredentialsRoot;

#[cfg(test)]
impl Drop for TestCredentialsRoot {
    fn drop(&mut self) {
        TEST_CREDENTIALS_ROOT.with(|r| *r.borrow_mut() = None);
    }
}

/// [`new_credential_source`] for one watcher group: `group` is the group's cancel signal.
/// Once it reads `true` (the supervisor stopped the group, or replaced it after its
/// target changed — a changed `user` among them) the source never writes to the store
/// again, so a mint the old group started and that finishes late cannot overwrite what
/// the replacement group, held to the new config, stored.
pub fn new_group_credential_source(
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: &str,
    group: watch::Receiver<bool>,
) -> Arc<CredentialSource> {
    build_credential_source(minter, target, scope, store_for_scope(scope), Some(group))
}

/// [`new_credential_source`] with an explicit store (`None` = never persist).
///
/// The seam exists for tests and for an embedder that owns its own state root; production
/// goes through [`new_credential_source`], which picks the store the scope implies.
pub fn new_credential_source_with_store(
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: &str,
    store: Option<CredStore>,
) -> Arc<CredentialSource> {
    build_credential_source(minter, target, scope, store, None)
}

/// [`new_group_credential_source`] with an explicit store (tests).
#[cfg(test)]
pub(crate) fn new_group_credential_source_with_store(
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: &str,
    store: Option<CredStore>,
    group: watch::Receiver<bool>,
) -> Arc<CredentialSource> {
    build_credential_source(minter, target, scope, store, Some(group))
}

fn build_credential_source(
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: &str,
    store: Option<CredStore>,
    fence: Option<watch::Receiver<bool>>,
) -> Arc<CredentialSource> {
    let cert_resolver = Arc::new(ClientCertResolver::new());
    // Rehydrate BEFORE the source is handed out, so the transport built from
    // `cert_resolver()` already presents the persisted identity on its first handshake
    // and the agent skips an SSH mint it does not need on every restart.
    let hydrated = store
        .as_ref()
        .filter(|_| !target.name.is_empty())
        .and_then(|s| s.load(&target.name).ok().flatten());
    // Config wins: the entry's `user` is the held user whenever it is recorded.
    let config_user = (!target.user.is_empty()).then(|| target.user.clone());
    let mut state = SourceState {
        cred: None,
        expiry: None,
        rejected: false,
        terminal_err: None,
        inflight: None,
        held_user: config_user.clone(),
        refusal: None,
        refusal_backoff: None,
        refusal_until: None,
    };
    let mut hydration_note = None;
    if let Some(sc) = hydrated {
        match stored_user(&sc.cert_pem, config_user.as_deref()) {
            Ok(user) => {
                // A stored named certificate holds its user when the config records
                // none — this is how a restart keeps the first named user.
                if let (None, CertUser::Named(name)) = (&state.held_user, &user) {
                    state.held_user = Some(name.clone());
                }
                cert_resolver.set(Some(sc.certified.clone()));
                state.expiry = sc.not_after_unix;
                state.cred = Some(Held::Certificate {
                    certified: sc.certified,
                    cert_pem: sc.cert_pem,
                    // The key PEM is not re-read into memory: nothing rewrites a
                    // credential it merely loaded — the next rotation persists its own
                    // freshly minted pair.
                    key_pem: String::new(),
                    serial: String::new(),
                });
            }
            // Not presented: the source starts with no credential and mints (J3: the
            // app cannot run a refetch, so it re-mints where the CLI would refuse).
            Err(reason) => {
                hydration_note = Some(format!(
                    "server {}: the stored {scope} certificate was not used: {reason}; \
                     a new one will be minted",
                    display_server_name(&target.name)
                ));
            }
        }
    }
    Arc::new(CredentialSource {
        minter,
        target,
        scope: scope.to_string(),
        cert_resolver,
        store,
        hydration_note,
        fence,
        state: Mutex::new(state),
        now_mono: Arc::new(Instant::now),
        now_unix: Arc::new(now_unix),
        #[cfg(test)]
        pause_before_write: Mutex::new(None),
    })
}

impl CredentialSource {
    /// Test seam: measure the refusal wait on `now` instead of [`Instant::now`]. Called
    /// right after the source is built, before any other handle to it exists.
    #[cfg(test)]
    pub(crate) fn with_mono_clock(
        mut self: Arc<Self>,
        now: impl Fn() -> Instant + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::get_mut(&mut self)
            .expect("the clock is set before the source is shared")
            .now_mono = Arc::new(now);
        self
    }

    /// Test seam: judge expiry on `now` (unix seconds) instead of the system clock. Called
    /// right after the source is built, before any other handle to it exists.
    #[cfg(test)]
    pub(crate) fn with_wall_clock(
        mut self: Arc<Self>,
        now: impl Fn() -> i64 + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::get_mut(&mut self)
            .expect("the clock is set before the source is shared")
            .now_unix = Arc::new(now);
        self
    }

    /// Whether the held credential is stale ([`stale`]) on this source's wall clock.
    fn stale_now(&self, st: &SourceState) -> bool {
        stale(st.expiry, (self.now_unix)())
    }

    /// While a held-user refusal stands (plan 037 A25: recorded, and no credential adopted
    /// since), a held certificate past its expiry is not something to present: it is
    /// dropped and withdrawn from the transport, so [`Self::holds_certificate`] answers
    /// `false` and no handshake offers it. A request then has nothing to present and,
    /// refused by the held-user rule, is not sent at all. The held USER is kept: only the
    /// certificate goes. A certificate still valid is kept and presented as before, and
    /// after any other failure an expired certificate stays as it was (the surviving
    /// credential, as shed-core's provider keeps it). Caller holds the state lock.
    fn withdraw_expired_locked(&self, st: &mut SourceState) {
        if st.refusal.is_some()
            && matches!(st.cred, Some(Held::Certificate { .. }))
            && expired(st.expiry, (self.now_unix)())
        {
            st.cred = None;
            self.cert_resolver.set(None);
        }
    }

    /// The kept held-user refusal while its wait runs (plan 037 A25), else `None`.
    /// Caller holds the state lock.
    fn refusal_waiting(&self, st: &SourceState) -> Option<ObtainError> {
        match (&st.refusal, st.refusal_until) {
            (Some(refusal), Some(until)) if (self.now_mono)() < until => Some(refusal.clone()),
            _ => None,
        }
    }

    /// The server this source mints for (used by the control-token provider to detect an
    /// endpoint change and recreate the source).
    pub fn target(&self) -> &ServerTarget {
        &self.target
    }

    /// Why the stored certificate was not presented at start (its user cannot be read,
    /// it names no user, or it names another user than the entry's `user`), for the
    /// caller to log; `None` when it was presented or there was none.
    pub fn hydration_note(&self) -> Option<&str> {
        self.hydration_note.as_deref()
    }

    /// The client-certificate resolver to install on this source's transport.
    ///
    /// The transport takes it at build time and never looks at it again: every later
    /// credential change is a write THROUGH this handle, which is what keeps the
    /// `reqwest::Client` immutable across rotations and mode flips (plan 001 D5). In token
    /// state it resolves to `None` and rustls continues without client authentication,
    /// byte-identically to a config built with no resolver at all.
    pub fn cert_resolver(&self) -> Arc<ClientCertResolver> {
        self.cert_resolver.clone()
    }

    /// The serial of the certificate currently held, or `None` in token state (and for a
    /// credential rehydrated from disk, whose serial was never recorded beside it).
    ///
    /// Opaque to the agent: it exists so a rotation is OBSERVABLE — logs and the rotation
    /// tests both need to say "serial A became serial B" about a transport that, by
    /// design, was never rebuilt and therefore looks unchanged from the outside.
    pub fn credential_serial(&self) -> Option<String> {
        match &self.state.lock().unwrap().cred {
            Some(Held::Certificate { serial, .. }) if !serial.is_empty() => Some(serial.clone()),
            _ => None,
        }
    }

    /// The current bearer token, minting or re-minting as needed (implements the token
    /// half of `sdk.TokenProvider`). Consumed by the bus provider bridge and by the egress
    /// SSE consumer's `EgressTokenSource` bridge; cleared by [`Self::invalidate`] after a
    /// 401. The `token.get` control path instead uses [`Self::force_token_with_expiry`]
    /// (never a cached copy).
    ///
    /// In mtls state this returns the EMPTY string rather than an error: the caller's job
    /// is to decide whether to send an `Authorization` header, and both bearer guards
    /// already read "" as "send none". The mint still happens — which is the point, since
    /// it is what arms the certificate the request will actually authenticate with.
    pub async fn token(self: &Arc<Self>) -> Result<String, String> {
        self.token_for_request().await.map_err(|e| e.message)
    }

    /// [`Self::token`] for one request, with why it failed: [`ObtainError::refused`] is
    /// set when the mint THIS call made or joined was refused (plan 037 §3.2.2), so the
    /// request explains a server's refusal with its own mint's refusal and never with one
    /// another request's mint produced. A credential served from the cache carries none.
    pub async fn token_for_request(self: &Arc<Self>) -> Result<String, ObtainError> {
        self.obtain(false)
            .await
            .map(|(bearer, _)| bearer.unwrap_or_default())
    }

    /// Drop any completed cached credential and mint fresh, while still coalescing callers
    /// that overlap a single in-flight mint. The control path (`token.get`) uses this: a
    /// restarted server silently invalidates control tokens, so a cached copy must never
    /// be served (mirror `credmint.go:forceTokenWithExpiry`).
    ///
    /// Unlike [`Self::token`], a certificate is an ERROR here. This method's one caller
    /// hands the result to a desktop app that asked for a bearer token; returning "" would
    /// surface much later as an unexplained 401 against a server that is working exactly
    /// as designed.
    pub async fn force_token_with_expiry(
        self: &Arc<Self>,
    ) -> Result<(String, Option<i64>), String> {
        match self.obtain(true).await.map_err(|e| e.message)? {
            (Some(tok), expiry) => Ok((tok, expiry)),
            (None, _) => Err(format!(
                "server \"{}\" issues client certificates (auth.mode: mtls); \
                 control-token minting is unavailable",
                self.target.name
            )),
        }
    }

    /// Mark the held credential server-refused so the next obtain re-mints. Called after
    /// a 401 or a classified TLS certificate alert (the bus `TokenProvider` and egress
    /// `EgressTokenSource` bridges; mirror Go `credentialSource.Invalidate` → `Reject`).
    ///
    /// Deliberately NON-destructive (whole-branch review finding 3): the credential stays
    /// held and the certificate stays armed on the transport's resolver. A rejection can
    /// be transient — a mid-rotation allowlist refresh — and if the replacement mint then
    /// FAILS (SSH down), a client that had withdrawn its certificate would send the next
    /// handshake with no identity at all, guaranteeing rejection even once the server
    /// would have accepted the old certificate again. Go keeps presenting the surviving
    /// credential in exactly this state; the `rejected` flag is what forces the re-mint
    /// attempt without discarding what we hold.
    pub fn invalidate(&self) {
        self.state.lock().unwrap().rejected = true;
    }

    /// The bearer still held after a failed re-mint, or `None` (no credential,
    /// a certificate — which travels in the handshake, not a header — or the
    /// terminal fail-closed state). Lets a caller keep presenting what the
    /// server might still accept while the mint error surfaces (Go parity:
    /// `credentialSource.Token` returns the surviving token WITH the error).
    pub fn surviving_bearer(&self) -> Option<String> {
        let st = self.state.lock().unwrap();
        if st.terminal_err.is_some() {
            return None; // fail closed — never present after a host-key pin mismatch
        }
        st.cred
            .as_ref()
            .and_then(Held::bearer)
            .filter(|t| !t.is_empty())
    }

    /// Whether a certificate identity is currently held (and therefore armed on
    /// the transport's resolver) — used by callers to pick the right log line
    /// when a re-mint fails, and to send nothing when a held-user refusal leaves
    /// nothing to present. While a held-user refusal stands, a certificate past its
    /// expiry is not held: it is withdrawn here first ([`Self::withdraw_expired_locked`]),
    /// so this answer and what the next handshake offers agree.
    pub fn holds_certificate(&self) -> bool {
        let mut st = self.state.lock().unwrap();
        self.withdraw_expired_locked(&mut st);
        st.terminal_err.is_none() && matches!(st.cred, Some(Held::Certificate { .. }))
    }

    /// Whether [`Self::token`] would now start or join a mint instead of serving the held
    /// credential — the same test `obtain` makes: a mint is in flight, nothing usable is
    /// held, it is stale, or the server REFUSED it (the `rejected` flag, which stays set
    /// until a mint succeeds — so after a failed re-mint every call mints again). The
    /// terminal state fails at once without minting, and so does a call while a held-user
    /// refusal's wait runs (plan 037 A25): it is answered the kept refusal. Lets a caller
    /// bound its mint attempts (a bus respond makes at most one).
    pub fn would_mint(&self) -> bool {
        let mut st = self.state.lock().unwrap();
        if st.terminal_err.is_some() {
            return false;
        }
        if st.inflight.is_some() {
            return true;
        }
        self.withdraw_expired_locked(&mut st);
        !matches!(&st.cred, Some(c) if c.usable() && !self.stale_now(&st) && !st.rejected)
            && self.refusal_waiting(&st).is_none()
    }

    /// Proactively re-mint, best-effort (errors surface on the next obtain). Driven by
    /// [`Self::refresh_loop`] so an idle server's token stays fresh.
    #[cfg(test)]
    pub async fn refresh(self: &Arc<Self>) {
        let _ = self.obtain_refresh().await;
    }

    /// The shared body of the obtain variants (mirror `obtainTokenWithExpiry`). Fails
    /// closed on the terminal error, then serves a fresh cached token or starts/joins a
    /// single-flight mint. `force` skips the cache short-circuit and clears any completed
    /// token so it is never served.
    async fn obtain(self: &Arc<Self>, force: bool) -> Result<MintOutcomeOk, ObtainError> {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if let Some(err) = &st.terminal_err {
                return Err(ObtainError::plain(err.clone()));
            }
            self.withdraw_expired_locked(&mut st);
            if force {
                st.cred = None; // force a re-mint; never serve a completed cached credential
            } else if let Some(c) = &st.cred {
                // A rejected credential is disqualified from the cache
                // short-circuit even when locally unexpired: the server
                // refused it, so serving it as "fresh" would skip the re-mint
                // forever (mirror Go EnsureFreshErr's !rejected condition).
                if c.usable() && !self.stale_now(&st) && !st.rejected {
                    return Ok((c.bearer(), st.expiry));
                }
            }
            self.obtain_locked(&mut st)?
        };
        // Await OFF the lock (Go releases s.mu before <-call.done).
        wait_outcome(rx).await
    }

    /// Always start/join a mint (no cache short-circuit and no completed-token clear) —
    /// the proactive-refresh path (mirror `credmint.go:refresh`, which calls
    /// `obtainLocked` directly). Shared by [`Self::refresh`] (tests) and
    /// [`Self::refresh_loop`] (the supervisor spawns the loop per secure server).
    async fn obtain_refresh(self: &Arc<Self>) -> Result<MintOutcomeOk, ObtainError> {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if let Some(err) = &st.terminal_err {
                return Err(ObtainError::plain(err.clone()));
            }
            self.withdraw_expired_locked(&mut st);
            self.obtain_locked(&mut st)?
        };
        wait_outcome(rx).await
    }

    /// Return the in-flight mint's receiver, starting one (spawned, off the lock) if none
    /// is running so N concurrent callers share ONE mint. Caller holds the state lock.
    ///
    /// While a held-user refusal's wait runs (plan 037 A25) no mint is started: the call
    /// gets the kept refusal at once, as the mint that recorded it answered, and the held
    /// certificate stays armed when [`Self::keep_only_held`] kept it, until it expires
    /// ([`Self::withdraw_expired_locked`]). Every other failure
    /// starts no wait, so it is retried as before.
    fn obtain_locked(
        self: &Arc<Self>,
        st: &mut SourceState,
    ) -> Result<watch::Receiver<Option<Arc<MintOutcome>>>, ObtainError> {
        if let Some(rx) = &st.inflight {
            return Ok(rx.clone());
        }
        if let Some(refusal) = self.refusal_waiting(st) {
            return Err(refusal);
        }
        let (tx, rx) = watch::channel(None);
        st.inflight = Some(rx.clone());
        let this = Arc::clone(self);
        tokio::spawn(async move { this.do_mint(tx).await });
        Ok(rx)
    }

    /// Perform the mint off the state lock, then decide and adopt under it. A host-key
    /// pin mismatch is recorded as terminal (fail closed) so it is never retried.
    ///
    /// A certificate is ARMED on the transport's resolver before this returns — the
    /// caller's very next request may handshake — and written to the store outside the
    /// state lock (the store takes a cross-process `flock`, which must never be held under
    /// an in-process mutex every other caller is waiting on), but still INSIDE the single
    /// flight: the in-flight marker is cleared only once the write has completed, so no
    /// later mint can adopt, and write, before this one's write lands.
    ///
    /// The certificate that establishes the first named user is written FIRST and adopted
    /// only when that write succeeds; a failed write makes the mint a refusal (a restart
    /// would otherwise read the older stored certificate and forget the name). Any other
    /// write is best-effort: the credential is live in memory, the user it is for is
    /// already held, and an unwritten file costs one extra SSH mint after a restart.
    async fn do_mint(self: Arc<Self>, tx: watch::Sender<Option<Arc<MintOutcome>>>) {
        let result = self.minter.mint(&self.target, &self.scope).await;
        let step = {
            let mut st = self.state.lock().unwrap();
            self.decide_locked(&mut st, result)
        };
        let outcome: MintOutcome = match step {
            MintStep::Done(outcome) => outcome,
            MintStep::Adopted { outcome, write } => {
                if let Some((cert_pem, key_pem)) = write {
                    let _ = self.write_store(&cert_pem, &key_pem).await;
                }
                outcome
            }
            MintStep::Establish { held, expiry, name } => {
                let written = match &held {
                    Held::Certificate {
                        cert_pem, key_pem, ..
                    } => self.write_store(cert_pem, key_pem).await,
                    Held::Token(_) => Ok(()),
                };
                let mut st = self.state.lock().unwrap();
                match written {
                    Ok(()) => {
                        st.held_user = Some(name);
                        Ok(self.arm_locked(&mut st, held, expiry))
                    }
                    Err(e) => {
                        self.keep_only_held(&mut st);
                        Err(ObtainError::refusal(format!(
                            "server {}: the server issued a certificate for user \"{name}\", \
                             the first user this entry would be held to, but it could not be \
                             stored ({e}); it was not used, so that a restart cannot forget \
                             the user. Make the app's state directory writable, then restart \
                             the app.",
                            display_server_name(&self.target.name)
                        )))
                    }
                }
            }
        };
        // The single flight ends only now, after the write.
        self.state.lock().unwrap().inflight = None;
        // The joined callers read THIS call's outcome (not a re-read of shared state).
        let _ = tx.send(Some(Arc::new(outcome)));
    }

    /// Present (mtls) or withdraw (token) the identity and hold the credential. Caller
    /// holds the state lock. Done BEFORE the mint returns, so a mode flip in either
    /// direction lands on the transport atomically with the credential the caller is
    /// about to use.
    fn arm_locked(&self, st: &mut SourceState, held: Held, expiry: Option<i64>) -> MintOutcomeOk {
        let bearer = held.bearer();
        self.cert_resolver.set(held.certified());
        st.expiry = expiry;
        st.cred = Some(held);
        // A fresh credential has no rejection history, and ends a refusal episode: the next
        // refusal waits the floor.
        st.rejected = false;
        st.refusal = None;
        st.refusal_backoff = None;
        st.refusal_until = None;
        (bearer, expiry)
    }

    /// Write a certificate to the store, when this source has one: under the store's
    /// lock, and never once this source's watcher group was cancelled (`fence`).
    async fn write_store(&self, cert_pem: &str, key_pem: &str) -> Result<(), String> {
        let Some(store) = &self.store else {
            return Ok(());
        };
        if self.target.name.is_empty() {
            // Nothing is ever read back for an unnamed target (see the hydration).
            return Ok(());
        }
        #[cfg(test)]
        {
            let pause = self.pause_before_write.lock().unwrap().take();
            if let Some((paused, resume)) = pause {
                paused.notify_one();
                resume.notified().await;
            }
        }
        store.write_unless(&self.target.name, cert_pem, key_pem, || {
            self.fence
                .as_ref()
                .filter(|f| *f.borrow())
                .map(|_| "this server's watcher was stopped or replaced".to_string())
        })
    }

    /// The decision of [`Self::do_mint`], under the state lock, against the user held
    /// NOW: what to adopt (adopting it here, unless it establishes the first named user),
    /// what to write, or why there is nothing.
    fn decide_locked(&self, st: &mut SourceState, result: Result<Minted, MinterError>) -> MintStep {
        let held_user = st.held_user.clone();
        let checked = result
            .map_err(MintFailure::Minter)
            .and_then(|minted| adopt(minted, &self.target.name).map_err(MintFailure::Unusable))
            .and_then(|(held, expiry)| {
                issued_user(&held, held_user.as_deref(), &self.target.name)
                    .map(|user| (held, expiry, user))
                    .map_err(MintFailure::Refused)
            });
        match checked {
            Ok((held, expiry, user)) => {
                // The first named certificate is held only once it is stored.
                if let (None, Some(CertUser::Named(name))) = (&st.held_user, &user) {
                    return MintStep::Establish {
                        held,
                        expiry,
                        name: name.clone(),
                    };
                }
                // While a name is held, an implicit certificate is used but not stored:
                // the stored named certificate stays what a restart reads.
                let store_it =
                    !(matches!(user, Some(CertUser::Implicit)) && st.held_user.is_some());
                let write = match &held {
                    Held::Certificate {
                        cert_pem, key_pem, ..
                    } if store_it && !key_pem.is_empty() => {
                        Some((cert_pem.clone(), key_pem.clone()))
                    }
                    _ => None,
                };
                MintStep::Adopted {
                    outcome: Ok(self.arm_locked(st, held, expiry)),
                    write,
                }
            }
            Err(MintFailure::Unusable(msg)) => MintStep::Done(Err(ObtainError::plain(msg))),
            Err(MintFailure::Refused(r)) => {
                // Nothing is persisted and the held credential stays armed — but only
                // after a positive check that it is still for the held user.
                self.keep_only_held(st);
                if !r.held_user {
                    // An unreadable certificate: no wait, retried as before.
                    return MintStep::Done(Err(ObtainError::refusal(r.message)));
                }
                // The held-user rule's refusal starts the wait (plan 037 A25): no mint
                // until it has passed, and every call meanwhile gets this refusal. On
                // the monotonic clock read after the mint (it took time), overflow-safe.
                let refusal = ObtainError::held_user_refusal(r.message);
                let wait = next_refusal_backoff(st.refusal_backoff);
                let now = (self.now_mono)();
                st.refusal_backoff = Some(wait);
                st.refusal_until = Some(now.checked_add(wait).unwrap_or(now));
                st.refusal = Some(refusal.clone());
                // The kept certificate is presented only while it is still valid.
                self.withdraw_expired_locked(st);
                MintStep::Done(Err(refusal))
            }
            Err(MintFailure::Minter(e)) if e.is_host_key_mismatch() => {
                // Go: fmt.Errorf("refusing to broker %q: SSH host key pin mismatch
                // (possible MITM): %w", name, err). Explicit quoted-name (not {:?}).
                let msg = format!(
                    "refusing to broker \"{}\": SSH host key pin mismatch (possible MITM): {}",
                    self.target.name,
                    e.message()
                );
                st.terminal_err = Some(msg.clone());
                // Withdraw EVERYTHING (fail closed): a possible MITM is
                // the one state where "keep presenting what the server
                // might still accept" inverts — the held credential and
                // the armed transport identity are both dropped, so no
                // handshake or header ever presents them again.
                st.cred = None;
                st.expiry = None;
                self.cert_resolver.set(None);
                MintStep::Done(Err(ObtainError::plain(msg)))
            }
            Err(MintFailure::Minter(e)) => {
                MintStep::Done(Err(ObtainError::plain(e.message().to_string())))
            }
        }
    }

    /// After a refused replacement: keep the held certificate only when it is positively
    /// for the held user or the implicit user; otherwise withdraw it from the transport.
    /// Every certificate this source holds passed the same rule when it was adopted or
    /// loaded, so in practice this keeps it; it is the check keeping it rests on. A token
    /// has no user and is kept. Caller holds the state lock.
    fn keep_only_held(&self, st: &mut SourceState) {
        let Some(held @ Held::Certificate { .. }) = &st.cred else {
            return;
        };
        let keep = match issued_user(held, st.held_user.as_deref(), &self.target.name) {
            Ok(Some(CertUser::Implicit)) => true,
            Ok(Some(CertUser::Named(name))) => st.held_user.as_deref() == Some(name.as_str()),
            _ => false,
        };
        if !keep {
            st.cred = None;
            st.expiry = None;
            self.cert_resolver.set(None);
        }
    }

    /// Proactively re-mint at ~50% of the time until expiry (jittered), so an idle
    /// server's token stays fresh. Stops when `cancel` resolves. Spawned per secure server
    /// by the supervisor's group builder ([`crate::bus::spawn_server_group`]).
    pub async fn refresh_loop(self: Arc<Self>, mut cancel: watch::Receiver<bool>) {
        loop {
            let delay = self.next_refresh_delay();
            tokio::select! {
                _ = cancel.wait_for(|c| *c) => return,
                _ = tokio::time::sleep(delay) => {}
            }
            if *cancel.borrow() {
                return;
            }
            // Race the wait against cancel: a shutdown arriving mid-mint must
            // not hold the group join for the mint timeout. Safe to abandon —
            // the mint itself runs in its own spawned task (obtain_locked) and
            // finishes for any other joiner; only this loop's WAIT is dropped.
            tokio::select! {
                _ = cancel.wait_for(|c| *c) => return,
                _ = self.obtain_refresh() => {}
            }
        }
    }

    /// ~50% of the time until the cached token expires, jittered ±25%, clamped
    /// `[MIN_REFRESH_DELAY, MAX_REFRESH_DELAY]`; `DEFAULT_REFRESH_DELAY` when no token has
    /// been minted yet (mirror `nextRefreshDelay`).
    fn next_refresh_delay(&self) -> Duration {
        let expiry = self.state.lock().unwrap().expiry;
        apply_jitter_and_clamp(
            base_refresh_delay(expiry, (self.now_unix)()),
            random_jitter_fraction(),
        )
    }
}

/// Whether a cached token with the given expiry is within [`TOKEN_REFRESH_WINDOW`] of
/// expiry at `now` (a `None` expiry is a non-expiring token — never stale). Mirror
/// `staleLocked`: `!expiry.IsZero() && now >= expiry - window`.
fn stale(expiry: Option<i64>, now: i64) -> bool {
    match expiry {
        None => false,
        Some(exp) => now >= exp - TOKEN_REFRESH_WINDOW.as_secs() as i64,
    }
}

/// Whether a credential with the given expiry has expired at `now` (unix seconds; a
/// `None` expiry never does). The same rule as shed-core's provider (`now >= exp`): a
/// certificate is not counted valid in the second its validity ends.
fn expired(expiry: Option<i64>, now: i64) -> bool {
    matches!(expiry, Some(exp) if now >= exp)
}

/// The pre-jitter refresh base: `remaining/2` while the token is valid, `MIN_REFRESH_DELAY`
/// once expired, `DEFAULT_REFRESH_DELAY` before any mint (mirror the branch in
/// `nextRefreshDelay`).
fn base_refresh_delay(expiry: Option<i64>, now: i64) -> Duration {
    match expiry {
        None => DEFAULT_REFRESH_DELAY,
        Some(exp) => {
            let remaining = exp - now;
            if remaining > 0 {
                Duration::from_secs((remaining / 2) as u64)
            } else {
                MIN_REFRESH_DELAY
            }
        }
    }
}

/// Spread `base` by ±[`JITTER_FRACTION`] using `frac` ∈ [-1, 1], then clamp to
/// `[MIN_REFRESH_DELAY, MAX_REFRESH_DELAY]`.
fn apply_jitter_and_clamp(base: Duration, frac: f64) -> Duration {
    let base_s = base.as_secs_f64();
    let d = base_s + frac * JITTER_FRACTION * base_s;
    let clamped = d.clamp(
        MIN_REFRESH_DELAY.as_secs_f64(),
        MAX_REFRESH_DELAY.as_secs_f64(),
    );
    Duration::from_secs_f64(clamped)
}

/// A pseudo-random fraction in [-1, 1) for jitter (fleet de-synchronization; not
/// cryptographic). Derived from the sub-nanosecond wall clock to avoid a `rand` dep,
/// matching the crate's dependency-minimal posture.
fn random_jitter_fraction() -> f64 {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    // (2*u - 1) is uniform in [-1, 1).
    2.0 * (nanos as f64 / 1_000_000_000.0) - 1.0
}

/// Await a single-flight mint's result off the state lock.
async fn wait_outcome(
    mut rx: watch::Receiver<Option<Arc<MintOutcome>>>,
) -> Result<MintOutcomeOk, ObtainError> {
    loop {
        {
            let guard = rx.borrow_and_update();
            if let Some(outcome) = guard.as_ref() {
                return (**outcome).clone();
            }
        }
        if rx.changed().await.is_err() {
            return Err(ObtainError::plain(
                "mint task ended unexpectedly".to_string(),
            ));
        }
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::bootstrap::{BootstrapError, Bundle};
    use shed_core::token::{REFUSAL_BACKOFF_CEILING, REFUSAL_BACKOFF_FLOOR};
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn target(name: &str, host: &str, port: u16) -> ServerTarget {
        ServerTarget {
            name: name.to_string(),
            url: String::new(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: host.to_string(),
            ssh_port: port,
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        }
    }

    // ---- known_hosts_pinned + normalize (mirror credmint_test.go) -------------------

    fn write_known_hosts(lines: &str) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("known_hosts");
        std::fs::write(&path, lines).unwrap();
        (dir, path.to_string_lossy().into_owned())
    }

    /// A syntactically-valid ssh-ed25519 known_hosts key blob (the committed harness
    /// test key's public part); the VALUE is irrelevant to the presence check.
    const ED25519_KEY: &str =
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILMdrPP0NLsfgrc8JIa6OWX1qhgyfW/UwJTSXdRuUsJJ";

    #[test]
    fn normalize_bare_host_for_port_22() {
        assert_eq!(normalize("mini3", 22), "mini3");
        assert_eq!(normalize("mini3", 2222), "[mini3]:2222");
    }

    #[test]
    fn known_hosts_pinned_present() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_ok());
    }

    #[test]
    fn known_hosts_pinned_port_22_bare_host() {
        let (_d, path) = write_known_hosts(&format!("mini3 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 22).is_ok());
    }

    #[test]
    fn known_hosts_pinned_comma_host_list() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222,[alt]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "alt", 2222).is_ok());
    }

    #[test]
    fn known_hosts_pinned_accepts_ecdsa() {
        // Go's x/crypto ParseKnownHosts accepts ecdsa (and rsa/dsa) host keys, so the
        // Rust presence check must too (shed treats ecdsa as first-class; a host key
        // other than ed25519 must not fail the pin pre-check). Regression guard against
        // an ssh-key ed25519-only build, which would `Err("unknown algorithm")` here.
        const ECDSA: &str = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBOTs3y4oiuC0EFlPcY3mL1v7XtzII37Vtz9qXbbXlglHDWUbdPnfUZ0Z42cpLHYwAQSy8hOqmzOkD5EGrCXKuh4=";
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ECDSA}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_ok());
    }

    #[test]
    fn known_hosts_pinned_errors() {
        // Missing file.
        assert!(known_hosts_pinned("/nonexistent/known_hosts", "mini3", 2222).is_err());
        // Pins a different host.
        let (_d, path) = write_known_hosts(&format!("[other]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_err());
    }

    #[test]
    fn known_hosts_pinned_skips_revoked() {
        let (_d, path) = write_known_hosts(&format!("@revoked [mini3]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_err());
    }

    #[test]
    fn known_hosts_pinned_skips_cert_authority() {
        let (_d, path) =
            write_known_hosts(&format!("@cert-authority [mini3]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_err());
    }

    // ---- CredentialMinter::mint (mirror TestCredentialMinterMint) -------------------

    /// A BootstrapRunner returning a canned outcome and counting calls.
    struct FakeRunner {
        result: std::sync::Mutex<Vec<Result<Bundle, BootstrapErrorKind>>>,
        calls: AtomicUsize,
        /// The last `Params` the minter composed — how the CSR-composition tests see what
        /// would actually have reached `ssh`.
        last_params: std::sync::Mutex<Option<Params>>,
    }
    /// A cloneable stand-in for BootstrapError (which isn't Clone) for the fake's script.
    enum BootstrapErrorKind {
        Mismatch,
    }
    impl FakeRunner {
        fn ok(bundle: Bundle) -> Arc<Self> {
            Arc::new(Self {
                result: std::sync::Mutex::new(vec![Ok(bundle)]),
                calls: AtomicUsize::new(0),
                last_params: std::sync::Mutex::new(None),
            })
        }
        fn mismatch() -> Arc<Self> {
            Arc::new(Self {
                result: std::sync::Mutex::new(vec![Err(BootstrapErrorKind::Mismatch)]),
                calls: AtomicUsize::new(0),
                last_params: std::sync::Mutex::new(None),
            })
        }
    }
    #[async_trait::async_trait]
    impl BootstrapRunner for FakeRunner {
        async fn run(&self, p: &Params) -> Result<Bundle, BootstrapError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            *self.last_params.lock().unwrap() = Some(p.clone());
            let mut r = self.result.lock().unwrap();
            match r.remove(0) {
                Ok(b) => Ok(b),
                Err(BootstrapErrorKind::Mismatch) => {
                    Err(BootstrapError::HostKeyMismatch("changed".into()))
                }
            }
        }
    }

    fn bundle(token: &str) -> Bundle {
        Bundle {
            auth_mode: String::new(),
            http_port: 8080,
            https_port: 0,
            tls_cert_fingerprint: String::new(),
            token: token.to_string(),
            client_cert: String::new(),
            scope: String::new(),
            token_id: "t1".into(),
            cert_serial: String::new(),
            expires_at: Some(now_unix() + 3600),
        }
    }

    #[tokio::test]
    async fn mint_success_passes_params_and_returns_token() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let runner = FakeRunner::ok(bundle("tok"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        let minted = m
            .mint(&target("s", "mini3", 2222), SCOPE_CREDENTIALS)
            .await
            .unwrap();
        assert_eq!(minted.bearer(), Some("tok"));
        assert_eq!(runner.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn mint_host_key_mismatch_is_terminal() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let m = CredentialMinter::with_runner(&path, FakeRunner::mismatch());
        let err = m
            .mint(&target("s", "mini3", 2222), SCOPE_CONTROL)
            .await
            .unwrap_err();
        assert!(err.is_host_key_mismatch());
        assert!(err
            .message()
            .starts_with("bootstrapping control token for \"s\":"));
    }

    #[tokio::test]
    async fn mint_missing_pin_is_non_terminal_and_skips_runner() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let runner = FakeRunner::ok(bundle("x"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        // A different, unpinned server: the pre-check fails before the runner runs.
        let err = m
            .mint(&target("s", "unpinned", 1), SCOPE_CREDENTIALS)
            .await
            .unwrap_err();
        assert!(!err.is_host_key_mismatch());
        assert_eq!(runner.calls.load(Ordering::SeqCst), 0);
    }

    // ---- CredentialSource (mirror the credmint_test.go source tests) ----------------

    /// A `Minter` returning canned results in sequence (repeating the last) + call count.
    pub(crate) struct FakeMinter {
        results: std::sync::Mutex<Vec<Result<Minted, bool>>>, // Err(bool) = terminal?
        pub(crate) calls: AtomicUsize,
    }
    impl FakeMinter {
        pub(crate) fn new(results: Vec<Result<Minted, bool>>) -> Arc<Self> {
            Arc::new(Self {
                results: std::sync::Mutex::new(results),
                calls: AtomicUsize::new(0),
            })
        }
    }
    #[async_trait::async_trait]
    impl Minter for FakeMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            let i = self.calls.fetch_add(1, Ordering::SeqCst);
            let results = self.results.lock().unwrap();
            let idx = i.min(results.len() - 1);
            match &results[idx] {
                Ok(m) => Ok(m.clone()),
                Err(true) => Err(MinterError::terminal("mismatch".into())),
                Err(false) => Err(MinterError::new("transient".into())),
            }
        }
    }

    fn minted(token: &str) -> Minted {
        Minted::token(token, Some(now_unix() + 24 * 3600))
    }

    /// A monotonic clock a test moves by hand, for the refusal wait
    /// ([`CredentialSource::with_mono_clock`]): it reads the same instant until advanced.
    #[derive(Clone)]
    pub(crate) struct MonoClock(Arc<std::sync::Mutex<Instant>>);

    impl MonoClock {
        pub(crate) fn new() -> Self {
            Self(Arc::new(std::sync::Mutex::new(Instant::now())))
        }

        pub(crate) fn advance(&self, by: Duration) {
            *self.0.lock().unwrap() += by;
        }

        /// The source's clock: reads this one.
        pub(crate) fn reader(&self) -> impl Fn() -> Instant + Send + Sync + 'static {
            let at = self.0.clone();
            move || *at.lock().unwrap()
        }

        /// `source`, measuring its refusal wait on this clock.
        pub(crate) fn on(&self, source: Arc<CredentialSource>) -> Arc<CredentialSource> {
            source.with_mono_clock(self.reader())
        }
    }

    /// A wall clock (unix seconds) a test sets by hand, for expiry
    /// ([`CredentialSource::with_wall_clock`]).
    #[derive(Clone)]
    pub(crate) struct WallClock(Arc<std::sync::atomic::AtomicI64>);

    impl WallClock {
        pub(crate) fn at(unix: i64) -> Self {
            Self(Arc::new(std::sync::atomic::AtomicI64::new(unix)))
        }

        pub(crate) fn set(&self, unix: i64) {
            self.0.store(unix, Ordering::SeqCst);
        }

        /// `source`, judging expiry on this clock.
        pub(crate) fn on(&self, source: Arc<CredentialSource>) -> Arc<CredentialSource> {
            let at = self.0.clone();
            source.with_wall_clock(move || at.load(Ordering::SeqCst))
        }
    }

    /// A source held to pat on both clocks: pat's certificate expiring at `expiry`, then
    /// `admin`'s from every later mint (plan 037 A25). The wall clock starts a day before
    /// `expiry`, so pat's is fresh.
    pub(crate) fn pat_then_admin(
        scope: &str,
        expiry: i64,
    ) -> (Arc<FakeMinter>, Arc<CredentialSource>, MonoClock, WallClock) {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, Some(expiry))),
            Ok(minted_cert_for(
                &ca,
                0x0b,
                Some(expiry + 86_400),
                &TestUser::Named("admin"),
            )),
        ]);
        let mono = MonoClock::new();
        let wall = WallClock::at(expiry - 86_400);
        let s = wall.on(mono.on(new_credential_source_with_store(
            fm.clone(),
            held_target("pat"),
            scope,
            None,
        )));
        (fm, s, mono, wall)
    }

    #[tokio::test]
    async fn source_caches_and_re_mints_on_invalidate() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Ok(minted("tok2"))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert_eq!(s.token().await.unwrap(), "tok1");
        assert_eq!(s.token().await.unwrap(), "tok1"); // served from cache
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1);
        s.invalidate();
        assert_eq!(s.token().await.unwrap(), "tok2"); // re-mint
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    // The egress slice un-gated `token()`/`invalidate()` from `#[cfg(test)]` to `pub`
    // (first production consumer). These two rows re-pin the cache + invalidate semantics
    // unguarded — the plan's BLOCKER-1 mapping rows.
    #[tokio::test]
    async fn credential_source_token_caches() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Ok(minted("tok2"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CONTROL);
        assert_eq!(s.token().await.unwrap(), "tok1");
        assert_eq!(s.token().await.unwrap(), "tok1"); // 2nd call served from cache
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1, "cached → one mint");
    }

    #[tokio::test]
    async fn credential_source_invalidate_forces_remint() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Ok(minted("tok2"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CONTROL);
        assert_eq!(s.token().await.unwrap(), "tok1");
        s.invalidate();
        assert_eq!(s.token().await.unwrap(), "tok2"); // next token() re-mints
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    /// Whole-branch review finding 3, token half: a rejection whose
    /// replacement mint FAILS keeps the surviving token presentable
    /// (`surviving_bearer`) while `token()` surfaces the mint error, and the
    /// next successful mint recovers. Counted in real MINT attempts.
    #[tokio::test]
    async fn invalidate_keeps_token_presentable_when_remint_fails() {
        let fm = FakeMinter::new(vec![
            Ok(minted("tok1")),
            Err(false), // invalidate's forced re-mint fails
            Ok(minted("tok2")),
        ]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CONTROL);
        assert_eq!(s.token().await.unwrap(), "tok1");
        s.invalidate();
        let err = s.token().await.unwrap_err();
        assert!(err.contains("transient"), "mint error must surface: {err}");
        assert_eq!(
            s.surviving_bearer().as_deref(),
            Some("tok1"),
            "the rejected-but-surviving token stays presentable"
        );
        // Recovery: the next call mints, clearing the rejection.
        assert_eq!(s.token().await.unwrap(), "tok2");
        assert_eq!(s.token().await.unwrap(), "tok2"); // cached again
        assert_eq!(
            fm.calls.load(Ordering::SeqCst),
            3,
            "initial + failed re-mint + recovery; steady state is cached"
        );
    }

    /// Whole-branch review finding 3, certificate half: invalidate must NOT
    /// withdraw the armed certificate — a failed replacement mint would leave
    /// every handshake identity-less, guaranteeing rejection even once a
    /// transient revocation is rolled back (Go keeps presenting; so do we).
    #[tokio::test]
    async fn invalidate_keeps_certificate_armed_when_remint_fails() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x01, Some(now_unix() + 86_400))),
            Err(false), // every re-mint fails from here on
        ]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert_eq!(s.token().await.unwrap(), ""); // mtls state: bearer is empty
        assert!(s.cert_resolver().current().is_some(), "certificate armed");

        s.invalidate();
        assert!(
            s.cert_resolver().current().is_some(),
            "invalidate must not strip the transport of the surviving identity"
        );
        assert!(s.holds_certificate());
        assert_eq!(s.surviving_bearer(), None, "a certificate is not a bearer");

        let err = s.token().await.unwrap_err();
        assert!(err.contains("transient"), "mint error surfaces: {err}");
        assert!(
            s.cert_resolver().current().is_some(),
            "the certificate survives the failed re-mint too"
        );
        assert_eq!(
            fm.calls.load(Ordering::SeqCst),
            2,
            "initial + exactly one forced re-mint attempt for this token() call"
        );
    }

    /// The rejected state forces one real mint attempt per obtain — bounded,
    /// no cache short-circuit, no runaway loop within a single call.
    #[tokio::test]
    async fn rejected_state_attempts_one_mint_per_obtain() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Err(false)]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CONTROL);
        assert_eq!(s.token().await.unwrap(), "tok1");
        s.invalidate();
        for i in 0..3 {
            let _ = s.token().await;
            assert_eq!(
                fm.calls.load(Ordering::SeqCst),
                2 + i,
                "one mint attempt per obtain while rejected"
            );
        }
    }

    /// The surviving-credential contract INVERTS on a latched host-key
    /// mismatch: a possible MITM withdraws the held credential AND the armed
    /// transport identity — nothing is presented again. (The empty-state
    /// terminal test cannot catch this; production commonly already holds a
    /// credential when the replacement mint goes terminal.)
    #[tokio::test]
    async fn terminal_mismatch_withdraws_the_held_credential() {
        let ca = TestCa::new();
        // Near-expiry certificate so the second token() forces the re-mint
        // that hits the terminal mismatch.
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, Some(now_unix() + 60))),
            Err(true),
        ]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert_eq!(s.token().await.unwrap(), "");
        assert!(
            s.cert_resolver().current().is_some(),
            "armed before terminal"
        );

        let err = s.token().await.unwrap_err();
        assert!(
            err.contains("possible MITM"),
            "terminal error surfaces: {err}"
        );
        assert!(
            s.cert_resolver().current().is_none(),
            "the armed identity is withdrawn on terminal"
        );
        assert!(!s.holds_certificate());
        assert_eq!(s.surviving_bearer(), None);
    }

    #[tokio::test]
    async fn source_pin_mismatch_is_terminal_no_retry() {
        let fm = FakeMinter::new(vec![Err(true)]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert!(s.token().await.is_err());
        assert!(s.token().await.is_err()); // persists, no re-mint
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn source_re_mints_near_expiry() {
        // Within the refresh window → the next obtain re-mints.
        let near = Minted::token(
            "near",
            Some(now_unix() + (TOKEN_REFRESH_WINDOW.as_secs() as i64) / 2),
        );
        let fm = FakeMinter::new(vec![Ok(near), Ok(minted("fresh"))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert_eq!(s.token().await.unwrap(), "near");
        assert_eq!(s.token().await.unwrap(), "fresh");
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn source_proactive_refresh() {
        let fm = FakeMinter::new(vec![Ok(minted("first")), Ok(minted("second"))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert_eq!(s.token().await.unwrap(), "first");
        s.refresh().await; // proactive re-mint even though the cached token is valid
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
        assert_eq!(s.token().await.unwrap(), "second");
    }

    /// A `Minter` that holds every mint open on a barrier so overlap can be forced.
    struct GatedMinter {
        calls: AtomicUsize,
        release: tokio::sync::Semaphore,
    }
    #[async_trait::async_trait]
    impl Minter for GatedMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let _p = self.release.acquire().await.unwrap();
            Ok(minted("tok"))
        }
    }

    #[tokio::test]
    async fn source_single_flight_collapses_concurrent_callers() {
        let gm = Arc::new(GatedMinter {
            calls: AtomicUsize::new(0),
            release: tokio::sync::Semaphore::new(0),
        });
        let s = new_credential_source_with_store(
            gm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );

        let mut handles = Vec::new();
        for _ in 0..8 {
            let s = s.clone();
            handles.push(tokio::spawn(async move { s.token().await }));
        }
        // Let all 8 join the single in-flight mint, then release it.
        tokio::time::sleep(Duration::from_millis(50)).await;
        gm.release.add_permits(8);
        for h in handles {
            assert_eq!(h.await.unwrap().unwrap(), "tok");
        }
        assert_eq!(
            gm.calls.load(Ordering::SeqCst),
            1,
            "single-flight → one mint"
        );
    }

    // ---- refresh-delay math (no Go test; panel F6) ----------------------------------

    #[test]
    fn base_refresh_delay_branches() {
        let now = 1_000_000;
        // No token minted → default (1h).
        assert_eq!(base_refresh_delay(None, now), DEFAULT_REFRESH_DELAY);
        // Valid token → remaining/2.
        assert_eq!(
            base_refresh_delay(Some(now + 4 * 3600), now),
            Duration::from_secs(2 * 3600)
        );
        // Expired → min.
        assert_eq!(base_refresh_delay(Some(now - 10), now), MIN_REFRESH_DELAY);
    }

    #[test]
    fn jitter_and_clamp_bounds() {
        // A 1h base spread ±25% stays within [45m, 75m] and inside the global clamp.
        for frac in [-1.0, -0.5, 0.0, 0.5, 0.999] {
            let d = apply_jitter_and_clamp(Duration::from_secs(3600), frac);
            assert!(d >= MIN_REFRESH_DELAY && d <= MAX_REFRESH_DELAY);
            assert!(d >= Duration::from_secs(45 * 60) && d <= Duration::from_secs(75 * 60));
        }
        // A huge base clamps to the 12h max; a tiny base clamps to the 1m min.
        assert_eq!(
            apply_jitter_and_clamp(Duration::from_secs(48 * 3600), 0.0),
            MAX_REFRESH_DELAY
        );
        assert_eq!(
            apply_jitter_and_clamp(Duration::from_secs(1), -1.0),
            MIN_REFRESH_DELAY
        );
    }

    // ---- mtls: the certificate half of the source (plan 001 D2/D5/D6) ---------------

    /// The user a [`TestCa`] certificate names unless told otherwise.
    const TEST_USER: &str = "pat";

    /// The userId attribute a test certificate carries: a named user (PrintableString),
    /// the implicit user (UTF8String `!implicit`), none (only a server older than 0.10.0
    /// issues that — kept for the refusal tests), or `pat` as an IA5String, which the
    /// reader refuses (a certificate whose user cannot be read).
    #[derive(Clone)]
    pub(crate) enum TestUser {
        Named(&'static str),
        Implicit,
        Unbound,
        Unreadable,
    }

    impl TestUser {
        fn push(&self, params: &mut rcgen::CertificateParams) {
            // userId, 0.9.2342.19200300.100.1.1, in an RDN of its own.
            let oid = rcgen::DnType::CustomDnType(vec![0, 9, 2342, 19_200_300, 100, 1, 1]);
            let value = match self {
                TestUser::Named(name) => {
                    rcgen::DnValue::PrintableString((*name).try_into().unwrap())
                }
                TestUser::Implicit => rcgen::DnValue::Utf8String("!implicit".into()),
                TestUser::Unbound => return,
                TestUser::Unreadable => rcgen::DnValue::Ia5String(TEST_USER.try_into().unwrap()),
            };
            params.distinguished_name.push(oid, value);
        }
    }

    /// A throwaway CA + client certificate, so the source is exercised against REAL
    /// certificate material (the pairing check, the resolver, the store round trip) rather
    /// than strings that happen to look like PEM.
    pub(crate) struct TestCa {
        cert: rcgen::Certificate,
        key: rcgen::KeyPair,
    }

    impl TestCa {
        pub(crate) fn new() -> Self {
            let key = rcgen::KeyPair::generate().unwrap();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Constrained(0));
            params
                .distinguished_name
                .push(rcgen::DnType::CommonName, "shed-ca");
            params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
            let cert = params.self_signed(&key).unwrap();
            Self { cert, key }
        }

        /// Issue a leaf for `key_pkcs8_der`'s public half, the way the server's CA signs a
        /// submitted CSR: the subject is composed here, not requested by the client.
        ///
        /// `not_after` is threaded through because the certificate's OWN validity is what a
        /// rehydrated credential reads its expiry from — the bundle's `expires_at` is gone
        /// once the process restarts.
        ///
        /// The subject names [`TEST_USER`], as a 0.10 server's always names a user.
        fn issue(&self, key_pkcs8_der: &[u8], serial: u64, not_after: Option<i64>) -> String {
            self.issue_for(
                key_pkcs8_der,
                serial,
                not_after,
                &TestUser::Named(TEST_USER),
            )
        }

        /// [`Self::issue`] with a chosen userId attribute.
        fn issue_for(
            &self,
            key_pkcs8_der: &[u8],
            serial: u64,
            not_after: Option<i64>,
            user: &TestUser,
        ) -> String {
            let kp = rcgen::KeyPair::try_from(key_pkcs8_der).unwrap();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params
                .distinguished_name
                .push(rcgen::DnType::CommonName, "SHA256:agent");
            params
                .distinguished_name
                .push(rcgen::DnType::OrganizationalUnitName, SCOPE_CREDENTIALS);
            user.push(&mut params);
            params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
            params.serial_number = Some(rcgen::SerialNumber::from(serial));
            if let Some(exp) = not_after {
                params.not_after = (UNIX_EPOCH + Duration::from_secs(exp.max(0) as u64)).into();
            }
            let leaf = params.signed_by(&kp, &self.cert, &self.key).unwrap();
            pem_encode(shed_core::csr::PEM_LABEL_CERTIFICATE, leaf.der())
        }
    }

    /// A mint carrying a freshly issued certificate for a fresh keypair — what an
    /// `auth.mode: mtls` server returns. The leaf's validity matches the bundle expiry, as
    /// a real issuance's does.
    fn minted_cert(ca: &TestCa, serial: u64, expiry: Option<i64>) -> Minted {
        minted_cert_for(ca, serial, expiry, &TestUser::Named(TEST_USER))
    }

    /// [`minted_cert`] naming a chosen user.
    pub(crate) fn minted_cert_for(
        ca: &TestCa,
        serial: u64,
        expiry: Option<i64>,
        user: &TestUser,
    ) -> Minted {
        let kp = ClientKeyPair::generate().unwrap();
        let cert_pem = ca.issue_for(kp.key_pkcs8_der(), serial, expiry, user);
        Minted {
            credential: MintedCredential::Certificate(MintedCertificate {
                cert_pem,
                key_pkcs8_der: kp.key_pkcs8_der().to_vec(),
                serial: format!("{serial:x}"),
            }),
            expiry,
        }
    }

    pub(crate) fn tmp_store() -> (tempfile::TempDir, CredStore) {
        let dir = tempfile::tempdir().unwrap();
        let store = CredStore::new(dir.path().join("creds"));
        (dir, store)
    }

    /// The mtls happy path: the mint arms the transport's resolver, and the bearer is
    /// EMPTY so no `Authorization` header is sent alongside the certificate.
    #[tokio::test]
    async fn mtls_mint_arms_the_resolver_and_sends_no_bearer() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, Some(now_unix() + 86_400)))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );

        assert!(s.cert_resolver().current().is_none(), "nothing armed yet");
        assert_eq!(
            s.token().await.unwrap(),
            "",
            "an mtls credential has no bearer to send"
        );
        assert!(
            s.cert_resolver().current().is_some(),
            "the certificate must be armed before the caller's first request"
        );
        assert_eq!(s.credential_serial().as_deref(), Some("a"));

        // The certificate is CACHED like a token: a second call re-serves it.
        assert_eq!(s.token().await.unwrap(), "");
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1, "cached → one mint");
    }

    /// A rotation replaces the armed identity in place. Nothing about the transport is
    /// rebuilt — the resolver handle the client holds is the SAME `Arc`, which is the
    /// whole point of plan 001 D5's adaptive transport.
    #[tokio::test]
    async fn rotation_swaps_the_identity_without_replacing_the_resolver() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, Some(now_unix() + 86_400))),
            Ok(minted_cert(&ca, 0x0b, Some(now_unix() + 86_400))),
        ]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        // The handle a transport would have been built with, captured ONCE.
        let installed = s.cert_resolver();

        s.token().await.unwrap();
        let first = installed.current().expect("serial A armed");
        assert_eq!(s.credential_serial().as_deref(), Some("a"));

        s.refresh().await; // proactive rotation
        let second = installed.current().expect("serial B armed");
        assert_eq!(s.credential_serial().as_deref(), Some("b"));

        assert!(
            !Arc::ptr_eq(&first, &second),
            "the identity must actually have been replaced"
        );
        assert!(
            Arc::ptr_eq(&installed, &s.cert_resolver()),
            "the resolver the client holds must survive the rotation"
        );
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    /// A mode flip in either direction is a pure credential-state change: token → mtls
    /// arms a certificate, mtls → token WITHDRAWS it (so the next handshake stops
    /// presenting a dead identity) and restores the bearer.
    #[tokio::test]
    async fn mode_flip_arms_and_withdraws_the_certificate() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted("tok1")),
            Ok(minted_cert(&ca, 0x0a, Some(now_unix() + 86_400))),
            Ok(minted("tok2")),
        ]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );

        assert_eq!(s.token().await.unwrap(), "tok1");
        assert!(
            s.cert_resolver().current().is_none(),
            "token state: no cert"
        );

        s.refresh().await; // server flipped to mtls
        assert_eq!(s.token().await.unwrap(), "");
        assert!(s.cert_resolver().current().is_some());

        s.refresh().await; // ...and back to token
        assert_eq!(s.token().await.unwrap(), "tok2");
        assert!(
            s.cert_resolver().current().is_none(),
            "a flip back to token must withdraw the certificate"
        );
    }

    /// A certificate that does not belong to the key this attempt generated is refused at
    /// adoption, never armed. Otherwise it would surface much later as an opaque handshake
    /// failure against a server that is working correctly.
    #[tokio::test]
    async fn a_mismatched_certificate_is_refused_not_armed() {
        let ca = TestCa::new();
        let stranger = ClientKeyPair::generate().unwrap();
        let mine = ClientKeyPair::generate().unwrap();
        let bad = Minted {
            credential: MintedCredential::Certificate(MintedCertificate {
                cert_pem: ca.issue(stranger.key_pkcs8_der(), 0x0a, None),
                key_pkcs8_der: mine.key_pkcs8_der().to_vec(),
                serial: "a".into(),
            }),
            expiry: None,
        };
        let fm = FakeMinter::new(vec![Ok(bad)]);
        let s = new_credential_source_with_store(fm, target("s", "", 0), SCOPE_CREDENTIALS, None);

        let err = s.token().await.unwrap_err();
        assert!(err.contains("unusable"), "{err}");
        assert!(s.cert_resolver().current().is_none(), "nothing armed");
    }

    /// Invalidation (a 401) drops the certificate from the transport too — a credential
    /// the server just refused must not keep being presented.
    #[tokio::test]
    async fn invalidate_keeps_the_certificate_and_re_mints() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, Some(now_unix() + 86_400))),
            Ok(minted_cert(&ca, 0x0b, Some(now_unix() + 86_400))),
        ]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );

        s.token().await.unwrap();
        assert!(s.cert_resolver().current().is_some());

        s.invalidate();
        // NOT withdrawn (whole-branch review finding 3): the rejection can be
        // transient, and stripping the identity would guarantee rejection if
        // the replacement mint then failed. The rejected flag forces the next
        // obtain to mint regardless.
        assert!(
            s.cert_resolver().current().is_some(),
            "the surviving certificate stays armed across invalidate"
        );

        s.token().await.unwrap();
        assert_eq!(s.credential_serial().as_deref(), Some("b"), "re-minted");
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    /// A certificate is persisted on mint and REHYDRATED by a fresh source over the same
    /// store — the restart path. The rehydrated source presents the credential on its
    /// first handshake and mints nothing.
    #[tokio::test]
    async fn a_certificate_persists_and_rehydrates_across_a_restart() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let expiry = now_unix() + 86_400;
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, Some(expiry)))]);
        let first = new_credential_source_with_store(
            fm.clone(),
            target("mini3", "", 0),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        first.token().await.unwrap();
        let armed = first.cert_resolver().current().unwrap();

        // Restart: a brand-new source over the same store, with a minter that would PANIC
        // the assertion below if it were consulted.
        let fm2 = FakeMinter::new(vec![Ok(minted("should-not-mint"))]);
        let second = new_credential_source_with_store(
            fm2.clone(),
            target("mini3", "", 0),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        let rehydrated = second
            .cert_resolver()
            .current()
            .expect("the persisted certificate must be armed at construction");
        assert_eq!(
            rehydrated.cert, armed.cert,
            "the same leaf must come back off disk"
        );
        assert_eq!(
            second.token().await.unwrap(),
            "",
            "still an mtls credential (no bearer)"
        );
        assert_eq!(
            fm2.calls.load(Ordering::SeqCst),
            0,
            "a rehydrated credential must not cost an SSH mint"
        );

        // The store carries the pair, both files, in the expected place.
        let (cert_path, key_path) = store.paths("mini3");
        assert!(cert_path.exists() && key_path.exists());
    }

    /// A REHYDRATED credential still knows when it expires — the leaf's `notAfter` is read
    /// off the certificate, so a restart does not reset the re-mint clock. Near expiry it
    /// re-mints exactly like a freshly minted one.
    #[tokio::test]
    async fn a_rehydrated_credential_re_mints_near_expiry() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        // Inside the 2h refresh window.
        let near = now_unix() + (TOKEN_REFRESH_WINDOW.as_secs() as i64) / 2;
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, Some(near)))]);
        new_credential_source_with_store(
            fm,
            target("mini3", "", 0),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        )
        .token()
        .await
        .unwrap();

        let fm2 = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0b, Some(now_unix() + 86_400)))]);
        let second = new_credential_source_with_store(
            fm2.clone(),
            target("mini3", "", 0),
            SCOPE_CREDENTIALS,
            Some(store),
        );
        second.token().await.unwrap();
        assert_eq!(
            fm2.calls.load(Ordering::SeqCst),
            1,
            "a stale rehydrated certificate must re-mint on first use"
        );
        assert_eq!(second.credential_serial().as_deref(), Some("b"));
    }

    /// A half-written pair on disk (the crash-between-renames shape) is "no credential",
    /// not a wedged source: construction succeeds with nothing armed and the first use
    /// enrolls.
    #[tokio::test]
    async fn a_mismatched_stored_pair_is_treated_as_no_credential() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let a = ClientKeyPair::generate().unwrap();
        let b = ClientKeyPair::generate().unwrap();
        store
            .write(
                "mini3",
                &ca.issue(a.key_pkcs8_der(), 0x0a, None),
                &pem_encode(PEM_LABEL_PRIVATE_KEY, b.key_pkcs8_der()),
            )
            .unwrap();

        let fm = FakeMinter::new(vec![Ok(minted("tok1"))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("mini3", "", 0),
            SCOPE_CREDENTIALS,
            Some(store),
        );
        assert!(
            s.cert_resolver().current().is_none(),
            "a mismatched pair must not be armed"
        );
        assert_eq!(s.token().await.unwrap(), "tok1");
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1);
    }

    /// The CONTROL scope is in-memory only: the egress stream's credential is never
    /// written to disk (one certificate carries one scope, and nothing reads a persisted
    /// control credential).
    #[tokio::test]
    async fn the_control_scope_persists_nothing() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, None))]);
        // `new_credential_source` picks the store from the scope; assert the choice.
        let s = new_credential_source(fm, target("mini3", "", 0), SCOPE_CONTROL);
        s.token().await.unwrap();
        assert!(s.cert_resolver().current().is_some());
        assert!(
            !store.paths("mini3").0.exists(),
            "the control scope must not write into a credentials store"
        );
        assert!(
            !CredStore::for_scope(SCOPE_CONTROL)
                .paths("mini3")
                .0
                .exists(),
            "...nor into a control store"
        );
    }

    /// Concurrent callers still collapse onto ONE mint in mtls state, and all of them see
    /// that mint's own result.
    #[tokio::test]
    async fn concurrent_mtls_callers_single_flight() {
        struct GatedCertMinter {
            calls: AtomicUsize,
            release: tokio::sync::Semaphore,
            minted: Mutex<Option<Minted>>,
        }
        #[async_trait::async_trait]
        impl Minter for GatedCertMinter {
            async fn mint(&self, _t: &ServerTarget, _s: &str) -> Result<Minted, MinterError> {
                self.calls.fetch_add(1, Ordering::SeqCst);
                let _p = self.release.acquire().await.unwrap();
                Ok(self.minted.lock().unwrap().clone().unwrap())
            }
        }
        let ca = TestCa::new();
        let gm = Arc::new(GatedCertMinter {
            calls: AtomicUsize::new(0),
            release: tokio::sync::Semaphore::new(0),
            minted: Mutex::new(Some(minted_cert(&ca, 0x0a, Some(now_unix() + 86_400)))),
        });
        let s = new_credential_source_with_store(
            gm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );

        let mut handles = Vec::new();
        for _ in 0..8 {
            let s = s.clone();
            handles.push(tokio::spawn(async move { s.token().await }));
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        gm.release.add_permits(8);
        for h in handles {
            assert_eq!(h.await.unwrap().unwrap(), "");
        }
        assert_eq!(
            gm.calls.load(Ordering::SeqCst),
            1,
            "single-flight → one mint"
        );
        assert!(s.cert_resolver().current().is_some());
    }

    /// The host-key mismatch stays TERMINAL in mtls state too: no retry, no certificate,
    /// and the latched message on every later call.
    #[tokio::test]
    async fn host_key_mismatch_is_still_terminal_with_certificates() {
        let fm = FakeMinter::new(vec![Err(true)]);
        let s = new_credential_source_with_store(
            fm.clone(),
            target("s", "", 0),
            SCOPE_CREDENTIALS,
            None,
        );
        assert!(s.token().await.is_err());
        assert!(s.force_token_with_expiry().await.is_err());
        assert!(s.cert_resolver().current().is_none());
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1, "never retried");
    }

    /// `token.get` cannot express a certificate, so `force_token_with_expiry` refuses one
    /// explicitly instead of handing back an empty string that would 401 much later. This
    /// is the STALE-entry path (the recorded mode said token; the server had flipped).
    #[tokio::test]
    async fn force_token_refuses_a_certificate() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, None))]);
        let s =
            new_credential_source_with_store(fm, target("prod", "", 0), SCOPE_CREDENTIALS, None);
        let err = s.force_token_with_expiry().await.unwrap_err();
        assert!(err.contains("issues client certificates"), "{err}");
        assert!(err.contains("prod"), "{err}");
    }

    /// The minter sends a CSR on EVERY mint — that is what makes a server-side mode flip a
    /// non-event — and a token-mode server's answer is unaffected by it.
    #[tokio::test]
    async fn every_mint_carries_a_csr() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let runner = FakeRunner::ok(bundle("tok"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        let minted = m
            .mint(&target("s", "mini3", 2222), SCOPE_CREDENTIALS)
            .await
            .unwrap();
        assert_eq!(minted.bearer(), Some("tok"));

        let seen = runner.last_params.lock().unwrap().clone().unwrap();
        assert!(!seen.csr.is_empty(), "a CSR must be submitted");
        crate::bootstrap::validate(&seen).expect("the composed params must be argv-safe");
        // Two mints use two DIFFERENT keys: key lifetime == certificate lifetime.
        let runner2 = FakeRunner::ok(bundle("tok"));
        let m2 = CredentialMinter::with_runner(&path, runner2.clone());
        m2.mint(&target("s", "mini3", 2222), SCOPE_CREDENTIALS)
            .await
            .unwrap();
        assert_ne!(
            seen.csr,
            runner2.last_params.lock().unwrap().clone().unwrap().csr
        );
    }

    /// An mtls bundle is turned into a certificate credential carrying THIS attempt's key.
    #[tokio::test]
    async fn an_mtls_bundle_becomes_a_certificate_credential() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let mut b = bundle("");
        b.auth_mode = "mtls".into();
        b.https_port = 8443;
        b.http_port = 0;
        b.tls_cert_fingerprint = "sha256:abc".into();
        b.client_cert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n".into();
        b.cert_serial = "0a1b".into();
        let runner = FakeRunner::ok(b);
        let m = CredentialMinter::with_runner(&path, runner.clone());

        let minted = m
            .mint(&target("s", "mini3", 2222), SCOPE_CREDENTIALS)
            .await
            .unwrap();
        assert_eq!(minted.bearer(), None);
        let MintedCredential::Certificate(c) = &minted.credential else {
            panic!("expected a certificate credential");
        };
        assert_eq!(c.serial, "0a1b");
        // The key returned is the one whose CSR was submitted.
        let sent = runner.last_params.lock().unwrap().clone().unwrap();
        let kp_der_b64 = shed_core::csr::pem_encode(PEM_LABEL_PRIVATE_KEY, &c.key_pkcs8_der);
        assert!(kp_der_b64.contains("BEGIN PRIVATE KEY"));
        assert!(!sent.csr.is_empty());
        // Debug must never render the private half.
        let rendered = format!("{minted:?}");
        assert!(rendered.contains("<redacted>"), "{rendered}");
    }

    /// The relay path submits the CALLER's CSR and generates no key of its own.
    #[tokio::test]
    async fn mint_relayed_passes_the_csr_through() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let mut b = bundle("");
        b.auth_mode = "mtls".into();
        b.https_port = 8443;
        b.http_port = 0;
        b.tls_cert_fingerprint = "sha256:abc".into();
        b.client_cert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n".into();
        b.cert_serial = "0a1b".into();
        let runner = FakeRunner::ok(b);
        let m = CredentialMinter::with_runner(&path, runner.clone());

        let got = m
            .mint_relayed(&target("s", "mini3", 2222), SCOPE_CONTROL, "QUJDREVG==")
            .await
            .unwrap();
        assert!(got.is_mtls);
        assert_eq!(got.cert_serial, "0a1b");
        let sent = runner.last_params.lock().unwrap().clone().unwrap();
        assert_eq!(sent.csr, "QUJDREVG==", "verbatim");
        assert_eq!(sent.scope, SCOPE_CONTROL);

        // An EMPTY relayed CSR sends no `csr=` argument at all (the legacy request).
        let runner2 = FakeRunner::ok(bundle("tok"));
        let m2 = CredentialMinter::with_runner(&path, runner2.clone());
        let got = m2
            .mint_relayed(&target("s", "mini3", 2222), SCOPE_CONTROL, "")
            .await
            .unwrap();
        assert!(!got.is_mtls);
        assert_eq!(got.token, "tok");
        let sent2 = runner2.last_params.lock().unwrap().clone().unwrap();
        assert!(sent2.csr.is_empty());
        assert!(!crate::bootstrap::ssh_args(&sent2)
            .iter()
            .any(|a| a.starts_with("csr=")));
    }

    /// The entry's `identity_file` reaches ssh on both mint paths (plan 037 A8): the
    /// source's own mint and the relay hand it to the runner, whose argv then carries
    /// the key; an entry with none hands none, and its argv is as before.
    #[tokio::test]
    async fn both_mint_paths_hand_the_runner_the_entrys_identity_file() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let keyed = ServerTarget {
            identity_file: "/home/pat/.ssh/id_work".into(),
            ..target("s", "mini3", 2222)
        };
        let runner = FakeRunner::ok(bundle("tok"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        m.mint(&keyed, SCOPE_CREDENTIALS).await.unwrap();
        let sent = runner.last_params.lock().unwrap().clone().unwrap();
        assert_eq!(sent.identity_file, "/home/pat/.ssh/id_work");
        assert!(crate::bootstrap::ssh_args(&sent)
            .windows(2)
            .any(|w| w == ["-i", "/home/pat/.ssh/id_work"]));

        let runner = FakeRunner::ok(bundle("tok"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        m.mint_relayed(&keyed, SCOPE_CONTROL, "").await.unwrap();
        let sent = runner.last_params.lock().unwrap().clone().unwrap();
        assert_eq!(sent.identity_file, "/home/pat/.ssh/id_work");

        let runner = FakeRunner::ok(bundle("tok"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        m.mint(&target("s", "mini3", 2222), SCOPE_CREDENTIALS)
            .await
            .unwrap();
        let sent = runner.last_params.lock().unwrap().clone().unwrap();
        assert!(sent.identity_file.is_empty());
        assert!(!crate::bootstrap::ssh_args(&sent).iter().any(|a| a == "-i"));
    }

    // ---- the held user (plan 037 §3.2.2) ----------------------------------------------

    pub(crate) const MISMATCH: &str = "server \"mini3\": the server issued a certificate for user \
        \"admin\", but this entry is held to \"pat\"; it was not used. Fix it with the shed \
        CLI (shed server update mini3 --identity-file /path/to/key, or --refetch \
        --allow-user-change), then restart the app.";
    const NO_USER: &str = "server \"mini3\": the server issued a certificate that names no \
        user (a shed-server older than 0.10.0); it was not used. Upgrade the server, then \
        restart the app.";

    fn day() -> Option<i64> {
        Some(now_unix() + 86_400)
    }

    fn held_target(user: &str) -> ServerTarget {
        ServerTarget {
            user: user.into(),
            ..target("mini3", "", 0)
        }
    }

    /// The bytes of the store's pair for `mini3` (`None` for a missing file).
    fn stored(store: &CredStore) -> (Option<Vec<u8>>, Option<Vec<u8>>) {
        let (cert, key) = store.paths("mini3");
        (std::fs::read(cert).ok(), std::fs::read(key).ok())
    }

    fn held_user_of(s: &CredentialSource) -> Option<String> {
        s.state.lock().unwrap().held_user.clone()
    }

    /// A stored pair for `mini3` whose certificate names `user` (the key matches, so
    /// only the user decides whether it is presented).
    fn store_pair(store: &CredStore, ca: &TestCa, user: &TestUser) {
        let kp = ClientKeyPair::generate().unwrap();
        store
            .write(
                "mini3",
                &ca.issue_for(kp.key_pkcs8_der(), 0x0c, day(), user),
                &pem_encode(PEM_LABEL_PRIVATE_KEY, kp.key_pkcs8_der()),
            )
            .unwrap();
    }

    /// A certificate for another named user, and one that names no user, are refused:
    /// nothing is written to the store and the held credential stays armed.
    #[tokio::test]
    async fn a_refused_mint_writes_nothing_and_keeps_the_held_credential() {
        for (refused, want) in [
            (TestUser::Named("admin"), MISMATCH),
            (TestUser::Unbound, NO_USER),
        ] {
            let ca = TestCa::new();
            let (_d, store) = tmp_store();
            let fm = FakeMinter::new(vec![
                Ok(minted_cert(&ca, 0x0a, day())),
                Ok(minted_cert_for(&ca, 0x0b, day(), &refused)),
                Ok(minted_cert(&ca, 0x0c, day())),
            ]);
            let clock = MonoClock::new();
            let s = clock.on(new_credential_source_with_store(
                fm.clone(),
                held_target("pat"),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
            ));
            s.token().await.unwrap();
            let armed = s.cert_resolver().current().unwrap();
            let before = stored(&store);
            assert!(before.0.is_some(), "pat's certificate was stored");

            s.invalidate(); // force the replacement mint
            let err = s.token_for_request().await.unwrap_err();
            assert_eq!(
                err,
                ObtainError::held_user_refusal(want.to_string()),
                "a refusal, the held-user rule's"
            );
            assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
            assert_eq!(stored(&store), before, "nothing written to the store");
            assert!(
                Arc::ptr_eq(&armed, &s.cert_resolver().current().unwrap()),
                "the held credential stays armed"
            );
            assert!(s.holds_certificate());
            assert_eq!(s.credential_serial().as_deref(), Some("a"));
            assert_eq!(held_user_of(&s).as_deref(), Some("pat"));

            // The next mint, once the refusal's wait has passed, is pat's again: adopted
            // and stored.
            clock.advance(REFUSAL_BACKOFF_FLOOR);
            s.token().await.unwrap();
            assert_eq!(s.credential_serial().as_deref(), Some("c"));
            assert_ne!(stored(&store), before);
        }
    }

    /// With nothing held, a certificate that names no user is refused before anything is
    /// stored or armed, whether or not a user is held.
    #[tokio::test]
    async fn a_first_mint_that_names_no_user_stores_nothing() {
        for user in ["", "pat"] {
            let ca = TestCa::new();
            let (_d, store) = tmp_store();
            let fm = FakeMinter::new(vec![Ok(minted_cert_for(
                &ca,
                0x0a,
                day(),
                &TestUser::Unbound,
            ))]);
            let s = new_credential_source_with_store(
                fm,
                held_target(user),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
            );
            assert_eq!(s.token().await.unwrap_err(), NO_USER);
            assert_eq!(stored(&store), (None, None));
            assert!(s.cert_resolver().current().is_none());
        }
    }

    /// An unreadable user is refused too (a fixed reason, no store write).
    #[tokio::test]
    async fn a_mint_whose_user_cannot_be_read_is_refused() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let fm = FakeMinter::new(vec![Ok(minted_cert_for(
            &ca,
            0x0a,
            day(),
            &TestUser::Unreadable,
        ))]);
        let s = new_credential_source_with_store(
            fm,
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        let err = s.token_for_request().await.unwrap_err();
        assert!(
            err.message.contains("cannot be read for its user"),
            "{err:?}"
        );
        // A refusal, but not the held-user rule's: a request is still sent as before.
        assert!(err.refused && !err.held_user, "{err:?}");
        assert_eq!(stored(&store), (None, None));
        assert!(s.cert_resolver().current().is_none());
    }

    /// No `user` in config: the first named certificate is held in memory, and a later
    /// one for another user is refused; an implicit one is adopted and sets nothing.
    #[tokio::test]
    async fn the_first_named_certificate_is_held_and_implicit_sets_nothing() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert_for(&ca, 0x0a, day(), &TestUser::Implicit)),
            Ok(minted_cert(&ca, 0x0b, day())),
            Ok(minted_cert_for(&ca, 0x0c, day(), &TestUser::Named("admin"))),
        ]);
        let s = new_credential_source_with_store(fm, held_target(""), SCOPE_CREDENTIALS, None);
        s.token().await.unwrap();
        assert_eq!(held_user_of(&s), None, "implicit sets no held user");
        s.refresh().await;
        assert_eq!(s.credential_serial().as_deref(), Some("b"));
        assert_eq!(held_user_of(&s).as_deref(), Some("pat"));
        s.invalidate();
        assert_eq!(s.token().await.unwrap_err(), MISMATCH);
        assert_eq!(s.credential_serial().as_deref(), Some("b"));
    }

    /// A stored named certificate holds its user across a restart when the config has
    /// none: a fresh source over the store refuses another user's certificate.
    #[tokio::test]
    async fn the_hydrated_user_is_held_when_config_has_none() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        store_pair(&store, &ca, &TestUser::Named("pat"));
        let fm = FakeMinter::new(vec![Ok(minted_cert_for(
            &ca,
            0x0b,
            day(),
            &TestUser::Named("admin"),
        ))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        assert!(
            s.cert_resolver().current().is_some(),
            "pat's stored certificate presented"
        );
        assert_eq!(s.hydration_note(), None);
        assert_eq!(held_user_of(&s).as_deref(), Some("pat"));
        let before = stored(&store);
        s.invalidate();
        assert_eq!(s.token().await.unwrap_err(), MISMATCH);
        assert_eq!(stored(&store), before);
    }

    /// A stored certificate whose user cannot be read, that names no user, or that names
    /// another user than the config's `user` is not presented: the source starts with no
    /// credential, mints, and says why. The control: the held user's own is presented.
    #[tokio::test]
    async fn a_stored_certificate_for_the_wrong_user_is_not_presented() {
        for (stored_user, why) in [
            (TestUser::Unreadable, "its user cannot be read"),
            (TestUser::Unbound, "it names no user"),
            (
                TestUser::Named("admin"),
                "it is for user \"admin\" but this entry is held to \"pat\"",
            ),
        ] {
            let ca = TestCa::new();
            let (_d, store) = tmp_store();
            store_pair(&store, &ca, &stored_user);
            let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0b, day()))]);
            let s = new_credential_source_with_store(
                fm.clone(),
                held_target("pat"),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
            );
            assert!(
                s.cert_resolver().current().is_none(),
                "{why}: not presented"
            );
            assert!(!s.holds_certificate());
            let note = s.hydration_note().expect("a note says why");
            assert!(note.contains(why), "{note}");
            assert!(
                note.starts_with("server \"mini3\": the stored credentials certificate"),
                "{note}"
            );
            // It mints instead, and pat's new certificate replaces the stored one.
            s.token().await.unwrap();
            assert_eq!(fm.calls.load(Ordering::SeqCst), 1);
            assert_eq!(s.credential_serial().as_deref(), Some("b"));
        }

        // The control: a stored certificate for the held user is presented.
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        store_pair(&store, &ca, &TestUser::Named("pat"));
        let fm = FakeMinter::new(vec![Ok(minted("unused"))]);
        let s = new_credential_source_with_store(
            fm.clone(),
            held_target("pat"),
            SCOPE_CREDENTIALS,
            Some(store),
        );
        assert!(s.cert_resolver().current().is_some());
        assert_eq!(s.hydration_note(), None);
        s.token().await.unwrap();
        assert_eq!(
            fm.calls.load(Ordering::SeqCst),
            0,
            "presented, not re-minted"
        );
    }

    /// Named, then implicit, then a restart, then a different named user: refused.
    /// While pat is held the implicit certificate is used but not stored, so the restart
    /// still reads pat's certificate and still holds pat.
    #[tokio::test]
    async fn named_then_implicit_then_restart_then_another_named_is_refused() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, day())),
            Ok(minted_cert_for(&ca, 0x0b, day(), &TestUser::Implicit)),
        ]);
        let first = new_credential_source_with_store(
            fm,
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        first.token().await.unwrap();
        let pats = stored(&store);
        first.refresh().await;
        assert_eq!(
            first.credential_serial().as_deref(),
            Some("b"),
            "implicit in use"
        );
        assert_eq!(
            stored(&store),
            pats,
            "the implicit certificate was not stored"
        );

        // Restart.
        let fm2 = FakeMinter::new(vec![Ok(minted_cert_for(
            &ca,
            0x0c,
            day(),
            &TestUser::Named("admin"),
        ))]);
        let second = new_credential_source_with_store(
            fm2,
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        assert_eq!(held_user_of(&second).as_deref(), Some("pat"));
        second.invalidate();
        assert_eq!(second.token().await.unwrap_err(), MISMATCH);
        assert_eq!(stored(&store), pats);
    }

    /// With no name held, an implicit certificate is stored as before.
    #[tokio::test]
    async fn an_implicit_certificate_is_stored_when_no_name_is_held() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let fm = FakeMinter::new(vec![Ok(minted_cert_for(
            &ca,
            0x0a,
            day(),
            &TestUser::Implicit,
        ))]);
        let s = new_credential_source_with_store(
            fm,
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        s.token().await.unwrap();
        assert!(stored(&store).0.is_some());
    }

    /// Keeping the held credential after a refusal rests on a positive check: a held
    /// certificate that is not the held user's is withdrawn at that point.
    #[tokio::test]
    async fn a_refusal_keeps_the_held_certificate_only_when_it_is_the_held_users() {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, day())),
            Ok(minted_cert_for(&ca, 0x0b, day(), &TestUser::Named("admin"))),
        ]);
        let s = new_credential_source_with_store(fm, held_target(""), SCOPE_CREDENTIALS, None);
        s.token().await.unwrap();
        // pat's certificate is held, but the held user is now someone else (no path
        // here does this; the check must not assume it cannot happen).
        s.state.lock().unwrap().held_user = Some("bob".into());
        s.invalidate();
        let err = s.token().await.unwrap_err();
        assert!(err.contains("for user \"admin\""), "{err}");
        assert!(s.cert_resolver().current().is_none(), "withdrawn");
        assert!(!s.holds_certificate());
    }

    /// Pause the source's next store write: returns `(paused, resume)`.
    fn pause_next_write(
        s: &CredentialSource,
    ) -> (Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>) {
        let paused = Arc::new(tokio::sync::Notify::new());
        let resume = Arc::new(tokio::sync::Notify::new());
        *s.pause_before_write.lock().unwrap() = Some((paused.clone(), resume.clone()));
        (paused, resume)
    }

    /// The review's interleaving: mint A adopts an implicit certificate (no name held) and
    /// is held up before its store write; meanwhile another caller asks again (invalidate,
    /// then obtain). That caller JOINS mint A — the single flight lasts until A's write
    /// has landed — so no second mint can adopt pat and write before A's implicit write.
    /// The next mint then adopts pat and writes it last, and a restart holds pat.
    #[tokio::test]
    async fn two_adoptions_are_written_in_the_order_they_were_adopted() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert_for(&ca, 0x0a, day(), &TestUser::Implicit)),
            Ok(minted_cert(&ca, 0x0b, day())),
        ]);
        let s = new_credential_source_with_store(
            fm.clone(),
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        let (paused, resume) = pause_next_write(&s);
        let a = tokio::spawn({
            let s = s.clone();
            async move { s.token().await }
        });
        paused.notified().await; // A adopted the implicit certificate; its write waits
        s.invalidate();
        let b = tokio::spawn({
            let s = s.clone();
            async move { s.token().await }
        });
        for _ in 0..50 {
            tokio::task::yield_now().await;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(
            fm.calls.load(Ordering::SeqCst),
            1,
            "no second mint starts while A's write is pending"
        );
        resume.notify_one();
        a.await.unwrap().unwrap();
        b.await.unwrap().unwrap();
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1, "B joined mint A");

        // pat's mint comes after A's write, so it is the one stored.
        s.invalidate();
        s.token().await.unwrap();
        assert_eq!(s.credential_serial().as_deref(), Some("b"));
        let restarted = new_credential_source_with_store(
            FakeMinter::new(vec![Ok(minted_cert_for(
                &ca,
                0x0c,
                day(),
                &TestUser::Named("admin"),
            ))]),
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        assert_eq!(held_user_of(&restarted).as_deref(), Some("pat"));
        restarted.invalidate();
        assert_eq!(restarted.token().await.unwrap_err(), MISMATCH);
    }

    /// A store whose root is a file: every write fails.
    fn unwritable_store() -> (tempfile::TempDir, CredStore) {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("not-a-dir");
        std::fs::write(&root, b"").unwrap();
        (dir, CredStore::new(root))
    }

    /// The certificate that would establish the first named user is adopted only once
    /// it is stored: when the write fails, it is a refusal that says so, nothing is armed
    /// and no user is held (a restart would read the older certificate and forget the
    /// name). For a certificate of the user already held, the write stays best-effort.
    #[tokio::test]
    async fn the_first_named_user_is_held_only_once_its_certificate_is_stored() {
        let ca = TestCa::new();
        let (_d, store) = unwritable_store();
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, day()))]);
        let s = new_credential_source_with_store(
            fm,
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        let err = s.token_for_request().await.unwrap_err();
        assert!(err.refused, "{err:?}");
        assert!(!err.held_user, "not the held-user rule's refusal: {err:?}");
        assert!(
            err.message.starts_with(
                "server \"mini3\": the server issued a certificate for user \"pat\", the first \
                 user this entry would be held to, but it could not be stored ("
            ),
            "{}",
            err.message
        );
        assert_eq!(held_user_of(&s), None, "no user held");
        assert!(s.cert_resolver().current().is_none(), "nothing armed");
        assert!(!s.holds_certificate());

        // pat held by config: the same failed write is best-effort, the certificate used.
        let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0b, day()))]);
        let s = new_credential_source_with_store(
            fm,
            held_target("pat"),
            SCOPE_CREDENTIALS,
            Some(store),
        );
        s.token_for_request().await.unwrap();
        assert!(s.cert_resolver().current().is_some(), "armed");
        assert_eq!(s.credential_serial().as_deref(), Some("b"));
    }

    /// A rotation for the user already held whose store write fails part-way — after the
    /// key and before the certificate (a full disk), or between the two renames — keeps
    /// the stored pair it replaces: loading the store gives pat's previous pair, and a
    /// restart still holds pat and refuses a certificate for admin (the review's case:
    /// the write used to delete the key, so a restart held no user and took admin).
    #[tokio::test]
    async fn a_failed_rotation_write_keeps_the_held_user_across_a_restart() {
        use crate::credstore::WriteStep;
        for step in [WriteStep::CertTemp, WriteStep::CertRename] {
            let ca = TestCa::new();
            let (_d, store) = tmp_store();
            let fm = FakeMinter::new(vec![
                Ok(minted_cert(&ca, 0x0a, day())),
                Ok(minted_cert(&ca, 0x0b, day())),
            ]);
            let s = new_credential_source_with_store(
                fm,
                held_target(""),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
            );
            s.token().await.unwrap(); // pat's first certificate: stored, pat held
            let pats = stored(&store);
            assert_eq!(held_user_of(&s).as_deref(), Some("pat"));

            store.fail_next_write_at(step);
            s.invalidate();
            s.token().await.unwrap(); // the rotation: used, its write best-effort
            assert_eq!(s.credential_serial().as_deref(), Some("b"), "{step:?}");
            assert_eq!(stored(&store), pats, "{step:?}: the previous pair stays");
            let loaded = store
                .load("mini3")
                .unwrap()
                .expect("the previous pair loads");
            assert_eq!(
                shed_core::certuser::cert_user_pem(&loaded.cert_pem).unwrap(),
                CertUser::Named("pat".into()),
                "{step:?}"
            );

            // Restart, then a certificate for another named user: refused.
            let restarted = new_credential_source_with_store(
                FakeMinter::new(vec![Ok(minted_cert_for(
                    &ca,
                    0x0c,
                    day(),
                    &TestUser::Named("admin"),
                ))]),
                held_target(""),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
            );
            assert_eq!(held_user_of(&restarted).as_deref(), Some("pat"), "{step:?}");
            assert!(restarted.cert_resolver().current().is_some(), "{step:?}");
            restarted.invalidate();
            assert_eq!(restarted.token().await.unwrap_err(), MISMATCH, "{step:?}");
            assert_eq!(stored(&store), pats, "{step:?}");
        }
    }

    /// A source whose watcher group was cancelled never writes to the store, even for a
    /// mint that started before the cancel; it still answers its caller.
    #[tokio::test]
    async fn a_cancelled_groups_source_writes_nothing() {
        for user in ["", "pat"] {
            let ca = TestCa::new();
            let (_d, store) = tmp_store();
            let (cancel, group) = watch::channel(false);
            let fm = FakeMinter::new(vec![Ok(minted_cert(&ca, 0x0a, day()))]);
            let s = new_group_credential_source_with_store(
                fm,
                held_target(user),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
                group,
            );
            let (paused, resume) = pause_next_write(&s);
            let t = tokio::spawn({
                let s = s.clone();
                async move { s.token_for_request().await }
            });
            paused.notified().await;
            cancel.send(true).unwrap();
            resume.notify_one();
            let got = t.await.unwrap();
            assert_eq!(
                stored(&store),
                (None, None),
                "user {user:?}: nothing written"
            );
            if user.is_empty() {
                // It would have established pat: not adopted.
                let err = got.unwrap_err();
                assert!(
                    err.message.contains("watcher was stopped or replaced"),
                    "{err:?}"
                );
            } else {
                got.unwrap();
            }
        }
    }

    /// While pat is held by config alone (nothing stored), an implicit certificate is
    /// used but not stored either.
    #[tokio::test]
    async fn an_implicit_certificate_is_not_stored_under_a_config_hold() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let fm = FakeMinter::new(vec![Ok(minted_cert_for(
            &ca,
            0x0a,
            day(),
            &TestUser::Implicit,
        ))]);
        let s = new_credential_source_with_store(
            fm,
            held_target("pat"),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        s.token().await.unwrap();
        assert!(s.holds_certificate(), "used");
        assert_eq!(stored(&store), (None, None), "not stored");
    }

    /// An EXPIRED stored certificate for pat still holds pat (expiry must not release the
    /// identity hold): it is armed, a failed re-mint leaves it armed, and a certificate for
    /// another user is still refused.
    #[tokio::test]
    async fn an_expired_stored_certificate_still_holds_its_user() {
        let ca = TestCa::new();
        let (_d, store) = tmp_store();
        let kp = ClientKeyPair::generate().unwrap();
        store
            .write(
                "mini3",
                &ca.issue_for(
                    kp.key_pkcs8_der(),
                    0x0c,
                    Some(now_unix() - 60),
                    &TestUser::Named("pat"),
                ),
                &pem_encode(PEM_LABEL_PRIVATE_KEY, kp.key_pkcs8_der()),
            )
            .unwrap();
        let fm = FakeMinter::new(vec![
            Err(false),
            Ok(minted_cert_for(&ca, 0x0d, day(), &TestUser::Named("admin"))),
        ]);
        let s = new_credential_source_with_store(
            fm,
            held_target(""),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        );
        assert_eq!(held_user_of(&s).as_deref(), Some("pat"));
        assert!(s.would_mint(), "stale, so it mints");
        let armed = s.cert_resolver().current().expect("armed");
        assert_eq!(s.token().await.unwrap_err(), "transient");
        assert!(Arc::ptr_eq(&armed, &s.cert_resolver().current().unwrap()));
        assert_eq!(s.token().await.unwrap_err(), MISMATCH);
        assert_eq!(held_user_of(&s).as_deref(), Some("pat"));
        // Once the held-user refusal stands, the expired certificate is not presented
        // (plan 037 A25); the name stays held.
        assert!(!s.holds_certificate());
        assert!(s.cert_resolver().current().is_none());
    }

    /// The held-user refusal's wait (plan 037 A25): a source holding pat's certificate,
    /// `admin`'s issued from the next mint on.
    fn refusing_source(
        first_expiry: Option<i64>,
        then: &TestUser,
    ) -> (Arc<FakeMinter>, Arc<CredentialSource>, MonoClock) {
        let ca = TestCa::new();
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, first_expiry)),
            Ok(minted_cert_for(&ca, 0x0b, day(), then)),
        ]);
        let clock = MonoClock::new();
        let s = clock.on(new_credential_source_with_store(
            fm.clone(),
            held_target("pat"),
            SCOPE_CREDENTIALS,
            None,
        ));
        (fm, s, clock)
    }

    fn mints(fm: &FakeMinter) -> usize {
        fm.calls.load(Ordering::SeqCst)
    }

    /// While the held certificate is stale (inside its refresh window), N requests in a
    /// row make ONE mint inside the first wait, not N: each gets the refusal, and pat's
    /// certificate stays armed for the request to go out with.
    #[tokio::test]
    async fn a_stale_held_certificate_mints_once_per_refusal_wait() {
        const N: usize = 10;
        let (fm, s, _clock) = refusing_source(Some(now_unix() + 60), &TestUser::Named("admin"));
        s.token().await.unwrap(); // pat's, already stale
        let armed = s.cert_resolver().current().unwrap();
        assert_eq!(mints(&fm), 1);
        for i in 0..N {
            let err = s.token_for_request().await.unwrap_err();
            assert_eq!(
                err,
                ObtainError::held_user_refusal(MISMATCH.to_string()),
                "{i}"
            );
            assert!(s.holds_certificate(), "{i}: pat's certificate is presented");
            assert!(Arc::ptr_eq(&armed, &s.cert_resolver().current().unwrap()));
            assert!(!s.would_mint(), "{i}: no mint while the wait runs");
        }
        assert_eq!(
            mints(&fm),
            2,
            "one mint for {N} requests inside the first wait"
        );
    }

    /// The wait doubles per consecutive refusal from 1 second to 30, measured on the
    /// monotonic clock: a millisecond short of it, no mint; at it, one.
    #[tokio::test]
    async fn the_refusal_wait_doubles_to_thirty_seconds() {
        let (fm, s, clock) = refusing_source(Some(now_unix() + 60), &TestUser::Named("admin"));
        s.token().await.unwrap();
        s.token_for_request().await.unwrap_err(); // the first refusal
        assert_eq!(mints(&fm), 2);
        let ms = Duration::from_millis(1);
        let mut want = 2;
        for wait in [1, 2, 4, 8, 16, 30, 30, 30].map(Duration::from_secs) {
            clock.advance(wait - ms);
            assert!(!s.would_mint());
            s.token_for_request().await.unwrap_err();
            assert_eq!(mints(&fm), want, "{wait:?} not yet passed");
            clock.advance(ms);
            assert!(s.would_mint());
            s.token_for_request().await.unwrap_err();
            want += 1;
            assert_eq!(mints(&fm), want, "{wait:?} passed: one mint");
        }
        assert_eq!(
            s.state.lock().unwrap().refusal_backoff,
            Some(REFUSAL_BACKOFF_CEILING)
        );
    }

    /// An adopted credential resets the wait: the next refusal waits the floor again.
    #[tokio::test]
    async fn an_adoption_resets_the_refusal_wait() {
        let ca = TestCa::new();
        let stale = || Some(now_unix() + 60);
        let fm = FakeMinter::new(vec![
            Ok(minted_cert(&ca, 0x0a, stale())),
            Ok(minted_cert_for(&ca, 0x0b, day(), &TestUser::Named("admin"))),
            Ok(minted_cert_for(&ca, 0x0c, day(), &TestUser::Named("admin"))),
            Ok(minted_cert(&ca, 0x0d, stale())),
            Ok(minted_cert_for(&ca, 0x0e, day(), &TestUser::Named("admin"))),
        ]);
        let clock = MonoClock::new();
        let s = clock.on(new_credential_source_with_store(
            fm.clone(),
            held_target("pat"),
            SCOPE_CREDENTIALS,
            None,
        ));
        s.token().await.unwrap();
        s.token().await.unwrap_err(); // refused: waits 1 s
        clock.advance(Duration::from_secs(1));
        s.token().await.unwrap_err(); // refused again: waits 2 s
        clock.advance(Duration::from_secs(2));
        s.token().await.unwrap(); // pat's again: adopted
        assert_eq!(s.credential_serial().as_deref(), Some("d"));
        s.token().await.unwrap_err(); // refused: the floor again, not 4 s
        assert_eq!(mints(&fm), 5);
        clock.advance(REFUSAL_BACKOFF_FLOOR - Duration::from_millis(1));
        s.token().await.unwrap_err();
        assert_eq!(mints(&fm), 5);
        clock.advance(Duration::from_millis(1));
        s.token().await.unwrap_err();
        assert_eq!(mints(&fm), 6);
    }

    /// A held certificate the server rejected (a 401) waits the same, for both arms of
    /// the rule: a certificate for another user, and one that names no user.
    #[tokio::test]
    async fn a_rejected_held_certificate_waits_the_same() {
        for (then, want) in [
            (TestUser::Named("admin"), MISMATCH),
            (TestUser::Unbound, NO_USER),
        ] {
            let (fm, s, clock) = refusing_source(day(), &then);
            s.token().await.unwrap();
            let armed = s.cert_resolver().current().unwrap();
            s.invalidate();
            for _ in 0..5 {
                let err = s.token_for_request().await.unwrap_err();
                assert_eq!(err, ObtainError::held_user_refusal(want.to_string()));
                assert!(Arc::ptr_eq(&armed, &s.cert_resolver().current().unwrap()));
            }
            assert_eq!(mints(&fm), 2, "one mint inside the first wait");
            clock.advance(REFUSAL_BACKOFF_FLOOR);
            s.token_for_request().await.unwrap_err();
            assert_eq!(mints(&fm), 3, "one more once it has passed");
        }
    }

    /// Every other failure keeps its behaviour: a minter that fails, and a certificate
    /// whose user cannot be read, are tried again on every call, with no wait.
    #[tokio::test]
    async fn another_failure_starts_no_refusal_wait() {
        let ca = TestCa::new();
        let cases: [Result<Minted, bool>; 2] = [
            Err(false),
            Ok(minted_cert_for(&ca, 0x0b, day(), &TestUser::Unreadable)),
        ];
        for then in cases {
            let fm = FakeMinter::new(vec![
                Ok(minted_cert(&ca, 0x0a, Some(now_unix() + 60))),
                then,
            ]);
            let s = MonoClock::new().on(new_credential_source_with_store(
                fm.clone(),
                held_target("pat"),
                SCOPE_CREDENTIALS,
                None,
            ));
            s.token().await.unwrap();
            for i in 0..5 {
                let err = s.token_for_request().await.unwrap_err();
                assert!(!err.held_user, "{err:?}");
                assert!(s.would_mint());
                assert_eq!(mints(&fm), 2 + i, "a mint on every call");
            }
        }
    }

    /// An expired held certificate is not something to present (plan 037 A25): while a
    /// held-user refusal stands, pat's certificate is presented one second before its
    /// expiry and withdrawn once it has passed, inside the same wait; no mint starts and
    /// the name stays held.
    #[tokio::test]
    async fn an_expired_held_certificate_is_not_presented_during_a_refusal() {
        const EXPIRY: i64 = 4_102_444_800;
        let refusal = || ObtainError::held_user_refusal(MISMATCH.to_string());
        let (fm, s, _mono, wall) = pat_then_admin(SCOPE_CREDENTIALS, EXPIRY);
        s.token().await.unwrap(); // pat's
        let armed = s.cert_resolver().current().unwrap();

        // One second before it expires (inside its refresh window): refused, and pat's
        // certificate is still presented.
        wall.set(EXPIRY - 1);
        assert_eq!(s.token_for_request().await.unwrap_err(), refusal());
        assert_eq!(mints(&fm), 2);
        assert!(s.holds_certificate());
        assert!(Arc::ptr_eq(&armed, &s.cert_resolver().current().unwrap()));

        // Past its expiry, inside the same wait: nothing is presented and the refusal
        // is the answer.
        wall.set(EXPIRY + 1);
        assert!(!s.holds_certificate());
        assert!(s.cert_resolver().current().is_none());
        assert_eq!(s.token_for_request().await.unwrap_err(), refusal());
        assert!(!s.would_mint());
        assert_eq!(mints(&fm), 2, "no mint inside the wait");
        assert_eq!(
            held_user_of(&s).as_deref(),
            Some("pat"),
            "the name stays held"
        );
    }
}
