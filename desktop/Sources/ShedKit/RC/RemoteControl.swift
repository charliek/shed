// RemoteControl.swift
//
// Guest-binary client for the RC Session Convention v2. The SSH+tmux choreography
// (bootstrap, classification, SHED_RC_* metadata, trust pre-seed, prompt delivery)
// now lives in the `shed-ext-rc` guest binary; shed-desktop invokes it over SSH and
// decodes the neutral JSON DTO. The normative spec lives in shed-remote-agent's
// docs/reference/rc-session-convention.md.

import Foundation

/// RC session kind (Convention v2). `<tool>-<mode>` so the model can grow to other
/// agents later; `shell` is tool-agnostic. v1's `agent`/`repl` were renamed.
///
/// The `.other` case is the **unknown-kind policy**: an unrecognized wire value is
/// PRESERVED verbatim (not coerced to claude-broker), so a session created by a
/// newer/other tool decodes and renders neutrally — its raw kind string is shown,
/// and no claude-specific affordance is attached. Mirrors `shed_core::rc::RcKind`
/// and the guest's `rc.Kind`.
public enum RcKind: Codable, Sendable, Hashable {
    case claudeRc
    case claudeBroker
    case codex
    case opencode
    case cursor
    case shell
    case other(String)

    public static let `default`: RcKind = .claudeRc

    /// The kebab-case wire value (an unknown kind's preserved raw string).
    public var rawValue: String {
        switch self {
        case .claudeRc: return "claude-rc"
        case .claudeBroker: return "claude-broker"
        case .codex: return "codex"
        case .opencode: return "opencode"
        case .cursor: return "cursor"
        case .shell: return "shell"
        case .other(let s): return s
        }
    }

    /// Parse a wire kind string, preserving an unrecognized value as `.other`.
    public init(wire: String) {
        switch wire {
        case "claude-rc": self = .claudeRc
        case "claude-broker": self = .claudeBroker
        case "codex": self = .codex
        case "opencode": self = .opencode
        case "cursor": self = .cursor
        case "shell": self = .shell
        default: self = .other(wire)
        }
    }

    public init(from decoder: Decoder) throws {
        self.init(wire: try decoder.singleValueContainer().decode(String.self))
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        try c.encode(rawValue)
    }

    /// A recognized kind (not the preserved-raw unknown case).
    public var isKnown: Bool {
        if case .other = self { return false }
        return true
    }

    /// Whether this kind accepts a typed kickoff line — a prompt for the agent
    /// REPLs/TUIs, a command for `shell`. Mirrors the guest's `AcceptsTypedInput`:
    /// every registered kind except `claude-broker` (input is a URL, not the pane);
    /// an unknown `.other` kind is NOT promptable (no affordances).
    public var acceptsTypedInput: Bool {
        switch self {
        case .claudeBroker, .other: return false
        case .claudeRc, .codex, .opencode, .cursor, .shell: return true
        }
    }

    /// The tool token this kind's agent maps to under `capabilities.agents`, or
    /// `nil` for a kind with no installable agent (`shell`) or an unknown kind.
    public var tool: String? {
        switch self {
        case .claudeRc, .claudeBroker: return "claude"
        case .codex: return "codex"
        case .opencode: return "opencode"
        case .cursor: return "cursor"
        case .shell, .other: return nil
        }
    }

    /// The per-agent login remediation for this kind's needs-auth state, mirroring
    /// the guest's `AuthHintFor` (`internal/ext/rc/agents.go`).
    public var authHint: String {
        switch self {
        case .claudeRc, .claudeBroker: return "run `claude` \u{2192} /login"
        case .codex: return "run `codex` and complete login (`codex login`)"
        case .opencode: return "run `opencode auth login`"
        case .cursor: return "run `cursor-agent login`"
        case .shell, .other: return "log in to the agent in a terminal"
        }
    }

    /// Kinds the New session sheet can offer for creation (`claude-broker` is
    /// URL-driven, `.other` never creatable). Capability gating narrows this per
    /// shed. `rc.launch` over IPC still accepts any decodable known kind so a
    /// session created elsewhere round-trips and displays; this only seeds the toggle.
    public static let creatable: [RcKind] = [.claudeRc, .codex, .opencode, .cursor, .shell]
}

