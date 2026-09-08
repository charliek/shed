//! The hub's state + config + input gate — `internal/ext/rc/hub.go`: the
//! daemon's CORE (config resolution, the four-lock state shape, the per-slug
//! input locks, `inputAccepted`, the idle-exit decision) from H9, and the
//! HTTP shell (`handler`/`serveOn`/bind-as-lock/health, the sessions /
//! messages / input handlers, the §2.5 env seams, the reconcile-loop driver)
//! on axum 0.8 from H10.
//!
//! Machine-posture deltas (plan 010 §2.3/§2.4 — deliberate, not parity debt):
//! the Rust hub is a SUPERVISED resident role inside shed-host-agent, so the
//! Go daemonization surface (`DetachHub`'s setsid double-fork, `EnsureHub`,
//! the respawn handoff, the advisory pidfile) is NOT ported. The idle-exit
//! DECISION (`shouldIdleExit`) is ported and unit-mirrored for Go parity, but
//! the host-agent role configures "never".

use std::collections::HashMap;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use axum::extract::{Path, State};
use chrono::{DateTime, Utc};
use shed_core::rc::{RcActivity, RcSessionDto, RcState};
use shed_rc_engine::tmux::Tmux;
use shed_rc_engine::tmux::TmuxRunner;

use super::events::Subscriber;
use super::reconcile::TrackedSession;
use super::watch::LogFn;

/// The fixed loopback TCP port the rc hub listens on (`HubPort`, `hub.go:55`).
/// 1029 sits just past the guest agent's 1028 TCP-proxy port; on a machine the
/// same fixed port keeps `sx`'s probe and the mixed-window bind handoff
/// (Go hub ⇄ agent-hosted hub) trivially aligned.
pub const HUB_PORT: u16 = 1029;

/// The hub's bind/dial address (`HubAddr`, `hub.go:62`). The bind is
/// 127.0.0.1 ONLY — a SECURITY invariant, not a default: the hub is
/// unauthenticated and trusts the loopback. Binding 0.0.0.0 would expose an
/// unauthenticated control surface. Never widen this to a non-loopback
/// interface.
pub const HUB_ADDR: &str = "127.0.0.1:1029";

/// The identity token GET /v1/health returns in `app` (`HubAppID`,
/// `hub.go:378`). Byte-frozen: the bind-as-lock and probe paths verify this
/// token so a foreign process squatting the port is an error, never mistaken
/// for a running hub.
pub const HUB_APP_ID: &str = "shed-rc-hub";

// Hub tuning defaults (`hub.go:84-106`). All overridable via HubConfig for
// tests; production uses these.
pub const DEFAULT_ACTIVE_INTERVAL: Duration = Duration::from_secs(2);
pub const DEFAULT_IDLE_INTERVAL: Duration = Duration::from_secs(10);
pub const DEFAULT_IDLE_TIMEOUT: Duration = Duration::from_secs(15 * 60);
pub const DEFAULT_HEARTBEAT: Duration = Duration::from_secs(25);
pub const DEFAULT_WRITE_TIMEOUT: Duration = Duration::from_secs(10);
pub const DEFAULT_SUBSCRIBER_BUFFER: usize = 256;

/// Configures a hub (`HubConfig`, `hub.go:112`). `runner` and `getenv` are
/// required; everything else is optional and falls back to the defaults (the
/// zero durations/ints are the "use default" signal), so tests pin a fast
/// clock, tiny intervals, and a throwaway loopback address while production
/// passes only the seams.
pub struct HubConfig {
    /// Runs tmux (the same injectable seam as the one-shot engine).
    pub runner: Arc<dyn TmuxRunner + Send + Sync>,
    /// Reads the environment (for $HOME → the JSONL roots). Injected for tests.
    pub getenv: Arc<dyn Fn(&str) -> String + Send + Sync>,
    /// The clock for activity timestamps + the idle-exit decision. None → Utc::now.
    pub now: Option<Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>>,
    /// Hub diagnostics. None → stderr (Go's `log.Printf` default).
    pub logf: Option<LogFn>,
    /// Overrides the bind/dial address. "" → [`HUB_ADDR`] (the loopback
    /// invariant). Tests set an ephemeral 127.0.0.1 address.
    pub addr: String,
    /// The embedding binary's version string, served by /v1/health (plan 010
    /// §2.4 — the Rust hub has no `version.Info()`; the host-agent passes its
    /// own).
    pub version: String,

    // Tuning overrides (zero → the matching default).
    pub active_interval: Duration,
    pub idle_interval: Duration,
    /// Zero → the 15 m default, mirroring Go's `resolve()`. "Never" is
    /// expressed by the embedder passing a huge value (the host-agent role
    /// does) — the Go seam cannot express "never" either (§2.5).
    pub idle_timeout: Duration,
    pub heartbeat: Duration,
    pub write_timeout: Duration,
    pub subscriber_buffer: usize,
}

/// HubConfig with every default applied (`hubResolved`, `hub.go:145`).
pub struct HubResolved {
    pub runner: Arc<dyn TmuxRunner + Send + Sync>,
    pub getenv: Arc<dyn Fn(&str) -> String + Send + Sync>,
    pub now: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
    pub logf: LogFn,
    pub addr: String,
    pub version: String,
    pub active_interval: Duration,
    pub idle_interval: Duration,
    pub idle_timeout: Duration,
    pub heartbeat: Duration,
    pub write_timeout: Duration,
    pub sub_buffer: usize,
}

