//! **Orderly shutdown** (plan 026 §3.3, plan 038 D6): one bounded, idempotent
//! teardown of every child the desktop runtime owns.
//!
//! ## The guarantee
//!
//! After an orderly exit has begun:
//!
//! * **no new child is spawned** — the app's [`SpawnGate`] is closed first, and
//!   every spawn site (the gated craze dials, `SshExec`, `SshBridge`'s
//!   establishment) takes a section of it;
//! * every app-owned child — `SshExec` masters, SSH craze bridges, roost
//!   `SshTunnel`s — is dead, or asked to exit by its own protocol, within a
//!   bounded time;
//! * **persistent processes the app does not own stay**: a remote craze hub
//!   and its session hosts, and a remote `roost-session`. Nothing here signals
//!   them.
//!
//! ## Phases
//!
//! | phase | what |
//! |---|---|
//! | 0 | optional source drain (plan 039: the delivery runtime's streams); `hosts.begin_closing()` + `lanes.begin_closing()`; `gate.close(GATE_CLOSE_WAIT)`; `hosts.join_clients(CLIENTS_JOIN_WAIT)` |
//! | 1 | `lanes.shutdown()` + `gate.close_slots()` on a dedicated OS thread |
//! | 2 | `hosts.stop_crazes()` + `gate.kill_exits(CHILDREN_KILL_WAIT)` |
//! | 3 | `hosts.shutdown_reaches()` (no outer timeout; roost bounds its own teardown) |
//! | 4 | `hosts.shutdown_execs()` (each master clamped to `MASTER_SHUTDOWN_BOUND`) |

use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use shed_roost::{SlotsClosed, SpawnGate};

use crate::shed_hosts::{JoinedClients, ShedHosts};
use crate::shed_lanes::{LanesClosed, ShedLanes};

/// How long closing the gate waits for spawn sections in progress.
pub const GATE_CLOSE_WAIT: Duration = Duration::from_millis(500);

/// Phase 0's bound for joining roost clients and admitted one-shots.
pub const CLIENTS_JOIN_WAIT: Duration = Duration::from_secs(1);

/// How long phase 2 waits for every registered async child's exit to be seen
/// after its kill.
pub const CHILDREN_KILL_WAIT: Duration = Duration::from_secs(2);

/// The signal path's escape: above the worst case, so it never cuts a compliant
/// shutdown short.
pub const ESCAPE: Duration = Duration::from_secs(20);

/// Non-blocking drain hook for the source runtime (plan 039: the delivery
/// runtime's streams and provider commands).
pub type SourceDrain = Box<dyn Fn() + Send + Sync>;

/// Optional line sink for shutdown phase and completion logs.
pub type ShutdownLog = Arc<dyn Fn(&str) + Send + Sync>;

/// The app's one orderly shutdown: what it tears down, and whether it has.
pub struct Shutdown {
    gate: SpawnGate,
    hosts: Arc<ShedHosts>,
    lanes: Arc<ShedLanes>,
    /// The source runtime's drain (plan 039): run first in phase 0.
    source: Mutex<Option<SourceDrain>>,
    log: Mutex<Option<ShutdownLog>>,
    handle: tokio::runtime::Handle,
    started: OnceLock<()>,
    done: tokio::sync::watch::Sender<Option<Report>>,
}

impl Shutdown {
    pub fn new(
        gate: SpawnGate,
        hosts: Arc<ShedHosts>,
        lanes: Arc<ShedLanes>,
        source: Option<SourceDrain>,
        handle: tokio::runtime::Handle,
    ) -> Arc<Shutdown> {
        Arc::new(Shutdown {
            gate,
            hosts,
            lanes,
            source: Mutex::new(source),
            log: Mutex::new(None),
            handle,
            started: OnceLock::new(),
            done: tokio::sync::watch::channel(None).0,
        })
    }

    /// Install or replace the source runtime's drain (plan 039) before shutdown starts:
    /// a non-blocking stop of its streams, run first in phase 0.
    /// No-op if shutdown has already begun.
    pub fn set_source_drain(&self, source: SourceDrain) {
        if self.started.get().is_none() {
            *self.source.lock().unwrap_or_else(|e| e.into_inner()) = Some(source);
        }
    }

