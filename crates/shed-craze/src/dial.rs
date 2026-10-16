//! The dial seam (plan 025 §3.3.2, P8): one fresh duplex to `craze bridge
//! --hub` per connection, from a dialer the CLIENT supplies, and the
//! classification of a dial that never reached a working hub.
//!
//! ```text
//! CrazeDial::dial() ─► CrazeStream { reader, writer, stderr tail?, exit? }
//!     ProcessDial  — a local `/bin/sh -c '<ladder>'` child (the desktop's own
//!                    machine; the recipe harness)
//!     TcpDial      — a loopback port (the phone's tunnel to an ssh exec)
//!     (the desktop's `SshExec` duplex, C9, builds a CrazeStream from its child)
//! ```
//!
//! Every connection — the roster, every `createOptions`, every create, and
//! every open lane — dials its own. A dial is never reused: craze's
//! hub answers one request at a time in order, and a splice takes the whole
//! connection.
//!
//! # Classification (feeds `SourceOffline` and the lanes' errors)
//!
//! [`connect_hub`] dials, writes the hub `hello` at once (a hub closes a
//! connection that says nothing within 5 s, craze `internal/hub/server.go:52`
//! — craze#89 item 5, undocumented in protocol.md), and sorts every way that
//! can fail, in this precedence ([`classify_pre_hello`]; plan 025 §3.3.2,
//! extended by plan 032 §3.5 / PC12 — the order is pinned by table cells):
//!
//! 1. the process exited **127** — the ladder's own `craze: command not found`
//!    — → [`DialError::NotInstalled`] (the exit code, so the wording of
//!    whichever `sh` ran it does not matter);
//! 2. it exited before answering `hello` with cobra's **`unknown flag:
//!    --hub`** on stderr (v0.0.1, for `bridge --hub` and for the find-only
//!    `providers --hub` alike — plan 025 Amendment A2), or a hub `hello`
//!    without `rosterSubscribe`/`connect`, an unknown codec, or a
//!    `bad_request`/`protocol_version` refusal → [`DialError::TooOld`];
//! 3. it exited **1** with craze 0.1.x's **`craze bridge: unknown or malformed
//!    flag`** on stderr → [`DialError::NoStartUnsupported`]: a craze too old
//!    for `bridge --hub --no-start`. Only a `--no-start` dial can draw it —
//!    a 0.1.x craze knows every flag of a plain `bridge --hub` — and it is
//!    the one failure [`connect_hub`] may redial past (below);
//! 4. it exited **3** with `no hub is running` on stderr, or nothing at all →
//!    [`DialError::NoHub`]: `bridge --hub --no-start` found no hub to attach
//!    to, and started none (craze `docs/reference/cli.md`, "craze bridge":
//!    exit 3 is the one bridge failure that is not 1);
//! 5. any other failure before `hello` → [`DialError::Unreachable`], whose
//!    reason is the stderr tail plus the first non-JSON stdout line, if any;
//! 6. any other refusal of `hello` → [`DialError::Failed`].
//!
//! **Rule 4 reads an exit code — the documented exception** to this crate's
//! "stderr TEXT, never an exit code alone" rule (the probe's, below; plan 032
//! PC14e): craze documents exit 3 as `--no-start`'s contract, and the code is
//! what tells "nothing to attach to" from every other bridge failure (all exit
//! 1). It still requires the text — or an empty stderr, which no other bridge
//! failure leaves — so an unrelated exit 3 with other words (a login shell's
//! own, say) stays `Unreachable`.
//!
//! **The `--no-start` fallback** (plan 032 PC12): on a
//! [`DialError::NoStartUnsupported`], [`connect_hub`] closes the failed
//! connection (its child killed) and asks the dial's
//! [`CrazeDial::fall_back`] whether it can dial the older way; on `true` it
//! dials ONCE more, and whatever that answers is the answer. The dial owns
//! the mode (the desktop's `SshCrazeDial` swaps to its probe-then-`bridge
//! --hub` legacy for good). A dial that cannot fall back — the default, and
//! every dial in this crate, none of which sends `--no-start` — leaves the
//! error standing, and it reads as `Unreachable` does: [`DialError::cause`]
//! and [`DialError::into_lane_error`] treat the two alike.
//!
//! A dial without a process ([`TcpDial`]) sees neither an exit code nor
//! stderr: everything before `hello` is `Unreachable` there, and on the phone
//! the Dart side's own classifier is authoritative for it (§3.3.2).
//!
//! **The dial itself is under a deadline** ([`DIAL_DEADLINE`]): a dial that
//! never hands back a stream — an ssh exec held at its host-key prompt, a
//! loopback connect that blocks, a test's held dial — is
//! [`DialError::TimedOut`], never a wait without end. A source reads it as
//! `Offline{Unreachable}` and redials with its backoff; a lane counts it toward
//! its outage bound; `createOptions` fails `Unavailable`; and a create, whose
//! outcome its caller must never guess, takes it as an unknown outcome — the
//! one retry under the same request id (nothing was written, so the retry is
//! safe either way).
//!
//! # The find-only probe
//!
//! `craze providers --hub --json` (`shed_core::craze::providers_hub_argv`)
//! never starts a hub. [`classify_probe`] sorts its output — by STDERR TEXT,
//! never by exit code alone, because "no hub is running" and v0.0.1's refusal
//! both exit 1 (Amendment A2) — and [`probe_process`] runs it locally.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::future::BoxFuture;
use shed_core::lane::{LaneError, SourceOffline};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite};
use tokio::process::{Child, Command};
use tokio::sync::{oneshot, watch};

use crate::conn::{hello_hub, lock, Conn, HelloError, HubHello, Notifications};
use crate::errors::craze_says;
use crate::wire::ClientInfo;

/// A boxed read half.
pub type BoxRead = Box<dyn AsyncRead + Send + Unpin>;
/// A boxed write half.
pub type BoxWrite = Box<dyn AsyncWrite + Send + Unpin>;

/// A fresh duplex to `craze bridge --hub`, one per connection, never reused.
pub trait CrazeDial: Send + Sync {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>>;

