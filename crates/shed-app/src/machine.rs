//! **The machine transport seam** (plan 012, roadmap R4).
//!
//! `machines:` has existed in [`shed_core::config`] since plan 009, but for two
//! plans the only thing that read it was the `sx` porcelain. This module is the
//! shared half the clients consume: the pure addressing lives in
//! [`shed_core::machine`], and what is left — the part that genuinely differs
//! per client — is exactly one thing.
//!
//! The long-lived hub watcher that used to sit on top of this seam went with the
//! RC hub in plan 022 (S6, `charliek/shed#328`), and `shed_core`'s hub wire
//! module went with it. What consumes the seam today is the agent lane: the
//! roost watcher ([`crate::roost`]) and the desktop's per-session opencode
//! forwards.
//!
//! ## The seam is a local port, and nothing above it is per-client
//!
//! What a client needs to reach is a loopback port on the FAR side — an agent's
//! HTTP server, a `roost-session` socket — so every client needs some way to get
//! a local socket that proxies there. That is the whole of the difference:
//!
//! | client | how it gets the port |
//! |---|---|
//! | Tauri | [`SshForward`] — an `ssh -N -L` child process |
//! | shed-mobile | a `dartssh2` local-forward bridge on the Dart side; Rust is handed the port ([`FixedPort`]) |
//!
//! Everything above the port — health probing, the snapshot, the event feed,
//! reconnect/backoff/resync — is shared, which is why a watcher takes a
//! `dyn MachineForward` and never learns which kind it has.
//!
//! Note mobile does NOT implement this trait from Dart: it stands the bridge up
//! itself and passes the resulting `u16` into [`FixedPort`]. Rust never calls
//! into Dart, matching the inverted shape shed-mobile already uses for one-shot
//! remote exec (Rust builds argv, Dart runs it, Rust decodes).
//!
//! ## The port is STABLE across a re-establish — the load-bearing invariant
//!
//! [`MachineForward::ensure`] may rebuild a dead forward, but it must never
//! change [`MachineForward::port`]. That is what lets reconnect be shared: "the
//! tunnel died" becomes "the socket refused", and recovery is redialing the same
//! address — identical for a respawned `ssh -N -L` child and for a Dart bridge
//! whose `SSHClient` dropped when the phone changed networks. Without it, every
//! client would need its own re-acquire protocol and the watcher could not be
//! written once.

use std::sync::{Arc, Condvar, Mutex, Weak};
use std::time::{Duration, Instant};

use shed_core::machine;

use crate::spawn_gate::{SlotClose, SpawnGate, SpawnSection, SHUTTING_DOWN};

/// An arbitrary far-side loopback port for the tests (and the faked-`ssh` seam)
/// that only care that a forward carries whatever port it was handed. It was
/// the RC hub's fixed `1029` until S6 (`charliek/shed#328`) removed the last
/// caller with a constant far side.
#[cfg(test)]
const SOME_REMOTE_PORT: u16 = 1029;

/// How long to wait for a freshly-established forward's local end to answer.
const FORWARD_READY_TIMEOUT: Duration = Duration::from_secs(10);

/// How long a killed forward child is waited for (`try_wait` polling) before
/// it is left to the kernel — plan 026 §3.3's bound on every reap, so no
/// teardown and no destructor can block an exit on a `waitpid` that does not
/// return.
const REAP_BOUND: Duration = Duration::from_secs(1);

/// How long [`MachineForward::close_now`] waits for an `ensure` already in
/// flight on the blocking pool to notice the closed slot and return (plan 026
/// §3.3: ≤ 0.5 s; its readiness loop re-checks every [`READY_POLL`]).
const IN_FLIGHT_WAIT: Duration = Duration::from_millis(500);

/// The readiness loop's poll interval, and the local probe's connect budget.
const READY_POLL: Duration = Duration::from_millis(100);

/// A forward could not be established.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ForwardError(pub String);

impl std::fmt::Display for ForwardError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ForwardError {}

/// A live local port proxying to a machine's `127.0.0.1:1029`.
///
/// **Contract:** [`port`] is fixed for the lifetime of the value; [`ensure`] is
/// idempotent and re-establishes the underlying transport if it died, WITHOUT
/// changing the port. See the module doc for why that matters.
///
/// [`port`]: MachineForward::port
/// [`ensure`]: MachineForward::ensure
#[async_trait::async_trait]
pub trait MachineForward: Send + Sync {
    /// The stable local port.
    fn port(&self) -> u16;

    /// Make the forward usable, rebuilding it if necessary.
    async fn ensure(&self) -> Result<(), ForwardError>;

    /// A CHEAP "is this still up?" — no network, no blocking syscall, safe to
    /// call on an async worker as often as a caller likes.
    ///
    /// It exists because [`ensure`] is NOT that. `ensure` is the authoritative
    /// answer and it pays for one: it serializes on a lock and probes the local
    /// port. A caller that re-checks before every verb on a live lane (plan
    /// 017's now-retired `TauriTransport` was shaped exactly this way, invoked
    /// before every gx request) would otherwise turn a per-verb hook into a
    /// per-verb connect.
    ///
    /// **`false` means "definitely ask `ensure`"; `true` means "probably fine".**
    /// It is a proxy, not a proof: [`SshForward`] answers it from whether the
    /// `ssh` child is still running, which `ExitOnForwardFailure=yes` makes a
    /// good one (a forward that breaks takes the child with it) but not a
    /// complete one (a child that lives on without listening reads as `true`).
    /// A caller must therefore still have SOME path that calls `ensure`; this
    /// only spares it from calling it every time.
    ///
    /// The default is `true`, which is right for a forward this side does not
    /// own ([`FixedPort`]): there is nothing here to check.
    ///
    /// [`ensure`]: MachineForward::ensure
    fn looks_alive(&self) -> bool {
        true
    }

    /// **Close this forward now, whoever else still holds it** (plan 026
    /// §3.3): kill and reap whatever child it owns, and refuse every later
    /// `ensure`. The app's orderly shutdown calls it on every forward it can
    /// reach, because dropping one is not enough — the last reference can sit
    /// in an aborted pump's future or a detached thread.
    ///
    /// **Blocking and bounded**: a kill, a reap bounded by its own deadline,
    /// and a bounded wait for an `ensure` in flight. Never call it on an async
    /// worker. Answers whether it SETTLED within those bounds: `false` when a
    /// reap or an `ensure` was still running at its bound (its child already
    /// SIGKILLed) and was abandoned (plan 026 AM4), for the shutdown's log.
    ///
    /// The default does nothing and answers `true`, which is right for a
    /// forward this side does not own ([`FixedPort`]) and keeps every other
    /// implementor compiling.
    fn close_now(&self) -> bool {
        true
    }
}

/// A forward someone else owns — the caller has already arranged that `port`
/// proxies to the machine's hub and is responsible for keeping it that way.
///
/// This is shed-mobile's implementation: Dart listens on a loopback port and
/// bridges each accepted connection onto a fresh `dartssh2` local-forward
/// channel, re-dialing the SSH connection underneath as needed. From Rust's side
/// the port simply keeps working, so [`ensure`] has nothing to do.
///
/// [`ensure`]: MachineForward::ensure
pub struct FixedPort(pub u16);

#[async_trait::async_trait]
impl MachineForward for FixedPort {
    fn port(&self) -> u16 {
        self.0
    }

    async fn ensure(&self) -> Result<(), ForwardError> {
        Ok(())
    }
}

/// An `ssh -N -L <port>:127.0.0.1:<remote> <machine>` child process — the
/// desktop implementation.
///
/// The child is killed and reaped on drop, so an early return or a Ctrl-C can
/// never leave a forward running. `ensure` respawns onto the SAME local port
/// when the child has exited; `ExitOnForwardFailure=yes` in the argv means a
/// lost race for that port is an immediate visible failure rather than a tunnel
/// that silently forwards nothing.
///
/// The REMOTE port is per-forward rather than a constant. It was the RC hub's
/// fixed `1029` for as long as the hub was the only thing on the far side; plan
/// 015's opencode lane forwards a loopback port an agent chose and roost
/// reported, which is a different number per session and cannot be known at
/// compile time — and with the hub deleted in plan 022 (S6,
/// `charliek/shed#328`) there is no constant left to default to. Every caller
/// names the far side through [`SshForward::reserve_for`].
pub struct SshForward {
    entry: shed_core::config::MachineEntry,
    port: u16,
    /// The far-side loopback port this tunnel lands on. Fixed for the life of
    /// the value, like [`SshForward::port`] — a forward that re-pointed itself
    /// would break the stable-address invariant the module doc pins.
    remote_port: u16,
    /// The label the tunnel is described by in errors — the machine's NAME,
    /// not its `user@host`, so a message reads in the same vocabulary as the
    /// `--on machine:<name>` the user typed.
    label: String,
    /// The live child. An `Arc` because the blocking spawn task stores the
    /// child itself the instant it exists (see [`MachineForward::ensure`]).
    child: Arc<ChildSlot>,
    /// Serializes `ensure()`. The trait is `Send + Sync` and consumers hold an
    /// `Arc<dyn MachineForward>`, so two callers can race; without this both
    /// would spawn onto the same local port and the loser (killed by
    /// `ExitOnForwardFailure`) could be the one we keep.
    ensuring: tokio::sync::Mutex<()>,
    /// The app's spawn gate (plan 026 §3.3), when it has one: every spawn of
    /// this forward runs inside one of its sections, and the slot is registered
    /// with it so the drain can close it. `None` by default.
    gate: Option<SpawnGate>,
    /// The `ensure`s whose blocking half is running — what
    /// [`MachineForward::close_now`] waits (bounded) to see end.
    in_flight: Arc<InFlight>,
    /// **Test seam** (see [`SshForward::reserve_faked`]): an argv prefix exec'd
    /// in place of `ssh`, with the real ssh argv handed to it as ignored
    /// trailing arguments. Compiled away outside `cfg(test)`.
    #[cfg(test)]
    exec_prefix: Option<Vec<String>>,
}

