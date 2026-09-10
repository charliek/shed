//! The client channel is BOUNDED, and a client that stops reading gets a
//! reseed — `shed_core::lane`'s module-doc correction 13, from the adapter's
//! side.
//!
//! Three cases, and the difference between them is WHERE the dropped frame
//! falls:
//!
//! 1. in steady state, after a generation reached its `Ready`;
//! 2. inside the REST seed, before one ever did;
//! 3. inside the lagged reseed itself, twice running.
//!
//! # How a lag is forced, deterministically
//!
//! Every test here runs on the default (current-thread) tokio runtime, and that
//! is load-bearing rather than incidental: the watcher's emit loops contain no
//! `await`, so a burst of rows is published *atomically* with respect to the
//! test task. A consumer that is not inside `recv()` at that moment cannot
//! drain a single frame of it. So "the consumer read nothing" is a fact here,
//! not a race the test has to win.
//!
//! The other half is arithmetic. A lag needs MORE than
//! [`LANE_CHANNEL_CAPACITY`] frames published in one burst, and the reseed that
//! follows has to need FEWER, or it would lag identically and forever. The two
//! are separated by using frames the agent does not persist: a `session.error`
//! is a display-only transcript row the fold mints from the live stream, and
//! `GET /session/{id}/message` — the only thing a reseed reads — has never
//! heard of it. So the burst is markers and the seed is real turns, and the
//! reseed rebuilds exactly what the agent says the transcript is.
//!
//! # Why there is a session cell here and no approval cell
//!
//! `shed-gx`'s suite has both. This one cannot, and the reason is a property of
//! opencode's fold rather than a gap: its `activity` is DERIVED from the open
//! approval count (`open_approvals() > 0 → NeedsApproval`) and
//! `LaneSession::pending_approvals` IS that count, so **every** approval change
//! it can produce also changes the session row. `emit_session` runs
//! immediately behind `emit_approvals`, so a swallowed lag there is always
//! caught one publish later and the generation ends anyway — the "silent
//! approval hole" is not reachable on this adapter. (The one shape that would
//! change an approval's DTO without touching the count, a re-`asked`
//! permission, is refused by the fold as "a replay is not a state change".)
//! The `?` on `emit_approvals` stays for uniformity with gx, where the same
//! hole IS reachable and is pinned there.

mod common;

use std::time::{Duration, Instant};

use common::{
    assert_clean, drain_now, marker, resets, shape, shape_head, texts, until_ready, wait_for,
};
use serde_json::{json, Value};
use shed_core::lane::{AgentLane, LaneEvent, LANE_CHANNEL_CAPACITY};
use shed_opencode::testing::FakeOpencode;
use shed_opencode::OpencodeClient;
use tokio::sync::mpsc::Receiver;

const SID: &str = "ses_a";

/// The seed for the steady-state case: big enough that the burst on top of it
/// crosses the bound at `LANE_CHANNEL_CAPACITY + 50` rows exactly, small enough
/// that the reseed which re-serves it fits inside the bound with room.
const SEED_ROWS: usize = 400;

/// A finished assistant turn per row, as `GET /session/{id}/message` serves
/// them: an `info` plus one text part with an end time, which is the fold's
/// terminal condition, so each row emits as one `Message` frame.
fn transcript(n: usize) -> Value {
    let rows: Vec<Value> = (0..n)
        .map(|i| {
            let started = 1_700_000_000_000i64 + i as i64;
            json!({
                "info": {
                    "id": format!("msg_{i}"),
                    "role": "assistant",
                    "time": { "created": started, "completed": started + 1 },
                },
                "parts": [{
                    "id": format!("prt_{i}"),
                    "messageID": format!("msg_{i}"),
                    "type": "text",
                    "text": format!("row {i}"),
                    "time": { "start": started, "end": started + 1 },
                }],
            })
        })
        .collect();
    json!(rows)
}

/// What [`transcript`] folds to, in order — the exact list a completed seed
/// must produce.
fn transcript_texts(n: usize) -> Vec<String> {
    (0..n).map(|i| format!("row {i}")).collect()
}

async fn fake_with(rows: usize) -> FakeOpencode {
    let fake = FakeOpencode::start().await;
    fake.add_session(SID, "root", "/w", None);
    fake.set_messages(SID, transcript(rows));
    fake.set_status(SID, "idle");
    fake.pin(SID);
    fake
}

