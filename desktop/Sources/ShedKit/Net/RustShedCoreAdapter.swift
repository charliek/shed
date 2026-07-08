// RustShedCoreAdapter.swift
//
// Bridges the Rust `ShedCore` read client (shed-core, over UniFFI) to the app's
// Swift `Models`. Flag-gated by SHED_DESKTOP_RUST_CORE; when off, ShedServerClient
// keeps its existing URLSession path.
//
// `@unchecked Sendable` is sound: the wrapped `ShedCore` is Arc-backed and its
// reqwest client is Send+Sync, so concurrent calls across actors are safe (the
// same justification as `PinningSessionDelegate`).
//
// The Rust records (see shed-core-ffi) mirror the wire DTOs; this file maps them
// field-by-field to the Swift `Models` that double as IPC wire shapes. The M2
// golden-JSON parity gate guards the two representations against drift. The Rust
// types are qualified `ShedRustCore.*` because they share names with `Models`.

import Foundation
import ShedRustCore

/// A read client backed by the Rust `shed-core`, returning Swift `Models`.
final class RustShedCoreAdapter: @unchecked Sendable {
    private let core: ShedRustCore.ShedCore

    init(baseURL: String, serverName: String, token: String, pin: String?, hostAgent: HostAgentClient?)
        throws
    {
        // A secure server's control token is minted by the host agent; the Rust
        // FSM caches/refreshes around it. Open servers have no host agent → the
        // static `token` (if any) is used instead.
        let minter: (any ShedRustCore.TokenMinter)? = hostAgent.map { HostAgentTokenMinter(hostAgent: $0) }
        self.core = try ShedRustCore.ShedCore(
            baseUrl: baseURL, serverName: serverName, token: token, pin: pin, minter: minter)
    }

    func info() async throws -> ServerInfo { Self.map(try await core.info()) }
    func listSheds() async throws -> [Shed] { try await core.listSheds().map(Self.map) }
    func systemDF() async throws -> SystemDiskUsage { Self.map(try await core.systemDf()) }
    func listImages() async throws -> [ShedImage] { try await core.listImages().map(Self.map) }
    func egressProfiles() async throws -> [EgressProfileInfo] {
        try await core.egressProfiles().map(Self.map)
    }

    func start(name: String) async throws { try await core.start(name: name) }
    func stop(name: String) async throws { try await core.stop(name: name) }
    func reset(name: String) async throws { try await core.reset(name: name) }
    func delete(name: String) async throws { try await core.delete(name: name) }

