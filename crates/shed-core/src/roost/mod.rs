//! The **roost client** — shed's read/drive path onto a `roost-session`.
//!
//! Part of the **Roost Pivot** epic (`epics/roost-pivot.md`, slice S1). A
//! roost-session is the substrate shed's clients are moving to: it owns the
//! terminal, the agent adapters report into it, and it is the thing that knows
//! what an agent is doing right now. This module is how `shed-core` talks to
//! one.
//!
//! ## The rule
//!
//! **Read status from roost; never derive it.** roost already carries the four
//! agent axes (`shell_state`, `agent_lifecycle`, `ownership`, and the sticky
//! `has_notification`) as first-class wire fields, written by the agent adapters
//! themselves. Shed's job is to *read* them and render them. Every attempt to
//! re-derive activity from terminal output — pane scraping — is banned by the
//! epic, and this module is the reason it no longer has an excuse to exist.
//!
//! ## Layout
//!
//! * [`conn`] — [`Conn`], a thin typed skin over roost's own
//!   [`roost_ipc::IpcClient`]. It re-implements **no** request/response
//!   machinery: id matching, error mapping and every future `IpcClient` fix come
//!   from the pinned rev. It adds two things `IpcClient` has no opinion about —
//!   a loopback-TCP transport (for a client whose reach is a forwarded port) and
//!   the compatibility gate on `session.identify`.
//! * [`paths`] — where the local session socket is. Shed resolves this itself
//!   rather than calling `roost_ipc::paths::BundleProfile::session()`, because
//!   that resolver appends `-dev` based on the **consuming** crate's build
//!   profile: a debug build of shed would go looking for a dev roost.
//! * [`model`] — what a roost tab IS to shed: [`RoostSession`] (roost's four
//!   agent axes, flattened out of the project tree), [`RoostInventory`] (one
//!   `tab.list` + `session.identify`), and the mapping onto the
//!   [`RcSessionDto`](crate::rc::RcSessionDto) every client already renders —
//!   which gains no field for roost. Rows are **agent-owned tabs only**: a
//!   roost-session is somebody's terminal multiplexer, and fifteen shells must
//!   not be fifteen cards. It also synthesizes the capabilities a roost host
//!   advertises ([`roost_capabilities`]), since roost has no `shed-ext-rc` to
//!   probe.
//! * [`fence`] — the revision fence ([`Fence`], [`Admit`]) and the event fold
//!   ([`RoostInventory::apply`]). Pure, so it is implemented exactly once for
//!   every client shed ships and testable against roost's own golden vectors.
//! * [`testing`] — an in-process fake `roost-session`, for this crate's tests
//!   and (under the non-default `test-support` feature) for `shed-app`'s.

pub mod conn;
mod error;
pub mod fence;
pub mod model;
pub mod paths;
#[cfg(any(test, feature = "test-support"))]
pub mod testing;

pub use conn::{Conn, RoostEndpoint, RoostEventStream};
pub use error::RoostError;
pub use fence::{Admit, Fence};
pub use model::{
    launch_argv, loopback_base_url, rfc3339_z, roost_capabilities, AgentLaneStamp, RoostInventory,
    RoostSession, APPROVAL_DETAILS, GX_REMOTE_KEY, OPENCODE_SERVER_URL_KEY,
};
pub use paths::{local_session_socket, ResolvedSocket};

/// Decode the `result` of a recorded roost response vector — the shape every
/// vector under `crates/fixtures/roost-vectors/` has (a full JSON-RPC reply,
/// never a bare payload). Shared by this module's test suites.
#[cfg(test)]
fn result_of<T: serde::de::DeserializeOwned>(vector: &str) -> T {
    let envelope: serde_json::Value = serde_json::from_str(vector).expect("a vector is valid JSON");
    serde_json::from_value(envelope["result"].clone()).expect("the vector's result decodes")
}
