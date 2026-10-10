//! **The spawn gate** (plan 026 §3.3, shed#389): what makes "after an orderly
//! exit has begun, no new child is spawned" a property of the spawn sites
//! themselves rather than of every entry point that might reach one.
//!
//! ## Why the entry points are not enough
//!
//! Work that was admitted before the exit began keeps running across its
//! awaits. A craze create holds a cloned source and its automatic retry redials
//! without asking anyone; a lane's resume redials; a forward's repair runs on a
//! blocking thread nobody can cancel. A closing flag checked at the door says
//! nothing about any of them. So the gate sits where the `spawn()` is.
//!
//! ## A write barrier, not a flag
//!
//! A flag read before a spawn is a check-then-act race: the close can land
//! between the read and the `spawn()`, and the child is born after the drain
//! that was meant to kill it. The gate is an `RwLock`-shaped barrier instead:
//!
//! * a **spawn section** ([`SpawnGate::enter`]) is the shared side. A spawn site
//!   holds one across check → `spawn()` → registering the child (and, for an
//!   async dial, across polling the dial future through its spawn);
//! * [`SpawnGate::close`] is the exclusive side. It marks the gate closed — so
//!   no section can be entered after it — and then waits, bounded, for every
//!   section already in progress to end.
//!
//! So a spawn either entered before the close and registered its child before
//! the closing flag was set (and the drain kills it), or it registers after —
//! and **a registration on a closed gate kills its own child at once and awaits
//! it** ([`SpawnGate::register_exit`]), mirroring a forward slot's closed store
//! — or it tried to enter after and got [`ShuttingDown`] (and spawned nothing).
//! The close's wait is bounded, so a section admitted before it can still be
//! running when it returns; the drain ([`SpawnGate::kill_exits`]) therefore
//! waits for every open section too, so the late child's self-kill has finished
//! before the drain does. Sections may nest (a dialer's section around an
//! `SshExec` spawn that takes its own): the count is a count, and a nested enter
//! after the close is simply refused.
//!
//! ## The registry holds kill-and-await handles, never a pid
//!
//! A child registered here is one the drain can end whoever else holds it: an
//! async child's [`ChildExit`] (a craze bridge's `ExitWatch`, an `SshExec`
//! step), or a std child's slot ([`SlotClose`], an `ssh -N -L` forward). Never a
//! bare pid: one that was reaped elsewhere can be recycled, and a signal sent by
//! number would then land on a stranger.
//!
//! **An entry leaves only once its child is gone**: an async child's when its
//! exit has been seen ([`ChildExit::done`]), a slot's when its last owner let
//! go (it is held weakly, and its own drop kills what it holds). So a child
//! whose holder dropped it a moment before the drain is still awaited by the
//! drain — its kill, sent by that drop, is otherwise a detached task racing the
//! process's exit. The entry holding a handle must therefore never be what
//! keeps a child alive: whoever registers an async child kills it explicitly
//! when its own holder lets go (a craze bridge's write half does).
//!
//! The gate is optional everywhere it is consulted (`None` by default): the
//! phone, which links this crate with default features, never builds one.

use std::fmt;
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::{Duration, Instant};

use futures::future::BoxFuture;

/// The error code a refused spawn answers with, in the IPC's snake_case family.
pub const SHUTTING_DOWN: &str = "shutting_down";

/// How long a registration on a closed gate waits for the child it killed to
/// be seen to exit.
pub const LATE_KILL_WAIT: Duration = Duration::from_secs(1);

/// A spawn refused because the gate has closed: the app is quitting.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ShuttingDown;

impl fmt::Display for ShuttingDown {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{SHUTTING_DOWN}: the app is quitting, so nothing new is started"
        )
    }
}

impl std::error::Error for ShuttingDown {}

/// An async child the drain can end: kill it, then wait (bounded) for its exit
/// to be seen.
pub trait ChildExit: Send + Sync {
    /// Kill it now. Cheap and non-blocking: a signal, or a message to whatever
    /// task owns the child.
    fn kill(&self);

