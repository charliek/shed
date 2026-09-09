//! An in-process fake gx lane, for tests.
//!
//! The `shed_opencode::testing::FakeOpencode` pattern, which is itself
//! `shed_core::roost::testing::FakeRoost`'s: a tokio listener, one connection
//! handler, everything a test wants to script behind one lock, exported through
//! the non-default `test-support` feature so a shipped binary never carries it.
//!
//! It is a **test double, not a leader**: no agent, no sessions of its own, no
//! persistence. What it is faithful about is the wire — the routes this crate
//! calls, their shapes, the token gate in front of everything but `healthz`,
//! gx's negative-`offset` history paging, its error envelope, and its approval
//! lifecycle (`pending → submitted → resolved`, with `409` on a second answer).
//!
//! # The three things a test asserts through it
//!
//! - **The request ledger** ([`FakeGx::requests`]) records every request's
//!   method, path, query, whether it carried a bearer, and its `Last-Event-ID`.
//!   That is what proves `healthz` came FIRST and carried no token, that a
//!   mismatch sent no token at all, and (in C3) which cursor a reconnect used.
//! - **The pin guard** holds every session-scoped route to one session id. A
//!   violation is recorded AND answered `500`, so an offending verb can never
//!   look like it worked. A suite that drives this crate correctly leaves
//!   [`FakeGx::violations`] empty.
//! - **The error injector** ([`FakeGx::fail`]) answers any of gx's eight codes
//!   on any route, which is how the error table is tested row by row without a
//!   leader that can be talked into each one.
//!
//! # What is not here yet
//!
//! `GET …/events` — the SSE stream, its four `Last-Event-ID` resume rules,
//! `reset` injection and listener stop/start — is plan 017 C3. The route is
//! recorded in the ledger and answered `404` in the meantime, so a premature
//! subscribe fails loudly rather than hanging.

use std::collections::{BTreeMap, HashMap};
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt as _, AsyncReadExt as _, AsyncWriteExt as _, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::task::JoinHandle;

/// A 64-lowercase-hex token that is obviously a fixture, so a grep for it in a
/// log, an IPC transcript or a `Debug` string is unambiguous.
pub const SENTINEL_TOKEN: &str = "5e471e15e471e15e471e15e471e15e471e15e471e15e471e15e471e15e471e15";

/// The instance id the fake reports until [`FakeGx::set_instance_id`] rotates
/// it.
pub const DEFAULT_INSTANCE_ID: &str = "facade00facade00facade00facade00";

/// One served request, as the ledger remembers it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RequestRecord {
    pub method: String,
    /// Query string stripped.
    pub path: String,
    pub query: String,
    /// Whether an `Authorization` header was present AT ALL — the assertion
    /// "healthz carried no token" is about this, not about whether it was
    /// right.
    pub had_bearer: bool,
    /// Whether that header matched the fake's token.
    pub bearer_ok: bool,
    /// The `Last-Event-ID` header, for C3's resume assertions.
    pub last_event_id: Option<String>,
    pub body: String,
}

/// One approval the fake is holding.
#[derive(Debug, Clone)]
struct ApprovalEntry {
    resource: Value,
    status: String,
    /// The `response` value of the answer that was accepted, so a test can
    /// assert the exact body a decision produced.
    answered_with: Option<Value>,
}

/// An injected failure: an `(error, status)` pair answered to every request
/// whose path ends with `suffix`.
#[derive(Debug, Clone)]
struct Failure {
    suffix: String,
    status: u16,
    code: String,
    message: String,
}

#[derive(Default)]
struct FakeState {
    instance_id: String,
    token: String,
    version: String,
    sessions: HashMap<String, Value>,
    order: Vec<String>,
    /// session id → its persisted envelopes, oldest first.
    history: HashMap<String, Vec<Value>>,
    /// session id → approval id → entry.
    approvals: HashMap<String, BTreeMap<String, ApprovalEntry>>,
    pin: String,
    violations: Vec<String>,
    requests: Vec<RequestRecord>,
    failures: Vec<Failure>,
    /// Path suffixes whose request gets NO response at all — the connection is
    /// closed after the request is read. A transport failure, not an HTTP one.
    hangups: Vec<String>,
    /// Path suffix → a status whose head promises a body that never arrives.
    truncations: Vec<(String, u16)>,
    next_id: u64,
}