public enum RcState: String, Codable, Sendable, Equatable {
    case starting
    case ready
    case reconnecting
    case needsTrust = "needs-trust"
    case needsAuth = "needs-auth"
    case dead
}

/// A pane-derived (state, url). The live RC path takes state/url from the binary's
/// DTO; this type backs the pure `rc.classify` IPC utility.
public struct RcClassification: Sendable, Equatable {
    public let state: RcState
    public let url: String?
    public init(state: RcState, url: String? = nil) {
        self.state = state
        self.url = url
    }
}

/// The machine-readable state of an approval request (contract v2), carried by an
/// `approval_request` feed row and — once a lane produces approvals — by a
/// session's `pendingApprovals` snapshot. Mirrors the guest's `rc.FeedApproval`
/// and `shed_core::rc::RcFeedApproval`. Every field decodes tolerantly (a
/// wrong-typed value degrades to the zero value rather than failing the row).
public struct RcFeedApproval: Codable, Sendable, Equatable {
    /// The lane-assigned approval id — the address the approval verb resolves
    /// (`POST /v1/sessions/{slug}/approvals/{id}`).
    public let id: String
    /// `"pending"` or `"resolved"`.
    public let status: String
    /// The decision that resolved it (absent while pending).
    public let decision: String?
    /// The decisions this request accepts, advertised per request so a client
    /// renders exactly the buttons the lane will honor. Empty when the producer
    /// advertised none.
    public let decisions: [String]

    enum CodingKeys: String, CodingKey {
        case id, status, decision, decisions
    }

    public init(id: String, status: String, decision: String? = nil, decisions: [String] = []) {
        self.id = id
        self.status = status
        self.decision = decision
        self.decisions = decisions
    }

    /// One `decisions[]` element, decoded lossily: a non-string element degrades
    /// to `nil` (dropped) instead of failing the array.
    private struct LossyDecision: Decodable {
        let value: String?
        init(from decoder: Decoder) throws {
            value = try? decoder.singleValueContainer().decode(String.self)
        }
    }

    public init(from decoder: Decoder) throws {
        // `try?` per field, not `try`: a wrong-TYPED field must degrade to its
        // default like the Rust mirror's tolerant deserializer — a plain
        // decodeIfPresent still throws on type mismatch, and one malformed
        // pending_approvals[] element would otherwise fail the WHOLE session/list
        // decode instead of degrading one approval to an un-actionable row.
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = (try? c.decodeIfPresent(String.self, forKey: .id)) ?? nil ?? ""
        status = (try? c.decodeIfPresent(String.self, forKey: .status)) ?? nil ?? ""
        decision = (try? c.decodeIfPresent(String.self, forKey: .decision)) ?? nil
        // Degrades PER ELEMENT like the Rust mirror's filter_map
        // (`crates/shed-core/src/rc.rs`): `["allow",5,"deny"]` → `["allow","deny"]`,
        // not `[]`. A wholly wrong-typed `decisions` (not an array) still degrades
        // to `[]` via the outer `try?`.
        let raw = (try? c.decodeIfPresent([LossyDecision].self, forKey: .decisions)) ?? nil
        decisions = (raw ?? []).compactMap(\.value)
    }

    /// Whether this row reports an approval still awaiting an answer.
    public var isPending: Bool { status == "pending" }
}

/// The neutral, target-agnostic session shape printed by `shed-ext-rc` (it runs
/// inside the shed and can't know the host alias / shed name — the app injects those
/// and maps `id`→`rcID`). Optional fields are absent (not null) when unknown.
public struct RcSessionDTO: Codable, Sendable, Equatable {
    public let slug: String
    public let tmuxSession: String
    public let kind: RcKind
    public let state: RcState
    public let managed: Bool
    /// The session's CURRENT lane (contract v2): `"tui"` or `"structured"`.
    /// Absent on a pre-v2 binary's payload — read through `laneOrTui`, which
    /// applies the contract's absent-⇒-`"tui"` rule.
    public let lane: String?
    public let displayName: String?
    public let workdir: String?
    public let url: String?
    public let id: String?
    public let createdBy: String?
    public let createdAt: String?
    public let targetLabel: String?
    /// Live-activity dimension (additive; derived by the rc hub). Absent when no
    /// hub is running, the kind is unsupported, or a blocking lifecycle state
    /// suppressed it.
    public let activity: String?
    /// RFC3339 timestamp the activity was last derived/changed; absent with `activity`.
    public let activityAt: String?
    /// A short preview of the session's most recent message. Absent when the hub has none.
    public let lastMessage: String?
    /// The session's currently-unresolved approval requests (contract v2) — absent
    /// on every wire today (no producer in this phase).
    public let pendingApprovals: [RcFeedApproval]?

