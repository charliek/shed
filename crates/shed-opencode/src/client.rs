//! The HTTP client — two reqwest clients, opencode's routes, and the
//! [`AgentLane`] implementation on top of them.
//!
//! # Two clients, not one
//!
//! [`OpencodeClient`] holds **two** `reqwest::Client`s, both `.no_proxy()`
//! (opencode is on loopback; an inherited `http_proxy` would send every call to
//! a proxy that cannot reach it):
//!
//! - `rest` — a 5 s **total** timeout, the hub's `restTimeout`/`ocVerbTimeout`.
//!   These are loopback calls to a process on this machine; a verb that has not
//!   answered in 5 s is not going to.
//! - `stream` — a 3 s **connect** timeout and **no request timeout at all**.
//!   `GET /event` is a long-lived stream: a client-level request timeout would
//!   cut it mid-session, which is exactly the bug a request timeout looks like
//!   nothing (the stream just ends, the watcher reconnects, and the transcript
//!   resets every N seconds forever). Liveness on that stream is the watcher's
//!   stall window, not a timeout here.
//!
//! # The two bounds the stream client still needs
//!
//! "No request timeout" is about the BODY. Two waits on that client are not the
//! body and are bounded explicitly by [`STREAM_HEAD_TIMEOUT`] (the hub's
//! `headerTimeout`):
//!
//! - the **response head**. A peer that completes the TCP handshake and then
//!   never sends a status line satisfies `connect_timeout` and would otherwise
//!   park the watcher forever — after its `Reset`, before there is any body for
//!   the stall timer to watch, so no stall, no retry and no `Down`.
//! - the **error body** of a non-2xx `/event`, which is a small JSON object and
//!   can wedge exactly the same way.
//!
//! # Bounded bodies
//!
//! Every REST body is read through [`read_body_capped`], which refuses anything
//! past [`MAX_REST_BYTES`] — the hub's `maxRESTBytes`. `.text()` collects an
//! entire response before anything can inspect it, so a hostile or broken
//! `/session/{id}/message` could exhaust memory inside the 5 s timeout; the
//! ring's own bounds only apply after the bytes are already allocated and
//! decoded. The SSE body is deliberately NOT capped this way — it streams.
//!
//! # Directory routing
//!
//! opencode resolves an *instance* from `?directory=` (its
//! `WorkspaceRoutingMiddleware`), falling back to the server process's cwd. Four
//! routes are instance-scoped and therefore MUST carry the subscribed session's
//! directory or they answer for the wrong workspace: `GET /event`,
//! `GET /session/status`, `GET /permission`, `GET /question`. `POST /session`
//! (create) carries the new session's `cwd` for the same reason. Every
//! id-addressed route (`/session/{id}…`, `/permission/{id}/reply`,
//! `/question/{id}/…`) is global and sends **no** `directory` — the id is the
//! address.
//!
//! # Error mapping
//!
//! One table, applied by route family ([`Route`]): 401 → `Unauthorized`; 404 →
//! `UnknownSession` on a session route and `UnknownApproval` on a
//! permission/question route; 409 (or a 4xx whose opencode error body names an
//! aborted/busy condition) → `NotAccepting`; any other 4xx carrying a decodable
//! opencode error body → `BadRequest(message)`; any other non-2xx → `Failed`;
//! a dial/transport failure → `Unavailable`.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::time::Duration;

use base64::Engine as _;
use serde::Deserialize;
use serde_json::json;
use serde_json::value::RawValue;
use shed_core::lane::{
    normalize_question_answer, option_kind, AgentLane, LaneAnswer, LaneApproval, LaneApprovalKind,
    LaneApprovalOption, LaneCapabilities, LaneDecision, LaneError, LaneHistory, LaneSession,
    LaneSubscription, SendMode,
};
use shed_core::rc::RcActivity;

use crate::fold::OpencodeFold;
use crate::helpers::{null_default, raw_opt};
use crate::ring::{now_utc, MessageRing};
use crate::watcher;

/// The `rest` client's total-request bound (the hub's `restTimeout` /
/// `ocVerbTimeout`, both 5 s).
pub(crate) const REST_TIMEOUT: Duration = Duration::from_secs(5);
/// The `stream` client's CONNECT bound (the hub's `dialTimeout`). There is
/// deliberately no request timeout beside it.
pub(crate) const STREAM_CONNECT_TIMEOUT: Duration = Duration::from_secs(3);
/// How long the `stream` client waits for the things on that connection that
/// are NOT the long-lived body: the response head, and the error body of a
/// non-2xx. The hub's `headerTimeout`, same 5 s.
///
/// Public because it is the bound a test asserts a wedged peer against.
pub const STREAM_HEAD_TIMEOUT: Duration = Duration::from_secs(5);

/// One REST response body's cap (the hub's `maxRESTBytes`). Enforced WHILE the
/// body is consumed, so an oversized response is refused rather than
/// allocated. The SSE body is exempt — it streams.
pub const MAX_REST_BYTES: usize = 8 << 20;

/// How far below the root [`OpencodeClient::approval_scope`] walks, and how
/// many ids it may hold in total (the root included).
///
/// opencode is untrusted input: `parentID` comes off its wire, a cycle is
/// representable, and a pathological tree would otherwise cost one
/// `/session/{id}/children` request per node forever. The visited set makes a
/// cycle terminate; these two make an honest-but-huge tree terminate. Both are
/// far above any real sub-agent tree (a handful of sessions, one or two levels)
/// — and the total doubles as the bound on how many requests one seed issues.
pub const MAX_DESCENDANT_DEPTH: usize = 8;
/// See [`MAX_DESCENDANT_DEPTH`].
pub const MAX_DESCENDANT_SESSIONS: usize = 256;

/// HTTP Basic credentials for an opencode server started with
/// `OPENCODE_SERVER_PASSWORD` (its `ServerAuth`; the username defaults to
/// `opencode`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BasicAuth {
    pub username: String,
    pub password: String,
}

impl BasicAuth {
    /// The credentials opencode's own default expects: username `opencode`.
    pub fn password(password: impl Into<String>) -> BasicAuth {
        BasicAuth {
            username: "opencode".to_string(),
            password: password.into(),
        }
    }

    /// The `Authorization` header value, computed once at construction rather
    /// than per request.
    fn header(&self) -> String {
        let raw = format!("{}:{}", self.username, self.password);
        format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode(raw)
        )
    }
}

/// Which 404 a route means. opencode answers 404 for both "no such session" and
/// "no such request", and the contract splits them — so the caller says which
/// family it asked about rather than the mapper guessing from the path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Route {
    /// `/session…` — a 404 is [`LaneError::UnknownSession`].
    Session,
    /// `/permission/{id}…`, `/question/{id}…` — a 404 is
    /// [`LaneError::UnknownApproval`].
    Approval,
}

