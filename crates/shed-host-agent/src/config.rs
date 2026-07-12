//! Minimal host-agent config reader for slice 0 — deliberately scoped to exactly
//! what the LiveStatus self-report needs: the three approval policies
//! (`ssh/aws/docker .approval.policy`) plus `logging.{enabled,path}`. The full
//! config schema (discovery, per-server overrides, AWS/Docker resolution,
//! biometric scope/ttl, `Validate()`) is a later slice.
//!
//! Parsing: the `yaml_lite` mod exposes the same tiny `Node` model shed-core's
//! reader uses, but its parser is backed by `saphyr-parser` (a pure-Rust,
//! no-serde/no-C YAML-1.2 *event* stream). The repo's deliberate no-YAML-dep
//! posture targets the serde-based crates (`serde_yaml`/`serde_norway`); the
//! host-agent reader is the scoped exception — a real parser gains block
//! sequences, flow maps, and malformed-input detection the line/colon reader
//! could not, which the shipped `configs/extensions.example.yaml` (block-style
//! `registries:`) actually needs. See `crates/CLAUDE.md` for the carve-out;
//! shed-core's own `yaml_lite` (a separate crate, Swift byte-parity test) stays
//! hand-rolled.

use std::collections::BTreeMap;
use std::io;
use std::path::PathBuf;
use std::time::Duration;

/// The three credential namespaces, in the fixed status/gate order.
pub const NS_SSH_AGENT: &str = "ssh-agent";
pub const NS_AWS_CREDENTIALS: &str = "aws-credentials";
pub const NS_DOCKER_CREDENTIALS: &str = "docker-credentials";
/// The audit namespace stamped on every egress-control decision (mirror Go's
/// `namespaceEgress`). Unlike the three above it is NOT a plugin-bus namespace — the
/// egress consumer reads the read-only `GET /api/egress/stream` SSE route — so it is
/// deliberately absent from `BUS_NAMESPACES`. Consumed by `egress_audit_entry`
/// (`egress.rs`), NOT within `config.rs` itself, so the golden test's isolated
/// `#[path]`-included `config` module (which pulls neither) would otherwise flag it dead.
#[allow(dead_code)]
pub const NS_EGRESS: &str = "egress";

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
    /// The layered Docker registry-credential policy (`docker.{registries,
    /// allow_all,config_path,servers…}`), mirroring Go's `DockerConfig`. The
    /// `docker.approval.policy` gate is kept in `docker_policy` above (unchanged);
    /// this field carries only the allowlist/config-path knobs. Consumed by the
    /// Docker backend (`docker_backend.rs`, commit 2) + `main.rs` bus wiring
    /// (commit 3); in commit 1 only the config tests + the docker_resolve golden
    /// read it.
    pub docker: DockerConfig,
}

