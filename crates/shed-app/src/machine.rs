//! **The machine transport seam** and the long-lived hub watcher on top of it
//! (plan 012, roadmap R4).
//!
//! `machines:` has existed in [`shed_core::config`] since plan 009, but for two
//! plans the only thing that read it was the `sx` porcelain. This module is the
//! shared half the clients consume: the pure addressing lives in
//! [`shed_core::machine`], the hub wire in [`shed_core::hub_client`], and what
//! is left — the part that genuinely differs per client — is exactly one thing.
//!
//! ## The seam is a local port, and nothing above it is per-client
//!
//! A machine's hub answers on ITS `127.0.0.1:1029`, so every client needs some
//! way to get a local socket that proxies there. That is the whole of the
//! difference:
//!
//! | client | how it gets the port |
//! |---|---|
//! | `sx`, Tauri | [`SshForward`] — an `ssh -N -L` child process |
//! | shed-mobile | a `dartssh2` local-forward bridge on the Dart side; Rust is handed the port ([`FixedPort`]) |
//!
//! Everything above the port — health probing, the snapshot, the SSE feed,
//! reconnect/backoff/resync — is shared, which is why [`MachineHubWatcher`]
//! takes a `dyn MachineForward` and never learns which kind it has.
//!
//! Note mobile does NOT implement this trait from Dart: it stands the bridge up
//! itself and passes the resulting `u16` into [`FixedPort`]. Rust never calls
//! into Dart, matching the inverted shape shed-mobile already uses for one-shot
//! RC exec (Rust builds argv, Dart runs it, Rust decodes).
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

use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::mpsc;

use shed_core::hub_client::{HubClient, HubError, HUB_PORT};
use shed_core::machine;
use shed_core::rc::RcSessionDto;
use shed_core::rc_events::RcEvent;

use crate::backoff;

/// How long to wait for a freshly-established forward's local end to answer.
const FORWARD_READY_TIMEOUT: Duration = Duration::from_secs(10);

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

/// An `ssh -N -L <port>:127.0.0.1:1029 <machine>` child process — the desktop
/// implementation, shared by `sx` and the Tauri app.
///
/// The child is killed and reaped on drop, so an early return or a Ctrl-C can
/// never leave a forward running. `ensure` respawns onto the SAME local port
/// when the child has exited; `ExitOnForwardFailure=yes` in the argv means a
/// lost race for that port is an immediate visible failure rather than a tunnel
/// that silently forwards nothing.
pub struct SshForward {
    entry: shed_core::config::MachineEntry,
    port: u16,
    /// The label the tunnel is described by in errors — the machine's NAME,
    /// not its `user@host`, so a message reads in the same vocabulary as the
    /// `--on machine:<name>` the user typed.
    label: String,
    /// The live child. An `Arc` because the blocking spawn task stores the
    /// child itself the instant it exists (see [`MachineForward::ensure`]).
    child: Arc<Mutex<Option<std::process::Child>>>,
    /// Serializes `ensure()`. The trait is `Send + Sync` and consumers hold an
    /// `Arc<dyn MachineForward>`, so two callers can race; without this both
    /// would spawn onto the same local port and the loser (killed by
    /// `ExitOnForwardFailure`) could be the one we keep.
    ensuring: tokio::sync::Mutex<()>,
    /// **Test seam** (see [`SshForward::reserve_faked`]): an argv prefix exec'd
    /// in place of `ssh`, with the real ssh argv handed to it as ignored
    /// trailing arguments. Compiled away outside `cfg(test)`.
    #[cfg(test)]
    exec_prefix: Option<Vec<String>>,
}

impl SshForward {
    /// Reserve a local port for this machine's hub. Nothing is spawned until
    /// [`MachineForward::ensure`] runs.
    ///
    /// The port is grabbed the way the engine allocates opencode's: bind `:0`,
    /// read the assignment, release. Racy in principle — and deliberately so,
    /// because the alternative (holding the socket) is what would prevent ssh
    /// from binding it at all.
    pub fn reserve(entry: shed_core::config::MachineEntry) -> Result<Self, ForwardError> {
        let port = free_loopback_port()
            .map_err(|e| ForwardError(format!("allocating a local forward port: {e}")))?;
        let label = format!("machine:{}", entry.name);
        Ok(Self {
            entry,
            port,
            label,
            child: Arc::new(Mutex::new(None)),
            ensuring: tokio::sync::Mutex::new(()),
            #[cfg(test)]
            exec_prefix: None,
        })
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
        let mut f = Self::reserve(entry)?;
        f.exec_prefix = Some(exec_prefix);
        Ok(f)
    }