    /// Resolve once its exit has been seen — `true` — or after `within` with
    /// `false`.
    fn exited(&self, within: Duration) -> BoxFuture<'static, bool>;

    /// Whether its exit has been seen already — when its entry may leave the
    /// registry.
    fn done(&self) -> bool;

    /// Its pid, when the handle knows it — what a loud log of a child whose
    /// exit was never seen names.
    fn pid(&self) -> Option<u32> {
        None
    }
}

/// A std child's slot the drain can close: take the child, kill it and reap it,
/// bounded. **Blocking** — never called on an async worker. Answers whether it
/// SETTLED: `false` when a kill another thread has in progress had not
/// finished its reap by the bound (its child already SIGKILLed — a slot sends
/// the signal before the child leaves it).
pub trait SlotClose: Send + Sync {
    fn close(&self) -> bool;
}

/// What [`SpawnGate::kill_exits`] did — and what it abandoned at its bound
/// (plan 026 AM4).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Drained {
    /// Registered children it killed.
    pub killed: usize,
    /// Of those, the ones whose exit it saw.
    pub exited: usize,
    /// Spawn sections still open when it returned — admitted before the close
    /// and not finished by the bound. Each one's child, if it ever spawns, is
    /// killed at its registration ([`SpawnGate::register_exit`]).
    pub sections_open: usize,
}

/// What [`SpawnGate::close_slots`] did.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct SlotsClosed {
    /// Live slots it closed.
    pub closed: usize,
    /// Of those, the ones that had not settled by their bound (a kill in
    /// progress on another thread, its child already SIGKILLed, its reap
    /// abandoned — plan 026 AM4).
    pub unsettled: usize,
}

/// The registrations that came after the close ([`SpawnGate::register_exit`]):
/// each one's child was killed there.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct LateRegistrations {
    /// How many children were killed at a late registration.
    pub killed: usize,
    /// The pids (where known) of those whose exit was NOT seen within
    /// [`LATE_KILL_WAIT`] — SIGKILLed, and abandoned (plan 026 AM4).
    pub unseen: Vec<Option<u32>>,
}

/// The gate. Cheap to clone; every clone is the same gate.
#[derive(Clone, Default)]
pub struct SpawnGate {
    inner: Arc<Inner>,
}

#[derive(Default)]
struct Inner {
    state: Mutex<State>,
    /// Woken whenever the last open section ends.
    drained: tokio::sync::Notify,
    /// Woken when [`SpawnGate::close`] begins ([`SpawnGate::closing`]).
    closing: tokio::sync::Notify,
}

#[derive(Default)]
struct State {
    closed: bool,
    /// When the closing flag was SET — the instant from which no section can
    /// be entered and no child can be registered alive.
    closing_at: Option<Instant>,
    /// When the barrier passed: the instant [`SpawnGate::close`] saw no
    /// section left (or gave up waiting for one). Every spawn the gate allowed
    /// happened before it.
    closed_at: Option<Instant>,
    /// Sections in progress.
    sections: usize,
    exits: Vec<Arc<dyn ChildExit>>,
    slots: Vec<Weak<dyn SlotClose>>,
    /// Children killed at a registration after the close.
    late: LateRegistrations,
}

impl State {
    /// Let go of every entry whose child is gone.
    fn prune(&mut self) {
        self.exits.retain(|child| !child.done());
        self.slots.retain(|slot| slot.strong_count() > 0);
    }
}

fn lock(m: &Mutex<State>) -> MutexGuard<'_, State> {
    m.lock().unwrap_or_else(|p| p.into_inner())
}

/// A registered child's identity — its handle's address, for the drain's
/// "already swept" set.
fn identity(child: &Arc<dyn ChildExit>) -> usize {
    Arc::as_ptr(child) as *const () as usize
}

/// One spawn section: the gate's shared guard. Held across check → spawn →
/// registration; the gate cannot finish closing while one is alive.
#[must_use = "a section that is dropped at once protects nothing"]
pub struct SpawnSection {
    inner: Arc<Inner>,
}

