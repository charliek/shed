//! The watcher, against `FakeGx`.
//!
//! Everything here is about ORDER and about what a client is told — the two
//! things a fold test cannot reach. The three cases plan 017 §3.4 names and C2
//! deferred are all here: the escalation ladder, a partial streak surviving an
//! EOF and a silent resume, and the seed/live overlap.
//!
//! **No test waits out a real window.** `common::fast()` scales
//! [`shed_gx::GxTimings`], and the watcher derives its backoff curve from
//! `down_after` for exactly this reason.

mod common;

use std::sync::Arc;
use std::time::Duration;

use serde_json::json;
use shed_core::lane::{AgentLane, LaneApprovalStatus, LaneEvent};
use shed_core::rc::RcActivity;
use shed_gx::discovery::StaticCredentials;
use shed_gx::testing::{FakeGx, DEFAULT_INSTANCE_ID, SENTINEL_TOKEN};
use shed_gx::transport::FixedDial;
use shed_gx::GxTimings;

use common::*;

/// A fake with one working session, its transcript, and the pin guard armed.
async fn seeded() -> FakeGx {
    let fake = FakeGx::start().await;
    // `idle` because the transcript below ends in `turn_completed`, and a
    // roster that disagreed with its own transcript would be a fixture, not a
    // gx.
    fake.add_session(SID, Some("a fixture session"), "/scratch", "idle", 0, false);
    fake.set_history(
        SID,
        vec![
            chunk(10, "user_message_chunk", "say pong", Some("p1")),
            chunk(12, "agent_message_chunk", "pong", Some("p1")),
            // Out of transcript order on purpose: gx's counters are bumped by
            // an atomic several sessions share, so 11 landing after 12 is
            // ordinary traffic and the cursor must still be the MAXIMUM.
            hook(11),
            turn_completed(13, "end_turn"),
        ],
    );
    fake.pin(SID);
    fake
}

// ---------------------------------------------------------------------------
// the seed
// ---------------------------------------------------------------------------

#[tokio::test]
async fn the_seed_opens_the_stream_first_and_publishes_in_the_pinned_order() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;

    let seed = until_ready(&mut rx).await;
    assert_eq!(
        shape(&seed),
        vec!["reset", "message", "message", "session", "ready"],
        "the pinned order is Reset → rows → Session → approvals → Ready: {:?}",
        shape(&seed)
    );
    assert_eq!(resets(&seed), vec![("connect".to_string(), 1)]);
    assert_eq!(texts(&seed), vec!["say pong", "pong"]);

    // The stream is opened BEFORE the seed reads anything, because an event
    // minted between a REST read and a later subscribe is gone forever.
    let paths = fake.paths();
    assert_eq!(paths[0], "/v1/healthz", "the pin comes first: {paths:?}");
    assert!(
        paths[1].ends_with("/events"),
        "the stream is opened before the seed GETs: {paths:?}"
    );
    assert_eq!(
        paths[2..],
        [
            format!("/v1/sessions/{SID}"),
            format!("/v1/sessions/{SID}/history"),
            format!("/v1/sessions/{SID}/approvals"),
        ],
        "the seed reads the row, the transcript and the approvals: {paths:?}"
    );

    // The credential invariant, on the wire: healthz carried no token, and
    // everything after it did.
    let requests = fake.requests();
    assert!(!requests[0].had_bearer, "healthz must be token-free");
    assert!(
        requests[1..].iter().all(|r| r.had_bearer && r.bearer_ok),
        "every route but healthz is authenticated"
    );
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