/// The opencode adapter: the transport, the verbs, and [`AgentLane`].
///
/// Cheap to clone — both reqwest clients are `Arc` inside, and the rest is a
/// URL and a header string. [`AgentLane::subscribe`] clones one into the pump
/// task it spawns.
#[derive(Debug, Clone)]
pub struct OpencodeClient {
    base: reqwest::Url,
    rest: reqwest::Client,
    stream: reqwest::Client,
    auth: Option<String>,
    /// Shared across clones on purpose: the ledger is about what THIS adapter
    /// has answered, and a clone handed to the pump task is the same adapter.
    answered: std::sync::Arc<std::sync::Mutex<AnswerLedger>>,
}

/// How many answered approvals the ledger remembers before forgetting the
/// oldest. A session has a handful of open asks at a time; the cap exists so a
/// very long-lived client cannot grow the map without bound.
const MAX_ANSWER_LEDGER: usize = 256;

/// What [`AgentLane::answer`] has already done with an approval id.
///
/// This is the whole implementation of [`LaneError::AlreadySubmitted`] and
/// [`LaneError::AlreadyResolved`], and it is deliberately CLIENT-SIDE: the
/// contract describes `Submitted` as "the client sent an answer and the adapter
/// has not yet seen the agent acknowledge it… so a double-tap is refusable
/// WITHOUT waiting a round trip", which is exactly a local ledger and not
/// anything opencode reports. An answer that FAILS is forgotten again, so a
/// retry after a transport error is not mistaken for a double-tap.
#[derive(Debug, Default)]
struct AnswerLedger {
    /// approval id → `true` while the POST is in flight, `false` once it
    /// succeeded.
    state: std::collections::HashMap<String, bool>,
    /// Insertion order, for the cap.
    order: std::collections::VecDeque<String>,
}

impl AnswerLedger {
    /// Claim the id, or say why it cannot be claimed.
    fn claim(&mut self, id: &str) -> Result<(), LaneError> {
        match self.state.get(id) {
            Some(true) => return Err(LaneError::AlreadySubmitted),
            Some(false) => return Err(LaneError::AlreadyResolved),
            None => {}
        }
        self.state.insert(id.to_string(), true);
        self.order.push_back(id.to_string());
        while self.order.len() > MAX_ANSWER_LEDGER {
            if let Some(old) = self.order.pop_front() {
                self.state.remove(&old);
            }
        }
        Ok(())
    }

    fn settle(&mut self, id: &str, ok: bool) {
        if ok {
            self.state.insert(id.to_string(), false);
        } else {
            self.state.remove(id);
            self.order.retain(|o| o != id);
        }
    }
}

/// The lock, with a poisoned ledger read anyway: a panic elsewhere must not
/// turn the double-tap gate into a permanent outage.
fn lock_ledger(ledger: &std::sync::Mutex<AnswerLedger>) -> std::sync::MutexGuard<'_, AnswerLedger> {
    ledger
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// A live claim on one approval id, released on [`Drop`] unless committed.
///
/// [`AgentLane::answer`] is an `async fn`, so its future can be dropped at the
/// await inside it — a `select!` arm that lost, an aborted task, a client that
/// navigated away. A claim released only on the normal return path would then
/// stay marked "in flight" forever and every later attempt at that approval
/// would answer [`LaneError::AlreadySubmitted`] while nothing at all was on the
/// wire. Releasing in `Drop` is the whole of what makes the gate
/// cancellation-safe: the claim outlives the future only if the future got to
/// [`AnswerClaim::settle`] it.
struct AnswerClaim {
    /// The ledger itself rather than the client: the guard must be movable into
    /// the future without borrowing `&self` across the await.
    ledger: std::sync::Arc<std::sync::Mutex<AnswerLedger>>,
    id: String,
    settled: bool,
}

impl AnswerClaim {
    /// The POST returned. `ok` records the id as answered; a failure forgets it
    /// so a retry after a transport error is not read as a double-tap.
    fn settle(mut self, ok: bool) {
        lock_ledger(&self.ledger).settle(&self.id, ok);
        self.settled = true;
    }
}

impl Drop for AnswerClaim {
    fn drop(&mut self) {
        if !self.settled {
            // Dropped before the POST returned: whatever happened to the
            // request, this client is not waiting on one any more.
            lock_ledger(&self.ledger).settle(&self.id, false);
        }
    }
}

impl OpencodeClient {
    /// Build a client for the opencode server at `base_url`.
    ///
    /// Fails only if reqwest cannot build its TLS/connector stack, which is a
    /// process-level problem rather than a per-call one — hence
    /// [`LaneError::Failed`] rather than `Unavailable`.
    pub fn new(
        base_url: reqwest::Url,
        auth: Option<BasicAuth>,
    ) -> Result<OpencodeClient, LaneError> {
        let rest = reqwest::Client::builder()
            .no_proxy()
            .timeout(REST_TIMEOUT)
            .build()
            .map_err(|e| LaneError::Failed(format!("building the opencode REST client: {e}")))?;
        // NO `.timeout(..)`: see the module doc. `connect_timeout` bounds the
        // dial only, which is what a wedged listener needs.
        let stream = reqwest::Client::builder()
            .no_proxy()
            .connect_timeout(STREAM_CONNECT_TIMEOUT)
            .build()
            .map_err(|e| LaneError::Failed(format!("building the opencode stream client: {e}")))?;
        Ok(OpencodeClient {
            base: base_url,
            rest,
            stream,
            auth: auth.map(|a| a.header()),
            answered: std::sync::Arc::new(std::sync::Mutex::new(AnswerLedger::default())),
        })
    }

    /// The server this adapter talks to — what an `Unavailable` names.
    pub fn base_url(&self) -> &reqwest::Url {
        &self.base
    }

    // ---- request plumbing ----

    /// `base` + `path` + an optional `?directory=`. A path that will not join
    /// is a programming error in this crate, so it surfaces as `Failed`.
    fn url(&self, path: &str, directory: Option<&str>) -> Result<reqwest::Url, LaneError> {
        let mut url = self
            .base
            .join(path.trim_start_matches('/'))
            .map_err(|e| LaneError::Failed(format!("building {path}: {e}")))?;
        if let Some(dir) = directory {
            url.query_pairs_mut().append_pair("directory", dir);
        }
        Ok(url)
    }

    fn auth_header(&self, rb: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        match &self.auth {
            Some(v) => rb.header(reqwest::header::AUTHORIZATION, v),
            None => rb,
        }
    }

