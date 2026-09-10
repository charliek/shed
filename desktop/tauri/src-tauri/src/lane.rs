//! **Agent lanes in the desktop app** — the client half of
//! [`shed_core::lane`] (plan 015 §3.4, plan 017 §3.5).
//!
//! A machine row whose roost tab reported an agent's control surface carries an
//! `agent_lane` stamp ([`crate::machines::machine_row`], derived by
//! [`shed_core::roost::RoostSession::agent_lane`]). This module is what that
//! stamp makes possible: open a live transcript for one agent session, send it a
//! prompt, cancel its turn, and answer the permissions and questions it is
//! blocked on.
//!
//! ```text
//! machine row  --agent_lane{kind, session_id, server_url}-->  Lanes::open
//!                                                          |
//!            ReachKind::Local   -> dial server_url          |
//!            ReachKind::Ssh(e)  -> SshForward::reserve_for  |
//!                                                          v
//!                              match stamp.kind {  "opencode" => OpencodeClient,
//!                                                  "gx"       => GxClient,
//!                                                  other      => UnsupportedLane }
//!                                                          |
//!                                              Arc<dyn AgentLane>
//!                                                          |
//!                                       subscribe() -> Reset … Ready … frames
//!                                                          |
//!                                     LaneView (staged, then swapped) + `lane-event`
//! ```
//!
//! # One trait, two adapters — and the line the dispatch draws
//!
//! [`LaneEntry`] holds an `Arc<dyn AgentLane>`, and the ONLY place in this app
//! that names a concrete client type is the `match` in [`Lanes::open`]. That is
//! the whole point of plan 017: everything below the match — the pump, the view,
//! the tunnel bookkeeping, the six IPC verbs — is written against the contract,
//! so the third adapter is a `match` arm rather than a refactor.
//!
//! An entry is keyed and evicted by the FULL stamp, `(kind, server_url)`. A tab
//! that restarts as a different agent on the same loopback port is a different
//! lane, and an entry compared on the URL alone would survive it and keep
//! pumping the wrong adapter at it.
//!
//! A `kind` this build has no adapter for is [`LaneFailure::UnsupportedLane`] —
//! refused BY NAME rather than silently rendered as no lane at all, because the
//! stamp's own doc makes that the client's obligation: shed-core may learn to
//! stamp a kind before a given desktop binary learns to speak it.
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
//! # Credentials — the client's job, deliberately
//!
//! The contract has no seat for a credential (plan 017 §3.1 #5): roost carries
//! only WHERE an agent is, never how to be let in. So an adapter that needs one
//! takes it at construction from a source the client supplies, and this module
//! is that client.
//!
//! * **opencode** needs none. A password-protected server answers 401, which
//!   surfaces as [`LaneError::Unauthorized`] and a status-only panel.
//!   [`shed_opencode::BasicAuth`] exists for the follow-up that adds a config
//!   field; nothing here can supply one, and no test claims otherwise.
//! * **gx** needs a bearer on every route but `healthz`, and both the token and
//!   the discovery record live on the host that RUNS gx. [`TauriGxCredentials`]
//!   reads them the way that host allows: directly off the filesystem when the
//!   machine is local, and over the reach with
//!   [`shed_gx::PROBE_SCRIPT`] when it is not. The token is a
//!   [`shed_gx::GxToken`] from the moment it exists, so nothing here can print
//!   it; see that type and [`TauriGxCredentials`]'s own doc for the rules.
//!
//! # Transport repair, and why gx does not need a `Reset` for it
//!
//! [`Lanes::spawn_pump`] re-`ensure`s the forward on every `Reset` after the
//! first, which is how a dead `ssh -N` child under an ESTABLISHED opencode lane
//! gets respawned — the adapter announces each reconnect attempt with a `Reset`,
//! and that announcement is the cadence.
//!
//! gx reconnects SILENTLY when its cursor resume is accepted: no `Reset`, same
//! generation, the panel untouched (plan 017 §3.1 #1). There is therefore no
//! frame for the pump to hang a repair on, and inventing one would defeat the
//! feature. Instead the repair rides [`shed_gx::GxTransport`]:
//! [`TauriTransport::dial`] re-`ensure`s the forward and answers the local end,
//! and `GxClient` calls it before every connect. The pump's `Reset` handler
//! stays exactly as it was — harmless for gx (a reseed ensures twice), essential
//! for opencode.

use std::collections::{BTreeMap, HashMap};
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::Duration;

use serde_json::{json, Value};
use tauri::{AppHandle, Emitter};

use shed_app::lane_view::LaneView;
use shed_app::machine::{MachineForward, SshForward};
use shed_core::config::MachineEntry;
use shed_core::lane::{
    AgentLane, LaneAnswer, LaneCapabilities, LaneDecision, LaneError, LaneEvent, LaneSession,
    SendMode,
};
use shed_core::roost::AgentLaneStamp;
use shed_gx::{FixedDial, GxClient, GxCredentialSource, GxDiscovery, GxTimings, GxTransport};
use shed_opencode::OpencodeClient;

use crate::machines::{Machines, ReachKind};

/// The Tauri event every lane frame reaches the UI on:
/// `{machine, session_id, event}`, `event` being a serialized
/// [`LaneEvent`].
///
/// Kebab-case like every other event this app emits (`refresh`,
/// `show-launch`, `prefs-changed`).
pub const LANE_EVENT: &str = "lane-event";

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

