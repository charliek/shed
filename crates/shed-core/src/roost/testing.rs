//! An in-process fake `roost-session`, for tests.
//!
//! It is a **test double**, not a server: no lease, no event stream, no PTY, no
//! persistence, one connection handler that reads a line and writes a line. What
//! it is faithful about is the wire — every reply is built from a vendored copy
//! of roost's own golden vector (`crates/fixtures/roost-vectors/`, taken at the
//! rev `roost-ipc` is pinned to), so the shapes here cannot drift away from the
//! shapes roost publishes without the copy step noticing.
//!
//! It listens on **both** a Unix socket and a loopback TCP port, because
//! [`super::conn::Conn`] has two transports and the TCP one goes through a
//! socketpair and a copy pump that a Unix-only test would never touch.
//!
//! Exported to other crates' tests through `shed-core`'s non-default
//! `test-support` feature (`shed-app`'s roost watcher tests are the consumer);
//! `#[cfg(any(test, feature = "test-support"))]` means a shipped binary never
//! carries it.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use base64::Engine as _;
use serde_json::{json, Map, Value};
use tokio::io::{AsyncBufReadExt as _, AsyncRead, AsyncWrite, AsyncWriteExt as _, BufReader};
use tokio::net::{TcpListener, UnixListener};
use tokio::sync::broadcast;
use tokio::task::JoinHandle;

// The vendored vectors. See the README beside them: never semantically edited,
// re-copied on a `roost-ipc` rev bump.
const VECTOR_SESSION_IDENTIFY: &str =
    include_str!("../../../fixtures/roost-vectors/session.identify.response.json");
const VECTOR_IDENTIFY: &str =
    include_str!("../../../fixtures/roost-vectors/identify.response.json");
const VECTOR_TAB_LIST: &str =
    include_str!("../../../fixtures/roost-vectors/tab.list.session.response.json");
const VECTOR_TAB_OPEN: &str =
    include_str!("../../../fixtures/roost-vectors/tab.open.response.json");
const VECTOR_ERROR: &str = include_str!("../../../fixtures/roost-vectors/response.error.json");

fn vector(text: &str) -> Value {
    serde_json::from_str(text).expect("a vendored roost vector is valid JSON")
}

/// A scratch directory that removes itself. Hand-rolled rather than `tempfile`
/// so this module — which compiles into the *library* under `test-support` —
/// adds no non-dev dependency to `shed-core`.
struct ScratchDir(PathBuf);

