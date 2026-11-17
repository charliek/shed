//! Whether the desktop app's embedded broker is running on this machine (plan 035 §3.4).
//!
//! The shed-host-agent daemon and the desktop app's embedded broker share this machine's
//! identity (`<state>/host-agent/machine.json`), so a server sees them as ONE machine and
//! lets only one of them hold each request type. Which one is decided here, locally, and
//! never by timing against the server: while the embedded broker runs, the app holds an
//! EXCLUSIVE `flock` on [`LOCK_FILE_NAME`] beside `machine.json` ([`Holder`]); the daemon
//! looks with a SHARED one ([`check`]) from one watcher per process ([`LockWatcher`]),
//! and stands by while the lock is held. The kernel drops the lock when the holder exits
//! or crashes, so there is no stale state and no timeout: the daemon takes over at its
//! next look.
//!
//! **Why exclusive against shared.** `flock(2)` on Linux and on macOS (BSD) has the same
//! rule: any number of shared locks may be held together, an exclusive lock excludes
//! every other lock on the file, and a non-blocking request that conflicts fails with
//! `EWOULDBLOCK` (`EAGAIN` on Linux; the same value). So checkers never get in each
//! other's way (two daemon processes, or a daemon and a status probe, can look at the
//! same instant and both see "absent"), and a checker's shared lock fails exactly while
//! the app holds the exclusive one: that failure IS "present". A checker opens the file
//! read-only, which is all a shared lock needs (on Linux, where `flock` on NFS is
//! emulated with byte-range locks, a shared lock needs only read access; an exclusive one
//! needs write access, which the app's read-write descriptor has).
//!
//! **The one cost.** While a checker holds its momentary shared lock, the app's
//! non-blocking exclusive request fails too, so the app never gives up after one try: a
//! [`Holder`] retries, quickly at first ([`HOLD_FAST_RETRY`] for [`HOLD_FAST_FOR`]: a
//! check costs it at most one retry) and then every [`HOLD_SLOW_RETRY`] for as long as the
//! app runs. That also covers a second app instance that outlives the first, and a lock
//! path that cannot be used now but can later.
//!
//! The lock file is never removed once created (removing a lock file someone holds
//! breaks the mutex), as `machine.lock` and the credential store's locks are not.
//!
//! Not covered: the two processes configured with different state directories (they are
//! then two machines to the server, each with its own lock), and a state directory on a
//! filesystem whose `flock` does not work between processes (the daemon then cannot see
//! the app, and either may hold a type, as before this lock existed).

use std::collections::HashSet;
use std::fs;
use std::os::unix::fs::OpenOptionsExt as _;
use std::os::unix::io::AsRawFd as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::{oneshot, watch};
use tokio::task::JoinHandle;
use tokio::time::Instant;

use crate::bus::{wait_shutdown, BusLog, PRESENCE_CHECK_INTERVAL};

/// The lock's basename, beside [`crate::machine::MACHINE_FILE_NAME`].
pub const LOCK_FILE_NAME: &str = "embedded-broker.lock";
/// The lock file's mode when [`hold`] creates it.
const FILE_PERM: u32 = 0o600;

/// How often a [`Holder`] tries again at first: a checker's shared lock lasts a few
/// syscalls, so one retry this soon gets past it.
pub const HOLD_FAST_RETRY: Duration = Duration::from_millis(50);
/// How long a [`Holder`] retries at [`HOLD_FAST_RETRY`] before it slows down.
pub const HOLD_FAST_FOR: Duration = Duration::from_secs(1);
/// How often a [`Holder`] tries again after [`HOLD_FAST_FOR`], for as long as it lives.
pub const HOLD_SLOW_RETRY: Duration = Duration::from_secs(2);
/// How long a [`LockWatcher`]'s first look may take before the daemon starts without its
/// answer (reading the lock as not held, and saying why).
pub const FIRST_LOOK_BOUND: Duration = Duration::from_secs(1);

/// The lock beside the machine identity file at `machine_path`: what the embedded broker
/// holds and the daemon checks, both deriving it from the one path they share.
pub fn lock_path_beside(machine_path: &Path) -> PathBuf {
    machine_path
        .parent()
        .unwrap_or_else(|| Path::new("."))
        .join(LOCK_FILE_NAME)
}

/// The lock beside this machine's identity file ([`crate::machine::machine_state_path`]).
pub fn lock_path() -> PathBuf {
    lock_path_beside(&crate::machine::machine_state_path())
}

/// The held lock. Dropping it releases the lock (so does the process ending, however it
/// ends). The descriptor is opened close-on-exec (Rust opens every file so), so a
/// program the app starts with `exec` (an `ssh`, a helper) never inherits it and so never
/// keeps the lock alive after the app is gone. A bare `fork` without `exec` would share
/// the descriptor; the app never does that.
#[derive(Debug)]
pub struct PresenceGuard {
    file: fs::File,
    path: PathBuf,
}

