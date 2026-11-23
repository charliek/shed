//! Multi-server discovery — the always-on server-model core the broker's bus,
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
use crate::config::ExtensionsYaml;

/// shed's default server HTTP port (mirror `discovery.go:defaultShedHTTPPort`).
const DEFAULT_SHED_HTTP_PORT: u16 = 8080;

// `ServerTarget` (pure data) moved to `shed_source::target` (plan 039 §3.7); re-exported
// so every `crate::discovery::ServerTarget` path keeps resolving.
pub use shed_source::target::ServerTarget;

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

    // A byte-order mark at the start is skipped, as Go's reader skips it (saphyr would
    // read it as part of the first key, so `servers` would not be found).
    let data = data.strip_prefix('\u{feff}').unwrap_or(&data);
    // A raw line break the parser does not split on as Go's does refuses the whole file
    // (plan 037 A8), before the parser reads it.
    yaml_lite::refuse_unsupported_line_breaks(data)
        .map_err(|e| format!("parsing shed config {path}: {e}"))?;
    let (root, marks) =
        yaml_lite::parse_marked(data).map_err(|e| format!("parsing shed config {path}: {e}"))?;
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
            // A field this reader does not read exactly as YAML means it (a tag, an
            // anchor, a value over more than one line): `user` and `identity_file` fail
            // closed on it. An entry with a merge key (`<<`) could carry either in the
            // merged mapping this reader does not follow, and one with a tagged key
            // (`!!binary aWRlbnRpdHlfZmlsZQ==` is `identity_file` to Go, its base64 text
            // here) could name either under a key this reader does not see as theirs:
            // both fail closed whatever the entry says, as in shed-core's reader.
            let exact = |field: &str| {
                !marks.values.contains(
                    ["servers", name.as_str(), field]
                        .map(String::from)
                        .as_slice(),
                )
            };
            let merged = fields.contains_key("<<")
                || marks
                    .tagged_keys
                    .contains(["servers", name.as_str()].map(String::from).as_slice());
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
                // Absent → "" (NOT "token"): the Go mirror keeps "never recorded" and
                // "recorded as token" distinguishable, and the golden fixture pins it.
                auth_mode: scalar("auth_mode").unwrap_or("").to_string(),
                user: if merged {
                    shed_core::config::USER_NOT_A_STRING.to_string()
                } else {
                    target_user(fields.get("user"), exact("user"))
                },
                identity_file: if merged {
                    shed_core::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE.to_string()
                } else {
                    target_identity_file(fields.get("identity_file"), exact("identity_file"))
                },
            });
        }
    }
    targets.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(targets)
}

/// An entry's `identity_file` (the same rule as shed-core's reader): absent or null → `""`;
/// a scalar read exactly → verbatim (checked at every use); a list, a map, or a value not
/// read exactly (`exact` false: a tag, an anchor, a value over more than one line) →
/// [`shed_core::identityfile::IDENTITY_FILE_UNREADABLE`], which every check refuses.
fn target_identity_file(node: Option<&Node>, exact: bool) -> String {
    match node {
        None => String::new(),
        Some(Node::Null) if exact => String::new(),
        Some(Node::Scalar(s)) if exact => s.clone(),
        Some(_) => shed_core::identityfile::IDENTITY_FILE_UNREADABLE.to_string(),
    }
}

/// An entry's `user` as the sources hold it (the same rule as shed-core's reader): absent
/// or null → `""` (no user recorded); a scalar read exactly → verbatim (a value that is no
/// user name matches no certificate, which refuses); a list, a map, or a value not read
/// exactly (as for [`target_identity_file`]) → [`shed_core::config::USER_NOT_A_STRING`], so
/// the entry fails closed instead of reading as "no user", which would take the first named
/// user it is issued.
fn target_user(node: Option<&Node>, exact: bool) -> String {
    match node {
        None => String::new(),
        Some(Node::Null) if exact => String::new(),
        Some(Node::Scalar(s)) if exact => s.clone(),
        Some(_) => shed_core::config::USER_NOT_A_STRING.to_string(),
    }
}