impl HubConfig {
    /// Applies the defaults (`resolve`, `hub.go`).
    fn resolve(self) -> HubResolved {
        fn dur(v: Duration, def: Duration) -> Duration {
            if v.is_zero() {
                def
            } else {
                v
            }
        }
        HubResolved {
            runner: self.runner,
            getenv: self.getenv,
            // shed-broker's chrono has no `clock` feature — the wall clock
            // comes from SystemTime (same instant Go's time.Now reads).
            now: self.now.unwrap_or_else(|| {
                Arc::new(|| DateTime::<Utc>::from(std::time::SystemTime::now()))
            }),
            logf: self
                .logf
                .unwrap_or_else(|| Arc::new(|line| eprintln!("{line}"))),
            addr: if self.addr.is_empty() {
                HUB_ADDR.to_string()
            } else {
                self.addr
            },
            version: self.version,
            active_interval: dur(self.active_interval, DEFAULT_ACTIVE_INTERVAL),
            idle_interval: dur(self.idle_interval, DEFAULT_IDLE_INTERVAL),
            idle_timeout: dur(self.idle_timeout, DEFAULT_IDLE_TIMEOUT),
            heartbeat: dur(self.heartbeat, DEFAULT_HEARTBEAT),
            write_timeout: dur(self.write_timeout, DEFAULT_WRITE_TIMEOUT),
            sub_buffer: if self.subscriber_buffer == 0 {
                DEFAULT_SUBSCRIBER_BUFFER
            } else {
                self.subscriber_buffer
            },
        }
    }
}

/// The reconcile state guarded by the track lock (`trackMu`-guarded fields,
/// `hub.go:224-226`).
pub(crate) struct TrackState {
    pub tracked: HashMap<String, TrackedSession>,
    /// When the session count first hit zero (`idleSince`; `None` = sessions
    /// exist). Go's zero `time.Time` maps to `None`.
    pub idle_since: Option<DateTime<Utc>>,
}

/// A running rc hub (`Hub`, `hub.go:220`). Construct with [`Hub::new`].
///
/// TWO independent locks, with Go's documented order preserved
/// (`hub.go:243-251`): `track` guards the reconcile state, and `subs` is kept
/// separate so broadcast can never deadlock against reconcile. **Lock order:
/// track → watcher.mu, never reversed.**
///
/// It held two more until A6 (`charliek/shed#322`): `input_locks` (the
/// per-SLUG input-delivery mutexes) and `ingest` (the cursor pre-watcher
/// queues). Both belonged to lanes that commit removed — `POST /input` answers
/// 409 without touching a pane, and the ingest route is gone.
pub struct Hub {
    pub(crate) cfg: HubResolved,
    pub(crate) track: Mutex<TrackState>,
    pub(crate) subs: Mutex<Vec<Arc<Subscriber>>>,
}

impl Hub {
    /// `newHub`, `hub.go:254`.
    pub fn new(cfg: HubConfig) -> Hub {
        Hub {
            cfg: cfg.resolve(),
            track: Mutex::new(TrackState {
                tracked: HashMap::new(),
                idle_since: None,
            }),
            subs: Mutex::new(Vec::new()),
        }
    }

    pub(crate) fn lock_track(&self) -> std::sync::MutexGuard<'_, TrackState> {
        self.track.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The SSE fan-out set (`subMu`-guarded `subs`, `hub.go:231`). Its own
    /// lock, kept off the track lock, so broadcast can never deadlock against
    /// reconcile.
    pub(crate) fn lock_subs(&self) -> std::sync::MutexGuard<'_, Vec<Arc<Subscriber>>> {
        self.subs.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// Whether the hub has held zero rc sessions for at least the idle
    /// timeout (`shouldIdleExit`, `hub.go:873`). Subscribers deliberately do
    /// not extend the window. Ported for Go parity (§2.4); the host-agent
    /// role passes an effectively-infinite timeout.
    pub fn should_idle_exit(&self, now: DateTime<Utc>) -> bool {
        let ts = self.lock_track();
        let Some(since) = ts.idle_since else {
            return false;
        };
        let Ok(timeout) = chrono::Duration::from_std(self.cfg.idle_timeout) else {
            return false; // an effectively-infinite timeout can never elapse
        };
        now.signed_duration_since(since) >= timeout
    }

    /// Releases every tracked session's watcher — hub shutdown
    /// (`closeAllWatchers`, `hub.go:859`).
    pub fn close_all_watchers(&self) {
        let mut ts = self.lock_track();
        for tr in ts.tracked.values_mut() {
            if let Some(w) = tr.watcher.take() {
                w.close();
            }
        }
    }
    // `input_accepted` — the gated-input acceptance merge (merged
    // needs_approval, the watcher's open-approval blocker, the kind's approval
    // anchor on the fresh visible frame) — lived here. `POST /input` answers
    // 409 `not_accepting` for every kind since A6 (`charliek/shed#322`), so
    // nothing calls it.
}

/// The lifecycle-trumps-activity precedence rule (`DisplayActivity`,
/// `activity.go:60`): a blocking lifecycle state (needs-trust / needs-auth /
/// dead) suppresses the session's whole activity dimension — Go's empty
/// `Activity` return is `None` here, so the omitted DTO fields drop out.
/// Suppression covers activity_at AND last_message alongside the activity.
pub fn display_activity(state: RcState, activity: RcActivity) -> Option<RcActivity> {
    match state {
        RcState::NeedsTrust | RcState::NeedsAuth | RcState::Dead => None,
        _ => Some(activity),
    }
}

// ---------------------------------------------------------------------------
// The HTTP shell (plan 010 H10, §2.2): axum 0.8 routes the frozen /v1
// surface; the Router is served from a hyper-util accept loop so the Go
// server's per-connection posture (`hub.go:746-757`) — ReadHeaderTimeout 10s,
// ReadTimeout 30s, deliberately NO global write timeout (the SSE stream paces
// its own frames) — is set per-connection. Contract-shaped body handling is
// manual (verbs.rs), bypassing axum's extractors.
// ---------------------------------------------------------------------------

/// `writeJSON` (`hub.go:693`). Go uses `json.NewEncoder(..).Encode`, which
/// appends a trailing newline — matched here, byte-for-byte.
///
/// RECORDED STATUS-LINE DELTA (accepted): the REASON PHRASE is the HTTP
/// stack's, not ours — Go's `net/http` writes 413 as "Request Entity Too
/// Large" (the RFC 2616 spelling) where hyper writes "Payload Too Large" (RFC
/// 7231). The phrase is explicitly non-semantic in HTTP/1.1 and no client
/// reads it; the code + this JSON body are the contract.
pub(crate) fn write_json<T: serde::Serialize>(
    status: http::StatusCode,
    v: &T,
) -> axum::response::Response {
    let mut body = serde_json::to_vec(v).unwrap_or_else(|_| b"{}".to_vec());
    body.push(b'\n');
    axum::response::Response::builder()
        .status(status)
        .header("Content-Type", "application/json")
        .body(axum::body::Body::from(body))
        .expect("static response head")
}