/// An in-process gx stand-in on a loopback TCP port.
pub struct FakeGx {
    state: Arc<Mutex<FakeState>>,
    addr: SocketAddr,
    accept: JoinHandle<()>,
}

impl FakeGx {
    /// Bind and start accepting on an ephemeral loopback port, serving
    /// [`SENTINEL_TOKEN`] and [`DEFAULT_INSTANCE_ID`].
    pub async fn start() -> FakeGx {
        FakeGx::start_with(SENTINEL_TOKEN, DEFAULT_INSTANCE_ID).await
    }

    pub async fn start_with(token: &str, instance_id: &str) -> FakeGx {
        let listener = TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("binding the fake gx listener");
        let addr = listener.local_addr().expect("the fake's address");
        let state = Arc::new(Mutex::new(FakeState {
            instance_id: instance_id.to_string(),
            token: token.to_string(),
            version: "1.0.16+gx.12".to_string(),
            ..FakeState::default()
        }));
        let accept = tokio::spawn({
            let state = Arc::clone(&state);
            async move {
                while let Ok((sock, _)) = listener.accept().await {
                    tokio::spawn(serve(sock, Arc::clone(&state)));
                }
            }
        });
        FakeGx {
            state,
            addr,
            accept,
        }
    }

    /// The URL a [`crate::GxClient`] DIALS. Carries a trailing slash, which is
    /// exactly what makes it not a reported URL.
    pub fn dial_url(&self) -> reqwest::Url {
        format!("http://{}/", self.addr)
            .parse()
            .expect("the fake's dial URL parses")
    }

    /// The URL a discovery record would REPORT: slash-free, and therefore
    /// `loopback_base_url`-clean.
    pub fn reported_url(&self) -> String {
        format!("http://{}", self.addr)
    }

    pub fn addr(&self) -> SocketAddr {
        self.addr
    }

    pub fn token(&self) -> String {
        self.lock().token.clone()
    }

    pub fn instance_id(&self) -> String {
        self.lock().instance_id.clone()
    }

    /// Rotate the leader instance without changing the token — a leader
    /// restart, which is exactly what the pin exists to notice.
    pub fn set_instance_id(&self, id: &str) {
        self.lock().instance_id = id.to_string();
    }

    // ---- scripting ----

    /// Add a session row. `activity` is one of gx's six.
    pub fn add_session(
        &self,
        id: &str,
        title: Option<&str>,
        cwd: &str,
        activity: &str,
        pending_approvals: u32,
        approximate: bool,
    ) {
        let mut st = self.lock();
        st.sessions.insert(
            id.to_string(),
            json!({
                "sessionId": id,
                // Nullable on the real wire, and the fake carries that: an
                // untitled session is `null`, not `""`.
                "title": title,
                "cwd": cwd,
                "activity": activity,
                "resident": true,
                "modelId": "fixture-model",
                "lastChangeUnixMs": 1_788_931_056_811i64,
                "attached": true,
                "pendingApprovals": pending_approvals,
                "approximate": approximate,
            }),
        );
        if !st.order.iter().any(|o| o == id) {
            st.order.push(id.to_string());
        }
    }

    /// Replace a session's persisted transcript (oldest first).
    pub fn set_history(&self, id: &str, updates: Vec<Value>) {
        self.lock().history.insert(id.to_string(), updates);
    }

    /// Add an approval in `pending`.
    pub fn add_approval(&self, session: &str, id: &str, kind: &str, method: &str, request: &Value) {
        let resource = json!({
            "id": id,
            "sessionId": session,
            "kind": kind,
            "method": method,
            "status": "pending",
            "request": request,
            "createdAt": 1_788_931_000_000i64,
            "submittedAt": null,
            "resolvedAt": null,
        });
        self.insert_pending(session, id, resource);
    }