impl HostAgentConfig {
    /// Load + parse the config at `path` (tilde-expanded). A missing/unreadable
    /// file is an **error**: the daemon requires a config to know its policies, so
    /// it exits 1 rather than fail open (matches the Go daemon's `LoadConfig` →
    /// `os.Exit(1)` on any load error).
    pub fn load(path: &str) -> io::Result<HostAgentConfig> {
        let expanded = expand_tilde(path);
        let text = std::fs::read_to_string(&expanded)?;
        // Malformed YAML is a hard error (mirrors Go's `yaml.Unmarshal` →
        // `os.Exit(1)`); the daemon must not fail open with a half-read policy.
        Self::try_parse(&text).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))
    }

    /// Parse config text, returning an error on malformed YAML. Missing keys take
    /// their defaults (policies default to empty → deny-all effective; logging
    /// enabled true). This is the fallible path `load` uses; the infallible
    /// `HostAgentConfig::parse` convenience wraps it for known-good test literals.
    pub fn try_parse(text: &str) -> Result<HostAgentConfig, String> {
        let root = yaml_lite::parse(text)?;
        Ok(Self::from_root(&root))
    }

    /// Parse known-good config text, panicking on malformed YAML. A convenience for
    /// the in-crate tests + fixtures that pass literals known to parse; production
    /// load goes through [`HostAgentConfig::try_parse`] (via `load`), so this is
    /// only reached from test builds — so it is `#[cfg(test)]`-gated out of the
    /// production binary entirely rather than kept as a dead-code-allowed symbol.
    #[cfg(test)]
    pub fn parse(text: &str) -> HostAgentConfig {
        Self::try_parse(text).expect("yaml fixture parses")
    }

    /// Build the config from an already-parsed `Node` tree (the `Node → config`
    /// half of `try_parse`).
    fn from_root(root: &yaml_lite::Node) -> HostAgentConfig {
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
            aws: AwsConfig::from_node(root),
            docker: DockerConfig::from_node(root),
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
    /// absent `source_profile` → `"default"`, `session_duration` → `"1h"`,
    /// `cache_refresh_before` → `"5m"`.
    ///
    /// Null-vs-empty parity (matches Go's `yaml.Unmarshal`-over-`DefaultConfig`
    /// merge, verified against `LoadConfig`): an ABSENT key OR a bare `key:` (YAML
    /// null) leaves the `DefaultConfig` default in place, while an EXPLICIT empty
    /// string `key: ""` overwrites it with `""` (Go would then call STS with
    /// profile `""`). The saphyr swap makes this representable — a null value parses
    /// to [`yaml_lite::Node::Null`] and a quoted empty to `Scalar("")` — so the
    /// `scalar_or_default` helper branches on the node shape, not on emptiness.
    /// (Pinned cross-language by the `source_profile` vectors in the `aws_resolve`
    /// golden.)
    ///
    /// `aws.sheds` (Go's removed global per-shed map) is intentionally NOT read: it
    /// is parse-and-ignore for resolution. Go's `AWSConfig.validate` rejects it with
    /// a migration message; that rejection is sub-plan 5's `validate()` (commit 2).
    fn from_node(root: &yaml_lite::Node) -> AwsConfig {
        use yaml_lite::Node;
        let aws = root.as_map().and_then(|m| m.get("aws")).and_then(Node::as_map);
        // A non-defaulted scalar: absent / null / non-scalar → "".
        let scalar = |key: &str| -> String {
            aws.and_then(|m| m.get(key))
                .and_then(Node::as_scalar)
                .unwrap_or("")
                .to_string()
        };
        // A defaulted scalar with Go's null-vs-empty semantics: an explicit
        // `Scalar` (including `""`) is kept verbatim; absent / null / non-scalar
        // falls back to the DefaultConfig default.
        let scalar_or_default = |key: &str, d: &str| -> String {
            match aws.and_then(|m| m.get(key)) {
                Some(Node::Scalar(s)) => s.clone(),
                _ => d.to_string(),
            }
        };

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
            source_profile: scalar_or_default("source_profile", "default"),
            default_role: scalar("default_role"),
            mode: scalar("mode"),
            session_duration: scalar_or_default("session_duration", "1h"),
            cache_refresh_before: scalar_or_default("cache_refresh_before", "5m"),
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

/// The layered Docker registry-credential config (`docker.*`), a faithful port of
/// Go's `DockerConfig` (`config.go:198-254`). The top-level fields are the defaults;
/// `servers` carries per-server (and per-shed) overrides layered over them.
/// `config_path` (the Docker `config.json` override) is process-global. The
/// `docker.approval.policy` gate lives on `HostAgentConfig.docker_policy`, not here.
///
/// Unlike [`AwsConfig`], Docker has **no `enabled()` method and no load-defaults**:
/// Go's `DockerConfig` carries no `Enabled()` and `DefaultConfig` sets no Docker
/// fields, so an absent/empty `docker:` block yields an empty-registries,
/// `allow_all: false`, empty-`config_path` config that STILL constructs a live
/// backend (which then denies every registry). That asymmetry — AWS gates itself
/// off when unconfigured, Docker stays on — is the crux of the Docker slice and is
/// carried by the constructor (commit 2), not by this config type.
///
/// Built by [`DockerConfig::from_node`] (called from `HostAgentConfig::parse`) and
/// consumed by the Docker backend (`docker_backend.rs`, commit 2) + `main.rs` bus
/// wiring (commit 3).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct DockerConfig {
    /// Registry hostnames to allow by default. A plain `Vec` (Go's top-level
    /// `[]string`): absent → empty, which — with `allow_all: false` — denies all.
    pub registries: Vec<String>,
    /// Bypass the allowlist entirely (Go's top-level `bool` default).
    pub allow_all: bool,
    /// Override the Docker `config.json` path (`~/`-expansion is applied by the
    /// backend constructor, mirroring Go's `NewDockerBackend`, not here — Go's
    /// `LoadConfig` likewise stores it raw).
    pub config_path: String,
    /// Per-server overrides, each optionally with per-shed nesting.
    pub servers: BTreeMap<String, DockerServerConfig>,
}

/// Per-server Docker overrides (Go's `DockerServerConfig`). `registries` and
/// `allow_all` are BOTH optional so an unset value **inherits** the parent rather
/// than forcing empty/false. `registries: Option<Vec<String>>` is the load-bearing
/// choice (mirroring Go's `sv.Registries != nil` replace check): `None` = inherit,
/// `Some(vec![])` = **replace-with-empty** (the child DENIES ALL — a security
/// lockdown a plain `Vec` would silently fold into "inherit"), `Some(vec![..])` =
/// replace. `allow_all: Option<bool>` mirrors Go's `*bool`: `None` = inherit,
/// `Some(false)` = force-off, `Some(true)` = force-on.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct DockerServerConfig {
    pub registries: Option<Vec<String>>,
    pub allow_all: Option<bool>,
    pub sheds: BTreeMap<String, DockerShedConfig>,
}

/// Per-shed Docker overrides (Go's `DockerShedConfig`). Same `Option` inherit/
/// replace/force semantics as [`DockerServerConfig`].
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct DockerShedConfig {
    pub registries: Option<Vec<String>>,
    pub allow_all: Option<bool>,
}