/// The hub's JSON error envelope `{error, message}` (`writeError`,
/// `hub.go:702`). Go marshals a `map[string]string`, whose keys sort
/// alphabetically — `error` before `message` — matched by field order here.
#[derive(serde::Serialize)]
struct ErrorEnvelope<'a> {
    error: &'a str,
    message: &'a str,
}

pub(crate) fn write_error(
    status: http::StatusCode,
    code: &str,
    msg: &str,
) -> axum::response::Response {
    write_json(
        status,
        &ErrorEnvelope {
            error: code,
            message: msg,
        },
    )
}

/// The GET /v1/health payload — the identity handshake (`hubHealth`,
/// `hub.go:381`). `version` is the embedding binary's string (plan 010 §2.4);
/// no probe parses it.
#[derive(Debug, serde::Serialize, serde::Deserialize)]
pub struct HubHealth {
    pub app: String,
    pub version: String,
    pub pid: u32,
}

/// The GET /v1/sessions body (`hubSessionsResponse`, `hub.go:368`) — the
/// enriched session array only; capability discovery stays on the one-shot
/// path.
#[derive(Debug, Default, serde::Serialize, serde::Deserialize)]
pub struct HubSessionsResponse {
    pub sessions: Vec<RcSessionDto>,
}

/// The POST /input body (`inputRequest`, `hub.go:447`). `text` rides
/// `null_default` for Go's `json.Unmarshal(null)` no-op: `{"text":null}`
/// leaves the zero value (→ 400 empty_text), where a bare serde `String` would
/// call it a type error (→ 400 invalid_json). Same status class, different
/// contract byte — the differential harness pins the code.
#[derive(Debug, Default, serde::Deserialize)]
pub(crate) struct InputRequest {
    #[serde(default, deserialize_with = "super::messages::null_default")]
    pub text: String,
}

/// `handleHealth` (`hub.go:388`).
async fn handle_health(State(hub): State<Arc<Hub>>) -> axum::response::Response {
    write_json(
        http::StatusCode::OK,
        &HubHealth {
            app: HUB_APP_ID.to_string(),
            version: hub.cfg.version.clone(),
            pid: std::process::id(),
        },
    )
}

/// `handleSessions` (`hub.go:317`): the one-shot List (unchecked listing —
/// a transient failure reads as an empty list, exactly Go's `List`) with each
/// session's derived activity + pending_approvals overlaid from its tracker.
async fn handle_sessions(State(hub): State<Arc<Hub>>) -> axum::response::Response {
    let list_hub = Arc::clone(&hub);
    // A tmux hiccup inside List already reads as an empty list (Go's `List`);
    // a PANICKED enumeration is a different animal and must not be laundered
    // into a successful "no sessions" — the aggregator would render every
    // session as gone. DELTA vs Go, where a panicking handler kills the
    // connection: 500 with the hub's own envelope is the closer client
    // contract than a torn connection.
    let mut sessions = match tokio::task::spawn_blocking(move || {
        let tmux = Tmux::new(&*list_hub.cfg.runner);
        super::reconcile::sessions_for_names(&tmux, &tmux.list_session_names())
    })
    .await
    {
        Ok(s) => s,
        Err(e) => {
            (hub.cfg.logf)(&format!("rc hub: session listing failed: {e}"));
            return write_error(
                http::StatusCode::INTERNAL_SERVER_ERROR,
                "internal_error",
                "session listing failed",
            );
        }
    };

    {
        let ts = hub.lock_track();
        for s in &mut sessions {
            let Some(tr) = ts.tracked.get(&s.slug) else {
                continue;
            };
            if !tr.same_identity(s) {
                continue;
            }
            // tr.activity already has DisplayActivity applied; None means
            // "suppress the whole activity dimension" so the optional DTO
            // fields drop out — the wire contract for a gated/dead session.
            if let Some(a) = tr.activity {
                s.activity = Some(a);
                s.activity_at = Some(tr.activity_at.clone());
                s.last_message = (!tr.last_message.is_empty()).then(|| tr.last_message.clone());
            }
            // pending_approvals is a HUB-LAYER overlay (the one-shot List
            // never sets it). Copied, never aliased; an empty snapshot maps
            // to None, which serialization omits (Go's nil + omitempty).
            let snap = super::reconcile::copy_approvals(&tr.approval_snapshot());
            s.pending_approvals = (!snap.is_empty()).then(|| {
                snap.into_iter()
                    .map(|a| shed_core::rc::RcFeedApproval {
                        id: a.id,
                        status: a.status,
                        decision: (!a.decision.is_empty()).then_some(a.decision),
                        decisions: a.decisions,
                    })
                    .collect()
            });
        }
    }
    write_json(http::StatusCode::OK, &HubSessionsResponse { sessions })
}

/// Go's `url.Values.Get` for one query key (`r.URL.Query().Get`): the FIRST
/// occurrence wins on duplicates, absent → "" (H10 review: axum's
/// `Query<HashMap>` is last-wins, which flipped outcomes on duplicated
/// params). `form_urlencoded` applies the same `+`-is-space and
/// percent-decoding rules.
pub(crate) fn query_get(query: Option<&str>, key: &str) -> String {
    let Some(query) = query else {
        return String::new();
    };
    form_urlencoded::parse(query.as_bytes())
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.into_owned())
        .unwrap_or_default()
}

