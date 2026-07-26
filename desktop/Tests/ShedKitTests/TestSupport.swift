// TestSupport.swift — shared ShedKitTests infrastructure.
//
// Two things every UDS/parity test in this target reached for its own copy of:
//
//   * `FakeHostAgent` — the in-test shed-host-agent: real Unix socket, the same
//     newline-JSON framing, a configurable `hello_ack` (capability list + delay),
//     `token.get` / `credential.get` replies, a connection it can drop on demand,
//     and a verbatim capture of every app→agent line. One fake, so a framing
//     change lands in one place and every consumer keeps testing the real wire.
//   * `RepoFixtures` — the cross-language fixtures that live OUTSIDE the Swift
//     package, located from `#filePath` (no SwiftPM resource copy; one file on
//     disk that Go, Rust and Swift all assert against).

import Darwin
import Foundation
import XCTest

@testable import ShedKit

// MARK: - repo-root fixtures

enum RepoFixtures {
    /// The monorepo root, walked up from this file:
    /// `<root>/desktop/Tests/ShedKitTests/TestSupport.swift`.
    static let root = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()  // ShedKitTests
        .deletingLastPathComponent()  // Tests
        .deletingLastPathComponent()  // desktop/ (Swift package root)
        .deletingLastPathComponent()  // monorepo root

    static func url(_ relativePath: String) -> URL { root.appendingPathComponent(relativePath) }

    static func json(_ relativePath: String) throws -> [String: Any] {
        let data = try Data(contentsOf: url(relativePath))
        return try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
    }

    /// `tests/host-agent-diff/fixtures/desktop-credential/<name>.json` — plan 002
    /// §7 P9's shared credential-wire vectors.
    static func desktopCredential(_ name: String) throws -> [String: Any] {
        try json("tests/host-agent-diff/fixtures/desktop-credential/\(name).json")
    }
}

// MARK: - host-agent client helpers

extension HelloClientInfo {
    static let test = HelloClientInfo(
        name: "test", version: "0", pid: 1, capabilities: [], replayEvents: 0)
}

extension XCTestCase {
    /// Short path under /tmp — `sockaddr_un.sun_path` is ~104 bytes and the
    /// system temp dir is too long on macOS.
    func tempSocketPath() -> String { "/tmp/shed-fake-\(UUID().uuidString.prefix(8)).sock" }

    /// Start the client draining its event stream (realistic usage; also keeps
    /// the AsyncStream alive so it isn't terminated before the test runs).
    func startDraining(_ client: HostAgentClient) -> Task<Void, Never> {
        let stream = client.start(client: .test)
        return Task { for await _ in stream {} }
    }

    /// Poll until the client has a live fd (connectOnce sets it right after the
    /// UDS connect succeeds), failing after ~5s.
    func waitConnected(
        _ client: HostAgentClient, file: StaticString = #filePath, line: UInt = #line
    ) async throws {
        for _ in 0..<200 {
            if client.isConnected { return }
            try await Task.sleep(for: .milliseconds(25))
        }
        XCTFail("client never connected to the fake host-agent", file: file, line: line)
    }

    /// Poll until a connection NEWER than `generation` has learned `want`.
    /// Waiting on the state alone races a reconnect: the old connection still
    /// reads `.supported` for the instant before its disconnect lands.
    @discardableResult
    func waitCapability(
        _ client: HostAgentClient, _ want: AgentCapabilityState, afterGeneration generation: UInt64,
        file: StaticString = #filePath, line: UInt = #line
    ) async throws -> AgentCapabilitySnapshot {
        for _ in 0..<200 {
            let snapshot = client.credentialCapabilitySnapshot
            if snapshot.state == want, snapshot.generation > generation { return snapshot }
            try await Task.sleep(for: .milliseconds(25))
        }
        XCTFail(
            "no connection after generation \(generation) became \(want) "
                + "(is \(client.credentialCapabilitySnapshot))", file: file, line: line)
        return client.credentialCapabilitySnapshot
    }

    /// Poll until this connection's `hello_ack` has been learned as `want`.
    func waitCapability(
        _ client: HostAgentClient, _ want: AgentCapabilityState,
        file: StaticString = #filePath, line: UInt = #line
    ) async throws {
        for _ in 0..<200 {
            if client.credentialCapability == want { return }
            try await Task.sleep(for: .milliseconds(25))
        }
        XCTFail(
            "capability never became \(want) (is \(client.credentialCapability))",
            file: file, line: line)
    }
}

// MARK: - the fake agent

/// Accepts connections in a LOOP — the real client reconnects with backoff, and
/// a one-shot accept would make the reconnect cells hang instead of fail.
final class FakeHostAgent: @unchecked Sendable {
    /// How the fake answers `token.get`.
    enum Mode {
        /// `server` overrides the echoed `token.get` server when non-nil (wrong-server tests).
        case reply(token: String?, expiresAt: String? = nil, error: String? = nil, server: String? = nil)
        case silent  // read the token.get, never reply (drives the client timeout)
        case dropOnGet  // close the conn on token.get (drives the disconnect path)
    }
    enum FakeError: Error { case socket, address, bind, listen }

    static let defaultToken = "shed_control_tok"
    /// Computed, not a `static let`: `[String: Any]` isn't Sendable, so a stored
    /// global trips Swift 6's concurrency check.
    static var defaultCredentialReply: [String: Any] {
        ["auth_mode": "token", "token": FakeHostAgent.defaultToken]
    }

