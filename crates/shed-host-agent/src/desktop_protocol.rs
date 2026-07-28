//! The SERVER direction of the shed-host-agent ⇄ desktop UDS wire protocol — the
//! agent's side of the same newline-delimited JSON codec whose CLIENT direction
//! lives in `shed_core::approval::protocol`. Only the desktop server (`desktop.rs`)
//! consumes these builders/decoders, so they live here — beside their sole
//! consumer — rather than in the client-shared core.
//!
//! The Go source of truth is `cmd/shed-host-agent/desktop_protocol.go`; field
//! names/tags/order and the `omitempty` set match it byte-for-byte so a golden
//! fixture can pin the bytes.
//!
//!   app -> agent (server INBOUND):  hello, approval_response, pong, token.get,
//!                                   credential.get
//!   agent -> app (server OUTBOUND): hello_ack, approval_request, event, ping,
//!                                   token.response, credential.response
//!
//! Pure: `id`/`ts` are caller-supplied (the stateful server owns UUID + clock), so
//! this module never touches time or randomness — same convention as the client
//! codec. The protocol version is the single source of truth in shed-core
//! (`HOST_AGENT_PROTOCOL_VERSION`), reused here so both directions emit the same `v`.
#![cfg(feature = "desktop-forwarding")]

use serde::{Deserialize, Serialize};

use shed_core::approval::protocol::{CAP_CREDENTIAL_GET, HOST_AGENT_PROTOCOL_VERSION};

/// What this build advertises in its `hello_ack`. Ordered, not sorted at send
/// time: it is a wire value pinned by golden fixtures in both implementations.
/// Mirrors Go `desktop_protocol.go:agentCapabilities`.
pub fn agent_capabilities() -> Vec<&'static str> {
    vec![CAP_CREDENTIAL_GET]
}

/// Peek a frame's `type` discriminator without fully decoding it. A private mirror
/// of the client codec's `TypeTag` (that one is not `pub`), kept here so
/// `decode_desktop` can peek without a dependency on shed-core internals.
#[derive(Deserialize)]
struct TypeTag {
    #[serde(default)]
    r#type: String,
}

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

/// The app's mode-agnostic credential request (Go `credentialGetMsg`).
///
/// `csr` is a standard-base64 PKCS#10 DER the APP generated; only it crosses the
/// socket, and the private key it will pair with never leaves the app process.
/// It is optional — an app that has no use for certificates may ask without one,
/// and an mtls server then answers with its own explicit upgrade error.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct CredentialGetMsg {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub server: String,
    #[serde(default)]
    pub csr: String,
}

/// A frame from the app (or a fake), decoded by `type`. Mirrors `HostAgentInbound`
/// on the client side.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DesktopInbound {
    Hello(HelloMsg),
    ApprovalResponse(ApprovalResponseMsg),
    Pong,
    TokenGet(TokenGetMsg),
    CredentialGet(CredentialGetMsg),
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
        "credential.get" => DesktopInbound::CredentialGet(serde_json::from_slice(line)?),
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
    agent_caps: &[&str],
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
        // OMITTED when empty, unlike the two above: Go tags this `omitempty`
        // because "the key is absent" is the load-bearing signal an old agent
        // sends, and `null` would be a third state nobody reads.
        #[serde(skip_serializing_if = "Option::is_none")]
        agent_capabilities: Option<Vec<&'a str>>,
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
    let agent_capabilities = if agent_caps.is_empty() {
        None
    } else {
        Some(agent_caps.to_vec())
    };
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "hello_ack",
        id,
        ts,
        agent,
        namespaces,
        gate_namespaces,
        agent_capabilities,
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

/// Build a `credential.response` (Go `credentialResponseMsg`). `server` is
/// always present; every credential field is omitempty. Fail-closed: on error
/// the caller passes `None` for all of them, so a partial credential can never
/// ship and an errored reply can never be mistaken for an empty success.
// One argument per Go field — a flat wire builder, not a params-struct case.
#[allow(clippy::too_many_arguments)]
pub fn credential_response(
    id: &str,
    ts: &str,
    in_reply_to: &str,
    server: &str,
    auth_mode: Option<&str>,
    token: Option<&str>,
    client_cert: Option<&str>,
    cert_serial: Option<&str>,
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
        auth_mode: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        token: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        client_cert: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        cert_serial: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        expires_at: &'a str,
        #[serde(skip_serializing_if = "is_empty_str")]
        error: &'a str,
    }
    serde_json::to_string(&Frame {
        v: HOST_AGENT_PROTOCOL_VERSION,
        ty: "credential.response",
        id,
        ts,
        in_reply_to,
        server,
        auth_mode: auth_mode.unwrap_or(""),
        token: token.unwrap_or(""),
        client_cert: client_cert.unwrap_or(""),
        cert_serial: cert_serial.unwrap_or(""),
        expires_at: expires_at.unwrap_or(""),
        error: error.unwrap_or(""),
    })
    .unwrap_or_default()
}

