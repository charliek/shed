//! Platform-seam traits shared by the shed clients. The pure decision logic
//! lives in `shed-core`; these are the I/O / platform boundaries the coordinator
//! and the host-agent client depend on, so `shed-app` stays UI-free. GTK ignores
//! the approval seams; Tauri implements them (B3+). This module grows as later
//! milestones add `AuthGate` / `Notifier` / `Paths`.

use std::sync::Arc;

/// Injectable "now", so the expiry / TTL / grant / TOCTOU edge-cases are
/// deterministic in tests. `now_iso8601` formats the wire `ts` fields
/// (hello / pong / approval_response / audit) off `now_unix`, keeping timestamp
/// formatting in `shed-app` (`shed-core` stays parse/format-free).
pub trait Clock: Send + Sync {
    fn now_unix(&self) -> i64;
    fn now_iso8601(&self) -> String {
        crate::timefmt::format_iso8601(self.now_unix())
    }
}

/// A shared clock handle (the coordinator + host-agent client share one clock).
pub type ClockRef = Arc<dyn Clock>;

/// The real clock — the only place `shed-app` reads the wall clock (chrono's
/// `clock` feature is disabled precisely so "now" flows through this seam).
pub struct SystemClock;

impl Clock for SystemClock {
    fn now_unix(&self) -> i64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0)
    }
}

/// A `SystemClock` behind an `Arc`, for wiring into the client/coordinator.
pub fn system_clock() -> ClockRef {
    Arc::new(SystemClock)
}

// ---- AuthGate (the native approval gate) ----

/// The outcome of an auth gate — a rich enum, never a bare `bool` (F5). Every
/// non-`Approved` outcome is deny-safe; the coordinator keeps the request
/// *pending* (mac-parity: a failed/absent gate leaves it for retry-or-expiry)
/// and surfaces the reason. `Unavailable` (no polkit/PAM on this system) is
/// distinguished from `Denied`/`Cancelled` so it can be audited differently.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AuthOutcome {
    Approved,
    Denied,
    Cancelled,
    Unavailable,
    Error(String),
}

/// A request for the native auth gate. `biometrics_only` selects the
/// no-password-fallback path (mac Touch-ID-only; fail-closed on Linux).
#[derive(Debug, Clone)]
pub struct AuthPrompt {
    pub reason: String,
    pub biometrics_only: bool,
}

/// The native approval gate (mac Touch ID; Linux polkit — B6). Backend/native-
/// mediated: the credential never touches the app or the webview.
#[async_trait::async_trait]
pub trait AuthGate: Send + Sync {
    async fn gate(&self, prompt: AuthPrompt) -> AuthOutcome;
}

pub type AuthGateRef = Arc<dyn AuthGate>;

// ---- Notifier (actionable approval notifications) ----

/// A notification the app asked to be shown — surfaced over IPC
/// (`notifications.list`) so the harness can assert one was posted.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct PostedNotification {
    pub id: String,
    pub title: String,
    pub body: String,
}

/// Standard title/body for an approval, shared by the real + fake presenters so
/// what the harness asserts matches what a user would see.
pub mod approval_notification_text {
    use shed_core::approval::ApprovalRequest;
    pub fn title(req: &ApprovalRequest) -> String {
        format!("Approve {}?", req.namespace)
    }
    pub fn body(req: &ApprovalRequest) -> String {
        format!("{} · {} · {}", req.op, req.qualified_shed(), req.detail)
    }
}

/// Posts/withdraws actionable approval notifications. The real impl (Tauri, B3+)
/// shows a native banner; the fake records posts so the harness can drive them.
pub trait Notifier: Send + Sync {
    fn post(&self, req: &shed_core::approval::ApprovalRequest);
    fn withdraw(&self, id: &str);
    /// The test-mode presenter records posts; the real one returns empty. The
    /// harness reads this via `notifications.list`.
    fn posted(&self) -> Vec<PostedNotification> {
        Vec::new()
    }
}

pub type NotifierRef = Arc<dyn Notifier>;

// ---- Responder (the coordinator's decision sink) ----

/// Sends an approve/deny decision to the host agent (a no-op if disconnected —
/// the agent then fails closed). Abstracts `HostAgentClient::respond` so the
/// coordinator is decoupled from the transport and unit-testable. The impl must
/// be synchronous + non-blocking so the coordinator can call it inside its atomic
/// command handler.
pub trait Responder: Send + Sync {
    fn respond(
        &self,
        request_id: &str,
        decision: shed_core::approval::ApprovalDecision,
        decided_by: shed_core::approval::DecidedBy,
        scope: Option<&str>,
        ttl: Option<&str>,
    );
}

pub type ResponderRef = Arc<dyn Responder>;

// ---- EventSink (push coordinator-state changes to the UI) ----

/// What changed in the coordinator, so a UI can re-fetch just that slice.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CoordinatorEvent {
    /// The pending-approvals queue changed (a new prompt, a decision, an expiry,
    /// a disconnect drop, a policy re-evaluation).
    Approvals,
    /// A new audit entry (a decision or a streamed host event).
    Activity,
    /// The host-agent connection came up — the delegated `gate_namespaces` are
    /// available (drives which approval-prefs sections show).
    Connected,
}

/// Pushes coordinator-state changes to the platform UI (Tauri: `app.emit`; GTK/
/// tests: no-op). Lets the webview re-fetch reactively instead of polling.
pub trait EventSink: Send + Sync {
    fn emit(&self, event: CoordinatorEvent);
}

pub type EventSinkRef = Arc<dyn EventSink>;
