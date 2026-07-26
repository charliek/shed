// HostAgentProtocol.swift — the UDS wire protocol between shed-host-agent
// and shed-desktop (M3). Newline-delimited JSON, one typed envelope per
// line. Mirrors the mini-RFC in shed-extensions.
//
//   app → agent:  hello, approval_response, pong, token.get, credential.get
//   agent → app:  hello_ack, approval_request, event, ping, token.response,
//                 credential.response

import Foundation

public let hostAgentProtocolVersion = 2

/// A frame from the host agent (or the fake), decoded by `type`.
public enum HostAgentInbound: Sendable {
    case helloAck(HelloAck)
    case approvalRequest(ApprovalRequest)
    case event(AuditEventFrame)
    case ping(id: String)
    case tokenResponse(TokenResponse)
    case credentialResponse(CredentialResponse)
    case unknown(type: String)
}

/// Optional messages an agent may advertise in `hello_ack.agent_capabilities`.
/// shed-desktop and shed-host-agent are separately released, so every version
/// pairing runs in the field; the capability list (not the frame version, which
/// is stamped and never checked) is how each side names what the other can do.
public enum HostAgentCapability {
    /// The agent answers `credential.get`: a mode-agnostic control credential,
    /// including relaying a CSR to an mtls-mode server and returning the issued
    /// certificate. Absent from an agent too old to know the message.
    public static let credentialGet = "credential.get"
}

/// What this connection knows about the agent's `credential.get` support
/// (plan 002 §7 P5). The third state is the point: before the first `hello_ack`
/// the app has learned NOTHING, and must not conclude "old agent" — that would
/// either produce a false "upgrade shed-host-agent" error or, worse, silently
/// fall back to `token.get` against a server that only accepts certificates.
public enum AgentCapabilityState: Sendable, Equatable {
    /// No `hello_ack` seen yet on the current connection (startup, reconnect).
    case unknown
    /// The ack advertised `credential.get`.
    case supported
    /// The ack arrived WITHOUT `credential.get` — a shipped older agent.
    case unsupported

    public init(helloAck ack: HelloAck) {
        self = ack.agentCapabilities.contains(HostAgentCapability.credentialGet)
            ? .supported : .unsupported
    }
}

public struct HelloAck: Sendable, Decodable {
    public let namespaces: [String]
    public let gateNamespaces: [String]
    /// Optional messages this agent answers (see `HostAgentCapability`). An
    /// agent older than the capability list omits the key entirely, which is
    /// exactly the signal `AgentCapabilityState.unsupported` keys on.
    public let agentCapabilities: [String]
    public let requestTimeoutMs: Int
    public let accepted: Bool

    enum CodingKeys: String, CodingKey {
        case namespaces
        case gateNamespaces = "gate_namespaces"
        case agentCapabilities = "agent_capabilities"
        case requestTimeoutMs = "request_timeout_ms"
        case accepted
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        // Match Rust #[serde(default)] — older agents may omit these keys.
        namespaces = try c.decodeIfPresent([String].self, forKey: .namespaces) ?? []
        gateNamespaces = try c.decodeIfPresent([String].self, forKey: .gateNamespaces) ?? []
        agentCapabilities = try c.decodeIfPresent([String].self, forKey: .agentCapabilities) ?? []
        requestTimeoutMs = try c.decodeIfPresent(Int.self, forKey: .requestTimeoutMs) ?? 0
        accepted = try c.decodeIfPresent(Bool.self, forKey: .accepted) ?? false
    }
}

/// The `event` frame — a superset of the host agent's audit row, covering
/// all three namespaces (only ssh delegates a decision; the rest are
/// stream-only).
public struct AuditEventFrame: Sendable, Decodable {
    public let kind: String?
    public let server: String?         // shed server (omitted in single-server mode)
    public let shed: String?
    public let ns: String?
    public let op: String?
    public let result: String
    public let detail: String?
    public let code: String?           // machine-readable failure cause (e.g. REGISTRY_NOT_ALLOWED); nil on success or older agents
    public let reason: String?         // short host-side explanation for a non-ok result; nil on success or older agents
    public let approval: String?
    public let requestID: String?
    public let ts: String?

