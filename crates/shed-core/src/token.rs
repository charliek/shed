//! Control-token FSM — ported from Swift's `ControlTokenProvider` actor, with
//! the mobile client's FSM extensions ported from
//! `shed-mobile/lib/control/control_token_provider.dart` (itself a faithful
//! port of the orchestrator's controlToken.ts).
//!
//! Caches a shed-server CONTROL token, refreshing it near expiry or on demand
//! (`invalidate*`, called on a 401). The mint primitive is a foreign
//! `TokenMinter` (the host agent, in Swift; a mock in tests) — this crate owns
//! only the cache/refresh/single-flight logic, so it stays pure.
//!
//! FSM extensions (all builder-opted, defaults preserve the pre-existing
//! desktop behavior — plan 001 §3.4):
//!   * a tunable refresh window ([`ControlTokenProvider::with_refresh_window`])
//!   * deterministic per-name refresh jitter ([`name_jitter`],
//!     [`ControlTokenProvider::with_name_jitter`])
//!   * a mint-failure cooldown ([`ControlTokenProvider::with_mint_cooldown`])
//!   * a persisted seed token ([`ControlTokenProvider::with_seed`])
//!   * an injectable clock ([`ControlTokenProvider::with_now`])
//!
//! Two unconditional semantics ported as strict improvements (§3.4):
//!   * keep-valid-on-proactive-failure — a failed refresh mint inside the
//!     refresh window returns the still-valid cached token instead of erroring
//!     (`control_token_provider.dart:82-92`)
//!   * the stale-401 guard — [`ControlTokenProvider::invalidate_if_current`]
//!     ignores a 401 for a token already rotated past
//!     (`control_token_provider.dart:99-105`)
//!
//! Fail-closed contract (mirrors the SDK/CLI, guarded by the tests here since
//! test mode drops the token path so e2e can't reach it): a mint failure with
//! no still-valid cached token yields an error and the client then sends NO
//! token — never a static downgrade.
//!
//! # From token cache to CREDENTIAL provider (plan 001 D5)
//!
//! The same FSM now holds either shape a shed-server issues:
//!
//! ```text
//! token(bearer, expiry)      — auth.mode: token (and legacy/open servers)
//! mtls(certificate, expiry)  — auth.mode: mtls
//! ```
//!
//! Which one it holds is decided by the SERVER at each mint, never by local
//! configuration: the provider hands the minter a fresh CSR, and adopts whichever
//! credential comes back ([`MintedCredential`]). An operator flipping a server
//! between token and mtls therefore needs no client reconfiguration — the next
//! mint moves the provider (and, via [`CredentialObserver`], the app's stored
//! entry) to the other state, in either direction.
//!
//! Every successful mint emits [`CredentialAdopted`], and a mint that changed the
//! SHAPE additionally emits the derived `mode_changed` (plan 002 §7 P1). Both are
//! delivered off the provider — enqueued under its lock, run on its own dispatcher
//! thread — so a foreign handler can neither block a mint nor re-enter the
//! provider unsafely.
//!
//! The transport above NEVER branches on the state it expects to be in and is
//! never rebuilt: the bearer header is populated from
//! [`Credential::bearer_token`] (empty in mtls state) and the client certificate
//! is presented by the [`crate::tls::ClientCertResolver`] this provider owns and
//! writes on every mint. Rotation and mode flips are writes into that shared
//! state; the `reqwest::Client` is untouched.

use std::sync::{Arc, LazyLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use regex::Regex;
use rustls::sign::CertifiedKey;
use serde_json::Value;
use tokio::sync::Mutex;

use crate::csr::ClientKeyPair;
use crate::http::ShedError;
use crate::models::dart_trim;
use crate::tls::{certified_key_from_pem, ClientCertResolver};

/// Size caps on the credential exchange, in both directions — the SINGLE
/// source of truth for every Rust consumer.
///
/// A control credential is a small, bounded thing — a token, a leaf
/// certificate, a hex serial — so a field arriving orders of magnitude larger
/// is a bug or an attempt to make this process carry something it should not.
/// Refusing costs one re-mint; accepting costs whatever the oversized value was
/// for.
///
/// These live in `shed-core` (rather than in one of the layers above it)
/// because every credential mapper the clients ship must enforce the SAME
/// numbers, and they sit in different crates: `shed-app`'s
/// `token_minter::credential_from_parts` (UDS + embedded broker) and
/// `shed-core-ffi`'s `credential_from_answer` (any foreign UniFFI minter, e.g.
/// Swift's). Both re-export or reference these constants — a cap that differed
/// by crate would be a cross-client divergence.
///
/// The THIRD mapper is Swift's `HostAgentCredentialLimits`
/// (`desktop/Sources/ShedKit/Approval/HostAgentProtocol.swift`), which cannot
/// import Rust constants; it is pinned to the same numbers by the shared
/// `tests/host-agent-diff/fixtures/desktop-credential/credential_response.json`
/// `limits` block, which every language asserts against.
pub mod limits {
    pub const MAX_TOKEN_BYTES: usize = 4 * 1024;
    pub const MAX_CLIENT_CERT_BYTES: usize = 64 * 1024;
    pub const MAX_CERT_SERIAL_BYTES: usize = 128;
    pub const MAX_ERROR_BYTES: usize = 4 * 1024;
    /// A P-256 PKCS#10 CSR is ~600 bytes base64; 16 KiB leaves room for larger
    /// key types without leaving room for a payload.
    pub const MAX_CSR_BYTES: usize = 16 * 1024;
}

/// A minted control token plus its optional expiry (unix seconds). `None` expiry
/// → only an explicit `invalidate*()` forces a refresh (mirrors `MintedToken`).
/// Swift parses the host agent's ISO-8601 expiry to epoch before handing it over;
/// the SSH-bundle path parses RFC3339 in this module instead
/// ([`parse_token_bundle`]), and always yields `Some` — a bundle without a
/// parseable expiry is rejected, never treated as non-expiring (plan 001 §3.5).
#[derive(Clone, PartialEq, Eq)]
pub struct MintedToken {
    pub token: String,
    pub expires_at_unix: Option<u64>,
}

/// Redacted Debug (mirrors [`ClientKeyPair`]'s): the token IS the credential, so
/// a derived Debug would print a live bearer into any log line, `anyhow` chain,
/// or `{:?}` of an enclosing type — including [`MintedCredential`], which nests
/// this. The expiry is kept: it is the field anyone debugging a credential
/// actually needs.
impl std::fmt::Debug for MintedToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MintedToken")
            .field("token", &"<redacted>")
            .field("token_len", &self.token.len())
            .field("expires_at_unix", &self.expires_at_unix)
            .finish()
    }
}

/// The credential shape a shed-server issues — the wire value of a bootstrap
/// bundle's `auth_mode`.
///
/// **Absent means token.** A server built before client-certificate support omits
/// the key entirely, and so does every stored client entry written by such a
/// build, so [`AuthMode::from_wire`] is the single place that rule is decided.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AuthMode {
    /// A bearer token (`auth.mode: token`, and legacy/open servers).
    Token,
    /// A client certificate bound to a key this process generated
    /// (`auth.mode: mtls`).
    Mtls,
}

impl AuthMode {
    /// The wire/config literal (`"token"` / `"mtls"`).
    pub fn as_str(self) -> &'static str {
        match self {
            AuthMode::Token => "token",
            AuthMode::Mtls => "mtls",
        }
    }

    /// Decode a wire/config value. `None`, `""`, and any UNRECOGNIZED value all
    /// decode as [`AuthMode::Token`] — matching Go's `sdk.Bundle.Mode`: an unknown
    /// future mode is not something this client can act on, and treating it as
    /// mtls (the branch that expects a certificate) would fail more confusingly
    /// than treating it as the shape whose fields are actually populated.
    pub fn from_wire(raw: Option<&str>) -> Self {
        match raw.map(str::trim) {
            Some("mtls") => AuthMode::Mtls,
            _ => AuthMode::Token,
        }
    }
}

/// A client certificate issued for the CSR the provider submitted. The matching
/// private key never appears here — it stays with the provider that generated it
/// and is never handed to a minter (plan 001 D6).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MintedCertificate {
    /// PEM leaf, exactly as the bundle's `client_cert` delivered it.
    pub cert_pem: String,
    /// Lower-case hex serial. Opaque to the client — logs and rotation proofs.
    pub serial: String,
    /// Unix seconds. `None` only for a minter that reports no expiry; the SSH
    /// bundle parse always yields `Some` (see [`parse_credential_bundle`]).
    pub expires_at_unix: Option<u64>,
}

/// What a mint returns — the SERVER's choice of credential shape, not the
/// client's.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum MintedCredential {
    Token(MintedToken),
    Certificate(MintedCertificate),
}

impl MintedCredential {
    pub fn mode(&self) -> AuthMode {
        match self {
            MintedCredential::Token(_) => AuthMode::Token,
            MintedCredential::Certificate(_) => AuthMode::Mtls,
        }
    }
}

/// What the provider hands a minter for one mint attempt.
///
/// It carries the base64 CSR when — and only when — the minter advertised
/// [`TokenMinter::supports_mtls`]. A minter that cannot enroll never pays for a
/// keypair generation it would ignore, and a server that answers a CSR-less
/// bootstrap in mtls mode returns its explicit "upgrade" error rather than a
/// certificate this client could not use.
#[derive(Clone, Debug, Default)]
pub struct CredentialRequest {
    csr_base64: Option<String>,
}

impl CredentialRequest {
    /// The `csr=<base64 std DER>` argument value for the `_bootstrap` request
    /// line, or `None` for a token-only mint.
    pub fn csr_base64(&self) -> Option<&str> {
        self.csr_base64.as_deref()
    }

    /// Build a request carrying `csr_base64`.
    ///
    /// Production never calls this — the provider constructs the request from the
    /// keypair it just generated, which is what keeps the private half here. It
    /// exists for the foreign minters' own tests (the embedded broker adapter, the
    /// mobile bridge): asserting "the CSR I was handed is the one I relayed" is the
    /// property that proves a bridge generated no second keypair, and it should not
    /// require standing up a whole provider to state. The CSR is public material,
    /// so a constructor for it widens nothing.
    pub fn with_csr(csr_base64: impl Into<String>) -> Self {
        Self {
            csr_base64: Some(csr_base64.into()),
        }
    }
}

/// The mint primitive: request a fresh CONTROL credential for `server`.
/// Implemented by the foreign host-agent bridge (Swift), the embedded broker, the
/// mobile bridge, or a test mock. A failure (Err) is fail-closed — the provider
/// surfaces it and the caller sends no credential.
///
/// **Extending an implementation for mtls** (plan 001 D5): override
/// [`Self::supports_mtls`] to return `true` and [`Self::mint_credential`] to run
/// the bootstrap with the request's `csr=` argument appended, returning whatever
/// the bundle carried. Implementations that do neither keep working unchanged —
/// the default `mint_credential` delegates to [`Self::mint`] and wraps the token,
/// which is the entire token-mode fleet's behavior today.
#[async_trait::async_trait]
pub trait TokenMinter: Send + Sync {
    async fn mint(&self, server: &str) -> Result<MintedToken, ShedError>;

    /// Can this minter carry a CSR to the server and a certificate back?
    ///
    /// Default `false`: a token-only minter. This is a capability advertisement,
    /// not a preference — an mtls-capable minter still receives whatever the
    /// server chooses to issue.
    fn supports_mtls(&self) -> bool {
        false
    }

    /// Mint whatever credential the server issues for `req`.
    ///
    /// The default implementation ignores the request (it can only be empty for a
    /// minter that did not advertise mtls) and wraps [`Self::mint`].
    async fn mint_credential(
        &self,
        server: &str,
        req: &CredentialRequest,
    ) -> Result<MintedCredential, ShedError> {
        let _ = req;
        self.mint(server).await.map(MintedCredential::Token)
    }
}

/// What the provider adopted, emitted after EVERY successful mint — plan 002 §7
/// P1's `credential_adopted`, the persistence primitive every client bridge hangs
/// its stored-entry write on.
///
/// # What it carries, and what it deliberately does not
///
/// The mint outcome minus everything a consumer has no business holding:
///   * `server` + `mode` + `expires_at_unix` — enough to persist a learned
///     `auth_mode` and to render "renews at …" in a UI;
///   * `token` — populated in [`AuthMode::Token`] ONLY, because the token IS the
///     credential there and the consumer's store is the sanctioned home for it
///     (mobile's `ServerRecord` seed);
///   * in [`AuthMode::Mtls`], NOTHING that could authenticate: no certificate, no
///     serial, and above all no private key. The key never leaves the provider
///     (plan 001 D6 / 002 §7 P3), and a certificate without it is useless, so
///     shipping either across a foreign boundary would buy a consumer nothing
///     while widening the surface an audit has to reason about.
#[derive(Clone, PartialEq, Eq)]
pub struct CredentialAdopted {
    /// The server name this provider was constructed for.
    pub server: String,
    /// The shape just adopted.
    pub mode: AuthMode,
    /// Unix seconds; `None` for a credential the minter reported no expiry for.
    pub expires_at_unix: Option<u64>,
    /// The bearer token — `Some` in [`AuthMode::Token`], ALWAYS `None` in
    /// [`AuthMode::Mtls`].
    pub token: Option<String>,
}

/// Redacted Debug, for the same reason [`MintedToken`]'s is: the event travels to
/// app-layer code that logs liberally, and in token mode it holds a live bearer.
impl std::fmt::Debug for CredentialAdopted {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CredentialAdopted")
            .field("server", &self.server)
            .field("mode", &self.mode)
            .field("expires_at_unix", &self.expires_at_unix)
            .field("token", &self.token.as_ref().map(|_| "<redacted>"))
            .finish()
    }
}

/// Notified about the provider's credential adoptions — plan 002 §7 P1.
///
/// The app layer owns persistence (writing `auth_mode`, and on mobile the token,
/// into its stored entry); the provider owns correctness. A client that misses an
/// event pays one re-learn on next launch and nothing more — no behavior here
/// depends on the write succeeding.
///
/// # Delivery contract (read before implementing)
///
/// Both callbacks run on the provider's own dispatcher THREAD, in emission order,
/// with NO provider lock held and nothing of the provider's on the stack:
///   * a handler may block for as long as it likes — it delays only later events,
///     never a mint (the mint path enqueues and returns);
///   * a handler may call back into the provider it was registered on without
///     deadlocking (it is not re-entrancy — the mint that produced the event has
///     already released the lock and returned to its caller);
///   * the callbacks are consequently ASYNCHRONOUS with respect to the
///     `credential()` call that produced them: a test (or a UI) that must see an
///     event waits for it rather than reading immediately after the mint returns.
pub trait CredentialObserver: Send + Sync {
    /// A mint succeeded and the provider adopted `event`'s credential. Fires on
    /// EVERY successful mint — including a plain rotation that changed nothing but
    /// the credential's value.
    fn on_credential_adopted(&self, event: &CredentialAdopted) {
        let _ = event;
    }

    /// The DERIVED transition event (plan 001 D5's `mode_changed`): the provider
    /// adopted a credential of a different shape than the one it last ANNOUNCED,
    /// in either direction. Emitted from the same queue as
    /// [`Self::on_credential_adopted`] and always immediately after it, so a
    /// consumer that handles both sees the adoption first.
    ///
    /// "Last announced" rather than "last cached" is what makes it a transition
    /// event: an invalidation (a 401, a refused certificate) drops the cached
    /// credential, and the re-mint that follows almost always lands on the SAME
    /// shape — a rotation, which this event must stay silent for.
    fn on_mode_changed(&self, server: &str, mode: AuthMode) {
        let _ = (server, mode);
    }
}

/// One queued observer notification: the adoption, plus whether this adoption was
/// also a mode TRANSITION.
///
/// The transition is decided by the provider (which knows the shape it last
/// announced, and knows a seeded credential was never announced at all because its
/// consumer supplied it) rather than by the dispatcher, which could only infer it
/// from the event stream and would report a spurious flip for the first mint after
/// a seed of the same shape.
struct Emission {
    event: CredentialAdopted,
    mode_changed: bool,
}

/// The non-blocking delivery seam (plan 002 §7 P1): the provider ENQUEUES under
/// its state lock, a dedicated thread DELIVERS with no lock held.
///
/// # Why a thread and not a task
///
/// [`CredentialObserver`]'s callbacks are synchronous and foreign — a Swift
/// closure over UniFFI, a Dart `StreamSink`, a UI store's mutex. Any of them may
/// block. Delivering them from a `tokio::spawn`ed task would put that block on a
/// runtime worker, and on a current-thread runtime (which is what a test, and any
/// embedder that builds one, gets) it would stall the entire executor — including
/// the mint path this design exists to protect. A thread is the only delivery
/// vehicle whose worst case is "this observer falls behind".
///
/// The thread is started only when an observer is registered ([`with_observer`]),
/// so a provider without one pays nothing, and it exits when the provider is
/// dropped (the channel closes and the `for` loop ends — after the callback
/// currently in flight returns). That last clause is the observer CONTRACT: a
/// handler may block briefly, but it must return. A handler that never returns
/// pins this thread, the observer, and any queued emissions (token values
/// included) for the life of the process — the design trades that bounded,
/// foreign-bug-only leak for the guarantee that no handler can ever stall the
/// mint path or the async runtime. If the thread cannot be spawned at all — a
/// process out of thread handles — the provider keeps working and simply emits
/// no events, which is the same degradation a consumer already tolerates when
/// it misses one.
///
/// [`with_observer`]: ControlTokenProvider::with_observer
struct CredentialEvents {
    tx: std::sync::mpsc::Sender<Emission>,
}

