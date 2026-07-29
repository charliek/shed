//! HTTP read client for one shed-server.
//!
//! reqwest + rustls; the base URL is injected (the app substitutes the hermetic
//! mock in test mode — the core is env-agnostic). Decoding is the defensive
//! `models` layer.
//!
//! Parity with Swift's `ShedServerClient`: an 8s GET timeout, an explicit
//! User-Agent, an https-only redirect policy, leaf-cert pinning (fail-closed on
//! a non-https URL), a credential with an auth-failure → invalidate + retry-once
//! (provider-backed only; the retry is skipped when the re-mint returns the same
//! rejected credential — plan 001 §3.4), and `ShedError` matching
//! `ShedClientError`. Lifecycle + SSE create land in M4.
//!
//! Every request shape — JSON, lifecycle, and BOTH SSE streams — goes through
//! one re-auth path, [`Client::send_authed`]: classify the outcome
//! ([`crate::authfail`]: an HTTP 401 or a peer TLS alert naming a certificate
//! problem), invalidate the credential, re-mint once, retry once. There is no
//! second policy for streams, and none of it branches on the mode the client
//! believes it is in — which is what makes a server-side token↔mtls flip
//! recover in either direction with no operator action (plan 001 D5).

use std::sync::Arc;
use std::time::Duration;

use futures_util::StreamExt;
use thiserror::Error;

use crate::models::{
    CreateShedRequest, EgressProfileInfo, ImageList, Overview, ServerInfo, SessionsResponse, Shed,
    ShedImage, ShedList, SystemDiskUsage,
};
use crate::rc::RcMessagesPage;
use crate::rc_events::{parse_rc_event, RcEvent};
use crate::sse::SseParser;
use crate::token::{ControlTokenProvider, TokenMinter};

/// Mirrors Swift's `ShedClientError` (same cases, same messages).
#[derive(Debug, Error)]
pub enum ShedError {
    #[error("shed-server returned HTTP {0}")]
    BadStatus(u16),
    #[error("transport error: {0}")]
    Transport(String),
    #[error("decode error: {0}")]
    Decode(String),
    #[error("create failed: {0}")]
    Create(String),
    #[error("{0}")]
    Config(String),
    /// The host-side credential broker must be upgraded before this server is
    /// usable — the one mint failure whose remedy is a user ACTION rather than
    /// waiting or debugging, so it is a typed case instead of a `Config` string:
    /// presentation layers branch on it (banner leads with the remedy, `detail`
    /// demoted to tooltip/log) and the empty state stops blaming config.yaml
    /// (shed#300). The Display keeps the remedy FIRST so even a string-only
    /// consumer truncates the context, never the action.
    #[error("upgrade shed-host-agent — it is too old to obtain a client certificate for {server} ({detail})")]
    AgentUpgradeRequired { server: String, detail: String },
}

const GET_TIMEOUT: Duration = Duration::from_secs(8);
const WRITE_TIMEOUT: Duration = Duration::from_secs(15);
/// Max gap between SSE bytes during a create before we give up (a hung stream);
/// generous so a healthy provision with periodic progress never trips it.
const CREATE_IDLE_TIMEOUT: Duration = Duration::from_secs(120);
/// Max gap between SSE BYTES on the long-lived rc-events stream before the
/// client treats the connection as silently dead. The server heartbeats every
/// 25s with a `: heartbeat` comment (`internal/api/rcevents.go:188-206`), and
/// those comment bytes reset this timer even though the parser swallows them
/// without emitting an event — the timeout wraps the byte-chunk future, never
/// the parsed-event future (plan 001 §3.3; see [`Client::rc_events`]). 60s is
/// two missed heartbeats plus slack.
const RC_EVENTS_IDLE_TIMEOUT: Duration = Duration::from_secs(60);
/// Cap on the bytes buffered for a single rc-events SSE event, matching the
/// broker bus's `MAX_SSE_EVENT_BYTES` (`shed-broker/src/bus.rs`): 1 MiB, the
/// same bound Go's `bufio.Scanner` enforces. rc-events is a long-lived stream
/// carrying guest-influenced payloads (unlike the bounded one-shot create
/// stream, which stays uncapped), so an oversized / never-terminating event
/// surfaces as an error → the watcher disconnects + reconnects, instead of
/// buffering unboundedly.
const RC_EVENTS_MAX_EVENT_BYTES: usize = 1 << 20;
const USER_AGENT: &str = concat!("shed-desktop-core/", env!("CARGO_PKG_VERSION"));

/// Sink for create progress. shed-core streams the SSE and drives these; the FFI
/// layer implements it to update a create-status store the Swift side polls.
pub trait CreateSink: Send + Sync {
    fn on_progress(&self, message: String);
    fn on_complete(&self, shed: Shed);
    fn on_error(&self, message: String);
}

/// Sink for the rc-events live-activity stream (mirrors [`CreateSink`]):
/// shed-core drives the SSE connection ([`Client::rc_events`]) and hands each
/// decoded [`RcEvent`] here in arrival order; the fold + reconnect layer
/// (shed-app's `RcEventsWatcher`) implements it.
pub trait RcEventSink: Send + Sync {
    fn on_event(&self, ev: RcEvent);
}

/// One completed send, plus the transport it went out on.
///
/// Holding the transport is load-bearing for the STREAMING callers: a
/// `reqwest::Response`'s body is fed by a connection its client owns, so a stream
/// must outlive the `Arc` that dialed it — including across a
/// [`Client::recycle_transport`] that replaces the shared one mid-stream.
struct Sent {
    resp: reqwest::Response,
    transport: Arc<reqwest::Client>,
}

/// What a credential resolution produced: what to present, and — when a mint
/// failed — why there is nothing better to present.
///
/// The pairing is what lets a failed mint be neither discarded nor fatal. The
/// error travels WITH the request instead of replacing it, because whether it
/// matters is the server's answer to give: an `auth.mode: open` server (which
/// shed-app also wires a minter to, and for which minting necessarily fails —
/// there is no credential to issue) answers the unauthenticated request
/// perfectly well, while an enforcing server refuses it and the carried error
/// is precisely the explanation the refusal lacks. See [`Client::credential`]
/// and [`Client::send_authed`].
#[derive(Default)]
struct Resolution {
    /// The credential to present: freshly minted, cached, or the SURVIVOR of a
    /// failed mint. `None` means the request goes out unauthenticated.
    cred: Option<crate::token::Credential>,
    /// The mint failure behind a missing or merely surviving credential.
    mint_error: Option<ShedError>,
}

impl Resolution {
    /// A resolution with nothing to explain: this credential is what to present.
    fn presenting(cred: crate::token::Credential) -> Self {
        Self {
            cred: Some(cred),
            mint_error: None,
        }
    }
}

/// The error a REFUSED request reports when a mint failure is what left it
/// unable to authenticate.
///
/// The mint error leads (it is the actionable half — "upgrade
/// shed-host-agent", "the SSH bootstrap is unreachable"), with the refusal
/// itself as context so the reader can still see WHAT the server said: an HTTP
/// status, or the flattened TLS alert of a handshake-level refusal. Leading
/// with the mint message is load-bearing, not cosmetic: banners truncate from
/// the END, and refusal-first ordering was exactly how the actionable clause
/// got cut off (shed#300). The mint error's variant is preserved so a consumer
/// mapping [`ShedError`] onto its own error surface (the FFI, the Swift/Tauri
/// clients) keeps the same category.
fn refused_because(outcome: &Result<reqwest::Response, ShedError>, mint: ShedError) -> ShedError {
    let refusal = match outcome {
        Ok(resp) => format!("HTTP {}", resp.status().as_u16()),
        Err(e) => e.to_string(),
    };
    let describe = |m: String| format!("{m} (request refused: {refusal})");
    match mint {
        ShedError::Config(m) => ShedError::Config(describe(m)),
        ShedError::Transport(m) => ShedError::Transport(describe(m)),
        ShedError::AgentUpgradeRequired { server, detail } => ShedError::AgentUpgradeRequired {
            server,
            detail: format!("{detail}; request refused: {refusal}"),
        },
        other => ShedError::Transport(describe(other.to_string())),
    }
}

/// Annotate a TRANSPORT-LEVEL wire failure with the mint failure that left the
/// request unauthenticated. The wire error keeps its identity (it is what
/// actually happened on the socket); the mint error rides along as context,
/// because an unclassified handshake refusal plus "credential setup failed:
/// upgrade shed-host-agent" is diagnosable where either alone is not. Only for
/// `Err` outcomes — never for an HTTP status (see the call site's comment).
fn with_mint_context(wire: &ShedError, mint: &ShedError) -> ShedError {
    // A `Transport` keeps its inner message rather than its `Display` so the
    // annotated form is not re-prefixed with "transport error: ".
    let wire = match wire {
        ShedError::Transport(m) => m.clone(),
        other => other.to_string(),
    };
    ShedError::Transport(format!(
        "{wire}; additionally, credential setup failed: {mint}"
    ))
}

/// Report a refusal the retry machinery decided not to act on: the mint error
/// that explains it when there is one ([`refused_because`]), the refusal itself
/// otherwise.
fn refuse(
    outcome: Result<reqwest::Response, ShedError>,
    mint_error: Option<ShedError>,
    sent: impl FnOnce(reqwest::Response) -> Sent,
) -> Result<Sent, ShedError> {
    match mint_error {
        Some(e) => Err(refused_because(&outcome, e)),
        None => outcome.map(sent),
    }
}

/// One attempt: build the request against `http`, apply `bearer`, send. The
/// response is returned WHATEVER its status — classification is the caller's
/// (a 401 has to be told apart from a 404, and only the caller knows which
/// statuses are success for its route).
async fn send_once(
    http: &reqwest::Client,
    build: &impl Fn(&reqwest::Client) -> reqwest::RequestBuilder,
    bearer: Option<&str>,
) -> Result<reqwest::Response, ShedError> {
    let mut req = build(http);
    if let Some(tok) = bearer {
        req = req.bearer_auth(tok);
    }
    req.send()
        .await
        .map_err(|e| ShedError::Transport(crate::authfail::flatten(&e)))
}

/// A read client for one shed-server host. `Clone` is cheap (the transport and
/// the token provider are Arc-backed) so a create task can own its own handle
/// sharing the same token cache.
pub struct Client {
    base_url: String,
    server_name: String,
    /// Static open-mode config token; used only when there is no `token_provider`.
    token: String,
    token_provider: Option<Arc<ControlTokenProvider>>,
    /// The transport. Built once, and replaced only by
    /// [`Client::recycle_transport`] — never by a credential change.
    ///
    /// `Arc`-wrapped on top of reqwest's own internal `Arc` for one reason: it
    /// gives the transport contract an ASSERTABLE identity. The rotation test
    /// compares `Arc::as_ptr` across a credential swap, so "we did not rebuild
    /// the client" is proven rather than asserted in a comment; the pooled-
    /// revocation test compares it the other way, so the pool purge is proven
    /// too.
    ///
    /// The extra `RwLock` exists for ONE operation — [`Client::recycle_transport`],
    /// the connection-pool purge a refused CERTIFICATE forces. Every read path
    /// clones the inner `Arc` once per request and works on that, so a recycle can
    /// never yank a transport out from under an in-flight request or stream.
    http: std::sync::RwLock<Arc<reqwest::Client>>,
    /// The two inputs [`build_http_client`] needs, retained so the transport can
    /// be rebuilt identically (same pin, same resolver) when its pool has to go.
    pin: Option<String>,
    resolver: Arc<crate::tls::ClientCertResolver>,
}

impl Clone for Client {
    /// Clones share the token cache and the resolver, and each takes the CURRENT
    /// transport. A later recycle in one handle is not seen by the other — which
    /// is the same "each handle keeps the pool it is using" property clones had
    /// before, and is safe because a stale pool is self-correcting: the first
    /// rejection on it recycles that handle too.
    fn clone(&self) -> Self {
        Self {
            base_url: self.base_url.clone(),
            server_name: self.server_name.clone(),
            token: self.token.clone(),
            token_provider: self.token_provider.clone(),
            http: std::sync::RwLock::new(self.transport()),
            pin: self.pin.clone(),
            resolver: self.resolver.clone(),
        }
    }
}

impl Client {
    /// `base_url` is injected by the app. `token` is the static open-mode config
    /// token (sent only when there's no minter). `pin` (`sha256:<hex>`) enables
    /// leaf pinning; a pin on a non-https URL is refused (fail-closed). `minter`,
    /// when present, backs a control-token FSM whose minted token is sent — and
    /// on a mint failure NO token is sent (never the static one; no downgrade).
    pub fn new(
        base_url: String,
        server_name: String,
        token: String,
        pin: Option<String>,
        minter: Option<Arc<dyn TokenMinter>>,
    ) -> Result<Self, ShedError> {
        let token_provider =
            minter.map(|m| Arc::new(ControlTokenProvider::new(server_name.clone(), m)));
        Self::build(base_url, server_name, token, pin, token_provider)
    }

    /// Like [`Self::new`] but with a caller-tuned [`ControlTokenProvider`]
    /// (plan 001 §3.4 Client plumbing): Phase B builds a provider with its
    /// jitter/cooldown/seed/window knobs and hands it over; `new` keeps
    /// constructing the default-knobbed provider from a bare minter.
    /// Provider-backed by definition, so there is no static-token parameter.
    ///
    /// A `Client`+provider pair is immutable per transport identity — the
    /// Dart provider's identity binding is deliberately NOT ported (§3.4):
    /// the app layer constructs a NEW `Client` (and provider) when the
    /// host/port/pin changes, deleting the identity-race class instead of
    /// managing it (a Phase B invariant).
    pub fn with_provider(
        base_url: String,
        server_name: String,
        pin: Option<String>,
        provider: Arc<ControlTokenProvider>,
    ) -> Result<Self, ShedError> {
        Self::build(base_url, server_name, String::new(), pin, Some(provider))
    }

    /// Shared tail of the constructors: pin validation (fail-closed on a
    /// non-https URL) + the reqwest client build.
    fn build(
        base_url: String,
        server_name: String,
        token: String,
        pin: Option<String>,
        token_provider: Option<Arc<ControlTokenProvider>>,
    ) -> Result<Self, ShedError> {
        let pin = pin.filter(|p| !p.is_empty());
        if pin.is_some() && !base_url.to_lowercase().starts_with("https://") {
            return Err(ShedError::Config(format!(
                "TLS pin configured for a non-https URL {base_url}; refusing to send unpinned plaintext"
            )));
        }
        // The adaptive transport (plan 001 D5): the client-certificate resolver is
        // installed ALWAYS — the provider's when there is one, an empty one
        // otherwise — so a credential that arrives (or changes shape) later needs
        // no new client. An empty resolver behaves exactly like the previous
        // `.with_no_client_auth()` build.
        let resolver = token_provider
            .as_ref()
            .map(|p| p.cert_resolver())
            .unwrap_or_default();
        let http = std::sync::RwLock::new(Arc::new(build_http_client(
            pin.as_deref(),
            resolver.clone(),
        )?));
        Ok(Self {
            base_url,
            server_name,
            token,
            token_provider,
            http,
            pin,
            resolver,
        })
    }

    /// The transport to use for one request or stream. Cloning the `Arc` up front
    /// means the caller keeps ITS transport for the whole exchange even if a
    /// concurrent rejection recycles the shared one.
    fn transport(&self) -> Arc<reqwest::Client> {
        self.http.read().unwrap_or_else(|e| e.into_inner()).clone()
    }

    /// Replace the connection pool: build a fresh `reqwest::Client` (same pin,
    /// same certificate resolver), install it as this handle's transport, and
    /// return it for the retry to use.
    ///
    /// # The pooled-identity problem, and why this is the fix
    ///
    /// In mtls mode the credential is presented at the HANDSHAKE, so it is a
    /// property of the CONNECTION, not of the request. When a server refuses that
    /// identity — an expired certificate, or one whose SSH key left the allowlist,
    /// caught by shed-server's per-request re-validation — re-minting is only half
    /// a recovery. The retry is checked out of the pool onto the SAME connection,
    /// which still presents the OLD certificate, and is refused again; worse, that
    /// connection stays in the pool and is re-used by every LATER request, so each
    /// one costs a rejection plus a mint (and a mint can be a real SSH round trip,
    /// or a Touch ID prompt) forever. The pool has to lose it.
    ///
    /// Go does this with `Transport.CloseIdleConnections()` after a refresh.
    /// reqwest 0.12 has no equivalent at any layer, which is why this is a rebuild
    /// rather than a purge:
    ///
    ///   * there is no `close_idle_connections` on `reqwest::Client` (checked
    ///     against 0.12.28) and no access to the underlying hyper pool;
    ///   * varying a header does NOT re-key the pool (pooling is keyed on
    ///     scheme/host/port + proxy), so a "connection epoch" header buys nothing;
    ///   * a custom connector cannot invalidate connections that are already IN
    ///     the pool — the pool sits above it;
    ///   * `pool_max_idle_per_host(0)` would prevent the problem, but by paying a
    ///     full TLS handshake on every request forever — including for the entire
    ///     token-mode fleet, which has no pooled-identity problem at all — to fix
    ///     a path taken only on a rare failure;
    ///   * a THROWAWAY client for just the retry fixes the one request and leaves
    ///     the poisoned connection in the main pool, i.e. the mint-per-request
    ///     steady state above. (Verified, not assumed: the pooled connection is
    ///     genuinely re-used — see
    ///     `mtls_tests::a_revoked_identity_recovers_on_a_pooled_keepalive_connection`,
    ///     which asserts the refused identity handshook exactly once, i.e. its
    ///     rejection really did arrive on a POOLED connection.)
    ///
    /// Dropping the old client drops its pool; connections still in use are
    /// unaffected, because every caller holds the `Arc` it started with (see
    /// [`Sent`], which keeps a streaming response's transport alive).
    ///
    /// # Scope: only when a certificate is involved
    ///
    /// [`Self::send_authed`] calls this only when the credential on either side of
    /// the re-mint is an mtls one — either the refused connection presented a
    /// certificate, or the freshly minted credential is one that can only be
    /// presented at a handshake (so the pooled, certificate-less connections are
    /// equally useless). A token-mode 401 recycles nothing: a bearer travels per
    /// request, so the pool is not part of the credential and throwing away good
    /// connections would be pure cost.
    ///
    /// # Relationship to the immutable-transport contract
    ///
    /// The contract (plan 001 D5) is that a credential CHANGE — a rotation, a
    /// token↔mtls flip — needs no transport rebuild and no swap owned by any layer
    /// above. That still holds exactly: rotations and flips write through the
    /// shared resolver, the `Client` object, its provider and its resolver are
    /// never replaced, and no caller observes anything. What is scoped out of the
    /// contract here is the one case it never covered: a pool holding connections
    /// authenticated as an identity the server has REFUSED. Those connections are
    /// not reusable state, they are stale credentials.
    ///
    /// Belt-and-braces with the server-side half: shed-server answers an
    /// identity-rejection 401 with `Connection: close`
    /// (`internal/api/auth.go:closeIdentityBoundConnection`), which fixes this for
    /// every client, in every language. This path is what makes the Rust client
    /// correct against a server that does not.
    fn recycle_transport(&self) -> Result<Arc<reqwest::Client>, ShedError> {
        let fresh = Arc::new(build_http_client(
            self.pin.as_deref(),
            self.resolver.clone(),
        )?);
        *self.http.write().unwrap_or_else(|e| e.into_inner()) = fresh.clone();
        Ok(fresh)
    }

