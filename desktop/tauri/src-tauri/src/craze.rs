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
//!   | **Dormant** | the probe said `no hub is running` | the probe again in [`DORMANT_PROBE`] | a probe that finds a hub |
//!   | **Live** | the probe found a hub, then the roster seeded | ONE `bridge --hub` roster connection | the roster's `Offline` (EOF, `hub_closing`, an error) |
//!   | **Offline** | the roster dropped, or the probe could not ask (unreachable; not installed and too old are offline causes too) | the probe again: after about a second once a live cycle ends (jittered [`PROBE_BACKOFF_BASE`]), on a backoff while unreachable, every [`DORMANT_PROBE`] while not installed or too old | the probe's next answer |
//!
//!   After a live cycle ends the probe runs again before anything redials: a
//!   hub that is gone sends the source back to Dormant (its rows dropped and
//!   its lanes evicted — the hub is gone, so nothing is listed), and a hub that
//!   is still there is re-joined.
//!
//! * **Every remote `bridge --hub` is probe-gated** ([`SshCrazeDial`]): the
//!   dialer itself runs the find-only probe on the same ControlMaster before it
//!   spawns the bridge, and answers `Unreachable` instead when no hub is
//!   running. That covers the roster's dials AND every lane's background
//!   reconnects (a lane keeps `Stale`, within its own 10-minute bound, rather
//!   than birthing a hub), and no caller can bypass it because the rule is in
//!   the dial. A lane's FIRST open runs the probe too — harmless, its row came
//!   from a live roster.
//! * **One race is ACCEPTED, not closed** (plan 025 §3.6.1, recorded again by
//!   the C9 review): the probe sees a hub that exits before the `bridge --hub`
//!   behind it joins, and the bridge — which always Ensures a hub — births one,
//!   ssh-born, with no user action. The probe-then-bridge pair cannot be made
//!   atomic from this side; craze SF-152 (a find-only `bridge --hub`, reserved
//!   2026-10-04) removes the race and the probe both. Until then the window is
//!   the gap between two execs on one ControlMaster.
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
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};

use shed_app::roost::SshExec;
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
    /// a shed) — and its dial is probe-gated too.
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
// the ssh probe and the probe-gated ssh dial
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

/// A remote host's dial: **probe-gated** `craze bridge --hub` over
/// [`SshExec::spawn_duplex`] (the module doc). Every connection — the roster's
/// and every lane's — asks the find-only probe first, and a host with no hub
/// running is `Unreachable`, never a hub this app births in the background.
///
/// The one UNGATED instance ([`Self::ungated`]) is the host's
/// [`CrazeReach::ensure`]: only a user's explicit action dials it.
pub struct SshCrazeDial {
    exec: Arc<SshExec>,
    /// `display_line` of the bridge form (production, or jailed in test mode).
    bridge: String,
    /// `None` only for [`Self::ungated`].
    probe: Option<Arc<dyn CrazeProbe>>,
}

impl SshCrazeDial {
    pub fn new(exec: Arc<SshExec>, bridge: String, probe: Arc<dyn CrazeProbe>) -> Self {
        Self {
            exec,
            bridge,
            probe: Some(probe),
        }
    }

    /// The same bridge with NO probe in front: it Ensures a hub on the far side
    /// whether or not one runs. For [`CrazeReach::ensure`] only — the create
    /// sheet's `create_options` and the create, a user's explicit action.
    pub fn ungated(exec: Arc<SshExec>, bridge: String) -> Self {
        Self {
            exec,
            bridge,
            probe: None,
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
        let bridge = self.bridge.clone();
        let probe = self.probe.as_ref().map(|p| p.probe());
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
            let child = exec.spawn_duplex(&bridge).map_err(|e| {
                DialError::Unreachable(format!("{}: could not run ssh: {e}", exec.label()))
            })?;
            child_stream(child)
        })
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
}