    /// `err` ended a dial before `hello`: can this dial fall back to a way the
    /// far side understands, so that ONE more dial may succeed? Asked by
    /// [`connect_hub`] only for [`DialError::NoStartUnsupported`] (plan 032
    /// PC12). `true` commits the dial to that way from now on (the desktop's
    /// `SshCrazeDial` swaps from `bridge --hub --no-start` to its
    /// probe-then-`bridge --hub` legacy — once: a dial already there answers
    /// `false`). The default, `false`, is every dial that never sends
    /// `--no-start`. **A dial that wraps another must forward this**, or the
    /// fallback the inner one carries is lost behind the wrapper.
    fn fall_back(&self, _err: &DialError) -> bool {
        false
    }
}

/// One connection's bytes, plus what its process can say about a failure
/// before `hello`: the tail of its stderr and its exit.
pub struct CrazeStream {
    reader: BoxRead,
    writer: BoxWrite,
    stderr_tail: Option<StderrTail>,
    exit: Option<ExitWatch>,
}

impl CrazeStream {
    /// A duplex with no process behind it (a TCP stream, a test's pipe).
    pub fn new<R, W>(reader: R, writer: W) -> CrazeStream
    where
        R: AsyncRead + Send + Unpin + 'static,
        W: AsyncWrite + Send + Unpin + 'static,
    {
        CrazeStream {
            reader: Box::new(reader),
            writer: Box::new(writer),
            stderr_tail: None,
            exit: None,
        }
    }

    /// The process's stderr, for the pre-`hello` classification.
    pub fn with_stderr_tail(mut self, tail: StderrTail) -> CrazeStream {
        self.stderr_tail = Some(tail);
        self
    }

    /// The process's exit — and, through it, its kill: the process is killed
    /// once the last clone of the watch is dropped.
    pub fn with_exit(mut self, exit: ExitWatch) -> CrazeStream {
        self.exit = Some(exit);
        self
    }

    /// The parts, for a dial that wraps another's (a test hook).
    pub fn into_parts(self) -> (BoxRead, BoxWrite, Option<StderrTail>, Option<ExitWatch>) {
        (self.reader, self.writer, self.stderr_tail, self.exit)
    }

    /// [`CrazeStream::into_parts`]'s inverse: a wrapping dial's stream, put
    /// back together around the halves it wrapped.
    pub fn from_parts(
        reader: BoxRead,
        writer: BoxWrite,
        stderr_tail: Option<StderrTail>,
        exit: Option<ExitWatch>,
    ) -> CrazeStream {
        CrazeStream {
            reader,
            writer,
            stderr_tail,
            exit,
        }
    }

    /// The process's exit, when there is one.
    pub fn exit(&self) -> Option<&ExitWatch> {
        self.exit.as_ref()
    }
}

/// Why a dial did not reach a hub this crate can use.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum DialError {
    /// craze is not on this machine: the ladder ran out of rungs (exit 127).
    #[error("craze is not installed on this machine ({0})")]
    NotInstalled(String),
    /// craze is there and too old to speak this protocol.
    #[error("{0}")]
    TooOld(String),
    /// Nothing answered `hello`: a transport that failed, a process that
    /// died, a deadline.
    #[error("{0}")]
    Unreachable(String),
    /// craze answered, and refused.
    #[error("{0}")]
    Failed(String),
    /// The dial itself handed back no stream within its deadline
    /// ([`DIAL_DEADLINE`]): nothing was written.
    #[error("{0}")]
    TimedOut(String),
    /// craze refused `bridge --hub --no-start`'s flag: a craze older than
    /// 0.2.0 (`craze bridge: unknown or malformed flag`, exit 1). The dial's
    /// [`CrazeDial::fall_back`] may take it (the module doc); one that does
    /// not reads as [`DialError::Unreachable`].
    #[error(
        "craze on this machine is older than 0.2.0 and cannot attach only to a running hub ({0})"
    )]
    NoStartUnsupported(String),
    /// `bridge --hub --no-start` found no hub running, and started none
    /// (exit 3): nothing to attach to — [`SourceOffline::NoHub`].
    #[error("no craze hub is running on this machine ({0})")]
    NoHub(String),
}

/// What a call whose `--no-start` dial found no hub is told
/// ([`DialError::into_lane_error`]).
pub const NO_CRAZE_HUB: &str = "no craze hub is running";

impl DialError {
    /// The source-level cause this is. An unconsumed
    /// [`DialError::NoStartUnsupported`] is `Unreachable` (the module doc).
    pub fn cause(&self) -> SourceOffline {
        match self {
            DialError::NotInstalled(_) => SourceOffline::NotInstalled,
            DialError::TooOld(_) => SourceOffline::TooOld,
            DialError::Unreachable(_)
            | DialError::TimedOut(_)
            | DialError::NoStartUnsupported(_) => SourceOffline::Unreachable,
            DialError::Failed(_) => SourceOffline::Failed,
            DialError::NoHub(_) => SourceOffline::NoHub,
        }
    }

    /// The contract error for a call whose dial failed. `NotInstalled`,
    /// `Unreachable`, `TimedOut` and an unconsumed `NoStartUnsupported` are
    /// quiet (`Unavailable`: nothing to talk to), and so is `NoHub`, in its
    /// own words ([`NO_CRAZE_HUB`]); `TooOld` and a refusal are `Failed` with
    /// craze's words.
    pub fn into_lane_error(self) -> LaneError {
        match self {
            e @ (DialError::NotInstalled(_)
            | DialError::Unreachable(_)
            | DialError::TimedOut(_)
            | DialError::NoStartUnsupported(_)) => LaneError::Unavailable(e.to_string()),
            DialError::NoHub(_) => LaneError::Unavailable(NO_CRAZE_HUB.to_string()),
            DialError::TooOld(m) | DialError::Failed(m) => LaneError::Failed(craze_says(m)),
        }
    }
}

/// How long [`probe_process`] waits, after the probe exited, for the rest of
/// its stderr.
const EXIT_GRACE: Duration = Duration::from_secs(2);

/// How long a dial may take to hand back a stream before it counts as a
/// failed dial ([`DialError::TimedOut`], the module doc).
pub const DIAL_DEADLINE: Duration = Duration::from_secs(30);

