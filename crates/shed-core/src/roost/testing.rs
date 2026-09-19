//! An in-process fake `roost-session`, for tests.
//!
//! It is a **test double**, not a server: no PTY, no persistence, one connection
//! handler that reads a line and writes a line — until `events.subscribe` flips
//! it into a one-way push stream, which is the one place the shape genuinely
//! differs. What it is faithful about is the wire — every reply and every pushed
//! envelope is built from a vendored copy of roost's own golden vector
//! (`crates/fixtures/roost-vectors/`, taken at the rev `roost-ipc` is pinned to),
//! so the shapes here cannot drift away from the shapes roost publishes without
//! the copy step noticing.
//!
//! At session protocol 6 the wire it speaks is unowned — the lease, its takeover
//! table and its one tombstone retired with generation 4 — so what is left to be
//! faithful about is the stream:
//!
//! * **`events.subscribe` registers, it does not classify.** The ack's
//!   `revision` and `session_id` and the subscriber's registration are taken
//!   under **one** state lock, so no mutation can commit between the number a
//!   client fences on and the first frame it is eligible to receive — a fake
//!   that let one slip through would manufacture the very gap the resync tests
//!   exist to distinguish from a real one.
//! * **Every mutation commits exactly one batch** at the new revision, built
//!   from the vendored envelope vectors. Empty commits are pushed too, because
//!   that is what makes a skipped revision mean loss and nothing else.
//! * **Every write op is open to every connection.** `tab.write` and
//!   `session.set_agent_hooks` take no authority, two connections can both drive
//!   the same tab, and the last writer wins — which is the one behaviour
//!   generation 5 actually changed and the one the deleted refusal table was
//!   standing in front of.
//! * **`session.set_agent_hooks` validates its params the way roost does.**
//!   Generation 6 made it a raise: `{agents, client}`, `deny_unknown_fields`, a
//!   non-empty `agents` with no blank element. A fake that took the old
//!   `{mode, skip}` shape would let a stale client pass here and fail on a real
//!   host, so the two refusals roost spells out — `invalid-param` for a bad
//!   `agents`/`client`, `unknown-field` for a retired key — are spelled out
//!   here too.
//!
//! It listens on **both** a Unix socket and a loopback TCP port, because
//! [`super::conn::Conn`] has two transports and the TCP one goes through a
//! socketpair and a copy pump that a Unix-only test would never touch.
//!
//! Exported to other crates' tests through `shed-core`'s non-default
//! `test-support` feature (`shed-app`'s roost watcher tests are the consumer);
//! `#[cfg(any(test, feature = "test-support"))]` means a shipped binary never
//! carries it.

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use base64::Engine as _;
use serde_json::{json, Map, Value};
use tokio::io::{AsyncBufReadExt as _, AsyncRead, AsyncWrite, AsyncWriteExt as _, BufReader};
use tokio::net::{TcpListener, UnixListener};
use tokio::sync::broadcast;
use tokio::task::JoinHandle;

// The vendored vectors. See the README beside them: never semantically edited,
// re-copied on a `roost-ipc` rev bump. `session.identify` is generation-suffixed
// upstream and shed vendors only the current one — a protocol-2 daemon is a
// control here ([`FakeRoost::set_session_protocol`]), not a second vector.
const VECTOR_SESSION_IDENTIFY: &str =
    include_str!("../../../fixtures/roost-vectors/session.identify.response.v6.json");
const VECTOR_IDENTIFY: &str =
    include_str!("../../../fixtures/roost-vectors/identify.response.json");
const VECTOR_TAB_LIST: &str =
    include_str!("../../../fixtures/roost-vectors/tab.list.session.response.json");
const VECTOR_TAB_OPEN: &str =
    include_str!("../../../fixtures/roost-vectors/tab.open.response.json");
const VECTOR_ERROR: &str = include_str!("../../../fixtures/roost-vectors/response.error.json");
const VECTOR_SET_AGENT_HOOKS: &str =
    include_str!("../../../fixtures/roost-vectors/session.set_agent_hooks.response.json");
const VECTOR_EVENTS_SUBSCRIBE: &str =
    include_str!("../../../fixtures/roost-vectors/events.subscribe.response.json");
const VECTOR_EVENTS_BATCH: &str = include_str!("../../../fixtures/roost-vectors/events.batch.json");
const VECTOR_TAB_OPENED: &str =
    include_str!("../../../fixtures/roost-vectors/tab.opened.event.json");
const VECTOR_TAB_CLOSED: &str =
    include_str!("../../../fixtures/roost-vectors/shed.tab.closed.event.json");
const VECTOR_TAB_NOTIFICATION: &str =
    include_str!("../../../fixtures/roost-vectors/shed.tab.notification.event.json");
const VECTOR_AGENT_REPORT_CHANGED: &str =
    include_str!("../../../fixtures/roost-vectors/agent_report.changed.event.json");
const VECTOR_SESSION_STOPPING: &str =
    include_str!("../../../fixtures/roost-vectors/session.stopping.event.json");
const VECTOR_STREAM_ENDED: &str =
    include_str!("../../../fixtures/roost-vectors/stream.ended.event.json");
const VECTOR_TABS_REORDERED: &str =
    include_str!("../../../fixtures/roost-vectors/tabs.reordered.event.json");
const VECTOR_PROJECTS_REORDERED: &str =
    include_str!("../../../fixtures/roost-vectors/projects.reordered.event.json");

fn vector(text: &str) -> Value {
    serde_json::from_str(text).expect("a vendored roost vector is valid JSON")
}

/// One pushed frame, already serialized. `Arc` because a `broadcast` clones the
/// value for every subscriber and a batch is the same bytes for all of them.
type Frame = Arc<String>;

/// The default fan-out depth. Small on purpose — roost's own relay queue is
/// shallow — but large enough that no ordinary test trips the lag rule; the one
/// that means to trip it asks for [`FakeRoost::start_with_frame_capacity`].
const DEFAULT_FRAME_CAPACITY: usize = 64;

/// How long one pushed frame may take to reach a peer before that subscriber is
/// treated as gone. Generous next to any in-process write and far shorter than
/// a test's own timeouts, so a wedged peer surfaces as a closed stream — the
/// answer roost's stall budget gives — rather than as a hung test.
const STREAM_WRITE_DEADLINE: std::time::Duration = std::time::Duration::from_secs(2);

/// A scratch directory that removes itself. Hand-rolled rather than `tempfile`
/// so this module — which compiles into the *library* under `test-support` —
/// adds no non-dev dependency to `shed-core`.
///
/// Shared with `roost::bootstrap`'s hermetic rig, which needs the same thing for
/// the same reason; the prefix is what tells two scratch dirs apart in a `/tmp`
/// listing when something goes wrong.
pub(crate) struct ScratchDir(pub(crate) PathBuf);

impl ScratchDir {
    fn new() -> ScratchDir {
        ScratchDir::with_prefix("shed-fake-roost")
    }

