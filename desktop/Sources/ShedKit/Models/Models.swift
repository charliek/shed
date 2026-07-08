// Models.swift
//
// App-side domain types. These double as the IPC wire shapes (snake_case
// CodingKeys) so `ui.state` / `sheds.list` / `host.list` results need no
// separate DTOs — the handler encodes these directly and shedctl/pytest
// read them back.

import Foundation

/// A shed's lifecycle status. `unknown` absorbs any value the server adds
/// later so decoding never fails on an unrecognized status.
public enum ShedStatus: String, Codable, Sendable, Equatable {
    case running
    case stopped
    case starting
    case error
    case unknown

    public init(serverValue: String) {
        self = ShedStatus(rawValue: serverValue) ?? .unknown
    }
}

/// A configured shed-server host (one entry in ~/.shed/config.yaml),
/// annotated with reachability + the info probe result.
public struct ShedHost: Codable, Sendable, Equatable {
    public var name: String
    public var host: String
    public var httpPort: Int
    public var sshPort: Int
    public var reachable: Bool
    public var backend: String?
    public var version: String?
    /// Why the host is unreachable (sanitized for display), or nil when reachable.
    public var lastError: String?

    enum CodingKeys: String, CodingKey {
        case name, host
        case httpPort = "http_port"
        case sshPort = "ssh_port"
        case reachable, backend, version
        case lastError = "last_error"
    }

    public init(
        name: String, host: String, httpPort: Int, sshPort: Int,
        reachable: Bool = false, backend: String? = nil, version: String? = nil,
        lastError: String? = nil
    ) {
        self.name = name
        self.host = host
        self.httpPort = httpPort
        self.sshPort = sshPort
        self.reachable = reachable
        self.backend = backend
        self.version = version
        self.lastError = lastError
    }
}

extension ShedHost {
    /// Apply a reachability-probe result, returning the updated host. Pure +
    /// testable: reachable + metadata on success; a sanitized error (tokens
    /// scrubbed so nothing secret reaches the UI) on failure; `lastError`
    /// cleared on success.
    public func applyingProbe(info: ServerInfo?, error: String?) -> ShedHost {
        var h = self
        h.reachable = info != nil
        h.backend = info?.backend
        h.version = info?.version
        h.lastError = info != nil ? nil : ShedHost.sanitizeProbeError(error)
        return h
    }

    static func sanitizeProbeError(_ raw: String?) -> String? {
        guard let raw, !raw.isEmpty else { return nil }
        return DiagnosticLog.redact(raw)
    }
}

/// A shed, annotated with which host (config server name) it came from.
public struct Shed: Codable, Sendable, Equatable, Identifiable {
    public var host: String
    public var name: String
    public var status: ShedStatus
    public var backend: String?
    public var repo: String?
    public var image: String?
    public var imageDigest: String?
    public var localDir: String?
    public var ipAddress: String?
    public var cpus: Int?
    public var memoryMB: Int?
    public var createdAt: String?
    public var startedAt: String?
    public var activeNamespaces: [String]

    public var id: String { "\(host)/\(name)" }

    enum CodingKeys: String, CodingKey {
        case host, name, status, backend, repo, image
        case imageDigest = "image_digest"
        case localDir = "local_dir"
        case ipAddress = "ip_address"
        case cpus
        case memoryMB = "memory_mb"
        case createdAt = "created_at"
        case startedAt = "started_at"
        case activeNamespaces = "active_namespaces"
    }