impl CredentialEvents {
    fn start(observer: Arc<dyn CredentialObserver>) -> Option<Self> {
        let (tx, rx) = std::sync::mpsc::channel::<Emission>();
        std::thread::Builder::new()
            // Linux caps thread names at 15 bytes; keep it inside that so the name
            // survives into `ps`/Instruments where it is actually useful.
            .name("shed-credevent".into())
            .spawn(move || {
                for em in rx {
                    observer.on_credential_adopted(&em.event);
                    if em.mode_changed {
                        observer.on_mode_changed(&em.event.server, em.event.mode);
                    }
                }
            })
            .ok()
            .map(|_detached| Self { tx })
    }

    /// Enqueue one notification. Never blocks and never fails loudly: a receiver
    /// that has gone away (only possible if the dispatcher thread panicked inside
    /// a foreign handler) must not turn into a mint failure.
    fn emit(&self, em: Emission) {
        let _ = self.tx.send(em);
    }
}

/// The credential a request should present — the public snapshot of the
/// provider's state. Deliberately carries NO private key: the key lives only
/// inside the provider and inside the rustls resolver it feeds.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct Credential {
    /// `None` when nothing is held at all (fresh provider, or a mint that never
    /// succeeded).
    pub mode: Option<AuthMode>,
    /// Bearer token; meaningful only in [`AuthMode::Token`].
    pub token: String,
    /// PEM leaf; meaningful only in [`AuthMode::Mtls`].
    pub cert_pem: String,
    /// Issued certificate serial (mtls only) — the rotation-visible identity.
    pub cert_serial: String,
    /// Unix seconds; `None` means "never proactively re-mint".
    pub expires_at_unix: Option<u64>,
}

/// Redacted Debug (mirrors [`ClientKeyPair`]'s). `Credential` is the value that
/// travels furthest — it is returned to callers, held by the request path, and
/// captured in retry-decision logs — so a derived Debug is the most likely place
/// for a live bearer token to escape. The certificate PEM is public material and
/// the serial is the rotation-visible identity, so both stay readable; only the
/// token is withheld.
impl std::fmt::Debug for Credential {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Credential")
            .field("mode", &self.mode)
            .field("token", &"<redacted>")
            .field("token_len", &self.token.len())
            .field("cert_pem_len", &self.cert_pem.len())
            .field("cert_serial", &self.cert_serial)
            .field("expires_at_unix", &self.expires_at_unix)
            .finish()
    }
}

impl Credential {
    /// The token to send in an `Authorization` header, or `None` when this
    /// credential is not a bearer token.
    ///
    /// The mode gate is load-bearing, not tidiness: in mtls mode the server never
    /// reads the header, so a client that kept sending a stale bearer alongside
    /// its certificate would be shipping a live credential to an endpoint that
    /// ignores it — pure downside.
    pub fn bearer_token(&self) -> Option<&str> {
        match self.mode {
            Some(AuthMode::Token) if !self.token.is_empty() => Some(&self.token),
            _ => None,
        }
    }

    /// Does this credential have anything to present?
    ///
    /// The default (nothing held) is NOT usable, and neither is a mode whose
    /// payload is missing — the shape a stored entry degrades to when its
    /// certificate files are gone or an older client stripped its fields. Both
    /// mean the same thing to a request: it goes out unauthenticated and a server
    /// that demands a credential refuses it. This is what the provider keys its
    /// "mint before the first request" decision on, because expiry cannot express
    /// it: a credential you never had has no expiry to be near (Go parity —
    /// `clienttoken.Credential.Usable`).
    pub fn usable(&self) -> bool {
        match self.mode {
            Some(AuthMode::Token) => !self.token.is_empty(),
            Some(AuthMode::Mtls) => !self.cert_pem.is_empty(),
            None => false,
        }
    }

    /// The identity a reactive invalidation compares against: the token string in
    /// token state, the certificate serial in mtls state. Two credentials with the
    /// same identity are the same credential.
    ///
    /// # It is VALUE identity, not a generation counter — deliberately
    ///
    /// A generation counter ("is the cache still on the mint I used?") would be
    /// strictly more precise, and it is not adopted because the two places this is
    /// consulted both want the value:
    ///
    ///   * the retry-skip in `http::Client::request` asks "would re-sending
    ///     change anything?" — a re-mint that returns a byte-identical token
    ///     would be refused identically, so treating it as the same credential is
    ///     the CORRECT answer and a generation counter would give the wrong one
    ///     (a pointless retry);
    ///   * [`ControlTokenProvider::invalidate_if_current_credential`] asks "is
    ///     what I hold the thing the server refused?" — for a re-issued identical
    ///     token the answer is materially yes, whatever generation it came from.
    ///
    /// The two shapes a value cannot distinguish are covered:
    ///   * a certificate issued WITHOUT a serial has no identity at all, so the
    ///     caller treats an empty identity as "always invalidate";
    ///   * and in mtls state attribution is skipped entirely now (see
    ///     `invalidate_if_current_credential`), so a missing serial cannot
    ///     produce a missed invalidation there in the first place.
    ///
    /// The residue is a TOKEN-state stale-401 for a token that was rotated away
    /// and then re-minted to the identical string; it invalidates where a
    /// generation counter would not, which costs one mint and is the safe
    /// direction. Threading a real generation would mean putting it on this
    /// public struct and through every construction site for that single case.
    pub(crate) fn identity(&self) -> &str {
        match self.mode {
            Some(AuthMode::Mtls) => &self.cert_serial,
            _ => &self.token,
        }
    }
}

/// The default 2h refresh window mirrors the SDK/CLI: refresh this long before
/// expiry so routine requests rarely race a 401. (Mobile passes its own 2h5m
/// via [`ControlTokenProvider::with_refresh_window`] — per-client parity, not
/// forced convergence.)
const REFRESH_WINDOW: Duration = Duration::from_secs(2 * 60 * 60);

/// The PERSISTED mint-failure message, served on later cooldown-blocked
/// calls. Deliberately FIXED/redacted: a [`TokenMinter`] error's Display text
/// can embed transport detail or even token material, which must never be
/// stored and replayed to later callers. The immediate caller of the failing
/// mint still receives the minter's real error (matching the pre-existing
/// propagation and Dart's rethrow, `control_token_provider.dart:149`).
const MINT_FAILED_REDACTED: &str = "control token mint failed";

/// Deterministic per-name jitter in `[0, max_ms)` — stable across restarts, no
/// RNG. Ported exactly from mobile's `nameJitter`
/// (`control_token_provider.dart:175-181`), which mirrors controlToken.ts (a
/// 32-bit signed hash like JS `|0`): iterate the name's UTF-16 CODE UNITS
/// (Dart `codeUnitAt` parity — a non-BMP char such as an emoji contributes its
/// surrogate PAIR, two units), fold `h = h*31 + cu` wrapped to 32-bit signed
/// at every step, then take `unsigned_abs()` (`i32::MIN`-safe where Dart's
/// 64-bit `abs()` is trivially safe and a Rust `abs()` would overflow) modulo
/// `max(max_ms, 1)`. `max_ms == 0` therefore always yields 0 (jitter off).
pub fn name_jitter(name: &str, max_ms: u64) -> u64 {
    let mut h: i32 = 0;
    for cu in name.encode_utf16() {
        // Dart computes `h * 31 + cu` in 64-bit then `.toSigned(32)`; a
        // stepwise 32-bit wrapping mul+add is identical (truncation mod 2^32
        // distributes over * and +).
        h = h.wrapping_mul(31).wrapping_add(i32::from(cu));
    }
    u64::from(h.unsigned_abs()) % max_ms.max(1)
}

// ---------------------------------------------------------------------------
// SSH token-bundle parsing (plan 001 §3.5) — fail-closed ports of mobile's
// `token_bundle.dart` (itself a port of the orchestrator's controlToken.ts).
//
// Sibling, NOT a duplicate: `shed-broker/src/bootstrap.rs:decode_bundle` also
// decodes a `_bootstrap` bundle, but with Go-parity semantics — expiry is
// OPTIONAL there (absent/zero → non-expiring `None`). These parsers are the
// MOBILE-parity view: expiry is REQUIRED and a bundle without one is rejected.
// The two must not be merged (§3.5); shed-core also must never depend on
// shed-broker (the dependency direction is broker → core).
// ---------------------------------------------------------------------------

/// Fail-closed rejection of an SSH `_bootstrap` token bundle. Mirrors the
/// mobile error codes (`app_error.dart`): `SHED_AUTH_EXPIRED`,
/// `SHED_TLS_PIN_MISMATCH`, `SHED_TLS_PIN_MISSING`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum TokenBundleError {
    /// Bad JSON, non-`control` scope, empty token, or a missing / unparseable
    /// / pre-epoch `expires_at` (mobile `SHED_AUTH_EXPIRED`).
    #[error("control token bundle rejected: expired or malformed (SHED_AUTH_EXPIRED)")]
    AuthExpired,
    /// The bundle's TLS fingerprint differs from the already-configured pin —
    /// a trust-model change we refuse to make silently
    /// (mobile `SHED_TLS_PIN_MISMATCH`).
    #[error("control token bundle TLS fingerprint does not match the configured pin (SHED_TLS_PIN_MISMATCH)")]
    PinMismatch,
    /// The bundle omits a well-formed TLS fingerprint where one is REQUIRED
    /// (the add-server flow, [`parse_control_bundle`]; mobile
    /// `SHED_TLS_PIN_MISSING`).
    #[error("control token bundle omits a valid tls_cert_fingerprint (SHED_TLS_PIN_MISSING)")]
    PinMissing,
}

/// The full `_bootstrap control` bundle (token + TLS pin + https port), used by
/// the add-server flow, which bootstraps the TLS pin from this SSH-delivered
/// value (the SSH channel is host-key-pinned). Stricter than
/// [`parse_token_bundle`]: the fingerprint and a positive `https_port` are
/// REQUIRED. Port of `token_bundle.dart:66-78`.
#[derive(Clone, PartialEq, Eq)]
pub struct ControlBundle {
    /// The credential shape the server issued. ABSENT `auth_mode` decodes as
    /// [`AuthMode::Token`] — a pre-mtls server omits the key entirely, and this is
    /// what keeps a new client working against a released server (plan 001 D4,
    /// compat matrix leg 4).
    pub auth_mode: AuthMode,
    /// Bearer token; empty in mtls mode (the bundle carries no token at all).
    pub token: String,
    /// PEM client certificate; empty in token mode.
    pub client_cert: String,
    /// Issued certificate serial, lower-case hex; empty in token mode.
    pub cert_serial: String,
    pub expires_at_unix: u64,
    /// Canonical `sha256:<lowercase hex>` leaf-cert pin (`fingerprint.dart:5-8`).
    pub tls_cert_fingerprint: String,
    pub https_port: u16,
}

/// Redacted Debug, for the same reason as [`MintedToken`]'s: a decoded bundle
/// holds the live bearer token in token mode, and the add-server flow logs
/// liberally around it.
impl std::fmt::Debug for ControlBundle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ControlBundle")
            .field("auth_mode", &self.auth_mode)
            .field("token", &"<redacted>")
            .field("token_len", &self.token.len())
            .field("client_cert_len", &self.client_cert.len())
            .field("cert_serial", &self.cert_serial)
            .field("expires_at_unix", &self.expires_at_unix)
            .field("tls_cert_fingerprint", &self.tls_cert_fingerprint)
            .field("https_port", &self.https_port)
            .finish()
    }
}

/// Canonical TLS pin form: `sha256:` + lowercase hex of SHA-256(DER leaf) —
/// mobile's `kTlsFingerprintRe` (`fingerprint.dart:8`). The SSH host-key pin
/// uses a different format (`SHA256:<base64>`); never cross-compare the two.
static RE_TLS_FINGERPRINT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^sha256:[0-9a-f]{64}$").unwrap());

/// Validate an SSH bootstrap bundle (one JSON line) into a [`MintedToken`],
/// failing closed. Rule-for-rule port of `token_bundle.dart:29-60`
/// (`parseTokenBundle`):
///   * bad JSON, a non-object, non-`control` scope, an empty/whitespace
///     token, or a missing/unparseable `expires_at` →
///     [`TokenBundleError::AuthExpired`]
///   * a minted `tls_cert_fingerprint` that is malformed or differs from an
///     already-configured `expected_pin` → [`TokenBundleError::PinMismatch`]
///     (a trust-model change we refuse to make silently)
///
/// When `expected_pin` is `Some`, a present bundle fingerprint must match; an
/// ABSENT one (or a non-string value) is tolerated — the bundle still arrived
/// over a host-key-pinned SSH channel (`token_bundle.dart:25-28,45-52`). The
/// add-server flow additionally requires the pin — that is
/// [`parse_control_bundle`]. `expected_pin` must already be in canonical
/// lowercase `sha256:<hex>` form (mobile stores it canonicalized).
///
/// The result's `expires_at_unix` is always `Some` — elsewhere in this module
/// `None` means non-expiring, but the bundle parse REJECTS an absent expiry
/// instead (plan 001 §3.5). One deliberate divergence from Dart: mobile keeps
/// the parsed expiry as a signed `DateTime`, while `MintedToken` carries
/// `u64` unix seconds — a PRE-EPOCH (negative) expiry therefore maps to
/// [`TokenBundleError::AuthExpired`] here. Justified fail-closed: a control
/// token that expired before 1970 is already expired / nonsensical, and no
/// live mint emits one.
pub fn parse_token_bundle(
    stdout: &str,
    expected_pin: Option<&str>,
) -> Result<MintedToken, TokenBundleError> {
    let raw: Value = serde_json::from_str(stdout).map_err(|_| TokenBundleError::AuthExpired)?;
    let obj = raw.as_object().ok_or(TokenBundleError::AuthExpired)?;

    if obj.get("scope").and_then(Value::as_str) != Some("control") {
        return Err(TokenBundleError::AuthExpired);
    }

    // Dart-set trim ([`dart_trim`]): Dart's `String.trim()` strips U+FEFF
    // (BOM) where Rust's `str::trim` does not — a BOM-only token must fail
    // closed here exactly as it does on mobile (`token_bundle.dart:42-43`).
    let token = dart_trim(
        obj.get("token")
            .and_then(Value::as_str)
            .ok_or(TokenBundleError::AuthExpired)?,
    );
    if token.is_empty() {
        return Err(TokenBundleError::AuthExpired);
    }

    // Conditional/tolerant pin check (`token_bundle.dart:45-52`): only when a
    // pin is configured AND the bundle carries a string fingerprint.
    if let Some(pin) = expected_pin {
        if let Some(minted_fp) = obj.get("tls_cert_fingerprint").and_then(Value::as_str) {
            let minted = dart_trim(minted_fp).to_lowercase();
            if !RE_TLS_FINGERPRINT.is_match(&minted) || minted != pin {
                return Err(TokenBundleError::PinMismatch);
            }
        }
    }

    let expires_at_unix = require_expiry_unix(obj)?;

    Ok(MintedToken {
        token: token.to_string(),
        expires_at_unix: Some(expires_at_unix),
    })
}

/// Parse the full `_bootstrap control` bundle for the add-server flow. Rule-
/// for-rule port of `token_bundle.dart:80-118` (`parseControlBundle`) —
/// STRICTER than [`parse_token_bundle`]:
///   * JSON / object / `scope == "control"` / non-blank token / required
///     parseable `expires_at` → [`TokenBundleError::AuthExpired`] on failure
///   * `tls_cert_fingerprint` REQUIRED: not a string, or malformed after
///     trim+lowercase → [`TokenBundleError::PinMissing`]; well-formed but
///     different from a `Some` `expected_pin` →
///     [`TokenBundleError::PinMismatch`]
///   * `https_port` REQUIRED: not an integer or outside `1..=65535` →
///     [`TokenBundleError::AuthExpired`]
///
/// Same pre-epoch-expiry divergence as [`parse_token_bundle`] (see its doc).
///
/// mtls (plan 001 D4): when `auth_mode` is `"mtls"` the bundle carries a
/// `client_cert` PEM instead of a token, and the token rules above are replaced
/// by "the certificate must be a non-empty PEM CERTIFICATE block". Everything
/// else — scope, required pin, required `https_port`, required expiry — is
/// identical, because those describe the SERVER, not the credential.
pub fn parse_control_bundle(
    stdout: &str,
    expected_pin: Option<&str>,
) -> Result<ControlBundle, TokenBundleError> {
    let raw: Value = serde_json::from_str(stdout).map_err(|_| TokenBundleError::AuthExpired)?;
    let obj = raw.as_object().ok_or(TokenBundleError::AuthExpired)?;

    if obj.get("scope").and_then(Value::as_str) != Some("control") {
        return Err(TokenBundleError::AuthExpired);
    }

    let auth_mode = AuthMode::from_wire(obj.get("auth_mode").and_then(Value::as_str));
    let (token, client_cert, cert_serial) = credential_fields(obj, auth_mode)?;

    let fp = dart_trim(
        obj.get("tls_cert_fingerprint")
            .and_then(Value::as_str)
            .ok_or(TokenBundleError::PinMissing)?,
    )
    .to_lowercase();
    if !RE_TLS_FINGERPRINT.is_match(&fp) {
        return Err(TokenBundleError::PinMissing);
    }
    if let Some(pin) = expected_pin {
        if fp != pin {
            return Err(TokenBundleError::PinMismatch);
        }
    }

    // Dart `portRaw is! int` (`token_bundle.dart:101-104`): a JSON float is
    // rejected — `Value::as_i64` is `None` for non-integer numbers.
    let port = obj
        .get("https_port")
        .and_then(Value::as_i64)
        .ok_or(TokenBundleError::AuthExpired)?;
    let https_port = u16::try_from(port).map_err(|_| TokenBundleError::AuthExpired)?;
    if https_port == 0 {
        return Err(TokenBundleError::AuthExpired);
    }

    let expires_at_unix = require_expiry_unix(obj)?;

    Ok(ControlBundle {
        auth_mode,
        token,
        client_cert,
        cert_serial,
        expires_at_unix,
        tls_cert_fingerprint: fp,
        https_port,
    })
}