impl Drop for SpawnSection {
    fn drop(&mut self) {
        let wake = {
            let mut state = lock(&self.inner.state);
            state.sections = state.sections.saturating_sub(1);
            state.sections == 0
        };
        if wake {
            self.inner.drained.notify_waiters();
        }
    }
}

impl SpawnGate {
    /// A fresh, open gate.
    pub fn new() -> SpawnGate {
        SpawnGate::default()
    }

    /// Enter a spawn section, or [`ShuttingDown`] once the gate has closed.
    pub fn enter(&self) -> Result<SpawnSection, ShuttingDown> {
        let mut state = lock(&self.inner.state);
        if state.closed {
            return Err(ShuttingDown);
        }
        state.sections += 1;
        Ok(SpawnSection {
            inner: Arc::clone(&self.inner),
        })
    }

    /// Whether [`Self::close`] has begun. A hint for a clean early refusal; the
    /// authority is [`Self::enter`].
    pub fn is_closed(&self) -> bool {
        lock(&self.inner.state).closed
    }

    /// When the barrier passed, once it has ([`State::closed_at`]).
    pub fn closed_at(&self) -> Option<Instant> {
        lock(&self.inner.state).closed_at
    }

    /// When the closing flag was set ([`State::closing_at`]): every child
    /// registered alive was spawned before it.
    pub fn closing_at(&self) -> Option<Instant> {
        lock(&self.inner.state).closing_at
    }

    /// How many spawn sections are in progress.
    pub fn sections(&self) -> usize {
        lock(&self.inner.state).sections
    }

    /// Resolve once [`Self::close`] has begun — what a long spawn (a tunnel's
    /// establishment) selects against, so it is abandoned rather than waited
    /// for.
    pub async fn closing(&self) {
        loop {
            let notified = self.inner.closing.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if lock(&self.inner.state).closed {
                return;
            }
            notified.await;
        }
    }

    /// Close the gate — no section can be entered after this begins — and wait
    /// at most `within` for every section in progress to end. Answers whether
    /// they all did. Idempotent: a later call waits the same way and changes
    /// nothing else.
    pub async fn close(&self, within: Duration) -> bool {
        {
            let mut state = lock(&self.inner.state);
            state.closed = true;
            state.closing_at.get_or_insert_with(Instant::now);
        }
        self.inner.closing.notify_waiters();
        let deadline = tokio::time::Instant::now() + within;
        loop {
            // Interest registered BEFORE the count is read, so a section ending
            // between the read and the wait still wakes this one.
            let notified = self.inner.drained.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            {
                let mut state = lock(&self.inner.state);
                if state.sections == 0 {
                    state.closed_at.get_or_insert_with(Instant::now);
                    return true;
                }
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                lock(&self.inner.state)
                    .closed_at
                    .get_or_insert_with(Instant::now);
                return false;
            }
        }
    }

    /// Register an async child spawned inside `section`. The entry stays until
    /// the child's exit has been seen.
    ///
    /// **On a closed gate the child is killed here, at once, and awaited**
    /// (bounded by [`LATE_KILL_WAIT`]), and the answer is [`ShuttingDown`]: a
    /// section admitted before the close whose spawn came after it — the close
    /// gave up waiting, or is still waiting — must not leave a child the drain
    /// has already swept past. The check and the push are one critical section
    /// with the close's flag, so a child is either in the list the drain reads
    /// or kills itself; the caller still holds `section`, so the drain (which
    /// waits for sections) waits for that kill too.
    pub async fn register_exit(
        &self,
        section: &SpawnSection,
        child: Arc<dyn ChildExit>,
    ) -> Result<(), ShuttingDown> {
        debug_assert!(Arc::ptr_eq(&section.inner, &self.inner));
        {
            let mut state = lock(&self.inner.state);
            if !state.closed {
                state.prune();
                state.exits.push(child);
                return Ok(());
            }
        }
        let pid = child.pid();
        child.kill();
        let seen = child.exited(LATE_KILL_WAIT).await;
        {
            let mut state = lock(&self.inner.state);
            state.late.killed += 1;
            if !seen {
                state.late.unseen.push(pid);
            }
        }
        if !seen {
            // Loud, and counted ([`Self::late`]): a child this gate killed and
            // could not see go is the one thing here that may outlive the app.
            tracing::error!(
                pid = ?pid,
                "spawn gate: a child registered after the close was killed, but its exit was \
                 not seen within {LATE_KILL_WAIT:?}; abandoned"
            );
        }
        Err(ShuttingDown)
    }

