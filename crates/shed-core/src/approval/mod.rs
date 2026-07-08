//! The credential-approval domain — pure models, the host-agent wire codec, and
//! the policy engine, ported from shed-desktop's `Sources/ShedKit/Approval/*`.
//!
//! Pure and I/O-free: the stateful `HostAgentClient` UDS state machine, the
//! approval coordinator, and the `AuditStore` writer live in `shed-app`; only
//! the decision logic + wire shapes + audit schema live here. Timestamps are
//! carried verbatim as strings — flexible parsing lives in `shed-app::timefmt`.

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
// Frame/vocabulary types are flat; the codec free-functions (`decode` + the
// encoders) stay namespaced under `approval::protocol::` for symmetry.
pub use protocol::{
    AuditEventFrame, HelloAck, HostAgentInbound, TokenResponse, HOST_AGENT_PROTOCOL_VERSION,
};