/// The credential half of a bundle, per mode: `(token, client_cert, cert_serial)`.
///
/// Token mode keeps the exact pre-existing rules (Dart-trimmed, non-blank, else
/// [`TokenBundleError::AuthExpired`]). mtls mode requires a `client_cert` that
/// actually looks like a PEM certificate — a bundle claiming mtls with no usable
/// certificate is rejected rather than adopted as an empty credential that would
/// fail every later handshake with no explanation.
fn credential_fields(
    obj: &serde_json::Map<String, Value>,
    mode: AuthMode,
) -> Result<(String, String, String), TokenBundleError> {
    match mode {
        AuthMode::Token => {
            // Dart-set trim (BOM parity, `token_bundle.dart:90-93`).
            let token = dart_trim(
                obj.get("token")
                    .and_then(Value::as_str)
                    .ok_or(TokenBundleError::AuthExpired)?,
            );
            if token.is_empty() {
                return Err(TokenBundleError::AuthExpired);
            }
            Ok((token.to_string(), String::new(), String::new()))
        }
        AuthMode::Mtls => {
            let cert = obj
                .get("client_cert")
                .and_then(Value::as_str)
                .ok_or(TokenBundleError::AuthExpired)?;
            if crate::csr::pem_decode(crate::csr::PEM_LABEL_CERTIFICATE, cert).is_err() {
                return Err(TokenBundleError::AuthExpired);
            }
            let serial = obj
                .get("cert_serial")
                .and_then(Value::as_str)
                .map(dart_trim)
                .unwrap_or_default()
                .to_string();
            Ok((String::new(), cert.to_string(), serial))
        }
    }
}

/// Decode one `_bootstrap` bundle into the credential the provider should adopt.
///
/// The MINT-path parse, sibling to [`parse_control_bundle`]'s add-server parse:
/// the TLS pin and `https_port` are already known here (the transport exists), so
/// only the credential and its expiry are required. `expected_pin`, when given,
/// is still enforced against a PRESENT fingerprint — a mid-life re-pin is a
/// trust-model change this client refuses to make silently — while an absent one
/// is tolerated, exactly as [`parse_token_bundle`] does.
///
/// This is the decoder a foreign minter (Swift's `HostAgentTokenMinter`, the
/// mobile mint sink, the broker's SSH bootstrap) runs over the raw bundle line to
/// produce a [`MintedCredential`].
pub fn parse_credential_bundle(
    stdout: &str,
    expected_pin: Option<&str>,
) -> Result<MintedCredential, TokenBundleError> {
    let raw: Value = serde_json::from_str(stdout).map_err(|_| TokenBundleError::AuthExpired)?;
    let obj = raw.as_object().ok_or(TokenBundleError::AuthExpired)?;

    if obj.get("scope").and_then(Value::as_str) != Some("control") {
        return Err(TokenBundleError::AuthExpired);
    }
    if let Some(pin) = expected_pin {
        if let Some(minted_fp) = obj.get("tls_cert_fingerprint").and_then(Value::as_str) {
            let minted = dart_trim(minted_fp).to_lowercase();
            if !RE_TLS_FINGERPRINT.is_match(&minted) || minted != pin {
                return Err(TokenBundleError::PinMismatch);
            }
        }
    }

    let auth_mode = AuthMode::from_wire(obj.get("auth_mode").and_then(Value::as_str));
    let (token, client_cert, cert_serial) = credential_fields(obj, auth_mode)?;
    let expires_at_unix = require_expiry_unix(obj)?;

    Ok(match auth_mode {
        AuthMode::Token => MintedCredential::Token(MintedToken {
            token,
            expires_at_unix: Some(expires_at_unix),
        }),
        AuthMode::Mtls => MintedCredential::Certificate(MintedCertificate {
            cert_pem: client_cert,
            serial: cert_serial,
            expires_at_unix: Some(expires_at_unix),
        }),
    })
}

/// The shared REQUIRED-expiry tail of both bundle parses
/// (`token_bundle.dart:54-57,107-110`): `expires_at` must be a string that
/// parses as RFC3339; anything else — including the pre-epoch u64 divergence
/// documented on [`parse_token_bundle`] — is [`TokenBundleError::AuthExpired`].
fn require_expiry_unix(obj: &serde_json::Map<String, Value>) -> Result<u64, TokenBundleError> {
    let raw = obj
        .get("expires_at")
        .and_then(Value::as_str)
        .ok_or(TokenBundleError::AuthExpired)?;
    let secs = parse_rfc3339_to_unix(raw).map_err(|()| TokenBundleError::AuthExpired)?;
    u64::try_from(secs).map_err(|_| TokenBundleError::AuthExpired)
}

/// Parse a strict UTC RFC3339 timestamp
/// (`YYYY-MM-DDTHH:MM:SS[.frac](Z|±HH:MM)`) to unix seconds, sub-second
/// digits validated then truncated.
///
/// Std-only (shed-core carries no time dependency): the same Howard Hinnant
/// days-from-civil mechanism as `shed-broker/src/status.rs:
/// parse_rfc3339_to_unix` — PORTED, not imported (the dependency direction is
/// broker → core), and with one semantic difference: the broker collapses the
/// Go zero time `0001-01-01T00:00:00Z` to `None` (Go `IsZero()` parity),
/// while this parser has no such case — it returns the plain instant
/// (-62135596800), matching Dart's `DateTime.tryParse`, which parses the Go
/// zero time successfully (`token_bundle.dart:56`). The bundle parses above
/// then reject any pre-epoch value at the u64 conversion.
///
/// Calendar validation is STRICTER than both siblings — deliberate for a
/// fail-closed auth parser (a live mint never emits an impossible date):
/// impossible dates (`2023-02-29`, day 32, month 13) are REJECTED rather
/// than silently normalized through the civil-date math (the broker parser)
/// or by `DateTime.tryParse` (Dart). The one permissive carve-out is
/// `second == 60`, kept because real RFC3339 timestamps carry leap seconds.
fn parse_rfc3339_to_unix(s: &str) -> Result<i64, ()> {
    let (date, time_and_zone) = s.split_once('T').ok_or(())?;
    let (y, mo, d) = {
        let mut it = date.split('-');
        let y: i64 = parse_fixed(it.next().ok_or(())?, 4)?;
        let mo: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let d: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (y, mo, d)
    };
    // Split the zone suffix off the time, then split off fractional seconds.
    let (time_part, offset_secs) = split_zone(time_and_zone)?;
    let (hms, frac) = match time_part.split_once('.') {
        Some((h, f)) => (h, Some(f)),
        None => (time_part, None),
    };
    if let Some(f) = frac {
        // Sub-second digits are validated then dropped (truncation).
        if f.is_empty() || !f.bytes().all(|b| b.is_ascii_digit()) {
            return Err(());
        }
    }
    let (h, mi, se) = {
        let mut it = hms.split(':');
        let h: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let mi: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let se: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (h, mi, se)
    };
    // Strict calendar validation (see the doc comment): real month/day only.
    // se == 60 admits a leap second (RFC3339 allows it).
    if !(1..=12).contains(&mo) || d < 1 || d > days_in_month(y, mo) || h > 23 || mi > 59 || se > 60
    {
        return Err(());
    }
    let days = days_from_civil(y, mo, d);
    Ok(days * 86_400 + i64::from(h) * 3_600 + i64::from(mi) * 60 + i64::from(se) - offset_secs)
}

/// Days in `month` of Gregorian `year` (proleptic leap rules:
/// `y%4==0 && (y%100!=0 || y%400==0)`). Callers guarantee `1 <= month <= 12`.
fn days_in_month(year: i64, month: u32) -> u32 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        _ => {
            if year % 4 == 0 && (year % 100 != 0 || year % 400 == 0) {
                29
            } else {
                28
            }
        }
    }
}

/// Days from the civil date to the unix epoch (Howard Hinnant's
/// `days_from_civil`; the same math as `shed-broker/src/status.rs:171-179`).
fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400; // [0, 399]
    let mp = i64::from(if m > 2 { m - 3 } else { m + 9 }); // [0, 11]
    let doy = (153 * mp + 2) / 5 + i64::from(d) - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe - 719_468
}

/// Split an RFC3339 zone suffix (`Z` or `±HH:MM`) off the end, returning the
/// remaining time text and the offset in seconds to SUBTRACT to reach UTC.
fn split_zone(time_and_zone: &str) -> Result<(&str, i64), ()> {
    if let Some(rest) = time_and_zone.strip_suffix('Z') {
        return Ok((rest, 0));
    }
    // The last '+' or '-' after index 0 starts the offset.
    let bytes = time_and_zone.as_bytes();
    let mut idx = None;
    for (i, &b) in bytes.iter().enumerate() {
        if (b == b'+' || b == b'-') && i > 0 {
            idx = Some(i);
        }
    }
    let i = idx.ok_or(())?;
    let (time_part, off) = time_and_zone.split_at(i);
    // `+05:00` is 5h AHEAD of UTC → subtract 5h to reach UTC; `-05:00` adds.
    let sign = if off.as_bytes()[0] == b'+' { 1 } else { -1 };
    let off = &off[1..];
    let (oh, om) = off.split_once(':').ok_or(())?;
    let oh = i64::from(parse_fixed::<u32>(oh, 2)?);
    let om = i64::from(parse_fixed::<u32>(om, 2)?);
    if oh > 23 || om > 59 {
        return Err(());
    }
    Ok((time_part, sign * (oh * 3_600 + om * 60)))
}

/// Parse an exactly-`width`-digit unsigned field (RFC3339 fields are
/// fixed-width).
fn parse_fixed<T: std::str::FromStr>(s: &str, width: usize) -> Result<T, ()> {
    if s.len() != width || !s.bytes().all(|b| b.is_ascii_digit()) {
        return Err(());
    }
    s.parse().map_err(|_| ())
}

/// The provider's mutable state, all behind one lock so the mint stays
/// single-flight (mirrors the Dart fields, `control_token_provider.dart:48-54`;
/// the Dart `_inflight` future dance is subsumed by holding the lock across
/// the mint).
#[derive(Default)]
struct State {
    /// The credential currently held, plus the private key that belongs to it in
    /// mtls state. The key is NEVER part of [`Credential`] (the public snapshot),
    /// so it cannot leak through a clone, a log line, or the FFI surface.
    cached: Option<Credential>,
    keypair: Option<Arc<ClientKeyPair>>,
    /// A 401 was observed for the cached token (`invalidate*`) — the next
    /// `token()` must mint and may never return the rejected token
    /// (`control_token_provider.dart:72-79`).
    must_mint: bool,
    /// Unix second before which no mint attempt is made (set by a mint
    /// failure; `control_token_provider.dart:52,147`).
    cooldown_until: u64,
    /// The shape most recently ANNOUNCED through `mode_changed`, kept separate
    /// from `cached` so the derived event describes a real transition.
    ///
    /// It must survive an invalidation: `reject_cached` drops the credential, so
    /// deriving the transition from `cached` alone would report a "flip" for every
    /// post-401 re-mint that landed on the same shape — a rotation dressed up as a
    /// mode change, which is exactly the event a consumer acts on by rewriting its
    /// stored entry. Seeded credentials pre-set it ([`ControlTokenProvider::with_seed`]):
    /// the consumer that supplied the seed already knows that shape.
    last_announced_mode: Option<AuthMode>,
    /// Presence of a recorded mint failure (`_lastError`,
    /// `control_token_provider.dart:53,148`), surfaced when a later call must
    /// error without attempting a mint (cooldown). Always the fixed
    /// [`MINT_FAILED_REDACTED`] text, never the minter's Display output — see
    /// that const's doc.
    last_error: Option<String>,
}

impl State {
    /// The cached credential was rejected (a 401, or a TLS alert naming our
    /// certificate): drop it — key and all — and force the next `credential()` to
    /// mint, never returning the rejected value
    /// (`control_token_provider.dart:72-79`). The shared tail of every
    /// `invalidate*` variant.
    fn reject_cached(&mut self) {
        self.cached = None;
        self.keypair = None;
        self.must_mint = true;
    }
}

/// Caches a control token, refreshing when missing or within the refresh window
/// of expiry. Concurrent `token()` callers serialize on the mint (single-flight:
/// a late caller re-checks the cache under the lock and returns the fresh token
/// rather than minting again).
///
/// Transport-identity binding is deliberately NOT ported from the Dart
/// provider (plan 001 §3.4): a shed-core `Client` + provider pair is immutable
/// per transport identity — the app layer constructs a NEW `Client` when the
/// host/port/pin changes, which deletes the identity-race class instead of
/// managing it. Recorded as a Phase B invariant.
///
/// Cancellation caveat (accepted limitation — C6 adversarial review #3, plan
/// §9): because the lock is held ACROSS the mint, cancelling a `token()`
/// future from outside (e.g. `Client::rc_events` bounds bearer resolution
/// with its connect timeout) can drop the very future that holds the lock
/// mid-mint. The lock frees on drop, but a remote mint side effect (host
/// agent request, SSH mint) may still complete out-of-band, so the caller's
/// immediate retry can start a SECOND concurrent mint. For this crate's
/// minters a rare double-mint costs one extra round trip plus one abandoned
/// short-TTL token — never a lockout (cooldown state mutates only when a mint
/// COMPLETES as a failure) and never a wrong cached token (the cancelled
/// mint's result is dropped, not cached). The upgrade, if it ever matters, is
/// provider-owned in-flight mint state joined by waiters off the lock —
/// shed-broker's watch-based single-flight (`shed-broker/src/minter.rs`,
/// module docs) is the in-repo pattern.
pub struct ControlTokenProvider {
    server: String,
    minter: Arc<dyn TokenMinter>,
    /// The clock, unix SECONDS (an `Arc<dyn Fn>` so tests can drive advancing
    /// time — a bare `fn` pointer can't capture per-test mutable state).
    now_unix: Arc<dyn Fn() -> u64 + Send + Sync>,
    refresh_window: Duration,
    /// Deterministic per-name refresh jitter, derived ONCE from the server
    /// name by [`Self::with_name_jitter`]. Subtracted from the refresh
    /// threshold, so refresh happens EARLIER — a fleet of providers with the
    /// same expiry de-synchronizes instead of thundering the minter together.
    jitter: Duration,
    /// Cooldown started by a mint failure; while it runs, `token()` makes no
    /// mint attempt (returns the cached still-valid token, or the last error).
    mint_cooldown: Duration,
    /// The handshake-time client-certificate source installed on the transport
    /// this provider backs. Created here, handed to the `Client` at build time,
    /// and rewritten on every mint — the seam that makes rotation and mode flips
    /// invisible to the `reqwest::Client` (plan 001 D5).
    cert_resolver: Arc<ClientCertResolver>,
    /// The observer delivery seam ([`CredentialEvents`]) — `None` until an
    /// observer is registered, so an unobserved provider allocates no channel and
    /// starts no thread.
    events: Option<CredentialEvents>,
    state: Mutex<State>,
}

impl ControlTokenProvider {
    pub fn new(server: String, minter: Arc<dyn TokenMinter>) -> Self {
        Self {
            server,
            minter,
            now_unix: Arc::new(default_now_unix),
            refresh_window: REFRESH_WINDOW,
            jitter: Duration::ZERO,
            mint_cooldown: Duration::ZERO,
            cert_resolver: Arc::new(ClientCertResolver::new()),
            events: None,
            state: Mutex::new(State::default()),
        }
    }

    /// The client-certificate resolver to install on this provider's transport.
    ///
    /// [`crate::http::Client`] takes it at build time and never looks at it again:
    /// every later credential change is a write THROUGH this handle, which is what
    /// keeps the `reqwest::Client` immutable across rotations and mode flips.
    pub fn cert_resolver(&self) -> Arc<ClientCertResolver> {
        self.cert_resolver.clone()
    }

    /// Observe this provider's adoptions — `credential_adopted` (every successful
    /// mint) and the derived `mode_changed` (plan 002 §7 P1 / 001 D5).
    ///
    /// Starts the dispatcher thread that owns `observer` and delivers to it; see
    /// [`CredentialObserver`]'s delivery contract for what a handler may and may
    /// not assume. Calling this twice replaces the observer — the previous
    /// dispatcher's channel drops and its thread ends.
    pub fn with_observer(mut self, observer: Arc<dyn CredentialObserver>) -> Self {
        self.events = CredentialEvents::start(observer);
        self
    }

    /// Replace the clock (unix seconds). Public, not `#[cfg(test)]`: the seam
    /// must be reachable from shed-app tests and the Phase B bridge (the same
    /// public-builder convention as `SseParser::with_max_event_bytes`).
    pub fn with_now(mut self, now: impl Fn() -> u64 + Send + Sync + 'static) -> Self {
        self.now_unix = Arc::new(now);
        self
    }

    /// Refresh this long before expiry (default [`REFRESH_WINDOW`], 2h —
    /// unchanged desktop behavior; mobile passes 2h5m).
    pub fn with_refresh_window(mut self, window: Duration) -> Self {
        self.refresh_window = window;
        self
    }

