// PolicyEngine.swift — the pure approval-policy decision (M3, spec §7.3).
//
// Given a request, the configured rules, and the set of currently-valid
// session grants, returns approve | deny | prompt with the gate to apply.
// No I/O — the whole matrix is unit-testable without a host agent.

import Foundation

public struct PolicyEngine: Sendable {
    public var rules: [PolicyRule]

    public init(rules: [PolicyRule]) {
        self.rules = rules
    }

    /// Decide. Precedence (most specific wins):
    ///   session grant > per-shed rule > per-namespace rule > default rule.
    /// `sessionGrants` must already be filtered to the currently-valid set
    /// by the caller (keeps this pure).
    public func decide(for req: ApprovalRequest, sessionGrants: Set<SessionGrantKey>) -> PolicyDecision {
        if sessionGrants.contains(SessionGrantKey(server: req.server, namespace: req.namespace, shed: req.shed)) {
            return PolicyDecision(action: .approve, gate: .none, appliedScope: .session)
        }
        // Per-shed rule: match the shed, and the server too unless the rule is
        // server-agnostic (nil server = any server).
        if let r = rules.first(where: { $0.scope == .shed && $0.shed == req.shed && ($0.server == nil || $0.server == req.server) }) {
            return PolicyDecision(action: r.action, gate: r.gate, appliedScope: .shed)
        }
        if let r = rules.first(where: { $0.scope == .namespace && $0.namespace == req.namespace }) {
            return PolicyDecision(action: r.action, gate: r.gate, appliedScope: .namespace)
        }
        if let r = rules.first(where: { $0.scope == .default }) {
            return PolicyDecision(action: r.action, gate: r.gate, appliedScope: .default)
        }
        // No rule at all → fail safe to a Touch ID prompt.
        return PolicyDecision(action: .prompt, gate: .biometricsOrPassword, appliedScope: .default)
    }
}