    enum CodingKeys: String, CodingKey {
        case slug
        case tmuxSession = "tmux_session"
        case kind, state, managed, lane
        case displayName = "display_name"
        case workdir, url, id
        case createdBy = "created_by"
        case createdAt = "created_at"
        case targetLabel = "target_label"
        case activity
        case activityAt = "activity_at"
        case lastMessage = "last_message"
        case pendingApprovals = "pending_approvals"
    }

    public init(
        slug: String, tmuxSession: String, kind: RcKind, state: RcState, managed: Bool,
        lane: String? = nil, displayName: String? = nil, workdir: String? = nil,
        url: String? = nil, id: String? = nil, createdBy: String? = nil,
        createdAt: String? = nil, targetLabel: String? = nil, activity: String? = nil,
        activityAt: String? = nil, lastMessage: String? = nil,
        pendingApprovals: [RcFeedApproval]? = nil
    ) {
        self.slug = slug
        self.tmuxSession = tmuxSession
        self.kind = kind
        self.state = state
        self.managed = managed
        self.lane = lane
        self.displayName = displayName
        self.workdir = workdir
        self.url = url
        self.id = id
        self.createdBy = createdBy
        self.createdAt = createdAt
        self.targetLabel = targetLabel
        self.activity = activity
        self.activityAt = activityAt
        self.lastMessage = lastMessage
        self.pendingApprovals = pendingApprovals
    }

    // Tolerant decode: `lane`, the activity trio, and `pending_approvals` are all
    // additive (contract v2); an older binary's payload omitting them decodes fine.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        slug = try c.decode(String.self, forKey: .slug)
        tmuxSession = try c.decode(String.self, forKey: .tmuxSession)
        kind = try c.decode(RcKind.self, forKey: .kind)
        state = try c.decode(RcState.self, forKey: .state)
        managed = try c.decode(Bool.self, forKey: .managed)
        lane = try c.decodeIfPresent(String.self, forKey: .lane)
        displayName = try c.decodeIfPresent(String.self, forKey: .displayName)
        workdir = try c.decodeIfPresent(String.self, forKey: .workdir)
        url = try c.decodeIfPresent(String.self, forKey: .url)
        id = try c.decodeIfPresent(String.self, forKey: .id)
        createdBy = try c.decodeIfPresent(String.self, forKey: .createdBy)
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt)
        targetLabel = try c.decodeIfPresent(String.self, forKey: .targetLabel)
        activity = try c.decodeIfPresent(String.self, forKey: .activity)
        activityAt = try c.decodeIfPresent(String.self, forKey: .activityAt)
        lastMessage = try c.decodeIfPresent(String.self, forKey: .lastMessage)
        pendingApprovals = try c.decodeIfPresent([RcFeedApproval].self, forKey: .pendingApprovals)
    }

    /// The session's lane with the contract's old-payload rule applied: absent or
    /// blank reads as `"tui"`.
    public var laneOrTui: String {
        RemoteControl.laneOrTui(lane)
    }
}

/// The `shed-ext-rc list` response shape. `capabilities` is tolerant of absence —
/// an OLD baked-in binary's bare `{"rc_sessions":[…]}` envelope decodes to nil.
public struct RcSessionListDTO: Codable, Sendable {
    public let rcSessions: [RcSessionDTO]
    public let capabilities: RcCapabilities?
    enum CodingKeys: String, CodingKey {
        case rcSessions = "rc_sessions"
        case capabilities
    }
}