    /// A GET whose 2xx body decodes into `T`.
    async fn get_json<T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        directory: Option<&str>,
        route: Route,
    ) -> Result<T, LaneError> {
        let url = self.url(path, directory)?;
        let resp = self
            .auth_header(self.rest.get(url))
            .send()
            .await
            .map_err(|e| dial_error(path, &e))?;
        let body = check_status(path, route, resp).await?;
        serde_json::from_str(&body).map_err(|e| LaneError::Failed(format!("decoding {path}: {e}")))
    }

    /// A POST with a JSON body whose 2xx body is discarded (opencode answers
    /// 204 for `prompt_async` and `true` for the rest).
    async fn post_json(
        &self,
        path: &str,
        directory: Option<&str>,
        route: Route,
        body: serde_json::Value,
    ) -> Result<String, LaneError> {
        let url = self.url(path, directory)?;
        let resp = self
            .auth_header(self.rest.post(url))
            .json(&body)
            .send()
            .await
            .map_err(|e| dial_error(path, &e))?;
        check_status(path, route, resp).await
    }

    /// Open the long-lived `GET /event` stream on the **stream** client.
    /// Instance-scoped: `directory` is the subscribed session's.
    pub(crate) async fn open_event_stream(
        &self,
        directory: &str,
    ) -> Result<reqwest::Response, LaneError> {
        let url = self.url("/event", Some(directory))?;
        // The HEAD is bounded even though the BODY is not: `connect_timeout`
        // is satisfied the moment the TCP handshake completes, so a peer that
        // accepts and then says nothing leaves the watcher parked after its
        // `Reset` with no body to run a stall timer against. Timing out here
        // drops the request future, which cancels it.
        let send = self.auth_header(self.stream.get(url)).send();
        let resp = match tokio::time::timeout(STREAM_HEAD_TIMEOUT, send).await {
            Err(_) => {
                return Err(LaneError::Unavailable(format!(
                    "opencode /event: no response headers within {}s",
                    STREAM_HEAD_TIMEOUT.as_secs()
                )))
            }
            Ok(sent) => sent.map_err(|e| dial_error("/event", &e))?,
        };
        if resp.status().is_success() {
            return Ok(resp);
        }
        let status = resp.status();
        // The error body gets the same bound and the same cap as a REST read —
        // it is a small JSON object, not a stream. A body that could not be
        // read degrades to an empty one: the STATUS is the actionable half, and
        // the table maps it either way.
        let body = tokio::time::timeout(STREAM_HEAD_TIMEOUT, read_body_capped("/event", resp))
            .await
            .ok()
            .and_then(Result::ok)
            .unwrap_or_default();
        Err(map_status(status, "/event", Route::Session, &body))
    }

    // ---- typed routes ----

    /// `GET /session` — the server's global session store, across directories.
    pub(crate) async fn rest_sessions(&self) -> Result<Vec<RestSession>, LaneError> {
        self.get_json("/session", None, Route::Session).await
    }

    /// `GET /session/{id}` — id-addressed, so no `?directory=`.
    pub(crate) async fn rest_session(&self, id: &str) -> Result<RestSession, LaneError> {
        self.get_json(&session_path(id, &[]), None, Route::Session)
            .await
    }

    /// `GET /session/{id}/children` — the descendants tracked at seed time.
    pub(crate) async fn rest_children(&self, id: &str) -> Result<Vec<RestSession>, LaneError> {
        self.get_json(&session_path(id, &["children"]), None, Route::Session)
            .await
    }

    /// `GET /session/{id}/message` — the transcript seed.
    pub(crate) async fn rest_messages(&self, id: &str) -> Result<Vec<RestMessage>, LaneError> {
        self.get_json(&session_path(id, &["message"]), None, Route::Session)
            .await
    }

    /// `GET /session/status?directory=` — instance-scoped. opencode OMITS idle
    /// sessions from the map, so an absent id in a 200 body means idle.
    pub(crate) async fn rest_status(
        &self,
        directory: &str,
    ) -> Result<HashMap<String, RestStatus>, LaneError> {
        self.get_json("/session/status", Some(directory), Route::Session)
            .await
    }

    /// `GET /permission?directory=` — instance-scoped, unfiltered; the caller
    /// scopes it to the root and its descendants.
    pub(crate) async fn rest_permissions(
        &self,
        directory: &str,
    ) -> Result<Vec<RestPermission>, LaneError> {
        self.get_json("/permission", Some(directory), Route::Approval)
            .await
    }

    /// `GET /question?directory=` — instance-scoped, same scoping rule.
    pub(crate) async fn rest_questions(
        &self,
        directory: &str,
    ) -> Result<Vec<RestQuestion>, LaneError> {
        self.get_json("/question", Some(directory), Route::Approval)
            .await
    }
}

/// `/session/{id}` plus trailing segments, percent-encoding the id so a hostile
/// id cannot escape its path segment.
fn session_path(id: &str, tail: &[&str]) -> String {
    let mut p = format!("/session/{}", encode_segment(id));
    for seg in tail {
        p.push('/');
        p.push_str(seg);
    }
    p
}

/// Percent-encodes everything outside the unreserved set. Hand-rolled rather
/// than taking a dependency: opencode ids are `ses_…`/`per_…`/`que_…` and never
/// need it, but an id that reached us from a remote roost tab is untrusted
/// input and a bare `/` in it would re-address the request.
fn encode_segment(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// Everything reqwest reports before a response head is a dial/transport
/// failure: nothing to talk to. The contract renders that as a quiet
/// unreachable row, so it must never come back as `Failed`.
fn dial_error(path: &str, e: &reqwest::Error) -> LaneError {
    LaneError::Unavailable(format!("opencode {path}: {e}"))
}

/// Reads the body and applies the status table.
async fn check_status(
    path: &str,
    route: Route,
    resp: reqwest::Response,
) -> Result<String, LaneError> {
    let status = resp.status();
    // The body is read even on success: `check_status` is the single place a
    // response is consumed, and a 2xx body is what `get_json` decodes.
    match read_body_capped(path, resp).await {
        Ok(body) if status.is_success() => Ok(body),
        Ok(body) => Err(map_status(status, path, route, &body)),
        Err(e) if status.is_success() => Err(e),
        // A non-2xx whose body could not be read still has an actionable
        // STATUS, and the table is what the caller branches on.
        Err(_) => Err(map_status(status, path, route, "")),
    }
}

/// Consumes a response body under [`MAX_REST_BYTES`], chunk by chunk.
///
/// Two refusals, in order:
///
/// - a declared `Content-Length` past the cap is refused before a single body
///   byte is read;
/// - an undeclared (or lying) body is refused the moment the accumulated total
///   would cross the cap, so at most one chunk past it is ever held.
///
/// This is why `.text()` is not used: it collects the WHOLE body before
/// anything can look at it, and the 5 s timeout does not bound what fits in 5 s
/// over loopback.
async fn read_body_capped(path: &str, mut resp: reqwest::Response) -> Result<String, LaneError> {
    if let Some(len) = resp.content_length() {
        if len > MAX_REST_BYTES as u64 {
            return Err(over_cap(path, len));
        }
    }
    let mut buf: Vec<u8> = Vec::new();
    loop {
        let next = resp.chunk().await.map_err(|e| {
            LaneError::Unavailable(format!("opencode {path}: reading the body: {e}"))
        })?;
        let Some(chunk) = next else { break };
        if buf.len() + chunk.len() > MAX_REST_BYTES {
            return Err(over_cap(path, (buf.len() + chunk.len()) as u64));
        }
        buf.extend_from_slice(&chunk);
    }
    // Lossy, which is what `.text()` did: a body this build cannot decode is a
    // decode failure to report, not a transport one.
    Ok(String::from_utf8_lossy(&buf).into_owned())
}

fn over_cap(path: &str, seen: u64) -> LaneError {
    LaneError::Failed(format!(
        "opencode {path}: response body of at least {seen} bytes exceeds the {MAX_REST_BYTES}-byte cap"
    ))
}

/// opencode's two error body shapes, read for the one thing the contract needs
/// from them: a name and a human message.
///
/// The effect-HTTP shape is `{"_tag": "InvalidRequestError", "message": "…"}`;
/// the named-error shape is `{"name": "MessageAbortedError", "data": {"message":
/// "…"}}`. Both are optional everywhere — an error body this build cannot read
/// degrades to `Failed`, never to a decode failure.
#[derive(Debug, Default, Deserialize)]
struct OcErrorBody {
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(default, rename = "_tag", deserialize_with = "null_default")]
    tag: String,
    #[serde(default, deserialize_with = "null_default")]
    message: String,
    #[serde(default, deserialize_with = "null_default")]
    data: OcErrorData,
}

