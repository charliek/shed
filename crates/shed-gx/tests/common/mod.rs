//! Shared scaffolding for the `FakeGx`-driven integration tests.
//!
//! Two rules hold everywhere in here:
//!
//! - **Every wait is BOUNDED and names what it was waiting for**, so a
//!   regression fails with a sentence instead of hanging until the harness
//!   timeout.
//! - **No test ever waits out a real window.** [`fast`] scales
//!   [`GxTimings`] to the tens of milliseconds, and the watcher's backoff curve
//!   is derived from `down_after` precisely so that scaling it scales the whole
//!   escalation ladder with it. A suite that slept out the real 30-second resume
//!   window would take half an hour and nobody would run it.

// Each test file uses a SUBSET of this module, including of its re-exports, so
// both an unused item and an unused `pub use` are ordinary here.
#![allow(dead_code, unused_imports)]

use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::{AgentLane, LaneApproval, LaneError, LaneEvent, LaneSession, LaneStop};
use shed_core::rc::RcFeedMessage;
use shed_gx::discovery::{GxCredentialSource, GxDiscovery, GxToken, StaticCredentials};
use shed_gx::testing::{FakeGx, DEFAULT_INSTANCE_ID, SENTINEL_TOKEN};
use shed_gx::transport::{FixedDial, GxTransport};
use shed_gx::{GxClient, GxTimings};
use tokio::sync::mpsc::Receiver;

/// The dial counter lives in `shed_gx::testing` — the unit tests need it too —
/// and is re-exported here so a test file names it the same way as everything
/// else in this module.
pub use shed_gx::testing::CountingDial;

/// The session every test drives. A real gx id shape (a UUIDv7), because the
/// fold splits an event id at the LAST hyphen and a toy id would not exercise
/// that.
pub const SID: &str = "01a0fa1e-0000-7000-8000-0000000000ab";

/// How long any single wait may take. Generous next to a loopback round trip
/// and the scaled backoff; short enough that a wedged test fails rather than
/// hangs.
pub const DEADLINE: Duration = Duration::from_secs(10);

/// The windows, scaled so a suite runs in seconds.
///
/// `resume_tries` is left at the real 3 — it is the number the escalation
/// ladder is specified in, and scaling it would test a different ladder.
///
/// The numbers are as small as they can be and no smaller. `cargo test` runs
/// these files' tests in PARALLEL, so a scheduling delay of a couple of hundred
/// milliseconds is ordinary; a `stall` under that would fire in the quiet part
/// of an unrelated test and turn every suite run into a coin flip. Two seconds
/// is comfortably past the worst case and still 15× faster than the real
/// window.
pub fn fast() -> GxTimings {
    GxTimings {
        stall: Duration::from_millis(2_000),
        resume_window: Duration::from_millis(2_000),
        resume_tries: 3,
        flush_after: Duration::from_millis(300),
        seed_limit: 500,
        rest_cap: 8 << 20,
        down_after: Duration::from_millis(4_000),
    }
}

// ---------------------------------------------------------------------------
// clients
// ---------------------------------------------------------------------------

pub fn client_for(fake: &FakeGx, timings: GxTimings) -> GxClient {
    client_with(
        fake,
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::new(
            StaticCredentials::from_parts(SENTINEL_TOKEN, DEFAULT_INSTANCE_ID)
                .expect("the sentinel token parses"),
        ),
        timings,
    )
}

pub fn client_with(
    fake: &FakeGx,
    transport: Arc<dyn GxTransport>,
    credentials: Arc<dyn GxCredentialSource>,
    timings: GxTimings,
) -> GxClient {
    GxClient::new(fake.reported_url(), transport, credentials, timings)
        .expect("the gx client builds")
}

/// A credential source that re-reads the fake's CURRENT instance id every time
/// — the local reader's behaviour after a leader rewrote its record.
///
/// The token never changes, because gx's is per-`$GROK_HOME` and a leader
/// restart does not mint a new one. That asymmetry is the whole reason the pin
/// exists.
pub struct FollowingCreds {
    fake: Arc<FakeGx>,
    calls: std::sync::atomic::AtomicUsize,
}

impl FollowingCreds {
    pub fn new(fake: Arc<FakeGx>) -> Arc<FollowingCreds> {
        Arc::new(FollowingCreds {
            fake,
            calls: std::sync::atomic::AtomicUsize::new(0),
        })
    }

    pub fn calls(&self) -> usize {
        self.calls.load(std::sync::atomic::Ordering::SeqCst)
    }
}

