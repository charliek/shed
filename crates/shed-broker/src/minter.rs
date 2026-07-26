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
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use rustls::sign::CertifiedKey;
use shed_core::csr::{pem_encode, ClientKeyPair, PEM_LABEL_PRIVATE_KEY};
use shed_core::tls::{certified_key_from_pem, ClientCertResolver};
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
    fn new(message: String) -> Self {
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
    /// Latched host-key-mismatch error — once set, the source fails closed forever.
    terminal_err: Option<String>,
    /// The in-flight mint's join handle, if one is running (single-flight).
    inflight: Option<watch::Receiver<Option<Arc<MintOutcome>>>>,
}

/// The result a joined caller reads: `(bearer, expiry)` or the error message. The bearer
/// is `None` in mtls state — the credential is presented by the TLS resolver instead, and
/// the distinction is what lets `token.get` refuse explicitly rather than hand back "".
type MintOutcome = Result<MintOutcomeOk, String>;
/// The success half of a [`MintOutcome`]: `(bearer, expiry)`.
type MintOutcomeOk = (Option<String>, Option<i64>);

/// Why a mint attempt produced no usable credential — kept as a two-armed enum only so the
/// terminal host-key latch stays keyed on the MINTER's error and can never be triggered by
/// an unusable-credential message that happens to read like one.
enum MintFailure {
    Minter(MinterError),
    Unusable(String),
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

/// An `sdk.TokenProvider` backed by the SSH credential minter (mirror
/// `credmint.go:credentialSource`). Construct via [`new_credential_source`]; hold it as
/// `Arc<CredentialSource>` (the obtain methods spawn the mint task, which needs an owned
/// clone).
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
    state: Mutex<SourceState>,
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
    let store = (scope == SCOPE_CREDENTIALS).then(|| CredStore::for_scope(scope));
    new_credential_source_with_store(minter, target, scope, store)
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
    let cert_resolver = Arc::new(ClientCertResolver::new());
    // Rehydrate BEFORE the source is handed out, so the transport built from
    // `cert_resolver()` already presents the persisted identity on its first handshake
    // and the agent skips an SSH mint it does not need on every restart.
    let hydrated = store
        .as_ref()
        .filter(|_| !target.name.is_empty())
        .and_then(|s| s.load(&target.name).ok().flatten());
    let mut state = SourceState {
        cred: None,
        expiry: None,
        terminal_err: None,
        inflight: None,
    };
    if let Some(sc) = hydrated {
        cert_resolver.set(Some(sc.certified.clone()));
        state.expiry = sc.not_after_unix;
        state.cred = Some(Held::Certificate {
            certified: sc.certified,
            cert_pem: sc.cert_pem,
            // The key PEM is not re-read into memory: nothing rewrites a credential it
            // merely loaded — the next rotation persists its own freshly minted pair.
            key_pem: String::new(),
            serial: String::new(),
        });
    }
    Arc::new(CredentialSource {
        minter,
        target,
        scope: scope.to_string(),
        cert_resolver,
        store,
        state: Mutex::new(state),
    })
}

impl CredentialSource {
    /// The server this source mints for (used by the control-token provider to detect an
    /// endpoint change and recreate the source).
    pub fn target(&self) -> &ServerTarget {
        &self.target
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
        match self.obtain(true).await? {
            (Some(tok), expiry) => Ok((tok, expiry)),
            (None, _) => Err(format!(
                "server \"{}\" issues client certificates (auth.mode: mtls); \
                 control-token minting is unavailable",
                self.target.name
            )),
        }
    }

    /// Clear the cached credential so the next obtain re-mints. Called after a 401 (the
    /// egress consumer's `EgressTokenSource::invalidate`, mirror Go
    /// `credentialSource.Invalidate`).
    ///
    /// The certificate is withdrawn from the transport at the same time: a credential the
    /// server just refused must not keep being presented on the next handshake. Existing
    /// pooled connections are unaffected — they already authenticated — which is exactly
    /// the semantics an invalidation wants.
    pub fn invalidate(&self) {
        {
            let mut st = self.state.lock().unwrap();
            st.cred = None;
        }
        self.cert_resolver.set(None);
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
    async fn obtain(self: &Arc<Self>, force: bool) -> Result<MintOutcomeOk, String> {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if let Some(err) = &st.terminal_err {
                return Err(err.clone());
            }
            if force {
                st.cred = None; // force a re-mint; never serve a completed cached credential
            } else if let Some(c) = &st.cred {
                if c.usable() && !stale(st.expiry) {
                    return Ok((c.bearer(), st.expiry));
                }
            }
            self.obtain_locked(&mut st)
        };
        // Await OFF the lock (Go releases s.mu before <-call.done).
        wait_outcome(rx).await
    }

