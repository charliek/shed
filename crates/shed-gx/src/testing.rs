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
//!   mismatch sent no token at all, and which cursor a reconnect resumed from.
//! - **The pin guard** holds every session-scoped route to one session id. A
//!   violation is recorded AND answered `500`, so an offending verb can never
//!   look like it worked. A suite that drives this crate correctly leaves
//!   [`FakeGx::violations`] empty.
//! - **The error injector** ([`FakeGx::fail`]) answers any of gx's eight codes
//!   on any route, which is how the error table is tested row by row without a
//!   leader that can be talked into each one.
//!
//! # The stream, and the two stores behind it
//!
//! `GET …/events` is faithful about the part that is hard to get right: the
//! **four `Last-Event-ID` resume rules**, evaluated in gx's own order
//! (`gx-remote-api/src/routes/events.rs::plan_replay`). To have those rules
//! mean anything the fake keeps TWO stores per session, exactly as a leader
//! does:
//!
//! - the **persisted transcript** ([`FakeGx::set_history`],
//!   [`FakeGx::push_update`]) — what `GET …/history` serves, and what a resume
//!   from before the ring falls back to;
//! - the **ring** — the recent frames a live stream can replay from memory,
//!   bounded by [`FakeGx::set_ring_cap`] and **dropped by
//!   [`FakeGx::restart_leader`]**, which is what a leader restart looks like
//!   from a client: a new `instanceId`, the same token, an empty ring, and a
//!   transcript still on disk.
//!
//! [`FakeGx::stop_listening`] / [`FakeGx::start_listening`] take the port away
//! and give it back, which is the dead-lane half of the escalation ladder.

use std::collections::{BTreeMap, HashMap};
use std::net::SocketAddr;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt as _, AsyncReadExt as _, AsyncWriteExt as _, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{broadcast, Notify};
use tokio::task::JoinHandle;

/// A 64-lowercase-hex token that is obviously a fixture, so a grep for it in a
/// log, an IPC transcript or a `Debug` string is unambiguous.
/// A transport that COUNTS its dials — the "`dial()` before every connect"
/// assertion — and can be re-pointed, which is a forward that moved.
///
/// Here rather than in either test file because BOTH need it: the unit tests
/// (`crate::client::tests`) and the integration tests (`tests/common`) cannot
/// see each other, and they can both see this module.
#[derive(Debug)]
pub struct CountingDial {
    url: Mutex<reqwest::Url>,
    calls: AtomicUsize,
}

impl CountingDial {
    pub fn new(url: reqwest::Url) -> Arc<CountingDial> {
        Arc::new(CountingDial {
            url: Mutex::new(url),
            calls: AtomicUsize::new(0),
        })
    }

    pub fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }

    pub fn point_at(&self, url: reqwest::Url) {
        *self.url.lock().expect("the dial lock") = url;
    }
}

#[async_trait::async_trait]
impl crate::transport::GxTransport for CountingDial {
    async fn dial(&self) -> Result<reqwest::Url, shed_core::lane::LaneError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Ok(self.url.lock().expect("the dial lock").clone())
    }
}

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
    /// The `Last-Event-ID` header — what a resume assertion reads.
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

/// One broadcast SSE frame, plus the session it belongs to. gx's stream is
/// session-scoped, so a connection drops everything that names another one.
#[derive(Debug, Clone)]
struct Frame {
    wire: String,
    session: String,
}

/// How many recent update envelopes one session's ring holds. gx's own is
/// 2,000; the default here is the same, and [`FakeGx::set_ring_cap`] shrinks it
/// so a test can force the fourth resume rule (a cursor older than the ring)
/// without pushing two thousand frames.
const DEFAULT_RING_CAP: usize = 2000;