#[derive(Debug, Default, Deserialize)]
struct OcErrorData {
    #[serde(default, deserialize_with = "null_default")]
    message: String,
}

impl OcErrorBody {
    fn parse(body: &str) -> Option<OcErrorBody> {
        let parsed: OcErrorBody = serde_json::from_str(body).ok()?;
        // An empty object is not an error body worth reporting.
        (!parsed.error_name().is_empty() || !parsed.error_message().is_empty()).then_some(parsed)
    }

    fn error_name(&self) -> &str {
        if self.name.is_empty() {
            &self.tag
        } else {
            &self.name
        }
    }

    fn error_message(&self) -> &str {
        if self.message.is_empty() {
            &self.data.message
        } else {
            &self.message
        }
    }

    /// Whether the named condition is "not right now" rather than "not ever":
    /// a turn that was aborted, a session that is busy. These are the bodies the
    /// contract maps to [`LaneError::NotAccepting`] — the caller retries, it
    /// does not surface a schema complaint.
    fn is_not_accepting(&self) -> bool {
        let name = self.error_name();
        name.contains("Aborted") || name.contains("Busy") || name.contains("Conflict")
    }
}

/// The status table. Kept as a free function so the tests can assert every arm
/// without a live server.
fn map_status(status: reqwest::StatusCode, path: &str, route: Route, body: &str) -> LaneError {
    if status == reqwest::StatusCode::UNAUTHORIZED {
        return LaneError::Unauthorized;
    }
    if status == reqwest::StatusCode::NOT_FOUND {
        return match route {
            Route::Session => LaneError::UnknownSession,
            Route::Approval => LaneError::UnknownApproval,
        };
    }
    if status == reqwest::StatusCode::CONFLICT {
        return LaneError::NotAccepting;
    }
    if status.is_client_error() {
        if let Some(err) = OcErrorBody::parse(body) {
            if err.is_not_accepting() {
                return LaneError::NotAccepting;
            }
            let msg = err.error_message();
            let msg = if msg.is_empty() {
                err.error_name()
            } else {
                msg
            };
            return LaneError::BadRequest(msg.to_string());
        }
    }
    LaneError::Failed(format!(
        "opencode {path}: status {}{}",
        status.as_u16(),
        first_line(body)
    ))
}

/// A one-line, bounded echo of an unrecognized error body — enough to debug
/// with, not enough to paste a page of HTML into a log line.
fn first_line(body: &str) -> String {
    let line = body.lines().next().unwrap_or_default().trim();
    if line.is_empty() {
        return String::new();
    }
    let bounded: String = line.chars().take(200).collect();
    format!(": {bounded}")
}

// ---- REST DTOs (tolerant, the fold's Go-shaped null discipline throughout) ----

/// `GET /session` / `GET /session/{id}` / `GET /session/{id}/children`.
#[derive(Debug, Default, Clone, Deserialize)]
pub(crate) struct RestSession {
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) id: String,
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) title: String,
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) directory: String,
    #[serde(default, rename = "parentID", deserialize_with = "null_default")]
    pub(crate) parent_id: String,
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) time: RestSessionTime,
}

#[derive(Debug, Default, Clone, Deserialize)]
pub(crate) struct RestSessionTime {
    /// Epoch millis of the last change opencode observed. `created` is
    /// deliberately not decoded: the contract's session row carries one
    /// timestamp ([`LaneSession::last_change_unix_ms`]) and an unread field is
    /// just wire surface to keep true.
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) updated: i64,
}

/// One `{info, parts}` entry of `GET /session/{id}/message`. Both halves stay
/// RAW so the synthesized seed envelope carries exactly the bytes opencode
/// served — a seeded row is then byte-identical to a live one.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct RestMessage {
    #[serde(default, deserialize_with = "raw_opt")]
    pub(crate) info: Option<Box<RawValue>>,
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) parts: Vec<Box<RawValue>>,
}

/// One entry of `GET /session/status`'s `{sessionID: status}` map.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct RestStatus {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    pub(crate) typ: String,
}

/// One `GET /permission` entry.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct RestPermission {
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) id: String,
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    pub(crate) session_id: String,
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) permission: String,
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) patterns: Vec<String>,
    #[serde(default, deserialize_with = "raw_opt")]
    pub(crate) metadata: Option<Box<RawValue>>,
}

/// One `GET /question` entry.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct RestQuestion {
    #[serde(default, deserialize_with = "null_default")]
    pub(crate) id: String,
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    pub(crate) session_id: String,
    #[serde(default, deserialize_with = "raw_opt")]
    pub(crate) questions: Option<Box<RawValue>>,
}

// ---- session-row assembly, shared by `sessions()`/`session()`/the watcher ----

/// The activity a cheap `/session/status` poll can report.
///
/// **Never `NeedsApproval`** — a status poll cannot see an approval, which is
/// why [`LaneSession::activity`]'s doc tells a client to read
/// `pending_approvals` for "is it blocked on me". `None` (the read failed)
/// degrades to [`RcActivity::Unknown`] rather than to a claim.
pub(crate) fn activity_from_status(status: Option<&RestStatus>, status_known: bool) -> RcActivity {
    if !status_known {
        return RcActivity::Unknown;
    }
    match status {
        // opencode omits idle sessions from the map.
        None => RcActivity::Idle,
        Some(st) if st.typ == "busy" || st.typ == "retry" => RcActivity::Working,
        Some(_) => RcActivity::Idle,
    }
}

