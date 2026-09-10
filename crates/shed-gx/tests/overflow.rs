//! The client channel is BOUNDED, and a client that stops reading gets a
//! reseed — `shed_core::lane`'s module-doc correction 13, from the gx side.
//!
//! Three cases, and the difference between them is WHERE the dropped frame
//! falls:
//!
//! 1. in steady state, after a generation reached its `Ready`;
//! 2. inside the seed, before one ever did;
//! 3. inside the lagged reseed itself, twice running.
//!
//! # How a lag is forced, deterministically
//!
//! These run on the default (current-thread) tokio runtime, and that is
//! load-bearing rather than incidental: the watcher's emit loops contain no
//! `await`, so a burst of rows is published *atomically* with respect to the
//! test task. A consumer that is not inside `recv()` at that moment cannot
//! drain a single frame of it, so "the consumer read nothing" is a fact here
//! and not a race the test has to win.
//!
//! The other half is arithmetic. A lag needs MORE than
//! [`LANE_CHANNEL_CAPACITY`] frames published in one burst, and the reseed that
//! follows has to need FEWER, or it would lag identically and forever. On gx
//! the seed window does that by itself: `GxTimings::seed_limit` caps what a
//! reseed reads at 500 envelopes, which is exactly the "a 500-row seed fits
//! with room" the bound was chosen against. Where a test wants the SEED itself
//! to lag it raises `seed_limit` on purpose.
//!
//! Every burst chunk carries its own `promptId`, so it segments the streak the
//! one before it opened: one envelope in, one transcript row out, live and on
//! the reseed alike.

mod common;

use std::time::{Duration, Instant};

use shed_core::lane::{LaneEvent, LANE_CHANNEL_CAPACITY};
use shed_gx::testing::FakeGx;
use shed_gx::GxTimings;
use tokio::sync::mpsc::Receiver;

use common::*;

/// A fake with one session and a short transcript that ends closed.
async fn seeded() -> FakeGx {
    let fake = FakeGx::start().await;
    fake.add_session(SID, Some("a fixture session"), "/scratch", "idle", 0, false);
    fake.set_history(
        SID,
        vec![
            chunk(10, "user_message_chunk", "say pong", Some("p1")),
            chunk(12, "agent_message_chunk", "pong", Some("p1")),
            turn_completed(13, "end_turn"),
        ],
    );
    fake.pin(SID);
    fake
}

/// A transcript of `n` closed rows, oldest first — what `GET …/history` serves.
fn transcript(n: usize) -> Vec<serde_json::Value> {
    let mut out: Vec<serde_json::Value> = (0..n)
        .map(|i| {
            chunk(
                100 + i as u64,
                "agent_message_chunk",
                &format!("row {i}"),
                Some(&format!("p{i}")),
            )
        })
        .collect();
    out.push(turn_completed(100 + n as u64, "end_turn"));
    out
}

fn transcript_texts(n: usize) -> Vec<String> {
    (0..n).map(|i| format!("row {i}")).collect()
}

/// Stream `n` rows onto the live stream. Each lands in the fake's persisted
/// transcript too, exactly as a leader's pump writes what it broadcasts.
fn stream_rows(fake: &FakeGx, n: usize) -> Vec<String> {
    let mut texts = Vec::with_capacity(n);
    for i in 0..n {
        let text = format!("live {i}");
        fake.push_update(
            SID,
            &chunk(
                5_000 + i as u64,
                "agent_message_chunk",
                &text,
                Some(&format!("live-p{i}")),
            ),
        );
        texts.push(text);
    }
    // Closes the last streak, so the transcript has no partial row hanging off
    // the end on either the live side or the reseed's.
    fake.push_update(SID, &turn_completed(5_000 + n as u64, "end_turn"));
    texts
}

