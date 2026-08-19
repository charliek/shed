//! Contract-v2 hub verbs — `internal/ext/rc/hub_verbs.go`:
//!
//! ```text
//! POST /v1/sessions/{slug}/turn            {"text": string, "options": object?}
//! POST /v1/sessions/{slug}/interrupt       (body ignored)
//! POST /v1/sessions/{slug}/approvals/{id}  {"decision": "allow"|"allow_always"|"deny"}
//! ```
//!
//! The OPENCODE lane implements all three; every other kind validates fully
//! and rejects 409 not_supported (its kind_features row advertises no verb).
//!
//! 409 VOCABULARY (defined here; mirrored in docs/extensions/rc-helper.md):
//! `not_supported` — the kind/lane never supports the verb; `not_accepting` —
//! supported but not right now (retryable in principle). Deliberately no 501s.
//!
//! HANDLER PRECEDENCE (identical across the verbs, and to handleInput's
//! precedent): body size (413) → body validation (400) → path-value validation
//! (400) → tracked-session lookup (404) → capability check (409). Validating
//! before the lookup keeps a malformed request's answer independent of which
//! sessions exist.
//!
//! The verb handlers deliberately take NO input mutex and capture NO pane:
//! they deliver through the lane's own protocol, not the pane.

use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, LazyLock};

use axum::extract::{Path, State};
use axum::response::Response;
use http::StatusCode;
use regex::Regex;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use shed_core::rc::RcKindFeatures;
use shed_rc_engine::capabilities::kind_features;
use shed_rc_engine::text::normalize_newlines;

use super::hub::{write_error, write_json, Hub};
use super::messages::{
    trim_feed_text, APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS,
    APPROVAL_DECISION_DENY, APPROVAL_STATUS_RESOLVED,
};
use super::watch::SessionWatcher;
use super::watch_opencode_transport::{ApprovalClaim, OcWatcherError};

/// Caps every POST body the hub accepts — 413 past it (`hubMaxBodyBytes`,
/// `hub_verbs.go:70`). 16 KiB is the input handler's long-standing cap, reused
/// so one number governs the whole surface (and mirrored by the server proxy's
/// blanket cap). KNOWN LIMIT: may be small for a structured-lane turn carrying
/// pasted context; raising it is a contract change, not a quiet bump.
pub const HUB_MAX_BODY_BYTES: usize = 16 << 10;

/// kind_features.input value for a lane accepting whole TURNS
/// (`inputModeTurn`, `hub_verbs.go:76`) — opencode.
pub const INPUT_MODE_TURN: &str = "turn";
/// kind_features.input value for pane-gated line delivery (`inputModeGated`,
/// `hub_verbs.go:82`). Single-valued: a kind that moves to "turn" LEAVES
/// "gated" behind — handleInput derives its gate from this value alone.
pub const INPUT_MODE_GATED: &str = "gated";
/// kind_features.approvals value for a lane answered THROUGH the hub
/// (`approvalsRemote`, `hub_verbs.go:86`).
pub const APPROVALS_REMOTE: &str = "remote";

// Hub error codes carried in the {error, message} envelope
// (`hub_verbs.go:126-137`). One vocabulary, declared once.
pub const ERR_NOT_SUPPORTED: &str = "not_supported";
pub const ERR_NOT_ACCEPTING: &str = "not_accepting";
/// 409: a DIFFERENT decision was posted for an already-resolved approval.
pub const ERR_ALREADY_RESOLVED: &str = "already_resolved";
/// 404: the slug is known but carries no approval with this (valid) id.
pub const ERR_UNKNOWN_APPROVAL: &str = "unknown_approval";

/// The rejection a verb falls to when the capability check passed but the
/// session has no watcher implementing the verb's lane (`noLaneMsg`,
/// `hub_verbs.go:146`). Genuinely reachable: an opencode session whose watcher
/// has not been built yet. A retryable 409, not a success with no effect.
pub const NO_LANE_MSG: &str = "no lane is attached to this session";

