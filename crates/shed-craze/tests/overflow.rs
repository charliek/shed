//! The client channel is BOUNDED, and a client that stops reading gets a
//! reseed — `shed_core::lane`'s module-doc correction 13, from the craze lane's
//! side. Ported from shed-gx's `tests/overflow.rs` (at `203250f`, deleted by
//! plan 025 C1) onto a scripted craze host: the same cases, the same claims.
//!
//! Where the dropped frame falls is what the cases differ in:
//!
//! 1. in steady state, after a generation reached its `Ready`;
//! 2. inside a reseed, before its `Ready` (a seed abandoned, never left
//!    staged);
//! 3. on the SESSION row, and on an APPROVAL frame — so a lag swallowed by any
//!    one emitter fails a cell;
//! 4. twice running, which climbs the backoff and keeps the bracket;
//! 5. at the end: `Down` is the one frame never dropped (`publish_final`).
//!
//! And the bounds: a lag resets the per-episode re-attach count, so a slow
//! consumer is never convicted of a host that keeps resetting.
//!
//! **Determinism.** craze's end is played line by line, and the flood is
//! written in one burst the watcher folds faster than any test task reads: a
//! consumer that is not reading cannot drain a frame of it.

mod common;

use std::time::Duration;

use common::*;
use serde_json::{json, Value};
use shed_core::lane::conformance::{drive_lane, DriveEnd, LaneChecker};
use shed_core::lane::{LaneEvent, LANE_CHANNEL_CAPACITY};
use shed_craze::testing::{attach_result, snapshot_at, HubEnd};
use tokio::sync::mpsc::Receiver;

/// A tool event that draws exactly one row (its `tool_use`).
fn tool(i: usize) -> Value {
    json!({"type": "tool", "tool": {"id": format!("t-{i}"), "status": "pending", "title": format!("step {i}")}})
}

/// Write `n` row-drawing events to `sub` from seq `from`; the next seq.
async fn flood(hub: &mut HubEnd, sub: &str, from: u64, n: usize) -> u64 {
    let mut seq = from;
    for i in 0..n {
        hub.event(sub, seq, tool(seq as usize * 10_000 + i)).await;
        seq += 1;
    }
    seq
}

/// The queue's depth once it stops moving.
async fn settle(rx: &Receiver<LaneEvent>) -> usize {
    let mut last = rx.len();
    loop {
        tokio::time::sleep(Duration::from_millis(50)).await;
        let now = rx.len();
        if now == last {
            return now;
        }
        last = now;
    }
}

/// Drain everything queued now, without the checker (frames of an abandoned
/// generation the client would have rendered as they came; the checker sees
/// them too).
fn drain_now(rx: &mut Receiver<LaneEvent>, checker: &mut LaneChecker) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    while let Ok(ev) = rx.try_recv() {
        checker.observe(&ev).unwrap_or_else(|v| panic!("{v}"));
        out.push(ev);
    }
    out
}

/// The whole of correction 13 in one case: a consumer that reads nothing
/// while `LANE_CHANNEL_CAPACITY + 50` rows go past gets the generation
/// abandoned at the first dropped frame and the connection DROPPED while it is
/// still not reading; once it drains, one `Reset{lagged} … Ready` rebuilt from
/// a snapshot — an attach with NO cursor, never a resume.
#[tokio::test]
async fn a_steady_stream_past_the_bound_is_abandoned_and_reseeds_as_lagged() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;

    flood(&mut hub, SUB, 2, LANE_CHANNEL_CAPACITY + 50).await;
    // The connection is gone BEFORE the client drains a frame: the wait for a
    // slow consumer holds no transport.
    assert!(
        hub.recv_within(Duration::from_secs(5)).await.is_none(),
        "the lagged generation let its connection go"
    );
    assert_eq!(
        settle(&rx).await,
        LANE_CHANNEL_CAPACITY,
        "a full queue, none of it read"
    );
    let abandoned = drain_now(&mut rx, &mut checker);
    assert_eq!(abandoned.len(), LANE_CHANNEL_CAPACITY);
    assert!(
        resets(&abandoned).is_empty(),
        "still generation 1 until the drain"
    );

    let (mut hub, attach) = seed_conn(&mut conns, SUB, 1100, json!({})).await;
    assert!(
        attach["params"].get("cursor").is_none(),
        "a lagged reconnect is a RESEED: {attach}"
    );
    hub.synchronized(SUB, 1100).await;
    let second = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
    assert_eq!(resets(&second), [("lagged".to_string(), 2)]);
    assert!(matches!(
        second.last(),
        Some(LaneEvent::Ready { generation: 2 })
    ));
    assert!(
        tokio::time::timeout(Duration::from_millis(300), rx.recv())
            .await
            .is_err(),
        "a healthy generation does not reseed again on its own"
    );
}