/// Dial (within `dial_deadline`), say `hello` to the hub (within
/// `hello_deadline`), and judge it — or classify why not. A
/// [`DialError::NoStartUnsupported`] the dial can fall back from
/// ([`CrazeDial::fall_back`]) is dialled past ONCE, under both deadlines
/// afresh (the module doc).
pub async fn connect_hub(
    dial: &dyn CrazeDial,
    client: &ClientInfo,
    dial_deadline: Duration,
    hello_deadline: Duration,
) -> Result<(Conn, Notifications, HubHello), DialError> {
    match connect_once(dial, client, dial_deadline, hello_deadline).await {
        // The failed connection is already closed — its child killed — by
        // `connect_once`, before the dial is asked, or dialled again.
        Err(err @ DialError::NoStartUnsupported(_)) if dial.fall_back(&err) => {
            connect_once(dial, client, dial_deadline, hello_deadline).await
        }
        other => other,
    }
}

/// One dial and its `hello` ([`connect_hub`], without the fallback).
async fn connect_once(
    dial: &dyn CrazeDial,
    client: &ClientInfo,
    dial_deadline: Duration,
    hello_deadline: Duration,
) -> Result<(Conn, Notifications, HubHello), DialError> {
    let stream = match tokio::time::timeout(dial_deadline, dial.dial()).await {
        Ok(stream) => stream?,
        Err(_) => {
            return Err(DialError::TimedOut(format!(
                "the dial to craze handed back no connection within {dial_deadline:?}"
            )))
        }
    };
    let (conn, notes) = Conn::start(stream);
    match hello_hub(&conn, client, hello_deadline).await {
        Ok(hello) => Ok((conn, notes, hello)),
        Err(HelloError::TooOld(m)) => Err(DialError::TooOld(m)),
        Err(HelloError::Refused(m)) => Err(DialError::Failed(m)),
        Err(HelloError::NoAnswer(why)) => {
            let err = classify_no_answer(&conn, &why).await;
            // Nothing more is read or written on it, and its process is
            // killed NOW — not whenever its last holder lets it go — so a
            // fallback's redial never runs beside it.
            conn.close("the dial never reached a hub");
            Err(err)
        }
    }
}

/// How long [`connect_hub`] waits, once the stream ended (or went quiet) before
/// `hello`, for BOTH the process's exit status and the end of its stderr — the
/// two waits run together, inside this one bound.
const PRE_HELLO_GRACE: Duration = Duration::from_secs(5);

/// A connection that ended (or went quiet) before `hello`: wait — together,
/// within [`PRE_HELLO_GRACE`] — for the process to exit and its stderr to end,
/// then read BOTH again and classify on everything known by then. Waiting for
/// one and then the other, each on its own short clock, misses an exit that
/// lands while stderr is still draining (an exit 127 read as `Unreachable`), or
/// a stderr line that lands after the process itself is gone (a child of it
/// still holding the pipe).
async fn classify_no_answer(conn: &Conn, why: &str) -> DialError {
    let exited = async {
        if let Some(exit) = conn.exit() {
            exit.wait(PRE_HELLO_GRACE).await;
        }
    };
    let drained = async {
        match conn.stderr() {
            Some(tail) => tail.wait_eof(PRE_HELLO_GRACE).await,
            None => true,
        }
    };
    let ((), complete) = tokio::join!(exited, drained);
    let mut code = conn.exit().and_then(ExitWatch::now).and_then(|e| e.code);
    let stderr = conn.stderr().map(StderrTail::text).unwrap_or_default();
    // **An exit 3 is craze's "no hub" only on a stderr that ENDED** (C6
    // review): the rule's empty-stderr arm must not read a stderr still open at
    // the grace — something else may yet say why it failed — so that case is
    // judged without the exit code, as any unexplained end is.
    if !complete && code == Some(NO_HUB_EXIT) && stderr.trim().is_empty() {
        code = None;
    }
    let preamble = conn.preamble();
    classify_pre_hello(code, &stderr, preamble.first().map(String::as_str), why)
}

/// The pre-`hello` classification, pure (plan 025 §3.3.2 + Amendment A2, and
/// plan 032 §3.5's two rows: the module doc's precedence, in its order).
/// `code` is the process's exit code when it exited with one; `stdout_line`
/// the first non-JSON line it printed; `why` what the connection itself said.
pub fn classify_pre_hello(
    code: Option<i32>,
    stderr: &str,
    stdout_line: Option<&str>,
    why: &str,
) -> DialError {
    let tail = stderr.trim();
    if code == Some(127) {
        let said = if tail.is_empty() { "exit 127" } else { tail };
        return DialError::NotInstalled(said.to_string());
    }
    if tail.contains(UNKNOWN_FLAG_HUB) {
        return DialError::TooOld(too_old_message(tail));
    }
    if code == Some(1) && tail.contains(BRIDGE_UNKNOWN_FLAG) {
        return DialError::NoStartUnsupported(tail.to_string());
    }
    // The documented exit-code exception (the module doc, rule 4).
    if code == Some(NO_HUB_EXIT) && (tail.is_empty() || tail.contains(NO_HUB_RUNNING)) {
        let said = if tail.is_empty() { "exit 3" } else { tail };
        return DialError::NoHub(said.to_string());
    }
    let mut reason = why.to_string();
    if !tail.is_empty() {
        reason = format!("{reason}; craze said: {tail}");
    }
    if let Some(line) = stdout_line {
        reason = format!("{reason}; before it, the login shell printed: {line}");
    }
    DialError::Unreachable(reason)
}

/// cobra's refusal of a flag v0.0.1 does not have — the only signal v0.0.1
/// gives (plan 025 §3.3.2, Amendment A2).
pub const UNKNOWN_FLAG_HUB: &str = "unknown flag: --hub";
/// What `craze providers --hub` says when it finds no hub to ask — and what
/// `craze bridge --hub --no-start` says with its exit 3 (`craze bridge: no hub
/// is running`).
pub const NO_HUB_RUNNING: &str = "no hub is running";
/// craze 0.1.x's `craze bridge` refusing a flag it does not have: the whole
/// line is `craze bridge: unknown or malformed flag; see craze bridge --help`
/// (craze v0.1.1 `internal/cli/args.go`'s `usageFlagError`, exit 1 by
/// `root.go`'s bridge contract). Only `--no-start` draws it (plan 032 §3.5).
pub const BRIDGE_UNKNOWN_FLAG: &str = "craze bridge: unknown or malformed flag";
/// `craze bridge --hub --no-start`'s one exit that is not 1: no hub is
/// running, and none was started (craze `docs/reference/cli.md`).
pub const NO_HUB_EXIT: i32 = 3;