/// The CONTRACT grammar for an approval id (`ApprovalIDRe`,
/// `hub_verbs.go:163`) — a deliberate design decision: must start
/// alphanumeric (so "."/".."/"..." can never match — traversal excluded by
/// the grammar itself), allows the dot/colon/underscore/dash native lane ids
/// carry, capped at 128 chars. Exported because it is SHARED: the server-side
/// proxy path classifier mirrors this exact expression.
pub static APPROVAL_ID_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$").expect("approval id re"));

/// A boxed lane future — the Rust spelling of Go's context-taking interface
/// methods (`async fn` in a trait is not dyn-compatible; the handler's own
/// cancellation IS the context, axum drops the future on disconnect).
pub type LaneFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, OcWatcherError>> + Send + 'a>>;

/// Delivers a whole turn, returning the lane's opaque turn handle
/// (`turnStarter`, `hub_verbs.go:97`).
pub trait TurnStarter: Send + Sync {
    fn start_turn<'a>(&'a self, text: &'a str) -> LaneFuture<'a, String>;
}

/// Asks the lane to abort the running turn (`turnInterrupter`,
/// `hub_verbs.go:102`). Success means DELIVERED, not stopped.
pub trait TurnInterrupter: Send + Sync {
    fn interrupt_turn(&self) -> LaneFuture<'_, ()>;
}

/// Answers an approval through the lane's own protocol (`approvalResolver`,
/// `hub_verbs.go:113`). The four bookkeeping methods make check-then-act
/// atomic: `approval_state` is the fold-backed oracle for the
/// 404/replay/conflict decision (NOT pending_approvals, which is pending-only
/// by wire contract); `claim_approval` takes exclusive ownership of a pending
/// id; `release_approval` hands it back when the upstream write fails;
/// `commit_approval` records the resolution synchronously and reports the
/// decision the lane ACTUALLY holds.
pub trait ApprovalResolver: Send + Sync {
    /// `(status, decision)`; `None` = unknown id.
    fn approval_state(&self, id: &str) -> Option<(String, String)>;
    fn claim_approval(&self, id: &str, decision: &str) -> ApprovalClaim;
    fn release_approval(&self, id: &str);
    fn commit_approval(&self, id: &str, decision: &str) -> String;
    fn resolve_approval<'a>(&'a self, id: &'a str, decision: &'a str) -> LaneFuture<'a, ()>;
}

/// The POST /turn body (`turnRequest`, `hub_verbs.go:167`). Unknown fields are
/// ignored; `options` is RESERVED — decoded rather than dropped so a client
/// can start sending it without a wire change.
///
/// `text` rides `null_default` for Go's `json.Unmarshal(null)` no-op:
/// `{"text":null}` leaves the zero value and falls to 400 empty_text, where a
/// bare serde `String` would call it a type error (400 invalid_json). Same
/// status, different contract byte.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct TurnRequest {
    #[serde(default, deserialize_with = "super::messages::null_default")]
    pub text: String,
    #[serde(default)]
    #[allow(dead_code)] // reserved (accepted and ignored, as in Go)
    pub options: Option<serde_json::Map<String, serde_json::Value>>,
}

/// The POST /approvals/{id} body (`approvalRequest`, `hub_verbs.go:178`).
/// `decision` rides `null_default` for the same reason as [`TurnRequest::text`]
/// — `{"decision":null}` is 400 invalid_decision in Go, not invalid_json.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct ApprovalRequest {
    #[serde(default, deserialize_with = "super::messages::null_default")]
    pub decision: String,
}

// The verb SUCCESS bodies, pinned by these types + the round-trip schema test
// (`hub_verbs.go:202-219`):
//
//	turn       → 202 {"turn_id": "<opaque>"}
//	interrupt  → 202 {"interrupting": true}
//	approvals  → 200 {"resolved": true, "decision": "<decision>"}
#[derive(Debug, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct TurnResponse {
    pub turn_id: String,
}

#[derive(Debug, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct InterruptResponse {
    pub interrupting: bool,
}

#[derive(Debug, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct ApprovalResponse {
    pub resolved: bool,
    pub decision: String,
}

