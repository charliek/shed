// HostAgentCredentialFixtureTests — the desktop half of plan 002 §7 P9's
// cross-language wire gate.
//
// The vectors live OUTSIDE this package, in
// `tests/host-agent-diff/fixtures/desktop-credential/`, so the Go agent tests,
// the Rust agent tests and these Swift tests can be pinned to one file per
// message instead of three hand-copied tables that drift apart in three
// languages (Go/Rust consumption is follow-up C3 work). Everything here goes
// through the REAL decoder + the REAL wire→arm mapper — the production path a
// `credential.response` takes on its way to the Rust core.

import Foundation
import XCTest

@testable import ShedKit

final class HostAgentCredentialFixtureTests: XCTestCase {
    // MARK: - fixture plumbing

    private func fixture(_ name: String) throws -> [String: Any] {
        try RepoFixtures.desktopCredential(name)
    }

    private func vectors(_ fixtureName: String) throws -> [[String: Any]] {
        try XCTUnwrap(fixture(fixtureName)["vectors"] as? [[String: Any]])
    }

    private func line(_ frame: [String: Any]) throws -> Data {
        try JSONSerialization.data(withJSONObject: frame)
    }

    /// A vector's frame, with an `oversize_field` expanded to one byte past its
    /// cap. The fixture declares the intent (field + fill char) instead of
    /// carrying a literal 4 KiB string, so the file stays readable and every
    /// language builds the same over-cap value.
    private func frame(of vector: [String: Any], limits: [String: Any]) throws -> [String: Any] {
        var frame = try XCTUnwrap(vector["frame"] as? [String: Any])
        guard let field = vector["oversize_field"] as? String else { return frame }
        let key = field == "client_cert" ? "client_cert_bytes" : "\(field)_bytes"
        let cap = try XCTUnwrap((limits[key] as? NSNumber)?.intValue, "no limit for \(field)")
        let char = (vector["oversize_char"] as? String) ?? "a"
        frame[field] = String(repeating: char, count: cap + 1)
        return frame
    }

    private func limits(_ fixtureName: String) throws -> [String: Any] {
        try XCTUnwrap(fixture(fixtureName)["limits"] as? [String: Any])
    }

    // MARK: - hello_ack → capability (§7 P5)

    func testHelloAckCapabilityVectors() throws {
        for vector in try vectors("hello_ack") {
            let name = vector["name"] as? String ?? "?"
            let frame = try XCTUnwrap(vector["frame"] as? [String: Any], name)
            let want = try XCTUnwrap(vector["expected_capability"] as? String, name)

            guard case .helloAck(let ack) = try HostAgentProtocol.decode(line: try line(frame)) else {
                return XCTFail("\(name): frame did not decode as hello_ack")
            }
            let got = AgentCapabilityState(helloAck: ack)
            switch want {
            case "supported": XCTAssertEqual(got, .supported, name)
            case "unsupported": XCTAssertEqual(got, .unsupported, name)
            default: XCTFail("\(name): unexpected expected_capability \(want)")
            }
        }
    }

    /// The pre-ack state is not derivable from any frame — it is the ABSENCE of
    /// one. Pinned here because collapsing it into `.unsupported` is exactly the
    /// §7 P5 bug (a false "upgrade shed-host-agent" before the agent has spoken).
    func testUnknownIsNotReachableFromAnyAckFixture() throws {
        for vector in try vectors("hello_ack") {
            let frame = try XCTUnwrap(vector["frame"] as? [String: Any])
            guard case .helloAck(let ack) = try HostAgentProtocol.decode(line: try line(frame)) else {
                return XCTFail("frame did not decode as hello_ack")
            }
            XCTAssertNotEqual(AgentCapabilityState(helloAck: ack), .unknown)
        }
    }

    // MARK: - credential.response → arm (§2 C2(ii): fail closed)

    func testCredentialResponseArmVectors() throws {
        let server = try XCTUnwrap(fixture("credential_response")["server"] as? String)
        let limits = try limits("credential_response")
        for vector in try vectors("credential_response") {
            let name = vector["name"] as? String ?? "?"
            let frame = try frame(of: vector, limits: limits)
            let expected = try XCTUnwrap(vector["expected"] as? [String: Any], name)

            guard case .credentialResponse(let resp) = try HostAgentProtocol.decode(line: try line(frame))
            else {
                return XCTFail("\(name): frame did not decode as credential.response")
            }
            let got = resp.validatedCredential(for: server)
            let arm = try XCTUnwrap(expected["arm"] as? String, name)
            switch (arm, got) {
            case ("token", .token(let token, let expiresAt)):
                XCTAssertEqual(token, expected["token"] as? String, name)
                XCTAssertEqual(
                    expiresAt.map { UInt64($0.timeIntervalSince1970) },
                    (expected["expires_at_unix"] as? NSNumber).map { $0.uint64Value }, name)
            case ("certificate", .certificate(let certPEM, let serial, let expiresAt)):
                XCTAssertEqual(certPEM, expected["cert_pem"] as? String, name)
                XCTAssertEqual(serial, expected["serial"] as? String, name)
                XCTAssertEqual(
                    expiresAt.map { UInt64($0.timeIntervalSince1970) },
                    (expected["expires_at_unix"] as? NSNumber).map { $0.uint64Value }, name)
            case ("refused", .refused(let message)):
                let needle = try XCTUnwrap(expected["message_contains"] as? String, name)
                XCTAssertTrue(
                    message.contains(needle),
                    "\(name): refusal \"\(message)\" does not mention \"\(needle)\"")
            default:
                XCTFail("\(name): expected the \(arm) arm, got \(got)")
            }
        }
    }

