//! Minimal host-agent config reader for slice 0 — deliberately scoped to exactly
//! what the LiveStatus self-report needs: the three approval policies
//! (`ssh/aws/docker .approval.policy`) plus `logging.{enabled,path}`. The full
//! config schema (discovery, per-server overrides, AWS/Docker resolution,
//! biometric scope/ttl, `Validate()`) is a later slice.
//!
//! Parser style follows shed-core's `config.rs`: a deliberately tiny
//! indentation-aware nested-map/scalar reader (`yaml_lite`) rather than a YAML
//! crate dependency — the repo intentionally avoids serde_yaml/serde_norway.

use std::collections::BTreeMap;
use std::io;
use std::path::PathBuf;
use std::time::Duration;

/// The three credential namespaces, in the fixed status/gate order.
pub const NS_SSH_AGENT: &str = "ssh-agent";
pub const NS_AWS_CREDENTIALS: &str = "aws-credentials";
pub const NS_DOCKER_CREDENTIALS: &str = "docker-credentials";

/// Approval-policy value that fails closed — the effective policy for an empty or
/// omitted `approval.policy` (matches the Go `EffectivePolicy`).
pub const POLICY_DENY_ALL: &str = "deny-all";
/// Approval-policy value that approves every request (the allowlist/role still apply
/// downstream in the AWS/Docker backends). Matches Go's `PolicyApproveAll`. Consumed
/// by the gate seam (`approval.rs`) + `main.rs`, not within `config.rs` itself, so
/// the golden test's isolated `#[path]`-included `config` module (which pulls neither)
/// would otherwise flag it dead.
#[allow(dead_code)]
pub const POLICY_APPROVE_ALL: &str = "approve-all";
/// Approval-policy value that delegates the decision to the shed-desktop app.
pub const POLICY_SHED_DESKTOP: &str = "shed-desktop";

/// The single-server bus URL default — matches Go's `Config.Server` default
/// (`config.go:428`). Used only in single-server mode (no `discovery:` block).
pub const DEFAULT_SERVER_URL: &str = "http://localhost:8080";

/// `effective_policy_from_raw` maps a raw `approval.policy` string to its effective
/// value: an empty/omitted policy fails closed to deny-all; any other value is
/// echoed verbatim. This is the namespace-independent core of Go's
/// `ApprovalConfig.EffectivePolicy`, exposed as a pure entry point so the
/// language-neutral golden fixtures can exercise it without building a config.
pub fn effective_policy_from_raw(raw: &str) -> String {
    if raw.is_empty() {
        POLICY_DENY_ALL.to_string()
    } else {
        raw.to_string()
    }
}

/// `user_home_dir` returns `$HOME`, falling back to `/tmp` when unset (mirrors the
/// Go daemon's `userHomeDir` → `os.UserHomeDir` with a `/tmp` fallback).
pub fn user_home_dir() -> PathBuf {
    match std::env::var_os("HOME") {
        Some(h) if !h.is_empty() => PathBuf::from(h),
        _ => PathBuf::from("/tmp"),
    }
}

/// `expand_tilde` expands a leading `~/` to the user's home directory (mirrors the
/// Go daemon's `expandTilde`).
pub fn expand_tilde(path: &str) -> String {
    if let Some(rest) = path.strip_prefix("~/") {
        return user_home_dir().join(rest).to_string_lossy().into_owned();
    }
    path.to_string()
}

/// Resolve `approval_timeout`'s raw string to a `Duration`, defaulting an
/// absent/invalid/non-positive value to 25s (fail-safe). Always called by `parse`.
fn parse_approval_timeout(raw: &str) -> Duration {
    const DEFAULT: Duration = Duration::from_secs(25);
    match parse_go_duration_nanos(raw) {
        Some(nanos) if nanos > 0 => Duration::from_nanos(nanos as u64),
        _ => DEFAULT,
    }
}

/// Parse a Go `time.ParseDuration` string (`"25s"`, `"40s"`, `"1h30m"`,
/// `"300ms"`, `"1.5h"`, optional leading sign) into signed total nanoseconds.
/// Units: `ns`, `us`/`µs`/`μs`, `ms`, `s`, `m`, `h`. `None` on an empty or
/// malformed string (unknown unit, missing number, etc.). A faithful subset — the
/// caller only needs positivity + magnitude, so the signed result lets it detect a
/// non-positive value and fall back to the default.
///
/// `pub(crate)` so `aws_backend`'s `parse_duration_or` (the AWS session-duration /
/// cache-refresh knobs) reuses the same Go-`time.ParseDuration` subset.
pub(crate) fn parse_go_duration_nanos(s: &str) -> Option<i128> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    let (neg, mut rest) = if let Some(r) = s.strip_prefix('-') {
        (true, r)
    } else if let Some(r) = s.strip_prefix('+') {
        (false, r)
    } else {
        (false, s)
    };
    // Go accepts a bare "0" (and "+0"/"-0") with no unit.
    if rest == "0" {
        return Some(0);
    }
    if rest.is_empty() {
        return None;
    }
    let mut total: i128 = 0;
    while !rest.is_empty() {
        // Number: digits and at most one '.'.
        let num_len = rest
            .find(|c: char| !(c.is_ascii_digit() || c == '.'))
            .unwrap_or(rest.len());
        if num_len == 0 {
            return None;
        }
        let value: f64 = rest[..num_len].parse().ok()?;
        rest = &rest[num_len..];
        // Unit: run of non-number characters.
        let unit_len = rest
            .find(|c: char| c.is_ascii_digit() || c == '.' || c == '+' || c == '-')
            .unwrap_or(rest.len());
        if unit_len == 0 {
            return None;
        }
        let unit_nanos: f64 = match &rest[..unit_len] {
            "ns" => 1.0,
            "us" | "µs" | "μs" => 1_000.0,
            "ms" => 1_000_000.0,
            "s" => 1_000_000_000.0,
            "m" => 60.0 * 1_000_000_000.0,
            "h" => 3_600.0 * 1_000_000_000.0,
            _ => return None,
        };
        rest = &rest[unit_len..];
        total += (value * unit_nanos) as i128;
    }
    Some(if neg { -total } else { total })
}