    enum CodingKeys: String, CodingKey {
        case kind, server, shed, ns, op, result, detail, code, reason, approval, ts
        case requestID = "request_id"
    }
}

/// The `token.response` frame — the host agent's reply to a `token.get`. The
/// `inReplyTo` echoes the request's `id` for correlation. On success `token` and
/// `expiresAt` are set; on failure `error` is set and they are nil (fail closed).
public struct TokenResponse: Sendable, Decodable {
    public let inReplyTo: String
    public let server: String
    public let token: String?
    public let expiresAt: String?
    public let error: String?

    enum CodingKeys: String, CodingKey {
        case server, token, error
        case inReplyTo = "in_reply_to"
        case expiresAt = "expires_at"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        inReplyTo = try c.decode(String.self, forKey: .inReplyTo)
        // Match Rust #[serde(default)] on server.
        server = try c.decodeIfPresent(String.self, forKey: .server) ?? ""
        token = try c.decodeIfPresent(String.self, forKey: .token)
        expiresAt = try c.decodeIfPresent(String.self, forKey: .expiresAt)
        error = try c.decodeIfPresent(String.self, forKey: .error)
    }
}

/// Size caps on the credential exchange. A control credential is a small,
/// bounded thing — a token, a leaf certificate, a hex serial — so a field
/// arriving orders of magnitude larger is a bug or an attempt to make this
/// process carry something it should not. Refusing costs one re-mint; accepting
/// costs whatever the oversized value was for. Mirrored on both directions of
/// the socket: what we accept, and the CSR we are willing to send.
///
/// The numbers are OWNED by `shed_core::token::limits` (Rust), where both Rust
/// credential mappers — shed-app's `credential_from_parts` and shed-core-ffi's
/// `credential_from_answer` — read them. Swift cannot import them, so this
/// third copy is pinned to the same values by the shared §7 P9 fixture's
/// `limits` block, which each language asserts against
/// (`HostAgentCredentialFixtureTests`). Change one, and the fixture assertion
/// fails in every language until all three agree.
public enum HostAgentCredentialLimits {
    public static let maxTokenBytes = 4 * 1024
    public static let maxClientCertBytes = 64 * 1024
    public static let maxCertSerialBytes = 128
    public static let maxErrorBytes = 4 * 1024
    /// A P-256 PKCS#10 CSR is ~600 bytes base64; 16 KiB leaves room for larger
    /// key types without leaving room for a payload.
    public static let maxCSRBytes = 16 * 1024
}

/// Errors raised while ENCODING an outbound frame (the inbound direction
/// returns refusals, never throws).
public enum HostAgentProtocolError: Error, Equatable, CustomStringConvertible, Sendable {
    case oversizedCSR(bytes: Int)

    public var description: String {
        switch self {
        case .oversizedCSR(let bytes):
            return "refusing to send a \(bytes)-byte CSR (cap \(HostAgentCredentialLimits.maxCSRBytes))"
        }
    }
}

/// The `credential.response` frame — the agent's reply to a `credential.get`,
/// the mode-agnostic successor to `token.response`. `authMode` names which of
/// `token` / `clientCert` is populated, so the app never infers the server's
/// mode from which field happens to be non-empty. On failure `error` is set and
/// every credential field is empty (fail closed, never a partial credential).
public struct CredentialResponse: Sendable, Decodable, Equatable {
    public let inReplyTo: String
    public let server: String
    /// The RAW wire value, deliberately un-normalized: interpreting it is
    /// `validatedCredential(for:)`'s job and its rules are strict.
    public let authMode: String?
    public let token: String?
    public let clientCert: String?
    public let certSerial: String?
    public let expiresAt: String?
    public let error: String?