    pub(crate) fn with_prefix(prefix: &str) -> ScratchDir {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        // Short by design: a `sun_path` is 108 bytes, and a socket under a long
        // temp path fails to bind with a confusing error.
        let path = std::env::temp_dir().join(format!(
            "{prefix}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::create_dir_all(&path).expect("creating a scratch dir");
        ScratchDir(path)
    }
}

impl Drop for ScratchDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Write a test script and make it executable.
///
/// Shared by `roost::bootstrap`'s two test rigs, which both invented this same
/// six lines independently: `create_dir_all`s the parent first (a no-op where
/// the caller already made it, load-bearing where a fixture binary's directory
/// — `app/`, `.local/bin/` — does not exist yet), then writes and `chmod +x`es.
///
/// `#[cfg(test)]` rather than living under the module's `test-support` gate
/// alone: both callers are `shed-core`'s own `#[cfg(test)]` test modules, never
/// an external crate, so under a build that only turns on `test-support`
/// (feature-unified in for `FakeRoost`, with no `#[cfg(test)]` code around to
/// call this) it would otherwise be unreachable dead code.
#[cfg(test)]
pub(crate) fn write_exec(path: &Path, body: &str) {
    use std::os::unix::fs::PermissionsExt as _;

    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).expect("mkdir for a test script");
    }

    // **Stage and rename, rather than write in place** — so a reader never sees
    // a half-written script, and so the name appears only once its contents are
    // complete and executable.
    //
    // This does NOT fix the `ETXTBSY` flake in this rig, and it is worth saying
    // so here rather than letting the next person assume it did. That race is
    // about the INODE, not the name: a sibling test thread calling
    // `Command::spawn` forks, the child inherits every open descriptor,
    // and exec'ing a file that any descriptor still holds open for writing
    // fails "Text file busy". Renaming moves the name onto the same inode the
    // forked child may be holding, so the window survives.
    //
    // Measured on this rig: `the_sibling_rung_is_taken_when_it_speaks_the_current_protocol`
    // fails 0 times in 20 runs on its own and about 1 in 10 inside the full
    // `roost::bootstrap` suite, which is the signature of exactly this race —
    // it needs concurrent forks to appear. The failure is invisible in the
    // assertion because the sibling rung is *designed* to fall through when its
    // local `identify` fails, so an exec error reads as "no sibling here".
    //
    // Closing it properly means not exec'ing a file this process wrote while
    // other threads are forking — a rig-level change (exec from a
    // pre-populated, read-only fixture directory) rather than a one-line fix,
    // and out of scope for the commit that noticed it.
    let staging = path.with_file_name(format!(
        ".{}.{}.{:?}.staging",
        path.file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("script"),
        std::process::id(),
        std::thread::current().id(),
    ));
    std::fs::write(&staging, body)
        .unwrap_or_else(|error| panic!("writing {}: {error}", staging.display()));
    std::fs::set_permissions(&staging, std::fs::Permissions::from_mode(0o755))
        .expect("chmod +x a test script");
    std::fs::rename(&staging, path).unwrap_or_else(|error| {
        let _ = std::fs::remove_file(&staging);
        panic!("renaming into {}: {error}", path.display())
    });
}

/// A one-shot seam that runs under the state lock just before a `tab.list`
/// reply. See [`FakeRoost::before_tab_list`].
type TabListHookFn = Box<dyn FnOnce(&mut TabListHook<'_>) + Send>;

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
    /// Every `session.set_agent_hooks` this fake has served, params verbatim —
    /// the assertion that shed sent the five names and its own `client` label,
    /// and that it sent it exactly when it says it does.
    agent_hooks_calls: Vec<Value>,
    /// Every `tab.open` this fake has served, params verbatim. The only way to
    /// assert a key is **absent** from a launch — `activate` is
    /// `skip_serializing_if`, so a struct literal says nothing about the bytes.
    tab_open_calls: Vec<Value>,
    /// What the next `session.set_agent_hooks` answers with, on top of the
    /// vendored vector's shape. `None` is the vector as recorded.
    agent_hooks_result: Option<Value>,
    /// Registered event streams, by connection id. Unclassified at generation 5
    /// — every subscriber gets every frame, `tab.effect` included.
    streams: BTreeSet<u64>,
    /// How many more `events.subscribe` acks answer from a **fresh**
    /// incarnation. See [`FakeRoost::restart_between_dials`].
    restarts_between_dials: u32,
    /// Bumped by each of those, so the moved id is unique however many times it
    /// moves.
    dial_restarts: u64,
    /// How many `tab.list` replies have been served — the assertion that the
    /// watcher reads the inventory once per cycle and not per event.
    tab_list_calls: usize,
    /// Runs once, under this lock, just before the next `tab.list` reply.
    before_tab_list: Option<TabListHookFn>,
    /// Pushed frames. In the state so a commit's revision bump and its batch
    /// leave the lock together — see the module doc.
    frames: broadcast::Sender<Frame>,
}

impl FakeState {
    fn new(frame_capacity: usize) -> FakeState {
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
        let (frames, _) = broadcast::channel(frame_capacity);
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
            agent_hooks_calls: Vec::new(),
            tab_open_calls: Vec::new(),
            agent_hooks_result: None,
            streams: BTreeSet::new(),
            restarts_between_dials: 0,
            dial_restarts: 0,
            tab_list_calls: 0,
            before_tab_list: None,
            frames,
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

    /// Commit one revision and push it as exactly one batch — including an empty
    /// one, which roost pushes too so a skipped number always means loss.
    ///
    /// The bump and the push are one operation on purpose: they happen under the
    /// caller's state lock, so a subscriber registered at revision `n` sees
    /// `n + 1` next and nothing in between.
    fn commit(&mut self, events: Vec<Value>) {
        self.revision += 1;
        let mut batch = vector(VECTOR_EVENTS_BATCH);
        batch["revision"] = json!(self.revision);
        batch["events"] = Value::Array(events);
        self.push(&batch);
    }

    /// Push one already-shaped frame to every registered stream.
    fn push(&self, frame: &Value) {
        // `Err` only means nobody is subscribed, which is an ordinary state.
        let _ = self
            .frames
            .send(Arc::new(serde_json::to_string(frame).expect("a frame")));
    }
}

/// What a [`FakeRoost::before_tab_list`] hook may do while the state lock is
/// held. Deliberately tiny: the seam exists to commit a mutation *between* a
/// subscribe ack and the `tab.list` reply it is fenced against, and anything
/// wider would be a second way to drive the fake.
pub struct TabListHook<'a> {
    state: &'a mut FakeState,
}

impl TabListHook<'_> {
    /// Commit a revision with no visible change — the cheapest mutation that
    /// still moves the fence.
    pub fn bump_revision(&mut self) {
        self.state.commit(Vec::new());
    }

    /// The revision this hook is about to let `tab.list` report.
    pub fn revision(&self) -> u64 {
        self.state.revision
    }

    /// Registered event streams, read from **inside** the `tab.list` lock.
    ///
    /// This is how a test proves the *ordering* of a client's prologue rather
    /// than only its arithmetic: a client that subscribed before listing has a
    /// stream registered by the time this runs, and one that listed first has
    /// none. [`FakeRoost::stream_count`] cannot answer it — that takes the same
    /// lock this hook is already holding.
    pub fn stream_count(&self) -> usize {
        self.state.streams.len()
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
    /// Latched by [`Self::stop`], lifted by [`Self::restart`]. A stopped daemon
    /// accepts nothing, so an accepted connection is dropped at once.
    stopped: Arc<AtomicBool>,
    accepts: Vec<JoinHandle<()>>,
    _dir: ScratchDir,
}

impl FakeRoost {
    /// Bind both listeners and start accepting. The returned value owns the
    /// listeners: drop it and the fake is gone.
    pub async fn start() -> FakeRoost {
        FakeRoost::start_with_frame_capacity(DEFAULT_FRAME_CAPACITY).await
    }