impl PresenceGuard {
    /// The lock file held.
    pub fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for PresenceGuard {
    fn drop(&mut self) {
        // SAFETY: `self.file` owns a live descriptor until this returns. Closing it
        // releases the lock too; unlocking first says so outright.
        unsafe { libc::flock(self.file.as_raw_fd(), libc::LOCK_UN) };
    }
}

/// What one [`hold`] attempt came to.
#[derive(Debug)]
pub enum Hold {
    /// This process holds the lock until the guard is dropped.
    Held(PresenceGuard),
    /// Another descriptor holds a lock on it right now: another instance of the app (an
    /// exclusive lock), or a daemon's look in progress (a momentary shared one).
    HeldElsewhere,
    /// The lock could not be taken (the directory cannot be made, a filesystem without
    /// `flock`).
    Failed(String),
}

/// ONE attempt to take the lock at `path`: create it (0600, its directory as the identity
/// file's is created) and take a NON-blocking exclusive `flock`. Never waits; [`Holder`]
/// is what retries.
pub fn hold(path: &Path) -> Hold {
    if let Some(dir) = path.parent().filter(|d| !d.as_os_str().is_empty()) {
        if let Err(e) = crate::machine::ensure_state_dir(dir) {
            return Hold::Failed(e);
        }
    }
    let file = match fs::OpenOptions::new()
        .create(true)
        .read(true)
        .write(true)
        .truncate(false)
        .mode(FILE_PERM)
        // Never block on the open itself (a FIFO put where the lock belongs).
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
    {
        Ok(f) => f,
        Err(e) => return Hold::Failed(format!("open {}: {e}", path.display())),
    };
    match flock_nb(&file, libc::LOCK_EX) {
        Ok(()) => Hold::Held(PresenceGuard {
            file,
            path: path.to_path_buf(),
        }),
        Err(e) if would_block(&e) => Hold::HeldElsewhere,
        Err(e) => Hold::Failed(format!("lock {}: {e}", path.display())),
    }
}

/// What a look at the lock found ([`check`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Look {
    /// An exclusive lock is held on it: an embedded broker is running.
    Present,
    /// Nobody holds it exclusively, or there is no lock file (no embedded broker has run
    /// here).
    Absent,
    /// The look failed (no permission, a filesystem without `flock`): read as absent, so
    /// the daemon serves rather than going silent.
    Unknown(String),
}

/// Look whether an embedded broker holds the lock at `path`: a NON-blocking SHARED
/// `flock` on a read-only descriptor of its own, released at once when it succeeds.
/// Creates nothing. Checkers never conflict with each other; only the app's exclusive
/// lock makes it fail (`EWOULDBLOCK`: present). File I/O: call it off the async workers
/// ([`LockWatcher`] does).
pub fn check(path: &Path) -> Look {
    let file = match fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
    {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Look::Absent,
        Err(e) => return Look::Unknown(format!("open {}: {e}", path.display())),
    };
    match flock_nb(&file, libc::LOCK_SH) {
        Ok(()) => {
            // SAFETY: `file` owns a live descriptor for the whole call.
            unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_UN) };
            Look::Absent
        }
        Err(e) if would_block(&e) => Look::Present,
        Err(e) => Look::Unknown(format!("lock {}: {e}", path.display())),
    }
}

/// Whether an embedded broker holds the lock at `path` ([`check`]); a failed look is
/// `false`.
pub fn present(path: &Path) -> bool {
    check(path) == Look::Present
}

fn flock_nb(file: &fs::File, kind: libc::c_int) -> std::io::Result<()> {
    // SAFETY: `file` owns a live descriptor for the whole call.
    if unsafe { libc::flock(file.as_raw_fd(), kind | libc::LOCK_NB) } == 0 {
        Ok(())
    } else {
        Err(std::io::Error::last_os_error())
    }
}

fn would_block(e: &std::io::Error) -> bool {
    e.kind() == std::io::ErrorKind::WouldBlock
}

/// Run `f` on a thread of its own and hand back its result. Not tokio's blocking pool:
/// the runtime's shutdown waits for every blocking-pool task, so a look or a lock stalled
/// on a hung filesystem would hang the process's exit. A thread of its own is never
/// joined: a stalled one is left behind, and dies with the process. A thread that cannot
/// be started drops the sender, so the receiver sees an error.
fn off_runtime<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> oneshot::Receiver<T> {
    let (tx, rx) = oneshot::channel();
    let _ = std::thread::Builder::new()
        .name("shed-presence".into())
        .spawn(move || {
            // A receiver gone (its holder or watcher stopped) drops the result here: a
            // guard dropped so releases the lock at once.
            let _ = tx.send(f());
        });
    rx
}

// ---------------------------------------------------------------------------
// The app's side: a holder that never gives up
// ---------------------------------------------------------------------------

/// The retry spacing of a [`Holder`] (the constants above; tests shrink them).
#[derive(Debug, Clone, Copy)]
pub struct HoldRetry {
    pub fast: Duration,
    pub fast_for: Duration,
    pub slow: Duration,
}

impl Default for HoldRetry {
    fn default() -> Self {
        HoldRetry {
            fast: HOLD_FAST_RETRY,
            fast_for: HOLD_FAST_FOR,
            slow: HOLD_SLOW_RETRY,
        }
    }
}

#[derive(Default)]
struct HolderState {
    guard: Option<PresenceGuard>,
    /// The holder was dropped: a lock taken after this is released at once.
    stopped: bool,
}

