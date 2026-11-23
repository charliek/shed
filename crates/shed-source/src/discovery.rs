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
//! that rule. An entry's `ssh_port` is taken only when a line of the entry names it
//! explicitly ([`explicit_ssh_ports`]) and that line's value is the port shed-core read;
//! otherwise it is 0. When the two readers disagree, or the scanner cannot place a line,
//! the answer is 0: nothing is minted, which fails closed.
//!
//! `ShedServerEntry` has no `credentials_token`, so [`ServerTarget::token`] is always
//! empty here: a source authenticates with its certificate (or nothing, in open mode).

use std::collections::HashMap;
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
    let explicit = explicit_ssh_ports(text);
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
        // Only a port both readers agree the entry names.
        ssh_port: match explicit_port {
            Some(p) if p == e.ssh_port => p,
            _ => 0,
        },
        auth_mode: e.auth_mode.clone(),
        user: e.user.clone(),
        identity_file: e.identity_file.clone(),
    }
}

/// The entries of `text`'s top-level `servers:` block whose own lines name `ssh_port`,
/// with the port each names. A line-based scan of the block form the shed CLI writes
/// (`servers:`, then one indented key per entry, then the entry's keys one level
/// deeper): an entry key is a plain or quoted key at the block's first indent, and only
/// a field at that entry's first field indent counts. Anything it cannot place, a value
/// that is not a port (`0`, a word, a null), or an entry that names it twice, gives no
/// port for the entry. Comments and blank lines are skipped.
pub fn explicit_ssh_ports(text: &str) -> HashMap<String, u16> {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let mut out: HashMap<String, Option<u16>> = HashMap::new();
    let mut in_servers = false;
    let mut entry_indent: Option<usize> = None;
    let mut field_indent: Option<usize> = None;
    let mut entry: Option<String> = None;
    for raw in text.lines() {
        let line = raw.trim_end();
        let content = line.trim_start_matches(' ');
        if content.is_empty() || content.starts_with('#') {
            continue;
        }
        if content.starts_with('\t') {
            // Tab indentation is not YAML's; nothing after it is placed.
            entry = None;
            continue;
        }
        let indent = line.len() - content.len();
        if indent == 0 {
            if matches!(content, "---" | "...") {
                break;
            }
            in_servers = split_key(content).is_some_and(|(k, v)| k == "servers" && v.is_empty());
            entry_indent = None;
            field_indent = None;
            entry = None;
            continue;
        }
        if !in_servers {
            continue;
        }
        let entry_at = *entry_indent.get_or_insert(indent);
        if indent == entry_at {
            entry = split_key(content).map(|(k, _)| k);
            field_indent = None;
            continue;
        }
        if indent < entry_at {
            // Not a shape the CLI writes: stop placing lines.
            in_servers = false;
            entry = None;
            continue;
        }
        let Some(name) = entry.as_ref() else { continue };
        let field_at = *field_indent.get_or_insert(indent);
        if indent != field_at {
            continue;
        }
        if let Some((key, value)) = split_key(content) {
            if key == "ssh_port" {
                let port = port_value(&value);
                out.entry(name.clone())
                    .and_modify(|p| *p = None)
                    .or_insert(port);
            }
        }
    }
    out.into_iter()
        .filter_map(|(k, v)| v.map(|p| (k, p)))
        .collect()
}

/// A `key: value` line's key (unquoted) and its raw value; `None` when the line is not
/// one. A plain key ends at the first `:` followed by a space or the line's end.
fn split_key(content: &str) -> Option<(String, String)> {
    let (key, rest) = match content.chars().next()? {
        q @ ('"' | '\'') => {
            let end = content[1..].find(q)? + 1;
            let key = &content[1..end];
            if key.contains('\\') || (q == '\'' && content[end + 1..].starts_with('\'')) {
                // An escape or a doubled quote: not a key this scan reads exactly.
                return None;
            }
            (key.to_string(), content[end + 1..].strip_prefix(':')?)
        }
        _ => {
            let mut at = None;
            for (i, _) in content.match_indices(':') {
                let after = &content[i + 1..];
                if after.is_empty() || after.starts_with(' ') {
                    at = Some(i);
                    break;
                }
            }
            let i = at?;
            (content[..i].trim_end().to_string(), &content[i + 1..])
        }
    };
    if !(rest.is_empty() || rest.starts_with(' ')) {
        return None;
    }
    Some((key, rest.trim().to_string()))
}

/// A port value: a plain or quoted run of digits, 1 to 65535, with an optional
/// ` # comment` after it.
fn port_value(value: &str) -> Option<u16> {
    let v = match value.find(" #") {
        Some(i) => value[..i].trim_end(),
        None => value,
    };
    let v = v
        .strip_prefix('"')
        .and_then(|s| s.strip_suffix('"'))
        .or_else(|| v.strip_prefix('\'').and_then(|s| s.strip_suffix('\'')))
        .unwrap_or(v);
    if v.is_empty() || !v.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    v.parse::<u16>().ok().filter(|p| *p != 0)
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
    fn the_scan_places_only_the_entrys_own_field() {
        let text = "\
servers:
  a:
    host: a.example.test
    nested:
      ssh_port: 2222
  b:
    host: b.example.test
    ssh_port: 2201
    ssh_port: 2202
  c:
    host: c.example.test
    ssh_port: ~
  d:
    host: d.example.test
    ssh_port: '2203'
other:
  e:
    ssh_port: 2204
";
        let ports = explicit_ssh_ports(text);
        assert_eq!(ports.get("a"), None, "a deeper key is not the entry's");
        assert_eq!(ports.get("b"), None, "named twice");
        assert_eq!(ports.get("c"), None, "a null");
        assert_eq!(ports.get("d"), Some(&2203));
        assert_eq!(ports.get("e"), None, "not under servers:");
    }

    /// Where the two readers disagree on the value, the port is 0.
    #[test]
    fn a_port_the_readers_disagree_on_is_none() {
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
