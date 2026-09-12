//! **The roost reach seam** and the observing inventory watcher on top of it
//! (plan 013 S1, re-cut onto roost's push feed by plan 014).
//!
//! [`shed_core::roost`] knows how to *talk* to a `roost-session` — one
//! [`Conn`][shed_core::roost::Conn], typed ops, the compatibility gate, the row
//! model. What it deliberately does not know is how a given client *gets* to
//! one. This module is that half, and it is the exact analogue of
//! [`crate::machine`]: the pure wire lives in `shed-core`, and what is left —
//! the part that genuinely differs per client — is one trait.
//!
//! ## The seam is an endpoint, not a port
//!
//! [`crate::machine::MachineForward`] hands back a `u16` and promises it never
//! moves, because a hub answers on a fixed remote port and every client's job is
//! to get a local socket pointed at it. A roost-session is not like that:
//!
//! | client | how it reaches a session |
//! |---|---|
//! | desktop, local | [`LocalSession`] — the session's own Unix socket, if one is there |
//! | desktop, a machine | [`SshBridge`] — roost's own [`SshTunnel`][roost_ipc::ssh::SshTunnel], whose `bridge.sock` lives in a **per-attempt** scratch directory |
//! | shed-mobile | a `dartssh2` bridge on the Dart side; Rust is handed the port ([`LabelledPort`], or the pinned [`FixedPort`]) |
//!
//! The middle row is why [`RoostReach::ensure`] returns a
//! [`RoostEndpoint`] rather than a fixed address. roost names each connect
//! attempt's scratch directory for the attempt (`roost-ssh-<host>-<pid>-<seq>`)
//! precisely so a disconnect racing the reconnect behind it cannot delete the
//! winner's files — which means the bridge socket's *path* legitimately moves
//! across a re-establish. A port-shaped seam would have to fight that; an
//! endpoint-shaped one just asks again.
//!
//! ## `invalidate` is not a probe
//!
//! [`RoostReach::invalidate`] is called after **any** request error, and the
//! next [`RoostReach::ensure`] rebuilds rather than trusting a liveness check.
//! That is deliberate: an SSH bridge socket is a local `UnixListener` that goes
//! on accepting connections perfectly happily after the `ssh` master behind it
//! has died — the failure only surfaces when the far side never answers. "Can I
//! connect to it" is therefore not the question worth asking, and a reach that
//! asked it would hand back a socket that accepts and then hangs.
//!
//! ## Why a failure is typed, and why it arrives late (plan 019 §3.6)
//!
//! The same fact that makes `invalidate` unconditional makes a failure's
//! *reason* hard to come by: the loop only ever sees its end of a local socket
//! going quiet. The `ssh` exec behind that socket is what failed, and its stderr
//! is classified onto the tunnel **after** it dies — after roost's warm-up has
//! already run `true` and succeeded, so `ensure` reported nothing wrong. So
//! there are two paths a reason can take and they are not interchangeable:
//!
//! * [`RoostReach::ensure`] fails, and answers with a typed [`ReachError`];
//! * `ensure` succeeded and a *request* failed, and the classification has to be
//!   fetched from the transport — [`RoostReach::last_error`], overlaid under a
//!   generation watermark so a record that has already been shown is never shown
//!   again as the reason for a later drop.
//!
//! Both end up as [`RoostUpdate::Down`]'s `kind`, which is what lets a client
//! offer an install for a host with no `roost-session` and a start for one with
//! no session running — decisions that were previously only expressible by
//! grepping a sentence.
//!
//! ## Two ssh stacks per host, for the length of a bootstrap
//!
//! [`SshBridge`] runs exactly one remote command — roost's candidate ladder,
//! ending in `roost-session client-bridge` — and is not parameterizable.
//! Putting a `roost-session` on a host means running roost's *scripts* there, so
//! [`SshExec`] is a second `ssh` stack on a ControlMaster of its own, driving
//! the sans-IO machines in [`shed_core::roost::bootstrap`] through
//! [`BootstrapRunner`]. Plan 019 §3.6 accepts that and names the unification as
//! future work; what keeps it honest meanwhile is that both stacks are built
//! from one host-key posture ([`pinned_config`]), so the duplication is a second
//! process and never a second opinion about who this host is.
//!
//! [`pinned_config`]: fn@pinned_config

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use roost_ipc::client::EventFrame;
use roost_ipc::messages::{Tab, TabDumpResult, TabOpenParams};
use roost_ipc::ssh::{
    classify, ResolvedTransport, SshConfigPaths, SshTarget, SshTunnel, SshTunnelOptions,
};
use tokio::sync::mpsc;

use shed_core::config::MachineEntry;
use shed_core::roost::bootstrap::{
    reach_code, wire_agent_hooks, BootstrapFailure, CallError, HooksResult, InstallMachine,
    InstallRequest, Installed, Outcome, Probe, ProbeMachine, SourceHandle, Stage, Stdin, Step,
};
use shed_core::roost::{local_session_socket, Admit, Conn, Fence, RoostError, RoostInventory};

use crate::backoff;
use crate::machine::{FixedPort, ForwardError};

/// Re-exported so a client that drives roost never has to name `shed_core`'s
/// module as well as this one: the endpoint shape the seam hands back, the
/// launch argv for a kind, and the capabilities a roost host advertises (roost
/// has no `shed-ext-rc` to probe, so they are synthesized).
pub use shed_core::roost::{launch_argv, roost_capabilities, RoostEndpoint};

// ---------------------------------------------------------------------------
// why a reach failed, typed
// ---------------------------------------------------------------------------

/// What a failed reach *means*, reduced to the four answers a client acts on
/// differently (plan 019 §3.6).
///
/// This is deliberately coarser than roost's own
/// [`SshFailure`](roost_ipc::ssh::SshFailure), which has six
/// families. Three of them — a changed host key, an unknown host key, a refused
/// login — are all "shed could not get onto that box", and a client's move is
/// the same for each: show the sentence roost wrote, and offer nothing. The two
/// that ARE different are the two the bootstrap turns on, because they say the
/// box was reached and something about `roost-session` there is missing:
/// [`NotInstalled`](ReachKind::NotInstalled) offers an install and
/// [`NoSession`](ReachKind::NoSession) offers a start.
///
/// `Copy` and field-less: it crosses to Dart as a plain enum (the FRB-mirror
/// rule in `crates/CLAUDE.md`), and the sentence that goes with it travels
/// beside it in [`ReachError::message`] rather than inside it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReachKind {
    /// Nothing on roost's candidate ladder: exit 127 / `command not found`.
    /// The one kind that means "an install would fix this".
    NotInstalled,
    /// A `roost-session` is there and is not serving — `client-bridge: no
    /// session`. A start would fix this.
    NoSession,
    /// shed never got as far as asking: the handshake failed, the key did not
    /// verify, the login was refused, the budget expired.
    Unreachable,
    /// Something that is not an `ssh` exec's verdict at all — a scratch
    /// directory that could not be made, a local socket that is not there, a
    /// reach that was never mapped. **No
    /// [`SshFailure`](roost_ipc::ssh::SshFailure) maps here**, which is
    /// the property `stderr-classes.json`'s `reach_kinds` section pins.
    Other,
}

impl ReachKind {
    /// A stable kebab name, for an IPC payload and a log line.
    pub fn as_str(&self) -> &'static str {
        match self {
            ReachKind::NotInstalled => "not-installed",
            ReachKind::NoSession => "no-session",
            ReachKind::Unreachable => "unreachable",
            ReachKind::Other => "other",
        }
    }
}

/// Why a reach could not be built or could not be read — the sentence *and* the
/// kind.
///
/// Replaces the bare [`ForwardError`] on [`RoostReach::ensure`]. The string was
/// never the problem: [`SystemSshTunnels::open`] had roost's classified
/// [`SshFailure`](roost_ipc::ssh::SshFailure) in its hand and rendered it to
/// text on the way out, so by the
/// time a client saw "roost-session isn't installed on mini3" the only way back
/// to that fact was to grep the copy. Everything downstream of this type —
/// `Down { kind }`, the probe's `NotInstalled`/`NoSession` split, the desktop's
/// install button — is a branch that used to be a substring search or did not
/// exist.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReachError {
    /// The user-facing sentence. roost's own copy when roost classified it —
    /// this module does not rewrite a message roost wrote for the same
    /// situation.
    pub message: String,
    pub kind: ReachKind,
}

impl ReachError {
    pub fn new(kind: ReachKind, message: impl Into<String>) -> ReachError {
        ReachError {
            message: message.into(),
            kind,
        }
    }

    /// The fallback: something failed, and it was not an `ssh` exec's verdict.
    pub fn other(message: impl Into<String>) -> ReachError {
        ReachError::new(ReachKind::Other, message)
    }

    /// roost's classified family, in shed's vocabulary, with roost's own copy.
    ///
    /// The mapping is pinned two ways round in
    /// `crates/fixtures/roost-vectors/stderr-classes.json`: `classes` says what
    /// [`classify_ssh_failure`](roost_ipc::ssh::classify_ssh_failure) answers
    /// for a given stderr, and `reach_kinds` says what that answer becomes here.
    /// Both are asserted from this crate's own tests, so a roost bump that adds
    /// a seventh family fails to compile here (the match is exhaustive) and a
    /// bump that re-routes an existing one fails the golden.
    pub fn from_ssh_failure(failure: &roost_ipc::ssh::SshFailure, target: &str) -> ReachError {
        use roost_ipc::ssh::SshFailure;
        let kind = match failure {
            SshFailure::NotFound => ReachKind::NotInstalled,
            SshFailure::NoSession => ReachKind::NoSession,
            // The three "could not get on the box" families and roost's
            // fallthrough. A client shows the sentence and offers nothing;
            // splitting them here would be four buttons that all do nothing.
            SshFailure::ChangedHostKey
            | SshFailure::HostKeyUnknown
            | SshFailure::Auth
            | SshFailure::Transport(_) => ReachKind::Unreachable,
        };
        ReachError::new(kind, failure.message(target))
    }
}

impl std::fmt::Display for ReachError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The message alone: it is the only half a `to_string()` caller — a
        // `Down` reason, a log line — has ever wanted, and prefixing the kind
        // would put a kebab word in front of every sentence a user reads.
        f.write_str(&self.message)
    }
}

impl std::error::Error for ReachError {}

/// A [`ForwardError`] is a reach failure with no family — the hub seam's shape,
/// and every local step that never got as far as an `ssh` exec.
impl From<ForwardError> for ReachError {
    fn from(error: ForwardError) -> ReachError {
        ReachError::other(error.0)
    }
}

/// One transport's most recent failure, and **which exec it came from**.
///
/// The generation is the whole point and it is roost's
/// ([`RecordedFailure`](roost_ipc::ssh::RecordedFailure)): a tunnel bumps it
/// once per `ssh` exec, so a reader that remembers the highest one it has
/// already shown can tell "something failed since" from "that is the error I
/// reported last time". Without it, overlaying a tunnel's last error onto a
/// disconnect would re-show a stale classification forever — every subsequent
/// drop on that tunnel would read as the first one's reason, because
/// `last_error` is never cleared for a tunnel's life.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RecordedReach {
    /// roost's watermark: the highest exec generation that has reported a
    /// failure. Not necessarily the exec `error` came from — roost keeps a
    /// classified family over a later fallthrough — which is exactly why the
    /// comparison a reader makes is against this number and not against the
    /// family's own.
    ///
    /// **Invariant, and a reach implementation owes it: this number is
    /// monotonic for the life of the REACH, not merely of one transport.**
    /// roost's counter is per `SshTunnel` and restarts at the bottom in every
    /// new one, while a reader's watermark ([`overlay_reach_reason`]) is per
    /// watcher and outlives any number of them. A reach that passed roost's
    /// number straight through would therefore have its first few records after
    /// every re-establish silently discarded as stale — the drop a client is
    /// being shown right now reported by the transport that is already gone. A
    /// reach whose transport can be replaced (today: [`SshBridge`]) offsets the
    /// number past everything the previous transport could ever have reported.
    pub generation: u64,
    pub error: ReachError,
}

// ---------------------------------------------------------------------------
// the reach seam
// ---------------------------------------------------------------------------

/// A way to reach one roost-session.
///
/// **Contract:** `ensure` is idempotent and may return a *different* endpoint
/// than last time (see the module doc — an SSH bridge's socket moves across a
/// re-establish); `invalidate` marks whatever is held as not to be trusted, so
/// the next `ensure` rebuilds it from scratch. Neither ever spawns a
/// roost-session: roost's rule is connect-if-present, and a client that started
/// somebody's session behind their back would be inventing state.
#[async_trait::async_trait]
pub trait RoostReach: Send + Sync {
    /// What to call this reach in a failure message. Not the row label — the
    /// watcher stamps rows from its own `label` argument — this one answers
    /// "which reach could not be built".
    fn label(&self) -> &str;

    /// Make the reach usable and say where to dial.
    async fn ensure(&self) -> Result<RoostEndpoint, ReachError>;

    /// Mark the held transport as suspect. Cheap and infallible: the rebuild
    /// happens in the next [`RoostReach::ensure`], where it can fail properly.
    async fn invalidate(&self);

    /// The typed reason the **held transport** last recorded, if it records
    /// any.
    ///
    /// Not the same question as `ensure`'s `Err`, and that is the whole reason
    /// it exists. An [`SshBridge`] whose `ensure` succeeded has a live
    /// `bridge.sock` and a warm mux; what fails afterwards is one *per-connection*
    /// `ssh` exec behind that socket, and the only thing this side sees of it is
    /// its end of a Unix socket going quiet. The classification — `exit 127`,
    /// `client-bridge: no session` — is on the tunnel, recorded after the fact,
    /// and this is how a caller reads it (plan 019 §3.6).
    ///
    /// Defaulted to `None` because most reaches genuinely have nothing to say:
    /// a loopback port somebody else owns, a local socket, a reach that is
    /// unreachable by construction. They report their reason through `ensure`,
    /// where it is not late.
    ///
    /// **An implementation that can replace its transport owes the monotonicity
    /// invariant on [`RecordedReach::generation`]** — a caller's watermark is
    /// per watcher and cannot see a transport being swapped underneath it.
    async fn last_error(&self) -> Option<RecordedReach> {
        None
    }
}

/// This machine's own `roost-session`, if one is running.
///
/// **Never spawns.** A missing socket is an ordinary state — most machines are
/// not running a session most of the time — so it is a plain
/// [`ForwardError`] naming every path that was looked at, not a fault.
pub struct LocalSession {
    label: String,
    socket: PathBuf,
    /// Every candidate the resolver considered, so the failure message names the
    /// place a session would normally be rather than saying "not found".
    tried: Vec<PathBuf>,
}

impl LocalSession {
    /// A session at a known socket path.
    pub fn new(label: impl Into<String>, socket: impl Into<PathBuf>) -> LocalSession {
        let socket = socket.into();
        LocalSession {
            label: label.into(),
            tried: vec![socket.clone()],
            socket,
        }
    }

    /// The local session, resolved through [`shed_core::roost::paths`] — which
    /// shed owns rather than roost's own resolver, because roost's picks the
    /// `-dev` socket from the *consuming* crate's build profile.
    ///
    /// Labelled `localhost`, the name the clients show this host under.
    pub fn default_local() -> LocalSession {
        let resolved = local_session_socket();
        LocalSession {
            label: "localhost".to_string(),
            socket: resolved.path,
            tried: resolved.tried,
        }
    }

    /// Where this reach dials.
    pub fn socket(&self) -> &Path {
        &self.socket
    }
}

#[async_trait::async_trait]
impl RoostReach for LocalSession {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        if self.socket.exists() {
            return Ok(RoostEndpoint::Unix(self.socket.clone()));
        }
        // **`NoSession`, not `NotInstalled`.** A missing socket says nothing at
        // all about whether the binary is there — this resolver looks for a
        // *session*, and the machine it looks on is the one shed is running on,
        // where a `roost-session` may well be installed and simply not started.
        // That is the same distinction the SSH ladder draws between exit 127 and
        // `client-bridge: no session`, and a client offering "install roost" on
        // its own laptop because no session happened to be up would be wrong in
        // the ordinary case.
        Err(ReachError::new(
            ReachKind::NoSession,
            format!(
                "no roost-session at {}",
                self.tried
                    .iter()
                    .map(|p| p.display().to_string())
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        ))
    }

    async fn invalidate(&self) {}
}

/// A loopback port somebody else already pointed at a session, with a name.
///
/// shed-mobile's reach: Dart stands up the `dartssh2` bridge, keeps it working
/// across a network change, and hands Rust the port — so `ensure` has nothing to
/// do and `invalidate` nothing to rebuild. The `label` is the machine's name,
/// which is what makes a failure message read in the vocabulary the user typed.
pub struct LabelledPort {
    label: String,
    port: u16,
}

impl LabelledPort {
    pub fn new(label: impl Into<String>, port: u16) -> LabelledPort {
        LabelledPort {
            label: label.into(),
            port,
        }
    }

    pub fn port(&self) -> u16 {
        self.port
    }
}

#[async_trait::async_trait]
impl RoostReach for LabelledPort {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        Ok(RoostEndpoint::TcpLoopback(self.port))
    }

    async fn invalidate(&self) {}
}

/// The hub seam's [`FixedPort`] reaches a roost-session too — same port, same
/// "somebody else owns it" contract. It carries no name of its own, so its label
/// is a constant; a client with a machine name to report should use
/// [`LabelledPort`].
#[async_trait::async_trait]
impl RoostReach for FixedPort {
    fn label(&self) -> &str {
        "fixed-port"
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        Ok(RoostEndpoint::TcpLoopback(self.0))
    }

    async fn invalidate(&self) {}
}

/// A reach that is known not to exist, and says why.
///
/// The honest answer for a machine nothing has been mapped for — the Tauri
/// harness's unmapped-machine case, and any client that wants a row rendered as
/// unreachable-with-a-reason rather than silently missing.
pub struct UnreachableReach {
    label: String,
    error: ReachError,
}

impl UnreachableReach {
    /// A reach with a reason and no family — nothing was mapped, nothing was
    /// tried, so there is nothing to classify.
    pub fn new(label: impl Into<String>, reason: impl Into<String>) -> UnreachableReach {
        UnreachableReach::typed(label, ReachError::other(reason))
    }

    /// A reach that is down for a reason somebody already classified — a probe
    /// that came back `not-installed`, a host the last connect could not
    /// authenticate to. The kind rides through to `Down { kind }` and the
    /// client's button reads off it.
    pub fn typed(label: impl Into<String>, error: ReachError) -> UnreachableReach {
        UnreachableReach {
            label: label.into(),
            error,
        }
    }
}

#[async_trait::async_trait]
impl RoostReach for UnreachableReach {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        Err(self.error.clone())
    }

    async fn invalidate(&self) {}
}

// ---------------------------------------------------------------------------
// the SSH bridge
// ---------------------------------------------------------------------------

/// A live SSH transport to one machine's roost-session, as this module needs it.
///
/// Two methods, because two are all [`SshBridge`] uses — and behind a trait
/// because the production implementation spawns `ssh` child processes, which a
/// unit test cannot assert against without either a real host or a fake binary.
#[async_trait::async_trait]
pub trait Tunnel: Send + Sync {
    /// The local Unix socket a client dials. Bound only after the tunnel has
    /// been established.
    fn bridge_socket(&self) -> &Path;

    /// The most recent per-connection failure, typed, with the exec generation
    /// roost recorded it at. `None` when this tunnel has had none.
    ///
    /// **This is a LATE record and reading it is the whole point.** A bridge
    /// connection is a `ssh … exec roost-session client-bridge` behind a local
    /// Unix socket; the master is warmed up with a plain `true` before any of
    /// them run, so `open`/`establish` succeed on a host where the ladder later
    /// falls off its end at exit 127. The classification for that exec only
    /// exists after it has died, and it arrives here.
    fn last_error(&self) -> Option<RecordedReach>;

    /// Close the mux and remove the scratch directory. Idempotent.
    async fn shutdown(&self);
}

/// roost's [`SshTunnel`] plus the target its failures are *about*.
///
/// The target is carried because roost's copy is written with the host in it
/// ([`SshFailure::message`](roost_ipc::ssh::SshFailure::message) interpolates
/// it — "run `ssh workbox` once in a terminal") and `SshTunnel` does not expose
/// the target it was opened for. Rendering the sentence without it, or
/// re-deriving it from the scratch path, would both be worse than holding the
/// one `String` the opener already had in its hand.
struct BridgeTunnel {
    inner: SshTunnel,
    target: String,
}

#[async_trait::async_trait]
impl Tunnel for BridgeTunnel {
    fn bridge_socket(&self) -> &Path {
        self.inner.bridge_socket()
    }

    fn last_error(&self) -> Option<RecordedReach> {
        self.inner.last_error().map(|recorded| RecordedReach {
            // roost's watermark, not the family's own generation: see
            // [`RecordedReach::generation`].
            generation: recorded.generation,
            error: ReachError::from_ssh_failure(&recorded.failure, &self.target),
        })
    }

    async fn shutdown(&self) {
        self.inner.shutdown().await;
    }
}

/// How a [`SshBridge`] gets a [`Tunnel`] — the seam a test replaces.
///
/// One method rather than roost's two (`open` then `establish`), because the two
/// are never useful apart here: a tunnel that opened but did not establish has
/// no bound socket and nothing to hand back, and the caller's only sane move is
/// to tear it down. Folding them keeps that teardown in one place.
#[async_trait::async_trait]
pub trait TunnelOpener: Send + Sync {
    async fn open(
        &self,
        host_id: &str,
        target: &SshTarget,
        options: SshTunnelOptions,
    ) -> Result<Box<dyn Tunnel>, ReachError>;
}

/// The production opener: roost's own [`SshTunnel`], opened and established.
pub struct SystemSshTunnels;

/// roost's own classification of an open/establish failure, kept.
///
/// [`SshTunnelError`](roost_ipc::ssh::SshTunnelError) splits by who can act on
/// it: an `Ssh` arm carries a [`SshFailure`](roost_ipc::ssh::SshFailure) that
/// was read off the far side's stderr, and a `Local` arm is this side's own
/// (a scratch directory that would not create, a socket that would not bind),
/// which has no family and no remedy on the remote host. That is exactly the
/// [`ReachKind::Other`] boundary, so the two map onto each other without a
/// judgement call.
///
/// This function is the reason `ReachError` exists at all: the string this used
/// to return was `SshTunnelError`'s `Display`, which IS
/// `failure.message(target)` — so the classification was *computed*, rendered,
/// and thrown away, one call before the only code that wanted it.
fn reach_error(target: &str, error: &roost_ipc::ssh::SshTunnelError) -> ReachError {
    match error.failure() {
        Some(failure) => ReachError::from_ssh_failure(failure, target),
        None => ReachError::other(error.to_string()),
    }
}

#[async_trait::async_trait]
impl TunnelOpener for SystemSshTunnels {
    async fn open(
        &self,
        host_id: &str,
        target: &SshTarget,
        options: SshTunnelOptions,
    ) -> Result<Box<dyn Tunnel>, ReachError> {
        let tunnel = SshTunnel::open(host_id, target, options)
            .await
            .map_err(|e| reach_error(&target.raw, &e))?;
        if let Err(e) = tunnel.establish().await {
            // The async teardown, not `Drop`'s blocking one: we are already in a
            // runtime, and roost's `Drop` explicitly exists for the case where
            // nobody could await. An establish that failed still owns a scratch
            // directory and possibly a `ControlPersist` master.
            tunnel.shutdown().await;
            return Err(reach_error(&target.raw, &e));
        }
        Ok(Box::new(BridgeTunnel {
            inner: tunnel,
            target: target.raw.clone(),
        }))
    }
}

/// Everything [`SshBridge`] would otherwise read from the environment.
///
/// A bundle rather than lookups so nothing about a shipped shed is steered by a
/// variable meant for roost's own test lane. In particular this module NEVER
/// calls [`SshTunnelOptions::from_env`]: that reads `ROOST_SSH_BIN` and
/// `ROOST_TEST_MODE`, and the latter sets `jail_fs_root`, which decides which
/// remote binary the exec chain resolves. shed pins it to `false`.
#[derive(Clone)]
pub struct SshBridgeOptions {
    /// The `ssh` binary. `None` → `ssh` on the PATH.
    pub ssh_bin: Option<PathBuf>,
    /// Candidate parents for roost's per-attempt scratch directory, in
    /// preference order. Empty → `$TMPDIR` then `/tmp`, roost's own order.
    pub scratch_parents: Vec<PathBuf>,
    /// The `ssh_config` files roost's generated config includes. `None` →
    /// `$HOME/.ssh/config` + `/etc/ssh/ssh_config`.
    pub config_paths: Option<SshConfigPaths>,
    /// How a tunnel is obtained. The test seam.
    pub opener: Arc<dyn TunnelOpener>,
}

impl Default for SshBridgeOptions {
    fn default() -> SshBridgeOptions {
        SshBridgeOptions {
            ssh_bin: None,
            scratch_parents: Vec::new(),
            config_paths: None,
            opener: Arc::new(SystemSshTunnels),
        }
    }
}

/// `$TMPDIR` then `/tmp` — roost's own candidate order, spelled here rather than
/// taken from [`SshTunnelOptions::from_env`] so the two variables that steer the
/// exec chain are never read alongside it.
fn default_scratch_parents() -> Vec<PathBuf> {
    let mut parents: Vec<PathBuf> = Vec::new();
    if let Some(tmpdir) = std::env::var_os("TMPDIR").filter(|value| !value.is_empty()) {
        parents.push(PathBuf::from(tmpdir));
    }
    let fallback = PathBuf::from("/tmp");
    if !parents.contains(&fallback) {
        parents.push(fallback);
    }
    parents
}

/// A directory this process owns and removes when the value dies.
///
/// Hand-rolled rather than `tempfile` because it is used by the *library*, and
/// [`SshBridge`] must not add a non-dev dependency to `shed-app` for four lines
/// of `create_dir_all`. Mode 0700: it holds an `ssh_config` naming a
/// `known_hosts` file, which is a host-key pinning decision, and a
/// world-writable one would be worth nothing.
struct ScratchDir(PathBuf);

