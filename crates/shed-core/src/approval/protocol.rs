//! The UDS wire protocol between shed-host-agent and the desktop, ported from
//! `HostAgentProtocol.swift`. Newline-delimited JSON, one typed envelope per
//! line. Mirrors the mini-RFC in shed-extensions.
//!
//!   app -> agent:  hello, approval_response, pong, token.get
//!   agent -> app:  hello_ack, approval_request, event, ping, token.response
//!
//! Pure: `id`/`ts` are caller-supplied (the stateful client owns UUID + clock),
//! so this crate never touches time or randomness.

use serde::{Deserialize, Serialize};
use serde_json::json;

use super::models::{ApprovalDecision, ApprovalRequest, AuditEntry, AuditSource, DecidedBy};

pub const HOST_AGENT_PROTOCOL_VERSION: u32 = 2;

/// A frame from the host agent (or the fake), decoded by `type`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum HostAgentInbound {
    HelloAck(HelloAck),
    ApprovalRequest(ApprovalRequest),
    Event(AuditEventFrame),
    Ping { id: String },
    TokenResponse(TokenResponse),
    Unknown { r#type: String },
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct HelloAck {
    #[serde(default)]
    pub namespaces: Vec<String>,
    #[serde(default)]
    pub gate_namespaces: Vec<String>,
    #[serde(default)]
    pub request_timeout_ms: i64,
    #[serde(default)]
    pub accepted: bool,
}

/// The `event` frame — a superset of the host agent's audit row, covering all
/// three namespaces (only ssh delegates a decision; the rest are stream-only).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct AuditEventFrame {
    pub kind: Option<String>,
    /// shed server (omitted in single-server mode).
    pub server: Option<String>,
    pub shed: Option<String>,
    pub ns: Option<String>,
    pub op: Option<String>,
    pub result: String,
    pub detail: Option<String>,
    /// Machine-readable failure cause; `None` on success or older agents.
    pub code: Option<String>,
    /// Short host-side explanation for a non-ok result; `None` on success/older.
    pub reason: Option<String>,
    pub approval: Option<String>,
    pub request_id: Option<String>,
    pub ts: Option<String>,
}

/// The `token.response` frame — the host agent's reply to a `token.get`.
/// `in_reply_to` echoes the request's `id` for correlation. On success `token`
/// and `expires_at` are set; on failure `error` is set and they are `None`
/// (fail closed).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct TokenResponse {
    pub in_reply_to: String,
    #[serde(default)]
    pub server: String,
    pub token: Option<String>,
    pub expires_at: Option<String>,
    pub error: Option<String>,
}

impl AuditEntry {
    /// Map a host-agent `event` frame into a stored entry (source = host-agent).
    /// The caller supplies `id`/`ts` fallbacks (a UUID + "now") when the frame
    /// omits them, keeping UUID + clock out of this crate.
    pub fn from_event_frame(
        frame: AuditEventFrame,
        id_fallback: String,
        ts_fallback: String,
    ) -> Self {
        // Take the frame by value and move its fields — `decode` returns an owned
        // frame and the decode -> map -> store flow discards it after.
        AuditEntry {
            id: frame.request_id.unwrap_or(id_fallback),
            ts: frame.ts.unwrap_or(ts_fallback),
            source: AuditSource::HostAgent,
            server: frame.server,
            shed: frame.shed,
            ns: frame.ns,
            op: frame.op,
            result: frame.result,
            detail: frame.detail,
            code: frame.code,
            reason: frame.reason,
            approval: frame.approval,
            policy: None,
        }
    }
}

/// Peek a frame's `type` discriminator without fully decoding it.
#[derive(Deserialize)]
struct TypeTag {
    #[serde(default)]
    r#type: String,
}

