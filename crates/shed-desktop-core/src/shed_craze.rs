//! **A craze source per shed** (plan 025 §3.6.1–§3.6.2, plan 038 D2) — the
//! desktop half of [`shed_craze::CrazeSource`]: how a shed-only desktop client
//! reaches each running shed's craze hub over SSH, the staged state it folds
//! the hub's roster into, and the attach-only loop that keeps it current.
//!
//! Every shed is remote and **attach-only**:
//!
//! * No background dial (`SshCrazeDial` in [`DialMode::NoStart`], or
//!   probe-gated [`DialMode::Legacy`] for craze < 0.2.0) ever starts a remote
//!   hub.
//! * Only an explicit user action (`craze.create_options`, `craze.create`,
//!   `craze.saved`, `craze.reopen`) dials the ungated [`CrazeReach::ensure`]
//!   command (`bridge --hub`), which may start a hub on the shed.

use std::collections::{BTreeMap, HashMap};
use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};

use shed_craze::{
    classify_probe, CrazeDial, CrazeSource, CrazeStream, DialError, ExitWatch, Probe, StderrTail,
};
use shed_lane::lane::{
    AgentSource as _, LaneCreateRequest, LaneError, LaneReopenError, LaneSession,
    SourceCapabilities, SourceEvent, SourceOffline,
};
use shed_lane::shell_quote::display_line;
use shed_roost::bootstrap::{Outcome, Stdin};
#[cfg(test)]
use shed_roost::{ChildExit, SpawnSection};
use shed_roost::{
    DuplexChild, LocalSession, RoostReach, ShuttingDown, SpawnGate, SshBridge, SshBridgeOptions,
    SshEndpoint, SshExec, TeardownSink, UnreachableReach,
};

use crate::shed_lanes::LaneFailure;

/// A boxed, `'static` future matching [`CrazeDial::dial`].
pub type BoxFut<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

/// How long a dormant remote source waits between find-only probes — and the
/// slow retry while craze there is not installed or too old.
const DORMANT_PROBE: Duration = Duration::from_secs(30);
/// The probe's backoff floor while the shed is unreachable, and the pause
/// after a live roster cycle ends before the probe asks again.
const PROBE_BACKOFF_BASE: Duration = Duration::from_secs(1);
/// The probe's backoff ceiling while the shed is unreachable.
const PROBE_BACKOFF_MAX: Duration = Duration::from_secs(30);
/// How much of the probe's stdout is kept: `createOptions` is a few KiB.
const PROBE_STDOUT_CAP: usize = 1 << 20;

/// Copy for a craze installation too old for create/options.
pub const CRAZE_TOO_OLD: &str = "update craze on this shed to start sessions from shed";

/// Transport configuration shared by roost and craze on every shed.
#[derive(Clone, Default)]
pub struct ShedReachOptions {
    /// `SHED_TAURI_ROOST_SOCKETS`: per-shed `roost-session` sockets keyed by
    /// `roost:<server>/<shed>`, reached directly instead of through roost's
    /// SSH client-bridge.
    pub roost_sockets: HashMap<String, PathBuf>,
    /// `SHED_TAURI_SSH_BIN`: the `ssh` binary a test-mode run execs. `None` in
    /// production, where `ssh` on `PATH` is used.
    pub ssh_bin: Option<PathBuf>,
    /// Whether test mode is active. In test mode a shed is reached ONLY
    /// through `roost_sockets` or `ssh_bin`; anything else is unreachable so
    /// hermetic runs never spawn system `ssh`.
    pub test_mode: bool,
    /// The app's spawn gate: every transport built from these options spawns
    /// only inside its sections.
    pub spawn_gate: Option<SpawnGate>,
    /// Shared roost-tunnel teardown sink so teardowns from removed sheds are
    /// joined by orderly shutdown.
    pub teardowns: Option<TeardownSink>,
}

impl ShedReachOptions {
    /// Whether these options allow spawning SSH (`false` in `test_mode` when
    /// no `ssh_bin` override is configured, so hermetic runs never spawn
    /// system `ssh`).
    pub fn allows_ssh(&self) -> bool {
        !self.test_mode || self.ssh_bin.is_some()
    }

    /// The bridge options shared by [`SshBridge`] and [`SshExec`].
    pub fn bridge_options(&self) -> SshBridgeOptions {
        SshBridgeOptions {
            ssh_bin: self.ssh_bin.clone(),
            spawn_gate: self.spawn_gate.clone(),
            teardowns: self.teardowns.clone(),
            ..SshBridgeOptions::default()
        }
    }
}

