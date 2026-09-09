//! An in-process fake opencode server, for tests.
//!
//! The `shed_core::roost::testing::FakeRoost` pattern: a tokio listener, one
//! connection handler, everything a test wants to script behind one lock,
//! exported through the non-default `test-support` feature so a shipped binary
//! never carries it. Integration tests reach it THROUGH that feature — never
//! `cfg(test)` — which is what lets `tests/*.rs` use the same double the unit
//! tests do.
//!
//! It is a **test double, not a server**: no agent, no models, no persistence.
//! What it is faithful about is the wire — the routes this crate calls, their
//! shapes, `?directory=` instance scoping on the four instance-scoped routes,
//! opencode's Basic-auth gate, and a `/event` stream that opens with
//! `server.connected` and is close-delimited.
//!
//! # The pin guard
//!
//! Its grammar is the one `tests/rc-parity/fake_opencode.py` enforces (its
//! `_SCOPED_RE`, the Go double's `ocScopedMutationRe`), extended with the two
//! request-addressed answer routes this crate uses:
//!
//! - a session-scoped POST (`/session/{id}/prompt_async`, `…/abort`,
//!   `…/permissions/{pid}`) or a `DELETE /session/{id}` must name the PINNED
//!   session;
//! - a `POST /permission/{rid}/reply` or `/question/{rid}/{reply,reject}` must
//!   name a request the fake ISSUED for the pinned session or one of its
//!   descendants (a child's approval is answerable from the root's panel —
//!   `requestID` is global — and that is precisely the thing worth pinning);
//! - `POST /session` (create) is global and allowed;
//! - anything else is a recorded violation, and answers 500 so the offending
//!   verb can never look like it worked.
//!
//! A suite that drives this crate correctly records ZERO violations.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt as _, AsyncReadExt as _, AsyncWriteExt as _, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::broadcast;
use tokio::task::JoinHandle;

/// One SSE frame on the wire, plus the session it belongs to so a connection
/// can apply opencode's instance (`?directory=`) filter.
#[derive(Clone, Debug)]
struct Frame {
    wire: String,
    /// `None` for a server-level frame (`server.connected`, a comment ping) —
    /// delivered to every connection.
    session_id: Option<String>,
}

/// An oversized body: how many bytes to write, and whether the head declares
/// them. Declared, the cap is refusable from the head alone; undeclared
/// (close-delimited), it has to be enforced while the body is consumed.
#[derive(Clone, Copy, Debug)]
struct Flood {
    bytes: usize,
    declare_length: bool,
}

/// Everything the fake knows, behind one lock so a test can poke it while
/// connections are live.
#[derive(Default)]
struct FakeState {
    /// Session objects keyed by id, in insertion order (`order`).
    sessions: HashMap<String, Value>,
    order: Vec<String>,
    /// session id → the raw `GET /session/{id}/message` array.
    messages: HashMap<String, Value>,
    /// session id → `"busy" | "retry" | "idle"`. An absent id is idle, which is
    /// what opencode's own map does.
    status: HashMap<String, String>,
    permissions: Vec<Value>,
    questions: Vec<Value>,
    /// request id → the session it was issued for (the pin guard's ledger).
    issued: HashMap<String, String>,

    /// The subscribed session the guard holds every mutation to.
    pin: String,
    violations: Vec<String>,
    get_paths: Vec<String>,
    post_paths: Vec<String>,
    /// The same mutations with their query string intact — `create` sends
    /// `?directory=` and a test has to be able to see it.
    post_targets: Vec<String>,
    /// (path, body) in order, so a test can assert what a verb actually sent.
    post_bodies: Vec<(String, String)>,

    /// `Some((user, pass))` demands Basic auth on every route.
    auth: Option<(String, String)>,
    /// Path-suffix → status override, for the failure arms.
    fail_post: Vec<(String, u16)>,
    fail_get: Vec<(String, u16)>,
    /// Path-suffix → a frame pushed just BEFORE that GET is answered. This is
    /// how a test injects an event DURING the REST seed deterministically.
    inject_on_get: Vec<(String, Frame)>,
    /// Path suffix → where a GET STALLS. `None` sends nothing at all; `Some`
    /// sends a complete head of that status and then never sends the body it
    /// promised. A wedged peer, which is a different failure from a slow one.
    stall: Vec<(String, Option<u16>)>,
    /// Path suffix → an oversized body, written in chunks so neither the fake
    /// nor the test ever allocates it whole.
    flood: Vec<(String, Flood)>,
    /// `Some(n)` once a [`FakeOpencode::flood_get`] response has STOPPED
    /// writing, `n` being how many body bytes it managed before the client hung
    /// up. `None` while it is still going, so a test can wait for the answer
    /// rather than race it.
    flooded: Option<usize>,
    /// Path suffixes whose GETs are PARKED until released. The seed becomes a
    /// place a test can stand, which is what makes "during the seed" a
    /// deterministic condition rather than a race.
    held: Vec<String>,
    /// The same for mutations — what makes "while the answer is IN FLIGHT" a
    /// condition rather than a race.
    held_post: Vec<String>,
    next_id: u64,
}

