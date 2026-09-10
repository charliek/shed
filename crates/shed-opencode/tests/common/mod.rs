//! Shared scaffolding for the `FakeOpencode`-driven integration tests.
//!
//! Every wait here is BOUNDED and names what it was waiting for, so a
//! regression fails with a sentence instead of hanging until the harness
//! timeout.
#![allow(dead_code)] // each test file uses a subset

use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::LaneEvent;
use shed_opencode::testing::FakeOpencode;
use tokio::sync::mpsc::Receiver;

/// How long any single wait may take. Generous next to a loopback round trip
/// and the watcher's 100 ms backoff floor; short enough that a wedged test
/// fails rather than hangs.
pub const DEADLINE: Duration = Duration::from_secs(5);

/// The next frame, or a failure naming what was being waited for.
pub async fn next_event(rx: &mut Receiver<LaneEvent>, what: &str) -> LaneEvent {
    match tokio::time::timeout(DEADLINE, rx.recv()).await {
        Err(_) => panic!("timed out after {DEADLINE:?} waiting for {what}"),
        Ok(None) => panic!("the lane stream ENDED while waiting for {what}"),
        Ok(Some(ev)) => ev,
    }
}

/// Every frame up to and including the next `Ready` (or a terminal `Down`).
pub async fn until_ready(rx: &mut Receiver<LaneEvent>) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    loop {
        let ev = next_event(rx, "the seed's Ready").await;
        let done = matches!(ev, LaneEvent::Ready { .. } | LaneEvent::Down { .. });
        out.push(ev);
        if done {
            return out;
        }
    }
}

/// Everything the adapter has ALREADY queued, without waiting for a frame that
/// may never come.
///
/// The overflow tests need exactly this. A lagged generation is abandoned with
/// no `Ready`, so [`until_ready`] would sit there until its deadline — and the
/// assertion under test is about what the client is holding BEFORE it drains,
/// which is a snapshot, not a wait.
pub fn drain_now(rx: &mut Receiver<LaneEvent>) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    while let Ok(ev) = rx.try_recv() {
        out.push(ev);
    }
    out
}

/// Everything already queued, PLUS everything up to the next `Ready`.
///
/// After a lag the queue still holds the abandoned generation's frames, and a
/// bare [`until_ready`] would read them as a seed that never finished. This is
/// the shape a client actually sees: the leftovers, then the reseed's bracket.
pub async fn drain_until_ready(rx: &mut Receiver<LaneEvent>) -> Vec<LaneEvent> {
    let mut out = drain_now(rx);
    out.extend(until_ready(rx).await);
    out
}

/// A one-word label per frame, so a failure prints the ORDER rather than a page
/// of DTOs.
pub fn shape(events: &[LaneEvent]) -> Vec<&'static str> {
    events
        .iter()
        .map(|e| match e {
            LaneEvent::Reset { .. } => "reset",
            LaneEvent::Ready { .. } => "ready",
            LaneEvent::Message { .. } => "message",
            LaneEvent::Session { .. } => "session",
            LaneEvent::Approval { .. } => "approval",
            LaneEvent::Down { .. } => "down",
            LaneEvent::Unknown => "unknown",
        })
        .collect()
}

/// The first 8 labels from [`shape`] — enough for a failure message to show
/// the order without dumping a whole abandoned/staged generation into it.
pub fn shape_head(events: &[LaneEvent]) -> Vec<&'static str> {
    let full = shape(events);
    full[..8.min(full.len())].to_vec()
}

/// Every `Reset`'s `(reason, generation)`, in order.
pub fn resets(events: &[LaneEvent]) -> Vec<(String, u64)> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Reset { reason, generation } => Some((reason.clone(), *generation)),
            _ => None,
        })
        .collect()
}

/// Every frame up to and including the one whose transcript text contains
/// `sentinel` — the way a test says "everything the adapter had to say, in
/// order, up to here".
pub async fn until_text(rx: &mut Receiver<LaneEvent>, sentinel: &str) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    loop {
        let ev = next_event(rx, &format!("the sentinel row {sentinel:?}")).await;
        let hit = message_text(&ev).is_some_and(|t| t.contains(sentinel));
        out.push(ev);
        if hit {
            return out;
        }
    }
}

/// A frame's transcript text, if it is a transcript row.
pub fn message_text(ev: &LaneEvent) -> Option<&str> {
    match ev {
        LaneEvent::Message { message, .. } => message.text.as_deref(),
        _ => None,
    }
}

/// Every transcript row's text, in order.
pub fn texts(events: &[LaneEvent]) -> Vec<String> {
    events
        .iter()
        .filter_map(|e| message_text(e).map(str::to_string))
        .collect()
}

/// Every transcript row's seq, in order.
pub fn seqs(events: &[LaneEvent]) -> Vec<u64> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Message { message, .. } => Some(message.seq),
            _ => None,
        })
        .collect()
}

/// Every approval id an `Approval` frame carried, in order.
pub fn approval_ids(events: &[LaneEvent]) -> Vec<String> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Approval { approval } => Some(approval.id.clone()),
            _ => None,
        })
        .collect()
}

/// Poll `cond` until it holds, or fail naming what never happened. Used for the
/// handful of conditions that are observable only on the FAKE's side (a request
/// arriving, a socket closing) and so cannot be awaited on the event stream.
pub async fn wait_for(what: &str, mut cond: impl FnMut() -> bool) {
    let deadline = tokio::time::Instant::now() + DEADLINE;
    while tokio::time::Instant::now() < deadline {
        if cond() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(2)).await;
    }
    panic!("timed out after {DEADLINE:?} waiting for {what}");
}

/// The suite-wide invariant: the adapter never addressed a session or a request
/// it was not entitled to.
pub fn assert_clean(fake: &FakeOpencode) {
    let violations = fake.violations();
    assert!(
        violations.is_empty(),
        "the pin guard recorded {} violation(s): {violations:#?}",
        violations.len()
    );
}

/// A finished assistant turn as opencode streams it: the message, then a text
/// part with an end time (the fold's terminal condition, so the row emits
/// immediately rather than waiting for completion).
pub fn assistant_turn(session: &str, msg: &str, part: &str, text: &str) -> Vec<Value> {
    vec![
        json!({
            "type": "message.updated",
            "properties": {
                "sessionID": session,
                "info": { "id": msg, "role": "assistant",
                          "time": { "created": 1_700_000_010_000i64, "completed": 1_700_000_011_000i64 } },
            },
        }),
        json!({
            "type": "message.part.updated",
            "properties": {
                "sessionID": session,
                "part": { "id": part, "messageID": msg, "type": "text", "text": text,
                          "time": { "start": 1_700_000_010_000i64, "end": 1_700_000_011_000i64 } },
            },
        }),
    ]
}

/// A `session.error`, which the fold turns into a display-only status row
/// straight away — the cheapest deterministic "one row, now" marker there is.
pub fn marker(session: &str, text: &str) -> Value {
    json!({
        "type": "session.error",
        "properties": {
            "sessionID": session,
            "error": { "name": "MarkerError", "data": { "message": text } },
        },
    })
}

/// A permission ask on the wire.
pub fn permission_asked(session: &str, id: &str, command: &str) -> Value {
    json!({
        "type": "permission.asked",
        "properties": {
            "sessionID": session,
            "id": id,
            "permission": "bash",
            "patterns": [command],
            "metadata": { "command": command },
        },
    })
}