#[derive(Default)]
struct FakeState {
    instance_id: String,
    token: String,
    version: String,
    sessions: HashMap<String, Value>,
    order: Vec<String>,
    /// session id → its persisted envelopes, oldest first.
    history: HashMap<String, Vec<Value>>,
    /// session id → the recent envelopes a live stream can replay from memory,
    /// oldest first and capped at [`FakeState::ring_cap`].
    ring: HashMap<String, Vec<Value>>,
    ring_cap: usize,
    /// session id → approval id → entry.
    approvals: HashMap<String, BTreeMap<String, ApprovalEntry>>,
    pin: String,
    violations: Vec<String>,
    requests: Vec<RequestRecord>,
    failures: Vec<Failure>,
    /// (path suffix, envelope) — pushed onto the stream just before the next
    /// matching request is answered. See [`FakeGx::inject_on_get`].
    injections: Vec<(String, Value)>,
    /// (path suffix, milliseconds) — how long to wait before answering a
    /// matching request. See [`FakeGx::delay_get`].
    delays: Vec<(String, u64)>,
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
    /// Broadcast to every live `/events` connection.
    frames: broadcast::Sender<Frame>,
    /// Sending ends every live connection handler ([`FakeGx::close_streams`]).
    hangup: broadcast::Sender<()>,
    /// Live `/events` connections, so a test can assert that a stopped
    /// subscription really released its socket.
    streams: Arc<AtomicUsize>,
    /// Wakes the accept loop so it can drop the listener and exit.
    stop: Arc<Notify>,
    /// `None` while the port is deliberately closed
    /// ([`FakeGx::stop_listening`]).
    accept: Mutex<Option<JoinHandle<()>>>,
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
            ring_cap: DEFAULT_RING_CAP,
            ..FakeState::default()
        }));
        // Roomy: a test that floods the inbox overflow arm queues thousands of
        // frames, and a lagged broadcast would end the connection first.
        let (frames, _) = broadcast::channel(8192);
        let (hangup, _) = broadcast::channel(8);
        let streams = Arc::new(AtomicUsize::new(0));
        let stop = Arc::new(Notify::new());
        let accept = tokio::spawn(accept_loop(
            listener,
            Arc::clone(&state),
            frames.clone(),
            hangup.clone(),
            Arc::clone(&streams),
            Arc::clone(&stop),
        ));
        FakeGx {
            state,
            addr,
            frames,
            hangup,
            streams,
            stop,
            accept: Mutex::new(Some(accept)),
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
    ///
    /// The stores are left alone: this is the narrow "the id changed" fixture
    /// the pin tests want. [`FakeGx::restart_leader`] is the whole event.
    pub fn set_instance_id(&self, id: &str) {
        self.lock().instance_id = id.to_string();
    }

    /// A leader RESTART, as a client sees one: a new `instanceId`, the **same
    /// token** (it is per-`$GROK_HOME`, not per-leader), an **empty ring**, and
    /// the persisted transcript untouched.
    ///
    /// That combination is the point. A resume cursor that was inside the ring
    /// a moment ago now falls through to the disk path, which is the only way
    /// to exercise gx's fourth replay rule — and the reason a lane can recover
    /// a transcript across a restart at all.
    pub fn restart_leader(&self, id: &str) {
        let mut st = self.lock();
        st.instance_id = id.to_string();
        st.ring.clear();
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
    ///
    /// The RING is untouched: this is "what was on disk before the client
    /// connected", which is exactly what a seed reads and what a cold resume
    /// falls back to.
    pub fn set_history(&self, id: &str, updates: Vec<Value>) {
        self.lock().history.insert(id.to_string(), updates);
    }

    /// How many envelopes one session's ring keeps. Shrink it to force the
    /// fourth resume rule (a cursor older than the ring) without pushing gx's
    /// real 2,000 frames.
    pub fn set_ring_cap(&self, cap: usize) {
        let mut st = self.lock();
        st.ring_cap = cap.max(1);
        let cap = st.ring_cap;
        for ring in st.ring.values_mut() {
            trim_ring(ring, cap);
        }
    }

    // ---- the stream ----

    /// The leader's own path for a live event: append it to the persisted
    /// transcript AND the ring, then broadcast it as `event: update` carrying
    /// the whole opaque `eventId` as the `id:` line.
    ///
    /// Both stores, because that is what a leader does — the pump writes the
    /// transcript and feeds the ring — and a fake that only broadcast would let
    /// a reseed silently pass on a session whose history it never wrote.
    pub fn push_update(&self, session: &str, envelope: &Value) {
        let frame = record_update(&mut self.lock(), session, envelope);
        self.broadcast(session, frame.wire);
    }

    /// `event: session` — the roster row changed. A **state invalidation**: no
    /// `id:` line, because an approval or a roster change is not a position in
    /// the session's event history.
    pub fn push_session_frame(&self, session: &str, row: &Value) {
        self.broadcast(session, sse("session", None, &row.to_string()));
    }

    /// `event: session` carrying gx's removal shape. The subscription ends on
    /// it.
    pub fn push_session_removed(&self, session: &str) {
        let body = json!({ "sessionId": session, "removed": true });
        self.broadcast(session, sse("session", None, &body.to_string()));
    }

    /// `event: approval` — the approval resource as the GET routes serve it.
    /// Broadcast only; the approvals store is scripted separately, so a test
    /// can make the frame and the store disagree on purpose.
    pub fn push_approval_frame(&self, session: &str, resource: &Value) {
        self.broadcast(session, sse("approval", None, &resource.to_string()));
    }

    /// The approval resource the fake is holding, as a `GET` would serve it —
    /// what [`FakeGx::push_approval_frame`] is normally given.
    pub fn approval_resource(&self, session: &str, id: &str) -> Option<Value> {
        self.lock()
            .approvals
            .get(session)
            .and_then(|m| m.get(id))
            .map(|e| e.resource.clone())
    }

    /// `event: reset {reason}` — gx telling a client its view is not
    /// resumable. `cursor_unresolvable` and `slow_consumer` are the two the
    /// leader actually sends.
    pub fn push_reset(&self, session: &str, reason: &str) {
        let body = json!({ "reason": reason });
        self.broadcast(session, sse("reset", None, &body.to_string()));
    }

    /// The keep-alive: a comment line, carrying no event at all. It is what a
    /// stall timer must count as liveness, and a timer that counted EVENTS
    /// would tear down every healthy idle stream.
    pub fn push_keepalive(&self) {
        // The session is irrelevant — a comment reaches every connection.
        let _ = self.frames.send(Frame {
            wire: ":keepalive\n\n".to_string(),
            session: String::new(),
        });
    }

    /// Push `envelope` onto the stream **just before** the next request whose
    /// path ends with `suffix` is answered — the deterministic way to put a
    /// frame INSIDE the seed window.
    ///
    /// Without it the seed/live overlap is a race: the watcher's select is
    /// biased toward a completed fetch, so a frame pushed "around then" lands
    /// either in the inbox or in steady state depending on scheduling, and the
    /// test would pass for the wrong reason half the time. Pair it with
    /// [`FakeGx::delay_get`] on a LATER route to make the window wide enough
    /// that the frame is certainly read.
    ///
    /// The envelope goes to the persisted transcript and the ring too, exactly
    /// as [`FakeGx::push_update`] does — which is what makes the overlap real:
    /// the seed's own `GET …/history` will also carry it, and only the fold's
    /// `seen` set stops it becoming two rows.
    pub fn inject_on_get(&self, suffix: &str, envelope: &Value) {
        self.lock()
            .injections
            .push((suffix.to_string(), envelope.clone()));
    }

    /// Wait `millis` before answering any request whose path ends with
    /// `suffix`. Widens the seed window so an injected frame is certainly read
    /// into the inbox rather than racing the fetch.
    pub fn delay_get(&self, suffix: &str, millis: u64) {
        self.lock().delays.push((suffix.to_string(), millis));
    }

    fn broadcast(&self, session: &str, wire: String) {
        let _ = self.frames.send(Frame {
            wire,
            session: session.to_string(),
        });
    }

    /// Hang up on every live stream — an EOF, which is what makes a watcher
    /// reconnect.
    pub fn close_streams(&self) {
        // `Err` only means nobody is connected, which is the state this asks
        // for.
        let _ = self.hangup.send(());
    }

    /// How many `/events` connections are live right now.
    pub fn stream_count(&self) -> usize {
        self.streams.load(Ordering::SeqCst)
    }

    /// Take the port away: stop accepting, and end every live stream.
    ///
    /// Awaits the accept task so the listener is genuinely dropped before this
    /// returns — otherwise a reconnect could still be accepted by a task that
    /// has been told to stop, and the test would be racing.
    pub async fn stop_listening(&self) {
        // A stored permit, not `notify_waiters`: the accept loop builds a fresh
        // `notified()` on every turn of its select, so a wakeup sent while it is
        // inside `accept()` must survive until the next one.
        self.stop.notify_one();
        let handle = self.lock_accept().take();
        if let Some(h) = handle {
            let _ = h.await;
        }
        let _ = self.hangup.send(());
    }

    /// Give the port back, on the SAME address, so a client's reconnect lands
    /// where its dial URL still points.
    pub async fn start_listening(&self) {
        if self.lock_accept().is_some() {
            return;
        }
        let listener = TcpListener::bind(self.addr)
            .await
            .expect("re-binding the fake gx listener");
        let task = tokio::spawn(accept_loop(
            listener,
            Arc::clone(&self.state),
            self.frames.clone(),
            self.hangup.clone(),
            Arc::clone(&self.streams),
            Arc::clone(&self.stop),
        ));
        *self.lock_accept() = Some(task);
    }

    fn lock_accept(&self) -> std::sync::MutexGuard<'_, Option<JoinHandle<()>>> {
        self.accept
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
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
    /// ONE entry point so a later addition to [`ApprovalEntry`] lands in one place
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

    /// Every request served whose path ends with `suffix`, in order.
    pub fn requests_to(&self, suffix: &str) -> Vec<RequestRecord> {
        self.lock()
            .requests
            .iter()
            .filter(|r| r.path.ends_with(suffix))
            .cloned()
            .collect()
    }

    /// The paths served so far, in order — the ledger a seed-order assertion
    /// reads ("the stream was opened BEFORE the seed GETs").
    pub fn paths(&self) -> Vec<String> {
        self.lock()
            .requests
            .iter()
            .map(|r| r.path.clone())
            .collect()
    }

    /// Forget every recorded request. A test that has already asserted the
    /// first connect's ledger uses this so the next assertion is about the
    /// RECONNECT and not about everything since the beginning.
    pub fn clear_requests(&self) {
        self.lock().requests.clear();
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
        let _ = self.hangup.send(());
        if let Some(task) = self.lock_accept().take() {
            task.abort();
        }
    }
}

// ---------------------------------------------------------------------------
// the connection handler
// ---------------------------------------------------------------------------

/// Accept until [`FakeGx::stop_listening`] says otherwise, at which point the
/// listener is DROPPED (returning from here drops it) and the port is free.
async fn accept_loop(
    listener: TcpListener,
    state: Arc<Mutex<FakeState>>,
    frames: broadcast::Sender<Frame>,
    hangup: broadcast::Sender<()>,
    streams: Arc<AtomicUsize>,
    stop: Arc<Notify>,
) {
    loop {
        tokio::select! {
            () = stop.notified() => return,
            accepted = listener.accept() => {
                let Ok((sock, _)) = accepted else { return };
                tokio::spawn(serve(
                    sock,
                    Arc::clone(&state),
                    frames.clone(),
                    hangup.subscribe(),
                    Arc::clone(&streams),
                ));
            }
        }
    }
}

/// One request per connection (`Connection: close`), except `/events`, which is
/// close-delimited and streams until the client goes away or the fake hangs up.
async fn serve(
    sock: TcpStream,
    state: Arc<Mutex<FakeState>>,
    frames: broadcast::Sender<Frame>,
    hangup: broadcast::Receiver<()>,
    streams: Arc<AtomicUsize>,
) {
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
    //
    // Injections and delays are computed under the SAME lock and applied after
    // it: the frames must reach the stream before this request's answer does,
    // and a sleep must never be taken while holding the state.
    let (outcome, injected, delay) = {
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

        // Injections fire on the request they name and are then spent, so a
        // reconnect that re-walks the same routes does not replay them.
        let (due, keep): (Vec<_>, Vec<_>) = std::mem::take(&mut st.injections)
            .into_iter()
            .partition(|(suffix, _)| path.ends_with(suffix.as_str()));
        st.injections = keep;
        let injected: Vec<Frame> = due
            .into_iter()
            .map(|(_, env)| {
                // LOUD, not defaulted. An empty session is this fake's
                // broadcast-to-everyone sentinel (see `serve_events`), so a
                // mis-shaped injection would be delivered to every subscribed
                // session and file its history under `""` — and the test would
                // PASS, for the wrong reason, via the wildcard. That is exactly
                // the masking the resume-accounting tests exist to rule out.
                let session = env
                    .get("params")
                    .and_then(|p| p.get("sessionId"))
                    .and_then(Value::as_str)
                    .unwrap_or_else(|| {
                        panic!(
                            "an injected envelope must name its session in \
                             params.sessionId; got: {env}"
                        )
                    })
                    .to_string();
                record_update(&mut st, &session, &env)
            })
            .collect();
        let delay = st
            .delays
            .iter()
            .find(|(suffix, _)| path.ends_with(suffix.as_str()))
            .map(|(_, ms)| *ms)
            .unwrap_or(0);

        let outcome = if st.hangups.iter().any(|h| path.ends_with(h.as_str())) {
            Served::Hangup
        } else if let Some((_, status)) = st
            .truncations
            .iter()
            .find(|(suffix, _)| path.ends_with(suffix.as_str()))
        {
            Served::Truncate(*status)
        } else if method == "GET" && events_session(&path).is_some() {
            open_stream(&mut st, &path, last_event_id.as_deref(), bearer_ok)
        } else {
            let (status, payload) = route(&mut st, &method, &path, &query, &body, bearer_ok);
            Served::Http(status, payload)
        };
        (outcome, injected, delay)
    };

    for frame in injected {
        let _ = frames.send(frame);
    }
    if delay > 0 {
        tokio::time::sleep(std::time::Duration::from_millis(delay)).await;
    }

    match outcome {
        // Hang up: no status line, no headers, nothing.
        Served::Hangup => {}
        // A head promising a body that never arrives.
        Served::Truncate(status) => {
            let head = format!(
                "HTTP/1.1 {status} Status\r\nContent-Type: application/json\r\nContent-Length: 4096\r\nConnection: close\r\n\r\n"
            );
            let _ = write.write_all(head.as_bytes()).await;
            let _ = write.flush().await;
        }
        Served::Http(status, payload) => {
            let _ = write_response(&mut write, status, &payload).await;
        }
        Served::Stream { session, opening } => {
            streams.fetch_add(1, Ordering::SeqCst);
            serve_events(&mut write, reader, frames, hangup, &session, &opening).await;
            streams.fetch_sub(1, Ordering::SeqCst);
        }
    }
}

/// What one request turns into.
enum Served {
    /// Read it, record it, answer nothing, close.
    Hangup,
    /// A head promising a body that never arrives.
    Truncate(u16),
    Http(u16, String),
    /// The SSE stream, with everything the resume rules decided already
    /// rendered.
    Stream {
        session: String,
        opening: String,
    },
}

/// The session id of a `/v1/sessions/{id}/events` path, or `None`.
fn events_session(path: &str) -> Option<String> {
    let rest = path.strip_prefix("/v1/sessions/")?;
    let (id, tail) = rest.split_once('/')?;
    (tail == "events" && !id.is_empty()).then(|| percent_decode(id))
}

/// The two gates every route shares, in gx's own order: the token, then an
/// injected failure. `Some` is the refusal.
///
/// Shared with [`open_stream`] rather than written twice, because the ORDER is
/// the thing under test and two copies of it are two things to keep in step.
fn global_gate(st: &FakeState, path: &str, bearer_ok: bool) -> Option<(u16, String)> {
    if !bearer_ok {
        // gx deliberately says nothing about WHICH part was wrong.
        return Some(gx_error(401, "unauthorized", "missing or invalid token"));
    }
    // Injected failures come after the token gate and before the pin guard: a
    // scripted `not_accepting` must not also be a guard violation.
    let f = st
        .failures
        .iter()
        .find(|f| path.ends_with(f.suffix.as_str()))?;
    Some(if f.code.is_empty() {
        (f.status, f.message.clone())
    } else {
        gx_error(f.status, &f.code, &f.message)
    })
}

/// The two gates every SESSION-SCOPED route shares: the pin guard, then the
/// session's existence. `Some` is the refusal.
fn session_gate(st: &mut FakeState, method: &str, path: &str, id: &str) -> Option<(u16, String)> {
    if !st.pin.is_empty() && id != st.pin {
        st.violations.push(format!(
            "{method} {path} addressed session {id}, not the pinned {}",
            st.pin
        ));
        // A violation can never look successful.
        return Some((
            500,
            r#"{"error":"pin_guard","message":"violation"}"#.to_string(),
        ));
    }
    if !st.sessions.contains_key(id) {
        return Some(gx_error(404, "unknown_session", "no such session"));
    }
    None
}

/// The `/events` gate, in gx's own order: the token, then an injected failure,
/// then the pin guard, then the session's existence — and only then the replay
/// plan.
///
/// The gates themselves are [`global_gate`] and [`session_gate`], shared with
/// [`route`]; only the SUCCESS path differs, because `route` answers
/// `(status, body)` and a stream is not that.
fn open_stream(st: &mut FakeState, path: &str, cursor: Option<&str>, bearer_ok: bool) -> Served {
    if let Some((s, b)) = global_gate(st, path, bearer_ok) {
        return Served::Http(s, b);
    }
    let Some(id) = events_session(path) else {
        let (s, b) = gx_error(404, "unknown_session", "no such route");
        return Served::Http(s, b);
    };
    if let Some((s, b)) = session_gate(st, "GET", path, &id) {
        return Served::Http(s, b);
    }

    let (reset, replay) = plan_replay(st, &id, cursor);
    let mut opening = String::new();
    if let Some(reason) = reset {
        opening.push_str(&sse(
            "reset",
            None,
            &json!({ "reason": reason }).to_string(),
        ));
    }
    for env in replay {
        let id = env.get("eventId").and_then(Value::as_str);
        opening.push_str(&sse("update", id, &env.to_string()));
    }
    Served::Stream {
        session: id,
        opening,
    }
}

/// gx's four resume rules, **in gx's order**, because they overlap: a cursor
/// can be both newer than everything known and older than the ring's oldest,
/// and only the order says which answer wins.
/// (`gx-remote-api/src/routes/events.rs::plan_replay`.)
///
/// 1. malformed, or another session's prefix → `cursor_unresolvable`;
/// 2. newer than anything known → `cursor_unresolvable`, because resuming would
///    mean skipping events that do not exist yet;
/// 3. inside the ring → replay from memory;
/// 4. older than the ring → the persisted transcript, then the ring's tail,
///    deduplicated by `eventId` (the two overlap by however much of the ring is
///    also on disk).
///
/// No cursor at all is NOT a reset — a fresh subscription just starts live.
fn plan_replay(
    st: &FakeState,
    session: &str,
    cursor: Option<&str>,
) -> (Option<&'static str>, Vec<Value>) {
    let Some(raw) = cursor.map(str::trim).filter(|c| !c.is_empty()) else {
        return (None, Vec::new());
    };

    // (1)
    let Some((prefix, cursor)) = split_event_id(raw) else {
        return (Some("cursor_unresolvable"), Vec::new());
    };
    if prefix != session {
        return (Some("cursor_unresolvable"), Vec::new());
    }

    let ring: &[Value] = st.ring.get(session).map(Vec::as_slice).unwrap_or(&[]);
    let bounds = ring_bounds(ring);
    let history: &[Value] = st.history.get(session).map(Vec::as_slice).unwrap_or(&[]);

    // (2)
    let newest = match bounds {
        Some((_, newest)) => Some(newest),
        None => history.iter().filter_map(counter_of).max(),
    };
    match newest {
        None => return (Some("cursor_unresolvable"), Vec::new()),
        Some(newest) if cursor > newest => return (Some("cursor_unresolvable"), Vec::new()),
        Some(_) => {}
    }

    // (3)
    if let Some((oldest, _)) = bounds {
        if cursor >= oldest {
            return (None, after_counter(ring, cursor));
        }
    }

    // (4)
    let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
    let mut last = cursor;
    let mut frames: Vec<Value> = Vec::new();
    for env in history {
        let Some(k) = counter_of(env) else { continue };
        if k <= cursor {
            continue;
        }
        last = last.max(k);
        if let Some(id) = env.get("eventId").and_then(Value::as_str) {
            seen.insert(id.to_string());
        }
        frames.push(env.clone());
    }
    frames.extend(after_counter(ring, last).into_iter().filter(|env| {
        env.get("eventId")
            .and_then(Value::as_str)
            .is_none_or(|id| !seen.contains(id))
    }));
    (None, frames)
}

/// The ring's oldest and newest COUNTERS, `None` when nothing in it carries an
/// id.
fn ring_bounds(ring: &[Value]) -> Option<(u64, u64)> {
    let mut it = ring.iter().filter_map(counter_of);
    let first = it.next()?;
    Some(it.fold((first, first), |(lo, hi), k| (lo.min(k), hi.max(k))))
}

fn after_counter(page: &[Value], cursor: u64) -> Vec<Value> {
    page.iter()
        .filter(|env| counter_of(env).is_some_and(|k| k > cursor))
        .cloned()
        .collect()
}

fn counter_of(env: &Value) -> Option<u64> {
    let raw = env.get("eventId").and_then(Value::as_str)?;
    split_event_id(raw).map(|(_, counter)| counter)
}

/// `<session-prefix>-<counter>`, split at the LAST hyphen (the prefix is a UUID
/// and carries four of its own).
/// The id grammar again, and **deliberately not** [`crate::fold::EventId`]'s.
///
/// This models the SERVER. A fake that split ids with the same code the fold
/// does would agree with it by construction, and the resume tests would pass
/// even if that one shared parser were wrong. The duplication is the
/// differential — do not "fix" it by sharing.
fn split_event_id(raw: &str) -> Option<(&str, u64)> {
    let (prefix, counter) = raw.trim().rsplit_once('-')?;
    if prefix.is_empty() || counter.is_empty() {
        return None;
    }
    Some((prefix, counter.parse().ok()?))
}

/// What a leader does with ONE event: append it to the persisted transcript AND
/// the ring, trim the ring, and say what would go on the wire.
///
/// Both stores, because that is what a leader does — the pump writes the
/// transcript and feeds the ring — and a fake that only broadcast would let a
/// reseed silently pass on a session whose history it never wrote. Written once
/// because the two callers ([`FakeGx::push_update`] and the injection path) are
/// exactly where the two stores would otherwise drift apart.
fn record_update(st: &mut FakeState, session: &str, env: &Value) -> Frame {
    st.history
        .entry(session.to_string())
        .or_default()
        .push(env.clone());
    let cap = st.ring_cap;
    let ring = st.ring.entry(session.to_string()).or_default();
    ring.push(env.clone());
    trim_ring(ring, cap);
    Frame {
        wire: sse(
            "update",
            env.get("eventId").and_then(Value::as_str),
            &env.to_string(),
        ),
        session: session.to_string(),
    }
}

fn trim_ring(ring: &mut Vec<Value>, cap: usize) {
    if ring.len() > cap {
        let drop = ring.len() - cap;
        ring.drain(..drop);
    }
}

/// One SSE frame on the wire. Only `update` ever carries an `id:`; `session`,
/// `approval` and `reset` are state invalidations, and giving one an `id:`
/// would let a client resume from a cursor that is not an event position at
/// all.
fn sse(event: &str, id: Option<&str>, data: &str) -> String {
    let mut out = format!("event: {event}\n");
    if let Some(id) = id {
        out.push_str(&format!("id: {id}\n"));
    }
    out.push_str(&format!("data: {data}\n\n"));
    out
}

/// The stream: the head, the replay the resume rules decided on, then whatever
/// is broadcast for this session.
async fn serve_events(
    write: &mut tokio::net::tcp::OwnedWriteHalf,
    mut reader: BufReader<tokio::net::tcp::OwnedReadHalf>,
    frames: broadcast::Sender<Frame>,
    mut hangup: broadcast::Receiver<()>,
    session: &str,
    opening: &str,
) {
    // Subscribe BEFORE the head is written, so a frame pushed the instant the
    // client sees the head cannot slip between the two.
    let mut rx = frames.subscribe();
    let head = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: close\r\n\r\n";
    if write.write_all(head.as_bytes()).await.is_err() {
        return;
    }
    if !opening.is_empty() && write.write_all(opening.as_bytes()).await.is_err() {
        return;
    }
    let _ = write.flush().await;

    let mut sink = [0u8; 256];
    loop {
        tokio::select! {
            _ = hangup.recv() => return,
            // The client going away (an aborted subscription drops the
            // response, which closes the socket) shows up here as EOF.
            n = reader.read(&mut sink) => {
                if matches!(n, Ok(0) | Err(_)) { return }
            }
            frame = rx.recv() => {
                let Ok(frame) = frame else {
                    // Lagged, or the sender is gone. Ending the connection is
                    // the honest answer: a dropped frame is a gap, and this
                    // crate's response to one is a reconnect.
                    return;
                };
                // An empty session is the keep-alive: everybody gets it.
                if !frame.session.is_empty() && frame.session != session { continue }
                if write.write_all(frame.wire.as_bytes()).await.is_err() { return }
                let _ = write.flush().await;
            }
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

    if let Some(refusal) = global_gate(st, path, bearer_ok) {
        return refusal;
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

    if let Some(refusal) = session_gate(st, method, path, &id) {
        return refusal;
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
        // `GET …/events` never reaches here — `serve` routes it to
        // [`open_stream`] before calling this, because a stream is not a
        // `(status, body)`.
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