fn too_old_message(said: &str) -> String {
    format!("craze on this machine is too old for shed (it says {said:?}); update craze")
}

// ---- the process's exit and stderr ----

/// How a process ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Exit {
    /// The exit code; `None` when a signal ended it.
    pub code: Option<i32>,
}

/// A process's exit, watched — and its kill switch. Cheap to clone; the
/// process is killed when the LAST clone is dropped (or at [`ExitWatch::kill`]),
/// so a connection that is dropped never leaves its bridge behind.
#[derive(Clone)]
pub struct ExitWatch {
    status: watch::Receiver<Option<Exit>>,
    switch: Arc<KillSwitch>,
    /// The process's pid, taken at [`ExitWatch::spawn`] — for a log line.
    pid: Option<u32>,
}

struct KillSwitch(Mutex<Option<oneshot::Sender<()>>>);

impl KillSwitch {
    fn pull(&self) {
        if let Some(tx) = lock(&self.0).take() {
            let _ = tx.send(());
        }
    }
}

impl Drop for KillSwitch {
    fn drop(&mut self) {
        self.pull();
    }
}

impl ExitWatch {
    /// Watch `child` from a task of its own: its exit is published, and it is
    /// killed (and reaped) when the watch is dropped or killed first.
    pub fn spawn(mut child: Child) -> ExitWatch {
        let pid = child.id();
        let (status_tx, status) = watch::channel(None);
        let (kill_tx, kill_rx) = oneshot::channel::<()>();
        tokio::spawn(async move {
            let done = tokio::select! {
                done = child.wait() => done,
                // A send or a dropped sender: either way, kill.
                _ = kill_rx => {
                    let _ = child.start_kill();
                    child.wait().await
                }
            };
            let exit = Exit {
                code: done.ok().and_then(|s| s.code()),
            };
            let _ = status_tx.send(Some(exit));
        });
        ExitWatch {
            status,
            switch: Arc::new(KillSwitch(Mutex::new(Some(kill_tx)))),
            pid,
        }
    }

    /// The process's pid, taken when it was spawned (`None` if it had already
    /// been reaped by then). For naming it in a log — never for signalling:
    /// once reaped, the number can belong to someone else.
    pub fn pid(&self) -> Option<u32> {
        self.pid
    }

    /// How it ended, if it has.
    pub fn now(&self) -> Option<Exit> {
        *self.status.borrow()
    }

    /// Wait at most `within` for it to end.
    pub async fn wait(&self, within: Duration) -> Option<Exit> {
        let mut rx = self.status.clone();
        let ended = match tokio::time::timeout(within, rx.wait_for(Option::is_some)).await {
            Ok(Ok(exit)) => *exit,
            _ => None,
        };
        ended.or_else(|| self.now())
    }

    /// Kill it now.
    pub fn kill(&self) {
        self.switch.pull();
    }
}

/// How much of a process's stderr is kept: its tail.
pub const STDERR_TAIL_BYTES: usize = 4 << 10;

/// The last [`STDERR_TAIL_BYTES`] of a process's stderr, read continuously (a
/// process that fills an unread stderr pipe would block) and kept for an error
/// message.
#[derive(Clone)]
pub struct StderrTail {
    buf: Arc<Mutex<Vec<u8>>>,
    eof: watch::Receiver<bool>,
}

impl StderrTail {
    /// Read `stderr` to its end from a task of its own.
    pub fn spawn<R: AsyncRead + Send + Unpin + 'static>(mut stderr: R) -> StderrTail {
        let buf = Arc::new(Mutex::new(Vec::new()));
        let (eof_tx, eof) = watch::channel(false);
        let sink = Arc::clone(&buf);
        tokio::spawn(async move {
            let mut chunk = [0u8; 4096];
            while let Ok(n) = stderr.read(&mut chunk).await {
                if n == 0 {
                    break;
                }
                let mut b = lock(&sink);
                b.extend_from_slice(&chunk[..n]);
                if b.len() > STDERR_TAIL_BYTES {
                    let cut = b.len() - STDERR_TAIL_BYTES;
                    b.drain(..cut);
                }
            }
            let _ = eof_tx.send(true);
        });
        StderrTail { buf, eof }
    }

    /// What it has said, lossily decoded and trimmed.
    pub fn text(&self) -> String {
        let b = lock(&self.buf);
        String::from_utf8_lossy(&b).trim().to_string()
    }

    /// Wait at most `within` for its end; whether it came.
    pub async fn wait_eof(&self, within: Duration) -> bool {
        let mut rx = self.eof.clone();
        tokio::time::timeout(within, rx.wait_for(|eof| *eof))
            .await
            .is_ok_and(|r| r.is_ok())
    }
}

// ---- the local process dial ----

/// The environment a local dial's process gets.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EnvPolicy {
    /// The caller's own (the desktop's localhost dial: its hub is born in the
    /// app's session with the app's environment).
    Inherit,
    /// Exactly these variables and nothing else (`env -i` — craze's recipe,
    /// and the desktop's jailed test mode).
    Exactly(Vec<(String, String)>),
}

/// The absolute shell every local dial runs (plan 025 §3.3.2): a test-mode
/// `PATH` holds only craze's binaries, so `sh` itself is never looked up.
pub const SHELL: &str = "/bin/sh";

/// A local `craze` child per connection: `["sh", "-c", <script>]` from
/// `shed_core::craze`, run as [`SHELL`] by absolute path under an
/// [`EnvPolicy`].
#[derive(Debug, Clone)]
pub struct ProcessDial {
    argv: Vec<String>,
    env: EnvPolicy,
    cwd: Option<PathBuf>,
}

impl ProcessDial {
    /// A dial running `argv` — `["sh", "-c", <script>]`, its `sh` replaced by
    /// [`SHELL`]. Any other argv runs as given (a test's own program).
    pub fn new(argv: Vec<String>, env: EnvPolicy) -> ProcessDial {
        ProcessDial {
            argv,
            env,
            cwd: None,
        }
    }

    /// The production local dial: craze's published ladder running `craze
    /// bridge --hub` (`shed_core::craze::bridge_hub_argv`).
    pub fn bridge_hub(env: EnvPolicy) -> ProcessDial {
        ProcessDial::new(shed_core::craze::bridge_hub_argv(), env)
    }