/// `validApprovalDecision`, `hub_verbs.go:222`.
pub(crate) fn valid_approval_decision(d: &str) -> bool {
    matches!(
        d,
        APPROVAL_DECISION_ALLOW | APPROVAL_DECISION_ALLOW_ALWAYS | APPROVAL_DECISION_DENY
    )
}

/// The kind_features row lookup every capability gate reads
/// (`kindFeatureRow`, `capabilities.go:285` — memoized like Go's
/// `sync.OnceValue`). A kind with NO row (shell, unknown) yields the ZERO
/// row, which advertises no verb — a missing row rejects exactly as finally
/// as an explicit false.
static KIND_FEATURE_ROWS: LazyLock<std::collections::HashMap<String, RcKindFeatures>> =
    LazyLock::new(kind_features);

/// The zero row a kind with no entry falls back to — Go's map lookup yielding
/// the zero `RCKindFeatures`. Borrowed, never cloned, so the capability gates
/// stay allocation-free on the request path.
static ZERO_KIND_FEATURE_ROW: LazyLock<RcKindFeatures> = LazyLock::new(|| RcKindFeatures {
    post_input: false,
    approvals: String::new(),
    watch: false,
    input: String::new(),
    feed: String::new(),
    interrupt: false,
    attach: String::new(),
});

pub(crate) fn kind_feature_row(kind: &shed_core::rc::RcKind) -> &'static RcKindFeatures {
    KIND_FEATURE_ROWS
        .get(kind.as_str())
        .unwrap_or(&ZERO_KIND_FEATURE_ROW)
}

// ---------------------------------------------------------------------------
// Body machinery (`decodeHubBody`/`drainToEOF`/`discardHubBody`,
// hub_verbs.go:498-563)
// ---------------------------------------------------------------------------

/// Go's `ReadTimeout: 30s` (`hub.go:756`), applied WHERE GO APPLIES IT.
///
/// Go's `ReadTimeout` bounds reading THE REQUEST, and `net/http` disarms it
/// the moment the handler starts — `connReader.startBackgroundRead` calls
/// `rwc.SetReadDeadline(time.Time{})` (go1.25 `src/net/http/server.go:697`) —
/// so a long-lived handler (the `/v1/events` SSE stream) is never killed by
/// it. A CONNECTION-level read deadline in Rust would not be equivalent:
/// hyper keeps polling the read half while a response body streams (to detect
/// EOF or a pipelined head), so an idle SSE client would trip the deadline one
/// heartbeat in and the stream would die at 30 s. The bound therefore lives on
/// the request-body read — the slow-dribble posture Go's ReadTimeout actually
/// defends, since `MaxBytesReader` caps SIZE but not the time to dribble it —
/// and the socket itself is served raw ([`super::hub::serve`]).
pub(crate) const BODY_READ_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);

/// A capped streaming body read — the `http.MaxBytesReader` half. Reads AT
/// MOST `cap + 1` bytes (the +1 is how "exceeded" is detected without reading
/// the tail, exactly as MaxBytesReader stops the decoder at the cap): returns
/// `(buf, exceeded)`; a transport read error is `Err(())`.
pub(crate) async fn read_body_capped(
    body: axum::body::Body,
    cap: usize,
) -> Result<(Vec<u8>, bool), ()> {
    let read = async move {
        use futures_util::StreamExt;
        let mut buf: Vec<u8> = Vec::new();
        let mut stream = body.into_data_stream();
        while let Some(chunk) = stream.next().await {
            let Ok(chunk) = chunk else {
                return Err(());
            };
            buf.extend_from_slice(&chunk);
            if buf.len() > cap {
                buf.truncate(cap + 1);
                return Ok((buf, true));
            }
        }
        Ok((buf, false))
    };
    // A dribbling client: Go's expired ReadTimeout surfaces to the handler as
    // a body READ ERROR, which is exactly what the `Err` arm means here (400
    // invalid_json on the decoding paths, ignored on the discard path).
    match tokio::time::timeout(BODY_READ_TIMEOUT, read).await {
        Ok(res) => res,
        Err(_elapsed) => Err(()),
    }
}