/// `dial()` runs before every connect — and this crate runs it before every
/// REQUEST, which is strictly more often and needs no bookkeeping to be right.
///
/// The count is exact rather than "at least one": a regression that cached the
/// dial URL for the life of the subscription would still pass a `>= 1`
/// assertion, and it is precisely that caching which makes a moved SSH forward
/// invisible until the next failure.
#[tokio::test]
async fn dial_runs_before_every_connect_and_before_every_request() {
    let fake = seeded().await;
    let dial = CountingDial::new(fake.dial_url());
    let lane = client_with(
        &fake,
        dial.clone(),
        Arc::new(
            StaticCredentials::from_parts(SENTINEL_TOKEN, DEFAULT_INSTANCE_ID).expect("token"),
        ),
        fast(),
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // One per pin (which is where `healthz` goes) plus one per bearer request:
    // the stream, then the row, the history and the approvals.
    assert_eq!(dial.calls(), 4, "seed dials: {:?}", fake.paths());

    let before = dial.calls();
    drop_streams(&fake).await;
    fake.push_update(SID, &chunk(20, "agent_message_chunk", "BACK", Some("p2")));
    until_text(&mut rx, "BACK").await;

    // The reconnect: the stream, then the reconcile's two GETs. A resume reads
    // no history.
    assert_eq!(
        dial.calls() - before,
        3,
        "reconnect dials: {:?}",
        fake.paths()
    );
}

#[tokio::test]
async fn the_seed_row_carries_gx_activity_and_the_resume_cursor_rides_every_message() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let seed = until_ready(&mut rx).await;

    let row = sessions(&seed).pop().expect("a Session frame");
    assert_eq!(row.id, SID);
    assert_eq!(row.title, "a fixture session");
    assert_eq!(row.pending_approvals, 0);
    assert!(
        !row.approximate,
        "gx reports it per row and this row earns false"
    );
    // At the seed the ROSTER is the newer of the two live sources — the row was
    // read after the history page — so it is the one that speaks.
    assert_eq!(row.activity, RcActivity::Idle, "{row:?}");

    // `history_cursor: true` is a promise about this field.
    let want = Some(format!("{SID}-13"));
    assert_eq!(
        cursors(&seed),
        vec![want.clone(), want],
        "every row carries the resume cursor, and it is the MAXIMUM counter"
    );
}

/// Activity has two LIVE sources — gx's roster row and this fold's verdict —
/// and they can only disagree about order, because both track the same turn. So
/// the newer one speaks. Here the fold speaks after the seed's row, and the
/// spinner starts without waiting for a roster frame.
#[tokio::test]
async fn the_fold_takes_over_the_activity_once_it_has_spoken_more_recently_than_the_roster() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let seed = until_ready(&mut rx).await;
    assert_eq!(
        sessions(&seed).pop().map(|s| s.activity),
        Some(RcActivity::Idle),
        "the roster row was read last, so it is what the seed publishes"
    );

    // A new turn starts. gx will send its own roster frame in a moment, but the
    // transcript already knows.
    fake.push_update(SID, &chunk(20, "user_message_chunk", "again", Some("p2")));
    loop {
        if let LaneEvent::Session { session } = next_event(&mut rx, "the working row").await {
            assert_eq!(session.activity, RcActivity::Working, "{session:?}");
            break;
        }
    }

    // …and when the roster does speak, it is the newer source again.
    fake.push_session_frame(
        SID,
        &json!({
            "sessionId": SID,
            "title": "a fixture session",
            "cwd": "/scratch",
            "activity": "needs_input",
            "pendingApprovals": 0,
            "approximate": false,
            "lastChangeUnixMs": 1_788_931_060_000i64,
        }),
    );
    loop {
        if let LaneEvent::Session { session } = next_event(&mut rx, "the roster's row").await {
            assert_eq!(session.activity, RcActivity::NeedsInput, "{session:?}");
            break;
        }
    }
}

/// **Seed/live overlap.** A frame that arrives while the seed is reading is
/// buffered, drained BEFORE `Ready`, and — when the seed's own history page
/// also carries it — folded exactly once.
#[tokio::test]
async fn frames_that_arrive_during_the_seed_are_drained_before_ready_and_never_duplicated() {
    let fake = seeded().await;
    // The injections fire when the session row is answered, and the history GET
    // that follows is held for long enough that they are certainly read into
    // the inbox rather than racing the fetch. The history page then also
    // carries them, which is the overlap: only the fold's `seen` set stops them
    // becoming two rows.
    //
    // The `turn_completed` is not decoration — it is what CLOSES the streak.
    // The seed deliberately does not flush an open one (a live stream is about
    // to arrive and finish the sentence), so a chunk on its own would leave the
    // row waiting on the flush timer rather than landing before `Ready`.
    fake.inject_on_get(
        &format!("/v1/sessions/{SID}"),
        &chunk(14, "agent_message_chunk", "OVERLAP", Some("p2")),
    );
    fake.inject_on_get(
        &format!("/v1/sessions/{SID}"),
        &turn_completed(15, "end_turn"),
    );
    fake.delay_get("/history", 250);

    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let seed = until_ready(&mut rx).await;

    let texts = texts(&seed);
    assert_eq!(
        texts.iter().filter(|t| t.as_str() == "OVERLAP").count(),
        1,
        "the buffered frame and the seed page carry the same envelope; it folds ONCE: {texts:?}"
    );
    assert_eq!(
        texts,
        vec!["say pong", "pong", "OVERLAP"],
        "and it is drained BEFORE Ready, in arrival order: {texts:?}"
    );
    assert_eq!(shape(&seed).last(), Some(&"ready"));
}

