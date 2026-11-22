//! shed-app — the display-free shed-server application layer: the
//! shed-core-backed [`Backend`] (one HTTP client per configured host + the
//! pull-based create store), approval [`Coordinator`], [`AuditStore`],
//! [`AuthModeRegistry`], and optional `EmbeddedBroker`.

pub mod audit_store;
pub mod auth_modes;
pub mod backend;
#[cfg(feature = "broker")]
pub mod broker_bridge;
/// What the credential broker tells the approval Coordinator.
pub mod broker_event;
pub mod coordinator;
/// The credential-shape rules for a minted control credential; the embedded
/// bridge is their only caller.
#[cfg(feature = "broker")]
mod credential_shape;
pub mod fakes;
pub mod timefmt;
pub mod traits;

pub use audit_store::AuditStore;
pub use auth_modes::{AuthModeRegistry, AuthModeState};
pub use backend::{
    Backend, HostDiskUsage, HostEgressProfiles, HostFailure, HostFailureKind, HostSource, RcTarget,
    Reachability, SourceMachineRow,
};
#[cfg(feature = "broker")]
pub use broker_bridge::{
    load_or_synthesize, machine_identity, BrokerConfig, BrokerError, EmbeddedBroker,
};
pub use broker_event::BrokerEvent;
pub use coordinator::{Coordinator, CoordinatorDeps, SshPrefs};
pub use fakes::{AlwaysApprovedGate, FakeNotifier, NoopEventSink};
/// The embedded broker's log seam ([`EmbeddedBroker::new`] takes one), so a
/// client implements it without depending on `shed-broker` itself.
#[cfg(feature = "broker")]
pub use shed_broker::bus::BusLog;
/// This machine's identity, as [`machine_identity`] and
/// [`EmbeddedBroker::machine_identity`] hand it out — named here so an embedder
/// needs no `shed-broker` dependency of its own.
#[cfg(feature = "broker")]
pub use shed_broker::machine::MachineIdentity;
pub use traits::{
    AuthGate, AuthGateRef, AuthOutcome, AuthPrompt, Clock, ClockRef, CoordinatorEvent, EventSink,
    EventSinkRef, Notifier, NotifierRef, PostedNotification, Responder, ResponderRef, SystemClock,
};
