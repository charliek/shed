// ShedServerClient.egressProfiles() decode tests — the GET /api/egress/profiles
// wire shape (array of {name, source, profile:{mode,allow,deny,rule}}).

import Foundation
import XCTest

@testable import ShedKit

final class ShedServerClientEgressTests: XCTestCase {
    private func client() -> ShedServerClient {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        return ShedServerClient(
            baseURL: URL(string: "http://stub.local")!, serverName: "t",
            session: URLSession(configuration: config))
    }

    func testEgressProfilesDecode() async throws {
        let body = """
            [
              {"name":"audit","source":"config","profile":{"mode":"audit"}},
              {"name":"my-stack","source":"user","profile":{"allow":["example.net","*.example.com"],"deny":["tracker.io"],"rule":"port == 443"}}
            ]
            """
        StubURLProtocol.reset([.init(status: 200, body: Data(body.utf8))])

        let profiles = try await client().egressProfiles()
        XCTAssertEqual(profiles.count, 2)
        XCTAssertEqual(profiles[0].name, "audit")
        XCTAssertEqual(profiles[0].source, "config")
        XCTAssertEqual(profiles[0].profile.mode, "audit")
        XCTAssertNil(profiles[0].profile.allow)
        XCTAssertEqual(profiles[1].source, "user")
        XCTAssertEqual(profiles[1].profile.allow, ["example.net", "*.example.com"])
        XCTAssertEqual(profiles[1].profile.deny, ["tracker.io"])
        XCTAssertEqual(profiles[1].profile.rule, "port == 443")
    }

    func testEgressProfilesEmptyArray() async throws {
        StubURLProtocol.reset([.init(status: 200, body: Data("[]".utf8))])
        let profiles = try await client().egressProfiles()
        XCTAssertTrue(profiles.isEmpty)
    }

    // Optional fields may be explicitly null, omitted entirely, or partially
    // present — the decoder must tolerate all three (the server uses omitempty).
    func testEgressProfilesOptionalFields() async throws {
        let body = """
            [
              {"name":"a","source":"config","profile":{"mode":null,"allow":null,"deny":null,"rule":null}},
              {"name":"b","source":"user","profile":{}},
              {"name":"c","source":"user","profile":{"allow":["x.com"]}}
            ]
            """
        StubURLProtocol.reset([.init(status: 200, body: Data(body.utf8))])

        let profiles = try await client().egressProfiles()
        XCTAssertEqual(profiles.count, 3)
        XCTAssertNil(profiles[0].profile.mode)
        XCTAssertNil(profiles[0].profile.allow)
        XCTAssertNil(profiles[1].profile.allow) // omitted → nil
        XCTAssertNil(profiles[1].profile.rule)
        XCTAssertEqual(profiles[2].profile.allow, ["x.com"])
        XCTAssertNil(profiles[2].profile.deny)
    }
}
