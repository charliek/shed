// CredentialModeObserverTests — plan 002 §7 P1 on the desktop: the learned auth
// mode is IN-MEMORY state and nothing more. The observer records what the core
// adopted, reports transitions to its sink, and deliberately drops the token the
// event may carry (this app's storage is not the token's home — `config.yaml` is
// CLI-owned and this package's parser is read-only).

import Foundation
import ShedRustCore
import XCTest

@testable import ShedKit

final class CredentialModeObserverTests: XCTestCase {
    func testStartsWithNothingLearned() {
        XCTAssertNil(CredentialModeObserver().learnedMode)
    }

    func testAdoptionRecordsTheModeWithoutASink() {
        let o = CredentialModeObserver()
        o.credentialAdopted(
            event: ShedRustCore.CredentialAdopted(
                server: "prod", mode: .token, expiresAtUnix: 4_071_049_445, token: "shed_control_x"))
        XCTAssertEqual(o.learnedMode, .token)

        o.credentialAdopted(
            event: ShedRustCore.CredentialAdopted(
                server: "prod", mode: .mtls, expiresAtUnix: nil, token: nil))
        XCTAssertEqual(o.learnedMode, .mtls)
    }

    func testModeChangedNotifiesTheSinkInBothDirections() {
        let box = ModeBox()
        let o = CredentialModeObserver(sink: { server, mode in box.record(server, mode) })

        o.modeChanged(server: "prod", mode: .mtls)
        XCTAssertEqual(o.learnedMode, .mtls)
        o.modeChanged(server: "prod", mode: .token)
        XCTAssertEqual(o.learnedMode, .token)

        XCTAssertEqual(box.entries.map(\.1), [.mtls, .token])
        XCTAssertEqual(Set(box.entries.map(\.0)), ["prod"])
    }

    /// The event may carry a bearer token (for clients whose store IS its home).
    /// The desktop must not retain it anywhere — the observer's whole public
    /// surface is the mode.
    func testTheAdoptedTokenIsNotRetained() {
        let o = CredentialModeObserver()
        o.credentialAdopted(
            event: ShedRustCore.CredentialAdopted(
                server: "prod", mode: .token, expiresAtUnix: nil, token: "shed_control_secret"))
        XCTAssertEqual(o.learnedMode, .token)
        // Mirror-based surface check: no stored property anywhere in the object
        // graph holds the token value.
        XCTAssertFalse(String(describing: Mirror(reflecting: o).children.map { $0.value })
            .contains("shed_control_secret"))
    }
}

/// Thread-safe recorder — the sink is documented to arrive on the core's
/// dispatcher thread.
private final class ModeBox: @unchecked Sendable {
    private let lock = NSLock()
    private var stored: [(String, ShedAuthMode)] = []

    func record(_ server: String, _ mode: ShedAuthMode) {
        lock.lock()
        stored.append((server, mode))
        lock.unlock()
    }

    var entries: [(String, ShedAuthMode)] {
        lock.lock()
        defer { lock.unlock() }
        return stored
    }
}