/// The effective Docker policy for a single `(server, shed)` pair (Go's
/// `ResolvedDocker`). `registry_count` in the backend's `Status` is just
/// `registries.len()`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedDocker {
    pub registries: Vec<String>,
    pub allow_all: bool,
}

impl DockerConfig {
    /// Build the Docker slice from the parsed config tree. Reads a **list**
    /// (`registries`, via [`yaml_lite::Node::as_scalar_list`]: absent key → `None`,
    /// present `[]` → `Some(vec![])`, present `[a,b]` → `Some(vec![a,b])`) and a
    /// **pointer bool** (`allow_all`: absent → `None`, present → `Some(_)`),
    /// mirroring Go's `[]string`/`*bool` fields. The exact `AwsConfig::from_node`
    /// walk, but sequence-aware.
    ///
    /// No load-defaults are applied (Go's `DefaultConfig` sets no Docker fields):
    /// an absent `docker:` block yields `DockerConfig::default()` — empty
    /// registries, `allow_all: false`, empty `config_path` — the unconfigured
    /// deny-all backend.
    fn from_node(root: &yaml_lite::Node) -> DockerConfig {
        use yaml_lite::Node;
        let docker = root
            .as_map()
            .and_then(|m| m.get("docker"))
            .and_then(Node::as_map);

        let mut servers = BTreeMap::new();
        if let Some(servers_map) = docker.and_then(|m| m.get("servers")).and_then(Node::as_map) {
            for (name, entry) in servers_map {
                let Some(fields) = entry.as_map() else {
                    continue;
                };
                let mut sheds = BTreeMap::new();
                if let Some(sheds_map) = fields.get("sheds").and_then(Node::as_map) {
                    for (sname, sentry) in sheds_map {
                        let Some(sf) = sentry.as_map() else { continue };
                        sheds.insert(
                            sname.clone(),
                            DockerShedConfig {
                                registries: sf.get("registries").and_then(Node::as_scalar_list),
                                allow_all: opt_bool(sf.get("allow_all")),
                            },
                        );
                    }
                }
                servers.insert(
                    name.clone(),
                    DockerServerConfig {
                        registries: fields.get("registries").and_then(Node::as_scalar_list),
                        allow_all: opt_bool(fields.get("allow_all")),
                        sheds,
                    },
                );
            }
        }

        DockerConfig {
            registries: docker
                .and_then(|m| m.get("registries"))
                .and_then(Node::as_scalar_list)
                .unwrap_or_default(),
            allow_all: opt_bool(docker.and_then(|m| m.get("allow_all"))).unwrap_or(false),
            config_path: docker
                .and_then(|m| m.get("config_path"))
                .and_then(Node::as_scalar)
                .unwrap_or("")
                .to_string(),
            servers,
        }
    }

    /// Layer Docker overrides for a `(server, shed)` pair, most specific wins:
    /// top-level defaults → `servers[server]` → `servers[server].sheds[shed]`. A
    /// **`Some` registries list REPLACES** (does not merge) the inherited one —
    /// `Some(vec![])` replaces with empty (child denies all); an unset (`None`)
    /// `registries`/`allow_all` inherits. A faithful port of Go's
    /// `DockerConfig.Resolve` (`config.go:233-254`, the `sv.Registries != nil` /
    /// `sv.AllowAll != nil` checks). Consumed by the Docker backend's
    /// `get_credentials`/`list_credentials`/`status` (`docker_backend.rs`, commit 2)
    /// + the config unit tests + the `docker_resolve` golden.
    pub fn resolve(&self, server: &str, shed: &str) -> ResolvedDocker {
        let mut registries = self.registries.clone();
        let mut allow_all = self.allow_all;
        if let Some(sv) = self.servers.get(server) {
            if let Some(r) = &sv.registries {
                registries = r.clone();
            }
            if let Some(a) = sv.allow_all {
                allow_all = a;
            }
            if let Some(sc) = sv.sheds.get(shed) {
                if let Some(r) = &sc.registries {
                    registries = r.clone();
                }
                if let Some(a) = sc.allow_all {
                    allow_all = a;
                }
            }
        }
        ResolvedDocker {
            registries,
            allow_all,
        }
    }
}

/// Read an optional YAML bool leaf, mirroring Go's `*bool` decode: an absent node
/// → `None` (inherit), a present scalar → `Some(scalar == "true")` (so
/// `allow_all: false` is `Some(false)` — force-off — not inherit). A present
/// non-scalar (e.g. a map) → `None`.
fn opt_bool(node: Option<&yaml_lite::Node>) -> Option<bool> {
    node.and_then(yaml_lite::Node::as_scalar)
        .map(|s| s == "true")
}

/// The host-agent config parser, exposing a tiny `Node` model (nested maps,
/// scalars, sequences, and an explicit null) over a real YAML-1.2 parser.
///
/// The parser is driven off `saphyr-parser`'s low-level *event* stream (see the
/// module-level carve-out note at the top of this file): [`parse`] pulls
/// `StreamStart`/`DocumentStart`/`MappingStart`/`Scalar`/… events and folds them
/// into `Node`. Only the FIRST document is consumed (mirrors Go's
/// `yaml.Unmarshal`); duplicate map keys and malformed input return `Err`
/// (matching yaml.v3). The old hand-rolled line/colon reader is gone — with it the
/// silent block-sequence drop, the flow-map-as-opaque-scalar gap, and the
/// can't-detect-malformed gap that `config.rs`/`README` tracked as divergences.
pub(crate) mod yaml_lite {
    use saphyr_parser::{Event, Parser, ScalarStyle};
    use std::borrow::Cow;
    use std::collections::HashMap;