impl ScratchDir {
    fn new(name: &str) -> std::io::Result<ScratchDir> {
        use std::os::unix::fs::DirBuilderExt as _;

        static NEXT: AtomicU64 = AtomicU64::new(0);
        let path = std::env::temp_dir().join(format!(
            "shed-roost-{name}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::DirBuilder::new().mode(0o700).create(&path)?;
        Ok(ScratchDir(path))
    }
}

impl Drop for ScratchDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// One machine's roost-session, over roost's own SSH client-bridge.
///
/// ## Why not `ssh -N -L`
///
/// The reach `sx` and the Tauri app use for a machine's RC hub is a local
/// forward onto a known remote port. That is not available here: a
/// roost-session's socket path is resolved on the *far* side (XDG runtime dir,
/// uid, the `-dev` sibling) and never sent, so there is nothing to name in a
/// `-L`. roost's answer — a local `bridge.sock` whose every accepted connection
/// gets its own `ssh -T <target> "… exec roost-session client-bridge"` over a
/// shared `ControlMaster` — is the transport, and reusing it means the candidate
/// ladder and the failure classification behind it come from the pin.
///
/// ## The re-establish order is load-bearing
///
/// [`RoostReach::ensure`] on an invalidated bridge **shuts the old tunnel down
/// and drops it first**, then opens a fresh one. Not an optimisation: roost's
/// `open` sweeps the host's older scratch directories and refuses to reclaim one
/// whose `bridge.sock` still answers ("another Roost owns this target"). Opening
/// before tearing down would make the bridge refuse to rebuild itself.
pub struct SshBridge {
    label: String,
    /// The machine name, sanitized into the token roost names its scratch
    /// directory with.
    host_id: String,
    target: SshTarget,
    options: SshTunnelOptions,
    opener: Arc<dyn TunnelOpener>,
    /// The held tunnel and the offset its generations are reported at. A
    /// `tokio` mutex, and it is held across the open: two concurrent `ensure`s
    /// must not each spawn a tunnel to the same machine — the loser's would be
    /// a live `ssh` master nothing ever tears down.
    held: tokio::sync::Mutex<Held>,
    invalidated: AtomicBool,
    /// Holds the generated per-machine `ssh_config` when the entry pins a
    /// `known_hosts` file. Never read — it is owned for its `Drop`, which is
    /// what removes the directory when the bridge goes.
    _config_dir: Option<ScratchDir>,
    config_path: Option<PathBuf>,
}

/// What a bridge holds between `ensure`s: the transport, and the offset that
/// makes its generations monotonic across a replacement.
///
/// The two are one value under one lock **on purpose**. `base` is only ever
/// advanced in the same critical section that drops the tunnel it was computed
/// from, so there is no window in which a reader can pair a new tunnel's
/// generation with the old tunnel's offset.
#[derive(Default)]
struct Held {
    tunnel: Option<Box<dyn Tunnel>>,
    /// Added to every generation the held tunnel reports (see
    /// [`RecordedReach::generation`]). Starts at zero, so the first tunnel's
    /// numbers are roost's own.
    base: u64,
}

impl SshBridge {
    /// Build a bridge for one `machines:` entry. Nothing is spawned until
    /// [`RoostReach::ensure`] runs.
    pub fn new(entry: &MachineEntry, options: SshBridgeOptions) -> Result<SshBridge, ForwardError> {
        let SshBridgeOptions {
            ssh_bin,
            scratch_parents,
            config_paths,
            opener,
        } = options;
        let target = ssh_target(entry)?;
        let host_id = host_id(&entry.name);

        let base = config_paths.unwrap_or_else(SshConfigPaths::from_env);
        let Pinned {
            dir: config_dir,
            path: config_path,
            paths: tunnel_config_paths,
        } = pinned_config(entry, &host_id, base)?;

        let scratch_parents = if scratch_parents.is_empty() {
            default_scratch_parents()
        } else {
            scratch_parents
        };

        Ok(SshBridge {
            label: entry.name.clone(),
            host_id,
            target,
            options: SshTunnelOptions {
                config_paths: tunnel_config_paths,
                scratch_parents,
                ssh_bin: ssh_bin.unwrap_or_else(|| "ssh".into()),
                // Never from the environment. `ROOST_TEST_MODE` steers which
                // remote binary roost's exec chain resolves; a shed that read it
                // would exec whatever a stray variable pointed at.
                jail_fs_root: false,
            },
            opener,
            held: tokio::sync::Mutex::new(Held::default()),
            invalidated: AtomicBool::new(false),
            _config_dir: config_dir,
            config_path,
        })
    }

    /// The ssh target string roost was handed — `ssh://[user@]host[:port]`.
    pub fn target(&self) -> &str {
        &self.target.raw
    }

    /// The generated per-machine `ssh_config`, when the entry pinned a
    /// `known_hosts` file. `None` when it did not.
    pub fn ssh_config_path(&self) -> Option<&Path> {
        self.config_path.as_deref()
    }
}

#[async_trait::async_trait]
impl RoostReach for SshBridge {
    fn label(&self) -> &str {
        &self.label
    }

    async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
        let mut held = self.held.lock().await;
        if !self.invalidated.load(Ordering::SeqCst) {
            if let Some(tunnel) = held.tunnel.as_ref() {
                return Ok(RoostEndpoint::Unix(tunnel.bridge_socket().to_path_buf()));
            }
        }
        // Shut down BEFORE opening — see the type's doc: roost refuses to
        // reclaim a scratch directory whose bridge socket still answers.
        if let Some(tunnel) = held.tunnel.take() {
            // **Where the monotonicity invariant is kept** (see
            // [`RecordedReach::generation`]). roost's generation counter lives
            // on the `SshTunnel`, so the replacement about to be opened starts
            // its numbering at the bottom again while every reader's watermark
            // is per watcher and remembers the number it was last shown. Lift
            // the base past everything this tunnel could ever have reported —
            // its final recorded generation — and the successor's first record
            // is strictly greater than the predecessor's last, which is what
            // "a record from a new tunnel is never discarded as stale" means.
            //
            // The `+ 1` makes that true without assuming roost numbers its
            // first exec from one rather than from zero. `saturating_add`
            // rather than `+`: the sum can never wrap back down to a small
            // number the way a restarting counter did, which is the whole
            // failure this exists to remove. At the (unreachable — 2^64 failed
            // execs on one bridge) ceiling it stops advancing instead, and a
            // record that stops being news is the safe end of that trade.
            let last = tunnel
                .last_error()
                .map_or(0, |recorded| recorded.generation);
            held.base = held.base.saturating_add(last).saturating_add(1);
            tunnel.shutdown().await;
            drop(tunnel);
        }
        // Cleared before the open, not after: an `invalidate` that lands while
        // this open is in flight must survive it, or the tunnel it was warning
        // about would be kept.
        self.invalidated.store(false, Ordering::SeqCst);

        let tunnel = self
            .opener
            .open(&self.host_id, &self.target, self.options.clone())
            .await
            // The message is prefixed, the KIND is not touched: roost classified
            // this and shed's job is to say which reach it was about.
            .map_err(|e| {
                ReachError::new(
                    e.kind,
                    format!("{}: {}", target_token(&self.label), e.message),
                )
            })?;
        let socket = tunnel.bridge_socket().to_path_buf();
        held.tunnel = Some(tunnel);
        Ok(RoostEndpoint::Unix(socket))
    }

    async fn invalidate(&self) {
        self.invalidated.store(true, Ordering::SeqCst);
    }

    /// The held tunnel's own last word, at the bridge's own generation.
    ///
    /// Read under the same mutex `ensure` holds across an open, so a caller can
    /// never see a tunnel that is half-replaced, nor a new tunnel's generation
    /// paired with the offset of the one it replaced. A bridge with nothing
    /// held (never ensured, or torn down) has nothing to report — its reason
    /// came back from `ensure` instead, where it was not late.
    ///
    /// The generation reported is the held tunnel's plus the bridge's base, so
    /// it never restarts across a re-establish ([`RecordedReach::generation`]).
    async fn last_error(&self) -> Option<RecordedReach> {
        let held = self.held.lock().await;
        let recorded = held.tunnel.as_ref()?.last_error()?;
        Some(RecordedReach {
            generation: held.base.saturating_add(recorded.generation),
            error: recorded.error,
        })
    }
}

/// The `[user@]host` half of the target — what an `ssh_config` `Host` pattern
/// has to match, which is the host as written and never the user or the port.
fn target_host(entry: &MachineEntry) -> String {
    if entry.host.is_empty() {
        entry.name.clone()
    } else {
        entry.host.clone()
    }
}

/// `ssh://[user@]host[:port]` from a `machines:` entry, classified by roost.
///
/// The scheme is always spelled, for two reasons. It is the only form roost's
/// `classify` accepts a port in — a bare `host:port` is explicitly refused — and
/// it keeps the string away from `classify`'s `localhost` sentinel, which
/// resolves the LOCAL session socket through roost's own build-profile-sensitive
/// resolver. A machine named `localhost` is still an ssh target here.
///
/// The user is omitted when the entry names none and the port when it is 22, so
/// a bare `~/.ssh/config` alias reaches `ssh` as itself and is resolved by `ssh`.
///
/// An **IPv6 literal is bracketed** here and nowhere else. `2001:db8::1` with
/// port 2222 written plainly is `ssh://2001:db8::1:2222`, whose authority every
/// parser — roost's own `split_host_port`, and `ssh`'s — reads from the right as
/// host `2001:db8:` port `:1:2222` or worse. The brackets are the URL form's
/// only way to say where the address ends, which is why roost's own refusal
/// message spells `ssh://[::1]:22`. The `Host` line of the generated pin config
/// keeps the address UNBRACKETED ([`target_host`]) because that is the hostname
/// `ssh` extracts and matches patterns against.
fn ssh_target(entry: &MachineEntry) -> Result<SshTarget, ForwardError> {
    let host = target_host(entry);
    if host.trim().is_empty() {
        return Err(ForwardError(format!(
            "{}: has no host to reach",
            target_token(&entry.name)
        )));
    }
    let user = match entry.user.as_deref().filter(|u| !u.is_empty()) {
        Some(user) => format!("{user}@"),
        None => String::new(),
    };
    let port = if entry.ssh_port == 0 || entry.ssh_port == 22 {
        String::new()
    } else {
        format!(":{}", entry.ssh_port)
    };
    let raw = format!("ssh://{user}{}{port}", url_host(&host));
    match classify(&raw) {
        Ok(ResolvedTransport::Ssh(target)) => Ok(target),
        Ok(other) => Err(ForwardError(format!(
            "{}: {raw} is not an ssh target ({other:?})",
            target_token(&entry.name)
        ))),
        Err(e) => Err(ForwardError(format!("{}: {e}", target_token(&entry.name)))),
    }
}

/// The host as the authority of an `ssh://` URL: an IPv6 literal bracketed,
/// anything else untouched.
///
/// The `:` test is the whole rule. A hostname and an `ssh_config` alias cannot
/// contain a colon, an IPv4 address cannot either, and an IPv6 literal always
/// does — so a colon in a host is an address that needs delimiting, and there is
/// no case where bracketing something else would be right. An address the user
/// already bracketed is left as it is rather than double-wrapped.
fn url_host(host: &str) -> String {
    if host.contains(':') && !(host.starts_with('[') && host.ends_with(']')) {
        format!("[{host}]")
    } else {
        host.to_string()
    }
}

/// The machine name as a scratch-directory token.
///
/// roost reads its own directory names back as `<host_id>-<pid>-<seq>`, parsed
/// from the right, so a `-` inside the id is fine — but a `/` would make the
/// leaf a path and an empty id would make the name unparseable. The length cap
/// is the `sun_path` budget: the directory holds a `bridge.sock`, and 103 bytes
/// is the whole of it.
fn host_id(name: &str) -> String {
    let sanitized: String = name
        .chars()
        .take(32)
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') {
                c
            } else {
                '-'
            }
        })
        .collect();
    if sanitized.is_empty() {
        "machine".to_string()
    } else {
        sanitized
    }
}

/// The per-machine `ssh_config` that pins a host key.
///
/// Ordering is the whole content of this function. `ssh` takes the **first**
/// value it obtains for a keyword, and roost's generated wrapper `Include`s this
/// file first — so the pin block goes at the top, where it beats anything the
/// user's own config says about `UserKnownHostsFile` or
/// `StrictHostKeyChecking` for this host, and the user's config is included
/// *after* it, where it still supplies the `HostName`/`Port`/`IdentityFile` an
/// alias needs.
///
/// The `Include` line is kept (rather than dropped as a double-include) because
/// roost includes *this file*, not `~/.ssh/config`: passing this as
/// `config_paths.user` displaces the user's own config, and without the line
/// nothing would resolve their aliases. `~` is expanded here rather than left to
/// `ssh` so the file says exactly which path it means.
///
/// **Both paths are quoted.** `UserKnownHostsFile` takes a *list* of files, so
/// `ssh` splits its argument on whitespace: an unquoted
/// `/Users/me/Library/Application Support/shed/known_hosts` becomes two
/// half-paths, neither of which exists, and the pin silently degrades into
/// trusting nothing — with `StrictHostKeyChecking yes` above it, into refusing
/// every connection. Double quotes are ssh_config's own escape for exactly this.
/// The `ssh_config` composition one reach to `entry` runs under.
///
/// **Shared by [`SshBridge`] and [`SshExec`] on purpose** (plan 019 §3.6: the
/// exec runner's configuration is the bridge's). The two stacks reach the same
/// host for the same user in the same breath — one to read its inventory, one to
/// run roost's install scripts on it — and a host-key pin that applied to only
/// one of them would be a pin with a hole in it that nothing in either file
/// would show.
struct Pinned {
    /// Owned for its `Drop`, which removes the directory the config lives in.
    dir: Option<ScratchDir>,
    path: Option<PathBuf>,
    paths: SshConfigPaths,
}

fn pinned_config(
    entry: &MachineEntry,
    host_id: &str,
    base: SshConfigPaths,
) -> Result<Pinned, ForwardError> {
    let Some(known_hosts) = entry.known_hosts.as_deref().filter(|k| !k.is_empty()) else {
        // No pin: the user's own config and strictness apply, exactly as for
        // any other `ssh` to this host.
        return Ok(Pinned {
            dir: None,
            path: None,
            paths: base,
        });
    };
    let dir = ScratchDir::new(host_id).map_err(|e| {
        ForwardError(format!(
            "{}: could not create the ssh config directory: {e}",
            target_token(&entry.name)
        ))
    })?;
    let path = dir.0.join("ssh_config");
    let body = pinned_ssh_config(&target_host(entry), known_hosts, base.user.as_deref());
    write_private(&path, body.as_bytes()).map_err(|e| {
        ForwardError(format!(
            "{}: could not write {}: {e}",
            target_token(&entry.name),
            path.display()
        ))
    })?;
    Ok(Pinned {
        dir: Some(dir),
        path: Some(path.clone()),
        paths: SshConfigPaths {
            user: Some(path),
            system: base.system,
        },
    })
}

/// How a reach names itself in a sentence a user reads — plan 019 §0's target
/// grammar, applied to a [`MachineEntry`] whose `name` is either a bare
/// `machines:` key or an already-qualified token.
///
/// `machines:` entries are keyed by a bare name (`mini3`), and every message
/// this module writes about one has always said `machine:mini3`. A shed's roost
/// host arrives through the *same* [`MachineEntry`] shape (see
/// [`shed_reach_entry`]) but its name is already the grammar's own word,
/// `roost:popos/p019-a` — so the prefix must not be applied twice, or a failure
/// would read `machine:roost:popos/p019-a` and belong to neither vocabulary.
///
/// The test is a `:`, because that is the grammar: a qualified token always has
/// one and a `machines:` key never can (a name with a colon in it could not be
/// addressed as `machine:<name>` unambiguously in the first place).
fn target_token(name: &str) -> std::borrow::Cow<'_, str> {
    if name.contains(':') {
        std::borrow::Cow::Borrowed(name)
    } else {
        std::borrow::Cow::Owned(format!("machine:{name}"))
    }
}

fn pinned_ssh_config(host: &str, known_hosts: &str, user_config: Option<&Path>) -> String {
    let mut out = format!(
        "Host {host}\n  UserKnownHostsFile \"{}\"\n  StrictHostKeyChecking yes\n",
        expand_tilde(known_hosts).display()
    );
    // Only an existing file: an `Include` of a path that is not there is at best
    // silently ignored and at worst an error, and neither is worth risking on
    // every connection to a host whose user simply has no `~/.ssh/config`.
    if let Some(user_config) = user_config.filter(|path| path.exists()) {
        out.push_str(&format!("Include \"{}\"\n", user_config.display()));
    }
    out
}

/// `~` / `~/x` against `$HOME`. Anything else is returned as it was.
fn expand_tilde(path: &str) -> PathBuf {
    let Some(rest) = path.strip_prefix('~') else {
        return PathBuf::from(path);
    };
    let Some(home) = std::env::var_os("HOME").filter(|home| !home.is_empty()) else {
        return PathBuf::from(path);
    };
    let rest = rest.strip_prefix('/').unwrap_or(rest);
    if rest.is_empty() {
        PathBuf::from(home)
    } else {
        PathBuf::from(home).join(rest)
    }
}

/// Write a file only this user can read. It names a host-key pin.
fn write_private(path: &Path, contents: &[u8]) -> std::io::Result<()> {
    use std::io::Write as _;
    use std::os::unix::fs::OpenOptionsExt as _;

    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)?;
    file.write_all(contents)
}

/// A shed's roost host, as a [`MachineEntry`] — which is what makes the
/// existing [`SshBridge`] reach it **unchanged** (plan 019 §3.6).
///
/// A shed already has an ssh identity, and it is not a new one: `<shed>@<server
/// host> -p <server ssh port>` with `~/.shed/known_hosts` pinned is what the
/// terminal opens, what RC's `ssh_argv` builds and what the phone dials. So the
/// whole of "a shed becomes a roost host" is restating that identity in the
/// shape the bridge already takes — no second transport, no second host-key
/// posture, no second place for the port to be wrong.
///
/// Three fields are decided rather than copied:
///
/// * **`name` is the target grammar's own token**, `roost:<server>/<shed>`, not
///   the shed's bare name. It is what every sentence, row origin and
///   capabilities key says (§0), and it is deliberately distinct from the hub's
///   `<server>/<shed>` so the two rows for one shed can sit side by side under
///   the union rule. [`target_token`] leaves it alone precisely because it is
///   already qualified.
/// * **`known_hosts` is always pinned.** shed mints these host keys itself, so
///   unlike a `machines:` entry there is no user `ssh_config` to defer to and no
///   case where deferring would be right.
/// * **`rc_bin` is `None`.** This entry is never an RC target; it exists to be
///   handed to a roost reach. (Pin P7 keeps `machines[].rc_bin` out of scope
///   entirely.)
pub fn shed_reach_entry(server: &shed_core::config::ShedServerEntry, shed: &str) -> MachineEntry {
    MachineEntry {
        name: format!("roost:{}/{}", server.name, shed),
        // The parser defaults `host` to the entry name, but a hand-written
        // config need not have gone through it — and the fallback cannot be
        // `entry.name` the way [`target_host`]'s is, because this entry's name
        // is `roost:<server>/<shed>` and that is not a hostname.
        host: if server.host.is_empty() {
            server.name.clone()
        } else {
            server.host.clone()
        },
        user: Some(shed.to_string()),
        ssh_port: server.ssh_port,
        rc_bin: None,
        known_hosts: Some(crate::backend::known_hosts_path()),
    }
}

// ---------------------------------------------------------------------------
// the watcher
// ---------------------------------------------------------------------------

/// How many consecutive resyncs are tolerated before the row is called down.
///
/// A resync is cheap and expected — a daemon restart, a stream the server
/// closed because we fell behind, a lost commit — and it costs no `Down`, no
/// backoff and no stale row. What it must not do is spin: a daemon that skips a
/// revision every time, or a bridge that EOFs the stream on every subscribe,
/// would otherwise reconnect as fast as the loop can run, forever. Three in a
/// row without a single applied batch between them is the point at which "we
/// are behind" stops being a better explanation than "this is broken".
///
/// A constant, not an env var: it is a correctness bound, and a knob would make
/// two clients disagree about when a session is down.
pub const MAX_CONSECUTIVE_RESYNCS: u32 = 3;

/// One update from a [`RoostWatcher`].
///
/// Two members, not three: roost's event batches are folded into the inventory
/// behind this same enum ([`shed_core::roost::fence`] is the fold) rather than
/// published as their own variant, so a consumer renders whole inventories and
/// never reconciles a patch stream. Unchanged across the R1 migration by
/// design — `machines.rs::consume` and shed-mobile's bridge did not move.
#[derive(Debug, Clone, PartialEq)]
pub enum RoostUpdate {
    /// A complete inventory. Emitted once at the head of every cycle (the
    /// `tab.list` the stream is fenced against), and afterwards only when a
    /// folded batch actually **changed a row, or retired a tab** — an empty
    /// commit, a hidden tab's churn, or a project rename that touches no session
    /// emits nothing, though the inventory's `revision` advances all the same and
    /// rides out with the next snapshot that does. A tab the inventory KNEW
    /// about disappearing is published even when it was hidden the whole time,
    /// because a client may be holding an optimistic row for it (the reason is
    /// on `observe_once`, this module's fold loop).
    Snapshot(RoostInventory),
    /// The session is not readable: no socket, the tunnel would not build, the
    /// thing on the other end is not a roost-session, or a request failed.
    ///
    /// **A normal state, not an error.** A machine that is asleep, or simply
    /// runs no roost-session, is expected — the consumer renders the row as
    /// stale-with-a-reason.
    Down {
        reason: String,
        /// What kind of "not readable" this is (plan 019 §3.6). The reason is
        /// for the user; this is for the client, which offers an install for a
        /// [`ReachKind::NotInstalled`] and a start for a
        /// [`ReachKind::NoSession`] and nothing at all for the other two.
        ///
        /// [`ReachKind::Other`] is the honest default and the common one: a
        /// stream that ended, a request that failed, a reach that never had a
        /// family. A classification only appears when the transport actually
        /// recorded one **since the last time this watcher reported** — see
        /// [`RecordedReach`].
        kind: ReachKind,
    },
}

/// A reconnecting **observer** over one roost-session's inventory.
///
/// Deliberately the same shape as [`crate::machine::MachineHubWatcher`]:
/// [`spawn`] starts the loop and hands back the receiver, [`stop`] (and `Drop`)
/// aborts it, and it is not restartable. The backoff is the same shared
/// schedule with the same reset-on-worked rule, so a roost row and a hub row in
/// one sessions view go stale at the same rate.
///
/// **Nothing here has a cadence.** Since roost R1 (session protocol 4) a
/// subscribe takes no lease and classifies instead: shed subscribes with an
/// empty one, which is an *observer* stream by construction, and every workspace
/// commit arrives as a batch. Latency is the push; the only sleep in this module
/// is the failure backoff.
///
/// [`spawn`]: RoostWatcher::spawn
/// [`stop`]: RoostWatcher::stop
pub struct RoostWatcher {
    label: String,
    task: tokio::task::JoinHandle<()>,
}

/// What a watcher does beyond watching.
///
/// Empty by default, and that is the ordinary case: a watcher over somebody
/// else's machine is a pure observer and must stay one. Only a host **shed
/// itself started a session on** gets the hooks entry, because only there does
/// shed hold a lease it is entitled to re-present.
#[derive(Default)]
pub struct RoostWatcherOptions {
    /// Re-send `session.set_agent_hooks` at the head of every successful cycle
    /// (plan 019 §3.4).
    ///
    /// **Why on every connect and not once at install time.** roost's `auto`
    /// mode wires only the agents whose config directory exists *at that
    /// moment*, so an agent the user sets up tomorrow is wired by the next call
    /// and by nothing else. roost's own UI re-sends on every connect for exactly
    /// this reason; the op is idempotent, and a session restart (every `shed
    /// start` is one) is precisely when a re-send is needed.
    pub hooks: Option<HooksRefresh>,
}

impl RoostWatcher {
    /// Spawn the connect-identify-poll-retry loop for `reach` onto `handle`.
    ///
    /// `label` is stamped on every row of every inventory this watcher emits
    /// (`RoostSession::host_label`), which is what a client turns into
    /// `origin: "machine:<label>"`. It is NOT `reach.label()` — a reach's label
    /// answers "which reach failed", and a client may well want to poll one
    /// reach under a name of its own.
    pub fn spawn(
        handle: &tokio::runtime::Handle,
        reach: Arc<dyn RoostReach>,
        label: String,
    ) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        Self::spawn_with(handle, reach, label, RoostWatcherOptions::default())
    }

    /// [`spawn`](Self::spawn) for a host whose agent hooks shed is responsible
    /// for keeping wired — i.e. one shed itself started a session on.
    pub fn spawn_with(
        handle: &tokio::runtime::Handle,
        reach: Arc<dyn RoostReach>,
        label: String,
        options: RoostWatcherOptions,
    ) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        Self::spawn_inner(handle, reach, label, options, BackoffSleeper::default())
    }

    /// [`spawn`](Self::spawn) with the backoff-sleep seam supplied — the real
    /// clock everywhere but this module's own tests.
    fn spawn_inner(
        handle: &tokio::runtime::Handle,
        reach: Arc<dyn RoostReach>,
        label: String,
        options: RoostWatcherOptions,
        sleeper: BackoffSleeper,
    ) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        let (tx, rx) = mpsc::unbounded_channel();
        let task = handle.spawn(run_loop(reach, tx, label.clone(), options, sleeper));
        (RoostWatcher { label, task }, rx)
    }

    /// The label this watcher stamps on its rows.
    pub fn label(&self) -> &str {
        &self.label
    }

    /// Abort the loop. Dropping the in-flight future closes the held
    /// connection; the reach is torn down when the last reference to it goes.
    pub fn stop(&self) {
        self.task.abort();
    }
}

impl Drop for RoostWatcher {
    fn drop(&mut self) {
        self.stop();
    }
}

