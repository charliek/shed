//! Host discovery: parse `~/.shed/config.yaml` — the multi-host server list that
//! shed and shed-remote-agent both read. A faithful Rust port of the Swift
//! `ShedConfig` (`Sources/ShedKit/Models/ShedConfig.swift`): a deliberately tiny
//! indentation-aware reader scoped to the machine-generated shape
//! (`servers: {NAME: {host, http_port, ssh_port, control_token, api_url,
//! tls_cert_fingerprint, auth_mode, client_cert_file, client_key_file,
//! client_cert_expires_at}}` + `default_server`), so neither client takes on a
//! YAML dependency. The GTK client (M2) reads secure/token-gated Linux hosts through
//! this, so it carries **all** per-server fields — not just host/ports.
//!
//! A cross-language parity test (`fixtures/config_sample.yaml`) pins this parser
//! byte-for-byte against the Swift one — the config analog of Phase 1's
//! golden-JSON backbone — until the Swift parser is retired (the two coexist for
//! now; see `docs/enhancements.md`).

/// One server entry from `~/.shed/config.yaml`.
///
/// `Default` is derived for construction convenience in tests and callers that
/// build a partial entry (`..Default::default()`); it is NOT the parser's default
/// entry — [`ShedConfig::parse`] applies the real per-field fallbacks (host =
/// server name, http_port 8080, ssh_port 22).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ShedServerEntry {
    pub name: String,
    pub host: String,
    pub http_port: u16,
    pub ssh_port: u16,
    /// Control-scoped bearer token; empty when the server isn't token-gated.
    pub control_token: String,
    /// HTTPS control-plane URL (`api_url`); overrides host+http_port when set.
    pub api_url: String,
    /// Pinned TLS cert fingerprint (`sha256:<hex>`, lowercased); empty for plain HTTP.
    pub tls_cert_fingerprint: String,
    /// The credential shape the server issued at the last bootstrap: `"token"` or
    /// `"mtls"` (Go `config.ServerEntry.AuthMode`).
    ///
    /// **ABSENT MEANS TOKEN.** Every entry written before client-certificate
    /// support has no `auth_mode` key and every one of those is a token/open
    /// server, so an empty value must never be read as "unknown, go find out". It
    /// is a CACHE of what the server last said, not a setting — a bootstrap that
    /// comes back in the other mode rewrites it, which is what lets an operator
    /// flip a server's `auth.mode` without re-adding every client. Kept as the raw
    /// string (rather than an enum) because this parser's job is to reproduce the
    /// file; interpretation is [`crate::token::AuthMode::from_wire`]'s.
    pub auth_mode: String,
    /// Path to this entry's client certificate (mtls only). A PATH, not inline
    /// PEM: the key beside it must live in a 0600 file of its own, not inside a
    /// config users hand-edit, copy between machines, and paste into issues.
    pub client_cert_file: String,
    /// Path to the private key matching [`Self::client_cert_file`].
    pub client_key_file: String,
    /// RFC3339 expiry of the certificate, cached so the proactive re-enrollment
    /// check costs no file read. The certificate itself remains the authority.
    pub client_cert_expires_at: String,
}

/// The resolved control-plane endpoint + TLS pin for a server entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedEndpoint {
    pub base_url: String,
    pub pin: String,
}

impl ShedServerEntry {
    /// The credential shape this entry last saw the server issue (absent ⇒
    /// [`crate::token::AuthMode::Token`]).
    pub fn auth_mode(&self) -> crate::token::AuthMode {
        crate::token::AuthMode::from_wire(Some(&self.auth_mode))
    }

    /// Is this entry's control plane HTTPS — which, for a shed-server, means the
    /// server enforces auth (token or mtls)? Open mode serves plain HTTP and has
    /// no HTTPS listener at all, so this is the one durable signal in a stored
    /// entry that separates "wants a credential" from "wants nothing".
    ///
    /// Either marker alone is enough (Go parity, `ServerEntry.UsesTLS`): an entry
    /// is user-editable, and a half-filled one still points at a listener that
    /// will demand a credential.
    pub fn uses_tls(&self) -> bool {
        !self.tls_cert_fingerprint.is_empty() || self.api_url.to_lowercase().starts_with("https://")
    }

