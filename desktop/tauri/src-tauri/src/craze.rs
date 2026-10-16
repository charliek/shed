//! **A craze source per machine** (plan 025 §3.6.1–§3.6.2, P15) — the desktop
//! half of [`shed_craze::CrazeSource`]: how this app reaches each host's craze
//! hub, the state it folds the hub's roster into, and the loop that keeps it
//! current.
//!
//! craze is the provider abstraction (D2): one per-machine hub lists every
//! cursor, grok, gx and native session there. [`crate::roost_hosts::RoostHosts`]
//! holds one [`CrazeState`] per host, under the same lock as that host's roost
//! rows, and runs one [`run`] task per host that feeds it. This module is that
//! task, the state, and the two dials.
//!
//! # Eager here, attach-only everywhere else
//!
//! * **This machine is EAGER** ([`CrazeReach::probe`] is `None`): a long-lived
//!   roster connection from boot, through a local `/bin/sh -c '<ladder>'`
//!   ([`shed_craze::ProcessDial`]). Its hub is born in the app's own session —
//!   the GUI session on a Mac, so cursor works there.
//! * **A remote machine or a shed is ATTACH-ONLY** ([`CrazeReach::probe`] is
//!   `Some`): an eager `bridge --hub` would birth a hub on every configured
//!   machine and hold it alive, and on a Mac an ssh-born hub keeps cursor
//!   `unavailable` for everyone in that namespace (craze SF-126). So a remote
//!   source cycles (the state machine, as implemented):
//!
//!   | phase | how it got here | what runs | leaves on |
//!   |---|---|---|---|
//!   | (absent) | started; nothing answered yet | the find-only probe | its first answer |
//!   | **Dormant** | the probe said `no hub is running` — or the roster's own `--no-start` dial found none (`Offline{NoHub}`, plan 032 §3.5) | the probe again in [`DORMANT_PROBE`] | a probe that finds a hub |
//!   | **Live** | the probe found a hub, then the roster seeded | ONE `bridge --hub --no-start` roster connection (`bridge --hub` behind a probe for a craze older than 0.2.0) | the roster's `Offline` (EOF, `hub_closing`, an error) |
//!   | **Offline** | the roster dropped, or the probe could not ask (unreachable; not installed and too old are offline causes too) | the probe again: after about a second once a live cycle ends (jittered [`PROBE_BACKOFF_BASE`]), on a backoff while unreachable, every [`DORMANT_PROBE`] while not installed or too old | the probe's next answer |
//!
//!   After a live cycle ends the probe runs again before anything redials: a
//!   hub that is gone sends the source back to Dormant (its rows dropped and
//!   its lanes evicted — the hub is gone, so nothing is listed), and a hub that
//!   is still there is re-joined. The probe stays even where every dial is
//!   find-only: it is what tells not installed, too old and dormant apart for
//!   the status line, and what a dormant source waits on. A no-hub answer
//!   from a probe (or roster dial) that started before a create or a reopen
//!   listed a session is stale, and is not folded: the probe asks again at
//!   once ([`attach_only`]).
//!
//! * **No background remote dial can start a hub** ([`SshCrazeDial`], plan 032
//!   §3.5). Every remote connection but a user's explicit action — the
//!   roster's, and every lane's, background reconnects included — is craze
//!   0.2.0's find-only `bridge --hub --no-start`: it attaches to a running hub,
//!   or exits 3 (`no hub is running`) and starts nothing. A roster that draws
//!   that is the source going DORMANT ([`attach_only`]); a lane keeps `Stale`,
//!   within its own 10-minute bound. There is no probe per dial: the dial
//!   itself cannot start a hub, so there is nothing to gate, and no caller can
//!   bypass the rule because it is in the dial.
//! * **A craze older than 0.2.0 falls back, once per source** ([`DialMode`]):
//!   it refuses the flag (`craze bridge: unknown or malformed flag`, exit 1),
//!   and the dial — ONE per host, shared by the roster and every lane — swaps
//!   for good to the older way: the find-only probe on the same ControlMaster
//!   first, then `bridge --hub`, `Unreachable` when no hub is running. On that
//!   legacy path **one race remains, ACCEPTED** (plan 025 §3.6.1): the probe
//!   sees a hub that exits before the `bridge --hub` behind it joins, and the
//!   bridge — which always Ensures a hub — births one, ssh-born, with no user
//!   action. The pair cannot be made atomic from this side; craze 0.2.0's
//!   `--no-start` is what closes it. A new source (a host registered again, a
//!   restart) builds a new dial, back on `--no-start`.
//! * **A hub is born remotely only by an explicit user action** (plan 025
//!   §3.6.1, C10): opening the create sheet (its `create_options`) and the
//!   create itself run through the host's UNGATED dial,
//!   [`CrazeReach::ensure`] — `bridge --hub` with no probe in front, which
//!   Ensures a hub — on a clone of the source that shares its rows
//!   ([`shed_craze::CrazeSource::dialling`]), so the session it creates is
//!   openable through the roster's source at once. The task is then woken
//!   ([`run`]'s `wake`) so a dormant source probes again now, finds the hub
//!   the user just started, and attaches to it, rather than at its next
//!   [`DORMANT_PROBE`]. Those two are the ONLY things that birth a remote hub
//!   (plan 025 Amendment A14): Open in terminal runs `craze attach` in a roost
//!   tab, which joins the session over the session's own control socket and
//!   starts nothing — no hub (craze's `cli.md`, "craze attach": "Nothing is
//!   spawned, nothing binds a socket").
//!
//! # The state, staged
//!
//! [`CrazeState`] folds the source's events with the contract's stage-and-swap
//! rule: a `Reset` opens a staging set, `Session`/`Removed`/`Capabilities`
//! land in it, and its own `Ready` swaps it in. **Retained rows render
//! stale**: an `Offline` keeps the last swapped-in rows (a `Dormant` drops
//! them), and every row a source holds while it is not `Live` is emitted
//! `stale` and `approximate`, the way an unreachable host's roost rows are.
//!
//! # Test mode
//!
//! `SHED_TAURI_CRAZE_PATH` (an exec seam: test mode AND a debug build) names a
//! directory; the local dial then runs the **jailed** ladder (rungs 1–2 only)
//! under `env_clear()` + `HOME`, `CRAZE_HOME`, `CRAZE_RUNTIME_DIR` from the
//! app's env + `PATH=<that dir>`. Unset in test mode, this machine has no craze
//! source at all. A remote craze source in test mode exists only through the
//! fake-ssh seam (`SHED_TAURI_SSH_BIN`), and it too composes the jailed
//! ladder — so neither path can reach a craze installed on the host running
//! the tests (`crate::machines::build_local_craze`/`build_ssh_craze`).

use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};

use shed_app::roost::SshExec;
use shed_app::spawn_gate::{ChildExit, SpawnGate, SpawnSection};
use shed_core::lane::backoff::{jittered, next_backoff};
use shed_core::lane::{LaneSession, SourceCapabilities, SourceEvent, SourceOffline};
use shed_core::roost::bootstrap::{Outcome, Stdin};
use shed_craze::{
    classify_probe, CrazeDial, CrazeSource, CrazeStream, DialError, ExitWatch, Probe, StderrTail,
};

/// A boxed, `'static` future — the shape [`CrazeDial::dial`] already answers
/// with, spelled out rather than taken from `futures_util`.
pub type BoxFut<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

/// How long a dormant remote source waits between find-only probes — and the
/// slow retry while craze there is not installed or too old.
pub const DORMANT_PROBE: Duration = Duration::from_secs(30);
/// The probe's backoff floor while the host is unreachable, and the pause
/// after a live roster cycle ends before the probe asks again.
pub const PROBE_BACKOFF_BASE: Duration = Duration::from_secs(1);
/// The probe's backoff ceiling while the host is unreachable.
pub const PROBE_BACKOFF_MAX: Duration = Duration::from_secs(30);
/// How much of the probe's stdout is kept: `createOptions` is a few KiB.
const PROBE_STDOUT_CAP: usize = 1 << 20;

/// A remote host's find-only probe (`craze providers --hub --json`, which
/// never starts a hub).
pub trait CrazeProbe: Send + Sync {
    fn probe(&self) -> BoxFut<Probe>;
}

/// How one host's craze is reached: the dial every connection goes through,
/// and — for an attach-only host — the probe that decides whether there is a
/// hub to attach to at all.
#[derive(Clone)]
pub struct CrazeReach {
    pub dial: Arc<dyn CrazeDial>,
    /// `None`: EAGER (this machine). `Some`: ATTACH-ONLY (a remote machine or
    /// a shed) — and its dial reaches only a running hub too (`bridge --hub
    /// --no-start`, or probe-gated for a craze older than 0.2.0:
    /// [`SshCrazeDial`]).
    pub probe: Option<Arc<dyn CrazeProbe>>,
    /// The dial a user's EXPLICIT action runs through — the create sheet's
    /// `create_options` and the create (the module doc): an attach-only host's
    /// ungated `bridge --hub`, which may birth a hub there. `None`: the same as
    /// [`Self::dial`], which for an eager host is already ungated.
    pub ensure: Option<Arc<dyn CrazeDial>>,
}

/// The source's clocks — the defaults are the pinned ones; a test shortens
/// them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CrazeTimings {
    pub dormant: Duration,
    pub backoff_base: Duration,
    pub backoff_max: Duration,
}

impl Default for CrazeTimings {
    fn default() -> Self {
        Self {
            dormant: DORMANT_PROBE,
            backoff_base: PROBE_BACKOFF_BASE,
            backoff_max: PROBE_BACKOFF_MAX,
        }
    }
}

// ---------------------------------------------------------------------------
// the ssh probe and the attach-only ssh dial
// ---------------------------------------------------------------------------

/// The find-only probe over a host's `SshExec` — a one-shot `run` on the
/// shared ControlMaster, classified by its stderr TEXT (plan 025 Amendment A2:
/// `no hub is running` and v0.0.1's `unknown flag: --hub` both exit 1).
pub struct SshCrazeProbe {
    exec: Arc<SshExec>,
    /// `display_line` of the providers form — the production ladder, or the
    /// jailed one in test mode.
    command: String,
}

impl SshCrazeProbe {
    pub fn new(exec: Arc<SshExec>, command: String) -> Self {
        Self { exec, command }
    }
}

impl CrazeProbe for SshCrazeProbe {
    fn probe(&self) -> BoxFut<Probe> {
        let exec = Arc::clone(&self.exec);
        let command = self.command.clone();
        Box::pin(async move {
            let outcome = exec
                .run(
                    &command,
                    Stdin::Empty,
                    shed_craze::dial::PROBE_DEADLINE,
                    PROBE_STDOUT_CAP,
                    true,
                )
                .await;
            match outcome {
                Outcome::Exec {
                    exit: Some(code),
                    stdout,
                    stderr_tail,
                } => classify_probe(Some(code), &String::from_utf8_lossy(&stdout), &stderr_tail),
                // No status: the budget expired, or ssh itself died — this
                // host cannot be asked right now.
                Outcome::Exec { stderr_tail, .. } => Probe::Offline {
                    cause: SourceOffline::Unreachable,
                    reason: stderr_tail,
                },
                // A runner answers an exec with an exec; anything else is a
                // probe that could not be read.
                _ => Probe::Offline {
                    cause: SourceOffline::Unreachable,
                    reason: "the craze probe did not answer as an exec".to_string(),
                },
            }
        })
    }
}