    /// The ssh argv this forward spawns — exposed so a caller can print it.
    pub fn argv(&self) -> Vec<String> {
        machine::forward_argv(&self.entry, self.port, HUB_PORT)
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

    /// Is the child gone (exited, reaped elsewhere, or never spawned)?
    fn child_is_dead(&self) -> bool {
        let mut guard = self.child.lock().unwrap_or_else(|e| e.into_inner());
        match guard.as_mut() {
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

    async fn ensure(&self) -> Result<(), ForwardError> {
        // One ensure at a time (see the `ensuring` field).
        let _serialized = self.ensuring.lock().await;
        if !self.child_is_dead() && port_answers(self.port) {
            return Ok(());
        }
        // Kill any predecessor BEFORE spawning: a child that is merely
        // unresponsive (rather than exited) still holds the local port, and
        // leaving it running would both orphan it and doom the replacement to
        // `ExitOnForwardFailure`.
        kill_child(&self.child);

        let argv = self.spawn_argv();
        let port = self.port;
        let label = self.label.clone();
        let slot = Arc::clone(&self.child);
        // Spawning and the readiness poll are both blocking; keep them off the
        // async worker so a slow-to-refuse host cannot stall the runtime.
        //
        // The slot is handed INTO the blocking task so the child is recorded
        // the instant it exists. `spawn_blocking` tasks cannot be aborted — if
        // this future is dropped mid-await (a watcher being stopped, which is
        // routine), the task still runs to completion, and storing the child
        // only after `.await` would orphan an `ssh -N -L` process that nothing
        // can ever kill: `std::process::Child`'s `Drop` does not kill.
        let outcome =
            tokio::task::spawn_blocking(move || spawn_and_wait(&slot, &argv, port, &label))
                .await
                .map_err(|e| ForwardError(format!("forward task failed: {e}")))?;
        if outcome.is_err() {
            // A failed attempt must not leave a half-established tunnel behind.
            kill_child(&self.child);
        }
        outcome
    }
}

impl Drop for SshForward {
    fn drop(&mut self) {
        kill_child(&self.child);
    }
}

/// Kill + reap whatever child is in `slot`, and empty it. Idempotent, and safe
/// on an already-reaped child (`try_wait` caches the status and `kill` refuses
/// to signal a reaped process, so no recycled PID can be hit).
fn kill_child(slot: &Mutex<Option<std::process::Child>>) {
    if let Some(mut child) = slot.lock().unwrap_or_else(|e| e.into_inner()).take() {
        let _ = child.kill();
        let _ = child.wait();
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
    slot: &Mutex<Option<std::process::Child>>,
    argv: &[String],
    port: u16,
    label: &str,
) -> Result<(), ForwardError> {
    let (bin, rest) = argv.split_first().expect("ssh argv is never empty");
    let child = std::process::Command::new(bin)
        .args(rest)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .spawn()
        .map_err(|e| ForwardError(format!("opening the hub tunnel to {label}: {e}")))?;
    // Recorded before anything can fail or be cancelled — this is the only
    // window in which the process is untracked, and it is now just `spawn`.
    *slot.lock().unwrap_or_else(|e| e.into_inner()) = Some(child);

    let deadline = std::time::Instant::now() + FORWARD_READY_TIMEOUT;
    loop {
        // Exit status first, then readiness (see the doc above).
        let exited = {
            let mut guard = slot.lock().unwrap_or_else(|e| e.into_inner());
            match guard.as_mut() {
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
        std::thread::sleep(Duration::from_millis(100));
    }
}

fn port_answers(port: u16) -> bool {
    std::net::TcpStream::connect(("127.0.0.1", port)).is_ok()
}

/// An unused loopback port: bind `:0`, read the assignment, release.
///
/// A deliberate second copy of `shed_rc_engine::free_loopback_port` rather than
/// a call to it: that crate is behind the `rc` feature and THIS module must not
/// be (shed-mobile links shed-app with default features). Four lines of
/// duplication is the cheaper side of that trade.
fn free_loopback_port() -> std::io::Result<u16> {
    let ln = std::net::TcpListener::bind("127.0.0.1:0")?;
    let port = ln.local_addr()?.port();
    drop(ln);
    Ok(port)
}

// ---------------------------------------------------------------------------
// the long-lived watcher
// ---------------------------------------------------------------------------

/// One update from a [`MachineHubWatcher`].
///
/// Shaped to match [`crate::rc_events_watcher::RcWatcherUpdate`] so a unified
/// sessions view folds both feeds through one consumer — a machine row and a
/// shed row differ in where they came from, not in how they update.
#[derive(Debug, Clone, PartialEq)]
pub enum MachineHubUpdate {
    /// A fresh connection's authoritative snapshot. Emitted on EVERY successful
    /// connect, including the first.
    ///
    /// The hub's `/v1/sessions` is authoritative and the feed is a patch stream
    /// on top of it, so a reconnect is a complete resync by construction — there
    /// is no replay window to negotiate and no gap for the consumer to reason
    /// about. This is why a backgrounded phone can simply stop the watcher and
    /// restart it on foreground.
    Snapshot { sessions: Vec<RcSessionDto> },
    /// A decoded event from the live feed.
    Event { event: RcEvent },
    /// The feed is not up: the forward could not be established, the hub did not
    /// answer, or a live stream ended. The watcher backs off and retries.
    ///
    /// **This is a normal state, not an error.** A machine that is asleep, off
    /// the network, or simply has no hub running is expected, and the consumer
    /// should render its rows as stale-with-a-reason rather than failing.
    Down { reason: String },
}

/// A reconnecting watcher over one machine's hub.
///
/// Constructing it ([`spawn`]) starts the loop; dropping it (or [`stop`]) aborts
/// it. Not restartable — build a new one per subscription, matching
/// [`crate::rc_events_watcher::RcEventsWatcher`].
///
/// [`spawn`]: MachineHubWatcher::spawn
/// [`stop`]: MachineHubWatcher::stop
pub struct MachineHubWatcher {
    machine: String,
    task: tokio::task::JoinHandle<()>,
}

impl MachineHubWatcher {
    /// Spawn the connect-snapshot-stream-retry loop for `forward` onto
    /// `handle`, returning the handle and the update stream. `machine` names the
    /// host to the consumer; the loop keys nothing off it.
    pub fn spawn(
        handle: &tokio::runtime::Handle,
        forward: Arc<dyn MachineForward>,
        machine: String,
    ) -> (MachineHubWatcher, mpsc::UnboundedReceiver<MachineHubUpdate>) {
        Self::spawn_inner(handle, forward, machine, BackoffSleeper::default())
    }

    /// [`spawn`](Self::spawn) with the backoff-sleep seam supplied — the real
    /// clock (`BackoffSleeper::default()`) everywhere but the schedule test.
    fn spawn_inner(
        handle: &tokio::runtime::Handle,
        forward: Arc<dyn MachineForward>,
        machine: String,
        sleeper: BackoffSleeper,
    ) -> (MachineHubWatcher, mpsc::UnboundedReceiver<MachineHubUpdate>) {
        let (tx, rx) = mpsc::unbounded_channel();
        let task = handle.spawn(run_loop(forward, tx, sleeper));
        (MachineHubWatcher { machine, task }, rx)
    }

    /// The machine this watcher follows.
    pub fn machine(&self) -> &str {
        &self.machine
    }

    /// Abort the loop. Aborting drops the in-flight connection future, which
    /// closes the underlying HTTP connection; the forward is torn down when the
    /// last reference to it goes.
    pub fn stop(&self) {
        self.task.abort();
    }
}

impl Drop for MachineHubWatcher {
    fn drop(&mut self) {
        self.stop();
    }
}

/// **Where the loop's backoff sleep goes — a `cfg(test)` seam that is an EMPTY
/// struct in a normal build**, its `sleep` a plain `tokio::time::sleep`.
///
/// The reset rule below is only observable from outside as *when* the next
/// attempt happens, and the schedule is deliberately long (500 ms → 30 s), so
/// asserting it against the real clock would mean sleeping through it. The
/// sibling watcher ([`crate::rc_events_watcher`]) carries the same seam as a
/// full `Sleeper` trait; here nothing outside this file's own tests injects it,
/// so the injectable half is `#[cfg(test)]` and a release build carries a
/// zero-sized value.
#[derive(Default)]
struct BackoffSleeper {
    /// Absent in a normal build: the struct is empty and [`sleep`] is
    /// `tokio::time::sleep`, verbatim.
    ///
    /// [`sleep`]: BackoffSleeper::sleep
    #[cfg(test)]
    scripted: Option<Arc<tests::ScriptedSleeper>>,
}

impl BackoffSleeper {
    async fn sleep(&self, wait: Duration) {
        #[cfg(test)]
        if let Some(scripted) = &self.scripted {
            return scripted.sleep(wait).await;
        }
        tokio::time::sleep(wait).await;
    }
}

async fn run_loop(
    forward: Arc<dyn MachineForward>,
    tx: mpsc::UnboundedSender<MachineHubUpdate>,
    sleeper: BackoffSleeper,
) {
    let mut backoff = backoff::INITIAL;
    loop {
        if tx.is_closed() {
            break;
        }
        // **The reset is keyed on the connection having WORKED, not on how it
        // later ended.** Almost every real disconnect is an `Err` — a stream
        // chunk error, the hub restarting, the forward dropping — so resetting
        // only on a clean end would ratchet the delay up across successful
        // connections and pin a healthy feed at the 30 s ceiling forever. That
        // is also what `rc_events_watcher` does (it resets on the first data of
        // a connection), and these two schedules are meant to stay identical.
        let mut connected = false;
        let outcome = connect_once(&forward, &tx, &mut connected).await;
        if connected {
            backoff = backoff::INITIAL;
        }
        let reason = match outcome {
            Ok(()) => "the hub feed ended".to_string(),
            Err(reason) => reason,
        };
        if tx.send(MachineHubUpdate::Down { reason }).is_err() {
            break;
        }
        let (wait, next) = backoff::step(backoff);
        backoff = next;
        // Race the sleep against the consumer going away: a machine that stays
        // down delivers no events, so a send failure alone would never be
        // observed here and an abandoned receiver would leak the task.
        tokio::select! {
            () = sleeper.sleep(wait) => {}
            _ = tx.closed() => break,
        }
    }
}

/// One full attempt: establish the forward, confirm a hub is there, send the
/// authoritative snapshot, then stream the feed until it ends.
///
/// `connected` is set once the hub has actually answered and the snapshot has
/// gone out — the caller's signal that this attempt worked, whatever happens to
/// the stream afterwards.
async fn connect_once(
    forward: &Arc<dyn MachineForward>,
    tx: &mpsc::UnboundedSender<MachineHubUpdate>,
    connected: &mut bool,
) -> Result<(), String> {
    forward.ensure().await.map_err(|e| e.to_string())?;
    // Built per attempt rather than once, because `HubClient::loopback` is
    // fallible and the port — though stable by the seam's contract — is read
    // from the forward each time; the cost is one `reqwest::Client`, which is
    // cheap next to establishing an SSH tunnel.
    let client = HubClient::loopback(forward.port()).map_err(|e: HubError| e.to_string())?;
    client.health().await.map_err(|e: HubError| e.to_string())?;

    let sessions = client
        .sessions()
        .await
        .map_err(|e: HubError| format!("the hub snapshot failed ({e})"))?;
    *connected = true;
    if tx.send(MachineHubUpdate::Snapshot { sessions }).is_err() {
        return Ok(());
    }

    let (ev_tx, mut ev_rx) = mpsc::unbounded_channel();
    let stream = client.events(&ev_tx);
    tokio::pin!(stream);
    loop {
        tokio::select! {
            result = &mut stream => {
                // Drain anything the stream delivered before ending.
                while let Ok(event) = ev_rx.try_recv() {
                    if tx.send(MachineHubUpdate::Event { event }).is_err() {
                        return Ok(());
                    }
                }
                return result.map_err(|e: HubError| format!("the hub feed stopped ({e})"));
            }
            Some(event) = ev_rx.recv() => {
                if tx.send(MachineHubUpdate::Event { event }).is_err() {
                    return Ok(());
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::path::Path;
    use std::sync::atomic::{AtomicUsize, Ordering};

    // ---- doubles ----

    /// The backoff-sleep seam's test half: record the wait the loop is ABOUT to
    /// take, and return immediately rather than spend it.
    ///
    /// After `park_after` waits it pends forever, which parks the loop at a
    /// known point instead of letting it spin against the mock hub while the
    /// test finishes its assertions.
    pub(super) struct ScriptedSleeper {
        waits: mpsc::UnboundedSender<Duration>,
        taken: AtomicUsize,
        park_after: usize,
    }

    impl ScriptedSleeper {
        fn new(park_after: usize) -> (Arc<ScriptedSleeper>, mpsc::UnboundedReceiver<Duration>) {
            let (waits, rx) = mpsc::unbounded_channel();
            (
                Arc::new(ScriptedSleeper {
                    waits,
                    taken: AtomicUsize::new(0),
                    park_after,
                }),
                rx,
            )
        }

        pub(super) async fn sleep(&self, wait: Duration) {
            let _ = self.waits.send(wait);
            if self.taken.fetch_add(1, Ordering::SeqCst) + 1 >= self.park_after {
                std::future::pending::<()>().await;
            }
        }
    }

    /// A forward that refuses its first `failures_left` `ensure`s and then hands
    /// over a port that works — the "machine is asleep, then wakes up" shape.
    /// The refusals cost no I/O at all, so a whole failing ladder runs in the
    /// test's own time.
    struct FlakyForward {
        port: u16,
        failures_left: AtomicUsize,
    }

    #[async_trait::async_trait]
    impl MachineForward for FlakyForward {
        fn port(&self) -> u16 {
            self.port
        }

        async fn ensure(&self) -> Result<(), ForwardError> {
            if self.failures_left.load(Ordering::SeqCst) > 0 {
                self.failures_left.fetch_sub(1, Ordering::SeqCst);
                return Err(ForwardError("the machine is asleep".to_string()));
            }
            Ok(())
        }
    }

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
    fn tunnel_once_started(
        port: u16,
        log: &Path,
        spawns: usize,
    ) -> tokio::task::JoinHandle<std::net::TcpListener> {
        let log = log.to_path_buf();
        tokio::spawn(async move {
            wait_for("the forward process to start", || {
                spawned_pids(&log).len() >= spawns
            })
            .await;
            std::net::TcpListener::bind(("127.0.0.1", port)).expect("bind the forward's local port")
        })
    }

    /// Poll until `cond` holds. A condition wait, not a fixed sleep: the
    /// expected path returns in microseconds and only a genuine regression pays
    /// the deadline.
    async fn wait_for(what: &str, mut cond: impl FnMut() -> bool) {
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
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
            rc_bin: None,
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
    fn a_reserved_ssh_forward_picks_a_port_and_forwards_the_hub() {
        let f = SshForward::reserve(entry()).expect("reserve");
        assert!(f.port() > 0);
        let argv = f.argv();
        // The concrete thing that matters: this local port maps to the hub's
        // fixed loopback port on the far side, and a lost race is loud.
        assert!(argv.windows(2).any(|w| w
            == [
                "-L",
                &format!("127.0.0.1:{}:127.0.0.1:{HUB_PORT}", f.port())
            ]));
        assert!(argv.contains(&"ExitOnForwardFailure=yes".to_string()));
        assert!(argv.contains(&"-N".to_string()), "runs no remote command");
        assert!(f.child_is_dead(), "nothing is spawned until ensure()");
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
        let f = SshForward::reserve(bad).expect("reserve");

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

    /// A watcher pointed at a port with no hub reports `Down` with a reason and
    /// keeps retrying — the "machine is asleep" case, which must never surface
    /// as a failure.
    #[tokio::test]
    async fn a_missing_hub_is_a_down_update_not_an_error() {
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = ln.local_addr().expect("addr").port();
        drop(ln);

        let (watcher, mut rx) = MachineHubWatcher::spawn(
            &tokio::runtime::Handle::current(),
            Arc::new(FixedPort(port)),
            "mini3".to_string(),
        );
        let update = tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .expect("an update should arrive")
            .expect("the channel stays open");
        match update {
            MachineHubUpdate::Down { reason } => assert!(!reason.is_empty()),
            other => panic!("expected Down, got {other:?}"),
        }
        assert_eq!(watcher.machine(), "mini3");
        watcher.stop();
    }

    /// **Live check against a real machine.** `#[ignore]`d, so CI and a normal
    /// `cargo test` never touch the network; run it deliberately:
    ///
    /// ```text
    /// SX_LIVE_MACHINE=mini3 cargo test -p shed-app --ignored live_machine
    /// ```
    ///
    /// It is here rather than in a scratch script because it is the only thing
    /// that exercises the parts unit tests structurally cannot: a real `ssh -N -L`
    /// child, a real hub answering `/v1/health` behind it, and a real
    /// snapshot — i.e. everything the `sx watch` machine path depends on.
    /// The machine must exist in `~/.shed/config.yaml` and be running a hub.
    #[tokio::test]
    #[ignore = "needs a live machine; set SX_LIVE_MACHINE"]
    async fn live_machine_forward_reaches_a_real_hub() {
        let Ok(name) = std::env::var("SX_LIVE_MACHINE") else {
            panic!("set SX_LIVE_MACHINE=<machine name from ~/.shed/config.yaml>");
        };
        let home = std::env::var("HOME").expect("HOME");
        let cfg = shed_core::config::ShedConfig::load(&format!("{home}/.shed/config.yaml"));
        let entry = shed_core::machine::resolve(&cfg, &name).expect("machine in config");

        let forward = SshForward::reserve(entry.clone()).expect("reserve");
        forward.ensure().await.expect("the tunnel should come up");

        let client = HubClient::loopback(forward.port()).expect("client");
        client.health().await.expect("a real hub should answer");
        let sessions = client.sessions().await.expect("snapshot");
        eprintln!(
            "live: {name} hub on local :{} — {} session(s)",
            forward.port(),
            sessions.len()
        );

        // ensure() is idempotent and must not move the port.
        let port = forward.port();
        forward.ensure().await.expect("second ensure is a no-op");
        assert_eq!(forward.port(), port, "ensure must never change the port");
        drop(forward);

        // **And the FEED, end to end through the watcher.** This is the half
        // unit tests cannot vouch for: a real hub emits `"shed":""`, and while
        // that was required-non-empty every frame decoded to `None` — the
        // snapshot above would still have passed while the feed stayed
        // permanently, invisibly silent.
        let forward = Arc::new(SshForward::reserve(entry.clone()).expect("reserve"));
        let (watcher, mut rx) =
            MachineHubWatcher::spawn(&tokio::runtime::Handle::current(), forward, name.clone());
        let first = tokio::time::timeout(Duration::from_secs(30), rx.recv())
            .await
            .expect("a snapshot should arrive")
            .expect("channel open");
        assert!(
            matches!(first, MachineHubUpdate::Snapshot { .. }),
            "expected the snapshot first, got {first:?}"
        );

        // Poke the machine so the hub emits something, then require a decoded
        // event within the window.
        // The stimulus has to change something the hub OBSERVES, and that has
        // two requirements learned the hard way:
        //
        //  * it must be a real state change — a read-only poke (`tmux
        //    list-sessions`) produces nothing but the ~25 s heartbeat comment;
        //  * the change must OUTLIVE a reconcile tick. The hub polls on a
        //    2 s active / 10 s idle cadence, so a create-then-kill inside one
        //    tick is never seen at all: the session appears and vanishes
        //    between two observations and no event is ever emitted.
        //
        // So: create and LEAVE it, then clean up after the assertion.
        //
        // `std::process`, not `tokio::process`: shed-app's tokio features
        // deliberately exclude `process` (it would enter shed-mobile's default
        // build), and a test may block briefly.
        let rc_bin = entry.rc_bin.as_deref().unwrap_or("sx");
        let dest = shed_core::machine::user_at_host(entry);
        let remote = |cmd: &str| {
            std::process::Command::new("ssh")
                .args(["-o", "BatchMode=yes", &dest, "--", cmd])
                .status()
        };
        let poke = remote(&format!(
            "{rc_bin} rc create --kind shell --name livefeed --slug livefd \
             --target local --created-by live-test >/dev/null 2>&1"
        ));
        eprintln!("live: poke status {poke:?}");

        let deadline = std::time::Instant::now() + Duration::from_secs(45);
        let mut saw_event = false;
        while std::time::Instant::now() < deadline {
            match tokio::time::timeout(Duration::from_secs(10), rx.recv()).await {
                Ok(Some(MachineHubUpdate::Event { event })) => {
                    eprintln!("live: decoded feed event {event:?}");
                    saw_event = true;
                    break;
                }
                Ok(Some(other)) => eprintln!("live: {other:?}"),
                Ok(None) => break,
                Err(_) => {}
            }
        }
        watcher.stop();
        let _ = remote(&format!("{rc_bin} rc kill --slug livefd >/dev/null 2>&1"));
        assert!(
            saw_event,
            "no feed event decoded within the window — the machine hub's \
             frames carry an empty `shed`, and requiring it non-empty drops \
             every one of them"
        );
    }

    /// **The load-bearing claim: a connect always yields the authoritative
    /// snapshot first, then the live feed.** That is what makes a reconnect a
    /// complete resync with no replay protocol to negotiate — and therefore
    /// what lets a backgrounded phone simply stop the watcher and restart it.
    ///
    /// Driven through `FixedPort` against a mock hub, so the REAL `HubClient`
    /// and the REAL loop run with no ssh anywhere. (This is the same shape the
    /// hermetic desktop harness will use.)
    #[tokio::test]
    async fn a_connect_yields_the_snapshot_then_the_feed() {
        let server = httpmock::MockServer::start();
        server.mock(|when, then| {
            when.method(httpmock::Method::GET).path("/v1/health");
            then.status(200).json_body(serde_json::json!({
                "app": shed_core::hub_client::HUB_APP_ID
            }));
        });
        server.mock(|when, then| {
            when.method(httpmock::Method::GET).path("/v1/sessions");
            then.status(200).json_body(serde_json::json!({"sessions": [{
                "slug": "hkn4vd",
                "tmux_session": "rc-hkn4vd",
                "kind": "shell",
                "state": "ready",
                "managed": true,
                "display_name": "plan012-probe"
            }]}));
        });
        server.mock(|when, then| {
            when.method(httpmock::Method::GET).path("/v1/events");
            then.status(200)
                .header("content-type", "text/event-stream")
                // A real machine hub sends an EMPTY shed — the frames are
                // shaped exactly like the ones captured off mini3.
                .body(concat!(
                    "event: activity.changed\n",
                    "data: {\"shed\":\"\",\"slug\":\"hkn4vd\",\"activity\":\"working\",\"state\":\"ready\"}\n",
                    "\n"
                ));
        });

        let (watcher, mut rx) = MachineHubWatcher::spawn(
            &tokio::runtime::Handle::current(),
            Arc::new(FixedPort(server.port())),
            "mini3".to_string(),
        );

        let first = tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .expect("a snapshot should arrive")
            .expect("channel open");
        match first {
            MachineHubUpdate::Snapshot { sessions } => {
                assert_eq!(sessions.len(), 1);
                assert_eq!(sessions[0].slug, "hkn4vd");
            }
            other => panic!("the snapshot must come first, got {other:?}"),
        }

        let second = tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .expect("a feed event should follow")
            .expect("channel open");
        match second {
            MachineHubUpdate::Event { event } => match event {
                RcEvent::ActivityChanged { shed, slug, .. } => {
                    assert_eq!(slug, "hkn4vd");
                    assert_eq!(shed, "", "a directly-read hub names no shed");
                }
                other => panic!("expected ActivityChanged, got {other:?}"),
            },
            other => panic!("expected a feed Event, got {other:?}"),
        }
        watcher.stop();
    }

    /// Dropping the receiver stops the loop rather than leaking the task — the
    /// case a silent (heartbeat-only) feed would otherwise hide.
    #[tokio::test]
    async fn dropping_the_receiver_ends_the_loop() {
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = ln.local_addr().expect("addr").port();
        drop(ln);

        let (watcher, rx) = MachineHubWatcher::spawn(
            &tokio::runtime::Handle::current(),
            Arc::new(FixedPort(port)),
            "mini3".to_string(),
        );
        drop(rx);
        for _ in 0..100 {
            if watcher.task.is_finished() {
                return;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        panic!("the loop should stop once the consumer is gone");
    }

    // ---- the reconnect schedule ----

    /// **The reset is keyed on the connection having WORKED, not on how it
    /// ended.** Almost every real disconnect is an `Err` — a stream chunk error,
    /// the hub restarting, the forward dropping — so a reset that fired only on
    /// a clean end of stream would ratchet the delay up across *successful*
    /// connections and pin a healthy feed at the 30 s ceiling forever.
    ///
    /// Driven through the real loop: two dead attempts to ratchet the delay,
    /// then one that reaches the hub, emits the snapshot, and dies on the feed —
    /// the shape a hub restart has. The wait after THAT must be the initial
    /// delay again, not the doubled one. (`backoff::step`'s own test pins the
    /// numbers; only this one can see which value the loop feeds it.)
    #[tokio::test]
    async fn a_connection_that_worked_resets_the_delay_however_it_later_ended() {
        let server = httpmock::MockServer::start();
        server.mock(|when, then| {
            when.method(httpmock::Method::GET).path("/v1/health");
            then.status(200).json_body(serde_json::json!({
                "app": shed_core::hub_client::HUB_APP_ID
            }));
        });
        server.mock(|when, then| {
            when.method(httpmock::Method::GET).path("/v1/sessions");
            then.status(200)
                .json_body(serde_json::json!({"sessions": []}));
        });
        // The feed refuses: a connection that reached the hub and then FAILED,
        // which is what a hub restart looks like from here.
        server.mock(|when, then| {
            when.method(httpmock::Method::GET).path("/v1/events");
            then.status(503);
        });

        let (sleeper, mut waits) = ScriptedSleeper::new(3);
        let forward = Arc::new(FlakyForward {
            port: server.port(),
            failures_left: AtomicUsize::new(2),
        });
        let (watcher, mut rx) = MachineHubWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            forward,
            "mini3".to_string(),
            BackoffSleeper {
                scripted: Some(sleeper),
            },
        );

        async fn next_wait(waits: &mut mpsc::UnboundedReceiver<Duration>) -> Duration {
            tokio::time::timeout(Duration::from_secs(5), waits.recv())
                .await
                .expect("the loop should reach its backoff")
                .expect("the sleeper outlives the loop")
        }

        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL,
            "a first dead attempt waits the initial delay"
        );
        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL * 2,
            "a second dead attempt ratchets"
        );
        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL,
            "the third attempt reached the hub and sent its snapshot, so the \
             schedule must start over — resetting only on a clean end of stream \
             would leave a healthy feed reconnecting at the ceiling"
        );

        // Everything the loop emitted is already queued (each `Down` precedes
        // the wait we just read), so this needs no further synchronisation.
        let updates: Vec<_> = std::iter::from_fn(|| rx.try_recv().ok()).collect();
        assert!(
            matches!(
                updates.as_slice(),
                [
                    MachineHubUpdate::Down { .. },
                    MachineHubUpdate::Down { .. },
                    MachineHubUpdate::Snapshot { .. },
                    MachineHubUpdate::Down { .. }
                ]
            ),
            "expected two dead attempts, then a snapshot and a lost feed: {updates:?}"
        );
        watcher.stop();
    }

    // ---- the ssh child's lifecycle ----
    //
    // Against a scriptable stand-in for `ssh` (see `SshForward::reserve_faked`)
    // and the test's own listener standing in for a live tunnel, so nothing
    // here needs a real machine.

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
            slot.lock().unwrap().as_ref().map(std::process::Child::id),
            Some(pids[0] as u32),
            "the recorded child must be the process that was actually spawned"
        );

        drop(forward);
        assert!(
            slot.lock().unwrap().is_none(),
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
        drop(tunnel);
        assert!(
            !forward.child_is_dead(),
            "the wedged child is still running"
        );

        let tunnel = tunnel_once_started(forward.port(), &log, 2);
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
}