/// The same rule INSIDE a reseed: a client that left most of the queue unread
/// gets a reseed that lags before its `Ready` — abandoned, never left staged —
/// and the next one completes.
#[tokio::test]
async fn a_reseed_that_lags_before_ready_is_abandoned_and_the_next_completes() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    // 900 rows the client does not read: no lag yet.
    let next = flood(&mut hub, SUB, 2, 900).await;
    assert_eq!(settle(&rx).await, 900);
    // The host resets; the re-attach's snapshot carries 300 entries — more
    // than the queue has room for.
    hub.reset(SUB, "replay_failed").await;
    let entries: Vec<Value> = (0..300)
        .map(|i| json!({"id": format!("{}.0", i + 1), "kind": "note", "text": format!("n{i}")}))
        .collect();
    hub.attached(attach_result(
        "s-2",
        &info(false),
        (INC, next - 1),
        Some(snapshot_at(INC, next - 1, json!({"entries": entries}))),
        None,
    ))
    .await;
    hub.synchronized("s-2", next - 1).await;
    assert!(
        hub.recv_within(Duration::from_secs(5)).await.is_none(),
        "the lagged reseed let its connection go"
    );
    let abandoned = drain_now(&mut rx, &mut checker);
    assert_eq!(
        resets(&abandoned),
        [("server_reset:replay_failed".to_string(), 2)]
    );
    assert!(
        !abandoned
            .iter()
            .any(|e| matches!(e, LaneEvent::Ready { generation: 2 })),
        "the seed that lagged emits no Ready"
    );
    let (mut hub, _) = seed_conn(&mut conns, SUB, next - 1, json!({})).await;
    hub.synchronized(SUB, next - 1).await;
    let f = drive(&mut rx, &mut checker, "the next seed", is_ready).await;
    assert_eq!(resets(&f), [("lagged".to_string(), 3)]);
    assert!(matches!(f.last(), Some(LaneEvent::Ready { generation: 3 })));
}

/// Fill the client queue to exactly `target` frames with rows the client does
/// not read; the next seq.
async fn fill_to(hub: &mut HubEnd, rx: &Receiver<LaneEvent>, from: u64, target: usize) -> u64 {
    let now = rx.len();
    assert!(now <= target);
    let next = flood(hub, SUB, from, target - now).await;
    assert_eq!(settle(rx).await, target, "the queue holds exactly {target}");
    next
}

/// **A lag on the SESSION row ends the generation.** With the queue full to
/// exactly the bound, a `presence` count — whose only publish is the session
/// row — is the frame that hits `Full`; what discriminates is that the
/// connection is let go.
#[tokio::test]
async fn a_lag_on_the_session_row_ends_the_generation() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    fill_to(&mut hub, &rx, 2, LANE_CHANNEL_CAPACITY).await;
    hub.notify("presence", json!({"subscription": SUB, "attached": 7}))
        .await;
    assert!(
        hub.recv_within(Duration::from_secs(5)).await.is_none(),
        "a swallowed lag would keep reading"
    );
    drain_now(&mut rx, &mut checker);
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let f = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
    assert_eq!(resets(&f)[0].0, "lagged");
}

