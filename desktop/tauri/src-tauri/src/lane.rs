//! **Agent lanes in the desktop app** — the client half of
//! [`shed_core::lane`] (plan 015 §3.4).
//!
//! A machine row whose roost tab reported an opencode server carries an
//! `agent_lane` stamp ([`crate::machines::machine_row`]). This module is what
//! that stamp makes possible: open a live transcript for one agent session, send
//! it a prompt, cancel its turn, and answer the permissions and questions it is
//! blocked on — over [`shed_opencode`], which implements
//! [`shed_core::lane::AgentLane`] against the session's OWN HTTP server.
//!
//! ```text
//! machine row  --agent_lane{session_id, server_url}-->  Lanes::open
//!                                                          |
//!            ReachKind::Local   -> dial server_url          |
//!            ReachKind::Ssh(e)  -> SshForward::reserve_for  |
//!                                                          v
//!                                              OpencodeClient (AgentLane)
//!                                                          |
//!                                       subscribe() -> Reset … Ready … frames
//!                                                          |
//!                                     LaneView (staged, then swapped) + `lane-event`
//! ```
//!
//! # Two transports, one seam
//!
//! `server_url` is a LOOPBACK url on the machine that reported it (roost's
//! plugin validates that), so reaching it depends on where that machine is:
//!
//! * [`ReachKind::Local`] — this host, or a harness socket map: its loopback is
//!   ours, so the URL is dialed as it stands.
//! * [`ReachKind::Ssh`] — an `ssh -N -L <local>:127.0.0.1:<reported>` child via
//!   [`SshForward::reserve_for`], and the client is pointed at
//!   `http://127.0.0.1:<local>`.
//!
//! Forwards are keyed by `(machine, remote_port)` and SHARED: two sessions on
//! one agent server ride one tunnel. One `ssh -N` per session-port per machine
//! is the accepted cost (§3.4) — a session on a second server gets a second
//! tunnel, because it is a second port.
//!
//! # Ownership, and why `open` is transactional
//!
//! [`Lanes::open`] has awaits in the middle of it — the forward's readiness
//! poll, and the roster GET — and both `close` and [`Lanes::reconcile`] can run
//! during them. So the bookkeeping is written as a transaction rather than as a
//! sequence of insertions:
//!
//! * A tunnel's users are COUNTED, and an open in flight counts
//!   ([`ForwardShare`]). Nothing walks the entry map to decide whether a tunnel
//!   is still wanted, so a reconcile in the middle of an open cannot conclude
//!   that a tunnel nobody has committed to yet is garbage — and a second open
//!   for the same server joins the tunnel the first one is still building
//!   instead of spawning a second `ssh` child onto the same far-side port.
//! * The share is RAII. Every `?` between reserving it and committing the entry
//!   gives it back, and the last one out drops the tunnel — so a failed open
//!   (an `Unauthorized` roster GET on a password-protected agent, a session that
//!   went away) leaves no `ssh -N` child owned by nothing.
//! * An open DECLARES itself ([`Pending`]) before its first await, and `close`
//!   and `reconcile` mark that declaration cancelled instead of finding no entry
//!   and doing nothing. The commit re-checks it under the same lock that inserts
//!   the entry, so an open whose panel closed underneath it rolls everything
//!   back rather than resurrecting a lane the user is no longer looking at.
//!
//! And because the last `Arc` on a tunnel can be held by anyone — the map, a
//! share, or a cancelled pump future that has not finished being dropped —
//! [`OwnedForward`] makes the question of WHERE the `ssh` child is reaped moot:
//! its `Drop` hands the forward to a plain OS thread, so the blocking
//! `kill`/`waitpid` in `SshForward::drop` can never run on an async worker.
//!
//! # Eviction, and why it hangs off the roost SNAPSHOT
//!
//! An entry is dropped when the user closes the panel (`lane.close`), when the
//! row's `server_url` changes (a restarted tab picks a new ephemeral port, so
//! the old entry addresses a socket that is gone), and when the tab disappears
//! from the roost snapshot. That last one is [`Lanes::reconcile`], driven by
//! [`crate::machines::OnLanes`] — and it matters most for the case nothing else
//! covers: a tab whose process **died before any adapter claimed it**. roost now
//! publishes a snapshot for a known tab that stopped existing even when nothing
//! visible changed (plan 014's ghost-row fix); without that signal such an entry
//! would hold a subscription, and an `ssh -N` child, behind a row nobody can see.
//!
//! Dropping the last entry on a forward drops the forward, whose `Drop` kills
//! and reaps the ssh child.
//!
//! # The staged view — why `lane.messages` never flickers
//!
//! The contract brackets every (re)connect with `Reset` … `Ready`, and requires
//! a client to STAGE what arrives between them and swap atomically. That is done
//! HERE rather than in the frontend, so `lane.messages` (which the harness reads,
//! and which is the same truth the panel renders) can never answer with a half
//! seeded transcript: a `Reset` opens a staging buffer, frames land in it, and
//! `Ready` swaps it in. `Down` marks the entry stale-with-a-reason and keeps the
//! last good view on screen — the "consume" posture, not an error dialog.
//!
//! `generation` is this module's own counter, and it is **the generation of the
//! rows being handed back**, not of the connect in flight: it is stamped on each
//! staging buffer at `Reset` and reaches a reader only when that buffer swaps in
//! at `Ready`. So a client that discards frames older than what it holds is
//! comparing against what is on its screen, and "the number moved" means "a
//! reseed completed", not "one started".
//!
//! It is not the adapter's number either: a subscription that has to be rebuilt
//! (a forward that went away and came back) starts a FRESH watcher whose
//! generation restarts at 1, and a number that went backwards would tell a
//! client to discard current frames as stale.
//!
//! # Credentials
//!
//! There are none in this cut. A password-protected opencode answers 401, which
//! surfaces as [`LaneError::Unauthorized`] and a status-only panel.
//! [`shed_opencode::BasicAuth`] exists for the follow-up that adds a config
//! field; nothing here can supply one, and no test claims otherwise.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::Duration;

use serde_json::{json, Value};
use tauri::{AppHandle, Emitter};

use shed_app::machine::{MachineForward, SshForward};
use shed_core::config::MachineEntry;
use shed_core::lane::{
    AgentLane, LaneAnswer, LaneApproval, LaneCapabilities, LaneDecision, LaneError, LaneEvent,
    LaneSession, SendMode,
};
use shed_core::rc::{RcActivity, RcFeedMessage};
use shed_opencode::OpencodeClient;

use crate::machines::{Machines, ReachKind};

/// The Tauri event every lane frame reaches the UI on:
/// `{machine, session_id, event}`, `event` being a serialized
/// [`LaneEvent`].
///
/// Kebab-case like every other event this app emits (`refresh`,
/// `show-launch`, `prefs-changed`).
pub const LANE_EVENT: &str = "lane-event";

/// How many transcript rows one lane keeps for the CURRENT generation.
///
/// The same 500 the adapter's own ring holds ([`shed_opencode::MessageRing`]'s
/// `MAX_RING_MESSAGES`), so a view that has replayed a whole generation holds
/// exactly what the adapter would hand back and no more.
const MAX_VIEW_MESSAGES: usize = 500;

/// The re-subscribe backoff, floor and ceiling.
///
/// Local to this module and deliberately short: the thing being retried is a
/// loopback dial (or an `ssh -N` respawn), not a WAN round trip, and a panel
/// the user is looking at should come back quickly. Reset once a generation
/// reaches its `Ready`.
const RESUBSCRIBE_BASE: Duration = Duration::from_millis(200);
/// See [`RESUBSCRIBE_BASE`].
const RESUBSCRIBE_MAX: Duration = Duration::from_secs(5);

/// The [`LaneEvent::Down`] reason that means "stop trying".
///
/// The adapter emits it when a reseed answers 404: the session was deleted, and
/// no amount of reconnecting brings it back. Every other `Down` is worth another
/// attempt (the agent restarted, the tunnel blipped).
const DOWN_UNKNOWN_SESSION: &str = "unknown_session";

/// `(machine, agent session id)` — one open lane.
type Key = (String, String);

/// `(machine, remote port)` — one shared `ssh -N -L` tunnel.
type ForwardKey = (String, u16);

/// Take a lock, ignoring poisoning — [`crate::machines`]'s rule, for the same
/// reason: every mutex here guards plain data, and turning one unrelated panic
/// into a permanently dead lane layer is strictly worse than reading a slightly
/// stale view.
fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// What a `lane.*` op can fail with.
///
/// [`LaneError`]'s variants map straight onto the IPC error envelope with the
/// contract's own snake_case codes, plus the one failure the contract has no
/// variant for because it is not the adapter's: this row has no lane.
#[derive(Debug)]
pub enum LaneFailure {
    /// There is no lane here — either the row carries no `agent_lane`, or no
    /// lane is currently open for it. Both are "there is no transcript to show",
    /// which is what a caller does something different about; the message says
    /// which.
    NoLane(String),
    /// The adapter (or the transport under it) said no.
    Lane(LaneError),
}

impl LaneFailure {
    /// A refusal from [`parse_answer`] / [`parse_mode`], as the failure BOTH IPC
    /// doors carry it.
    ///
    /// It exists so the two doors cannot drift: the socket answers with the
    /// envelope's `code` field and the `#[tauri::command]` twin answers with
    /// `code + ": " + message` in a bare string, and both get the code from
    /// here. A parse refusal that crossed as a bare message lost `bad_request`
    /// on the command door and left `bridge.ts` to guess a code out of the
    /// message's own punctuation.
    pub fn bad_request(message: String) -> LaneFailure {
        LaneFailure::Lane(LaneError::BadRequest(message))
    }

    /// The IPC envelope's `error.code`.
    pub fn code(&self) -> &'static str {
        match self {
            LaneFailure::NoLane(_) => "no_lane",
            LaneFailure::Lane(e) => match e {
                LaneError::Unauthorized => "unauthorized",
                LaneError::BadRequest(_) => "bad_request",
                LaneError::UnknownSession => "unknown_session",
                LaneError::UnknownApproval => "unknown_approval",
                LaneError::AlreadySubmitted => "already_submitted",
                LaneError::AlreadyResolved => "already_resolved",
                LaneError::NotAccepting => "not_accepting",
                LaneError::Unavailable(_) => "unavailable",
                LaneError::Failed(_) => "failed",
            },
        }
    }

    /// The IPC envelope's `error.message`.
    pub fn message(&self) -> String {
        match self {
            LaneFailure::NoLane(m) => m.clone(),
            LaneFailure::Lane(e) => e.to_string(),
        }
    }
}

