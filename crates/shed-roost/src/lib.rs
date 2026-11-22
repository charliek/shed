//! shed-roost — shed's roost-session wire client, reach/watcher layer,
//! bootstrap state machines, SSH endpoint helpers, and orderly-shutdown
//! [`SpawnGate`].
//!
//! This crate is the **sole direct consumer of `roost-ipc`** in `crates/`, so
//! `shed-core`, `shed-lane`, `shed-craze`, `shed-app`, and `shed-desktop-core`
//! stay completely isolated from `roost-ipc`.

pub mod bootstrap;
pub mod conn;
mod error;
pub mod fence;
pub mod model;
pub mod paths;
pub mod reach;
pub mod spawn_gate;
pub mod ssh_endpoint;
#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use conn::{Conn, RoostEndpoint, RoostEventStream};
pub use error::RoostError;
pub use fence::{Admit, Fence};
pub use model::{
    launch_argv, rfc3339_z, roost_capabilities, roost_kind_features, roost_source, AgentLaneStamp,
    RoostInventory, RoostSession, APPROVAL_DETAILS,
};
pub use paths::{local_session_socket, ResolvedSocket};
pub use reach::{
    shed_reach_entry, tab_close, tab_dump, tab_open, tab_open_command_fenced, tab_open_fenced,
    tab_set_title, BootstrapRunner, HooksRefresh, LabelledPort, LocalSession, OpenedTabInfo,
    ReachError, ReachKind, RecordedReach, RoostPeek, RoostReach, RoostUpdate, RoostWatcher,
    RoostWatcherOptions, SshBridge, SshBridgeOptions, SshExec, SystemSshTunnels, TabFence,
    TeardownSink, Tunnel, TunnelOpener, UnreachableReach,
};
pub use spawn_gate::{
    ChildExit, Drained, LateRegistrations, ShuttingDown, SlotClose, SlotsClosed, SpawnGate,
    SpawnSection, LATE_KILL_WAIT, SHUTTING_DOWN,
};
pub use ssh_endpoint::{
    check_identity, ssh_argv, user_at_host, FixedPort, ForwardError, MachineForward, SshEndpoint,
    CONNECT_TIMEOUT_SECS,
};

/// Decode the `result` of a recorded roost response vector — the shape every
/// vector under `crates/fixtures/roost-vectors/` has (a full JSON-RPC reply,
/// never a bare payload). Shared by this crate's test suites.
#[cfg(test)]
pub(crate) fn result_of<T: serde::de::DeserializeOwned>(vector: &str) -> T {
    let envelope: serde_json::Value = serde_json::from_str(vector).expect("a vector is valid JSON");
    serde_json::from_value(envelope["result"].clone()).expect("the vector's result decodes")
}