    /// Does this entry hold NOTHING it could present to a server that demands a
    /// credential, while having enough information (host + ssh port) to obtain one
    /// over the `_bootstrap` SSH channel?
    ///
    /// The Go rule verbatim (`ServerEntry.NeedsEnrollment`, fix `4553cc8`), because
    /// "no credential recorded" is ambiguous in a stored entry and the two readings
    /// need opposite handling:
    ///
    ///   * an OPEN server legitimately has none, and must never pay an SSH
    ///     round-trip to discover that — it is plain HTTP, so `uses_tls` is false;
    ///   * a SECURE server's entry that LOST its credential fields must enroll.
    ///     That is routine during an upgrade, not a corner case: a pre-mtls client
    ///     that loads and re-saves `config.yaml` silently drops every key its own
    ///     struct does not know (`auth_mode`, `client_cert_file`,
    ///     `client_key_file`, `client_cert_expires_at`), leaving exactly this
    ///     shape. Without enrollment such an entry fails FOREVER: the client holds
    ///     no credential to be rejected, so it never reaches its reactive re-mint.
    ///
    /// A legacy static token (a `control_token` with no expiry) is NOT this case —
    /// it has something to present and is deliberately never re-minted.
    pub fn needs_enrollment(&self) -> bool {
        if !self.uses_tls() || self.host.is_empty() || self.ssh_port == 0 {
            return false;
        }
        match self.auth_mode() {
            crate::token::AuthMode::Mtls => {
                self.client_cert_file.is_empty() || self.client_key_file.is_empty()
            }
            crate::token::AuthMode::Token => self.control_token.is_empty(),
        }
    }

    /// Resolve the control-plane endpoint the client should dial: the https
    /// `api_url` (with its pinned cert) when it's a valid http(s) URL with a
    /// host, else plain `http://host:http_port`. Mirrors Swift's
    /// `resolvedEndpoint()` — the resolution whose absence once dialed the dead
    /// `:8080` instead of the secure `:8443`.
    pub fn resolved_endpoint(&self) -> ResolvedEndpoint {
        if !self.api_url.is_empty() && is_http_url_with_host(&self.api_url) {
            ResolvedEndpoint {
                base_url: self.api_url.clone(),
                pin: self.tls_cert_fingerprint.clone(),
            }
        } else {
            ResolvedEndpoint {
                base_url: format!("http://{}:{}", self.host, self.http_port),
                pin: self.tls_cert_fingerprint.clone(),
            }
        }
    }
}

/// True iff `s` is an `http://`/`https://` URL with a non-empty, whitespace-free
/// host — the observable subset of Swift's `URL(string:)` + scheme + host check
/// that `resolvedEndpoint` relies on (a relative path, a non-http scheme, an
/// empty host, or an unparseable string all fail, matching Swift's fallback).
fn is_http_url_with_host(s: &str) -> bool {
    let lower = s.to_lowercase();
    let rest = match lower
        .strip_prefix("http://")
        .or_else(|| lower.strip_prefix("https://"))
    {
        Some(r) => r,
        None => return false,
    };
    let host = rest.split(['/', ':', '?', '#']).next().unwrap_or("");
    !host.is_empty() && !host.contains(char::is_whitespace)
}

/// The parsed `~/.shed/config.yaml`: server entries (sorted by name) + the
/// default server.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ShedConfig {
    pub servers: Vec<ShedServerEntry>,
    pub default_server: Option<String>,
}

impl ShedConfig {
    /// Load + parse the config at `path`. A missing/unreadable file → an empty
    /// config (a degraded but non-fatal state the dashboard surfaces, never an
    /// error) — mirrors Swift's `load`.
    pub fn load(path: &str) -> Self {
        match std::fs::read_to_string(path) {
            Ok(text) => Self::parse(&text),
            Err(_) => Self::default(),
        }
    }