/// Decode one newline-JSON line into a typed inbound frame. `Err` on malformed
/// JSON or a known frame that fails to decode (the caller skips such a line); an
/// unrecognized `type` decodes to `Unknown`, never an error.
pub fn decode(line: &[u8]) -> Result<HostAgentInbound, serde_json::Error> {
    let tag: TypeTag = serde_json::from_slice(line)?;
    Ok(match tag.r#type.as_str() {
        "hello_ack" => HostAgentInbound::HelloAck(serde_json::from_slice(line)?),
        "approval_request" => HostAgentInbound::ApprovalRequest(serde_json::from_slice(line)?),
        "event" => HostAgentInbound::Event(serde_json::from_slice(line)?),
        "ping" => {
            #[derive(Deserialize)]
            struct Ping {
                #[serde(default)]
                id: String,
            }
            let p: Ping = serde_json::from_slice(line)?;
            HostAgentInbound::Ping { id: p.id }
        }
        "token.response" => HostAgentInbound::TokenResponse(serde_json::from_slice(line)?),
        other => HostAgentInbound::Unknown {
            r#type: other.to_string(),
        },
    })
}

// Outbound encoders — one JSON line each, no trailing newline added here.

/// `id`/`ts` are supplied by the caller (the stateful client owns them).
pub fn hello(
    id: &str,
    ts: &str,
    name: &str,
    version: &str,
    pid: i32,
    capabilities: &[String],
    replay_events: i64,
) -> String {
    json!({
        "v": HOST_AGENT_PROTOCOL_VERSION, "type": "hello", "id": id, "ts": ts,
        "client": { "name": name, "version": version, "pid": pid },
        "capabilities": capabilities, "replay_events": replay_events,
    })
    .to_string()
}

pub fn approval_response(
    id: &str,
    ts: &str,
    request_id: &str,
    decision: ApprovalDecision,
    decided_by: DecidedBy,
    scope: Option<&str>,
    ttl: Option<&str>,
) -> String {
    let mut obj = json!({
        "v": HOST_AGENT_PROTOCOL_VERSION, "type": "approval_response", "id": id, "ts": ts,
        "request_id": request_id,
        "decision": decision,
        "decided_by": decided_by,
    });
    if let Some(scope) = scope {
        obj["scope"] = json!(scope);
    }
    if let Some(ttl) = ttl {
        obj["ttl"] = json!(ttl);
    }
    obj.to_string()
}

pub fn pong(id: &str, ts: &str) -> String {
    json!({ "v": HOST_AGENT_PROTOCOL_VERSION, "type": "pong", "id": id, "ts": ts }).to_string()
}

/// Request a CONTROL token for `server` from the host agent. The reply is a
/// `token.response` whose `in_reply_to` echoes `id` for correlation.
pub fn token_get(id: &str, server: &str) -> String {
    json!({ "v": HOST_AGENT_PROTOCOL_VERSION, "type": "token.get", "id": id, "server": server })
        .to_string()
}

// ===========================================================================
// SERVER DIRECTION — the agent's side of the same wire (mirror of the client
// direction above). The Go source of truth is
// `cmd/shed-host-agent/desktop_protocol.go`; field names/tags/order and the
// `omitempty` set match it byte-for-byte so a golden fixture can pin the bytes.
//
//   app -> agent (server INBOUND):  hello, approval_response, pong, token.get
//   agent -> app (server OUTBOUND): hello_ack, approval_request, event, ping,
//                                   token.response
// ===========================================================================

// ---- inbound decoders (app -> agent) --------------------------------------

/// The client identity self-reported in a `hello` (Go `clientInfo`). All fields
/// default so a partial hello still decodes (Go `json.Unmarshal` zero-fills).
#[derive(Debug, Clone, PartialEq, Eq, Default, Deserialize)]
pub struct ClientInfo {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub pid: i32,
}

/// The app's registration frame (Go `helloMsg`). `capabilities`/`replay_events`
/// default when absent.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct HelloMsg {
    #[serde(default)]
    pub client: ClientInfo,
    #[serde(default)]
    pub capabilities: Vec<String>,
    #[serde(default)]
    pub replay_events: i64,
}