/// **A lag on an APPROVAL frame ends the generation.** An ask's ending
/// publishes its resolved row, the session row (the pending count moved),
/// then the `Approval`: with the queue two short of the bound, the `Approval`
/// is the frame that hits `Full`.
#[tokio::test]
async fn a_lag_on_an_approval_frame_ends_the_generation() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.event(
        SUB,
        2,
        json!({"type": "permission", "permission": {"id": "perm-1", "tool": "Shell",
        "options": [{"optionId": "allow", "name": "Allow", "kind": "allow_once"}]}}),
    )
    .await;
    hub.synchronized(SUB, 2).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let next = fill_to(&mut hub, &rx, 3, LANE_CHANNEL_CAPACITY - 2).await;
    hub.event(SUB, next, json!({"type": "ask", "ask": {"id": "perm-1", "kind": "permission", "outcome": "answered", "optionId": "allow"}})).await;
    assert!(
        hub.recv_within(Duration::from_secs(5)).await.is_none(),
        "a swallowed lag would keep reading"
    );
    let held = drain_now(&mut rx, &mut checker);
    assert!(
        matches!(held.last(), Some(LaneEvent::Session { session }) if session.pending_approvals == 0),
        "the Approval was the dropped frame"
    );
    let (mut hub, _) = seed_conn(&mut conns, SUB, next, json!({})).await;
    hub.synchronized(SUB, next).await;
    drive(&mut rx, &mut checker, "the reseed", is_ready).await;
}

/// One host reset and the lane's re-attach, answered with a fresh snapshot
/// (no `synchronized`: the episode goes on).
async fn reset_and_reattach(hub: &mut HubEnd) {
    hub.reset(SUB, "replay_failed").await;
    hub.attached(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
}

/// A lag RESETS the per-episode re-attach count: a host that reset the stream
/// seven times without a `synchronized`, then a consumer that lagged, then
/// the lagged reseed and seven more re-attaches — the lane is still alive.
/// (Without the reset that is fifteen in one episode, and the bound is eight.
/// A lag is evidence the host served: a slow consumer is never convicted of a
/// host that keeps resetting — gx's `down_after` lesson.)
#[tokio::test]
async fn a_lag_resets_the_reattach_bound() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    for _ in 0..7 {
        reset_and_reattach(&mut hub).await;
    }
    flood(&mut hub, SUB, 2, LANE_CHANNEL_CAPACITY + 10).await;
    assert!(hub.recv_within(Duration::from_secs(5)).await.is_none());
    let mut checker = LaneChecker::new();
    drain_now(&mut rx, &mut checker);
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    for _ in 0..7 {
        reset_and_reattach(&mut hub).await;
    }
    hub.synchronized(SUB, 1).await;
    let f = drive(&mut rx, &mut checker, "alive", |e| {
        is_ready(e) || is_down(e)
    })
    .await;
    assert!(!f.iter().any(is_down), "no bound: {f:#?}");
    assert!(matches!(f.last(), Some(LaneEvent::Ready { .. })));
}

/// Two lags running: each abandoned generation has no `Ready`, the last
/// `Ready` a staging client shows is still generation 1's until the third
/// completes — and the BACKOFF climbs between them (paused clock).
#[tokio::test(start_paused = true)]
async fn consecutive_lags_keep_the_bracket_and_climb_the_backoff() {
    let (_dial, mut conns, lane) = scripted(shed_craze::LaneTimings::default());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;

    flood(&mut hub, SUB, 2, LANE_CHANNEL_CAPACITY + 10).await;
    assert!(hub.recv_within(Duration::from_secs(5)).await.is_none());
    drain_now(&mut rx, &mut checker);
    let drained1 = tokio::time::Instant::now();
    let mut hub = next_conn(&mut conns).await;
    let gap1 = drained1.elapsed();
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    // The reseed's replay floods before its synchronized: it lags too.
    flood(&mut hub, SUB, 2, LANE_CHANNEL_CAPACITY + 10).await;
    assert!(hub.recv_within(Duration::from_secs(5)).await.is_none());
    let abandoned = drain_now(&mut rx, &mut checker);
    assert!(
        !abandoned.iter().any(is_ready),
        "the lagged reseed has no Ready"
    );
    let drained2 = tokio::time::Instant::now();
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    let gap2 = drained2.elapsed();
    hub.synchronized(SUB, 1).await;
    let f = drive(&mut rx, &mut checker, "the third generation", is_ready).await;
    assert_eq!(resets(&f), [("lagged".to_string(), 3)]);
    assert!(
        gap1 <= Duration::from_millis(400),
        "the first lag's backoff: {gap1:?}"
    );
    assert!(
        gap2 >= Duration::from_millis(400),
        "the second climbed: {gap2:?}"
    );
}