    #[derive(Debug, Clone, PartialEq, Eq)]
    pub enum Node {
        Map(HashMap<String, Node>),
        Scalar(String),
        /// A YAML sequence (array), from either flow style (`[a, b, c]`) or block
        /// style (`- a`\n`- b`). Both fold to the same node now that a real parser
        /// backs the reader; the shipped `configs/extensions.example.yaml` uses the
        /// block form for `docker.registries`.
        Sequence(Vec<Node>),
        /// An explicit YAML null: a bare `key:`, `key: ~`, or `key: null`. Distinct
        /// from an empty string `key: ""` (a `Scalar("")`) so the readers can port
        /// Go's null-vs-empty merge semantics — a null leaves a `DefaultConfig`
        /// default in place; an empty string overwrites it.
        Null,
    }

    impl Node {
        pub fn as_scalar(&self) -> Option<&str> {
            match self {
                Node::Scalar(s) => Some(s),
                _ => None,
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
                _ => None,
            }
        }

        /// The underlying slice when this node is a `Sequence`. Only the config
        /// unit tests reach it directly today (the Docker `registries` reader uses
        /// [`Node::as_scalar_list`]); kept as a first-class accessor because the
        /// sequence node is reusable by later config slices.
        #[allow(dead_code)]
        pub fn as_seq(&self) -> Option<&[Node]> {
            match self {
                Node::Sequence(v) => Some(v),
                _ => None,
            }
        }

        /// A `Sequence` flattened to its scalar items (non-scalar items are
        /// dropped) — the convenience the Docker `registries` allowlist reader
        /// wants. `None` when this node is not a sequence, which lets the reader
        /// distinguish an ABSENT key (`get` → `None`) from a present empty list
        /// (`[]` → `Some(vec![])`) — the load-bearing nil-vs-empty distinction
        /// Go's `[]string != nil` replace check depends on.
        pub fn as_scalar_list(&self) -> Option<Vec<String>> {
            match self {
                Node::Sequence(v) => Some(
                    v.iter()
                        .filter_map(|n| n.as_scalar().map(str::to_string))
                        .collect(),
                ),
                _ => None,
            }
        }

        /// Whether this node is a map containing `key` (as a scalar or a nested
        /// block). Used to detect the presence of a top-level block like
        /// `discovery:` whose contents this minimal reader doesn't yet parse.
        pub fn has_key(&self, key: &str) -> bool {
            match self {
                Node::Map(m) => m.contains_key(key),
                _ => false,
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

    /// Parse config text into a [`Node`] tree, consuming ONLY the first YAML
    /// document (mirrors Go's `yaml.Unmarshal`, which decodes the first document
    /// and ignores the rest). Returns `Err(message)` on malformed input or a
    /// duplicate map key — the cases yaml.v3 rejects and the old line/colon reader
    /// silently swallowed. An empty input (no document) yields an empty map.
    pub fn parse(text: &str) -> Result<Node, String> {
        let mut builder = Builder {
            parser: Parser::new_from_str(text),
        };
        loop {
            match builder.next_event()? {
                // Skip stream/framing events until the first document's root.
                Event::StreamStart | Event::Nothing => continue,
                Event::StreamEnd => return Ok(Node::Map(HashMap::new())),
                Event::DocumentStart(_) => {
                    let root_ev = builder.next_event()?;
                    // A document with no content node (e.g. `---` alone) → null.
                    return match root_ev {
                        Event::DocumentEnd | Event::StreamEnd => Ok(Node::Null),
                        other => builder.build_node(other),
                    };
                }
                other => return Err(format!("unexpected top-level YAML event: {other:?}")),
            }
        }
    }

    /// Drives the saphyr event stream into the `Node` model.
    struct Builder<'a> {
        parser: Parser<'a, saphyr_parser::StrInput<'a>>,
    }

    impl<'a> Builder<'a> {
        /// Pull the next event, flattening a scan error or a truncated stream into
        /// an `Err` message. The returned `Event` borrows from the parser INPUT
        /// (`'a`), not from `&mut self`, so a built child node can be assembled while
        /// the next event is pulled.
        fn next_event(&mut self) -> Result<Event<'a>, String> {
            match self.parser.next_event() {
                Some(Ok((ev, _span))) => Ok(ev),
                Some(Err(e)) => Err(e.to_string()),
                None => Err("unexpected end of YAML event stream".to_string()),
            }
        }

        /// Build a node from `first_ev`, recursing into maps/sequences.
        fn build_node(&mut self, first_ev: Event<'a>) -> Result<Node, String> {
            match first_ev {
                Event::Scalar(value, style, _anchor, _tag) => Ok(scalar_node(value, style)),
                Event::SequenceStart(..) => self.build_sequence(),
                Event::MappingStart(..) => self.build_mapping(),
                Event::Alias(_) => {
                    Err("YAML anchors/aliases are not supported in host-agent config".to_string())
                }
                other => Err(format!("unexpected YAML node event: {other:?}")),
            }
        }

        /// Fold a sequence's items until `SequenceEnd`.
        fn build_sequence(&mut self) -> Result<Node, String> {
            let mut items = Vec::new();
            loop {
                match self.next_event()? {
                    Event::SequenceEnd => return Ok(Node::Sequence(items)),
                    ev => items.push(self.build_node(ev)?),
                }
            }
        }

        /// Fold a mapping's key/value pairs until `MappingEnd`. A duplicate key is
        /// an error (yaml.v3 rejects it; a silent last-wins insert would over- or
        /// under-grant a policy). Non-scalar keys are rejected (documented-open).
        fn build_mapping(&mut self) -> Result<Node, String> {
            let mut map = HashMap::new();
            loop {
                let key = match self.next_event()? {
                    Event::MappingEnd => return Ok(Node::Map(map)),
                    Event::Scalar(value, _style, _anchor, _tag) => value.into_owned(),
                    other => {
                        return Err(format!("unsupported non-scalar map key: {other:?}"));
                    }
                };
                let value_ev = self.next_event()?;
                let value = self.build_node(value_ev)?;
                if map.insert(key.clone(), value).is_some() {
                    return Err(format!("duplicate map key {key:?}"));
                }
            }
        }
    }