    /// The transport's identity — the address of the `reqwest::Client` currently
    /// backing this handle. Only for tests asserting the transport contract: it
    /// must not change for a credential rotation or a mode flip, and it changes
    /// exactly once when a refused certificate forces a pool purge
    /// ([`Self::recycle_transport`]).
    #[cfg(test)]
    pub(crate) fn transport_id(&self) -> usize {
        Arc::as_ptr(&self.transport()) as usize
    }

    /// The bearer token to send, or `None`. Provider-backed clients send the
    /// minted token, or the surviving one when the mint failed with something
    /// still held, or NOTHING when neither exists (never the static token — no
    /// secure-by-default downgrade); provider-less clients send the static token.
    /// The mint error that explains that last state rides along with the request
    /// ([`Resolution`]) rather than showing up here.
    ///
    /// Test-only since the request path was unified on
    /// [`Self::send_authed`], which pins the whole CREDENTIAL (the header is only
    /// half of one in mtls mode) rather than just its bearer half. Tests keep it
    /// as the shortest way to ask "what would the next request send?".
    #[cfg(test)]
    pub(crate) async fn bearer(&self) -> Option<String> {
        self.credential()
            .await
            .cred
            .and_then(|c| c.bearer_token().map(str::to_string))
    }

    /// The provider's current credential, resolving (minting) it if needed.
    ///
    /// This is where an mtls credential is obtained too: nothing about the call
    /// site changes, because the certificate leaves through the resolver rather
    /// than through this return value. A provider-less client has no credential to
    /// resolve — its static token is applied by [`Self::bearer`] instead.
    ///
    /// # A failed mint is CARRIED, not discarded and not raised
    ///
    /// Three outcomes, and the difference between the last two is the point:
    ///
    ///   * a credential to present, freshly minted or cached;
    ///   * the mint FAILED but the provider still holds something usable
    ///     ([`crate::token::ControlTokenProvider::surviving_credential`]) —
    ///     present the survivor and let the request proceed (Go parity,
    ///     `66abaa9`: "presenting something the server might still accept beats
    ///     presenting nothing"). The error rides along so a refusal can name it;
    ///   * the mint failed and NOTHING usable is held — the request still goes
    ///     out, unauthenticated, with the error riding along.
    ///
    /// That last case used to `.ok()`-discard the error, which is why an mtls
    /// server answered by a host-agent too old to mint a certificate reported a
    /// bare 401 instead of "upgrade shed-host-agent". Raising it here instead
    /// would be the opposite bug: shed-app installs the shared minter on EVERY
    /// server (`shed-app/src/backend.rs`), including `auth.mode: open` ones,
    /// where the mint fails because there is no credential to issue and the
    /// unauthenticated request is exactly right. So the error is carried and
    /// only becomes the caller's error if the server actually refuses the
    /// attempt — see [`Self::send_authed`], which is where the two cases
    /// separate, and [`refused_because`], which shapes the message.
    ///
    /// # Accepted: a survivor the server still accepts hides its mint error
    ///
    /// While the server keeps accepting the surviving credential the mint error
    /// stays invisible, and each request pays one failed mint attempt (bounded:
    /// one per request, cooled down by the provider). That is the same trade the
    /// Go side accepts, and it is not degraded service — the requests work. The
    /// moment the server refuses the survivor the error surfaces automatically
    /// through the refusal path below.
    async fn credential(&self) -> Resolution {
        match &self.token_provider {
            Some(p) => match p.credential().await {
                Ok(cred) => Resolution::presenting(cred),
                Err(e) => match p.surviving_credential().await {
                    Some(surviving) => Resolution {
                        cred: Some(surviving),
                        mint_error: Some(e),
                    },
                    None => Resolution {
                        cred: None,
                        mint_error: Some(e),
                    },
                },
            },
            None if !self.token.is_empty() => Resolution::presenting(crate::token::Credential {
                mode: Some(crate::token::AuthMode::Token),
                token: self.token.clone(),
                ..Default::default()
            }),
            None => Resolution::default(),
        }
    }

    /// [`Self::credential`] BOUNDED by `bound`, mirroring the connect-phase guard
    /// [`Self::rc_events`] already applies: a foreign [`crate::TokenMinter`] impl
    /// can hang indefinitely (the provider holds its mutex across a mint), so an
    /// unbounded credential resolution in a JSON/lifecycle request or a create
    /// would wedge the whole call. On timeout surface a Transport error rather
    /// than block forever. Used by [`Self::request`] for both the initial and the
    /// retry resolution.
    ///
    /// The timeout is the ONLY error it can produce: a mint failure is carried
    /// inside the [`Resolution`], not raised (see [`Self::credential`]).
    async fn bounded_credential(&self, bound: Duration) -> Result<Resolution, ShedError> {
        // The message names the stage that stalled: this timeout fires when the
        // MINTER (shed-host-agent on desktop; the SSH bootstrap elsewhere) sat on
        // the resolution past the request bound, and "credential resolution" alone
        // read as a generic transport stall (shed#297) — pointing the operator at
        // the network instead of the agent.
        tokio::time::timeout(bound, self.credential())
            .await
            .map_err(|_| {
                ShedError::Transport(
                    "credential resolution timed out; the credential minter \
                     (shed-host-agent on desktop) did not answer in time"
                        .into(),
                )
            })
    }

    /// Reactive invalidation of the credential a request actually used, BOUNDED
    /// by `bound`: invalidating only contends on the provider mutex, but that
    /// mutex is held across a mint by design, so a concurrent wedged mint could
    /// otherwise pin the await forever. `false` means the bound elapsed and the
    /// caller must not retry (it surfaces the original failure; the cache keeps
    /// the stale credential one extra cycle and the next failure retries the
    /// invalidate). No-op `true` for provider-less clients.
    ///
    /// What "if current" means differs by state and is the provider's call — the
    /// stale-401 guard in token state, unconditional in mtls state where a
    /// certificate cannot be attributed to a request (see
    /// [`crate::token::ControlTokenProvider::invalidate_if_current_credential`]).
    async fn invalidate_used(
        &self,
        used: Option<&crate::token::Credential>,
        bound: Duration,
    ) -> bool {
        let (Some(p), Some(cred)) = (&self.token_provider, used) else {
            return true;
        };
        tokio::time::timeout(bound, p.invalidate_if_current_credential(cred))
            .await
            .is_ok()
    }

    /// Build a request URL from literal path `segments` and `query` pairs.
    /// Each segment is appended via the url crate's `path_segments_mut`, which
    /// percent-encodes it as exactly ONE path segment (a `/` inside a value
    /// becomes `%2F`, never a new segment), and bare `""`/`.`/`..` segments
    /// are rejected outright (a `..` that survived to the wire would be
    /// dot-normalized by the server's router into a DIFFERENT route — e.g. a
    /// session-delete crossing into a shed-delete). Identifiers here are
    /// server-vended (validated shed/session names, hub-generated slugs), but
    /// the client enforces one-segment encoding anyway — defense in depth,
    /// matching mobile's Dart client, which component-encodes every segment.
    fn build_url(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
    ) -> Result<reqwest::Url, ShedError> {
        let mut url = reqwest::Url::parse(&self.base_url)
            .map_err(|e| ShedError::Config(format!("invalid base URL {}: {e}", self.base_url)))?;
        {
            let mut parts = url.path_segments_mut().map_err(|_| {
                ShedError::Config(format!("base URL {} cannot carry a path", self.base_url))
            })?;
            parts.pop_if_empty(); // tolerate a trailing slash on base_url
            for seg in segments {
                if seg.is_empty() || *seg == "." || *seg == ".." {
                    return Err(ShedError::Config(format!(
                        "invalid URL path segment {seg:?}"
                    )));
                }
                parts.push(seg);
            }
        }
        for (k, v) in query {
            url.query_pairs_mut().append_pair(k, v);
        }
        Ok(url)
    }

    /// Send once, and on a provider-backed AUTH-SHAPED failure re-mint the
    /// credential and retry once, ON A FRESH CONNECTION (at-most-once, mirrors
    /// the SDK/CLI).
    ///
    /// This is the ONE re-auth path in this client. The unary JSON/lifecycle
    /// requests ([`Self::request`]) and both SSE streams
    /// ([`Self::create_stream`], [`Self::rc_events`]) route through it, so a
    /// rejected credential recovers identically whether it was refused with a 401
    /// or with a TLS alert, and whether the response was going to be a JSON body
    /// or an event stream. (The streams previously classified nothing and only
    /// invalidated a BEARER token, which is always `None` in mtls state — a
    /// revoked certificate left them failing forever.)
    ///
    /// "Auth-shaped" is [`crate::authfail::is_auth_failure`]: an HTTP 401, or a
    /// peer TLS alert naming a certificate problem. Both shapes are handled the
    /// same way and REGARDLESS of the mode this client believes it is in — that
    /// symmetry is what makes a server-side token↔mtls flip recover with no
    /// operator action, in either direction (plan 001 D5).
    ///
    /// The retry is SKIPPED — surfacing the refusal — when:
    ///   * the attempt had NOTHING to present because the mint failed
    ///     ([`Resolution::mint_error`] with no credential). The refusal IS that
    ///     mint failure, observed one round trip later, so it is reported as
    ///     such ([`refused_because`]) and the mint is NOT re-attempted: it
    ///     failed moments ago, and a provider inside its failure cooldown would
    ///     answer the redacted marker, replacing the actionable message with
    ///     "control token mint failed". Exactly one mint attempt per request in
    ///     that state;
    ///   * the invalidate timed out (a wedged mint holds the provider mutex;
    ///     the follow-up resolution would block on the same mutex);
    ///   * the re-resolved credential is missing or unusable — a provider-backed
    ///     request never retries UNAUTHENTICATED: the re-mint failed, and
    ///     dropping the credential is a guaranteed second rejection;
    ///   * the re-resolved credential has the same identity as the rejected one
    ///     (same bearer token, or same certificate serial) — a guaranteed second
    ///     rejection. The successful same-credential re-mint also re-CACHED the
    ///     rejected value, so it is invalidated again before returning — the
    ///     must-mint flag stays armed and no later call serves it.
    ///
    /// In every one of those skips the reported error is the mint failure when
    /// there is one — the freshest one, i.e. the re-mint's over the one carried
    /// in — and the plain refusal otherwise. "The host-agent cannot mint a
    /// replacement" is the actionable half of "the server refused what we had",
    /// and Go says the same: `cmd/shed`'s `reauthenticated` returns a failed
    /// re-mint rather than the 401 that triggered it.
    ///
    /// Ahead of all that, the ambiguous "connection was not ready" shape
    /// ([`crate::authfail::is_connection_lost_message`]) gets ONE plain re-send:
    /// the request was never dispatched, so re-sending is safe for any method, and
    /// a fresh connection turns the ambiguity into either success or a
    /// classifiable alert. It deliberately does NOT re-mint — that shape is also
    /// what a server restart produces, and a needless mint can cost a real SSH
    /// round-trip (and, on desktop, a Touch ID prompt).
    ///
    /// `resolved` is what this attempt goes out with — the captured credential
    /// (if any) plus the mint error that explains its absence or staleness. Note
    /// that the capture is authoritative only for the HEADER: in mtls state the
    /// certificate is read live from the resolver at handshake time, which is
    /// why invalidation there is attribution-free (see
    /// [`Self::invalidate_used`]).
    ///
    /// # Pool purge on an mtls invalidation, re-mint or not
    ///
    /// When the credential this attempt USED was a certificate, the pool is
    /// purged ([`Self::recycle_transport`]) as soon as the invalidation lands —
    /// not only when a replacement is successfully minted. Those idle pooled
    /// connections were all dialed under the identity the server just refused,
    /// so they are stale credentials whether or not a replacement exists; a
    /// failed re-mint used to leave every one of them checked in, ready to
    /// re-present the refused certificate on the next request. (shed-server's
    /// `Connection: close` on an identity-bound 401 only closes the ONE
    /// connection the refusal came back on, and a TLS-alert refusal has no
    /// response to carry a header at all.)
    ///
    /// `build` is invoked per attempt against the transport the attempt uses, so
    /// the retry can be issued on [`Self::fresh_transport`] — that is what makes
    /// the retry a NEW connection carrying the NEW certificate rather than a
    /// pooled one still presenting the rejected identity.
    async fn send_authed(
        &self,
        bound: Duration,
        resolved: Resolution,
        build: impl Fn(&reqwest::Client) -> reqwest::RequestBuilder,
    ) -> Result<Sent, ShedError> {
        let Resolution {
            cred: used,
            mint_error,
        } = resolved;
        let bearer = used
            .as_ref()
            .and_then(|c| c.bearer_token().map(str::to_string));
        let transport = self.transport();
        let mut outcome = send_once(&transport, &build, bearer.as_deref()).await;
        if matches!(&outcome, Err(ShedError::Transport(msg)) if crate::authfail::is_connection_lost_message(msg))
        {
            outcome = send_once(&transport, &build, bearer.as_deref()).await;
        }

        let auth_shaped = match &outcome {
            Ok(resp) => crate::authfail::is_auth_failure(Some(resp.status().as_u16()), None),
            Err(e) => crate::authfail::is_auth_failure(None, Some(e)),
        };
        let sent = |resp| Sent {
            resp,
            transport: transport.clone(),
        };
        if !auth_shaped || self.token_provider.is_none() {
            // The mint-error promotion below runs only for auth-shaped refusals,
            // so this `!auth_shaped` return fires FIRST and would silently drop a
            // carried mint error (the control-flow subtlety of shed#297's
            // secondary hole — do not "simplify" the promotion above this
            // return). When the wire itself failed and nothing was presented,
            // the failed mint is context the raw error lacks: append it WITHOUT
            // replacing the wire error's identity. A `BadStatus` (404/500) or a
            // healthy response keeps today's behavior — an open-mode server
            // answers fine while carrying a mint error, and an unrelated HTTP
            // status must never be masked by a host-agent message.
            if let (Err(wire), None, Some(mint)) = (&outcome, &used, &mint_error) {
                return Err(with_mint_context(wire, mint));
            }
            return outcome.map(sent);
        }
        // Nothing was presented because the mint failed: report THAT, and do not
        // re-mint what just failed (see the skip list above).
        let mint_error = match (used.as_ref(), mint_error) {
            (None, Some(e)) => return Err(refused_because(&outcome, e)),
            (_, carried) => carried,
        };
        if !self.invalidate_used(used.as_ref(), bound).await {
            // Wedged provider — no retry.
            return refuse(outcome, mint_error, sent);
        }
        // The refused identity was connection-bound, so every idle pooled
        // connection is carrying it. Purge them on the INVALIDATION, before the
        // re-mint that may not succeed (see the pool-purge note above). A
        // token-state refusal recycles nothing: a bearer travels per request.
        let purged = match used.as_ref().and_then(|c| c.mode) {
            Some(crate::token::AuthMode::Mtls) => Some(self.recycle_transport()?),
            _ => None,
        };
        let Resolution {
            cred: fresh,
            mint_error: remint_error,
        } = self.bounded_credential(bound).await?;
        let Some(fresh) = fresh.filter(crate::token::Credential::usable) else {
            // Never retry unauthenticated; the freshest explanation wins.
            return refuse(outcome, remint_error.or(mint_error), sent);
        };
        if used.as_ref().map(|c| c.identity()) == Some(fresh.identity()) {
            // Re-arm the must-mint flag the same-credential re-mint just cleared,
            // so the rejected credential is never served again. (A survivor can
            // land here too — same identity, and the mint that would have
            // replaced it failed — which is why the re-mint error is reported.)
            let _ = self.invalidate_used(used.as_ref(), bound).await;
            return refuse(outcome, remint_error, sent);
        }
        // Drop the refused response BEFORE retrying, so a 401 whose body is still
        // unread cannot hold its connection open behind the retry.
        drop(outcome);
        // The retry rides the pool purged above; a token→mtls flip purges here
        // instead, because the certificate can only be presented at a handshake
        // and every pooled connection is certificate-less. A token→token re-mint
        // keeps the pool — a bearer is not part of the connection.
        let retry_transport = match purged {
            Some(fresh_pool) => fresh_pool,
            None if matches!(fresh.mode, Some(crate::token::AuthMode::Mtls)) => {
                self.recycle_transport()?
            }
            None => transport,
        };
        let resp = send_once(&retry_transport, &build, fresh.bearer_token()).await?;
        Ok(Sent {
            resp,
            transport: retry_transport,
        })
    }