/// A [`LaneSession`] from a REST row plus a polled activity.
pub(crate) fn lane_session(
    s: &RestSession,
    activity: RcActivity,
    pending_approvals: u32,
    approximate: bool,
) -> LaneSession {
    LaneSession {
        id: s.id.clone(),
        title: s.title.clone(),
        cwd: s.directory.clone(),
        activity,
        pending_approvals,
        approximate,
        parent_id: (!s.parent_id.is_empty()).then(|| s.parent_id.clone()),
        last_change_unix_ms: (s.time.updated > 0).then_some(s.time.updated),
    }
}

// ---- the contract ----

#[async_trait::async_trait]
impl AgentLane for OpencodeClient {
    /// Pinned by plan 015 §3.1: opencode has no interject (`prompt_async` joins
    /// the running turn, it does not preempt it) and no resumable history
    /// cursor (every `history` refolds from the top).
    fn capabilities(&self) -> LaneCapabilities {
        LaneCapabilities {
            kind: "opencode".to_string(),
            interject: false,
            create: true,
            cancel: true,
            approvals: true,
            history_cursor: false,
        }
    }

    /// Every ROOT session in the server's global store.
    ///
    /// Activity comes from one `/session/status` read per DISTINCT directory —
    /// that route is instance-scoped, so a single un-scoped read would report
    /// only the server process's own cwd. Rows are `approximate: true`: this is
    /// a poll, not a fold, and it cannot see approvals.
    async fn sessions(&self) -> Result<Vec<LaneSession>, LaneError> {
        let all = self.rest_sessions().await?;
        let roots: Vec<RestSession> = all.into_iter().filter(|s| s.parent_id.is_empty()).collect();

        // BTreeMap so the per-directory reads are issued in a deterministic
        // order — a test asserting which directories were consulted should not
        // depend on hash iteration order.
        let dirs: BTreeMap<&str, ()> = roots.iter().map(|s| (s.directory.as_str(), ())).collect();
        let mut status: BTreeMap<String, (bool, HashMap<String, RestStatus>)> = BTreeMap::new();
        for dir in dirs.keys() {
            // A failed status read degrades that directory's rows to
            // `Unknown`; it must not fail the whole roster.
            let entry = match self.rest_status(dir).await {
                Ok(map) => (true, map),
                Err(_) => (false, HashMap::new()),
            };
            status.insert((*dir).to_string(), entry);
        }

        Ok(roots
            .iter()
            .map(|s| {
                let (known, map) = match status.get(&s.directory) {
                    Some((known, map)) => (*known, Some(map)),
                    None => (false, None),
                };
                let activity = activity_from_status(map.and_then(|m| m.get(&s.id)), known);
                lane_session(s, activity, 0, true)
            })
            .collect())
    }

    /// One session row, with a real approval count (the roster's cheap poll
    /// cannot afford one per row; a single row can).
    async fn session(&self, id: &str) -> Result<LaneSession, LaneError> {
        let s = self.rest_session(id).await?;
        let status = self.rest_status(&s.directory).await;
        let (known, map) = match &status {
            Ok(map) => (true, Some(map)),
            Err(_) => (false, None),
        };
        let activity = activity_from_status(map.and_then(|m| m.get(id)), known);
        let pending = self.approvals(id).await.map(|a| a.len()).unwrap_or(0);
        Ok(lane_session(&s, activity, pending as u32, true))
    }

    /// Refolds the transcript from the top through a FRESH ring (seq from 1)
    /// and returns its tail.
    ///
    /// `cursor` is ignored — `history_cursor: false` — and the page is the most
    /// recent `limit` rows, so a client renders the end of the conversation
    /// rather than its beginning. `truncated` is true whenever the client is
    /// therefore NOT holding the whole history.
    async fn history(
        &self,
        id: &str,
        _cursor: Option<&str>,
        limit: u32,
    ) -> Result<LaneHistory, LaneError> {
        let msgs = self.rest_messages(id).await?;
        let mut fold = OpencodeFold::new();
        for raw in watcher::seed_message_envelopes(id, &msgs) {
            fold.apply_line(&raw);
        }
        let mut ring = MessageRing::new();
        let now = now_utc().timestamp_millis();
        for m in fold.drain_messages() {
            ring.append(m, now);
        }
        let (messages, truncated) = ring.page(limit);
        Ok(LaneHistory {
            messages,
            truncated,
            cursor: None,
        })
    }

    /// `POST /session?directory=<cwd>` then `prompt_async` — opencode takes the
    /// directory as a QUERY parameter on create, not in the body.
    async fn create(&self, cwd: &str, text: &str) -> Result<LaneSession, LaneError> {
        let body = self
            .post_json("/session", Some(cwd), Route::Session, json!({}))
            .await?;
        let created: RestSession = serde_json::from_str(&body)
            .map_err(|e| LaneError::Failed(format!("decoding POST /session: {e}")))?;
        if created.id.is_empty() {
            return Err(LaneError::Failed(
                "POST /session returned no session id".to_string(),
            ));
        }
        self.send(&created.id, text, SendMode::Queue).await?;
        // `Working` is asserted rather than polled: a prompt was just accepted,
        // and a `/session/status` read this instant would race the runner and
        // report idle. `approximate: true` is what says so.
        Ok(lane_session(&created, RcActivity::Working, 0, true))
    }

    /// [`SendMode::Queue`] is `prompt_async` (204). [`SendMode::Interject`] is
    /// refused rather than silently downgraded — `capabilities().interject` is
    /// false and a client that ignored it must see the refusal.
    async fn send(&self, id: &str, text: &str, mode: SendMode) -> Result<(), LaneError> {
        if mode == SendMode::Interject {
            return Err(LaneError::NotAccepting);
        }
        self.post_json(
            &session_path(id, &["prompt_async"]),
            None,
            Route::Session,
            json!({ "parts": [{ "type": "text", "text": text }] }),
        )
        .await?;
        Ok(())
    }

    async fn cancel(&self, id: &str) -> Result<(), LaneError> {
        self.post_json(
            &session_path(id, &["abort"]),
            None,
            Route::Session,
            json!({}),
        )
        .await?;
        Ok(())
    }

    /// Everything open on `id` **and its descendants** — a child's approval
    /// blocks the same agent, so it belongs on the parent's panel.
    ///
    /// Both halves are read independently: a failed `/question` read must not
    /// hide the permissions that DID load, and vice versa. Both failing is the
    /// error.
    async fn approvals(&self, id: &str) -> Result<Vec<LaneApproval>, LaneError> {
        let root = self.rest_session(id).await?;
        let scope = self.approval_scope(id).await;

        let perms = self.rest_permissions(&root.directory).await;
        let questions = self.rest_questions(&root.directory).await;
        if let (Err(pe), Err(_)) = (&perms, &questions) {
            return Err(pe.clone());
        }
        let fold = seeded_fold(&scope, perms.as_deref().ok(), questions.as_deref().ok());
        Ok(fold.pending_approvals())
    }