/// The app's reply to an approval request (Go `approvalResponseMsg`). `decision`
/// is a raw string — the server treats `"approve"` as approve and anything else
/// as deny, so decode it permissively (never fail on an unexpected token).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ApprovalResponseMsg {
    #[serde(default)]
    pub request_id: String,
    #[serde(default)]
    pub decision: String,
    #[serde(default)]
    pub decided_by: String,
    #[serde(default)]
    pub scope: Option<String>,
    #[serde(default)]
    pub ttl: Option<String>,
}

/// The app's control-token request (Go `tokenGetMsg`). `id` is echoed back as the
/// `token.response`'s `in_reply_to`.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct TokenGetMsg {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub server: String,
}

/// A frame from the app (or a fake), decoded by `type`. Mirrors `HostAgentInbound`
/// on the client side.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DesktopInbound {
    Hello(HelloMsg),
    ApprovalResponse(ApprovalResponseMsg),
    Pong,
    TokenGet(TokenGetMsg),
    Unknown { r#type: String },
}

/// Decode one newline-JSON line the app sent into a typed inbound frame. `Err` on
/// malformed JSON or a known frame that fails to decode; an unrecognized `type`
/// decodes to `Unknown`, never an error (matches the Go server's `switch et.Type`
/// with no default). Mirrors `decode` on the client side.
pub fn decode_desktop(line: &[u8]) -> Result<DesktopInbound, serde_json::Error> {
    let tag: TypeTag = serde_json::from_slice(line)?;
    Ok(match tag.r#type.as_str() {
        "hello" => DesktopInbound::Hello(serde_json::from_slice(line)?),
        "approval_response" => DesktopInbound::ApprovalResponse(serde_json::from_slice(line)?),
        "pong" => DesktopInbound::Pong,
        "token.get" => DesktopInbound::TokenGet(serde_json::from_slice(line)?),
        other => DesktopInbound::Unknown {
            r#type: other.to_string(),
        },
    })
}

// ---- outbound builders (agent -> app) -------------------------------------

/// The audit fields the agent copies into an `event` frame — a direct Rust mirror
/// of the Go `AuditEntry` (which sources the fan-out, `desktop_server.go
/// forwardAudit`). Plain `String`s so the `event` builder can reproduce Go's
/// `omitempty` exactly (a field is emitted iff its string is non-empty). `ts` is
/// carried for the caller's convenience; the `event` builder takes it as an
/// explicit argument (id/ts are caller-supplied, per this module's convention).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AuditEntryView {
    pub ts: String,
    pub server: String,
    pub shed: String,
    pub ns: String,
    pub op: String,
    pub result: String,
    pub detail: String,
    pub code: String,
    pub reason: String,
    pub approval: String,
    pub decided_by: String,
    pub scope: String,
    pub ttl: String,
}

/// `skip_serializing_if` predicate for borrowed `&str` fields. serde passes a
/// `&FieldType` to the predicate, so for a `&str` field the argument is `&&str` —
/// the omitempty test is on the double reference. This reproduces Go's
/// `omitempty` for string fields (omit iff `== ""`).
fn is_empty_str(s: &&str) -> bool {
    s.is_empty()
}

