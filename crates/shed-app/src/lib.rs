//! shed-app — the display-free app-logic layer shared by the shed clients: the
//! shed-core-backed [`Backend`] (one HTTP client per configured host + the
//! pull-based create store), with no UI or env-prefix coupling. The GTK + Tauri
//! clients (and later the Swift app via the FFI) each build it from their own
//! `SHED_*_` env via [`Backend::from_env_parts`]. Depends only on the pure
//! `shed-core` protocol crate — this is where the per-client app logic that was
//! Swift-only (poller, df/images, the reachability rollup) will also land (A1a-add).

pub mod audit_store;
pub mod auth_modes;
pub mod backend;
/// The reconnect schedule shared by the long-lived feed watchers
/// ([`rc_events_watcher`] and [`machine`]), so their "deliberately identical"
/// cadences are identical by construction rather than by convention.
mod backoff;
#[cfg(feature = "broker")]
pub mod broker_bridge;
pub mod coordinator;
pub mod fakes;
pub mod host_agent;
/// The machine transport seam + the reconnecting hub watcher (plan 012).
/// Deliberately NOT behind the `rc` feature: shed-mobile links this crate with
/// default features and the machine feed is exactly what it needs.
pub mod machine;
#[cfg(feature = "rc")]
pub mod rc;
/// The ported one-shot RC **engine** (plan 009 C3) — the local, synchronous
/// producer of RC sessions, as opposed to [`rc`]'s async client of a REMOTE one.
/// Graduated into its own crate at its second consumer (plan 010 H2:
/// shed-broker's `rc_hub`); re-exported here so sx and the desktop keep the
/// `shed_app::rc_engine::…` paths. Behind the same `rc` feature: a consumer
/// that has no RC pane wants neither.
#[cfg(feature = "rc")]
pub use shed_rc_engine as rc_engine;
pub mod rc_events_watcher;
pub mod timefmt;
pub mod token_minter;
pub mod traits;

pub use audit_store::AuditStore;
pub use auth_modes::{AuthModeRegistry, AuthModeState};
pub use backend::{
    Backend, HostDiskUsage, HostEgressProfiles, HostFailure, HostFailureKind, RcTarget,
    Reachability,
};
#[cfg(feature = "broker")]
pub use broker_bridge::{
    detect_mode, load_or_synthesize, probe_sockets, probe_sockets_at, resolve_mode, BrokerConfig,
    BrokerError, DetectedMode, EffectiveMode, EmbeddedHostAgent, ModePref, ModeProbe, RcHubHost,
    ResolvedMode,
};
pub use coordinator::{Coordinator, CoordinatorDeps, SshPrefs};
pub use fakes::{AlwaysApprovedGate, FakeNotifier, NoopEventSink};
pub use host_agent::{
    AgentCapabilityState, CapabilitySnapshot, HelloClientInfo, HostAgentClient,
    HostAgentClientError, HostAgentEvent,
};
pub use machine::{
    FixedPort, ForwardError, MachineForward, MachineHubUpdate, MachineHubWatcher, SshForward,
};
#[cfg(feature = "rc")]
pub use rc::{RcRunner, RcRunnerRef, RcService, RunOutput, TokioProcessRunner};
#[cfg(feature = "rc")]
pub use rc_engine::{
    CreateOptions as RcCreateOptions, Engine as RcEngine, EngineError as RcEngineError, ExecRunner,
    PromptOptions as RcPromptOptions, TmuxResult, TmuxRunner,
};
pub use rc_events_watcher::{RcEventsWatcher, RcWatcherUpdate};
pub use token_minter::HostAgentTokenMinter;
pub use traits::{
    AuthGate, AuthGateRef, AuthOutcome, AuthPrompt, Clock, ClockRef, CoordinatorEvent, EventSink,
    EventSinkRef, Notifier, NotifierRef, PostedNotification, Responder, ResponderRef, SystemClock,
};
