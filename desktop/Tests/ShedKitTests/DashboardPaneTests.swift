import XCTest

import ShedKit

final class DashboardPaneTests: XCTestCase {
    // Guards the egress pane's discoverability: it must be a navigable pane in
    // DashboardPane.allCases, which the shedctl `ui navigate` error message is
    // derived from — so the advertised pane list can't silently drift.
    func testEgressIsANavigablePane() {
        XCTAssertTrue(DashboardPane.allCases.contains(.egress))
        XCTAssertEqual(DashboardPane(rawValue: "egress"), .egress)
    }

    func testAllCasesMatchExpectedSet() {
        XCTAssertEqual(
            DashboardPane.allCases.map(\.rawValue),
            ["sheds", "approvals", "agents", "activity", "egress", "system"]
        )
    }
}
