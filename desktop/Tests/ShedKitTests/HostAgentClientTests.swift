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
