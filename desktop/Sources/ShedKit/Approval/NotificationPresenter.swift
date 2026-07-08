// NotificationPresenter.swift — actionable approval notifications (M5).
//
// A seam so the headline approval gate can reach the user when the dashboard
// and menu are out of sight. The real presenter (in the app target) posts a
// UNUserNotificationCenter banner with Approve/Deny actions; the fake here
// records what was posted and lets the test harness invoke an action, so the
// notification path is driveable over IPC without a real Notification Center.

import Foundation

/// A notification the app asked to be shown — surfaced over IPC
/// (`notifications.list`) so the harness can assert one was posted.
public struct PostedNotification: Codable, Sendable, Equatable, Identifiable {
    public var id: String          // the approval request id
    public var title: String
    public var body: String
    public init(id: String, title: String, body: String) {
        self.id = id
        self.title = title
        self.body = body
    }
}

@MainActor
public protocol NotificationPresenter: AnyObject {
    /// The sink invoked when the user acts on a notification (Approve/Deny).
    var onAction: ((String, ApprovalDecision) -> Void)? { get set }
    /// Invoked when the user taps the notification body (not an action button) —
    /// the app opens the dashboard on the Approvals pane.
    var onOpen: (() -> Void)? { get set }
    /// Ask the OS for permission to post notifications (real impl); no-op fake.
    func requestAuthorization()
    /// Post (or replace) an actionable approval notification.
    func post(_ req: ApprovalRequest)
    /// Withdraw a delivered notification once its request is resolved.
    func withdraw(id: String)
}

/// Standard title/body for an approval, shared by the real + fake presenters
/// so what the harness asserts matches what a user would see.
public enum ApprovalNotificationText {
    public static func title(_ req: ApprovalRequest) -> String { "Approve \(req.namespace)?" }
    public static func body(_ req: ApprovalRequest) -> String { "\(req.op) · \(req.qualifiedShed) · \(req.detail)" }
}

/// Test-mode presenter: records posted notifications and lets the harness
/// drive a Approve/Deny via `notification.invoke`.
@MainActor
public final class FakeNotificationPresenter: NotificationPresenter {
    public var onAction: ((String, ApprovalDecision) -> Void)?
    public var onOpen: (() -> Void)?
    public private(set) var posted: [PostedNotification] = []

    public init() {}
    public func requestAuthorization() {}

    /// Drive a notification-body tap from the harness (the default action).
    public func triggerOpen() { onOpen?() }

    public func post(_ req: ApprovalRequest) {
        posted.removeAll { $0.id == req.id }
        posted.append(PostedNotification(
            id: req.id, title: ApprovalNotificationText.title(req), body: ApprovalNotificationText.body(req)))
    }

    public func withdraw(id: String) { posted.removeAll { $0.id == id } }

    /// Drive a user action from the harness; false if no such notification.
    @discardableResult
    public func invoke(id: String, decision: ApprovalDecision) -> Bool {
        guard posted.contains(where: { $0.id == id }) else { return false }
        onAction?(id, decision)
        return true
    }
}