impl FakeState {
    fn directory_of(&self, session_id: &str) -> Option<String> {
        self.sessions
            .get(session_id)
            .and_then(|s| s.get("directory"))
            .and_then(Value::as_str)
            .map(str::to_string)
    }

    /// The pinned session plus every descendant of it — the scope an answer
    /// route may address.
    fn pin_scope(&self) -> Vec<String> {
        if self.pin.is_empty() {
            return Vec::new();
        }
        let mut scope = vec![self.pin.clone()];
        // Sessions are few; a fixpoint sweep is simpler than a tree and copes
        // with any insertion order.
        loop {
            let mut grew = false;
            for id in &self.order {
                if scope.iter().any(|s| s == id) {
                    continue;
                }
                let parent = self
                    .sessions
                    .get(id)
                    .and_then(|s| s.get("parentID"))
                    .and_then(Value::as_str)
                    .unwrap_or_default();
                if scope.iter().any(|s| s == parent) {
                    scope.push(id.clone());
                    grew = true;
                }
            }
            if !grew {
                break;
            }
        }
        scope
    }
}

/// An in-process opencode stand-in on a loopback TCP port.
pub struct FakeOpencode {
    state: Arc<Mutex<FakeState>>,
    addr: SocketAddr,
    /// Broadcast to every live `/event` connection.
    frames: broadcast::Sender<Frame>,
    /// Sending ends every live connection handler ([`Self::close_streams`]).
    hangup: broadcast::Sender<()>,
    /// Live `/event` connections, for asserting a stopped subscription really
    /// released its socket.
    streams: Arc<AtomicUsize>,
    /// Woken by [`FakeOpencode::release_get`].
    release: Arc<tokio::sync::Notify>,
    accept: JoinHandle<()>,
}

impl FakeOpencode {
    /// Bind and start accepting on an ephemeral loopback port.
    pub async fn start() -> FakeOpencode {
        FakeOpencode::start_inner(None).await
    }

    /// Same, but demanding HTTP Basic auth on every route — opencode's
    /// `OPENCODE_SERVER_PASSWORD` posture (username defaults to `opencode`).
    pub async fn start_with_auth(user: &str, password: &str) -> FakeOpencode {
        FakeOpencode::start_inner(Some((user.to_string(), password.to_string()))).await
    }

    async fn start_inner(auth: Option<(String, String)>) -> FakeOpencode {
        let listener = TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("binding the fake opencode listener");
        let addr = listener.local_addr().expect("the fake's address");
        let state = Arc::new(Mutex::new(FakeState {
            auth,
            ..FakeState::default()
        }));
        // Roomy enough that a test can queue the thousands of frames the inbox
        // overflow arm needs without the broadcast lagging first.
        let (frames, _) = broadcast::channel(8192);
        let (hangup, _) = broadcast::channel(8);
        let streams = Arc::new(AtomicUsize::new(0));
        let release = Arc::new(tokio::sync::Notify::new());

        let accept = tokio::spawn({
            let state = Arc::clone(&state);
            let frames = frames.clone();
            let hangup = hangup.clone();
            let streams = Arc::clone(&streams);
            let release = Arc::clone(&release);
            async move {
                while let Ok((sock, _)) = listener.accept().await {
                    tokio::spawn(serve(
                        sock,
                        Arc::clone(&state),
                        frames.clone(),
                        hangup.subscribe(),
                        Arc::clone(&streams),
                        Arc::clone(&release),
                    ));
                }
            }
        });

        FakeOpencode {
            state,
            addr,
            frames,
            hangup,
            streams,
            release,
            accept,
        }
    }

    /// The base URL an [`crate::OpencodeClient`] is built on.
    pub fn base_url(&self) -> reqwest::Url {
        format!("http://{}/", self.addr)
            .parse()
            .expect("the fake's base URL parses")
    }