    /// Map a scalar event to a node, resolving a plain YAML null (`` / `~` /
    /// `null`) to [`Node::Null`]. A quoted empty (`""`, style `DoubleQuoted`/
    /// `SingleQuoted`) stays `Scalar("")` — the null-vs-empty distinction the AWS
    /// reader depends on. saphyr has already unquoted/unescaped the content.
    fn scalar_node(value: Cow<'_, str>, style: ScalarStyle) -> Node {
        if style == ScalarStyle::Plain && is_yaml_null(&value) {
            Node::Null
        } else {
            // Move the String out of an owned Cow (escaped/quoted scalars) for
            // free; allocate only for a borrowed one — mirrors the map-key path's
            // `value.into_owned()` rather than an unconditional `to_string()`.
            Node::Scalar(value.into_owned())
        }
    }

    /// The YAML-1.2 core-schema null tokens (only meaningful for a PLAIN scalar).
    /// An empty plain node surfaces as `~` from saphyr, but `""` is included
    /// defensively.
    fn is_yaml_null(s: &str) -> bool {
        matches!(s, "" | "~" | "null" | "Null" | "NULL")
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

    // ---- yaml_lite flow-list sequence (the Docker registries prerequisite) -------

    /// The new `Node::Sequence` flow-list parser: `[a, b]`, empty `[]`, single item,
    /// quoted items, and internal whitespace, plus the absent-vs-empty distinction
    /// the Docker registries reader depends on.
    #[test]
    fn yaml_lite_sequence_flow_list() {
        use yaml_lite::Node;
        let root = yaml_lite::parse(
            "\
a: [x, y, z]
b: []
c: [only]
d: [\"q1\", 'q2']
e: [ spaced ,  items ]
f: scalar
g:
  nested: [deep]
",
        )
        .expect("valid flow-list config parses");
        let m = root.as_map().unwrap();
        // Multi-item flow list.
        assert_eq!(
            m["a"].as_scalar_list().unwrap(),
            vec!["x".to_string(), "y".to_string(), "z".to_string()]
        );
        // Empty list is Some(empty) — present, not absent.
        assert_eq!(m["b"].as_scalar_list().unwrap(), Vec::<String>::new());
        assert!(matches!(&m["b"], Node::Sequence(v) if v.is_empty()));
        // Single item.
        assert_eq!(m["c"].as_scalar_list().unwrap(), vec!["only".to_string()]);
        // Quoted items are unquoted.
        assert_eq!(
            m["d"].as_scalar_list().unwrap(),
            vec!["q1".to_string(), "q2".to_string()]
        );
        // Internal whitespace is trimmed.
        assert_eq!(
            m["e"].as_scalar_list().unwrap(),
            vec!["spaced".to_string(), "items".to_string()]
        );
        // A scalar is NOT a sequence.
        assert!(m["f"].as_scalar_list().is_none());
        assert_eq!(m["f"].as_scalar(), Some("scalar"));
        // Nested block still parses; the leaf inside it is a sequence.
        assert_eq!(
            m["g"].as_map().unwrap()["nested"].as_scalar_list().unwrap(),
            vec!["deep".to_string()]
        );
        // as_seq exposes the raw nodes.
        assert_eq!(m["a"].as_seq().unwrap().len(), 3);
        // An ABSENT key is None — the inherit signal (distinct from present `[]`).
        assert!(m.get("absent").is_none());
    }

    // ---- yaml_lite: the saphyr-backed parser (block seqs, flow maps, null-vs-empty,
    //      malformed/duplicate-key detection, first-document-only) -------------------

    /// Block-style sequences now parse — the load-bearing new deliverable. The
    /// shipped `configs/extensions.example.yaml` writes `docker.registries` as a
    /// `- item` block list; the old line/colon reader dropped the colonless items so
    /// `docker.registries` parsed EMPTY. Cross-language pinned by the block-seq
    /// vector in the `docker_resolve` golden.
    #[test]
    fn yaml_lite_block_sequence() {
        let cfg = HostAgentConfig::parse(
            "\
docker:
  registries:
    - index.docker.io
    - ghcr.io
  approval:
    policy: approve-all
",
        );
        assert_eq!(
            cfg.docker.registries,
            vec!["index.docker.io".to_string(), "ghcr.io".to_string()]
        );
    }

    /// The shipped example config's block-style `registries:` parses to the two
    /// hostnames Go's `TestExampleConfigIsValid` asserts — the real uncaught
    /// divergence on the product's own default config. (Full validate parity of the
    /// example config is commit 2; this pins only the parse-level block-seq win.)
    #[test]
    fn example_config_registries_parse() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../configs/extensions.example.yaml");
        let text = std::fs::read_to_string(&path).expect("read example config");
        let cfg = HostAgentConfig::try_parse(&text).expect("example config parses");
        assert_eq!(
            cfg.docker.registries,
            vec!["index.docker.io".to_string(), "ghcr.io".to_string()]
        );
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), "approve-all");
    }