    /// Add the placeholder a `pending_interaction` creates: no method, no
    /// request, nothing to render but the raw body.
    pub fn add_placeholder_approval(&self, session: &str, id: &str) {
        let resource = json!({
            "id": id,
            "sessionId": session,
            "kind": "permission",
            "method": null,
            "status": "pending",
            "request": null,
            "createdAt": 1_788_931_000_000i64,
        });
        self.insert_pending(session, id, resource);
    }

    /// File a freshly built resource as `pending`. The approval lifecycle has
    /// ONE entry point so C3's additions to [`ApprovalEntry`] land in one place
    /// rather than in each scripting method that mints one.
    fn insert_pending(&self, session: &str, id: &str, resource: Value) {
        self.lock()
            .approvals
            .entry(session.to_string())
            .or_default()
            .insert(
                id.to_string(),
                ApprovalEntry {
                    resource,
                    status: "pending".to_string(),
                    answered_with: None,
                },
            );
    }

    /// Move an approval to `resolved` — the TUI answered first, or the agent
    /// moved on. A later answer is then `409 already_resolved`.
    pub fn resolve_approval(&self, session: &str, id: &str) {
        if let Some(entry) = self
            .lock()
            .approvals
            .get_mut(session)
            .and_then(|m| m.get_mut(id))
        {
            entry.status = "resolved".to_string();
            entry.resource["status"] = json!("resolved");
        }
    }

    /// The `response` value the fake accepted for an approval, if any.
    pub fn answered_with(&self, session: &str, id: &str) -> Option<Value> {
        self.lock()
            .approvals
            .get(session)
            .and_then(|m| m.get(id))
            .and_then(|e| e.answered_with.clone())
    }

    // ---- failure injection ----

    /// Answer `status` with gx's `{"error":code,"message":…}` envelope to every
    /// request whose path ends with `suffix`.
    pub fn fail(&self, suffix: &str, status: u16, code: &str, message: &str) {
        self.lock().failures.push(Failure {
            suffix: suffix.to_string(),
            status,
            code: code.to_string(),
            message: message.to_string(),
        });
    }

    /// Answer `status` with a body that is NOT a gx error envelope — the
    /// "unparseable" row of the error table.
    pub fn fail_raw(&self, suffix: &str, status: u16, body: &str) {
        self.lock().failures.push(Failure {
            suffix: suffix.to_string(),
            status,
            // An empty code is the flag for "write `message` verbatim".
            code: String::new(),
            message: body.to_string(),
        });
    }

    pub fn clear_failures(&self) {
        self.lock().failures.clear();
    }

    /// Read the request, record it, then **close the connection without
    /// answering**. reqwest reports that as a send failure — the transport-level
    /// error a dead forward or a restarted leader produces, and one of the two
    /// paths that used to leave a stale epoch pinned.
    pub fn hangup(&self, suffix: &str) {
        self.lock().hangups.push(suffix.to_string());
    }

    /// Answer with a complete head promising 4 KiB of body, then close without
    /// sending it. The status is readable; the body EOFs. The other path that
    /// used to leave a stale epoch pinned — and the one where a status-derived
    /// `Failed` would have hidden a transport failure.
    pub fn truncate_body(&self, suffix: &str, status: u16) {
        self.lock().truncations.push((suffix.to_string(), status));
    }

    pub fn clear_transport_faults(&self) {
        let mut st = self.lock();
        st.hangups.clear();
        st.truncations.clear();
    }

    /// Make `GET …/approvals/{id}` answer with a resource whose OWN `id` and
    /// `sessionId` are somebody else's, while still being served at `id`'s
    /// address. The map key is untouched, so the route still resolves — this is
    /// a server answering an id-addressed GET with the wrong resource.
    pub fn misdirect_approval(&self, session: &str, id: &str, as_id: &str, as_session: &str) {
        if let Some(entry) = self
            .lock()
            .approvals
            .get_mut(session)
            .and_then(|m| m.get_mut(id))
        {
            entry.resource["id"] = json!(as_id);
            entry.resource["sessionId"] = json!(as_session);
        }
    }

    // ---- the guard + the ledger ----

