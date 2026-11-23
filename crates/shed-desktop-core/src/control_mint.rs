//! The app's control-certificate mint (plan 039 §3.7, PR B): the [`TokenMinter`] the
//! desktop hands `shed_app::Backend`, built from `shed-source` alone.
//!
//! On a secure server the Backend's per-server credential provider generates a keypair,
//! and this minter relays its CSR over the server's SSH `_bootstrap` channel
//! ([`CredentialMinter`], pinned by `~/.shed/known_hosts`) in the CONTROL scope, through
//! `shed-source`'s control-token provider resolving servers from the shed CLI config
//! ([`shed_source::discovery::resolver`]: an entry with no explicit `ssh_port` has no SSH
//! endpoint, so nothing is minted for it). [`SourceTokenMinter`] maps what the server
//! issued onto shed-core's credential and refuses every other shape.
//!
//! Nothing here holds state of its own: the minted control certificate lives in the
//! Backend's provider (in memory, re-minted on a cold launch), and this machine's identity
//! and credentials-scope store (`<state>/broker/machine.json`, `<state>/broker/creds`)
//! belong to the source runtime ([`crate::delivery`]), which reads them as they are.

use std::sync::Arc;

use shed_core::token::TokenMinter;
use shed_source::controltoken::ControlTokenProvider;
use shed_source::minter::{CredentialMinter, Minter};
use shed_source::token_minter::SourceTokenMinter;

/// The `known_hosts` the SSH minter pins servers against: the file `shed server add`
/// writes.
pub const KNOWN_HOSTS: &str = "~/.shed/known_hosts";

/// The production control minter: the SSH-bootstrap [`CredentialMinter`] over
/// `known_hosts`, resolving servers from the shed CLI config at `shed_config`.
pub fn control_minter(shed_config: &str, known_hosts: &str) -> Arc<dyn TokenMinter> {
    control_minter_over(Arc::new(CredentialMinter::new(known_hosts)), shed_config)
}

/// [`control_minter`] over a given SSH-bootstrap minter (tests inject one).
pub fn control_minter_over(minter: Arc<dyn Minter>, shed_config: &str) -> Arc<dyn TokenMinter> {
    let provider = Arc::new(ControlTokenProvider::new(
        minter,
        shed_config,
        shed_source::discovery::resolver(),
    ));
    Arc::new(SourceTokenMinter::new(provider))
}

#[cfg(test)]
#[path = "control_mint_tests.rs"]
mod tests;