// The `resolve_targets` methods live here (rather than on `ExtensionsYaml` in
// `config.rs`) because they depend on this module's `ServerTarget` /
// `load_discovered_servers`. `config.rs` stays self-contained (only std + serde +
// saphyr-parser) as a matter of hygiene — it is the crate's foundational module and
// carries no dependency on the rest of the broker.
impl ExtensionsYaml {
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
                // Single-server mode has no CLI config entry to learn a mode from; the
                // unnamed target is plain-http/open in practice, and an enrollment would
                // discover the mode from the bundle anyway.
                auth_mode: String::new(),
                user: String::new(),
                identity_file: String::new(),
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

    /// The `load_discovered_servers` golden — the shared fixture under
    /// `tests/broker-wire/fixtures/`, recorded while the Go daemon still read it too
    /// (retired in plan 006); this test is now its one reader. It pins the
    /// ssh_port=0/empty-host/sort semantics and the verbatim `auth_mode` and `user`.
    #[test]
    fn golden_load_discovered_servers() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/broker-wire/fixtures/load_discovered_servers.json");
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
                        "auth_mode": t.auth_mode,
                        "user": t.user,
                        "identity_file": t.identity_file,
                    })
                })
                .collect();
            // `auth_mode` is REQUIRED on every expected entry, not tolerated as absent:
            // a fixture that stopped carrying it would otherwise silently stop pinning
            // the "" vs "token" divergence this field exists to preserve, and the Go
            // runner (which unmarshals into a struct) would not notice either.
            for (i, want) in v["expected"].as_array().unwrap().iter().enumerate() {
                assert!(
                    want.get("auth_mode")
                        .and_then(serde_json::Value::as_str)
                        .is_some(),
                    "golden vector {name:?} entry {i} has no auth_mode key — the fixture \
                     must pin it (absent-in-config means \"\", never \"token\")"
                );
                // `user` is required the same way: an entry that stopped carrying it
                // would stop pinning that an absent user is "" (no user held).
                assert!(
                    want.get("user")
                        .and_then(serde_json::Value::as_str)
                        .is_some(),
                    "golden vector {name:?} entry {i} has no user key — the fixture must pin it"
                );
                // And `identity_file`: an absent one is "" (no key), and the fixture pins
                // that a value is carried verbatim.
                assert!(
                    want.get("identity_file")
                        .and_then(serde_json::Value::as_str)
                        .is_some(),
                    "golden vector {name:?} entry {i} has no identity_file key — the fixture \
                     must pin it"
                );
            }
            assert_eq!(
                serde_json::Value::from(got_json),
                v["expected"],
                "golden vector {name:?}"
            );
        }
    }

    /// `auth_mode` round-trips verbatim, and an entry that omits it yields the EMPTY
    /// string rather than a defaulted `"token"` (the Go mirror's rule). `is_mtls` is
    /// case-insensitive, matching Go's `strings.EqualFold`.
    #[test]
    fn load_carries_auth_mode_verbatim() {
        let (_d, cfg) = write_config(
            "\
servers:
  legacy:
    api_url: https://legacy.example:8443
    host: legacy.example
    ssh_port: 2222
  mtls:
    api_url: https://mtls.example:8443
    host: mtls.example
    ssh_port: 2222
    auth_mode: mtls
  shouty:
    api_url: https://shouty.example:8443
    host: shouty.example
    ssh_port: 2222
    auth_mode: MTLS
  tok:
    api_url: https://tok.example:8443
    host: tok.example
    ssh_port: 2222
    auth_mode: token
",
        );
        let got = load_discovered_servers(&cfg).unwrap();
        let by = |n: &str| got.iter().find(|t| t.name == n).unwrap().clone();

        // Absent → "" (NOT "token"), and "" is not mtls.
        assert_eq!(by("legacy").auth_mode, "");
        assert!(!by("legacy").is_mtls());

        assert_eq!(by("mtls").auth_mode, "mtls");
        assert!(by("mtls").is_mtls());

        // Case-insensitive predicate, but the stored value is verbatim.
        assert_eq!(by("shouty").auth_mode, "MTLS");
        assert!(by("shouty").is_mtls());

        assert_eq!(by("tok").auth_mode, "token");
        assert!(!by("tok").is_mtls());
    }

    /// What the broker's reader makes of one config, for one entry.
    #[derive(Debug, PartialEq)]
    enum BrokerReads {
        /// A target with this `identity_file`.
        Target(&'static str),
        /// No target for the entry (an empty host: no ssh, no mint for it).
        NoTarget,
        /// The whole file is an error: no target at all.
        FileError,
    }

    /// **Both Rust readers, on every shape of the A8 review's table** (plan 037 A8,
    /// `reviews/A8-round2a.md`), each answer pinned: shed-core's reader (the app),
    /// the broker's (the minter), and, as the third column, what Go's reader
    /// (gopkg.in/yaml.v3 into `config.ServerEntry`) reads. Where the two Rust readers
    /// differ, both are on the safe side: a path Go reads too, or a refusal. Neither is
    /// ever "no identity file" ("") where Go reads a path.
    ///
    /// shed-core reads a server entry only when every line in it is a plain
    /// `key: value` it reads exactly (its whitelist), so every other shape is the whole
    /// entry refused (`E`, its host empty too). The broker reads real YAML and refuses
    /// a value with a tag, an anchor or over more than one line (`U`), and an entry with
    /// a merge key or a tagged key (`E`).
    ///
    /// The third review's rows (`reviews/A8-round3.md`, ruled in `A8-lead-rulings-3.md`)
    /// follow: only the first document is read; a raw CR, NEL, U+2028 or U+2029 refuses
    /// the whole file in both readers (`F`, `FileError`); a plain name with colons is
    /// read under its own name; a byte-order mark is skipped; a second `servers:`, and
    /// `servers:` in flow form or with a tag or an anchor, refuse the whole file in
    /// shed-core; a tab-indented line refuses its entry; a no-break space before a colon
    /// makes another key.
    #[test]
    fn both_readers_on_every_shape_of_the_review() {
        use shed_core::identityfile::{
            IDENTITY_FILE_ENTRY_UNREADABLE as E, IDENTITY_FILE_UNREADABLE as U,
        };
        use BrokerReads::{FileError, NoTarget, Target};
        const P: &str = "/keys/prod";
        const F: &str = "<the whole file refused>";
        // (shape, the config, shed-core's identity_file, the broker's answer, Go's)
        let rows: Vec<(&str, &str, &str, BrokerReads, &str)> = vec![
            (
                "an escaped quoted key",
                "    host: prod.example\n    \"identity\\u005ffile\": /keys/prod\n",
                P,
                Target(P),
                "path",
            ),
            (
                "a plain value continued on an indented line",
                "    host: prod.example\n    identity_file: /keys/prod\n      more\n",
                E,
                Target(U),
                "\"/keys/prod more\"",
            ),
            (
                "a folded block scalar",
                "    host: prod.example\n    identity_file: >-\n      /keys/prod\n",
                E,
                Target(U),
                "path",
            ),
            (
                "a literal block scalar",
                "    host: prod.example\n    identity_file: |\n      /keys/prod\n",
                E,
                Target(U),
                "the path and a newline (refused by the value rule)",
            ),
            (
                "a value tagged !!str",
                "    host: prod.example\n    identity_file: !!str /keys/prod\n",
                E,
                Target(U),
                "path",
            ),
            (
                "!!str null",
                "    host: prod.example\n    identity_file: !!str null\n",
                E,
                Target(U),
                "the string \"null\" (refused by the value rule)",
            ),
            (
                "a key tagged !!str",
                "    host: prod.example\n    !!str identity_file: /keys/prod\n",
                E,
                Target(E),
                "path",
            ),
            (
                "a key tagged !!binary",
                "    host: prod.example\n    !!binary aWRlbnRpdHlfZmlsZQ==: /keys/prod\n",
                E,
                Target(E),
                "path (the key decodes to identity_file)",
            ),
            (
                "an anchored value",
                "    host: prod.example\n    identity_file: &k /keys/prod\n",
                E,
                Target(U),
                "path",
            ),
            (
                "an alias value",
                "    host: prod.example\n    other: &k /keys/prod\n    identity_file: *k\n",
                E,
                FileError,
                "the anchored path",
            ),
            (
                "an anchored key",
                "    host: prod.example\n    &k identity_file: /keys/prod\n",
                E,
                Target(P),
                "path",
            ),
            (
                "a merge key",
                "    host: prod.example\n    <<: {identity_file: /keys/prod}\n",
                E,
                Target(E),
                "path",
            ),
            (
                "a repeated identity_file",
                "    host: prod.example\n    identity_file: /keys/a\n    identity_file: /keys/b\n",
                E,
                FileError,
                "an error for the whole file",
            ),
            (
                "the fields as a flow mapping on the next line",
                "    {host: prod.example, identity_file: /keys/prod}\n",
                E,
                Target(P),
                "path",
            ),
            (
                "an explicit ? key and : value",
                "    host: prod.example\n    ? identity_file\n    : /keys/prod\n",
                E,
                Target(P),
                "path",
            ),
            (
                "an explicit ? key and : value before the host",
                "    ? identity_file\n    : /keys/prod\n    host: prod.example\n",
                E,
                Target(P),
                "path",
            ),
            (
                "an unquoted null",
                "    host: prod.example\n    identity_file: null\n",
                "",
                Target(""),
                "no identity file",
            ),
            (
                "an unquoted ~",
                "    host: prod.example\n    identity_file: ~\n",
                "",
                Target(""),
                "no identity file",
            ),
            (
                "a bare value",
                "    host: prod.example\n    identity_file:\n",
                "",
                Target(""),
                "no identity file",
            ),
            (
                "a quoted empty value",
                "    host: prod.example\n    identity_file: \"\"\n",
                "",
                Target(""),
                "no identity file",
            ),
            (
                "a path, a space and a comment",
                "    host: prod.example\n    identity_file: /keys/prod # the key\n",
                P,
                Target(P),
                "path",
            ),
            (
                "a path, a tab and a comment",
                "    host: prod.example\n    identity_file: /keys/prod\t# the key\n",
                P,
                Target(P),
                "path",
            ),
            (
                "a null host",
                "    host: null\n    identity_file: /keys/prod\n",
                E,
                NoTarget,
                "an empty host",
            ),
            (
                "a ~ host",
                "    host: ~\n    identity_file: /keys/prod\n",
                E,
                NoTarget,
                "an empty host",
            ),
            (
                "a bare host",
                "    host:\n    identity_file: /keys/prod\n",
                E,
                NoTarget,
                "an empty host",
            ),
        ];
        // The entry on its own line, a flow mapping: shed-core used to skip it silently.
        let inline = "servers:\n  prod: {host: prod.example, identity_file: /keys/prod}\n";
        // A merge through an alias: the anchor is outside `servers:`.
        let merged_alias = "base: &base\n  identity_file: /keys/prod\nservers:\n  prod:\n    \
                            host: prod.example\n    <<: *base\n";
        // The third review (`reviews/A8-round3.md`; the lead's ruling,
        // `A8-lead-rulings-3.md`): F is shed-core refusing the WHOLE file (no entry read).
        let base = "servers:\n  prod:\n    host: prod.example\n    user: alice\n    \
                    identity_file: /keys/prod\n";
        let in_comment = |br: &str| {
            format!(
                "servers:\n  prod:\n    host: prod.example\n    # note{br}    identity_file: \
                 /keys/prod\n"
            )
        };
        let long = "x".repeat(130);
        let third: Vec<(String, String, String, &str, BrokerReads, &str)> = vec![
            (
                "a second document replaces the entry".into(),
                format!("{base}---\nservers:\n  prod:\n    host: other.example\n"),
                "prod".into(),
                P,
                Target(P),
                "path (the first document)",
            ),
            (
                "a document end, then another servers:".into(),
                format!("{base}...\nservers:\n  prod:\n    host: other.example\n"),
                "prod".into(),
                P,
                Target(P),
                "path (the first document)",
            ),
            (
                "a CR inside a comment".into(),
                in_comment("\r"),
                "prod".into(),
                F,
                FileError,
                "path",
            ),
            (
                "a NEL inside a comment".into(),
                in_comment("\u{85}"),
                "prod".into(),
                F,
                FileError,
                "path",
            ),
            (
                "a line separator inside a comment".into(),
                in_comment("\u{2028}"),
                "prod".into(),
                F,
                FileError,
                "path",
            ),
            (
                "a paragraph separator inside a comment".into(),
                in_comment("\u{2029}"),
                "prod".into(),
                F,
                FileError,
                "path",
            ),
            (
                "a plain name with a colon (yaml.v3's output)".into(),
                "servers:\n    prod:blue:\n        host: prod.example\n        ssh_port: 22\n        \
                 identity_file: /keys/prod\ndefault_server: \"\"\nsheds: {}\n"
                    .into(),
                "prod:blue".into(),
                P,
                Target(P),
                "path",
            ),
            (
                "a plain name with two colons".into(),
                "servers:\n  a:b:c:\n    host: prod.example\n    identity_file: /keys/prod\n"
                    .into(),
                "a:b:c".into(),
                P,
                Target(P),
                "path",
            ),
            (
                "a name over 128 bytes, an explicit key (yaml.v3's output)".into(),
                format!(
                    "servers:\n    ? {long}\n    :   host: prod.example\n        identity_file: \
                     /keys/prod\n"
                ),
                long.clone(),
                E,
                Target(P),
                "path",
            ),
            (
                "a byte-order mark before servers:".into(),
                format!("\u{feff}{base}"),
                "prod".into(),
                P,
                Target(P),
                "path",
            ),
            (
                "a second servers: map".into(),
                format!("{base}servers:\n  prod:\n    host: other.example\n"),
                "prod".into(),
                F,
                FileError,
                "an error for the whole file",
            ),
            (
                "the identity line indented with two tabs".into(),
                base.replace("    identity_file", "\t\tidentity_file"),
                "prod".into(),
                E,
                FileError,
                "an error for the whole file",
            ),
            (
                "a no-break space before the host's colon".into(),
                base.replace("host:", "host\u{a0}:"),
                "prod".into(),
                P,
                NoTarget,
                "path (an unknown key host\u{a0}, no host)",
            ),
            (
                "servers: in flow form".into(),
                "servers: {prod: {host: prod.example, identity_file: /keys/prod}}\n".into(),
                "prod".into(),
                F,
                Target(P),
                "path",
            ),
            (
                "servers: with a tagged key".into(),
                format!("!!str {base}"),
                "prod".into(),
                F,
                Target(P),
                "path",
            ),
            (
                "servers: with an anchored key".into(),
                format!("&s {base}"),
                "prod".into(),
                F,
                Target(P),
                "path",
            ),
            (
                "servers: with an anchored value".into(),
                base.replace("servers:", "servers: &s"),
                "prod".into(),
                F,
                Target(P),
                "path",
            ),
            (
                "servers: with a tagged value".into(),
                base.replace("servers:", "servers: !!map"),
                "prod".into(),
                F,
                Target(P),
                "path",
            ),
        ];
        for (shape, yaml, name, core_reads, broker_reads, go_reads) in rows
            .into_iter()
            .map(|(shape, entry, c, b, g)| {
                (
                    shape.to_string(),
                    format!("servers:\n  prod:\n{entry}"),
                    "prod".to_string(),
                    c,
                    b,
                    g,
                )
            })
            .chain([
                (
                    "the entry as a flow mapping on its own line".to_string(),
                    inline.to_string(),
                    "prod".to_string(),
                    E,
                    Target(P),
                    "path",
                ),
                (
                    "a merge through an alias".to_string(),
                    merged_alias.to_string(),
                    "prod".to_string(),
                    E,
                    FileError,
                    "the merged path",
                ),
            ])
            .chain(third)
        {
            let core = shed_core::config::ShedConfig::parse(&yaml);
            if core_reads == F {
                // The whole file refused, saying why: no entry at all, none with defaults.
                assert!(core.refused.is_some(), "shed-core, {shape}");
                assert!(core.servers.is_empty(), "shed-core, {shape}");
            } else {
                assert_eq!(core.refused, None, "shed-core, {shape}");
            }
            let c = core
                .servers
                .iter()
                .find(|s| s.name == name)
                .cloned()
                .unwrap_or_else(|| {
                    assert_eq!(core_reads, F, "{shape}: shed-core dropped the entry");
                    shed_core::config::ShedServerEntry {
                        identity_file: F.to_string(),
                        ..Default::default()
                    }
                });
            assert_eq!(c.identity_file, core_reads, "shed-core, {shape}");
            if core_reads == P {
                // Read: the first document's host and user, never another's.
                assert_eq!(
                    c.host,
                    if go_reads.contains("no host") {
                        name.as_str()
                    } else {
                        "prod.example"
                    },
                    "shed-core, {shape}"
                );
            }
            if core_reads == E {
                // Refused before any field is read: no host to fall back to, no user.
                assert_eq!(c.host, "", "shed-core, {shape}");
                assert_eq!(
                    c.user,
                    shed_core::config::USER_NOT_A_STRING,
                    "shed-core, {shape}"
                );
            }
            let (_d, cfg) = write_config(&yaml);
            let b = match load_discovered_servers(&cfg) {
                Err(_) => FileError,
                Ok(targets) => match targets.iter().find(|t| t.name == name) {
                    None => NoTarget,
                    Some(t) => Target(match t.identity_file.as_str() {
                        s if s == E => E,
                        s if s == U => U,
                        s if s == P => P,
                        "" => "",
                        other => panic!("{shape}: the broker read {other:?}"),
                    }),
                },
            };
            assert_eq!(b, broker_reads, "the broker, {shape}");
            // The safe side: never "no identity file" where Go reads a path.
            if go_reads.starts_with("path") || go_reads.ends_with("path") {
                assert_ne!(
                    c.identity_file, "",
                    "shed-core reads no key where Go reads {go_reads}: {shape}"
                );
                assert_ne!(
                    b,
                    Target(""),
                    "the broker reads no key where Go reads {go_reads}: {shape}"
                );
            }
        }
    }

    /// A raw line break the broker's parser does not split on as Go's does refuses the
    /// whole file, with a reason naming the character (plan 037 A8); the same characters
    /// escaped inside a double-quoted value are text, and CRLF endings are line endings.
    #[test]
    fn unsupported_line_breaks_refuse_the_file_and_their_escapes_do_not() {
        for (br, what) in [
            ("\r", "carriage return"),
            ("\u{85}", "U+0085"),
            ("\u{2028}", "U+2028"),
            ("\u{2029}", "U+2029"),
        ] {
            let (_d, cfg) = write_config(&format!(
                "servers:\n  prod:\n    host: prod.example\n    # note{br}    identity_file: \
                 /keys/prod\n"
            ));
            let err = load_discovered_servers(&cfg).expect_err(what);
            assert!(
                err.contains("parsing shed config") && err.contains(what),
                "{err}"
            );
        }
        for text in [
            "servers:\n  prod:\n    host: prod.example\n    user: \"a\\r\\N\\L\\Pb\"\n",
            "servers:\r\n  prod:\r\n    host: prod.example\r\n",
        ] {
            let (_d, cfg) = write_config(text);
            let got = load_discovered_servers(&cfg).unwrap();
            assert_eq!(got[0].ssh_host, "prod.example", "{text:?}");
        }
    }

    /// **The promise** (plan 037 A8): the config.yaml the shed CLI's own save path writes
    /// (`crates/fixtures/clientconfig/`, written and kept fresh by
    /// `internal/config/clientconfig_fixture_test.go`) gives one target per entry, with
    /// the host, ssh_port, user and identity_file the CLI holds for it.
    #[test]
    fn the_cli_written_config_is_read_as_the_cli_reads_it() {
        let dir =
            std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../fixtures/clientconfig");
        let got = load_discovered_servers(&dir.join("config.yaml").to_string_lossy()).unwrap();
        let expected: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(dir.join("expected.json")).unwrap())
                .unwrap();
        let want = expected["servers"].as_array().unwrap();
        assert_eq!(got.len(), want.len());
        for w in want {
            let name = w["name"].as_str().unwrap();
            let t = got
                .iter()
                .find(|t| t.name == name)
                .unwrap_or_else(|| panic!("no target {name}"));
            assert_eq!(t.ssh_host, w["host"].as_str().unwrap(), "{name} host");
            assert_eq!(
                u64::from(t.ssh_port),
                w["ssh_port"].as_u64().unwrap(),
                "{name} ssh_port"
            );
            assert_eq!(t.user, w["user"].as_str().unwrap(), "{name} user");
            assert_eq!(
                t.identity_file,
                w["identity_file"].as_str().unwrap(),
                "{name} identity_file"
            );
        }
    }

    /// The single-server (no `discovery:` block) target carries no learned mode — it has
    /// no CLI config entry to read one from.
    #[test]
    fn single_server_target_has_no_auth_mode() {
        let cfg = ExtensionsYaml::parse("server: http://localhost:8080\n");
        let got = cfg.resolve_targets_from(Vec::new());
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].auth_mode, "");
        assert!(!got[0].is_mtls());
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

    /// The Go-written fixture (`crates/fixtures/identityfile/`), read through this reader:
    /// every accepted entry of `config.yaml` and every refused entry of `refused.yaml`
    /// decodes to Go's `identity_file`, and each accepted one to the fixture's host and
    /// port. The other half — that those values give Go's bootstrap argv and that the
    /// refused ones spawn no ssh — is `shed_source::bootstrap`'s test of the same fixture
    /// (split from one test there by plan 039 §3.7).
    #[test]
    fn the_go_identityfile_fixture_decodes_as_go_decodes_it() {
        let root =
            std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../fixtures/identityfile");
        let expected: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(root.join("expected.json")).unwrap())
                .unwrap();
        let targets = load_discovered_servers(root.join("config.yaml").to_str().unwrap()).unwrap();
        let accepted = expected["accepted"].as_array().unwrap();
        assert_eq!(targets.len(), accepted.len());
        for want in accepted {
            let name = want["entry"].as_str().unwrap();
            let target = targets.iter().find(|t| t.name == name).unwrap();
            assert_eq!(
                target.identity_file,
                want["identity_file"].as_str().unwrap(),
                "{name}"
            );
            assert_eq!(
                target.ssh_host,
                expected["host"].as_str().unwrap(),
                "{name}"
            );
            assert_eq!(
                u64::from(target.ssh_port),
                expected["ssh_port"].as_u64().unwrap(),
                "{name}"
            );
        }

        let refused = load_discovered_servers(root.join("refused.yaml").to_str().unwrap()).unwrap();
        for want in expected["refused"].as_array().unwrap() {
            let name = want["entry"].as_str().unwrap();
            let target = refused.iter().find(|t| t.name == name).unwrap();
            assert_eq!(
                target.identity_file,
                want["identity_file"].as_str().unwrap(),
                "{name}: decoded as Go decodes it"
            );
        }
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
            auth_mode: String::new(),
            user: String::new(),
            identity_file: String::new(),
        };
        let discovered = vec![
            mk("mini2", "http://mini2:8080"),
            mk("mini3", "http://mini3:8080"),
        ];

        // Legacy single-server: one unnamed target, `discovered` ignored.
        let cfg = ExtensionsYaml::parse("server: http://localhost:8080\n");
        assert_eq!(
            cfg.resolve_targets_from(discovered.clone()),
            vec![mk("", "http://localhost:8080")]
        );

        // Discover all.
        let cfg = ExtensionsYaml::parse("discovery:\n  servers: all\n");
        assert_eq!(cfg.resolve_targets_from(discovered.clone()), discovered);

        // Include a subset.
        let cfg = ExtensionsYaml::parse("discovery:\n  servers: [mini3]\n");
        assert_eq!(
            cfg.resolve_targets_from(discovered.clone()),
            vec![mk("mini3", "http://mini3:8080")]
        );

        // Dedup by name (first occurrence wins).
        let dup = vec![
            mk("mini2", "http://mini2:8080"),
            mk("mini2", "http://other:8080"),
        ];
        let cfg = ExtensionsYaml::parse("discovery:\n  servers: all\n");
        let got = cfg.resolve_targets_from(dup);
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].url, "http://mini2:8080");

        // Explicit empty list (`servers: []`) selects nothing.
        let cfg = ExtensionsYaml::parse("discovery:\n  servers: []\n");
        assert!(cfg.resolve_targets_from(discovered).is_empty());
    }

    /// The I/O `resolve_targets` (Go's free `resolveTargets(cfg)`): legacy mode returns
    /// the unnamed target with no file read; discovery mode reads + filters the source.
    /// This is the commit-1 caller of the method the supervisor wires into `main.rs` at
    /// commit 3.
    #[test]
    fn resolve_targets_reads_source() {
        // Legacy: no discovery block → the unnamed single target, no I/O.
        let cfg = ExtensionsYaml::parse("server: http://localhost:8080\n");
        let got = cfg.resolve_targets().expect("legacy resolves");
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].name, "");
        assert_eq!(got[0].url, "http://localhost:8080");

        // Discovery: source points at a written shed config; the selector filters it.
        let (_d, shed) = write_config(
            "servers:\n  mini2:\n    host: mini2\n    http_port: 8080\n  mini3:\n    host: mini3\n",
        );
        let cfg = ExtensionsYaml::parse(&format!(
            "discovery:\n  servers: [mini3]\n  source: {shed}\n"
        ));
        let got = cfg.resolve_targets().expect("discovery resolves");
        assert_eq!(
            got.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(),
            ["mini3"]
        );
    }
}
