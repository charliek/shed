//! The credential-approval domain, ported from shed-desktop's
//! `ApprovalModels.swift`. These double as IPC wire shapes (snake_case), like
//! the rest of the models, so the approval queue + activity feed need no
//! separate DTOs.
//!
//! Timestamps (`ts`, `expires_at`) are carried VERBATIM as strings — flexible
//! parsing lives in `shed-app` (`timefmt`), off this pure crate, matching
//! `models.rs` and `token.rs`.

use serde::{Deserialize, Serialize};

// The defensive `null`/absent -> default deserializer is shared with the other
// wire DTOs (mirrors Swift's `decodeIfPresent(...) ?? default`).
use crate::models::null_default;

/// The credential namespaces the host agent brokers. Only `ssh-agent` is gated
/// today; the rest are audit-only (visible in the activity feed).
pub mod namespace {
    pub const SSH: &str = "ssh-agent";
    pub const AWS: &str = "aws-credentials";
    pub const DOCKER: &str = "docker-credentials";
    pub const ALL: [&str; 3] = [SSH, AWS, DOCKER];
}

/// The default approval grant duration — used when the duration field is empty
/// or unparseable, and as the pre-fill default.
pub const DEFAULT_APPROVAL_TTL: &str = "2h";

/// A credential-approval request delegated from shed-host-agent. The app only
/// ever sees metadata — never key material.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ApprovalRequest {
    pub id: String,
    pub ts: String,
    /// The shed server this came from. Omitted by the host agent in
    /// single-server mode (shed-extensions #21), so decode it defensively
    /// (absent OR null -> ""); the rest are always present on the wire.
    #[serde(default, deserialize_with = "null_default")]
    pub server: String,
    /// `ssh-agent` | `aws-credentials` | `docker-credentials`.
    pub namespace: String,
    /// `sign` | `get_credentials` | …
    pub op: String,
    pub shed: String,
    /// Human-readable (key type, role, registry).
    pub detail: String,
    pub expires_at: String,
}