    /// The jailed ladder (rungs 1–2, no PATH enhancement) — test mode only, so
    /// nothing the run did not put on its own `HOME` or `PATH` can answer.
    pub fn bridge_hub_jailed(env: EnvPolicy) -> ProcessDial {
        ProcessDial::new(shed_core::craze::bridge_hub_argv_jailed(), env)
    }

    /// Run the child in `dir` (default: the caller's working directory).
    pub fn with_cwd(mut self, dir: PathBuf) -> ProcessDial {
        self.cwd = Some(dir);
        self
    }

    fn command(&self) -> Result<Command, DialError> {
        let (first, rest) = self
            .argv
            .split_first()
            .ok_or_else(|| DialError::Unreachable("an empty craze command".to_string()))?;
        let program = if first == "sh" { SHELL } else { first.as_str() };
        let mut cmd = Command::new(program);
        cmd.args(rest);
        if let EnvPolicy::Exactly(vars) = &self.env {
            cmd.env_clear();
            cmd.envs(vars.iter().map(|(k, v)| (k.as_str(), v.as_str())));
        }
        if let Some(dir) = &self.cwd {
            cmd.current_dir(dir);
        }
        Ok(cmd)
    }
}

impl CrazeDial for ProcessDial {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>> {
        let cmd = self.command();
        Box::pin(async move {
            let mut cmd = cmd?;
            cmd.stdin(std::process::Stdio::piped())
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::piped())
                .kill_on_drop(true);
            let mut child = cmd.spawn().map_err(|e| {
                DialError::Unreachable(format!("could not start {SHELL} to reach craze: {e}"))
            })?;
            let (Some(stdin), Some(stdout), Some(stderr)) =
                (child.stdin.take(), child.stdout.take(), child.stderr.take())
            else {
                return Err(DialError::Unreachable(
                    "the craze child has no stdio".to_string(),
                ));
            };
            Ok(CrazeStream::new(stdout, stdin)
                .with_stderr_tail(StderrTail::spawn(stderr))
                .with_exit(ExitWatch::spawn(child)))
        })
    }
}

// ---- the loopback TCP dial ----

/// A dial to `127.0.0.1:<port>` — the phone's stable local port, behind which
/// its tunnel runs `craze bridge --hub` over ssh per accepted connection.
/// There is no process here, so no exit and no stderr: a failure before
/// `hello` is `Unreachable`, and the phone's Dart classifier, which owns the
/// exec, says more (§3.3.2).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TcpDial(pub u16);

impl CrazeDial for TcpDial {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>> {
        let port = self.0;
        Box::pin(async move {
            let stream = tokio::net::TcpStream::connect(("127.0.0.1", port))
                .await
                .map_err(|e| {
                    DialError::Unreachable(format!(
                        "could not connect to craze's tunnel at 127.0.0.1:{port}: {e}"
                    ))
                })?;
            let (r, w) = stream.into_split();
            Ok(CrazeStream::new(r, w))
        })
    }
}

// ---- the find-only probe ----

/// What `craze providers --hub --json` said (the find-only probe, which never
/// starts a hub).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Probe {
    /// A hub is running, and answered.
    Hub,
    /// craze is there, and no hub is running: dormant.
    NoHub,
    /// craze cannot be asked: not installed, too old, or the probe failed.
    Offline {
        cause: SourceOffline,
        reason: String,
    },
}

/// Sort a probe's ending (Amendment A2: by stderr text, never by exit code
/// alone — "no hub is running" and v0.0.1's refusal both exit 1). Exit 127 is
/// the ladder's own not-found; a JSON object on stdout with exit 0 is a hub's
/// answer.
pub fn classify_probe(code: Option<i32>, stdout: &str, stderr: &str) -> Probe {
    let tail = stderr.trim();
    if code == Some(127) {
        return Probe::Offline {
            cause: SourceOffline::NotInstalled,
            reason: if tail.is_empty() {
                "exit 127".to_string()
            } else {
                tail.to_string()
            },
        };
    }
    if tail.contains(UNKNOWN_FLAG_HUB) {
        return Probe::Offline {
            cause: SourceOffline::TooOld,
            reason: too_old_message(tail),
        };
    }
    if tail.contains(NO_HUB_RUNNING) {
        return Probe::NoHub;
    }
    let answered = stdout
        .lines()
        .rev()
        .find(|l| !l.trim().is_empty())
        .is_some_and(|l| {
            matches!(
                serde_json::from_str::<serde_json::Value>(l),
                Ok(serde_json::Value::Object(_))
            )
        });
    if code == Some(0) && answered {
        return Probe::Hub;
    }
    Probe::Offline {
        cause: SourceOffline::Unreachable,
        reason: format!(
            "craze providers --hub {}{}",
            match code {
                Some(c) => format!("exited {c}"),
                None => "ended without an exit code".to_string(),
            },
            if tail.is_empty() {
                String::new()
            } else {
                format!(": {tail}")
            }
        ),
    }
}

/// How long a probe may run: a hub's `createOptions` reads its config and looks
/// binaries up, no network.
pub const PROBE_DEADLINE: Duration = Duration::from_secs(30);
/// How much of a probe's stdout is read: a `createOptions` answer is at most a
/// few KiB.
const PROBE_STDOUT_MAX: u64 = 1 << 20;

/// Run the probe locally — `argv` is `shed_core::craze::providers_hub_argv()`
/// (or its jailed twin), its `sh` run as [`SHELL`] — and classify it.
pub async fn probe_process(argv: Vec<String>, env: EnvPolicy) -> Probe {
    match run_probe(argv, env).await {
        Ok((code, stdout, stderr)) => classify_probe(code, &stdout, &stderr),
        Err(reason) => Probe::Offline {
            cause: SourceOffline::Unreachable,
            reason,
        },
    }
}

