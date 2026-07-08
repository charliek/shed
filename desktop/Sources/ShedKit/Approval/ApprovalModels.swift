// ApprovalModels.swift — the credential-approval domain (M3).
//
// These double as IPC wire shapes (snake_case CodingKeys), like the rest of
// the models, so the approval queue + activity feed need no separate DTOs.

import Foundation

/// A credential-approval request delegated from shed-host-agent. The app
/// only ever sees metadata — never key material.
public struct ApprovalRequest: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var ts: String
    public var server: String          // shed server this came from ("" in single-server mode)
    public var namespace: String       // ssh-agent | aws-credentials | docker-credentials
    public var op: String              // sign | get_credentials | …
    public var shed: String
    public var detail: String          // human-readable (key type, role, registry)
    public var expiresAt: String

    enum CodingKeys: String, CodingKey {
        case id, ts, server, namespace, op, shed, detail
        case expiresAt = "expires_at"
    }

    public init(id: String, ts: String, server: String = "", namespace: String, op: String, shed: String, detail: String, expiresAt: String) {
        self.id = id
        self.ts = ts
        self.server = server
        self.namespace = namespace
        self.op = op
        self.shed = shed
        self.detail = detail
        self.expiresAt = expiresAt
    }

    // `server` is omitted by the host agent in single-server mode (shed-extensions
    // #21), so decode it defensively; the rest are always present on the wire.
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        ts = try c.decode(String.self, forKey: .ts)
        server = try c.decodeIfPresent(String.self, forKey: .server) ?? ""
        namespace = try c.decode(String.self, forKey: .namespace)
        op = try c.decode(String.self, forKey: .op)
        shed = try c.decode(String.self, forKey: .shed)
        detail = try c.decode(String.self, forKey: .detail)
        expiresAt = try c.decode(String.self, forKey: .expiresAt)
    }

    public var expiresAtDate: Date? { DateFormatting.parseFlexibleTimestamp(expiresAt) }

    /// "server/shed" when multi-server, else just the shed name.
    public var qualifiedShed: String { server.isEmpty ? shed : "\(server)/\(shed)" }
}

public enum ApprovalDecision: String, Codable, Sendable {
    case approve, deny

    /// The matching policy action (AWS/Docker live mode → a namespace rule).
    public var policyAction: PolicyAction { self == .approve ? .approve : .deny }
}

/// The credential namespaces the host agent brokers. Only `ssh-agent` is
/// gated today; the rest are audit-only (visible in the activity feed).
public enum CredentialNamespace {
    public static let ssh = "ssh-agent"
    public static let aws = "aws-credentials"
    public static let docker = "docker-credentials"
    public static let all = [ssh, aws, docker]
}

public enum DecidedBy: String, Codable, Sendable {
    case policy, user, touchid, timeout
}

/// An entry in the app's own audit store — a superset of the host agent's
/// JSON log (adds `id`, `source`, `policy`).
public struct AuditEntry: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    public var ts: String
    public var source: AuditSource
    public var server: String?
    public var shed: String?
    public var ns: String?
    public var op: String?
    public var result: String          // ok | denied | error | …
    public var detail: String?
    public var code: String?           // machine-readable failure cause (REGISTRY_NOT_ALLOWED, …); nil on success/older agents
    public var reason: String?         // short host-side explanation for a non-ok result; nil on success/older agents
    public var approval: String?
    public var policy: String?

    public init(
        id: String, ts: String, source: AuditSource, server: String? = nil, shed: String? = nil, ns: String? = nil,
        op: String? = nil, result: String, detail: String? = nil, code: String? = nil, reason: String? = nil,
        approval: String? = nil, policy: String? = nil
    ) {
        self.id = id
        self.ts = ts
        self.source = source
        self.server = server
        self.shed = shed
        self.ns = ns
        self.op = op
        self.result = result
        self.detail = detail
        self.code = code
        self.reason = reason
        self.approval = approval
        self.policy = policy
    }

    /// Map a host-agent `event` frame into a stored entry (source = host-agent).
    public init(frame: AuditEventFrame) {
        self.init(
            id: frame.requestID ?? UUID().uuidString,
            ts: frame.ts ?? DateFormatting.nowISO8601(),
            source: .hostAgent, server: frame.server, shed: frame.shed, ns: frame.ns, op: frame.op,
            result: frame.result, detail: frame.detail, code: frame.code, reason: frame.reason, approval: frame.approval)
    }
}

public extension AuditEntry {
    /// The audit namespace for egress-control decisions, matching the
    /// host-agent's stamping (shed-extensions egress subscriber).
    static let egressNamespace = "egress"

