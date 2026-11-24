//! Server discovery from the shed CLI config (plan 039 §3.7, commit 2): every server
//! `~/.shed/config.yaml` lists, as a [`ServerTarget`], read with shed-core's own reader
//! ([`shed_core::config::ShedConfig`]) rather than the broker's saphyr-backed one, which
//! stays in the broker with its selector logic.
//!
//! # A missing `ssh_port` mints nothing
//!
//! [`ShedConfig`] fills an absent `ssh_port` with 22, which is right for the app's SSH
//! uses and wrong here: the broker's reader reads it as 0, "no SSH endpoint", and the
//! minter refuses to bootstrap over a port the config never named. This module keeps
//! that rule. An entry's `ssh_port` is taken only when the entry names it explicitly,
//! as shed-core's own reader reads the file ([`ShedConfig::explicit_ssh_ports`]: the
//! same document-marker, comment and quoting rules as [`ShedConfig::parse`], so the two
//! cannot disagree on a file's form), and that port is the one the entry holds;
//! otherwise it is 0. A port it is 0 for mints nothing, which fails closed.
//!
//! `ShedServerEntry` has no `credentials_token`, so [`ServerTarget::token`] is always
//! empty here: a source authenticates with its certificate (or nothing, in open mode).

use std::sync::Arc;

use shed_core::config::{ShedConfig, ShedServerEntry};

use crate::controltoken::ServerResolver;
use crate::target::ServerTarget;

/// The targets of `text` (the config's contents): one per server entry shed-core reads,
/// in its order (sorted by name). An entry it refuses (no host it could trust) is left
/// out. A file refused whole is an error, with the reader's reason.
pub fn parse_servers(text: &str) -> Result<Vec<ServerTarget>, String> {
    let config = ShedConfig::parse(text);
    if let Some(reason) = config.refused {
        return Err(format!("config.yaml is not read: {reason}"));
    }
    let explicit = ShedConfig::explicit_ssh_ports(text);
    Ok(config
        .servers
        .iter()
        .filter(|e| !e.host.is_empty())
        .map(|e| target_of(e, explicit.get(&e.name).copied()))
        .collect())
}

/// [`parse_servers`] of the file at `path` (already tilde-expanded). A missing file is
/// no servers, not an error; one that cannot be read is an error.
pub fn load_servers(path: &str) -> Result<Vec<ServerTarget>, String> {
    match std::fs::read_to_string(path) {
        Ok(text) => parse_servers(&text),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Vec::new()),
        Err(e) => Err(format!("reading {path}: {e}")),
    }
}

/// [`load_servers`] as the control-token provider's injected resolver
/// ([`crate::controltoken::ControlTokenProvider::new`]).
pub fn resolver() -> ServerResolver {
    Arc::new(load_servers)
}