/// The slice-0/1 view of the host-agent config: the fields LiveStatus needs plus
/// `approval_timeout` (the delegated-approval budget the desktop server enforces).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HostAgentConfig {
    ssh_policy: String,
    aws_policy: String,
    docker_policy: String,
    /// The SSH backend mode (`ssh.mode`): `"agent-forward"`, `"local-keys"`, or `""`
    /// (auto-detect). Mirrors Go's `SSHConfig.Mode`; consumed by
    /// `ssh_backend::resolve_ssh_backend`.
    ssh_mode: String,
    pub logging_enabled: bool,
    pub logging_path: String,
    // Read only by `approval_timeout()`, which only the desktop server calls.
    #[cfg_attr(not(feature = "desktop-forwarding"), allow(dead_code))]
    approval_timeout: Duration,
    /// The single-server bus URL (`server:`), defaulting to `DEFAULT_SERVER_URL`.
    /// The message-bus daemon connects here in single-server mode. In Go this is
    /// `Config.Server`, used only when `Discovery` is nil.
    pub server: String,
    /// Whether a `discovery:` block is present. When true the agent is in
    /// multi-server discovery mode (a later slice) and the single-server bus is NOT
    /// started — mirroring Go's `cfg.Discovery != nil` gate in `main.go`. This
    /// minimal reader does not yet parse the discovery contents, only its presence.
    pub has_discovery: bool,
    /// The layered AWS credential policy (`aws.{source_profile,default_role,mode,
    /// session_duration,cache_refresh_before,servers…}`), mirroring Go's `AWSConfig`.
    /// The `aws.approval.policy` gate is kept in `aws_policy` above (unchanged); this
    /// field carries only the resolution/vending knobs. Consumed by the AWS backend
    /// (`aws_backend.rs`).
    pub aws: AwsConfig,
}

impl HostAgentConfig {
    /// Load + parse the config at `path` (tilde-expanded). A missing/unreadable
    /// file is an **error**: the daemon requires a config to know its policies, so
    /// it exits 1 rather than fail open (matches the Go daemon's `LoadConfig` →
    /// `os.Exit(1)` on any load error).
    pub fn load(path: &str) -> io::Result<HostAgentConfig> {
        let expanded = expand_tilde(path);
        let text = std::fs::read_to_string(&expanded)?;
        Ok(Self::parse(&text))
    }

    /// Parse config text. Missing keys take their defaults (policies default to
    /// empty → deny-all effective; logging enabled true).
    pub fn parse(text: &str) -> HostAgentConfig {
        let root = yaml_lite::parse(text);
        let policy = |ns_key: &str| -> String {
            root.get_path(&[ns_key, "approval", "policy"])
                .unwrap_or_default()
                .to_string()
        };
        let logging_enabled = match root.get_path(&["logging", "enabled"]) {
            Some(v) => v != "false",
            None => true,
        };
        let logging_path = match root.get_path(&["logging", "path"]) {
            Some(p) if !p.is_empty() => expand_tilde(p),
            _ => user_home_dir()
                .join(".local")
                .join("share")
                .join("shed")
                .join("extensions-audit.log")
                .to_string_lossy()
                .into_owned(),
        };
        let server = match root.get_path(&["server"]) {
            Some(s) if !s.is_empty() => s.to_string(),
            _ => DEFAULT_SERVER_URL.to_string(),
        };
        HostAgentConfig {
            ssh_policy: policy("ssh"),
            aws_policy: policy("aws"),
            docker_policy: policy("docker"),
            ssh_mode: root
                .get_path(&["ssh", "mode"])
                .unwrap_or_default()
                .to_string(),
            logging_enabled,
            logging_path,
            approval_timeout: parse_approval_timeout(
                root.get_path(&["approval_timeout"]).unwrap_or_default(),
            ),
            server,
            has_discovery: root.has_key("discovery"),
            aws: AwsConfig::from_node(&root),
        }
    }

    /// True when the agent runs in single-server mode — no `discovery:` block, so
    /// the message-bus daemon connects to the single `server:` URL. Mirrors Go's
    /// `cfg.Discovery == nil` branch in `main.go`; multi-server discovery is a later
    /// slice, so with a `discovery:` block present the single-server bus stays off.
    pub fn is_single_server(&self) -> bool {
        !self.has_discovery
    }

