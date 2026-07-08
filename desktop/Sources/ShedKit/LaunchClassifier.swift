// LaunchClassifier.swift
//
// Decides whether a launch was user-initiated (open the dashboard) or a quiet
// login-item launch (stay menu-bar-only) — issue #4. A user launch (Finder
// double-click, Launchpad, Spotlight, `open`) carries a `kAEOpenApplication`
// Apple Event; a login-item launch is flagged with `keyAELaunchedAsLogInItem`.
// This is activation-independent, which matters because an `LSUIElement`
// (accessory) app is not brought to the foreground on launch either way, so
// activation state can't distinguish the two.
//
// The classification is a pure function of the launch event's codes so it can
// be unit-tested without a live Apple Event (the caller in ShedDesktopApp pulls
// the codes off NSAppleEventManager.currentAppleEvent).

import CoreServices

public enum LaunchClassifier {
    /// True when the app was launched by the user, false for a quiet login-item
    /// launch or when there is no launch event at all.
    ///
    /// - eventID: the launch Apple Event's id (`currentAppleEvent?.eventID`).
    /// - loginItemProp: the `keyAEPropData` enum code of that event, if any.
    public static func isUserInitiated(eventID: AEEventID?, loginItemProp: OSType?) -> Bool {
        guard eventID == kAEOpenApplication else { return false }
        // Any system/background launch flag means "not user-initiated"; only a
        // plain open event opens the dashboard. We don't register as a service
        // item today, but excluding it too keeps the default safely closed.
        return loginItemProp != keyAELaunchedAsLogInItem
            && loginItemProp != keyAELaunchedAsServiceItem
    }
}