/// **`Down` is never the dropped frame.** A full queue, then the session
/// closes: the client drains and finds `Down` last — `publish_final` waited
/// for room.
#[tokio::test]
async fn down_is_never_dropped() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let next = fill_to(&mut hub, &rx, 2, LANE_CHANNEL_CAPACITY - 1).await;
    hub.event(
        SUB,
        next,
        json!({"type": "text", "text": "a partial row the flush will try"}),
    )
    .await;
    hub.reset(SUB, "session_closed").await;
    tokio::time::sleep(Duration::from_millis(200)).await;
    let d = drive_lane(&mut rx, &mut checker, Duration::from_secs(5), is_down)
        .await
        .unwrap();
    assert_eq!(d.end, DriveEnd::Matched, "{:?}", d.frames.last());
    assert_eq!(
        d.frames.last(),
        Some(&LaneEvent::Down {
            reason: "session_closed".into()
        })
    );
    assert!(rx.recv().await.is_none(), "nothing after Down");
}

/// **A terminal `Down` never costs the transcript its last words.** The queue
/// EXACTLY full (the text's own `Session` frame takes the last slot) with text
/// still in the open segment, then the session closes: the flush's partial row
/// waits for room like the `Down` does, and lands right before it — there is
/// no reseed after a `Down` to restore a dropped row.
#[tokio::test]
async fn a_terminal_down_keeps_the_last_rows_behind_a_full_queue() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let next = fill_to(&mut hub, &rx, 2, LANE_CHANNEL_CAPACITY - 1).await;
    hub.event(SUB, next, json!({"type": "text", "text": "the last words"}))
        .await;
    assert_eq!(
        settle(&rx).await,
        LANE_CHANNEL_CAPACITY,
        "exactly full: the text is in the open segment, its Session frame in the last slot"
    );
    hub.reset(SUB, "session_closed").await;
    tokio::time::sleep(Duration::from_millis(200)).await;
    let d = drive_lane(&mut rx, &mut checker, Duration::from_secs(5), is_down)
        .await
        .unwrap();
    assert_eq!(d.end, DriveEnd::Matched, "{:?}", d.frames.last());
    let n = d.frames.len();
    assert!(
        matches!(&d.frames[n - 2], LaneEvent::Message { message, .. }
            if message.text.as_deref() == Some("the last words")),
        "the open segment's row, right before Down: {:?}",
        &d.frames[n.saturating_sub(3)..]
    );
    assert_eq!(
        d.frames[n - 1],
        LaneEvent::Down {
            reason: "session_closed".into()
        }
    );
    assert!(rx.recv().await.is_none(), "nothing after Down");
}

/// **A seed must fit the channel.** A snapshot window holding more entries
/// than the client channel has slots seeds its NEWEST rows under one "earlier
/// transcript omitted" row, and reaches `Ready` — uncapped, the seed would be
/// one burst bigger than the channel, lag at the same frame on every reseed,
/// and never reach `Ready` at all (the contract's correction 13).
#[tokio::test]
async fn a_seed_longer_than_the_channel_is_capped_and_completes() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let entries: Vec<Value> = (0..LANE_CHANNEL_CAPACITY + 500)
        .map(|i| json!({"id": format!("{}.0", i + 1), "kind": "note", "text": format!("n{i}")}))
        .collect();
    let (mut hub, _) = seed_conn(&mut conns, SUB, 2000, json!({ "entries": entries })).await;
    hub.synchronized(SUB, 2000).await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "the capped seed", is_ready).await;
    assert_eq!(
        resets(&f),
        [("connect".to_string(), 1)],
        "it fit the first time"
    );
    let t = texts(&f);
    assert_eq!(t.len(), shed_craze::watcher::SEED_ROWS);
    assert_eq!(t[0], "earlier transcript omitted");
    assert_eq!(
        t.last().map(String::as_str),
        Some(format!("n{}", LANE_CHANNEL_CAPACITY + 499).as_str()),
        "the newest rows"
    );
}
