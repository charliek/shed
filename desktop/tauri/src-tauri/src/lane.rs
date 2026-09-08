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

use std::collections::{BTreeMap, HashMap, HashSet, VecDeque};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use serde_json::{json, Value};
use tauri::{AppHandle, Emitter};

use shed_app::machine::{MachineForward, SshForward};
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
                self.target()
                    .approvals
                    .insert(approval.id.clone(), approval.clone());
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
// one open lane
// ---------------------------------------------------------------------------

struct LaneEntry {
    /// The `server_url` this entry was opened against. The eviction key for a
    /// restarted tab: a new port means a different server, not a reconnect.
    server_url: String,
    /// The forward this entry rides, if any. Its refcount is "how many entries
    /// still need this tunnel".
    forward: Option<ForwardKey>,
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
}

impl Drop for LaneEntry {
    /// Ending the pump drops the [`shed_core::lane::LaneStop`] it holds, which
    /// aborts the adapter's watcher, which closes the `/event` socket.
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
    forwards: HashMap<ForwardKey, Arc<SshForward>>,
}

/// Every open lane in this app, and the tunnels under them.
pub struct Lanes {
    handle: tokio::runtime::Handle,
    app: AppHandle,
    machines: Arc<Machines>,
    inner: Mutex<Inner>,
    /// One async gate per key, so two concurrent `lane.open`s on the same
    /// session build ONE entry and ONE subscription.
    ///
    /// Per key rather than one global gate: an `open` may block for as long as
    /// `SshForward::ensure`'s readiness deadline, and a slow machine must not
    /// hold up a panel on a different one.
    gates: Mutex<HashMap<Key, Arc<tokio::sync::Mutex<()>>>>,
}