    private let path: String
    private let mode: Mode
    private let helloAckDelayMs: Int
    /// Extra fields merged into the `credential.response` (the envelope +
    /// `in_reply_to` + `server` are filled in by the fake).
    private let credentialReply: [String: Any]
    private let lock = NSLock()
    private var advertisesCredentialGet: Bool
    private var listenFD: Int32 = -1
    private var connFD: Int32 = -1
    private var captured: [String] = []
    private var running = true

    init(
        path: String,
        mode: Mode = .reply(token: FakeHostAgent.defaultToken),
        advertisesCredentialGet: Bool = false,
        helloAckDelayMs: Int = 0,
        credentialReply: [String: Any] = FakeHostAgent.defaultCredentialReply
    ) {
        self.path = path
        self.mode = mode
        self.advertisesCredentialGet = advertisesCredentialGet
        self.helloAckDelayMs = helloAckDelayMs
        self.credentialReply = credentialReply
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
        guard Darwin.listen(fd, 4) == 0 else { Darwin.close(fd); throw FakeError.listen }
        listenFD = fd
        let t = Thread { [weak self] in self?.acceptLoop() }
        t.stackSize = 1 << 20
        t.start()
    }

    func stop() {
        lock.lock()
        running = false
        let listen = listenFD
        listenFD = -1
        let conn = connFD
        connFD = -1
        lock.unlock()
        if conn >= 0 { _ = Darwin.shutdown(conn, SHUT_RDWR) }
        if listen >= 0 { Darwin.close(listen) }
        unlink(path)
    }

    func setAdvertisesCredentialGet(_ on: Bool) {
        lock.lock(); advertisesCredentialGet = on; lock.unlock()
    }

    /// Write a rejection `hello_ack` ("superseded") on the live connection — the
    /// frame the agent sends the loser when a second app takes over. It carries
    /// no capability list and describes no working session.
    func sendSupersededAck() {
        lock.lock()
        let conn = connFD
        lock.unlock()
        guard conn >= 0 else { return }
        _ = writeAll(
            fd: conn,
            data: lineData([
                "v": hostAgentProtocolVersion, "type": "hello_ack",
                "accepted": false, "reason": "superseded",
            ]))
    }

    /// Drop the current connection (simulating an agent restart) without closing
    /// the listener, so the client's reconnect lands on the new advertisement.
    func dropConnection() {
        lock.lock()
        let conn = connFD
        connFD = -1
        lock.unlock()
        if conn >= 0 { _ = Darwin.shutdown(conn, SHUT_RDWR) }
    }

    /// Every app→agent line, verbatim.
    func rawLines() -> [String] {
        lock.lock(); defer { lock.unlock() }
        return captured
    }

    func frames() -> [[String: Any]] {
        rawLines().compactMap {
            (try? JSONSerialization.jsonObject(with: Data($0.utf8))) as? [String: Any]
        }
    }

    /// The `type` of each captured frame, in order — the cheapest way to state
    /// "a token.get was never sent".
    func frameTypes() -> [String] {
        frames().compactMap { $0["type"] as? String }
    }

    private func isRunning() -> Bool { lock.lock(); defer { lock.unlock() }; return running }

    private func acceptLoop() {
        while isRunning() {
            lock.lock()
            let listen = listenFD
            lock.unlock()
            guard listen >= 0 else { return }
            let conn = accept(listen, nil, nil)
            guard conn >= 0 else { return }
            lock.lock()
            connFD = conn
            let delay = helloAckDelayMs
            let advertise = advertisesCredentialGet
            lock.unlock()

            if delay > 0 { Thread.sleep(forTimeInterval: Double(delay) / 1000.0) }
            var ack: [String: Any] = [
                "v": hostAgentProtocolVersion, "type": "hello_ack",
                "namespaces": [], "gate_namespaces": [],
                "request_timeout_ms": 5000, "accepted": true,
            ]
            if advertise { ack["agent_capabilities"] = [HostAgentCapability.credentialGet] }
            _ = writeAll(fd: conn, data: lineData(ack))

            serve(conn: conn)
            lock.lock()
            if connFD == conn { connFD = -1 }
            lock.unlock()
            Darwin.close(conn)
        }
    }

    private func serve(conn: Int32) {
        var reader = LineFrameReader(fd: conn)
        while let line = try? reader.readLine() {
            lock.lock(); captured.append(String(decoding: line, as: UTF8.self)); lock.unlock()
            guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any] else {
                continue
            }
            let id = obj["id"] as? String ?? ""
            let server = obj["server"] as? String ?? ""
            switch obj["type"] as? String {
            case "token.get":
                switch mode {
                case .silent:
                    continue
                case .dropOnGet:
                    return
                case .reply(let token, let expiresAt, let error, let serverOverride):
                    var resp: [String: Any] = [
                        "v": hostAgentProtocolVersion, "type": "token.response",
                        "in_reply_to": id, "server": serverOverride ?? server,
                    ]
                    if let token { resp["token"] = token }
                    if let expiresAt { resp["expires_at"] = expiresAt }
                    if let error { resp["error"] = error }
                    _ = writeAll(fd: conn, data: lineData(resp))
                }
            case "credential.get":
                var resp: [String: Any] = [
                    "v": hostAgentProtocolVersion, "type": "credential.response",
                    "in_reply_to": id, "server": server,
                ]
                for (k, v) in credentialReply { resp[k] = v }
                _ = writeAll(fd: conn, data: lineData(resp))
            default:
                continue
            }
        }
    }

    private func lineData(_ obj: [String: Any]) -> Data {
        var d = (try? JSONSerialization.data(withJSONObject: obj)) ?? Data()
        d.append(0x0a)
        return d
    }
}