    /// [`Self::send_authed`] with the credential resolved here, bounded by the
    /// same `bound` (a foreign [`crate::TokenMinter`] impl can hang, and the
    /// provider holds its mutex across a mint).
    ///
    /// A mint failure does not abort the call here — it rides along in the
    /// [`Resolution`] and becomes the error only if the server refuses the
    /// attempt ([`Self::send_authed`]). Every path reaches the wire through this
    /// function (unary JSON/lifecycle via [`Self::request`], and both SSE
    /// streams, [`Self::create_stream`] and [`Self::rc_events`]), so all three
    /// get that behavior identically: open servers keep working, and a refusal
    /// carries the actionable message instead of a bare 401.
    async fn send_resolved(
        &self,
        bound: Duration,
        build: impl Fn(&reqwest::Client) -> reqwest::RequestBuilder,
    ) -> Result<Sent, ShedError> {
        let resolved = self.bounded_credential(bound).await?;
        self.send_authed(bound, resolved, build).await
    }

    /// A JSON/lifecycle request: [`Self::send_resolved`] plus the status check and
    /// body read. `body`, when present, is a JSON request body (re-sent on the
    /// retry). Static/no-credential clients don't retry.
    async fn request(
        &self,
        method: reqwest::Method,
        url: &reqwest::Url,
        timeout: Duration,
        body: Option<&serde_json::Value>,
    ) -> Result<Vec<u8>, ShedError> {
        // The whole call — credential resolution included — is bounded by this
        // request's own `timeout` (GET 8s / WRITE 15s).
        let sent = self
            .send_resolved(timeout, |http| {
                let mut req = http.request(method.clone(), url.clone()).timeout(timeout);
                if let Some(b) = body {
                    req = req.json(b);
                }
                req
            })
            .await?;
        let status = sent.resp.status().as_u16();
        if !(200..300).contains(&status) {
            return Err(ShedError::BadStatus(status));
        }
        Ok(sent
            .resp
            .bytes()
            .await
            .map_err(|e| ShedError::Transport(crate::authfail::flatten(&e)))?
            .to_vec())
    }

    /// GET the segment-built URL ([`Self::build_url`]) and decode JSON.
    async fn get_json<T: serde::de::DeserializeOwned>(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
    ) -> Result<T, ShedError> {
        let url = self.build_url(segments, query)?;
        let bytes = self
            .request(reqwest::Method::GET, &url, GET_TIMEOUT, None)
            .await?;
        serde_json::from_slice(&bytes).map_err(|e| ShedError::Decode(e.to_string()))
    }

    /// A lifecycle mutation (POST/DELETE, no request body; any response body
    /// ignored — success is any 2xx). 15s timeout.
    async fn lifecycle(&self, method: reqwest::Method, segments: &[&str]) -> Result<(), ShedError> {
        let url = self.build_url(segments, &[])?;
        self.request(method, &url, WRITE_TIMEOUT, None)
            .await
            .map(|_| ())
    }

    /// `GET /api/info`.
    pub async fn info(&self) -> Result<ServerInfo, ShedError> {
        self.get_json(&["api", "info"], &[]).await
    }

    /// `GET /api/sheds` -> sheds stamped with this host's config name (the server
    /// omits `host`; the client stamps it, as Swift's `listSheds` does).
    pub async fn list_sheds(&self) -> Result<Vec<Shed>, ShedError> {
        let list: ShedList = self.get_json(&["api", "sheds"], &[]).await?;
        Ok(list
            .sheds
            .into_iter()
            .map(|mut s| {
                s.host = self.server_name.clone();
                s
            })
            .collect())
    }

    /// `GET /api/system/df`.
    pub async fn system_df(&self) -> Result<SystemDiskUsage, ShedError> {
        self.get_json(&["api", "system", "df"], &[]).await
    }

    /// `GET /api/images`.
    pub async fn list_images(&self) -> Result<Vec<ShedImage>, ShedError> {
        let list: ImageList = self.get_json(&["api", "images"], &[]).await?;
        Ok(list.images)
    }

    /// `GET /api/egress/profiles`.
    pub async fn egress_profiles(&self) -> Result<Vec<EgressProfileInfo>, ShedError> {
        self.get_json(&["api", "egress", "profiles"], &[]).await
    }

    // Path-building note: every method below hands `shed`/`session`/`slug`
    // values to `build_url`, which percent-encodes each as exactly ONE path
    // segment and rejects ""/"."/"..". The values are server-vended
    // identifiers (validated shed/session names, hub-generated slugs), but
    // the client enforces one-segment encoding anyway — defense in depth
    // against a traversal like `delete_session(shed, "../../victim")`
    // rewriting the route (mobile's Dart client component-encodes too).

    /// `GET /api/overview` — the single-call host snapshot (server identity +
    /// features, disk usage, every shed with its rc-enriched sessions and
    /// capabilities; Go `internal/api/overview.go:38-63`). The decode is the
    /// tolerant, never-failing [`Overview`] adapter.
    ///
    /// On an old server (pre-`overview`) the unrouted path falls through to
    /// chi's default NotFound handler — a `text/plain` "404 page not found"
    /// body that the server's ContentTypeJSON middleware has already labeled
    /// `application/json`. That surfaces here as `BadStatus(404)`, never a
    /// `Decode` error: the non-2xx check short-circuits before any body parse.
    /// Don't feature-probe with this 404 — the reliable capability signal is
    /// `ServerInfo::features` from the unauthenticated `/api/info` bootstrap
    /// call ([`crate::models::FEATURE_OVERVIEW`]).
    pub async fn overview(&self) -> Result<Overview, ShedError> {
        self.get_json(&["api", "overview"], &[]).await
    }

    /// `GET /api/sheds/{shed}/sessions` — the shed's tmux sessions, rc-enriched
    /// by default (Go `internal/api/handlers.go:592-610`; wire shapes
    /// `internal/config/types.go:182-215, 287-291`). `warnings` carries
    /// enrichment degradations (the rc rows then lack their `rc` block).
    /// Errors are status-only per `mapSessionError`
    /// (`handlers.go:765-786`): 404 unknown shed, 409 shed stopped, 503 tmux
    /// unavailable.
    pub async fn list_sessions(&self, shed: &str) -> Result<SessionsResponse, ShedError> {
        self.get_json(&["api", "sheds", shed, "sessions"], &[])
            .await
    }

    /// `DELETE /api/sheds/{shed}/sessions/{session}` — kill one tmux session
    /// (Go `internal/api/handlers.go:614-632`). The server replies 204; any
    /// 2xx is success (consistent with the other lifecycle mutations). Errors
    /// are status-only per `mapSessionError` (`handlers.go:765-786`): 400
    /// invalid session name, 404 unknown session/shed, 409 shed stopped, 503
    /// tmux unavailable.
    pub async fn delete_session(&self, shed: &str, session: &str) -> Result<(), ShedError> {
        self.lifecycle(
            reqwest::Method::DELETE,
            &["api", "sheds", shed, "sessions", session],
        )
        .await
    }

    /// `GET /api/sheds/{shed}/rc/v1/sessions/{slug}/messages?since=N[&limit=M]`
    /// — one page of an RC session's message feed, reverse-proxied into the
    /// guest's rc hub (proxy `internal/api/rchub.go:280-375`; hub handler
    /// `internal/ext/rc/hub.go:332-385`). `since` is the exclusive seq cursor
    /// (0 = from the start); `limit` defaults to 100 server-side (capped at
    /// 200) when `None`. Decode is the tolerant [`RcMessagesPage`].
    ///
    /// Errors are status-only (plan §3.2 — the hub's flat `{code,message}`
    /// bodies are deliberately not decoded): 400 malformed since/limit, 404
    /// unknown slug/shed, 503 shed not running / hub unavailable, 502 proxy
    /// failed / oversized upstream body.
    pub async fn rc_messages(
        &self,
        shed: &str,
        slug: &str,
        since: u64,
        limit: Option<u32>,
    ) -> Result<RcMessagesPage, ShedError> {
        let mut query = vec![("since", since.to_string())];
        if let Some(limit) = limit {
            query.push(("limit", limit.to_string()));
        }
        self.get_json(
            &[
                "api", "sheds", shed, "rc", "v1", "sessions", slug, "messages",
            ],
            &query,
        )
        .await
    }

    /// `POST /api/sheds/{shed}/rc/v1/sessions/{slug}/input` with
    /// `{"text": …}` — deliver a line of feed input to a gated RC session
    /// (proxy `internal/api/rchub.go:280-375`; hub handler
    /// `internal/ext/rc/hub.go:391-521`). Success is any 2xx; the 200 body
    /// (`{"delivered":true}`) is ignored. Goes through the standard `request`
    /// pipeline (WRITE_TIMEOUT, provider-backed 401 → invalidate +
    /// retry-once, body re-sent).
    ///
    /// Errors are status-only (plan §3.2 — hub `{code,message}` bodies not
    /// decoded; `BadStatus` carries the status): 400 invalid/unsafe text, 404
    /// unknown slug/shed, 409 not accepting (`not_accepting` — wrong
    /// activity, recreated identity, or a non-input-gated kind), 413 body too
    /// large (`too_large`, >16 KiB), 503 shed not running / hub unavailable,
    /// 502 proxy failed.
    pub async fn rc_input(&self, shed: &str, slug: &str, text: &str) -> Result<(), ShedError> {
        let body = serde_json::json!({ "text": text });
        let url = self.build_url(
            &["api", "sheds", shed, "rc", "v1", "sessions", slug, "input"],
            &[],
        )?;
        self.request(reqwest::Method::POST, &url, WRITE_TIMEOUT, Some(&body))
            .await
            .map(|_| ())
    }

    /// `GET /api/rc/events` with `Accept: text/event-stream` — the host-wide
    /// aggregate rc live-activity stream (Go `internal/api/rcevents.go:170-208`:
    /// the server opens with a `: ok` comment preamble, heartbeats every 25s
    /// with `: heartbeat` comments, and never sends a `retry:` hint — reconnect
    /// policy is entirely the client's). Each SSE record is decoded via
    /// [`parse_rc_event`] and delivered to `sink` in arrival order; a
    /// malformed/unknown frame is skipped WITHOUT ending the stream (the decode
    /// is tolerant by design — one bad guest frame must not become a reconnect
    /// storm), and comment frames never reach the sink (the parser swallows
    /// them). Success is exactly a 200 SSE response — any other status,
    /// including a body-less 2xx like 204, is `BadStatus` (an empty-stream Ok
    /// would mask the fault from the watcher). Returns `Ok(())` on clean EOF,
    /// after flushing any final unterminated record to the sink. The idle
    /// duration also bounds the whole connect phase (bearer mint + send).
    ///
    /// ONE connection, and no in-method RECONNECT: the reconnect loop (shed-app's
    /// `RcEventsWatcher`) owns backoff, and a stream that dies mid-flight is its
    /// problem, not this method's.
    ///
    /// The CONNECT, though, runs the standard re-auth path
    /// ([`Self::send_authed`]): a refused credential is invalidated, re-minted
    /// once, and the connect retried once. Leaving that to the watcher was a bug
    /// rather than a division of labour — the old code invalidated a BEARER token,
    /// which is `None` in mtls state, so a revoked or expired certificate meant
    /// every reconnect presented the same dead identity forever. An unrecoverable
    /// failure still surfaces (`BadStatus(401)` after the retry is refused too),
    /// so the watcher's Down/backoff signal is unchanged.
    ///
    /// Liveness (plan 001 §3.3, panel-critical pin): the
    /// [`RC_EVENTS_IDLE_TIMEOUT`] idle timer wraps the BYTE-chunk future
    /// (`bytes_stream().next()`, the `create_stream` pattern), NOT the
    /// parsed-event future — the server's 25s heartbeat comments arrive as
    /// bytes and reset it even though the parser emits no event for them. An
    /// event-level timer would falsely kill every healthy-but-quiet stream; the
    /// byte-level timer converts a silently-dead connection into
    /// disconnect → reconnect → resync (a liveness watchdog mobile's Dart loop
    /// lacks). Idle/transport failures surface as [`ShedError::Transport`]
    /// (`Create` is create-specific; to the reconnecting watcher every
    /// teardown is the same "connection died" condition). Events are parsed
    /// through a 1 MiB-capped [`SseParser`] ([`RC_EVENTS_MAX_EVENT_BYTES`]);
    /// an overflow is an error, ending the stream.
    pub async fn rc_events(&self, sink: &dyn RcEventSink) -> Result<(), ShedError> {
        self.rc_events_with_idle(sink, RC_EVENTS_IDLE_TIMEOUT).await
    }

    /// [`Self::rc_events`] with an injectable idle timeout — the test seam
    /// (deterministic timer tests must not wait out the 60s production value).
    pub(crate) async fn rc_events_with_idle(
        &self,
        sink: &dyn RcEventSink,
        idle: Duration,
    ) -> Result<(), ShedError> {
        self.rc_events_with_limits(sink, idle, RC_EVENTS_MAX_EVENT_BYTES)
            .await
    }

    /// The full rc-events implementation with both knobs injectable (the cap
    /// seam exists only for tests — an oversized-event test must not build a
    /// >1 MiB body).
    async fn rc_events_with_limits(
        &self,
        sink: &dyn RcEventSink,
        idle: Duration,
        max_event_bytes: usize,
    ) -> Result<(), ShedError> {
        let url = self.build_url(&["api", "rc", "events"], &[])?;
        // Connection-open timeout: the same duration bounds the WHOLE connect
        // phase — credential resolution (a foreign `TokenMinter` impl can hang
        // indefinitely), the request send, and the one re-auth attempt — so no
        // pre-stream await sits outside the bound (a server that accepts but
        // never responds is as dead as a silent stream).
        let connect = self.send_resolved(idle, |http| {
            http.get(url.clone())
                .header(reqwest::header::ACCEPT, "text/event-stream")
        });
        let sent = match tokio::time::timeout(idle, connect).await {
            Err(_) => {
                return Err(ShedError::Transport("rc-events connect timeout".into()));
            }
            Ok(r) => r?,
        };
        let status = sent.resp.status().as_u16();
        // Exactly 200, not any-2xx: SSE lives in a 200 response body — a
        // 204/206 minted by an intermediary carries no event stream, and
        // treating it as success would end as a silent empty-stream Ok,
        // masking the fault from the watcher's Down/backoff signal.
        if status != 200 {
            return Err(ShedError::BadStatus(status));
        }
        // Keep the retry's throwaway transport alive for the life of the stream
        // (see [`Sent`]); `bytes_stream` consumes the response.
        let _transport = sent.transport;
        let mut stream = sent.resp.bytes_stream();
        let mut parser = SseParser::new().with_max_event_bytes(max_event_bytes);
        loop {
            // The idle timer wraps the BYTE-chunk future (see rc_events docs):
            // heartbeat comment bytes reset it even though they emit no event.
            match tokio::time::timeout(idle, stream.next()).await {
                Err(_) => {
                    return Err(ShedError::Transport("rc-events stream idle timeout".into()));
                }
                Ok(None) => break, // clean EOF
                Ok(Some(chunk)) => {
                    let chunk =
                        chunk.map_err(|e| ShedError::Transport(crate::authfail::flatten(&e)))?;
                    let events = parser
                        .try_feed(&chunk)
                        .map_err(|e| ShedError::Transport(format!("rc-events stream: {e}")))?;
                    for ev in &events {
                        if let Some(rc) = parse_rc_event(ev) {
                            sink.on_event(rc);
                        }
                    }
                }
            }
        }
        // Flush a final record that lacked its trailing blank line.
        for ev in parser.finish() {
            if let Some(rc) = parse_rc_event(&ev) {
                sink.on_event(rc);
            }
        }
        Ok(())
    }

