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
//! machine row  --agent_lane{kind, session_id, server_url?}-->  Lanes::open
//!                                                           |
//!   "craze"     -> the host's CrazeSource, .open(hostId)     |  (no reach, no forward)
//!   "opencode"  -> ReachKind::Local   -> dial server_url     |
//!                  ReachKind::Ssh(e)  -> SshForward::reserve_for
//!                  then OpencodeSource::new(url).open(id)   |
//!   other       -> UnsupportedLane                           |
//!                                                           v
//!                                               Arc<dyn AgentLane>
//!                                                           |
//!                                        subscribe() -> Reset … Ready … frames
//!                                                           |
//!                                      LaneView (staged, then swapped) + `lane-event`
//! ```
//!
//! # One trait, two adapters — and the line the dispatch draws
//!
//! [`LaneEntry`] holds an `Arc<dyn AgentLane>`, and the ONLY place in this app
//! that builds a concrete adapter is [`Lanes::open`] — its craze branch, and the
//! roost-stamped `match` it hands every other kind to. That is
//! the whole point of plan 017: everything below the match — the pump, the view,
//! the tunnel bookkeeping, the `lane.*` verbs — is written against the contract,
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
//! be torn down by it — and the craze source's own news governs the craze
//! entries ([`Lanes::craze_news`]): a row that left it (a `Removed`, a reseed
//! that dropped it, the hub gone, its tab ended) starts the lane's DEPARTURE
//! ([`Lanes::evict_craze`], below), a row listed again ends one
//! ([`Lanes::craze_returned`]), and the host's removal ends every lane of its
//! generation at once ([`Lanes::evict_craze_generation`]). A craze entry is
//! filed under the GENERATION of the source it was opened through, and every
//! piece of news names its generation: the news of a source that has since
//! been replaced (the host removed and registered again) never touches a lane
//! opened through the new one. An open re-reads the host's current generation
//! after it declares itself, so a removal that published its sweep before the
//! declaration — with nothing yet to cancel — still stops the open.
//!
//! # A craze lane outlives its row by a grace, then ends visibly (plan 032 §3.1)
//!
//! craze 0.2.0 drops a stopping host's row on the hub's next 1 s roster
//! round, usually BEFORE the session's last events and its
//! `reset{session_closed}` arrive on the lane's own connection — an
//! attachment made before the stop runs on to the session's end (craze's
//! protocol.md), and nothing orders the two connections. Retiring the lane
//! the moment its row left (as this module did through plan 029) froze the
//! panel on its last frame: no "ended" banner, tools still "running", an
//! enabled composer, and `no_lane` on the next read. So an entry has an
//! explicit lifecycle, [`EntryState`]:
//!
//! | state | how it got here | the pump | verbs | leaves on |
//! |---|---|---|---|---|
//! | **Live** | committed by `open`; a return | applies + emits | sent | its row leaving (→ Departing), a final `Down` (→ Finished) |
//! | **Departing{token, cause}** | its row left the source | applies + emits | sent | the row listed again (→ Live), a final `Down`, the grace's end, the host removed (→ Finished) |
//! | **Finished{reason}** | a final `Down` — the adapter's, or this layer's `session_gone`/`hub_gone`/`host_removed` | stopped | refused `lane_ended` | never (one-way); `open` on a resolving row replaces it |
//!
//! * **The grace** ([`CRAZE_DEPART_GRACE`], 10 s) is one task per departure,
//!   holding only a `Weak` to the entry. It ends at its deadline or at the
//!   entry's `finished` notification, whichever is first; at the deadline it
//!   ends the lane — `Down{session_gone}` for a row the roster removed,
//!   `Down{hub_gone}` for a hub that went (the source Dormant) — and RETIRES
//!   it. A departure has an identity (its `token`): a duplicate removal never
//!   extends one, a return cancels it (the stale task finds the token gone),
//!   and a removal after a return starts a fresh grace with a new token.
//! * **News is applied in the order it HAPPENED, not the order it arrives**
//!   ([`NewsMark`], C1 re-review). Departures, returns and the host's removal
//!   come from three publishers on their own tasks — the source's fold, an End
//!   tab, the removal — each publishing after it released the roost-host
//!   lock its news was computed under. So each piece carries a number minted
//!   under that lock ([`CrazeNews`]'s `seq`), and a lane (or an open in
//!   flight) takes only news newer than the last it took: a return delayed
//!   past an End tab's departure cannot revive the lane and lose its grace's
//!   end, and a departure delayed past a return cannot depart it again.
//! * **Atomic, under one lock per entry.** Every transition, AND every frame
//!   the pump applies and emits, happens under the entry's `state` lock
//!   ([`LaneCore`]), and nothing is applied once the entry is `Finished` (or
//!   the app is closing), so a late `Ready` can never un-end a lane and
//!   exactly one terminal `Down` is ever emitted. **The lock order is `inner`
//!   (the registry) → an entry's `state` → its `view`**; nothing takes
//!   `inner` while holding `state`.
//! * **Retired is not removed.** A finished entry stays in the registry,
//!   readable from its view (`lane.messages`, `lane.approvals`, and
//!   `lane.settings` from the view's last settings), with its adapter client,
//!   pump and forward share released ([`LaneEntry::retire`] — or, at the
//!   session's own final `Down`, by the pump on its way out,
//!   [`LaneCore::release`]) — so a finished lane holds no dial, no
//!   `SshExec`, no tunnel, whatever finished it. Only `close` (and the
//!   shutdown, and a roost-stamped eviction) removes an entry.
//! * **A verb is admitted, or refused** ([`Lanes::admit`]): the lifecycle
//!   check and the adapter clone are one `state` acquisition, so a verb
//!   admitted before the lane finished runs its course on its own clone, and
//!   one after is refused `lane_ended` — never `no_lane` for a client a
//!   finish released.
//! * **The shutdown's start fences first** ([`Inner::close`]): every entry is
//!   marked closing under its `state` before the registry's `closing` flag
//!   is set, so no frame of any lane is emitted once the shutdown began.
//! * **`finished` is not `view.ended`.** Every `Down` sets the view's `ended`,
//!   a retryable one included — the pump resubscribes after it and a later
//!   `Ready` clears it. `lane_ended`, `lane.messages`' `finished` and the
//!   panel's disabled composer all key on the entry state.
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
use shed_app::spawn_gate::ShuttingDown;
use shed_core::config::MachineEntry;
use shed_core::lane::{
    AgentLane, AgentSource, LaneAnswer, LaneDecision, LaneError, LaneEvent, LaneSession,
    LaneSettingChange, SendMode,
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

/// How long [`Lanes::shutdown`] waits for its aborted pumps to have ended — an
/// aborted task ends at its next poll, so this bounds only a pump stuck in
/// something that never yields.
const PUMP_JOIN_WAIT: Duration = Duration::from_secs(1);

/// The [`LaneEvent::Down`] reason an adapter ends a lane with when the session
/// does not exist (opencode: a reseed answered 404; craze: `unknown_session` on
/// connect or attach).
const DOWN_UNKNOWN_SESSION: &str = "unknown_session";

/// How long a craze lane keeps its own connection after its row left the
/// source (plan 032 §3.1) — the window in which a stopping session's last
/// events and its `reset{session_closed}` still land.
///
/// Why 10 s: craze's roster round is 1 s and a stop's tool teardown takes
/// seconds; a host that died with no final event gives the lane's watcher a
/// final `unknown_session` on its redial anyway, so this only bounds the worst
/// case. Tests shorten it per layer ([`Lanes::with_depart_grace`]).
pub const CRAZE_DEPART_GRACE: Duration = Duration::from_secs(10);

/// The `Down` reason THIS layer ends a departing lane with when its grace
/// runs out and its row left the roster (`Removed`, a reseed that dropped it,
/// its tab ended) — the session is gone and no final event said so.
pub const DOWN_SESSION_GONE: &str = "session_gone";
/// …and when the row left with the hub itself (the source went Dormant).
pub const DOWN_HUB_GONE: &str = "hub_gone";
/// The `Down` reason a lane ends with, at once and with no grace, when its
/// HOST is removed (plan 032 §3.1.1 item 5): its source — the one thing that
/// could have listed the row again — is gone.
pub const DOWN_HOST_REMOVED: &str = "host_removed";

/// Whether a lane that ENDED with this `Down` reason is gone for good — so the
/// pump stops instead of resubscribing, and the entry is `Finished`.
///
/// `shed_core::lane`'s module doc (plan 025 §3.3.5) names three: the session
/// does not exist (`unknown_session`), it was closed (`session_closed`, craze's
/// end after a stop), or it never started (`start_failed:<cause>`). No amount
/// of reconnecting changes any of them. Plan 032 adds this layer's own three
/// ([`DOWN_SESSION_GONE`], [`DOWN_HUB_GONE`], [`DOWN_HOST_REMOVED`]), which
/// only this layer emits and only as an entry's last frame. Every other `Down`
/// is worth another attempt (the agent restarted, the tunnel blipped, a bound
/// ran out).
fn down_is_final(reason: &str) -> bool {
    reason == DOWN_UNKNOWN_SESSION
        || reason == "session_closed"
        || reason == "start_failed"
        || reason.starts_with("start_failed:")
        || reason == DOWN_SESSION_GONE
        || reason == DOWN_HUB_GONE
        || reason == DOWN_HOST_REMOVED
}

/// Why a craze lane's row left its source — what its grace ends it with
/// (plan 032 PC14g).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Departure {
    /// The roster let the row go: a `Removed`, a `Ready` swap that no longer
    /// lists it, or the tab of a TUI-hosted session ended. Expires as
    /// [`DOWN_SESSION_GONE`].
    Removed,
    /// The hub went (the find-only probe found none: the source is Dormant),
    /// taking every row with it. Expires as [`DOWN_HUB_GONE`].
    HubGone,
}

impl Departure {
    /// The `Down` reason a departure of this cause expires with.
    fn reason(self) -> &'static str {
        match self {
            Departure::Removed => DOWN_SESSION_GONE,
            Departure::HubGone => DOWN_HUB_GONE,
        }
    }
}

/// What a craze source's change means for the lanes opened through it — the
/// payload of [`crate::roost_hosts::OnCrazeNews`], dispatched by
/// [`Lanes::craze_news`] (plan 032 §3.1.1 items 2, 3, 5).
///
/// Every piece carries its `seq`: the source's news sequence number, minted
/// under the roost-host layer's state lock when the news was computed
/// ([`crate::craze::CrazeState::mint_news`]) — the order it HAPPENED in,
/// which the lane layer applies it by ([`Lanes::craze_news`]'s news fence),
/// whatever order its publishers' tasks deliver it in.
#[derive(Debug, Clone, Copy)]
pub enum CrazeNews<'a> {
    /// These rows left the source: their lanes DEPART ([`Lanes::evict_craze`]).
    Departed {
        host_ids: &'a [String],
        cause: Departure,
        seq: u64,
    },
    /// These rows are newly listed (a `Session` upsert of an absent host, a
    /// `Ready` that lists it): a departing lane on one is LIVE again
    /// ([`Lanes::craze_returned`]).
    Returned { host_ids: &'a [String], seq: u64 },
    /// The host was removed: every lane of the generation ends now, with no
    /// grace ([`Lanes::evict_craze_generation`]).
    HostRemoved { seq: u64 },
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
/// source's, and its news ([`Lanes::craze_news`]) is theirs.
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
/// concrete `AgentLane` adapter (the craze branch's `CrazeSource::open` and the
/// roost-stamped match's `OpencodeSource::new(…).open(…)`, both under
/// [`Lanes::open`]). Exists so a control can assert "no adapter was
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
    /// A verb whose answer was lost — its connection dropped while it was in
    /// flight, or it was never answered — so it MAY have run, and it is never
    /// resent ([`shed_craze::is_outcome_unknown`], the craze lane's "outcome
    /// unknown"). A code of its own, `outcome_unknown` (the `craze.*` ops'
    /// too), because a client does something different about it than about a
    /// refusal and must not have to read a message to know which it got: the
    /// settings sheet shows such a change "not confirmed" until the session's
    /// next `Settings` says what it is at (plan 025 §3.10), where a refusal is
    /// shown as one.
    OutcomeUnknown(String),
    /// The app is quitting (plan 026 §3.3): the lane layer has closed, and no
    /// lane opens again. `shutting_down`, the spawn gate's own code.
    ShuttingDown,
    /// The lane is FINISHED (plan 032 §3.1.1 item 8): its session ended — a
    /// final `Down` arrived, or this layer ended it (its grace ran out, its
    /// host was removed) — so a verb has nothing to reach. `lane_ended`, "the
    /// session has ended", at once: a verb on an ended craze lane used to wait
    /// out the adapter's 10 s reconnect wait and fail `unavailable`, which
    /// reads as "try again". The transcript stays readable.
    Ended,
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
            LaneFailure::OutcomeUnknown(_) => "outcome_unknown",
            LaneFailure::ShuttingDown => shed_app::spawn_gate::SHUTTING_DOWN,
            LaneFailure::Ended => "lane_ended",
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
            LaneFailure::NoLane(m) | LaneFailure::OutcomeUnknown(m) => m.clone(),
            LaneFailure::UnsupportedLane(kind) => format!(
                "this build has no adapter for agent lanes of kind {kind:?} \
                 (it speaks {})",
                LANE_KINDS.join(" and ")
            ),
            LaneFailure::ShuttingDown => ShuttingDown.to_string(),
            LaneFailure::Ended => "the session has ended".to_string(),
            LaneFailure::Lane(e) => e.to_string(),
        }
    }
}

impl From<LaneError> for LaneFailure {
    /// The adapter's refusal under its own code — but an "outcome unknown" (a
    /// craze verb whose answer was lost) under `outcome_unknown`, recognised
    /// by the one test that recognises it, never by this layer reading text.
    fn from(e: LaneError) -> Self {
        if shed_craze::is_outcome_unknown(&e) {
            return LaneFailure::OutcomeUnknown(e.to_string());
        }
        LaneFailure::Lane(e)
    }
}