impl SshForward {
    /// Reserve a local port forwarding to `remote_port` on the machine's
    /// loopback. Nothing is spawned until [`MachineForward::ensure`] runs.
    ///
    /// The local port is grabbed the cheap way: bind `:0`, read the assignment,
    /// release. Racy in principle — and deliberately so, because the
    /// alternative (holding the socket) is what would prevent ssh from binding
    /// it at all.
    ///
    /// This is the agent-lane door (plan 015 §3.4; opencode today, gx until
    /// plan 025 C1 retired it): the agent's HTTP server binds an ephemeral
    /// loopback port, roost reports it as `server_url`, and the desktop needs
    /// a local socket that lands on exactly that one.
    pub fn reserve_for(
        entry: shed_core::config::MachineEntry,
        remote_port: u16,
    ) -> Result<Self, ForwardError> {
        let port = free_loopback_port()
            .map_err(|e| ForwardError(format!("allocating a local forward port: {e}")))?;
        let label = format!("machine:{}", entry.name);
        Ok(Self {
            entry,
            port,
            remote_port,
            label,
            child: Arc::new(ChildSlot::default()),
            ensuring: tokio::sync::Mutex::new(()),
            gate: None,
            in_flight: Arc::new(InFlight::default()),
            #[cfg(test)]
            exec_prefix: None,
        })
    }

    /// Spawn only inside `gate`'s sections, and register the slot with it
    /// (plan 026 §3.3). A forward without one — the default — spawns freely.
    pub fn with_spawn_gate(mut self, gate: SpawnGate) -> Self {
        self.gate = Some(gate);
        self
    }

    /// **Test-only:** a forward that execs `exec_prefix` (plus the real ssh
    /// argv, ignored) instead of `ssh`.
    ///
    /// The child LIFECYCLE — recorded the instant it exists, killed on drop,
    /// never spawned twice onto one port — is the part of this type that has
    /// actually had bugs, and real `ssh` cannot exercise it without a live
    /// machine (which would make the tests both slow and conditional). A
    /// scriptable stand-in is spawned, recorded, waited on, and killed through
    /// exactly the same code, so the lifecycle claims are testable hermetically.
    #[cfg(test)]
    fn reserve_faked(
        entry: shed_core::config::MachineEntry,
        exec_prefix: Vec<String>,
    ) -> Result<Self, ForwardError> {
        Self::reserve_faked_for(entry, SOME_REMOTE_PORT, exec_prefix)
    }

    /// [`SshForward::reserve_faked`] against an arbitrary far-side port — the
    /// lane's shape ([`SshForward::reserve_for`]) under the same stand-in.
    #[cfg(test)]
    fn reserve_faked_for(
        entry: shed_core::config::MachineEntry,
        remote_port: u16,
        exec_prefix: Vec<String>,
    ) -> Result<Self, ForwardError> {
        let mut f = Self::reserve_for(entry, remote_port)?;
        f.exec_prefix = Some(exec_prefix);
        Ok(f)
    }

    /// The ssh argv this forward spawns — exposed so a caller can print it.
    pub fn argv(&self) -> Vec<String> {
        machine::forward_argv(&self.entry, self.port, self.remote_port)
    }

    /// What is actually exec'd: [`argv`](Self::argv), unless a test seam has
    /// substituted a stand-in for the `ssh` binary.
    fn spawn_argv(&self) -> Vec<String> {
        #[cfg(test)]
        if let Some(prefix) = &self.exec_prefix {
            let mut argv = prefix.clone();
            argv.extend(self.argv());
            return argv;
        }
        self.argv()
    }

    /// Does the local end accept a connection?
    ///
    /// **On a blocking thread**, for the same reason the spawn below is: this is
    /// `TcpStream::connect`, and a syscall that can block belongs off the async
    /// worker even when the address is loopback and the answer is usually
    /// instant. It used to run inline on the fast path — the one path taken on
    /// every healthy call — while holding `ensuring`, which is the shape the
    /// slow path's own comment already warns against.
    async fn port_answers(&self) -> Result<bool, ForwardError> {
        let port = self.port;
        tokio::task::spawn_blocking(move || port_answers(port))
            .await
            .map_err(|e| ForwardError(format!("forward probe failed: {e}")))
    }

    /// Is the child gone (exited, reaped elsewhere, or never spawned)?
    fn child_is_dead(&self) -> bool {
        let mut guard = self.child.lock();
        match guard.child.as_mut() {
            None => true,
            // A probe error means it was reaped elsewhere — either way there is
            // no tunnel to wait for.
            Some(child) => !matches!(child.try_wait(), Ok(None)),
        }
    }
}

#[async_trait::async_trait]
impl MachineForward for SshForward {
    fn port(&self) -> u16 {
        self.port
    }

    fn looks_alive(&self) -> bool {
        // Just the `try_wait` — deliberately NOT `port_answers`, which is the
        // blocking half. See the trait method's doc for what that costs in
        // precision and why it is the right trade for a per-request caller.
        !self.child_is_dead()
    }

    async fn ensure(&self) -> Result<(), ForwardError> {
        // A closed forward is closed for good (plan 026 §3.3): refused before
        // the lock, and again under it.
        if self.child.is_closed() {
            return Err(shutting_down(&self.label));
        }
        // One ensure at a time (see the `ensuring` field).
        let _serialized = self.ensuring.lock().await;
        if self.child.is_closed() {
            return Err(shutting_down(&self.label));
        }
        if !self.child_is_dead() && self.port_answers().await? {
            return Ok(());
        }

        let argv = self.spawn_argv();
        let port = self.port;
        let label = self.label.clone();
        let slot = Arc::clone(&self.child);
        let gate = self.gate.clone();
        // Counted INTO the blocking task: an `ensure` whose future is dropped
        // mid-await still has its blocking half running, and that half is what
        // could spawn.
        let in_flight = self.in_flight.enter();
        // Spawning, the predecessor's kill and the readiness poll are all
        // blocking; keep them off the async worker so a slow-to-refuse host
        // cannot stall the runtime, and no `kill`/`waitpid` runs on one.
        //
        // The slot is handed INTO the blocking task so the child is recorded
        // the instant it exists. `spawn_blocking` tasks cannot be aborted — if
        // this future is dropped mid-await (a watcher being stopped, which is
        // routine), the task still runs to completion, and storing the child
        // only after `.await` would orphan an `ssh -N -L` process that nothing
        // can ever kill: `std::process::Child`'s `Drop` does not kill.
        tokio::task::spawn_blocking(move || {
            let _in_flight = in_flight;
            // Kill any predecessor BEFORE spawning: a child that is merely
            // unresponsive (rather than exited) still holds the local port, and
            // leaving it running would both orphan it and doom the replacement
            // to `ExitOnForwardFailure`.
            kill_child(&slot);
            let outcome = spawn_and_wait(&slot, &argv, port, &label, gate.as_ref());
            if outcome.is_err() {
                // A failed attempt must not leave a half-established tunnel
                // behind — killed here, inside the task, so a dropped `ensure`
                // cannot skip it.
                kill_child(&slot);
            }
            outcome
        })
        .await
        .map_err(|e| ForwardError(format!("forward task failed: {e}")))?
    }

    /// Close the slot — its child killed and reaped (bounded), every later
    /// store refused — then wait at most [`IN_FLIGHT_WAIT`] for an `ensure`
    /// already in flight to notice and return. **Blocking.** Answers whether
    /// both settled within their bounds.
    fn close_now(&self) -> bool {
        let settled = self.child.close();
        let idle = self.in_flight.wait_idle(IN_FLIGHT_WAIT);
        settled && idle
    }
}

impl Drop for SshForward {
    /// Through the slot's close (plan 026 §3.3, F5): the kill is the slot's
    /// recorded obligation while it runs, so a registry close that finds the
    /// slot empty mid-drop waits (bounded) for this kill instead of reading
    /// "empty" as "nothing here". This drop usually runs on
    /// `OwnedForward`'s detached thread.
    fn drop(&mut self) {
        let _ = self.child.close();
    }
}

/// Kill + reap whatever child is in `slot`, and empty it. Idempotent, and safe
/// on an already-reaped child (`try_wait` caches the status and `kill` refuses
/// to signal a reaped process, so no recycled PID can be hit). The child is
/// TAKEN under the slot's mutex — and the kill recorded there as in progress —
/// and killed outside it, the reap bounded. Unlike [`ChildSlot::close`] it
/// leaves the slot open: it is the respawn's own teardown.
fn kill_child(slot: &ChildSlot) {
    let deadline = Instant::now() + REAP_BOUND;
    let child = slot.lock().take_for_kill();
    if let Some(child) = child {
        slot.reap_taken(child, deadline);
    }
}