/// `handleMessages` (`hub.go:405`): a page of the session's feed ring after
/// the exclusive `since` seq, bounded by `limit` (≤200, default 100). 404 for
/// an unknown slug, 400 for a malformed `since`/`limit`. POLICY (intended
/// asymmetry with DisplayActivity): message history REMAINS readable while a
/// blocking lifecycle state gates the activity dimension and input posting —
/// the ring holds pre-gate content the operator already saw on the pane.
async fn handle_messages(
    State(hub): State<Arc<Hub>>,
    Path(slug): Path<String>,
    axum::extract::RawQuery(query): axum::extract::RawQuery,
) -> axum::response::Response {
    let mut since = 0u64;
    let raw = query_get(query.as_deref(), "since");
    if !raw.is_empty() {
        // DIGITS ONLY, before the parse: Go reads `since` with
        // `strconv.ParseUint`, which rejects a sign, while `u64::from_str`
        // accepts a leading `+` — so `?since=+5` is a 400 in Go and would be
        // a 200 here. (`limit` needs no such guard: Go reads it with
        // `strconv.Atoi`, which DOES accept a sign, matching `i64::from_str`.)
        let digits = raw.bytes().all(|b| b.is_ascii_digit());
        match raw.parse::<u64>() {
            Ok(v) if digits => since = v,
            _ => {
                return write_error(
                    http::StatusCode::BAD_REQUEST,
                    "invalid_since",
                    "since must be a non-negative integer",
                );
            }
        }
    }
    let mut limit = super::messages::DEFAULT_MESSAGES_LIMIT as i64;
    let raw = query_get(query.as_deref(), "limit");
    if !raw.is_empty() {
        match raw.parse::<i64>() {
            Ok(v) if v >= 0 => limit = v,
            _ => {
                return write_error(
                    http::StatusCode::BAD_REQUEST,
                    "invalid_limit",
                    "limit must be a non-negative integer",
                );
            }
        }
    }

    let ring = {
        let ts = hub.lock_track();
        match ts.tracked.get(&slug) {
            Some(tr) => Arc::clone(&tr.ring),
            None => {
                drop(ts);
                return write_error(
                    http::StatusCode::NOT_FOUND,
                    "unknown_slug",
                    "no such rc session",
                );
            }
        }
    };
    let (messages, truncated) = ring.since(since, limit);
    // A Vec encodes an empty page as [] (Go's handler coerces its nil to
    // []feedMessage{} for exactly this — `hub.go:440-442`).
    write_json(
        http::StatusCode::OK,
        &super::messages::HubMessagesResponse {
            messages,
            truncated,
        },
    )
}

/// `handleInput` (`hub.go:455`). The route and its request validation survive
/// A6 (`charliek/shed#322`); its DELIVERY does not. The gated lane — the
/// per-slug delivery mutex, the pane re-verify, the approval-anchor/watcher
/// acceptance merge — existed only for codex and cursor, whose derived lanes
/// are gone, and `kind_features.input` is now "" for every TUI kind and "turn"
/// for opencode. So no kind is `gated` any more and a well-formed request for a
/// live session ends in 409 `not_accepting` rather than a keystroke.
///
/// Statuses (unchanged in every other respect): 400 invalid/unsafe text, 404
/// unknown slug, 409 not accepting, 413 too large. Clients read
/// `kind_features.input` to know which surface a kind takes (opencode:
/// `POST /turn`).
async fn handle_input(
    State(hub): State<Arc<Hub>>,
    Path(slug): Path<String>,
    req: axum::extract::Request,
) -> axum::response::Response {
    let body: InputRequest = match super::verbs::decode_hub_body(req.into_body()).await {
        Ok(v) => v,
        Err(resp) => return resp,
    };
    let text = shed_rc_engine::text::normalize_newlines(&body.text);
    if super::messages::trim_feed_text(&text).is_empty() {
        return write_error(
            http::StatusCode::BAD_REQUEST,
            "empty_text",
            "text is required",
        );
    }
    if shed_rc_engine::text::has_unsafe_prompt_chars(&text) {
        return write_error(
            http::StatusCode::BAD_REQUEST,
            "unsafe_text",
            "text contains an unsupported control character",
        );
    }

    // An unknown slug is still a 404 — the body validation above runs first so
    // a malformed request is reported as malformed regardless of which slug it
    // names. The read runs under the track lock; reconcile mutates tracked
    // under the same lock.
    {
        let ts = hub.lock_track();
        if !ts.tracked.contains_key(&slug) {
            drop(ts);
            return write_error(
                http::StatusCode::NOT_FOUND,
                "unknown_slug",
                "no such rc session",
            );
        }
    }

    write_error(
        http::StatusCode::CONFLICT,
        super::verbs::ERR_NOT_ACCEPTING,
        "this kind does not accept feed input",
    )
}

impl Hub {
    /// The hub's HTTP routes (`handler`, `hub.go:296`). axum answers a wrong
    /// method on a known path 405 and an unknown path 404 automatically, like
    /// Go's method+wildcard ServeMux — rc-helper.md forbids clients from
    /// interpreting those bare shapes, so only status codes are contract
    /// (§2.2).
    ///
    /// RECORDED MUX DELTA (accepted; the differential harness scopes these
    /// cells out): Go's `ServeMux` CLEANS the request path and answers a
    /// 301 redirect to the cleaned form — `/v1/sessions/x/approvals/..` and
    /// `//v1/health` are 301s there, 404s here. Both are rejections of a path
    /// no client sends, on the bare-mux surface clients may not interpret; the
    /// approvals-id traversal case that DOES matter (`%2E%2E`, which neither
    /// mux resolves) is pinned by `APPROVAL_ID_RE` and its 400.
    pub fn router(self: &Arc<Self>) -> axum::Router {
        use axum::routing::{get, post};
        axum::Router::new()
            .route("/v1/health", get(handle_health))
            .route("/v1/sessions", get(handle_sessions))
            .route("/v1/events", get(super::events::handle_events))
            .route("/v1/sessions/{slug}/messages", get(handle_messages))
            .route("/v1/sessions/{slug}/input", post(handle_input))
            .route("/v1/sessions/{slug}/turn", post(super::verbs::handle_turn))
            .route(
                "/v1/sessions/{slug}/interrupt",
                post(super::verbs::handle_interrupt),
            )
            .route(
                "/v1/sessions/{slug}/approvals/{id}",
                post(super::verbs::handle_approval),
            )
            .with_state(Arc::clone(self))
    }
}

