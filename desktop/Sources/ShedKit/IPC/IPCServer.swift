// IPCServer.swift
//
// Newline-delimited JSON over a Unix-domain socket. Ported from roost's
// IPCServer.swift:
//   * One process-wide listener bound at the bundle profile's socketPath.
//   * Each accepted connection runs its own read loop on a detached task.
//   * Frames are read with a 16 MiB cap; responses are JSON + '\n'.
//   * Handler dispatch hops to @MainActor (in the impl) before touching UI.
//
// Darwin sockets directly rather than NWListener — NWListener on UDS is
// fragile around path-vs-endpoint shape and frame boundaries.

import Darwin
import Foundation

@MainActor
public final class IPCServer {
    private var listenFD: Int32 = -1
    private let socketPath: String
    private let handler: IPCHandler

    /// Bind a fresh server at `socketPath`. When `recoverStaleSocket` is
    /// true (caller holds the single-instance flock), an EADDRINUSE from a
    /// previous kill -9'd instance is recovered by unlinking the stale node
    /// after a connect() probe confirms nothing live is listening.
    public init(socketPath: String, handler: IPCHandler, recoverStaleSocket: Bool = false) throws {
        self.socketPath = socketPath
        self.handler = handler

        let parent = (socketPath as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(atPath: parent, withIntermediateDirectories: true)

        self.listenFD = try Self.bindWithRecovery(
            socketPath: socketPath, recoverStaleSocket: recoverStaleSocket)
    }

    private static func bindWithRecovery(socketPath: String, recoverStaleSocket: Bool) throws -> Int32 {
        switch try tryBindOnce(socketPath: socketPath) {
        case .ok(let fd):
            return fd
        case .addrInUse:
            if !recoverStaleSocket {
                throw IPCServerError.alreadyBound(path: socketPath)
            }
            if Self.connectProbe(socketPath: socketPath) {
                throw IPCServerError.alreadyBound(path: socketPath)
            }
            try? FileManager.default.removeItem(atPath: socketPath)
            switch try tryBindOnce(socketPath: socketPath) {
            case .ok(let fd): return fd
            case .addrInUse: throw IPCServerError.alreadyBound(path: socketPath)
            }
        }
    }

    private enum BindOutcome {
        case ok(Int32)
        case addrInUse
    }

    private static func tryBindOnce(socketPath: String) throws -> BindOutcome {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        if fd < 0 { throw IPCServerError.socketCreate(errno: errno) }

        guard var addr = makeUnixSocketAddress(path: socketPath) else {
            Darwin.close(fd)
            throw IPCServerError.pathTooLong(socketPath)
        }

        let bindResult = withUnsafePointer(to: &addr) { addrPtr in
            addrPtr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                bind(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if bindResult < 0 {
            let e = errno
            Darwin.close(fd)
            if e == EADDRINUSE { return .addrInUse }
            throw IPCServerError.bind(path: socketPath, errno: e)
        }

        if listen(fd, 32) < 0 {
            let e = errno
            Darwin.close(fd)
            throw IPCServerError.listen(errno: e)
        }

        chmod(socketPath, 0o600)
        return .ok(fd)
    }

    private static func connectProbe(socketPath: String) -> Bool {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        if fd < 0 { return false }
        defer { Darwin.close(fd) }

        guard var addr = makeUnixSocketAddress(path: socketPath) else { return false }
        let rc = withUnsafePointer(to: &addr) { addrPtr in
            addrPtr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                Darwin.connect(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        return rc == 0
    }

    deinit {
        if listenFD >= 0 { Darwin.close(listenFD) }
        try? FileManager.default.removeItem(atPath: socketPath)
    }

    /// Begin accepting connections on a detached task so the accept loop
    /// never blocks the main actor.
    public nonisolated func start() {
        let fdTask = Task { @MainActor in self.listenFD }
        let handlerTask = Task { @MainActor in self.handler }
        Task.detached {
            let listenFD = await fdTask.value
            let handler = await handlerTask.value
            IPCServer.acceptLoop(listenFD: listenFD, handler: handler)
        }
    }

    private nonisolated static func acceptLoop(listenFD: Int32, handler: IPCHandler) {
        while listenFD >= 0 {
            let conn = accept(listenFD, nil, nil)
            if conn < 0 {
                if errno == EINTR { continue }
                NSLog("ipc: accept failed: \(errno)")
                return
            }
            Task.detached {
                await IPCServer.serveConnection(fd: conn, handler: handler)
            }
        }
    }

    private nonisolated static func serveConnection(fd: Int32, handler: IPCHandler) async {
        defer { Darwin.close(fd) }
        var reader = LineFrameReader(fd: fd)
        while true {
            do {
                guard let line = try reader.readLine() else { return }
                let response = await IPCServer.dispatch(line: line, handler: handler)
                let body = try JSONEncoder().encode(response) + Data([0x0a])
                if !writeAll(fd: fd, data: body) { return }
            } catch {
                NSLog("ipc: connection error: \(error)")
                return
            }
        }
    }

    private nonisolated static func dispatch(line: Data, handler: IPCHandler) async -> IPCResponse {
        let request: IPCRequest
        do {
            request = try JSONDecoder().decode(IPCRequest.self, from: line)
        } catch {
            return IPCResponse.failure(id: 0, code: "parse-error", message: "envelope decode failed: \(error)")
        }
        do {
            let result = try await handler.handle(op: request.op, params: request.params)
            return IPCResponse.success(id: request.id, result: result)
        } catch let err as IPCHandlerError {
            return IPCResponse.failure(id: request.id, code: err.code, message: err.message)
        } catch {
            return IPCResponse.failure(id: request.id, code: "internal", message: "\(error)")
        }
    }
}

// MARK: - Handler

public protocol IPCHandler: Sendable {
    func handle(op: String, params: AnyCodable?) async throws -> AnyCodable?
}

public struct IPCHandlerError: Error, CustomStringConvertible {
    public let code: String
    public let message: String

    public init(code: String, message: String) {
        self.code = code
        self.message = message
    }

    public var description: String { "\(code): \(message)" }

    public static func unknownOp(_ op: String) -> IPCHandlerError {
        IPCHandlerError(code: "unknown-op", message: "no such op: \(op)")
    }
    public static func invalidParam(_ message: String) -> IPCHandlerError {
        IPCHandlerError(code: "invalid-param", message: message)
    }
    public static func notFound(_ message: String) -> IPCHandlerError {
        IPCHandlerError(code: "not-found", message: message)
    }
    public static func internalError(_ message: String) -> IPCHandlerError {
        IPCHandlerError(code: "internal", message: message)
    }
    public static func notEnabled(_ message: String) -> IPCHandlerError {
        IPCHandlerError(code: "not-enabled", message: message)
    }
}

// MARK: - Errors

public enum IPCServerError: Error, CustomStringConvertible {
    case socketCreate(errno: Int32)
    case pathTooLong(String)
    case bind(path: String, errno: Int32)
    case alreadyBound(path: String)
    case listen(errno: Int32)
    case read(errno: Int32)
    case frameTooLarge

    public var description: String {
        switch self {
        case .socketCreate(let e): return "socket() failed: \(strerrorString(e))"
        case .pathTooLong(let p): return "socket path too long: \(p)"
        case .bind(let p, let e): return "bind(\(p)) failed: \(strerrorString(e))"
        case .alreadyBound(let p): return "socket already in use: \(p)"
        case .listen(let e): return "listen() failed: \(strerrorString(e))"
        case .read(let e): return "read() failed: \(strerrorString(e))"
        case .frameTooLarge: return "frame larger than \(ipcMaxFrameBytes) bytes"
        }
    }
}

private func strerrorString(_ code: Int32) -> String {
    if let c = strerror(code), let s = String(validatingCString: c) { return s }
    return "errno \(code)"
}
