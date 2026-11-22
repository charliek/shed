//! **Shed-only roost & craze host registry** (plan 019 §3.6, plan 025, plan
//! 026, plan 032, plan 038 D2/D6).
//!
//! Tracks every running shed (`ShedId`, `roost:<server>/<shed>`) reported by
//! configured shed servers, probes each for a running `roost-session` (bounded
//! concurrency, budget, and cooldown), promotes answering sheds to live
//! [`RoostWatcher`]s, runs one attach-only craze source per running shed, and
//! owns the `roost.{probe,preview,bootstrap}` and `craze.*` operations.
//!
//! ## Security & Authorization Invariants
//!
//! 1. **Parsing a valid [`ShedId`] never authorizes SSH.** Every probe,
//!    preview, bootstrap, watch, inject, and craze op routes through
//!    `ssh_entry`, which requires the shed to be currently present
//!    in `listed_sheds` (populated by [`ShedHosts::observe_sheds`]). An unlisted
//!    shed spawns zero SSH processes and leaves no registry state behind.
//! 2. **In-memory bootstrap entitlement.** A watcher refreshes agent hooks ONLY
//!    for sheds bootstrapped during this app run (`armed`). An already-running
//!    `roost-session` never receives unsolicited `session.set_agent_hooks`
//!    writes.
//! 3. **Removal racing with bootstrap.** Removing a shed disarms its flag FIRST
//!    and removes it from `armed`; arming after a successful bootstrap takes
//!    `armed -> reg` and only sets the flag if the shed is still registered. A
//!    removed and re-registered shed never inherits entitlement.
//! 4. **Use-time identity recheck.** `shed_reach_entry` checks `identity_file`
//!    at registration, and `SshBridge`/`SshExec` re-check it via
//!    [`shed_roost::check_identity`] before spawning SSH.
//!
//! ## Lock Ordering
//!
//! * `state -> armed -> reg -> execs -> retired` (`listed_sheds` is a leaf lock,
//!   never held while acquiring any other mutex)
//! * `reg -> probes`
//! * No `std::sync::Mutex` is ever held across an `.await`.
//! * [`HostEvents`] callbacks and [`OnCrazeNews`] observers are ALWAYS invoked
//!   AFTER releasing `observe_pass` and every `ShedHosts` mutex, so `ShedHosts`
//!   and [`crate::ShedLanes`] never nest locks in either direction.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use shed_core::config::ShedConfig;
use shed_craze::CrazeSource;
use shed_lane::craze_rows::{fold_plan, FoldPlan, RoostTabRef};
use shed_lane::lane::{
    AgentSource as _, LaneCreateRequest, LaneSession, SourceEvent, SourceOffline,
};
use shed_lane::rc::RcKind;
use shed_roost::bootstrap::{
    self, BootstrapFailure, HooksResult, InstallRequest, Installed, Plan, Probe, ProbeOutcome,
    SessionState, SourceEnv, Stage,
};
use shed_roost::{
    shed_reach_entry, tab_open_command_fenced, AgentLaneStamp, BootstrapRunner, Conn, HooksRefresh,
    ReachKind as ReachFamily, RoostInventory, RoostReach, RoostSession, RoostUpdate, RoostWatcher,
    RoostWatcherOptions, ShuttingDown, SpawnGate, SshEndpoint, SshExec, TabFence, SHUTTING_DOWN,
};

use crate::id::ShedId;
use crate::shed_craze::{
    build_ssh_craze, build_ssh_reach, CrazeFailure, CrazePhase, CrazeReach, CrazeSink, CrazeState,
    CrazeTimings, Dormancy, ShedReachOptions, CRAZE_TOO_OLD,
};
use crate::shed_lanes::{CrazeNews, Departure, CRAZE_KIND};

/// Who shed says it is on roost's wire (`session.set_agent_hooks`'s `client`).
pub const CLIENT_LABEL: &str = "shed-desktop";

/// Default home directory inside every shed VM.
pub const SHED_HOME: &str = "/home/shed";

/// How long one shed's `session.identify` probe may take.
const SHED_PROBE_BUDGET: Duration = Duration::from_secs(8);

/// How many shed probes may be in flight at once.
const SHED_PROBE_CONCURRENCY: usize = 4;

/// How long a shed that did not answer is left alone before re-probing.
const SHED_PROBE_COOLDOWN: Duration = Duration::from_secs(30);

/// How long a removed shed's craze source teardown waits (best-effort bound).
const CRAZE_STOP_WAIT: Duration = Duration::from_secs(2);

/// How long an unfenced `craze.open_terminal` tab outlasts snapshots that do
/// not list it.
const UNFENCED_GRACE: Duration = Duration::from_secs(5);

static NEXT_CRAZE_GEN: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
static NEXT_REG_GEN: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);

pub(crate) fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// Event callbacks emitted by [`ShedHosts`].
///
/// Always invoked after releasing all internal mutexes.
pub trait HostEvents: Send + Sync + 'static {
    /// Emit UI-facing snapshot invalidation (`rc-changed` / `refresh`).
    fn on_hosts_changed(&self);

    /// Called when a shed's Craze source is installed with a new generation.
    fn on_craze_source_changed(&self, _shed: &ShedId, _generation: u64) {}
}

/// No-op [`HostEvents`] implementation (for tests or headless drivers).
#[derive(Debug, Default, Clone, Copy)]
pub struct NoopHostEvents;

impl HostEvents for NoopHostEvents {
    fn on_hosts_changed(&self) {}
}

/// Internal craze-news hook connecting [`ShedHosts`] to [`crate::ShedLanes`].
pub type OnCrazeNews = Arc<dyn Fn(&ShedId, u64, CrazeNews<'_>) + Send + Sync>;

/// Snapshot returned by [`ShedHosts::snapshot`] under a single lock acquisition.
#[derive(Debug, Clone, Default)]
pub struct HostSnapshot {
    pub sessions: Vec<Value>,
    pub hosts: Vec<Value>,
    pub shed_craze: Vec<Value>,
}

/// Which sheds a snapshot is filtered to.
#[derive(Debug, Clone, Copy)]
pub struct HostFilter<'a> {
    pub server: Option<&'a str>,
    pub shed: Option<&'a str>,
}

impl HostFilter<'_> {
    pub const ALL: HostFilter<'static> = HostFilter {
        server: None,
        shed: None,
    };

    fn keeps(&self, id: &ShedId) -> bool {
        self.server.is_none_or(|want| want == id.server())
            && self.shed.is_none_or(|want| want == id.shed())
    }
}

/// A synthetic roost row injected in test mode (`rc.inject_test`).
#[derive(Debug, Clone)]
pub struct InjectedRow {
    pub tab_id: i64,
    pub kind: RcKind,
    pub title: String,
    pub cwd: String,
    pub lifecycle: Option<String>,
    pub attention: bool,
}

impl InjectedRow {
    fn into_session(self, id: &ShedId) -> Result<RoostSession, String> {
        RoostSession::synthetic(
            id.as_target(),
            self.tab_id,
            &self.kind,
            self.title,
            self.cwd,
            self.lifecycle.as_deref(),
            self.attention,
        )
    }
}

/// A bootstrap refused BEFORE starting (call-level error, not a stage failure).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Refusal {
    ConsentRequired(String),
    BadTarget(String),
    AlreadyBootstrapping(String),
    ShuttingDown(String),
}

impl Refusal {
    pub fn code(&self) -> &'static str {
        match self {
            Refusal::ConsentRequired(_) => "consent_required",
            Refusal::BadTarget(_) => "bad_request",
            Refusal::AlreadyBootstrapping(_) => "already_bootstrapping",
            Refusal::ShuttingDown(_) => SHUTTING_DOWN,
        }
    }

    pub fn message(self) -> String {
        match self {
            Refusal::ConsentRequired(m)
            | Refusal::BadTarget(m)
            | Refusal::AlreadyBootstrapping(m)
            | Refusal::ShuttingDown(m) => m,
        }
    }
}

/// Summary of what [`ShedHosts::join_clients`] joined or abandoned at its bound.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct JoinedClients {
    pub watchers: usize,
    pub watchers_left: usize,
    pub probes: usize,
    pub probes_aborted: usize,
    pub probes_left: usize,
    pub cancelled: usize,
    pub one_shots_left: usize,
}

#[derive(Clone)]
pub(crate) enum CrazeReaches {
    Options,
    #[cfg(test)]
    Fixed(FixedCrazeReach),
}

#[cfg(test)]
pub(crate) type FixedCrazeReach = Arc<dyn Fn(&ShedId) -> Option<CrazeReach> + Send + Sync>;

#[cfg(test)]
pub(crate) type TestGap = (Arc<tokio::sync::Notify>, Arc<tokio::sync::Notify>);

#[cfg(test)]
pub(crate) async fn at_gap(slot: &Mutex<Option<TestGap>>) {
    let gap = lock(slot).take();
    if let Some((reached, proceed)) = gap {
        reached.notify_one();
        proceed.notified().await;
    }
}

struct CrazeHost {
    source: Option<CrazeSource>,
    ask: Option<CrazeSource>,
    wake: Arc<tokio::sync::Notify>,
    task: tokio::task::JoinHandle<()>,
    gen: u64,
    gone: Option<tokio::sync::oneshot::Receiver<()>>,
}

impl CrazeHost {
    async fn stop(mut self) -> bool {
        self.task.abort();
        self.source = None;
        self.ask = None;
        let gone = self.gone.take();
        let task = &mut self.task;
        tokio::time::timeout(CRAZE_STOP_WAIT, async move {
            let _ = task.await;
            if let Some(gone) = gone {
                let _ = gone.await;
            }
        })
        .await
        .is_ok()
    }
}

impl Drop for CrazeHost {
    fn drop(&mut self) {
        self.task.abort();
    }
}

struct Detached {
    id: ShedId,
    was_listed: bool,
    was_offered: bool,
    craze: Option<CrazeHost>,
    craze_removed: Option<(u64, u64)>,
}

struct RemovedEvents {
    craze_removed: Vec<(ShedId, u64, u64)>,
    hosts_changed: bool,
}

struct HostState {
    sessions: Vec<RoostSession>,
    reachable: bool,
    detail: Option<String>,
    down_kind: Option<ReachFamily>,
    seen: bool,
    watched: bool,
    listed: bool,
    craze: CrazeState,
    reg_gen: u64,
    opened_tabs: BTreeMap<i64, OpenedTab>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct OpenedTab {
    host_id: String,
    fence: Option<TabFence>,
    opened: Instant,
}

impl OpenedTab {
    fn retained(&self, present: bool, inventory: &RoostInventory, now: Instant) -> bool {
        if present {
            return true;
        }
        match &self.fence {
            Some(fence) if fence.same_daemon(inventory) => inventory
                .revision
                .is_some_and(|revision| revision < fence.revision),
            Some(_) => false,
            None => now.saturating_duration_since(self.opened) < UNFENCED_GRACE,
        }
    }
}

impl HostState {
    fn new(listed: bool) -> Self {
        Self {
            sessions: Vec::new(),
            reachable: false,
            detail: None,
            down_kind: None,
            seen: false,
            watched: false,
            listed,
            craze: CrazeState::default(),
            reg_gen: NEXT_REG_GEN.fetch_add(1, Ordering::Relaxed),
            opened_tabs: BTreeMap::new(),
        }
    }

    fn in_shed_craze(&self) -> bool {
        !self.listed && self.craze.proven_unattached()
    }