async fn subscribe(fake: &FakeOpencode) -> (Receiver<LaneEvent>, shed_core::lane::LaneStop) {
    OpencodeClient::new(fake.base_url(), None)
        .expect("the client builds")
        .subscribe(SID, None)
        .await
        .expect("the subscription opens")
        .into_parts()
}

/// Stream `rows` display-only transcript rows. They reach the client, and they
/// are NOT in the agent's persisted transcript — see the module doc.
fn stream_rows(fake: &FakeOpencode, rows: usize) {
    for i in 0..rows {
        fake.push_event(&marker(SID, &format!("live {i}")));
    }
}

/// The whole of correction 13 in one case: a consumer that reads nothing while
/// `LANE_CHANNEL_CAPACITY + 50` rows go past gets the generation abandoned at
/// the first dropped frame, the transport released while it is still not
/// reading, and — once it drains — one `Reset{lagged}` … `Ready` carrying the
/// agent's whole transcript.
#[tokio::test]
async fn a_steady_stream_past_the_bound_is_abandoned_and_reseeds_as_lagged() {
    let fake = fake_with(SEED_ROWS).await;
    let (mut rx, _stop) = subscribe(&fake).await;

    // The consumer reads NOTHING from here on. The seed's own rows count
    // towards the bound, so the burst is what takes the total to CAP + 50.
    wait_for("the seed to be served", || {
        fake.get_paths().iter().any(|p| p.ends_with("/message"))
    })
    .await;
    stream_rows(&fake, LANE_CHANNEL_CAPACITY + 50 - SEED_ROWS);

    // The generation ends at the FIRST dropped frame, and ending it drops the
    // `/event` stream — which is what makes the wait that follows hold no
    // transport. Asserted BEFORE the client drains a single frame: that is the
    // whole claim, and a version that released the stream only after the
    // consumer caught up would pass every other assertion here.
    wait_for("the lagged generation to release its stream", || {
        fake.stream_count() == 0
    })
    .await;
    assert_eq!(
        rx.len(),
        LANE_CHANNEL_CAPACITY,
        "the client is holding a full queue and has read none of it"
    );

    let abandoned = drain_now(&mut rx);
    assert_eq!(
        abandoned.len(),
        LANE_CHANNEL_CAPACITY,
        "exactly the bound was queued; everything past it was dropped"
    );
    assert_eq!(
        resets(&abandoned),
        vec![("seed".to_string(), 1)],
        "one generation so far: {:?}",
        shape_head(&abandoned)
    );

    // Now it drains, and gets ONE usable generation: a fresh bracket, rebuilt
    // from the agent's REST seed.
    let second = until_ready(&mut rx).await;
    assert_eq!(
        resets(&second),
        vec![("lagged".to_string(), 2)],
        "the reseed names the client channel, and there is exactly one of it"
    );
    assert!(
        matches!(second.last(), Some(LaneEvent::Ready { generation: 2 })),
        "the reseed COMPLETES its bracket: {:?}",
        shape(&second)
    );
    assert_eq!(
        texts(&second),
        transcript_texts(SEED_ROWS),
        "the Ready view is the agent's whole transcript — nothing lost inside \
         it, nothing duplicated"
    );

    // And nothing follows it: one abandoned generation, one usable one.
    assert!(
        tokio::time::timeout(Duration::from_millis(300), rx.recv())
            .await
            .is_err(),
        "a healthy generation must not reseed again on its own"
    );
    assert_clean(&fake);
}