    enum CodingKeys: String, CodingKey {
        case server, token, error
        case inReplyTo = "in_reply_to"
        case authMode = "auth_mode"
        case clientCert = "client_cert"
        case certSerial = "cert_serial"
        case expiresAt = "expires_at"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        inReplyTo = try c.decode(String.self, forKey: .inReplyTo)
        // Match the agents' omitempty/#[serde(default)] shapes.
        server = try c.decodeIfPresent(String.self, forKey: .server) ?? ""
        authMode = try c.decodeIfPresent(String.self, forKey: .authMode)
        token = try c.decodeIfPresent(String.self, forKey: .token)
        clientCert = try c.decodeIfPresent(String.self, forKey: .clientCert)
        certSerial = try c.decodeIfPresent(String.self, forKey: .certSerial)
        expiresAt = try c.decodeIfPresent(String.self, forKey: .expiresAt)
        error = try c.decodeIfPresent(String.self, forKey: .error)
    }

    public init(
        inReplyTo: String, server: String, authMode: String? = nil, token: String? = nil,
        clientCert: String? = nil, certSerial: String? = nil, expiresAt: String? = nil,
        error: String? = nil
    ) {
        self.inReplyTo = inReplyTo
        self.server = server
        self.authMode = authMode
        self.token = token
        self.clientCert = clientCert
        self.certSerial = certSerial
        self.expiresAt = expiresAt
        self.error = error
    }
}

/// Outcome of validating a host-agent `token.response`: the validated token, or
/// a descriptive failure message the caller maps to its own error type.
public enum TokenValidation: Sendable {
    case valid(String)
    case invalid(String)
}

/// Outcome of validating a host-agent `credential.response` — the ARM is the
/// mode. Once a response has been through this mapper the auth-mode string is
/// gone: nothing downstream can re-interpret it, and the only way to reach the
/// token arm is to have satisfied the token rules here.
public enum CredentialValidation: Sendable, Equatable {
    case token(token: String, expiresAt: Date?)
    case certificate(certPEM: String, serial: String, expiresAt: Date?)
    /// Refused, with the message to surface verbatim. Nothing is adopted.
    case refused(String)
}

extension CredentialResponse {
    /// The STRICT wire→arm mapper — the single place plan 002's fail-closed rule
    /// is enforced on the desktop (§2 C2(ii)).
    ///
    /// Rules, in order:
    ///   1. a non-empty `error` refuses (the agent already failed closed);
    ///   2. a non-empty `server` that isn't the one asked for refuses;
    ///   3. the mode string selects the arm, matched EXACTLY (no trimming, no
    ///      case folding — `"MTLS"` is not `"mtls"`, mirroring the Go bundle
    ///      rule that case must not fuzzy-match):
    ///        * absent / `""` / `"token"` / the legacy `"secure"` → token arm,
    ///        * `"mtls"` → certificate arm,
    ///        * ANYTHING else → refused. A future mode this build cannot
    ///          evaluate must never be answered with a bearer token: that is
    ///          precisely the silent downgrade the mtls rollout exists to
    ///          prevent, and the honest reply is "upgrade the app";
    ///   4. every populated field is within its size cap (`HostAgentCredentialLimits`);
    ///   5. a populated `expires_at` must PARSE — an unparseable one refuses
    ///      rather than silently becoming "no expiry", which would disable the
    ///      proactive refresh and leave the credential to die mid-request
    ///      (absent stays fine: it genuinely means "the agent reported none");
    ///   6. the selected arm's payload must be complete AND exclusive — a token
    ///      arm carrying a certificate OR a certificate serial (and vice versa)
    ///      is a protocol violation from an implementation neither agent has,
    ///      not an empty success.
    public func validatedCredential(for server: String) -> CredentialValidation {
        if let error, !error.isEmpty {
            guard error.utf8.count <= HostAgentCredentialLimits.maxErrorBytes else {
                return .refused(
                    "host agent returned an oversized error message for \(server) "
                        + "(\(error.utf8.count) bytes); refusing")
            }
            return .refused(error)
        }
        // Empty server is allowed (both agents default it); a non-empty mismatch
        // is fail-closed — the same rule token.get validation uses.
        if !self.server.isEmpty, self.server != server {
            return .refused("host agent returned a credential for unexpected server \(self.server)")
        }
        let cert = clientCert ?? ""
        let tok = token ?? ""
        let serial = certSerial ?? ""
        if let oversized = Self.overCapField(token: tok, cert: cert, serial: serial) {
            return .refused(
                "host agent returned an oversized \(oversized) for \(server); refusing")
        }
        let expiry: Date?
        switch parsedExpiry() {
        case .some(let date): expiry = date
        case .none: return .refused(
            "host agent returned an unparseable expiry \"\(expiresAt ?? "")\" for \(server); refusing")
        }
        switch authMode ?? "" {
        case "", "token", "secure":
            guard !tok.isEmpty else {
                return .refused("host agent returned no token for \(server)")
            }
            guard cert.isEmpty, serial.isEmpty else {
                return .refused(
                    "host agent returned a token AND certificate fields for \(server); refusing an ambiguous credential")
            }
            return .token(token: tok, expiresAt: expiry)
        case "mtls":
            guard !cert.isEmpty else {
                return .refused("host agent reported auth mode mtls but returned no certificate for \(server)")
            }
            guard tok.isEmpty else {
                return .refused(
                    "host agent returned a certificate AND a token for \(server); refusing an ambiguous credential")
            }
            return .certificate(certPEM: cert, serial: serial, expiresAt: expiry)
        case let other:
            return .refused(
                "host agent reported unknown auth mode \"\(other)\" for \(server); "
                    + "refusing to fall back to a bearer token — upgrade shed-desktop")
        }
    }