    /// `POST /api/sheds/{name}/start`.
    pub async fn start(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::POST, &["api", "sheds", name, "start"])
            .await
    }

    /// `POST /api/sheds/{name}/stop`.
    pub async fn stop(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::POST, &["api", "sheds", name, "stop"])
            .await
    }

    /// `POST /api/sheds/{name}/reset`.
    pub async fn reset(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::POST, &["api", "sheds", name, "reset"])
            .await
    }

    /// `DELETE /api/sheds/{name}`.
    pub async fn delete(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::DELETE, &["api", "sheds", name])
            .await
    }

    /// `POST /api/sheds` with `Accept: text/event-stream`: streams progress then
    /// a final shed, delivered via `sink`. A transport/parse/error-event failure,
    /// or a stream that ends without a `complete`, is delivered as
    /// `sink.on_error`. Create mints its token inline once and does NOT 401-retry
    /// (one-shot stream), never downgrading to the static token — mirroring
    /// Swift's `createShed`.
    pub async fn create_shed(&self, req: &CreateShedRequest, sink: &dyn CreateSink) {
        if let Err(e) = self.create_stream(req, sink).await {
            sink.on_error(e.to_string());
        }
    }

    async fn create_stream(
        &self,
        req: &CreateShedRequest,
        sink: &dyn CreateSink,
    ) -> Result<(), ShedError> {
        let url = self.build_url(&["api", "sheds"], &[])?;
        // Bound the WHOLE connect phase — credential resolution (a foreign
        // TokenMinter can hang), the request send, and the one re-auth attempt —
        // under CREATE_IDLE_TIMEOUT, mirroring rc_events, so no pre-stream await
        // sits outside the bound.
        let connect = self.send_resolved(CREATE_IDLE_TIMEOUT, |http| {
            http.post(url.clone())
                .header(reqwest::header::ACCEPT, "text/event-stream")
                .json(req)
        });
        let sent = match tokio::time::timeout(CREATE_IDLE_TIMEOUT, connect).await {
            Err(_) => {
                return Err(ShedError::Create("create stream idle timeout".into()));
            }
            Ok(r) => r?,
        };
        let status = sent.resp.status().as_u16();
        if !(200..300).contains(&status) {
            return Err(ShedError::BadStatus(status));
        }
        // Keep the retry's throwaway transport alive for the life of the stream
        // (see [`Sent`]); `bytes_stream` consumes the response.
        let _transport = sent.transport;
        let mut stream = sent.resp.bytes_stream();
        let mut parser = SseParser::new();
        let mut saw_complete = false;
        loop {
            match tokio::time::timeout(CREATE_IDLE_TIMEOUT, stream.next()).await {
                Err(_) => return Err(ShedError::Create("create stream idle timeout".into())),
                Ok(None) => break,
                Ok(Some(chunk)) => {
                    let chunk =
                        chunk.map_err(|e| ShedError::Transport(crate::authfail::flatten(&e)))?;
                    for ev in parser.feed(&chunk) {
                        self.handle_create_event(&ev, sink, &mut saw_complete)?;
                    }
                }
            }
        }
        for ev in parser.finish() {
            self.handle_create_event(&ev, sink, &mut saw_complete)?;
        }
        if !saw_complete {
            return Err(ShedError::Create(
                "stream ended before a complete event".into(),
            ));
        }
        Ok(())
    }

    fn handle_create_event(
        &self,
        ev: &crate::sse::SseEvent,
        sink: &dyn CreateSink,
        saw_complete: &mut bool,
    ) -> Result<(), ShedError> {
        match ev.event.as_str() {
            "progress" => {
                if let Some(msg) = decode_progress(&ev.data) {
                    sink.on_progress(msg);
                }
            }
            "complete" => {
                let mut shed: Shed =
                    serde_json::from_str(&ev.data).map_err(|e| ShedError::Decode(e.to_string()))?;
                shed.host = self.server_name.clone(); // stamp host (SSE-complete path)
                *saw_complete = true;
                sink.on_complete(shed);
            }
            "error" => return Err(ShedError::Create(decode_error(&ev.data))),
            _ => {}
        }
        Ok(())
    }
}

fn build_http_client(
    pin: Option<&str>,
    resolver: Arc<crate::tls::ClientCertResolver>,
) -> Result<reqwest::Client, ShedError> {
    let mut builder = reqwest::Client::builder()
        .user_agent(USER_AGENT)
        // Fail closed on a plaintext redirect, mirroring the Swift pinned session.
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            if attempt.url().scheme() == "https" {
                attempt.follow()
            } else {
                attempt.stop()
            }
        }));
    if let Some(pin) = pin {
        builder = builder.use_preconfigured_tls(crate::tls::pinned_client_config_with_client_auth(
            pin, resolver,
        )?);
    }
    builder
        .build()
        .map_err(|e| ShedError::Transport(crate::authfail::flatten(&e)))
}

/// A progress event's `{"message": ...}`, or the raw data as a fallback.
fn decode_progress(data: &str) -> Option<String> {
    #[derive(serde::Deserialize)]
    struct Progress {
        message: Option<String>,
    }
    if let Ok(p) = serde_json::from_str::<Progress>(data) {
        if let Some(m) = p.message {
            return Some(m);
        }
    }
    if data.is_empty() {
        None
    } else {
        Some(data.to_string())
    }
}