    /// A flow-style map (`ssh: { approval: { policy: X } }`) parses to the SAME
    /// nested `Node::Map` structure as the block form — the old reader treated the
    /// whole `{ ... }` as an opaque scalar (→ all deny-all).
    #[test]
    fn yaml_lite_inline_flow_map() {
        let flow = HostAgentConfig::parse("ssh: { approval: { policy: shed-desktop } }\n");
        let block = HostAgentConfig::parse("ssh:\n  approval:\n    policy: shed-desktop\n");
        assert_eq!(flow.effective_policy(NS_SSH_AGENT), "shed-desktop");
        assert_eq!(
            flow.effective_policy(NS_SSH_AGENT),
            block.effective_policy(NS_SSH_AGENT)
        );
    }

    /// Flow sequences: empty `[]` is a present-but-empty `Sequence(vec![])` (NOT
    /// absent) so the Docker `Option<Vec>` replace semantics see a deny-all
    /// lockdown; a non-empty list carries its items.
    #[test]
    fn yaml_lite_flow_sequence_empty_and_nonempty() {
        use yaml_lite::Node;
        let root = yaml_lite::parse("a: []\nb: [x, y]\n").expect("parses");
        let m = root.as_map().unwrap();
        assert!(matches!(&m["a"], Node::Sequence(v) if v.is_empty()));
        assert_eq!(m["a"].as_scalar_list().unwrap(), Vec::<String>::new());
        assert_eq!(
            m["b"].as_scalar_list().unwrap(),
            vec!["x".to_string(), "y".to_string()]
        );
    }

    /// Null-vs-empty scalar recovery (the M1 adapter contract + C1): a bare `key:`
    /// (YAML null) is `Node::Null`; a quoted empty `key: ""` is `Scalar("")`. A
    /// null-valued key is STILL inserted into the map (so `has_key` stays true for a
    /// bare `discovery:`). The Go-golden `source_profile` cross-language vector
    /// (`aws_resolve` golden) pins the resolved-config half.
    #[test]
    fn yaml_lite_null_vs_empty_scalar() {
        use yaml_lite::Node;
        let root = yaml_lite::parse(
            "\
bare:
tilde: ~
word: null
empty: \"\"
value: hi
discovery:
",
        )
        .expect("parses");
        let m = root.as_map().unwrap();
        assert_eq!(m["bare"], Node::Null);
        assert_eq!(m["tilde"], Node::Null);
        assert_eq!(m["word"], Node::Null);
        assert_eq!(m["empty"], Node::Scalar(String::new()));
        assert_eq!(m["value"], Node::Scalar("hi".to_string()));
        // A null-valued key is inserted — presence detection still works.
        assert!(root.has_key("discovery"));
        assert!(root.has_key("bare"));
        // as_scalar on Null is None → get_path yields None → the reader defaults.
        assert_eq!(m["bare"].as_scalar(), None);

        // The resolved-config half: bare `source_profile:` (null) keeps the
        // DefaultConfig default; explicit `source_profile: ""` keeps the empty
        // string. (Verified against Go's LoadConfig; goldened cross-language.)
        let null_sp = HostAgentConfig::parse("aws:\n  source_profile:\n");
        assert_eq!(null_sp.aws.source_profile, "default");
        let empty_sp = HostAgentConfig::parse("aws:\n  source_profile: \"\"\n");
        assert_eq!(empty_sp.aws.source_profile, "");
    }

