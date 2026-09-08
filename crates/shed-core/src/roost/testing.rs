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
//! At session protocol 4 it also speaks the parts of the lease protocol shed's
//! client can now reach:
//!
//! * **`events.subscribe` is leaseless and classifies.** The ack's `revision`
//!   and the subscriber's registration are taken under **one** state lock, so no
//!   mutation can commit between the number a client fences on and the first
//!   frame it is eligible to receive — a fake that let one slip through would
//!   manufacture the very gap the resync tests exist to distinguish from a real
//!   one.
//! * **Every mutation commits exactly one batch** at the new revision, built
//!   from the vendored envelope vectors. Empty commits are pushed too, because
//!   that is what makes a skipped revision mean loss and nothing else.
//! * **`tab.write` on a session socket requires the lease**, with roost's own
//!   takeover table and its *one* tombstone behind it.
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
    include_str!("../../../fixtures/roost-vectors/session.identify.response.v4.json");
const VECTOR_IDENTIFY: &str =
    include_str!("../../../fixtures/roost-vectors/identify.response.json");
const VECTOR_TAB_LIST: &str =
    include_str!("../../../fixtures/roost-vectors/tab.list.session.response.json");
const VECTOR_TAB_OPEN: &str =
    include_str!("../../../fixtures/roost-vectors/tab.open.response.json");
const VECTOR_ERROR: &str = include_str!("../../../fixtures/roost-vectors/response.error.json");
const VECTOR_SESSION_CONNECT: &str =
    include_str!("../../../fixtures/roost-vectors/session.connect.response.json");
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
const VECTOR_SESSION_DRIVER_CHANGED: &str =
    include_str!("../../../fixtures/roost-vectors/session.driver_changed.event.json");
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

/// How a registered event stream is classified — roost's own distinction, kept
/// so a test can assert shed subscribed the way it claims to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum StreamKind {
    /// The lease presented was the current one.
    Driver,
    /// No lease, a stale one, or one this session never minted.
    Observer,
}

