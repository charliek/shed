// SystemNotificationPresenter.swift — the real (M5) notification presenter.
//
// Posts an actionable Approve/Deny banner via UNUserNotificationCenter and
// routes the chosen action back through `onAction` → AppModel.decideApproval.
// Only instantiated in non-test mode; the harness uses FakeNotificationPresenter
// so CI never touches the real Notification Center (no authorization prompt,
// no bundle/TCC requirements).

import Foundation
import ShedKit
import UserNotifications

@MainActor
final class SystemNotificationPresenter: NSObject, NotificationPresenter, UNUserNotificationCenterDelegate {
    var onAction: ((String, ApprovalDecision) -> Void)?
    var onOpen: (() -> Void)?

    private static let categoryID = "approval"
    private static let approveAction = "approve"
    private static let denyAction = "deny"

    private let center = UNUserNotificationCenter.current()

    override init() {
        super.init()
        center.delegate = self
        // No `.authenticationRequired` on Approve: the app applies its own
        // Touch ID gate in decideApproval, so requiring it here too would
        // double-prompt.
        let approve = UNNotificationAction(identifier: Self.approveAction, title: "Approve", options: [])
        let deny = UNNotificationAction(identifier: Self.denyAction, title: "Deny", options: [.destructive])
        let category = UNNotificationCategory(
            identifier: Self.categoryID, actions: [approve, deny], intentIdentifiers: [], options: [])
        center.setNotificationCategories([category])
    }

    func requestAuthorization() {
        // The completion-handler overload runs this @MainActor type's closure on
        // UserNotifications' background XPC queue → a hard SIGTRAP on first
        // launch on macOS 26 (issue #2). Use the async overload from a DETACHED
        // (nonisolated) task: the grant result is discarded, so nothing needs
        // the main actor. Detaching also means we never pass the
        // @MainActor-isolated `self.center` into the nonisolated async API,
        // which Swift 6.0/6.1 reject as "sending risks data races" (6.2's
        // region-based isolation allows it, which masked this locally).
        Task.detached {
            _ = try? await UNUserNotificationCenter.current()
                .requestAuthorization(options: [.alert, .sound])
        }
    }

    func post(_ req: ApprovalRequest) {
        let content = UNMutableNotificationContent()
        content.title = ApprovalNotificationText.title(req)
        content.body = ApprovalNotificationText.body(req)
        content.categoryIdentifier = Self.categoryID
        content.sound = .default
        // Use the request id as the notification id so withdraw() can target it.
        center.add(UNNotificationRequest(identifier: req.id, content: content, trigger: nil))
    }

    func withdraw(id: String) {
        center.removeDeliveredNotifications(withIdentifiers: [id])
        center.removePendingNotificationRequests(withIdentifiers: [id])
    }

    // Show the banner even when shed-desktop is frontmost.
    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .sound])
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        let id = response.notification.request.identifier
        switch response.actionIdentifier {
        case Self.approveAction: Task { @MainActor in self.onAction?(id, .approve) }
        case Self.denyAction: Task { @MainActor in self.onAction?(id, .deny) }
        case UNNotificationDefaultActionIdentifier:
            // Tapping the banner body leaves the request pending and opens the
            // dashboard on the Approvals pane.
            Task { @MainActor in self.onOpen?() }
        default:
            break   // dismiss / unknown → leave it pending, no UI
        }
        completionHandler()
    }
}