    /// True when this entry is an egress-control decision (ns == "egress").
    var isEgress: Bool { ns == AuditEntry.egressNamespace }
}

public enum AuditSource: String, Codable, Sendable {
    case hostAgent = "host-agent"
    case app
    case lifecycle
    case rc
}

// MARK: - Policy

public enum PolicyAction: String, Codable, Sendable {
    case approve, deny, prompt
}

/// The biometric prompt the app applies before approving (issue: per-provider
/// approval). `none` = approve straight from the UI with no biometric.
public enum PolicyGate: String, Codable, Sendable {
    case biometrics                                          // Touch ID only
    case biometricsOrPassword = "biometrics-or-password"     // Touch ID / Watch / password
    case none

    /// Whether this gate shows a biometric prompt (and a fingerprint icon).
    public var isBiometric: Bool { self != .none }
}

/// How shed-desktop prompts for an SSH approval when the host-agent delegates
/// (policy: shed-desktop). The preferences "method" dropdown.
public enum ApprovalMethod: String, Codable, Sendable, CaseIterable {
    case biometricsOrPassword = "biometrics-or-password"
    case biometrics
    case prompt   // no biometric — a plain Approve button

    public var gate: PolicyGate {
        switch self {
        case .biometricsOrPassword: return .biometricsOrPassword
        case .biometrics: return .biometrics
        case .prompt: return .none
        }
    }

    public var label: String {
        switch self {
        case .biometricsOrPassword: return "Touch ID or password"
        case .biometrics: return "Touch ID only"
        case .prompt: return "Prompt (no Touch ID)"
        }
    }
}

/// The scope/duration a user picks when approving an SSH request, and the
/// per-provider default pre-filled into the card.
public enum ApprovalScope: String, Codable, Sendable, CaseIterable {
    case perRequest = "per-request"
    case perSession = "per-session"
    case perShed = "per-shed"

}

/// The SSH approval policy, ordered most → least permissive. Configured in
/// Preferences (it also labels that picker) and applied by the policy engine:
/// `alwaysAllow`/`alwaysDeny` decide every sign outright (no prompt);
/// `perShedAllow` prompts once per shed then grants until restart;
/// `timeBasedAllow` prompts then grants for the duration; `alwaysAsk` prompts
/// every time.
public enum SSHApprovalPolicy: String, Codable, Sendable, CaseIterable, Identifiable {
    case alwaysAllow = "always-allow"
    case perShedAllow = "per-shed-allow"
    case timeBasedAllow = "time-based-allow"
    case alwaysAsk = "always-ask"
    case alwaysDeny = "always-deny"

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .alwaysAllow: return "Always Allow"
        case .perShedAllow: return "Per Shed Allow"
        case .timeBasedAllow: return "Time Based Allow"
        case .alwaysAsk: return "Always Ask"
        case .alwaysDeny: return "Always Deny"
        }
    }

    /// Only the time-based policy uses the duration field.
    public var usesDuration: Bool { self == .timeBasedAllow }

    /// The provider-level (namespace) action this policy installs: the two
    /// "Always" options decide outright with no prompt; the rest prompt (and
    /// the chosen scope governs the grant created when the user approves).
    public var namespaceAction: PolicyAction {
        switch self {
        case .alwaysAllow: return .approve
        case .alwaysDeny: return .deny
        case .perShedAllow, .timeBasedAllow, .alwaysAsk: return .prompt
        }
    }

    /// Whether this policy prompts the user — so Method (Touch ID) and the
    /// Duration field are relevant only for these.
    public var prompts: Bool { namespaceAction == .prompt }

    /// The default grant scope this policy applies when the user approves a
    /// prompt (nil for the non-prompting Always rules, which never reach a card).
    public var defaultScope: ApprovalScope? {
        switch self {
        case .perShedAllow: return .perShed
        case .timeBasedAllow: return .perSession
        case .alwaysAsk: return .perRequest
        case .alwaysAllow, .alwaysDeny: return nil
        }
    }
}

public enum PolicyScope: String, Codable, Sendable {
    case `default`, namespace, shed, session
}

/// A single policy rule. The engine resolves the most specific match.
/// `server` scopes a per-shed rule to one shed server ("" = the single/unnamed
/// server); nil means any server (also how a rule predating the multi-server
/// `server` dimension decodes).
public struct PolicyRule: Codable, Sendable, Equatable {
    public var scope: PolicyScope
    public var server: String?
    public var namespace: String?
    public var shed: String?
    public var action: PolicyAction
    public var gate: PolicyGate