    fn fold_plan(&self, created: &[LaneSession]) -> FoldPlan {
        let mut tabs: Vec<RoostTabRef> = self
            .sessions
            .iter()
            .map(|s| RoostTabRef {
                tab_id: s.tab_id,
                craze_owner: (s.agent_kind() == RcKind::Craze).then(|| {
                    s.ownership
                        .as_ref()
                        .map(|o| o.session_id.clone())
                        .unwrap_or_default()
                }),
            })
            .collect();
        let mut rows: BTreeMap<&str, &LaneSession> = self
            .craze
            .rows
            .values()
            .map(|row| (row.id.as_str(), row))
            .collect();
        for row in created {
            rows.entry(row.id.as_str()).or_insert(row);
        }
        let mut claims: BTreeMap<&str, usize> = BTreeMap::new();
        for row in rows.values() {
            if let Some(psid) = row.provider_session_id.as_deref() {
                *claims.entry(psid).or_default() += 1;
            }
        }
        let live = self.craze.live();
        let hub: Option<Vec<LaneSession>> =
            live.then(|| rows.values().map(|row| (*row).clone()).collect());
        let mut direct: Vec<(i64, &str)> = Vec::new();
        for (&tab_id, opened) in &self.opened_tabs {
            if self.sessions.iter().any(|s| s.tab_id == tab_id) {
                continue;
            }
            let psid = rows
                .get(opened.host_id.as_str())
                .and_then(|row| row.provider_session_id.as_deref());
            match psid {
                Some(psid) if live && claims.get(psid) == Some(&1) => tabs.push(RoostTabRef {
                    tab_id,
                    craze_owner: Some(psid.to_string()),
                }),
                _ => direct.push((tab_id, &opened.host_id)),
            }
        }
        let mut plan = fold_plan(&tabs, hub.as_deref());
        for (tab_id, host_id) in direct {
            if !rows.contains_key(host_id) {
                if live {
                    plan.hidden.insert(tab_id);
                }
                continue;
            }
            match plan.tab_of(host_id) {
                None => {
                    plan.absorbed.insert(tab_id, host_id.to_string());
                }
                Some(current) if current < tab_id => {
                    plan.absorbed.remove(&current);
                    plan.hidden.insert(current);
                    plan.absorbed.insert(tab_id, host_id.to_string());
                }
                Some(_) => {
                    plan.hidden.insert(tab_id);
                }
            }
        }
        plan
    }
}

#[derive(Default)]
struct Registry {
    ids: Vec<ShedId>,
    reaches: BTreeMap<ShedId, Arc<dyn RoostReach>>,
    watchers: BTreeMap<ShedId, RoostWatcher>,
    crazes: BTreeMap<ShedId, CrazeHost>,
    closing: bool,
}

impl Registry {
    fn install_watcher(
        &mut self,
        id: &ShedId,
        reach: &Arc<dyn RoostReach>,
        watcher: RoostWatcher,
    ) -> bool {
        if self.closing || self.watchers.contains_key(id) {
            return false;
        }
        let Some(current) = self.reaches.get(id) else {
            return false;
        };
        if !Arc::ptr_eq(current, reach) {
            return false;
        }
        self.watchers.insert(id.clone(), watcher);
        true
    }
}

type ArmedFlags = BTreeMap<ShedId, Arc<AtomicBool>>;

fn install_and_flag(
    state: &Mutex<BTreeMap<ShedId, HostState>>,
    reg: &Mutex<Registry>,
    id: &ShedId,
    expected_reg_gen: u64,
    reach: &Arc<dyn RoostReach>,
    watcher: RoostWatcher,
) -> bool {
    let mut state = lock(state);
    if !state.get(id).is_some_and(|m| m.reg_gen == expected_reg_gen) {
        return false;
    }
    let mut reg = lock(reg);
    if !reg.install_watcher(id, reach, watcher) {
        return false;
    }
    if let Some(m) = state.get_mut(id) {
        m.watched = true;
    }
    true
}

#[derive(Default)]
struct Retired {
    reaches: Vec<Weak<dyn RoostReach>>,
    /// Strong references to detached `SshExec` runners so any running SSH
    /// master from a removed shed is still shut down with PID-verified
    /// fallback during orderly shutdown.
    execs: Vec<Arc<SshExec>>,
}

impl Retired {
    fn prune(&mut self) {
        self.reaches.retain(|r| r.strong_count() > 0);
    }
}

#[derive(Default)]
pub(crate) struct Admitted {
    count: std::sync::atomic::AtomicUsize,
    idle: tokio::sync::Notify,
    cancelled: AtomicBool,
    cancel: tokio::sync::Notify,
}

impl Admitted {
    fn admit(self: &Arc<Self>) -> AdmittedGuard {
        self.count.fetch_add(1, Ordering::SeqCst);
        AdmittedGuard(Arc::clone(self))
    }

    fn in_flight(&self) -> usize {
        self.count.load(Ordering::SeqCst)
    }

    async fn wait_idle(&self, deadline: tokio::time::Instant) -> bool {
        loop {
            let notified = self.idle.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.count.load(Ordering::SeqCst) == 0 {
                return true;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return self.count.load(Ordering::SeqCst) == 0;
            }
        }
    }

    fn cancel_all(&self) {
        self.cancelled.store(true, Ordering::SeqCst);
        self.cancel.notify_waiters();
    }

    async fn cancelled(&self) {
        loop {
            let notified = self.cancel.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.cancelled.load(Ordering::SeqCst) {
                return;
            }
            notified.await;
        }
    }
}

pub(crate) struct AdmittedGuard(Arc<Admitted>);

impl AdmittedGuard {
    async fn run<T>(
        &self,
        work: impl std::future::Future<Output = T>,
        cancelled: impl FnOnce() -> T,
    ) -> T {
        tokio::select! {
            biased;
            () = self.0.cancelled() => cancelled(),
            out = work => out,
        }
    }
}

impl Drop for AdmittedGuard {
    fn drop(&mut self) {
        if self.0.count.fetch_sub(1, Ordering::SeqCst) == 1 {
            self.0.idle.notify_waiters();
        }
    }
}

struct BootstrapGate {
    set: Arc<Mutex<BTreeSet<ShedId>>>,
    id: ShedId,
}

impl Drop for BootstrapGate {
    fn drop(&mut self) {
        lock(&self.set).remove(&self.id);
    }
}

struct AnsweredRow {
    row: Value,
    ended: bool,
    superseded: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum CrazeNeed {
    CreateOptions,
    Create,
    Saved,
    Reopen,
}

impl CrazeNeed {
    fn unsupported(self) -> CrazeFailure {
        match self {
            CrazeNeed::CreateOptions | CrazeNeed::Create => {
                CrazeFailure::new("too_old", CRAZE_TOO_OLD)
            }
            CrazeNeed::Saved | CrazeNeed::Reopen => CrazeFailure::new(
                "unavailable",
                shed_craze::errors::reopen_unsupported_by("", None),
            ),
        }
    }
}

type CrazeAsk = (CrazeSource, u64, Arc<tokio::sync::Notify>, AdmittedGuard);

/// Atomic `(reg_gen, reach, exec)` bundle captured from a single registration.
type RegisteredTransports = (u64, Arc<dyn RoostReach>, Arc<SshExec>);

/// The shed-only roost & craze host registry.
pub struct ShedHosts {
    state: Arc<Mutex<BTreeMap<ShedId, HostState>>>,
    reg: Arc<Mutex<Registry>>,
    handle: tokio::runtime::Handle,
    config: ShedConfig,
    reach_options: ShedReachOptions,
    jail_fs_root: bool,
    execs: Mutex<BTreeMap<ShedId, Arc<SshExec>>>,
    probing: Arc<Mutex<BTreeSet<ShedId>>>,
    listed_sheds: Arc<Mutex<BTreeSet<ShedId>>>,
    bootstrapping: Arc<Mutex<BTreeSet<ShedId>>>,
    armed: Arc<Mutex<ArmedFlags>>,
    probed: Arc<Mutex<BTreeMap<ShedId, Instant>>>,
    probe_slots: Arc<tokio::sync::Semaphore>,
    events: Arc<dyn HostEvents>,
    on_craze: Arc<Mutex<Option<OnCrazeNews>>>,
    craze_reaches: CrazeReaches,
    craze_timings: CrazeTimings,
    observe_seq: AtomicU64,
    last_observe_seq: AtomicU64,
    observe_pass: tokio::sync::Mutex<()>,
    probes: Mutex<tokio::task::JoinSet<()>>,
    admitted: Arc<Admitted>,
    retired: Mutex<Retired>,
    #[cfg(test)]
    pub(crate) remove_gap: Mutex<Option<TestGap>>,
    #[cfg(test)]
    pub(crate) observe_gap: Mutex<Option<TestGap>>,
    #[cfg(test)]
    pub(crate) create_gap: Mutex<Option<TestGap>>,
}

impl ShedHosts {
    pub fn start(
        handle: &tokio::runtime::Handle,
        config: &ShedConfig,
        reach_options: ShedReachOptions,
        jail_fs_root: bool,
        events: Arc<dyn HostEvents>,
    ) -> ShedHosts {
        Self::start_with(
            handle,
            config,
            reach_options,
            jail_fs_root,
            events,
            CrazeReaches::Options,
            CrazeTimings::default(),
        )
    }

    pub(crate) fn start_with(
        handle: &tokio::runtime::Handle,
        config: &ShedConfig,
        reach_options: ShedReachOptions,
        jail_fs_root: bool,
        events: Arc<dyn HostEvents>,
        craze_reaches: CrazeReaches,
        craze_timings: CrazeTimings,
    ) -> ShedHosts {
        ShedHosts {
            state: Arc::new(Mutex::new(BTreeMap::new())),
            reg: Arc::new(Mutex::new(Registry::default())),
            handle: handle.clone(),
            config: config.clone(),
            reach_options,
            jail_fs_root,
            execs: Mutex::new(BTreeMap::new()),
            probing: Arc::new(Mutex::new(BTreeSet::new())),
            listed_sheds: Arc::new(Mutex::new(BTreeSet::new())),
            bootstrapping: Arc::new(Mutex::new(BTreeSet::new())),
            armed: Arc::new(Mutex::new(ArmedFlags::new())),
            probed: Arc::new(Mutex::new(BTreeMap::new())),
            probe_slots: Arc::new(tokio::sync::Semaphore::new(SHED_PROBE_CONCURRENCY)),
            events,
            on_craze: Arc::new(Mutex::new(None)),
            craze_reaches,
            craze_timings,
            observe_seq: AtomicU64::new(1),
            last_observe_seq: AtomicU64::new(0),
            observe_pass: tokio::sync::Mutex::new(()),
            probes: Mutex::new(tokio::task::JoinSet::new()),
            admitted: Arc::new(Admitted::default()),
            retired: Mutex::new(Retired::default()),
            #[cfg(test)]
            remove_gap: Mutex::new(None),
            #[cfg(test)]
            observe_gap: Mutex::new(None),
            #[cfg(test)]
            create_gap: Mutex::new(None),
        }
    }

    /// Install the craze-news observer (typically [`crate::ShedLanes::craze_news`]).
    pub fn set_craze_observer(&self, observer: OnCrazeNews) {
        *lock(&self.on_craze) = Some(observer);
    }

    /// Configured server names from the config snapshot.
    pub fn servers(&self) -> Vec<String> {
        self.config.servers.iter().map(|s| s.name.clone()).collect()
    }

    /// The spawn gate this registry uses, if configured.
    pub fn spawn_gate(&self) -> Option<SpawnGate> {
        self.reach_options.spawn_gate.clone()
    }

    /// Read sessions, host status rows (`hosts`), and `shed_craze` offers under
    /// ONE lock acquisition so rows and health never contradict each other.
    pub fn snapshot(&self, filter: HostFilter<'_>) -> HostSnapshot {
        let guard = lock(&self.state);
        HostSnapshot {
            sessions: self.sessions_locked(&guard, filter),
            hosts: self.status_locked(&guard, filter),
            shed_craze: self.shed_craze_locked(&guard, filter),
        }
    }

    /// Per-host health rows for all listed sheds.
    pub fn status(&self) -> Vec<Value> {
        let guard = lock(&self.state);
        self.status_locked(&guard, HostFilter::ALL)
    }

    fn status_locked(
        &self,
        guard: &BTreeMap<ShedId, HostState>,
        filter: HostFilter<'_>,
    ) -> Vec<Value> {
        let reg = lock(&self.reg);
        reg.ids
            .iter()
            .filter(|id| filter.keeps(id))
            .filter_map(|id| {
                let m = guard.get(id);
                if m.is_some_and(|m| !m.listed) {
                    return None;
                }
                let target = id.as_target();
                Some(json!({
                    "name": target,
                    "target": target,
                    "label": id.shed(),
                    "origin": target,
                    "kind": "shed",
                    "server": id.server(),
                    "shed": id.shed(),
                    "reachable": m.is_some_and(|m| m.reachable),
                    "connected_once": m.is_some_and(|m| m.seen),
                    "roost_watched": m.is_some_and(|m| m.watched),
                    "sessions": m.map_or(0, |m| {
                        let created = created_rows(&reg, id, m);
                        let plan = m.fold_plan(&created);
                        m.sessions.iter().filter(|s| !plan.folds(s.tab_id)).count()
                            + m.craze.rows.len()
                            + created.len()
                    }),
                    "detail": m.and_then(|m| m.detail.clone()),
                    "down_kind": m.and_then(|m| m.down_kind).map(|k| k.as_str()),
                    "craze": m.map_or_else(|| CrazeState::default().status(), |m| m.craze.status()),
                }))
            })
            .collect()
    }

    fn shed_craze_locked(
        &self,
        guard: &BTreeMap<ShedId, HostState>,
        filter: HostFilter<'_>,
    ) -> Vec<Value> {
        let reg = lock(&self.reg);
        reg.ids
            .iter()
            .filter(|id| filter.keeps(id))
            .filter_map(|id| {
                let m = guard.get(id).filter(|m| m.in_shed_craze())?;
                let target = id.as_target();
                Some(json!({
                    "name": target,
                    "target": target,
                    "label": id.shed(),
                    "origin": target,
                    "kind": "shed",
                    "server": id.server(),
                    "shed": id.shed(),
                    "craze": m.craze.status(),
                }))
            })
            .collect()
    }

    fn sessions_locked(
        &self,
        guard: &BTreeMap<ShedId, HostState>,
        filter: HostFilter<'_>,
    ) -> Vec<Value> {
        let mut out = Vec::new();
        let reg = lock(&self.reg);
        for (id, m) in guard.iter() {
            if !m.listed || !filter.keeps(id) {
                continue;
            }
            let created = created_rows(&reg, id, m);
            let plan = m.fold_plan(&created);
            for session in &m.sessions {
                if plan.folds(session.tab_id) {
                    continue;
                }
                out.push(host_row(id, session, !m.reachable));
            }
            let stale = !m.craze.live();
            for row in m.craze.rows.values().chain(created.iter()) {
                out.push(craze_row(id, row, stale, plan.tab_of(&row.id)));
            }
        }
        out
    }