/// `lane.set_title`'s answer from the adapter's (plan 029 P19, E4): `{}` on a
/// rename that took; `{warning: <the message>}` on one that TOOK and that
/// craze could not record (`index_write`) — the adapter words it
/// `"renamed, but craze could not record it: <cause>"` and
/// [`shed_craze::index_write_cause`] is the one test that recognises it, so
/// this layer never reads text. The header already shows the new title (the
/// delta carried it), so a client shows the warning beside it, clears the
/// draft, and neither rolls back nor retries. Every other error stays an
/// error under its own code — `outcome_unknown` included.
pub fn renamed(result: Result<(), LaneError>) -> Result<Value, LaneFailure> {
    match result {
        Ok(()) => Ok(json!({})),
        Err(e) if shed_craze::index_write_cause(&e).is_some() => {
            Ok(json!({ "warning": e.to_string() }))
        }
        Err(e) => Err(e.into()),
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
    /// ([`Lanes::craze_news`]) and which `open` fences an adoption by.
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

    /// The default's forward, spawning only inside the app's spawn gate (plan
    /// 026 §3.3) and registered with it.
    fn forward(
        &self,
        entry: &MachineEntry,
        remote_port: u16,
    ) -> Result<Box<dyn MachineForward>, String> {
        let forward =
            SshForward::reserve_for(entry.clone(), remote_port).map_err(|e| e.to_string())?;
        Ok(Box::new(match self.spawn_gate() {
            Some(gate) => forward.with_spawn_gate(gate),
            None => forward,
        }))
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
    /// The newest craze news this open took ([`NewsMark`]): a departure older
    /// than a return it already took does not cancel it, and the entry it
    /// commits starts its own fence HERE ([`Life::news`]) — so news that
    /// reached the open is never undone, after the commit, by older news
    /// delivered late.
    news: NewsMark,
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

/// Where an entry is in its life (plan 032 §3.1.1 item 1; the module doc's
/// table). Read and written only under its [`LaneCore::state`] lock.
#[derive(Debug, Clone, PartialEq, Eq)]
enum EntryState {
    /// Its pump runs and its row is listed (or it is not a craze lane).
    Live,
    /// Its row left the source; its grace task holds `token`. `cause` is
    /// what the grace ends it with ([`Departure::reason`]).
    Departing { token: u64, cause: Departure },
    /// Its pump has stopped for good: a final `Down` (the adapter's, or one
    /// this layer emitted). One-way.
    Finished { reason: String },
}

/// [`LaneCore::state`]'s contents: the entry's state, and the shutdown's mark.
#[derive(Debug)]
struct Life {
    state: EntryState,
    /// Set, under this lock, when the entry LEAVES the registry or the
    /// shutdown BEGINS: by [`Inner::close`] — the shutdown's start
    /// ([`Lanes::begin_closing`], and [`Lanes::shutdown`] before it drains),
    /// every entry fenced BEFORE the registry's own flag is set (plan 032
    /// §3.1.1 item 4) — and by `close` and every eviction as they remove it.
    /// From then on nothing is applied or emitted for this entry — not a
    /// pump's frame, not a grace's expiry — so no `lane-event` follows the
    /// shutdown's start or speaks for a lane that is no longer registered.
    /// Never cleared.
    closing: bool,
    /// The newest craze news this entry took ([`NewsMark`], C1 re-review) —
    /// started from its open's ([`Pending::news`]) at the commit. Read and
    /// advanced only under this lock, by [`Lanes::craze_news`]'s handlers,
    /// which hold the registry's first (the lock order).
    news: NewsMark,
}

/// **The craze news fence** (C1 re-review): the sequence number of the newest
/// piece of craze news ([`CrazeNews`]'s `seq`) a lane — a committed entry
/// ([`Life::news`]) or an open in flight ([`Pending::news`]) — has TAKEN.
///
/// **Why there is one.** Craze news has three publishers on three kinds of
/// task — the source's fold (departures and returns), an End tab (a
/// departure, from the IPC op's task) and the host's removal — and each
/// publishes after releasing the state lock its news was computed under, so
/// a piece can reach this layer AFTER a piece computed later. Unfenced, a
/// reseed's return delayed past an End tab's departure revived the departing
/// lane and cancelled its grace — its 10 s end lost when no final event
/// followed — and a departure delayed past a return started a departure the
/// row's return had already answered.
///
/// **The rule.** A piece is minted a number, strictly increasing per source,
/// at the moment it is computed, under that lock
/// ([`crate::craze::CrazeState::mint_news`]). A lane takes a piece only if
/// its number is NEWER than the last it took, and records it — whether or not
/// it changed the lane's state (a duplicate departure is still newer news
/// than a return minted before it). Old news is dropped, so it never undoes
/// newer news, in either direction. It is judged AFTER the generation fence:
/// another source's numbers are another sequence, and its news never reaches
/// this mark at all.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
struct NewsMark(u64);

impl NewsMark {
    /// Take news `seq` if it is newer than everything this lane took —
    /// answering whether it did. A piece not taken must not be applied.
    fn take(&mut self, seq: u64) -> bool {
        if seq <= self.0 {
            return false;
        }
        self.0 = seq;
        true
    }
}

impl Life {
    /// Whether a frame may still be applied and emitted.
    fn admits(&self) -> bool {
        !self.closing && !matches!(self.state, EntryState::Finished { .. })
    }
}

/// What one entry's pump, its grace task and the registry SHARE: the
/// lifecycle and the view, the notification that the lifecycle ended, and the
/// TRANSPORT the lane holds — its adapter client and its tunnel share.
///
/// Its own `Arc` rather than fields of [`LaneEntry`], because the pump is
/// spawned before the entry exists (and must not hold the entry: dropping the
/// entry is what ends the pump) — this holds no reference to either task, so
/// there is no cycle. The transport is here, not on the entry, so that the
/// pump can let it go itself when the session's own final `Down` finishes the
/// lane ([`LaneCore::release`], C1 review): a finished entry holds no
/// transport whichever path finished it.
///
/// **The lock order is `inner` → `state` → `view`** (module doc): every
/// method below takes `state`, then `view` under it, and neither ever takes
/// the registry's. `client` and `forward` are leaf locks, never held while
/// another is taken — and a tunnel share is never dropped under `inner` or
/// `state`, since giving it back takes `inner`.
struct LaneCore {
    /// The lifecycle — the plan's "entry `state`". Every frame the pump applies
    /// and emits is applied and emitted UNDER this lock, so a transition and a
    /// frame can never interleave.
    state: Mutex<Life>,
    /// The staged view `lane.messages` reads.
    view: Mutex<LaneView>,
    /// Notified (after the lock is released) when the entry becomes
    /// `Finished` — what lets a departure's grace task end early.
    finished: tokio::sync::Notify,
    /// The adapter, as the CONTRACT. Every verb reaches the agent through
    /// this trait object; the concrete type was chosen once, in
    /// [`Lanes::open`]'s match, and is deliberately not knowable from here.
    ///
    /// `None` once RELEASED (plan 032 PC4, [`LaneCore::release`]): by
    /// [`LaneEntry::retire`], or by the pump at the session's own final
    /// `Down` — so a finished entry kept for its transcript holds no craze
    /// dial, no `SshExec`, no tunnel. A verb admitted before the release
    /// finishes with the clone it took ([`LaneCore::client`]).
    client: Mutex<Option<Arc<dyn AgentLane>>>,
    /// This entry's share of the tunnel it rides, if any. `None` for a local
    /// lane, and once released. Taken rather than waited for: an op holding a
    /// clone of the entry (or of this core) across an await must not be able
    /// to delay a closed panel's `ssh` child from dying.
    forward: Mutex<Option<ForwardShare>>,
}

impl LaneCore {
    /// A live lane's core, holding its transport — `client` and its tunnel
    /// share (`None` for a local lane, and for a bare core in a test).
    fn new(client: Option<Arc<dyn AgentLane>>, forward: Option<ForwardShare>) -> Arc<LaneCore> {
        Arc::new(LaneCore {
            state: Mutex::new(Life {
                state: EntryState::Live,
                closing: false,
                news: NewsMark::default(),
            }),
            view: Mutex::new(LaneView::default()),
            finished: tokio::sync::Notify::new(),
            client: Mutex::new(client),
            forward: Mutex::new(forward),
        })
    }

    /// **A verb's admission** (plan 032 §3.1.1 item 8): `lane_ended` once the
    /// entry is FINISHED, else a clone of the adapter — `None` when a `close`
    /// already released it. The lifecycle check and the clone are ONE `state`
    /// acquisition, and every release follows the `Finished` transition (or
    /// the entry's removal), so a verb is either admitted before the lane
    /// finished — and runs its course on the clone it took (a release after
    /// this takes the core's, not the verb's) — or refused `lane_ended`;
    /// never handed a finished lane's released client as a missing one.
    fn client(&self) -> Result<Option<Arc<dyn AgentLane>>, LaneFailure> {
        let life = lock(&self.state);
        if matches!(life.state, EntryState::Finished { .. }) {
            return Err(LaneFailure::Ended);
        }
        Ok(lock(&self.client).clone())
    }

    /// **Release the transport** (plan 032 PC4): take the adapter client and
    /// the tunnel share out of the core and drop them. Idempotent. Called by
    /// [`LaneEntry::retire`], and by the pump itself once the session's own
    /// final `Down` finished the lane ([`LaneCore::end`]; C1 review: a
    /// natural end used to keep both until `close` or the host's removal).
    /// **Never under `inner` or `state`**: giving the last tunnel share back
    /// takes the registry's lock — and each slot's lock is released before
    /// what it held is dropped.
    fn release(&self) {
        let forward = lock(&self.forward).take();
        let client = lock(&self.client).take();
        drop(forward);
        drop(client);
    }

    /// The pump's own end: the session's final `Down` (`reason`) — applied,
    /// emitted and `Finished` unless something finished the lane first
    /// ([`LaneCore::finish`]) — and then the transport released
    /// ([`LaneCore::release`]), outside the `state` lock. The pump's work is
    /// over either way: a lane that ended for good has nothing left to dial.
    fn end(&self, sink: &EventSink, key: &Key, reason: &str) {
        self.finish(sink, key, reason);
        self.release();
    }

    /// Apply `event` to the view and emit it, under `state`, unless the
    /// entry is `Finished` or closing. Answers whether it was: `false` is the
    /// pump's cue that its work is over.
    fn apply(&self, sink: &EventSink, key: &Key, event: &LaneEvent) -> bool {
        let life = lock(&self.state);
        if !life.admits() {
            return false;
        }
        lock(&self.view).apply(event);
        emit(sink, key, event);
        true
    }

    /// End the lane with `Down{reason}`: under `state`, unless it is already
    /// `Finished` or closing — apply and emit the `Down`, become
    /// `Finished{reason}`, and (the lock released) notify. Answers whether it
    /// did, so exactly ONE terminal `Down` is emitted whoever races to it: the
    /// pump's final `Down`, a grace's expiry, the host's removal.
    fn finish(&self, sink: &EventSink, key: &Key, reason: &str) -> bool {
        let mut life = lock(&self.state);
        if !life.admits() {
            return false;
        }
        finish_locked(&mut life, &self.view, sink, key, reason);
        drop(life);
        self.finished.notify_waiters();
        true
    }

    /// The host's removal reaching this entry as craze news `seq` (plan 032
    /// §3.1.1 item 5): taken only past the news fence ([`NewsMark`] — always,
    /// by construction: a removal is minted as the host's state leaves the
    /// map, so nothing of its generation is minted after it), and then the
    /// lane ends `Down{host_removed}` as [`Self::finish`] ends it, unless it
    /// is already finished or closing. Answers whether the news was taken —
    /// the caller retires the entry if so.
    fn host_removed(&self, sink: &EventSink, key: &Key, seq: u64) -> bool {
        let mut life = lock(&self.state);
        if !life.news.take(seq) {
            return false;
        }
        let ends = life.admits();
        if ends {
            finish_locked(&mut life, &self.view, sink, key, DOWN_HOST_REMOVED);
        }
        drop(life);
        if ends {
            self.finished.notify_waiters();
        }
        true
    }

    /// A grace's deadline (plan 032 §3.1.1 item 4): end the lane — with its
    /// departure's own reason — only if it is STILL departing under `token`
    /// and the app is not closing, the check and the transition under one
    /// `state` lock. A return (a new state, or a later departure's new token)
    /// or a final `Down` that won the race makes this a no-op.
    fn expire(&self, sink: &EventSink, key: &Key, token: u64) -> bool {
        let mut life = lock(&self.state);
        let cause = match life.state {
            EntryState::Departing { token: held, cause } if held == token && !life.closing => cause,
            _ => return false,
        };
        finish_locked(&mut life, &self.view, sink, key, cause.reason());
        drop(life);
        self.finished.notify_waiters();
        true
    }

    /// Whether the departure `token` is still the one in effect.
    fn departing_under(&self, token: u64) -> bool {
        matches!(lock(&self.state).state, EntryState::Departing { token: held, .. } if held == token)
    }

    fn is_finished(&self) -> bool {
        matches!(lock(&self.state).state, EntryState::Finished { .. })
    }
}

/// [`LaneCore::finish`]'s body, for a caller already holding `state` and
/// having checked it admits a frame.
fn finish_locked(
    life: &mut Life,
    view: &Mutex<LaneView>,
    sink: &EventSink,
    key: &Key,
    reason: &str,
) {
    let down = LaneEvent::Down {
        reason: reason.to_string(),
    };
    lock(view).apply(&down);
    emit(sink, key, &down);
    life.state = EntryState::Finished {
        reason: reason.to_string(),
    };
}

struct LaneEntry {
    /// The stamp this entry was opened against — the eviction key for a
    /// restarted tab. A new port means a different server, not a reconnect; and
    /// a new KIND on the same port means a different agent, which is the half
    /// the URL alone used to miss.
    stamp: AgentLaneStamp,
    /// What `lane.open` answered with, cached so a second `open` is genuinely
    /// idempotent rather than a second round trip. The session row ONLY: the
    /// lane's capabilities are stream state, read from the view by
    /// `lane.messages` (module doc), and deliberately not cached here.
    session: LaneSession,
    /// The lifecycle, the view and the transport (the adapter client and the
    /// tunnel share), shared with the pump and the grace task.
    core: Arc<LaneCore>,
    /// The supervision loop. Shares nothing with this struct but the core, so
    /// there is no reference cycle and dropping the entry really does end it.
    /// Behind a lock so [`Lanes::shutdown`] can take the handle and AWAIT it
    /// (plan 026 §3.3) — an abort alone leaves the loop running until its
    /// next poll.
    pump: Mutex<Option<tokio::task::JoinHandle<()>>>,
    /// The running departure's grace task (plan 032 §3.1.1 item 2), if any —
    /// held so a return, a host removal, `close` and the shutdown can cancel
    /// it, and the shutdown AWAIT it like a pump. It holds only a `Weak` to
    /// this entry.
    grace: Mutex<Option<tokio::task::JoinHandle<()>>>,
    /// For a craze lane, the generation of the craze source it was opened
    /// through; `None` for a roost-stamped one. Every piece of craze news
    /// touches only the lanes of the generation it names, so a stale source's
    /// news — published after a remove and a re-registration of the same host
    /// — never touches a lane opened through the new one (C9 review), and
    /// `open` never adopts an entry of an older one (plan 032 §3.1.1 item 12).
    craze_gen: Option<u64>,
}

impl LaneEntry {
    fn opened(&self) -> Value {
        json!({ "session": self.session })
    }

    fn is_finished(&self) -> bool {
        self.core.is_finished()
    }

    /// The adapter a verb is sent through — or `lane_ended` once the entry is
    /// FINISHED (plan 032 §3.1.1 item 8), decided with the clone under one
    /// `state` lock ([`LaneCore::client`]). The clone is the verb's own: a
    /// release after this point takes the core's, not the one the verb is
    /// using.
    fn client(&self) -> Result<Arc<dyn AgentLane>, LaneFailure> {
        // Not finished, and no client: a lane `close` retired between the
        // registry read and here — there is no lane any more.
        self.core.client()?.ok_or_else(|| {
            LaneFailure::NoLane(format!(
                "the lane for session {:?} was closed",
                self.stamp.session_id
            ))
        })
    }

    /// End the subscription, give the tunnel share back and release the
    /// adapter (plan 032 PC4, [`LaneCore::release`]). The entry itself stays
    /// wherever it is — a finished craze entry stays in the registry,
    /// readable from its view. Idempotent, and **must not be called under the
    /// `inner` lock** — releasing the last share takes it.
    fn retire(&self) {
        self.abort_pump();
        self.core.release();
    }

    /// Mark the entry as leaving the registry ([`Life::closing`]): nothing
    /// more is applied or emitted for it. Called under the registry lock (the
    /// lock order allows `inner` → `state`) by whatever removes it, and by
    /// the shutdown's start ([`Inner::close`]) for every entry at once.
    fn detach(&self) {
        lock(&self.core.state).closing = true;
    }

    /// Cancel the departure's grace task, if one runs, handing it back to be
    /// joined.
    fn cancel_grace(&self) -> Option<tokio::task::JoinHandle<()>> {
        let task = lock(&self.grace).take();
        if let Some(task) = &task {
            task.abort();
        }
        task
    }

    fn abort_pump(&self) {
        if let Some(pump) = lock(&self.pump).as_ref() {
            pump.abort();
        }
    }
}

impl Drop for LaneEntry {
    /// Ending the pump drops the [`shed_core::lane::LaneStop`] it holds, which
    /// aborts the adapter's watcher, which closes the `/event` socket.
    ///
    /// A backstop: every eviction path calls [`LaneEntry::retire`] first.
    fn drop(&mut self) {
        self.abort_pump();
        if let Some(grace) = lock(&self.grace).as_ref() {
            grace.abort();
        }
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
    /// Set when the app's orderly shutdown begins (plan 026 §3.3), never
    /// cleared: read under this lock by every open's declaration and commit,
    /// and by a pump before it re-`ensure`s its tunnel. Set only by
    /// [`Inner::close`], AFTER every entry is fenced.
    closing: bool,
    /// The last departure token handed out ([`EntryState::Departing`]) —
    /// minted under this lock, so two departures never share one.
    departures: u64,
}

impl Inner {
    /// **The shutdown's start: fence first, flag second** (plan 032 §3.1.1
    /// items 4 and 11; C1 review). Every registered entry is marked closing
    /// ([`LaneEntry::detach`]) under its own `state` lock, and only THEN is
    /// [`Inner::closing`] set — all within the caller's one acquisition of
    /// this lock. A frame a pump has in flight, or a grace's expiry, holds
    /// its entry's `state`, so the fence waits it out; once the flag is set,
    /// every entry is fenced, and no frame of any of them is applied or
    /// emitted after the shutdown began. (Flag first, the walk let an entry it
    /// had not reached yet — its pump mid-frame, its grace expiring — emit
    /// after it.) No entry is registered after it: every commit checks the
    /// flag under this same lock. Idempotent.
    fn close(&mut self) {
        for entry in self.entries.values() {
            entry.detach();
        }
        self.closing = true;
    }
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
/// What [`Lanes::shutdown`] closed — and what it abandoned at a bound (plan
/// 026 AM4), for the shutdown's phase line.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LanesClosed {
    /// Every entry the registry held — FINISHED ones included (plan 032
    /// §3.1.1 item 11): a finished craze lane stays registered, readable,
    /// until it is closed.
    pub lanes: usize,
    /// …of which were already finished.
    pub finished: usize,
    pub forwards: usize,
    /// Forwards whose close did not settle by its bound: a reap another thread
    /// owns, or an `ensure` still in flight (the child SIGKILLed either way).
    pub forwards_unsettled: usize,
    pub pumps: usize,
    /// Pumps aborted but not ended by [`PUMP_JOIN_WAIT`].
    pub pumps_unjoined: usize,
    /// Departure grace tasks aborted and joined alongside the pumps, under
    /// the same bound.
    pub graces: usize,
    /// Grace tasks aborted but not ended by [`PUMP_JOIN_WAIT`].
    pub graces_unjoined: usize,
}

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
    /// How long a departing craze lane waits for its session's final word
    /// ([`CRAZE_DEPART_GRACE`]; a test's own with [`Lanes::with_depart_grace`]).
    depart_grace: Duration,
    /// [`crate::roost_hosts::TestGap`], armed by a test: `open`'s gap between
    /// resolving a craze source and declaring the open.
    #[cfg(test)]
    pub(crate) open_gap: Mutex<Option<crate::roost_hosts::TestGap>>,
    /// [`crate::roost_hosts::TestGap`], armed by a test: `open`'s gap between
    /// its declaration and its commit — where craze news reaches the open in
    /// flight ([`Pending::news`]).
    #[cfg(test)]
    pub(crate) commit_gap: Mutex<Option<crate::roost_hosts::TestGap>>,
    /// [`crate::roost_hosts::TestGap`]s armed by a test at one side of a
    /// verb's admission ([`Admission`], [`Lanes::admit`]) — each taken by the
    /// first verb to reach its side.
    #[cfg(test)]
    pub(crate) verb_gaps: Mutex<Vec<(Admission, crate::roost_hosts::TestGap)>>,
}

/// **Test-only:** which side of a verb's admission ([`Lanes::admit`]) a
/// [`Lanes::verb_gaps`] gap parks the verb on — so a cell can land a lane's
/// final `Down` on either side of it (plan 032 §3.1.1 item 8; C1 review).
#[cfg(test)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Admission {
    /// The entry read from the registry, the lifecycle not yet decided.
    Before,
    /// Admitted: the adapter clone in hand, the adapter not yet called.
    After,
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
    /// A FINISHED craze lane is still committed (it stays readable); ask
    /// [`Self::lane_state`] which state it is in.
    #[cfg(test)]
    pub(crate) fn is_open(&self, machine: &str, kind: &str, session_id: &str) -> bool {
        lock(&self.inner)
            .entries
            .contains_key(&key(machine, kind, session_id))
    }

    /// The committed entry's state — `"live"`, `"departing"` or `"finished"`
    /// — or `None` with no entry: the registry's own word, for the cells that
    /// pin the lifecycle (plan 032 §3.1.1).
    #[cfg(test)]
    pub(crate) fn lane_state(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Option<&'static str> {
        let entry = self.entry(&key(machine, kind, session_id))?;
        let life = lock(&entry.core.state);
        Some(match life.state {
            EntryState::Live => "live",
            EntryState::Departing { .. } => "departing",
            EntryState::Finished { .. } => "finished",
        })
    }

    /// This layer with a departure grace of `grace` instead of
    /// [`CRAZE_DEPART_GRACE`] — the test seam plan 032 §3.1 names.
    #[cfg(test)]
    pub(crate) fn with_depart_grace(mut self, grace: Duration) -> Lanes {
        self.depart_grace = grace;
        self
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
            depart_grace: CRAZE_DEPART_GRACE,
            #[cfg(test)]
            open_gap: Mutex::new(None),
            #[cfg(test)]
            commit_gap: Mutex::new(None),
            #[cfg(test)]
            verb_gaps: Mutex::new(Vec::new()),
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
    /// **The registry first** (plan 032 §3.1.1 item 9, PC5): an entry is
    /// judged before the row is required, because a DEPARTING lane has no row
    /// to resolve and is still the lane — re-opening it (the panel's remount)
    /// answers it as it is. A FINISHED entry is a readable ended lane: with no
    /// row it is answered as it is; with a row that resolves (the session runs
    /// again — a reopened one, or an opencode tab still listed) a fresh lane is
    /// opened, and the finished entry is replaced only when that open COMMITS,
    /// so a failed open leaves it readable. **Generation-fenced** (item 12): a
    /// craze entry of an older source generation is never adopted by an equal
    /// stamp — it is evicted and reopened through the current source.
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
        if self.closing() {
            return Err(LaneFailure::ShuttingDown);
        }
        // The row is resolved BEFORE a gate is registered for the key — unless
        // the registry already holds the key (a departing or finished lane,
        // which may have no row). Both IPC doors take `machine` and
        // `session_id` as free strings, so a call that names nothing real must
        // not be able to make this app remember it: `lane.open` with junk (or
        // with the session ids of a machine whose tabs churn) used to mint a
        // gate per call and keep it for the life of the process.
        if self.entry(&key).is_none() {
            self.lane_stamp(machine, kind, session_id)?;
        }
        let gate = self.gate(&key);
        let _serialized = gate.lock().await;
        // **An open that waited on its gate re-checks before going on** (plan
        // 026 §3.3): the shutdown may have begun while it waited, and nothing
        // below may build a tunnel or a subscription after that. The
        // declaration below checks again, under the registry lock.
        if self.closing() {
            return Err(LaneFailure::ShuttingDown);
        }

        // Re-resolved under the gate, and this is the value everything below
        // uses: the pre-gate one was read before waiting, and waiting is exactly
        // when a tab restarts onto a new port or goes away. Using it would open
        // a lane against a socket the snapshot has already retired.
        let resolved = self.lane_stamp(machine, kind, session_id);
        if let Some(entry) = self.entry(&key) {
            // Read once: `Finished` is one-way, so a lane that finishes after
            // this read is answered as the live one it was a moment ago (a
            // readable lane either way) or replaced by a fresh open at commit.
            let finished = entry.is_finished();
            match &resolved {
                // No row: a departing lane is still the lane (its grace runs),
                // and a finished one is a readable ended lane.
                Err(_) => return Ok(entry.opened()),
                // A finished entry stays until the fresh open commits.
                Ok(_) if finished => {}
                Ok(stamp) if *stamp == entry.stamp && self.current_generation(machine, &entry) => {
                    return Ok(entry.opened());
                }
                // Another stamp, or another source generation: not this lane.
                Ok(_) => self.evict(&key),
            }
        }
        let stamp = resolved?;

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
        // anywhere below has something to cancel — and refused, under the
        // same lock, once the shutdown has begun.
        let _pending = self.declare(&key, &stamp, craze_gen)?;

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

        // The transport goes into the core the pump shares — declared before
        // the registry guard below, so an early return drops the guard first
        // and the tunnel share (which takes `inner`) after it.
        let forward_weak = forward.as_ref().map(ForwardShare::weak);
        let core = LaneCore::new(Some(Arc::clone(&client)), forward);
        #[cfg(test)]
        crate::roost_hosts::at_gap(&self.commit_gap).await;
        // Commit, or roll back. ONE acquisition: the re-check and the insert
        // must not be separable, or a `close` landing between them would be
        // lost — and the pump is not started until the commit is decided, so a
        // rolled-back open never had a subscription to leak either.
        let mut inner = lock(&self.inner);
        if inner.closing {
            return Err(LaneFailure::ShuttingDown);
        }
        let Some(news) = inner
            .pending
            .get(&key)
            .filter(|p| !p.cancelled)
            .map(|p| p.news)
        else {
            return Err(LaneFailure::NoLane(format!(
                "the lane for session {session_id:?} on machine {machine:?} was closed \
                 while it was being opened"
            )));
        };
        // The entry starts its news fence where its open's stands (C1
        // re-review): the craze news that reached the open is never undone
        // by older news delivered after the commit. Under the registry lock,
        // which every piece of news is applied under first, so none lands
        // between this and the insert.
        lock(&core.state).news = news;
        let pump = self.spawn_pump(key.clone(), client, Arc::clone(&core), forward_weak);
        let entry = Arc::new(LaneEntry {
            stamp,
            session,
            core,
            pump: Mutex::new(Some(pump)),
            grace: Mutex::new(None),
            craze_gen,
        });
        let opened = entry.opened();
        // The one entry this can replace is a FINISHED one, kept readable
        // until now (item 9): opens on a key are serialized by its gate, and
        // a live one was evicted above.
        let replaced = inner.entries.insert(key, entry);
        if let Some(old) = &replaced {
            old.detach();
        }
        drop(inner);
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        if let Some(old) = replaced {
            old.cancel_grace();
            old.retire();
        }
        Ok(opened)
    }

    /// Whether `entry` was opened through `machine`'s CURRENT craze source —
    /// always, for a roost-stamped lane (plan 032 §3.1.1 item 12).
    fn current_generation(&self, machine: &str, entry: &LaneEntry) -> bool {
        entry.craze_gen.is_none_or(|gen| {
            self.machines
                .craze_source(machine)
                .is_ok_and(|(_, now)| now == gen)
        })
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
    /// payload: `{messages, activity, session, generation, stale, ended,
    /// capabilities, settings}`.
    ///
    /// `capabilities` and `settings` are the LIVE generation's (each `null`
    /// until a seed carrying it has swapped in), which is where a client reads
    /// what the session can do — never from `lane.open` (module doc). `stale` is
    /// the banner and `ended` the lifecycle; the two are different facts
    /// (`shed_app::lane_view`'s module doc).
    ///
    /// `session` is the live generation's session ROW — the stream's latest
    /// `Session`, `null` until a seed has swapped one in — and it is where a
    /// client reads the row's facts once there is one. `lane.open`'s row is
    /// cached for the life of the entry (that is what makes it idempotent), and
    /// it is whatever the source listed at the open: for a craze session opened
    /// the moment its create answered, the create's own row, with none of the
    /// attach info document's facts — so a header that kept reading it showed
    /// no permission line for as long as the panel stayed open (plan 025
    /// §3.6.5; live leg 1).
    ///
    /// The fold and the projection are [`shed_app::lane_view`]'s; the only thing
    /// that belongs here is the envelope's SHAPE, which is Tauri's and not a
    /// client-neutral API (the phone converts the same
    /// [`shed_app::lane_view::LaneViewSnapshot`] into its own DTOs).
    ///
    /// `finished` (plan 032 §3.1.1 item 13, PC14b) is the ENTRY's lifecycle,
    /// not the view's `ended`: `true` once the lane is over for good — a final
    /// `Down`, its grace run out, its host removed — when every verb is
    /// refused `lane_ended` and the panel disables its composer. `ended` alone
    /// may be a retryable `Down` the pump is already resubscribing after. Read
    /// with the snapshot under one `state` lock, so a `finished` lane's
    /// snapshot always carries its terminal `Down`.
    pub fn messages(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        let (finished, snap) = {
            let life = lock(&entry.core.state);
            let finished = matches!(life.state, EntryState::Finished { .. });
            (finished, lock(&entry.core.view).snapshot(None))
        };
        Ok(json!({
            "messages": snap.messages,
            "activity": snap.activity,
            "session": snap.session,
            "generation": snap.generation,
            "stale": snap.stale,
            "ended": snap.ended,
            "finished": finished,
            "capabilities": snap.capabilities,
            "settings": snap.settings,
            // The session's `/` menu, RESOLVED (plan 029 §3.4, P4/P10):
            // `shed_app::slash_menu::resolve` of its catalog against its
            // capabilities, cached by the view — `null` when the
            // capabilities do not say `slash` or no catalog has arrived, and
            // then the composer offers no menu. The panel hands it back to
            // the `lane_slash_*` commands; it never re-derives a rule.
            "slash_menu": snap.slash_menu,
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
        let snap = lock(&entry.core.view).snapshot(None);
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
        self.admit(machine, kind, session_id)
            .await?
            .send(text, mode)
            .await?;
        Ok(json!({}))
    }

    /// `lane.cancel` — stop the turn in flight.
    pub async fn cancel(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        self.admit(machine, kind, session_id)
            .await?
            .cancel()
            .await?;
        Ok(json!({}))
    }

    /// `lane.stop` — end the SESSION (plan 025 §3.6.4), not just this
    /// transcript: craze's `session.stop`, answered on its receipt; the lane
    /// itself ends when the session's `session_closed` arrives, and the row
    /// leaves around then — the roster's `Removed` (on craze 0.2.0 usually
    /// FIRST, which the lane's departure grace absorbs: plan 032 §3.1), or,
    /// for a session no roster has listed yet, the source letting its
    /// created row go (`shed_craze::lane`'s "The session's end"). A session whose
    /// capabilities say `stop: false` (a TUI-hosted craze session, every
    /// opencode one) refuses it — the panel offers no Stop there.
    pub async fn stop(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        self.admit(machine, kind, session_id).await?.stop().await?;
        Ok(json!({}))
    }

    /// `lane.settings` — the session's settings NOW, the adapter's one-shot
    /// read ([`AgentLane::settings`]; a craze lane answers from its running
    /// watcher's fold once it has seeded). A session whose capabilities say
    /// `settings: false` answers the empty default. What the settings sheet
    /// renders is the STREAM's copy — `lane.messages`' `settings`, staged with
    /// the rest of the view — which this read does not replace.
    ///
    /// **A finished lane answers from its view** (plan 032 §3.1.1 item 7):
    /// the last settings the session stated (the empty default if it stated
    /// none), with no adapter call — a retired lane has no adapter, and an
    /// ended session has nothing newer to say.
    pub async fn settings(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        let client = match entry.client() {
            Ok(client) => client,
            Err(LaneFailure::Ended) => {
                let settings = lock(&entry.core.view)
                    .snapshot(None)
                    .settings
                    .unwrap_or_default();
                return Ok(json!({ "settings": settings }));
            }
            Err(e) => return Err(e),
        };
        let settings = client.settings().await?;
        Ok(json!({ "settings": settings }))
    }

    /// `lane.set` — change one setting (plan 025 §3.10): a model, a mode, or
    /// one of the current model's options ([`parse_setting`]). `{}` is the
    /// agent's confirmation; the new value arrives on the stream (`Settings`,
    /// re-emitted from the change's own `meta` delta, which craze delivers
    /// ahead of the answer). A refusal keeps its code — craze's `stale_model`
    /// (the session left the model an option was chosen for) is
    /// `not_accepting` — and an answer lost to a drop is `outcome_unknown`:
    /// never resent, and decided by the next `Settings`.
    pub async fn set(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
        change: LaneSettingChange,
    ) -> Result<Value, LaneFailure> {
        self.admit(machine, kind, session_id)
            .await?
            .set(change)
            .await?;
        Ok(json!({}))
    }

    /// `lane.set_title` — rename the SESSION (plan 029 D4, D12): craze's
    /// `session.setTitle`, the composer's `/rename <title>`. The title is
    /// trimmed and a blank one is refused here, `bad_request`, with nothing
    /// sent (craze would refuse it too; a client never sends one empty). The
    /// new title reaches the header on the stream — the `meta` delta craze
    /// sends with it, re-emitted as `Session` — never from this answer, which
    /// is [`renamed`]'s. A session whose capabilities say `set_title: false`
    /// (every opencode one) refuses it, `failed`.
    pub async fn set_title(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
        title: &str,
    ) -> Result<Value, LaneFailure> {
        let title = title.trim();
        if title.is_empty() {
            return Err(LaneFailure::bad_request(
                "a session's title must not be blank".to_string(),
            ));
        }
        renamed(
            self.admit(machine, kind, session_id)
                .await?
                .set_title(title)
                .await,
        )
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
        self.admit(machine, kind, session_id)
            .await?
            .answer(approval_id, answer)
            .await?;
        Ok(json!({}))
    }

    /// `lane.close` — end the subscription and release the transport.
    ///
    /// Idempotent: closing a lane that is not open is success, because the
    /// caller's intent (there is no lane here any more) is already true. The
    /// panel calls this on unmount, and an unmount can race an eviction.
    ///
    /// Removes the entry in ANY state — live, departing (its grace task
    /// cancelled) or finished (plan 032 §3.1.1 item 10).
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
                    entry.detach();
                    gone.push(Arc::clone(entry));
                }
                keep
            });
            gone
        };
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        for entry in gone {
            entry.cancel_grace();
            entry.retire();
        }
    }

    /// One piece of `machine`'s craze source news, of generation `gen`, to the
    /// handler it names — what [`crate::roost_hosts::OnCrazeNews`] is wired to
    /// (`lib.rs`, and the craze cells' `lanes_over`).
    ///
    /// **Two fences, in this order, both under the registry lock and then the
    /// lane's `state` (the lock order)**, for every kind of news and every
    /// lane it names — a committed entry, or an open in flight:
    ///
    /// 1. **The generation** (C9 review): only a lane opened through source
    ///    generation `gen` is touched — a removed and re-registered host's
    ///    old source never ends, or revives, a lane of its new one.
    /// 2. **The news sequence** (C1 re-review, [`NewsMark`]): the lane takes
    ///    the piece only if its `seq` is newer than every piece it already
    ///    took, so news delivered out of the order it happened in — its three
    ///    publishers (the source's fold, an End tab, the host's removal) each
    ///    publish from their own task after releasing the lock their news was
    ///    minted under — never undoes newer news: a return delayed past an
    ///    End tab's departure does not revive the lane (its grace runs on and
    ///    ends it on time), and a departure delayed past a return does not
    ///    depart it again.
    ///
    /// The generation fence comes first so that another generation's numbers
    /// — another sequence — never advance a lane's mark.
    pub fn craze_news(&self, machine: &str, gen: u64, news: CrazeNews<'_>) {
        match news {
            CrazeNews::Departed {
                host_ids,
                cause,
                seq,
            } => self.evict_craze(machine, gen, seq, host_ids, cause),
            CrazeNews::Returned { host_ids, seq } => {
                self.craze_returned(machine, gen, seq, host_ids)
            }
            CrazeNews::HostRemoved { seq } => self.evict_craze_generation(machine, gen, seq),
        }
    }

    /// **A craze lane's row left its source** (plan 032 §3.1.1 item 2) — a
    /// roster `Removed`, a `Ready` swap that no longer lists it, its tab
    /// ended (`cause` [`Departure::Removed`]), or the hub gone, the source
    /// Dormant ([`Departure::HubGone`]) — so the lane DEPARTS: it keeps its
    /// own connection for [`Self::depart_grace`] and ends at its session's
    /// final `Down`, a return, or the grace's end. Any open of the row still in
    /// flight is cancelled, as before plan 032.
    ///
    /// Per entry of generation `gen`, under the registry lock and then the
    /// entry's `state` (the lock order), and only if it takes news `seq`
    /// ([`Self::craze_news`]'s fence — an entry that already took a newer
    /// return stays live):
    ///
    /// * **Live** → `Departing{a fresh token, cause}`, and one grace task
    ///   starts on this layer's runtime, holding a `Weak` to the entry; its
    ///   handle is stored on the entry (a stale one, from a departure a return
    ///   ended, is aborted).
    /// * **Departing or Finished** → unchanged: a duplicate removal never
    ///   extends a departure (PC2). Its `seq` is still taken: a return minted
    ///   before it, delivered after, must not cancel the departure.
    ///
    /// A pending open takes the news by the same fence, and is cancelled if
    /// it does.
    ///
    /// **Fenced by the generation, under this registry's own lock** (C9
    /// review): the news is published after the roost-host layer's lock that
    /// computed it is released, and by then the host may have been removed,
    /// registered again and had a lane opened on the same hostId through its
    /// NEW source. Only an entry (or a pending open) filed under `gen` is
    /// touched, and the comparison and the transition are one acquisition, so
    /// a lane of another generation can never be ended by this one's news.
    pub fn evict_craze(
        &self,
        machine: &str,
        gen: u64,
        seq: u64,
        host_ids: &[String],
        cause: Departure,
    ) {
        let mut inner = lock(&self.inner);
        for host_id in host_ids {
            let key = key(machine, CRAZE, host_id);
            if let Some(pending) = inner.pending.get_mut(&key) {
                if pending.craze_gen == Some(gen) && pending.news.take(seq) {
                    pending.cancelled = true;
                }
            }
            let Some(entry) = inner
                .entries
                .get(&key)
                .filter(|entry| entry.craze_gen == Some(gen))
                .map(Arc::clone)
            else {
                continue;
            };
            let mut life = lock(&entry.core.state);
            // The fence first: a newer piece is taken even when it changes
            // nothing below (a duplicate departure).
            if !life.news.take(seq) || life.closing || life.state != EntryState::Live {
                continue;
            }
            inner.departures += 1;
            let token = inner.departures;
            life.state = EntryState::Departing { token, cause };
            let task = self.handle.spawn(depart(
                Arc::downgrade(&entry),
                Arc::clone(&entry.core),
                token,
                self.depart_grace,
                Arc::clone(&self.sink),
                key,
            ));
            drop(life);
            let stale = lock(&entry.grace).replace(task);
            if let Some(stale) = stale {
                stale.abort();
            }
        }
    }

    /// **A departing craze lane's row is listed again** (plan 032 §3.1.1
    /// item 3, PC2): the row came back within the grace — a reseed that
    /// briefly dropped it, a source that went Dormant and found the hub again
    /// — so the lane is LIVE again, its connection never having been let go.
    /// Its grace task is aborted (and would find its token gone if it were
    /// already past its wait); a later removal starts a fresh grace under a
    /// new token. Only a `Departing` entry of generation `gen` that takes news
    /// `seq` changes ([`Self::craze_news`]'s fences): a re-registered host's
    /// source never revives an older generation's lane (item 12), a return
    /// minted before the departure it would cancel — delivered after it —
    /// never revives the lane (C1 re-review), and nothing un-finishes a
    /// finished one.
    ///
    /// A pending open of `gen` takes the news too, so a departure minted
    /// before this return cannot cancel it afterwards, nor depart the entry it
    /// commits. (One already cancelled stays cancelled: a cancelled open fails,
    /// as `close` makes it fail, and its panel's retry opens the listed row.)
    pub fn craze_returned(&self, machine: &str, gen: u64, seq: u64, host_ids: &[String]) {
        let mut inner = lock(&self.inner);
        for host_id in host_ids {
            let key = key(machine, CRAZE, host_id);
            if let Some(pending) = inner.pending.get_mut(&key) {
                if pending.craze_gen == Some(gen) {
                    // Taken (or not) — a return changes nothing else here.
                    pending.news.take(seq);
                }
            }
            let Some(entry) = inner
                .entries
                .get(&key)
                .filter(|entry| entry.craze_gen == Some(gen))
            else {
                continue;
            };
            let mut life = lock(&entry.core.state);
            if life.news.take(seq) && matches!(life.state, EntryState::Departing { .. }) {
                life.state = EntryState::Live;
                drop(life);
                entry.cancel_grace();
            }
        }
    }

    /// **`machine`'s host was removed** (plan 032 §3.1.1 item 5, PC3): its
    /// craze source of generation `gen` is gone, so EVERY lane filed under
    /// it — listed or departing, whatever the source last held — ends now,
    /// with no grace: under its `state`, unless already finished,
    /// `Down{host_removed}` is applied and emitted and it becomes `Finished`;
    /// then its grace task is cancelled and it is retired at once (its client
    /// released, PC4). The entries STAY, readable — `lane.messages` still
    /// answers. Every pending open of `gen` is cancelled.
    ///
    /// Through the news fence like any news ([`Self::craze_news`]), which it
    /// always passes: `seq` is minted in the critical section that takes the
    /// host's state out of the map, so it is the last of its generation — no
    /// later news of `gen` can be minted.
    ///
    /// The old rule published only the rows the source still held, which
    /// missed a lane whose row had already left (a departing lane) and left it
    /// holding the removed host's dial for the rest of its grace.
    pub fn evict_craze_generation(&self, machine: &str, gen: u64, seq: u64) {
        let ended: Vec<Arc<LaneEntry>> = {
            let mut inner = lock(&self.inner);
            for (key, pending) in inner.pending.iter_mut() {
                if key.0 == machine
                    && key.1 == CRAZE
                    && pending.craze_gen == Some(gen)
                    && pending.news.take(seq)
                {
                    pending.cancelled = true;
                }
            }
            inner
                .entries
                .iter()
                .filter(|((m, _, _), entry)| m == machine && entry.craze_gen == Some(gen))
                .filter(|(key, entry)| entry.core.host_removed(&self.sink, key, seq))
                .map(|(_, entry)| Arc::clone(entry))
                .collect()
        };
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        for entry in ended {
            entry.cancel_grace();
            entry.retire();
        }
    }

    // ---- internals ----

    /// Declare an open in flight for `key`. See [`Pending`].
    fn declare(
        &self,
        key: &Key,
        stamp: &AgentLaneStamp,
        craze_gen: Option<u64>,
    ) -> Result<PendingGuard, LaneFailure> {
        let mut inner = lock(&self.inner);
        // Under the lock the shutdown sets the flag under, so an open either
        // declared before it (and is cancelled by it) or is refused here.
        if inner.closing {
            return Err(LaneFailure::ShuttingDown);
        }
        inner.pending.insert(
            key.clone(),
            Pending {
                stamp: stamp.clone(),
                cancelled: false,
                craze_gen,
                news: NewsMark::default(),
            },
        );
        Ok(PendingGuard {
            inner: Arc::clone(&self.inner),
            key: key.clone(),
        })
    }

    /// Whether the shutdown has begun — read under the registry lock.
    fn closing(&self) -> bool {
        lock(&self.inner).closing
    }

    /// **Refuse new lanes from now on** (plan 026 §3.3): set with the app's
    /// spawn gate, at the start of its orderly shutdown. Every open not yet
    /// declared is refused `shutting_down`; [`Self::shutdown`] tears down the
    /// rest.
    ///
    /// **And emit nothing from now on** (plan 032 §3.1.1 item 4; C1 review):
    /// this is where the registry's `closing` flag is first set, so every
    /// entry is fenced FIRST ([`Inner::close`]) — waiting out a frame a pump
    /// has in flight — and no pump frame or grace expiry lands after it.
    pub fn begin_closing(&self) {
        lock(&self.inner).close();
    }

    /// **The lanes phase of the orderly shutdown** (plan 026 §3.3, phase 1).
    /// **Blocking**: run it on a dedicated OS thread, never an async worker —
    /// it kills and reaps `ssh` children — and never on the blocking pool.
    ///
    /// * Every committed entry — finished ones included — and every pending
    ///   open is taken (the opens cancelled, so none commits). Each entry is
    ///   marked `closing` under its own `state` lock BEFORE the registry's
    ///   flag is set ([`Inner::close`], plan 032 §3.1.1 item 4; a no-op past
    ///   [`Self::begin_closing`], which did it first), so from then on no pump
    ///   frame and no grace expiry is applied or emitted.
    /// * Every pump AND every departure's grace task is aborted and AWAITED
    ///   (one bound for them all, [`PUMP_JOIN_WAIT`]): an aborted pump keeps
    ///   running until its next poll, holding its forward.
    /// * Every forward is closed through the trait ([`MachineForward::close_now`]),
    ///   all at once on threads of their own, overlapping the pump joins
    ///   (F10), whatever its reference count —
    ///   dropping is not enough: [`OwnedForward`]'s drop detaches a thread, and
    ///   an aborted pump holds its `Arc` until its future drops. Each close is
    ///   self-bounded (a bounded reap, then a bounded wait for an `ensure` in
    ///   flight).
    ///
    /// Answers what it closed — and what it abandoned at a bound (plan 026
    /// AM4) — for the shutdown's log ([`LanesClosed`]).
    pub fn shutdown(&self) -> LanesClosed {
        let (entries, forwards) = {
            let mut inner = lock(&self.inner);
            // Fence every entry, then set the flag — under each entry's
            // `state` lock (the lock order: `inner` first) and BEFORE the
            // drain: a grace expiry checks its entry's mark and makes its
            // transition under that same lock, so none lands after this.
            inner.close();
            for pending in inner.pending.values_mut() {
                pending.cancelled = true;
            }
            let entries: Vec<Arc<LaneEntry>> = inner.entries.drain().map(|(_, e)| e).collect();
            let forwards: Vec<Arc<OwnedForward>> = inner
                .forwards
                .values()
                .map(|slot| Arc::clone(&slot.forward))
                .collect();
            (entries, forwards)
        };
        let finished = entries.iter().filter(|entry| entry.is_finished()).count();
        let pumps: Vec<tokio::task::JoinHandle<()>> = entries
            .iter()
            .filter_map(|entry| lock(&entry.pump).take())
            .collect();
        for pump in &pumps {
            pump.abort();
        }
        let graces: Vec<tokio::task::JoinHandle<()>> = entries
            .iter()
            .filter_map(|entry| entry.cancel_grace())
            .collect();
        let pump_count = pumps.len();
        let grace_count = graces.len();
        // The forwards' closes and the tasks' joins OVERLAP (F10): each close
        // on a thread of its own, the joins on this one. An aborted pump in its
        // tunnel's `ensure` sees the slot closed and ends within about a poll.
        let (forwards_unsettled, (pumps_unjoined, graces_unjoined)) = std::thread::scope(|scope| {
            // Every close is started before anything is joined.
            let mut closes = Vec::with_capacity(forwards.len());
            for forward in &forwards {
                closes.push(scope.spawn(move || forward.get().close_now()));
            }
            let unjoined = if pumps.is_empty() && graces.is_empty() {
                (0, 0)
            } else {
                // Not a runtime worker (the caller's promise), so blocking on
                // the app's runtime here is legal. One deadline for them all;
                // a task still running at it is aborted already and is left —
                // counted — to end at its next poll.
                self.handle.block_on(async move {
                    let deadline = tokio::time::Instant::now() + PUMP_JOIN_WAIT;
                    (
                        join_by(deadline, pumps).await,
                        join_by(deadline, graces).await,
                    )
                })
            };
            let forwards_unsettled = closes
                .into_iter()
                .map(|close| close.join().unwrap_or(false))
                .filter(|settled| !settled)
                .count();
            (forwards_unsettled, unjoined)
        });
        let closed = LanesClosed {
            lanes: entries.len(),
            finished,
            forwards: forwards.len(),
            forwards_unsettled,
            pumps: pump_count,
            pumps_unjoined,
            graces: grace_count,
            graces_unjoined,
        };
        // The shares go back here, on this thread: the last one out drops the
        // forward (already closed) through [`OwnedForward`]'s own thread.
        for entry in entries {
            entry.retire();
        }
        closed
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

    /// **A verb's admission** (plan 032 §3.1.1 item 8): the entry, then the
    /// adapter the verb is sent through — refused `lane_ended` once the lane
    /// is finished, the lifecycle decided with the clone under one `state`
    /// lock ([`LaneCore::client`]). A verb admitted before the lane finished
    /// runs its course on the clone it took, whatever retires the entry
    /// meanwhile; one admitted after is refused, with nothing sent. The
    /// entry itself is not held past this, so a verb in flight never keeps a
    /// retired entry alive.
    async fn admit(
        &self,
        machine: &str,
        kind: &str,
        session_id: &str,
    ) -> Result<Arc<dyn AgentLane>, LaneFailure> {
        let entry = self.open_entry(machine, kind, session_id)?;
        #[cfg(test)]
        self.at_verb_gap(Admission::Before).await;
        let client = entry.client()?;
        drop(entry);
        #[cfg(test)]
        self.at_verb_gap(Admission::After).await;
        Ok(client)
    }

    /// Pass through the first [`Self::verb_gaps`] gap armed at `side`, if a
    /// test armed one.
    #[cfg(test)]
    async fn at_verb_gap(&self, side: Admission) {
        let gap = {
            let mut gaps = lock(&self.verb_gaps);
            gaps.iter()
                .position(|(at, _)| *at == side)
                .map(|i| gaps.remove(i).1)
        };
        if let Some((reached, proceed)) = gap {
            reached.notify_one();
            proceed.notified().await;
        }
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
            let entry = inner.entries.remove(key);
            if let Some(entry) = &entry {
                entry.detach();
            }
            entry
        };
        // Outside the lock: retiring gives a tunnel share back, which takes it.
        if let Some(entry) = entry {
            entry.cancel_grace();
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
    /// does not replace one that ended for good ([`down_is_final`]): that
    /// `Down` is applied together with the entry's move to `Finished`
    /// ([`LaneCore::finish`], plan 032 §3.1.1 item 1), and the pump returns.
    /// Every frame goes through [`LaneCore::apply`] — under the entry's
    /// `state`, and only while it is not finished — so once this layer has
    /// ended the lane (a grace's expiry, the host's removal) nothing more is
    /// applied, and the pump stops at its next frame. Every
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
        core: Arc<LaneCore>,
        forward: Option<Weak<OwnedForward>>,
    ) -> tokio::task::JoinHandle<()> {
        let sink = Arc::clone(&self.sink);
        let inner = Arc::clone(&self.inner);
        self.handle.spawn(async move {
            let mut backoff = RESUBSCRIBE_BASE;
            loop {
                match ensure_forward(&forward, &inner).await {
                    Ensured::Ready => {}
                    Ensured::Gone => return,
                    Ensured::Failed(e) => {
                        if !core.apply(&sink, &key, &stale(format!("forward: {e}"))) {
                            return;
                        }
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
                        core.end(&sink, &key, DOWN_UNKNOWN_SESSION);
                        return;
                    }
                    Err(e) => {
                        if !core.apply(&sink, &key, &stale(e.to_string())) {
                            return;
                        }
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                };
                // BOTH halves, for the whole read: taking `.rx` alone drops the
                // stop handle, which aborts the pump it is reading from.
                let (mut rx, stop) = subscription.into_parts();
                // The subscription's FIRST Reset is the seed of the connect this
                // loop just ensured for; every later one is a reconnect.
                let mut generations = 0usize;
                while let Some(event) = rx.recv().await {
                    match &event {
                        // A generation that reached steady state is the signal
                        // the transport is healthy again.
                        LaneEvent::Ready { .. } => backoff = RESUBSCRIBE_BASE,
                        // **The lane is over for good** (plan 032 §3.1.1 item
                        // 1): applied, emitted and `Finished` in one `state`
                        // acquisition — unless a grace's expiry or the host's
                        // removal ended it first, in which case this `Down`
                        // is not applied at all — then the transport released
                        // (C1 review: a natural end lets it go as every other
                        // end does), and the pump's work is done.
                        LaneEvent::Down { reason } if down_is_final(reason) => {
                            core.end(&sink, &key, reason);
                            return;
                        }
                        LaneEvent::Reset { .. } => generations += 1,
                        // A `Stale` is the adapter retrying on its own, cursor
                        // intact: keep reading. It is folded into the view (the
                        // banner) like any other frame, and that is all. A
                        // retryable `Down` is folded too, and the subscription
                        // ends behind it.
                        _ => {}
                    }
                    // Under `state`, and only while the entry is not finished:
                    // once it is, nothing more is applied — a late `Ready`
                    // cannot un-end a lane — and the pump stops.
                    if !core.apply(&sink, &key, &event) {
                        return;
                    }
                    if generations > 1 && matches!(event, LaneEvent::Reset { .. }) {
                        match ensure_forward(&forward, &inner).await {
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
                                if !core.apply(&sink, &key, &stale(format!("forward: {e}"))) {
                                    return;
                                }
                                break;
                            }
                        }
                    }
                }
                drop(stop);
                // The subscription ENDED (its channel closed, or a retryable
                // `Down`), or this loop ended it: replace it. One that ended
                // for good returned above, at its final `Down`.
                tokio::time::sleep(backoff).await;
                backoff = next_backoff(backoff);
            }
        })
    }
}

/// Await every task by `deadline`, answering how many had not ended by then
/// (aborted already; each ends at its next poll).
async fn join_by(deadline: tokio::time::Instant, tasks: Vec<tokio::task::JoinHandle<()>>) -> usize {
    let mut unjoined = 0;
    for task in tasks {
        if tokio::time::timeout_at(deadline, task).await.is_err() {
            unjoined += 1;
        }
    }
    unjoined
}

/// What one attempt at making the tunnel usable said.
enum Ensured {
    /// Usable — or there is no tunnel, because the lane is local.
    Ready,
    /// Every share is gone: the entry was evicted and the pump has no work.
    Gone,
    Failed(String),
}

async fn ensure_forward(forward: &Option<Weak<OwnedForward>>, inner: &Mutex<Inner>) -> Ensured {
    let Some(weak) = forward.as_ref() else {
        return Ensured::Ready;
    };
    // **No re-`ensure` once the shutdown has begun** (plan 026 §3.3): the
    // pump's work is over, and a respawned tunnel would outlive the drain.
    if lock(inner).closing {
        return Ensured::Gone;
    }
    let Some(forward) = weak.upgrade() else {
        return Ensured::Gone;
    };
    match forward.ensure().await {
        Ok(()) => Ensured::Ready,
        Err(e) => Ensured::Failed(e),
    }
}

/// A failure THIS layer is retrying, as the frame that says so — folded into
/// the view and emitted exactly as the adapter's own frames are
/// ([`LaneCore::apply`]): the panel must not care whether the thing that went
/// away was the agent or the tunnel to it, since both mean "this transcript is
/// not live" and both are recovered by the same retry.
///
/// [`LaneEvent::Stale`], not `Down`: the pump has already scheduled its next
/// attempt, so the lane has not ended — and `Down` would set the view's `ended`,
/// which a client reads as "this lane is over, reopen it".
fn stale(reason: String) -> LaneEvent {
    LaneEvent::Stale { reason }
}

/// **One departure's grace** (plan 032 §3.1.1 items 2 and 4): wait until
/// `grace` passes or the lane finishes, whichever is first; at the deadline,
/// end the lane — `Down{session_gone}` or `Down{hub_gone}`, by its cause —
/// if it is STILL departing under `token` and the app is not closing
/// ([`LaneCore::expire`], the check and the transition under one `state`
/// lock), then retire it, outside every lock. Anything else — a return (the
/// token gone), the session's own final `Down`, the entry closed, the
/// shutdown — and it exits doing nothing.
///
/// Holds a `Weak` to the entry, so a closed lane is not kept alive for the
/// rest of its grace, and the entry's [`LaneCore`] for the notification it
/// waits on.
async fn depart(
    entry: Weak<LaneEntry>,
    core: Arc<LaneCore>,
    token: u64,
    grace: Duration,
    sink: EventSink,
    key: Key,
) {
    // Registered BEFORE the state is read, so a finish between the departure
    // and this task's first poll is never missed: either the read below sees
    // `Finished`, or the notification wakes the wait.
    let finished = core.finished.notified();
    tokio::pin!(finished);
    finished.as_mut().enable();
    if !core.departing_under(token) {
        return;
    }
    tokio::select! {
        _ = tokio::time::sleep(grace) => {}
        _ = &mut finished => return,
    }
    let Some(entry) = entry.upgrade() else {
        return;
    };
    if core.expire(&sink, &key, token) {
        // Outside the `state` lock (and never under `inner`): retiring gives a
        // tunnel share back, which takes the registry's.
        entry.retire();
    }
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

/// Decode `lane.set`'s `change` — the contract's [`LaneSettingChange`], read
/// STRICTLY by its own serde (`deny_unknown_fields`): `{"kind": "model", "id":
/// "<model id>"}`, `{"kind": "mode", "id": "<mode id>"}` or `{"kind": "config",
/// "id": "<option id>", "value": "<value id>", "for_model"?: "<model id>"}` —
/// the ids `lane.messages`' `settings` offer, sent back verbatim (opaque, never
/// trimmed); `for_model` is the model the client DISPLAYED the option for
/// (Amendment A13), which the adapter binds the change to. A change is a
/// command: a shape this build cannot name, a field it would drop, or an empty
/// id or model (which no setting offers) is refused rather than sent as
/// something else.
pub fn parse_setting(value: &Value) -> Result<LaneSettingChange, LaneFailure> {
    let change: LaneSettingChange = serde_json::from_value(value.clone()).map_err(|e| {
        LaneFailure::bad_request(format!(
            "a setting change is {{kind: \"model\"|\"mode\", id}} or \
             {{kind: \"config\", id, value}}: {e}"
        ))
    })?;
    let (id, for_model) = match &change {
        LaneSettingChange::Model { id } | LaneSettingChange::Mode { id } => (id, None),
        LaneSettingChange::Config { id, for_model, .. } => (id, for_model.as_ref()),
    };
    if id.is_empty() {
        return Err(LaneFailure::bad_request(
            "a setting change names a model, mode or option the session offers by its id"
                .to_string(),
        ));
    }
    if for_model.is_some_and(String::is_empty) {
        return Err(LaneFailure::bad_request(
            "`for_model` names the model the option was chosen for; omit it rather than send it empty"
                .to_string(),
        ));
    }
    Ok(change)
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
        // The app quitting (plan 026 §3.3): the spawn gate's own code.
        assert_eq!(LaneFailure::ShuttingDown.code(), "shutting_down");
        assert!(LaneFailure::ShuttingDown
            .message()
            .starts_with("shutting_down"));
        assert!(seen.insert("shutting_down"));
        // A verb on a FINISHED lane (plan 032 §3.1.1 item 8): its own code,
        // and the words the panel's action-error line shows.
        assert_eq!(LaneFailure::Ended.code(), "lane_ended");
        assert_eq!(LaneFailure::Ended.message(), "the session has ended");
        assert!(seen.insert("lane_ended"));
        // A craze verb whose answer was lost: its own code, recognised by the
        // adapter's one test — the settings sheet's "not confirmed" (plan 025
        // §3.10) — and its message whole.
        let lost = LaneFailure::from(shed_craze::errors::outcome_unknown(
            "the connection to craze dropped; check the transcript",
        ));
        assert_eq!(lost.code(), "outcome_unknown");
        assert!(
            lost.message().starts_with("outcome unknown"),
            "{}",
            lost.message()
        );
        assert!(seen.insert("outcome_unknown"));
        // …and ONLY one the adapter constructed: a definite craze failure
        // whose own words happen to begin the same way is `failed` (C11
        // review), never taken for a lost answer.
        let said = LaneFailure::from(shed_craze::lane_error(
            &shed_craze::wire::RpcError::from_value(&json!({
                "code": -32000, "message": "outcome unknown: x",
                "data": {"code": "failed", "reason": "failed"}
            })),
        ));
        assert_eq!(said.code(), "failed", "{}", said.message());
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

    /// `lane.set`'s `change` is the contract's [`LaneSettingChange`], read
    /// strictly: the three forms, verbatim ids, and nothing else — an unknown
    /// kind, a field beside the form, a config change with no value, an empty
    /// id or a non-object are each refused `bad_request`.
    #[test]
    fn the_setting_change_forms_are_strict() {
        assert_eq!(
            parse_setting(&json!({"kind": "model", "id": "claude-opus-5"})).unwrap(),
            LaneSettingChange::Model {
                id: "claude-opus-5".into()
            }
        );
        assert_eq!(
            parse_setting(&json!({"kind": "mode", "id": "plan"})).unwrap(),
            LaneSettingChange::Mode { id: "plan".into() }
        );
        assert_eq!(
            parse_setting(&json!({"kind": "config", "id": "fast", "value": " true "})).unwrap(),
            LaneSettingChange::Config {
                id: "fast".into(),
                value: " true ".into(),
                for_model: None,
            },
            "ids and values are opaque: never trimmed"
        );
        // Amendment A13: the model the client displayed the option for.
        assert_eq!(
            parse_setting(
                &json!({"kind": "config", "id": "fast", "value": "true", "for_model": "grok-4.6"})
            )
            .unwrap(),
            LaneSettingChange::Config {
                id: "fast".into(),
                value: "true".into(),
                for_model: Some("grok-4.6".into()),
            }
        );
        for bad in [
            json!({"kind": "effort", "id": "high"}),
            json!({"kind": "model", "id": "m", "value": "x"}),
            json!({"kind": "config", "id": "fast", "value": "true", "forModel": "m"}),
            json!({"kind": "config", "id": "fast", "value": "true", "for_model": ""}),
            json!({"kind": "model", "id": "m", "for_model": "m0"}),
            json!({"kind": "config", "id": "fast"}),
            json!({"kind": "model", "id": ""}),
            json!({"kind": "config", "id": "", "value": "true"}),
            json!({"id": "m"}),
            json!("model"),
        ] {
            let refused = parse_setting(&bad).expect_err(&bad.to_string());
            assert_eq!(refused.code(), "bad_request", "{bad}");
        }
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
        /// `close_now` calls (plan 026 §3.3): the shutdown's close through the
        /// trait, whatever the forward's reference count.
        closed: AtomicUsize,
        /// When non-zero, the `ensure` with this ordinal (1-based) HANGS until
        /// the forward is closed — an `ensure` in flight at shutdown.
        hang_on: AtomicUsize,
        /// Wakes a hung `ensure` when the forward is closed.
        unhang: tokio::sync::Notify,
        /// Set — a moment late, on purpose — when a hung `ensure`'s future is
        /// DROPPED: the proof a pump was awaited, not merely aborted.
        hung_dropped: AtomicBool,
    }

    /// Marks a hung `ensure`'s future dropped, slowly — so a caller that
    /// aborted the pump without awaiting it returns before this lands.
    struct SlowDrop(Arc<ForwardLog>);

    impl Drop for SlowDrop {
        fn drop(&mut self) {
            std::thread::sleep(Duration::from_millis(300));
            self.0.hung_dropped.store(true, SeqCst);
        }
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
            let n = self.log.ensures.fetch_add(1, SeqCst) + 1;
            if self.log.hang_on.load(SeqCst) == n {
                let _drop = SlowDrop(Arc::clone(&self.log));
                loop {
                    let unhang = self.log.unhang.notified();
                    if self.log.closed.load(SeqCst) > 0 {
                        return Err(ForwardError("the forward was closed".to_string()));
                    }
                    unhang.await;
                }
            }
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

        fn close_now(&self) -> bool {
            self.log.alive.store(false, SeqCst);
            self.log.closed.fetch_add(1, SeqCst);
            self.log.unhang.notify_waiters();
            true
        }
    }

    /// A forward whose `ensure` starts a REAL child (`/bin/sh` writing its pid,
    /// then `sleep`) and then waits — an `ensure` mid-spawn — until closed.
    struct SpawningForward {
        port: u16,
        pids: std::path::PathBuf,
        child: Mutex<Option<std::process::Child>>,
        closed: AtomicBool,
        wake: tokio::sync::Notify,
    }

    #[async_trait::async_trait]
    impl MachineForward for SpawningForward {
        fn port(&self) -> u16 {
            self.port
        }

        async fn ensure(&self) -> Result<(), ForwardError> {
            let child = std::process::Command::new("/bin/sh")
                .arg("-c")
                .arg(format!(
                    "printf '%s\\n' $$ >> '{}'; exec sleep 30",
                    self.pids.display()
                ))
                .spawn()
                .map_err(|e| ForwardError(e.to_string()))?;
            *lock(&self.child) = Some(child);
            loop {
                let wake = self.wake.notified();
                if self.closed.load(SeqCst) {
                    return Err(ForwardError("the forward was closed".to_string()));
                }
                wake.await;
            }
        }

        fn close_now(&self) -> bool {
            self.closed.store(true, SeqCst);
            if let Some(mut child) = lock(&self.child).take() {
                let _ = child.kill();
                let _ = child.wait();
            }
            self.wake.notify_waiters();
            true
        }
    }

    impl Drop for SpawningForward {
        fn drop(&mut self) {
            if let Some(mut child) = lock(&self.child).take() {
                let _ = child.kill();
                let _ = child.wait();
            }
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
        /// When set, every forward is a [`SpawningForward`] writing its child's
        /// pid here (plan 026 §3.3's mid-spawn cell).
        spawning: Mutex<Option<std::path::PathBuf>>,
        /// The craze source's generation [`LaneMachines::craze_source`]
        /// answers — moved by a cell to play a host removed and registered
        /// again (plan 032 §3.1.1 item 12).
        craze_gen: std::sync::atomic::AtomicU64,
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
                .map(|(source, _)| (source.clone(), self.craze_gen.load(SeqCst)))
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
            if let Some(pids) = lock(&self.spawning).clone() {
                return Ok(Box::new(SpawningForward {
                    port: self.port,
                    pids,
                    child: Mutex::new(None),
                    closed: AtomicBool::new(false),
                    wake: tokio::sync::Notify::new(),
                }));
            }
            Ok(Box::new(FakeForward {
                port: self.port,
                log: Arc::clone(&self.log),
            }))
        }
    }

    /// Every `lane-event` the layer emitted, by kind — and every `Down`'s
    /// reason, in order (plan 032: exactly one terminal `Down`).
    #[derive(Default)]
    struct Recorder {
        kinds: Mutex<Vec<&'static str>>,
        downs: Mutex<Vec<String>>,
    }

    impl Recorder {
        fn record(&self, event: &LaneEvent) {
            if let LaneEvent::Down { reason } = event {
                lock(&self.downs).push(reason.clone());
            }
            lock(&self.kinds).push(match event {
                LaneEvent::Reset { .. } => "reset",
                LaneEvent::Ready { .. } => "ready",
                LaneEvent::Down { .. } => "down",
                LaneEvent::Message { .. } => "message",
                LaneEvent::Session { .. } => "session",
                LaneEvent::Approval { .. } => "approval",
                LaneEvent::Capabilities { .. } => "capabilities",
                LaneEvent::Settings { .. } => "settings",
                LaneEvent::Slash { .. } => "slash",
                LaneEvent::Stale { .. } => "stale",
                LaneEvent::Unknown => "unknown",
            });
        }

        fn count(&self, kind: &str) -> usize {
            lock(&self.kinds).iter().filter(|k| **k == kind).count()
        }

        fn downs(&self) -> Vec<String> {
            lock(&self.downs).clone()
        }

        fn total(&self) -> usize {
            lock(&self.kinds).len()
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
        let (lanes, log, recorder, _machines) = lanes_with_machines(port, rows, craze, None);
        (lanes, log, recorder)
    }

    /// The departure grace every cell runs with (plan 032 §3.1's test seam):
    /// short enough to wait out, long enough that a cell's own steps fit
    /// inside it.
    const TEST_GRACE: Duration = Duration::from_millis(300);

    /// [`lanes_with_craze`], and the machine layer itself (a cell that moves
    /// its craze rows or generation), every forward a [`SpawningForward`]
    /// writing to `spawning` when given. The departure grace is
    /// [`TEST_GRACE`].
    fn lanes_with_machines(
        port: u16,
        rows: BTreeMap<String, AgentLaneStamp>,
        craze: Option<(CrazeSource, Vec<String>)>,
        spawning: Option<&std::path::Path>,
    ) -> (
        Arc<Lanes>,
        Arc<ForwardLog>,
        Arc<Recorder>,
        Arc<FakeMachines>,
    ) {
        let log = Arc::new(ForwardLog::default());
        let recorder = Arc::new(Recorder::default());
        let machines = Arc::new(FakeMachines {
            lanes: Mutex::new(rows),
            port,
            log: Arc::clone(&log),
            reach: ReachKind::Ssh(fake_entry()),
            craze: Mutex::new(craze),
            spawning: Mutex::new(spawning.map(std::path::Path::to_path_buf)),
            craze_gen: std::sync::atomic::AtomicU64::new(1),
        });
        let sink: EventSink = {
            let recorder = Arc::clone(&recorder);
            Arc::new(
                move |_machine: &str, _kind: &str, _session: &str, event: &LaneEvent| {
                    recorder.record(event)
                },
            )
        };
        let lanes = Arc::new(
            Lanes::with_sink(
                tokio::runtime::Handle::current(),
                sink,
                Arc::clone(&machines) as Arc<dyn LaneMachines>,
            )
            .with_depart_grace(TEST_GRACE),
        );
        (lanes, log, recorder, machines)
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

    /// **`lane.messages` carries the LIVE session row; `lane.open`'s is the one
    /// it opened with** (plan 025 §3.6.5; live leg 1's permission-line
    /// finding). `lane.open` caches its row for the life of the entry — which
    /// is what makes it idempotent — so a row fact the stream states later (an
    /// activity here; for a craze session created a moment before, the attach
    /// info document's permission mode) reaches a client only through
    /// `lane.messages`' `session`. The panel's header reads it from there.
    #[tokio::test]
    async fn the_live_session_row_rides_lane_messages_and_lane_open_keeps_its_own() {
        let fake = one_session("ses_a").await;
        let (lanes, _log, _events) = lanes_for(&fake, &["ses_a"]);

        let opened = lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        assert_eq!(opened["session"]["activity"], "idle");
        wait_for("the first generation to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;
        let view = lanes.messages(MACHINE, OC, "ses_a").expect("lane.messages");
        assert_eq!(view["session"]["id"], "ses_a", "the seed's row: {view}");
        assert_eq!(view["session"]["title"], "the lane");

        // The agent starts a turn: the stream's next `Session` says so.
        fake.set_status("ses_a", "busy");
        fake.push_event(&json!({"type": "session.status",
            "properties": {"sessionID": "ses_a", "status": {"type": "busy"}}}));
        let live = wait_for("the live row to say working", || {
            let v = lanes.messages(MACHINE, OC, "ses_a").ok()?;
            (v["session"]["activity"] == "working").then_some(v)
        })
        .await;
        assert_eq!(live["activity"], "working", "the same row's activity");
        let again = lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("an idempotent re-open");
        assert_eq!(
            again["session"]["activity"], "idle",
            "lane.open re-answers the row it opened with — which is why a \
             client reads the row's facts from lane.messages"
        );
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
        // Re-based on the lifecycle (plan 032 §3.1.1): another generation's
        // news leaves it LIVE; its own generation's starts its departure, and
        // the grace's end finishes it — readable, never removed.
        let gone = [ID.to_string()];
        let departed = CrazeNews::Departed {
            host_ids: &gone,
            cause: Departure::Removed,
            seq: 1,
        };
        lanes.craze_news(MACHINE, 2, departed);
        assert_eq!(
            lanes.lane_state(MACHINE, CRAZE, ID),
            Some("live"),
            "another generation's eviction leaves it alone"
        );
        lanes.craze_news(MACHINE, 1, departed);
        assert_eq!(lanes.lane_state(MACHINE, CRAZE, ID), Some("departing"));
        wait_for("the grace to end the lane", || {
            (lanes.lane_state(MACHINE, CRAZE, ID) == Some("finished")).then_some(())
        })
        .await;
        assert!(
            lanes.is_open(MACHINE, CRAZE, ID),
            "a finished craze lane stays, readable"
        );
        let view = lanes.messages(MACHINE, CRAZE, ID).expect("still readable");
        assert_eq!(view["finished"], true, "{view}");
        assert_eq!(view["stale"], DOWN_SESSION_GONE, "{view}");
    }

    /// **A craze lane opened on a row that states no permission mode shows the
    /// one its attach states, on `lane.messages`** (plan 025 §3.6.5; live leg
    /// 1's permission-line finding). The panel the create sheet opens at once
    /// is opened on the create's own row, which says nothing of the session's
    /// posture; `lane.open` caches that row for the life of the lane. The
    /// attach's info document says `bypass`, the watcher's `Session` carries
    /// it, and `lane.messages`' `session` is where the panel's header reads it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_craze_lanes_permission_mode_arrives_on_lane_messages_not_lane_open() {
        use shed_core::lane::AgentSource as _;
        use shed_craze::testing::{
            attach_result, full_hub_capabilities, host_session_row, roster_row, session_caps,
            session_info, snapshot_at, ScriptedDial,
        };

        const HOST: &str = "0123456789ab";
        const SID: &str = "craze-1";
        const INC: &str = "INCARNATION-1";
        let (dial, mut conns) = ScriptedDial::new();
        let source = CrazeSource::new(dial, "shed-desktop-test");
        let _roster = source.subscribe().await.expect("subscribe");
        let mut hub = conns.recv().await.expect("the roster dialled");
        hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
        // A row with no permission mode — what a just-created session's is.
        hub.subscribed(
            "sub-1",
            "0a1b2c3d4e5f",
            json!([roster_row(HOST, SID, "/w", json!({"title": "w-new"}))]),
        )
        .await;
        wait_for("the source lists the row", || {
            source.listed(HOST).map(|_| ())
        })
        .await;
        let (lanes, _log, _recorder) = lanes_with_craze(
            REMOTE_PORT,
            BTreeMap::new(),
            Some((source, vec![HOST.to_string()])),
        );

        let opened = lanes
            .open(MACHINE, CRAZE, HOST)
            .await
            .expect("the craze lane");
        assert_eq!(
            opened["session"].get("permission_mode"),
            None,
            "opened on a row that says none: {opened}"
        );

        // The lane's own connection: the host's row, then an attach whose info
        // document says `bypass`.
        let mut conn = conns.recv().await.expect("the lane dialled");
        conn.splice(HOST).await;
        let mut info = session_info(HOST, SID, INC, session_caps(true));
        info["permissionMode"] = json!("bypass");
        conn.listed(host_session_row(&info, json!({"title": "w-new"})))
            .await;
        conn.attached(attach_result(
            "s-1",
            &info,
            (INC, 1),
            Some(snapshot_at(INC, 1, json!({}))),
            None,
        ))
        .await;
        conn.synchronized("s-1", 1).await;

        let view = wait_for("the seed's row on lane.messages", || {
            let v = lanes.messages(MACHINE, CRAZE, HOST).ok()?;
            (!v["session"].is_null()).then_some(v)
        })
        .await;
        assert_eq!(view["session"]["id"], HOST);
        assert_eq!(
            view["session"]["permission_mode"], "bypass",
            "the attach's permission mode, on the live row: {view}"
        );
        let again = lanes
            .open(MACHINE, CRAZE, HOST)
            .await
            .expect("an idempotent re-open");
        assert_eq!(
            again["session"].get("permission_mode"),
            None,
            "lane.open keeps the row it opened with"
        );
    }

    /// **`lane.set_title`'s answer** (plan 029 P19, E4): a rename that took is
    /// `{}`; one that took and craze could not RECORD is a success carrying
    /// the adapter's whole warning — never an error, so the panel clears the
    /// draft and shows it; every other error keeps its code, an unknown
    /// outcome included.
    #[test]
    fn a_rename_craze_could_not_record_is_a_warning_not_a_failure() {
        assert_eq!(renamed(Ok(())).unwrap(), json!({}));
        let unrecorded = LaneError::Failed(format!(
            "{}disk full",
            shed_craze::errors::RENAMED_UNRECORDED
        ));
        assert_eq!(
            renamed(Err(unrecorded)).unwrap(),
            json!({ "warning": "renamed, but craze could not record it: disk full" })
        );
        for (e, code) in [
            (LaneError::Failed("index_write".into()), "failed"),
            (LaneError::BadRequest("params.title".into()), "bad_request"),
            (LaneError::NotAccepting, "not_accepting"),
            (
                shed_craze::errors::outcome_unknown("the connection dropped"),
                "outcome_unknown",
            ),
            // craze's OWN words that happen to begin like the warning are
            // escaped by the adapter, and are a failure here.
            (
                LaneError::Failed(shed_craze::errors::craze_says(
                    "renamed, but craze could not record it: no",
                )),
                "failed",
            ),
        ] {
            let what = e.to_string();
            assert_eq!(renamed(Err(e)).unwrap_err().code(), code, "{what}");
        }
    }

    /// **The slash menu rides `lane.messages`, and a rename goes out as
    /// `session.setTitle`** (plan 029 D12, P10): a craze session whose
    /// capabilities say `slashCatalog` and whose snapshot carries a `slash`
    /// section is answered with the RESOLVED menu (the agent's prompt row,
    /// shown); `lane.set_title` trims and sends the title once, and craze's
    /// `index_write` comes back as `{warning}`, a definite refusal as its own
    /// code — and a blank title is refused with nothing sent.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_craze_lane_answers_its_slash_menu_and_renames() {
        use shed_core::lane::AgentSource as _;
        use shed_craze::testing::{
            attach_result, full_hub_capabilities, host_session_row, roster_row, session_info,
            slash_items, slash_session_caps, snapshot_at, ScriptedDial,
        };

        const HOST: &str = "0123456789ab";
        const SID: &str = "craze-1";
        const INC: &str = "INCARNATION-1";
        let (dial, mut conns) = ScriptedDial::new();
        let source = CrazeSource::new(dial, "shed-desktop-test");
        let _roster = source.subscribe().await.expect("subscribe");
        let mut hub = conns.recv().await.expect("the roster dialled");
        hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
        hub.subscribed(
            "sub-1",
            "0a1b2c3d4e5f",
            json!([roster_row(HOST, SID, "/w", json!({"title": "before"}))]),
        )
        .await;
        wait_for("the source lists the row", || {
            source.listed(HOST).map(|_| ())
        })
        .await;
        let (lanes, _log, _recorder) = lanes_with_craze(
            REMOTE_PORT,
            BTreeMap::new(),
            Some((source, vec![HOST.to_string()])),
        );
        lanes
            .open(MACHINE, CRAZE, HOST)
            .await
            .expect("the craze lane");
        let mut conn = conns.recv().await.expect("the lane dialled");
        conn.splice(HOST).await;
        let info = session_info(HOST, SID, INC, slash_session_caps(true));
        conn.listed(host_session_row(&info, json!({"title": "before"})))
            .await;
        let mut snap = snapshot_at(INC, 1, json!({}));
        snap["settings"]["slash"] = slash_items(&["research"]);
        conn.attached(attach_result("s-1", &info, (INC, 1), Some(snap), None))
            .await;
        conn.synchronized("s-1", 1).await;

        let view = wait_for("the seed's slash menu on lane.messages", || {
            let v = lanes.messages(MACHINE, CRAZE, HOST).ok()?;
            (!v["slash_menu"].is_null()).then_some(v)
        })
        .await;
        assert_eq!(view["capabilities"]["slash"], true, "{view}");
        assert_eq!(view["capabilities"]["set_title"], true, "{view}");
        let rows = view["slash_menu"]["rows"].as_array().expect("rows");
        assert_eq!(rows.len(), 1, "{view}");
        assert_eq!(rows[0]["name"], "research");
        assert_eq!(rows[0]["action"], json!({"kind": "prompt"}));

        // A blank title: refused here, nothing sent.
        let blank = lanes.set_title(MACHINE, CRAZE, HOST, "  \t ").await;
        assert_eq!(blank.unwrap_err().code(), "bad_request");

        // index_write: the rename took, the index did not record it.
        let renaming = {
            let lanes = Arc::clone(&lanes);
            tokio::spawn(async move {
                lanes
                    .set_title(MACHINE, CRAZE, HOST, "  the new name ")
                    .await
            })
        };
        let req = conn.expect("session.setTitle").await;
        assert_eq!(req["params"]["title"], "the new name", "trimmed: {req}");
        assert_eq!(req["params"]["sessionId"], SID);
        conn.refuse(
            &req,
            "index_write",
            "index_write",
            json!({"cause": "disk full"}),
        )
        .await;
        assert_eq!(
            renaming.await.unwrap().expect("a warning, not a failure"),
            json!({ "warning": "renamed, but craze could not record it: disk full" })
        );

        // A definite refusal keeps its code.
        let refusing = {
            let lanes = Arc::clone(&lanes);
            tokio::spawn(async move { lanes.set_title(MACHINE, CRAZE, HOST, "again").await })
        };
        let req = conn.expect("session.setTitle").await;
        conn.refuse(&req, "bad_request", "bad_request", json!({}))
            .await;
        assert_eq!(refusing.await.unwrap().unwrap_err().code(), "bad_request");
    }

    // -----------------------------------------------------------------------
    // a craze lane outlives its row by a grace (plan 032 §3.1.1, §7 W1)
    //
    // The REAL `CrazeSource` and `CrazeLane` over a SCRIPTED hub (the test
    // plays craze line by line), a machine layer whose craze rows and source
    // generation a cell moves, and a `TEST_GRACE` departure grace. The
    // lifecycle's own races (exactly one terminal `Down`, nothing applied
    // after the finish or the shutdown) are pinned on a bare `LaneCore`
    // below them, where the interleavings can be forced.
    // -----------------------------------------------------------------------

    use shed_craze::testing::HubEnd;

    /// The craze session every grace cell's lane is on.
    const GHOST: &str = "0123456789ab";
    const GSID: &str = "craze-1";
    const GINC: &str = "INCARNATION-1";

    /// One craze lane's rig: a source over a scripted hub listing [`GHOST`],
    /// and a `Lanes` over it whose machine layer lists the row too.
    struct CrazeRig {
        lanes: Arc<Lanes>,
        events: Arc<Recorder>,
        machines: Arc<FakeMachines>,
        source: CrazeSource,
        /// The roster's connection — a cell's `roster` frames go out on it.
        hub: HubEnd,
        conns: tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
        _roster: shed_core::lane::SourceSubscription,
        /// The last news number [`Self::seq`] minted — the source's
        /// [`crate::craze::CrazeState::mint_news`], played by the cell.
        seq: std::sync::atomic::AtomicU64,
    }

    impl CrazeRig {
        async fn start() -> CrazeRig {
            CrazeRig::start_with(None, shed_craze::LaneTimings::default()).await
        }

        /// [`Self::start`], its craze lanes on `lane` clocks, and — given an
        /// opencode fake — the machine also stamping that fake's `ses_a` (its
        /// tunnels landing on the fake's port), for a cell about two lanes of
        /// two kinds at once.
        async fn start_with(
            opencode: Option<&FakeOpencode>,
            lane: shed_craze::LaneTimings,
        ) -> CrazeRig {
            use shed_core::lane::AgentSource as _;
            use shed_craze::testing::{full_hub_capabilities, roster_row, ScriptedDial};

            let (dial, mut conns) = ScriptedDial::new();
            let source = CrazeSource::new(dial, "shed-desktop-test").with_lane_timings(lane);
            let roster = source.subscribe().await.expect("subscribe");
            let mut hub = conns.recv().await.expect("the roster dialled");
            hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
            hub.subscribed(
                "sub-1",
                "0a1b2c3d4e5f",
                json!([roster_row(GHOST, GSID, "/w", json!({"title": "w"}))]),
            )
            .await;
            wait_for("the source lists the row", || {
                source.listed(GHOST).map(|_| ())
            })
            .await;
            let (port, rows) = match opencode {
                Some(fake) => (fake.addr().port(), lane_rows(&["ses_a"])),
                None => (REMOTE_PORT, BTreeMap::new()),
            };
            let (lanes, _log, events, machines) = lanes_with_machines(
                port,
                rows,
                Some((source.clone(), vec![GHOST.to_string()])),
                None,
            );
            CrazeRig {
                lanes,
                events,
                machines,
                source,
                hub,
                conns,
                _roster: roster,
                seq: std::sync::atomic::AtomicU64::new(0),
            }
        }

        /// Open the lane and seed it; answers the lane's own connection.
        async fn open_seeded(&mut self) -> HubEnd {
            use shed_craze::testing::{
                attach_result, host_session_row, session_caps, session_info, snapshot_at,
            };

            self.lanes
                .open(MACHINE, CRAZE, GHOST)
                .await
                .expect("the craze lane opens");
            let mut conn = self.conns.recv().await.expect("the lane dialled");
            conn.splice(GHOST).await;
            let info = session_info(GHOST, GSID, GINC, session_caps(true));
            conn.listed(host_session_row(&info, json!({"title": "w"})))
                .await;
            conn.attached(attach_result(
                "s-1",
                &info,
                (GINC, 1),
                Some(snapshot_at(GINC, 1, json!({}))),
                None,
            ))
            .await;
            conn.synchronized("s-1", 1).await;
            wait_for("the seed to swap in", || {
                let v = self.lanes.messages(MACHINE, CRAZE, GHOST).ok()?;
                (v["generation"].as_u64() >= Some(1)).then_some(())
            })
            .await;
            conn
        }

        fn news(&self, gen: u64, news: CrazeNews<'_>) {
            self.lanes.craze_news(MACHINE, gen, news);
        }

        /// The source's next news number: a piece the cell publishes at once
        /// is minted as it is published; one minted and published LATER plays
        /// a publisher paused between its lock and its publication.
        fn seq(&self) -> u64 {
            self.seq.fetch_add(1, SeqCst) + 1
        }

        /// The row left the source (generation 1), for `cause`.
        fn depart(&self, cause: Departure) {
            self.depart_as(cause, self.seq());
        }

        /// [`Self::depart`], as news number `seq`.
        fn depart_as(&self, cause: Departure, seq: u64) {
            self.news(
                1,
                CrazeNews::Departed {
                    host_ids: &[GHOST.to_string()],
                    cause,
                    seq,
                },
            );
        }

        /// The row is listed again (generation 1).
        fn returned(&self) {
            self.returned_as(self.seq());
        }

        /// [`Self::returned`], as news number `seq`.
        fn returned_as(&self, seq: u64) {
            self.news(
                1,
                CrazeNews::Returned {
                    host_ids: &[GHOST.to_string()],
                    seq,
                },
            );
        }

        /// The host was removed (generation `gen`).
        fn host_removed(&self, gen: u64) {
            self.news(gen, CrazeNews::HostRemoved { seq: self.seq() });
        }

        /// What the machine layer lists — `false` plays the row gone, so a
        /// stamp no longer resolves.
        fn listed(&self, listed: bool) {
            if let Some((_, ids)) = lock(&self.machines.craze).as_mut() {
                ids.clear();
                if listed {
                    ids.push(GHOST.to_string());
                }
            }
        }

        fn state(&self) -> Option<&'static str> {
            self.lanes.lane_state(MACHINE, CRAZE, GHOST)
        }

        fn entry(&self) -> Arc<LaneEntry> {
            self.lanes
                .entry(&key(MACHINE, CRAZE, GHOST))
                .expect("the entry")
        }

        fn token(&self) -> Option<u64> {
            match lock(&self.entry().core.state).state {
                EntryState::Departing { token, .. } => Some(token),
                _ => None,
            }
        }

        fn messages(&self) -> Value {
            self.lanes
                .messages(MACHINE, CRAZE, GHOST)
                .expect("lane.messages answers")
        }

        async fn finished(&self, what: &str) -> Value {
            wait_for(what, || (self.state() == Some("finished")).then_some(())).await;
            self.messages()
        }
    }

    /// **W1 cell 1 — a vanished host ends `session_gone`, within the grace
    /// plus 1 s.** The row leaves and no final event follows: the lane keeps
    /// its connection for the grace, then THIS layer ends it — one
    /// `Down{session_gone}`, `finished`, still readable — and retires it: the
    /// adapter is released (its connection closes) and every verb is refused
    /// `lane_ended`.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_vanished_hosts_lane_ends_session_gone_within_the_grace() {
        let mut rig = CrazeRig::start().await;
        let mut conn = rig.open_seeded().await;
        let departed = std::time::Instant::now();
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"));
        assert_eq!(rig.messages()["finished"], false, "departing is not over");
        let view = rig.finished("the grace to end the lane").await;
        let took = departed.elapsed();
        assert!(
            took >= TEST_GRACE && took <= TEST_GRACE + Duration::from_secs(1),
            "ended {took:?} after the row left (grace {TEST_GRACE:?})"
        );
        assert_eq!(rig.events.downs(), [DOWN_SESSION_GONE]);
        assert_eq!(view["finished"], true);
        assert_eq!(view["ended"], true);
        assert_eq!(view["stale"], DOWN_SESSION_GONE);
        assert!(
            lock(&rig.entry().core.client).is_none(),
            "the adapter released"
        );
        // The released lane's connection closes (its watcher went with it).
        let started = std::time::Instant::now();
        assert!(conn.recv_within(Duration::from_secs(5)).await.is_none());
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "closed, not idle"
        );
        let refused = rig
            .lanes
            .send(MACHINE, CRAZE, GHOST, "anyone?", SendMode::Queue)
            .await
            .expect_err("an ended lane takes no verb");
        assert_eq!(refused.code(), "lane_ended");
        assert_eq!(refused.message(), "the session has ended");
        assert!(rig.lanes.approvals(MACHINE, CRAZE, GHOST).is_ok());
    }

    /// **W1 cell 2 — a return within the grace keeps the lane live, and a
    /// later send works.**
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_return_within_the_grace_keeps_the_lane_live() {
        let mut rig = CrazeRig::start().await;
        let mut conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"));
        rig.returned();
        assert_eq!(rig.state(), Some("live"), "the return cancels it");
        assert!(lock(&rig.entry().grace).is_none(), "its grace task went");
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(rig.state(), Some("live"), "and nothing ends it later");
        assert!(rig.events.downs().is_empty());
        let sending = {
            let lanes = Arc::clone(&rig.lanes);
            tokio::spawn(async move {
                lanes
                    .send(MACHINE, CRAZE, GHOST, "still here", SendMode::Queue)
                    .await
            })
        };
        let req = conn.expect("session.prompt").await;
        assert_eq!(req["params"]["text"], "still here");
        conn.reply(&req, json!({"turn": "turn-1", "text": "still here"}))
            .await;
        sending
            .await
            .expect("the send task")
            .expect("a returned lane takes verbs");
    }

    /// **W1 cell 3 — a departure after a return gets a full, fresh grace**
    /// (PC2): a new token, and an end measured from the SECOND removal —
    /// the first departure's deadline passes with the lane still departing.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_departure_after_a_return_gets_a_full_fresh_grace() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        let first = rig.token().expect("departing");
        tokio::time::sleep(TEST_GRACE * 2 / 3).await;
        rig.returned();
        let again = std::time::Instant::now();
        rig.depart(Departure::Removed);
        let second = rig.token().expect("departing again");
        assert_ne!(first, second, "a fresh departure, a fresh token");
        // Past the FIRST departure's deadline.
        tokio::time::sleep(TEST_GRACE / 2).await;
        assert_eq!(
            rig.state(),
            Some("departing"),
            "the first grace's task did not end the second departure"
        );
        rig.finished("the second grace to end").await;
        assert!(
            again.elapsed() >= TEST_GRACE,
            "a full grace from the second removal"
        );
        assert_eq!(rig.events.downs(), [DOWN_SESSION_GONE]);
    }

    /// **W1 cell 4 — a duplicate removal never extends the grace** (PC2):
    /// the same token, and the end lands at the FIRST removal's deadline.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_duplicate_removal_does_not_extend_the_grace() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        let departed = std::time::Instant::now();
        rig.depart(Departure::Removed);
        let token = rig.token().expect("departing");
        tokio::time::sleep(TEST_GRACE / 2).await;
        rig.depart(Departure::Removed);
        rig.depart(Departure::HubGone);
        assert_eq!(rig.token(), Some(token), "the same departure");
        rig.finished("the first grace to end").await;
        assert!(
            departed.elapsed() < TEST_GRACE + TEST_GRACE / 2,
            "ended {:?} after the FIRST removal — extended by the duplicate",
            departed.elapsed()
        );
        assert_eq!(
            rig.events.downs(),
            [DOWN_SESSION_GONE],
            "the first removal's cause, too"
        );
    }

    /// **W1 cell 5 — the host removed during a grace ends the lane
    /// `host_removed` AT ONCE** (PC3, PC4): no waiting out the grace, its
    /// adapter released, its grace task gone — and `lane.messages` still
    /// answers.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_host_removed_during_a_grace_ends_the_lane_at_once() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        rig.listed(false);
        rig.host_removed(1);
        assert_eq!(rig.state(), Some("finished"), "at once, no grace");
        assert_eq!(rig.events.downs(), [DOWN_HOST_REMOVED]);
        let entry = rig.entry();
        assert!(lock(&entry.core.client).is_none(), "the adapter released");
        assert!(lock(&entry.grace).is_none(), "the grace task cancelled");
        let view = rig.messages();
        assert_eq!(view["finished"], true);
        assert_eq!(view["stale"], DOWN_HOST_REMOVED);
        // The cancelled grace never speaks again.
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(rig.events.downs(), [DOWN_HOST_REMOVED], "one terminal Down");
    }

    /// **W1 cell 6 — old-generation news touches nothing** (item 12): the
    /// host was registered again (the source's generation is now 2), and the
    /// OLD source's departure, removal and return all leave the new lane be;
    /// its own generation's news governs it. The old news is numbered past
    /// anything the new lane takes, and is still refused — and never advances
    /// the lane's news mark (C1 re-review): the generation fence comes first,
    /// so the current generation's lower numbers still govern.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_old_generations_news_touches_nothing() {
        let mut rig = CrazeRig::start().await;
        rig.machines.craze_gen.store(2, SeqCst);
        let _conn = rig.open_seeded().await;
        let gone = [GHOST.to_string()];
        rig.news(
            1,
            CrazeNews::Departed {
                host_ids: &gone,
                cause: Departure::Removed,
                seq: u64::MAX,
            },
        );
        assert_eq!(rig.state(), Some("live"), "an old departure");
        rig.news(1, CrazeNews::HostRemoved { seq: u64::MAX });
        assert_eq!(rig.state(), Some("live"), "an old removal");
        rig.news(
            2,
            CrazeNews::Departed {
                host_ids: &gone,
                cause: Departure::Removed,
                seq: rig.seq(),
            },
        );
        assert_eq!(rig.state(), Some("departing"));
        rig.news(
            1,
            CrazeNews::Returned {
                host_ids: &gone,
                seq: u64::MAX,
            },
        );
        assert_eq!(rig.state(), Some("departing"), "an old return");
        rig.news(
            2,
            CrazeNews::Returned {
                host_ids: &gone,
                seq: rig.seq(),
            },
        );
        assert_eq!(rig.state(), Some("live"));
        assert!(rig.events.downs().is_empty());
    }

    /// **An open in flight carries the news fence into its entry** (C1
    /// re-review): craze news reaches an open between its declaration and its
    /// commit ([`Pending::news`]). Two departures are minted, their
    /// publishers paused, and then the return that answers them; the return
    /// reaches the open first. The first stale departure lands on the open and
    /// does not cancel it; the second lands once it committed and does not
    /// depart it — the entry starts its news mark where its open's stood. A
    /// departure minted after all of them still does.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_open_in_flight_carries_the_news_fence_into_its_entry() {
        let rig = CrazeRig::start().await;
        let (first, second, back) = (rig.seq(), rig.seq(), rig.seq());
        let reached = Arc::new(tokio::sync::Notify::new());
        let proceed = Arc::new(tokio::sync::Notify::new());
        *lock(&rig.lanes.commit_gap) = Some((Arc::clone(&reached), Arc::clone(&proceed)));
        let opening = {
            let lanes = Arc::clone(&rig.lanes);
            tokio::spawn(async move { lanes.open(MACHINE, CRAZE, GHOST).await })
        };
        reached.notified().await;
        rig.returned_as(back);
        rig.depart_as(Departure::Removed, first);
        proceed.notify_one();
        opening
            .await
            .expect("the open task")
            .expect("a departure older than the open's return does not cancel it");
        assert_eq!(rig.state(), Some("live"));
        rig.depart_as(Departure::Removed, second);
        assert_eq!(
            rig.state(),
            Some("live"),
            "an older departure, delivered after the commit, does not depart it"
        );
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"), "a newer one does");
    }

    /// **W1 cell 7 — the session's final `Down` racing the expiry: whichever
    /// lands first, exactly ONE terminal `Down`.** Here the session's own
    /// `reset{session_closed}` lands inside the grace: the lane ends as a
    /// normal end does, the grace task wakes on `finished` and leaves, and
    /// the grace's deadline adds nothing. (Both orders, forced, are pinned on
    /// a bare core: `exactly_one_terminal_down_whoever_lands_first`.)
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_sessions_final_down_inside_the_grace_ends_the_lane_once() {
        let mut rig = CrazeRig::start().await;
        let mut conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        conn.reset("s-1", "session_closed").await;
        let view = rig.finished("the session's close").await;
        assert_eq!(view["stale"], "session_closed");
        wait_for("the grace task to leave on `finished`", || {
            lock(&rig.entry().grace)
                .as_ref()
                .is_some_and(tokio::task::JoinHandle::is_finished)
                .then_some(())
        })
        .await;
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(rig.events.downs(), ["session_closed"], "one terminal Down");
    }

    /// **W1 cell 10 — `open` on a departing entry returns it** (item 9, PC5):
    /// its row no longer resolves, and the lane is still the lane — no new
    /// adapter, no new connection, still departing.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn open_on_a_departing_entry_returns_it() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        rig.listed(false);
        let before = rig.entry();
        let opened = rig
            .lanes
            .open(MACHINE, CRAZE, GHOST)
            .await
            .expect("a departing lane re-opens");
        assert_eq!(opened["session"]["id"], GHOST);
        assert!(Arc::ptr_eq(&rig.entry(), &before), "the same entry");
        assert_eq!(rig.state(), Some("departing"));
        assert!(
            tokio::time::timeout(Duration::from_millis(200), rig.conns.recv())
                .await
                .is_err(),
            "no second connection"
        );
        // …and so is a finished one with no row: a readable ended lane.
        rig.finished("the grace to end").await;
        let again = rig
            .lanes
            .open(MACHINE, CRAZE, GHOST)
            .await
            .expect("a finished lane with no row re-opens readable");
        assert_eq!(again["session"]["id"], GHOST);
        assert_eq!(rig.state(), Some("finished"));
    }

    /// **W1 cell 11 — `open` on a finished entry whose row resolves opens a
    /// fresh lane and replaces it — only when that open COMMITS** (item 9):
    /// a failing open (the source no longer lists the session, though the
    /// machine layer does) leaves the finished entry readable; a good one
    /// replaces it with a live lane.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn open_on_a_finished_entry_replaces_it_only_when_the_open_commits() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        rig.finished("the grace to end").await;
        let finished = rig.entry();

        // The machine layer lists the row; the source's roster does not.
        rig.hub
            .roster("sub-1", "0a1b2c3d4e5f", json!([]), json!([GHOST]))
            .await;
        wait_for("the source to let the row go", || {
            rig.source.listed(GHOST).is_none().then_some(())
        })
        .await;
        rig.listed(true);
        let failed = rig
            .lanes
            .open(MACHINE, CRAZE, GHOST)
            .await
            .expect_err("the source lists no such session");
        assert_eq!(failed.code(), "unknown_session", "{}", failed.message());
        assert!(Arc::ptr_eq(&rig.entry(), &finished), "kept");
        assert_eq!(rig.messages()["finished"], true, "and readable");

        // Listed again: the fresh open commits, and replaces it.
        rig.hub
            .roster(
                "sub-1",
                "0a1b2c3d4e5f",
                json!([shed_craze::testing::roster_row(
                    GHOST,
                    GSID,
                    "/w",
                    json!({"title": "w"})
                )]),
                json!([]),
            )
            .await;
        wait_for("the source to list the row", || {
            rig.source.listed(GHOST).map(|_| ())
        })
        .await;
        rig.lanes
            .open(MACHINE, CRAZE, GHOST)
            .await
            .expect("a fresh lane");
        assert!(!Arc::ptr_eq(&rig.entry(), &finished), "replaced");
        assert_eq!(rig.state(), Some("live"));
        assert_eq!(rig.messages()["finished"], false);
        let _fresh = rig.conns.recv().await.expect("the fresh lane dialled");
    }

    /// **W1 cell 14, the `open` half — an equal stamp never adopts an older
    /// generation's lane** (item 12): the host was registered again (the
    /// source's generation moved) and no removal reached this live entry —
    /// `open` evicts it and opens a fresh lane through the current source,
    /// filed under the new generation.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn open_never_adopts_an_older_generations_lane() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        let old = rig.entry();
        rig.lanes
            .open(MACHINE, CRAZE, GHOST)
            .await
            .expect("an idempotent re-open");
        assert!(Arc::ptr_eq(&rig.entry(), &old), "same generation: adopted");
        rig.machines.craze_gen.store(2, SeqCst);
        rig.lanes
            .open(MACHINE, CRAZE, GHOST)
            .await
            .expect("a fresh lane");
        let fresh = rig.entry();
        assert!(
            !Arc::ptr_eq(&fresh, &old),
            "another generation: not adopted"
        );
        assert_eq!(fresh.craze_gen, Some(2));
        assert_eq!(rig.state(), Some("live"));
    }

    /// **W1 cell 12 — `lane.settings` on a finished entry answers from the
    /// view** (item 7): the session's last-stated settings, with no adapter
    /// (a retired lane has none).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn lane_settings_on_a_finished_entry_answer_from_the_view() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        let stated = wait_for("the seed's settings", || {
            let v = rig.messages();
            (!v["settings"].is_null()).then_some(v["settings"].clone())
        })
        .await;
        assert_eq!(stated["model"], "grok", "{stated}");
        rig.host_removed(1);
        assert!(
            lock(&rig.entry().core.client).is_none(),
            "no adapter to ask"
        );
        let answered = rig
            .lanes
            .settings(MACHINE, CRAZE, GHOST)
            .await
            .expect("answered from the view");
        assert_eq!(answered["settings"], stated);
    }

    /// **W1 cell 15 — a verb after the final `Down` is refused `lane_ended`
    /// at once** (item 8): the session closed, and a send that follows is
    /// refused by THIS layer — not handed to the adapter to wait out its 10 s
    /// reconnect wait and fail `unavailable` — with nothing sent. The race
    /// itself, both admission orders forced on one final `Down`, is
    /// `a_craze_verb_racing_the_final_down_both_orders` (and its opencode
    /// twin).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_verb_after_the_final_down_is_refused_lane_ended_at_once() {
        let mut rig = CrazeRig::start().await;
        let mut conn = rig.open_seeded().await;
        conn.reset("s-1", "session_closed").await;
        rig.finished("the session's close").await;
        let started = std::time::Instant::now();
        let refused = rig
            .lanes
            .send(MACHINE, CRAZE, GHOST, "too late", SendMode::Queue)
            .await
            .expect_err("refused");
        assert_eq!(refused.code(), "lane_ended");
        assert!(started.elapsed() < Duration::from_secs(1), "at once");
        for verb in [
            rig.lanes.cancel(MACHINE, CRAZE, GHOST).await,
            rig.lanes.stop(MACHINE, CRAZE, GHOST).await,
            rig.lanes.set_title(MACHINE, CRAZE, GHOST, "renamed").await,
            rig.lanes
                .answer(MACHINE, CRAZE, GHOST, "perm-1", LaneAnswer::Reject)
                .await,
            rig.lanes
                .set(
                    MACHINE,
                    CRAZE,
                    GHOST,
                    LaneSettingChange::Mode { id: "agent".into() },
                )
                .await,
        ] {
            assert_eq!(verb.expect_err("refused").code(), "lane_ended");
        }
        const VERBS: [&str; 6] = [
            "session.prompt",
            "session.cancel",
            "session.stop",
            "session.setTitle",
            "session.set",
            "asks.answer",
        ];
        while let Some(req) = conn.recv_within(Duration::from_millis(100)).await {
            let method = req["method"].as_str().unwrap_or_default();
            assert!(
                !VERBS.contains(&method),
                "a verb went out after the end: {req}"
            );
        }
    }

    /// **W1 cell 16 — a Dormant departure expires `hub_gone`** (PC14g): the
    /// hub went, not just the row, and the banner says so.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_expired_dormant_departure_ends_hub_gone() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::HubGone);
        let view = rig.finished("the grace to end").await;
        assert_eq!(view["stale"], DOWN_HUB_GONE);
        assert_eq!(rig.events.downs(), [DOWN_HUB_GONE]);
    }

    /// **W1 cell 9 — the shutdown during a grace JOINS the grace task, and
    /// nothing is emitted after `closing`** (item 11): the lanes phase counts
    /// the departure's grace (and joins it within its bound) and the finished
    /// lane beside it, and no frame — not the grace's expiry — follows.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_shutdown_joins_a_running_grace_and_emits_nothing_after() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        let emitted = rig.events.total();
        let (tx, rx) = tokio::sync::oneshot::channel();
        let lanes = Arc::clone(&rig.lanes);
        std::thread::spawn(move || {
            let _ = tx.send(lanes.shutdown());
        });
        let closed = rx.await.expect("the lanes thread");
        assert_eq!(
            (
                closed.lanes,
                closed.finished,
                closed.graces,
                closed.graces_unjoined
            ),
            (1, 0, 1, 0),
            "{closed:?}"
        );
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(rig.events.total(), emitted, "nothing after closing");
        assert!(rig.events.downs().is_empty());
    }

    /// The lanes phase counts a FINISHED lane too (item 11): it is still
    /// registered, readable, until closed.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_shutdown_counts_a_finished_lane() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.host_removed(1);
        let (tx, rx) = tokio::sync::oneshot::channel();
        let lanes = Arc::clone(&rig.lanes);
        std::thread::spawn(move || {
            let _ = tx.send(lanes.shutdown());
        });
        let closed = rx.await.expect("the lanes thread");
        assert_eq!((closed.lanes, closed.finished), (1, 1), "{closed:?}");
    }

    /// `close` removes a DEPARTING lane outright and cancels its grace: the
    /// lane is gone (no transcript, `no_lane` once its row is gone too) and
    /// the grace never emits (item 10).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn close_removes_a_departing_lane_and_cancels_its_grace() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        rig.listed(false);
        let grace = lock(&rig.entry().grace)
            .as_ref()
            .map(tokio::task::JoinHandle::abort_handle)
            .expect("a grace task");
        rig.lanes.close(MACHINE, CRAZE, GHOST);
        assert!(!rig.lanes.is_open(MACHINE, CRAZE, GHOST));
        wait_for("the grace task to end", || {
            grace.is_finished().then_some(())
        })
        .await;
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert!(rig.events.downs().is_empty(), "no Down for a closed lane");
        assert_eq!(
            rig.lanes
                .messages(MACHINE, CRAZE, GHOST)
                .expect_err("gone")
                .code(),
            "no_lane"
        );
    }

    // ---- the C1 review's cells: a natural end's transport, the shutdown's
    // ---- fence, and the verb race with both admission orders forced

    /// **A natural final `Down` releases the transport** (plan 032 PC4; C1
    /// review): the session's own `reset{session_closed}` — no grace run
    /// out, no host removed — finishes the lane, and the pump lets the
    /// adapter go on its way out, so the finished entry kept for its
    /// transcript holds no craze dial. Live, and inside a departure too:
    /// there the grace task leaves on `finished` without retiring anything,
    /// and the pump's release is the only one.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_natural_final_down_releases_the_craze_lanes_client() {
        for departing in [false, true] {
            let mut rig = CrazeRig::start().await;
            let mut conn = rig.open_seeded().await;
            if departing {
                rig.depart(Departure::Removed);
            }
            conn.reset("s-1", "session_closed").await;
            let view = rig.finished("the session's close").await;
            assert_eq!(view["stale"], "session_closed", "departing={departing}");
            let entry = rig.entry();
            wait_for("the pump to release the adapter", || {
                lock(&entry.core.client).is_none().then_some(())
            })
            .await;
            assert!(rig.lanes.is_open(MACHINE, CRAZE, GHOST), "kept, readable");
            assert_eq!(rig.messages()["finished"], true);
            assert_eq!(rig.events.downs(), ["session_closed"]);
        }
    }

    /// **An opencode lane's natural end releases its client AND its tunnel
    /// share** (plan 032 PC4; C1 review): a reseed answered 404 is
    /// `Down{unknown_session}`, final — the lane finishes, the pump lets the
    /// adapter and its share of the tunnel go, and with its last user gone
    /// the tunnel's child is reaped, off the async runtime. The finished
    /// entry stays readable.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_opencode_lanes_natural_end_releases_its_client_and_tunnel() {
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
        assert_eq!(forward_users(&lanes), Some(1));

        // The session is gone: the reconnect's seed answers 404.
        fake.fail_get("/session/ses_a", 404);
        fake.close_streams();
        wait_for("the lane to finish", || {
            (lanes.lane_state(MACHINE, OC, "ses_a") == Some("finished")).then_some(())
        })
        .await;
        let entry = lanes.entry(&key(MACHINE, OC, "ses_a")).expect("kept");
        wait_for("the pump to release the transport", || {
            (lock(&entry.core.client).is_none() && lock(&entry.core.forward).is_none())
                .then_some(())
        })
        .await;
        assert!(
            forward_users(&lanes).is_none(),
            "the tunnel's last share went with the lane"
        );
        wait_for("the tunnel's child to be reaped", || {
            (log.dropped.load(SeqCst) == 1).then_some(())
        })
        .await;
        assert_eq!(log.dropped_on_runtime.load(SeqCst), 0);
        assert_eq!(events.downs(), [DOWN_UNKNOWN_SESSION]);
        let view = lanes
            .messages(MACHINE, OC, "ses_a")
            .expect("a finished lane stays readable");
        assert_eq!(view["finished"], true);
        assert_eq!(view["stale"], DOWN_UNKNOWN_SESSION);
    }

    /// **The shutdown's start fences EVERY lane before its flag, so nothing is
    /// emitted after it** (plan 032 §3.1.1 items 4 and 11; C1 review), the
    /// window forced with two lanes. One lane's `state` is HELD — what a
    /// frame its pump has in flight holds (here a plain thread's, so no
    /// runtime worker is parked) — so the shutdown's start blocks on it, and
    /// the other lane's grace runs out while it waits. Whichever lane the
    /// fence reaches first, that expiry lands before `begin_closing` returns
    /// or never, and from its return on no frame of either lane is emitted.
    /// (Flag first, the walk let a lane it had not reached yet emit after the
    /// flag — here, the expiry's `Down`.)
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_shutdowns_start_fences_every_lane_before_its_flag() {
        let fake = one_session("ses_a").await;
        let mut rig = CrazeRig::start_with(Some(&fake), shed_craze::LaneTimings::default()).await;
        let _conn = rig.open_seeded().await;
        rig.lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the opencode lane opens");
        wait_for("the opencode lane to seed", || {
            (generation(&rig.lanes, "ses_a") >= 1).then_some(())
        })
        .await;

        // The opencode lane's `state`, held as a frame in flight holds it.
        let held = rig
            .lanes
            .entry(&key(MACHINE, OC, "ses_a"))
            .expect("the opencode lane");
        let (holding, release) = {
            let (took, holding) = std::sync::mpsc::channel();
            let (release, released) = std::sync::mpsc::channel::<()>();
            let core = Arc::clone(&held.core);
            std::thread::spawn(move || {
                let _frame = lock(&core.state);
                let _ = took.send(());
                let _ = released.recv();
            });
            (holding, release)
        };
        holding.recv().expect("the state held");
        // The craze lane departs: its grace runs out inside the window.
        rig.depart(Departure::Removed);
        let (tx, rx) = tokio::sync::oneshot::channel();
        {
            let lanes = Arc::clone(&rig.lanes);
            let events = Arc::clone(&rig.events);
            std::thread::spawn(move || {
                lanes.begin_closing();
                // Read the moment it returns: nothing may follow this.
                let _ = tx.send(events.total());
            });
        }
        tokio::time::sleep(TEST_GRACE + Duration::from_millis(200)).await;
        release.send(()).expect("the held state");
        let at_close = rx.await.expect("the closing thread");
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(
            rig.events.total(),
            at_close,
            "a frame landed after the shutdown began"
        );
        assert!(
            matches!(rig.state(), Some("departing" | "finished")),
            "{:?}",
            rig.state()
        );
        let refused = rig
            .lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect_err("closing");
        assert_eq!(refused.code(), shed_app::spawn_gate::SHUTTING_DOWN);

        // The opencode lane's next frame — a reconnect's — is not emitted
        // either, and the drain joins both lanes' tasks.
        fake.close_streams();
        let (tx, rx) = tokio::sync::oneshot::channel();
        let lanes = Arc::clone(&rig.lanes);
        std::thread::spawn(move || {
            let _ = tx.send(lanes.shutdown());
        });
        let closed = rx.await.expect("the lanes thread");
        assert_eq!(
            (closed.lanes, closed.pumps_unjoined, closed.graces_unjoined),
            (2, 0, 0),
            "{closed:?}"
        );
        assert_eq!(rig.events.total(), at_close, "nor during the drain");
    }

    /// Arm a [`Lanes::verb_gaps`] gap at `side`: answers its `(reached,
    /// proceed)` pair.
    fn verb_gap(
        lanes: &Lanes,
        side: Admission,
    ) -> (Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>) {
        let reached = Arc::new(tokio::sync::Notify::new());
        let proceed = Arc::new(tokio::sync::Notify::new());
        lock(&lanes.verb_gaps).push((side, (Arc::clone(&reached), Arc::clone(&proceed))));
        (reached, proceed)
    }

    /// Two sends on `session`'s lane parked around their admission (C1
    /// review): the first ADMITTED — its adapter clone in hand — the second
    /// past its registry read, NOT yet admitted. Answers both tasks and the
    /// gate that releases each, in that order.
    async fn sends_around_admission(
        lanes: &Arc<Lanes>,
        kind: &'static str,
        session: &'static str,
    ) -> (
        tokio::task::JoinHandle<Result<Value, LaneFailure>>,
        Arc<tokio::sync::Notify>,
        tokio::task::JoinHandle<Result<Value, LaneFailure>>,
        Arc<tokio::sync::Notify>,
    ) {
        let send = |text: &'static str| {
            let lanes = Arc::clone(lanes);
            tokio::spawn(async move {
                lanes
                    .send(MACHINE, kind, session, text, SendMode::Queue)
                    .await
            })
        };
        // In turn, so the first passes the `Before` side with no gap armed.
        let (at, admitted_go) = verb_gap(lanes, Admission::After);
        let admitted = send("admitted before the end");
        at.notified().await;
        let (at, late_go) = verb_gap(lanes, Admission::Before);
        let late = send("admitted after the end");
        at.notified().await;
        (admitted, admitted_go, late, late_go)
    }

    /// **A verb racing the final `Down`, both admission orders forced** (plan
    /// 032 §3.1.1 item 8; C1 review), on a craze lane. One send is ADMITTED —
    /// its adapter clone in hand — and parked; another has read the entry and
    /// is parked BEFORE its admission. The session's `reset{session_closed}`
    /// then finishes the lane, and the pump releases the entry's client. The
    /// admitted send runs its course on its own clone — the adapter, its
    /// connection gone with the session, answers it `unavailable`, not sent
    /// (the lane's connect wait shortened here) — and the other is refused
    /// `lane_ended` at once: not `no_lane`, though the entry's client is
    /// gone. Neither prompt reaches the hub.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_craze_verb_racing_the_final_down_both_orders() {
        let lane = shed_craze::LaneTimings {
            wait_connected: Duration::from_millis(300),
            ..shed_craze::LaneTimings::default()
        };
        let mut rig = CrazeRig::start_with(None, lane).await;
        let mut conn = rig.open_seeded().await;
        let (admitted, admitted_go, late, late_go) =
            sends_around_admission(&rig.lanes, CRAZE, GHOST).await;

        conn.reset("s-1", "session_closed").await;
        rig.finished("the session's close").await;
        let entry = rig.entry();
        wait_for("the pump to release the adapter", || {
            lock(&entry.core.client).is_none().then_some(())
        })
        .await;

        late_go.notify_one();
        let started = std::time::Instant::now();
        let refused = late
            .await
            .expect("the late send's task")
            .expect_err("admitted after the end");
        assert_eq!(refused.code(), "lane_ended", "{}", refused.message());
        assert!(started.elapsed() < Duration::from_millis(250), "at once");
        admitted_go.notify_one();
        let answered = admitted
            .await
            .expect("the admitted send's task")
            .expect_err("no connection to send it on");
        assert_eq!(
            answered.code(),
            "unavailable",
            "the ADAPTER's answer, on the verb's own clone: {}",
            answered.message()
        );
        assert!(
            answered.message().contains("was not sent"),
            "{}",
            answered.message()
        );
        while let Some(req) = conn.recv_within(Duration::from_millis(100)).await {
            assert_ne!(req["method"], "session.prompt", "a prompt went out: {req}");
        }
    }

    /// **The same race on an opencode lane** (C1 review: opencode's natural
    /// end refuses verbs `lane_ended` now too). The admitted send runs on its
    /// clone and SUCCEEDS — opencode's prompt is a plain POST that needs no
    /// subscription, so the agent takes it although the lane has finished and
    /// released its client — and the other is refused `lane_ended`, with
    /// nothing sent: exactly one prompt reaches the agent.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_opencode_verb_racing_the_final_down_both_orders() {
        let fake = one_session("ses_a").await;
        let (lanes, _log, _events) = lanes_for(&fake, &["ses_a"]);
        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        wait_for("the first generation to seed", || {
            (generation(&lanes, "ses_a") >= 1).then_some(())
        })
        .await;
        let (admitted, admitted_go, late, late_go) =
            sends_around_admission(&lanes, OC, "ses_a").await;

        // The session's natural end: the reconnect's seed answers 404.
        fake.fail_get("/session/ses_a", 404);
        fake.close_streams();
        wait_for("the lane to finish", || {
            (lanes.lane_state(MACHINE, OC, "ses_a") == Some("finished")).then_some(())
        })
        .await;
        let entry = lanes.entry(&key(MACHINE, OC, "ses_a")).expect("kept");
        wait_for("the pump to release the adapter", || {
            lock(&entry.core.client).is_none().then_some(())
        })
        .await;

        late_go.notify_one();
        let refused = late
            .await
            .expect("the late send's task")
            .expect_err("admitted after the end");
        assert_eq!(refused.code(), "lane_ended", "{}", refused.message());
        admitted_go.notify_one();
        admitted
            .await
            .expect("the admitted send's task")
            .expect("the admitted send runs on its own clone");
        let prompts: Vec<String> = fake
            .post_paths()
            .into_iter()
            .filter(|p| p.ends_with("/prompt_async"))
            .collect();
        assert_eq!(prompts.len(), 1, "only the admitted send: {prompts:?}");
        assert!(
            fake.post_body("/prompt_async")
                .is_some_and(|b| b.contains("admitted before the end")),
            "{:?}",
            fake.post_body("/prompt_async")
        );
    }

    /// A bare [`LaneCore`] whose frames go to a [`Recorder`], DEPARTING under
    /// `token` 7 for `cause`.
    fn departing_core(cause: Departure) -> (Arc<LaneCore>, EventSink, Arc<Recorder>) {
        let core = LaneCore::new(None, None);
        lock(&core.state).state = EntryState::Departing { token: 7, cause };
        let recorder = Arc::new(Recorder::default());
        let sink: EventSink = {
            let recorder = Arc::clone(&recorder);
            Arc::new(move |_: &str, _: &str, _: &str, event: &LaneEvent| recorder.record(event))
        };
        (core, sink, recorder)
    }

    fn core_key() -> Key {
        key(MACHINE, CRAZE, GHOST)
    }

    /// **W1 cell 7, forced — exactly one terminal `Down` whoever lands
    /// first** (item 4): the session's final `Down` then the grace's expiry,
    /// the expiry then the `Down`, the host's removal after either — and the
    /// two racing on threads, many times over. The check and the transition
    /// share one `state` lock, so the loser emits nothing.
    #[test]
    fn exactly_one_terminal_down_whoever_lands_first() {
        let k = core_key();
        let (core, sink, events) = departing_core(Departure::Removed);
        assert!(core.finish(&sink, &k, "session_closed"));
        assert!(!core.expire(&sink, &k, 7), "the expiry lost");
        assert!(
            !core.finish(&sink, &k, DOWN_HOST_REMOVED),
            "so did the removal"
        );
        assert_eq!(events.downs(), ["session_closed"]);

        let (core, sink, events) = departing_core(Departure::Removed);
        assert!(core.expire(&sink, &k, 7));
        assert!(!core.finish(&sink, &k, "session_closed"), "the Down lost");
        assert_eq!(events.downs(), [DOWN_SESSION_GONE]);

        // A stale token (a departure a return ended) expires nothing.
        let (core, sink, events) = departing_core(Departure::Removed);
        assert!(!core.expire(&sink, &k, 6));
        assert!(events.downs().is_empty());

        for _ in 0..200 {
            let (core, sink, events) = departing_core(Departure::HubGone);
            let barrier = Arc::new(std::sync::Barrier::new(2));
            let racer = {
                let (core, sink, barrier, k) = (
                    Arc::clone(&core),
                    Arc::clone(&sink),
                    Arc::clone(&barrier),
                    k.clone(),
                );
                std::thread::spawn(move || {
                    barrier.wait();
                    core.expire(&sink, &k, 7)
                })
            };
            barrier.wait();
            let finished = core.finish(&sink, &k, "session_closed");
            let expired = racer.join().expect("the racer");
            assert!(finished ^ expired, "exactly one won");
            assert_eq!(events.downs().len(), 1, "{:?}", events.downs());
        }
    }

    /// **W1 cell 8 — a late `Ready` after the lane finishes is not applied**
    /// (item 1, PC1): a whole reseed arriving after the end would clear the
    /// view's `ended` and `stale`; under the finished state none of it is
    /// applied or emitted, and the banner stays.
    #[test]
    fn a_late_ready_after_the_finish_is_not_applied() {
        let k = core_key();
        let (core, sink, events) = departing_core(Departure::Removed);
        assert!(core.apply(
            &sink,
            &k,
            &LaneEvent::Reset {
                reason: "connect".into(),
                generation: 1
            }
        ));
        assert!(core.apply(&sink, &k, &LaneEvent::Ready { generation: 1 }));
        assert!(core.expire(&sink, &k, 7));
        for late in [
            LaneEvent::Reset {
                reason: "connect".into(),
                generation: 2,
            },
            LaneEvent::Ready { generation: 2 },
            LaneEvent::Ready { generation: 1 },
        ] {
            assert!(!core.apply(&sink, &k, &late), "{late:?} applied");
        }
        let snap = lock(&core.view).snapshot(None);
        assert!(snap.ended, "the late Ready un-ended the lane");
        assert_eq!(snap.stale.as_deref(), Some(DOWN_SESSION_GONE));
        assert_eq!(events.count("ready"), 1, "only the live seed's Ready");
    }

    /// **The shutdown's mark stops everything** (item 4): an entry marked
    /// closing applies, emits, finishes and expires nothing.
    #[test]
    fn a_closing_entry_applies_and_expires_nothing() {
        let k = core_key();
        let (core, sink, events) = departing_core(Departure::Removed);
        lock(&core.state).closing = true;
        assert!(!core.expire(&sink, &k, 7));
        assert!(!core.finish(&sink, &k, "session_closed"));
        assert!(!core.apply(&sink, &k, &LaneEvent::Ready { generation: 1 }));
        assert_eq!(events.total(), 0);
    }

    /// The three reasons this layer ends a lane with are final: the pump
    /// never resubscribes after one (plan 032 §3.1.1 item 12).
    #[test]
    fn this_layers_own_reasons_are_final() {
        for reason in [DOWN_SESSION_GONE, DOWN_HUB_GONE, DOWN_HOST_REMOVED] {
            assert!(down_is_final(reason), "{reason}");
        }
        assert_eq!(Departure::Removed.reason(), DOWN_SESSION_GONE);
        assert_eq!(Departure::HubGone.reason(), DOWN_HUB_GONE);
    }

    // -----------------------------------------------------------------------
    // the orderly shutdown's lanes phase (plan 026 §3.3, shed#389)
    // -----------------------------------------------------------------------

    /// An empty roost-host layer and the app's gate — what
    /// `shutdown_children` needs beside the lanes under test.
    fn empty_hosts() -> (Arc<RoostHosts>, shed_app::spawn_gate::SpawnGate) {
        let gate = shed_app::spawn_gate::SpawnGate::new();
        let hosts = Arc::new(RoostHosts::start(
            &tokio::runtime::Handle::current(),
            &shed_core::config::ShedConfig::default(),
            crate::machines::ReachOptions {
                test_mode: true,
                spawn_gate: Some(gate.clone()),
                ..crate::machines::ReachOptions::default()
            },
            false,
            Arc::new(|| {}),
        ));
        (hosts, gate)
    }

    /// [`Lanes::shutdown`] on a plain OS thread of its own — the way the
    /// coordinator runs it — joined by the caller.
    async fn shutdown_on_a_thread(lanes: &Arc<Lanes>) -> (usize, usize) {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let lanes = Arc::clone(lanes);
        std::thread::spawn(move || {
            let _ = tx.send(lanes.shutdown());
        });
        let closed = rx.await.expect("the lanes thread");
        assert_eq!(
            (closed.forwards_unsettled, closed.pumps_unjoined),
            (0, 0),
            "nothing abandoned: {closed:?}"
        );
        (closed.lanes, closed.forwards)
    }

    fn pid_alive(pid: i32) -> bool {
        // SAFETY: signal 0 performs only an existence/permission check.
        unsafe { libc::kill(pid, 0) == 0 }
    }

    /// **A fake opencode forward is closed when the shutdown returns** (plan
    /// 026 §3.3): `shutdown_children` over the real registries closes it
    /// through the trait — `close_now`, whatever its reference count — the
    /// lane is gone, and no lane opens again.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_fake_opencode_forward_is_closed_when_the_shutdown_returns() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);
        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        assert!(log.alive.load(SeqCst));
        let (hosts, gate) = empty_hosts();

        let report = crate::shutdown::shutdown_children(&gate, &hosts, &lanes, None).await;
        assert_eq!(
            log.closed.load(SeqCst),
            1,
            "the forward was not closed through the trait"
        );
        assert!(
            !log.alive.load(SeqCst),
            "the forward's child is alive on return"
        );
        assert_eq!(
            (report.lanes.lanes, report.lanes.forwards),
            (1, 1),
            "{report:?}"
        );
        assert!(!lanes.is_open(MACHINE, OC, "ses_a"));
        let refused = lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect_err("no lane opens once the shutdown has begun");
        assert_eq!(refused.code(), shed_app::spawn_gate::SHUTTING_DOWN);
        assert_eq!(
            log.built.load(SeqCst),
            1,
            "a tunnel was built after the shutdown"
        );
    }

    /// **The lanes phase awaits every pump** (plan 026 §3.3): a pump parked in
    /// its tunnel's re-`ensure` is aborted AND joined — its future has been
    /// dropped by the time `shutdown` returns, not merely scheduled to be.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_lanes_phase_awaits_every_pump_before_it_returns() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);
        // The open's own ensure is the first; the pump's first is the second.
        log.hang_on.store(2, SeqCst);
        lanes
            .open(MACHINE, OC, "ses_a")
            .await
            .expect("the lane opens");
        wait_for("the pump to park in its ensure", || {
            (log.ensures.load(SeqCst) >= 2).then_some(())
        })
        .await;

        let (closed, forwards) = shutdown_on_a_thread(&lanes).await;
        assert_eq!((closed, forwards), (1, 1));
        assert!(
            log.hung_dropped.load(SeqCst),
            "shutdown returned while its aborted pump was still running"
        );
    }

    /// **A forward whose `ensure` is mid-spawn when the shutdown starts: its
    /// child is dead on return** (plan 026 §3.3). An open is in flight, its
    /// tunnel's child spawned and its readiness still pending; the shutdown
    /// closes that forward through the trait, the child dies, and the open
    /// commits nothing.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_forward_mid_spawn_when_shutdown_starts_is_dead_on_return() {
        let dir = tempfile::tempdir().expect("tempdir");
        let pids = dir.path().join("pids");
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) =
            lanes_spawning(fake.addr().port(), lane_rows(&["ses_a"]), &pids);
        let opening = tokio::spawn({
            let lanes = Arc::clone(&lanes);
            async move { lanes.open(MACHINE, OC, "ses_a").await }
        });
        let pid: i32 = wait_for("the forward's child to spawn", || {
            std::fs::read_to_string(&pids).ok()?.trim().parse().ok()
        })
        .await;
        assert!(pid_alive(pid));
        let (hosts, gate) = empty_hosts();

        crate::shutdown::shutdown_children(&gate, &hosts, &lanes, None).await;
        assert!(
            !pid_alive(pid),
            "the mid-spawn forward's child {pid} outlived the shutdown"
        );
        let opened = tokio::time::timeout(Duration::from_secs(5), opening)
            .await
            .expect("the open in flight returned")
            .expect("the open task");
        assert!(opened.is_err(), "an open in flight at shutdown committed");
        assert!(!lanes.is_open(MACHINE, OC, "ses_a"));
        assert_eq!(log.built.load(SeqCst), 1);
    }

    /// **An open waiting on its gate when the shutdown begins is refused, and
    /// builds no tunnel** (plan 026 §3.3): it re-checks the closing flag
    /// before declaring itself, so it answers `shutting_down` without
    /// reserving a forward.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_open_waiting_on_its_gate_is_refused_and_builds_no_tunnel() {
        let fake = one_session("ses_a").await;
        let (lanes, log, _events) = lanes_for(&fake, &["ses_a"]);
        // The first open's ensure hangs, holding the key's gate.
        log.hang_on.store(1, SeqCst);
        let first = tokio::spawn({
            let lanes = Arc::clone(&lanes);
            async move { lanes.open(MACHINE, OC, "ses_a").await }
        });
        wait_for("the first open to hold the gate", || {
            (log.ensures.load(SeqCst) == 1).then_some(())
        })
        .await;
        let second = tokio::spawn({
            let lanes = Arc::clone(&lanes);
            async move { lanes.open(MACHINE, OC, "ses_a").await }
        });
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(!second.is_finished(), "the second open waits on the gate");

        shutdown_on_a_thread(&lanes).await;
        let first = tokio::time::timeout(Duration::from_secs(5), first)
            .await
            .expect("the first open returned once its forward closed")
            .expect("the first open task");
        assert!(first.is_err());
        let second = tokio::time::timeout(Duration::from_secs(5), second)
            .await
            .expect("the waiting open returned")
            .expect("the second open task");
        let refused = second.expect_err("an open that waited across the shutdown is refused");
        assert_eq!(
            refused.code(),
            shed_app::spawn_gate::SHUTTING_DOWN,
            "{}",
            refused.message()
        );
        assert_eq!(log.built.load(SeqCst), 1, "the waiting open built a tunnel");
        assert_eq!(
            log.ensures.load(SeqCst),
            1,
            "the waiting open started a tunnel"
        );
    }

    /// [`lanes_on`], every forward a [`SpawningForward`] writing to `pids`.
    fn lanes_spawning(
        port: u16,
        rows: BTreeMap<String, AgentLaneStamp>,
        pids: &std::path::Path,
    ) -> (Arc<Lanes>, Arc<ForwardLog>, Arc<Recorder>) {
        let (lanes, log, recorder, _machines) = lanes_with_machines(port, rows, None, Some(pids));
        (lanes, log, recorder)
    }
}