impl From<LaneError> for LaneFailure {
    fn from(e: LaneError) -> Self {
        LaneFailure::Lane(e)
    }
}

// ---------------------------------------------------------------------------
// the staged view
// ---------------------------------------------------------------------------

/// One generation's accumulated truth: the transcript, the approvals, the row.
#[derive(Default)]
struct Snapshot {
    /// Which generation these rows belong to. It rides on the SNAPSHOT rather
    /// than on the view so that what `lane.messages` reports is the generation
    /// of the rows it is handing back — a client discards frames stamped older
    /// than what it HOLDS, and a number that moved at `Reset` would tell it to
    /// discard the very generation still on its screen.
    generation: u64,
    messages: VecDeque<RcFeedMessage>,
    /// Id-keyed and LAST-WRITE-WINS, exactly as the contract requires: an
    /// `Approval` frame may arrive `resolved` without its `pending` predecessor
    /// ever having been seen.
    approvals: BTreeMap<String, LaneApproval>,
    session: Option<LaneSession>,
}

impl Snapshot {
    fn push(&mut self, m: RcFeedMessage) {
        self.messages.push_back(m);
        while self.messages.len() > MAX_VIEW_MESSAGES {
            self.messages.pop_front();
        }
    }
}

/// What `lane.messages` / `lane.approvals` answer from.
#[derive(Default)]
struct LaneView {
    /// Generations STARTED — bumped on every [`LaneEvent::Reset`], and stamped
    /// onto the snapshot that connect is seeding. See the module doc for why the
    /// counter is ours and not the adapter's.
    ///
    /// Not what a reader is told: that is `live.generation`, which only moves
    /// when a seed completes.
    started: u64,
    /// Set by [`LaneEvent::Down`], cleared by the next `Ready`.
    stale: Option<String>,
    /// What a reader sees.
    live: Snapshot,
    /// Where frames go between `Reset` and `Ready`. `None` in steady state.
    staged: Option<Snapshot>,
}

impl LaneView {
    /// The buffer the next frame belongs in: the staging one mid-seed, the live
    /// one otherwise.
    fn target(&mut self) -> &mut Snapshot {
        match self.staged.as_mut() {
            Some(staged) => staged,
            None => &mut self.live,
        }
    }

    fn apply(&mut self, event: &LaneEvent) {
        match event {
            LaneEvent::Reset { .. } => {
                self.started += 1;
                // The live view is deliberately UNTOUCHED: it keeps rendering
                // the last complete generation — and reporting ITS generation
                // number — until this one is whole.
                self.staged = Some(Snapshot {
                    generation: self.started,
                    ..Snapshot::default()
                });
            }
            LaneEvent::Message { message, .. } => self.target().push(message.clone()),
            LaneEvent::Session { session } => self.target().session = Some(session.clone()),
            LaneEvent::Approval { approval } => {
                let target = self.target();
                // Last-write-wins, then DROP what is no longer waiting on the
                // human. A generation can run for days, and every ask that ever
                // resolved inside it used to stay in this map with its whole
                // payload and `request_json` — nothing reads a non-pending entry
                // (`approvals()` filters to `is_pending`), so keeping one buys
                // nothing and costs the transcript of every tool call the agent
                // ever asked about.
                //
                // Written as insert-then-drop rather than "only insert pending"
                // because the two differ on the case that matters: a `resolved`
                // for an id this view holds as `pending` must REPLACE it, not be
                // ignored. A later `pending` for the same id re-inserts it — an
                // id the agent re-opens is a new ask, and this is the same
                // last-write-wins rule it always was.
                target
                    .approvals
                    .insert(approval.id.clone(), approval.clone());
                if !approval.status.is_pending() {
                    target.approvals.remove(&approval.id);
                }
            }
            LaneEvent::Ready { .. } => {
                if let Some(staged) = self.staged.take() {
                    self.live = staged;
                }
                self.stale = None;
            }
            LaneEvent::Down { reason } => self.stale = Some(reason.clone()),
            // A frame this build cannot name. The contract is explicit: ignore
            // it — not an error, not a gap, not a reason to resubscribe.
            LaneEvent::Unknown => {}
        }
    }

    /// `lane.messages`' payload.
    fn messages(&self) -> Value {
        json!({
            "messages": self.live.messages.iter().collect::<Vec<_>>(),
            "activity": self
                .live
                .session
                .as_ref()
                .map(|s| s.activity)
                .unwrap_or(RcActivity::Unknown),
            "generation": self.live.generation,
            "stale": self.stale,
        })
    }

    /// `lane.approvals`' payload: the ones still waiting on the human.
    ///
    /// `is_pending()` rather than `!= Resolved` — the contract's rule. An
    /// unrecognized status is at least as likely to be terminal (`cancelled`,
    /// `expired`) as live, and offering answer buttons for it would post an
    /// answer the agent stopped listening for.
    fn approvals(&self) -> Value {
        let mut open: Vec<&LaneApproval> = self
            .live
            .approvals
            .values()
            .filter(|a| a.status.is_pending())
            .collect();
        // Oldest first, id as the tiebreak so the order is total and stable
        // across reads (a BTreeMap already orders by id; this puts the ones the
        // agent has been waiting on longest at the top).
        open.sort_by(|a, b| {
            a.created_at_unix_ms
                .cmp(&b.created_at_unix_ms)
                .then_with(|| a.id.cmp(&b.id))
        });
        json!({ "approvals": open })
    }
}

// ---------------------------------------------------------------------------
// the transport, and who owns it
// ---------------------------------------------------------------------------

/// What the lane layer needs from the machine layer, and the one thing it needs
/// the machine layer to BUILD.
///
/// [`Machines`] is the production implementation and the only one that ships.
/// It is a trait because everything this module has actually had bugs in — the
/// races between `open`, `close` and `reconcile`, and who owns a tunnel while an
/// open is still in flight — is unreachable in a test that has to stand up a
/// roost snapshot and spawn a real `ssh -N` child first.
pub trait LaneMachines: Send + Sync {
    /// Agent session id → `server_url`, for every lane this machine exposes.
    fn agent_lanes(&self, machine: &str) -> BTreeMap<String, String>;

    /// How this machine is reached — the lane's transport choice.
    fn reach_kind(&self, machine: &str) -> Result<ReachKind, String>;

    /// RESERVE (do not start) a tunnel to `remote_port` on `entry`'s machine.
    /// [`MachineForward::ensure`] is what starts it.
    fn forward(
        &self,
        entry: &MachineEntry,
        remote_port: u16,
    ) -> Result<Box<dyn MachineForward>, String> {
        SshForward::reserve_for(entry.clone(), remote_port)
            .map(|f| Box::new(f) as Box<dyn MachineForward>)
            .map_err(|e| e.to_string())
    }
}

impl LaneMachines for Machines {
    fn agent_lanes(&self, machine: &str) -> BTreeMap<String, String> {
        Machines::agent_lanes(self, machine)
    }

    fn reach_kind(&self, machine: &str) -> Result<ReachKind, String> {
        Machines::reach_kind(self, machine)
    }
}

/// A tunnel, wrapped so that the blocking teardown in its `Drop` is guaranteed
/// off the async runtime.
///
/// `SshForward::drop` KILLS AND REAPS an `ssh -N` child — a `waitpid`, on
/// whichever thread happens to release the last reference. And which one that is
/// is genuinely unpredictable: the map holds one, every [`ForwardShare`] holds
/// one, and a pump that has been `abort`ed still holds one until its future is
/// dropped, which happens on an async worker at a time nothing here controls.
/// Handing ONE of those references to a blocking task therefore does not decide
/// where final destruction happens.
///
/// Wrapping makes the question moot. The last `Arc<OwnedForward>` runs THIS
/// `Drop`, which is cheap and safe on any thread, and it moves the forward
/// itself onto a plain OS thread to die there. A thread rather than
/// `spawn_blocking` because this runs from `Drop` — including at shutdown, where
/// a `Handle::spawn_blocking` onto a finished runtime panics — and a lane
/// teardown is rare enough that one short-lived thread costs nothing.
struct OwnedForward {
    /// `Some` for the whole life of the value; taken only by `Drop`.
    forward: Option<Box<dyn MachineForward>>,
}

impl OwnedForward {
    fn new(forward: Box<dyn MachineForward>) -> OwnedForward {
        OwnedForward {
            forward: Some(forward),
        }
    }

    fn get(&self) -> &dyn MachineForward {
        self.forward
            .as_deref()
            .expect("a forward is only taken by Drop")
    }

    fn port(&self) -> u16 {
        self.get().port()
    }

    /// [`MachineForward::ensure`], with the error flattened to the string every
    /// caller here turns it into anyway.
    async fn ensure(&self) -> Result<(), String> {
        self.get().ensure().await.map_err(|e| e.to_string())
    }
}

impl Drop for OwnedForward {
    fn drop(&mut self) {
        if let Some(forward) = self.forward.take() {
            std::thread::spawn(move || drop(forward));
        }
    }
}

/// One shared tunnel and its user count.
struct ForwardSlot {
    forward: Arc<OwnedForward>,
    /// How many users still need this tunnel — committed entries AND opens in
    /// flight. The tunnel is dropped when it reaches zero.
    ///
    /// Counted rather than derived from the entry map, because an open that has
    /// reserved a tunnel and is still awaiting its roster GET has no entry to be
    /// derived from, and a reconcile that ran in that window used to conclude
    /// the tunnel was garbage (and a concurrent open for a second session on the
    /// same server then built a SECOND `ssh` child onto the same far-side port).
    users: usize,
}

/// One user's share of a tunnel — RAII, because `open` has awaits after the
/// tunnel exists and every early return has to give the share back.
struct ForwardShare {
    inner: Arc<Mutex<Inner>>,
    key: ForwardKey,
    forward: Arc<OwnedForward>,
}

impl ForwardShare {
    fn port(&self) -> u16 {
        self.forward.port()
    }

    /// The handle the pump re-`ensure`s through. WEAK on purpose: the pump must
    /// not keep an `ssh` child alive past the eviction that released the last
    /// share, and a failed upgrade is how it learns its lane is gone.
    fn weak(&self) -> Weak<OwnedForward> {
        Arc::downgrade(&self.forward)
    }
}

