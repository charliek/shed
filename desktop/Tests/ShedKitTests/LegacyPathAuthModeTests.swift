// LegacyPathAuthModeTests — plan 002 §7 P6's zero-network guarantee.
//
// `SHED_DESKTOP_RUST_CORE=0` selects the legacy Swift `URLSession` path, which
// has no way to present a client certificate. Against an mtls server that path
// must not "try anyway": an unauthenticatable request produces a TLS alert that
// reads like a network fault and buries the one sentence that fixes it. So the
// client refuses at CONSTRUCTION, and these tests prove the refusal by counting
// requests at the URLProtocol seam — zero, across reads, lifecycle writes, and
// the streaming create.

import Foundation
import XCTest

@testable import ShedKit

/// Counts every request that reaches the transport. Any request at all is a
/// failure for the mtls cells; the token control cell asserts the counter is
/// wired up by observing a non-zero count.
final class RequestCountingURLProtocol: URLProtocol {
    nonisolated(unsafe) private static var count = 0
    nonisolated(unsafe) private static var urls: [String] = []
    private static let lock = NSLock()

    static func reset() {
        lock.lock()
        count = 0
        urls = []
        lock.unlock()
    }

    static var requestCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return count
    }

    static var requestedURLs: [String] {
        lock.lock()
        defer { lock.unlock() }
        return urls
    }

    override class func canInit(with request: URLRequest) -> Bool {
        lock.lock()
        count += 1
        urls.append(request.url?.absoluteString ?? "?")
        lock.unlock()
        return true
    }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}

    override func startLoading() {
        let http = HTTPURLResponse(
            url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: nil)!
        client?.urlProtocol(self, didReceive: http, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(#"{"sheds":null}"#.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
}

final class LegacyPathAuthModeTests: XCTestCase {
    private func spySession() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [RequestCountingURLProtocol.self]
        return URLSession(configuration: config)
    }

    private func client(authMode: ShedAuthMode) -> ShedServerClient {
        // useRustCore defaults to false — this IS the legacy path.
        ShedServerClient(
            baseURL: URL(string: "http://stub.local")!, serverName: "prod",
            token: "static-tok", session: spySession(), authMode: authMode)
    }

    private func expectUnsupported(
        _ body: () async throws -> Void, _ label: String,
        file: StaticString = #filePath, line: UInt = #line
    ) async {
        do {
            try await body()
            XCTFail("\(label): expected an unsupported-auth-mode error", file: file, line: line)
        } catch let e as ShedClientError {
            guard case .unsupportedAuthMode(let m) = e else {
                return XCTFail("\(label): expected unsupportedAuthMode, got \(e)", file: file, line: line)
            }
            XCTAssertTrue(
                m.contains("requires mtls") && m.contains("unset SHED_DESKTOP_RUST_CORE"),
                "\(label): the message must name the fix, got: \(m)", file: file, line: line)
        } catch {
            XCTFail("\(label): unexpected error \(error)", file: file, line: line)
        }
    }

    func testMtlsEntryOnLegacyPathMakesZeroRequests() async throws {
        RequestCountingURLProtocol.reset()
        let c = client(authMode: .mtls)

        await expectUnsupported({ _ = try await c.info() }, "info")
        await expectUnsupported({ _ = try await c.listSheds() }, "listSheds")
        await expectUnsupported({ _ = try await c.systemDF() }, "systemDF")
        await expectUnsupported({ _ = try await c.listImages() }, "listImages")
        await expectUnsupported({ _ = try await c.egressProfiles() }, "egressProfiles")
        await expectUnsupported({ try await c.start(name: "s") }, "start")
        await expectUnsupported({ try await c.stop(name: "s") }, "stop")
        await expectUnsupported({ try await c.reset(name: "s") }, "reset")
        await expectUnsupported({ try await c.delete(name: "s") }, "delete")

        // The streaming create builds its request inside a Task; the refusal has
        // to happen before that too.
        await expectUnsupported(
            {
                for try await _ in c.createShed(CreateShedRequest(name: "s")) {}
            }, "createShed")

        XCTAssertEqual(
            RequestCountingURLProtocol.requestCount, 0,
            "the legacy path must send NOTHING to an mtls server; saw \(RequestCountingURLProtocol.requestedURLs)")
    }

    func testTokenEntryOnLegacyPathStillSendsRequests() async throws {
        // The control: the same construction with a token entry is unaffected,
        // which is also what proves the spy counts.
        RequestCountingURLProtocol.reset()
        let c = client(authMode: .token)
        _ = try await c.listSheds()
        XCTAssertEqual(RequestCountingURLProtocol.requestCount, 1)
    }

    func testTheMtlsRefusalWinsOverAPinMisconfiguration() async throws {
        // Two terminal diagnoses at once (a pin on a plain-http URL AND an mtls
        // entry on the legacy path). The dedicated one wins: fixing the pin would
        // leave the user exactly as unable to connect, so naming it first sends
        // them down the wrong road.
        RequestCountingURLProtocol.reset()
        let c = ShedServerClient(
            baseURL: URL(string: "http://stub.local")!, serverName: "prod",
            tlsCertFingerprint: "sha256:aabbcc", session: spySession(), authMode: .mtls)
        await expectUnsupported({ _ = try await c.listSheds() }, "listSheds")
        await expectUnsupported(
            {
                for try await _ in c.createShed(CreateShedRequest(name: "s")) {}
            }, "createShed")
        XCTAssertEqual(RequestCountingURLProtocol.requestCount, 0)
    }

    func testMtlsEntryOnTheRustCorePathIsNotRefused() async throws {
        // The refusal is scoped to the legacy path: the Rust core is exactly the
        // thing that CAN present a certificate, so an mtls entry there must go
        // out on the wire (and fail here only because the port is closed).
        let c = ShedServerClient(
            baseURL: URL(string: "http://127.0.0.1:1")!, serverName: "prod",
            useRustCore: true, authMode: .mtls)
        XCTAssertNil(c.learnedAuthMode, "nothing is learned before the first mint")
        do {
            _ = try await c.info()
            XCTFail("expected a transport failure against a closed port")
        } catch let e as ShedClientError {
            if case .unsupportedAuthMode = e {
                XCTFail("the Rust-core path must not be refused for an mtls entry")
            }
        } catch {
            // A ShedRustCore.ShedError is the expected shape here.
        }
    }

    func testAbsentAuthModeIsTreatedAsTokenOnTheLegacyPath() async throws {
        // ABSENT MEANS TOKEN: an entry written before certificates existed must
        // not be refused. (An entry whose SERVER has since flipped fails with the
        // raw TLS error — accepted and documented in §7 P6.)
        RequestCountingURLProtocol.reset()
        let entry = ShedServerEntry(name: "prod", host: "stub.local", httpPort: 80, sshPort: 22)
        XCTAssertEqual(entry.authMode, .token)
        let c = ShedServerClient(
            baseURL: URL(string: "http://stub.local")!, serverName: "prod",
            session: spySession(), authMode: entry.authMode)
        _ = try await c.listSheds()
        XCTAssertEqual(RequestCountingURLProtocol.requestCount, 1)
    }
}
