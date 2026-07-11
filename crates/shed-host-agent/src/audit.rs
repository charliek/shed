//! The audit seam — a Rust port of the Go daemon's `audit.go`. Every credential
//! operation that a handler wants recorded goes through an [`AuditSink`]: the
//! production [`JsonlAuditSink`] writes a durable JSON-lines record AND fans the
//! entry out to the shed-desktop app (so the app's all-namespace activity feed works
//! even when file logging is disabled).
//!
//! Wire-visible shapes matched to Go EXACTLY:
//!   * **Durable JSONL** (catalog §3.1): one object per line, `\n`-terminated, field
//!     order = the Go `AuditEntry` struct order `ts,server,shed,ns,op,result,detail,
//!     code,reason,approval,decided_by,scope,ttl`, with Go's omitempty set (a field
//!     is emitted iff non-empty, EXCEPT `ts`/`shed`/`ns`/`op`/`result`/`approval`,
//!     which are always present). Built from a typed struct (declaration order) so
//!     the field order is pinned regardless of serde_json's map ordering.
//!   * **Desktop fan-out** (catalog §3.3): each entry → an `event` frame 1:1 via
//!     [`DesktopServer::publish_audit`](crate::desktop::DesktopServer::publish_audit).
//!   * **Disabled / open failure** (catalog §3.2): logging off or the file can't be
//!     opened → no file writes, no panic; the fan-out still happens.
//!   * File perms: dir `0700`, file `O_APPEND|O_CREATE|O_WRONLY 0600` (umask-safe:
//!     0600 has no group/other bits), matching Go's `MkdirAll`/`OpenFile`.

use std::fs::File;
use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::Path;
use std::sync::Mutex;

use serde::Serialize;

use crate::config::HostAgentConfig;
use crate::status::now_rfc3339;

/// A single audit record. All fields are plain `String`s so the JSONL builder can
/// reproduce Go's omitempty exactly (emit iff non-empty). Mirrors Go's `AuditEntry`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AuditEntry {
    /// RFC3339 UTC; stamped by [`AuditSink::log`] if left empty (Go `LogEntry`).
    pub ts: String,
    /// Discovery server name; empty (omitted) in single-server mode.
    pub server: String,
    pub shed: String,
    pub ns: String,
    pub op: String,
    pub result: String,
    /// Free text (key type, `N keys`, …); omitted when empty.
    pub detail: String,
    /// Protocol error code or the audit-only `APPROVAL_DENIED`; omitted when empty.
    pub code: String,
    /// Host-side human explanation for a non-ok result; omitted when empty.
    pub reason: String,
    /// The policy method: `deny-all`/`approve-all`/`shed-desktop`/`none`.
    pub approval: String,
    /// Gated ops: who decided; omitted when empty.
    pub decided_by: String,
    pub scope: String,
    pub ttl: String,
}

/// The durable JSONL wire shape: field order = Go struct order, omitempty = Go's set.
/// Serialized directly to a string (not via `serde_json::Value`) so the declaration
/// order is the emission order regardless of serde_json's map ordering.
#[derive(Serialize)]
struct WireEntry<'a> {
    ts: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    server: &'a str,
    shed: &'a str,
    ns: &'a str,
    op: &'a str,
    result: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    detail: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    code: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    reason: &'a str,
    approval: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    decided_by: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    scope: &'a str,
    #[serde(skip_serializing_if = "is_empty_str")]
    ttl: &'a str,
}

/// serde passes `&FieldType` to the predicate, so for a `&str` field the arg is
/// `&&str` — the omitempty test is on the double reference (Go `omitempty` for a
/// string is "omit iff `== \"\"`").
fn is_empty_str(s: &&str) -> bool {
    s.is_empty()
}

/// Serialize one entry to its durable JSONL line (no trailing newline).
fn to_jsonl(entry: &AuditEntry) -> String {
    let wire = WireEntry {
        ts: &entry.ts,
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
    };
    serde_json::to_string(&wire).unwrap_or_default()
}

/// Records an audit entry. Every credential handler holds an `Arc<dyn AuditSink>`;
/// the seam keeps the durable-log + desktop-fan-out concerns out of the handler.
/// `log` is synchronous and non-blocking (no `.await`, no lock held across one) so
/// it never stalls the bus request path.
pub trait AuditSink: Send + Sync {
    fn log(&self, entry: AuditEntry);
}