impl Drop for ForwardShare {
    fn drop(&mut self) {
        let mut inner = lock(&self.inner);
        let Some(slot) = inner.forwards.get_mut(&self.key) else {
            return;
        };
        slot.users = slot.users.saturating_sub(1);
        if slot.users == 0 {
            inner.forwards.remove(&self.key);
        }
    }
}

/// An `open` between its first await and its commit.
///
/// It exists so `close` and `reconcile` have something to say no TO. Both used
/// to look for an entry, find none (it has not been inserted yet), and do
/// nothing — after which the open committed anyway, leaving a live subscription
/// (and possibly an `ssh` child) behind a panel the user had already closed.
struct Pending {
    /// The `server_url` this open is building against. `reconcile` compares it
    /// to the fresh snapshot by exactly the same rule it judges a committed
    /// entry by: a row that now reports a different port makes this open
    /// obsolete before it ever commits.
    server_url: String,
    /// Set by `close`/`reconcile` when the key stopped being wanted. Read under
    /// the same lock acquisition that inserts the entry, so the decision cannot
    /// be raced.
    cancelled: bool,
}

/// Removes the [`Pending`] declaration on every exit path, committed or not.
struct PendingGuard {
    inner: Arc<Mutex<Inner>>,
    key: Key,
}

impl Drop for PendingGuard {
    fn drop(&mut self) {
        lock(&self.inner).pending.remove(&self.key);
    }
}

/// The per-key open gate ([`Lanes::gates`]), held for as long as one `open`
/// needs it and REMOVED from the map when the last holder lets go.
///
/// The map is keyed by two caller-supplied strings that arrive over IPC, so
/// "created on first use, never removed" is a leak anyone who can call
/// `lane.open` can drive: junk keys, or the real ones of a machine whose tabs
/// churn, retain an entry for the life of the process. It is RAII instead, and
/// the removal rule is exactly "nobody else wants this gate" — the map's own
/// reference plus this guard's and no other.
struct GateGuard {
    gates: Arc<Mutex<Gates>>,
    key: Key,
    gate: Arc<tokio::sync::Mutex<()>>,
}

impl GateGuard {
    /// Serialize against every other open for this key.
    async fn lock(&self) -> tokio::sync::MutexGuard<'_, ()> {
        self.gate.lock().await
    }
}

impl Drop for GateGuard {
    fn drop(&mut self) {
        let mut gates = lock(&self.gates);
        // Two references — the map's and ours — means no other open holds or is
        // waiting on this gate, so removing it cannot let a later open past a
        // gate someone is still standing behind. Any other count means a waiter
        // exists and the entry stays; that waiter's own guard does the removal.
        //
        // Checked under the same lock `gate()` clones under, so a `gate()` that
        // is about to bump the count cannot slip between the check and the
        // removal.
        if gates
            .get(&self.key)
            .is_some_and(|g| Arc::strong_count(g) == 2)
        {
            gates.remove(&self.key);
        }
    }
}

// ---------------------------------------------------------------------------
// one open lane
// ---------------------------------------------------------------------------

struct LaneEntry {
    /// The `server_url` this entry was opened against. The eviction key for a
    /// restarted tab: a new port means a different server, not a reconnect.
    server_url: String,
    /// This entry's share of the tunnel it rides, if any. `None` for a local
    /// lane. Taken by [`LaneEntry::retire`] rather than waited for: an op
    /// holding a clone of the `Arc<LaneEntry>` across an await must not be able
    /// to delay a closed panel's `ssh` child from dying.
    forward: Mutex<Option<ForwardShare>>,
    /// What `lane.open` answered with, cached so a second `open` is genuinely
    /// idempotent rather than a second round trip.
    session: LaneSession,
    capabilities: LaneCapabilities,
    client: Arc<OpencodeClient>,
    view: Arc<Mutex<LaneView>>,
    /// The supervision loop. Shares nothing with this struct but the view, so
    /// there is no reference cycle and dropping the entry really does end it.
    pump: tokio::task::JoinHandle<()>,
}

impl LaneEntry {
    fn opened(&self) -> Value {
        json!({ "session": self.session, "capabilities": self.capabilities })
    }

    /// End the subscription and give the tunnel share back. Idempotent, and
    /// **must not be called under the `inner` lock** — releasing the last share
    /// takes it.
    fn retire(&self) {
        self.pump.abort();
        drop(lock(&self.forward).take());
    }
}

impl Drop for LaneEntry {
    /// Ending the pump drops the [`shed_core::lane::LaneStop`] it holds, which
    /// aborts the adapter's watcher, which closes the `/event` socket.
    ///
    /// A backstop: every eviction path calls [`LaneEntry::retire`] first.
    fn drop(&mut self) {
        self.pump.abort();
    }
}

// ---------------------------------------------------------------------------
// the layer
// ---------------------------------------------------------------------------

#[derive(Default)]
struct Inner {
    entries: HashMap<Key, Arc<LaneEntry>>,
    forwards: HashMap<ForwardKey, ForwardSlot>,
    /// The opens currently between their first await and their commit. See
    /// [`Pending`].
    pending: HashMap<Key, Pending>,
}

/// The open gates, by key. See [`Lanes::gates`] and [`GateGuard`].
type Gates = HashMap<Key, Arc<tokio::sync::Mutex<()>>>;

/// Where a lane frame goes on its way to the UI.
///
/// A closure rather than the [`AppHandle`] itself so the ownership rules below
/// can be tested without a Tauri app; production builds one that emits
/// [`LANE_EVENT`] and nothing else does.
type EventSink = Arc<dyn Fn(&str, &str, &LaneEvent) + Send + Sync>;

/// Every open lane in this app, and the tunnels under them.
pub struct Lanes {
    handle: tokio::runtime::Handle,
    sink: EventSink,
    machines: Arc<dyn LaneMachines>,
    inner: Arc<Mutex<Inner>>,
    /// One async gate per key, so two concurrent `lane.open`s on the same
    /// session build ONE entry and ONE subscription.
    ///
    /// Per key rather than one global gate: an `open` may block for as long as
    /// `SshForward::ensure`'s readiness deadline, and a slow machine must not
    /// hold up a panel on a different one.
    ///
    /// It serialises opens against each OTHER; it says nothing about `close` and
    /// `reconcile`, which is what [`Pending`] is for.
    ///
    /// **Transient.** An entry lives only while an open holds or waits on it
    /// ([`GateGuard`]) — the keys are caller-supplied strings off an IPC socket,
    /// and a map that only grows is a leak reachable by anyone who can name a
    /// machine and a session.
    gates: Arc<Mutex<Gates>>,
}

impl Lanes {
    pub fn new(handle: tokio::runtime::Handle, app: AppHandle, machines: Arc<Machines>) -> Lanes {
        let sink: EventSink =
            Arc::new(move |machine: &str, session_id: &str, event: &LaneEvent| {
                let _ = app.emit(
                    LANE_EVENT,
                    json!({ "machine": machine, "session_id": session_id, "event": event }),
                );
            });
        Lanes::with_sink(handle, sink, machines)
    }

    fn with_sink(
        handle: tokio::runtime::Handle,
        sink: EventSink,
        machines: Arc<dyn LaneMachines>,
    ) -> Lanes {
        Lanes {
            handle,
            sink,
            machines,
            inner: Arc::new(Mutex::new(Inner::default())),
            gates: Arc::new(Mutex::new(Gates::new())),
        }
    }

    /// `lane.open` — ensure the transport, build the client, start the
    /// subscription, and answer with the session row plus what this adapter can
    /// do.
    ///
    /// **Idempotent.** A second call for a key that is already open re-answers
    /// from the entry; it does not open a second subscription. A call for a key
    /// whose row now reports a DIFFERENT `server_url` evicts the stale entry and
    /// opens against the new one — that is a restarted tab, not a reconnect.
    ///
    /// **Transactional.** See the module doc: everything it builds is owned
    /// while it builds it, and it commits under the same lock acquisition that
    /// re-checks whether the lane is still wanted. There is no path on which it
    /// leaves a tunnel behind, and none on which it inserts an entry for a key
    /// that was closed or evicted while it was in flight.
    pub async fn open(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let key = (machine.to_string(), session_id.to_string());
        // The row is resolved BEFORE a gate is registered for the key. Both IPC
        // doors take `machine` and `session_id` as free strings, so a call that
        // names nothing real must not be able to make this app remember it:
        // `lane.open` with junk (or with the session ids of a machine whose tabs
        // churn) used to mint a gate per call and keep it for the life of the
        // process.
        self.lane_url(machine, session_id)?;
        let gate = self.gate(&key);
        let _serialized = gate.lock().await;

        // Re-resolved under the gate, and this is the value everything below
        // uses: the pre-gate one was read before waiting, and waiting is exactly
        // when a tab restarts onto a new port or goes away. Using it would open
        // a lane against a socket the snapshot has already retired.
        let server_url = self.lane_url(machine, session_id)?;
        if let Some(entry) = self.entry(&key) {
            if entry.server_url == server_url {
                return Ok(entry.opened());
            }
            self.evict(&key);
        }

        // Declared BEFORE the first await, so a `close` or a `reconcile` landing
        // anywhere below has something to cancel.
        let _pending = self.declare(&key, &server_url);

        let kind = self
            .machines
            .reach_kind(machine)
            .map_err(|e| LaneFailure::Lane(LaneError::Unavailable(e)))?;
        // Reserved, not merely created: from here every `?` gives the share back
        // (and with it the `ssh` child, if this open was its only user).
        let (base_url, forward) = self.transport(machine, &kind, &server_url).await?;

        let url = reqwest::Url::parse(&base_url).map_err(|e| {
            LaneFailure::Lane(LaneError::BadRequest(format!(
                "the reported agent server {server_url:?} is not a usable URL: {e}"
            )))
        })?;
        // No credential source in this cut — see the module doc.
        let client = Arc::new(OpencodeClient::new(url, None)?);
        // The roster row is fetched BEFORE the subscription starts: a 404 here
        // is an honest `unknown_session` the caller can render, where the same
        // failure inside the pump would be a `Down` the panel has to wait for.
        // It is also the last await, and the one that fails on a
        // password-protected agent — hence the share above.
        let session = client.session(session_id).await?;
        let capabilities = client.capabilities();

        let view = Arc::new(Mutex::new(LaneView::default()));
        // Commit, or roll back. ONE acquisition: the re-check and the insert
        // must not be separable, or a `close` landing between them would be
        // lost — and the pump is not started until the commit is decided, so a
        // rolled-back open never had a subscription to leak either.
        let mut inner = lock(&self.inner);
        if !inner.pending.get(&key).is_some_and(|p| !p.cancelled) {
            return Err(LaneFailure::NoLane(format!(
                "the lane for session {session_id:?} on machine {machine:?} was closed \
                 while it was being opened"
            )));
        }
        let pump = self.spawn_pump(
            machine.to_string(),
            session_id.to_string(),
            Arc::clone(&client),
            Arc::clone(&view),
            forward.as_ref().map(ForwardShare::weak),
        );
        let entry = Arc::new(LaneEntry {
            server_url,
            forward: Mutex::new(forward),
            session,
            capabilities,
            client,
            view,
            pump,
        });
        let opened = entry.opened();
        inner.entries.insert(key, entry);
        Ok(opened)
    }