    /// Pin the subscribed session: every session-scoped route must name it.
    pub fn pin(&self, session_id: &str) {
        self.lock().pin = session_id.to_string();
    }

    /// Every recorded pin-guard violation. A correct suite leaves this empty.
    pub fn violations(&self) -> Vec<String> {
        self.lock().violations.clone()
    }

    /// Every request served, in order.
    pub fn requests(&self) -> Vec<RequestRecord> {
        self.lock().requests.clone()
    }

    /// Every request that carried an `Authorization` header — the ledger a
    /// "no token was sent" assertion reads.
    pub fn bearer_requests(&self) -> Vec<RequestRecord> {
        self.lock()
            .requests
            .iter()
            .filter(|r| r.had_bearer)
            .cloned()
            .collect()
    }

    /// The body of the first recorded request whose path ends with `suffix`.
    pub fn body_of(&self, suffix: &str) -> Option<String> {
        self.lock()
            .requests
            .iter()
            .find(|r| r.path.ends_with(suffix) && !r.body.is_empty())
            .map(|r| r.body.clone())
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, FakeState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

impl Drop for FakeGx {
    fn drop(&mut self) {
        self.accept.abort();
    }
}

// ---------------------------------------------------------------------------
// the connection handler
// ---------------------------------------------------------------------------

/// One request per connection (`Connection: close`).
async fn serve(sock: TcpStream, state: Arc<Mutex<FakeState>>) {
    let (read, mut write) = sock.into_split();
    let mut reader = BufReader::new(read);

    let mut request_line = String::new();
    if reader.read_line(&mut request_line).await.unwrap_or(0) == 0 {
        return;
    }
    let mut it = request_line.split_whitespace();
    let method = it.next().unwrap_or_default().to_string();
    let target = it.next().unwrap_or_default().to_string();

    let mut content_length = 0usize;
    let mut authorization: Option<String> = None;
    let mut last_event_id: Option<String> = None;
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line).await.unwrap_or(0) == 0 {
            return;
        }
        let line = line.trim_end_matches(['\r', '\n']);
        if line.is_empty() {
            break;
        }
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        match name.trim().to_ascii_lowercase().as_str() {
            "content-length" => content_length = value.trim().parse().unwrap_or(0),
            "authorization" => authorization = Some(value.trim().to_string()),
            "last-event-id" => last_event_id = Some(value.trim().to_string()),
            _ => {}
        }
    }
    let mut body = vec![0u8; content_length];
    if content_length > 0 && reader.read_exact(&mut body).await.is_err() {
        return;
    }
    let body = String::from_utf8_lossy(&body).to_string();

    let (path, query) = match target.split_once('?') {
        Some((p, q)) => (percent_decode(p), q.to_string()),
        None => (percent_decode(&target), String::new()),
    };

    // The request is RECORDED before any fault is applied, so a test can assert
    // what was attempted even when nothing was answered.
    let outcome = {
        let mut st = state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let bearer_ok = authorization.as_deref() == Some(&format!("Bearer {}", st.token));
        st.requests.push(RequestRecord {
            method: method.clone(),
            path: path.clone(),
            query: query.clone(),
            had_bearer: authorization.is_some(),
            bearer_ok,
            last_event_id: last_event_id.clone(),
            body: body.clone(),
        });
        if st.hangups.iter().any(|h| path.ends_with(h.as_str())) {
            None
        } else if let Some((_, status)) = st
            .truncations
            .iter()
            .find(|(suffix, _)| path.ends_with(suffix.as_str()))
        {
            Some(Err(*status))
        } else {
            Some(Ok(route(&mut st, &method, &path, &query, &body, bearer_ok)))
        }
    };

    match outcome {
        // Hang up: no status line, no headers, nothing.
        None => {}
        // A head promising a body that never arrives.
        Some(Err(status)) => {
            let head = format!(
                "HTTP/1.1 {status} Status\r\nContent-Type: application/json\r\nContent-Length: 4096\r\nConnection: close\r\n\r\n"
            );
            let _ = write.write_all(head.as_bytes()).await;
            let _ = write.flush().await;
        }
        Some(Ok((status, payload))) => {
            let _ = write_response(&mut write, status, &payload).await;
        }
    }
}