/// The production sink: appends durable JSONL to the configured file AND fans each
/// entry out to the desktop server. Either half may be absent — a disabled/failed
/// file still fans out; a headless build (`--no-default-features`) has no desktop
/// half at all.
pub struct JsonlAuditSink {
    /// `None` when logging is disabled or the file couldn't be opened (no-op file
    /// half — the fan-out still runs). Behind a `Mutex` so concurrent handlers append
    /// atomically (Go guards `encoder.Encode` with a mutex).
    file: Option<Mutex<File>>,
    /// The desktop fan-out target. `None` when no desktop server is present.
    #[cfg(feature = "desktop-forwarding")]
    desktop: Option<std::sync::Arc<crate::desktop::DesktopServer>>,
}

impl JsonlAuditSink {
    /// Build the production sink from config. Opens the durable file when
    /// `logging.enabled` (creating the dir `0700` / file `0600`); an open failure
    /// degrades to the no-op file half (no panic), still fanning out. `desktop` is
    /// the fan-out target (pass the running `DesktopServer`, or `None`).
    #[cfg(feature = "desktop-forwarding")]
    pub fn new(
        cfg: &HostAgentConfig,
        desktop: Option<std::sync::Arc<crate::desktop::DesktopServer>>,
    ) -> JsonlAuditSink {
        JsonlAuditSink {
            file: open_audit_file(cfg),
            desktop,
        }
    }

    /// Headless build: no desktop fan-out half.
    #[cfg(not(feature = "desktop-forwarding"))]
    pub fn new(cfg: &HostAgentConfig) -> JsonlAuditSink {
        JsonlAuditSink {
            file: open_audit_file(cfg),
        }
    }
}

/// Open the durable audit file per config, or `None` (no-op) when disabled or on any
/// dir/file error. Mirrors Go's `NewAuditLogger`.
fn open_audit_file(cfg: &HostAgentConfig) -> Option<Mutex<File>> {
    if !cfg.logging_enabled {
        return None;
    }
    let path = Path::new(&cfg.logging_path);
    if let Some(dir) = path.parent() {
        if std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)
            .is_err()
        {
            return None; // dir couldn't be created -> no-op (Go warns + no-ops)
        }
    }
    std::fs::OpenOptions::new()
        .append(true)
        .create(true)
        .mode(0o600)
        .open(path)
        .ok()
        .map(Mutex::new)
}

impl AuditSink for JsonlAuditSink {
    fn log(&self, mut entry: AuditEntry) {
        if entry.ts.is_empty() {
            entry.ts = now_rfc3339();
        }
        if let Some(file) = &self.file {
            let mut line = to_jsonl(&entry);
            line.push('\n');
            if let Ok(mut f) = file.lock() {
                let _ = f.write_all(line.as_bytes());
            }
        }
        // Fan out to the desktop app even when file logging is disabled (catalog §3.3).
        #[cfg(feature = "desktop-forwarding")]
        if let Some(desktop) = &self.desktop {
            desktop.publish_audit(&entry_to_view(&entry));
        }
    }
}

