// IPCHandlerImpl.swift
//
// Bridges IPCServer's IPCHandler protocol to the running app. handle()
// runs on the handler's own actor; each op is a @MainActor method that
// reaches AppModel/AppState via the UiBridge and returns only Sendable
// results (the non-Sendable bridge never crosses the actor boundary).
// Mirrors roost's handler: strict params decoding (deny unknown fields)
// and an in-process screenshot.

import AppKit
import Foundation
import ShedKit

actor IPCHandlerImpl: IPCHandler {
    private let socketPath: String
    private let appLabel: String
    private let appID: String

    init(socketPath: String, appLabel: String, appID: String) {
        self.socketPath = socketPath
        self.appLabel = appLabel
        self.appID = appID
    }

    func handle(op: String, params: AnyCodable?) async throws -> AnyCodable? {
        switch op {
        case "identify":
            _ = try decodeParams(params, as: EmptyParams.self, expected: ["client_name", "client_version"])
            return try await encodeResult(identify())
        case "app.screenshot":
            return try await screenshot(params: params)
        case "app.window_metrics":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(windowMetricsOp())
        case "ui.window_state":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(windowStateOp())
        case "ui.state":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(uiStateOp())
        case "ui.navigate":
            let p = try decodeParams(params, as: PaneParams.self, expected: ["pane"])
            return try await uiNavigate(pane: p.pane)
        case "ui.set_ssh_approval":
            let p = try decodeParams(params, as: SetSshApprovalParams.self, expected: ["method", "policy", "ttl"])
            try await setSshApprovalOp(p)
            return emptyResult
        case "ui.show_window":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await showWindowOp()
            return emptyResult
        case "ui.hide_window":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await hideWindowOp()
            return emptyResult
        case "ui.show_create":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await showCreateSheetOp()
            return emptyResult
        case "ui.show_launch":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await showLaunchSheetOp()
            return emptyResult
        case "ui.open_preferences":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await openPreferencesOp()
            return emptyResult
        case "ui.open_menu":
            let p = try decodeParams(params, as: OpenMenuParams.self, expected: ["open"])
            try await openMenuOp(p.open)
            return AnyCodable(["open": p.open])
        case "host.list":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(hostListOp())
        case "sheds.list":
            let p = try decodeParams(params, as: HostFilterParams.self, expected: ["host"])
            return try await encodeResult(shedsList(host: p.host))
        case "sheds.refresh":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await shedsRefreshOp()
            return emptyResult
        case "system.df":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(systemDFOp())
        case "images.list":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(imagesListOp())
        case "shed.start", "shed.stop", "shed.reset", "shed.delete":
            guard let action = ShedAction(rawValue: String(op.dropFirst("shed.".count))) else {
                throw IPCHandlerError.unknownOp(op)
            }
            let p = try decodeParams(params, as: ShedActionParams.self, expected: ["host", "name"])
            try await shedActionOp(action, host: p.host, name: p.name)
            return emptyResult
        case "create.start":
            let p = try decodeParams(params, as: CreateStartParams.self,
                expected: ["host", "name", "repo", "local_dir", "image", "backend", "cpus", "memory_mb", "no_provision"])
            return try await createStartOp(p)
        case "create.status":
            let p = try decodeParams(params, as: CreateStatusParams.self, expected: ["create_id"])
            return try await encodeResult(createStatusOp(id: p.createID))
        case "create.cancel":
            let p = try decodeParams(params, as: CreateStatusParams.self, expected: ["create_id"])
            try await createCancelOp(id: p.createID)
            return emptyResult
        case "terminal.preview":
            let p = try decodeParams(params, as: TerminalParams.self, expected: ["host", "shed", "session"])
            return try await encodeResult(terminalPreviewOp(p))
        case "terminal.open":
            let p = try decodeParams(params, as: TerminalParams.self, expected: ["host", "shed", "session"])
            return try await encodeResult(terminalOpenOp(p))
        case "rc.classify":
            let p = try decodeParams(params, as: RcClassifyParams.self, expected: ["kind", "pane"])
            let cls = RemoteControl.classifyPane(kind: p.kind, pane: p.pane)
            return try encodeResult(RcClassifyResult(state: cls.state, url: cls.url))
        case "rc.list":
            let p = try decodeParams(params, as: RcListParams.self, expected: ["host", "shed"])
            return try await encodeResult(rcListOp(p))
        case "rc.launch":
            let p = try decodeParams(params, as: RcLaunchParams.self,
                expected: ["host", "shed", "kind", "display_name", "workdir", "initial_prompt"])
            return try await encodeResult(rcLaunchOp(p))
        case "rc.kill":
            let p = try decodeParams(params, as: RcKillParams.self, expected: ["host", "shed", "slug"])
            try await rcKillOp(p)
            return emptyResult
        case "rc.inject_test":
            let p = try decodeParams(params, as: RcInjectTestParams.self,
                expected: ["host", "shed", "slug", "kind", "state", "display_name", "workdir", "url",
                           "managed", "rc_id", "created_by", "created_at", "target_label"])
            try await rcInjectTestOp(p)
            return emptyResult
        case "approvals.list":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(approvalsListOp())
        case "approval.decide":
            let p = try decodeParams(params, as: ApprovalDecideParams.self, expected: ["id", "decision", "scope", "ttl", "persist"])
            try await approvalDecideOp(p)
            return emptyResult
        case "activity.list":
            let p = try decodeParams(params, as: ActivityListParams.self, expected: ["limit"])
            return try await encodeResult(activityListOp(limit: p.limit))
        case "activity.log_path":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(activityLogPathOp())
        case "policy.set":
            let p = try decodeParams(params, as: PolicySetParams.self, expected: ["rules"])
            try await policySetOp(p)
            return emptyResult
        case "policy.list":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(policyListOp())
        case "notifications.list":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            return try await encodeResult(notificationsListOp())
        case "notification.invoke":
            let p = try decodeParams(params, as: NotificationInvokeParams.self, expected: ["id", "action"])
            try await notificationInvokeOp(p)
            return emptyResult
        case "notification.open":
            _ = try decodeParams(params, as: EmptyParams.self, expected: [])
            try await notificationOpenOp()
            return emptyResult
        default:
            throw IPCHandlerError.unknownOp(op)
        }
    }

    // MARK: - @MainActor ops (the bridge is fetched + used here, never returned)

    @MainActor
    private func uiBridge() throws -> any UiBridge {
        guard let ui = ShedBackend.shared.ui else {
            throw IPCHandlerError.internalError("UI not registered")
        }
        return ui
    }

    @MainActor
    private func identify() -> IdentifyResult {
        IdentifyResult(
            socketPath: socketPath,
            pid: Int32(ProcessInfo.processInfo.processIdentifier),
            appLabel: appLabel,
            appID: appID,
            uiVersion: Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "0.0.0",
            protocolVersion: ipcProtocolVersion,
            testMode: ShedBackend.shared.testMode,
            mockBaseURL: ShedBackend.shared.mockBaseURL,
            core: ShedBackend.shared.rustCore ? "rust" : "swift")
    }

    @MainActor private func windowMetricsOp() throws -> WindowMetrics { try uiBridge().windowMetrics() }
    @MainActor private func windowStateOp() throws -> WindowState { try uiBridge().windowState() }
    @MainActor private func setSshApprovalOp(_ p: SetSshApprovalParams) throws {
        try uiBridge().setSshApproval(method: p.method, policy: p.policy, ttl: p.ttl)
    }
    @MainActor private func uiStateOp() throws -> UIState { try uiBridge().uiState() }
    @MainActor private func showWindowOp() throws { try uiBridge().showWindow() }
    @MainActor private func hideWindowOp() throws { try uiBridge().hideWindow() }
    @MainActor private func showCreateSheetOp() throws { try uiBridge().showCreateSheet() }
    @MainActor private func showLaunchSheetOp() throws { try uiBridge().showLaunchSheet() }
    @MainActor private func openPreferencesOp() throws { try uiBridge().openPreferences() }
    @MainActor private func openMenuOp(_ open: Bool) throws { try uiBridge().setMenuOpen(open) }
    @MainActor private func hostListOp() throws -> HostListResult { HostListResult(hosts: try uiBridge().uiState().hosts) }

    @MainActor private func shedsRefreshOp() async throws {
        try await uiBridge().refreshSheds()
    }

    @MainActor private func systemDFOp() async throws -> SystemUsageResult {
        SystemUsageResult(usage: try await uiBridge().refreshSystemUsage())
    }

    @MainActor private func imagesListOp() async throws -> ImagesListResult {
        ImagesListResult(images: try await uiBridge().refreshImages())
    }

    @MainActor private func shedActionOp(_ action: ShedAction, host: String?, name: String) async throws {
        try await uiBridge().shedAction(action, host: host, name: name)
    }

    @MainActor private func createStartOp(_ p: CreateStartParams) throws -> AnyCodable? {
        let id = try uiBridge().startCreate(host: p.host, request: p.request)
        return AnyCodable(["create_id": id])
    }

    @MainActor private func createStatusOp(id: String) throws -> CreateProgress {
        guard let status = try uiBridge().createStatus(id: id) else {
            throw IPCHandlerError.notFound("no create with id \(id)")
        }
        return status
    }

    @MainActor private func createCancelOp(id: String) throws {
        try uiBridge().cancelCreate(id: id)
    }

    @MainActor private func terminalPreviewOp(_ p: TerminalParams) throws -> TerminalPreviewResult {
        let r = try uiBridge().terminalLaunchPreview(shed: p.shed, host: p.host, session: p.session)
        // Keep `argv`/`command` at top level for backward compatibility; add the
        // active preset + resolved invocation for observability.
        return TerminalPreviewResult(
            argv: r.command.argv, command: r.command.command,
            preset: r.preset.rawValue, invocation: r.invocation)
    }

    @MainActor private func rcListOp(_ p: RcListParams) async throws -> RcListResult {
        RcListResult(sessions: try await uiBridge().rcList(host: p.host, shed: p.shed))
    }

    @MainActor private func rcLaunchOp(_ p: RcLaunchParams) async throws -> RcSession {
        try await uiBridge().rcLaunch(host: p.host, shed: p.shed, kind: p.kind, displayName: p.displayName, workdir: p.workdir, initialPrompt: p.initialPrompt)
    }

    @MainActor private func rcKillOp(_ p: RcKillParams) async throws {
        try await uiBridge().rcKill(host: p.host, shed: p.shed, slug: p.slug)
    }

    @MainActor private func rcInjectTestOp(_ p: RcInjectTestParams) throws {
        let managed = p.managed ?? false
        let session = RcSession(
            host: p.host ?? "", shed: p.shed, slug: p.slug,
            tmuxSession: RemoteControl.tmuxName(slug: p.slug),
            displayName: p.displayName ?? (managed ? p.slug : "\(p.shed)/\(p.slug)"),
            workdir: p.workdir ?? RemoteControl.defaultWorkdir,
            kind: p.kind ?? .default, state: p.state ?? .ready, url: p.url,
            rcID: p.rcID, createdBy: p.createdBy, createdAt: p.createdAt,
            targetLabel: p.targetLabel, managed: managed)
        try uiBridge().rcInjectTest(session)
    }

    @MainActor private func approvalsListOp() throws -> ApprovalListResult {
        ApprovalListResult(approvals: try uiBridge().approvalsList())
    }

    @MainActor private func approvalDecideOp(_ p: ApprovalDecideParams) async throws {
        try await uiBridge().decideApproval(id: p.id, choice: ApprovalChoice(decision: p.decision, scope: p.scope, ttl: p.ttl, persist: p.persist))
    }

    @MainActor private func activityListOp(limit: Int) throws -> ActivityListResult {
        ActivityListResult(entries: try uiBridge().activityList(limit: limit))
    }

    @MainActor private func activityLogPathOp() throws -> AuditLogPathResult {
        AuditLogPathResult(path: try uiBridge().auditLogPath())
    }

    @MainActor private func policySetOp(_ p: PolicySetParams) throws {
        guard ShedBackend.shared.testMode else {
            throw IPCHandlerError.notEnabled("policy.set requires SHED_DESKTOP_TEST_MODE=1")
        }
        try uiBridge().setPolicyRules(p.rules)
    }

    @MainActor private func policyListOp() throws -> PolicyListResult {
        PolicyListResult(rules: try uiBridge().policyRules())
    }

    @MainActor private func notificationsListOp() throws -> NotificationListResult {
        NotificationListResult(notifications: try uiBridge().postedNotifications())
    }

    @MainActor private func notificationInvokeOp(_ p: NotificationInvokeParams) throws {
        try uiBridge().invokeNotification(id: p.id, decision: p.action)
    }

    @MainActor private func notificationOpenOp() throws {
        try uiBridge().invokeNotificationOpen()
    }

    @MainActor private func terminalOpenOp(_ p: TerminalParams) throws -> TerminalCommand {
        // Never spawn a terminal under the test harness.
        guard !ShedBackend.shared.testMode else {
            throw IPCHandlerError.notEnabled("terminal.open is disabled in test mode (use terminal.preview)")
        }
        return try uiBridge().openTerminal(shed: p.shed, host: p.host, session: p.session)
    }

    @MainActor
    private func uiNavigate(pane: String) throws -> AnyCodable? {
        guard try uiBridge().navigate(toPane: pane) else {
            let want = DashboardPane.allCases.map(\.rawValue).joined(separator: "|")
            throw IPCHandlerError.invalidParam("unknown pane: \(pane) (want \(want))")
        }
        return AnyCodable(["pane": pane])
    }

    @MainActor
    private func shedsList(host: String?) throws -> ShedListResult {
        let sheds = try uiBridge().uiState().sheds
        let filtered = host.map { h in sheds.filter { $0.host == h } } ?? sheds
        return ShedListResult(sheds: filtered)
    }

    @MainActor
    private func screenshot(params: AnyCodable?) throws -> AnyCodable? {
        let p = try decodeParams(params, as: ScreenshotParams.self, expected: ["scale", "surface"])
        guard (1...2).contains(p.scale) else {
            throw IPCHandlerError.invalidParam("scale must be 1 or 2, got \(p.scale)")
        }
        guard let window = try uiBridge().window(for: p.surface) else {
            let hint = p.surface == .menu ? "menu is not open; call ui.open_menu {open:true} first" : "no window to capture"
            throw IPCHandlerError.internalError(hint)
        }
        do {
            let img = try captureWindowPNG(window, scale: p.scale)
            return AnyCodable([
                "png": img.png.base64EncodedString(),
                "width": img.width,
                "height": img.height,
                "scale": p.scale,
                "surface": p.surface.rawValue,
            ])
        } catch let e as ScreenshotError {
            throw IPCHandlerError.internalError("\(e)")
        }
    }

    private var emptyResult: AnyCodable? { AnyCodable([:] as [String: Any]) }
}