/// The `agent_lane` kinds THIS BUILD has an adapter for.
///
/// One list, two readers: [`Lanes::open`]'s pre-reserve guard and
/// [`LaneFailure::message`]'s refusal. The `match` in `Lanes::open` is
/// deliberately NOT a third — it is where a kind binds to a constructor, which
/// is the one place a concrete client type may be named — but the guard and the
/// human message are the same fact twice, and the message is the copy that rots
/// silently: a third adapter that forgot it would go on claiming this build
/// speaks two.
const LANE_KINDS: [&str; 2] = ["opencode", "gx"];

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
    /// The row DOES carry a lane, and this build has no adapter for its kind.
    ///
    /// Distinct from [`LaneFailure::NoLane`] on purpose, and the distinction is
    /// the point: `no_lane` means "there is nothing to open here", which is a
    /// permanent property of the row, and a client renders no affordance for it.
    /// `unsupported_lane` means "there IS something here and I cannot speak to
    /// it" — a client can say so, and a NEWER build of this app may well be able
    /// to. [`shed_core::roost::AgentLaneStamp`]'s doc makes refusing by name the
    /// client's obligation for exactly this reason: shed-core can learn to stamp
    /// a kind before every binary that reads the stamp learns to speak it.
    ///
    /// Carries the kind verbatim, because the whole value of the variant is
    /// naming what was refused.
    UnsupportedLane(String),
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
            LaneFailure::UnsupportedLane(_) => "unsupported_lane",
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
            LaneFailure::UnsupportedLane(kind) => format!(
                "this build has no adapter for agent lanes of kind {kind:?} \
                 (it speaks {})",
                LANE_KINDS.join(" and ")
            ),
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
    /// Agent session id → its [`AgentLaneStamp`], for every lane this machine
    /// exposes. The WHOLE stamp: `kind` picks the adapter, `server_url` is the
    /// reported URL, and the pair of them is the eviction key.
    fn agent_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp>;

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
    fn agent_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp> {
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

    /// [`MachineForward::looks_alive`] — the cheap check
    /// [`TauriTransport::dial`] gates on.
    fn looks_alive(&self) -> bool {
        self.get().looks_alive()
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
    /// The stamp this open is building against. `reconcile` compares it to the
    /// fresh snapshot by exactly the same rule it judges a committed entry by: a
    /// row that now reports a different port — or a different AGENT on the same
    /// port — makes this open obsolete before it ever commits.
    stamp: AgentLaneStamp,
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
    /// The stamp this entry was opened against — the eviction key for a
    /// restarted tab. A new port means a different server, not a reconnect; and
    /// a new KIND on the same port means a different agent, which is the half
    /// the URL alone used to miss.
    stamp: AgentLaneStamp,
    /// This entry's share of the tunnel it rides, if any. `None` for a local
    /// lane. Taken by [`LaneEntry::retire`] rather than waited for: an op
    /// holding a clone of the `Arc<LaneEntry>` across an await must not be able
    /// to delay a closed panel's `ssh` child from dying.
    forward: Mutex<Option<ForwardShare>>,
    /// What `lane.open` answered with, cached so a second `open` is genuinely
    /// idempotent rather than a second round trip.
    session: LaneSession,
    capabilities: LaneCapabilities,
    /// The adapter, as the CONTRACT. Every verb below reaches the agent through
    /// this trait object; the concrete type was chosen once, in
    /// [`Lanes::open`]'s match, and is deliberately not knowable from here.
    client: Arc<dyn AgentLane>,
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

/// What the gx adapter needs from the process environment, in one value.
///
/// A struct rather than two arguments so a third gx knob does not change every
/// construction site, and `Default` so a test that is not about gx says nothing
/// about it.
#[derive(Debug, Clone, Default)]
pub struct GxConfig {
    /// [`crate::env::Env::gx_home`] — the RESOLVED directory the LOCAL reader
    /// looks in. Which of the three candidates won is `env.rs`'s decision and
    /// only `env.rs`'s: this layer reads no environment of its own, which is
    /// what keeps the test-mode gate in one place.
    ///
    /// `Default` is the empty path, which no reader can find a record under —
    /// the right answer for a test that is not about gx, and a loud one for a
    /// test that is and forgot to say so.
    pub home: PathBuf,
    /// [`crate::env::Env::gx_timings`] — the adapter's windows, shrunk by the
    /// harness so a cell does not wait out a thirty-second stall.
    pub timings: GxTimings,
}

/// The gx discovery cache: `(machine, reported_url)` → what the last successful
/// read found there. See [`TauriGxCredentials`] for the rule that governs it.
type GxCache = Arc<Mutex<HashMap<(String, String), GxDiscovery>>>;

/// How many `(machine, reported_url)` pairs [`GxCache`] keeps before it is
/// cleared wholesale.
///
/// Bounded for two reasons. It is a map that would otherwise only grow — the
/// [`Lanes::gates`] lesson — and, unlike the gates map, every value in it is a
/// BEARER TOKEN. Keys turn over whenever a gx leader restarts onto a new
/// ephemeral port, so a long-lived app would otherwise accumulate the tokens of
/// every leader it had ever seen. Sixteen is far more than the number of gx
/// lanes a person has open and small enough that the tokens do not linger.
const MAX_GX_CACHE: usize = 16;

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
    /// The gx adapter's environment — see [`GxConfig`].
    gx: GxConfig,
    /// Discovery, cached across opens. See [`TauriGxCredentials`].
    gx_cache: GxCache,
}

impl Lanes {
    pub fn new(
        handle: tokio::runtime::Handle,
        app: AppHandle,
        machines: Arc<Machines>,
        gx: GxConfig,
    ) -> Lanes {
        let sink: EventSink =
            Arc::new(move |machine: &str, session_id: &str, event: &LaneEvent| {
                let _ = app.emit(
                    LANE_EVENT,
                    json!({ "machine": machine, "session_id": session_id, "event": event }),
                );
            });
        Lanes::with_sink(handle, sink, machines, gx)
    }

    fn with_sink(
        handle: tokio::runtime::Handle,
        sink: EventSink,
        machines: Arc<dyn LaneMachines>,
        gx: GxConfig,
    ) -> Lanes {
        Lanes {
            handle,
            sink,
            machines,
            inner: Arc::new(Mutex::new(Inner::default())),
            gates: Arc::new(Mutex::new(Gates::new())),
            gx,
            gx_cache: Arc::new(Mutex::new(HashMap::new())),
        }
    }

    /// `lane.open` — ensure the transport, build the client, start the
    /// subscription, and answer with the session row plus what this adapter can
    /// do.
    ///
    /// **Idempotent.** A second call for a key that is already open re-answers
    /// from the entry; it does not open a second subscription. A call for a key
    /// whose row now reports a DIFFERENT stamp — another port, or another agent
    /// on the same port — evicts the stale entry and opens against the new one:
    /// that is a restarted tab, not a reconnect.
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
        self.lane_stamp(machine, session_id)?;
        let gate = self.gate(&key);
        let _serialized = gate.lock().await;

        // Re-resolved under the gate, and this is the value everything below
        // uses: the pre-gate one was read before waiting, and waiting is exactly
        // when a tab restarts onto a new port or goes away. Using it would open
        // a lane against a socket the snapshot has already retired.
        let stamp = self.lane_stamp(machine, session_id)?;
        if let Some(entry) = self.entry(&key) {
            if entry.stamp == stamp {
                return Ok(entry.opened());
            }
            self.evict(&key);
        }

        // Refused BEFORE anything is reserved. A kind with no adapter is a
        // permanent property of the row, so there is no reason to spend an ssh
        // child and a readiness poll discovering it.
        if !LANE_KINDS.contains(&stamp.kind.as_str()) {
            return Err(LaneFailure::UnsupportedLane(stamp.kind.clone()));
        }

        // Declared BEFORE the first await, so a `close` or a `reconcile` landing
        // anywhere below has something to cancel.
        let _pending = self.declare(&key, &stamp);

        let reach = self
            .machines
            .reach_kind(machine)
            .map_err(|e| LaneFailure::Lane(LaneError::Unavailable(e)))?;
        // Reserved, not merely created: from here every `?` gives the share back
        // (and with it the `ssh` child, if this open was its only user).
        let (base_url, forward) = self.transport(machine, &reach, &stamp.server_url).await?;

        // **The one place this app names a concrete adapter.** Everything after
        // it is written against `dyn AgentLane`; see the module doc.
        let client: Arc<dyn AgentLane> = match stamp.kind.as_str() {
            "opencode" => {
                let url = reqwest::Url::parse(&base_url).map_err(|e| {
                    LaneFailure::Lane(LaneError::BadRequest(format!(
                        "the reported agent server {:?} is not a usable URL: {e}",
                        stamp.server_url
                    )))
                })?;
                // No credential source — see the module doc.
                Arc::new(OpencodeClient::new(url, None)?)
            }
            "gx" => {
                // The REPORTED url goes to the client (it is what a discovery
                // record is matched against); the DIAL url is the transport's
                // business, resolved fresh before every connect.
                //
                // Local is shed-gx's own `FixedDial` — its loopback is ours, so
                // there is nothing to ensure. Forwarded is the one that has to
                // re-`ensure` a tunnel, which is all `TauriTransport` is for.
                let transport: Arc<dyn GxTransport> = match forward.as_ref() {
                    None => Arc::new(FixedDial::parse(&base_url).map_err(LaneFailure::Lane)?),
                    Some(share) => Arc::new(TauriTransport::new(share)),
                };
                let credentials = TauriGxCredentials::new(
                    machine,
                    &stamp.server_url,
                    &reach,
                    &self.gx,
                    Arc::clone(&self.gx_cache),
                );
                Arc::new(GxClient::new(
                    stamp.server_url.clone(),
                    transport,
                    Arc::new(credentials),
                    self.gx.timings.clone(),
                )?)
            }
            // Unreachable: the guard above ran before anything was reserved.
            // Restated rather than `unreachable!()` so that adding a kind to one
            // list and forgetting the other is a refusal, not a panic.
            other => return Err(LaneFailure::UnsupportedLane(other.to_string())),
        };
        // The roster row is fetched BEFORE the subscription starts: a 404 here
        // is an honest `unknown_session` the caller can render, where the same
        // failure inside the pump would be a `Down` the panel has to wait for.
        // It is also the last await, and the one that fails on a
        // password-protected agent (and, on gx, the one that discovers and pins
        // the credential) — hence the share above.
        let session = match client.session(session_id).await {
            Ok(session) => session,
            Err(e) => {
                // A credential the cache handed out and the agent then refused
                // must not be handed out again — otherwise a rotated token
                // wedges every future open on this lane. See
                // [`TauriGxCredentials`].
                if matches!(e, LaneError::Unauthorized) {
                    self.forget_gx_credentials(machine, &stamp.server_url);
                }
                return Err(e.into());
            }
        };
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
            stamp,
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

    /// `lane.messages` — the staged-then-swapped view, as this app's IPC
    /// payload.
    ///
    /// The fold and the projection are [`shed_app::lane_view`]'s; the only thing
    /// that belongs here is the envelope's SHAPE, which is Tauri's and not a
    /// client-neutral API (the phone converts the same
    /// [`shed_app::lane_view::LaneViewSnapshot`] into its own DTOs).
    pub fn messages(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        let snap = lock(&entry.view).snapshot(None);
        Ok(json!({
            "messages": snap.messages,
            "activity": snap.activity,
            "generation": snap.generation,
            "stale": snap.stale,
        }))
    }

    /// `lane.approvals` — what is blocking on the human, this session's and its
    /// descendants'.
    ///
    /// Pending only, oldest first: the snapshot already filtered and sorted
    /// them ([`shed_app::lane_view::LaneView::snapshot`] owns that rule), so
    /// this is the envelope and nothing else.
    pub fn approvals(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, session_id)?;
        let snap = lock(&entry.view).snapshot(None);
        Ok(json!({ "approvals": snap.approvals }))
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
    /// every entry whose tab is gone or whose STAMP moved.
    ///
    /// See [`crate::machines::OnLanes`] for why the snapshot is the signal.
    pub fn reconcile(&self, machine: &str, lanes: &BTreeMap<String, AgentLaneStamp>) {
        let gone: Vec<Arc<LaneEntry>> = {
            let mut inner = lock(&self.inner);
            // Opens still in flight are judged by the SAME rule as committed
            // entries. Without this a tab that went away mid-open would be
            // resurrected by the open that was already past the check.
            for (key, pending) in inner.pending.iter_mut() {
                if key.0 == machine && lanes.get(&key.1) != Some(&pending.stamp) {
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
                    .is_some_and(|stamp| stamp == &entry.stamp);
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
    fn declare(&self, key: &Key, stamp: &AgentLaneStamp) -> PendingGuard {
        lock(&self.inner).pending.insert(
            key.clone(),
            Pending {
                stamp: stamp.clone(),
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

    /// Drop the cached gx credential for a lane, so the next open re-reads it.
    ///
    /// Called when the agent REFUSED what the cache handed out — see
    /// [`TauriGxCredentials`]. A no-op for a kind with no cache entry
    /// (opencode), and for a key that was a miss anyway, which is why it is not
    /// gated on the stamp's kind: "forget any credential we cached for this
    /// lane" is true and cheap to say for every adapter.
    fn forget_gx_credentials(&self, machine: &str, reported_url: &str) {
        lock(&self.gx_cache).remove(&(machine.to_string(), reported_url.to_string()));
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
        match self.lane_stamp(machine, session_id) {
            Err(e) => Err(e),
            Ok(_) => Err(LaneFailure::NoLane(format!(
                "no lane is open for session {session_id:?} on machine {machine:?} — \
                 call lane.open first"
            ))),
        }
    }

    /// The [`AgentLaneStamp`] this row reports, or `no_lane`.
    fn lane_stamp(&self, machine: &str, session_id: &str) -> Result<AgentLaneStamp, LaneFailure> {
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
        // `reach`, not `kind`: since plan 017 a lane has TWO kinds, and the one
        // that decides the transport is the machine's reach, not the row's
        // adapter token.
        reach: &ReachKind,
        server_url: &str,
    ) -> Result<(String, Option<ForwardShare>), LaneFailure> {
        let entry = match reach {
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
    ///
    /// **Look and build under ONE acquisition.** This used to look first,
    /// release the lock to build a candidate, and insert-if-absent — so two
    /// opens for two sessions on ONE agent server could both look into an empty
    /// map and both call [`LaneMachines::forward`]. Only the winner's was ever
    /// registered or `ensure`d, so the loser's was a reservation thrown away
    /// rather than a second `ssh -N` child; but "one server, one tunnel" then
    /// held by interleaving rather than by construction, and the next thing to
    /// grow inside `forward()` would have made the difference matter.
    ///
    /// The lock is a plain `Mutex` and this is a synchronous fn, so nothing is
    /// held across an await. `forward()` RESERVES (see its doc): it binds
    /// `127.0.0.1:0`, reads the assignment back and closes — no spawn, no
    /// network, no path back into this map. Holding the map across that is
    /// cheaper than the discarded reservations were.
    fn reserve(&self, key: ForwardKey, entry: &MachineEntry) -> Result<ForwardShare, LaneFailure> {
        let mut inner = lock(&self.inner);
        let slot = match inner.forwards.entry(key.clone()) {
            std::collections::hash_map::Entry::Occupied(slot) => slot.into_mut(),
            std::collections::hash_map::Entry::Vacant(vacant) => vacant.insert(ForwardSlot {
                forward: Arc::new(OwnedForward::new(
                    self.machines
                        .forward(entry, key.1)
                        .map_err(LaneError::Unavailable)?,
                )),
                users: 0,
            }),
        };
        slot.users += 1;
        Ok(ForwardShare {
            inner: Arc::clone(&self.inner),
            key,
            forward: Arc::clone(&slot.forward),
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
    /// **gx does not depend on the second of those for the common case, and must
    /// not have to.** Its bounded silent resume emits no `Reset` at all, so a
    /// dead `ssh` child is caught by [`TauriTransport::dial`] instead — see the
    /// module doc.
    ///
    /// It is still load-bearing for gx, though, and for the one case `dial`
    /// cannot see: a child that is ALIVE but no longer listening. `dial`'s cheap
    /// check calls that healthy, so requests fail, the watcher exhausts its
    /// silent resumes and reseeds — and the reseed's `Reset` lands here, where
    /// the full `ensure` (port probe included) repairs it. The two mechanisms
    /// are the fast path and the backstop, and this loop needs no knowledge of
    /// which adapter it is pumping to run either.
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
        client: Arc<dyn AgentLane>,
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

// ---------------------------------------------------------------------------
// gx: the transport hook and the credential source
// ---------------------------------------------------------------------------

/// **The desktop's [`GxTransport`] for a FORWARDED lane** — re-`ensure` the
/// tunnel, answer its local end.
///
/// gx calls this before every connect, which is what lets a forward be repaired
/// without the contract growing an event for it (module doc, "Transport repair").
///
/// # It is called per REQUEST, so the healthy path has to be free
///
/// [`shed_gx::GxTransport`]'s own doc states the requirement: *"The desktop's is
/// a cache read behind a lock when nothing is wrong … a hook that did real work
/// per call would make every verb pay for a tunnel that is fine."* So this gates
/// on [`MachineForward::looks_alive`] — a `try_wait` on the `ssh` child, no
/// network — and only calls [`MachineForward::ensure`] when that says the child
/// is gone. Calling `ensure` unconditionally meant a blocking loopback connect,
/// under a lock, before every gx verb; on a reconnect ladder with a 100 ms floor
/// that is about ten of them a second per down lane.
///
/// The port is read AFTER any ensure, not cached at construction: `ensure` is
/// what makes the port mean something, and reading it afterwards is what keeps
/// this correct if a forward ever re-reserves.
///
/// **The residual, and what covers it.** `looks_alive` is a proxy: a child that
/// is alive but has stopped listening reads as healthy, so this returns a URL
/// that will not answer. `ExitOnForwardFailure=yes` makes that a state `ssh`
/// does not reach on its own (a broken forward takes the child with it), and
/// when it does happen the requests fail, the watcher exhausts its silent
/// resumes and reseeds, and the reseed's `Reset` drives
/// [`Lanes::spawn_pump`]'s re-`ensure` — the authoritative check, port probe and
/// all. That is why the pump's `Reset` handler is load-bearing for gx too, not
/// merely harmless.
///
/// **A LOCAL lane does not use this type at all** — it is [`shed_gx::FixedDial`],
/// which that crate wrote for exactly this case ("a lane on this machine") and
/// already re-exports. Its loopback is ours: there is nothing to ensure and
/// nothing to move, so a second hand-rolled "hold one parsed URL and answer it"
/// here would only be a copy that can drift.
///
/// The handle is **weak**, exactly like the pump's. A transport is owned by the
/// `GxClient`, which is owned by the [`LaneEntry`] — so a strong reference would
/// keep an `ssh` child alive past the eviction that took the entry's share away,
/// which is the one thing [`LaneEntry::retire`] exists to prevent. A failed
/// upgrade is how a dial learns its lane is gone, and it reads as `Unavailable`
/// like every other "this transcript is not live".
struct TauriTransport(Weak<OwnedForward>);

impl TauriTransport {
    fn new(share: &ForwardShare) -> TauriTransport {
        TauriTransport(share.weak())
    }
}

#[async_trait::async_trait]
impl GxTransport for TauriTransport {
    async fn dial(&self) -> Result<reqwest::Url, LaneError> {
        let forward = self
            .0
            .upgrade()
            .ok_or_else(|| LaneError::Unavailable("this lane's tunnel was released".to_string()))?;
        // The cheap check first; `ensure` only when it says there is something
        // to fix. See the type doc.
        if !forward.looks_alive() {
            forward.ensure().await.map_err(LaneError::Unavailable)?;
        }
        let port = forward.port();
        reqwest::Url::parse(&format!("http://127.0.0.1:{port}/"))
            .map_err(|e| LaneError::Unavailable(format!("the tunnel's local url: {e}")))
    }
}

/// How the SSH half of gx discovery runs its probe.
///
/// A boxed async closure rather than a direct call to
/// [`shed_app::machine::exec`], so the ONE rule this path has — **`exec`'s error
/// string is never forwarded** — can be asserted against an error that really
/// does carry a token, without an sshd and without a real machine.
type ProbeFuture =
    std::pin::Pin<Box<dyn std::future::Future<Output = Result<String, String>> + Send>>;
/// See [`ProbeFuture`].
type ProbeRunner = Arc<dyn Fn() -> ProbeFuture + Send + Sync>;

/// The two ways to read gx's credentials, one per reach.
enum GxSource {
    /// `ReachKind::Local` — read the files. **Never shells out**: the reader is
    /// [`shed_gx::local_discovery`], which makes the same checks gx's own reader
    /// makes (regular file, mode `0600`, owned by us) and resolves the token's
    /// path from the RECORD rather than by guessing a filename.
    Local { home: PathBuf, uid: u32 },
    /// `ReachKind::Ssh` — run [`shed_gx::PROBE_SCRIPT`] over the reach and parse
    /// what it printed. The trust boundary on the far side is *the same UID on
    /// that host*, which is precisely the boundary roost's own socket has.
    Ssh(ProbeRunner),
}

/// **Where the gx lane's bearer token comes from**, and the app's implementation
/// of the seam the contract deliberately does not have (plan 017 §3.3).
///
/// # The caching rule, and why it is "once"
///
/// Discovery is cached per `(machine, reported_url)` in a map shared by every
/// lane ([`Lanes::gx_cache`]), because a probe over SSH is a whole round trip
/// and a machine's second open should not pay for it again.
///
/// But `GxClient::ensure_pinned` asks TWICE when it has to: it compares the
/// discovered `instanceId` against `healthz`, and on a mismatch it calls
/// `discover` once more before giving up. That second ask exists precisely
/// because the first answer was wrong — a leader restarted, and its
/// `instanceId` moved while its token did not — so answering it from the same
/// cache entry would turn a recoverable restart into a permanent `unavailable`.
///
/// So the rule is: **one source serves the cache at most once**, on its first
/// `discover`, and reads fresh every time after that (refreshing the shared
/// entry, which is what "cleared on a pin mismatch" means in practice). The
/// benefit the cache is for — a second lane on a machine that is already known —
/// is kept; the hazard it would create is not. The cost is that a RECONNECT
/// re-reads the record, which is the right posture anyway: the connection the
/// last credential was pinned on is the one that just broke.
///
/// # A refused credential is EVICTED, not merely bypassed
///
/// Serving once per source is not enough on its own, and the gap is not a
/// degraded state that heals — it is a permanent loop. The pin catches a moved
/// `instanceId`; nothing catches a moved TOKEN under a stable one. gx documents
/// that combination as impossible (one token per `$GROK_HOME`, and a leader
/// restart changes the instance, never the token), but if it happened: the cache
/// holds `(token A, instance I)`, the token rotates to B, `healthz` still says
/// I — so the pin SUCCEEDS on the stale token and the first bearer request 401s.
/// Every fresh `lane.open` builds a new source with `answered = false`, reads the
/// same cached A, and 401s again. For ever, until the app restarts.
///
/// So [`Lanes::open`] removes the entry when the agent refuses the credential
/// (`Unauthorized`), and the next open re-probes and picks up B. The mismatch
/// path needs no eviction of its own: `pin_epoch` asks a second time, this
/// source reads fresh for it, and `store` overwrites the entry on the way past.
///
/// **The residual that remains, deliberately:** two concurrent COLD opens on one
/// `(machine, url)` both miss and both probe, last writer winning. It is benign
/// and left alone — the key includes the machine and the URL, so neither
/// answer can be used against the wrong target, and both are reads of the same
/// file. Deduping reads in flight would buy one saved round trip in a race a
/// panel does not produce (it opens lanes sequentially) at the cost of a second
/// piece of shared state.
///
/// # What never leaves
///
/// The token is a [`shed_gx::GxToken`] from the moment it is parsed, so it has
/// no `Display`, no `Serialize`, and a `Debug` that prints `<redacted>`. On top
/// of that this type never forwards a probe FAILURE: `shed_app::machine::exec`
/// builds its error string from the remote's **stdout** when stderr is empty,
/// and the remote's stdout is one `cat` away from being the token. The raw error
/// goes to `tracing::debug` and the caller gets a fixed sentence.
struct TauriGxCredentials {
    /// For the message and the cache key. The reported URL is the other half.
    machine: String,
    reported_url: String,
    source: GxSource,
    cache: GxCache,
    /// Whether this source has answered at all yet. See the caching rule above.
    answered: std::sync::atomic::AtomicBool,
}

impl TauriGxCredentials {
    fn new(
        machine: &str,
        reported_url: &str,
        reach: &ReachKind,
        gx: &GxConfig,
        cache: GxCache,
    ) -> TauriGxCredentials {
        let source = match reach {
            ReachKind::Local => GxSource::Local {
                // ALREADY resolved, in `env.rs`, because whether a var is
                // honoured at all is a test-mode question and that is where
                // every other one of those is decided. This function reads no
                // environment: reading `GROK_HOME` (or `HOME`) here is what let
                // an inherited value reach a hermetic run — `$HOME` is
                // redirected to the harness runtime dir, but nothing clears
                // `$GROK_HOME`, so a developer who exports it would have had the
                // app read their real token.
                home: gx.home.clone(),
                uid: crate::env::current_uid(),
            },
            ReachKind::Ssh(entry) => GxSource::Ssh(probe_over_ssh(entry.clone())),
        };
        TauriGxCredentials {
            machine: machine.to_string(),
            reported_url: reported_url.to_string(),
            source,
            cache,
            answered: std::sync::atomic::AtomicBool::new(false),
        }
    }

    fn key(&self) -> (String, String) {
        (self.machine.clone(), self.reported_url.clone())
    }

    /// The cached entry, if this source has not answered yet. See the type doc.
    fn cached(&self) -> Option<GxDiscovery> {
        if self
            .answered
            .swap(true, std::sync::atomic::Ordering::SeqCst)
        {
            return None;
        }
        lock(&self.cache).get(&self.key()).cloned()
    }

    fn store(&self, discovery: &GxDiscovery) {
        // Built once, and BEFORE the lock: the key is two owned `String`s, and
        // there is no reason to allocate them (twice) with the cache held.
        let key = self.key();
        let mut cache = lock(&self.cache);
        // Wholesale rather than LRU: the map is a cache of a cheap-to-rebuild
        // fact, and the thing being bounded is how many bearer tokens this
        // process is holding. See [`MAX_GX_CACHE`].
        if cache.len() >= MAX_GX_CACHE && !cache.contains_key(&key) {
            cache.clear();
        }
        cache.insert(key, discovery.clone());
    }

    /// Read the credential for real — files locally, the probe over ssh.
    async fn read(&self) -> Result<GxDiscovery, LaneError> {
        match &self.source {
            GxSource::Local { home, uid } => {
                shed_gx::local_discovery(home, &self.reported_url, *uid)
            }
            GxSource::Ssh(run) => self.read_over_ssh(run).await,
        }
    }

    async fn read_over_ssh(&self, run: &ProbeRunner) -> Result<GxDiscovery, LaneError> {
        let machine = &self.machine;
        let stdout = match run().await {
            Ok(stdout) => stdout,
            Err(raw) => {
                // NOT forwarded — see the type doc. `raw` can contain the
                // remote's stdout, and the remote's stdout is where the token
                // is. TWO independent defences, because one of them is a
                // property of the whole tree rather than of this line: the
                // caller gets a fixed sentence, and what reaches `tracing` goes
                // through `redact_hex64` first. No subscriber is installed
                // anywhere in this repo today, so the macro is a no-op — but
                // that is somebody else's decision to change, and the day it
                // changes an un-redacted token would land in the app log, which
                // is exactly what the harness greps.
                tracing::debug!(
                    machine = %machine,
                    error = %shed_gx::redact_hex64(&raw),
                    "the gx discovery probe failed"
                );
                return Err(LaneError::Unavailable(format!(
                    "gx discovery failed on {machine}"
                )));
            }
        };
        // `ProbeError`'s variants are fixed strings by construction (its own doc
        // is explicit that it never carries probe output), so THIS one is safe
        // to show: it is the difference between "the probe never ran" and "the
        // token file is not eligible", which is the whole of what a user can act
        // on.
        let probe = shed_gx::parse_probe(&stdout)
            .map_err(|e| LaneError::Unavailable(format!("gx discovery on {machine}: {e}")))?;
        if probe.unreadable_records > 0 {
            tracing::debug!(
                machine = %machine,
                unreadable = probe.unreadable_records,
                "some gx discovery records did not parse"
            );
        }
        // The FIRST record naming this URL wins, which is `local_discovery`'s
        // rule restated so the two readers agree about WHICH RECORD: a URL is a
        // port, two leaders cannot bind one, so a second record for it is stale
        // — and the client's `instanceId` pin is what catches a wrong pick
        // anyway.
        //
        // **They agree about the record and NOT about the token's path, and
        // that asymmetry is deliberate.** `local_discovery` resolves the token
        // from the record's own `tokenFile` (through `token_path_for`, which
        // bounds it inside `$GROK_HOME`); `PROBE_SCRIPT` always reads
        // `$GROK_HOME/gx-remote.token` and ignores the field. It is invisible
        // today because gx writes exactly that path — verified against two
        // independent leaders, one of them on a non-default socket whose RECORD
        // filename is suffixed while its `tokenFile` is not. If gx ever starts
        // writing a non-default `tokenFile`, the local reader follows it and
        // this one silently keeps reading the default, so **the probe has to
        // change with it**; the mismatch would show up as a lane that works
        // locally and answers `unavailable` over SSH.
        //
        // The probe is NOT widened to follow `tokenFile` on purpose. A remote
        // reader that took a path out of a file it just read on the far side
        // could be pointed at any readable file by whatever wrote that record;
        // `token_path_for`'s `$GROK_HOME` containment is what makes that safe
        // locally, and re-implementing containment in POSIX `sh` across an SSH
        // boundary is not a trade worth making for a field that is always the
        // default. A remote reader that cannot be redirected by record contents
        // is the stronger property.
        let record = shed_gx::records_for(&probe.records, &self.reported_url)
            .into_iter()
            .next()
            .ok_or_else(|| {
                LaneError::Unavailable(format!(
                    "no gx discovery record for {} on {machine}",
                    self.reported_url
                ))
            })?;
        let token = probe.token.ok_or_else(|| {
            LaneError::Unavailable(format!("the gx token could not be read on {machine}"))
        })?;
        Ok(GxDiscovery {
            token,
            instance_id: record.instance_id.clone(),
        })
    }
}

/// The production [`ProbeRunner`]: one `sh -c <PROBE_SCRIPT>` over the machine's
/// reach.
///
/// The argv is pinned by `tests/machine-transport`'s `gx-probe` scenario — SSH
/// has no argv API, so this multi-line script crosses as ONE re-parsed string
/// and every transport that composes it (here, and shed-mobile's Dart one) has
/// to compose it identically.
fn probe_over_ssh(entry: MachineEntry) -> ProbeRunner {
    Arc::new(move || {
        let entry = entry.clone();
        Box::pin(async move {
            let argv = [
                "sh".to_string(),
                "-c".to_string(),
                shed_gx::PROBE_SCRIPT.to_string(),
            ];
            shed_app::machine::exec(&entry, &argv).await
        })
    })
}

#[async_trait::async_trait]
impl GxCredentialSource for TauriGxCredentials {
    async fn discover(&self, _reported_url: &str) -> Result<GxDiscovery, LaneError> {
        // The argument is ignored on purpose: this source was BUILT for one
        // reported URL (it is half of its cache key), and a client that asked it
        // about another would be asking the wrong source.
        if let Some(hit) = self.cached() {
            return Ok(hit);
        }
        let fresh = self.read().await?;
        self.store(&fresh);
        Ok(fresh)
    }
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
/// Four forms, one per thing a client can actually do (plan 015 §3.4, plan 017
/// §3.5):
///
/// * `{"choice": "<option id>"}` — **the offered option with this id, exactly as
///   offered.** The form the panel uses, and the only one that can express a
///   real agent's real menu: gx offers FIVE permission options with TWO of kind
///   `allow_once`, so `{"permission": …}` cannot name which one the user
///   pressed. Ids are opaque (they are whatever the agent called them) and this
///   never parses one.
/// * `{"permission": "allow-once" | "allow-always" | "reject"}` — by SEMANTIC
///   kind, resolved by the adapter against the offered options' `kind` fields,
///   never by id. Kept because it is what a script can write without first
///   fetching the approval, and it is what every existing caller sends.
/// * `{"question": [["<option id>", …], …]}` — one inner list per question,
///   optionally beside `{"custom_text": ["<typed>", null, …]}` — the free text
///   per question, positional like `question`, `null` where nothing was typed.
///   `custom_text` is a MODIFIER of `question` and of nothing else: beside
///   `choice`, `permission` or `reject` it is a `bad_request`, because a client
///   that typed something and named the wrong form has to be told the text did
///   not travel. The adapter refuses text aimed at a question whose `custom` is
///   false (`shed_core::lane::normalize_question_answer`), so this door only
///   checks the shape.
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
    const FORMS: [&str; 4] = ["choice", "permission", "question", "reject"];
    let named: Vec<&str> = FORMS
        .into_iter()
        .filter(|form| value.get(form).is_some())
        .collect();
    match named.as_slice() {
        [_one] => {}
        [] => {
            return Err(LaneFailure::bad_request(
                "answer must be {choice: \"<option id>\"}, {permission: …}, \
                 {question: [[…]]} or {reject: true}"
                    .to_string(),
            ))
        }
        several => {
            return Err(LaneFailure::bad_request(format!(
                "an answer names exactly one of choice, permission, question or reject; \
                 this one names {}",
                several.join(" and ")
            )))
        }
    }
    // `custom_text` is not a FORM — it modifies exactly one of them. Checked
    // here, against the single form just established, so that every other arm
    // below can be written as if the key did not exist: a `custom_text` beside
    // `choice` is refused at this door rather than ignored on the way past it,
    // which is the difference between "your text did not travel" and a silent
    // drop of something a human typed.
    if value.get("custom_text").is_some() && named[0] != "question" {
        return Err(LaneFailure::bad_request(format!(
            "`custom_text` answers a question's free-text field; \
             it has no meaning beside `{}`",
            named[0]
        )));
    }

    if let Some(option_id) = value.get("choice") {
        let option_id = option_id
            .as_str()
            .ok_or_else(|| LaneFailure::bad_request("`choice` must be a string".to_string()))?;
        // **Not trimmed**, unlike `permission` below. That one names a value out
        // of a closed vocabulary this code owns, so tidying whitespace is safe.
        // An option id is OPAQUE — whatever the agent called it, and gx's own
        // fixtures prove ids and semantics are independent — so trimming would
        // be this layer silently rewriting a caller's identifier and then
        // matching it against an approval that never offered the rewritten form.
        // The only judgement made is that the empty string is not an id any
        // approval can have offered, which saves a round trip; anything else
        // goes through untouched and the adapter refuses what it does not
        // recognise.
        if option_id.is_empty() {
            return Err(LaneFailure::bad_request(
                "`choice` must name an option id offered by the approval".to_string(),
            ));
        }
        return Ok(LaneAnswer::Choice {
            option_id: option_id.to_string(),
        });
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
        // Absent is an empty vec, NOT a vec of nulls: the contract skips the
        // field when it is empty, so an answer with no free text stays exactly
        // the payload this build sent before the field existed.
        let custom_text: Vec<Option<String>> = match value.get("custom_text") {
            None => Vec::new(),
            Some(v) => serde_json::from_value(v.clone()).map_err(|e| {
                LaneFailure::bad_request(format!(
                    "`custom_text` must be a list of strings or nulls, one per question: {e}"
                ))
            })?,
        };
        return Ok(LaneAnswer::Question {
            answers,
            custom_text,
        });
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
        // The kind-dispatch refusal (plan 017 §3.5). A code of its own, and it
        // NAMES the kind — that is the whole reason it is not `no_lane`.
        let unsupported = LaneFailure::UnsupportedLane("claude".into());
        assert_eq!(unsupported.code(), "unsupported_lane");
        assert!(
            unsupported.message().contains("\"claude\""),
            "the refusal must name the kind it refused: {}",
            unsupported.message()
        );
    }

    #[test]
    fn the_answer_forms_are_the_four_the_plan_names() {
        // The form the panel sends: the offered option, BY ITS OWN ID. Ids are
        // opaque — a real gx permission offers five options with two of kind
        // `allow_once`, so nothing here may infer a semantic from the string.
        assert_eq!(
            parse_answer(&json!({"choice": "p-2"})).expect("choice"),
            LaneAnswer::Choice {
                option_id: "p-2".into()
            }
        );
        // An id is opaque, so it crosses VERBATIM — no trimming, no casing, no
        // interpretation. A layer that tidied it would be rewriting a caller's
        // identifier and then failing to match the approval that offered it.
        assert_eq!(
            parse_answer(&json!({"choice": "  p 2  "})).expect("an id is opaque"),
            LaneAnswer::Choice {
                option_id: "  p 2  ".into()
            }
        );
        // The one judgement: the empty string is not an id any approval offered.
        assert!(parse_answer(&json!({"choice": ""})).is_err());
        assert!(parse_answer(&json!({"choice": 3})).is_err());
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
                answers: vec![vec!["yes".into()], vec!["a".into(), "b".into()]],
                // Absent `custom_text` is EMPTY, not a vec of nulls — the
                // contract skips the field when it is empty, so this build's
                // ordinary answer is byte-identical to the previous build's.
                custom_text: vec![],
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

    /// **`custom_text` is a modifier of `question` and of nothing else**
    /// (plan 018 §3.2).
    ///
    /// It is not a fifth FORM — it does not resolve an approval on its own —
    /// so the form count is unchanged and this door only decides where the key
    /// is allowed to appear. Beside `choice`, `permission` or `reject` it is
    /// refused: a client that typed something and named the wrong form has to
    /// be told the text did not travel, and the alternative (ignoring the key)
    /// is a silent drop of the one part of the answer a human wrote by hand.
    #[test]
    fn custom_text_rides_beside_question_and_nowhere_else() {
        assert_eq!(
            parse_answer(&json!({
                "question": [["yes"], []],
                "custom_text": [null, "  a branch I typed  "],
            }))
            .expect("free text beside a question"),
            LaneAnswer::Question {
                answers: vec![vec!["yes".into()], vec![]],
                // NOT trimmed here: `normalize_question_answer` is the one
                // place that trims, so both adapters trim identically.
                custom_text: vec![None, Some("  a branch I typed  ".into())],
            }
        );
        // All-null is still a legal vector — the panel sends one entry per
        // question and the adapter reads them all as "nothing typed".
        assert_eq!(
            parse_answer(&json!({"question": [["yes"]], "custom_text": [null]}))
                .expect("all-null free text"),
            LaneAnswer::Question {
                answers: vec![vec!["yes".into()]],
                custom_text: vec![None],
            }
        );

        // Beside every other form: refused, and the refusal NAMES the form it
        // was found beside.
        for (payload, form) in [
            (json!({"choice": "p-2", "custom_text": ["typed"]}), "choice"),
            (
                json!({"permission": "allow-once", "custom_text": ["typed"]}),
                "permission",
            ),
            (json!({"reject": true, "custom_text": ["typed"]}), "reject"),
            // …including when it carries nothing: the KEY is the mistake, not
            // its contents, and a client that sends `[]` beside `choice` still
            // believes free text is a thing this form takes.
            (json!({"choice": "p-2", "custom_text": []}), "choice"),
            (json!({"choice": "p-2", "custom_text": [null]}), "choice"),
        ] {
            let err = parse_answer(&payload).expect_err("custom_text needs a question");
            assert_eq!(err.code(), "bad_request", "{payload}");
            assert!(
                err.message().contains(form) && err.message().contains("custom_text"),
                "the refusal names the form it was found beside: {}",
                err.message()
            );
        }

        // Shape, not just placement: `custom_text` is a list of strings-or-null.
        assert!(parse_answer(&json!({"question": [["yes"]], "custom_text": "typed"})).is_err());
        assert!(parse_answer(&json!({"question": [["yes"]], "custom_text": [3]})).is_err());
        assert!(parse_answer(&json!({"question": [["yes"]], "custom_text": {"0": "t"}})).is_err());

        // And it never becomes a form of its own: alone it is still "name a
        // form", because free text answers nothing by itself.
        assert_eq!(
            parse_answer(&json!({"custom_text": ["typed"]}))
                .expect_err("free text is not an answer")
                .code(),
            "bad_request"
        );
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
            // The fourth form counts too: `{choice}` and `{permission}` resolve
            // the same approval by two different rules (by id, by kind), and a
            // caller that sent both has to be told which one did not happen.
            json!({"choice": "p-1", "permission": "allow-once"}),
            json!({"choice": "p-1", "reject": true}),
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
        assert!(parse_answer(&json!({"choice": "p-1"})).is_ok());
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

        /// `alive` IS the fake's child: the real one answers this from a
        /// `try_wait`, and a cell kills it by clearing the flag.
        fn looks_alive(&self) -> bool {
            self.log.alive.load(SeqCst)
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
        lanes: Mutex<BTreeMap<String, AgentLaneStamp>>,
        port: u16,
        log: Arc<ForwardLog>,
        /// How this machine is reached. `Ssh` for every cell about tunnels;
        /// `Local` for the ones about the LOCAL credential reader, which is the
        /// path that reads files instead of shelling out.
        reach: ReachKind,
    }

    impl LaneMachines for FakeMachines {
        fn agent_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp> {
            if machine == MACHINE {
                lock(&self.lanes).clone()
            } else {
                BTreeMap::new()
            }
        }

        fn reach_kind(&self, _machine: &str) -> Result<ReachKind, String> {
            Ok(self.reach.clone())
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

    /// The rows a machine reports: every session on ONE server, all one kind.
    fn lane_rows_of(kind: &str, sessions: &[&str]) -> BTreeMap<String, AgentLaneStamp> {
        sessions
            .iter()
            .map(|s| {
                (
                    (*s).to_string(),
                    AgentLaneStamp {
                        kind: kind.to_string(),
                        session_id: (*s).to_string(),
                        server_url: format!("http://127.0.0.1:{REMOTE_PORT}/"),
                    },
                )
            })
            .collect()
    }

    /// [`lane_rows_of`] for the adapter most cells are about.
    fn lane_rows(sessions: &[&str]) -> BTreeMap<String, AgentLaneStamp> {
        lane_rows_of("opencode", sessions)
    }

    /// A `Lanes` on the two doubles: `MACHINE` is an SSH target exposing
    /// `sessions`, and its tunnels land on `fake`'s real port.
    fn lanes_for(
        fake: &FakeOpencode,
        sessions: &[&str],
    ) -> (Arc<Lanes>, Arc<ForwardLog>, Arc<Recorder>) {
        lanes_on(fake.addr().port(), lane_rows(sessions))
    }

    // -----------------------------------------------------------------------
    // the gx half: kind dispatch, the transport hook, the credential source
    // -----------------------------------------------------------------------

    /// A gx session id shaped like a real one (a UUIDv7): the fold splits an
    /// event id at the LAST hyphen, so a toy id would not exercise that.
    const GX_SID: &str = "01a0fa1e-0000-7000-8000-0000000000ab";

    /// The gx adapter's windows, scaled the way `shed-gx`'s own suite scales
    /// them — no cell here waits out a real one.
    fn gx_fast() -> GxTimings {
        GxTimings {
            stall: Duration::from_millis(2_000),
            resume_window: Duration::from_millis(2_000),
            resume_tries: 3,
            flush_after: Duration::from_millis(300),
            seed_limit: 500,
            rest_cap: 8 << 20,
            down_after: Duration::from_millis(4_000),
        }
    }

    /// gx's own `turn_completed` extension — the frame that CLOSES an open
    /// streak, so a seeded transcript does not leave one hanging.
    fn gx_turn_completed(n: u64) -> Value {
        json!({
            "eventId": format!("{GX_SID}-{n}"),
            "method": "_x.ai/session/update",
            "params": {
                "sessionId": GX_SID,
                "update": { "sessionUpdate": "turn_completed", "stop_reason": "end_turn" },
                "_meta": { "agentTimestampMs": 1_788_931_000_000i64 + (n as i64) * 1_000 },
            },
        })
    }

    /// One `session/update` envelope carrying `text`.
    fn gx_chunk(n: u64, text: &str) -> Value {
        json!({
            "eventId": format!("{GX_SID}-{n}"),
            "method": "session/update",
            "params": {
                "sessionId": GX_SID,
                "update": { "sessionUpdate": "agent_message_chunk",
                            "content": { "type": "text", "text": text } },
                "_meta": { "agentTimestampMs": 1_788_931_000_000i64 + (n as i64) * 1_000 },
            },
        })
    }

    /// A `Lanes` on the doubles, with `rows` as the machine's stamped lanes and
    /// tunnels landing on `port`.
    fn lanes_on(
        port: u16,
        rows: BTreeMap<String, AgentLaneStamp>,
    ) -> (Arc<Lanes>, Arc<ForwardLog>, Arc<Recorder>) {
        let log = Arc::new(ForwardLog::default());
        let recorder = Arc::new(Recorder::default());
        let machines = Arc::new(FakeMachines {
            lanes: Mutex::new(rows),
            port,
            log: Arc::clone(&log),
            reach: ReachKind::Ssh(fake_entry()),
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
            GxConfig::default(),
        ));
        (lanes, log, recorder)
    }

    /// A `Lanes` whose machine is reached LOCALLY — no tunnel, and the gx
    /// credential source reads files under `gx.home` instead of probing over
    /// ssh. The path a hermetic harness cell takes.
    fn lanes_local(rows: BTreeMap<String, AgentLaneStamp>, gx: GxConfig) -> Arc<Lanes> {
        let machines = Arc::new(FakeMachines {
            lanes: Mutex::new(rows),
            port: 0,
            log: Arc::new(ForwardLog::default()),
            reach: ReachKind::Local,
        });
        Arc::new(Lanes::with_sink(
            tokio::runtime::Handle::current(),
            Arc::new(|_: &str, _: &str, _: &LaneEvent| {}),
            machines,
            gx,
        ))
    }

    /// The `MachineEntry` [`FakeMachines`] hands out — what a forward is
    /// reserved against.
    fn fake_entry() -> MachineEntry {
        MachineEntry {
            name: MACHINE.to_string(),
            host: MACHINE.to_string(),
            ssh_port: 22,
            ..MachineEntry::default()
        }
    }

    /// **A kind this build has no adapter for is refused BY NAME, and nothing is
    /// reserved on the way to finding that out.**
    ///
    /// `no_lane` would have been the easy answer and it is the wrong one: the
    /// row DOES carry a lane, a newer build may well speak it, and a client that
    /// cannot tell the two apart cannot say anything useful about either. The
    /// second half of the assertion is the one that would rot silently — the
    /// refusal must happen before the tunnel, or an unopenable row would cost an
    /// `ssh -N` child and a readiness poll every time a panel touched it.
    #[tokio::test]
    async fn a_kind_with_no_adapter_is_unsupported_lane_and_reserves_no_tunnel() {
        let (lanes, log, _recorder) = lanes_on(1, lane_rows_of("claude", &["ses_x"]));

        let failure = lanes
            .open(MACHINE, "ses_x")
            .await
            .expect_err("this build speaks opencode and gx, not claude");
        assert_eq!(failure.code(), "unsupported_lane");
        assert!(
            failure.message().contains("\"claude\""),
            "the refusal names the kind: {}",
            failure.message()
        );

        assert_eq!(log.built.load(SeqCst), 0, "no tunnel was reserved");
        assert!(forward_users(&lanes).is_none(), "no tunnel was registered");
        // And the row is still there to be refused again the same way — the
        // refusal is stateless, not a poisoned entry.
        assert_eq!(
            lanes
                .open(MACHINE, "ses_x")
                .await
                .expect_err("still refused")
                .code(),
            "unsupported_lane"
        );
    }

    /// **A row that changes AGENT on the same port is a different lane.**
    ///
    /// The entry used to be keyed on `server_url` alone, which cannot see this:
    /// a tab that restarts as gx on the port opencode had would have kept the
    /// old entry, and the panel would have gone on pumping an opencode client at
    /// a gx server. The stamp is compared whole.
    #[tokio::test]
    async fn an_entry_is_evicted_when_the_kind_changes_under_a_stable_url() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _recorder) = lanes_for(&fake, &["ses_a"]);
        lanes.open(MACHINE, "ses_a").await.expect("opens");
        assert_eq!(forward_users(&lanes), Some(1));

        // Same session, same URL, different agent.
        lanes.reconcile(MACHINE, &lane_rows_of("gx", &["ses_a"]));
        assert!(
            forward_users(&lanes).is_none(),
            "the opencode entry (and its tunnel) did not survive the kind change"
        );
        wait_for("the tunnel's child to be reaped", || {
            (!log.alive.load(SeqCst)).then_some(())
        })
        .await;
    }

    /// **The forwarded transport hook.**
    ///
    /// It answers the tunnel's local end and `ensure`s it EVERY time — which is
    /// the whole mechanism behind "a gx lane repairs its forward without a
    /// `Reset`". And a dial after the last share is gone is `Unavailable`, not a
    /// resurrected `ssh` child behind a lane nobody has open.
    ///
    /// The LOCAL shape is not this type: it is [`shed_gx::FixedDial`], whose own
    /// suite pins both the URL it answers and the trailing-slash normalisation
    /// (`transport.rs::fixed_dial_answers_the_url_it_was_built_on`). Nothing is
    /// left here to restate about it.
    #[tokio::test]
    async fn the_transport_hook_is_a_cache_read_until_the_tunnel_dies() {
        let (lanes, log, _recorder) = lanes_on(45_999, BTreeMap::new());

        let share = lanes
            .reserve((MACHINE.to_string(), REMOTE_PORT), &fake_entry())
            .expect("reserves");
        let forwarded = TauriTransport::new(&share);
        assert_eq!(log.ensures.load(SeqCst), 0, "reserving is not ensuring");

        // A reserved-but-never-ensured forward is not alive, so the FIRST dial
        // establishes it — and every dial after that is free.
        for _ in 0..4 {
            assert_eq!(
                forwarded.dial().await.expect("dials").as_str(),
                "http://127.0.0.1:45999/"
            );
        }
        assert_eq!(
            log.ensures.load(SeqCst),
            1,
            "gx dials before EVERY request; a healthy tunnel must cost nothing \
             but a `try_wait` (shed_gx::GxTransport's own contract)"
        );

        // The child dies — a killed `ssh -N -L`, the case §8.5 stages by hand.
        // The very next dial repairs it, with no `LaneEvent` and nothing waiting
        // on a human.
        log.alive.store(false, SeqCst);
        assert_eq!(
            forwarded.dial().await.expect("dials").as_str(),
            "http://127.0.0.1:45999/"
        );
        assert_eq!(
            log.ensures.load(SeqCst),
            2,
            "a dead tunnel is re-established"
        );
        assert!(log.alive.load(SeqCst), "…and is alive again afterwards");

        // …and back to free.
        forwarded.dial().await.expect("dials");
        assert_eq!(log.ensures.load(SeqCst), 2);

        drop(share);
        let failure = forwarded
            .dial()
            .await
            .expect_err("the tunnel is gone with its last share");
        assert!(matches!(failure, LaneError::Unavailable(_)), "{failure:?}");
        assert_eq!(
            log.ensures.load(SeqCst),
            2,
            "a dial with no tunnel left does not build one"
        );
    }

    /// A `$GROK_HOME` with one record and an eligible token, as gx writes them.
    fn gx_home_fixture(url: &str, instance: &str, token: &str) -> tempfile::TempDir {
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().expect("a scratch grok home");
        let token_path = dir.path().join("gx-remote.token");
        // Real gx writes a trailing newline; the reader has to tolerate it.
        std::fs::write(&token_path, format!("{token}\n")).expect("the token");
        std::fs::set_permissions(&token_path, std::fs::Permissions::from_mode(0o600))
            .expect("0600");
        // The suffixed filename is the shape gx uses when its leader is on a
        // non-default socket — and the token file is NOT suffixed with it, which
        // is exactly why the token's path comes off the record.
        std::fs::write(
            dir.path().join("gx-remote-0123456789abcdef.json"),
            json!({
                "url": url,
                "pid": std::process::id(),
                "instanceId": instance,
                "socketPath": "/tmp/leader.sock",
                "tokenFile": token_path.to_string_lossy(),
                "version": "1.0.16+gx.12",
                "startedAt": 1_788_931_000i64,
            })
            .to_string(),
        )
        .expect("the record");
        dir
    }

    const FIXTURE_TOKEN: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    /// **The LOCAL reader reads files, and only eligible ones.**
    ///
    /// It never shells out (the harness proves that from outside with a
    /// process-tree check; here the point is that the shipped path IS
    /// `shed_gx::local_discovery`, checks included). The `0644` half is what
    /// makes the checks load-bearing rather than decorative: gx's own reader
    /// refuses a world-readable token and shed must not be the weaker reader.
    #[tokio::test]
    async fn the_local_reader_finds_an_eligible_token_and_refuses_an_ineligible_one() {
        use std::os::unix::fs::PermissionsExt as _;
        let url = "http://127.0.0.1:2431";
        let home = gx_home_fixture(url, "inst-local", FIXTURE_TOKEN);
        let cache: GxCache = cold_cache();

        let creds = reading(url, home.path(), Arc::clone(&cache));
        let found = creds
            .discover(url)
            .await
            .expect("the fixture home is readable");
        assert_eq!(found.instance_id, "inst-local");
        assert_eq!(
            found.token.expose(),
            FIXTURE_TOKEN,
            "the newline is trimmed"
        );
        // Nothing about the token is printable.
        assert!(
            !format!("{found:?}").contains(FIXTURE_TOKEN),
            "Debug leaked the token: {found:?}"
        );

        // Now make the token world-readable and read it with a FRESH source (the
        // one above has answered, and the cache holds its answer).
        std::fs::set_permissions(
            home.path().join("gx-remote.token"),
            std::fs::Permissions::from_mode(0o644),
        )
        .expect("0644");
        let refused = reading(url, home.path(), cold_cache())
            .discover(url)
            .await
            .expect_err("a 0644 token is not eligible");
        assert!(matches!(refused, LaneError::Unavailable(_)), "{refused:?}");

        // A record for a DIFFERENT url is not this lane's record.
        let elsewhere = reading("http://127.0.0.1:2999", home.path(), cold_cache())
            .discover("http://127.0.0.1:2999")
            .await
            .expect_err("no record names that url");
        assert!(
            matches!(elsewhere, LaneError::Unavailable(_)),
            "{elsewhere:?}"
        );
    }

    /// A credential source on the LOCAL reach, reading `home`. `probing`'s twin.
    fn reading(url: &str, home: &std::path::Path, cache: GxCache) -> TauriGxCredentials {
        TauriGxCredentials::new(
            MACHINE,
            url,
            &ReachKind::Local,
            &GxConfig {
                home: home.to_path_buf(),
                timings: GxTimings::default(),
            },
            cache,
        )
    }

    /// A fresh, empty [`GxCache`] — for a cell that is about the READ, not the
    /// cache.
    fn cold_cache() -> GxCache {
        Arc::new(Mutex::new(HashMap::new()))
    }

    /// A [`ProbeRunner`] that always answers `out`.
    fn responds(out: &str) -> ProbeRunner {
        let out = out.to_string();
        Arc::new(move || {
            let out = out.clone();
            Box::pin(async move { Ok(out) })
        })
    }

    /// A [`ProbeRunner`] that always fails with `error`.
    fn fails(error: &str) -> ProbeRunner {
        let error = error.to_string();
        Arc::new(move || {
            let error = error.clone();
            Box::pin(async move { Err(error) })
        })
    }

    /// **A credential the agent refused is evicted, so a rotated token does not
    /// wedge the lane for ever.**
    ///
    /// Serving the shared cache once per source is not enough on its own. If the
    /// token moves while `instanceId` does NOT, the pin still succeeds — it only
    /// checks the instance — and the stale token 401s. Every fresh `lane.open`
    /// builds a source with `answered = false`, reads the same cached token, and
    /// 401s again; there is no path back, because nothing in the pin sequence
    /// ever disagrees. Not a degraded-but-recovering state: a permanent loop
    /// until the app restarts.
    ///
    /// The cell stages exactly that, in the direction a hermetic test can drive:
    /// the home starts with the WRONG token, which poisons the cache on the
    /// first open, and then the file is rewritten with the right one — a token
    /// rotation from the reader's point of view, with the leader's instance id
    /// never moving.
    #[tokio::test]
    async fn a_refused_credential_is_evicted_so_a_rotated_token_recovers() {
        use shed_gx::testing::FakeGx;

        let fake = FakeGx::start().await;
        fake.add_session(GX_SID, Some("the lane"), "/w", "idle", 0, false);
        fake.set_history(GX_SID, vec![gx_chunk(10, "seeded"), gx_turn_completed(11)]);
        fake.pin(GX_SID);

        // A home whose record names the leader correctly and whose TOKEN is
        // stale. `instanceId` matches, so the pin will succeed and the bearer
        // request is what fails — which is the whole point.
        let home = gx_home_fixture(&fake.reported_url(), &fake.instance_id(), FIXTURE_TOKEN);
        assert_ne!(
            fake.token(),
            FIXTURE_TOKEN,
            "the fixture token must be the WRONG one for this cell to mean anything"
        );

        let rows = BTreeMap::from([(
            GX_SID.to_string(),
            AgentLaneStamp {
                kind: "gx".to_string(),
                session_id: GX_SID.to_string(),
                server_url: fake.reported_url(),
            },
        )]);
        let lanes = lanes_local(
            rows,
            GxConfig {
                home: home.path().to_path_buf(),
                timings: gx_fast(),
            },
        );

        // Open #1: the stale token is read, the pin succeeds on the matching
        // instance id, and the roster GET is refused.
        let refused = lanes
            .open(MACHINE, GX_SID)
            .await
            .expect_err("a stale token is refused");
        assert_eq!(refused.code(), "unauthorized", "{}", refused.message());

        // **The eviction.** Without it the entry still holds the stale token and
        // every later open reads it back.
        assert!(
            lock(&lanes.gx_cache).is_empty(),
            "a credential the agent refused must not stay in the cache — it is \
             what every future open would read"
        );

        // The token rotates to the one the leader actually wants. The record,
        // and so the instance id, is untouched: nothing in the pin sequence has
        // any reason to disagree.
        std::fs::write(
            home.path().join("gx-remote.token"),
            format!("{}\n", fake.token()),
        )
        .expect("rotate the token");

        // Open #2 re-reads, and the lane comes up. THIS is the user-visible
        // property: without the eviction it would 401 on the cached token
        // again, and so would every open after it, for ever.
        let opened = lanes.open(MACHINE, GX_SID).await.unwrap_or_else(|e| {
            panic!(
                "the rotated token was not picked up ({}: {}) — the lane is \
                 wedged on a cached credential the agent already refused",
                e.code(),
                e.message()
            )
        });
        assert_eq!(opened["session"]["id"], json!(GX_SID));
        assert_eq!(opened["capabilities"]["kind"], json!("gx"));

        // …and the cache is warm again with the credential that WORKS, so the
        // eviction cost one re-read rather than the benefit the cache is for.
        assert_eq!(
            lock(&lanes.gx_cache).len(),
            1,
            "the fresh credential is cached for the next open"
        );
        lanes.close(MACHINE, GX_SID);
    }

    /// A credential source whose probe is a closure, so the SSH half can be
    /// driven without an sshd.
    fn probing(url: &str, cache: GxCache, run: ProbeRunner) -> TauriGxCredentials {
        TauriGxCredentials {
            machine: MACHINE.to_string(),
            reported_url: url.to_string(),
            source: GxSource::Ssh(run),
            cache,
            answered: AtomicBool::new(false),
        }
    }

    /// What [`shed_gx::PROBE_SCRIPT`] prints on a healthy host.
    fn probe_output(url: &str, instance: &str, token: &str) -> String {
        format!(
            "{}\n---\n===token===\n{token}\n",
            json!({
                "url": url,
                "pid": 4242,
                "instanceId": instance,
                "socketPath": "/tmp/leader.sock",
                "tokenFile": "/home/u/.grok/gx-remote.token",
                "version": "1.0.16+gx.12",
                "startedAt": 1_788_931_000i64,
            })
        )
    }

    /// **A failing probe never forwards what the remote printed.**
    ///
    /// `shed_app::machine::exec` builds its error from the remote's **stdout**
    /// when stderr is empty, and the remote's stdout is one `cat` away from
    /// being the token. So the error this cell plants is the realistic one — a
    /// 64-hex run inside `exec`'s own message — and the assertion is that none
    /// of it reaches the caller.
    #[tokio::test]
    async fn a_failing_probe_is_a_fixed_message_and_never_the_raw_error() {
        let url = "http://127.0.0.1:2431";
        let leaky = format!("machine:{MACHINE}: rc failed (exit 1): {FIXTURE_TOKEN}");
        let creds = probing(url, cold_cache(), fails(&leaky));

        let failure = creds.discover(url).await.expect_err("the probe failed");
        let LaneError::Unavailable(message) = &failure else {
            panic!("a failed probe is the quiet variant, not {failure:?}");
        };
        assert_eq!(message, &format!("gx discovery failed on {MACHINE}"));
        assert!(
            !format!("{failure:?}").contains(FIXTURE_TOKEN),
            "the raw error reached the caller: {failure:?}"
        );
    }

    /// **The SSH reader parses the probe, and refuses what it cannot use.**
    #[tokio::test]
    async fn the_ssh_reader_pins_the_record_for_its_url_and_needs_a_token() {
        let url = "http://127.0.0.1:2431";
        let cache: GxCache = cold_cache();

        let out = probe_output(url, "inst-ssh", FIXTURE_TOKEN);
        let found = probing(url, Arc::clone(&cache), responds(&out))
            .discover(url)
            .await
            .expect("a healthy probe");
        assert_eq!(found.instance_id, "inst-ssh");
        assert_eq!(found.token.expose(), FIXTURE_TOKEN);

        // The sentinel with nothing after it: the file was there and INELIGIBLE
        // (a symlink, `0644`, or somebody else's), which the script reports by
        // printing no token rather than by failing.
        let withheld = probing(url, cold_cache(), responds("===token===\n"))
            .discover(url)
            .await
            .expect_err("no record and no token");
        assert!(
            matches!(withheld, LaneError::Unavailable(_)),
            "{withheld:?}"
        );

        // Output that never reached the sentinel: the probe did not run to
        // completion. `ProbeError`'s messages are fixed strings by construction,
        // so THIS one is safe to show — it is the difference a user can act on.
        let truncated = probing(url, cold_cache(), responds("sh: not found"))
            .discover(url)
            .await
            .expect_err("truncated");
        assert!(
            truncated.to_string().contains("did not run to completion"),
            "{truncated:?}"
        );
    }

    /// **The cache serves once per source, and a re-ask always reads fresh.**
    ///
    /// This is the rule that makes `ensure_pinned`'s retry work. It asks a
    /// second time PRECISELY because the first answer did not match `healthz` —
    /// a leader restarted, its `instanceId` moved, its token did not — and
    /// answering that from the same cache entry would turn a recoverable restart
    /// into a permanent `unavailable`. The benefit the cache exists for (a
    /// second lane on a machine already known costs no round trip) is kept.
    #[tokio::test]
    async fn a_source_serves_the_cache_once_and_re_reads_after_that() {
        let url = "http://127.0.0.1:2431";
        let cache: GxCache = cold_cache();
        let calls = Arc::new(AtomicUsize::new(0));
        let runner: ProbeRunner = {
            let calls = Arc::clone(&calls);
            let url = url.to_string();
            Arc::new(move || {
                let n = calls.fetch_add(1, SeqCst);
                let out = probe_output(&url, &format!("inst-{n}"), FIXTURE_TOKEN);
                Box::pin(async move { Ok(out) })
            })
        };

        // A cold source: a miss, then a fresh read for the retry.
        let first = probing(url, Arc::clone(&cache), Arc::clone(&runner));
        assert_eq!(
            first.discover(url).await.expect("cold").instance_id,
            "inst-0"
        );
        assert_eq!(
            first.discover(url).await.expect("retry").instance_id,
            "inst-1"
        );
        assert_eq!(
            calls.load(SeqCst),
            2,
            "the retry did NOT come from the cache"
        );

        // A second lane on the same (machine, url): warm, and free.
        let second = probing(url, Arc::clone(&cache), Arc::clone(&runner));
        assert_eq!(
            second.discover(url).await.expect("warm").instance_id,
            "inst-1",
            "the second lane reused what the first one left"
        );
        assert_eq!(calls.load(SeqCst), 2, "no probe ran for the warm open");
        // …and its own retry still reads fresh, refreshing the shared entry.
        assert_eq!(
            second.discover(url).await.expect("retry").instance_id,
            "inst-2"
        );
        assert_eq!(calls.load(SeqCst), 3);

        let third = probing(url, Arc::clone(&cache), Arc::clone(&runner));
        assert_eq!(
            third.discover(url).await.expect("warm").instance_id,
            "inst-2",
            "a pin mismatch left the FRESH record behind, not the stale one"
        );

        // A different url is a different key, so it is a miss.
        let other = "http://127.0.0.1:2999";
        let elsewhere = probing(other, Arc::clone(&cache), Arc::clone(&runner));
        assert!(
            elsewhere.discover(other).await.is_err(),
            "the probe's record names 2431, not 2999"
        );
        assert_eq!(calls.load(SeqCst), 4, "a different url probes for itself");
    }

    /// A transport that counts its dials — the production [`TauriTransport`]
    /// underneath, so what is counted is the real hook.
    struct CountingTransport {
        inner: TauriTransport,
        dials: AtomicUsize,
    }

    #[async_trait::async_trait]
    impl GxTransport for CountingTransport {
        async fn dial(&self) -> Result<reqwest::Url, LaneError> {
            self.dials.fetch_add(1, SeqCst);
            self.inner.dial().await
        }
    }

    /// **Forward repair rides the transport hook, not a `LaneEvent`** (plan 017
    /// §3.1 #1, §3.5).
    ///
    /// A gx stream that ends and resumes from its cursor emits NO `Reset` — that
    /// is the feature — so [`Lanes::spawn_pump`]'s `Reset`-driven re-`ensure`,
    /// which is what respawns a dead `ssh -N` child under an opencode lane, has
    /// nothing to fire on. This cell is the proof that the forward is
    /// nevertheless re-ensured: cut the stream, watch the frame that arrives
    /// afterwards, and assert the tunnel was ensured again across the gap with
    /// the client's view never restaged.
    #[tokio::test]
    async fn a_gx_lane_re_ensures_its_tunnel_across_a_silent_resume_with_no_reset() {
        use shed_gx::discovery::StaticCredentials;
        use shed_gx::testing::FakeGx;

        let fake = FakeGx::start().await;
        // `idle`, because the seeded transcript ends in `turn_completed` — a
        // roster that disagreed with its own transcript would be a fixture, not
        // a gx.
        fake.add_session(GX_SID, Some("the lane"), "/w", "idle", 0, false);
        fake.set_history(GX_SID, vec![gx_chunk(10, "seeded"), gx_turn_completed(11)]);
        fake.pin(GX_SID);

        // A real tunnel share, whose `ensure` count is the assertion.
        let (lanes, log, _recorder) = lanes_on(fake.addr().port(), BTreeMap::new());
        let share = lanes
            .reserve((MACHINE.to_string(), REMOTE_PORT), &fake_entry())
            .expect("reserves");
        let transport = Arc::new(CountingTransport {
            inner: TauriTransport::new(&share),
            dials: AtomicUsize::new(0),
        });
        let client = GxClient::new(
            fake.reported_url(),
            Arc::clone(&transport) as Arc<dyn GxTransport>,
            Arc::new(
                StaticCredentials::from_parts(&fake.token(), &fake.instance_id())
                    .expect("the fake's token parses"),
            ),
            gx_fast(),
        )
        .expect("the gx client builds");

        let (mut rx, _stop) = client
            .subscribe(GX_SID, None)
            .await
            .expect("subscribe never fails")
            .into_parts();
        // Drain the seed.
        loop {
            match rx.recv().await.expect("the seed") {
                LaneEvent::Ready { .. } => break,
                LaneEvent::Down { reason } => panic!("the seed went down: {reason}"),
                _ => {}
            }
        }
        let dials_at_ready = transport.dials.load(SeqCst);
        let ensures_at_ready = log.ensures.load(SeqCst);
        assert!(
            ensures_at_ready > 0,
            "the seed dialled through the hook, so the tunnel was ensured"
        );

        // **The `ssh -N -L` child dies**, which is what killed the stream — the
        // failure §8.5 stages by hand, modelled here in the order it really
        // happens: the tunnel goes, and the stream ends BECAUSE it went.
        log.alive.store(false, SeqCst);
        fake.close_streams();
        wait_for("the fake to release the stream", || {
            (fake.stream_count() == 0).then_some(())
        })
        .await;
        // …and something happened on the far side while the lane was gone.
        fake.push_update(GX_SID, &gx_chunk(20, "GAP"));

        // The gap frame arriving IS the silent resume completing.
        let mut seen = Vec::new();
        loop {
            let event = tokio::time::timeout(Duration::from_secs(10), rx.recv())
                .await
                .expect("the resumed stream delivered nothing")
                .expect("the lane stream ended");
            let hit = matches!(&event, LaneEvent::Message { message, .. }
                if message.text.as_deref().is_some_and(|t| t.contains("GAP")));
            seen.push(event);
            if hit {
                break;
            }
        }

        assert!(
            !seen.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
            "a resume the server accepted is SILENT — the panel is never restaged"
        );
        assert!(
            transport.dials.load(SeqCst) > dials_at_ready,
            "the reconnect went through the transport hook"
        );
        assert!(
            log.ensures.load(SeqCst) > ensures_at_ready,
            "and the hook re-established the tunnel — which is the ONLY thing \
             that respawns a dead `ssh -N` child under a lane that never emits \
             a Reset"
        );
        assert!(
            log.alive.load(SeqCst),
            "the tunnel is up again, and nothing waited on a human for it"
        );
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
