// HostAgentProtocol.swift — the UDS wire protocol between shed-host-agent
// and shed-desktop (M3). Newline-delimited JSON, one typed envelope per
// line. Mirrors the mini-RFC in shed-extensions.
//
//   app → agent:  hello, approval_response, pong, token.get
//   agent → app:  hello_ack, approval_request, event, ping, token.response

import Foundation

public let hostAgentProtocolVersion = 2

/// A frame from the host agent (or the fake), decoded by `type`.
public enum HostAgentInbound: Sendable {
    case helloAck(HelloAck)
    case approvalRequest(ApprovalRequest)
    case event(AuditEventFrame)
    case ping(id: String)
    case tokenResponse(TokenResponse)
    case unknown(type: String)
}

public struct HelloAck: Sendable, Decodable {
    public let namespaces: [String]
    public let gateNamespaces: [String]
    public let requestTimeoutMs: Int
    public let accepted: Bool

    enum CodingKeys: String, CodingKey {
        case namespaces
        case gateNamespaces = "gate_namespaces"
        case requestTimeoutMs = "request_timeout_ms"
        case accepted
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
}

public enum HostAgentProtocol {
    /// Decode one newline-JSON line into a typed inbound frame.
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

    private static func line(_ obj: [String: Any]) throws -> Data {
        try JSONSerialization.data(withJSONObject: obj)
    }
}
