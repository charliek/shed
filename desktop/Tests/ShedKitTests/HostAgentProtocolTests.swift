// HostAgentProtocol token.get / token.response wire tests (Phase 5a).
// The desktop sends `token.get` and correlates the host agent's
// `token.response` by `in_reply_to`. Mirrors the Go side in shed-extensions
// (desktop_protocol.go tokenGetMsg/tokenResponseMsg).

import XCTest
@testable import ShedKit

final class HostAgentProtocolTests: XCTestCase {
    // MARK: - token.response decode

    func testTokenResponseSuccessDecodes() throws {
        let json = """
        {"v":2,"type":"token.response","id":"srv-1","ts":"2026-06-14T00:00:00Z",
         "in_reply_to":"req-42","server":"mini3","token":"shed_control_abc",
         "expires_at":"2026-06-14T01:00:00Z"}
        """
        guard case .tokenResponse(let r) = try HostAgentProtocol.decode(line: Data(json.utf8)) else {
            return XCTFail("expected .tokenResponse")
        }
        XCTAssertEqual(r.inReplyTo, "req-42")
        XCTAssertEqual(r.server, "mini3")
        XCTAssertEqual(r.token, "shed_control_abc")
        XCTAssertEqual(r.expiresAt, "2026-06-14T01:00:00Z")
        XCTAssertNil(r.error)
    }

    func testTokenResponseErrorDecodesFailClosed() throws {
        // On failure the host agent sets `error` and omits token/expires_at.
        let json = """
        {"v":2,"type":"token.response","in_reply_to":"req-7","server":"mini3",
         "error":"host key mismatch"}
        """
        guard case .tokenResponse(let r) = try HostAgentProtocol.decode(line: Data(json.utf8)) else {
            return XCTFail("expected .tokenResponse")
        }
        XCTAssertEqual(r.inReplyTo, "req-7")
        XCTAssertEqual(r.error, "host key mismatch")
        XCTAssertNil(r.token)
        XCTAssertNil(r.expiresAt)
    }

    func testUnknownFrameStillDecodes() throws {
        // Forward-compat: an unrecognized type is surfaced, not thrown.
        let json = #"{"v":2,"type":"some.future.frame","id":"x"}"#
        guard case .unknown(let type) = try HostAgentProtocol.decode(line: Data(json.utf8)) else {
            return XCTFail("expected .unknown")
        }
        XCTAssertEqual(type, "some.future.frame")
    }

    // MARK: - token.get encode

    func testTokenGetEncodesExpectedJSON() throws {
        let data = try HostAgentProtocol.tokenGet(id: "req-42", server: "mini3")
        let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        XCTAssertEqual(obj?["type"] as? String, "token.get")
        XCTAssertEqual(obj?["id"] as? String, "req-42")
        XCTAssertEqual(obj?["server"] as? String, "mini3")
        XCTAssertEqual(obj?["v"] as? Int, hostAgentProtocolVersion)
        // No trailing newline is added by the encoder.
        XCTAssertFalse(String(decoding: data, as: UTF8.self).hasSuffix("\n"))
    }
}