/// The whole of correction 13 in one case: a consumer that reads nothing while
/// `LANE_CHANNEL_CAPACITY + 50` rows go past gets the generation abandoned at
/// the first dropped frame, the SSE stream dropped and the transport epoch
/// unpinned while it is still not reading, and — once it drains — one
/// `Reset{lagged}` … `Ready` rebuilt from gx's own seed, with no
/// `Last-Event-ID` anywhere near it.
#[tokio::test]
async fn a_steady_stream_past_the_bound_is_abandoned_and_reseeds_as_lagged() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;

    // Generation 1 completes, and is READ — so the burst below is
    // unambiguously steady state.
    let first = until_ready(&mut rx).await;
    assert_eq!(resets(&first), vec![("connect".to_string(), 1)]);

    // From here the consumer reads NOTHING.
    let pushed = stream_rows(&fake, LANE_CHANNEL_CAPACITY + 50);

    // The generation ends at the FIRST dropped frame. Ending it drops the SSE
    // stream and unpins the epoch — asserted BEFORE the client drains a single
    // frame, because that is the claim: the wait for a slow consumer holds no
    // transport. An implementation that released these only after the consumer
    // caught up would pass every other assertion in this file.
    until(
        || fake.stream_count() == 0,
        "the lagged generation to release its SSE stream",
    )
    .await;
    assert!(
        lane.pinned_instance().await.is_none(),
        "the transport epoch is unpinned while the client is still not reading"
    );
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
    assert!(
        resets(&abandoned).is_empty(),
        "the abandoned generation is still generation 1 — nothing new was \
         announced before the client drained: {:?}",
        shape_head(&abandoned)
    );

    // Now it drains, and gets ONE usable generation.
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

    // The Ready view is gx's own seed window, whole: a contiguous tail of what
    // the agent actually said, ending at the last thing it said. Nothing lost
    // inside it, nothing duplicated.
    let seen = texts(&second);
    assert!(
        seen.len() >= 400,
        "the reseed serves the seed window, not a handful: {} rows",
        seen.len()
    );
    assert_eq!(
        seen,
        pushed[pushed.len() - seen.len()..],
        "the rebuilt transcript is the agent's own tail, in order"
    );

    // **No cursor resume for a lag.** The dropped frames were folded at or
    // before the cursor, so `Last-Event-ID` would ask gx for everything AFTER
    // them and leave the hole permanent.
    let events = fake.requests_to("/events");
    assert_eq!(events.len(), 2, "one connect per generation");
    assert!(
        events.iter().all(|r| r.last_event_id.is_none()),
        "a lagged reconnect is a RESEED, never a resume: {:?}",
        events.iter().map(|r| &r.last_event_id).collect::<Vec<_>>()
    );

    // And nothing follows it: one abandoned generation, one usable one.
    assert!(
        tokio::time::timeout(Duration::from_millis(300), rx.recv())
            .await
            .is_err(),
        "a healthy generation must not reseed again on its own"
    );
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