    // ---- craze lane lookup ----

    /// The craze lanes `id` currently exposes (roster + created rows).
    pub fn craze_lanes(&self, id: &ShedId) -> BTreeMap<String, AgentLaneStamp> {
        let guard = lock(&self.state);
        guard
            .get(id)
            .map(|m| {
                let created = created_rows(&lock(&self.reg), id, m);
                m.craze
                    .rows
                    .keys()
                    .chain(created.iter().map(|row| &row.id))
                    .map(|host_id| {
                        (
                            host_id.clone(),
                            AgentLaneStamp {
                                kind: CRAZE_KIND.to_string(),
                                session_id: host_id.clone(),
                                server_url: String::new(),
                            },
                        )
                    })
                    .collect()
            })
            .unwrap_or_default()
    }

    /// `id`'s current craze source and the generation it was started under.
    pub fn craze_source(&self, id: &ShedId) -> Result<(CrazeSource, u64), String> {
        lock(&self.reg)
            .crazes
            .get(id)
            .and_then(|c| c.source.clone().map(|source| (source, c.gen)))
            .ok_or_else(|| format!("{id} has no craze source"))
    }

    // ---- test injection (`rc.inject_test`, shed-only) ----

    /// Inject a synthetic roost session row into a listed shed (test-only).
    pub fn inject_test(&self, id: &ShedId, row: InjectedRow) -> Result<(), String> {
        let kind = row.kind.as_str().to_string();
        let session = row.into_session(id)?;
        if !session.is_agent_owned() {
            return Err(format!(
                "roost has no adapter for kind {kind:?}, so an injected {kind} tab \
                 is unowned — and an unowned tab is not a session row"
            ));
        }
        self.ensure_registered_and_notify(id)?;
        {
            let mut guard = lock(&self.state);
            let Some(state) = guard.get_mut(id) else {
                return Err(format!("{id} was removed while the row was injected"));
            };
            state.sessions.retain(|s| s.tab_id != session.tab_id);
            state.sessions.push(session);
            state.listed = true;
            state.reachable = true;
            state.seen = true;
            state.detail = None;
            state.down_kind = None;
        }
        self.events.on_hosts_changed();
        Ok(())
    }

    // ---- craze explicit user ops ----

    fn craze_ask(&self, id: &ShedId, need: CrazeNeed) -> Result<CrazeAsk, CrazeFailure> {
        // Authorization check: unlisted shed fails before touching anything.
        if !lock(&self.listed_sheds).contains(id) {
            return Err(CrazeFailure::bad_request(unlisted_shed(id)));
        }
        let state = lock(&self.state);
        let reg = lock(&self.reg);
        if reg.closing {
            return Err(CrazeFailure::new(SHUTTING_DOWN, ShuttingDown.to_string()));
        }
        if !reg.ids.contains(id) {
            return Err(CrazeFailure::bad_request(unknown_host(id, &reg.ids)));
        }
        let no_craze = || CrazeFailure::new("no_craze", format!("{id} has no craze source"));
        let craze = reg.crazes.get(id).ok_or_else(no_craze)?;
        let ask = craze.ask.clone().ok_or_else(no_craze)?;
        if let Some(m) = state.get(id).filter(|m| m.craze.gen == craze.gen) {
            match (m.craze.phase, &m.craze.offline) {
                (Some(CrazePhase::Offline), Some((SourceOffline::NotInstalled, why))) => {
                    return Err(CrazeFailure::new(
                        "not_installed",
                        format!("craze is not installed on {id}: {why}"),
                    ))
                }
                (Some(CrazePhase::Offline), Some((SourceOffline::TooOld, _))) => {
                    return Err(CrazeFailure::new("too_old", CRAZE_TOO_OLD))
                }
                (Some(CrazePhase::Live), _) => {
                    let caps = m.craze.caps.as_ref();
                    let has = match need {
                        CrazeNeed::CreateOptions => caps.is_some_and(|c| c.create_options),
                        CrazeNeed::Create => caps.is_some_and(|c| c.create),
                        CrazeNeed::Saved | CrazeNeed::Reopen => caps.is_some_and(|c| c.reopen),
                    };
                    if !has {
                        return Err(need.unsupported());
                    }
                }
                _ => {}
            }
        }
        Ok((
            ask,
            craze.gen,
            Arc::clone(&craze.wake),
            self.admitted.admit(),
        ))
    }

    /// `craze.create_options {target}`.
    pub async fn craze_create_options(&self, id: &ShedId) -> Result<Value, CrazeFailure> {
        let (ask, gen, wake, admitted) = self.craze_ask(id, CrazeNeed::CreateOptions)?;
        let Some(answer) = admitted
            .run(async { Some(ask.create_options().await) }, || None)
            .await
        else {
            return Err(CrazeFailure::new(SHUTTING_DOWN, cancelled()));
        };
        wake.notify_one();
        let options = answer.map_err(CrazeFailure::lane)?;
        Ok(json!({
            "target": id.as_target(),
            "options": options,
            "source_gen": gen,
        }))
    }

    /// `craze.create {target, cwd, provider?, prompt?, title?, request_id?, held?}`.
    pub async fn craze_create(
        &self,
        id: &ShedId,
        request: LaneCreateRequest,
        held: bool,
    ) -> Result<Value, CrazeFailure> {
        let (ask, gen, wake, admitted) = match self.craze_ask(id, CrazeNeed::Create) {
            Ok(asked) => asked,
            Err(f) if held => {
                return Err(CrazeFailure::new(
                    "outcome_unknown",
                    shed_craze::errors::outcome_unknown(&format!(
                        "the retry did not reach craze on {id} ({}: {}); it may have started a session — try again once craze is back, with the same request id",
                        f.code, f.message
                    ))
                    .to_string(),
                ))
            }
            Err(f) => return Err(f),
        };
        let request_id = request.request_id.clone();
        let creating = async {
            Some(if held {
                ask.create_held(request).await
            } else {
                ask.create(request).await
            })
        };
        let Some(answer) = admitted.run(creating, || None).await else {
            return Err(CrazeFailure::new(
                "outcome_unknown",
                shed_craze::errors::outcome_unknown(&format!(
                    "the app quit while this create was in flight on {id}; it may have started a session"
                ))
                .to_string(),
            ));
        };
        wake.notify_one();
        let created = answer.map_err(CrazeFailure::lane)?;
        let host_id = created.session.id.clone();
        #[cfg(test)]
        at_gap(&self.create_gap).await;
        let answered = self.list_answered_row(id, gen, &ask, &created.session);
        self.events.on_hosts_changed();
        Ok(json!({
            "session": answered.row,
            "host_id": host_id,
            "ended": answered.ended,
            "prompt": created.prompt.as_str(),
            "prompt_error": created.prompt_error,
            "request_id": request_id,
        }))
    }

    /// `craze.saved {target}`.
    pub async fn craze_saved(&self, id: &ShedId) -> Result<Value, CrazeFailure> {
        let (ask, gen, wake, admitted) = self.craze_ask(id, CrazeNeed::Saved)?;
        let Some(answer) = admitted
            .run(async { Some(ask.saved().await) }, || None)
            .await
        else {
            return Err(CrazeFailure::new(SHUTTING_DOWN, cancelled()));
        };
        wake.notify_one();
        let saved = answer.map_err(CrazeFailure::lane)?;
        Ok(json!({
            "target": id.as_target(),
            "saved": saved,
            "home": SHED_HOME,
            "source_gen": gen,
        }))
    }

    /// `craze.reopen {target, id}`.
    pub async fn craze_reopen(&self, id: &ShedId, saved_id: &str) -> Result<Value, CrazeFailure> {
        let (ask, gen, wake, admitted) = self.craze_ask(id, CrazeNeed::Reopen)?;
        let Some(answer) = admitted
            .run(async { Some(ask.reopen(saved_id).await) }, || None)
            .await
        else {
            return Err(CrazeFailure::new(
                SHUTTING_DOWN,
                format!(
                    "the app quit while this reopen was in flight on {id}; the session may be running — resuming it again joins it"
                ),
            ));
        };
        wake.notify_one();
        let reopened = answer.map_err(CrazeFailure::reopen)?;
        let host_id = reopened.session.id.clone();
        let answered = self.list_answered_row(id, gen, &ask, &reopened.session);
        self.events.on_hosts_changed();
        Ok(json!({
            "session": answered.row,
            "host_id": host_id,
            "ended": answered.ended,
            "superseded": answered.superseded,
            "joined": reopened.joined,
            "id": saved_id,
            "source_gen": gen,
        }))
    }

    /// `craze.open_terminal {target, session_id}` — opens a roost tab running
    /// `craze attach <session_id>` via `shed_roost::tab_open_command_fenced`.
    pub async fn craze_open_terminal(
        &self,
        id: &ShedId,
        session_id: &str,
    ) -> Result<Value, CrazeFailure> {
        let admitted = self
            .admit()
            .map_err(|e| CrazeFailure::new(SHUTTING_DOWN, e))?;
        admitted
            .run(self.craze_open_terminal_admitted(id, session_id), || {
                Err(CrazeFailure::new(SHUTTING_DOWN, cancelled()))
            })
            .await
    }

    async fn craze_open_terminal_admitted(
        &self,
        id: &ShedId,
        session_id: &str,
    ) -> Result<Value, CrazeFailure> {
        if !lock(&self.listed_sheds).contains(id) {
            return Err(CrazeFailure::bad_request(unlisted_shed(id)));
        }
        let argv = shed_lane::craze::attach_argv(session_id)
            .map_err(|e| CrazeFailure::bad_request(e.to_string()))?;
        // Capture `(cwd, reg_gen, reach)` atomically from the same registration
        // incarnation (`state` -> `reg`).
        let (cwd, reg_gen, reach) = {
            let guard = lock(&self.state);
            let reg = lock(&self.reg);
            if reg.closing {
                return Err(CrazeFailure::new(SHUTTING_DOWN, ShuttingDown.to_string()));
            }
            if !lock(&self.listed_sheds).contains(id) {
                return Err(CrazeFailure::bad_request(unlisted_shed(id)));
            }
            let row = guard.get(id).and_then(|m| {
                let held = m.craze.rows.get(session_id).cloned();
                let row = held.or_else(|| {
                    created_rows(&reg, id, m)
                        .into_iter()
                        .find(|row| row.id == session_id)
                });
                row.map(|row| (row.cwd, m.reg_gen))
            });
            let (cwd, reg_gen) = match row {
                Some(pair) => pair,
                None => {
                    return Err(CrazeFailure::new(
                        "unknown_session",
                        format!("{id} lists no craze session {session_id:?}"),
                    ))
                }
            };
            let reach = match reg.reaches.get(id) {
                Some(reach) => Arc::clone(reach),
                None => {
                    return Err(CrazeFailure::new(
                        "action_failed",
                        format!("{id} has no usable transport (its reach could not be built)"),
                    ))
                }
            };
            (cwd, reg_gen, reach)
        };
        if !self.is_same_listed_incarnation(id, reg_gen) {
            return Err(CrazeFailure::bad_request(unlisted_shed(id)));
        }
        let (tab, fence) = tab_open_command_fenced(reach.as_ref(), argv.clone(), &cwd)
            .await
            .map_err(|e| CrazeFailure::new("action_failed", e))?;
        let (same_incarnation, wake) = {
            let mut guard = lock(&self.state);
            let reg = lock(&self.reg);
            if let Some(m) = guard.get_mut(id).filter(|m| m.reg_gen == reg_gen) {
                m.opened_tabs.insert(
                    tab.id,
                    OpenedTab {
                        host_id: session_id.to_string(),
                        fence,
                        opened: Instant::now(),
                    },
                );
                (true, reg.crazes.get(id).map(|c| Arc::clone(&c.wake)))
            } else {
                (false, None)
            }
        };
        if same_incarnation {
            self.watch(id, reg_gen);
            if let Some(wake) = wake {
                wake.notify_one();
            }
            self.events.on_hosts_changed();
        }
        Ok(json!({
            "origin": id.as_target(),
            "target": id.as_target(),
            "session_id": session_id,
            "tab_id": tab.id.to_string(),
            "cwd": tab.cwd,
            "argv": argv,
        }))
    }