    /// Always start/join a mint (no cache short-circuit and no completed-token clear) —
    /// the proactive-refresh path (mirror `credmint.go:refresh`, which calls
    /// `obtainLocked` directly). Shared by [`Self::refresh`] (tests) and
    /// [`Self::refresh_loop`] (the supervisor spawns the loop per secure server).
    async fn obtain_refresh(self: &Arc<Self>) -> Result<MintOutcomeOk, String> {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if let Some(err) = &st.terminal_err {
                return Err(err.clone());
            }
            self.obtain_locked(&mut st)
        };
        wait_outcome(rx).await
    }

    /// Return the in-flight mint's receiver, starting one (spawned, off the lock) if none
    /// is running so N concurrent callers share ONE mint. Caller holds the state lock.
    fn obtain_locked(
        self: &Arc<Self>,
        st: &mut SourceState,
    ) -> watch::Receiver<Option<Arc<MintOutcome>>> {
        if let Some(rx) = &st.inflight {
            return rx.clone();
        }
        let (tx, rx) = watch::channel(None);
        st.inflight = Some(rx.clone());
        let this = Arc::clone(self);
        tokio::spawn(async move { this.do_mint(tx).await });
        rx
    }

    /// Perform the mint off the state lock, then store the result under it. A host-key
    /// pin mismatch is recorded as terminal (fail closed) so it is never retried.
    ///
    /// A certificate is ARMED on the transport's resolver before this returns — the
    /// caller's very next request may handshake — and persisted afterwards, outside the
    /// state lock (the store takes a cross-process `flock`, which must never be held under
    /// an in-process mutex every other caller is waiting on). A persistence failure is
    /// deliberately NOT a mint failure: the credential is live and usable in memory, and
    /// the only cost of an unwritten file is one extra SSH mint after the next restart.
    async fn do_mint(self: Arc<Self>, tx: watch::Sender<Option<Arc<MintOutcome>>>) {
        let result = self.minter.mint(&self.target, &self.scope).await;
        let mut persist: Option<(String, String)> = None;
        let outcome: MintOutcome = {
            let mut st = self.state.lock().unwrap();
            st.inflight = None;
            match result
                .map_err(MintFailure::Minter)
                .and_then(|minted| adopt(minted, &self.target.name).map_err(MintFailure::Unusable))
            {
                Ok((held, expiry)) => {
                    let bearer = held.bearer();
                    if let Held::Certificate {
                        cert_pem, key_pem, ..
                    } = &held
                    {
                        if !key_pem.is_empty() {
                            persist = Some((cert_pem.clone(), key_pem.clone()));
                        }
                    }
                    // Present (mtls) or withdraw (token) the identity BEFORE returning, so
                    // a mode flip in either direction lands on the transport atomically
                    // with the credential the caller is about to use.
                    self.cert_resolver.set(held.certified());
                    st.expiry = expiry;
                    st.cred = Some(held);
                    Ok((bearer, expiry))
                }
                Err(MintFailure::Unusable(msg)) => Err(msg),
                Err(MintFailure::Minter(e)) if e.is_host_key_mismatch() => {
                    // Go: fmt.Errorf("refusing to broker %q: SSH host key pin mismatch
                    // (possible MITM): %w", name, err). Explicit quoted-name (not {:?}).
                    let msg = format!(
                        "refusing to broker \"{}\": SSH host key pin mismatch (possible MITM): {}",
                        self.target.name,
                        e.message()
                    );
                    st.terminal_err = Some(msg.clone());
                    Err(msg)
                }
                Err(MintFailure::Minter(e)) => Err(e.message().to_string()),
            }
        };
        // Persist OUTSIDE the state lock (the store takes a cross-process flock) and
        // best-effort: an unwritten file costs one extra SSH mint after a restart, which
        // is not worth failing a live credential over.
        if let (Some(store), Some((cert_pem, key_pem))) = (&self.store, persist) {
            let _ = store.write(&self.target.name, &cert_pem, &key_pem);
        }
        // The joined callers read THIS call's outcome (not a re-read of shared state).
        let _ = tx.send(Some(Arc::new(outcome)));
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
            base_refresh_delay(expiry, now_unix()),
            random_jitter_fraction(),
        )
    }
}

/// Whether a cached token with the given expiry is within [`TOKEN_REFRESH_WINDOW`] of
/// expiry (a `None` expiry is a non-expiring token — never stale). Mirror `staleLocked`:
/// `!expiry.IsZero() && now >= expiry - window`.
fn stale(expiry: Option<i64>) -> bool {
    match expiry {
        None => false,
        Some(exp) => now_unix() >= exp - TOKEN_REFRESH_WINDOW.as_secs() as i64,
    }
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
) -> Result<MintOutcomeOk, String> {
    loop {
        {
            let guard = rx.borrow_and_update();
            if let Some(outcome) = guard.as_ref() {
                return (**outcome).clone();
            }
        }
        if rx.changed().await.is_err() {
            return Err("mint task ended unexpectedly".to_string());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bootstrap::{BootstrapError, Bundle};
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
    struct FakeMinter {
        results: std::sync::Mutex<Vec<Result<Minted, bool>>>, // Err(bool) = terminal?
        calls: AtomicUsize,
    }
    impl FakeMinter {
        fn new(results: Vec<Result<Minted, bool>>) -> Arc<Self> {
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

    /// A throwaway CA + client certificate, so the source is exercised against REAL
    /// certificate material (the pairing check, the resolver, the store round trip) rather
    /// than strings that happen to look like PEM.
    struct TestCa {
        cert: rcgen::Certificate,
        key: rcgen::KeyPair,
    }

    impl TestCa {
        fn new() -> Self {
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
        fn issue(&self, key_pkcs8_der: &[u8], serial: u64, not_after: Option<i64>) -> String {
            let kp = rcgen::KeyPair::try_from(key_pkcs8_der).unwrap();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params
                .distinguished_name
                .push(rcgen::DnType::CommonName, "SHA256:agent");
            params
                .distinguished_name
                .push(rcgen::DnType::OrganizationalUnitName, SCOPE_CREDENTIALS);
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
        let kp = ClientKeyPair::generate().unwrap();
        let cert_pem = ca.issue(kp.key_pkcs8_der(), serial, expiry);
        Minted {
            credential: MintedCredential::Certificate(MintedCertificate {
                cert_pem,
                key_pkcs8_der: kp.key_pkcs8_der().to_vec(),
                serial: format!("{serial:x}"),
            }),
            expiry,
        }
    }

    fn tmp_store() -> (tempfile::TempDir, CredStore) {
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
    async fn invalidate_withdraws_the_certificate_and_re_mints() {
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
        assert!(s.cert_resolver().current().is_none(), "withdrawn on 401");

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
}