#[tokio::test]
async fn a_caller_supplied_cursor_is_not_silently_swallowed() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, Some(format!("{SID}-10"))).await;
    let seed = until_ready(&mut rx).await;

    assert_eq!(
        resets(&seed),
        vec![("connect:cursor-ignored".to_string(), 1)],
        "a client that handed over a cursor must not be left believing it resumed"
    );
    // And the connect really did carry no `Last-Event-ID`, so gx never had to
    // decide whether it could place one.
    let events = fake.requests_to("/events");
    assert_eq!(events.len(), 1);
    assert_eq!(events[0].last_event_id, None, "{events:?}");
}

// ---------------------------------------------------------------------------
// the silent resume
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_stream_that_ends_resumes_silently_from_the_maximum_counter() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let seed = until_ready(&mut rx).await;
    let generation = resets(&seed)[0].1;
    fake.clear_requests();

    // The lane goes away and comes back, and something happened in the gap.
    drop_streams(&fake).await;
    fake.push_update(SID, &chunk(20, "agent_message_chunk", "GAP", Some("p2")));

    let seen = until_text(&mut rx, "GAP").await;
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
        "a resume the server accepted is SILENT — no Reset, no restaged view: {:?}",
        shape(&seen)
    );
    assert_eq!(
        texts(&seen).iter().filter(|t| t.as_str() == "GAP").count(),
        1,
        "the gap frame arrives exactly once"
    );

    // The cursor is the MAXIMUM counter, not the last-applied one: the seed's
    // last id-bearing envelope in transcript order was 13, and 13 is also the
    // maximum here — the hook at 11 arrived after 12 and must not have become
    // the cursor.
    let events = fake.requests_to("/events");
    assert_eq!(events.len(), 1, "exactly one reconnect: {events:?}");
    assert_eq!(
        events[0].last_event_id.as_deref(),
        Some(&*format!("{SID}-13"))
    );

    // Nothing about the generation changed, which is what the client's view
    // surviving MEANS.
    fake.push_reset(SID, "slow_consumer");
    let after = next_event(&mut rx, "the reseed the server asked for").await;
    match after {
        LaneEvent::Reset { generation: g, .. } => assert_eq!(
            g,
            generation + 1,
            "the silent resume did not consume a generation"
        ),
        other => panic!("expected the server-driven Reset, got {other:?}"),
    }
}

/// **A partial streak across an EOF.** An open streak survives a silent resume
/// — no duplicate half-row, and no lost text.
#[tokio::test]
async fn a_partial_streak_survives_an_eof_and_a_silent_resume() {
    // The seeded transcript is kept, and that is not incidental: it is what
    // gives the fold a cursor to resume FROM. Starting from an empty history
    // would make the test a coin flip — if the half-sentence lost its race with
    // the hangup there would be no cursor at all, and the reconnect would
    // correctly (but uninterestingly) reseed.
    let fake = seeded().await;
    // The flush timer is taken out of the picture: this test is about the EOF,
    // and a streak flushed on a timer would be a different (also correct) row.
    let lane = client_for(
        &fake,
        GxTimings {
            flush_after: Duration::from_secs(30),
            ..fast()
        },
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // Half a sentence, then the stream dies mid-turn.
    fake.push_update(SID, &chunk(20, "agent_message_chunk", "Hello ", Some("p2")));
    drop_streams(&fake).await;
    // The rest of it lands on the resumed connection.
    fake.push_update(SID, &chunk(21, "agent_message_chunk", "world.", Some("p2")));
    fake.push_update(SID, &turn_completed(22, "end_turn"));

    let seen = until_text(&mut rx, "world").await;
    assert_eq!(
        texts(&seen),
        vec!["Hello world."],
        "the streak was neither flushed early nor restarted: {:?}",
        texts(&seen)
    );
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
        "and none of it cost the client a reseed: {:?}",
        resets(&seen)
    );
}

