// PreferencesStore.swift — persisted user settings (M4).
//
// UserDefaults-backed, honoring SHED_DESKTOP_DEFAULTS_SUITE so the test
// harness writes to a throwaway suite instead of the dev's real defaults.

import Foundation
import ShedKit

struct PreferencesStore {
    private let defaults: UserDefaults

    init() {
        if let suite = ProcessInfo.processInfo.environment["SHED_DESKTOP_DEFAULTS_SUITE"],
           let scoped = UserDefaults(suiteName: suite) {
            defaults = scoped
        } else {
            defaults = .standard
        }
    }

    /// Custom terminal command template with `{cmd}`/`{shed}` placeholders;
    /// used only when `terminalPreset == .custom`.
    var terminalTemplate: String {
        get { defaults.string(forKey: "terminalTemplate") ?? "" }
        nonmutating set { defaults.set(newValue, forKey: "terminalTemplate") }
    }

    /// The selected terminal preset. Absent (fresh install) → derive once from
    /// the legacy template (non-empty ⇒ `.custom`, else `.terminalApp`).
    var terminalPreset: TerminalPreset {
        get {
            TerminalPreset.derive(
                legacyTemplate: defaults.string(forKey: "terminalTemplate") ?? "",
                storedRaw: defaults.string(forKey: "terminalPreset"))
        }
        nonmutating set { defaults.set(newValue.rawValue, forKey: "terminalPreset") }
    }

    // Per-provider approval config (issue: per-provider approval). SSH gets a
    // policy + method + default TTL; AWS/Docker get a live Allow/Deny mode.
    var sshMethod: ApprovalMethod {
        get { ApprovalMethod(rawValue: defaults.string(forKey: "sshMethod") ?? "") ?? .biometricsOrPassword }
        nonmutating set { defaults.set(newValue.rawValue, forKey: "sshMethod") }
    }
    /// The SSH approval policy (5 options). `alwaysAllow`/`alwaysDeny` decide
    /// every sign outright; the rest prompt and grant per their scope. Default
    /// is Time Based Allow (prompt once, grant for the duration).
    var sshPolicy: SSHApprovalPolicy {
        get { SSHApprovalPolicy(rawValue: defaults.string(forKey: "sshPolicy") ?? "") ?? .timeBasedAllow }
        nonmutating set { defaults.set(newValue.rawValue, forKey: "sshPolicy") }
    }
    var sshTTL: String {
        get { defaults.string(forKey: "sshTTL") ?? defaultApprovalTTL }
        nonmutating set { defaults.set(newValue, forKey: "sshTTL") }
    }

    /// Live Allow/Deny mode for a credential namespace (aws/docker). Defaults to
    /// deny (safe) until the user opts in.
    func providerMode(_ ns: String) -> ApprovalDecision {
        ApprovalDecision(rawValue: defaults.string(forKey: "mode.\(ns)") ?? "") ?? .deny
    }
    func setProviderMode(_ ns: String, _ mode: ApprovalDecision) {
        defaults.set(mode.rawValue, forKey: "mode.\(ns)")
    }

    /// Per-shed "always allow / always deny" rules. JSON-encoded in the defaults
    /// suite so they survive relaunch. (Per-provider defaults live in the keys
    /// above; the namespace rules are derived from those at policy-build time.)
    var policyRules: [PolicyRule] {
        get {
            guard let data = defaults.data(forKey: "policyRules"),
                  let rules = try? JSONDecoder().decode([PolicyRule].self, from: data) else { return [] }
            return rules
        }
        nonmutating set {
            // Only write on a successful encode — never set(nil), which would
            // silently wipe every per-namespace/per-shed rule.
            guard let data = try? JSONEncoder().encode(newValue) else { return }
            defaults.set(data, forKey: "policyRules")
        }
    }
}
