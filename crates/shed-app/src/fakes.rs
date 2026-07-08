//! Test-mode implementations of the approval seam traits, used both by the
//! hermetic harness (the Tauri app wires these when `SHED_TAURI_TEST_MODE=1`) and
//! by the coordinator unit tests. The real impls (native notifications, polkit)
//! live in the Tauri crate (B3/B6).

use std::sync::Mutex;

use shed_core::approval::ApprovalRequest;

use crate::traits::{
    approval_notification_text, AuthGate, AuthOutcome, AuthPrompt, CoordinatorEvent, EventSink,
    Notifier, PostedNotification,
};

/// A no-op event sink — the default for tests + GTK (which has no approval UI to
/// notify). Tauri wires a real one that `app.emit`s.
pub struct NoopEventSink;

impl EventSink for NoopEventSink {
    fn emit(&self, _event: CoordinatorEvent) {}
}

/// Records posted notifications so the harness can assert one was shown and
/// (via `notification.invoke`) act on it. Withdrawal removes it.
#[derive(Default)]
pub struct FakeNotifier {
    posted: Mutex<Vec<PostedNotification>>,
}

impl FakeNotifier {
    pub fn new() -> Self {
        Self::default()
    }
}

impl Notifier for FakeNotifier {
    fn post(&self, req: &ApprovalRequest) {
        let mut posted = self.posted.lock().unwrap();
        posted.retain(|n| n.id != req.id);
        posted.push(PostedNotification {
            id: req.id.clone(),
            title: approval_notification_text::title(req),
            body: approval_notification_text::body(req),
        });
    }

    fn withdraw(&self, id: &str) {
        self.posted.lock().unwrap().retain(|n| n.id != id);
    }

    fn posted(&self) -> Vec<PostedNotification> {
        self.posted.lock().unwrap().clone()
    }
}

/// The test-mode gate: approves without a prompt (mirrors the mac Touch-ID
/// bypass under the harness), so the full approval matrix is green independent
/// of the real native gate.
pub struct AlwaysApprovedGate;

#[async_trait::async_trait]
impl AuthGate for AlwaysApprovedGate {
    async fn gate(&self, _prompt: AuthPrompt) -> AuthOutcome {
        AuthOutcome::Approved
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn req(id: &str) -> ApprovalRequest {
        ApprovalRequest {
            id: id.into(),
            ts: "t".into(),
            server: "".into(),
            namespace: "ssh-agent".into(),
            op: "sign".into(),
            shed: "s".into(),
            detail: "ed25519".into(),
            expires_at: "e".into(),
        }
    }

    #[test]
    fn fake_notifier_records_and_withdraws() {
        let n = FakeNotifier::new();
        n.post(&req("a"));
        n.post(&req("b"));
        assert_eq!(n.posted().len(), 2);
        assert_eq!(n.posted()[0].title, "Approve ssh-agent?");
        assert_eq!(n.posted()[0].body, "sign · s · ed25519");
        n.withdraw("a");
        let p = n.posted();
        assert_eq!(p.len(), 1);
        assert_eq!(p[0].id, "b");
    }

    #[test]
    fn fake_notifier_post_replaces_same_id() {
        let n = FakeNotifier::new();
        n.post(&req("a"));
        n.post(&req("a"));
        assert_eq!(n.posted().len(), 1);
    }
}
