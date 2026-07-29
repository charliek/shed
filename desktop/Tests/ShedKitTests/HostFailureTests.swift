// HostFailureTests — plan 006 D6 / shed#300: the per-host failure a probe
// produces, and the two places it changes what the user reads (the banner
// summary and the Sheds empty state).
//
// The Swift twin of the Rust `HostFailure::from_error` tests: same kinds, same
// summaries, and the same rule that no generated FFI enum case ever reaches a
// rendered string.

import Foundation
import ShedRustCore
import XCTest

@testable import ShedKit

final class HostFailureTests: XCTestCase {
    private func host(name: String, reachable: Bool = false, failure: HostFailure? = nil) -> ShedHost {
        ShedHost(
            name: name, host: "h", httpPort: 8080, sshPort: 2222, reachable: reachable,
            lastError: failure?.summary, failure: failure)
    }

    private let upgrade = ShedRustCore.ShedError.AgentUpgradeRequired(
        server: "mini2",
        detail: "the connected shed-host-agent does not support `credential.get`, and mini2 "
            + "requires auth.mode: mtls")

    // MARK: - error → failure

    func testTypedUpgradeErrorKeepsItsRemedyAsTheWholeSummary() {
        let f = HostFailure.from(server: "mini2", error: upgrade)
        XCTAssertEqual(f.kind, .agentUpgradeRequired)
        XCTAssertEqual(f.server, "mini2")
        XCTAssertEqual(
            f.summary, "Upgrade shed-host-agent — it can't obtain a certificate for mini2.")
        XCTAssertTrue(f.detail.contains("requires auth.mode: mtls"), f.detail)
    }

    func testEveryOtherErrorLeadsWithTheHostAndCarriesNoEnumWrapper() {
        // The shed#300 leak: `"\(error)"` on the generated enum renders the CASE
        // (`Config(message: "…")`), and uniffi's LocalizedError conformance is
        // `String(reflecting:)`, so `localizedDescription` is no better. Both are
        // banned here for EVERY case, not just the typed one.
        let cases: [(ShedRustCore.ShedError, String)] = [
            (.Config(message: "control token mint failed"), "mini2: control token mint failed"),
            (.Transport(message: "connection refused"), "mini2: transport error: connection refused"),
            (.BadStatus(status: 503), "mini2: shed-server returned HTTP 503"),
            (.Decode(message: "bad json"), "mini2: decode error: bad json"),
            (.Create(message: "no image"), "mini2: create failed: no image"),
        ]
        for (error, want) in cases {
            let f = HostFailure.from(server: "mini2", error: error)
            XCTAssertEqual(f.kind, .other)
            XCTAssertEqual(f.summary, want)
            for wrapper in ["Config(message:", "Transport(message:", "BadStatus(", "ShedError."] {
                XCTAssertFalse(
                    f.summary.contains(wrapper) || f.detail.contains(wrapper),
                    "an enum wrapper leaked into \(f)")
            }
        }
    }

    func testANonCoreErrorStillRendersItsOwnSentence() {
        let f = HostFailure.from(
            server: "mini2", error: ShedClientError.unsupportedAuthMode("mini2 requires mtls"))
        XCTAssertEqual(f.kind, .other)
        XCTAssertEqual(f.summary, "mini2: mini2 requires mtls")
    }

    /// An app error type that carries its sentence in `description` must not be
    /// degraded to `localizedDescription`'s "operation couldn't be completed"
    /// NSError boilerplate — the regression the create-error e2e catches.
    func testADescribableErrorKeepsItsOwnText() {
        struct Carrier: Error, CustomStringConvertible { let description = "create failed: doomed" }
        XCTAssertEqual(
            HostFailure.displayMessage(for: Carrier()), "create failed: doomed")
    }

    // MARK: - probe application

    func testProbeKeepsTheTypedFailureAndSanitizesBothStrings() throws {
        let leaky = HostFailure(
            server: "mini2", kind: .other,
            summary: "mini2: Authorization Bearer shed_control_secret123 refused",
            detail: "Authorization Bearer shed_control_secret123 refused")
        let out = host(name: "mini2").applyingProbe(info: nil, failure: leaky)
        let f = try XCTUnwrap(out.failure)
        XCTAssertFalse(out.reachable)
        XCTAssertEqual(out.lastError, f.summary, "lastError stays the summary, for string consumers")
        XCTAssertFalse(f.summary.contains("shed_control_secret123"), f.summary)
        XCTAssertFalse(f.detail.contains("shed_control_secret123"), f.detail)
    }

    func testAReachableProbeClearsTheFailure() {
        let out = host(name: "mini2", failure: HostFailure.from(server: "mini2", error: upgrade))
            .applyingProbe(info: ServerInfo(name: "mini2", version: "0.8.0", backend: "fc"),
                           failure: nil)
        XCTAssertTrue(out.reachable)
        XCTAssertNil(out.failure)
        XCTAssertNil(out.lastError)
    }

    // MARK: - the empty state

    func testEmptyStateBlamesConfigOnlyWhenNothingIsKnown() {
        XCTAssertEqual(
            HostFailure.shedsEmptyState(hosts: [
                host(name: "a", failure: HostFailure.from(
                    server: "a", error: ShedRustCore.ShedError.Transport(message: "connection refused"))),
            ]),
            "No reachable hosts. Check ~/.shed/config.yaml and that shed-server is running.")
    }

    func testEmptyStateDefersToAKnownHostFailure() {
        let hosts = [
            host(name: "a", failure: HostFailure.from(
                server: "a", error: ShedRustCore.ShedError.Transport(message: "connection refused"))),
            host(name: "mini2", failure: HostFailure.from(server: "mini2", error: upgrade)),
        ]
        let text = HostFailure.shedsEmptyState(hosts: hosts)
        XCTAssertEqual(text, "Upgrade shed-host-agent — it can't obtain a certificate for mini2.")
        XCTAssertFalse(text.contains("config.yaml"), "a known cause must not send the user to config")
    }

    func testAReachableHostOutranksAnyFailure() {
        let hosts = [
            host(name: "ok", reachable: true),
            host(name: "mini2", failure: HostFailure.from(server: "mini2", error: upgrade)),
        ]
        XCTAssertEqual(
            HostFailure.shedsEmptyState(hosts: hosts), "No sheds across the reachable hosts.")
    }

    // MARK: - the IPC wire shape

    func testUIStateCarriesTheEmptyStateAndTheTypedFailure() throws {
        let hosts = [host(name: "mini2", failure: HostFailure.from(server: "mini2", error: upgrade))]
        let data = try JSONEncoder().encode(
            UIState(pane: "sheds", hosts: hosts, sheds: []))
        let json = try XCTUnwrap(
            JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(
            json["sheds_empty_state"] as? String,
            "Upgrade shed-host-agent — it can't obtain a certificate for mini2.")
        let failure = try XCTUnwrap(
            ((json["hosts"] as? [[String: Any]])?.first?["failure"]) as? [String: Any])
        XCTAssertEqual(failure["kind"] as? String, "agent_upgrade_required")
        XCTAssertEqual(failure["server"] as? String, "mini2")
    }
}
