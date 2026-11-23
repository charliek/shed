//! # shed-broker — the credential broker core the desktop app embeds
//!
//! The credential-broker engine, ported from the retired Go daemon. The desktop
//! app runs it in-process (`shed-app`'s `broker` feature); the test-only
//! `shed-broker-harness` hosts the same broker for the wire harness. The standalone
//! daemon that used to drive it was removed in plan 037.
//!
//! What lives here: the shed-server plugin **bus** (`bus`), the
//! multi-server **supervisor** + discovery-source **watcher** (`supervisor`,
//! `watcher`, `discovery`), the credential **backends** (`ssh_backend`,
//! `ssh_backend_agent`, `aws_backend`, `docker_backend`, `egress`), the
//! **approval**/**audit** seams (`approval`, `audit`, `touchid`), the **config**
//! reader (`config`), and the **health** types (`status`).
//!
//! The SSH-bootstrap **minter** + control-token provider (`minter`, `controltoken`), the
//! client-credential store (`credstore`), the **state directory** (`paths`) and this
//! machine's **identity** file (`machine`) live in `shed-source` (plan 039 §3.7) and are
//! re-exported here under their old paths, as are its RFC3339 helpers (through `status`),
//! `ServerTarget` (through `discovery`) and the home-directory helpers (through `config`).
//!
//! ## Seams the embedder drives
//! * [`supervisor::Supervisor`] + [`supervisor::SharedDeps`] — the reconcile engine.
//! * [`approval::ApprovalGate`] — inject an app-native gate (built-ins:
//!   [`approval::ApproveAllGate`] / [`approval::DenyAllGate`] / the `touchid` gate);
//!   [`approval::select_builtin_gate`] routes a policy string to its built-in gate.
//! * [`audit::AuditFanout`] — an always-compiled fan-out seam feeding an activity UI.
//! * [`controltoken::ControlTokenProvider`] ([`controltoken::ControlTokenMinter`]) —
//!   the `token.get` control-token vend, minting-only and bus-independent.

pub mod approval;
pub mod audit;
pub mod aws_backend;
pub mod bus;
pub mod config;
// The control-token provider's tests: they resolve through this crate's saphyr-backed
// `discovery::load_discovered_servers`, so they live here, not in `shed-source`.
#[cfg(test)]
mod controltoken_tests;
pub mod discovery;
pub mod docker_backend;
// The always-on egress-audit SSE consumer — internal; the supervisor spawns it.
mod egress;
// What the broker will sign for a shed: the pure sign-request parser and policy (plan
// 035 §3.5). Nothing calls it from the signing path yet.
pub mod sign_check;
pub mod ssh_backend;
#[cfg(test)]
mod testalert;
// The agent-forward SSH backend — internal; `ssh_backend` resolves into it.
mod ssh_backend_agent;
// The hand-rolled SSH-wire reader shared by the agent-forward backend and `sign_check`.
mod ssh_wire;
pub mod status;
// The native macOS Touch-ID / biometrics approval gate (`#[cfg(target_os="macos")]`
// inside; off-mac the biometric policies fail closed to deny-all).
pub mod supervisor;
pub mod touchid;
pub mod watcher;

// The modules extracted to `shed-source` (plan 039 §3.7), under their old paths so every
// `crate::minter::…` (and embedder `shed_broker::machine::…`) path keeps resolving:
// - `controltoken`: the control-token provider (`token.get`/`credential.get`).
// - `credstore`: the agent's own on-disk client-credential store (the mtls certificate +
//   key it enrolls for itself), at `<state>/broker/creds`.
// - `machine`: this machine's stable id + display name (`<state>/broker/machine.json`;
//   plan 035 §3.4).
// - `minter`: the SSH-bootstrap minter and per-server credential source (the bootstrap
//   exchange itself is internal to `shed-source`).
// - `paths`: the state directory (`SHED_BROKER_STATE_DIR`, else the per-user data dir).
pub use shed_source::{controltoken, credstore, machine, minter, paths};

// --- Curated crate-root re-exports (the API surface the embedded bridge and the
// harness consume). Everything not re-exported here stays reachable only via
// its module path or is `pub(crate)`-scoped inside the core. ---
pub use approval::{
    select_builtin_gate, ApprovalGate, ApprovalOutcome, ApproveAllGate, DenyAllGate,
};
pub use audit::{AuditEntry, AuditFanout, AuditSink, JsonlAuditSink};
pub use config::ExtensionsYaml;
pub use controltoken::{
    ControlCredentialMinter, ControlTokenMinter, ControlTokenProvider, MintedControlCredential,
    MintedControlToken,
};
pub use credstore::CredStore;
pub use discovery::{load_discovered_servers, ServerTarget};
pub use machine::MachineIdentity;
pub use status::{ActiveSource, NamespaceHealth, ServerHealth, SshSigningStatus};
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

/// The on-disk paths the extraction to `shed-source` (plan 039 §3.7) must not move: a
/// moved `machine.json` would make every server see a new machine, and a moved `creds`
/// directory would lose every certificate. Pinned as literals, through the broker's own
/// re-exported paths, under the `SHED_BROKER_STATE_DIR` override the app and both
/// harnesses set.
#[cfg(test)]
mod extracted_paths_tests {
    use std::path::{Path, PathBuf};

    #[test]
    fn machine_json_and_creds_keep_their_paths() {
        let _guard = crate::env_lock();
        std::env::set_var("SHED_BROKER_STATE_DIR", "/pinned/state");
        assert_eq!(crate::paths::state_dir(), PathBuf::from("/pinned/state"));
        assert_eq!(
            crate::machine::machine_state_path(),
            PathBuf::from("/pinned/state/broker/machine.json")
        );
        for scope in [
            crate::minter::SCOPE_CREDENTIALS,
            crate::minter::SCOPE_CONTROL,
        ] {
            assert_eq!(
                crate::credstore::CredStore::for_scope(scope).root(),
                Path::new("/pinned/state/broker/creds").join(scope)
            );
        }
        assert_eq!(
            crate::credstore::CredStore::for_scope("credentials").root(),
            Path::new("/pinned/state/broker/creds/credentials")
        );
        std::env::remove_var("SHED_BROKER_STATE_DIR");
    }
}
