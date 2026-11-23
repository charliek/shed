//! **Agent lanes in the shed-only desktop runtime** — craze-only client half
//! of [`shed_lane::lane`] (plan 025, plan 032, plan 038 D2/D6).
//!
//! Every lane is a craze session on a running shed (`ShedId`,
//! `roost:<server>/<shed>`), opened through that shed's [`CrazeSource`].
//!
//! ## Lock Ordering
//!
//! * Inside [`ShedLanes`]: `inner` (the registry) → an entry's `core.state` →
//!   `core.view`. `core.client` is a leaf lock; `gates` is independent.
//! * Across [`crate::ShedHosts`] and [`ShedLanes`]: **never nested in either
//!   direction**.
//!   * `ShedHosts` always releases every mutex before publishing [`CrazeNews`]
//!     into [`ShedLanes::craze_news`].
//!   * `ShedLanes` always calls [`LaneHosts::craze_lanes`] and
//!     [`LaneHosts::craze_source`] OUTSIDE `inner` and `core.state`.
//!   * [`LaneEvents::on_lane_event`] is invoked while holding `core.state`
//!     (and NOT `inner`), so frame ordering and terminal-`Down` exclusivity are
//!     atomic per lane; embedder `LaneEvents` implementations must be non-blocking
//!     and must never re-enter `ShedLanes` or `ShedHosts`.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::Duration;

use serde::Serialize;
use serde_json::{json, Value};

use shed_craze::CrazeSource;
use shed_lane::lane::{
    AgentLane, AgentSource as _, LaneAnswer, LaneDecision, LaneError, LaneEvent, LaneSession,
    LaneSettingChange, SendMode,
};
use shed_lane::lane_view::LaneView;
use shed_roost::{AgentLaneStamp, ShuttingDown, SHUTTING_DOWN};

use crate::id::ShedId;
use crate::shed_hosts::ShedHosts;

/// The UI event payload emitted for every lane frame (plan 038 D2):
/// `{ "target": "roost:<server>/<shed>", "session_id": "...", "event": ... }`.
#[derive(Debug, Clone, Serialize)]
pub struct LaneUiEvent {
    pub target: ShedId,
    pub session_id: String,
    pub event: LaneEvent,
}

/// Sink for lane frames produced by [`ShedLanes`].
///
/// Implementations must be cheap and non-blocking and must not re-enter
/// [`ShedLanes`] or [`ShedHosts`].
pub trait LaneEvents: Send + Sync + 'static {
    fn on_lane_event(&self, event: LaneUiEvent);
}

/// No-op [`LaneEvents`] implementation (for tests or headless drivers).
#[derive(Debug, Default, Clone, Copy)]
pub struct NoopLaneEvents;

impl LaneEvents for NoopLaneEvents {
    fn on_lane_event(&self, _event: LaneUiEvent) {}
}

/// The craze adapter's kind token (`shed_craze::KIND`).
pub const CRAZE_KIND: &str = shed_craze::KIND;

const RESUBSCRIBE_BASE: Duration = Duration::from_millis(200);
const RESUBSCRIBE_MAX: Duration = Duration::from_secs(5);
const PUMP_JOIN_WAIT: Duration = Duration::from_secs(1);
const DOWN_UNKNOWN_SESSION: &str = "unknown_session";

/// How long a craze lane keeps its own connection after its row left the
/// source (plan 032 §3.1) — the window in which a stopping session's last
/// events and its `reset{session_closed}` still land.
pub const CRAZE_DEPART_GRACE: Duration = Duration::from_secs(10);

/// `Down` reason emitted when a departing lane's grace expires after its row
/// left the roster.
pub const DOWN_SESSION_GONE: &str = "session_gone";
/// `Down` reason emitted when a departing lane's grace expires after the hub
/// went Dormant.
pub const DOWN_HUB_GONE: &str = "hub_gone";
/// `Down` reason emitted immediately when the shed is removed.
pub const DOWN_HOST_REMOVED: &str = "host_removed";

fn down_is_final(reason: &str) -> bool {
    reason == DOWN_UNKNOWN_SESSION
        || reason == "session_closed"
        || reason == "start_failed"
        || reason.starts_with("start_failed:")
        || reason == DOWN_SESSION_GONE
        || reason == DOWN_HUB_GONE
        || reason == DOWN_HOST_REMOVED
}

/// Why a craze lane's row left its source.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Departure {
    /// The roster dropped the row (`Removed`, or a `Ready` swap without it).
    Removed,
    /// The hub went away (source went Dormant).
    HubGone,
}

impl Departure {
    fn reason(self) -> &'static str {
        match self {
            Departure::Removed => DOWN_SESSION_GONE,
            Departure::HubGone => DOWN_HUB_GONE,
        }
    }
}

/// Generation- and sequence-tagged craze source news delivered to
/// [`ShedLanes::craze_news`].
#[derive(Debug, Clone, Copy)]
pub enum CrazeNews<'a> {
    Departed {
        host_ids: &'a [String],
        cause: Departure,
        seq: u64,
    },
    Returned {
        host_ids: &'a [String],
        seq: u64,
    },
    HostRemoved {
        seq: u64,
    },
}

/// `(ShedId, session_id)` — one open craze lane.
type Key = (ShedId, String);

fn key(target: &ShedId, session_id: &str) -> Key {
    (target.clone(), session_id.to_string())
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// What a `lane.*` op can fail with.
#[derive(Debug)]
pub enum LaneFailure {
    NoLane(String),
    OutcomeUnknown(String),
    ShuttingDown,
    Ended,
    Lane(LaneError),
}

impl LaneFailure {
    pub fn bad_request(message: String) -> LaneFailure {
        LaneFailure::Lane(LaneError::BadRequest(message))
    }

    /// The IPC envelope's `error.code`.
    pub fn code(&self) -> &'static str {
        match self {
            LaneFailure::NoLane(_) => "no_lane",
            LaneFailure::OutcomeUnknown(_) => "outcome_unknown",
            LaneFailure::ShuttingDown => SHUTTING_DOWN,
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
            LaneFailure::ShuttingDown => ShuttingDown.to_string(),
            LaneFailure::Ended => "the session has ended".to_string(),
            LaneFailure::Lane(e) => e.to_string(),
        }
    }

    /// Formatted `"<code>: <message>"` error string for command doors.
    pub fn command_string(&self) -> String {
        format!("{}: {}", self.code(), self.message())
    }
}

impl From<LaneError> for LaneFailure {
    fn from(e: LaneError) -> Self {
        if shed_craze::is_outcome_unknown(&e) {
            return LaneFailure::OutcomeUnknown(e.to_string());
        }
        LaneFailure::Lane(e)
    }
}

/// `lane.set_title`'s answer: `{}` on success, `{warning}` on `index_write`.
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
// host seam
// ---------------------------------------------------------------------------

/// What [`ShedLanes`] reads from [`ShedHosts`].
pub trait LaneHosts: Send + Sync {
    /// The craze lanes this shed currently lists: hostId → stamp.
    fn craze_lanes(&self, shed: &ShedId) -> BTreeMap<String, AgentLaneStamp>;

    /// This shed's current craze source and the generation it was started
    /// under.
    fn craze_source(&self, shed: &ShedId) -> Result<(CrazeSource, u64), String>;
}

impl LaneHosts for ShedHosts {
    fn craze_lanes(&self, shed: &ShedId) -> BTreeMap<String, AgentLaneStamp> {
        ShedHosts::craze_lanes(self, shed)
    }

    fn craze_source(&self, shed: &ShedId) -> Result<(CrazeSource, u64), String> {
        ShedHosts::craze_source(self, shed)
    }
}

// ---------------------------------------------------------------------------
// pending opens & open gates
// ---------------------------------------------------------------------------