/// Build a `hello_ack` (Go `helloAckMsg`). `agent.approval_method` is always
/// `"shed-desktop"`. An EMPTY `namespaces`/`gate_namespaces` slice serializes as
/// JSON `null` (Go marshals a nil `[]string` as `null`, and both are non-omitempty
/// so always present); a non-empty slice serializes as an array. `reason` is
/// omitempty.
// One argument per Go `helloAckMsg` field — a flat wire builder, not a params-struct case.
#[allow(clippy::too_many_arguments)]
pub fn hello_ack(
    id: &str,
    ts: &str,
    agent_version: &str,
    namespaces: &[&str],
    gate_namespaces: &[String],
    request_timeout_ms: i64,
    accepted: bool,
    reason: Option<&str>,
) -> String {
    #[derive(Serialize)]
    struct AgentOut<'a> {
        version: &'a str,
        approval_method: &'a str,
    }
    #[derive(Serialize)]
    struct Frame<'a> {
        v: u32,
        #[serde(rename = "type")]
        ty: &'a str,
        id: &'a str,
        ts: &'a str,
        agent: AgentOut<'a>,
        // None -> `null`, Some -> array (mirrors Go's nil-slice-as-null).
        namespaces: Option<Vec<&'a str>>,
        gate_namespaces: Option<Vec<&'a str>>,
        request_timeout_ms: i64,
        accepted: bool,
        #[serde(skip_serializing_if = "Option::is_none")]
        reason: Option<&'a str>,
    }
    let namespaces = if namespaces.is_empty() {
        None
    } else {
        Some(namespaces.to_vec())
    };
    let gate_namespaces = if gate_namespaces.is_empty() {
        None
    } else {
        Some(gate_namespaces.iter().map(String::as_str).collect())
    };
    // Go sets a full agentInfo only on an ACCEPTED ack; the superseded/rejected
    // ack (the only accepted:false path) sends a zero-value agentInfo{} — empty
    // version + empty approval_method (desktop_server.go:234 accepted vs :355 old).
    let agent = if accepted {
        AgentOut {
            version: agent_version,
            approval_method: "shed-desktop",
        }
    } else {
        AgentOut {
            version: "",
            approval_method: "",
        }
    };
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "hello_ack",
        id,
        ts,
        agent,
        namespaces,
        gate_namespaces,
        request_timeout_ms,
        accepted,
        reason,
    })
    .unwrap_or_default()
}

/// Build an `approval_request` (Go `approvalRequestMsg`). `server` is omitempty
/// (omitted when `None`/empty, matching single-server mode).
// One argument per Go `approvalRequestMsg` field — a flat wire builder.
#[allow(clippy::too_many_arguments)]
pub fn approval_request(
    id: &str,
    ts: &str,
    namespace: &str,
    op: &str,
    server: Option<&str>,
    shed: &str,
    detail: &str,
    expires_at: &str,
) -> String {
    #[derive(Serialize)]
    struct Frame<'a> {
        v: u32,
        #[serde(rename = "type")]
        ty: &'a str,
        id: &'a str,
        ts: &'a str,
        namespace: &'a str,
        op: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        server: &'a str,
        shed: &'a str,
        detail: &'a str,
        expires_at: &'a str,
    }
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "approval_request",
        id,
        ts,
        namespace,
        op,
        server: server.unwrap_or(""),
        shed,
        detail,
        expires_at,
    })
    .unwrap_or_default()
}

/// Build an `event` frame (Go `eventMsg`) from an audit entry: `kind:"audit"`,
/// then the entry's fields with Go's exact omitempty set (only `result`, `v`,
/// `type`, `id`, `ts`, `kind` are always present). `ts` is the audit entry's own
/// timestamp (the caller passes `entry.ts`), NOT re-stamped.
pub fn event(id: &str, ts: &str, entry: &AuditEntryView) -> String {
    #[derive(Serialize)]
    struct Frame<'a> {
        v: u32,
        #[serde(rename = "type")]
        ty: &'a str,
        id: &'a str,
        ts: &'a str,
        kind: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        server: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        shed: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        ns: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        op: &'a str,
        result: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        detail: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        code: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        reason: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        approval: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        decided_by: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        scope: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        ttl: &'a str,
    }
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "event",
        id,
        ts,
        kind: "audit",
        server: &entry.server,
        shed: &entry.shed,
        ns: &entry.ns,
        op: &entry.op,
        result: &entry.result,
        detail: &entry.detail,
        code: &entry.code,
        reason: &entry.reason,
        approval: &entry.approval,
        decided_by: &entry.decided_by,
        scope: &entry.scope,
        ttl: &entry.ttl,
    })
    .unwrap_or_default()
}

/// Build a server->app `ping` keepalive (Go `pingMsg`).
pub fn ping(id: &str, ts: &str) -> String {
    #[derive(Serialize)]
    struct Frame<'a> {
        v: u32,
        #[serde(rename = "type")]
        ty: &'a str,
        id: &'a str,
        ts: &'a str,
    }
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "ping",
        id,
        ts,
    })
    .unwrap_or_default()
}