    /// Names the first field over its cap, or nil when all fit. A credential is
    /// a small, bounded thing; anything larger is a bug or an attempt to make
    /// this process hold something it should not.
    private static func overCapField(token: String, cert: String, serial: String) -> String? {
        if token.utf8.count > HostAgentCredentialLimits.maxTokenBytes { return "token" }
        if cert.utf8.count > HostAgentCredentialLimits.maxClientCertBytes { return "certificate" }
        if serial.utf8.count > HostAgentCredentialLimits.maxCertSerialBytes { return "certificate serial" }
        return nil
    }

    /// Expiry as a Date, distinguishing "absent" from "unparseable": the outer
    /// `Optional` is nil ONLY when a populated value failed to parse, so the
    /// caller can refuse it. Swift owns the flexible ISO-8601 parsing (neither
    /// the Rust core nor the agents re-parse it). An empty string is treated as
    /// absent — both agents use omitempty, so it cannot carry meaning.
    private func parsedExpiry() -> Date?? {
        guard let expiresAt, !expiresAt.isEmpty else { return .some(nil) }
        guard let date = DateFormatting.parseFlexibleTimestamp(expiresAt) else { return nil }
        return .some(date)
    }
}

extension TokenResponse {
    /// Fail-closed validation shared by the two host-agent token minters
    /// (`ControlTokenProvider.hostAgent` and `HostAgentTokenMinter`). One copy
    /// keeps the two paths from diverging — a fail-open drift here would be a
    /// security regression.
    public func validatedToken(for server: String) -> TokenValidation {
        if let error, !error.isEmpty {
            return .invalid(error)
        }
        // Empty server is allowed (serde default); a non-empty mismatch is fail-closed.
        if !self.server.isEmpty, self.server != server {
            return .invalid("host agent returned token for unexpected server \(self.server)")
        }
        guard let token, !token.isEmpty else {
            return .invalid("host agent returned no token for \(server)")
        }
        return .valid(token)
    }
}