/// Build the roost transport for one shed (`roost:<server>/<shed>`).
pub fn build_ssh_reach(
    entry: &SshEndpoint,
    options: &ShedReachOptions,
) -> Result<Arc<dyn RoostReach>, String> {
    if let Some(socket) = options.roost_sockets.get(&entry.name) {
        return Ok(Arc::new(LocalSession::new(&entry.name, socket.clone())));
    }
    if !options.allows_ssh() {
        return Ok(Arc::new(UnreachableReach::new(
            &entry.name,
            "no roost-session mapped for this shed in test mode",
        )));
    }
    SshBridge::new(entry, options.bridge_options())
        .map(|b| Arc::new(b) as Arc<dyn RoostReach>)
        .map_err(|e| e.to_string())
}

/// Build the attach-only craze reach for one shed over its shared [`SshExec`].
///
/// In test mode this exists only through the fake-ssh seam (`ssh_bin`) and
/// composes the jailed ladder so no system craze on the test host can answer.
pub fn build_ssh_craze(exec: Arc<SshExec>, options: &ShedReachOptions) -> Option<CrazeReach> {
    let (bridge, no_start, probe) = if options.test_mode {
        options.ssh_bin.as_ref()?;
        (
            display_line(&shed_lane::craze::bridge_hub_argv_jailed()),
            display_line(&shed_lane::craze::bridge_hub_no_start_argv_jailed()),
            display_line(&shed_lane::craze::providers_hub_argv_jailed()),
        )
    } else {
        (
            shed_lane::craze::bridge_hub_command(),
            shed_lane::craze::bridge_hub_no_start_command(),
            display_line(&shed_lane::craze::providers_hub_argv()),
        )
    };
    let probe: Arc<dyn CrazeProbe> = Arc::new(SshCrazeProbe::new(Arc::clone(&exec), probe));
    let gate = options.spawn_gate.clone();
    Some(CrazeReach {
        dial: Arc::new(
            SshCrazeDial::new(
                Arc::clone(&exec),
                bridge.clone(),
                no_start,
                Arc::clone(&probe),
            )
            .with_spawn_gate(gate.clone()),
        ),
        probe: Some(probe),
        ensure: Some(Arc::new(
            SshCrazeDial::ungated(exec, bridge).with_spawn_gate(gate),
        )),
    })
}

/// A remote shed's find-only probe (`craze providers --hub --json`, which
/// never starts a hub).
pub trait CrazeProbe: Send + Sync {
    fn probe(&self) -> BoxFut<Probe>;
}

/// How one shed's craze is reached: the attach-only dial every background
/// connection goes through, the find-only probe, and the ungated dial for
/// explicit user actions.
#[derive(Clone)]
pub struct CrazeReach {
    pub dial: Arc<dyn CrazeDial>,
    /// `Some` in production (attach-only). Unit tests with in-process scripted
    /// hubs may set `None` to subscribe directly without a probe.
    pub probe: Option<Arc<dyn CrazeProbe>>,
    /// The dial an explicit user action (`craze.create_options`,
    /// `craze.create`, `craze.saved`, `craze.reopen`) runs through, which may
    /// start a hub on the shed. `None` falls back to [`Self::dial`].
    pub ensure: Option<Arc<dyn CrazeDial>>,
}

/// The source's clocks — the defaults are the pinned ones; tests shorten them.
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

/// The find-only probe over a shed's `SshExec` — a one-shot `run` on the
/// shared ControlMaster, classified by its stderr text.
pub struct SshCrazeProbe {
    exec: Arc<SshExec>,
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
                Outcome::Exec { stderr_tail, .. } => Probe::Offline {
                    cause: SourceOffline::Unreachable,
                    reason: stderr_tail,
                },
                _ => Probe::Offline {
                    cause: SourceOffline::Unreachable,
                    reason: "the craze probe did not answer as an exec".to_string(),
                },
            }
        })
    }
}

/// How an attach-only [`SshCrazeDial`] reaches its hub (plan 032 §3.5) — one
/// mode per dial, shared by the shed's roster and every lane it opens.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum DialMode {
    /// craze 0.2.0+: `bridge --hub --no-start`, no probe in front.
    NoStart = 0,
    /// Fallen back for craze < 0.2.0: find-only probe, then `bridge --hub`.
    Legacy = 1,
}

/// A remote shed's dial over [`SshExec::spawn_duplex`].
pub struct SshCrazeDial {
    exec: Arc<SshExec>,
    bridge: String,
    no_start: Option<String>,
    probe: Option<Arc<dyn CrazeProbe>>,
    mode: AtomicU8,
    gate: Option<SpawnGate>,
}

impl SshCrazeDial {
    /// An attach-only dial: `no_start` first, falling back once per source to
    /// `probe` + `bridge` for a craze older than 0.2.0.
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

