//! The streaming half: the generation bracket, the pinned seed order, the
//! scoping rules, and every way a generation can end.

mod common;

use std::time::Duration;

use common::{
    approval_ids, assert_clean, assistant_turn, marker, message_text, next_event, permission_asked,
    seqs, texts, until_ready, until_text, wait_for,
};
use serde_json::json;
use shed_core::lane::{AgentLane, LaneError, LaneEvent, LaneSubscription};
use shed_core::rc::RcActivity;
use shed_opencode::client::STREAM_HEAD_TIMEOUT;
use shed_opencode::testing::FakeOpencode;
use shed_opencode::OpencodeClient;

fn client(fake: &FakeOpencode) -> OpencodeClient {
    OpencodeClient::new(fake.base_url(), None).expect("the client builds")
}

/// One root session in `/w`, with a two-turn transcript, pinned.
async fn one_session() -> FakeOpencode {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "root", "/w", None);
    fake.set_simple_transcript("ses_a", "hello", "hi there");
    fake.set_status("ses_a", "idle");
    fake.pin("ses_a");
    fake
}

async fn subscribe(fake: &FakeOpencode, id: &str) -> LaneSubscription {
    client(fake)
        .subscribe(id, None)
        .await
        .expect("the subscription opens")
}

#[tokio::test]
async fn the_seed_is_bracketed_by_reset_and_ready() {
    let fake = one_session().await;
    fake.add_permission("ses_a", "per_1", "bash", "ls");
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();

    let seed = until_ready(&mut rx).await;

    match &seed[0] {
        LaneEvent::Reset { reason, generation } => {
            assert_eq!(reason, "seed", "the first connect's reason");
            assert_eq!(*generation, 1);
        }
        other => panic!("the stream must OPEN with a Reset, got {other:?}"),
    }
    match seed.last().expect("a last frame") {
        LaneEvent::Ready { generation } => assert_eq!(*generation, 1),
        other => panic!("the seed must CLOSE with Ready, got {other:?}"),
    }

    // The transcript, in order, with ring-assigned seqs from 1.
    let rows = texts(&seed);
    assert_eq!(rows[0], "hello");
    assert_eq!(rows[1], "hi there");
    assert_eq!(seqs(&seed)[..2], [1, 2]);

    // The session row, then the approvals — the order `shed_core::lane`'s
    // module doc pins for a seed.
    let session_at = seed
        .iter()
        .position(|e| matches!(e, LaneEvent::Session { .. }))
        .expect("a Session frame");
    let approval_at = seed
        .iter()
        .position(|e| matches!(e, LaneEvent::Approval { .. }))
        .expect("an Approval frame");
    assert!(
        session_at < approval_at,
        "messages, then session, then approvals"
    );

    let LaneEvent::Session { session } = &seed[session_at] else {
        unreachable!()
    };
    assert_eq!(session.id, "ses_a");
    assert_eq!(session.cwd, "/w");
    assert_eq!(
        session.activity,
        RcActivity::NeedsApproval,
        "an open ask blocks the session"
    );
    assert_eq!(session.pending_approvals, 1);
    assert!(!session.approximate, "a live fold is not a poll");
    assert_eq!(approval_ids(&seed), vec!["per_1"]);

    // `/event` is instance-scoped, like the three REST reads beside it.
    let gets = fake.get_paths();
    assert!(
        gets.iter().any(|g| g == "/event?directory=%2Fw"),
        "{gets:?}"
    );
    assert!(gets.iter().any(|g| g == "/session/status?directory=%2Fw"));
    assert!(gets.iter().any(|g| g == "/permission?directory=%2Fw"));
    assert!(gets.iter().any(|g| g == "/question?directory=%2Fw"));
    // Id-addressed routes send NO directory — the id is the address.
    assert!(gets.iter().any(|g| g == "/session/ses_a"));
    assert!(gets.iter().any(|g| g == "/session/ses_a/message"));
    assert!(gets.iter().any(|g| g == "/session/ses_a/children"));
    assert_clean(&fake);
}

