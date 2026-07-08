import CoreServices
import XCTest
@testable import ShedKit

final class LaunchClassifierTests: XCTestCase {
    // A user launch (Finder/Spotlight/`open`) is a kAEOpenApplication event
    // that is NOT flagged as a login-item launch → open the dashboard.
    func testUserLaunchIsUserInitiated() {
        XCTAssertTrue(LaunchClassifier.isUserInitiated(eventID: kAEOpenApplication, loginItemProp: nil))
        // An empty/zero prop descriptor (observed in practice) is still a user launch.
        XCTAssertTrue(LaunchClassifier.isUserInitiated(eventID: kAEOpenApplication, loginItemProp: 0))
    }

    // The path that can't be exercised without a reboot: when macOS flags the
    // launch as a login item, we must stay quiet (menu-bar-only). A service-item
    // launch is likewise treated as background (defensive — we don't register
    // one today).
    func testBackgroundLaunchStaysQuiet() {
        XCTAssertFalse(LaunchClassifier.isUserInitiated(
            eventID: kAEOpenApplication, loginItemProp: keyAELaunchedAsLogInItem))
        XCTAssertFalse(LaunchClassifier.isUserInitiated(
            eventID: kAEOpenApplication, loginItemProp: keyAELaunchedAsServiceItem))
    }

    // No launch event (e.g. a bare launchd exec) → quiet, not user-initiated.
    func testNoLaunchEventStaysQuiet() {
        XCTAssertFalse(LaunchClassifier.isUserInitiated(eventID: nil, loginItemProp: nil))
    }
}