/// Ask a connection holding `lease` to close — every one but `exempt`, which is
/// the connection that requested the takeover (roost spares the requester).
type LeaseClose = (String, Option<u64>);

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
    /// When true, `session.identify` omits `features` entirely — a session from
    /// before the key existed, which must still decode.
    strip_features: bool,
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
    /// The live interactive lease, and the label its holder reported.
    lease: Option<String>,
    lease_label: Option<String>,
    /// **Exactly one** tombstone, as roost keeps: the most recently displaced
    /// lease, so its holder hears `taken-over` rather than `connect-required`.
    /// A lease displaced twice is forgotten.
    tombstone: Option<String>,
    /// Seeds the deterministic lease mint.
    lease_counter: u64,
    /// Registered event streams, by connection id. A takeover reclassifies these
    /// in place; it never closes them.
    streams: BTreeMap<u64, StreamKind>,
    /// How many `tab.list` replies have been served — the assertion that the
    /// watcher reads the inventory once per cycle and not per event.
    tab_list_calls: usize,
    /// Runs once, under this lock, just before the next `tab.list` reply.
    before_tab_list: Option<TabListHookFn>,
    /// Pushed frames. In the state so a commit's revision bump and its batch
    /// leave the lock together — see the module doc.
    frames: broadcast::Sender<Frame>,
    /// Asks the connections holding a displaced lease to close.
    close_lease: broadcast::Sender<LeaseClose>,
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
        let (close_lease, _) = broadcast::channel(8);
        FakeState {
            session_protocol: identified["result"]["session_protocol"]
                .as_u64()
                .expect("the session.identify vector carries a protocol")
                as u32,
            ui_socket: false,
            strip_features: false,
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
            lease: None,
            lease_label: None,
            tombstone: None,
            lease_counter: 0,
            streams: BTreeMap::new(),
            tab_list_calls: 0,
            before_tab_list: None,
            frames,
            close_lease,
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

    /// Mint a lease: 32 lowercase hex from a counter-seeded generator, so it is
    /// shaped exactly like the wire's and is the same on every run.
    fn mint_lease(&mut self) -> String {
        self.lease_counter += 1;
        let mut hex = String::with_capacity(32);
        let mut x = self.lease_counter.wrapping_mul(0x9E37_79B9_7F4A_7C15);
        for _ in 0..2 {
            // splitmix64's finalizer: a counter run through it still looks like
            // 16 hex characters of nothing in particular.
            let mut z = x;
            z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
            z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
            z ^= z >> 31;
            hex.push_str(&format!("{z:016x}"));
            x = z;
        }
        hex
    }

    /// Displace the current lease and mint a fresh one, exactly as roost's
    /// takeover does: one tombstone, every registered stream told once, and
    /// every non-stream connection under the old lease asked to close.
    ///
    /// **Only an actual displacement announces itself.** `session.driver_changed`
    /// is what a *deposed* driver and its onlookers are told; minting into an
    /// unheld session displaces nobody, and roost sends nothing. A fake that
    /// announced it anyway would let a client be written against — and tested
    /// green on — an envelope a real daemon never emits there.
    fn take_over_lease(&mut self, label: Option<String>, exempt: Option<u64>) -> String {
        let displaced = self.lease.take();
        if let Some(old) = displaced.clone() {
            // Exactly one: a lease displaced twice is forgotten and falls back
            // to `connect-required`.
            self.tombstone = Some(old.clone());
            let _ = self.close_lease.send((old, exempt));
        }
        let minted = self.mint_lease();
        self.lease = Some(minted.clone());
        self.lease_label = label.clone();

        if displaced.is_some() {
            // A takeover reclassifies a driver stream in place; it never closes
            // one.
            for kind in self.streams.values_mut() {
                *kind = StreamKind::Observer;
            }
            let mut envelope = vector(VECTOR_SESSION_DRIVER_CHANGED);
            envelope["data"]["taken_by"] =
                json!(label.unwrap_or_else(|| "unknown client".to_string()));
            self.push(&envelope);
        }
        minted
    }

    /// How a `tab.write`'s `lease` key is judged on a **session** socket.
    fn check_write_lease(&self, presented: Option<&str>) -> Result<(), Refusal> {
        match presented {
            Some(lease) if Some(lease) == self.lease.as_deref() => Ok(()),
            Some(lease) if Some(lease) == self.tombstone.as_deref() => Err(refuse(
                "taken-over",
                "another client took the interactive lease",
            )),
            // Absent, unknown, or a lease displaced twice and forgotten.
            _ => Err(refuse(
                "connect-required",
                "tab.write on a session socket needs the interactive lease",
            )),
        }
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

    /// Registered observer streams, read from **inside** the `tab.list` lock.
    ///
    /// This is how a test proves the *ordering* of a client's prologue rather
    /// than only its arithmetic: a client that subscribed before listing has a
    /// stream registered by the time this runs, and one that listed first has
    /// none. [`FakeRoost::observer_count`] cannot answer it — that takes the
    /// same lock this hook is already holding.
    pub fn observer_count(&self) -> usize {
        self.state
            .streams
            .values()
            .filter(|k| **k == StreamKind::Observer)
            .count()
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

/// Characters roost refuses in a `client_label`, beyond [`char::is_control`].
///
/// `is_control` alone is not enough and the difference is visible: the line and
/// paragraph separators break the takeover banner onto a second line, and the
/// bidi overrides reorder everything after them — and Unicode classifies none of
/// them as control characters, so they sail straight through a
/// `filter(!is_control)` into a string the session renders. roost's own
/// `is_layout_hostile` (`roost-engine/src/ipc.rs` at the pinned rev) is this
/// exact set.
fn is_layout_hostile(c: char) -> bool {
    c.is_control()
        || matches!(c, '\u{2028}' | '\u{2029}' | '\u{202a}'..='\u{202e}' | '\u{2066}'..='\u{2069}')
}

/// roost's own `client_label` normalization: trim, drop the layout-hostile
/// characters, cap at 128 UTF-8-safe bytes, empty after that → absent.
fn normalize_label(raw: &str) -> Option<String> {
    let cleaned: String = raw
        .trim()
        .chars()
        .filter(|c| !is_layout_hostile(*c))
        .collect::<String>();
    let mut capped = cleaned.trim().to_string();
    while capped.len() > 128 {
        capped.pop();
    }
    if capped.is_empty() {
        None
    } else {
        Some(capped)
    }
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

    /// Registered streams that presented no live lease — what shed's watcher is.
    pub fn observer_count(&self) -> usize {
        self.lock()
            .streams
            .values()
            .filter(|k| **k == StreamKind::Observer)
            .count()
    }

    /// Registered streams that presented the current lease.
    pub fn driver_count(&self) -> usize {
        self.lock()
            .streams
            .values()
            .filter(|k| **k == StreamKind::Driver)
            .count()
    }

    /// Report a different `session_protocol` — the compatibility gate's input.
    pub fn set_session_protocol(&self, protocol: u32) {
        self.lock().session_protocol = protocol;
    }

    /// Omit `features` from `session.identify` entirely — a session from before
    /// the key existed, which a current client must still decode (as an empty
    /// list) rather than refuse.
    pub fn serve_without_features(&self) {
        self.lock().strip_features = true;
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

    /// An external client takes the interactive lease.
    ///
    /// Roost's takeover, whole: the old lease is tombstoned (exactly one),
    /// every non-stream connection registered under it is closed, every
    /// registered stream is reclassified to observer in place and told once with
    /// `session.driver_changed{taken_by}` — and **no stream is closed**, which is
    /// the R1 re-cut this fake exists to let shed's watcher prove it survives.
    pub fn take_over(&self, label: &str) -> String {
        self.lock().take_over_lease(normalize_label(label), None)
    }

    /// The lease currently held, if any.
    pub fn lease(&self) -> Option<String> {
        self.lock().lease.clone()
    }

    /// The label the current lease holder reported on `session.connect`.
    pub fn lease_label(&self) -> Option<String> {
        self.lock().lease_label.clone()
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
            state.lease = None;
            state.lease_label = None;
            state.tombstone = None;
            state.streams.clear();
        }
        self.stopped.store(false, Ordering::SeqCst);
        let _ = self.hangup.send(());
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
    let mut closes = state
        .lock()
        .expect("the fake roost lock is not poisoned")
        .close_lease
        .subscribe();
    let (read, mut write) = tokio::io::split(stream);
    let mut lines = BufReader::new(read).lines();
    // The lease this connection presented on `session.connect`, if any — what a
    // takeover closes a deposed holder's control connection by.
    let mut held_lease: Option<String> = None;
    loop {
        let line = tokio::select! {
            _ = hangup.recv() => return,
            closed = closes.recv() => match closed {
                Ok((lease, exempt)) => {
                    if held_lease.as_deref() == Some(lease.as_str()) && exempt != Some(conn_id) {
                        return;
                    }
                    continue;
                }
                // Lagged or closed: nothing this connection can act on.
                Err(_) => continue,
            },
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

        let body = match dispatch(&state, op, &params, conn_id, &mut held_lease) {
            Ok(result) => json!({ "id": id, "ok": true, "result": result }),
            Err(refusal) => error_envelope(&id, refusal),
        };
        if write_line(&mut write, &body).await.is_err() {
            return;
        }
    }
}

/// Read the ack's revision and register the subscriber **under one lock**.
///
/// Splitting the two is the fake's most tempting bug: a mutation that commits
/// between them is delivered to nobody and skipped in the sequence, so the
/// client sees a gap the daemon never had — which would flake exactly the
/// resync tests this whole surface exists for.
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
    let presented = params
        .get("lease")
        .and_then(Value::as_str)
        .filter(|lease| !lease.is_empty());
    // The classifier, not a gate: reading a session is not interactive
    // authority, so a subscribe is never refused for want of a lease.
    let kind = if presented.is_some() && presented == state.lease.as_deref() {
        StreamKind::Driver
    } else {
        StreamKind::Observer
    };
    state.streams.insert(conn_id, kind);
    let frames = state.frames.subscribe();
    let mut ack = vector(VECTOR_EVENTS_SUBSCRIBE)["result"].clone();
    ack["revision"] = json!(state.revision);
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

fn dispatch(
    state: &Mutex<FakeState>,
    op: &str,
    params: &Value,
    conn_id: u64,
    held_lease: &mut Option<String>,
) -> Result<Value, Refusal> {
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
            if state.strip_features {
                if let Some(object) = result.as_object_mut() {
                    object.remove("features");
                }
            }
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
            // **The order is roost's and it is load-bearing.** roost decodes the
            // params, then runs `require_lease`, then hands the bytes to the
            // supervisor — so a write to a tab that does not exist, presented
            // WITHOUT authority, answers `connect-required` and never leaks the
            // fact that the tab is missing. Decode, gate, then look for the tab.
            let id = params_tab_id(params)?;
            let encoded = params
                .get("data")
                .and_then(Value::as_str)
                .ok_or_else(|| refuse("invalid-param", "tab.write needs base64 `data`"))?;
            let bytes = base64::engine::general_purpose::STANDARD
                .decode(encoded)
                .map_err(|e| refuse("invalid-param", format!("tab.write data: {e}")))?;
            let presented = params.get("lease").and_then(Value::as_str);
            // On a UI socket the key is accepted and ignored — that socket mints
            // no leases, and refusing it would make one client unable to talk to
            // both kinds of socket.
            if !state.ui_socket {
                state.check_write_lease(presented)?;
                // **Presenting the live lease REGISTERS this connection under
                // it**, exactly as roost's `present()` does for every
                // lease-carrying op — not just for `session.connect`. A takeover
                // closes everything on that list, so a connection that only ever
                // wrote would otherwise survive one and keep writing at a
                // session it no longer drives.
                *held_lease = presented.map(str::to_string);
            }
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
        "session.connect" => {
            if state.ui_socket {
                return Err(refuse("unknown-op", "no such op: session.connect"));
            }
            let takeover = params
                .get("takeover")
                .and_then(Value::as_bool)
                .unwrap_or(false);
            let label = params
                .get("client_label")
                .and_then(Value::as_str)
                .and_then(normalize_label);
            // roost's table, whole: no holder → mint; held by ANYONE (this very
            // connection included) without `takeover` → `already-connected`;
            // with `takeover` → displace and mint.
            if state.lease.is_some() && !takeover {
                return Err(refuse(
                    "already-connected",
                    "a client already holds the interactive lease",
                ));
            }
            let minted = if state.lease.is_some() {
                state.take_over_lease(label, Some(conn_id))
            } else {
                let minted = state.mint_lease();
                state.lease = Some(minted.clone());
                state.lease_label = label;
                minted
            };
            *held_lease = Some(minted.clone());
            let mut result = vector(VECTOR_SESSION_CONNECT)["result"].clone();
            result["lease"] = json!(minted);
            result["revision"] = json!(state.revision);
            Ok(result)
        }
        // Everything else: the fake serves inventory, the lease ops and the
        // one-shots, and an op shed reaches for that roost does not serve here
        // should fail loudly in a test rather than pass.
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
        assert!(vector(VECTOR_SESSION_CONNECT)["result"]["lease"].is_string());
        assert!(vector(VECTOR_EVENTS_SUBSCRIBE)["result"]["revision"].is_u64());
        assert_eq!(
            vector(VECTOR_SESSION_DRIVER_CHANGED)["event"],
            json!("session.driver_changed")
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

    /// A minted lease has to look like the wire's — 32 lowercase hex — and be
    /// the same on every run, so a failure dump is comparable.
    #[test]
    fn a_minted_lease_is_wire_shaped_and_deterministic() {
        let mut first = FakeState::new(DEFAULT_FRAME_CAPACITY);
        let mut second = FakeState::new(DEFAULT_FRAME_CAPACITY);
        let a = first.mint_lease();
        assert_eq!(a.len(), 32, "{a}");
        assert!(
            a.chars()
                .all(|c| c.is_ascii_digit() || ('a'..='f').contains(&c)),
            "{a}"
        );
        assert_eq!(a, second.mint_lease(), "same counter, same token");
        assert_ne!(a, first.mint_lease(), "and it moves");
    }

    #[test]
    fn a_client_label_is_normalized_the_way_roost_normalizes_it() {
        assert_eq!(normalize_label("  workbox  "), Some("workbox".to_string()));
        assert_eq!(normalize_label("work\u{7}box"), Some("workbox".to_string()));
        assert_eq!(normalize_label("   "), None);
        assert_eq!(
            normalize_label(&"x".repeat(200)).map(|l| l.len()),
            Some(128)
        );

        // **The half `is_control` does not cover.** Unicode calls none of these
        // control characters, so a `filter(!is_control)` passes them through
        // into the takeover banner — the separators break it onto a second line
        // and the overrides reorder everything after them. A test that only fed
        // ASCII would certify a parity with roost that does not exist.
        assert_eq!(
            normalize_label("work\u{202e}box"),
            Some("workbox".to_string()),
            "a right-to-left override"
        );
        assert_eq!(
            normalize_label("work\u{2066}box\u{2069}"),
            Some("workbox".to_string()),
            "the isolate family"
        );
        assert_eq!(
            normalize_label("work\u{2028}box"),
            Some("workbox".to_string()),
            "a line separator"
        );
        assert_eq!(
            normalize_label("work\u{2029}box"),
            Some("workbox".to_string()),
            "a paragraph separator"
        );
        assert_eq!(normalize_label("\u{202e}\u{2028}"), None, "hostile-only");
    }

    /// **A lease-bearing write registers its connection under the lease**, not
    /// just `session.connect` does.
    ///
    /// roost's `present()` is what every lease-carrying op runs, and on the live
    /// lease it pushes the connection onto the holder's list — which is the list
    /// a takeover closes. A fake that registered only the connecting one would
    /// let a second connection keep writing straight through a takeover, which
    /// is precisely the authority the lease exists to move.
    #[tokio::test]
    async fn a_write_with_the_live_lease_registers_that_connection_too() {
        let fake = FakeRoost::start().await;
        let mut owner = Conn::unix(fake.socket_path()).await.expect("dial");
        let lease = owner
            .session_connect(false, Some("owner"))
            .await
            .expect("mints")
            .lease;

        // A SECOND connection that never connected, only wrote.
        let mut writer = Conn::unix(fake.socket_path()).await.expect("dial");
        writer
            .tab_write(5, b"ok", Some(&lease))
            .await
            .expect("the live lease authorizes it");

        // A stream, to prove the takeover's reach stops at control connections.
        let observer = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = observer.subscribe("").await.expect("subscribe");

        fake.take_over("usurper");

        assert!(
            owner
                .tab_list()
                .await
                .expect_err("the holder is closed")
                .is_unavailable(),
            "the connecting holder must be closed"
        );
        assert!(
            writer
                .tab_list()
                .await
                .expect_err("the writer is closed too")
                .is_unavailable(),
            "a connection registered by its WRITE must be closed by a takeover"
        );

        // The stream survived, and still delivers.
        match stream.next().await.expect("a frame") {
            Some(EventFrame::DriverChanged(changed)) => assert_eq!(changed.taken_by, "usurper"),
            other => panic!("expected driver_changed, got {other:?}"),
        }
        fake.bump_revision();
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Batch(_)) => {}
            other => panic!("expected the stream to keep delivering, got {other:?}"),
        }
    }

    /// **The lease is checked before the tab.** roost decodes, runs
    /// `require_lease`, and only then writes — so an unknown tab presented
    /// without authority answers `connect-required` and never confirms whether
    /// that tab exists.
    #[tokio::test]
    async fn the_write_gate_runs_before_the_tab_lookup() {
        let fake = FakeRoost::start().await;
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");

        let blind = conn
            .tab_write(4242, b"x", None)
            .await
            .expect_err("no lease, no answer about the tab");
        assert_eq!(blind.server_code(), Some(ServerCode::ConnectRequired));

        let lease = conn
            .session_connect(false, None)
            .await
            .expect("mints")
            .lease;
        let missing = conn
            .tab_write(4242, b"x", Some(&lease))
            .await
            .expect_err("authorized, and the tab really is gone");
        assert_eq!(missing.server_code(), Some(ServerCode::NotFound));
    }

    /// The takeover table, and the **one** tombstone behind it.
    #[tokio::test]
    async fn the_lease_follows_roosts_takeover_table_with_exactly_one_tombstone() {
        let fake = FakeRoost::start().await;
        let mut first = Conn::unix(fake.socket_path()).await.expect("dial");
        let one = first
            .session_connect(false, Some("first"))
            .await
            .expect("no holder: mints");
        assert_eq!(fake.lease().as_deref(), Some(one.lease.as_str()));
        assert_eq!(fake.lease_label().as_deref(), Some("first"));

        // Held — by this very connection — and no takeover: refused.
        let refused = first
            .session_connect(false, None)
            .await
            .expect_err("already connected");
        assert_eq!(refused.server_code(), Some(ServerCode::AlreadyConnected));

        // A second client takes it. The first lease is now the tombstone.
        let two = fake.take_over("second");
        assert_ne!(two, one.lease);
        assert_eq!(fake.lease_label().as_deref(), Some("second"));
        let mut writer = Conn::unix(fake.socket_path()).await.expect("dial");
        let displaced = writer
            .tab_write(5, b"x", Some(&one.lease))
            .await
            .expect_err("the first lease was displaced");
        assert_eq!(displaced.server_code(), Some(ServerCode::TakenOver));

        // A third takeover forgets the first: exactly one tombstone, so its
        // holder falls back to `connect-required` — it has already been told.
        fake.take_over("third");
        let forgotten = writer
            .tab_write(5, b"x", Some(&one.lease))
            .await
            .expect_err("a lease displaced twice is forgotten");
        assert_eq!(forgotten.server_code(), Some(ServerCode::ConnectRequired));
    }

    /// A takeover closes the deposed holder's **control** connection — and
    /// spares the connection that asked for it.
    #[tokio::test]
    async fn a_takeover_closes_the_deposed_holders_control_connection() {
        let fake = FakeRoost::start().await;
        let mut deposed = Conn::unix(fake.socket_path()).await.expect("dial");
        deposed
            .session_connect(false, Some("deposed"))
            .await
            .expect("mints");

        fake.take_over("usurper");
        let dead = deposed
            .tab_list()
            .await
            .expect_err("the deposed holder's connection is closed");
        assert!(dead.is_unavailable(), "{dead}");

        // The requester's own connection survives its own takeover.
        let mut same = Conn::unix(fake.socket_path()).await.expect("dial");
        same.session_connect(false, None)
            .await
            .expect_err("a lease is live");
        let mine = same
            .session_connect(true, Some("mine"))
            .await
            .expect("takeover mints");
        same.tab_write(5, b"ok", Some(&mine.lease))
            .await
            .expect("the requester keeps its connection");
    }

    /// A UI socket mints no leases, so it accepts the key and ignores it.
    #[tokio::test]
    async fn a_ui_socket_accepts_and_ignores_the_write_lease() {
        let fake = FakeRoost::start().await;
        fake.serve_as_ui_socket(true);
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        conn.tab_write(5, b"hi", None).await.expect("no lease");
        conn.tab_write(5, b"!", Some("nonsense"))
            .await
            .expect("an unknown lease is ignored too");
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
        match conn.subscribe("").await {
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
        use tokio::io::AsyncBufReadExt as _;

        let fake = FakeRoost::start().await;
        let stream = tokio::net::UnixStream::connect(fake.socket_path())
            .await
            .expect("dial");
        let (read, mut write) = tokio::io::split(stream);
        let request = json!({
            "id": "1",
            "op": "events.subscribe",
            "params": { "lease": "", "tab_id_filter": "5" },
        });
        write_line(&mut write, &request).await.expect("write");
        let line = BufReader::new(read)
            .lines()
            .next_line()
            .await
            .expect("read")
            .expect("a reply");
        let reply: Value = serde_json::from_str(&line).expect("valid JSON");
        assert_eq!(reply["ok"], json!(false), "{line}");
        assert_eq!(
            ServerCode::from_wire(reply["error"]["code"].as_str().unwrap_or_default()),
            ServerCode::InvalidParam,
            "{line}"
        );
        assert_eq!(
            fake.observer_count(),
            0,
            "a refused subscribe registers no stream"
        );
    }

    /// A commit made **after** a completed subscribe is delivered, at exactly
    /// `ack + 1`, and the stream it arrives on is classified as an observer.
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
        let mut stream = conn.subscribe("").await.expect("subscribe");
        let acked = stream.revision();
        assert_eq!(acked, fake.revision());
        assert_eq!(fake.observer_count(), 1);
        assert_eq!(fake.driver_count(), 0, "an empty lease is an observer");

        fake.bump_revision();
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Batch(batch)) => {
                assert_eq!(batch.revision, acked + 1);
                assert!(batch.events.is_empty(), "an empty commit is still a batch");
            }
            other => panic!("expected a batch, got {other:?}"),
        }
    }

    /// A lease-bearing subscribe is the **driver** stream — the classification
    /// shed never asks for, pinned so "shed is an observer" means something.
    #[tokio::test]
    async fn a_current_lease_subscribes_as_a_driver() {
        let fake = FakeRoost::start().await;
        let mut control = Conn::unix(fake.socket_path()).await.expect("dial");
        let lease = control
            .session_connect(false, Some("driver"))
            .await
            .expect("mints")
            .lease;

        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let _stream = conn.subscribe(&lease).await.expect("subscribe");
        assert_eq!(fake.driver_count(), 1);
        assert_eq!(fake.observer_count(), 0);

        // A takeover reclassifies it in place rather than closing it.
        fake.take_over("someone else");
        assert_eq!(fake.driver_count(), 0);
        assert_eq!(fake.observer_count(), 1);
    }

    /// Every registered stream hears a **real** takeover once, and keeps
    /// delivering afterwards.
    ///
    /// A lease is minted first, deliberately: `session.driver_changed` is what a
    /// *deposed* driver's onlookers are told, so a session nobody held is not
    /// the scenario — announcing there would codify an envelope a real daemon
    /// never sends. The first half of this test is that negative control.
    #[tokio::test]
    async fn a_takeover_tells_every_stream_and_closes_none() {
        let fake = FakeRoost::start().await;
        let conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let mut stream = conn.subscribe("").await.expect("subscribe");

        // The negative control, on the same code path: claiming an UNHELD
        // session displaces nobody, so nothing is announced. The empty batch
        // after it is what proves the stream was live and simply had nothing
        // to deliver.
        fake.take_over("first");
        fake.bump_revision();
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Batch(_)) => {}
            other => panic!("a first claim deposes nobody and announces nothing, got {other:?}"),
        }

        // Now there IS a holder to depose.
        fake.take_over("workbox");
        match stream.next().await.expect("a frame") {
            Some(EventFrame::DriverChanged(changed)) => assert_eq!(changed.taken_by, "workbox"),
            other => panic!("expected driver_changed, got {other:?}"),
        }

        // Not terminal: the next commit still arrives.
        fake.bump_revision();
        match stream.next().await.expect("a frame") {
            Some(EventFrame::Batch(_)) => {}
            other => panic!("expected the stream to keep delivering, got {other:?}"),
        }
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
        let mut stream = conn.subscribe("").await.expect("subscribe");

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
        let mut stream = conn.subscribe("").await.expect("subscribe");

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
        let mut stream = conn.subscribe("").await.expect("subscribe");
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
        let mut stream = conn.subscribe("").await.expect("subscribe");

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
        let stream = conn.subscribe("").await.expect("subscribe");
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
