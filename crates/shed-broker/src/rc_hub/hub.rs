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
use shed_core::rc::{RcActivity, RcKind, RcSessionDto, RcState};
use shed_core::rc_agents::{approval_anchor_for, composer_under_modal, prompt_anchor_for};
use shed_rc_engine::tmux::Tmux;
use shed_rc_engine::tmux::TmuxRunner;

use super::events::Subscriber;
use super::ingest::PreWatcherQueues;
use super::reconcile::TrackedSession;
use super::watch::{merged_activity, LogFn, SessionWatcher};

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
    pub quiet_period: Duration,
    /// Zero → the 15 m default, mirroring Go's `resolve()`. "Never" is
    /// expressed by the embedder passing a huge value (the host-agent role
    /// does) — the Go seam cannot express "never" either (§2.5).
    pub idle_timeout: Duration,
    pub heartbeat: Duration,
    pub write_timeout: Duration,
    pub subscriber_buffer: usize,
    /// The settle before the Enter keypress in the input handler's delivery
    /// (`sendLineSettle` — a package global in Go, mutated by tests; a config
    /// seam here). `None` → the engine default (750 ms); tests pass
    /// `Some(Duration::ZERO)`.
    pub send_line_settle: Option<Duration>,
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
    pub quiet: Duration,
    pub idle_timeout: Duration,
    pub heartbeat: Duration,
    pub write_timeout: Duration,
    pub sub_buffer: usize,
    pub send_settle: Duration,
}

impl HubConfig {
    /// Applies the defaults (`resolve`, `hub.go:161`). The stability quiet
    /// period's own default lives in the tracker
    /// ([`super::stability::DEFAULT_QUIET_PERIOD`] — a zero `quiet` passes
    /// through and the tracker applies it, same net effect as Go resolving it
    /// here).
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
            quiet: self.quiet_period,
            idle_timeout: dur(self.idle_timeout, DEFAULT_IDLE_TIMEOUT),
            heartbeat: dur(self.heartbeat, DEFAULT_HEARTBEAT),
            write_timeout: dur(self.write_timeout, DEFAULT_WRITE_TIMEOUT),
            sub_buffer: if self.subscriber_buffer == 0 {
                DEFAULT_SUBSCRIBER_BUFFER
            } else {
                self.subscriber_buffer
            },
            send_settle: self
                .send_line_settle
                .unwrap_or(shed_rc_engine::tmux::DEFAULT_SEND_LINE_SETTLE),
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
/// FOUR independent locks, with Go's documented order preserved
/// (`hub.go:243-251`): `track` guards the reconcile state; `subs` is kept
/// separate so broadcast can never deadlock against reconcile; `input_locks`
/// holds the per-SLUG input mutexes (hub-keyed, NOT per tracked entry, so
/// input serialization survives a tracked-entry replacement); `ingest` (inside
/// [`PreWatcherQueues`]) guards the cursor pre-watcher queues. **Lock order:
/// track → ingest → watcher.mu, never reversed** — reconcile is the one path
/// holding track and ingest together (`drainPreWatcher` under the commit
/// lock), and no path takes ingest first. `input_locks` ALSO nests inside
/// track (never the reverse): reconcile's disappearance sweep calls
/// [`Hub::prune_input_lock`] while still holding the track lock, exactly as Go
/// does (`hub_reconcile.go:508`).
pub struct Hub {
    pub(crate) cfg: HubResolved,
    pub(crate) track: Mutex<TrackState>,
    pub(crate) subs: Mutex<Vec<Arc<Subscriber>>>,
    input_locks: Mutex<HashMap<String, Arc<Mutex<()>>>>,
    pub(crate) ingest: PreWatcherQueues,
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
            input_locks: Mutex::new(HashMap::new()),
            ingest: PreWatcherQueues::new(),
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

    /// The slug's input-delivery mutex, created on first use (`inputLock`,
    /// `hub.go:268`). The same slug always yields the same mutex until the
    /// session disappears (pruned), so input serialization survives a
    /// tracked-entry replacement (kill+recreate keeps the slug present →
    /// keeps the lock).
    pub(crate) fn input_lock(&self, slug: &str) -> Arc<Mutex<()>> {
        let mut locks = self
            .input_locks
            .lock()
            .unwrap_or_else(PoisonError::into_inner);
        Arc::clone(locks.entry(slug.to_string()).or_default())
    }