    public init(
        host: String, name: String, status: ShedStatus,
        backend: String? = nil, repo: String? = nil, image: String? = nil,
        imageDigest: String? = nil,
        localDir: String? = nil, ipAddress: String? = nil,
        cpus: Int? = nil, memoryMB: Int? = nil,
        createdAt: String? = nil, startedAt: String? = nil,
        activeNamespaces: [String] = []
    ) {
        self.host = host
        self.name = name
        self.status = status
        self.backend = backend
        self.repo = repo
        self.image = image
        self.imageDigest = imageDigest
        self.localDir = localDir
        self.ipAddress = ipAddress
        self.cpus = cpus
        self.memoryMB = memoryMB
        self.createdAt = createdAt
        self.startedAt = startedAt
        self.activeNamespaces = activeNamespaces
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        // `host` is absent in shed-server JSON (the client stamps it after
        // decode); present when this same type is read back over IPC. One
        // decoder serves both paths.
        self.host = try c.decodeIfPresent(String.self, forKey: .host) ?? ""
        self.name = try c.decode(String.self, forKey: .name)
        // Lenient status: a string we don't know maps to `.unknown`.
        let statusRaw = try c.decodeIfPresent(String.self, forKey: .status) ?? "unknown"
        self.status = ShedStatus(serverValue: statusRaw)
        self.backend = try c.decodeIfPresent(String.self, forKey: .backend)
        self.repo = try c.decodeIfPresent(String.self, forKey: .repo)
        self.image = try c.decodeIfPresent(String.self, forKey: .image)
        self.imageDigest = try c.decodeIfPresent(String.self, forKey: .imageDigest)
        self.localDir = try c.decodeIfPresent(String.self, forKey: .localDir)
        self.ipAddress = try c.decodeIfPresent(String.self, forKey: .ipAddress)
        self.cpus = try c.decodeIfPresent(Int.self, forKey: .cpus)
        self.memoryMB = try c.decodeIfPresent(Int.self, forKey: .memoryMB)
        self.createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt)
        self.startedAt = try c.decodeIfPresent(String.self, forKey: .startedAt)
        self.activeNamespaces = try c.decodeIfPresent([String].self, forKey: .activeNamespaces) ?? []
    }

    /// The manifest digest in shed's short `sha256:<12hex>` form, or nil.
    /// Mirrors `vmimage.ShortDigest`: only the canonical `sha256:`-prefixed
    /// form is shortened; anything else is returned verbatim rather than given
    /// a synthetic prefix.
    public var shortImageDigest: String? {
        guard let d = imageDigest, !d.isEmpty else { return nil }
        guard d.hasPrefix("sha256:") else { return d }
        let hex = d.dropFirst("sha256:".count)
        guard hex.count >= 12 else { return d }
        return "sha256:" + hex.prefix(12)
    }

    /// The image a shed runs, rendered as `<label> (sha256:short)` — mirrors
    /// shed's `formatShedImage` / `shed list -vv`. The label is the ref/alias
    /// it was created from; the short digest pins the exact manifest. nil when
    /// neither a label nor a digest is known (e.g. a not-yet-provisioned shed).
    public var imageDisplay: String? {
        let label = (image?.isEmpty == false) ? image : nil
        guard let short = shortImageDigest else { return label }
        guard let label else { return short }
        return "\(label) (\(short))"
    }

    /// The image label for the dashboard: the matching image's `repo:tag` ref,
    /// resolved by digest against the host's image list (shed-server returns only
    /// a digest on a shed, not the name). Falls back to `imageDisplay` (any label
    /// and/or the short digest) when there's no match.
    public func imageLabel(in images: [ShedImage]) -> String? {
        if let d = imageDigest, !d.isEmpty,
           let match = images.first(where: { $0.digest == d }) {
            return match.shortRef
        }
        return imageDisplay
    }
}

/// Body for `POST /api/sheds`. `repo` and `localDir` are mutually
/// exclusive; only non-nil fields are sent.
public struct CreateShedRequest: Codable, Sendable, Equatable {
    public var name: String
    public var repo: String?
    public var localDir: String?
    public var image: String?
    public var backend: String?
    public var cpus: Int?
    public var memoryMB: Int?
    public var noProvision: Bool?

    enum CodingKeys: String, CodingKey {
        case name, repo, image, backend, cpus
        case localDir = "local_dir"
        case memoryMB = "memory_mb"
        case noProvision = "no_provision"
    }

    public init(
        name: String, repo: String? = nil, localDir: String? = nil,
        image: String? = nil, backend: String? = nil,
        cpus: Int? = nil, memoryMB: Int? = nil, noProvision: Bool? = nil
    ) {
        self.name = name
        self.repo = repo
        self.localDir = localDir
        self.image = image
        self.backend = backend
        self.cpus = cpus
        self.memoryMB = memoryMB
        self.noProvision = noProvision
    }
}

