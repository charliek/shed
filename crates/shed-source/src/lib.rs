//! # shed-source — what a machine needs to be a credential source
//!
//! Plan 039 §3.7: the modules extracted from the retired credential broker (whose
//! on-disk paths they keep), and the delivery side.
//!
//! * [`machine`] — this machine's stable id + display name (`<state>/broker/machine.json`).
//! * [`paths`] — the state directory (`SHED_BROKER_STATE_DIR`, else the per-user data
//!   dir) and the home-directory helpers.
//! * [`credstore`] — the on-disk client-credential store (`<state>/broker/creds`).
//! * [`minter`] — the SSH-bootstrap minter and the per-server credential source.
//! * [`controltoken`] — the control-token provider, resolving servers through an
//!   injected [`controltoken::ServerResolver`] ([`discovery::resolver`]; this crate
//!   has no YAML parser).
//! * [`target`] — [`ServerTarget`], a resolved server (pure data).
//! * [`time`] — the std-only RFC3339 helpers.
//!
//! Every on-disk path and environment variable is the retired broker's, kept as it was,
//! so no machine id, host-key pin or certificate is lost.
//!
//! Isolation (`shed-core/tests/crate_isolation.rs`): this crate reaches only `shed-core`
//! and `shed-lane` among the workspace crates, and never `saphyr-parser`.
//!
//! The `test-support` feature exposes the minter's test seams and helpers
//! (`minter::tests`, the SSH-bootstrap seam in [`test_support`]) to an embedder's tests;
//! only `[dev-dependencies]` enable it.

// The SSH-bootstrap exchange — internal; the `minter` is its only consumer.
mod bootstrap;
pub mod controltoken;
pub mod credentials;
pub mod credstore;
pub mod delivery;
pub mod discovery;
pub mod machine;
pub mod minter;
pub mod paths;
pub mod paused;
pub mod target;
pub mod tasks;
pub mod time;
pub mod token_minter;

pub use target::ServerTarget;

/// The SSH-bootstrap seam ([`minter::CredentialMinter::with_runner`]'s runner and what it
/// exchanges), for an embedder's tests of the production wiring.
#[cfg(any(test, feature = "test-support"))]
pub mod test_support {
    pub use crate::bootstrap::{BootstrapError, BootstrapRunner, Bundle, Params};
}

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