#[tokio::test]
async fn an_open_streak_silent_for_flush_after_is_emitted_as_a_partial_row() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // A turn that stops mid-sentence — the model is thinking, the tool is slow.
    // Without the flush the client would see nothing at all.
    fake.push_update(SID, &chunk(20, "agent_message_chunk", "thinking", None));
    let first = until_text(&mut rx, "thinking").await;
    assert_eq!(
        texts(&first),
        vec!["thinking"],
        "the streak is emitted as a partial row rather than waiting for a turn \
         that has not ended"
    );

    // The next chunk opens a NEW streak rather than reopening the flushed one:
    // rows are append-only, so a flush is a segment boundary.
    fake.push_update(SID, &chunk(21, "agent_message_chunk", " harder", None));
    let second = until_text(&mut rx, "harder").await;
    assert_eq!(texts(&second), vec![" harder"], "{:?}", texts(&second));
}

/// …and the flush clock is the STREAK's, not the connection's.
///
/// This is the case the test above cannot see, because it goes fully silent and
/// then passes under either clock. On real wire a stalled streak is almost
/// never silent: `hook_execution` is an ignored kind that §3.4 requires to be
/// *transparent* to a streak, and it is **15 of every 40 frames** in the
/// committed recording, interleaving chunk streaks constantly.
///
/// So a flush timer driven by bytes-on-the-connection is reset by frames that
/// are by definition invisible, and a turn that has stopped producing text
/// sits behind that trickle showing the user nothing at all. The timer has to
/// run from the last time the STREAK itself grew.
#[tokio::test]
async fn a_stalled_streak_flushes_even_while_ignored_frames_keep_arriving() {
    let fake = Arc::new(seeded().await);
    let timings = fast();
    let lane = client_for(&fake, timings.clone());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // The turn stops mid-sentence…
    fake.push_update(
        SID,
        &chunk(20, "agent_message_chunk", "half a thought", None),
    );

    // …but the connection stays busy with frames that contribute no transcript.
    // They keep the stream ALIVE (the stall timer must see them) and must not
    // keep the streak from flushing.
    let pump = tokio::spawn({
        let fake = Arc::clone(&fake);
        let every = timings.flush_after / 4;
        async move {
            for n in 30..200u64 {
                tokio::time::sleep(every).await;
                fake.push_update(SID, &hook(n));
            }
        }
    });

    let seen = until_text(&mut rx, "half a thought").await;
    pump.abort();

    assert_eq!(
        texts(&seen),
        vec!["half a thought"],
        "the partial row must reach the client while the ignored frames are \
         still arriving: {:?}",
        shape(&seen)
    );
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Down { .. })),
        "and the ignored frames kept the stream alive, so nothing stalled: {:?}",
        shape(&seen)
    );
}

/// A cursor that cannot be sent as a header is a LOST cursor, and it says so.
///
/// The failure mode this guards is silent and total: drop the unusable cursor,
/// connect anyway, and gx streams live-only while the watcher still believes it
/// resumed — no `Reset`, no `Ready`, and every frame between the cursor and now
/// simply missing from the transcript with nothing to indicate it.
///
/// The session id reaches this adapter off a roost tab, so it is untrusted, and
/// the resume cursor is built from it (`<session-id>-<counter>`). A control
/// character anywhere in that id is therefore enough to produce a cursor no
/// header can carry.
#[tokio::test]
async fn a_cursor_that_cannot_be_a_header_reseeds_rather_than_streaming_live_only() {
    // DEL (0x7f) is outside the printable range a header value allows, and it
    // survives the client's percent-encoding of the path, so every ROUTE still
    // addresses the right session — only the header is impossible.
    let odd = format!("01a0fa1e-0000-7000-8000-0000{}00ab", '\u{7f}');
    let event_id = format!("{odd}-5");

    let fake = FakeGx::start().await;
    fake.add_session(&odd, Some("odd id"), "/scratch", "idle", 0, false);
    fake.set_history(
        &odd,
        vec![
            json!({
                "eventId": event_id,
                "method": "session/update",
                "params": {
                    "sessionId": odd,
                    "update": {
                        "sessionUpdate": "agent_message_chunk",
                        "content": { "type": "text", "text": "before the break" },
                    },
                    "_meta": { "agentTimestampMs": 1_788_931_000_000i64 },
                },
            }),
            // The turn is closed, so the seed emits its row rather than leaving
            // the streak open for the live stream to finish.
            json!({
                "eventId": format!("{odd}-6"),
                "method": "_x.ai/session/update",
                "params": {
                    "sessionId": odd,
                    "update": { "sessionUpdate": "turn_completed", "stop_reason": "end_turn" },
                },
            }),
        ],
    );
    fake.pin(&odd);

    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, &odd, None).await;
    let seed = until_ready(&mut rx).await;
    assert_eq!(resets(&seed), vec![("connect".to_string(), 1)]);
    fake.clear_requests();

    // The stream drops. The fold holds a perfectly good cursor — it just cannot
    // be spelled in a header.
    fake.close_streams();

    let recovered = until_ready(&mut rx).await;
    assert_eq!(
        resets(&recovered),
        vec![("cursor_lost".to_string(), 2)],
        "an unusable cursor takes the cursor_lost path, so the client is told to \
         restage rather than left holding a transcript with a silent hole: {:?}",
        shape(&recovered)
    );
    assert_eq!(
        texts(&recovered),
        vec!["before the break"],
        "and the reseed rebuilt the transcript from history"
    );

    // The reconnect really did go out without a cursor, which is what makes it
    // a reseed rather than a resume that lost its header.
    let events = fake.requests_to("/events");
    assert_eq!(events.len(), 1, "{events:?}");
    assert_eq!(events[0].last_event_id, None, "{events:?}");
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