#[tokio::test]
async fn a_cursor_this_adapter_cannot_honor_names_itself_on_the_reset() {
    let fake = one_session().await;
    let (mut rx, _stop) = client(&fake)
        .subscribe("ses_a", Some("some-old-cursor".to_string()))
        .await
        .expect("the subscription opens")
        .into_parts();
    match next_event(&mut rx, "the opening Reset").await {
        LaneEvent::Reset { reason, generation } => {
            assert_eq!(
                reason, "cursor_unresolvable",
                "history_cursor is false: say so instead of silently ignoring it"
            );
            assert_eq!(generation, 1);
        }
        other => panic!("got {other:?}"),
    }
    assert_clean(&fake);
}

#[tokio::test]
async fn a_live_delta_arrives_after_ready() {
    let fake = one_session().await;
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let seed = until_ready(&mut rx).await;
    let last_seq = *seqs(&seed).last().expect("a seeded row");

    for frame in assistant_turn("ses_a", "msg_live", "prt_live", "pong") {
        fake.push_event(&frame);
    }

    let ev = next_event(&mut rx, "the live transcript row").await;
    match &ev {
        LaneEvent::Message { message, cursor } => {
            assert_eq!(message.text.as_deref(), Some("pong"));
            assert_eq!(message.role, "assistant");
            assert!(message.seq > last_seq, "seq keeps counting past the seed");
            assert_eq!(*cursor, None);
        }
        other => panic!("got {other:?}"),
    }
    assert_clean(&fake);
}

/// The reason `/event` is opened BEFORE the REST seed. The fake pushes the
/// event while it is answering the seed's `/session/{id}/message` read, and the
/// assertion is EXACTLY ONCE — the seed-then-subscribe order loses it, and a
/// buffer that is drained twice duplicates it.
#[tokio::test]
async fn an_event_injected_during_the_seed_is_neither_lost_nor_duplicated() {
    let fake = one_session().await;
    fake.inject_on_get(
        "/session/ses_a/message",
        &marker("ses_a", "during-the-seed"),
    );

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let seed = until_ready(&mut rx).await;

    // A sentinel pushed after Ready bounds the window: everything the injected
    // event could possibly appear in has been received by the time it arrives.
    fake.push_event(&marker("ses_a", "sentinel"));
    let mut all = seed;
    all.extend(until_text(&mut rx, "sentinel").await);

    let hits = all
        .iter()
        .filter(|e| message_text(e).is_some_and(|t| t.contains("during-the-seed")))
        .count();
    assert_eq!(hits, 1, "exactly once — not lost, not duplicated: {all:#?}");
    assert_clean(&fake);
}

#[tokio::test]
async fn a_reconnect_is_a_new_generation_with_monotonic_seq() {
    let fake = one_session().await;
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let first = until_ready(&mut rx).await;
    let highest = *seqs(&first).last().expect("a seeded row");

    fake.close_streams();

    let second = until_ready(&mut rx).await;
    match &second[0] {
        LaneEvent::Reset { reason, generation } => {
            assert_eq!(reason, "reconnect");
            assert_eq!(*generation, 2, "each connection is a generation");
        }
        other => panic!("a reconnect must open with a Reset, got {other:?}"),
    }
    match second.last().expect("a last frame") {
        LaneEvent::Ready { generation } => assert_eq!(*generation, 2),
        other => panic!("got {other:?}"),
    }

    // The seed replays in full (the fold was reset) …
    assert_eq!(texts(&second), vec!["hello", "hi there"]);
    // … but the RING is not, so seq is monotonic ACROSS the reset — the rule a
    // client's "seq went backwards → refetch" check depends on.
    let replayed = seqs(&second);
    assert!(
        replayed[0] > highest,
        "seq {replayed:?} must continue past {highest}"
    );
    assert!(replayed.windows(2).all(|w| w[0] < w[1]));
    assert_clean(&fake);
}

