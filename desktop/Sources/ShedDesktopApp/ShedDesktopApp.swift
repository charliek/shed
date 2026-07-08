// ShedDesktopApp.swift — entry point.
//
// A manual AppKit bootstrap (rather than the SwiftUI App lifecycle) so the
// app fully owns its windows: the dashboard and the menu popover are
// AppKit NSWindows hosting SwiftUI views, which gives the IPC screenshot
// op a stable window handle and deterministic show/hide. The app launches as
// an accessory (menu-bar item, no Dock icon — also `LSUIElement` in the
// bundle's Info.plist); `AppModel` raises it to a regular app (Dock icon +
// ⌘-Tab + the app menu) while a window is open and reverts on close.

import AppKit
import ShedKit

@main
@MainActor
enum ShedDesktopMain {
    // NSApplication.delegate is weak; keep a strong ref so the delegate
    // (and the AppModel it owns) lives for the whole process.
    static var delegate: AppDelegate?

    static func main() {
        let app = NSApplication.shared
        let d = AppDelegate()
        delegate = d
        app.delegate = d
        app.setActivationPolicy(.accessory)
        app.run()
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let appModel = AppModel()

    func applicationDidFinishLaunching(_ notification: Notification) {
        appModel.start(profile: .mac())

        let event = NSAppleEventManager.shared().currentAppleEvent
        let loginProp = event?.paramDescriptor(forKeyword: keyAEPropData)?.enumCodeValue
        let userLaunch = LaunchClassifier.isUserInitiated(eventID: event?.eventID, loginItemProp: loginProp)
        // Logged so a real login launch (only verifiable on a reboot) can be
        // confirmed in Console: a quiet login launch should read userInitiated=0.
        NSLog("shed-desktop launch: userInitiated=\(userLaunch ? 1 : 0) eventID=\(Self.fourCC(event?.eventID)) loginFlag=\(Self.fourCC(loginProp))")

        // An active user launch (Finder double-click, Launchpad, Spotlight,
        // `open`) opens the dashboard; a quiet login-item launch stays
        // menu-bar-only. Gated off under the harness so the hermetic suite
        // keeps its hidden-start / accessory policy.
        if userLaunch && !ShedBackend.shared.testMode {
            appModel.showWindow()
        }
    }

    // Re-opening an already-running instance (double-click / ⌘-Space) reaches
    // the dashboard. This is also the escape hatch when the status-item icon
    // is hidden under the notch and unclickable (issue #4).
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        appModel.showWindow()
        return true
    }

    /// Render a FourCharCode for the launch log (e.g. `oapp`), `nil` when absent.
    private static func fourCC(_ code: OSType?) -> String {
        guard let code, code != 0 else { return "nil" }
        let bytes = [UInt8((code >> 24) & 0xff), UInt8((code >> 16) & 0xff),
                     UInt8((code >> 8) & 0xff), UInt8(code & 0xff)]
        return String(bytes: bytes, encoding: .ascii) ?? "\(code)"
    }
}