// MARK: - params + results

private struct EmptyParams: Decodable {}

private struct PaneParams: Decodable { let pane: String }

private struct OpenMenuParams: Decodable { let open: Bool }

private struct HostFilterParams: Decodable { let host: String? }

private struct ShedActionParams: Decodable {
    let host: String?
    let name: String
}

/// The create body fields (decoded straight into `CreateShedRequest`, the
/// single owner of that field list) plus the flat `host` selector.
private struct CreateStartParams: Decodable {
    let host: String?
    let request: CreateShedRequest

    init(from decoder: Decoder) throws {
        self.request = try CreateShedRequest(from: decoder)
        self.host = try decoder.container(keyedBy: HostKey.self).decodeIfPresent(String.self, forKey: .host)
    }
    private enum HostKey: String, CodingKey { case host }
}

private struct CreateStatusParams: Decodable {
    let createID: String
    enum CodingKeys: String, CodingKey { case createID = "create_id" }
}

private struct TerminalParams: Decodable {
    let host: String?
    let shed: String
    let session: String?
}

private struct TerminalPreviewResult: Encodable, Sendable {
    let argv: [String]
    let command: String
    let preset: String
    let invocation: LaunchInvocation
}

private struct RcClassifyParams: Decodable { let kind: RcKind; let pane: String }
private struct RcListParams: Decodable { let host: String?; let shed: String? }
private struct RcKillParams: Decodable { let host: String?; let shed: String; let slug: String }

