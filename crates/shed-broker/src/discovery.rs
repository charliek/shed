//! Multi-server discovery — the always-on server-model core the host-agent's bus,
//! supervisor, and control-token provider all share.
//!
//! This module is the Rust home for the pieces of Go's `discovery.go` /
//! `config.go` that are NOT desktop-specific: [`ServerTarget`] (a resolved shed
//! server the agent brokers for), its [`ServerTarget::is_secure`] https-scheme
//! signal, and [`load_discovered_servers`] (the `~/.shed/config.yaml` reader). They
//! were hoisted here out of `minter.rs`/`controltoken.rs` (both behind
//! `desktop-forwarding`) so the always-on discovery/supervisor path can use them
//! headless — this module's `resolve_targets` consumes them in single- AND
//! multi-server mode, and the (later) supervisor slice reconciles over them. The
//! selector + `DiscoveryConfig` parse that drives the filtering lives in `config.rs`.

use crate::config::yaml_lite::{self, Node};
use crate::config::HostAgentConfig;

/// shed's default server HTTP port (mirror `discovery.go:defaultShedHTTPPort`).
const DEFAULT_SHED_HTTP_PORT: u16 = 8080;

/// A resolved shed server this agent brokers for (mirror `config.go:ServerTarget`).
/// `name` is the identity key (empty in single-server mode); `url` is the broker base
/// URL. `ssh_host`/`ssh_port` are the SSH endpoint used to mint over `_bootstrap`;
/// `ssh_port == 0` means the entry omitted it (the agent can't self-mint).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServerTarget {
    pub name: String,
    pub url: String,
    /// The credentials-scoped bearer token from config (empty when not token-gated). In
    /// secure mode the agent mints this itself; carried for the (deferred) bus provider.
    pub token: String,
    pub tls_fingerprint: String,
    pub ssh_host: String,
    pub ssh_port: u16,
}

impl ServerTarget {
    /// Whether the server is reached over https — the authoritative local signal that
    /// it runs in secure mode (tokens ⟺ TLS ⟺ secure) and that the agent should
    /// self-mint (mirror `config.go:IsSecure`: scheme, not fingerprint presence).
    /// Consumed by the always-on [`crate::supervisor::should_mint`] in BOTH feature configs.
    pub fn is_secure(&self) -> bool {
        self.url.to_lowercase().starts_with("https://")
    }
}

/// Read the shed CLI config at `path` and return one [`ServerTarget`] per registered
/// server, sorted by name (mirror `discovery.go:LoadDiscoveredServers`). A missing file
/// is not an error — it yields an empty slice so the agent picks servers up live once the
/// file appears. Entries with an empty host are skipped. **`ssh_port` defaults to 0 when
/// omitted (NOT 22)** — the divergence from `shed-core::ShedConfig` that makes an https
/// server missing `ssh_port` reject as "no ssh endpoint" rather than silently minting
/// against port 22.
///
/// Malformed YAML is an error, matching Go's `LoadDiscoveredServers` — the `yaml_lite`
/// reader is backed by `saphyr-parser`, so a malformed `~/.shed/config.yaml` returns
/// `Err`. Go uses `parsing shed config %s` for a YAML parse failure (`discovery.go:73`)
/// and `reading shed config %s` for a file-read failure (`discovery.go:68`); this mirrors
/// both.
pub fn load_discovered_servers(path: &str) -> Result<Vec<ServerTarget>, String> {
    let data = match std::fs::read_to_string(path) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(format!("reading shed config {path}: {e}")),
    };

    let root = yaml_lite::parse(&data).map_err(|e| format!("parsing shed config {path}: {e}"))?;
    let mut targets = Vec::new();
    if let Some(servers) = root
        .as_map()
        .and_then(|m| m.get("servers"))
        .and_then(Node::as_map)
    {
        for (name, entry) in servers {
            let Some(fields) = entry.as_map() else {
                continue;
            };
            let scalar = |k: &str| fields.get(k).and_then(Node::as_scalar);
            let host = scalar("host").unwrap_or("");
            if host.is_empty() {
                continue;
            }
            let http_port: u16 = scalar("http_port")
                .and_then(|s| s.parse().ok())
                .unwrap_or(DEFAULT_SHED_HTTP_PORT);
            // Prefer the explicit api_url (carries the https scheme + port) over the
            // legacy host+http_port plain-HTTP form.
            let url = match scalar("api_url") {
                Some(u) if !u.is_empty() => u.to_string(),
                _ => format!("http://{host}:{http_port}"),
            };
            targets.push(ServerTarget {
                name: name.clone(),
                url,
                token: scalar("credentials_token").unwrap_or("").to_string(),
                tls_fingerprint: scalar("tls_cert_fingerprint").unwrap_or("").to_string(),
                ssh_host: host.to_string(),
                // ssh_port omitted → 0 (NOT 22): a server without it can't self-mint.
                ssh_port: scalar("ssh_port").and_then(|s| s.parse().ok()).unwrap_or(0),
            });
        }
    }
    targets.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(targets)
}