    /// Drops a disappeared slug's input mutex (`pruneInputLock`,
    /// `hub.go:282`). A request still holding the old mutex finishes against a
    /// gone pane (its delivery 404s); a later recreate at the same slug gets a
    /// fresh lock.
    pub(crate) fn prune_input_lock(&self, slug: &str) {
        self.input_locks
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(slug);
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

    /// Re-derives, from a FRESH pane capture, whether the session is waiting
    /// for typed input (`inputAccepted`, `hub.go:657`) — running the SAME
    /// watcher+stability merge the reconcile loop uses, so the handler can
    /// never be more permissive than the hub's own displayed activity. The
    /// SEVEN arms, in precedence order (each rationale in full at
    /// `hub.go:600-656`):
    ///
    /// 1. merged working OR needs_approval → reject (mid-turn delivery /
    ///    dialog owns the keyboard — the lane-derived kinds' approval arm).
    /// 2. the watcher reports ANY open approval → reject, DELIBERATELY
    ///    ignoring transport health/freshness: the merge demotes an unhealthy
    ///    watcher to pane stability, which would re-open exactly the hole arm
    ///    1 closes. A stale reject costs a retry; a stale accept costs an
    ///    approval nobody meant to give. Also catches opencode QUESTIONS.
    /// 3. the kind's ApprovalAnchor visible on the FRESH visible pane →
    ///    reject (pane-derived kinds — codex, cursor). UNDEBOUNCED, unlike
    ///    reconcile's derivation: one frame showing a dialog is enough to
    ///    refuse a keystroke.
    /// 4. expired-working + ComposerUnderModal (cursor) → GUARDED recovery:
    ///    accept ONLY when the fresh visible pane shows no known approval
    ///    surface AND stability has independently SETTLED on needs_input.
    ///    cursor's `stop` hook fires reliably only on turn 1, so a blanket
    ///    reject would make phone steering work once. codex is unaffected
    ///    (ComposerUnderModal=false — its overlay replaces the composer).
    /// 5. fresh watcher needs_input → accept outright (the structured signal
    ///    is settled and authoritative).
    /// 6. /7. anything else → the degraded-path policy: accept only if the
    ///    kind's prompt anchor is visible on the FRESH pane (closing the
    ///    lookup→lock race — a pane that flipped back to churning no longer
    ///    shows the composer).
    pub(crate) fn input_accepted(
        &self,
        watcher: Option<&dyn SessionWatcher>,
        stability: RcActivity,
        kind: &RcKind,
        pane: &str,
        visible_pane: &str,
    ) -> bool {
        let mut watcher_act = RcActivity::Unknown;
        let (mut watcher_fresh, mut expired_working) = (false, false);
        if let Some(w) = watcher {
            w.refresh((self.cfg.now)());
            // The message is discarded, as in Go (`_`) — the gate consults
            // activity only.
            let (a, _msg, fresh, expired) = w.snapshot((self.cfg.now)());
            (watcher_act, watcher_fresh, expired_working) = (a, fresh, expired);
        }
        let (merged, _) =
            merged_activity(watcher_act, "", watcher_fresh, expired_working, stability);
        if merged == RcActivity::Working || merged == RcActivity::NeedsApproval {
            return false;
        }
        if watcher
            .and_then(SessionWatcher::as_approval_blocker)
            .is_some_and(|b| b.has_open_approvals())
        {
            return false;
        }
        if approval_anchor_for(kind).is_some_and(|a| a.is_match(visible_pane)) {
            return false;
        }
        if expired_working && composer_under_modal(kind) {
            // Guarded recovery (arm 4). The anchor arm above already rejected
            // on a matching visible pane; re-checking it here keeps this arm
            // self-evidently safe under any future re-ordering (`hub.go:676`).
            let modal_on_screen =
                approval_anchor_for(kind).is_some_and(|a| a.is_match(visible_pane));
            return !modal_on_screen && stability == RcActivity::NeedsInput;
        }
        if watcher_fresh && watcher_act == RcActivity::NeedsInput {
            return true;
        }
        prompt_anchor_for(kind).is_some_and(|a| a.is_match(pane))
    }
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

#[derive(serde::Serialize)]
struct Delivered {
    delivered: bool,
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

/// `handleInput` (`hub.go:455`): validate + re-derive live state under the
/// per-session mutex, then deliver through the bracketed-paste path. 400
/// invalid/unsafe text, 404 unknown/gone slug, 409 not accepting (wrong
/// activity, recreated identity, or a non-input-gated kind), 413 too large.
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

    // Look up the tracked session and snapshot the identity the re-check pins
    // against (under the track lock — reconcile mutates tracked under it).
    let (want_id, want_created_at) = {
        let ts = hub.lock_track();
        match ts.tracked.get(&slug) {
            Some(tr) => (tr.id.clone(), tr.created_at.clone()),
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

    // The rest is sync tmux work under the per-slug input mutex — off the
    // async runtime (§2.3: the input gate's fresh pane captures run under
    // spawn_blocking).
    let deliver_hub = Arc::clone(&hub);
    tokio::task::spawn_blocking(move || {
        deliver_hub.deliver_input(&slug, &text, &want_id, &want_created_at)
    })
    .await
    .unwrap_or_else(|_| {
        write_error(
            http::StatusCode::INTERNAL_SERVER_ERROR,
            "delivery_failed",
            "input delivery failed",
        )
    })
}

impl Hub {
    /// The locked half of `handleInput` (`hub.go:489-597`): the per-SLUG
    /// mutex (hub-keyed, not on the tracked entry) makes the acceptance
    /// re-check + delivery one critical section — two concurrent posts can
    /// never interleave keystrokes into one pane, even across a tracked-entry
    /// replacement.
    fn deliver_input(
        &self,
        slug: &str,
        text: &str,
        want_id: &str,
        want_created_at: &str,
    ) -> axum::response::Response {
        use shed_rc_engine::ops::{
            capture_pane_checked, capture_visible_pane_checked, EngineError,
        };

        let mu = self.input_lock(slug);
        let _guard = mu.lock().unwrap_or_else(PoisonError::into_inner);

        let name = shed_core::rc::tmux_name(slug);
        let tmux = Tmux::new(&*self.cfg.runner).with_settle(self.cfg.send_settle);
        // Maps a checked-capture failure: the session vanishing between the
        // lookup and this re-capture is a 404; a transient tmux failure is a
        // server error so the client retries rather than dropping the session.
        fn capture_err(e: &EngineError) -> axum::response::Response {
            if matches!(e, EngineError::SessionNotFound(_)) {
                return write_error(
                    http::StatusCode::NOT_FOUND,
                    "unknown_slug",
                    "rc session is gone",
                );
            }
            write_error(
                http::StatusCode::INTERNAL_SERVER_ERROR,
                "capture_failed",
                "pane re-capture failed",
            )
        }
        let pane = match capture_pane_checked(&tmux, &name) {
            Ok(p) => p,
            Err(e) => return capture_err(&e),
        };
        let fresh =
            shed_core::rc_agents::parse_session(&name, &tmux.show_environment(&name), &pane, None);

        // Identity guard: the slug must still be the same incarnation.
        if fresh.id.as_deref().unwrap_or("") != want_id
            || fresh.created_at.as_deref().unwrap_or("") != want_created_at
        {
            return write_error(
                http::StatusCode::CONFLICT,
                super::verbs::ERR_NOT_ACCEPTING,
                "session was recreated",
            );
        }
        // The gated feed-input surface is DERIVED from the kind's advertised
        // row: kind_features.input is single-valued, so a kind that graduates
        // to a whole-turn lane stops accepting /input in the same edit that
        // flips its row.
        if super::verbs::kind_feature_row(&fresh.kind).input != super::verbs::INPUT_MODE_GATED {
            return write_error(
                http::StatusCode::CONFLICT,
                super::verbs::ERR_NOT_ACCEPTING,
                "this kind does not accept feed input",
            );
        }
        // A blocking lifecycle state suppresses the activity dimension
        // entirely — nothing is accepting typed input.
        if display_activity(fresh.state, RcActivity::Working).is_none() {
            return write_error(
                http::StatusCode::CONFLICT,
                super::verbs::ERR_NOT_ACCEPTING,
                "session is not in an input-accepting state",
            );
        }

        // Re-read the CURRENT watcher + stability under the track lock (they
        // may have been replaced since the pre-lock lookup; identity was just
        // re-verified above).
        let (watcher, stability) = {
            let ts = self.lock_track();
            match ts.tracked.get(slug) {
                Some(tr) => (tr.watcher.clone(), tr.last_stability),
                None => (None, RcActivity::Unknown),
            }
        };

        // Acceptance→delivery gap: re-capture HERE, as late as possible, and
        // run the acceptance merge on THAT fresh pane. Residual (accepted):
        // the few calls between this capture and send_line remain un-gated —
        // tmux offers no atomic capture-and-send.
        let deliver_pane = match capture_pane_checked(&tmux, &name) {
            Ok(p) => p,
            Err(e) => return capture_err(&e),
        };
        // The ApprovalAnchor arm needs the VISIBLE frame, not scrollback: an
        // answered dialog stays in the history verbatim, and gating on that
        // would wedge the session's input permanently. Captured only for
        // anchor-declaring kinds; a capture failure is fail-CLOSED.
        let mut visible_pane = String::new();
        if approval_anchor_for(&fresh.kind).is_some() {
            visible_pane = match capture_visible_pane_checked(&tmux, &name) {
                Ok(p) => p,
                Err(e) => return capture_err(&e),
            };
        }
        if !self.input_accepted(
            watcher.as_deref().map(|w| w as &dyn SessionWatcher),
            stability,
            &fresh.kind,
            &deliver_pane,
            &visible_pane,
        ) {
            return write_error(
                http::StatusCode::CONFLICT,
                super::verbs::ERR_NOT_ACCEPTING,
                "session is not waiting for input",
            );
        }

        // Deliver via the shared bracketed-paste path (single line →
        // send-keys -l + Enter; multi-line → set-buffer + paste-buffer +
        // Enter).
        let res = tmux.send_line(&name, text);
        if res.code != 0 {
            if shed_rc_engine::tmux::is_missing_session(&res.stderr) {
                return write_error(
                    http::StatusCode::NOT_FOUND,
                    "unknown_slug",
                    "rc session is gone",
                );
            }
            return write_error(
                http::StatusCode::INTERNAL_SERVER_ERROR,
                "delivery_failed",
                "input delivery failed",
            );
        }
        write_json(http::StatusCode::OK, &Delivered { delivered: true })
    }

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
            // The cursor hook ingest route: called by a process INSIDE the
            // shed (the preseeded hook script), never by the server proxy —
            // which deliberately does not allowlist it.
            .route(
                "/v1/ingest/cursor",
                post(super::ingest::handle_ingest_cursor),
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
// The sanctioned env seams (plan 010 §2.5 — `clirc.applyHubEnvOverrides`,
// clirc.go:596; the Rust hub honors the SAME variables so the Go↔Rust
// differential harness runs both implementations on distinct ephemeral ports
// with fast ticks). Inert unless set; test-only, not user surface.
// ---------------------------------------------------------------------------

pub const ENV_HUB_ADDR: &str = "SHED_RC_HUB_ADDR";
pub const ENV_HUB_ACTIVE_MS: &str = "SHED_RC_HUB_ACTIVE_MS";
pub const ENV_HUB_IDLE_MS: &str = "SHED_RC_HUB_IDLE_MS";
pub const ENV_HUB_QUIET_MS: &str = "SHED_RC_HUB_QUIET_MS";
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
        let valid = addr.rsplit_once(':').is_some_and(|(host, port)| {
            // net.SplitHostPort strips a bracketed host ("[127.0.0.1]:80" is
            // accepted by Go — H10 review LOW).
            let host = host
                .strip_prefix('[')
                .and_then(|h| h.strip_suffix(']'))
                .unwrap_or(host);
            host == "127.0.0.1" && port.parse::<u32>().is_ok_and(|p| (1..=65535).contains(&p))
        });
        if valid {
            cfg.addr = addr;
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
    apply(ENV_HUB_QUIET_MS, &mut cfg.quiet_period);
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
/// closes the SSE subscribers + the session watchers (JSONL tails, opencode
/// SSE clients) before returning. Only the graceful `srv.Shutdown` half is
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

/// Starts the best-effort fsnotify layer over the codex + claude JSONL roots,
/// forwarding each nudge into the reconcile loop's channel (`startFSNudger`,
/// `hub.go:826`). `None` (no roots / fsnotify unavailable) leaves the tick as
/// the sole driver — correctness unchanged, latency only. The forwarder
/// thread (and its watcher) stops when the loop's receiver is dropped.
pub fn spawn_fs_nudger(
    hub: &Arc<Hub>,
    tx: std::sync::mpsc::Sender<LoopSignal>,
) -> Option<std::thread::JoinHandle<()>> {
    let getenv: &dyn Fn(&str) -> String = &*hub.cfg.getenv;
    let mut roots = Vec::new();
    for r in [
        super::watch_codex::codex_sessions_root(getenv),
        super::watch_claude::claude_projects_root(getenv),
    ] {
        if !r.is_empty() {
            roots.push(r);
        }
    }
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
    use std::sync::Arc;
    use std::time::Duration;

    use super::super::hub_test_support::{
        codex_ready_pane, hook_ev, pane_fixture, rig, test_hub, StubApprovalWatcher, StubWatcher,
        CURSOR_SID,
    };
    use super::super::watch::{CursorIngester, WATCHER_WORKING_GRACE};
    use super::super::watch_cursor::CursorWatcher;
    use super::*;

    fn settled_stub() -> StubWatcher {
        StubWatcher {
            activity: RcActivity::NeedsInput,
            fresh: true,
            ..StubWatcher::default()
        }
    }

    /// `expiredCursorWatcher` (`cursor_approval_test.go:323`): an
    /// expired-working verdict — the state a stuck cursor fold is in.
    fn expired_stub() -> StubWatcher {
        StubWatcher {
            activity: RcActivity::Working,
            expired_working: true,
            ..StubWatcher::default()
        }
    }

    // The per-slug input mutex is HUB-keyed: the same slug yields the same
    // mutex until pruned, so input serialization survives a tracked-entry
    // replacement (the unit half of TestHubInputLockSurvivesEntryReplacement
    // — the HTTP half is `input_lock_survives_entry_replacement`).
    #[test]
    fn input_lock_survives_replacement_and_prunes() {
        let h = test_hub();
        let l1 = h.input_lock("abc123");
        let l2 = h.input_lock("abc123");
        assert!(Arc::ptr_eq(&l1, &l2), "same slug → same mutex");
        h.prune_input_lock("abc123");
        let l3 = h.input_lock("abc123");
        assert!(!Arc::ptr_eq(&l1, &l3), "a pruned slug gets a fresh lock");
    }

    // Mirrors TestCursorInputGateAcceptsReadyRejectsApproval
    // (cursor_approval_test.go:159).
    #[test]
    fn cursor_input_gate_accepts_ready_rejects_approval() {
        let h = test_hub();
        let settled = settled_stub();
        let ready = pane_fixture("cursor-ready");
        let approval = pane_fixture("cursor-ready-approval-shell");

        assert!(
            h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a ready cursor composer must accept feed input"
        );
        assert!(
            !h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &approval,
                &approval
            ),
            "an approval prompt on the visible frame must reject input"
        );
        // The approval fixture still matches the READY/prompt anchor (cursor
        // keeps its composer drawn, disabled, under the decision surface) —
        // exactly why the approval arm has to exist.
        assert!(
            prompt_anchor_for(&RcKind::Cursor)
                .expect("cursor prompt anchor")
                .is_match(&approval),
            "premise: the approval fixture still shows the composer anchor"
        );
        // Scrollback is not evidence about the present.
        assert!(
            h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &approval,
                &ready
            ),
            "an approval prompt only in the scrollback must not gate input"
        );
        // Post-resolution and quoted prose both flow again.
        for fx in [
            "cursor-ready-approval-resolved",
            "cursor-ready-approval-quoted",
        ] {
            let pane = pane_fixture(fx);
            assert!(
                h.input_accepted(
                    Some(&settled),
                    RcActivity::NeedsInput,
                    &RcKind::Cursor,
                    &pane,
                    &pane
                ),
                "{fx}: input must be accepted"
            );
        }
    }

    // Mirrors TestCursorInputGateRejectsExpiredWorkingUnderAnUnknownModal
    // (cursor_approval_test.go:235).
    #[test]
    fn cursor_input_gate_rejects_expired_working_under_unknown_modal() {
        let (h, _, clk) = rig();

        // A cursor turn in flight, then the operator walks away: no `stop`
        // ever arrives, so the verdict expires.
        let w = CursorWatcher::new("", None);
        w.push_hook_event(hook_ev(
            "preToolUse",
            &format!(
                r#"{{"session_id":"{CURSOR_SID}","tool_name":"Delete","tool_input":{{"file_path":"/home/shed/proj/build.json"}}}}"#
            ),
        ));
        w.refresh(clk.now());
        clk.advance(WATCHER_WORKING_GRACE + Duration::from_secs(1));
        let (_, _, fresh, expired) = w.snapshot(clk.now());
        assert!(
            !fresh && expired,
            "premise: the verdict must be expired-working"
        );

        // A widget the anchor does not know, cursor's composer still drawn
        // beneath it, stability idle — NOT the settled needs_input the
        // recovery requires, so the arm rejects.
        let unknown_modal =
            pane_fixture("cursor-ready") + "\n Some future approval widget?\n   → Yes, do it (y)\n";
        assert!(
            !approval_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&unknown_modal),
            "premise: must NOT match the approval anchor"
        );
        assert!(
            prompt_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&unknown_modal),
            "premise: the composer must still be visible under the widget"
        );
        assert!(
            !h.input_accepted(
                Some(&w),
                RcActivity::Idle,
                &RcKind::Cursor,
                &unknown_modal,
                &unknown_modal
            ),
            "expired-working cursor with a non-needs_input stability must reject"
        );

        // The legitimate case is unaffected: when `stop` DOES fire it settles
        // the fold, and a settled verdict accepts however long it's been quiet.
        w.push_hook_event(hook_ev(
            "stop",
            &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
        ));
        w.refresh(clk.now());
        clk.advance(Duration::from_secs(24 * 3600));
        let ready = pane_fixture("cursor-ready");
        assert!(
            h.input_accepted(Some(&w), RcActivity::Idle, &RcKind::Cursor, &ready, &ready),
            "a settled cursor verdict must still accept input"
        );

        // And a cursor session with NO watcher verdict at all keeps the
        // degraded anchor path.
        assert!(
            h.input_accepted(None, RcActivity::Idle, &RcKind::Cursor, &ready, &ready),
            "with no watcher verdict the composer anchor must still accept"
        );
    }

    // Mirrors TestCursorInputGateRejectsWhileWorking
    // (cursor_approval_test.go:292).
    #[test]
    fn cursor_input_gate_rejects_while_working() {
        let (h, _, clk) = rig();

        let w = CursorWatcher::new("", None);
        w.push_hook_event(hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"go"}}"#),
        ));
        w.refresh(clk.now());
        let ready = pane_fixture("cursor-ready");
        assert!(
            !h.input_accepted(
                Some(&w),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a working hook verdict must reject input even at the composer"
        );

        w.push_hook_event(hook_ev(
            "stop",
            &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
        ));
        w.refresh(clk.now());
        assert!(
            h.input_accepted(Some(&w), RcActivity::Idle, &RcKind::Cursor, &ready, &ready),
            "a settled hook verdict must accept input"
        );
    }

    // Mirrors TestCursorInputGateExpiredWorkingRealDialogRejected — THE
    // F1-SAFETY PIN (cursor_approval_test.go:333): stability=needs_input is
    // the worst case (the composer under the dialog settles, so recovery
    // condition (b) holds) — only the exhaustive anchor stands between a
    // posted line and the widget.
    #[test]
    fn cursor_input_gate_expired_working_real_dialog_rejected() {
        let h = test_hub();
        assert!(
            approval_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&pane_fixture("cursor-ready-approval-shell")),
            "premise: the approval fixture matches the anchor on the visible frame"
        );
        for fx in [
            "cursor-ready-approval-shell",
            "cursor-ready-approval-delete",
            "cursor-ready-approval-write",
        ] {
            let dialog = pane_fixture(fx);
            assert!(
                !h.input_accepted(
                    Some(&expired_stub()),
                    RcActivity::NeedsInput,
                    &RcKind::Cursor,
                    &dialog,
                    &dialog
                ),
                "{fx}: an expired-working cursor with a real dialog on the visible frame must REJECT"
            );
        }
    }