/// The same rule inside the SEED. A seed window larger than the channel lags
/// before the generation ever reaches `Ready`, and the generation is abandoned
/// rather than left staged — a client told to `Reset` and never given a `Ready`
/// has discarded its view and been handed nothing to replace it with.
#[tokio::test]
async fn a_seed_past_the_bound_is_abandoned_before_ready_and_the_next_one_completes() {
    let big = LANE_CHANNEL_CAPACITY + 50;
    let fake = FakeGx::start().await;
    fake.add_session(SID, Some("a fixture session"), "/scratch", "idle", 0, false);
    fake.set_history(SID, transcript(big));
    fake.pin(SID);
    // The seed window is normally 500 — which is what keeps a seed inside the
    // bound. Opened up on purpose here, so the seed itself is what lags.
    let timings = GxTimings {
        seed_limit: 4_000,
        ..fast()
    };
    let lane = client_for(&fake, timings);

    // Park the history read. "The consumer is stalled while the seed is in
    // flight" is then a condition this test stands on rather than a race it has
    // to widen a window to win.
    fake.hold_get("/history");
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until(
        || fake.paths().iter().any(|p| p.ends_with("/history")),
        "the seed's history read to be parked",
    )
    .await;
    assert_eq!(
        fake.stream_count(),
        1,
        "the stream is open and the seed is mid-flight"
    );
    fake.release_get("/history");

    until(
        || fake.stream_count() == 0,
        "the lagged seed to release its stream",
    )
    .await;

    // A reseed re-reads the same window, so a seed past the bound would lag
    // again, forever — which is the honest behaviour, and why the bound is a
    // count that gx's own 500-envelope seed sits well inside. Shrinking the
    // agent's transcript here is this test saying "and now it fits"; done
    // BEFORE the drain, so the reseed cannot start against the old one.
    fake.set_history(SID, transcript(3));

    let abandoned = drain_now(&mut rx);
    assert_eq!(
        resets(&abandoned),
        vec![("connect".to_string(), 1)],
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
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

/// Timings that keep the partial-row FLUSH out of the way.
///
/// The fills below leave a streak open by construction (a chunk opens one and
/// the next chunk closes it), and a flush firing mid-fill would publish a row
/// the count did not account for — or, once the queue is full, lag on a frame
/// other than the one under test.
fn no_flush() -> GxTimings {
    GxTimings {
        flush_after: Duration::from_secs(30),
        ..fast()
    }
}

/// Fill the client queue to EXACTLY `target` frames.
///
/// Each chunk carries its own `promptId`, so it segments the streak the one
/// before it opened: one envelope in, one `Message` out. The very first one also
/// moves the activity to `working` and so publishes a session row; the loop
/// converges on `rx.len()` rather than assuming a yield, and the last few go one
/// at a time because the whole point is a queue with a KNOWN number of free
/// slots.
async fn fill_queue(fake: &FakeGx, rx: &Receiver<LaneEvent>, target: usize) {
    let mut n = 9_000u64;
    for _ in 0..80 {
        let have = rx.len();
        if have >= target {
            break;
        }
        let short = target - have;
        // Short of the target by 4, so a batch that yields one frame more than
        // it has envelopes (the first chunk also publishes a session row)
        // cannot overshoot into the bound.
        let batch = if short > 8 { short - 4 } else { 1 };
        for _ in 0..batch {
            fake.push_update(
                SID,
                &chunk(
                    n,
                    "agent_message_chunk",
                    &format!("fill {n}"),
                    Some(&format!("fill-p{n}")),
                ),
            );
            n += 1;
        }
        // Wait for THIS batch to land. Waiting for the queue to merely stop
        // growing is not enough: a thousand frames crossing a loopback socket
        // on a current-thread runtime pause for longer than any quiet window
        // worth using, and a batch sized off a half-delivered count overshoots
        // the bound and lags on the wrong frame.
        until(
            || rx.len() >= (have + batch).min(target),
            "the fill batch to land",
        )
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
/// `emit_session` would pass all of them — and go on reading gx's stream with
/// the client holding a session row it will never be told is wrong.
///
/// The construction makes `emit_session` the ONLY publish: an `event: session`
/// frame mints no transcript row, and with no approvals open `emit_approvals`
/// publishes nothing either. So with a queue filled to exactly the bound, the
/// session row is the frame that hits `Full`.
///
/// What discriminates is that the generation ENDS — the SSE stream is released.
/// A swallowed lag leaves `apply_frame` answering `Ok(None)`, the read loop
/// running, and the stream open.
#[tokio::test]
async fn a_lag_on_the_session_row_ends_the_generation() {
    let fake = seeded().await;
    let lane = client_for(&fake, no_flush());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
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

    // The fill left the row reading `working` (the fold's verdict overrides a
    // roster row it is newer than); an `idle` roster frame is therefore a real
    // change, and a `session` frame sets `row_at` so no override reverses it.
    let row = serde_json::json!({
        "sessionId": SID,
        "title": "a fixture session",
        "cwd": "/scratch",
        "activity": "idle",
        "pendingApprovals": 0,
        "approximate": false,
    });
    let pushed_at = Instant::now();
    fake.push_session_frame(SID, &row);
    until(
        || fake.stream_count() == 0,
        "the generation to END on the dropped session row (a swallowed lag \
         keeps reading instead)",
    )
    .await;
    // ON the dropped frame — not two seconds later when the stall timer
    // notices the fake has gone quiet, which is how a swallowed lag ends up
    // looking like a slow pass.
    assert!(
        pushed_at.elapsed() < no_flush().stall / 2,
        "the generation must end on the dropped session row, not on a later \
         stall: {:?}",
        pushed_at.elapsed()
    );

    fake.set_history(SID, transcript(3));
    drain_now(&mut rx);
    let second = until_ready(&mut rx).await;
    assert_eq!(resets(&second), vec![("lagged".to_string(), 2)]);
    assert!(
        !sessions(&second).is_empty(),
        "the reseed republishes the session row it dropped: {:?}",
        shape(&second)
    );
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

/// **A lag on an APPROVAL frame ends the generation.** The sharpest failure the
/// bound admits: a queue full of transcript, an approval change whose failed
/// publish is ignored, and a generation that carries on with a silent approval
/// hole — a client left rendering a stale approval state nobody will correct.
///
/// Making `emit_approvals` the only publish takes one setup step. Every approval
/// transition that crosses `pending` also moves
/// `LaneSession::pending_approvals`, so `emit_session` behind it would publish
/// too and mask a swallowed lag. `submitted → resolved` does not: both are
/// non-pending, the count stays 0, and the session row is byte-identical. So the
/// approval is walked to `submitted` FIRST, and the frame under test is the one
/// that resolves it.
///
/// What discriminates, again, is that the generation ENDS.
#[tokio::test]
async fn a_lag_on_an_approval_frame_ends_the_generation() {
    let fake = seeded().await;
    fake.add_approval(
        SID,
        "app_1",
        "permission",
        "session/request_permission",
        &permission_request("id -un"),
    );
    let lane = client_for(&fake, no_flush());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let first = until_ready(&mut rx).await;
    assert_eq!(
        approvals(&first)
            .iter()
            .map(|a| a.status.as_str().to_string())
            .collect::<Vec<_>>(),
        vec!["pending".to_string()],
        "the seed announced the approval: {:?}",
        shape(&first)
    );

    // Step off `pending`, so the frame under test cannot move the count.
    fake.push_approval_frame(SID, &approval_at(&fake, "submitted"));
    until_approval(&mut rx, "app_1").await;
    settle(&rx).await;
    drain_now(&mut rx);

    fill_queue(&fake, &rx, LANE_CHANNEL_CAPACITY).await;
    assert_eq!(fake.stream_count(), 1);

    let pushed_at = Instant::now();
    fake.push_approval_frame(SID, &approval_at(&fake, "resolved"));
    until(
        || fake.stream_count() == 0,
        "the generation to END on the dropped approval frame (a swallowed lag \
         keeps reading instead)",
    )
    .await;
    // ON the dropped frame — see the session cell.
    assert!(
        pushed_at.elapsed() < no_flush().stall / 2,
        "the generation must end on the dropped approval frame, not on a later \
         stall: {:?}",
        pushed_at.elapsed()
    );

    fake.set_history(SID, transcript(3));
    drain_now(&mut rx);
    let second = until_ready(&mut rx).await;
    assert_eq!(resets(&second), vec![("lagged".to_string(), 2)]);
    assert!(
        !approvals(&second).is_empty(),
        "the reseed republishes the approval it dropped: {:?}",
        shape(&second)
    );
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

/// The fake's own approval resource, at `status` — what a leader's
/// `event: approval` frame carries.
fn approval_at(fake: &FakeGx, status: &str) -> serde_json::Value {
    let mut res = fake
        .approval_resource(SID, "app_1")
        .expect("the fake holds the approval");
    res["status"] = serde_json::Value::String(status.to_string());
    res
}

/// A lag CLEARS gx's `down_after` clock — the "reseeds that keep failing end the
/// subscription" clock — rather than leaving it running across the drain wait.
///
/// The bug this pins was real and subtle: the lagged arm `continue`d past the
/// `failing_since` block, so a clock started by an EARLIER failed reseed kept
/// ticking through `wait_drained()`. A consumer that then stalled for longer
/// than `down_after` handed the next brief reseed failure an already-expired
/// clock, and the lane went terminally `Down("unreachable: …")` — a slow
/// consumer convicted of an unreachable agent. A lag is positive evidence of
/// the opposite: gx served the seed and kept streaming, which is the only
/// reason 1024 slots filled.
#[tokio::test]
async fn a_lag_clears_the_unreachable_clock_so_a_slow_consumer_is_never_down() {
    let timings = GxTimings {
        down_after: Duration::from_millis(1_000),
        ..fast()
    };
    let fake = seeded().await;
    // Generation 1 is a RESEED, and it FAILS. That is what starts the clock.
    fake.fail("/history", 503, "leader_unavailable", "no leader right now");
    let lane = client_for(&fake, timings);
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until(
        || !fake.requests_to("/history").is_empty(),
        "the first reseed's seed read to fail",
    )
    .await;
    fake.clear_failures();

    // A later generation comes up — and the clock is NOT cleared by that,
    // because `failing_since` is only reset when a generation ENDS.
    let ready = until_ready(&mut rx).await;
    assert!(
        matches!(ready.last(), Some(LaneEvent::Ready { .. })),
        "a generation must come up: {:?}",
        shape_head(&ready)
    );

    // Now the consumer stops reading, that generation ends as a LAG, and the
    // consumer stalls for LONGER than `down_after`.
    stream_rows(&fake, LANE_CHANNEL_CAPACITY + 50);
    until(
        || fake.stream_count() == 0,
        "the lagged generation to release its stream",
    )
    .await;
    tokio::time::sleep(Duration::from_millis(1_300)).await;

    // The reseed the drain finally allows fails ONCE, which is what reads the
    // clock. Armed BEFORE the drain, so there is no race about which generation
    // it hits.
    fake.fail("/history", 503, "leader_unavailable", "a blip");
    let before = fake.requests_to("/history").len();
    drain_now(&mut rx);
    until(
        || fake.requests_to("/history").len() > before,
        "the lagged reseed to attempt its seed",
    )
    .await;
    fake.clear_failures();

    // With a stale clock this is a terminal `Down("unreachable: …")`.
    let recovered = until_ready(&mut rx).await;
    assert_eq!(
        down_reason(&recovered),
        None,
        "a slow consumer must never be answered with an unreachable-agent Down"
    );
    assert!(
        matches!(recovered.last(), Some(LaneEvent::Ready { .. })),
        "…and the lane comes back: {:?}",
        shape_head(&recovered)
    );
}

/// A consumer that stalls again INSIDE the lagged reseed. Two things have to
/// hold: the bracket (the reseed that lagged emits no `Ready`, so the last
/// `Ready` view a staging client shows is still the first generation's), and
/// the BACKOFF — consecutive lagged generations climb gx's ordinary failure
/// ladder, so a consumer draining one frame at a time cannot turn itself into a
/// seed-per-frame load on a live leader.
#[tokio::test]
async fn consecutive_lags_keep_the_bracket_and_climb_the_backoff() {
    let fake = seeded().await;
    // A seed window wide enough to re-serve everything the burst persists —
    // which is what makes the SECOND generation lag as well.
    let timings = GxTimings {
        seed_limit: 4_000,
        ..fast()
    };
    let lane = client_for(&fake, timings);
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;

    // Generation 1 completes. THIS is the view a staging client holds for the
    // rest of the test.
    let first = until_ready(&mut rx).await;
    assert_eq!(resets(&first), vec![("connect".to_string(), 1)]);
    assert_eq!(texts(&first), vec!["say pong", "pong"]);

    // Stop reading; overflow. The burst lands in the agent's transcript too, so
    // generation 2's seed cannot fit either.
    stream_rows(&fake, LANE_CHANNEL_CAPACITY + 50);
    until(
        || fake.stream_count() == 0,
        "the first lagged generation to release its stream",
    )
    .await;
    assert_eq!(drain_now(&mut rx).len(), LANE_CHANNEL_CAPACITY);

    let drained_at = Instant::now();
    until(|| !rx.is_empty(), "generation 2 to say anything at all").await;
    let first_backoff = drained_at.elapsed();
    assert!(
        first_backoff >= Duration::from_millis(100),
        "the reseed waits out the failure backoff, not just the drain: \
         {first_backoff:?}"
    );

    until(
        || rx.len() == LANE_CHANNEL_CAPACITY,
        "generation 2 to lag as well",
    )
    .await;
    // Shrink first, so generation 3 has something that fits by the time the
    // drain below lets it start.
    fake.set_history(SID, transcript(2));
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
    let reset = next_event(&mut rx, "generation 3's Reset").await;
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
    assert_eq!(texts(&rest), transcript_texts(2));
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}