/// The Go server's per-connection read posture (`hub.go:746-757`), half one:
/// `ReadHeaderTimeout: 10s` maps 1:1 onto hyper's `header_read_timeout`, which
/// re-arms for every keep-alive head exactly as Go re-arms `hdrDeadline` per
/// request.
///
/// Half two, `ReadTimeout: 30s`, deliberately does NOT live here — see
/// [`super::verbs::BODY_READ_TIMEOUT`] for why a connection-level read
/// deadline would be a mistranslation.
///
/// Writes stay unbounded on purpose: Go sets no global `WriteTimeout` because
/// the SSE stream paces its own frames (`writeSSE`'s per-frame deadline, which
/// maps to the pump's capacity-1 handoff in [`super::events`]).
const READ_HEADER_TIMEOUT: Duration = Duration::from_secs(10);

/// The accept-retry backoff window (`net/http` `Server.Serve`'s `tempDelay`
/// loop: 5 ms doubling to a 1 s ceiling).
const ACCEPT_BACKOFF_MIN: Duration = Duration::from_millis(5);
const ACCEPT_BACKOFF_MAX: Duration = Duration::from_secs(1);

/// Serves the hub's router on `listener` — the HTTP half of `serveOn`
/// (`hub.go:746`; the reconcile-loop half is [`run_reconcile_loop`]). One task
/// per connection, each with the Go per-connection header timeout. The future
/// is dropped (or the listener closed) to stop serving; the embedder (H11's
/// host-agent role) owns that lifecycle.
///
/// An accept error is LOGGED AND RETRIED rather than returned, mirroring Go's
/// `Serve` (which only returns on a permanent failure): the bind IS the hub's
/// lock, so an accept loop that fell over on one transient `ECONNABORTED` /
/// `EMFILE` would release the port and let a second hub take it.
pub async fn serve(hub: Arc<Hub>, listener: tokio::net::TcpListener) -> std::io::Result<()> {
    let router = hub.router();
    let mut backoff = ACCEPT_BACKOFF_MIN;
    loop {
        let (stream, _peer) = match listener.accept().await {
            Ok(conn) => conn,
            Err(e) => {
                (hub.cfg.logf)(&format!(
                    "rc hub: accept error ({e}); retrying in {backoff:?}"
                ));
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(ACCEPT_BACKOFF_MAX);
                continue;
            }
        };
        backoff = ACCEPT_BACKOFF_MIN;
        let svc = hyper_util::service::TowerToHyperService::new(router.clone());
        tokio::spawn(async move {
            // hyper's http1 connection DIRECTLY, not hyper-util's `auto`
            // builder: `auto` sniffs the HTTP/2 preface and would serve h2c on
            // this plain listener, a wire surface Go's HTTP/1.1-only
            // `http.Server` does not have.
            let io = hyper_util::rt::TokioIo::new(stream);
            let _ = hyper::server::conn::http1::Builder::new()
                .timer(hyper_util::rt::TokioTimer::new())
                .header_read_timeout(READ_HEADER_TIMEOUT)
                .serve_connection(io, svc)
                .await;
        });
    }
}

/// Listens on `addr`, reporting `already = true` (not an error) when the
/// address is in use — the bind-as-lock signal that a hub is running
/// (`bindHubListener`, `hub.go:881`).
pub fn bind_hub_listener(addr: &str) -> std::io::Result<(Option<std::net::TcpListener>, bool)> {
    match std::net::TcpListener::bind(addr) {
        Ok(l) => Ok((Some(l), false)),
        Err(e) if e.kind() == std::io::ErrorKind::AddrInUse => Ok((None, true)),
        Err(e) => Err(e),
    }
}

// ---------------------------------------------------------------------------
// The identity handshake CLIENT (`queryHubHealth`/`probeHubIdentity`,
// hub.go:916-956) — used by the host-agent's mixed-fleet bind retry (H11) and
// sx's ensure-hub probe (H13). Sync std networking on purpose: both callers
// probe from outside the hub's own runtime (the bind-retry task wraps it in
// spawn_blocking; sx has no runtime at all).
//
// TWO accepted deltas vs Go's `net/http` client, both harmless for the
// loopback identity check this is:
//
//   - NO redirect following (Go's default client follows up to 10). A 3xx from
//     the port holder reads as "not a hub" — the real hub answers 200 inline,
//     and a redirecting squatter is exactly what we want to refuse.
//   - NUMERIC addresses only: `addr` is parsed as a `SocketAddr`, never
//     resolved through DNS. Every caller is loopback-enforced (`HUB_ADDR`, or
//     a `SHED_RC_HUB_ADDR` that [`apply_hub_env_overrides`] has already pinned
//     to 127.0.0.1), so there is no name to resolve.
// ---------------------------------------------------------------------------

/// The head/body byte caps. Go bounds the DECODED body with
/// `io.LimitReader(resp.Body, 4096)`; `net/http` bounds the response head
/// separately (`MaxResponseHeaderBytes`). 8 KiB of head is far more than a
/// health answer needs and keeps a hostile holder from growing our buffer.
const PROBE_HEAD_CAP: usize = 8192;
const PROBE_BODY_CAP: usize = 4096;

/// The PROBE's decode of `/v1/health` — deliberately narrower than the
/// serialized [`HubHealth`] (whose shape is frozen and unchanged): the
/// identity decision reads `app` and nothing else, so `version`/`pid` are not
/// declared at all. serde ignores unknown fields, which reproduces Go's
/// tolerance — `json.Decoder` zero-fills an absent `version`, and its
/// `PID int` accepts a negative pid — without carrying two never-read fields.
/// (It is tolerant of a `pid` that isn't even a number, where Go's decoder
/// would type-error into "not a hub"; no hub emits that, and erring toward
/// "this IS a hub" only ever makes the bind-retry back off politely.)
#[derive(serde::Deserialize)]
struct HubHealthProbe {
    #[serde(default)]
    app: String,
}

