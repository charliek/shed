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
//! (from C8) every open lane — dials its own. A dial is never reused: craze's
//! hub answers one request at a time in order, and a splice takes the whole
//! connection.
//!
//! # Classification (feeds `SourceOffline` and the lanes' errors)
//!
//! [`connect_hub`] dials, writes the hub `hello` at once (a hub closes a
//! connection that says nothing within 5 s, craze `internal/hub/server.go:52`
//! — craze#89 item 5, undocumented in protocol.md), and sorts every way that
//! can fail, in this precedence:
//!
//! 1. the process exited **127** — the ladder's own `craze: command not found`
//!    — → [`DialError::NotInstalled`] (the exit code, so the wording of
//!    whichever `sh` ran it does not matter);
//! 2. it exited before answering `hello` with cobra's **`unknown flag:
//!    --hub`** on stderr (v0.0.1, for `bridge --hub` and for the find-only
//!    `providers --hub` alike — plan 025 Amendment A2), or a hub `hello`
//!    without `rosterSubscribe`/`connect`, an unknown codec, or a
//!    `bad_request`/`protocol_version` refusal → [`DialError::TooOld`];
//! 3. any other failure before `hello` → [`DialError::Unreachable`], whose
//!    reason is the stderr tail plus the first non-JSON stdout line, if any;
//! 4. any other refusal of `hello` → [`DialError::Failed`].
//!
//! A dial without a process ([`TcpDial`]) sees neither an exit code nor
//! stderr: everything before `hello` is `Unreachable` there, and on the phone
//! the Dart side's own classifier is authoritative for it (§3.3.2).
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
use crate::wire::ClientInfo;

/// A boxed read half.
pub type BoxRead = Box<dyn AsyncRead + Send + Unpin>;
/// A boxed write half.
pub type BoxWrite = Box<dyn AsyncWrite + Send + Unpin>;

/// A fresh duplex to `craze bridge --hub`, one per connection, never reused.
pub trait CrazeDial: Send + Sync {
    fn dial(&self) -> BoxFuture<'static, Result<CrazeStream, DialError>>;
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
}

impl DialError {
    /// The source-level cause this is.
    pub fn cause(&self) -> SourceOffline {
        match self {
            DialError::NotInstalled(_) => SourceOffline::NotInstalled,
            DialError::TooOld(_) => SourceOffline::TooOld,
            DialError::Unreachable(_) => SourceOffline::Unreachable,
            DialError::Failed(_) => SourceOffline::Failed,
        }
    }

    /// The contract error for a call whose dial failed. `NotInstalled` and
    /// `Unreachable` are quiet (`Unavailable`: nothing to talk to); `TooOld`
    /// and a refusal are `Failed` with craze's words.
    pub fn into_lane_error(self) -> LaneError {
        match self {
            e @ (DialError::NotInstalled(_) | DialError::Unreachable(_)) => {
                LaneError::Unavailable(e.to_string())
            }
            DialError::TooOld(m) | DialError::Failed(m) => LaneError::Failed(m),
        }
    }
}

/// How long [`probe_process`] waits, after the probe exited, for the rest of
/// its stderr.
const EXIT_GRACE: Duration = Duration::from_secs(2);

/// Dial, say `hello` to the hub, and judge it — or classify why not.
pub async fn connect_hub(
    dial: &dyn CrazeDial,
    client: &ClientInfo,
    hello_deadline: Duration,
) -> Result<(Conn, Notifications, HubHello), DialError> {
    let stream = dial.dial().await?;
    let (conn, notes) = Conn::start(stream);
    match hello_hub(&conn, client, hello_deadline).await {
        Ok(hello) => Ok((conn, notes, hello)),
        Err(HelloError::TooOld(m)) => Err(DialError::TooOld(m)),
        Err(HelloError::Refused(m)) => Err(DialError::Failed(m)),
        Err(HelloError::NoAnswer(why)) => Err(classify_no_answer(&conn, &why).await),
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
        if let Some(tail) = conn.stderr() {
            tail.wait_eof(PRE_HELLO_GRACE).await;
        }
    };
    tokio::join!(exited, drained);
    let code = conn.exit().and_then(ExitWatch::now).and_then(|e| e.code);
    let stderr = conn.stderr().map(StderrTail::text).unwrap_or_default();
    let preamble = conn.preamble();
    classify_pre_hello(code, &stderr, preamble.first().map(String::as_str), why)
}

/// The pre-`hello` classification, pure (plan 025 §3.3.2 + Amendment A2). `code`
/// is the process's exit code when it exited with one; `stdout_line` the first
/// non-JSON line it printed; `why` what the connection itself said.
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
/// What `craze providers --hub` says when it finds no hub to ask.
pub const NO_HUB_RUNNING: &str = "no hub is running";

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
        }
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
        match connect_hub(&dial, &client, Duration::from_secs(30)).await {
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

    #[tokio::test]
    async fn a_tcp_dial_to_nothing_is_unreachable() {
        // Port 1 (tcpmux) listens nowhere we run. (A just-released ephemeral
        // port is no good: Linux may hand it back as the connect's own source
        // port and the socket connects to itself.)
        let err = TcpDial(1).dial().await.err().expect("refused");
        assert!(matches!(err, DialError::Unreachable(_)), "{err:?}");
    }
}