/// How an attach-only [`SshCrazeDial`] reaches its hub (plan 032 §3.5) — ONE
/// mode per dial, and one dial per source, shared by its roster and every lane
/// it opens (`crate::machines::build_ssh_craze`), so the first refusal moves
/// them all.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum DialMode {
    /// craze 0.2.0+: `bridge --hub --no-start`, no probe in front — it
    /// attaches to a running hub or exits 3, and never starts one.
    NoStart = 0,
    /// Fallen back for a craze older than 0.2.0: the find-only probe, then
    /// `bridge --hub` (the module doc's accepted race).
    Legacy = 1,
}

/// A remote host's dial over [`SshExec::spawn_duplex`] (the module doc): every
/// connection — the roster's and every lane's — reaches only a hub that is
/// already running, and a host with none is never given one by a background
/// dial. In [`DialMode::NoStart`] that is craze's own `bridge --hub
/// --no-start`; in [`DialMode::Legacy`] the find-only probe gates a `bridge
/// --hub`, and a host with no hub is `Unreachable`.
///
/// The one UNGATED instance ([`Self::ungated`]) is the host's
/// [`CrazeReach::ensure`]: only a user's explicit action dials it, and it runs
/// `bridge --hub`, which may start a hub.
pub struct SshCrazeDial {
    exec: Arc<SshExec>,
    /// `display_line` of the bridge form (production, or jailed in test mode):
    /// what [`Self::ungated`] and [`DialMode::Legacy`] run.
    bridge: String,
    /// `display_line` of the `--no-start` form — what [`DialMode::NoStart`]
    /// runs. `None` only for [`Self::ungated`].
    no_start: Option<String>,
    /// The find-only probe [`DialMode::Legacy`] runs in front of the bridge.
    /// `None` only for [`Self::ungated`].
    probe: Option<Arc<dyn CrazeProbe>>,
    /// A [`DialMode`] (`as u8`): `NoStart` until a refusal of the flag moves
    /// it to `Legacy` ([`CrazeDial::fall_back`]), for good.
    mode: AtomicU8,
    /// The app's spawn gate (plan 026 §3.3). This dial takes its section
    /// around the `spawn_duplex` and the child's registration ALONE — not
    /// around the find-only probe in front of them, which can take
    /// `PROBE_DEADLINE` (30 s) and runs through the exec's own gate — so a
    /// close never waits on somebody's probe.
    gate: Option<SpawnGate>,
}

impl SshCrazeDial {
    /// An attach-only dial: `no_start` first, falling back to `probe` +
    /// `bridge` for a craze older than 0.2.0.
    pub fn new(
        exec: Arc<SshExec>,
        bridge: String,
        no_start: String,
        probe: Arc<dyn CrazeProbe>,
    ) -> Self {
        Self {
            exec,
            bridge,
            no_start: Some(no_start),
            probe: Some(probe),
            mode: AtomicU8::new(DialMode::NoStart as u8),
            gate: None,
        }
    }

    /// The bridge with NO probe in front and no `--no-start`: it Ensures a hub
    /// on the far side whether or not one runs. For [`CrazeReach::ensure`]
    /// only — the create sheet's `create_options` and the create, a user's
    /// explicit action. It never falls back: it has nothing to fall back from.
    /// ("Ungated" names the PROBE gate; the spawn gate is [`Self::with_spawn_gate`].)
    pub fn ungated(exec: Arc<SshExec>, bridge: String) -> Self {
        Self {
            exec,
            bridge,
            no_start: None,
            probe: None,
            mode: AtomicU8::new(DialMode::Legacy as u8),
            gate: None,
        }
    }

    /// Spawn only inside `gate`'s sections, registering every bridge child.
    pub fn with_spawn_gate(mut self, gate: Option<SpawnGate>) -> Self {
        self.gate = gate;
        self
    }

    /// The mode the next dial runs in.
    pub fn mode(&self) -> DialMode {
        if self.mode.load(Ordering::Acquire) == DialMode::NoStart as u8 {
            DialMode::NoStart
        } else {
            DialMode::Legacy
        }
    }
}

/// What the gated dial answers when the probe found no hub to join.
pub fn no_hub(label: &str) -> DialError {
    DialError::Unreachable(format!(
        "no craze hub is running on {label}; shed starts one there only when you ask it to"
    ))
}

impl CrazeDial for SshCrazeDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        let exec = Arc::clone(&self.exec);
        let gate = self.gate.clone();
        // A closed gate refuses before the probe, too: a clean early answer.
        if gate.as_ref().is_some_and(SpawnGate::is_closed) {
            return Box::pin(async { Err(shutting_down_dial()) });
        }
        // NoStart: the find-only bridge, with NO probe in front — it cannot
        // start a hub, so there is nothing to gate. Legacy (and the ungated
        // dial, which has no probe): the bridge, behind the probe if any.
        let (bridge, probe) = match (&self.no_start, self.mode()) {
            (Some(no_start), DialMode::NoStart) => (no_start.clone(), None),
            _ => (self.bridge.clone(), self.probe.as_ref().map(|p| p.probe())),
        };
        Box::pin(async move {
            match probe {
                None => {}
                Some(probe) => match probe.await {
                    Probe::Hub => {}
                    Probe::NoHub => return Err(no_hub(exec.label())),
                    Probe::Offline { cause, reason } => {
                        return Err(match cause {
                            SourceOffline::NotInstalled => DialError::NotInstalled(reason),
                            SourceOffline::TooOld => DialError::TooOld(reason),
                            SourceOffline::Failed => DialError::Failed(reason),
                            _ => DialError::Unreachable(reason),
                        })
                    }
                },
            }
            // **The spawn section** (plan 026 §3.3): check → spawn →
            // register, under the gate's shared guard.
            let section = match &gate {
                Some(gate) => Some(gate.enter().map_err(|_| shutting_down_dial())?),
                None => None,
            };
            let child = exec.spawn_duplex(&bridge).map_err(|e| {
                DialError::Unreachable(format!("{}: could not run ssh: {e}", exec.label()))
            })?;
            let stream = child_stream(child)?;
            match (&gate, section.as_ref()) {
                (Some(gate), Some(section)) => register_stream(gate, section, stream)
                    .await
                    .map_err(|_| shutting_down_dial()),
                _ => Ok(stream),
            }
        })
    }

    /// **The `--no-start` fallback** (plan 032 PC12): craze refused the flag,
    /// so it is older than 0.2.0 — swap this dial (the roster's and every
    /// lane's: one dial per source) to [`DialMode::Legacy`] and answer `true`,
    /// so `connect_hub` dials once more, the older way. ONCE: a dial already
    /// `Legacy` — another dial's refusal won the swap, or this is the ungated
    /// one — answers `false`, and the refusal it was offered stands (it reads
    /// as `Unreachable`; the source or lane redials on its own clock, legacy
    /// from then on).
    fn fall_back(&self, err: &DialError) -> bool {
        if !matches!(err, DialError::NoStartUnsupported(_)) || self.no_start.is_none() {
            return false;
        }
        let swapped = self
            .mode
            .compare_exchange(
                DialMode::NoStart as u8,
                DialMode::Legacy as u8,
                Ordering::AcqRel,
                Ordering::Acquire,
            )
            .is_ok();
        if swapped {
            app_info!(
                "shed-desktop-tauri: craze on {} does not know `bridge --hub --no-start` \
                 (older than 0.2.0): its dials probe before `bridge --hub` from now on ({err})",
                self.exec.label()
            );
        }
        swapped
    }
}

// ---------------------------------------------------------------------------
// the spawn gate (plan 026 §3.3)
// ---------------------------------------------------------------------------

/// What a gated dial answers once the app's spawn gate has closed.
pub fn shutting_down_dial() -> DialError {
    DialError::Unreachable(shed_app::spawn_gate::ShuttingDown.to_string())
}

/// A dial whose every spawn runs inside a section of the app's spawn gate
/// (plan 026 §3.3) — the desktop's wrapper around its local
/// [`shed_craze::ProcessDial`]. Dialers are the client's (plan 025 P8), so
/// this is where shutting down is refused, with no change to shed-craze: the
/// roster, every lane, every `create_options`, every create and every one of
/// its automatic retries dials through it.
///
/// The section is held across polling the inner dial future THROUGH its
/// spawn and the registration of the child it made: a close either waits for
/// both, so the drain finds the child, or comes first, and the dial answers
/// `shutting_down` without ever polling the inner one.
pub struct GatedDial {
    inner: Arc<dyn CrazeDial>,
    gate: SpawnGate,
    /// Test seam: a pause between the gate check and the inner dial.
    #[cfg(test)]
    pause: std::sync::Mutex<Option<crate::roost_hosts::TestGap>>,
}

/// `inner`, gated by `gate` when there is one; `inner` itself otherwise.
pub fn gated(inner: Arc<dyn CrazeDial>, gate: Option<&SpawnGate>) -> Arc<dyn CrazeDial> {
    match gate {
        Some(gate) => Arc::new(GatedDial::new(inner, gate.clone())),
        None => inner,
    }
}

impl GatedDial {
    pub fn new(inner: Arc<dyn CrazeDial>, gate: SpawnGate) -> GatedDial {
        GatedDial {
            inner,
            gate,
            #[cfg(test)]
            pause: std::sync::Mutex::new(None),
        }
    }

    /// Arm the next dial's pause between the check and the spawn (a test).
    #[cfg(test)]
    pub(crate) fn pause_next(&self, gap: crate::roost_hosts::TestGap) {
        *self.pause.lock().unwrap() = Some(gap);
    }
}

impl CrazeDial for GatedDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        let inner = Arc::clone(&self.inner);
        let gate = self.gate.clone();
        #[cfg(test)]
        let pause = self.pause.lock().unwrap().take();
        Box::pin(async move {
            let section = gate.enter().map_err(|_| shutting_down_dial())?;
            #[cfg(test)]
            if let Some((reached, proceed)) = pause {
                reached.notify_one();
                proceed.notified().await;
            }
            let stream = inner.dial().await?;
            register_stream(&gate, &section, stream)
                .await
                .map_err(|_| shutting_down_dial())
        })
    }

    /// A wrapper forwards the fallback it wraps (`CrazeDial::fall_back`).
    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// Register `stream`'s child — its [`ExitWatch`], the kill-and-await handle —
/// with `gate`, inside the spawn's `section`. **On a gate that has closed, the
/// child is killed and awaited here and the dial answers `shutting_down`**
/// (plan 026 §3.3, F1): a section admitted before the close whose spawn came
/// after it leaves nothing behind.
///
/// The registry keeps the entry until the child's exit has been SEEN, so the
/// drain awaits even a bridge whose connection was let go a moment before it
/// (that drop's kill is otherwise a detached task racing the process's exit).
/// Its clone of the watch would then keep the child alive past its holder —
/// the watch kills only once its LAST clone is gone — so the stream's write
/// half kills it explicitly when it drops ([`RegisteredWrite`]): what letting
/// the connection go always did.
pub async fn register_stream(
    gate: &SpawnGate,
    section: &SpawnSection,
    stream: CrazeStream,
) -> Result<CrazeStream, shed_app::spawn_gate::ShuttingDown> {
    let (reader, writer, tail, exit) = stream.into_parts();
    let Some(exit) = exit else {
        return Ok(CrazeStream::from_parts(reader, writer, tail, None));
    };
    gate.register_exit(section, Arc::new(ExitKill(exit.clone())))
        .await?;
    Ok(CrazeStream::from_parts(
        reader,
        Box::new(RegisteredWrite {
            inner: writer,
            exit: exit.clone(),
        }),
        tail,
        Some(exit),
    ))
}

/// A craze child's [`ExitWatch`] as the gate's drain uses it: kill, then wait
/// (bounded) for its exit to be published.
struct ExitKill(ExitWatch);

impl ChildExit for ExitKill {
    fn kill(&self) {
        self.0.kill();
    }

    fn exited(&self, within: Duration) -> BoxFut<bool> {
        let exit = self.0.clone();
        Box::pin(async move { exit.wait(within).await.is_some() })
    }