// The `resolve_targets` methods live here (rather than on `HostAgentConfig` in
// `config.rs`) because they depend on this module's `ServerTarget` /
// `load_discovered_servers`. `config.rs` stays self-contained (only std + serde +
// saphyr-parser) as a matter of hygiene — it is the crate's foundational module and
// carries no dependency on the rest of the broker.
impl HostAgentConfig {
    /// The desired server set to broker for, filtered from an already-loaded
    /// `discovered` list — a faithful port of Go's `Config.ResolveTargets`
    /// (`config.go:162`). Single-server mode (no `discovery:` block) returns ONE unnamed
    /// target `{name:"", url: server}`, ignoring `discovered` (the empty name keeps
    /// `server` out of audit logs / desktop events and scopes per-shed state under
    /// `/<shed>`). Discovery mode filters `discovered` by the selector and dedups by name
    /// (first occurrence wins). Pure (no I/O); [`resolve_targets`](Self::resolve_targets)
    /// is the I/O wrapper the daemon reconcile loop calls.
    pub(crate) fn resolve_targets_from(&self, discovered: Vec<ServerTarget>) -> Vec<ServerTarget> {
        let Some(dc) = self.discovery.as_ref() else {
            return vec![ServerTarget {
                name: String::new(),
                url: self.server.clone(),
                token: String::new(),
                tls_fingerprint: String::new(),
                ssh_host: String::new(),
                ssh_port: 0,
            }];
        };
        let mut out = Vec::with_capacity(discovered.len());
        let mut seen = std::collections::HashSet::new();
        for t in discovered {
            // `seen.insert` returns false when the name is already present → skip the
            // duplicate (dedup by name, first wins), matching Go's `seen` map.
            if dc.servers.selected(&t.name) && seen.insert(t.name.clone()) {
                out.push(t);
            }
        }
        out
    }

    /// Compute the desired server set, reading + filtering the discovery source — a
    /// faithful port of Go's free `resolveTargets(cfg)` (`discovery.go:45`), the shared
    /// entry the daemon reconcile loop and the `status` subcommand both call. Discovery
    /// mode loads `discovery.source` then filters; single-server mode returns the unnamed
    /// target. `Err` only on a discovery read/parse failure (the caller keeps its current
    /// servers). Called by `main.rs`'s reconcile closure (and shared with `status`).
    /// `pub` so the daemon bin (and a future embedder's reconcile) drives it cross-crate.
    pub fn resolve_targets(&self) -> Result<Vec<ServerTarget>, String> {
        let discovered = match self.discovery.as_ref() {
            Some(dc) => load_discovered_servers(&dc.source)?,
            None => Vec::new(),
        };
        Ok(self.resolve_targets_from(discovered))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write_config(content: &str) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        std::fs::write(&path, content).unwrap();
        (dir, path.to_string_lossy().into_owned())
    }

    #[test]
    fn load_missing_file_is_empty() {
        assert_eq!(
            load_discovered_servers("/nonexistent/config.yaml").unwrap(),
            Vec::new()
        );
    }

    /// A malformed `~/.shed/config.yaml` is an error (mirrors Go's
    /// `LoadDiscoveredServers`; the saphyr-backed reader detects it where the old
    /// line/colon reader was silently permissive). The error carries Go's inner
    /// `parsing shed config` prefix (`resolve` in `controltoken.rs` wraps it with the
    /// outer `reading server config:` before it reaches the `token.get` response).
    #[test]
    fn load_discovered_servers_errors_on_malformed() {
        let (_d, cfg) = write_config("{{invalid yaml");
        let err = load_discovered_servers(&cfg).expect_err("malformed config rejected");
        assert!(err.contains("parsing shed config"), "err = {err}");
    }