    /// [`Self::start`] with the push fan-out's depth chosen — for the one test
    /// whose subject is what happens when a subscriber falls behind it.
    pub async fn start_with_frame_capacity(frame_capacity: usize) -> FakeRoost {
        let dir = ScratchDir::new();
        let socket_path = dir.0.join("roost.sock");
        let unix = UnixListener::bind(&socket_path).expect("binding the fake roost socket");
        let tcp = TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("binding the fake roost TCP listener");
        let tcp_port = tcp.local_addr().expect("the fake's TCP port").port();

        let state = Arc::new(Mutex::new(FakeState::new(frame_capacity)));
        let (hangup, _) = broadcast::channel(8);
        let stopped = Arc::new(AtomicBool::new(false));
        // Connection ids, so a takeover can spare the connection that asked for
        // it the way roost spares the requester.
        let next_conn = Arc::new(AtomicU64::new(1));

        let accepts = vec![
            tokio::spawn({
                let state = Arc::clone(&state);
                let hangup = hangup.clone();
                let stopped = Arc::clone(&stopped);
                let next_conn = Arc::clone(&next_conn);
                async move {
                    while let Ok((stream, _)) = unix.accept().await {
                        if stopped.load(Ordering::SeqCst) {
                            continue;
                        }
                        let id = next_conn.fetch_add(1, Ordering::SeqCst);
                        tokio::spawn(serve(stream, id, Arc::clone(&state), hangup.subscribe()));
                    }
                }
            }),
            tokio::spawn({
                let state = Arc::clone(&state);
                let hangup = hangup.clone();
                let stopped = Arc::clone(&stopped);
                let next_conn = Arc::clone(&next_conn);
                async move {
                    while let Ok((stream, _)) = tcp.accept().await {
                        if stopped.load(Ordering::SeqCst) {
                            continue;
                        }
                        let id = next_conn.fetch_add(1, Ordering::SeqCst);
                        tokio::spawn(serve(stream, id, Arc::clone(&state), hangup.subscribe()));
                    }
                }
            }),
        ];

        FakeRoost {
            state,
            socket_path,
            tcp_port,
            hangup,
            stopped,
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

    /// How many `tab.list` replies have been served since the fake started.
    ///
    /// The observer loop reads the inventory **once per cycle** and folds
    /// everything after that off the stream, so this is what proves a pushed
    /// change did not quietly cost a re-read.
    pub fn tab_list_calls(&self) -> usize {
        self.lock().tab_list_calls
    }

    /// Registered event streams — one per subscribed connection.
    ///
    /// Unclassified at generation 5: the driver/observer split went with the
    /// lease, so this is the whole of what a test can assert about who is
    /// listening.
    pub fn stream_count(&self) -> usize {
        self.lock().streams.len()
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
    /// tab). Commits a new revision and pushes it as one batch: an
    /// `agent_report.changed`, plus a `tab.notification` when the sticky bit
    /// actually moved.
    pub fn set_tab_axes(
        &self,
        tab_id: i64,
        lifecycle: &str,
        ownership: Option<Value>,
        has_notification: bool,
    ) {
        let mut state = self.lock();
        let Some(tab) = state.tab_mut(tab_id) else {
            panic!("no tab {tab_id} to set axes on");
        };
        let notification_moved = tab["has_notification"].as_bool() != Some(has_notification);
        tab["agent_lifecycle"] = json!(lifecycle);
        tab["has_notification"] = json!(has_notification);
        match ownership.clone() {
            Some(ownership) => tab["ownership"] = ownership,
            None => {
                if let Some(object) = tab.as_object_mut() {
                    object.remove("ownership");
                }
            }
        }
        let shell_state = tab["shell_state"].clone();
        let hook_active = tab["hook_active"].clone();

        let mut report = vector(VECTOR_AGENT_REPORT_CHANGED);
        report["data"]["tab_id"] = json!(tab_id.to_string());
        report["data"]["agent_lifecycle"] = json!(lifecycle);
        if !shell_state.is_null() {
            report["data"]["shell_state"] = shell_state;
        }
        if !hook_active.is_null() {
            report["data"]["hook_active"] = hook_active;
        }
        match ownership {
            Some(ownership) => report["data"]["ownership"] = ownership,
            None => {
                if let Some(object) = report["data"].as_object_mut() {
                    object.remove("ownership");
                }
            }
        }
        let mut events = vec![report];
        if notification_moved {
            let mut fired = vector(VECTOR_TAB_NOTIFICATION);
            fired["data"]["tab_id"] = json!(tab_id.to_string());
            fired["data"]["has_pending"] = json!(has_notification);
            events.push(fired);
        }
        state.commit(events);
    }

    /// Commit a revision with no visible change — an empty batch, which roost
    /// pushes too (that is what makes a gap mean loss and nothing else).
    pub fn bump_revision(&self) {
        self.lock().commit(Vec::new());
    }

    /// Advance the commit counter **without pushing a batch**, so the next batch
    /// arrives with a hole in the sequence. The only way to manufacture the loss
    /// a resync exists for — roost itself never does this, it closes instead.
    pub fn skip_revision(&self) {
        self.lock().revision += 1;
    }

    /// Reverse the first project's tab order and commit it as roost does: a
    /// `tabs.reordered` naming the whole post-reorder sequence.
    ///
    /// The event names the SET, not a member, which is why no client folds it
    /// into a row. The fake really does reorder its own tabs so a re-list
    /// observes the new order.
    pub fn reorder_tabs(&self) {
        let mut state = self.lock();
        let Some(project) = state.projects.first_mut() else {
            panic!("the fake has no projects to reorder");
        };
        let project_id = project["id"].clone();
        let Some(tabs) = project["tabs"].as_array_mut() else {
            panic!("a project with no tabs array");
        };
        tabs.reverse();
        let order: Vec<Value> = tabs.iter().map(|t| t["id"].clone()).collect();
        let mut envelope = vector(VECTOR_TABS_REORDERED);
        envelope["data"]["project_id"] = project_id;
        envelope["data"]["tab_ids"] = Value::Array(order);
        state.commit(vec![envelope]);
    }

    /// Reverse the project order and commit it as a `projects.reordered`.
    pub fn reorder_projects(&self) {
        let mut state = self.lock();
        state.projects.reverse();
        let order: Vec<Value> = state.projects.iter().map(|p| p["id"].clone()).collect();
        let mut envelope = vector(VECTOR_PROJECTS_REORDERED);
        envelope["data"]["project_ids"] = Value::Array(order);
        state.commit(vec![envelope]);
    }

    /// Every `session.set_agent_hooks` this fake has served, params verbatim.
    ///
    /// The assertion that shed asked for what it says it asks for — the five
    /// [`ROOST_WIRED_AGENTS`](crate::roost::bootstrap::ROOST_WIRED_AGENTS) and
    /// its own `client` label — and, just as important, that it asked **only**
    /// where consent was given: an empty list is what "no hook op happened
    /// without consent" looks like.
    pub fn agent_hooks_calls(&self) -> Vec<Value> {
        self.lock().agent_hooks_calls.clone()
    }

    /// Every `tab.open` this fake has served, params verbatim.
    ///
    /// The one way to assert a key is **absent** from a launch. `TabOpenParams`
    /// declares `activate` `skip_serializing_if = "Option::is_none"`, so the
    /// difference between "shed sent no `activate`" and "shed sent `null`" is
    /// only visible in the bytes the host received — a struct literal, or a
    /// decoded params object, cannot tell them apart.
    pub fn tab_open_calls(&self) -> Vec<Value> {
        self.lock().tab_open_calls.clone()
    }

    /// Answer the next `session.set_agent_hooks` with this `result` instead of
    /// the vendored vector's.
    ///
    /// For the partial-failure row: roost reports per-agent `errors` inside a
    /// **successful** reply, and a client that treated a non-empty `errors` as a
    /// failed call would throw away four wired agents over one that was not.
    pub fn set_agent_hooks_result(&self, result: Value) {
        self.lock().agent_hooks_result = Some(result);
    }

    /// Push roost's **other** terminal control frame: `stream.ended`, with this
    /// `reason` (roost's own is `"backend-switch"`).
    ///
    /// It pushes the envelope and **nothing else** — no hang-up, no latch, no
    /// revision bump. That is the whole point of it being separate from
    /// [`Self::stop`]: the property under test is that a client answers
    /// `EventFrame::Ended` with a *resync* rather than a Down, and a fake that
    /// also closed the connection would make every client pass, because the EOF
    /// alone is already a resync. What is left is the frame, on a daemon that is
    /// still answering, which is exactly the shape a real UI socket produces
    /// when it moves its tabs to another backend.
    ///
    /// Built from roost's own vendored envelope rather than hand-constructed,
    /// like every other frame this fake pushes.
    pub fn end_stream(&self, reason: &str) {
        let state = self.lock();
        let mut envelope = vector(VECTOR_STREAM_ENDED);
        envelope["data"]["reason"] = json!(reason);
        state.push(&envelope);
    }

    /// The daemon is stopping: every stream gets the terminal
    /// `session.stopping{reason: "stop"}`, every connection is hung up, and the
    /// fake **latches unavailable** — new dials get nothing — until
    /// [`Self::restart`]. A stopped daemon accepts nothing, and a fake that kept
    /// answering would make "further attempts fail" untestable.
    pub fn stop(&self) {
        {
            let state = self.lock();
            let mut envelope = vector(VECTOR_SESSION_STOPPING);
            envelope["data"]["reason"] = json!("stop");
            state.push(&envelope);
        }
        self.stopped.store(true, Ordering::SeqCst);
        let _ = self.hangup.send(());
    }

    /// Restart the daemon: a new `session_id`, `revision` back to 1, **tab ids
    /// unchanged**, every live connection hung up, and any [`Self::stop`] latch
    /// lifted.
    ///
    /// All of it is real roost behaviour — ids are persisted, the revision
    /// counter is in-process, and a restart is a new process, so what a client
    /// sees is an EOF. Together they are why a client fences per connection and
    /// keys rows by tab id.
    pub fn restart(&self) {
        {
            let mut state = self.lock();
            state.session_id = format!("{}-restart-{}", state.session_id, state.revision);
            state.revision = 1;
            state.streams.clear();
        }
        self.stopped.store(false, Ordering::SeqCst);
        let _ = self.hangup.send(());
    }

    /// Restart the daemon **between a client's two dials**, for the next
    /// `times` subscribes: `session.identify` reports one incarnation and the
    /// `events.subscribe` ack that follows reports another.
    ///
    /// This is the one thing [`Self::restart`] cannot express. A restart moves
    /// the id and hangs everybody up, so a client that re-dials afterwards sees
    /// one consistent incarnation. What a real restart landing *between* a
    /// client's identify and its subscribe produces is a **mismatched pair** —
    /// a snapshot from the process that is gone and a stream from the one that
    /// replaced it — and the only thing that tells a client about it is the
    /// `session_id` on the ack.
    ///
    /// Reduced to that one axis on purpose. It moves the id and nothing else:
    /// it does not reset the revision (that would mix a second resync cause,
    /// the gap, into a test about the first) and it does not hang up the live
    /// connection the client identified on. A client that only recovered
    /// because its control leg died under it would be passing by luck, and the
    /// property under test is the comparison.
    pub fn restart_between_dials(&self, times: u32) {
        self.lock().restarts_between_dials = times;
    }

    /// Hang up on every connection that is live right now.
    pub fn close_all(&self) {
        // `Err` only means nobody is connected — which is the state this asks
        // for, so it is not a failure.
        let _ = self.hangup.send(());
    }

    /// Run `hook` **once**, under the state lock, just before the next
    /// `tab.list` reply is built.
    ///
    /// The subscribe-then-list prologue's race in one seam: a mutation that
    /// commits here lands between the ack a client fenced on and the snapshot it
    /// is about to take, which is exactly the interleaving a real busy daemon
    /// produces and the one a naive client turns into a spurious gap.
    pub fn before_tab_list(&self, hook: impl FnOnce(&mut TabListHook<'_>) + Send + 'static) {
        self.lock().before_tab_list = Some(Box::new(hook));
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

/// One connection: read a line, answer it, until EOF or a hang-up — or until
/// `events.subscribe` flips it into a one-way push stream.
async fn serve<S>(
    stream: S,
    conn_id: u64,
    state: Arc<Mutex<FakeState>>,
    mut hangup: broadcast::Receiver<()>,
) where
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

        // `events.subscribe` is the one op whose reply is not the end of the
        // exchange: the ack and the subscriber registration are taken under one
        // lock, and everything after it is a push.
        if op == "events.subscribe" {
            match register_stream(&state, &params, conn_id) {
                Ok((result, frames)) => {
                    let body = json!({ "id": id, "ok": true, "result": result });
                    if write_line(&mut write, &body).await.is_err() {
                        unregister_stream(&state, conn_id);
                        return;
                    }
                    push_frames(write, lines, frames, hangup, &state, conn_id).await;
                    return;
                }
                Err(refusal) => {
                    let body = error_envelope(&id, refusal);
                    if write_line(&mut write, &body).await.is_err() {
                        return;
                    }
                    continue;
                }
            }
        }

        let body = match dispatch(&state, op, &params) {
            Ok(result) => json!({ "id": id, "ok": true, "result": result }),
            Err(refusal) => error_envelope(&id, refusal),
        };
        if write_line(&mut write, &body).await.is_err() {
            return;
        }
    }
}

/// Read the ack's fence and register the subscriber **under one lock**.
///
/// Splitting the two is the fake's most tempting bug: a mutation that commits
/// between them is delivered to nobody and skipped in the sequence, so the
/// client sees a gap the daemon never had — which would flake exactly the
/// resync tests this whole surface exists for.
///
/// The ack carries `session_id` as well as `revision` at generation 5, and it is
/// **required** on roost's side — a fake that omitted it would fail to decode in
/// every consumer rather than in one test.
fn register_stream(
    state: &Mutex<FakeState>,
    params: &Value,
    conn_id: u64,
) -> Result<(Value, broadcast::Receiver<Frame>), Refusal> {
    let mut state = state.lock().expect("the fake roost lock is not poisoned");
    if state.ui_socket {
        return Err(refuse(
            "not-implemented",
            "events.subscribe is not yet implemented",
        ));
    }
    let filter = params
        .get("tab_id_filter")
        .and_then(wire_id)
        .unwrap_or_default();
    if filter != 0 {
        // Refused rather than ignored: a filter the server does not apply is a
        // contract lie.
        return Err(refuse(
            "invalid-param",
            "tab_id_filter is not implemented; pass \"0\"",
        ));
    }
    // Under the same lock as the ack, and before it is built: this is the
    // restart that lands *between* a client's two dials, so what it must
    // produce is an ack naming an incarnation the client's `session.identify`
    // never saw. See [`FakeRoost::restart_between_dials`].
    if state.restarts_between_dials > 0 {
        state.restarts_between_dials -= 1;
        state.dial_restarts += 1;
        let moved = format!("{}-redial-{}", state.session_id, state.dial_restarts);
        state.session_id = moved;
    }
    state.streams.insert(conn_id);
    let frames = state.frames.subscribe();
    let mut ack = vector(VECTOR_EVENTS_SUBSCRIBE)["result"].clone();
    ack["revision"] = json!(state.revision);
    // The incarnation answering, echoed so a client that identified on one
    // connection and subscribed on another can refuse a mismatched pair. It
    // tracks [`FakeRoost::restart`] and [`FakeRoost::restart_between_dials`],
    // which are the only things that move it.
    ack["session_id"] = json!(state.session_id);
    Ok((ack, frames))
}

fn unregister_stream(state: &Mutex<FakeState>, conn_id: u64) {
    state
        .lock()
        .expect("the fake roost lock is not poisoned")
        .streams
        .remove(&conn_id);
}

/// The push half of a subscribed connection.
///
/// Four things end it: a hang-up, the peer going away, the peer falling behind
/// the fan-out, and a **write that will not complete**. The middle one is
/// roost's rule — **the server closes rather than thins** — so a `Lagged`
/// receiver is not resynchronized, it is dropped, and the client's bare EOF is
/// the resync signal.
///
/// The last one matters here even though a real daemon would rarely hit it: a
/// peer that has stopped reading eventually fills its socket buffer, and a
/// `write_all` with no way out would park this task forever — holding the
/// stream registered, so a test's [`FakeRoost::stop`] or
/// [`FakeRoost::close_all`] would never be honoured and the test would **hang
/// instead of fail**. Racing the write against the hang-up and a short deadline
/// keeps a wedged peer a closed connection, which is the same answer roost's
/// stall budget gives it.
async fn push_frames<W, R>(
    mut write: W,
    mut lines: tokio::io::Lines<BufReader<R>>,
    mut frames: broadcast::Receiver<Frame>,
    mut hangup: broadcast::Receiver<()>,
    state: &Mutex<FakeState>,
    conn_id: u64,
) where
    W: AsyncWrite + Unpin,
    R: AsyncRead + Unpin,
{
    loop {
        // **Biased, frames first.** `stop()` pushes its terminal envelope and
        // then hangs up, so an unbiased select would drop the goodbye about half
        // the time — and a client that never hears `session.stopping` cannot
        // tell a stop from a backpressure close. Pending frames drain, then the
        // hang-up is honoured.
        let frame = tokio::select! {
            biased;
            frame = frames.recv() => match frame {
                Ok(frame) => frame,
                // roost's rule: **the server closes rather than thins.** A
                // subscriber that fell behind is dropped, and the bare EOF is
                // its resync signal.
                Err(broadcast::error::RecvError::Lagged(_)) => break,
                Err(broadcast::error::RecvError::Closed) => break,
            },
            // Frames the peer writes after the flip are read and discarded —
            // roost still reads the connection so it notices a peer that went
            // away, and never dispatches or replies to what it reads.
            next = lines.next_line() => match next {
                Ok(Some(_)) => continue,
                _ => break,
            },
            _ = hangup.recv() => break,
        };
        let mut bytes = frame.as_bytes().to_vec();
        bytes.push(b'\n');
        let wrote = tokio::select! {
            biased;
            wrote = tokio::time::timeout(STREAM_WRITE_DEADLINE, write.write_all(&bytes)) => wrote,
            // A hang-up beats an in-flight write. `write_all` is not
            // cancel-safe — the peer may see a truncated frame — but a peer
            // being hung up on is not going to read it either, and the
            // alternative is a task nothing can stop.
            _ = hangup.recv() => break,
        };
        match wrote {
            Ok(Ok(())) => {}
            // A closed peer, or one that stopped reading long enough to fill
            // the buffer. Both are "this subscriber is gone".
            Ok(Err(_)) | Err(_) => break,
        }
    }
    unregister_stream(state, conn_id);
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
        "tab.list" => {
            if let Some(hook) = state.before_tab_list.take() {
                hook(&mut TabListHook { state: &mut state });
            }
            state.tab_list_calls += 1;
            // **`revision` only on a session socket.** A UI socket serves no
            // event stream, so it omits the key ENTIRELY — not `null` — and a
            // fence read off one would be a number with nothing to fence
            // against. Sending it here would let a client be tested green
            // against a shape the real UI socket never produces.
            if state.ui_socket {
                return Ok(json!({ "projects": state.projects }));
            }
            Ok(json!({
                "projects": state.projects,
                "revision": state.revision,
            }))
        }
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
            state.tab_open_calls.push(params.clone());
            let id = state.next_tab_id;
            state.next_tab_id += 1;
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
            let mut opened = vector(VECTOR_TAB_OPENED);
            opened["data"]["tab"] = tab.clone();
            state.commit(vec![opened]);
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
            let mut closed = vector(VECTOR_TAB_CLOSED);
            closed["data"]["tab_id"] = json!(id.to_string());
            state.commit(vec![closed]);
            Ok(json!({}))
        }
        "tab.write" => {
            // Decode, then look for the tab. Generation 4 ran roost's
            // `require_lease` between the two — deliberately, so a write to a
            // missing tab without authority never leaked that the tab was
            // missing. At 5 there is no authority to check and nothing to leak
            // it to: every same-UID client may write.
            let id = params_tab_id(params)?;
            let encoded = params
                .get("data")
                .and_then(Value::as_str)
                .ok_or_else(|| refuse("invalid-param", "tab.write needs base64 `data`"))?;
            let bytes = base64::engine::general_purpose::STANDARD
                .decode(encoded)
                .map_err(|e| refuse("invalid-param", format!("tab.write data: {e}")))?;
            if state.tab(id).is_none() {
                return Err(refuse("not-found", format!("no such tab: {id}")));
            }
            state
                .writes
                .entry(id)
                .or_default()
                .extend_from_slice(&bytes);
            Ok(json!({}))
        }
        "session.set_agent_hooks" => {
            // roost's own order at generation 6: decode (strict), validate the
            // names, handle. The `AgentHooksAuthority` check that generation 4
            // ran between the first two was deleted with the lease, so this op
            // WRITES FILES under the session user's home for whichever same-UID
            // client asked last.
            if state.ui_socket {
                return Err(refuse("unknown-op", "no such op: session.set_agent_hooks"));
            }
            // `SessionSetAgentHooksParams` is `deny_unknown_fields`, so a
            // generation-5 client's `mode`/`skip` never reaches roost's own
            // validation at all — it is a decode refusal. Spelled out rather
            // than folded into the `invalid-param` below, because "your client
            // is too old" and "your list is malformed" are different bugs.
            for retired in ["mode", "skip", "lease"] {
                if params.get(retired).is_some() {
                    return Err(refuse(
                        "unknown-field",
                        format!("session.set_agent_hooks: unknown field `{retired}`"),
                    ));
                }
            }
            // roost's `check_names`, in its order: a blank element is checked
            // before emptiness, because a blank one can only be a client bug.
            let agents = params
                .get("agents")
                .and_then(Value::as_array)
                .ok_or_else(|| {
                    refuse(
                        "invalid-param",
                        "session.set_agent_hooks needs an `agents` array",
                    )
                })?;
            if agents
                .iter()
                .any(|name| name.as_str().is_none_or(|name| name.trim().is_empty()))
            {
                return Err(refuse(
                    "invalid-param",
                    "session.set_agent_hooks: `agents` carries an empty name",
                ));
            }
            if agents.is_empty() {
                return Err(refuse(
                    "invalid-param",
                    "session.set_agent_hooks requires a non-empty `agents`: a client \
                     with nothing to raise does not send the op",
                ));
            }
            if params.get("client").and_then(Value::as_str).is_none() {
                return Err(refuse(
                    "invalid-param",
                    "session.set_agent_hooks needs a `client`",
                ));
            }
            state.agent_hooks_calls.push(params.clone());
            // `take`, not a clone: both this field's doc and the setter's say
            // "the NEXT `session.set_agent_hooks`", and a seed that is never
            // consumed answers every later call in the same fake too. That is
            // the shape that passes for the wrong reason — a test seeding the
            // partial-failure row and then asserting the vendored vector on a
            // second call would read the seed back and never notice.
            Ok(match state.agent_hooks_result.take() {
                Some(seeded) => seeded,
                None => vector(VECTOR_SET_AGENT_HOOKS)["result"].clone(),
            })
        }
        // Everything else: the fake serves inventory and the one-shots, and an
        // op shed reaches for that roost does not serve here should fail loudly
        // in a test rather than pass.
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

    use roost_ipc::client::{EventFrame, ServerCode};
    use roost_ipc::messages::SESSION_PROTOCOL_VERSION;

    use crate::roost::bootstrap::ROOST_WIRED_AGENTS;
    use crate::roost::{Conn, RoostError};

    /// The vectors are the fake's whole claim to fidelity — if one stops
    /// parsing, or loses the key the fake reads out of it, that has to fail
    /// here rather than as a confusing decode error three layers up.
    #[test]
    fn the_vendored_vectors_carry_what_the_fake_reads() {
        let state = FakeState::new(DEFAULT_FRAME_CAPACITY);
        assert_eq!(state.revision, 42, "the session tab.list vector's revision");
        assert_eq!(state.session_protocol, SESSION_PROTOCOL_VERSION);
        assert!(!state.session_id.is_empty());
        assert_eq!(state.next_tab_id, 6, "one past the vector's highest tab id");
        assert!(state.tab(5).is_some());
        assert!(vector(VECTOR_ERROR)["error"]["code"].is_string());
        assert!(vector(VECTOR_IDENTIFY)["result"]["app_label"].is_string());
        assert!(
            vector(VECTOR_SESSION_IDENTIFY)["result"]
                .get("features")
                .is_none(),
            "`features` retired at generation 5; the template must not serve it"
        );
        for key in ["wired", "refreshed", "removed", "skipped", "errors"] {
            assert!(
                vector(VECTOR_SET_AGENT_HOOKS)["result"][key].is_array(),
                "the set_agent_hooks vector carries {key}"
            );
        }
        assert!(vector(VECTOR_EVENTS_SUBSCRIBE)["result"]["revision"].is_u64());
        assert!(
            vector(VECTOR_EVENTS_SUBSCRIBE)["result"]["session_id"].is_string(),
            "the ack's `session_id` is required at generation 5, not optional"
        );
        assert_eq!(vector(VECTOR_TAB_CLOSED)["event"], json!("tab.closed"));
        assert_eq!(
            vector(VECTOR_TAB_NOTIFICATION)["event"],
            json!("tab.notification")
        );
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

    /// **Decode, then look for the tab.** Generation 4 ran `require_lease`
    /// between the two so a refusal never confirmed whether a tab existed; at 5
    /// there is no gate, so an unknown tab is `not-found` on the first try and
    /// the decode failure still comes first.
    #[tokio::test]
    async fn a_write_decodes_before_it_looks_for_the_tab() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");

        let missing = conn
            .tab_write(4242, b"x")
            .await
            .expect_err("the tab really is gone");
        assert_eq!(missing.server_code(), Some(ServerCode::NotFound));
    }

    /// `session.set_agent_hooks` is **open to every connection** at generation 5
    /// and the last writer wins.
    ///
    /// This is the one behaviour the bump actually changed about this op —
    /// roost deleted `AgentHooksAuthority` and `AgentHooksError::Unauthorized`
    /// outright — and it is the thing the deleted lease-registry test was
    /// standing in front of. Two connections, no coordination, both served.
    #[tokio::test]
    async fn two_connections_both_wire_hooks_and_the_last_one_is_recorded() {
        let fake = FakeRoost::start().await;
        let mut desktop = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut phone = Conn::unix(fake.socket_path()).await.expect("dial");

        let result = desktop
            .session_set_agent_hooks(&ROOST_WIRED_AGENTS, "shed-desktop")
            .await
            .expect("no authority to hold");
        assert_eq!(
            result.wired,
            vec!["claude".to_string(), "codex".to_string()]
        );
        phone
            .session_set_agent_hooks(&ROOST_WIRED_AGENTS, "shed-mobile")
            .await
            .expect("a second client is not refused");

        let calls = fake.agent_hooks_calls();
        assert_eq!(calls.len(), 2, "both landed");
        assert_eq!(calls[0]["agents"], json!(ROOST_WIRED_AGENTS));
        assert_eq!(calls[0]["client"], json!("shed-desktop"));
        assert_eq!(
            calls[1]["client"],
            json!("shed-mobile"),
            "`client` is the record of who wrote last"
        );
        for call in &calls {
            assert!(
                call.get("lease").is_none(),
                "no authority travels with this op any more: {call}"
            );
        }
    }

    /// **Semantic idempotence, asserted rather than argued.** Sending the op
    /// twice leaves the same wired set — which is the whole of decision D1's
    /// safety argument: an unconditional re-send on every watcher cycle is safe
    /// exactly because the second call says the same thing as the first.
    #[tokio::test]
    async fn the_hooks_op_sent_twice_yields_an_identical_wired_set() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");

        let first = conn
            .session_set_agent_hooks(&ROOST_WIRED_AGENTS, "shed-desktop")
            .await
            .expect("the first call");
        let second = conn
            .session_set_agent_hooks(&ROOST_WIRED_AGENTS, "shed-desktop")
            .await
            .expect("the second call");
        assert_eq!(first.wired, second.wired);
        assert_eq!(first.refreshed, second.refreshed);
        assert_eq!(first.removed, second.removed);

        let calls = fake.agent_hooks_calls();
        assert_eq!(calls.len(), 2);
        assert_eq!(calls[0], calls[1], "the same request, byte for byte");
    }

    /// One raw request against the fake, hand-built — the way to send a params
    /// shape no typed op will produce, and the way to read a reply envelope
    /// whole rather than through a decoder. Answers it verbatim.
    async fn raw_call(fake: &FakeRoost, op: &str, params: Value) -> Value {
        let stream = tokio::net::UnixStream::connect(fake.socket_path())
            .await
            .expect("dial");
        let (read, mut write) = tokio::io::split(stream);
        let request = json!({ "id": "1", "op": op, "params": params });
        write_line(&mut write, &request).await.expect("write");
        let line = BufReader::new(read)
            .lines()
            .next_line()
            .await
            .expect("read")
            .expect("a reply");
        serde_json::from_str(&line).expect("valid JSON")
    }

    fn refusal_code(reply: &Value) -> ServerCode {
        assert_eq!(reply["ok"], json!(false), "{reply}");
        ServerCode::from_wire(reply["error"]["code"].as_str().unwrap_or_default())
    }

    /// **The generation-6 params, validated the way roost validates them.**
    ///
    /// Every row here is a refusal a real host answers with, and every one of
    /// them is unreachable through [`Conn::session_set_agent_hooks`] — which is
    /// exactly why the fake has to spell them out. A fake that accepted
    /// anything would let a malformed raise pass in shed's tests and fail on
    /// somebody's machine.
    ///
    /// `agents` first, in roost's own order: the blank element is checked before
    /// emptiness, because a blank one can only ever be a client bug.
    #[tokio::test]
    async fn the_hooks_params_are_validated_the_way_roost_validates_them() {
        let fake = FakeRoost::start().await;

        for (why, params) in [
            (
                "a blank element",
                json!({ "agents": ["claude", "  "], "client": "shed-desktop" }),
            ),
            (
                "an empty list",
                json!({ "agents": [], "client": "shed-desktop" }),
            ),
            ("a missing list", json!({ "client": "shed-desktop" })),
            (
                "a list that is not a list",
                json!({ "agents": "claude", "client": "shed-desktop" }),
            ),
            (
                "a non-string element",
                json!({ "agents": ["claude", 7], "client": "shed-desktop" }),
            ),
            ("a missing client", json!({ "agents": ["claude"] })),
            (
                "a non-string client",
                json!({ "agents": ["claude"], "client": 7 }),
            ),
        ] {
            let reply = raw_call(&fake, "session.set_agent_hooks", params).await;
            assert_eq!(
                refusal_code(&reply),
                ServerCode::InvalidParam,
                "{why} must be invalid-param: {reply}"
            );
        }

        // And the generation-5 shape is a DECODE refusal, not a validation one:
        // roost's params are `deny_unknown_fields`, so "your client is too old"
        // and "your list is malformed" answer differently on purpose.
        for retired in ["mode", "skip", "lease"] {
            let mut params = json!({ "agents": ["claude"], "client": "shed-desktop" });
            params[retired] = json!("auto");
            let reply = raw_call(&fake, "session.set_agent_hooks", params).await;
            assert_eq!(
                refusal_code(&reply),
                ServerCode::UnknownField,
                "a retired `{retired}` must be unknown-field: {reply}"
            );
        }

        // None of the refusals were recorded as calls — a refused op wrote
        // nothing on a real host either.
        assert!(
            fake.agent_hooks_calls().is_empty(),
            "{:?}",
            fake.agent_hooks_calls()
        );

        // …and the generation-6 shape is served.
        let reply = raw_call(
            &fake,
            "session.set_agent_hooks",
            json!({ "agents": ROOST_WIRED_AGENTS, "client": "shed-desktop" }),
        )
        .await;
        assert_eq!(reply["ok"], json!(true), "{reply}");
        assert_eq!(fake.agent_hooks_calls().len(), 1);
    }

    /// `stream.ended` reaches a subscriber as [`EventFrame::Ended`], on a daemon
    /// that is **still answering**.
    ///
    /// The second half is the whole point: [`FakeRoost::end_stream`] pushes the
    /// envelope and nothing else, so a client that treated the frame as a
    /// hang-up would be reading its own EOF rather than roost's frame. The
    /// client-side consequence — a resync, not a Down — is asserted in
    /// `shed_app::roost`.
    #[tokio::test]
    async fn end_stream_pushes_the_terminal_frame_and_leaves_the_daemon_up() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe().await.expect("subscribe");

        fake.end_stream("backend-switch");
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Ended(ended)) => assert_eq!(ended.reason, "backend-switch"),
            other => panic!("expected an Ended frame, got {other:?}"),
        }

