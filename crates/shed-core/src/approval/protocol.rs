//! The frames the credential broker hands the approval Coordinator: an approval
//! request, or an audit row from the broker's all-namespace feed.
//!
//! Pure: `id`/`ts` fallbacks are caller-supplied (the app owns UUID + clock), so
//! this crate never touches time or randomness.

use serde::Deserialize;

use super::models::{ApprovalRequest, AuditEntry, AuditSource};

/// A frame from the broker.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum BrokerFrame {
    ApprovalRequest(ApprovalRequest),
    Event(AuditEventFrame),
}

/// The `event` frame — the broker's audit row, covering every namespace (only
/// ssh delegates a decision; the rest are stream-only).
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

impl AuditEntry {
    /// Map a broker `event` frame into a stored entry (source = broker).
    /// The caller supplies `id`/`ts` fallbacks (a UUID + "now") when the frame
    /// omits them, keeping UUID + clock out of this crate.
    pub fn from_event_frame(
        frame: AuditEventFrame,
        id_fallback: String,
        ts_fallback: String,
    ) -> Self {
        // Take the frame by value and move its fields — the map -> store flow
        // discards the frame after.
        AuditEntry {
            id: frame.request_id.unwrap_or(id_fallback),
            ts: frame.ts.unwrap_or(ts_fallback),
            source: AuditSource::Broker,
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

#[cfg(test)]
mod tests {
    use super::*;

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
        assert_eq!(e.source, AuditSource::Broker);
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