fn target_of(e: &ShedServerEntry, explicit_port: Option<u16>) -> ServerTarget {
    let endpoint = e.resolved_endpoint();
    ServerTarget {
        name: e.name.clone(),
        url: endpoint.base_url,
        token: String::new(),
        tls_fingerprint: endpoint.pin,
        ssh_host: e.host.clone(),
        // Only a port the entry names itself.
        ssh_port: match explicit_port {
            Some(p) if p == e.ssh_port => p,
            _ => 0,
        },
        auth_mode: e.auth_mode.clone(),
        user: e.user.clone(),
        identity_file: e.identity_file.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const CONFIG: &str = "\
servers:
    named:
        host: named.example.test
        http_port: 8080
        ssh_port: 2222
        api_url: https://named.example.test:18443
        tls_cert_fingerprint: SHA256:ABCD
        auth_mode: mtls
        user: ada
    portless:
        host: portless.example.test
        http_port: 8080
    \"quoted\":
        host: 127.0.0.1
        \"ssh_port\": \"2200\" # a comment
    zero:
        host: zero.example.test
        ssh_port: 0
default_server: named
";

    fn by_name(targets: &[ServerTarget], name: &str) -> ServerTarget {
        targets
            .iter()
            .find(|t| t.name == name)
            .unwrap_or_else(|| panic!("no target {name}: {targets:?}"))
            .clone()
    }

    #[test]
    fn every_entry_is_a_target_with_its_endpoint() {
        let targets = parse_servers(CONFIG).unwrap();
        assert_eq!(targets.len(), 4);
        let named = by_name(&targets, "named");
        assert_eq!(named.url, "https://named.example.test:18443");
        assert_eq!(named.tls_fingerprint, "sha256:abcd");
        assert_eq!(named.ssh_host, "named.example.test");
        assert_eq!(named.ssh_port, 2222);
        assert_eq!(named.auth_mode, "mtls");
        assert_eq!(named.user, "ada");
        assert!(named.token.is_empty(), "no credentials_token is ever read");
        assert!(named.is_secure() && named.is_mtls());
        let quoted = by_name(&targets, "quoted");
        assert_eq!(quoted.url, "http://127.0.0.1:8080");
        assert_eq!(quoted.ssh_port, 2200);
    }

    /// The trap of plan 039 S0: shed-core reads an absent `ssh_port` as 22, and a source
    /// built on it would mint over a port the config never named. Here it is 0.
    #[test]
    fn a_missing_ssh_port_is_no_ssh_endpoint() {
        let config = ShedConfig::parse(CONFIG);
        let portless = config
            .servers
            .iter()
            .find(|e| e.name == "portless")
            .unwrap();
        assert_eq!(portless.ssh_port, 22, "shed-core's own default");
        let targets = parse_servers(CONFIG).unwrap();
        assert_eq!(by_name(&targets, "portless").ssh_port, 0);
        assert_eq!(by_name(&targets, "zero").ssh_port, 0);
    }

    /// The control-token provider refuses to mint for an entry with no `ssh_port`
    /// through this resolver, before any SSH.
    #[tokio::test]
    async fn the_resolver_refuses_to_mint_without_an_explicit_port() {
        use crate::controltoken::{ControlCredentialMinter, ControlTokenProvider};
        use crate::minter::{Minted, Minter, MinterError};

        struct Never;
        #[async_trait::async_trait]
        impl Minter for Never {
            async fn mint(&self, _: &ServerTarget, _: &str) -> Result<Minted, MinterError> {
                panic!("no mint may start")
            }
        }

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        let text = CONFIG.replace("http://", "https://").replace(
            "        http_port: 8080\n    \"quoted\"",
            "        http_port: 8080\n        api_url: https://portless.example.test:18443\n    \"quoted\"",
        );
        std::fs::write(&path, text).unwrap();
        let provider =
            ControlTokenProvider::new(Arc::new(Never), path.to_str().unwrap(), resolver());
        let err = provider
            .mint_control_credential("portless", "")
            .await
            .unwrap_err();
        assert!(err.contains("no ssh endpoint"), "{err}");
        let err = provider
            .mint_control_credential("absent", "")
            .await
            .unwrap_err();
        assert!(err.contains("unknown server"), "{err}");
    }

    #[test]
    fn only_the_entrys_own_field_is_its_port() {
        let text = "\
servers:
  a:
    host: a.example.test
    nested:
      ssh_port: 2222
  c:
    host: c.example.test
    ssh_port: ~
  d:
    host: d.example.test
    ssh_port: '2203'
  w:
    host: w.example.test
    ssh_port: twenty-two
other:
  e:
    host: e.example.test
    ssh_port: 2204
";
        let targets = parse_servers(text).unwrap();
        assert_eq!(
            by_name(&targets, "a").ssh_port,
            0,
            "a deeper key is not the entry's"
        );
        assert_eq!(by_name(&targets, "c").ssh_port, 0, "a null");
        assert_eq!(by_name(&targets, "d").ssh_port, 2203);
        assert_eq!(by_name(&targets, "w").ssh_port, 0, "not a port");
        assert!(targets.iter().all(|t| t.name != "e"), "not under servers:");
    }

    /// Review of plan 039 PR B: a config the core reader reads — a leading `---`
    /// document marker, `servers: # comment`, comments after an entry key and a
    /// field — keeps its explicit port. (The line scanner this replaced stopped at
    /// the marker and did not see the commented `servers:`, so a cold launch read
    /// port 0 and refused to mint "no ssh endpoint".)
    #[test]
    fn a_document_marker_and_comments_keep_the_explicit_port() {
        for text in [
            "---\nservers:\n    a:\n        host: a.example.test\n        ssh_port: 2222\n",
            "servers: # the shed servers\n    a:\n        host: a.example.test\n        ssh_port: 2222\n",
            "--- # cfg\n# header\nservers:   # comment\n    a: # entry\n        host: a.example.test\n        ssh_port: 2222 # port\n    b:\n        host: b.example.test\n",
        ] {
            let core = ShedConfig::parse(text);
            assert!(core.refused.is_none(), "the core reader reads it: {text:?}");
            assert_eq!(
                core.servers.iter().find(|e| e.name == "a").map(|e| e.ssh_port),
                Some(2222),
                "{text:?}"
            );
            let targets = parse_servers(text).unwrap();
            assert_eq!(by_name(&targets, "a").ssh_port, 2222, "{text:?}");
            if let Some(b) = targets.iter().find(|t| t.name == "b") {
                assert_eq!(b.ssh_port, 0, "a missing port still refuses: {text:?}");
            }
        }
    }

    /// The same forms through the resolver the control-token provider and the delivery
    /// stream both use: the explicit port resolves, a missing one is still 0.
    #[test]
    fn the_resolver_reads_a_marked_and_commented_config() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        std::fs::write(
            &path,
            "---\nservers: # comment\n    a:\n        host: a.example.test\n        ssh_port: 2222\n    b:\n        host: b.example.test\n",
        )
        .unwrap();
        let targets = resolver()(path.to_str().unwrap()).unwrap();
        assert_eq!(by_name(&targets, "a").ssh_port, 2222);
        assert_eq!(by_name(&targets, "b").ssh_port, 0);
    }

    /// A value that is no port is 0.
    #[test]
    fn a_port_out_of_range_is_none() {
        let text = "servers:\n    a:\n        host: a.example.test\n        ssh_port: 70000\n";
        assert_eq!(parse_servers(text).unwrap()[0].ssh_port, 0);
    }

    #[test]
    fn a_refused_file_is_an_error_and_a_missing_one_is_empty() {
        let err = parse_servers("servers:\n    a: {}\nservers:\n    b: {}\n").unwrap_err();
        assert!(err.contains("not read"), "{err}");
        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("none.yaml");
        assert_eq!(load_servers(missing.to_str().unwrap()).unwrap(), vec![]);
    }

    /// An entry shed-core reads but holds no host for (one it refuses) is left out.
    #[test]
    fn an_entry_with_no_host_is_left_out() {
        let text = "servers:\n    a:\n        host:\n        ssh_port: 22\n    b:\n        host: b.example.test\n";
        let targets = parse_servers(text).unwrap();
        assert_eq!(
            targets.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(),
            vec!["b"]
        );
    }
}