/// More frames than the inbox holds arrive while the seed is parked, so they
/// cannot be dropped one-by-one: the generation is abandoned and reseeded.
#[tokio::test]
async fn an_inbox_overflow_forces_a_reseed() {
    let fake = one_session().await;
    fake.hold_get("/session/ses_a/message");
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();

    match next_event(&mut rx, "the opening Reset").await {
        LaneEvent::Reset { generation, .. } => assert_eq!(generation, 1),
        other => panic!("got {other:?}"),
    }
    wait_for("the seed to park on /message", || {
        fake.get_paths()
            .iter()
            .any(|g| g == "/session/ses_a/message")
    })
    .await;

    // Comfortably past MAX_INBOX_ITEMS (1024). They are irrelevant frames on
    // purpose: what overflows the inbox is COUNT, not content.
    for i in 0..1_200 {
        fake.push_event(&json!({
            "type": "message.part.updated",
            "properties": { "sessionID": "ses_a", "part": { "id": format!("prt_{i}") } },
        }));
    }

    match next_event(&mut rx, "the overflow Reset").await {
        LaneEvent::Reset { reason, generation } => {
            assert_eq!(reason, "overflow");
            assert_eq!(generation, 2, "an overflow reconnects and reseeds");
        }
        other => panic!("got {other:?}"),
    }

    fake.release_get("/session/ses_a/message");
    let second = until_ready(&mut rx).await;
    assert!(
        matches!(second.last(), Some(LaneEvent::Ready { generation: 2 })),
        "the reseed completes: {second:#?}"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn a_reseed_that_404s_ends_the_subscription() {
    let fake = one_session().await;
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    until_ready(&mut rx).await;

    // The session is deleted (in the TUI, or by another client) and the stream
    // drops. No amount of retrying brings it back.
    fake.remove_session("ses_a");
    fake.close_streams();

    match next_event(&mut rx, "the reconnect's Reset").await {
        LaneEvent::Reset { generation, .. } => assert_eq!(generation, 2),
        other => panic!("got {other:?}"),
    }
    match next_event(&mut rx, "the Down").await {
        LaneEvent::Down { reason } => assert_eq!(reason, "unknown_session"),
        other => panic!("a 404 on reseed is terminal, got {other:?}"),
    }
    assert!(
        tokio::time::timeout(Duration::from_secs(2), rx.recv())
            .await
            .expect("the channel closes promptly")
            .is_none(),
        "the task ENDS after Down — the contract's 'this subscription is over'"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn a_first_connect_that_never_comes_up_is_down_not_a_retry_loop() {
    let fake = one_session().await;
    // `subscribe` resolves the directory first and succeeds; the pump's own
    // `/event` open is what fails.
    fake.fail_get("/event", 500);
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();

    match next_event(&mut rx, "the opening Reset").await {
        LaneEvent::Reset { generation, .. } => assert_eq!(generation, 1),
        other => panic!("got {other:?}"),
    }
    match next_event(&mut rx, "the Down").await {
        LaneEvent::Down { reason } => assert!(
            reason.contains("500"),
            "the Down names what was tried: {reason}"
        ),
        other => panic!("got {other:?}"),
    }
    assert_clean(&fake);
}

#[tokio::test]
async fn a_401_refuses_the_subscription_up_front() {
    let fake = FakeOpencode::start_with_auth("opencode", "hunter2").await;
    fake.add_session("ses_a", "root", "/w", None);
    let err = client(&fake)
        .subscribe("ses_a", None)
        .await
        .err()
        .expect("no credentials");
    assert_eq!(err, LaneError::Unauthorized);
    assert_clean(&fake);
}

/// The plan-012 bug, made unrepresentable: two ROOT sessions in ONE directory.
/// The id is the only filter — no directory matching, no `not_before`, no
/// claim.
#[tokio::test]
async fn two_sessions_in_one_directory_never_cross() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "A", "/w", None);
    fake.add_session("ses_b", "B", "/w", None);
    fake.set_simple_transcript("ses_a", "hello A", "reply A");
    fake.set_simple_transcript("ses_b", "hello B", "reply B");
    fake.set_status("ses_a", "idle");
    fake.set_status("ses_b", "busy");
    fake.add_permission("ses_b", "per_b", "bash", "rm -rf /");
    fake.pin("ses_a");

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let seed = until_ready(&mut rx).await;

    // A's seed never includes B's history …
    assert_eq!(texts(&seed), vec!["hello A", "reply A"]);
    // … nor B's approvals, even though `/permission?directory=/w` returned it.
    assert!(approval_ids(&seed).is_empty(), "{seed:#?}");

    // B's live traffic never surfaces on A, including its idle boundary.
    for frame in assistant_turn("ses_b", "msg_b", "prt_b", "B SAID THIS") {
        fake.push_event(&frame);
    }
    fake.push_event(&permission_asked("ses_b", "per_b2", "curl evil.example"));
    fake.push_event(&json!({
        "type": "session.idle", "properties": { "sessionID": "ses_b" },
    }));
    // A sentinel on A bounds the window: everything B pushed has been read by
    // the time this arrives, so "never surfaced" is a real statement.
    fake.push_event(&marker("ses_a", "sentinel"));
    let live = until_text(&mut rx, "sentinel").await;

    for ev in &live {
        if let Some(text) = message_text(ev) {
            assert!(
                !text.contains("B SAID THIS"),
                "B's transcript leaked: {ev:?}"
            );
        }
        if let LaneEvent::Approval { approval } = ev {
            panic!("B's approval leaked onto A: {approval:?}");
        }
    }
    assert_clean(&fake);
}

#[tokio::test]
async fn two_directories_on_one_server_are_scoped_by_the_query() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "A", "/w1", None);
    fake.add_session("ses_c", "C", "/w2", None);
    fake.set_simple_transcript("ses_a", "hello A", "reply A");
    fake.set_simple_transcript("ses_c", "hello C", "reply C");
    fake.pin("ses_a");

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let seed = until_ready(&mut rx).await;
    assert_eq!(texts(&seed), vec!["hello A", "reply A"]);

    // Every instance-scoped route carries /w1, never /w2.
    let gets = fake.get_paths();
    for route in ["/event", "/session/status", "/permission", "/question"] {
        assert!(
            gets.iter()
                .any(|g| g == &format!("{route}?directory=%2Fw1")),
            "{route} must be scoped to the session's directory: {gets:?}"
        );
        assert!(
            !gets
                .iter()
                .any(|g| g == &format!("{route}?directory=%2Fw2")),
            "{route} must not be asked about the other workspace"
        );
    }

    // And the other directory's traffic does not arrive on this stream at all.
    for frame in assistant_turn("ses_c", "msg_c", "prt_c", "C SAID THIS") {
        fake.push_event(&frame);
    }
    fake.push_event(&marker("ses_a", "sentinel"));
    for ev in until_text(&mut rx, "sentinel").await {
        if let Some(text) = message_text(&ev) {
            assert!(!text.contains("C SAID THIS"), "the other instance leaked");
        }
    }
    assert_clean(&fake);
}