    fn list_answered_row(
        &self,
        id: &ShedId,
        gen: u64,
        ask: &CrazeSource,
        answered: &LaneSession,
    ) -> AnsweredRow {
        let host_id = &answered.id;
        let (listing, returned) = {
            let mut guard = lock(&self.state);
            let roster_row = ask.roster_row(host_id);
            let ended = ask.listed(host_id).is_none();
            let shown = roster_row.clone().unwrap_or_else(|| answered.clone());
            match guard.get_mut(id).filter(|m| m.craze.gen == gen) {
                Some(m) => {
                    if let Some(listed) = roster_row {
                        m.craze.listed_now(listed);
                    }
                    m.listed = true;
                    let returned = (!ended).then(|| m.craze.answer_listed());
                    let created_now = created_rows(&lock(&self.reg), id, m);
                    let tab = m.fold_plan(&created_now).tab_of(host_id);
                    let row = craze_row(id, &shown, !m.craze.live() || ended, tab);
                    (
                        AnsweredRow {
                            row,
                            ended,
                            superseded: false,
                        },
                        returned,
                    )
                }
                None => (
                    AnsweredRow {
                        row: craze_row(id, &shown, true, None),
                        ended,
                        superseded: true,
                    },
                    None,
                ),
            }
        };
        if let Some(seq) = returned {
            publish_craze_returned(&self.on_craze, id, gen, seq, std::slice::from_ref(host_id));
        }
        listing
    }

    // ---- shed discovery & probe-then-watch ----

    /// Stamp a monotonic observation sequence number BEFORE querying a backend
    /// (`backend.refresh().await` or `backend.rc_targets(...).await`), then pass
    /// it to [`Self::observe_sheds_at`] so a delayed older response cannot
    /// reauthorize a shed removed by a newer observation.
    pub fn next_observe_seq(&self) -> u64 {
        self.observe_seq.fetch_add(1, Ordering::SeqCst)
    }

    /// Reconcile running sheds against the latest server listing under a fresh
    /// sequence number.
    ///
    /// `authoritative` names the servers that answered in full; sheds on those
    /// servers that are absent from `running` are removed. Pass `&[]` for
    /// add-only filtered observations.
    pub async fn observe_sheds(&self, running: &[ShedId], authoritative: &[String]) {
        let seq = self.next_observe_seq();
        self.observe_sheds_at(seq, running, authoritative).await;
    }

    /// Reconcile running sheds stamped with `seq` (from [`Self::next_observe_seq`]).
    /// Observations older than the last applied sequence are ignored.
    pub async fn observe_sheds_at(&self, seq: u64, running: &[ShedId], authoritative: &[String]) {
        let Ok(admitted) = self.admit() else {
            return;
        };
        admitted
            .run(
                self.observe_sheds_admitted(seq, running, authoritative),
                || (),
            )
            .await;
    }

    async fn observe_sheds_admitted(&self, seq: u64, running: &[ShedId], authoritative: &[String]) {
        let (removed_events, craze_started) = {
            let _pass = self.observe_pass.lock().await;
            if lock(&self.reg).closing {
                return;
            }
            if seq < self.last_observe_seq.load(Ordering::SeqCst) {
                return;
            }
            // Only authoritative observations advance the high-water mark: a
            // filtered (add-only) `rc.list` finishing ahead of an in-flight
            // full refresh must never suppress that full refresh's removal of
            // stopped sheds.
            if !authoritative.is_empty() {
                self.last_observe_seq.store(seq, Ordering::SeqCst);
            }

            let answered: BTreeSet<&str> = authoritative.iter().map(String::as_str).collect();
            let live: BTreeSet<ShedId> = running.iter().cloned().collect();

            // Update the authorization set FIRST before any registration or probe.
            {
                let mut listed = lock(&self.listed_sheds);
                listed.retain(|id| !answered.contains(id.server()));
                listed.extend(live.iter().cloned());
            }

            let (known, watched): (Vec<ShedId>, BTreeSet<ShedId>) = {
                let reg = lock(&self.reg);
                (reg.ids.clone(), reg.watchers.keys().cloned().collect())
            };
            #[cfg(test)]
            at_gap(&self.observe_gap).await;

            let stopped: Vec<ShedId> = known
                .into_iter()
                .filter(|id| answered.contains(id.server()) && !live.contains(id))
                .collect();
            if lock(&self.reg).closing {
                return;
            }
            let removed_events = self.remove(&stopped).await;

            let mut craze_started = Vec::new();
            for id in live {
                if watched.contains(&id) {
                    continue;
                }
                if let Some(started) = self.probe_shed_in_background(&id) {
                    craze_started.push(started);
                }
            }
            (removed_events, craze_started)
        };
        // Dispatch every callback AFTER releasing `observe_pass` and all mutexes.
        for (id, gen, news_seq) in removed_events.craze_removed {
            publish_craze(
                &self.on_craze,
                &id,
                gen,
                CrazeNews::HostRemoved { seq: news_seq },
            );
        }
        for (id, gen) in craze_started {
            self.events.on_craze_source_changed(&id, gen);
        }
        if removed_events.hosts_changed {
            self.events.on_hosts_changed();
        }
    }

    fn probe_shed_in_background(&self, id: &ShedId) -> Option<(ShedId, u64)> {
        {
            let mut probed = lock(&self.probed);
            let now = Instant::now();
            if probed
                .get(id)
                .is_some_and(|last| now.duration_since(*last) < SHED_PROBE_COOLDOWN)
            {
                return None;
            }
            probed.insert(id.clone(), now);
        }
        {
            let mut probing = lock(&self.probing);
            if !probing.insert(id.clone()) {
                return None;
            }
        }
        let craze_started = match self.ensure_registered(id) {
            Ok(started) => started,
            Err(_) => {
                lock(&self.probing).remove(id);
                return None;
            }
        };
        let Ok((reg_gen, reach)) = self.registered_reach(id) else {
            lock(&self.probing).remove(id);
            return craze_started;
        };
        let probing = Arc::clone(&self.probing);
        let slots = Arc::clone(&self.probe_slots);
        let hosts = self.watch_handle();
        let task = {
            let probing = Arc::clone(&probing);
            let id = id.clone();
            async move {
                let answered = {
                    let _permit = slots.acquire().await;
                    // Recheck authorization & incarnation AFTER acquiring the
                    // semaphore permit so a shed removed while queued spawns
                    // zero SSH.
                    if !hosts.still_registered(&id, reg_gen) {
                        false
                    } else {
                        session_answers(reach.as_ref()).await
                    }
                };
                if answered && hosts.still_registered(&id, reg_gen) {
                    hosts.watch(&id, reg_gen);
                }
                lock(&probing).remove(&id);
            }
        };
        let reg = lock(&self.reg);
        if reg.closing {
            drop(reg);
            lock(&self.probing).remove(id);
            return craze_started;
        }
        let mut probes = lock(&self.probes);
        while probes.try_join_next().is_some() {}
        probes.spawn_on(task, &self.handle);
        craze_started
    }

    fn ensure_registered_and_notify(&self, id: &ShedId) -> Result<(), String> {
        let started = self.ensure_registered(id)?;
        if let Some((shed_id, gen)) = started {
            self.events.on_craze_source_changed(&shed_id, gen);
        }
        Ok(())
    }

    fn ensure_registered(&self, id: &ShedId) -> Result<Option<(ShedId, u64)>, String> {
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        {
            let reg = lock(&self.reg);
            if reg.closing {
                return Err(ShuttingDown.to_string());
            }
            if reg.reaches.contains_key(id) {
                return Ok(None);
            }
        }
        let entry = self.ssh_entry(id)?;
        let built = build_ssh_reach(&entry, &self.reach_options)?;
        let lost = {
            let mut state = lock(&self.state);
            let mut reg = lock(&self.reg);
            if reg.closing {
                return Err(ShuttingDown.to_string());
            }
            if !lock(&self.listed_sheds).contains(id) {
                return Err(unlisted_shed(id));
            }
            if reg.reaches.contains_key(id) {
                Some(built)
            } else {
                if !reg.ids.contains(id) {
                    reg.ids.push(id.clone());
                }
                reg.reaches.insert(id.clone(), built);
                state
                    .entry(id.clone())
                    .or_insert_with(|| HostState::new(false));
                None
            }
        };
        if lost.is_some() {
            return Ok(None);
        }
        Ok(self.start_craze(id))
    }

    fn watch(&self, id: &ShedId, expected_reg_gen: u64) {
        self.watch_handle().watch(id, expected_reg_gen);
    }

    fn craze_reach(&self, id: &ShedId) -> (Option<CrazeReach>, Option<Arc<SshExec>>) {
        match &self.craze_reaches {
            #[cfg(test)]
            CrazeReaches::Fixed(reach) => (reach(id), None),
            CrazeReaches::Options => {
                if !self.reach_options.allows_ssh() {
                    return (None, None);
                }
                let Ok(exec) = self.exec_for(id) else {
                    return (None, None);
                };
                (
                    build_ssh_craze(Arc::clone(&exec), &self.reach_options),
                    Some(exec),
                )
            }
        }
    }

    fn start_craze(&self, id: &ShedId) -> Option<(ShedId, u64)> {
        let reg_gen = {
            let state = lock(&self.state);
            let reg = lock(&self.reg);
            if reg.closing || reg.crazes.contains_key(id) || !reg.ids.contains(id) {
                return None;
            }
            state.get(id)?.reg_gen
        };
        let (reach, exec) = self.craze_reach(id);
        let reach = reach?;
        let (dial, gone) = crate::shed_craze::tracked(Arc::clone(&reach.dial));
        let source = CrazeSource::new(Arc::clone(&dial), CLIENT_LABEL);
        let events_for_created = Arc::clone(&self.events);
        source.on_created_gone(move || events_for_created.on_hosts_changed());
        let ask = match &reach.ensure {
            Some(ensure) => source.dialling(crate::shed_craze::tethered(Arc::clone(ensure), &dial)),
            None => source.clone(),
        };
        drop(dial);
        let wake = Arc::new(tokio::sync::Notify::new());
        let gen = NEXT_CRAZE_GEN.fetch_add(1, Ordering::Relaxed);
        let installed = {
            let mut state = lock(&self.state);
            let mut reg = lock(&self.reg);
            match state.get_mut(id) {
                Some(m)
                    if !reg.closing
                        && m.reg_gen == reg_gen
                        && reg.ids.contains(id)
                        && !reg.crazes.contains_key(id) =>
                {
                    m.craze = CrazeState::new(gen);
                    // Hold `Weak` handles so dropping `ShedHosts` drops
                    // `Registry` and aborts this task with zero ref cycle.
                    let sink: Arc<dyn CrazeSink> = Arc::new(HostCrazeSink {
                        id: id.clone(),
                        gen,
                        state: Arc::downgrade(&self.state),
                        reg: Arc::downgrade(&self.reg),
                        events: Arc::clone(&self.events),
                        on_craze: Arc::downgrade(&self.on_craze),
                    });
                    let task = self.handle.spawn(crate::shed_craze::run(
                        source.clone(),
                        reach.probe.clone(),
                        sink,
                        self.craze_timings,
                        Arc::clone(&wake),
                    ));
                    reg.crazes.insert(
                        id.clone(),
                        CrazeHost {
                            source: Some(source.clone()),
                            ask: Some(ask.clone()),
                            wake: Arc::clone(&wake),
                            task,
                            gen,
                            gone: Some(gone),
                        },
                    );
                    true
                }
                _ => false,
            }
        };
        if installed {
            Some((id.clone(), gen))
        } else {
            if let Some(exec) = exec {
                let registered = lock(&self.reg).ids.contains(id);
                let mut execs = lock(&self.execs);
                if !registered && execs.get(id).is_some_and(|e| Arc::ptr_eq(e, &exec)) {
                    execs.remove(id);
                }
            }
            None
        }
    }

    async fn remove(&self, ids: &[ShedId]) -> RemovedEvents {
        let mut removed = Vec::with_capacity(ids.len());
        for id in ids {
            removed.push(self.detach(id).await);
        }
        let stops: Vec<_> = removed
            .iter_mut()
            .filter_map(|r| {
                r.craze
                    .take()
                    .map(|craze| (r.id.clone(), self.handle.spawn(craze.stop())))
            })
            .collect();
        for (id, stop) in stops {
            if !stop.await.unwrap_or(false) {
                tracing::warn!(
                    target = %id,
                    secs = CRAZE_STOP_WAIT.as_secs(),
                    "craze source had not stopped after removal (a lane verb still holds it); its last holder releases it"
                );
            }
        }
        let craze_removed = removed
            .iter()
            .filter_map(|r| r.craze_removed.map(|(gen, seq)| (r.id.clone(), gen, seq)))
            .collect();
        let hosts_changed = removed.iter().any(|r| r.was_listed || r.was_offered);
        RemovedEvents {
            craze_removed,
            hosts_changed,
        }
    }