/// An error event's `message ?? code ?? raw` (mirrors Swift's decodeErrorMessage).
fn decode_error(data: &str) -> String {
    #[derive(serde::Deserialize)]
    struct ApiError {
        code: Option<String>,
        message: Option<String>,
    }
    if let Ok(e) = serde_json::from_str::<ApiError>(data) {
        return e.message.or(e.code).unwrap_or_else(|| data.to_string());
    }
    data.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::token::MintedToken;
    use httpmock::prelude::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn client(server: &MockServer) -> Client {
        Client::new(
            server.base_url(),
            "mini2".to_string(),
            String::new(),
            None,
            None,
        )
        .unwrap()
    }

    #[tokio::test]
    async fn info_decodes() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let info = client(&server).info().await.unwrap();
        assert_eq!(info.name, "mini2");
        assert_eq!(info.backend.as_deref(), Some("firecracker"));
    }

    #[tokio::test]
    async fn list_sheds_stamps_host() {
        let server = MockServer::start_async().await;
        let body = format!(
            r#"{{"sheds":[{}]}}"#,
            include_str!("../../fixtures/shed_real.json")
        );
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(200).body(body);
            })
            .await;
        let sheds = client(&server).list_sheds().await.unwrap();
        assert_eq!(sheds.len(), 1);
        assert_eq!(sheds[0].name, "hello-world");
        assert_eq!(sheds[0].host, "mini2"); // stamped by the client
    }

    #[tokio::test]
    async fn list_sheds_null_is_empty() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(200).body(r#"{"sheds":null}"#);
            })
            .await;
        assert!(client(&server).list_sheds().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn system_df_decodes() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/system/df");
                t.status(200)
                    .body(include_str!("../../fixtures/system_df.json"));
            })
            .await;
        let df = client(&server).system_df().await.unwrap();
        assert_eq!(df.images.len(), 1);
        assert_eq!(df.totals.all.logical_bytes, 1073743872);
    }

    #[tokio::test]
    async fn images_and_egress_decode() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/images");
                t.status(200).body(format!(
                    r#"{{"images":[{}]}}"#,
                    include_str!("../../fixtures/image_enriched.json")
                ));
            })
            .await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/egress/profiles");
                t.status(200)
                    .body(include_str!("../../fixtures/egress_profiles.json"));
            })
            .await;
        let c = client(&server);
        let imgs = c.list_images().await.unwrap();
        assert_eq!(imgs.len(), 1);
        assert_eq!(imgs[0].alias.as_deref(), Some("base"));
        let profiles = c.egress_profiles().await.unwrap();
        assert_eq!(profiles.len(), 2);
    }

    #[tokio::test]
    async fn bad_status_maps() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(404);
            })
            .await;
        let err = client(&server).info().await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(404)));
    }

    #[tokio::test]
    async fn malformed_maps_to_decode() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(200).body("not json");
            })
            .await;
        let err = client(&server).info().await.unwrap_err();
        assert!(matches!(err, ShedError::Decode(_)));
    }

    #[tokio::test]
    async fn lifecycle_start_posts() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds/hello/start");
                t.status(200);
            })
            .await;
        client(&server).start("hello").await.unwrap();
        m.assert_async().await;
    }

    #[tokio::test]
    async fn lifecycle_delete_ok_and_stop_bad_status() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(DELETE).path("/api/sheds/gone");
                t.status(200);
            })
            .await;
        client(&server).delete("gone").await.unwrap();
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds/x/stop");
                t.status(500);
            })
            .await;
        assert!(matches!(
            client(&server).stop("x").await,
            Err(ShedError::BadStatus(500))
        ));
    }

    // A minter returning tok-1, tok-2, ... on successive mints.
    struct SeqMinter {
        calls: AtomicUsize,
    }
    #[async_trait::async_trait]
    impl TokenMinter for SeqMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            Ok(MintedToken {
                token: format!("tok-{n}"),
                expires_at_unix: None,
            })
        }
    }
    struct FailMinter;
    #[async_trait::async_trait]
    impl TokenMinter for FailMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            Err(ShedError::Transport("mint down".into()))
        }
    }

    #[tokio::test]
    async fn provider_sends_bearer_token() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-1");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter),
        )
        .unwrap();
        assert_eq!(c.info().await.unwrap().name, "mini2");
    }

    #[tokio::test]
    async fn retries_once_on_401_with_reminted_token() {
        let server = MockServer::start_async().await;
        // Stale token -> 401.
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        // Re-minted token -> 200.
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-2");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter),
        )
        .unwrap();
        assert_eq!(c.info().await.unwrap().name, "mini2"); // succeeds after retry
    }

    // A minter that always returns the SAME token, however often it's called.
    struct SameMinter {
        calls: AtomicUsize,
    }
    #[async_trait::async_trait]
    impl TokenMinter for SameMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(MintedToken {
                token: "tok-same".into(),
                expires_at_unix: None,
            })
        }
    }

    #[tokio::test]
    async fn retry_skipped_when_remint_returns_the_same_rejected_token() {
        // Plan §3.4: after the 401 → invalidate_if_current, the forced re-mint
        // yields the SAME rejected token — a retry would be a guaranteed
        // second 401, so request() skips it: exactly ONE request hits the
        // server and the original 401 surfaces.
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-same");
                t.status(401);
            })
            .await;
        let minter = Arc::new(SameMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();
        let err = c.info().await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(401)), "got {err:?}");
        assert_eq!(
            m.hits_async().await,
            1,
            "the same-token retry must be skipped"
        );
        // Two mints: the initial resolve + the forced post-invalidate re-mint
        // that produced the identical token.
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn retry_never_proceeds_unauthenticated_when_the_remint_fails() {
        // C6 adversarial review #2a: after the 401 → invalidate, the forced
        // re-mint FAILS, so nothing presentable is left. The retry must be
        // skipped — a provider-backed request never drops its Authorization
        // header (an unauthenticated retry against a secure server is a
        // guaranteed second 401 and ships the request with no proof of
        // identity) — and the failure the user sees is the MINT error, not the
        // 401 that triggered it: "the replacement could not be minted" is the
        // actionable half, and Go says the same (`cmd/shed`'s `reauthenticated`
        // returns a failed re-mint unwrapped).
        struct FlipMinter {
            calls: AtomicUsize,
        }
        #[async_trait::async_trait]
        impl TokenMinter for FlipMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
                if n == 1 {
                    Ok(MintedToken {
                        token: "tok-1".into(),
                        expires_at_unix: None,
                    })
                } else {
                    Err(ShedError::Transport("mint down".into()))
                }
            }
        }
        let server = MockServer::start_async().await;
        // An unauthenticated retry would hit THIS mock and turn the result
        // into Ok — assert it is never touched.
        let unauth = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info").matches(|req| {
                    !req.headers.as_ref().is_some_and(|h| {
                        h.iter()
                            .any(|(k, _)| k.eq_ignore_ascii_case("authorization"))
                    })
                });
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let authed = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        let minter = Arc::new(FlipMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();
        let err = c.info().await.unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("mint down") && msg.contains("HTTP 401"),
            "the mint error leads, with the refusal as context: {err:?}"
        );
        assert_eq!(authed.hits_async().await, 1, "exactly one authed attempt");
        assert_eq!(unauth.hits_async().await, 0, "no unauthenticated retry");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2); // initial + failed re-mint
    }

    // ---- A failed mint is CARRIED, and explains a refusal ----
    //
    // Plan 002 C2(i): the resolution used to be `.ok()`-discarded, so a provider
    // that could not mint sent the request UNAUTHENTICATED and reported whatever
    // the server said back — a generic 401 or TLS alert. The one message worth
    // reading ("this server issues certificates; upgrade shed-host-agent", "the
    // SSH bootstrap is unreachable") was thrown away at the call site. Go has
    // said the actionable thing all along (`cmd/shed`'s `reauthenticated`
    // returns the failed re-mint unwrapped; `sdk`'s `setAuth` names what goes on
    // the wire); these pin the Rust half.
    //
    // The error is carried rather than raised because the SAME state is normal
    // on an `auth.mode: open` server — shed-app installs the shared minter on
    // every server, and there is no credential for an open one to issue — so the
    // unauthenticated request must still go out and succeed. The pair below is
    // the whole contract: open server → works; enforcing server → the refusal
    // reports the mint failure.

    /// A minter that always fails with a fixed, recognisable message, counting
    /// attempts. The message stands in for the real one an mtls server's
    /// too-old host-agent produces.
    struct CountingFailMinter {
        calls: AtomicUsize,
    }
    const UPGRADE_MSG: &str = "this server issues client certificates; upgrade shed-host-agent";
    impl CountingFailMinter {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
            })
        }
        fn calls(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
    }
    #[async_trait::async_trait]
    impl TokenMinter for CountingFailMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Err(ShedError::Transport(UPGRADE_MSG.into()))
        }
    }

    #[tokio::test]
    async fn an_open_server_still_answers_when_the_mint_fails() {
        // The regression guard for the carry-don't-raise rule: an open server
        // (whose minter necessarily fails — nothing to issue) must keep working
        // exactly as before. The mock ONLY matches a request with no
        // Authorization header, so a token sneaking out would 404 instead.
        let server = MockServer::start_async().await;
        let unauth = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info").matches(|req| {
                    !req.headers.as_ref().is_some_and(|h| {
                        h.iter()
                            .any(|(k, _)| k.eq_ignore_ascii_case("authorization"))
                    })
                });
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = CountingFailMinter::new();
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();

        assert_eq!(c.info().await.unwrap().name, "mini2");
        assert_eq!(unauth.hits_async().await, 1, "the request went out");
        assert_eq!(minter.calls(), 1, "one mint attempt, its failure ignored");
    }

    #[tokio::test]
    async fn a_refusal_reports_the_mint_error_instead_of_the_bare_401() {
        // Same client state, enforcing server: the request goes out with nothing
        // to present, is refused, and the error names WHY nothing could be
        // presented — with the refusal itself as context. This is the message
        // the `.ok()` swallow used to lose.
        let server = MockServer::start_async().await;
        let refused = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(401);
            })
            .await;
        let minter = CountingFailMinter::new();
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();

        let err = c.info().await.unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains(UPGRADE_MSG) && msg.contains("HTTP 401"),
            "the mint error must lead, the 401 as context: {err:?}"
        );
        assert_eq!(refused.hits_async().await, 1, "one attempt, no retry");
        // Exactly ONE mint: the refusal of a request that had nothing to present
        // is the mint failure observed a round trip later — re-minting it here
        // would both cost a round trip and (inside the provider's failure
        // cooldown) replace the actionable text with the redacted marker.
        assert_eq!(minter.calls(), 1, "one mint attempt per request");
    }

    #[tokio::test]
    async fn a_surviving_credential_is_presented_when_the_mint_fails() {
        // The other half of the contract: the mint failed but the provider still
        // HOLDS something usable (here a seeded token the clock has moved past,
        // which is why the resolution minted at all). The survivor goes on the
        // wire — the server may well still accept it — and the mint error must
        // not fail a request that had a credential to send.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer surviving-tok");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = CountingFailMinter::new();
        let provider = Arc::new(
            ControlTokenProvider::new("mini2".into(), minter.clone())
                .with_seed(MintedToken {
                    token: "surviving-tok".into(),
                    expires_at_unix: Some(1_000),
                })
                .with_now(|| 5_000),
        );
        let c = Client::with_provider(server.base_url(), "mini2".into(), None, provider.clone())
            .unwrap();

        assert_eq!(c.info().await.unwrap().name, "mini2");
        assert_eq!(minter.calls(), 1, "the mint was attempted and failed");
        assert_eq!(
            provider.surviving_credential().await.map(|c| c.token),
            Some("surviving-tok".to_string()),
            "the failed mint leaves the held credential in place"
        );
    }

    #[tokio::test]
    async fn the_streaming_paths_report_the_mint_error_when_refused() {
        // create (SSE) and rc-events resolve through the same helper, so both
        // halves of the contract reach them: the stream is attempted, and the
        // refusal reports the mint failure rather than a bare 401.
        let server = MockServer::start_async().await;
        let create = server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(401);
            })
            .await;
        let events = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(401);
            })
            .await;
        let minter = CountingFailMinter::new();
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();

        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "folio".into(),
            ..Default::default()
        };
        c.create_shed(&req, sink.as_ref()).await;
        let snap = sink.snapshot();
        assert!(
            snap.error
                .as_deref()
                .is_some_and(|m| m.contains(UPGRADE_MSG) && m.contains("HTTP 401")),
            "create must report the mint error, got {:?}",
            snap.error
        );
        assert!(snap.shed.is_none());

        let rc_sink = RecordingRcSink::default();
        let err = c.rc_events(&rc_sink).await.unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains(UPGRADE_MSG) && msg.contains("HTTP 401"),
            "rc-events must report the mint error, got {err:?}"
        );
        assert!(rc_sink.events().is_empty());

        assert_eq!(create.hits_async().await, 1, "one attempt, no retry");
        assert_eq!(events.hits_async().await, 1, "one attempt, no retry");
        // One mint attempt per stream call — never a failed mint immediately
        // re-attempted by the retry machinery.
        assert_eq!(minter.calls(), 2);
    }

    #[tokio::test]
    async fn an_open_server_streams_when_the_mint_fails() {
        // The open-server half for the streaming paths: the create stream still
        // opens and completes with no credential at all.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds").matches(|req| {
                    !req.headers.as_ref().is_some_and(|h| {
                        h.iter()
                            .any(|(k, _)| k.eq_ignore_ascii_case("authorization"))
                    })
                });
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body("event: complete\ndata: {\"name\":\"folio\",\"status\":\"running\"}\n\n");
            })
            .await;
        let minter = CountingFailMinter::new();
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();

        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "folio".into(),
            ..Default::default()
        };
        c.create_shed(&req, sink.as_ref()).await;
        let snap = sink.snapshot();
        assert_eq!(snap.error, None, "the open server's stream must succeed");
        assert_eq!(snap.shed.map(|s| s.name).as_deref(), Some("folio"));
        assert_eq!(minter.calls(), 1);
    }

    #[tokio::test]
    async fn rejected_token_is_not_served_after_a_same_token_retry_skip() {
        // C6 adversarial review #2c: the successful same-token re-mint
        // re-caches the rejected token and clears the must-mint flag; the
        // equality-skip path must invalidate it AGAIN so a SUBSEQUENT call
        // force-mints instead of serving the rejected token from cache.
        struct ScriptMinter {
            calls: AtomicUsize,
            script: Vec<&'static str>,
        }
        #[async_trait::async_trait]
        impl TokenMinter for ScriptMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                let n = self.calls.fetch_add(1, Ordering::SeqCst);
                Ok(MintedToken {
                    token: self.script[n.min(self.script.len() - 1)].into(),
                    expires_at_unix: None,
                })
            }
        }
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-same");
                t.status(401);
            })
            .await;
        let minter = Arc::new(ScriptMinter {
            calls: AtomicUsize::new(0),
            script: vec!["tok-same", "tok-same", "tok-fresh"],
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();
        let err = c.info().await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(401)), "got {err:?}");
        assert_eq!(m.hits_async().await, 1, "same-token retry skipped");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2);
        // The next bearer resolution must FORCE a mint (call 3 → tok-fresh),
        // never serve the rejected tok-same from the re-populated cache.
        assert_eq!(c.bearer().await.as_deref(), Some("tok-fresh"));
        assert_eq!(minter.calls.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn invalidate_used_is_bounded_when_the_provider_mutex_is_wedged() {
        // C6 adversarial review #5b: the invalidate contends on the provider
        // mutex, which is held ACROSS a mint by design — a wedged concurrent
        // mint must not pin the auth-failure path forever. Wedge the mutex with
        // a never-resolving mint, then assert invalidate_used returns false
        // within its bound instead of hanging.
        struct WedgeMinter {
            entered: std::sync::atomic::AtomicBool,
        }
        #[async_trait::async_trait]
        impl TokenMinter for WedgeMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                self.entered.store(true, Ordering::SeqCst);
                std::future::pending().await
            }
        }
        let minter = Arc::new(WedgeMinter {
            entered: std::sync::atomic::AtomicBool::new(false),
        });
        let provider = Arc::new(ControlTokenProvider::new("mini2".into(), minter.clone()));
        let c = Client::with_provider(
            "http://127.0.0.1:9".into(),
            "mini2".into(),
            None,
            provider.clone(),
        )
        .unwrap();
        // Wedge: this task acquires the provider lock and pends inside the
        // mint forever (dropped with the test runtime).
        let wedger = provider.clone();
        tokio::spawn(async move {
            let _ = wedger.token().await;
        });
        // Real-time poll (repo test convention) until the mint holds the lock.
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !minter.entered.load(Ordering::SeqCst) {
            assert!(std::time::Instant::now() < deadline, "wedge never started");
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        let rejected = crate::token::Credential {
            mode: Some(crate::token::AuthMode::Token),
            token: "tok-x".into(),
            ..Default::default()
        };
        let invalidated = tokio::time::timeout(
            Duration::from_secs(5),
            c.invalidate_used(Some(&rejected), Duration::from_millis(100)),
        )
        .await
        .expect("invalidate_used must return within its bound, not hang");
        assert!(
            !invalidated,
            "a wedged provider must report a timed-out invalidate"
        );
    }

    #[tokio::test]
    async fn with_provider_client_sends_the_tuned_providers_token() {
        // Client::with_provider (plan §3.4 Client plumbing): a caller-tuned
        // provider (here: seeded) backs the client — the seed is sent as the
        // bearer with no mint.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer seeded-tok");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let provider = Arc::new(
            ControlTokenProvider::new("mini2".into(), minter.clone()).with_seed(MintedToken {
                token: "seeded-tok".into(),
                expires_at_unix: None,
            }),
        );
        let c = Client::with_provider(server.base_url(), "mini2".into(), None, provider).unwrap();
        assert_eq!(c.info().await.unwrap().name, "mini2");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 0, "seed used, no mint");
    }

    #[test]
    fn with_provider_pin_on_non_https_is_config_error() {
        let provider = Arc::new(ControlTokenProvider::new(
            "s".into(),
            Arc::new(SeqMinter {
                calls: AtomicUsize::new(0),
            }),
        ));
        let result = Client::with_provider(
            "http://x".into(),
            "s".into(),
            Some("sha256:aa".into()),
            provider,
        );
        assert!(matches!(result, Err(ShedError::Config(_))));
    }

    #[tokio::test]
    async fn static_token_used_without_provider() {
        let c = Client::new(
            "http://x".into(),
            "s".into(),
            "static-tok".into(),
            None,
            None,
        )
        .unwrap();
        assert_eq!(c.bearer().await, Some("static-tok".to_string()));
    }

    #[tokio::test]
    async fn mint_failure_is_fail_closed_no_downgrade() {
        // Provider fails + a static token is set → NO token (never the static).
        let c = Client::new(
            "http://x".into(),
            "s".into(),
            "static-tok".into(),
            None,
            Some(Arc::new(FailMinter)),
        )
        .unwrap();
        assert_eq!(c.bearer().await, None);
    }

    // ---- plan 006 D5/D6 — mint-error plumbing + the typed upgrade case ----

    #[test]
    fn refused_because_leads_with_the_mint_error() {
        // Banners truncate from the END; refusal-first ordering was exactly how
        // the actionable clause got cut off (shed#300).
        let outcome: Result<reqwest::Response, ShedError> = Err(ShedError::Transport(
            "received fatal alert: CertificateRequired".into(),
        ));
        let e = refused_because(
            &outcome,
            ShedError::Config("upgrade shed-host-agent".into()),
        );
        let ShedError::Config(m) = e else {
            panic!("variant must be preserved");
        };
        assert!(m.starts_with("upgrade shed-host-agent"), "{m}");
        assert!(
            m.contains("(request refused: transport error: received fatal alert"),
            "{m}"
        );
    }

    #[test]
    fn refused_because_preserves_the_typed_upgrade_case() {
        let outcome: Result<reqwest::Response, ShedError> =
            Err(ShedError::Transport("tls alert".into()));
        let e = refused_because(
            &outcome,
            ShedError::AgentUpgradeRequired {
                server: "mini2".into(),
                detail: "does not support `credential.get`".into(),
            },
        );
        let ShedError::AgentUpgradeRequired { server, detail } = e else {
            panic!("the typed case must survive the refusal wrap, got {e:?}");
        };
        assert_eq!(server, "mini2");
        assert!(detail.starts_with("does not support"), "{detail}");
        assert!(detail.contains("request refused"), "{detail}");
    }

    #[test]
    fn mint_context_keeps_the_wire_errors_identity() {
        let wire = ShedError::Transport("connection refused".into());
        let mint = ShedError::Config("upgrade shed-host-agent".into());
        let ShedError::Transport(m) = with_mint_context(&wire, &mint) else {
            panic!("the wire error's variant must survive");
        };
        assert!(m.starts_with("connection refused"), "{m}");
        assert!(
            m.contains("additionally, credential setup failed: upgrade"),
            "{m}"
        );
    }

    #[tokio::test]
    async fn unrelated_status_is_never_masked_by_a_mint_error() {
        // An open-mode 404 (or any non-auth status) must surface AS the status:
        // the carried mint error explains nothing about it (plan 006 D5's
        // narrowing — the blanket "mint error wins when nothing was presented"
        // would have replaced this 404 with a host-agent message).
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(404);
            })
            .await;
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(Arc::new(FailMinter)),
        )
        .unwrap();
        let e = c.info().await.unwrap_err();
        assert!(matches!(e, ShedError::BadStatus(404)), "got {e:?}");
    }

    #[tokio::test]
    async fn transport_failure_with_a_failed_mint_carries_both() {
        // The shed#297 secondary hole: a non-auth-shaped TRANSPORT failure used
        // to silently drop the carried mint error at the `!auth_shaped` early
        // return. Now the wire error keeps its identity and the mint failure
        // rides along as context.
        let c = Client::new(
            // Port 1 is never listening: a fast ECONNREFUSED.
            "http://127.0.0.1:1".into(),
            "mini2".into(),
            String::new(),
            None,
            Some(Arc::new(FailMinter)),
        )
        .unwrap();
        let e = c.info().await.unwrap_err();
        let ShedError::Transport(m) = e else {
            panic!("expected the wire transport identity, got a different variant");
        };
        assert!(m.contains("additionally, credential setup failed"), "{m}");
        assert!(m.contains("mint down"), "{m}");
    }

    #[test]
    fn pin_on_non_https_is_config_error() {
        let result = Client::new(
            "http://x".into(),
            "s".into(),
            String::new(),
            Some("sha256:aa".into()),
            None,
        );
        assert!(matches!(result, Err(ShedError::Config(_))));
    }

    #[tokio::test]
    async fn redirect_to_non_https_is_not_followed() {
        // The https-only redirect policy must NOT follow a redirect to a
        // non-https URL (a plaintext downgrade) — it stops, surfacing the 3xx
        // rather than dialing the target. Exercised on Linux since the GTK
        // e2e's plain-HTTP mock never trips the pin/redirect paths.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(302)
                    .header("location", "http://example.invalid/api/info");
            })
            .await;
        // BadStatus(302), not a transport error from dialing example.invalid.
        match client(&server).info().await {
            Err(ShedError::BadStatus(302)) => {}
            other => panic!("expected the redirect to be stopped (BadStatus 302), got {other:?}"),
        }
    }

    #[derive(Default, Clone)]
    struct RecordState {
        messages: Vec<String>,
        shed: Option<Shed>,
        error: Option<String>,
    }
    #[derive(Default)]
    struct RecordingSink {
        state: std::sync::Mutex<RecordState>,
    }
    impl RecordingSink {
        fn snapshot(&self) -> RecordState {
            self.state.lock().unwrap().clone()
        }
    }
    impl CreateSink for RecordingSink {
        fn on_progress(&self, message: String) {
            self.state.lock().unwrap().messages.push(message);
        }
        fn on_complete(&self, shed: Shed) {
            self.state.lock().unwrap().shed = Some(shed);
        }
        fn on_error(&self, message: String) {
            self.state.lock().unwrap().error = Some(message);
        }
    }

    #[tokio::test]
    async fn create_streams_progress_then_complete() {
        let server = MockServer::start_async().await;
        let sse = "event: progress\ndata: {\"message\":\"building\"}\n\n\
                   event: complete\ndata: {\"name\":\"folio\",\"status\":\"running\"}\n\n";
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(sse);
            })
            .await;
        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "folio".into(),
            repo: Some("charliek/folio".into()),
            ..Default::default()
        };
        client(&server).create_shed(&req, sink.as_ref()).await;
        let s = sink.snapshot();
        assert_eq!(s.messages, vec!["building"]);
        let shed = s.shed.expect("a complete shed");
        assert_eq!(shed.name, "folio");
        assert_eq!(shed.host, "mini2"); // stamped on the SSE-complete path
        assert!(s.error.is_none());
    }

    #[tokio::test]
    async fn create_error_event_reports_error() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .body("event: error\ndata: {\"message\":\"disk full\"}\n\n");
            })
            .await;
        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "x".into(),
            ..Default::default()
        };
        client(&server).create_shed(&req, sink.as_ref()).await;
        assert_eq!(
            sink.snapshot().error.as_deref(),
            Some("create failed: disk full")
        );
    }

    #[tokio::test]
    async fn create_end_without_complete_reports_error() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .body("event: progress\ndata: {\"message\":\"x\"}\n\n");
            })
            .await;
        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "x".into(),
            ..Default::default()
        };
        client(&server).create_shed(&req, sink.as_ref()).await;
        assert_eq!(
            sink.snapshot().error.as_deref(),
            Some("create failed: stream ended before a complete event")
        );
    }

    #[tokio::test]
    async fn create_401_remints_and_retries_once() {
        // Create runs the same classify → invalidate → one re-mint → retry path
        // as every other request (it used to be one-shot, invalidating only a
        // bearer token — `None` in mtls state, so a refused certificate left
        // create permanently failing). The JSON body is re-sent on the retry.
        let server = MockServer::start_async().await;
        let stale = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        let fresh = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds")
                    .header("authorization", "Bearer tok-2")
                    .json_body(serde_json::json!({"name": "x"}));
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body("event: progress\ndata: {\"message\":\"building\"}\n\nevent: complete\ndata: {\"name\":\"x\",\"status\":\"running\"}\n\n");
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();
        // Prime the provider cache with tok-1.
        let _ = c.bearer().await;
        assert_eq!(minter.calls.load(Ordering::SeqCst), 1);

        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "x".into(),
            ..Default::default()
        };
        c.create_shed(&req, sink.as_ref()).await;
        let snap = sink.snapshot();
        assert_eq!(snap.error, None, "create should have recovered");
        assert_eq!(snap.messages, vec!["building"]);
        assert_eq!(snap.shed.expect("a complete shed").name, "x");
        assert_eq!(stale.hits_async().await, 1, "exactly one rejected attempt");
        assert_eq!(fresh.hits_async().await, 1, "exactly one retry");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2, "one re-mint");
    }

    // ---- overview (fetchOverview cases ported from mobile's
    // overview_test.dart:119-134) ----

    #[tokio::test]
    async fn overview_decodes_golden_200() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/overview");
                t.status(200)
                    .header("content-type", "application/json")
                    .body(include_str!("../../fixtures/overview.json"));
            })
            .await;
        let o = client(&server).overview().await.unwrap();
        assert_eq!(o.sheds.len(), 2);
        assert_eq!(o.server.version, "0.8.0");
    }

    #[tokio::test]
    async fn overview_404_old_server_is_bad_status_never_decode() {
        // AC#9: an old server has no /api/overview route — chi's default
        // NotFound handler writes a text/plain "404 page not found" body, but
        // the server's ContentTypeJSON middleware has ALREADY stamped
        // Content-Type: application/json on the response. The client must
        // surface BadStatus(404) (the provider maps it to "unsupported"), and
        // must never try to decode the mislabeled non-JSON body.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/overview");
                t.status(404)
                    .header("content-type", "application/json")
                    .body("404 page not found");
            })
            .await;
        let err = client(&server).overview().await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(404)), "got {err:?}");
    }

    // ---- sessions read-plane ----

    #[tokio::test]
    async fn list_sessions_decodes_rc_enriched_and_plain_rows() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds/proj/sessions");
                t.status(200).body(
                    r#"{"sessions":[
                        {"name":"default","shed_name":"proj",
                         "created_at":"2026-06-19T18:52:00Z","attached":true},
                        {"name":"rc-abc234","shed_name":"proj",
                         "created_at":"2026-06-19T18:53:00Z","attached":false,
                         "rc":{"kind":"claude-rc","state":"ready","managed":true,
                               "activity":"working",
                               "last_message":"Running the test suite now."}}
                    ],"warnings":null}"#,
                );
            })
            .await;
        let r = client(&server).list_sessions("proj").await.unwrap();
        assert_eq!(r.sessions.len(), 2);
        assert!(r.warnings.is_empty()); // null → []
        assert!(r.sessions[0].rc.is_none()); // plain tmux row
        let rc = r.sessions[1].rc.as_ref().unwrap();
        assert_eq!(rc.kind.as_deref(), Some("claude-rc"));
        assert!(rc.managed);
        assert_eq!(rc.activity.as_deref(), Some("working"));
    }

    #[tokio::test]
    async fn list_sessions_warnings_present_and_error_map() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds/degraded/sessions");
                t.status(200).body(
                    r#"{"sessions":[{"name":"rc-x","shed_name":"degraded",
                                     "created_at":"2026-01-01T00:00:00Z","attached":false}],
                        "warnings":["degraded: rc enrichment degraded"]}"#,
                );
            })
            .await;
        let r = client(&server).list_sessions("degraded").await.unwrap();
        assert_eq!(r.warnings, ["degraded: rc enrichment degraded"]);
        assert!(r.sessions[0].rc.is_none()); // enrichment degraded → no rc block
                                             // Unknown shed → status-only 404 (mapSessionError).
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds/missing/sessions");
                t.status(404);
            })
            .await;
        assert!(matches!(
            client(&server).list_sessions("missing").await,
            Err(ShedError::BadStatus(404))
        ));
    }

    #[tokio::test]
    async fn delete_session_204_ok_and_404_maps() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(DELETE).path("/api/sheds/proj/sessions/rc-abc234");
                t.status(204); // the server's success shape (handlers.go:631)
            })
            .await;
        client(&server)
            .delete_session("proj", "rc-abc234")
            .await
            .unwrap();
        m.assert_async().await;
        server
            .mock_async(|w, t| {
                w.method(DELETE).path("/api/sheds/proj/sessions/nope");
                t.status(404);
            })
            .await;
        assert!(matches!(
            client(&server).delete_session("proj", "nope").await,
            Err(ShedError::BadStatus(404))
        ));
    }

    // ---- rc proxy: messages + input ----

    #[tokio::test]
    async fn rc_messages_happy_path_with_query_params() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/sheds/proj/rc/v1/sessions/abc234/messages")
                    .query_param("since", "7")
                    .query_param("limit", "50");
                t.status(200).body(
                    r#"{"messages":[
                        {"seq":8,"ts":"2026-06-19T18:53:00Z","role":"user","type":"text","text":"hi"},
                        {"seq":9,"role":"tool","type":"tool_use",
                         "tool":{"name":"shell","detail":"ls -la"}}
                    ],"truncated":false}"#,
                );
            })
            .await;
        let p = client(&server)
            .rc_messages("proj", "abc234", 7, Some(50))
            .await
            .unwrap();
        m.assert_async().await;
        assert_eq!(p.messages.len(), 2);
        assert!(!p.truncated);
        assert_eq!(p.messages[0].seq, 8);
        assert_eq!(
            p.messages[1].tool.as_ref().unwrap().name.as_deref(),
            Some("shell")
        );
    }

    #[tokio::test]
    async fn rc_messages_omits_limit_when_none() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/sheds/proj/rc/v1/sessions/abc234/messages")
                    .query_param("since", "0")
                    // The server defaults limit (100, cap 200) — the client
                    // must not send one.
                    .matches(|req| {
                        !req.query_params
                            .as_ref()
                            .is_some_and(|q| q.iter().any(|(k, _)| k == "limit"))
                    });
                t.status(200).body(r#"{"messages":[],"truncated":true}"#);
            })
            .await;
        let p = client(&server)
            .rc_messages("proj", "abc234", 0, None)
            .await
            .unwrap();
        m.assert_async().await;
        assert!(p.messages.is_empty());
        assert!(p.truncated);
    }

    #[tokio::test]
    async fn rc_messages_tolerates_missing_keys_and_maps_errors() {
        let server = MockServer::start_async().await;
        // A body with no messages key decodes to the empty page (tolerant).
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/sheds/proj/rc/v1/sessions/sparse/messages");
                t.status(200).body(r#"{"truncated":false}"#);
            })
            .await;
        let p = client(&server)
            .rc_messages("proj", "sparse", 0, None)
            .await
            .unwrap();
        assert!(p.messages.is_empty());
        assert!(!p.truncated);
        // Unknown slug → status-only 404 (hub `{code,message}` body ignored).
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/sheds/proj/rc/v1/sessions/nope/messages");
                t.status(404)
                    .body(r#"{"code":"unknown_slug","message":"no such rc session"}"#);
            })
            .await;
        assert!(matches!(
            client(&server).rc_messages("proj", "nope", 0, None).await,
            Err(ShedError::BadStatus(404))
        ));
    }

    #[tokio::test]
    async fn rc_input_posts_json_body_and_ignores_delivered_body() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds/proj/rc/v1/sessions/abc234/input")
                    .header("content-type", "application/json")
                    .json_body(serde_json::json!({"text": "looks good, continue"}));
                t.status(200).body(r#"{"delivered":true}"#);
            })
            .await;
        client(&server)
            .rc_input("proj", "abc234", "looks good, continue")
            .await
            .unwrap();
        m.assert_async().await;
    }

    #[tokio::test]
    async fn rc_input_status_code_errors_are_bad_status() {
        // Status-only dispatch (plan §3.2): the hub's flat {code,message}
        // bodies (hub.go:404-521) are NOT decoded; BadStatus carries the
        // status the caller keys off.
        let server = MockServer::start_async().await;
        for (slug, status, body) in [
            (
                "busy",
                409,
                r#"{"code":"not_accepting","message":"session is not waiting for input"}"#,
            ),
            (
                "big",
                413,
                r#"{"code":"too_large","message":"input body exceeds 16 KiB"}"#,
            ),
            (
                "gone",
                404,
                r#"{"code":"unknown_slug","message":"no such rc session"}"#,
            ),
        ] {
            server
                .mock_async(|w, t| {
                    w.method(POST)
                        .path(format!("/api/sheds/proj/rc/v1/sessions/{slug}/input"));
                    t.status(status).body(body);
                })
                .await;
            let err = client(&server)
                .rc_input("proj", slug, "x")
                .await
                .unwrap_err();
            match err {
                ShedError::BadStatus(s) => assert_eq!(s, status),
                other => panic!("expected BadStatus({status}), got {other:?}"),
            }
        }
    }

    #[tokio::test]
    async fn rc_input_retries_once_on_401_resending_body() {
        // Same 401 → invalidate + retry-once contract as every request()
        // path; the JSON body must be re-sent on the retried attempt.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds/proj/rc/v1/sessions/abc234/input")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        let ok = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds/proj/rc/v1/sessions/abc234/input")
                    .header("authorization", "Bearer tok-2")
                    .json_body(serde_json::json!({"text": "hi"}));
                t.status(200).body(r#"{"delivered":true}"#);
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter),
        )
        .unwrap();
        c.rc_input("proj", "abc234", "hi").await.unwrap();
        ok.assert_async().await;
    }

    // ---- URL path-segment safety (build_url defense in depth) ----

    #[tokio::test]
    async fn delete_session_traversal_is_encoded_as_one_segment() {
        // A malicious/corrupt session name must never rewrite the route: with
        // naive string interpolation, `../../victim` would dot-normalize into
        // DELETE /api/sheds/victim — a session-delete crossing into a
        // shed-delete. build_url percent-encodes the value as ONE segment.
        let server = MockServer::start_async().await;
        let victim = server
            .mock_async(|w, t| {
                w.method(DELETE).path("/api/sheds/victim");
                t.status(200);
            })
            .await;
        let encoded = server
            .mock_async(|w, t| {
                w.method(DELETE)
                    .path("/api/sheds/proj/sessions/..%2F..%2Fvictim");
                t.status(204);
            })
            .await;
        client(&server)
            .delete_session("proj", "../../victim")
            .await
            .unwrap();
        encoded.assert_async().await; // the encoded single segment was sent
        assert_eq!(victim.hits_async().await, 0, "traversal must not escape");
    }

    #[tokio::test]
    async fn rc_input_slug_with_slash_stays_one_segment() {
        // A '/' inside a (remote-influenced) slug is %2F, never a new segment.
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds/proj/rc/v1/sessions/a%2Fb/input")
                    .json_body(serde_json::json!({"text": "hi"}));
                t.status(200).body(r#"{"delivered":true}"#);
            })
            .await;
        client(&server).rc_input("proj", "a/b", "hi").await.unwrap();
        m.assert_async().await;
    }

    #[tokio::test]
    async fn bare_dot_and_empty_segments_are_rejected_client_side() {
        // ""/"."/".." can't be neutralized by encoding alone (a raw ".." that
        // reached the wire would be dot-normalized by the server's router into
        // a different route) — build_url refuses them outright.
        let c = Client::new("http://x".into(), "s".into(), String::new(), None, None).unwrap();
        assert!(matches!(c.delete("..").await, Err(ShedError::Config(_))));
        assert!(matches!(
            c.delete_session("proj", ".").await,
            Err(ShedError::Config(_))
        ));
        assert!(matches!(
            c.list_sessions("").await,
            Err(ShedError::Config(_))
        ));
        assert!(matches!(
            c.rc_messages("proj", "..", 0, None).await,
            Err(ShedError::Config(_))
        ));
    }

    // ---- rc-events SSE stream (plan §3.3 test matrix + AC#8) ----

    use crate::rc_events::RcEvent;

    #[derive(Default)]
    struct RecordingRcSink {
        events: std::sync::Mutex<Vec<RcEvent>>,
    }
    impl RecordingRcSink {
        fn events(&self) -> Vec<RcEvent> {
            self.events.lock().unwrap().clone()
        }
    }
    impl RcEventSink for RecordingRcSink {
        fn on_event(&self, ev: RcEvent) {
            self.events.lock().unwrap().push(ev);
        }
    }

    /// A minimal raw-TCP SSE server for the streaming-TIMING tests httpmock
    /// can't express (httpmock only serves complete pre-built bodies — it
    /// cannot trickle bytes with real gaps): accepts ONE connection, reads the
    /// request headers, writes an HTTP/1.1 200 SSE response head, then runs
    /// `script` against the raw socket on a plain OS thread (std sleeps
    /// between writes = real inter-chunk gaps). Dropping the socket at the end
    /// of `script` is the clean EOF (`connection: close`). Every script runs
    /// for a bounded time by construction; the caller MUST end its test with
    /// [`join_sse_server`] so no thread or listener outlives the test.
    fn spawn_sse_server(
        script: impl FnOnce(&mut std::net::TcpStream) + Send + 'static,
    ) -> (String, std::thread::JoinHandle<()>) {
        use std::io::{Read, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            stream.set_nodelay(true).unwrap(); // each write = one prompt chunk
            let mut req = Vec::new();
            let mut buf = [0u8; 4096];
            loop {
                let n = stream.read(&mut buf).unwrap_or(0);
                if n == 0 {
                    return;
                }
                req.extend_from_slice(&buf[..n]);
                if req.windows(4).any(|w| w == b"\r\n\r\n") {
                    break; // headers complete (GET — no body follows)
                }
            }
            stream
                .write_all(
                    b"HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\nconnection: close\r\n\r\n",
                )
                .unwrap();
            script(&mut stream);
        });
        (format!("http://{addr}"), handle)
    }

    /// Join the raw-TCP server thread with a bound (std's `JoinHandle` has no
    /// timed join, so the blocking join runs on the blocking pool under a
    /// tokio timeout): a wedged script fails the test instead of hanging the
    /// suite, and a panic inside the server thread is propagated, not lost.
    async fn join_sse_server(handle: std::thread::JoinHandle<()>) {
        tokio::time::timeout(
            Duration::from_secs(10),
            tokio::task::spawn_blocking(move || handle.join().expect("sse server thread panicked")),
        )
        .await
        .expect("sse server thread did not finish within the bound")
        .unwrap();
    }

    #[tokio::test]
    async fn rc_events_delivers_decoded_events_in_order_and_clean_eof_is_ok() {
        // Happy path: the server's `: ok` preamble (rcevents.go:185), two data
        // frames, then EOF. The comment preamble produces no sink call; the
        // events arrive decoded, in order; the ended stream is Ok(()).
        let server = MockServer::start_async().await;
        let sse = ": ok\n\n\
                   event: activity.changed\n\
                   data: {\"shed\":\"proj\",\"slug\":\"cdx777\",\"activity\":\"working\",\"state\":\"ready\"}\n\n\
                   event: session.updated\n\
                   data: {\"shed\":\"proj\",\"slug\":\"cdx777\",\"session\":{\"state\":\"ready\",\"activity\":\"idle\"}}\n\n";
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/rc/events")
                    .header("accept", "text/event-stream");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(sse);
            })
            .await;
        let sink = RecordingRcSink::default();
        client(&server).rc_events(&sink).await.unwrap();
        let evs = sink.events();
        assert_eq!(evs.len(), 2, "got {evs:?}");
        match &evs[0] {
            RcEvent::ActivityChanged {
                shed,
                slug,
                activity,
                ..
            } => {
                assert_eq!(shed, "proj");
                assert_eq!(slug, "cdx777");
                assert_eq!(*activity, Some(crate::rc::RcActivity::Working));
            }
            other => panic!("wrong first event: {other:?}"),
        }
        match &evs[1] {
            RcEvent::SessionUpdated { removed, .. } => assert!(!removed),
            other => panic!("wrong second event: {other:?}"),
        }
    }

    #[tokio::test]
    async fn rc_events_skips_malformed_and_unknown_frames_without_ending_stream() {
        // An unknown event name, a non-JSON payload, and a frame missing its
        // required keys are each skipped (parse_rc_event → None) — the stream
        // keeps going and a LATER valid event is still delivered, then Ok.
        let server = MockServer::start_async().await;
        let sse = "event: totally.unknown\ndata: {\"shed\":\"p\"}\n\n\
                   event: activity.changed\ndata: not-json\n\n\
                   event: activity.changed\ndata: {\"shed\":\"p\"}\n\n\
                   event: message.appended\ndata: {\"shed\":\"p\",\"slug\":\"s\",\"seq\":7}\n\n";
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(200).body(sse);
            })
            .await;
        let sink = RecordingRcSink::default();
        client(&server).rc_events(&sink).await.unwrap();
        assert_eq!(
            sink.events(),
            vec![RcEvent::MessageAppended {
                shed: "p".into(),
                slug: "s".into(),
                seq: 7,
            }]
        );
    }

    #[tokio::test]
    async fn rc_events_clean_eof_flushes_final_unterminated_record() {
        // A final record with no trailing blank line is flushed to the sink
        // via parser.finish() on EOF — delivered, not dropped.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(200)
                    .body("event: shed.stopped\ndata: {\"shed\":\"p\"}");
            })
            .await;
        let sink = RecordingRcSink::default();
        client(&server).rc_events(&sink).await.unwrap();
        assert_eq!(
            sink.events(),
            vec![RcEvent::ShedStopped { shed: "p".into() }]
        );
    }

    #[tokio::test]
    async fn rc_events_oversized_event_is_an_error() {
        // The capped parser (with_max_event_bytes + try_feed — the broker
        // bus's convention): an event exceeding the cap ends the stream as an
        // error (the watcher reconnects with a fresh parser), never buffers
        // on. Cap injected via the private seam so the test body stays small.
        let server = MockServer::start_async().await;
        let sse = format!("event: activity.changed\ndata: {}\n\n", "x".repeat(1024));
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(200).body(sse);
            })
            .await;
        let sink = RecordingRcSink::default();
        let err = client(&server)
            .rc_events_with_limits(&sink, Duration::from_secs(5), 64)
            .await
            .unwrap_err();
        match err {
            ShedError::Transport(msg) => {
                assert!(msg.contains("exceeded 64 bytes"), "got: {msg}");
            }
            other => panic!("expected Transport overflow, got {other:?}"),
        }
        assert!(sink.events().is_empty());
    }

    #[tokio::test]
    async fn rc_events_401_remints_and_retries_once() {
        // The rc-events connect runs the SAME classify → invalidate → one
        // re-mint → retry path as a unary request (it used to surface the 401
        // after invalidating only a bearer token — which is `None` in mtls
        // state, so a refused certificate could never recover there).
        let server = MockServer::start_async().await;
        let stale = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/rc/events")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        let fresh = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/rc/events")
                    .header("authorization", "Bearer tok-2");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(": ok\n\nevent: activity.changed\ndata: {\"shed\":\"proj\",\"slug\":\"cdx777\",\"activity\":\"working\",\"state\":\"ready\"}\n\n");
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();
        let sink = RecordingRcSink::default();
        c.rc_events(&sink).await.unwrap();
        assert_eq!(sink.events().len(), 1, "the retried stream delivered");
        assert_eq!(stale.hits_async().await, 1, "exactly one rejected attempt");
        assert_eq!(fresh.hits_async().await, 1, "exactly one retry");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2, "one re-mint");
    }

    #[tokio::test]
    async fn rc_events_401_that_survives_the_remint_is_surfaced_once() {
        // At-most-once: when the retry is refused too, rc_events surfaces the
        // status rather than looping. The watcher owns the reconnect.
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(401);
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter.clone()),
        )
        .unwrap();
        let sink = RecordingRcSink::default();
        let err = c.rc_events(&sink).await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(401)), "got {err:?}");
        assert_eq!(m.hits_async().await, 2, "one attempt + one retry, no more");
        assert!(sink.events().is_empty());
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn rc_events_connection_open_timeout() {
        // A server that accepts but never responds trips the connection-open
        // timeout (the same idle duration bounds the initial send).
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(200).body(": ok\n\n").delay(Duration::from_secs(2));
            })
            .await;
        let sink = RecordingRcSink::default();
        let err = client(&server)
            .rc_events_with_idle(&sink, Duration::from_millis(150))
            .await
            .unwrap_err();
        match err {
            ShedError::Transport(msg) => assert!(msg.contains("connect"), "got: {msg}"),
            other => panic!("expected Transport connect timeout, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn rc_events_idle_timeout_fires_on_a_silent_stream() {
        // Mid-stream silence: the server delivers one event then goes quiet
        // while HOLDING the socket open (a silently-dead connection — the case
        // the watchdog exists for). httpmock can't hold a stream open, so this
        // uses the raw-TCP helper. The pre-silence event was delivered; the
        // silence surfaces as the idle-timeout Transport error. Margin: the
        // 2s silence is ~7x the 300ms idle, so the timeout fires well inside
        // the quiet stretch even on a loaded worker.
        let (base, server) = spawn_sse_server(|s| {
            use std::io::Write;
            s.write_all(b": ok\n\nevent: hub.unavailable\ndata: {\"shed\":\"p\"}\n\n")
                .unwrap();
            std::thread::sleep(Duration::from_secs(2)); // silence >> idle
        });
        let c = Client::new(base, "mini2".into(), String::new(), None, None).unwrap();
        let sink = RecordingRcSink::default();
        let err = c
            .rc_events_with_idle(&sink, Duration::from_millis(300))
            .await
            .unwrap_err();
        match err {
            ShedError::Transport(msg) => assert!(msg.contains("idle timeout"), "got: {msg}"),
            other => panic!("expected Transport idle timeout, got {other:?}"),
        }
        assert_eq!(
            sink.events(),
            vec![RcEvent::HubUnavailable { shed: "p".into() }]
        );
        join_sse_server(server).await;
    }

    #[tokio::test]
    async fn rc_events_heartbeat_comments_reset_the_idle_timer() {
        // AC#8, the panel-critical pin: comment-only `: heartbeat` frames must
        // NOT trip the idle timer, even though the parser swallows them
        // without emitting an event — the timer wraps the BYTE-chunk future.
        // Real streaming gaps via the raw-TCP helper: 2.5s of comment-only
        // traffic (25 × 100ms) against a 1.5s idle. Margins (both directions,
        // CI-scheduling-safe): an event-level timer fires deterministically —
        // zero parsed events for 2.5s, a full 1s past the idle window — while
        // a false-fail of the byte-level timer needs the writer thread
        // descheduled >1.5s, 15× its 100ms cadence. The event after the quiet
        // stretch still arrives, then clean EOF → Ok.
        let (base, server) = spawn_sse_server(|s| {
            use std::io::Write;
            s.write_all(b": ok\n\n").unwrap();
            for _ in 0..25 {
                std::thread::sleep(Duration::from_millis(100));
                s.write_all(b": heartbeat\n\n").unwrap();
            }
            s.write_all(b"event: shed.stopped\ndata: {\"shed\":\"p\"}\n\n")
                .unwrap();
        });
        let c = Client::new(base, "mini2".into(), String::new(), None, None).unwrap();
        let sink = RecordingRcSink::default();
        c.rc_events_with_idle(&sink, Duration::from_millis(1500))
            .await
            .unwrap();
        assert_eq!(
            sink.events(),
            vec![RcEvent::ShedStopped { shed: "p".into() }]
        );
        join_sse_server(server).await;
    }

    #[tokio::test]
    async fn rc_events_connect_timeout_covers_a_hung_bearer_mint() {
        // The connect bound wraps the WHOLE connect phase, bearer resolution
        // included: a foreign TokenMinter that never resolves must surface as
        // the connect timeout, not hang rc_events forever. (The mint pends
        // before any dial, so the unroutable base URL is never touched.)
        struct NeverMinter;
        #[async_trait::async_trait]
        impl TokenMinter for NeverMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                std::future::pending().await
            }
        }
        let c = Client::new(
            "http://127.0.0.1:9".into(),
            "s".into(),
            String::new(),
            None,
            Some(Arc::new(NeverMinter)),
        )
        .unwrap();
        let sink = RecordingRcSink::default();
        let err = c
            .rc_events_with_idle(&sink, Duration::from_millis(100))
            .await
            .unwrap_err();
        match err {
            // Either message is a correct answer, and both are the SAME bound:
            // credential resolution is now bounded inside the connect phase
            // (`send_resolved`), so the inner timer usually wins the race with
            // the outer one. What this pins is that a never-resolving mint ends
            // the call instead of hanging it.
            ShedError::Transport(msg) => assert!(
                msg.contains("credential resolution timed out") || msg.contains("connect"),
                "got: {msg}"
            ),
            other => panic!("expected a Transport timeout, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn request_bearer_resolution_is_bounded_against_a_hung_mint() {
        // The bearer resolution in request() is bounded by the request's own
        // timeout: a foreign TokenMinter that never resolves must surface as an
        // error within the bound, not hang every JSON/lifecycle call. Mirrors
        // rc_events_connect_timeout_covers_a_hung_bearer_mint for the JSON path.
        struct NeverMinter;
        #[async_trait::async_trait]
        impl TokenMinter for NeverMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                std::future::pending().await
            }
        }
        let c = Client::new(
            "http://127.0.0.1:9".into(),
            "s".into(),
            String::new(),
            None,
            Some(Arc::new(NeverMinter)),
        )
        .unwrap();
        let url = c.build_url(&["api", "info"], &[]).unwrap();
        // Outer guard: if the fix regressed, request() would hang and this
        // 2s timeout would fire with the expect message instead of deadlocking.
        let err = tokio::time::timeout(
            Duration::from_secs(2),
            c.request(reqwest::Method::GET, &url, Duration::from_millis(100), None),
        )
        .await
        .expect("request must return within the bound, not hang")
        .unwrap_err();
        match err {
            // "credential" since the resolution generalized past bearer tokens
            // (plan 001 D5); the bound and the behavior are unchanged.
            ShedError::Transport(msg) => assert!(msg.contains("credential"), "got: {msg}"),
            other => panic!("expected Transport credential timeout, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn rc_events_non_200_success_status_is_bad_status() {
        // Exactly-200 contract: SSE lives in a 200 response body. A 204/206
        // minted by an intermediary carries no event stream — any-2xx
        // acceptance would end as a silent empty-stream Ok, masking the fault
        // from the watcher's Down/backoff signal.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/rc/events");
                t.status(204);
            })
            .await;
        let sink = RecordingRcSink::default();
        let err = client(&server).rc_events(&sink).await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(204)), "got {err:?}");
        assert!(sink.events().is_empty());
    }
}