/// A remote-control session running in a shed (a detached tmux session
/// named `rc-<slug>`).
public struct RcSession: Codable, Sendable, Equatable, Identifiable {
    public var host: String
    public var shed: String
    public var slug: String
    public var tmuxSession: String
    public var displayName: String
    public var workdir: String
    public var kind: RcKind
    public var state: RcState
    public var url: String?
    // RC Session Convention v1 metadata. `rcID` is the SHED_RC_ID (a UUID) and
    // is encoded under `rc_id` — NOT `id`, which stays the computed Identifiable
    // key below (`host/shed/slug`). All optional + absent on legacy/unmanaged
    // sessions; `managed` says whether a valid SHED_RC_V was present.
    public var rcID: String?
    public var createdBy: String?
    public var createdAt: String?
    public var targetLabel: String?
    public var managed: Bool

    public var id: String { Self.compositeID(host: host, shed: shed, slug: slug) }

    /// The table/wire identity — `host/shed/slug`. The single source of truth so
    /// a kill that has only those three parts keys exactly the same entry.
    public static func compositeID(host: String, shed: String, slug: String) -> String {
        "\(host)/\(shed)/\(slug)"
    }

    enum CodingKeys: String, CodingKey {
        case host, shed, slug
        case tmuxSession = "tmux_session"
        case displayName = "display_name"
        case workdir, kind, state, url
        case rcID = "rc_id"
        case createdBy = "created_by"
        case createdAt = "created_at"
        case targetLabel = "target_label"
        case managed
    }

    public init(
        host: String, shed: String, slug: String, tmuxSession: String,
        displayName: String, workdir: String, kind: RcKind, state: RcState, url: String? = nil,
        rcID: String? = nil, createdBy: String? = nil, createdAt: String? = nil,
        targetLabel: String? = nil, managed: Bool = false
    ) {
        self.host = host
        self.shed = shed
        self.slug = slug
        self.tmuxSession = tmuxSession
        self.displayName = displayName
        self.workdir = workdir
        self.kind = kind
        self.state = state
        self.url = url
        self.rcID = rcID
        self.createdBy = createdBy
        self.createdAt = createdAt
        self.targetLabel = targetLabel
        self.managed = managed
    }

    // Defensive decode (the repo convention): the v1 metadata + `managed` are
    // absent on older wire payloads, so default them rather than fail.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        host = try c.decode(String.self, forKey: .host)
        shed = try c.decode(String.self, forKey: .shed)
        slug = try c.decode(String.self, forKey: .slug)
        tmuxSession = try c.decode(String.self, forKey: .tmuxSession)
        displayName = try c.decode(String.self, forKey: .displayName)
        workdir = try c.decode(String.self, forKey: .workdir)
        kind = try c.decode(RcKind.self, forKey: .kind)
        state = try c.decode(RcState.self, forKey: .state)
        url = try c.decodeIfPresent(String.self, forKey: .url)
        rcID = try c.decodeIfPresent(String.self, forKey: .rcID)
        createdBy = try c.decodeIfPresent(String.self, forKey: .createdBy)
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt)
        targetLabel = try c.decodeIfPresent(String.self, forKey: .targetLabel)
        managed = try c.decodeIfPresent(Bool.self, forKey: .managed) ?? false
    }
}

/// `GET /api/info` response.
public struct ServerInfo: Codable, Sendable, Equatable {
    public var name: String
    public var version: String
    public var sshPort: Int?
    public var httpPort: Int?
    public var backend: String?

    enum CodingKeys: String, CodingKey {
        case name, version
        case sshPort = "ssh_port"
        case httpPort = "http_port"
        case backend
    }

    public init(name: String, version: String, sshPort: Int? = nil, httpPort: Int? = nil, backend: String? = nil) {
        self.name = name
        self.version = version
        self.sshPort = sshPort
        self.httpPort = httpPort
        self.backend = backend
    }
}