    /// The delegated-approval budget the desktop server enforces per request, and
    /// which drives the `hello_ack.request_timeout_ms` it advertises. Mirrors Go's
    /// `ApprovalTimeoutDuration` + `NewDesktopServer`'s guard: an absent, invalid,
    /// or non-positive `approval_timeout` falls back to 25s. (The full config's
    /// `Validate()` hard-rejects an invalid value at startup; this minimal reader
    /// does no validation, so it fails safe to the default instead — a later slice.)
    #[cfg_attr(not(feature = "desktop-forwarding"), allow(dead_code))]
    pub fn approval_timeout(&self) -> Duration {
        self.approval_timeout
    }

    /// The configured SSH backend mode (`ssh.mode`): `"agent-forward"`,
    /// `"local-keys"`, or `""` (auto-detect). Passed to
    /// `ssh_backend::resolve_ssh_backend_from_env` at daemon startup.
    pub fn ssh_mode(&self) -> &str {
        &self.ssh_mode
    }

    /// `effective_policy` returns the configured policy for a namespace, defaulting
    /// an empty/omitted value to deny-all (fail-closed) — matches Go's
    /// `ApprovalConfig.EffectivePolicy`. An unknown namespace also yields deny-all.
    pub fn effective_policy(&self, ns: &str) -> String {
        let raw: &str = match ns {
            NS_SSH_AGENT => self.ssh_policy.as_str(),
            NS_AWS_CREDENTIALS => self.aws_policy.as_str(),
            NS_DOCKER_CREDENTIALS => self.docker_policy.as_str(),
            _ => "",
        };
        effective_policy_from_raw(raw)
    }

    /// `gate_namespaces` lists the namespaces whose effective policy is
    /// shed-desktop, in the fixed order ssh-agent, aws-credentials,
    /// docker-credentials (matches Go's `desktopGateNamespaces`).
    pub fn gate_namespaces(&self) -> Vec<String> {
        [NS_SSH_AGENT, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS]
            .into_iter()
            .filter(|ns| self.effective_policy(ns) == POLICY_SHED_DESKTOP)
            .map(str::to_string)
            .collect()
    }
}

/// AWS credential vending modes (mirror Go's `AWSMode*` in `config.go`). `mode`
/// selects how the agent obtains the creds it vends for a shed: `assume-role`
/// (default) does `sts:AssumeRole` from the source profile into the resolved role;
/// `passthrough` vends the source profile's existing session credentials directly
/// (SSO/SAML setups with no assumable role). An empty mode means assume-role.
pub const AWS_MODE_ASSUME_ROLE: &str = "assume-role";
pub const AWS_MODE_PASSTHROUGH: &str = "passthrough";

/// `normalize_aws_mode` maps an empty mode to the assume-role default (Go's
/// `normalizeAWSMode`).
fn normalize_aws_mode(mode: &str) -> &str {
    if mode.is_empty() {
        AWS_MODE_ASSUME_ROLE
    } else {
        mode
    }
}

/// The layered AWS config (`aws.*`), a faithful port of Go's `AWSConfig`. The
/// top-level fields are the defaults; `servers` carries per-server (and per-shed)
/// overrides layered over them. `source_profile` and `cache_refresh_before` are
/// process-global and are not overridable per server. The `aws.approval.policy`
/// gate lives on `HostAgentConfig.aws_policy`, not here.
///
/// Built by [`AwsConfig::from_node`] (called from `HostAgentConfig::parse`) and
/// consumed by the AWS backend (`aws_backend.rs`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AwsConfig {
    pub source_profile: String,
    pub default_role: String,
    /// `""` (= assume-role) | `assume-role` | `passthrough`.
    pub mode: String,
    pub session_duration: String,
    pub cache_refresh_before: String,
    pub servers: BTreeMap<String, AwsServerConfig>,
}

/// Per-server AWS overrides (Go's `AWSServerConfig`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AwsServerConfig {
    pub default_role: String,
    pub mode: String,
    pub session_duration: String,
    pub sheds: BTreeMap<String, AwsShedConfig>,
}

/// Per-shed AWS overrides (Go's `ShedAWSConfig`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AwsShedConfig {
    pub role: String,
    pub mode: String,
    pub session_duration: String,
}

/// The effective AWS policy for a single `(server, shed)` pair (Go's `ResolvedAWS`).
/// Constructed by [`AwsConfig::resolve`] and consumed by the AWS backend
/// (`aws_backend.rs`), which `main.rs` wires into the bus.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedAws {
    /// Assumed-role ARN (`""` => no role configured).
    pub role: String,
    /// `AWS_MODE_ASSUME_ROLE` | `AWS_MODE_PASSTHROUGH` (never `""`).
    pub mode: String,
    /// `""` => fall back to the backend default.
    pub session_duration: String,
}

