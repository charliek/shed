//! The credential-approval domain — pure models, the broker's frame types, and
//! the policy engine.
//!
//! Pure and I/O-free: the approval coordinator and the `AuditStore` writer live
//! in `shed-app`; only the decision logic + frame shapes + audit schema live
//! here. Timestamps are carried verbatim as strings — flexible parsing lives in
//! `shed-app::timefmt`.

pub mod models;
pub mod policy;
pub mod protocol;

pub use models::{
    namespace, ttl_shorthand_seconds, ApprovalChoice, ApprovalDecision, ApprovalMethod,
    ApprovalRequest, ApprovalScope, AuditEntry, AuditSource, DecidedBy, PendingApprovalItem,
    PolicyAction, PolicyDecision, PolicyGate, PolicyRule, PolicyScope, SessionGrantKey,
    SshApprovalPolicy, DEFAULT_APPROVAL_TTL,
};
pub use policy::PolicyEngine;
pub use protocol::{AuditEventFrame, BrokerFrame};