    fn done(&self) -> bool {
        self.0.now().is_some()
    }

    fn pid(&self) -> Option<u32> {
        self.0.pid()
    }
}

/// A registered stream's write half: the connection's end is its child's kill
/// ([`register_stream`]).
struct RegisteredWrite {
    inner: shed_craze::dial::BoxWrite,
    exit: ExitWatch,
}

impl Drop for RegisteredWrite {
    fn drop(&mut self) {
        self.exit.kill();
    }
}

impl tokio::io::AsyncWrite for RegisteredWrite {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(cx, buf)
    }

    fn poll_flush(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

/// A craze source's dial, wrapped so that its LAST holder letting go is
/// observable ([`tracked`]).
///
/// Every connection a source makes — its roster's pump, every lane opened
/// through it, every lane's watcher — holds the source's dial, and a remote
/// dial holds the host's [`SshExec`] (and so its ControlMaster). The
/// roost-host layer's `remove` waits on this signal — a **bounded best-effort
/// teardown**: it waits up to `CRAZE_STOP_WAIT` for the task and the dial; a
/// held lane verb can outlive it, and the last holder's drop releases it.
pub struct TrackedDial {
    inner: Arc<dyn CrazeDial>,
    /// Never sent: DROPPED with this dial, which is what the receiver hears.
    _gone: tokio::sync::oneshot::Sender<()>,
}

impl CrazeDial for TrackedDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        self.inner.dial()
    }

    /// Forwarded: the source's roster and lanes dial through THIS wrapper, so
    /// a host's `--no-start` fallback (plan 032 §3.5) is only reached here.
    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// `inner`, tracked: the dial to build a source on, and a receiver that
/// resolves once the last clone of that dial has been dropped.
pub fn tracked(
    inner: Arc<dyn CrazeDial>,
) -> (Arc<dyn CrazeDial>, tokio::sync::oneshot::Receiver<()>) {
    let (tx, rx) = tokio::sync::oneshot::channel();
    (Arc::new(TrackedDial { inner, _gone: tx }), rx)
}

/// A host's explicit-action dial ([`CrazeReach::ensure`]) TETHERED to its
/// tracked dial: it holds a clone of `tether`, so the host's teardown — which
/// waits for the tracked dial's last holder ([`tracked`]) — waits for a create
/// in flight through it too.
struct Tethered {
    inner: Arc<dyn CrazeDial>,
    _tether: Arc<dyn CrazeDial>,
}

impl CrazeDial for Tethered {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        self.inner.dial()
    }

    /// Forwarded, as every wrapper does (the ungated dial inside never
    /// sends `--no-start`, so it never takes one).
    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// `inner`, tethered to `tether` (see [`Tethered`]).
pub fn tethered(inner: Arc<dyn CrazeDial>, tether: &Arc<dyn CrazeDial>) -> Arc<dyn CrazeDial> {
    Arc::new(Tethered {
        inner,
        _tether: Arc::clone(tether),
    })
}

/// A spawned bridge child as a [`CrazeStream`]: its stdout and stdin, its
/// stderr's tail for the pre-`hello` classification, and its exit — which is
/// also its kill switch, so a stream that is dropped kills its bridge.
pub fn child_stream(mut child: tokio::process::Child) -> Result<CrazeStream, DialError> {
    let (Some(stdin), Some(stdout), Some(stderr)) =
        (child.stdin.take(), child.stdout.take(), child.stderr.take())
    else {
        return Err(DialError::Unreachable(
            "the craze bridge has no stdio".to_string(),
        ));
    };
    Ok(CrazeStream::new(stdout, stdin)
        .with_stderr_tail(StderrTail::spawn(stderr))
        .with_exit(ExitWatch::spawn(child)))
}

// ---------------------------------------------------------------------------
// the state
// ---------------------------------------------------------------------------

/// Where a host's craze source is (the module doc's table).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CrazePhase {
    /// craze is there and no hub is running (a remote host only).
    Dormant,
    /// The roster seeded and is following the hub.
    Live,
    /// The roster dropped, or craze could not be asked; see the cause.
    Offline,
}

/// A seed in progress: the rows and capabilities between a `Reset` and its
/// `Ready`.
#[derive(Debug, Default)]
struct Staged {
    generation: u64,
    rows: BTreeMap<String, LaneSession>,
    caps: Option<SourceCapabilities>,
}

/// One host's craze roster, as this app shows it (plan 025 §3.6.2).
#[derive(Debug, Default)]
pub struct CrazeState {
    /// The registration fence: the generation of the [`run`] task that may
    /// write here. `0` — this host has no craze source at all.
    pub gen: u64,
    /// The swapped-in rows, by hostId (P11) — live while `Live`, retained (and
    /// rendered stale) while `Offline`, none while `Dormant`.
    pub rows: BTreeMap<String, LaneSession>,
    /// `None` until the source first says anything.
    pub phase: Option<CrazePhase>,
    pub offline: Option<(SourceOffline, String)>,
    pub caps: Option<SourceCapabilities>,
    pub truncated: bool,
    staged: Option<Staged>,
    /// The last craze news sequence number [`Self::mint_news`] handed out —
    /// `0` before the first.
    news: u64,
    /// **The listing epoch** ([`Self::listings`]): how many rows a create or
    /// a reopen answered with this source has listed
    /// ([`Self::answer_listed`]).
    listings: u64,
}

/// What one fold changed, for the caller to act on outside the lock.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Applied {
    /// Rows that left the swapped-in set — their lanes DEPART (`Removed`, or a
    /// `Ready` swap that dropped them: an epoch reseed produces no `Removed`;
    /// plan 032 §3.1.1 item 2).
    pub departed: Vec<String>,
    /// Rows newly IN the swapped-in set — a `Session` upsert of a hostId not
    /// held, or a `Ready` swap that lists one the old set did not — whose
    /// departing lanes (if any) are live again (plan 032 §3.1.1 item 3, PC2).
    /// A row already held is never in it — one [`CrazeState::listed_now`]
    /// folded in ahead of its frame included: the create or reopen that
    /// listed it published its return itself
    /// ([`crate::roost_hosts::RoostHosts`]'s `list_answered_row`, branch
    /// review), and `listed_now`'s own answer's is always empty.
    ///
    /// The fold publishes it after releasing the state lock, as every
    /// publisher of craze news does, each from its own task — this fold, a
    /// create's or reopen's listing, the host's removal — so a return can
    /// reach a lane after a departure computed later. The news sequence
    /// ([`CrazeState::mint_news`]) orders them: a lane never takes news
    /// older than what it already took.
    ///
    /// Disjoint from [`Self::departed`] — a fold lists or departs a row, never
    /// both — so the two share the fold's one news number.
    pub listed: Vec<String>,
    /// A seed swapped in: the host is listed from now on.
    pub ready: bool,
    /// Something a reader can see changed.
    pub visible: bool,
}

impl CrazeState {
    /// A host's state under the fence `gen`.
    pub fn new(gen: u64) -> Self {
        Self {
            gen,
            ..Self::default()
        }
    }

    /// **The next craze news sequence number** (C1 re-review): what one piece
    /// of this source's news about its lanes carries
    /// ([`crate::lane::CrazeNews`]) — a fresh number per call, strictly
    /// increasing for the life of the source (a new source, a new
    /// generation, starts again, and the generation fence keeps the two
    /// apart). Never 0.
    ///
    /// **Minted under the roost-host layer's state lock, at the moment the
    /// news is computed** — by the source's fold, a create's or reopen's
    /// listing of the row it answered with, and the host's removal
    /// ([`crate::roost_hosts::OnCrazeNews`]). Each publishes AFTER
    /// that lock is released, from its own task, so news can REACH the lane
    /// layer in another order than it happened in; the number is the order it
    /// happened in, and the lane layer applies a piece to a lane only if it is
    /// newer than every piece that lane already took
    /// ([`crate::lane::Lanes::craze_news`]).
    pub fn mint_news(&mut self) -> u64 {
        // A u64 is never exhausted here: one piece of news a microsecond, for
        // the life of one source, would take over 500,000 years to wrap.
        self.news += 1;
        self.news
    }

    /// **The listing epoch** (plan 032 §3.1.1 item 2, C8 review finding 2):
    /// how many rows a create or a reopen answered with this source has
    /// listed ([`Self::answer_listed`]). Read under the roost-host layer's
    /// state lock as a find-only probe — or a roster's `--no-start` dial —
    /// STARTS ([`CrazeSink::listings`]), and handed to [`Self::dormant`] when
    /// that probe's no-hub answer lands: an answer older than a listing is
    /// not folded.
    pub fn listings(&self) -> u64 {
        self.listings
    }

    /// **A create's or reopen's answered row was just listed** (plan 032
    /// §3.1.1 item 3, C8 review): the listing epoch moves — a no-hub answer
    /// whose probe started before now is stale ([`Self::dormant`]) — and the
    /// row's RETURN gets its news number, minted here, under the same lock
    /// ([`Self::mint_news`]).
    ///
    /// **Always, a Dormant folded while the request was in flight or not**
    /// (C8 review): this layer cannot tell a Dormant that rests on a probe
    /// that looked before the create or reopen started its hub from one that
    /// saw the hub die after craze answered, so it sides with the session
    /// craze just said runs — a successful create's or reopen's row always
    /// stays openable, and its return always ends a departure minted before
    /// it. What that accepts — a hub that really died meanwhile — is
    /// [`crate::roost_hosts::RoostHosts`]'s `list_answered_row`'s to say.
    pub fn answer_listed(&mut self) -> u64 {
        self.listings += 1;
        self.mint_news()
    }

    /// Whether the hub feed is live — the one condition the row merge absorbs
    /// on (D4).
    pub fn live(&self) -> bool {
        self.phase == Some(CrazePhase::Live)
    }

    /// Every hostId this host holds a row for.
    pub fn held(&self) -> Vec<String> {
        self.rows.keys().cloned().collect()
    }

    /// Fold one source event (stage-and-swap; the module doc).
    pub fn apply(&mut self, event: SourceEvent) -> Applied {
        let mut out = Applied::default();
        match event {
            SourceEvent::Reset { generation, .. } => {
                self.staged = Some(Staged {
                    generation,
                    ..Staged::default()
                });
            }
            SourceEvent::Session { session } => match self.staged.as_mut() {
                Some(staged) => {
                    staged.rows.insert(session.id.clone(), session);
                }
                None => {
                    let id = session.id.clone();
                    if self.rows.insert(id.clone(), session).is_none() {
                        out.listed.push(id);
                    }
                    out.visible = true;
                }
            },
            SourceEvent::Removed { session_id } => match self.staged.as_mut() {
                Some(staged) => {
                    staged.rows.remove(&session_id);
                }
                None => {
                    if self.rows.remove(&session_id).is_some() {
                        out.departed.push(session_id);
                        out.visible = true;
                    }
                }
            },
            SourceEvent::Capabilities { capabilities } => match self.staged.as_mut() {
                Some(staged) => staged.caps = Some(capabilities),
                None => {
                    self.caps = Some(capabilities);
                    out.visible = true;
                }
            },
            SourceEvent::Ready {
                generation,
                truncated,
            } => {
                // MATCHED, never assumed: only the seed this `Ready` names
                // swaps in.
                if self.staged.as_ref().map(|s| s.generation) == Some(generation) {
                    if let Some(staged) = self.staged.take() {
                        out.departed = self
                            .rows
                            .keys()
                            .filter(|id| !staged.rows.contains_key(*id))
                            .cloned()
                            .collect();
                        out.listed = staged
                            .rows
                            .keys()
                            .filter(|id| !self.rows.contains_key(*id))
                            .cloned()
                            .collect();
                        self.rows = staged.rows;
                        if staged.caps.is_some() {
                            self.caps = staged.caps;
                        }
                        self.truncated = truncated;
                        self.phase = Some(CrazePhase::Live);
                        self.offline = None;
                        out.ready = true;
                        out.visible = true;
                    }
                }
            }
            SourceEvent::Offline { reason, cause } => {
                // A loss abandons a seed in progress; the last swapped-in rows
                // stay, and render stale.
                self.staged = None;
                self.phase = Some(CrazePhase::Offline);
                self.offline = Some((cause, reason));
                out.visible = true;
            }
            SourceEvent::Unknown => {}
        }
        out
    }

