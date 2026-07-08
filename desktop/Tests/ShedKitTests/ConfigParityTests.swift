import Foundation
import XCTest

@testable import ShedKit

/// Cross-language parity: the Swift `ShedConfig` parser must produce the SAME
/// result from `crates/fixtures/config_sample.yaml` as the Rust `shed_core::config`
/// parser (crates/shed-core/src/config.rs `parity_fixture_matches_expected`). The
/// two parsers coexist until the Swift one is retired (docs/enhancements.md);
/// keep these assertions in lockstep with the Rust test.
final class ConfigParityTests: XCTestCase {
    private func loadSharedFixture() throws -> ShedConfig {
        // Navigate from this test file to the shared fixture at the monorepo
        // root (the same #filePath trick RCTests uses for its golden fixture).
        let root = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // ShedKitTests
            .deletingLastPathComponent()  // Tests
            .deletingLastPathComponent()  // desktop/ (Swift package root)
            .deletingLastPathComponent()  // monorepo root
        let fixture = root.appendingPathComponent("crates/fixtures/config_sample.yaml")
        let text = try String(contentsOf: fixture, encoding: .utf8)
        return ShedConfig.parse(text)
    }

    func testSharedFixtureParsesIdenticallyToRust() throws {
        let config = try loadSharedFixture()
        XCTAssertEqual(config.defaultServer, "mini2")
        // Sorted by name (byte-wise: '2' < 'm', so mini2 < minimal).
        XCTAssertEqual(config.servers.map(\.name), ["mini2", "minimal", "secure"])

        let mini2 = try XCTUnwrap(config.servers.first { $0.name == "mini2" })
        XCTAssertEqual(mini2.host, "mini2")
        XCTAssertEqual(mini2.httpPort, 8080)
        XCTAssertEqual(mini2.sshPort, 2222)
        XCTAssertEqual(mini2.controlToken, "shed_control_abc123")
        XCTAssertEqual(mini2.apiURL, "")
        XCTAssertEqual(mini2.tlsCertFingerprint, "")
        XCTAssertEqual(mini2.resolvedEndpoint().baseURL.absoluteString, "http://mini2:8080")
        XCTAssertEqual(mini2.resolvedEndpoint().pin, "")

        let secure = try XCTUnwrap(config.servers.first { $0.name == "secure" })
        XCTAssertEqual(secure.host, "localhost")
        XCTAssertEqual(secure.controlToken, "")
        XCTAssertEqual(secure.apiURL, "https://localhost:8443")
        // Mixed-case pin lowercased.
        XCTAssertEqual(secure.tlsCertFingerprint, "sha256:aabbcc")
        XCTAssertEqual(secure.resolvedEndpoint().baseURL.absoluteString, "https://localhost:8443")
        XCTAssertEqual(secure.resolvedEndpoint().pin, "sha256:aabbcc")

        // `minimal: {}` → all defaults; host defaults to the server name, ssh_port 22.
        let minimal = try XCTUnwrap(config.servers.first { $0.name == "minimal" })
        XCTAssertEqual(minimal.host, "minimal")
        XCTAssertEqual(minimal.httpPort, 8080)
        XCTAssertEqual(minimal.sshPort, 22)
        XCTAssertEqual(minimal.resolvedEndpoint().baseURL.absoluteString, "http://minimal:8080")
    }
}
