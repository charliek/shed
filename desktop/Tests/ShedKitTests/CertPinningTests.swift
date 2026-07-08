// CertPinningTests.swift
//
// Phase 5e: TLS cert pinning for the desktop client.

import Foundation
import XCTest

@testable import ShedKit

final class CertPinningTests: XCTestCase {
    // The "sha256:"+lowerhex format must be byte-for-byte what the server and Go
    // clients produce, so a pin captured by `shed server add` verifies here.
    func testCertFingerprintMatchesGoFormat() {
        // sha256("hello") == 2cf24dba...9824 (verified out of band).
        XCTAssertEqual(
            certFingerprint(Data("hello".utf8)),
            "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
    }

    func testPinMatches() {
        let der = Data("hello".utf8)
        let pin = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
        XCTAssertTrue(pinMatches(leafDER: der, fingerprint: pin))
        XCTAssertFalse(pinMatches(leafDER: der, fingerprint: "sha256:" + String(repeating: "00", count: 32)))
        XCTAssertFalse(pinMatches(leafDER: Data("other".utf8), fingerprint: pin))
    }

    func testShedConfigParsesTLSFields() {
        let yaml = """
            servers:
              plain:
                host: plainhost
                http_port: 8080
              tls:
                host: tlshost
                http_port: 8080
                api_url: https://tlshost:8443
                tls_cert_fingerprint: SHA256:ABC123
                control_token: shed_control_xyz
            """
        let config = ShedConfig.parse(yaml)
        let byName = Dictionary(uniqueKeysWithValues: config.servers.map { ($0.name, $0) })

        let plain = try? XCTUnwrap(byName["plain"])
        XCTAssertEqual(plain?.apiURL, "")
        XCTAssertEqual(plain?.tlsCertFingerprint, "")

        let tls = try? XCTUnwrap(byName["tls"])
        XCTAssertEqual(tls?.apiURL, "https://tlshost:8443")
        // Canonicalized to lowercase so it matches the server-emitted pin.
        XCTAssertEqual(tls?.tlsCertFingerprint, "sha256:abc123")
        XCTAssertEqual(tls?.controlToken, "shed_control_xyz")
    }

    // A pin with a non-https URL would silently send plaintext; the client must
    // fail closed (mirrors the Go/sdk contract).
    func testClientFailsClosedOnPinWithoutHTTPS() async {
        let client = ShedServerClient(
            baseURL: URL(string: "http://localhost:8080")!,
            serverName: "t",
            tlsCertFingerprint: "sha256:" + String(repeating: "ab", count: 32))
        do {
            _ = try await client.info()
            XCTFail("a TLS pin on a non-https URL must fail closed, not send plaintext")
        } catch {
            // Expected: the configured pin/URL mismatch throws before any request.
        }
    }
}