    pub fn parse(text: &str) -> Self {
        let Node::Map(top) = yaml_lite::parse(text) else {
            return Self::default();
        };
        let mut servers = Vec::new();
        if let Some(Node::Map(server_map)) = top.get("servers") {
            for (name, value) in server_map {
                let Node::Map(fields) = value else { continue };
                let scalar = |k: &str| fields.get(k).and_then(Node::as_scalar);
                servers.push(ShedServerEntry {
                    name: name.clone(),
                    host: scalar("host").unwrap_or(name).to_string(),
                    http_port: scalar("http_port")
                        .and_then(|s| s.parse().ok())
                        .unwrap_or(8080),
                    ssh_port: scalar("ssh_port")
                        .and_then(|s| s.parse().ok())
                        .unwrap_or(22),
                    control_token: scalar("control_token").unwrap_or("").to_string(),
                    api_url: scalar("api_url").unwrap_or("").to_string(),
                    // Canonicalize to the lowercase "sha256:<hex>" the server emits,
                    // so a hand-edited upper/mixed-case pin still matches at
                    // handshake time rather than silently failing every connection.
                    tls_cert_fingerprint: scalar("tls_cert_fingerprint")
                        .unwrap_or("")
                        .to_lowercase(),
                    // Lower-cased like the pin: the writer always emits the
                    // canonical literal, and a hand-edited `MTLS` should mean what
                    // it obviously means rather than silently falling back to
                    // token mode.
                    auth_mode: scalar("auth_mode").unwrap_or("").to_lowercase(),
                    client_cert_file: scalar("client_cert_file").unwrap_or("").to_string(),
                    client_key_file: scalar("client_key_file").unwrap_or("").to_string(),
                    client_cert_expires_at: scalar("client_cert_expires_at")
                        .unwrap_or("")
                        .to_string(),
                });
            }
        }
        servers.sort_by(|a, b| a.name.cmp(&b.name));
        ShedConfig {
            servers,
            default_server: top
                .get("default_server")
                .and_then(Node::as_scalar)
                .map(str::to_string),
        }
    }
}

/// A deliberately tiny indentation-based reader. Handles exactly what
/// `~/.shed/config.yaml` contains: nested maps and scalar leaves. Inline `{}` is
/// an empty map; comments (`#`) and blank lines are skipped. A faithful port of
/// Swift's `YAMLLite`.
mod yaml_lite {
    use std::collections::HashMap;

    #[derive(Debug, Clone, PartialEq, Eq)]
    pub enum Node {
        Map(HashMap<String, Node>),
        Scalar(String),
    }

    impl Node {
        pub fn as_scalar(&self) -> Option<&str> {
            match self {
                Node::Scalar(s) => Some(s),
                Node::Map(_) => None,
            }
        }
    }

    struct Line {
        indent: usize,
        key: String,
        value: Option<String>, // None → a nested block follows
    }

    pub fn parse(text: &str) -> Node {
        let lines: Vec<Line> = text
            .split('\n')
            .filter_map(|raw| {
                let trimmed = raw.trim();
                if trimmed.is_empty() || trimmed.starts_with('#') {
                    return None;
                }
                let indent = raw.len() - raw.trim_start_matches(' ').len();
                let colon = raw.find(':')?;
                let key = raw[..colon].trim();
                let mut rest = raw[colon + 1..].trim().to_string();
                // Strip an inline comment after a scalar value.
                if let Some(hash) = rest.find('#') {
                    rest = rest[..hash].trim().to_string();
                }
                let value = if rest.is_empty() || rest == "{}" {
                    None
                } else {
                    Some(unquote(&rest))
                };
                Some(Line {
                    indent,
                    key: unquote(key),
                    value,
                })
            })
            .collect();
        let mut index = 0;
        build(&lines, &mut index, -1)
    }