/// What one fold changed, for the caller to act on outside the lock.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Applied {
    /// Rows that left the swapped-in set — their lanes go (`Removed`, or a
    /// `Ready` swap that dropped them: an epoch reseed produces no `Removed`).
    pub departed: Vec<String>,
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
                    self.rows.insert(session.id.clone(), session);
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
    pub fn dormant(&mut self) -> Applied {
        let departed = self.held();
        let visible = !departed.is_empty() || self.phase != Some(CrazePhase::Dormant);
        self.rows.clear();
        self.staged = None;
        self.caps = None;
        self.truncated = false;
        self.phase = Some(CrazePhase::Dormant);
        self.offline = None;
        Applied {
            departed,
            ready: false,
            visible,
        }
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
            ready: false,
            visible,
        }
    }

    /// The status row's `craze` half (plan 025 §3.6.2): `state` is `live`,
    /// `dormant`, `offline` or `absent` (never reached — and not installed,
    /// which renders as absent); `cause` names an offline (or not-installed)
    /// state's reason class; `create`/`create_options` are the LIVE hub's
    /// capabilities, `false` otherwise.
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
///   ([`shed_craze::is_outcome_unknown`]): the ONE refusal after which the
///   sheet keeps its request id (plan 025 §3.8);
/// * `too_old` — this machine's craze cannot list providers or create
///   (`createOptions`/`sessionCreate` missing from its hub's `hello`, or a
///   v0.0.1): the sheet says "update craze on this machine";
/// * `not_installed` / `no_craze` — no craze there, or no craze source for
///   the host at all;
/// * `bad_request` — a caller's mistake (an unknown host, a request id not in
///   craze's form, a session this machine does not list, a host id that is not
///   craze's form) as well as craze's own `bad_request`.
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
    Ok(shed_core::lane::LaneCreateRequest {
        cwd: cwd.to_string(),
        provider: provider
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_string),
        prompt: prompt.filter(|s| !s.trim().is_empty()).map(str::to_string),
        request_id,
    })
}

// ---------------------------------------------------------------------------
// the loop
// ---------------------------------------------------------------------------