/// A snapshot of the app's view-model for the `ui.state` IPC op — the
/// drivability backbone the harness reads to assert without screenshots.
public struct UIState: Codable, Sendable, Equatable {
    public var pane: String
    public var hosts: [ShedHost]
    public var sheds: [Shed]
    public var hostAgentConnected: Bool
    public var lastError: String?

    enum CodingKeys: String, CodingKey {
        case pane, hosts, sheds
        case hostAgentConnected = "host_agent_connected"
        case lastError = "last_error"
    }

    public init(pane: String, hosts: [ShedHost], sheds: [Shed], hostAgentConnected: Bool = false, lastError: String? = nil) {
        self.pane = pane
        self.hosts = hosts
        self.sheds = sheds
        self.hostAgentConnected = hostAgentConnected
        self.lastError = lastError
    }
}

/// Logical (point) measurements of the running window, for the
/// `app.window_metrics` op.
public struct WindowMetrics: Codable, Sendable, Equatable {
    public var windowWidth: Double
    public var windowHeight: Double
    public var sidebarWidth: Double
    public var visiblePane: String

    enum CodingKeys: String, CodingKey {
        case windowWidth = "window_width"
        case windowHeight = "window_height"
        case sidebarWidth = "sidebar_width"
        case visiblePane = "visible_pane"
    }

    public init(windowWidth: Double, windowHeight: Double, sidebarWidth: Double, visiblePane: String) {
        self.windowWidth = windowWidth
        self.windowHeight = windowHeight
        self.sidebarWidth = sidebarWidth
        self.visiblePane = visiblePane
    }
}

/// Whether the dashboard window is on screen and the app's current
/// activation policy, for the `ui.window_state` op. Lets the launch/reopen
/// behavior (issue #4) be asserted over IPC: an active user launch shows the
/// window and goes `.regular`; a quiet login launch stays hidden/`.accessory`.
public struct WindowState: Codable, Sendable, Equatable {
    public var visible: Bool
    /// `"regular"` (Dock icon) or `"accessory"` (menu-bar only).
    public var activationPolicy: String

    enum CodingKeys: String, CodingKey {
        case visible
        case activationPolicy = "activation_policy"
    }

    public init(visible: Bool, activationPolicy: String) {
        self.visible = visible
        self.activationPolicy = activationPolicy
    }
}

/// One egress profile fragment (`allow`/`deny` domain globs, an optional CEL
/// `rule`, `mode: audit`). Mirrors shed-server's `config.EgressProfile` wire
/// shape (all keys lowercase single words → no CodingKeys needed).
public struct EgressProfile: Codable, Sendable, Equatable {
    public var mode: String?
    public var allow: [String]?
    public var deny: [String]?
    public var rule: String?
    public init(mode: String? = nil, allow: [String]? = nil, deny: [String]? = nil, rule: String? = nil) {
        self.mode = mode
        self.allow = allow
        self.deny = deny
        self.rule = rule
    }
}

/// One entry of `GET /api/egress/profiles`: a named profile plus whether it
/// comes from the server config (read-only baseline) or the runtime user store.
public struct EgressProfileInfo: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var source: String // "config" | "user"
    public var profile: EgressProfile
    public var id: String { name }
    public init(name: String, source: String, profile: EgressProfile) {
        self.name = name
        self.source = source
        self.profile = profile
    }
}

/// A server's egress profiles (or the per-host fetch error), for the desktop's
/// fan-out across hosts. Not a wire type.
public struct HostEgressProfiles: Sendable, Equatable, Identifiable {
    public var host: String
    public var profiles: [EgressProfileInfo]
    public var error: String?
    public var id: String { host }
    public init(host: String, profiles: [EgressProfileInfo], error: String? = nil) {
        self.host = host
        self.profiles = profiles
        self.error = error
    }
}

/// The sidebar panes the dashboard exposes; also the `ui.navigate` target
/// vocabulary.
public enum DashboardPane: String, Codable, Sendable, CaseIterable {
    case sheds
    case approvals
    case agents
    case activity
    case egress
    case system
}

// MARK: - System disk usage (M7: GET /api/system/df)