/// The same rule inside the SEED. A transcript larger than the channel lags
/// before the generation ever reaches `Ready`, and the generation is abandoned
/// rather than left staged — a client told to `Reset` and never given a `Ready`
/// has discarded its view and been handed nothing to replace it with.
#[tokio::test]
async fn a_seed_past_the_bound_is_abandoned_before_ready_and_the_next_one_completes() {
    let fake = fake_with(LANE_CHANNEL_CAPACITY + 50).await;
    // Park the seed's transcript read. "The consumer is stalled while the seed
    // is in flight" is then a condition this test stands on rather than a race
    // it has to widen a window to win.
    fake.hold_get("/message");
    let (mut rx, _stop) = subscribe(&fake).await;
    wait_for("the seed's transcript read to be parked", || {
        fake.get_paths().iter().any(|p| p.ends_with("/message"))
    })
    .await;
    assert_eq!(
        fake.stream_count(),
        1,
        "the stream is open and the seed is mid-flight"
    );
    fake.release_get("/message");

    wait_for("the lagged seed to release its stream", || {
        fake.stream_count() == 0
    })
    .await;

    // A reseed re-reads the SAME route, so a seed past the bound would lag
    // again, forever — which is the honest behaviour and why the bound is a
    // count that a 500-row seed sits well inside. The agent's transcript is
    // what a reseed reads; shrinking it here is this test saying "and now it
    // fits". Done BEFORE the drain, so the reseed cannot start against the old
    // one.
    fake.set_messages(SID, transcript(3));

    let abandoned = drain_now(&mut rx);
    assert_eq!(
        resets(&abandoned),
        vec![("seed".to_string(), 1)],
        "the first generation opened its bracket"
    );
    assert!(
        !abandoned
            .iter()
            .any(|e| matches!(e, LaneEvent::Ready { .. })),
        "and never closed it — a lagged seed is ABANDONED, not staged: {:?}",
        shape_head(&abandoned)
    );
    assert_eq!(abandoned.len(), LANE_CHANNEL_CAPACITY);

    let second = until_ready(&mut rx).await;
    assert_eq!(resets(&second), vec![("lagged".to_string(), 2)]);
    assert!(matches!(
        second.last(),
        Some(LaneEvent::Ready { generation: 2 })
    ));
    assert_eq!(texts(&second), transcript_texts(3));
    assert_clean(&fake);
}

/// Fill the client queue to EXACTLY `target` frames.
///
/// Each `marker` is one `Message` frame and nothing else — a `session.error` is
/// display-only and never touches the activity verdict, so it cannot smuggle in
/// a `Session` frame and make the count inexact. The last few go one at a time
/// because the whole point is a queue with a KNOWN number of free slots.
async fn fill_queue(fake: &FakeOpencode, rx: &Receiver<LaneEvent>, target: usize) {
    let mut n = 9_000usize;
    for _ in 0..80 {
        let have = rx.len();
        if have >= target {
            break;
        }
        let short = target - have;
        let batch = if short > 8 { short - 4 } else { 1 };
        for _ in 0..batch {
            fake.push_event(&marker(SID, &format!("fill {n}")));
            n += 1;
        }
        // Wait for THIS batch to land. Waiting for the queue to merely stop
        // growing is not enough: a thousand frames crossing a loopback socket
        // on a current-thread runtime pause for longer than any quiet window
        // worth using, and a batch sized off a half-delivered count overshoots
        // the bound and lags on the wrong frame.
        wait_for("the fill batch to land", || {
            rx.len() >= (have + batch).min(target)
        })
        .await;
    }
    assert_eq!(
        settle(rx).await,
        target,
        "the queue must be filled to EXACTLY {target}"
    );
}