public enum HostAgentProtocol {
    /// Decode one newline-JSON line into a typed inbound frame.
    ///
    /// Duplicate keys: Foundation is FIRST-key-wins (both `JSONSerialization`
    /// and `JSONDecoder`), while Go's `encoding/json` and `serde_json` take the
    /// LAST — so a frame with two `auth_mode` keys would be read differently by
    /// each language. That divergence is out of the threat model rather than
    /// unnoticed: the only peer on this socket IS the credential broker, which
    /// composes every frame from typed structs and is already trusted with the
    /// credential itself. Nothing is gained by hardening the parser against a
    /// peer that could simply send the value it wanted.
    public static func decode(line: Data) throws -> HostAgentInbound {
        let obj = try JSONSerialization.jsonObject(with: line) as? [String: Any] ?? [:]
        let type = obj["type"] as? String ?? ""
        switch type {
        case "hello_ack":
            return .helloAck(try JSONDecoder().decode(HelloAck.self, from: line))
        case "approval_request":
            return .approvalRequest(try JSONDecoder().decode(ApprovalRequest.self, from: line))
        case "event":
            return .event(try JSONDecoder().decode(AuditEventFrame.self, from: line))
        case "ping":
            return .ping(id: obj["id"] as? String ?? "")
        case "token.response":
            return .tokenResponse(try JSONDecoder().decode(TokenResponse.self, from: line))
        case "credential.response":
            return .credentialResponse(try JSONDecoder().decode(CredentialResponse.self, from: line))
        default:
            return .unknown(type: type)
        }
    }

    // MARK: - outbound encoders (one JSON line, no trailing newline added here)

    public static func hello(id: String, ts: String, name: String, version: String, pid: Int32, capabilities: [String], replayEvents: Int) throws -> Data {
        try line([
            "v": hostAgentProtocolVersion, "type": "hello", "id": id, "ts": ts,
            "client": ["name": name, "version": version, "pid": Int(pid)],
            "capabilities": capabilities, "replay_events": replayEvents,
        ])
    }

    public static func approvalResponse(id: String, ts: String, requestID: String, decision: ApprovalDecision, decidedBy: DecidedBy, scope: String? = nil, ttl: String? = nil) throws -> Data {
        var obj: [String: Any] = [
            "v": hostAgentProtocolVersion, "type": "approval_response", "id": id, "ts": ts,
            "request_id": requestID, "decision": decision.rawValue, "decided_by": decidedBy.rawValue,
        ]
        if let scope { obj["scope"] = scope }
        if let ttl { obj["ttl"] = ttl }
        return try line(obj)
    }

    public static func pong(id: String, ts: String) throws -> Data {
        try line(["v": hostAgentProtocolVersion, "type": "pong", "id": id, "ts": ts])
    }

    /// Request a CONTROL token for `server` from the host agent. The reply is a
    /// `token.response` whose `in_reply_to` echoes `id` for correlation.
    public static func tokenGet(id: String, server: String) throws -> Data {
        try line(["v": hostAgentProtocolVersion, "type": "token.get", "id": id, "server": server])
    }

    /// Request a mode-agnostic CONTROL credential for `server`. `csrBase64` is
    /// the standard-base64 PKCS#10 request the Rust core composed around a
    /// keypair it holds; it is relayed VERBATIM and is the only key-adjacent
    /// value that crosses this socket — the private half never leaves the core
    /// (plan 002 §7 P3). Omitted when the core supplied none. The reply is a
    /// `credential.response` whose `in_reply_to` echoes `id`.
    public static func credentialGet(id: String, server: String, csrBase64: String?) throws -> Data {
        var obj: [String: Any] = [
            "v": hostAgentProtocolVersion, "type": "credential.get", "id": id, "server": server,
        ]
        if let csrBase64, !csrBase64.isEmpty {
            guard csrBase64.utf8.count <= HostAgentCredentialLimits.maxCSRBytes else {
                throw HostAgentProtocolError.oversizedCSR(bytes: csrBase64.utf8.count)
            }
            obj["csr"] = csrBase64
        }
        return try line(obj)
    }

    private static func line(_ obj: [String: Any]) throws -> Data {
        try JSONSerialization.data(withJSONObject: obj)
    }
}
