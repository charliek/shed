//! The UDS wire protocol between shed-host-agent and the desktop, ported from
//! `HostAgentProtocol.swift`. Newline-delimited JSON, one typed envelope per
//! line. Mirrors the mini-RFC in shed-extensions.
//!
//!   app -> agent:  hello, approval_response, pong, token.get
//!   agent -> app:  hello_ack, approval_request, event, ping, token.response
//!
//! Pure: `id`/`ts` are caller-supplied (the stateful client owns UUID + clock),
//! so this crate never touches time or randomness.

use serde::Deserialize;
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
}