/// SIGKILL `child`, then reap it — `try_wait` polling for at most
/// [`REAP_BOUND`]. A child that is still not reapable then is left to the
/// kernel: an exit must never wait on a `waitpid` that does not return.
fn kill_and_reap(mut child: std::process::Child) {
    let _ = child.kill();
    reap_until(&mut child, Instant::now() + REAP_BOUND);
}

fn reap_until(child: &mut std::process::Child, deadline: Instant) {
    loop {
        match child.try_wait() {
            Ok(Some(_)) | Err(_) => return,
            Ok(None) if Instant::now() >= deadline => return,
            Ok(None) => std::thread::sleep(Duration::from_millis(5)),
        }
    }
}

/// The error a closed forward or a closed gate answers with.
fn shutting_down(label: &str) -> ForwardError {
    ForwardError(format!(
        "{SHUTTING_DOWN}: the app is quitting, so the hub tunnel to {label} is not started"
    ))
}

/// The `ensure`s whose blocking half is running: a count, and a `Condvar` —
/// its one waiter is [`MachineForward::close_now`], which runs on a plain OS
/// thread.
#[derive(Default)]
struct InFlight {
    count: Mutex<usize>,
    idle: Condvar,
}

impl InFlight {
    fn enter(self: &Arc<Self>) -> InFlightGuard {
        *self.count.lock().unwrap_or_else(|e| e.into_inner()) += 1;
        InFlightGuard(Arc::clone(self))
    }

    /// Wait at most `within` for the count to reach zero; whether it did.
    fn wait_idle(&self, within: Duration) -> bool {
        let guard = self.count.lock().unwrap_or_else(|e| e.into_inner());
        let (guard, _) = self
            .idle
            .wait_timeout_while(guard, within, |count| *count > 0)
            .unwrap_or_else(|e| e.into_inner());
        *guard == 0
    }
}

struct InFlightGuard(Arc<InFlight>);

impl Drop for InFlightGuard {
    fn drop(&mut self) {
        let mut count = self.0.count.lock().unwrap_or_else(|e| e.into_inner());
        *count = count.saturating_sub(1);
        if *count == 0 {
            self.0.idle.notify_all();
        }
    }
}

/// The live `ssh -N -L` child — in a slot that **kills whatever it is still
/// holding when its last reference goes**.
///
/// The killing `Drop` is not belt-and-braces; it closes an orphan this module
/// could otherwise still produce, from the opposite direction to the one
/// [`MachineForward::ensure`]'s comment already guards.
///
/// The sequence: a tunnel is dead, so `ensure` queues a repair on the blocking
/// pool — and before that task is scheduled, the lane is evicted. The
/// [`SshForward`] drops, its `Drop` calls [`kill_child`] on a slot that is
/// EMPTY (the child does not exist yet), so it kills nothing and returns
/// happily. The queued task then runs anyway (`spawn_blocking` cannot be
/// aborted), spawns `ssh`, and stores it in what is now its own last surviving
/// reference to this slot. Nothing is left that could ever kill it, and
/// `std::process::Child`'s own `Drop` does not: an `ssh -N -L` process outlives
/// the app.
///
/// Making the SLOT responsible removes the interleaving question entirely. The
/// task ends, its `Arc` drops, this runs, and the child dies — whichever order
/// the eviction and the spawn happened in, and without any storer having to
/// remember a check. It is the natural completion of the "hand the slot INTO
/// the task" design: the slot owns the child, so the slot owns killing it.
///
/// **And it can be closed for good** (plan 026 §3.3). The orderly shutdown
/// cannot wait for a last reference that may live in an aborted pump's future
/// or a detached thread, so [`ChildSlot::close`] evicts permanently: it marks
/// the slot closed and TAKES the child under the mutex, then kills and reaps it
/// outside it, bounded. The store a spawn makes ([`ChildSlot::store`]) checks
/// the same flag under the same mutex, so a child spawned after the close is
/// killed at once — by the store's ordering, not by some later drop.
///
/// **An empty slot is not a dead child** (F5). Whoever takes the child to kill
/// it records the kill as in progress (`killing`) until its reap is done, so a
/// [`ChildSlot::close`] that finds the slot empty while another thread — a
/// detached `SshForward` drop, say — still owns an unfinished reap waits for it
/// (bounded).
///
/// **And the SIGKILL is sent before the child leaves the slot** (R2-2, plan
/// 026 AM4): under the slot's mutex — `Child::kill` sends the signal and does
/// not wait — so a child counted in `killing` has always been signalled
/// already. A thread descheduled after taking it can delay only the REAP; a
/// close whose bound passes first returns with that child SIGKILLed, and says
/// so.
#[derive(Default)]
struct ChildSlot {
    state: Mutex<SlotState>,
    /// Signalled when the last kill in progress has finished.
    settled: Condvar,
    /// Test seam: run after a teardown took the child (already SIGKILLed,
    /// under the mutex) and before its reap — a destroying thread descheduled
    /// at the worst moment.
    #[cfg(test)]
    at_taken: Mutex<Option<Box<dyn FnOnce() + Send>>>,
    /// Test seam: run inside the spawn section, before the spawn — a spawn
    /// admitted before a close and descheduled until after it.
    #[cfg(test)]
    at_spawn: Mutex<Option<Box<dyn FnOnce() + Send>>>,
    /// Test record: the pid of every child spawned into this slot, taken at
    /// the spawn itself (a child killed at once may never run far enough to
    /// say so itself).
    #[cfg(test)]
    spawned: Mutex<Vec<i32>>,
    /// Test seam: run just before the reap, outside the mutex — where a test
    /// proves the mutex is not held.
    #[cfg(test)]
    at_reap: Mutex<Option<Box<dyn FnOnce() + Send>>>,
}

#[derive(Default)]
struct SlotState {
    child: Option<std::process::Child>,
    /// Set by [`ChildSlot::close`], never cleared.
    closed: bool,
    /// Whether this slot is in the spawn gate's registry — entered inside the
    /// first spawn section that used it, held weakly there, so it leaves when
    /// the slot drops.
    registered: bool,
    /// Kills in progress: children taken out of this slot whose kill and reap
    /// some thread has not finished yet.
    killing: usize,
}

impl SlotState {
    /// SIGKILL the child HERE — the caller holds the slot's mutex, and `kill`
    /// sends the signal without waiting — then take it for its reap, recording
    /// the reap as in progress (R2-2). `kill` on a child already reaped is a
    /// no-op (std caches its status), so no recycled pid is ever signalled.
    fn take_for_kill(&mut self) -> Option<std::process::Child> {
        let mut child = self.child.take()?;
        let _ = child.kill();
        self.killing += 1;
        Some(child)
    }
}

impl ChildSlot {
    /// The guard, ignoring poisoning — this module's rule, for the reason
    /// [`crate::machine`]'s peers give: the data behind it is one child and a
    /// flag, and a panic elsewhere must not turn a tunnel into an un-killable
    /// one.
    fn lock(&self) -> std::sync::MutexGuard<'_, SlotState> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn is_closed(&self) -> bool {
        self.lock().closed
    }

    /// Record a freshly spawned child — unless the slot was closed, in which
    /// case the child is killed and reaped here and now and the store answers
    /// [`SHUTTING_DOWN`]. The check and the store are one critical section, so
    /// a close can land before it (the child is refused) or after it (the close
    /// takes the child), never between.
    fn store(&self, child: std::process::Child, label: &str) -> Result<(), ForwardError> {
        let mut state = self.lock();
        if state.closed {
            // Signalled under the mutex, like every other kill here (R2-2).
            let mut child = child;
            let _ = child.kill();
            state.killing += 1;
            drop(state);
            self.reap_taken(child, Instant::now() + REAP_BOUND);
            return Err(shutting_down(label));
        }
        state.child = Some(child);
        Ok(())
    }

    /// Register this slot with `gate`, inside the spawn's `section` — and, on a
    /// gate that has CLOSED, close the slot here and now (its child killed and
    /// reaped, bounded) and answer [`SHUTTING_DOWN`] (plan 026 §3.3, F1): a
    /// section admitted before the close whose child came after it must not
    /// leave that child for a drain that may already have passed. Blocking —
    /// this is the spawn's own blocking thread.
    fn register(
        self: &Arc<Self>,
        gate: &SpawnGate,
        section: &SpawnSection,
        label: &str,
    ) -> Result<(), ForwardError> {
        if !gate.is_closed() && self.lock().registered {
            return Ok(());
        }
        let weak: Weak<dyn SlotClose> = Arc::downgrade(self) as Weak<ChildSlot>;
        match gate.register_slot(section, weak) {
            Ok(()) => {
                self.lock().registered = true;
                Ok(())
            }
            Err(_) => {
                self.close();
                Err(shutting_down(label))
            }
        }
    }

    /// Reap a child taken from this slot — SIGKILLed already, under the mutex,
    /// and its reap recorded as in progress — until `deadline`, and settle the
    /// record. Outside the mutex.
    fn reap_taken(&self, mut child: std::process::Child, deadline: Instant) {
        #[cfg(test)]
        if let Some(hook) = self
            .at_taken
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
        {
            hook();
        }
        #[cfg(test)]
        if let Some(hook) = self
            .at_reap
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
        {
            hook();
        }
        reap_until(&mut child, deadline);
        let mut state = self.lock();
        state.killing = state.killing.saturating_sub(1);
        if state.killing == 0 {
            self.settled.notify_all();
        }
    }

    /// Close for good: mark it closed, and SIGKILL and take the child, under
    /// the mutex; then reap it OUTSIDE the mutex — and wait for any reap
    /// another thread has in progress. All of it within one [`REAP_BOUND`].
    /// Idempotent.
    ///
    /// Answers whether it SETTLED. A reap another thread still owns at the
    /// bound (a destroy descheduled after taking its child) is abandoned to
    /// that thread — its child was SIGKILLed before it left the slot — and
    /// that is said (plan 026 AM4).
    fn close(&self) -> bool {
        let deadline = Instant::now() + REAP_BOUND;
        let child = {
            let mut state = self.lock();
            state.closed = true;
            state.take_for_kill()
        };
        if let Some(child) = child {
            self.reap_taken(child, deadline);
        }
        let state = self.lock();
        let left = deadline.saturating_duration_since(Instant::now());
        let (state, _) = self
            .settled
            .wait_timeout_while(state, left, |state| state.killing > 0)
            .unwrap_or_else(|e| e.into_inner());
        let unsettled = state.killing;
        drop(state);
        if unsettled > 0 {
            tracing::warn!(
                unsettled,
                "forward slot: a reap another thread owns had not finished by the bound; its \
                 child was SIGKILLed before it left the slot, and the reap is abandoned to it"
            );
        }
        unsettled == 0
    }
}