/// Run the probe to its end: its exit code, stdout and stderr — or why it
/// could not be run or did not end in time (all `Unreachable`).
async fn run_probe(
    argv: Vec<String>,
    env: EnvPolicy,
) -> Result<(Option<i32>, String, String), String> {
    let mut cmd = ProcessDial::new(argv, env)
        .command()
        .map_err(|e| e.to_string())?;
    cmd.stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true);
    let mut child = cmd
        .spawn()
        .map_err(|e| format!("could not start {SHELL} to probe craze: {e}"))?;
    let (Some(stdout), Some(stderr)) = (child.stdout.take(), child.stderr.take()) else {
        return Err("the craze probe has no stdio".to_string());
    };
    let tail = StderrTail::spawn(stderr);
    let run = async {
        let mut out = Vec::new();
        let _ = stdout.take(PROBE_STDOUT_MAX).read_to_end(&mut out).await;
        let status = child.wait().await;
        tail.wait_eof(EXIT_GRACE).await;
        (out, status.ok().and_then(|s| s.code()))
    };
    let (out, code) = tokio::time::timeout(PROBE_DEADLINE, run)
        .await
        .map_err(|_| format!("craze providers --hub did not answer within {PROBE_DEADLINE:?}"))?;
    Ok((
        code,
        String::from_utf8_lossy(&out).into_owned(),
        tail.text(),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exit_127_is_not_installed_whatever_was_said() {
        assert!(matches!(
            classify_pre_hello(Some(127), "craze: command not found", None, "closed"),
            DialError::NotInstalled(m) if m.contains("command not found")
        ));
        // A different `sh`'s wording changes nothing: the code decides.
        assert!(matches!(
            classify_pre_hello(Some(127), "sh: 1: something else", None, "closed"),
            DialError::NotInstalled(_)
        ));
    }

    #[test]
    fn unknown_flag_hub_is_too_old_and_not_its_exit_code() {
        let e = classify_pre_hello(
            Some(1),
            "Error: unknown flag: --hub\nUsage: …",
            None,
            "closed",
        );
        assert!(matches!(&e, DialError::TooOld(_)), "{e:?}");
        assert_eq!(e.cause(), SourceOffline::TooOld);
        // 127 still wins over the text: the ladder never ran craze at all.
        assert!(matches!(
            classify_pre_hello(Some(127), "unknown flag: --hub", None, "closed"),
            DialError::NotInstalled(_)
        ));
    }

    #[test]
    fn anything_else_before_hello_is_unreachable_with_the_tail_and_the_first_stdout_line() {
        let e = classify_pre_hello(
            Some(1),
            "craze bridge: no hub: the runtime dir is group-writable",
            Some("Welcome to the machine"),
            "the connection to craze closed",
        );
        let DialError::Unreachable(m) = &e else {
            panic!("{e:?}")
        };
        assert!(
            m.contains("group-writable") && m.contains("Welcome to the machine"),
            "{m}"
        );
        assert!(matches!(
            classify_pre_hello(None, "", None, "no answer"),
            DialError::Unreachable(_)
        ));
    }

    /// craze 0.1.x's line for a flag `craze bridge` does not have (v0.1.1).
    const OLD_BRIDGE_FLAG: &str =
        "craze bridge: unknown or malformed flag; see craze bridge --help";
    /// craze 0.2.0's `bridge --hub --no-start` with no hub running.
    const NO_HUB_LINE: &str = "craze bridge: no hub is running";

    /// **The pre-`hello` precedence, row by row** (plan 032 §3.5 / PC12, the
    /// module doc's order): 127 first, then v0.0.1's `--hub`, then 0.1.x's
    /// unknown flag ONLY with exit 1, then exit 3 ONLY with craze's words or
    /// none, then everything else `Unreachable`. Each row names the rule that
    /// must win, so an earlier rule's precedence is pinned against every later
    /// one's text.
    #[test]
    fn the_pre_hello_classification_keeps_its_order() {
        type Row = (Option<i32>, &'static str, &'static str);
        let rows: &[Row] = &[
            // 1. exit 127 wins over every text.
            (Some(127), "craze: command not found", "NotInstalled"),
            (Some(127), OLD_BRIDGE_FLAG, "NotInstalled"),
            (Some(127), NO_HUB_LINE, "NotInstalled"),
            (Some(127), "Error: unknown flag: --hub", "NotInstalled"),
            // 2. v0.0.1's `--hub` wins over 0.1.x's line and over exit 3.
            (Some(1), "Error: unknown flag: --hub", "TooOld"),
            (
                Some(1),
                "Error: unknown flag: --hub
craze bridge: unknown or malformed flag",
                "TooOld",
            ),
            (Some(3), "Error: unknown flag: --hub", "TooOld"),
            // 3. 0.1.x's unknown flag: exit 1 AND its words.
            (Some(1), OLD_BRIDGE_FLAG, "NoStartUnsupported"),
            (
                Some(1),
                "craze bridge: unknown or malformed flag --no-start; see craze bridge --help",
                "NoStartUnsupported",
            ),
            (Some(2), OLD_BRIDGE_FLAG, "Unreachable"),
            (Some(3), OLD_BRIDGE_FLAG, "Unreachable"),
            (None, OLD_BRIDGE_FLAG, "Unreachable"),
            // An unrelated exit 1 is never a fallback.
            (
                Some(1),
                "craze bridge: no hub: the runtime dir is group-writable",
                "Unreachable",
            ),
            (
                Some(1),
                "craze bridge: the hub is unreachable: connection refused",
                "Unreachable",
            ),
            (Some(1), "", "Unreachable"),
            // 4. exit 3 AND craze's words, or no words at all.
            (Some(3), NO_HUB_LINE, "NoHub"),
            (Some(3), "", "NoHub"),
            (Some(1), NO_HUB_LINE, "Unreachable"),
            (None, NO_HUB_LINE, "Unreachable"),
            (
                Some(3),
                "bash: line 1: something else entirely",
                "Unreachable",
            ),
            // 5. everything else.
            (None, "", "Unreachable"),
            (Some(0), "", "Unreachable"),
        ];
        for (code, stderr, want) in rows {
            let e = classify_pre_hello(*code, stderr, None, "the connection closed");
            let got = format!("{e:?}");
            assert!(
                got.starts_with(&format!("{want}(")),
                "exit {code:?}, stderr {stderr:?}: want {want}, got {got}"
            );
        }
        // The kept words: craze's line, or the exit code when it said nothing.
        assert_eq!(
            classify_pre_hello(Some(3), "", None, "x"),
            DialError::NoHub("exit 3".to_string())
        );
        assert_eq!(
            classify_pre_hello(Some(3), &format!("  {NO_HUB_LINE}\n"), None, "x"),
            DialError::NoHub(NO_HUB_LINE.to_string())
        );
        assert_eq!(
            classify_pre_hello(Some(1), OLD_BRIDGE_FLAG, None, "x"),
            DialError::NoStartUnsupported(OLD_BRIDGE_FLAG.to_string())
        );
    }

    #[test]
    fn a_probe_is_classified_by_its_stderr_text() {
        assert_eq!(
            classify_probe(Some(1), "", "craze providers: no hub is running\n"),
            Probe::NoHub
        );
        assert!(matches!(
            classify_probe(Some(1), "", "Error: unknown flag: --hub"),
            Probe::Offline {
                cause: SourceOffline::TooOld,
                ..
            }
        ));
        assert!(matches!(
            classify_probe(Some(127), "", "craze: command not found"),
            Probe::Offline {
                cause: SourceOffline::NotInstalled,
                ..
            }
        ));
        assert_eq!(
            classify_probe(Some(0), "{\"providers\":[],\"recentDirs\":[]}\n", ""),
            Probe::Hub
        );
        assert!(matches!(
            classify_probe(Some(1), "", "craze providers: hub: busy"),
            Probe::Offline {
                cause: SourceOffline::Unreachable,
                ..
            }
        ));
        assert!(matches!(
            classify_probe(Some(0), "not json\n", ""),
            Probe::Offline {
                cause: SourceOffline::Unreachable,
                ..
            }
        ));
    }

    #[test]
    fn a_dial_error_maps_to_the_contract() {
        assert!(matches!(
            DialError::NotInstalled("x".into()).into_lane_error(),
            LaneError::Unavailable(_)
        ));
        assert!(matches!(
            DialError::Unreachable("x".into()).into_lane_error(),
            LaneError::Unavailable(_)
        ));
        assert!(matches!(
            DialError::TooOld("x".into()).into_lane_error(),
            LaneError::Failed(_)
        ));
        assert!(matches!(
            DialError::Failed("x".into()).into_lane_error(),
            LaneError::Failed(_)
        ));
        // Plan 032 §3.5: no hub is its own cause, quiet, in its own words; an
        // unconsumed no-start refusal reads as `Unreachable` does.
        let no_hub = DialError::NoHub(NO_HUB_LINE.into());
        assert_eq!(no_hub.cause(), SourceOffline::NoHub);
        assert_eq!(
            no_hub.into_lane_error(),
            LaneError::Unavailable("no craze hub is running".to_string())
        );
        let unsupported = DialError::NoStartUnsupported(OLD_BRIDGE_FLAG.into());
        assert_eq!(unsupported.cause(), SourceOffline::Unreachable);
        assert!(matches!(
            unsupported.into_lane_error(),
            LaneError::Unavailable(m) if m.contains("older than 0.2.0") && m.contains(OLD_BRIDGE_FLAG)
        ));
    }

    /// A child the dial spawned dies with its last watch.
    #[tokio::test]
    async fn the_child_is_killed_with_its_last_watch() {
        let dial = ProcessDial::new(
            vec!["sh".into(), "-c".into(), "exec sleep 30".into()],
            EnvPolicy::Exactly(vec![("PATH".into(), "/usr/bin:/bin".into())]),
        );
        let stream = dial.dial().await.expect("dial");
        let exit = stream.exit().expect("a process dial has an exit").clone();
        drop(stream);
        // The stream's watch is gone, and this clone still holds the switch:
        // the child runs on.
        assert_eq!(exit.wait(Duration::from_millis(200)).await, None);
        exit.kill();
        let ended = exit.wait(Duration::from_secs(5)).await;
        assert!(ended.is_some(), "the child was killed and reaped");
        assert_eq!(ended.unwrap().code, None, "by a signal");
    }

    /// The local dial runs `/bin/sh` by absolute path: a PATH with no `sh` on
    /// it still dials.
    #[tokio::test]
    async fn a_process_dial_needs_no_sh_on_path() {
        let dial = ProcessDial::new(
            vec![
                "sh".into(),
                "-c".into(),
                "echo out; echo err >&2; exit 3".into(),
            ],
            EnvPolicy::Exactly(vec![("PATH".into(), "/nonexistent".into())]),
        );
        let stream = dial.dial().await.expect("dial");
        let (mut r, _w, tail, exit) = stream.into_parts();
        let mut out = String::new();
        r.read_to_string(&mut out).await.unwrap();
        assert_eq!(out, "out\n");
        assert_eq!(
            exit.unwrap().wait(Duration::from_secs(5)).await,
            Some(Exit { code: Some(3) })
        );
        let tail = tail.unwrap();
        assert!(tail.wait_eof(Duration::from_secs(5)).await);
        assert_eq!(tail.text(), "err");
    }

    /// `connect_hub` over a local child running `script` under a bare `PATH`.
    async fn dial_script(script: &str) -> DialError {
        let dial = ProcessDial::new(
            vec!["sh".into(), "-c".into(), script.into()],
            EnvPolicy::Exactly(vec![("PATH".into(), "/usr/bin:/bin".into())]),
        );
        let client = crate::wire::ClientInfo::shed("t");
        match connect_hub(&dial, &client, DIAL_DEADLINE, Duration::from_secs(30)).await {
            Ok(_) => panic!("{script}: no hub here"),
            Err(e) => e,
        }
    }

    /// stdout ends at once, the exit (127) comes three seconds later: the
    /// classification waits for it rather than reading a missing exit code as
    /// `Unreachable`.
    #[tokio::test]
    async fn a_late_exit_127_is_still_not_installed() {
        let e =
            dial_script("exec 1>&-; sleep 3; echo 'craze: command not found' >&2; exit 127").await;
        assert!(
            matches!(&e, DialError::NotInstalled(m) if m.contains("command not found")),
            "{e:?}"
        );
    }

    /// The process exits at once, but a child of it still holds stderr and
    /// says `unknown flag: --hub` two and a half seconds later: the
    /// classification waits for stderr's END, not for a fixed beat after the
    /// exit.
    #[tokio::test]
    async fn a_late_unknown_flag_hub_is_still_too_old() {
        let e =
            dial_script("exec 1>&-; (sleep 2.5; echo 'Error: unknown flag: --hub' >&2) & exit 1")
                .await;
        assert!(matches!(&e, DialError::TooOld(_)), "{e:?}");
    }

    /// A dial whose n-th connection runs `scripts[n]` (the last one again
    /// after that), and which records each [`CrazeDial::fall_back`] it is
    /// asked — answering `takes`.
    struct SeqDial {
        scripts: Vec<&'static str>,
        takes: bool,
        dials: std::sync::atomic::AtomicUsize,
        asked: Mutex<Vec<DialError>>,
    }

    impl SeqDial {
        fn new(takes: bool, scripts: &[&'static str]) -> SeqDial {
            SeqDial {
                scripts: scripts.to_vec(),
                takes,
                dials: std::sync::atomic::AtomicUsize::new(0),
                asked: Mutex::new(Vec::new()),
            }
        }

        fn dials(&self) -> usize {
            self.dials.load(std::sync::atomic::Ordering::SeqCst)
        }

        fn asked(&self) -> Vec<DialError> {
            lock(&self.asked).clone()
        }
    }

    impl CrazeDial for SeqDial {
        fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>> {
            let n = self.dials.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            let script = self.scripts[n.min(self.scripts.len() - 1)];
            ProcessDial::new(
                vec!["sh".into(), "-c".into(), script.into()],
                EnvPolicy::Exactly(vec![("PATH".into(), "/usr/bin:/bin".into())]),
            )
            .dial()
        }

        fn fall_back(&self, err: &DialError) -> bool {
            lock(&self.asked).push(err.clone());
            self.takes
        }
    }

    /// 0.1.x's refusal of `--no-start`, as a script.
    const REFUSES_NO_START: &str =
        "echo 'craze bridge: unknown or malformed flag; see craze bridge --help' >&2; exit 1";
    /// The older way's dial, answering something recognisable.
    const LEGACY_ANSWERS: &str = "echo 'craze bridge: no hub: the legacy dial ran' >&2; exit 1";

    async fn connect(dial: &SeqDial) -> DialError {
        let client = crate::wire::ClientInfo::shed("t");
        match connect_hub(dial, &client, DIAL_DEADLINE, Duration::from_secs(30)).await {
            Ok(_) => panic!("no hub here"),
            Err(e) => e,
        }
    }

    /// **A no-start refusal is dialled past ONCE** (plan 032 PC12): the dial
    /// is asked, takes it, and the ONE redial's answer is the answer — here
    /// the legacy dial's own failure. A dial that takes it again is not
    /// redialled a third time: the second refusal stands.
    #[tokio::test]
    async fn a_no_start_refusal_the_dial_takes_is_redialled_once() {
        let dial = SeqDial::new(true, &[REFUSES_NO_START, LEGACY_ANSWERS]);
        let e = connect(&dial).await;
        assert!(
            matches!(&e, DialError::Unreachable(m) if m.contains("the legacy dial ran")),
            "{e:?}"
        );
        assert_eq!(dial.dials(), 2);
        let asked = dial.asked();
        assert_eq!(asked.len(), 1, "{asked:?}");
        assert!(
            matches!(&asked[0], DialError::NoStartUnsupported(_)),
            "{asked:?}"
        );

        // Refused again after the redial: no third dial.
        let dial = SeqDial::new(true, &[REFUSES_NO_START]);
        let e = connect(&dial).await;
        assert!(matches!(e, DialError::NoStartUnsupported(_)), "{e:?}");
        assert_eq!(dial.dials(), 2, "one redial, never a loop");
        assert_eq!(dial.asked().len(), 1);
    }

    /// **A dial that cannot fall back keeps the refusal** (the trait's
    /// default — every dial here), and it reads as `Unreachable`; and **an
    /// unrelated exit 1 never asks** — not even a dial that would take it.
    #[tokio::test]
    async fn an_unconsumed_refusal_stands_and_an_unrelated_exit_1_never_asks() {
        let declines = SeqDial::new(false, &[REFUSES_NO_START, LEGACY_ANSWERS]);
        let e = connect(&declines).await;
        assert!(matches!(e, DialError::NoStartUnsupported(_)), "{e:?}");
        assert_eq!(e.cause(), SourceOffline::Unreachable);
        assert_eq!(declines.dials(), 1, "declined: no redial");
        assert_eq!(declines.asked().len(), 1);

        let default = ProcessDial::new(
            vec!["sh".into(), "-c".into(), REFUSES_NO_START.into()],
            EnvPolicy::Exactly(vec![("PATH".into(), "/usr/bin:/bin".into())]),
        );
        assert!(!default.fall_back(&e), "the trait's default takes nothing");

        let takes = SeqDial::new(true, &[LEGACY_ANSWERS]);
        let e = connect(&takes).await;
        assert!(matches!(e, DialError::Unreachable(_)), "{e:?}");
        assert_eq!(takes.dials(), 1);
        assert!(
            takes.asked().is_empty(),
            "only a no-start refusal is offered"
        );
    }

    /// **No hub, through a real child** (plan 032 §3.5): `--no-start`'s exit
    /// 3 and its line come back as [`DialError::NoHub`] — offered to no
    /// fallback, and quiet in the contract's words.
    #[tokio::test]
    async fn an_exit_3_with_crazes_line_is_no_hub() {
        let dial = SeqDial::new(
            true,
            &["echo 'craze bridge: no hub is running' >&2; exit 3"],
        );
        let e = connect(&dial).await;
        assert_eq!(e, DialError::NoHub(NO_HUB_LINE.to_string()));
        assert!(dial.asked().is_empty());
        assert_eq!(dial.dials(), 1);
    }

    /// **An exit 3 with stderr still open is not no hub** (C6 review): the
    /// child exits 3 at once, but something behind it holds stderr open and
    /// writes after the grace. The empty-stderr arm needs a stderr that ended,
    /// so this is `Unreachable` (never Dormant), not [`DialError::NoHub`].
    #[tokio::test]
    async fn an_exit_3_with_stderr_still_open_is_not_no_hub() {
        let dial = SeqDial::new(
            true,
            &["exec 1>&-; (sleep 7; echo 'ssh wrapper: unrelated failure' >&2) & exit 3"],
        );
        let e = connect(&dial).await;
        assert!(matches!(e, DialError::Unreachable(_)), "{e:?}");
        assert!(dial.asked().is_empty());
    }

    #[tokio::test]
    async fn a_tcp_dial_to_nothing_is_unreachable() {
        // Port 1 (tcpmux) listens nowhere we run. (A just-released ephemeral
        // port is no good: Linux may hand it back as the connect's own source
        // port and the socket connects to itself.)
        let err = TcpDial(1).dial().await.err().expect("refused");
        assert!(matches!(err, DialError::Unreachable(_)), "{err:?}");
    }
}