/// One agent's install-probe result under `capabilities.agents`. `version` is
/// absent when the agent is not installed. Mirrors the guest's `rc.AgentInfo`.
public struct RcAgentInfo: Codable, Sendable, Equatable {
    public let installed: Bool
    public let version: String?
}

/// Per-kind UI hints from `capabilities.kind_features`. Mirrors `rc.KindFeatures` /
/// `shed_core::rc::RcKindFeatures`.
///
/// `watch` and `input` are additive hub hints (codex-only in this phase; absent
/// decodes to `false`/`""`). Contract v2 adds three more, all additive so an
/// old (v3 or earlier) payload omitting them decodes with safe defaults rather
/// than failing: `feed` is what the hub can stream for the kind (`"messages"`,
/// `"activity"`, or `"none"`), `interrupt` reports the `turn/interrupt` verb
/// (`false` for every kind in this phase), and `attach` is how a terminal
/// reaches the session (`"tmux"`, `"native-remote"`, or `"none"`). `watch` is
/// DEPRECATED by `feed` — the guest holds `watch == (feed == "messages")` in
/// lockstep until every client reads `feed`; read them through
/// `feedMessages`, which prefers `feed` and falls back to `watch` on a payload
/// that predates it.
public struct RcKindFeatures: Codable, Sendable, Equatable {
    public let postInput: Bool
    public let approvals: String
    public let watch: Bool
    public let input: String
    /// Empty on a pre-v2 payload — read through `feedMessages` rather than
    /// comparing this directly.
    public let feed: String
    public let interrupt: Bool
    /// Empty on a pre-v2 payload — read through `attachKind`, which applies
    /// the `"tmux"` fallback.
    public let attach: String

    enum CodingKeys: String, CodingKey {
        case postInput = "post_input"
        case approvals, watch, input, feed, interrupt, attach
    }

    public init(
        postInput: Bool, approvals: String, watch: Bool = false, input: String = "",
        feed: String = "", interrupt: Bool = false, attach: String = ""
    ) {
        self.postInput = postInput
        self.approvals = approvals
        self.watch = watch
        self.input = input
        self.feed = feed
        self.interrupt = interrupt
        self.attach = attach
    }

    // Tolerant decode: `watch`/`input`/`feed`/`interrupt`/`attach` are all additive
    // (contract v2 or later); an older payload omitting them decodes with the safe
    // defaults above instead of failing.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        postInput = try c.decode(Bool.self, forKey: .postInput)
        approvals = try c.decode(String.self, forKey: .approvals)
        watch = try c.decodeIfPresent(Bool.self, forKey: .watch) ?? false
        input = try c.decodeIfPresent(String.self, forKey: .input) ?? ""
        feed = try c.decodeIfPresent(String.self, forKey: .feed) ?? ""
        interrupt = try c.decodeIfPresent(Bool.self, forKey: .interrupt) ?? false
        attach = try c.decodeIfPresent(String.self, forKey: .attach) ?? ""
    }

    /// Whether feed input is gated (`input == "gated"`) — a watch view's input
    /// bar is only ever enabled for a gated kind waiting for input.
    public var inputGated: Bool { input == "gated" }

    /// Whether the hub streams a normalized message feed for this kind — the
    /// v3-fallback read of the deprecated `watch` bit: `feed == "messages"`, or,
    /// when `feed` is absent/blank, the legacy `watch` flag. Mirrors
    /// `shed_core::rc::RcKindFeatures::feed_messages`.
    public var feedMessages: Bool {
        let trimmed = feed.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed == "messages" || (trimmed.isEmpty && watch)
    }

    /// How a terminal reaches this kind's sessions, with the contract's
    /// absent-⇒-`"tmux"` fallback applied. Mirrors `RcKindFeatures::attach_kind`.
    public var attachKind: String {
        let trimmed = attach.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? "tmux" : trimmed
    }
}

/// A shed's RC capabilities — `shed-ext-rc capabilities`, also embedded in the
/// `list` envelope. Tells the client which kinds a shed offers, which agents are
/// installed (and at what version), the feature set, and per-kind hints. Mirrors
/// `shed_core::rc::RcCapabilities` / the guest's `rc.Capabilities`. Optional maps/
/// lists default to empty so a partial payload still decodes.
public struct RcCapabilities: Codable, Sendable, Equatable {
    public let rcVersion: Int
    public let kinds: [RcKind]
    public let agents: [String: RcAgentInfo]
    public let features: [String]
    public let kindFeatures: [String: RcKindFeatures]