impl SlotClose for ChildSlot {
    fn close(&self) -> bool {
        ChildSlot::close(self)
    }
}

impl Drop for ChildSlot {
    fn drop(&mut self) {
        // `get_mut` rather than `lock`: we hold `&mut self`, so there is no
        // contention to wait on and no way to deadlock against a poisoned lock.
        let state = self.state.get_mut().unwrap_or_else(|e| e.into_inner());
        if let Some(child) = state.child.take() {
            kill_and_reap(child);
        }
    }
}

/// Spawn the forward, record it in `slot` immediately, and block until its
/// local end answers.
///
/// **Deadline-poll AND watch the child**, because the two common failures are
/// instant: a taken local port (`ExitOnForwardFailure`) and an unreachable or
/// refusing host both exit ssh in well under a second. Waiting out the full
/// timeout for a process that is already gone is ten seconds of nothing for no
/// information.
///
/// The child is checked BEFORE the port is trusted: a predecessor's still-open
/// forward would otherwise make `port_answers` true on the first evaluation and
/// a doomed child would be reported ready without its exit ever being consulted.
fn spawn_and_wait(
    slot: &Arc<ChildSlot>,
    argv: &[String],
    port: u16,
    label: &str,
    gate: Option<&SpawnGate>,
) -> Result<(), ForwardError> {
    let (bin, rest) = argv.split_first().expect("ssh argv is never empty");
    // **The spawn section** (plan 026 §3.3): check → spawn → store → register,
    // all under the gate's shared guard, so a close either waits for this
    // spawn to be recorded where the drain finds it or refuses it outright.
    let section = match gate {
        Some(gate) => Some(gate.enter().map_err(|_| shutting_down(label))?),
        None => None,
    };
    if slot.is_closed() {
        return Err(shutting_down(label));
    }
    #[cfg(test)]
    if let Some(hook) = slot
        .at_spawn
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .take()
    {
        hook();
    }
    let child = std::process::Command::new(bin)
        .args(rest)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .spawn()
        .map_err(|e| ForwardError(format!("opening the hub tunnel to {label}: {e}")))?;
    #[cfg(test)]
    slot.spawned
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .push(child.id() as i32);
    // Recorded before anything can fail or be cancelled — this is the only
    // window in which the process is untracked, and it is now just `spawn`. A
    // slot closed meanwhile kills it here.
    slot.store(child, label)?;
    if let (Some(gate), Some(section)) = (gate, section.as_ref()) {
        slot.register(gate, section, label)?;
    }
    drop(section);

    let deadline = std::time::Instant::now() + FORWARD_READY_TIMEOUT;
    loop {
        // A close ends the wait within about one poll (plan 026 §3.3): it took
        // the child, and nothing here may outlive it.
        if slot.is_closed() {
            return Err(shutting_down(label));
        }
        // Exit status first, then readiness (see the doc above).
        let exited = {
            let mut guard = slot.lock();
            match guard.child.as_mut() {
                Some(child) => match child.try_wait() {
                    Ok(Some(status)) => Some(status.to_string()),
                    Ok(None) => None,
                    Err(e) => Some(e.to_string()),
                },
                // Something else took the child (a concurrent teardown).
                None => Some("the tunnel was torn down".to_string()),
            }
        };
        if let Some(status) = exited {
            return Err(ForwardError(format!(
                "the hub tunnel to {label} exited immediately ({status})"
            )));
        }
        if port_answers(port) {
            return Ok(());
        }
        if std::time::Instant::now() >= deadline {
            return Err(ForwardError(format!(
                "the hub tunnel to {label} did not come up"
            )));
        }
        std::thread::sleep(READY_POLL);
    }
}

/// Does the local end accept a connection — within [`READY_POLL`]? Bounded,
/// so a close lands within about one poll even when the connect would hang.
fn port_answers(port: u16) -> bool {
    let addr = std::net::SocketAddr::from(([127, 0, 0, 1], port));
    std::net::TcpStream::connect_timeout(&addr, READY_POLL).is_ok()
}

