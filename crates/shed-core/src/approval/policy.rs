//! The pure approval-policy decision, ported from `PolicyEngine.swift`.
//!
//! Given a request, the configured rules, and the set of currently-valid session
//! grants, returns approve | deny | prompt with the gate to apply. No I/O — the
//! whole matrix is unit-testable without a host agent.

use std::collections::HashSet;

use super::models::{
    ApprovalRequest, PolicyAction, PolicyDecision, PolicyGate, PolicyRule, PolicyScope,
    SessionGrantKey,
};

#[derive(Debug, Clone, Default)]
pub struct PolicyEngine {
    pub rules: Vec<PolicyRule>,
}

impl PolicyEngine {
    pub fn new(rules: Vec<PolicyRule>) -> Self {
        Self { rules }
    }

    /// Decide. Precedence (most specific wins):
    ///   session grant > per-shed rule > per-namespace rule > default rule.
    /// `session_grants` must already be filtered to the currently-valid set by
    /// the caller (keeps this pure).
    pub fn decide(
        &self,
        req: &ApprovalRequest,
        session_grants: &HashSet<SessionGrantKey>,
    ) -> PolicyDecision {
        let key = SessionGrantKey::new(req.server.clone(), req.namespace.clone(), req.shed.clone());
        if session_grants.contains(&key) {
            return PolicyDecision {
                action: PolicyAction::Approve,
                gate: PolicyGate::None,
                applied_scope: PolicyScope::Session,
            };
        }
        // Per-shed rule: match the shed, and the server too unless the rule is
        // server-agnostic (None server = any server).
        if let Some(r) = self.rules.iter().find(|r| {
            r.scope == PolicyScope::Shed
                && r.shed.as_deref() == Some(req.shed.as_str())
                && (r.server.is_none() || r.server.as_deref() == Some(req.server.as_str()))
        }) {
            return PolicyDecision {
                action: r.action,
                gate: r.gate,
                applied_scope: PolicyScope::Shed,
            };
        }
        if let Some(r) = self.rules.iter().find(|r| {
            r.scope == PolicyScope::Namespace
                && r.namespace.as_deref() == Some(req.namespace.as_str())
        }) {
            return PolicyDecision {
                action: r.action,
                gate: r.gate,
                applied_scope: PolicyScope::Namespace,
            };
        }
        if let Some(r) = self.rules.iter().find(|r| r.scope == PolicyScope::Default) {
            return PolicyDecision {
                action: r.action,
                gate: r.gate,
                applied_scope: PolicyScope::Default,
            };
        }
        // No rule at all -> fail safe to a native-auth prompt.
        PolicyDecision {
            action: PolicyAction::Prompt,
            gate: PolicyGate::BiometricsOrPassword,
            applied_scope: PolicyScope::Default,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::approval::models::namespace;

    fn req(server: &str, ns: &str, shed: &str) -> ApprovalRequest {
        ApprovalRequest {
            id: "r1".into(),
            ts: "2026-07-03T00:00:00Z".into(),
            server: server.into(),
            namespace: ns.into(),
            op: "sign".into(),
            shed: shed.into(),
            detail: "".into(),
            expires_at: "2026-07-03T00:00:30Z".into(),
        }
    }

    fn no_grants() -> HashSet<SessionGrantKey> {
        HashSet::new()
    }

    #[test]
    fn no_rules_fails_safe_to_native_prompt() {
        let e = PolicyEngine::new(vec![]);
        let d = e.decide(&req("", namespace::SSH, "s"), &no_grants());
        assert_eq!(d.action, PolicyAction::Prompt);
        assert_eq!(d.gate, PolicyGate::BiometricsOrPassword);
        assert_eq!(d.applied_scope, PolicyScope::Default);
    }

    #[test]
    fn session_grant_beats_every_rule() {
        // A deny-all default rule would deny, but a live grant wins.
        let e = PolicyEngine::new(vec![PolicyRule {
            scope: PolicyScope::Default,
            server: None,
            namespace: None,
            shed: None,
            action: PolicyAction::Deny,
            gate: PolicyGate::None,
        }]);
        let mut grants = HashSet::new();
        grants.insert(SessionGrantKey::new("srv", namespace::SSH, "s"));
        let d = e.decide(&req("srv", namespace::SSH, "s"), &grants);
        assert_eq!(d.action, PolicyAction::Approve);
        assert_eq!(d.gate, PolicyGate::None);
        assert_eq!(d.applied_scope, PolicyScope::Session);
    }

    #[test]
    fn per_shed_rule_beats_namespace_and_default() {
        let e = PolicyEngine::new(vec![
            PolicyRule {
                scope: PolicyScope::Default,
                server: None,
                namespace: None,
                shed: None,
                action: PolicyAction::Deny,
                gate: PolicyGate::None,
            },
            PolicyRule {
                scope: PolicyScope::Namespace,
                server: None,
                namespace: Some(namespace::SSH.into()),
                shed: None,
                action: PolicyAction::Prompt,
                gate: PolicyGate::BiometricsOrPassword,
            },
            PolicyRule {
                scope: PolicyScope::Shed,
                server: Some("srv".into()),
                namespace: None,
                shed: Some("s".into()),
                action: PolicyAction::Approve,
                gate: PolicyGate::None,
            },
        ]);
        let d = e.decide(&req("srv", namespace::SSH, "s"), &no_grants());
        assert_eq!(d.action, PolicyAction::Approve);
        assert_eq!(d.applied_scope, PolicyScope::Shed);
    }

    #[test]
    fn per_shed_server_specific_does_not_match_other_server() {
        // F12: a rule scoped to server "mini3" must NOT cover shed "s" on "studio".
        let e = PolicyEngine::new(vec![PolicyRule {
            scope: PolicyScope::Shed,
            server: Some("mini3".into()),
            namespace: None,
            shed: Some("s".into()),
            action: PolicyAction::Approve,
            gate: PolicyGate::None,
        }]);
        // mini3 -> approved by the rule.
        assert_eq!(
            e.decide(&req("mini3", namespace::SSH, "s"), &no_grants())
                .action,
            PolicyAction::Approve
        );
        // studio -> the rule doesn't apply; no other rule -> fail-safe prompt.
        let d = e.decide(&req("studio", namespace::SSH, "s"), &no_grants());
        assert_eq!(d.action, PolicyAction::Prompt);
        assert_eq!(d.applied_scope, PolicyScope::Default);
    }

    #[test]
    fn per_shed_server_agnostic_rule_matches_any_server() {
        // server = None => any server (wildcard).
        let e = PolicyEngine::new(vec![PolicyRule {
            scope: PolicyScope::Shed,
            server: None,
            namespace: None,
            shed: Some("s".into()),
            action: PolicyAction::Deny,
            gate: PolicyGate::None,
        }]);
        assert_eq!(
            e.decide(&req("anything", namespace::SSH, "s"), &no_grants())
                .action,
            PolicyAction::Deny
        );
    }

    #[test]
    fn namespace_rule_beats_default() {
        let e = PolicyEngine::new(vec![
            PolicyRule {
                scope: PolicyScope::Default,
                server: None,
                namespace: None,
                shed: None,
                action: PolicyAction::Prompt,
                gate: PolicyGate::BiometricsOrPassword,
            },
            PolicyRule {
                scope: PolicyScope::Namespace,
                server: None,
                namespace: Some(namespace::AWS.into()),
                shed: None,
                action: PolicyAction::Deny,
                gate: PolicyGate::None,
            },
        ]);
        let d = e.decide(&req("", namespace::AWS, "s"), &no_grants());
        assert_eq!(d.action, PolicyAction::Deny);
        assert_eq!(d.applied_scope, PolicyScope::Namespace);
    }
}