    /// The probe found no hub: nothing is listed, and every row (and its lane)
    /// goes.
    ///
    /// **`created` departs too** (plan 032 PC14g, branch review): the
    /// source's created rows read now ([`shed_craze::CrazeSource::created_rows`],
    /// less the rows held here) — sessions a create or a reopen answered with
    /// that no roster has listed yet. Their lanes ride the hub as much as a
    /// roster row's do, so a hub gone before the first seed departs them
    /// `hub_gone` as well; otherwise nothing would ever end such a lane on
    /// screen. They are the source's to drop, not this state's (they stay
    /// listed until the source lets them go), so they change nothing a reader
    /// sees here: a repeated dormant probe repaints nothing, and only departs
    /// such a lane again if a listing brought it back meanwhile.
    ///
    /// **Only if no create or reopen listed a row since the answer's probe
    /// started** (C8 review finding 2): `probed_at` is the listing epoch
    /// ([`Self::listings`]) read as the probe — or the roster's `--no-start`
    /// dial — that found no hub STARTED. A listing since then answered with
    /// a session the probe may have looked too early to see: the create or
    /// reopen may have started the hub it runs on after the probe looked. So
    /// that answer is STALE and folds nothing — `None`, nothing changed,
    /// nothing minted — and the source probes again at once
    /// ([`attach_only`]); folded, it would depart the new session's lane
    /// with news newer than the listing's return, and its grace would end a
    /// running session's panel `hub_gone`.
    pub fn dormant(&mut self, created: &[LaneSession], probed_at: u64) -> Option<Applied> {
        if self.listings != probed_at {
            return None;
        }
        let held = self.held();
        let visible = !held.is_empty() || self.phase != Some(CrazePhase::Dormant);
        let mut departed = held;
        for row in created {
            if !departed.contains(&row.id) {
                departed.push(row.id.clone());
            }
        }
        self.rows.clear();
        self.staged = None;
        self.caps = None;
        self.truncated = false;
        self.phase = Some(CrazePhase::Dormant);
        self.offline = None;
        Some(Applied {
            departed,
            listed: Vec::new(),
            ready: false,
            visible,
        })
    }

    /// The probe could not ask: retained rows stay, stale.
    pub fn offline(&mut self, cause: SourceOffline, reason: String) -> Applied {
        let said = Some((cause, reason));
        let visible = self.phase != Some(CrazePhase::Offline) || self.offline != said;
        self.staged = None;
        self.phase = Some(CrazePhase::Offline);
        self.offline = said;
        Applied {
            departed: Vec::new(),
            listed: Vec::new(),
            ready: false,
            visible,
        }
    }

    /// A row the source's ROSTER already lists, folded in ahead of its own
    /// `Session` frame (plan 025 §3.6.4): a create answers once its session
    /// runs, by when the roster has usually listed it — but that frame can
    /// still be queued behind the create's answer, and the transcript the
    /// sheet opens next resolves its row here. The frame that follows is the
    /// same row, and every later frame (a `Removed`, a swap) governs it as any
    /// roster row — so this holds nothing of its own: a row only a CREATE
    /// answered with is never folded here; it is the source's
    /// ([`shed_craze::CrazeSource::created_rows`]), read at listing time.
    ///
    /// Into a seed in progress too, so that seed's swap cannot depart it (and
    /// its lane) before its frame lands. Never over a row already held.
    ///
    /// **It reports no return** (plan 032 §3.1.1 item 3): its answer's
    /// [`Applied::listed`] is always empty, and the frame that lands next
    /// reports none either — the row is already held. The caller lists the
    /// row and publishes its return itself, a created-only row's too, under a
    /// news number it mints under the same lock
    /// ([`crate::roost_hosts::RoostHosts`]'s `list_answered_row`, branch
    /// review): the news sequence, not the publishing task, orders it with
    /// the fold's departures.
    pub fn listed_now(&mut self, session: LaneSession) -> Applied {
        if let Some(staged) = self.staged.as_mut() {
            staged
                .rows
                .entry(session.id.clone())
                .or_insert_with(|| session.clone());
        }
        let visible = !self.rows.contains_key(&session.id);
        self.rows.entry(session.id.clone()).or_insert(session);
        Applied {
            departed: Vec::new(),
            listed: Vec::new(),
            ready: false,
            visible,
        }
    }

    /// Where the source stands as a status reader can tell it: its phase and,
    /// while offline, the CAUSE — never the reason's words. An UNLISTED host's
    /// repaint is decided on a change of this (plan 026 §3.2), so a repeated
    /// probe that says the same thing in other words repaints nothing.
    pub fn standing(&self) -> (Option<CrazePhase>, Option<SourceOffline>) {
        (
            self.phase,
            self.offline.as_ref().map(|(cause, _)| cause.clone()),
        )
    }

    /// Whether the find-only probe has PROVED craze on this host with no hub
    /// roster to list (plan 026 §3.2): **dormant** (craze is there and no hub
    /// runs — somewhere to create a session, the sheet starting its hub), or
    /// offline **too old** (craze is there, and the note says to update it).
    /// What makes an UNLISTED host an `rc.list` `shed_craze` entry; not
    /// installed, unreachable, any other offline, never probed and no craze
    /// source at all are not.
    pub fn proven_unattached(&self) -> bool {
        match (self.gen, self.phase, &self.offline) {
            (0, ..) => false,
            (_, Some(CrazePhase::Dormant), _) => true,
            (_, Some(CrazePhase::Offline), Some((SourceOffline::TooOld, _))) => true,
            _ => false,
        }
    }

    /// The status row's `craze` half (plan 025 §3.6.2): `state` is `live`,
    /// `dormant`, `offline` or `absent` (never reached — and not installed,
    /// which renders as absent); `cause` names an offline (or not-installed)
    /// state's reason class; `create`/`create_options`/`create_title` are the
    /// LIVE hub's capabilities, `false` otherwise — so a dormant machine's
    /// create sheet offers "Name (optional)" once its hub is live, exactly as
    /// it learns `create_options` (plan 029 P10).
    ///
    /// `source_gen` is this source's generation ([`Self::gen`]) — the host's
    /// INCARNATION, as `craze.create_options`, `craze.saved` and
    /// `craze.reopen` report it — `null` while the host has no craze source
    /// (gen `0`). A host removed and registered again under the same address
    /// is a new incarnation with a new generation, and generations only GROW
    /// (one counter for the app's life), so a later incarnation's is always
    /// the larger. Carried on every status (C5 re-review) so the sheet's
    /// per-machine Resume state is reset by the status it already reads —
    /// sheet open or not — and never waits on a create-options read that may
    /// fail.
    pub fn status(&self) -> Value {
        let (state, cause) = match (self.gen, self.phase, &self.offline) {
            (0, ..) | (_, None, _) => ("absent", None),
            (_, Some(CrazePhase::Live), _) => ("live", None),
            (_, Some(CrazePhase::Dormant), _) => ("dormant", None),
            (_, Some(CrazePhase::Offline), Some((SourceOffline::NotInstalled, _))) => {
                ("absent", Some("not_installed".to_string()))
            }
            (_, Some(CrazePhase::Offline), Some((cause, _))) => {
                ("offline", Some(cause.as_str().to_string()))
            }
            (_, Some(CrazePhase::Offline), None) => ("offline", None),
        };
        let caps = self.caps.as_ref().filter(|_| self.live());
        json!({
            "state": state,
            "cause": cause,
            "create": caps.is_some_and(|c| c.create),
            "create_options": caps.is_some_and(|c| c.create_options),
            "create_title": caps.is_some_and(|c| c.create_title),
            "source_gen": (self.gen != 0).then_some(self.gen),
        })
    }
}

// ---------------------------------------------------------------------------
// the create sheet's ops: their refusals
// ---------------------------------------------------------------------------

/// A refusal of one of the craze ops a user's click lands in —
/// `craze.create_options`, `craze.create`, `craze.open_terminal` (plan 025
/// §3.6.7) — as BOTH doors carry it: the socket's `{code, message}` envelope,
/// and a `#[tauri::command]`'s `"<code>: <message>"` string, which `bridge.ts`
/// splits back (the `lane.*` ops' rule, so one vocabulary crosses both).
///
/// The codes are the lane contract's own ([`crate::lane::LaneFailure::code`]),
/// plus the ones only a create sheet needs:
///
/// * `outcome_unknown` — a create whose answer was lost, twice
///   ([`shed_craze::is_outcome_unknown`]), or a HELD create (a replay of a
///   kept id, plan 026 §3.1) that got no answer about its request — not even
///   this layer's own preflight (an unregistered host, craze offline or too
///   old): the ONE refusal after which the sheet keeps its request id (plan
///   025 §3.8);
/// * `too_old` — this machine's craze cannot list providers or create
///   (`createOptions`/`sessionCreate` missing from its hub's `hello`, or a
///   v0.0.1): the sheet says "update craze on this machine";
/// * `not_installed` / `no_craze` — no craze there, or no craze source for
///   the host at all;
/// * `bad_request` — a caller's mistake (an unknown host, a request id not in
///   craze's form, a session this machine does not list, a host id that is not
///   craze's form, a blank saved-session id) as well as craze's own
///   `bad_request`.
///
/// And the ones only a RESUME needs (`craze.reopen`, plan 032 §3.4) — one per
/// [`shed_core::lane::LaneReopenError`] variant, so the sheet branches on the
/// code and never on the words ([`CrazeFailure::reopen`]):
///
/// * `gone` — the saved session cannot run here any more (no such id, its
///   directory gone): the sheet drops its row;
/// * `start_failed` — it was found and its start failed (`could not start:
///   <cause>`): shown, the row kept;
/// * `unavailable` — transient, a lost answer included ("Retry is safe": a
///   reopen is idempotent), and a hub that cannot reopen saved sessions at
///   all (craze's older-hub words): shown with Retry;
/// * anything else is the inner [`shed_core::lane::LaneError`]'s own code
///   (`bad_request`, `failed`, …): shown, the row kept.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CrazeFailure {
    pub code: &'static str,
    pub message: String,
}

impl CrazeFailure {
    pub fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }

    pub fn bad_request(message: impl Into<String>) -> Self {
        Self::new("bad_request", message)
    }

    /// A source call's [`LaneError`](shed_core::lane::LaneError): the lane
    /// contract's code, except a lost create's — `outcome_unknown`, its own.
    pub fn lane(e: shed_core::lane::LaneError) -> Self {
        if shed_craze::is_outcome_unknown(&e) {
            return Self::new("outcome_unknown", e.to_string());
        }
        let message = e.to_string();
        Self::new(crate::lane::LaneFailure::Lane(e).code(), message)
    }

    /// A reopen's [`LaneReopenError`](shed_core::lane::LaneReopenError) (plan
    /// 032 §3.4), its VARIANT kept visible as the code — `gone`,
    /// `start_failed`, `unavailable` — and an inner contract error as that
    /// error's own code. Never `outcome_unknown`: a reopen is idempotent, so a
    /// lost answer is the source's `Unavailable` ("Retry is safe", PC14d),
    /// and the sheet holds nothing a lost answer would make it keep.
    pub fn reopen(e: shed_core::lane::LaneReopenError) -> Self {
        use shed_core::lane::LaneReopenError;
        let message = e.to_string();
        match e {
            LaneReopenError::Gone(_) => Self::new("gone", message),
            LaneReopenError::StartFailed(_) => Self::new("start_failed", message),
            LaneReopenError::Unavailable(_) => Self::new("unavailable", message),
            LaneReopenError::Lane(e) => {
                Self::new(crate::lane::LaneFailure::Lane(e).code(), message)
            }
        }
    }

    /// The `#[tauri::command]` door's error string.
    pub fn command_string(&self) -> String {
        format!("{}: {}", self.code, self.message)
    }
}