    /// Every fixture that must NOT yield a bearer token is checked as a set, so
    /// a future mapper change that quietly widens the token arm fails loudly
    /// even if someone edits an individual assertion.
    func testNoRefusedVectorEverProducesACredential() throws {
        let server = try XCTUnwrap(fixture("credential_response")["server"] as? String)
        let limits = try limits("credential_response")
        var refusals = 0
        for vector in try vectors("credential_response") {
            let expected = try XCTUnwrap(vector["expected"] as? [String: Any])
            guard expected["arm"] as? String == "refused" else { continue }
            refusals += 1
            let frame = try frame(of: vector, limits: limits)
            guard case .credentialResponse(let resp) = try HostAgentProtocol.decode(line: try line(frame))
            else {
                return XCTFail("frame did not decode as credential.response")
            }
            guard case .refused = resp.validatedCredential(for: server) else {
                return XCTFail("\(vector["name"] ?? "?"): adopted a credential it must refuse")
            }
        }
        XCTAssertGreaterThanOrEqual(refusals, 15, "the refusal corpus shrank — did a vector get dropped?")
    }

    /// The caps the fixture declares must BE the caps the code enforces —
    /// otherwise the vectors would pass while testing a different contract.
    func testFixtureLimitsMatchTheCode() throws {
        let limits = try limits("credential_response")
        XCTAssertEqual(
            (limits["token_bytes"] as? NSNumber)?.intValue, HostAgentCredentialLimits.maxTokenBytes)
        XCTAssertEqual(
            (limits["client_cert_bytes"] as? NSNumber)?.intValue,
            HostAgentCredentialLimits.maxClientCertBytes)
        XCTAssertEqual(
            (limits["cert_serial_bytes"] as? NSNumber)?.intValue,
            HostAgentCredentialLimits.maxCertSerialBytes)
        XCTAssertEqual(
            (limits["error_bytes"] as? NSNumber)?.intValue, HostAgentCredentialLimits.maxErrorBytes)
        XCTAssertEqual(
            (limits["csr_bytes"] as? NSNumber)?.intValue, HostAgentCredentialLimits.maxCSRBytes)
        XCTAssertEqual(
            (try fixture("credential_get")["max_csr_bytes"] as? NSNumber)?.intValue,
            HostAgentCredentialLimits.maxCSRBytes)
    }

    /// A field exactly AT its cap is accepted — the refusal is for over-cap, not
    /// for "large", so the boundary is pinned in both directions.
    func testAFieldAtItsCapIsAccepted() {
        let atCap = String(repeating: "a", count: HostAgentCredentialLimits.maxTokenBytes)
        let resp = CredentialResponse(
            inReplyTo: "q", server: "prod", authMode: "token", token: atCap)
        guard case .token(let token, _) = resp.validatedCredential(for: "prod") else {
            return XCTFail("a token at exactly the cap must be accepted")
        }
        XCTAssertEqual(token.utf8.count, HostAgentCredentialLimits.maxTokenBytes)
    }

    /// The outbound cap: we refuse to WRITE an oversized CSR rather than hand
    /// the agent something that size.
    func testAnOversizedCsrIsNeverSent() throws {
        let big = String(repeating: "A", count: HostAgentCredentialLimits.maxCSRBytes + 1)
        XCTAssertThrowsError(
            try HostAgentProtocol.credentialGet(id: "q", server: "prod", csrBase64: big)
        ) { error in
            XCTAssertEqual(
                error as? HostAgentProtocolError,
                .oversizedCSR(bytes: HostAgentCredentialLimits.maxCSRBytes + 1))
        }
        // At the cap it still goes out.
        let atCap = String(repeating: "A", count: HostAgentCredentialLimits.maxCSRBytes)
        XCTAssertNoThrow(try HostAgentProtocol.credentialGet(id: "q", server: "prod", csrBase64: atCap))
    }

    // MARK: - credential.get frames (§7 P3 key containment)