/// **Where the loop's backoff sleep goes — a `cfg(test)` seam that is an EMPTY
/// struct in a normal build**, its `sleep` a plain `tokio::time::sleep`.
///
/// Lifted verbatim from [`crate::machine`]'s, and for the same reason: the reset
/// rule is only observable as *when* the next attempt happens, and the schedule
/// (500 ms → 30 s) is far too long to assert against a real clock.
#[derive(Default)]
struct BackoffSleeper {
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

/// Whether a batch reordered something, i.e. whether the snapshot's *order* is
/// now stale even though every row in it is current.
///
/// The two events name the SET that was reordered rather than a member of it
/// (`tabs.reordered` carries the project, `projects.reordered` the sidebar), so
/// neither is foldable into a row without shed keeping an ordering model of its
/// own. See the caller for why re-listing is the answer instead.
fn is_reorder(batch: &roost_ipc::messages::EventBatch) -> bool {
    batch.events.iter().any(|envelope| {
        matches!(
            envelope.event.as_str(),
            roost_ipc::messages::ops::EVENT_TABS_REORDERED
                | roost_ipc::messages::ops::EVENT_PROJECTS_REORDERED
        )
    })
}

/// How a cycle ended, when it did not end in an error.
enum Cycle {
    /// The consumer went away. There is nothing left to do.
    Done,
    /// Start over at once: re-identify, re-subscribe, re-list.
    ///
    /// **Not a failure.** The daemon is alive and we are behind it — a lost
    /// commit, a stream the server closed, a restart. No `Down`, no
    /// `invalidate`, no backoff sleep; only [`MAX_CONSECUTIVE_RESYNCS`] bounds
    /// it.
    Resync,
}

/// A `RoostError` as a reach failure with no family.
///
/// Everything a built connection can report — a socket that went quiet, a frame
/// that would not decode, a session that refused an op — describes the *wire*
/// and never the `ssh` exec behind it. Guessing a family from the wording would
/// be exactly the substring-matching this type exists to remove; the real
/// classification, when there is one, comes off the transport in
/// [`overlay_reach_reason`].
fn reach_other(error: &RoostError) -> ReachError {
    ReachError::other(error.to_string())
}

/// Replace a generic drop reason with the transport's own, when the transport
/// has recorded something **newer than this watcher has already shown**.
///
/// roost's [`overlay_ssh_reason`] in one function, and it is roost's for a
/// reason: the shape of the problem is identical on both sides. The loop only
/// ever sees its end of a local socket going quiet — "the connection closed" —
/// while the `ssh` exec behind that socket is what actually failed, and its
/// stderr was classified onto the tunnel after the fact. Turning the first
/// sentence into the second is the whole of this.
///
/// **The watermark is what keeps it honest.** `last_error` is never cleared for
/// a tunnel's life, so without `seen` the first classified failure would be
/// re-reported as the reason for every drop after it — a `no-session` recorded
/// once would keep a client offering "start a session" long after the session
/// was up and the row went down for some other reason entirely. Comparing
/// against the highest generation already reported makes the overlay say only
/// "something failed **since**".
///
/// **What it relies on in return** is [`RecordedReach::generation`]'s
/// invariant: the number is monotonic for the life of the *reach*. A watermark
/// is per watcher and a transport is not — a bridge re-establishes underneath
/// one — so a reach that let its numbering restart would have this function
/// discard the new transport's first records as ones it had "already shown",
/// which is the same stale-classification bug read backwards.
///
/// [`overlay_ssh_reason`]: https://github.com/charliek/roost/blob/c67ac27/crates/roost-iced/src/host_conn.rs
async fn overlay_reach_reason(
    reach: &dyn RoostReach,
    seen: &mut u64,
    fallback: ReachError,
) -> ReachError {
    let Some(recorded) = reach.last_error().await else {
        return fallback;
    };
    // `<=`, not `<`: a generation that has already been shown is not news,
    // however many times the transport is asked about it.
    if recorded.generation <= *seen {
        return fallback;
    }
    *seen = recorded.generation;
    recorded.error
}

async fn run_loop(
    reach: Arc<dyn RoostReach>,
    tx: mpsc::UnboundedSender<RoostUpdate>,
    label: String,
    options: RoostWatcherOptions,
    sleeper: BackoffSleeper,
) {
    let mut backoff = backoff::INITIAL;
    // Consecutive resyncs with no applied batch between them. See
    // [`MAX_CONSECUTIVE_RESYNCS`].
    let mut resyncs: u32 = 0;
    // The highest transport failure generation this watcher has already
    // reported — [`overlay_reach_reason`]'s watermark. Per watcher, because
    // "have I shown this yet" is a question about this loop's own output.
    let mut seen: u64 = 0;
    loop {
        if tx.is_closed() {
            break;
        }
        // **The reset is keyed on the connection having WORKED, not on how it
        // later ended** — the same rule `machine.rs` documents. Almost every
        // real disconnect is an `Err` (the session restarted, the ssh master
        // died, the phone changed networks), so resetting only on a clean end
        // would ratchet a healthy feed up to the 30 s ceiling and keep it there.
        let mut worked = false;
        // Set by an *applied* batch, which is the only evidence the stream is
        // actually carrying commits. A cycle's own `tab.list` is NOT progress:
        // a daemon that EOFs before every first batch would otherwise reset the
        // bound on every attempt and spin forever.
        let mut applied = false;
        let outcome = observe_once(
            &reach,
            &tx,
            &label,
            options.hooks.as_ref(),
            &mut worked,
            &mut applied,
        )
        .await;
        if worked {
            backoff = backoff::INITIAL;
        }
        if applied {
            resyncs = 0;
        }
        let failure = match outcome {
            Ok(Cycle::Done) => break,
            Ok(Cycle::Resync) => {
                resyncs += 1;
                if resyncs <= MAX_CONSECUTIVE_RESYNCS {
                    tracing::warn!(
                        label = %label,
                        attempt = resyncs,
                        "roost stream resync"
                    );
                    continue;
                }
                ReachError::other(format!(
                    "resyncing too often ({resyncs} in a row without a commit)"
                ))
            }
            Err(failure) => failure,
        };
        // **Before `invalidate`, deliberately.** The typed reason lives on the
        // held tunnel, and `invalidate` is the flag that makes the next `ensure`
        // throw that tunnel away — asking afterwards would be a race against the
        // rebuild for the one fact this failure is about.
        let down = overlay_reach_reason(reach.as_ref(), &mut seen, failure).await;
        // After ANY error, unconditionally — a bridge socket that accepts while
        // its `ssh` is gone would pass any liveness probe this could run
        // instead.
        reach.invalidate().await;
        if tx
            .send(RoostUpdate::Down {
                reason: down.message,
                kind: down.kind,
            })
            .is_err()
        {
            break;
        }
        // Entering backoff is itself a reset: the next attempt starts a fresh
        // run, and carrying the count across a `Down` would make the second
        // failure after a recovery trip the bound.
        resyncs = 0;
        let (wait, next) = backoff::step(backoff);
        backoff = next;
        // Race the sleep against the consumer going away: a session that stays
        // down delivers nothing, so a send failure alone would never be observed
        // here and an abandoned receiver would leak the task.
        tokio::select! {
            () = sleeper.sleep(wait) => {}
            _ = tx.closed() => break,
        }
    }
}

/// One observe cycle: identify, subscribe, snapshot, then fold the push feed
/// for as long as it lasts.
///
/// **Subscribe before listing, on a second connection.** The ack's `revision`
/// `s` is a fence — the first batch delivered is exactly `s + 1` — so a snapshot
/// taken *after* the ack (at some `r0 >= s`) can never be ahead of the stream:
/// batches `s+1..=r0` are discarded, `r0+1` applies, and a busy daemon produces
/// no spurious gap. List-then-subscribe on one connection would make every
/// commit landing between the two calls a `Gap` and cost a resync for nothing.
/// The price is two connections per (re)sync — over an [`SshBridge`] two remote
/// execs on a shared `ControlMaster` — paid on connect, gap, EOF and stopping,
/// never per event.
///
/// **The gate runs once per cycle**, not per event. `poll_once` re-identified on
/// every poll because a restart need not drop a polled socket; a *held stream*
/// cannot outlive its daemon, so a restart is an EOF and the next cycle
/// re-identifies. The one edge is a restart landing between conn A's identify
/// and conn B's subscribe: that snapshot carries the old `daemon_session_id`,
/// the stream EOFs immediately, and the next cycle fixes both.
///
/// `worked` is set once the cycle's first snapshot has gone out; `applied` once
/// a batch has actually been folded in. Returns `Err` only for something the
/// consumer should see as [`RoostUpdate::Down`].
async fn observe_once(
    reach: &Arc<dyn RoostReach>,
    tx: &mpsc::UnboundedSender<RoostUpdate>,
    label: &str,
    hooks: Option<&HooksRefresh>,
    worked: &mut bool,
    applied: &mut bool,
) -> Result<Cycle, ReachError> {
    // **The one error here that arrives already typed.** Everything after it is
    // a `RoostError` from a connection that was built, which has no ssh family
    // of its own — the transport's own word for those comes from
    // [`overlay_reach_reason`] on the way out.
    let endpoint = reach.ensure().await?;

    // Conn A — the gate. `NotASession` and `ProtocolMismatch` are `Down` reasons
    // that name themselves, so this is also what keeps a roost UI socket or an
    // un-upgraded daemon from ever being read as machine inventory.
    let mut conn = Conn::endpoint(&endpoint)
        .await
        .map_err(|e| reach_other(&e))?;
    let identify = conn.session_identify().await.map_err(|e| reach_other(&e))?;

    // **The hooks re-send, at the head of a cycle that has proved the session is
    // there and speaks shed's protocol.** It runs on conn A, which the prologue
    // below is about to finish with — and its result is never allowed to end
    // the cycle: a refused hooks call is a missing enrichment, not an
    // unreadable host. It has no data dependency on conn B (below), so the two
    // dial/refresh concurrently rather than paying the hooks round trip before
    // conn B's dial even starts.
    let hooks_fut = async {
        if let Some(hooks) = hooks {
            hooks.refresh(&mut conn).await;
        }
    };

    // Conn B — the observer stream. **An empty lease is an observer by
    // construction on roost's side**, not merely by serde default: it builds the
    // presented lease with `(!lease.is_empty()).then(…)` and requires a
    // non-empty one to classify a driver. So this takes nothing from whoever is
    // driving the session, and a takeover reclassifies rather than ends it.
    let subscribe_fut = async {
        let subscriber = Conn::endpoint(&endpoint)
            .await
            .map_err(|e| reach_other(&e))?;
        subscriber.subscribe("").await.map_err(|e| reach_other(&e))
    };

    let (_, stream_result) = tokio::join!(hooks_fut, subscribe_fut);
    let mut stream = stream_result?;

    // Conn A again — the snapshot the stream is fenced against — and then conn A
    // is done: everything after this comes off the push feed.
    let list = conn.tab_list().await.map_err(|e| reach_other(&e))?;
    drop(conn);

    let mut inventory = RoostInventory::from_list(label, &list, &identify);
    // A session socket always carries the revision; the ack's is the honest
    // fallback rather than a panic, and the gate above has already refused the
    // one socket (a UI socket) that omits it.
    let mut fence = Fence::new(list.revision.unwrap_or_else(|| stream.revision()));
    *worked = true;
    if tx.send(RoostUpdate::Snapshot(inventory.clone())).is_err() {
        return Ok(Cycle::Done);
    }

    loop {
        let frame = tokio::select! {
            frame = stream.next() => frame,
            _ = tx.closed() => return Ok(Cycle::Done),
        };
        match frame {
            // **A bare EOF is a resync, never a `Down`.** It is what roost
            // produces when it drops a subscriber that fell behind ("the server
            // closes rather than thins"), and what a daemon restart looks like.
            // The cycle's first snapshot already went out, so there is no
            // never-worked case to fall through to; a dead `ssh` behind it costs
            // exactly one wasted attempt, whose `session_identify` then fails
            // properly.
            Ok(None) => return Ok(Cycle::Resync),
            // **The gap surfaces here, not from the fence.** `EventStream::next`
            // validates the revision sequence against its own ack before it
            // yields, so a skipped commit is this error rather than an
            // `Admit::Gap` below — which stays as a defensive second layer.
            Err(RoostError::RevisionGap { expected, got }) => {
                tracing::warn!(
                    label = %label,
                    expected,
                    got,
                    "roost event stream skipped a revision"
                );
                return Ok(Cycle::Resync);
            }
            Err(e) => return Err(reach_other(&e)),
            Ok(Some(EventFrame::Batch(batch))) => match fence.admit(batch.revision) {
                // Everything at or below the snapshot — the `s+1..=r0` the
                // prologue's ordering deliberately produces.
                Admit::Discard => {}
                Admit::Apply => {
                    *applied = true;
                    let before = inventory.sessions.clone();
                    let known_before = inventory.known_tab_ids();
                    inventory.apply(&batch);
                    // **A row change is news. So is a tab vanishing.** An empty
                    // commit, a hidden tab's churn and a project rename nobody's
                    // row carries all advance the revision inside the inventory
                    // and publish nothing; the next snapshot that does go out
                    // carries the moved number with it.
                    //
                    // The one hidden-half change that IS news is a tab we knew
                    // about ceasing to exist. A client may hold an optimistic
                    // row for a tab it opened itself, before any adapter has
                    // claimed it (`machines.rs::create`); if the launched
                    // process dies before it ever reports, BOTH the `tab.opened`
                    // and the `tab.closed` touch the hidden half only, and
                    // without this the client would never hear that the tab it
                    // is showing a card for is gone. The poller repaired that on
                    // its next list; an event-only watcher has to say it.
                    // Deliberately narrow: a hidden tab appearing, or changing,
                    // still emits nothing.
                    let vanished = known_before.iter().any(|id| !inventory.knows(*id));
                    if (inventory.sessions != before || vanished)
                        && tx.send(RoostUpdate::Snapshot(inventory.clone())).is_err()
                    {
                        return Ok(Cycle::Done);
                    }
                    // **A reorder is a resync, deliberately the cheap way.**
                    // `RoostInventory::apply` folds no ordering — rows are
                    // keyed by tab id and carried in list order, and modelling
                    // roost's two reorder events would mean re-deriving a
                    // sequence shed does not otherwise own. In the poll era a
                    // stale order fixed itself within one 2 s tick; an
                    // event-only watcher would keep it until some unrelated
                    // resync, which is a user dragging a tab and watching
                    // nothing happen. A re-list is exactly what restores the
                    // order, so take one. Reorders are rare and user-driven,
                    // and the batch we just applied has already reset the
                    // resync bound, so this cannot spin.
                    if is_reorder(&batch) {
                        tracing::debug!(
                            label = %label,
                            revision = batch.revision,
                            "roost reordered; re-listing for the new order"
                        );
                        return Ok(Cycle::Resync);
                    }
                }
                Admit::Gap { expected, got } => {
                    tracing::warn!(
                        label = %label,
                        expected,
                        got,
                        "roost batch is past the fence"
                    );
                    return Ok(Cycle::Resync);
                }
            },
            // Informational: somebody else took the interactive lease. The
            // stream survives it (that is the whole R1 re-cut) and shed never
            // held the lease in the first place, so there is nothing to do but
            // say so.
            Ok(Some(EventFrame::DriverChanged(changed))) => {
                tracing::debug!(
                    label = %label,
                    taken_by = %changed.taken_by,
                    "roost driver changed"
                );
            }
            // The one terminal envelope an event stream can see at protocol 4.
            // The daemon is going away, so this is a `Down` with a reason and
            // not a resync.
            Ok(Some(EventFrame::Stopping(stopping))) => {
                return Err(ReachError::other(format!(
                    "session stopping: {}",
                    stopping.reason
                )));
            }
        }
    }
}

// ---------------------------------------------------------------------------
// one-shots
// ---------------------------------------------------------------------------

/// Dial through a reach, invalidating it if the dial fails.
async fn dial(reach: &dyn RoostReach) -> Result<Conn, String> {
    let endpoint = match reach.ensure().await {
        Ok(endpoint) => endpoint,
        Err(e) => {
            reach.invalidate().await;
            return Err(e.to_string());
        }
    };
    finish(reach, Conn::endpoint(&endpoint).await).await
}

/// A request's outcome, with the reach invalidated when it failed — after ANY
/// error, unconditionally, for the reason in the module doc.
async fn finish<T>(reach: &dyn RoostReach, result: Result<T, RoostError>) -> Result<T, String> {
    match result {
        Ok(value) => Ok(value),
        Err(e) => {
            reach.invalidate().await;
            Err(e.to_string())
        }
    }
}

/// Whether a failed request means **the wire** is suspect, rather than the
/// request.
///
/// A one-shot does not need this — it dials per call, so invalidating after any
/// error costs it nothing. A held connection does: it is the only caller that
/// can be told "that tab is gone" by a transport that is working perfectly, and
/// tearing the transport down for that would make every stale tab id cost an
/// `ssh` re-establish.
///
/// Matched exhaustively on purpose: a new [`RoostError`] variant should fail to
/// compile here rather than default into either answer.
fn is_transport_error(error: &RoostError) -> bool {
    match error {
        // Nothing is there, the frame did not decode, or the stream lost a
        // revision. All three say the connection cannot be trusted for the next
        // request, whatever the reach thinks it is holding.
        RoostError::Unavailable(_) | RoostError::Wire(_) | RoostError::RevisionGap { .. } => true,
        // A refusal the session MINTED is proof the wire works end to end: it
        // was read, dispatched, and answered. `not-found` on a closed tab is the
        // common one, and it is not a reason to rebuild anything.
        RoostError::Server { .. } => false,
        // The gate's two. They describe the peer, not the pipe — a reconnect
        // reaches the same wrong thing.
        RoostError::ProtocolMismatch { .. } | RoostError::NotASession => false,
    }
}

/// `tab.open` — start an agent in a new tab and get the tab back.
///
/// One connection per call, deliberately: over SSH each is a fresh remote exec,
/// which is the honest cost of a one-shot and is why the *watcher* holds one
/// connection instead.
pub async fn tab_open(reach: &dyn RoostReach, params: TabOpenParams) -> Result<Tab, String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_open(params).await).await
}

/// `tab.close` — end a tab. It leaves `tab.list` entirely.
pub async fn tab_close(reach: &dyn RoostReach, tab_id: i64) -> Result<(), String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_close(tab_id).await).await
}

/// `tab.dump` — one tab's viewport as text. For a repeated peek use
/// [`RoostPeek`], which holds the connection.
pub async fn tab_dump(reach: &dyn RoostReach, tab_id: i64) -> Result<TabDumpResult, String> {
    let mut conn = dial(reach).await?;
    finish(reach, conn.tab_dump(tab_id).await).await
}

// There is no `tab_write` one-shot here. A write is lease-gated at session
// protocol 4, so it is not a one-shot at all: a caller has to hold a lease
// across the `session.connect` that minted it and the write it authorizes, and
// a per-call dial would take the lease from whoever is driving on every
// keystroke. `shed_core::roost::Conn::{session_connect, tab_write}` is the
// surface for the code that will drive a tab (A4/S4); nothing in shed calls it
// today.

/// A held connection for repeatedly dumping one tab.
///
/// The read-only terminal affordance until roost R3 lands attach. It holds ONE
/// connection for the life of the peek because the alternative — a dial per
/// frame — is a remote `ssh` exec per frame on a machine.
///
/// **Dropping it closes the connection**, which is the whole of closing a peek:
/// there is no server-side state to release (`tab.dump` is lease-free and
/// stateless), so there is nothing an explicit `close()` could do that the drop
/// does not.
///
/// ## It keeps the reach, not just the connection
///
/// A peek is the one thing in this module that survives across requests, so it
/// is the one thing that has to invalidate the reach itself. Every other caller
/// routes through [`finish`], which invalidates after any error; a peek that
/// held only its `Conn` would leave the reach believing its transport was fine.
/// Over an [`SshBridge`] that is a concrete bug: the `ssh` master dies, `dump`
/// returns [`RoostError::Unavailable`], the caller re-opens the peek, and
/// [`RoostReach::ensure`] hands back the same dead `bridge.sock` — which accepts
/// connections happily and answers nothing — forever.
///
/// It invalidates on a **transport-shaped** error only ([`is_transport_error`]):
/// a `not-found` for a tab somebody closed is the session working, not the wire
/// failing.
pub struct RoostPeek {
    /// Held so a failed [`dump`](Self::dump) can mark the transport suspect —
    /// the whole reason this is an `Arc<dyn RoostReach>` and not a `Conn` alone.
    reach: Arc<dyn RoostReach>,
    conn: Conn,
    tab_id: i64,
}

impl RoostPeek {
    /// Dial and hold. The tab is not validated here — the first
    /// [`dump`](Self::dump) is what says whether it exists.
    ///
    /// **Takes the reach by `Arc`** because the peek outlives the call: it keeps
    /// the reach for as long as it keeps the connection, to invalidate it when a
    /// frame fails on the wire. A caller holding a bare reach wraps it —
    /// `RoostPeek::open(Arc::new(FixedPort(port)), tab_id)`.
    pub async fn open(reach: Arc<dyn RoostReach>, tab_id: i64) -> Result<RoostPeek, String> {
        let conn = dial(reach.as_ref()).await?;
        Ok(RoostPeek {
            reach,
            conn,
            tab_id,
        })
    }

    /// The tab being peeked at.
    pub fn tab_id(&self) -> i64 {
        self.tab_id
    }

    /// One frame.
    ///
    /// Returns the typed [`RoostError`] rather than a string: a peek loop wants
    /// to tell "that tab is gone" (`Server { code: "not-found" }` — stop) from
    /// "the wire died" (`Unavailable` — re-open), and a rendered message makes
    /// that a spelling comparison.
    ///
    /// A wire failure also **invalidates the reach** on the way out, so the
    /// re-open the caller is about to do rebuilds the transport instead of
    /// being handed the dead one back.
    pub async fn dump(&mut self) -> Result<TabDumpResult, RoostError> {
        match self.conn.tab_dump(self.tab_id).await {
            Ok(dump) => Ok(dump),
            Err(e) => {
                if is_transport_error(&e) {
                    self.reach.invalidate().await;
                }
                Err(e)
            }
        }
    }
}

// ---------------------------------------------------------------------------
// the exec runner
// ---------------------------------------------------------------------------

/// `ssh`'s own connect budget, and roost's provider phase budget. The one part
/// of a step this side cannot bound itself, so it is bounded there.
const CONNECT_TIMEOUT_SECS: u32 = 5;

/// How long a `ControlMaster` outlives its last connection. roost's own number
/// ([`mux_connect_argv`](roost_ipc::ssh)) — a bootstrap is a dozen execs over
/// a minute or two, and paying a fresh handshake for each of them turns a
/// 20-second install into a 2-minute one.
const CONTROL_PERSIST: &str = "60s";

/// How much of a failed exec's stderr is kept. Only the LAST bytes are worth
/// keeping: a failing `ssh` puts its banner first and its diagnosis last.
const STDERR_TAIL_CAP: usize = 4 * 1024;

/// Runs the bootstrap machines' [`Step::Exec`] steps on a far side, over an
/// `ssh` subprocess on a **private `ControlMaster`** (plan 019 §3.6).
///
/// ## Why this is not the bridge
///
/// [`SshBridge`] is roost's transport, and roost's transport runs exactly one
/// remote command: the candidate-ladder exec chain that ends in `roost-session
/// client-bridge`. That is the whole of its job and it is not parameterized —
/// there is no seam in it through which a `/bin/sh -s` carrying roost's prepare
/// script could be run. So a host shed is bootstrapping has **two ssh stacks**
/// for the duration: the bridge, reading whatever session is there, and this,
/// running the scripts that put one there.
///
/// Plan 019 §3.6 records that as an accepted consequence rather than an
/// oversight, with the unification named as future work. What makes it
/// tolerable rather than merely accepted is that the two share their
/// *configuration* — [`pinned_config`] builds one host-key posture and both
/// stacks run under it — so the duplication is a second process, not a second
/// set of rules about who this host is.
///
/// ## The scratch directory
///
/// Holds the generated `ssh_config` and the control socket, at 0700, and is
/// **removed when this value is dropped** ([`ScratchDir`]), with the master
/// asked to exit first so a `ControlPersist` window cannot outlive the app that
/// opened it. [`SshExec::shutdown`] is the same teardown for a client that can
/// await it (app quit); `Drop` is the one for a client that cannot, which is
/// roost's own arrangement for the same problem.
///
/// The directory name is NOT in roost's `roost-ssh-*` namespace: roost sweeps
/// that namespace on its own schedule, and a live control socket for an install
/// that is mid-stream is exactly the wrong thing to have swept.
pub struct SshExec {
    /// The reach's name in the target grammar — what a failure says.
    label: String,
    /// ssh's destination word: `[user@]host`.
    dest: String,
    port: u16,
    /// Whether the port is forced on the command line. False for 22, so a
    /// `~/.ssh/config` `Port` for that host still wins — `shed_core::machine`'s
    /// rule, and the Go provider's.
    emit_port: bool,
    ssh_bin: PathBuf,
    config_path: PathBuf,
    ctl_path: PathBuf,
    /// Owned for its `Drop`. Holds both paths above.
    dir: ScratchDir,
    /// The host-key pin's own directory, when the entry has one. Never read —
    /// owned because [`generate_ssh_config`](roost_ipc::ssh::generate_ssh_config)
    /// baked its path into the config `-F` points at, so it has to outlive that
    /// config and be removed with it.
    _pin_dir: Option<ScratchDir>,
}

impl SshExec {
    /// Build a runner for one entry — a `machines:` entry, or a shed's
    /// [`shed_reach_entry`]. Nothing is spawned until [`SshExec::run`].
    ///
    /// `options` is [`SshBridge`]'s, and the parts it uses are the parts that
    /// decide *who this host is*: the `ssh` binary and the `ssh_config` files.
    /// The scratch parents and the tunnel opener are the bridge's alone.
    pub fn new(entry: &MachineEntry, options: &SshBridgeOptions) -> Result<SshExec, ReachError> {
        let label = target_token(&entry.name).into_owned();
        let fail = |msg: String| ReachError::other(format!("{label}: {msg}"));
        let host = target_host(entry);
        if host.trim().is_empty() {
            return Err(fail("has no host to reach".to_string()));
        }
        // ssh parses options before it reads the destination, so a destination
        // that begins with a dash reaches it as a flag. The bridge is protected
        // from this by roost's `classify`, which refuses the same shape; this
        // path has no URL form to be refused by, so it refuses here.
        if host.starts_with('-') || entry.user.as_deref().is_some_and(|u| u.starts_with('-')) {
            return Err(fail(format!(
                "an ssh destination may not begin with a dash ({host})"
            )));
        }
        let dest = match entry.user.as_deref().filter(|u| !u.is_empty()) {
            Some(user) => format!("{user}@{host}"),
            None => host,
        };

        // A SHORT scratch name. The control socket is an `AF_UNIX` path and
        // `sun_path` is 104 bytes on macOS; the bridge's own directories are
        // parented under `/tmp` for that reason and this one is parented under
        // `std::env::temp_dir()`, which on macOS is a ~50-byte
        // `/var/folders/…/T/`. Eight characters of the host id plus a one-letter
        // socket keeps the whole path comfortably inside the budget on both
        // platforms.
        let id = host_id(&entry.name);
        let short: String = id.chars().take(8).collect();
        let dir = ScratchDir::new(&format!("x{short}"))
            .map_err(|e| fail(format!("could not create a scratch dir: {e}")))?;

        let base = options
            .config_paths
            .clone()
            .unwrap_or_else(SshConfigPaths::from_env);
        let pinned = pinned_config(entry, &id, base)?;
        // roost's own generator, so the include order + `Host *` defaults an
        // exec runs under are the ones a bridge connection runs under.
        let config_path = dir.0.join("ssh_config");
        let body = roost_ipc::ssh::generate_ssh_config(
            pinned.paths.user.as_deref(),
            pinned.paths.system.as_deref(),
        );
        write_private(&config_path, body.as_bytes())
            .map_err(|e| fail(format!("could not write {}: {e}", config_path.display())))?;

        Ok(SshExec {
            label,
            dest,
            port: entry.ssh_port,
            emit_port: entry.ssh_port != 0 && entry.ssh_port != 22,
            ssh_bin: options.ssh_bin.clone().unwrap_or_else(|| "ssh".into()),
            ctl_path: dir.0.join("c"),
            config_path,
            dir,
            _pin_dir: pinned.dir,
        })
    }

    /// The reach's name in the target grammar.
    pub fn label(&self) -> &str {
        &self.label
    }

    /// The full argv for one exec, argv[0] excluded (it is [`Self::ssh_bin`]).
    ///
    /// Pure and reachable from a test so the wire shape is asserted directly
    /// rather than inferred from a fake `ssh`'s behaviour: a test that only
    /// checked "the command ran" would pass with the `ControlPath` silently
    /// dropped, and every step would then pay its own handshake with nobody the
    /// wiser.
    ///
    /// `-T` is not decoration. A `Step::Exec`'s stdout is parsed as NUL-delimited
    /// records and its stdin is a script or a binary; a PTY would echo the input
    /// back into the output, translate LF to CRLF, and corrupt both. `-T` on the
    /// command line also beats a `RequestTTY force` in the user's own config,
    /// which `-o RequestTTY=no` alone would not.
    fn argv(&self, command: &str) -> Vec<String> {
        let mut argv = vec![
            "-F".to_string(),
            self.config_path.display().to_string(),
            "-S".to_string(),
            self.ctl_path.display().to_string(),
            "-o".to_string(),
            "ControlMaster=auto".to_string(),
            "-o".to_string(),
            format!("ControlPersist={CONTROL_PERSIST}"),
            "-o".to_string(),
            "BatchMode=yes".to_string(),
            "-o".to_string(),
            format!("ConnectTimeout={CONNECT_TIMEOUT_SECS}"),
            "-T".to_string(),
        ];
        if self.emit_port {
            argv.push("-p".to_string());
            argv.push(self.port.to_string());
        }
        // `--` ends option parsing so a remote command beginning with a dash is
        // data. It does NOT protect the destination — ssh has already parsed
        // options by the time it reads this — which is what the dash check in
        // `new` is for.
        argv.push(self.dest.clone());
        argv.push("--".to_string());
        argv.push(command.to_string());
        argv
    }

    /// The ssh binary this runner execs.
    pub fn ssh_bin(&self) -> &Path {
        &self.ssh_bin
    }

    /// Run one [`Step::Exec`] and answer with its [`Outcome`].
    ///
    /// ## `exit: None` is only ever a real timeout here
    ///
    /// The machines treat `exit: None` as a failure, with one exception a
    /// *runner* may apply: a transport that reported a clean EOF for a step
    /// whose stdout was read to the end may report `Some(0)`. That exception is
    /// mobile's, for `dartssh2` channels that close without an `exit-status`
    /// message. **A subprocess always has a status**, so this runner never
    /// applies it: the only `None` it produces is a budget that genuinely
    /// expired, and reporting that as success would turn a half-written install
    /// into a claimed one.
    pub async fn run(
        &self,
        command: &str,
        stdin: Stdin,
        budget: Duration,
        stdout_cap: usize,
        capture_stdout: bool,
    ) -> Outcome {
        match tokio::time::timeout(
            budget,
            self.run_unbounded(command, stdin, stdout_cap, capture_stdout),
        )
        .await
        {
            Ok(outcome) => outcome,
            // The child is killed by `run_unbounded`'s future being dropped —
            // `tokio::process::Child` kills on drop only with `kill_on_drop`,
            // which `spawn_child` sets for exactly this.
            Err(_) => Outcome::Exec {
                exit: None,
                stdout: Vec::new(),
                stderr_tail: format!(
                    "{}: the remote step did not finish within {}s",
                    self.label,
                    budget.as_secs()
                ),
            },
        }
    }

    async fn run_unbounded(
        &self,
        command: &str,
        stdin: Stdin,
        stdout_cap: usize,
        capture_stdout: bool,
    ) -> Outcome {
        use tokio::io::AsyncReadExt as _;
        use tokio::io::AsyncWriteExt as _;

        let mut child = match self.spawn_child(command) {
            Ok(child) => child,
            Err(e) => {
                return Outcome::Exec {
                    exit: None,
                    stdout: Vec::new(),
                    stderr_tail: format!("{}: could not run ssh: {e}", self.label),
                }
            }
        };

        let mut sink = child.stdin.take().expect("the child's stdin is piped");
        let mut out = child.stdout.take().expect("the child's stdout is piped");
        let mut err = child.stderr.take().expect("the child's stderr is piped");

        // **All three bands concurrently.** A stream step feeds tens of
        // megabytes into a pipe whose reader is the far side's `tee`; writing it
        // all before reading a byte of stdout or stderr deadlocks the moment the
        // far side says anything at all, because its 64 KiB pipe buffer fills
        // and it stops draining ours.
        let writer = async move {
            let result = match stdin {
                Stdin::Empty => Ok(()),
                Stdin::Bytes(bytes) => sink.write_all(&bytes).await,
                Stdin::Source(source) => stream_source(&mut sink, &source).await,
            };
            // The close is the EOF the far side is waiting for — `sh -s` reads
            // its program until then, and `tee` its input. Dropping `sink` does
            // it too; shutting down first surfaces a write error that a bare
            // drop would swallow.
            let _ = sink.shutdown().await;
            drop(sink);
            result
        };
        let reader = async {
            let mut buffer = Vec::new();
            if capture_stdout {
                // `take` rather than a read-and-truncate: a far side that
                // decides to `cat` something must not be able to make this side
                // allocate without bound. A step whose answer was cut fails its
                // own parse, which is the correct outcome — roost's parsers
                // refuse output that does not end in its NUL.
                let _ = (&mut out)
                    .take(stdout_cap as u64)
                    .read_to_end(&mut buffer)
                    .await;
            }
            // Drained either way: an unread stdout is a pipe that fills and a
            // far side that blocks forever.
            let mut sink = tokio::io::sink();
            let _ = tokio::io::copy(&mut out, &mut sink).await;
            buffer
        };
        let stderr = rolling_tail(&mut err, STDERR_TAIL_CAP);

        let (written, stdout, stderr) = tokio::join!(writer, reader, stderr);
        let status = child.wait().await;

        // Already at most `STDERR_TAIL_CAP` bytes — [`rolling_tail`] applied the
        // bound while reading rather than afterwards. What is left for `tail`
        // here is the lossy decode and the byte-boundary cut.
        let mut stderr_tail = tail(&stderr, STDERR_TAIL_CAP);
        // A broken pipe here is the ordinary way a *failing* remote step ends —
        // the far side exited while this side was still feeding it — so it is
        // never a verdict of its own. It is worth one line of evidence when
        // there is nothing else, and worth nothing when the far side already
        // said why.
        if let Err(e) = written {
            if stderr_tail.trim().is_empty() {
                stderr_tail = format!("{}: sending stdin failed: {e}", self.label);
            }
        }
        Outcome::Exec {
            exit: status.ok().and_then(|status| status.code()),
            stdout,
            stderr_tail,
        }
    }