    async fn detach(&self, id: &ShedId) -> Detached {
        let (removed, craze, dropped) = {
            // Canonical lock order: `state` -> `armed` -> `reg` -> `execs` -> `retired`.
            // Clearing `armed` and removing `reg` atomically closes any race
            // with `arm_if_same_incarnation`.
            let mut state = lock(&self.state);
            let mut armed = lock(&self.armed);
            let mut reg = lock(&self.reg);
            if let Some(flag) = armed.remove(id) {
                flag.store(false, Ordering::Release);
            }
            let watcher = reg.watchers.remove(id);
            let reach = reg.reaches.remove(id);
            reg.ids.retain(|known| known != id);
            let craze = reg.crazes.remove(id);
            lock(&self.probed).remove(id);
            let mut execs = lock(&self.execs);
            let exec = execs.remove(id);
            {
                let mut retired = lock(&self.retired);
                retired.prune();
                if let Some(reach) = &reach {
                    retired.reaches.push(Arc::downgrade(reach));
                }
                if let Some(exec) = &exec {
                    // Retain a strong ref until orderly shutdown sweeps it.
                    if !retired.execs.iter().any(|e| Arc::ptr_eq(e, exec)) {
                        retired.execs.push(Arc::clone(exec));
                    }
                }
            }
            drop(execs);
            let mut removed = state.remove(id);
            let seq = removed.as_mut().map(|m| m.craze.mint_news());
            (removed.zip(seq), craze, (watcher, reach, exec))
        };
        #[cfg(test)]
        at_gap(&self.remove_gap).await;
        drop(dropped);
        let craze_removed = removed.as_ref().map(|(m, seq)| (m.craze.gen, *seq));
        let removed = removed.map(|(m, _)| m);
        Detached {
            id: id.clone(),
            was_listed: removed.as_ref().is_some_and(|m| m.listed),
            was_offered: removed.as_ref().is_some_and(HostState::in_shed_craze),
            craze,
            craze_removed,
        }
    }

    // ---- bootstrap ops (`roost.probe` / `preview` / `bootstrap`) ----

    pub async fn probe(&self, id: &ShedId) -> Result<Value, BootstrapFailure> {
        let admitted = self
            .admit()
            .map_err(|e| BootstrapFailure::new(Stage::Probe, e))?;
        admitted
            .run(self.probe_admitted(id), || {
                Err(BootstrapFailure::new(Stage::Probe, cancelled()))
            })
            .await
    }

    async fn probe_admitted(&self, id: &ShedId) -> Result<Value, BootstrapFailure> {
        let probe = self.run_probe(id).await?;
        let token = id.as_target();
        let plan = Plan::for_probe(&token, &probe);
        Ok(json!({
            "target": token,
            "probe": probe_json(&probe),
            "plan": plan_json(&plan),
        }))
    }

    pub async fn preview(&self, id: &ShedId) -> Result<Value, BootstrapFailure> {
        let admitted = self
            .admit()
            .map_err(|e| BootstrapFailure::new(Stage::Probe, e))?;
        admitted
            .run(self.preview_admitted(id), || {
                Err(BootstrapFailure::new(Stage::Probe, cancelled()))
            })
            .await
    }

    async fn preview_admitted(&self, id: &ShedId) -> Result<Value, BootstrapFailure> {
        let probe = self.run_probe(id).await?;
        let token = id.as_target();
        let plan = Plan::for_probe(&token, &probe);
        let env = SourceEnv::from_env();
        let source = bootstrap::preview(&env, &token, &probe.arch);
        Ok(json!({
            "target": token,
            "probe": probe_json(&probe),
            "plan": plan_json(&plan),
            "source": {
                "rung": source.source.as_str(),
                "sentence": source.describe(&token),
                "available": source.available(),
                "skipped": source.skipped,
            },
            "actionable": plan.actionable() && (!plan.needs_source() || source.available()),
            "client_label": CLIENT_LABEL,
            "wired_agents": bootstrap::ROOST_WIRED_AGENTS,
        }))
    }

    pub async fn bootstrap(
        &self,
        id: &ShedId,
        fingerprint: &str,
        consent: bool,
    ) -> Result<Result<Installed, BootstrapFailure>, Refusal> {
        let token = id.as_target();
        let admitted = self
            .admit()
            .map_err(|e| Refusal::ShuttingDown(format!("{token}: {e} — nothing was changed")))?;
        admitted
            .run(self.bootstrap_admitted(id, fingerprint, consent), || {
                Ok(Err(bootstrap_cancelled(&token)))
            })
            .await
    }

    async fn bootstrap_admitted(
        &self,
        id: &ShedId,
        fingerprint: &str,
        consent: bool,
    ) -> Result<Result<Installed, BootstrapFailure>, Refusal> {
        let token = id.as_target();
        if !consent {
            return Err(Refusal::ConsentRequired(format!(
                "{token}: roost.bootstrap needs consent: true — nothing was changed"
            )));
        }
        if lock(&self.reg).closing {
            return Err(Refusal::ShuttingDown(format!(
                "{token}: {ShuttingDown} — nothing was changed"
            )));
        }
        let Some(_gate) = self.begin_bootstrap(id) else {
            return Err(Refusal::AlreadyBootstrapping(format!(
                "{token} is already being bootstrapped — nothing was changed; \
                 wait for that one to finish"
            )));
        };
        let fail = |message: String| BootstrapFailure::new(Stage::Probe, message);
        if let Err(message) = self.ensure_registered_and_notify(id) {
            return Ok(Err(fail(message)));
        }
        // Capture `(reg_gen, reach, exec)` atomically from ONE listed
        // incarnation before the initial probe and keep those exact transports
        // across the entire bootstrap.
        let (reg_gen, reach, exec) = match self.registered_transports(id) {
            Ok(triple) => triple,
            Err(message) => return Ok(Err(fail(message))),
        };
        let probe = {
            let runner = BootstrapRunner {
                exec: exec.as_ref(),
                reach: reach.as_ref(),
                target: &token,
                jail_fs_root: self.jail_fs_root,
            };
            match runner.probe().await {
                Ok(probe) => probe,
                Err(failure) => return Ok(Err(failure)),
            }
        };
        if !self.is_same_listed_incarnation(id, reg_gen) {
            return Ok(Err(BootstrapFailure::new(
                Stage::Probe,
                format!("{token} was removed while probing — nothing was changed"),
            )));
        }
        if probe.fingerprint != fingerprint {
            return Ok(Err(bootstrap::copy::fingerprint_changed(&token)));
        }
        let plan = Plan::for_probe(&token, &probe);
        if let Plan::Report { message, .. } = &plan {
            return Ok(Err(BootstrapFailure::new(Stage::Report, message.clone())));
        }
        if !plan.actionable() {
            return Ok(Err(BootstrapFailure::new(
                Stage::Probe,
                format!("{token} already runs a roost-session this build can read"),
            )));
        }

        let scratch = std::env::temp_dir().join(format!(
            "shed-roost-src-{}-{}",
            std::process::id(),
            fingerprint.get(..8).unwrap_or("0")
        ));
        let source = if plan.needs_source() {
            match bootstrap::resolve(&SourceEnv::from_env(), &token, &probe.arch, &scratch).await {
                Ok(handle) => Some(handle),
                Err(failure) => return Ok(Err(failure)),
            }
        } else {
            None
        };

        // Re-verify the exact same listed incarnation right before installing.
        if !self.is_same_listed_incarnation(id, reg_gen) {
            let _ = std::fs::remove_dir_all(&scratch);
            return Ok(Err(BootstrapFailure::new(
                Stage::Probe,
                format!("{token} was removed before install — nothing was changed"),
            )));
        }

        let request = InstallRequest {
            target: token.clone(),
            jail_fs_root: self.jail_fs_root,
            fingerprint: fingerprint.to_string(),
            client_label: CLIENT_LABEL.to_string(),
        };
        let outcome = {
            let runner = BootstrapRunner {
                exec: exec.as_ref(),
                reach: reach.as_ref(),
                target: &token,
                jail_fs_root: self.jail_fs_root,
            };
            runner.install(request, source).await
        };
        let _ = std::fs::remove_dir_all(&scratch);

        if let Ok(installed) = &outcome {
            // Only grant refresh entitlement if THIS bootstrap actually started
            // the session (not `already-running` from another starter) AND the
            // same registration incarnation is still live & listed.
            if should_arm_hooks(installed) {
                self.arm_if_same_incarnation(id, reg_gen);
            }
            if self.is_same_listed_incarnation(id, reg_gen) {
                self.watch(id, reg_gen);
            }
        }
        Ok(outcome)
    }

    fn arm_if_same_incarnation(&self, id: &ShedId, reg_gen: u64) -> bool {
        let state = lock(&self.state);
        let mut armed = lock(&self.armed);
        let reg = lock(&self.reg);
        if reg.closing || !reg.ids.contains(id) || !lock(&self.listed_sheds).contains(id) {
            return false;
        }
        if !state.get(id).is_some_and(|m| m.reg_gen == reg_gen) {
            return false;
        }
        armed
            .entry(id.clone())
            .or_default()
            .store(true, Ordering::Release);
        true
    }

    fn is_same_listed_incarnation(&self, id: &ShedId, reg_gen: u64) -> bool {
        let state = lock(&self.state);
        let reg = lock(&self.reg);
        !reg.closing
            && lock(&self.listed_sheds).contains(id)
            && state.get(id).is_some_and(|m| m.reg_gen == reg_gen)
    }

    fn begin_bootstrap(&self, id: &ShedId) -> Option<BootstrapGate> {
        let mut running = lock(&self.bootstrapping);
        if !running.insert(id.clone()) {
            return None;
        }
        Some(BootstrapGate {
            set: Arc::clone(&self.bootstrapping),
            id: id.clone(),
        })
    }

    async fn run_probe(&self, id: &ShedId) -> Result<Probe, BootstrapFailure> {
        let fail = |message: String| BootstrapFailure::new(Stage::Probe, message);
        let token = id.as_target();
        self.ensure_registered_and_notify(id).map_err(fail)?;
        let (_reg_gen, reach, exec) = self.registered_transports(id).map_err(fail)?;
        let runner = BootstrapRunner {
            exec: exec.as_ref(),
            reach: reach.as_ref(),
            target: &token,
            jail_fs_root: self.jail_fs_root,
        };
        runner.probe().await
    }

    fn registered_reach(&self, id: &ShedId) -> Result<(u64, Arc<dyn RoostReach>), String> {
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        let state = lock(&self.state);
        let reg = lock(&self.reg);
        if reg.closing {
            return Err(ShuttingDown.to_string());
        }
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        let reg_gen = state
            .get(id)
            .map(|m| m.reg_gen)
            .ok_or_else(|| unknown_host(id, &reg.ids))?;
        let reach = reg.reaches.get(id).cloned().ok_or_else(|| {
            format!("{id} has no usable transport (its reach could not be built)")
        })?;
        Ok((reg_gen, reach))
    }

    fn registered_transports(&self, id: &ShedId) -> Result<RegisteredTransports, String> {
        let _ = self.exec_for(id)?;
        let state = lock(&self.state);
        let reg = lock(&self.reg);
        if reg.closing {
            return Err(ShuttingDown.to_string());
        }
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        let reg_gen = state
            .get(id)
            .map(|m| m.reg_gen)
            .ok_or_else(|| unknown_host(id, &reg.ids))?;
        let reach = reg.reaches.get(id).cloned().ok_or_else(|| {
            format!("{id} has no usable transport (its reach could not be built)")
        })?;
        let exec = lock(&self.execs)
            .get(id)
            .cloned()
            .ok_or_else(|| unknown_host(id, &reg.ids))?;
        Ok((reg_gen, reach, exec))
    }

    fn exec_for(&self, id: &ShedId) -> Result<Arc<SshExec>, String> {
        if !self.reach_options.allows_ssh() {
            return Err(format!(
                "{id}: refusing to spawn system ssh in test mode without an ssh_bin override"
            ));
        }
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        if lock(&self.reg).closing {
            return Err(ShuttingDown.to_string());
        }
        if let Some(exec) = lock(&self.execs).get(id) {
            if lock(&self.listed_sheds).contains(id) {
                return Ok(Arc::clone(exec));
            }
            return Err(unlisted_shed(id));
        }
        let entry = self.ssh_entry(id)?;
        let exec = Arc::new(
            SshExec::new(&entry, &self.reach_options.bridge_options())
                .map_err(|e| e.to_string())?,
        );
        let reg = lock(&self.reg);
        if reg.closing {
            return Err(ShuttingDown.to_string());
        }
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        let mut execs = lock(&self.execs);
        Ok(Arc::clone(execs.entry(id.clone()).or_insert_with(|| exec)))
    }

    /// The single authorization door for SSH on every shed.
    fn ssh_entry(&self, id: &ShedId) -> Result<SshEndpoint, String> {
        let entry = self
            .config
            .servers
            .iter()
            .find(|s| s.name == id.server())
            .ok_or_else(|| format!("no server {:?} in ~/.shed/config.yaml", id.server()))?;
        if !lock(&self.listed_sheds).contains(id) {
            return Err(unlisted_shed(id));
        }
        shed_reach_entry(entry, id.shed())
    }

    #[cfg(test)]
    fn reach(&self, id: &ShedId) -> Result<Arc<dyn RoostReach>, String> {
        self.registered_reach(id).map(|(_, reach)| reach)
    }

    fn watch_handle(&self) -> WatchHandle {
        WatchHandle {
            state: Arc::clone(&self.state),
            reg: Arc::clone(&self.reg),
            listed_sheds: Arc::clone(&self.listed_sheds),
            handle: self.handle.clone(),
            events: Arc::clone(&self.events),
            armed: Arc::clone(&self.armed),
        }
    }

    fn admit(&self) -> Result<AdmittedGuard, String> {
        let reg = lock(&self.reg);
        if reg.closing {
            return Err(ShuttingDown.to_string());
        }
        Ok(self.admitted.admit())
    }