struct Pending {
    cancelled: bool,
    craze_gen: u64,
    news: NewsMark,
}

struct PendingGuard {
    inner: Arc<Mutex<Inner>>,
    key: Key,
}

impl Drop for PendingGuard {
    fn drop(&mut self) {
        lock(&self.inner).pending.remove(&self.key);
    }
}

type Gates = HashMap<Key, Arc<tokio::sync::Mutex<()>>>;

struct GateGuard {
    gates: Arc<Mutex<Gates>>,
    key: Key,
    gate: Arc<tokio::sync::Mutex<()>>,
}

impl GateGuard {
    async fn lock(&self) -> tokio::sync::MutexGuard<'_, ()> {
        self.gate.lock().await
    }
}

impl Drop for GateGuard {
    fn drop(&mut self) {
        let mut gates = lock(&self.gates);
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

#[derive(Debug, Clone, PartialEq, Eq)]
enum EntryState {
    Live,
    Departing { token: u64, cause: Departure },
    Finished { reason: String },
}

#[derive(Debug)]
struct Life {
    state: EntryState,
    closing: bool,
    news: NewsMark,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, PartialOrd, Ord)]
struct NewsMark(u64);

impl NewsMark {
    fn take(&mut self, seq: u64) -> bool {
        if seq <= self.0 {
            return false;
        }
        self.0 = seq;
        true
    }
}

impl Life {
    fn admits(&self) -> bool {
        !self.closing && !matches!(self.state, EntryState::Finished { .. })
    }
}

struct LaneCore {
    /// Orders this lane's emits to match its state transitions: held across
    /// a transition AND its emit, so no event can reach [`LaneEvents`] after
    /// the terminal `Down` that followed it. Taken before `state`, and never
    /// while holding `ShedLanes::inner` or `state` — callbacks still run with
    /// neither held, so they may re-enter (`messages`, `snapshot`).
    emitting: Mutex<()>,
    state: Mutex<Life>,
    view: Mutex<LaneView>,
    finished: tokio::sync::Notify,
    client: Mutex<Option<Arc<dyn AgentLane>>>,
}

impl LaneCore {
    fn new(client: Option<Arc<dyn AgentLane>>) -> Arc<LaneCore> {
        Arc::new(LaneCore {
            emitting: Mutex::new(()),
            state: Mutex::new(Life {
                state: EntryState::Live,
                closing: false,
                news: NewsMark::default(),
            }),
            view: Mutex::new(LaneView::default()),
            finished: tokio::sync::Notify::new(),
            client: Mutex::new(client),
        })
    }

    fn client(&self) -> Result<Option<Arc<dyn AgentLane>>, LaneFailure> {
        let life = lock(&self.state);
        if life.closing {
            return Err(LaneFailure::ShuttingDown);
        }
        if matches!(life.state, EntryState::Finished { .. }) {
            return Err(LaneFailure::Ended);
        }
        Ok(lock(&self.client).clone())
    }

    fn release(&self) {
        let client = lock(&self.client).take();
        drop(client);
    }

    fn end(&self, events: &Arc<dyn LaneEvents>, key: &Key, reason: &str) {
        self.finish(events, key, reason);
        self.release();
    }

    fn apply(&self, events: &Arc<dyn LaneEvents>, key: &Key, event: &LaneEvent) -> bool {
        let _order = lock(&self.emitting);
        {
            let life = lock(&self.state);
            if !life.admits() {
                return false;
            }
            lock(&self.view).apply(event);
        }
        emit(events, key, event);
        true
    }

    fn finish(&self, events: &Arc<dyn LaneEvents>, key: &Key, reason: &str) -> bool {
        let _order = lock(&self.emitting);
        let down = {
            let mut life = lock(&self.state);
            if !life.admits() {
                return false;
            }
            finish_locked(&mut life, &self.view, reason)
        };
        emit(events, key, &down);
        self.finished.notify_waiters();
        true
    }

    /// Mark the lane removed by its host under locks, returning `(matched,
    /// optional_down_event)` so callers emit `LaneEvents` and notify waiters
    /// AFTER releasing every mutex (including `ShedLanes::inner`).
    fn host_removed_locked(&self, seq: u64) -> (bool, Option<LaneEvent>) {
        let mut life = lock(&self.state);
        if !life.news.take(seq) {
            return (false, None);
        }
        let down = life
            .admits()
            .then(|| finish_locked(&mut life, &self.view, DOWN_HOST_REMOVED));
        (true, down)
    }

    fn expire(&self, events: &Arc<dyn LaneEvents>, key: &Key, token: u64) -> bool {
        let _order = lock(&self.emitting);
        let down = {
            let mut life = lock(&self.state);
            let cause = match life.state {
                EntryState::Departing { token: held, cause } if held == token && !life.closing => {
                    cause
                }
                _ => return false,
            };
            finish_locked(&mut life, &self.view, cause.reason())
        };
        emit(events, key, &down);
        self.finished.notify_waiters();
        true
    }

    fn departing_under(&self, token: u64) -> bool {
        matches!(lock(&self.state).state, EntryState::Departing { token: held, .. } if held == token)
    }

    fn is_finished(&self) -> bool {
        matches!(lock(&self.state).state, EntryState::Finished { .. })
    }
}

fn finish_locked(life: &mut Life, view: &Mutex<LaneView>, reason: &str) -> LaneEvent {
    let down = LaneEvent::Down {
        reason: reason.to_string(),
    };
    lock(view).apply(&down);
    life.state = EntryState::Finished {
        reason: reason.to_string(),
    };
    down
}

struct LaneEntry {
    session_id: String,
    session: LaneSession,
    core: Arc<LaneCore>,
    pump: Mutex<Option<tokio::task::JoinHandle<()>>>,
    grace: Mutex<Option<tokio::task::JoinHandle<()>>>,
    craze_gen: u64,
}

impl LaneEntry {
    fn opened(&self) -> Value {
        json!({ "session": self.session })
    }

    fn is_finished(&self) -> bool {
        self.core.is_finished()
    }

    fn client(&self) -> Result<Arc<dyn AgentLane>, LaneFailure> {
        self.core.client()?.ok_or_else(|| {
            LaneFailure::NoLane(format!(
                "the lane for session {:?} was closed",
                self.session_id
            ))
        })
    }

    fn retire(&self) {
        self.abort_pump();
        self.core.release();
    }

    fn detach(&self) {
        lock(&self.core.state).closing = true;
    }

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
    fn drop(&mut self) {
        self.abort_pump();
        if let Some(grace) = lock(&self.grace).as_ref() {
            grace.abort();
        }
    }
}

// ---------------------------------------------------------------------------
// the registry
// ---------------------------------------------------------------------------

#[derive(Default)]
struct Inner {
    entries: HashMap<Key, Arc<LaneEntry>>,
    pending: HashMap<Key, Pending>,
    closing: bool,
    departures: u64,
    craze_marks: HashMap<ShedId, CrazeMarks>,
}

const CRAZE_MARKS_CAP: usize = 4096;

#[derive(Debug, Default)]
struct CrazeMarks {
    gen: u64,
    removed: bool,
    hosts: HashMap<String, NewsMark>,
}

impl Inner {
    fn note_craze_news(&mut self, shed: &ShedId, gen: u64, seq: u64, host_ids: &[String]) {
        let marks = self.craze_marks.entry(shed.clone()).or_default();
        if gen > marks.gen {
            *marks = CrazeMarks {
                gen,
                ..CrazeMarks::default()
            };
        }
        if gen < marks.gen || marks.removed {
            return;
        }
        for host_id in host_ids {
            marks.hosts.entry(host_id.clone()).or_default().take(seq);
        }
        while marks.hosts.len() > CRAZE_MARKS_CAP {
            let oldest = marks
                .hosts
                .iter()
                .min_by_key(|(_, mark)| mark.0)
                .map(|(host_id, _)| host_id.clone());
            match oldest {
                Some(host_id) => {
                    marks.hosts.remove(&host_id);
                }
                None => break,
            }
        }
    }

