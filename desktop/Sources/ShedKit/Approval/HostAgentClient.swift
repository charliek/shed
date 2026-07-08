// HostAgentClient.swift — UDS client for shed-host-agent (M3).
//
// Connects to the host agent's socket, registers with a `hello`, streams
// inbound frames (approval requests + the all-namespace audit/event feed),
// answers pings, and sends approve/deny responses. Auto-reconnects with
// backoff. If we're not connected when a decision is made, the response is
// simply dropped — the host agent fails closed (deny), which is correct.

import Darwin
import Foundation

public struct HelloClientInfo: Sendable {
    public let name: String
    public let version: String
    public let pid: Int32
    public let capabilities: [String]
    public let replayEvents: Int
    public init(name: String, version: String, pid: Int32, capabilities: [String], replayEvents: Int) {
        self.name = name
        self.version = version
        self.pid = pid
        self.capabilities = capabilities
        self.replayEvents = replayEvents
    }
}

public enum HostAgentEvent: Sendable {
    case connected(HelloAck)
    case disconnected
    case frame(HostAgentInbound)
}

public enum HostAgentClientError: Error, Equatable, CustomStringConvertible, Sendable {
    case notConnected
    case timedOut
    case disconnected
    case encodingFailed

    public var description: String {
        switch self {
        case .notConnected: return "host agent not connected"
        case .timedOut: return "timed out waiting for host agent reply"
        case .disconnected: return "host agent connection dropped"
        case .encodingFailed: return "failed to encode host agent request"
        }
    }
}

public final class HostAgentClient: @unchecked Sendable {
    private let socketPath: String
    private let lock = NSLock()
    private var currentFD: Int32 = -1
    private var running = false
    private var loopTask: Task<Void, Never>?
    /// In-flight `token.get` requests keyed by request id, each awaiting the
    /// correlated `token.response` (matched by `in_reply_to`). Guarded by `lock`.
    /// removeValue is the single-resume guard — whoever removes a continuation
    /// owns its (exactly-once) resume.
    private var pending: [String: CheckedContinuation<TokenResponse, Error>] = [:]
    /// Per-request timeout tasks keyed by the same id; cancelled when the request
    /// resolves (reply/disconnect/stop) so a resolved request leaves no lingering
    /// timer. Guarded by `lock`.
    private var pendingTimeouts: [String: Task<Void, Never>] = [:]

    public init(socketPath: String) {
        self.socketPath = socketPath
    }

    /// Start connecting and return a stream of connection + frame events.
    public func start(client: HelloClientInfo) -> AsyncStream<HostAgentEvent> {
        AsyncStream { continuation in
            lock.lock(); running = true; lock.unlock()
            let task = Task.detached { [weak self] in
                guard let self else { return }
                await self.runLoop(client: client, continuation: continuation)
            }
            lock.lock(); loopTask = task; lock.unlock()
            continuation.onTermination = { [weak self] _ in self?.stop() }
        }
    }

    public func stop() {
        lock.lock()
        running = false
        let task = loopTask
        loopTask = nil
        if currentFD >= 0 { Darwin.close(currentFD); currentFD = -1 }
        lock.unlock()
        task?.cancel()
        failAllPending(error: HostAgentClientError.disconnected)
    }

    public var isConnected: Bool {
        lock.lock(); defer { lock.unlock() }
        return currentFD >= 0
    }

    /// Send an approve/deny for a request. No-op (→ host fails closed) if
    /// not currently connected.
    public func respond(requestID: String, decision: ApprovalDecision, decidedBy: DecidedBy, scope: String? = nil, ttl: String? = nil) {
        guard let data = try? HostAgentProtocol.approvalResponse(
            id: UUID().uuidString, ts: DateFormatting.nowISO8601(), requestID: requestID,
            decision: decision, decidedBy: decidedBy, scope: scope, ttl: ttl) else { return }
        writeLine(data)
    }

    // MARK: - token.get / token.response

    /// Request a CONTROL token for `server` from the host agent over the UDS.
    /// Sends a `token.get` and awaits the correlated `token.response`. Throws
    /// `.notConnected` if there is no live connection, `.timedOut` if no reply
    /// arrives within `timeout`, or `.disconnected` if the connection drops
    /// while waiting. A fail-closed reply (its `error` set, `token` nil) is
    /// returned in the `TokenResponse` — the caller inspects it, it is not thrown.
    public func requestToken(server: String, timeout: Duration = .seconds(10)) async throws -> TokenResponse {
        let id = UUID().uuidString
        guard let data = try? HostAgentProtocol.tokenGet(id: id, server: server) else {
            throw HostAgentClientError.encodingFailed
        }
        return try await withCheckedThrowingContinuation { (cont: CheckedContinuation<TokenResponse, Error>) in
            // Take the lock, fail fast if disconnected, else register before
            // writing so a fast reply can't race ahead of registration. (writeLine
            // re-takes the lock, so write after unlock — NSLock isn't reentrant.)
            lock.lock()
            guard currentFD >= 0 else {
                lock.unlock()
                cont.resume(throwing: HostAgentClientError.notConnected)
                return
            }
            pending[id] = cont
            // Arm the timeout only AFTER registering, so it can always find the
            // continuation (a tiny timeout can't fire before registration). It's a
            // backstop so a registered continuation never leaks; the resolve paths
            // cancel it.
            pendingTimeouts[id] = Task { [weak self] in
                try? await Task.sleep(for: timeout)
                guard !Task.isCancelled else { return }
                self?.failPending(id: id, error: HostAgentClientError.timedOut)
            }
            lock.unlock()
            writeLine(data)
        }
    }