/// A child's approval blocks the same agent, so it surfaces on the root's
/// panel; a child's conversation is not the root's transcript; a SIBLING root
/// is neither.
#[tokio::test]
async fn a_childs_approval_surfaces_but_its_transcript_does_not() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "root", "/w", None);
    fake.add_session("ses_child", "child", "/w", Some("ses_a"));
    fake.add_session("ses_sib", "sibling", "/w", None);
    fake.set_simple_transcript("ses_a", "hello", "hi there");
    fake.add_permission("ses_child", "per_child", "bash", "pwd");
    fake.add_permission("ses_sib", "per_sib", "bash", "rm -rf /");
    fake.pin("ses_a");

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let seed = until_ready(&mut rx).await;

    assert_eq!(
        approval_ids(&seed),
        vec!["per_child"],
        "the child's, never the sibling root's"
    );
    let LaneEvent::Approval { approval } = seed
        .iter()
        .find(|e| matches!(e, LaneEvent::Approval { .. }))
        .expect("the child's approval")
    else {
        unreachable!()
    };
    assert_eq!(
        approval.session_id, "ses_child",
        "attributed to the descendant that raised it"
    );

    // A GRANDCHILD announced live joins the scope, and its approval surfaces.
    fake.push_event(&json!({
        "type": "session.created",
        "properties": { "info": { "id": "ses_grand", "parentID": "ses_child", "directory": "/w" } },
    }));
    fake.push_event(&permission_asked("ses_grand", "per_grand", "make"));
    // The child's own conversation must NOT reach the root's transcript.
    for frame in assistant_turn("ses_child", "msg_c", "prt_c", "CHILD SAID THIS") {
        fake.push_event(&frame);
    }
    // And a sibling root's approval must not either.
    fake.push_event(&permission_asked(
        "ses_sib",
        "per_sib2",
        "curl evil.example",
    ));
    fake.push_event(&marker("ses_a", "sentinel"));

    let live = until_text(&mut rx, "sentinel").await;
    assert_eq!(approval_ids(&live), vec!["per_grand"]);
    for ev in &live {
        if let Some(text) = message_text(ev) {
            assert!(
                !text.contains("CHILD SAID THIS"),
                "the transcript stays root-only: {ev:?}"
            );
        }
    }

    // Answering the child's approval addresses the CHILD's request id — the pin
    // guard's ledger is what proves that is what happened.
    client(&fake)
        .answer(
            "ses_a",
            "per_child",
            shed_core::lane::LaneAnswer::Permission {
                decision: shed_core::lane::LaneDecision::AllowOnce,
            },
        )
        .await
        .expect("answering the child's approval from the root's panel");
    assert_eq!(fake.post_paths(), vec!["/permission/per_child/reply"]);
    assert_clean(&fake);
}