/// The remaining slice of an ABSOLUTE probe deadline (`None` once elapsed;
/// zero is folded into `None` because std rejects a zero socket timeout).
///
/// The budget is TOTAL, exactly like Go's `http.Client{Timeout}`, which covers
/// dial + write + the whole body read. Re-arming each read with the full
/// timeout instead would let a slow drip (one byte per timeout-minus-epsilon)
/// hold the probe open indefinitely — and, through the host-agent's bind-retry
/// task, hold up daemon shutdown.
fn remaining(deadline: std::time::Instant) -> Option<Duration> {
    let left = deadline.checked_duration_since(std::time::Instant::now())?;
    (!left.is_zero()).then_some(left)
}

/// ONE identity check against `addr`'s /v1/health (`queryHubHealth`,
/// `hub.go:916`):
///
/// - `Ok(true)`  — a live hub answered with `app == HUB_APP_ID`;
/// - `Ok(false)` — SOMETHING is listening but it is not a hub;
/// - `Err(_)`    — nothing answered at all (connection refused / timeout).
///
/// The raw TCP dial exists only to split "nothing listening" from "listening
/// but not a hub" — a mere successful dial is never treated as a hub (the
/// identity comes from the HTTP handshake, not the port being open).
///
/// `timeout` is the TOTAL budget for the whole exchange (see [`remaining`]);
/// running out of it after something answered is "listening, but not speaking
/// our HTTP" → `Ok(false)`, matching Go, where a `client.Get` timeout takes
/// the same error arm.
pub fn query_hub_health(addr: &str, timeout: Duration) -> std::io::Result<bool> {
    let deadline = std::time::Instant::now() + timeout;
    let sock_addr = addr
        .parse::<std::net::SocketAddr>()
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidInput, e))?;
    let Some(dial_budget) = remaining(deadline) else {
        return Err(std::io::Error::new(
            std::io::ErrorKind::TimedOut,
            "probe budget elapsed",
        ));
    };
    let conn = std::net::TcpStream::connect_timeout(&sock_addr, dial_budget)?;
    drop(conn);

    // The HTTP handshake. Any failure past the dial — connect refused mid-way,
    // a non-HTTP answer, a non-200, a body that isn't the health JSON, or the
    // budget running out — reads as "listening, but not a hub" (Ok(false)),
    // exactly Go's client.Get error arm.
    Ok(probe_health(&sock_addr, addr, deadline).unwrap_or(false))
}

/// The HTTP half of [`query_hub_health`]: `None`/`Some(false)` both mean "not
/// a hub"; every step is bounded by the shared absolute `deadline`.
fn probe_health(
    sock_addr: &std::net::SocketAddr,
    host: &str,
    deadline: std::time::Instant,
) -> Option<bool> {
    use std::io::{Read, Write};
    let mut conn = std::net::TcpStream::connect_timeout(sock_addr, remaining(deadline)?).ok()?;
    conn.set_read_timeout(Some(remaining(deadline)?)).ok()?;
    conn.set_write_timeout(Some(remaining(deadline)?)).ok()?;
    conn.write_all(
        format!("GET /v1/health HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n").as_bytes(),
    )
    .ok()?;

    // Bounded read: the head cap, the body cap, and a little chunked-framing
    // overhead. `Connection: close` makes the hub end the body with EOF.
    let wire_cap = PROBE_HEAD_CAP + PROBE_BODY_CAP + 256;
    let mut buf = Vec::new();
    let mut chunk = [0u8; 2048];
    while buf.len() < wire_cap {
        // Re-arm with what is LEFT of the total budget, never the full timeout.
        let Some(left) = remaining(deadline) else {
            return Some(false); // out of budget: it answered, but not in time
        };
        if conn.set_read_timeout(Some(left)).is_err() {
            break;
        }
        match conn.read(&mut chunk) {
            Ok(0) => break,
            Ok(n) => buf.extend_from_slice(&chunk[..n]),
            Err(_) => break, // timeout / reset: judge what we already have
        }
    }

    let split = find_subslice(&buf, b"\r\n\r\n")?;
    if split > PROBE_HEAD_CAP {
        return Some(false);
    }
    let head = String::from_utf8_lossy(&buf[..split]).into_owned();
    let mut lines = head.split("\r\n");
    let status: u16 = lines.next()?.split_whitespace().nth(1)?.parse().ok()?;
    if status != 200 {
        return Some(false);
    }
    let chunked = lines.any(|line| match line.split_once(':') {
        Some((k, v)) => {
            k.trim().eq_ignore_ascii_case("transfer-encoding")
                && v.split(',')
                    .any(|t| t.trim().eq_ignore_ascii_case("chunked"))
        }
        None => false,
    });
    let raw_body = &buf[split + 4..];
    let body = if chunked {
        dechunk(raw_body, PROBE_BODY_CAP)?
    } else {
        raw_body.to_vec()
    };
    // Go's `io.LimitReader(resp.Body, 4096)`, applied to the DECODED body.
    let body = &body[..body.len().min(PROBE_BODY_CAP)];
    // The FIRST JSON value only: Go's `Decoder.Decode` stops at the end of the
    // first value and never looks at what trails it.
    let hh: HubHealthProbe = serde_json::Deserializer::from_slice(body)
        .into_iter()
        .next()?
        .ok()?;
    Some(hh.app == HUB_APP_ID)
}

