//! **Agent lanes in the desktop app** — the client half of
//! [`shed_core::lane`] (plan 015 §3.4, plan 017 §3.5).
//!
//! A machine row whose roost tab reported an agent's control surface carries an
//! `agent_lane` stamp ([`crate::roost_hosts::host_row`], derived by
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
//!                 match stamp.kind {  "opencode" => OpencodeSource::new(url).open(id),
//!                                     other      => UnsupportedLane }
//!                                                          |
//!                                              Arc<dyn AgentLane>
//!                                                          |
//!                                       subscribe() -> Reset … Ready … frames
//!                                                          |
//!                                     LaneView (staged, then swapped) + `lane-event`
//! ```
//!
//! # One trait, one adapter today — and the line the dispatch draws
//!
//! [`LaneEntry`] holds an `Arc<dyn AgentLane>`, and the ONLY place in this app
//! that names a concrete adapter type is the `match` in [`Lanes::open`]. That is
//! the whole point of plan 017: everything below the match — the pump, the view,
//! the tunnel bookkeeping, the six IPC verbs — is written against the contract,
//! so the next adapter is a `match` arm rather than a refactor. (gx held this
//! second slot from plan 017 until plan 025 C1 retired it, shed#390; craze
//! holds it since plan 025 C9.)
//!
//! # Two kinds of stamp: roost's and craze's
//!
//! An opencode row's lane is stamped by ROOST — its tab reported a server —
//! and is reached by that URL, over a forward when the machine is remote. A
//! craze row's lane is stamped by the machine's craze SOURCE: the row is the
//! hub's (`agent_lane: {kind: "craze", session_id: <hostId>}`,
//! [`crate::roost_hosts::RoostHosts::craze_lanes`]), and [`Lanes::open`]
//! branches on the kind BEFORE any reach or forward: a craze stamp resolves the
//! host's [`shed_craze::CrazeSource`] and calls `source.open(hostId)`, which
//! binds with no I/O; the lane reaches its session through the hub's splice on
//! connections of its own. Everything below that branch — the pump, the view,
//! `lane-event`, the `lane.*` verbs — is the same for both.
//!
//! **The registry key carries the kind**, `(machine, kind, session_id)`, so a
//! craze hostId and an opencode session id live in separate namespaces, and
//! every `lane.*` op takes `kind` (the row's `agent_lane.kind`). **Eviction is
//! split the same way**: [`Lanes::reconcile`] judges only the roost-stamped
//! entries against a roost snapshot — a craze lane is not in it and must not
//! be torn down by it — and [`Lanes::evict_craze`] retires the craze entries
//! whose rows the source no longer holds (a `Removed`, a reseed that dropped
//! them, the hub gone, the host removed, its tab ended). A craze entry is
//! filed under the GENERATION of the source it was opened through, and an
//! eviction names its generation: the news of a source that has since been
//! replaced (the host removed and registered again) never ends a lane opened
//! through the new one. An open re-reads the host's current generation after
//! it declares itself, so a removal that published its eviction before the
//! declaration — with nothing yet to cancel — still stops the open.
//!
//! Since plan 025 split the contract (§3.2), the arm builds the agent's SOURCE
//! and opens the session-scoped lane through it — `OpencodeSource::new(url,
//! None).open(session_id)`, binding with no I/O — so every adapter is reached
//! the same way. And the lane's **capabilities ride its stream**
//! ([`LaneEvent::Capabilities`]), not a getter: [`LaneEntry`] caches none, and
//! `lane.open` answers `{session}` alone. What the session can do is read where
//! the panel reads everything else — `lane.messages`, from the staged view —
//! because a craze session's capabilities change with its incarnation and a
//! copy taken at open would go stale.
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
//! [`crate::roost_hosts::OnLanes`] — and it matters most for the case nothing else
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
//! `Ready` swaps it in. `Stale` and `Down` both mark the view stale-with-a-reason
//! and keep the last good view on screen — the "consume" posture, not an error
//! dialog — but only `Down` sets `ended`, and only an ENDED subscription is ever
//! replaced (see [`Lanes::spawn_pump`]): reopening on a mere stale mark would
//! throw away the cursor a silent resume needs (plan 025 §3.2.4).
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
//!
//! # Transport repair
//!
//! [`Lanes::spawn_pump`] re-`ensure`s the forward on every `Reset` after the
//! first, which is how a dead `ssh -N` child under an ESTABLISHED opencode lane
//! gets respawned — the adapter announces each reconnect attempt with a `Reset`,
//! and that announcement is the cadence.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::Duration;

use serde_json::{json, Value};
use tauri::{AppHandle, Emitter};

use shed_app::lane_view::LaneView;
use shed_app::machine::{MachineForward, SshForward};
use shed_core::config::MachineEntry;
use shed_core::lane::{
    AgentLane, AgentSource, LaneAnswer, LaneDecision, LaneError, LaneEvent, LaneSession, SendMode,
};
use shed_core::roost::AgentLaneStamp;
use shed_craze::CrazeSource;
use shed_opencode::OpencodeSource;

use crate::machines::ReachKind;
use crate::roost_hosts::RoostHosts;

/// The Tauri event every lane frame reaches the UI on:
/// `{machine, kind, session_id, event}`, `event` being a serialized
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

/// The [`LaneEvent::Down`] reason an adapter ends a lane with when the session
/// does not exist (opencode: a reseed answered 404; craze: `unknown_session` on
/// connect or attach).
const DOWN_UNKNOWN_SESSION: &str = "unknown_session";

/// Whether a lane that ENDED with this `Down` reason is gone for good — so the
/// pump stops instead of resubscribing.
///
/// `shed_core::lane`'s module doc (plan 025 §3.3.5) names three: the session
/// does not exist (`unknown_session`), it was closed (`session_closed`, craze's
/// end after a stop), or it never started (`start_failed:<cause>`). No amount
/// of reconnecting changes any of them. Every other `Down` is worth another
/// attempt (the agent restarted, the tunnel blipped, a bound ran out).
fn down_is_final(reason: &str) -> bool {
    reason == DOWN_UNKNOWN_SESSION
        || reason == "session_closed"
        || reason == "start_failed"
        || reason.starts_with("start_failed:")
}

/// `(machine, kind, agent session id)` — one open lane. The kind is part of
/// the key (plan 025 §3.6.4): a craze hostId and an opencode session id are
/// different namespaces, and one must never answer for the other.
type Key = (String, String, String);

/// The one place a [`Key`] is built — every op addresses its lane through it.
fn key(machine: &str, kind: &str, session_id: &str) -> Key {
    (
        machine.to_string(),
        kind.to_string(),
        session_id.to_string(),
    )
}