/// Build a `token.response` (Go `tokenResponseMsg`). `server` is always present;
/// `token`/`expires_at`/`error` are omitempty. Fail-closed: on error the caller
/// passes `token=None`/`expires_at=None` so a partial token can never ship.
pub fn token_response(
    id: &str,
    ts: &str,
    in_reply_to: &str,
    server: &str,
    token: Option<&str>,
    expires_at: Option<&str>,
    error: Option<&str>,
) -> String {
    #[derive(Serialize)]
    struct Frame<'a> {
        v: u32,
        #[serde(rename = "type")]
        ty: &'a str,
        id: &'a str,
        ts: &'a str,
        in_reply_to: &'a str,
        server: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        token: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        expires_at: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        error: &'a str,
    }
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "token.response",
        id,
        ts,
        in_reply_to,
        server,
        token: token.unwrap_or(""),
        expires_at: expires_at.unwrap_or(""),
        error: error.unwrap_or(""),
    })
    .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    #[test]
    fn decodes_hello_ack() {
        let line = br#"{"type":"hello_ack","namespaces":["ssh-agent"],"gate_namespaces":["ssh-agent"],"request_timeout_ms":25000,"accepted":true}"#;
        match decode(line).unwrap() {
            HostAgentInbound::HelloAck(a) => {
                assert_eq!(a.namespaces, vec!["ssh-agent"]);
                assert_eq!(a.gate_namespaces, vec!["ssh-agent"]);
                assert_eq!(a.request_timeout_ms, 25000);
                assert!(a.accepted);
            }
            other => panic!("expected hello_ack, got {other:?}"),
        }
    }

    #[test]
    fn decodes_approval_request_with_omitted_server() {
        let line = br#"{"type":"approval_request","id":"r1","ts":"t","namespace":"ssh-agent","op":"sign","shed":"s","detail":"d","expires_at":"e"}"#;
        match decode(line).unwrap() {
            HostAgentInbound::ApprovalRequest(r) => {
                assert_eq!(r.server, ""); // omitted -> "" (single-server mode)
                assert_eq!(r.id, "r1");
                assert_eq!(r.namespace, "ssh-agent");
            }
            other => panic!("expected approval_request, got {other:?}"),
        }
    }

    #[test]
    fn decodes_ping() {
        match decode(br#"{"type":"ping","id":"p9"}"#).unwrap() {
            HostAgentInbound::Ping { id } => assert_eq!(id, "p9"),
            other => panic!("expected ping, got {other:?}"),
        }
    }

    #[test]
    fn decodes_token_response_success_and_failure() {
        let ok = br#"{"type":"token.response","in_reply_to":"q1","server":"mini2","token":"tok","expires_at":"2026-07-03T02:00:00Z"}"#;
        match decode(ok).unwrap() {
            HostAgentInbound::TokenResponse(t) => {
                assert_eq!(t.in_reply_to, "q1");
                assert_eq!(t.token.as_deref(), Some("tok"));
                assert!(t.error.is_none());
            }
            other => panic!("expected token.response, got {other:?}"),
        }
        // Fail-closed: error set, token/expires_at absent.
        let fail = br#"{"type":"token.response","in_reply_to":"q1","server":"mini2","error":"host key mismatch"}"#;
        match decode(fail).unwrap() {
            HostAgentInbound::TokenResponse(t) => {
                assert_eq!(t.error.as_deref(), Some("host key mismatch"));
                assert!(t.token.is_none());
                assert!(t.expires_at.is_none());
            }
            other => panic!("expected token.response, got {other:?}"),
        }
    }

    #[test]
    fn decodes_event_frame() {
        let line = br#"{"type":"event","kind":"audit","ns":"aws-credentials","op":"get_credentials","shed":"s","result":"ok","approval":"none"}"#;
        match decode(line).unwrap() {
            HostAgentInbound::Event(e) => {
                assert_eq!(e.ns.as_deref(), Some("aws-credentials"));
                assert_eq!(e.result, "ok");
                assert!(e.code.is_none());
            }
            other => panic!("expected event, got {other:?}"),
        }
    }

    #[test]
    fn unknown_type_is_not_an_error() {
        match decode(br#"{"type":"future_frame","x":1}"#).unwrap() {
            HostAgentInbound::Unknown { r#type } => assert_eq!(r#type, "future_frame"),
            other => panic!("expected unknown, got {other:?}"),
        }
    }

    #[test]
    fn malformed_json_is_an_error() {
        assert!(decode(b"{not json").is_err());
    }

    #[test]
    fn hello_encodes_expected_shape() {
        let caps = vec!["approval.ssh".to_string(), "event.stream".to_string()];
        let v: Value =
            serde_json::from_str(&hello("i", "t", "shed-desktop", "1.2.0", 42, &caps, 50)).unwrap();
        assert_eq!(v["v"], 2);
        assert_eq!(v["type"], "hello");
        assert_eq!(v["client"]["name"], "shed-desktop");
        assert_eq!(v["client"]["pid"], 42);
        assert_eq!(v["replay_events"], 50);
    }

    #[test]
    fn approval_response_omits_absent_scope_ttl() {
        let bare: Value = serde_json::from_str(&approval_response(
            "i",
            "t",
            "r1",
            ApprovalDecision::Deny,
            DecidedBy::Timeout,
            None,
            None,
        ))
        .unwrap();
        assert_eq!(bare["decision"], "deny");
        assert_eq!(bare["decided_by"], "timeout");
        assert!(bare.get("scope").is_none());
        assert!(bare.get("ttl").is_none());

        let full: Value = serde_json::from_str(&approval_response(
            "i",
            "t",
            "r1",
            ApprovalDecision::Approve,
            DecidedBy::Touchid,
            Some("per-session"),
            Some("1h"),
        ))
        .unwrap();
        assert_eq!(full["decision"], "approve");
        assert_eq!(full["decided_by"], "touchid");
        assert_eq!(full["scope"], "per-session");
        assert_eq!(full["ttl"], "1h");
    }

    #[test]
    fn token_get_encodes_server() {
        let v: Value = serde_json::from_str(&token_get("q1", "mini2")).unwrap();
        assert_eq!(v["type"], "token.get");
        assert_eq!(v["id"], "q1");
        assert_eq!(v["server"], "mini2");
    }

    #[test]
    fn audit_entry_from_event_frame_maps_fields_and_fallbacks() {
        let frame = AuditEventFrame {
            kind: Some("audit".into()),
            server: Some("mini2".into()),
            shed: Some("s".into()),
            ns: Some("ssh-agent".into()),
            op: Some("sign".into()),
            result: "ok".into(),
            detail: Some("ed25519".into()),
            code: None,
            reason: None,
            approval: Some("host".into()),
            request_id: Some("rid".into()),
            ts: Some("2026-07-03T00:00:00Z".into()),
        };
        let e = AuditEntry::from_event_frame(frame, "fallback-id".into(), "fallback-ts".into());
        assert_eq!(e.id, "rid"); // request_id wins over fallback
        assert_eq!(e.ts, "2026-07-03T00:00:00Z");
        assert_eq!(e.source, AuditSource::HostAgent);
        assert_eq!(e.ns.as_deref(), Some("ssh-agent"));

        // Missing request_id/ts -> fallbacks.
        let bare = AuditEventFrame {
            kind: None,
            server: None,
            shed: None,
            ns: None,
            op: None,
            result: "denied".into(),
            detail: None,
            code: Some("X".into()),
            reason: None,
            approval: None,
            request_id: None,
            ts: None,
        };
        let e2 = AuditEntry::from_event_frame(bare, "fallback-id".into(), "fallback-ts".into());
        assert_eq!(e2.id, "fallback-id");
        assert_eq!(e2.ts, "fallback-ts");
        assert_eq!(e2.code.as_deref(), Some("X"));
    }

    // ---- server direction (agent -> app builders, app -> agent decoders) ----

    #[test]
    fn hello_ack_accepted_pins_go_field_order() {
        // Exact bytes pin the Go struct field order (helloAckMsg) + nested agent.
        let s = hello_ack(
            "i",
            "t",
            "v1",
            &[
                "ssh-agent",
                "aws-credentials",
                "docker-credentials",
                "egress",
            ],
            &["ssh-agent".to_string()],
            25000,
            true,
            None,
        );
        assert_eq!(
            s,
            r#"{"v":2,"type":"hello_ack","id":"i","ts":"t","agent":{"version":"v1","approval_method":"shed-desktop"},"namespaces":["ssh-agent","aws-credentials","docker-credentials","egress"],"gate_namespaces":["ssh-agent"],"request_timeout_ms":25000,"accepted":true}"#
        );
    }

    #[test]
    fn hello_ack_superseded_uses_empty_agent_null_slices_and_reason() {
        // A rejected (superseded) ack carries a ZERO-VALUE agent — empty version AND
        // empty approval_method — matching Go's `helloAckMsg{...}` with an unset
        // Agent field (desktop_server.go:355). Empty namespaces/gate -> `null`
        // (Go's nil-slice marshal); reason present.
        let s = hello_ack("i", "t", "", &[], &[], 0, false, Some("superseded"));
        assert_eq!(
            s,
            r#"{"v":2,"type":"hello_ack","id":"i","ts":"t","agent":{"version":"","approval_method":""},"namespaces":null,"gate_namespaces":null,"request_timeout_ms":0,"accepted":false,"reason":"superseded"}"#
        );
    }

    #[test]
    fn approval_request_omits_absent_server() {
        assert_eq!(
            approval_request("i", "t", "ssh-agent", "sign", None, "s", "d", "e"),
            r#"{"v":2,"type":"approval_request","id":"i","ts":"t","namespace":"ssh-agent","op":"sign","shed":"s","detail":"d","expires_at":"e"}"#
        );
        // Present server slots in its Go field position (after op, before shed).
        assert_eq!(
            approval_request("i", "t", "ssh-agent", "sign", Some("mini2"), "s", "d", "e"),
            r#"{"v":2,"type":"approval_request","id":"i","ts":"t","namespace":"ssh-agent","op":"sign","server":"mini2","shed":"s","detail":"d","expires_at":"e"}"#
        );
    }

    #[test]
    fn event_emits_kind_and_go_omitempty_set() {
        let entry = AuditEntryView {
            ts: "T".into(),
            ns: "aws-credentials".into(),
            op: "get_credentials".into(),
            shed: "s".into(),
            result: "ok".into(),
            approval: "none".into(),
            ..Default::default()
        };
        // server/detail/code/reason/decided_by/scope/ttl empty -> omitted; result always present.
        assert_eq!(
            event("i", &entry.ts, &entry),
            r#"{"v":2,"type":"event","id":"i","ts":"T","kind":"audit","shed":"s","ns":"aws-credentials","op":"get_credentials","result":"ok","approval":"none"}"#
        );
        // A gated deny carries code/reason/decided_by/scope/ttl in Go order.
        let gated = AuditEntryView {
            ts: "T".into(),
            server: "mini2".into(),
            shed: "web".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "denied".into(),
            detail: "ssh-ed25519".into(),
            code: "APPROVAL_DENIED".into(),
            reason: "approval denied by shed-desktop".into(),
            approval: "shed-desktop".into(),
            decided_by: "user".into(),
            scope: "per-request".into(),
            ttl: "2h".into(),
        };
        assert_eq!(
            event("i", &gated.ts, &gated),
            r#"{"v":2,"type":"event","id":"i","ts":"T","kind":"audit","server":"mini2","shed":"web","ns":"ssh-agent","op":"sign","result":"denied","detail":"ssh-ed25519","code":"APPROVAL_DENIED","reason":"approval denied by shed-desktop","approval":"shed-desktop","decided_by":"user","scope":"per-request","ttl":"2h"}"#
        );
    }

    #[test]
    fn ping_shape() {
        assert_eq!(ping("i", "t"), r#"{"v":2,"type":"ping","id":"i","ts":"t"}"#);
    }

    #[test]
    fn token_response_success_and_failure_omitempty() {
        assert_eq!(
            token_response(
                "i",
                "t",
                "q1",
                "mini2",
                Some("tok"),
                Some("2026-01-01T00:00:00Z"),
                None
            ),
            r#"{"v":2,"type":"token.response","id":"i","ts":"t","in_reply_to":"q1","server":"mini2","token":"tok","expires_at":"2026-01-01T00:00:00Z"}"#
        );
        // Fail-closed: error present, token/expires_at omitted; server still present.
        assert_eq!(
            token_response(
                "i",
                "t",
                "q1",
                "mini2",
                None,
                None,
                Some("host key mismatch")
            ),
            r#"{"v":2,"type":"token.response","id":"i","ts":"t","in_reply_to":"q1","server":"mini2","error":"host key mismatch"}"#
        );
    }

    #[test]
    fn decode_desktop_hello_full_and_partial() {
        match decode_desktop(
            br#"{"type":"hello","client":{"name":"App","version":"1.0","pid":42},"capabilities":["x"],"replay_events":50}"#,
        )
        .unwrap()
        {
            DesktopInbound::Hello(h) => {
                assert_eq!(h.client.name, "App");
                assert_eq!(h.client.version, "1.0");
                assert_eq!(h.client.pid, 42);
                assert_eq!(h.capabilities, vec!["x".to_string()]);
                assert_eq!(h.replay_events, 50);
            }
            other => panic!("expected hello, got {other:?}"),
        }
        // A bare hello zero-fills (Go json.Unmarshal semantics).
        match decode_desktop(br#"{"type":"hello"}"#).unwrap() {
            DesktopInbound::Hello(h) => {
                assert_eq!(h.client, ClientInfo::default());
                assert!(h.capabilities.is_empty());
                assert_eq!(h.replay_events, 0);
            }
            other => panic!("expected hello, got {other:?}"),
        }
    }

    #[test]
    fn decode_desktop_approval_response_scope_ttl_optional() {
        match decode_desktop(
            br#"{"type":"approval_response","request_id":"r1","decision":"approve","decided_by":"touchid","scope":"per-session","ttl":"1h"}"#,
        )
        .unwrap()
        {
            DesktopInbound::ApprovalResponse(r) => {
                assert_eq!(r.request_id, "r1");
                assert_eq!(r.decision, "approve");
                assert_eq!(r.decided_by, "touchid");
                assert_eq!(r.scope.as_deref(), Some("per-session"));
                assert_eq!(r.ttl.as_deref(), Some("1h"));
            }
            other => panic!("expected approval_response, got {other:?}"),
        }
        // A deny with omitted scope/ttl (and any non-"approve" token) decodes fine.
        match decode_desktop(
            br#"{"type":"approval_response","request_id":"r1","decision":"deny","decided_by":""}"#,
        )
        .unwrap()
        {
            DesktopInbound::ApprovalResponse(r) => {
                assert_eq!(r.decision, "deny");
                assert!(r.scope.is_none());
                assert!(r.ttl.is_none());
            }
            other => panic!("expected approval_response, got {other:?}"),
        }
    }

    #[test]
    fn decode_desktop_pong_tokenget_unknown_and_malformed() {
        assert_eq!(
            decode_desktop(br#"{"type":"pong"}"#).unwrap(),
            DesktopInbound::Pong
        );
        match decode_desktop(br#"{"type":"token.get","id":"q1","server":"mini2"}"#).unwrap() {
            DesktopInbound::TokenGet(t) => {
                assert_eq!(t.id, "q1");
                assert_eq!(t.server, "mini2");
            }
            other => panic!("expected token.get, got {other:?}"),
        }
        match decode_desktop(br#"{"type":"future_frame","x":1}"#).unwrap() {
            DesktopInbound::Unknown { r#type } => assert_eq!(r#type, "future_frame"),
            other => panic!("expected unknown, got {other:?}"),
        }
        assert!(decode_desktop(b"{not json").is_err());
    }
}
