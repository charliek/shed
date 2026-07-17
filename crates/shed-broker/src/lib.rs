//! # shed-broker — the embeddable host-agent broker core
//!
//! The credential-broker engine ported from the Go `cmd/shed-host-agent`, carved out
//! of the `shed-host-agent` bin so it can be driven **two ways**: by the standalone
//! daemon (the `shed-host-agent` binary, which adds the CLI, signal handling, socket
//! **bind** ceremony, the Surface-A desktop UDS server, and the status UDS server),
//! and — later — embedded in-process by the desktop app.
//!
//! What lives here (daemon-agnostic): the shed-server plugin **bus** (`bus`), the
//! multi-server **supervisor** + discovery-source **watcher** (`supervisor`,
//! `watcher`, `discovery`), the credential **backends** (`ssh_backend`,
//! `ssh_backend_agent`, `aws_backend`, `docker_backend`, `egress`), the SSH-bootstrap
//! **minter** + control-token provider (`minter`, `bootstrap`, `controltoken`), the
//! **approval**/**audit** seams (`approval`, `audit`, `touchid`), the **config**
//! reader (`config`), socket **path resolution + liveness probes** (`sockets`), and
//! the **LiveStatus snapshot** builder + shared RFC3339 helpers (`status`).
//!
//! What stays in the `shed-host-agent` bin (daemon-only): `main` (CLI/signals/
//! `run_daemon`/`Log`), the socket **bind ceremony** (`socket_bind`), the status UDS
//! **server** + `status` CLI **client** (`status_server`), the Surface-A desktop
//! approval channel (`desktop`, `desktop_protocol`), and the `desktop-forwarding`
//! feature gating exactly those.
//!
//! ## Seams the embedder drives
//! * [`supervisor::Supervisor`] + [`supervisor::SharedDeps`] — the reconcile engine.
//! * [`approval::ApprovalGate`] — inject an app-native gate (built-ins:
//!   [`approval::ApproveAllGate`] / [`approval::DenyAllGate`] / the `touchid` gate);
//!   [`approval::select_builtin_gate`] routes a policy string to its built-in gate.
//! * [`audit::AuditFanout`] — an always-compiled fan-out seam feeding an activity UI.
//! * [`controltoken::ControlTokenProvider`] ([`controltoken::ControlTokenMinter`]) —
//!   the `token.get` control-token vend, minting-only and bus-independent.
//! * [`sockets::socket_is_live`] / [`sockets::connect_unix_timeout`] — the startup
//!   mode-probe an embedder uses to detect a running daemon.

pub mod approval;
pub mod audit;
pub mod aws_backend;
// The SSH-bootstrap minter (bootstrap exchange + credential source) is internal —
// the always-on `minter` is its only consumer.
mod bootstrap;
pub mod bus;
pub mod config;
// The control-token provider (`token.get`) — un-gated in the core (its
// `ControlTokenMinter`/`MintedControlToken` seam relocated here from the daemon's
// `desktop` module, so the core carries no dependency on the desktop server).
pub mod controltoken;
pub mod discovery;
pub mod docker_backend;
// The always-on egress-audit SSE consumer — internal; the supervisor spawns it.
mod egress;
pub mod minter;
pub mod sockets;
pub mod ssh_backend;
// The agent-forward SSH backend — internal; `ssh_backend` resolves into it.
mod ssh_backend_agent;
pub mod status;
// The native macOS Touch-ID / biometrics approval gate (`#[cfg(target_os="macos")]`
// inside; off-mac the biometric policies fail closed to deny-all).
pub mod touchid;
pub mod supervisor;
pub mod watcher;

// --- Curated crate-root re-exports (the API surface the daemon bin + the future
// embedded bridge consume). Everything not re-exported here stays reachable only via
// its module path or is `pub(crate)`-scoped inside the core. ---
pub use approval::{
    select_builtin_gate, ApprovalGate, ApprovalOutcome, ApproveAllGate, DenyAllGate,
};
pub use audit::{AuditEntry, AuditFanout, AuditSink, JsonlAuditSink};
pub use config::HostAgentConfig;
pub use controltoken::{ControlTokenMinter, ControlTokenProvider, MintedControlToken};
pub use discovery::{load_discovered_servers, ServerTarget};
pub use sockets::{connect_unix_timeout, desktop_socket_path, socket_is_live, status_socket_path};
pub use status::{build_live_status, LiveStatus, NamespaceHealth, ServerHealth};
pub use supervisor::{SharedDeps, Supervisor};

/// Serialize the tests that mutate process-global environment variables (Rust runs
/// tests in-process and in parallel, so `set_var`/`remove_var` would otherwise
/// race across modules). Poisoning is ignored — the lock only orders access.
#[cfg(test)]
pub(crate) fn env_lock() -> std::sync::MutexGuard<'static, ()> {
    static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    ENV_LOCK
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}