    fn note_craze_removed(&mut self, shed: &ShedId, gen: u64) {
        let marks = self.craze_marks.entry(shed.clone()).or_default();
        if gen >= marks.gen {
            *marks = CrazeMarks {
                gen,
                removed: true,
                hosts: HashMap::new(),
            };
        }
    }

    fn craze_mark(&self, shed: &ShedId, gen: u64, host_id: &str) -> NewsMark {
        self.craze_marks
            .get(shed)
            .filter(|marks| marks.gen == gen)
            .and_then(|marks| marks.hosts.get(host_id).copied())
            .unwrap_or_default()
    }

    fn close(&mut self) {
        for entry in self.entries.values() {
            entry.detach();
        }
        self.closing = true;
    }
}

/// Summary of what [`ShedLanes::shutdown`] closed.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LanesClosed {
    pub lanes: usize,
    pub finished: usize,
    pub pumps: usize,
    pub pumps_unjoined: usize,
    pub graces: usize,
    pub graces_unjoined: usize,
}

/// Every open craze lane across all running sheds.
pub struct ShedLanes {
    handle: tokio::runtime::Handle,
    events: Arc<dyn LaneEvents>,
    hosts: Arc<dyn LaneHosts>,
    inner: Arc<Mutex<Inner>>,
    gates: Arc<Mutex<Gates>>,
    depart_grace: Duration,
    #[cfg(test)]
    pub(crate) open_gap: Mutex<Option<crate::shed_hosts::TestGap>>,
    #[cfg(test)]
    pub(crate) commit_gap: Mutex<Option<crate::shed_hosts::TestGap>>,
    #[cfg(test)]
    pub(crate) verb_gaps: Mutex<Vec<(Admission, crate::shed_hosts::TestGap)>>,
}

#[cfg(test)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Admission {
    Before,
    After,
}