pub(crate) enum BodyError {
    /// 413 — the cap tripped (`wroteTooLarge`, `hub_verbs.go:556`).
    TooLarge,
    /// 400 invalid_json, "malformed request body".
    Malformed,
    /// 400 invalid_json, "request body has trailing data after the JSON value".
    Trailing,
}

impl BodyError {
    pub(crate) fn respond(&self) -> Response {
        match self {
            BodyError::TooLarge => write_error(
                StatusCode::PAYLOAD_TOO_LARGE,
                "too_large",
                "request body exceeds 16 KiB",
            ),
            BodyError::Malformed => write_error(
                StatusCode::BAD_REQUEST,
                "invalid_json",
                "malformed request body",
            ),
            BodyError::Trailing => write_error(
                StatusCode::BAD_REQUEST,
                "invalid_json",
                "request body has trailing data after the JSON value",
            ),
        }
    }
}

/// Bounds and JSON-decodes a POST body under Go's STREAM-ORDER semantics
/// (`decodeHubBody` + `drainToEOF`): the decoder only ever sees the first
/// `cap` bytes (MaxBytesReader never hands it more), so
///
/// - a syntax error within the cap → 400, even when the body is huge
///   (the "oversized junk earns the 400" cell — the parse fails before the
///   cap trips);
/// - a value (or its trailer) still open at the cap with more body behind it
///   → 413 (the decoder's next read hits the cap);
/// - a value still open at genuine end-of-body → 400 (Go's unexpected EOF);
/// - a complete value + a second value / non-whitespace garbage within the
///   cap → 400 (the body is exactly ONE JSON value);
/// - a complete value + anything (even whitespace) beyond the cap → 413
///   (drainToEOF reads the tail through the MaxBytesReader).
///
/// Unknown fields are IGNORED (an old server must not reject a newer client's
/// additive field); Content-Type is not enforced; a top-level `null` decodes
/// as the zero value (Go's `json.Unmarshal(null)` no-op).
///
/// TWO ACCEPTED DIVERGENCES, both fail-CLOSED, both inside the 16 KiB cap, and
/// both recorded for the H12 canonicalization:
///
/// - `serde_json` enforces a 128-level recursion limit where Go's decoder is
///   effectively unbounded, so a `>128`-deep `options` object is 400
///   invalid_json here and 200 in Go. Rejecting deeply-nested input is the
///   safer half of the pair.
/// - A LONE SURROGATE ESCAPE (`"\ud800"` — well-formed JSON text, unpaired)
///   is 400 here; Go's `encoding/json` substitutes U+FFFD and proceeds. serde
///   has no lossy mode for escapes (unlike raw bytes, handled below), and the
///   input is hostile-only — no real client emits one.
pub(crate) fn decode_json_capped<T: DeserializeOwned + Default>(
    buf: &[u8],
    exceeded: bool,
    cap: usize,
) -> Result<T, BodyError> {
    let head = &buf[..buf.len().min(cap)];
    match decode_json_head(head, exceeded) {
        Ok(v) => Ok(v),
        Err(e) => {
            // RAW invalid UTF-8 inside a JSON string: Go's decoder substitutes
            // U+FFFD per bad byte and proceeds (`{"text":"a\xffb"}` DELIVERS
            // "a\u{fffd}b"), while serde rejects the slice outright. Retry once
            // over the lossily-converted text so the two agree.
            //
            // The cap model's byte offsets (`byte_offset`, the trailer scan)
            // then live in CONVERTED space: each invalid byte widens to the
            // 3-byte U+FFFD, so a body sitting within 2 bytes of the cap can
            // classify differently between the two spaces. Accepted — that
            // combination is hostile-input-only and both answers are a
            // rejection.
            if std::str::from_utf8(head).is_err() {
                return decode_json_head(String::from_utf8_lossy(head).as_bytes(), exceeded);
            }
            Err(e)
        }
    }
}