    fn spawn_child(&self, command: &str) -> std::io::Result<tokio::process::Child> {
        tokio::process::Command::new(&self.ssh_bin)
            .args(self.argv(command))
            // **`LC_ALL=C`.** Failures on this path are classified by English
            // substring (roost's `classify_ssh_failure`), so a non-English
            // locale silently demotes every classified family to the generic
            // one. The Go provider sets the same variable for the same reason.
            .env("LC_ALL", "C")
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            // What makes the budget in `run` a kill and not merely a give-up.
            .kill_on_drop(true)
            .spawn()
    }

    /// Ask the `ControlMaster` to exit. Idempotent; safe to call on a runner
    /// that never opened one.
    ///
    /// The explicit half of the teardown, for app quit. `Drop` does the same
    /// thing blockingly for every other way a runner can end.
    pub async fn shutdown(&self) {
        if !self.ctl_path.exists() {
            return;
        }
        let _ = tokio::process::Command::new(&self.ssh_bin)
            .args(self.exit_argv())
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true)
            .status()
            .await;
    }

    /// `-O exit`: ask the running master to close, taking every multiplexed
    /// connection with it. No remote command — this execs nothing on the far
    /// side.
    fn exit_argv(&self) -> Vec<String> {
        vec![
            "-F".to_string(),
            self.config_path.display().to_string(),
            "-S".to_string(),
            self.ctl_path.display().to_string(),
            "-O".to_string(),
            "exit".to_string(),
            "-o".to_string(),
            "BatchMode=yes".to_string(),
            self.dest.clone(),
        ]
    }
}

impl Drop for SshExec {
    fn drop(&mut self) {
        // **Blocking, and on purpose** — the same arrangement roost's own
        // `SshTunnel::Drop` makes, for the same reason: there is no way to await
        // in a `Drop`, and the alternative is a `ControlPersist` master that
        // outlives the process that opened it by a minute, holding a connection
        // to somebody's shed with nothing left to close it. `-O exit` against a
        // live local socket is a round trip to a process on this machine.
        if self.ctl_path.exists() {
            let _ = std::process::Command::new(&self.ssh_bin)
                .args(self.exit_argv())
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status();
        }
        // `self.dir`'s own `Drop` removes the directory, control socket and all,
        // immediately after this returns.
        let _ = &self.dir;
    }
}

/// Pump a verified descriptor into a child's stdin, a chunk at a time.
///
/// Chunked rather than `read_to_end` + one write because the thing being pumped
/// is a whole `roost-session` binary — tens of megabytes — and the point of
/// [`SourceHandle`] is that nothing has to hold it in memory to send it. The
/// read is blocking (it is a `std::fs::File`) but each chunk is small and the
/// write between them yields, which is what keeps the other two bands moving.
async fn stream_source(
    sink: &mut tokio::process::ChildStdin,
    source: &SourceHandle,
) -> std::io::Result<()> {
    use tokio::io::AsyncWriteExt as _;

    const CHUNK: usize = 64 * 1024;
    loop {
        let chunk = source.read_chunk(CHUNK)?;
        if chunk.is_empty() {
            return Ok(());
        }
        sink.write_all(&chunk).await?;
    }
}

/// Drain a reader to EOF **while holding at most its last `cap` bytes**.
///
/// The same defect the C5 review found on the sibling rung's local `identify`,
/// on the other band: `read_to_end` into a `Vec` that is trimmed afterwards
/// caps nothing at all. A remote step that streams to stderr — a `tee` shouting
/// about a full disk once per block, a chatty `sh -x`, a far side that simply
/// will not stop — grows that allocation at pipe throughput for the whole step
/// budget, and the 4 KiB in [`STDERR_TAIL_CAP`] describes only what survived
/// the trim.
///
/// **The bound is applied here, per chunk, and the fix is NOT `take(cap)`**,
/// which is what the identify path could use. Two reasons it cannot be:
///
/// * the wanted bytes are the LAST ones ([`tail`] explains why), so a reader
///   cut off at the front keeps the far side's banner and loses the diagnosis;
/// * closing this pipe early is a very different act from closing a candidate
///   binary's stdout. It would hand the running `ssh` an `EPIPE`/`SIGPIPE` on
///   its own diagnostics mid-install, which is neither something to do to a
///   step that is mid-commit nor something whose exit status would still mean
///   what this runner reads it as.
///
/// So everything is read and only `cap` bytes are ever kept: the high-water
/// mark is `cap + CHUNK`, whatever the far side sends.
async fn rolling_tail<R>(reader: &mut R, cap: usize) -> Vec<u8>
where
    R: tokio::io::AsyncRead + Unpin,
{
    use tokio::io::AsyncReadExt as _;

    const CHUNK: usize = 8 * 1024;

    let mut held: Vec<u8> = Vec::new();
    let mut chunk = vec![0u8; CHUNK];
    // A read error ends the loop rather than reporting: the step's own exit
    // status is what says whether it worked, and the bytes already held are
    // still the best evidence there is.
    while let Ok(read) = reader.read(&mut chunk).await {
        if read == 0 {
            break;
        }
        held.extend_from_slice(&chunk[..read]);
        if held.len() > cap {
            held.drain(..held.len() - cap);
        }
    }
    held
}

/// The last `cap` bytes of `text`, as a lossy `String`.
///
/// The **last**, because that is where a diagnosis is: an `ssh` that fails
/// prints the far side's login banner first and its own error last, and a cap
/// applied to the front would keep the banner and lose the sentence. Cut on a
/// byte boundary and decoded lossily, so a multi-byte character split by the cut
/// costs one replacement character rather than the whole tail.
fn tail(bytes: &[u8], cap: usize) -> String {
    let start = bytes.len().saturating_sub(cap);
    String::from_utf8_lossy(&bytes[start..]).into_owned()
}

// ---------------------------------------------------------------------------
// the lease table
// ---------------------------------------------------------------------------

/// The in-memory lease table plan 019 §3.4 pins: **one lease per target, for the
/// app run**.
///
/// roost's lease is a bearer token that **outlives the connection that minted
/// it** — a reconnect is a takeover, not a resumption — so it cannot live on a
/// `Conn` and it must not be re-minted per use: `session.connect {takeover:
/// false}` against a live lease answers `already-connected` *even when the
/// caller is the holder*, so a client that re-minted on every reconnect would
/// lock itself out of its own session on the second try.
///
/// What it is for is one behaviour: re-sending `session.set_agent_hooks` on
/// every watcher (re)connect, because `mode: auto` only wires the agents whose
/// config directory exists at that moment. What ends it is `taken-over` —
/// whoever took the lease wires the hooks themselves, and shed stops. **Nothing
/// here ever takes a lease over.**
///
/// Deliberately not persisted. A lease is daemon-lifetime state; a token
/// written to disk would be a stale one the next run tried to present, and the
/// session that minted it is gone anyway.
///
/// ## The dialogue is a transaction, and the map's lock is not what makes it one
///
/// "Is shed still entitled, with which token, and send" is **one decision**, and
/// the mutex around the map protects only its three separate steps. Two watchers
/// on the same target (a card refresh racing a reconnect) could interleave into
/// exactly the outcome plan 019 §3.4 forbids: A reads `active` and the token, B
/// completes a dialogue that came back `taken-over` and records the permanent
/// surrender, and A — deciding on a fact that is now false — presents the
/// surrendered lease anyway. Whoever took the session over would then be fought
/// for it by a client that had already stepped back.
///
/// So every read-decide-send for one target runs under that target's
/// [`gate`](RoostLeases::gate), and the decision itself is the single locked
/// read [`armed`](RoostLeases::armed) rather than `active` followed by `lease`.
/// The gate is per target, not global: a dialogue holds it across a network
/// round trip, and one unreachable host must not stall every other host's
/// refresh.
#[derive(Default)]
pub struct RoostLeases {
    inner: std::sync::Mutex<std::collections::HashMap<String, LeaseEntry>>,
}

/// One target's lease, whether shed is still entitled to it, and the gate that
/// serializes the dialogues about it.
#[derive(Debug, Clone, Default)]
struct LeaseEntry {
    /// The bearer token, while it is still good.
    lease: Option<String>,
    /// Set by `taken-over`: somebody else is driving, and they wire the hooks.
    /// Terminal for the app run — a shed that re-armed on the next reconnect
    /// would be fighting a user's roost UI for their own session.
    ///
    /// The one thing that clears it is [`RoostLeases::forget`], which is not a
    /// re-arm: it says the session this was decided about is gone.
    surrendered: bool,
    /// Held for the length of one read-decide-send about this target. An
    /// `async` mutex because it is held across the wire call — that is the
    /// point — and an `Arc` so a holder does not borrow the map it is about to
    /// need again.
    gate: Arc<tokio::sync::Mutex<()>>,
}

impl RoostLeases {
    pub fn new() -> RoostLeases {
        RoostLeases::default()
    }

    /// The lease held for `target`, if shed is still holding one.
    pub fn lease(&self, target: &str) -> Option<String> {
        self.inner
            .lock()
            .expect("the roost lease table")
            .get(target)
            .and_then(|entry| entry.lease.clone())
    }

    /// Whether shed should still be re-sending hooks for `target`.
    pub fn active(&self, target: &str) -> bool {
        self.armed(target).is_some()
    }

    /// The lease to present **and** the entitlement to present it, read
    /// together under one lock.
    ///
    /// The atomic form of `active(t).then(|| lease(t))`, and the only form a
    /// sender may use: between those two calls a `taken-over` can land, and the
    /// caller would then send a token it has just been told to stop sending.
    fn armed(&self, target: &str) -> Option<String> {
        let held = self.inner.lock().expect("the roost lease table");
        held.get(target)
            .filter(|entry| !entry.surrendered)
            .and_then(|entry| entry.lease.clone())
    }

    /// This target's serialization gate — see the type's doc.
    ///
    /// Taken by **every** path that presents a lease ([`HooksRefresh::refresh`]
    /// and [`BootstrapRunner::hooks`]), so a surrender recorded by one of them
    /// is visible to the next before it decides, rather than after it has sent.
    fn gate(&self, target: &str) -> Arc<tokio::sync::Mutex<()>> {
        self.inner
            .lock()
            .expect("the roost lease table")
            .entry(target.to_string())
            .or_default()
            .gate
            .clone()
    }

    /// Fold one dialogue's result in.
    ///
    /// Three outcomes, and they are not the same:
    ///
    /// * a lease came back — keep it, and keep re-sending;
    /// * `taken-over` — surrender, permanently for this run;
    /// * anything else with no lease (`already-connected` at the first connect,
    ///   a dead connection) — **forget the token and leave the door open**. The
    ///   first is somebody else driving *right now*, which a later reconnect may
    ///   well find over; the second is a transport failure that says nothing
    ///   about entitlement at all.
    ///
    /// That third row is a *clear*, not a no-op, and the difference is a real
    /// session: a token is only ever good for the session that minted it, and
    /// every result that came back without one is evidence that the session
    /// shed's token belongs to did not accept it (or is no longer there to).
    /// Keeping it would have the watcher present a dead session's lease on
    /// every reconnect for the rest of the run — a wasted `connect-required`
    /// round trip each time, and an `active` target that is nothing of the kind.
    pub fn record(&self, target: &str, result: &HooksResult) {
        let mut held = self.inner.lock().expect("the roost lease table");
        let entry = held.entry(target.to_string()).or_default();
        if result.skipped_code.as_deref() == Some("taken-over") {
            entry.lease = None;
            entry.surrendered = true;
            return;
        }
        // `clone`, not `take`-if-present: `None` IS the answer here.
        //
        // A surrendered entry keeps nothing either, whatever came back: the
        // table would then be holding a token it has already promised not to
        // present, which is the same stale-token shape one line further on.
        // Only [`RoostLeases::forget`] ends a surrender, and it clears both.
        entry.lease = (!entry.surrendered).then(|| result.lease.clone()).flatten();
    }

    /// Drop what is known about `target` — a host that was removed, or one
    /// whose session shed has just replaced.
    ///
    /// **Including a surrender**, which is what distinguishes this from a
    /// `record` that came back empty: surrendering is a decision about one
    /// session's lease, and this says that session is gone. The caller in this
    /// module is [`BootstrapRunner::install`], where the user has just
    /// consented to shed starting another one.
    ///
    /// The target's [`gate`](RoostLeases::gate) survives: it is identity, not
    /// state. Removing it would hand the next caller a *different* mutex from
    /// the one a dialogue is holding right now, and the two would stop
    /// excluding each other at exactly the moment it matters.
    pub fn forget(&self, target: &str) {
        let mut held = self.inner.lock().expect("the roost lease table");
        if let Some(entry) = held.get_mut(target) {
            entry.lease = None;
            entry.surrendered = false;
        }
    }
}

/// The watcher's standing instruction to keep one host's agent hooks wired.
///
/// Cloneable and cheap: the table is shared, the two strings are the target's
/// grammar token and shed's own client label.
#[derive(Clone)]
pub struct HooksRefresh {
    pub leases: Arc<RoostLeases>,
    /// The target grammar token — the table's key, and what the copy says.
    pub target: String,
    /// `shed-desktop` / `shed-mobile`. Becomes the lease's label, so a user who
    /// finds their session driven can see who is driving it.
    pub client_label: String,
}

impl HooksRefresh {
    /// Re-present the held lease and re-send `session.set_agent_hooks`.
    ///
    /// A **no-op unless shed is still holding a lease for this target** — this
    /// is a refresh, not a first wiring. The first one happens inside
    /// [`BootstrapRunner::hooks`], after a Start that shed itself performed,
    /// which is the only moment shed is entitled to connect at all: a session
    /// that was already running belongs to whoever started it.
    ///
    /// **One transaction.** The turn is taken before the entitlement is read
    /// and held until the result is folded back in, so a `taken-over` recorded
    /// by another watcher on this target cannot land between this one's
    /// decision and its send. Without it, surrender — which §3.4 makes
    /// permanent — could be overtaken by a dialogue that had already read
    /// `active` and was merely slow ([`RoostLeases`]).
    async fn refresh(&self, conn: &mut Conn) {
        let gate = self.leases.gate(&self.target);
        let _turn = gate.lock().await;
        let Some(lease) = self.leases.armed(&self.target) else {
            return;
        };
        let result = wire_agent_hooks(conn, &self.client_label, Some(&lease)).await;
        if let Some(code) = &result.skipped_code {
            tracing::debug!(target = %self.target, code = %code, "roost hooks refresh skipped");
        } else if let Some(error) = &result.error {
            tracing::warn!(target = %self.target, error = %error, "roost hooks refresh failed");
        }
        self.leases.record(&self.target, &result);
    }
}

// ---------------------------------------------------------------------------
// driving the bootstrap machines
// ---------------------------------------------------------------------------

/// Everything a machine's steps need doing to them: the far side, the session,
/// and the lease table.
///
/// One struct rather than four arguments repeated twice, and borrowed rather
/// than owned so a caller can drive a probe and then an install against the same
/// `ssh` master without rebuilding it — which is the whole point of the master.
pub struct BootstrapRunner<'a> {
    /// Runs `Step::Exec`.
    pub exec: &'a SshExec,
    /// Runs `Step::Call` and `Step::Hooks` — the client's own roost connection,
    /// which for a desktop is the bridge and for a phone is a loopback port.
    pub reach: &'a dyn RoostReach,
    /// Where a `Step::Hooks` result is remembered.
    pub leases: &'a RoostLeases,
    /// The target grammar token, for the lease table's key and for copy.
    pub target: &'a str,
    /// roost's `BootstrapOptions::jail_fs_root`. **`false` in production**; the
    /// only thing that sets it is a hermetic lane, where it prefixes the
    /// candidate ladder's absolute rungs with `${ROOST_BOOTSTRAP_FS_ROOT}` so a
    /// test about a cold host cannot find the developer's own
    /// `/usr/bin/roost-session` — which on a Linux box with roost installed is
    /// exactly what it would otherwise find.
    ///
    /// Carried here rather than read from the environment, for the reason
    /// [`SshBridgeOptions`] gives about `ROOST_TEST_MODE`: a variable meant for
    /// roost's own test lane must never steer which binary a shipped shed execs.
    pub jail_fs_root: bool,
}

/// How many steps a machine is allowed before the runner calls it a loop.
///
/// The same bound the sans-IO machines' own test rig uses, and for the same
/// reason: an install is about twenty steps, a probe about five, and anything
/// past this is a machine cycling rather than a slow host. Without it a runner
/// bug would spin a remote `ssh` exec as fast as the loop can run.
const MAX_STEPS: usize = 64;

impl BootstrapRunner<'_> {
    /// One read-only look at the host: what it is, what `roost-session` binaries
    /// are on it, and whether one is serving (plan 019 §3.4).
    ///
    /// **Writes nothing**, which is what makes it safe to run before the consent
    /// card — and what lets the card carry the [`Probe::fingerprint`] the
    /// install re-checks.
    pub async fn probe(&self) -> Result<Probe, BootstrapFailure> {
        let mut machine = ProbeMachine::new(self.target, self.jail_fs_root);
        let mut step = machine.begin();
        for _ in 0..MAX_STEPS {
            match step {
                Step::Done(result) => return result,
                other => step = machine.feed(self.perform(other).await?),
            }
        }
        Err(BootstrapFailure::new(
            Stage::Probe,
            format!("{}: the probe did not finish", self.target),
        ))
    }

    /// Install (or update, or merely start) a `roost-session`, and wire the
    /// host's agent hooks if shed started it.
    ///
    /// `request.fingerprint` is the consented probe's; the machine re-probes
    /// first and refuses if the host moved. `source` is `Some` exactly when the
    /// consented plan needs bytes.
    pub async fn install(
        &self,
        request: InstallRequest,
        source: Option<SourceHandle>,
    ) -> Result<Installed, BootstrapFailure> {
        // **Nothing this target's previous session decided survives into the
        // one this is about to start.** An install is only ever planned for a
        // host with no session shed can use (the plan matrix: a live protocol-4
        // session is "nothing to do"), and the machine re-probes the consented
        // fingerprint before it touches anything — so whatever token or
        // surrender is in the table belongs to a session that is gone. Carried
        // forward, a stale token would be presented to the new session's first
        // hooks call, and a stale *surrender* would silently mute the hooks
        // re-send for a session shed itself is about to start at the user's
        // request. Neither is a decision about this session.
        self.leases.forget(self.target);
        let mut machine = InstallMachine::new(request, source);
        let mut step = machine.begin();
        for _ in 0..MAX_STEPS {
            match step {
                Step::Done(result) => return result,
                other => step = machine.feed(self.perform(other).await?),
            }
        }
        // `Stage::Probe` because a machine that never terminated has no stage
        // of its own to report, and an install *begins* at its re-probe. The
        // message is what names the guard; the stage is the honest floor.
        Err(BootstrapFailure::new(
            Stage::Probe,
            format!("{}: the install did not finish", self.target),
        ))
    }

    /// Do one step. The `Err` is reserved for a step the runner itself cannot
    /// perform — a `Done` is handled by the caller and never reaches here.
    async fn perform(&self, step: Step<impl Sized>) -> Result<Outcome, BootstrapFailure> {
        match step {
            Step::Exec {
                command,
                stdin,
                budget,
                stdout_cap,
                capture_stdout,
            } => Ok(self
                .exec
                .run(&command, stdin, budget, stdout_cap, capture_stdout)
                .await),
            Step::Call { op, params } => Ok(Outcome::Call(self.call(&op, params).await)),
            Step::Hooks { client_label } => Ok(Outcome::Hooks(self.hooks(&client_label).await)),
            // Unreachable: both drivers above match `Done` before calling this.
            Step::Done(_) => Err(BootstrapFailure::new(
                Stage::Probe,
                format!("{}: the machine finished twice", self.target),
            )),
        }
    }

    /// One wire call over the client's own roost connection — **raw and
    /// ungated**, because the machine owns the protocol gate: a protocol-2
    /// session has to arrive as an answer so pin P6's "report it, never restart
    /// it" row can be produced at all.
    ///
    /// ## Where the probe's three-way answer actually comes from
    ///
    /// `session.identify` failing is not one state but three, and telling them
    /// apart is what the whole plan matrix turns on: a session answered, a
    /// `roost-session` is installed and not running, or there is none. The wire
    /// cannot say which — a dial to a bridge socket whose `ssh` exec died at 127
    /// looks exactly like one whose exec died saying `no session`. The
    /// classification is on the transport, and this is where it is read: the
    /// reach's [`last_error`](RoostReach::last_error) under its own generation
    /// watermark, mapped onto the [`reach_code`] spellings the machine
    /// understands.
    ///
    /// A runner that cannot classify sends something else, and the machine
    /// reports the probe as failed rather than guessing — which is why the
    /// fallthrough here is `transport` and not a cheerful `not-installed`.
    async fn call(
        &self,
        op: &str,
        params: serde_json::Value,
    ) -> Result<serde_json::Value, CallError> {
        // A fresh watermark per call, deliberately. A probe is a handful of
        // steps against a transport this runner may not even own; carrying a
        // watermark across them would mean a second `session.identify` in one
        // probe could not read the record its own exec had just left.
        let mut seen: u64 = 0;
        let endpoint = match self.reach.ensure().await {
            Ok(endpoint) => endpoint,
            Err(error) => {
                self.reach.invalidate().await;
                return Err(call_error(error));
            }
        };
        let mut conn = match Conn::endpoint(&endpoint).await {
            Ok(conn) => conn,
            Err(e) => {
                // **A dial failure is a wire failure, so the wire is rebuilt.**
                // This branch used to return without invalidating, which the
                // watermark then turned into a wrong answer rather than a
                // missing one: `call` resets `seen` per call (see above), so the
                // NEXT call over the same un-rebuilt tunnel re-read the record
                // this one had already reported and presented it as a fresh
                // reason for a different failure. The rule below — a wire that
                // failed is rebuilt, a session that refused is not — always
                // covered this case; only the code did not.
                let error = self.classified(&mut seen, reach_other(&e)).await;
                self.reach.invalidate().await;
                return Err(error);
            }
        };
        match conn.call_raw(op, params).await {
            Ok(value) => Ok(value),
            Err(RoostError::Server { code, message }) => Err(CallError::new(code, message)),
            Err(other) => {
                let transport = is_transport_error(&other);
                let error = self.classified(&mut seen, reach_other(&other)).await;
                if transport {
                    // The same rule the one-shots follow: a wire that failed is
                    // rebuilt, a session that refused is not.
                    self.reach.invalidate().await;
                }
                Err(error)
            }
        }
    }

    /// The transport's own classification for a failed call, if it recorded one
    /// this call has not already used.
    async fn classified(&self, seen: &mut u64, fallback: ReachError) -> CallError {
        call_error(overlay_reach_reason(self.reach, seen, fallback).await)
    }

    /// The lease dialogue, over the same connection kind — and the one place a
    /// lease enters the table.
    ///
    /// Under the target's turn, for the reason [`RoostLeases`] gives: this is
    /// the other read-decide-send about the same token, and a watcher's refresh
    /// interleaving with it would be deciding on a fact this step is in the
    /// middle of changing.
    async fn hooks(&self, client_label: &str) -> HooksResult {
        let gate = self.leases.gate(self.target);
        let _turn = gate.lock().await;
        let endpoint = match self.reach.ensure().await {
            Ok(endpoint) => endpoint,
            Err(error) => {
                self.reach.invalidate().await;
                return hooks_error(client_label, error.message);
            }
        };
        let mut conn = match Conn::endpoint(&endpoint).await {
            Ok(conn) => conn,
            Err(e) => {
                // Same rule as `call`: the endpoint we were handed did not
                // accept a connection, so the tunnel behind it is rebuilt
                // rather than handed to the next caller.
                self.reach.invalidate().await;
                return hooks_error(client_label, e.to_string());
            }
        };
        // A previous run's lease is presented rather than re-minted: see
        // [`RoostLeases`].
        let held = self.leases.lease(self.target);
        let result = wire_agent_hooks(&mut conn, client_label, held.as_deref()).await;
        self.leases.record(self.target, &result);
        result
    }
}

/// A reach failure as the machine's `Step::Call` refusal.
///
/// The two kinds the plan matrix turns on get roost's own kebab codes; the rest
/// get `transport`, which the machine reads as "the probe could not be
/// completed" rather than as a state of the far side.
fn call_error(error: ReachError) -> CallError {
    let code = match error.kind {
        ReachKind::NotInstalled => reach_code::NOT_INSTALLED,
        ReachKind::NoSession => reach_code::NO_SESSION,
        ReachKind::Unreachable | ReachKind::Other => "transport",
    };
    CallError::new(code, error.message)
}