    pub fn addr(&self) -> SocketAddr {
        self.addr
    }

    // ---- scripting ----

    /// Add a session. `parent` makes it a child (its approvals surface on the
    /// parent's panel; its transcript does not).
    pub fn add_session(&self, id: &str, title: &str, directory: &str, parent: Option<&str>) {
        let mut st = self.lock();
        st.sessions.insert(
            id.to_string(),
            json!({
                "id": id,
                "title": title,
                "directory": directory,
                "parentID": parent.unwrap_or_default(),
                "time": { "created": 1_700_000_000_000i64, "updated": 1_700_000_001_000i64 },
            }),
        );
        if !st.order.iter().any(|o| o == id) {
            st.order.push(id.to_string());
        }
    }

    /// Delete a session — every id-addressed route then answers 404, which is
    /// what turns a reseed into `Down{"unknown_session"}`.
    pub fn remove_session(&self, id: &str) {
        let mut st = self.lock();
        st.sessions.remove(id);
        st.order.retain(|o| o != id);
        st.messages.remove(id);
        st.status.remove(id);
    }

    /// `"busy" | "retry" | "idle"`; `""` removes the entry (which opencode
    /// reads as idle).
    pub fn set_status(&self, id: &str, status: &str) {
        let mut st = self.lock();
        if status.is_empty() {
            st.status.remove(id);
        } else {
            st.status.insert(id.to_string(), status.to_string());
        }
    }

    /// The raw `GET /session/{id}/message` body — an array of `{info, parts}`.
    pub fn set_messages(&self, id: &str, messages: Value) {
        self.lock().messages.insert(id.to_string(), messages);
    }

    /// A convenience for the common seed: one user turn and one assistant turn.
    pub fn set_simple_transcript(&self, id: &str, user: &str, assistant: &str) {
        self.set_messages(
            id,
            json!([
                {
                    "info": { "id": "msg_u", "role": "user",
                              "time": { "created": 1_700_000_000_000i64, "completed": 1_700_000_000_000i64 } },
                    "parts": [ { "id": "prt_u", "messageID": "msg_u", "type": "text", "text": user } ],
                },
                {
                    "info": { "id": "msg_a", "role": "assistant",
                              "time": { "created": 1_700_000_000_500i64, "completed": 1_700_000_001_000i64 } },
                    "parts": [ { "id": "prt_a", "messageID": "msg_a", "type": "text", "text": assistant } ],
                },
            ]),
        );
    }

    /// Add an open permission request, and record that the fake issued it for
    /// `session` (the pin guard's ledger).
    pub fn add_permission(&self, session: &str, id: &str, permission: &str, command: &str) {
        let mut st = self.lock();
        st.permissions.push(json!({
            "id": id,
            "sessionID": session,
            "permission": permission,
            "patterns": [command],
            "metadata": { "command": command },
        }));
        st.issued.insert(id.to_string(), session.to_string());
    }

    /// Add an open question request, single-choice over `options`.
    pub fn add_question(
        &self,
        session: &str,
        id: &str,
        header: &str,
        question: &str,
        options: &[&str],
    ) {
        let mut st = self.lock();
        st.questions.push(json!({
            "id": id,
            "sessionID": session,
            "questions": [ {
                "header": header,
                "question": question,
                "options": options.iter().map(|o| json!({"label": o, "description": ""})).collect::<Vec<_>>(),
            } ],
        }));
        st.issued.insert(id.to_string(), session.to_string());
    }

    /// Retire an open request (what a reseed sees after the TUI answered it).
    pub fn remove_request(&self, id: &str) {
        let mut st = self.lock();
        st.permissions
            .retain(|p| p.get("id").and_then(Value::as_str) != Some(id));
        st.questions
            .retain(|q| q.get("id").and_then(Value::as_str) != Some(id));
    }

    // ---- the stream ----

    /// Broadcast one `/event` payload. Delivered only to connections whose
    /// `?directory=` matches the frame's session (or to all, when the frame
    /// names no session).
    pub fn push_event(&self, event: &Value) {
        let session_id = event
            .get("properties")
            .and_then(|p| p.get("sessionID"))
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .map(str::to_string);
        let wire = format!("data: {event}\n\n");
        let _ = self.frames.send(Frame { wire, session_id });
    }

    /// Broadcast a comment ping — bytes with no event in them. The wire's only
    /// keep-alive (opencode has no `server.heartbeat`), and the thing a stall
    /// timer must count as liveness.
    pub fn push_ping(&self) {
        let _ = self.frames.send(Frame {
            wire: ": ping\n\n".to_string(),
            session_id: None,
        });
    }