    // ---- orderly shutdown phases ----

    pub fn begin_closing(&self) {
        lock(&self.reg).closing = true;
    }

    pub async fn join_clients(&self, within: Duration) -> JoinedClients {
        let started = tokio::time::Instant::now();
        let hard = started + within;
        let soft = started + within.mul_f64(0.6);
        let watchers: Vec<RoostWatcher> = {
            let mut reg = lock(&self.reg);
            reg.closing = true;
            std::mem::take(&mut reg.watchers).into_values().collect()
        };
        let mut probes = {
            let _reg = lock(&self.reg);
            std::mem::take(&mut *lock(&self.probes))
        };
        let watcher_count = watchers.len();
        let probe_count = probes.len();

        let mut watcher_stops: Vec<tokio::task::JoinHandle<()>> = watchers
            .into_iter()
            .map(|watcher| self.handle.spawn(watcher.stop_and_join()))
            .collect();
        let watchers_join = async {
            let mut left = 0;
            for stop in watcher_stops.iter_mut() {
                if tokio::time::timeout_at(hard, stop).await.is_err() {
                    left += 1;
                }
            }
            left
        };
        let probes_join = async {
            let ended = tokio::time::timeout_at(soft, async {
                while probes.join_next().await.is_some() {}
            })
            .await
            .is_ok();
            if ended {
                return (0, 0);
            }
            let aborted = probes.len();
            probes.abort_all();
            let _ = tokio::time::timeout_at(hard, async {
                while probes.join_next().await.is_some() {}
            })
            .await;
            let left = probes.len();
            probes.detach_all();
            (aborted, left)
        };
        let admitted_join = async {
            if self.admitted.wait_idle(soft).await {
                return (0, 0);
            }
            let cancelled = self.admitted.in_flight();
            self.admitted.cancel_all();
            self.admitted.wait_idle(hard).await;
            (cancelled, self.admitted.in_flight())
        };
        let (watchers_left, (probes_aborted, probes_left), (cancelled, one_shots_left)) =
            tokio::join!(watchers_join, probes_join, admitted_join);
        if watchers_left + probes_left + one_shots_left > 0 {
            tracing::warn!(
                watchers_left,
                probes_left,
                one_shots_left,
                "shutdown: roost clients still running at bound; abandoned"
            );
        }
        JoinedClients {
            watchers: watcher_count,
            watchers_left,
            probes: probe_count,
            probes_aborted,
            probes_left,
            cancelled,
            one_shots_left,
        }
    }

    pub async fn stop_crazes(&self) -> (usize, usize) {
        let crazes: Vec<(ShedId, CrazeHost)> = std::mem::take(&mut lock(&self.reg).crazes)
            .into_iter()
            .collect();
        let count = crazes.len();
        let stops: Vec<(ShedId, tokio::task::JoinHandle<bool>)> = crazes
            .into_iter()
            .map(|(id, craze)| (id, self.handle.spawn(craze.stop())))
            .collect();
        let mut late = 0;
        for (id, stop) in stops {
            if !stop.await.unwrap_or(false) {
                late += 1;
                tracing::warn!(
                    target = %id,
                    secs = CRAZE_STOP_WAIT.as_secs(),
                    "shutdown: craze source had not stopped within bound; registry drain ends its children"
                );
            }
        }
        (count, late)
    }

    pub async fn shutdown_reaches(&self) -> usize {
        let mut reaches: Vec<Arc<dyn RoostReach>> =
            lock(&self.reg).reaches.values().cloned().collect();
        for retired in lock(&self.retired)
            .reaches
            .iter()
            .filter_map(std::sync::Weak::upgrade)
        {
            if !reaches.iter().any(|reach| Arc::ptr_eq(reach, &retired)) {
                reaches.push(retired);
            }
        }
        let count = reaches.len();
        all(
            &self.handle,
            reaches
                .into_iter()
                .map(|reach| async move { reach.shutdown().await }),
        )
        .await;
        if let Some(sink) = &self.reach_options.teardowns {
            sink.join_all().await;
        }
        count
    }

    pub async fn shutdown_execs(&self) -> (usize, usize) {
        let execs: Vec<Arc<SshExec>> = {
            let registered = lock(&self.execs);
            let mut retired = lock(&self.retired);
            let mut execs: Vec<Arc<SshExec>> = registered.values().cloned().collect();
            for retired in std::mem::take(&mut retired.execs) {
                if !execs.iter().any(|exec| Arc::ptr_eq(exec, &retired)) {
                    execs.push(retired);
                }
            }
            execs
        };
        let count = execs.len();
        let unconfirmed = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        all(
            &self.handle,
            execs.iter().cloned().map(|exec| {
                let unconfirmed = Arc::clone(&unconfirmed);
                async move {
                    if !exec.shutdown().await {
                        unconfirmed.fetch_add(1, Ordering::SeqCst);
                        tracing::warn!(
                            target = %exec.label(),
                            "shutdown: ssh master could not be confirmed ended after retried -O exit"
                        );
                    }
                }
            }),
        )
        .await;
        lock(&self.execs).clear();
        (count, unconfirmed.load(Ordering::SeqCst))
    }
}

/// Only grant hook-refresh entitlement when THIS bootstrap actually started the
/// session (not when `hooks` is `None` or another starter won with
/// `already-running`).
fn should_arm_hooks(installed: &Installed) -> bool {
    installed.hooks.is_some()
        && !installed
            .verdict
            .as_deref()
            .is_some_and(|v| v.starts_with("already-running"))
}

struct WatchHandle {
    state: Arc<Mutex<BTreeMap<ShedId, HostState>>>,
    reg: Arc<Mutex<Registry>>,
    listed_sheds: Arc<Mutex<BTreeSet<ShedId>>>,
    handle: tokio::runtime::Handle,
    events: Arc<dyn HostEvents>,
    armed: Arc<Mutex<ArmedFlags>>,
}

impl WatchHandle {
    fn still_registered(&self, id: &ShedId, reg_gen: u64) -> bool {
        let state = lock(&self.state);
        let reg = lock(&self.reg);
        !reg.closing
            && lock(&self.listed_sheds).contains(id)
            && state.get(id).is_some_and(|m| m.reg_gen == reg_gen)
    }

    fn watch(&self, id: &ShedId, expected_reg_gen: u64) {
        let (reach, flag) = {
            // Canonical lock order: `state` -> `armed` -> `reg`.
            let state = lock(&self.state);
            if !state.get(id).is_some_and(|m| m.reg_gen == expected_reg_gen) {
                return;
            }
            let mut armed = lock(&self.armed);
            let reg = lock(&self.reg);
            if reg.closing || reg.watchers.contains_key(id) {
                return;
            }
            let Some(reach) = reg.reaches.get(id) else {
                return;
            };
            let flag = Arc::clone(armed.entry(id.clone()).or_default());
            (Arc::clone(reach), flag)
        };
        let (watcher, rx) = RoostWatcher::spawn_with(
            &self.handle,
            Arc::clone(&reach),
            id.as_target(),
            RoostWatcherOptions {
                hooks: Some(HooksRefresh {
                    target: id.as_target(),
                    client_label: CLIENT_LABEL.to_string(),
                    armed: flag,
                }),
            },
        );
        if !install_and_flag(
            &self.state,
            &self.reg,
            id,
            expected_reg_gen,
            &reach,
            watcher,
        ) {
            return;
        }
        self.handle.spawn(consume(
            id.clone(),
            expected_reg_gen,
            rx,
            Arc::downgrade(&self.state),
            Arc::clone(&self.events),
        ));
        self.events.on_hosts_changed();
    }
}

struct HostCrazeSink {
    id: ShedId,
    gen: u64,
    state: Weak<Mutex<BTreeMap<ShedId, HostState>>>,
    reg: Weak<Mutex<Registry>>,
    events: Arc<dyn HostEvents>,
    on_craze: Weak<Mutex<Option<OnCrazeNews>>>,
}

impl HostCrazeSink {
    fn fold(
        &self,
        cause: Departure,
        f: impl FnOnce(&mut CrazeState, &[LaneSession]) -> Option<crate::shed_craze::Applied>,
    ) -> Dormancy {
        let (Some(state), Some(reg)) = (self.state.upgrade(), self.reg.upgrade()) else {
            return Dormancy::Gone;
        };
        let result = {
            let mut guard = lock(&state);
            let Some(m) = guard.get_mut(&self.id) else {
                return Dormancy::Gone;
            };
            if m.craze.gen != self.gen {
                return Dormancy::Gone;
            }
            let before_standing = m.craze.standing();
            let created = created_rows(&lock(&reg), &self.id, m);
            let Some(applied) = f(&mut m.craze, &created) else {
                return Dormancy::Stale;
            };
            if applied.ready {
                m.listed = true;
            }
            let seq = (!applied.departed.is_empty() || !applied.listed.is_empty())
                .then(|| m.craze.mint_news());
            let changed = if m.listed {
                applied.visible
            } else {
                m.craze.standing() != before_standing
            };
            (applied.departed, applied.listed, seq, changed)
        };
        let (departed, listed, seq, changed) = result;
        if let (Some(seq), Some(on_craze)) = (seq, self.on_craze.upgrade()) {
            publish_craze_departed(&on_craze, &self.id, self.gen, seq, &departed, cause);
            publish_craze_returned(&on_craze, &self.id, self.gen, seq, &listed);
        }
        if changed {
            self.events.on_hosts_changed();
        }
        Dormancy::Folded
    }
}

impl CrazeSink for HostCrazeSink {
    fn event(&self, event: SourceEvent) -> bool {
        self.fold(Departure::Removed, |c, _| Some(c.apply(event))) != Dormancy::Gone
    }

    fn listings(&self) -> u64 {
        let Some(state) = self.state.upgrade() else {
            return 0;
        };
        let count = lock(&state)
            .get(&self.id)
            .filter(|m| m.craze.gen == self.gen)
            .map_or(0, |m| m.craze.listings());
        count
    }

    fn dormant(&self, probed_at: u64) -> Dormancy {
        self.fold(Departure::HubGone, |c, created| {
            c.dormant(created, probed_at)
        })
    }