// ---------------------------------------------------------------------------
// reconcile
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_reconnect_reconciles_an_approval_the_stream_could_not_replay() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // An `approval` frame carries no `id:`, so a cursor resume never replays
    // one. Raising it while the stream is down is the case the reconcile GETs
    // exist for.
    drop_streams(&fake).await;
    fake.add_approval(
        SID,
        "call_fixture",
        "permission",
        "session/request_permission",
        &permission_request("id -un"),
    );

    let seen = until_approval(&mut rx, "call_fixture").await;
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
        "reconciling is not reseeding: {:?}",
        shape(&seen)
    );
    let approval = approvals(&seen).pop().expect("the Approval frame");
    assert_eq!(approval.status, LaneApprovalStatus::Pending);
    assert_eq!(
        approval
            .options
            .iter()
            .map(|o| o.id.as_str())
            .collect::<Vec<_>>(),
        vec![
            "enable-always-approve",
            "allow-always-command",
            "allow-once",
            "reject-once",
            "reject-always-command"
        ],
        "the ids stay opaque and in the order gx offered them"
    );
    // And it left a trace in the transcript, so a reader scrolling back sees
    // where the agent stopped to ask.
    assert!(
        messages(&seen)
            .iter()
            .any(|m| m.msg_type == "approval_request"),
        "{:?}",
        messages(&seen)
    );
}

#[tokio::test]
async fn a_held_approval_the_refetch_no_longer_lists_is_tombstoned_resolved() {
    let fake = seeded().await;
    fake.add_approval(
        SID,
        "call_fixture",
        "permission",
        "session/request_permission",
        &permission_request("id -un"),
    );
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let seed = until_ready(&mut rx).await;
    assert_eq!(
        approvals(&seed).pop().map(|a| a.status),
        Some(LaneApprovalStatus::Pending),
        "the seed publishes what is waiting on the human"
    );
    // The lane's own row says so too, and it OVERRIDES the roster's `working`.
    assert_eq!(
        sessions(&seed)
            .pop()
            .map(|s| (s.activity, s.pending_approvals)),
        Some((RcActivity::NeedsApproval, 1))
    );

    // Answered in the TUI while the lane was down: gx stops listing it, and the
    // panel would otherwise stay blocked on a question nobody can answer.
    drop_streams(&fake).await;
    fake.resolve_approval(SID, "call_fixture");

    let seen = until_approval(&mut rx, "call_fixture").await;
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
        "the tombstone rides a silent resume: {:?}",
        shape(&seen)
    );
    assert_eq!(
        approvals(&seen).pop().map(|a| a.status),
        Some(LaneApprovalStatus::Resolved),
        "a held-pending approval the refetch no longer lists is tombstoned"
    );
    // Append-only: the tombstone is a frame, never a second transcript row.
    assert!(
        !messages(&seen)
            .iter()
            .any(|m| m.msg_type == "approval_request"),
        "{:?}",
        messages(&seen)
    );
}

// ---------------------------------------------------------------------------
// the ladder
// ---------------------------------------------------------------------------

