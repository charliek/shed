//! **The orderly shutdown** (plan 026 §3.3, shed#389): one bounded, idempotent
//! teardown of every child this app owns, from every way it can exit.
//!
//! ## The guarantee
//!
//! After an orderly exit has begun:
//!
//! * **no new child is spawned** — the app's [`SpawnGate`] is closed first, and
//!   every spawn site (the gated craze dials, `SshExec`, `SshForward`'s slot,
//!   `SshBridge`'s establishment) takes a section of it;
//! * every app-owned child — (A) the opencode `ssh -N -L` forwards, (B) the
//!   `SshExec` masters and the ssh craze bridges, (C) the local craze bridges,
//!   (D) roost's `SshTunnel`s — is dead, or asked to exit by its own protocol,
//!   before the process exits, within a bounded time;
//! * **persistent processes the app does not own stay**: a craze hub and its
//!   session hosts, and a remote `roost-session`. Nothing here signals them.
//!
//! ## The phases, in order, each bounding itself
//!
//! The code's real worst case, phase by phase (recomputed in C3 fix round 1,
//! F10 — each row is the sum of the waits that can run one after another):
//!
//! | phase | what | sequential worst case |
//! |---|---|---|
//! | 0 | close the gate ([`GATE_CLOSE_WAIT`] 0.5 s — a tunnel establishment no longer holds it: the close abandons it); then, on a bound of their OWN ([`CLIENTS_JOIN_WAIT`] 1 s), watchers aborted and awaited, probes and admitted one-shots given 0.6 s, then aborted/cancelled and awaited — side by side | 0.5 + 1.0 = **1.5 s** |
//! | 1 | lanes ([`Lanes::shutdown`], a dedicated OS thread): every forward's close (slot close ≤ 1 s, including a kill another thread has in progress, + an in-flight `ensure` ≤ 0.5 s) OVERLAPPED with the pump joins (≤ 1 s); the gate's slot sweep (≤ 1 s) on a thread beside it | max(1.5, 1, 1) = **1.5 s** |
//! | 2 | craze sources ([`RoostHosts::stop_crazes`], `CRAZE_STOP_WAIT` 2 s, in parallel), then every registered child killed and awaited, the open sections waited for, within [`CHILDREN_KILL_WAIT`] 2 s | 2 + 2 = **4 s** |
//! | 3 | every reach's own teardown, in parallel, NO outer timeout: roost's 2 s drain + 2 s `exit_master` (an establishment was abandoned in phase 0, its roost cleanup ≤ 2 s run then) | **4 s** + a SIGKILLed child's reap |
//! | 4 | every `SshExec` master, in parallel, each clamped to ONE `MASTER_SHUTDOWN_BOUND` (5 s): `-O exit` 2 s + 0.2 s reap; the fallback — `-O check` 1 s (retried once), identity 2 × 0.2 s, SIGTERM wait 1 s, identity again 2 × 0.2 s — clamped to the bound LESS a 1.2 s reserve; and, for a master it cannot confirm, the retried `-O exit` its `Drop` would have sent (1 s + 0.2 s reap) in that reserve (R2-1, C3 round 4). Every runner is marked done, so no destructor runs afterwards, whatever N is | **5.0 s** |
//! | | **total** | **≈ 16 s**, 4 s inside the 20 s [`ESCAPE`] |
//!
//! **What a phase gives up on at its bound is logged in its own line** (plan
//! 026 AM4): spawn sections still open, roost clients still running, forward
//! reaps and pumps not finished, late children whose exit was not seen,
//! masters not confirmed. The guarantee holds for work that progresses within
//! the bounds; work a worker is descheduled through past every bound is
//! abandoned with the gate still closed — anything it spawns later is killed
//! at its registration — and the process exits.
//!
//! Typically well under 3 s. No phase is cancelled midway — there is no outer
//! timeout around the run — so roost's own teardown is never cut short, and
//! every destructor on the way is itself bounded. The one roost wait with no
//! bound of its own (a SIGKILLed `-O exit` that does not reap: only an
//! uninterruptible-sleep child) is bounded by the escape and recorded in plan
//! §9. **The roost clients are joined in phase 0, before phase 3 shuts the
//! tunnels down** (R10): roost's `SshTunnel` spawns an `ssh` per connection to
//! its bridge socket in a task of its own, and this app's watchers, probes and
//! admitted one-shots are the only clients of that socket.
//!
//! ## Idempotent, and started once
//!
//! [`Shutdown::start`] spawns the run on the app's runtime exactly once, guarded
//! by a `OnceLock`; every caller — the tray's `ExitRequested`, `RunEvent::Exit`,
//! the signal thread — waits for that one run to finish, and a caller that is
//! cancelled cancels only its own wait.