    enum CodingKeys: String, CodingKey {
        case rcVersion = "rc_version"
        case kinds, agents, features
        case kindFeatures = "kind_features"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        rcVersion = try c.decode(Int.self, forKey: .rcVersion)
        kinds = try c.decodeIfPresent([RcKind].self, forKey: .kinds) ?? []
        agents = try c.decodeIfPresent([String: RcAgentInfo].self, forKey: .agents) ?? [:]
        features = try c.decodeIfPresent([String].self, forKey: .features) ?? []
        kindFeatures = try c.decodeIfPresent([String: RcKindFeatures].self, forKey: .kindFeatures) ?? [:]
    }

    /// Whether the launch UI should OFFER `kind` for creation: it's advertised in
    /// `kinds` AND its backing agent (if any) is installed. `shell` (no agent) is
    /// offered whenever advertised. Mirrors `RcCapabilities::offers`.
    public func offers(_ kind: RcKind) -> Bool {
        guard kinds.contains(kind) else { return false }
        guard let tool = kind.tool else { return true }
        return agents[tool]?.installed ?? false
    }

    /// The creatable kinds this shed offers, in canonical create-form order.
    public var creatableKinds: [RcKind] {
        RcKind.creatable.filter { offers($0) }
    }

    /// Whether `feature` is advertised (feature discovery, replacing error-sniffing).
    public func hasFeature(_ feature: String) -> Bool { features.contains(feature) }
}

/// A binary-domain outcome distinguished from an SSH transport failure by the
/// exit code (the orchestrator maps SSH auth/unreachable; these are the binary's).
public enum RcError: Error, CustomStringConvertible, Equatable {
    case slugTaken(String)
    case notFound(String)
    case badRequest(String)
    case missingBinary
    case failed(String)

    public var description: String {
        switch self {
        case .slugTaken(let s): return "rc session already exists: \(s)"
        case .notFound(let s): return "rc session not found: \(s)"
        case .badRequest(let s): return "invalid rc request: \(s)"
        case .missingBinary: return "shed-ext-rc is not installed on this shed — update the shed image"
        case .failed(let s): return "rc operation failed: \(s)"
        }
    }
}

public enum RemoteControl {
    public static let tmuxPrefix = "rc-"
    /// Fallback workdir for a legacy/unmanaged session whose DTO omits one (the
    /// binary resolves $SHED_WORKSPACE for managed sessions).
    public static let defaultWorkdir = "/workspace"
    /// Stable tool id for SHED_RC_CREATED_BY (`<tool>/<version>`; no `/`).
    public static let toolName = "shed-desktop"
    /// Convention schema version the binary writes.
    public static let schemaVersion = 2
    /// The default session lane (contract v2) — an rc-tmux pane. Every kind in
    /// this phase is `tui`, and an old (pre-v2) payload that omits `lane`
    /// entirely is read as this value (`laneOrTui`).
    public static let laneTui = "tui"

    // Confusable-free alphabet (no i, l, o, 0, 1) — matches the convention.
    static let slugAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