    fn offline(&self, cause: SourceOffline, reason: String) -> bool {
        self.fold(Departure::Removed, |c, _| Some(c.offline(cause, reason))) != Dormancy::Gone
    }
}

fn publish_craze(slot: &Mutex<Option<OnCrazeNews>>, id: &ShedId, gen: u64, news: CrazeNews<'_>) {
    let observer = lock(slot).clone();
    if let Some(observer) = observer {
        observer(id, gen, news);
    }
}

fn publish_craze_departed(
    slot: &Mutex<Option<OnCrazeNews>>,
    id: &ShedId,
    gen: u64,
    seq: u64,
    host_ids: &[String],
    cause: Departure,
) {
    if !host_ids.is_empty() {
        publish_craze(
            slot,
            id,
            gen,
            CrazeNews::Departed {
                host_ids,
                cause,
                seq,
            },
        );
    }
}

fn publish_craze_returned(
    slot: &Mutex<Option<OnCrazeNews>>,
    id: &ShedId,
    gen: u64,
    seq: u64,
    host_ids: &[String],
) {
    if !host_ids.is_empty() {
        publish_craze(slot, id, gen, CrazeNews::Returned { host_ids, seq });
    }
}

async fn consume(
    id: ShedId,
    reg_gen: u64,
    mut rx: tokio::sync::mpsc::UnboundedReceiver<RoostUpdate>,
    state: Weak<Mutex<BTreeMap<ShedId, HostState>>>,
    events: Arc<dyn HostEvents>,
) {
    while let Some(update) = rx.recv().await {
        let Some(state) = state.upgrade() else {
            return;
        };
        let visible = {
            let mut guard = lock(&state);
            // Generation fence: discard queued updates from an aborted old
            // watcher if the shed was removed or replaced by a new incarnation.
            let Some(m) = guard.get_mut(&id).filter(|m| m.reg_gen == reg_gen) else {
                return;
            };
            match update {
                RoostUpdate::Snapshot(inventory) => {
                    let known = inventory.known_tab_ids();
                    let now = Instant::now();
                    m.opened_tabs.retain(|tab_id, opened| {
                        opened.retained(known.contains(tab_id), &inventory, now)
                    });
                    m.sessions = inventory.sessions;
                    m.reachable = true;
                    m.detail = None;
                    m.down_kind = None;
                    m.seen = true;
                    m.listed = true;
                }
                RoostUpdate::Down { reason, kind } => {
                    m.reachable = false;
                    m.detail = Some(reason);
                    m.down_kind = Some(kind);
                }
            }
            m.listed
        };
        if visible {
            events.on_hosts_changed();
        }
    }
}

async fn session_answers(reach: &dyn RoostReach) -> bool {
    let probe = async {
        let endpoint = reach.ensure().await.ok()?;
        let mut conn = Conn::endpoint(&endpoint).await.ok()?;
        conn.session_identify().await.ok()
    };
    match tokio::time::timeout(SHED_PROBE_BUDGET, probe).await {
        Ok(Some(_)) => true,
        _ => {
            reach.invalidate().await;
            false
        }
    }
}

async fn all<F>(handle: &tokio::runtime::Handle, futures: impl IntoIterator<Item = F>)
where
    F: std::future::Future<Output = ()> + Send + 'static,
{
    let tasks: Vec<tokio::task::JoinHandle<()>> =
        futures.into_iter().map(|f| handle.spawn(f)).collect();
    for task in tasks {
        let _ = task.await;
    }
}

fn created_rows(reg: &Registry, id: &ShedId, m: &HostState) -> Vec<LaneSession> {
    reg.crazes
        .get(id)
        .filter(|craze| craze.gen == m.craze.gen)
        .and_then(|craze| craze.source.as_ref())
        .map(CrazeSource::created_rows)
        .unwrap_or_default()
        .into_iter()
        .filter(|row| !m.craze.rows.contains_key(&row.id))
        .collect()
}

fn host_row(id: &ShedId, session: &RoostSession, stale: bool) -> Value {
    let mut row = serde_json::to_value(session.to_rc_dto()).unwrap_or_else(|_| json!({}));
    if let Some(obj) = row.as_object_mut() {
        let target = id.as_target();
        obj.insert("origin".into(), json!(target));
        obj.insert("origin_kind".into(), json!("shed"));
        obj.insert("target".into(), json!(target));
        obj.insert("source".into(), json!("roost"));
        obj.insert("host".into(), json!(id.server()));
        obj.insert("shed".into(), json!(id.shed()));
        obj.insert("stale".into(), json!(stale));
        obj.insert("attention".into(), json!(session.attention));
        obj.insert("tab_id".into(), json!(session.tab_id.to_string()));
    }
    row
}

fn craze_row(id: &ShedId, session: &LaneSession, stale: bool, tab_id: Option<i64>) -> Value {
    let target = id.as_target();
    let mut row = json!({
        "host": id.server(),
        "shed": id.shed(),
        "slug": session.id,
        "tmux_session": "",
        "display_name": session.title,
        "workdir": session.cwd,
        "kind": CRAZE_KIND,
        "state": "ready",
        "managed": false,
        "activity": session.activity,
        "origin": target,
        "origin_kind": "shed",
        "target": target,
        "source": "craze",
        "stale": stale,
        "approximate": session.approximate || stale,
        "attention": false,
        "pending_approvals": session.pending_approvals,
        "provider": session.provider,
        "model": session.model,
        "doing": session.doing,
        "head_ask_summary": session.head_ask_summary,
        "last_reply": session.last_reply,
        "attached": session.attached,
        "start_error": session.start_error,
        "permission_mode": session.permission_mode,
        "provider_session_id": session.provider_session_id,
        "agent_lane": {"kind": CRAZE_KIND, "session_id": session.id},
    });
    if let (Some(tab), Some(obj)) = (tab_id, row.as_object_mut()) {
        obj.insert("tab_id".into(), json!(tab.to_string()));
    }
    row
}

fn unknown_host(id: &ShedId, ids: &[ShedId]) -> String {
    format!(
        "no shed {id} is being watched (have: {})",
        ids.iter()
            .map(ShedId::as_target)
            .collect::<Vec<_>>()
            .join(", ")
    )
}

fn unlisted_shed(id: &ShedId) -> String {
    format!(
        "no shed {:?} on server {:?}: {:?} has not listed a running shed by that name, so nothing was contacted",
        id.shed(),
        id.server(),
        id.server(),
    )
}

fn cancelled() -> String {
    format!("{ShuttingDown} (the app quit while this was in flight)")
}

fn bootstrap_cancelled(target: &str) -> BootstrapFailure {
    BootstrapFailure::new(
        Stage::Probe,
        format!(
            "{target}: {} — the app quit while this bootstrap ran, and it may have \
             stopped part way; preview the host again before retrying",
            cancelled()
        ),
    )
}

// ---------------------------------------------------------------------------
// bootstrap JSON encoders
// ---------------------------------------------------------------------------

fn probe_json(probe: &Probe) -> Value {
    let outcome = match &probe.outcome {
        ProbeOutcome::Compatible { path, identity } => json!({
            "kind": "compatible",
            "path": path,
            "identity": identity_json(Some(identity)),
        }),
        ProbeOutcome::Mismatch { path, identity } => json!({
            "kind": "mismatch",
            "path": path,
            "identity": identity_json(identity.as_ref()),
        }),
        ProbeOutcome::Missing => json!({ "kind": "missing" }),
    };
    let session = match &probe.session {
        SessionState::Running { identity } => json!({
            "state": "running",
            "app_version": identity.app_version,
            "session_protocol": identity.session_protocol,
            "session_id": identity.session_id,
            "started_at": identity.started_at,
        }),
        SessionState::NoSession => json!({ "state": "no-session" }),
        SessionState::NotInstalled => json!({ "state": "not-installed" }),
    };
    json!({
        "outcome": outcome,
        "arch": probe.arch,
        "home": probe.home,
        "session": session,
        "candidates": probe.candidates,
        "dest": probe.install_dest(),
        "fingerprint": probe.fingerprint,
    })
}

fn identity_json(identity: Option<&bootstrap::Identity>) -> Value {
    match identity {
        None => Value::Null,
        Some(identity) => json!({
            "app_version": identity.app_version,
            "session_protocol": identity.session_protocol,
            "libghostty_build": identity.libghostty_build,
        }),
    }
}

fn plan_json(plan: &Plan) -> Value {
    let mut value = json!({ "kind": plan.as_str(), "needs_source": plan.needs_source() });
    let object = value.as_object_mut().expect("a json object");
    match plan {
        Plan::Install { dest } => {
            object.insert("dest".into(), json!(dest));
        }
        Plan::Update {
            path,
            incumbent,
            replaces_newer,
            dest,
        } => {
            object.insert("path".into(), json!(path));
            object.insert("incumbent".into(), identity_json(incumbent.as_ref()));
            object.insert("replaces_newer".into(), json!(replaces_newer));
            object.insert("dest".into(), json!(dest));
        }
        Plan::Start { path } => {
            object.insert("path".into(), json!(path));
        }
        Plan::UpToDate { identity } => {
            object.insert("session_protocol".into(), json!(identity.session_protocol));
            object.insert("app_version".into(), json!(identity.app_version));
        }
        Plan::Report { protocol, message } => {
            object.insert("session_protocol".into(), json!(protocol));
            object.insert("message".into(), json!(message));
        }
    }
    value
}

/// Encode a successful [`Installed`] result as `roost.bootstrap` JSON.
pub fn installed_json(installed: &Installed) -> Value {
    json!({
        "ok": true,
        "target": installed.target,
        "plan": plan_json(&installed.plan),
        "dest": installed.dest,
        "verdict": installed.verdict,
        "session_protocol": installed.session.as_ref().map(|s| s.session_protocol),
        "session_id": installed.session.as_ref().map(|s| s.session_id.clone()),
        "path_warning": installed.path_warning,
        "backup_warning": installed.backup_warning,
        "hooks": installed.hooks.as_ref().map(hooks_json),
    })
}

fn hooks_json(hooks: &HooksResult) -> Value {
    json!({
        "client": hooks.client_label,
        "applied": hooks.applied(),
        "wired": hooks.wired,
        "refreshed": hooks.refreshed,
        "removed": hooks.removed,
        "skipped": hooks.skipped.iter().map(|s| json!({ "agent": s.agent, "reason": s.reason }))
            .collect::<Vec<_>>(),
        "errors": hooks.errors.iter().map(|e| json!({ "agent": e.agent, "error": e.error }))
            .collect::<Vec<_>>(),
        "error": hooks.error,
    })
}

/// Encode a [`BootstrapFailure`] as `roost.bootstrap` JSON.
pub fn failure_json(failure: &BootstrapFailure) -> Value {
    json!({
        "ok": false,
        "error": {
            "stage": failure.stage.as_str(),
            "message": failure.message,
            "restored": failure.restored,
        },
    })
}

#[cfg(test)]
mod craze_tests;

#[cfg(test)]
mod shutdown_tests;

#[cfg(test)]
mod tests {
    use super::*;

    use std::collections::HashMap;
    use std::os::unix::fs::PermissionsExt as _;

    use shed_core::config::ShedServerEntry;
    use shed_roost::testing::{ownership, FakeRoost};

    const VECTOR_TAB: i64 = 5;

    fn shed(server: &str, name: &str) -> ShedId {
        ShedId::new(server, name).unwrap()
    }

    fn server_config(name: &str, identity_file: &str) -> ShedConfig {
        ShedConfig {
            servers: vec![ShedServerEntry {
                name: name.to_string(),
                host: "127.0.0.1".to_string(),
                http_port: 8080,
                ssh_port: 2222,
                identity_file: identity_file.to_string(),
                ..Default::default()
            }],
            ..Default::default()
        }
    }

    fn write_recording_ssh(dir: &std::path::Path) -> (std::path::PathBuf, std::path::PathBuf) {
        let log = dir.join("ssh-calls.log");
        let ssh = dir.join("fake-ssh");
        let script = format!(
            "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '{}'\nexit 1\n",
            log.display()
        );
        std::fs::write(&ssh, script).unwrap();
        std::fs::set_permissions(&ssh, std::fs::Permissions::from_mode(0o755)).unwrap();
        (ssh, log)
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

    /// **Security invariant 1**: parsing a valid `ShedId` for a configured
    /// server NEVER authorizes SSH if the shed is not currently server-listed.
    /// Zero SSH processes are spawned for probe, preview, bootstrap, craze ops,
    /// and open_terminal.
    #[tokio::test]
    async fn unlisted_shed_spawns_zero_ssh_across_all_ops() {
        let dir = tempfile::tempdir().unwrap();
        let (ssh_bin, ssh_log) = write_recording_ssh(dir.path());
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                ssh_bin: Some(ssh_bin),
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        let unlisted = shed("srv", "phantom");
        assert!(hosts.probe(&unlisted).await.is_err());
        assert!(hosts.preview(&unlisted).await.is_err());
        assert!(hosts
            .bootstrap(&unlisted, "fp", true)
            .await
            .unwrap()
            .is_err());
        assert_eq!(
            hosts
                .craze_create_options(&unlisted)
                .await
                .unwrap_err()
                .code,
            "bad_request"
        );
        let req =
            crate::shed_craze::create_request(Some("/home/shed"), None, None, None, None).unwrap();
        assert_eq!(
            hosts
                .craze_create(&unlisted, req, false)
                .await
                .unwrap_err()
                .code,
            "bad_request"
        );
        assert_eq!(
            hosts.craze_saved(&unlisted).await.unwrap_err().code,
            "bad_request"
        );
        assert_eq!(
            hosts
                .craze_reopen(&unlisted, "saved-1")
                .await
                .unwrap_err()
                .code,
            "bad_request"
        );
        assert_eq!(
            hosts
                .craze_open_terminal(&unlisted, "0123456789ab")
                .await
                .unwrap_err()
                .code,
            "bad_request"
        );
        assert!(
            !ssh_log.exists(),
            "unlisted shed must never spawn SSH, got: {:?}",
            std::fs::read_to_string(&ssh_log)
        );
        assert!(lock(&hosts.reg).ids.is_empty());
    }

    /// **Security invariant 2**: an already-running `roost-session` discovered
    /// by probe-then-watch NEVER receives unsolicited `session.set_agent_hooks`
    /// writes.
    #[tokio::test]
    async fn already_running_roost_session_never_receives_unsolicited_hook_writes() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(
            VECTOR_TAB,
            "working",
            Some(ownership("claude", "ses-1", "", 1_700_000_100)),
            false,
        );
        let target = shed("srv", "s1");

        let mut sockets = HashMap::new();
        sockets.insert(target.as_target(), fake.socket_path().to_path_buf());
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                roost_sockets: sockets,
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        hosts
            .observe_sheds(std::slice::from_ref(&target), &["srv".to_string()])
            .await;

        wait_for("shed watcher delivers first snapshot", || {
            let snap = hosts.snapshot(HostFilter::ALL);
            (!snap.sessions.is_empty()).then_some(snap)
        })
        .await;

        // Give the watcher a brief beat after initial subscribe/tab.list.
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(
            fake.agent_hooks_calls().is_empty(),
            "already-running roost-session must never get unsolicited hook writes: {:?}",
            fake.agent_hooks_calls()
        );
    }

    /// **Security invariant 3**: removing and re-registering a shed clears its
    /// bootstrap entitlement; the re-registered shed never inherits `armed`.
    #[tokio::test]
    async fn removed_and_reregistered_shed_never_inherits_bootstrap_entitlement() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(
            VECTOR_TAB,
            "working",
            Some(ownership("claude", "ses-1", "", 1_700_000_100)),
            false,
        );
        let target = shed("srv", "s1");

        let mut sockets = HashMap::new();
        sockets.insert(target.as_target(), fake.socket_path().to_path_buf());
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                roost_sockets: sockets,
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        hosts
            .observe_sheds(std::slice::from_ref(&target), &["srv".to_string()])
            .await;
        wait_for("initial watch", || {
            (!hosts.snapshot(HostFilter::ALL).sessions.is_empty()).then_some(())
        })
        .await;