    /// The registrations that came after the close, so far.
    pub fn late(&self) -> LateRegistrations {
        lock(&self.inner.state).late.clone()
    }

    /// Register a std child's slot, inside `section`. Held weakly: a slot whose
    /// owner let go has already killed what it held.
    ///
    /// **On a closed gate it is refused** ([`ShuttingDown`]) and not kept: the
    /// caller — which holds the slot's child and its section — closes the slot
    /// itself, at once (a blocking kill and reap, on the spawn's own blocking
    /// thread). [`Self::register_exit`]'s rule, for a std child.
    pub fn register_slot(
        &self,
        section: &SpawnSection,
        slot: Weak<dyn SlotClose>,
    ) -> Result<(), ShuttingDown> {
        debug_assert!(Arc::ptr_eq(&section.inner, &self.inner));
        let mut state = lock(&self.inner.state);
        if state.closed {
            return Err(ShuttingDown);
        }
        state.prune();
        state.slots.push(slot);
        Ok(())
    }

    /// How many async children are registered and not yet seen to exit.
    pub fn registered_exits(&self) -> usize {
        let mut state = lock(&self.inner.state);
        state.prune();
        state.exits.len()
    }

    /// Kill every registered async child, then wait — together, at most
    /// `within` — for each exit to be seen. Answers what it killed, saw exit,
    /// and left open ([`Drained`]).
    ///
    /// Independent of who holds the child: a lane verb keeping its connection
    /// across a 30 s request cannot keep its bridge alive through this.
    ///
    /// **Not one snapshot.** After each sweep it waits, inside the same bound,
    /// for every spawn section still open — a section admitted before the close
    /// that has not spawned yet registers late, and that registration kills its
    /// own child before the section ends ([`Self::register_exit`]) — and then
    /// sweeps again for anything registered since. It returns once no section
    /// is open and nothing unswept is left, or at the bound — and then says how
    /// many sections it abandoned open (plan 026 AM4: a section descheduled
    /// past every bound still kills its own child at registration, because the
    /// gate stays closed).
    pub async fn kill_exits(&self, within: Duration) -> Drained {
        let deadline = tokio::time::Instant::now() + within;
        let mut swept: std::collections::HashSet<usize> = std::collections::HashSet::new();
        let (mut killed, mut exited) = (0, 0);
        loop {
            let fresh: Vec<Arc<dyn ChildExit>> = {
                let mut state = lock(&self.inner.state);
                state.prune();
                state
                    .exits
                    .iter()
                    .filter(|child| !swept.contains(&identity(child)))
                    .cloned()
                    .collect()
            };
            for child in &fresh {
                swept.insert(identity(child));
                child.kill();
            }
            let left = deadline.saturating_duration_since(tokio::time::Instant::now());
            let seen = futures::future::join_all(fresh.iter().map(|c| c.exited(left))).await;
            killed += fresh.len();
            exited += seen.into_iter().filter(|exited| *exited).count();
            let drained = self.sections_ended(deadline).await;
            let unswept = {
                let mut state = lock(&self.inner.state);
                state.prune();
                state
                    .exits
                    .iter()
                    .any(|child| !swept.contains(&identity(child)))
            };
            if !unswept && (drained || tokio::time::Instant::now() >= deadline) {
                return self.drained_report(killed, exited);
            }
            if tokio::time::Instant::now() >= deadline {
                return self.drained_report(killed, exited);
            }
        }
    }