#[tokio::test]
async fn an_approval_resolving_reaches_the_client_as_a_second_frame() {
    let fake = one_session().await;
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    until_ready(&mut rx).await;

    // An ask reaches a client TWICE: as the `approval_request` transcript row
    // and as the authoritative `Approval` frame. Only the second is what a
    // panel answers against.
    fake.push_event(&permission_asked("ses_a", "per_1", "ls -la"));
    fake.push_event(&marker("ses_a", "asked"));
    let asked = until_text(&mut rx, "asked").await;
    let pending = asked
        .iter()
        .find_map(|e| match e {
            LaneEvent::Approval { approval } if approval.id == "per_1" => Some(approval),
            _ => None,
        })
        .expect("the ask reaches the client as an Approval frame");
    assert!(pending.status.is_pending());
    assert!(
        asked
            .iter()
            .any(|e| matches!(e, LaneEvent::Message { message, .. }
            if message.msg_type == "approval_request")),
        "and as a transcript row beside it"
    );

    fake.push_event(&json!({
        "type": "permission.replied",
        "properties": { "sessionID": "ses_a", "requestID": "per_1", "reply": "once" },
    }));
    fake.push_event(&marker("ses_a", "sentinel"));

    let after = until_text(&mut rx, "sentinel").await;
    let resolved = after
        .iter()
        .find_map(|e| match e {
            LaneEvent::Approval { approval } if approval.id == "per_1" => Some(approval),
            _ => None,
        })
        .expect("the resolution reaches the client");
    assert!(
        !resolved.status.is_pending(),
        "last-write-wins: the SAME id, resolved"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn stop_aborts_the_task_and_closes_the_socket() {
    let fake = one_session().await;
    let subscription = subscribe(&fake, "ses_a").await;
    let (mut rx, stop) = subscription.into_parts();
    until_ready(&mut rx).await;
    assert_eq!(
        fake.stream_count(),
        1,
        "the pump holds one /event connection"
    );

    stop.stop();
    wait_for("the /event socket to close", || fake.stream_count() == 0).await;
    assert!(tokio::time::timeout(Duration::from_secs(2), rx.recv())
        .await
        .expect("the channel closes when the task is aborted")
        .is_none());
    assert_clean(&fake);
}

/// GLM's blocker, as a test: a request timeout on the STREAM client would cut
/// `GET /event` mid-session. This holds the stream open well past the 5 s the
/// REST client uses, on comment pings alone, and asserts it never reconnected.
///
/// It costs real seconds — deliberately. A paused clock cannot prove the
/// absence of a timeout inside reqwest's own stack.
#[tokio::test]
async fn the_stream_client_has_no_request_timeout() {
    let fake = one_session().await;
    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    until_ready(&mut rx).await;

    // 6 s of nothing but keep-alives — past the REST client's 5 s total bound.
    for _ in 0..12 {
        tokio::time::sleep(Duration::from_millis(500)).await;
        fake.push_ping();
    }
    assert_eq!(
        fake.stream_count(),
        1,
        "the connection is still the one the seed opened"
    );

    fake.push_event(&marker("ses_a", "still-here"));
    match next_event(&mut rx, "a row on the still-open stream").await {
        LaneEvent::Message { message, .. } => assert!(message
            .text
            .as_deref()
            .is_some_and(|t| t.contains("still-here"))),
        other => panic!("a Reset here means the stream was cut and reconnected: {other:?}"),
    }
    assert_clean(&fake);
}

/// The P1 wedge: a peer that completes the TCP handshake and then never sends a
/// status line satisfies `connect_timeout`, so an unbounded wait for the
/// response HEAD parks the pump after its `Reset` forever — no body, therefore
/// no stall timer, therefore no retry and no `Down`. The bound is on the head
/// alone; the SUCCESSFUL SSE body stays timeout-free (see
/// `the_stream_client_has_no_request_timeout`).
#[tokio::test]
async fn an_event_stream_that_never_sends_headers_gives_up_rather_than_hanging() {
    let fake = one_session().await;
    fake.stall_get("/event", None);

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    match next_event(&mut rx, "the opening Reset").await {
        LaneEvent::Reset { generation, .. } => assert_eq!(generation, 1),
        other => panic!("got {other:?}"),
    }

    // Bounded by the constant the bound is made of, with room for a loopback
    // round trip — never an unbounded `recv()`.
    let budget = STREAM_HEAD_TIMEOUT + Duration::from_secs(5);
    let ev = tokio::time::timeout(budget, rx.recv())
        .await
        .unwrap_or_else(|_| {
            panic!("the pump never gave up on a peer that sent no headers within {budget:?}")
        })
        .expect("the lane stream ended without saying anything");
    match ev {
        LaneEvent::Down { reason } => assert!(
            reason.contains("no response headers"),
            "the Down names the wedge: {reason}"
        ),
        other => panic!("a wedged first connect is a Down, got {other:?}"),
    }
    assert_clean(&fake);
}

/// The same wedge one layer later: a non-2xx head, and then the error body it
/// promised never arrives. Reading it unbounded hangs exactly as a headerless
/// peer does. The STATUS is what the caller branches on, so a body that could
/// not be read degrades to an empty one rather than to a hang.
#[tokio::test]
async fn a_non_2xx_event_whose_error_body_never_arrives_still_resolves() {
    let fake = one_session().await;
    fake.stall_get("/event", Some(500));

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    match next_event(&mut rx, "the opening Reset").await {
        LaneEvent::Reset { generation, .. } => assert_eq!(generation, 1),
        other => panic!("got {other:?}"),
    }

    let budget = STREAM_HEAD_TIMEOUT + Duration::from_secs(5);
    let ev = tokio::time::timeout(budget, rx.recv())
        .await
        .unwrap_or_else(|_| {
            panic!("the pump never gave up on an error body that never arrived within {budget:?}")
        })
        .expect("the lane stream ended without saying anything");
    match ev {
        LaneEvent::Down { reason } => assert!(
            reason.contains("500"),
            "the status survives the unreadable body: {reason}"
        ),
        other => panic!("got {other:?}"),
    }
    assert_clean(&fake);
}

/// The pinned seed order under the interleaving that breaks it, end to end: the
/// REST seed finds a pending approval AND a root frame is delivered while the
/// seed is still running, so step 4's replay is non-empty. Emitting that frame's
/// effects as it lands puts `Approval` ahead of the first `Session`, and the
/// closing sequence cannot repair it — emission is deduplicated.
///
/// The frame is injected on the FIRST seed-only read and the LAST seed read is
/// parked, so the replay window is one the test stands on rather than races.
#[tokio::test]
async fn a_buffered_frame_does_not_reorder_the_seeds_session_and_approvals() {
    let fake = one_session().await;
    fake.add_permission("ses_a", "per_1", "bash", "ls");
    fake.inject_on_get("/session/ses_a/message", &marker("ses_a", "buffered"));
    fake.hold_get("/question");

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    match next_event(&mut rx, "the opening Reset").await {
        LaneEvent::Reset { generation, .. } => assert_eq!(generation, 1),
        other => panic!("got {other:?}"),
    }
    wait_for("the seed to park on /question", || {
        fake.get_paths().iter().any(|g| g.starts_with("/question"))
    })
    .await;
    fake.release_get("/question");

    let seed = until_ready(&mut rx).await;
    assert!(
        texts(&seed).iter().any(|t| t.contains("buffered")),
        "the injected frame WAS replayed inside the seed: {seed:#?}"
    );
    let session_at = seed
        .iter()
        .position(|e| matches!(e, LaneEvent::Session { .. }))
        .unwrap_or_else(|| panic!("a Session frame: {seed:#?}"));
    let approval_at = seed
        .iter()
        .position(|e| matches!(e, LaneEvent::Approval { .. }))
        .unwrap_or_else(|| panic!("an Approval frame: {seed:#?}"));
    assert!(
        session_at < approval_at,
        "messages, then session, then approvals — the buffered frame must not jump the row: {seed:#?}"
    );
    assert_eq!(approval_ids(&seed), vec!["per_1"]);
    assert_clean(&fake);
}

/// A reconnect clears the scope and rebuilds it from REST. A one-level
/// `/session/{id}/children` read restores only the immediate children, so a
/// GRANDCHILD's pending approvals — and every later approval frame for it, since
/// the scope is also the live filter — are silently dropped. §3.2 pins the
/// tracked set as "the root's descendants", grown transitively by
/// `session.created`; a reseed has to restore the same transitive set.
#[tokio::test]
async fn a_reseed_restores_the_whole_descendant_tree_not_just_the_children() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "root", "/w", None);
    fake.add_session("ses_b", "child", "/w", Some("ses_a"));
    fake.add_session("ses_c", "grandchild", "/w", Some("ses_b"));
    fake.set_simple_transcript("ses_a", "hello", "hi there");
    fake.set_status("ses_a", "idle");
    fake.add_permission("ses_c", "per_c", "bash", "make");
    fake.pin("ses_a");

    let (mut rx, _stop) = subscribe(&fake, "ses_a").await.into_parts();
    let first = until_ready(&mut rx).await;
    assert_eq!(
        approval_ids(&first),
        vec!["per_c"],
        "a grandchild's approval is in scope from the first seed: {first:#?}"
    );

    fake.close_streams();
    let second = until_ready(&mut rx).await;
    assert_eq!(
        approval_ids(&second),
        vec!["per_c"],
        "and survives the reconnect that cleared the scope: {second:#?}"
    );

    // The scope is the LIVE filter too, so a later approval for the grandchild
    // must still reach the root's panel after the reseed.
    fake.push_event(&permission_asked("ses_c", "per_c2", "cargo test"));
    fake.push_event(&marker("ses_a", "sentinel"));
    let live = until_text(&mut rx, "sentinel").await;
    assert_eq!(
        approval_ids(&live),
        vec!["per_c2"],
        "the reseeded scope still admits the grandchild: {live:#?}"
    );
    assert_clean(&fake);
}