    /// Bridges the Rust pull-based create (create_start + poll create_status) back
    /// to the AsyncThrowingStream ShedServerClient exposes, so AppModel's create
    /// flow is unchanged. onTermination cancels the Rust stream, since Task.cancel
    /// does not propagate over the FFI (M0 finding).
    func createShed(_ request: CreateShedRequest) -> AsyncThrowingStream<CreateEvent, Error> {
        // The Rust ShedCore isn't Sendable, but it's Arc-backed + thread-safe, so
        // box it to hand to the Task (bounded buffer mirrors the Swift path). The
        // request is mapped inside the Task: the Swift CreateShedRequest is
        // Sendable, but the generated FFI record is not.
        let boxed = UncheckedSendable(core)
        return AsyncThrowingStream(CreateEvent.self, bufferingPolicy: .bufferingNewest(256)) {
            continuation in
            let task = Task {
                let core = boxed.value
                let id = await core.createStart(request: Self.map(request))
                var yielded = 0
                while true {
                    if Task.isCancelled {
                        core.createCancel(id: id)
                        continuation.finish()
                        return
                    }
                    guard let progress = core.createStatus(id: id) else {
                        continuation.finish()
                        return
                    }
                    if progress.messages.count > yielded {
                        for msg in progress.messages[yielded...] {
                            continuation.yield(.progress(msg))
                        }
                        yielded = progress.messages.count
                    }
                    switch progress.state {
                    case .complete:
                        if let shed = progress.shed { continuation.yield(.complete(Self.map(shed))) }
                        continuation.finish()
                        return
                    case .error:
                        continuation.finish(
                            throwing: RustCreateError(message: progress.error ?? "create failed"))
                        return
                    case .progress:
                        try? await Task.sleep(for: .milliseconds(100))
                    }
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    // MARK: - Rust record -> Swift Model

    static func map(_ v: ShedRustCore.ServerInfo) -> ServerInfo {
        ServerInfo(
            name: v.name, version: v.version,
            sshPort: v.sshPort.map(Int.init), httpPort: v.httpPort.map(Int.init),
            backend: v.backend)
    }

    static func map(_ s: ShedRustCore.ShedStatus) -> ShedStatus {
        switch s {
        case .running: return .running
        case .stopped: return .stopped
        case .starting: return .starting
        case .error: return .error
        case .unknown: return .unknown
        @unknown default: return .unknown
        }
    }

    static func map(_ v: ShedRustCore.Shed) -> Shed {
        Shed(
            host: v.host, name: v.name, status: map(v.status),
            backend: v.backend, repo: v.repo, image: v.image,
            imageDigest: v.imageDigest, localDir: v.localDir, ipAddress: v.ipAddress,
            cpus: v.cpus.map(Int.init), memoryMB: v.memoryMb.map(Int.init),
            createdAt: v.createdAt, startedAt: v.startedAt,
            activeNamespaces: v.activeNamespaces)
    }

    static func map(_ v: ShedRustCore.ShedImage) -> ShedImage {
        ShedImage(
            name: v.name, dockerRef: v.dockerRef, alias: v.alias,
            isDefault: v.isDefault, cached: v.cached, inUse: v.inUse,
            digest: v.digest, source: v.source, sizeBytes: v.sizeBytes)
    }

    static func map(_ v: ShedRustCore.DiskSize) -> DiskSize {
        DiskSize(logicalBytes: v.logicalBytes, physicalBytes: v.physicalBytes)
    }

    static func map(_ v: ShedRustCore.DiskEntry) -> DiskEntry {
        DiskEntry(name: v.name, dockerRef: v.dockerRef, size: map(v.size))
    }

    static func map(_ v: ShedRustCore.DiskTotals) -> DiskTotals {
        DiskTotals(
            images: map(v.images), sheds: map(v.sheds), snapshots: map(v.snapshots),
            orphans: map(v.orphans), all: map(v.all))
    }

    static func map(_ v: ShedRustCore.SystemDiskUsage) -> SystemDiskUsage {
        SystemDiskUsage(
            serverName: v.serverName, backend: v.backend,
            images: v.images.map(map), sheds: v.sheds.map(map),
            orphans: v.orphans.map(map), totals: map(v.totals))
    }

    static func map(_ v: ShedRustCore.EgressProfile) -> EgressProfile {
        EgressProfile(mode: v.mode, allow: v.allow, deny: v.deny, rule: v.rule)
    }

    static func map(_ v: ShedRustCore.EgressProfileInfo) -> EgressProfileInfo {
        EgressProfileInfo(name: v.name, source: v.source, profile: map(v.profile))
    }

    static func map(_ v: CreateShedRequest) -> ShedRustCore.CreateShedRequest {
        ShedRustCore.CreateShedRequest(
            name: v.name, repo: v.repo, localDir: v.localDir, image: v.image,
            backend: v.backend, cpus: v.cpus.map(Int64.init), memoryMb: v.memoryMB.map(Int64.init),
            noProvision: v.noProvision)
    }
}

/// A create error whose description IS the Rust message, so AppModel's
/// `progress.error = "\(error)"` renders exactly what the Rust path stored
/// (e.g. "create failed: ...") without double-wrapping it.
private struct RustCreateError: Error, CustomStringConvertible {
    let message: String
    var description: String { message }
}

/// Boxes a value the caller asserts is safe to send across concurrency domains
/// (here: the Arc-backed, thread-safe Rust `ShedCore`).
private struct UncheckedSendable<T>: @unchecked Sendable {
    let value: T
    init(_ value: T) { self.value = value }
}