/// The embedded broker's hold on the lock: a task that tries ([`hold`]) until it holds
/// it, then keeps it until this value is dropped. Each try runs off the async workers.
/// It retries every [`HoldRetry::fast`] for the first [`HoldRetry::fast_for`] (a daemon's
/// momentary look costs one retry), then every [`HoldRetry::slow`] for ever, saying why
/// once per distinct cause: so a lock path that cannot be used now but can later, or a
/// first app instance that exits, lets this one take it. Dropping the holder releases the
/// lock (or stops the trying); so does the process ending.
pub struct Holder {
    state: Arc<Mutex<HolderState>>,
    held: watch::Receiver<Option<Instant>>,
    task: JoinHandle<()>,
    path: PathBuf,
}

impl Holder {
    /// Start holding the lock at `path` (inside a tokio runtime).
    pub fn spawn(path: PathBuf, log: Arc<dyn BusLog>) -> Holder {
        Holder::spawn_with(path, log, HoldRetry::default())
    }

    /// [`Self::spawn`] with its own retry spacing.
    pub fn spawn_with(path: PathBuf, log: Arc<dyn BusLog>, retry: HoldRetry) -> Holder {
        let state = Arc::new(Mutex::new(HolderState::default()));
        let (tx, held) = watch::channel(None);
        let task = tokio::spawn(hold_loop(path.clone(), log, retry, state.clone(), tx));
        Holder {
            state,
            held,
            task,
            path,
        }
    }

    /// The lock file this holder takes.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Whether the lock is held now.
    pub fn is_held(&self) -> bool {
        self.held.borrow().is_some()
    }

    /// When the lock was taken (`None` until it is), as a channel that changes once.
    pub fn held_since(&self) -> watch::Receiver<Option<Instant>> {
        self.held.clone()
    }

    /// Wait at most `within` for the lock to be held; whether it is.
    pub async fn wait_held(&self, within: Duration) -> bool {
        let mut rx = self.held.clone();
        let _ = tokio::time::timeout(within, rx.wait_for(|h| h.is_some())).await;
        self.is_held()
    }
}

impl Drop for Holder {
    fn drop(&mut self) {
        let guard = {
            let mut st = self.state.lock().unwrap_or_else(|p| p.into_inner());
            st.stopped = true;
            st.guard.take()
        };
        // Released here, synchronously, not whenever the runtime gets round to dropping
        // an aborted task.
        drop(guard);
        self.task.abort();
    }
}

async fn hold_loop(
    path: PathBuf,
    log: Arc<dyn BusLog>,
    retry: HoldRetry,
    state: Arc<Mutex<HolderState>>,
    tx: watch::Sender<Option<Instant>>,
) {
    let started = Instant::now();
    let mut said: HashSet<String> = HashSet::new();
    loop {
        let attempt = {
            let path = path.clone();
            off_runtime(move || hold(&path)).await.unwrap_or_else(|_| {
                Hold::Failed("could not start a thread to take the lock".into())
            })
        };
        let fast = started.elapsed() < retry.fast_for;
        match attempt {
            Hold::Held(guard) => {
                let mut st = state.lock().unwrap_or_else(|p| p.into_inner());
                if st.stopped {
                    return; // dropped meanwhile: `guard` is released here
                }
                st.guard = Some(guard);
                drop(st);
                tx.send_replace(Some(Instant::now()));
                log.info(&format!(
                    "embedded broker lock held path={}; a shed-host-agent daemon on this \
                     machine stands by while this app runs{}",
                    path.display(),
                    if said.is_empty() {
                        ""
                    } else {
                        " (taken after the trouble logged above)"
                    }
                ));
                return;
            }
            Hold::HeldElsewhere => {
                // A daemon's look holds it for an instant: said only once it has lasted
                // past the fast retries, when it is another instance of the app.
                if !fast && said.insert("held".into()) {
                    log.warn(&format!(
                        "embedded broker lock is held by another process path={} (another \
                         instance of this app?); trying again every {:?} until it is free",
                        path.display(),
                        retry.slow
                    ));
                }
            }
            Hold::Failed(e) => {
                if said.insert(e.clone()) {
                    log.warn(&format!(
                        "could not take the embedded broker lock; trying again every {:?} \
                         (until then a shed-host-agent daemon on this machine does not know \
                         this app is answering) error={e}",
                        retry.slow
                    ));
                }
            }
        }
        tokio::time::sleep(if fast { retry.fast } else { retry.slow }).await;
    }
}

// ---------------------------------------------------------------------------
// The daemon's side: one watcher per process
// ---------------------------------------------------------------------------

/// What a [`LockWatcher`] runs for each look (`check` on its path; tests pass their own).
pub type LookFn = Arc<dyn Fn() -> Look + Send + Sync>;

/// What a [`LockWatcher`] publishes: whether the desktop app's embedded broker runs now,
/// and the YIELD EPOCH, which counts every time it has been seen to start (`present`
/// becoming true). The epoch is what makes a yield permanent for the daemon's work: a
/// watch channel keeps no history, so an app that starts and quits again between two of
/// a subscription's observations can leave `present` reading `false` throughout, but
/// never the epoch unchanged. A request the daemon received, or an answer it began, under
/// an older epoch belongs to whoever answered after that yield, even once the app has
/// gone again ([`crate::bus`]).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Presence {
    /// The embedded broker runs on this machine (the lock is held).
    pub present: bool,
    /// How many times `present` has become true since this process started watching.
    pub epoch: u64,
}

