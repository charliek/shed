//! The embedded broker bridge (leg 3a.2) — an in-process replacement for the
//! [`HostAgentClient`](crate::host_agent::HostAgentClient) UDS client, backed by a
//! `shed_broker::Supervisor` running on the app's own tokio runtime instead of a
//! socket to a standalone `shed-host-agent` daemon.
//!
//! It presents the SAME Coordinator-facing surface the external-mode client does:
//!   * an [`mpsc::UnboundedReceiver<HostAgentEvent>`] the [`Coordinator`] consumes via
//!     [`Coordinator::spawn`](crate::coordinator::Coordinator::spawn) — carrying a
//!     synthesized `Connected(HelloAck)` (so the approvals UI sections light up exactly
//!     as in external mode), plus each broker approval-request as a `Frame` and each
//!     audit row (fanned in from the broker) as an `Event` frame;
//!   * a synchronous [`Responder`] the Coordinator calls inside its atomic decision
//!     handler (bounded-`mpsc` handoff → the bridge's async side completes the matching
//!     `oneshot`, so nothing awaits under the actor lock);
//!   * a [`TokenMinter`] adapter over the broker's `ControlTokenProvider` for the
//!     Backend's secure-server credential vend — mtls-capable (plan 002 §7 P2): it
//!     relays the shed-core provider's CSR into the broker's control-credential mint,
//!     so an embedded-broker app enrolls against an mtls server exactly as an
//!     external-agent one does.
//!
//! **Deadlock-free** (the §3.2 analysis): the Coordinator is two-phase (never awaits the
//! gate inline) and `Responder` is synchronous, so `AppGate`-awaits-oneshot /
//! `Responder`-completes-oneshot has no cycle.
//!
//! **No replay buffer:** the UDS hello's `replay_events: 50` has no embedded analogue.
//! Post-restart activity comes from the app's own `AuditStore`, not a broker ring —
//! documented and accepted (§3.2).

use std::collections::HashMap;
use std::io;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::{mpsc, oneshot, watch};

use shed_core::approval::protocol::{AuditEventFrame, HostAgentInbound};
use shed_core::approval::{
    ApprovalDecision, ApprovalRequest, DecidedBy, HelloAck, CAP_CREDENTIAL_GET,
};
use shed_core::http::ShedError;
use shed_core::token::{CredentialRequest, MintedCredential, MintedToken, TokenMinter};

use shed_broker::approval::{select_builtin_gate, ApprovalGate, ApprovalOutcome};
use shed_broker::audit::{AuditEntry as BrokerAuditEntry, AuditFanout, AuditSink, JsonlAuditSink};
use shed_broker::config::{
    DiscoveryConfig, HostAgentConfig, DEFAULT_DISCOVERY_SOURCE, NS_AWS_CREDENTIALS,
    NS_DOCKER_CREDENTIALS, NS_EGRESS, NS_SSH_AGENT,
};
use shed_broker::controltoken::{
    ControlCredentialMinter, ControlTokenMinter, ControlTokenProvider, MintedControlCredential,
};
use shed_broker::status::ServerHealth;
use shed_broker::supervisor::{SharedDeps, Supervisor};
use shed_broker::{
    aws_backend, bus, desktop_socket_path, docker_backend, minter as broker_minter, socket_is_live,
    ssh_backend, status_socket_path, watcher,
};

use crate::host_agent::HostAgentEvent;
use crate::timefmt;
// The credential-shape rules are the UDS minter's, shared verbatim — see
// `map_embedded_credential`.
use crate::token_minter::{credential_from_parts, expiry_unix, minted_token, CredentialParts};
use crate::traits::{system_clock, ClockRef, Responder, ResponderRef};

/// The known_hosts pin the control-token minter bootstraps over (parity with the
/// daemon's `main.rs`, which hard-codes the same trust file `shed server add` wrote).
const KNOWN_HOSTS_PATH: &str = "~/.shed/known_hosts";

/// Bounds the Responder→bridge decision handoff. A synchronous `try_send` under the
/// Coordinator's atomic handler must never block; on the (pathological) full-queue case
/// the decision is dropped and the `AppGate` fails closed via its timeout — the same
/// backpressure posture as the daemon's bounded desktop writer.
///
/// **Why 64 is enough / why the bounded-drop is accepted:** approvals are human-paced —
/// each in-flight request blocks on a user decision, so the realistic depth is one or a
/// small handful, never 64. A full (or closed) queue means the async consumer is wedged
/// or gone; dropping the decision there is fail-CLOSED, never a bypass: the `AppGate`
/// times out and the bus denies. That parity with the daemon's UDS
/// respond-while-disconnected no-op is the reason the drop is a documented, accepted
/// posture rather than a hard error — the drop path emits a loud `BusLog::warn`
/// (see [`BridgeResponder::respond`]) so an audited approve that never reached the gate
/// stays diagnosable.
const RESPOND_QUEUE_CAP: usize = 64;

fn new_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

// ---------------------------------------------------------------------------
// Typed error state (§3.4)
// ---------------------------------------------------------------------------

/// A broker-startup failure that fails CLOSED without taking the app down (deliberate
/// divergence from the daemon, which exits 1). The error surfaces in `broker.status`
/// (C3); the app keeps running and token minting still works.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BrokerError {
    /// `extensions.yaml` present but malformed/invalid (the daemon would exit 1).
    Config(String),
    /// SSH backend resolve failed at spawn (unknown `ssh.mode`, or an explicit
    /// `agent-forward` with `$SSH_AUTH_SOCK` unset).
    SshBackend(String),
    /// [`EmbeddedHostAgent::spawn`] was called more than once. `spawn` is once-only —
    /// a second call must NOT build a second `Supervisor` + watch loop (dual brokers
    /// fighting for the same servers, 409s) — so it returns this WITHOUT side effects.
    AlreadySpawned,
}

impl std::fmt::Display for BrokerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            BrokerError::Config(m) => write!(f, "broker config error: {m}"),
            BrokerError::SshBackend(m) => write!(f, "broker ssh backend error: {m}"),
            BrokerError::AlreadySpawned => write!(f, "embedded broker already spawned"),
        }
    }
}

impl std::error::Error for BrokerError {}

/// The config the embedded broker will run on, tagged with how it was obtained so
/// `broker.status`/tray can distinguish a fresh-install synthesized default from a
/// user-authored `extensions.yaml`.
#[derive(Debug, Clone)]
pub enum BrokerConfig {
    /// A valid `extensions.yaml` was found and parsed — honored exactly like the daemon.
    Loaded(HostAgentConfig),
    /// No `extensions.yaml` (fresh desktop install) — the synthesized desktop default.
    Synthesized(HostAgentConfig),
}

impl BrokerConfig {
    /// The underlying config, regardless of provenance.
    pub fn config(&self) -> &HostAgentConfig {
        match self {
            BrokerConfig::Loaded(c) | BrokerConfig::Synthesized(c) => c,
        }
    }

    /// Consume into the underlying config.
    pub fn into_config(self) -> HostAgentConfig {
        match self {
            BrokerConfig::Loaded(c) | BrokerConfig::Synthesized(c) => c,
        }
    }
}

/// Load `extensions.yaml`, or synthesize the fresh-install default (§3.4):
///   * present + valid → [`BrokerConfig::Loaded`] (honored exactly like the daemon);
///   * missing → [`BrokerConfig::Synthesized`] (discovery-mode select-all over
///     `~/.shed/config.yaml`, ssh policy `shed-desktop` → the app gate, `ssh.mode: ""`
///     auto-detect, aws/docker as under an empty config, egress on);
///   * present but malformed/invalid → [`Err`] (broker NOT started; app keeps running).
///
/// The missing-file discrimination keys on [`io::ErrorKind::NotFound`], so a genuinely
/// unreadable-or-malformed file is a `Config` error rather than a silent synthesis.
pub fn load_or_synthesize(config_path: &str) -> Result<BrokerConfig, BrokerError> {
    match HostAgentConfig::load(config_path) {
        Ok(cfg) => Ok(BrokerConfig::Loaded(cfg)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(BrokerConfig::Synthesized(
            synthesize_default(DEFAULT_DISCOVERY_SOURCE),
        )),
        Err(e) => Err(BrokerError::Config(e.to_string())),
    }
}

/// Build the fresh-install default through the SAME parser/validation path
/// `HostAgentConfig::load` uses (a real config text, not a hand-rolled struct — parity
/// is structural). `discovery_source` overrides the shed-CLI config path (production
/// passes [`DEFAULT_DISCOVERY_SOURCE`]; tests point it at a temp file).
pub(crate) fn synthesize_default(discovery_source: &str) -> HostAgentConfig {
    let yaml = format!(
        "ssh:\n  approval:\n    policy: shed-desktop\ndiscovery:\n  source: {discovery_source}\n"
    );
    // A known-good literal: `try_parse` + `validate` mirror `load` exactly. A failure
    // here is a bug in this constant, not a runtime input, so `expect` is correct.
    let cfg = HostAgentConfig::try_parse(&yaml).expect("synthesized default parses");
    cfg.validate().expect("synthesized default validates");
    cfg
}

// ---------------------------------------------------------------------------
// Mode detection (§3.3)
// ---------------------------------------------------------------------------

/// The daemon-socket liveness evidence auto-detect keys on, surfaced verbatim in
/// `broker.status` so the UI can explain why `auto` chose what it did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ModeProbe {
    pub desktop_socket_live: bool,
    pub status_socket_live: bool,
}

/// What the three-way probe detected (before the pref overlay).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetectedMode {
    /// A desktop daemon owns the approval channel — use today's UDS client, unchanged.
    External,
    /// A deliberately-run headless daemon owns brokering — do NOT start the bus broker
    /// (no 409 fights) and get no UDS approvals; token minting falls back to in-process.
    HeadlessCoexist,
    /// No daemon — start the in-process broker.
    Embedded,
}

/// The persisted user preference (default `auto`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ModePref {
    Auto,
    Embedded,
    External,
}