/// A logical/physical byte pair. Decodes defensively (missing → 0) since the
/// backend omits zero sizes in some shapes.
public struct DiskSize: Codable, Sendable, Equatable {
    public var logicalBytes: Int64
    public var physicalBytes: Int64
    enum CodingKeys: String, CodingKey { case logicalBytes = "logical_bytes"; case physicalBytes = "physical_bytes" }
    public init(logicalBytes: Int64 = 0, physicalBytes: Int64 = 0) {
        self.logicalBytes = logicalBytes
        self.physicalBytes = physicalBytes
    }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        logicalBytes = try c.decodeIfPresent(Int64.self, forKey: .logicalBytes) ?? 0
        physicalBytes = try c.decodeIfPresent(Int64.self, forKey: .physicalBytes) ?? 0
    }
    public static let zero = DiskSize()
}

/// One image/shed/orphan disk entry.
public struct DiskEntry: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var dockerRef: String?
    public var size: DiskSize
    public var id: String { name }
    enum CodingKeys: String, CodingKey { case name; case dockerRef = "docker_ref"; case size }
    public init(name: String, dockerRef: String? = nil, size: DiskSize = .zero) {
        self.name = name
        self.dockerRef = dockerRef
        self.size = size
    }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? "?"
        dockerRef = try c.decodeIfPresent(String.self, forKey: .dockerRef)
        size = try c.decodeIfPresent(DiskSize.self, forKey: .size) ?? .zero
    }
}

public struct DiskTotals: Codable, Sendable, Equatable {
    public var images: DiskSize
    public var sheds: DiskSize
    public var snapshots: DiskSize
    public var orphans: DiskSize
    public var all: DiskSize
    public init(images: DiskSize = .zero, sheds: DiskSize = .zero, snapshots: DiskSize = .zero, orphans: DiskSize = .zero, all: DiskSize = .zero) {
        self.images = images; self.sheds = sheds; self.snapshots = snapshots; self.orphans = orphans; self.all = all
    }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        func d(_ k: CodingKeys) throws -> DiskSize { try c.decodeIfPresent(DiskSize.self, forKey: k) ?? .zero }
        images = try d(.images); sheds = try d(.sheds); snapshots = try d(.snapshots); orphans = try d(.orphans); all = try d(.all)
    }
    enum CodingKeys: String, CodingKey { case images, sheds, snapshots, orphans, all }
}

/// `GET /api/system/df` — server disk usage. Arrays default to [] (the real
/// empty shape uses `null`/omitted), totals to zero.
public struct SystemDiskUsage: Codable, Sendable, Equatable {
    public var serverName: String?
    public var backend: String?
    public var images: [DiskEntry]
    public var sheds: [DiskEntry]
    public var orphans: [DiskEntry]
    public var totals: DiskTotals
    enum CodingKeys: String, CodingKey {
        case serverName = "server_name"
        case backend, images, sheds, orphans, totals
    }
    public init(serverName: String? = nil, backend: String? = nil, images: [DiskEntry] = [], sheds: [DiskEntry] = [], orphans: [DiskEntry] = [], totals: DiskTotals = DiskTotals()) {
        self.serverName = serverName; self.backend = backend
        self.images = images; self.sheds = sheds; self.orphans = orphans; self.totals = totals
    }
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        serverName = try c.decodeIfPresent(String.self, forKey: .serverName)
        backend = try c.decodeIfPresent(String.self, forKey: .backend)
        images = try c.decodeIfPresent([DiskEntry].self, forKey: .images) ?? []
        sheds = try c.decodeIfPresent([DiskEntry].self, forKey: .sheds) ?? []
        orphans = try c.decodeIfPresent([DiskEntry].self, forKey: .orphans) ?? []
        totals = try c.decodeIfPresent(DiskTotals.self, forKey: .totals) ?? DiskTotals()
    }
}

/// One host's disk usage, for the System pane / `system.df`.
public struct HostDiskUsage: Codable, Sendable, Equatable, Identifiable {
    public var host: String
    public var usage: SystemDiskUsage?
    public var error: String?
    public var id: String { host }
    public init(host: String, usage: SystemDiskUsage? = nil, error: String? = nil) {
        self.host = host
        self.usage = usage
        self.error = error
    }
}