/// Where a host's [`run`] task reports — implemented by the roost-host layer,
/// which owns the lock the state lives under. Every method answers `false`
/// once the host this task was started for is gone (removed, or re-registered
/// under a newer fence): the task then ends.
pub trait CrazeSink: Send + Sync {
    fn event(&self, event: SourceEvent) -> bool;
    fn dormant(&self) -> bool;
    fn offline(&self, cause: SourceOffline, reason: String) -> bool;
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
        match probe.probe().await {
            Probe::Hub => {
                let Ok(subscription) = source.subscribe().await else {
                    return;
                };
                let (mut rx, stop) = subscription.into_parts();
                let mut reached = false;
                loop {
                    let Some(event) = rx.recv().await else {
                        return;
                    };
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
                backoff = next_backoff(backoff, reached, t.backoff_base, t.backoff_max);
                wait(
                    jittered(if reached { t.backoff_base } else { backoff }),
                    &wake,
                )
                .await;
            }
            Probe::NoHub => {
                if !sink.dormant() {
                    return;
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
        let dormant = s.dormant();
        assert_eq!(dormant.departed, ["a"]);
        assert!(s.rows.is_empty());
        assert_eq!(s.status()["state"], "dormant");
    }

    /// A probe with one fixed answer.
    struct FixedProbe(Probe);

    impl CrazeProbe for FixedProbe {
        fn probe(&self) -> BoxFut<Probe> {
            let answer = self.0.clone();
            Box::pin(async move { answer })
        }
    }

    /// **A remote dial is probe-gated** (plan 025 §3.6.1): with no hub running
    /// it answers `Unreachable` and never spawns `bridge --hub` over ssh — so
    /// no caller (the roster's redial, a lane's background reconnect) can birth
    /// a remote hub through it; not installed and too old keep their causes; a
    /// hub that is there gets its duplex.
    #[tokio::test]
    async fn a_remote_dial_spawns_no_bridge_unless_the_probe_found_a_hub() {
        use std::os::unix::fs::PermissionsExt as _;

        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("ssh.log");
        let ssh = dir.path().join("ssh");
        // A fake ssh: records its remote command (the last argument), then
        // runs as a duplex (`cat`) until its stdin closes.
        std::fs::write(
            &ssh,
            format!(
                "#!/bin/sh\nfor a in \"$@\"; do last=\"$a\"; done\nprintf '%s\\n' \"$last\" >> '{}'\nexec cat\n",
                log.display()
            ),
        )
        .expect("write");
        std::fs::set_permissions(&ssh, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        let entry = shed_core::config::MachineEntry {
            name: "mini3".to_string(),
            host: "mini3".to_string(),
            ssh_port: 22,
            ..Default::default()
        };
        let options = shed_app::roost::SshBridgeOptions {
            ssh_bin: Some(ssh),
            ..Default::default()
        };
        let exec = Arc::new(SshExec::new(&entry, &options).expect("exec"));
        let dial = |probe: Probe| {
            SshCrazeDial::new(
                Arc::clone(&exec),
                "BRIDGE-HUB".to_string(),
                Arc::new(FixedProbe(probe)),
            )
        };

        let err = dial(Probe::NoHub).dial().await.err().expect("no hub");
        assert!(
            matches!(&err, DialError::Unreachable(m) if m.contains("no craze hub is running")),
            "{err:?}"
        );
        for (cause, want) in [
            (SourceOffline::NotInstalled, "NotInstalled"),
            (SourceOffline::TooOld, "TooOld"),
            (SourceOffline::Unreachable, "Unreachable"),
        ] {
            let err = dial(Probe::Offline {
                cause,
                reason: "said".to_string(),
            })
            .dial()
            .await
            .err()
            .expect("offline");
            assert!(format!("{err:?}").starts_with(want), "{err:?}");
        }
        assert!(
            !log.exists(),
            "no ssh ran while the probe said there was nothing to join"
        );

        let stream = dial(Probe::Hub).dial().await.expect("a hub: its duplex");
        // The child is spawned when the dial answers; it records a moment later.
        let mut recorded = String::new();
        for _ in 0..500 {
            recorded = std::fs::read_to_string(&log).unwrap_or_default();
            if !recorded.is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(recorded.trim(), "BRIDGE-HUB", "the bridge ran over ssh");
        drop(stream);

        // The UNGATED instance — a user's explicit action, `CrazeReach::ensure`
        // — asks no probe: it runs the bridge, which Ensures a hub.
        std::fs::remove_file(&log).expect("rm log");
        let stream = SshCrazeDial::ungated(Arc::clone(&exec), "ENSURE-HUB".to_string())
            .dial()
            .await
            .expect("an ungated dial always spawns its bridge");
        let mut recorded = String::new();
        for _ in 0..500 {
            recorded = std::fs::read_to_string(&log).unwrap_or_default();
            if !recorded.is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(
            recorded.trim(),
            "ENSURE-HUB",
            "no probe, straight to the bridge"
        );
        drop(stream);
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
            let e = create_request(bad, None, None, None).unwrap_err();
            assert_eq!(e.code, "bad_request", "{bad:?}");
        }
        let r = create_request(Some(" /w/x "), Some("  "), Some(" \n "), None).unwrap();
        assert_eq!(r.cwd, "/w/x");
        assert_eq!((r.provider, r.prompt), (None, None));
        assert!(
            shed_craze::valid_request_id(&r.request_id),
            "{}",
            r.request_id
        );
        let r = create_request(
            Some("/w"),
            Some("grok"),
            Some("two\nlines\n"),
            Some("shed-abc"),
        )
        .unwrap();
        assert_eq!(r.provider.as_deref(), Some("grok"));
        assert_eq!(r.prompt.as_deref(), Some("two\nlines\n"), "as typed");
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
            let e = create_request(Some("/w"), None, None, Some(bad)).unwrap_err();
            assert_eq!(e.code, "bad_request", "{bad:?}");
        }
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
            json!({"state": "live", "cause": null, "create": true, "create_options": true})
        );
    }
}