    /// Enable deterministic refresh jitter in `[0, max)`: the value is derived
    /// ONCE from the server name via [`name_jitter`] (Dart parity —
    /// `control_token_provider.dart:39`, `nameJitter(name, jitterMs)`) and
    /// SUBTRACTED from the refresh threshold, so this provider refreshes up to
    /// `max` earlier than an un-jittered one. Default off (zero jitter).
    ///
    /// [`name_jitter`] works in the Dart algorithm's millisecond domain for
    /// cross-language stability; the provider's clock is unix seconds, so the
    /// derived jitter truncates to whole seconds here (sub-second precision is
    /// noise against mobile's 5-minute max).
    pub fn with_name_jitter(mut self, max: Duration) -> Self {
        self.jitter = Duration::from_millis(name_jitter(&self.server, max.as_millis() as u64));
        self
    }

    /// After a mint failure, make no further mint attempt until `cooldown` has
    /// elapsed — a polling caller can't storm a host with doomed mints
    /// (`control_token_provider.dart:107-123`). During the cooldown `token()`
    /// returns the cached still-valid token if one exists, else the recorded
    /// last error. Default `0` = off (every call may attempt a mint).
    pub fn with_mint_cooldown(mut self, cooldown: Duration) -> Self {
        self.mint_cooldown = cooldown;
        self
    }

    /// Pre-populate the cache with a persisted token (mobile's config seed,
    /// `control_token_provider.dart:153-157`) so the first `token()` can skip
    /// the mint. An empty/whitespace token is IGNORED — an unusable credential
    /// is never cached (plan 001 §3.4).
    pub fn with_seed(mut self, seed: MintedToken) -> Self {
        if !seed.token.trim().is_empty() {
            let st = self.state.get_mut();
            st.cached = Some(Credential {
                mode: Some(AuthMode::Token),
                token: seed.token,
                expires_at_unix: seed.expires_at_unix,
                ..Credential::default()
            });
            // The consumer that persisted this seed already knows the shape, so a
            // first mint that stays in token mode is a rotation, not a transition.
            st.last_announced_mode = Some(AuthMode::Token);
        }
        self
    }

    /// The current token, minting/refreshing when it is missing or near expiry.
    /// FSM ported from Dart's `get()` (`control_token_provider.dart:57-97`),
    /// minus the identity-binding and legacy-host branches (see the type-level
    /// docs / the test-module note):
    ///   * after an `invalidate*` the mint is FORCED — on failure this errors,
    ///     never returning the rejected token (`:72-79`)
    ///   * a still-valid cached token inside the refresh window mints
    ///     proactively but KEEPS the cached token when that mint fails
    ///     (`:81-92`)
    ///   * missing/expired → mint; a failure here is fail-closed (the caller
    ///     then sends no token) (`:94-96`)
    pub async fn token(&self) -> Result<String, ShedError> {
        let cred = self.credential().await?;
        match cred.mode {
            // Fail LOUD rather than returning an empty string: a caller reaching
            // for a bearer while the server issues certificates is asking the
            // wrong question, and silently sending no header would surface as an
            // unexplained 401 much further away.
            Some(AuthMode::Mtls) => Err(ShedError::Config(
                "this server issues client certificates; there is no bearer token".into(),
            )),
            _ => Ok(cred.token),
        }
    }

    /// The current credential, minting/refreshing when it is missing, unusable,
    /// or near expiry. The generalization of [`Self::token`] — same FSM, either
    /// credential shape (plan 001 D5).
    ///
    /// The "unusable" trigger is not a corner case (Go parity, `4553cc8`): a
    /// provider that holds NOTHING must mint before the first request, not fail
    /// it. Expiry cannot express that state — a credential you never had has no
    /// expiry to be near — so [`Credential::usable`] is checked separately.
    pub async fn credential(&self) -> Result<Credential, ShedError> {
        // Hold the lock across the mint so concurrent callers serialize: the
        // first mints, the rest re-check here and return its result.
        let mut st = self.state.lock().await;
        let now = (self.now_unix)();

        // Reactive: a prior rejection means the current credential is refused —
        // mint, and do not fall back to it.
        if st.must_mint {
            return match self.mint_locked(&mut st, now).await {
                Ok(minted) => {
                    st.must_mint = false;
                    Ok(minted)
                }
                Err(e) => Err(e),
            };
        }

        if let Some(current) = st.cached.clone() {
            if current.usable() && !expired(&current, now) {
                if self.needs_refresh(&current, now) {
                    match self.mint_locked(&mut st, now).await {
                        Ok(minted) => return Ok(minted),
                        Err(e) => {
                            // Keep-valid-on-proactive-failure (strict
                            // improvement #1, `control_token_provider.dart:82-92`):
                            // the refresh mint failed — normally fall through and
                            // return the cached token rather than erroring. BUT
                            // `now` was read before awaiting the minter: a slow
                            // mint can finish AFTER the cached token expired, and
                            // returning it then would hand back a dead credential.
                            // Re-read the clock and only keep the token if it is
                            // STILL valid against the fresh now; otherwise fail
                            // closed with the real mint error.
                            let fresh_now = (self.now_unix)();
                            if expired(&current, fresh_now) {
                                return Err(e);
                            }
                        }
                    }
                }
                return Ok(current);
            }
        }

        self.mint_locked(&mut st, now).await
    }

    /// The credential still HELD right now, without minting anything — the
    /// "surviving credential" a caller presents when [`Self::credential`] failed
    /// (Go parity: `clienttoken.Source.Current`, which `sdk` `setAuth` and
    /// `cmd/shed`'s `pinCredential` read after a failed mint, `66abaa9`).
    ///
    /// A failed mint says nothing about what is already held: `mint_locked`
    /// deliberately leaves the cache alone, so a client whose refresh/enrollment
    /// just failed can still present what it has — the server may well accept it,
    /// and presenting something beats presenting nothing. `None` means there is
    /// genuinely nothing to send, which is what makes the mint error the
    /// request's error rather than a downstream 401 with no explanation (see
    /// `http::Client::credential`).
    ///
    /// Two properties are deliberate:
    ///
    ///   * it never mints and never blocks on one — it takes the same state lock,
    ///     so a call racing an in-flight mint waits for that mint and then sees
    ///     its result, which is the answer the caller wants anyway;
    ///   * a REJECTED credential does not survive here. Go's `Source.Reject`
    ///     keeps the refused credential presentable and only marks it; this
    ///     provider's `reject_cached` drops it (Dart parity — "never return the
    ///     rejected token", pinned by
    ///     `must_mint_failure_errs_even_with_an_unexpired_cached_token`). So on
    ///     this side "surviving" means held-and-not-refused: the stale/expired
    ///     cache a failed refresh left behind, or a credential a concurrent
    ///     caller minted in the meantime.
    ///
    /// Unusable shapes ([`Credential::usable`]) are filtered out: a credential
    /// with no payload is indistinguishable from having none.
    pub async fn surviving_credential(&self) -> Option<Credential> {
        let st = self.state.lock().await;
        st.cached.clone().filter(Credential::usable)
    }

    /// Drop the cached token UNCONDITIONALLY so the next `token()` force-mints.
    /// Kept for back-compat; prefer [`Self::invalidate_if_current`] when the
    /// rejected token is known — this variant can erase a NEWER token when a
    /// stale 401 races a rotation.
    pub async fn invalidate(&self) {
        let mut st = self.state.lock().await;
        self.reject_locked(&mut st);
    }

    /// Stale-401 guard (strict improvement #2,
    /// `control_token_provider.dart:99-105`): drop the cache ONLY if the
    /// cached token is `observed` (the one the 401 rejected) — a 401 for a
    /// token already rotated past is ignored. Sets the must-mint flag so the
    /// next `token()` force-mints and never returns the rejected token.
    pub async fn invalidate_if_current(&self, observed: &str) {
        let mut st = self.state.lock().await;
        if let Some(c) = &st.cached {
            if c.identity() != observed {
                return; // stale 401 — the rejected token is already gone
            }
        }
        self.reject_locked(&mut st);
    }

    /// The credential-shaped [`Self::invalidate_if_current`]: drop the cache
    /// because a request that carried `observed` was refused.
    ///
    /// # Why the mtls case is attribution-FREE
    ///
    /// In token state the guard is exact: the caller pinned the bearer into the
    /// request's own header, so "was the rejected credential still the current
    /// one" is answerable and a 401 for an already-rotated token is correctly
    /// ignored.
    ///
    /// A certificate cannot be attributed that way. `reqwest` offers no
    /// per-request TLS control: the transport reads the LIVE resolver during the
    /// handshake, so a request that recorded credential N can transmit N+1 if a
    /// mint lands in between (and, worse, a POOLED connection presents whatever
    /// was current when it was dialed — which may be neither). Applying the
    /// identity guard there produces the exact failure it was meant to prevent:
    /// the rejection of N+1 is dismissed as "stale, we already hold N+1", nothing
    /// is invalidated, and the retry re-sends the credential the server just
    /// refused. So in mtls state a refusal invalidates whatever is CURRENT,
    /// unconditionally.
    ///
    /// The cost is bounded and small: at worst one extra mint, because the mint
    /// is single-flighted under this same lock. The alternative — having the
    /// resolver record what it last handed out — was evaluated and rejected: it
    /// records the last HANDSHAKE, not the one this response came back on, so it
    /// answers a different question while looking authoritative.
    pub async fn invalidate_if_current_credential(&self, observed: &Credential) {
        let mut st = self.state.lock().await;
        let attributable = !matches!(
            st.cached.as_ref().and_then(|c| c.mode),
            Some(AuthMode::Mtls)
        );
        if attributable {
            if let Some(c) = &st.cached {
                if !observed.identity().is_empty() && c.identity() != observed.identity() {
                    return; // stale failure — already rotated past
                }
            }
        }
        self.reject_locked(&mut st);
    }

    /// Reject the cached credential and withdraw it from the transport, both
    /// under the caller's ALREADY-HELD state lock.
    ///
    /// The ordering is the point. These are two writes to two different cells,
    /// and the state lock is what serializes every other mutation of the pair
    /// (`mint_locked` installs a credential and its certificate under it). An
    /// invalidation that dropped the lock first — as this code used to — leaves a
    /// window in which a concurrent mint can acquire the lock, install credential
    /// N+1, and publish its certificate, only for the older invalidation to then
    /// clear the resolver: the provider believes it holds N+1 and hands out its
    /// bearer/serial while the transport presents nothing. Taking `&mut State`
    /// rather than `&self` is deliberate — the caller cannot invoke this without
    /// the guard in hand.
    fn reject_locked(&self, st: &mut State) {
        st.reject_cached();
        self.cert_resolver.set(None);
    }

    /// One mint attempt under the held state lock (the single-flight point),
    /// with the failure cooldown — Dart's `_mint` + `_doMint`
    /// (`control_token_provider.dart:107-151`): inside the cooldown no attempt
    /// is made (the recorded redacted error is returned, state untouched); a
    /// success replaces the cache and clears the last error; a failure starts
    /// the cooldown, records the fixed [`MINT_FAILED_REDACTED`] marker (never
    /// the minter's Display text — it can embed secrets), returns the REAL
    /// error to this immediate caller, and leaves the cache alone (an
    /// expired-but-cached token stays for a later retry; a still-valid one
    /// backs keep-valid). An empty minted token is a failure, not a usable
    /// credential (fail-closed), even if the minter reported success.
    async fn mint_locked(&self, st: &mut State, now: u64) -> Result<Credential, ShedError> {
        if now < st.cooldown_until {
            return Err(last_error(st));
        }
        // A fresh keypair per mint keeps key lifetime == certificate lifetime
        // (plan 001 D5). Generated only for a minter that can actually carry a
        // CSR, so token-only minters pay nothing.
        let keypair = if self.minter.supports_mtls() {
            Some(Arc::new(ClientKeyPair::generate()?))
        } else {
            None
        };
        let req = CredentialRequest {
            csr_base64: keypair.as_ref().map(|k| k.csr_base64()),
        };
        let outcome = self
            .minter
            .mint_credential(&self.server, &req)
            .await
            .and_then(|minted| self.adopt(minted, keypair.as_ref()));
        match outcome {
            Ok((cred, keypair, certified)) => {
                let prior_mode = st.last_announced_mode;
                st.cached = Some(cred.clone());
                st.keypair = keypair;
                st.last_error = None;
                st.last_announced_mode = cred.mode;
                // Present (mtls) or withdraw (token) the certificate BEFORE
                // returning: the caller's very next request may handshake.
                self.cert_resolver.set(certified);
                self.emit_adopted(&cred, prior_mode);
                Ok(cred)
            }
            Err(e) => {
                // Fresh clock read for the cooldown start (Dart `_doMint`
                // re-reads `_now()`, `:147` — the mint itself took time).
                st.cooldown_until = (self.now_unix)() + self.mint_cooldown.as_secs();
                st.last_error = Some(MINT_FAILED_REDACTED.into());
                Err(e)
            }
        }
    }

    /// Queue the `credential_adopted` notification for a credential just adopted,
    /// plus the derived `mode_changed` when `prior_mode` — the shape last
    /// ANNOUNCED, not the one last cached — differed.
    ///
    /// Called from [`Self::mint_locked`], i.e. with the state lock HELD — but only
    /// the ENQUEUE happens here (a channel send, O(1), no foreign code). Every
    /// handler runs on the dispatcher thread with no lock held, which is what makes
    /// a blocking or re-entrant observer harmless (plan 002 §7 P1).
    ///
    /// Emitting under the lock rather than after it is deliberate: the lock is what
    /// serializes adoptions, so enqueueing inside it is what guarantees the queue's
    /// order IS the adoption order. Post-lock emission would let two mints reorder.
    fn emit_adopted(&self, cred: &Credential, prior_mode: Option<AuthMode>) {
        let (Some(events), Some(mode)) = (&self.events, cred.mode) else {
            return; // no observer, or (unreachable) an adopted credential with no mode
        };
        events.emit(Emission {
            event: CredentialAdopted {
                server: self.server.clone(),
                mode,
                expires_at_unix: cred.expires_at_unix,
                // Token mode only — see [`CredentialAdopted`]'s doc for why the
                // mtls event carries no credential material at all.
                token: match mode {
                    AuthMode::Token => Some(cred.token.clone()),
                    AuthMode::Mtls => None,
                },
            },
            mode_changed: prior_mode != Some(mode),
        });
    }

    /// Validate a freshly minted credential and turn it into the triple the state
    /// machine stores: the public snapshot, the private key it belongs to, and the
    /// rustls identity to present.
    ///
    /// Fail-closed on every shape that cannot authenticate:
    ///   * an empty token (a minter reporting success with nothing to send);
    ///   * a certificate when this mint carried NO CSR — that credential's key
    ///     lives in another process (or nowhere), so it could never be presented;
    ///   * a certificate that does not match the key we generated for the CSR
    ///     (caught by [`certified_key_from_pem`], which verifies the pairing).
    #[allow(clippy::type_complexity)]
    fn adopt(
        &self,
        minted: MintedCredential,
        keypair: Option<&Arc<ClientKeyPair>>,
    ) -> Result<
        (
            Credential,
            Option<Arc<ClientKeyPair>>,
            Option<Arc<CertifiedKey>>,
        ),
        ShedError,
    > {
        match minted {
            MintedCredential::Token(t) if t.token.is_empty() => Err(ShedError::Transport(
                "control-token mint returned an empty token".into(),
            )),
            MintedCredential::Token(t) => Ok((
                Credential {
                    mode: Some(AuthMode::Token),
                    token: t.token,
                    expires_at_unix: t.expires_at_unix,
                    ..Credential::default()
                },
                None,
                None,
            )),
            MintedCredential::Certificate(c) => {
                let keypair = keypair.ok_or_else(|| {
                    ShedError::Transport(
                        "mint returned a client certificate for a request that carried no CSR"
                            .into(),
                    )
                })?;
                let certified = certified_key_from_pem(&c.cert_pem, keypair.key_pkcs8_der())
                    .map_err(|e| {
                        ShedError::Transport(format!("minted client certificate unusable: {e}"))
                    })?;
                Ok((
                    Credential {
                        mode: Some(AuthMode::Mtls),
                        cert_pem: c.cert_pem,
                        cert_serial: c.serial,
                        expires_at_unix: c.expires_at_unix,
                        ..Credential::default()
                    },
                    Some(keypair.clone()),
                    Some(Arc::new(certified)),
                ))
            }
        }
    }

    /// Within the refresh window of expiry: `now >= exp - window - jitter`
    /// (`control_token_provider.dart:162-166`; the jitter subtraction makes
    /// refresh earlier). No expiry → never (only `invalidate*` refreshes).
    /// `saturating_sub` matches Dart's signed math: a threshold below zero
    /// means "always refresh".
    fn needs_refresh(&self, t: &Credential, now: u64) -> bool {
        match t.expires_at_unix {
            None => false,
            Some(exp) => {
                now >= exp.saturating_sub(self.refresh_window.as_secs() + self.jitter.as_secs())
            }
        }
    }
}

/// Past expiry (`control_token_provider.dart:159-160`). No expiry → never.
fn expired(t: &Credential, now: u64) -> bool {
    matches!(t.expires_at_unix, Some(exp) if now >= exp)
}

/// The error for a `token()` call blocked from minting (cooldown): the
/// recorded redacted marker, or a generic fail-closed message (Dart's
/// `_lastError ?? AppError.authExpired()`, `control_token_provider.dart:78,96`
/// — except the persisted text is [`MINT_FAILED_REDACTED`], never the original
/// minter error).
fn last_error(st: &State) -> ShedError {
    ShedError::Transport(
        st.last_error
            .clone()
            .unwrap_or_else(|| "control token expired and mint unavailable".into()),
    )
}