    /// The bridge with no probe in front and no `--no-start` (may birth a hub).
    /// Used exclusively by explicit user actions ([`CrazeReach::ensure`]).
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
fn no_hub(label: &str) -> DialError {
    DialError::Unreachable(format!(
        "no craze hub is running on {label}; shed starts one there only when you ask it to"
    ))
}

impl CrazeDial for SshCrazeDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        let exec = Arc::clone(&self.exec);
        let gate = self.gate.clone();
        if exec.is_closed() || gate.as_ref().is_some_and(SpawnGate::is_closed) {
            return Box::pin(async { Err(shutting_down_dial()) });
        }
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
            let duplex = exec
                .spawn_duplex_with_gate(&bridge, gate.as_ref())
                .await
                .map_err(|e| {
                    DialError::Unreachable(format!("{}: could not run ssh: {e}", exec.label()))
                })?;
            Ok(duplex_stream(duplex))
        })
    }

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
            tracing::info!(
                target = %self.exec.label(),
                error = %err,
                "craze does not know `bridge --hub --no-start` (older than 0.2.0): \
                 falling back to probe before `bridge --hub`"
            );
        }
        swapped
    }
}

// ---------------------------------------------------------------------------
// the spawn gate (plan 026 §3.3)
// ---------------------------------------------------------------------------

/// What a gated dial answers once the app's spawn gate has closed.
fn shutting_down_dial() -> DialError {
    DialError::Unreachable(ShuttingDown.to_string())
}

/// A dial whose every spawn runs inside a section of the app's spawn gate.
#[cfg(test)]
pub(crate) struct GatedDial {
    inner: Arc<dyn CrazeDial>,
    gate: SpawnGate,
    pause: std::sync::Mutex<Option<crate::shed_hosts::TestGap>>,
}

/// `inner`, gated by `gate` when there is one; `inner` itself otherwise.
#[cfg(test)]
pub(crate) fn gated(inner: Arc<dyn CrazeDial>, gate: Option<&SpawnGate>) -> Arc<dyn CrazeDial> {
    match gate {
        Some(gate) => Arc::new(GatedDial::new(inner, gate.clone())),
        None => inner,
    }
}

#[cfg(test)]
impl GatedDial {
    pub(crate) fn new(inner: Arc<dyn CrazeDial>, gate: SpawnGate) -> GatedDial {
        GatedDial {
            inner,
            gate,
            pause: std::sync::Mutex::new(None),
        }
    }

    pub(crate) fn pause_next(&self, gap: crate::shed_hosts::TestGap) {
        *self.pause.lock().unwrap() = Some(gap);
    }
}

#[cfg(test)]
impl CrazeDial for GatedDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        let inner = Arc::clone(&self.inner);
        let gate = self.gate.clone();
        let pause = self.pause.lock().unwrap().take();
        Box::pin(async move {
            let section = gate.enter().map_err(|_| shutting_down_dial())?;
            if let Some((reached, proceed)) = pause {
                reached.notify_one();
                proceed.notified().await;
            }
            let stream = inner.dial().await?;
            register_stream(&[(&gate, &section)], stream)
                .await
                .map_err(|_| shutting_down_dial())
        })
    }

    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// Register `stream`'s child ([`ExitWatch`]) with every `(gate, section)` pair.
