//! What the credential broker tells the approval [`Coordinator`](crate::Coordinator).
//!
//! The embedded broker bridge (`broker_bridge.rs`, behind the `broker` feature)
//! is the only producer; the Coordinator is the consumer. Ungated because the
//! Coordinator is: a default-features build links the Coordinator and simply
//! never builds a broker.

use shed_core::approval::protocol::BrokerFrame;

/// One event from the broker. `Frame` is boxed — an approval request or an audit
/// row is much larger than `Connected`.
#[derive(Debug)]
pub enum BrokerEvent {
    /// The broker is up. `gate_namespaces` are the namespaces whose approvals it
    /// delegates to this app (drives which approval-prefs sections show).
    Connected { gate_namespaces: Vec<String> },
    /// An approval request, or an audit row fanned in from the broker.
    Frame(Box<BrokerFrame>),
}
