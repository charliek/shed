//! Minimal host-agent config reader for slice 0 — deliberately scoped to exactly
//! what the LiveStatus self-report needs: the three approval policies
//! (`ssh/aws/docker .approval.policy`) plus `logging.{enabled,path}`. `load` now
//! also runs [`HostAgentConfig::validate`] — a faithful port of Go's
//! `Config.Validate` (the three policy allow-sets, `aws.sheds`/`aws.mode`, and
//! `approval_timeout`), so the Rust daemon rejects (exit 1) exactly the configs the
//! Go daemon rejects. The reader also surfaces the native-biometric approval knobs
//! (`ssh.approval.{scope,session_ttl}`) that feed `touchid::new_biometric_gate`; the
//! remaining schema (discovery-block contents) stays structurally out of this
//! headless reader.
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
/// Native Touch ID biometric policy (SSH only — `validate` rejects it for
/// aws/docker). Matches Go's `PolicyBiometrics`. Referenced only by `validate`'s
/// ssh allow-set.
pub const POLICY_BIOMETRICS: &str = "biometrics";
/// Native Touch ID + Apple Watch / password fallback (SSH only). Matches Go's
/// `PolicyBiometricsOrPassword`.
pub const POLICY_BIOMETRICS_OR_PASSWORD: &str = "biometrics-or-password";

/// The single-server bus URL default — matches Go's `Config.Server` default
/// (`config.go:428`). Used only in single-server mode (no `discovery:` block).
pub const DEFAULT_SERVER_URL: &str = "http://localhost:8080";

/// The shed CLI config the agent discovers servers from when discovery is enabled
/// without an explicit `source:` (mirror `config.go:DefaultDiscoverySource`). Applied
/// (and tilde-expanded) by [`DiscoveryConfig::apply_defaults`]; also re-exported by
/// `controltoken` for its single-server resolve default.
pub const DEFAULT_DISCOVERY_SOURCE: &str = "~/.shed/config.yaml";

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

/// Validate one extension's `approval.policy` against its allow-set, mirroring Go's
/// `validatePolicy` (`config.go:544-554`). An empty policy is valid (means deny-all).
/// A non-empty value not in `allowed` → the exact Go error
/// `%s.approval.policy %q is not one of %s` (allow-set joined by `", "`, in the
/// order Go passes it). The `%q` render is reproduced with literal double quotes so
/// the bytes match for the ASCII policy tokens in play (never `{:?}`, which could
/// diverge from Go's `%q` escaping on non-ASCII).
fn validate_policy(provider: &str, policy: &str, allowed: &[&str]) -> Result<(), String> {
    if policy.is_empty() || allowed.contains(&policy) {
        return Ok(());
    }
    Err(format!(
        "{provider}.approval.policy \"{policy}\" is not one of {}",
        allowed.join(", ")
    ))
}

/// Validate an AWS `mode` field, mirroring Go's `validateMode` (`config.go:580-588`).
/// An empty value (means assume-role) or a known mode is valid; anything else → the
/// exact Go error `%s %q is not one of %s, %s` (`assume-role, passthrough`). `field`
/// is the located field name (`aws.mode`, `aws.servers.<s>.mode`,
/// `aws.servers.<s>.sheds.<sh>.mode`).
fn validate_mode(field: &str, mode: &str) -> Result<(), String> {
    match mode {
        "" | AWS_MODE_ASSUME_ROLE | AWS_MODE_PASSTHROUGH => Ok(()),
        _ => Err(format!(
            "{field} \"{mode}\" is not one of {AWS_MODE_ASSUME_ROLE}, {AWS_MODE_PASSTHROUGH}"
        )),
    }
}

/// Validate `approval_timeout` off its RAW string, mirroring Go's
/// `ApprovalTimeoutDuration` (`config.go:511-524`) as invoked by `Validate`: an
/// EMPTY raw defaults to `"25s"` (valid); a non-empty raw is parsed with the strict
/// Go-`time.ParseDuration` semantics — a parse failure → `approval_timeout %q is not
/// a valid duration: <inner>` (the `%q` shows the RAW, so it is `""` for the empty
/// case, matching Go), and a non-positive value → `approval_timeout %q must be
/// positive`. Both `%q` renders use the RAW (not the defaulted `"25s"`), matching
/// Go's `c.ApprovalTimeout` in the format args.
fn validate_approval_timeout(raw: &str) -> Result<(), String> {
    let to_parse = if raw.is_empty() { "25s" } else { raw };
    match parse_go_duration_strict(to_parse) {
        Err(inner) => Err(format!(
            "approval_timeout \"{raw}\" is not a valid duration: {inner}"
        )),
        Ok(nanos) if nanos <= 0 => Err(format!("approval_timeout \"{raw}\" must be positive")),
        Ok(_) => Ok(()),
    }
}