/// The queue's depth once it stops moving.
async fn settle(rx: &Receiver<LaneEvent>) -> usize {
    let mut last = usize::MAX;
    loop {
        let now = rx.len();
        if now == last {
            return now;
        }
        last = now;
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// **A lag on the SESSION row ends the generation.** Every other cell in this
/// file forces the overflow through `Message` emission, so an implementation
/// that propagated the lag out of `drain_rows` and swallowed it in
/// `emit_session` would pass all of them — and go on reading the agent's stream
/// with the client holding a session row it will never be told is wrong.
///
/// The construction makes `emit_session` the ONLY publish: a `session.status`
/// frame mints no transcript row, and with no approvals open `emit_approvals`
/// publishes nothing either. So with a queue filled to exactly the bound, the
/// session row is the frame that hits `Full`.
///
/// What discriminates is that the generation ENDS — the stream is released.
/// A swallowed lag leaves `apply_frame` answering `Ok`, the read loop running,
/// and the stream open.
#[tokio::test]
async fn a_lag_on_the_session_row_ends_the_generation() {
    let fake = fake_with(2).await;
    let (mut rx, _stop) = subscribe(&fake).await;
    let first = until_ready(&mut rx).await;
    assert!(
        matches!(first.last(), Some(LaneEvent::Ready { generation: 1 })),
        "generation 1 comes up first: {:?}",
        shape(&first)
    );

    fill_queue(&fake, &rx, LANE_CHANNEL_CAPACITY).await;
    assert_eq!(
        fake.stream_count(),
        1,
        "the generation is alive, in steady state, with a full queue"
    );

    // `idle` → `busy` flips the fold's verdict, so the row genuinely changed.
    fake.push_event(&json!({
        "type": "session.status",
        "properties": { "sessionID": SID, "status": { "type": "busy" } },
    }));
    wait_for(
        "the generation to END on the dropped session row (a swallowed lag \
         keeps reading instead)",
        || fake.stream_count() == 0,
    )
    .await;

    fake.set_messages(SID, transcript(3));
    drain_now(&mut rx);
    let second = until_ready(&mut rx).await;
    assert_eq!(resets(&second), vec![("lagged".to_string(), 2)]);
    assert!(
        second
            .iter()
            .any(|e| matches!(e, LaneEvent::Session { .. })),
        "the reseed republishes the session row it dropped: {:?}",
        shape(&second)
    );
    assert_clean(&fake);
}

/// A consumer that stalls again INSIDE the lagged reseed. Two things have to
/// hold: the bracket (the reseed that lagged emits no `Ready`, so the last
/// `Ready` view a staging client shows is still the first generation's), and
/// the BACKOFF — consecutive lagged generations climb the ordinary failure
/// curve, so a consumer draining one frame at a time cannot turn itself into a
/// seed-per-frame load on a live agent.
#[tokio::test]
async fn consecutive_lags_keep_the_bracket_and_climb_the_backoff() {
    const SMALL: usize = 2;
    let fake = fake_with(SMALL).await;
    let (mut rx, _stop) = subscribe(&fake).await;

    // Generation 1 completes. THIS is the view a staging client holds for the
    // rest of the test.
    let first = until_ready(&mut rx).await;
    assert_eq!(resets(&first), vec![("seed".to_string(), 1)]);
    assert_eq!(texts(&first), transcript_texts(SMALL));

    // Stop reading; overflow.
    stream_rows(&fake, LANE_CHANNEL_CAPACITY + 50);
    wait_for("the first lagged generation", || fake.stream_count() == 0).await;

    // The reseed will lag too: the agent's transcript is now bigger than the
    // channel, so generation 2 cannot finish its own seed either.
    fake.set_messages(SID, transcript(LANE_CHANNEL_CAPACITY + 50));
    assert_eq!(drain_now(&mut rx).len(), LANE_CHANNEL_CAPACITY);

    let drained_at = Instant::now();
    wait_for("generation 2 to say anything at all", || !rx.is_empty()).await;
    let first_backoff = drained_at.elapsed();
    assert!(
        first_backoff >= Duration::from_millis(100),
        "the reseed waits out the failure backoff, not just the drain: \
         {first_backoff:?}"
    );

    wait_for("generation 2 to lag as well", || {
        rx.len() == LANE_CHANNEL_CAPACITY
    })
    .await;
    // Shrink first, so generation 3 has something that fits by the time the
    // drain below lets it start.
    fake.set_messages(SID, transcript(SMALL));
    let staged = drain_now(&mut rx);
    assert_eq!(
        resets(&staged),
        vec![("lagged".to_string(), 2)],
        "generation 2 opened a bracket"
    );
    assert!(
        !staged.iter().any(|e| matches!(e, LaneEvent::Ready { .. })),
        "…and did NOT close it, so the last Ready view a client shows is still \
         generation 1's: {:?}",
        shape_head(&staged)
    );

    // Generation 3, and the backoff has DOUBLED: two failures in a row.
    let drained_at = Instant::now();
    let reset = common::next_event(&mut rx, "generation 3's Reset").await;
    let second_backoff = drained_at.elapsed();
    match &reset {
        LaneEvent::Reset { reason, generation } => {
            assert_eq!(reason, "lagged");
            assert_eq!(*generation, 3);
        }
        other => panic!("generation 3 must open with a Reset, got {other:?}"),
    }
    assert!(
        second_backoff >= Duration::from_millis(200),
        "a second consecutive lag doubles the wait ({first_backoff:?} → \
         {second_backoff:?}); without the climb a one-frame-at-a-time consumer \
         is a reseed per frame"
    );

    let rest = until_ready(&mut rx).await;
    assert!(matches!(
        rest.last(),
        Some(LaneEvent::Ready { generation: 3 })
    ));
    assert_eq!(texts(&rest), transcript_texts(SMALL));
    assert_clean(&fake);
}
