// ControlTokenProvider tests (Phase 5c-i). The cache/refresh/single-flight/
// invalidate logic is driven with an injected mint closure + a controllable
// clock; two factory tests exercise the real HostAgentClient adapter against
// the shared in-test FakeHostAgent (fail-closed on an error reply).

import Foundation
import XCTest

@testable import ShedKit

/// A clock the test advances by hand, so refresh-window logic is deterministic.
final class TestClock: @unchecked Sendable {
    private let lock = NSLock()
    private var current: Date
    init(_ start: Date) { current = start }
    var now: Date {
        lock.lock()
        defer { lock.unlock() }
        return current
    }
    func advance(_ seconds: TimeInterval) {
        lock.lock()
        current += seconds
        lock.unlock()
    }
}

/// Records mint calls and serves a configurable result, so tests can assert
/// how many times the provider actually minted.
actor MintRecorder {
    private(set) var calls = 0
    var expiry: Date?
    var failTimes = 0
    var delay: Duration = .zero

    init(expiry: Date? = nil) { self.expiry = expiry }

    func setExpiry(_ d: Date?) { expiry = d }
    func setFailTimes(_ n: Int) { failTimes = n }
    func setDelay(_ d: Duration) { delay = d }

    func mint() async throws -> MintedToken {
        calls += 1
        let n = calls
        if delay > .zero { try? await Task.sleep(for: delay) }
        if failTimes > 0 {
            failTimes -= 1
            throw ControlTokenError.mintFailed("boom")
        }
        return MintedToken(token: "tok-\(n)", expiresAt: expiry)
    }
}

final class ControlTokenProviderTests: XCTestCase {
    private func makeProvider(_ rec: MintRecorder, window: TimeInterval, clock: TestClock) -> ControlTokenProvider {
        ControlTokenProvider(refreshWindow: window, now: { clock.now }) { try await rec.mint() }
    }

    /// `XCTAssertEqual` takes autoclosure args that can't `await`, so bind first.
    private func assertToken(
        _ p: ControlTokenProvider, _ expected: String,
        file: StaticString = #filePath, line: UInt = #line
    ) async throws {
        let t = try await p.token()
        XCTAssertEqual(t, expected, file: file, line: line)
    }

    func testCachesAfterFirstMint() async throws {
        let clock = TestClock(Date(timeIntervalSince1970: 1_000))
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))  // far future
        let p = makeProvider(rec, window: 60, clock: clock)

        try await assertToken(p, "tok-1")
        try await assertToken(p, "tok-1")
        let calls = await rec.calls
        XCTAssertEqual(calls, 1)
    }

    func testRefreshesWithinWindow() async throws {
        let t0 = Date(timeIntervalSince1970: 1_000)
        let clock = TestClock(t0)
        let rec = MintRecorder(expiry: t0.addingTimeInterval(100))
        let p = makeProvider(rec, window: 60, clock: clock)

        // now=t0, expiry-window = t0+40; t0 < t0+40 → cached.
        try await assertToken(p, "tok-1")
        try await assertToken(p, "tok-1")

        // Advance to t0+50 ≥ t0+40 → within the window → re-mint.
        clock.advance(50)
        await rec.setExpiry(t0.addingTimeInterval(300))
        try await assertToken(p, "tok-2")
        let calls = await rec.calls
        XCTAssertEqual(calls, 2)
    }

    func testNoExpiryNeverTimeRefreshes() async throws {
        let clock = TestClock(Date(timeIntervalSince1970: 1_000))
        let rec = MintRecorder(expiry: nil)  // no expiry reported
        let p = makeProvider(rec, window: 60, clock: clock)

        try await assertToken(p, "tok-1")
        clock.advance(1_000_000_000)
        try await assertToken(p, "tok-1")  // time alone never refreshes
        var calls = await rec.calls
        XCTAssertEqual(calls, 1)

        // …but an explicit invalidate (the 401 path) still forces a re-mint.
        await p.invalidate()
        try await assertToken(p, "tok-2")
        calls = await rec.calls
        XCTAssertEqual(calls, 2)
    }

    func testInvalidateForcesRemint() async throws {
        let clock = TestClock(Date(timeIntervalSince1970: 1_000))
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        let p = makeProvider(rec, window: 60, clock: clock)

        try await assertToken(p, "tok-1")
        await p.invalidate()
        try await assertToken(p, "tok-2")
        let calls = await rec.calls
        XCTAssertEqual(calls, 2)
    }

    func testSingleFlightCollapsesConcurrentMints() async throws {
        let clock = TestClock(Date(timeIntervalSince1970: 1_000))
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        await rec.setDelay(.milliseconds(100))  // keep the first mint in flight while the others arrive
        let p = makeProvider(rec, window: 60, clock: clock)

        let results = try await withThrowingTaskGroup(of: String.self) { group -> [String] in
            for _ in 0..<8 { group.addTask { try await p.token() } }
            var out: [String] = []
            for try await r in group { out.append(r) }
            return out
        }
        XCTAssertEqual(results, Array(repeating: "tok-1", count: 8))
        let calls = await rec.calls
        XCTAssertEqual(calls, 1)  // 8 callers, one mint
    }

    func testMintFailurePropagatesAndDoesNotPoison() async throws {
        let clock = TestClock(Date(timeIntervalSince1970: 1_000))
        let rec = MintRecorder(expiry: Date(timeIntervalSince1970: 100_000))
        await rec.setFailTimes(1)
        let p = makeProvider(rec, window: 60, clock: clock)

        do {
            _ = try await p.token()
            XCTFail("expected mintFailed")
        } catch let e as ControlTokenError {
            XCTAssertEqual(e, .mintFailed("boom"))
        }
        // The failure left no cached token and cleared the in-flight slot, so a
        // later call retries cleanly.
        try await assertToken(p, "tok-2")
        let calls = await rec.calls
        XCTAssertEqual(calls, 2)
    }

    // MARK: - hostAgent factory (real client adapter)

    private static let info = HelloClientInfo(
        name: "test", version: "0", pid: 1, capabilities: [], replayEvents: 0)

    private func tempSocketPath() -> String { "/tmp/shed-ctl-\(UUID().uuidString.prefix(8)).sock" }

    private func waitConnected(_ client: HostAgentClient) async throws {
        for _ in 0..<200 {
            if client.isConnected { return }
            try await Task.sleep(for: .milliseconds(25))
        }
        XCTFail("client never connected to fake host-agent")
    }

    func testHostAgentFactorySuccessParsesTokenAndExpiry() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(
            path: path, mode: .reply(token: "ctl-abc", expiresAt: "2026-06-14T01:00:00Z", error: nil))
        try fake.start()
        defer { fake.stop() }
        let client = HostAgentClient(socketPath: path)
        let stream = client.start(client: Self.info)
        let drain = Task { for await _ in stream {} }
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)

        let provider = ControlTokenProvider.hostAgent(client, server: "mini3")
        try await assertToken(provider, "ctl-abc")
    }

    func testHostAgentFactoryFailClosedOnErrorReply() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(path: path, mode: .reply(token: nil, expiresAt: nil, error: "no ssh_port"))
        try fake.start()
        defer { fake.stop() }
        let client = HostAgentClient(socketPath: path)
        let stream = client.start(client: Self.info)
        let drain = Task { for await _ in stream {} }
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)

        let provider = ControlTokenProvider.hostAgent(client, server: "mini3")
        do {
            _ = try await provider.token()
            XCTFail("expected mintFailed")
        } catch let e as ControlTokenError {
            XCTAssertEqual(e, .mintFailed("no ssh_port"))
        }
    }
}