impl Lanes {
    pub fn new(handle: tokio::runtime::Handle, app: AppHandle, machines: Arc<Machines>) -> Lanes {
        Lanes {
            handle,
            app,
            machines,
            inner: Mutex::new(Inner::default()),
            gates: Mutex::new(HashMap::new()),
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
    pub async fn open(&self, machine: &str, session_id: &str) -> Result<Value, LaneFailure> {
        let key = (machine.to_string(), session_id.to_string());
        let gate = self.gate(&key);
        let _serialized = gate.lock().await;

        let server_url = self.lane_url(machine, session_id)?;
        if let Some(entry) = self.entry(&key) {
            if entry.server_url == server_url {
                return Ok(entry.opened());
            }
            self.evict(&key);
        }

        let kind = self
            .machines
            .reach_kind(machine)
            .map_err(|e| LaneFailure::Lane(LaneError::Unavailable(e)))?;
        let (base_url, forward_key, forward) = self.transport(machine, &kind, &server_url).await?;

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
        let session = client.session(session_id).await?;
        let capabilities = client.capabilities();

        let view = Arc::new(Mutex::new(LaneView::default()));
        let pump = self.spawn_pump(
            machine.to_string(),
            session_id.to_string(),
            Arc::clone(&client),
            Arc::clone(&view),
            forward,
        );
        let entry = Arc::new(LaneEntry {
            server_url,
            forward: forward_key,
            session,
            capabilities,
            client,
            view,
            pump,
        });
        let opened = entry.opened();
        lock(&self.inner).entries.insert(key, entry);
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
        let mut gone: Vec<Arc<LaneEntry>> = Vec::new();
        let dropped = {
            let mut inner = lock(&self.inner);
            inner.entries.retain(|(m, session_id), entry| {
                if m != machine {
                    return true;
                }
                let keep = lanes
                    .get(session_id)
                    .is_some_and(|url| url == &entry.server_url);
                if !keep {
                    // Abort NOW rather than relying on the `Drop`: another op
                    // may be holding a clone of this Arc across an await, and
                    // the subscription must end when the tab does.
                    entry.pump.abort();
                    gone.push(Arc::clone(entry));
                }
                keep
            });
            prune_forwards(&mut inner)
        };
        drop(gone);
        self.reap(dropped);
    }

    // ---- internals ----

    /// The per-key open gate, created on first use.
    fn gate(&self, key: &Key) -> Arc<tokio::sync::Mutex<()>> {
        let mut gates = lock(&self.gates);
        // The map is bounded by the number of (machine, session) pairs this app
        // has ever opened a panel on, which is the same order as the row set.
        Arc::clone(
            gates
                .entry(key.clone())
                .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(()))),
        )
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

    /// Resolve the base URL to build a client on, ensuring a tunnel first when
    /// the machine is remote.
    #[allow(clippy::type_complexity)]
    async fn transport(
        &self,
        machine: &str,
        kind: &ReachKind,
        server_url: &str,
    ) -> Result<(String, Option<ForwardKey>, Option<Arc<SshForward>>), LaneFailure> {
        let entry = match kind {
            // Its loopback is ours.
            ReachKind::Local => return Ok((server_url.to_string(), None, None)),
            ReachKind::Ssh(entry) => entry,
        };
        let remote_port = remote_port(server_url)?;
        let key: ForwardKey = (machine.to_string(), remote_port);
        // Reserve OUTSIDE the lock only when there is nothing to reuse: the
        // reservation binds a socket, and a second reserve for a key another
        // task just inserted would leak a port. Insert-if-absent under one
        // acquisition settles the race in favour of whoever got there first.
        let existing = lock(&self.inner).forwards.get(&key).map(Arc::clone);
        let forward = match existing {
            Some(forward) => forward,
            None => {
                let fresh = Arc::new(
                    SshForward::reserve_for(entry.clone(), remote_port)
                        .map_err(|e| LaneError::Unavailable(e.to_string()))?,
                );
                let mut inner = lock(&self.inner);
                Arc::clone(inner.forwards.entry(key.clone()).or_insert(fresh))
            }
        };
        forward
            .ensure()
            .await
            .map_err(|e| LaneError::Unavailable(e.to_string()))?;
        let base = format!("http://127.0.0.1:{}/", forward.port());
        Ok((base, Some(key), Some(forward)))
    }

    /// Remove one entry and any tunnel it was the last user of.
    fn evict(&self, key: &Key) {
        let (entry, dropped) = {
            let mut inner = lock(&self.inner);
            let entry = inner.entries.remove(key);
            if let Some(entry) = &entry {
                entry.pump.abort();
            }
            let dropped = prune_forwards(&mut inner);
            (entry, dropped)
        };
        drop(entry);
        self.reap(dropped);
    }

    /// Let dropped forwards die on a blocking thread.
    ///
    /// `SshForward`'s `Drop` kills AND waits its `ssh` child, and this runs from
    /// an async context — a roost snapshot's consumer, or an IPC op. Handing the
    /// waitpid to `spawn_blocking` keeps a reaping tunnel off the runtime's
    /// worker.
    fn reap(&self, dropped: Vec<Arc<SshForward>>) {
        if dropped.is_empty() {
            return;
        }
        self.handle.spawn_blocking(move || drop(dropped));
    }

    /// The supervision loop for one lane: ensure the transport, subscribe, pump
    /// frames into the view and out to the UI, and start over on a backoff when
    /// the subscription ends.
    ///
    /// The adapter reconnects on its own inside one subscription (that is the
    /// `Reset` … `Ready` bracket); this loop is the layer ABOVE it, and it
    /// exists for the failure the adapter cannot fix — a transport that has gone
    /// away. On a remote machine, re-`ensure`ing the forward inside the backoff
    /// is what respawns a dead `ssh -N` child before redialing.
    fn spawn_pump(
        &self,
        machine: String,
        session_id: String,
        client: Arc<OpencodeClient>,
        view: Arc<Mutex<LaneView>>,
        forward: Option<Arc<SshForward>>,
    ) -> tokio::task::JoinHandle<()> {
        let app = self.app.clone();
        self.handle.spawn(async move {
            let mut backoff = RESUBSCRIBE_BASE;
            loop {
                if let Some(forward) = &forward {
                    if let Err(e) = forward.ensure().await {
                        let reason = format!("forward: {e}");
                        note_down(&app, &view, &machine, &session_id, reason);
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
                            &app,
                            &view,
                            &machine,
                            &session_id,
                            DOWN_UNKNOWN_SESSION.to_string(),
                        );
                        return;
                    }
                    Err(e) => {
                        note_down(&app, &view, &machine, &session_id, e.to_string());
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                };
                // BOTH halves, for the whole read: taking `.rx` alone drops the
                // stop handle, which aborts the pump it is reading from.
                let (mut rx, stop) = subscription.into_parts();
                let mut down: Option<String> = None;
                while let Some(event) = rx.recv().await {
                    match &event {
                        // A generation that reached steady state is the signal
                        // the transport is healthy again.
                        LaneEvent::Ready { .. } => backoff = RESUBSCRIBE_BASE,
                        LaneEvent::Down { reason } => down = Some(reason.clone()),
                        _ => {}
                    }
                    lock(&view).apply(&event);
                    emit(&app, &machine, &session_id, &event);
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

/// Every tunnel no remaining entry rides. Called under the `inner` lock.
fn prune_forwards(inner: &mut Inner) -> Vec<Arc<SshForward>> {
    let live: HashSet<ForwardKey> = inner
        .entries
        .values()
        .filter_map(|e| e.forward.clone())
        .collect();
    let mut dropped = Vec::new();
    inner.forwards.retain(|key, forward| {
        if live.contains(key) {
            return true;
        }
        dropped.push(Arc::clone(forward));
        false
    });
    dropped
}

/// Record a transport-level failure as the same stale-with-a-reason state a
/// [`LaneEvent::Down`] produces, and tell the UI about it on the same event.
///
/// The panel must not care whether the thing that went away was the agent or the
/// tunnel to it: both mean "this transcript is not live", and both are recovered
/// by the same retry.
fn note_down(
    app: &AppHandle,
    view: &Arc<Mutex<LaneView>>,
    machine: &str,
    session_id: &str,
    reason: String,
) {
    let event = LaneEvent::Down { reason };
    lock(view).apply(&event);
    emit(app, machine, session_id, &event);
}

fn emit(app: &AppHandle, machine: &str, session_id: &str, event: &LaneEvent) {
    let _ = app.emit(
        LANE_EVENT,
        json!({ "machine": machine, "session_id": session_id, "event": event }),
    );
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
pub fn parse_answer(value: &Value) -> Result<LaneAnswer, String> {
    if let Some(decision) = value.get("permission") {
        let decision = decision
            .as_str()
            .ok_or("`permission` must be a string")?
            .trim();
        let decision = match decision {
            "allow-once" => LaneDecision::AllowOnce,
            "allow-always" => LaneDecision::AllowAlways,
            "reject" => LaneDecision::Reject,
            other => {
                return Err(format!(
                    "unknown permission decision {other:?} \
                     (want allow-once, allow-always or reject)"
                ))
            }
        };
        return Ok(LaneAnswer::Permission { decision });
    }
    if let Some(answers) = value.get("question") {
        let answers: Vec<Vec<String>> = serde_json::from_value(answers.clone())
            .map_err(|e| format!("`question` must be a list of lists of option ids: {e}"))?;
        return Ok(LaneAnswer::Question { answers });
    }
    if value.get("reject").and_then(Value::as_bool) == Some(true) {
        return Ok(LaneAnswer::Reject);
    }
    Err("answer must be {permission: …}, {question: [[…]]} or {reject: true}".to_string())
}

/// Decode the optional `mode` of `lane.send`.
pub fn parse_mode(value: Option<&str>) -> Result<SendMode, String> {
    match value.map(str::trim).unwrap_or("queue") {
        "queue" | "" => Ok(SendMode::Queue),
        "interject" => Ok(SendMode::Interject),
        other => Err(format!(
            "unknown send mode {other:?} (want queue or interject)"
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
}