private struct RcLaunchParams: Decodable {
    let host: String?
    let shed: String
    let kind: RcKind
    let displayName: String?
    let workdir: String?
    let initialPrompt: String?

    enum CodingKeys: String, CodingKey {
        case host, shed, kind, workdir
        case displayName = "display_name"
        case initialPrompt = "initial_prompt"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.host = try c.decodeIfPresent(String.self, forKey: .host)
        self.shed = try c.decode(String.self, forKey: .shed)
        self.kind = try c.decodeIfPresent(RcKind.self, forKey: .kind) ?? .default
        self.displayName = try c.decodeIfPresent(String.self, forKey: .displayName)
        self.workdir = try c.decodeIfPresent(String.self, forKey: .workdir)
        self.initialPrompt = try c.decodeIfPresent(String.self, forKey: .initialPrompt)
    }
}

private struct RcListResult: Encodable, Sendable { let sessions: [RcSession] }
private struct RcClassifyResult: Encodable, Sendable { let state: RcState; let url: String? }

/// Test-only: inject a session (managed or legacy) into the table for an e2e
/// screenshot. Only `shed` + `slug` are required; the rest default (applied in
/// `rcInjectTestOp`), so the synthesized decoder suffices.
private struct RcInjectTestParams: Decodable {
    let host: String?
    let shed: String
    let slug: String
    let kind: RcKind?
    let state: RcState?
    let displayName: String?
    let workdir: String?
    let url: String?
    let managed: Bool?
    let rcID: String?
    let createdBy: String?
    let createdAt: String?
    let targetLabel: String?