/// The strict single-pass half of [`decode_json_capped`] (the byte model
/// proper), so the invalid-UTF-8 retry above can run it twice.
fn decode_json_head<T: DeserializeOwned + Default>(
    head: &[u8],
    exceeded: bool,
) -> Result<T, BodyError> {
    let mut values = serde_json::Deserializer::from_slice(head).into_iter::<serde_json::Value>();
    let first = match values.next() {
        // Empty / whitespace-only body: Go's Decode returns io.EOF → 400.
        None => return Err(BodyError::Malformed),
        Some(Err(e)) => {
            if e.is_eof() && exceeded {
                return Err(BodyError::TooLarge);
            }
            return Err(BodyError::Malformed);
        }
        Some(Ok(v)) => v,
    };
    let end = values.byte_offset();
    // The trailer scan (`drainToEOF`): the body is exactly ONE JSON value.
    let rest = &head[end..];
    let mut trailer =
        serde_json::Deserializer::from_slice(rest).into_iter::<serde::de::IgnoredAny>();
    match trailer.next() {
        None => {
            // Whitespace-only tail within the cap; anything beyond it — even
            // whitespace — still trips the cap (Go's io.Copy through the
            // MaxBytesReader).
            if exceeded {
                return Err(BodyError::TooLarge);
            }
        }
        Some(Ok(_)) => return Err(BodyError::Trailing), // trailing JSON value
        Some(Err(e)) => {
            if e.is_eof() && exceeded {
                return Err(BodyError::TooLarge);
            }
            return Err(BodyError::Trailing);
        }
    }
    if first.is_null() {
        return Ok(T::default()); // Go: Unmarshal(null) leaves the zero value
    }
    // Go unmarshals into a STRUCT: any non-object value is "cannot unmarshal
    // <kind> into Go value of type rc.turnRequest" → 400. serde's DERIVED
    // Deserialize also accepts the TUPLE form, so `["hi"]` posted to /input
    // would decode as text="hi" and DELIVER where Go 400s. The object
    // requirement is the wire contract, so it is checked here rather than
    // relying on the shape of each request type's derive.
    if !first.is_object() {
        return Err(BodyError::Malformed);
    }
    serde_json::from_value(first).map_err(|_| BodyError::Malformed)
}

/// `decodeHubBody` — read + decode under the shared 16 KiB cap; `Err` is the
/// already-built rejection response.
pub(crate) async fn decode_hub_body<T: DeserializeOwned + Default>(
    body: axum::body::Body,
) -> Result<T, Response> {
    let (buf, exceeded) = read_body_capped(body, HUB_MAX_BODY_BYTES)
        .await
        .map_err(|()| BodyError::Malformed.respond())?;
    decode_json_capped(&buf, exceeded, HUB_MAX_BODY_BYTES).map_err(|e| e.respond())
}

/// Drains a body the verb does not read, purely to enforce the size cap
/// (`discardHubBody`, `hub_verbs.go:548`): an oversized body is a 413 even
/// when its content is irrelevant. Any OTHER read error is ignored — the body
/// is not part of the verb's contract.
pub(crate) async fn discard_hub_body(body: axum::body::Body) -> Result<(), Response> {
    match read_body_capped(body, HUB_MAX_BODY_BYTES).await {
        Ok((_, true)) => Err(BodyError::TooLarge.respond()),
        Ok((_, false)) | Err(()) => Ok(()),
    }
}

// ---------------------------------------------------------------------------
// The shared verb middle + lane-error mapping
// ---------------------------------------------------------------------------

/// What `verbTarget` (`hub_verbs.go:481`) resolves: the watcher pointer copied
/// out under the track lock (reconcile commits that field under the same
/// lock; asserting it after the unlock would be a race) plus the kind's row.
/// Go also returns the `*trackedSession` for the republish identity check —
/// the Rust identity is the watcher `Arc` itself (a recreate closes and
/// replaces it together with the entry, so `Arc::ptr_eq` on the CURRENT
/// entry's watcher answers "is this still the incarnation I resolved?").
///
/// The returned watcher is used UNLOCKED (the lane call must never run under
/// the track lock). KNOWN, ACCEPTED WINDOW: between this copy and the lane
/// call, reconcile may replace the entry and close the old watcher; the
/// in-flight call then targets a dead per-create port, fails, and maps to 409
/// not_accepting.
pub(crate) struct VerbTarget {
    pub watcher: Option<Arc<dyn SessionWatcher + Send + Sync>>,
    pub kf: &'static RcKindFeatures,
}

