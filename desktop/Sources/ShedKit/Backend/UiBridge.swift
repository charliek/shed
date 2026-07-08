// UiBridge.swift
//
// The one seam the IPC handler uses to reach the running SwiftUI app.
// The handler never touches AppKit or NSApp directly — every main-thread
// op goes through this protocol, whose sole conformer is the app's
// AppModel. Mirrors roost's UiBridge.

import AppKit
import Foundation

/// A request to launch a remote-control session, passed from the UI to the app
/// as one value so the launch fields can't be transposed positionally (the
/// sheet builds it; `AppState.onRcLaunch` carries it). `workdir` is resolved by
/// the binary, so the sheet doesn't supply one.
public struct RcLaunchInput: Sendable {
    public let host: String?
    public let shed: String
    public let kind: RcKind
    public let displayName: String?
    public let initialPrompt: String?
    public init(host: String?, shed: String, kind: RcKind, displayName: String?, initialPrompt: String?) {
        self.host = host
        self.shed = shed
        self.kind = kind
        self.displayName = displayName
        self.initialPrompt = initialPrompt
    }
}

@MainActor
public protocol UiBridge: AnyObject {
    /// The NSWindow backing a capturable `surface`, for `app.screenshot`.
    /// Returns nil when that surface isn't currently available (e.g. the
    /// menu popover is closed), which the handler maps to an error.
    func window(for surface: ScreenshotSurface) -> NSWindow?

    /// Order the dashboard window front (the accessory app may launch with
    /// it closed). Used by `ui.showWindow` before a window screenshot.
    func showWindow()

    /// Close the dashboard window (reverting to a menu-bar-only accessory via
    /// `windowWillClose`). The inverse of `showWindow`; lets a driver model a
    /// user closing the window so the reopen escape hatch can be exercised.
    func hideWindow()

    /// Open (and front) the preferences window.
    func openPreferences()

    /// Force the menu-bar popover open or closed (`ui.openMenu`).
    func setMenuOpen(_ open: Bool)

    /// Switch the dashboard's selected sidebar pane (`ui.navigate`).
    /// Returns false for an unknown pane name.
    func navigate(toPane pane: String) -> Bool

    /// Apply SSH approval preferences (any subset) and reset live SSH grants so
    /// the change takes effect on the next request (`ui.set_ssh_approval`).
    func setSshApproval(method: ApprovalMethod?, policy: SSHApprovalPolicy?, ttl: String?)

    /// Snapshot of the view-model for `ui.state`.
    func uiState() -> UIState

    /// Logical window measurements for `app.window_metrics`.
    func windowMetrics() -> WindowMetrics

    /// Dashboard visibility + activation policy for `ui.window_state` — the
    /// observable surface of the launch/reopen behavior (issue #4).
    func windowState() -> WindowState

    /// Force an immediate poll of all hosts (`sheds.refresh`); returns once
    /// the refresh has completed so tests can assert without waiting for the
    /// poll interval.
    func refreshSheds() async

    /// Fan out `GET /api/system/df` to every host and publish + return the
    /// per-host disk usage (`system.df`, M7).
    func refreshSystemUsage() async -> [HostDiskUsage]

    /// Fan out `GET /api/images` to every host and publish + return the
    /// per-host image lists (`images.list`), feeding the New-Shed picker.
    func refreshImages() async -> [HostImageList]

    /// Open the dashboard and present the New-Shed sheet (`ui.show_create`),
    /// so the harness can screenshot the image picker.
    func showCreateSheet()

    /// Open the dashboard and present the Launch-agent sheet (`ui.show_launch`).
    func showLaunchSheet()

    // MARK: - M1: lifecycle, create, terminal

    /// Run a lifecycle mutation on a shed, then refresh so the result is
    /// reflected by the time this returns.
    func shedAction(_ action: ShedAction, host: String?, name: String) async throws

    /// Kick off a create; returns a create id whose progress is polled via
    /// `createStatus`.
    func startCreate(host: String?, request: CreateShedRequest) throws -> String
    func createStatus(id: String) -> CreateProgress?
    /// Cancel an in-flight create: stop the driving task (propagating the cancel
    /// through the backend) and drop the store entry, so a later `createStatus`
    /// no longer finds it. Idempotent — a no-op for an unknown/completed id.
    func cancelCreate(id: String)

    /// Build the ssh command to reach a shed (pure; spawns nothing).
    func terminalCommand(shed: String, host: String?, session: String?) throws -> TerminalCommand
    /// Resolve the launch (active preset + the exact invocation) without
    /// spawning — backs `terminal.preview` so an agent can observe what would run.
    func terminalLaunchPreview(shed: String, host: String?, session: String?) throws
        -> (command: TerminalCommand, preset: TerminalPreset, invocation: LaunchInvocation)
    /// Build AND launch the terminal (side-effecting; gated to test mode off).
    func openTerminal(shed: String, host: String?, session: String?) throws -> TerminalCommand

    // MARK: - M2: remote-control sessions

    func rcList(host: String?, shed: String?) async throws -> [RcSession]
    func rcLaunch(host: String?, shed: String, kind: RcKind, displayName: String?, workdir: String?, initialPrompt: String?) async throws -> RcSession
    func rcKill(host: String?, shed: String, slug: String) async throws
    /// Inject a session into the table directly — test-only (e.g. a legacy row
    /// for an e2e screenshot). Throws outside the harness.
    func rcInjectTest(_ session: RcSession) throws

    // MARK: - M3: credential approvals + activity

    func approvalsList() -> [PendingApprovalItem]
    /// Resolve a pending approval. `choice` carries the SSH scope/TTL and whether
    /// to persist a per-shed rule (always-allow / always-deny).
    func decideApproval(id: String, choice: ApprovalChoice) async throws
    func activityList(limit: Int) -> [AuditEntry]
    /// Filesystem path of the append-only audit log (FR-6 export).
    func auditLogPath() -> String
    /// Replace the policy rules (test-mode only, to exercise the matrix E2E).
    func setPolicyRules(_ rules: [PolicyRule])
    /// The current effective rule set (default + per-namespace + per-shed).
    func policyRules() -> [PolicyRule]

    // MARK: - M5: notifications (driveable over IPC)

    /// Notifications the app has posted (only the test presenter records these).
    func postedNotifications() -> [PostedNotification]
    /// Drive a posted notification's Approve/Deny action (test presenter only).
    func invokeNotification(id: String, decision: ApprovalDecision) throws
    /// Drive a notification-body tap → open the Approvals pane (test presenter only).
    func invokeNotificationOpen() throws
}