/// Map a durable [`AuditEntry`] to the desktop `event` frame's source view (1:1 copy;
/// the `event` builder adds only `id`/`v`/`type`/`kind`).
#[cfg(feature = "desktop-forwarding")]
fn entry_to_view(entry: &AuditEntry) -> crate::desktop_protocol::AuditEntryView {
    crate::desktop_protocol::AuditEntryView {
        ts: entry.ts.clone(),
        server: entry.server.clone(),
        shed: entry.shed.clone(),
        ns: entry.ns.clone(),
        op: entry.op.clone(),
        result: entry.result.clone(),
        detail: entry.detail.clone(),
        code: entry.code.clone(),
        reason: entry.reason.clone(),
        approval: entry.approval.clone(),
        decided_by: entry.decided_by.clone(),
        scope: entry.scope.clone(),
        ttl: entry.ttl.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn jsonl_field_order_and_omitempty_gated_ok_entry() {
        // A shed-desktop ok sign entry: detail + decided_by present, server/code/
        // reason/scope/ttl empty -> omitted; ts/shed/ns/op/result/approval always
        // present. Exercises the full omitempty + field-order set.
        let entry = AuditEntry {
            ts: "2026-07-10T00:00:00Z".into(),
            shed: "web".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            detail: "ssh-ed25519".into(),
            approval: "shed-desktop".into(),
            decided_by: "user".into(),
            ..Default::default()
        };
        assert_eq!(
            to_jsonl(&entry),
            r#"{"ts":"2026-07-10T00:00:00Z","shed":"web","ns":"ssh-agent","op":"sign","result":"ok","detail":"ssh-ed25519","approval":"shed-desktop","decided_by":"user"}"#
        );
    }

    #[test]
    fn jsonl_denied_entry_omits_detail_and_code() {
        // A shed-desktop deny (matching ssh_handler.go's deny audit): result=denied,
        // approval=shed-desktop, decided_by/scope/ttl from the outcome; NO detail, NO
        // code, NO reason (the SSH sign deny path sets none of those — unlike Docker).
        let entry = AuditEntry {
            ts: "2026-07-10T00:00:00Z".into(),
            shed: "web".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "denied".into(),
            approval: "shed-desktop".into(),
            decided_by: "user".into(),
            scope: "per-session".into(),
            ttl: "1h".into(),
            ..Default::default()
        };
        assert_eq!(
            to_jsonl(&entry),
            r#"{"ts":"2026-07-10T00:00:00Z","shed":"web","ns":"ssh-agent","op":"sign","result":"denied","approval":"shed-desktop","decided_by":"user","scope":"per-session","ttl":"1h"}"#
        );
    }

    #[test]
    fn empty_shed_and_server_shape() {
        // Single-server + approve-all: server empty -> omitted; shed empty -> PRESENT
        // as ""; decided_by empty (approve-all's zero-value outcome) -> omitted.
        let entry = AuditEntry {
            ts: "T".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            detail: "ssh-ed25519".into(),
            approval: "approve-all".into(),
            ..Default::default()
        };
        assert_eq!(
            to_jsonl(&entry),
            r#"{"ts":"T","shed":"","ns":"ssh-agent","op":"sign","result":"ok","detail":"ssh-ed25519","approval":"approve-all"}"#
        );
    }

    #[test]
    fn disabled_logging_writes_no_file_and_stamps_ts() {
        let cfg = HostAgentConfig::parse("logging:\n  enabled: false\n");
        #[cfg(feature = "desktop-forwarding")]
        let sink = JsonlAuditSink::new(&cfg, None);
        #[cfg(not(feature = "desktop-forwarding"))]
        let sink = JsonlAuditSink::new(&cfg);
        // No file half; log() is a no-op-plus-stamp and must not panic.
        assert!(sink.file.is_none());
        sink.log(AuditEntry {
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            ..Default::default()
        });
    }

    #[test]
    fn enabled_logging_appends_jsonl_with_trailing_newline() {
        let dir = std::env::temp_dir().join(format!("shed-audit-{}", uuid::Uuid::new_v4()));
        let path = dir.join("audit.log");
        let cfg = HostAgentConfig::parse(&format!(
            "logging:\n  enabled: true\n  path: {}\n",
            path.display()
        ));
        #[cfg(feature = "desktop-forwarding")]
        let sink = JsonlAuditSink::new(&cfg, None);
        #[cfg(not(feature = "desktop-forwarding"))]
        let sink = JsonlAuditSink::new(&cfg);
        sink.log(AuditEntry {
            ts: "T1".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "ok".into(),
            detail: "ssh-ed25519".into(),
            approval: "approve-all".into(),
            ..Default::default()
        });
        sink.log(AuditEntry {
            ts: "T2".into(),
            ns: "ssh-agent".into(),
            op: "sign".into(),
            result: "denied".into(),
            approval: "deny-all".into(),
            ..Default::default()
        });
        let contents = std::fs::read_to_string(&path).unwrap();
        let lines: Vec<&str> = contents.lines().collect();
        assert_eq!(lines.len(), 2);
        assert!(contents.ends_with('\n'));
        assert_eq!(
            lines[0],
            r#"{"ts":"T1","shed":"","ns":"ssh-agent","op":"sign","result":"ok","detail":"ssh-ed25519","approval":"approve-all"}"#
        );
        assert_eq!(
            lines[1],
            r#"{"ts":"T2","shed":"","ns":"ssh-agent","op":"sign","result":"denied","approval":"deny-all"}"#
        );

        // File perms are 0600 (owner-only), matching Go's OpenFile mode.
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600, "audit file must be 0600");

        let _ = std::fs::remove_dir_all(&dir);
    }
}