/// A `craze.create` request from CALLER-SUPPLIED fields — the one place both
/// doors (the `craze.create` socket op and the `craze_create` command) decide
/// what they mean, so the same request cannot mean two things by door:
///
/// * `cwd` is required and must be ABSOLUTE (trimmed; plan 025 §3.8 refuses a
///   relative or empty path client-side, and so does this) — craze itself
///   checks that it exists;
/// * a blank `provider` is absent (craze's default provider);
/// * a blank `prompt` is absent (an idle session); any other prompt goes
///   EXACTLY as typed, newlines and all;
/// * `title` (plan 029 D14, the sheet's "Name (optional)") is trimmed, and a
///   blank one is absent — never sent empty (craze refuses an empty title);
///   a hub that cannot take one is `shed_craze`'s create gate's (P11);
/// * `request_id`, when PRESENT, must be craze's form
///   ([`shed_craze::valid_request_id`]) and is used exactly as given — the
///   caller is retrying an unknown outcome under it, or minted it itself. A
///   present id that is empty, blank or not craze's form is `bad_request`, never
///   quietly replaced: minting a fresh one in its place would turn the retry of
///   an unknown outcome into a SECOND session. Only an ABSENT id is minted here
///   ([`shed_craze::new_request_id`]). (A door whose wire can carry a
///   non-string `request_id` refuses that before it gets here — `ipc.rs`'s
///   `craze_create_request`.)
pub fn create_request(
    cwd: Option<&str>,
    provider: Option<&str>,
    prompt: Option<&str>,
    title: Option<&str>,
    request_id: Option<&str>,
) -> Result<shed_core::lane::LaneCreateRequest, CrazeFailure> {
    let cwd = cwd.map(str::trim).unwrap_or_default();
    if !cwd.starts_with('/') {
        return Err(CrazeFailure::bad_request(format!(
            "the session's directory must be an absolute path, got {cwd:?}"
        )));
    }
    let request_id = match request_id {
        Some(id) if shed_craze::valid_request_id(id) => id.to_string(),
        Some(id) => {
            return Err(CrazeFailure::bad_request(format!(
                "request id {id:?} is not craze's form (1–64 of [A-Za-z0-9._-])"
            )))
        }
        None => shed_craze::new_request_id(),
    };
    let trimmed = |s: Option<&str>| {
        s.map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
    };
    Ok(shed_core::lane::LaneCreateRequest {
        cwd: cwd.to_string(),
        provider: trimmed(provider),
        prompt: prompt.filter(|s| !s.trim().is_empty()).map(str::to_string),
        request_id,
        title: trimmed(title),
    })
}

/// Whether a `craze.create` REPLAYS a held request id (plan 026 §3.1) — what
/// `held` means at both doors. The sheet sets it exactly when it re-sends the
/// id it kept from an unknown outcome; the create then ends that id only on
/// craze's own answer about it ([`shed_core::lane::AgentSource::create_held`]).
/// A held create without a `request_id` is `bad_request`: there is nothing to
/// replay, and minting one in its place would make the "replay" a new request.
pub fn held_create(held: bool, request_id: Option<&str>) -> Result<bool, CrazeFailure> {
    if held && request_id.is_none() {
        return Err(CrazeFailure::bad_request(
            "a held create replays the request id it was held under: send `request_id` with `held`",
        ));
    }
    Ok(held)
}

/// The saved session a `craze.reopen` names, from a CALLER-SUPPLIED `id` —
/// both doors' one reading (plan 032 §3.4). The id is OPAQUE (craze's
/// session id, or `<provider>:<providerSessionId>` for a session recorded
/// before craze ids — [`shed_core::lane::LaneSavedSession::id`]) and goes to
/// craze exactly as `sessions.saved` listed it, inside a JSON request: never
/// spliced into a command line, so nothing about its form is checked here. A
/// missing, empty or blank one is `bad_request`: it names no session.
pub fn reopen_id(id: Option<&str>) -> Result<String, CrazeFailure> {
    match id {
        Some(id) if !id.trim().is_empty() => Ok(id.to_string()),
        _ => Err(CrazeFailure::bad_request(
            "a reopen names the saved session it runs: send its `id`, as `craze.saved` listed it",
        )),
    }
}

// ---------------------------------------------------------------------------
// the loop
// ---------------------------------------------------------------------------

/// Where a host's [`run`] task reports — implemented by the roost-host layer,
/// which owns the lock the state lives under. Every method answers `false`
/// (or [`Dormancy::Gone`]) once the host this task was started for is gone
/// (removed, or re-registered under a newer fence): the task then ends.
pub trait CrazeSink: Send + Sync {
    fn event(&self, event: SourceEvent) -> bool;
    /// The host's listing epoch now ([`CrazeState::listings`]), read under
    /// the state lock — what a find-only probe, or a roster's `--no-start`
    /// dial, records as it STARTS, for the [`Self::dormant`] its no-hub
    /// answer may become (C8 review finding 2). `0` once the host is gone:
    /// the next call says so.
    fn listings(&self) -> u64;
    /// The hub is gone, by an answer whose probe (or roster dial) started at
    /// listing epoch `probed_at` ([`CrazeState::dormant`]).
    fn dormant(&self, probed_at: u64) -> Dormancy;
    fn offline(&self, cause: SourceOffline, reason: String) -> bool;
}

/// What [`CrazeSink::dormant`] made of a no-hub answer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dormancy {
    /// Folded: the source is Dormant, its rows — and their lanes — gone.
    Folded,
    /// Not folded — STALE (C8 review finding 2): a create or a reopen listed
    /// a session since the answer's probe started, and may have started the
    /// hub it runs on after the probe looked. Nothing changed; the source
    /// probes again at once, and that answer decides.
    Stale,
    /// The host this task was started for is gone: the task ends.
    Gone,
}

/// One host's craze source, for as long as the host is registered (the module
/// doc's state machine). Aborting the task drops the roster subscription,
/// whose stop ends the source's pump and kills its bridge child.
///
/// `wake` cuts an attach-only source's wait short: a user's explicit action
/// that may have started a hub there (the create sheet's `create_options`, a
/// create) wakes it, so it probes again NOW and attaches to that hub. A wake
/// that arrives while no wait is pending is kept for the next one (one
/// `Notify` permit), so it is never lost to a probe in flight; an eager
/// source has no wait to cut.
pub async fn run(
    source: CrazeSource,
    probe: Option<Arc<dyn CrazeProbe>>,
    sink: Arc<dyn CrazeSink>,
    timings: CrazeTimings,
    wake: Arc<tokio::sync::Notify>,
) {
    match probe {
        None => eager(source, sink).await,
        Some(probe) => attach_only(source, probe, sink, timings, wake).await,
    }
}

/// Sleep `d`, or until `wake` is notified — whichever is first.
async fn wait(d: Duration, wake: &tokio::sync::Notify) {
    tokio::select! {
        _ = tokio::time::sleep(d) => {}
        _ = wake.notified() => {}
    }
}

/// This machine: one subscription for good — the source redials on its own,
/// and its `bridge --hub` births a hub when there is none.
async fn eager(source: CrazeSource, sink: Arc<dyn CrazeSink>) {
    use shed_core::lane::AgentSource as _;
    let Ok(subscription) = source.subscribe().await else {
        return;
    };
    let (mut rx, _stop) = subscription.into_parts();
    while let Some(event) = rx.recv().await {
        if !sink.event(event) {
            return;
        }
    }
}