/// Read one of the plan 002 §7 P9 desktop-credential vectors — the SAME files
/// the Go agent runner (`cmd/shed-host-agent/golden_test.go`), the Rust client
/// (`crates/shed-app`), and the Swift client read. Lives outside the tests
/// module so `desktop.rs`'s live-server golden shares it.
#[cfg(test)]
pub(crate) fn desktop_fixture(name: &str) -> serde_json::Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/host-agent-diff/fixtures/desktop-credential")
        .join(name);
    let raw = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("read fixture {}: {e}", path.display()));
    serde_json::from_str(&raw).expect("fixture is valid JSON")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

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
            &agent_capabilities(),
            25000,
            true,
            None,
        );
        assert_eq!(
            s,
            r#"{"v":2,"type":"hello_ack","id":"i","ts":"t","agent":{"version":"v1","approval_method":"shed-desktop"},"namespaces":["ssh-agent","aws-credentials","docker-credentials","egress"],"gate_namespaces":["ssh-agent"],"agent_capabilities":["credential.get"],"request_timeout_ms":25000,"accepted":true}"#
        );
    }

    #[test]
    fn hello_ack_superseded_uses_empty_agent_null_slices_and_reason() {
        // A rejected (superseded) ack carries a ZERO-VALUE agent — empty version AND
        // empty approval_method — matching Go's `helloAckMsg{...}` with an unset
        // Agent field (desktop_server.go:355). Empty namespaces/gate -> `null`
        // (Go's nil-slice marshal); reason present.
        let s = hello_ack("i", "t", "", &[], &[], &[], 0, false, Some("superseded"));
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

    #[test]
    fn credential_response_pins_go_field_order_and_omitempty() {
        // Token mode: auth_mode + token + expiry; no certificate fields.
        assert_eq!(
            credential_response(
                "i",
                "t",
                "q1",
                "mini2",
                Some("token"),
                Some("tok"),
                None,
                None,
                Some("2026-01-01T00:00:00Z"),
                None
            ),
            r#"{"v":2,"type":"credential.response","id":"i","ts":"t","in_reply_to":"q1","server":"mini2","auth_mode":"token","token":"tok","expires_at":"2026-01-01T00:00:00Z"}"#
        );
        // mtls: the certificate and its serial, and NO bearer token.
        assert_eq!(
            credential_response(
                "i",
                "t",
                "q2",
                "mini2",
                Some("mtls"),
                None,
                Some("PEM"),
                Some("0a0b"),
                Some("2026-01-01T00:00:00Z"),
                None
            ),
            r#"{"v":2,"type":"credential.response","id":"i","ts":"t","in_reply_to":"q2","server":"mini2","auth_mode":"mtls","client_cert":"PEM","cert_serial":"0a0b","expires_at":"2026-01-01T00:00:00Z"}"#
        );
        // Fail-closed: error only, server still present.
        assert_eq!(
            credential_response(
                "i",
                "t",
                "q3",
                "mini2",
                None,
                None,
                None,
                None,
                None,
                Some("unknown server")
            ),
            r#"{"v":2,"type":"credential.response","id":"i","ts":"t","in_reply_to":"q3","server":"mini2","error":"unknown server"}"#
        );
    }

    #[test]
    fn hello_ack_omits_capabilities_when_there_are_none() {
        // The OLD-agent shape. Absent, not `null` and not `[]`: absence is the
        // signal a new app turns into "upgrade shed-host-agent".
        let s = hello_ack("i", "t", "v1", &[], &[], &[], 0, true, None);
        assert!(
            !s.contains("agent_capabilities"),
            "an ack with no capabilities must omit the key: {s}"
        );
    }

    #[test]
    fn decode_desktop_credential_get_with_and_without_a_csr() {
        match decode_desktop(
            br#"{"type":"credential.get","id":"q1","server":"mini2","csr":"QUJD"}"#,
        )
        .unwrap()
        {
            DesktopInbound::CredentialGet(c) => {
                assert_eq!(c.id, "q1");
                assert_eq!(c.server, "mini2");
                assert_eq!(c.csr, "QUJD");
            }
            other => panic!("expected credential.get, got {other:?}"),
        }
        // An omitted csr zero-fills (Go json.Unmarshal semantics) rather than
        // failing: asking without one is legal, and the answer is the server's.
        match decode_desktop(br#"{"type":"credential.get","id":"q2","server":"mini2"}"#).unwrap() {
            DesktopInbound::CredentialGet(c) => assert_eq!(c.csr, ""),
            other => panic!("expected credential.get, got {other:?}"),
        }
    }

    // --- plan 002 §7 P9: the shared desktop-credential wire vectors ---------
    //
    // The SAME files the Go agent runner (cmd/shed-host-agent/golden_test.go),
    // the Rust client (crates/shed-app), and the Swift client
    // (desktop/Tests/ShedKitTests/HostAgentCredentialFixtureTests.swift) read.
    // This crate is only ever the AGENT, so it asserts the two directions an
    // agent owns: the frames it EMITS and the frame it DECODES. Which of those
    // frames a client may adopt is the clients' half of the same fixture.

    /// The capability derivation every client performs on an ack, using THIS
    /// crate's production constant — plus the assertion that the ack this build
    /// emits is a `supported` one. Renaming `CAP_CREDENTIAL_GET` or dropping it
    /// from `agent_capabilities()` fails here, in lockstep with the Go runner.
    #[test]
    fn golden_desktop_hello_ack_capability() {
        let fx = desktop_fixture("hello_ack.json");
        assert_eq!(fx["protocol_version"], 1, "fixture version skew");
        let vectors = fx["vectors"].as_array().expect("vectors");
        assert!(!vectors.is_empty());
        let supports = |caps: Option<&Vec<Value>>| {
            caps.is_some_and(|c| c.iter().any(|v| v == CAP_CREDENTIAL_GET))
        };
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let caps = v["frame"]["agent_capabilities"].as_array();
            let want = match v["expected_capability"].as_str().unwrap() {
                "supported" => true,
                "unsupported" => false,
                other => panic!("{name}: unexpected fixture capability {other:?}"),
            };
            assert_eq!(supports(caps), want, "{name}");
        }
        // This build's own ack, through the production builder.
        let emitted: Value = serde_json::from_str(&hello_ack(
            "i",
            "t",
            "v1",
            &["ssh-agent"],
            &["ssh-agent".to_string()],
            &agent_capabilities(),
            25000,
            true,
            None,
        ))
        .unwrap();
        assert!(
            supports(emitted["agent_capabilities"].as_array()),
            "this agent's hello_ack does not advertise {CAP_CREDENTIAL_GET}"
        );
    }

    /// The agent's DECODE of the app's request. The load-bearing case is the
    /// CSR-less one: `csr` absent must read as "no CSR" — the condition an mtls
    /// server answers with its own explicit upgrade error — never as a
    /// zero-length CSR the agent would relay as if it were one.
    #[test]
    fn golden_desktop_credential_get() {
        let fx = desktop_fixture("credential_get.json");
        assert_eq!(fx["protocol_version"], 1, "fixture version skew");
        let vectors = fx["vectors"].as_array().expect("vectors");
        assert!(!vectors.is_empty());
        for v in vectors {
            let name = v["name"].as_str().unwrap();
            let line = serde_json::to_vec(&v["expected_frame"]).unwrap();
            let msg = match decode_desktop(&line).unwrap() {
                DesktopInbound::CredentialGet(c) => c,
                other => panic!("{name}: expected credential.get, got {other:?}"),
            };
            assert_eq!(
                msg.server,
                v["request"]["server"].as_str().unwrap(),
                "{name}"
            );
            // The wire is authoritative: a null/empty request csr is omitted by
            // the sender, so the agent sees the empty string either way.
            let want_csr = v["expected_frame"]["csr"].as_str().unwrap_or("");
            assert_eq!(msg.csr, want_csr, "{name}: csr");
        }
    }
}