    /// Broadcast a raw `data:` payload of `bytes` bytes — for the
    /// oversized-frame arm.
    pub fn push_oversized(&self, bytes: usize) {
        let _ = self.frames.send(Frame {
            wire: format!("data: {}\n\n", "x".repeat(bytes)),
            session_id: None,
        });
    }

    /// Push `event` just before the next GET whose path ENDS WITH `suffix` is
    /// answered — the deterministic way to inject an event DURING the REST
    /// seed.
    pub fn inject_on_get(&self, suffix: &str, event: &Value) {
        let session_id = event
            .get("properties")
            .and_then(|p| p.get("sessionID"))
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .map(str::to_string);
        self.lock().inject_on_get.push((
            suffix.to_string(),
            Frame {
                wire: format!("data: {event}\n\n"),
                session_id,
            },
        ));
    }

    /// Hang up on every live connection — what makes a watcher reconnect.
    pub fn close_streams(&self) {
        // `Err` only means nobody is connected, which is the state this asks
        // for.
        let _ = self.hangup.send(());
    }

    /// How many `/event` connections are live right now.
    pub fn stream_count(&self) -> usize {
        self.streams.load(Ordering::SeqCst)
    }

    // ---- failure injection ----

    /// Answer `status` to every POST whose path ends with `suffix`.
    pub fn fail_post(&self, suffix: &str, status: u16) {
        self.lock().fail_post.push((suffix.to_string(), status));
    }

    /// Answer `status` to every GET whose path ends with `suffix`.
    pub fn fail_get(&self, suffix: &str, status: u16) {
        self.lock().fail_get.push((suffix.to_string(), status));
    }

    /// ACCEPT the connection for every GET whose path ends with `suffix`, and
    /// then STALL — forever, until the fake hangs up or is dropped.
    ///
    /// `head` picks where the wedge is. `None` sends nothing at all: no status
    /// line, no headers — a peer that satisfies `connect_timeout` and then goes
    /// quiet, which is the shape a header-read bound exists for. `Some(status)`
    /// sends a complete head promising a body that never arrives, which is the
    /// same wedge one layer later.
    pub fn stall_get(&self, suffix: &str, head: Option<u16>) {
        self.lock().stall.push((suffix.to_string(), head));
    }

    /// Answer every GET whose path ends with `suffix` with `bytes` of filler,
    /// written in 64 KiB chunks so neither side allocates it whole.
    ///
    /// `declare_length` picks whether the head carries a `Content-Length` (the
    /// cap is then refusable before a body byte is read) or the body is
    /// close-delimited (the cap has to be enforced while it is consumed).
    pub fn flood_get(&self, suffix: &str, bytes: usize, declare_length: bool) {
        self.lock().flood.push((
            suffix.to_string(),
            Flood {
                bytes,
                declare_length,
            },
        ));
    }

    /// How many body bytes a [`Self::flood_get`] managed to write, once it has
    /// stopped writing — `None` while it is still going. A client that refused
    /// the body from its declared length leaves this far below the cap.
    pub fn flooded_bytes(&self) -> Option<usize> {
        self.lock().flooded
    }

    /// PARK every GET whose path ends with `suffix` until [`Self::release_get`]
    /// — the seed becomes a place a test can stand on, so "while the seed is in
    /// flight" is a condition rather than a race. The request is already
    /// recorded in [`Self::get_paths`] while it is parked, which is how a test
    /// waits for the hold to take effect.
    pub fn hold_get(&self, suffix: &str) {
        self.lock().held.push(suffix.to_string());
    }

    /// Release everything [`Self::hold_get`] parked for `suffix`.
    pub fn release_get(&self, suffix: &str) {
        self.lock().held.retain(|h| h != suffix);
        self.release.notify_waiters();
    }

    /// PARK every POST/DELETE whose path ends with `suffix` until
    /// [`Self::release_post`]. The mutation is already recorded in
    /// [`Self::post_paths`] while it is parked.
    pub fn hold_post(&self, suffix: &str) {
        self.lock().held_post.push(suffix.to_string());
    }

    /// Release everything [`Self::hold_post`] parked for `suffix`.
    pub fn release_post(&self, suffix: &str) {
        self.lock().held_post.retain(|h| h != suffix);
        self.release.notify_waiters();
    }

    // ---- the guard + recording ----