    enum CodingKeys: String, CodingKey {
        case host, shed, slug, kind, state, workdir, url, managed
        case displayName = "display_name"
        case rcID = "rc_id"
        case createdBy = "created_by"
        case createdAt = "created_at"
        case targetLabel = "target_label"
    }
}

private struct SetSshApprovalParams: Decodable {
    let method: ApprovalMethod?
    let policy: SSHApprovalPolicy?
    let ttl: String?
    enum CodingKeys: String, CodingKey { case method, policy, ttl }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.method = try c.decodeIfPresent(ApprovalMethod.self, forKey: .method)
        self.policy = try c.decodeIfPresent(SSHApprovalPolicy.self, forKey: .policy)
        self.ttl = try c.decodeIfPresent(String.self, forKey: .ttl)
    }
}

private struct ApprovalDecideParams: Decodable {
    let id: String
    let decision: ApprovalDecision
    let scope: ApprovalScope?
    let ttl: String?
    let persist: Bool
    enum CodingKeys: String, CodingKey { case id, decision, scope, ttl, persist }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.id = try c.decode(String.self, forKey: .id)
        self.decision = try c.decode(ApprovalDecision.self, forKey: .decision)
        self.scope = try c.decodeIfPresent(ApprovalScope.self, forKey: .scope)
        self.ttl = try c.decodeIfPresent(String.self, forKey: .ttl)
        self.persist = try c.decodeIfPresent(Bool.self, forKey: .persist) ?? false
    }
}

