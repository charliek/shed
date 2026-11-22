//! **Toolkit-free shed-only desktop runtime** (plan 038 D1/D2/D6).
//!
//! Owns the desktop app's shed-only roost/craze/lane/slash/shutdown runtime
//! without pulling in any UI toolkit (`tauri`, `wry`, `tao`, `webkit2gtk`,
//! `iced`), credential broker (`shed-broker`), or direct `roost-ipc`
//! dependency.
//!
//! ## Wiring
//!
//! Construct a [`ShedDesktopRuntime`] with a Tokio runtime handle, a
//! [`ShedConfig`], [`ShedReachOptions`], [`HostEvents`], and [`LaneEvents`].
//! `ShedDesktopRuntime` wires generation-fenced Craze notifications internally
//! from [`ShedHosts`] to [`ShedLanes`] without ever nesting mutexes between the
//! two registries:
//!
//! * `ShedHosts` lock order: `state -> armed -> reg -> execs -> retired`;
//!   `reg -> probes`. Releases ALL locks before invoking `HostEvents` or the
//!   Craze observer.
//! * `ShedLanes` lock order: `inner -> core.state -> core.view`. Never calls
//!   into `ShedHosts` while holding any lock.

pub mod id;
pub mod shed_craze;
pub mod shed_hosts;
pub mod shed_lanes;
pub mod shutdown;
pub mod slash;

use std::sync::Arc;

use serde_json::{json, Value};

use shed_app::{Backend, Reachability};
use shed_core::config::ShedConfig;
use shed_roost::{roost_capabilities, SpawnGate, TeardownSink};

pub use crate::id::{ShedId, SHED_PREFIX};
pub use crate::shed_craze::{
    build_ssh_craze, build_ssh_reach, create_request, held_create, reopen_id, CrazeFailure,
    CrazePhase, CrazeProbe, CrazeReach, CrazeState, CrazeTimings, DialMode, ShedReachOptions,
    SshCrazeDial, SshCrazeProbe, CRAZE_TOO_OLD,
};
pub use crate::shed_hosts::{
    failure_json, installed_json, HostEvents, HostFilter, HostSnapshot, InjectedRow, JoinedClients,
    NoopHostEvents, OnCrazeNews, Refusal, ShedHosts, CLIENT_LABEL, SHED_HOME,
};
pub use crate::shed_lanes::{
    parse_answer, parse_mode, parse_setting, renamed, CrazeNews, Departure, LaneEvents,
    LaneFailure, LaneHosts, LaneUiEvent, LanesClosed, NoopLaneEvents, ShedLanes,
    CRAZE_DEPART_GRACE, CRAZE_KIND, DOWN_HOST_REMOVED, DOWN_HUB_GONE, DOWN_SESSION_GONE,
};
pub use crate::shutdown::{
    shutdown_children, BrokerDrain, Report as ShutdownReport, Shutdown, CHILDREN_KILL_WAIT,
    CLIENTS_JOIN_WAIT, ESCAPE, GATE_CLOSE_WAIT,
};

/// Top-level toolkit-free desktop runtime owning [`ShedHosts`], [`ShedLanes`],
/// [`SpawnGate`], and its single [`Shutdown`] coordinator, with internal
/// generation-fenced wiring between hosts and lanes.
pub struct ShedDesktopRuntime {
    hosts: Arc<ShedHosts>,
    lanes: Arc<ShedLanes>,
    gate: SpawnGate,
    handle: tokio::runtime::Handle,
    shutdown: Arc<Shutdown>,
}

impl ShedDesktopRuntime {
    /// Start a new shed-only desktop runtime.
    ///
    /// If `reach_options.spawn_gate` is `None`, a fresh [`SpawnGate`] is
    /// created and shared across all transports and shutdown phases.
    /// If `reach_options.teardowns` is `None`, a shared [`TeardownSink`] is
    /// installed so roost reach teardowns are joined during shutdown.
    pub fn start(
        handle: tokio::runtime::Handle,
        config: &ShedConfig,
        mut reach_options: ShedReachOptions,
        jail_fs_root: bool,
        host_events: Arc<dyn HostEvents>,
        lane_events: Arc<dyn LaneEvents>,
    ) -> Self {
        let gate = reach_options
            .spawn_gate
            .get_or_insert_with(SpawnGate::new)
            .clone();
        reach_options
            .teardowns
            .get_or_insert_with(TeardownSink::default);

        let hosts = Arc::new(ShedHosts::start(
            &handle,
            config,
            reach_options,
            jail_fs_root,
            host_events,
        ));
        let lanes = Arc::new(ShedLanes::new(
            handle.clone(),
            lane_events,
            Arc::clone(&hosts) as Arc<dyn LaneHosts>,
        ));
        wire_craze_observer(&hosts, &lanes);
        let shutdown = Shutdown::new(
            gate.clone(),
            Arc::clone(&hosts),
            Arc::clone(&lanes),
            None,
            handle.clone(),
        );

        Self {
            hosts,
            lanes,
            gate,
            handle,
            shutdown,
        }
    }

    /// Convenience constructor with no-op event sinks (useful for headless
    /// drivers and unit tests).
    pub fn start_headless(
        handle: tokio::runtime::Handle,
        config: &ShedConfig,
        reach_options: ShedReachOptions,
        jail_fs_root: bool,
    ) -> Self {
        Self::start(
            handle,
            config,
            reach_options,
            jail_fs_root,
            Arc::new(NoopHostEvents),
            Arc::new(NoopLaneEvents),
        )
    }