#[async_trait::async_trait]
impl GxCredentialSource for FollowingCreds {
    async fn discover(&self, _reported_url: &str) -> Result<GxDiscovery, LaneError> {
        self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        Ok(GxDiscovery {
            token: GxToken::parse(&self.fake.token()).expect("the fake's token parses"),
            instance_id: self.fake.instance_id(),
        })
    }
}

// ---------------------------------------------------------------------------
// subscribing
// ---------------------------------------------------------------------------

/// Subscribe and take the result apart.
///
/// `subscribe` never fails (the contract's whole answer to "the agent is not
/// up" is a subscription that says `Down`), so the `expect` is not a judgement
/// call a test should be re-making 20 times. Both halves are returned because
/// `LaneSubscription::into_parts`' doc is explicit that dropping the `LaneStop`
/// kills the pump — a caller still has to bind it.
pub async fn subscribed(
    lane: &GxClient,
    id: &str,
    cursor: Option<String>,
) -> (Receiver<LaneEvent>, LaneStop) {
    lane.subscribe(id, cursor)
        .await
        .expect("subscribe never fails")
        .into_parts()
}

/// Cut every open SSE stream and wait until the fake has actually released
/// them.
///
/// The wait is the point: `close_streams` only asks, and a test that asserted
/// on the reconnect before the old stream was gone would be racing the fake.
pub async fn drop_streams(fake: &FakeGx) {
    fake.close_streams();
    until(|| fake.stream_count() == 0, "the stream to be released").await;
}

// ---------------------------------------------------------------------------
// waiting
// ---------------------------------------------------------------------------

/// The next frame, or a failure naming what was being waited for.
pub async fn next_event(rx: &mut Receiver<LaneEvent>, what: &str) -> LaneEvent {
    match tokio::time::timeout(DEADLINE, rx.recv()).await {
        Err(_) => panic!("timed out after {DEADLINE:?} waiting for {what}"),
        Ok(None) => panic!("the lane stream ENDED while waiting for {what}"),
        Ok(Some(ev)) => ev,
    }
}

/// Every frame up to and including the first one `done` accepts.
///
/// The four `until_*` waits below are this loop under four names — the names are
/// what a call site reads, the loop is written once.
pub async fn until_frame<F: FnMut(&LaneEvent) -> bool>(
    rx: &mut Receiver<LaneEvent>,
    what: &str,
    mut done: F,
) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    loop {
        let ev = next_event(rx, what).await;
        let hit = done(&ev);
        out.push(ev);
        if hit {
            return out;
        }
    }
}

/// Every frame up to and including the next `Ready` (or a terminal `Down`).
pub async fn until_ready(rx: &mut Receiver<LaneEvent>) -> Vec<LaneEvent> {
    until_frame(rx, "the seed's Ready", |ev| {
        matches!(ev, LaneEvent::Ready { .. } | LaneEvent::Down { .. })
    })
    .await
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

/// Every frame up to and including the terminal `Down`.
pub async fn until_down(rx: &mut Receiver<LaneEvent>) -> Vec<LaneEvent> {
    until_frame(rx, "the terminal Down", |ev| {
        matches!(ev, LaneEvent::Down { .. })
    })
    .await
}

/// Every frame up to and including the one whose transcript text contains
/// `sentinel` — "everything the adapter had to say, in order, up to here".
pub async fn until_text(rx: &mut Receiver<LaneEvent>, sentinel: &str) -> Vec<LaneEvent> {
    until_frame(rx, &format!("the sentinel row {sentinel:?}"), |ev| {
        text_of(ev).is_some_and(|t| t.contains(sentinel))
    })
    .await
}

/// Every frame up to and including the first `Approval` for `id`.
pub async fn until_approval(rx: &mut Receiver<LaneEvent>, id: &str) -> Vec<LaneEvent> {
    until_frame(
        rx,
        &format!("an Approval frame for {id}"),
        |ev| matches!(ev, LaneEvent::Approval { approval } if approval.id == id),
    )
    .await
}

/// Poll `probe` until it answers `true`, or fail naming what was expected.
pub async fn until<F: FnMut() -> bool>(mut probe: F, what: &str) {
    let deadline = std::time::Instant::now() + DEADLINE;
    while std::time::Instant::now() < deadline {
        if probe() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    panic!("timed out after {DEADLINE:?} waiting for {what}");
}

// ---------------------------------------------------------------------------
// reading frames
// ---------------------------------------------------------------------------

pub fn text_of(ev: &LaneEvent) -> Option<&str> {
    match ev {
        LaneEvent::Message { message, .. } => message.text.as_deref(),
        _ => None,
    }
}

pub fn messages(events: &[LaneEvent]) -> Vec<RcFeedMessage> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Message { message, .. } => Some(message.clone()),
            _ => None,
        })
        .collect()
}