    /// `lane.messages` — the staged-then-swapped view.
    pub fn messages(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        let view = lock(&entry.view);
        Ok(view.messages())
    }

    /// `lane.approvals` — what is blocking on the human, this session's and its
    /// descendants'.
    pub fn approvals(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        let view = lock(&entry.view);
        Ok(view.approvals())
    }

    /// `lane.send` — a prompt. `mode` defaults to `queue`; `interject` is
    /// refused by the adapter (`capabilities.interject` is false) rather than
    /// silently downgraded.
    pub async fn send(
        &self,
        machine: &str,
        session_id: &str,
        text: &str,
        mode: SendMode,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        entry.client.send(session_id, text, mode).await?;
        Ok(json!({}))
    }

    /// `lane.cancel` — stop the turn in flight.
    pub async fn cancel(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        entry.client.cancel(session_id).await?;
        Ok(json!({}))
    }

    /// `lane.answer` — resolve one approval.
    pub async fn answer(
        &self,
        machine: &str,
        session_id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        entry.client.answer(session_id, approval_id, answer).await?;
        Ok(json!({}))
    }

    /// `lane.close` — end the subscription and release the transport.
    ///
    /// Idempotent: closing a lane that is not open is success, because the
    /// caller's intent (there is no lane here any more) is already true. The
    /// panel calls this on unmount, and an unmount can race an eviction.
    pub fn close(&self, machine: &str, session_id: &str) -> Value {
        self.evict(&(machine.to_string(), session_id.to_string()));
        json!({})
    }

    /// Reconcile one machine's open lanes against a fresh roost snapshot: evict
    /// every entry whose tab is gone or whose `server_url` moved.
    ///
    /// See [`crate::machines::OnLanes`] for why the snapshot is the signal.
    pub fn reconcile(&self, machine: &str, lanes: &BTreeMap<String, String>) {
        let gone: Vec<Arc<LaneEntry>> = {
            let mut inner = lock(&self.inner);
            // Opens still in flight are judged by the SAME rule as committed
            // entries. Without this a tab that went away mid-open would be
            // resurrected by the open that was already past the check.
            for (key, pending) in inner.pending.iter_mut() {
                if key.0 == machine && lanes.get(&key.1) != Some(&pending.server_url) {
                    pending.cancelled = true;
                }
            }
            let mut gone = Vec::new();
            inner.entries.retain(|(m, session_id), entry| {
                if m != machine {
                    return true;
                }
                let keep = lanes
                    .get(session_id)
                    .is_some_and(|url| url == &entry.server_url);
                if !keep {
                    gone.push(Arc::clone(entry));
                }
                keep
            });
            gone
        };
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        for entry in gone {
            entry.retire();
        }
    }

    // ---- internals ----

    /// Declare an open in flight for `key`. See [`Pending`].
    fn declare(&self, key: &Key, server_url: &str) -> PendingGuard {
        lock(&self.inner).pending.insert(
            key.clone(),
            Pending {
                server_url: server_url.to_string(),
                cancelled: false,
            },
        );
        PendingGuard {
            inner: Arc::clone(&self.inner),
            key: key.clone(),
        }
    }