    pub fn hosts(&self) -> &Arc<ShedHosts> {
        &self.hosts
    }

    pub fn lanes(&self) -> &Arc<ShedLanes> {
        &self.lanes
    }

    pub fn spawn_gate(&self) -> &SpawnGate {
        &self.gate
    }

    pub fn handle(&self) -> &tokio::runtime::Handle {
        &self.handle
    }

    /// Register an optional broker drain hook on this runtime's single
    /// [`Shutdown`] coordinator (before shutdown begins).
    pub fn set_broker_drain(&self, broker: BrokerDrain) {
        self.shutdown.set_broker_drain(broker);
    }

    /// Return this runtime's single, idempotent [`Shutdown`] coordinator.
    ///
    /// Every caller shares the same [`Shutdown`] instance, guaranteeing
    /// once-only shutdown across the entire runtime.
    pub fn shutdown_coordinator(&self) -> Arc<Shutdown> {
        Arc::clone(&self.shutdown)
    }

    /// Run this runtime's single orderly shutdown sequence and return its
    /// [`ShutdownReport`].
    pub async fn shutdown_children(&self) -> ShutdownReport {
        self.shutdown.run().await
    }
}

/// Wire `hosts`'s Craze news into `lanes` via a `Weak<ShedLanes>` so there is
/// no reference cycle and late notifications from replaced generations are
/// fenced inside [`ShedLanes::craze_news`].
pub fn wire_craze_observer(hosts: &ShedHosts, lanes: &Arc<ShedLanes>) {
    let weak = Arc::downgrade(lanes);
    hosts.set_craze_observer(Arc::new(move |shed, gen, news| {
        if let Some(lanes) = weak.upgrade() {
            lanes.craze_news(shed, gen, news);
        }
    }));
}

/// Feed a [`Reachability`] snapshot from `shed_app::Backend` into
/// [`ShedHosts::observe_sheds_at`] under a pre-stamped sequence `seq`
/// (obtained via [`ShedHosts::next_observe_seq`] BEFORE calling
/// `backend.refresh().await`).
///
/// Only `running` sheds are promoted to `listed_sheds`; every server that did
/// not report an error in `reachability.host_errors` is passed as authoritative
/// so stopped/deleted sheds on those servers are removed. An observation whose
/// `seq` is older than an already-applied observation is ignored so a delayed
/// refresh cannot reauthorize a removed shed.
pub async fn observe_reachability_at(hosts: &ShedHosts, seq: u64, reachability: &Reachability) {
    use shed_core::models::ShedStatus;

    let failed: std::collections::HashSet<&str> = reachability
        .host_errors
        .iter()
        .map(|e| e.server.as_str())
        .collect();
    let running: Vec<ShedId> = reachability
        .sheds
        .iter()
        .filter(|s| s.status == ShedStatus::Running)
        .filter_map(|s| ShedId::new(&s.host, &s.name).ok())
        .collect();
    let answered: Vec<String> = reachability
        .sheds
        .iter()
        .map(|s| s.host.clone())
        .chain(hosts.servers())
        .filter(|server| !failed.contains(server.as_str()))
        .collect();
    hosts.observe_sheds_at(seq, &running, &answered).await;
}

/// Convenience wrapper for [`observe_reachability_at`] stamping a fresh
/// sequence number now. Callers that race with other refreshes should stamp
/// [`ShedHosts::next_observe_seq`] BEFORE `backend.refresh().await` and call
/// [`observe_reachability_at`].
pub async fn observe_reachability(hosts: &ShedHosts, reachability: &Reachability) {
    let seq = hosts.next_observe_seq();
    observe_reachability_at(hosts, seq, reachability).await;
}

/// Build the `rc.list` JSON payload:
/// `{ sessions, capabilities, hosts, shed_craze }`.
///
/// Stamps a sequence number BEFORE querying `backend.rc_targets`, updates
/// `hosts` (add-only, fenced by `seq` so a delayed `rc.list` cannot reauthorize
/// a shed removed by a newer refresh), and reads a consistent [`HostSnapshot`].
pub async fn rc_list_payload(
    backend: &Backend,
    hosts: &ShedHosts,
    host: Option<&str>,
    shed: Option<&str>,
) -> Value {
    let seq = hosts.next_observe_seq();
    let targets = backend.rc_targets(host, shed).await;
    let running: Vec<ShedId> = targets
        .iter()
        .filter_map(|(s, target)| ShedId::new(&target.server_name, &s.name).ok())
        .collect();
    hosts.observe_sheds_at(seq, &running, &[]).await;

    let snap = hosts.snapshot(HostFilter { server: host, shed });
    let capabilities: serde_json::Map<String, Value> = snap
        .hosts
        .iter()
        .filter_map(|h| h.get("origin").and_then(Value::as_str))
        .map(|origin| (origin.to_string(), json!(roost_capabilities())))
        .collect();
    json!({
        "sessions": snap.sessions,
        "capabilities": Value::Object(capabilities),
        "hosts": snap.hosts,
        "shed_craze": snap.shed_craze,
    })
}
