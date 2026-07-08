// HostAgentClient token.get correlation tests (Phase 5b). Drives the real
// client against a minimal in-test UDS host-agent that speaks the same
// newline-JSON framing, exercising the request/response correlation, the
// fail-closed error reply, the timeout backstop, and the disconnect path.

import Darwin
import Foundation
import XCTest

@testable import ShedKit

final class HostAgentClientTokenTests: XCTestCase {
    private static let info = HelloClientInfo(
        name: "test", version: "0", pid: 1, capabilities: [], replayEvents: 0)

    private func tempSocketPath() -> String {
        // Short path under /tmp — sockaddr_un.sun_path is ~104 bytes and the
        // system temp dir is too long on macOS.
        "/tmp/shed-fake-\(UUID().uuidString.prefix(8)).sock"
    }

    /// Start the client draining its event stream (realistic usage; also keeps
    /// the AsyncStream alive so it isn't terminated before the test runs).
    private func startDraining(_ client: HostAgentClient) -> Task<Void, Never> {
        let stream = client.start(client: Self.info)
        return Task { for await _ in stream {} }
    }

    /// Poll until the client has a live fd (connectOnce sets it right after the
    /// UDS connect succeeds), failing after ~5s.
    private func waitConnected(
        _ client: HostAgentClient, file: StaticString = #filePath, line: UInt = #line
    ) async throws {
        for _ in 0..<200 {
            if client.isConnected { return }
            try await Task.sleep(for: .milliseconds(25))
        }
        XCTFail("client never connected to fake host-agent", file: file, line: line)
    }

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
}

/// Minimal in-test UDS server mimicking shed-host-agent's framing: accepts one
/// client, greets with a `hello_ack`, then replies to `token.get` per `mode`.
/// Reuses ShedKit's public socket helpers (makeUnixSocketAddress/writeAll/
/// LineFrameReader) so the wire path matches the real agent.
final class FakeHostAgent: @unchecked Sendable {
    enum Mode {
        case reply(token: String?, expiresAt: String?, error: String?)
        case silent  // read the token.get, never reply (drives the client timeout)
        case dropOnGet  // close the conn on token.get (drives the disconnect path)
    }
    enum FakeError: Error { case socket, address, bind, listen }

    private let path: String
    private let mode: Mode
    private var listenFD: Int32 = -1

    init(path: String, mode: Mode) {
        self.path = path
        self.mode = mode
    }

    func start() throws {
        unlink(path)
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw FakeError.socket }
        guard var addr = makeUnixSocketAddress(path: path) else {
            Darwin.close(fd)
            throw FakeError.address
        }
        let rc = withUnsafePointer(to: &addr) { p in
            p.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                Darwin.bind(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard rc == 0 else { Darwin.close(fd); throw FakeError.bind }
        guard Darwin.listen(fd, 1) == 0 else { Darwin.close(fd); throw FakeError.listen }
        listenFD = fd
        let t = Thread { [weak self] in self?.serve() }
        t.stackSize = 1 << 20
        t.start()
    }

    func stop() {
        if listenFD >= 0 { Darwin.close(listenFD); listenFD = -1 }
        unlink(path)
    }

    private func serve() {
        let conn = accept(listenFD, nil, nil)
        guard conn >= 0 else { return }
        // Greet so the client's runLoop yields .connected (also realistic).
        _ = writeAll(
            fd: conn,
            data: lineData([
                "v": hostAgentProtocolVersion, "type": "hello_ack",
                "namespaces": [], "gate_namespaces": [],
                "request_timeout_ms": 5000, "accepted": true,
            ]))
        var reader = LineFrameReader(fd: conn)
        while let line = try? reader.readLine() {
            guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
                obj["type"] as? String == "token.get"
            else { continue }
            let id = obj["id"] as? String ?? ""
            let server = obj["server"] as? String ?? ""
            switch mode {
            case .silent:
                continue
            case .dropOnGet:
                Darwin.close(conn)
                return
            case .reply(let token, let expiresAt, let error):
                var resp: [String: Any] = [
                    "v": hostAgentProtocolVersion, "type": "token.response",
                    "in_reply_to": id, "server": server,
                ]
                if let token { resp["token"] = token }
                if let expiresAt { resp["expires_at"] = expiresAt }
                if let error { resp["error"] = error }
                _ = writeAll(fd: conn, data: lineData(resp))
            }
        }
        Darwin.close(conn)
    }

    private func lineData(_ obj: [String: Any]) -> Data {
        var d = (try? JSONSerialization.data(withJSONObject: obj)) ?? Data()
        d.append(0x0a)
        return d
    }
}