// `from_node` is live (called by `HostAgentConfig::parse`); `resolve`/`enabled` are
// reached through the AWS backend + `main.rs`, wired into the bus (`new_sts_backend`
// calls `enabled`; the backend's get_credentials/status call `resolve`).
impl AwsConfig {
    /// Build the AWS slice from the parsed config tree, applying the same load
    /// defaults Go's `DefaultConfig`/`LoadConfig` do (`config.go:439-443`): an
    /// empty/absent `source_profile` → `"default"`, `session_duration` → `"1h"`,
    /// `cache_refresh_before` → `"5m"`. For an ABSENT key (or absent `aws:` block)
    /// this matches Go exactly (the defaults sit on `DefaultConfig` and the YAML
    /// merge only overwrites keys the document names). For an EXPLICITLY-EMPTY
    /// scalar (`source_profile: ""`) Go keeps the empty string — it would call STS
    /// with profile `""` — while this port re-applies the default; `yaml_lite`
    /// cannot even represent the distinction (a bare `key:` parses as an empty
    /// map). Known divergence, same class as the other `yaml_lite` permissiveness
    /// gaps, closed by the config-port sub-plan (cursor review, 2026-07-11).
    ///
    /// `aws.sheds` (Go's removed global per-shed map) is intentionally NOT read: it
    /// is parse-and-ignore for resolution. Go's `AWSConfig.validate` rejects it with
    /// a migration message; that rejection is sub-plan 5.
    fn from_node(root: &yaml_lite::Node) -> AwsConfig {
        use yaml_lite::Node;
        let aws = root.as_map().and_then(|m| m.get("aws")).and_then(Node::as_map);
        let scalar = |key: &str| -> String {
            aws.and_then(|m| m.get(key))
                .and_then(Node::as_scalar)
                .unwrap_or("")
                .to_string()
        };
        let default_if_empty = |v: String, d: &str| if v.is_empty() { d.to_string() } else { v };

        let mut servers = BTreeMap::new();
        if let Some(servers_map) = aws.and_then(|m| m.get("servers")).and_then(Node::as_map) {
            for (name, entry) in servers_map {
                let Some(fields) = entry.as_map() else { continue };
                let sfield = |k: &str| {
                    fields
                        .get(k)
                        .and_then(Node::as_scalar)
                        .unwrap_or("")
                        .to_string()
                };
                let mut sheds = BTreeMap::new();
                if let Some(sheds_map) = fields.get("sheds").and_then(Node::as_map) {
                    for (sname, sentry) in sheds_map {
                        let Some(sf) = sentry.as_map() else { continue };
                        let g = |k: &str| sf.get(k).and_then(Node::as_scalar).unwrap_or("").to_string();
                        sheds.insert(
                            sname.clone(),
                            AwsShedConfig {
                                role: g("role"),
                                mode: g("mode"),
                                session_duration: g("session_duration"),
                            },
                        );
                    }
                }
                servers.insert(
                    name.clone(),
                    AwsServerConfig {
                        default_role: sfield("default_role"),
                        mode: sfield("mode"),
                        session_duration: sfield("session_duration"),
                        sheds,
                    },
                );
            }
        }

        AwsConfig {
            source_profile: default_if_empty(scalar("source_profile"), "default"),
            default_role: scalar("default_role"),
            mode: scalar("mode"),
            session_duration: default_if_empty(scalar("session_duration"), "1h"),
            cache_refresh_before: default_if_empty(scalar("cache_refresh_before"), "5m"),
            servers,
        }
    }

    /// Layer AWS overrides for a `(server, shed)` pair, most specific wins:
    /// top-level defaults → `servers[server]` → `servers[server].sheds[shed]`. Role,
    /// mode, and session_duration each layer independently; an empty mode means "no
    /// override" while layering and is normalized to `AWS_MODE_ASSUME_ROLE` at the
    /// end (so a child that only sets a role under a passthrough parent stays
    /// passthrough). A faithful port of Go's `AWSConfig.Resolve` (config.go:317-343).
    pub fn resolve(&self, server: &str, shed: &str) -> ResolvedAws {
        let mut role = self.default_role.clone();
        let mut mode = self.mode.clone();
        let mut session_duration = self.session_duration.clone();
        if let Some(sv) = self.servers.get(server) {
            if !sv.default_role.is_empty() {
                role = sv.default_role.clone();
            }
            if !sv.mode.is_empty() {
                mode = sv.mode.clone();
            }
            if !sv.session_duration.is_empty() {
                session_duration = sv.session_duration.clone();
            }
            if let Some(sc) = sv.sheds.get(shed) {
                if !sc.role.is_empty() {
                    role = sc.role.clone();
                }
                if !sc.mode.is_empty() {
                    mode = sc.mode.clone();
                }
                if !sc.session_duration.is_empty() {
                    session_duration = sc.session_duration.clone();
                }
            }
        }
        ResolvedAws {
            role,
            mode: normalize_aws_mode(&mode).to_string(),
            session_duration,
        }
    }