    /// The Rust half of the `load_discovered_servers` golden — reads the SAME shared
    /// fixture the Go runner reads (`cmd/shed-host-agent/golden_test.go`), so the two
    /// impls can't drift together on the ssh_port=0/empty-host/sort semantics. Lives
    /// in-crate (not `tests/golden.rs`) because this is a binary crate with no lib, so
    /// an integration test can't reach `load_discovered_servers`.
    #[test]
    fn golden_load_discovered_servers() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/load_discovered_servers.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let (_d, cfg) = write_config(v["config_yaml"].as_str().unwrap());
            let got = load_discovered_servers(&cfg).unwrap();
            let got_json: Vec<serde_json::Value> = got
                .iter()
                .map(|t| {
                    serde_json::json!({
                        "name": t.name,
                        "url": t.url,
                        "token": t.token,
                        "tls_fingerprint": t.tls_fingerprint,
                        "ssh_host": t.ssh_host,
                        "ssh_port": t.ssh_port,
                    })
                })
                .collect();
            assert_eq!(
                serde_json::Value::from(got_json),
                v["expected"],
                "golden vector {name:?}"
            );
        }
    }

    #[test]
    fn load_defaults_and_skips_empty_host() {
        let (_d, cfg) = write_config(
            "\
servers:
  https-with-ssh:
    api_url: https://a.example:8443
    host: a.example
    ssh_port: 2222
  https-no-ssh:
    api_url: https://b.example:8443
    host: b.example
  plain:
    host: c.example
  empty-host:
    ssh_port: 2222
",
        );
        let got = load_discovered_servers(&cfg).unwrap();
        // Sorted by name; empty-host entry skipped.
        let names: Vec<&str> = got.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, ["https-no-ssh", "https-with-ssh", "plain"]);

        let with_ssh = &got[1];
        assert_eq!(with_ssh.ssh_port, 2222);
        assert!(with_ssh.is_secure());

        // The divergence: an https server missing ssh_port → 0 (NOT 22).
        let no_ssh = &got[0];
        assert_eq!(no_ssh.ssh_port, 0);
        assert!(no_ssh.is_secure());

        // Plain http fallback URL + http_port default 8080.
        let plain = &got[2];
        assert_eq!(plain.url, "http://c.example:8080");
        assert!(!plain.is_secure());
    }

    /// Mirrors `config_discovery_test.go:TestResolveTargets` (the pure
    /// `Config.ResolveTargets` path): legacy single-server → one unnamed target from
    /// `server`, ignoring the discovered list; discovery → filter by the selector +
    /// dedup by name (first wins); an explicit empty list selects nothing. Lives here
    /// (not `config.rs`) because it builds `ServerTarget` + calls `resolve_targets_from`,
    /// both defined in this module.
    #[test]
    fn resolve_targets_single_and_filtered() {
        let mk = |name: &str, url: &str| ServerTarget {
            name: name.into(),
            url: url.into(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: String::new(),
            ssh_port: 0,
        };
        let discovered = vec![
            mk("mini2", "http://mini2:8080"),
            mk("mini3", "http://mini3:8080"),
        ];

        // Legacy single-server: one unnamed target, `discovered` ignored.
        let cfg = HostAgentConfig::parse("server: http://localhost:8080\n");
        assert_eq!(
            cfg.resolve_targets_from(discovered.clone()),
            vec![mk("", "http://localhost:8080")]
        );

        // Discover all.
        let cfg = HostAgentConfig::parse("discovery:\n  servers: all\n");
        assert_eq!(cfg.resolve_targets_from(discovered.clone()), discovered);

        // Include a subset.
        let cfg = HostAgentConfig::parse("discovery:\n  servers: [mini3]\n");
        assert_eq!(
            cfg.resolve_targets_from(discovered.clone()),
            vec![mk("mini3", "http://mini3:8080")]
        );

        // Dedup by name (first occurrence wins).
        let dup = vec![
            mk("mini2", "http://mini2:8080"),
            mk("mini2", "http://other:8080"),
        ];
        let cfg = HostAgentConfig::parse("discovery:\n  servers: all\n");
        let got = cfg.resolve_targets_from(dup);
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].url, "http://mini2:8080");

        // Explicit empty list (`servers: []`) selects nothing.
        let cfg = HostAgentConfig::parse("discovery:\n  servers: []\n");
        assert!(cfg.resolve_targets_from(discovered).is_empty());
    }

    /// The I/O `resolve_targets` (Go's free `resolveTargets(cfg)`): legacy mode returns
    /// the unnamed target with no file read; discovery mode reads + filters the source.
    /// This is the commit-1 caller of the method the supervisor wires into `main.rs` at
    /// commit 3.
    #[test]
    fn resolve_targets_reads_source() {
        // Legacy: no discovery block → the unnamed single target, no I/O.
        let cfg = HostAgentConfig::parse("server: http://localhost:8080\n");
        let got = cfg.resolve_targets().expect("legacy resolves");
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].name, "");
        assert_eq!(got[0].url, "http://localhost:8080");

        // Discovery: source points at a written shed config; the selector filters it.
        let (_d, shed) = write_config(
            "servers:\n  mini2:\n    host: mini2\n    http_port: 8080\n  mini3:\n    host: mini3\n",
        );
        let cfg = HostAgentConfig::parse(&format!(
            "discovery:\n  servers: [mini3]\n  source: {shed}\n"
        ));
        let got = cfg.resolve_targets().expect("discovery resolves");
        assert_eq!(
            got.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(),
            ["mini3"]
        );
    }
}