fn route(
    st: &mut FakeState,
    method: &str,
    path: &str,
    query: &str,
    body: &str,
    bearer_ok: bool,
) -> (u16, String) {
    // The one unauthenticated route, and it is answered before the token gate
    // so a client that health-checks first never needs a credential to do it.
    if method == "GET" && path == "/v1/healthz" {
        return (
            200,
            json!({
                "ok": true,
                "version": st.version,
                "leaderPid": 4242,
                "instanceId": st.instance_id,
                "build": "gx",
            })
            .to_string(),
        );
    }

    if !bearer_ok {
        // gx deliberately says nothing about WHICH part was wrong.
        return gx_error(401, "unauthorized", "missing or invalid token");
    }

    // Injected failures come after the token gate and before the pin guard: a
    // scripted `not_accepting` must not also be a guard violation.
    if let Some(f) = st
        .failures
        .iter()
        .find(|f| path.ends_with(f.suffix.as_str()))
        .cloned()
    {
        if f.code.is_empty() {
            return (f.status, f.message);
        }
        return gx_error(f.status, &f.code, &f.message);
    }

    if method == "GET" && path == "/v1/sessions" {
        let all: Vec<&Value> = st
            .order
            .iter()
            .filter_map(|id| st.sessions.get(id))
            .collect();
        return (200, json!({ "sessions": all }).to_string());
    }
    if method == "POST" && path == "/v1/sessions" {
        // Global by construction — a create names no session yet, so the pin
        // guard does not apply to it.
        st.next_id += 1;
        let id = format!("01a0fake-0000-7000-8000-00000000{:04}", st.next_id);
        st.sessions.insert(
            id.clone(),
            json!({
                "sessionId": id,
                "title": null,
                "cwd": json_str(body, "cwd"),
                "activity": "working",
                "resident": true,
                "modelId": "fixture-model",
                "lastChangeUnixMs": 1_788_931_060_000i64,
                "attached": false,
                "pendingApprovals": 0,
                "approximate": false,
            }),
        );
        st.order.push(id.clone());
        // gx's create is `201 {sessionId}`, and the pin follows the created
        // session so the row read that comes next is not a violation.
        if !st.pin.is_empty() {
            st.pin = id.clone();
        }
        return (201, json!({ "sessionId": id }).to_string());
    }

    let Some(rest) = path.strip_prefix("/v1/sessions/") else {
        return gx_error(404, "unknown_session", "no such route");
    };
    let mut parts = rest.split('/');
    let id = parts.next().unwrap_or_default().to_string();
    let tail: Vec<&str> = parts.collect();

    // The pin guard: every session-scoped route must name the pinned session.
    if !st.pin.is_empty() && id != st.pin {
        st.violations.push(format!(
            "{method} {path} addressed session {id}, not the pinned {}",
            st.pin
        ));
        // A violation can never look successful.
        return (
            500,
            r#"{"error":"pin_guard","message":"violation"}"#.to_string(),
        );
    }
    if !st.sessions.contains_key(&id) {
        return gx_error(404, "unknown_session", "no such session");
    }

    match (method, tail.as_slice()) {
        ("GET", []) => (200, st.sessions[&id].to_string()),
        ("GET", ["history"]) => (200, history_page(st, &id, query)),
        ("POST", ["messages"]) => {
            let mode = json_str(body, "mode");
            let mode = if mode.is_empty() {
                "queue".to_string()
            } else {
                mode
            };
            (202, json!({ "accepted": true, "mode": mode }).to_string())
        }
        ("POST", ["cancel"]) => (202, json!({ "accepted": true }).to_string()),
        ("GET", ["approvals"]) => {
            let list: Vec<Value> = st
                .approvals
                .get(&id)
                .map(|m| m.values().map(|e| e.resource.clone()).collect())
                .unwrap_or_default();
            (200, json!({ "approvals": list }).to_string())
        }
        ("GET", ["approvals", aid]) => {
            let aid = percent_decode(aid);
            match st.approvals.get(&id).and_then(|m| m.get(&aid)) {
                Some(e) => (200, e.resource.to_string()),
                None => gx_error(404, "unknown_approval", "no such approval"),
            }
        }
        ("POST", ["approvals", aid]) => {
            let aid = percent_decode(aid);
            let Some(entry) = st.approvals.get_mut(&id).and_then(|m| m.get_mut(&aid)) else {
                return gx_error(404, "unknown_approval", "no such approval");
            };
            match entry.status.as_str() {
                "submitted" => {
                    gx_error(409, "already_submitted", "an answer is already on the wire")
                }
                "resolved" => gx_error(409, "already_resolved", "the interaction closed"),
                _ => {
                    let parsed: Value = serde_json::from_str(body).unwrap_or(Value::Null);
                    let Some(response) = parsed.get("response").filter(|r| r.is_object()) else {
                        return gx_error(
                            400,
                            "bad_request",
                            "response is missing or not an object",
                        );
                    };
                    entry.answered_with = Some(response.clone());
                    entry.status = "submitted".to_string();
                    entry.resource["status"] = json!("submitted");
                    (202, json!({ "status": "submitted" }).to_string())
                }
            }
        }
        // The SSE stream is plan 017 C3. Answered loudly so a premature
        // subscribe fails instead of hanging.
        ("GET", ["events"]) => (
            404,
            r#"{"error":"unknown_session","message":"the fake's SSE stream lands in C3"}"#
                .to_string(),
        ),
        _ => gx_error(404, "unknown_session", "no such route"),
    }
}

