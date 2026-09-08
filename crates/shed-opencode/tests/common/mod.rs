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
use tokio::sync::mpsc::UnboundedReceiver;

/// How long any single wait may take. Generous next to a loopback round trip
/// and the watcher's 100 ms backoff floor; short enough that a wedged test
/// fails rather than hangs.
pub const DEADLINE: Duration = Duration::from_secs(5);

/// The next frame, or a failure naming what was being waited for.
pub async fn next_event(rx: &mut UnboundedReceiver<LaneEvent>, what: &str) -> LaneEvent {
    match tokio::time::timeout(DEADLINE, rx.recv()).await {
        Err(_) => panic!("timed out after {DEADLINE:?} waiting for {what}"),
        Ok(None) => panic!("the lane stream ENDED while waiting for {what}"),
        Ok(Some(ev)) => ev,
    }
}

/// Every frame up to and including the next `Ready` (or a terminal `Down`).
pub async fn until_ready(rx: &mut UnboundedReceiver<LaneEvent>) -> Vec<LaneEvent> {
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

/// Every frame up to and including the one whose transcript text contains
/// `sentinel` — the way a test says "everything the adapter had to say, in
/// order, up to here".
pub async fn until_text(rx: &mut UnboundedReceiver<LaneEvent>, sentinel: &str) -> Vec<LaneEvent> {
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