/// The effective mode after the pref overlay — fixed for the process lifetime.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EffectiveMode {
    External,
    HeadlessCoexist,
    Embedded,
}

/// The resolved mode plus the probe evidence that produced it (for `broker.status`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ResolvedMode {
    pub mode: EffectiveMode,
    pub probe: ModeProbe,
}

/// Probe BOTH daemon sockets (§3.1 liveness probes) at their well-known paths.
///
/// **Blocking:** each probe is a synchronous `connect(2)` (via
/// [`shed_broker::socket_is_live`]), bounded to 500 ms per socket by
/// `SOCKET_PROBE_TIMEOUT`. A caller ON an async runtime should run this inside
/// [`tokio::task::spawn_blocking`] so a pathological peer (full accept backlog) can't
/// occupy a runtime worker for up to a second. The signature stays sync deliberately —
/// this is the startup mode-probe, typically run before the runtime spins up.
pub fn probe_sockets() -> ModeProbe {
    ModeProbe {
        desktop_socket_live: socket_is_live(&desktop_socket_path()),
        status_socket_live: socket_is_live(&status_socket_path()),
    }
}

/// Sibling status-socket filename the daemon binds next to the desktop socket
/// (parity with `status_socket_path()`'s basename).
const STATUS_SOCKET_FILE: &str = "host-agent-status.sock";

/// Probe the daemon sockets the app would connect to in EXTERNAL mode, keyed off the
/// app's **configured** desktop socket path rather than the shed-broker default.
///
/// An embedder (the Tauri app, its harness) may point the external client at a
/// non-default socket via its own env override; auto-detect MUST probe the SAME path
/// the client would dial, which [`probe_sockets`] — bound to the shed-broker default —
/// would miss. The status socket is derived as the sibling [`STATUS_SOCKET_FILE`] in
/// that path's directory (the daemon binds both there). Bounded-blocking like
/// [`probe_sockets`]: run it before the runtime spins up, or via `spawn_blocking`.
pub fn probe_sockets_at(desktop_socket: &std::path::Path) -> ModeProbe {
    let status_socket = desktop_socket
        .parent()
        .map(|dir| dir.join(STATUS_SOCKET_FILE))
        .unwrap_or_else(|| std::path::PathBuf::from(STATUS_SOCKET_FILE));
    ModeProbe {
        desktop_socket_live: socket_is_live(desktop_socket),
        status_socket_live: socket_is_live(&status_socket),
    }
}

/// The pure three-way probe (§3.3): desktop live ⇒ External; status-only live ⇒
/// HeadlessCoexist; neither ⇒ Embedded.
///
/// This function itself is pure; but note its input is produced by [`probe_sockets`],
/// which does bounded (500 ms) blocking `connect(2)`s — a caller on an async runtime
/// should obtain the [`ModeProbe`] via [`tokio::task::spawn_blocking`].
pub fn detect_mode(probe: ModeProbe) -> DetectedMode {
    if probe.desktop_socket_live {
        DetectedMode::External
    } else if probe.status_socket_live {
        DetectedMode::HeadlessCoexist
    } else {
        DetectedMode::Embedded
    }
}

/// Apply the pref overlay: `auto` follows the probe; `embedded`/`external` pin the mode
/// explicitly (user intent — an explicitly pinned `embedded` alongside a daemon means
/// the daemon 409s, surfaced not hidden). The probe evidence is retained regardless.
pub fn resolve_mode(pref: ModePref, probe: ModeProbe) -> ResolvedMode {
    let mode = match pref {
        ModePref::External => EffectiveMode::External,
        ModePref::Embedded => EffectiveMode::Embedded,
        ModePref::Auto => match detect_mode(probe) {
            DetectedMode::External => EffectiveMode::External,
            DetectedMode::HeadlessCoexist => EffectiveMode::HeadlessCoexist,
            DetectedMode::Embedded => EffectiveMode::Embedded,
        },
    };
    ResolvedMode { mode, probe }
}

// ---------------------------------------------------------------------------
// Outcome mapping (§3.2) — pinned against the daemon's decision_to_outcome
// ---------------------------------------------------------------------------

/// The app's decision as delivered internally, before the `decided_by` default is
/// applied — the embedded mirror of the daemon's `DesktopDecision`
/// (`crates/shed-host-agent/src/desktop.rs:147-154`).
#[derive(Debug, Clone, PartialEq, Eq)]
struct AppDecision {
    approved: bool,
    decided_by: String,
    scope: Option<String>,
    ttl: Option<String>,
}

/// Map an app decision to the broker's `ApprovalOutcome`, field-for-field with the
/// daemon's `decision_to_outcome` (`crates/shed-host-agent/src/desktop.rs:156-173`): an
/// empty `decided_by` defaults to `"user"`; scope/ttl pass through as raw strings;
/// `reason` is always empty (the ssh/aws deny audits don't read it, and the
/// docker-desktop deny reason is a later slice). Keeping this a free function of a plain
/// `AppDecision` lets the fixture pin it byte-for-byte without a live gate.
fn decision_to_outcome(dec: AppDecision) -> ApprovalOutcome {
    let decided_by = if dec.decided_by.is_empty() {
        "user".to_string()
    } else {
        dec.decided_by
    };
    ApprovalOutcome {
        approved: dec.approved,
        decided_by,
        scope: dec.scope,
        ttl: dec.ttl,
        reason: String::new(),
    }
}

/// The wire string for a `DecidedBy` (serde's lowercase rename), so an embedded audit
/// row's `decided_by` matches the daemon's.
fn decided_by_wire(d: DecidedBy) -> String {
    match d {
        DecidedBy::Policy => "policy",
        DecidedBy::User => "user",
        DecidedBy::Touchid => "touchid",
        DecidedBy::Timeout => "timeout",
    }
    .to_string()
}

// ---------------------------------------------------------------------------
// The bridge core: approval channel + audit fan-in
// ---------------------------------------------------------------------------

/// The shared state the `AppGate` (which registers + awaits) and the Responder consumer
/// (which resolves) both hold.
struct BridgeInner {
    /// The Coordinator-facing event stream — approval requests + fanned-in audit rows.
    event_tx: mpsc::UnboundedSender<HostAgentEvent>,
    clock: ClockRef,
    /// The delegated-approval budget, from `cfg.approval_timeout()` (NOT a constant).
    timeout: Duration,
    /// In-flight approvals keyed by request id, each awaiting the Coordinator's decision.
    /// `remove` is the single-resume guard — whoever removes the sender owns its resume
    /// (mirror the daemon's `Inner.pending`).
    pending: Mutex<HashMap<String, oneshot::Sender<AppDecision>>>,
}

/// The in-process approval gate injected as the ssh/aws/docker `shed-desktop` gate. Each
/// `approve` posts an approval-request `Frame` into the Coordinator's stream and awaits a
/// per-request `oneshot`, enforcing `cfg.approval_timeout()` itself and failing closed
/// (the daemon's no-decision outcome) on expiry. Replaces the daemon's `DesktopGate`.
struct AppGate {
    inner: Arc<BridgeInner>,
}

#[async_trait]
impl ApprovalGate for AppGate {
    async fn approve(
        &self,
        ns: &str,
        op: &str,
        server: &str,
        shed: &str,
        detail: &str,
    ) -> ApprovalOutcome {
        let id = new_id();
        let now = self.inner.clock.now_unix();
        let req = ApprovalRequest {
            id: id.clone(),
            ts: self.inner.clock.now_iso8601(),
            server: server.to_string(),
            namespace: ns.to_string(),
            op: op.to_string(),
            shed: shed.to_string(),
            detail: detail.to_string(),
            // `as_secs()` truncates sub-second timeout to whole seconds (a ~1s
            // expires_at truncation, and a sub-second `approval_timeout` footgun) — a
            // deliberate byte-parity choice with the daemon's `as_secs` truncation
            // (crates/shed-host-agent/src/desktop.rs:302), not an oversight.
            expires_at: timefmt::format_iso8601(now + self.inner.timeout.as_secs() as i64),
        };

        let (tx, rx) = oneshot::channel();
        // Register BEFORE posting so a fast decision can't race ahead of registration.
        self.inner.pending.lock().unwrap().insert(id.clone(), tx);
        if self
            .inner
            .event_tx
            .send(HostAgentEvent::Frame(Box::new(
                HostAgentInbound::ApprovalRequest(req),
            )))
            .is_err()
        {
            // The Coordinator's event receiver is gone — nothing will ever decide. Fail
            // closed (drop our pending so a late resolve is a no-op).
            self.inner.pending.lock().unwrap().remove(&id);
            return ApprovalOutcome::denied_no_decision();
        }

        let outcome = tokio::select! {
            res = rx => match res {
                Ok(dec) => decision_to_outcome(dec),
                // The sender was dropped (the Responder consumer stopped) — fail closed.
                Err(_) => ApprovalOutcome::denied_no_decision(),
            },
            _ = tokio::time::sleep(self.inner.timeout) => ApprovalOutcome::denied_no_decision(),
        };
        // Idempotent cleanup. Removing our pending here IS the corpse-drop the daemon's
        // approval_dismiss achieves: a late Coordinator decision (the user clicking
        // approve after the timeout) finds no pending and is a harmless no-op, so we
        // never act on a dead request. The Coordinator's own expiry tick (keyed on the
        // `expires_at` we set to now+timeout) then clears the stale UI prompt on
        // schedule — full external-mode parity, and it needs no new Coordinator seam.
        self.inner.pending.lock().unwrap().remove(&id);
        outcome
    }

    fn method(&self) -> &str {
        shed_broker::config::POLICY_SHED_DESKTOP
    }
}

/// The decision a resolved Responder call carries to the bridge's async consumer.
struct RespondMsg {
    request_id: String,
    decision: AppDecision,
}

