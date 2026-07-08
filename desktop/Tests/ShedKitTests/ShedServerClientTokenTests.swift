// ShedServerClient control-token wiring tests (Phase 5c-ii). A stub URLProtocol
// serves canned responses and records each request's Authorization header, so we
// can assert the 401 → invalidate → re-mint → retry-once path for provider-backed
// (secure) clients and that static-token (open-mode) clients are unchanged.

import Foundation
import XCTest

@testable import ShedKit

/// Canned-response URLProtocol. Serves `queue` in order (200/empty once drained)
/// and records the Authorization header seen on each request. Tests run serially,
/// resetting state per test.
final class StubURLProtocol: URLProtocol {
    struct Stub {
        let status: Int
        let body: Data
    }

    nonisolated(unsafe) private static var queue: [Stub] = []
    nonisolated(unsafe) private static var auths: [String?] = []
    private static let lock = NSLock()

    static func reset(_ stubs: [Stub]) {
        lock.lock()
        queue = stubs
        auths = []
        lock.unlock()
    }

    static func recordedAuths() -> [String?] {
        lock.lock()
        defer { lock.unlock() }
        return auths
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}

    override func startLoading() {
        Self.lock.lock()
        Self.auths.append(request.value(forHTTPHeaderField: "Authorization"))
        let stub = Self.queue.isEmpty ? Stub(status: 200, body: Data()) : Self.queue.removeFirst()
        Self.lock.unlock()

        let http = HTTPURLResponse(
            url: request.url!, statusCode: stub.status, httpVersion: "HTTP/1.1", headerFields: nil)!
        client?.urlProtocol(self, didReceive: http, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: stub.body)
        client?.urlProtocolDidFinishLoading(self)
    }
}

final class ShedServerClientTokenTests: XCTestCase {
    private func stubSession() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        return URLSession(configuration: config)
    }

    private func provider(_ rec: MintRecorder) -> ControlTokenProvider {
        ControlTokenProvider(now: { Date(timeIntervalSince1970: 1_000) }) { try await rec.mint() }
    }

    private func client(_ p: ControlTokenProvider? = nil, token: String = "") -> ShedServerClient {
        ShedServerClient(
            baseURL: URL(string: "http://stub.local")!, serverName: "t",
            token: token, tokenProvider: p, session: stubSession())
    }

    func testRetriesOnceOn401WithFreshlyMintedToken() async throws {
        StubURLProtocol.reset([
            .init(status: 401, body: Data()),
            .init(status: 200, body: Data(#"{"sheds":null}"#.utf8)),
        ])
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        let c = client(provider(rec))

        let sheds = try await c.listSheds()
        XCTAssertEqual(sheds.count, 0)

        // First request used the cached tok-1 → 401; the retry re-minted tok-2.
        XCTAssertEqual(StubURLProtocol.recordedAuths(), ["Bearer tok-1", "Bearer tok-2"])
        let calls = await rec.calls
        XCTAssertEqual(calls, 2)
    }

    func testStaticTokenSurfaces401WithoutRetry() async throws {
        StubURLProtocol.reset([.init(status: 401, body: Data())])
        let c = client(token: "static-tok")
        do {
            _ = try await c.listSheds()
            XCTFail("expected badStatus(401)")
        } catch let e as ShedClientError {
            guard case .badStatus(401) = e else { return XCTFail("expected badStatus(401), got \(e)") }
        }
        // Exactly one request, carrying the static token — no refresh path.
        XCTAssertEqual(StubURLProtocol.recordedAuths(), ["Bearer static-tok"])
    }

    func testProviderTokenUsedOnHappyPath() async throws {
        StubURLProtocol.reset([.init(status: 200, body: Data(#"{"sheds":null}"#.utf8))])
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        let c = client(provider(rec))

        _ = try await c.listSheds()
        XCTAssertEqual(StubURLProtocol.recordedAuths(), ["Bearer tok-1"])
        let calls = await rec.calls
        XCTAssertEqual(calls, 1)  // no 401 → minted once, no retry
    }

    func testSecondConsecutive401SurfacesAfterOneRetry() async throws {
        // Both attempts 401 (e.g. the token is genuinely unauthorized): surface
        // the 401 after exactly one retry, not an infinite loop.
        StubURLProtocol.reset([.init(status: 401, body: Data()), .init(status: 401, body: Data())])
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        let c = client(provider(rec))
        do {
            _ = try await c.listSheds()
            XCTFail("expected badStatus(401)")
        } catch let e as ShedClientError {
            guard case .badStatus(401) = e else { return XCTFail("expected badStatus(401), got \(e)") }
        }
        XCTAssertEqual(StubURLProtocol.recordedAuths(), ["Bearer tok-1", "Bearer tok-2"])
    }

    func testProviderMintFailureSendsNoTokenNeverStatic() async throws {
        // Security: a mint failure must NOT downgrade to the static config token.
        // The request goes out with no Authorization (a secure server 401s; an
        // open server accepts it) — never the legacy static token.
        StubURLProtocol.reset([.init(status: 200, body: Data(#"{"sheds":null}"#.utf8))])
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        await rec.setFailTimes(100)
        let c = client(provider(rec), token: "static-must-not-be-sent")

        let sheds = try await c.listSheds()
        XCTAssertEqual(sheds.count, 0)
        XCTAssertEqual(StubURLProtocol.recordedAuths(), [nil])  // no Authorization header
    }

    func testProviderMintFailureFailsClosedThroughRetry() async throws {
        // A secure server whose mint keeps failing: the attempt AND the 401-retry
        // both send no token — never the static fallback — and the 401 surfaces.
        StubURLProtocol.reset([.init(status: 401, body: Data()), .init(status: 401, body: Data())])
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        await rec.setFailTimes(100)
        let c = client(provider(rec), token: "static-must-not-be-sent")
        do {
            _ = try await c.listSheds()
            XCTFail("expected badStatus(401)")
        } catch let e as ShedClientError {
            guard case .badStatus(401) = e else { return XCTFail("expected badStatus(401), got \(e)") }
        }
        XCTAssertEqual(StubURLProtocol.recordedAuths(), [nil, nil])  // never the static token
    }
}