    /// Resume the request matching `resp.inReplyTo`. A no-op if it already timed
    /// out or was failed by a disconnect (removeValue is the single-resume guard).
    private func resolvePending(_ resp: TokenResponse) {
        lock.lock()
        let cont = pending.removeValue(forKey: resp.inReplyTo)
        pendingTimeouts.removeValue(forKey: resp.inReplyTo)?.cancel()
        lock.unlock()
        cont?.resume(returning: resp)
    }

    private func failPending(id: String, error: Error) {
        lock.lock()
        let cont = pending.removeValue(forKey: id)
        pendingTimeouts.removeValue(forKey: id)?.cancel()
        lock.unlock()
        cont?.resume(throwing: error)
    }

    private func failAllPending(error: Error) {
        lock.lock()
        let conts = pending
        pending.removeAll()
        let timeouts = pendingTimeouts
        pendingTimeouts.removeAll()
        lock.unlock()
        for t in timeouts.values { t.cancel() }
        for cont in conts.values { cont.resume(throwing: error) }
    }

    // MARK: - loop

    private func runLoop(client: HelloClientInfo, continuation: AsyncStream<HostAgentEvent>.Continuation) async {
        var backoff = 0.5
        while isRunning(), !Task.isCancelled {
            guard let fd = connectOnce() else {
                try? await Task.sleep(for: .seconds(backoff))
                backoff = min(backoff * 2, 5)
                continue
            }
            setCurrentFD(fd)
            backoff = 0.5
            if let hello = try? HostAgentProtocol.hello(
                id: UUID().uuidString, ts: DateFormatting.nowISO8601(), name: client.name, version: client.version,
                pid: client.pid, capabilities: client.capabilities, replayEvents: client.replayEvents) {
                writeLine(hello)
            }

            var reader = LineFrameReader(fd: fd)
            while isRunning(), !Task.isCancelled {
                guard let lineData = try? reader.readLine() else { break }
                guard let frame = try? HostAgentProtocol.decode(line: lineData) else { continue }
                switch frame {
                case .ping(let id):
                    if let pong = try? HostAgentProtocol.pong(id: id, ts: DateFormatting.nowISO8601()) { writeLine(pong) }
                case .helloAck(let ack):
                    continuation.yield(.connected(ack))
                case .tokenResponse(let resp):
                    // Correlated reply — resume the waiter, never surface as a frame.
                    resolvePending(resp)
                default:
                    continuation.yield(.frame(frame))
                }
            }
            closeCurrentIf(fd)
            // Fail any in-flight token requests so awaiting callers don't hang
            // until their individual timeout fires.
            failAllPending(error: HostAgentClientError.disconnected)
            continuation.yield(.disconnected)
            try? await Task.sleep(for: .seconds(0.5))
        }
        continuation.finish()
    }

    private func isRunning() -> Bool { lock.lock(); defer { lock.unlock() }; return running }

    private func setCurrentFD(_ fd: Int32) { lock.lock(); currentFD = fd; lock.unlock() }

    private func closeCurrentIf(_ fd: Int32) {
        // Close UNDER the lock so a concurrent writeLine can't write to this
        // fd as (or after) it's closed and the number is reused.
        lock.lock()
        if currentFD == fd { currentFD = -1 }
        Darwin.close(fd)
        lock.unlock()
    }

    private func writeLine(_ data: Data) {
        var frame = data
        frame.append(0x0a)
        // Hold the lock across the whole write so the fd can't be closed +
        // reused mid-write (the frames are tiny control messages).
        lock.lock(); defer { lock.unlock() }
        guard currentFD >= 0 else { return }
        _ = writeAll(fd: currentFD, data: frame)
    }

    private func connectOnce() -> Int32? {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        if fd < 0 { return nil }
        guard var addr = makeUnixSocketAddress(path: socketPath) else { Darwin.close(fd); return nil }
        let rc = withUnsafePointer(to: &addr) { p in
            p.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                Darwin.connect(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if rc != 0 { Darwin.close(fd); return nil }
        return fd
    }
}