/// A STRICT Go-`time.ParseDuration` port for the validate path: unlike the fail-safe
/// [`parse_go_duration_nanos`] (which the accessor keeps), this does NOT trim
/// whitespace (Go rejects `" 5s "`), uses integer/checked arithmetic (no `f64`
/// precision loss), and returns `Err` on overflow beyond `i64` nanoseconds (Go's
/// duration is an `int64`). Result is signed nanoseconds so the caller can test
/// positivity. The `Err` payload is Rust-internal (the golden pins only
/// `is not a valid duration`, never the inner text — yaml/`fmt`-lib specific, per the
/// docker suffix precedent).
fn parse_go_duration_strict(s: &str) -> Result<i128, String> {
    if s.is_empty() {
        return Err("empty duration".to_string());
    }
    let (neg, rest0) = match s.strip_prefix('-') {
        Some(r) => (true, r),
        None => (false, s.strip_prefix('+').unwrap_or(s)),
    };
    // Go accepts a bare "0" (and "+0"/"-0") with no unit.
    if rest0 == "0" {
        return Ok(0);
    }
    if rest0.is_empty() {
        return Err("no digits in duration".to_string());
    }
    let mut rest = rest0;
    let mut total: i128 = 0;
    while !rest.is_empty() {
        // Each fragment is a number (integer part + optional `.frac`) then a unit.
        // A leading char that is neither a digit nor '.' is malformed (this also
        // rejects embedded whitespace, since we never trimmed).
        if !rest.starts_with(|c: char| c.is_ascii_digit() || c == '.') {
            return Err("expected digit or '.' in duration".to_string());
        }
        let int_len = rest
            .find(|c: char| !c.is_ascii_digit())
            .unwrap_or(rest.len());
        let int_str = &rest[..int_len];
        rest = &rest[int_len..];
        let mut frac_str = "";
        if let Some(after_dot) = rest.strip_prefix('.') {
            let frac_len = after_dot
                .find(|c: char| !c.is_ascii_digit())
                .unwrap_or(after_dot.len());
            frac_str = &after_dot[..frac_len];
            rest = &after_dot[frac_len..];
        }
        if int_str.is_empty() && frac_str.is_empty() {
            return Err("number without digits in duration".to_string());
        }
        // Unit: the run of chars up to the next number/'.'.
        let unit_len = rest
            .find(|c: char| c.is_ascii_digit() || c == '.')
            .unwrap_or(rest.len());
        let unit_nanos: i128 = match &rest[..unit_len] {
            "" => return Err("missing unit in duration".to_string()),
            "ns" => 1,
            "us" | "µs" | "μs" => 1_000,
            "ms" => 1_000_000,
            "s" => 1_000_000_000,
            "m" => 60_000_000_000,
            "h" => 3_600_000_000_000,
            other => return Err(format!("unknown unit {other:?} in duration")),
        };
        rest = &rest[unit_len..];
        // Integer contribution (checked, no f64).
        let int_val: i128 = int_str
            .parse()
            .map_err(|_| "integer overflow in duration".to_string())?;
        let mut frag = int_val.checked_mul(unit_nanos).ok_or("duration overflow")?;
        // Fractional contribution: keep up to 18 digits (ns precision; guards the
        // scale `pow` from overflowing) and scale down with integer division.
        if !frac_str.is_empty() {
            let flen = frac_str.len().min(18);
            let frac_val: i128 = frac_str[..flen]
                .parse()
                .map_err(|_| "fraction overflow in duration".to_string())?;
            let scale = 10i128.pow(flen as u32);
            let frac_nanos = frac_val.checked_mul(unit_nanos).ok_or("duration overflow")? / scale;
            frag = frag.checked_add(frac_nanos).ok_or("duration overflow")?;
        }
        total = total.checked_add(frag).ok_or("duration overflow")?;
        // Bound to i64 nanoseconds like Go's int64 duration.
        if total > i64::MAX as i128 {
            return Err("duration overflow".to_string());
        }
    }
    Ok(if neg { -total } else { total })
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
    /// The native-biometric approval scope (`ssh.approval.scope`): `per-request`,
    /// `per-session` (default), or `per-shed`. Applies ONLY to the biometric policies
    /// (validate confines them to ssh); passed to `touchid::new_biometric_gate`. Read
    /// NULL-AWARE to mirror Go's `DefaultConfig`-then-overlay: absent/null →
    /// `per-session`, an explicit `scope: ""` is kept verbatim (→ the gate always
    /// prompts). Mirrors Go's `ApprovalConfig.Scope`.
    ssh_scope: String,
    /// The RAW native-biometric session TTL (`ssh.approval.session_ttl`), default
    /// `4h`. Kept verbatim: the gate parses it (parse-fail → 4h) AND audits the raw
    /// text (`out.TTL = cfg.SessionTTL`). Same null-aware defaulting as `ssh_scope`.
    /// Mirrors Go's `ApprovalConfig.SessionTTL`.
    ssh_session_ttl_raw: String,
    pub logging_enabled: bool,
    pub logging_path: String,
    // Read only by `approval_timeout()`, which only the desktop server calls.
    #[cfg_attr(not(feature = "desktop-forwarding"), allow(dead_code))]
    approval_timeout: Duration,
    /// The RAW `approval_timeout` string exactly as written in the config (`""`
    /// when the key is absent/null). Retained so [`HostAgentConfig::validate`] can
    /// re-check it the way Go's `Validate` re-parses `Config.ApprovalTimeout`: an
    /// EMPTY raw is valid (→ 25s default); a non-empty raw that fails to parse or is
    /// non-positive is rejected. The parsed `approval_timeout` above still fail-safes
    /// the VALUE to 25s (the accessor never rejects), mirroring Go's second,
    /// error-ignoring `ApprovalTimeoutDuration()` call in `main.go`.
    approval_timeout_raw: String,
    /// The single-server bus URL (`server:`), defaulting to `DEFAULT_SERVER_URL`.
    /// The message-bus daemon connects here in single-server mode. In Go this is
    /// `Config.Server`, used only when `Discovery` is nil.
    pub server: String,
    /// The parsed `discovery:` block, or `None` in single-server mode (the block is
    /// absent or an explicit YAML null). Mirrors Go's `Config.Discovery
    /// *DiscoveryConfig` (nil ⟺ single-server): when `Some`, the agent brokers for the
    /// discovered servers the selector picks; `is_single_server`/`has_discovery` and
    /// the reconcile path (`resolve_targets`, wired by the supervisor slice) gate on
    /// its presence. Defaults are applied at parse time (matching Go's `LoadConfig`
    /// calling `applyDefaults` only when `Discovery != nil`).
    pub(crate) discovery: Option<DiscoveryConfig>,
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
        // load = parse → validate (mirrors Go's `LoadConfig`: `yaml.Unmarshal` then
        // `Validate()`). Malformed YAML is a hard error (Go's `parsing config` path);
        // a validate failure is a hard error too (Go's `invalid config` path). Either
        // one → `Err` → `main.rs` exits 1; the daemon must not fail open with a
        // half-read or invalid policy.
        let cfg =
            Self::try_parse(&text).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
        cfg.validate()
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
        Ok(cfg)
    }

    /// Validate the loaded config, in Go's EXACT `Config.Validate` check order
    /// (`config.go:487-505`): the three approval-policy allow-sets
    /// (ssh → aws → docker), then `AWS.validate` (`aws.sheds` → `aws.mode` →
    /// per-server → per-shed), then `approval_timeout`. Returns the FIRST failure's
    /// message (Go returns the first error), with the same field prefixes and
    /// message shapes so the language-neutral golden can pin per-vector substrings.
    /// An empty policy is valid (→ deny-all); SSH alone accepts the native biometric
    /// policies (aws/docker reject them). Called from [`HostAgentConfig::load`].
    pub fn validate(&self) -> Result<(), String> {
        // Go's `sshAllowed` / `credAllowed` (`config.go:488-489`), same order — the
        // order is byte-visible in the `is not one of %s` join.
        const SSH_ALLOWED: &[&str] = &[
            POLICY_DENY_ALL,
            POLICY_APPROVE_ALL,
            POLICY_BIOMETRICS,
            POLICY_BIOMETRICS_OR_PASSWORD,
            POLICY_SHED_DESKTOP,
        ];
        const CRED_ALLOWED: &[&str] = &[POLICY_DENY_ALL, POLICY_APPROVE_ALL, POLICY_SHED_DESKTOP];
        validate_policy("ssh", &self.ssh_policy, SSH_ALLOWED)?;
        validate_policy("aws", &self.aws_policy, CRED_ALLOWED)?;
        validate_policy("docker", &self.docker_policy, CRED_ALLOWED)?;
        self.aws.validate()?;
        validate_approval_timeout(&self.approval_timeout_raw)?;
        Ok(())
    }

    /// Parse config text, returning an error on malformed YAML. Missing keys take
    /// their defaults (policies default to empty → deny-all effective; logging
    /// enabled true). This is the fallible path `load` uses; the infallible
    /// `HostAgentConfig::parse` convenience wraps it for known-good test literals.
    pub fn try_parse(text: &str) -> Result<HostAgentConfig, String> {
        let root = yaml_lite::parse(text)?;
        Self::from_root(&root)
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
    /// half of `try_parse`). Fallible only through the discovery `ServerSelector`
    /// (D3): a map-valued `discovery.servers:` surfaces the exact Go error so
    /// `try_parse`/`load` reject it → exit 1, matching Go's `UnmarshalYAML`.
    fn from_root(root: &yaml_lite::Node) -> Result<HostAgentConfig, String> {
        let policy = |ns_key: &str| -> String {
            root.get_path(&[ns_key, "approval", "policy"])
                .unwrap_or_default()
                .to_string()
        };
        // The RAW `approval_timeout` string, read once and shared between the
        // fail-safe parsed Duration (the accessor) and the string `validate` re-checks.
        let approval_timeout_raw = root
            .get_path(&["approval_timeout"])
            .unwrap_or_default()
            .to_string();
        // `logging.enabled` defaults to true (Go's `DefaultConfig` sets it true and
        // yaml.v3 overlays). An ABSENT/null value keeps that default; a present scalar
        // resolves through yaml.v3's bool set (`false`/`no`/`off`/`n` → off — the D2
        // FIX; `off` used to leak through the old `v != "false"` and keep logging ON).
        // A non-resolvable present scalar (`nonsense`/`1`/`0`) is the D6 residue: Go
        // errors, here it falls back to the default `true` (the pre-D2 lenient outcome
        // for any non-`false` scalar — preserved).
        let logging_enabled = match root.get_path(&["logging", "enabled"]) {
            Some(v) => parse_yaml_bool(v).unwrap_or(true),
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
        // The native-biometric knobs under `ssh.approval.*`, read NULL-AWARE (the same
        // `scalar_or_default` shape `AwsConfig::from_node` uses): an explicit `Scalar`
        // (including `""`) is kept verbatim; absent / null / non-scalar falls back to
        // the `DefaultConfig` default (`per-session` / `4h`). This reproduces Go's
        // `DefaultConfig`-then-overlay so `scope: ""` → always-prompt while absent →
        // `per-session`.
        let ssh_approval = root
            .as_map()
            .and_then(|m| m.get("ssh"))
            .and_then(yaml_lite::Node::as_map)
            .and_then(|m| m.get("approval"))
            .and_then(yaml_lite::Node::as_map);
        let ssh_scalar_or_default = |key: &str, d: &str| -> String {
            match ssh_approval.and_then(|m| m.get(key)) {
                Some(yaml_lite::Node::Scalar(s)) => s.clone(),
                _ => d.to_string(),
            }
        };
        // A `discovery:` key PRESENT and NON-NULL flips to multi-server mode. A bare
        // `discovery:` (YAML null) must read as ABSENT: Go unmarshals a null into
        // `*DiscoveryConfig` as nil, and every discovery path gates on `cfg.Discovery
        // != nil` (`main.go:142/207/235`), so a null `discovery:` stays SINGLE-server.
        // `discovery: {}` (empty map) is non-null → multi (Go gives a non-nil empty
        // struct). (CodeRabbit review finding.) Defaults are applied here, matching
        // Go's `LoadConfig` calling `applyDefaults` only when `Discovery != nil`. The
        // `?` propagates a map-valued `discovery.servers:` as the D3 exit-1 error.
        let discovery = match root.as_map().and_then(|m| m.get("discovery")) {
            None | Some(yaml_lite::Node::Null) => None,
            Some(node) => {
                let mut dc = DiscoveryConfig::from_node(node)?;
                dc.apply_defaults();
                Some(dc)
            }
        };
        Ok(HostAgentConfig {
            ssh_policy: policy("ssh"),
            aws_policy: policy("aws"),
            docker_policy: policy("docker"),
            ssh_mode: root
                .get_path(&["ssh", "mode"])
                .unwrap_or_default()
                .to_string(),
            ssh_scope: ssh_scalar_or_default("scope", "per-session"),
            ssh_session_ttl_raw: ssh_scalar_or_default("session_ttl", "4h"),
            logging_enabled,
            logging_path,
            approval_timeout: parse_approval_timeout(&approval_timeout_raw),
            approval_timeout_raw,
            server,
            discovery,
            aws: AwsConfig::from_node(root),
            docker: DockerConfig::from_node(root),
        })
    }

    /// True when the agent runs in single-server mode — no `discovery:` block, so
    /// the message-bus daemon connects to the single `server:` URL. Mirrors Go's
    /// `cfg.Discovery == nil` branch in `main.go`; with a `discovery:` block present
    /// the daemon reconciles over the discovered servers instead. Now that the daemon
    /// unifies on the supervisor (which drives both modes via `resolve_targets`, keying
    /// on `discovery.as_ref()` directly), production no longer branches on this — it
    /// remains as a test/probe accessor (like [`has_discovery`](Self::has_discovery)).
    #[cfg(test)]
    pub fn is_single_server(&self) -> bool {
        self.discovery.is_none()
    }

    /// Whether a `discovery:` block is present (the inverse of
    /// [`is_single_server`](Self::is_single_server)) — mirrors Go's
    /// `cfg.Discovery != nil`. Retained as the test/probe accessor now that the field
    /// is a parsed `Option` rather than the old presence bool.
    #[cfg(test)]
    pub fn has_discovery(&self) -> bool {
        self.discovery.is_some()
    }

    /// The delegated-approval budget the desktop server enforces per request, and
    /// which drives the `hello_ack.request_timeout_ms` it advertises. Mirrors Go's
    /// `ApprovalTimeoutDuration` + `NewDesktopServer`'s guard: an absent, invalid,
    /// or non-positive `approval_timeout` falls back to 25s. [`HostAgentConfig::load`]
    /// now hard-rejects an invalid/non-positive value at startup (via `validate`), so
    /// production never reaches this accessor with a bad value; the fail-safe remains
    /// for the in-crate `parse` convenience (which skips validation), mirroring Go's
    /// second, error-ignoring `ApprovalTimeoutDuration()` call in `main.go`.
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

    /// The native-biometric approval scope (`ssh.approval.scope`), null-aware
    /// defaulted to `per-session`. Passed to `touchid::new_biometric_gate` in
    /// `main.rs`'s `select_gate` (the biometric arm); ignored by every non-biometric
    /// gate. Mirrors Go's `ApprovalConfig.Scope`.
    pub(crate) fn ssh_scope(&self) -> &str {
        &self.ssh_scope
    }

    /// The RAW native-biometric session TTL (`ssh.approval.session_ttl`), null-aware
    /// defaulted to `4h`. Passed verbatim to `touchid::new_biometric_gate`, which
    /// parses it for the cache TTL (parse-fail → 4h) and audits the raw text. Mirrors
    /// Go's `ApprovalConfig.SessionTTL`.
    pub(crate) fn ssh_session_ttl(&self) -> &str {
        &self.ssh_session_ttl_raw
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

/// Selects which discovered servers a single agent process watches and how it reacts
/// to changes in the discovery source — a faithful port of Go's `DiscoveryConfig`
/// (`config.go:60`). Present (`Some`) on [`HostAgentConfig`] means multi-server mode.
///
/// The reload knobs (`watch`/`poll_interval`/`debounce`) are consumed by the watcher
/// (`run_watch_loop`) and `servers` by the reconcile path (`resolve_targets`);
/// `apply_defaults` reads the four string knobs at parse time.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub(crate) struct DiscoveryConfig {
    /// Which discovered servers to watch (default: all). See [`ServerSelector`].
    pub servers: ServerSelector,
    /// Overrides the shed CLI config path (default `~/.shed/config.yaml`,
    /// tilde-expanded by `apply_defaults`).
    pub source: String,
    /// Live-reload mode: `"fsnotify"` (default), `"poll"`, or `"off"`.
    pub watch: String,
    /// Reconcile cadence when `watch == "poll"` (default `10s`).
    pub poll_interval: String,
    /// Debounce window coalescing rapid fsnotify events (default `500ms`).
    pub debounce: String,
}

impl DiscoveryConfig {
    /// Build a `DiscoveryConfig` from the `discovery:` block node — the scalar knobs
    /// plus the [`ServerSelector`]. Absent/non-scalar knobs read as `""` and are filled
    /// by [`apply_defaults`](Self::apply_defaults) (mirroring Go's zero-value struct +
    /// `LoadConfig`-time `applyDefaults`). Fallible only via [`ServerSelector::from_node`]
    /// — a map-valued `servers:` propagates the D3 exit-1 error up to `try_parse`/`load`.
    fn from_node(node: &yaml_lite::Node) -> Result<DiscoveryConfig, String> {
        let map = node.as_map();
        let scalar = |k: &str| {
            map.and_then(|m| m.get(k))
                .and_then(yaml_lite::Node::as_scalar)
                .unwrap_or("")
                .to_string()
        };
        Ok(DiscoveryConfig {
            servers: ServerSelector::from_node(map.and_then(|m| m.get("servers")))?,
            source: scalar("source"),
            watch: scalar("watch"),
            poll_interval: scalar("poll_interval"),
            debounce: scalar("debounce"),
        })
    }

    /// Fill unset discovery fields and expand `~` in `source` — a faithful port of Go's
    /// `applyDefaults` (`config.go:595`): `source` defaults to
    /// [`DEFAULT_DISCOVERY_SOURCE`] then tilde-expands; `watch` defaults to `"fsnotify"`,
    /// `poll_interval` to `"10s"`, `debounce` to `"500ms"`.
    fn apply_defaults(&mut self) {
        if self.source.is_empty() {
            self.source = DEFAULT_DISCOVERY_SOURCE.to_string();
        }
        self.source = expand_tilde(&self.source);
        if self.watch.is_empty() {
            self.watch = "fsnotify".to_string();
        }
        if self.poll_interval.is_empty() {
            self.poll_interval = "10s".to_string();
        }
        if self.debounce.is_empty() {
            self.debounce = "500ms".to_string();
        }
    }
}

/// Chooses which discovered servers to watch — a faithful port of Go's `ServerSelector`
/// and its `UnmarshalYAML` (`config.go:78-124`). The scalar `""`/`"all"` (and a bare
/// null) select every server; any other scalar is a one-element list; a YAML sequence is
/// the explicit list. The nil-vs-empty distinction is load-bearing: an OMITTED selector
/// (`names` is `None`) selects everything, while an EXPLICIT empty list (`servers: []`,
/// i.e. `names` is `Some(vec![])`) selects nothing.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub(crate) struct ServerSelector {
    /// The `all`/`""`/null scalar form (mirror Go's `All bool`).
    pub all: bool,
    /// The explicit name list. `None` = omitted selector (⇒ select all, matching Go's
    /// `Names == nil`); `Some(vec![])` = explicit empty (`servers: []` ⇒ select none);
    /// `Some(names)` = membership. Mirrors Go's `Names []string` nil-vs-empty.
    pub names: Option<Vec<String>>,
}

impl ServerSelector {
    /// Interpret the `servers:` node, mirroring Go's `UnmarshalYAML` (`config.go:85`):
    /// an OMITTED node → the zero value (⇒ select all); a null (bare `servers:`) or the
    /// scalar `""`/`"all"` → `all = true`; any other scalar → a one-element list; a
    /// sequence → the name list (empty stays `Some(vec![])`, the watch-none form). A
    /// MAPPING value is invalid: Go's `UnmarshalYAML` returns the error
    /// `discovery.servers must be "all" or a list of server names` (`config.go:107`) →
    /// `LoadConfig` fails → exit 1. This reader is fallible for exactly that case (D3
    /// FIX), returning the SAME message so `try_parse`/`load` reject it too (Go-faithful
    /// exit 1, not the pre-D3 silent select-all fallback).
    fn from_node(node: Option<&yaml_lite::Node>) -> Result<ServerSelector, String> {
        use yaml_lite::Node;
        Ok(match node {
            None => ServerSelector::default(),
            // A bare `servers:` is a YAML null; Go's UnmarshalYAML sees a scalar whose
            // Value is "" → All. Match that (a null and `servers: ""` both ⇒ all).
            Some(Node::Null) => ServerSelector {
                all: true,
                names: None,
            },
            Some(Node::Scalar(s)) => {
                if s.is_empty() || s == "all" {
                    ServerSelector {
                        all: true,
                        names: None,
                    }
                } else {
                    ServerSelector {
                        all: false,
                        names: Some(vec![s.clone()]),
                    }
                }
            }
            // Delegate to the canonical sequence-flatten helper, whose doc pins the
            // load-bearing empty-`[]` → `Some(vec![])` (watch-none) semantics; a
            // `Sequence` node always yields `Some`, so `names` is never `None` here.
            Some(seq @ Node::Sequence(_)) => ServerSelector {
                all: false,
                names: seq.as_scalar_list(),
            },
            // A mapping selector is invalid in Go (its `UnmarshalYAML` default arm
            // errors) → reject with the exact Go message so the daemon exits 1.
            Some(Node::Map(_)) => {
                return Err(
                    "discovery.servers must be \"all\" or a list of server names".to_string(),
                );
            }
        })
    }

    /// Whether a discovered server name should be watched — a faithful port of Go's
    /// `Selected` (`config.go:114`): `all` OR an omitted list (`names == None`) selects
    /// everything; an explicit list is a membership test (so `servers: []` selects
    /// nothing). Reached by `resolve_targets_from` on the daemon reconcile path.
    pub fn selected(&self, name: &str) -> bool {
        if self.all || self.names.is_none() {
            return true;
        }
        self.names.as_ref().is_some_and(|names| names.iter().any(|n| n == name))
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
    /// The REMOVED global per-shed override map (Go's `AWSConfig.Sheds`). Parsed
    /// ONLY so [`HostAgentConfig::validate`] can reject a populated one with the
    /// migration message (`config.go:561`, the `len > 0` reject); it does NOT affect
    /// resolution. Bare `sheds:` / `sheds: {}` / `sheds: null` parse to an EMPTY map
    /// (len 0 = valid); only a populated map rejects.
    pub sheds: BTreeMap<String, AwsShedConfig>,
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

/// Read a `sheds:` submap into `AwsShedConfig` entries (the per-shed `role`/`mode`/
/// `session_duration` scalars). Shared by both the per-server `servers.<s>.sheds`
/// walk (used for resolution) and the removed global `aws.sheds` walk (used only for
/// the validate reject). A non-map entry is skipped, matching the lenient reader.
fn read_aws_sheds(
    map: &std::collections::HashMap<String, yaml_lite::Node>,
) -> BTreeMap<String, AwsShedConfig> {
    use yaml_lite::Node;
    let mut out = BTreeMap::new();
    for (name, entry) in map {
        let Some(sf) = entry.as_map() else { continue };
        let g = |k: &str| sf.get(k).and_then(Node::as_scalar).unwrap_or("").to_string();
        out.insert(
            name.clone(),
            AwsShedConfig {
                role: g("role"),
                mode: g("mode"),
                session_duration: g("session_duration"),
            },
        );
    }
    out
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
    /// `aws.sheds` (Go's removed global per-shed map) IS parsed now — but only so
    /// [`HostAgentConfig::validate`] can reject a populated one (the `len > 0`
    /// migration reject, `config.go:561`). It stays parse-and-ignore for resolution.
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
                let sheds = fields
                    .get("sheds")
                    .and_then(Node::as_map)
                    .map(read_aws_sheds)
                    .unwrap_or_default();
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
            // The removed global `aws.sheds` map: parsed for the validate reject
            // (`!is_empty()`), never used for resolution. Bare/`{}`/`null` → empty.
            sheds: aws
                .and_then(|m| m.get("sheds"))
                .and_then(Node::as_map)
                .map(read_aws_sheds)
                .unwrap_or_default(),
            servers,
        }
    }

    /// Validate the AWS slice, mirroring Go's `AWSConfig.validate` (`config.go:560-578`)
    /// in the same order: the removed `aws.sheds` map (`len > 0` → migration reject),
    /// then `aws.mode`, then each server's mode, then each per-shed mode.
    fn validate(&self) -> Result<(), String> {
        if !self.sheds.is_empty() {
            return Err(
                "aws.sheds was removed; move entries under aws.servers.<server>.sheds.<shed>"
                    .to_string(),
            );
        }
        validate_mode("aws.mode", &self.mode)?;
        for (name, sv) in &self.servers {
            validate_mode(&format!("aws.servers.{name}.mode"), &sv.mode)?;
            for (shed, sc) in &sv.sheds {
                validate_mode(&format!("aws.servers.{name}.sheds.{shed}.mode"), &sc.mode)?;
            }
        }
        Ok(())
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

/// Resolve a YAML scalar to a bool EXACTLY as `gopkg.in/yaml.v3` v3.0.1 does when
/// decoding a plain scalar into a `bool` field (orchestrator-probed + re-verified
/// against the vendored yaml.v3): the YAML-1.1 bool token set in its three canonical
/// case forms — `true/True/TRUE`, `yes/Yes/YES`, `on/On/ON`, `y/Y` → `Some(true)`;
/// `false/False/FALSE`, `no/No/NO`, `off/Off/OFF`, `n/N` → `Some(false)`. Anything
/// else — a non-canonical case (`tRUe`, `yEs`), `nonsense`, `1`, `0`, or a non-scalar
/// — is NOT a resolvable bool: yaml.v3 ERRORS (`!!str`/`!!int` into bool), but the
/// stringly-typed host-agent reader can't error without a typed `Node` layer, so it
/// returns `None` (the D6 typed-decode residue, ACCEPT — the caller keeps its lenient
/// default). This is the D2 FIX: `opt_bool`/`logging_enabled` previously matched only
/// lowercase `true`/`false`, silently mis-reading `allow_all: yes` / `logging.enabled:
/// off`.
fn parse_yaml_bool(s: &str) -> Option<bool> {
    match s {
        "true" | "True" | "TRUE" | "yes" | "Yes" | "YES" | "on" | "On" | "ON" | "y" | "Y" => {
            Some(true)
        }
        "false" | "False" | "FALSE" | "no" | "No" | "NO" | "off" | "Off" | "OFF" | "n" | "N" => {
            Some(false)
        }
        _ => None,
    }
}

/// Read an optional YAML bool leaf, mirroring Go's `*bool` decode: an absent node
/// → `None` (inherit), a present scalar → `Some(resolved)` for yaml.v3's bool token
/// set (so `allow_all: false`/`no`/`off` are `Some(false)` — force-off — and
/// `allow_all: yes`/`on`/`true` are `Some(true)` — force-on — not inherit). A present
/// scalar that yaml.v3 would NOT resolve to a bool (`nonsense`/`1`/`0`) is the D6
/// residue: Go errors, but here it maps to `Some(false)` (the pre-D2 lenient outcome
/// for a non-`true` scalar — preserved, not silently flipped). A present non-scalar
/// (e.g. a map) → `None` (inherit).
fn opt_bool(node: Option<&yaml_lite::Node>) -> Option<bool> {
    node.and_then(yaml_lite::Node::as_scalar)
        .map(|s| parse_yaml_bool(s).unwrap_or(false))
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

        /// Whether this node is a map containing `key` (present at any value,
        /// including a null value). Production now distinguishes present-and-null
        /// from present-and-non-null directly (`has_discovery`), so this stays as a
        /// test-only presence accessor.
        #[cfg(test)]
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
        assert!(cfg.has_discovery());
        assert_eq!(cfg.server, DEFAULT_SERVER_URL);
        // The block CONTENTS parse: the explicit empty list is the watch-none form, and
        // `watch`/`source` are read through (source is absolute, so tilde-expand is a
        // no-op). `apply_defaults` still fills `poll_interval`/`debounce`.
        let dc = cfg.discovery.as_ref().expect("discovery present");
        assert_eq!(dc.servers.names, Some(Vec::<String>::new()));
        assert!(!dc.servers.all);
        assert_eq!(dc.watch, "off");
        assert_eq!(dc.source, "/tmp/x.yaml");
        assert_eq!(dc.poll_interval, "10s");
        assert_eq!(dc.debounce, "500ms");

        // A bare `discovery:` (YAML null) reads as ABSENT → SINGLE-server, matching
        // Go's nil `*DiscoveryConfig` (CodeRabbit review). An empty MAP `discovery: {}`
        // is non-null → multi-server (Go non-nil empty struct).
        assert!(HostAgentConfig::parse("discovery:\n").is_single_server());
        assert!(!HostAgentConfig::parse("discovery: {}\n").is_single_server());
        assert!(!HostAgentConfig::parse("discovery:\n  watch: off\n").is_single_server());
    }

    // ---- discovery: ServerSelector / DiscoveryConfig / resolve_targets ---------------

    /// Parse a `discovery:`-block-body YAML literal into a `DiscoveryConfig` via the same
    /// `Node → DiscoveryConfig` path `from_root` uses (WITHOUT `apply_defaults`, so the
    /// raw selector/knob parse can be asserted). Mirrors Go's `yaml.Unmarshal(body, &dc)`.
    fn parse_discovery(body: &str) -> DiscoveryConfig {
        let node = yaml_lite::parse(body).expect("discovery body parses");
        DiscoveryConfig::from_node(&node).expect("discovery selector valid")
    }

    /// Mirrors `config_discovery_test.go:TestServerSelectorUnmarshal`: scalar `all`/`""`
    /// (and a bare null) → `all`; an omitted selector → the zero value (nil names); any
    /// other scalar → a one-element list; a sequence → the list (empty stays non-nil —
    /// the watch-none form).
    #[test]
    fn server_selector_unmarshal() {
        let cases: &[(&str, bool, Option<Vec<String>>)] = &[
            ("servers: all\n", true, None),
            ("watch: poll\n", false, None), // omitted selector
            ("servers: \"\"\n", true, None), // empty scalar
            ("servers: []\n", false, Some(vec![])), // explicit empty = watch none
            (
                "servers: [mini2, mini3]\n",
                false,
                Some(vec!["mini2".into(), "mini3".into()]),
            ),
            ("servers: mini2\n", false, Some(vec!["mini2".into()])),
            ("servers:\n", true, None), // bare null ⇒ all (Go's null Value == "")
        ];
        for (body, want_all, want_names) in cases {
            let dc = parse_discovery(body);
            assert_eq!(dc.servers.all, *want_all, "all for {body:?}");
            assert_eq!(&dc.servers.names, want_names, "names for {body:?}");
        }
    }

    /// Mirrors `config_discovery_test.go:TestServerSelectorSelected`: `all` OR an omitted
    /// list selects everything; an explicit list is a membership test; an explicit empty
    /// list (`servers: []`) selects nothing — the load-bearing nil-vs-empty distinction.
    #[test]
    fn server_selector_selected() {
        let all = ServerSelector {
            all: true,
            names: None,
        };
        assert!(all.selected("anything"));
        // The zero value (omitted selector) selects everything (nil names).
        assert!(ServerSelector::default().selected("anything"));
        let list = ServerSelector {
            all: false,
            names: Some(vec!["mini2".into()]),
        };
        assert!(list.selected("mini2"));
        assert!(!list.selected("mini3"));
        let empty = ServerSelector {
            all: false,
            names: Some(vec![]),
        };
        assert!(!empty.selected("anything"));
    }

    /// Mirrors `config_discovery_test.go:TestLoadConfigDiscoveryDefaults`: through the
    /// real `parse` (which applies defaults when the block is present) an absent
    /// `watch`/`poll_interval`/`debounce` default to `fsnotify`/`10s`/`500ms`, and
    /// `source` defaults to the shed CLI config with `~` EXPANDED (not the literal
    /// `~/...`). Explicit values are kept.
    #[test]
    fn discovery_apply_defaults() {
        let cfg = HostAgentConfig::parse("discovery:\n  servers: all\n");
        let dc = cfg.discovery.as_ref().expect("discovery present");
        assert!(dc.servers.all);
        assert_eq!(dc.watch, "fsnotify");
        assert_eq!(dc.poll_interval, "10s");
        assert_eq!(dc.debounce, "500ms");
        // Source defaulted AND tilde-expanded (so neither empty nor the literal default).
        assert!(!dc.source.is_empty());
        assert_ne!(dc.source, DEFAULT_DISCOVERY_SOURCE);
        assert!(!dc.source.starts_with("~/"));

        // Explicit knobs are kept, not overwritten by defaults.
        let cfg = HostAgentConfig::parse(
            "discovery:\n  watch: poll\n  poll_interval: 3s\n  debounce: 100ms\n  source: /abs/x.yaml\n",
        );
        let dc = cfg.discovery.as_ref().unwrap();
        assert_eq!(dc.watch, "poll");
        assert_eq!(dc.poll_interval, "3s");
        assert_eq!(dc.debounce, "100ms");
        assert_eq!(dc.source, "/abs/x.yaml");
    }

    // The `resolve_targets` unit tests (which build `crate::discovery::ServerTarget`
    // and call the `resolve_targets*` methods) live in `discovery.rs` — those methods
    // + type are defined there, and `config.rs` must stay self-contained for the
    // standalone `#[path]` golden include (`tests/golden.rs`).

    /// The Rust half of the `server_selector` golden — reads the SAME shared fixture the
    /// Go runner reads (`cmd/shed-host-agent/golden_test.go:TestGoldenServerSelector`, via
    /// a real `yaml.Unmarshal` into `DiscoveryConfig` + `Selected`), so the two impls
    /// can't drift on the all / one-name / list-member / empty-list-none / absent-all
    /// selector semantics (the nil-vs-empty distinction). In-crate (binary crate, no lib
    /// — the `aws_resolve`/`docker_resolve` precedent).
    #[test]
    fn golden_server_selector() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/server_selector.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let selector = parse_discovery(v["selector_yaml"].as_str().unwrap()).servers;
            for q in v["queries"].as_array().unwrap() {
                let qn = q["name"].as_str().unwrap();
                assert_eq!(
                    selector.selected(qn),
                    q["selected"].as_bool().unwrap(),
                    "vector {name:?} name {qn:?}"
                );
            }
        }
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

    // ---- validate() parity (mirror config_test.go's Validate tests) --------------

    /// Parse `yaml` then `validate()` — the `load = parse → validate` chain without a
    /// temp file. Returns the validate error (parse is known-good for these literals).
    fn validate_yaml(yaml: &str) -> Result<(), String> {
        HostAgentConfig::parse(yaml).validate()
    }

    /// Mirrors `config_test.go:TestLoadConfigRejectsBadPolicy`. An unknown ssh policy
    /// is rejected with the EXACT Go message (allow-set joined in Go's order). (P: C2)
    /// the substring the golden pins is `ssh.approval.policy`, NOT bare `is not one
    /// of` (which also matches `aws.mode`).
    #[test]
    fn validate_rejects_unknown_ssh_policy() {
        let err = validate_yaml("ssh:\n  approval:\n    policy: maybe\n")
            .expect_err("unknown ssh policy rejected");
        assert_eq!(
            err,
            "ssh.approval.policy \"maybe\" is not one of \
             deny-all, approve-all, biometrics, biometrics-or-password, shed-desktop"
        );
    }

    /// aws + docker unknown policies are rejected with the cred allow-set (no
    /// biometrics). The prefixes prove the aws/docker path fired, not ssh.
    #[test]
    fn validate_rejects_unknown_aws_and_docker_policy() {
        let aws = validate_yaml("aws:\n  approval:\n    policy: maybe\n")
            .expect_err("unknown aws policy rejected");
        assert_eq!(
            aws,
            "aws.approval.policy \"maybe\" is not one of deny-all, approve-all, shed-desktop"
        );
        let docker = validate_yaml("docker:\n  approval:\n    policy: maybe\n")
            .expect_err("unknown docker policy rejected");
        assert_eq!(
            docker,
            "docker.approval.policy \"maybe\" is not one of deny-all, approve-all, shed-desktop"
        );
    }

    /// Mirrors `config_test.go:TestValidateRejectsBiometricForAWS` — the native
    /// biometric policies are SSH-only; the cred allow-set excludes them for
    /// aws/docker. The same policy is ACCEPTED for ssh.
    #[test]
    fn validate_rejects_biometrics_for_aws_and_docker() {
        assert!(validate_yaml("aws:\n  approval:\n    policy: biometrics\n").is_err());
        assert!(
            validate_yaml("docker:\n  approval:\n    policy: biometrics-or-password\n").is_err()
        );
        // ...but ssh accepts both native biometric policies.
        assert!(validate_yaml("ssh:\n  approval:\n    policy: biometrics\n").is_ok());
        assert!(validate_yaml("ssh:\n  approval:\n    policy: biometrics-or-password\n").is_ok());
    }

    /// Mirrors `config_test.go:TestValidateAcceptsValidPolicies` — ssh
    /// biometrics-or-password + aws shed-desktop + docker approve-all all validate.
    #[test]
    fn validate_accepts_valid_policies() {
        assert!(validate_yaml(
            "ssh:\n  approval:\n    policy: biometrics-or-password\n\
             aws:\n  approval:\n    policy: shed-desktop\n\
             docker:\n  approval:\n    policy: approve-all\n"
        )
        .is_ok());
    }

    /// Mirrors `config_test.go:TestValidateAWS` (removed-sheds subtest). A POPULATED
    /// `aws.sheds` map is rejected with the migration message (P: H2 — the reject is
    /// `len > 0`, `config.go:561`, built as a map then `!is_empty()`).
    #[test]
    fn validate_rejects_aws_sheds_removed() {
        let err = validate_yaml("aws:\n  sheds:\n    web:\n      role: arn:aws:iam::123:role/web\n")
            .expect_err("populated aws.sheds rejected");
        assert_eq!(
            err,
            "aws.sheds was removed; move entries under aws.servers.<server>.sheds.<shed>"
        );
    }

    /// (P: H2 — the four-case pin.) Bare `sheds:` (null), `sheds: {}` (empty map), and
    /// `sheds: null` are ALL valid (len 0); only a populated map rejects. This proves
    /// the check is `!is_empty()`, NOT `has_key`.
    #[test]
    fn validate_aws_sheds_empty_forms_ok() {
        assert!(validate_yaml("aws:\n  sheds:\n").is_ok(), "bare sheds:");
        assert!(validate_yaml("aws:\n  sheds: {}\n").is_ok(), "empty map");
        assert!(validate_yaml("aws:\n  sheds: null\n").is_ok(), "explicit null");
        // ...and an absent aws block entirely.
        assert!(validate_yaml("").is_ok(), "no aws block");
    }

    /// Mirrors `config_test.go:TestValidateAWS` (mode subtests). An unknown mode at
    /// the top level, a server, or a shed is rejected with the located field name and
    /// the `assume-role, passthrough` allow-set.
    #[test]
    fn validate_rejects_bad_aws_mode() {
        let top = validate_yaml("aws:\n  mode: bogus\n").expect_err("bad top mode");
        assert_eq!(top, "aws.mode \"bogus\" is not one of assume-role, passthrough");
        let server = validate_yaml("aws:\n  servers:\n    mini2:\n      mode: nope\n")
            .expect_err("bad server mode");
        assert_eq!(
            server,
            "aws.servers.mini2.mode \"nope\" is not one of assume-role, passthrough"
        );
        let shed = validate_yaml(
            "aws:\n  servers:\n    mini2:\n      sheds:\n        web:\n          mode: nope\n",
        )
        .expect_err("bad shed mode");
        assert_eq!(
            shed,
            "aws.servers.mini2.sheds.web.mode \"nope\" is not one of assume-role, passthrough"
        );
    }

    /// Mirrors `config_test.go:TestValidateAWS` (accepts-valid-modes subtest). Valid
    /// modes at every level (empty/assume-role/passthrough) validate.
    #[test]
    fn validate_accepts_valid_aws_modes() {
        let ok = validate_yaml(
            "\
aws:
  mode: passthrough
  servers:
    mini2:
      mode: assume-role
      sheds:
        web:
          mode: passthrough
",
        );
        assert!(ok.is_ok(), "{ok:?}");
    }

    /// Mirrors `config_test.go:TestApprovalTimeout` + the H1 edge vectors. The
    /// accessor still fail-safes the VALUE to 25s (unchanged); `validate` now REJECTS
    /// a bad/non-positive raw with the RIGHT per-vector message (P: C2), and the
    /// STRICT parser rejects what the fail-safe f64 parser tolerated (whitespace /
    /// overflow).
    #[test]
    fn validate_rejects_bad_approval_timeout() {
        // Empty raw is valid (→ 25s default) — Go's `v == "" → "25s"`.
        assert!(validate_yaml("").is_ok(), "absent approval_timeout");
        assert!(
            validate_yaml("approval_timeout: 40s\n").is_ok(),
            "valid approval_timeout"
        );
        // Unparseable → `is not a valid duration`.
        for bad in ["nonsense", "10"] {
            let err = validate_yaml(&format!("approval_timeout: {bad}\n"))
                .expect_err("unparseable approval_timeout");
            assert!(
                err.contains("is not a valid duration"),
                "approval_timeout {bad:?}: {err:?}"
            );
            assert!(err.contains("approval_timeout"), "{err:?}");
        }
        // Non-positive → `must be positive`.
        for bad in ["0s", "-5s"] {
            let err = validate_yaml(&format!("approval_timeout: {bad}\n"))
                .expect_err("non-positive approval_timeout");
            assert!(err.contains("must be positive"), "approval_timeout {bad:?}: {err:?}");
        }
        // STRICT-parser hardening (P: H1): quoted leading/trailing whitespace (Go
        // errors, the fail-safe parser trimmed) and an i64-overflowing magnitude are
        // BOTH rejected here even though the accessor would silently 25s them.
        let ws = validate_yaml("approval_timeout: \" 5s \"\n").expect_err("whitespace rejected");
        assert!(ws.contains("is not a valid duration"), "{ws:?}");
        let overflow = validate_yaml("approval_timeout: 10000000000000000000h\n")
            .expect_err("overflow rejected");
        assert!(overflow.contains("is not a valid duration"), "{overflow:?}");
    }

    /// The strict duration parser (`parse_go_duration_strict`) is a faithful
    /// `time.ParseDuration` subset: it ACCEPTS the same valid forms the fail-safe f64
    /// parser does, but REJECTS whitespace + overflow (no trim, integer/checked math).
    #[test]
    fn go_duration_strict_edge_cases() {
        assert_eq!(parse_go_duration_strict("25s"), Ok(25_000_000_000));
        assert_eq!(parse_go_duration_strict("1.5h"), Ok(5_400_000_000_000));
        assert_eq!(parse_go_duration_strict("1h30m"), Ok(5_400_000_000_000));
        assert_eq!(parse_go_duration_strict("300ms"), Ok(300_000_000));
        assert_eq!(parse_go_duration_strict("0"), Ok(0));
        assert_eq!(parse_go_duration_strict("-5s"), Ok(-5_000_000_000));
        // Rejected (Go rejects too): no unit, whitespace, empty, unknown unit, overflow.
        assert!(parse_go_duration_strict("10").is_err(), "bare number, no unit");
        assert!(parse_go_duration_strict(" 5s ").is_err(), "whitespace not trimmed");
        assert!(parse_go_duration_strict("5 s").is_err(), "internal whitespace");
        assert!(parse_go_duration_strict("").is_err(), "empty");
        assert!(parse_go_duration_strict("5y").is_err(), "unknown unit");
        assert!(
            parse_go_duration_strict("10000000000000000000h").is_err(),
            "overflow"
        );
    }

    /// Mirrors `config_test.go:TestExampleConfigIsValid`. The shipped default config
    /// loads + validates and carries the documented, CARRIED defaults (ssh
    /// biometrics-or-password, aws off/deny-all, docker approve-all + the block-seq
    /// `registries == [index.docker.io, ghcr.io]`). Now that the reader surfaces the
    /// native-biometric knobs, it ALSO asserts `ssh.approval.{scope,session_ttl}` from
    /// the example (`per-session` / `1h`, `configs/extensions.example.yaml:67-68`).
    #[test]
    fn example_config_loads_and_validates() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../configs/extensions.example.yaml");
        let text = std::fs::read_to_string(&path).expect("read example config");
        let cfg = HostAgentConfig::try_parse(&text).expect("example config parses");
        cfg.validate().expect("example config validates");
        assert_eq!(cfg.effective_policy(NS_SSH_AGENT), "biometrics-or-password");
        assert_eq!(cfg.effective_policy(NS_AWS_CREDENTIALS), POLICY_DENY_ALL);
        assert_eq!(cfg.effective_policy(NS_DOCKER_CREDENTIALS), "approve-all");
        assert_eq!(
            cfg.docker.registries,
            vec!["index.docker.io".to_string(), "ghcr.io".to_string()]
        );
        assert_eq!(cfg.ssh_scope(), "per-session");
        assert_eq!(cfg.ssh_session_ttl(), "1h");
    }

    /// The native-biometric knobs default null-aware (Go's `DefaultConfig`-then-
    /// overlay): an ABSENT `ssh.approval.{scope,session_ttl}` → `per-session` / `4h`;
    /// a bare `scope:` / `session_ttl:` (YAML null) also → the defaults. Mirrors Go's
    /// `applyDefaults` seeding.
    #[test]
    fn biometric_knobs_default_when_absent_or_null() {
        // Absent (only a policy under ssh.approval).
        let cfg = HostAgentConfig::parse("ssh:\n  approval:\n    policy: biometrics\n");
        assert_eq!(cfg.ssh_scope(), "per-session");
        assert_eq!(cfg.ssh_session_ttl(), "4h");
        // Explicit YAML null → still the defaults (null-aware read).
        let cfg =
            HostAgentConfig::parse("ssh:\n  approval:\n    scope:\n    session_ttl: null\n");
        assert_eq!(cfg.ssh_scope(), "per-session");
        assert_eq!(cfg.ssh_session_ttl(), "4h");
    }

    /// (H2) An EXPLICIT empty `scope: ""` / `session_ttl: ""` is kept verbatim — it
    /// overwrites the `DefaultConfig` default with `""` (Go's overlay), so the gate
    /// always-prompts and the raw `""` is what `session_ttl` audits. The null-aware
    /// read must NOT re-default an explicit empty string.
    #[test]
    fn biometric_knobs_keep_explicit_empty() {
        let cfg = HostAgentConfig::parse(
            "ssh:\n  approval:\n    scope: \"\"\n    session_ttl: \"\"\n",
        );
        assert_eq!(cfg.ssh_scope(), "");
        assert_eq!(cfg.ssh_session_ttl(), "");
    }

    /// A non-default scope/ttl round-trips verbatim through the accessors.
    #[test]
    fn biometric_knobs_read_explicit_values() {
        let cfg = HostAgentConfig::parse(
            "ssh:\n  approval:\n    scope: per-shed\n    session_ttl: 30m\n",
        );
        assert_eq!(cfg.ssh_scope(), "per-shed");
        assert_eq!(cfg.ssh_session_ttl(), "30m");
    }

    /// Mirrors `config_test.go:TestLoadConfigDeprecatedDesktopKeysIgnored`. A config
    /// that still sets the deprecated `desktop.*` keys with EXPLICIT ZERO/false values
    /// LOADS OK (exit 0) — the Rust reader ignores the block entirely, and validate
    /// touches only policies/aws/approval_timeout. (P: H3) this is unit-owned: "still
    /// loads" has no live exit-code cell. (P: H4) warnings are op-log-only and not
    /// ported. Driven through the real `load` (parse → validate) on a temp file.
    #[test]
    fn deprecated_desktop_keys_load_ok() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        std::fs::write(
            &path,
            "server: http://localhost:8080\n\
             desktop:\n  enabled: false\n  socket_path: ~/somewhere/else.sock\n  timeout_ms: 0\n",
        )
        .unwrap();
        let cfg =
            HostAgentConfig::load(path.to_str().unwrap()).expect("deprecated desktop keys load ok");
        // The budget still comes from approval_timeout (defaulted 25s), not timeout_ms.
        assert_eq!(cfg.approval_timeout(), Duration::from_secs(25));
    }

    /// Validate runs in Go's EXACT order (`config.go:487-505`): ssh → aws → docker
    /// policy → aws.validate(sheds → mode → …) → approval_timeout. With multiple
    /// errors present, the FIRST in that order surfaces.
    #[test]
    fn validate_check_order_is_go() {
        // ssh policy is checked before aws.mode before approval_timeout: a config bad
        // on all three surfaces the SSH error first.
        let err = validate_yaml(
            "ssh:\n  approval:\n    policy: maybe\naws:\n  mode: bogus\napproval_timeout: nonsense\n",
        )
        .expect_err("multi-error config");
        assert!(err.starts_with("ssh.approval.policy"), "{err:?}");
        // With ssh valid, aws.validate (mode) beats approval_timeout.
        let err = validate_yaml("aws:\n  mode: bogus\napproval_timeout: nonsense\n")
            .expect_err("aws + timeout bad");
        assert!(err.starts_with("aws.mode"), "{err:?}");
    }

    /// Wiring proof: a validate-failing config FILE surfaces through `load` as an
    /// `Err` (the same path `main.rs` maps to exit 1, mirroring `main.go:82-86`).
    #[test]
    fn load_rejects_invalid_config() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.yaml");
        std::fs::write(&path, "ssh:\n  approval:\n    policy: maybe\n").unwrap();
        assert!(
            HostAgentConfig::load(path.to_str().unwrap()).is_err(),
            "invalid policy config must fail load (main.rs exit 1)"
        );
    }

    /// The Rust half of the `config_validate` golden — reads the SAME shared fixture
    /// the Go runner reads (`cmd/shed-host-agent/golden_test.go:TestGoldenConfigValidate`,
    /// via the production `LoadConfig`), driving `try_parse` → `validate` (the
    /// `load = parse → validate` chain minus the file I/O). Pins that both impls agree
    /// on valid-vs-invalid and (for the semantic errors) share the PER-VECTOR message
    /// substring. Malformed-YAML + duplicate-key vectors pin only `{valid:false}` (the
    /// message body is yaml-lib specific — docker suffix precedent). In-crate (the
    /// `aws_resolve`/`docker_resolve` precedent — binary crate, no lib).
    #[test]
    fn golden_config_validate() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/config_validate.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let want_valid = v["valid"].as_bool().unwrap();
            let yaml = v["config_yaml"].as_str().unwrap();
            // load = parse → validate; a parse error (malformed / duplicate-key) OR a
            // validate error both mean "not valid" (Go's LoadConfig fails at either).
            let result: Result<(), String> = match HostAgentConfig::try_parse(yaml) {
                Err(e) => Err(e),
                Ok(cfg) => cfg.validate(),
            };
            assert_eq!(result.is_ok(), want_valid, "valid {name:?}: {result:?}");
            if let Some(sub) = v["error_substring"].as_str() {
                let err = result.expect_err("substring vector must produce an error");
                assert!(err.contains(sub), "vector {name:?}: {err:?} lacks {sub:?}");
            }
        }
    }

    // ---- D2 bool alternate forms + D3 selector-map exit-1 ------------------------

    /// `parse_yaml_bool` resolves EXACTLY yaml.v3's plain-scalar bool set (the D2 FIX,
    /// orchestrator-probed + re-verified in this worktree): the three canonical case
    /// forms of the YAML-1.1 bool tokens resolve; a non-canonical case (`tRUe`/`yEs`),
    /// `nonsense`, `1`, `0` do NOT (yaml.v3 errors — the D6 residue, `None` here).
    #[test]
    fn parse_yaml_bool_matches_yaml_v3_resolved_set() {
        for t in [
            "true", "True", "TRUE", "yes", "Yes", "YES", "on", "On", "ON", "y", "Y",
        ] {
            assert_eq!(parse_yaml_bool(t), Some(true), "{t:?} → true");
        }
        for f in [
            "false", "False", "FALSE", "no", "No", "NO", "off", "Off", "OFF", "n", "N",
        ] {
            assert_eq!(parse_yaml_bool(f), Some(false), "{f:?} → false");
        }
        // The D6 residue: yaml.v3 ERRORS on these (Rust can't without a typed layer → None).
        for e in ["nonsense", "1", "0", "tRUe", "yEs", "", "2", "yess"] {
            assert_eq!(parse_yaml_bool(e), None, "{e:?} → residue None");
        }
    }

    /// Wire behavior of the D2 FIX through the real reader: `logging.enabled: off`/`no`
    /// now reads OFF (was ON via the old `!= "false"`), `docker.allow_all: yes`/`on`/
    /// `True` now reads ON (was force-off via the old `== "true"`), and the residue
    /// forms preserve the pre-D2 lenient outcome (`logging` stays ON, `allow_all` reads
    /// force-off).
    #[test]
    fn bool_alternate_forms_wire_through_reader() {
        // logging.enabled resolving-false forms → OFF.
        for off in ["off", "no", "false", "No", "OFF", "n", "N"] {
            let cfg = HostAgentConfig::parse(&format!("logging:\n  enabled: {off}\n"));
            assert!(!cfg.logging_enabled, "logging.enabled: {off} → off");
        }
        // logging.enabled resolving-true forms + absent → ON.
        for on in ["on", "yes", "true", "Yes", "TRUE", "y"] {
            let cfg = HostAgentConfig::parse(&format!("logging:\n  enabled: {on}\n"));
            assert!(cfg.logging_enabled, "logging.enabled: {on} → on");
        }
        assert!(HostAgentConfig::parse("").logging_enabled, "absent → default on");
        // Residue: a non-resolvable value keeps the pre-D2 lenient default (ON).
        for res in ["nonsense", "1", "0"] {
            let cfg = HostAgentConfig::parse(&format!("logging:\n  enabled: {res}\n"));
            assert!(cfg.logging_enabled, "logging.enabled: {res} → residue on");
        }
        // docker.allow_all resolving-true forms → force-on.
        for on in ["yes", "on", "true", "True", "ON"] {
            let cfg = HostAgentConfig::parse(&format!("docker:\n  allow_all: {on}\n"));
            assert!(cfg.docker.allow_all, "docker.allow_all: {on} → on");
        }
        // docker.allow_all resolving-false + residue → off.
        for off in ["no", "off", "false", "nonsense", "1", "0"] {
            let cfg = HostAgentConfig::parse(&format!("docker:\n  allow_all: {off}\n"));
            assert!(!cfg.docker.allow_all, "docker.allow_all: {off} → off");
        }
    }

    /// D3: a MAP-valued `discovery.servers:` is rejected by `try_parse` with the exact
    /// Go `ServerSelector.UnmarshalYAML` message (`config.go:107`) — so `load` → exit 1,
    /// matching Go (which errored → select-ALL was the pre-D3 Rust fallback). The valid
    /// selector forms (all / one / list / none / bare-null) still parse.
    #[test]
    fn discovery_servers_map_value_rejected() {
        let err = HostAgentConfig::try_parse("discovery:\n  servers:\n    web: {}\n")
            .expect_err("map-valued servers rejected");
        assert_eq!(
            err,
            "discovery.servers must be \"all\" or a list of server names"
        );
        // A nested-map form errors too.
        assert!(HostAgentConfig::try_parse("discovery:\n  servers:\n    web:\n      x: y\n").is_err());
        // The valid forms still parse (all / one / list / explicit-none / bare null).
        for ok in [
            "discovery:\n  servers: all\n",
            "discovery:\n  servers: mini2\n",
            "discovery:\n  servers: [mini2, mini3]\n",
            "discovery:\n  servers: []\n",
            "discovery:\n  servers:\n",
        ] {
            assert!(HostAgentConfig::try_parse(ok).is_ok(), "valid selector {ok:?}");
        }
    }

    /// The Rust half of the `config_bool_forms` golden — reads the SAME shared fixture
    /// the Go runner reads (`cmd/shed-host-agent/golden_test.go:TestGoldenConfigBoolForms`,
    /// which decodes `v: <value>` into a Go `bool` via yaml.v3, asserting resolve-vs-error).
    /// The Rust side routes each value through [`parse_yaml_bool`]: a fixture `resolved`
    /// of `true`/`false` must be `Some(bool)`; a `resolved` of `"error"` is the D6
    /// asymmetry — Go ERRORS (`!!str`/`!!int` into bool), Rust returns `None` (the
    /// stringly-typed reader can't error without a typed layer, so the residue is `None`,
    /// documented here, NOT a silent pass). In-crate (the `docker_resolve` precedent).
    #[test]
    fn golden_config_bool_forms() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../tests/host-agent-diff/fixtures/config_bool_forms.json");
        let raw = std::fs::read_to_string(&path).expect("read golden fixture");
        let fx: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(fx["protocol_version"], 1, "version skew");
        let vectors = fx["vectors"].as_array().unwrap();
        assert!(!vectors.is_empty(), "fixture has no vectors");
        for v in vectors {
            let value = v["value"].as_str().unwrap();
            let got = parse_yaml_bool(value);
            match &v["resolved"] {
                serde_json::Value::Bool(b) => {
                    assert_eq!(got, Some(*b), "value {value:?} → resolved {b}");
                }
                serde_json::Value::String(s) if s == "error" => {
                    // Go yaml.v3 errors here; Rust maps the error set to None (D6 residue).
                    assert_eq!(got, None, "value {value:?} → Go-errors / Rust-None residue");
                }
                other => panic!("value {value:?}: bad fixture `resolved` {other:?}"),
            }
        }
    }
}