    public init(scope: PolicyScope, server: String? = nil, namespace: String? = nil, shed: String? = nil, action: PolicyAction, gate: PolicyGate = .biometricsOrPassword) {
        self.scope = scope
        self.server = server
        self.namespace = namespace
        self.shed = shed
        self.action = action
        self.gate = gate
    }
}

/// A pending approval as published to the UI: the request plus the decided gate
/// (drives the fingerprint icon) and the per-provider scope/TTL defaults the
/// card pre-fills. SSH-only details; AWS/Docker decide via policy without prompting.
public struct PendingApprovalItem: Sendable, Equatable, Identifiable, Encodable {
    public let request: ApprovalRequest
    public let gate: PolicyGate
    public let defaultScope: ApprovalScope
    public let defaultTTL: String
    public var id: String { request.id }

    public init(request: ApprovalRequest, gate: PolicyGate, defaultScope: ApprovalScope = .perSession, defaultTTL: String = defaultApprovalTTL) {
        self.request = request
        self.gate = gate
        self.defaultScope = defaultScope
        self.defaultTTL = defaultTTL
    }

    /// What an Approve tap sends — applies the configured policy's grant
    /// scope/TTL. Shared by the dashboard card and the menu-bar mini-card so the
    /// two surfaces can't drift on what "Approve" means.
    public var approveChoice: ApprovalChoice {
        ApprovalChoice(decision: .approve, scope: defaultScope, ttl: defaultTTL)
    }
    /// What a Deny tap sends — this request only (never persists a rule).
    public var denyChoice: ApprovalChoice { ApprovalChoice(decision: .deny) }

    // Encode the request fields inline (so `approvals.list` keeps id/server/…)
    // plus the decided gate + scope/TTL defaults, for IPC drivability.
    enum CodingKeys: String, CodingKey {
        case id, ts, server, namespace, op, shed, detail
        case expiresAt = "expires_at"
        case gate
        case defaultScope = "default_scope"
        case defaultTTL = "default_ttl"
    }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(request.id, forKey: .id)
        try c.encode(request.ts, forKey: .ts)
        try c.encode(request.server, forKey: .server)
        try c.encode(request.namespace, forKey: .namespace)
        try c.encode(request.op, forKey: .op)
        try c.encode(request.shed, forKey: .shed)
        try c.encode(request.detail, forKey: .detail)
        try c.encode(request.expiresAt, forKey: .expiresAt)
        try c.encode(gate, forKey: .gate)
        try c.encode(defaultScope, forKey: .defaultScope)
        try c.encode(defaultTTL, forKey: .defaultTTL)
    }
}

/// What the user chose on an SSH approval card (or a quick approve/deny). For
/// AWS/Docker the decision comes from policy, not a card.
public struct ApprovalChoice: Sendable, Equatable {
    public var decision: ApprovalDecision
    /// Approve only: per-request (once) vs a timed grant (per-session/per-shed).
    public var scope: ApprovalScope?
    /// TTL shorthand for a timed grant (e.g. "1h").
    public var ttl: String?
    /// Persist a per-shed rule (always-allow when approve, always-deny when deny).
    public var persist: Bool

    public init(decision: ApprovalDecision, scope: ApprovalScope? = nil, ttl: String? = nil, persist: Bool = false) {
        self.decision = decision
        self.scope = scope
        self.ttl = ttl
        self.persist = persist
    }
}

/// The default approval grant duration — used when the duration field is empty
/// or unparseable, and as the pre-fill default.
public let defaultApprovalTTL = "2h"

/// Parse a TTL shorthand like `45s`, `4m`, `3h`, `1d` into seconds. Returns nil
/// for empty/invalid input so the UI can fall back to a default.
public enum TTLShorthand {
    public static func seconds(_ raw: String) -> Int? {
        let s = raw.trimmingCharacters(in: .whitespaces).lowercased()
        guard let last = s.last, let unit = ["s": 1, "m": 60, "h": 3600, "d": 86400][String(last)] else { return nil }
        guard let n = Int(s.dropLast()), n > 0 else { return nil }
        return n * unit
    }
}

/// A session-scoped "approve for this session" grant key (server+namespace+shed).
public struct SessionGrantKey: Hashable, Sendable {
    public let server: String
    public let namespace: String
    public let shed: String
    public init(server: String, namespace: String, shed: String) {
        self.server = server
        self.namespace = namespace
        self.shed = shed
    }
}

public struct PolicyDecision: Sendable, Equatable {
    public let action: PolicyAction
    public let gate: PolicyGate
    /// The scope of the rule that decided this, for the audit trail.
    public let appliedScope: PolicyScope

    public init(action: PolicyAction, gate: PolicyGate, appliedScope: PolicyScope) {
        self.action = action
        self.gate = gate
        self.appliedScope = appliedScope
    }
}