/// The Coordinator's decision sink: a synchronous, non-blocking bounded `try_send` (the
/// contract [`Responder`] demands — called inside the actor's atomic handler). The
/// async consumer completes the matching `oneshot`.
struct BridgeResponder {
    tx: mpsc::Sender<RespondMsg>,
    /// Loud-log sink for the (accepted) drop path — see [`RESPOND_QUEUE_CAP`] and the
    /// disposition note on `respond`.
    log: Arc<dyn bus::BusLog>,
}

impl Responder for BridgeResponder {
    fn respond(
        &self,
        request_id: &str,
        decision: ApprovalDecision,
        decided_by: DecidedBy,
        scope: Option<&str>,
        ttl: Option<&str>,
    ) {
        let msg = RespondMsg {
            request_id: request_id.to_string(),
            decision: AppDecision {
                approved: decision == ApprovalDecision::Approve,
                decided_by: decided_by_wire(decided_by),
                scope: scope.map(str::to_string),
                ttl: ttl.map(str::to_string),
            },
        };
        // A full queue (wedged consumer) or closed queue (consumer gone) → drop the
        // decision; the AppGate then fails CLOSED on its own timeout (the bus denies by
        // timeout — never a bypass; same class as the UDS respond-while-disconnected
        // no-op). We must never block the Coordinator's atomic handler, so the drop is
        // accepted (§RESPOND_QUEUE_CAP) — but it is LOUDLY logged so an audited approve
        // whose decision never reached the gate is diagnosable.
        if let Err(e) = self.tx.try_send(msg) {
            let cause = match e {
                mpsc::error::TrySendError::Full(_) => "queue full",
                mpsc::error::TrySendError::Closed(_) => "consumer gone",
            };
            self.log.warn(&format!(
                "embedded broker: approval decision for request {request_id} DROPPED ({cause}); failing closed via gate timeout"
            ));
        }
    }
}

/// Drains resolved decisions and completes the matching in-flight `oneshot`. Runs until
/// every Responder is dropped (i.e. the Coordinator is gone).
async fn run_respond_consumer(inner: Arc<BridgeInner>, mut rx: mpsc::Receiver<RespondMsg>) {
    while let Some(msg) = rx.recv().await {
        let sender = inner.pending.lock().unwrap().remove(&msg.request_id);
        if let Some(tx) = sender {
            let _ = tx.send(msg.decision); // oneshot: consumed on send; a dropped rx is fine
        }
    }
}

/// Fans each broker audit row into the Coordinator's activity stream as an `Event`
/// frame — the embedded analogue of the daemon's desktop `event`-frame forwarder, wired
/// through the C1 [`AuditFanout`] seam (the single mechanism; no second path).
struct ActivityFanout {
    event_tx: mpsc::UnboundedSender<HostAgentEvent>,
}

impl AuditFanout for ActivityFanout {
    fn forward(&self, entry: &BrokerAuditEntry) {
        let frame = audit_entry_to_event_frame(entry);
        let _ = self
            .event_tx
            .send(HostAgentEvent::Frame(Box::new(HostAgentInbound::Event(
                frame,
            ))));
    }
}

/// Map a broker [`BrokerAuditEntry`] to the app's `AuditEventFrame` (the wire an
/// external-mode agent would stream). The broker entry carries no request id, so the
/// frame omits it and the Coordinator stamps its own (matching `from_event_frame`'s
/// fallback). Empty strings map to `None` to reproduce Go's omitempty.
fn audit_entry_to_event_frame(e: &BrokerAuditEntry) -> AuditEventFrame {
    let opt = |s: &str| (!s.is_empty()).then(|| s.to_string());
    AuditEventFrame {
        kind: None,
        server: opt(&e.server),
        // The app records `shed` unconditionally (its `from_event_frame` keeps the
        // Option as-is); the durable JSONL always carries `shed`, so keep it present.
        shed: Some(e.shed.clone()),
        ns: opt(&e.ns),
        op: opt(&e.op),
        result: e.result.clone(),
        detail: opt(&e.detail),
        code: opt(&e.code),
        reason: opt(&e.reason),
        approval: opt(&e.approval),
        request_id: None,
        ts: opt(&e.ts),
    }
}

// ---------------------------------------------------------------------------
// Credential minter adapter (§3.2; mtls per plan 002 §7 P2)
// ---------------------------------------------------------------------------

/// Adapts the broker's control-credential seams onto shed-core's [`TokenMinter`], so
/// the Backend's secure-server vend calls the in-process provider directly instead of
/// going out over the UDS. Fail-closed: a mint error surfaces as `Err`, so the FSM sends
/// NO credential (parity with [`crate::HostAgentTokenMinter`]). Bus-independent +
/// synchronously constructible, so it exists BEFORE the Backend (§3.2).
///
/// Both halves of the contract are implemented (plan 002 §7 P2):
///   * [`ControlCredentialMinter::mint_control_credential`] — the CSR-bearing mint, the
///     one production actually uses (see [`Self::supports_mtls`]);
///   * [`ControlTokenMinter::mint_control`] — the legacy token-only mint, kept because
///     the trait requires it and because it is the seam the token-shaped unit tests
///     drive directly.
///
/// Internal: it only ever escapes as the `Arc<dyn TokenMinter>` from
/// [`EmbeddedHostAgent::token_minter`], so the concrete type stays crate-private.
struct EmbeddedTokenMinter {
    tokens: Arc<dyn ControlTokenMinter>,
    credentials: Arc<dyn ControlCredentialMinter>,
}

impl EmbeddedTokenMinter {
    /// Both halves come from the SAME `ControlTokenProvider` in production — two
    /// `dyn` views of one object — but they are taken separately so a test can drive
    /// either path against a stand-in.
    fn new(
        tokens: Arc<dyn ControlTokenMinter>,
        credentials: Arc<dyn ControlCredentialMinter>,
    ) -> Self {
        Self {
            tokens,
            credentials,
        }
    }
}

#[async_trait]
impl TokenMinter for EmbeddedTokenMinter {
    /// Capability, and here it is a STATIC one — the deliberate difference from
    /// [`crate::HostAgentTokenMinter`], which has to ask the socket what the agent on
    /// the other end can do.
    ///
    /// There is no other end: the broker is this build, linked into this process, and
    /// the credential path is compiled in right here. Version skew — the thing the UDS
    /// capability handshake exists to absorb — cannot occur, so advertising anything
    /// but `true` would make the provider withhold a CSR from a minter that can always
    /// relay one, and an mtls server would answer the resulting CSR-less bootstrap with
    /// its "upgrade the app" refusal.
    fn supports_mtls(&self) -> bool {
        true
    }

    /// Relay the PROVIDER's CSR into the broker's control-credential mint and hand back
    /// whatever the server issued.
    ///
    /// The key containment rule (plan 001 D6 / 002 §7 P3) is the whole reason this is a
    /// relay and not a mint: `shed_core`'s `ControlTokenProvider` generated the keypair,
    /// kept the private half, and derived `req`'s CSR from it. Nothing here generates a
    /// second keypair — a certificate issued for a key THIS layer held would be one the
    /// provider could never present, which is precisely the failure the CSR-relay shape
    /// exists to prevent. The broker's own `mint_control_credential` carries the same
    /// rule on its side; this method's only job is to pass the CSR through verbatim.
    ///
    /// A `req` with no CSR (which the provider only produces for a minter that did not
    /// advertise mtls, so not this one) degrades to the empty string — the legacy
    /// token-only bootstrap the broker already defines for that argument.
    async fn mint_credential(
        &self,
        server: &str,
        req: &CredentialRequest,
    ) -> Result<MintedCredential, ShedError> {
        let minted = self
            .credentials
            .mint_control_credential(server, req.csr_base64().unwrap_or_default())
            .await
            .map_err(ShedError::Config)?;
        map_embedded_credential(minted, server)
    }

    async fn mint(&self, server: &str) -> Result<MintedToken, ShedError> {
        let minted = self
            .tokens
            .mint_control(server)
            .await
            .map_err(ShedError::Config)?;
        // Same guards as the UDS minter's `map_response`, because they are the same
        // functions: fail-closed on an empty token (an `Ok(MintedControlToken { token:
        // "" })` must never become a valid-looking blank token), and an
        // absent/unparseable expiry → None → the provider caches until invalidate().
        minted_token(
            minted.token,
            expiry_unix(minted.expires_at.as_deref()),
            server,
        )
    }
}

/// Map a broker [`MintedControlCredential`] onto shed-core's [`MintedCredential`], or a
/// fail-closed `Err`. Pure, so every shape is unit-tested without a live SSH mint.
///
/// The in-process sibling of the UDS reply mapper (`token_minter.rs`), and identical in
/// its rules BY CONSTRUCTION: this only normalizes the broker's struct into
/// [`CredentialParts`] and hands it to the shared [`credential_from_parts`], which is
/// where the mode/fail-closed rules live. Nothing here may add a rule of its own — the
/// two paths feed the SAME provider FSM, so a divergence would mean the app behaved
/// differently in embedded mode than in external mode against the same server.
fn map_embedded_credential(
    minted: MintedControlCredential,
    server: &str,
) -> Result<MintedCredential, ShedError> {
    credential_from_parts(
        CredentialParts {
            auth_mode: Some(minted.auth_mode),
            token: minted.token,
            client_cert: minted.client_cert,
            cert_serial: minted.cert_serial,
            expires_at_unix: expiry_unix(minted.expires_at.as_deref()),
        },
        server,
    )
}

// ---------------------------------------------------------------------------
// EmbeddedHostAgent — the assembled bridge
// ---------------------------------------------------------------------------