    /// The contract's old-payload lane rule, shared by the DTO and the enriched
    /// session: an absent (pre-v2 binary) or blank `lane` reads as `laneTui`, so
    /// a client never has to distinguish "absent" from `"tui"`. Mirrors
    /// `shed_core::rc::lane_or_tui`.
    public static func laneOrTui(_ lane: String?) -> String {
        let trimmed = (lane ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? laneTui : trimmed
    }

    public static func tmuxName(slug: String) -> String { "\(tmuxPrefix)\(slug)" }

    /// Generate a 6-char confusable-free slug (the app picks the slug so it can
    /// build its `<shed>/<slug>` display name; the binary accepts a caller slug).
    public static func generateSlug(length: Int = 6) -> String {
        let alpha = Array(slugAlphabet)
        return String((0..<length).map { _ in alpha.randomElement()! })
    }

    // MARK: - shed-ext-rc invocation

    /// Binary name (or path). Defaults to `shed-ext-rc` (on PATH in the shed `full`
    /// image); overridable via SHED_EXT_RC_BIN for dev/proof (scp'd to e.g. /tmp).
    public static func binaryName() -> String {
        ProcessInfo.processInfo.environment["SHED_EXT_RC_BIN"] ?? "shed-ext-rc"
    }

    /// argv for `shed-ext-rc create --wait` (the binary resolves the workdir,
    /// pre-seeds trust, polls to ready, accepts trust, and delivers a stdin prompt).
    public static func createArgv(
        kind: RcKind, name: String, slug: String, workdir: String?,
        createdBy: String, target: String, hasPrompt: Bool
    ) -> [String] {
        var a = [
            binaryName(), "create",
            "--kind", kind.rawValue,
            "--name", name,
            "--slug", slug,
            "--created-by", createdBy,
            "--target", target,
            "--wait",
        ]
        if let w = workdir, !w.isEmpty { a += ["--workdir", w] }
        if hasPrompt { a += ["--prompt-stdin"] }
        return a
    }

    /// True when `s` carries no control characters. A superset-strict guard over
    /// the guest's `HasControlChars` (which rejects only `<= 0x1f` and `0x7f`):
    /// this also rejects a few Unicode format chars, which is safe — the client
    /// stays stricter than the guest, never sending a value the guest would reject.
    public static func isSafeRCValue(_ s: String) -> Bool {
        !s.unicodeScalars.contains { CharacterSet.controlCharacters.contains($0) }
    }

    /// Normalize + validate a caller-supplied kickoff line. Leading/trailing
    /// whitespace (incl. newlines) is trimmed; an empty/blank value returns `nil`
    /// so the caller omits `--prompt-stdin` rather than feeding the guest an empty
    /// stdin (a guest hard-error). After trimming, throws `RcError.badRequest` for
    /// an embedded control character, an over-long value (>2000 UTF-8 bytes), or a
    /// prompt on a kind that doesn't accept typed input. Mirrors shed-remote-agent's
    /// create-request normalization: trim first, then validate the trimmed value
    /// (so a surrounding newline normalizes away rather than being rejected).
    public static func normalizeRcPrompt(_ raw: String?, kind: RcKind) throws -> String? {
        guard let trimmed = raw?.trimmingCharacters(in: .whitespacesAndNewlines),
              !trimmed.isEmpty else { return nil }
        guard kind.acceptsTypedInput else {
            throw RcError.badRequest("kind \(kind.rawValue) does not accept an initial prompt")
        }
        guard isSafeRCValue(trimmed) else {
            throw RcError.badRequest("initial prompt must not contain control characters")
        }
        // Orchestrator-layer cap (the guest enforces none): matches shed-remote-agent's
        // 2000-char create limit and bounds what gets typed into the pane. Counted in
        // UTF-8 bytes — what actually crosses stdin.
        guard trimmed.utf8.count <= 2000 else {
            throw RcError.badRequest("initial prompt exceeds 2000 bytes")
        }
        return trimmed
    }

    /// Build the `create` argv and its stdin together, so the `--prompt-stdin`
    /// flag and the stdin payload can never disagree. `prompt` must already be
    /// normalized (see `normalizeRcPrompt`); it is dropped for a kind that doesn't
    /// accept typed input. The line is delivered verbatim (no trailing newline;
    /// `normalizeRcPrompt`/`isSafeRCValue` already forbid embedded newlines).
    public static func createInvocation(
        kind: RcKind, name: String, slug: String, workdir: String?,
        createdBy: String, target: String, prompt: String?
    ) -> (argv: [String], stdin: String?) {
        let effective = kind.acceptsTypedInput ? prompt : nil
        let argv = createArgv(
            kind: kind, name: name, slug: slug, workdir: workdir,
            createdBy: createdBy, target: target, hasPrompt: effective != nil)
        return (argv, effective)
    }

    public static func listArgv() -> [String] { [binaryName(), "list"] }
    public static func killArgv(slug: String) -> [String] { [binaryName(), "kill", "--slug", slug] }

    /// Map a non-zero exit code + stderr to an RcError. SSH-transport failures
    /// (the binary never ran) surface as `.failed` with the ssh stderr.
    public static func error(exitCode: Int32, stderr: String, stdout: String) -> RcError {
        let detail = (stderr.isEmpty ? stdout : stderr).trimmingCharacters(in: .whitespacesAndNewlines)
        switch exitCode {
        case 3: return .slugTaken(detail)
        case 4: return .notFound(detail)
        case 2: return .badRequest(detail)
        case 127: return .missingBinary
        default:
            if stderr.localizedCaseInsensitiveContains("command not found") { return .missingBinary }
            return .failed(detail.isEmpty ? "shed-ext-rc exited \(exitCode)" : detail)
        }
    }

    // MARK: - DTO → wire RcSession

    /// Adapt a binary DTO into the app's `RcSession`, injecting the host/shed the
    /// binary can't know and applying the `<shed>/<slug>` display fallback. `id`
    /// becomes `rcID` (the app's `id` is the computed `host/shed/slug`).
    public static func rcSession(fromDTO dto: RcSessionDTO, serverName: String, shed: String) -> RcSession {
        RcSession(
            host: serverName, shed: shed, slug: dto.slug,
            tmuxSession: dto.tmuxSession,
            displayName: dto.displayName ?? "\(shed)/\(dto.slug)",
            workdir: dto.workdir ?? defaultWorkdir,
            kind: dto.kind, state: dto.state, url: dto.url,
            rcID: dto.id, createdBy: dto.createdBy, createdAt: dto.createdAt,
            targetLabel: dto.targetLabel, managed: dto.managed,
            // Verbatim, absence included: the fallback lives in `laneOrTui`, so
            // "the producer said tui" stays distinguishable from "the producer
            // is too old to say".
            lane: dto.lane, activity: dto.activity, activityAt: dto.activityAt,
            lastMessage: cleanPreview(dto.lastMessage))
    }

    /// Sanitize a guest-controlled preview string the way the Rust core's
    /// `from_dto` does (`strip_format_chars` + trim + drop-empty): strip Unicode
    /// FORMAT characters (category Cf — bidi overrides like U+202E, zero-widths,
    /// BOM) that the hub's ANSI/C0C1 sanitizer deliberately leaves alone, trim
    /// whitespace, and degrade a value that was ONLY such characters to nil. The
    /// Swift path must not be laxer than the Rust/overview paths on text it
    /// renders into the UI.
    static func cleanPreview(_ s: String?) -> String? {
        guard let s else { return nil }
        let stripped = String(String.UnicodeScalarView(
            s.unicodeScalars.filter { $0.properties.generalCategory != .format }))
        let trimmed = stripped.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }

    /// Reconcile per-shed capability probe results into a cache keyed by shed id:
    /// for every shed probed in this refresh, overwrite-or-remove — non-nil → set,
    /// nil (old/downgraded binary, failed probe) → remove, so a stale block can't
    /// keep gating the launch sheet. Un-probed sheds keep their entries. (Assigning
    /// nil through the subscript removes the key.)
    public static func mergeCapabilities(
        into cache: inout [String: RcCapabilities],
        probes: [(key: String, capabilities: RcCapabilities?)]
    ) {
        for probe in probes { cache[probe.key] = probe.capabilities }
    }

    /// Decode a single-session DTO from the binary's stdout.
    public static func decodeSession(_ stdout: String) throws -> RcSessionDTO {
        guard let data = stdout.data(using: .utf8) else {
            throw RcError.failed("shed-ext-rc returned no output")
        }
        do { return try JSONDecoder().decode(RcSessionDTO.self, from: data) }
        catch { throw RcError.failed("shed-ext-rc returned an invalid session DTO") }
    }

    /// Decode the full `list` envelope — sessions PLUS the optional `capabilities`
    /// block. An old baked-in binary's bare envelope yields `capabilities: nil`.
    public static func decodeListResponse(_ stdout: String)
        throws -> (sessions: [RcSessionDTO], capabilities: RcCapabilities?)
    {
        guard let data = stdout.data(using: .utf8) else { return ([], nil) }
        do {
            let dto = try JSONDecoder().decode(RcSessionListDTO.self, from: data)
            return (dto.rcSessions, dto.capabilities)
        } catch { throw RcError.failed("shed-ext-rc returned an invalid session list") }
    }

    // MARK: - Pure pane classifier (backs the `rc.classify` IPC utility)

    public static func classifyPane(kind: RcKind, pane: String) -> RcClassification {
        // Trust + auth heuristics use claude-specific pane text, so they gate ONLY
        // the claude kinds. The per-agent pane classifiers for codex/opencode/cursor
        // are owned by the guest binary (authoritative); the client consumes the
        // DTO's `state`, so this pure utility renders every non-claude/unknown kind
        // neutrally.
        let isClaude = (kind == .claudeRc || kind == .claudeBroker)
        if isClaude {
            if pane.contains(/Workspace not trusted/.ignoresCase()) {
                return RcClassification(state: .needsTrust, url: extractURL(kind: kind, pane: pane))
            }
            if pane.contains(/Quick safety check/.ignoresCase())
                || pane.contains(/Yes,\s*I trust this folder/.ignoresCase()) {
                return RcClassification(state: .needsTrust, url: extractURL(kind: kind, pane: pane))
            }
            if pane.contains(/requires a claude\.ai subscription/.ignoresCase())
                || pane.contains(/not logged in/.ignoresCase())
                || pane.contains(/claude auth login/.ignoresCase()) {
                return RcClassification(state: .needsAuth, url: extractURL(kind: kind, pane: pane))
            }
        }

        switch kind {
        case .claudeBroker:
            let url = extractURL(kind: .claudeBroker, pane: pane)
            if pane.contains(/\bReconnecting\b/) { return RcClassification(state: .reconnecting, url: url) }
            if pane.contains(/\bConnected\b/), url != nil { return RcClassification(state: .ready, url: url) }
            if url != nil { return RcClassification(state: .ready, url: url) }
            return RcClassification(state: .starting)
        case .claudeRc:
            let url = extractURL(kind: .claudeRc, pane: pane)
            if pane.contains(/Remote Control connecting/.ignoresCase()), url == nil {
                return RcClassification(state: .starting)
            }
            if pane.contains(/Remote Control active/.ignoresCase()), url != nil {
                return RcClassification(state: .ready, url: url)
            }
            if url != nil { return RcClassification(state: .ready, url: url) }
            return RcClassification(state: .starting)
        // Shell, the non-claude agent kinds (codex/opencode/cursor), and unknown
        // kinds: neutral — blank is starting, anything drawn reads ready, no URL.
        case .codex, .opencode, .cursor, .shell, .other:
            return pane.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                ? RcClassification(state: .starting)
                : RcClassification(state: .ready)
        }
    }

    /// Extract the claude.ai URL for the given kind (claude-broker uses
    /// `?environment=env_…`, claude-rc uses `/session_…`); no URL for other kinds.
    public static func extractURL(kind: RcKind, pane: String) -> String? {
        switch kind {
        case .claudeBroker:
            if let m = pane.firstMatch(of: /https?:\/\/claude\.ai\/code\?environment=env_[A-Za-z0-9_-]+/) {
                return String(m.0)
            }
        case .claudeRc:
            if let m = pane.firstMatch(of: /https?:\/\/claude\.ai\/code\/session_[A-Za-z0-9_-]+/) {
                return String(m.0)
            }
        case .codex, .opencode, .cursor, .shell, .other:
            return nil
        }
        return nil
    }

    // MARK: - SSH

    /// Build the ssh argv that runs `remoteArgv` on the target. Mirrors
    /// shed-remote-agent's ssh options.
    public static func sshArgv(user: String, host: String, port: Int, remoteArgv: [String], connectTimeout: Int = 10) -> [String] {
        let remote = remoteArgv.map(shellQuote).joined(separator: " ")
        return [
            "ssh",
            "-o", "BatchMode=yes",
        ] + ShedSSH.hostKeyOptions + [
            "-o", "ConnectTimeout=\(connectTimeout)",
            "-p", String(port),
            "\(user)@\(host)",
            "--", remote,
        ]
    }
}