fn default_now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};

    // Port coverage of mobile's `control_token_provider_test.dart:28-252`
    // (plan 001 §7 AC#4). NOT ported (§3.4 decision, see the type docs): the
    // two identity-binding cases (`drops the cached token when the host
    // transport identity changes`, `does not hand an in-flight mint to a
    // changed identity`) — a shed-core Client+provider pair is immutable per
    // transport identity; the app layer constructs a NEW Client (and provider)
    // on a host/port/pin change, deleting the identity-race class those cases
    // guard. TRANSLATED rather than ported: `returns null for a legacy
    // (non-secure) host` — Rust represents "legacy" at Client construction (a
    // minter-less Client resolves its bearer from the static token or sends
    // none; http.rs `static_token_used_without_provider` +
    // `mint_failure_is_fail_closed_no_downgrade` pin that contract), not as a
    // provider state.

    /// A minter that counts calls and returns `tok-<n>` (or fails — the
    /// switch is runtime-flippable, the Dart tests' `shouldFail` captures; a
    /// FAILED attempt is counted in `n` too). Optional expiry lets a test
    /// force the refresh-window path; a delay lets one force single-flight.
    struct MockMinter {
        calls: AtomicUsize,
        fail: AtomicBool,
        expires_at_unix: Option<u64>,
        delay_ms: u64,
    }

    impl MockMinter {
        fn new(fail: bool, expires_at_unix: Option<u64>, delay_ms: u64) -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: AtomicBool::new(fail),
                expires_at_unix,
                delay_ms,
            })
        }
        fn ok() -> Arc<Self> {
            Self::new(false, None, 0)
        }
        fn failing() -> Arc<Self> {
            Self::new(true, None, 0)
        }
        /// The Dart FSM tests' minter shape: flippable failure, far-future
        /// expiry.
        fn flaky(fail: bool) -> Arc<Self> {
            Self::new(fail, Some(FAR_FUTURE), 0)
        }
        fn set_fail(&self, v: bool) {
            self.fail.store(v, Ordering::SeqCst);
        }
        fn count(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for MockMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            if self.delay_ms > 0 {
                tokio::time::sleep(Duration::from_millis(self.delay_ms)).await;
            }
            if self.fail.load(Ordering::SeqCst) {
                return Err(ShedError::Transport("mint failed".into()));
            }
            Ok(MintedToken {
                token: format!("tok-{n}"),
                expires_at_unix: self.expires_at_unix,
            })
        }
    }

    /// Dart's `farFuture` (99999999 ms there; unix seconds here — only "far
    /// beyond every test clock" matters).
    const FAR_FUTURE: u64 = 99_999_999;

    fn seed(exp: u64) -> MintedToken {
        MintedToken {
            token: "seed".into(),
            expires_at_unix: Some(exp),
        }
    }

    #[tokio::test]
    async fn caches_a_no_expiry_token() {
        let minter = MockMinter::ok();
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-1"); // cached, no re-mint
        assert_eq!(minter.count(), 1);
    }

    #[tokio::test]
    async fn invalidate_forces_remint() {
        let minter = MockMinter::ok();
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        p.invalidate().await;
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn refreshes_within_expiry_window() {
        // Expiry = now → expired → re-mint each call.
        let now = default_now_unix();
        let minter = MockMinter::new(false, Some(now), 0);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn does_not_refresh_far_from_expiry() {
        let far = default_now_unix() + 10 * 60 * 60; // 10h out, beyond the 2h window
        let minter = MockMinter::new(false, Some(far), 0);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-1"); // still fresh
        assert_eq!(minter.count(), 1);
    }

    #[tokio::test]
    async fn mint_failure_is_fail_closed() {
        let minter = MockMinter::failing();
        let p = ControlTokenProvider::new("mini2".into(), minter);
        assert!(p.token().await.is_err()); // caller then sends NO token
    }

    #[tokio::test]
    async fn empty_minted_token_is_fail_closed() {
        struct EmptyMinter;
        #[async_trait::async_trait]
        impl TokenMinter for EmptyMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                Ok(MintedToken {
                    token: String::new(),
                    expires_at_unix: None,
                })
            }
        }
        let p = ControlTokenProvider::new("mini2".into(), Arc::new(EmptyMinter));
        assert!(p.token().await.is_err()); // empty token → mint failure, not cached
    }

    // Also the port of Dart's `collapses concurrent mints into one
    // (single-flight)` (`control_token_provider_test.dart:114-134`): no seed →
    // every caller must mint, and the held lock collapses them to one.
    #[tokio::test]
    async fn concurrent_callers_mint_once() {
        // Single-flight: a slow mint + concurrent callers → exactly one mint.
        let minter = MockMinter::new(false, None, 40);
        let p = Arc::new(ControlTokenProvider::new("mini2".into(), minter.clone()));
        let (a, b, c) = tokio::join!(p.token(), p.token(), p.token());
        assert_eq!(a.unwrap(), "tok-1");
        assert_eq!(b.unwrap(), "tok-1");
        assert_eq!(c.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // ---- mobile FSM ports (control_token_provider_test.dart:28-252) ----

    // Dart `uses the config seed without minting when it is fresh` (:47-62).
    #[tokio::test]
    async fn uses_the_seed_without_minting_when_fresh() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1000))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 0);
    }

    // Dart `proactively mints when within the refresh window` (:64-79).
    #[tokio::test]
    async fn proactively_mints_within_the_refresh_window() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 9000)
            .with_refresh_window(Duration::from_secs(5000))
            .with_seed(seed(10_000));
        // 9000 >= 10000 - 5000 → inside the window (not yet expired) → mint.
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // Dart `keeps a still-valid token when a proactive mint fails` (:81-91) —
    // strict improvement #1 over the pre-C6 Rust FSM, which errored here.
    #[tokio::test]
    async fn keeps_a_still_valid_token_when_a_proactive_mint_fails() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 9000)
            .with_refresh_window(Duration::from_secs(5000))
            .with_seed(seed(10_000));
        assert_eq!(p.token().await.unwrap(), "seed"); // kept, not an error
        assert_eq!(minter.count(), 1); // the refresh mint WAS attempted
    }

    // Finding 4 (fail-closed correctness): `now` is captured BEFORE awaiting the
    // minter, so a SLOW proactive mint can finish after the cached token has
    // expired. Keep-valid must re-check the clock and NOT serve a now-expired
    // token. A minter that advances the shared clock while it runs models the
    // slow mint; the two sub-cases are (a) clock crosses expiry → Err, and (b)
    // clock stays before expiry → the still-valid cached token.
    #[tokio::test]
    async fn proactive_mint_failure_fails_closed_when_the_clock_crossed_expiry() {
        // A failing minter that, mid-mint, advances the shared clock to a target
        // (simulating wall-clock elapsing during a slow doomed mint).
        struct SlowFailMinter {
            clock: Arc<AtomicU64>,
            advance_to: u64,
        }
        #[async_trait::async_trait]
        impl TokenMinter for SlowFailMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                self.clock.store(self.advance_to, Ordering::SeqCst);
                Err(ShedError::Transport("mint failed".into()))
            }
        }

        // (a) clock advances 900 -> 1500, past the 1000 expiry → fail closed.
        let clock = Arc::new(AtomicU64::new(900));
        let p = ControlTokenProvider::new(
            "mini3".into(),
            Arc::new(SlowFailMinter {
                clock: clock.clone(),
                advance_to: 1500,
            }),
        )
        .with_now({
            let clock = clock.clone();
            move || clock.load(Ordering::SeqCst)
        })
        .with_refresh_window(Duration::from_secs(5000)) // 900 >= 1000-5000 → in window
        .with_seed(seed(1000));
        // In the refresh window, mint attempted, fails, and by the time it
        // returns the clock reads 1500 >= 1000 → the cached token is now expired.
        assert!(
            p.token().await.is_err(),
            "must not return the now-expired cached token"
        );
        assert_eq!(clock.load(Ordering::SeqCst), 1500);

        // (b) clock stays at 900 (mint fails without wall-clock crossing expiry)
        // → the still-valid cached token is kept.
        let clock = Arc::new(AtomicU64::new(900));
        let p = ControlTokenProvider::new(
            "mini3".into(),
            Arc::new(SlowFailMinter {
                clock: clock.clone(),
                advance_to: 900, // no advance past expiry
            }),
        )
        .with_now({
            let clock = clock.clone();
            move || clock.load(Ordering::SeqCst)
        })
        .with_refresh_window(Duration::from_secs(5000))
        .with_seed(seed(1000));
        assert_eq!(p.token().await.unwrap(), "seed"); // still valid → kept
    }

    // Dart `mints when the seed is expired, throwing if that mint fails`
    // (:93-112). Cooldown 0 (the default) so the immediate retry may mint.
    #[tokio::test]
    async fn expired_seed_mints_and_errs_when_that_mint_fails() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 5000)
            .with_seed(seed(1000)); // expired
        assert!(p.token().await.is_err());
        minter.set_fail(false);
        assert_eq!(p.token().await.unwrap(), "tok-2"); // attempt 1 failed
        assert_eq!(minter.count(), 2);
    }

    // Dart `forces a fresh mint on invalidate (401), never the rejected token`
    // (:136-155).
    #[tokio::test]
    async fn invalidate_if_current_forces_a_fresh_mint_never_the_rejected_token() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        p.invalidate_if_current("seed").await;
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // The must-mint failure half of Dart's `get()` reactive branch (:72-79):
    // after an invalidate, a failed mint is an ERROR — the still-valid (but
    // rejected) seed is never handed back.
    #[tokio::test]
    async fn must_mint_failure_errs_even_with_an_unexpired_cached_token() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed"); // far from expiry: no mint
        assert_eq!(minter.count(), 0);
        p.invalidate_if_current("seed").await;
        assert!(p.token().await.is_err()); // forced mint failed → Err, not "seed"
        assert_eq!(minter.count(), 1);
    }

    // Dart `does not re-mint within the cooldown after a failed mint`
    // (:157-179). Dart drives ms; the Rust clock is unix seconds — same
    // numbers, seconds domain.
    #[tokio::test]
    async fn does_not_remint_within_the_cooldown_after_a_failed_mint() {
        let now = Arc::new(AtomicU64::new(5000));
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(1000)); // expired
        assert!(p.token().await.is_err()); // fails; cooldown until 6000
        assert_eq!(minter.count(), 1);
        now.store(5500, Ordering::SeqCst); // within cooldown → NO mint attempt
        assert!(p.token().await.is_err());
        assert_eq!(minter.count(), 1);
        now.store(6000, Ordering::SeqCst); // cooldown elapsed → mints again
        assert!(p.token().await.is_err());
        assert_eq!(minter.count(), 2);
    }

    // The cooldown's still-valid half (plan §3.4): during a cooldown a cached
    // still-valid token is returned WITHOUT a mint attempt.
    #[tokio::test]
    async fn cooldown_returns_the_cached_still_valid_token_without_minting() {
        let now = Arc::new(AtomicU64::new(9000));
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_refresh_window(Duration::from_secs(5000))
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(10_000));
        // In the refresh window: the mint fails (keep-valid) + starts cooldown.
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 1);
        now.store(9100, Ordering::SeqCst); // still in window, cooldown active
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 1); // no second attempt
    }

    // C6 adversarial review #2b: the PERSISTED last_error (served on later
    // cooldown-blocked calls) is the fixed redacted marker — a minter error
    // embedding secret material must never be stored and replayed. The
    // immediate caller of the failing mint still receives the real error
    // (pre-C6 propagation parity; Dart rethrows too, :149).
    #[tokio::test]
    async fn cooldown_blocked_error_is_redacted_never_the_minter_text() {
        struct SecretErrMinter;
        #[async_trait::async_trait]
        impl TokenMinter for SecretErrMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                Err(ShedError::Transport(
                    "mint blew up: SECRET-TOKEN-MATERIAL".into(),
                ))
            }
        }
        let now = Arc::new(AtomicU64::new(5000));
        let p = ControlTokenProvider::new("mini3".into(), Arc::new(SecretErrMinter))
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(1000)); // expired seed → the mint is forced
        let immediate = p.token().await.unwrap_err().to_string();
        // The immediate failure surfaces the minter's real error.
        assert!(
            immediate.contains("SECRET-TOKEN-MATERIAL"),
            "got: {immediate}"
        );
        // A later cooldown-blocked call serves only the redacted marker.
        now.store(5500, Ordering::SeqCst);
        let blocked = p.token().await.unwrap_err().to_string();
        assert!(
            !blocked.contains("SECRET"),
            "persisted error leaked: {blocked}"
        );
        assert!(
            blocked.contains("control token mint failed"),
            "got: {blocked}"
        );
    }

    // Dart `ignores a stale 401 for a token it has already rotated past`
    // (:181-203) — strict improvement #2.
    #[tokio::test]
    async fn ignores_a_stale_401_for_an_already_rotated_token() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        p.invalidate_if_current("seed").await; // real 401 on the seed
        assert_eq!(p.token().await.unwrap(), "tok-1"); // rotated
        p.invalidate_if_current("seed").await; // stale 401 → ignored
        assert_eq!(p.token().await.unwrap(), "tok-1"); // still cached
        assert_eq!(minter.count(), 1);
    }

    // Plan §3.4: `with_seed` ignores an empty/whitespace token — never caches
    // an unusable credential; the first token() mints as if unseeded.
    #[tokio::test]
    async fn with_seed_ignores_empty_and_whitespace_tokens() {
        for junk in ["", "   \t "] {
            let minter = MockMinter::flaky(false);
            let p = ControlTokenProvider::new("mini3".into(), minter.clone())
                .with_now(|| 0)
                .with_seed(MintedToken {
                    token: junk.into(),
                    expires_at_unix: Some(FAR_FUTURE),
                });
            assert_eq!(p.token().await.unwrap(), "tok-1", "seed {junk:?}");
            assert_eq!(minter.count(), 1);
        }
    }

    // Jitter is SUBTRACTED from the refresh threshold — refresh happens
    // EARLIER. name_jitter("my-server", 300000 ms) = 145668 ms → 145 s in the
    // provider's seconds-domain comparison. exp = 100000, window = 1000 s:
    // threshold without jitter = 99000, with = 98855. At now = 98900 only the
    // jittered provider refreshes.
    #[tokio::test]
    async fn name_jitter_is_subtracted_so_refresh_happens_earlier() {
        let jittered_minter = MockMinter::flaky(false);
        let jittered = ControlTokenProvider::new("my-server".into(), jittered_minter.clone())
            .with_now(|| 98_900)
            .with_refresh_window(Duration::from_secs(1000))
            .with_name_jitter(Duration::from_secs(300))
            .with_seed(seed(100_000));
        assert_eq!(jittered.token().await.unwrap(), "tok-1");
        assert_eq!(jittered_minter.count(), 1);

        let plain_minter = MockMinter::flaky(false);
        let plain = ControlTokenProvider::new("my-server".into(), plain_minter.clone())
            .with_now(|| 98_900)
            .with_refresh_window(Duration::from_secs(1000))
            .with_seed(seed(100_000));
        assert_eq!(plain.token().await.unwrap(), "seed");
        assert_eq!(plain_minter.count(), 0);
    }

    // Cross-language vectors for [`name_jitter`] (plan §3.4/AC#5): expected
    // values derived by running the Dart algorithm
    // (`control_token_provider.dart:175-181`) by hand — per UTF-16 code unit,
    // h = (h*31 + cu).toSigned(32), then h.abs() % max(maxMs, 1).
    #[test]
    fn name_jitter_matches_the_dart_algorithm() {
        // "my-server" (ASCII), code units m=109 y=121 -=45 s=115 e=101 r=114
        // v=118 e=101 r=114:
        //   109 → 3500 → 108545 → 3365010 → 104315411
        //   → 104315411*31+114 = 3233777855   → wraps to -1061189441
        //   → -1061189441*31+118              → wraps to  1462865815
        //   →  1462865815*31+101              → wraps to -1895799890
        //   → -1895799890*31+114              → wraps to  1359745668
        // abs(1359745668) % 300000 = 145668; % 1000 = 668.
        assert_eq!(name_jitter("my-server", 300_000), 145_668);
        assert_eq!(name_jitter("my-server", 1_000), 668);

        // "grüß-server" (non-ASCII, still BMP — ONE code unit each: ü=252
        // ß=223), units g=103 r=114 ü=252 ß=223 -=45 s=115 e=101 r=114 v=118
        // e=101 r=114:
        //   103 → 3307 → 102769 → 3186062 → 98767967
        //   → 98767967*31+115 = 3061807092    → wraps to -1233160204
        //   → -1233160204*31+101              → wraps to   426739441
        //   →   426739441*31+114              → wraps to   344020897
        //   →   344020897*31+118              → wraps to  2074713333
        //   →  2074713333*31+101              → wraps to  -108396016
        //   →  -108396016*31+114              → wraps to   934690914
        // abs(934690914) % 300000 = 190914; % 1000 = 914.
        assert_eq!(name_jitter("grüß-server", 300_000), 190_914);
        assert_eq!(name_jitter("grüß-server", 1_000), 914);

        // "shed-🚀" (emoji = a SURROGATE PAIR, TWO code units — Dart
        // `codeUnitAt` sees the UTF-16 halves of U+1F680: high 0xD83D=55357,
        // low 0xDE80=56960), units s=115 h=104 e=101 d=100 -=45 55357 56960:
        //   115 → 3669 → 113840 → 3529140 → 109403385
        //   → 109403385*31+55357 = 3391560292 → wraps to  -903407004
        //   → -903407004*31+56960             → wraps to  2059210908
        // abs(2059210908) % 300000 = 10908; % 1000 = 908.
        assert_eq!(name_jitter("shed-🚀", 300_000), 10_908);
        assert_eq!(name_jitter("shed-🚀", 1_000), 908);

        // A name whose FINAL hash is negative, exercising unsigned_abs:
        // "my-server-dev" folds to h = -267572916;
        // abs(-267572916) % 300000 = 272916.
        assert_eq!(name_jitter("my-server-dev", 300_000), 272_916);

        // Dart `maxMs < 1 ? 1 : maxMs`: max 0 → % 1 → always 0 (jitter off).
        assert_eq!(name_jitter("my-server", 0), 0);
        assert_eq!(name_jitter("", 300_000), 0); // h stays 0
    }
}