impl ScratchDir {
    fn new() -> ScratchDir {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        // Short by design: a `sun_path` is 108 bytes, and a socket under a long
        // temp path fails to bind with a confusing error.
        let path = std::env::temp_dir().join(format!(
            "shed-fake-roost-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::create_dir_all(&path).expect("creating the fake roost scratch dir");
        ScratchDir(path)
    }
}

impl Drop for ScratchDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Everything the fake knows, behind one lock so a test can poke it while
/// connections are live.
struct FakeState {
    /// What `session.identify` reports. A test sets this to something other
    /// than roost's own to exercise the compatibility gate.
    session_protocol: u32,
    /// When true, `session.identify` answers `unknown-op` — roost's own tell
    /// for a **UI** socket.
    ui_socket: bool,
    session_id: String,
    started_at: String,
    /// The in-process commit counter. Resets on [`FakeRoost::restart`], exactly
    /// as roost's does, and every mutation below bumps it.
    revision: u64,
    next_tab_id: i64,
    /// roost `Project` values, tabs nested inside — kept as JSON so a test can
    /// set an axis roost adds later without this file changing.
    projects: Vec<Value>,
    /// Bytes `tab.write` delivered, per tab.
    writes: BTreeMap<i64, Vec<u8>>,
}

impl FakeState {
    fn new() -> FakeState {
        let listed = vector(VECTOR_TAB_LIST);
        let identified = vector(VECTOR_SESSION_IDENTIFY);
        let projects = listed["result"]["projects"]
            .as_array()
            .expect("the tab.list vector carries projects")
            .clone();
        let next_tab_id = projects
            .iter()
            .flat_map(|p| p["tabs"].as_array().cloned().unwrap_or_default())
            .filter_map(|t| wire_id(&t["id"]))
            .max()
            .unwrap_or(0)
            + 1;
        FakeState {
            session_protocol: identified["result"]["session_protocol"]
                .as_u64()
                .expect("the session.identify vector carries a protocol")
                as u32,
            ui_socket: false,
            session_id: identified["result"]["session_id"]
                .as_str()
                .expect("the session.identify vector carries a session id")
                .to_string(),
            started_at: identified["result"]["started_at"]
                .as_str()
                .expect("the session.identify vector carries a start time")
                .to_string(),
            revision: listed["result"]["revision"]
                .as_u64()
                .expect("the SESSION variant of tab.list carries a revision"),
            next_tab_id,
            projects,
            writes: BTreeMap::new(),
        }
    }

    fn tab_mut(&mut self, id: i64) -> Option<&mut Value> {
        self.projects
            .iter_mut()
            .filter_map(|p| p["tabs"].as_array_mut())
            .flatten()
            .find(|t| wire_id(&t["id"]) == Some(id))
    }

    fn tab(&self, id: i64) -> Option<&Value> {
        self.projects
            .iter()
            .filter_map(|p| p["tabs"].as_array())
            .flatten()
            .find(|t| wire_id(&t["id"]) == Some(id))
    }
}

/// A string-int64 wire id as a number. Every id on this wire is a *string* so a
/// JavaScript client cannot round it through a lossy `Number`.
fn wire_id(value: &Value) -> Option<i64> {
    value.as_str()?.parse().ok()
}

type Refusal = (String, String);

fn refuse(code: &str, message: impl Into<String>) -> Refusal {
    (code.to_string(), message.into())
}

/// An in-process `roost-session` stand-in, on a Unix socket and a TCP port.
pub struct FakeRoost {
    state: Arc<Mutex<FakeState>>,
    socket_path: PathBuf,
    tcp_port: u16,
    /// Sending on this ends every live connection handler (a test's
    /// [`Self::close_all`]); handlers accepted afterwards are unaffected.
    hangup: broadcast::Sender<()>,
    accepts: Vec<JoinHandle<()>>,
    _dir: ScratchDir,
}

impl FakeRoost {
    /// Bind both listeners and start accepting. The returned value owns the
    /// listeners: drop it and the fake is gone.
    pub async fn start() -> FakeRoost {
        let dir = ScratchDir::new();
        let socket_path = dir.0.join("roost.sock");
        let unix = UnixListener::bind(&socket_path).expect("binding the fake roost socket");
        let tcp = TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("binding the fake roost TCP listener");
        let tcp_port = tcp.local_addr().expect("the fake's TCP port").port();

        let state = Arc::new(Mutex::new(FakeState::new()));
        let (hangup, _) = broadcast::channel(8);

        let accepts = vec![
            tokio::spawn({
                let state = Arc::clone(&state);
                let hangup = hangup.clone();
                async move {
                    while let Ok((stream, _)) = unix.accept().await {
                        tokio::spawn(serve(stream, Arc::clone(&state), hangup.subscribe()));
                    }
                }
            }),
            tokio::spawn({
                let state = Arc::clone(&state);
                let hangup = hangup.clone();
                async move {
                    while let Ok((stream, _)) = tcp.accept().await {
                        tokio::spawn(serve(stream, Arc::clone(&state), hangup.subscribe()));
                    }
                }
            }),
        ];

        FakeRoost {
            state,
            socket_path,
            tcp_port,
            hangup,
            accepts,
            _dir: dir,
        }
    }

    /// The Unix socket path — what a `LocalSession` reach would hand back.
    pub fn socket_path(&self) -> &Path {
        &self.socket_path
    }

    /// The loopback TCP port — what a `FixedPort` reach would hand back.
    pub fn tcp_port(&self) -> u16 {
        self.tcp_port
    }

    /// The daemon-instance id `session.identify` currently reports.
    pub fn session_id(&self) -> String {
        self.lock().session_id.clone()
    }

    /// The commit revision `tab.list` currently reports.
    pub fn revision(&self) -> u64 {
        self.lock().revision
    }

    /// Report a different `session_protocol` — the compatibility gate's input.
    pub fn set_session_protocol(&self, protocol: u32) {
        self.lock().session_protocol = protocol;
    }

    /// Answer `unknown-op` to `session.identify`, i.e. behave like a roost **UI**
    /// socket rather than a session socket.
    pub fn serve_as_ui_socket(&self, ui: bool) {
        self.lock().ui_socket = ui;
    }

    /// Set a tab's agent axes — the whole point of the fake for a status test.
    /// `ownership` is roost's `Ownership` object (or `None` for a plain shell
    /// tab). Commits a new revision, as the real mutation would.
    pub fn set_tab_axes(
        &self,
        tab_id: i64,
        lifecycle: &str,
        ownership: Option<Value>,
        has_notification: bool,
    ) {
        let mut state = self.lock();
        state.revision += 1;
        let Some(tab) = state.tab_mut(tab_id) else {
            panic!("no tab {tab_id} to set axes on");
        };
        tab["agent_lifecycle"] = json!(lifecycle);
        tab["has_notification"] = json!(has_notification);
        match ownership {
            Some(ownership) => tab["ownership"] = ownership,
            None => {
                if let Some(object) = tab.as_object_mut() {
                    object.remove("ownership");
                }
            }
        }
    }

    /// Commit a revision with no visible change — an empty batch, which roost
    /// pushes too (that is what makes a gap mean loss and nothing else).
    pub fn bump_revision(&self) {
        self.lock().revision += 1;
    }

    /// Restart the daemon: a new `session_id`, `revision` back to 1, **tab ids
    /// unchanged**. Both halves are real roost behaviour — ids are persisted,
    /// the revision counter is in-process — and together they are why a client
    /// fences per connection and keys rows by tab id.
    pub fn restart(&self) {
        let mut state = self.lock();
        state.session_id = format!("{}-restart-{}", state.session_id, state.revision);
        state.revision = 1;
    }

    /// Hang up on every connection that is live right now.
    pub fn close_all(&self) {
        // `Err` only means nobody is connected — which is the state this asks
        // for, so it is not a failure.
        let _ = self.hangup.send(());
    }

    /// The bytes `tab.write` delivered to a tab, in order.
    pub fn written(&self, tab_id: i64) -> Vec<u8> {
        self.lock().writes.get(&tab_id).cloned().unwrap_or_default()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, FakeState> {
        self.state
            .lock()
            .expect("the fake roost lock is not poisoned")
    }
}

impl Drop for FakeRoost {
    fn drop(&mut self) {
        let _ = self.hangup.send(());
        for task in &self.accepts {
            task.abort();
        }
    }
}

/// One connection: read a line, answer it, until EOF or a hang-up.
async fn serve<S>(stream: S, state: Arc<Mutex<FakeState>>, mut hangup: broadcast::Receiver<()>)
where
    S: AsyncRead + AsyncWrite + Send + Unpin + 'static,
{
    let (read, mut write) = tokio::io::split(stream);
    let mut lines = BufReader::new(read).lines();
    loop {
        let line = tokio::select! {
            _ = hangup.recv() => return,
            next = lines.next_line() => match next {
                Ok(Some(line)) => line,
                _ => return,
            },
        };
        let request: Value = match serde_json::from_str(&line) {
            Ok(request) => request,
            Err(e) => {
                let body = error_envelope(&Value::Null, refuse("parse-error", e.to_string()));
                if write_line(&mut write, &body).await.is_err() {
                    return;
                }
                continue;
            }
        };
        let id = request.get("id").cloned().unwrap_or(Value::Null);
        let op = request.get("op").and_then(Value::as_str).unwrap_or("");
        let params = request.get("params").cloned().unwrap_or(json!({}));
        let body = match dispatch(&state, op, &params) {
            Ok(result) => json!({ "id": id, "ok": true, "result": result }),
            Err(refusal) => error_envelope(&id, refusal),
        };
        if write_line(&mut write, &body).await.is_err() {
            return;
        }
    }
}

/// The refusal envelope, built by overwriting the vendored error vector's own
/// fields — so the key names come from roost's wire, not from memory.
fn error_envelope(id: &Value, (code, message): Refusal) -> Value {
    let mut envelope = vector(VECTOR_ERROR);
    envelope["id"] = id.clone();
    envelope["error"]["code"] = json!(code);
    envelope["error"]["message"] = json!(message);
    envelope
}

async fn write_line<W: AsyncWrite + Unpin>(write: &mut W, body: &Value) -> std::io::Result<()> {
    let mut bytes = serde_json::to_vec(body).expect("the fake's replies serialize");
    bytes.push(b'\n');
    write.write_all(&bytes).await
}

fn dispatch(state: &Mutex<FakeState>, op: &str, params: &Value) -> Result<Value, Refusal> {
    let mut state = state.lock().expect("the fake roost lock is not poisoned");
    match op {
        "session.identify" => {
            if state.ui_socket {
                // Exactly what a roost UI socket answers, and the only way a
                // client can tell the two sockets apart.
                return Err(refuse("unknown-op", "no such op: session.identify"));
            }
            let mut result = vector(VECTOR_SESSION_IDENTIFY)["result"].clone();
            result["session_protocol"] = json!(state.session_protocol);
            result["session_id"] = json!(state.session_id);
            result["started_at"] = json!(state.started_at);
            Ok(result)
        }
        "identify" => Ok(vector(VECTOR_IDENTIFY)["result"].clone()),
        "tab.list" => Ok(json!({
            "projects": state.projects,
            "revision": state.revision,
        })),
        "tab.dump" => {
            let id = params_tab_id(params)?;
            let tab = state
                .tab(id)
                .ok_or_else(|| refuse("not-found", format!("no such tab: {id}")))?;
            let title = tab["title"].as_str().unwrap_or_default().to_string();
            let cwd = tab["cwd"].as_str().unwrap_or_default().to_string();
            let rows_text = vec![
                format!("tab {id} {title}"),
                format!("cwd {cwd}"),
                String::new(),
            ];
            Ok(json!({
                "cols": 80,
                "rows": rows_text.len(),
                "cursor": { "row": 0, "col": 0, "visible": true },
                "rows_text": rows_text,
            }))
        }
        "tab.open" => {
            let id = state.next_tab_id;
            state.next_tab_id += 1;
            state.revision += 1;
            let project_id = params
                .get("project_id")
                .and_then(wire_id)
                .filter(|id| *id != 0);
            let mut tab = vector(VECTOR_TAB_OPEN)["result"]["tab"].clone();
            tab["id"] = json!(id.to_string());
            tab["cwd"] = params.get("cwd").cloned().unwrap_or(json!(""));
            tab["title"] = params.get("title").cloned().unwrap_or(json!(""));
            // A defaulted / unknown project id lands in the first project, the
            // way roost's own "no project named" default does.
            let index = project_id
                .and_then(|wanted| {
                    state
                        .projects
                        .iter()
                        .position(|p| wire_id(&p["id"]) == Some(wanted))
                })
                .unwrap_or(0);
            let project = state
                .projects
                .get_mut(index)
                .ok_or_else(|| refuse("not-found", "the fake has no projects"))?;
            tab["project_id"] = project["id"].clone();
            project["tabs"]
                .as_array_mut()
                .ok_or_else(|| refuse("internal", "a project with no tabs array"))?
                .push(tab.clone());
            Ok(json!({ "tab": tab }))
        }
        "tab.close" => {
            let id = params_tab_id(params)?;
            let mut removed = false;
            for project in &mut state.projects {
                if let Some(tabs) = project["tabs"].as_array_mut() {
                    let before = tabs.len();
                    tabs.retain(|t| wire_id(&t["id"]) != Some(id));
                    removed |= tabs.len() != before;
                }
            }
            if !removed {
                return Err(refuse("not-found", format!("no such tab: {id}")));
            }
            state.revision += 1;
            Ok(json!({}))
        }
        "tab.write" => {
            let id = params_tab_id(params)?;
            let encoded = params
                .get("data")
                .and_then(Value::as_str)
                .ok_or_else(|| refuse("invalid-param", "tab.write needs base64 `data`"))?;
            let bytes = base64::engine::general_purpose::STANDARD
                .decode(encoded)
                .map_err(|e| refuse("invalid-param", format!("tab.write data: {e}")))?;
            state
                .writes
                .entry(id)
                .or_default()
                .extend_from_slice(&bytes);
            Ok(json!({}))
        }
        // Everything else, `session.connect` and `events.subscribe` included:
        // the fake serves inventory and one-shots, and a watcher that reaches
        // for the lease should fail loudly in a test rather than pass.
        other => Err(refuse("unknown-op", format!("no such op: {other}"))),
    }
}

fn params_tab_id(params: &Value) -> Result<i64, Refusal> {
    params
        .get("tab_id")
        .and_then(wire_id)
        .ok_or_else(|| refuse("invalid-param", "a string-int64 `tab_id` is required"))
}

/// A minimal `Ownership` object, for a test that wants an agent-owned tab
/// without hand-writing the shape.
pub fn ownership(source: &str, session_id: &str, detail: &str, last_event_at: i64) -> Value {
    let mut object = Map::new();
    object.insert("source".into(), json!(source));
    object.insert("session_id".into(), json!(session_id));
    object.insert("last_event_at".into(), json!(last_event_at));
    object.insert("detail".into(), json!(detail));
    object.insert("metadata".into(), json!({}));
    Value::Object(object)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The vectors are the fake's whole claim to fidelity — if one stops
    /// parsing, or loses the key the fake reads out of it, that has to fail
    /// here rather than as a confusing decode error three layers up.
    #[test]
    fn the_vendored_vectors_carry_what_the_fake_reads() {
        let state = FakeState::new();
        assert_eq!(state.revision, 42, "the session tab.list vector's revision");
        assert_eq!(state.session_protocol, 2);
        assert!(!state.session_id.is_empty());
        assert_eq!(state.next_tab_id, 6, "one past the vector's highest tab id");
        assert!(state.tab(5).is_some());
        assert!(vector(VECTOR_ERROR)["error"]["code"].is_string());
        assert!(vector(VECTOR_IDENTIFY)["result"]["app_label"].is_string());
    }

    #[tokio::test]
    async fn axes_are_settable_and_commit_a_revision() {
        let fake = FakeRoost::start().await;
        let before = fake.revision();

        fake.set_tab_axes(
            5,
            "working",
            Some(ownership("opencode", "ses_1", "session_status", 1700000060)),
            true,
        );
        assert_eq!(fake.revision(), before + 1);
        {
            let state = fake.lock();
            let tab = state.tab(5).expect("tab 5");
            assert_eq!(tab["agent_lifecycle"], json!("working"));
            assert_eq!(tab["ownership"]["source"], json!("opencode"));
            assert_eq!(tab["has_notification"], json!(true));
        }

        // Clearing ownership REMOVES the key: a plain shell tab carries no
        // `ownership` at all on roost's wire, it does not carry a null one.
        fake.set_tab_axes(5, "inactive", None, false);
        let state = fake.lock();
        assert!(state.tab(5).expect("tab 5").get("ownership").is_none());
    }
}