use std::sync::{Arc, OnceLock};
use std::time::{Duration, Instant};

use shed_app::spawn_gate::{SlotsClosed, SpawnGate};

use crate::lane::{Lanes, LanesClosed};
use crate::roost_hosts::RoostHosts;

/// How long closing the gate waits for the spawn sections in progress. Every
/// section is short — a `spawn()` and a registration — once the close abandons
/// a tunnel establishment (F3); one that is still open past this registers
/// late and kills its own child (F1).
pub const GATE_CLOSE_WAIT: Duration = Duration::from_millis(500);

/// Phase 0's bound for joining the roost clients and the admitted work — its
/// own, not what the gate's close left over (F2).
pub const CLIENTS_JOIN_WAIT: Duration = Duration::from_secs(1);

/// How long phase 2 waits for every registered async child's exit to be seen
/// after its kill.
pub const CHILDREN_KILL_WAIT: Duration = Duration::from_secs(2);

/// The signal path's escape: a shutdown not finished this long after the
/// first signal ends the process at once. Above the worst case, so it never
/// cuts a compliant shutdown short.
pub const ESCAPE: Duration = Duration::from_secs(20);

/// The embedded broker's drain ([`crate::broker::BrokerRuntime::signal_shutdown`]):
/// a non-blocking signal, run first.
pub type BrokerDrain = Box<dyn Fn() + Send + Sync>;

/// The app's one orderly shutdown: what it tears down, and whether it has.
pub struct Shutdown {
    gate: SpawnGate,
    hosts: Arc<RoostHosts>,
    lanes: Arc<Lanes>,
    broker: Option<BrokerDrain>,
    /// The runtime the run is spawned on — the app's own.
    handle: tokio::runtime::Handle,
    started: OnceLock<()>,
    done: tokio::sync::watch::Sender<bool>,
}

impl Shutdown {
    pub fn new(
        gate: SpawnGate,
        hosts: Arc<RoostHosts>,
        lanes: Arc<Lanes>,
        broker: Option<BrokerDrain>,
        handle: tokio::runtime::Handle,
    ) -> Arc<Shutdown> {
        Arc::new(Shutdown {
            gate,
            hosts,
            lanes,
            broker,
            handle,
            started: OnceLock::new(),
            done: tokio::sync::watch::channel(false).0,
        })
    }

    /// Start the run — on the app's runtime, the FIRST time only — and answer
    /// a receiver that reads `true` once it has finished. Never blocks, and
    /// safe from any thread (the signal thread starts it this way).
    pub fn start(self: &Arc<Self>) -> tokio::sync::watch::Receiver<bool> {
        let done = self.done.subscribe();
        if self.started.set(()).is_ok() {
            let this = Arc::clone(self);
            self.handle.spawn(async move {
                shutdown_children(&this.gate, &this.hosts, &this.lanes, this.broker.as_ref()).await;
                this.done.send_replace(true);
            });
        }
        done
    }

    /// [`Self::start`], and wait for the run to finish. A later caller waits
    /// for the first run; none starts a second.
    pub async fn run(self: &Arc<Self>) {
        let mut done = self.start();
        let _ = done.wait_for(|finished| *finished).await;
    }

    /// Whether the run has finished.
    #[cfg(test)]
    pub fn is_done(&self) -> bool {
        *self.done.borrow()
    }
}