/// A remote host: probe, attach only to a hub that is there, and probe again
/// whenever the roster drops (the module doc's table).
///
/// **A no-hub answer is judged against the listings since it was asked**
/// (C8 review finding 2): the probe — and the roster's own `--no-start`
/// dial, the other way this loop learns the hub is gone — records the
/// listing epoch ([`CrazeSink::listings`]) as it STARTS, and a create or a
/// reopen that listed a session before its answer lands makes that answer
/// stale ([`Dormancy::Stale`]): it is not folded, and the loop probes again
/// at once, rather than departing a just-started session's lane on evidence
/// from before its hub existed.
async fn attach_only(
    source: CrazeSource,
    probe: Arc<dyn CrazeProbe>,
    sink: Arc<dyn CrazeSink>,
    t: CrazeTimings,
    wake: Arc<tokio::sync::Notify>,
) {
    use shed_core::lane::AgentSource as _;
    let mut backoff = t.backoff_base;
    loop {
        let probed_at = sink.listings();
        match probe.probe().await {
            Probe::Hub => {
                // The roster's dial starts with the subscription (and the
                // loop drops it at its first `Offline`, so a `NoHub` is its
                // first dial's).
                let dialled_at = sink.listings();
                let Ok(subscription) = source.subscribe().await else {
                    return;
                };
                let (mut rx, stop) = subscription.into_parts();
                let mut reached = false;
                let mut no_hub = false;
                loop {
                    let Some(event) = rx.recv().await else {
                        return;
                    };
                    // **The hub went between the probe and the roster's dial**
                    // (plan 032 §3.5): its `--no-start` found none, and started
                    // none. That is no outage — nothing runs there — so the
                    // source is DORMANT, not offline.
                    if let SourceEvent::Offline {
                        cause: SourceOffline::NoHub,
                        ..
                    } = &event
                    {
                        no_hub = true;
                        break;
                    }
                    let offline = matches!(event, SourceEvent::Offline { .. });
                    reached |= matches!(event, SourceEvent::Ready { .. });
                    if !sink.event(event) {
                        return;
                    }
                    if offline {
                        break;
                    }
                }
                // The roster is over: drop it (its bridge with it) and let the
                // probe say whether there is still a hub to re-join.
                drop(stop);
                drop(rx);
                if no_hub {
                    // As a probe's `NoHub` below: dormant, and the next probe on
                    // the dormant clock — or, stale, the next probe now.
                    match sink.dormant(dialled_at) {
                        Dormancy::Gone => return,
                        Dormancy::Stale => continue,
                        Dormancy::Folded => {}
                    }
                    backoff = t.backoff_base;
                    wait(t.dormant, &wake).await;
                    continue;
                }
                backoff = next_backoff(backoff, reached, t.backoff_base, t.backoff_max);
                wait(
                    jittered(if reached { t.backoff_base } else { backoff }),
                    &wake,
                )
                .await;
            }
            Probe::NoHub => {
                match sink.dormant(probed_at) {
                    Dormancy::Gone => return,
                    Dormancy::Stale => continue,
                    Dormancy::Folded => {}
                }
                backoff = t.backoff_base;
                wait(t.dormant, &wake).await;
            }
            Probe::Offline { cause, reason } => {
                let slow = matches!(cause, SourceOffline::NotInstalled | SourceOffline::TooOld);
                if !sink.offline(cause, reason) {
                    return;
                }
                if slow {
                    wait(t.dormant, &wake).await;
                } else {
                    wait(jittered(backoff), &wake).await;
                    backoff = next_backoff(backoff, false, t.backoff_base, t.backoff_max);
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_core::rc::RcActivity;

    fn row(id: &str) -> LaneSession {
        LaneSession {
            id: id.to_string(),
            title: id.to_string(),
            cwd: "/work".to_string(),
            activity: RcActivity::Idle,
            ..LaneSession::default()
        }
    }

    fn caps() -> SourceCapabilities {
        SourceCapabilities {
            kind: "craze".to_string(),
            create: true,
            create_options: true,
            create_title: true,
            reopen: false,
        }
    }

    fn seed(state: &mut CrazeState, generation: u64, ids: &[&str]) -> Applied {
        state.apply(SourceEvent::Reset {
            reason: "connect".into(),
            generation,
        });
        for id in ids {
            state.apply(SourceEvent::Session { session: row(id) });
        }
        state.apply(SourceEvent::Capabilities {
            capabilities: caps(),
        });
        state.apply(SourceEvent::Ready {
            generation,
            truncated: false,
        })
    }

    /// A no-hub answer whose probe started just now — no listing since — so
    /// it folds.
    fn dormant(state: &mut CrazeState, created: &[LaneSession]) -> Applied {
        let probed_at = state.listings();
        state.dormant(created, probed_at).expect("no listing since")
    }

    #[test]
    fn a_seed_swaps_in_only_on_its_own_ready() {
        let mut s = CrazeState::new(1);
        s.apply(SourceEvent::Reset {
            reason: "connect".into(),
            generation: 1,
        });
        s.apply(SourceEvent::Session { session: row("a") });
        assert!(s.rows.is_empty(), "staged, not live");
        assert_eq!(s.status()["state"], "absent");
        let wrong = s.apply(SourceEvent::Ready {
            generation: 9,
            truncated: false,
        });
        assert!(
            !wrong.ready && s.rows.is_empty(),
            "a Ready for another seed"
        );
        let applied = s.apply(SourceEvent::Ready {
            generation: 1,
            truncated: false,
        });
        assert!(applied.ready);
        assert_eq!(s.held(), ["a"]);
        assert!(s.live());
        assert_eq!(s.status()["state"], "live");
    }

    /// A reseed that no longer lists a row departs it — the eviction signal an
    /// epoch reseed (which carries no `Removed`) would otherwise never send.
    #[test]
    fn a_swap_that_drops_a_row_departs_it() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a", "b"]);
        let applied = seed(&mut s, 2, &["b", "c"]);
        assert_eq!(applied.departed, ["a"]);
        assert_eq!(s.held(), ["b", "c"]);
        let removed = s.apply(SourceEvent::Removed {
            session_id: "b".into(),
        });
        assert_eq!(removed.departed, ["b"]);
    }

    /// **A fold says which rows it newly LISTS** (plan 032 §3.1.1 item 3):
    /// the return signal a departing lane is waiting for — a `Ready` swap's
    /// rows the old set did not hold, and a live `Session` upsert of a hostId
    /// not held — and never a row already held, nor anything staged.
    #[test]
    fn a_fold_reports_the_rows_it_newly_lists() {
        let mut s = CrazeState::new(1);
        assert_eq!(seed(&mut s, 1, &["a", "b"]).listed, ["a", "b"]);
        let swapped = seed(&mut s, 2, &["b", "c"]);
        assert_eq!(
            (swapped.departed, swapped.listed),
            (vec!["a".to_string()], vec!["c".to_string()])
        );
        let held = s.apply(SourceEvent::Session { session: row("b") });
        assert!(
            held.listed.is_empty(),
            "an upsert of a held row is no return"
        );
        let new = s.apply(SourceEvent::Session { session: row("d") });
        assert_eq!(new.listed, ["d"]);
        s.apply(SourceEvent::Reset {
            reason: "connect".into(),
            generation: 3,
        });
        let staged = s.apply(SourceEvent::Session { session: row("e") });
        assert!(staged.listed.is_empty(), "staged rows wait for their Ready");
        // The hub gone, then back: every row it lists is a return.
        dormant(&mut s, &[]);
        assert_eq!(seed(&mut s, 4, &["a"]).listed, ["a"]);
    }

    /// **A row folded in ahead of its frame reports no return, and neither
    /// does its frame** (plan 032 §3.1.1 item 3, branch review): the create
    /// or reopen that listed it published the return itself, minted under the
    /// state lock (`RoostHosts::list_answered_row`), so the fold never says
    /// it again — not the live upsert of its frame, nor the swap of the seed
    /// it was folded into. From then on the roster's frames govern it as any
    /// row: a `Removed` before its frame departs it, and a later listing is a
    /// fresh return.
    #[test]
    fn a_row_folded_ahead_reports_no_return_of_its_own() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let ahead = s.listed_now(row("n"));
        assert!(ahead.visible && ahead.listed.is_empty(), "{ahead:?}");
        assert_eq!(s.held(), ["a", "n"]);
        let framed = s.apply(SourceEvent::Session { session: row("n") });
        assert!(framed.listed.is_empty(), "its listing said it: {framed:?}");

        // Folded into a seed in progress: its swap keeps it, and says nothing.
        s.apply(SourceEvent::Reset {
            reason: "connect".into(),
            generation: 2,
        });
        s.apply(SourceEvent::Session { session: row("a") });
        s.apply(SourceEvent::Session { session: row("n") });
        assert!(s.listed_now(row("m")).listed.is_empty());
        let swapped = s.apply(SourceEvent::Ready {
            generation: 2,
            truncated: false,
        });
        assert!(
            swapped.departed.is_empty() && swapped.listed.is_empty(),
            "{swapped:?}"
        );
        assert_eq!(s.held(), ["a", "m", "n"]);

        // Removed before its frame: it departs, and a later listing returns.
        s.listed_now(row("p"));
        let removed = s.apply(SourceEvent::Removed {
            session_id: "p".into(),
        });
        assert_eq!(removed.departed, ["p"]);
        let relisted = s.apply(SourceEvent::Session { session: row("p") });
        assert_eq!(relisted.listed, ["p"], "a fresh listing");
    }

    /// Retained across an `Offline`, dropped by a `Dormant`.
    #[test]
    fn offline_retains_and_dormant_drops() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        s.apply(SourceEvent::Offline {
            reason: "the connection to craze's hub closed".into(),
            cause: SourceOffline::Unreachable,
        });
        assert_eq!(s.held(), ["a"], "retained, stale");
        assert!(!s.live(), "but not live: the merge absorbs nothing");
        assert_eq!(s.status()["state"], "offline");
        assert_eq!(s.status()["cause"], "unreachable");
        let gone = dormant(&mut s, &[]);
        assert_eq!(gone.departed, ["a"]);
        assert!(s.rows.is_empty());
        assert_eq!(s.status()["state"], "dormant");
    }

    /// **A Dormant departs the source's created rows too** (plan 032 PC14g,
    /// branch review): a lane opened on a session only a create or a reopen
    /// answered with rides the hub as much as a roster row's, so the hub gone
    /// before any seed listed it departs it with the rest. They are the
    /// source's rows, not this state's — nothing a reader sees changes for
    /// them, so a repeated dormant probe still repaints nothing — and a row
    /// both held and created departs once.
    #[test]
    fn a_dormant_departs_the_created_rows_too() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let first = dormant(&mut s, &[row("c"), row("a")]);
        assert_eq!(first.departed, ["a", "c"], "{first:?}");
        assert!(first.visible && first.listed.is_empty());
        let again = dormant(&mut s, &[row("c")]);
        assert_eq!(again.departed, ["c"], "still listed by the source");
        assert!(!again.visible, "a repeated probe repaints nothing");
    }

    /// **A no-hub answer older than a listing folds nothing** (plan 032
    /// §3.1.1 item 2, C8 review finding 2): once a create or a reopen listed
    /// a row, the listing epoch read as an earlier probe STARTED is behind —
    /// the session may run on a hub that probe looked too early to see — so
    /// that probe's `dormant` is `None`: no row dropped, nothing departed and
    /// no news number minted. A probe started after the listing folds as
    /// ever.
    #[test]
    fn a_no_hub_answer_older_than_a_listing_folds_nothing() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let probed_at = s.listings();
        let returned = s.answer_listed();
        assert!(s.dormant(&[row("c")], probed_at).is_none(), "stale");
        assert_eq!(s.held(), ["a"], "nothing dropped");
        assert!(s.live());
        assert_eq!(s.mint_news(), returned + 1, "no news minted");
        let gone = dormant(&mut s, &[row("c")]);
        assert_eq!(gone.departed, ["a", "c"], "a probe from after it folds");
    }

    /// **A listing returns its row even after a Dormant folded while its
    /// request was in flight** (plan 032 §3.1.1 item 3, C8 review): craze's
    /// answer says the session runs, so its return is minted all the same —
    /// in the news sequence, newer than that Dormant's departure (minted as
    /// the fold mints it, after folding), so it ends that departure — the
    /// listing epoch moves, and the roster's row, folded ahead, is held
    /// again (openable). If the hub really stayed gone, a probe started
    /// after the listing finds none and departs the row again.
    #[test]
    fn a_listing_after_a_dormant_still_returns_its_row() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        // The request goes out, and the hub goes before its row is listed.
        let gone = dormant(&mut s, &[]);
        assert_eq!(gone.departed, ["a"]);
        let departed = s.mint_news();
        let listings = s.listings();
        let returned = s.answer_listed();
        assert!(returned > departed, "newer than the departure it ends");
        assert_eq!(s.listings(), listings + 1, "and it was listed");
        s.listed_now(row("a"));
        assert_eq!(s.held(), ["a"], "held again: openable");
        // The hub stayed gone: a probe from after the listing departs it.
        let again = dormant(&mut s, &[]);
        assert_eq!(again.departed, ["a"]);
    }

    /// A probe with one fixed answer, counting the times it is asked.
    struct FixedProbe(Probe, Arc<std::sync::atomic::AtomicUsize>);

    impl FixedProbe {
        fn counted(answer: Probe) -> (Arc<dyn CrazeProbe>, Arc<std::sync::atomic::AtomicUsize>) {
            let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
            (Arc::new(FixedProbe(answer, Arc::clone(&calls))), calls)
        }
    }

    impl CrazeProbe for FixedProbe {
        fn probe(&self) -> BoxFut<Probe> {
            self.1.fetch_add(1, Ordering::SeqCst);
            let answer = self.0.clone();
            Box::pin(async move { answer })
        }
    }

    /// A fake `ssh` under `dir`: records each remote command (its last
    /// argument, one line each) to `<dir>/ssh.log`, ignores `-O` control
    /// commands, and then answers it by its text:
    ///
    /// * `*--no-start*` — runs `<dir>/no-start.sh` when it exists (a case's
    ///   own ending: 0.1.x's refusal, exit 3, …), else a duplex (`cat`);
    /// * `*providers*` — a hub's `createOptions` answer, exit 0 (the probe
    ///   finds a hub);
    /// * anything else — a HUB: it answers the client's first line (a hub
    ///   `hello`, id `"1"`) with `hello`, then drains its stdin.
    ///
    /// Written under a staging name and renamed into place (a script still
    /// open for writing while a sibling test forks answers `execve` with
    /// ETXTBSY).
    fn fake_ssh(dir: &std::path::Path) -> std::path::PathBuf {
        use std::os::unix::fs::PermissionsExt as _;

        let hello = json!({"jsonrpc": "2.0", "id": "1",
                           "result": shed_craze::testing::hub_hello_result(
                               "0a1b2c3d4e5f", shed_craze::testing::reopen_hub_capabilities())});
        let script = format!(
            "#!/bin/sh\n\
             for a in \"$@\"; do [ \"$a\" = -O ] && exit 0; last=\"$a\"; done\n\
             printf '%s\\n' \"$last\" >> '{log}'\n\
             case \"$last\" in\n\
             *--no-start*) if [ -e '{ns}' ]; then exec /bin/sh '{ns}'; fi; exec cat ;;\n\
             *providers*) printf '%s\\n' '{{\"providers\":[],\"recentDirs\":[]}}'; exit 0 ;;\n\
             esac\n\
             IFS= read -r line\n\
             printf '%s\\n' '{hello}'\n\
             exec cat >/dev/null\n",
            log = dir.join("ssh.log").display(),
            ns = dir.join("no-start.sh").display(),
        );
        let staged = dir.join("ssh.staging");
        let ssh = dir.join("ssh");
        std::fs::write(&staged, script).expect("write");
        std::fs::set_permissions(&staged, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        std::fs::rename(&staged, &ssh).expect("rename");
        ssh
    }

    /// An `SshExec` for "mini3" through the fake `ssh` under `dir`.
    fn fake_exec(dir: &std::path::Path) -> Arc<SshExec> {
        let entry = shed_core::config::MachineEntry {
            name: "mini3".to_string(),
            host: "mini3".to_string(),
            ssh_port: 22,
            ..Default::default()
        };
        let options = shed_app::roost::SshBridgeOptions {
            ssh_bin: Some(fake_ssh(dir)),
            ..Default::default()
        };
        Arc::new(SshExec::new(&entry, &options).expect("exec"))
    }

    /// The remote commands the fake `ssh` under `dir` recorded, in order —
    /// once `want` of them have landed (a spawned child records a moment
    /// after the dial answers).
    async fn recorded(dir: &std::path::Path, want: usize) -> Vec<String> {
        let mut lines = Vec::new();
        for _ in 0..500 {
            lines = std::fs::read_to_string(dir.join("ssh.log"))
                .unwrap_or_default()
                .lines()
                .map(str::to_string)
                .collect();
            if lines.len() >= want {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        lines
    }

    /// `connect_hub` through `dial`, as the source and the lanes run it.
    async fn connect(
        dial: &dyn CrazeDial,
    ) -> Result<(shed_craze::conn::Conn, shed_craze::conn::Notifications), DialError> {
        let client = shed_craze::wire::ClientInfo::shed("t");
        shed_craze::connect_hub(
            dial,
            &client,
            Duration::from_secs(10),
            Duration::from_secs(10),
        )
        .await
        .map(|(conn, notes, _hello)| (conn, notes))
    }

    /// The probe's verdicts a LEGACY dial turns into errors without spawning
    /// anything: no hub is `Unreachable` in this app's words, and not
    /// installed / too old / unreachable keep their causes.
    async fn assert_legacy_gate(exec: &Arc<SshExec>, dir: &std::path::Path) {
        let legacy = |probe: Probe| {
            let (probe, _) = FixedProbe::counted(probe);
            let dial = SshCrazeDial::new(
                Arc::clone(exec),
                "BRIDGE-HUB".to_string(),
                "NO-START-HUB --no-start".to_string(),
                probe,
            );
            assert!(dial.fall_back(&DialError::NoStartUnsupported("x".into())));
            dial
        };
        let before = recorded(dir, 0).await.len();
        let err = legacy(Probe::NoHub).dial().await.err().expect("no hub");
        assert!(
            matches!(&err, DialError::Unreachable(m) if m.contains("no craze hub is running")),
            "{err:?}"
        );
        for (cause, want) in [
            (SourceOffline::NotInstalled, "NotInstalled"),
            (SourceOffline::TooOld, "TooOld"),
            (SourceOffline::Unreachable, "Unreachable"),
        ] {
            let err = legacy(Probe::Offline {
                cause,
                reason: "said".to_string(),
            })
            .dial()
            .await
            .err()
            .expect("offline");
            assert!(format!("{err:?}").starts_with(want), "{err:?}");
        }
        assert_eq!(
            recorded(dir, 0).await.len(),
            before,
            "no ssh ran while the probe said there was nothing to join"
        );
    }

    /// **A remote dial reaches only a hub that is already running** (plan 032
    /// §3.5; plan 025 §3.6.1 before it). In `NoStart` it spawns the find-only
    /// `bridge --hub --no-start` and asks NO probe — it cannot start a hub, so
    /// there is nothing to gate. Fallen back to `Legacy`, it is probe-gated:
    /// with no hub running it answers `Unreachable` and never spawns `bridge
    /// --hub` over ssh, not installed and too old keep their causes, and a
    /// hub that is there gets its duplex. The UNGATED instance —
    /// `CrazeReach::ensure`, a user's explicit action — asks no probe and runs
    /// the plain bridge, and has no fallback to take.
    #[tokio::test]
    async fn a_remote_dial_reaches_only_a_running_hub() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec = fake_exec(dir.path());

        // NoStart: the find-only bridge, no probe.
        let (probe, asked) = FixedProbe::counted(Probe::NoHub);
        let dial = SshCrazeDial::new(
            Arc::clone(&exec),
            "BRIDGE-HUB".to_string(),
            "NO-START-HUB --no-start".to_string(),
            probe,
        );
        assert_eq!(dial.mode(), DialMode::NoStart);
        let stream = dial
            .dial()
            .await
            .expect("a no-start dial spawns its bridge");
        assert_eq!(recorded(dir.path(), 1).await, ["NO-START-HUB --no-start"]);
        assert_eq!(asked.load(Ordering::SeqCst), 0, "no probe per dial");
        drop(stream);

        // Only a no-start refusal is taken, and only once.
        for unrelated in [
            DialError::Unreachable("craze bridge: no hub: x".into()),
            DialError::TooOld("unknown flag: --hub".into()),
            DialError::NotInstalled("exit 127".into()),
            DialError::NoHub("exit 3".into()),
        ] {
            assert!(!dial.fall_back(&unrelated), "{unrelated:?}");
            assert_eq!(dial.mode(), DialMode::NoStart, "{unrelated:?}");
        }
        let refused =
            DialError::NoStartUnsupported("craze bridge: unknown or malformed flag".into());
        assert!(dial.fall_back(&refused), "the first refusal swaps the mode");
        assert_eq!(dial.mode(), DialMode::Legacy);
        assert!(
            !dial.fall_back(&refused),
            "once: Legacy has nothing to fall back to"
        );

        // Legacy: probe-gated.
        assert_legacy_gate(&exec, dir.path()).await;
        let (probe, asked) = FixedProbe::counted(Probe::Hub);
        let legacy = SshCrazeDial::new(
            Arc::clone(&exec),
            "BRIDGE-HUB".to_string(),
            "NO-START-HUB --no-start".to_string(),
            probe,
        );
        assert!(legacy.fall_back(&refused));
        let stream = legacy.dial().await.expect("a hub: its duplex");
        assert_eq!(
            recorded(dir.path(), 2).await,
            ["NO-START-HUB --no-start", "BRIDGE-HUB"],
            "the probe found a hub, then the plain bridge ran over ssh"
        );
        assert_eq!(asked.load(Ordering::SeqCst), 1);
        drop(stream);

        // The UNGATED instance asks no probe, runs the plain bridge, and never
        // falls back.
        let ungated = SshCrazeDial::ungated(Arc::clone(&exec), "ENSURE-HUB".to_string());
        let stream = ungated
            .dial()
            .await
            .expect("an ungated dial always spawns its bridge");
        assert_eq!(
            recorded(dir.path(), 3).await.last().map(String::as_str),
            Some("ENSURE-HUB"),
            "no probe, straight to the bridge"
        );
        assert!(!ungated.fall_back(&refused));
        drop(stream);
    }

    /// **A refusal of `--no-start` falls back ONCE for the whole source**
    /// (plan 032 PC12): the roster and a lane dial concurrently through the
    /// source's ONE dial — the reach `build_ssh_craze` builds, tracked as
    /// `RoostHosts::start_craze` tracks it, so the wrapper's forwarding is
    /// under test too — both draw the 0.1.x refusal, the mode swaps once, and
    /// the dial that won the swap redials the older way (the probe, then
    /// `bridge --hub`) and goes live. The other's refusal stands, and reads as
    /// `Unreachable`; its retry, and every dial after, is legacy — no
    /// `--no-start` is ever sent again by this source. A NEW source — the
    /// host registered again — builds a new dial, back on `--no-start`.
    #[tokio::test]
    async fn a_refused_no_start_falls_back_once_for_every_dial_of_the_source() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec = fake_exec(dir.path());
        // 0.1.x: a moment, then the refusal — so both dials spawn before either
        // is classified.
        std::fs::write(
            dir.path().join("no-start.sh"),
            "sleep 0.3\nprintf '%s\\n' 'craze bridge: unknown or malformed flag; see craze bridge --help' >&2\nexit 1\n",
        )
        .expect("write");
        let options = crate::machines::ReachOptions::default();
        let reach = crate::machines::build_ssh_craze(Arc::clone(&exec), &options)
            .expect("a production remote reach");
        let (dial, _gone) = tracked(Arc::clone(&reach.dial));
        let no_starts = |lines: &[String]| {
            lines
                .iter()
                .filter(|l| l.contains("bridge --hub --no-start"))
                .count()
        };

        let (roster, lane) = tokio::join!(connect(dial.as_ref()), connect(dial.as_ref()));
        let (won, lost) = match (roster, lane) {
            (Ok(won), Err(lost)) | (Err(lost), Ok(won)) => (won, lost),
            (r, l) => panic!(
                "exactly one dial wins the swap: roster {:?}, lane {:?}",
                r.err(),
                l.err()
            ),
        };
        assert!(matches!(lost, DialError::NoStartUnsupported(_)), "{lost:?}");
        assert_eq!(
            lost.cause(),
            SourceOffline::Unreachable,
            "unconsumed: as Unreachable"
        );
        let lines = recorded(dir.path(), 4).await;
        assert_eq!(no_starts(&lines), 2, "{lines:#?}");
        assert_eq!(
            lines
                .iter()
                .filter(|l| l.contains("providers --hub --json"))
                .count(),
            1,
            "the ONE fallback probed once: {lines:#?}"
        );
        assert_eq!(
            lines
                .iter()
                .filter(|l| l.contains("bridge --hub") && !l.contains("--no-start"))
                .count(),
            1,
            "and dialled the plain bridge once: {lines:#?}"
        );
        drop(won);

        // The loser's retry — and every dial after — is legacy.
        let (_conn, _notes) = connect(dial.as_ref()).await.expect("legacy: live");
        let lines = recorded(dir.path(), 6).await;
        assert_eq!(
            no_starts(&lines),
            2,
            "no --no-start after the swap: {lines:#?}"
        );

        // A new source generation: a new dial, back on --no-start.
        std::fs::remove_file(dir.path().join("no-start.sh")).expect("rm");
        let again = crate::machines::build_ssh_craze(Arc::clone(&exec), &options)
            .expect("the host registered again");
        let (dial, _gone) = tracked(Arc::clone(&again.dial));
        let stream = dial.dial().await.expect("a duplex");
        let lines = recorded(dir.path(), 7).await;
        assert_eq!(
            no_starts(&lines),
            3,
            "the new source dials --no-start: {lines:#?}"
        );
        drop(stream);
    }

    /// **Precedence holds through the real dial** (plan 032 PC12): a
    /// `--no-start` that exits 127, says v0.0.1's `unknown flag: --hub`, fails
    /// with an unrelated exit 1, or finds no hub (exit 3) is classified as
    /// such — and none of them moves the mode or redials.
    #[tokio::test]
    async fn only_a_refusal_of_the_flag_falls_back() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec = fake_exec(dir.path());
        let options = crate::machines::ReachOptions::default();
        let reach = crate::machines::build_ssh_craze(Arc::clone(&exec), &options).expect("reach");
        let (dial, _gone) = tracked(Arc::clone(&reach.dial));
        let cases: [(&str, &str); 4] = [
            ("printf '%s\\n' 'sh: 1: craze: not found' >&2; exit 127\n", "NotInstalled"),
            ("printf '%s\\n' 'Error: unknown flag: --hub' >&2; exit 1\n", "TooOld"),
            (
                "printf '%s\\n' 'craze bridge: no hub: the runtime dir is group-writable' >&2; exit 1\n",
                "Unreachable",
            ),
            ("printf '%s\\n' 'craze bridge: no hub is running' >&2; exit 3\n", "NoHub"),
        ];
        for (i, (ending, want)) in cases.iter().enumerate() {
            std::fs::write(dir.path().join("no-start.sh"), ending).expect("write");
            let err = connect(dial.as_ref()).await.err().expect("no hub here");
            assert!(format!("{err:?}").starts_with(want), "{want}: {err:?}");
            let lines = recorded(dir.path(), i + 1).await;
            assert_eq!(lines.len(), i + 1, "one dial, no fallback: {lines:#?}");
            assert!(
                lines.iter().all(|l| l.contains("bridge --hub --no-start")),
                "still --no-start: {lines:#?}"
            );
        }
        let no_hub = connect(dial.as_ref()).await.err().expect("no hub");
        assert_eq!(no_hub.cause(), SourceOffline::NoHub);
    }

    /// **A roster-listed row is folded ahead of its frame** (plan 025
    /// §3.6.4): never over a row already held; into a seed in progress, so
    /// that seed's swap does not depart it; and governed from then on by the
    /// roster's frames like any other row — this state keeps no created rows
    /// of its own (the source does: C10 confirmation).
    #[test]
    fn a_roster_listed_row_is_folded_ahead_of_its_frame() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let applied = s.listed_now(row("n"));
        assert!(applied.visible && applied.departed.is_empty());
        assert_eq!(s.held(), ["a", "n"]);

        let mut roster = row("n");
        roster.title = "the frame's".to_string();
        s.apply(SourceEvent::Session { session: roster });
        let mut stale = row("n");
        stale.title = "an earlier read".to_string();
        assert!(!s.listed_now(stale).visible, "never over a held row");
        assert_eq!(s.rows["n"].title, "the frame's");

        // A seed in progress that started before the frame keeps it on its swap.
        s.apply(SourceEvent::Reset {
            reason: "server_reset:omitted".into(),
            generation: 2,
        });
        s.apply(SourceEvent::Session { session: row("a") });
        s.listed_now(row("m"));
        let swapped = s.apply(SourceEvent::Ready {
            generation: 2,
            truncated: false,
        });
        assert_eq!(
            swapped.departed,
            ["n"],
            "m kept on that swap; n is the seed's own say (it does not list it)"
        );

        // From then on the roster's frames govern it.
        let removed = s.apply(SourceEvent::Removed {
            session_id: "m".into(),
        });
        assert_eq!(removed.departed, ["m"]);
        assert_eq!(s.held(), ["a"]);
    }

    /// Both doors' one reading of a create: an absolute directory required, a
    /// blank provider or prompt absent, a prompt otherwise sent as typed, a
    /// given request id kept (in craze's form) and an absent one minted.
    #[test]
    fn a_create_request_is_read_one_way() {
        for bad in [
            None,
            Some(""),
            Some("   "),
            Some("relative/dir"),
            Some("~/w"),
        ] {
            let e = create_request(bad, None, None, None, None).unwrap_err();
            assert_eq!(e.code, "bad_request", "{bad:?}");
        }
        let r =
            create_request(Some(" /w/x "), Some("  "), Some(" \n "), Some(" \t "), None).unwrap();
        assert_eq!(r.cwd, "/w/x");
        assert_eq!((r.provider, r.prompt), (None, None));
        assert_eq!(r.title, None, "a blank title is absent, never sent empty");
        assert!(
            shed_craze::valid_request_id(&r.request_id),
            "{}",
            r.request_id
        );
        let r = create_request(
            Some("/w"),
            Some("grok"),
            Some("two\nlines\n"),
            Some("  Fix the login bug "),
            Some("shed-abc"),
        )
        .unwrap();
        assert_eq!(r.provider.as_deref(), Some("grok"));
        assert_eq!(r.prompt.as_deref(), Some("two\nlines\n"), "as typed");
        assert_eq!(r.title.as_deref(), Some("Fix the login bug"), "trimmed");
        assert_eq!(r.request_id, "shed-abc", "a caller's id is kept");
        // A PRESENT id that is not craze's form is refused — blank and empty
        // included — never replaced by a fresh one (a retry under it would
        // then be a second session).
        for bad in [
            "not craze's!",
            "",
            "   ",
            " shed-abc",
            "x".repeat(65).as_str(),
        ] {
            let e = create_request(Some("/w"), None, None, None, Some(bad)).unwrap_err();
            assert_eq!(e.code, "bad_request", "{bad:?}");
        }
    }

    /// `held` replays an id, so it needs one; absent, a create is fresh.
    #[test]
    fn a_held_create_needs_the_request_id_it_replays() {
        assert_eq!(held_create(true, Some("shed-held")), Ok(true));
        assert_eq!(held_create(false, Some("shed-held")), Ok(false));
        assert_eq!(held_create(false, None), Ok(false));
        assert_eq!(held_create(true, None).unwrap_err().code, "bad_request");
    }

    /// The create ops' failures: the lane contract's codes, and a lost
    /// create's own `outcome_unknown` — the one the sheet keeps its id on.
    #[test]
    fn a_craze_failure_carries_the_lane_code_or_outcome_unknown() {
        use shed_core::lane::LaneError;
        let unknown = CrazeFailure::lane(shed_craze::errors::outcome_unknown("lost twice"));
        assert_eq!(unknown.code, "outcome_unknown");
        assert_eq!(
            CrazeFailure::lane(LaneError::BadRequest("no such dir".into())).code,
            "bad_request"
        );
        let failed = CrazeFailure::lane(LaneError::Failed("Error: KEYCHAIN LOCKED".into()));
        assert_eq!(failed.code, "failed");
        assert_eq!(failed.message, "Error: KEYCHAIN LOCKED");
        assert_eq!(failed.command_string(), "failed: Error: KEYCHAIN LOCKED");
        assert_eq!(
            CrazeFailure::lane(LaneError::Unavailable("busy".into())).code,
            "unavailable"
        );
    }

    /// **A reopen's failure keeps its variant as the code** (plan 032 §3.4):
    /// the resume sheet drops a row on `gone`, keeps it on `start_failed`
    /// and on an inner contract error, and offers Retry on `unavailable` — a
    /// lost answer's too, which is never `outcome_unknown` (PC14d).
    #[test]
    fn a_reopen_failure_carries_its_variant_or_the_inner_lane_code() {
        use shed_core::lane::{LaneError, LaneReopenError};
        let gone = CrazeFailure::reopen(LaneReopenError::Gone("no such session".into()));
        assert_eq!(
            (gone.code, gone.message.as_str()),
            ("gone", "no such session")
        );
        let failed = CrazeFailure::reopen(LaneReopenError::StartFailed(
            "could not start: grok exited".into(),
        ));
        assert_eq!(failed.code, "start_failed");
        assert_eq!(
            failed.command_string(),
            "start_failed: could not start: grok exited"
        );
        let lost = CrazeFailure::reopen(shed_craze::errors::reopen_lost("twice"));
        assert_eq!(lost.code, "unavailable");
        assert!(lost.message.contains("Retry is safe"), "{lost:?}");
        assert_eq!(
            CrazeFailure::reopen(LaneReopenError::Lane(LaneError::BadRequest("no".into()))).code,
            "bad_request"
        );
        assert_eq!(
            CrazeFailure::reopen(LaneReopenError::Lane(LaneError::Failed("boom".into()))).code,
            "failed"
        );
        // Even text that reads as shed's lost-create words is the inner code
        // here: a reopen holds no request id to keep.
        let worded = CrazeFailure::reopen(LaneReopenError::Lane(
            shed_craze::errors::outcome_unknown("lost"),
        ));
        assert_eq!(worded.code, "failed");
    }

    /// **A reopen names its saved session**: any non-blank id goes as given
    /// (opaque — a legacy `grok:<id>` included); a missing or blank one is
    /// `bad_request`.
    #[test]
    fn a_reopen_id_is_any_non_blank_string_as_given() {
        assert_eq!(reopen_id(Some("0192f0aa-6666")), Ok("0192f0aa-6666".into()));
        assert_eq!(reopen_id(Some("grok:abc def")), Ok("grok:abc def".into()));
        for blank in [None, Some(""), Some("   ")] {
            assert_eq!(
                reopen_id(blank).unwrap_err().code,
                "bad_request",
                "{blank:?}"
            );
        }
    }

    /// **Which states make an unlisted host a `shed_craze` entry** (plan 026
    /// §3.2): dormant and too old — the probe proved craze there — and nothing
    /// else; and the standing a repaint is decided on ignores the reason's
    /// words.
    #[test]
    fn the_states_an_unlisted_host_is_offered_on() {
        assert!(
            !CrazeState::default().proven_unattached(),
            "no craze source"
        );
        let mut s = CrazeState::new(4);
        assert!(!s.proven_unattached(), "never probed");
        dormant(&mut s, &[]);
        assert!(s.proven_unattached(), "dormant");
        s.offline(SourceOffline::TooOld, "unknown flag: --hub".into());
        assert!(s.proven_unattached(), "too old");
        let too_old = s.standing();
        s.offline(SourceOffline::TooOld, "said otherwise".into());
        assert_eq!(s.standing(), too_old, "the words are not the standing");
        for cause in [
            SourceOffline::NotInstalled,
            SourceOffline::Unreachable,
            SourceOffline::Failed,
        ] {
            s.offline(cause.clone(), "x".into());
            assert!(!s.proven_unattached(), "{cause:?}");
        }
        seed(&mut s, 1, &[]);
        assert!(!s.proven_unattached(), "live: a roster lists it");
    }

    #[test]
    fn the_status_vocabulary() {
        assert_eq!(CrazeState::default().status()["state"], "absent");
        let mut s = CrazeState::new(3);
        assert_eq!(s.status()["state"], "absent", "never reached");
        s.offline(SourceOffline::NotInstalled, "exit 127".into());
        assert_eq!(
            s.status()["state"],
            "absent",
            "not installed renders absent"
        );
        assert_eq!(s.status()["cause"], "not_installed");
        s.offline(SourceOffline::TooOld, "unknown flag: --hub".into());
        assert_eq!(s.status()["state"], "offline");
        assert_eq!(s.status()["cause"], "too_old");
        assert_eq!(s.status()["create"], false);
        seed(&mut s, 1, &[]);
        assert_eq!(
            s.status(),
            json!({"state": "live", "cause": null, "create": true, "create_options": true,
                   "create_title": true, "source_gen": 3})
        );
    }

    /// **Every status names the source's incarnation** (C5 re-review):
    /// `source_gen` is the source's generation in every state — the sheet
    /// resets its per-machine Resume state on a new one without reading the
    /// create options — and `null` for a host with no craze source at all.
    #[test]
    fn every_status_names_the_sources_generation() {
        assert_eq!(
            CrazeState::default().status()["source_gen"],
            Value::Null,
            "no craze source: no incarnation to name"
        );
        let mut s = CrazeState::new(7);
        assert_eq!(s.status()["source_gen"], 7, "never reached");
        dormant(&mut s, &[]);
        assert_eq!(s.status()["source_gen"], 7, "dormant");
        s.offline(SourceOffline::Unreachable, "no route".into());
        assert_eq!(s.status()["source_gen"], 7, "offline");
        seed(&mut s, 1, &[]);
        assert_eq!(s.status()["source_gen"], 7, "live");
        assert_eq!(
            CrazeState::new(8).status()["source_gen"],
            8,
            "another source, another generation"
        );
    }
}