/// The in-process broker bridge. Built in two phases to honor the minter-before-Backend
/// launch order (§3.2): [`new`](Self::new) (sync) yields the token minter + Responder +
/// event receiver immediately; [`spawn`](Self::spawn) (async, after the Backend +
/// Coordinator exist) starts the bus and emits the synthesized `Connected` event.
pub struct EmbeddedHostAgent {
    inner: Arc<BridgeInner>,
    responder: ResponderRef,
    token_minter: Arc<dyn TokenMinter>,
    /// Shared by the supervisor's per-server bus (each secure server self-mints over
    /// SSH) — the SAME `Arc` the control-token provider wraps.
    minter: Arc<dyn broker_minter::Minter>,
    cfg: HostAgentConfig,
    shutdown_tx: watch::Sender<bool>,
    shutdown_rx: watch::Receiver<bool>,
    gate_namespaces: Vec<String>,
    /// Once-only guard for [`spawn`]: set on the first call so a second call returns
    /// [`BrokerError::AlreadySpawned`] WITHOUT building a second `Supervisor` + watch loop.
    spawned: AtomicBool,
    /// The running supervisor (`None` until [`spawn`] succeeds) — `health()` reads it.
    supervisor: Mutex<Option<Arc<Supervisor>>>,
    /// The SSH mode the backend resolved to at spawn (env-dependent for `ssh.mode: ""`);
    /// surfaced in `broker.status`.
    resolved_ssh_mode: Mutex<Option<String>>,
}

impl EmbeddedHostAgent {
    /// Phase 1 (sync, must run inside a tokio runtime): build the approval/event seams +
    /// the control-token minter. The token minter + Responder are usable immediately;
    /// the bus is not started until [`spawn`](Self::spawn). Returns the agent plus the
    /// event receiver to hand to [`Coordinator::spawn`](crate::coordinator::Coordinator::spawn).
    pub fn new(cfg: HostAgentConfig) -> (Self, mpsc::UnboundedReceiver<HostAgentEvent>) {
        let (event_tx, event_rx) = mpsc::unbounded_channel();
        let (respond_tx, respond_rx) = mpsc::channel(RESPOND_QUEUE_CAP);
        let clock = system_clock();

        let inner = Arc::new(BridgeInner {
            event_tx,
            clock,
            timeout: cfg.approval_timeout(),
            pending: Mutex::new(HashMap::new()),
        });

        // The bounded-mpsc → oneshot handoff consumer (§3.2). Runs until every Responder
        // is dropped.
        tokio::spawn(run_respond_consumer(inner.clone(), respond_rx));

        // The credential minter, built ONCE and shared by the control-token provider
        // (token.get) AND the supervisor (secure-server bus self-mint) — parity with the
        // daemon's single `minter` (`main.rs`).
        let minter: Arc<dyn broker_minter::Minter> =
            Arc::new(broker_minter::CredentialMinter::new(KNOWN_HOSTS_PATH));
        // The control mints resolve servers from the shed CLI config (the discovery-source
        // override is a per-server concern the supervisor owns), matching the daemon. ONE
        // provider backs both seams — it implements both traits, and the adapter takes two
        // `dyn` views of it rather than two objects.
        let control_provider = Arc::new(ControlTokenProvider::new(
            minter.clone(),
            DEFAULT_DISCOVERY_SOURCE,
        ));
        let token_minter: Arc<dyn TokenMinter> = Arc::new(EmbeddedTokenMinter::new(
            control_provider.clone() as Arc<dyn ControlTokenMinter>,
            control_provider as Arc<dyn ControlCredentialMinter>,
        ));

        let (shutdown_tx, shutdown_rx) = watch::channel(false);
        let gate_namespaces = cfg.gate_namespaces();

        // The Responder's loud-log sink for the (accepted) decision-drop path — stderr,
        // the same `bus::FileBusLog` idiom `spawn` uses for its broker-startup warnings.
        let responder_log: Arc<dyn bus::BusLog> = Arc::new(bus::FileBusLog::new(""));

        let agent = EmbeddedHostAgent {
            inner,
            responder: Arc::new(BridgeResponder {
                tx: respond_tx,
                log: responder_log,
            }),
            token_minter,
            minter,
            cfg,
            shutdown_tx,
            shutdown_rx,
            gate_namespaces,
            spawned: AtomicBool::new(false),
            supervisor: Mutex::new(None),
            resolved_ssh_mode: Mutex::new(None),
        };
        (agent, event_rx)
    }

    /// The control-token minter for the Backend (clone before [`spawn`]).
    pub fn token_minter(&self) -> Arc<dyn TokenMinter> {
        self.token_minter.clone()
    }

    /// The Coordinator's Responder.
    pub fn responder(&self) -> ResponderRef {
        self.responder.clone()
    }

    /// The shutdown handle for `RunEvent::ExitRequested` — flip to drain the supervisor
    /// groups best-effort (§3.6).
    pub fn shutdown_handle(&self) -> watch::Sender<bool> {
        self.shutdown_tx.clone()
    }

    /// The delegated-gate namespaces the synthesized `Connected` advertises.
    pub fn gate_namespaces(&self) -> &[String] {
        &self.gate_namespaces
    }

    /// The running supervisor's per-server health for `broker.status` (empty until
    /// [`spawn`] succeeds).
    pub fn health(&self) -> Vec<ServerHealth> {
        self.supervisor
            .lock()
            .unwrap()
            .as_ref()
            .map(|s| s.health())
            .unwrap_or_default()
    }

    /// The SSH backend mode resolved at spawn (`None` until [`spawn`] succeeds).
    pub fn resolved_ssh_mode(&self) -> Option<String> {
        self.resolved_ssh_mode.lock().unwrap().clone()
    }