/// A [`HooksResult`] reporting a failure before the hooks dialogue itself ran
/// (the reach couldn't be ensured, or the connection couldn't be dialed) —
/// `client_label` is preserved and every other field stays at its default.
fn hooks_error(client_label: &str, message: impl Into<String>) -> HooksResult {
    HooksResult {
        client_label: client_label.to_string(),
        error: Some(message.into()),
        ..HooksResult::default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::sync::atomic::AtomicUsize;

    use shed_core::rc::RcActivity;
    use shed_core::roost::testing::{ownership, FakeRoost};

    // ---- doubles ----

    /// The backoff-sleep seam's test half: record the wait the loop is ABOUT to
    /// take and return immediately rather than spend it. After `park_after`
    /// waits it pends forever, parking the loop at a known point instead of
    /// letting it spin while the test finishes its assertions.
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

    /// A reach that refuses its first `failures_left` `ensure`s and then hands
    /// over a working endpoint — "the machine is asleep, then wakes up". The
    /// refusals cost no I/O, so a whole failing ladder runs in the test's own
    /// time.
    struct FlakyReach {
        endpoint: RoostEndpoint,
        failures_left: AtomicUsize,
        /// Every `ensure`, refused or not — i.e. how many connection attempts
        /// the loop has made. What "the loop is still running" is read off.
        ensures: AtomicUsize,
        invalidations: AtomicUsize,
    }

    impl FlakyReach {
        fn new(endpoint: RoostEndpoint, failures: usize) -> Arc<FlakyReach> {
            Arc::new(FlakyReach {
                endpoint,
                failures_left: AtomicUsize::new(failures),
                ensures: AtomicUsize::new(0),
                invalidations: AtomicUsize::new(0),
            })
        }
    }

    #[async_trait::async_trait]
    impl RoostReach for FlakyReach {
        fn label(&self) -> &str {
            "flaky"
        }

        async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
            self.ensures.fetch_add(1, Ordering::SeqCst);
            if self.failures_left.load(Ordering::SeqCst) > 0 {
                self.failures_left.fetch_sub(1, Ordering::SeqCst);
                return Err(ReachError::other("the machine is asleep"));
            }
            Ok(self.endpoint.clone())
        }

        async fn invalidate(&self) {
            self.invalidations.fetch_add(1, Ordering::SeqCst);
        }
    }

    // ---- helpers ----

    /// A watcher on a fake. No cadence to shorten — an observer's latency is the
    /// push, and the only sleep left is the failure backoff.
    fn watch(reach: Arc<dyn RoostReach>) -> (RoostWatcher, mpsc::UnboundedReceiver<RoostUpdate>) {
        RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost-host".to_string(),
            RoostWatcherOptions::default(),
            BackoffSleeper::default(),
        )
    }

    /// Assert nothing arrives for a beat. Paired with a control that then makes
    /// something arrive — an "it stayed silent" assertion on its own passes just
    /// as well against a watcher that died.
    async fn stays_silent(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) {
        tokio::time::sleep(Duration::from_millis(50)).await;
        if let Ok(update) = rx.try_recv() {
            panic!("expected silence, got {update:?}");
        }
    }

    async fn next_update(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> RoostUpdate {
        tokio::time::timeout(Duration::from_secs(5), rx.recv())
            .await
            .expect("an update should arrive")
            .expect("the channel stays open")
    }

    async fn next_snapshot(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> RoostInventory {
        match next_update(rx).await {
            RoostUpdate::Snapshot(inventory) => inventory,
            other => panic!("expected a snapshot, got {other:?}"),
        }
    }

    /// The next snapshot, past however many `Down`s a failing ladder emitted
    /// first. Only for a test whose subject is the ladder itself — everywhere
    /// else an unexpected `Down` is the bug and [`next_snapshot`] should say so.
    async fn snapshot_past_downs(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> RoostInventory {
        loop {
            if let RoostUpdate::Snapshot(inventory) = next_update(rx).await {
                return inventory;
            }
        }
    }

    async fn next_down(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> String {
        match next_update(rx).await {
            RoostUpdate::Down { reason, .. } => reason,
            other => panic!("expected Down, got {other:?}"),
        }
    }

    /// The next snapshot whose rows are in `order`.
    ///
    /// A reorder commits one batch and the resync it triggers re-lists, so the
    /// cycle's own head snapshot is the one that carries the new order — but the
    /// batch's snapshot (rows unchanged, order not modelled) may legitimately go
    /// out first. Skipping to the one under test removes that race without
    /// weakening anything: under the bug this exists for the new order never
    /// arrives and [`next_update`]'s timeout fails the test.
    async fn snapshot_with_order(
        rx: &mut mpsc::UnboundedReceiver<RoostUpdate>,
        order: &[i64],
    ) -> RoostInventory {
        loop {
            let inventory = next_snapshot(rx).await;
            if inventory
                .sessions
                .iter()
                .map(|s| s.tab_id)
                .eq(order.iter().copied())
            {
                return inventory;
            }
        }
    }

    /// The next snapshot carrying `session_id`.
    ///
    /// A restart lands between the two requests one poll makes, so the first
    /// snapshot after it can legitimately still carry the previous instance's id
    /// alongside the new revision. Skipping to the id under test removes that
    /// race without weakening anything: under the bug these tests exist for, the
    /// id never arrives at all and [`next_update`]'s timeout fails the test.
    async fn snapshot_from_daemon(
        rx: &mut mpsc::UnboundedReceiver<RoostUpdate>,
        session_id: &str,
    ) -> RoostInventory {
        loop {
            let inventory = next_snapshot(rx).await;
            if inventory.daemon_session_id == session_id {
                return inventory;
            }
        }
    }

    fn entry(name: &str) -> MachineEntry {
        MachineEntry {
            name: name.to_string(),
            host: name.to_string(),
            user: None,
            ssh_port: 22,
            rc_bin: None,
            known_hosts: None,
        }
    }

    /// The tab the vendored `tab.list` vector carries that this suite drives.
    const TAB: i64 = 5;

    fn owned(detail: &str) -> serde_json::Value {
        ownership("opencode", "ses_test", detail, 1_700_000_060)
    }

    // ---- the reaches ----

    #[tokio::test]
    async fn a_local_session_is_the_socket_when_it_exists_and_names_it_when_it_does_not() {
        let fake = FakeRoost::start().await;
        let present = LocalSession::new("localhost", fake.socket_path());
        assert_eq!(
            present.ensure().await.expect("the socket is there"),
            RoostEndpoint::Unix(fake.socket_path().to_path_buf())
        );

        let missing_path = fake.socket_path().with_file_name("nothing.sock");
        let missing = LocalSession::new("localhost", &missing_path);
        let err = missing.ensure().await.expect_err("nothing is bound there");
        assert!(
            err.to_string()
                .contains(&missing_path.display().to_string()),
            "the reason must name the path that was tried: {err}"
        );
        // Never spawns: a failed `ensure` leaves nothing behind.
        assert!(!missing_path.exists());
    }

    /// The resolver's whole candidate ladder reaches the error message — "no
    /// roost-session" with no path in it is the least actionable thing this
    /// could say.
    #[test]
    fn the_default_local_reach_reports_every_path_it_looked_at() {
        let resolved = local_session_socket();
        let reach = LocalSession::default_local();
        assert_eq!(reach.label(), "localhost");
        assert_eq!(reach.socket(), resolved.path.as_path());
        assert_eq!(reach.tried, resolved.tried);
        assert!(reach.tried.len() >= 2, "release and the -dev sibling");
    }

    #[tokio::test]
    async fn a_labelled_port_is_the_port_and_a_fixed_port_reaches_roost_too() {
        let reach = LabelledPort::new("mini3", 41234);
        assert_eq!(reach.label(), "mini3");
        assert_eq!(
            reach.ensure().await.expect("a handed-over port is ready"),
            RoostEndpoint::TcpLoopback(41234)
        );
        reach.invalidate().await;
        assert_eq!(
            reach.ensure().await.expect("still ready"),
            RoostEndpoint::TcpLoopback(41234),
            "invalidating a port somebody else owns must not move it"
        );

        let pinned = FixedPort(41234);
        assert_eq!(
            RoostReach::ensure(&pinned).await.expect("ready"),
            RoostEndpoint::TcpLoopback(41234)
        );
    }

    #[tokio::test]
    async fn an_unreachable_reach_says_why() {
        let reach = UnreachableReach::new("mini9", "no roost transport is mapped for mini9");
        assert_eq!(reach.label(), "mini9");
        let err = reach.ensure().await.expect_err("it is unreachable by name");
        assert!(err.to_string().contains("mini9"), "{err}");
    }

    // ---- the watcher ----

    /// Both transports, because the TCP one goes through a socketpair and a copy
    /// pump that a Unix-only test would never touch.
    #[tokio::test]
    async fn the_first_snapshot_of_a_cycle_is_the_list_over_both_transports() {
        let fake = FakeRoost::start().await;

        let (unix_watcher, mut unix_rx) =
            watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let over_unix = next_snapshot(&mut unix_rx).await;
        unix_watcher.stop();

        let (tcp_watcher, mut tcp_rx) =
            watch(Arc::new(LabelledPort::new("mini3", fake.tcp_port())));
        let over_tcp = next_snapshot(&mut tcp_rx).await;
        tcp_watcher.stop();

        assert_eq!(over_unix.host_label, "roost-host");
        assert_eq!(over_unix.revision, Some(fake.revision()));
        assert_eq!(over_unix.daemon_session_id, fake.session_id());
        assert_eq!(
            over_unix.sessions, over_tcp.sessions,
            "the transport must not change what the inventory says"
        );
    }

    /// **shed watches; it never drives.** The subscribe carries an empty lease,
    /// which is an observer stream by construction on roost's side — so a
    /// watcher running against somebody's machine takes nothing away from the
    /// roost UI they are looking at.
    #[tokio::test]
    async fn the_watcher_subscribes_as_an_observer() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        next_snapshot(&mut rx).await;

        assert_eq!(fake.observer_count(), 1);
        assert_eq!(fake.driver_count(), 0, "shed holds no lease, ever");
        watcher.stop();
    }

    /// **A pushed batch is the whole update path.** The change arrives on the
    /// stream and is folded in; the inventory is re-read exactly once per cycle,
    /// which is what the `tab.list` counter pins.
    #[tokio::test]
    async fn an_axis_change_arrives_as_a_batch_without_re_reading_the_list() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let first = next_snapshot(&mut rx).await;
        let before = first
            .sessions
            .iter()
            .find(|s| s.tab_id == TAB)
            .expect("the owned tab is a row");
        assert_eq!(before.activity(), Some(RcActivity::Working));
        assert!(!before.attention);
        assert_eq!(fake.tab_list_calls(), 1);

        fake.set_tab_axes(TAB, "waiting", Some(owned("permission_asked")), true);
        let second = next_snapshot(&mut rx).await;
        let after = second
            .sessions
            .iter()
            .find(|s| s.tab_id == TAB)
            .expect("still a row");
        assert_eq!(after.activity(), Some(RcActivity::NeedsApproval));
        assert!(after.attention, "roost's sticky notification bit");
        assert!(second.revision > first.revision);
        assert_eq!(
            fake.tab_list_calls(),
            1,
            "a pushed change must not cost a snapshot re-read"
        );
        watcher.stop();
    }

    /// **Only a row change is news.** An empty commit and a tab nobody's adapter
    /// owns both advance the revision and publish nothing — and the control that
    /// follows says the silence was a decision and not a dead watcher.
    #[tokio::test]
    async fn an_empty_commit_and_a_hidden_tab_emit_nothing() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);
        let reach: Arc<dyn RoostReach> =
            Arc::new(LocalSession::new("localhost", fake.socket_path()));

        let (watcher, mut rx) = watch(Arc::clone(&reach));
        let first = next_snapshot(&mut rx).await;
        assert_eq!(first.sessions.len(), 1);

        // A commit that produced no events. roost pushes it anyway, which is
        // what makes a skipped revision mean loss — but it changes no row.
        fake.bump_revision();
        stays_silent(&mut rx).await;

        // A plain shell tab: it opens, the fold remembers it so a later claim can
        // promote it, and it is not a session row.
        let opened = tab_open(
            reach.as_ref(),
            TabOpenParams {
                title: "zsh".to_string(),
                ..Default::default()
            },
        )
        .await
        .expect("tab.open");
        stays_silent(&mut rx).await;

        // The control.
        fake.set_tab_axes(TAB, "waiting", Some(owned("question_asked")), false);
        let second = next_snapshot(&mut rx).await;
        assert_eq!(second.sessions.len(), 1);
        assert!(
            second.revision > first.revision,
            "the quiet commits advanced the revision even though nothing emitted"
        );
        assert!(
            !second.sessions.iter().any(|s| s.tab_id == opened.id),
            "an unowned tab is somebody's terminal, not a session row"
        );
        assert_eq!(fake.tab_list_calls(), 1);
        watcher.stop();
    }

    /// **A hidden tab VANISHING is news, even though its opening was not.**
    ///
    /// The asymmetry is the point. A client can be holding a row of its own for
    /// a tab it opened and no adapter has claimed yet — the Tauri client inserts
    /// one optimistically so a launch appears at once — and if the launched
    /// process dies before it ever reports, every event about that tab lands in
    /// the hidden half. Suppressing the close as "hidden churn" would leave that
    /// client showing a card for a tab that no longer exists, with no next poll
    /// to repair it.
    #[tokio::test]
    async fn a_vanished_hidden_tab_emits_a_snapshot() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);
        let reach: Arc<dyn RoostReach> =
            Arc::new(LocalSession::new("localhost", fake.socket_path()));

        let (watcher, mut rx) = watch(Arc::clone(&reach));
        let first = next_snapshot(&mut rx).await;
        assert_eq!(first.sessions.len(), 1);

        // Opening it emits nothing — the suppression the test above pins.
        let opened = tab_open(
            reach.as_ref(),
            TabOpenParams {
                title: "zsh".to_string(),
                ..Default::default()
            },
        )
        .await
        .expect("tab.open");
        stays_silent(&mut rx).await;

        tab_close(reach.as_ref(), opened.id)
            .await
            .expect("tab.close");
        let second = next_snapshot(&mut rx).await;
        assert!(
            !second.knows(opened.id),
            "the inventory published the tab as gone"
        );
        assert_eq!(
            second.sessions.len(),
            1,
            "and the VISIBLE row set never moved"
        );
        assert!(second.revision > first.revision);
        assert_eq!(
            fake.tab_list_calls(),
            1,
            "a vanished tab is still a pushed change, not a re-read"
        );
        watcher.stop();
    }

    /// The sticky notification bit clearing is a change like any other. It is
    /// worth its own lane because it is the one axis that moves *backwards*
    /// under normal use (roost clears it on UI focus).
    #[tokio::test]
    async fn clearing_has_notification_emits() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "waiting", Some(owned("question_asked")), true);

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let first = next_snapshot(&mut rx).await;
        assert!(first
            .sessions
            .iter()
            .any(|s| s.tab_id == TAB && s.attention));

        fake.set_tab_axes(TAB, "waiting", Some(owned("question_asked")), false);
        let second = next_snapshot(&mut rx).await;
        assert!(second
            .sessions
            .iter()
            .any(|s| s.tab_id == TAB && !s.attention));
        watcher.stop();
    }

    /// **A lost commit is a resync, not a `Down`.** The daemon is alive and we
    /// are behind it, so the cycle starts over at once: one more `tab.list`, a
    /// fresh fence, and the row that moved.
    #[tokio::test]
    async fn a_revision_gap_resyncs_without_a_down() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);

        let (watcher, mut rx) = watch(reach.clone());
        next_snapshot(&mut rx).await;
        assert_eq!(fake.tab_list_calls(), 1);

        // The only way to manufacture loss: advance the counter without pushing,
        // then commit. The client's own stream raises the gap before the batch
        // is ever yielded.
        fake.skip_revision();
        fake.set_tab_axes(TAB, "waiting", Some(owned("permission_asked")), false);

        // `next_snapshot` panics on a `Down`, so this asserts both halves.
        let after = next_snapshot(&mut rx).await;
        assert_eq!(
            after
                .sessions
                .iter()
                .find(|s| s.tab_id == TAB)
                .expect("still a row")
                .activity(),
            Some(RcActivity::NeedsApproval),
            "the resync's snapshot carries the state the lost batch would have"
        );
        assert_eq!(
            fake.tab_list_calls(),
            2,
            "exactly one more list — a resync, not a poll"
        );
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            0,
            "a resync tears down no transport: the daemon is fine, we are behind"
        );
        watcher.stop();
    }

    /// **`session.driver_changed` asks for nothing.** Somebody else took the
    /// interactive lease; shed never held it, the stream survives (that is the
    /// whole R1 re-cut), and the next commit still arrives on the same
    /// subscription.
    #[tokio::test]
    async fn a_driver_change_is_informational_and_the_stream_keeps_delivering() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);
        // Somebody has to be holding the lease for a takeover to depose them —
        // roost announces a *change* of driver, not a first claim.
        let mut driver = Conn::endpoint(&RoostEndpoint::Unix(fake.socket_path().to_path_buf()))
            .await
            .expect("dial");
        driver
            .session_connect(false, Some("the-roost-ui"))
            .await
            .expect("mints");

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        next_snapshot(&mut rx).await;

        fake.take_over("workbox");
        stays_silent(&mut rx).await;

        fake.set_tab_axes(TAB, "waiting", Some(owned("permission_asked")), false);
        let after = next_snapshot(&mut rx).await;
        assert_eq!(
            after
                .sessions
                .iter()
                .find(|s| s.tab_id == TAB)
                .expect("still a row")
                .activity(),
            Some(RcActivity::NeedsApproval)
        );
        assert_eq!(
            fake.tab_list_calls(),
            1,
            "a takeover must not cost a resync — the subscription is still ours"
        );
        watcher.stop();
    }

    /// **The subscribe/list race, which the prologue's ordering exists for.** A
    /// mutation commits between the ack and the `tab.list` reply: the batch it
    /// pushed is already in the snapshot, so it is discarded by the fence rather
    /// than mistaken for a gap, and the next commit applies normally.
    ///
    /// The fence assertions alone would **not** catch a list-first client: it
    /// would list, take the hook's commit into its snapshot, and then subscribe
    /// at that same revision, and every number below would still line up. What
    /// makes the ordering observable is the hook reading the fake's subscriber
    /// registry from *inside* the `tab.list` lock — a client that subscribed
    /// first has a stream registered by then, and a list-first one has none.
    #[tokio::test]
    async fn a_commit_between_the_ack_and_the_list_is_discarded_not_a_gap() {
        let fake = FakeRoost::start().await;
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);
        let acked = fake.revision();

        // Recorded rather than asserted in the hook: a panic inside the fake's
        // connection task would kill that task and time the test out, which
        // reports the wrong thing.
        let subscribed_by_list_time = Arc::new(AtomicUsize::new(usize::MAX));
        let recorder = Arc::clone(&subscribed_by_list_time);
        // Runs under the fake's state lock, once, just before the reply — the
        // exact interleaving a busy daemon produces.
        fake.before_tab_list(move |hook| {
            recorder.store(hook.observer_count(), Ordering::SeqCst);
            hook.bump_revision();
        });

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let first = next_snapshot(&mut rx).await;
        assert_eq!(
            subscribed_by_list_time.load(Ordering::SeqCst),
            1,
            "the stream must already be registered when the list is served — \
             a list-first prologue would read 0 here and turn every commit in \
             the window into a spurious gap"
        );
        assert_eq!(
            first.revision,
            Some(acked + 1),
            "the snapshot is already past the batch the stream is about to deliver"
        );

        // The queued `acked + 1` is discarded; `acked + 2` applies. A
        // list-then-subscribe prologue would have called this a gap.
        fake.set_tab_axes(TAB, "waiting", Some(owned("permission_asked")), false);
        let second = next_snapshot(&mut rx).await;
        assert_eq!(second.revision, Some(acked + 2));
        assert_eq!(
            fake.tab_list_calls(),
            1,
            "no resync happened, so the list was read exactly once"
        );
        watcher.stop();
    }

    /// **A reorder costs a re-list, on purpose.** `RoostInventory` folds no
    /// ordering, so the only way the new order reaches a client is a fresh
    /// `tab.list` — and under the old poll loop it arrived within one tick for
    /// free. An event-only watcher that ignored the two reorder events would
    /// leave a user who just dragged a tab looking at the old order until some
    /// unrelated resync.
    #[tokio::test]
    async fn a_reorder_costs_exactly_one_re_list_and_no_down() {
        let fake = FakeRoost::start().await;
        // Two owned tabs, so an order is observable at all.
        let second = {
            let reach: Arc<dyn RoostReach> =
                Arc::new(LocalSession::new("localhost", fake.socket_path()));
            tab_open(
                reach.as_ref(),
                TabOpenParams {
                    title: "second".to_string(),
                    ..Default::default()
                },
            )
            .await
            .expect("tab.open")
            .id
        };
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);
        fake.set_tab_axes(second, "working", Some(owned("session_status")), false);

        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let before = next_snapshot(&mut rx).await;
        let order: Vec<i64> = before.sessions.iter().map(|s| s.tab_id).collect();
        assert_eq!(order, vec![TAB, second]);
        assert_eq!(fake.tab_list_calls(), 1);

        fake.reorder_tabs();
        // `next_snapshot` panics on a `Down`, so this pins "no Down" too.
        let after = snapshot_with_order(&mut rx, &[second, TAB]).await;
        assert_eq!(
            after.sessions.iter().map(|s| s.tab_id).collect::<Vec<_>>(),
            vec![second, TAB],
            "the re-list is what carries the new order"
        );
        assert_eq!(
            fake.tab_list_calls(),
            2,
            "exactly one re-list — the reorder is a resync, not a poll"
        );

        // And the bound is not spent: the applied batch reset it, so a reorder
        // storm cannot ratchet a healthy session into `Down`.
        for _ in 0..MAX_CONSECUTIVE_RESYNCS + 2 {
            fake.reorder_tabs();
            next_snapshot(&mut rx).await;
        }
        watcher.stop();
    }

    /// **A restart is an EOF, and an EOF is a resync.** No `Down`, no backoff:
    /// the next cycle re-identifies and the snapshot carries the daemon that is
    /// actually there now, with the tab ids roost persisted across it.
    #[tokio::test]
    async fn a_daemon_restart_is_a_resync_that_re_identifies() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        let (watcher, mut rx) = watch(reach.clone());
        let before = next_snapshot(&mut rx).await;
        assert!(before.revision > Some(1), "the vector starts well above 1");

        fake.restart();
        let restarted = fake.session_id();
        // `next_snapshot` panics on a `Down`, so the whole point — that a
        // restart never renders the row stale-with-a-reason — is asserted by
        // getting here at all.
        let after = snapshot_from_daemon(&mut rx, &restarted).await;
        assert_eq!(after.revision, Some(1), "the counter is in-process");
        assert_ne!(
            after.daemon_session_id, before.daemon_session_id,
            "a restarted daemon is a new instance and the rows must say so"
        );
        assert_eq!(
            after.sessions.iter().map(|s| s.tab_id).collect::<Vec<_>>(),
            before.sessions.iter().map(|s| s.tab_id).collect::<Vec<_>>(),
            "tab ids persist across a restart, which is why rows key off them"
        );
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            0,
            "the transport was never the problem"
        );
        watcher.stop();
    }

    /// A hang-up with nothing else wrong is the same story as a restart: the
    /// stream ends, the cycle starts over, and the row never goes stale.
    #[tokio::test]
    async fn a_hangup_is_a_resync_with_a_fresh_snapshot_and_no_down() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        let (watcher, mut rx) = watch(reach.clone());
        next_snapshot(&mut rx).await;

        fake.close_all();
        let again = next_snapshot(&mut rx).await;
        assert_eq!(again.daemon_session_id, fake.session_id());
        assert_eq!(reach.invalidations.load(Ordering::SeqCst), 0);
        watcher.stop();
    }

    /// **The resync bound, on EOFs.** A daemon that ends the stream before it
    /// ever delivers a commit would otherwise be reconnected to as fast as the
    /// loop can run, forever. Three in a row are silent; the fourth is a `Down`
    /// that says so.
    #[tokio::test]
    async fn a_run_of_eofs_is_bounded_and_the_fourth_is_a_down() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));

        for attempt in 0..=MAX_CONSECUTIVE_RESYNCS {
            // `next_snapshot` panics on a `Down`, so the first
            // MAX_CONSECUTIVE_RESYNCS rounds assert the silence too.
            next_snapshot(&mut rx).await;
            assert!(
                fake.tab_list_calls() == attempt as usize + 1,
                "one list per cycle"
            );
            fake.close_all();
        }
        let reason = next_down(&mut rx).await;
        assert!(
            reason.contains("resyncing too often"),
            "the reason has to name the bound, not the last EOF: {reason}"
        );
        watcher.stop();
    }

    /// **An applied batch resets the bound.** A feed that is delivering commits
    /// and merely reconnecting a lot is healthy; only a run with no progress in
    /// it is not. Without the reset the fourth EOF below would be a `Down`.
    #[tokio::test]
    async fn an_applied_batch_resets_the_resync_bound() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));

        for _ in 0..MAX_CONSECUTIVE_RESYNCS {
            next_snapshot(&mut rx).await;
            fake.close_all();
        }

        // The cycle that makes progress: an empty commit is *applied* (it moves
        // the fence) even though it changes no row and emits nothing.
        next_snapshot(&mut rx).await;
        fake.bump_revision();
        stays_silent(&mut rx).await;
        fake.close_all();

        // Two more bare EOFs. Counting from the reset these are 2 and 3; without
        // it they would be 5 and 6, and the run would have died at 4.
        for _ in 0..2 {
            next_snapshot(&mut rx).await;
            fake.close_all();
        }
        next_snapshot(&mut rx).await;
        watcher.stop();
    }

    /// Mirrors `machine.rs`'s `a_connection_that_worked_resets_the_delay…`: the
    /// reset is keyed on the connection having WORKED, not on how it ended.
    ///
    /// The terminal event is a `session.stopping` rather than a hang-up, because
    /// a bare EOF is no longer a `Down` at all — it is a resync, and a resync
    /// never reaches the backoff this test reads.
    #[tokio::test]
    async fn a_connection_that_worked_resets_the_delay_however_it_later_ended() {
        let fake = FakeRoost::start().await;
        let (sleeper, mut waits) = ScriptedSleeper::new(3);
        // Two dead attempts, then a live one whose session then stops.
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 2);
        let (watcher, mut rx) = RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost-host".to_string(),
            RoostWatcherOptions::default(),
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

        // The third attempt reaches the fake and emits; then the session stops.
        // (The two dead attempts' `Down`s are queued ahead of it — each one is
        // sent before the wait that was just read.)
        snapshot_past_downs(&mut rx).await;
        fake.stop();
        assert_eq!(
            next_wait(&mut waits).await,
            backoff::INITIAL,
            "the third attempt connected and sent its snapshot, so the schedule \
             must start over — resetting only on a clean end would leave a \
             healthy feed reconnecting at the ceiling"
        );
        watcher.stop();
    }

    /// **`session.stopping` is the one thing on a stream that IS a `Down`.** The
    /// daemon is going away, so this is not a resync; the row goes
    /// stale-with-a-reason, stays that way while nothing answers, and comes back
    /// when the daemon does.
    #[tokio::test]
    async fn a_stopping_session_is_a_down_that_recovers_on_restart() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        next_snapshot(&mut rx).await;

        fake.stop();
        assert_eq!(
            next_down(&mut rx).await,
            "session stopping: stop",
            "the reason is roost's own, carried through"
        );
        // A stopped daemon accepts nothing, so the retry after the backoff fails
        // too — the row does not flicker back to fresh on its own.
        let while_stopped = next_down(&mut rx).await;
        assert!(!while_stopped.is_empty(), "a Down always says why");

        fake.restart();
        let after = snapshot_past_downs(&mut rx).await;
        assert_eq!(after.daemon_session_id, fake.session_id());
        watcher.stop();
    }

    /// A roost **UI** socket is never read as machine inventory, and the reason
    /// says which of the two it found.
    #[tokio::test]
    async fn a_ui_socket_is_a_down_that_names_itself() {
        let fake = FakeRoost::start().await;
        fake.serve_as_ui_socket(true);
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let reason = next_down(&mut rx).await;
        assert!(
            reason.contains("UI socket"),
            "the reason must name what it found: {reason}"
        );
        watcher.stop();
    }

    #[tokio::test]
    async fn a_protocol_mismatch_is_a_down_naming_both_numbers() {
        let fake = FakeRoost::start().await;
        let theirs = roost_ipc::messages::SESSION_PROTOCOL_VERSION + 7;
        fake.set_session_protocol(theirs);
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let reason = next_down(&mut rx).await;
        assert!(reason.contains(&theirs.to_string()), "{reason}");
        assert!(
            reason.contains(&roost_ipc::messages::SESSION_PROTOCOL_VERSION.to_string()),
            "both numbers, or the message cannot say which side to upgrade: {reason}"
        );
        watcher.stop();
    }

    /// The un-upgraded machine on the network today: a protocol-2 daemon is
    /// refused rather than limped through, and the row says so.
    #[tokio::test]
    async fn a_protocol_two_daemon_is_a_down_rather_than_a_degraded_row() {
        let fake = FakeRoost::start().await;
        fake.set_session_protocol(2);
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        let reason = next_down(&mut rx).await;
        assert!(reason.contains("session protocol 2"), "{reason}");
        watcher.stop();
    }

    #[tokio::test]
    async fn stopping_a_watcher_ends_its_loop() {
        let fake = FakeRoost::start().await;
        let (watcher, mut rx) = watch(Arc::new(LocalSession::new("localhost", fake.socket_path())));
        next_snapshot(&mut rx).await;
        assert_eq!(watcher.label(), "roost-host");

        watcher.stop();
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), true);
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(
            rx.try_recv().is_err(),
            "a stopped watcher must publish nothing, even against a live session"
        );
    }

    /// An abandoned receiver must end the task, not leak it — the watcher itself
    /// is deliberately kept alive here, so only the dropped receiver can stop
    /// the loop.
    ///
    /// Read off `ensure` calls rather than off the channel that was just
    /// dropped: one attempt is all a stopped loop ever makes, and a loop that
    /// kept resyncing against a session that keeps hanging up would climb.
    #[tokio::test]
    async fn dropping_the_receiver_ends_the_loop() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        let (_watcher, mut rx) = watch(reach.clone());
        next_snapshot(&mut rx).await;
        assert_eq!(reach.ensures.load(Ordering::SeqCst), 1);
        drop(rx);

        // Keep giving it something to reconnect to, then something to fail on.
        for _ in 0..5 {
            fake.close_all();
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(
            reach.ensures.load(Ordering::SeqCst),
            1,
            "a loop with nobody listening must not keep reconnecting"
        );
    }

    // ---- one-shots ----

    #[tokio::test]
    async fn the_one_shots_drive_a_tab_end_to_end() {
        let fake = FakeRoost::start().await;
        // An `Arc` because the peek below keeps it: a peek outlives the call
        // that opened it and has to be able to invalidate the reach itself.
        let reach: Arc<dyn RoostReach> =
            Arc::new(LocalSession::new("localhost", fake.socket_path()));

        let tab = tab_open(
            reach.as_ref(),
            TabOpenParams {
                cwd: "/home/shed/app".to_string(),
                argv: launch_argv(&shed_core::rc::RcKind::Opencode)
                    .expect("opencode is launchable"),
                title: "opencode".to_string(),
                ..Default::default()
            },
        )
        .await
        .expect("tab.open");
        assert_eq!(tab.cwd, "/home/shed/app");

        // No write here: a `tab.write` is lease-gated at session protocol 4 and
        // therefore not a one-shot at all — it lives on `Conn`, beside the
        // `session.connect` that authorizes it, and is tested there.
        let dump = tab_dump(reach.as_ref(), tab.id).await.expect("dump");
        assert!(dump.rows_text.iter().any(|line| line.contains("opencode")));

        // A peek holds ONE connection across frames.
        let mut peek = RoostPeek::open(Arc::clone(&reach), tab.id)
            .await
            .expect("peek");
        assert_eq!(peek.tab_id(), tab.id);
        let first = peek.dump().await.expect("first frame");
        let second = peek.dump().await.expect("second frame on the same conn");
        assert_eq!(first.rows_text, second.rows_text);
        drop(peek);

        tab_close(reach.as_ref(), tab.id).await.expect("close");
        let gone = tab_dump(reach.as_ref(), tab.id)
            .await
            .expect_err("a closed tab leaves the list");
        assert!(gone.contains("not-found"), "{gone}");
    }

    /// A failing one-shot invalidates the reach so the next call rebuilds it.
    #[tokio::test]
    async fn a_failed_one_shot_invalidates_the_reach() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);
        tab_dump(reach.as_ref(), 999_999)
            .await
            .expect_err("no such tab");
        assert_eq!(reach.invalidations.load(Ordering::SeqCst), 1);
    }

    /// **A peek is the one holder of a connection, so it is the one thing that
    /// must invalidate the reach itself.** Everything else here dials per call
    /// and routes its errors through `finish`. A peek that only held its `Conn`
    /// would, over an [`SshBridge`] whose `ssh` master has died, get
    /// `Unavailable`, be re-opened by its caller, and be handed the same dead
    /// `bridge.sock` back — which accepts and then answers nothing, forever.
    ///
    /// The second half is the discrimination: a session refusing a tab id is the
    /// wire *working*, and tearing an ssh tunnel down for a closed tab would
    /// make every stale id cost a re-establish.
    #[tokio::test]
    async fn a_peek_invalidates_on_a_dead_wire_and_not_on_a_gone_tab() {
        let fake = FakeRoost::start().await;
        let reach = FlakyReach::new(RoostEndpoint::Unix(fake.socket_path().to_path_buf()), 0);

        // A tab that is not there: the daemon read, dispatched and refused, so
        // the transport is demonstrably fine.
        let mut missing = RoostPeek::open(reach.clone(), 999_999)
            .await
            .expect("the peek opens — the tab is only checked by a dump");
        let refused = missing.dump().await.expect_err("no such tab");
        assert_eq!(
            refused.server_code(),
            Some(roost_ipc::client::ServerCode::NotFound),
            "{refused}"
        );
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            0,
            "a gone tab is not a dead transport: {refused}"
        );
        drop(missing);

        // The wire dying under a live peek is the other case.
        let mut peek = RoostPeek::open(reach.clone(), TAB).await.expect("peek");
        peek.dump().await.expect("the first frame comes back");
        fake.close_all();
        // The hang-up is the only thing this connection has pending, so it is
        // processed before the next request is even written.
        tokio::time::sleep(Duration::from_millis(100)).await;

        let dead = peek.dump().await.expect_err("the connection is gone");
        assert!(dead.is_unavailable(), "a hang-up is Unavailable: {dead}");
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            1,
            "the next `ensure` has to rebuild rather than hand back the dead socket"
        );
    }

    // ---- the ssh bridge ----

    #[test]
    fn a_target_omits_the_user_and_the_default_port_and_never_says_localhost() {
        let bare = ssh_target(&entry("mini3")).expect("classified");
        assert_eq!(bare.raw, "ssh://mini3");

        let mut full = entry("mini3");
        full.user = Some("charliek".into());
        full.ssh_port = 2222;
        assert_eq!(
            ssh_target(&full).expect("classified").raw,
            "ssh://charliek@mini3:2222"
        );

        // A machine literally named `localhost` is still an ssh target: the bare
        // string is roost's sentinel for THIS machine's session, resolved through
        // its build-profile-sensitive path resolver.
        let local = ssh_target(&entry("localhost")).expect("classified");
        assert_eq!(local.raw, "ssh://localhost");

        let mut hostless = entry("mini3");
        hostless.host = String::new();
        hostless.name = String::new();
        assert!(ssh_target(&hostless).is_err(), "nothing to reach");
    }

    /// **An IPv6 literal is bracketed, and only an IPv6 literal is.**
    /// `ssh://2001:db8::1:2222` has no reading that recovers the address the
    /// user configured — roost's own authority parser takes the last colon as
    /// the port separator — so the brackets are what make a port and an IPv6
    /// host expressible at the same time.
    #[test]
    fn an_ipv6_literal_is_bracketed_and_nothing_else_is() {
        let mut v6 = entry("edge");
        v6.host = "2001:db8::1".to_string();
        v6.ssh_port = 2222;
        assert_eq!(
            ssh_target(&v6).expect("classified").raw,
            "ssh://[2001:db8::1]:2222"
        );

        // The user goes before the brackets, where an authority's user always is.
        v6.user = Some("me".into());
        assert_eq!(
            ssh_target(&v6).expect("classified").raw,
            "ssh://me@[2001:db8::1]:2222"
        );

        // On the default port there is no port to disambiguate, but the address
        // is still bracketed — one spelling, and `ssh` accepts it.
        let mut v6_default = entry("edge");
        v6_default.host = "2001:db8::1".to_string();
        assert_eq!(
            ssh_target(&v6_default).expect("classified").raw,
            "ssh://[2001:db8::1]"
        );

        // An address the user already bracketed is not double-wrapped.
        let mut prebracketed = entry("edge");
        prebracketed.host = "[2001:db8::1]".to_string();
        prebracketed.ssh_port = 2222;
        assert_eq!(
            ssh_target(&prebracketed).expect("classified").raw,
            "ssh://[2001:db8::1]:2222"
        );

        // The negative control: an alias, a hostname and an IPv4 address are
        // untouched — bracketing any of them would be a target `ssh` cannot
        // resolve.
        let mut alias = entry("work");
        alias.host = "work-box".to_string();
        alias.ssh_port = 2222;
        assert_eq!(
            ssh_target(&alias).expect("classified").raw,
            "ssh://work-box:2222"
        );
        let mut v4 = entry("edge");
        v4.host = "10.0.0.4".to_string();
        assert_eq!(ssh_target(&v4).expect("classified").raw, "ssh://10.0.0.4");
    }

    /// The `Host` line of the pin keeps the address UNBRACKETED: `ssh` matches
    /// `Host` patterns against the hostname it extracted from the target, which
    /// is what was inside the brackets.
    #[test]
    fn the_pin_matches_an_ipv6_host_the_way_ssh_spells_it() {
        let mut v6 = entry("edge");
        v6.host = "2001:db8::1".to_string();
        v6.ssh_port = 2222;
        v6.known_hosts = Some("/k".to_string());

        let bridge = SshBridge::new(&v6, SshBridgeOptions::default()).expect("bridge");
        assert_eq!(bridge.target(), "ssh://[2001:db8::1]:2222");
        let body = std::fs::read_to_string(bridge.ssh_config_path().expect("a pin")).expect("read");
        assert!(
            body.starts_with("Host 2001:db8::1\n"),
            "the pattern is the hostname, not the URL authority: {body}"
        );
    }

    #[test]
    fn a_host_id_stays_a_parseable_scratch_directory_leaf() {
        assert_eq!(host_id("mini3"), "mini3");
        assert_eq!(host_id("work-box.local"), "work-box.local");
        assert_eq!(host_id("a/b c"), "a-b-c", "no path separators, no spaces");
        assert_eq!(host_id(""), "machine");
        assert_eq!(host_id(&"x".repeat(64)).len(), 32, "the sun_path budget");
        // roost reads its own directory names back; ours must survive that.
        let name = roost_ipc::ssh::scratch_dir_name(&host_id("a/b c"));
        let (parsed, _, _) =
            roost_ipc::ssh::parse_scratch_dir_name(&name).expect("roost can read it back");
        assert_eq!(parsed, "a-b-c");
    }

    /// The pin has to win over the user's own config, which means it has to come
    /// FIRST — `ssh` takes the first value it obtains for a keyword.
    #[test]
    fn a_pinned_config_puts_the_pin_above_the_user_config_it_includes() {
        let dir = tempfile::tempdir().expect("tempdir");
        let user = dir.path().join("user_config");
        std::fs::write(&user, "Host mini3\n  HostName 10.0.0.4\n").expect("write");

        let body = pinned_ssh_config("mini3", "/home/me/.shed/known_hosts", Some(&user));
        let lines: Vec<&str> = body.lines().collect();
        assert_eq!(lines[0], "Host mini3");
        assert_eq!(
            lines[1],
            "  UserKnownHostsFile \"/home/me/.shed/known_hosts\""
        );
        assert_eq!(lines[2], "  StrictHostKeyChecking yes");
        assert_eq!(lines[3], format!("Include \"{}\"", user.display()));

        // A user config that is not there is not included at all.
        let absent = dir.path().join("nope");
        assert_eq!(
            pinned_ssh_config("mini3", "/k", Some(&absent))
                .lines()
                .count(),
            3
        );
    }

    #[test]
    fn a_tilde_in_known_hosts_expands_against_home() {
        let home = std::env::var("HOME").expect("a HOME on this host");
        let body = pinned_ssh_config("mini3", "~/.ssh/known_hosts_mini3", None);
        assert!(
            body.contains(&format!(
                "UserKnownHostsFile \"{home}/.ssh/known_hosts_mini3\""
            )),
            "{body}"
        );
        // An absolute path is left exactly as written.
        assert!(
            pinned_ssh_config("mini3", "/etc/kh", None).contains("UserKnownHostsFile \"/etc/kh\"")
        );
    }

    /// **A `known_hosts` path with a space in it must survive as one path.**
    /// `UserKnownHostsFile` takes a whitespace-separated LIST, so an unquoted
    /// `.../Application Support/...` reaches `ssh` as two files that do not
    /// exist — and with `StrictHostKeyChecking yes` directly beneath it, that is
    /// not a loose pin, it is a host that can never be connected to. The
    /// `Include` line has always been quoted for the same reason; this is the
    /// other half.
    #[test]
    fn a_known_hosts_path_with_a_space_is_quoted() {
        let spaced = "/Users/me/Library/Application Support/shed/known_hosts";
        let body = pinned_ssh_config("mini3", spaced, None);
        assert!(
            body.contains(&format!("  UserKnownHostsFile \"{spaced}\"\n")),
            "the path has to arrive as one quoted argument: {body}"
        );
        // Not "quoted somewhere" — quoted around the WHOLE path, so the line
        // holds exactly one file.
        let line = body
            .lines()
            .find(|line| line.contains("UserKnownHostsFile"))
            .expect("the pin line");
        assert_eq!(
            line.matches('"').count(),
            2,
            "one opening and one closing quote: {line}"
        );

        // A `~` path with a space expands AND stays quoted — the expansion must
        // not be what breaks the quoting.
        let home = std::env::var("HOME").expect("a HOME on this host");
        let expanded = pinned_ssh_config("mini3", "~/known hosts", None);
        assert!(
            expanded.contains(&format!("UserKnownHostsFile \"{home}/known hosts\"")),
            "{expanded}"
        );
    }

    /// A `machines:` entry with `known_hosts` writes the pin file up front (it
    /// has to exist before roost renders its own config, which existence-checks
    /// what it includes) and hands it over as the user config.
    #[test]
    fn a_known_hosts_entry_generates_the_per_machine_config() {
        let mut pinned = entry("mini3");
        pinned.known_hosts = Some("/home/me/.shed/known_hosts".to_string());
        let bridge = SshBridge::new(&pinned, SshBridgeOptions::default()).expect("bridge");

        let path = bridge.ssh_config_path().expect("a pin generates a config");
        let body = std::fs::read_to_string(path).expect("the file is written at construction");
        assert!(body.contains("Host mini3\n"), "{body}");
        assert!(
            body.contains("  UserKnownHostsFile \"/home/me/.shed/known_hosts\"\n"),
            "{body}"
        );
        assert!(body.contains("  StrictHostKeyChecking yes\n"), "{body}");
        assert_eq!(bridge.options.config_paths.user.as_deref(), Some(path));
        assert!(!bridge.options.jail_fs_root, "never from the environment");

        // No pin, no generated file — the user's own config and strictness apply.
        let plain = SshBridge::new(&entry("mini3"), SshBridgeOptions::default()).expect("bridge");
        assert!(plain.ssh_config_path().is_none());
    }

    /// The generated config directory is this bridge's, and it goes when the
    /// bridge does.
    #[test]
    fn a_bridges_config_directory_is_removed_with_it() {
        let mut pinned = entry("mini3");
        pinned.known_hosts = Some("/k".to_string());
        let bridge = SshBridge::new(&pinned, SshBridgeOptions::default()).expect("bridge");
        let dir = bridge
            .ssh_config_path()
            .and_then(Path::parent)
            .expect("a config lives in a directory")
            .to_path_buf();
        assert!(dir.exists());
        drop(bridge);
        assert!(!dir.exists(), "the scratch directory outlived the bridge");
    }

    // -- the fake ssh --

    /// A stand-in for `ssh` that pipes a connection's stdio to a Unix socket.
    ///
    /// Modelled on roost's own `tools/roosttest/fixtures/fake-ssh.sh` and cut
    /// down to the two behaviours this suite needs a real [`SshTunnel`] to see:
    ///
    /// * **`-O exit` is a recorded no-op** that removes the control socket, the
    ///   way the real master does on its way out. Teardown ordering — exit the
    ///   master while its socket is still on disk — is otherwise invisible.
    /// * **A remote command of exactly `true` is honoured literally**, because
    ///   that is what roost's warm-up (`establish_argv`) runs. Everything else
    ///   is a connection, and gets pumped.
    /// * **A `<dir>/no-session` marker turns every later CONNECTION into the far
    ///   side's own refusal** (`client-bridge: no session` on stderr, exit 1)
    ///   while the warm-up keeps succeeding. That asymmetry is the whole point:
    ///   it is what a real host does when `roost-session` is installed and not
    ///   running, and it is the only way to produce a LATE record on the tunnel
    ///   (plan 019 §3.6). `establish` ran `true` and got a zero; the
    ///   classification only exists once a per-connection exec has died, which
    ///   is why nothing but the tunnel itself can report it.
    ///
    /// The pump is `python3` rather than `nc -U`/`socat`: `python3` is the only
    /// one of the three that can be relied on, and a half-close has to be a real
    /// `shutdown(SHUT_WR)` or the far side never sees EOF.
    fn write_fake_ssh(dir: &Path, socket: &Path, log: &Path) -> PathBuf {
        let script = format!(
            r#"#!/bin/sh
set -u
ctl=
want=0
prev=
is_exit=0
remote=
for arg in "$@"; do
    if [ "$want" -eq 1 ]; then
        ctl="$arg"
        want=0
    elif [ "$arg" = "-S" ]; then
        want=1
    fi
    if [ "$prev" = "-O" ] && [ "$arg" = "exit" ]; then is_exit=1; fi
    prev="$arg"
    remote="$arg"
done
if [ "$is_exit" -eq 1 ]; then
    printf 'exit\n' >>'{log}'
    if [ -n "$ctl" ]; then rm -f "$ctl"; fi
    exit 0
fi
if [ -n "$ctl" ] && [ ! -e "$ctl" ]; then : >"$ctl"; fi
if [ "$remote" = "true" ]; then
    printf 'establish\n' >>'{log}'
    exit 0
fi
if [ -e '{no_session}' ]; then
    printf 'no-session\n' >>'{log}'
    printf '%s\n' 'client-bridge: no session is listening at /run/user/1000/roost/session.sock' >&2
    exit 1
fi
printf 'connect\n' >>'{log}'
exec python3 -c '
import os, socket, sys, threading
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
def up():
    try:
        while True:
            chunk = os.read(0, 65536)
            if not chunk:
                break
            s.sendall(chunk)
    except Exception:
        pass
    try:
        s.shutdown(socket.SHUT_WR)
    except Exception:
        pass
threading.Thread(target=up, daemon=True).start()
try:
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        os.write(1, chunk)
except Exception:
    pass
' '{socket}'
"#,
            log = log.display(),
            socket = socket.display(),
            no_session = no_session_marker(dir).display(),
        );
        write_staged_script(dir, "ssh", script)
    }

    /// Writes `script` to `<dir>/<name>` under a staging name and renames it
    /// into place: a script created while a sibling test thread is forking
    /// answers ETXTBSY to `execve`, and a rename never leaves the final path
    /// open for writing.
    fn write_staged_script(dir: &Path, name: &str, script: String) -> PathBuf {
        let staged = dir.join(format!("{name}.staging"));
        let final_path = dir.join(name);
        std::fs::write(&staged, script).expect("write the fake script");
        let mut perms = std::fs::metadata(&staged).expect("stat").permissions();
        {
            use std::os::unix::fs::PermissionsExt as _;
            perms.set_mode(0o700);
        }
        std::fs::set_permissions(&staged, perms).expect("chmod");
        std::fs::rename(&staged, &final_path).expect("rename into place");
        final_path
    }

    fn ran(log: &Path) -> Vec<String> {
        std::fs::read_to_string(log)
            .unwrap_or_default()
            .lines()
            .map(str::to_string)
            .collect()
    }

    /// A bridge to `name` that spawns `ssh` and scratches inside the test's own
    /// directory.
    ///
    /// The scratch parent is short, and ours: roost length-checks the whole
    /// `<parent>/<leaf>/bridge.sock` against `sun_path`, and sweeping a shared
    /// /tmp would meet other tests' leftovers. The empty [`SshConfigPaths`] is
    /// the same isolation for the config — a developer's own `~/.ssh/config`
    /// must not reach the fake ssh.
    fn faked_bridge(name: &str, ssh: PathBuf) -> SshBridge {
        SshBridge::new(
            &entry(name),
            SshBridgeOptions {
                ssh_bin: Some(ssh),
                // NOT the per-test tempdir: on macOS that is
                // `/var/folders/<..>/T/.tmpXXXX`, and roost refuses a scratch
                // parent that leaves no room for its `<dir>/ctl.<16 hex>` under
                // the 103-byte AF_UNIX limit (the CI Swift job runs this suite
                // there). `/tmp` is short on every platform, and roost's own
                // per-attempt `roost-ssh-<host>-<pid>-<seq>` naming keeps two
                // processes apart under it.
                scratch_parents: vec![PathBuf::from("/tmp")],
                config_paths: Some(SshConfigPaths {
                    user: None,
                    system: None,
                }),
                ..SshBridgeOptions::default()
            },
        )
        .expect("bridge")
    }

    /// The whole bridge against a real [`SshTunnel`]: establish, answer
    /// `session.identify` through the bridge socket, then re-establish on a fresh
    /// scratch directory after an `invalidate`.
    #[tokio::test]
    async fn an_ssh_bridge_establishes_answers_and_re_establishes_after_invalidate() {
        let fake = FakeRoost::start().await;
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("ssh.log");
        let ssh = write_fake_ssh(dir.path(), fake.socket_path(), &log);
        let bridge = faked_bridge("mini3", ssh);

        let first = bridge.ensure().await.expect("the tunnel establishes");
        let RoostEndpoint::Unix(first_socket) = first.clone() else {
            panic!("an ssh bridge is always a unix socket: {first:?}");
        };
        assert!(first_socket.exists(), "establish binds the bridge socket");
        assert_eq!(ran(&log), vec!["establish"], "the warm-up runs `true`");

        // The wire actually works through it.
        let mut conn = Conn::endpoint(&first).await.expect("dial the bridge");
        let identify = conn.session_identify().await.expect("session.identify");
        assert_eq!(identify.session_id, fake.session_id());
        drop(conn);

        // A held, un-invalidated tunnel is reused rather than rebuilt.
        assert_eq!(bridge.ensure().await.expect("still held"), first);

        bridge.invalidate().await;
        let second = bridge.ensure().await.expect("it re-establishes");
        let RoostEndpoint::Unix(second_socket) = second else {
            panic!("still a unix socket");
        };
        assert_ne!(
            second_socket, first_socket,
            "roost names a scratch directory per ATTEMPT, so the endpoint moves"
        );
        assert!(second_socket.exists());
        // The old tunnel is gone, and its master was exited before the new
        // attempt's warm-up (the log below pins that order). What this CANNOT
        // observe is the reason the order is pinned: roost's sweep reclaims
        // *this process's own* leftovers with no probe, so in one process the
        // two orderings end identically. The refusal the order avoids is
        // another process's live `bridge.sock`, which no unit test can stand up.
        assert!(!first_socket.exists(), "the old scratch directory is gone");
        assert_eq!(
            ran(&log),
            vec!["establish", "connect", "exit", "establish"],
            "one warm-up per attempt, the identify over its own exec, and the \
             old master exited before the new attempt"
        );
    }

    /// A bridge that cannot reach anything fails as a reason, not a panic — and
    /// the message names the machine the way the user addressed it.
    #[tokio::test]
    async fn an_ssh_bridge_that_cannot_connect_names_the_machine() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ssh = dir.path().join("no-such-ssh");
        let bridge = faked_bridge("mini9", ssh);
        let err = bridge.ensure().await.expect_err("there is no ssh to spawn");
        assert!(err.to_string().contains("machine:mini9"), "{err}");
    }

    /// The watcher over the real SSH transport, end to end.
    #[tokio::test]
    async fn a_watcher_reads_an_inventory_through_an_ssh_bridge() {
        let fake = FakeRoost::start().await;
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("ssh.log");
        let ssh = write_fake_ssh(dir.path(), fake.socket_path(), &log);
        fake.set_tab_axes(TAB, "working", Some(owned("session_status")), false);

        // A machine name of its own, and it is load-bearing: roost's `open`
        // sweeps THIS PROCESS's older scratch directories for the same host id
        // with no liveness probe, so two concurrent tests sharing a name have
        // one of them delete the other's `bridge.sock` out from under it.
        let bridge = faked_bridge("mini-watch", ssh);
        let (watcher, mut rx) = watch(Arc::new(bridge));
        let inventory = next_snapshot(&mut rx).await;
        assert_eq!(inventory.host_label, "roost-host");
        assert!(inventory
            .sessions
            .iter()
            .any(|s| s.tab_id == TAB && s.activity() == Some(RcActivity::Working)));
        watcher.stop();
    }

    // ---- typed reach errors (plan 019 C6) ----

    /// Where the `no-session` marker lives for a fake-ssh directory. Shared by
    /// the writer (which bakes the path into the script) and the tests that
    /// create it, so the two cannot drift.
    fn no_session_marker(dir: &Path) -> PathBuf {
        dir.join("no-session")
    }

    /// One row of `stderr-classes.json`'s `reach_kinds` section.
    #[derive(serde::Deserialize)]
    struct ReachKindRow {
        name: String,
        class: String,
        kind: String,
    }

    #[derive(serde::Deserialize)]
    struct ClassRow {
        name: String,
        exit_code: Option<i32>,
        stderr: String,
        class: String,
    }

    #[derive(serde::Deserialize)]
    struct StderrClasses {
        classes: Vec<ClassRow>,
        reach_kinds: Vec<ReachKindRow>,
    }

    fn stderr_classes() -> StderrClasses {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../fixtures/roost-vectors/stderr-classes.json");
        let text = std::fs::read_to_string(&path)
            .unwrap_or_else(|e| panic!("reading {}: {e}", path.display()));
        serde_json::from_str(&text).expect("stderr-classes.json")
    }

    /// **The shed-app leg of `stderr-classes.json`** — the one plan 019 §3.3
    /// names beside Go's and C2 could not yet write, because `ReachError` did
    /// not exist.
    ///
    /// It asserts the composition rather than the mapping in isolation: each
    /// pinned stderr blob goes through roost's LIVE classifier and then through
    /// `ReachError::from_ssh_failure`, and the kind that comes out is the one
    /// the golden says. So a roost bump that re-routes a family, and a shed edit
    /// that re-routes one, both fail here — which is the whole reason the two
    /// sections live in one file.
    #[test]
    fn the_stderr_classes_golden_maps_onto_reach_kinds() {
        let golden = stderr_classes();
        let by_class: std::collections::BTreeMap<&str, &str> = golden
            .reach_kinds
            .iter()
            .map(|row| (row.class.as_str(), row.kind.as_str()))
            .collect();

        for case in &golden.classes {
            let failure = roost_ipc::ssh::classify_ssh_failure(case.exit_code, &case.stderr);
            let error = ReachError::from_ssh_failure(&failure, "mini3");
            let want = by_class.get(case.class.as_str()).unwrap_or_else(|| {
                panic!(
                    "`classes` case {} is class {:?}, which `reach_kinds` does not map",
                    case.name, case.class
                )
            });
            assert_eq!(
                error.kind.as_str(),
                *want,
                "reach kind for the {} case",
                case.name
            );
            // roost's own copy, not a shed paraphrase of it: the sentence a user
            // reads about a changed host key is one nobody should be rewriting.
            assert_eq!(error.message, failure.message("mini3"));
        }

        // Every row of `reach_kinds` names a class the classifier can produce,
        // and every class the classifier can produce has a row. A half-covered
        // mapping would pass the loop above while leaving a branch unasserted.
        let mapped: std::collections::BTreeSet<&str> = by_class.keys().copied().collect();
        assert_eq!(
            mapped,
            std::collections::BTreeSet::from([
                "auth",
                "changed-host-key",
                "host-key-unknown",
                "no-session",
                "not-found",
                "transport",
            ])
        );
        // Every declared kind is a spelling this enum actually has. Without
        // this a typo in the golden would look like a *code* bug on whichever
        // class carried it, rather than like the fixture edit it was.
        let spellings: std::collections::BTreeSet<&str> = [
            ReachKind::NotInstalled,
            ReachKind::NoSession,
            ReachKind::Unreachable,
            ReachKind::Other,
        ]
        .iter()
        .map(ReachKind::as_str)
        .collect();
        for row in &golden.reach_kinds {
            assert!(
                spellings.contains(row.kind.as_str()),
                "{} names kind {:?}, which no ReachKind spells",
                row.name,
                row.kind
            );
            // And `other` is reachable from NO class — the property the
            // section's comment claims. It is what a failure that never ran an
            // ssh exec reports.
            assert_ne!(
                row.kind, "other",
                "{} maps an ssh classification onto `other`",
                row.name
            );
        }
    }

    /// A local session that is not there is a **start** offer, not an install
    /// offer: the resolver looked for a socket, and a socket's absence says
    /// nothing about the binary.
    #[tokio::test]
    async fn a_missing_local_session_is_a_no_session_kind() {
        let dir = tempfile::tempdir().expect("tempdir");
        let reach = LocalSession::new("localhost", dir.path().join("nope.sock"));
        let error = reach.ensure().await.expect_err("no socket");
        assert_eq!(error.kind, ReachKind::NoSession);
        assert!(error.message.contains("nope.sock"), "{error}");
    }

    /// A bridge that cannot even spawn `ssh` has no family: roost's `Local` arm
    /// is this side's own failure, and there is no remedy on the far host.
    #[tokio::test]
    async fn a_bridge_that_cannot_spawn_ssh_has_no_family() {
        let dir = tempfile::tempdir().expect("tempdir");
        let bridge = faked_bridge("mini8", dir.path().join("no-such-ssh"));
        let error = bridge.ensure().await.expect_err("there is no ssh");
        assert_eq!(error.kind, ReachKind::Other);
    }

    /// A reach whose `ensure` always fails the same way, and which reports one
    /// fixed transport record.
    ///
    /// The two halves are independent on purpose: that is exactly the live
    /// shape — a transport that recorded something once, and a loop that keeps
    /// failing afterwards for reasons of its own — and it is what makes the
    /// watermark observable without a real `ssh`.
    struct ScriptedReach {
        fallback: ReachError,
        recorded: Option<RecordedReach>,
    }

    #[async_trait::async_trait]
    impl RoostReach for ScriptedReach {
        fn label(&self) -> &str {
            "scripted"
        }

        async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
            Err(self.fallback.clone())
        }

        async fn invalidate(&self) {}

        async fn last_error(&self) -> Option<RecordedReach> {
            self.recorded.clone()
        }
    }

    /// A watcher whose backoff is scripted away, so a test can watch several
    /// failed attempts in its own time.
    fn watch_failing(
        reach: Arc<dyn RoostReach>,
        attempts: usize,
    ) -> (
        RoostWatcher,
        mpsc::UnboundedReceiver<RoostUpdate>,
        Arc<ScriptedSleeper>,
    ) {
        let (sleeper, _waits) = ScriptedSleeper::new(attempts);
        let (watcher, rx) = RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost-host".to_string(),
            RoostWatcherOptions::default(),
            BackoffSleeper {
                scripted: Some(sleeper.clone()),
            },
        );
        (watcher, rx, sleeper)
    }

    async fn next_down_full(rx: &mut mpsc::UnboundedReceiver<RoostUpdate>) -> (String, ReachKind) {
        match next_update(rx).await {
            RoostUpdate::Down { reason, kind } => (reason, kind),
            other => panic!("expected Down, got {other:?}"),
        }
    }

    /// **Every kind survives to the row.** A reach that fails typed is reported
    /// typed: this is the half of plan 019 §3.6 that has nothing to do with a
    /// tunnel, and it is what a probe result turned into an `UnreachableReach`
    /// rides out on.
    #[tokio::test]
    async fn every_reach_error_kind_reaches_down_kind() {
        for kind in [
            ReachKind::NotInstalled,
            ReachKind::NoSession,
            ReachKind::Unreachable,
            ReachKind::Other,
        ] {
            let reach = UnreachableReach::typed(
                "mini3",
                ReachError::new(kind, format!("the {} case", kind.as_str())),
            );
            let (watcher, mut rx, _sleeper) = watch_failing(Arc::new(reach), 1);
            let (reason, got) = next_down_full(&mut rx).await;
            assert_eq!(got, kind, "kind for {reason}");
            assert_eq!(reason, format!("the {} case", kind.as_str()));
            watcher.stop();
        }
    }

    /// **A record is news exactly once.** The first `Down` after a classified
    /// failure carries it; the second carries the loop's own generic reason,
    /// because nothing new has failed since.
    ///
    /// This is the watermark, observed through the real loop. Without it,
    /// `last_error` — which roost never clears for a tunnel's life — would make
    /// every later drop on that transport report the first one's family, and a
    /// client would keep offering "start a session" long after the reason for
    /// the row being down had changed.
    #[tokio::test]
    async fn a_stale_record_is_not_re_reported_as_a_fresh_reason() {
        let reach = ScriptedReach {
            fallback: ReachError::other("the connection closed"),
            recorded: Some(RecordedReach {
                generation: 7,
                error: ReachError::new(ReachKind::NoSession, "mini3 has no roost session running"),
            }),
        };
        let (watcher, mut rx, _sleeper) = watch_failing(Arc::new(reach), 4);

        let (reason, kind) = next_down_full(&mut rx).await;
        assert_eq!(kind, ReachKind::NoSession);
        assert_eq!(reason, "mini3 has no roost session running");

        // Generation 7 again, and 7 is not greater than 7.
        let (reason, kind) = next_down_full(&mut rx).await;
        assert_eq!(
            kind,
            ReachKind::Other,
            "a generation already reported is not news"
        );
        assert_eq!(reason, "the connection closed");
        watcher.stop();
    }

    /// The other side of the same rule: a **newer** generation IS news, even
    /// though one was already shown.
    ///
    /// Asserted against the overlay directly rather than through the loop,
    /// because the interesting sequence is three records in a row and a watcher
    /// cannot be handed three different transports.
    #[tokio::test]
    async fn a_newer_generation_replaces_the_reason_and_advances_the_watermark() {
        let generic = || ReachError::other("the connection closed");
        let mut seen = 0;

        let reach = ScriptedReach {
            fallback: generic(),
            recorded: Some(RecordedReach {
                generation: 3,
                error: ReachError::new(ReachKind::NoSession, "no session"),
            }),
        };
        let first = overlay_reach_reason(&reach, &mut seen, generic()).await;
        assert_eq!(first.kind, ReachKind::NoSession);
        assert_eq!(seen, 3, "the watermark advances to what was shown");

        // Older than the watermark: a straggler from an exec that died before
        // the one already reported.
        let stale = ScriptedReach {
            fallback: generic(),
            recorded: Some(RecordedReach {
                generation: 2,
                error: ReachError::new(ReachKind::NotInstalled, "not installed"),
            }),
        };
        let second = overlay_reach_reason(&stale, &mut seen, generic()).await;
        assert_eq!(second.kind, ReachKind::Other);
        assert_eq!(seen, 3, "a stale record does not move the watermark");

        // Newer: the ladder fell off its end this time.
        let newer = ScriptedReach {
            fallback: generic(),
            recorded: Some(RecordedReach {
                generation: 4,
                error: ReachError::new(ReachKind::NotInstalled, "not installed"),
            }),
        };
        let third = overlay_reach_reason(&newer, &mut seen, generic()).await;
        assert_eq!(third.kind, ReachKind::NotInstalled);
        assert_eq!(third.message, "not installed");
        assert_eq!(seen, 4);
    }

    /// A tunnel with a fixed record, at **roost's own** generation — the number
    /// that is per `SshTunnel` and restarts at the bottom in every new one.
    struct ScriptedTunnel {
        socket: PathBuf,
        recorded: Option<RecordedReach>,
    }

    #[async_trait::async_trait]
    impl Tunnel for ScriptedTunnel {
        fn bridge_socket(&self) -> &Path {
            &self.socket
        }

        fn last_error(&self) -> Option<RecordedReach> {
            self.recorded.clone()
        }

        async fn shutdown(&self) {}
    }

    /// Hands out one scripted tunnel per `open`, in order — a bridge's whole
    /// life without an `ssh` in it.
    struct ScriptedOpener {
        socket: PathBuf,
        tunnels: std::sync::Mutex<std::collections::VecDeque<Option<RecordedReach>>>,
    }

    impl ScriptedOpener {
        fn new(
            socket: PathBuf,
            tunnels: impl IntoIterator<Item = Option<RecordedReach>>,
        ) -> Arc<ScriptedOpener> {
            Arc::new(ScriptedOpener {
                socket,
                tunnels: std::sync::Mutex::new(tunnels.into_iter().collect()),
            })
        }
    }

    #[async_trait::async_trait]
    impl TunnelOpener for ScriptedOpener {
        async fn open(
            &self,
            _host_id: &str,
            _target: &SshTarget,
            _options: SshTunnelOptions,
        ) -> Result<Box<dyn Tunnel>, ReachError> {
            let recorded = self
                .tunnels
                .lock()
                .expect("the scripted opener")
                .pop_front()
                .expect("the test scripted a tunnel for this open");
            Ok(Box::new(ScriptedTunnel {
                socket: self.socket.clone(),
                recorded,
            }))
        }
    }

    /// **A record from a REPLACED tunnel is news, however low its number is.**
    ///
    /// The regression test for the watermark's other end. A watcher's `seen` is
    /// per watcher and outlives any number of transports; roost's generation
    /// counter lives on the `SshTunnel` and starts again at the bottom in each
    /// one. So a bridge that reached generation 10, was invalidated, and came
    /// back as a fresh tunnel reporting `not-installed` at its own generation 1
    /// would have that record dropped as "already shown" — and the client would
    /// be told the generic connection failure, with no install offered, for a
    /// host that plainly has no `roost-session` on it.
    ///
    /// The bridge is the real one; only the transport under it is scripted,
    /// because the fact under test is precisely what a `SshTunnel` replacement
    /// does to the numbering.
    #[tokio::test]
    async fn a_record_from_a_replaced_tunnel_is_not_discarded_as_stale() {
        let dir = tempfile::tempdir().expect("tempdir");
        let opener = ScriptedOpener::new(
            dir.path().join("bridge.sock"),
            [
                Some(RecordedReach {
                    generation: 10,
                    error: ReachError::new(ReachKind::NoSession, "mini9 has no roost session"),
                }),
                // Tunnel B's FIRST failed exec. Roost numbers it from the
                // bottom because it is a different tunnel.
                Some(RecordedReach {
                    generation: 1,
                    error: ReachError::new(
                        ReachKind::NotInstalled,
                        "roost-session: command not found",
                    ),
                }),
            ],
        );
        let bridge = SshBridge::new(
            &entry("mini9"),
            SshBridgeOptions {
                opener: opener.clone(),
                config_paths: Some(SshConfigPaths {
                    user: None,
                    system: None,
                }),
                ..SshBridgeOptions::default()
            },
        )
        .expect("bridge");

        let generic = || ReachError::other("the connection closed");
        // One watermark across both tunnels — the watcher's own, which is the
        // whole point.
        let mut seen: u64 = 0;

        bridge.ensure().await.expect("tunnel A");
        let first = overlay_reach_reason(&bridge, &mut seen, generic()).await;
        assert_eq!(first.kind, ReachKind::NoSession);
        assert_eq!(seen, 10, "the watermark advances to what was shown");

        // What the loop does after ANY failure: invalidate, then rebuild.
        bridge.invalidate().await;
        bridge.ensure().await.expect("tunnel B");

        let second = overlay_reach_reason(&bridge, &mut seen, generic()).await;
        assert_eq!(
            second.kind,
            ReachKind::NotInstalled,
            "the replacement tunnel's own record was discarded as stale: {}",
            second.message
        );
        assert!(
            seen > 10,
            "the watermark must have advanced past the old tunnel's: {seen}"
        );
    }

    /// **The late record, end to end, over a real [`SshTunnel`].**
    ///
    /// The whole shape plan 019 §3.6 describes, with nothing stubbed between the
    /// watcher and the classification: the master warms up with `true` and
    /// succeeds, a first cycle reads a real inventory through the bridge, and
    /// only THEN does the far side start refusing per-connection execs. The
    /// bridge socket goes on accepting perfectly happily — it is a local
    /// `UnixListener` — so the loop's own evidence is an EOF and nothing more,
    /// and the reason it reports has to come off the tunnel.
    #[tokio::test]
    async fn a_late_no_session_reaches_down_kind_through_a_real_tunnel() {
        let fake = FakeRoost::start().await;
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("ssh.log");
        let ssh = write_fake_ssh(dir.path(), fake.socket_path(), &log);

        let bridge = faked_bridge("mini-late", ssh);
        let (watcher, mut rx) = watch(Arc::new(bridge));
        next_snapshot(&mut rx).await;

        // From here the far side has no session. The master stays up.
        std::fs::write(no_session_marker(dir.path()), "").expect("the marker");
        // End the held stream, which is a resync — not a `Down` — and sends the
        // loop straight back through a connect that now fails.
        fake.close_all();

        let (reason, kind) = next_down_full(&mut rx).await;
        assert_eq!(
            kind,
            ReachKind::NoSession,
            "the tunnel's own classification did not reach the row: {reason}"
        );
        assert!(
            reason.contains("mini-late") || reason.contains("no roost session"),
            "{reason}"
        );
        assert!(
            ran(&log).contains(&"no-session".to_string()),
            "the fake ssh never refused a connection: {:?}",
            ran(&log)
        );
        watcher.stop();
    }

    // ---- the exec runner ----

    /// A stand-in for `ssh` that runs the remote command **locally**, in a
    /// controlled environment.
    ///
    /// The remote command is the last argument, which is what
    /// [`SshExec::argv`] puts there; stdin, stdout and stderr are inherited, so
    /// a `/bin/sh -s` really does read its script off this process's pipe. That
    /// is the point — the thing under test is the runner's stdio wiring, and a
    /// fake that answered from a table would test nothing about it.
    ///
    /// `env` is a jail: `HOME`, a `PATH` of the test's own making, and
    /// `ROOST_BOOTSTRAP_FS_ROOT`. Without it a probe would walk the ladder into
    /// the developer's own `/usr/bin/roost-session`, which on a Linux box with
    /// roost installed is exactly the binary a "cold host" test must not find.
    ///
    /// ## What this fake CANNOT tell you, and how that gap was closed
    ///
    /// It takes the remote command as its last argument and never parses
    /// options, so it is blind to real `ssh`'s own argv rules — in particular to
    /// what OpenSSH does with the `--` [`SshExec::argv`] puts after the
    /// destination. A fake that emulated that would be a second, worse
    /// implementation of the thing under test, so it does not try; the layout
    /// was checked against a **real** OpenSSH 9.6p1 server over the network
    /// instead, and this is the record of it:
    ///
    /// ```text
    /// $ ssh -o BatchMode=yes mini3 -- /bin/echo hello   → hello      (exit 0)
    /// $ echo 'echo FROM_SCRIPT' | ssh mini3 -- /bin/sh -s → FROM_SCRIPT
    /// ```
    ///
    /// `--` after the destination is consumed by ssh as its option terminator
    /// and never reaches the remote shell — which is why it is safe where it is,
    /// and why it protects a remote command that begins with a dash. The
    /// sibling Go provider (`internal/roostprovider`) ships the same shape.
    fn write_exec_ssh(dir: &Path, env: &[(&str, String)]) -> PathBuf {
        let assignments = env
            .iter()
            .map(|(key, value)| {
                format!("{key}={}", shed_core::rc_agents::shell_quote_always(value))
            })
            .collect::<Vec<_>>()
            .join(" ");
        let script = format!(
            r#"#!/bin/sh
set -u
remote=
prev=
is_exit=0
for arg in "$@"; do
    if [ "$prev" = "-O" ] && [ "$arg" = exit ]; then is_exit=1; fi
    prev="$arg"
    remote="$arg"
done
# `-O exit` addresses the master and execs nothing remote. There is no master
# here, so it is a no-op — but it must not be mistaken for a remote command,
# which is what the destination word at the end would otherwise look like.
if [ "$is_exit" -eq 1 ]; then exit 0; fi
exec {env_bin} -i {assignments} /bin/sh -c "$remote"
"#,
            env_bin = real_tool("env").display(),
        );
        write_staged_script(dir, "exec-ssh", script)
    }

    /// Where a utility the rig needs actually lives. A `PATH` lookup will not
    /// do: the rigs below hand their children a `PATH` of their own.
    fn real_tool(name: &str) -> PathBuf {
        ["/usr/bin", "/bin"]
            .iter()
            .map(|dir| Path::new(dir).join(name))
            .find(|path| path.exists())
            .unwrap_or_else(|| panic!("the rig needs {name} in /usr/bin or /bin"))
    }

    fn write_exec(path: &Path, body: &str) {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::create_dir_all(path.parent().expect("a parent")).expect("mkdir");
        std::fs::write(path, body).expect("write");
        let mut perms = std::fs::metadata(path).expect("stat").permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(path, perms).expect("chmod");
    }

    fn exec_options(ssh: PathBuf) -> SshBridgeOptions {
        SshBridgeOptions {
            ssh_bin: Some(ssh),
            // A developer's own `~/.ssh/config` must not reach a fake ssh.
            config_paths: Some(SshConfigPaths {
                user: None,
                system: None,
            }),
            ..SshBridgeOptions::default()
        }
    }

    /// The argv is the wire shape, so it is asserted as one. A test that only
    /// checked "the command ran" would pass with the `ControlPath` quietly
    /// dropped, and every step of a bootstrap would then pay its own handshake.
    #[test]
    fn an_ssh_exec_pins_a_private_control_master_and_a_pinned_host_key() {
        let dir = tempfile::tempdir().expect("tempdir");
        let mut pinned = entry("mini3");
        pinned.user = Some("shed".to_string());
        pinned.ssh_port = 2222;
        pinned.known_hosts = Some("/k/known_hosts".to_string());
        let exec = SshExec::new(&pinned, &exec_options(dir.path().join("ssh"))).expect("exec");

        let argv = exec.argv("/bin/sh -s");
        let joined = argv.join(" ");
        assert!(joined.contains("ControlMaster=auto"), "{joined}");
        assert!(joined.contains("ControlPersist=60s"), "{joined}");
        assert!(joined.contains("BatchMode=yes"), "{joined}");
        assert!(joined.contains("ConnectTimeout=5"), "{joined}");
        assert!(argv.contains(&"-T".to_string()), "{joined}");
        // The control socket is this runner's own and lives in a directory that
        // is NOT in roost's `roost-ssh-*` sweep namespace.
        let ctl = argv
            .iter()
            .find(|arg| arg.starts_with("-S") || arg.contains("/c"))
            .cloned()
            .unwrap_or_default();
        assert!(!ctl.contains("roost-ssh-"), "{ctl}");
        // Destination, `--`, then the command. In that order, or a remote
        // command beginning with a dash would be read as a flag.
        assert_eq!(
            &argv[argv.len() - 3..],
            &[
                "shed@mini3".to_string(),
                "--".to_string(),
                "/bin/sh -s".to_string()
            ]
        );
        assert!(joined.contains("-p 2222"), "{joined}");

        // The host-key pin is in the config `-F` names, reached through roost's
        // own generated wrapper.
        let config = std::fs::read_to_string(&exec.config_path).expect("the generated config");
        let included = config
            .lines()
            .find_map(|line| line.trim().strip_prefix("Include "))
            .expect("the generated config includes the pin")
            .trim_matches('"')
            .to_string();
        let pin = std::fs::read_to_string(&included).expect("the pin file");
        assert!(pin.contains("StrictHostKeyChecking yes"), "{pin}");
        assert!(pin.contains("/k/known_hosts"), "{pin}");
    }

    /// The default port is NOT forced, so a `~/.ssh/config` `Port` for that host
    /// still wins — the same rule the RC argv builder follows.
    #[test]
    fn an_ssh_exec_leaves_port_22_to_the_users_own_config() {
        let dir = tempfile::tempdir().expect("tempdir");
        let exec =
            SshExec::new(&entry("mini3"), &exec_options(dir.path().join("ssh"))).expect("exec");
        assert!(
            !exec.argv("true").contains(&"-p".to_string()),
            "{:?}",
            exec.argv("true")
        );
    }

    /// **A script on stdin, through a real subprocess.** `/bin/sh -s` is the
    /// remote command every roost bootstrap script runs as, and the script
    /// itself arrives as DATA on stdin — which is the discipline that makes a
    /// path with an apostrophe in it a non-event.
    #[tokio::test]
    async fn an_ssh_exec_feeds_a_script_to_sh_on_stdin() {
        let dir = tempfile::tempdir().expect("tempdir");
        let home = dir.path().join("home");
        std::fs::create_dir_all(&home).expect("mkdir home");
        let ssh = write_exec_ssh(
            dir.path(),
            &[
                ("HOME", home.display().to_string()),
                ("PATH", "/usr/bin:/bin".to_string()),
            ],
        );
        let exec = SshExec::new(&entry("mini3"), &exec_options(ssh)).expect("exec");

        let outcome = exec
            .run(
                "/bin/sh -s",
                // An apostrophe in the payload, because that is what the
                // script-on-stdin convention exists to make boring.
                Stdin::Bytes(b"printf 'it'\\''s %s' \"$HOME\"".to_vec()),
                Duration::from_secs(10),
                4096,
                true,
            )
            .await;
        let Outcome::Exec { exit, stdout, .. } = outcome else {
            panic!("an exec step answers with an exec outcome");
        };
        assert_eq!(exit, Some(0));
        assert_eq!(
            String::from_utf8_lossy(&stdout),
            format!("it's {}", home.display())
        );
    }

    /// A step that fails keeps its status and the **tail** of its stderr, and a
    /// step that asked for no stdout gets none however much the far side echoed.
    #[tokio::test]
    async fn an_ssh_exec_keeps_the_status_the_stderr_tail_and_honours_no_capture() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ssh = write_exec_ssh(dir.path(), &[("PATH", "/usr/bin:/bin".to_string())]);
        let exec = SshExec::new(&entry("mini3"), &exec_options(ssh)).expect("exec");

        // A banner far bigger than the tail cap, and the diagnosis last: an
        // `ssh` that fails prints the far side's motd first and its own error at
        // the end, so a cap taken off the FRONT keeps the part nobody needs and
        // drops the only line worth quoting.
        let Outcome::Exec {
            exit, stderr_tail, ..
        } = exec
            .run(
                "/bin/sh -s",
                Stdin::Bytes(
                    b"i=0; while [ $i -lt 400 ]; do printf 'banner-%s-aaaaaaaaaaaaaaaaaaaa\\n' \"$i\" >&2; i=$((i+1)); done; printf 'diagnosis\\n' >&2; exit 3"
                        .to_vec(),
                ),
                Duration::from_secs(10),
                4096,
                true,
            )
            .await
        else {
            panic!("an exec outcome");
        };
        assert_eq!(exit, Some(3));
        assert!(
            stderr_tail.len() <= STDERR_TAIL_CAP,
            "{}",
            stderr_tail.len()
        );
        assert!(
            stderr_tail.trim_end().ends_with("diagnosis"),
            "{stderr_tail}"
        );
        assert!(
            !stderr_tail.contains("banner-0-"),
            "the cap kept the head of the banner instead of the tail"
        );

        let Outcome::Exec { exit, stdout, .. } = exec
            .run(
                "/bin/sh -s",
                Stdin::Bytes(b"printf 'lots and lots of echoed bytes'".to_vec()),
                Duration::from_secs(10),
                4096,
                false,
            )
            .await
        else {
            panic!("an exec outcome");
        };
        assert_eq!(exit, Some(0));
        assert!(
            stdout.is_empty(),
            "a stream step must not accumulate the far side's echo: {stdout:?}"
        );
    }

    /// **The cap is a bound on what is READ, not a trim on what was kept.**
    ///
    /// The distinction is the whole finding: `read_to_end` followed by a
    /// truncation reports the same 4 KiB and holds however many megabytes the
    /// far side felt like sending, at pipe throughput, for the whole step
    /// budget. A remote step really can stream stderr without end — `tee`
    /// complaining once per block about a full disk is the ordinary way it
    /// happens on the install path, and an `sh -x` on a chatty script is the
    /// other.
    ///
    /// The buffer's own `capacity` is the honest witness: it is what the
    /// process is holding, and under the defect it would be the size of the
    /// flood rather than of the cap. Asserted here rather than through
    /// [`SshExec::run`] because the outcome's `String` cannot say what the read
    /// behind it did — the very reason the defect survived a passing tail test.
    #[tokio::test]
    async fn a_stderr_flood_is_bounded_while_it_streams_not_trimmed_afterwards() {
        use tokio::io::AsyncReadExt as _;

        // Four megabytes of banner, then the sentence that matters. Generated,
        // not allocated: a `Vec` of the flood would put the very allocation
        // this is about in the test itself.
        const FLOOD: u64 = 4 * 1024 * 1024;
        let mut stream = tokio::io::repeat(b'x')
            .take(FLOOD)
            .chain(&b"diagnosis\n"[..]);

        let held = rolling_tail(&mut stream, STDERR_TAIL_CAP).await;

        assert_eq!(held.len(), STDERR_TAIL_CAP, "the tail is the cap, filled");
        assert!(
            held.ends_with(b"diagnosis\n"),
            "the LAST bytes are what survived, not the first"
        );
        assert!(
            held.capacity() <= 64 * 1024,
            "the read grew with the flood instead of staying bounded: {} bytes held for a \
             {STDERR_TAIL_CAP}-byte cap",
            held.capacity()
        );
    }

    /// The same bound, wired: a far side that floods stderr still ends with its
    /// status, its diagnosis and a tail no bigger than the cap.
    ///
    /// The leg [`a_stderr_flood_is_bounded_while_it_streams_not_trimmed_afterwards`]
    /// cannot cover — that a real child's stderr goes through this path at all,
    /// and that bounding it cost the step neither its exit status nor its
    /// diagnosis. **Its honest limit**, stated because the reviewer's point was
    /// exactly this: an outcome carries a `String`, so this can only see what
    /// was *kept*. Nothing asserted here would notice the read behind it going
    /// unbounded again — that claim lives one level down, where the buffer
    /// itself is visible.
    #[tokio::test]
    async fn an_ssh_exec_keeps_its_verdict_through_a_stderr_flood() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ssh = write_exec_ssh(dir.path(), &[("PATH", "/usr/bin:/bin".to_string())]);
        let exec = SshExec::new(&entry("mini3"), &exec_options(ssh)).expect("exec");

        let Outcome::Exec {
            exit, stderr_tail, ..
        } = exec
            .run(
                "/bin/sh -s",
                Stdin::Bytes(
                    b"head -c 4000000 /dev/zero | tr '\\0' 'x' >&2; printf 'diagnosis\\n' >&2; exit 3"
                        .to_vec(),
                ),
                Duration::from_secs(30),
                4096,
                true,
            )
            .await
        else {
            panic!("an exec outcome");
        };
        assert_eq!(exit, Some(3), "the flood cost the step its status");
        assert!(
            stderr_tail.len() <= STDERR_TAIL_CAP,
            "{}",
            stderr_tail.len()
        );
        assert!(
            stderr_tail.trim_end().ends_with("diagnosis"),
            "{stderr_tail}"
        );
    }

    /// A budget that expires is **`exit: None`**, which the machines read as a
    /// failure. The one exception to that rule (a clean EOF with a fully-parsed
    /// stdout reported as `Some(0)`) is mobile's, for a transport that has no
    /// exit status to give; a subprocess always has one, so this runner never
    /// applies it and a timeout can never launder into a success.
    ///
    /// ## The remote sleep is SHORT, and that is load-bearing for the suite
    ///
    /// A cancelled `tokio::process::Child` is SIGKILLed by its `Drop`, but the
    /// zombie is reaped by tokio's **`GlobalOrphanQueue`**, which is a
    /// process-wide static that **every** runtime's process driver drains on
    /// every park (`tokio::runtime::process::Driver::park`). A `#[tokio::test]`
    /// runtime ends the moment its test does, so a child that outlives it sits
    /// in that global queue — and every other test's runtime in this binary then
    /// contends on the queue's lock and registers a SIGCHLD listener of its own,
    /// once per park, for as long as the child lives.
    ///
    /// With a 30-second sleep here, two `machine::tests` that wait for their own
    /// `std::process` children to appear timed out on their 60 s bound in **two
    /// runs out of six**; with this one they passed six out of six, and so did
    /// the same six runs with this test skipped altogether. So: a test in this
    /// binary may leave a child behind for a moment, never for half a minute.
    #[tokio::test]
    async fn an_ssh_exec_that_outruns_its_budget_has_no_exit_status() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ssh = write_exec_ssh(dir.path(), &[("PATH", "/usr/bin:/bin".to_string())]);
        let exec = SshExec::new(&entry("mini3"), &exec_options(ssh)).expect("exec");

        let Outcome::Exec {
            exit, stderr_tail, ..
        } = exec
            .run(
                "/bin/sh -s",
                Stdin::Bytes(b"sleep 2".to_vec()),
                Duration::from_millis(200),
                4096,
                true,
            )
            .await
        else {
            panic!("an exec outcome");
        };
        assert_eq!(exit, None);
        assert!(stderr_tail.contains("did not finish"), "{stderr_tail}");
    }

    /// The scratch directory, control socket and generated config go when the
    /// runner does.
    #[tokio::test]
    async fn an_ssh_execs_scratch_directory_is_removed_with_it() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ssh = write_exec_ssh(dir.path(), &[("PATH", "/usr/bin:/bin".to_string())]);
        let exec = SshExec::new(&entry("mini3"), &exec_options(ssh)).expect("exec");
        let scratch = exec.dir.0.clone();
        assert!(scratch.exists());
        drop(exec);
        assert!(
            !scratch.exists(),
            "the scratch directory outlived the runner"
        );
    }

    // ---- the bootstrap runner ----

    /// A shed's roost host is the shed's own ssh identity, restated.
    #[test]
    fn a_shed_reach_entry_is_the_sheds_own_ssh_identity() {
        let server = shed_core::config::ShedServerEntry {
            name: "popos".to_string(),
            host: "10.0.0.4".to_string(),
            ssh_port: 2222,
            ..Default::default()
        };
        let reach = shed_reach_entry(&server, "p019-a");
        assert_eq!(reach.name, "roost:popos/p019-a");
        assert_eq!(reach.host, "10.0.0.4");
        assert_eq!(reach.user.as_deref(), Some("p019-a"));
        assert_eq!(reach.ssh_port, 2222);
        assert_eq!(reach.rc_bin, None);
        assert!(
            reach
                .known_hosts
                .as_deref()
                .is_some_and(|k| k.ends_with("/.shed/known_hosts")),
            "{:?}",
            reach.known_hosts
        );
        // The grammar's token is already qualified, so nothing prefixes it
        // again — a failure about this host reads `roost:popos/p019-a`, never
        // `machine:roost:popos/p019-a`.
        assert_eq!(target_token(&reach.name), "roost:popos/p019-a");
        assert_eq!(target_token("mini3"), "machine:mini3");

        // A server whose `host` was never filled in falls back to its NAME and
        // not to the entry name, which is the grammar token and not a host.
        let bare = shed_core::config::ShedServerEntry {
            name: "popos".to_string(),
            ssh_port: 22,
            ..Default::default()
        };
        assert_eq!(shed_reach_entry(&bare, "p019-a").host, "popos");
    }

    /// The `roost-session` stand-in the bootstrap rig installs and starts: one
    /// `sh` script that answers `identify` and `start` the way the binary does.
    ///
    /// `start` writes the marker [`MarkerReach`] reads, because what makes a
    /// host reachable is the far side starting a session — not a flag this side
    /// sets.
    fn fake_session(protocol: u32) -> String {
        format!(
            r#"#!/bin/sh
case "$1" in
  identify)
    printf '%s\n' '{{"app_version":"0.0.19","session_protocol":{protocol},"libghostty_build":"ghostty-f2d5758f6305867d+snapshot.v1"}}'
    ;;
  start)
    : > "${{HOME}}/.session-running"
    printf 'ready pid=4242\n'
    ;;
  *) exit 2 ;;