impl ApprovalRequest {
    /// "server/shed" when multi-server, else just the shed name.
    pub fn qualified_shed(&self) -> String {
        if self.server.is_empty() {
            self.shed.clone()
        } else {
            format!("{}/{}", self.server, self.shed)
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ApprovalDecision {
    Approve,
    Deny,
}

impl ApprovalDecision {
    /// The matching policy action (AWS/Docker live mode -> a namespace rule).
    pub fn policy_action(self) -> PolicyAction {
        match self {
            ApprovalDecision::Approve => PolicyAction::Approve,
            ApprovalDecision::Deny => PolicyAction::Deny,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum DecidedBy {
    Policy,
    User,
    Touchid,
    Timeout,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum AuditSource {
    #[serde(rename = "host-agent")]
    HostAgent,
    App,
    Lifecycle,
    Rc,
}

/// An entry in the app's own audit store — a superset of the host agent's JSON
/// log (adds `id`, `source`, `policy`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AuditEntry {
    pub id: String,
    pub ts: String,
    pub source: AuditSource,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub server: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub shed: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub ns: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub op: Option<String>,
    /// `ok` | `denied` | `error` | …
    pub result: String,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub detail: Option<String>,
    /// Machine-readable failure cause (`REGISTRY_NOT_ALLOWED`, …); `None` on
    /// success/older agents.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub code: Option<String>,
    /// Short host-side explanation for a non-ok result; `None` on success/older
    /// agents.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub approval: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub policy: Option<String>,
}

impl AuditEntry {
    /// The audit namespace for egress-control decisions, matching the
    /// host-agent's stamping (shed-extensions egress subscriber).
    pub const EGRESS_NAMESPACE: &'static str = "egress";

    /// True when this entry is an egress-control decision (ns == "egress").
    pub fn is_egress(&self) -> bool {
        self.ns.as_deref() == Some(Self::EGRESS_NAMESPACE)
    }
}

// ---- Policy ----

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PolicyAction {
    Approve,
    Deny,
    Prompt,
}

/// The biometric prompt the app applies before approving. `None` = approve
/// straight from the UI with no biometric.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum PolicyGate {
    /// Touch ID only (mac) / fail-closed on Linux until fprintd.
    #[serde(rename = "biometrics")]
    Biometrics,
    /// Touch ID / Watch / password (mac) — polkit native password (Linux).
    #[serde(rename = "biometrics-or-password")]
    BiometricsOrPassword,
    /// Approve with a plain button press, no gate.
    #[serde(rename = "none")]
    None,
}

impl PolicyGate {
    /// Whether this gate shows a biometric/native-auth prompt.
    pub fn is_biometric(self) -> bool {
        self != PolicyGate::None
    }
}

/// How the app prompts for an SSH approval when the host-agent delegates
/// (policy: shed-desktop). The preferences "method" dropdown.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum ApprovalMethod {
    #[serde(rename = "biometrics-or-password")]
    BiometricsOrPassword,
    #[serde(rename = "biometrics")]
    Biometrics,
    /// No biometric — a plain Approve button.
    #[serde(rename = "prompt")]
    Prompt,
}

impl ApprovalMethod {
    pub fn gate(self) -> PolicyGate {
        match self {
            ApprovalMethod::BiometricsOrPassword => PolicyGate::BiometricsOrPassword,
            ApprovalMethod::Biometrics => PolicyGate::Biometrics,
            ApprovalMethod::Prompt => PolicyGate::None,
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            ApprovalMethod::BiometricsOrPassword => "Touch ID or password",
            ApprovalMethod::Biometrics => "Touch ID only",
            ApprovalMethod::Prompt => "Prompt (no Touch ID)",
        }
    }

    pub const ALL: [ApprovalMethod; 3] = [
        ApprovalMethod::BiometricsOrPassword,
        ApprovalMethod::Biometrics,
        ApprovalMethod::Prompt,
    ];
}

/// The scope/duration a user picks when approving an SSH request, and the
/// per-provider default pre-filled into the card.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum ApprovalScope {
    PerRequest,
    PerSession,
    PerShed,
}

impl ApprovalScope {
    /// The wire scope string reported to the host agent for its audit.
    pub fn wire(self) -> &'static str {
        match self {
            ApprovalScope::PerRequest => "per-request",
            ApprovalScope::PerSession => "per-session",
            ApprovalScope::PerShed => "per-shed",
        }
    }
}

/// The SSH approval policy, ordered most -> least permissive. `alwaysAllow`/
/// `alwaysDeny` decide every sign outright (no prompt); `perShedAllow` prompts
/// once per shed then grants until restart; `timeBasedAllow` prompts then grants
/// for the duration; `alwaysAsk` prompts every time.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum SshApprovalPolicy {
    AlwaysAllow,
    PerShedAllow,
    TimeBasedAllow,
    AlwaysAsk,
    AlwaysDeny,
}

impl SshApprovalPolicy {
    pub fn label(self) -> &'static str {
        match self {
            SshApprovalPolicy::AlwaysAllow => "Always Allow",
            SshApprovalPolicy::PerShedAllow => "Per Shed Allow",
            SshApprovalPolicy::TimeBasedAllow => "Time Based Allow",
            SshApprovalPolicy::AlwaysAsk => "Always Ask",
            SshApprovalPolicy::AlwaysDeny => "Always Deny",
        }
    }

    /// Only the time-based policy uses the duration field.
    pub fn uses_duration(self) -> bool {
        self == SshApprovalPolicy::TimeBasedAllow
    }

    /// The provider-level (namespace) action this policy installs: the two
    /// "Always" options decide outright with no prompt; the rest prompt (and the
    /// chosen scope governs the grant created when the user approves).
    pub fn namespace_action(self) -> PolicyAction {
        match self {
            SshApprovalPolicy::AlwaysAllow => PolicyAction::Approve,
            SshApprovalPolicy::AlwaysDeny => PolicyAction::Deny,
            SshApprovalPolicy::PerShedAllow
            | SshApprovalPolicy::TimeBasedAllow
            | SshApprovalPolicy::AlwaysAsk => PolicyAction::Prompt,
        }
    }

    /// Whether this policy prompts the user — so Method (Touch ID) and Duration
    /// are relevant only for these.
    pub fn prompts(self) -> bool {
        self.namespace_action() == PolicyAction::Prompt
    }