    /// Pin the subscribed session: every session-scoped mutation must name it,
    /// and every answer route must name a request issued for it or a
    /// descendant.
    pub fn pin(&self, session_id: &str) {
        self.lock().pin = session_id.to_string();
    }

    /// Every recorded pin-guard violation. A correct suite leaves this empty.
    pub fn violations(&self) -> Vec<String> {
        self.lock().violations.clone()
    }

    /// Every GET path served, in order (query string included).
    pub fn get_paths(&self) -> Vec<String> {
        self.lock().get_paths.clone()
    }

    /// Every POST/DELETE path served, in order (query string stripped).
    pub fn post_paths(&self) -> Vec<String> {
        self.lock().post_paths.clone()
    }

    /// Every POST/DELETE request target, in order, query string INCLUDED.
    pub fn post_targets(&self) -> Vec<String> {
        self.lock().post_targets.clone()
    }

    /// The body of the first recorded POST whose path ends with `suffix`.
    pub fn post_body(&self, suffix: &str) -> Option<String> {
        self.lock()
            .post_bodies
            .iter()
            .find(|(p, _)| p.ends_with(suffix))
            .map(|(_, b)| b.clone())
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, FakeState> {
        self.state
            .lock()
            .expect("the fake opencode lock is not poisoned")
    }
}

impl Drop for FakeOpencode {
    fn drop(&mut self) {
        let _ = self.hangup.send(());
        self.accept.abort();
    }
}

// ---- the connection handler ----

/// One request per connection (`Connection: close`), except `/event`, which is
/// close-delimited and streams until the client goes away or the fake hangs up.
async fn serve(
    sock: TcpStream,
    state: Arc<Mutex<FakeState>>,
    frames: broadcast::Sender<Frame>,
    mut hangup: broadcast::Receiver<()>,
    streams: Arc<AtomicUsize>,
    release: Arc<tokio::sync::Notify>,
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
    let mut authorization = String::new();
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line).await.unwrap_or(0) == 0 {
            return;
        }
        let line = line.trim_end_matches(['\r', '\n']);
        if line.is_empty() {
            break;
        }
        let (name, value) = match line.split_once(':') {
            Some((n, v)) => (n.trim().to_ascii_lowercase(), v.trim().to_string()),
            None => continue,
        };
        match name.as_str() {
            "content-length" => content_length = value.parse().unwrap_or(0),
            "authorization" => authorization = value,
            _ => {}
        }
    }
    let mut body = vec![0u8; content_length];
    if content_length > 0 && reader.read_exact(&mut body).await.is_err() {
        return;
    }
    let body = String::from_utf8_lossy(&body).to_string();

    let (path, query) = match target.split_once('?') {
        Some((p, q)) => (p.to_string(), q.to_string()),
        None => (target.clone(), String::new()),
    };
    let directory = query_param(&query, "directory");

    // Auth first: opencode's gate is in front of every route.
    let want_auth = state
        .lock()
        .expect("the fake opencode lock is not poisoned")
        .auth
        .clone();
    if let Some((user, pass)) = want_auth {
        let expected = basic_header(&user, &pass);
        if authorization != expected {
            let _ = write_response(&mut write, 401, "{\"message\":\"unauthorized\"}").await;
            return;
        }
    }

    // Every request is RECORDED (and any injection fired) before it may be
    // parked, so a test can wait on `get_paths`/`post_paths` to know the
    // request has arrived and is sitting here.
    let is_get = method == "GET";
    let injected: Vec<Frame> = {
        let mut st = state
            .lock()
            .expect("the fake opencode lock is not poisoned");
        let mut out = Vec::new();
        if is_get {
            st.get_paths.push(target.clone());
            st.inject_on_get.retain(|(suffix, frame)| {
                if path.ends_with(suffix.as_str()) {
                    out.push(frame.clone());
                    false
                } else {
                    true
                }
            });
        } else if method == "POST" || method == "DELETE" {
            st.post_paths.push(path.clone());
            st.post_targets.push(target.clone());
            st.post_bodies.push((path.clone(), body.clone()));
        }
        out
    };
    for frame in injected {
        let _ = frames.send(frame);
    }
    loop {
        // The `notified()` future is created BEFORE the condition is re-read,
        // so a release that lands in between is not missed.
        let notified = release.notified();
        let held = {
            let st = state
                .lock()
                .expect("the fake opencode lock is not poisoned");
            let list = if is_get { &st.held } else { &st.held_post };
            list.iter().any(|h| path.ends_with(h.as_str()))
        };
        if !held {
            break;
        }
        notified.await;
    }

    // An injected GET failure applies to `/event` too — that is how a test
    // makes the pump's FIRST connect fail.
    if is_get {
        let injected_status = {
            let st = state
                .lock()
                .expect("the fake opencode lock is not poisoned");
            st.fail_get
                .iter()
                .find(|(suffix, _)| path.ends_with(suffix.as_str()))
                .map(|(_, status)| *status)
        };
        if let Some(status) = injected_status {
            let _ = write_response(&mut write, status, "{\"message\":\"injected failure\"}").await;
            return;
        }

        let (stall, flood) = {
            let st = state
                .lock()
                .expect("the fake opencode lock is not poisoned");
            (
                st.stall
                    .iter()
                    .find(|(suffix, _)| path.ends_with(suffix.as_str()))
                    .map(|(_, head)| *head),
                st.flood
                    .iter()
                    .find(|(suffix, _)| path.ends_with(suffix.as_str()))
                    .map(|(_, f)| *f),
            )
        };
        if let Some(head) = stall {
            if let Some(status) = head {
                // A complete head promising a body that never arrives.
                let promise = format!(
                    "HTTP/1.1 {status} Status\r\nContent-Type: application/json\r\nContent-Length: 4096\r\nConnection: close\r\n\r\n"
                );
                if write.write_all(promise.as_bytes()).await.is_err() {
                    return;
                }
                let _ = write.flush().await;
            }
            // Nothing more, ever. Only the hangup (or the fake's Drop) ends it.
            let _ = hangup.recv().await;
            return;
        }
        if let Some(flood) = flood {
            write_flood(&mut write, &state, flood).await;
            return;
        }
    }

    if method == "GET" && path == "/event" {
        streams.fetch_add(1, Ordering::SeqCst);
        serve_event(&mut write, reader, state, frames, hangup, directory).await;
        streams.fetch_sub(1, Ordering::SeqCst);
        return;
    }

    let (status, payload) = match method.as_str() {
        "GET" => serve_get(&state, &path, directory.as_deref()),
        "POST" | "DELETE" => serve_mutation(&state, &method, &path),
        _ => (405, "{}".to_string()),
    };
    let _ = write_response(&mut write, status, &payload).await;
}

