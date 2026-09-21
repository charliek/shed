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
//! roost watcher ([`crate::roost`]) and the desktop's per-session opencode/gx
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

use std::sync::{Arc, Mutex};
use std::time::Duration;

use shed_core::machine;

/// An arbitrary far-side loopback port for the tests (and the faked-`ssh` seam)
/// that only care that a forward carries whatever port it was handed. It was
/// the RC hub's fixed `1029` until S6 (`charliek/shed#328`) removed the last
/// caller with a constant far side.
#[cfg(test)]
const SOME_REMOTE_PORT: u16 = 1029;

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

    /// A CHEAP "is this still up?" — no network, no blocking syscall, safe to
    /// call on an async worker as often as a caller likes.
    ///
    /// It exists because [`ensure`] is NOT that. `ensure` is the authoritative
    /// answer and it pays for one: it serializes on a lock and probes the local
    /// port. A caller that only wants to know whether it needs to ask — plan
    /// 017's `TauriTransport`, which is invoked before **every** gx request —
    /// would otherwise turn a per-verb hook into a per-verb connect.
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
    /// This is the opencode/gx lane's door (plan 015 §3.4): the agent's HTTP
    /// server binds an ephemeral loopback port, roost reports it as
    /// `server_url`, and the desktop needs a local socket that lands on exactly
    /// that one.
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

    fn looks_alive(&self) -> bool {
        // Just the `try_wait` — deliberately NOT `port_answers`, which is the
        // blocking half. See the trait method's doc for what that costs in
        // precision and why it is the right trade for a per-request caller.
        !self.child_is_dead()
    }

    async fn ensure(&self) -> Result<(), ForwardError> {
        // One ensure at a time (see the `ensuring` field).
        let _serialized = self.ensuring.lock().await;
        if !self.child_is_dead() && self.port_answers().await? {
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
fn kill_child(slot: &ChildSlot) {
    if let Some(mut child) = slot.lock().take() {
        let _ = child.kill();
        let _ = child.wait();
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
#[derive(Default)]
struct ChildSlot(Mutex<Option<std::process::Child>>);

impl ChildSlot {
    /// The guard, ignoring poisoning — this module's rule, for the reason
    /// [`crate::machine`]'s peers give: the data behind it is one `Option`, and
    /// a panic elsewhere must not turn a tunnel into an un-killable one.
    fn lock(&self) -> std::sync::MutexGuard<'_, Option<std::process::Child>> {
        self.0.lock().unwrap_or_else(|e| e.into_inner())
    }
}

impl Drop for ChildSlot {
    fn drop(&mut self) {
        // `get_mut` rather than `lock`: we hold `&mut self`, so there is no
        // contention to wait on and no way to deadlock against a poisoned lock.
        if let Some(mut child) = self.0.get_mut().unwrap_or_else(|e| e.into_inner()).take() {
            let _ = child.kill();
            let _ = child.wait();
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
    slot: &ChildSlot,
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
    *slot.lock() = Some(child);

    let deadline = std::time::Instant::now() + FORWARD_READY_TIMEOUT;
    loop {
        // Exit status first, then readiness (see the doc above).
        let exited = {
            let mut guard = slot.lock();
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

// ---------------------------------------------------------------------------
// one-shot exec (the control verbs)
// ---------------------------------------------------------------------------

/// How long a one-shot machine command may take end to end. Generous because
/// the far side may poll a pane; ssh's own `ConnectTimeout` is what bounds an
/// unreachable host.
const EXEC_TIMEOUT: Duration = Duration::from_secs(60);

/// Spawn `argv`, wait for it, and KILL it if it overruns `timeout`.
///
/// **A `spawn_blocking` task cannot be aborted** — dropping its handle detaches
/// it — so a bare `timeout(spawn_blocking(… .output()))` leaves BOTH the child
/// process and the blocked thread running after the timeout fires, with nothing
/// able to reach either. (The same leak class [`SshForward::ensure`] had.) The
/// child is therefore spawned HERE, where its pid stays reachable; signalling
/// that pid unblocks the waiter, which reaps the child and lets the thread end.
///
/// `wait_with_output` is kept for the wait itself because it drains stdout and
/// stderr concurrently; polling `try_wait` instead would deadlock against a
/// child that fills a pipe buffer before exiting.
///
/// [`SshForward::ensure`]: MachineForward::ensure
async fn run_with_deadline(
    argv: &[String],
    timeout: Duration,
    label: &str,
) -> Result<std::process::Output, String> {
    let (bin, rest) = argv.split_first().expect("argv is never empty");
    let child = std::process::Command::new(bin)
        .args(rest)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
        .map_err(|e| format!("{label}: running ssh: {e}"))?;
    let pid = child.id();
    match tokio::time::timeout(
        timeout,
        tokio::task::spawn_blocking(move || child.wait_with_output()),
    )
    .await
    {
        Ok(joined) => joined
            .map_err(|e| format!("{label}: {e}"))?
            .map_err(|e| format!("{label}: running ssh: {e}")),
        Err(_) => {
            // SIGKILL rather than SIGTERM: ssh with a wedged remote can ignore a
            // polite signal, and by here the caller has already given up. The
            // detached blocking thread reaps the child and exits on its own.
            //
            // The pid cannot have been recycled: the child is un-reaped (the
            // waiter still holds it), so it is a zombie at worst, and a zombie's
            // pid is not reassigned.
            // SAFETY: `kill` has no preconditions beyond a valid signal number.
            unsafe { libc::kill(pid as libc::pid_t, libc::SIGKILL) };
            Err(format!("{label}: the command timed out"))
        }
    }
}

/// Run one command on a machine over SSH and return its stdout.
///
/// The CONTROL half of machine reach. Kept here rather than in a client so the
/// desktop and (via the pure builders in [`shed_core::machine`]) mobile address
/// a machine identically.
///
/// **A non-zero exit reports the remote's stderr, or its STDOUT when stderr is
/// empty.** That fallback is load-bearing and callers are built on it:
/// `shed-gx`'s discovery probe exits 0 and reports in band precisely because a
/// failure here could otherwise quote the token it just printed
/// (`shed_gx::PROBE_SCRIPT`'s rule 1).
pub async fn exec(
    entry: &shed_core::config::MachineEntry,
    remote_argv: &[String],
) -> Result<String, String> {
    let argv = machine::ssh_argv(entry, remote_argv);
    let label = format!("machine:{}", entry.name);
    let out = run_with_deadline(&argv, EXEC_TIMEOUT, &label).await?;

    if !out.status.success() {
        let stderr = String::from_utf8_lossy(&out.stderr);
        let stdout = String::from_utf8_lossy(&out.stdout);
        let detail = if stderr.trim().is_empty() {
            stdout.trim()
        } else {
            stderr.trim()
        };
        let code = out.status.code().unwrap_or(-1);
        let bin = remote_argv.first().map(String::as_str).unwrap_or_default();
        return Err(if detail.is_empty() {
            format!("{label}: {bin} exited {code}")
        } else {
            format!("{label}: {detail}")
        });
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

fn port_answers(port: u16) -> bool {
    std::net::TcpStream::connect(("127.0.0.1", port)).is_ok()
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
        drop(tunnel);
        let tunnel = tunnel_once_started(port, &log, 2);
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

    /// **The timeout must KILL the child, not orphan it.**
    ///
    /// `spawn_blocking` cannot be aborted, so the naive
    /// `timeout(spawn_blocking(… .output()))` leaves a live process and a stuck
    /// thread behind when it fires — invisible in normal runs because the child
    /// eventually exits on its own, and fatal when it does not (a wedged ssh to
    /// an unresponsive machine is exactly that case).
    ///
    /// The child writes its OWN pid to a file before sleeping, and liveness is
    /// then checked with `kill(pid, 0)`. Deterministic, unlike matching a `pgrep`
    /// pattern — which can both miss (escaping) and collide with another test's
    /// process, and would make this assertion vacuous either way.
    #[tokio::test]
    async fn an_overrunning_command_is_killed_not_orphaned() {
        let dir = std::env::temp_dir().join(format!("shed-exec-kill-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");
        let pidfile = dir.join("pid");
        let argv = vec![
            "/bin/sh".to_string(),
            "-c".to_string(),
            // `exec` so the pid recorded IS the sleeping process, not a parent
            // shell that might exit independently.
            format!("echo $$ > {}; exec sleep 30", pidfile.display()),
        ];

        let started = std::time::Instant::now();
        let err = run_with_deadline(&argv, Duration::from_millis(300), "machine:test")
            .await
            .expect_err("a 30s sleep must overrun a 300ms deadline");
        assert!(err.contains("timed out"), "{err}");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "the deadline must not wait out the child: {:?}",
            started.elapsed()
        );

        let pid: i32 = std::fs::read_to_string(&pidfile)
            .expect("the child recorded its pid")
            .trim()
            .parse()
            .expect("a numeric pid");
        // Give the signal a moment to land, then require the process to be gone.
        // `kill(pid, 0)` reports whether it is still signalable — 0 means alive.
        let mut alive = true;
        for _ in 0..50 {
            // SAFETY: signal 0 performs no action; it only probes deliverability.
            if unsafe { libc::kill(pid, 0) } != 0 {
                alive = false;
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        let _ = std::fs::remove_dir_all(&dir);
        assert!(!alive, "the child (pid {pid}) survived the timeout");
    }

    /// The ordinary path still returns the child's output.
    #[tokio::test]
    async fn a_command_that_finishes_returns_its_output() {
        let argv = vec![
            "/bin/sh".to_string(),
            "-c".to_string(),
            "printf hello; printf oops >&2; exit 0".to_string(),
        ];
        let out = run_with_deadline(&argv, Duration::from_secs(10), "machine:test")
            .await
            .expect("it exits well inside the deadline");
        assert_eq!(String::from_utf8_lossy(&out.stdout), "hello");
        assert_eq!(String::from_utf8_lossy(&out.stderr), "oops");
        assert!(out.status.success());
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
        *queued.lock() = Some(child);
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
            slot.lock().as_ref().map(std::process::Child::id),
            Some(pids[0] as u32),
            "the recorded child must be the process that was actually spawned"
        );

        drop(forward);
        assert!(
            slot.lock().is_none(),
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
