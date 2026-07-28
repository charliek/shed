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
    case unexpectedReply
    /// The connection that advertised `credential.get` is gone (or has not
    /// announced itself yet) — transient, retried by the next request.
    case capabilityLost
    /// The connection that is live right now does not answer `credential.get`.
    case capabilityUnsupported

    public var description: String {
        switch self {
        case .notConnected: return "host agent not connected"
        case .timedOut: return "timed out waiting for host agent reply"
        case .disconnected: return "host agent connection dropped"
        case .encodingFailed: return "failed to encode host agent request"
        case .unexpectedReply: return "host agent answered with the wrong reply type"
        case .capabilityLost: return "host agent connection changed before the credential request was sent"
        case .capabilityUnsupported: return "host agent does not answer credential.get"
        }
    }
}

/// The capability learned on ONE connection, tagged with that connection's
/// generation. Handing both to a caller is what lets a request that decided to
/// use `credential.get` re-check, at send time, that it is still talking to the
/// connection that advertised it — a reconnect between the decision and the
/// write must not smuggle the old answer onto the new socket.
public struct AgentCapabilitySnapshot: Sendable, Equatable {
    public let state: AgentCapabilityState
    public let generation: UInt64
}

/// A correlated reply to an app→agent request. One pending table serves both
/// request types; the requester asserts the arm it asked for, so an agent that
/// answers a `credential.get` with a `token.response` fails closed instead of
/// being silently reinterpreted.
enum HostAgentReply: Sendable {
    case token(TokenResponse)
    case credential(CredentialResponse)
}