private struct ActivityListParams: Decodable {
    let limit: Int
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.limit = try c.decodeIfPresent(Int.self, forKey: .limit) ?? 200
    }
    enum CodingKeys: String, CodingKey { case limit }
}

private struct PolicySetParams: Decodable { let rules: [PolicyRule] }
private struct PolicyListResult: Encodable, Sendable { let rules: [PolicyRule] }

private struct NotificationInvokeParams: Decodable { let id: String; let action: ApprovalDecision }

private struct ApprovalListResult: Encodable, Sendable { let approvals: [PendingApprovalItem] }
private struct ActivityListResult: Encodable, Sendable { let entries: [AuditEntry] }
private struct AuditLogPathResult: Encodable, Sendable { let path: String }
private struct NotificationListResult: Encodable, Sendable { let notifications: [PostedNotification] }

private struct ScreenshotParams: Decodable {
    let scale: Int
    let surface: ScreenshotSurface
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.scale = try c.decodeIfPresent(Int.self, forKey: .scale) ?? 1
        self.surface = try c.decodeIfPresent(ScreenshotSurface.self, forKey: .surface) ?? .window
    }
    enum CodingKeys: String, CodingKey { case scale, surface }
}

private struct IdentifyResult: Encodable, Sendable {
    let socketPath: String
    let pid: Int32
    let appLabel: String
    let appID: String
    let uiVersion: String
    let protocolVersion: UInt32
    let testMode: Bool
    let mockBaseURL: String?
    let core: String
    enum CodingKeys: String, CodingKey {
        case socketPath = "socket_path"
        case pid
        case appLabel = "app_label"
        case appID = "app_id"
        case uiVersion = "ui_version"
        case protocolVersion = "protocol_version"
        case testMode = "test_mode"
        case mockBaseURL = "mock_base_url"
        case core
    }
}

private struct HostListResult: Encodable, Sendable { let hosts: [ShedHost] }
private struct ShedListResult: Encodable, Sendable { let sheds: [Shed] }
private struct SystemUsageResult: Encodable, Sendable { let usage: [HostDiskUsage] }
private struct ImagesListResult: Encodable, Sendable { let images: [HostImageList] }

// MARK: - decode/encode helpers (ported from roost)

private func decodeParams<T: Decodable>(
    _ params: AnyCodable?, as: T.Type, expected: Set<String>
) throws -> T {
    let raw = params?.value ?? [String: Any]()
    if let dict = raw as? [String: Any] {
        let extras = Set(dict.keys).subtracting(expected)
        if !extras.isEmpty {
            throw IPCHandlerError(code: "unknown-field",
                message: "unknown params: \(extras.sorted().joined(separator: ", "))")
        }
    }
    do {
        let data = try JSONSerialization.data(withJSONObject: raw, options: [.fragmentsAllowed])
        return try JSONDecoder().decode(T.self, from: data)
    } catch {
        throw IPCHandlerError.invalidParam("\(error)")
    }
}

private func encodeResult<T: Encodable>(_ value: T) throws -> AnyCodable? {
    let data = try JSONEncoder().encode(value)
    let any = try JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])
    return AnyCodable(any)
}