pub fn texts(events: &[LaneEvent]) -> Vec<String> {
    messages(events)
        .iter()
        .filter_map(|m| m.text.clone())
        .collect()
}

pub fn cursors(events: &[LaneEvent]) -> Vec<Option<String>> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Message { cursor, .. } => Some(cursor.clone()),
            _ => None,
        })
        .collect()
}

pub fn sessions(events: &[LaneEvent]) -> Vec<LaneSession> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Session { session } => Some(session.clone()),
            _ => None,
        })
        .collect()
}

pub fn approvals(events: &[LaneEvent]) -> Vec<LaneApproval> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Approval { approval } => Some(approval.clone()),
            _ => None,
        })
        .collect()
}

pub fn resets(events: &[LaneEvent]) -> Vec<(String, u64)> {
    events
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Reset { reason, generation } => Some((reason.clone(), *generation)),
            _ => None,
        })
        .collect()
}

pub fn down_reason(events: &[LaneEvent]) -> Option<String> {
    events.iter().find_map(|e| match e {
        LaneEvent::Down { reason } => Some(reason.clone()),
        _ => None,
    })
}

/// A one-word label per frame, so a failure prints the ORDER rather than a
/// page of DTOs.
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

// ---------------------------------------------------------------------------
// wire fixtures
// ---------------------------------------------------------------------------

pub fn eid(n: u64) -> String {
    format!("{SID}-{n}")
}

/// One `session/update` chunk. `prompt` is `_meta.promptId`, absent when
/// `None` — which the fold reads as "no change", not "a different prompt".
pub fn chunk(n: u64, kind: &str, text: &str, prompt: Option<&str>) -> Value {
    let mut meta = json!({ "agentTimestampMs": 1_788_931_000_000i64 + (n as i64) * 1_000 });
    if let Some(p) = prompt {
        meta["promptId"] = json!(p);
    }
    json!({
        "eventId": eid(n),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": kind,
                "content": { "type": "text", "text": text },
            },
            "_meta": meta,
        },
    })
}

/// gx's own `turn_completed` extension — the frame that closes a streak.
pub fn turn_completed(n: u64, stop_reason: &str) -> Value {
    json!({
        "eventId": eid(n),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "turn_completed", "stop_reason": stop_reason },
            "_meta": { "agentTimestampMs": 1_788_931_000_000i64 + (n as i64) * 1_000 },
        },
    })
}

/// A `hook_execution` — one of the ignored kinds, which is transparent to a
/// streak and still advances the cursor.
pub fn hook(n: u64) -> Value {
    json!({
        "eventId": eid(n),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "hook_execution", "event_name": "post_tool_use" },
        },
    })
}

/// A permission whose five options are exactly the shape a real gx serves —
/// **opaque ids**, and **two of them declaring `allow_once`**.
///
/// The ids and kinds are copied from a live gx 1.0.16+gx.12 permission
/// (`fixtures/1.0.16+gx.12/approvals.jsonl` holds the recording). The two
/// `allow_once` options are the load-bearing part: one is "yes, proceed" and
/// the other turns prompting off for the whole session, so a three-valued
/// `LaneAnswer::Permission{AllowOnce}` genuinely cannot say which the human
/// meant — which is why the adapter refuses it instead of guessing, and why a
/// four-option fixture would quietly stop testing that.
pub fn permission_request(command: &str) -> Value {
    json!({
        "sessionId": SID,
        "toolCall": {
            "toolCallId": "call_fixture",
            "kind": "execute",
            "title": format!("Execute `{command}`"),
            "rawInput": { "command": command, "description": "a fixture command" },
        },
        "options": [
            { "optionId": "enable-always-approve", "name": "Yes, and don't ask again", "kind": "allow_once" },
            { "optionId": "allow-always-command", "name": format!("Always allow: {command}"), "kind": "allow_always" },
            { "optionId": "allow-once", "name": "Yes, proceed", "kind": "allow_once" },
            { "optionId": "reject-once", "name": "No", "kind": "reject_once" },
            { "optionId": "reject-always-command", "name": format!("Never allow: {command}"), "kind": "reject_always" },
        ],
    })
}