/// The SSE stream: `server.connected` first, then whatever is broadcast, with
/// the instance (`?directory=`) filter opencode's own `/event` applies.
async fn serve_event(
    write: &mut tokio::net::tcp::OwnedWriteHalf,
    mut reader: BufReader<tokio::net::tcp::OwnedReadHalf>,
    state: Arc<Mutex<FakeState>>,
    frames: broadcast::Sender<Frame>,
    mut hangup: broadcast::Receiver<()>,
    directory: Option<String>,
) {
    // Subscribe BEFORE the head is written, so a frame pushed the instant the
    // client sees the head cannot slip between the two.
    let mut rx = frames.subscribe();
    let head = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: close\r\n\r\n";
    if write.write_all(head.as_bytes()).await.is_err() {
        return;
    }
    if write
        .write_all(b"data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
        .await
        .is_err()
    {
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
                if matches!(n, Ok(0) | Err(_)) { return; }
            }
            frame = rx.recv() => {
                let frame = match frame {
                    Ok(f) => f,
                    // Lagged: a test pushed more than the channel holds. Ending
                    // the connection is the honest answer — it is a dropped
                    // frame, and this crate's response to one is a reconnect.
                    Err(_) => return,
                };
                if !frame_matches(&state, &frame, directory.as_deref()) { continue; }
                if write.write_all(frame.wire.as_bytes()).await.is_err() { return; }
                let _ = write.flush().await;
            }
        }
    }
}

/// opencode's instance filter: a frame naming a session in another directory
/// never reaches this connection.
fn frame_matches(state: &Arc<Mutex<FakeState>>, frame: &Frame, directory: Option<&str>) -> bool {
    let Some(session_id) = &frame.session_id else {
        return true; // server-level: everybody gets it
    };
    let Some(want) = directory else {
        return true; // no `?directory=`: the process cwd instance sees all
    };
    let st = state
        .lock()
        .expect("the fake opencode lock is not poisoned");
    match st.directory_of(session_id) {
        // An unknown session cannot be filtered out — that is how a
        // `session.created` for a brand-new child still arrives.
        None => true,
        Some(dir) => dir == want,
    }
}