/// **The escalation ladder**: three SILENT resumes, then `Reset{cursor_lost}`,
/// then `Down` once reseeds have been failing for `down_after`.
///
/// The lane keeps LISTENING and refuses the stream (`503 leader_unavailable`)
/// rather than having its port taken away, and that is what makes the
/// assertion exact: every attempt lands in the ledger with the
/// `Last-Event-ID` it carried, so "three silent resumes" is counted from the
/// wire instead of from a frame-arrival race. A cursor is only ever sent by a
/// RESUME — a reseed connects without one — so the count is unambiguous, and
/// it is still three after the run has finished escalating.
#[tokio::test]
async fn the_ladder_is_three_silent_resumes_then_cursor_lost_then_down() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;
    fake.clear_requests();

    // The leader goes away: the stream is refused from here on.
    fake.fail("/events", 503, "leader_unavailable", "the leader is gone");
    fake.close_streams();

    let rest = until_down(&mut rx).await;
    let reasons: Vec<String> = resets(&rest).into_iter().map(|(r, _)| r).collect();
    assert_eq!(
        reasons.first().map(String::as_str),
        Some("cursor_lost"),
        "the three resume attempts are SILENT; the first frame the client sees \
         after Ready is the escalation: {:?}",
        shape(&rest)
    );
    assert!(
        reasons[1..].iter().all(|r| r == "reconnect"),
        "and once the fold was discarded there is no cursor left to lose: {reasons:?}"
    );

    let attempts = fake.requests_to("/events");
    let resumed: Vec<&Option<String>> = attempts
        .iter()
        .map(|r| &r.last_event_id)
        .filter(|c| c.is_some())
        .collect();
    assert_eq!(
        resumed.len(),
        3,
        "exactly three attempts carried a cursor — `resume_tries` — out of {} \
         connects in total",
        attempts.len()
    );
    assert!(
        resumed
            .iter()
            .all(|c| c.as_deref() == Some(&*format!("{SID}-13"))),
        "and every one of them resumed from the MAXIMUM counter: {resumed:?}"
    );

    let down = down_reason(&rest).expect("the terminal Down");
    assert!(
        down.starts_with("unreachable:"),
        "reseeds that keep failing for down_after end the subscription: {down}"
    );
}

#[tokio::test]
async fn a_lane_that_comes_back_recovers_rather_than_ending() {
    let fake = Arc::new(seeded().await);
    let lane = client_for(
        &fake,
        GxTimings {
            // Room to come back: the ladder must be allowed to run to
            // `cursor_lost` without `down_after` firing first.
            down_after: Duration::from_secs(20),
            ..fast()
        },
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    fake.stop_listening().await;
    // Wait for the escalation, then give the port back.
    loop {
        match next_event(&mut rx, "the cursor_lost escalation").await {
            LaneEvent::Reset { reason, .. } if reason == "cursor_lost" => break,
            LaneEvent::Down { reason } => panic!("gave up too early: {reason}"),
            _ => {}
        }
    }
    fake.start_listening().await;

    let recovered = until_ready(&mut rx).await;
    assert_eq!(
        shape(&recovered).last(),
        Some(&"ready"),
        "the reseed completes once the lane is back: {:?}",
        shape(&recovered)
    );
    assert_eq!(
        texts(&recovered),
        vec!["say pong", "pong"],
        "and it is a full reseed, rebuilt from history"
    );
}

// ---------------------------------------------------------------------------
// the server's own reset, and the ends of a subscription
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_server_reset_frame_reseeds_under_its_own_reason() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    let seed = until_ready(&mut rx).await;
    let last_seq = messages(&seed).last().map(|m| m.seq).expect("seeded rows");

    fake.push_reset(SID, "slow_consumer");
    let again = until_ready(&mut rx).await;
    assert_eq!(
        resets(&again),
        vec![("server_reset:slow_consumer".to_string(), 2)],
        "the reason is gx's, verbatim, and nothing branches on it"
    );
    assert_eq!(
        texts(&again),
        vec!["say pong", "pong"],
        "a reseed rebuilds the whole transcript"
    );
    // §0's invariant 3: a reseed rebuilds the RING along with everything else,
    // so the new generation numbers its rows from the start. A `seq` that kept
    // climbing would be numbering a fresh view as a continuation of the one the
    // client was just told to discard.
    let reseeded: Vec<u64> = messages(&again).iter().map(|m| m.seq).collect();
    assert!(
        reseeded.iter().all(|seq| *seq <= last_seq),
        "the ring restarted with the rest of the generation: {reseeded:?} \
         against a pre-reset high-water mark of {last_seq}"
    );
    assert_eq!(
        reseeded,
        messages(&seed).iter().map(|m| m.seq).collect::<Vec<_>>(),
        "and it numbered the rebuilt transcript exactly as the first seed did"
    );
}