/// An unused loopback port: bind `:0`, read the assignment, release.
fn free_loopback_port() -> std::io::Result<u16> {
    let ln = std::net::TcpListener::bind("127.0.0.1:0")?;
    let port = ln.local_addr()?.port();
    drop(ln);
    Ok(port)
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::path::Path;

    // ---- doubles ----

    /// The stand-in for the `ssh` child: it appends its own pid to `log` — the
    /// side-effect a test can wait on WITHOUT consulting the slot that is under
    /// test — and then runs `body`. `exec` keeps the pid it logged.
    fn fake_ssh(log: &Path, body: &str) -> Vec<String> {
        vec![
            "/bin/sh".to_string(),
            "-c".to_string(),
            format!("printf '%s\\n' $$ >> '{}'; {body}", log.display()),
        ]
    }

    /// Every pid the fake ssh has been started as, in spawn order — i.e. how
    /// many forward processes this test has created.
    fn spawned_pids(log: &Path) -> Vec<i32> {
        std::fs::read_to_string(log)
            .unwrap_or_default()
            .lines()
            .filter_map(|line| line.trim().parse().ok())
            .collect()
    }

    /// Does `pid` still exist? `kill(pid, 0)` signals nothing and fails with
    /// `ESRCH` once the process is gone AND reaped — which is exactly the
    /// question "did the forward clean up after itself".
    fn pid_is_alive(pid: i32) -> bool {
        // SAFETY: `kill` with signal 0 performs only an existence/permission
        // check; it touches no memory we own.
        unsafe { libc::kill(pid, 0) == 0 }
    }

    /// Bring the forward's local port up once the fake ssh has actually
    /// started — the tunnel a real `ssh -L` opens after it connects.
    ///
    /// Deliberately NOT bound up front: the readiness poll must become true as a
    /// CONSEQUENCE of a child having started, or `ensure` can return before its
    /// child has run at all and every "how many were spawned" assertion below
    /// races the log. The listener lives in the task's output, so awaiting the
    /// handle hands the test something to hold the port with.
    /// Stop a test tunnel answering — for EVERY copy of its fd, not just
    /// this one. A plain drop closes one descriptor; a child that another test
    /// thread forked a moment earlier holds an inherited copy until its exec
    /// (CLOEXEC), and the kernel keeps accepting on the port meanwhile — so a
    /// re-ensure probing the port right after the drop could read the dead
    /// tunnel as alive and skip its respawn (#397; diagnosed in plan 026 C3's
    /// fix round 2: the cell's first child still alive, nothing listening on
    /// the port seconds later). `shutdown(2)` takes a listening socket out of
    /// LISTEN for every descriptor that shares it. On a platform where it does
    /// not apply to a listener, it fails harmlessly and the drop still closes.
    fn close_tunnel(listener: std::net::TcpListener) {
        use std::os::fd::AsRawFd as _;
        // SAFETY: shutdown(2) on a socket this test owns; no memory is touched.
        unsafe {
            libc::shutdown(listener.as_raw_fd(), libc::SHUT_RDWR);
        }
        drop(listener);
    }

    /// A freed loopback port held by a **bound, non-listening** socket (plan 026
    /// §3.6, #397): `connect` to it is refused, so a re-`ensure` reads the port
    /// as dead and respawns, and no other bind can take it meanwhile.
    ///
    /// Not a listener that never accepts — the kernel completes `connect` from
    /// the backlog and the fast path would still be taken. And NOT
    /// `SO_REUSEADDR`: on Linux that lets a second `SO_REUSEADDR` bind (which is
    /// what `std`'s `TcpListener::bind` sets) share any non-listening bound
    /// port, which would make the reservation a no-op.
    struct PortReservation {
        fd: libc::c_int,
    }

    impl PortReservation {
        /// Take `port`, retrying while a descriptor inherited by another
        /// thread's `fork()` still holds the old listener's binding (until that
        /// child's `exec`, about a millisecond).
        fn take(port: u16) -> Self {
            let deadline = std::time::Instant::now() + Duration::from_secs(10);
            loop {
                // SAFETY: plain socket(2)/bind(2) on memory this function owns.
                unsafe {
                    let fd = libc::socket(libc::AF_INET, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0);
                    assert!(fd >= 0, "socket: {}", std::io::Error::last_os_error());
                    let mut addr: libc::sockaddr_in = std::mem::zeroed();
                    addr.sin_family = libc::AF_INET as libc::sa_family_t;
                    addr.sin_port = port.to_be();
                    addr.sin_addr.s_addr = u32::from_ne_bytes([127, 0, 0, 1]);
                    let rc = libc::bind(
                        fd,
                        std::ptr::addr_of!(addr).cast(),
                        std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t,
                    );
                    if rc == 0 {
                        return Self { fd };
                    }
                    let err = std::io::Error::last_os_error();
                    libc::close(fd);
                    assert!(
                        std::time::Instant::now() < deadline,
                        "could not reserve port {port}: {err}"
                    );
                }
                std::thread::sleep(Duration::from_millis(1));
            }
        }
    }

    impl Drop for PortReservation {
        fn drop(&mut self) {
            // SAFETY: closes the descriptor this value owns, once.
            unsafe {
                libc::close(self.fd);
            }
        }
    }

    /// [`close_tunnel`], then hold the freed port until the reservation is
    /// dropped (see [`PortReservation`]).
    fn close_tunnel_and_reserve(listener: std::net::TcpListener, port: u16) -> PortReservation {
        close_tunnel(listener);
        PortReservation::take(port)
    }

    fn tunnel_once_started(
        port: u16,
        log: &Path,
        spawns: usize,
    ) -> tokio::task::JoinHandle<std::net::TcpListener> {
        tunnel_once_started_holding(port, log, spawns, None)
    }

    /// [`tunnel_once_started`] for a port a reservation holds: the reservation
    /// is released just before the replacement tunnel binds.
    fn tunnel_once_started_holding(
        port: u16,
        log: &Path,
        spawns: usize,
        reservation: Option<PortReservation>,
    ) -> tokio::task::JoinHandle<std::net::TcpListener> {
        let log = log.to_path_buf();
        tokio::spawn(async move {
            let held = reservation;
            wait_for("the forward process to start", || {
                spawned_pids(&log).len() >= spawns
            })
            .await;
            drop(held);
            // A descriptor another thread's `fork()` inherited a moment ago
            // (the reservation's, or the old listener's) holds the binding
            // until that child's `exec`; the bind is retried across that
            // window — milliseconds — and no longer.
            let deadline = std::time::Instant::now() + Duration::from_secs(10);
            loop {
                match std::net::TcpListener::bind(("127.0.0.1", port)) {
                    Ok(listener) => break listener,
                    Err(e) if std::time::Instant::now() < deadline => {
                        let _ = e;
                        tokio::time::sleep(Duration::from_millis(1)).await;
                    }
                    Err(e) => panic!("bind the forward's local port: {e}"),
                }
            }
        })
    }

    /// Poll until `cond` holds. A condition wait, not a fixed sleep: the
    /// expected path returns in microseconds and only a genuine regression pays
    /// the deadline. The deadline bounds a HANG, not a latency — the poll is
    /// already a consequence of a real event (the fake ssh's pid landing in
    /// the spawn log), so the happy path returns in microseconds and pays
    /// nothing for however large this number is; only a genuine deadlock
    /// spends it. 60s, not a smaller "should be plenty" guess: a 15s bound
    /// was watched fail here, once, under a full workspace `cargo test` run
    /// concurrent with a sustained `cargo build --release` load (the exact
    /// CI-shaped contention this bound exists to survive) — so the number is
    /// sized against OBSERVED contention on a loaded box, not against
    /// expected scheduling latency.
    async fn wait_for(what: &str, mut cond: impl FnMut() -> bool) {
        let deadline = std::time::Instant::now() + Duration::from_secs(60);
        while std::time::Instant::now() < deadline {
            if cond() {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("timed out waiting for {what}");
    }

    fn entry() -> shed_core::config::MachineEntry {
        shed_core::config::MachineEntry {
            name: "mini3".into(),
            host: "mini3".into(),
            user: Some("charliek".into()),
            ssh_port: 22,
            known_hosts: None,
        }
    }

    /// The seam's central invariant: `ensure` never moves the port.
    #[tokio::test]
    async fn a_fixed_port_forward_is_stable_and_needs_no_work() {
        let f = FixedPort(41234);
        assert_eq!(f.port(), 41234);
        f.ensure().await.expect("a fixed port is always ready");
        assert_eq!(f.port(), 41234, "ensure must never change the port");
    }

    #[test]
    fn a_reserved_ssh_forward_picks_a_local_port_and_spawns_nothing_yet() {
        let f = SshForward::reserve_for(entry(), SOME_REMOTE_PORT).expect("reserve");
        assert!(f.port() > 0);
        let argv = f.argv();
        // The concrete thing that matters: this local port maps to the named
        // loopback port on the far side, and a lost race is loud.
        assert!(argv.windows(2).any(|w| w
            == [
                "-L",
                &format!("127.0.0.1:{}:127.0.0.1:{SOME_REMOTE_PORT}", f.port())
            ]));
        assert!(argv.contains(&"ExitOnForwardFailure=yes".to_string()));
        assert!(argv.contains(&"-N".to_string()), "runs no remote command");
        assert!(f.child_is_dead(), "nothing is spawned until ensure()");
    }

    /// **The lane's forward names its own far side** (plan 015 §3.4).
    ///
    /// `reserve_for` is what an opencode lane reserves with: the agent's HTTP
    /// server binds an ephemeral loopback port, roost reports it, and the tunnel
    /// has to land on THAT port. The `-L` spec is the only place the number
    /// appears, so this asserts it there.
    #[test]
    fn a_forward_reserved_for_a_port_tunnels_to_that_port() {
        let remote = 41_811;
        assert_ne!(remote, SOME_REMOTE_PORT, "a different port on purpose");
        let f = SshForward::reserve_for(entry(), remote).expect("reserve_for");
        let argv = f.argv();
        assert!(
            argv.windows(2)
                .any(|w| w == ["-L", &format!("127.0.0.1:{}:127.0.0.1:{remote}", f.port())]),
            "argv does not forward to the reported port: {argv:?}"
        );
        // The rest of the tunnel is unchanged — a lane forward is an ssh -N
        // tunnel like any other, and a lost local port is still loud.
        assert!(argv.contains(&"-N".to_string()), "runs no remote command");
        assert!(argv.contains(&"ExitOnForwardFailure=yes".to_string()));
        assert!(
            !argv
                .iter()
                .any(|a| a.contains(&SOME_REMOTE_PORT.to_string())),
            "another forward's port leaked into this one: {argv:?}"
        );
        // A second forward names its own far side independently — the port is
        // per-forward, not a shared constant.
        let other = SshForward::reserve_for(entry(), SOME_REMOTE_PORT).expect("reserve_for");
        assert!(other.argv().windows(2).any(|w| w
            == [
                "-L",
                &format!("127.0.0.1:{}:127.0.0.1:{SOME_REMOTE_PORT}", other.port())
            ]));
    }

    /// **The local port survives a re-`ensure`, far-side port included.**
    ///
    /// The module's central invariant, asserted on the lane's constructor: the
    /// lane's reconnect path re-`ensure`s the forward inside its backoff loop
    /// (§3.4), and a forward that moved its local port there would leave the
    /// `OpencodeClient` holding a base URL that points at nothing — with no
    /// error, because something else may well have taken the port.
    #[tokio::test]
    async fn a_lane_forward_keeps_its_local_port_across_a_re_ensure() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let remote = 41_812;
        let forward =
            SshForward::reserve_faked_for(entry(), remote, fake_ssh(&log, "exec sleep 30"))
                .expect("reserve_faked_for");
        let port = forward.port();
        let argv_before = forward.argv();

        let tunnel = tunnel_once_started(port, &log, 1);
        forward.ensure().await.expect("the first ensure");
        let tunnel = tunnel.await.expect("the tunnel task");
        assert_eq!(forward.port(), port, "ensure moved the local port");

        // Kill the tunnel so the next ensure genuinely re-establishes rather
        // than short-circuiting on a healthy child.
        let reservation = close_tunnel_and_reserve(tunnel, port);
        let tunnel = tunnel_once_started_holding(port, &log, 2, Some(reservation));
        forward.ensure().await.expect("the re-establish");
        let _tunnel = tunnel.await.expect("the tunnel task");

        assert_eq!(spawned_pids(&log).len(), 2, "a replacement was spawned");
        assert_eq!(
            forward.port(),
            port,
            "the re-establish moved the local port"
        );
        assert_eq!(
            forward.argv(),
            argv_before,
            "the re-established tunnel forwards somewhere else"
        );
    }

    /// A forward whose destination refuses must fail via the WATCH-THE-CHILD
    /// branch, not by waiting out the readiness deadline.
    ///
    /// Deliberately `127.0.0.1` at a just-released port: that gives an instant
    /// `ECONNREFUSED` regardless of the host's network. An unroutable address
    /// (TEST-NET-1) would seem more realistic but is worse as a test — on a
    /// network that DROPs rather than refuses it blackholes for the full
    /// `ConnectTimeout`, and since that equals `FORWARD_READY_TIMEOUT` both
    /// failure branches fire at once and the assertion stops distinguishing
    /// them.
    #[tokio::test]
    async fn a_refused_machine_fails_via_the_child_watch_not_the_deadline() {
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let refused = ln.local_addr().expect("addr").port();
        drop(ln);

        let mut bad = entry();
        bad.host = "127.0.0.1".into();
        bad.ssh_port = refused;
        bad.user = None;
        let f = SshForward::reserve_for(bad, 1029).expect("reserve");

        let started = std::time::Instant::now();
        let err = f.ensure().await.expect_err("nothing is listening there");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "took {:?} — a refused connect must not wait out the {FORWARD_READY_TIMEOUT:?} deadline",
            started.elapsed()
        );
        assert!(
            err.to_string().contains("exited immediately"),
            "should fail via the child watch: {err}"
        );
        // Errors name the machine the way the user addressed it.
        assert!(err.to_string().contains("machine:mini3"), "{err}");
        // A failed attempt leaves nothing running.
        assert!(f.child_is_dead());
    }

    /// **An eviction that lands before a queued repair still must not orphan
    /// that repair's child.**
    ///
    /// The interleaving is the one [`ChildSlot`]'s doc names, and it is the
    /// mirror image of the case the test below covers: there the child exists
    /// and the owner tears it down; HERE the owner tears down FIRST, finds an
    /// empty slot, kills nothing — and the repair it thought it had cancelled
    /// goes on to spawn `ssh` anyway, because a `spawn_blocking` task cannot be
    /// aborted.
    ///
    /// Modelled at the slot rather than through a saturated blocking pool: the
    /// property under test is "the last reference to a slot kills what the slot
    /// holds", and asserting it directly is both deterministic and the thing a
    /// future storer of a child actually relies on. Against the previous
    /// `Mutex<Option<Child>>` this fails — `std::process::Child`'s `Drop` does
    /// not kill, which is the whole reason the orphan was reachable.
    #[test]
    fn a_child_stored_after_its_forward_was_evicted_is_still_killed() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");

        let slot = Arc::new(ChildSlot::default());
        // The queued repair's reference — the one that outlives the forward.
        let queued = Arc::clone(&slot);

        // The eviction. The owner lets go BEFORE the child exists, so its own
        // teardown finds an empty slot and has nothing to kill.
        kill_child(&slot);
        drop(slot);

        // The repair runs anyway and records its child in the only reference
        // left, exactly as `spawn_and_wait` does.
        let argv = fake_ssh(&log, "exec sleep 30");
        let (bin, rest) = argv.split_first().expect("argv is never empty");
        let child = std::process::Command::new(bin)
            .args(rest)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .spawn()
            .expect("the fake forward starts");
        let pid = child.id() as i32;
        queued.lock().child = Some(child);
        assert!(pid_is_alive(pid), "the fake forward really started");

        // The task ends, releasing the last reference.
        drop(queued);
        assert!(
            !pid_is_alive(pid),
            "pid {pid} outlived every reference to its slot — an orphaned ssh \
             tunnel that nothing can ever kill"
        );
    }

    /// **An `ensure` that is dropped mid-flight must still leave a killable
    /// child.** Dropping it is routine, not exotic: `MachineHubWatcher::stop`
    /// (and therefore its `Drop`) aborts the loop task, which drops whatever
    /// `ensure` was in flight.
    ///
    /// The spawn runs inside `spawn_blocking`, and those tasks CANNOT be
    /// aborted — dropping the join handle merely detaches them — so the child
    /// gets created no matter what. If it is recorded only after the `.await`,
    /// the abort loses the handle to a live `ssh -N -L` process that nothing can
    /// ever kill: `std::process::Child`'s `Drop` does not kill.
    #[tokio::test]
    async fn an_ensure_aborted_mid_flight_still_leaves_its_child_killable() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        // Nothing is listening on the reserved port, so the readiness poll
        // never succeeds and the abort lands while `ensure` is still in flight.
        let forward = Arc::new(
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve"),
        );
        let slot = Arc::clone(&forward.child);

        let ensuring = Arc::clone(&forward);
        let task = tokio::spawn(async move { ensuring.ensure().await });
        // Wait on the CHILD's own side effect: the slot is the thing under test
        // and must not be part of the synchronisation.
        wait_for("the forward process to start", || {
            !spawned_pids(&log).is_empty()
        })
        .await;
        task.abort();
        let _ = task.await;

        let pids = spawned_pids(&log);
        assert_eq!(pids.len(), 1, "exactly one forward process was started");
        wait_for("the child to be recorded", || !forward.child_is_dead()).await;
        assert_eq!(
            slot.lock().child.as_ref().map(std::process::Child::id),
            Some(pids[0] as u32),
            "the recorded child must be the process that was actually spawned"
        );

        drop(forward);
        assert!(
            slot.lock().child.is_none(),
            "dropping the forward reaps and clears the child"
        );
        assert!(
            !pid_is_alive(pids[0]),
            "pid {} outlived the forward — an orphaned ssh tunnel",
            pids[0]
        );
    }

    /// **Two racing `ensure`s must leave one child, not two.** Consumers hold an
    /// `Arc<dyn MachineForward>` and the trait is `Send + Sync`, so this race is
    /// available to any two callers; unserialized, both would spawn onto the
    /// same local port and the second would overwrite — and thereby orphan — the
    /// first, since assigning over a `std::process::Child` does not kill it.
    #[tokio::test]
    async fn two_racing_ensures_leave_exactly_one_child() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let forward =
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve");
        let tunnel = tunnel_once_started(forward.port(), &log, 1);

        let (first, second) = tokio::join!(forward.ensure(), forward.ensure());
        first.expect("the first ensure");
        second.expect("the second ensure");
        let _tunnel = tunnel.await.expect("the tunnel task");

        let pids = spawned_pids(&log);
        assert_eq!(
            pids.len(),
            1,
            "a second ensure raced onto the same local port: pids {pids:?}"
        );
        assert!(!forward.child_is_dead(), "the surviving child is live");

        drop(forward);
        for pid in pids {
            assert!(!pid_is_alive(pid), "pid {pid} outlived the forward");
        }
    }

    /// **Re-establishing over an UNRESPONSIVE predecessor kills it first.** The
    /// child that is merely wedged — still running, no longer forwarding — is
    /// the case that separates "respawn" from "leak": it still holds the local
    /// port, so leaving it running both orphans it and dooms the replacement to
    /// `ExitOnForwardFailure`. Assigning over a `std::process::Child` does not
    /// kill it, so the kill has to be explicit.
    #[tokio::test]
    async fn re_establishing_kills_the_unresponsive_predecessor() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let forward =
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve");

        let tunnel = tunnel_once_started(forward.port(), &log, 1);
        forward.ensure().await.expect("the first ensure");
        let tunnel = tunnel.await.expect("the tunnel task");
        let wedged = spawned_pids(&log);
        assert_eq!(wedged.len(), 1);

        // The tunnel stops forwarding while its process lives on — from here
        // the local port refuses, but the child is still very much running.
        let reservation = close_tunnel_and_reserve(tunnel, forward.port());
        assert!(
            !forward.child_is_dead(),
            "the wedged child is still running"
        );

        let tunnel = tunnel_once_started_holding(forward.port(), &log, 2, Some(reservation));
        forward.ensure().await.expect("the re-establish");
        let _tunnel = tunnel.await.expect("the tunnel task");

        let pids = spawned_pids(&log);
        assert_eq!(pids.len(), 2, "a replacement was spawned");
        assert!(
            !pid_is_alive(wedged[0]),
            "pid {} was replaced without being killed — an orphaned tunnel \
             still holding the local port",
            wedged[0]
        );

        drop(forward);
        assert!(
            !pid_is_alive(pids[1]),
            "the replacement outlived the forward"
        );
    }

    /// **A tunnel that was closed while another process still holds a copy of
    /// its listener is closed for good** (#397, plan 026 §3.6): the cause the
    /// evidence found. Another test thread's `fork()` hands a child a copy of
    /// every descriptor until its `exec`; a plain `drop` closes only one copy,
    /// the kernel keeps accepting on the port, and a re-`ensure` probing it
    /// takes its fast path and never respawns. Deterministic here: the child
    /// is a bare `fork()` that waits, so the inherited copy lives as long as
    /// the cell needs it.
    #[test]
    fn closing_a_tunnel_stops_it_answering_even_with_a_forked_copy_of_its_fd() {
        fn forked_holder() -> libc::pid_t {
            // SAFETY: the child calls only async-signal-safe functions
            // (`pause`, `_exit`) between fork and its end.
            let pid = unsafe { libc::fork() };
            assert!(pid >= 0, "fork: {}", std::io::Error::last_os_error());
            if pid == 0 {
                unsafe {
                    libc::pause();
                    libc::_exit(0);
                }
            }
            pid
        }
        fn reap(pid: libc::pid_t) {
            assert!(pid > 1, "refusing to signal pid {pid}");
            // SAFETY: signals and reaps only the child this cell forked.
            unsafe {
                libc::kill(pid, libc::SIGKILL);
                libc::waitpid(pid, std::ptr::null_mut(), 0);
            }
        }

        // The failure: a plain drop leaves the inherited copy answering.
        let plain = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let plain_port = plain.local_addr().expect("addr").port();
        let holder = forked_holder();
        drop(plain);
        let still_answers = port_answers(plain_port);
        reap(holder);
        assert!(
            still_answers,
            "premise: a plain drop leaves a forked copy of the listener answering"
        );

        // The fix: `close_tunnel` takes it out of LISTEN for every copy.
        let closed = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let closed_port = closed.local_addr().expect("addr").port();
        let holder = forked_holder();
        close_tunnel(closed);
        let answers = port_answers(closed_port);
        reap(holder);
        assert!(
            !answers,
            "a closed tunnel still answers through a forked copy of its fd"
        );
    }

    /// **A freed port that something else listens on is a live tunnel to the
    /// fast path** (#397, plan 026 §3.6 hypothesis: port theft). In production
    /// that is right — a live `ssh -L` holds the port. This pins the mechanism
    /// the reservation below exists to rule out in the rig: with a thief
    /// listening on the freed port, `ensure` takes its fast path, does not
    /// respawn, and leaves the wedged predecessor alone.
    #[tokio::test]
    async fn a_thief_listening_on_the_freed_port_makes_ensure_skip_its_respawn() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let forward =
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve");
        let tunnel = tunnel_once_started(forward.port(), &log, 1);
        forward.ensure().await.expect("the first ensure");
        let tunnel = tunnel.await.expect("the tunnel task");
        let first = spawned_pids(&log);
        assert_eq!(first.len(), 1);

        close_tunnel(tunnel);
        // Another test binds and LISTENS on the freed port.
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        let thief = loop {
            match std::net::TcpListener::bind(("127.0.0.1", forward.port())) {
                Ok(l) => break l,
                Err(e) => {
                    assert!(
                        std::time::Instant::now() < deadline,
                        "the thief could not bind: {e}"
                    );
                    std::thread::sleep(Duration::from_millis(1));
                }
            }
        };
        forward.ensure().await.expect("ensure over the thief");
        assert_eq!(
            spawned_pids(&log),
            first,
            "the fast path should have trusted the thief's listener and not respawned"
        );
        assert!(pid_is_alive(first[0]), "the predecessor was left alone");
        drop(thief);
        drop(forward);
        assert!(!pid_is_alive(first[0]), "the forward outlived its drop");
    }

    /// **The reservation holds the freed port across the gap** (#397, plan 026
    /// §3.6): the rig's fix for port theft. While it is held a bind on the port
    /// fails and a `connect` is refused, so the re-`ensure` reads the tunnel as
    /// dead, kills the wedged predecessor and respawns — deterministically,
    /// whatever else is binding ports in the process.
    #[tokio::test]
    async fn a_reserved_freed_port_cannot_be_stolen_and_the_re_ensure_respawns() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let forward =
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve");
        let port = forward.port();
        let tunnel = tunnel_once_started(port, &log, 1);
        forward.ensure().await.expect("the first ensure");
        let tunnel = tunnel.await.expect("the tunnel task");
        let first = spawned_pids(&log);
        assert_eq!(first.len(), 1);

        let reservation = close_tunnel_and_reserve(tunnel, port);
        assert!(
            std::net::TcpListener::bind(("127.0.0.1", port)).is_err(),
            "another bind took a reserved port"
        );
        let refused = std::net::TcpStream::connect(("127.0.0.1", port))
            .expect_err("a reserved port must refuse connections");
        assert_eq!(refused.kind(), std::io::ErrorKind::ConnectionRefused);
        assert!(
            pid_is_alive(first[0]),
            "the predecessor is alive before the re-ensure"
        );

        let tunnel = tunnel_once_started_holding(port, &log, 2, Some(reservation));
        forward.ensure().await.expect("the re-establish");
        let _tunnel = tunnel.await.expect("the tunnel task");
        let pids = spawned_pids(&log);
        assert_eq!(pids.len(), 2, "the re-ensure respawned");
        assert!(
            !pid_is_alive(first[0]),
            "the re-ensure killed the predecessor"
        );
        drop(forward);
        assert!(
            !pid_is_alive(pids[1]),
            "the replacement outlived the forward"
        );
    }

    /// `ensure` is idempotent: on a forward whose tunnel is up it is a no-op,
    /// not a kill-and-respawn. A reconnecting watcher calls it on every attempt,
    /// so a needless respawn would tear down a working tunnel on each pass.
    #[tokio::test]
    async fn a_second_ensure_on_a_healthy_forward_is_a_no_op() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let forward =
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve");
        let tunnel = tunnel_once_started(forward.port(), &log, 1);

        let port = forward.port();
        forward.ensure().await.expect("the first ensure");
        let _tunnel = tunnel.await.expect("the tunnel task");
        let pids = spawned_pids(&log);
        assert_eq!(pids.len(), 1);

        forward.ensure().await.expect("the second ensure");
        assert_eq!(
            spawned_pids(&log),
            pids,
            "a healthy forward must not be respawned"
        );
        assert_eq!(forward.port(), port, "ensure must never change the port");

        drop(forward);
        assert!(!pid_is_alive(pids[0]));
    }

    // ---- plan 026 §3.3: the orderly shutdown's half of the forward ----

    /// Spawn `argv` as a fake forward child, the way `spawn_and_wait` does.
    fn spawn_fake(argv: &[String]) -> std::process::Child {
        let (bin, rest) = argv.split_first().expect("argv is never empty");
        std::process::Command::new(bin)
            .args(rest)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .spawn()
            .expect("the fake forward starts")
    }

    /// **A child stored after `close()` is killed by the store itself** — by
    /// the store's ordering, while the slot is still held, not by a later
    /// last-reference drop (plan 026 §3.3).
    #[test]
    fn a_child_stored_after_close_is_killed_by_the_store() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let slot = Arc::new(ChildSlot::default());
        slot.close();

        let child = spawn_fake(&fake_ssh(&log, "exec sleep 30"));
        let pid = child.id() as i32;
        let err = slot
            .store(child, "machine:mini3")
            .expect_err("a closed slot refuses the store");
        assert!(err.0.starts_with(SHUTTING_DOWN), "{err}");
        assert!(slot.lock().child.is_none(), "nothing was recorded");
        assert!(
            !pid_is_alive(pid),
            "pid {pid} outlived a store into a closed slot — and the slot is still held"
        );
    }

    /// **A child that ignores SIGTERM is still gone when `close()` returns**,
    /// within the reap bound — the kill is a SIGKILL — and the slot's mutex is
    /// never held across the reap (plan 026 §3.3, R12): the seam between the
    /// kill and the reap finds the mutex free.
    #[test]
    fn closing_a_slot_whose_child_ignores_sigterm_is_bounded_and_reaps_outside_the_mutex() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let slot = Arc::new(ChildSlot::default());
        let child = spawn_fake(&fake_ssh(&log, "trap '' TERM; exec sleep 30"));
        let pid = child.id() as i32;
        slot.store(child, "machine:mini3")
            .expect("an open slot stores");

        let free_during_reap = Arc::new(std::sync::atomic::AtomicBool::new(false));
        {
            let probe = Arc::clone(&slot);
            let free = Arc::clone(&free_during_reap);
            *slot.at_reap.lock().unwrap() = Some(Box::new(move || {
                free.store(
                    probe.state.try_lock().is_ok(),
                    std::sync::atomic::Ordering::SeqCst,
                );
            }));
        }
        let started = std::time::Instant::now();
        slot.close();
        assert!(
            started.elapsed() < REAP_BOUND + Duration::from_millis(500),
            "close took {:?}",
            started.elapsed()
        );
        assert!(
            free_during_reap.load(std::sync::atomic::Ordering::SeqCst),
            "the slot's mutex was held across the reap"
        );
        assert!(!pid_is_alive(pid), "pid {pid} survived close()");
        assert!(slot.is_closed());
    }

    /// **`close_now` kills a live forward's child whoever holds it, and the
    /// forward stays closed**: a later `ensure` answers `shutting_down` and
    /// spawns nothing.
    #[tokio::test]
    async fn close_now_kills_the_child_and_refuses_every_later_ensure() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let forward = Arc::new(
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve"),
        );
        let tunnel = tunnel_once_started(forward.port(), &log, 1);
        forward.ensure().await.expect("the first ensure");
        let _tunnel = tunnel.await.expect("the tunnel task");
        let pids = spawned_pids(&log);
        assert_eq!(pids.len(), 1);

        // A second holder, as an aborted pump's future would be.
        let held = Arc::clone(&forward);
        let closing = Arc::clone(&forward);
        tokio::task::spawn_blocking(move || closing.close_now())
            .await
            .expect("close_now");
        assert!(
            !pid_is_alive(pids[0]),
            "the forward's child outlived close_now"
        );

        let err = held
            .ensure()
            .await
            .expect_err("a closed forward stays closed");
        assert!(err.0.starts_with(SHUTTING_DOWN), "{err}");
        assert_eq!(spawned_pids(&log), pids, "a closed forward spawned again");
    }

    /// **An `ensure` in flight when the forward is closed ends within about one
    /// poll, and its child is dead on return** — the readiness loop re-checks
    /// the slot every poll, and `close_now` waits (bounded) for it.
    #[tokio::test]
    async fn close_now_ends_an_ensure_in_flight_and_its_child() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        // Nothing ever listens on the port: the ensure sits in its readiness loop.
        let forward = Arc::new(
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30")).expect("reserve"),
        );
        let ensuring = Arc::clone(&forward);
        let task = tokio::spawn(async move { ensuring.ensure().await });
        wait_for("the forward process to start", || {
            !spawned_pids(&log).is_empty()
        })
        .await;
        let pid = spawned_pids(&log)[0];

        let closing = Arc::clone(&forward);
        let started = std::time::Instant::now();
        tokio::task::spawn_blocking(move || closing.close_now())
            .await
            .expect("close_now");
        let outcome = tokio::time::timeout(Duration::from_secs(5), task)
            .await
            .expect("the in-flight ensure returned")
            .expect("the ensure task");
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "took {:?}",
            started.elapsed()
        );
        let err = outcome.expect_err("an ensure closed mid-flight fails");
        assert!(err.0.starts_with(SHUTTING_DOWN), "{err}");
        assert!(!pid_is_alive(pid), "pid {pid} outlived the close");
        assert_eq!(spawned_pids(&log).len(), 1, "nothing respawned");
    }

    /// **A closed gate refuses the spawn itself**: the forward's `ensure`
    /// answers `shutting_down` and the fake ssh never ran.
    #[tokio::test]
    async fn a_closed_gate_refuses_the_forward_spawn() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let gate = SpawnGate::new();
        let forward = SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30"))
            .expect("reserve")
            .with_spawn_gate(gate.clone());
        assert!(gate.close(Duration::from_millis(100)).await);
        let err = forward
            .ensure()
            .await
            .expect_err("a closed gate spawns nothing");
        assert!(err.0.starts_with(SHUTTING_DOWN), "{err}");
        assert!(
            spawned_pids(&log).is_empty(),
            "the fake ssh ran past a closed gate"
        );
    }

    /// **The slot is registered with the gate, so the drain closes it whoever
    /// holds the forward**: `close_slots` kills a live tunnel's child while the
    /// forward itself is still alive.
    #[tokio::test]
    async fn the_gate_registers_the_slot_and_its_drain_kills_the_child() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let gate = SpawnGate::new();
        let forward = SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30"))
            .expect("reserve")
            .with_spawn_gate(gate.clone());
        let tunnel = tunnel_once_started(forward.port(), &log, 1);
        forward.ensure().await.expect("the first ensure");
        let _tunnel = tunnel.await.expect("the tunnel task");
        let pid = spawned_pids(&log)[0];

        assert!(gate.close(Duration::from_millis(100)).await);
        let drain = gate.clone();
        let closed = tokio::task::spawn_blocking(move || drain.close_slots())
            .await
            .expect("close_slots");
        assert_eq!(
            closed,
            crate::spawn_gate::SlotsClosed {
                closed: 1,
                unsettled: 0
            },
            "the forward's slot was registered, and settled"
        );
        assert!(!pid_is_alive(pid), "pid {pid} outlived the gate's drain");
        assert!(
            forward.child_is_dead(),
            "the forward still holds a child after the drain"
        );
    }

    // ---- plan 026 C3 fix round 1 ----

    /// **A detached destroy mid-kill is waited for** (F5): the forward's drop
    /// (on `OwnedForward`'s thread) has taken the child and is descheduled
    /// before its reap; the shutdown's slot sweep finds the slot EMPTY — and
    /// waits for that reap (within its bound) instead of returning at once.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_slot_sweep_waits_for_a_destroy_in_progress() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let gate = SpawnGate::new();
        let forward = SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30"))
            .expect("reserve")
            .with_spawn_gate(gate.clone());
        let tunnel = tunnel_once_started(forward.port(), &log, 1);
        forward.ensure().await.expect("the first ensure");
        let _tunnel = tunnel.await.expect("the tunnel task");
        let pid = spawned_pids(&log)[0];

        let (reached_tx, reached_rx) = std::sync::mpsc::channel::<()>();
        let (go_tx, go_rx) = std::sync::mpsc::channel::<()>();
        *forward.child.at_taken.lock().unwrap() = Some(Box::new(move || {
            let _ = reached_tx.send(());
            let _ = go_rx.recv();
        }));
        let destroying = std::thread::spawn(move || drop(forward));
        reached_rx
            .recv()
            .expect("the destroy took the child and paused");

        let sweep = {
            let gate = gate.clone();
            tokio::task::spawn_blocking(move || {
                let started = std::time::Instant::now();
                let closed = gate.close_slots();
                assert_eq!(closed.unsettled, 0, "the sweep settled: {closed:?}");
                started.elapsed()
            })
        };
        tokio::time::sleep(Duration::from_millis(300)).await;
        assert!(
            !sweep.is_finished(),
            "the sweep returned while a kill was still in progress"
        );
        go_tx.send(()).expect("release the destroy");
        let took = sweep.await.expect("the sweep");
        assert!(
            took >= Duration::from_millis(250),
            "the sweep did not wait: {took:?}"
        );
        assert!(
            !pid_is_alive(pid),
            "pid {pid} was alive when the sweep returned"
        );
        destroying.join().expect("the destroy");
    }

    /// Whether `pid` is gone or a zombie — exited (here: SIGKILLed) and not
    /// yet reaped by its parent. `ps -o stat=` (macOS and Linux alike, no
    /// `/proc`); a zombie's state starts with `Z`.
    fn gone_or_zombie(pid: i32) -> bool {
        if !pid_is_alive(pid) {
            return true;
        }
        let out = std::process::Command::new("ps")
            .args(["-o", "stat=", "-p", &pid.to_string()])
            .output()
            .expect("ps");
        let stat = String::from_utf8_lossy(&out.stdout);
        let stat = stat.trim();
        stat.is_empty() || stat.starts_with('Z')
    }

    /// **A destroy descheduled past the sweep's bound has already signalled
    /// its child** (R2-2, plan 026 AM4): the forward's drop took the child
    /// and is paused before its reap for longer than the slot sweep waits.
    /// The sweep returns at its bound and SAYS it did not settle — and the
    /// child, SIGKILLed under the slot's mutex before it left the slot, is
    /// already dead (a zombie the paused thread has yet to reap).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_destroy_paused_past_the_sweeps_bound_has_already_signalled_its_child() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let gate = SpawnGate::new();
        let forward = SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30"))
            .expect("reserve")
            .with_spawn_gate(gate.clone());
        let tunnel = tunnel_once_started(forward.port(), &log, 1);
        forward.ensure().await.expect("the first ensure");
        // Let go of the port at once: the child is what this cell is about,
        // and a listener held through the >1 s pause below only widens the
        // window in which another cell can meet it (a reserved port is
        // released until its tunnel binds it).
        close_tunnel(tunnel.await.expect("the tunnel task"));
        let pid = spawned_pids(&log)[0];

        let (reached_tx, reached_rx) = std::sync::mpsc::channel::<()>();
        let (go_tx, go_rx) = std::sync::mpsc::channel::<()>();
        *forward.child.at_taken.lock().unwrap() = Some(Box::new(move || {
            let _ = reached_tx.send(());
            let _ = go_rx.recv();
        }));
        let destroying = std::thread::spawn(move || drop(forward));
        reached_rx
            .recv()
            .expect("the destroy took the child and paused");

        let (closed, took, dead) = {
            let gate = gate.clone();
            tokio::task::spawn_blocking(move || {
                let started = std::time::Instant::now();
                let closed = gate.close_slots();
                // Read AT the sweep's return, the destroy still paused.
                (closed, started.elapsed(), gone_or_zombie(pid))
            })
            .await
            .expect("the sweep")
        };
        assert_eq!(
            closed,
            crate::spawn_gate::SlotsClosed {
                closed: 1,
                unsettled: 1
            },
            "the sweep reports the reap it abandoned"
        );
        assert!(
            took >= REAP_BOUND - Duration::from_millis(50)
                && took < REAP_BOUND + Duration::from_millis(500),
            "the sweep returned at its bound: {took:?}"
        );
        assert!(
            dead,
            "pid {pid} was still running when the sweep gave up on its reap"
        );
        go_tx.send(()).expect("release the destroy");
        destroying.join().expect("the destroy");
        assert!(!pid_is_alive(pid), "pid {pid} was never reaped");
    }

    /// **A spawn admitted before the close and made after it closes its own
    /// slot** (F1): the section was entered while the gate was open, the close
    /// gave up waiting for it, the spawn happened anyway — and the slot's
    /// registration, finding the gate closed, killed the child at once.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_spawn_after_a_timed_out_close_is_killed_at_registration() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("spawns");
        let gate = SpawnGate::new();
        let forward = Arc::new(
            SshForward::reserve_faked(entry(), fake_ssh(&log, "exec sleep 30"))
                .expect("reserve")
                .with_spawn_gate(gate.clone()),
        );
        let (reached_tx, reached_rx) = std::sync::mpsc::channel::<()>();
        let (go_tx, go_rx) = std::sync::mpsc::channel::<()>();
        *forward.child.at_spawn.lock().unwrap() = Some(Box::new(move || {
            let _ = reached_tx.send(());
            let _ = go_rx.recv();
        }));
        let ensuring = Arc::clone(&forward);
        let ensure = tokio::spawn(async move { ensuring.ensure().await });
        tokio::task::spawn_blocking(move || reached_rx.recv())
            .await
            .expect("join")
            .expect("the spawn section was entered and paused");

        assert!(
            !gate.close(Duration::from_millis(100)).await,
            "the close gave up on the section"
        );
        go_tx.send(()).expect("release the spawn");
        let err = tokio::time::timeout(Duration::from_secs(5), ensure)
            .await
            .expect("the ensure returned")
            .expect("the ensure task")
            .expect_err("a child registered on a closed gate is refused");
        assert!(err.0.starts_with(SHUTTING_DOWN), "{err}");
        let pids = forward.child.spawned.lock().unwrap().clone();
        assert_eq!(pids.len(), 1, "the late spawn happened");
        assert!(
            !pid_is_alive(pids[0]),
            "the late child {} survived its registration",
            pids[0]
        );
        assert!(forward.child.is_closed(), "the slot closed itself");
    }
}
