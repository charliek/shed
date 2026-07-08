// Preferences.swift — the observable backing the preferences window.
// The app wires the on* closures to persist + apply each change. The approval
// model is per-provider: shown only for the credential namespaces the host
// agent delegates to shed-desktop (from hello_ack.gate_namespaces).

import ShedKit
import SwiftUI

/// One per-shed override row shown (and removable) in preferences.
public struct ShedRuleRow: Identifiable, Equatable, Sendable {
    public let server: String
    public let shed: String
    public let action: ApprovalDecision   // approve = always-allow, deny = always-deny
    public var id: String { "\(server)/\(shed)" }
    public init(server: String, shed: String, action: ApprovalDecision) {
        self.server = server
        self.shed = shed
        self.action = action
    }
}

@MainActor public final class Preferences: ObservableObject {
    @Published public var launchAtLogin: Bool
    @Published public var terminalTemplate: String

    /// The selected terminal preset, and the presets actually offered (the
    /// app computes the latter from which terminals are installed).
    @Published public var terminalPreset: TerminalPreset = .terminalApp
    @Published public var availableTerminalPresets: [TerminalPreset] = [.terminalApp, .custom]

    /// Credential namespaces the host agent delegates to shed-desktop
    /// (`hello_ack.gate_namespaces`) — drives which approval sections show.
    @Published public var gatedNamespaces: [String] = []

    // SSH (shown when "ssh-agent" is gated): full interactive approval.
    @Published public var sshMethod: ApprovalMethod = .biometricsOrPassword
    @Published public var sshPolicy: SSHApprovalPolicy = .timeBasedAllow
    @Published public var sshTTL: String = defaultApprovalTTL

    // AWS / Docker (shown when gated): a live Allow/Deny toggle.
    @Published public var awsMode: ApprovalDecision = .deny
    @Published public var dockerMode: ApprovalDecision = .deny

    /// Per-shed "always allow / always deny" overrides, for display + removal.
    @Published public var shedRules: [ShedRuleRow] = []

    public var onLaunchAtLogin: ((Bool) -> Void)?
    public var onTerminalTemplate: ((String) -> Void)?
    public var onTerminalPreset: ((TerminalPreset) -> Void)?
    public var onSSHMethod: ((ApprovalMethod) -> Void)?
    public var onSSHPolicy: ((SSHApprovalPolicy) -> Void)?
    public var onSSHTTL: ((String) -> Void)?
    /// Set the live Allow/Deny mode for a credential namespace (aws/docker).
    public var onProviderMode: ((String, ApprovalDecision) -> Void)?
    public var onRemoveShedRule: ((String, String) -> Void)?

    public init(launchAtLogin: Bool = false, terminalTemplate: String = "") {
        self.launchAtLogin = launchAtLogin
        self.terminalTemplate = terminalTemplate
    }
}