    fn build(lines: &[Line], index: &mut usize, parent_indent: isize) -> Node {
        let mut map = HashMap::new();
        if *index >= lines.len() {
            return Node::Map(map);
        }
        let child_indent = lines[*index].indent as isize;
        while *index < lines.len() {
            let indent = lines[*index].indent as isize;
            if indent <= parent_indent {
                break;
            }
            if indent != child_indent {
                // A line deeper than expected without a parent (defensive skip).
                *index += 1;
                continue;
            }
            let key = lines[*index].key.clone();
            match lines[*index].value.clone() {
                Some(value) => {
                    map.insert(key, Node::Scalar(value));
                    *index += 1;
                }
                None => {
                    *index += 1;
                    let child = build(lines, index, child_indent);
                    map.insert(key, child);
                }
            }
        }
        Node::Map(map)
    }

    fn unquote(s: &str) -> String {
        if s.len() >= 2
            && ((s.starts_with('"') && s.ends_with('"'))
                || (s.starts_with('\'') && s.ends_with('\'')))
        {
            s[1..s.len() - 1].to_string()
        } else {
            s.to_string()
        }
    }
}

use yaml_lite::Node;

#[cfg(test)]
mod tests {
    use super::*;

    fn by_name<'a>(config: &'a ShedConfig, name: &str) -> &'a ShedServerEntry {
        config
            .servers
            .iter()
            .find(|s| s.name == name)
            .unwrap_or_else(|| panic!("no server {name}"))
    }

    #[test]
    fn parses_servers_and_default() {
        // Mirrors Swift's testConfigParsesServersAndDefault.
        let yaml = "\
servers:
    mini2:
        host: mini2
        http_port: 8080
        ssh_port: 2222
        control_token: shed_control_abc123
        added_at: 2026-05-09T01:44:44.395385-05:00
    my-server:
        host: localhost
        http_port: 8080
        ssh_port: 2222
default_server: mini2
sheds: {}
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(config.servers.len(), 2);
        assert_eq!(config.default_server.as_deref(), Some("mini2"));
        let mini2 = by_name(&config, "mini2");
        assert_eq!(mini2.host, "mini2");
        assert_eq!(mini2.http_port, 8080);
        assert_eq!(mini2.ssh_port, 2222);
        assert_eq!(mini2.control_token, "shed_control_abc123");
        // An entry without a token parses to an empty control_token.
        assert_eq!(by_name(&config, "my-server").control_token, "");
    }

    #[test]
    fn missing_file_is_empty_not_error() {
        assert_eq!(
            ShedConfig::load("/nonexistent/does-not-exist.yaml"),
            ShedConfig::default()
        );
    }

    #[test]
    fn empty_or_absent_servers_parse_to_zero_servers() {
        // A present-but-empty `servers`, an entirely-absent one, and an empty
        // document all yield an empty server list — a degraded-but-valid config
        // (the dashboard surfaces it), never a parse failure.
        assert!(ShedConfig::parse("servers: []\n").servers.is_empty());
        assert!(ShedConfig::parse("default_server: mini2\nsheds: {}\n")
            .servers
            .is_empty());
        assert!(ShedConfig::parse("").servers.is_empty());
    }

    #[test]
    fn default_server_absent_from_servers_still_parses() {
        // Validation ("does the default exist?") is deferred to the client, not the
        // parser: an unknown default_server parses fine and is surfaced verbatim.
        let yaml = "\
servers:
    mini2:
        host: mini2
        http_port: 8080
default_server: ghost
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(config.servers.len(), 1);
        assert_eq!(config.default_server.as_deref(), Some("ghost"));
    }

    #[test]
    fn https_api_url_keeps_pin() {
        let e = ShedServerEntry {
            name: "localmac".into(),
            host: "localhost".into(),
            http_port: 8080,
            ssh_port: 2222,
            api_url: "https://localhost:8443".into(),
            tls_cert_fingerprint: "sha256:abc".into(),
            ..Default::default()
        };
        let r = e.resolved_endpoint();
        assert_eq!(r.base_url, "https://localhost:8443");
        assert_eq!(r.pin, "sha256:abc");
    }

    #[test]
    fn plain_http_fallback_when_no_api_url() {
        let e = ShedServerEntry {
            name: "mini2".into(),
            host: "mini2".into(),
            http_port: 8080,
            ssh_port: 2222,
            ..Default::default()
        };
        let r = e.resolved_endpoint();
        assert_eq!(r.base_url, "http://mini2:8080");
        assert_eq!(r.pin, "");
    }

    #[test]
    fn malformed_api_url_falls_back_to_plain_http() {
        // Relative path, wrong scheme, no host, unparseable, host-with-space —
        // all fall back to the plain endpoint (matches Swift's URL(string:) checks).
        for bad in [
            "/api/info",
            "ftp://example.com:21",
            "https://",
            "not a url",
            "http://exa mple.com",
        ] {
            let e = ShedServerEntry {
                name: "s".into(),
                host: "h".into(),
                http_port: 8080,
                ssh_port: 2222,
                api_url: bad.into(),
                tls_cert_fingerprint: "sha256:x".into(),
                ..Default::default()
            };
            assert_eq!(
                e.resolved_endpoint().base_url,
                "http://h:8080",
                "api_url {bad:?} should fall back to the plain endpoint"
            );
        }
    }

    /// Cross-language parity backbone: the Swift `ShedConfig` parser asserts the
    /// SAME expected values against this SAME fixture (see
    /// Tests/ShedKitTests/ConfigParityTests.swift). Keep the two in lockstep.
    #[test]
    fn parity_fixture_matches_expected() {
        let config = ShedConfig::parse(include_str!("../../fixtures/config_sample.yaml"));
        assert_eq!(config.default_server.as_deref(), Some("mini2"));
        // Sorted by name (byte-wise: '2' (0x32) < 'm' (0x6d), so mini2 < minimal).
        let names: Vec<&str> = config.servers.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(names, ["mini2", "minimal", "secure"]);

        let mini2 = by_name(&config, "mini2");
        assert_eq!(mini2.host, "mini2");
        assert_eq!(mini2.http_port, 8080);
        assert_eq!(mini2.ssh_port, 2222);
        assert_eq!(mini2.control_token, "shed_control_abc123");
        assert_eq!(mini2.api_url, "");
        assert_eq!(mini2.tls_cert_fingerprint, "");
        assert_eq!(mini2.resolved_endpoint().base_url, "http://mini2:8080");
        assert_eq!(mini2.resolved_endpoint().pin, "");

        let secure = by_name(&config, "secure");
        assert_eq!(secure.host, "localhost");
        assert_eq!(secure.control_token, "");
        assert_eq!(secure.api_url, "https://localhost:8443");
        // Mixed-case pin lowercased.
        assert_eq!(secure.tls_cert_fingerprint, "sha256:aabbcc");
        assert_eq!(
            secure.resolved_endpoint().base_url,
            "https://localhost:8443"
        );
        assert_eq!(secure.resolved_endpoint().pin, "sha256:aabbcc");

        // `minimal: {}` → all defaults, host defaults to the server name, ssh_port 22.
        let minimal = by_name(&config, "minimal");
        assert_eq!(minimal.host, "minimal");
        assert_eq!(minimal.http_port, 8080);
        assert_eq!(minimal.ssh_port, 22);
        assert_eq!(minimal.resolved_endpoint().base_url, "http://minimal:8080");
    }

    /// The mtls half of the same fixture (plan 001 D6). Swift does not model
    /// these keys yet and ignores them, which is why adding them to the shared
    /// fixture leaves `ConfigParityTests` passing unchanged.
    #[test]
    fn parity_fixture_carries_the_mtls_entry_fields() {
        use crate::token::AuthMode;
        let config = ShedConfig::parse(include_str!("../../fixtures/config_sample.yaml"));

        let secure = by_name(&config, "secure");
        assert_eq!(secure.auth_mode, "mtls");
        assert_eq!(secure.auth_mode(), AuthMode::Mtls);
        assert_eq!(
            secure.client_cert_file,
            "/Users/dev/.shed/creds/secure/client.crt"
        );
        assert_eq!(
            secure.client_key_file,
            "/Users/dev/.shed/creds/secure/client.key"
        );
        assert_eq!(secure.client_cert_expires_at, "2026-07-26T12:00:00Z");
        assert!(secure.uses_tls());
        // It holds both credential files, so nothing to enroll.
        assert!(!secure.needs_enrollment());

        // ABSENT auth_mode is token, not "unknown" — the legacy-entry rule.
        let mini2 = by_name(&config, "mini2");
        assert_eq!(mini2.auth_mode, "");
        assert_eq!(mini2.auth_mode(), AuthMode::Token);
        assert!(!mini2.uses_tls()); // plain http entry → open server, never enrolls
        assert!(!mini2.needs_enrollment());
    }

    /// The "no usable credential ⇒ enroll" discrimination (Go `4553cc8` parity).
    #[test]
    fn needs_enrollment_discriminates_open_from_stripped_secure_entries() {
        let base = ShedServerEntry {
            name: "s".into(),
            host: "h".into(),
            http_port: 8080,
            ssh_port: 2222,
            control_token: String::new(),
            api_url: "https://h:8443".into(),
            tls_cert_fingerprint: "sha256:aa".into(),
            auth_mode: String::new(),
            client_cert_file: String::new(),
            client_key_file: String::new(),
            client_cert_expires_at: String::new(),
        };

        // (a) an https entry stripped of every credential field by an older
        // client — the reproduced-live case — enrolls.
        assert!(base.needs_enrollment());

        // (b) the same entry with an mtls mode but no files still enrolls...
        let mtls_stripped = ShedServerEntry {
            auth_mode: "mtls".into(),
            ..base.clone()
        };
        assert!(mtls_stripped.needs_enrollment());

        // ...and does not once both files are present.
        let mtls_ready = ShedServerEntry {
            client_cert_file: "/creds/c.crt".into(),
            client_key_file: "/creds/c.key".into(),
            ..mtls_stripped.clone()
        };
        assert!(!mtls_ready.needs_enrollment());
        // A half-written pair is as unusable as none.
        assert!(ShedServerEntry {
            client_key_file: String::new(),
            ..mtls_ready.clone()
        }
        .needs_enrollment());

        // (c) a token entry that still holds its token does not enroll.
        assert!(!ShedServerEntry {
            control_token: "shed_control_x".into(),
            ..base.clone()
        }
        .needs_enrollment());

        // (d) an OPEN server (plain http, no pin) never enrolls, credential or
        // not — it must not pay an SSH round-trip to discover it needs nothing.
        assert!(!ShedServerEntry {
            api_url: String::new(),
            tls_cert_fingerprint: String::new(),
            ..base.clone()
        }
        .needs_enrollment());

        // (e) no way to reach SSH → nothing to enroll WITH.
        assert!(!ShedServerEntry {
            ssh_port: 0,
            ..base.clone()
        }
        .needs_enrollment());
        assert!(!ShedServerEntry {
            host: String::new(),
            ..base.clone()
        }
        .needs_enrollment());
    }

    #[test]
    fn auth_mode_is_lowercased_and_unknown_values_read_as_token() {
        use crate::token::AuthMode;
        let yaml = "\
servers:
    a:
        host: a
        auth_mode: MTLS
    b:
        host: b
        auth_mode: something-new
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(by_name(&config, "a").auth_mode(), AuthMode::Mtls);
        // Forward-compat: an unrecognized mode is the shape whose fields are
        // actually populated (Go `sdk.Bundle.Mode` parity), never mtls.
        assert_eq!(by_name(&config, "b").auth_mode(), AuthMode::Token);
    }
}
