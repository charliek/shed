// Per-provider approval model: method→gate, decision→action, TTL shorthand.

import XCTest
@testable import ShedKit

final class ApprovalModelTests: XCTestCase {
    func testMethodMapsToGate() {
        XCTAssertEqual(ApprovalMethod.biometricsOrPassword.gate, .biometricsOrPassword)
        XCTAssertEqual(ApprovalMethod.biometrics.gate, .biometrics)
        XCTAssertEqual(ApprovalMethod.prompt.gate, .none)
        // Only the biometric methods show a prompt / fingerprint icon.
        XCTAssertTrue(ApprovalMethod.biometricsOrPassword.gate.isBiometric)
        XCTAssertTrue(ApprovalMethod.biometrics.gate.isBiometric)
        XCTAssertFalse(ApprovalMethod.prompt.gate.isBiometric)
    }

    func testDecisionMapsToPolicyAction() {
        XCTAssertEqual(ApprovalDecision.approve.policyAction, .approve)
        XCTAssertEqual(ApprovalDecision.deny.policyAction, .deny)
    }

    func testTTLShorthand() {
        XCTAssertEqual(TTLShorthand.seconds("45s"), 45)
        XCTAssertEqual(TTLShorthand.seconds("4m"), 240)
        XCTAssertEqual(TTLShorthand.seconds("3h"), 3 * 3600)
        XCTAssertEqual(TTLShorthand.seconds("1d"), 86400)
        XCTAssertEqual(TTLShorthand.seconds(" 2H "), 2 * 3600) // trimmed + case-insensitive
        XCTAssertNil(TTLShorthand.seconds(""))
        XCTAssertNil(TTLShorthand.seconds("h"))
        XCTAssertNil(TTLShorthand.seconds("0h"))
        XCTAssertNil(TTLShorthand.seconds("3w"))
        XCTAssertNil(TTLShorthand.seconds("abc"))
    }

    func testSSHApprovalPolicyOrderingAndFlags() {
        // allCases is the picker order: most → least permissive.
        XCTAssertEqual(SSHApprovalPolicy.allCases,
                       [.alwaysAllow, .perShedAllow, .timeBasedAllow, .alwaysAsk, .alwaysDeny])
        XCTAssertTrue(SSHApprovalPolicy.allCases.filter(\.usesDuration) == [.timeBasedAllow])
    }

    func testSSHApprovalPolicyNamespaceActionAndPrompts() {
        // The two "Always" options decide outright (no prompt); the rest prompt
        // and grant per their scope. Drives the SSH namespace rule + which
        // Preferences controls (Method/Duration) are shown.
        XCTAssertEqual(SSHApprovalPolicy.alwaysAllow.namespaceAction, .approve)
        XCTAssertEqual(SSHApprovalPolicy.alwaysDeny.namespaceAction, .deny)
        XCTAssertFalse(SSHApprovalPolicy.alwaysAllow.prompts)
        XCTAssertFalse(SSHApprovalPolicy.alwaysDeny.prompts)
        for d in [SSHApprovalPolicy.perShedAllow, .timeBasedAllow, .alwaysAsk] {
            XCTAssertEqual(d.namespaceAction, .prompt)
            XCTAssertTrue(d.prompts)
        }
    }

    func testSSHApprovalPolicyDefaultScope() {
        // The prompting policies expose their grant scope; the Always rules
        // never reach a card, so they have none.
        XCTAssertEqual(SSHApprovalPolicy.perShedAllow.defaultScope, .perShed)
        XCTAssertEqual(SSHApprovalPolicy.timeBasedAllow.defaultScope, .perSession)
        XCTAssertEqual(SSHApprovalPolicy.alwaysAsk.defaultScope, .perRequest)
        XCTAssertNil(SSHApprovalPolicy.alwaysAllow.defaultScope)
        XCTAssertNil(SSHApprovalPolicy.alwaysDeny.defaultScope)
    }

    // The gate enum's wire values pin the protocol contract.
    func testGateWireValues() {
        XCTAssertEqual(PolicyGate.biometricsOrPassword.rawValue, "biometrics-or-password")
        XCTAssertEqual(PolicyGate.biometrics.rawValue, "biometrics")
        XCTAssertEqual(PolicyGate.none.rawValue, "none")
    }
}