impl ShedLanes {
    pub fn new(
        handle: tokio::runtime::Handle,
        events: Arc<dyn LaneEvents>,
        hosts: Arc<dyn LaneHosts>,
    ) -> ShedLanes {
        ShedLanes {
            handle,
            events,
            hosts,
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

    /// Override the departure grace duration (used by tests).
    pub fn with_depart_grace(mut self, grace: Duration) -> ShedLanes {
        self.depart_grace = grace;
        self
    }

    #[cfg(test)]
    pub(crate) fn is_open(&self, target: &ShedId, session_id: &str) -> bool {
        lock(&self.inner)
            .entries
            .contains_key(&key(target, session_id))
    }

    #[cfg(test)]
    pub(crate) fn lane_state(&self, target: &ShedId, session_id: &str) -> Option<&'static str> {
        let entry = self.entry(&key(target, session_id))?;
        let life = lock(&entry.core.state);
        Some(match life.state {
            EntryState::Live => "live",
            EntryState::Departing { .. } => "departing",
            EntryState::Finished { .. } => "finished",
        })
    }

    /// `lane.open` — open (or return an existing) craze lane on `target`.
    pub async fn open(&self, target: &ShedId, session_id: &str) -> Result<Value, LaneFailure> {
        let key = key(target, session_id);
        if self.closing() {
            return Err(LaneFailure::ShuttingDown);
        }
        if self.entry(&key).is_none() {
            self.lane_stamp(target, session_id)?;
        }
        let gate = self.gate(&key);
        let _serialized = gate.lock().await;
        if self.closing() {
            return Err(LaneFailure::ShuttingDown);
        }

        let resolved = self.lane_stamp(target, session_id);
        if let Some(entry) = self.entry(&key) {
            let finished = entry.is_finished();
            match &resolved {
                Err(_) => return Ok(entry.opened()),
                Ok(_) if finished => {}
                Ok(_) if self.current_generation(target, &entry) => {
                    return Ok(entry.opened());
                }
                Ok(_) => self.evict(&key),
            }
        }
        let _stamp = resolved?;

        let (source, craze_gen) = self
            .hosts
            .craze_source(target)
            .map_err(|e| LaneFailure::Lane(LaneError::Unavailable(e)))?;

        #[cfg(test)]
        crate::shed_hosts::at_gap(&self.open_gap).await;

        let _pending = self.declare(&key, craze_gen)?;

        // Re-check generation after declaration so a removal published before
        // declaration cannot leave a zombie lane from a dead source.
        let current = self.hosts.craze_source(target).map(|(_, now)| now);
        if current.as_ref().ok() != Some(&craze_gen) {
            return Err(LaneFailure::Lane(LaneError::Unavailable(format!(
                "{target}'s craze source stopped while the lane for session \
                 {session_id:?} was being opened"
            ))));
        }

        let client: Arc<dyn AgentLane> = source.open(session_id).await?;
        let session = client.session().await.map_err(LaneFailure::from)?;
        let core = LaneCore::new(Some(Arc::clone(&client)));

        #[cfg(test)]
        crate::shed_hosts::at_gap(&self.commit_gap).await;

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
                "the lane for session {session_id:?} on {target} was closed \
                 while it was being opened"
            )));
        };
        let news = news.max(inner.craze_mark(target, craze_gen, session_id));
        lock(&core.state).news = news;
        let pump = self.spawn_pump(key.clone(), client, Arc::clone(&core));
        let entry = Arc::new(LaneEntry {
            session_id: session_id.to_string(),
            session,
            core,
            pump: Mutex::new(Some(pump)),
            grace: Mutex::new(None),
            craze_gen,
        });
        let opened = entry.opened();
        let replaced = inner.entries.insert(key, entry);
        if let Some(old) = &replaced {
            old.detach();
        }
        drop(inner);
        if let Some(old) = replaced {
            old.cancel_grace();
            old.retire();
        }
        Ok(opened)
    }

    fn current_generation(&self, target: &ShedId, entry: &LaneEntry) -> bool {
        self.hosts
            .craze_source(target)
            .is_ok_and(|(_, now)| now == entry.craze_gen)
    }

    /// `lane.messages` — staged-then-swapped view as IPC payload.
    pub fn messages(&self, target: &ShedId, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(target, session_id)?;
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
            "slash_menu": snap.slash_menu,
            "effort_option": snap.effort_option,
        }))
    }

    /// `lane.approvals` — pending approvals, oldest first.
    pub fn approvals(&self, target: &ShedId, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(target, session_id)?;
        let snap = lock(&entry.core.view).snapshot(None);
        Ok(json!({ "approvals": snap.approvals }))
    }

    /// `lane.send` — send a prompt.
    pub async fn send(
        &self,
        target: &ShedId,
        session_id: &str,
        text: &str,
        mode: SendMode,
    ) -> Result<Value, LaneFailure> {
        self.admit(target, session_id)
            .await?
            .send(text, mode)
            .await?;
        Ok(json!({}))
    }

    /// `lane.cancel` — stop the turn in flight.
    pub async fn cancel(&self, target: &ShedId, session_id: &str) -> Result<Value, LaneFailure> {
        self.admit(target, session_id).await?.cancel().await?;
        Ok(json!({}))
    }

    /// `lane.stop` — end the session.
    pub async fn stop(&self, target: &ShedId, session_id: &str) -> Result<Value, LaneFailure> {
        self.admit(target, session_id).await?.stop().await?;
        Ok(json!({}))
    }

    /// `lane.settings` — one-shot settings read (answers from view when finished).
    pub async fn settings(&self, target: &ShedId, session_id: &str) -> Result<Value, LaneFailure> {
        let entry = self.open_entry(target, session_id)?;
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

    /// `lane.set` — change one setting.
    pub async fn set(
        &self,
        target: &ShedId,
        session_id: &str,
        change: LaneSettingChange,
    ) -> Result<Value, LaneFailure> {
        self.admit(target, session_id).await?.set(change).await?;
        Ok(json!({}))
    }

    /// `lane.set_title` — rename the session.
    pub async fn set_title(
        &self,
        target: &ShedId,
        session_id: &str,
        title: &str,
    ) -> Result<Value, LaneFailure> {
        let title = title.trim();
        if title.is_empty() {
            return Err(LaneFailure::bad_request(
                "a session's title must not be blank".to_string(),
            ));
        }
        renamed(self.admit(target, session_id).await?.set_title(title).await)
    }

    /// `lane.answer` — resolve one approval.
    pub async fn answer(
        &self,
        target: &ShedId,
        session_id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<Value, LaneFailure> {
        self.admit(target, session_id)
            .await?
            .answer(approval_id, answer)
            .await?;
        Ok(json!({}))
    }

    /// `lane.close` — idempotently end and remove the lane entry.
    pub fn close(&self, target: &ShedId, session_id: &str) -> Value {
        self.evict(&key(target, session_id));
        json!({})
    }

    /// Deliver one piece of generation- and sequence-fenced craze source news
    /// from [`ShedHosts`].
    pub fn craze_news(&self, target: &ShedId, gen: u64, news: CrazeNews<'_>) {
        match news {
            CrazeNews::Departed {
                host_ids,
                cause,
                seq,
            } => self.evict_craze(target, gen, seq, host_ids, cause),
            CrazeNews::Returned { host_ids, seq } => {
                self.craze_returned(target, gen, seq, host_ids)
            }
            CrazeNews::HostRemoved { seq } => self.evict_craze_generation(target, gen, seq),
        }
    }

    fn evict_craze(
        &self,
        target: &ShedId,
        gen: u64,
        seq: u64,
        host_ids: &[String],
        cause: Departure,
    ) {
        let mut inner = lock(&self.inner);
        inner.note_craze_news(target, gen, seq, host_ids);
        for host_id in host_ids {
            let key = key(target, host_id);
            if let Some(pending) = inner.pending.get_mut(&key) {
                if pending.craze_gen == gen && pending.news.take(seq) {
                    pending.cancelled = true;
                }
            }
            let Some(entry) = inner
                .entries
                .get(&key)
                .filter(|entry| entry.craze_gen == gen)
                .map(Arc::clone)
            else {
                continue;
            };
            let mut life = lock(&entry.core.state);
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
                Arc::clone(&self.events),
                key,
            ));
            drop(life);
            let stale = lock(&entry.grace).replace(task);
            if let Some(stale) = stale {
                stale.abort();
            }
        }
    }

    fn craze_returned(&self, target: &ShedId, gen: u64, seq: u64, host_ids: &[String]) {
        let mut inner = lock(&self.inner);
        inner.note_craze_news(target, gen, seq, host_ids);
        for host_id in host_ids {
            let key = key(target, host_id);
            if let Some(pending) = inner.pending.get_mut(&key) {
                if pending.craze_gen == gen {
                    pending.news.take(seq);
                }
            }
            let Some(entry) = inner
                .entries
                .get(&key)
                .filter(|entry| entry.craze_gen == gen)
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

    fn evict_craze_generation(&self, target: &ShedId, gen: u64, seq: u64) {
        let ended: Vec<(Key, Arc<LaneEntry>, Option<LaneEvent>)> = {
            let mut inner = lock(&self.inner);
            inner.note_craze_removed(target, gen);
            for (key, pending) in inner.pending.iter_mut() {
                if &key.0 == target && pending.craze_gen == gen && pending.news.take(seq) {
                    pending.cancelled = true;
                }
            }
            inner
                .entries
                .iter()
                .filter(|((shed, _), entry)| shed == target && entry.craze_gen == gen)
                .filter_map(|(key, entry)| {
                    let (matched, down) = entry.core.host_removed_locked(seq);
                    matched.then(|| (key.clone(), Arc::clone(entry), down))
                })
                .collect()
        };
        for (key, entry, down) in ended {
            if let Some(down) = down {
                // `inner` is released: wait out any emit in flight so the Down
                // is the last event this lane publishes.
                {
                    let _order = lock(&entry.core.emitting);
                    emit(&self.events, &key, &down);
                }
                entry.core.finished.notify_waiters();
            }
            entry.cancel_grace();
            entry.retire();
        }
    }

    // ---- shutdown ----

    /// Fence every entry, then refuse all new opens.
    pub fn begin_closing(&self) {
        lock(&self.inner).close();
    }

    /// Blocking shutdown phase (must run on a dedicated OS thread, never on a
    /// tokio runtime worker).
    pub fn shutdown(&self) -> LanesClosed {
        let entries = {
            let mut inner = lock(&self.inner);
            inner.close();
            for pending in inner.pending.values_mut() {
                pending.cancelled = true;
            }
            inner
                .entries
                .drain()
                .map(|(_, e)| e)
                .collect::<Vec<Arc<LaneEntry>>>()
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
        let (pumps_unjoined, graces_unjoined) = if pumps.is_empty() && graces.is_empty() {
            (0, 0)
        } else {
            self.handle.block_on(async move {
                let deadline = tokio::time::Instant::now() + PUMP_JOIN_WAIT;
                (
                    join_by(deadline, pumps).await,
                    join_by(deadline, graces).await,
                )
            })
        };
        let closed = LanesClosed {
            lanes: entries.len(),
            finished,
            pumps: pump_count,
            pumps_unjoined,
            graces: grace_count,
            graces_unjoined,
        };
        for entry in entries {
            entry.retire();
        }
        closed
    }

    // ---- internals ----

    fn declare(&self, key: &Key, craze_gen: u64) -> Result<PendingGuard, LaneFailure> {
        let mut inner = lock(&self.inner);
        if inner.closing {
            return Err(LaneFailure::ShuttingDown);
        }
        let news = inner.craze_mark(&key.0, craze_gen, &key.1);
        inner.pending.insert(
            key.clone(),
            Pending {
                cancelled: false,
                craze_gen,
                news,
            },
        );
        Ok(PendingGuard {
            inner: Arc::clone(&self.inner),
            key: key.clone(),
        })
    }

    fn closing(&self) -> bool {
        lock(&self.inner).closing
    }

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

    async fn admit(
        &self,
        target: &ShedId,
        session_id: &str,
    ) -> Result<Arc<dyn AgentLane>, LaneFailure> {
        if self.closing() {
            return Err(LaneFailure::ShuttingDown);
        }
        let entry = self.open_entry(target, session_id)?;
        #[cfg(test)]
        self.at_verb_gap(Admission::Before).await;
        let client = entry.client()?;
        drop(entry);
        #[cfg(test)]
        self.at_verb_gap(Admission::After).await;
        Ok(client)
    }

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

    fn open_entry(&self, target: &ShedId, session_id: &str) -> Result<Arc<LaneEntry>, LaneFailure> {
        let key = key(target, session_id);
        if let Some(entry) = self.entry(&key) {
            return Ok(entry);
        }
        match self.lane_stamp(target, session_id) {
            Err(e) => Err(e),
            Ok(_) => Err(LaneFailure::NoLane(format!(
                "no lane is open for session {session_id:?} on {target} — call lane.open first"
            ))),
        }
    }

    fn lane_stamp(&self, target: &ShedId, session_id: &str) -> Result<AgentLaneStamp, LaneFailure> {
        self.hosts
            .craze_lanes(target)
            .remove(session_id)
            .ok_or_else(|| {
                LaneFailure::NoLane(format!(
                    "{target}'s craze source lists no session {session_id:?}"
                ))
            })
    }

    fn evict(&self, key: &Key) {
        let entry = {
            let mut inner = lock(&self.inner);
            if let Some(pending) = inner.pending.get_mut(key) {
                pending.cancelled = true;
            }
            let entry = inner.entries.remove(key);
            if let Some(entry) = &entry {
                entry.detach();
            }
            entry
        };
        if let Some(entry) = entry {
            entry.cancel_grace();
            entry.retire();
        }
    }

    fn spawn_pump(
        &self,
        key: Key,
        client: Arc<dyn AgentLane>,
        core: Arc<LaneCore>,
    ) -> tokio::task::JoinHandle<()> {
        let events = Arc::clone(&self.events);
        self.handle.spawn(async move {
            let mut backoff = RESUBSCRIBE_BASE;
            loop {
                let subscription = match client.subscribe(None).await {
                    Ok(subscription) => subscription,
                    Err(LaneError::UnknownSession) => {
                        core.end(&events, &key, DOWN_UNKNOWN_SESSION);
                        return;
                    }
                    Err(e) => {
                        if !core.apply(&events, &key, &stale(e.to_string())) {
                            return;
                        }
                        tokio::time::sleep(backoff).await;
                        backoff = next_backoff(backoff);
                        continue;
                    }
                };
                let (mut rx, stop) = subscription.into_parts();
                while let Some(event) = rx.recv().await {
                    match &event {
                        LaneEvent::Ready { .. } => backoff = RESUBSCRIBE_BASE,
                        LaneEvent::Down { reason } if down_is_final(reason) => {
                            core.end(&events, &key, reason);
                            return;
                        }
                        _ => {}
                    }
                    if !core.apply(&events, &key, &event) {
                        return;
                    }
                }
                drop(stop);
                tokio::time::sleep(backoff).await;
                backoff = next_backoff(backoff);
            }
        })
    }
}

async fn join_by(deadline: tokio::time::Instant, tasks: Vec<tokio::task::JoinHandle<()>>) -> usize {
    let mut unjoined = 0;
    for task in tasks {
        if tokio::time::timeout_at(deadline, task).await.is_err() {
            unjoined += 1;
        }
    }
    unjoined
}

fn stale(reason: String) -> LaneEvent {
    LaneEvent::Stale { reason }
}

async fn depart(
    entry: Weak<LaneEntry>,
    core: Arc<LaneCore>,
    token: u64,
    grace: Duration,
    events: Arc<dyn LaneEvents>,
    key: Key,
) {
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
    if core.expire(&events, &key, token) {
        entry.retire();
    }
}

fn emit(events: &Arc<dyn LaneEvents>, key: &Key, event: &LaneEvent) {
    events.on_lane_event(LaneUiEvent {
        target: key.0.clone(),
        session_id: key.1.clone(),
        event: event.clone(),
    });
}

fn next_backoff(current: Duration) -> Duration {
    std::cmp::min(current.saturating_mul(2), RESUBSCRIBE_MAX)
}

/// Decode `lane.answer`'s `answer` object strictly.
pub fn parse_answer(value: &Value) -> Result<LaneAnswer, LaneFailure> {
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
        _ => Err(LaneFailure::bad_request(
            "`reject` must be the literal true".to_string(),
        )),
    }
}

/// Decode `lane.set`'s `change` strictly.
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
    use std::sync::atomic::{AtomicU64, Ordering::SeqCst};

    use shed_craze::testing::{
        attach_result, full_hub_capabilities, host_session_row, roster_row, session_caps,
        session_info, snapshot_at, HubEnd, ScriptedDial,
    };

    const GHOST: &str = "0123456789ab";
    const GSID: &str = "ses-1";
    const GINC: &str = "inc-1";
    const TEST_GRACE: Duration = Duration::from_millis(120);

    fn shed(name: &str) -> ShedId {
        ShedId::new("srv", name).unwrap()
    }

    #[derive(Default)]
    struct RecordedEvents {
        events: Mutex<Vec<LaneUiEvent>>,
    }

    impl RecordedEvents {
        fn total(&self) -> usize {
            lock(&self.events).len()
        }

        fn downs(&self) -> Vec<String> {
            lock(&self.events)
                .iter()
                .filter_map(|e| match &e.event {
                    LaneEvent::Down { reason } => Some(reason.clone()),
                    _ => None,
                })
                .collect()
        }

        fn snapshot(&self) -> Vec<LaneUiEvent> {
            lock(&self.events).clone()
        }
    }

    impl LaneEvents for RecordedEvents {
        fn on_lane_event(&self, event: LaneUiEvent) {
            lock(&self.events).push(event);
        }
    }

    type FakeSourceEntry = (CrazeSource, u64, Vec<String>);

    struct FakeHosts {
        sources: Mutex<HashMap<ShedId, FakeSourceEntry>>,
    }

    impl FakeHosts {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                sources: Mutex::new(HashMap::new()),
            })
        }

        fn insert(&self, shed: ShedId, source: CrazeSource, gen: u64, ids: Vec<String>) {
            lock(&self.sources).insert(shed, (source, gen, ids));
        }

        fn set_listed(&self, shed: &ShedId, ids: Vec<String>) {
            if let Some(entry) = lock(&self.sources).get_mut(shed) {
                entry.2 = ids;
            }
        }
    }

    impl LaneHosts for FakeHosts {
        fn craze_lanes(&self, shed: &ShedId) -> BTreeMap<String, AgentLaneStamp> {
            lock(&self.sources)
                .get(shed)
                .map(|(_, _, ids)| {
                    ids.iter()
                        .map(|id| {
                            (
                                id.clone(),
                                AgentLaneStamp {
                                    kind: CRAZE_KIND.to_string(),
                                    session_id: id.clone(),
                                    server_url: String::new(),
                                },
                            )
                        })
                        .collect()
                })
                .unwrap_or_default()
        }

        fn craze_source(&self, shed: &ShedId) -> Result<(CrazeSource, u64), String> {
            lock(&self.sources)
                .get(shed)
                .map(|(source, gen, _)| (source.clone(), *gen))
                .ok_or_else(|| format!("{shed} has no craze source"))
        }
    }

    async fn wait_for<T>(what: &str, mut f: impl FnMut() -> Option<T>) -> T {
        for _ in 0..2_000 {
            if let Some(v) = f() {
                return v;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("timed out waiting for {what}");
    }

    struct CrazeRig {
        shed: ShedId,
        lanes: Arc<ShedLanes>,
        events: Arc<RecordedEvents>,
        hosts: Arc<FakeHosts>,
        _hub: HubEnd,
        conns: tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
        _roster: shed_lane::lane::SourceSubscription,
        seq: AtomicU64,
    }

    impl CrazeRig {
        async fn start() -> CrazeRig {
            CrazeRig::start_with(shed_craze::LaneTimings::default()).await
        }

        async fn start_with(lane: shed_craze::LaneTimings) -> CrazeRig {
            let target = shed("s1");
            let (dial, mut conns) = ScriptedDial::new();
            let source = CrazeSource::new(dial, "shed-desktop-test").with_lane_timings(lane);
            let roster = source.subscribe().await.expect("subscribe");
            let mut hub = conns.recv().await.expect("roster dialled");
            hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
            hub.subscribed(
                "sub-1",
                "0a1b2c3d4e5f",
                json!([roster_row(GHOST, GSID, "/home/shed", json!({"title": "w"}))]),
            )
            .await;
            wait_for("source lists row", || source.listed(GHOST).map(|_| ())).await;

            let hosts = FakeHosts::new();
            hosts.insert(target.clone(), source, 1, vec![GHOST.to_string()]);
            let events = Arc::new(RecordedEvents::default());
            let lanes = Arc::new(
                ShedLanes::new(
                    tokio::runtime::Handle::current(),
                    events.clone(),
                    hosts.clone(),
                )
                .with_depart_grace(TEST_GRACE),
            );
            CrazeRig {
                shed: target,
                lanes,
                events,
                hosts,
                _hub: hub,
                conns,
                _roster: roster,
                seq: AtomicU64::new(0),
            }
        }

        async fn open_seeded(&mut self) -> HubEnd {
            self.lanes
                .open(&self.shed, GHOST)
                .await
                .expect("lane opens");
            let mut conn = self.conns.recv().await.expect("lane dialled");
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
            wait_for("seed swaps in", || {
                let v = self.lanes.messages(&self.shed, GHOST).ok()?;
                (v["generation"].as_u64() >= Some(1)).then_some(())
            })
            .await;
            conn
        }

        fn seq(&self) -> u64 {
            self.seq.fetch_add(1, SeqCst) + 1
        }

        fn depart(&self, cause: Departure) {
            self.depart_as(cause, self.seq());
        }

        fn depart_as(&self, cause: Departure, seq: u64) {
            self.lanes.craze_news(
                &self.shed,
                1,
                CrazeNews::Departed {
                    host_ids: &[GHOST.to_string()],
                    cause,
                    seq,
                },
            );
        }

        fn returned(&self) {
            self.returned_as(self.seq());
        }

        fn returned_as(&self, seq: u64) {
            self.lanes.craze_news(
                &self.shed,
                1,
                CrazeNews::Returned {
                    host_ids: &[GHOST.to_string()],
                    seq,
                },
            );
        }

        fn host_removed(&self, gen: u64) {
            self.lanes
                .craze_news(&self.shed, gen, CrazeNews::HostRemoved { seq: self.seq() });
        }

        fn state(&self) -> Option<&'static str> {
            self.lanes.lane_state(&self.shed, GHOST)
        }

        fn entry(&self) -> Arc<LaneEntry> {
            self.lanes.entry(&key(&self.shed, GHOST)).expect("entry")
        }

        fn messages(&self) -> Value {
            self.lanes
                .messages(&self.shed, GHOST)
                .expect("lane.messages")
        }

        async fn finished(&self, what: &str) -> Value {
            wait_for(what, || (self.state() == Some("finished")).then_some(())).await;
            self.messages()
        }
    }

    #[test]
    fn lane_ui_event_serializes_with_target_and_no_machine_or_kind() {
        let ev = LaneUiEvent {
            target: shed("alpha"),
            session_id: GHOST.to_string(),
            event: LaneEvent::Down {
                reason: DOWN_SESSION_GONE.to_string(),
            },
        };
        let v = serde_json::to_value(&ev).expect("serialize");
        assert_eq!(v["target"], "roost:srv/alpha");
        assert_eq!(v["session_id"], GHOST);
        assert!(v.get("machine").is_none(), "must not emit machine: {v}");
        assert!(v.get("kind").is_none(), "must not emit kind: {v}");
    }

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
            assert!(seen.insert(code));
        }
        assert_eq!(LaneFailure::NoLane("nope".into()).code(), "no_lane");
        assert_eq!(LaneFailure::ShuttingDown.code(), "shutting_down");
        assert_eq!(LaneFailure::Ended.code(), "lane_ended");
        let lost = LaneFailure::from(shed_craze::errors::outcome_unknown("dropped"));
        assert_eq!(lost.code(), "outcome_unknown");
    }

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
            }
        );
        for bad in [
            json!({"kind": "effort", "id": "high"}),
            json!({"kind": "model", "id": "m", "value": "x"}),
            json!({"kind": "config", "id": "fast", "value": "true", "for_model": ""}),
            json!({"kind": "model", "id": ""}),
        ] {
            assert_eq!(parse_setting(&bad).unwrap_err().code(), "bad_request");
        }
    }

    #[test]
    fn answer_and_mode_parsing_are_strict() {
        assert_eq!(
            parse_answer(&json!({"choice": "  p 2  "})).unwrap(),
            LaneAnswer::Choice {
                option_id: "  p 2  ".into()
            }
        );
        assert_eq!(
            parse_answer(&json!({"permission": "allow-once"})).unwrap(),
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce
            }
        );
        assert_eq!(
            parse_answer(&json!({"question": [["yes"]], "custom_text": ["typed"]})).unwrap(),
            LaneAnswer::Question {
                answers: vec![vec!["yes".into()]],
                custom_text: vec![Some("typed".into())],
            }
        );
        assert_eq!(
            parse_answer(&json!({"reject": true})).unwrap(),
            LaneAnswer::Reject
        );
        for bad in [
            json!({}),
            json!({"choice": ""}),
            json!({"reject": false}),
            json!({"permission": "reject", "question": [["yes"]]}),
            json!({"permission": "allow-always", "option_id": "reject"}),
            json!({"choice": "p-2", "custom_text": ["typed"]}),
        ] {
            assert_eq!(parse_answer(&bad).unwrap_err().code(), "bad_request");
        }
        assert_eq!(parse_mode(None).unwrap(), SendMode::Queue);
        assert_eq!(parse_mode(Some("interject")).unwrap(), SendMode::Interject);
        assert_eq!(parse_mode(Some("fast")).unwrap_err().code(), "bad_request");
    }

    #[test]
    fn renamed_turns_index_write_into_warning() {
        assert_eq!(renamed(Ok(())).unwrap(), json!({}));
        let warn = renamed(Err(shed_craze::errors::set_title_error(
            &shed_craze::wire::RpcError::from_value(&json!({
                "code": -32000, "message": "index_write",
                "data": {"code": "index_write", "reason": "index_write", "cause": "disk full"}
            })),
        )))
        .unwrap();
        assert!(warn["warning"].as_str().unwrap().contains("disk full"));
    }

    /// Two different sheds hosting the same `session_id` stay completely
    /// isolated and emit `LaneUiEvent`s with distinct `target`s.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn two_sheds_with_same_session_id_route_events_to_distinct_targets() {
        let s1 = shed("s1");
        let s2 = shed("s2");
        let (dial1, mut conns1) = ScriptedDial::new();
        let (dial2, mut conns2) = ScriptedDial::new();
        let src1 = CrazeSource::new(dial1, "test");
        let src2 = CrazeSource::new(dial2, "test");
        let _r1 = src1.subscribe().await.unwrap();
        let _r2 = src2.subscribe().await.unwrap();
        let mut h1 = conns1.recv().await.unwrap();
        let mut h2 = conns2.recv().await.unwrap();
        h1.hello("111111111111", full_hub_capabilities()).await;
        h2.hello("222222222222", full_hub_capabilities()).await;
        h1.subscribed(
            "sub-1",
            "111111111111",
            json!([roster_row(
                GHOST,
                GSID,
                "/home/shed",
                json!({"title": "one"})
            )]),
        )
        .await;
        h2.subscribed(
            "sub-2",
            "222222222222",
            json!([roster_row(
                GHOST,
                GSID,
                "/home/shed",
                json!({"title": "two"})
            )]),
        )
        .await;
        wait_for("both sources list GHOST", || {
            (src1.listed(GHOST).is_some() && src2.listed(GHOST).is_some()).then_some(())
        })
        .await;

        let hosts = FakeHosts::new();
        hosts.insert(s1.clone(), src1, 1, vec![GHOST.to_string()]);
        hosts.insert(s2.clone(), src2, 1, vec![GHOST.to_string()]);
        let events = Arc::new(RecordedEvents::default());
        let lanes = Arc::new(ShedLanes::new(
            tokio::runtime::Handle::current(),
            events.clone(),
            hosts,
        ));

        lanes.open(&s1, GHOST).await.unwrap();
        lanes.open(&s2, GHOST).await.unwrap();
        let mut c1 = conns1.recv().await.unwrap();
        let mut c2 = conns2.recv().await.unwrap();
        c1.splice(GHOST).await;
        c2.splice(GHOST).await;
        let info1 = session_info(GHOST, GSID, GINC, session_caps(true));
        let info2 = session_info(GHOST, GSID, GINC, session_caps(true));
        c1.listed(host_session_row(&info1, json!({"title": "one"})))
            .await;
        c2.listed(host_session_row(&info2, json!({"title": "two"})))
            .await;
        c1.attached(attach_result(
            "s-1",
            &info1,
            (GINC, 1),
            Some(snapshot_at(GINC, 1, json!({}))),
            None,
        ))
        .await;
        c2.attached(attach_result(
            "s-2",
            &info2,
            (GINC, 1),
            Some(snapshot_at(GINC, 1, json!({}))),
            None,
        ))
        .await;
        c1.synchronized("s-1", 1).await;
        c2.synchronized("s-2", 1).await;

        wait_for("both sheds seeded", || {
            let m1 = lanes.messages(&s1, GHOST).ok()?;
            let m2 = lanes.messages(&s2, GHOST).ok()?;
            (m1["generation"].as_u64() >= Some(1) && m2["generation"].as_u64() >= Some(1))
                .then_some(())
        })
        .await;

        let recorded = events.snapshot();
        assert!(recorded
            .iter()
            .any(|e| e.target == s1 && e.session_id == GHOST));
        assert!(recorded
            .iter()
            .any(|e| e.target == s2 && e.session_id == GHOST));

        // Closing s1 leaves s2 untouched.
        lanes.close(&s1, GHOST);
        assert!(!lanes.is_open(&s1, GHOST));
        assert!(lanes.is_open(&s2, GHOST));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_vanished_hosts_lane_ends_session_gone_within_the_grace() {
        let mut rig = CrazeRig::start().await;
        let mut conn = rig.open_seeded().await;
        let departed = std::time::Instant::now();
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"));
        assert_eq!(rig.messages()["finished"], false);
        let view = rig.finished("grace ends lane").await;
        let took = departed.elapsed();
        assert!(took >= TEST_GRACE && took <= TEST_GRACE + Duration::from_secs(1));
        assert_eq!(rig.events.downs(), [DOWN_SESSION_GONE]);
        assert_eq!(view["finished"], true);
        assert_eq!(view["ended"], true);
        assert_eq!(view["stale"], DOWN_SESSION_GONE);
        assert!(lock(&rig.entry().core.client).is_none());
        assert!(conn.recv_within(Duration::from_secs(5)).await.is_none());
        let refused = rig
            .lanes
            .send(&rig.shed, GHOST, "anyone?", SendMode::Queue)
            .await
            .expect_err("ended lane refuses verb");
        assert_eq!(refused.code(), "lane_ended");
        assert!(rig.lanes.approvals(&rig.shed, GHOST).is_ok());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_return_within_the_grace_keeps_the_lane_live() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"));
        rig.returned();
        assert_eq!(rig.state(), Some("live"));
        assert!(lock(&rig.entry().grace).is_none());
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(rig.state(), Some("live"));
        assert!(rig.events.downs().is_empty());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_host_removed_during_a_grace_ends_the_lane_at_once() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"));
        rig.host_removed(1);
        assert_eq!(rig.state(), Some("finished"));
        assert_eq!(rig.events.downs(), [DOWN_HOST_REMOVED]);
        assert!(lock(&rig.entry().core.client).is_none());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_old_generations_news_touches_nothing() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        // Generation 2 news does not match generation 1 entry.
        rig.lanes.craze_news(
            &rig.shed,
            2,
            CrazeNews::Departed {
                host_ids: &[GHOST.to_string()],
                cause: Departure::Removed,
                seq: 1,
            },
        );
        assert_eq!(rig.state(), Some("live"));
        rig.lanes
            .craze_news(&rig.shed, 2, CrazeNews::HostRemoved { seq: 2 });
        assert_eq!(rig.state(), Some("live"));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn news_sequence_prevents_out_of_order_revival_or_departure() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        let s1 = rig.seq();
        let s2 = rig.seq();
        // Depart minted at s2 lands first, then stale return minted at s1 lands late.
        rig.depart_as(Departure::Removed, s2);
        assert_eq!(rig.state(), Some("departing"));
        rig.returned_as(s1);
        assert_eq!(rig.state(), Some("departing"), "stale return ignored");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn news_that_landed_before_the_open_fences_the_lane() {
        let mut rig = CrazeRig::start().await;
        let stale_depart = rig.seq();
        let newer_return = rig.seq();
        // Newer return lands while no lane is open yet.
        rig.returned_as(newer_return);
        let _conn = rig.open_seeded().await;
        // Stale departure arrives afterwards and is ignored by CrazeMarks.
        rig.depart_as(Departure::HubGone, stale_depart);
        assert_eq!(rig.state(), Some("live"));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn open_on_a_departing_and_finished_entry_behaves_correctly() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.hosts.set_listed(&rig.shed, vec![]);
        rig.depart(Departure::Removed);
        assert_eq!(rig.state(), Some("departing"));
        // Re-opening a departing entry with no row returns it as-is.
        assert!(rig.lanes.open(&rig.shed, GHOST).await.is_ok());
        rig.finished("grace expires").await;
        // Re-opening a finished entry with no row still returns it as-is.
        assert!(rig.lanes.open(&rig.shed, GHOST).await.is_ok());
        // Settings on a finished entry answer from view without adapter call.
        assert!(rig.lanes.settings(&rig.shed, GHOST).await.is_ok());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn an_expired_dormant_departure_ends_hub_gone() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::HubGone);
        rig.finished("hub_gone grace").await;
        assert_eq!(rig.events.downs(), [DOWN_HUB_GONE]);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_craze_verb_racing_the_final_down_both_orders() {
        let lane = shed_craze::LaneTimings {
            wait_connected: Duration::from_millis(300),
            ..shed_craze::LaneTimings::default()
        };
        let mut rig = CrazeRig::start_with(lane).await;
        let mut conn = rig.open_seeded().await;

        let arm = |side: Admission| {
            let reached = Arc::new(tokio::sync::Notify::new());
            let proceed = Arc::new(tokio::sync::Notify::new());
            lock(&rig.lanes.verb_gaps).push((side, (Arc::clone(&reached), Arc::clone(&proceed))));
            (reached, proceed)
        };
        let (at, admitted_go) = arm(Admission::After);
        let admitted = {
            let lanes = Arc::clone(&rig.lanes);
            let target = rig.shed.clone();
            tokio::spawn(async move {
                lanes
                    .send(&target, GHOST, "admitted before", SendMode::Queue)
                    .await
            })
        };
        at.notified().await;

        let (at, late_go) = arm(Admission::Before);
        let late = {
            let lanes = Arc::clone(&rig.lanes);
            let target = rig.shed.clone();
            tokio::spawn(async move {
                lanes
                    .send(&target, GHOST, "admitted after", SendMode::Queue)
                    .await
            })
        };
        at.notified().await;

        conn.reset("s-1", "session_closed").await;
        rig.finished("session closed").await;
        let entry = rig.entry();
        wait_for("pump releases adapter", || {
            lock(&entry.core.client).is_none().then_some(())
        })
        .await;

        late_go.notify_one();
        let refused = late.await.unwrap().expect_err("admitted after end");
        assert_eq!(refused.code(), "lane_ended");

        admitted_go.notify_one();
        let answered = admitted.await.unwrap().expect_err("no connection");
        assert_eq!(answered.code(), "unavailable");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_shutdown_fences_and_joins_running_grace() {
        let mut rig = CrazeRig::start().await;
        let _conn = rig.open_seeded().await;
        rig.depart(Departure::Removed);
        rig.lanes.begin_closing();
        let at_close = rig.events.total();
        tokio::time::sleep(TEST_GRACE * 2).await;
        assert_eq!(rig.events.total(), at_close);

        let lanes = Arc::clone(&rig.lanes);
        let (tx, rx) = tokio::sync::oneshot::channel();
        std::thread::spawn(move || {
            let _ = tx.send(lanes.shutdown());
        });
        let closed = rx.await.unwrap();
        assert_eq!(
            (closed.lanes, closed.pumps_unjoined, closed.graces_unjoined),
            (1, 0, 0)
        );
        assert_eq!(rig.events.total(), at_close);
    }

    #[test]
    fn exactly_one_terminal_down_and_no_late_ready_after_finish() {
        let events: Arc<dyn LaneEvents> = Arc::new(RecordedEvents::default());
        let recorded = Arc::new(RecordedEvents::default());
        let events_rec: Arc<dyn LaneEvents> = recorded.clone();
        let core = LaneCore::new(None);
        let k = key(&shed("s1"), GHOST);
        assert!(core.finish(&events_rec, &k, "session_closed"));
        assert!(!core.finish(&events_rec, &k, DOWN_SESSION_GONE));
        assert!(!core.apply(&events_rec, &k, &LaneEvent::Ready { generation: 2 }));
        assert_eq!(recorded.downs(), ["session_closed"]);
        drop(events);
    }

    /// A terminal `Down` is never published ahead of an event whose state
    /// transition came first: a `finish` racing an `apply` whose callback is
    /// still running waits for it, so consumers see `Ready` then `Down`.
    #[test]
    fn terminal_down_is_never_emitted_ahead_of_an_inflight_event() {
        use std::sync::mpsc;

        struct GatedEvents {
            events: Mutex<Vec<LaneUiEvent>>,
            entered: Mutex<Option<mpsc::Sender<()>>>,
            release: Mutex<Option<mpsc::Receiver<()>>>,
        }
        impl LaneEvents for GatedEvents {
            fn on_lane_event(&self, event: LaneUiEvent) {
                if matches!(event.event, LaneEvent::Ready { .. }) {
                    if let Some(tx) = lock(&self.entered).take() {
                        let _ = tx.send(());
                        if let Some(rx) = lock(&self.release).take() {
                            let _ = rx.recv();
                        }
                    }
                }
                lock(&self.events).push(event);
            }
        }

        let (entered_tx, entered_rx) = mpsc::channel();
        let (release_tx, release_rx) = mpsc::channel();
        let gated = Arc::new(GatedEvents {
            events: Mutex::new(Vec::new()),
            entered: Mutex::new(Some(entered_tx)),
            release: Mutex::new(Some(release_rx)),
        });
        let events: Arc<dyn LaneEvents> = gated.clone();
        let core = LaneCore::new(None);
        let k = key(&shed("s1"), GHOST);

        let pump = {
            let (core, events, k) = (Arc::clone(&core), Arc::clone(&events), k.clone());
            std::thread::spawn(move || core.apply(&events, &k, &LaneEvent::Ready { generation: 1 }))
        };
        entered_rx.recv().expect("Ready callback entered");
        let ender = {
            let (core, events, k) = (Arc::clone(&core), Arc::clone(&events), k.clone());
            std::thread::spawn(move || core.finish(&events, &k, "session_closed"))
        };
        // Give the ender every chance to (wrongly) publish its Down first.
        std::thread::sleep(Duration::from_millis(100));
        release_tx.send(()).expect("release Ready");
        assert!(pump.join().unwrap());
        assert!(ender.join().unwrap());

        let order: Vec<&'static str> = lock(&gated.events)
            .iter()
            .map(|e| match e.event {
                LaneEvent::Ready { .. } => "ready",
                LaneEvent::Down { .. } => "down",
                _ => "other",
            })
            .collect();
        assert_eq!(order, ["ready", "down"]);
    }

    /// **Fix 7**: `begin_closing` immediately blocks new RPCs (`send`, `set`,
    /// `set_title`, `cancel`, `stop`, `answer`) on existing open lanes.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn closing_lane_rejects_new_rpcs_immediately() {
        let mut rig = CrazeRig::start().await;
        let _hub = rig.open_seeded().await;

        rig.lanes.begin_closing();

        let err = rig
            .lanes
            .send(&rig.shed, GHOST, "hello", SendMode::Queue)
            .await
            .unwrap_err();
        assert_eq!(err.code(), SHUTTING_DOWN);

        let err = rig
            .lanes
            .set(
                &rig.shed,
                GHOST,
                LaneSettingChange::Model { id: "m".into() },
            )
            .await
            .unwrap_err();
        assert_eq!(err.code(), SHUTTING_DOWN);

        let err = rig
            .lanes
            .set_title(&rig.shed, GHOST, "new title")
            .await
            .unwrap_err();
        assert_eq!(err.code(), SHUTTING_DOWN);

        let err = rig.lanes.cancel(&rig.shed, GHOST).await.unwrap_err();
        assert_eq!(err.code(), SHUTTING_DOWN);

        let err = rig.lanes.stop(&rig.shed, GHOST).await.unwrap_err();
        assert_eq!(err.code(), SHUTTING_DOWN);

        let err = rig
            .lanes
            .answer(&rig.shed, GHOST, "ask-1", LaneAnswer::Reject)
            .await
            .unwrap_err();
        assert_eq!(err.code(), SHUTTING_DOWN);
    }

    /// **Fix 5**: `LaneEvents::on_lane_event` is NEVER invoked while holding
    /// `inner` or `core.state`, so a re-entrant callback calling `messages()`
    /// never deadlocks (including on `HostRemoved`).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn lane_events_callback_can_reenter_without_deadlock() {
        use std::sync::atomic::Ordering;

        struct ReentrantEvents {
            lanes: Mutex<Option<Arc<ShedLanes>>>,
            reentered: std::sync::atomic::AtomicUsize,
        }

        impl LaneEvents for ReentrantEvents {
            fn on_lane_event(&self, ev: LaneUiEvent) {
                if let Some(lanes) = lock(&self.lanes).clone() {
                    let _ = lanes.messages(&ev.target, &ev.session_id);
                    self.reentered.fetch_add(1, Ordering::SeqCst);
                }
            }
        }

        let target = shed("s1");
        let (dial, mut conns) = ScriptedDial::new();
        let source = CrazeSource::new(dial, "shed-desktop-test");
        let _roster = source.subscribe().await.expect("subscribe");
        let mut hub = conns.recv().await.expect("roster dialled");
        hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
        hub.subscribed(
            "sub-1",
            "0a1b2c3d4e5f",
            json!([roster_row(GHOST, GSID, "/home/shed", json!({"title": "w"}))]),
        )
        .await;
        wait_for("source lists row", || source.listed(GHOST).map(|_| ())).await;

        let hosts = FakeHosts::new();
        hosts.insert(target.clone(), source, 1, vec![GHOST.to_string()]);
        let events = Arc::new(ReentrantEvents {
            lanes: Mutex::new(None),
            reentered: std::sync::atomic::AtomicUsize::new(0),
        });
        let lanes = Arc::new(ShedLanes::new(
            tokio::runtime::Handle::current(),
            Arc::clone(&events) as Arc<dyn LaneEvents>,
            hosts,
        ));
        *lock(&events.lanes) = Some(Arc::clone(&lanes));

        lanes.open(&target, GHOST).await.expect("open lane");
        lanes.craze_news(&target, 1, CrazeNews::HostRemoved { seq: 1 });
        assert!(events.reentered.load(Ordering::SeqCst) >= 1);
        *lock(&events.lanes) = None;
    }
}
