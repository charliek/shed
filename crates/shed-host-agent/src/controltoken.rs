//! The control-token provider — a faithful Rust port of
//! `cmd/shed-host-agent/controltoken.go`. The `ServerTarget` / `load_discovered_servers`
//! slice its resolve path needs now lives in the always-on [`crate::discovery`] module
//! (hoisted out of here so the headless supervisor path can use it too).
//!
//! [`ControlTokenProvider`] mints and caches CONTROL-scoped tokens for named servers on
//! the desktop's behalf (the `token.get` UDS request). Minting is BROAD: it can mint for
//! any server in the shed CLI config the agent's SSH key is allowlisted on — not only the
//! discovery-scoped servers it brokers credentials for. It **always mints fresh** (never
//! serves a per-server source's completed cache) because a restarted server silently
//! invalidates control tokens with no signal to the agent.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use crate::desktop::{ControlTokenMinter, MintedControlToken};
use crate::discovery::{load_discovered_servers, ServerTarget};
use crate::minter::{new_credential_source, CredentialSource, Minter, SCOPE_CONTROL};
use crate::status::rfc3339_utc;

/// The shed CLI config (`~/.shed/config.yaml`) the control-token provider resolves
/// servers from in single-server mode (mirror `config.go:DefaultDiscoverySource`).
/// Re-exported from `config` (the single canonical definition the discovery
/// `apply_defaults` also uses) so `main.rs`'s `controltoken::DEFAULT_DISCOVERY_SOURCE`
/// reference stays stable.
pub use crate::config::DEFAULT_DISCOVERY_SOURCE;

/// Answers `token.get` by minting CONTROL tokens over SSH (mirror
/// `controltoken.go:controlTokenProvider`).
pub struct ControlTokenProvider {
    minter: Arc<dyn Minter>,
    /// The shed CLI config to resolve servers from (tilde-expanded).
    source_path: String,
    sources: Mutex<HashMap<String, Arc<CredentialSource>>>,
}

impl ControlTokenProvider {
    pub fn new(minter: Arc<dyn Minter>, source_path: &str) -> Self {
        Self {
            minter,
            source_path: crate::config::expand_tilde(source_path),
            sources: Mutex::new(HashMap::new()),
        }
    }

    /// Look the server up by name in the shed CLI config. Minting requires both an SSH
    /// endpoint (to bootstrap over) and a secure (https) server — an open http server
    /// needs no token and can't be minted for. Rejects **before any mint**, checking the
    /// ssh-endpoint gate BEFORE the secure gate (mirror `controltoken.go:resolve`).
    fn resolve(&self, name: &str) -> Result<ServerTarget, String> {
        let targets = load_discovered_servers(&self.source_path)
            .map_err(|e| format!("reading server config: {e}"))?;
        for t in targets {
            if t.name == name {
                if t.ssh_host.is_empty() || t.ssh_port == 0 {
                    return Err(format!(
                        "server \"{name}\" has no ssh endpoint to mint a control token over"
                    ));
                }
                if !t.is_secure() {
                    return Err(format!(
                        "server \"{name}\" is not a secure (https) server; control-token minting is unavailable"
                    ));
                }
                return Ok(t);
            }
        }
        Err(format!("unknown server \"{name}\""))
    }

    /// The per-server control-token source, (re)creating it when the server's endpoint
    /// changed since it was cached (mirror `controltoken.go:sourceFor`).
    fn source_for(&self, name: &str, target: &ServerTarget) -> Arc<CredentialSource> {
        let mut sources = self.sources.lock().unwrap();
        if let Some(src) = sources.get(name) {
            if src.target() == target {
                return src.clone();
            }
        }
        let src = new_credential_source(self.minter.clone(), target.clone(), SCOPE_CONTROL);
        sources.insert(name.to_string(), src.clone());
        src
    }
}