impl Presence {
    /// The value to publish after a look found `present`: the epoch moves on when the app
    /// is seen to start, and only then.
    pub fn next(self, present: bool) -> Presence {
        Presence {
            present,
            epoch: if present && !self.present {
                self.epoch.wrapping_add(1)
            } else {
                self.epoch
            },
        }
    }
}

/// The daemon's view of the lock: ONE watcher per process, which looks ([`check`]) off
/// the async workers every [`PRESENCE_CHECK_INTERVAL`] and publishes the answer on a
/// watch channel that every bus subscription reads ([`Self::subscribe`]); no subscription
/// touches the file itself. A look that fails reads as absent (the daemon serves rather
/// than going silent), said once per distinct failure.
///
/// The channel reads not present (epoch 0) until [`Self::start`] has made the first look, which the
/// daemon awaits before any subscription starts; a first look that takes longer than
/// [`FIRST_LOOK_BOUND`] is not waited for (read as absent, and said), and its answer is
/// published when it comes. One look at a time: a stalled look is waited for, and the
/// watcher stops at shutdown without waiting for it.
pub struct LockWatcher {
    look: LookFn,
    log: Arc<dyn BusLog>,
    tx: watch::Sender<Presence>,
    interval: Duration,
    first_bound: Duration,
    describe: String,
}

impl LockWatcher {
    /// A watcher of the lock at `path`.
    pub fn new(path: PathBuf, log: Arc<dyn BusLog>) -> LockWatcher {
        let describe = path.display().to_string();
        LockWatcher::with_look(Arc::new(move || check(&path)), describe, log)
    }

    /// A watcher of whatever `look` says (`describe` names it in the log).
    pub fn with_look(look: LookFn, describe: String, log: Arc<dyn BusLog>) -> LockWatcher {
        LockWatcher {
            look,
            log,
            tx: watch::channel(Presence::default()).0,
            interval: PRESENCE_CHECK_INTERVAL,
            first_bound: FIRST_LOOK_BOUND,
            describe,
        }
    }

    /// Test seam: other spacing.
    #[cfg(test)]
    fn with_timing(mut self, interval: Duration, first_bound: Duration) -> Self {
        self.interval = interval;
        self.first_bound = first_bound;
        self
    }

    /// The channel every subscription reads: `present` while the app's broker runs, and
    /// the yield epoch ([`Presence`]).
    pub fn subscribe(&self) -> watch::Receiver<Presence> {
        self.tx.subscribe()
    }

    /// Make the first look (waiting at most [`FIRST_LOOK_BOUND`] for it), publish it, and
    /// start the watcher's task, which runs until `shutdown`. Await this before any
    /// subscription starts.
    pub async fn start(self, shutdown: watch::Receiver<bool>) -> JoinHandle<()> {
        let mut said: HashSet<String> = HashSet::new();
        let mut in_flight = Some(self.spawn_look());
        let first = tokio::time::timeout(self.first_bound, in_flight.as_mut().unwrap()).await;
        match first {
            Ok(r) => {
                in_flight = None;
                let present = self.read(r, &mut said);
                self.tx.send_modify(|v| *v = v.next(present));
            }
            Err(_) => {
                self.log.warn(&format!(
                    "the first look at the desktop app's embedded broker lock did not finish \
                     within {:?} (a slow or hung filesystem?) path={}; reading it as not \
                     running, so this daemon answers, until a look finishes",
                    self.first_bound, self.describe
                ));
            }
        }
        tokio::spawn(async move {
            loop {
                let look = match in_flight.take() {
                    Some(look) => look,
                    None => {
                        tokio::select! {
                            _ = wait_shutdown(shutdown.clone()) => return,
                            _ = tokio::time::sleep(self.interval) => {}
                        }
                        self.spawn_look()
                    }
                };
                let r = tokio::select! {
                    _ = wait_shutdown(shutdown.clone()) => return,
                    r = look => r,
                };
                let present = self.read(r, &mut said);
                self.tx.send_if_modified(|v| {
                    let next = v.next(present);
                    let changed = *v != next;
                    *v = next;
                    changed
                });
            }
        })
    }

    fn spawn_look(&self) -> oneshot::Receiver<Look> {
        let look = self.look.clone();
        off_runtime(move || look())
    }

    /// A look's answer as present or not; a failure said once per distinct cause.
    fn read(&self, r: Result<Look, oneshot::error::RecvError>, said: &mut HashSet<String>) -> bool {
        let e = match r {
            Ok(Look::Present) => return true,
            Ok(Look::Absent) => return false,
            Ok(Look::Unknown(e)) => e,
            Err(_) => "could not start a thread to look".to_string(),
        };
        if said.insert(e.clone()) {
            self.log.warn(&format!(
                "cannot tell whether the desktop app's embedded broker is running \
                 (reading it as not running, so this daemon answers): {e}"
            ));
        }
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead as _, BufReader};
    use std::os::unix::fs::PermissionsExt as _;
    use std::process::{Child, Command, Stdio};
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    /// Marks the child's own lines in its output.
    const CHILD_SAYS: &str = "presence-child: ";