    /// Phase 2 (async, in a tokio runtime): resolve the SSH backend, build the
    /// `SharedDeps` supervisor, spawn the watch/reconcile loop on the current runtime,
    /// and emit the synthesized `Connected(HelloAck)`. On SSH-resolve failure returns
    /// `Err(BrokerError::SshBackend)` (broker NOT started, but token minting still works
    /// — the Backend already holds the minter).
    ///
    /// **Once-only:** a second call returns `Err(BrokerError::AlreadySpawned)` WITHOUT
    /// side effects (no second `Supervisor`/watch loop, no overwrite of `self.supervisor`,
    /// no second `Connected`). The guard trips on the FIRST call regardless of outcome —
    /// there is no retry path (the app fails the broker closed and keeps running), so
    /// "attempted once" is the invariant we protect.
    pub async fn spawn(&self) -> Result<(), BrokerError> {
        // Re-entry guard: `swap` publishes "attempted" and reads the prior state in one
        // atomic step. A prior `true` means someone already spawned — bail before any
        // side effect (no Supervisor, no tokio::spawn, no Connected).
        if self.spawned.swap(true, Ordering::SeqCst) {
            return Err(BrokerError::AlreadySpawned);
        }

        let cfg = &self.cfg;
        let bus_log: Arc<dyn bus::BusLog> = Arc::new(bus::FileBusLog::new(""));

        // Resolve the SSH backend UNCONDITIONALLY (parity with the daemon's startup) —
        // a resolve error fails the broker closed WITHOUT taking the app down. Run it on
        // a blocking thread: the `ssh.mode: ""` auto path does an UN-timeout'd blocking
        // `UnixStream::connect` on `$SSH_AUTH_SOCK` (ssh_backend.rs:773) and local-keys
        // does sync `~/.ssh` reads — either would stall an async runtime worker. The
        // error mapping is unchanged (`BrokerError::SshBackend`); a join failure (the
        // resolve thread panicked) is likewise a fail-closed `SshBackend`.
        let ssh_mode = cfg.ssh_mode().to_string();
        let (ssh_backend, ssh_warnings) = tokio::task::spawn_blocking(move || {
            ssh_backend::resolve_ssh_backend_from_env(&ssh_mode)
        })
        .await
        .map_err(|e| BrokerError::SshBackend(format!("ssh backend resolve task failed: {e}")))?
        .map_err(BrokerError::SshBackend)?;
        for w in &ssh_warnings {
            bus_log.warn(w);
        }
        *self.resolved_ssh_mode.lock().unwrap() = Some(ssh_backend.mode().to_string());

        // Per-namespace gate: the built-in routing (approve-all / biometrics / unknown →
        // deny) is shared with the daemon; the `shed-desktop` arm (`None`) supplies THIS
        // bridge's in-process `AppGate` instead of the daemon's `DesktopGate`-over-UDS.
        let app_gate: Arc<dyn ApprovalGate> = Arc::new(AppGate {
            inner: self.inner.clone(),
        });
        let select_gate = |policy: &str| -> Arc<dyn ApprovalGate> {
            select_builtin_gate(policy, cfg).unwrap_or_else(|| app_gate.clone())
        };
        let ssh_gate = select_gate(&cfg.effective_policy(NS_SSH_AGENT));

        // The audit sink fans every entry into the Coordinator's activity stream (the C1
        // seam) AND writes the broker's configured durable JSONL (daemon parity); the app
        // keeps its own `audit.jsonl` separately (§3.2, both durable logs written).
        let fanout: Arc<dyn AuditFanout> = Arc::new(ActivityFanout {
            event_tx: self.inner.event_tx.clone(),
        });
        let audit: Arc<dyn AuditSink> = Arc::new(JsonlAuditSink::new(cfg, Some(fanout)));

        // AWS: unconfigured → None → the aws-credentials namespace is never subscribed.
        let aws = match aws_backend::new_sts_backend(cfg.aws.clone(), bus_log.clone()) {
            Ok(sts) => {
                let backend: Arc<dyn aws_backend::AwsBackend> = Arc::new(sts);
                Some(bus::AwsHandlers {
                    backend,
                    gate: select_gate(&cfg.effective_policy(NS_AWS_CREDENTIALS)),
                })
            }
            Err(e) => {
                bus_log.warn(&format!("AWS handler disabled error={e}"));
                None
            }
        };
        // Docker: an absent/empty block still constructs a live (deny-everything) backend,
        // so docker-credentials IS subscribed (parity with the daemon's asymmetry).
        let docker = match docker_backend::new_docker_backend(cfg.docker.clone(), bus_log.clone()) {
            Ok(backend) => {
                let backend: Arc<dyn docker_backend::DockerBackend> = Arc::new(backend);
                Some(bus::DockerHandlers {
                    backend,
                    gate: select_gate(&cfg.effective_policy(NS_DOCKER_CREDENTIALS)),
                })
            }
            Err(e) => {
                bus_log.warn(&format!("Docker handler disabled error={e}"));
                None
            }
        };

        let deps = SharedDeps {
            ssh_backend,
            ssh_gate,
            aws,
            docker,
            audit,
            minter: Some(self.minter.clone()),
            log: bus_log.clone(),
        };
        let sup = Arc::new(Supervisor::new(self.shutdown_rx.clone(), deps));
        *self.supervisor.lock().unwrap() = Some(sup.clone());

        // Single-server = discovery-off (reconcile once, never reload); else the parsed
        // discovery config (parity with the daemon's watch-mode computation).
        let watch_cfg = cfg.discovery.clone().unwrap_or_else(|| DiscoveryConfig {
            watch: "off".to_string(),
            ..Default::default()
        });

        // The watch/reconcile loop on the app's runtime: reconcile per the watch mode
        // until shutdown flips, then drain the groups (best-effort). A discovery READ
        // error keeps the current servers (a transient read can't tear all groups down),
        // matching the daemon.
        let cfg_arc = Arc::new(cfg.clone());
        let shutdown = self.shutdown_rx.clone();
        let reconcile_log = bus_log.clone();
        let loop_log = bus_log.clone();
        tokio::spawn(async move {
            let reconcile = {
                let sup = sup.clone();
                move || {
                    let sup = sup.clone();
                    let cfg = cfg_arc.clone();
                    let log = reconcile_log.clone();
                    async move {
                        match cfg.resolve_targets() {
                            Ok(desired) => sup.reconcile(desired).await,
                            Err(e) => log.warn(&format!(
                                "discovery read failed; keeping current servers error={e}"
                            )),
                        }
                    }
                }
            };
            watcher::run_watch_loop(watch_cfg, reconcile, shutdown, loop_log).await;
            sup.shutdown().await;
        });

        // Synthesize the Connected(HelloAck) so the approvals UI sections light up exactly
        // as in external mode (no replay buffer — post-restart activity comes from the
        // app's AuditStore).
        let ack = HelloAck {
            namespaces: vec![
                NS_SSH_AGENT.to_string(),
                NS_AWS_CREDENTIALS.to_string(),
                NS_DOCKER_CREDENTIALS.to_string(),
                NS_EGRESS.to_string(),
            ],
            gate_namespaces: cfg.gate_namespaces(),
            // The EMBEDDED broker is the same build as the app, so there is no
            // version skew to describe here — but the synthesized ack must still
            // advertise what the app can rely on, or capability-gated paths would
            // work in external mode and silently refuse in embedded mode.
            agent_capabilities: vec![CAP_CREDENTIAL_GET.to_string()],
            request_timeout_ms: cfg.approval_timeout().as_millis() as i64,
            accepted: true,
        };
        let _ = self.inner.event_tx.send(HostAgentEvent::Connected(ack));
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- (iii) outcome-mapping fixture: field-for-field vs the daemon's
    // decision_to_outcome (crates/shed-host-agent/src/desktop.rs:156-173) --------------

    #[test]
    fn decision_to_outcome_matches_daemon_fixture() {
        struct Case {
            name: &'static str,
            input: AppDecision,
            want: ApprovalOutcome,
        }
        let cases = vec![
            Case {
                name: "approve with touchid + scope/ttl passes through",
                input: AppDecision {
                    approved: true,
                    decided_by: "touchid".into(),
                    scope: Some("per-session".into()),
                    ttl: Some("1h".into()),
                },
                want: ApprovalOutcome {
                    approved: true,
                    decided_by: "touchid".into(),
                    scope: Some("per-session".into()),
                    ttl: Some("1h".into()),
                    reason: String::new(),
                },
            },
            Case {
                // The load-bearing empty→user default (desktop.rs:157-161).
                name: "empty decided_by defaults to user",
                input: AppDecision {
                    approved: true,
                    decided_by: String::new(),
                    scope: None,
                    ttl: None,
                },
                want: ApprovalOutcome {
                    approved: true,
                    decided_by: "user".into(),
                    scope: None,
                    ttl: None,
                    reason: String::new(),
                },
            },
            Case {
                name: "deny with empty decided_by defaults to user, reason stays empty",
                input: AppDecision {
                    approved: false,
                    decided_by: String::new(),
                    scope: None,
                    ttl: None,
                },
                want: ApprovalOutcome {
                    approved: false,
                    decided_by: "user".into(),
                    scope: None,
                    ttl: None,
                    reason: String::new(),
                },
            },
            Case {
                name: "explicit user + scope/ttl on a deny passes through",
                input: AppDecision {
                    approved: false,
                    decided_by: "user".into(),
                    scope: Some("per-request".into()),
                    ttl: Some("30m".into()),
                },
                want: ApprovalOutcome {
                    approved: false,
                    decided_by: "user".into(),
                    scope: Some("per-request".into()),
                    ttl: Some("30m".into()),
                    reason: String::new(),
                },
            },
        ];
        for c in cases {
            assert_eq!(decision_to_outcome(c.input.clone()), c.want, "{}", c.name);
        }
    }

    #[test]
    fn decided_by_wire_covers_all_variants() {
        assert_eq!(decided_by_wire(DecidedBy::Policy), "policy");
        assert_eq!(decided_by_wire(DecidedBy::User), "user");
        assert_eq!(decided_by_wire(DecidedBy::Touchid), "touchid");
        assert_eq!(decided_by_wire(DecidedBy::Timeout), "timeout");
    }

    // ---- the approval round-trip (AppGate ↔ Responder ↔ oneshot) ---------------------

    /// Build just the approval-channel seams (no bus) for the round-trip + timeout tests.
    fn channel_only(
        timeout: Duration,
    ) -> (
        Arc<BridgeInner>,
        ResponderRef,
        mpsc::UnboundedReceiver<HostAgentEvent>,
    ) {
        let (event_tx, event_rx) = mpsc::unbounded_channel();
        let (respond_tx, respond_rx) = mpsc::channel(RESPOND_QUEUE_CAP);
        let inner = Arc::new(BridgeInner {
            event_tx,
            clock: system_clock(),
            timeout,
            pending: Mutex::new(HashMap::new()),
        });
        tokio::spawn(run_respond_consumer(inner.clone(), respond_rx));
        let responder: ResponderRef = Arc::new(BridgeResponder {
            tx: respond_tx,
            log: Arc::new(bus::FileBusLog::new("")),
        });
        (inner, responder, event_rx)
    }

    #[tokio::test]
    async fn approve_round_trip_carries_decision_fields() {
        let (inner, responder, mut events) = channel_only(Duration::from_secs(10));
        let gate = AppGate { inner };

        // The gate fires; a fake Coordinator side reads the request and responds approve.
        let gate_fut = tokio::spawn(async move {
            gate.approve("ssh-agent", "sign", "mini2", "web", "SSH sign request")
                .await
        });

        let ev = tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .unwrap()
            .unwrap();
        let req = match ev {
            HostAgentEvent::Frame(f) => match *f {
                HostAgentInbound::ApprovalRequest(r) => r,
                other => panic!("expected ApprovalRequest, got {other:?}"),
            },
            other => panic!("expected Frame, got {other:?}"),
        };
        assert_eq!(req.namespace, "ssh-agent");
        assert_eq!(req.op, "sign");
        assert_eq!(req.server, "mini2");
        assert_eq!(req.shed, "web");
        assert_eq!(req.detail, "SSH sign request");
        assert!(!req.id.is_empty());
        assert!(!req.expires_at.is_empty());

        responder.respond(
            &req.id,
            ApprovalDecision::Approve,
            DecidedBy::User,
            Some("per-session"),
            Some("1h"),
        );
        let outcome = tokio::time::timeout(Duration::from_secs(5), gate_fut)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(
            outcome,
            ApprovalOutcome {
                approved: true,
                decided_by: "user".into(),
                scope: Some("per-session".into()),
                ttl: Some("1h".into()),
                reason: String::new(),
            }
        );
    }

    // ---- (ii) timeout → no-decision deny; a late decision is a harmless no-op --------

    #[tokio::test]
    async fn timeout_fails_closed_and_drops_the_corpse() {
        let (inner, responder, mut events) = channel_only(Duration::from_millis(120));
        let gate = AppGate {
            inner: inner.clone(),
        };
        let gate_fut = tokio::spawn(async move {
            gate.approve("ssh-agent", "sign", "", "web", "SSH sign request")
                .await
        });

        let ev = tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .unwrap()
            .unwrap();
        let req = match ev {
            HostAgentEvent::Frame(f) => match *f {
                HostAgentInbound::ApprovalRequest(r) => r,
                other => panic!("unexpected {other:?}"),
            },
            other => panic!("unexpected {other:?}"),
        };

        // No decision within the (120ms) budget → fail-closed no-decision deny.
        let outcome = tokio::time::timeout(Duration::from_secs(5), gate_fut)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(outcome, ApprovalOutcome::denied_no_decision());
        assert!(!outcome.approved);
        assert_eq!(outcome.decided_by, "");

        // The pending was dropped on timeout, so a late user decision is a no-op (no
        // pending oneshot to resolve, no panic) — the corpse-safety the dismiss provides.
        assert!(inner.pending.lock().unwrap().is_empty());
        responder.respond(
            &req.id,
            ApprovalDecision::Approve,
            DecidedBy::User,
            None,
            None,
        );
        // Give the consumer a tick; nothing should blow up and pending stays empty.
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert!(inner.pending.lock().unwrap().is_empty());
    }

    // ---- audit fan-in → Event frame shape --------------------------------------------

    #[test]
    fn audit_entry_maps_to_event_frame_with_omitempty() {
        let entry = BrokerAuditEntry {
            ts: "2026-07-15T00:00:00Z".into(),
            server: String::new(), // omitted → None
            shed: "web".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            detail: "ssh-ed25519".into(),
            code: String::new(),
            reason: String::new(),
            approval: "shed-desktop".into(),
            decided_by: "user".into(),
            scope: String::new(),
            ttl: String::new(),
        };
        let f = audit_entry_to_event_frame(&entry);
        assert_eq!(f.server, None);
        assert_eq!(f.shed.as_deref(), Some("web"));
        assert_eq!(f.ns.as_deref(), Some("ssh-agent"));
        assert_eq!(f.op.as_deref(), Some("sign"));
        assert_eq!(f.result, "ok");
        assert_eq!(f.detail.as_deref(), Some("ssh-ed25519"));
        assert_eq!(f.code, None);
        assert_eq!(f.reason, None);
        assert_eq!(f.approval.as_deref(), Some("shed-desktop"));
        assert_eq!(f.request_id, None);
        assert_eq!(f.ts.as_deref(), Some("2026-07-15T00:00:00Z"));

        // The app maps it into a stored entry (host-agent source), stamping its own id.
        let stored =
            shed_core::approval::AuditEntry::from_event_frame(f, "gen-id".into(), "gen-ts".into());
        assert_eq!(stored.id, "gen-id"); // no request_id → fallback
        assert_eq!(stored.ts, "2026-07-15T00:00:00Z");
        assert_eq!(stored.approval.as_deref(), Some("shed-desktop"));
    }

    // ---- (i) end-to-end against an httpmock synthetic bus ----------------------------
    // Drive the AppGate as the real ssh `shed-desktop` gate inside `spawn_server_group`:
    // subscribe → inject a `sign` request over SSE → the AppGate posts an approval-request
    // to the event stream → a fake Coordinator side responds → the bus `respond` POST +
    // the audit outcome are asserted. A DENY keeps the flow crypto-free (handle_sign
    // parses the public key only AFTER an approve); the approve outcome shape is pinned by
    // the round-trip + fixture tests above.

    use httpmock::prelude::*;
    use shed_broker::discovery::ServerTarget;
    use shed_broker::ssh_backend::{SshBackend, SshKeyInfo, SshSignature};

    /// Records every audit entry for asserting the sign outcome shape.
    #[derive(Default)]
    struct CollectingAudit {
        entries: Mutex<Vec<BrokerAuditEntry>>,
    }
    impl AuditSink for CollectingAudit {
        fn log(&self, entry: BrokerAuditEntry) {
            self.entries.lock().unwrap().push(entry);
        }
    }

    /// A no-op SSH backend — the deny flow never reaches signing.
    struct NoSignBackend;
    impl SshBackend for NoSignBackend {
        fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
            Ok(Vec::new())
        }
        fn sign(&self, _pk: &[u8], _data: &[u8], _flags: u32) -> Result<SshSignature, String> {
            Err("unused".into())
        }
        fn mode(&self) -> &str {
            "local-keys"
        }
    }