/// gx's negative-`offset` paging: `offset` may count back from the end, and
/// `hasMore` says whether anything precedes the returned page.
fn history_page(st: &FakeState, id: &str, query: &str) -> String {
    let updates = st.history.get(id).cloned().unwrap_or_default();
    let n = updates.len() as i64;
    let offset: i64 = query_param(query, "offset")
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);
    let limit: i64 = query_param(query, "limit")
        .and_then(|v| v.parse().ok())
        .unwrap_or(50);
    let start = if offset < 0 {
        (n + offset).max(0)
    } else {
        offset.min(n)
    };
    let end = (start + limit.max(0)).min(n);
    let page: Vec<Value> = updates[start as usize..end as usize].to_vec();
    // "the newest id IN THE RETURNED PAGE, found by reverse-scanning it" —
    // null when no line in the page carried one.
    let last_event_id = page
        .iter()
        .rev()
        .find_map(|u| u.get("eventId").and_then(Value::as_str))
        .map(str::to_string);
    json!({
        "updates": page,
        "totalCount": n,
        "hasMore": start > 0,
        "lastEventId": last_event_id,
    })
    .to_string()
}

fn gx_error(status: u16, code: &str, message: &str) -> (u16, String) {
    (
        status,
        json!({ "error": code, "message": message }).to_string(),
    )
}

fn json_str(body: &str, key: &str) -> String {
    serde_json::from_str::<Value>(body)
        .ok()
        .and_then(|v| v.get(key).and_then(Value::as_str).map(str::to_string))
        .unwrap_or_default()
}

async fn write_response(
    write: &mut tokio::net::tcp::OwnedWriteHalf,
    status: u16,
    body: &str,
) -> std::io::Result<()> {
    let reason = match status {
        200 => "OK",
        201 => "Created",
        202 => "Accepted",
        400 => "Bad Request",
        401 => "Unauthorized",
        404 => "Not Found",
        409 => "Conflict",
        500 => "Internal Server Error",
        503 => "Service Unavailable",
        _ => "Status",
    };
    let head = format!(
        "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    write.write_all(head.as_bytes()).await?;
    write.write_all(body.as_bytes()).await?;
    write.flush().await
}

fn query_param(query: &str, name: &str) -> Option<String> {
    query.split('&').find_map(|pair| {
        let (k, v) = pair.split_once('=')?;
        (percent_decode(k) == name).then(|| percent_decode(&v.replace('+', " ")))
    })
}

fn percent_decode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out: Vec<u8> = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let Ok(b) = u8::from_str_radix(&s[i + 1..i + 3], 16) {
                out.push(b);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).to_string()
}