    /// A duplicate map key is an error (yaml.v3 rejects it; a silent last-wins
    /// insert would over/under-grant). Cross-language pinned `{valid:false}` in
    /// commit 2's validate golden.
    #[test]
    fn yaml_lite_duplicate_key_errors() {
        let err = yaml_lite::parse("server: http://a:8080\nserver: http://b:8080\n")
            .expect_err("duplicate key rejected");
        assert!(err.contains("duplicate map key"), "err = {err}");
        // Nested duplicate keys are caught too.
        assert!(
            yaml_lite::parse("aws:\n  mode: passthrough\n  mode: assume-role\n").is_err(),
            "nested duplicate key should error"
        );
    }

    /// Malformed input returns `Err` (the saphyr win the line/colon reader could
    /// not detect): top-level garbage and an unterminated flow bracket both fail,
    /// so `load` exits 1 like Go's `yaml.Unmarshal`.
    #[test]
    fn yaml_lite_parse_errors_on_malformed() {
        assert!(yaml_lite::parse("{{invalid yaml").is_err());
        assert!(yaml_lite::parse("docker:\n  registries: [only-this\n").is_err());
        assert!(yaml_lite::parse("a: b\n\tc: d\n").is_err()); // tab indentation
        // And `load` surfaces it as an error (main.rs exits 1).
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("bad.yaml");
        std::fs::write(&path, "{{invalid yaml").unwrap();
        assert!(HostAgentConfig::load(path.to_str().unwrap()).is_err());
    }

    /// Only the FIRST document is consumed (mirrors Go's `yaml.Unmarshal`): a
    /// multi-document stream resolves the first document's values and ignores the
    /// rest.
    #[test]
    fn yaml_lite_multi_doc_takes_first() {
        let cfg = HostAgentConfig::parse("server: http://a:8080\n---\nserver: http://b:8080\n");
        assert_eq!(cfg.server, "http://a:8080");
    }

    /// Empty / comment-only input yields an empty map (all defaults), unchanged from
    /// the old reader.
    #[test]
    fn yaml_lite_empty_and_comment_only() {
        assert_eq!(yaml_lite::parse("").unwrap(), yaml_lite::Node::Map(Default::default()));
        assert_eq!(
            yaml_lite::parse("# just a comment\n").unwrap(),
            yaml_lite::Node::Map(Default::default())
        );
    }

    // ---- Docker config slice (mirror config_discovery_test.go / config_test.go) ---

    /// Mirrors `config_discovery_test.go:TestDockerResolve`, extended to a full
    /// matrix over the `Option` inherit/replace/force semantics — Rust-stronger:
    /// Go has no `Resolve` layering test beyond the three subtests folded in here
    /// (absent→inherit, per-server pointer override, per-shed replace+force-false),
    /// plus the `Some(vec![])`→replace-with-empty (deny-all lockdown) and the
    /// `allow_all` `Option<bool>` None/false/true cases the golden also pins.
    #[test]
    fn docker_resolve_matrix() {
        // (shed-name, registries-override, allow_all-override) — a type alias keeps
        // the `sv` helper below under clippy::type_complexity.
        type ShedSpec<'a> = (&'a str, Option<Vec<&'a str>>, Option<bool>);
        fn sv(
            registries: Option<Vec<&str>>,
            allow_all: Option<bool>,
            sheds: &[ShedSpec],
        ) -> DockerServerConfig {
            DockerServerConfig {
                registries: registries.map(|v| v.iter().map(|s| s.to_string()).collect()),
                allow_all,
                sheds: sheds
                    .iter()
                    .map(|(name, r, a)| {
                        (
                            name.to_string(),
                            DockerShedConfig {
                                registries: r
                                    .as_ref()
                                    .map(|v| v.iter().map(|s| s.to_string()).collect()),
                                allow_all: *a,
                            },
                        )
                    })
                    .collect(),
            }
        }
        let want = |registries: &[&str], allow_all: bool| ResolvedDocker {
            registries: registries.iter().map(|s| s.to_string()).collect(),
            allow_all,
        };

        // The TestDockerResolve config: top-level [ghcr.io]/allow_all=false, server
        // mini2 forces allow_all=true, shed mini2/web replaces registries + forces
        // allow_all back to false.
        let cfg = DockerConfig {
            registries: vec!["ghcr.io".to_string()],
            allow_all: false,
            config_path: String::new(),
            servers: [(
                "mini2".to_string(),
                sv(
                    None,
                    Some(true),
                    &[("web", Some(vec!["reg.example.com"]), Some(false))],
                ),
            )]
            .into_iter()
            .collect(),
        };
        // defaults when no server override
        assert_eq!(cfg.resolve("mini3", "anything"), want(&["ghcr.io"], false));
        // per-server allow_all pointer override, registries inherited
        assert_eq!(cfg.resolve("mini2", "api"), want(&["ghcr.io"], true));
        // per-shed replaces registries + forces allow_all false
        assert_eq!(
            cfg.resolve("mini2", "web"),
            want(&["reg.example.com"], false)
        );

        // Some(vec![]) at the server tier → replace-with-empty (deny-all lockdown),
        // even though the top level allows ghcr.io. A sibling server inherits.
        let locked = DockerConfig {
            registries: vec!["ghcr.io".to_string(), "index.docker.io".to_string()],
            allow_all: false,
            config_path: String::new(),
            servers: [("locked".to_string(), sv(Some(vec![]), None, &[]))]
                .into_iter()
                .collect(),
        };
        assert_eq!(locked.resolve("locked", "x"), want(&[], false));
        assert_eq!(
            locked.resolve("other", "x"),
            want(&["ghcr.io", "index.docker.io"], false)
        );

        // Some(vec![]) at the SHED tier under an allow_all=true server → deny-all
        // lockdown for that one shed; the sibling shed inherits allow_all=true.
        let shed_lock = DockerConfig {
            registries: vec![],
            allow_all: true,
            config_path: String::new(),
            servers: [(
                "mini2".to_string(),
                sv(None, None, &[("locked", Some(vec![]), Some(false))]),
            )]
            .into_iter()
            .collect(),
        };
        assert_eq!(shed_lock.resolve("mini2", "locked"), want(&[], false));
        assert_eq!(shed_lock.resolve("mini2", "other"), want(&[], true));

        // Unconfigured (default) → empty registries, allow_all false (deny-all).
        assert_eq!(
            DockerConfig::default().resolve("any", "any"),
            want(&[], false)
        );
    }

