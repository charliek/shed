//! Shared scaffolding for the lane's scripted cells: a lane on a
//! [`ScriptedDial`], craze's end of each of its connections played line by
//! line, and the conformance kit over every frame.

#![allow(dead_code)]

use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::conformance::{drive_lane, Drive, DriveEnd, LaneChecker};
use shed_core::lane::{AgentLane, LaneEvent, LaneSession};
use shed_core::rc::RcActivity;
use shed_craze::testing::{
    attach_result, host_session_row, session_caps, session_info, snapshot_at, HubEnd, ScriptedDial,
    SCRIPT_WAIT,
};
use shed_craze::wire::ClientInfo;
use shed_craze::{CrazeLane, LaneTimings};
use tokio::sync::mpsc::{Receiver, UnboundedReceiver};

pub const HOST: &str = "0123456789ab";
pub const SID: &str = "session-fake-1";
pub const INC: &str = "INCARNATION-1";
pub const SUB: &str = "s-1";

/// The roster row a source would have opened the lane with.
pub fn roster_row() -> LaneSession {
    LaneSession {
        id: HOST.to_string(),
        title: "work".to_string(),
        cwd: "/work".to_string(),
        activity: RcActivity::Idle,
        ..LaneSession::default()
    }
}

pub fn info(stop: bool) -> Value {
    session_info(HOST, SID, INC, session_caps(stop))
}

pub fn row() -> Value {
    host_session_row(&info(false), json!({}))
}

/// Short timings for scripted cells: everything quick, the bounds pinned.
pub fn fast() -> LaneTimings {
    LaneTimings {
        backoff_base: Duration::from_millis(5),
        backoff_max: Duration::from_millis(20),
        wait_connected: Duration::from_millis(500),
        request: Duration::from_secs(5),
        ..LaneTimings::default()
    }
}

/// A lane opened by hostId, with the roster's row and session id.
pub fn lane(dial: Arc<ScriptedDial>, timings: LaneTimings) -> CrazeLane {
    CrazeLane::new(
        dial,
        ClientInfo::shed("shed-lane-test"),
        HOST,
        Some((roster_row(), SID.to_string())),
        timings,
    )
}

/// A dial, its connections, and a lane on it.
pub fn scripted(timings: LaneTimings) -> (Arc<ScriptedDial>, UnboundedReceiver<HubEnd>, CrazeLane) {
    let (dial, conns) = ScriptedDial::new();
    let lane = lane(Arc::clone(&dial), timings);
    (dial, conns, lane)
}

/// The next connection the lane dials.
pub async fn next_conn(conns: &mut UnboundedReceiver<HubEnd>) -> HubEnd {
    tokio::time::timeout(SCRIPT_WAIT, conns.recv())
        .await
        .expect("the lane dialled in time")
        .expect("the dial is alive")
}

/// A seeded connection: the splice, the row, and a no-cursor attach answered
/// with `main` cut at `seq`. Returns craze's end and the attach request.
pub async fn seed_conn(
    conns: &mut UnboundedReceiver<HubEnd>,
    sub: &str,
    seq: u64,
    main: Value,
) -> (HubEnd, Value) {
    let mut hub = next_conn(conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let req = hub
        .attached(attach_result(
            sub,
            &info(false),
            (INC, seq),
            Some(snapshot_at(INC, seq, main)),
            None,
        ))
        .await;
    (hub, req)
}

/// Read frames through the conformance kit until `until` matches.
pub async fn drive(
    rx: &mut Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    what: &str,
    until: impl FnMut(&LaneEvent) -> bool,
) -> Vec<LaneEvent> {
    drive_within(rx, checker, SCRIPT_WAIT, what, until).await
}

pub async fn drive_within(
    rx: &mut Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    within: Duration,
    what: &str,
    until: impl FnMut(&LaneEvent) -> bool,
) -> Vec<LaneEvent> {
    let d: Drive<LaneEvent> = drive_lane(rx, checker, within, until)
        .await
        .unwrap_or_else(|v| panic!("{what}: the stream broke a contract rule: {v}"));
    assert_eq!(
        d.end,
        DriveEnd::Matched,
        "{what}: not seen: {:#?}",
        d.frames
    );
    d.frames
}

pub fn is_ready(e: &LaneEvent) -> bool {
    matches!(e, LaneEvent::Ready { .. })
}

pub fn is_down(e: &LaneEvent) -> bool {
    matches!(e, LaneEvent::Down { .. })
}

/// The transcript texts among `frames`, in order.
pub fn texts(frames: &[LaneEvent]) -> Vec<String> {
    frames
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Message { message, .. } => Some(message.text.clone().unwrap_or_default()),
            _ => None,
        })
        .collect()
}

/// The `Reset`s among `frames`: `(reason, generation)`.
pub fn resets(frames: &[LaneEvent]) -> Vec<(String, u64)> {
    frames
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Reset { reason, generation } => Some((reason.clone(), *generation)),
            _ => None,
        })
        .collect()
}

/// A text event.
pub fn text(t: &str) -> Value {
    json!({"type": "text", "text": t, "at": "2026-01-01T00:00:00Z"})
}

/// A lane subscribed, its stream and the conformance checker.
pub async fn subscribed(lane: &CrazeLane) -> (Receiver<LaneEvent>, shed_core::lane::LaneStop) {
    lane.subscribe(None).await.unwrap().into_parts()
}
