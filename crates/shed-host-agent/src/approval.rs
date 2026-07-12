//! The approval-gate seam — a Rust port of the Go daemon's `approval.go` +
//! `desktop_gate.go` (`gateFor(policy)` → a gate impl). Every credential operation
//! that mutates or vends a secret goes through a gate; the deny-all default fails
//! closed. The bus/ssh handler depends on the [`ApprovalGate`] trait, never on a
//! concrete gate or on the desktop server directly, so the desktop concern stays
//! behind the seam (and the `DesktopGate` — the only gate that needs the desktop
//! server — lives behind the `desktop-forwarding` feature in `desktop.rs`).
//!
//! Mirrors Go EXACTLY:
//!   * `Approve` returns an [`ApprovalOutcome`] on BOTH approve and deny (Go returns
//!     the outcome even on the deny path, so a denied op is still audited with its
//!     `decided_by`/`scope`/`ttl`); `approved` is the allow/deny bit that Go signals
//!     via `(outcome, error)`.
//!   * `method()` names the policy for the audit `approval` field — one of the
//!     `POLICY_*` constants (`deny-all`/`approve-all`/`shed-desktop`).

use crate::config::{POLICY_APPROVE_ALL, POLICY_DENY_ALL};

/// Audit detail about how a request was decided, for the durable log. The
/// shed-desktop gate populates it from the app's response (who decided + the
/// scope/TTL the app applied); the approve-all/deny-all gates leave it empty
/// (matching Go's `ApprovalOutcome{}`). This is the single canonical outcome type,
/// shared by the gate seam (always compiled) and the desktop server (feature-gated),
/// so there's exactly one shape flowing from `request_approval` to the audit entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApprovalOutcome {
    /// Allow/deny — Go signals this via `(ApprovalOutcome, error)`; the seam folds it
    /// into the outcome so a single value carries the decision + its audit detail.
    pub approved: bool,
    /// `"user"`/`"touchid"`/`"policy"`/`"timeout"`/`""` — who/what decided.
    pub decided_by: String,
    /// Approval scope applied (e.g. `per-session`). `None` → omitted from audit.
    pub scope: Option<String>,
    /// TTL applied (e.g. `4h`). `None` → omitted from audit.
    pub ttl: Option<String>,
    /// The deny reason — the faithful port of Go's `(ApprovalOutcome, error)`: on a
    /// deny, Go carries an `error` alongside the outcome whose `Error()` some handlers
    /// record as the audit `reason` (the docker `get` deny path,
    /// `docker_handler.go:115`). The seam folds that string in here so a single value
    /// carries the decision + its deny reason. Empty on approve. The ssh/aws deny
    /// audits set no `reason`, so they ignore it; only the docker `get` deny reads it.
    pub reason: String,
}

impl ApprovalOutcome {
    /// The fail-closed outcome when NO decision was made (deny-all policy, no
    /// consumer, timeout, disconnect, transport error): denied with an empty
    /// `decided_by`, matching the Go gate's `ApprovalOutcome{}` on the error path.
    pub fn denied_no_decision() -> ApprovalOutcome {
        ApprovalOutcome {
            approved: false,
            decided_by: String::new(),
            scope: None,
            ttl: None,
            // No specific reason — this is the generic fail-closed (no consumer /
            // timeout / disconnect); the deny-all gate builds its own reason below.
            reason: String::new(),
        }
    }
}

/// Decides whether a credential operation is allowed. Every request goes through a
/// gate — the deny-all default fails closed. `Send + Sync` so the async bus tasks
/// can share one `Arc<dyn ApprovalGate>`.
#[async_trait::async_trait]
pub trait ApprovalGate: Send + Sync {
    /// Decide the operation. Returns the [`ApprovalOutcome`] (with `approved` set) on
    /// both allow and deny. `ns`/`op`/`server`/`shed` identify the request; `detail`
    /// is the human-facing reason shown in the desktop approval prompt (for SSH sign
    /// this is the fixed `"SSH sign request"`, matching Go's `desktopGate.Approve`
    /// reason). No lock may be held across the await.
    async fn approve(
        &self,
        ns: &str,
        op: &str,
        server: &str,
        shed: &str,
        detail: &str,
    ) -> ApprovalOutcome;