fn find_subslice(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

/// Minimal `Transfer-Encoding: chunked` decode (`net/http` does this for Go).
/// Chunk extensions are ignored, a truncated wire yields what arrived, and
/// malformed framing is `None` → "not a hub".
fn dechunk(mut body: &[u8], cap: usize) -> Option<Vec<u8>> {
    let mut out = Vec::new();
    while out.len() < cap {
        let idx = find_subslice(body, b"\r\n")?;
        let size_line = std::str::from_utf8(&body[..idx]).ok()?;
        let n = usize::from_str_radix(size_line.split(';').next()?.trim(), 16).ok()?;
        body = &body[idx + 2..];
        if n == 0 {
            break; // terminal chunk (any trailer is irrelevant here)
        }
        let take = n.min(body.len());
        out.extend_from_slice(&body[..take]);
        if take < n {
            break; // truncated by EOF / the wire cap
        }
        body = &body[n..];
        if body.starts_with(b"\r\n") {
            body = &body[2..];
        }
    }
    Some(out)
}

/// Polls `addr` until a VERIFIED hub answers /v1/health or the budget elapses
/// (`probeHubIdentity`, `hub.go:943`). A foreign listener fails fast with a
/// clear error — it will never become a hub. Each attempt gets Go's 500 ms,
/// clamped to what is left so the CALLER's budget is the hard ceiling.
pub fn probe_hub_identity(addr: &str, budget: Duration) -> Result<(), String> {
    let deadline = std::time::Instant::now() + budget;
    while let Some(left) = remaining(deadline) {
        match query_hub_health(addr, left.min(Duration::from_millis(500))) {
            Ok(true) => return Ok(()),
            Ok(false) => {
                return Err(format!(
                    "port {addr} is held by another process that is not a shed rc hub"
                ));
            }
            Err(_) => std::thread::sleep(Duration::from_millis(20).min(left)),
        }
    }
    Err(format!(
        "rc hub did not come up on {addr} within the probe budget"
    ))
}

// ---------------------------------------------------------------------------
// The sanctioned env seams (plan 010 §2.5 — `clirc.applyHubEnvOverrides`,
// clirc.go:596; the Rust hub honors the SAME variables so the Go↔Rust
// differential harness runs both implementations on distinct ephemeral ports
// with fast ticks). Inert unless set; test-only, not user surface.
// ---------------------------------------------------------------------------

pub const ENV_HUB_ADDR: &str = "SHED_RC_HUB_ADDR";
pub const ENV_HUB_ACTIVE_MS: &str = "SHED_RC_HUB_ACTIVE_MS";
pub const ENV_HUB_IDLE_MS: &str = "SHED_RC_HUB_IDLE_MS";
// A sixth seam set the pane-stability tracker's settle window. It went with that
// tracker in S2 (`charliek/shed#324`) — on BOTH sides, so the differential harness
// has no knob the two hubs could read differently.
pub const ENV_HUB_IDLE_EXIT_MS: &str = "SHED_RC_HUB_IDLE_EXIT_MS";
pub const ENV_HUB_HEARTBEAT_MS: &str = "SHED_RC_HUB_HEARTBEAT_MS";
pub const ENV_HUB_WRITE_TIMEOUT_MS: &str = "SHED_RC_HUB_WRITE_TIMEOUT_MS";

/// Reads the sanctioned env seams into `cfg` (`applyHubEnvOverrides`,
/// clirc.go:596). Every malformed or rejected value is ignored with a `note`
/// (never an error — a bad test seam must not change production behavior
/// beyond the note; the caller prefixes its prog name).
///
/// - `SHED_RC_HUB_ADDR` is LOOPBACK-ENFORCED: any value whose host is not
///   `127.0.0.1` is ignored — a stray environment export must never widen
///   the unauthenticated hub. A CONCRETE port 1–65535 is required (`:0`
///   would break bind-as-lock: port 0 can never EADDRINUSE).
/// - `*_MS` values are positive integer milliseconds; a value that would
///   overflow the duration multiply is rejected too (overflow would fall
///   back to the DEFAULT — the opposite of the override's intent).
pub fn apply_hub_env_overrides(
    cfg: &mut HubConfig,
    getenv: &dyn Fn(&str) -> String,
    note: &mut dyn FnMut(&str),
) {
    let addr = getenv(ENV_HUB_ADDR);
    if !addr.is_empty() {
        // NORMALIZED at acceptance: the stored value is what gets bound and
        // dialed, and neither `TcpListener::bind` nor `SocketAddr::parse`
        // accepts a bracketed IPv4 literal — storing "[127.0.0.1]:80" verbatim
        // would leave the role retrying a bind that can never succeed.
        let normalized = addr.rsplit_once(':').and_then(|(host, port)| {
            // net.SplitHostPort strips a bracketed host ("[127.0.0.1]:80" is
            // accepted by Go — H10 review LOW).
            let host = host
                .strip_prefix('[')
                .and_then(|h| h.strip_suffix(']'))
                .unwrap_or(host);
            let ok =
                host == "127.0.0.1" && port.parse::<u32>().is_ok_and(|p| (1..=65535).contains(&p));
            ok.then(|| format!("{host}:{port}"))
        });
        if let Some(normalized) = normalized {
            cfg.addr = normalized;
        } else {
            note(&format!(
                "ignoring {ENV_HUB_ADDR}={addr:?}: must be 127.0.0.1:<port 1-65535>"
            ));
        }
    }
    // The ceiling guards the duration multiply (clirc.go:637): ~292 years is
    // plenty for "large finite".
    const MAX_MS: i64 = i64::MAX / 1_000_000;
    let mut apply = |env: &str, dst: &mut Duration| {
        let v = getenv(env);
        if v.is_empty() {
            return;
        }
        match v.parse::<i64>() {
            Ok(ms) if ms > 0 && ms <= MAX_MS => *dst = Duration::from_millis(ms as u64),
            _ => note(&format!(
                "ignoring {env}={v:?}: must be a positive integer (milliseconds)"
            )),
        }
    };
    apply(ENV_HUB_ACTIVE_MS, &mut cfg.active_interval);
    apply(ENV_HUB_IDLE_MS, &mut cfg.idle_interval);
    apply(ENV_HUB_IDLE_EXIT_MS, &mut cfg.idle_timeout);
    apply(ENV_HUB_HEARTBEAT_MS, &mut cfg.heartbeat);
    apply(ENV_HUB_WRITE_TIMEOUT_MS, &mut cfg.write_timeout);
}

// ---------------------------------------------------------------------------
// The reconcile-loop thread (§2.3: a dedicated OS thread, the sole writer of
// tracked state; tick cadence via recv_timeout on a signal channel — the Go
// `select` shape of `serveOn`, hub.go:767-799).
// ---------------------------------------------------------------------------

/// A signal into the reconcile loop's channel: an fsnotify nudge reconciles
/// sub-tick; Stop ends the loop.
pub enum LoopSignal {
    Nudge,
    Stop,
}

/// The reconcile-loop half of `serveOn` (`hub.go:767-799`): seed the session
/// list, then tick — fast (activeInterval) while any SSE subscriber is
/// attached, slow (idleInterval) otherwise; a nudge reconciles now instead of
/// next tick. The idle-exit decision is ported for Go parity (§2.4): on the
/// host-agent role the timeout is effectively infinite so it never fires; the
/// respawn handoff is NOT ported (supervised daemon). Runs on the CALLER's
/// thread — spawn it on a dedicated OS thread.
///
/// EVERY exit runs the same cleanup: Go reaches `h.shutdown` (`hub.go:848`)
/// from the signal arm, the server-error arm, AND the idle-exit arm, and it
/// closes the SSE subscribers + the session watchers (opencode SSE clients —
/// the only kind of watcher left since A6, `charliek/shed#322`, retired the
/// codex JSONL tail) before returning. Only the graceful `srv.Shutdown` half is
/// unported — the HTTP server's lifecycle belongs to [`serve`]'s embedder.
pub fn run_reconcile_loop(hub: &Arc<Hub>, signals: &std::sync::mpsc::Receiver<LoopSignal>) {
    hub.reconcile(); // seed the list + fire appear events before the first tick
    loop {
        let interval = if hub.subscriber_count() > 0 {
            hub.cfg.active_interval
        } else {
            hub.cfg.idle_interval
        };
        match signals.recv_timeout(interval) {
            // Stop (the embedder's signal arm) / a dropped sender: shut down.
            Ok(LoopSignal::Stop) | Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                return shutdown(hub);
            }
            Ok(LoopSignal::Nudge) => hub.reconcile(), // surface it now, not next tick
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                hub.reconcile();
                if hub.should_idle_exit((hub.cfg.now)()) {
                    // Idle exit: zero rc sessions for the idle window.
                    // Subscribers do NOT block this — close their streams and
                    // stop. (No respawn handoff — §2.4.)
                    (hub.cfg.logf)(&format!(
                        "rc hub: idle for {:?} with zero rc sessions; exiting",
                        hub.cfg.idle_timeout
                    ));
                    return shutdown(hub);
                }
            }
        }
    }
}