    func testCredentialGetFrameVectors() throws {
        for vector in try vectors("credential_get") {
            let name = vector["name"] as? String ?? "?"
            let request = try XCTUnwrap(vector["request"] as? [String: Any], name)
            let expectedFrame = try XCTUnwrap(vector["expected_frame"] as? [String: Any], name)
            let expectedKeys = Set(try XCTUnwrap(vector["expected_keys"] as? [String], name))
            let server = try XCTUnwrap(request["server"] as? String, name)
            // JSON null decodes to NSNull, not nil.
            let csr = request["csr"] as? String

            let data = try HostAgentProtocol.credentialGet(id: "req-id", server: server, csrBase64: csr)
            let obj = try XCTUnwrap(
                JSONSerialization.jsonObject(with: data) as? [String: Any], name)

            XCTAssertEqual(Set(obj.keys), expectedKeys, name)
            XCTAssertEqual(obj["v"] as? Int, expectedFrame["v"] as? Int, name)
            XCTAssertEqual(obj["type"] as? String, expectedFrame["type"] as? String, name)
            XCTAssertEqual(obj["server"] as? String, expectedFrame["server"] as? String, name)
            XCTAssertEqual(obj["csr"] as? String, expectedFrame["csr"] as? String, name)
            // The CSR is relayed VERBATIM — never re-encoded, never regenerated.
            if let csr, !csr.isEmpty { XCTAssertEqual(obj["csr"] as? String, csr, name) }
        }
    }

    /// The shared forbidden-substring list applied to the frames this app
    /// actually emits (§7 P3(a): no private-key material on the UDS).
    func testNoCredentialGetFrameCarriesPrivateKeyMarkers() throws {
        let f = try fixture("credential_get")
        let forbidden = try XCTUnwrap(f["forbidden_substrings"] as? [String])
        let csr = try XCTUnwrap(f["csr_base64"] as? String)
        for csrValue in [csr, nil] {
            let data = try HostAgentProtocol.credentialGet(
                id: UUID().uuidString, server: "prod", csrBase64: csrValue)
            let text = String(decoding: data, as: UTF8.self)
            for marker in forbidden {
                XCTAssertFalse(
                    text.localizedCaseInsensitiveContains(marker),
                    "credential.get frame carried \"\(marker)\": \(text)")
            }
        }
    }

    /// The mutation check for the marker list (trap 4): a private key handed to
    /// the CSR parameter is the realistic accident — it looks like ordinary
    /// base64, so only the DER prefixes catch it. If this stops failing-when-it-
    /// should, the frame scan has become decorative.
    func testAPrivateKeyPassedAsACsrIsCaughtByTheMarkers() throws {
        let forbidden = try XCTUnwrap(fixture("credential_get")["forbidden_substrings"] as? [String])
        // Real openssl output prefixes (bodies truncated — only the header matters).
        let keys = [
            "MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgYqUSleC1eYqlZZZZ",  // PKCS#8 EC P-256
            "MHcCAQEEIIV50QMelyzvZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",  // SEC1 EC
            "MIIEvwIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjZZZZZZZZZZZZZZZZZZZZZZZZ",  // PKCS#8 RSA
        ]
        for key in keys {
            let data = try HostAgentProtocol.credentialGet(id: "q", server: "prod", csrBase64: key)
            let text = String(decoding: data, as: UTF8.self)
            XCTAssertTrue(
                forbidden.contains { text.localizedCaseInsensitiveContains($0) },
                "a private key in the CSR field slipped past every marker: \(key.prefix(24))…")
        }
        // ...and the legitimate CSR does NOT trip them (no false positive).
        let csr = try XCTUnwrap(fixture("credential_get")["csr_base64"] as? String)
        let data = try HostAgentProtocol.credentialGet(id: "q", server: "prod", csrBase64: csr)
        let text = String(decoding: data, as: UTF8.self)
        for marker in forbidden {
            XCTAssertFalse(text.localizedCaseInsensitiveContains(marker), marker)
        }
    }

    // MARK: - config auth_mode (the CACHE rule, deliberately not the wire rule)

    func testConfigAuthModeAbsentAndUnknownDecodeAsToken() {
        XCTAssertEqual(ShedAuthMode(configValue: nil), .token)
        XCTAssertEqual(ShedAuthMode(configValue: ""), .token)
        XCTAssertEqual(ShedAuthMode(configValue: "token"), .token)
        XCTAssertEqual(ShedAuthMode(configValue: "secure"), .token)
        XCTAssertEqual(ShedAuthMode(configValue: "quantum"), .token)
        XCTAssertEqual(ShedAuthMode(configValue: "MTLS"), .token)
        XCTAssertEqual(ShedAuthMode(configValue: "mtls"), .mtls)
        XCTAssertEqual(ShedAuthMode(configValue: " mtls "), .mtls)
    }

}
