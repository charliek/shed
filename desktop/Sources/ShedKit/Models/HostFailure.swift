// HostFailure.swift
//
// One host's probe failure in PRESENTATION shape — the Swift twin of Rust's
// `shed_app::backend::HostFailure` (plan 006 D6 / shed#300), field for field and
// string for string.
//
// Why it exists: the poller used to keep only `"\(server): \(error)"`, and
// interpolating a generated FFI enum renders its CASE (`Config(message: "…")`),
// not its message. That leak reached the banner. It also erased the one thing a
// UI must branch on: whether the host is unreachable because shed-host-agent is
// too old to obtain a certificate — a remedy the user can act on, and one the
// generic "check ~/.shed/config.yaml" empty state actively misdirects.
//
//   * `kind`    — what a view branches on (open set; `other` renders generically)
//   * `summary` — the one-line banner text, REMEDY FIRST (banners truncate from
//                 the end, so the actionable clause must lead)
//   * `detail`  — the tooltip / DiagnosticLog body
//
// Doubles as an IPC wire shape (snake_case CodingKeys), like everything in
// `Models.swift`.

import Foundation
import ShedRustCore

/// The known causes a UI treats specially. `other` is the open set — an
/// unrecognized failure renders generically but still names its host.
public enum HostFailureKind: String, Codable, Sendable, Equatable {
    case agentUpgradeRequired = "agent_upgrade_required"
    case other
}

public struct HostFailure: Codable, Sendable, Equatable {
    public var server: String
    public var kind: HostFailureKind
    public var summary: String
    public var detail: String

    public init(server: String, kind: HostFailureKind, summary: String, detail: String) {
        self.server = server
        self.kind = kind
        self.summary = summary
        self.detail = detail
    }
}

extension HostFailure {
    /// Presentation mapping for one host's error, mirroring Rust's
    /// `HostFailure::from_error`. The typed `AgentUpgradeRequired` keeps its
    /// remedy as the whole summary; everything else leads with the host name so
    /// a multi-host list stays scannable.
    ///
    /// The message is always taken from a MESSAGE property — never
    /// `"\(error)"`, which renders an enum case for the generated FFI errors
    /// (and `localizedDescription` is no better there: uniffi's
    /// `LocalizedError` conformance is `String(reflecting:)`).
    public static func from(server: String, error: Error) -> HostFailure {
        if let e = error as? ShedRustCore.ShedError,
            case .AgentUpgradeRequired(let forServer, let detail) = e
        {
            return HostFailure(
                server: server,
                kind: .agentUpgradeRequired,
                summary: "Upgrade shed-host-agent — it can't obtain a certificate for "
                    + "\(forServer).",
                detail: detail)
        }
        let detail = displayMessage(for: error)
        return HostFailure(
            server: server, kind: .other, summary: "\(server): \(detail)", detail: detail)
    }

    /// Both strings redacted (tokens scrubbed) — applied ONCE at the probe seam
    /// so the host row and the banner rollup carry identical text. Idempotent:
    /// redacting already-redacted text is a no-op.
    public func sanitized() -> HostFailure {
        HostFailure(
            server: server, kind: kind,
            summary: DiagnosticLog.redact(summary),
            detail: DiagnosticLog.redact(detail))
    }

    /// The human sentence carried by an error, whatever kind of error it is.
    /// THE single message extractor: every user-visible rendering of a caught
    /// error goes through here, never `"\(error)"` (which prints the generated
    /// FFI enum's case) — banners, per-host pane rows, create progress, and the
    /// IPC `internal` failure body included.
    /// The generated `ShedRustCore.ShedError` arm mirrors the Rust `#[error(…)]`
    /// Display strings byte for byte (shed-core `http.rs` / shed-core-ffi
    /// `lib.rs`), which is what keeps the two clients saying the same thing.
    public static func displayMessage(for error: Error) -> String {
        if let e = error as? ShedRustCore.ShedError {
            switch e {
            case .BadStatus(let status): return "shed-server returned HTTP \(status)"
            case .Transport(let message): return "transport error: \(message)"
            case .Decode(let message): return "decode error: \(message)"
            case .Create(let message): return "create failed: \(message)"
            case .Config(let message): return message
            case .AgentUpgradeRequired(let server, let detail):
                return "upgrade shed-host-agent — it is too old to obtain a client certificate "
                    + "for \(server) (\(detail))"
            }
        }
        // Then whatever the error type says about ITSELF. `description` first:
        // the app's own error types (`ShedClientError`, `HostAgentClientError`,
        // `RustCreateError`) carry their sentence there, and it is the one the
        // server/agent actually wrote. `localizedDescription` is last because for
        // a plain Swift error it degrades to the "operation couldn't be
        // completed" NSError boilerplate.
        if let e = error as? CustomStringConvertible { return e.description }
        if let e = error as? LocalizedError, let text = e.errorDescription { return text }
        return error.localizedDescription
    }

    /// The Sheds pane's empty-state text. When nothing is reachable, a host
    /// failure with a KNOWN kind speaks instead of the generic advice — telling
    /// someone to check `~/.shed/config.yaml` when the actual cause is an old
    /// shed-host-agent sends them to the wrong file (shed#300). The generic
    /// sentence stays for the case it is actually about: no typed cause at all.
    public static func shedsEmptyState(hosts: [ShedHost]) -> String {
        if hosts.contains(where: \.reachable) {
            return "No sheds across the reachable hosts."
        }
        if let known = hosts.compactMap(\.failure).first(where: { $0.kind != .other }) {
            return known.summary
        }
        return "No reachable hosts. Check ~/.shed/config.yaml and that shed-server is running."
    }
}