    /// Names the policy for the audit log: one of the `POLICY_*` constants.
    fn method(&self) -> &str;
}

/// Approves every request — the approve-all policy. Returns an EMPTY outcome
/// (`decided_by`/`scope`/`ttl` all empty), matching Go's `noopGate.Approve` which
/// returns a zero-value `ApprovalOutcome{}` — so an approve-all audit omits
/// `decided_by`/`scope`/`ttl`. The allowlist/role still applies downstream in the
/// AWS/Docker backends (not relevant to SSH sign).
pub struct ApproveAllGate;

#[async_trait::async_trait]
impl ApprovalGate for ApproveAllGate {
    async fn approve(
        &self,
        _ns: &str,
        _op: &str,
        _server: &str,
        _shed: &str,
        _detail: &str,
    ) -> ApprovalOutcome {
        ApprovalOutcome {
            approved: true,
            decided_by: String::new(),
            scope: None,
            ttl: None,
            reason: String::new(),
        }
    }
    fn method(&self) -> &str {
        POLICY_APPROVE_ALL
    }
}

/// Rejects every request — the deny-all policy and the safe default (an
/// omitted/empty policy resolves here). Denies with an empty `decided_by`
/// (Go's `denyAllGate.Approve` returns `ApprovalOutcome{}` + error).
pub struct DenyAllGate;

#[async_trait::async_trait]
impl ApprovalGate for DenyAllGate {
    async fn approve(
        &self,
        _ns: &str,
        _op: &str,
        _server: &str,
        _shed: &str,
        _detail: &str,
    ) -> ApprovalOutcome {
        // The deny-all reason is Go's `denyAllGate.Approve` error string verbatim
        // (`approval.go:36`) — the docker `get` deny audit records it as `reason`.
        ApprovalOutcome {
            reason: "denied: approval policy is deny-all".to_string(),
            ..ApprovalOutcome::denied_no_decision()
        }
    }
    fn method(&self) -> &str {
        POLICY_DENY_ALL
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn approve_all_allows_with_empty_outcome() {
        // Matches Go's noopGate: approved, but a zero-value outcome (empty
        // decided_by/scope/ttl -> omitted from the audit).
        let g = ApproveAllGate;
        let out = g
            .approve("ssh-agent", "sign", "", "web", "SSH sign request")
            .await;
        assert!(out.approved);
        assert_eq!(out.decided_by, "");
        assert_eq!(out.scope, None);
        assert_eq!(out.ttl, None);
        assert_eq!(g.method(), "approve-all");
    }

    #[tokio::test]
    async fn deny_all_denies_no_decision() {
        let g = DenyAllGate;
        let out = g
            .approve("ssh-agent", "sign", "", "web", "SSH sign request")
            .await;
        assert!(!out.approved);
        assert_eq!(out.decided_by, "");
        assert_eq!(out.scope, None);
        assert_eq!(out.ttl, None);
        // The deny-all reason is Go's verbatim error string (recorded as the docker
        // get deny audit `reason`).
        assert_eq!(out.reason, "denied: approval policy is deny-all");
        assert_eq!(g.method(), "deny-all");
    }

    #[test]
    fn denied_no_decision_is_fail_closed() {
        let out = ApprovalOutcome::denied_no_decision();
        assert!(!out.approved);
        assert_eq!(out.decided_by, "");
        assert!(out.scope.is_none());
        assert!(out.ttl.is_none());
        // The generic fail-closed carries no specific reason (the deny-all gate sets
        // its own; the desktop no-decision paths leave it empty — sub-plan 5).
        assert_eq!(out.reason, "");
    }
}