impl Hub {
    /// `verbTarget` — 404 unknown_slug when the slug is not tracked (the
    /// handleMessages rule: no re-derivation from tmux).
    // The Err IS the response every caller returns verbatim — boxing it would
    // add an allocation to every rejection for a lint aimed at hot Ok-paths.
    #[allow(clippy::result_large_err)]
    pub(crate) fn verb_target(&self, slug: &str) -> Result<VerbTarget, Response> {
        let ts = self.lock_track();
        let Some(tr) = ts.tracked.get(slug) else {
            return Err(write_error(
                StatusCode::NOT_FOUND,
                "unknown_slug",
                "no such rc session",
            ));
        };
        Ok(VerbTarget {
            watcher: tr.watcher.clone(),
            kf: kind_feature_row(&tr.kind),
        })
    }

    /// Refreshes the session's pending_approvals snapshot right after a
    /// resolve (`republishApprovals`, `hub_verbs.go:422`), through the SAME
    /// producer reconcile uses. The entry may be an ORPHAN by now (a recreate
    /// mid-POST): the write happens only when the CURRENT entry still holds
    /// the watcher this request resolved through (`Arc::ptr_eq` — the Rust
    /// spelling of Go's `cur == tr` pointer identity; entry and watcher are
    /// replaced together). Read UNLOCKED, committed under the track lock.
    pub(crate) fn republish_approvals(
        &self,
        slug: &str,
        watcher: &Arc<dyn SessionWatcher + Send + Sync>,
    ) {
        let Some(publisher) = watcher.as_approval_publisher() else {
            return;
        };
        let pending = publisher.pending_approvals();
        let mut ts = self.lock_track();
        if let Some(cur) = ts.tracked.get_mut(slug) {
            if cur
                .watcher
                .as_ref()
                .is_some_and(|w| Arc::ptr_eq(w, watcher))
            {
                cur.pending_approvals = pending;
            }
        }
    }

    /// Maps a lane method's failure onto the 409 not_accepting envelope
    /// (`writeLaneError`, `hub_verbs.go:446`) — never a new status code,
    /// never a 5xx. The WIRE message is deliberately coarse: a lane error's
    /// text names the upstream URL (loopback port + pinned agent session id —
    /// hub-internal addressing), so the detail goes to the hub log and the
    /// client gets the verb's prefix plus, at most, the upstream status code.
    /// The two sentinels leak nothing and are surfaced verbatim.
    pub(crate) fn write_lane_error(&self, prefix: &str, err: &OcWatcherError) -> Response {
        match err {
            OcWatcherError::NoAgentSession => {
                return write_error(StatusCode::CONFLICT, ERR_NOT_ACCEPTING, &err.to_string());
            }
            OcWatcherError::Closed => {
                return write_error(
                    StatusCode::CONFLICT,
                    ERR_NOT_ACCEPTING,
                    &format!("{prefix}: the agent session is gone"),
                );
            }
            _ => {}
        }
        (self.cfg.logf)(&format!("rc hub: {prefix}: {err}"));
        let detail = match err {
            OcWatcherError::Status { status, .. } => format!("upstream status {status}"),
            _ => "upstream request failed".to_string(),
        };
        write_error(
            StatusCode::CONFLICT,
            ERR_NOT_ACCEPTING,
            &format!("{prefix}: {detail}"),
        )
    }
}

// ---------------------------------------------------------------------------
// The three verb handlers
// ---------------------------------------------------------------------------

