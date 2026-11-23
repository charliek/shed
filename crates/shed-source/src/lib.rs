//! # shed-source — what a machine needs to be a credential source
//!
//! Plan 039 §3.7, commit 1: the neutral modules extracted, unchanged in behaviour, from
//! `shed-broker`, which depends on this crate and re-exports every module under its old
//! path (`shed_broker::{machine, paths, credstore, minter, controltoken}`, and
//! `status`/`discovery`/`config` re-export the helpers below), so no broker caller moved.
//!
//! * [`machine`] — this machine's stable id + display name (`<state>/broker/machine.json`).
//! * [`paths`] — the state directory (`SHED_BROKER_STATE_DIR`, else the per-user data
//!   dir) and the home-directory helpers.
//! * [`credstore`] — the on-disk client-credential store (`<state>/broker/creds`).
//! * [`minter`] — the SSH-bootstrap minter and the per-server credential source.
//! * [`controltoken`] — the control-token provider, resolving servers through an
//!   injected [`controltoken::ServerResolver`] (the broker passes its saphyr-backed
//!   reader; this crate has no YAML parser).
//! * [`target`] — [`ServerTarget`], a resolved server (pure data).
//! * [`time`] — the std-only RFC3339 helpers.
//!
//! Every on-disk path and environment variable is the broker's, kept as it was, so no
//! machine id, host-key pin or certificate is lost across the move.
//!
//! Isolation (`shed-core/tests/crate_isolation.rs`): this crate reaches only `shed-core`
//! and `shed-lane` among the workspace crates, and never `saphyr-parser`.
//!
//! The `test-support` feature exposes the minter's test seams and helpers
//! (`minter::tests`) to `shed-broker`'s tests; only its `[dev-dependencies]` enable it.

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
pub mod target;
pub mod tasks;
pub mod time;
pub mod token_minter;

pub use target::ServerTarget;

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