    /// Answers an approval on opencode's LIVE routes:
    /// `POST /permission/{requestID}/reply {reply}` and
    /// `POST /question/{requestID}/reply {answers}` / `…/reject`. Both are
    /// id-addressed and global — `requestID` is unique across sessions, which is
    /// what lets a DESCENDANT's approval be answered from the root's panel.
    ///
    /// `id` (the session) never reaches the wire, but it is not decoration
    /// either: it is the SCOPE the addressed approval is resolved inside
    /// ([`OpencodeClient::resolve_approval`]), which is what keeps this
    /// session's panel from answering a SIBLING session's request. It is also
    /// the pin the caller asserts and the fake's guard checks.
    async fn answer(
        &self,
        id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<(), LaneError> {
        // The double-tap gate, before anything reaches the wire. See
        // [`AnswerLedger`]. The claim is an RAII guard so that dropping THIS
        // future mid-request releases it — see [`AnswerClaim`].
        let claim = self.claim_answer(approval_id)?;
        let result = self.answer_inner(id, approval_id, answer).await;
        claim.settle(result.is_ok());
        result
    }

    /// Opens a live stream for one session. See [`crate::watcher`] for the
    /// generation bracket and the seed order.
    async fn subscribe(
        &self,
        id: &str,
        cursor: Option<String>,
    ) -> Result<LaneSubscription, LaneError> {
        // Resolved ONCE and cached on the subscription: `/event` is
        // instance-scoped and the pump cannot open it without the directory.
        // A failure here is the caller's — a 404 is `UnknownSession`, a dead
        // server is `Unavailable` — rather than a `Down` the caller has to
        // wait for.
        let session = self.rest_session(id).await?;
        Ok(watcher::spawn(
            self.clone(),
            id.to_string(),
            session.directory,
            cursor,
        ))
    }
}

impl OpencodeClient {
    fn ledger(&self) -> std::sync::MutexGuard<'_, AnswerLedger> {
        lock_ledger(&self.answered)
    }

    /// Take the claim, or say why it cannot be taken. The returned guard
    /// releases the id if it is dropped without being settled.
    fn claim_answer(&self, approval_id: &str) -> Result<AnswerClaim, LaneError> {
        self.ledger().claim(approval_id)?;
        Ok(AnswerClaim {
            ledger: std::sync::Arc::clone(&self.answered),
            id: approval_id.to_string(),
            settled: false,
        })
    }

    /// POST one permission decision to the live reply route.
    ///
    /// The `once|always|reject` table lives here and nowhere else: both
    /// [`LaneAnswer::Permission`] (the semantic answer) and
    /// [`LaneAnswer::Choice`] (an offered option id) resolve to a
    /// [`LaneDecision`] and land on this one call, so the two can never drift
    /// into sending different bodies for the same decision.
    async fn reply_permission(&self, seg: &str, decision: LaneDecision) -> Result<(), LaneError> {
        let reply = match decision {
            LaneDecision::AllowOnce => "once",
            LaneDecision::AllowAlways => "always",
            LaneDecision::Reject => "reject",
        };
        self.post_json(
            &format!("/permission/{seg}/reply"),
            None,
            Route::Approval,
            json!({ "reply": reply }),
        )
        .await?;
        Ok(())
    }

    /// The ADDRESSED approval, resolved **inside `id`'s own scope** — the root
    /// session plus its transitive descendants, computed exactly the way
    /// [`AgentLane::approvals`] computes it (one helper,
    /// [`OpencodeClient::approval_scope`], for both).
    ///
    /// **Why the scope, and not the whole directory.** opencode has no by-id GET
    /// for a request, so the only lookup available is the two directory-wide
    /// lists — and those carry SIBLING sessions' approvals, since an instance is
    /// per DIRECTORY and a directory holds many unrelated roots. An unscoped
    /// lookup would therefore let this session's panel answer a sibling's
    /// request: it would resolve, translate against the sibling's options, and
    /// POST to a global id-addressed route that has no idea whose panel asked.
    /// The scope filter is what refuses that, and [`crate::testing`]'s pin guard
    /// records it as a violation if it ever regresses.
    ///
    /// **A failed list PROPAGATES.** A half-read directory cannot say "unknown":
    /// the id it did not see may simply live in the half that failed, and
    /// [`LaneError::UnknownApproval`] reads to a client as "somebody already
    /// answered it". This is the one place the two lists' independent authority
    /// (which is what [`AgentLane::approvals`] wants — see `seed_approvals`)
    /// would be actively wrong.
    ///
    /// An id in NEITHER list is [`LaneError::UnknownApproval`]. An id in BOTH is
    /// refused as ambiguous rather than routed by a guess — the fold tracks an
    /// approval by (kind, id) precisely because an id alone does not identify
    /// one, and guessing the kind is how a permission reply ends up resolving a
    /// question.
    ///
    /// **Cost: four GETs per answer** on a childless session — the root session,
    /// its `/children` read (one more per descendant level), and the two lists.
    /// gx pays a single GET here, because gx HAS a by-id route. What it buys is
    /// what gx's re-read buys: the answer is translated against the options the
    /// agent ACTUALLY offered, on an approval this session owns.
    ///
    /// **TOCTOU**: between this read and the POST the agent's own TUI may answer
    /// the same request. That is the same window gx accepts, and it closes the
    /// same way — the reply route refuses a request that is no longer live (404
    /// → [`LaneError::UnknownApproval`]), so the loser of the race is told and
    /// nothing is decided twice.
    pub(crate) async fn resolve_approval(
        &self,
        id: &str,
        approval_id: &str,
    ) -> Result<LaneApproval, LaneError> {
        let root = self.rest_session(id).await?;
        let scope = self.approval_scope(id).await;
        // `?` on each, in order: either half failing is the whole lookup's
        // error.
        let perms = self.rest_permissions(&root.directory).await?;
        let questions = self.rest_questions(&root.directory).await?;

        let fold = seeded_fold(&scope, Some(&perms), Some(&questions));
        // Kind-agnostic on purpose: the ANSWER names a shape and the approval
        // names a kind, and this is the one place the two are compared.
        let mut found = fold.approvals_for_id(approval_id);
        match found.len() {
            0 => Err(LaneError::UnknownApproval),
            1 => Ok(found.remove(0)),
            _ => Err(LaneError::BadRequest(format!(
                "ambiguous approval id: {approval_id} is open as {}",
                found
                    .iter()
                    .map(|a| a.kind.as_str())
                    .collect::<Vec<_>>()
                    .join(" and ")
            ))),
        }
    }