    /// The per-key open gate, created on first use and dropped by the last
    /// holder. See [`GateGuard`].
    fn gate(&self, key: &Key) -> GateGuard {
        let mut gates = lock(&self.gates);
        let gate = Arc::clone(
            gates
                .entry(key.clone())
                .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(()))),
        );
        GateGuard {
            gates: Arc::clone(&self.gates),
            key: key.clone(),
            gate,
        }
    }

    fn entry(&self, key: &Key) -> Option<Arc<LaneEntry>> {
        lock(&self.inner).entries.get(key).map(Arc::clone)
    }

    /// The entry every verb but `open` needs, or the reason there is none.
    fn open_entry(&self, machine: &str, session_id: &str) -> Result<Arc<LaneEntry>, LaneFailure> {
        let key = (machine.to_string(), session_id.to_string());
        if let Some(entry) = self.entry(&key) {
            return Ok(entry);
        }
        // Distinguish the two shapes of "no lane" in the MESSAGE, not the code:
        // a caller does the same thing about both (there is no transcript here),
        // and a second code would be one more thing for a client to branch on
        // for no behavioural difference.
        match self.lane_url(machine, session_id) {
            Err(e) => Err(e),
            Ok(_) => Err(LaneFailure::NoLane(format!(
                "no lane is open for session {session_id:?} on machine {machine:?} — \
                 call lane.open first"
            ))),
        }
    }

    /// The `server_url` this row reports, or `no_lane`.
    fn lane_url(&self, machine: &str, session_id: &str) -> Result<String, LaneFailure> {
        self.machines
            .agent_lanes(machine)
            .remove(session_id)
            .ok_or_else(|| {
                LaneFailure::NoLane(format!(
                    "session {session_id:?} on machine {machine:?} carries no agent_lane \
                     (its tab reported no agent server)"
                ))
            })
    }

    /// Resolve the base URL to build a client on, RESERVING a share of the
    /// tunnel (and starting it) first when the machine is remote.
    ///
    /// The share is taken BEFORE `ensure` is awaited, which is what makes the
    /// tunnel visibly in-use for the whole window an open occupies.
    async fn transport(
        &self,
        machine: &str,
        kind: &ReachKind,
        server_url: &str,
    ) -> Result<(String, Option<ForwardShare>), LaneFailure> {
        let entry = match kind {
            // Its loopback is ours.
            ReachKind::Local => return Ok((server_url.to_string(), None)),
            ReachKind::Ssh(entry) => entry,
        };
        let key: ForwardKey = (machine.to_string(), remote_port(server_url)?);
        let share = self.reserve(key, entry)?;
        share
            .forward
            .ensure()
            .await
            .map_err(LaneError::Unavailable)?;
        let base = format!("http://127.0.0.1:{}/", share.port());
        Ok((base, Some(share)))
    }

    /// A share of the tunnel for `key`, joining the existing one or building it.
    fn reserve(&self, key: ForwardKey, entry: &MachineEntry) -> Result<ForwardShare, LaneFailure> {
        if let Some(share) = self.join(&key) {
            return Ok(share);
        }
        // Build OUTSIDE the lock: the reservation binds a socket to read the
        // port assignment back, and a second reserve for a key another task just
        // inserted would leak a port. Insert-if-absent under one acquisition
        // settles the race in favour of whoever got there first; the loser's
        // unspawned forward is simply dropped.
        let fresh = Arc::new(OwnedForward::new(
            self.machines
                .forward(entry, key.1)
                .map_err(LaneError::Unavailable)?,
        ));
        let mut inner = lock(&self.inner);
        let slot = inner
            .forwards
            .entry(key.clone())
            .or_insert_with(|| ForwardSlot {
                forward: fresh,
                users: 0,
            });
        slot.users += 1;
        Ok(ForwardShare {
            inner: Arc::clone(&self.inner),
            key,
            forward: Arc::clone(&slot.forward),
        })
    }

    /// Take a share of an EXISTING tunnel, if there is one.
    fn join(&self, key: &ForwardKey) -> Option<ForwardShare> {
        let mut inner = lock(&self.inner);
        let slot = inner.forwards.get_mut(key)?;
        slot.users += 1;
        let forward = Arc::clone(&slot.forward);
        Some(ForwardShare {
            inner: Arc::clone(&self.inner),
            key: key.clone(),
            forward,
        })
    }

    /// Remove one entry, cancel any open still in flight for it, and give back
    /// the tunnel share it held.
    fn evict(&self, key: &Key) {
        let entry = {
            let mut inner = lock(&self.inner);
            // The open that has not committed yet is the one `close` used to
            // miss entirely: no entry to remove, so nothing happened, and the
            // open went on to insert a lane nobody wanted.
            if let Some(pending) = inner.pending.get_mut(key) {
                pending.cancelled = true;
            }
            inner.entries.remove(key)
        };
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        if let Some(entry) = entry {
            entry.retire();
        }
    }

    /// The supervision loop for one lane: ensure the transport, subscribe, pump
    /// frames into the view and out to the UI, and start over on a backoff when
    /// the subscription ends.
    ///
    /// The adapter reconnects on its own inside one subscription (that is the
    /// `Reset` … `Ready` bracket); this loop is the layer ABOVE it, and it
    /// exists for the failure the adapter cannot fix — a transport that has gone
    /// away. On a remote machine, re-`ensure`ing the forward is what respawns a
    /// dead `ssh -N` child before redialing.
    ///
    /// **Two places re-`ensure`, and the second one is the one that matters.**
    /// Before subscribing is the obvious one. But once a subscription has
    /// connected, the adapter retries transport failures INTERNALLY and
    /// indefinitely and its receiver stays open, so a tunnel that dies under an
    /// established lane never ends `rx.recv()` and this loop never comes back
    /// round — only a close and a reopen recovered it. The adapter DOES announce
    /// each attempt: it emits [`LaneEvent::Reset`] at the start of every
    /// generation, reconnects included. So every Reset after the first of a
    /// subscription re-`ensure`s, which respawns the child inside the adapter's
    /// own backoff (§3.4's "the watcher's reconnect drops and re-`ensure`s the
    /// forward inside the backoff loop"). No polling: the adapter's retry
    /// cadence IS the cadence, and a healthy child makes `ensure` a liveness
    /// probe that returns at once.
    fn spawn_pump(
        &self,
        machine: String,
        session_id: String,
        client: Arc<OpencodeClient>,
        view: Arc<Mutex<LaneView>>,
        forward: Option<Weak<OwnedForward>>,
    ) -> tokio::task::JoinHandle<()> {
        let sink = Arc::clone(&self.sink);
        self.handle.spawn(async move {
            let mut backoff = RESUBSCRIBE_BASE;
            loop {
                match ensure_forward(&forward).await {
                    Ensured::Ready => {}
                    Ensured::Gone => return,
                    Ensured::Failed(e) => {
                        note_down(&sink, &view, &machine, &session_id, format!("forward: {e}"));
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                }
                let subscription = match client.subscribe(&session_id, None).await {
                    Ok(subscription) => subscription,
                    // The session is gone for good. Anything else is worth
                    // retrying — the agent may simply be restarting.
                    Err(LaneError::UnknownSession) => {
                        note_down(
                            &sink,
                            &view,
                            &machine,
                            &session_id,
                            DOWN_UNKNOWN_SESSION.to_string(),
                        );
                        return;
                    }
                    Err(e) => {
                        note_down(&sink, &view, &machine, &session_id, e.to_string());
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                };
                // BOTH halves, for the whole read: taking `.rx` alone drops the
                // stop handle, which aborts the pump it is reading from.
                let (mut rx, stop) = subscription.into_parts();
                let mut down: Option<String> = None;
                // The subscription's FIRST Reset is the seed of the connect this
                // loop just ensured for; every later one is a reconnect.
                let mut generations = 0usize;
                while let Some(event) = rx.recv().await {
                    match &event {
                        // A generation that reached steady state is the signal
                        // the transport is healthy again.
                        LaneEvent::Ready { .. } => backoff = RESUBSCRIBE_BASE,
                        LaneEvent::Down { reason } => down = Some(reason.clone()),
                        LaneEvent::Reset { .. } => generations += 1,
                        _ => {}
                    }
                    lock(&view).apply(&event);
                    emit(&sink, &machine, &session_id, &event);
                    if generations > 1 && matches!(event, LaneEvent::Reset { .. }) {
                        match ensure_forward(&forward).await {
                            Ensured::Ready => {}
                            // Every share is gone: this lane was evicted.
                            Ensured::Gone => return,
                            // Stale-with-a-reason, and keep reading: the adapter
                            // is still retrying, and its next Reset is the next
                            // attempt at the tunnel too.
                            Ensured::Failed(e) => note_down(
                                &sink,
                                &view,
                                &machine,
                                &session_id,
                                format!("forward: {e}"),
                            ),
                        }
                    }
                }
                drop(stop);
                if down.as_deref() == Some(DOWN_UNKNOWN_SESSION) {
                    return;
                }
                tokio::time::sleep(backoff).await;
                backoff = next_backoff(backoff);
            }
        })
    }
}

/// What one attempt at making the tunnel usable said.
enum Ensured {
    /// Usable — or there is no tunnel, because the lane is local.
    Ready,
    /// Every share is gone: the entry was evicted and the pump has no work.
    Gone,
    Failed(String),
}

async fn ensure_forward(forward: &Option<Weak<OwnedForward>>) -> Ensured {
    let Some(weak) = forward.as_ref() else {
        return Ensured::Ready;
    };
    let Some(forward) = weak.upgrade() else {
        return Ensured::Gone;
    };
    match forward.ensure().await {
        Ok(()) => Ensured::Ready,
        Err(e) => Ensured::Failed(e),
    }
}

/// Record a transport-level failure as the same stale-with-a-reason state a
/// [`LaneEvent::Down`] produces, and tell the UI about it on the same event.
///
/// The panel must not care whether the thing that went away was the agent or the
/// tunnel to it: both mean "this transcript is not live", and both are recovered
/// by the same retry.
fn note_down(
    sink: &EventSink,
    view: &Arc<Mutex<LaneView>>,
    machine: &str,
    session_id: &str,
    reason: String,
) {
    let event = LaneEvent::Down { reason };
    lock(view).apply(&event);
    emit(sink, machine, session_id, &event);
}

fn emit(sink: &EventSink, machine: &str, session_id: &str, event: &LaneEvent) {
    (**sink)(machine, session_id, event);
}

fn next_backoff(current: Duration) -> Duration {
    std::cmp::min(current.saturating_mul(2), RESUBSCRIBE_MAX)
}

/// The loopback port a `server_url` names.
///
/// Explicit rather than `Url::port_or_known_default()`: roost validates that the
/// URL is loopback and carries a port before it stamps it, so a URL that arrives
/// here without one is not an http default to fill in — it is a fact this app
/// cannot act on, and guessing 80 would build a tunnel to nothing.
fn remote_port(server_url: &str) -> Result<u16, LaneFailure> {
    let url = reqwest::Url::parse(server_url).map_err(|e| {
        LaneFailure::Lane(LaneError::BadRequest(format!(
            "the reported agent server {server_url:?} is not a usable URL: {e}"
        )))
    })?;
    url.port().ok_or_else(|| {
        LaneFailure::Lane(LaneError::BadRequest(format!(
            "the reported agent server {server_url:?} names no port to forward"
        )))
    })
}

/// Decode the `answer` object the `lane.answer` op takes.
///
/// Three forms, one per thing a client can actually do (plan 015 §3.4):
///
/// * `{"permission": "allow-once" | "allow-always" | "reject"}`
/// * `{"question": [["<option id>", …], …]}` — one inner list per question
/// * `{"reject": true}` — decline the whole request
///
/// STRICT, like the contract's own [`LaneAnswer`]: an answer is a command, and a
/// shape this build cannot name must be refused rather than degraded into
/// something the user did not ask for.
///
/// The refusal is a [`LaneFailure`] rather than a bare string ON PURPOSE: a bare
/// string is exactly what a `#[tauri::command]`'s error channel is, so `?` on it
/// compiled and silently dropped the `bad_request` code that the socket door
/// supplies for the identical input. With a typed refusal neither door can lose
/// it — the command one will not compile without saying how it is spelled.
///
/// **Exactly one form.** The keys are counted before any of them is read, so a
/// payload naming two — `{"permission": "reject", "question": [["yes"]]}` — is
/// refused instead of resolving as whichever the code happened to check first
/// and silently discarding the other half. An answer RESOLVES an approval; a
/// client that sent two of them has to be told which one did not happen.
pub fn parse_answer(value: &Value) -> Result<LaneAnswer, LaneFailure> {
    /// The answer forms, and the whole vocabulary this refusal counts.
    const FORMS: [&str; 3] = ["permission", "question", "reject"];
    let named: Vec<&str> = FORMS
        .into_iter()
        .filter(|form| value.get(form).is_some())
        .collect();
    match named.as_slice() {
        [_one] => {}
        [] => {
            return Err(LaneFailure::bad_request(
                "answer must be {permission: …}, {question: [[…]]} or {reject: true}".to_string(),
            ))
        }
        several => {
            return Err(LaneFailure::bad_request(format!(
                "an answer names exactly one of permission, question or reject; \
                 this one names {}",
                several.join(" and ")
            )))
        }
    }

    if let Some(decision) = value.get("permission") {
        let decision = decision
            .as_str()
            .ok_or_else(|| LaneFailure::bad_request("`permission` must be a string".to_string()))?
            .trim();
        let decision = match decision {
            "allow-once" => LaneDecision::AllowOnce,
            "allow-always" => LaneDecision::AllowAlways,
            "reject" => LaneDecision::Reject,
            other => {
                return Err(LaneFailure::bad_request(format!(
                    "unknown permission decision {other:?} \
                     (want allow-once, allow-always or reject)"
                )))
            }
        };
        return Ok(LaneAnswer::Permission { decision });
    }
    if let Some(answers) = value.get("question") {
        let answers: Vec<Vec<String>> = serde_json::from_value(answers.clone()).map_err(|e| {
            LaneFailure::bad_request(format!(
                "`question` must be a list of lists of option ids: {e}"
            ))
        })?;
        return Ok(LaneAnswer::Question { answers });
    }
    match value.get("reject").and_then(Value::as_bool) {
        Some(true) => Ok(LaneAnswer::Reject),
        // `{"reject": false}` is not "do nothing", it is a client that meant
        // something this grammar cannot express.
        _ => Err(LaneFailure::bad_request(
            "`reject` must be the literal true".to_string(),
        )),
    }
}

/// Decode the optional `mode` of `lane.send`.
pub fn parse_mode(value: Option<&str>) -> Result<SendMode, LaneFailure> {
    match value.map(str::trim).unwrap_or("queue") {
        "queue" | "" => Ok(SendMode::Queue),
        "interject" => Ok(SendMode::Interject),
        other => Err(LaneFailure::bad_request(format!(
            "unknown send mode {other:?} (want queue or interject)"
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::collections::HashSet;

    use shed_core::lane::{LaneApprovalKind, LaneApprovalStatus};

    /// A transcript row. `text` is the identity in these tests — the feed row
    /// has no id of its own, `seq` is the transport's, and the text is what a
    /// reader would actually see.
    fn message(text: &str, seq: u64) -> RcFeedMessage {
        RcFeedMessage {
            seq,
            role: "assistant".to_string(),
            msg_type: "text".to_string(),
            text: Some(text.to_string()),
            ..RcFeedMessage::default()
        }
    }

    fn approval(id: &str, status: LaneApprovalStatus) -> LaneApproval {
        LaneApproval {
            id: id.to_string(),
            session_id: "ses_a".to_string(),
            kind: LaneApprovalKind::Permission,
            status,
            title: id.to_string(),
            detail: None,
            options: Vec::new(),
            questions: Vec::new(),
            request_json: "{}".to_string(),
            created_at_unix_ms: None,
        }
    }

    fn rows(view: &LaneView) -> Vec<String> {
        view.live
            .messages
            .iter()
            .map(|m| m.text.clone().unwrap_or_default())
            .collect::<Vec<_>>()
    }

    /// **The whole point of staging**: between `Reset` and `Ready` a reader sees
    /// the PREVIOUS generation whole, never an empty or half-seeded one.
    #[test]
    fn a_reseed_never_shows_a_partial_view() {
        let mut view = LaneView::default();
        for event in [
            LaneEvent::Reset {
                reason: "seed".into(),
                generation: 1,
            },
            LaneEvent::Message {
                message: message("m1", 1),
                cursor: None,
            },
            LaneEvent::Ready { generation: 1 },
        ] {
            view.apply(&event);
        }
        assert_eq!(rows(&view), ["m1"]);
        assert_eq!(view.live.generation, 1);

        // A reconnect starts over. Mid-seed the live view is untouched …
        view.apply(&LaneEvent::Reset {
            reason: "reconnect".into(),
            generation: 1,
        });
        assert_eq!(rows(&view), ["m1"], "the live view was cleared mid-seed");
        assert_eq!(
            view.messages()["generation"],
            json!(1),
            "the reported generation moved before the rows it names did"
        );
        view.apply(&LaneEvent::Message {
            message: message("m1", 2),
            cursor: None,
        });
        assert_eq!(
            rows(&view),
            ["m1"],
            "a staged row leaked into the live view"
        );
        view.apply(&LaneEvent::Message {
            message: message("m2", 3),
            cursor: None,
        });
        assert_eq!(rows(&view), ["m1"]);

        // … and swaps in whole at Ready, with a HIGHER generation.
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert_eq!(rows(&view), ["m1", "m2"]);
        assert_eq!(
            view.messages()["generation"],
            json!(2),
            "generation must be ours, monotonic, and move only on the swap"
        );
        assert_eq!(
            view.live.messages.back().map(|m| m.seq),
            Some(3),
            "the re-seeded rows keep the adapter's higher seqs"
        );
    }

    /// `Down` is stale-with-a-reason, not a wipe; the next `Ready` clears it.
    #[test]
    fn down_marks_stale_and_keeps_the_rows() {
        let mut view = LaneView::default();
        view.apply(&LaneEvent::Reset {
            reason: "seed".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Message {
            message: message("m1", 1),
            cursor: None,
        });
        view.apply(&LaneEvent::Ready { generation: 1 });

        view.apply(&LaneEvent::Down {
            reason: "unknown_session".into(),
        });
        assert_eq!(rows(&view), ["m1"], "Down cleared the transcript");
        let payload = view.messages();
        assert_eq!(payload["stale"], json!("unknown_session"));
        assert_eq!(payload["generation"], json!(1));
        assert_eq!(payload["activity"], json!("unknown"));

        view.apply(&LaneEvent::Reset {
            reason: "reconnect".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert_eq!(view.messages()["stale"], Value::Null);
    }

    /// An `Unknown` frame is IGNORED — not a gap, not a reason to resubscribe,
    /// and above all not something that disturbs a staging swap in progress.
    #[test]
    fn an_unknown_frame_changes_nothing() {
        let mut view = LaneView::default();
        view.apply(&LaneEvent::Reset {
            reason: "seed".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Unknown);
        view.apply(&LaneEvent::Message {
            message: message("m1", 1),
            cursor: None,
        });
        view.apply(&LaneEvent::Unknown);
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert_eq!(rows(&view), ["m1"]);
        assert_eq!(view.live.generation, 1);
    }

    /// The ring is bounded, oldest-first, at the adapter's own 500.
    #[test]
    fn the_view_keeps_the_last_500_rows() {
        let mut view = LaneView::default();
        for i in 0..(MAX_VIEW_MESSAGES + 25) {
            view.apply(&LaneEvent::Message {
                message: message(&format!("m{i}"), i as u64 + 1),
                cursor: None,
            });
        }
        assert_eq!(view.live.messages.len(), MAX_VIEW_MESSAGES);
        assert_eq!(
            view.live.messages.front().and_then(|m| m.text.clone()),
            Some("m25".into())
        );
    }

    /// Approvals are id-keyed and last-write-wins, and only PENDING ones are
    /// offered — an unknown status is not an affordance.
    #[test]
    fn approvals_are_last_write_wins_and_only_pending_surface() {
        let mut view = LaneView::default();
        for a in [
            approval("per_1", LaneApprovalStatus::Pending),
            approval("per_2", LaneApprovalStatus::Pending),
            approval("per_3", LaneApprovalStatus::Other("cancelled".into())),
            // The resolution of per_1, arriving without its own `pending`
            // predecessor having been re-sent.
            approval("per_1", LaneApprovalStatus::Resolved),
        ] {
            view.apply(&LaneEvent::Approval { approval: a });
        }
        let open = view.approvals();
        let ids: Vec<&str> = open["approvals"]
            .as_array()
            .expect("approvals is a list")
            .iter()
            .map(|a| a["id"].as_str().unwrap_or_default())
            .collect();
        assert_eq!(ids, ["per_2"]);
    }

    /// **Review finding: resolved approvals accumulated without bound.** A
    /// generation ends only at a `Reset`, and a healthy
    /// lane can run for days without one — so every approval that RESOLVED
    /// inside it used to stay in the snapshot forever, whole payload and
    /// `request_json` included, invisible to every reader.
    #[test]
    fn a_resolved_approval_is_dropped_and_a_reopened_one_comes_back() {
        let mut view = LaneView::default();
        let resolutions = [
            LaneApprovalStatus::Resolved,
            LaneApprovalStatus::Submitted,
            LaneApprovalStatus::Other("cancelled".into()),
        ];
        for i in 0..300 {
            let id = format!("per_{i}");
            view.apply(&LaneEvent::Approval {
                approval: approval(&id, LaneApprovalStatus::Pending),
            });
            assert_eq!(
                view.live.approvals.len(),
                1,
                "an ask the agent is waiting on must be held"
            );
            view.apply(&LaneEvent::Approval {
                approval: approval(&id, resolutions[i % resolutions.len()].clone()),
            });
            assert_eq!(
                view.live.approvals.len(),
                0,
                "{id} was still held after it stopped waiting on anyone"
            );
        }

        // A resolution for an id this view never saw pending is not a way to
        // plant one either.
        view.apply(&LaneEvent::Approval {
            approval: approval("never_asked", LaneApprovalStatus::Resolved),
        });
        assert!(view.live.approvals.is_empty());

        // Dropping is not forgetting: the agent re-opening an id it already
        // resolved is a NEW ask, and it has to render.
        view.apply(&LaneEvent::Approval {
            approval: approval("per_7", LaneApprovalStatus::Pending),
        });
        let ids: Vec<String> = view.approvals()["approvals"]
            .as_array()
            .expect("approvals is a list")
            .iter()
            .map(|a| a["id"].as_str().unwrap_or_default().to_string())
            .collect();
        assert_eq!(ids, ["per_7"]);
    }

    /// Every [`LaneError`] variant has a distinct snake_case code, and `no_lane`
    /// is ours.
    #[test]
    fn every_failure_maps_to_the_contracts_code() {
        let cases = [
            (LaneError::Unauthorized, "unauthorized"),
            (LaneError::BadRequest("x".into()), "bad_request"),
            (LaneError::UnknownSession, "unknown_session"),
            (LaneError::UnknownApproval, "unknown_approval"),
            (LaneError::AlreadySubmitted, "already_submitted"),
            (LaneError::AlreadyResolved, "already_resolved"),
            (LaneError::NotAccepting, "not_accepting"),
            (LaneError::Unavailable("x".into()), "unavailable"),
            (LaneError::Failed("x".into()), "failed"),
        ];
        let mut seen = HashSet::new();
        for (error, code) in cases {
            let failure = LaneFailure::from(error);
            assert_eq!(failure.code(), code);
            assert!(seen.insert(code), "duplicate code {code}");
        }
        assert_eq!(LaneFailure::NoLane("nope".into()).code(), "no_lane");
        assert_eq!(LaneFailure::NoLane("nope".into()).message(), "nope");
    }

    #[test]
    fn the_answer_forms_are_the_three_the_plan_names() {
        assert_eq!(
            parse_answer(&json!({"permission": "allow-once"})).expect("allow-once"),
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce
            }
        );
        assert_eq!(
            parse_answer(&json!({"permission": "allow-always"})).expect("allow-always"),
            LaneAnswer::Permission {
                decision: LaneDecision::AllowAlways
            }
        );
        assert_eq!(
            parse_answer(&json!({"permission": "reject"})).expect("reject"),
            LaneAnswer::Permission {
                decision: LaneDecision::Reject
            }
        );
        assert_eq!(
            parse_answer(&json!({"question": [["yes"], ["a", "b"]]})).expect("question"),
            LaneAnswer::Question {
                answers: vec![vec!["yes".into()], vec!["a".into(), "b".into()]]
            }
        );
        assert_eq!(
            parse_answer(&json!({"reject": true})).expect("bare reject"),
            LaneAnswer::Reject
        );
        // Strict: an unnamed decision is refused, not defaulted.
        assert!(parse_answer(&json!({"permission": "maybe"})).is_err());
        assert!(parse_answer(&json!({"question": "yes"})).is_err());
        assert!(parse_answer(&json!({"reject": false})).is_err());
        assert!(parse_answer(&json!({})).is_err());
    }

    /// **Review finding: an answer naming two forms was guessed at, not
    /// refused.** The forms used to be TRIED in order, so a payload
    /// naming two resolved as whichever was checked first and threw the other
    /// half away — the one thing the function's own doc says it must not do. An
    /// answer resolves an approval; a client that sent two has to be told.
    #[test]
    fn an_answer_that_names_two_forms_is_refused_not_guessed() {
        for ambiguous in [
            json!({"permission": "reject", "question": [["yes"]]}),
            json!({"question": [["yes"]], "permission": "reject"}),
            json!({"permission": "allow-once", "reject": true}),
            json!({"question": [["yes"]], "reject": true}),
            json!({"permission": "allow-once", "question": [["yes"]], "reject": true}),
        ] {
            let failure = parse_answer(&ambiguous)
                .err()
                .unwrap_or_else(|| panic!("{ambiguous} names two answers and must be refused"));
            assert_eq!(failure.code(), "bad_request");
            assert!(
                failure.message().contains("exactly one"),
                "the refusal must say what is wrong: {}",
                failure.message()
            );
        }
        // …and each single form still parses, so the count did not become a
        // blanket refusal. A key that is NOT one of the three is not counted:
        // `{permission, note}` is still one answer.
        assert!(parse_answer(&json!({"permission": "reject"})).is_ok());
        assert!(parse_answer(&json!({"question": [["yes"]]})).is_ok());
        assert!(parse_answer(&json!({"reject": true})).is_ok());
        assert!(parse_answer(&json!({"permission": "reject", "note": "hi"})).is_ok());
    }

    #[test]
    fn send_modes_are_named_not_guessed() {
        assert_eq!(parse_mode(None).expect("default"), SendMode::Queue);
        assert_eq!(parse_mode(Some("queue")).expect("queue"), SendMode::Queue);
        assert_eq!(
            parse_mode(Some("interject")).expect("interject"),
            SendMode::Interject
        );
        assert!(parse_mode(Some("later")).is_err());
    }

    /// A `server_url` with no port cannot be tunnelled to, and 80 is a guess
    /// rather than a default here.
    #[test]
    fn a_server_url_must_name_the_port_a_tunnel_lands_on() {
        assert_eq!(
            remote_port("http://127.0.0.1:41234/")
                .map_err(|e| e.code())
                .expect("a port"),
            41234
        );
        assert_eq!(
            remote_port("http://127.0.0.1/").unwrap_err().code(),
            "bad_request"
        );
        assert_eq!(remote_port("not a url").unwrap_err().code(), "bad_request");
    }

    #[test]
    fn the_backoff_climbs_and_caps() {
        let mut d = RESUBSCRIBE_BASE;
        for _ in 0..10 {
            d = next_backoff(d);
        }
        assert_eq!(d, RESUBSCRIBE_MAX);
    }

    // -----------------------------------------------------------------------
    // ownership: `open` against `close`, `reconcile` and itself
    //
    // These drive the REAL `Lanes` — the real `OpencodeClient`, the real
    // watcher, the real staging — against two doubles: an in-process
    // `FakeOpencode` on a loopback port, and a machine layer whose "ssh tunnel"
    // is a scriptable stand-in that lands on that port. The double is what makes
    // the races reachable: an `ssh -N` child needs a live machine, and the
    // things that have actually gone wrong here all happen in the window
    // between reserving a tunnel and committing an entry.
    // -----------------------------------------------------------------------

    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering::SeqCst};

    use shed_app::machine::ForwardError;
    use shed_core::config::MachineEntry;
    use shed_opencode::testing::FakeOpencode;

    /// The machine every cell below opens a lane on.
    const MACHINE: &str = "m1";
    /// The FAR-side port the row reports. Deliberately NOT the fake's own port:
    /// a lane on an ssh machine must dial the FORWARD's local port, and a test
    /// where the two numbers agree would not notice if it dialed the reported
    /// one.
    const REMOTE_PORT: u16 = 45_678;

    /// What the fake tunnels did — shared by every forward one cell builds, so
    /// "how many were built" is answerable.
    #[derive(Default)]
    struct ForwardLog {
        /// Forwards BUILT. A second one for one `(machine, remote_port)` is the
        /// sharing bug.
        built: AtomicUsize,
        /// `ensure` calls. The pump's re-`ensure` on a reconnect is counted
        /// here.
        ensures: AtomicUsize,
        /// "The ssh child is up": set by `ensure`, cleared by a test killing it
        /// and by the forward's own `Drop`.
        alive: AtomicBool,
        /// Forwards DROPPED, and how many of those drops ran on a thread that
        /// belongs to the async runtime — which is what `SshForward`'s blocking
        /// `kill`+`waitpid` must never do.
        dropped: AtomicUsize,
        dropped_on_runtime: AtomicUsize,
    }

    /// A forward that is not a tunnel: it simply names the port a fake opencode
    /// is already listening on.
    struct FakeForward {
        port: u16,
        log: Arc<ForwardLog>,
    }

    #[async_trait::async_trait]
    impl MachineForward for FakeForward {
        fn port(&self) -> u16 {
            self.port
        }

        async fn ensure(&self) -> Result<(), ForwardError> {
            self.log.ensures.fetch_add(1, SeqCst);
            self.log.alive.store(true, SeqCst);
            Ok(())
        }
    }

    impl Drop for FakeForward {
        fn drop(&mut self) {
            self.log.alive.store(false, SeqCst);
            // A plain OS thread has no runtime context; a runtime worker and a
            // `spawn_blocking` thread both do. This is the probe, and the cell
            // that reads it also asserts it is not vacuous.
            if tokio::runtime::Handle::try_current().is_ok() {
                self.log.dropped_on_runtime.fetch_add(1, SeqCst);
            }
            self.log.dropped.fetch_add(1, SeqCst);
        }
    }

    /// One ssh machine, whose rows a cell can rewrite.
    struct FakeMachines {
        lanes: Mutex<BTreeMap<String, String>>,
        port: u16,
        log: Arc<ForwardLog>,
    }

    impl LaneMachines for FakeMachines {
        fn agent_lanes(&self, machine: &str) -> BTreeMap<String, String> {
            if machine == MACHINE {
                lock(&self.lanes).clone()
            } else {
                BTreeMap::new()
            }
        }

        fn reach_kind(&self, _machine: &str) -> Result<ReachKind, String> {
            Ok(ReachKind::Ssh(MachineEntry {
                name: MACHINE.to_string(),
                host: MACHINE.to_string(),
                ssh_port: 22,
                ..MachineEntry::default()
            }))
        }

        fn forward(
            &self,
            _entry: &MachineEntry,
            _remote_port: u16,
        ) -> Result<Box<dyn MachineForward>, String> {
            self.log.built.fetch_add(1, SeqCst);
            Ok(Box::new(FakeForward {
                port: self.port,
                log: Arc::clone(&self.log),
            }))
        }
    }

    /// Every `lane-event` the layer emitted, by kind.
    #[derive(Default)]
    struct Recorder {
        kinds: Mutex<Vec<&'static str>>,
    }

    impl Recorder {
        fn record(&self, event: &LaneEvent) {
            lock(&self.kinds).push(match event {
                LaneEvent::Reset { .. } => "reset",
                LaneEvent::Ready { .. } => "ready",
                LaneEvent::Down { .. } => "down",
                LaneEvent::Message { .. } => "message",
                LaneEvent::Session { .. } => "session",
                LaneEvent::Approval { .. } => "approval",
                LaneEvent::Unknown => "unknown",
            });
        }

        fn count(&self, kind: &str) -> usize {
            lock(&self.kinds).iter().filter(|k| **k == kind).count()
        }
    }

    /// The rows a machine reports: every session on ONE server.
    fn lane_rows(sessions: &[&str]) -> BTreeMap<String, String> {
        sessions
            .iter()
            .map(|s| ((*s).to_string(), format!("http://127.0.0.1:{REMOTE_PORT}/")))
            .collect()
    }

    /// A `Lanes` on the two doubles: `MACHINE` is an SSH target exposing
    /// `sessions`, and its tunnels land on `fake`'s real port.
    fn lanes_for(
        fake: &FakeOpencode,
        sessions: &[&str],
    ) -> (Arc<Lanes>, Arc<ForwardLog>, Arc<Recorder>) {
        let log = Arc::new(ForwardLog::default());
        let recorder = Arc::new(Recorder::default());
        let machines = Arc::new(FakeMachines {
            lanes: Mutex::new(lane_rows(sessions)),
            port: fake.addr().port(),
            log: Arc::clone(&log),
        });
        let sink: EventSink = {
            let recorder = Arc::clone(&recorder);
            Arc::new(move |_machine: &str, _session: &str, event: &LaneEvent| {
                recorder.record(event)
            })
        };
        let lanes = Arc::new(Lanes::with_sink(
            tokio::runtime::Handle::current(),
            sink,
            machines,
        ));
        (lanes, log, recorder)
    }

    /// A fake with one root session, seeded so its transcript is non-empty.
    async fn one_session(id: &str) -> FakeOpencode {
        let fake = FakeOpencode::start().await;
        fake.add_session(id, "the lane", "/w", None);
        fake.set_simple_transcript(id, "a question", "an answer");
        fake.set_status(id, "idle");
        fake
    }

    /// Poll `f` until it answers, or fail naming what never happened.
    ///
    /// The condition is always an in-process fact another task writes (a
    /// counter, a recorded request, a swapped-in generation), never a duration —
    /// bounded at ten seconds so a broken cell fails instead of hanging.
    async fn wait_for<T>(what: &str, mut f: impl FnMut() -> Option<T>) -> T {
        for _ in 0..2_000 {
            if let Some(value) = f() {
                return value;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("timed out waiting for {what}");
    }

    /// How many users the shared tunnel has, or `None` if there is none.
    fn forward_users(lanes: &Lanes) -> Option<usize> {
        lock(&lanes.inner)
            .forwards
            .get(&(MACHINE.to_string(), REMOTE_PORT))
            .map(|slot| slot.users)
    }

    /// The generation `lane.messages` is currently handing back — 0 before the
    /// first seed swaps in.
    fn generation(lanes: &Lanes, session: &str) -> u64 {
        lanes
            .messages(MACHINE, session)
            .ok()
            .and_then(|v| v["generation"].as_u64())
            .unwrap_or(0)
    }

    /// Park an `open` on its roster GET and hand back the task running it.
    fn open_parked(
        lanes: &Arc<Lanes>,
        session: &'static str,
    ) -> tokio::task::JoinHandle<Result<Value, LaneFailure>> {
        let lanes = Arc::clone(lanes);
        tokio::spawn(async move { lanes.open(MACHINE, session).await })
    }

    /// Wait until the fake has RECEIVED (and parked) the roster GET.
    async fn parked_on(fake: &FakeOpencode, session: &str) {
        let want = format!("/session/{session}");
        wait_for("the roster GET to arrive", || {
            fake.get_paths()
                .iter()
                .any(|p| p.ends_with(&want))
                .then_some(())
        })
        .await;
    }

    /// **Review finding: the gate map was an IPC-reachable leak.** The per-key
    /// open gate used to be created on first use and never removed, and its keys
    /// are two caller-supplied strings: `lane.open` on either IPC door with junk
    /// — or with the real session ids of a machine whose tabs churn — retained an
    /// entry for the life of the process.
    ///
    /// The rule the fix pins: a gate exists only while an open holds or waits on
    /// it. A row that resolves to nothing never mints one at all (the row is
    /// resolved BEFORE the key is registered), and a real open gives its own
    /// back on the way out.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn opens_leave_no_gate_behind() {
        let fake = one_session("ses_a").await;
        let (lanes, _log, _events) = lanes_for(&fake, &["ses_a"]);

        for i in 0..64 {
            // A machine this app has never heard of …
            let failure = lanes
                .open("no-such-machine", &format!("ses_{i}"))
                .await
                .expect_err("a machine with no rows has no lane");
            assert_eq!(failure.code(), "no_lane", "{}", failure.message());
            // … and a real one whose rows do not name this session (the churn
            // case: yesterday's tab ids, replayed).
            let failure = lanes
                .open(MACHINE, &format!("churned_{i}"))
                .await
                .expect_err("a session with no row has no lane");
            assert_eq!(failure.code(), "no_lane", "{}", failure.message());
        }
        assert!(
            lock(&lanes.gates).is_empty(),
            "{} gates were retained for lanes that never existed",
            lock(&lanes.gates).len()
        );

        // A REAL open, and its close, likewise.
        lanes.open(MACHINE, "ses_a").await.expect("the lane opens");
        assert!(
            lock(&lanes.gates).is_empty(),
            "a committed open kept its gate"
        );
        lanes.close(MACHINE, "ses_a");
        assert!(lock(&lanes.gates).is_empty());

        // And the gate still DOES its job: two concurrent opens on one key are
        // serialized into one entry, and the gate they shared is gone once both
        // are done.
        fake.hold_get("/session/ses_a");
        let first = open_parked(&lanes, "ses_a");
        let second = open_parked(&lanes, "ses_a");
        parked_on(&fake, "ses_a").await;
        wait_for("both opens to be waiting on the one gate", || {
            (lock(&lanes.gates).len() == 1).then_some(())
        })
        .await;
        fake.release_get("/session/ses_a");
        first.await.expect("the first task").expect("it opens");
        second.await.expect("the second task").expect("it opens");
        assert_eq!(
            lock(&lanes.inner).entries.len(),
            1,
            "two opens on one key built two entries"
        );
        assert!(
            lock(&lanes.gates).is_empty(),
            "the gate two opens shared outlived both of them"
        );
        lanes.close(MACHINE, "ses_a");
    }

    /// **Review finding 1.** `close` used to look for an entry, find none
    /// (the open had not inserted it yet) and do nothing — after which the open
    /// committed anyway, leaving a live subscription and an `ssh` child behind a
    /// panel the user had already closed.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_close_racing_an_open_leaves_no_entry_no_pump_and_no_tunnel() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);

        fake.hold_get("/session/ses_a");
        let opening = open_parked(&lanes, "ses_a");
        parked_on(&fake, "ses_a").await;
        // The tunnel is reserved for the whole window, which is the other half
        // of the fix — see the sharing cell.
        assert_eq!(
            forward_users(&lanes),
            Some(1),
            "an open in flight must own the tunnel it reserved"
        );

        lanes.close(MACHINE, "ses_a");
        fake.release_get("/session/ses_a");

        let failure = opening
            .await
            .expect("the open task")
            .expect_err("an open whose lane was closed must not commit");
        assert_eq!(failure.code(), "no_lane", "{}", failure.message());

        assert!(
            lock(&lanes.inner).entries.is_empty(),
            "a closed lane was resurrected by the open that was in flight"
        );
        assert_eq!(
            forward_users(&lanes),
            None,
            "the rolled-back open left its tunnel registered"
        );
        wait_for("the tunnel's child to be reaped", || {
            (log.dropped.load(SeqCst) == 1).then_some(())
        })
        .await;
        assert_eq!(
            fake.stream_count(),
            0,
            "a rolled-back open started a subscription"
        );
    }

    /// The same race, driven by the roost snapshot instead of the panel: the tab
    /// went away while the open was in flight.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_snapshot_that_retires_the_tab_mid_open_stops_the_commit() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);

        fake.hold_get("/session/ses_a");
        let opening = open_parked(&lanes, "ses_a");
        parked_on(&fake, "ses_a").await;

        // The tab is gone from the fresh snapshot.
        lanes.reconcile(MACHINE, &BTreeMap::new());
        fake.release_get("/session/ses_a");

        let failure = opening
            .await
            .expect("the open task")
            .expect_err("an open whose tab went away must not commit");
        assert_eq!(failure.code(), "no_lane", "{}", failure.message());
        assert!(lock(&lanes.inner).entries.is_empty());
        assert_eq!(forward_users(&lanes), None);
        wait_for("the tunnel's child to be reaped", || {
            (log.dropped.load(SeqCst) == 1).then_some(())
        })
        .await;
    }

    /// **Review finding 3.** The tunnel is started BEFORE the roster GET, so a
    /// GET that fails — 401 on a password-protected agent, 404 on a session that
    /// went away — used to `?` straight out and leave the forward, and its
    /// child, owned by nothing.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_failed_roster_get_leaves_no_tunnel_and_no_child() {
        for (status, code) in [(401u16, "unauthorized"), (404u16, "unknown_session")] {
            let fake = one_session("ses_a").await;
            fake.fail_get("/session/ses_a", status);
            let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);

            let failure = lanes
                .open(MACHINE, "ses_a")
                .await
                .err()
                .unwrap_or_else(|| panic!("a {status} roster GET must fail the open"));
            assert_eq!(failure.code(), code, "{}", failure.message());

            assert_eq!(
                log.built.load(SeqCst),
                1,
                "the open really did build a tunnel first ({status})"
            );
            assert_eq!(
                forward_users(&lanes),
                None,
                "a failed open left its tunnel registered ({status})"
            );
            wait_for("the tunnel's child to be reaped", || {
                (log.dropped.load(SeqCst) == 1).then_some(())
            })
            .await;
            assert!(
                !log.alive.load(SeqCst),
                "the ssh child outlived the open that spawned it ({status})"
            );
        }
    }

    /// **Review finding 4.** Two sessions on ONE agent server ride ONE tunnel —
    /// including while both opens are still in flight, and including across a
    /// reconcile that lands between them. Pruning used to walk the COMMITTED
    /// entries, see nothing using the tunnel the first open was still building,
    /// drop it, and let the second open spawn a second `ssh` child onto the same
    /// far-side port.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn two_opens_on_one_server_share_one_tunnel_across_a_reconcile() {
        let fake = one_session("ses_a").await;
        fake.add_session("ses_b", "the neighbour", "/w", None);
        fake.set_simple_transcript("ses_b", "neighbourly", "indeed");
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a", "ses_b"]);

        fake.hold_get("/session/ses_a");
        fake.hold_get("/session/ses_b");
        let a = open_parked(&lanes, "ses_a");
        let b = open_parked(&lanes, "ses_b");
        parked_on(&fake, "ses_a").await;
        parked_on(&fake, "ses_b").await;

        assert_eq!(
            log.built.load(SeqCst),
            1,
            "two sessions on one server built two tunnels"
        );
        assert_eq!(forward_users(&lanes), Some(2));

        // A roost snapshot arriving mid-open must not conclude the tunnel is
        // garbage just because nothing has committed to it yet.
        lanes.reconcile(MACHINE, &lane_rows(&["ses_a", "ses_b"]));
        assert_eq!(
            forward_users(&lanes),
            Some(2),
            "a reconcile split a tunnel two opens in flight were sharing"
        );
        assert_eq!(log.dropped.load(SeqCst), 0);

        fake.release_get("/session/ses_a");
        fake.release_get("/session/ses_b");
        a.await.expect("the ses_a task").expect("ses_a opens");
        b.await.expect("the ses_b task").expect("ses_b opens");

        assert_eq!(log.built.load(SeqCst), 1, "one server, one tunnel");
        assert_eq!(forward_users(&lanes), Some(2));

        // And the refcount is what decides when it dies: the first close keeps
        // the neighbour's tunnel, the second reaps it.
        lanes.close(MACHINE, "ses_a");
        assert_eq!(
            forward_users(&lanes),
            Some(1),
            "closing one lane took the other's tunnel with it"
        );
        assert_eq!(log.dropped.load(SeqCst), 0);
        lanes.close(MACHINE, "ses_b");
        assert_eq!(forward_users(&lanes), None);
        wait_for("the tunnel's child to be reaped", || {
            (log.dropped.load(SeqCst) == 1).then_some(())
        })
        .await;
    }

    /// **Review finding 2.** Once a subscription has connected the adapter
    /// retries transport failures internally and forever, so its receiver never
    /// closes and the pump never came back round to `ensure`. A tunnel that died
    /// under an established lane was unrecoverable without a close and a reopen.
    ///
    /// The rule the fix pins, and what this asserts: **`ensures == resets + 1`,
    /// always**. `open` starts the tunnel (+1) and the pump ensures once before
    /// it subscribes, which covers the first generation; every generation AFTER
    /// that is a re-dial the adapter announces with a `Reset`, and each one
    /// re-`ensure`s exactly once. More than that would be a poll; fewer is the
    /// bug.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_established_lane_re_ensures_its_tunnel_on_a_reconnect() {
        let fake = one_session("ses_a").await;
        let (lanes, log, events) = lanes_for(&fake, &["ses_a"]);

        lanes.open(MACHINE, "ses_a").await.expect("the lane opens");
        wait_for("the first generation to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;
        assert_eq!(
            (events.count("reset"), log.ensures.load(SeqCst)),
            (1, 2),
            "a healthy lane ensures its tunnel once on open and once per generation"
        );

        // The ssh child dies under an established lane. Nothing about the
        // ADAPTER's connection has to change for this to be unrecoverable: it
        // is the pump that has to notice.
        log.alive.store(false, SeqCst);
        fake.close_streams();

        wait_for("the reconnect to re-ensure the tunnel", || {
            let resets = events.count("reset");
            (resets >= 2 && log.ensures.load(SeqCst) == resets + 1).then_some(())
        })
        .await;
        assert!(
            log.alive.load(SeqCst),
            "the pump never respawned the dead tunnel"
        );
        // …and the lane really came back, with no close and no reopen.
        wait_for("the reseeded generation to swap in", || {
            (generation(&lanes, "ses_a") >= 2).then_some(())
        })
        .await;
        assert_eq!(log.built.load(SeqCst), 1, "recovery rebuilt the tunnel");
    }

    /// **Review finding 5.** `SshForward`'s `Drop` kills and REAPS its child —
    /// a blocking `waitpid`. Which thread runs it is decided by whoever holds
    /// the last `Arc`, and that can be a cancelled pump future being dropped on
    /// an async worker, so moving one reference onto a blocking task did not
    /// settle the question. It is settled here instead: the wrapper's `Drop`
    /// hands the forward to a plain OS thread.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_tunnels_child_is_never_reaped_on_an_async_worker() {
        assert!(
            tokio::runtime::Handle::try_current().is_ok(),
            "the probe is vacuous unless this cell itself runs in a runtime"
        );
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);

        lanes.open(MACHINE, "ses_a").await.expect("the lane opens");
        wait_for("the lane to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;

        lanes.close(MACHINE, "ses_a");
        wait_for("the tunnel's child to be reaped", || {
            (log.dropped.load(SeqCst) == 1).then_some(())
        })
        .await;
        assert_eq!(
            log.dropped_on_runtime.load(SeqCst),
            0,
            "the blocking kill/waitpid ran on a runtime thread"
        );
    }
}
