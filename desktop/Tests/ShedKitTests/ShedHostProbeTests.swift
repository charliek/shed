import XCTest

@testable import ShedKit

final class ShedHostProbeTests: XCTestCase {
    private func host(lastError: String? = nil) -> ShedHost {
        ShedHost(name: "srv", host: "h", httpPort: 8080, sshPort: 2222, lastError: lastError)
    }

    func testReachableSetsMetadataAndClearsError() {
        let info = ServerInfo(name: "srv", version: "0.7.2", backend: "vz")
        let out = host(lastError: "was unreachable").applyingProbe(info: info, error: nil)
        XCTAssertTrue(out.reachable)
        XCTAssertEqual(out.backend, "vz")
        XCTAssertEqual(out.version, "0.7.2")
        XCTAssertNil(out.lastError, "a reachable host clears its error")
    }

    func testUnreachableCarriesSanitizedError() throws {
        let out = host().applyingProbe(
            info: nil,
            error: "localmac: Authorization Bearer shed_control_secret123 connection refused")
        XCTAssertFalse(out.reachable)
        XCTAssertNil(out.backend)
        let err = try XCTUnwrap(out.lastError)
        XCTAssertTrue(err.contains("connection refused"), err)
        XCTAssertFalse(err.contains("shed_control_secret123"), "tokens must be scrubbed before the UI")
    }
}