    fn drained_report(&self, killed: usize, exited: usize) -> Drained {
        let sections_open = lock(&self.inner.state).sections;
        if sections_open > 0 {
            tracing::warn!(
                sections_open,
                "spawn gate: the drain's bound passed with spawn sections still open; abandoned \
                 (a late child is killed at its registration)"
            );
        }
        Drained {
            killed,
            exited,
            sections_open,
        }
    }

    /// Wait until no spawn section is open, or `deadline`; whether none is.
    async fn sections_ended(&self, deadline: tokio::time::Instant) -> bool {
        loop {
            let notified = self.inner.drained.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if lock(&self.inner.state).sections == 0 {
                return true;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return lock(&self.inner.state).sections == 0;
            }
        }
    }

    /// Close every registered std child's slot, all at once on threads of their
    /// own, each bounded by its own reap. **Blocking** — run it on a thread of
    /// its own, never an async worker. Answers how many were still there, and
    /// how many of those had not settled by their bound ([`SlotsClosed`]).
    pub fn close_slots(&self) -> SlotsClosed {
        let slots: Vec<Arc<dyn SlotClose>> = {
            let mut state = lock(&self.inner.state);
            state.prune();
            state.slots.iter().filter_map(Weak::upgrade).collect()
        };
        // Every close is started before any is joined: they run side by side.
        let settled: Vec<bool> = std::thread::scope(|scope| {
            let mut closes = Vec::with_capacity(slots.len());
            for slot in &slots {
                closes.push(scope.spawn(move || slot.close()));
            }
            closes
                .into_iter()
                .map(|close| close.join().unwrap_or(false))
                .collect()
        });
        SlotsClosed {
            closed: slots.len(),
            unsettled: settled.iter().filter(|settled| !**settled).count(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    #[test]
    fn a_closed_gate_refuses_every_section() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("an open gate admits a section");
        drop(section);
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
            .expect("rt");
        assert!(rt.block_on(gate.close(Duration::from_millis(50))));
        assert!(gate.is_closed());
        assert_eq!(gate.enter().err(), Some(ShuttingDown));
        assert!(
            ShuttingDown.to_string().starts_with(SHUTTING_DOWN),
            "the refusal leads with its code: {ShuttingDown}"
        );
    }

    /// **The barrier**: a close waits for a section in progress, and the section
    /// may still register what it spawned; nothing enters after.
    #[tokio::test]
    async fn close_waits_for_a_section_in_progress() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("open");
        let closing = tokio::spawn({
            let gate = gate.clone();
            async move { gate.close(Duration::from_secs(10)).await }
        });
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!closing.is_finished(), "close returned with a section open");
        assert!(gate.enter().is_err(), "a section entered after close began");
        assert!(gate.closed_at().is_none(), "the barrier has not passed yet");
        drop(section);
        assert!(closing.await.expect("close"), "every section ended in time");
        assert!(gate.closed_at().is_some());
    }

    #[tokio::test]
    async fn close_is_bounded_by_a_section_that_never_ends() {
        let gate = SpawnGate::new();
        let _stuck = gate.enter().expect("open");
        let started = Instant::now();
        assert!(!gate.close(Duration::from_millis(100)).await);
        assert!(started.elapsed() < Duration::from_secs(2));
        assert!(gate.enter().is_err(), "still closed after the bound");
    }

    #[test]
    fn sections_nest() {
        let gate = SpawnGate::new();
        let outer = gate.enter().expect("outer");
        let inner = gate
            .enter()
            .expect("a nested section is admitted while open");
        drop(inner);
        drop(outer);
    }

    struct Child {
        killed: AtomicBool,
        exits: bool,
        done: AtomicBool,
        pid: Option<u32>,
    }

    impl Child {
        fn new(exits: bool) -> Arc<Child> {
            Arc::new(Child {
                killed: AtomicBool::new(false),
                exits,
                done: AtomicBool::new(false),
                pid: None,
            })
        }

        /// A child that never exits, with a pid to name.
        fn stuck(pid: u32) -> Arc<Child> {
            Arc::new(Child {
                killed: AtomicBool::new(false),
                exits: false,
                done: AtomicBool::new(false),
                pid: Some(pid),
            })
        }
    }