    /// Install or replace the optional shutdown log sink before shutdown starts.
    /// No-op if shutdown has already begun.
    pub fn set_log_sink(&self, sink: ShutdownLog) {
        if self.started.get().is_none() {
            *self.log.lock().unwrap_or_else(|e| e.into_inner()) = Some(sink);
        }
    }

    /// Start the run — on the app's runtime, the FIRST time only — and return
    /// a receiver that reads `Some(Report)` once it has finished. Never blocks,
    /// and safe from any thread.
    pub fn start(self: &Arc<Self>) -> tokio::sync::watch::Receiver<Option<Report>> {
        let done = self.done.subscribe();
        if self.started.set(()).is_ok() {
            let this = Arc::clone(self);
            self.handle.spawn(async move {
                let source = this.source.lock().unwrap_or_else(|e| e.into_inner()).take();
                let log = this.log.lock().unwrap_or_else(|e| e.into_inner()).clone();
                let report = shutdown_children_with_log(
                    &this.gate,
                    &this.hosts,
                    &this.lanes,
                    source.as_ref(),
                    log.as_deref(),
                )
                .await;
                this.done.send_replace(Some(report));
            });
        }
        done
    }

    /// [`Self::start`], and wait for the run to finish, returning its [`Report`].
    /// A later caller waits for the first run; none starts a second.
    pub async fn run(self: &Arc<Self>) -> Report {
        let mut done = self.start();
        let _ = done.wait_for(|finished| finished.is_some()).await;
        let report = done.borrow().clone().unwrap_or_default();
        report
    }

    /// Whether the run has finished.
    pub fn is_done(&self) -> bool {
        self.done.borrow().is_some()
    }
}

/// What one run did, phase by phase, and what each phase abandoned at its bound.
#[derive(Debug, Clone, Default)]
pub struct Report {
    pub gate_drained: bool,
    pub sections_open_at_close: usize,
    pub clients: JoinedClients,
    pub lanes: LanesClosed,
    pub slots: SlotsClosed,
    pub crazes: usize,
    pub crazes_late: usize,
    pub children_killed: usize,
    pub children_exited: usize,
    pub sections_open_at_drain: usize,
    pub reaches: usize,
    pub masters: usize,
    pub masters_unconfirmed: usize,
    pub late_killed: usize,
    pub late_unseen: usize,
    pub elapsed: Duration,
}

/// **The shutdown itself** — the five phases, in order.
pub async fn shutdown_children(
    gate: &SpawnGate,
    hosts: &ShedHosts,
    lanes: &Arc<ShedLanes>,
    source: Option<&SourceDrain>,
) -> Report {
    shutdown_children_with_log(gate, hosts, lanes, source, None).await
}