#[tokio::test]
async fn a_removed_session_frame_ends_the_subscription() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    fake.push_session_removed(SID);
    let rest = until_down(&mut rx).await;
    assert_eq!(down_reason(&rest).as_deref(), Some("session_removed"));
}

#[tokio::test]
async fn an_unknown_session_ends_the_subscription_without_retrying() {
    let fake = FakeGx::start().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, "01a0-dead-0", None).await;

    let rest = until_down(&mut rx).await;
    assert_eq!(
        shape(&rest),
        vec!["reset", "down"],
        "no retry loop: a session gx does not know is not coming back"
    );
    assert_eq!(down_reason(&rest).as_deref(), Some("unknown_session"));
}

#[tokio::test]
async fn a_refused_token_ends_the_subscription_and_is_not_retried() {
    let fake = seeded().await;
    // A well-formed token that is not the fake's. `healthz` is token-free, so
    // the pin succeeds and the refusal lands on the first bearer request —
    // which is the stream itself.
    let wrong = "0".repeat(64);
    let lane = client_with(
        &fake,
        Arc::new(FixedDial::new(fake.dial_url())),
        Arc::new(StaticCredentials::from_parts(&wrong, DEFAULT_INSTANCE_ID).expect("token")),
        fast(),
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;

    let rest = until_down(&mut rx).await;
    assert_eq!(down_reason(&rest).as_deref(), Some("unauthorized"));
    assert_eq!(
        fake.requests_to("/events").len(),
        1,
        "one refusal is enough: retrying a rejected credential is a loop, not a repair"
    );
}

// ---------------------------------------------------------------------------
// the pin, across a leader restart
// ---------------------------------------------------------------------------

/// A leader restart is a NEW `instanceId` on the SAME token, with the ring
/// dropped and the transcript still on disk. The pin has to notice, re-discover
/// and resume — from gx's fourth replay rule, the one that reads the disk.
#[tokio::test]
async fn a_leader_restart_re_pins_and_resumes_from_the_persisted_transcript() {
    let fake = Arc::new(seeded().await);
    let creds = FollowingCreds::new(Arc::clone(&fake));
    let lane = client_with(
        &fake,
        Arc::new(FixedDial::new(fake.dial_url())),
        creds.clone(),
        fast(),
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;
    assert_eq!(
        lane.pinned_instance().await.as_deref(),
        Some(DEFAULT_INSTANCE_ID)
    );

    drop_streams(&fake).await;
    fake.restart_leader("beefbeefbeefbeefbeefbeefbeefbeef");
    // Written by the new leader, into the transcript the old one left behind.
    fake.push_update(SID, &chunk(20, "agent_message_chunk", "AFTER", Some("p2")));

    let seen = until_text(&mut rx, "AFTER").await;
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Reset { .. })),
        "a restart the cursor survives costs the client nothing: {:?}",
        shape(&seen)
    );
    assert_eq!(
        lane.pinned_instance().await.as_deref(),
        Some("beefbeefbeefbeefbeefbeefbeefbeef"),
        "every reconnect opens a new epoch, so the new leader was health-checked \
         and re-pinned before a single token went out"
    );
    assert!(
        creds.calls() >= 2,
        "and the record was re-read rather than remembered"
    );
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}

// ---------------------------------------------------------------------------
// odds and ends the stream has to survive
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_keepalive_is_liveness_and_an_undecodable_frame_is_not_fatal() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // gx's keep-alive is a comment line carrying no event at all: a stall timer
    // that counted EVENTS would tear down every healthy idle stream.
    fake.push_keepalive();
    // A frame this build cannot read costs one row, never the subscription.
    fake.push_update(
        SID,
        &json!({ "eventId": eid(20), "params": "not an object" }),
    );
    fake.push_update(
        SID,
        &chunk(21, "agent_message_chunk", "STILL HERE", Some("p2")),
    );

    let seen = until_text(&mut rx, "STILL HERE").await;
    assert!(
        !seen.iter().any(|e| matches!(e, LaneEvent::Down { .. })),
        "{:?}",
        shape(&seen)
    );
}

#[tokio::test]
async fn dropping_the_subscription_releases_the_stream() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;
    assert_eq!(fake.stream_count(), 1);

    drop(stop);
    drop(rx);
    until(|| fake.stream_count() == 0, "the socket to be released").await;
}