public final class HostAgentClient: @unchecked Sendable {
    private let socketPath: String
    private let lock = NSLock()
    private var currentFD: Int32 = -1
    private var running = false
    private var loopTask: Task<Void, Never>?
    /// In-flight `token.get` / `credential.get` requests keyed by request id,
    /// each awaiting the correlated reply (matched by `in_reply_to`). Guarded by
    /// `lock`. removeValue is the single-resume guard — whoever removes a
    /// continuation owns its (exactly-once) resume.
    private var pending: [String: CheckedContinuation<HostAgentReply, Error>] = [:]
    /// What the CURRENT connection's `hello_ack` said about `credential.get`
    /// (plan 002 §7 P5). `.unknown` until the ack lands and again after every
    /// disconnect: a capability is a property of the agent on the other end of
    /// THIS connection, and a reconnect may land on a different build. Guarded
    /// by `lock` so the minter can read it without awaiting anything.
    private var capability: AgentCapabilityState = .unknown
    /// Bumped for every connection this client establishes. The capability above
    /// describes THIS generation and nothing else.
    private var connectionGeneration: UInt64 = 0
    /// Per-request timeout tasks keyed by the same id; cancelled when the request
    /// resolves (reply/disconnect/stop) so a resolved request leaves no lingering
    /// timer. Guarded by `lock`.
    private var pendingTimeouts: [String: Task<Void, Never>] = [:]
    /// TEST SEAM — nil in production, set before `start()` by one test.
    ///
    /// Invoked on the requesting thread immediately BEFORE a correlated
    /// request's frame is written, with `lock` HELD. That is the invariant it
    /// exists to prove: whatever the closure does while it runs, the connection
    /// the request just validated cannot be closed and replaced before the
    /// write, because every path that swaps `currentFD` needs this same lock.
    /// Keep it adjacent to the write — a version of this file that wrote after
    /// unlocking would have its bug window exactly here.
    var beforeWriteHookForTests: (@Sendable () -> Void)?

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
        // Wake a blocked readLine without closing here — closeCurrentIf owns the
        // single Darwin.close (avoids fd-reuse double-close).
        if currentFD >= 0 {
            _ = Darwin.shutdown(currentFD, SHUT_RDWR)
            currentFD = -1
        }
        capability = .unknown
        lock.unlock()
        task?.cancel()
        failAllPending(error: HostAgentClientError.disconnected)
    }

    public var isConnected: Bool {
        lock.lock(); defer { lock.unlock() }
        return currentFD >= 0
    }

    /// What this connection's `hello_ack` said about `credential.get`, with the
    /// connection generation it describes. A cached read of state the read loop
    /// maintains — NEVER I/O, because the Rust provider calls it (via
    /// `TokenMinter.supportsMtls`) while holding its own mint lock.
    public var credentialCapabilitySnapshot: AgentCapabilitySnapshot {
        lock.lock(); defer { lock.unlock() }
        return AgentCapabilitySnapshot(state: capability, generation: connectionGeneration)
    }

    /// The capability alone, for callers that only branch on it.
    public var credentialCapability: AgentCapabilityState { credentialCapabilitySnapshot.state }

    /// Await the current connection's capability, resolving as soon as a
    /// `hello_ack` lands. Returns a `.unknown` snapshot if `timeout` elapses
    /// first — the caller decides what an unresolved capability means for the
    /// request it is about to make; this never guesses on its behalf.
    ///
    /// Polled rather than continuation-based: the wait is bounded, sub-second in
    /// practice (the ack is the agent's first frame), and the alternative is a
    /// waiter list that must also be woken from every disconnect path.
    public func awaitCredentialCapability(timeout: Duration = .seconds(2)) async -> AgentCapabilitySnapshot {
        let deadline = ContinuousClock.now.advanced(by: timeout)
        while true {
            let snapshot = credentialCapabilitySnapshot
            if snapshot.state != .unknown { return snapshot }
            if ContinuousClock.now >= deadline || Task.isCancelled { return snapshot }
            try? await Task.sleep(for: .milliseconds(20))
        }
    }

    private func setCapability(_ state: AgentCapabilityState) {
        lock.lock(); capability = state; lock.unlock()
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
        guard case .token(let resp) = try await request(id: id, data: data, timeout: timeout) else {
            throw HostAgentClientError.unexpectedReply
        }
        return resp
    }

    /// Request a mode-agnostic CONTROL credential for `server` over the UDS,
    /// relaying `csrBase64` (the Rust core's PKCS#10 request) when it has one.
    ///
    /// `capability` is the snapshot the CALLER decided on. It is re-checked here
    /// under the lock, at send time, against the live connection: a reconnect (or
    /// a supersede) between the decision and the write invalidates it, and the
    /// request throws `.capabilityLost` / `.capabilityUnsupported` instead of
    /// putting a `credential.get` on a socket whose agent never advertised it.
    ///
    /// Like `requestToken`, a fail-closed reply (its `error` set) is RETURNED,
    /// not thrown — interpreting it is `validatedCredential(for:)`'s job.
    public func requestCredential(
        server: String, csrBase64: String?, capability: AgentCapabilitySnapshot,
        timeout: Duration = .seconds(10)
    ) async throws -> CredentialResponse {
        let id = UUID().uuidString
        let data = try HostAgentProtocol.credentialGet(id: id, server: server, csrBase64: csrBase64)
        let reply = try await request(id: id, data: data, timeout: timeout) { [self] in
            // Called with `lock` held — read the ivars directly, never re-enter.
            guard capability.state == .supported else { return .capabilityUnsupported }
            guard self.capability == .supported else {
                return self.capability == .unsupported ? .capabilityUnsupported : .capabilityLost
            }
            return connectionGeneration == capability.generation ? nil : .capabilityLost
        }
        guard case .credential(let resp) = reply else {
            throw HostAgentClientError.unexpectedReply
        }
        return resp
    }

    /// Send one correlated request and await its reply. Shared by `token.get`
    /// and `credential.get` so the registration/timeout/disconnect handling has
    /// exactly one implementation.
    ///
    /// The connected check, `precondition`, the registration AND the write all
    /// happen under ONE lock acquisition (the Rust client holds its state lock
    /// across the same span — `shed-app/src/host_agent.rs`). Unlocking between
    /// the check and the write would leave a window in which the validated
    /// connection can drop and be replaced: the frame would then land on a NEW,
    /// not-yet-acked connection whose agent may not support it — an old agent
    /// drops it, a new one performs an extra mint whose reply has no waiter.
    private func request(
        id: String, data: Data, timeout: Duration,
        precondition: (() -> HostAgentClientError?)? = nil
    ) async throws -> HostAgentReply {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<HostAgentReply, Error>) in
            lock.lock()
            guard currentFD >= 0 else {
                lock.unlock()
                cont.resume(throwing: HostAgentClientError.notConnected)
                return
            }
            if let failure = precondition?() {
                lock.unlock()
                cont.resume(throwing: failure)
                return
            }
            // Register before writing so a fast reply can't race ahead of
            // registration (the reader needs this same lock to resolve it).
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
            beforeWriteHookForTests?()
            guard writeLineLocked(data) else {
                // The write failed on the very fd we validated. Unregister and
                // fail now rather than leaving the caller to its timeout.
                pending.removeValue(forKey: id)
                pendingTimeouts.removeValue(forKey: id)?.cancel()
                lock.unlock()
                cont.resume(throwing: HostAgentClientError.notConnected)
                return
            }
            lock.unlock()
        }
    }

    /// Resume the request matching `inReplyTo`. A no-op if it already timed out
    /// or was failed by a disconnect (removeValue is the single-resume guard).
    private func resolvePending(inReplyTo: String, with reply: HostAgentReply) {
        lock.lock()
        let cont = pending.removeValue(forKey: inReplyTo)
        pendingTimeouts.removeValue(forKey: inReplyTo)?.cancel()
        lock.unlock()
        cont?.resume(returning: reply)
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
                    // Learn the capability BEFORE announcing the connection, so a
                    // consumer that reacts to .connected already sees it resolved.
                    // A REJECTION ("superseded": another app took the consumer
                    // slot) is not an old agent — it is a connection that will
                    // answer nothing. Back to `.unknown`, which the credential
                    // path treats as "not usable yet" rather than manufacturing
                    // an upgrade error out of a hand-off.
                    setCapability(ack.accepted ? AgentCapabilityState(helloAck: ack) : .unknown)
                    continuation.yield(.connected(ack))
                case .tokenResponse(let resp):
                    // Correlated reply — resume the waiter, never surface as a frame.
                    resolvePending(inReplyTo: resp.inReplyTo, with: .token(resp))
                case .credentialResponse(let resp):
                    resolvePending(inReplyTo: resp.inReplyTo, with: .credential(resp))
                default:
                    continuation.yield(.frame(frame))
                }
            }
            closeCurrentIf(fd)
            // The capability described THAT agent process; the next connection
            // re-learns it from its own hello_ack (plan 002 §7 P5 reconnect).
            setCapability(.unknown)
            // Fail any in-flight token requests so awaiting callers don't hang
            // until their individual timeout fires.
            failAllPending(error: HostAgentClientError.disconnected)
            continuation.yield(.disconnected)
            try? await Task.sleep(for: .seconds(0.5))
        }
        continuation.finish()
    }

    private func isRunning() -> Bool { lock.lock(); defer { lock.unlock() }; return running }

    /// Install a freshly connected fd and open a new capability generation: the
    /// new agent has said nothing yet, so nothing is known about it.
    private func setCurrentFD(_ fd: Int32) {
        lock.lock()
        currentFD = fd
        connectionGeneration &+= 1
        capability = .unknown
        lock.unlock()
    }

    private func closeCurrentIf(_ fd: Int32) {
        // Close UNDER the lock so a concurrent writeLine can't write to this
        // fd as (or after) it's closed and the number is reused.
        lock.lock()
        if currentFD == fd { currentFD = -1 }
        Darwin.close(fd)
        lock.unlock()
    }

    private func writeLine(_ data: Data) {
        // Hold the lock across the whole write so the fd can't be closed +
        // reused mid-write (the frames are tiny control messages).
        lock.lock(); defer { lock.unlock() }
        _ = writeLineLocked(data)
    }

    /// `writeLine`'s body, for callers that already hold `lock` (NSLock is not
    /// reentrant). Exists so a correlated request can validate the connection,
    /// register its waiter and write WITHOUT ever dropping the lock — see
    /// `request(id:data:timeout:precondition:)`.
    ///
    /// Returns whether the frame reached the socket; `false` for "not
    /// connected" as well as a failed write.
    private func writeLineLocked(_ data: Data) -> Bool {
        var frame = data
        frame.append(0x0a)
        guard currentFD >= 0 else { return false }
        return writeAll(fd: currentFD, data: frame)
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
        // Writing to a socket the agent has closed must return EPIPE, not raise
        // SIGPIPE. The app ignores the signal process-wide (`ShedBackend`), but
        // this fd is rolled by hand and outlives that assumption in any host
        // that doesn't (a test runner, an embedder) — so say it on the socket.
        var on: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        return fd
    }
}

