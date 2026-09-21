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
/// The reconnect schedule for the long-lived feed watchers.
mod backoff;
#[cfg(feature = "broker")]
pub mod broker_bridge;
pub mod coordinator;
pub mod fakes;
pub mod host_agent;
/// The staged agent-lane view (plan 018 §3.5) — the client-side fold of a
/// [`shed_core::lane`] subscription into a renderable, TYPED snapshot.
/// Ungated for the same reason [`machine`] and [`roost`] are: shed-mobile links
/// this crate with default features and folds the same subscription the desktop
/// does, so the fold has to be one implementation rather than two.
pub mod lane_view;
/// The machine transport seam (plan 012) — the per-client local-port forward
/// every machine reach is built on. Ungated for the same reason [`roost`] is:
/// shed-mobile links this crate with default features.
pub mod machine;
/// The roost reach seam + the polling roost-session inventory watcher (plan
/// 013). Ungated for the same reason [`machine`] is: shed-mobile links this
/// crate with default features, and reading a roost-session is exactly what it
/// needs.
pub mod roost;
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
    BrokerError, DetectedMode, EffectiveMode, EmbeddedHostAgent, ModePref, ModeProbe, ResolvedMode,
};
pub use coordinator::{Coordinator, CoordinatorDeps, SshPrefs};
pub use fakes::{AlwaysApprovedGate, FakeNotifier, NoopEventSink};
pub use host_agent::{
    AgentCapabilityState, CapabilitySnapshot, HelloClientInfo, HostAgentClient,
    HostAgentClientError, HostAgentEvent,
};
pub use lane_view::{LaneView, LaneViewSnapshot, MAX_VIEW_MESSAGES};
pub use machine::{FixedPort, ForwardError, MachineForward, SshForward};
pub use roost::{
    launch_argv, roost_capabilities, shed_reach_entry, tab_close, tab_dump, tab_open,
    BootstrapRunner, HooksRefresh, LabelledPort, LocalSession, ReachError, ReachKind,
    RecordedReach, RoostEndpoint, RoostPeek, RoostReach, RoostUpdate, RoostWatcher,
    RoostWatcherOptions, SshBridge, SshBridgeOptions, SshExec, SystemSshTunnels, Tunnel,
    TunnelOpener, UnreachableReach,
};
pub use token_minter::HostAgentTokenMinter;
pub use traits::{
    AuthGate, AuthGateRef, AuthOutcome, AuthPrompt, Clock, ClockRef, CoordinatorEvent, EventSink,
    EventSinkRef, Notifier, NotifierRef, PostedNotification, Responder, ResponderRef, SystemClock,
};