// ---------------------------------------------------------------------------
// the ladder's ordering, the terminal flush, and the pin's lifetime
// ---------------------------------------------------------------------------

/// **`down_after` is the RESEED clock.** A slow network must not let the pump
/// skip the ladder.
///
/// Measuring it from the first loss instead lets three slow resume failures
/// exhaust the budget between them, so the subscription ends *before* the third
/// resume and before any `cursor_lost` reseed — §3.4 spends `Down` on reseeds
/// that keep failing, not on a ladder that is still climbing. Here each attempt
/// costs 400 ms against a 700 ms `down_after`, which is enough for the old
/// accounting to give up after two.
#[tokio::test]
async fn a_slow_network_still_gets_the_whole_ladder_before_down() {
    let fake = seeded().await;
    let lane = client_for(
        &fake,
        GxTimings {
            down_after: Duration::from_millis(700),
            ..fast()
        },
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // Every connect from here is slow AND fails.
    fake.fail("/events", 503, "leader_unavailable", "the leader is gone");
    fake.delay_get("/events", 400);
    fake.close_streams();

    let rest = until_down(&mut rx).await;
    let reasons: Vec<String> = resets(&rest).into_iter().map(|(r, _)| r).collect();
    assert!(
        reasons.iter().any(|r| r == "cursor_lost"),
        "the ladder must reach cursor_lost before Down however slow each attempt \
         is — resumes are bounded by resume_tries, not by the reseed budget. \
         Saw: {reasons:?}"
    );
    assert!(
        down_reason(&rest).is_some_and(|d| d.starts_with("unreachable:")),
        "{:?}",
        down_reason(&rest)
    );
}

/// §3.4: an open streak "is flushed as a partial row at `Down`".
///
/// A terminal event arriving before `flush_after` would otherwise take the last
/// thing the agent said with it — and there is no later generation to recover
/// it, because the subscription is over. `flush_after` is parked at 30 s here
/// so the ONLY thing that can produce the row is the terminal path itself.
#[tokio::test]
async fn a_terminal_event_flushes_the_open_streak_before_down() {
    let fake = seeded().await;
    let lane = client_for(
        &fake,
        GxTimings {
            flush_after: Duration::from_secs(30),
            ..fast()
        },
    );
    let (mut rx, _stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;

    // Mid-sentence, and then the session is removed underneath it.
    fake.push_update(
        SID,
        &chunk(20, "agent_message_chunk", "the last thing", None),
    );
    fake.push_session_removed(SID);

    let rest = until_down(&mut rx).await;
    assert_eq!(
        texts(&rest),
        vec!["the last thing"],
        "the partial row must survive the way down: {:?}",
        shape(&rest)
    );
    assert_eq!(
        shape(&rest).last(),
        Some(&"down"),
        "and it is emitted BEFORE the Down, not after: {:?}",
        shape(&rest)
    );
    assert_eq!(down_reason(&rest).as_deref(), Some("session_removed"));
}

/// A cancelled pump must not leave the shared client pinned.
///
/// `run` unpins after every generation, but `LaneStop` ABORTS the task, and an
/// abort while the pump awaits the SSE body runs none of those arms. The epoch
/// would stay pinned to a connection that no longer exists, and the next verb
/// on the same client would skip `healthz` — the one check that notices a
/// different leader answering on the same address.
#[tokio::test]
async fn aborting_a_subscription_leaves_no_pinned_epoch_behind() {
    let fake = seeded().await;
    let lane = client_for(&fake, fast());
    let (mut rx, stop) = subscribed(&lane, SID, None).await;
    until_ready(&mut rx).await;
    let pinned_by_the_seed = fake.requests_to("/v1/healthz").len();
    assert_eq!(pinned_by_the_seed, 1, "the seed pinned exactly once");

    // Abort mid-stream — the pump is parked on the SSE body.
    drop(stop);
    drop(rx);
    until(|| fake.stream_count() == 0, "the stream to be released").await;

    // The next verb on the SAME client must re-establish the pin.
    lane.session(SID).await.expect("a verb after the abort");
    assert_eq!(
        fake.requests_to("/v1/healthz").len(),
        pinned_by_the_seed + 1,
        "the aborted pump's epoch was inherited instead of being dropped"
    );
    assert!(fake.violations().is_empty(), "{:?}", fake.violations());
}