/// The adaptive transport end to end (plan 001 D5), against real in-process TLS
/// listeners rather than a mock: a client that enrolls, rotates, and follows a
/// server-side mode flip — all on ONE `reqwest::Client` that is never rebuilt.
#[cfg(test)]
mod mtls_tests {
    use super::*;
    use crate::testtls::*;
    use crate::token::{
        AuthMode, ControlTokenProvider, CredentialObserver, CredentialRequest, MintedCredential,
        MintedToken, TokenMinter,
    };
    use base64::engine::general_purpose::STANDARD as BASE64;
    use base64::Engine as _;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Mutex;

    /// What the mock server hands back on the next mint — flipped mid-test to
    /// model an operator changing `auth.mode`.
    #[derive(Clone)]
    enum Issue {
        Certificate,
        /// A certificate signed by the right CA but already EXPIRED — the shape
        /// the server refuses inside the handshake, with a TLS alert rather than
        /// an HTTP status.
        ExpiredCertificate,
        Token(String),
    }

    /// A minter that behaves like the real `_bootstrap` exchange: it receives the
    /// CSR, signs it with the server's CA (or issues a bearer token instead),
    /// serializes the SAME JSON bundle shape the Go server emits, and decodes it
    /// back through [`crate::token::parse_credential_bundle`] — so these tests
    /// exercise the wire decoder, not a hand-built credential.
    struct CaMinter {
        ca: Arc<TestCa>,
        issue: Mutex<Issue>,
        calls: AtomicUsize,
        csrs: Mutex<Vec<String>>,
        delay_ms: u64,
        /// Issue a certificate for an UNRELATED key, modelling a server (or a
        /// man in the middle) returning a certificate this process cannot use.
        foreign_key: bool,
    }