esac
"#
        )
    }

    /// The reach a bootstrap rig hands its `Step::Call` steps: exactly the
    /// three-way answer a real transport gives, read off the far side's own
    /// filesystem.
    ///
    /// Not a stub of the machine's input — a stand-in for the TRANSPORT, which
    /// is the layer that actually classifies. A session is reachable once the
    /// far side has started one; a binary with no session is `no-session`; no
    /// binary at all is `not-installed`. Those are the same three states
    /// `ReachError` carries out of a real `SshTunnel`.
    struct MarkerReach {
        home: PathBuf,
        socket: PathBuf,
    }

    #[async_trait::async_trait]
    impl RoostReach for MarkerReach {
        fn label(&self) -> &str {
            "marker"
        }

        async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
            if self.home.join(".session-running").exists() {
                return Ok(RoostEndpoint::Unix(self.socket.clone()));
            }
            if self.home.join(".local/bin/roost-session").is_file() {
                Err(ReachError::new(
                    ReachKind::NoSession,
                    "client-bridge: no session",
                ))
            } else {
                Err(ReachError::new(
                    ReachKind::NotInstalled,
                    "roost-session: command not found",
                ))
            }
        }

        async fn invalidate(&self) {}
    }

    /// The hermetic bootstrap rig: a jailed `$HOME`, a `PATH` of the test's own
    /// making, a fake `ssh` that runs the scripts locally under both, and a real
    /// [`FakeRoost`] behind a [`MarkerReach`].
    struct BootRig {
        _dir: tempfile::TempDir,
        home: PathBuf,
        fake: FakeRoost,
        exec: SshExec,
        reach: Arc<MarkerReach>,
        leases: Arc<RoostLeases>,
    }

    const BOOT_TARGET: &str = "roost:popos/p019-a";

    impl BootRig {
        async fn new() -> BootRig {
            let dir = tempfile::tempdir().expect("tempdir");
            let home = dir.path().join("home");
            let jail = dir.path().join("jail");
            let utils = dir.path().join("utils");
            for path in [&home, &jail, &utils] {
                std::fs::create_dir_all(path).expect("mkdir");
            }
            // The utilities roost's scripts need, and NOTHING else. In
            // particular `$HOME/.local/bin` is deliberately absent, which is what
            // makes the post-install PATH warning fire the way it does on a real
            // shed.
            for tool in ["sh", "mkdir", "rm", "mv", "chmod", "tee", "cat"] {
                std::os::unix::fs::symlink(real_tool(tool), utils.join(tool))
                    .expect("linking a utility");
            }
            // **`uname` is a shim, not the real one, and that is not a
            // convenience.** roost's discovery script asks the far side what it
            // is with `uname -s` / `uname -m`, and `check_os` refuses anything
            // but Linux — correctly, since `roost-session` is built for Linux
            // only. Symlinking the host's `uname` therefore made every runner
            // test that reaches discovery pass on a Linux dev box and fail on
            // macOS with "reports itself as Darwin", which is exactly what CI
            // caught. The far side these tests model is a SHED, so it answers
            // Linux wherever the test happens to run, and a fixed `x86_64` so
            // the arch assertions are deterministic rather than inherited from
            // the runner. C4's rig shims the same tool for the opposite
            // purpose — its unsupported-OS row lies the other way.
            write_staged_script(
                &utils,
                "uname",
                concat!(
                    "#!/bin/sh\n",
                    "case \"$1\" in\n",
                    "  -s) echo Linux ;;\n",
                    "  -m) echo x86_64 ;;\n",
                    "  *) echo Linux ;;\n",
                    "esac\n",
                )
                .to_string(),
            );
            let fake = FakeRoost::start().await;
            let ssh = write_exec_ssh(
                dir.path(),
                &[
                    ("HOME", home.display().to_string()),
                    ("PATH", utils.display().to_string()),
                    ("ROOST_BOOTSTRAP_FS_ROOT", jail.display().to_string()),
                ],
            );
            let entry = MachineEntry {
                name: BOOT_TARGET.to_string(),
                host: "10.0.0.4".to_string(),
                user: Some("p019-a".to_string()),
                ssh_port: 2222,
                rc_bin: None,
                known_hosts: None,
            };
            let exec = SshExec::new(&entry, &exec_options(ssh)).expect("exec");
            let reach = Arc::new(MarkerReach {
                home: home.clone(),
                socket: fake.socket_path().to_path_buf(),
            });
            BootRig {
                _dir: dir,
                home,
                fake,
                exec,
                reach,
                leases: Arc::new(RoostLeases::new()),
            }
        }

        fn runner(&self) -> BootstrapRunner<'_> {
            BootstrapRunner {
                exec: &self.exec,
                reach: self.reach.as_ref(),
                leases: &self.leases,
                target: BOOT_TARGET,
                // The jail. Without it the ladder walks into the developer's own
                // `/usr/bin/roost-session`, which on this host exists.
                jail_fs_root: true,
            }
        }

        fn seed_session(&self, protocol: u32) {
            write_exec(
                &self.home.join(".local/bin/roost-session"),
                &fake_session(protocol),
            );
        }
    }

    /// **A cold host, probed through the real runner.** Every `Step::Exec` is a
    /// real subprocess running roost's own discovery and identity scripts; the
    /// `Step::Call` is a real reach answering the way a transport answers.
    #[tokio::test]
    async fn the_runner_probes_a_cold_host_as_missing_and_not_installed() {
        let rig = BootRig::new().await;
        let probe = rig.runner().probe().await.expect("the probe");
        assert_eq!(
            probe.outcome,
            shed_core::roost::bootstrap::ProbeOutcome::Missing
        );
        assert_eq!(
            probe.session,
            shed_core::roost::bootstrap::SessionState::NotInstalled
        );
        assert_eq!(probe.home, rig.home.display().to_string());
        assert_eq!(
            shed_core::roost::bootstrap::Plan::for_probe(BOOT_TARGET, &probe),
            shed_core::roost::bootstrap::Plan::Install {
                dest: Some(
                    rig.home
                        .join(".local/bin/roost-session")
                        .display()
                        .to_string()
                ),
            }
        );
    }

    /// A compatible binary that nothing is serving is a **Start**, and the probe
    /// says so because the transport said `no-session` rather than `not-found`.
    #[tokio::test]
    async fn the_runner_probes_an_installed_but_stopped_session_as_a_start() {
        let rig = BootRig::new().await;
        rig.seed_session(4);
        let probe = rig.runner().probe().await.expect("the probe");
        assert!(
            matches!(
                probe.outcome,
                shed_core::roost::bootstrap::ProbeOutcome::Compatible { .. }
            ),
            "{:?}",
            probe.outcome
        );
        assert_eq!(
            probe.session,
            shed_core::roost::bootstrap::SessionState::NoSession
        );
        assert_eq!(
            shed_core::roost::bootstrap::Plan::for_probe(BOOT_TARGET, &probe),
            shed_core::roost::bootstrap::Plan::Start {
                path: rig
                    .home
                    .join(".local/bin/roost-session")
                    .display()
                    .to_string(),
            }
        );
    }

    /// **The start, the hooks and the lease, through the real runner.**
    ///
    /// The payoff path in miniature: a shed with a compatible `roost-session`
    /// that is not running gets one started over the exec runner, the hooks
    /// dialogue runs over the client's own connection, and the lease it minted
    /// is kept in the table for the app run.
    #[tokio::test]
    async fn the_runner_starts_a_session_wires_hooks_and_keeps_the_lease() {
        let rig = BootRig::new().await;
        rig.seed_session(4);
        let probe = rig.runner().probe().await.expect("the probe");

        let installed = rig
            .runner()
            .install(
                InstallRequest {
                    target: BOOT_TARGET.to_string(),
                    jail_fs_root: true,
                    fingerprint: probe.fingerprint.clone(),
                    client_label: "shed-desktop".to_string(),
                },
                None,
            )
            .await
            .expect("the install");

        assert!(
            matches!(
                installed.plan,
                shed_core::roost::bootstrap::Plan::Start { .. }
            ),
            "{:?}",
            installed.plan
        );
        assert_eq!(installed.verdict.as_deref(), Some("ready pid=4242"));
        assert!(installed.dest.is_none(), "a start writes no binary");
        // **No PATH warning on a start-only flow**, and that is roost's rule
        // rather than an omission: the warning's whole subject is the file that
        // was just written ("that shell's own roost-session won't be this one"),
        // and a start wrote nothing to contrast with. The round trip is skipped
        // with it.
        assert_eq!(installed.path_warning, None, "{installed:?}");

        let hooks = installed.hooks.expect("a start shed performed wires hooks");
        assert!(hooks.applied(), "{hooks:?}");
        // The op really reached the session, with the arguments plan 019 pins.
        let calls = rig.fake.agent_hooks_calls();
        assert_eq!(calls.len(), 1, "{calls:?}");
        assert_eq!(calls[0]["mode"], "auto");
        assert_eq!(calls[0]["client"], "shed-desktop");

        // The lease outlives the dialogue and is held per target, which is what
        // makes a re-send on the next reconnect possible at all.
        assert!(rig.leases.active(BOOT_TARGET));
        assert_eq!(rig.leases.lease(BOOT_TARGET), rig.fake.lease());
        assert_eq!(rig.fake.lease_label().as_deref(), Some("shed-desktop"));
    }

    /// **shed never takes over.** A session somebody else is driving answers
    /// `already-connected` at the connect, and the dialogue stops there: no
    /// second connect, no `takeover: true`, no hooks op.
    #[tokio::test]
    async fn a_driven_session_is_left_alone() {
        let rig = BootRig::new().await;
        rig.seed_session(4);
        let probe = rig.runner().probe().await.expect("the probe");
        // Somebody else is holding the interactive lease before shed's start.
        rig.fake.take_over("roost-ui");

        let installed = rig
            .runner()
            .install(
                InstallRequest {
                    target: BOOT_TARGET.to_string(),
                    jail_fs_root: true,
                    fingerprint: probe.fingerprint.clone(),
                    client_label: "shed-desktop".to_string(),
                },
                None,
            )
            .await
            .expect("the install still succeeds");

        let hooks = installed.hooks.expect("the dialogue ran");
        assert_eq!(hooks.skipped_code.as_deref(), Some("already-connected"));
        assert!(
            rig.fake.agent_hooks_calls().is_empty(),
            "shed wired hooks against somebody else's session"
        );
        assert_eq!(
            rig.fake.lease_label().as_deref(),
            Some("roost-ui"),
            "shed took the lease from the driver"
        );
        // No lease, so nothing to re-send on the next reconnect.
        assert!(!rig.leases.active(BOOT_TARGET));
    }

    /// **A decision about a session shed has just replaced does not carry into
    /// the new one.** The install's call to
    /// [`RoostLeases::forget`](RoostLeases::forget), from the far end.
    ///
    /// An earlier session on this target was taken over, so shed stepped back
    /// for the run — correctly, while that session was the one being driven.
    /// Then the session goes, and the user consents to shed starting another.
    /// If the surrender rode through, shed would start a session, mint a lease
    /// for it, wire its hooks once and then never re-send them: `mode: auto`
    /// wires only the agents configured at that moment, so every agent set up
    /// afterwards would stay unwired for the rest of the run, silently.
    #[tokio::test]
    async fn a_bootstrap_re_arms_a_target_that_surrendered_an_earlier_session() {
        let rig = BootRig::new().await;
        rig.seed_session(4);
        // The earlier session's dialogue: somebody took the lease.
        rig.leases
            .record(BOOT_TARGET, &hooks_result(None, Some("taken-over")));
        assert!(!rig.leases.active(BOOT_TARGET));

        let probe = rig.runner().probe().await.expect("the probe");
        let installed = rig
            .runner()
            .install(
                InstallRequest {
                    target: BOOT_TARGET.to_string(),
                    jail_fs_root: true,
                    fingerprint: probe.fingerprint.clone(),
                    client_label: "shed-desktop".to_string(),
                },
                None,
            )
            .await
            .expect("the install");

        let hooks = installed.hooks.expect("a start shed performed wires hooks");
        assert!(hooks.applied(), "{hooks:?}");
        assert!(
            rig.leases.active(BOOT_TARGET),
            "the new session's lease was filed under the old session's surrender, so the \
             watcher will never re-send its hooks"
        );
    }

    /// A reach whose `ensure` always hands back an endpoint nothing is
    /// listening on — every dial fails — and whose `last_error` reports one
    /// fixed [`RecordedReach`] until `invalidate` is called, after which it
    /// reports `None`: exactly what a freshly rebuilt `SshTunnel` looks like
    /// (something classified on the OLD transport, nothing yet on the new one).
    ///
    /// The regression double for the dial-failure arms of `call`/`hooks`: those
    /// arms used to return without calling `invalidate`, so the SAME un-rebuilt
    /// transport stayed in place and kept handing back the SAME stale record —
    /// which the per-call watermark reset then turned into "a fresh reason"
    /// every single call, forever. `ensure` failing to dial at all is the
    /// simplest way to force that arm without a real `ssh`.
    struct UnrebuiltDialReach {
        /// Points nowhere a listener will ever bind.
        endpoint: RoostEndpoint,
        /// `Some` until the first `invalidate`, `None` from then on.
        live: std::sync::Mutex<Option<RecordedReach>>,
        invalidations: AtomicUsize,
    }

    impl UnrebuiltDialReach {
        fn new(endpoint: RoostEndpoint, record: RecordedReach) -> Arc<UnrebuiltDialReach> {
            Arc::new(UnrebuiltDialReach {
                endpoint,
                live: std::sync::Mutex::new(Some(record)),
                invalidations: AtomicUsize::new(0),
            })
        }
    }

    #[async_trait::async_trait]
    impl RoostReach for UnrebuiltDialReach {
        fn label(&self) -> &str {
            "unrebuilt-dial"
        }

        async fn ensure(&self) -> Result<RoostEndpoint, ReachError> {
            Ok(self.endpoint.clone())
        }

        async fn invalidate(&self) {
            self.invalidations.fetch_add(1, Ordering::SeqCst);
            *self.live.lock().expect("lock") = None;
        }

        async fn last_error(&self) -> Option<RecordedReach> {
            self.live.lock().expect("lock").clone()
        }
    }

    /// **A dial failure invalidates the reach, and doing so is what keeps the
    /// next call honest.**
    ///
    /// The regression test for both `call`'s and `hooks`'s dial-failure arms —
    /// exercised here through `call`, the smaller of the two seams. Before the
    /// fix, that arm returned its error without calling `invalidate`, so this
    /// reach's one stale [`RecordedReach`] — left over from a transport that
    /// failed BEFORE either of these two calls ran — was still there, unrebuilt,
    /// for the second call. Because `call` resets its own generation watermark
    /// on every call (see the comment on `call` itself), an un-rebuilt transport
    /// made the second call re-read that same old record and report it as its
    /// own fresh reason. With the fix, the first dial failure invalidates the
    /// reach, so by the second call the record is gone and the failure is
    /// reported as the plain transport error it actually is.
    #[tokio::test]
    async fn a_dial_failure_invalidates_the_reach_so_the_next_call_does_not_reuse_its_record() {
        let dir = tempfile::tempdir().expect("tempdir");
        let record = RecordedReach {
            generation: 7,
            error: ReachError::new(ReachKind::NoSession, "mini-x: client-bridge: no session"),
        };
        let reach = UnrebuiltDialReach::new(
            RoostEndpoint::Unix(dir.path().join("no-listener.sock")),
            record,
        );
        let exec =
            SshExec::new(&entry("mini-x"), &exec_options(dir.path().join("ssh"))).expect("exec");
        let leases = RoostLeases::new();
        let runner = BootstrapRunner {
            exec: &exec,
            reach: reach.as_ref(),
            leases: &leases,
            target: "roost:mini-x/user",
            jail_fs_root: true,
        };

        let first = runner
            .call("session.identify", serde_json::json!({}))
            .await
            .expect_err("nothing is listening on the endpoint");
        assert_eq!(
            first.code,
            reach_code::NO_SESSION,
            "the first dial failure should still read the pre-existing record: {first:?}"
        );
        assert_eq!(
            reach.invalidations.load(Ordering::SeqCst),
            1,
            "a dial failure must invalidate the reach so the wire is rebuilt"
        );

        let second = runner
            .call("session.identify", serde_json::json!({}))
            .await
            .expect_err("still nothing is listening");
        assert_ne!(
            second.code, first.code,
            "the second call must not present the first call's already-reported \
             record as a fresh reason: got {second:?} again"
        );
        assert_eq!(
            second.code, "transport",
            "with the record gone, a bare dial failure is a plain transport error: {second:?}"
        );
    }

    // ---- the lease table ----

    fn hooks_result(lease: Option<&str>, skipped: Option<&str>) -> HooksResult {
        HooksResult {
            client_label: "shed-desktop".to_string(),
            lease: lease.map(str::to_string),
            skipped_code: skipped.map(str::to_string),
            ..HooksResult::default()
        }
    }

    /// `taken-over` is terminal; `already-connected` is not.
    ///
    /// The difference is the whole of plan 019 §3.4's lease rule: somebody who
    /// TOOK the lease wires the hooks themselves and shed stops for the run,
    /// while somebody who merely happened to be holding it at this moment may
    /// well be gone by the next reconnect.
    #[test]
    fn the_lease_table_surrenders_on_taken_over_and_not_on_already_connected() {
        let leases = RoostLeases::new();
        leases.record("roost:popos/a", &hooks_result(Some("L1"), None));
        assert!(leases.active("roost:popos/a"));
        assert_eq!(leases.lease("roost:popos/a").as_deref(), Some("L1"));

        leases.record("roost:popos/a", &hooks_result(None, Some("taken-over")));
        assert!(!leases.active("roost:popos/a"));
        assert_eq!(leases.lease("roost:popos/a"), None);
        // And it stays surrendered: a later success cannot re-arm a run shed has
        // stepped back from.
        leases.record("roost:popos/a", &hooks_result(Some("L2"), None));
        assert!(
            !leases.active("roost:popos/a"),
            "a surrendered target re-armed itself"
        );
        assert_eq!(
            leases.lease("roost:popos/a"),
            None,
            "a surrendered target kept a token it has promised not to present"
        );

        let other = RoostLeases::new();
        other.record(
            "roost:popos/b",
            &hooks_result(None, Some("already-connected")),
        );
        assert!(!other.active("roost:popos/b"), "there is no lease to hold");
        other.record("roost:popos/b", &hooks_result(Some("L3"), None));
        assert!(
            other.active("roost:popos/b"),
            "already-connected must not be terminal"
        );
    }

    /// **A result with no lease in it CLEARS the token**, which is the contract
    /// the method's own doc states and the only reading under which a token
    /// cannot outlive its session.
    ///
    /// A lease is good for exactly the session that minted it. Every result
    /// that comes back carrying none is evidence that the session shed's token
    /// belongs to did not accept it, or is not there to — and keeping it makes
    /// the table say `active` about a session that no longer exists.
    #[test]
    fn the_lease_table_forgets_a_token_no_result_confirmed() {
        let leases = RoostLeases::new();
        leases.record("roost:popos/a", &hooks_result(Some("L1"), None));

        // Somebody else is driving right now: no lease came back.
        leases.record(
            "roost:popos/a",
            &hooks_result(None, Some("already-connected")),
        );
        assert_eq!(
            leases.lease("roost:popos/a"),
            None,
            "a token no result confirmed stayed in the table"
        );
        assert!(!leases.active("roost:popos/a"));

        // Same for a dialogue that simply failed — a transport error says
        // nothing about entitlement, but it does not confirm a token either.
        let leases = RoostLeases::new();
        leases.record("roost:popos/b", &hooks_result(Some("L2"), None));
        leases.record(
            "roost:popos/b",
            &HooksResult {
                client_label: "shed-desktop".to_string(),
                error: Some("the connection closed".to_string()),
                ..HooksResult::default()
            },
        );
        assert_eq!(leases.lease("roost:popos/b"), None);
    }

    /// **A stale token does not outlive the session that minted it**, through
    /// the real dialogue.
    ///
    /// S1 mints shed's lease; S1 exits; S2 comes up on the same target with
    /// somebody else driving it. The watcher reconnects and presents the dead
    /// token — S2 has never heard of it — and what must not happen is the table
    /// keeping it: every later reconnect would present it again, for the rest
    /// of the run, against a session it was never valid for.
    #[tokio::test]
    async fn a_lease_whose_session_was_replaced_is_not_presented_again() {
        let fake = FakeRoost::start().await;
        let leases = Arc::new(RoostLeases::new());
        let target = "roost:popos/a";

        // S1 mints, the way a bootstrap's Start would have.
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let minted = conn
            .session_connect(false, Some("shed-desktop"))
            .await
            .expect("a fresh session has no lease");
        drop(conn);
        leases.record(target, &hooks_result(Some(&minted.lease), None));
        assert!(leases.active(target));

        // S1 is gone and S2 is up on the same target — a new session id, no
        // lease, no tombstone — and a roost UI is driving it by the time shed
        // reconnects.
        fake.restart();
        fake.take_over("roost-ui");

        let refresh = HooksRefresh {
            leases: leases.clone(),
            target: target.to_string(),
            client_label: "shed-desktop".to_string(),
        };
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        refresh.refresh(&mut conn).await;
        drop(conn);

        assert!(
            fake.agent_hooks_calls().is_empty(),
            "shed wired hooks against somebody else's session"
        );
        assert_eq!(
            leases.lease(target),
            None,
            "the dead session's token survived the session"
        );
        assert!(
            !leases.active(target),
            "the table still calls a dead lease active"
        );

        // And the next reconnect presents nothing at all — no round trip, no
        // `connect-required`, and the driver keeps their session.
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        refresh.refresh(&mut conn).await;
        assert!(fake.agent_hooks_calls().is_empty());
        assert_eq!(
            fake.lease_label().as_deref(),
            Some("roost-ui"),
            "shed took the lease from the driver"
        );
    }

    /// **A surrender cannot be overtaken.** §3.4 makes `taken-over` permanent
    /// for the run, and "permanent" has to survive a second watcher on the same
    /// target that was already part-way through its own decision.
    ///
    /// The interleaving, which the map's mutex does not prevent because it
    /// guards three separate steps and not the decision: A reads `active` and
    /// the token, B's dialogue comes back `taken-over` and records the
    /// surrender, A sends the token anyway. Here B's turn is held while A
    /// starts, and A must send nothing — not while B holds it, and not after B
    /// has stepped shed back.
    #[tokio::test]
    async fn a_surrender_cannot_be_overtaken_by_a_refresh_that_is_mid_decision() {
        let fake = FakeRoost::start().await;
        let leases = Arc::new(RoostLeases::new());
        let target = "roost:popos/a";

        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let minted = conn
            .session_connect(false, Some("shed-desktop"))
            .await
            .expect("a fresh session has no lease");
        drop(conn);
        leases.record(target, &hooks_result(Some(&minted.lease), None));

        // Watcher B: mid-dialogue, holding this target's turn.
        let gate = leases.gate(target);
        let turn = gate.lock().await;

        // Watcher A: starts now, with an entitlement that is about to stop
        // being true.
        let refresh = HooksRefresh {
            leases: leases.clone(),
            target: target.to_string(),
            client_label: "shed-desktop".to_string(),
        };
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let a = tokio::spawn(async move { refresh.refresh(&mut conn).await });
        // Long enough for a dialogue against a local socket several times over.
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(
            fake.agent_hooks_calls().is_empty(),
            "a refresh sent while another dialogue on the same target was mid-decision"
        );

        // B's dialogue came back `taken-over`.
        leases.record(target, &hooks_result(None, Some("taken-over")));
        drop(turn);

        a.await.expect("the refresh task");
        assert!(
            fake.agent_hooks_calls().is_empty(),
            "a surrendered lease was presented anyway"
        );
        assert!(!leases.active(target), "the surrender did not stick");
    }

    /// **The re-send on every (re)connect**, which is what keeps `mode: auto`
    /// honest: it wires only the agents whose config directory exists at that
    /// moment, so an agent configured later is wired by a LATER call and by
    /// nothing else.
    #[tokio::test]
    async fn a_watcher_re_sends_agent_hooks_on_every_connect_while_the_lease_holds() {
        let fake = FakeRoost::start().await;
        let leases = Arc::new(RoostLeases::new());

        // Mint a lease the way a bootstrap would have, and record it.
        let mut conn = Conn::unix(fake.socket_path()).await.expect("dial");
        let minted = conn
            .session_connect(false, Some("shed-desktop"))
            .await
            .expect("a fresh session has no lease");
        drop(conn);
        leases.record("roost:popos/a", &hooks_result(Some(&minted.lease), None));

        let reach: Arc<dyn RoostReach> = Arc::new(LocalSession::new("popos", fake.socket_path()));
        let (watcher, mut rx) = RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost:popos/a".to_string(),
            RoostWatcherOptions {
                hooks: Some(HooksRefresh {
                    leases: leases.clone(),
                    target: "roost:popos/a".to_string(),
                    client_label: "shed-desktop".to_string(),
                }),
            },
            BackoffSleeper::default(),
        );

        next_snapshot(&mut rx).await;
        assert_eq!(fake.agent_hooks_calls().len(), 1, "the first connect");

        // A reconnect: the stream ends, the loop resyncs, and the hooks go again
        // with the SAME lease — never a second `session.connect`, which would
        // answer `already-connected` against shed's own lease.
        fake.close_all();
        next_snapshot(&mut rx).await;
        let calls = fake.agent_hooks_calls();
        assert_eq!(calls.len(), 2, "the reconnect did not re-send: {calls:?}");
        assert!(calls.iter().all(|call| call["mode"] == "auto"));
        watcher.stop();
    }

    /// A watcher with no hooks entry is a **pure observer**, which is what every
    /// watcher over somebody else's machine must stay.
    #[tokio::test]
    async fn a_watcher_without_a_hooks_entry_wires_nothing() {
        let fake = FakeRoost::start().await;
        let reach: Arc<dyn RoostReach> = Arc::new(LocalSession::new("popos", fake.socket_path()));
        let (watcher, mut rx) = watch(reach);
        next_snapshot(&mut rx).await;
        stays_silent(&mut rx).await;
        assert!(
            fake.agent_hooks_calls().is_empty(),
            "an observer wired somebody's hooks"
        );
        assert_eq!(fake.lease(), None, "an observer took a lease");
        watcher.stop();
    }

    /// **A hooks entry with no lease behind it still wires nothing**, which is a
    /// different claim and the one that actually needs a guard.
    ///
    /// The refresh is a RE-send: it presents a lease shed already holds. A
    /// watcher that minted one instead would be connecting to a session shed did
    /// not start — somebody else's terminal multiplexer — on the strength of
    /// having been asked to watch it. That is the difference between keeping a
    /// host's hooks current and taking a host over.
    #[tokio::test]
    async fn a_hooks_entry_with_no_lease_mints_nothing() {
        let fake = FakeRoost::start().await;
        let reach: Arc<dyn RoostReach> = Arc::new(LocalSession::new("popos", fake.socket_path()));
        let (watcher, mut rx) = RoostWatcher::spawn_inner(
            &tokio::runtime::Handle::current(),
            reach,
            "roost:popos/a".to_string(),
            RoostWatcherOptions {
                hooks: Some(HooksRefresh {
                    // Empty: nothing has bootstrapped this host.
                    leases: Arc::new(RoostLeases::new()),
                    target: "roost:popos/a".to_string(),
                    client_label: "shed-desktop".to_string(),
                }),
            },
            BackoffSleeper::default(),
        );
        next_snapshot(&mut rx).await;
        stays_silent(&mut rx).await;
        assert!(
            fake.agent_hooks_calls().is_empty(),
            "a watcher with no lease wired hooks anyway"
        );
        assert_eq!(fake.lease(), None, "a watcher minted a lease of its own");
        watcher.stop();
    }
}