/// POST /v1/sessions/{slug}/turn (`handleTurn`, `hub_verbs.go:234`).
pub(crate) async fn handle_turn(
    State(hub): State<Arc<Hub>>,
    Path(slug): Path<String>,
    req: axum::extract::Request,
) -> Response {
    let body: TurnRequest = match decode_hub_body(req.into_body()).await {
        Ok(v) => v,
        Err(resp) => return resp,
    };
    // The lane receives the NORMALIZED text verbatim (CRLF folded), not a
    // sanitized or re-quoted copy: a turn travels as a JSON string over the
    // agent's own protocol — there is no pane, no shell, no escaping layer.
    let text = normalize_newlines(&body.text);
    if trim_feed_text(&text).is_empty() {
        return write_error(StatusCode::BAD_REQUEST, "empty_text", "text is required");
    }
    let target = match hub.verb_target(&slug) {
        Ok(t) => t,
        Err(resp) => return resp,
    };
    if target.kf.input != INPUT_MODE_TURN {
        return write_error(
            StatusCode::CONFLICT,
            ERR_NOT_SUPPORTED,
            "this session's kind does not accept turns",
        );
    }
    let Some(starter) = target
        .watcher
        .as_deref()
        .and_then(SessionWatcher::as_turn_starter)
    else {
        return write_error(StatusCode::CONFLICT, ERR_NOT_ACCEPTING, NO_LANE_MSG);
    };
    match starter.start_turn(&text).await {
        Ok(turn_id) => write_json(StatusCode::ACCEPTED, &TurnResponse { turn_id }),
        Err(err) => hub.write_lane_error("the agent did not accept the turn", &err),
    }
}

/// POST /v1/sessions/{slug}/interrupt (`handleInterrupt`, `hub_verbs.go:274`).
/// The body is IGNORED but still size-capped.
pub(crate) async fn handle_interrupt(
    State(hub): State<Arc<Hub>>,
    Path(slug): Path<String>,
    req: axum::extract::Request,
) -> Response {
    if let Err(resp) = discard_hub_body(req.into_body()).await {
        return resp;
    }
    let target = match hub.verb_target(&slug) {
        Ok(t) => t,
        Err(resp) => return resp,
    };
    if !target.kf.interrupt {
        return write_error(
            StatusCode::CONFLICT,
            ERR_NOT_SUPPORTED,
            "this session's kind does not support interrupt",
        );
    }
    let Some(interrupter) = target
        .watcher
        .as_deref()
        .and_then(SessionWatcher::as_turn_interrupter)
    else {
        return write_error(StatusCode::CONFLICT, ERR_NOT_ACCEPTING, NO_LANE_MSG);
    };
    match interrupter.interrupt_turn().await {
        Ok(()) => write_json(
            StatusCode::ACCEPTED,
            &InterruptResponse { interrupting: true },
        ),
        Err(err) => hub.write_lane_error("the interrupt was not delivered", &err),
    }
}