    impl ChildExit for Child {
        fn kill(&self) {
            self.killed.store(true, Ordering::SeqCst);
        }

        fn exited(&self, within: Duration) -> BoxFuture<'static, bool> {
            let exits = self.exits;
            Box::pin(async move {
                if !exits {
                    tokio::time::sleep(within).await;
                }
                exits
            })
        }

        fn done(&self) -> bool {
            self.done.load(Ordering::SeqCst)
        }

        fn pid(&self) -> Option<u32> {
            self.pid
        }
    }

    #[tokio::test]
    async fn the_drain_kills_every_registered_child_and_a_finished_one_leaves() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("open");
        let a = Child::new(true);
        let b = Child::new(false);
        let gone = Child::new(true);
        gate.register_exit(&section, a.clone()).await.expect("open");
        gate.register_exit(&section, b.clone()).await.expect("open");
        gate.register_exit(&section, gone.clone())
            .await
            .expect("open");
        drop(section);
        assert_eq!(gate.registered_exits(), 3);
        gone.done.store(true, Ordering::SeqCst);
        assert_eq!(gate.registered_exits(), 2, "a child seen to exit leaves");

        let started = Instant::now();
        let drained = gate.kill_exits(Duration::from_millis(200)).await;
        assert_eq!(
            drained,
            Drained {
                killed: 2,
                exited: 1,
                sections_open: 0
            }
        );
        assert!(started.elapsed() < Duration::from_secs(2), "bounded");
        assert!(a.killed.load(Ordering::SeqCst) && b.killed.load(Ordering::SeqCst));
        assert!(!gone.killed.load(Ordering::SeqCst));
    }

    struct Slot(AtomicUsize, bool);

    impl Slot {
        fn new() -> Arc<Slot> {
            Arc::new(Slot(AtomicUsize::new(0), true))
        }
    }

    impl SlotClose for Slot {
        fn close(&self) -> bool {
            self.0.fetch_add(1, Ordering::SeqCst);
            self.1
        }
    }

    /// Every live slot is closed, a dropped one skipped — and one that did not
    /// settle by its bound is counted (plan 026 AM4).
    #[test]
    fn close_slots_closes_each_live_slot_and_skips_a_dropped_one() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("open");
        let live = Slot::new();
        let unsettled = Arc::new(Slot(AtomicUsize::new(0), false));
        let dead = Slot::new();
        for slot in [&live, &unsettled, &dead] {
            let weak: Weak<dyn SlotClose> = Arc::downgrade(slot) as Weak<Slot>;
            gate.register_slot(&section, weak).expect("open");
        }
        drop(section);
        drop(dead);
        assert_eq!(
            gate.close_slots(),
            SlotsClosed {
                closed: 2,
                unsettled: 1
            }
        );
        assert_eq!(live.0.load(Ordering::SeqCst), 1);
        assert_eq!(unsettled.0.load(Ordering::SeqCst), 1);
    }

    /// **A registration on a closed gate kills its own child, awaits it, and is
    /// refused** (plan 026 §3.3, F1): a section admitted before the close whose
    /// child came after it leaves nothing for a drain that already swept.
    #[tokio::test]
    async fn a_registration_on_a_closed_gate_kills_its_child_and_is_refused() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("admitted before the close");
        assert!(
            !gate.close(Duration::from_millis(20)).await,
            "the section is still open"
        );
        let late = Child::new(true);
        let refused = gate.register_exit(&section, late.clone()).await;
        assert_eq!(refused, Err(ShuttingDown));
        assert!(
            late.killed.load(Ordering::SeqCst),
            "the late child was killed at registration"
        );
        assert_eq!(gate.registered_exits(), 0, "nothing was kept");
        assert_eq!(
            gate.late(),
            LateRegistrations {
                killed: 1,
                unseen: Vec::new()
            },
            "a late kill whose exit was seen is counted, and not as unseen"
        );
        let slot = Slot::new();
        let weak: Weak<dyn SlotClose> = Arc::downgrade(&slot) as Weak<Slot>;
        assert_eq!(gate.register_slot(&section, weak), Err(ShuttingDown));
        drop(section);
    }

    /// **A late kill whose exit is NOT seen is checked, counted and named**
    /// (plan 026 §3.3, R2-4): killed, awaited for [`LATE_KILL_WAIT`], then
    /// recorded with its pid — never treated as an exit.
    #[tokio::test]
    async fn a_late_kill_whose_exit_is_unseen_is_counted_with_its_pid() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("admitted before the close");
        assert!(!gate.close(Duration::from_millis(20)).await);
        let stuck = Child::stuck(4242);
        let started = Instant::now();
        assert_eq!(
            gate.register_exit(&section, stuck.clone()).await,
            Err(ShuttingDown)
        );
        assert!(stuck.killed.load(Ordering::SeqCst), "it was killed");
        assert!(
            started.elapsed() >= LATE_KILL_WAIT,
            "its exit was awaited for the full bound"
        );
        assert_eq!(
            gate.late(),
            LateRegistrations {
                killed: 1,
                unseen: vec![Some(4242)]
            }
        );
        drop(section);
    }

    /// **A section still open at the drain's bound is reported, not waited
    /// for** (plan 026 AM4) — and the gate stays closed, so when that section
    /// finally registers its child, the child is killed there.
    #[tokio::test]
    async fn the_drain_reports_a_section_it_abandoned_and_the_late_child_is_still_killed() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("admitted before the close");
        assert!(!gate.close(Duration::from_millis(20)).await);
        let drained = gate.kill_exits(Duration::from_millis(100)).await;
        assert_eq!(
            drained.sections_open, 1,
            "the abandoned section is reported"
        );
        assert!(gate.is_closed(), "the gate stays closed");
        let late = Child::new(true);
        assert_eq!(
            gate.register_exit(&section, late.clone()).await,
            Err(ShuttingDown)
        );
        assert!(
            late.killed.load(Ordering::SeqCst),
            "the late child was killed at registration, after the drain"
        );
        drop(section);
    }

    /// **The drain waits for a section still open after the close gave up**,
    /// and returns only once it has ended — so a late child's self-kill lands
    /// before the drain does (F1).
    #[tokio::test]
    async fn the_drain_waits_for_a_section_the_close_gave_up_on() {
        let gate = SpawnGate::new();
        let section = gate.enter().expect("open");
        assert!(!gate.close(Duration::from_millis(20)).await);
        assert!(
            gate.closing_at().is_some(),
            "the closing flag's instant is recorded"
        );
        let draining = tokio::spawn({
            let gate = gate.clone();
            async move { gate.kill_exits(Duration::from_secs(5)).await }
        });
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert!(
            !draining.is_finished(),
            "the drain returned with a section still open"
        );
        let late = Child::new(true);
        assert!(gate.register_exit(&section, late.clone()).await.is_err());
        drop(section);
        let started = Instant::now();
        let drained = tokio::time::timeout(Duration::from_secs(2), draining)
            .await
            .expect("the drain returned once the section ended")
            .expect("the drain");
        assert!(started.elapsed() < Duration::from_secs(1));
        assert_eq!(drained.sections_open, 0, "no section was abandoned");
        assert!(late.killed.load(Ordering::SeqCst));
    }

    #[tokio::test]
    async fn closing_resolves_once_the_close_begins() {
        let gate = SpawnGate::new();
        let waiting = tokio::spawn({
            let gate = gate.clone();
            async move { gate.closing().await }
        });
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert!(!waiting.is_finished());
        let _held = gate.enter().expect("open");
        let closing = tokio::spawn({
            let gate = gate.clone();
            async move { gate.close(Duration::from_millis(200)).await }
        });
        tokio::time::timeout(Duration::from_secs(1), waiting)
            .await
            .expect("closing() resolved while the close still waits")
            .expect("the waiter");
        drop(_held);
        assert!(closing.await.expect("close"));
    }
}
