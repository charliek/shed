// AppState.swift
//
// The observable view-model the SwiftUI views render. Lives in the UI
// module (ShedKit stays SwiftUI-free); the app target's AppModel owns one
// instance, drives it from the host poller, and exposes a snapshot of it
// over the `ui.state` IPC op. Keeping the rendered state here (not in
// AppModel, which the UI module can't see) is the seam that lets the app
// depend on the UI rather than the reverse.

import Foundation
import ShedKit
import SwiftUI

@MainActor
public final class AppState: ObservableObject {
    @Published public var pane: DashboardPane = .sheds
    @Published public var hosts: [ShedHost] = []
    @Published public var sheds: [Shed] = []
    @Published public var lastError: String?
    /// The most recent in-flight/finished create, for the create sheet.
    @Published public var activeCreate: CreateProgress?
    /// Header indicator; wired to the real host-agent connection in M3.
    @Published public var hostAgentConnected: Bool = false
    /// Whether the create-shed sheet is presented.
    @Published public var showCreateSheet: Bool = false
    /// Whether the launch-agent sheet is presented.
    @Published public var showLaunchSheet: Bool = false
    /// Remote-control sessions across sheds (the Agents pane).
    @Published public var rcSessions: [RcSession] = []
    /// Pending credential-approval requests (the Approvals pane + menu bar),
    /// each with its decided gate + SSH scope/TTL defaults for the card.
    @Published public var approvals: [PendingApprovalItem] = []
    /// Merged activity feed (host-agent audit + lifecycle + RC).
    @Published public var activity: [AuditEntry] = []
    /// Per-host disk usage (the System pane).
    @Published public var systemUsage: [HostDiskUsage] = []
    /// Per-host installed images (the New-Shed picker), keyed by host name.
    @Published public var imagesByHost: [HostImageList] = []
    /// Per-host egress profiles (the read-only Egress → Profiles view).
    @Published public var egressProfiles: [HostEgressProfiles] = []

    // Action seams the app wires up (the UI module can't reach AppModel
    // directly, so it calls these). All run on the main actor. Contract:
    // these are fire-and-forget — results surface back through the
    // @Published state above (sheds/activeCreate refresh; failures land in
    // lastError), not via return values.
    //
    // STOP RULE: this bag is past its practical limit and overdue for
    // promotion to a `weak var actions: AppActions?` delegate protocol (named
    // methods, one wiring point, missing wiring = compile error). That's a
    // cross-cutting cleanup, kept out of feature PRs; do it as its own change
    // before threading further closures through here.
    public var onShedAction: ((ShedAction, Shed) -> Void)?
    public var onOpenTerminal: ((Shed) -> Void)?
    public var onCreate: ((String?, CreateShedRequest) -> Void)?
    /// Launch an RC session. Takes a single `RcLaunchInput` rather than threading
    /// more positional args through this STOP-ruled bag — two adjacent `String?`
    /// (display name + initial prompt) would otherwise be silently transposable.
    public var onRcLaunch: ((RcLaunchInput) -> Void)?
    public var onRcKill: ((RcSession) -> Void)?
    public var onRcRefresh: (() -> Void)?
    /// Open a terminal attached to a session's tmux (`rc-<slug>`). Knowingly
    /// adds to the STOP-ruled bag above: it's one closure for a small feature,
    /// and an AgentRow carries an RcSession (not a Shed), so the Shed-typed
    /// onOpenTerminal seam doesn't fit. The delegate refactor stays separate.
    public var onRcAttach: ((RcSession) -> Void)?
    /// Refresh per-host disk usage (the System pane).
    public var onSystemRefresh: (() -> Void)?
    /// Refresh per-host installed images (the New-Shed picker).
    public var onImagesRefresh: (() -> Void)?
    /// Refresh per-host egress profiles (the Egress → Profiles view).
    public var onEgressRefresh: (() -> Void)?
    public var onOpenURL: ((String) -> Void)?
    /// Decide a pending approval: (request, choice). The choice carries scope/
    /// TTL and whether to persist a per-shed always-allow / always-deny rule.
    public var onApprovalDecide: ((ApprovalRequest, ApprovalChoice) -> Void)?
    /// Reveal the audit log file in Finder (FR-6).
    public var onRevealAuditLog: (() -> Void)?
    /// Reveal the shed-desktop diagnostic log file in Finder.
    public var onRevealDiagnosticLog: (() -> Void)?
    /// Reload ~/.shed/config.yaml and reconnect to all hosts.
    public var onReconnect: (() -> Void)?

    public init() {}

    /// Installed images for a host (the per-host fan-out lives in `imagesByHost`).
    public func images(forHost host: String) -> [ShedImage] {
        imagesByHost.first { $0.host == host }?.images ?? []
    }

    /// Display label for a shed's image — `repo:tag` resolved from the host's
    /// image list (`imagesByHost`), falling back to the short digest.
    public func imageLabel(for shed: Shed) -> String? {
        shed.imageLabel(in: images(forHost: shed.host))
    }

    /// Sheds grouped by host config name, in host order, for the dashboard.
    public func shedsByHost() -> [(host: ShedHost, sheds: [Shed])] {
        hosts.map { host in
            (host, sheds.filter { $0.host == host.name })
        }
    }

    public var runningCount: Int {
        sheds.filter { $0.status == .running }.count
    }

    /// Snapshot for the `ui.state` IPC op.
    public func snapshot() -> UIState {
        UIState(pane: pane.rawValue, hosts: hosts, sheds: sheds, hostAgentConnected: hostAgentConnected, lastError: lastError)
    }
}