/// POST /v1/sessions/{slug}/approvals/{id} (`handleApproval`,
/// `hub_verbs.go:303`). A kind that answers approvals on the pane
/// (approvals == "tui") is rejected 409 not_supported before any lookup —
/// including the informational `pane-*` ids, which are deliberately not
/// remotely resolvable.
pub(crate) async fn handle_approval(
    State(hub): State<Arc<Hub>>,
    Path((slug, id)): Path<(String, String)>,
    req: axum::extract::Request,
) -> Response {
    let body: ApprovalRequest = match decode_hub_body(req.into_body()).await {
        Ok(v) => v,
        Err(resp) => return resp,
    };
    if !valid_approval_decision(&body.decision) {
        return write_error(
            StatusCode::BAD_REQUEST,
            "invalid_decision",
            "decision must be one of allow, allow_always, deny",
        );
    }
    // A syntactically invalid id is a malformed REQUEST, not a missing
    // approval: 400, never 404 (that would imply the id was well-formed but
    // unknown — the distinct unknown_approval case).
    if !APPROVAL_ID_RE.is_match(&id) {
        return write_error(
            StatusCode::BAD_REQUEST,
            "invalid_approval_id",
            "approval id must match ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$",
        );
    }
    let target = match hub.verb_target(&slug) {
        Ok(t) => t,
        Err(resp) => return resp,
    };
    if target.kf.approvals != APPROVALS_REMOTE {
        return write_error(
            StatusCode::CONFLICT,
            ERR_NOT_SUPPORTED,
            "approvals for this session's kind are answered in the terminal",
        );
    }
    let Some(watcher) = target.watcher else {
        return write_error(StatusCode::CONFLICT, ERR_NOT_ACCEPTING, NO_LANE_MSG);
    };
    let Some(resolver) = watcher.as_approval_resolver() else {
        return write_error(StatusCode::CONFLICT, ERR_NOT_ACCEPTING, NO_LANE_MSG);
    };

    // Resolution state comes from the LANE's fold, never pendingApprovals
    // (pending-only by wire contract — cannot tell "already answered" from
    // "never existed").
    if let Some(resp) = answered_from_approval_state(resolver, &id, &body.decision) {
        return resp;
    }

    // CLAIM the id before writing: check-then-act would let two concurrent
    // requests both find it pending and both POST upstream. Exactly one
    // request owns an id's resolution.
    match resolver.claim_approval(&id, &body.decision) {
        ApprovalClaim::Busy => {
            // Deliberately reported even for the SAME decision: the honest
            // answer is "in flight"; the retry sees the recorded resolution.
            return write_error(
                StatusCode::CONFLICT,
                ERR_NOT_ACCEPTING,
                "a resolution for this approval is already in progress",
            );
        }
        ApprovalClaim::Settled => {
            // It stopped being pending between the read and the claim. Answer
            // from the recorded state rather than POSTing a second answer.
            if let Some(resp) = answered_from_approval_state(resolver, &id, &body.decision) {
                return resp;
            }
            return write_error(
                StatusCode::CONFLICT,
                ERR_NOT_ACCEPTING,
                "this approval changed state; retry",
            );
        }
        ApprovalClaim::Claimed => {}
    }

    if let Err(err) = resolver.resolve_approval(&id, &body.decision).await {
        // The write failed, nothing was resolved: hand the claim back so a
        // retry (or the operator, in the TUI) can still answer the ask.
        resolver.release_approval(&id);
        return hub.write_lane_error("the agent did not accept the decision", &err);
    }
    // Bookkeeping runs SYNCHRONOUSLY on success, closing the ~1-tick window
    // in which a same-decision replay would re-POST. commit reports the
    // decision the fold ACTUALLY holds — if the stream's own reply won the
    // race, its record is the truth.
    let recorded = resolver.commit_approval(&id, &body.decision);
    hub.republish_approvals(&slug, &watcher);
    write_json(
        StatusCode::OK,
        &ApprovalResponse {
            resolved: true,
            decision: recorded,
        },
    )
}

/// Answers the request from the lane's recorded approval state when that
/// state is final (`answeredFromApprovalState`, `hub_verbs.go:393`): unknown
/// id (404), same decision replayed (200 idempotent, NO upstream write), or a
/// different decision against a resolved ask (409 already_resolved — also
/// covering "resolved with no known decision"). `None` means the ask is
/// PENDING and the caller proceeds to claim it.
fn answered_from_approval_state(
    resolver: &dyn ApprovalResolver,
    id: &str,
    want: &str,
) -> Option<Response> {
    let Some((status, decision)) = resolver.approval_state(id) else {
        return Some(write_error(
            StatusCode::NOT_FOUND,
            ERR_UNKNOWN_APPROVAL,
            "this session has no approval with that id",
        ));
    };
    if status == APPROVAL_STATUS_RESOLVED && decision == want {
        return Some(write_json(
            StatusCode::OK,
            &ApprovalResponse {
                resolved: true,
                decision,
            },
        ));
    }
    if status == APPROVAL_STATUS_RESOLVED {
        return Some(write_error(
            StatusCode::CONFLICT,
            ERR_ALREADY_RESOLVED,
            "this approval was already resolved",
        ));
    }
    None
}