    /// Mirrors the Docker parse rows of `config_test.go` (`TestLoadConfig:62`,
    /// `TestExampleConfigIsValid:276-281`): flow-list `registries` parse + the
    /// `docker.approval.policy` gate (still via the existing accessor). Validate
    /// biometric-reject parity is sub-plan 5.
    #[test]
    fn docker_load_parse() {
        // Single-item flow list + shed-desktop policy (TestLoadConfig header).
        let cfg = HostAgentConfig::parse(
            "\
docker:
  registries: [ghcr.io]
  approval:
    policy: shed-desktop
",
        );
        assert_eq!(cfg.docker.registries, vec!["ghcr.io".to_string()]);
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), "shed-desktop");
        assert!(!cfg.docker.allow_all);

        // Two-item flow list + allow_all + config_path + per-server/shed nesting.
        let cfg = HostAgentConfig::parse(
            "\
docker:
  registries: [index.docker.io, ghcr.io]
  allow_all: false
  config_path: /custom/config.json
  approval:
    policy: approve-all
  servers:
    mini2:
      allow_all: true
      sheds:
        web:
          registries: [reg.example.com]
",
        );
        assert_eq!(
            cfg.docker.registries,
            vec!["index.docker.io".to_string(), "ghcr.io".to_string()]
        );
        assert_eq!(cfg.docker.config_path, "/custom/config.json");
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), "approve-all");
        assert_eq!(cfg.docker.servers["mini2"].allow_all, Some(true));
        assert_eq!(
            cfg.docker.servers["mini2"].sheds["web"].registries,
            Some(vec!["reg.example.com".to_string()])
        );
        // A resolve through the parsed tree honors the pointer + replace semantics.
        assert_eq!(
            cfg.docker.resolve("mini2", "web"),
            ResolvedDocker {
                registries: vec!["reg.example.com".to_string()],
                allow_all: true,
            }
        );
    }

    /// Mirrors `config_test.go:TestLoadConfigDefaults:179` — an absent `docker:`
    /// block leaves the gate at deny-all and the resolution config empty (the
    /// unconfigured deny-all backend). No load-defaults are applied to Docker.
    #[test]
    fn docker_policy_default_deny_all() {
        let cfg = HostAgentConfig::parse("server: http://localhost:8080\n");
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), POLICY_DENY_ALL);
        assert_eq!(cfg.docker, DockerConfig::default());
        assert!(cfg.docker.registries.is_empty());
        assert!(!cfg.docker.allow_all);
        assert!(cfg.docker.config_path.is_empty());
    }

    /// The Rust half of the `docker_resolve` golden — reads the SAME shared fixture
    /// the Go runner reads (`cmd/shed-host-agent/golden_test.go:TestGoldenDockerResolve`,
    /// via the production `LoadConfig`→`Docker.Resolve` path), so the two impls can't
    /// drift together on the `Option<Vec<String>>` replace / `Option<bool>` force /
    /// flow-list-parse semantics. Go has NO `Resolve` layering test, so this golden
    /// is Rust-stronger. In-crate (the `aws_resolve` precedent — binary crate, no lib).
    #[test]
    fn golden_docker_resolve() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/docker_resolve.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let cfg = HostAgentConfig::parse(v["config_yaml"].as_str().unwrap());
            for q in v["queries"].as_array().unwrap() {
                let r = cfg
                    .docker
                    .resolve(q["server"].as_str().unwrap(), q["shed"].as_str().unwrap());
                let want_registries: Vec<String> = q["registries"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|s| s.as_str().unwrap().to_string())
                    .collect();
                assert_eq!(r.allow_all, q["allow_all"].as_bool().unwrap(), "allow_all {name:?}");
                assert_eq!(r.registries, want_registries, "registries {name:?}");
                // registry_count is the backend Status verdict (registries.len()).
                assert_eq!(
                    r.registries.len() as u64,
                    q["registry_count"].as_u64().unwrap(),
                    "registry_count {name:?}"
                );
            }
        }
    }
}
