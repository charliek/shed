// HostAgentClient token.get correlation tests (Phase 5b). Drives the real
// client against the shared in-test UDS host-agent (`FakeHostAgent` in
// TestSupport.swift), exercising the request/response correlation, the
// fail-closed error reply, the timeout backstop, and the disconnect path.

import Darwin
import Foundation
import XCTest

@testable import ShedKit

final class HostAgentClientTokenTests: XCTestCase {
    func testRequestTokenRoundTrip() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(
            path: path, mode: .reply(token: "shed_control_xyz", expiresAt: "2026-06-14T01:00:00Z", error: nil))
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let drain = startDraining(client)
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)

        let resp = try await client.requestToken(server: "mini3")
        XCTAssertEqual(resp.server, "mini3")
        XCTAssertEqual(resp.token, "shed_control_xyz")
        XCTAssertEqual(resp.expiresAt, "2026-06-14T01:00:00Z")
        XCTAssertNil(resp.error)
    }

    func testRequestTokenErrorReplyIsReturnedNotThrown() async throws {
        // A fail-closed reply (error set, token nil) comes back in the struct —
        // it is the caller's to inspect, not an thrown transport error.
        let path = tempSocketPath()
        let fake = FakeHostAgent(
            path: path, mode: .reply(token: nil, expiresAt: nil, error: "host key mismatch"))
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let drain = startDraining(client)
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)

        let resp = try await client.requestToken(server: "mini3")
        XCTAssertEqual(resp.error, "host key mismatch")
        XCTAssertNil(resp.token)
        XCTAssertNil(resp.expiresAt)
    }

    func testRequestTokenTimesOut() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(path: path, mode: .silent)
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let drain = startDraining(client)
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)

        do {
            _ = try await client.requestToken(server: "mini3", timeout: .milliseconds(300))
            XCTFail("expected a timeout")
        } catch let err as HostAgentClientError {
            XCTAssertEqual(err, .timedOut)
        }
    }

    func testRequestTokenFailsOnDisconnect() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(path: path, mode: .dropOnGet)
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let drain = startDraining(client)
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)

        do {
            // Generous timeout so the disconnect (ms) wins the race, not the timer.
            _ = try await client.requestToken(server: "mini3", timeout: .seconds(5))
            XCTFail("expected a disconnect")
        } catch let err as HostAgentClientError {
            XCTAssertEqual(err, .disconnected)
        }
    }

    func testRequestTokenNotConnected() async throws {
        // Never started → no live fd → fails fast (no wait for the timeout).
        let client = HostAgentClient(socketPath: tempSocketPath())
        do {
            _ = try await client.requestToken(server: "mini3")
            XCTFail("expected notConnected")
        } catch let err as HostAgentClientError {
            XCTAssertEqual(err, .notConnected)
        }
    }

    func testStopMidReadWakesWithoutDoubleClose() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(path: path, mode: .silent)
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let drain = startDraining(client)
        defer { drain.cancel() }
        try await waitConnected(client)

        client.stop()
        try await Task.sleep(for: .milliseconds(150))
        client.stop()
        XCTAssertFalse(client.isConnected)
    }
}

/// A one-shot, thread-safe latch (the write hook runs off the test's actor).
private final class Latch: @unchecked Sendable {
    private let lock = NSLock()
    private var fired = false
    /// `true` the first time only.
    func fire() -> Bool {
        lock.lock(); defer { lock.unlock() }
        if fired { return false }
        fired = true
        return true
    }
    var didFire: Bool {
        lock.lock(); defer { lock.unlock() }
        return fired
    }
}

/// The send-time capability check and the frame write are ONE atomic step.
/// Rust holds its state lock across the same span (`shed-app`'s
/// `host_agent.rs`); Swift used to unlock in between, which left a window where
/// the validated connection could be replaced and the `credential.get` land on
/// the NEW, not-yet-acked one — dropped by an old agent, or answered by a new
/// one with an extra mint whose reply has no waiter.
final class HostAgentClientAtomicSendTests: XCTestCase {
    func testValidatedCredentialGetIsNeverWrittenOnAReplacedConnection() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(path: path, advertisesCredentialGet: true)
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let latch = Latch()
        // Hooked at the instant between "the capability check passed" and the
        // write: tear the validated connection down and give the client every
        // chance to replace it (its reconnect backoff is 0.5 s). It cannot —
        // swapping the fd needs the very lock this request is holding — so the
        // frame can only ever reach the connection that was validated.
        client.beforeWriteHookForTests = { [weak fake] in
            guard let fake, latch.fire() else { return }
            fake.dropConnection()
            let deadline = Date().addingTimeInterval(1.2)
            while Date() < deadline, fake.connectionCount() == 1 {
                Thread.sleep(forTimeInterval: 0.02)
            }
        }

        let drain = startDraining(client)
        defer { drain.cancel(); client.stop() }
        try await waitConnected(client)
        try await waitCapability(client, .supported)
        let snapshot = client.credentialCapabilitySnapshot

        // The request itself is allowed to fail — the connection it validated
        // was torn down under it. What must never happen is the frame showing
        // up on the replacement.
        _ = try? await client.requestCredential(
            server: "prod", csrBase64: "Q1NS", capability: snapshot, timeout: .seconds(2))

        XCTAssertTrue(latch.didFire, "the write hook never ran — the test proved nothing")
        // Let the reconnect land so there IS a later connection to inspect.
        try await Task.sleep(for: .milliseconds(900))
        XCTAssertGreaterThan(fake.connectionCount(), 1, "the client should have reconnected")
        for index in 1..<fake.connectionCount() {
            XCTAssertFalse(
                fake.frameTypes(connection: index).contains("credential.get"),
                "a credential.get validated against connection 0 landed on connection \(index)")
        }
    }
}