    /// The child half of the cross-process cells: the test binary run again with
    /// `SHED_PRESENCE_CHILD=hold:<path>` (take the lock, say `held`, then wait to be
    /// killed, a minute at most) or `check:<path>` (print what a look finds). Without the
    /// variable it is an ordinary, empty test.
    #[test]
    fn presence_child_helper() {
        let Ok(what) = std::env::var("SHED_PRESENCE_CHILD") else {
            return;
        };
        let (mode, path) = what.split_once(':').expect("mode:path");
        match mode {
            "hold" => {
                let Hold::Held(_guard) = hold(Path::new(path)) else {
                    panic!("child could not hold the lock");
                };
                // On a line of its own: the harness has already written the test's
                // name, with no newline, before a test prints anything.
                println!("\n{CHILD_SAYS}held");
                use std::io::Write as _;
                std::io::stdout().flush().unwrap();
                // Killed by the parent; never outlives a parent that failed first.
                std::thread::sleep(std::time::Duration::from_secs(60));
                std::process::exit(0);
            }
            "check" => println!("\n{CHILD_SAYS}present={}", present(Path::new(path))),
            other => panic!("unknown mode {other}"),
        }
    }

    fn child(mode: &str, path: &Path) -> Child {
        Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "presence::tests::presence_child_helper",
                "--nocapture",
                "--test-threads=1",
            ])
            .env("SHED_PRESENCE_CHILD", format!("{mode}:{}", path.display()))
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .unwrap()
    }

    /// What the child said (the rest of its first line marked [`CHILD_SAYS`]).
    fn read_line(c: &mut Child) -> String {
        let out = c.stdout.as_mut().unwrap();
        for line in BufReader::new(out).lines() {
            if let Some(said) = line.unwrap().strip_prefix(CHILD_SAYS) {
                return said.to_string();
            }
        }
        panic!("the child said nothing");
    }

    fn lock_in(dir: &tempfile::TempDir) -> PathBuf {
        lock_path_beside(&dir.path().join("state/host-agent/machine.json"))
    }

    #[test]
    fn the_lock_sits_beside_the_identity_file() {
        assert_eq!(
            lock_path_beside(Path::new("/s/host-agent/machine.json")),
            Path::new("/s/host-agent/embedded-broker.lock")
        );
        assert_eq!(
            lock_path().parent(),
            crate::machine::machine_state_path().parent()
        );
    }

    /// Held: another descriptor of this process sees it present, and so does another
    /// process; the guard dropped, both see it absent. The file (0600) and its directory
    /// are created by `hold`; a second `hold` while it is held is `HeldElsewhere`.
    #[test]
    fn hold_then_present_from_another_descriptor_and_another_process() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        assert!(!present(&path), "no lock file yet: absent");
        assert!(!path.exists(), "a look creates nothing");

        let Hold::Held(guard) = hold(&path) else {
            panic!("hold");
        };
        assert_eq!(guard.path(), path);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(check(&path), Look::Present);
        assert!(matches!(hold(&path), Hold::HeldElsewhere));
        let mut c = child("check", &path);
        assert_eq!(read_line(&mut c), "present=true");
        c.wait().unwrap();

        drop(guard);
        assert_eq!(check(&path), Look::Absent);
        let mut c = child("check", &path);
        assert_eq!(read_line(&mut c), "present=false");
        c.wait().unwrap();
        assert!(path.exists(), "the lock file is never removed");
    }

    /// A holder killed outright (no unlock, no destructor) releases the lock: the kernel
    /// drops it with the process.
    #[test]
    fn a_killed_holder_releases_the_lock() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        let mut c = child("hold", &path);
        assert_eq!(read_line(&mut c), "held");
        assert!(present(&path), "held by the child");
        assert!(matches!(hold(&path), Hold::HeldElsewhere));
        c.kill().unwrap(); // SIGKILL
        c.wait().unwrap();
        assert!(!present(&path), "released with the process");
        assert!(matches!(hold(&path), Hold::Held(_)));
    }

    /// A look that cannot be made is `false`, never an error and never a wait: no such
    /// directory, a path under a regular file, a file this user may not read.
    #[test]
    fn a_missing_or_unlockable_path_is_not_present() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(check(&dir.path().join("no/such/dir/x.lock")), Look::Absent);
        assert!(!present(&dir.path().join("no/such/dir/x.lock")));

        let file = dir.path().join("plain");
        fs::write(&file, b"").unwrap();
        let under = file.join("x.lock");
        assert!(
            matches!(check(&under), Look::Unknown(_)),
            "{:?}",
            check(&under)
        );
        assert!(!present(&under));
        assert!(matches!(hold(&under), Hold::Failed(_)));

        let locked_out = dir.path().join("locked-out.lock");
        fs::write(&locked_out, b"").unwrap();
        fs::set_permissions(&locked_out, fs::Permissions::from_mode(0o000)).unwrap();
        // Root reads anything; the cell means a user who cannot.
        if unsafe { libc::geteuid() } != 0 {
            assert!(matches!(check(&locked_out), Look::Unknown(_)));
            assert!(!present(&locked_out));
        }
    }

    /// A look needs only read access: a lock file this user may read but not write (the
    /// app made it; the daemon runs as the same user, but nothing about the look needs
    /// more) is looked at like any other.
    #[test]
    fn a_look_needs_only_read_access() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        let Hold::Held(guard) = hold(&path) else {
            panic!("hold");
        };
        fs::set_permissions(&path, fs::Permissions::from_mode(0o400)).unwrap();
        assert_eq!(check(&path), Look::Present);
        drop(guard);
        assert_eq!(check(&path), Look::Absent);
    }

    /// Checkers never read each other as the app: many threads looking at once, with
    /// nobody holding the lock exclusively, never see `Present` (with an exclusive look,
    /// two that overlap would). A shared lock held for a long while by one of them does
    /// not change that either.
    #[test]
    fn concurrent_checkers_never_see_each_other() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        drop(hold(&path)); // create the file, then release it
                           // One checker parks a shared lock across the whole run, so every other look
                           // overlaps a checker's lock, not only by chance.
        let parked = fs::File::open(&path).unwrap();
        flock_nb(&parked, libc::LOCK_SH).unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let seen_present = Arc::new(AtomicUsize::new(0));
        let looks = Arc::new(AtomicUsize::new(0));
        let threads: Vec<_> = (0..8)
            .map(|_| {
                let (path, stop, seen, looks) = (
                    path.clone(),
                    stop.clone(),
                    seen_present.clone(),
                    looks.clone(),
                );
                std::thread::spawn(move || {
                    while !stop.load(Ordering::SeqCst) {
                        if check(&path) != Look::Absent {
                            seen.fetch_add(1, Ordering::SeqCst);
                        }
                        looks.fetch_add(1, Ordering::SeqCst);
                    }
                })
            })
            .collect();
        std::thread::sleep(Duration::from_millis(500));
        stop.store(true, Ordering::SeqCst);
        for t in threads {
            t.join().unwrap();
        }
        assert!(looks.load(Ordering::SeqCst) > 1000, "the checkers ran");
        assert_eq!(
            seen_present.load(Ordering::SeqCst),
            0,
            "a checker read another checker as the app"
        );
        drop(parked);
    }

    #[derive(Default)]
    struct Lines(Mutex<Vec<String>>);
    impl Lines {
        fn lines(&self) -> Vec<String> {
            self.0.lock().unwrap().clone()
        }
    }
    impl BusLog for Lines {
        fn info(&self, m: &str) {
            self.0.lock().unwrap().push(m.to_string());
        }
        fn warn(&self, m: &str) {
            self.0.lock().unwrap().push(m.to_string());
        }
        fn debug(&self, _m: &str) {}
        fn error(&self, m: &str) {
            self.0.lock().unwrap().push(m.to_string());
        }
    }

    /// A holder whose first try lands on a checker's shared lock does not give up: it
    /// takes the lock once that look is over, within its fast retries. (A single try,
    /// what the app did before, is told "held elsewhere" and runs without the lock.)
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_holder_gets_past_a_look_in_progress() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        drop(hold(&path));
        // A look in progress: a shared lock held across the holder's first try.
        let look = fs::File::open(&path).unwrap();
        flock_nb(&look, libc::LOCK_SH).unwrap();
        let log = Arc::new(Lines::default());
        let h = Holder::spawn(path.clone(), log.clone());
        assert!(
            !h.wait_held(Duration::from_millis(150)).await,
            "not while the look runs"
        );
        drop(look);
        assert!(
            h.wait_held(HOLD_FAST_RETRY * 4).await,
            "taken within the fast retries once the look is over"
        );
        assert_eq!(check(&path), Look::Present);
        assert!(
            !log.lines()
                .iter()
                .any(|l| l.contains("held by another process")),
            "a look in progress is not reported as another app: {:?}",
            log.lines()
        );
        drop(h);
        assert_eq!(
            check(&path),
            Look::Absent,
            "released when the holder is dropped"
        );
    }

    /// The race of the review, many times over with real threads: checker threads keep
    /// looking (as several daemons would, far more often than the real two seconds) while
    /// a holder starts; every round the holder takes the lock within its fast retries,
    /// and no checker ever reads `Present` before the holder has it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn holders_win_against_hammering_checkers_every_round() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        drop(hold(&path));
        // Phases, by an epoch the test bumps: even = nobody may hold the lock exclusively
        // (a checker's `Present` is then wrong), odd = a holder may. A look counts only
        // when the epoch is the same, and even, before and after it.
        let epoch = Arc::new(AtomicUsize::new(0));
        let stop = Arc::new(AtomicBool::new(false));
        let false_present = Arc::new(AtomicUsize::new(0));
        let counted = Arc::new(AtomicUsize::new(0));
        let checkers: Vec<_> = (0..6)
            .map(|_| {
                let (path, stop, epoch, fp, counted) = (
                    path.clone(),
                    stop.clone(),
                    epoch.clone(),
                    false_present.clone(),
                    counted.clone(),
                );
                std::thread::spawn(move || {
                    while !stop.load(Ordering::SeqCst) {
                        let before = epoch.load(Ordering::SeqCst);
                        let look = check(&path);
                        let after = epoch.load(Ordering::SeqCst);
                        if before == after && before % 2 == 0 {
                            counted.fetch_add(1, Ordering::SeqCst);
                            if look == Look::Present {
                                fp.fetch_add(1, Ordering::SeqCst);
                            }
                        }
                        // Far more often than a real daemon looks (every 2 s), never in
                        // a tight spin: `flock` has no queue, so a ceaseless stream of
                        // shared locks would keep any exclusive one out.
                        std::thread::sleep(Duration::from_micros(500));
                    }
                })
            })
            .collect();
        for round in 0..30 {
            std::thread::sleep(Duration::from_millis(20)); // checkers only
            epoch.fetch_add(1, Ordering::SeqCst); // odd: a holder may hold
            let start = std::time::Instant::now();
            let h = Holder::spawn(path.clone(), Arc::new(Lines::default()));
            assert!(
                h.wait_held(HOLD_FAST_FOR).await,
                "round {round}: not held within the fast retries"
            );
            let took = start.elapsed();
            assert!(took < HOLD_FAST_FOR, "round {round}: {took:?}");
            std::thread::sleep(Duration::from_millis(10));
            drop(h); // released synchronously
            assert_eq!(check(&path), Look::Absent, "round {round}: released");
            epoch.fetch_add(1, Ordering::SeqCst); // even: nobody holds
        }
        std::thread::sleep(Duration::from_millis(20));
        stop.store(true, Ordering::SeqCst);
        for c in checkers {
            c.join().unwrap();
        }
        assert!(counted.load(Ordering::SeqCst) > 100, "looks were counted");
        assert_eq!(
            false_present.load(Ordering::SeqCst),
            0,
            "a checker read present while nobody held the lock"
        );
    }

    /// A second holder, started while the first holds the lock, keeps trying and takes it
    /// once the first lets go — with no restart; and says once that another holds it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_second_holder_takes_over_when_the_first_lets_go() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        let quick = HoldRetry {
            fast: Duration::from_millis(10),
            fast_for: Duration::from_millis(50),
            slow: Duration::from_millis(100),
        };
        let first = Holder::spawn_with(path.clone(), Arc::new(Lines::default()), quick);
        assert!(first.wait_held(Duration::from_secs(2)).await);
        let log = Arc::new(Lines::default());
        let second = Holder::spawn_with(path.clone(), log.clone(), quick);
        assert!(!second.wait_held(Duration::from_millis(400)).await);
        let said = log
            .lines()
            .iter()
            .filter(|l| l.contains("held by another process"))
            .count();
        assert_eq!(said, 1, "said once: {:?}", log.lines());
        drop(first);
        assert!(
            second.wait_held(Duration::from_secs(2)).await,
            "the second takes it without a restart"
        );
        assert!(present(&path));
    }

    /// A lock path that cannot be used at first (its directory is a plain file) but can
    /// later is taken once it can; the failure is said once, not every retry.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_holder_takes_the_lock_once_its_path_becomes_usable() {
        let dir = tempfile::tempdir().unwrap();
        let blocker = dir.path().join("host-agent");
        fs::write(&blocker, b"").unwrap();
        let path = blocker.join(LOCK_FILE_NAME);
        let quick = HoldRetry {
            fast: Duration::from_millis(10),
            fast_for: Duration::from_millis(50),
            slow: Duration::from_millis(50),
        };
        let log = Arc::new(Lines::default());
        let h = Holder::spawn_with(path.clone(), log.clone(), quick);
        assert!(!h.wait_held(Duration::from_millis(400)).await);
        let failures = log
            .lines()
            .iter()
            .filter(|l| l.contains("could not take"))
            .count();
        assert_eq!(failures, 1, "said once: {:?}", log.lines());
        fs::remove_file(&blocker).unwrap();
        assert!(
            h.wait_held(Duration::from_secs(2)).await,
            "taken once usable"
        );
        assert!(present(&path));
    }

    /// A holder dropped before it got the lock stops trying, and never holds it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_dropped_holder_stops_trying() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        let Hold::Held(other) = hold(&path) else {
            panic!("hold");
        };
        let quick = HoldRetry {
            fast: Duration::from_millis(5),
            fast_for: Duration::from_millis(20),
            slow: Duration::from_millis(5),
        };
        let h = Holder::spawn_with(path.clone(), Arc::new(Lines::default()), quick);
        tokio::time::sleep(Duration::from_millis(50)).await;
        drop(h);
        drop(other);
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(check(&path), Look::Absent, "nobody took it after the drop");
    }

    // ---- the daemon's watcher ----------------------------------------------------------

    /// Whether `rx` reads `present == want` within `within`. Only the value counts: a
    /// watcher that stops (its sender dropped) without publishing it is `false`.
    async fn wait_value(rx: &mut watch::Receiver<Presence>, want: bool, within: Duration) -> bool {
        matches!(
            tokio::time::timeout(within, rx.wait_for(|v| v.present == want)).await,
            Ok(Ok(_))
        )
    }

    /// The helper above is not satisfied by a watcher that went away: a dropped sender
    /// that never published the value reads `false`, not success.
    #[tokio::test]
    async fn wait_value_needs_the_value_not_a_closed_channel() {
        let (tx, mut rx) = watch::channel(Presence::default());
        drop(tx);
        assert!(!wait_value(&mut rx, true, Duration::from_millis(100)).await);
        let (tx, mut rx) = watch::channel(Presence::default());
        tx.send_modify(|v| *v = v.next(true));
        assert!(wait_value(&mut rx, true, Duration::from_millis(100)).await);
    }

    /// The epoch counts each start of the app, and only a start: absent, present, present
    /// again (no change), absent, present is two starts.
    #[test]
    fn the_epoch_counts_each_start() {
        let mut v = Presence::default();
        assert_eq!(v.epoch, 0);
        for (present, epoch) in [(false, 0), (true, 1), (true, 1), (false, 1), (true, 2)] {
            v = v.next(present);
            assert_eq!(v, Presence { present, epoch });
        }
    }

    /// The watcher publishes the real lock: present while held, absent once released,
    /// within an interval; the first look is made before `start` returns.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_watcher_publishes_the_lock() {
        let dir = tempfile::tempdir().unwrap();
        let path = lock_in(&dir);
        let Hold::Held(guard) = hold(&path) else {
            panic!("hold");
        };
        let w = LockWatcher::new(path.clone(), Arc::new(Lines::default()))
            .with_timing(Duration::from_millis(50), FIRST_LOOK_BOUND);
        let mut rx = w.subscribe();
        assert!(!rx.borrow().present, "absent until the first look");
        let (tx, shutdown) = watch::channel(false);
        let task = w.start(shutdown).await;
        assert_eq!(
            *rx.borrow(),
            Presence {
                present: true,
                epoch: 1
            },
            "the first look is in before start returns"
        );
        drop(guard);
        assert!(wait_value(&mut rx, false, Duration::from_secs(2)).await);
        assert_eq!(rx.borrow().epoch, 1, "a quit does not move the epoch");
        let Hold::Held(_again) = hold(&path) else {
            panic!("hold");
        };
        assert!(wait_value(&mut rx, true, Duration::from_secs(2)).await);
        assert_eq!(rx.borrow().epoch, 2, "a second start moves it");
        let _ = tx.send(true);
        tokio::time::timeout(Duration::from_secs(2), task)
            .await
            .expect("stops at shutdown")
            .unwrap();
    }

    /// A first look that does not finish within its bound is not waited for: the watcher
    /// starts reading absent, says why once, and publishes the look's answer when it
    /// comes. A look stalled at shutdown does not hold the watcher up. Never two looks at
    /// once.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_stalled_look_is_bounded_at_start_and_never_holds_shutdown() {
        let (release_tx, release_rx) = std::sync::mpsc::channel::<Look>();
        let release_rx = Arc::new(Mutex::new(release_rx));
        let running = Arc::new(AtomicUsize::new(0));
        let most = Arc::new(AtomicUsize::new(0));
        let look: LookFn = {
            let (running, most) = (running.clone(), most.clone());
            Arc::new(move || {
                let now = running.fetch_add(1, Ordering::SeqCst) + 1;
                most.fetch_max(now, Ordering::SeqCst);
                // Each look waits for the test to say what it finds.
                let r = release_rx.lock().unwrap().recv().unwrap_or(Look::Absent);
                running.fetch_sub(1, Ordering::SeqCst);
                r
            })
        };
        let log = Arc::new(Lines::default());
        let w = LockWatcher::with_look(look, "test".into(), log.clone())
            .with_timing(Duration::from_millis(20), Duration::from_millis(100));
        let mut rx = w.subscribe();
        let (tx, shutdown) = watch::channel(false);
        let started = std::time::Instant::now();
        let task = w.start(shutdown).await;
        assert!(
            started.elapsed() < Duration::from_secs(1),
            "start was bounded"
        );
        assert!(
            !rx.borrow().present,
            "absent while the first look is stalled"
        );
        assert!(
            log.lines()
                .iter()
                .any(|l| l.contains("did not finish within")),
            "{:?}",
            log.lines()
        );
        // The stalled first look answers: published.
        release_tx.send(Look::Present).unwrap();
        assert!(wait_value(&mut rx, true, Duration::from_secs(2)).await);
        // The next look stalls; shutdown does not wait for it.
        tokio::time::sleep(Duration::from_millis(100)).await;
        let _ = tx.send(true);
        tokio::time::timeout(Duration::from_millis(500), task)
            .await
            .expect("shutdown with a look stalled")
            .unwrap();
        assert_eq!(most.load(Ordering::SeqCst), 1, "one look at a time");
        drop(release_tx); // the stalled look ends (its thread was never joined)
    }

    /// A look that fails reads absent and is said once per distinct failure, not every
    /// interval.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_failed_look_reads_absent_and_is_said_once() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("plain");
        fs::write(&file, b"").unwrap();
        let log = Arc::new(Lines::default());
        let w = LockWatcher::new(file.join("x.lock"), log.clone())
            .with_timing(Duration::from_millis(10), FIRST_LOOK_BOUND);
        let rx = w.subscribe();
        let (tx, shutdown) = watch::channel(false);
        let task = w.start(shutdown).await;
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert!(!rx.borrow().present);
        let lines = log.lines();
        assert_eq!(
            lines.iter().filter(|l| l.contains("cannot tell")).count(),
            1,
            "{lines:?}"
        );
        let _ = tx.send(true);
        let _ = task.await;
    }
}