    impl CaMinter {
        fn new(ca: Arc<TestCa>) -> Arc<Self> {
            Arc::new(Self {
                ca,
                issue: Mutex::new(Issue::Certificate),
                calls: AtomicUsize::new(0),
                csrs: Mutex::new(Vec::new()),
                delay_ms: 0,
                foreign_key: false,
            })
        }
        fn slow(ca: Arc<TestCa>) -> Arc<Self> {
            Arc::new(Self {
                delay_ms: 40,
                ..Self::bare(ca)
            })
        }
        fn foreign(ca: Arc<TestCa>) -> Arc<Self> {
            Arc::new(Self {
                foreign_key: true,
                ..Self::bare(ca)
            })
        }
        fn bare(ca: Arc<TestCa>) -> Self {
            Self {
                ca,
                issue: Mutex::new(Issue::Certificate),
                calls: AtomicUsize::new(0),
                csrs: Mutex::new(Vec::new()),
                delay_ms: 0,
                foreign_key: false,
            }
        }
        fn set_issue(&self, issue: Issue) {
            *self.issue.lock().unwrap() = issue;
        }
        fn calls(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
        fn csrs(&self) -> Vec<String> {
            self.csrs.lock().unwrap().clone()
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for CaMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            unreachable!("an mtls-capable minter is driven through mint_credential")
        }

        fn supports_mtls(&self) -> bool {
            true
        }

        async fn mint_credential(
            &self,
            _server: &str,
            req: &CredentialRequest,
        ) -> Result<MintedCredential, ShedError> {
            let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            if let Some(csr) = req.csr_base64() {
                self.csrs.lock().unwrap().push(csr.to_string());
            }
            if self.delay_ms > 0 {
                tokio::time::sleep(Duration::from_millis(self.delay_ms)).await;
            }
            let issue = self.issue.lock().unwrap().clone();
            let expired = matches!(issue, Issue::ExpiredCertificate);
            let bundle = match issue {
                Issue::Certificate | Issue::ExpiredCertificate => {
                    let csr = req
                        .csr_base64()
                        .ok_or_else(|| ShedError::Transport("no CSR offered".into()))?;
                    let der = BASE64.decode(csr.as_bytes()).unwrap();
                    let (pem, serial) = if self.foreign_key {
                        let other = self.ca.client_cert(
                            &format!("SHA256:client-{n}"),
                            "control",
                            valid_window(),
                        );
                        (other.cert_pem, "beef".to_string())
                    } else {
                        let window = if expired {
                            expired_window()
                        } else {
                            valid_window()
                        };
                        self.ca.sign_csr(
                            &der,
                            &format!("SHA256:client-{n}"),
                            "control",
                            n as u64,
                            window,
                        )
                    };
                    serde_json::json!({
                        "auth_mode": "mtls",
                        "https_port": 8443,
                        "tls_cert_fingerprint": "sha256:aa",
                        "client_cert": pem,
                        "scope": "control",
                        "cert_serial": serial,
                        "expires_at": "2036-01-01T00:00:00Z",
                    })
                }
                Issue::Token(t) => serde_json::json!({
                    "auth_mode": "token",
                    "http_port": 8080,
                    "https_port": 8443,
                    "tls_cert_fingerprint": "sha256:aa",
                    "token": t,
                    "scope": "control",
                    "token_id": "id-1",
                    "expires_at": "2036-01-01T00:00:00Z",
                }),
            };
            crate::token::parse_credential_bundle(&bundle.to_string(), None)
                .map_err(|e| ShedError::Transport(e.to_string()))
        }
    }

    /// A [`CaMinter`] that can be switched to FAIL — the "the host-agent can no
    /// longer mint" state (it was upgraded away, SSH is down) reached AFTER a
    /// certificate has already been enrolled and armed.
    struct FailAfterEnroll {
        inner: Arc<CaMinter>,
        fail: std::sync::atomic::AtomicBool,
        failed: AtomicUsize,
    }
    const MTLS_MINT_DOWN: &str = "the SSH bootstrap is unreachable";
    impl FailAfterEnroll {
        fn new(inner: Arc<CaMinter>) -> Arc<Self> {
            Arc::new(Self {
                inner,
                fail: std::sync::atomic::AtomicBool::new(false),
                failed: AtomicUsize::new(0),
            })
        }
        fn set_fail(&self) {
            self.fail.store(true, Ordering::SeqCst);
        }
        fn failed_calls(&self) -> usize {
            self.failed.load(Ordering::SeqCst)
        }
    }
    #[async_trait::async_trait]
    impl TokenMinter for FailAfterEnroll {
        async fn mint(&self, server: &str) -> Result<MintedToken, ShedError> {
            self.inner.mint(server).await
        }
        fn supports_mtls(&self) -> bool {
            true
        }
        async fn mint_credential(
            &self,
            server: &str,
            req: &CredentialRequest,
        ) -> Result<MintedCredential, ShedError> {
            if self.fail.load(Ordering::SeqCst) {
                self.failed.fetch_add(1, Ordering::SeqCst);
                return Err(ShedError::Transport(MTLS_MINT_DOWN.into()));
            }
            self.inner.mint_credential(server, req).await
        }
    }

    /// Records every `mode_changed` event, in order.
    #[derive(Default)]
    struct ModeLog(Mutex<Vec<AuthMode>>);
    impl ModeLog {
        /// Assert the delivered sequence, waiting for it (bounded) first.
        ///
        /// Observer callbacks are delivered off the provider's dispatcher thread
        /// (token.rs `CredentialEvents`), so they are asynchronous with respect to
        /// the request that triggered the mint — reading immediately after
        /// `get()` would race delivery. The wait is the consumer-side shape too:
        /// a UI learns the flip a beat after the request it rode on.
        async fn wait_for(&self, expected: &[AuthMode]) {
            for _ in 0..400 {
                if self.0.lock().unwrap().len() >= expected.len() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
            let got = self.0.lock().unwrap().clone();
            assert_eq!(got, expected, "mode_changed sequence");
        }
    }
    impl CredentialObserver for ModeLog {
        fn on_mode_changed(&self, _server: &str, mode: AuthMode) {
            self.0.lock().unwrap().push(mode);
        }
    }

    fn client_for(srv: &TlsServer, provider: Arc<ControlTokenProvider>) -> Client {
        Client::with_provider(
            srv.base_url(),
            "mtls-test".into(),
            Some(srv.pin.clone()),
            provider,
        )
        .unwrap()
    }

    async fn get(c: &Client) -> Result<Vec<u8>, ShedError> {
        let url = c.build_url(&["api", "info"], &[]).unwrap();
        c.request(reqwest::Method::GET, &url, GET_TIMEOUT, None)
            .await
    }

    /// [`get`] for tests that assert on the EXACT rejection error. Production
    /// re-sends the ambiguous "connection was not ready" shape once
    /// (`authfail.rs`, finding 6) and hands a second occurrence back to the
    /// caller; under parallel test load that can happen twice in a row, which
    /// would report a false regression in the alert being pinned.
    async fn get_stable(c: &Client) -> Result<Vec<u8>, ShedError> {
        let mut outcome = get(c).await;
        for _ in 0..3 {
            match &outcome {
                Err(ShedError::Transport(m)) if crate::authfail::is_connection_lost_message(m) => {
                    outcome = get(c).await;
                }
                _ => break,
            }
        }
        outcome
    }

    // (a) The whole enrollment path against a REAL RequireAndVerifyClientCert
    // listener: keypair + CSR generated here, certificate signed by the server's
    // CA, presented at the handshake, request authorized.
    #[tokio::test(flavor = "multi_thread")]
    async fn client_authenticates_with_an_enrolled_certificate() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, false).await;
        let minter = CaMinter::new(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());

        let body = get(&c).await.unwrap();
        assert_eq!(body, b"{\"ok\":true}");
        // The server saw OUR certificate — issued for the CSR this process sent.
        assert_eq!(srv.client_cns(), vec!["SHA256:client-1".to_string()]);
        // The CSR really was carried out (and is a decodable P-256 CSR).
        let csrs = minter.csrs();
        assert_eq!(csrs.len(), 1);
        assert_eq!(BASE64.decode(csrs[0].as_bytes()).unwrap()[0], 0x30);
        // The provider reports the adopted shape, and no bearer is ever sent.
        let cred = provider.credential().await.unwrap();
        assert_eq!(cred.mode, Some(AuthMode::Mtls));
        assert_eq!(cred.bearer_token(), None);
        assert!(provider.cert_resolver().current().is_some());
    }