#[async_trait::async_trait]
impl ControlTokenMinter for ControlTokenProvider {
    /// Mint a control token for `server`, always FRESH (the per-server source's completed
    /// cache is never served — a restarted server silently invalidates control tokens).
    /// `Err(message)` fails the `token.get` closed with the message (mirror
    /// `controltoken.go:Token` + `handleTokenGet`'s `resp.Error = err.Error()`).
    async fn mint_control(&self, server: &str) -> Result<MintedControlToken, String> {
        let target = self.resolve(server)?;
        let source = self.source_for(server, &target);
        let (token, expiry) = source.force_token_with_expiry().await?;
        Ok(MintedControlToken {
            token,
            // Go: exp.UTC().Format(time.RFC3339) only when !exp.IsZero(); a None expiry
            // omits `token.response.expires_at`.
            expires_at: expiry.map(rfc3339_utc),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::minter::{Minted, MinterError};
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn write_config(content: &str) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        std::fs::write(&path, content).unwrap();
        (dir, path.to_string_lossy().into_owned())
    }

    const PROD_SECURE: &str = "\
servers:
  prod:
    api_url: https://prod.example:8443
    host: prod.example
    ssh_port: 2222
";

    /// A `Minter` returning canned tokens in sequence (repeating the last) + call count.
    struct FakeMinter {
        results: Mutex<Vec<String>>,
        calls: AtomicUsize,
    }
    impl FakeMinter {
        fn new(tokens: &[&str]) -> Arc<Self> {
            Arc::new(Self {
                results: Mutex::new(tokens.iter().map(|s| s.to_string()).collect()),
                calls: AtomicUsize::new(0),
            })
        }
    }
    #[async_trait::async_trait]
    impl Minter for FakeMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            let i = self.calls.fetch_add(1, Ordering::SeqCst);
            let toks = self.results.lock().unwrap();
            let idx = i.min(toks.len() - 1);
            Ok(Minted {
                token: toks[idx].clone(),
                expiry: None,
            })
        }
    }

    #[tokio::test]
    async fn always_mints_fresh() {
        let (_d, cfg) = write_config(PROD_SECURE);
        let fm = FakeMinter::new(&["ctl-1", "ctl-2"]);
        let p = ControlTokenProvider::new(fm.clone(), &cfg);

        assert_eq!(p.mint_control("prod").await.unwrap().token, "ctl-1");
        // A second sequential call re-mints — no completed-cache short-circuit.
        assert_eq!(p.mint_control("prod").await.unwrap().token, "ctl-2");
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn errors_before_any_mint() {
        let (_d, cfg) = write_config(
            "\
servers:
  open-no-ssh:
    host: open1.example
    http_port: 8080
  open-http:
    host: open2.example
    http_port: 8080
    ssh_port: 2222
",
        );
        let fm = FakeMinter::new(&["x"]);
        let p = ControlTokenProvider::new(fm.clone(), &cfg);

        // unknown / no-ssh-endpoint / open-http-with-ssh all error before minting.
        assert!(p.mint_control("missing").await.is_err());
        assert!(p.mint_control("open-no-ssh").await.is_err());
        assert!(p.mint_control("open-http").await.is_err());
        assert_eq!(fm.calls.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn error_strings_match_go() {
        let (_d, cfg) = write_config(
            "\
servers:
  open-no-ssh:
    host: open1.example
    http_port: 8080
  open-http:
    host: open2.example
    http_port: 8080
    ssh_port: 2222
",
        );
        let p = ControlTokenProvider::new(FakeMinter::new(&["x"]), &cfg);
        assert_eq!(
            p.mint_control("missing").await.unwrap_err(),
            "unknown server \"missing\""
        );
        assert_eq!(
            p.mint_control("open-no-ssh").await.unwrap_err(),
            "server \"open-no-ssh\" has no ssh endpoint to mint a control token over"
        );
        assert_eq!(
            p.mint_control("open-http").await.unwrap_err(),
            "server \"open-http\" is not a secure (https) server; control-token minting is unavailable"
        );
    }

    // The `load_discovered_servers` unit + golden tests moved to `discovery.rs`
    // alongside the hoisted function (the ssh_port=0-vs-22 + empty-host + sort +
    // malformed divergences it pins).
}