/// Same as [`shutdown_children`], forwarding phase and completion lines to
/// `log` when provided (falling back to `tracing::info!` otherwise).
pub async fn shutdown_children_with_log(
    gate: &SpawnGate,
    hosts: &ShedHosts,
    lanes: &Arc<ShedLanes>,
    source: Option<&SourceDrain>,
    log: Option<&(dyn Fn(&str) + Send + Sync)>,
) -> Report {
    let started = Instant::now();
    let mut report = Report::default();

    // ---- phase 0: stop new work, then join what was admitted -------------
    let phase = Instant::now();
    if let Some(drain) = source {
        drain();
    }
    hosts.begin_closing();
    lanes.begin_closing();
    report.gate_drained = gate.close(GATE_CLOSE_WAIT).await;
    report.sections_open_at_close = gate.sections();
    let joined = hosts.join_clients(CLIENTS_JOIN_WAIT).await;
    report.clients = joined;
    log_phase(
        log,
        0,
        "gate closed; roost clients and admitted work joined",
        phase,
        format_args!(
            "drained={} watchers={} probes={} (aborted {}) one-shots cancelled={}; \
             abandoned: sections={} watchers={} probes={} one-shots={}",
            report.gate_drained,
            joined.watchers,
            joined.probes,
            joined.probes_aborted,
            joined.cancelled,
            report.sections_open_at_close,
            joined.watchers_left,
            joined.probes_left,
            joined.one_shots_left
        ),
    );

    // ---- phase 1: lanes, on a dedicated OS thread ------------------------
    let phase = Instant::now();
    let (lanes_closed, slots) = lanes_phase(Arc::clone(lanes), gate.clone()).await;
    report.lanes = lanes_closed;
    report.slots = slots;
    log_phase(
        log,
        1,
        "lanes",
        phase,
        format_args!(
            "lanes={} (finished={}) pumps={} graces={} slots={}; abandoned: \
             pumps={} graces={} slot reaps={}",
            lanes_closed.lanes,
            lanes_closed.finished,
            lanes_closed.pumps,
            lanes_closed.graces,
            slots.closed,
            lanes_closed.pumps_unjoined,
            lanes_closed.graces_unjoined,
            slots.unsettled
        ),
    );

    // ---- phase 2: craze sources, then every registered child -------------
    let phase = Instant::now();
    let (crazes, late) = hosts.stop_crazes().await;
    let drained = gate.kill_exits(CHILDREN_KILL_WAIT).await;
    report.crazes = crazes;
    report.crazes_late = late;
    report.children_killed = drained.killed;
    report.children_exited = drained.exited;
    report.sections_open_at_drain = drained.sections_open;
    let late_now = gate.late();
    log_phase(
        log,
        2,
        "craze sources and registered children",
        phase,
        format_args!(
            "crazes={crazes} killed={} exited={} late registrations killed={}; abandoned: \
             crazes={late} unseen exits={} sections={} late unseen={:?}",
            drained.killed,
            drained.exited,
            late_now.killed,
            drained.killed.saturating_sub(drained.exited),
            drained.sections_open,
            late_now.unseen
        ),
    );

    // ---- phase 3: roost tunnels, no outer timeout -------------------------
    let phase = Instant::now();
    report.reaches = hosts.shutdown_reaches().await;
    log_phase(
        log,
        3,
        "roost reaches",
        phase,
        format_args!(
            "reaches={} (roost bounds its own teardown; nothing is cut short)",
            report.reaches
        ),
    );

    // ---- phase 4: ssh masters ---------------------------------------------
    let phase = Instant::now();
    let (masters, unconfirmed) = hosts.shutdown_execs().await;
    report.masters = masters;
    report.masters_unconfirmed = unconfirmed;
    log_phase(
        log,
        4,
        "ssh masters",
        phase,
        format_args!("masters={masters}; abandoned: unconfirmed={unconfirmed}"),
    );

    let late_now = gate.late();
    report.late_killed = late_now.killed;
    report.late_unseen = late_now.unseen.len();
    report.elapsed = started.elapsed();
    let done_line = format!(
        "shutdown: done in {} ms (late registrations so far: killed={}, unseen={:?}; gate stays closed)",
        report.elapsed.as_millis() as u64,
        late_now.killed,
        late_now.unseen,
    );
    if let Some(sink) = log {
        sink(&done_line);
    } else {
        tracing::info!("{done_line}");
    }
    report
}

async fn lanes_phase(lanes: Arc<ShedLanes>, gate: SpawnGate) -> (LanesClosed, SlotsClosed) {
    let (tx, rx) = tokio::sync::oneshot::channel();
    let spawned = std::thread::Builder::new()
        .name("shed-shutdown-lanes".to_string())
        .spawn(move || {
            let closed = std::thread::scope(|scope| {
                let sweep = scope.spawn(|| gate.close_slots());
                let lanes_closed = lanes.shutdown();
                (lanes_closed, sweep.join().unwrap_or_default())
            });
            let _ = tx.send(closed);
        });
    if let Err(e) = spawned {
        tracing::warn!(error = %e, "shutdown: could not start the lanes thread");
        return Default::default();
    }
    rx.await.unwrap_or_default()
}

fn log_phase(
    log: Option<&(dyn Fn(&str) + Send + Sync)>,
    n: u8,
    what: &str,
    since: Instant,
    detail: std::fmt::Arguments<'_>,
) {
    let line = format!(
        "shutdown: phase {n} ({what}) took {} ms: {detail}",
        since.elapsed().as_millis() as u64
    );
    if let Some(sink) = log {
        sink(&line);
    } else {
        tracing::info!("{line}");
    }
}