    /// The default grant scope this policy applies when the user approves a
    /// prompt (`None` for the non-prompting Always rules, which never reach a card).
    pub fn default_scope(self) -> Option<ApprovalScope> {
        match self {
            SshApprovalPolicy::PerShedAllow => Some(ApprovalScope::PerShed),
            SshApprovalPolicy::TimeBasedAllow => Some(ApprovalScope::PerSession),
            SshApprovalPolicy::AlwaysAsk => Some(ApprovalScope::PerRequest),
            SshApprovalPolicy::AlwaysAllow | SshApprovalPolicy::AlwaysDeny => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PolicyScope {
    Default,
    Namespace,
    Shed,
    Session,
}

impl PolicyScope {
    /// The wire scope label recorded in the audit trail (the `policy` field).
    pub fn wire(self) -> &'static str {
        match self {
            PolicyScope::Default => "default",
            PolicyScope::Namespace => "namespace",
            PolicyScope::Shed => "shed",
            PolicyScope::Session => "session",
        }
    }
}

/// A single policy rule. The engine resolves the most specific match. `server`
/// scopes a per-shed rule to one shed server ("" = the single/unnamed server);
/// `None` means any server (also how a rule predating the multi-server `server`
/// dimension decodes).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PolicyRule {
    pub scope: PolicyScope,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub server: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub namespace: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub shed: Option<String>,
    pub action: PolicyAction,
    #[serde(default = "default_gate")]
    pub gate: PolicyGate,
}

fn default_gate() -> PolicyGate {
    PolicyGate::BiometricsOrPassword
}

/// A pending approval as published to the UI: the request plus the decided gate
/// (drives the fingerprint icon) and the per-provider scope/TTL defaults the card
/// pre-fills. Encodes the request fields inline (so `approvals.list` keeps
/// id/server/…) plus `gate`/`default_scope`/`default_ttl`, for IPC drivability.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct PendingApprovalItem {
    #[serde(flatten)]
    pub request: ApprovalRequest,
    pub gate: PolicyGate,
    pub default_scope: ApprovalScope,
    pub default_ttl: String,
}

impl PendingApprovalItem {
    pub fn new(
        request: ApprovalRequest,
        gate: PolicyGate,
        default_scope: ApprovalScope,
        default_ttl: String,
    ) -> Self {
        Self {
            request,
            gate,
            default_scope,
            default_ttl,
        }
    }

    /// What an Approve tap sends — applies the configured policy's grant
    /// scope/TTL. Shared so surfaces can't drift on what "Approve" means.
    pub fn approve_choice(&self) -> ApprovalChoice {
        ApprovalChoice {
            decision: ApprovalDecision::Approve,
            scope: Some(self.default_scope),
            ttl: Some(self.default_ttl.clone()),
            persist: false,
        }
    }

    /// What a Deny tap sends — this request only (never persists a rule).
    pub fn deny_choice(&self) -> ApprovalChoice {
        ApprovalChoice {
            decision: ApprovalDecision::Deny,
            scope: None,
            ttl: None,
            persist: false,
        }
    }
}

/// What the user chose on an SSH approval card (or a quick approve/deny). For
/// AWS/Docker the decision comes from policy, not a card.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApprovalChoice {
    pub decision: ApprovalDecision,
    /// Approve only: per-request (once) vs a timed grant (per-session/per-shed).
    pub scope: Option<ApprovalScope>,
    /// TTL shorthand for a timed grant (e.g. "1h").
    pub ttl: Option<String>,
    /// Persist a per-shed rule (always-allow when approve, always-deny when deny).
    pub persist: bool,
}

/// A session-scoped "approve for this session" grant key (server+namespace+shed).
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct SessionGrantKey {
    pub server: String,
    pub namespace: String,
    pub shed: String,
}