fn serve_get(state: &Arc<Mutex<FakeState>>, path: &str, directory: Option<&str>) -> (u16, String) {
    let st = state
        .lock()
        .expect("the fake opencode lock is not poisoned");
    if path == "/session" {
        let all: Vec<&Value> = st
            .order
            .iter()
            .filter_map(|id| st.sessions.get(id))
            .collect();
        return (200, json!(all).to_string());
    }
    if path == "/session/status" {
        let mut map = serde_json::Map::new();
        for (id, status) in &st.status {
            if let Some(dir) = directory {
                if st.directory_of(id).as_deref() != Some(dir) {
                    continue;
                }
            }
            map.insert(id.clone(), json!({ "type": status }));
        }
        return (200, Value::Object(map).to_string());
    }
    if path == "/permission" || path == "/question" {
        let source = if path == "/permission" {
            &st.permissions
        } else {
            &st.questions
        };
        let filtered: Vec<&Value> = source
            .iter()
            .filter(|r| match directory {
                None => true,
                Some(dir) => {
                    let sid = r
                        .get("sessionID")
                        .and_then(Value::as_str)
                        .unwrap_or_default();
                    st.directory_of(sid).as_deref() == Some(dir)
                }
            })
            .collect();
        return (200, json!(filtered).to_string());
    }
    if let Some(rest) = path.strip_prefix("/session/") {
        let (id, tail) = match rest.split_once('/') {
            Some((id, tail)) => (percent_decode(id), tail),
            None => (percent_decode(rest), ""),
        };
        if !st.sessions.contains_key(&id) {
            return (
                404,
                r#"{"name":"NotFoundError","data":{"message":"no such session"}}"#.to_string(),
            );
        }
        return match tail {
            "" => (200, st.sessions[&id].to_string()),
            "message" => (
                200,
                st.messages
                    .get(&id)
                    .cloned()
                    .unwrap_or_else(|| json!([]))
                    .to_string(),
            ),
            "children" => {
                let kids: Vec<&Value> = st
                    .order
                    .iter()
                    .filter_map(|k| st.sessions.get(k))
                    .filter(|s| s.get("parentID").and_then(Value::as_str) == Some(id.as_str()))
                    .collect();
                (200, json!(kids).to_string())
            }
            _ => (404, "{}".to_string()),
        };
    }
    (404, "{}".to_string())
}