    /// Report whether the AWS handler should start at all: true if any resolution
    /// path selects passthrough mode or configures a non-empty role. An explicit
    /// assume-role with no role anywhere is "AWS off" (false). A faithful port of
    /// Go's `AWSConfig.Enabled` (config.go:356-371).
    pub fn enabled(&self) -> bool {
        if self.mode == AWS_MODE_PASSTHROUGH || !self.default_role.is_empty() {
            return true;
        }
        for sv in self.servers.values() {
            if sv.mode == AWS_MODE_PASSTHROUGH || !sv.default_role.is_empty() {
                return true;
            }
            for s in sv.sheds.values() {
                if s.mode == AWS_MODE_PASSTHROUGH || !s.role.is_empty() {
                    return true;
                }
            }
        }
        false
    }
}

/// A deliberately tiny indentation-based reader. Handles exactly what the
/// host-agent config's relevant subset contains: nested maps and scalar leaves.
/// Inline `{}` is an empty map; comments (`#`) and blank lines are skipped. A port
/// of shed-core's `yaml_lite` (with a `get_path` map-walk helper added).
pub(crate) mod yaml_lite {
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

        /// The underlying map when this node is a `Map` (used by
        /// `load_discovered_servers` to iterate the `servers` submap). `allow(dead_code)`
        /// because its only caller (`controltoken.rs`) is feature-gated AND the golden
        /// integration test compiles this module standalone via `#[path]`.
        #[allow(dead_code)]
        pub fn as_map(&self) -> Option<&HashMap<String, Node>> {
            match self {
                Node::Map(m) => Some(m),
                Node::Scalar(_) => None,
            }
        }

        /// Whether this node is a map containing `key` (as a scalar or a nested
        /// block). Used to detect the presence of a top-level block like
        /// `discovery:` whose contents this minimal reader doesn't yet parse.
        pub fn has_key(&self, key: &str) -> bool {
            match self {
                Node::Map(m) => m.contains_key(key),
                Node::Scalar(_) => false,
            }
        }