#[cfg(test)]
mod bundle_tests {
    use super::*;
    use serde_json::json;

    // Port of mobile's `token_bundle_test.dart:29-152` (plan 001 §7 AC#5),
    // plus the RFC3339 parser's own unit tests and the documented pre-epoch
    // u64 divergence.

    /// Dart's `bundle()` helper (`token_bundle_test.dart:17-27`).
    fn bundle(scope: &str, token: &str, fp: Option<&str>, expires_at: Option<&str>) -> String {
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!(scope));
        m.insert("token".into(), json!(token));
        if let Some(fp) = fp {
            m.insert("tls_cert_fingerprint".into(), json!(fp));
        }
        if let Some(e) = expires_at {
            m.insert("expires_at".into(), json!(e));
        }
        Value::Object(m).to_string()
    }

    fn default_bundle() -> String {
        bundle(
            "control",
            "shed_control_abc",
            None,
            Some("2026-06-27T19:09:50.730171-05:00"),
        )
    }

    /// Dart's `cbundle()` helper (`token_bundle_test.dart:98-111`): like its
    /// `includeFp: true` default, the fingerprint is always present — `None`
    /// means the default `sha256:aaa…` pin, not omission (the omission case
    /// builds its map by hand).
    fn cbundle(fp: Option<&str>, port: Value, exp: &str) -> String {
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!("control"));
        m.insert("token".into(), json!("tok"));
        m.insert(
            "tls_cert_fingerprint".into(),
            json!(fp.map_or_else(|| pin('a'), str::to_string)),
        );
        m.insert("https_port".into(), port);
        m.insert("expires_at".into(), json!(exp));
        Value::Object(m).to_string()
    }

    fn pin(c: char) -> String {
        format!("sha256:{}", c.to_string().repeat(64))
    }

    // ---- parseTokenBundle ports (`token_bundle_test.dart:30-95`) ----

    // Dart `accepts a valid control bundle` (:31-35).
    #[test]
    fn token_accepts_a_valid_control_bundle() {
        let t = parse_token_bundle(&default_bundle(), None).unwrap();
        assert_eq!(t.token, "shed_control_abc");
        assert!(t.expires_at_unix.is_some());
    }

    // Dart `accepts a matching minted fingerprint` (:37-41).
    #[test]
    fn token_accepts_a_matching_minted_fingerprint() {
        let p = pin('a');
        let src = bundle(
            "control",
            "shed_control_abc",
            Some(&p),
            Some("2026-06-27T19:09:50.730171-05:00"),
        );
        let t = parse_token_bundle(&src, Some(&p)).unwrap();
        assert_eq!(t.token, "shed_control_abc");
    }

    // Dart `rejects a non-control scope` (:43-50).
    #[test]
    fn token_rejects_a_non_control_scope() {
        let src = bundle(
            "session",
            "shed_control_abc",
            None,
            Some("2026-06-27T19:09:50.730171-05:00"),
        );
        assert_eq!(
            parse_token_bundle(&src, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Dart `rejects a minted fingerprint that differs from the pin (no silent
    // re-pin)` (:52-69).
    #[test]
    fn token_rejects_a_differing_minted_fingerprint() {
        let src = bundle(
            "control",
            "shed_control_abc",
            Some(&pin('b')),
            Some("2026-06-27T19:09:50.730171-05:00"),
        );
        assert_eq!(
            parse_token_bundle(&src, Some(&pin('a'))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // Dart `rejects unparseable JSON` (:71-76).
    #[test]
    fn token_rejects_unparseable_json() {
        assert_eq!(
            parse_token_bundle("not json", None),
            Err(TokenBundleError::AuthExpired)
        );
        // A parseable non-object is equally rejected (`raw is! Map`, :36).
        assert_eq!(
            parse_token_bundle("\"a string\"", None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Dart `rejects an empty or whitespace-only token` (:78-83).
    #[test]
    fn token_rejects_an_empty_or_whitespace_only_token() {
        let src = bundle("control", "   ", None, Some("2026-06-27T19:09:50Z"));
        assert_eq!(
            parse_token_bundle(&src, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // BOM parity (codex C7 review): Dart's `String.trim()` strips U+FEFF,
    // Rust's `str::trim` does not — [`dart_trim`] closes the gap. A BOM-only
    // token must fail closed in BOTH parsers, exactly as on mobile.
    #[test]
    fn bom_only_token_is_auth_expired_in_both_parsers() {
        let t = bundle("control", "\u{FEFF}", None, Some("2026-06-27T19:09:50Z"));
        assert_eq!(
            parse_token_bundle(&t, None),
            Err(TokenBundleError::AuthExpired)
        );
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!("control"));
        m.insert("token".into(), json!("\u{FEFF}"));
        m.insert("tls_cert_fingerprint".into(), json!(pin('a')));
        m.insert("https_port".into(), json!(8443));
        m.insert("expires_at".into(), json!("2026-06-27T19:09:50Z"));
        assert_eq!(
            parse_control_bundle(&Value::Object(m).to_string(), None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // The BOM half of the fingerprint normalization: a BOM-wrapped but
    // otherwise valid fingerprint matches after [`dart_trim`] in both parsers.
    #[test]
    fn bom_wrapped_fingerprint_matches_after_dart_trim() {
        let wrapped = format!("\u{FEFF}{}\u{FEFF}", pin('a'));
        let t = bundle(
            "control",
            "shed_control_abc",
            Some(&wrapped),
            Some("2026-06-27T19:09:50Z"),
        );
        let minted = parse_token_bundle(&t, Some(&pin('a'))).unwrap();
        assert_eq!(minted.token, "shed_control_abc");

        let c = cbundle(Some(&wrapped), json!(8443), "2026-06-27T19:09:50Z");
        let b = parse_control_bundle(&c, Some(&pin('a'))).unwrap();
        assert_eq!(b.tls_cert_fingerprint, pin('a'));
    }

    // Strict calendar validation surfaces as AuthExpired at the bundle level:
    // an impossible expiry date is a failed parse, never a usable token.
    #[test]
    fn impossible_expiry_date_is_auth_expired() {
        let t = bundle(
            "control",
            "shed_control_abc",
            None,
            Some("2023-02-29T00:00:00Z"),
        );
        assert_eq!(
            parse_token_bundle(&t, None),
            Err(TokenBundleError::AuthExpired)
        );
        let c = cbundle(None, json!(8443), "2023-02-29T00:00:00Z");
        assert_eq!(
            parse_control_bundle(&c, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Dart `rejects a missing or unparseable expiry (never non-expiring)`
    // (:85-94).
    #[test]
    fn token_rejects_a_missing_or_unparseable_expiry() {
        let missing = bundle("control", "shed_control_abc", None, None);
        assert_eq!(
            parse_token_bundle(&missing, None),
            Err(TokenBundleError::AuthExpired)
        );
        let junk = bundle("control", "shed_control_abc", None, Some("soon"));
        assert_eq!(
            parse_token_bundle(&junk, None),
            Err(TokenBundleError::AuthExpired)
        );
    }

    // Load-bearing tolerance (`token_bundle.dart:25-28,45-52`, not a Dart test
    // case): a configured pin with an ABSENT bundle fingerprint is accepted —
    // the bundle arrived over a host-key-pinned SSH channel.
    #[test]
    fn token_tolerates_an_absent_fingerprint_when_a_pin_is_configured() {
        let t = parse_token_bundle(&default_bundle(), Some(&pin('a'))).unwrap();
        assert_eq!(t.token, "shed_control_abc");
    }

    // The regex half of the pin gate (`token_bundle.dart:49`): a present but
    // MALFORMED fingerprint under a configured pin is PinMismatch.
    #[test]
    fn token_rejects_a_malformed_fingerprint_when_a_pin_is_configured() {
        let src = bundle(
            "control",
            "shed_control_abc",
            Some("sha256:nothex"),
            Some("2026-06-27T19:09:50Z"),
        );
        assert_eq!(
            parse_token_bundle(&src, Some(&pin('a'))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // Exact expiry pin: trimmed token + a known epoch instant.
    #[test]
    fn token_yields_exact_unix_expiry() {
        let src = bundle(
            "control",
            "  shed_control_abc  ",
            None,
            Some("2001-09-09T01:46:40Z"),
        );
        let t = parse_token_bundle(&src, None).unwrap();
        assert_eq!(t.token, "shed_control_abc"); // trimmed (:42)
        assert_eq!(t.expires_at_unix, Some(1_000_000_000));
    }

    // ---- parseControlBundle ports (`token_bundle_test.dart:97-152`) ----

    // Dart `accepts a valid bundle` (:113-118).
    #[test]
    fn control_accepts_a_valid_bundle() {
        let b = parse_control_bundle(&cbundle(None, json!(8443), "2026-06-27T19:09:50Z"), None)
            .unwrap();
        assert_eq!(b.https_port, 8443);
        assert_eq!(b.tls_cert_fingerprint, pin('a'));
        assert_eq!(b.token, "tok");
    }

    // Dart `requires the TLS fingerprint` (:120-127). `cbundle(None, ...)`
    // defaults the fingerprint like Dart's `includeFp: true`; build without it
    // here.
    #[test]
    fn control_requires_the_tls_fingerprint() {
        let mut m = serde_json::Map::new();
        m.insert("scope".into(), json!("control"));
        m.insert("token".into(), json!("tok"));
        m.insert("https_port".into(), json!(8443));
        m.insert("expires_at".into(), json!("2026-06-27T19:09:50Z"));
        let src = Value::Object(m).to_string();
        assert_eq!(
            parse_control_bundle(&src, None),
            Err(TokenBundleError::PinMissing)
        );
    }

    // A malformed fingerprint is PinMissing in the control variant
    // (`token_bundle.dart:98`), unlike the token variant's PinMismatch.
    #[test]
    fn control_rejects_a_malformed_fingerprint_as_pin_missing() {
        let src = cbundle(Some("sha256:nothex"), json!(8443), "2026-06-27T19:09:50Z");
        assert_eq!(
            parse_control_bundle(&src, None),
            Err(TokenBundleError::PinMissing)
        );
    }

    // Dart `rejects an out-of-range https_port` (:129-138).
    #[test]
    fn control_rejects_an_out_of_range_https_port() {
        for port in [json!(70_000), json!(0)] {
            let src = cbundle(None, port, "2026-06-27T19:09:50Z");
            assert_eq!(
                parse_control_bundle(&src, None),
                Err(TokenBundleError::AuthExpired)
            );
        }
        // Dart `portRaw is! int`: a JSON float / string port is equally out.
        for port in [json!(8443.5), json!("8443")] {
            let src = cbundle(None, port, "2026-06-27T19:09:50Z");
            assert_eq!(
                parse_control_bundle(&src, None),
                Err(TokenBundleError::AuthExpired)
            );
        }
    }

    // Dart `enforces an expected pin` (:140-151).
    #[test]
    fn control_enforces_an_expected_pin() {
        let src = cbundle(None, json!(8443), "2026-06-27T19:09:50Z");
        assert_eq!(
            parse_control_bundle(&src, Some(&pin('b'))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // Fingerprint normalization (`token_bundle.dart:97,113-115`): uppercase
    // hex is lowercased before the match, and the RESULT carries the
    // normalized form.
    #[test]
    fn control_normalizes_the_fingerprint_to_lowercase() {
        let upper = format!("sha256:{}", "A".repeat(64));
        let src = cbundle(Some(&upper), json!(8443), "2026-06-27T19:09:50Z");
        let b = parse_control_bundle(&src, Some(&pin('a'))).unwrap();
        assert_eq!(b.tls_cert_fingerprint, pin('a'));
    }

    // ---- the documented pre-epoch u64 divergence (plan 001 §3.5) ----

    // Dart's `DateTime.tryParse` parses a pre-epoch expiry (incl. the Go zero
    // time) into a negative instant; `MintedToken.expires_at_unix` is u64, so
    // both parses map it to AuthExpired — a control token that expired before
    // 1970 is already expired.
    #[test]
    fn pre_epoch_expiry_is_auth_expired() {
        for exp in ["0001-01-01T00:00:00Z", "1969-12-31T23:59:59Z"] {
            let t = bundle("control", "shed_control_abc", None, Some(exp));
            assert_eq!(
                parse_token_bundle(&t, None),
                Err(TokenBundleError::AuthExpired),
                "token bundle, expiry {exp}"
            );
            let c = cbundle(None, json!(8443), exp);
            assert_eq!(
                parse_control_bundle(&c, None),
                Err(TokenBundleError::AuthExpired),
                "control bundle, expiry {exp}"
            );
        }
        // The epoch itself is representable and accepted.
        let ok = bundle("control", "t", None, Some("1970-01-01T00:00:00Z"));
        assert_eq!(
            parse_token_bundle(&ok, None).unwrap().expires_at_unix,
            Some(0)
        );
    }

    // ---- the std-only RFC3339 parser ----

    #[test]
    fn rfc3339_known_epochs() {
        assert_eq!(parse_rfc3339_to_unix("1970-01-01T00:00:00Z"), Ok(0));
        // The Unix "billennium": 2001-09-09 01:46:40 UTC.
        assert_eq!(
            parse_rfc3339_to_unix("2001-09-09T01:46:40Z"),
            Ok(1_000_000_000)
        );
        assert_eq!(
            parse_rfc3339_to_unix("2023-11-14T22:13:20Z"),
            Ok(1_700_000_000)
        );
    }

    #[test]
    fn rfc3339_leap_year() {
        // 2024-02-29 exists (leap year) and lands on the known instant.
        assert_eq!(
            parse_rfc3339_to_unix("2024-02-29T00:00:00Z"),
            Ok(1_709_164_800)
        );
        // 2000 is a leap year (divisible by 400); 1900 and 2023 are not —
        // strict calendar validation rejects their Feb 29 (fail-closed;
        // stricter than Dart's normalizing `DateTime.tryParse`).
        assert!(parse_rfc3339_to_unix("2000-02-29T00:00:00Z").is_ok());
        assert_eq!(parse_rfc3339_to_unix("1900-02-29T00:00:00Z"), Err(()));
        assert_eq!(parse_rfc3339_to_unix("2023-02-29T00:00:00Z"), Err(()));
        // 30-day months cap at 30.
        assert_eq!(parse_rfc3339_to_unix("2023-04-31T00:00:00Z"), Err(()));
        assert!(parse_rfc3339_to_unix("2023-04-30T00:00:00Z").is_ok());
    }

    #[test]
    fn rfc3339_timezone_offsets() {
        // +05:00 is 5h ahead of UTC → same instant as midnight UTC.
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T05:00:00+05:00"),
            Ok(1_893_456_000)
        );
        // -05:00 is behind → 5h AFTER the epoch.
        assert_eq!(
            parse_rfc3339_to_unix("1970-01-01T00:00:00-05:00"),
            Ok(18_000)
        );
    }

    #[test]
    fn rfc3339_fractional_seconds_truncate() {
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T00:00:00.500Z"),
            Ok(1_893_456_000)
        );
        // The mobile fixture shape: fraction + offset together.
        assert_eq!(
            parse_rfc3339_to_unix("2026-06-27T19:09:50.730171-05:00"),
            Ok(1_782_605_390)
        );
    }

    #[test]
    fn rfc3339_go_zero_time_parses_pre_epoch() {
        // No zero-time→None collapse here (unlike the broker's parser): the
        // Go zero value parses to its plain pre-epoch instant, Dart
        // `DateTime.tryParse` parity.
        assert_eq!(
            parse_rfc3339_to_unix("0001-01-01T00:00:00Z"),
            Ok(-62_135_596_800)
        );
    }

    #[test]
    fn rfc3339_rejects_malformed_input() {
        for s in [
            "",
            "soon",
            "garbage",
            "2030-01-01",                // date only, no 'T'
            "2030-01-01T00:00:00",       // missing zone
            "2030-13-01T00:00:00Z",      // bad month
            "2023-13-01T00:00:00Z",      // bad month
            "2030-00-01T00:00:00Z",      // month 0
            "2030-01-32T00:00:00Z",      // day 32
            "2023-01-32T00:00:00Z",      // day 32
            "2023-02-29T00:00:00Z",      // impossible date (non-leap Feb 29)
            "2030-01-00T00:00:00Z",      // day 0
            "2030-01-01T24:00:00Z",      // hour 24
            "2030-01-01T25:00:00Z",      // hour 25
            "2030-01-01T00:60:00Z",      // minute 60
            "2030-01-01T00:61:00Z",      // minute 61
            "2030-01-01T00:00:61Z",      // second 61 (60 = leap second is OK)
            "2030-01-01T00:00:00.Z",     // empty fraction
            "2030-01-01T00:00:00.5xZ",   // non-digit fraction
            "2030-1-01T00:00:00Z",       // non-fixed-width month
            "30-01-01T00:00:00Z",        // non-fixed-width year
            "2030-01-01T00:00:00+5:00",  // non-fixed-width offset hour
            "2030-01-01T00:00:00+25:00", // offset hour out of range
        ] {
            assert_eq!(parse_rfc3339_to_unix(s), Err(()), "should reject {s:?}");
        }
        // The leap second itself is admitted (broker-parser parity).
        assert!(parse_rfc3339_to_unix("2030-01-01T00:00:60Z").is_ok());
    }
}

/// The credential-provider half of plan 001 D5 at the FSM level: bundle decoding
/// across both modes, the legacy-minter compat path, and the state transitions a
/// transport never sees. The end-to-end proof (real handshakes, rotation, mode
/// flip on one `reqwest::Client`) lives in `http::mtls_tests`.
#[cfg(test)]
mod credential_tests {
    use super::*;
    use crate::testtls::{valid_window, TestCa};
    use serde_json::json;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    fn cert_bundle(cert_pem: &str, serial: &str) -> String {
        json!({
            "auth_mode": "mtls",
            "https_port": 8443,
            "tls_cert_fingerprint": format!("sha256:{}", "a".repeat(64)),
            "client_cert": cert_pem,
            "scope": "control",
            "cert_serial": serial,
            "expires_at": "2036-01-01T00:00:00Z",
        })
        .to_string()
    }

    fn token_bundle_json(auth_mode: Option<&str>) -> String {
        let mut m = serde_json::Map::new();
        if let Some(mode) = auth_mode {
            m.insert("auth_mode".into(), json!(mode));
        }
        m.insert("http_port".into(), json!(8080));
        m.insert("https_port".into(), json!(8443));
        m.insert(
            "tls_cert_fingerprint".into(),
            json!(format!("sha256:{}", "a".repeat(64))),
        );
        m.insert("token".into(), json!("shed_control_abc"));
        m.insert("scope".into(), json!("control"));
        m.insert("token_id".into(), json!("id-1"));
        m.insert("expires_at".into(), json!("2036-01-01T00:00:00Z"));
        Value::Object(m).to_string()
    }

    // ---- absent auth_mode means token (the compat-matrix rule) ----

    #[test]
    fn absent_auth_mode_decodes_as_token_in_both_parsers() {
        // A pre-mtls server emits no auth_mode key at all.
        let legacy = token_bundle_json(None);
        let b = parse_control_bundle(&legacy, None).unwrap();
        assert_eq!(b.auth_mode, AuthMode::Token);
        assert_eq!(b.token, "shed_control_abc");
        assert!(b.client_cert.is_empty());
        assert_eq!(
            parse_credential_bundle(&legacy, None).unwrap(),
            MintedCredential::Token(MintedToken {
                token: "shed_control_abc".into(),
                expires_at_unix: Some(2_082_758_400),
            })
        );

        // An explicit "token" is the same thing, and so is an UNRECOGNIZED value
        // (never mtls — Go `sdk.Bundle.Mode` parity).
        for mode in ["token", "", "future-mode"] {
            let b = parse_control_bundle(&token_bundle_json(Some(mode)), None).unwrap();
            assert_eq!(b.auth_mode, AuthMode::Token, "mode {mode:?}");
            assert_eq!(b.token, "shed_control_abc");
        }
        assert_eq!(AuthMode::from_wire(None), AuthMode::Token);
        assert_eq!(AuthMode::from_wire(Some(" mtls ")), AuthMode::Mtls);
        assert_eq!(AuthMode::Mtls.as_str(), "mtls");
        assert_eq!(AuthMode::Token.as_str(), "token");
    }

    #[test]
    fn mtls_bundles_decode_to_a_certificate_and_reject_unusable_ones() {
        let ca = TestCa::new("shed-ca");
        let issued = ca.client_cert("SHA256:abc", "control", valid_window());
        let raw = cert_bundle(&issued.cert_pem, "01ff");

        let b = parse_control_bundle(&raw, None).unwrap();
        assert_eq!(b.auth_mode, AuthMode::Mtls);
        assert_eq!(b.cert_serial, "01ff");
        assert!(b.token.is_empty(), "an mtls bundle carries no bearer token");
        assert_eq!(b.https_port, 8443);

        match parse_credential_bundle(&raw, None).unwrap() {
            MintedCredential::Certificate(c) => {
                assert_eq!(c.serial, "01ff");
                assert_eq!(c.cert_pem, issued.cert_pem);
                assert_eq!(c.expires_at_unix, Some(2_082_758_400));
            }
            other => panic!("expected a certificate, got {other:?}"),
        }

        // Fail-closed: mtls with no / unusable certificate is rejected, never
        // adopted as an empty credential that fails every later handshake.
        for bad in [
            "",
            "not a pem",
            "-----BEGIN CERTIFICATE-----\n!!\n-----END CERTIFICATE-----",
        ] {
            assert_eq!(
                parse_control_bundle(&cert_bundle(bad, "01"), None),
                Err(TokenBundleError::AuthExpired),
                "cert {bad:?}"
            );
            assert_eq!(
                parse_credential_bundle(&cert_bundle(bad, "01"), None),
                Err(TokenBundleError::AuthExpired),
                "cert {bad:?}"
            );
        }
        // The pin is still enforced on the mint path when one is configured.
        assert_eq!(
            parse_credential_bundle(&raw, Some(&format!("sha256:{}", "b".repeat(64)))),
            Err(TokenBundleError::PinMismatch)
        );
    }

    // ---- the legacy minter compat path ----

    /// A pre-mtls [`TokenMinter`]: it implements only `mint`, exactly as every
    /// in-tree implementation did before this change.
    struct LegacyMinter {
        calls: AtomicUsize,
    }

    #[async_trait::async_trait]
    impl TokenMinter for LegacyMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(MintedToken {
                token: "legacy-tok".into(),
                expires_at_unix: Some(FAR_FUTURE),
            })
        }
    }

    /// 2100-01-01. Unlike the FSM tests above (which drive an injected clock),
    /// these run on the real clock, so the expiry has to be genuinely far away or
    /// every call would re-mint.
    const FAR_FUTURE: u64 = 4_102_444_800;

    #[tokio::test]
    async fn a_legacy_token_only_minter_still_works_and_is_never_asked_for_a_csr() {
        let minter = Arc::new(LegacyMinter {
            calls: AtomicUsize::new(0),
        });
        assert!(!minter.supports_mtls());
        // The default `mint_credential` wraps `mint` — and the request it would
        // receive carries no CSR, so no keypair is generated for a minter that
        // could not use one.
        let req = CredentialRequest::default();
        assert_eq!(req.csr_base64(), None);
        assert_eq!(
            minter.mint_credential("s", &req).await.unwrap(),
            MintedCredential::Token(MintedToken {
                token: "legacy-tok".into(),
                expires_at_unix: Some(FAR_FUTURE),
            })
        );

        let p = ControlTokenProvider::new("s".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "legacy-tok");
        let cred = p.credential().await.unwrap();
        assert_eq!(cred.mode, Some(AuthMode::Token));
        assert_eq!(cred.bearer_token(), Some("legacy-tok"));
        // Nothing is ever installed on the transport in token state.
        assert!(p.cert_resolver().current().is_none());
        // Two mints total: the direct trait call above, plus the provider's one —
        // the second `credential()` was served from the cache.
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2);
    }

    // ---- Credential semantics ----

    #[test]
    fn usable_and_bearer_token_gate_on_the_mode() {
        assert!(!Credential::default().usable());
        let tok = Credential {
            mode: Some(AuthMode::Token),
            token: "t".into(),
            ..Default::default()
        };
        assert!(tok.usable());
        assert_eq!(tok.bearer_token(), Some("t"));
        // A mode whose payload is missing is NOT usable — the shape a stripped
        // entry degrades to.
        assert!(!Credential {
            mode: Some(AuthMode::Token),
            ..Default::default()
        }
        .usable());
        let cert = Credential {
            mode: Some(AuthMode::Mtls),
            cert_pem: "PEM".into(),
            cert_serial: "ab".into(),
            ..Default::default()
        };
        assert!(cert.usable());
        // Load-bearing: an mtls credential never sends a bearer, even if some
        // caller stuffed one into the struct.
        assert_eq!(
            Credential {
                token: "leftover".into(),
                ..cert.clone()
            }
            .bearer_token(),
            None
        );
        assert!(!Credential {
            mode: Some(AuthMode::Mtls),
            ..Default::default()
        }
        .usable());
        // Identity: the token in token state, the serial in mtls state.
        assert_eq!(tok.identity(), "t");
        assert_eq!(cert.identity(), "ab");
    }

    /// A minter whose next answer is scripted, recording the CSRs it received.
    struct ScriptMinter {
        answers: std::sync::Mutex<Vec<Result<MintedCredential, String>>>,
        csrs: std::sync::Mutex<Vec<Option<String>>>,
    }

    impl ScriptMinter {
        fn new(answers: Vec<Result<MintedCredential, String>>) -> Arc<Self> {
            Arc::new(Self {
                answers: std::sync::Mutex::new(answers),
                csrs: std::sync::Mutex::new(Vec::new()),
            })
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for ScriptMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            unreachable!()
        }
        fn supports_mtls(&self) -> bool {
            true
        }
        async fn mint_credential(
            &self,
            _server: &str,
            req: &CredentialRequest,
        ) -> Result<MintedCredential, ShedError> {
            self.csrs
                .lock()
                .unwrap()
                .push(req.csr_base64().map(str::to_string));
            let mut answers = self.answers.lock().unwrap();
            if answers.is_empty() {
                return Err(ShedError::Transport("script exhausted".into()));
            }
            answers.remove(0).map_err(ShedError::Transport)
        }
    }

    #[tokio::test]
    async fn an_mtls_capable_minter_always_receives_a_fresh_csr() {
        let ca = TestCa::new("shed-ca");
        let issued = ca.client_cert("SHA256:abc", "control", valid_window());
        // The certificate is issued for an UNRELATED key, so adoption must fail —
        // what this asserts is the CSR handed out, not the outcome.
        let minter =
            ScriptMinter::new(vec![Ok(MintedCredential::Certificate(MintedCertificate {
                cert_pem: issued.cert_pem,
                serial: "01".into(),
                expires_at_unix: Some(FAR_FUTURE),
            }))]);
        let p = ControlTokenProvider::new("s".into(), minter.clone());
        let err = p.credential().await.unwrap_err();
        assert!(
            err.to_string().contains("unusable"),
            "a certificate for a key we do not hold must be refused: {err}"
        );
        let csrs = minter.csrs.lock().unwrap().clone();
        assert_eq!(csrs.len(), 1);
        assert!(csrs[0].is_some(), "an mtls-capable minter is handed a CSR");
        assert!(p.cert_resolver().current().is_none());
    }

    #[tokio::test]
    async fn a_certificate_answer_without_a_csr_is_refused() {
        // Only reachable through the trait directly (the provider always sends a
        // CSR to an mtls-capable minter), but it is the one shape that would
        // otherwise install a certificate whose key lives in another process.
        struct NoCsrCertMinter(String);
        #[async_trait::async_trait]
        impl TokenMinter for NoCsrCertMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                unreachable!()
            }
            fn supports_mtls(&self) -> bool {
                false // ...yet answers with a certificate anyway
            }
            async fn mint_credential(
                &self,
                _server: &str,
                _req: &CredentialRequest,
            ) -> Result<MintedCredential, ShedError> {
                Ok(MintedCredential::Certificate(MintedCertificate {
                    cert_pem: self.0.clone(),
                    serial: "01".into(),
                    expires_at_unix: Some(FAR_FUTURE),
                }))
            }
        }
        let ca = TestCa::new("shed-ca");
        let issued = ca.client_cert("SHA256:abc", "control", valid_window());
        let p = ControlTokenProvider::new("s".into(), Arc::new(NoCsrCertMinter(issued.cert_pem)));
        let err = p.credential().await.unwrap_err();
        assert!(err.to_string().contains("no CSR"), "{err}");
    }

    #[tokio::test]
    async fn token_accessor_errors_in_mtls_state_instead_of_sending_nothing() {
        // The provider generates its OWN keypair, so a certificate minted for any
        // other key could not be adopted — drive the state through a matching mint
        // instead, by letting the provider's own CSR be signed (`FlipMinter` in
        // certificate mode does exactly that).
        let p = ControlTokenProvider::new("s".into(), FlipMinter::new(true));
        let cred = p.credential().await.unwrap();
        assert_eq!(cred.mode, Some(AuthMode::Mtls));
        assert!(p.cert_resolver().current().is_some());
        let err = p.token().await.unwrap_err();
        assert!(err.to_string().contains("no bearer token"), "{err}");
    }

    #[tokio::test]
    async fn invalidate_if_current_credential_ignores_a_stale_rejection() {
        let ca = TestCa::new("shed-ca");
        let issued = ca.client_cert("SHA256:abc", "control", valid_window());
        let _ = issued;
        let minter = ScriptMinter::new(vec![
            Ok(MintedCredential::Token(MintedToken {
                token: "tok-1".into(),
                expires_at_unix: Some(FAR_FUTURE),
            })),
            Ok(MintedCredential::Token(MintedToken {
                token: "tok-2".into(),
                expires_at_unix: Some(FAR_FUTURE),
            })),
        ]);
        let p = ControlTokenProvider::new("s".into(), minter);
        assert_eq!(p.credential().await.unwrap().token, "tok-1");

        // A stale rejection naming a credential we have already rotated past is
        // ignored (no re-mint, cache intact).
        p.invalidate_if_current_credential(&Credential {
            mode: Some(AuthMode::Mtls),
            cert_serial: "long-gone".into(),
            ..Default::default()
        })
        .await;
        assert_eq!(p.credential().await.unwrap().token, "tok-1");

        // The current one does invalidate.
        let current = p.credential().await.unwrap();
        p.invalidate_if_current_credential(&current).await;
        assert_eq!(p.credential().await.unwrap().token, "tok-2");
    }

    #[tokio::test]
    async fn an_unusable_cached_credential_is_re_minted_rather_than_served() {
        // The stripped-entry shape at the FSM level: a seed that holds a mode but
        // nothing to present must mint, not be handed out.
        let minter = ScriptMinter::new(vec![Ok(MintedCredential::Token(MintedToken {
            token: "fresh".into(),
            expires_at_unix: Some(FAR_FUTURE),
        }))]);
        let p = ControlTokenProvider::new("s".into(), minter);
        {
            // Reach into the state the way a persisted-but-degraded entry would
            // arrive (with_seed refuses an empty token outright, which is the
            // other half of the same rule).
            let mut st = p.state.lock().await;
            st.cached = Some(Credential {
                mode: Some(AuthMode::Mtls),
                cert_serial: "01".into(),
                expires_at_unix: Some(FAR_FUTURE),
                ..Default::default()
            });
        }
        assert_eq!(p.credential().await.unwrap().token, "fresh");
    }

    /// Put the provider into the mtls steady state without a live minter: a
    /// cached certificate credential plus the matching identity installed on the
    /// resolver, exactly as a successful mint leaves things.
    async fn mtls_state(p: &ControlTokenProvider, serial: &str) {
        let ca = TestCa::new("shed-ca");
        let kp = crate::csr::ClientKeyPair::generate().unwrap();
        let (pem, _) = ca.sign_csr(kp.csr_der(), "SHA256:abc", "control", 1, valid_window());
        let certified = Arc::new(certified_key_from_pem(&pem, kp.key_pkcs8_der()).unwrap());
        let mut st = p.state.lock().await;
        st.cached = Some(Credential {
            mode: Some(AuthMode::Mtls),
            cert_pem: pem,
            cert_serial: serial.into(),
            expires_at_unix: Some(FAR_FUTURE),
            ..Default::default()
        });
        p.cert_resolver.set(Some(certified));
    }

    #[tokio::test]
    async fn an_mtls_rejection_invalidates_whatever_is_current() {
        // The capture/transmit race, at the provider level: reqwest reads the
        // resolver LIVE during the handshake, so the request can transmit
        // credential N+1 while the caller recorded N — and a pooled connection
        // presents whatever was current when it was dialed, which may be neither.
        // A certificate rejection therefore cannot be attributed, and dismissing
        // it as stale is how the retry ends up re-sending the refused credential.
        let minter = ScriptMinter::new(vec![Ok(MintedCredential::Token(MintedToken {
            token: "fresh".into(),
            expires_at_unix: Some(FAR_FUTURE),
        }))]);
        let p = ControlTokenProvider::new("s".into(), minter);
        mtls_state(&p, "current-serial").await;

        p.invalidate_if_current_credential(&Credential {
            mode: Some(AuthMode::Mtls),
            cert_serial: "some-other-serial".into(),
            ..Default::default()
        })
        .await;

        let st = p.state.lock().await;
        assert!(
            st.cached.is_none(),
            "the current credential must be dropped"
        );
        assert!(st.must_mint, "the next resolution must mint");
        assert!(
            p.cert_resolver.current().is_none(),
            "and the transport must stop presenting it"
        );
    }

    // The lock-ORDERING property of the resolver withdrawal (it happens UNDER the
    // state lock, not after releasing it).
    //
    // Made observable rather than raced: `block_writes` stalls the resolver write,
    // and the test then asks whether the provider's state is still locked. Ordered
    // correctly, an invalidation in flight blocks every other state mutation, so
    // no mint can start — which is precisely what stops a concurrent mint from
    // installing certificate N+1 into a resolver an older invalidation is about to
    // clear (leaving the provider handing out a credential the transport no longer
    // presents). With the write moved back outside the lock, the mint below runs
    // to completion instead of timing out.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    #[allow(clippy::await_holding_lock)] // the point of the test is to hold it
    async fn resolver_withdrawal_is_ordered_under_the_state_lock() {
        let minter = ScriptMinter::new(vec![Ok(MintedCredential::Token(MintedToken {
            token: "fresh".into(),
            expires_at_unix: Some(FAR_FUTURE),
        }))]);
        let p = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        mtls_state(&p, "serial-1").await;

        // Stall the resolver write.
        let guard = p.cert_resolver.block_writes();
        let invalidator = {
            let p = p.clone();
            tokio::spawn(async move { p.invalidate().await })
        };
        // Let it take the state lock and reach the (now blocked) withdrawal.
        tokio::time::sleep(Duration::from_millis(50)).await;

        // Taking the state lock is the FIRST thing a concurrent mint does before
        // it installs certificate N+1 and publishes it to the resolver. If it can
        // be taken while a withdrawal is in flight, that interleaving is live.
        let locked = tokio::time::timeout(Duration::from_millis(250), p.state.lock()).await;
        assert!(
            locked.is_err(),
            "the state lock was released before the resolver was withdrawn — \
             a concurrent mint could install a certificate this invalidation then clears"
        );
        assert!(
            minter.csrs.lock().unwrap().is_empty(),
            "no mint may even be attempted"
        );

        drop(guard);
        invalidator.await.unwrap();
        let st = p.state.lock().await;
        assert!(st.cached.is_none());
        assert!(p.cert_resolver.current().is_none());
    }

    // ---- credential_adopted + the derived mode_changed (plan 002 §7 P1) ----

    /// Poll `done` for up to ~2s, then return regardless — every assertion about
    /// a delivered event goes through this, because delivery is asynchronous BY
    /// CONTRACT (the dispatcher thread). Returning on timeout rather than
    /// panicking leaves the caller's own `assert_eq!` to report what was missing.
    async fn wait_until(mut done: impl FnMut() -> bool) {
        for _ in 0..400 {
            if done() {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    /// One entry per callback, in a SINGLE interleaved log — so the
    /// adoption-before-mode_changed ordering contract is assertable, not just
    /// each stream independently.
    #[derive(Clone, Debug)]
    enum Fired {
        Adopted(CredentialAdopted),
        Mode(String, AuthMode),
    }

    /// Records every delivered notification, in order.
    #[derive(Default)]
    struct EventLog {
        fired: std::sync::Mutex<Vec<Fired>>,
    }

    impl EventLog {
        fn adopted_snapshot(&self) -> Vec<CredentialAdopted> {
            self.fired
                .lock()
                .unwrap()
                .iter()
                .filter_map(|f| match f {
                    Fired::Adopted(e) => Some(e.clone()),
                    Fired::Mode(..) => None,
                })
                .collect()
        }

        async fn wait_adopted(&self, n: usize) -> Vec<CredentialAdopted> {
            wait_until(|| self.adopted_snapshot().len() >= n).await;
            let got = self.adopted_snapshot();
            assert_eq!(got.len(), n, "expected {n} adoption events, got {got:?}");
            got
        }

        async fn wait_modes(&self, expected: &[AuthMode]) {
            let count = || {
                self.fired
                    .lock()
                    .unwrap()
                    .iter()
                    .filter(|f| matches!(f, Fired::Mode(..)))
                    .count()
            };
            wait_until(|| count() >= expected.len()).await;
            let fired = self.fired.lock().unwrap().clone();
            let modes: Vec<AuthMode> = fired
                .iter()
                .filter_map(|f| match f {
                    Fired::Mode(_, m) => Some(*m),
                    Fired::Adopted(_) => None,
                })
                .collect();
            assert_eq!(modes, expected, "mode_changed sequence");
            // The ordering contract: every mode_changed is preceded, somewhere
            // earlier in the interleaved log, by the adoption that caused it —
            // and never fires as the very first event.
            for (i, f) in fired.iter().enumerate() {
                if matches!(f, Fired::Mode(..)) {
                    assert!(
                        fired[..i].iter().any(|p| matches!(p, Fired::Adopted(_))),
                        "mode_changed fired before any credential_adopted: {fired:?}"
                    );
                }
            }
        }
    }

    impl CredentialObserver for EventLog {
        fn on_credential_adopted(&self, event: &CredentialAdopted) {
            self.fired
                .lock()
                .unwrap()
                .push(Fired::Adopted(event.clone()));
        }
        fn on_mode_changed(&self, server: &str, mode: AuthMode) {
            self.fired
                .lock()
                .unwrap()
                .push(Fired::Mode(server.to_string(), mode));
        }
    }

    /// Answers with whichever shape the test currently selects, signing the
    /// PROVIDER's own CSR in certificate mode so the adoption actually succeeds.
    struct FlipMinter {
        ca: TestCa,
        issue_cert: AtomicBool,
        n: AtomicUsize,
    }

    impl FlipMinter {
        fn new(issue_cert: bool) -> Arc<Self> {
            Arc::new(Self {
                ca: TestCa::new("shed-ca"),
                issue_cert: AtomicBool::new(issue_cert),
                n: AtomicUsize::new(0),
            })
        }
        fn set_issue_cert(&self, v: bool) {
            self.issue_cert.store(v, Ordering::SeqCst);
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for FlipMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            unreachable!("an mtls-capable minter is always asked through mint_credential")
        }
        fn supports_mtls(&self) -> bool {
            true
        }
        async fn mint_credential(
            &self,
            _server: &str,
            req: &CredentialRequest,
        ) -> Result<MintedCredential, ShedError> {
            use base64::Engine as _;
            let n = self.n.fetch_add(1, Ordering::SeqCst) + 1;
            if !self.issue_cert.load(Ordering::SeqCst) {
                return Ok(MintedCredential::Token(MintedToken {
                    token: format!("tok-{n}"),
                    expires_at_unix: Some(FAR_FUTURE),
                }));
            }
            let der = base64::engine::general_purpose::STANDARD
                .decode(req.csr_base64().expect("a CSR for an mtls-capable minter"))
                .unwrap();
            // A distinctive serial so a leak assertion on it cannot pass by
            // coincidence against a short number that occurs in a timestamp.
            let (pem, serial) = self.ca.sign_csr(
                &der,
                "SHA256:abc",
                "control",
                0x00c0_ffee_0000 + n as u64,
                valid_window(),
            );
            Ok(MintedCredential::Certificate(MintedCertificate {
                cert_pem: pem,
                serial,
                expires_at_unix: Some(FAR_FUTURE),
            }))
        }
    }

    // Every successful mint emits an adoption — including the ROTATIONS that
    // change nothing but the credential's value. That is what makes the event
    // usable as a persistence primitive (a consumer that only heard about shape
    // changes would never learn a refreshed token or a new expiry).
    #[tokio::test]
    async fn credential_adopted_fires_on_every_successful_mint() {
        let log = Arc::new(EventLog::default());
        let minter = FlipMinter::new(false);
        let p = ControlTokenProvider::new("mini2".into(), minter)
            .with_observer(log.clone() as Arc<dyn CredentialObserver>);

        for _ in 0..3 {
            p.credential().await.unwrap();
            p.invalidate().await; // force the next resolution to mint again
        }

        let adopted = log.wait_adopted(3).await;
        for (i, ev) in adopted.iter().enumerate() {
            assert_eq!(ev.server, "mini2");
            assert_eq!(ev.mode, AuthMode::Token);
            assert_eq!(ev.expires_at_unix, Some(FAR_FUTURE));
            assert_eq!(ev.token.as_deref(), Some(format!("tok-{}", i + 1).as_str()));
        }
        // ...while the DERIVED transition event fired only once: three mints, one
        // shape.
        log.wait_modes(&[AuthMode::Token]).await;
    }

    // The payload split (plan 002 §7 P1 / §7 P3): an mtls adoption carries no
    // credential material at all — no token, and nothing else that could be
    // presented to a server either.
    #[tokio::test]
    async fn an_mtls_adoption_event_carries_no_credential_material() {
        let log = Arc::new(EventLog::default());
        let minter = FlipMinter::new(true);
        let p = ControlTokenProvider::new("mini2".into(), minter)
            .with_observer(log.clone() as Arc<dyn CredentialObserver>);

        let cred = p.credential().await.unwrap();
        assert_eq!(cred.mode, Some(AuthMode::Mtls));
        assert!(
            !cred.cert_pem.is_empty(),
            "the PROVIDER holds the certificate"
        );

        let ev = log.wait_adopted(1).await.remove(0);
        assert_eq!(ev.server, "mini2");
        assert_eq!(ev.mode, AuthMode::Mtls);
        assert_eq!(ev.expires_at_unix, Some(FAR_FUTURE));
        assert_eq!(ev.token, None, "an mtls event never carries a bearer token");
        // The whole rendered event: no certificate, no key, no serial — the
        // structural audit plan 002 §7 P3 asks for, run against the REAL event.
        let rendered = format!("{ev:?}");
        for forbidden in [
            "BEGIN CERTIFICATE",
            "PRIVATE KEY",
            &cred.cert_pem[..40],
            &cred.cert_serial,
        ] {
            assert!(
                !rendered.contains(forbidden),
                "adoption event leaked {forbidden:?}: {rendered}"
            );
        }
        // ...and the token-mode Debug still redacts the bearer it does carry.
        let rendered = format!(
            "{:?}",
            CredentialAdopted {
                server: "mini2".into(),
                mode: AuthMode::Token,
                expires_at_unix: None,
                token: Some("sk-live-do-not-log-me".into()),
            }
        );
        assert!(!rendered.contains("sk-live"), "{rendered}");
        assert!(rendered.contains("<redacted>"), "{rendered}");
    }

    // `mode_changed` survives as a DERIVED event: it still fires on a transition,
    // in BOTH directions, and stays silent for a mint that kept the shape.
    #[tokio::test]
    async fn mode_changed_is_derived_and_still_fires_on_flips_both_directions() {
        let log = Arc::new(EventLog::default());
        let minter = FlipMinter::new(false);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone())
            .with_observer(log.clone() as Arc<dyn CredentialObserver>);

        // token → token (a rotation: adoption, no transition)
        p.credential().await.unwrap();
        p.invalidate().await;
        p.credential().await.unwrap();

        // token → mtls
        minter.set_issue_cert(true);
        p.invalidate().await;
        assert_eq!(p.credential().await.unwrap().mode, Some(AuthMode::Mtls));

        // ...and back again.
        minter.set_issue_cert(false);
        p.invalidate().await;
        assert_eq!(p.credential().await.unwrap().mode, Some(AuthMode::Token));

        log.wait_adopted(4).await;
        log.wait_modes(&[AuthMode::Token, AuthMode::Mtls, AuthMode::Token])
            .await;
        // The event names the server the app has to migrate.
        assert!(log.fired.lock().unwrap().iter().all(|f| match f {
            Fired::Mode(s, _) => s == "mini2",
            Fired::Adopted(e) => e.server == "mini2",
        }));
    }

    // A foreign handler that blocks — a Swift closure taking a UI lock, a Dart
    // sink whose isolate is busy — must not slow the mint path down, let alone
    // stall it. It cannot, because the mint only ENQUEUES: everything foreign runs
    // on the dispatcher thread.
    //
    // Note the runtime: this is the DEFAULT current-thread test runtime, which is
    // exactly the configuration a `tokio::spawn`ed dispatcher would fail — a
    // handler blocking a worker there would stall the executor the mint below runs
    // on. Passing here is the evidence for the thread-not-task decision.
    #[tokio::test]
    async fn a_blocking_observer_neither_delays_nor_stalls_the_mint_path() {
        struct BlockingObserver {
            entered: AtomicUsize,
            release: std::sync::Mutex<std::sync::mpsc::Receiver<()>>,
            delivered: std::sync::Mutex<Vec<String>>,
        }
        impl CredentialObserver for BlockingObserver {
            fn on_credential_adopted(&self, event: &CredentialAdopted) {
                if self.entered.fetch_add(1, Ordering::SeqCst) == 0 {
                    // Park inside the handler until the test lets go.
                    let _ = self.release.lock().unwrap().recv();
                }
                self.delivered
                    .lock()
                    .unwrap()
                    .push(event.token.clone().unwrap_or_default());
            }
        }

        let (release_tx, release_rx) = std::sync::mpsc::channel::<()>();
        let obs = Arc::new(BlockingObserver {
            entered: AtomicUsize::new(0),
            release: std::sync::Mutex::new(release_rx),
            delivered: std::sync::Mutex::new(Vec::new()),
        });
        let minter = FlipMinter::new(false);
        let p = ControlTokenProvider::new("mini2".into(), minter)
            .with_observer(obs.clone() as Arc<dyn CredentialObserver>);

        // Three mints while the first handler is wedged. Each is bounded: a mint
        // path that waited on the observer would blow the timeout.
        for i in 0..3 {
            tokio::time::timeout(Duration::from_secs(2), async {
                p.credential().await.unwrap();
                p.invalidate().await;
            })
            .await
            .unwrap_or_else(|_| panic!("mint {i} waited on the blocked observer"));
        }
        // The handler really is still parked (so the mints above genuinely ran
        // past it, rather than the events happening to be delivered quickly).
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(obs.entered.load(Ordering::SeqCst), 1);
        assert!(obs.delivered.lock().unwrap().is_empty());

        // Released, the queued events drain in order — nothing was dropped.
        release_tx.send(()).unwrap();
        wait_until(|| obs.delivered.lock().unwrap().len() >= 3).await;
        assert_eq!(
            *obs.delivered.lock().unwrap(),
            vec!["tok-1".to_string(), "tok-2".into(), "tok-3".into()]
        );
    }

    // The post-lock proof: a handler may call straight back INTO the provider that
    // notified it. Under the previous under-the-lock delivery this deadlocks (the
    // provider's `Mutex` is not reentrant); it succeeds now because the mint that
    // produced the event released the lock — and returned to its caller — before
    // anything foreign ran.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_observer_may_call_back_into_the_provider_that_notified_it() {
        struct ReenteringObserver {
            provider: std::sync::OnceLock<std::sync::Weak<ControlTokenProvider>>,
            handle: tokio::runtime::Handle,
            once: AtomicBool,
            reentered: std::sync::Mutex<Vec<String>>,
        }
        impl CredentialObserver for ReenteringObserver {
            fn on_credential_adopted(&self, _event: &CredentialAdopted) {
                if self.once.swap(true, Ordering::SeqCst) {
                    return; // one re-entry is the proof; more would just recurse
                }
                let p = self
                    .provider
                    .get()
                    .expect("provider wired before the first mint")
                    .upgrade()
                    .expect("provider alive");
                // A bounded re-entrant call: on the broken (under-lock) design this
                // reports the deadlock instead of hanging the suite forever.
                let got = self.handle.block_on(async {
                    tokio::time::timeout(Duration::from_secs(5), p.credential()).await
                });
                self.reentered.lock().unwrap().push(match got {
                    Ok(Ok(c)) => c.token,
                    Ok(Err(e)) => format!("mint error: {e}"),
                    Err(_) => "DEADLOCK".into(),
                });
            }
        }

        let obs = Arc::new(ReenteringObserver {
            provider: std::sync::OnceLock::new(),
            handle: tokio::runtime::Handle::current(),
            once: AtomicBool::new(false),
            reentered: std::sync::Mutex::new(Vec::new()),
        });
        let minter = FlipMinter::new(false);
        let p = Arc::new(
            ControlTokenProvider::new("mini2".into(), minter)
                .with_observer(obs.clone() as Arc<dyn CredentialObserver>),
        );
        obs.provider.set(Arc::downgrade(&p)).ok().unwrap();

        assert_eq!(p.credential().await.unwrap().token, "tok-1");
        wait_until(|| !obs.reentered.lock().unwrap().is_empty()).await;
        assert_eq!(
            *obs.reentered.lock().unwrap(),
            vec!["tok-1".to_string()],
            "the re-entrant credential() must return the cached credential, not deadlock"
        );
    }

    // ---- Debug redaction ----

    #[test]
    fn debug_impls_never_render_the_bearer_token() {
        const SECRET: &str = "sk-live-do-not-log-me";
        let minted = MintedToken {
            token: SECRET.into(),
            expires_at_unix: Some(FAR_FUTURE),
        };
        for rendered in [
            format!("{minted:?}"),
            format!("{:?}", MintedCredential::Token(minted.clone())),
            format!("{:?}", Some(minted.clone())),
            format!(
                "{:?}",
                Credential {
                    mode: Some(AuthMode::Token),
                    token: SECRET.into(),
                    ..Default::default()
                }
            ),
            format!(
                "{:?}",
                ControlBundle {
                    auth_mode: AuthMode::Token,
                    token: SECRET.into(),
                    client_cert: String::new(),
                    cert_serial: String::new(),
                    expires_at_unix: FAR_FUTURE,
                    tls_cert_fingerprint: "sha256:aa".into(),
                    https_port: 8443,
                }
            ),
        ] {
            assert!(
                !rendered.contains(SECRET),
                "a live token reached a Debug rendering: {rendered}"
            );
            assert!(rendered.contains("<redacted>"), "{rendered}");
        }
        // The non-secret fields stay legible — a redacted Debug still has to be
        // useful for debugging.
        let cred = Credential {
            mode: Some(AuthMode::Mtls),
            cert_serial: "0a1b".into(),
            expires_at_unix: Some(FAR_FUTURE),
            ..Default::default()
        };
        let rendered = format!("{cred:?}");
        assert!(rendered.contains("Mtls"), "{rendered}");
        assert!(rendered.contains("0a1b"), "{rendered}");
    }
}