    /// The wire half of [`AgentLane::answer`], past the double-tap gate.
    ///
    /// The addressed approval is resolved FIRST
    /// ([`OpencodeClient::resolve_approval`]), and every refusal below is
    /// decided against THAT — before anything reaches the wire. An answer whose
    /// shape does not match the approval it names is a
    /// [`LaneError::BadRequest`], never a POST to whichever route happens to
    /// accept that body: opencode's two answer routes are id-addressed and
    /// global, so a mis-routed answer is not a type error the server catches,
    /// it is a decision recorded against the wrong request.
    async fn answer_inner(
        &self,
        id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<(), LaneError> {
        let approval = self.resolve_approval(id, approval_id).await?;
        let is_permission = approval.kind == LaneApprovalKind::Permission;
        let is_question = approval.kind == LaneApprovalKind::Question;
        let seg = encode_segment(approval_id);
        match answer {
            // The SEMANTIC answer, resolved through the contract's ONE
            // implementation of the by-kind rule (`option_for`, correction 3)
            // against the options THIS approval offered. That is also what
            // refuses a decision aimed at a question, with no kind check of its
            // own: `LaneApproval::kind` selects which list is populated, so a
            // question's top-level options are empty and no decision can match.
            LaneAnswer::Permission { decision } => {
                approval
                    .option_for(decision)
                    .ok_or_else(|| unresolvable_decision(&approval, decision))?;
                self.reply_permission(&seg, decision).await?;
            }
            // `Choice` names an OFFERED option id, matched against what this
            // approval actually offered rather than against opencode's fixed
            // three — which is what used to let one of those three ids, posted
            // at a QUESTION, reach the permission route. A question's options
            // are its answer LABELS and are chosen positionally, so they arrive
            // as `LaneAnswer::Question`, never here.
            LaneAnswer::Choice { option_id } => {
                if !is_permission {
                    return Err(wrong_kind(&approval, "answer it with `question`"));
                }
                let offered = approval
                    .options
                    .iter()
                    .find(|o| o.id == option_id)
                    .ok_or_else(|| {
                        LaneError::BadRequest(format!(
                            "opencode did not offer the option {option_id:?} on {approval_id}"
                        ))
                    })?;
                self.reply_permission(&seg, decision_of(offered)?).await?;
            }
            // Positional in, positional out — opencode files a question's
            // answers by index and has no key. Free text is one MORE entry in
            // that question's list, which is exactly what opencode's own TUI
            // posts for a typed answer: `QuestionReply.answers` is "an array of
            // selected labels" and a custom answer is a label the ask did not
            // offer. So the smuggle the panel used to do lives here, where the
            // agent's shape is known, and the wire is unchanged.
            LaneAnswer::Question {
                answers,
                custom_text,
            } => {
                if !is_question {
                    return Err(wrong_kind(
                        &approval,
                        "answer it with `permission` or `choice`",
                    ));
                }
                // Against the RESOLVED approval's questions, before anything
                // reaches the wire: an over-long vector, or text aimed at a
                // question that does not take it, is refused here.
                let replies =
                    normalize_question_answer(&approval.questions, &answers, &custom_text)?;
                let answers: Vec<Vec<String>> = replies
                    .into_iter()
                    .map(|r| {
                        let mut labels = r.labels;
                        // A question with neither stays `[]` — opencode reads an
                        // empty list as unanswered, and inventing an entry would
                        // answer a question the human skipped.
                        labels.extend(r.text);
                        labels
                    })
                    .collect();
                self.post_json(
                    &format!("/question/{seg}/reply"),
                    None,
                    Route::Approval,
                    json!({ "answers": answers }),
                )
                .await?;
            }
            // A bare reject names no kind, so it is the QUESTION route's "no
            // thanks". A permission's "no" is one of its three offered
            // options and arrives as `Permission { Reject }` or as the
            // equivalent `Choice`, which is why a permission is refused here
            // rather than silently posted to `/question/{id}/reject`.
            LaneAnswer::Reject => {
                if !is_question {
                    return Err(wrong_kind(
                        &approval,
                        "refuse it with `permission` (decision `reject`) or `choice`",
                    ));
                }
                self.post_json(
                    &format!("/question/{seg}/reject"),
                    None,
                    Route::Approval,
                    json!({}),
                )
                .await?;
            }
            // The escape hatch for an approval kind this build cannot name: the
            // body is the caller's, verbatim, on the permission reply route
            // (the only one that takes a free-form object). It is pinned to a
            // resolved PERMISSION — opencode has no unnameable kind, so a `Raw`
            // aimed at anything else is a caller mistake, and honoring one on a
            // question would post an arbitrary body to the permission route
            // under a question's id.
            LaneAnswer::Raw { json } => {
                if !is_permission {
                    return Err(wrong_kind(
                        &approval,
                        "`raw` answers only an approval kind this build cannot name, and opencode has none",
                    ));
                }
                let body: serde_json::Value = serde_json::from_str(&json)
                    .map_err(|e| LaneError::BadRequest(format!("raw answer is not JSON: {e}")))?;
                self.post_json(
                    &format!("/permission/{seg}/reply"),
                    None,
                    Route::Approval,
                    body,
                )
                .await?;
            }
        }
        Ok(())
    }

    /// The root plus its descendants **transitively** — the ids whose APPROVALS
    /// surface on the root's panel.
    ///
    /// A breadth-first walk of `/session/{id}/children`, not one level: a
    /// sub-agent can spawn a sub-agent, live `session.created` frames already
    /// grow the watcher's scope that way ("a `session.created` whose `parentID`
    /// is in the set"), and a reseed that restored only the immediate children
    /// would silently drop a grandchild's pending approvals — and every later
    /// approval frame for it, since the scope is also the live filter.
    ///
    /// Bounded and cycle-safe, because opencode is untrusted input: the visited
    /// set makes a `parentID` cycle terminate, and
    /// [`MAX_DESCENDANT_DEPTH`]/[`MAX_DESCENDANT_SESSIONS`] make an
    /// honest-but-huge tree terminate (the total is also the ceiling on how many
    /// `/children` requests one seed issues).
    ///
    /// A failed `/children` read degrades to "no children BELOW that node"
    /// rather than failing the call: missing a child's approval is a gap,
    /// refusing to answer is an outage.
    pub(crate) async fn approval_scope(&self, root: &str) -> HashSet<String> {
        let mut scope = HashSet::new();
        scope.insert(root.to_string());
        let mut frontier = vec![root.to_string()];
        for _depth in 0..MAX_DESCENDANT_DEPTH {
            if frontier.is_empty() || scope.len() >= MAX_DESCENDANT_SESSIONS {
                break;
            }
            let mut next = Vec::new();
            for parent in std::mem::take(&mut frontier) {
                if scope.len() >= MAX_DESCENDANT_SESSIONS {
                    break;
                }
                let Ok(children) = self.rest_children(&parent).await else {
                    continue;
                };
                for c in children {
                    if c.id.is_empty() || scope.len() >= MAX_DESCENDANT_SESSIONS {
                        continue;
                    }
                    // `insert` returning false is the cycle guard AND the
                    // diamond guard: an id already in scope is never expanded
                    // twice.
                    if scope.insert(c.id.clone()) {
                        next.push(c.id);
                    }
                }
            }
            frontier = next;
        }
        scope
    }
}

// ---- the approval lookup's pure half ----

/// A FRESH fold holding everything the two lists say is open **inside `scope`**
/// — the one place a REST approval snapshot is turned into
/// [`LaneApproval`]s, so [`AgentLane::approvals`] (which lists them) and
/// [`OpencodeClient::resolve_approval`] (which addresses one) cannot decode the
/// same request into two different DTOs.
///
/// `None` for a half means "that read failed and this half says nothing"; the
/// two callers differ only in whether they tolerate one
/// ([`OpencodeClient::resolve_approval`] does not — see its doc).
fn seeded_fold(
    scope: &HashSet<String>,
    permissions: Option<&[RestPermission]>,
    questions: Option<&[RestQuestion]>,
) -> OpencodeFold {
    let mut fold = OpencodeFold::new();
    let seed = watcher::seed_approvals(scope, permissions, questions);
    for (_session_id, raw) in seed.envelopes {
        fold.apply_line(&raw);
    }
    fold
}

/// "That approval is not the shape this answer is for." One spelling for all
/// four kind refusals, so each names the id AND the kind that was actually
/// resolved — a client that sent the wrong variant needs both to fix it.
fn wrong_kind(approval: &LaneApproval, remedy: &str) -> LaneError {
    LaneError::BadRequest(format!(
        "{} is a {}; {remedy}",
        approval.id,
        approval.kind.as_str()
    ))
}

/// [`LaneAnswer::Permission`]'s refusal: the decision names a semantic option
/// kind, and this approval offers none of it.
///
/// It names opencode because the contract's [`LaneApproval::option_for`]
/// deliberately does not — it answers `None` and leaves the message to whoever
/// knows which agent is being talked to. Unlike gx, opencode cannot offer the
/// AMBIGUOUS case (its three options carry three distinct kinds), so there is
/// no second sentence for it here.
fn unresolvable_decision(approval: &LaneApproval, decision: LaneDecision) -> LaneError {
    let wanted = match decision {
        LaneDecision::AllowOnce => option_kind::ALLOW_ONCE,
        LaneDecision::AllowAlways => option_kind::ALLOW_ALWAYS,
        LaneDecision::Reject => option_kind::REJECT_ONCE,
    };
    LaneError::BadRequest(format!(
        "opencode offered no option of kind {wanted} on {} (a {})",
        approval.id,
        approval.kind.as_str()
    ))
}

/// The [`LaneDecision`] an OFFERED option resolves to — **by its ACP
/// [`LaneApprovalOption::kind`], never by its id** (the module doc's correction
/// 3, in the direction `option_for` does not cover).
///
/// This is what a [`LaneAnswer::Choice`] goes through, so the two answer shapes
/// reach [`OpencodeClient::reply_permission`] via the same table. `reject*`
/// collapses to `reject` because opencode has one refusal and no
/// reject-always; an option whose kind is absent or unknown is refused rather
/// than guessed — unreachable for opencode's own three options, and not this
/// function's job to assume.
fn decision_of(option: &LaneApprovalOption) -> Result<LaneDecision, LaneError> {
    match option.kind.as_deref() {
        Some(option_kind::ALLOW_ONCE) => Ok(LaneDecision::AllowOnce),
        Some(option_kind::ALLOW_ALWAYS) => Ok(LaneDecision::AllowAlways),
        Some(k) if k.starts_with("reject") => Ok(LaneDecision::Reject),
        other => Err(LaneError::BadRequest(format!(
            "opencode cannot reply the option {} (kind {}) to a permission",
            option.id,
            other.unwrap_or("<none>")
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn basic_auth_header_is_rfc7617() {
        let auth = BasicAuth {
            username: "opencode".to_string(),
            password: "hunter2".to_string(),
        };
        assert_eq!(auth.header(), "Basic b3BlbmNvZGU6aHVudGVyMg==");
        assert_eq!(BasicAuth::password("hunter2"), auth);
    }

    #[test]
    fn session_path_encodes_the_id() {
        assert_eq!(session_path("ses_1", &[]), "/session/ses_1");
        assert_eq!(
            session_path("ses_1", &["message"]),
            "/session/ses_1/message"
        );
        // A `/` in an id cannot re-address the request.
        assert_eq!(session_path("a/b", &["abort"]), "/session/a%2Fb/abort");
    }

    #[test]
    fn status_table_maps_every_arm() {
        use reqwest::StatusCode;
        let s = Route::Session;
        assert_eq!(
            map_status(StatusCode::UNAUTHORIZED, "/x", s, ""),
            LaneError::Unauthorized
        );
        assert_eq!(
            map_status(StatusCode::NOT_FOUND, "/x", s, ""),
            LaneError::UnknownSession
        );
        assert_eq!(
            map_status(StatusCode::NOT_FOUND, "/x", Route::Approval, ""),
            LaneError::UnknownApproval
        );
        assert_eq!(
            map_status(StatusCode::CONFLICT, "/x", s, ""),
            LaneError::NotAccepting
        );
        // 4xx + an opencode error body → BadRequest carrying ITS message.
        assert_eq!(
            map_status(
                StatusCode::BAD_REQUEST,
                "/x",
                s,
                r#"{"_tag":"InvalidRequestError","message":"parts is required"}"#
            ),
            LaneError::BadRequest("parts is required".to_string())
        );
        // The named-error shape, and a name that means "not right now".
        assert_eq!(
            map_status(
                StatusCode::BAD_REQUEST,
                "/x",
                s,
                r#"{"name":"MessageAbortedError","data":{"message":"aborted"}}"#
            ),
            LaneError::NotAccepting
        );
        // A 4xx with no recognizable body is the loud residue, not a guess.
        assert!(matches!(
            map_status(StatusCode::BAD_REQUEST, "/x", s, "<html>"),
            LaneError::Failed(_)
        ));
        assert!(matches!(
            map_status(StatusCode::INTERNAL_SERVER_ERROR, "/x", s, ""),
            LaneError::Failed(_)
        ));
    }

    #[test]
    fn activity_from_status_never_claims_on_a_failed_read() {
        assert_eq!(activity_from_status(None, false), RcActivity::Unknown);
        // Absent from a 200 map == idle (opencode omits idle sessions).
        assert_eq!(activity_from_status(None, true), RcActivity::Idle);
        for typ in ["busy", "retry"] {
            let st = RestStatus { typ: typ.into() };
            assert_eq!(activity_from_status(Some(&st), true), RcActivity::Working);
        }
        let st = RestStatus { typ: "idle".into() };
        assert_eq!(activity_from_status(Some(&st), true), RcActivity::Idle);
    }

    #[test]
    fn url_appends_the_directory_only_when_given() {
        let c = OpencodeClient::new("http://127.0.0.1:4096/".parse().unwrap(), None).unwrap();
        assert_eq!(
            c.url("/session/ses_1", None).unwrap().as_str(),
            "http://127.0.0.1:4096/session/ses_1"
        );
        assert_eq!(
            c.url("/event", Some("/tmp/a b")).unwrap().as_str(),
            "http://127.0.0.1:4096/event?directory=%2Ftmp%2Fa+b"
        );
    }
}