impl SessionGrantKey {
    pub fn new(
        server: impl Into<String>,
        namespace: impl Into<String>,
        shed: impl Into<String>,
    ) -> Self {
        Self {
            server: server.into(),
            namespace: namespace.into(),
            shed: shed.into(),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PolicyDecision {
    pub action: PolicyAction,
    pub gate: PolicyGate,
    /// The scope of the rule that decided this, for the audit trail.
    pub applied_scope: PolicyScope,
}

/// Parse a TTL shorthand like `45s`, `4m`, `3h`, `1d` into seconds. Returns
/// `None` for empty/invalid input so the caller can fall back to a default.
pub fn ttl_shorthand_seconds(raw: &str) -> Option<i64> {
    let s = raw.trim();
    let last = s.chars().last()?;
    let unit: i64 = match last.to_ascii_lowercase() {
        's' => 1,
        'm' => 60,
        'h' => 3600,
        'd' => 86400,
        _ => return None,
    };
    let n: i64 = s[..s.len() - last.len_utf8()].parse().ok()?;
    (n > 0).then_some(n * unit)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};

    #[test]
    fn decode_request_with_missing_server_defaults_empty_string() {
        // The host agent omits `server` in single-server mode (F12/defensive).
        let r: ApprovalRequest = serde_json::from_str(
            r#"{"id":"r1","ts":"t","namespace":"ssh-agent","op":"sign","shed":"s","detail":"d","expires_at":"e"}"#,
        )
        .unwrap();
        assert_eq!(r.server, "");
    }

    #[test]
    fn decode_request_with_null_server_defaults_empty_string() {
        let r: ApprovalRequest = serde_json::from_str(
            r#"{"id":"r1","ts":"t","server":null,"namespace":"ssh-agent","op":"sign","shed":"s","detail":"d","expires_at":"e"}"#,
        )
        .unwrap();
        assert_eq!(r.server, "");
    }

    #[test]
    fn decode_request_keeps_present_server() {
        let r: ApprovalRequest = serde_json::from_str(
            r#"{"id":"r1","ts":"t","server":"mini3","namespace":"ssh-agent","op":"sign","shed":"s","detail":"d","expires_at":"e"}"#,
        )
        .unwrap();
        assert_eq!(r.server, "mini3");
        assert_eq!(r.qualified_shed(), "mini3/s");
    }

    #[test]
    fn qualified_shed_is_bare_in_single_server_mode() {
        let mut r: ApprovalRequest = serde_json::from_str(
            r#"{"id":"r1","ts":"t","namespace":"ssh-agent","op":"sign","shed":"s","detail":"d","expires_at":"e"}"#,
        )
        .unwrap();
        assert_eq!(r.qualified_shed(), "s");
        r.server = "mini3".into();
        assert_eq!(r.qualified_shed(), "mini3/s");
    }

    #[test]
    fn ttl_shorthand_parses_units_and_rejects_junk() {
        assert_eq!(ttl_shorthand_seconds("45s"), Some(45));
        assert_eq!(ttl_shorthand_seconds("4m"), Some(240));
        assert_eq!(ttl_shorthand_seconds("3h"), Some(10800));
        assert_eq!(ttl_shorthand_seconds("1d"), Some(86400));
        assert_eq!(ttl_shorthand_seconds(" 2H "), Some(7200)); // trimmed + lowercased
        assert_eq!(ttl_shorthand_seconds(""), None);
        assert_eq!(ttl_shorthand_seconds("garbage"), None);
        assert_eq!(ttl_shorthand_seconds("0h"), None); // must be > 0
        assert_eq!(ttl_shorthand_seconds("-1h"), None);
        assert_eq!(ttl_shorthand_seconds("5"), None); // no unit
    }

    #[test]
    fn ssh_policy_mappings() {
        assert_eq!(
            SshApprovalPolicy::AlwaysAllow.namespace_action(),
            PolicyAction::Approve
        );
        assert_eq!(
            SshApprovalPolicy::AlwaysDeny.namespace_action(),
            PolicyAction::Deny
        );
        assert_eq!(
            SshApprovalPolicy::AlwaysAsk.namespace_action(),
            PolicyAction::Prompt
        );
        assert!(SshApprovalPolicy::TimeBasedAllow.uses_duration());
        assert!(!SshApprovalPolicy::PerShedAllow.uses_duration());
        assert_eq!(
            SshApprovalPolicy::PerShedAllow.default_scope(),
            Some(ApprovalScope::PerShed)
        );
        assert_eq!(
            SshApprovalPolicy::TimeBasedAllow.default_scope(),
            Some(ApprovalScope::PerSession)
        );
        assert_eq!(SshApprovalPolicy::AlwaysAllow.default_scope(), None);
    }

    #[test]
    fn approval_method_maps_to_gate() {
        assert_eq!(
            ApprovalMethod::BiometricsOrPassword.gate(),
            PolicyGate::BiometricsOrPassword
        );
        assert_eq!(ApprovalMethod::Biometrics.gate(), PolicyGate::Biometrics);
        assert_eq!(ApprovalMethod::Prompt.gate(), PolicyGate::None);
        assert!(!PolicyGate::None.is_biometric());
        assert!(PolicyGate::BiometricsOrPassword.is_biometric());
    }

    #[test]
    fn enum_wire_values() {
        assert_eq!(
            serde_json::to_value(ApprovalDecision::Approve).unwrap(),
            json!("approve")
        );
        assert_eq!(
            serde_json::to_value(PolicyGate::BiometricsOrPassword).unwrap(),
            json!("biometrics-or-password")
        );
        assert_eq!(
            serde_json::to_value(PolicyGate::None).unwrap(),
            json!("none")
        );
        assert_eq!(
            serde_json::to_value(ApprovalScope::PerSession).unwrap(),
            json!("per-session")
        );
        assert_eq!(
            serde_json::to_value(SshApprovalPolicy::PerShedAllow).unwrap(),
            json!("per-shed-allow")
        );
        assert_eq!(
            serde_json::to_value(AuditSource::HostAgent).unwrap(),
            json!("host-agent")
        );
        assert_eq!(
            serde_json::to_value(DecidedBy::Touchid).unwrap(),
            json!("touchid")
        );
    }

    #[test]
    fn pending_item_serializes_flat_wire_shape() {
        let req = ApprovalRequest {
            id: "r1".into(),
            ts: "t".into(),
            server: "mini3".into(),
            namespace: "ssh-agent".into(),
            op: "sign".into(),
            shed: "s".into(),
            detail: "d".into(),
            expires_at: "e".into(),
        };
        let item = PendingApprovalItem::new(
            req,
            PolicyGate::BiometricsOrPassword,
            ApprovalScope::PerSession,
            "2h".into(),
        );
        let v: Value = serde_json::to_value(&item).unwrap();
        // Request fields inlined (flatten) + the decided gate/scope/ttl.
        assert_eq!(v["id"], "r1");
        assert_eq!(v["server"], "mini3");
        assert_eq!(v["namespace"], "ssh-agent");
        assert_eq!(v["expires_at"], "e");
        assert_eq!(v["gate"], "biometrics-or-password");
        assert_eq!(v["default_scope"], "per-session");
        assert_eq!(v["default_ttl"], "2h");
    }

    #[test]
    fn audit_entry_skips_none_optionals() {
        let e = AuditEntry {
            id: "r1".into(),
            ts: "t".into(),
            source: AuditSource::App,
            server: None,
            shed: Some("s".into()),
            ns: None,
            op: None,
            result: "ok".into(),
            detail: None,
            code: None,
            reason: None,
            approval: Some("shed-desktop".into()),
            policy: Some("manual".into()),
        };
        let v: Value = serde_json::to_value(&e).unwrap();
        assert!(v.get("server").is_none()); // None skipped
        assert!(v.get("ns").is_none());
        assert_eq!(v["shed"], "s");
        assert_eq!(v["result"], "ok");
        assert_eq!(v["approval"], "shed-desktop");
    }

    #[test]
    fn audit_entry_egress_detection() {
        let mut e = AuditEntry {
            id: "1".into(),
            ts: "t".into(),
            source: AuditSource::HostAgent,
            server: None,
            shed: None,
            ns: Some("egress".into()),
            op: None,
            result: "ok".into(),
            detail: None,
            code: None,
            reason: None,
            approval: None,
            policy: None,
        };
        assert!(e.is_egress());
        e.ns = Some("ssh-agent".into());
        assert!(!e.is_egress());
    }

    #[test]
    fn pending_item_choices() {
        let req = ApprovalRequest {
            id: "r1".into(),
            ts: "t".into(),
            server: "".into(),
            namespace: "ssh-agent".into(),
            op: "sign".into(),
            shed: "s".into(),
            detail: "d".into(),
            expires_at: "e".into(),
        };
        let item =
            PendingApprovalItem::new(req, PolicyGate::None, ApprovalScope::PerShed, "2h".into());
        let a = item.approve_choice();
        assert_eq!(a.decision, ApprovalDecision::Approve);
        assert_eq!(a.scope, Some(ApprovalScope::PerShed));
        assert_eq!(a.ttl.as_deref(), Some("2h"));
        assert!(!a.persist);
        let d = item.deny_choice();
        assert_eq!(d.decision, ApprovalDecision::Deny);
        assert!(d.scope.is_none());
    }
}