    #[tokio::test]
    async fn bus_sign_request_routes_through_appgate_and_responds() {
        let server = MockServer::start_async().await;
        // One `sign` request over the ssh-agent SSE stream (arbitrary base64 — the deny
        // path never decodes the key).
        let sse = "data: {\"id\":\"sign-1\",\"namespace\":\"ssh-agent\",\"type\":\"request\",\"final\":true,\"timestamp\":\"t\",\"payload\":{\"operation\":\"sign\",\"public_key\":\"AAAA\",\"data\":\"YWJj\",\"flags\":0},\"shed\":{\"name\":\"web\",\"backend\":\"vz\",\"server\":\"mini2\"}}\n\n";
        let messages = server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/plugins/listeners/ssh-agent/messages")
                    .header("accept", "text/event-stream");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(sse);
            })
            .await;
        // The respond POST must carry the request envelope id as `in_reply_to`.
        let respond = server
            .mock_async(|w, t| {
                w.method(POST)
                    .path("/api/plugins/listeners/ssh-agent/respond")
                    .body_contains("\"in_reply_to\":\"sign-1\"");
                t.status(204);
            })
            .await;

        // The bridge approval channel (event stream + Responder + consumer).
        let (inner, responder, mut events) = channel_only(Duration::from_secs(10));

        // The fake Coordinator side: DENY every approval request it sees (loops so a
        // reconnect re-firing the sign is answered too).
        let deny_responder = responder.clone();
        tokio::spawn(async move {
            while let Some(ev) = events.recv().await {
                if let HostAgentEvent::Frame(f) = ev {
                    if let HostAgentInbound::ApprovalRequest(req) = *f {
                        deny_responder.respond(
                            &req.id,
                            ApprovalDecision::Deny,
                            DecidedBy::User,
                            Some("per-request"),
                            None,
                        );
                    }
                }
            }
        });

        let audit = Arc::new(CollectingAudit::default());
        let deps = SharedDeps {
            ssh_backend: Arc::new(NoSignBackend),
            ssh_gate: Arc::new(AppGate {
                inner: inner.clone(),
            }),
            aws: None,
            docker: None,
            audit: audit.clone(),
            minter: None,
            log: Arc::new(bus::FileBusLog::new("")),
        };
        let target = ServerTarget {
            name: String::new(), // single-server unnamed target (open http)
            url: server.base_url(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: String::new(),
            ssh_port: 0,
            // No recorded credential shape: an open http target never enrolled.
            auth_mode: String::new(),
        };
        let (_shutdown_tx, shutdown_rx) = watch::channel(false);
        let group = bus::spawn_server_group(shutdown_rx, &target, &deps);

        // Wait for the respond POST + a recorded deny audit (bounded).
        let mut ok = false;
        for _ in 0..100 {
            if respond.hits_async().await >= 1
                && audit
                    .entries
                    .lock()
                    .unwrap()
                    .iter()
                    .any(|e| e.op == "sign" && e.result == "denied")
            {
                ok = true;
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        // Tear the group down before asserting (frees the SSE connection).
        let _ = group.cancel.send(true);
        let _ = group.done.await;

        assert!(ok, "expected a respond POST + a denied sign audit");
        messages.assert_hits_async(1).await; // at least connected once
        let entries = audit.entries.lock().unwrap();
        let denied = entries
            .iter()
            .find(|e| e.op == "sign" && e.result == "denied")
            .expect("denied sign audit");
        // The outcome the AppGate produced flows into the audit row (the daemon shape).
        assert_eq!(denied.shed, "web");
        assert_eq!(denied.approval, "shed-desktop");
        assert_eq!(denied.decided_by, "user");
        assert_eq!(denied.scope, "per-request");
    }

    // ---- spawn() wiring smoke (hermetic: watch off, empty temp discovery source) -----

    #[tokio::test]
    async fn spawn_wires_supervisor_and_emits_connected() {
        // A discovery config over an EMPTY temp shed CLI config (no live servers), watch
        // off (no fsnotify thread). Exercises the real spawn path — SSH auto-detect,
        // SharedDeps/Supervisor build, watch/reconcile loop, and the synthesized
        // Connected — without any network.
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("config.yaml");
        std::fs::write(&src, "servers: {}\n").unwrap();
        let yaml = format!(
            "ssh:\n  approval:\n    policy: shed-desktop\ndiscovery:\n  source: {}\n  watch: off\n",
            src.display()
        );
        let cfg = HostAgentConfig::try_parse(&yaml).unwrap();

        let (agent, mut events) = EmbeddedHostAgent::new(cfg);
        agent
            .spawn()
            .await
            .expect("spawn ok (ssh auto-detect never fails; empty discovery)");

        let ev = tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .unwrap()
            .unwrap();
        match ev {
            HostAgentEvent::Connected(ack) => {
                assert!(ack.accepted);
                assert_eq!(ack.gate_namespaces, vec!["ssh-agent".to_string()]);
                assert_eq!(ack.request_timeout_ms, 25_000); // default approval_timeout
            }
            other => panic!("expected Connected, got {other:?}"),
        }
        // Populated post-spawn (surfaced in broker.status).
        assert!(agent.resolved_ssh_mode().is_some());
        // Best-effort drain (the watch loop returns on the flip, then drains groups).
        let _ = agent.shutdown_handle().send(true);
    }

    // ---- (FIX 2) spawn() is once-only: a second call errors with no side effects -----

    #[tokio::test]
    async fn spawn_is_once_only_and_builds_no_second_task() {
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("config.yaml");
        std::fs::write(&src, "servers: {}\n").unwrap();
        let yaml = format!(
            "ssh:\n  approval:\n    policy: shed-desktop\ndiscovery:\n  source: {}\n  watch: off\n",
            src.display()
        );
        let cfg = HostAgentConfig::try_parse(&yaml).unwrap();

        let (agent, mut events) = EmbeddedHostAgent::new(cfg);
        // First spawn succeeds and emits exactly one Connected.
        agent.spawn().await.expect("first spawn ok");
        let ev = tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .unwrap()
            .unwrap();
        assert!(
            matches!(ev, HostAgentEvent::Connected(_)),
            "first spawn Connected"
        );
        let sup_after_first = agent.supervisor.lock().unwrap().clone();
        assert!(sup_after_first.is_some(), "first spawn built a supervisor");

        // Second spawn returns AlreadySpawned WITHOUT side effects.
        let err = agent.spawn().await.unwrap_err();
        assert_eq!(err, BrokerError::AlreadySpawned);

        // No second Supervisor was built (same Arc, no overwrite) and no second Connected
        // (or any other event) was emitted by the guarded call.
        let sup_after_second = agent.supervisor.lock().unwrap().clone();
        assert!(
            Arc::ptr_eq(
                sup_after_first.as_ref().unwrap(),
                sup_after_second.as_ref().unwrap()
            ),
            "supervisor must not be overwritten by the second spawn"
        );
        // Give any (erroneously) spawned task a tick, then assert the event stream is idle.
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert!(
            matches!(events.try_recv(), Err(mpsc::error::TryRecvError::Empty)),
            "second spawn must emit no event"
        );

        let _ = agent.shutdown_handle().send(true);
    }

    // ---- token minter adapter --------------------------------------------------------

    /// A credential seam that must never be reached — paired with the token-only stubs
    /// below, which drive `mint()` directly.
    struct NoCredentials;
    #[async_trait]
    impl ControlCredentialMinter for NoCredentials {
        async fn mint_control_credential(
            &self,
            _server: &str,
            _csr_base64: &str,
        ) -> Result<MintedControlCredential, String> {
            unreachable!("this test drives the token seam")
        }
    }

    /// Pairs a token-only stub with [`NoCredentials`] (the shape every pre-mtls test
    /// here wants).
    fn token_only(tokens: Arc<dyn ControlTokenMinter>) -> EmbeddedTokenMinter {
        EmbeddedTokenMinter::new(tokens, Arc::new(NoCredentials))
    }

    #[tokio::test]
    async fn token_minter_maps_ok_and_parses_expiry() {
        use shed_broker::controltoken::MintedControlToken;
        struct Ok1;
        #[async_trait]
        impl ControlTokenMinter for Ok1 {
            async fn mint_control(&self, server: &str) -> Result<MintedControlToken, String> {
                assert_eq!(server, "prod");
                Ok(MintedControlToken {
                    token: "ctl-1".into(),
                    expires_at: Some("2026-07-03T00:00:00Z".into()),
                })
            }
        }
        let m = token_only(Arc::new(Ok1));
        let out = m.mint("prod").await.unwrap();
        assert_eq!(out.token, "ctl-1");
        let unix = out.expires_at_unix.expect("expiry parsed");
        assert_eq!(timefmt::format_iso8601(unix as i64), "2026-07-03T00:00:00Z");
    }

    #[tokio::test]
    async fn token_minter_fails_closed_on_error() {
        use shed_broker::controltoken::MintedControlToken;
        struct Err1;
        #[async_trait]
        impl ControlTokenMinter for Err1 {
            async fn mint_control(&self, _server: &str) -> Result<MintedControlToken, String> {
                Err("host key mismatch".into())
            }
        }
        let m = token_only(Arc::new(Err1));
        let e = m.mint("prod").await.unwrap_err();
        assert!(matches!(e, ShedError::Config(msg) if msg.contains("host key mismatch")));
    }

    #[tokio::test]
    async fn token_minter_absent_expiry_is_none() {
        use shed_broker::controltoken::MintedControlToken;
        struct Ok2;
        #[async_trait]
        impl ControlTokenMinter for Ok2 {
            async fn mint_control(&self, _server: &str) -> Result<MintedControlToken, String> {
                Ok(MintedControlToken {
                    token: "ctl".into(),
                    expires_at: None,
                })
            }
        }
        let m = token_only(Arc::new(Ok2));
        let out = m.mint("prod").await.unwrap();
        assert_eq!(out.token, "ctl");
        assert_eq!(out.expires_at_unix, None);
    }

    // (FIX 1) An Ok mint carrying an EMPTY token is fail-closed — never a valid
    // MintedToken (parity with token_minter.rs's `missing_or_empty_token_is_fail_closed`).
    #[tokio::test]
    async fn token_minter_fails_closed_on_empty_token() {
        use shed_broker::controltoken::MintedControlToken;
        struct EmptyTok;
        #[async_trait]
        impl ControlTokenMinter for EmptyTok {
            async fn mint_control(&self, _server: &str) -> Result<MintedControlToken, String> {
                // A protocol-legal Ok with a blank token — must NOT become a token.
                Ok(MintedControlToken {
                    token: String::new(),
                    expires_at: Some("2026-07-03T00:00:00Z".into()),
                })
            }
        }
        let m = token_only(Arc::new(EmptyTok));
        let e = m.mint("prod").await.unwrap_err();
        assert!(
            matches!(e, ShedError::Config(msg) if msg.contains("no token")),
            "empty token must be a fail-closed Config error"
        );
    }

    // ---- credential (mtls) minter adapter (plan 002 §7 P2) ---------------------------

    /// An in-process stand-in for the broker's `ControlCredentialMinter`: records every
    /// CSR it is handed and answers with a certificate whose PEM NAMES that CSR, so a
    /// test can prove the exact bytes relayed out came back bound to the same request.
    #[derive(Default)]
    struct RecordingCredentials {
        csrs: Mutex<Vec<String>>,
        /// `None` → answer with a certificate issued for the CSR just received;
        /// `Some(cred)` → answer with exactly `cred` ([`Self::answering`]).
        answer: Mutex<Option<MintedControlCredential>>,
    }

    impl RecordingCredentials {
        /// A stand-in scripted to answer every mint with `cred`.
        fn answering(cred: MintedControlCredential) -> Arc<Self> {
            let this = Arc::new(Self::default());
            *this.answer.lock().unwrap() = Some(cred);
            this
        }

        fn csrs(&self) -> Vec<String> {
            self.csrs.lock().unwrap().clone()
        }
    }

    #[async_trait]
    impl ControlCredentialMinter for RecordingCredentials {
        async fn mint_control_credential(
            &self,
            server: &str,
            csr_base64: &str,
        ) -> Result<MintedControlCredential, String> {
            assert_eq!(server, "prod");
            self.csrs.lock().unwrap().push(csr_base64.to_string());
            if let Some(scripted) = self.answer.lock().unwrap().clone() {
                return Ok(scripted);
            }
            Ok(MintedControlCredential {
                auth_mode: "mtls".into(),
                token: String::new(),
                client_cert: format!(
                    "-----BEGIN CERTIFICATE-----\nfor:{csr_base64}\n-----END CERTIFICATE-----\n"
                ),
                cert_serial: "0a1b".into(),
                expires_at: Some("2026-07-03T00:00:00Z".into()),
            })
        }
    }

    // The relay, stated as an equality: the CSR the PROVIDER composed is the CSR the
    // broker seam received, and the certificate that comes back is the one issued for
    // it. Nothing in this adapter generates a keypair — if it did, the certificate
    // below would be bound to a key the provider does not hold and could never present
    // (plan 001 D6 / 002 §7 P3).
    #[tokio::test]
    async fn credential_minter_relays_the_csr_verbatim_and_returns_the_certificate() {
        let creds = Arc::new(RecordingCredentials::default());
        let m = EmbeddedTokenMinter::new(Arc::new(NoTokens), creds.clone());

        // The embedded broker is this build — no socket, no version skew, so the
        // capability is unconditional and the provider always sends a CSR.
        assert!(m.supports_mtls());

        const CSR: &str = "Q1NSLURFUi1CWVRFUw==";
        let out = m
            .mint_credential("prod", &CredentialRequest::with_csr(CSR))
            .await
            .unwrap();

        assert_eq!(creds.csrs(), vec![CSR.to_string()], "relayed VERBATIM");
        match out {
            MintedCredential::Certificate(c) => {
                assert!(
                    c.cert_pem.contains("for:Q1NSLURFUi1CWVRFUw=="),
                    "the certificate must be the one issued for OUR csr: {}",
                    c.cert_pem
                );
                assert_eq!(c.serial, "0a1b");
                let unix = c.expires_at_unix.expect("expiry parsed");
                assert_eq!(timefmt::format_iso8601(unix as i64), "2026-07-03T00:00:00Z");
            }
            other => panic!("expected a certificate, got {other:?}"),
        }
    }

    // The same relay driven by the REAL shed-core provider, which is what production
    // wires up: the keypair and CSR are generated inside `ControlTokenProvider`, and
    // every mint composes a fresh one. The adapter is a pass-through in both directions.
    #[tokio::test]
    async fn the_provider_generates_the_keypair_and_the_adapter_only_carries_its_csr() {
        let creds = Arc::new(RecordingCredentials::default());
        let m: Arc<dyn TokenMinter> =
            Arc::new(EmbeddedTokenMinter::new(Arc::new(NoTokens), creds.clone()));
        let p = shed_core::token::ControlTokenProvider::new("prod".into(), m);

        // The stand-in's PEM is not a real certificate, so adoption fails — what this
        // asserts is the CSR that went OUT, and that a second mint composes a new one.
        assert!(p.credential().await.is_err());
        assert!(p.credential().await.is_err());

        let csrs = creds.csrs();
        assert_eq!(csrs.len(), 2);
        assert_ne!(csrs[0], csrs[1], "a fresh keypair (and CSR) per mint");
        for csr in &csrs {
            // Shape-check without pulling a base64 decoder into shed-app: standard
            // (padded) base64, and a leading 'M' — which encodes first-byte 0x30, the
            // DER SEQUENCE tag every CertificationRequest opens with. A placeholder or
            // a re-encoded something-else would not satisfy both.
            assert!(csr.len() % 4 == 0 && csr.len() > 100, "csr {csr:?}");
            assert!(
                csr.bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b == b'+' || b == b'/' || b == b'='),
                "std base64 alphabet: {csr:?}"
            );
            assert!(csr.starts_with('M'), "a DER SEQUENCE: {csr:?}");
        }
    }

    // A token-mode server answered over the SAME CSR-bearing request: the mode comes
    // from the credential's `auth_mode`, and the provider adopts a bearer token. This is
    // the mode-flip leg — the embedded path needs no reconfiguration to serve either.
    #[tokio::test]
    async fn credential_minter_maps_a_token_mode_answer() {
        let creds = RecordingCredentials::answering(MintedControlCredential {
            auth_mode: "token".into(),
            token: "ctl-1".into(),
            client_cert: String::new(),
            cert_serial: String::new(),
            expires_at: None,
        });
        let m = EmbeddedTokenMinter::new(Arc::new(NoTokens), creds);
        let out = m
            .mint_credential("prod", &CredentialRequest::with_csr("QUJD"))
            .await
            .unwrap();
        assert_eq!(
            out,
            MintedCredential::Token(MintedToken {
                token: "ctl-1".into(),
                expires_at_unix: None,
            })
        );
    }

    // Fail-closed on every answer that cannot authenticate — the embedded twin of
    // `token_minter.rs`'s UDS-reply guards, so the two modes behave identically.
    #[tokio::test]
    async fn credential_minter_fails_closed_on_unusable_answers() {
        // (a) an mtls answer with no certificate
        let creds = RecordingCredentials::answering(MintedControlCredential {
            auth_mode: "mtls".into(),
            token: String::new(),
            client_cert: String::new(),
            cert_serial: "0a".into(),
            expires_at: None,
        });
        let e = EmbeddedTokenMinter::new(Arc::new(NoTokens), creds)
            .mint_credential("prod", &CredentialRequest::with_csr("QUJD"))
            .await
            .unwrap_err();
        assert!(
            matches!(e, ShedError::Config(m) if m.contains("no certificate")),
            "mtls with no certificate must fail closed"
        );

        // (b) a token answer with a blank token (a protocol-legal Ok that is not a
        // credential) — and note the mode is read from `auth_mode`, so this is NOT
        // silently reinterpreted as some other shape.
        let creds = RecordingCredentials::answering(MintedControlCredential {
            auth_mode: "token".into(),
            token: String::new(),
            client_cert: "-----BEGIN CERTIFICATE-----".into(),
            cert_serial: String::new(),
            expires_at: None,
        });
        let e = EmbeddedTokenMinter::new(Arc::new(NoTokens), creds)
            .mint_credential("prod", &CredentialRequest::with_csr("QUJD"))
            .await
            .unwrap_err();
        assert!(matches!(e, ShedError::Config(m) if m.contains("no token")));

        // (c) an outright broker error (unknown server, host-key mismatch, …)
        struct Failing;
        #[async_trait]
        impl ControlCredentialMinter for Failing {
            async fn mint_control_credential(
                &self,
                _server: &str,
                _csr_base64: &str,
            ) -> Result<MintedControlCredential, String> {
                Err("unknown server \"prod\"".into())
            }
        }
        let e = EmbeddedTokenMinter::new(Arc::new(NoTokens), Arc::new(Failing))
            .mint_credential("prod", &CredentialRequest::with_csr("QUJD"))
            .await
            .unwrap_err();
        assert!(matches!(e, ShedError::Config(m) if m.contains("unknown server")));
    }

    /// The token seam for the credential-path tests: reaching it would mean the adapter
    /// took the legacy route for a CSR-bearing mint.
    struct NoTokens;
    #[async_trait]
    impl ControlTokenMinter for NoTokens {
        async fn mint_control(
            &self,
            _server: &str,
        ) -> Result<shed_broker::controltoken::MintedControlToken, String> {
            unreachable!("the credential path must never fall back to token.get")
        }
    }

    // ---- (iv) load_or_synthesize -----------------------------------------------------

    fn write_tmp(name: &str, content: &str) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(name);
        std::fs::write(&path, content).unwrap();
        (dir, path.to_string_lossy().into_owned())
    }

    #[test]
    fn load_or_synthesize_missing_yields_synthesized_default() {
        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("nope.yaml").to_string_lossy().into_owned();
        let bc = load_or_synthesize(&missing).unwrap();
        let cfg = match &bc {
            BrokerConfig::Synthesized(c) => c,
            other => panic!("expected Synthesized, got {other:?}"),
        };
        // discovery-mode select-all over ~/.shed/config.yaml.
        assert!(
            cfg.discovery.is_some(),
            "synthesized default is discovery-mode"
        );
        // ssh policy routes to the app gate; gate_namespaces is EXACTLY [ssh-agent]
        // (aws/docker default deny-all, not shed-desktop → not gated).
        assert_eq!(cfg.effective_policy(NS_SSH_AGENT), "shed-desktop");
        assert_eq!(cfg.gate_namespaces(), vec!["ssh-agent".to_string()]);
        assert_eq!(cfg.effective_policy(NS_AWS_CREDENTIALS), "deny-all");
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), "deny-all");
        // ssh.mode "" → auto-detect.
        assert_eq!(cfg.ssh_mode(), "");
    }

    #[test]
    fn load_or_synthesize_valid_file_is_honored() {
        let (_d, path) = write_tmp(
            "extensions.yaml",
            "ssh:\n  approval:\n    policy: approve-all\n",
        );
        let bc = load_or_synthesize(&path).unwrap();
        let cfg = match &bc {
            BrokerConfig::Loaded(c) => c,
            other => panic!("expected Loaded, got {other:?}"),
        };
        assert_eq!(cfg.effective_policy(NS_SSH_AGENT), "approve-all");
        // No discovery block → single-server mode (parity with the daemon).
        assert!(cfg.discovery.is_none());
    }

    #[test]
    fn load_or_synthesize_malformed_is_typed_error_no_panic() {
        // An invalid ssh policy fails `validate` → Config error (the daemon would exit 1;
        // the bridge returns a typed error and the app keeps running).
        let (_d, path) = write_tmp(
            "extensions.yaml",
            "ssh:\n  approval:\n    policy: bogus-policy\n",
        );
        let err = load_or_synthesize(&path).unwrap_err();
        assert!(matches!(err, BrokerError::Config(_)), "got {err:?}");
    }

    // ---- (v) mode detection ----------------------------------------------------------

    #[test]
    fn detect_mode_three_way() {
        assert_eq!(
            detect_mode(ModeProbe {
                desktop_socket_live: true,
                status_socket_live: true
            }),
            DetectedMode::External
        );
        assert_eq!(
            detect_mode(ModeProbe {
                desktop_socket_live: true,
                status_socket_live: false
            }),
            DetectedMode::External
        );
        assert_eq!(
            detect_mode(ModeProbe {
                desktop_socket_live: false,
                status_socket_live: true
            }),
            DetectedMode::HeadlessCoexist
        );
        assert_eq!(
            detect_mode(ModeProbe {
                desktop_socket_live: false,
                status_socket_live: false
            }),
            DetectedMode::Embedded
        );
    }

    #[test]
    fn resolve_mode_pref_overlay_pins_and_keeps_evidence() {
        let both_dead = ModeProbe {
            desktop_socket_live: false,
            status_socket_live: false,
        };
        let desktop_live = ModeProbe {
            desktop_socket_live: true,
            status_socket_live: false,
        };
        // Auto follows the probe.
        assert_eq!(
            resolve_mode(ModePref::Auto, both_dead),
            ResolvedMode {
                mode: EffectiveMode::Embedded,
                probe: both_dead
            }
        );
        // Embedded pins even with a live daemon (the 409 is surfaced, not hidden).
        assert_eq!(
            resolve_mode(ModePref::Embedded, desktop_live).mode,
            EffectiveMode::Embedded
        );
        // External pins even with no daemon.
        assert_eq!(
            resolve_mode(ModePref::External, both_dead).mode,
            EffectiveMode::External
        );
        // The evidence is retained regardless of the pref.
        assert_eq!(
            resolve_mode(ModePref::External, desktop_live).probe,
            desktop_live
        );
    }

    /// Serialize the env-mutating socket-dir probe tests (Rust runs tests in-process +
    /// parallel; `set_var`/`remove_var` on `SHED_HOST_AGENT_SOCKET_DIR` would race).
    fn socket_env_lock() -> std::sync::MutexGuard<'static, ()> {
        static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
        LOCK.lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    #[test]
    fn probe_sockets_real_temp_uds_matrix() {
        use std::os::unix::net::UnixListener;
        let _guard = socket_env_lock();
        // A short /tmp root keeps the sun_path under the AF_UNIX cap.
        let dir = tempfile::Builder::new()
            .prefix("shed-mode")
            .tempdir_in("/tmp")
            .unwrap();
        std::env::set_var("SHED_HOST_AGENT_SOCKET_DIR", dir.path());

        // Neither live → Embedded.
        assert_eq!(detect_mode(probe_sockets()), DetectedMode::Embedded);

        // Status-only live → HeadlessCoexist.
        let status = UnixListener::bind(dir.path().join("host-agent-status.sock")).unwrap();
        assert_eq!(detect_mode(probe_sockets()), DetectedMode::HeadlessCoexist);

        // Desktop live too → External (desktop wins).
        let desktop = UnixListener::bind(dir.path().join("host-agent.sock")).unwrap();
        let p = probe_sockets();
        assert!(p.desktop_socket_live && p.status_socket_live);
        assert_eq!(detect_mode(p), DetectedMode::External);
        // A pref override still pins.
        assert_eq!(
            resolve_mode(ModePref::Embedded, p).mode,
            EffectiveMode::Embedded
        );

        drop(status);
        drop(desktop);
        std::env::remove_var("SHED_HOST_AGENT_SOCKET_DIR");
    }

    #[test]
    fn probe_sockets_at_keys_off_the_given_path_and_sibling_status() {
        use std::os::unix::net::UnixListener;
        // No env: this variant probes an EXPLICIT desktop path (the embedder's
        // configured socket) + its sibling status socket — the path the harness
        // overrides that the default `probe_sockets` would miss.
        let dir = tempfile::Builder::new()
            .prefix("shed-mode-at")
            .tempdir_in("/tmp")
            .unwrap();
        let desktop = dir.path().join("host-agent.sock");

        // Neither bound → Embedded.
        assert_eq!(
            detect_mode(probe_sockets_at(&desktop)),
            DetectedMode::Embedded
        );

        // Only the sibling status socket live → HeadlessCoexist.
        let status = UnixListener::bind(dir.path().join(STATUS_SOCKET_FILE)).unwrap();
        assert_eq!(
            detect_mode(probe_sockets_at(&desktop)),
            DetectedMode::HeadlessCoexist
        );

        // The configured desktop socket live too → External (desktop wins).
        let d = UnixListener::bind(&desktop).unwrap();
        let p = probe_sockets_at(&desktop);
        assert!(p.desktop_socket_live && p.status_socket_live);
        assert_eq!(detect_mode(p), DetectedMode::External);

        drop(status);
        drop(d);
    }
}