/// What one run did, phase by phase — logged as it goes — and what each phase
/// abandoned at its bound (plan 026 AM4).
#[derive(Debug, Default)]
pub struct Report {
    pub gate_drained: bool,
    /// Spawn sections still open when the close's bound passed.
    pub sections_open_at_close: usize,
    pub clients: crate::roost_hosts::JoinedClients,
    pub lanes: LanesClosed,
    pub slots: SlotsClosed,
    pub crazes: usize,
    pub crazes_late: usize,
    pub children_killed: usize,
    pub children_exited: usize,
    /// Spawn sections still open when the drain's bound passed.
    pub sections_open_at_drain: usize,
    pub reaches: usize,
    pub masters: usize,
    /// Masters not confirmed ended, even after their retried `-O exit`.
    pub masters_unconfirmed: usize,
    /// Children killed at a registration after the close, as of the end of the
    /// run, and how many of those were never seen to exit.
    pub late_killed: usize,
    pub late_unseen: usize,
    pub elapsed: Duration,
}

/// **The shutdown itself** — the five phases, in order (the module doc).
pub async fn shutdown_children(
    gate: &SpawnGate,
    hosts: &RoostHosts,
    lanes: &Arc<Lanes>,
    broker: Option<&BrokerDrain>,
) -> Report {
    let started = Instant::now();
    let mut report = Report::default();

    // ---- phase 0: stop new work, then join what was admitted -------------
    let phase = Instant::now();
    if let Some(drain) = broker {
        drain();
    }
    // The entry points' closing flags first — clean refusals at the doors —
    // then the gate itself, the authority at every spawn site.
    hosts.begin_closing();
    lanes.begin_closing();
    report.gate_drained = gate.close(GATE_CLOSE_WAIT).await;
    report.sections_open_at_close = gate.sections();
    let joined = hosts.join_clients(CLIENTS_JOIN_WAIT).await;
    report.clients = joined;
    log(
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
    log(
        1,
        "lanes",
        phase,
        format_args!(
            "lanes={} forwards={} pumps={} slots={}; abandoned: forward reaps={} pumps={} \
             slot reaps={}",
            lanes_closed.lanes,
            lanes_closed.forwards,
            lanes_closed.pumps,
            slots.closed,
            lanes_closed.forwards_unsettled,
            lanes_closed.pumps_unjoined,
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
    log(
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
    log(
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
    log(
        4,
        "ssh masters",
        phase,
        format_args!("masters={masters}; abandoned: unconfirmed={unconfirmed}"),
    );

    let late_now = gate.late();
    report.late_killed = late_now.killed;
    report.late_unseen = late_now.unseen.len();
    report.elapsed = started.elapsed();
    eprintln!(
        "shed-desktop-tauri: shutdown: done in {} ms (late registrations so far: killed={}, \
         exit unseen={:?}; the gate stays closed)",
        report.elapsed.as_millis(),
        late_now.killed,
        late_now.unseen
    );
    report
}

/// Phase 1 on a thread of its own, joined by this future: [`Lanes::shutdown`]
/// kills and reaps `ssh` children, which must never happen on an async worker
/// — nor on the blocking pool, which the runtime may be winding down — and the
/// gate's std slots ([`SpawnGate::close_slots`]) are swept BESIDE it, on a
/// scoped thread of their own (F10: the two bounds overlap rather than add),
/// covering a forward the lane registry no longer reaches.
async fn lanes_phase(lanes: Arc<Lanes>, gate: SpawnGate) -> (LanesClosed, SlotsClosed) {
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
        eprintln!("shed-desktop-tauri: shutdown: could not start the lanes thread ({e})");
        return Default::default();
    }
    rx.await.unwrap_or_default()
}

fn log(n: u8, what: &str, since: Instant, detail: std::fmt::Arguments<'_>) {
    eprintln!(
        "shed-desktop-tauri: shutdown: phase {n} ({what}) took {} ms: {detail}",
        since.elapsed().as_millis()
    );
}