        /// Walk a chain of map keys to a leaf scalar; `None` if any key is missing,
        /// an intermediate node isn't a map, or the leaf isn't a scalar.
        pub fn get_path(&self, keys: &[&str]) -> Option<&str> {
            let mut node = self;
            for (i, key) in keys.iter().enumerate() {
                let Node::Map(m) = node else { return None };
                node = m.get(*key)?;
                if i + 1 == keys.len() {
                    return node.as_scalar();
                }
            }
            None
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_policies_and_logging() {
        let cfg = HostAgentConfig::parse(
            "\
ssh:
  approval:
    policy: shed-desktop
aws:
  approval:
    policy: approve-all
docker:
  approval:
    policy: deny-all
logging:
  enabled: false
  path: /tmp/x/audit.log
",
        );
        assert_eq!(cfg.effective_policy(NS_SSH_AGENT), "shed-desktop");
        assert_eq!(cfg.effective_policy(NS_AWS_CREDENTIALS), "approve-all");
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), "deny-all");
        assert!(!cfg.logging_enabled);
        assert_eq!(cfg.logging_path, "/tmp/x/audit.log");
    }

    #[test]
    fn empty_or_missing_policy_defaults_to_deny_all() {
        // An entirely empty config → all three deny-all, logging enabled default.
        let cfg = HostAgentConfig::parse("");
        assert_eq!(cfg.effective_policy(NS_SSH_AGENT), POLICY_DENY_ALL);
        assert_eq!(cfg.effective_policy(NS_AWS_CREDENTIALS), POLICY_DENY_ALL);
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), POLICY_DENY_ALL);
        assert!(cfg.logging_enabled);
        // A present ssh block with an empty policy still resolves to deny-all.
        let cfg2 = HostAgentConfig::parse("ssh:\n  approval:\n    policy:\n");
        assert_eq!(cfg2.effective_policy(NS_SSH_AGENT), POLICY_DENY_ALL);
        // An unknown namespace is deny-all too.
        assert_eq!(cfg2.effective_policy("nope"), POLICY_DENY_ALL);
    }

    #[test]
    fn gate_namespaces_selects_shed_desktop_in_fixed_order() {
        // Config order is docker, ssh, aws — the selection must still come back in
        // the fixed ssh, aws, docker order, and skip the non-shed-desktop aws.
        let cfg = HostAgentConfig::parse(
            "\
docker:
  approval:
    policy: shed-desktop
ssh:
  approval:
    policy: shed-desktop
aws:
  approval:
    policy: approve-all
",
        );
        assert_eq!(
            cfg.gate_namespaces(),
            vec!["ssh-agent".to_string(), "docker-credentials".to_string()]
        );
        // No shed-desktop anywhere → empty gate list.
        assert!(HostAgentConfig::parse("").gate_namespaces().is_empty());
    }

    #[test]
    fn load_missing_file_is_error() {
        assert!(HostAgentConfig::load("/nonexistent/does-not-exist-xyz.yaml").is_err());
    }

    #[test]
    fn server_defaults_and_single_server_detection() {
        // No `server:` and no `discovery:` → default URL + single-server mode.
        let cfg = HostAgentConfig::parse("");
        assert_eq!(cfg.server, DEFAULT_SERVER_URL);
        assert!(cfg.is_single_server());

        // An explicit `server:` overrides the default and stays single-server.
        let cfg = HostAgentConfig::parse("server: http://mini2:8080\n");
        assert_eq!(cfg.server, "http://mini2:8080");
        assert!(cfg.is_single_server());

        // A `discovery:` block flips to multi-server mode (bus stays off), and the
        // `server:` field defaults but is unused. Mirrors the watch-none diff config.
        let cfg = HostAgentConfig::parse(
            "\
discovery:
  servers: []
  watch: off
  source: /tmp/x.yaml
",
        );
        assert!(!cfg.is_single_server());
        assert!(cfg.has_discovery);
        assert_eq!(cfg.server, DEFAULT_SERVER_URL);
    }

    #[test]
    fn ssh_mode_reads_or_defaults_empty() {
        // Absent → "" (auto).
        assert_eq!(HostAgentConfig::parse("").ssh_mode(), "");
        // An ssh block with only an approval policy → still "".
        assert_eq!(
            HostAgentConfig::parse("ssh:\n  approval:\n    policy: approve-all\n").ssh_mode(),
            ""
        );
        // Explicit modes.
        assert_eq!(
            HostAgentConfig::parse("ssh:\n  mode: local-keys\n").ssh_mode(),
            "local-keys"
        );
        assert_eq!(
            HostAgentConfig::parse("ssh:\n  mode: agent-forward\n  approval:\n    policy: shed-desktop\n")
                .ssh_mode(),
            "agent-forward"
        );
    }

    #[test]
    fn approval_timeout_parses_and_defaults() {
        // Absent -> 25s default (matches Go's config default).
        assert_eq!(
            HostAgentConfig::parse("").approval_timeout(),
            Duration::from_secs(25)
        );
        assert_eq!(
            HostAgentConfig::parse("approval_timeout: 40s").approval_timeout(),
            Duration::from_secs(40)
        );
        assert_eq!(
            HostAgentConfig::parse("approval_timeout: 1h30m").approval_timeout(),
            Duration::from_secs(5400)
        );
        assert_eq!(
            HostAgentConfig::parse("approval_timeout: 300ms").approval_timeout(),
            Duration::from_millis(300)
        );
        // Invalid / non-positive -> fail-safe 25s (the full config's Validate() would
        // hard-reject these; this minimal reader falls back instead).
        for bad in ["nonsense", "0s", "-5s", "10"] {
            assert_eq!(
                HostAgentConfig::parse(&format!("approval_timeout: {bad}")).approval_timeout(),
                Duration::from_secs(25),
                "approval_timeout: {bad}"
            );
        }
    }

    #[test]
    fn go_duration_parser_units() {
        assert_eq!(parse_go_duration_nanos("25s"), Some(25_000_000_000));
        assert_eq!(parse_go_duration_nanos("1.5h"), Some(5_400_000_000_000));
        assert_eq!(parse_go_duration_nanos("1h30m"), Some(5_400_000_000_000));
        assert_eq!(parse_go_duration_nanos("0"), Some(0));
        assert_eq!(parse_go_duration_nanos("-5s"), Some(-5_000_000_000));
        assert_eq!(parse_go_duration_nanos(""), None);
        assert_eq!(parse_go_duration_nanos("10"), None); // no unit
        assert_eq!(parse_go_duration_nanos("abc"), None);
    }

    #[test]
    fn expand_tilde_expands_home_prefix_only() {
        assert_eq!(expand_tilde("/abs/path"), "/abs/path");
        assert_eq!(expand_tilde("plain"), "plain");
        let home = user_home_dir();
        assert_eq!(
            expand_tilde("~/x/y"),
            home.join("x/y").to_string_lossy().into_owned()
        );
    }

    // ---- AWS config slice (mirror aws_backend_test.go / config_test.go) ----------

    fn sheds(entries: &[(&str, AwsShedConfig)]) -> BTreeMap<String, AwsShedConfig> {
        entries
            .iter()
            .map(|(k, v)| (k.to_string(), v.clone()))
            .collect()
    }

    fn servers(entries: &[(&str, AwsServerConfig)]) -> BTreeMap<String, AwsServerConfig> {
        entries
            .iter()
            .map(|(k, v)| (k.to_string(), v.clone()))
            .collect()
    }

    fn shed_role(role: &str) -> AwsShedConfig {
        AwsShedConfig {
            role: role.to_string(),
            ..Default::default()
        }
    }

    /// Mirrors `aws_backend_test.go:TestAWSResolve` — layering matrix, role + mode
    /// only (session_duration layering is exercised by the golden).
    #[test]
    fn aws_resolve_matrix() {
        struct Case {
            name: &'static str,
            cfg: AwsConfig,
            server: &'static str,
            shed: &'static str,
            want_role: &'static str,
            want_mode: &'static str,
        }
        let cases = vec![
            Case {
                name: "default role",
                cfg: AwsConfig {
                    default_role: "arn:aws:iam::123:role/default".into(),
                    ..Default::default()
                },
                server: "mini2",
                shed: "my-shed",
                want_role: "arn:aws:iam::123:role/default",
                want_mode: AWS_MODE_ASSUME_ROLE,
            },
            Case {
                name: "per-server override",
                cfg: AwsConfig {
                    default_role: "arn:aws:iam::123:role/default".into(),
                    servers: servers(&[(
                        "mini2",
                        AwsServerConfig {
                            default_role: "arn:aws:iam::123:role/mini2".into(),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                server: "mini2",
                shed: "my-shed",
                want_role: "arn:aws:iam::123:role/mini2",
                want_mode: AWS_MODE_ASSUME_ROLE,
            },
            Case {
                name: "per-server-per-shed override wins",
                cfg: AwsConfig {
                    default_role: "arn:aws:iam::123:role/default".into(),
                    servers: servers(&[(
                        "mini2",
                        AwsServerConfig {
                            default_role: "arn:aws:iam::123:role/mini2".into(),
                            sheds: sheds(&[("web", shed_role("arn:aws:iam::123:role/web"))]),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                server: "mini2",
                shed: "web",
                want_role: "arn:aws:iam::123:role/web",
                want_mode: AWS_MODE_ASSUME_ROLE,
            },
            Case {
                name: "same shed name on different server is isolated",
                cfg: AwsConfig {
                    default_role: "arn:aws:iam::123:role/default".into(),
                    servers: servers(&[(
                        "mini2",
                        AwsServerConfig {
                            sheds: sheds(&[("web", shed_role("arn:aws:iam::123:role/mini2-web"))]),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                server: "mini3",
                shed: "web",
                want_role: "arn:aws:iam::123:role/default",
                want_mode: AWS_MODE_ASSUME_ROLE,
            },
            Case {
                name: "no config normalizes to assume-role",
                cfg: AwsConfig::default(),
                server: "mini2",
                shed: "my-shed",
                want_role: "",
                want_mode: AWS_MODE_ASSUME_ROLE,
            },
            Case {
                name: "top-level passthrough",
                cfg: AwsConfig {
                    mode: AWS_MODE_PASSTHROUGH.into(),
                    ..Default::default()
                },
                server: "mini2",
                shed: "my-shed",
                want_role: "",
                want_mode: AWS_MODE_PASSTHROUGH,
            },
            Case {
                name: "server-level passthrough ignores role",
                cfg: AwsConfig {
                    default_role: "arn:aws:iam::123:role/default".into(),
                    servers: servers(&[(
                        "mini2",
                        AwsServerConfig {
                            mode: AWS_MODE_PASSTHROUGH.into(),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                server: "mini2",
                shed: "web",
                want_role: "arn:aws:iam::123:role/default",
                want_mode: AWS_MODE_PASSTHROUGH,
            },
            Case {
                name: "child role under passthrough parent stays passthrough",
                cfg: AwsConfig {
                    servers: servers(&[(
                        "mini2",
                        AwsServerConfig {
                            mode: AWS_MODE_PASSTHROUGH.into(),
                            sheds: sheds(&[("web", shed_role("arn:aws:iam::123:role/web"))]),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                server: "mini2",
                shed: "web",
                want_role: "arn:aws:iam::123:role/web",
                want_mode: AWS_MODE_PASSTHROUGH,
            },
            Case {
                name: "child assume-role overrides passthrough parent",
                cfg: AwsConfig {
                    mode: AWS_MODE_PASSTHROUGH.into(),
                    servers: servers(&[(
                        "mini2",
                        AwsServerConfig {
                            sheds: sheds(&[(
                                "scoped",
                                AwsShedConfig {
                                    mode: AWS_MODE_ASSUME_ROLE.into(),
                                    role: "arn:aws:iam::123:role/scoped".into(),
                                    ..Default::default()
                                },
                            )]),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                server: "mini2",
                shed: "scoped",
                want_role: "arn:aws:iam::123:role/scoped",
                want_mode: AWS_MODE_ASSUME_ROLE,
            },
        ];
        for c in cases {
            let got = c.cfg.resolve(c.server, c.shed);
            assert_eq!(got.role, c.want_role, "{}: role", c.name);
            assert_eq!(got.mode, c.want_mode, "{}: mode", c.name);
        }
    }

    /// Mirrors `aws_backend_test.go:TestAWSEnabled`.
    #[test]
    fn aws_enabled_matrix() {
        let cases: Vec<(&str, AwsConfig, bool)> = vec![
            ("empty", AwsConfig::default(), false),
            (
                "explicit assume-role, no role",
                AwsConfig {
                    mode: AWS_MODE_ASSUME_ROLE.into(),
                    ..Default::default()
                },
                false,
            ),
            (
                "default role",
                AwsConfig {
                    default_role: "x".into(),
                    ..Default::default()
                },
                true,
            ),
            (
                "top-level passthrough",
                AwsConfig {
                    mode: AWS_MODE_PASSTHROUGH.into(),
                    ..Default::default()
                },
                true,
            ),
            (
                "server default role",
                AwsConfig {
                    servers: servers(&[(
                        "m",
                        AwsServerConfig {
                            default_role: "x".into(),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                true,
            ),
            (
                "server passthrough",
                AwsConfig {
                    servers: servers(&[(
                        "m",
                        AwsServerConfig {
                            mode: AWS_MODE_PASSTHROUGH.into(),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                true,
            ),
            (
                "shed role",
                AwsConfig {
                    servers: servers(&[(
                        "m",
                        AwsServerConfig {
                            sheds: sheds(&[("s", shed_role("x"))]),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                true,
            ),
            (
                "shed passthrough",
                AwsConfig {
                    servers: servers(&[(
                        "m",
                        AwsServerConfig {
                            sheds: sheds(&[(
                                "s",
                                AwsShedConfig {
                                    mode: AWS_MODE_PASSTHROUGH.into(),
                                    ..Default::default()
                                },
                            )]),
                            ..Default::default()
                        },
                    )]),
                    ..Default::default()
                },
                true,
            ),
        ];
        for (name, cfg, want) in cases {
            assert_eq!(cfg.enabled(), want, "{name}");
        }
    }

    /// Mirrors `aws_backend_test.go:TestMixedModeResolve`.
    #[test]
    fn aws_mixed_mode_resolve() {
        let cfg = AwsConfig {
            default_role: "arn:aws:iam::111:role/dev".into(),
            servers: servers(&[(
                "mini2",
                AwsServerConfig {
                    sheds: sheds(&[
                        (
                            "sso-app",
                            AwsShedConfig {
                                mode: AWS_MODE_PASSTHROUGH.into(),
                                ..Default::default()
                            },
                        ),
                        ("scoped-app", shed_role("arn:aws:iam::111:role/scoped")),
                    ]),
                    ..Default::default()
                },
            )]),
            ..Default::default()
        };
        assert_eq!(cfg.resolve("mini2", "sso-app").mode, AWS_MODE_PASSTHROUGH);
        let scoped = cfg.resolve("mini2", "scoped-app");
        assert_eq!(scoped.mode, AWS_MODE_ASSUME_ROLE);
        assert_eq!(scoped.role, "arn:aws:iam::111:role/scoped");
    }

    /// Mirrors the parse halves of `config_test.go:TestLoadConfigAWS` +
    /// `TestLoadConfigDefaults` (Validate parity is sub-plan 5).
    #[test]
    fn aws_load_and_defaults() {
        // TestLoadConfigAWS: explicit values + nested per-shed overrides parse.
        let cfg = HostAgentConfig::parse(
            "\
server: http://localhost:8080
aws:
  source_profile: staging
  default_role: arn:aws:iam::123456789012:role/dev
  session_duration: 2h
  cache_refresh_before: 10m
  approval:
    policy: approve-all
  servers:
    mini2:
      sheds:
        sso-app:
          mode: passthrough
        my-service:
          role: arn:aws:iam::123456789012:role/my-service
",
        );
        let aws = &cfg.aws;
        assert_eq!(aws.source_profile, "staging");
        assert_eq!(aws.default_role, "arn:aws:iam::123456789012:role/dev");
        assert_eq!(aws.session_duration, "2h");
        assert_eq!(aws.cache_refresh_before, "10m");
        // The aws.approval.policy gate still resolves via the existing accessor.
        assert_eq!(cfg.effective_policy(NS_AWS_CREDENTIALS), "approve-all");
        let sheds = &aws.servers["mini2"].sheds;
        assert_eq!(sheds["sso-app"].mode, AWS_MODE_PASSTHROUGH);
        assert_eq!(sheds["my-service"].role, "arn:aws:iam::123456789012:role/my-service");

        // TestLoadConfigDefaults: with no aws block the load defaults apply.
        let defaults = HostAgentConfig::parse("server: http://localhost:8080\n");
        assert_eq!(defaults.aws.source_profile, "default");
        assert_eq!(defaults.aws.session_duration, "1h");
        assert_eq!(defaults.aws.cache_refresh_before, "5m");
    }

    /// The Rust half of the `aws_resolve` golden — reads the SAME shared fixture the
    /// Go runner reads (`cmd/shed-host-agent/golden_test.go:TestGoldenAWSResolve`),
    /// so the two impls can't drift together on the layering / defaults / enabled
    /// semantics. Lives in-crate (the `load_discovered_servers` precedent) because
    /// this is a binary crate with no lib.
    #[test]
    fn golden_aws_resolve() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/aws_resolve.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let cfg = HostAgentConfig::parse(v["config_yaml"].as_str().unwrap());
            let aws = &cfg.aws;
            assert_eq!(
                aws.enabled(),
                v["enabled"].as_bool().unwrap(),
                "enabled {name:?}"
            );
            let d = &v["defaults"];
            assert_eq!(
                aws.source_profile,
                d["source_profile"].as_str().unwrap(),
                "source_profile {name:?}"
            );
            assert_eq!(
                aws.session_duration,
                d["session_duration"].as_str().unwrap(),
                "session_duration {name:?}"
            );
            assert_eq!(
                aws.cache_refresh_before,
                d["cache_refresh_before"].as_str().unwrap(),
                "cache_refresh_before {name:?}"
            );
            for q in v["queries"].as_array().unwrap() {
                let r = aws.resolve(q["server"].as_str().unwrap(), q["shed"].as_str().unwrap());
                assert_eq!(r.role, q["role"].as_str().unwrap(), "role {name:?}");
                assert_eq!(r.mode, q["mode"].as_str().unwrap(), "mode {name:?}");
                assert_eq!(
                    r.session_duration,
                    q["session_duration"].as_str().unwrap(),
                    "session_duration {name:?}"
                );
            }
        }
    }
}