/// Every POST/DELETE, through the pin guard.
fn serve_mutation(state: &Arc<Mutex<FakeState>>, method: &str, path: &str) -> (u16, String) {
    let mut st = state
        .lock()
        .expect("the fake opencode lock is not poisoned");

    let pin = st.pin.clone();
    let scope = st.pin_scope();
    let mut violation: Option<String> = None;
    let mut created: Option<String> = None;
    // A session-scoped mutation naming a session the server does not have is a
    // 404, exactly as on a GET — otherwise a verb addressed at a nonexistent id
    // would look like it worked.
    let mut unknown_session = false;

    let allowed = if method == "POST" && path == "/session" {
        // Global by construction — a create names no session yet.
        st.next_id += 1;
        let id = format!("ses_created_{}", st.next_id);
        created = Some(id);
        true
    } else if let Some(rest) = path.strip_prefix("/session/") {
        let (id, tail) = match rest.split_once('/') {
            Some((id, tail)) => (percent_decode(id), tail.to_string()),
            None => (percent_decode(rest), String::new()),
        };
        let shaped = (method == "POST"
            && (tail == "prompt_async" || tail == "abort" || tail.starts_with("permissions/")))
            || (method == "DELETE" && tail.is_empty());
        if !shaped {
            violation = Some(format!(
                "not a session-scoped mutation route: {method} {path}"
            ));
            false
        } else if !pin.is_empty() && id != pin {
            violation = Some(format!("addressed session {id}, not the pinned {pin}"));
            false
        } else if let Some(rid) = tail.strip_prefix("permissions/") {
            // The DEPRECATED session-scoped answer route. Naming the right
            // session is not enough: it answers an approval, so it owes the
            // same ledger check the live `/permission/{id}/reply` arm below
            // makes. Without this a test could answer a request the fake never
            // issued and the guard would record nothing — the hole the Python
            // fake had at `fake_opencode.py`'s legacy route.
            let rid = percent_decode(rid);
            match st.issued.get(&rid) {
                None => {
                    violation = Some(format!("answered {rid}, which the fake never issued"));
                    false
                }
                Some(owner) if !scope.iter().any(|s| s == owner) => {
                    violation = Some(format!(
                        "answered {rid}, issued for {owner}, outside the pinned {pin}'s scope"
                    ));
                    false
                }
                Some(_) => {
                    unknown_session = !st.sessions.contains_key(&id);
                    true
                }
            }
        } else {
            unknown_session = !st.sessions.contains_key(&id);
            true
        }
    } else if let Some(rest) = path
        .strip_prefix("/permission/")
        .or_else(|| path.strip_prefix("/question/"))
    {
        let (rid, tail) = match rest.split_once('/') {
            Some((rid, tail)) => (percent_decode(rid), tail.to_string()),
            None => (percent_decode(rest), String::new()),
        };
        if method != "POST" || !(tail == "reply" || tail == "reject") {
            violation = Some(format!("not an answer route: {method} {path}"));
            false
        } else {
            match st.issued.get(&rid) {
                None => {
                    violation = Some(format!("answered {rid}, which the fake never issued"));
                    false
                }
                Some(owner) if !pin.is_empty() && !scope.iter().any(|s| s == owner) => {
                    violation = Some(format!(
                        "answered {rid}, issued for {owner}, outside the pinned {pin}'s scope"
                    ));
                    false
                }
                Some(_) => true,
            }
        }
    } else {
        violation = Some(format!("unknown mutation route: {method} {path}"));
        false
    };

    if let Some(v) = violation {
        st.violations.push(v);
    }
    if !allowed {
        // A violation can never look successful.
        return (500, r#"{"message":"pin guard violation"}"#.to_string());
    }
    if unknown_session {
        return (
            404,
            r#"{"name":"NotFoundError","data":{"message":"no such session"}}"#.to_string(),
        );
    }

    if let Some((_, status)) = st
        .fail_post
        .iter()
        .find(|(s, _)| path.ends_with(s.as_str()))
    {
        let status = *status;
        return (
            status,
            r#"{"_tag":"InvalidRequestError","message":"injected failure"}"#.to_string(),
        );
    }

    if let Some(id) = created {
        let directory = "/".to_string();
        let session = json!({
            "id": id,
            "title": "",
            "directory": directory,
            "parentID": "",
            "time": { "created": 1_700_000_002_000i64, "updated": 1_700_000_002_000i64 },
        });
        st.sessions.insert(id.clone(), session.clone());
        st.order.push(id);
        return (200, session.to_string());
    }
    if path.ends_with("/prompt_async") {
        return (204, String::new());
    }
    (200, "true".to_string())
}

// ---- wire helpers ----

/// Writes an oversized body in 64 KiB chunks, recording how far it got. The
/// client's refusal shows up here as a write error, which is the point: a
/// client that read the whole thing would let this run to completion.
async fn write_flood(
    write: &mut tokio::net::tcp::OwnedWriteHalf,
    state: &Arc<Mutex<FakeState>>,
    flood: Flood,
) {
    let head = if flood.declare_length {
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            flood.bytes
        )
    } else {
        // No Content-Length: the body is close-delimited, so the cap can only
        // be enforced while it is consumed.
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n".to_string()
    };
    if write.write_all(head.as_bytes()).await.is_err() {
        state
            .lock()
            .expect("the fake opencode lock is not poisoned")
            .flooded = Some(0);
        return;
    }
    let chunk = vec![b'x'; 64 * 1024];
    let mut sent = 0usize;
    while sent < flood.bytes {
        let n = chunk.len().min(flood.bytes - sent);
        if write.write_all(&chunk[..n]).await.is_err() {
            break;
        }
        sent += n;
    }
    let _ = write.flush().await;
    state
        .lock()
        .expect("the fake opencode lock is not poisoned")
        .flooded = Some(sent);
}

async fn write_response(
    write: &mut tokio::net::tcp::OwnedWriteHalf,
    status: u16,
    body: &str,
) -> std::io::Result<()> {
    let reason = match status {
        200 => "OK",
        204 => "No Content",
        400 => "Bad Request",
        401 => "Unauthorized",
        404 => "Not Found",
        405 => "Method Not Allowed",
        409 => "Conflict",
        500 => "Internal Server Error",
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

fn basic_header(user: &str, password: &str) -> String {
    use base64::Engine as _;
    format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!("{user}:{password}"))
    )
}

/// The value of one `application/x-www-form-urlencoded` query parameter —
/// the encoding `reqwest`'s `query_pairs_mut` writes (`+` for a space).
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