        // Simulate a successful bootstrap arming this shed during this run.
        {
            let mut armed = lock(&hosts.armed);
            if lock(&hosts.reg).ids.contains(&target) {
                armed
                    .entry(target.clone())
                    .or_default()
                    .store(true, Ordering::Release);
            }
        }
        assert!(lock(&hosts.armed)
            .get(&target)
            .is_some_and(|f| f.load(Ordering::Acquire)));

        // Authoritative refresh reports the shed stopped -> detaches & disarms.
        hosts.observe_sheds(&[], &["srv".to_string()]).await;
        assert!(!lock(&hosts.armed).contains_key(&target));
        assert!(lock(&hosts.reg).ids.is_empty());

        // Re-registering the same shed starts with fresh `false` entitlement.
        let before_hooks = fake.agent_hooks_calls().len();
        hosts
            .observe_sheds(std::slice::from_ref(&target), &["srv".to_string()])
            .await;
        wait_for("re-registered watch", || {
            (!hosts.snapshot(HostFilter::ALL).sessions.is_empty()).then_some(())
        })
        .await;
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(
            fake.agent_hooks_calls().len(),
            before_hooks,
            "re-registered shed must not inherit bootstrap entitlement"
        );
        assert!(
            !lock(&hosts.armed)
                .get(&target)
                .is_some_and(|f| f.load(Ordering::Acquire)),
            "fresh flag must be false"
        );
    }

    /// **Security invariant 3b (ABA race)**: if a shed is detached AND
    /// re-registered while a bootstrap is in flight, completing the stale
    /// bootstrap MUST NOT arm the new incarnation.
    #[tokio::test]
    async fn removal_and_reregistration_racing_bootstrap_never_arms_new_incarnation() {
        let target = shed("srv", "s1");
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        lock(&hosts.listed_sheds).insert(target.clone());
        hosts.ensure_registered(&target).unwrap();
        let gen1 = lock(&hosts.state).get(&target).unwrap().reg_gen;

        // Detach + re-register before the in-flight bootstrap finishes.
        hosts.detach(&target).await;
        hosts.ensure_registered(&target).unwrap();
        let gen2 = lock(&hosts.state).get(&target).unwrap().reg_gen;
        assert_ne!(gen1, gen2);

        // Stale gen1 bootstrap finishing must refuse to arm gen2.
        assert!(!hosts.arm_if_same_incarnation(&target, gen1));
        assert!(!lock(&hosts.armed)
            .get(&target)
            .is_some_and(|f| f.load(Ordering::Acquire)));

        // A bootstrap belonging to gen2 itself DOES arm gen2.
        assert!(hosts.arm_if_same_incarnation(&target, gen2));
        assert!(lock(&hosts.armed)
            .get(&target)
            .is_some_and(|f| f.load(Ordering::Acquire)));
    }

    /// **Security invariant 4**: if the server's `identity_file` is removed
    /// after registration, subsequent SSH use refuses cleanly and spawns zero
    /// SSH processes.
    #[tokio::test]
    async fn identity_key_removed_after_registration_spawns_zero_ssh() {
        let dir = tempfile::tempdir().unwrap();
        let (ssh_bin, ssh_log) = write_recording_ssh(dir.path());
        let key_path = dir.path().join("id_ed25519");
        std::fs::write(&key_path, "dummy-key").unwrap();
        std::fs::set_permissions(&key_path, std::fs::Permissions::from_mode(0o600)).unwrap();

        let target = shed("srv", "s1");
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", &key_path.display().to_string()),
            ShedReachOptions {
                ssh_bin: Some(ssh_bin),
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        // Mark shed as listed and register its reach while key exists.
        lock(&hosts.listed_sheds).insert(target.clone());
        hosts
            .ensure_registered(&target)
            .expect("registered while key exists");
        assert!(!ssh_log.exists(), "registration alone spawns no SSH");

        // Now remove the identity key from disk BEFORE any SSH use.
        std::fs::remove_file(&key_path).unwrap();

        // Both probe (which creates SshExec / dials SshBridge) and direct
        // reach ensure fail without spawning SSH.
        let err = hosts.probe(&target).await.expect_err("missing key refuses");
        assert!(
            err.message.contains("identity") || err.message.contains("id_ed25519"),
            "unexpected error: {}",
            err.message
        );
        let reach = hosts.reach(&target).unwrap();
        assert!(reach.ensure().await.is_err());
        assert!(
            !ssh_log.exists(),
            "zero SSH must spawn after identity key removal, got: {:?}",
            std::fs::read_to_string(&ssh_log)
        );
    }

    /// `rc.inject_test` works for a listed shed and emits `"target"` (not `"machine"`).
    #[tokio::test]
    async fn inject_test_populates_shed_row_with_target_and_no_machine_field() {
        let target = shed("srv", "s1");
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );
        lock(&hosts.listed_sheds).insert(target.clone());
        hosts
            .inject_test(
                &target,
                InjectedRow {
                    tab_id: 7,
                    kind: RcKind::ClaudeRc,
                    title: "claude".into(),
                    cwd: "/home/shed".into(),
                    lifecycle: Some("working".into()),
                    attention: true,
                },
            )
            .expect("inject succeeds");

        let snap = hosts.snapshot(HostFilter::ALL);
        assert_eq!(snap.hosts.len(), 1);
        assert_eq!(snap.hosts[0]["target"], "roost:srv/s1");
        assert_eq!(snap.sessions.len(), 1);
        assert_eq!(snap.sessions[0]["target"], "roost:srv/s1");
        assert_eq!(snap.sessions[0]["host"], "srv");
        assert_eq!(snap.sessions[0]["shed"], "s1");
        assert_eq!(snap.sessions[0]["source"], "roost");
        assert!(snap.sessions[0].get("machine").is_none());
    }

    /// **Fix 2**: an out-of-order older observation sequence (e.g. a delayed
    /// `rc.list` response) cannot reauthorize a shed removed by a newer refresh.
    #[tokio::test]
    async fn older_observe_seq_cannot_reauthorize_removed_shed() {
        let dir = tempfile::tempdir().unwrap();
        let (ssh_bin, ssh_log) = write_recording_ssh(dir.path());
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                ssh_bin: Some(ssh_bin),
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        let s1 = shed("srv", "s1");
        let seq_stale = hosts.next_observe_seq();
        let seq_newer = hosts.next_observe_seq();

        // Newer authoritative refresh reports `srv` has no running sheds.
        hosts
            .observe_sheds_at(seq_newer, &[], &["srv".to_string()])
            .await;
        // Delayed older `rc.list` tries to re-list `s1` with `seq_stale`.
        hosts
            .observe_sheds_at(seq_stale, std::slice::from_ref(&s1), &[])
            .await;

        assert!(
            !lock(&hosts.listed_sheds).contains(&s1),
            "stale observation must not reauthorize removed shed"
        );
        assert!(hosts.probe(&s1).await.is_err());
        assert!(!ssh_log.exists(), "stale shed must spawn zero SSH");
    }

    /// **Fix 1**: removing a shed from `listed_sheds` immediately blocks
    /// transport lookup (`exec_for`, `registered_reach`, `probe`) even before
    /// `detach` clears `reg`/`execs`.
    #[tokio::test]
    async fn unlisted_shed_cannot_reuse_cached_transports_for_ssh() {
        let dir = tempfile::tempdir().unwrap();
        let (ssh_bin, ssh_log) = write_recording_ssh(dir.path());
        let target = shed("srv", "s1");
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                ssh_bin: Some(ssh_bin),
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );

        lock(&hosts.listed_sheds).insert(target.clone());
        hosts.ensure_registered(&target).unwrap();
        let _cached_exec = hosts.exec_for(&target).unwrap();

        // Now unlist the shed without even running detach yet.
        lock(&hosts.listed_sheds).remove(&target);

        assert!(hosts.exec_for(&target).is_err());
        assert!(hosts.registered_reach(&target).is_err());
        assert!(hosts.probe(&target).await.is_err());
        assert!(!ssh_log.exists(), "unlisted shed must spawn zero SSH");
    }

    /// **Fix 11**: in `test_mode: true` with `ssh_bin: None`, `probe` /
    /// `preview` / `bootstrap` / `exec_for` refuse to construct system `ssh`.
    #[tokio::test]
    async fn test_mode_without_ssh_bin_refuses_system_ssh() {
        let target = shed("srv", "s1");
        let hosts = ShedHosts::start(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                test_mode: true,
                ssh_bin: None,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
        );
        lock(&hosts.listed_sheds).insert(target.clone());

        let err = match hosts.exec_for(&target) {
            Ok(_) => panic!("expected test_mode without ssh_bin to fail"),
            Err(e) => e,
        };
        assert!(err.contains("test mode"), "unexpected error: {err}");
        let err = hosts.probe(&target).await.unwrap_err();
        assert!(
            err.message.contains("test mode"),
            "unexpected error: {}",
            err.message
        );
        let err = hosts.preview(&target).await.unwrap_err();
        assert!(
            err.message.contains("test mode"),
            "unexpected error: {}",
            err.message
        );
        let err = hosts
            .bootstrap(&target, "fp", true)
            .await
            .unwrap()
            .unwrap_err();
        assert!(
            err.message.contains("test mode"),
            "unexpected error: {}",
            err.message
        );
    }

    /// **Fix 4**: `should_arm_hooks` refuses `already-running` verdicts and
    /// `hooks: None`.
    #[test]
    fn should_arm_hooks_refuses_already_running_and_no_hooks() {
        let base = Installed {
            target: "roost:srv/s1".into(),
            plan: Plan::Start {
                path: "/usr/bin/roost-session".into(),
            },
            dest: None,
            verdict: Some("ready pid=42".into()),
            session: None,
            path_warning: None,
            backup_warning: None,
            hooks: Some(HooksResult {
                client_label: CLIENT_LABEL.into(),
                wired: vec![],
                refreshed: vec![],
                removed: vec![],
                skipped: vec![],
                errors: vec![],
                error: None,
            }),
        };
        assert!(should_arm_hooks(&base));

        let mut already = base.clone();
        already.verdict = Some("already-running".into());
        assert!(!should_arm_hooks(&already));

        let mut no_hooks = base;
        no_hooks.hooks = None;
        assert!(!should_arm_hooks(&no_hooks));
    }

    /// **Fix 6**: a queued `RoostUpdate` from an old watcher's `reg_gen` is
    /// discarded and cannot overwrite a replacement registration's state.
    #[tokio::test]
    async fn queued_consume_update_from_old_reg_gen_is_discarded() {
        let target = shed("srv", "s1");
        let state = Arc::new(Mutex::new(BTreeMap::new()));
        let mut fresh = HostState::new(true);
        fresh.reachable = true;
        let new_gen = fresh.reg_gen;
        let old_gen = new_gen.wrapping_sub(1);
        lock(&state).insert(target.clone(), fresh);

        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
        tx.send(RoostUpdate::Down {
            reason: "stale disconnect".into(),
            kind: ReachFamily::Unreachable,
        })
        .unwrap();
        drop(tx);

        consume(
            target.clone(),
            old_gen,
            rx,
            Arc::downgrade(&state),
            Arc::new(NoopHostEvents),
        )
        .await;

        let guard = lock(&state);
        let entry = guard.get(&target).unwrap();
        assert!(
            entry.reachable,
            "stale watcher Down must not overwrite replacement registration"
        );
        assert!(entry.detail.is_none());
    }

    /// **Fix 9**: dropping `ShedDesktopRuntime` drops `ShedHosts` (no `Arc`
    /// cycle through `HostCrazeSink`) and aborts background craze tasks.
    #[tokio::test]
    async fn dropping_runtime_drops_hosts_and_aborts_background_craze_tasks() {
        let s1 = shed("srv", "s1");
        let (dial, _conns) = shed_craze::testing::ScriptedDial::new();
        let craze_reach = CrazeReach {
            dial,
            probe: None,
            ensure: None,
        };
        let hosts = Arc::new(ShedHosts::start_with(
            &tokio::runtime::Handle::current(),
            &server_config("srv", ""),
            ShedReachOptions {
                test_mode: true,
                ..Default::default()
            },
            false,
            Arc::new(NoopHostEvents),
            CrazeReaches::Fixed(Arc::new(move |id: &ShedId| {
                (*id == s1).then(|| craze_reach.clone())
            })),
            CrazeTimings::default(),
        ));
        let s1_id = shed("srv", "s1");
        hosts
            .observe_sheds(std::slice::from_ref(&s1_id), &["srv".to_string()])
            .await;
        assert!(lock(&hosts.reg).crazes.contains_key(&s1_id));

        let weak_hosts = Arc::downgrade(&hosts);
        let weak_reg = Arc::downgrade(&hosts.reg);
        drop(hosts);

        // Give the aborted task a tick to unwind and drop its `HostCrazeSink`.
        wait_for("ShedHosts & Registry drop with zero ref cycle", || {
            (weak_hosts.upgrade().is_none() && weak_reg.upgrade().is_none()).then_some(())
        })
        .await;
    }
}