/// The craze adapter's kind token (`shed_craze::KIND`) — a craze row's
/// `agent_lane.kind`.
pub(crate) const CRAZE: &str = shed_craze::KIND;

/// Whether `kind`'s lanes are stamped by ROOST (a tab that reported a server)
/// rather than listed by a machine-level source — the entries
/// [`Lanes::reconcile`] judges against a roost snapshot. craze's are the
/// source's, and [`Lanes::evict_craze`] is theirs.
fn roost_stamped(kind: &str) -> bool {
    kind != CRAZE
}

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
/// silently: a second adapter that forgot it would go on claiming this build
/// speaks only one.
const LANE_KINDS: [&str; 2] = ["opencode", CRAZE];

/// **Test-only seam:** how many times this module has actually constructed a
/// concrete `AgentLane` adapter (today, the one `OpencodeSource::new(…).open(…)`
/// in [`Lanes::open`]'s match). Exists so a control can assert "no adapter was
/// built" as a fact about the code, not an inference from "no tunnel was
/// reserved" — see `a_gx_stamped_row_is_unsupported_lane_with_no_forward_reserved`.
/// `#[cfg(test)]` end to end: zero cost and zero surface in a shipped binary.
#[cfg(test)]
static ADAPTER_BUILDS: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

/// Note a successful adapter construction. Called once, right after the one
/// line in [`Lanes::open`] that builds a concrete client — never before it,
/// so a constructor that itself fails (`?`) does not count as "built".
#[cfg(test)]
fn note_adapter_built() {
    ADAPTER_BUILDS.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
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

    /// The craze lanes this machine's craze source lists: hostId → its stamp
    /// (`kind: "craze"`, no URL). Empty for a machine with no craze source.
    fn craze_lanes(&self, _machine: &str) -> BTreeMap<String, AgentLaneStamp> {
        BTreeMap::new()
    }

    /// This machine's craze source — what a craze lane opens through — and
    /// the generation it was started under, which the lane is filed with
    /// ([`Lanes::evict_craze`]).
    fn craze_source(&self, machine: &str) -> Result<(CrazeSource, u64), String> {
        Err(format!("{machine} has no craze source"))
    }

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

impl LaneMachines for RoostHosts {
    fn agent_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp> {
        RoostHosts::agent_lanes(self, machine)
    }

    fn reach_kind(&self, machine: &str) -> Result<ReachKind, String> {
        RoostHosts::reach_kind(self, machine)
    }

    fn craze_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp> {
        RoostHosts::craze_lanes(self, machine)
    }

    fn craze_source(&self, machine: &str) -> Result<(CrazeSource, u64), String> {
        RoostHosts::craze_source(self, machine)
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
    /// The stamp this open is building against. `reconcile` compares it to the
    /// fresh snapshot by exactly the same rule it judges a committed entry by: a
    /// row that now reports a different port — or a different AGENT on the same
    /// port — makes this open obsolete before it ever commits.
    stamp: AgentLaneStamp,
    /// Set by `close`/`reconcile` when the key stopped being wanted. Read under
    /// the same lock acquisition that inserts the entry, so the decision cannot
    /// be raced.
    cancelled: bool,
    /// The craze source generation this open is building through, or `None`
    /// for a roost-stamped lane — the same fence [`LaneEntry::craze_gen`] is.
    craze_gen: Option<u64>,
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
    /// idempotent rather than a second round trip. The session row ONLY: the
    /// lane's capabilities are stream state, read from the view by
    /// `lane.messages` (module doc), and deliberately not cached here.
    session: LaneSession,
    /// The adapter, as the CONTRACT. Every verb below reaches the agent through
    /// this trait object; the concrete type was chosen once, in
    /// [`Lanes::open`]'s match, and is deliberately not knowable from here.
    client: Arc<dyn AgentLane>,
    view: Arc<Mutex<LaneView>>,
    /// The supervision loop. Shares nothing with this struct but the view, so
    /// there is no reference cycle and dropping the entry really does end it.
    pump: tokio::task::JoinHandle<()>,
    /// For a craze lane, the generation of the craze source it was opened
    /// through; `None` for a roost-stamped one. [`Lanes::evict_craze`] ends
    /// only the lanes of the generation it names, so a stale source's eviction
    /// — published after a remove and a re-registration of the same host —
    /// never ends a lane opened through the new one (C9 review).
    craze_gen: Option<u64>,
}

impl LaneEntry {
    fn opened(&self) -> Value {
        json!({ "session": self.session })
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
type EventSink = Arc<dyn Fn(&str, &str, &str, &LaneEvent) + Send + Sync>;

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
    /// [`crate::roost_hosts::TestGap`], armed by a test: `open`'s gap between
    /// resolving a craze source and declaring the open.
    #[cfg(test)]
    pub(crate) open_gap: Mutex<Option<crate::roost_hosts::TestGap>>,
}

impl Lanes {
    pub fn new(handle: tokio::runtime::Handle, app: AppHandle, machines: Arc<RoostHosts>) -> Lanes {
        let sink: EventSink = Arc::new(
            move |machine: &str, kind: &str, session_id: &str, event: &LaneEvent| {
                let _ = app.emit(
                    LANE_EVENT,
                    json!({ "machine": machine, "kind": kind, "session_id": session_id, "event": event }),
                );
            },
        );
        Lanes::with_sink(handle, sink, machines)
    }

    /// A layer whose frames go nowhere — for the roost-host layer's tests,
    /// which drive the craze half through a real [`RoostHosts`].
    #[cfg(test)]
    pub(crate) fn for_test(
        handle: tokio::runtime::Handle,
        machines: Arc<dyn LaneMachines>,
    ) -> Lanes {
        Lanes::with_sink(
            handle,
            Arc::new(|_: &str, _: &str, _: &str, _: &LaneEvent| {}),
            machines,
        )
    }

    /// Whether a lane is committed under this key — the registry's own state.
    #[cfg(test)]
    pub(crate) fn is_open(&self, machine: &str, kind: &str, session_id: &str) -> bool {
        lock(&self.inner)
            .entries
            .contains_key(&key(machine, kind, session_id))
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
            #[cfg(test)]
            open_gap: Mutex::new(None),
        }
    }

    /// `lane.open` — ensure the transport, open the lane through its agent's
    /// source, start the subscription, and answer with the session row.
    ///
    /// Not with what the lane can do: capabilities are per session and ride the
    /// stream, so `lane.messages` answers them from the staged view (module
    /// doc).
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
    pub async fn open(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let key = key(machine, kind, session_id);
        // The row is resolved BEFORE a gate is registered for the key. Both IPC
        // doors take `machine` and `session_id` as free strings, so a call that
        // names nothing real must not be able to make this app remember it:
        // `lane.open` with junk (or with the session ids of a machine whose tabs
        // churn) used to mint a gate per call and keep it for the life of the
        // process.
        self.lane_stamp(machine, kind, session_id)?;
        let gate = self.gate(&key);
        let _serialized = gate.lock().await;

        // Re-resolved under the gate, and this is the value everything below
        // uses: the pre-gate one was read before waiting, and waiting is exactly
        // when a tab restarts onto a new port or goes away. Using it would open
        // a lane against a socket the snapshot has already retired.
        let stamp = self.lane_stamp(machine, kind, session_id)?;
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

        // A craze lane's source — and the generation it is filed under —
        // resolved before the declaration, so the declaration carries it.
        let craze = if stamp.kind == CRAZE {
            Some(
                self.machines
                    .craze_source(machine)
                    .map_err(|e| LaneFailure::Lane(LaneError::Unavailable(e)))?,
            )
        } else {
            None
        };
        let craze_gen = craze.as_ref().map(|(_, gen)| *gen);
        #[cfg(test)]
        crate::roost_hosts::at_gap(&self.open_gap).await;

        // Declared BEFORE the first await, so a `close` or a `reconcile` landing
        // anywhere below has something to cancel.
        let _pending = self.declare(&key, &stamp, craze_gen);

        // **A craze source's generation is re-checked AFTER the declaration**
        // (C9 confirmation, N1). The source was resolved above, outside the
        // lane registry's lock, and the host can be removed in between: its
        // eviction ([`Self::evict_craze`]) then finds no open to cancel, and
        // the clone resolved here — a stopped source still holding its old
        // roster — would commit a lane nothing would ever end. The removal
        // takes the source out of the registry BEFORE it publishes the
        // eviction, and the eviction and the declaration take the same lock, so
        // one of the two always catches it: an eviction after the declaration
        // cancels this pending open; one before it left this re-check a host
        // with no source, or another generation's.
        if let Some(gen) = craze_gen {
            let current = self.machines.craze_source(machine).map(|(_, now)| now);
            if current.as_ref().ok() != Some(&gen) {
                return Err(LaneFailure::Lane(LaneError::Unavailable(format!(
                    "{machine}'s craze source stopped while the lane for session \
                     {session_id:?} was being opened"
                ))));
            }
        }

        // **A craze stamp branches off BEFORE any reach or forward** (plan 025
        // §3.6.4): its lane is the source's, reached through the machine's hub
        // on connections of its own — there is no URL to forward to.
        let opened: (Arc<dyn AgentLane>, Option<ForwardShare>) = if let Some((source, _)) = craze {
            // Binding, not dialling (`CrazeSource::open`).
            let built = source.open(session_id).await?;
            #[cfg(test)]
            note_adapter_built();
            (built, None)
        } else {
            self.open_roost_stamped(machine, session_id, &stamp).await?
        };
        let (client, forward) = opened;

        // The roster row is fetched BEFORE the subscription starts: a 404 here
        // is an honest `unknown_session` the caller can render, where the same
        // failure inside the pump would be a `Down` the panel has to wait for.
        // It is also the last await, and the one that fails on a
        // password-protected agent — hence the share above.
        let session = client.session().await.map_err(LaneFailure::from)?;

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
            key.clone(),
            Arc::clone(&client),
            Arc::clone(&view),
            forward.as_ref().map(ForwardShare::weak),
        );
        let entry = Arc::new(LaneEntry {
            stamp,
            forward: Mutex::new(forward),
            session,
            client,
            view,
            pump,
            craze_gen,
        });
        let opened = entry.opened();
        inner.entries.insert(key, entry);
        Ok(opened)
    }

    /// The roost-stamped half of [`Self::open`]: the machine's reach, the
    /// forward (RESERVED, so every `?` after it gives the share back — and
    /// with it the `ssh` child, if this open was its only user), and the
    /// adapter on the reported URL.
    async fn open_roost_stamped(
        &self,
        machine: &str,
        session_id: &str,
        stamp: &AgentLaneStamp,
    ) -> Result<(Arc<dyn AgentLane>, Option<ForwardShare>), LaneFailure> {
        let reach = self
            .machines
            .reach_kind(machine)
            .map_err(|e| LaneFailure::Lane(LaneError::Unavailable(e)))?;
        let (base_url, forward) = self.transport(machine, &reach, &stamp.server_url).await?;

        // **The one place this app names a roost-stamped adapter.** Everything
        // after it is written against `dyn AgentLane`; see the module doc.
        let client: Arc<dyn AgentLane> = match stamp.kind.as_str() {
            "opencode" => {
                let url = reqwest::Url::parse(&base_url).map_err(|e| {
                    LaneFailure::Lane(LaneError::BadRequest(format!(
                        "the reported agent server {:?} is not a usable URL: {e}",
                        stamp.server_url
                    )))
                })?;
                // No credential source — see the module doc. Opening is
                // binding: the source dials nothing here.
                let built = OpencodeSource::new(url, None)?.open(session_id).await?;
                #[cfg(test)]
                note_adapter_built();
                built
            }
            // Unreachable: the guard in `open` ran before anything was
            // reserved. Restated rather than `unreachable!()` so that adding a
            // kind to one list and forgetting the other is a refusal, not a
            // panic.
            other => return Err(LaneFailure::UnsupportedLane(other.to_string())),
        };
        Ok((client, forward))
    }

    /// `lane.messages` — the staged-then-swapped view, as this app's IPC
    /// payload: `{messages, activity, generation, stale, ended, capabilities,
    /// settings}`.
    ///
    /// `capabilities` and `settings` are the LIVE generation's (each `null`
    /// until a seed carrying it has swapped in), which is where a client reads
    /// what the session can do — never from `lane.open` (module doc). `stale` is
    /// the banner and `ended` the lifecycle; the two are different facts
    /// (`shed_app::lane_view`'s module doc).
    ///
    /// The fold and the projection are [`shed_app::lane_view`]'s; the only thing
    /// that belongs here is the envelope's SHAPE, which is Tauri's and not a
    /// client-neutral API (the phone converts the same
    /// [`shed_app::lane_view::LaneViewSnapshot`] into its own DTOs).
    pub fn messages(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        let snap = lock(&entry.view).snapshot(None);
        Ok(json!({
            "messages": snap.messages,
            "activity": snap.activity,
            "generation": snap.generation,
            "stale": snap.stale,
            "ended": snap.ended,
            "capabilities": snap.capabilities,
            "settings": snap.settings,
        }))
    }

    /// `lane.approvals` — what is blocking on the human, this session's and its
    /// descendants'.
    ///
    /// Pending only, oldest first: the snapshot already filtered and sorted
    /// them ([`shed_app::lane_view::LaneView::snapshot`] owns that rule), so
    /// this is the envelope and nothing else.
    pub fn approvals(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        let snap = lock(&entry.view).snapshot(None);
        Ok(json!({ "approvals": snap.approvals }))
    }

    /// `lane.send` — a prompt. `mode` defaults to `queue`; `interject` on a
    /// session whose capabilities say `interject: false` is refused by the
    /// adapter rather than silently downgraded.
    pub async fn send(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
        text: &str,
        mode: SendMode,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        entry.client.send(text, mode).await?;
        Ok(json!({}))
    }

    /// `lane.cancel` — stop the turn in flight.
    pub async fn cancel(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        entry.client.cancel().await?;
        Ok(json!({}))
    }

    /// `lane.stop` — end the SESSION (plan 025 §3.6.4), not just this
    /// transcript: craze's `session.stop`, answered on its receipt; the lane
    /// itself ends when the session's `session_closed` arrives, and the row
    /// leaves the roster then. A session whose capabilities say `stop: false`
    /// (a TUI-hosted craze session, every opencode one) refuses it — the panel
    /// offers no Stop there.
    pub async fn stop(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        entry.client.stop().await?;
        Ok(json!({}))
    }

    /// `lane.answer` — resolve one approval.
    pub async fn answer(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        entry.client.answer(approval_id, answer).await?;
        Ok(json!({}))
    }

    /// `lane.close` — end the subscription and release the transport.
    ///
    /// Idempotent: closing a lane that is not open is success, because the
    /// caller's intent (there is no lane here any more) is already true. The
    /// panel calls this on unmount, and an unmount can race an eviction.
    pub fn close(&self, machine: &str, kind: &str, session_id: &str) -> Value {
        self.evict(&key(machine, kind, session_id));
        json!({})
    }

    /// Reconcile one machine's ROOST-STAMPED lanes against a fresh roost
    /// snapshot: evict every entry whose tab is gone or whose STAMP moved.
    ///
    /// See [`crate::roost_hosts::OnLanes`] for why the snapshot is the signal.
    ///
    /// **Scoped to the kinds roost stamps** (plan 025 §3.6.4): a craze lane's
    /// row is its machine's craze source's, which a roost snapshot never
    /// lists — judged by this rule, every roost snapshot on its machine would
    /// tear it down. Its eviction is [`Self::evict_craze`].
    pub fn reconcile(&self, machine: &str, lanes: &BTreeMap<String, AgentLaneStamp>) {
        let gone: Vec<Arc<LaneEntry>> = {
            let mut inner = lock(&self.inner);
            // Opens still in flight are judged by the SAME rule as committed
            // entries. Without this a tab that went away mid-open would be
            // resurrected by the open that was already past the check.
            for (key, pending) in inner.pending.iter_mut() {
                if key.0 == machine
                    && roost_stamped(&key.1)
                    && lanes.get(&key.2) != Some(&pending.stamp)
                {
                    pending.cancelled = true;
                }
            }
            let mut gone = Vec::new();
            inner.entries.retain(|(m, kind, session_id), entry| {
                if m != machine || !roost_stamped(kind) {
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

    /// Retire the craze lanes on `machine` whose rows left its craze source of
    /// generation `gen` (plan 025 §3.6.4) — a roster `Removed`, a `Ready` swap
    /// that no longer lists them, the hub gone (Dormant), the host removed, or
    /// the tab of a TUI-hosted session ended — and cancel any open of theirs
    /// still in flight. Driven by [`crate::roost_hosts::OnCrazeGone`].
    ///
    /// **Fenced by the generation, under this registry's own lock** (C9
    /// review): the eviction is published after the roost-host layer's lock
    /// that computed it is released, and by then the host may have been
    /// removed, registered again and had a lane opened on the same hostId
    /// through its NEW source. Only an entry (or a pending open) filed under
    /// `gen` is touched, and the comparison and the removal are one
    /// acquisition, so a lane of another generation can never be ended by
    /// this one's news.
    pub fn evict_craze(&self, machine: &str, gen: u64, host_ids: &[String]) {
        let gone: Vec<Arc<LaneEntry>> = {
            let mut inner = lock(&self.inner);
            host_ids
                .iter()
                .filter_map(|host_id| {
                    let key = key(machine, CRAZE, host_id);
                    if let Some(pending) = inner.pending.get_mut(&key) {
                        if pending.craze_gen == Some(gen) {
                            pending.cancelled = true;
                        }
                    }
                    let ours = inner
                        .entries
                        .get(&key)
                        .is_some_and(|entry| entry.craze_gen == Some(gen));
                    if ours {
                        inner.entries.remove(&key)
                    } else {
                        None
                    }
                })
                .collect()
        };
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        for entry in gone {
            entry.retire();
        }
    }

    // ---- internals ----

    /// Declare an open in flight for `key`. See [`Pending`].
    fn declare(&self, key: &Key, stamp: &AgentLaneStamp, craze_gen: Option<u64>) -> PendingGuard {
        lock(&self.inner).pending.insert(
            key.clone(),
            Pending {
                stamp: stamp.clone(),
                cancelled: false,
                craze_gen,
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
    fn open_entry(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Arc<LaneEntry>, LaneFailure> {
        let key = key(machine, kind, session_id);
        if let Some(entry) = self.entry(&key) {
            return Ok(entry);
        }
        // Distinguish the two shapes of "no lane" in the MESSAGE, not the code:
        // a caller does the same thing about both (there is no transcript here),
        // and a second code would be one more thing for a client to branch on
        // for no behavioural difference.
        match self.lane_stamp(machine, kind, session_id) {
            Err(e) => Err(e),
            Ok(_) => Err(LaneFailure::NoLane(format!(
                "no lane is open for session {session_id:?} on machine {machine:?} — \
                 call lane.open first"
            ))),
        }
    }

    /// The [`AgentLaneStamp`] this row reports, or `no_lane`.
    ///
    /// Looked up in the namespace `kind` names (plan 025 §3.6.4): a craze
    /// hostId among the craze source's rows, anything else among roost's
    /// stamps — and a roost stamp of ANOTHER kind under that id is no lane of
    /// this one.
    fn lane_stamp(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<AgentLaneStamp, LaneFailure> {
        if kind == CRAZE {
            return self
                .machines
                .craze_lanes(machine)
                .remove(session_id)
                .ok_or_else(|| {
                    LaneFailure::NoLane(format!(
                        "machine {machine:?}'s craze source lists no session {session_id:?}"
                    ))
                });
        }
        self.machines
            .agent_lanes(machine)
            .remove(session_id)
            .filter(|stamp| stamp.kind == kind)
            .ok_or_else(|| {
                LaneFailure::NoLane(format!(
                    "session {session_id:?} on machine {machine:?} carries no {kind} agent_lane \
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
    /// the subscription ENDS.
    ///
    /// The adapter reconnects on its own inside one subscription (that is the
    /// `Reset` … `Ready` bracket, or — on an adapter that can resume — a `Stale`
    /// and a lone `Ready`); this loop is the layer ABOVE it, and it exists for
    /// the failure the adapter cannot fix — a transport that has gone away. On a
    /// remote machine, re-`ensure`ing the forward is what respawns a dead
    /// `ssh -N` child before redialing.
    ///
    /// **It replaces a subscription only when that subscription ENDED** (its
    /// `Down`, or its channel closing) — never on an ADAPTER's `Stale`, which
    /// is the adapter saying it is retrying with its cursor intact; a
    /// resubscribe there would throw the cursor away (plan 025 §3.2.4). And it
    /// does not replace one that ended for good ([`down_is_final`]). Every
    /// failure this loop itself retries — a forward that would not come up, a
    /// `subscribe` refused for any reason but a missing session — is shown as
    /// [`LaneEvent::Stale`], because it is not an end: the next attempt is
    /// already scheduled.
    ///
    /// **The one time it ends a subscription itself** is its own: a tunnel that
    /// fails its re-`ensure` mid-subscription. That `Stale` abandons the seed
    /// the adapter has just opened, so the subscription is dropped and a fresh
    /// one taken once the tunnel is back — a kept one could finish the
    /// abandoned seed and stream on into a view that takes none of it, a lane
    /// left stale on a healthy connection.
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
        key: Key,
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
                        note(&sink, &view, &key, stale(format!("forward: {e}")));
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                }
                let subscription = match client.subscribe(None).await {
                    Ok(subscription) => subscription,
                    // The session is gone for good: an END, and the last one.
                    // Anything else is worth retrying — the agent may simply be
                    // restarting — so it is stale, not ended.
                    Err(LaneError::UnknownSession) => {
                        note(
                            &sink,
                            &view,
                            &key,
                            LaneEvent::Down {
                                reason: DOWN_UNKNOWN_SESSION.to_string(),
                            },
                        );
                        return;
                    }
                    Err(e) => {
                        note(&sink, &view, &key, stale(e.to_string()));
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                };
                // BOTH halves, for the whole read: taking `.rx` alone drops the
                // stop handle, which aborts the pump it is reading from.
                let (mut rx, stop) = subscription.into_parts();
                let mut down: Option<String> = None;
                // This loop's OWN reason to drop the subscription — set when the
                // tunnel under it failed, so the read below is abandoned and a
                // fresh subscription brings the lane back.
                let mut restart = false;
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
                        // A `Stale` is the adapter retrying on its own, cursor
                        // intact: keep reading. It is folded into the view (the
                        // banner) like any other frame, and that is all.
                        _ => {}
                    }
                    lock(&view).apply(&event);
                    emit(&sink, &key, &event);
                    if generations > 1 && matches!(event, LaneEvent::Reset { .. }) {
                        match ensure_forward(&forward).await {
                            Ensured::Ready => {}
                            // Every share is gone: this lane was evicted.
                            Ensured::Gone => return,
                            // Stale-with-a-reason — not a `Down`, which would
                            // mark a live lane ended — AND a restart. The
                            // `Stale` lands inside the seed this `Reset` just
                            // opened, which abandons it in the view (a loss
                            // before `Ready` reseeds — `shed_app::lane_view`);
                            // if this subscription were kept, an adapter that
                            // reconnected anyway would finish that seed and
                            // stream on into a view that can no longer take any
                            // of it, stale until some unrelated reconnect. So
                            // this loop drops the subscription itself and
                            // resubscribes once the tunnel is back: recovery
                            // always arrives as the adapter's fresh `Reset …
                            // Ready`. (An ADAPTER's own `Stale` is not this
                            // case: the adapter reseeds or resumes on its own.)
                            Ensured::Failed(e) => {
                                note(&sink, &view, &key, stale(format!("forward: {e}")));
                                restart = true;
                                break;
                            }
                        }
                    }
                }
                drop(stop);
                // The subscription ENDED, or this loop ended it. Replace it —
                // unless the adapter ended it for good.
                if !restart && down.as_deref().is_some_and(down_is_final) {
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

/// A failure THIS layer is retrying, as the frame that says so.
///
/// [`LaneEvent::Stale`], not `Down`: the pump has already scheduled its next
/// attempt, so the lane has not ended — and `Down` would set the view's `ended`,
/// which a client reads as "this lane is over, reopen it".
fn stale(reason: String) -> LaneEvent {
    LaneEvent::Stale { reason }
}

/// Fold a frame THIS layer minted (not the adapter) into the view, and tell the
/// UI about it on the same event.
///
/// The panel must not care whether the thing that went away was the agent or the
/// tunnel to it: both mean "this transcript is not live", and both are recovered
/// by the same retry.
fn note(sink: &EventSink, view: &Arc<Mutex<LaneView>>, key: &Key, event: LaneEvent) {
    lock(view).apply(&event);
    emit(sink, key, &event);
}

fn emit(sink: &EventSink, key: &Key, event: &LaneEvent) {
    (**sink)(&key.0, &key.1, &key.2, event);
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
/// **Exactly one form, and nothing beside it** (but `question`'s
/// `custom_text`): a key this grammar does not read is refused, never skipped
/// — `{"permission": "allow-always", "option_id": "reject"}` must not execute
/// as "allow always". **Exactly one form.** The keys are counted before any of
/// them is read, so a
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
    // **Nothing else rides beside the form** (review, astra 6). A key this
    // grammar does not read is not harmless decoration: `{"permission":
    // "allow-always", "option_id": "reject"}` reads as "refuse" to a human and
    // used to execute as "allow always", the `option_id` dropped on the way past.
    // The contract's own `LaneAnswer` refuses that payload by
    // `deny_unknown_fields`; this door is held to the same rule, so the answer
    // either means exactly what it says or is refused before anything is sent.
    // `custom_text` is the one modifier, and only `question` takes it — checked
    // just below, with its own message.
    if let Some(stray) = value
        .as_object()
        .into_iter()
        .flat_map(|o| o.keys())
        .find(|key| key.as_str() != named[0] && key.as_str() != "custom_text")
    {
        return Err(LaneFailure::bad_request(format!(
            "an answer carries its one form and nothing else; `{stray}` is not part \
             of a `{}` answer",
            named[0]
        )));
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
        // blanket refusal.
        assert!(parse_answer(&json!({"choice": "p-1"})).is_ok());
        assert!(parse_answer(&json!({"permission": "reject"})).is_ok());
        assert!(parse_answer(&json!({"question": [["yes"]]})).is_ok());
        assert!(parse_answer(&json!({"reject": true})).is_ok());
    }

    /// **An answer carries its one form and NOTHING else** (review, astra 6). A
    /// key this grammar does not read used to be skipped: `{"permission":
    /// "allow-always", "option_id": "reject"}` — "refuse" to a human — decoded
    /// as `Permission{AllowAlways}` and opencode would have executed it as a
    /// persistent approval. The contract's `LaneAnswer` refuses that payload
    /// (`deny_unknown_fields`); this door now does too, before any answer exists.
    #[test]
    fn an_answer_with_any_key_beside_its_form_is_refused() {
        for stray in [
            json!({"permission": "allow-always", "option_id": "reject"}),
            json!({"permission": "reject", "note": "hi"}),
            json!({"choice": "p-1", "decision": "allow_once"}),
            json!({"question": [["yes"]], "option_id": "p-1"}),
            json!({"question": [["yes"]], "custom_text": [null], "extra": 1}),
            json!({"reject": true, "why": "no"}),
        ] {
            let failure = parse_answer(&stray)
                .err()
                .unwrap_or_else(|| panic!("{stray} carries a stray key and must be refused"));
            assert_eq!(failure.code(), "bad_request", "{stray}");
            assert!(
                failure.message().contains("nothing else"),
                "the refusal names what is wrong: {}",
                failure.message()
            );
        }
        // The contract's own tagged spelling is not this door's grammar at all:
        // a `kind` key names no form, so the payload is refused, extra keys or
        // not — the unit-variant leniency serde has for `{"kind":"reject",…}`
        // never reaches an adapter through this door.
        for tagged in [
            json!({"kind": "reject", "extra": 1}),
            json!({"kind": "reject"}),
        ] {
            assert_eq!(
                parse_answer(&tagged).err().map(|f| f.code()),
                Some("bad_request"),
                "{tagged}"
            );
        }
        // `custom_text` is the one modifier `question` takes.
        assert!(parse_answer(&json!({"question": [["yes"]], "custom_text": [null]})).is_ok());
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
    // These drive the REAL `Lanes` — the real `OpencodeSource` and its lane,
    // the real watcher, the real staging — against two doubles: an in-process
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
    /// The kind most cells open — opencode's lanes are the roost-stamped ones.
    const OC: &str = "opencode";
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
        /// When set, `ensure` FAILS — a tunnel that will not come back up.
        fail_ensure: AtomicBool,
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
            if self.log.fail_ensure.load(SeqCst) {
                return Err(ForwardError("the ssh child will not start".to_string()));
            }
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
        /// The machine's craze source and the hostIds it lists, for the cells
        /// about the craze namespace. `None` everywhere else.
        craze: Mutex<Option<(CrazeSource, Vec<String>)>>,
    }

    impl LaneMachines for FakeMachines {
        fn agent_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp> {
            if machine == MACHINE {
                lock(&self.lanes).clone()
            } else {
                BTreeMap::new()
            }
        }

        fn craze_lanes(&self, machine: &str) -> BTreeMap<String, AgentLaneStamp> {
            let craze = lock(&self.craze);
            let Some((_, ids)) = craze.as_ref().filter(|_| machine == MACHINE) else {
                return BTreeMap::new();
            };
            ids.iter()
                .map(|id| {
                    (
                        id.clone(),
                        AgentLaneStamp {
                            kind: CRAZE.to_string(),
                            session_id: id.clone(),
                            server_url: String::new(),
                        },
                    )
                })
                .collect()
        }

        fn craze_source(&self, machine: &str) -> Result<(CrazeSource, u64), String> {
            lock(&self.craze)
                .as_ref()
                .filter(|_| machine == MACHINE)
                .map(|(source, _)| (source.clone(), 1))
                .ok_or_else(|| format!("{machine} has no craze source"))
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
                LaneEvent::Capabilities { .. } => "capabilities",
                LaneEvent::Settings { .. } => "settings",
                LaneEvent::Stale { .. } => "stale",
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

    /// A `Lanes` on the doubles, with `rows` as the machine's stamped lanes and
    /// tunnels landing on `port`.
    fn lanes_on(
        port: u16,
        rows: BTreeMap<String, AgentLaneStamp>,
    ) -> (Arc<Lanes>, Arc<ForwardLog>, Arc<Recorder>) {
        lanes_with_craze(port, rows, None)
    }

    /// [`lanes_on`], the machine also having a craze source listing `craze`'s
    /// hostIds.
    fn lanes_with_craze(
        port: u16,
        rows: BTreeMap<String, AgentLaneStamp>,
        craze: Option<(CrazeSource, Vec<String>)>,
    ) -> (Arc<Lanes>, Arc<ForwardLog>, Arc<Recorder>) {
        let log = Arc::new(ForwardLog::default());
        let recorder = Arc::new(Recorder::default());
        let machines = Arc::new(FakeMachines {
            lanes: Mutex::new(rows),
            port,
            log: Arc::clone(&log),
            reach: ReachKind::Ssh(fake_entry()),
            craze: Mutex::new(craze),
        });
        let sink: EventSink = {
            let recorder = Arc::clone(&recorder);
            Arc::new(
                move |_machine: &str, _kind: &str, _session: &str, event: &LaneEvent| {
                    recorder.record(event)
                },
            )
        };
        let lanes = Arc::new(Lanes::with_sink(
            tokio::runtime::Handle::current(),
            sink,
            machines,
        ));
        (lanes, log, recorder)
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
            .open(MACHINE, "claude", "ses_x")
            .await
            .expect_err("this build speaks opencode, not claude");
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
                .open(MACHINE, "claude", "ses_x")
                .await
                .expect_err("still refused")
                .code(),
            "unsupported_lane"
        );
    }

    /// **The retired gx lane is refused the same way, with nothing reserved,
    /// nothing registered and nothing built on the way to finding that out**
    /// (plan 025 C1, shed#390).
    ///
    /// This is the kept negative control for the retirement: a `gx`-stamped row
    /// is `unsupported_lane`, exactly as any other unknown kind is, and the
    /// refusal is total — no tunnel, no registry entry (committed OR pending),
    /// no adapter client, no subscription. It goes red the moment `"gx"` is
    /// restored to [`LANE_KINDS`] without restoring the adapter this crate
    /// deleted — the pre-reserve guard would then let the open past the kind
    /// check and into `transport()`, reserving a tunnel (and, were the match
    /// arm not `unreachable` by construction, going on to build a client and
    /// subscribe) for a kind this build cannot actually speak to.
    #[tokio::test]
    async fn a_gx_stamped_row_is_unsupported_lane_with_no_forward_reserved() {
        let (lanes, log, recorder) = lanes_on(1, lane_rows_of("gx", &["ses_gx"]));
        let key: Key = (MACHINE.to_string(), "gx".to_string(), "ses_gx".to_string());
        // Snapshotted, not asserted against zero: this counter is a single
        // process-wide static shared by every test in this binary, and
        // `cargo test` runs them concurrently. The claim is "this `open` built
        // no adapter", i.e. no DELTA across the call — not "nothing else in
        // the suite ever has".
        let builds_before = ADAPTER_BUILDS.load(SeqCst);

        let failure = lanes
            .open(MACHINE, "gx", "ses_gx")
            .await
            .expect_err("the gx adapter left this app in plan 025 C1");

        // (c) the refusal itself.
        assert_eq!(failure.code(), "unsupported_lane");
        assert!(
            failure.message().contains("\"gx\""),
            "the refusal names the kind: {}",
            failure.message()
        );

        // The transport half (already covered, kept as-is).
        assert_eq!(log.built.load(SeqCst), 0, "no tunnel was reserved");
        assert!(forward_users(&lanes).is_none(), "no tunnel was registered");

        // (a) no trace in the registry — neither a committed entry NOR a
        // pending-open declaration survives a refusal that happened before
        // `declare` ever ran. Read under the same lock `entry`/`declare` use,
        // so this is the registry's own state, not an inference from a
        // method that happens to read it.
        {
            let inner = lock(&lanes.inner);
            assert!(
                !inner.entries.contains_key(&key),
                "a refused open must not leave a committed lane entry"
            );
            assert!(
                !inner.pending.contains_key(&key),
                "a refused open must not leave a pending-open declaration either"
            );
        }

        // (b) no adapter was built, and therefore nothing was there to
        // subscribe: `spawn_pump` (the only caller of `AgentLane::subscribe`)
        // is only ever invoked on the `client` this same match produces, so a
        // build count that did not move proves subscribe was never reached
        // either — there is no client in this run for it to have been called
        // on.
        assert_eq!(
            ADAPTER_BUILDS.load(SeqCst),
            builds_before,
            "no adapter client was constructed for the refused kind"
        );

        // The pre-existing event-level check, kept: no `Ready` ever reached
        // the sink either.
        assert_eq!(
            recorder.count("ready"),
            0,
            "no subscription was started, so no lane event was ever emitted"
        );
    }

    /// **A row that changes AGENT on the same port is a different lane.**
    ///
    /// The entry used to be keyed on `server_url` alone, which cannot see this:
    /// a tab that restarts as a different kind on the port opencode had would
    /// have kept the old entry, and the panel would have gone on pumping an
    /// opencode client at the wrong server. The stamp is compared whole.
    #[tokio::test]
    async fn an_entry_is_evicted_when_the_kind_changes_under_a_stable_url() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _recorder) = lanes_for(&fake, &["ses_a"]);
        lanes.open(MACHINE, OC, "ses_a").await.expect("opens");
        assert_eq!(forward_users(&lanes), Some(1));

        // Same session, same URL, different agent.
        lanes.reconcile(MACHINE, &lane_rows_of("cursor", &["ses_a"]));
        assert!(
            forward_users(&lanes).is_none(),
            "the opencode entry (and its tunnel) did not survive the kind change"
        );
        wait_for("the tunnel's child to be reaped", || {
            (!log.alive.load(SeqCst)).then_some(())
        })
        .await;
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
            .messages(MACHINE, OC, session)
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
        tokio::spawn(async move { lanes.open(MACHINE, OC, session).await })
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
                .open("no-such-machine", OC, &format!("ses_{i}"))
                .await
                .expect_err("a machine with no rows has no lane");
            assert_eq!(failure.code(), "no_lane", "{}", failure.message());
            // … and a real one whose rows do not name this session (the churn
            // case: yesterday's tab ids, replayed).
            let failure = lanes
                .open(MACHINE, OC, &format!("churned_{i}"))
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
        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        assert!(
            lock(&lanes.gates).is_empty(),
            "a committed open kept its gate"
        );
        lanes.close(MACHINE, OC, "ses_a");
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
        lanes.close(MACHINE, OC, "ses_a");
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

        lanes.close(MACHINE, OC, "ses_a");
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
                .open(MACHINE, OC, "ses_a")
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
        lanes.close(MACHINE, OC, "ses_a");
        assert_eq!(
            forward_users(&lanes),
            Some(1),
            "closing one lane took the other's tunnel with it"
        );
        assert_eq!(log.dropped.load(SeqCst), 0);
        lanes.close(MACHINE, OC, "ses_b");
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

        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
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

    /// **Capabilities ride `lane.messages`, not `lane.open`** (plan 025 §3.2.6).
    /// `lane.open` answers the session row alone; once the seed swaps in, the
    /// staged view's capabilities — opencode's fixed row, carried by every seed —
    /// are on `lane.messages`, beside `settings` (none: opencode has none) and
    /// the two lifecycle facts.
    #[tokio::test]
    async fn capabilities_and_settings_ride_lane_messages_not_lane_open() {
        let fake = one_session("ses_a").await;
        let (lanes, _log, _events) = lanes_for(&fake, &["ses_a"]);

        let opened = lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        assert_eq!(opened["session"]["id"], "ses_a");
        assert!(
            opened.get("capabilities").is_none(),
            "lane.open answers {{session}} alone: {opened}"
        );
        wait_for("the first generation to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;
        let view = lanes.messages(MACHINE, OC, "ses_a").expect("lane.messages");
        assert_eq!(
            view["capabilities"],
            serde_json::to_value(shed_opencode::opencode_capabilities()).expect("caps encode"),
            "the seed's capabilities, from the staged view: {view}"
        );
        assert_eq!(view["settings"], Value::Null, "opencode has no settings");
        assert_eq!(view["stale"], Value::Null);
        assert_eq!(view["ended"], false);
    }

    /// **A tunnel that will not come back under a live lane is STALE, not an
    /// end — and the lane comes back on its own once the tunnel does.** The pump
    /// has already scheduled its next attempt, so it must not mark the view
    /// `ended` (which a client reads as "reopen me"). Its `Stale` lands inside
    /// the reconnect's seed and abandons it (a loss before `Ready` reseeds —
    /// `shed_app::lane_view`), so the pump also DROPS that subscription and
    /// resubscribes once the tunnel is back: recovery arrives as the adapter's
    /// fresh `Reset … Ready`, with no manual reconnect (review, sol confirm).
    /// Kept instead, the subscription would finish the abandoned seed and stream
    /// on into a view that takes none of it — stale forever on a healthy
    /// connection, which is what this cell's bound turns red.
    #[tokio::test]
    async fn a_failed_tunnel_under_a_live_lane_is_stale_and_recovers_by_itself() {
        let fake = one_session("ses_a").await;
        let (lanes, log, events) = lanes_for(&fake, &["ses_a"]);
        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        wait_for("the first generation to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;
        let event_dials = || {
            fake.get_paths()
                .iter()
                .filter(|p| p.starts_with("/event"))
                .count()
        };

        // The agent's stream drops, and the tunnel under it will not come back.
        log.fail_ensure.store(true, SeqCst);
        fake.close_streams();
        wait_for("the failed re-ensure to be reported", || {
            (events.count("stale") >= 1).then_some(())
        })
        .await;
        assert_eq!(events.count("down"), 0, "a retried failure is never a Down");
        let view = lanes.messages(MACHINE, OC, "ses_a").expect("lane.messages");
        assert_eq!(view["ended"], false, "the lane did not end: {view}");
        assert!(
            !view["stale"].is_null(),
            "and it says it is not live: {view}"
        );
        let dials_while_down = event_dials();

        // The tunnel recovers. Nothing else happens — no stream close, no
        // reopen — and the lane must come back by itself, through a FRESH
        // subscription (a new `/event` dial), not the abandoned one.
        log.fail_ensure.store(false, SeqCst);
        wait_for(
            "the lane to reseed and clear its stale mark on its own (a kept \
             subscription streams into the abandoned seed forever)",
            || {
                let view = lanes.messages(MACHINE, OC, "ses_a").ok()?;
                (view["generation"].as_u64() >= Some(2) && view["stale"].is_null()).then_some(())
            },
        )
        .await;
        assert!(
            event_dials() > dials_while_down,
            "the recovery rode a fresh subscription: {} /event dials before, {} after",
            dials_while_down,
            event_dials()
        );
        let view = lanes.messages(MACHINE, OC, "ses_a").expect("lane.messages");
        assert_eq!(view["ended"], false, "and the lane was never ended: {view}");
        assert!(
            view["capabilities"].is_object(),
            "the fresh seed's capabilities are live: {view}"
        );
        wait_for("the abandoned subscription's stream to be released", || {
            (fake.stream_count() == 1).then_some(())
        })
        .await;
    }

    /// The `Down` reasons the pump never resubscribes after: the three plan 025
    /// §3.3.5 names, and nothing else.
    #[test]
    fn only_a_final_down_stops_the_pump() {
        for reason in [
            "unknown_session",
            "session_closed",
            "start_failed",
            "start_failed: acp: agent exited",
        ] {
            assert!(down_is_final(reason), "{reason:?} is final");
        }
        for reason in [
            "unreachable",
            "re-attach bound",
            "craze unavailable: not installed",
            "the opencode event stream ended",
            "closed",
            "session_closed_soon",
            "",
        ] {
            assert!(
                !down_is_final(reason),
                "{reason:?} is worth another attempt"
            );
        }
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

        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        wait_for("the lane to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;

        lanes.close(MACHINE, OC, "ses_a");
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

    /// **The lane key carries the kind** (plan 025 §3.6.4): a craze hostId and
    /// an opencode session id are separate namespaces, so the SAME id on one
    /// machine is two lanes — opening one never answers for, evicts or closes
    /// the other.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_craze_lane_and_an_opencode_lane_sharing_an_id_are_two_lanes() {
        use shed_core::lane::AgentSource as _;
        use shed_craze::testing::{full_hub_capabilities, roster_row, ScriptedDial};

        const ID: &str = "ses_a";
        let fake = one_session(ID).await;
        let (dial, mut conns) = ScriptedDial::new();
        let source = CrazeSource::new(dial, "shed-desktop-test");
        let _roster = source.subscribe().await.expect("subscribe");
        let mut hub = conns.recv().await.expect("the roster dialled");
        hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
        hub.subscribed(
            "sub-1",
            "0a1b2c3d4e5f",
            json!([roster_row(ID, "craze-1", "/w", json!({"title": "craze"}))]),
        )
        .await;
        wait_for("the source lists the row", || source.listed(ID).map(|_| ())).await;

        let (lanes, _log, _recorder) = lanes_with_craze(
            fake.addr().port(),
            lane_rows(&[ID]),
            Some((source, vec![ID.to_string()])),
        );
        lanes
            .open(MACHINE, OC, ID)
            .await
            .expect("the opencode lane");
        lanes
            .open(MACHINE, CRAZE, ID)
            .await
            .expect("the craze lane");
        assert!(
            lanes.is_open(MACHINE, OC, ID),
            "the opencode lane survived the craze open"
        );
        assert!(lanes.is_open(MACHINE, CRAZE, ID));
        assert_eq!(lock(&lanes.inner).entries.len(), 2, "two lanes, one id");

        lanes.close(MACHINE, CRAZE, ID);
        assert!(!lanes.is_open(MACHINE, CRAZE, ID));
        assert!(
            lanes.is_open(MACHINE, OC, ID),
            "closing the craze lane left the opencode one"
        );
        // And a roost reconcile that drops the opencode row leaves a craze one
        // alone — only roost-stamped lanes are roost's to reconcile.
        lanes
            .open(MACHINE, CRAZE, ID)
            .await
            .expect("the craze lane again");
        lanes.reconcile(MACHINE, &BTreeMap::new());
        assert!(
            !lanes.is_open(MACHINE, OC, ID),
            "the roost-stamped lane went with its row"
        );
        assert!(
            lanes.is_open(MACHINE, CRAZE, ID),
            "the craze lane is not roost's to evict"
        );
        lanes.evict_craze(MACHINE, 2, &[ID.to_string()]);
        assert!(
            lanes.is_open(MACHINE, CRAZE, ID),
            "another generation's eviction leaves it alone"
        );
        lanes.evict_craze(MACHINE, 1, &[ID.to_string()]);
        assert!(!lanes.is_open(MACHINE, CRAZE, ID), "evict_craze retires it");
    }
}