#[cfg(test)]
async fn register_stream(
    gates: &[(&SpawnGate, &SpawnSection)],
    stream: CrazeStream,
) -> Result<CrazeStream, ShuttingDown> {
    let (reader, writer, tail, exit) = stream.into_parts();
    let Some(exit) = exit else {
        return Ok(CrazeStream::from_parts(reader, writer, tail, None));
    };
    for (gate, section) in gates {
        gate.register_exit(section, Arc::new(ExitKill(exit.clone())))
            .await?;
    }
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

#[cfg(test)]
struct ExitKill(ExitWatch);

#[cfg(test)]
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

/// A craze source's dial wrapped so its last holder letting go is observable.
struct TrackedDial {
    inner: Arc<dyn CrazeDial>,
    _gone: tokio::sync::oneshot::Sender<()>,
}

impl CrazeDial for TrackedDial {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        self.inner.dial()
    }

    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// `inner`, tracked: returns the dial and a receiver that resolves once the
/// last clone of that dial has been dropped.
pub(crate) fn tracked(
    inner: Arc<dyn CrazeDial>,
) -> (Arc<dyn CrazeDial>, tokio::sync::oneshot::Receiver<()>) {
    let (tx, rx) = tokio::sync::oneshot::channel();
    (Arc::new(TrackedDial { inner, _gone: tx }), rx)
}

struct Tethered {
    inner: Arc<dyn CrazeDial>,
    _tether: Arc<dyn CrazeDial>,
}

impl CrazeDial for Tethered {
    fn dial(&self) -> BoxFut<Result<CrazeStream, DialError>> {
        self.inner.dial()
    }

    fn fall_back(&self, err: &DialError) -> bool {
        self.inner.fall_back(err)
    }
}

/// `inner`, tethered to `tether` so teardown waits for calls in flight on
/// `inner` too.
pub(crate) fn tethered(
    inner: Arc<dyn CrazeDial>,
    tether: &Arc<dyn CrazeDial>,
) -> Arc<dyn CrazeDial> {
    Arc::new(Tethered {
        inner,
        _tether: Arc::clone(tether),
    })
}

/// Wrap a runner-tracked [`DuplexChild`] as a [`CrazeStream`].
fn duplex_stream(duplex: DuplexChild) -> CrazeStream {
    let pid = duplex.exit.pid();
    let mut code_rx = duplex.exit.subscribe();
    let (status_tx, status_rx) = tokio::sync::watch::channel(
        (*code_rx.borrow()).map(|code| shed_craze::dial::Exit { code }),
    );
    tokio::spawn(async move {
        if status_tx.borrow().is_some() {
            return;
        }
        while code_rx.changed().await.is_ok() {
            if let Some(code) = *code_rx.borrow() {
                let _ = status_tx.send(Some(shed_craze::dial::Exit { code }));
                return;
            }
        }
    });
    let exit_handle = duplex.exit;
    let exit = ExitWatch::from_parts(pid, status_rx, move || {
        exit_handle.kill();
    });
    CrazeStream::from_parts(
        Box::new(duplex.stdout),
        Box::new(RegisteredWrite {
            inner: Box::new(duplex.stdin),
            exit: exit.clone(),
        }),
        Some(StderrTail::spawn(duplex.stderr)),
        Some(exit),
    )
}

/// Wrap a spawned bridge child as a [`CrazeStream`].
#[cfg(test)]
pub(crate) fn child_stream(mut child: tokio::process::Child) -> Result<CrazeStream, DialError> {
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

/// Where a shed's craze source is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CrazePhase {
    /// craze is installed and no hub is running.
    Dormant,
    /// The roster seeded and is following the hub.
    Live,
    /// The roster dropped, or craze could not be asked; see the cause.
    Offline,
}

#[derive(Debug, Default)]
struct Staged {
    generation: u64,
    rows: BTreeMap<String, LaneSession>,
    caps: Option<SourceCapabilities>,
}

/// One shed's craze roster, staged and generation-fenced.
#[derive(Debug, Default)]
pub struct CrazeState {
    /// The registration fence: the generation of the craze run task that may
    /// write here. `0` means no craze source is installed.
    pub gen: u64,
    /// The swapped-in rows, by hostId — live while `Live`, retained (and
    /// rendered stale) while `Offline`, empty while `Dormant`.
    pub rows: BTreeMap<String, LaneSession>,
    pub phase: Option<CrazePhase>,
    pub offline: Option<(SourceOffline, String)>,
    pub caps: Option<SourceCapabilities>,
    pub truncated: bool,
    staged: Option<Staged>,
    news: u64,
    listings: u64,
}

/// What one fold changed, for the caller to act on outside the lock.
#[derive(Debug, Default, PartialEq, Eq)]
pub(crate) struct Applied {
    pub departed: Vec<String>,
    pub listed: Vec<String>,
    pub ready: bool,
    pub visible: bool,
}

impl CrazeState {
    pub fn new(gen: u64) -> Self {
        Self {
            gen,
            ..Self::default()
        }
    }

    /// Mint the next strictly increasing craze news sequence number for this
    /// source generation (never 0). Called under `ShedHosts::state`.
    pub(crate) fn mint_news(&mut self) -> u64 {
        self.news += 1;
        self.news
    }

    /// Current listing epoch: bumped each time a create or reopen lists a row.
    pub(crate) fn listings(&self) -> u64 {
        self.listings
    }

    /// Record that a create or reopen answered with a listed row: advances the
    /// listing epoch and mints the return news number under the same lock.
    pub(crate) fn answer_listed(&mut self) -> u64 {
        self.listings += 1;
        self.mint_news()
    }

    /// Whether the hub feed is live.
    pub fn live(&self) -> bool {
        self.phase == Some(CrazePhase::Live)
    }

    /// Every hostId this shed currently holds a row for.
    fn held(&self) -> Vec<String> {
        self.rows.keys().cloned().collect()
    }

    /// Fold one source event (stage-and-swap).
    pub(crate) fn apply(&mut self, event: SourceEvent) -> Applied {
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
                self.staged = None;
                self.phase = Some(CrazePhase::Offline);
                self.offline = Some((cause, reason));
                out.visible = true;
            }
            SourceEvent::Unknown => {}
        }
        out
    }

    /// Fold a no-hub answer if no create/reopen listed a row since `probed_at`.
    pub(crate) fn dormant(&mut self, created: &[LaneSession], probed_at: u64) -> Option<Applied> {
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

    /// Fold an offline probe result: retained rows stay, rendered stale.
    pub(crate) fn offline(&mut self, cause: SourceOffline, reason: String) -> Applied {
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

    /// Fold a roster-listed row ahead of its own `Session` frame.
    pub(crate) fn listed_now(&mut self, session: LaneSession) -> Applied {
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

    /// Coarse standing used to decide whether an unlisted shed's `shed_craze`
    /// offer changed.
    pub(crate) fn standing(&self) -> (Option<CrazePhase>, Option<SourceOffline>) {
        (
            self.phase,
            self.offline.as_ref().map(|(cause, _)| cause.clone()),
        )
    }

    /// Whether the probe has proved craze is present without a live roster
    /// (`Dormant`, or `Offline(TooOld)`).
    pub fn proven_unattached(&self) -> bool {
        match (self.gen, self.phase, &self.offline) {
            (0, ..) => false,
            (_, Some(CrazePhase::Dormant), _) => true,
            (_, Some(CrazePhase::Offline), Some((SourceOffline::TooOld, _))) => true,
            _ => false,
        }
    }

    /// The status row's `craze` object.
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
// craze op failures & request parsing
// ---------------------------------------------------------------------------

/// A refusal of a `craze.*` operation (`craze.create_options`, `craze.create`,
/// `craze.saved`, `craze.reopen`, `craze.open_terminal`).
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

    pub fn lane(e: LaneError) -> Self {
        if shed_craze::is_outcome_unknown(&e) {
            return Self::new("outcome_unknown", e.to_string());
        }
        let message = e.to_string();
        Self::new(LaneFailure::Lane(e).code(), message)
    }

    pub fn reopen(e: LaneReopenError) -> Self {
        let message = e.to_string();
        match e {
            LaneReopenError::Gone(_) => Self::new("gone", message),
            LaneReopenError::StartFailed(_) => Self::new("start_failed", message),
            LaneReopenError::Unavailable(_) => Self::new("unavailable", message),
            LaneReopenError::Lane(e) => Self::new(LaneFailure::Lane(e).code(), message),
        }
    }

    pub fn command_string(&self) -> String {
        format!("{}: {}", self.code, self.message)
    }
}

/// Parse and validate a `craze.create` request from caller-supplied fields.
pub fn create_request(
    cwd: Option<&str>,
    provider: Option<&str>,
    prompt: Option<&str>,
    title: Option<&str>,
    request_id: Option<&str>,
) -> Result<LaneCreateRequest, CrazeFailure> {
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
    Ok(LaneCreateRequest {
        cwd: cwd.to_string(),
        provider: trimmed(provider),
        prompt: prompt.filter(|s| !s.trim().is_empty()).map(str::to_string),
        request_id,
        title: trimmed(title),
    })
}

/// Validate `held` flag on `craze.create`.
pub fn held_create(held: bool, request_id: Option<&str>) -> Result<bool, CrazeFailure> {
    if held && request_id.is_none() {
        return Err(CrazeFailure::bad_request(
            "a held create replays the request id it was held under: send `request_id` with `held`",
        ));
    }
    Ok(held)
}

/// Validate the opaque saved session id for `craze.reopen`.
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

/// Where a shed's craze run task reports its roster and probe updates.
pub(crate) trait CrazeSink: Send + Sync {
    fn event(&self, event: SourceEvent) -> bool;
    fn listings(&self) -> u64;
    fn dormant(&self, probed_at: u64) -> Dormancy;
    fn offline(&self, cause: SourceOffline, reason: String) -> bool;
}

/// What [`CrazeSink::dormant`] made of a no-hub answer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Dormancy {
    Folded,
    Stale,
    Gone,
}

/// Run one shed's craze source loop until the shed is removed or the task is
/// aborted.
pub(crate) async fn run(
    source: CrazeSource,
    probe: Option<Arc<dyn CrazeProbe>>,
    sink: Arc<dyn CrazeSink>,
    timings: CrazeTimings,
    wake: Arc<tokio::sync::Notify>,
) {
    match probe {
        None => direct(source, sink).await,
        Some(probe) => attach_only(source, probe, sink, timings, wake).await,
    }
}

async fn wait(d: Duration, wake: &tokio::sync::Notify) {
    tokio::select! {
        _ = tokio::time::sleep(d) => {}
        _ = wake.notified() => {}
    }
}

/// Direct subscription loop (used by unit tests with in-process scripted hubs).
async fn direct(source: CrazeSource, sink: Arc<dyn CrazeSink>) {
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

/// Remote attach-only loop: probe, attach only to a running hub, and re-probe
/// whenever the roster drops.
async fn attach_only(
    source: CrazeSource,
    probe: Arc<dyn CrazeProbe>,
    sink: Arc<dyn CrazeSink>,
    t: CrazeTimings,
    wake: Arc<tokio::sync::Notify>,
) {
    let mut backoff = t.backoff_base;
    loop {
        let probed_at = sink.listings();
        match probe.probe().await {
            Probe::Hub => {
                let dialled_at = sink.listings();
                let Ok(subscription) = source.subscribe().await else {
                    return;
                };
                let (mut rx, stop) = subscription.into_parts();
                let mut reached = false;
                let mut hub_absent = false;
                loop {
                    let Some(event) = rx.recv().await else {
                        return;
                    };
                    if let SourceEvent::Offline {
                        cause: SourceOffline::NoHub,
                        ..
                    } = &event
                    {
                        hub_absent = true;
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
                drop(stop);
                drop(rx);
                if hub_absent {
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

fn next_backoff(current: Duration, reached: bool, base: Duration, max: Duration) -> Duration {
    if reached {
        base
    } else {
        current.saturating_mul(2).min(max)
    }
}

fn jittered(d: Duration) -> Duration {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |t| t.subsec_nanos());
    let quarter = d.as_nanos() / 4;
    if quarter == 0 {
        return d;
    }
    let offset = (u128::from(nanos) % quarter).saturating_sub(quarter / 2);
    let adjusted = d.as_nanos().saturating_add(offset).max(1);
    Duration::from_nanos(adjusted.min(u128::from(u64::MAX)) as u64)
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_lane::rc::RcActivity;

    fn row(id: &str) -> LaneSession {
        LaneSession {
            id: id.to_string(),
            title: id.to_string(),
            cwd: "/home/shed".to_string(),
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
        assert!(!wrong.ready && s.rows.is_empty());
        let applied = s.apply(SourceEvent::Ready {
            generation: 1,
            truncated: false,
        });
        assert!(applied.ready);
        assert_eq!(s.held(), ["a"]);
        assert!(s.live());
        assert_eq!(s.status()["state"], "live");
    }

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
        assert!(held.listed.is_empty());
        let new = s.apply(SourceEvent::Session { session: row("d") });
        assert_eq!(new.listed, ["d"]);
        s.apply(SourceEvent::Reset {
            reason: "connect".into(),
            generation: 3,
        });
        let staged = s.apply(SourceEvent::Session { session: row("e") });
        assert!(staged.listed.is_empty());
        dormant(&mut s, &[]);
        assert_eq!(seed(&mut s, 4, &["a"]).listed, ["a"]);
    }

    #[test]
    fn a_row_folded_ahead_reports_no_return_of_its_own() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let ahead = s.listed_now(row("n"));
        assert!(ahead.visible && ahead.listed.is_empty(), "{ahead:?}");
        assert_eq!(s.held(), ["a", "n"]);
        let framed = s.apply(SourceEvent::Session { session: row("n") });
        assert!(framed.listed.is_empty());

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
        assert!(swapped.departed.is_empty() && swapped.listed.is_empty());
        assert_eq!(s.held(), ["a", "m", "n"]);

        s.listed_now(row("p"));
        let removed = s.apply(SourceEvent::Removed {
            session_id: "p".into(),
        });
        assert_eq!(removed.departed, ["p"]);
        let relisted = s.apply(SourceEvent::Session { session: row("p") });
        assert_eq!(relisted.listed, ["p"]);
    }

    #[test]
    fn offline_retains_and_dormant_drops() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        s.apply(SourceEvent::Offline {
            reason: "the connection to craze's hub closed".into(),
            cause: SourceOffline::Unreachable,
        });
        assert_eq!(s.held(), ["a"]);
        assert!(!s.live());
        assert_eq!(s.status()["state"], "offline");
        assert_eq!(s.status()["cause"], "unreachable");
        let gone = dormant(&mut s, &[]);
        assert_eq!(gone.departed, ["a"]);
        assert!(s.rows.is_empty());
        assert_eq!(s.status()["state"], "dormant");
    }

    #[test]
    fn a_dormant_departs_the_created_rows_too() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let first = dormant(&mut s, &[row("c"), row("a")]);
        assert_eq!(first.departed, ["a", "c"], "{first:?}");
        assert!(first.visible && first.listed.is_empty());
        let again = dormant(&mut s, &[row("c")]);
        assert_eq!(again.departed, ["c"]);
        assert!(!again.visible);
    }

    #[test]
    fn a_no_hub_answer_older_than_a_listing_folds_nothing() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let probed_at = s.listings();
        let returned = s.answer_listed();
        assert!(s.dormant(&[row("c")], probed_at).is_none());
        assert_eq!(s.held(), ["a"]);
        assert!(s.live());
        assert_eq!(s.mint_news(), returned + 1);
        let gone = dormant(&mut s, &[row("c")]);
        assert_eq!(gone.departed, ["a", "c"]);
    }

    #[test]
    fn a_listing_after_a_dormant_still_returns_its_row() {
        let mut s = CrazeState::new(1);
        seed(&mut s, 1, &["a"]);
        let gone = dormant(&mut s, &[]);
        assert_eq!(gone.departed, ["a"]);
        let departed = s.mint_news();
        let listings = s.listings();
        let returned = s.answer_listed();
        assert!(returned > departed);
        assert_eq!(s.listings(), listings + 1);
        s.listed_now(row("a"));
        assert_eq!(s.held(), ["a"]);
        let again = dormant(&mut s, &[]);
        assert_eq!(again.departed, ["a"]);
    }

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

    fn fake_ssh(dir: &std::path::Path) -> PathBuf {
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

    fn fake_exec(dir: &std::path::Path) -> Arc<SshExec> {
        let entry = SshEndpoint {
            name: "roost:srv/dev".to_string(),
            host: "127.0.0.1".to_string(),
            user: Some("dev".to_string()),
            ssh_port: 2222,
            known_hosts: None,
            identity_file: None,
        };
        let options = SshBridgeOptions {
            ssh_bin: Some(fake_ssh(dir)),
            ..Default::default()
        };
        Arc::new(SshExec::new(&entry, &options).expect("exec"))
    }

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
        assert_eq!(recorded(dir, 0).await.len(), before);
    }

    #[tokio::test]
    async fn a_remote_dial_reaches_only_a_running_hub() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec = fake_exec(dir.path());

        let (probe, asked) = FixedProbe::counted(Probe::NoHub);
        let dial = SshCrazeDial::new(
            Arc::clone(&exec),
            "BRIDGE-HUB".to_string(),
            "NO-START-HUB --no-start".to_string(),
            probe,
        );
        assert_eq!(dial.mode(), DialMode::NoStart);
        let stream = dial.dial().await.expect("no-start dial");
        assert_eq!(recorded(dir.path(), 1).await, ["NO-START-HUB --no-start"]);
        assert_eq!(asked.load(Ordering::SeqCst), 0);
        drop(stream);

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
        assert!(dial.fall_back(&refused));
        assert_eq!(dial.mode(), DialMode::Legacy);
        assert!(!dial.fall_back(&refused));

        assert_legacy_gate(&exec, dir.path()).await;
        let (probe, asked) = FixedProbe::counted(Probe::Hub);
        let legacy = SshCrazeDial::new(
            Arc::clone(&exec),
            "BRIDGE-HUB".to_string(),
            "NO-START-HUB --no-start".to_string(),
            probe,
        );
        assert!(legacy.fall_back(&refused));
        let stream = legacy.dial().await.expect("legacy dial");
        assert_eq!(
            recorded(dir.path(), 2).await,
            ["NO-START-HUB --no-start", "BRIDGE-HUB"]
        );
        assert_eq!(asked.load(Ordering::SeqCst), 1);
        drop(stream);

        let ungated = SshCrazeDial::ungated(Arc::clone(&exec), "ENSURE-HUB".to_string());
        let stream = ungated.dial().await.expect("ungated dial");
        assert_eq!(
            recorded(dir.path(), 3).await.last().map(String::as_str),
            Some("ENSURE-HUB")
        );
        assert!(!ungated.fall_back(&refused));
        drop(stream);
    }

    #[tokio::test]
    async fn a_refused_no_start_falls_back_once_for_every_dial_of_the_source() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec = fake_exec(dir.path());
        std::fs::write(
            dir.path().join("no-start.sh"),
            "sleep 0.3\nprintf '%s\\n' 'craze bridge: unknown or malformed flag; see craze bridge --help' >&2\nexit 1\n",
        )
        .expect("write");
        let options = ShedReachOptions::default();
        let reach = build_ssh_craze(Arc::clone(&exec), &options).expect("remote reach");
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
        assert_eq!(lost.cause(), SourceOffline::Unreachable);
        let lines = recorded(dir.path(), 4).await;
        assert_eq!(no_starts(&lines), 2, "{lines:#?}");
        assert_eq!(
            lines
                .iter()
                .filter(|l| l.contains("providers --hub --json"))
                .count(),
            1
        );
        assert_eq!(
            lines
                .iter()
                .filter(|l| l.contains("bridge --hub") && !l.contains("--no-start"))
                .count(),
            1
        );
        drop(won);

        let (_conn, _notes) = connect(dial.as_ref()).await.expect("legacy: live");
        let lines = recorded(dir.path(), 6).await;
        assert_eq!(no_starts(&lines), 2);

        std::fs::remove_file(dir.path().join("no-start.sh")).expect("rm");
        let again = build_ssh_craze(Arc::clone(&exec), &options).expect("registered again");
        let (dial, _gone) = tracked(Arc::clone(&again.dial));
        let stream = dial.dial().await.expect("duplex");
        let lines = recorded(dir.path(), 7).await;
        assert_eq!(no_starts(&lines), 3);
        drop(stream);
    }

    #[tokio::test]
    async fn only_a_refusal_of_the_flag_falls_back() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec = fake_exec(dir.path());
        let options = ShedReachOptions::default();
        let reach = build_ssh_craze(Arc::clone(&exec), &options).expect("reach");
        let (dial, _gone) = tracked(Arc::clone(&reach.dial));
        let cases: [(&str, &str); 4] = [
            (
                "printf '%s\\n' 'sh: 1: craze: not found' >&2; exit 127\n",
                "NotInstalled",
            ),
            (
                "printf '%s\\n' 'Error: unknown flag: --hub' >&2; exit 1\n",
                "TooOld",
            ),
            (
                "printf '%s\\n' 'craze bridge: no hub: the runtime dir is group-writable' >&2; exit 1\n",
                "Unreachable",
            ),
            (
                "printf '%s\\n' 'craze bridge: no hub is running' >&2; exit 3\n",
                "NoHub",
            ),
        ];
        for (i, (ending, want)) in cases.iter().enumerate() {
            std::fs::write(dir.path().join("no-start.sh"), ending).expect("write");
            let err = connect(dial.as_ref()).await.err().expect("no hub");
            assert!(format!("{err:?}").starts_with(want), "{want}: {err:?}");
            let lines = recorded(dir.path(), i + 1).await;
            assert_eq!(lines.len(), i + 1);
            assert!(lines.iter().all(|l| l.contains("bridge --hub --no-start")));
        }
        let no_hub = connect(dial.as_ref()).await.err().expect("no hub");
        assert_eq!(no_hub.cause(), SourceOffline::NoHub);
    }

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
        assert!(!s.listed_now(stale).visible);
        assert_eq!(s.rows["n"].title, "the frame's");

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
        assert_eq!(swapped.departed, ["n"]);

        let removed = s.apply(SourceEvent::Removed {
            session_id: "m".into(),
        });
        assert_eq!(removed.departed, ["m"]);
        assert_eq!(s.held(), ["a"]);
    }

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
        assert_eq!(r.title, None);
        assert!(shed_craze::valid_request_id(&r.request_id));
        let r = create_request(
            Some("/w"),
            Some("grok"),
            Some("two\nlines\n"),
            Some("  Fix the login bug "),
            Some("shed-abc"),
        )
        .unwrap();
        assert_eq!(r.provider.as_deref(), Some("grok"));
        assert_eq!(r.prompt.as_deref(), Some("two\nlines\n"));
        assert_eq!(r.title.as_deref(), Some("Fix the login bug"));
        assert_eq!(r.request_id, "shed-abc");
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

    #[test]
    fn a_held_create_needs_the_request_id_it_replays() {
        assert_eq!(held_create(true, Some("shed-held")), Ok(true));
        assert_eq!(held_create(false, Some("shed-held")), Ok(false));
        assert_eq!(held_create(false, None), Ok(false));
        assert_eq!(held_create(true, None).unwrap_err().code, "bad_request");
    }

    #[test]
    fn a_craze_failure_carries_the_lane_code_or_outcome_unknown() {
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
    fn a_reopen_failure_carries_its_variant_or_the_inner_lane_code() {
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
        let worded = CrazeFailure::reopen(LaneReopenError::Lane(
            shed_craze::errors::outcome_unknown("lost"),
        ));
        assert_eq!(worded.code, "failed");
    }

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

    #[test]
    fn the_states_an_unlisted_host_is_offered_on() {
        assert!(!CrazeState::default().proven_unattached());
        let mut s = CrazeState::new(4);
        assert!(!s.proven_unattached());
        dormant(&mut s, &[]);
        assert!(s.proven_unattached());
        s.offline(SourceOffline::TooOld, "unknown flag: --hub".into());
        assert!(s.proven_unattached());
        let too_old = s.standing();
        s.offline(SourceOffline::TooOld, "said otherwise".into());
        assert_eq!(s.standing(), too_old);
        for cause in [
            SourceOffline::NotInstalled,
            SourceOffline::Unreachable,
            SourceOffline::Failed,
        ] {
            s.offline(cause.clone(), "x".into());
            assert!(!s.proven_unattached(), "{cause:?}");
        }
        seed(&mut s, 1, &[]);
        assert!(!s.proven_unattached());
    }

    #[test]
    fn the_status_vocabulary() {
        assert_eq!(CrazeState::default().status()["state"], "absent");
        let mut s = CrazeState::new(3);
        assert_eq!(s.status()["state"], "absent");
        s.offline(SourceOffline::NotInstalled, "exit 127".into());
        assert_eq!(s.status()["state"], "absent");
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

    #[test]
    fn every_status_names_the_sources_generation() {
        assert_eq!(CrazeState::default().status()["source_gen"], Value::Null);
        let mut s = CrazeState::new(7);
        assert_eq!(s.status()["source_gen"], 7);
        dormant(&mut s, &[]);
        assert_eq!(s.status()["source_gen"], 7);
        s.offline(SourceOffline::Unreachable, "no route".into());
        assert_eq!(s.status()["source_gen"], 7);
        seed(&mut s, 1, &[]);
        assert_eq!(s.status()["source_gen"], 7);
        assert_eq!(CrazeState::new(8).status()["source_gen"], 8);
    }
}
