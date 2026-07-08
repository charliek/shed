//! shed-app — the display-free app-logic layer shared by the shed clients: the
//! shed-core-backed [`Backend`] (one HTTP client per configured host + the
//! pull-based create store), with no UI or env-prefix coupling. The GTK + Tauri
//! clients (and later the Swift app via the FFI) each build it from their own
//! `SHED_*_` env via [`Backend::from_env_parts`]. Depends only on the pure
//! `shed-core` protocol crate — this is where the per-client app logic that was
//! Swift-only (poller, df/images, the reachability rollup) will also land (A1a-add).

pub mod audit_store;
pub mod backend;
pub mod coordinator;
pub mod fakes;
pub mod host_agent;
#[cfg(feature = "rc")]
pub mod rc;
pub mod timefmt;
pub mod token_minter;
pub mod traits;

pub use audit_store::AuditStore;
pub use backend::{Backend, HostDiskUsage, RcTarget, Reachability};
#[cfg(feature = "rc")]
pub use rc::{RcRunner, RcRunnerRef, RcService, RunOutput, TokioProcessRunner};
pub use coordinator::{Coordinator, CoordinatorDeps, SshPrefs};
pub use fakes::{AlwaysApprovedGate, FakeNotifier, NoopEventSink};
pub use host_agent::{HelloClientInfo, HostAgentClient, HostAgentClientError, HostAgentEvent};
pub use token_minter::HostAgentTokenMinter;
pub use traits::{
    AuthGate, AuthGateRef, AuthOutcome, AuthPrompt, Clock, ClockRef, CoordinatorEvent, EventSink,
    EventSinkRef, Notifier, NotifierRef, PostedNotification, Responder, ResponderRef, SystemClock,
};
