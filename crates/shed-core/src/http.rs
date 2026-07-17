//! HTTP read client for one shed-server.
//!
//! reqwest + rustls; the base URL is injected (the app substitutes the hermetic
//! mock in test mode — the core is env-agnostic). Decoding is the defensive
//! `models` layer.
//!
//! Parity with Swift's `ShedServerClient`: an 8s GET timeout, an explicit
//! User-Agent, an https-only redirect policy, leaf-cert pinning (fail-closed on
//! a non-https URL), a control-token bearer with a 401 → invalidate + retry-once
//! (provider-backed only; the invalidate is the stale-401-guarded
//! `invalidate_if_current` on the token actually SENT, and the retry is
//! skipped when the re-mint returns the same rejected token — plan 001 §3.4),
//! and `ShedError` matching `ShedClientError`. Lifecycle + SSE create land in
//! M4.

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

/// A read client for one shed-server host. `Clone` is cheap (reqwest::Client and
/// the token provider are Arc-backed) so a create task can own its own handle
/// sharing the same token cache.
#[derive(Clone)]
pub struct Client {
    base_url: String,
    server_name: String,
    /// Static open-mode config token; used only when there is no `token_provider`.
    token: String,
    token_provider: Option<Arc<ControlTokenProvider>>,
    http: reqwest::Client,
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
        let http = build_http_client(pin.as_deref())?;
        Ok(Self {
            base_url,
            server_name,
            token,
            token_provider,
            http,
        })
    }

    /// The bearer token to send, or `None`. Provider-backed clients send the
    /// minted token, or NOTHING on a mint failure (never the static token — no
    /// secure-by-default downgrade); provider-less clients send the static token.
    pub(crate) async fn bearer(&self) -> Option<String> {
        if let Some(p) = &self.token_provider {
            p.token().await.ok().filter(|t| !t.is_empty())
        } else if !self.token.is_empty() {
            Some(self.token.clone())
        } else {
            None
        }
    }

    /// 401 aftermath, shared by [`Self::request`], [`Self::create_stream`],
    /// and [`Self::rc_events`]: drop the provider's cache for the token
    /// actually SENT (`invalidate_if_current` — the stale-401 guard: a 401
    /// racing a rotation must not erase the newer token; plan 001 §3.4),
    /// BOUNDED by `bound`: invalidate only contends on the provider mutex,
    /// but that mutex is held across a mint by design, so a concurrent
    /// wedged mint could otherwise pin the await forever. Returns `false`
    /// when the bound elapsed — the caller then surfaces the original 401
    /// without retrying (the cache keeps the stale token one extra cycle and
    /// the next 401 retries the invalidate). No-op `true` for provider-less
    /// clients or when no token was sent. Retry policy stays with each
    /// caller (request retries once; the streams don't — their watchers own
    /// re-auth).
    async fn invalidate_sent(&self, sent: Option<&str>, bound: Duration) -> bool {
        let (Some(p), Some(tok)) = (&self.token_provider, sent) else {
            return true;
        };
        tokio::time::timeout(bound, p.invalidate_if_current(tok))
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
        let mut url = reqwest::Url::parse(&self.base_url).map_err(|e| {
            ShedError::Config(format!("invalid base URL {}: {e}", self.base_url))
        })?;
        {
            let mut parts = url.path_segments_mut().map_err(|_| {
                ShedError::Config(format!(
                    "base URL {} cannot carry a path",
                    self.base_url
                ))
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

    /// One attempt with `bearer` (resolved by the CALLER — [`Self::request`]
    /// resolves it per attempt so the 401 path knows exactly which token was
    /// sent and can `invalidate_if_current` it; plan 001 §3.4).
    async fn send_once(
        &self,
        method: reqwest::Method,
        url: &reqwest::Url,
        timeout: Duration,
        body: Option<&serde_json::Value>,
        bearer: Option<&str>,
    ) -> Result<Vec<u8>, ShedError> {
        let mut req = self.http.request(method, url.clone()).timeout(timeout);
        if let Some(b) = body {
            req = req.json(b);
        }
        if let Some(tok) = bearer {
            req = req.bearer_auth(tok);
        }
        let resp = req
            .send()
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        if !(200..300).contains(&status) {
            return Err(ShedError::BadStatus(status));
        }
        Ok(resp
            .bytes()
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?
            .to_vec())
    }

    /// Send once, and on a provider-backed 401 invalidate the token actually
    /// SENT ([`Self::invalidate_sent`], bounded by this request's own
    /// `timeout`) then retry once with a re-resolved bearer (at-most-once,
    /// mirrors the SDK/CLI). The retry is SKIPPED — surfacing the original
    /// 401 — when:
    ///   * the invalidate timed out (a wedged mint holds the provider mutex;
    ///     the follow-up bearer resolution would block on the same mutex);
    ///   * the re-resolved bearer is `None` — a provider-backed request never
    ///     retries UNAUTHENTICATED: the re-mint failed, and dropping the
    ///     Authorization header is a guaranteed second 401;
    ///   * the re-resolved bearer equals the rejected one — a same-token
    ///     retry is a guaranteed second 401 (Dart's `_mustMint` semantics
    ///     make the provider force-mint here, so equality means the minter
    ///     re-returned the rejected token). The successful same-token re-mint
    ///     also re-CACHED the rejected token, so it is invalidated again
    ///     before returning — the must-mint flag stays armed and no later
    ///     call serves it.
    ///
    /// Static/no-token clients don't retry. `body`, when present, is a JSON
    /// request body (re-sent on the retry).
    async fn request(
        &self,
        method: reqwest::Method,
        url: &reqwest::Url,
        timeout: Duration,
        body: Option<&serde_json::Value>,
    ) -> Result<Vec<u8>, ShedError> {
        let sent = self.bearer().await;
        match self
            .send_once(method.clone(), url, timeout, body, sent.as_deref())
            .await
        {
            Err(ShedError::BadStatus(401)) if self.token_provider.is_some() => {
                if !self.invalidate_sent(sent.as_deref(), timeout).await {
                    return Err(ShedError::BadStatus(401)); // wedged provider — no retry
                }
                let retry = self.bearer().await;
                if retry.is_none() {
                    return Err(ShedError::BadStatus(401)); // never retry unauthenticated
                }
                if retry == sent {
                    // Re-arm the must-mint flag the same-token re-mint just
                    // cleared, so the rejected token is never served again.
                    let _ = self.invalidate_sent(sent.as_deref(), timeout).await;
                    return Err(ShedError::BadStatus(401));
                }
                self.send_once(method, url, timeout, body, retry.as_deref())
                    .await
            }
            other => other,
        }
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
        self.get_json(&["api", "sheds", shed, "sessions"], &[]).await
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
            &["api", "sheds", shed, "rc", "v1", "sessions", slug, "messages"],
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
    /// ONE connection — no in-method reconnect and no 401 retry: the reconnect
    /// loop (shed-app's `RcEventsWatcher`) owns backoff AND re-auth. On a 401
    /// this invalidates the token it SENT (`invalidate_if_current` — the
    /// stale-401 guard, plan §3.4) and returns `BadStatus(401)`, so the
    /// watcher's next connect re-mints; an in-method retry would double up
    /// on the watcher's backoff and hide the auth failure from its Down
    /// signal.
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
        // phase — bearer resolution (a foreign `TokenMinter` impl can hang
        // indefinitely) plus the request send — so no pre-stream await sits
        // outside the bound (a server that accepts but never responds is as
        // dead as a silent stream).
        let connect = async {
            let bearer = self.bearer().await;
            let mut rb = self
                .http
                .get(url)
                .header(reqwest::header::ACCEPT, "text/event-stream");
            if let Some(tok) = &bearer {
                rb = rb.bearer_auth(tok);
            }
            (bearer, rb.send().await)
        };
        let (sent_bearer, resp) = match tokio::time::timeout(idle, connect).await {
            Err(_) => {
                return Err(ShedError::Transport("rc-events connect timeout".into()));
            }
            Ok((bearer, r)) => (bearer, r.map_err(|e| ShedError::Transport(e.to_string()))?),
        };
        let status = resp.status().as_u16();
        if status == 401 {
            // Stale token: invalidate the token we SENT ([`Self::invalidate_sent`],
            // the stale-401 guard, bounded by `idle` — see the helper's docs)
            // so the NEXT connect re-mints, then surface the 401 — the
            // watcher owns re-auth (no in-method retry). On a timed-out
            // invalidate we fall through and surface the 401 anyway.
            let _ = self.invalidate_sent(sent_bearer.as_deref(), idle).await;
        }
        // Exactly 200, not any-2xx: SSE lives in a 200 response body — a
        // 204/206 minted by an intermediary carries no event stream, and
        // treating it as success would end as a silent empty-stream Ok,
        // masking the fault from the watcher's Down/backoff signal.
        if status != 200 {
            return Err(ShedError::BadStatus(status));
        }
        let mut stream = resp.bytes_stream();
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
                    let chunk = chunk.map_err(|e| ShedError::Transport(e.to_string()))?;
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
        let bearer = self.bearer().await;
        let mut rb = self
            .http
            .post(url)
            .header(reqwest::header::ACCEPT, "text/event-stream")
            .json(req);
        if let Some(tok) = &bearer {
            rb = rb.bearer_auth(tok);
        }
        let resp = match tokio::time::timeout(CREATE_IDLE_TIMEOUT, rb.send()).await {
            Err(_) => {
                return Err(ShedError::Create("create stream idle timeout".into()));
            }
            Ok(r) => r.map_err(|e| ShedError::Transport(e.to_string()))?,
        };
        let status = resp.status().as_u16();
        if status == 401 {
            // Invalidate the token we SENT ([`Self::invalidate_sent`], the
            // stale-401 guard, bounded by the create idle timeout — see the
            // helper's docs) so the next attempt re-mints; create stays
            // one-shot (no retry), so a timed-out invalidate just falls
            // through to the BadStatus below.
            let _ = self
                .invalidate_sent(bearer.as_deref(), CREATE_IDLE_TIMEOUT)
                .await;
        }
        if !(200..300).contains(&status) {
            return Err(ShedError::BadStatus(status));
        }
        let mut stream = resp.bytes_stream();
        let mut parser = SseParser::new();
        let mut saw_complete = false;
        loop {
            match tokio::time::timeout(CREATE_IDLE_TIMEOUT, stream.next()).await {
                Err(_) => return Err(ShedError::Create("create stream idle timeout".into())),
                Ok(None) => break,
                Ok(Some(chunk)) => {
                    let chunk = chunk.map_err(|e| ShedError::Transport(e.to_string()))?;
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

fn build_http_client(pin: Option<&str>) -> Result<reqwest::Client, ShedError> {
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
        builder = builder.use_preconfigured_tls(crate::tls::pinned_client_config(pin)?);
    }
    builder
        .build()
        .map_err(|e| ShedError::Transport(e.to_string()))
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
        // re-mint FAILS, so bearer() resolves None. The retry must be skipped
        // — a provider-backed request never drops its Authorization header
        // (an unauthenticated retry against a secure server is a guaranteed
        // second 401 and ships the request with no proof of identity).
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
        assert!(matches!(err, ShedError::BadStatus(401)), "got {err:?}");
        assert_eq!(authed.hits_async().await, 1, "exactly one authed attempt");
        assert_eq!(unauth.hits_async().await, 0, "no unauthenticated retry");
        assert_eq!(minter.calls.load(Ordering::SeqCst), 2); // initial + failed re-mint
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
    async fn invalidate_sent_is_bounded_when_the_provider_mutex_is_wedged() {
        // C6 adversarial review #5b: invalidate_if_current contends on the
        // provider mutex, which is held ACROSS a mint by design — a wedged
        // concurrent mint must not pin the 401 path forever. Wedge the mutex
        // with a never-resolving mint, then assert invalidate_sent returns
        // false within its bound instead of hanging.
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
        let invalidated = tokio::time::timeout(
            Duration::from_secs(5),
            c.invalidate_sent(Some("tok-x"), Duration::from_millis(100)),
        )
        .await
        .expect("invalidate_sent must return within its bound, not hang");
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
    async fn create_401_invalidates_provider_without_retry() {
        // Create is one-shot (no 401 retry), but must still invalidate a stale
        // cached token so the next attempt remints.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/sheds")
                    .header("authorization", "Bearer tok-1");
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
        // Prime the provider cache with tok-1.
        let _ = c.bearer().await;
        assert_eq!(minter.calls.load(Ordering::SeqCst), 1);

        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "x".into(),
            ..Default::default()
        };
        c.create_shed(&req, sink.as_ref()).await;
        assert!(
            sink.snapshot()
                .error
                .as_deref()
                .is_some_and(|e| e.contains("401") || e.contains("BadStatus") || e.contains("create")),
            "create should surface the 401: {:?}",
            sink.snapshot().error
        );
        // Still only one mint so far (no create retry); invalidate forces remint next.
        assert_eq!(minter.calls.load(Ordering::SeqCst), 1);
        let _ = c.bearer().await;
        assert_eq!(
            minter.calls.load(Ordering::SeqCst),
            2,
            "401 on create must invalidate so the next bearer remints"
        );
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
        assert!(matches!(c.list_sessions("").await, Err(ShedError::Config(_))));
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
    async fn rc_events_401_invalidates_provider_with_no_second_request() {
        // 401 → invalidate + Err(BadStatus(401)), and NO in-method retry (the
        // C5 watcher owns reconnect/re-auth): exactly one request hits the
        // server, and the next bearer() re-mints.
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/rc/events")
                    .header("authorization", "Bearer tok-1");
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
        assert_eq!(m.hits_async().await, 1, "401 must not retry in-method");
        assert!(sink.events().is_empty());
        // Invalidated: one mint so far, the next bearer() re-mints.
        assert_eq!(minter.calls.load(Ordering::SeqCst), 1);
        let _ = c.bearer().await;
        assert_eq!(
            minter.calls.load(Ordering::SeqCst),
            2,
            "401 must invalidate so the watcher's next connect re-mints"
        );
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
            ShedError::Transport(msg) => assert!(msg.contains("connect"), "got: {msg}"),
            other => panic!("expected Transport connect timeout, got {other:?}"),
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