    // Mirrors TestCursorInputGateExpiredWorkingIdleComposerRecovers — THE
    // RECOVERY (cursor_approval_test.go:356).
    #[test]
    fn cursor_input_gate_expired_working_idle_composer_recovers() {
        let h = test_hub();
        let ready = pane_fixture("cursor-ready");
        assert!(
            !approval_anchor_for(&RcKind::Cursor)
                .expect("anchor")
                .is_match(&ready),
            "premise: the clean composer must NOT match the ApprovalAnchor"
        );
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "expired-working + clean idle composer + settled needs_input must ACCEPT"
        );
        // A dialog answered long ago sits in the 200-line history (`pane`)
        // while the visible frame is clean — still a recovery.
        let stale = pane_fixture("cursor-ready-approval-shell");
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &stale,
                &ready
            ),
            "a dialog only in scrollback must not block the recovery"
        );
    }

    // Mirrors TestCursorInputGateExpiredWorkingUnsettledStabilityRejected —
    // NO FALSE ACCEPT MID-WORK (cursor_approval_test.go:380).
    #[test]
    fn cursor_input_gate_expired_working_unsettled_stability_rejected() {
        let h = test_hub();
        let ready = pane_fixture("cursor-ready");
        // Still churning: stability=working ⇒ merged stays working ⇒ reject.
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::Working,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a still-churning (working) stability must REJECT"
        );
        // Settled but idle: passes the merge but fails recovery condition (b).
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::Idle,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a settled-idle (not needs_input) stability must REJECT"
        );
    }

    // Mirrors TestCursorInputGateFreshFirstTurnAccepts
    // (cursor_approval_test.go:400).
    #[test]
    fn cursor_input_gate_fresh_first_turn_accepts() {
        let (h, _, clk) = rig();

        let w = CursorWatcher::new("", None);
        w.push_hook_event(hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"go"}}"#),
        ));
        w.refresh(clk.now());
        w.push_hook_event(hook_ev(
            "stop",
            &format!(r#"{{"session_id":"{CURSOR_SID}","status":"completed"}}"#),
        ));
        w.refresh(clk.now());
        let (_, _, fresh, expired) = w.snapshot(clk.now());
        assert!(
            fresh && !expired,
            "premise: a just-settled fold must be fresh, not expired"
        );
        let ready = pane_fixture("cursor-ready");
        assert!(
            h.input_accepted(
                Some(&w),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &ready,
                &ready
            ),
            "a fresh settled cursor fold (turn 1) must accept /input"
        );
    }

    // Mirrors TestCursorInputGateCodexExpiredWorkingUnchanged
    // (cursor_approval_test.go:424): the guarded arm is gated on
    // ComposerUnderModal, FALSE for codex.
    #[test]
    fn cursor_input_gate_codex_expired_working_unchanged() {
        let h = test_hub();
        assert!(
            !composer_under_modal(&RcKind::Codex),
            "premise: codex must NOT declare ComposerUnderModal"
        );
        let ready = codex_ready_pane();
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &ready,
                &ready
            ),
            "codex expired-working + settled needs_input + clean composer must accept"
        );
        let dialog = pane_fixture("codex-ready-approval-exec");
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &dialog,
                &dialog
            ),
            "codex expired-working + a dialog on the visible frame must reject via codex's own anchor"
        );
    }

    // The open-approval blocker arm DELIBERATELY ignores transport
    // health/freshness (the unit half of
    // TestHubInputOpenApprovalRejectsWhenTransportUnhealthy): an UNFRESH
    // watcher's merge falls to stability (needs_input — would accept), but an
    // open ask still owns the keyboard. Also covers opencode QUESTIONS
    // (blocked without a snapshot entry).
    #[test]
    fn input_gate_open_approval_rejects_when_transport_unhealthy() {
        let h = test_hub();
        let pane = "opencode\n> Ask anything...";
        let blocked = StubApprovalWatcher {
            stub: StubWatcher {
                activity: RcActivity::Idle,
                ..StubWatcher::default()
            },
            blocked: true,
            ..StubApprovalWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&blocked),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                pane,
                ""
            ),
            "an open approval must reject even with the watcher unfresh"
        );
        let clear = StubApprovalWatcher {
            stub: StubWatcher {
                activity: RcActivity::Idle,
                ..StubWatcher::default()
            },
            ..StubApprovalWatcher::default()
        };
        assert!(
            h.input_accepted(
                Some(&clear),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                pane,
                ""
            ),
            "no open approvals + settled stability at the composer must accept"
        );
    }

    // The SEVEN-ARM table, pinned by ARM IDENTITY (plan 010 AC2): each row
    // names the arm that decides it, with inputs that make every EARLIER arm
    // pass through — so a precedence reorder fails the table even where the
    // outcome would coincide.
    #[test]
    fn input_accepted_seven_arm_table() {
        let h = test_hub();
        let cursor_ready = pane_fixture("cursor-ready");
        let cursor_dialog = pane_fixture("cursor-ready-approval-shell");
        let codex_ready = codex_ready_pane();
        let codex_dialog = pane_fixture("codex-ready-approval-exec");

        // ARM 1a (merged working): fresh working watcher; pane at the
        // composer (later arms would accept).
        let fresh_working = StubWatcher {
            activity: RcActivity::Working,
            fresh: true,
            ..StubWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&fresh_working),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &codex_ready,
                &codex_ready
            ),
            "arm 1a: merged working rejects"
        );

        // ARM 1b (merged needs_approval): the lane-derived approval arm.
        let fresh_approval = StubWatcher {
            activity: RcActivity::NeedsApproval,
            fresh: true,
            ..StubWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&fresh_approval),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                "opencode\n> Ask anything...",
                ""
            ),
            "arm 1b: merged needs_approval rejects"
        );

        // ARM 2 (open-approval blocker): merged is idle (arm 1 passes), no
        // anchor on the pane (arm 3 passes) — the blocker alone rejects.
        let blocked = StubApprovalWatcher {
            stub: StubWatcher {
                activity: RcActivity::Idle,
                fresh: true,
                ..StubWatcher::default()
            },
            blocked: true,
            ..StubApprovalWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&blocked),
                RcActivity::Idle,
                &RcKind::Opencode,
                "opencode\n> Ask anything...",
                ""
            ),
            "arm 2: an open approval rejects regardless of the merge"
        );

        // ARM 3 (approval anchor on the visible frame): fresh needs_input
        // watcher (arm 5 would accept) — the anchor rejects first.
        assert!(
            !h.input_accepted(
                Some(&settled_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &cursor_dialog,
                &cursor_dialog
            ),
            "arm 3: a visible approval anchor rejects even a fresh needs_input"
        );

        // ARM 4 (guarded recovery): expired-working + ComposerUnderModal;
        // accepts ONLY with a clean visible pane AND settled needs_input.
        assert!(
            h.input_accepted(
                Some(&expired_stub()),
                RcActivity::NeedsInput,
                &RcKind::Cursor,
                &cursor_ready,
                &cursor_ready
            ),
            "arm 4: recovery accepts"
        );
        assert!(
            !h.input_accepted(
                Some(&expired_stub()),
                RcActivity::Idle,
                &RcKind::Cursor,
                &cursor_ready,
                &cursor_ready
            ),
            "arm 4: unsettled stability rejects INSIDE the arm (never falls through to the anchor path)"
        );

        // ARM 5 (fresh needs_input accepts outright): the pane deliberately
        // does NOT match the prompt anchor — only arm 5 can accept it.
        assert!(
            h.input_accepted(
                Some(&settled_stub()),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                "no anchor here",
                ""
            ),
            "arm 5: a fresh needs_input accepts without the anchor"
        );

        // ARM 6 (degraded path, anchor visible): no watcher, stale stability.
        assert!(
            h.input_accepted(None, RcActivity::Idle, &RcKind::Codex, &codex_ready, ""),
            "arm 6: the fresh prompt anchor accepts on the degraded path"
        );

        // ARM 7 (degraded path, no anchor): everything else rejects.
        assert!(
            !h.input_accepted(
                None,
                RcActivity::Idle,
                &RcKind::Codex,
                "churning output",
                ""
            ),
            "arm 7: no anchor on the fresh pane rejects"
        );
        // ...including a codex dialog pane: its overlay REPLACES the composer,
        // so the prompt anchor cannot match (the arm-3 anchor also fires, but
        // arm 7 is what this row pins — visiblePane is "" here).
        assert!(
            !h.input_accepted(None, RcActivity::Idle, &RcKind::Codex, &codex_dialog, ""),
            "arm 7: a codex dialog pane cannot match its prompt anchor"
        );
    }

    // Mirrors TestHubInputNeedsApprovalRejected (`hub_input_test.go:339`):
    // the gate is driven by the MERGE — a fresh needs_approval rejects, a
    // stale one yields to the settled stability verdict.
    #[test]
    fn input_needs_approval_rejected_via_merge() {
        let h = test_hub();
        let ready = "opencode\n> Ask anything...";
        let blocked = StubWatcher {
            activity: RcActivity::NeedsApproval,
            fresh: true,
            ..StubWatcher::default()
        };
        assert!(
            !h.input_accepted(
                Some(&blocked),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                ready,
                ready
            ),
            "a fresh needs_approval watcher must reject even on an anchored pane"
        );
        let stale = StubWatcher {
            activity: RcActivity::NeedsApproval,
            ..StubWatcher::default()
        };
        assert!(
            h.input_accepted(
                Some(&stale),
                RcActivity::NeedsInput,
                &RcKind::Opencode,
                ready,
                ready
            ),
            "a stale needs_approval watcher must yield to the settled stability verdict"
        );
    }

    // Mirrors TestHubInputApprovalAnchorRejected (`hub_input_test.go:396`):
    // codex's real anchor against the committed fixtures, with a FRESH
    // settled watcher (the verdict that otherwise short-circuits to accept).
    #[test]
    fn input_approval_anchor_rejected_codex_fixtures() {
        let h = test_hub();
        assert!(approval_anchor_for(&RcKind::Codex).is_some());
        let settled = settled_stub();
        for fx in ["codex-ready-approval-exec", "codex-ready-approval-network"] {
            let pane = pane_fixture(fx);
            assert!(
                !h.input_accepted(
                    Some(&settled),
                    RcActivity::NeedsInput,
                    &RcKind::Codex,
                    &pane,
                    &pane
                ),
                "{fx}: an approval dialog on the fresh pane must reject input"
            );
        }
        for fx in [
            "codex-ready-approval-resolved",
            "codex-ready-approval-quoted",
        ] {
            let pane = pane_fixture(fx);
            assert!(
                h.input_accepted(
                    Some(&settled),
                    RcActivity::NeedsInput,
                    &RcKind::Codex,
                    &pane,
                    &pane
                ),
                "{fx}: input must flow again"
            );
        }
        let ready = codex_ready_pane();
        assert!(h.input_accepted(
            Some(&settled),
            RcActivity::NeedsInput,
            &RcKind::Codex,
            &ready,
            &ready
        ));
        // The arm reads the VISIBLE frame, never the scrollback.
        assert!(
            h.input_accepted(
                Some(&settled),
                RcActivity::NeedsInput,
                &RcKind::Codex,
                &pane_fixture("codex-ready-approval-exec"),
                &ready
            ),
            "a dialog present ONLY in the scrollback must not gate input"
        );
    }

    // Mirrors TestHubInputAcceptedWatcherBranch (`hub_input_test.go:434`): a
    // FRESH JSONL verdict (a real FileWatcher over a real codex fold) wins
    // over the pane anchor in both directions.
    #[test]
    fn input_accepted_watcher_branch_with_real_fold() {
        use super::super::watch::FileWatcher;
        use super::super::watch_codex::CodexFold;
        let (h, _f, clk) = rig();
        let dir = std::env::temp_dir().join(format!("rc-hub-gate-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();

        // A settled turn → needs_input, authoritative even with no anchor.
        let settled = dir.join("settled.jsonl");
        std::fs::write(
            &settled,
            "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"last_agent_message\":\"done\"}}\n",
        )
        .unwrap();
        let w = FileWatcher::new(settled.to_str().unwrap(), true, Box::new(CodexFold::new()));
        w.refresh(clk.now());
        assert!(
            h.input_accepted(
                Some(&w),
                RcActivity::Working,
                &RcKind::Codex,
                "no anchor here",
                "no anchor here"
            ),
            "a fresh needs_input watcher must accept regardless of the pane anchor"
        );

        // An open tool call → working; rejects even at the composer anchor.
        let working = dir.join("working.jsonl");
        std::fs::write(
            &working,
            "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"call_id\":\"c1\",\"name\":\"exec\"}}\n",
        )
        .unwrap();
        let ready = codex_ready_pane();
        let w2 = FileWatcher::new(working.to_str().unwrap(), true, Box::new(CodexFold::new()));
        w2.refresh(clk.now());
        assert!(
            !h.input_accepted(
                Some(&w2),
                RcActivity::Working,
                &RcKind::Codex,
                &ready,
                &ready
            ),
            "a fresh working watcher must reject even with the composer anchor present"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    // Mirrors TestHubInputLongQuietWorkingRejected (`hub_input_test.go:468`):
    // an EXPIRED-working verdict with unsettled stability still merges to
    // working (a >120s tool call is a live turn); only a SETTLED quiet
    // stability releases it to the anchor path.
    #[test]
    fn input_long_quiet_working_rejected() {
        use super::super::watch::FileWatcher;
        use super::super::watch_codex::CodexFold;
        let (h, _f, clk) = rig();
        let dir = std::env::temp_dir().join(format!("rc-hub-lqw-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let long = dir.join("long.jsonl");
        std::fs::write(
            &long,
            "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"custom_tool_call\",\"call_id\":\"c1\",\"name\":\"exec\"}}\n",
        )
        .unwrap();
        let w = FileWatcher::new(long.to_str().unwrap(), true, Box::new(CodexFold::new()));
        w.refresh(clk.now());
        clk.advance(WATCHER_WORKING_GRACE + Duration::from_secs(1));

        let ready = codex_ready_pane();
        assert!(
            !h.input_accepted(
                Some(&w),
                RcActivity::Working,
                &RcKind::Codex,
                &ready,
                &ready
            ),
            "expired-working with unsettled stability must reject (still mid-turn)"
        );
        assert!(
            h.input_accepted(Some(&w), RcActivity::Idle, &RcKind::Codex, &ready, &ready),
            "expired-working with settled-idle stability + anchor should accept"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

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