/// `shutdown` (`hub.go:848`) minus the graceful `srv.Shutdown`: releases every
/// SSE subscriber and every session watcher. Idempotent — both `close` halves
/// are.
fn shutdown(hub: &Arc<Hub>) {
    hub.close_all_subscribers();
    hub.close_all_watchers();
}

/// Starts the best-effort fsnotify layer over the file-backed lanes' roots,
/// forwarding each nudge into the reconcile loop's channel (`startFSNudger`,
/// `hub.go:826`). `None` (no roots / fsnotify unavailable) leaves the tick as
/// the sole driver — correctness unchanged, latency only. The forwarder
/// thread (and its watcher) stops when the loop's receiver is dropped.
///
/// DORMANT BY DESIGN, NOT AN OVERSIGHT. The one root this ever had was codex's
/// `~/.codex/sessions`, removed with A6 (`charliek/shed#322`); opencode's SSE
/// stream is its own arrival signal and needs no filesystem wake-up, so the root
/// set is empty, this returns `None`, and [`super::watch::FsNudger`]'s
/// implementation is exercised only by its own tests. It is kept — with its
/// `notify` dependency — because the seam is exactly what the next file-backed
/// lane would need and re-deriving it is real work; it retires with the hub
/// itself in S6 if none arrives first. Delete the two together, not this alone.
pub fn spawn_fs_nudger(
    hub: &Arc<Hub>,
    tx: std::sync::mpsc::Sender<LoopSignal>,
) -> Option<std::thread::JoinHandle<()>> {
    let roots: Vec<String> = Vec::new();
    if roots.is_empty() {
        return None;
    }
    let nudger = match super::watch::FsNudger::new(&roots, Arc::clone(&hub.cfg.logf)) {
        Ok(n) => n,
        Err(e) => {
            (hub.cfg.logf)(&format!(
                "rc hub: fsnotify unavailable ({e}); tick-only activity"
            ));
            return None;
        }
    };
    Some(std::thread::spawn(move || {
        while nudger.nudge().recv().is_ok() {
            if tx.send(LoopSignal::Nudge).is_err() {
                break;
            }
        }
    }))
}

#[cfg(test)]
mod tests {
    use shed_core::rc::RcKind;

    use std::sync::Arc;
    use std::time::Duration;

    use super::super::hub_test_support::rig;
    use super::*;

    // The gated-input unit suite lived here: the per-slug mutex identity/prune
    // cell, the cursor and codex input-gate arms (ready/approval/working/
    // expired-working/stuck-verdict), the three-rejections matrix, the
    // approval-anchor and needs-approval merge arms, and the real-fold
    // acceptance branch. All of it belonged to `input_accepted` and the
    // per-slug delivery lock, which went with the gated lane in A6
    // (`charliek/shed#322`); `POST /input` answers 409 `not_accepting` for
    // every kind now (`hub_http_tests.rs`).

    // The reconcile-loop driver (`serveOn`'s loop half): nudges reconcile
    // sub-tick, Stop ends the loop, and the idle-exit decision closes
    // subscribers + watchers when it fires.
    #[test]
    fn reconcile_loop_nudge_and_stop() {
        let (h, f, _clk) = rig();
        let h = Arc::new(h);
        f.set(
            "rc-loop11",
            "boot >_ OpenAI Codex (v1.0)",
            &super::super::hub_test_support::managed_env("id-l", &RcKind::Codex),
        );
        let (tx, rx) = std::sync::mpsc::channel();
        let lh = Arc::clone(&h);
        let t = std::thread::spawn(move || run_reconcile_loop(&lh, &rx));
        // The seed reconcile tracked the session.
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        while std::time::Instant::now() < deadline {
            if h.lock_track().tracked.contains_key("loop11") {
                break;
            }
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(
            h.lock_track().tracked.contains_key("loop11"),
            "seed tick ran"
        );

        // A nudge folds a change without waiting out the (10s idle) tick.
        f.set_pane("rc-loop11", "boot >_ OpenAI Codex (v1.0)\nchanged");
        tx.send(LoopSignal::Nudge).unwrap();
        tx.send(LoopSignal::Stop).unwrap();
        t.join().unwrap();
    }
}