        // Still up: a fresh dial identifies, which `stop()` would have latched
        // away.
        let mut after = Conn::unix(fake.socket_path()).await.expect("re-dial");
        assert_eq!(
            after.session_identify().await.expect("identify").session_id,
            fake.session_id()
        );
    }

    /// A UI socket has no session state at all, so it takes a write and ignores
    /// the fence.
    #[tokio::test]
    async fn a_ui_socket_takes_a_write_and_publishes_no_fence() {
        let fake = FakeRoost::start().await;
        fake.serve_as_ui_socket(true);
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        conn.tab_write(5, b"hi").await.expect("write");
        conn.tab_write(5, b"!").await.expect("write");
        assert_eq!(fake.written(5), b"hi!");

        // And it publishes **no fence**: a UI socket serves no event stream, so
        // it omits `revision` from `tab.list` entirely rather than sending a
        // number nothing could be fenced against.
        assert_eq!(conn.tab_list().await.expect("list").revision, None);
    }

    #[tokio::test]
    async fn a_ui_socket_does_not_serve_an_event_stream() {
        let fake = FakeRoost::start().await;
        fake.serve_as_ui_socket(true);
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        match conn.subscribe().await {
            Err(err @ RoostError::Server { .. }) => {
                assert_eq!(err.server_code(), Some(ServerCode::NotImplemented));
            }
            Err(other) => panic!("expected not-implemented, got {other:?}"),
            Ok(_) => panic!("a UI socket pushes nothing and must not ack a subscribe"),
        }
    }

    /// `Conn::subscribe` always sends `"0"`, so the refusal is only reachable by
    /// hand — which is exactly what a client written against a roost that
    /// implements the filter would do, and what must not be silently served an
    /// unfiltered stream instead.
    #[tokio::test]
    async fn a_filtered_subscribe_is_refused_rather_than_served_unfiltered() {
        let fake = FakeRoost::start().await;
        let reply = raw_call(&fake, "events.subscribe", json!({ "tab_id_filter": "5" })).await;
        assert_eq!(refusal_code(&reply), ServerCode::InvalidParam, "{reply}");
        assert_eq!(
            fake.stream_count(),
            0,
            "a refused subscribe registers no stream"
        );
    }

    /// A commit made **after** a completed subscribe is delivered, at exactly
    /// `ack + 1`.
    ///
    /// It is worth being precise about what this does *not* pin. The dangerous
    /// interval is between reading the ack's revision and registering the
    /// subscriber; a mutation there would be counted by the ack and delivered to
    /// nobody, and the client would see a gap its daemon never had. This test
    /// cannot reach that interval — `subscribe()` has already returned before it
    /// mutates. **That atomicity is structural, not tested**: `register_stream`
    /// (`testing.rs`, the `let mut state = state.lock()` at the top of its body)
    /// reads `state.revision` for the ack and calls `state.frames.subscribe()`
    /// under one lock acquisition, and `FakeState::commit` needs that same lock
    /// to bump the revision and push. There is no interleaving to test because
    /// there is no window; a barrier here would only pin the absence of one in
    /// a way a refactor could silently satisfy.
    #[tokio::test]
    async fn a_commit_after_a_subscribe_arrives_at_the_acked_revision_plus_one() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe().await.expect("subscribe");
        let acked = stream.revision();
        assert_eq!(acked, fake.revision());
        assert_eq!(fake.stream_count(), 1);

        fake.bump_revision();
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Batch(batch)) => {
                assert_eq!(batch.revision, acked + 1);
                assert!(batch.events.is_empty(), "an empty commit is still a batch");
            }
            other => panic!("expected a batch, got {other:?}"),
        }
    }

    /// The ack names the incarnation answering, and it **tracks a restart** —
    /// which is the whole reason roost made the field required at generation 5:
    /// a client that identifies on one connection and subscribes on another has
    /// to be able to tell that the two dials landed on different processes.
    ///
    /// Read off the raw ack rather than through `Conn`, because the value is
    /// what is under test: a fake that hardcoded the vector's recorded id would
    /// decode perfectly and lie about exactly this.
    #[tokio::test]
    async fn the_subscribe_ack_echoes_the_session_id_and_follows_a_restart() {
        let fake = FakeRoost::start().await;
        let mut control = Conn::unix(fake.socket_path()).await.expect("dial");
        let identified = control.session_identify().await.expect("identify");
        assert_eq!(
            raw_subscribe_ack(&fake).await["session_id"],
            json!(identified.session_id)
        );

        fake.restart();
        let after = raw_subscribe_ack(&fake).await;
        assert_ne!(
            after["session_id"],
            json!(identified.session_id),
            "a restart is a new incarnation and the ack has to say so"
        );
        assert_eq!(after["session_id"], json!(fake.session_id()));
    }

    /// [`FakeRoost::restart_between_dials`] hands out the pair a client cannot
    /// otherwise be made to see: an identify and a subscribe on two
    /// incarnations, with the control connection still live underneath.
    ///
    /// Read through `Conn`, because the accessor a watcher compares on is the
    /// thing being wired here — an ack the fake moved but `RoostEventStream`
    /// did not surface would leave the check with nothing to read.
    #[tokio::test]
    async fn a_restart_between_the_dials_leaves_the_two_legs_disagreeing() {
        let fake = FakeRoost::start().await;
        let mut control = Conn::unix(fake.socket_path()).await.expect("dial");
        let identified = control.session_identify().await.expect("identify");

        fake.restart_between_dials(1);
        let event_leg = Conn::unix(fake.socket_path())
            .await
            .expect("dial")
            .subscribe()
            .await
            .expect("subscribe");
        assert_ne!(
            event_leg.session_id(),
            identified.session_id,
            "the ack has to name the incarnation that answered it"
        );
        assert_eq!(event_leg.session_id(), fake.session_id());
        assert!(
            control.tab_list().await.is_ok(),
            "the control leg is deliberately left alive: the mismatch is the \
             signal, not a dead connection"
        );

        // One subscribe, and the count is spent: the next pair agrees again,
        // which is what lets a test drive exactly as many mismatched cycles as
        // it asked for.
        let after = Conn::unix(fake.socket_path())
            .await
            .expect("dial")
            .subscribe()
            .await
            .expect("subscribe");
        assert_eq!(after.session_id(), fake.session_id());
    }

    /// The `result` object of one hand-written `events.subscribe`.
    async fn raw_subscribe_ack(fake: &FakeRoost) -> Value {
        let reply = raw_call(fake, "events.subscribe", json!({ "tab_id_filter": "0" })).await;
        assert_eq!(reply["ok"], json!(true), "{reply}");
        reply["result"].clone()
    }

    /// **The server closes rather than thins.** A subscriber that falls behind
    /// the fan-out is dropped, and the bare EOF is the client's resync signal.
    ///
    /// Deterministic on the current-thread test runtime: the commits below are
    /// synchronous and there is no await between them, so the push task cannot
    /// be polled until the client asks for its next frame — by which time the
    /// one-slot channel has overrun.
    #[tokio::test]
    async fn a_lagging_subscriber_is_closed_rather_than_thinned() {
        let fake = FakeRoost::start_with_frame_capacity(1).await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe().await.expect("subscribe");

        for _ in 0..8 {
            fake.bump_revision();
        }
        assert!(
            stream.next().await.expect("no error, a close").is_none(),
            "a lagged subscriber gets a bare EOF, never a thinned sequence"
        );
    }

    /// `stop()` is terminal and latching: the stream is told why, and nothing
    /// answers afterwards until a restart.
    #[tokio::test]
    async fn stop_says_why_and_then_serves_nothing() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe().await.expect("subscribe");

        fake.stop();
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Stopping(stopping)) => assert_eq!(stopping.reason, "stop"),
            other => panic!("expected the stopping envelope, got {other:?}"),
        }

        let mut redial = Conn::unix(fake.socket_path())
            .await
            .expect("the socket file is still there");
        assert!(
            redial.session_identify().await.is_err(),
            "a stopped daemon answers nothing"
        );

        fake.restart();
        let mut after = Conn::unix(fake.socket_path()).await.expect("dial");
        after.session_identify().await.expect("it serves again");
    }

    /// `skip_revision` is the only way to manufacture loss, and the client's own
    /// stream is what detects it.
    #[tokio::test]
    async fn a_skipped_revision_surfaces_as_a_gap() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe().await.expect("subscribe");
        let acked = stream.revision();

        fake.skip_revision();
        fake.bump_revision();
        match stream.next().await {
            Err(RoostError::RevisionGap { expected, got }) => {
                assert_eq!(expected, acked + 1);
                assert_eq!(got, acked + 2);
            }
            other => panic!("expected a revision gap, got {other:?}"),
        }
    }

    /// The mutations that push a batch push exactly one, built from the vendored
    /// envelope vectors.
    #[tokio::test]
    async fn every_mutation_commits_exactly_one_batch() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe().await.expect("subscribe");

        let mut driver = Conn::unix(fake.socket_path()).await.expect("dial");
        let opened = driver
            .tab_open(roost_ipc::messages::TabOpenParams {
                title: "zsh".into(),
                ..Default::default()
            })
            .await
            .expect("open");

        async fn names(stream: &mut crate::roost::RoostEventStream) -> Vec<String> {
            match stream.next().await.expect("a frame") {
                Some(EventFrame::Batch(batch)) => {
                    batch.events.iter().map(|e| e.event.clone()).collect()
                }
                other => panic!("expected a batch, got {other:?}"),
            }
        }

        assert_eq!(names(&mut stream).await, vec!["tab.opened".to_string()]);

        fake.set_tab_axes(5, "waiting", Some(ownership("claude", "s", "d", 1)), true);
        assert_eq!(
            names(&mut stream).await,
            vec![
                "agent_report.changed".to_string(),
                "tab.notification".to_string()
            ],
            "a notification flip rides the same commit"
        );

        // The sticky bit did not move this time, so there is no second envelope.
        fake.set_tab_axes(5, "working", Some(ownership("claude", "s", "d", 2)), true);
        assert_eq!(
            names(&mut stream).await,
            vec!["agent_report.changed".to_string()]
        );

        driver.tab_close(opened.id).await.expect("close");
        assert_eq!(names(&mut stream).await, vec!["tab.closed".to_string()]);

        fake.bump_revision();
        assert!(names(&mut stream).await.is_empty(), "an empty commit");
    }

    /// The hook runs under the lock, once, before the reply it races.
    #[tokio::test]
    async fn the_tab_list_hook_commits_before_the_reply_it_races() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let stream = conn.subscribe().await.expect("subscribe");
        let acked = stream.revision();

        fake.before_tab_list(|hook| hook.bump_revision());
        let mut reader = Conn::unix(fake.socket_path()).await.expect("dial");
        let listed = reader.tab_list().await.expect("list");
        assert_eq!(
            listed.revision,
            Some(acked + 1),
            "the snapshot is already past the batch the hook pushed"
        );

        // Once: the next list is not raced again.
        assert_eq!(
            reader.tab_list().await.expect("list").revision,
            Some(acked + 1)
        );
        assert_eq!(fake.tab_list_calls(), 2);
    }
}