    // (b) ROTATION: serial A → serial B across a forced reconnect, on the SAME
    // reqwest::Client instance (plan 001 D5's rotation acceptance test).
    #[tokio::test(flavor = "multi_thread")]
    async fn rotation_presents_the_new_certificate_without_rebuilding_the_transport() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        // close_after_each: the server drops the connection after each response,
        // so request #2 must re-handshake — the "forced reconnect" the AC names.
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, true).await;
        let minter = CaMinter::new(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());

        let transport_before = c.transport_id();
        get(&c).await.unwrap();
        let cert_a = provider.cert_resolver().current().unwrap();

        // Rotate: the credential is refused / near expiry → one re-mint.
        //
        // This is the test that caught TLS 1.3 session RESUMPTION silently
        // re-presenting the OLD identity — see the resumption note in
        // `tls::pinned_client_config_with_client_auth`. Without that fix the
        // server below sees `client-1` twice.
        provider.invalidate().await;
        get(&c).await.unwrap();
        let cert_b = provider.cert_resolver().current().unwrap();

        assert_eq!(
            c.transport_id(),
            transport_before,
            "rotation must not rebuild the reqwest::Client"
        );
        assert!(
            !Arc::ptr_eq(&cert_a, &cert_b),
            "the resolver must hold a NEW certified key"
        );
        assert_eq!(minter.calls(), 2);
        assert_eq!(minter.csrs().len(), 2, "each renewal generates a fresh CSR");
        assert_ne!(minter.csrs()[0], minter.csrs()[1], "fresh keypair per mint");
        // Two handshakes, serial A then serial B — the server saw the rotation.
        assert_eq!(srv.handshake_count(), 2);
        assert_eq!(
            srv.client_cns(),
            vec!["SHA256:client-1".to_string(), "SHA256:client-2".to_string()]
        );
    }

    // (c) MODE FLIP, both directions, on one transport: mtls → token → mtls, each
    // flip driven only by the server's answer and recovered by a single silent
    // re-mint (plan 001 D5's migration story).
    #[tokio::test(flavor = "multi_thread")]
    async fn a_server_mode_flip_never_rebuilds_the_client() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_flip_server(&ca, ServerAuthMode::Mtls, true).await;
        let minter = CaMinter::new(ca.clone());
        let log = Arc::new(ModeLog::default());
        let provider = Arc::new(
            ControlTokenProvider::new("s".into(), minter.clone())
                .with_observer(log.clone() as Arc<dyn CredentialObserver>),
        );
        let c = client_for(&srv, provider.clone());
        let resolver = provider.cert_resolver();

        // Phase 1 — mtls: the certificate authorizes the request.
        get(&c).await.unwrap();
        log.wait_for(&[AuthMode::Mtls]).await;
        assert!(provider.cert_resolver().current().is_some());

        // Phase 2 — the operator flips the server to token mode. The next request
        // still presents the (now meaningless) certificate, gets a 401, and the
        // client re-mints, adopts the token, and retries — all silently.
        srv.set_mode(ServerAuthMode::Token("tok-flip".into()));
        minter.set_issue(Issue::Token("tok-flip".into()));
        get(&c).await.unwrap();
        log.wait_for(&[AuthMode::Mtls, AuthMode::Token]).await;
        assert_eq!(
            provider.credential().await.unwrap().bearer_token(),
            Some("tok-flip")
        );
        assert!(
            provider.cert_resolver().current().is_none(),
            "the stale certificate must be withdrawn from the transport"
        );

        // Phase 3 — and back again.
        srv.set_mode(ServerAuthMode::Mtls);
        minter.set_issue(Issue::Certificate);
        get(&c).await.unwrap();
        log.wait_for(&[AuthMode::Mtls, AuthMode::Token, AuthMode::Mtls])
            .await;
        assert!(provider.cert_resolver().current().is_some());

        // The flip is invisible ABOVE this client, which is what D5 asks for: the
        // same `Client`, the same provider, the same resolver handle carried the
        // whole mtls → token → mtls journey, and nothing had to be reconstructed
        // by a caller. The connection POOL is a different matter — each flip has a
        // certificate on one side of it, and a pool cannot carry one (see
        // `Client::recycle_transport`), so it is purged rather than reused.
        assert!(
            Arc::ptr_eq(&provider.cert_resolver(), &resolver),
            "the flip must be a write through the SAME resolver, not a new one"
        );
        assert_eq!(minter.calls(), 3, "one mint per flip, no retry storms");
    }

    // (b2) The POOLED path, which is the one that actually bites: a keep-alive
    // server that revokes an identity mid-connection. The 401 arrives on a
    // connection whose handshake already happened, so re-minting alone does not
    // help — the retry has to leave that connection behind.
    //
    // What keeps this test honest is the IDENTITY SEQUENCE the server saw, and in
    // particular that the refused identity handshook exactly ONCE: one dial means
    // the rejected request really was served from the pool (had the pool already
    // dropped the connection, the refused certificate would have been presented on
    // a second, fresh dial and the pooled path would never have been exercised),
    // and it also means the retry did not re-present it.
    //
    // It deliberately does NOT assert a total dial count. The test server counts a
    // handshake — and records the CN — the moment `accept` returns, BEFORE it has
    // read a request, so the tally is a count of DIALS, not of authorized
    // requests. hyper's pool may establish a connection that the request it was
    // dialed for does not end up using (its checkout/connect race is resolved by
    // whichever finishes first, and the loser's connection can still complete its
    // handshake), which is platform- and timing-dependent: an exact-length
    // assertion passes on macOS and fails on Linux with a benign extra
    // `client-2` dial. A request-less extra dial costs a handshake and nothing
    // else — `minter.calls()` below pins the expensive part (a mint is an SSH
    // round trip and, on desktop, a Touch ID prompt) at exactly one.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_revoked_identity_recovers_on_a_pooled_keepalive_connection() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_flip_server(
            &ca,
            ServerAuthMode::MtlsAllow(vec!["SHA256:client-1".into()]),
            false, // keep-alive: the server never hands us a free reconnect
        )
        .await;
        let minter = CaMinter::new(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());
        let transport = c.transport_id();

        get(&c).await.unwrap();
        assert_eq!(srv.handshake_count(), 1);

        // The SSH key leaves the allowlist: the certificate this connection
        // presented is refused from now on, and only the NEXT one is accepted.
        srv.set_mode(ServerAuthMode::MtlsAllow(vec!["SHA256:client-2".into()]));

        get(&c)
            .await
            .expect("the retry must recover on a fresh connection");

        assert_eq!(minter.calls(), 2, "exactly one re-mint");

        let cns = srv.client_cns();
        assert!(
            cns.len() >= 2,
            "the retry must have been a NEW handshake, not a re-send on the \
             pooled connection: {cns:?}"
        );
        assert_eq!(
            cns.first().map(String::as_str),
            Some("SHA256:client-1"),
            "the refused identity is the first thing the server saw: {cns:?}"
        );
        assert_eq!(
            cns.iter().filter(|cn| *cn == "SHA256:client-1").count(),
            1,
            "the refused identity handshook exactly ONCE — the 401 arrived on the \
             POOLED connection (not on a fresh dial), and the retry never \
             re-presented it: {cns:?}"
        );
        assert!(
            cns[1..].iter().all(|cn| cn == "SHA256:client-2"),
            "every handshake after the re-mint carries the NEW identity: {cns:?}"
        );
        assert_ne!(
            c.transport_id(),
            transport,
            "the pool that held the refused connection must be gone, not reused"
        );

        // And the recovery is DURABLE — the property a throwaway retry client
        // would not have given: the next request rides the new pool with no
        // rejection, no mint and no extra dial. (Before the pool purge, the
        // refused connection stayed checked in and poisoned every later request,
        // costing a mint each time.) Stated as the identity invariant rather than
        // as a dial count, for the reason in the doc comment: a throwaway-retry
        // implementation fails BOTH of these — the later request rides the
        // poisoned connection, so the server sees `client-1` a second time and
        // the client pays another mint.
        get(&c).await.unwrap();
        assert_eq!(minter.calls(), 2, "no further mint");
        let cns = srv.client_cns();
        assert_eq!(
            cns.iter().filter(|cn| *cn == "SHA256:client-1").count(),
            1,
            "the refused identity is never presented again: {cns:?}"
        );
        assert!(
            cns[1..].iter().all(|cn| cn == "SHA256:client-2"),
            "later requests ride the recycled pool as the NEW identity: {cns:?}"
        );
    }

    // (d) Single-flight: concurrent resolutions of a missing credential mint once.
    #[tokio::test(flavor = "multi_thread")]
    async fn concurrent_enrollments_mint_exactly_once() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, false).await;
        let minter = CaMinter::slow(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());

        let (a, b, d) = tokio::join!(get(&c), get(&c), get(&c));
        a.unwrap();
        b.unwrap();
        d.unwrap();
        assert_eq!(minter.calls(), 1, "concurrent enrollments must coalesce");
        assert_eq!(minter.csrs().len(), 1, "only one keypair is generated");
    }

    // (f) "No usable credential ⇒ enroll" (Go `4553cc8` parity): a provider that
    // holds NOTHING mints BEFORE the first request — the request must not be sent
    // unauthenticated and rejected first.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_provider_holding_nothing_enrolls_before_the_first_request() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, false).await;
        let minter = CaMinter::new(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        assert!(provider.cert_resolver().current().is_none());

        let c = client_for(&srv, provider.clone());
        get(&c).await.unwrap();

        assert_eq!(minter.calls(), 1);
        assert_eq!(
            srv.handshake_count(),
            1,
            "exactly one handshake: no rejected unauthenticated attempt first"
        );
    }

    // ---- The SSE streams take the SAME re-auth path (FIX 2) ----
    //
    // Both streams used to bypass the classifier entirely: `rc_events` and
    // `create_stream` flattened a TLS error and returned, and their 401 handling
    // invalidated a BEARER token — which is `None` in mtls state. A revoked or
    // expired certificate therefore left every stream permanently failing, with
    // the provider still holding the refused credential. Each test below drives
    // one stream through one rejection and asserts it recovers EXACTLY once.

    #[derive(Default)]
    struct RcSink(Mutex<Vec<crate::rc_events::RcEvent>>);
    impl RcEventSink for RcSink {
        fn on_event(&self, ev: crate::rc_events::RcEvent) {
            self.0.lock().unwrap().push(ev);
        }
    }

    #[derive(Default)]
    struct CreateLog {
        progress: Mutex<Vec<String>>,
        shed: Mutex<Option<String>>,
        error: Mutex<Option<String>>,
    }
    impl CreateSink for CreateLog {
        fn on_progress(&self, message: String) {
            self.progress.lock().unwrap().push(message);
        }
        fn on_complete(&self, shed: crate::models::Shed) {
            *self.shed.lock().unwrap() = Some(shed.name);
        }
        fn on_error(&self, message: String) {
            *self.error.lock().unwrap() = Some(message);
        }
    }

    /// A listener that refuses the FIRST certificate at the TLS layer: the mint
    /// hands out an expired certificate, so the rejection arrives as a
    /// `CertificateExpired` alert with no HTTP response at all — the shape the
    /// streams used to flatten and give up on.
    async fn tls_alert_setup() -> (TlsServer, Arc<CaMinter>, Arc<ControlTokenProvider>) {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, false).await;
        let minter = CaMinter::new(ca.clone());
        minter.set_issue(Issue::ExpiredCertificate);
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        // Enroll NOW, while the mint is the expired one, so the credential the
        // stream goes out with is already the doomed one. From here the minter
        // issues valid certificates, so the re-mint can recover.
        provider.credential().await.unwrap();
        minter.set_issue(Issue::Certificate);
        (srv, minter, provider)
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn rc_events_recovers_from_a_tls_alert_rejection_exactly_once() {
        let (srv, minter, provider) = tls_alert_setup().await;
        let c = client_for(&srv, provider.clone());
        // The re-mint issues a VALID certificate, so the retry authenticates.
        let sink = RcSink::default();
        c.rc_events(&sink).await.expect("the stream must recover");
        assert_eq!(
            sink.0.lock().unwrap().len(),
            1,
            "the retried stream delivered"
        );
        assert_eq!(minter.calls(), 2, "exactly one re-mint");
        assert_eq!(
            provider.credential().await.unwrap().cert_serial,
            "2",
            "the refused certificate must not still be cached"
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn create_recovers_from_a_tls_alert_rejection_exactly_once() {
        let (srv, minter, provider) = tls_alert_setup().await;
        let c = client_for(&srv, provider.clone());
        let sink = CreateLog::default();
        let req = crate::models::CreateShedRequest {
            name: "folio".into(),
            ..Default::default()
        };
        c.create_shed(&req, &sink).await;
        assert_eq!(*sink.error.lock().unwrap(), None, "create must recover");
        assert_eq!(sink.shed.lock().unwrap().as_deref(), Some("folio"));
        assert_eq!(*sink.progress.lock().unwrap(), vec!["building".to_string()]);
        assert_eq!(minter.calls(), 2, "exactly one re-mint");
    }

    /// A keep-alive listener that authorizes only the LATEST identity, so the
    /// first stream connect is refused with an HTTP 401 on a pooled connection.
    async fn revoked_setup() -> (TlsServer, Arc<CaMinter>, Arc<ControlTokenProvider>, Client) {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_flip_server(
            &ca,
            ServerAuthMode::MtlsAllow(vec!["SHA256:client-1".into()]),
            false,
        )
        .await;
        let minter = CaMinter::new(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());
        // One ordinary request to enroll as client-1 AND pool the connection.
        get(&c).await.unwrap();
        srv.set_mode(ServerAuthMode::MtlsAllow(vec!["SHA256:client-2".into()]));
        (srv, minter, provider, c)
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn rc_events_recovers_from_a_401_rejection_exactly_once() {
        let (_srv, minter, _provider, c) = revoked_setup().await;
        let sink = RcSink::default();
        c.rc_events(&sink).await.expect("the stream must recover");
        assert_eq!(sink.0.lock().unwrap().len(), 1);
        assert_eq!(minter.calls(), 2, "exactly one re-mint");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn create_recovers_from_a_401_rejection_exactly_once() {
        let (_srv, minter, _provider, c) = revoked_setup().await;
        let sink = CreateLog::default();
        let req = crate::models::CreateShedRequest {
            name: "folio".into(),
            ..Default::default()
        };
        c.create_shed(&req, &sink).await;
        assert_eq!(*sink.error.lock().unwrap(), None, "create must recover");
        assert_eq!(sink.shed.lock().unwrap().as_deref(), Some("folio"));
        assert_eq!(minter.calls(), 2, "exactly one re-mint");
    }

    // ---- The capture/transmit race (FIX 3) ----
    //
    // `send_authed` pins the credential it puts in the HEADER, but reqwest gives
    // no per-request TLS control: the resolver is read live during the handshake,
    // so a mint that lands between the capture and the dial means the request goes
    // out as credential N+1 while having recorded N. (A pooled connection is the
    // same problem in a different order — it presents whatever was current when it
    // was dialed.)
    //
    // Reproduced deterministically, with no timing at all: `send_authed` takes the
    // captured credential as an argument, so the test hands it a STALE one while
    // the provider holds a newer one — exactly the state the race produces.
    // Attributing the failure to the capture would dismiss it as "stale, we
    // already rotated past that", invalidate nothing, and re-send the credential
    // the server just refused.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_stale_capture_still_invalidates_and_recovers() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_flip_server(
            &ca,
            ServerAuthMode::MtlsAllow(vec!["SHA256:client-3".into()]),
            false,
        )
        .await;
        let minter = CaMinter::new(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());

        // Credential N (client-1) is captured...
        let captured = provider.credential().await.unwrap();
        assert_eq!(captured.cert_serial, "1");
        // ...and a concurrent mint moves the provider (and the resolver the
        // handshake reads) to N+1 (client-2) before this request is dialed.
        provider.invalidate().await;
        let current = provider.credential().await.unwrap();
        assert_eq!(current.cert_serial, "2");

        // The server accepts neither: only client-3, the NEXT mint, is authorized.
        let url = c.build_url(&["api", "info"], &[]).unwrap();
        let sent = c
            .send_authed(GET_TIMEOUT, Resolution::presenting(captured), |http| {
                http.get(url.clone()).timeout(GET_TIMEOUT)
            })
            .await
            .unwrap();

        assert_eq!(
            sent.resp.status().as_u16(),
            200,
            "the rejection of the credential actually transmitted must be acted on"
        );
        assert_eq!(minter.calls(), 3, "one re-mint on top of the two above");
        assert_eq!(
            provider.credential().await.unwrap().cert_serial,
            "3",
            "the refused credential must not still be cached"
        );
    }

    // ---- mtls half of the C2(i) contract ----
    //
    // The token half (`http::tests::a_surviving_credential_is_presented_when_the
    // _mint_fails`) proves the survivor goes out in a header; this proves the
    // same for the credential that travels in the HANDSHAKE. The armed
    // certificate outlives a failed re-mint (the provider's cache and the
    // resolver are both untouched by a mint failure), so the request proceeds
    // presenting it rather than failing with the mint error — Go's
    // "presenting something the server might still accept beats presenting
    // nothing" (`66abaa9`).
    #[tokio::test(flavor = "multi_thread")]
    async fn an_armed_certificate_is_still_presented_when_the_re_mint_fails() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        // close_after_each: request #2 must re-handshake, so what it presents is
        // observable rather than inherited from a pooled connection.
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, true).await;
        let minter = FailAfterEnroll::new(CaMinter::new(ca.clone()));
        let clock = Arc::new(std::sync::atomic::AtomicU64::new(1_700_000_000));
        let ticking = clock.clone();
        let provider = Arc::new(
            ControlTokenProvider::new("s".into(), minter.clone())
                .with_now(move || ticking.load(Ordering::SeqCst)),
        );
        let c = client_for(&srv, provider.clone());
        get(&c).await.unwrap(); // enrolled as client-1, certificate armed

        // The clock moves past the certificate's expiry, so the next resolution
        // MUST mint — and the host-agent can no longer mint one.
        clock.store(4_000_000_000, Ordering::SeqCst);
        minter.set_fail();

        get(&c)
            .await
            .expect("the surviving certificate must still be presented");
        assert_eq!(minter.failed_calls(), 1, "the re-mint was attempted, once");
        assert_eq!(
            srv.client_cns(),
            vec!["SHA256:client-1".to_string(), "SHA256:client-1".to_string()],
            "the second handshake presented the surviving certificate"
        );
        assert!(
            provider.cert_resolver().current().is_some(),
            "a failed mint must not withdraw the armed identity"
        );
        // And the resolution HANDS BACK that identity rather than a bare `None`:
        // the request captures it, so a rejection of the surviving certificate is
        // attributed and invalidated like any other. (Before C2(i) this resolved
        // to `None` — the certificate went out via the resolver with the request
        // believing it carried nothing.) The mint error rides along with it.
        let resolved = c.credential().await;
        let presented = resolved.cred.expect("something to present");
        assert_eq!(presented.mode, Some(AuthMode::Mtls));
        assert_eq!(presented.cert_serial, "1");
        assert!(
            resolved.mint_error.is_some(),
            "the failed mint is carried alongside the survivor"
        );
        assert_eq!(minter.failed_calls(), 2, "one attempt per resolution");
    }

    // The pool purge is driven by the INVALIDATION, not by a successful re-mint.
    //
    // A refused certificate leaves every idle pooled connection carrying it (they
    // were all dialed under that identity). Recycling only after a successful
    // re-mint meant a failed one left those connections checked in, ready to
    // re-present the refused certificate on the next request — and the server's
    // `Connection: close` closes only the connection the 401 came back on.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_refused_identity_purges_the_pool_even_when_the_re_mint_fails() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_flip_server(
            &ca,
            ServerAuthMode::MtlsAllow(vec!["SHA256:client-1".into()]),
            false, // keep-alive: the refused connection stays pooled
        )
        .await;
        let minter = FailAfterEnroll::new(CaMinter::new(ca.clone()));
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());

        get(&c).await.unwrap(); // enrolled as client-1, connection pooled
        let poisoned_pool = c.transport_id();

        // The identity is revoked AND the host-agent can no longer mint.
        srv.set_mode(ServerAuthMode::MtlsAllow(vec!["SHA256:client-2".into()]));
        minter.set_fail();

        let err = get_stable(&c).await.unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains(MTLS_MINT_DOWN),
            "the failed re-mint is what the caller hears about: {err:?}"
        );
        assert_ne!(
            c.transport_id(),
            poisoned_pool,
            "the pool holding the refused identity must be gone, re-mint or not"
        );
        assert!(
            provider.cert_resolver().current().is_none(),
            "the refused certificate is withdrawn"
        );
    }

    // Fail-closed: a certificate that does not match the key we generated is
    // never adopted, never presented, and surfaces as an error.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_certificate_for_a_foreign_key_is_refused() {
        let ca = Arc::new(TestCa::new("shed-ca"));
        let srv = spawn_mtls_server(&ca, TlsVersion::Any, false).await;
        let minter = CaMinter::foreign(ca.clone());
        let provider = Arc::new(ControlTokenProvider::new("s".into(), minter.clone()));
        let c = client_for(&srv, provider.clone());

        let err = get_stable(&c).await.unwrap_err();
        // Nothing usable was ever installed, so the request goes out with no
        // certificate and the listener refuses it inside the handshake — and the
        // error the caller sees names WHY there was nothing to present, with the
        // TLS refusal as context. (It used to report only the listener's generic
        // `CertificateRequired` alert: the C2(i) swallow.)
        assert!(provider.cert_resolver().current().is_none());
        let msg = err.to_string();
        assert!(
            msg.contains("minted client certificate unusable"),
            "the adoption failure must lead: {err:?}"
        );
        assert_eq!(
            srv.handshake_count(),
            0,
            "no handshake COMPLETED — the listener requires a certificate"
        );
        assert!(provider.credential().await.is_err());
    }
}