// MARK: - Images (GET /api/images)

/// One installed image from `GET /api/images` (shed-server's `ImageInfo`).
/// Decodes leniently — the picker needs only name/alias/default/cached, and
/// pre-v0.6.1 servers omit `alias`/`is_default`.
public struct ShedImage: Codable, Sendable, Equatable, Identifiable {
    public var name: String
    public var dockerRef: String?
    public var alias: String?
    public var isDefault: Bool
    public var cached: Bool
    public var inUse: Bool
    public var digest: String?
    public var source: String?
    public var sizeBytes: Int64

    public var id: String { digest ?? dockerRef ?? name }

    /// The ref minus its registry/namespace, for compact display, e.g.
    /// "ghcr.io/charliek/shed-vz-full:v0.6.2" → "shed-vz-full:v0.6.2".
    public var shortRef: String {
        let ref = (dockerRef?.isEmpty == false ? dockerRef : nil) ?? name
        return ref.split(separator: "/").last.map(String.init) ?? ref
    }

    enum CodingKeys: String, CodingKey {
        case name
        case dockerRef = "docker_ref"
        case alias
        case isDefault = "is_default"
        case cached
        case inUse = "in_use"
        case digest
        case source
        case sizeBytes = "size_bytes"
    }

    public init(
        name: String, dockerRef: String? = nil, alias: String? = nil,
        isDefault: Bool = false, cached: Bool = false, inUse: Bool = false,
        digest: String? = nil, source: String? = nil, sizeBytes: Int64 = 0
    ) {
        self.name = name
        self.dockerRef = dockerRef
        self.alias = alias
        self.isDefault = isDefault
        self.cached = cached
        self.inUse = inUse
        self.digest = digest
        self.source = source
        self.sizeBytes = sizeBytes
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? "?"
        dockerRef = try c.decodeIfPresent(String.self, forKey: .dockerRef)
        alias = try c.decodeIfPresent(String.self, forKey: .alias)
        isDefault = try c.decodeIfPresent(Bool.self, forKey: .isDefault) ?? false
        cached = try c.decodeIfPresent(Bool.self, forKey: .cached) ?? false
        inUse = try c.decodeIfPresent(Bool.self, forKey: .inUse) ?? false
        digest = try c.decodeIfPresent(String.self, forKey: .digest)
        source = try c.decodeIfPresent(String.self, forKey: .source)
        sizeBytes = try c.decodeIfPresent(Int64.self, forKey: .sizeBytes) ?? 0
    }
}

/// One host's image list for the `images.list` op (mirrors `HostDiskUsage`).
public struct HostImageList: Codable, Sendable, Equatable, Identifiable {
    public var host: String
    public var images: [ShedImage]?
    public var error: String?
    public var id: String { host }
    public init(host: String, images: [ShedImage]? = nil, error: String? = nil) {
        self.host = host
        self.images = images
        self.error = error
    }
}

/// A capturable surface for `app.screenshot`. Decoding the param as this
/// enum gives window|menu validation for free; adding a surface later is
/// one case + one arm in the app's `window(for:)`.
public enum ScreenshotSurface: String, Codable, Sendable {
    case window
    case menu
    case preferences
}

/// A shed lifecycle mutation (`shed.start`/`stop`/`reset`/`delete`).
public enum ShedAction: String, Codable, Sendable {
    case start, stop, reset, delete
}

/// The lifecycle of an in-flight create. Encodes to the same wire strings
/// (`progress`/`complete`/`error`) the harness asserts on.
public enum CreateState: String, Codable, Sendable {
    case progress, complete, error
}

/// Progress of an in-flight `create.start`, polled via `create.status`.
public struct CreateProgress: Codable, Sendable, Equatable {
    public var id: String
    public var state: CreateState
    public var messages: [String]
    public var shed: Shed?
    public var error: String?

    public init(id: String, state: CreateState, messages: [String] = [], shed: Shed? = nil, error: String? = nil) {
        self.id = id
        self.state = state
        self.messages = messages
        self.shed = shed
        self.error = error
    }
}
