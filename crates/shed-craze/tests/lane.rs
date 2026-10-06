//! `CrazeLane` against a SCRIPTED host behind a scripted hub — the watcher's
//! state machine cell by cell (plan 025 §3.3.4–3.3.5), deterministic: craze's
//! end of every connection is played line by line (`testing::ScriptedDial`),
//! and every frame goes through the contract's conformance kit.
//!
//! The same behaviours run against the REAL hub and `craze-fake-host` in
//! `tests/recipe_lane.rs`; these cells pin the edges a real host will not
//! produce on demand (a hole in the seqs, a wrong-sequence `synchronized`, an
//! `omitted` closing reset, the bounds on a paused clock).

mod common;

use std::sync::Arc;
use std::time::Duration;

use common::*;
use serde_json::{json, Value};
use shed_core::lane::conformance::LaneChecker;
use shed_core::lane::{AgentLane, LaneAnswer, LaneError, LaneEvent, LaneSettingChange, SendMode};
use shed_core::rc::RcActivity;
use shed_craze::testing::{
    ask_record, attach_result, host_session_row, session_info, snapshot_at, SCRIPT_WAIT,
};
use shed_craze::{is_outcome_unknown, DialError};

fn permission_event(id: &str) -> Value {
    json!({"type": "permission", "at": "2026-01-01T00:00:01Z",
           "permission": {"id": id, "tool": "Shell",
                          "options": [{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
                                      {"optionId": "deny", "name": "Deny", "kind": "reject_once"}]}})
}

/// The seed, in the contract's order: `Reset`, the snapshot's rows (the
/// omitted ledger's, then the entries, then an open ask's pending row),
/// `Session`, `Capabilities`, `Settings`, the `Approval`, the replayed events'
/// rows — and `Ready` only at the attachment's `synchronized`, at the last seq
/// held.
#[tokio::test]
async fn the_seed_is_bracketed_and_ready_waits_for_synchronized() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let main = json!({"omitted": [[100]], "entries": [
        {"id": "2.0", "kind": "user", "text": "hi", "at": "2026-01-01T00:00:00Z"},
        {"id": "3.0", "kind": "assistant", "text": "hello", "at": "2026-01-01T00:00:00Z"}]});
    let mut snap = snapshot_at(INC, 4, main);
    let perm = json!({"id": "perm-1", "tool": "Shell", "options": [
        {"optionId": "allow", "name": "Allow", "kind": "allow_once"}]});
    snap["asks"] = json!([{"id": "perm-1", "kind": "permission",
        "body": {"permission": perm.clone()}, "at": "2026-01-01T00:00:00Z"}]);
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let attach = hub
        .attached_only(attach_result(SUB, &info(false), (INC, 4), Some(snap), None))
        .await;
    // The registry holds the open ask too (A11: it is the approval set).
    hub.registry(
        json!([ask_record("permission", &perm, "2026-01-01T00:00:00Z")]),
        4,
    )
    .await;
    assert_eq!(
        attach["params"],
        json!({"sessionId": SID}),
        "no cursor, no when, no budget"
    );
    hub.event(SUB, 5, text("replayed")).await;
    let mut checker = LaneChecker::new();
    // Everything up to the replayed row — and no Ready, because no
    // synchronized has come.
    let before = drive(&mut rx, &mut checker, "the seed", |e| {
        matches!(e, LaneEvent::Session { .. })
    })
    .await;
    assert_eq!(resets(&before), [("connect".to_string(), 1)]);
    assert_eq!(
        texts(&before),
        ["earlier transcript omitted", "hi", "hello", "Shell"]
    );
    let rest = drive(&mut rx, &mut checker, "caps, settings, approval", |e| {
        matches!(e, LaneEvent::Approval { .. })
    })
    .await;
    assert!(
        matches!(&rest[0], LaneEvent::Capabilities { capabilities } if capabilities.kind == "craze" && capabilities.settings && !capabilities.stop)
    );
    assert!(
        matches!(&rest[1], LaneEvent::Settings { settings } if settings.model.as_deref() == Some("grok"))
    );
    assert!(
        tokio::time::timeout(Duration::from_millis(300), rx.recv())
            .await
            .is_err(),
        "no Ready before synchronized"
    );
    hub.synchronized(SUB, 5).await;
    let end = drive(&mut rx, &mut checker, "Ready", is_ready).await;
    assert!(matches!(
        end.last(),
        Some(LaneEvent::Ready { generation: 1 })
    ));
    // The replayed text is still the open streak: the clock flushes it.
    assert_eq!(checker.live_generation(), Some(1));
    let session = lane.session().await.unwrap();
    assert_eq!(session.id, HOST);
    assert_eq!(
        session.activity,
        RcActivity::NeedsApproval,
        "the open ask overrides"
    );
    assert_eq!(session.pending_approvals, 1);
}

/// A `synchronized` at the wrong seq, a second one, or one for another
/// subscription is a protocol fault: the connection is dropped and the next
/// one RESEEDS (`Reset{protocol}`) — never a `Ready` on a stream that did not
/// arrive whole.
#[tokio::test]
async fn a_wrong_synchronized_is_a_fault_that_reseeds() {
    for (case, seq, sub) in [("early", 1, SUB), ("late", 9, SUB), ("other sub", 2, "s-9")] {
        let (_dial, mut conns, lane) = scripted(fast());
        let (mut rx, _stop) = subscribed(&lane).await;
        let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
        hub.event(SUB, 2, text("a")).await;
        hub.synchronized(sub, seq).await;
        let (mut hub2, _) = seed_conn(&mut conns, SUB, 2, json!({})).await;
        hub2.synchronized(SUB, 2).await;
        let mut checker = LaneChecker::new();
        let frames = drive(&mut rx, &mut checker, case, is_ready).await;
        let r = resets(&frames);
        assert_eq!(r.len(), 2, "{case}: {frames:#?}");
        assert_eq!(r[1], ("protocol".to_string(), 2), "{case}");
        assert!(
            matches!(frames.last(), Some(LaneEvent::Ready { generation: 2 })),
            "{case}: only the second seed is ready"
        );
        drop(hub);
    }
}

/// A hole or a duplicate in the event seqs is a protocol fault — reseed.
#[tokio::test]
async fn a_hole_or_a_duplicate_seq_reseeds() {
    for (case, seq) in [("hole", 3), ("duplicate", 1)] {
        let (_dial, mut conns, lane) = scripted(fast());
        let (mut rx, _stop) = subscribed(&lane).await;
        let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
        hub.synchronized(SUB, 1).await;
        hub.event(SUB, seq, text("x")).await;
        let (mut hub2, _) = seed_conn(&mut conns, SUB, 3, json!({})).await;
        hub2.synchronized(SUB, 3).await;
        let mut checker = LaneChecker::new();
        let frames = drive(&mut rx, &mut checker, case, |e| {
            matches!(e, LaneEvent::Ready { generation: 2 })
        })
        .await;
        assert_eq!(resets(&frames)[1].0, "protocol", "{case}");
        drop(hub);
    }
}

/// **The silent resume.** A connection lost after `Ready`: `Stale`, a redial,
/// an attach WITH the cursor `{incarnation, last seq folded}`; the reply with
/// no snapshot continues the stream; the replayed events fold onto the live
/// rows; the row is re-read; a lone `Ready` of the SAME generation — no
/// `Reset`.
#[tokio::test]
async fn a_loss_after_ready_resumes_silently_from_the_cursor() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.event(
        SUB,
        2,
        json!({"type": "turn", "turn": {"id": "t1", "phase": "started", "text": "go"}}),
    )
    .await;
    hub.synchronized(SUB, 2).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.close().await;
    let stale = drive(&mut rx, &mut checker, "Stale", |e| {
        matches!(e, LaneEvent::Stale { .. })
    })
    .await;
    assert!(matches!(stale.last(), Some(LaneEvent::Stale { reason }) if reason == "reconnecting"));

    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(host_session_row(
        &info(false),
        json!({"activity": "working", "attached": 2}),
    ))
    .await;
    let attach = hub
        .attached(attach_result("s-2", &info(false), (INC, 2), None, None))
        .await;
    assert_eq!(
        attach["params"],
        json!({"sessionId": SID, "cursor": {"incarnation": INC, "seq": 2}}),
        "the cursor is the last event folded"
    );
    hub.event("s-2", 3, text("during the outage")).await;
    hub.event("s-2", 4, json!({"type": "done", "stopReason": "end_turn"}))
        .await;
    hub.synchronized("s-2", 4).await;
    let resumed = drive(&mut rx, &mut checker, "the lone Ready", is_ready).await;
    assert!(resets(&resumed).is_empty(), "no Reset: {resumed:#?}");
    assert!(
        matches!(resumed.last(), Some(LaneEvent::Ready { generation: 1 })),
        "the same generation"
    );
    assert_eq!(texts(&resumed), ["during the outage"]);
    assert_eq!(checker.resets(), 1);
    assert_eq!(checker.stales(), 1);
    let session = lane.session().await.unwrap();
    assert_eq!(session.attached, Some(2), "the row re-read");
    assert_eq!(
        session.activity,
        RcActivity::Idle,
        "the fold's done is newer than the row"
    );
}

/// **A loss before the seed's `Ready` reseeds, never resumes** — a lone
/// `Ready` would otherwise publish a half seed.
#[tokio::test]
async fn a_loss_before_ready_reseeds() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.event(SUB, 2, text("half")).await;
    hub.close().await;
    let (mut hub, attach) = seed_conn(
        &mut conns,
        SUB,
        2,
        json!({"entries": [
        {"id": "2.0", "kind": "assistant", "text": "half"}]}),
    )
    .await;
    assert!(
        attach["params"].get("cursor").is_none(),
        "no cursor: {attach}"
    );
    hub.synchronized(SUB, 2).await;
    let mut checker = LaneChecker::new();
    let frames = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
    assert_eq!(
        resets(&frames),
        [("connect".to_string(), 1), ("reconnect".to_string(), 2)]
    );
    assert!(matches!(
        frames.last(),
        Some(LaneEvent::Ready { generation: 2 })
    ));
}

/// **A refused cursor reseeds**: the attach answered with a snapshot (and its
/// reason) is `Reset{cursor_lost:<reason>} … Ready`.
#[tokio::test]
async fn a_refused_cursor_reseeds() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.close().await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let new_info = session_info(
        HOST,
        SID,
        "INCARNATION-2",
        shed_craze::testing::session_caps(false),
    );
    let attach = hub
        .attached(attach_result(
            SUB,
            &new_info,
            ("INCARNATION-2", 1),
            Some(snapshot_at("INCARNATION-2", 1, json!({}))),
            Some("foreign_incarnation"),
        ))
        .await;
    assert!(attach["params"].get("cursor").is_some());
    hub.synchronized(SUB, 1).await;
    let frames = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
    assert_eq!(
        resets(&frames),
        [("cursor_lost:foreign_incarnation".to_string(), 2)]
    );
}

/// `session_replaced`: the connection is dropped, the next says `hello`
/// afresh, RE-LEARNS the craze session id from the host's `sessions.list`,
/// and attaches with no cursor.
#[tokio::test]
async fn session_replaced_relearns_the_session_id_and_reseeds() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.reset(SUB, "session_replaced").await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    let replaced = session_info(
        HOST,
        "session-fake-2",
        "INCARNATION-2",
        shed_craze::testing::session_caps(false),
    );
    hub.listed(host_session_row(&replaced, json!({}))).await;
    let attach = hub
        .attached(attach_result(
            SUB,
            &replaced,
            ("INCARNATION-2", 1),
            Some(snapshot_at("INCARNATION-2", 1, json!({}))),
            None,
        ))
        .await;
    assert_eq!(
        attach["params"],
        json!({"sessionId": "session-fake-2"}),
        "re-learned, no cursor"
    );
    hub.synchronized(SUB, 1).await;
    let frames = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
    assert_eq!(resets(&frames)[0].0, "server_reset:session_replaced");
    assert_eq!(lane.craze_session_id().as_deref(), Some("session-fake-2"));
}

/// The terminal ends, each `Down` last and each with its pinned reason.
#[tokio::test]
async fn the_terminal_ends() {
    // reset{session_closed}
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    hub.reset(SUB, "session_closed").await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "closed", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "session_closed".into()
        })
    );

    // unknown_session on connect
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.hello("0a1b2c3d4e5f", shed_craze::testing::full_hub_capabilities())
        .await;
    let connect = hub.expect("session.connect").await;
    hub.refuse(&connect, "unknown_session", "unknown_session", json!({}))
        .await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "unknown", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "unknown_session".into()
        })
    );

    // start_failed on attach, with its cause
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let attach = hub.expect("session.attach").await;
    hub.refuse(
        &attach,
        "not_accepting",
        "start_failed",
        json!({"cause": "KEYCHAIN LOCKED"}),
    )
    .await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "start_failed", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "start_failed: KEYCHAIN LOCKED".into()
        })
    );

    // an omitted that is the session's closing reset: the connection ends
    // under the re-attach it asked for — and the redial that confirms it
    // finds no such session.
    let (dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    hub.reset(SUB, "omitted").await;
    let again = hub.expect("session.attach").await;
    assert!(
        again["params"].get("cursor").is_none(),
        "omitted: no cursor"
    );
    hub.close().await;
    let mut confirm = next_conn(&mut conns).await;
    confirm
        .hello("0a1b2c3d4e5f", shed_craze::testing::full_hub_capabilities())
        .await;
    let connect = confirm.expect("session.connect").await;
    confirm
        .refuse(&connect, "unknown_session", "unknown_session", json!({}))
        .await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "closing omitted", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "session_closed".into()
        }),
        "the confirmed close is the session's close"
    );
    assert_eq!(dial.dials(), 2, "one confirming redial");
}

/// **A dropped transport after `reset{omitted}` is not the session's close.**
/// The `omitted`'s re-attach loses its connection before any answer — as the
/// closing `omitted` would end it, and as a cellular drop does. The lane does
/// not guess: it redials, and a session that is still there answers the
/// no-cursor attach with a snapshot — a reseed, never a `Down`.
#[tokio::test]
async fn a_drop_after_an_omitted_reset_redials_to_confirm_and_reseeds() {
    let (dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.reset(SUB, "omitted").await;
    hub.expect("session.attach").await;
    // The transport drops under the re-attach, unanswered.
    hub.close().await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let req = hub
        .attached(attach_result(
            "s-2",
            &info(false),
            (INC, 3),
            Some(snapshot_at(
                INC,
                3,
                json!({"entries": [
                {"id": "1.0", "kind": "note", "text": "still here"}]}),
            )),
            None,
        ))
        .await;
    assert!(
        req["params"].get("cursor").is_none(),
        "the confirming attach takes no cursor"
    );
    hub.synchronized("s-2", 3).await;
    let f = drive(&mut rx, &mut checker, "the reseed", |e| {
        matches!(e, LaneEvent::Ready { generation: 2 })
    })
    .await;
    assert!(!f.iter().any(is_down), "a drop is not a close: {f:#?}");
    assert_eq!(resets(&f), [("server_reset:omitted".to_string(), 2)]);
    assert_eq!(texts(&f), ["still here"]);
    assert_eq!(dial.dials(), 2);
}

/// A plain `omitted` (not the closing one) re-attaches with no cursor on the
/// SAME connection and reseeds.
#[tokio::test]
async fn an_omitted_reset_reattaches_without_a_cursor_on_the_same_connection() {
    let (dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    hub.reset(SUB, "omitted").await;
    hub.attached(attach_result(
        "s-2",
        &info(false),
        (INC, 3),
        Some(snapshot_at(INC, 3, json!({}))),
        None,
    ))
    .await;
    hub.synchronized("s-2", 3).await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "the reseed", |e| {
        matches!(e, LaneEvent::Ready { generation: 2 })
    })
    .await;
    assert_eq!(resets(&f)[1].0, "server_reset:omitted");
    assert_eq!(dial.dials(), 1, "the same connection");
}

/// `slow_consumer` after readiness re-attaches WITH the cursor on the same
/// connection; honoured, nothing is announced at all.
#[tokio::test]
async fn slow_consumer_after_ready_reattaches_with_the_cursor() {
    let (dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.event(SUB, 2, json!({"type": "done", "stopReason": "end_turn"}))
        .await;
    hub.reset(SUB, "slow_consumer").await;
    let again = hub
        .attached(attach_result("s-2", &info(false), (INC, 2), None, None))
        .await;
    assert_eq!(
        again["params"]["cursor"],
        json!({"incarnation": INC, "seq": 2})
    );
    hub.event("s-2", 3, text("after")).await;
    hub.synchronized("s-2", 3).await;
    hub.event("s-2", 4, json!({"type": "done", "stopReason": "end_turn"}))
        .await;
    let f = drive(&mut rx, &mut checker, "the row", |e| {
        matches!(e, LaneEvent::Message { .. })
    })
    .await;
    assert!(
        resets(&f).is_empty() && !f.iter().any(is_ready),
        "nothing announced: {f:#?}"
    );
    assert_eq!(texts(&f), ["after"]);
    assert_eq!(dial.dials(), 1);
}

/// `presence` is the row's `attached`; a compaction's start is its working
/// label; its end is a note.
#[tokio::test]
async fn presence_and_compaction_ride_the_session_row() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.notify("presence", json!({"subscription": SUB, "attached": 3}))
        .await;
    let f = drive(
        &mut rx,
        &mut checker,
        "presence",
        |e| matches!(e, LaneEvent::Session { session } if session.attached == Some(3)),
    )
    .await;
    assert!(!f.is_empty());
    hub.event(
        SUB,
        2,
        json!({"type": "compaction", "compaction": {"phase": "started", "reason": "manual"}}),
    )
    .await;
    drive(&mut rx, &mut checker, "compacting", |e| matches!(e, LaneEvent::Session { session } if session.doing.as_deref() == Some("compacting context…"))).await;
    hub.event(SUB, 3, json!({"type": "compaction", "compaction": {"phase": "ended", "reason": "manual", "tokensBefore": 890000, "tokensAfter": 21000}})).await;
    let f = drive(&mut rx, &mut checker, "compacted", |e| {
        matches!(e, LaneEvent::Message { .. })
    })
    .await;
    assert_eq!(
        texts(&f),
        ["context compacted on request · 890k → 21k tokens"]
    );
}

/// **The attach info document's `permissionMode` rides the session row**
/// (plan 025 §3.6.5; live leg 1's permission-line finding), over an opened
/// row that says none — a just-created session's: the desktop's transcript
/// header reads the stream's row, and a sheet-created session runs `bypass`.
/// The host's `sessions.list` row is given none either, so this pins the info
/// document's own mapping, not the row's.
#[tokio::test]
async fn the_info_documents_permission_mode_rides_the_session_row() {
    let (_dial, mut conns, lane) = scripted(fast());
    assert_eq!(
        lane.session()
            .await
            .expect("the opened row")
            .permission_mode,
        None,
        "opened with a row that says none"
    );
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut bypass = info(false);
    bypass["permissionMode"] = json!("bypass");
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached(attach_result(
        SUB,
        &bypass,
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let session = seed
        .iter()
        .rev()
        .find_map(|e| match e {
            LaneEvent::Session { session } => Some(session.clone()),
            _ => None,
        })
        .expect("the seed carries the session row");
    assert_eq!(
        session.permission_mode.as_deref(),
        Some("bypass"),
        "the info document's permissionMode, on the seed's row"
    );
    assert_eq!(
        lane.session()
            .await
            .expect("the row")
            .permission_mode
            .as_deref(),
        Some("bypass"),
        "and the lane's own row follows the stream's"
    );
}

/// The flush clock: a streak that stops growing for 2 s is flushed as a
/// partial row; the continuing stream starts a new segment.
#[tokio::test(start_paused = true)]
async fn a_paused_streak_is_flushed_by_the_clock() {
    let (_dial, mut conns, lane) = scripted(shed_craze::LaneTimings::default());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.event(SUB, 2, text("Hello, ")).await;
    hub.event(SUB, 3, text("world")).await;
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(10),
        "the flush",
        |e| matches!(e, LaneEvent::Message { .. }),
    )
    .await;
    assert_eq!(texts(&f), ["Hello, world"]);
    hub.event(SUB, 4, text("again")).await;
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(10),
        "the next segment",
        |e| matches!(e, LaneEvent::Message { .. }),
    )
    .await;
    assert_eq!(texts(&f), ["again"]);
}

/// **The re-attach bound**: every attach of an episode after the first counts,
/// and the ninth is refused `Down{"re-attach bound"}` — a host that keeps
/// resetting before `synchronized` cannot loop the lane forever.
#[tokio::test]
async fn the_ninth_reattach_of_an_episode_is_the_bound() {
    let (dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    for _ in 0..8 {
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
    hub.reset(SUB, "replay_failed").await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "the bound", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "re-attach bound".into()
        })
    );
    assert_eq!(
        resets(&f).len(),
        9,
        "the first attach and eight re-attaches"
    );
    assert_eq!(dial.dials(), 1);
}

/// The bound resets at `synchronized`: eight re-attaches, a `synchronized`,
/// then eight more — still alive.
#[tokio::test]
async fn the_reattach_count_resets_at_synchronized() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    for _ in 0..2 {
        for _ in 0..8 {
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
        hub.synchronized(SUB, 1).await;
    }
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "alive", |e| {
        matches!(e, LaneEvent::Ready { generation: 17 })
    })
    .await;
    assert!(!f.iter().any(is_down));
}

/// **The ten-minute bound**, on a paused clock: dials that keep failing end
/// the lane `Down{"unreachable"}` ten minutes after the outage began — not
/// before — with one `Stale` said first.
#[tokio::test(start_paused = true)]
async fn dials_give_up_ten_minutes_after_the_outage_began() {
    let (dial, mut conns, lane) = scripted(shed_craze::LaneTimings::default());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    for _ in 0..1000 {
        dial.fail_next(DialError::Unreachable("no route".into()));
    }
    let lost_at = tokio::time::Instant::now();
    hub.close().await;
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(3600),
        "Down",
        is_down,
    )
    .await;
    let elapsed = lost_at.elapsed();
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "unreachable".into()
        })
    );
    assert_eq!(checker.stales(), 1, "one Stale per outage, not per dial");
    assert!(elapsed >= Duration::from_secs(600), "{elapsed:?}");
    assert!(
        elapsed < Duration::from_secs(600) + Duration::from_secs(31),
        "within one backoff of the bound: {elapsed:?}"
    );
    assert!(dial.dials() > 10);
}

/// Five consecutive refused connects end the lane rather than loop.
#[tokio::test]
async fn five_refused_connects_end_the_lane() {
    let (dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    for _ in 0..5 {
        let mut hub = next_conn(&mut conns).await;
        hub.hello("0a1b2c3d4e5f", shed_craze::testing::full_hub_capabilities())
            .await;
        let connect = hub.expect("session.connect").await;
        hub.refuse(&connect, "unavailable", "host_unreachable", json!({}))
            .await;
    }
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "refused", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "connect refused: unavailable/host_unreachable".into()
        })
    );
    assert_eq!(dial.dials(), 5);
}

/// `session()` NEVER dials: it answers the row the lane was opened with,
/// before any subscription; an id no source listed is `UnknownSession` at
/// once.
#[tokio::test]
async fn session_never_dials() {
    let (dial, _conns, lane) = scripted(fast());
    let s = lane.session().await.unwrap();
    assert_eq!((s.id.as_str(), s.title.as_str()), (HOST, "work"));
    assert_eq!(dial.dials(), 0, "session() dialled");
    let unlisted = shed_craze::CrazeLane::new(
        dial.clone(),
        shed_craze::wire::ClientInfo::shed("t"),
        "0f0f0f0f0f0f",
        None,
        fast(),
    );
    assert_eq!(unlisted.session().await, Err(LaneError::UnknownSession));
    assert_eq!(unlisted.session_id(), "0f0f0f0f0f0f");
    assert_eq!(dial.dials(), 0);
}

/// **The identity cell**: opened by hostId, every call carries the craze
/// sessionId — the connect names the host, the attach and every verb the
/// session; commandIds count from 1 and never repeat.
#[tokio::test]
async fn opened_by_host_id_every_call_carries_the_craze_session_id() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    let connect = hub.splice(HOST).await;
    assert_eq!(
        connect["params"]["sessionId"], HOST,
        "session.connect is given the hostId"
    );
    hub.listed(row()).await;
    let mut snap = snapshot_at(INC, 2, json!({}));
    let permission = permission_event("perm-1")["permission"].clone();
    snap["asks"] =
        json!([{"id": "perm-1", "kind": "permission", "body": {"permission": permission}}]);
    let attach = hub
        .attached_only(attach_result(SUB, &info(true), (INC, 2), Some(snap), None))
        .await;
    assert_eq!(attach["params"]["sessionId"], SID);
    hub.registry(
        json!([ask_record(
            "permission",
            &permission,
            "2026-01-01T00:00:01Z"
        )]),
        2,
    )
    .await;
    hub.synchronized(SUB, 2).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;

    let l = Arc::clone(&lane);
    let send = tokio::spawn(async move { l.send("hi", SendMode::Queue).await });
    let req = hub.expect("session.prompt").await;
    assert_eq!(
        req["params"],
        json!({"sessionId": SID, "commandId": "1", "text": "hi", "mode": "queue"})
    );
    hub.reply(&req, json!({"turn": "turn-1", "text": "hi"}))
        .await;
    send.await.unwrap().unwrap();

    let l = Arc::clone(&lane);
    let cancel = tokio::spawn(async move { l.cancel().await });
    let req = hub.expect("session.cancel").await;
    assert_eq!(
        req["params"],
        json!({"sessionId": SID, "commandId": "2"}),
        "no turnId"
    );
    hub.refuse(&req, "not_accepting", "not_accepting", json!({}))
        .await;
    assert_eq!(cancel.await.unwrap(), Err(LaneError::NotAccepting));

    let l = Arc::clone(&lane);
    let answer = tokio::spawn(async move {
        l.answer(
            "perm-1",
            LaneAnswer::Choice {
                option_id: "deny".into(),
            },
        )
        .await
    });
    let req = hub.expect("asks.answer").await;
    assert_eq!(
        req["params"],
        json!({"sessionId": SID, "commandId": "3", "askId": "perm-1", "answer": {"optionId": "deny"}})
    );
    hub.reply(&req, json!({})).await;
    answer.await.unwrap().unwrap();

    let l = Arc::clone(&lane);
    let stop = tokio::spawn(async move { l.stop().await });
    let req = hub.expect("session.stop").await;
    assert_eq!(req["params"], json!({"sessionId": SID, "commandId": "4"}));
    hub.reply(&req, json!({})).await;
    stop.await.unwrap().unwrap();
}

/// **A verb in flight when the connection drops** fails at once, outcome
/// unknown, and is NEVER resent on the next connection.
#[tokio::test]
async fn a_verb_in_flight_at_a_drop_is_outcome_unknown_and_never_resent() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let l = Arc::clone(&lane);
    let send = tokio::spawn(async move { l.send("hi", SendMode::Queue).await });
    hub.expect("session.prompt").await;
    hub.close().await;
    let got = send.await.unwrap().unwrap_err();
    assert!(is_outcome_unknown(&got), "{got:?}");
    // The resume's connection carries the attach and nothing else.
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached(attach_result("s-2", &info(false), (INC, 1), None, None))
        .await;
    hub.synchronized("s-2", 1).await;
    drive(&mut rx, &mut checker, "resumed", is_ready).await;
    assert!(
        hub.recv_within(Duration::from_millis(300)).await.is_none(),
        "the prompt was resent"
    );
}

/// **A verb past its deadline** is outcome unknown AND tears the connection
/// down — the half-open transport — so the watcher goes `Stale` and resumes.
#[tokio::test]
async fn a_verb_past_its_deadline_tears_the_connection_down() {
    let mut t = fast();
    t.request = Duration::from_millis(300);
    let (_dial, mut conns, lane) = scripted(t);
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let l = Arc::clone(&lane);
    let cancel = tokio::spawn(async move { l.cancel().await });
    hub.expect("session.cancel").await;
    // No answer.
    let got = cancel.await.unwrap().unwrap_err();
    assert!(is_outcome_unknown(&got), "{got:?}");
    drive(&mut rx, &mut checker, "Stale", |e| {
        matches!(e, LaneEvent::Stale { .. })
    })
    .await;
    let mut hub2 = next_conn(&mut conns).await;
    hub2.splice(HOST).await;
    hub2.listed(row()).await;
    let attach = hub2
        .attached(attach_result("s-2", &info(false), (INC, 1), None, None))
        .await;
    assert!(attach["params"].get("cursor").is_some(), "a resume");
    drop(hub);
}

/// **A verb issued while disconnected** waits for the reconnect, then fails
/// `Unavailable` — never sent, so a retry is safe.
#[tokio::test]
async fn a_verb_while_disconnected_waits_then_fails_unavailable() {
    let (_dial, _conns, lane) = scripted(fast());
    let start = tokio::time::Instant::now();
    let got = lane.send("hi", SendMode::Queue).await;
    assert!(
        matches!(&got, Err(LaneError::Unavailable(m)) if m.contains("not sent")),
        "{got:?}"
    );
    assert!(
        start.elapsed() >= Duration::from_millis(450),
        "it waited: {:?}",
        start.elapsed()
    );
}

/// **`session.stop`**: `stop()` resolves on the RECEIPT; the closing records
/// that follow fold; the lane ends only at `reset{session_closed}` (Amendment
/// A7) — receipt, then events, then reset.
#[tokio::test]
async fn stop_resolves_on_the_receipt_and_the_lane_ends_on_the_reset() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.event(
        SUB,
        2,
        json!({"type": "turn", "turn": {"id": "t1", "phase": "started", "text": "go"}}),
    )
    .await;
    hub.synchronized(SUB, 2).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let l = Arc::clone(&lane);
    let stop = tokio::spawn(async move { l.stop().await });
    let req = hub.expect("session.stop").await;
    hub.reply(&req, json!({})).await;
    stop.await.unwrap().unwrap();
    let quiet = shed_core::lane::conformance::drive_lane(
        &mut rx,
        &mut checker,
        Duration::from_millis(200),
        is_down,
    )
    .await
    .unwrap();
    assert_eq!(
        quiet.end,
        shed_core::lane::conformance::DriveEnd::Deadline,
        "the receipt is not the end: {:#?}",
        quiet.frames
    );
    hub.event(SUB, 3, json!({"type": "done", "stopReason": "cancelled"}))
        .await;
    hub.event(
        SUB,
        4,
        json!({"type": "turn", "turn": {"id": "t1", "phase": "ended", "stopReason": "closing"}}),
    )
    .await;
    hub.reset(SUB, "session_closed").await;
    let f = drive(&mut rx, &mut checker, "Down", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "session_closed".into()
        })
    );
    assert!(
        texts(&f).contains(&"cancelled".to_string()),
        "the closing records folded first: {f:#?}"
    );
}

/// Interject on a session whose capabilities say it cannot is `NotAccepting`
/// at once (SendMode's contract) — nothing sent.
#[tokio::test]
async fn interject_on_a_session_that_cannot_is_not_accepting() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert_eq!(
        lane.send("x", SendMode::Interject).await,
        Err(LaneError::NotAccepting)
    );
    assert!(hub.recv_within(Duration::from_millis(200)).await.is_none());
}

/// A foreign turn's start and end (A7): the end draws no row and idles the
/// session.
#[tokio::test]
async fn a_foreign_turn_end_idles_the_session() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.event(SUB, 2, json!({"type": "foreign_turn", "foreignTurn": {"id": "wake-1", "text": "sub-agent result", "reason": "subagent_wake", "running": true}, "at": "2026-01-01T00:00:00Z"})).await;
    let f = drive(
        &mut rx,
        &mut checker,
        "working",
        |e| matches!(e, LaneEvent::Session { session } if session.activity == RcActivity::Working),
    )
    .await;
    assert_eq!(texts(&f), ["sub-agent finished — the agent continues"]);
    hub.event(SUB, 3, json!({"type": "foreign_turn", "foreignTurn": {"id": "wake-1", "reason": "subagent_wake"}, "at": "2026-01-01T00:00:00Z"})).await;
    let f = drive(
        &mut rx,
        &mut checker,
        "idle",
        |e| matches!(e, LaneEvent::Session { session } if session.activity == RcActivity::Idle),
    )
    .await;
    assert!(texts(&f).is_empty(), "the end bracket draws no row: {f:#?}");
    let _ = SCRIPT_WAIT;
}

/// The reads without a subscription: `history`, `approvals` and `settings`
/// each read the session on a short connection of their own — the splice,
/// the host's row, `session.snapshot` at its default budget — and fold it:
/// rows numbered locally with `truncated` from the window, the open asks,
/// the catalogs and the snapshot's settings in craze's order.
#[tokio::test]
async fn the_reads_without_a_subscription_dial_a_connection_of_their_own() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = std::sync::Arc::new(lane);
    let mut snap = snapshot_at(
        INC,
        3,
        json!({"windowed": true, "entries": [
        {"id": "1.0", "kind": "user", "text": "hi"},
        {"id": "2.0", "kind": "assistant", "text": "hello", "streaming": true}], "streamOpen": true}),
    );
    snap["asks"] = json!([{"id": "perm-1", "kind": "permission",
        "body": {"permission": {"id": "perm-1", "tool": "Shell", "options": []}}}]);
    let reply = json!({ "snapshot": snap.clone() });
    for read in ["history", "approvals", "settings"] {
        let l = std::sync::Arc::clone(&lane);
        let task = tokio::spawn(async move {
            match read {
                "history" => serde_json::to_value(l.history(None, 50).await.unwrap()).unwrap(),
                "approvals" => serde_json::to_value(l.approvals().await.unwrap()).unwrap(),
                _ => serde_json::to_value(l.settings().await.unwrap()).unwrap(),
            }
        });
        let mut hub = next_conn(&mut conns).await;
        hub.splice(HOST).await;
        hub.listed(row()).await;
        if read == "approvals" {
            // The registry (A11), not the snapshot's asks: a sub-agent's
            // ask, which no snapshot carries, is one of them.
            let sub_perm = json!({"id": "perm-sub", "tool": "Write", "options": []});
            hub.registry_reads(json!([
                ask_record(
                    "permission",
                    &snap["asks"][0]["body"]["permission"],
                    "2026-01-01T00:00:00Z"
                ),
                ask_record("permission", &sub_perm, "2026-01-01T00:00:00Z")
            ]))
            .await;
        } else {
            let req = hub.expect("session.snapshot").await;
            assert_eq!(
                req["params"],
                json!({"sessionId": SID}),
                "{read}: the default budget"
            );
            hub.reply(&req, reply.clone()).await;
        }
        let got = task.await.unwrap();
        match read {
            "history" => {
                let texts: Vec<&str> = got["messages"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|m| m["text"].as_str().unwrap())
                    .collect();
                assert_eq!(
                    texts,
                    ["hi", "Shell", "hello"],
                    "the open run flushed for a page"
                );
                assert_eq!(got["truncated"], true, "the window was cut");
                assert_eq!(got["messages"][0]["seq"], 1, "seqs local to the call");
            }
            "approvals" => {
                let ids: Vec<&str> = got
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|a| a["id"].as_str().unwrap())
                    .collect();
                assert_eq!(ids, ["perm-1", "perm-sub"]);
            }
            _ => {
                assert_eq!(got["model"], "grok");
                assert_eq!(got["models"][0]["id"], "grok", "the current model first");
                assert_eq!(got["options"][0]["id"], "effort");
            }
        }
    }
}

/// `Capabilities` and `Settings` ride the stream: an attach that raced the
/// start (`ready: false`, catalogs still empty, no options) seeds them; the
/// `ready` notification's final info document re-emits both; a `meta` delta
/// re-emits `Settings`. A `ready` that says the start failed ends the lane.
#[tokio::test]
async fn capabilities_and_settings_follow_ready_and_meta() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut early = info(false);
    early["catalogs"] = json!({"models": [], "modes": []});
    let mut attach = attach_result(
        SUB,
        &early,
        (INC, 1),
        Some(json!({"version": 1, "incarnation": INC, "seq": 1, "main": {}})),
        None,
    );
    attach["ready"] = json!(false);
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached(attach).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert!(seed
        .iter()
        .any(|e| matches!(e, LaneEvent::Capabilities { capabilities } if !capabilities.settings)));
    assert!(
        !seed.iter().any(|e| matches!(e, LaneEvent::Settings { .. })),
        "nothing to show yet"
    );
    hub.notify(
        "ready",
        json!({"subscription": SUB, "session": info(false), "startFailed": false}),
    )
    .await;
    let f = drive(&mut rx, &mut checker, "the final document", |e| {
        matches!(e, LaneEvent::Settings { .. })
    })
    .await;
    assert!(f
        .iter()
        .any(|e| matches!(e, LaneEvent::Capabilities { capabilities } if capabilities.settings)));
    hub.event(SUB, 2, json!({"type": "meta", "state": {"model": "fast"}}))
        .await;
    let f = drive(&mut rx, &mut checker, "the meta delta", |e| {
        matches!(e, LaneEvent::Settings { .. })
    })
    .await;
    assert!(
        matches!(f.last(), Some(LaneEvent::Settings { settings }) if settings.model.as_deref() == Some("fast"))
    );
    hub.notify("ready", json!({"subscription": SUB, "session": info(false), "startFailed": true, "err": "no such binary"})).await;
    let f = drive(&mut rx, &mut checker, "Down", is_down).await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "start_failed: no such binary".into()
        })
    );
}

// ---- the review's edges (C7+C8 review: the dial deadline, synchronized, the
// spliced host's identity, the seeded reads' owner) ----

/// A lane on `dial` with `timings`, opened with the roster's row.
fn lane_on(
    dial: Arc<dyn shed_craze::CrazeDial>,
    timings: shed_craze::LaneTimings,
) -> shed_craze::CrazeLane {
    shed_craze::CrazeLane::new(
        dial,
        shed_craze::wire::ClientInfo::shed("shed-lane-test"),
        HOST,
        Some((roster_row(), SID.to_string())),
        timings,
    )
}

/// **A dial that never resolves is a failed dial**, on a paused clock: each
/// held dial gives up at the dial deadline (30 s), counts toward the outage,
/// and the lane ends `Down{"unreachable"}` at the ten-minute bound — not a
/// wait without end on the first held dial.
#[tokio::test(start_paused = true)]
async fn a_held_dial_counts_toward_the_ten_minute_bound() {
    let (scripted, mut conns) = shed_craze::testing::ScriptedDial::new();
    let hook = shed_craze::testing::HookDial::new(scripted.clone());
    let lane = lane_on(hook.clone(), shed_craze::LaneTimings::default());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hook.hold_dials();
    let lost_at = tokio::time::Instant::now();
    hub.close().await;
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(3600),
        "Down",
        is_down,
    )
    .await;
    let elapsed = lost_at.elapsed();
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "unreachable".into()
        })
    );
    assert_eq!(checker.stales(), 1, "one Stale per outage");
    assert!(elapsed >= Duration::from_secs(600), "{elapsed:?}");
    assert!(
        elapsed < Duration::from_secs(600) + Duration::from_secs(31),
        "within one backoff of the bound: {elapsed:?}"
    );
    let held = hook.dials() - 1;
    assert!(
        (2..=21).contains(&held),
        "each held dial gave up at its deadline and the lane redialled: {held}"
    );
    assert_eq!(scripted.dials(), 1, "no held dial ever got through");
}

/// **An attach that never synchronizes is a dead connection**, on a paused
/// clock: the seed's attach is answered and the host goes quiet; 60 s later
/// the lane lets the connection go and — the seed never reached its `Ready` —
/// redials and RESEEDS. The wait is progress-based: on the second connection a
/// replay that keeps delivering (an event every 50 s, longer in all than the
/// wait) is never cut short.
#[tokio::test(start_paused = true)]
async fn an_attach_that_never_synchronizes_is_dropped_and_reseeded() {
    let (dial, mut conns, lane) = scripted(shed_craze::LaneTimings::default());
    let (mut rx, _stop) = subscribed(&lane).await;
    let started = tokio::time::Instant::now();
    let (mut quiet, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    let mut hub = tokio::time::timeout(Duration::from_secs(120), conns.recv())
        .await
        .expect("the lane gave the silent attachment up and redialled")
        .expect("the dial is alive");
    assert!(
        started.elapsed() >= Duration::from_secs(60),
        "not before the wait: {:?}",
        started.elapsed()
    );
    assert!(
        quiet.recv_within(Duration::from_secs(1)).await.is_none(),
        "the silent connection was let go"
    );
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let req = hub
        .attached(attach_result(
            "s-2",
            &info(false),
            (INC, 1),
            Some(snapshot_at(INC, 1, json!({}))),
            None,
        ))
        .await;
    assert!(
        req["params"].get("cursor").is_none(),
        "a seed that never reached Ready reseeds"
    );
    for seq in 2..=4 {
        tokio::time::sleep(Duration::from_secs(50)).await;
        hub.event("s-2", seq, text(&format!("replayed {seq}")))
            .await;
    }
    tokio::time::sleep(Duration::from_secs(50)).await;
    hub.synchronized("s-2", 4).await;
    let mut checker = LaneChecker::new();
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(600),
        "the reseed's Ready",
        |e| matches!(e, LaneEvent::Ready { generation: 2 }),
    )
    .await;
    assert_eq!(
        resets(&f),
        [("connect".to_string(), 1), ("reconnect".to_string(), 2)]
    );
    assert_eq!(checker.stales(), 1);
    assert_eq!(
        dial.dials(),
        2,
        "200 s of steady replay was never cut short"
    );
}

/// Serve every connection a lane makes up to its attach, answering the
/// attach with a snapshot and then saying nothing — a host that never
/// synchronizes. Counts the attaches it answered.
fn serve_without_synchronized(
    mut conns: tokio::sync::mpsc::UnboundedReceiver<shed_craze::testing::HubEnd>,
) -> Arc<std::sync::atomic::AtomicUsize> {
    let answered = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let count = Arc::clone(&answered);
    tokio::spawn(async move {
        let mut held = Vec::new();
        while let Some(mut hub) = conns.recv().await {
            hub.splice(HOST).await;
            hub.listed(row()).await;
            // A lane at its bound ends here, before attaching.
            let Some(req) = hub.recv_within(Duration::from_secs(5)).await else {
                continue;
            };
            assert_eq!(req["method"], "session.attach");
            hub.reply(
                &req,
                attach_result(
                    SUB,
                    &info(false),
                    (INC, 1),
                    Some(snapshot_at(INC, 1, json!({}))),
                    None,
                ),
            )
            .await;
            hub.registry(json!([]), 1).await;
            count.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            held.push(hub);
        }
    });
    answered
}

/// **The bounds still end a lane whose host never synchronizes**: every
/// attach is answered and none is ever synchronized, so neither the
/// per-episode count nor the outage is ever reset. The ninth attach of the
/// episode is the bound (`Down{"re-attach bound"}`, the ten-minute bound held
/// off); and with the attach bound held off, the ten-minute one ends it
/// (`Down{"unreachable"}`).
#[tokio::test(start_paused = true)]
async fn a_host_that_never_synchronizes_is_ended_by_the_bounds() {
    let (dial, conns) = shed_craze::testing::ScriptedDial::new();
    let answered = serve_without_synchronized(conns);
    let timings = shed_craze::LaneTimings {
        give_up: Duration::from_secs(24 * 3600),
        ..shed_craze::LaneTimings::default()
    };
    let lane = lane_on(dial.clone(), timings);
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut checker = LaneChecker::new();
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(4 * 3600),
        "the attach bound",
        is_down,
    )
    .await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "re-attach bound".into()
        })
    );
    assert_eq!(
        answered.load(std::sync::atomic::Ordering::SeqCst),
        9,
        "the first attach and eight re-attaches"
    );
    assert_eq!(dial.dials(), 10, "the tenth connection hit the bound");

    let (dial, conns) = shed_craze::testing::ScriptedDial::new();
    let answered = serve_without_synchronized(conns);
    let timings = shed_craze::LaneTimings {
        reattaches: 10_000,
        ..shed_craze::LaneTimings::default()
    };
    let lane = lane_on(dial.clone(), timings);
    let (mut rx, _stop) = subscribed(&lane).await;
    let started = tokio::time::Instant::now();
    let mut checker = LaneChecker::new();
    let f = drive_within(
        &mut rx,
        &mut checker,
        Duration::from_secs(4 * 3600),
        "the ten-minute bound",
        is_down,
    )
    .await;
    assert_eq!(
        f.last(),
        Some(&LaneEvent::Down {
            reason: "unreachable".into()
        })
    );
    let elapsed = started.elapsed();
    assert!(
        elapsed >= Duration::from_secs(600) && elapsed < Duration::from_secs(600 + 120),
        "the outage began at the first silent attachment's loss: {elapsed:?}"
    );
    assert!(answered.load(std::sync::atomic::Ordering::SeqCst) >= 5);
}

/// **The spliced host is checked on every path.** A hub that splices another
/// host than the hostId the connect named: the watcher ends the lane on it
/// (`Down{"protocol: …"}`), and a read without a subscription refuses it
/// (`Failed("protocol: …")`) — as it refuses a host whose own row names
/// another hostId. Never another host's session id, row or data.
#[tokio::test]
async fn a_splice_to_another_host_is_a_protocol_fault() {
    const OTHER: &str = "fedcba987654";
    // The watcher.
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice_answered_by(HOST, OTHER).await;
    let mut checker = LaneChecker::new();
    let f = drive(&mut rx, &mut checker, "the fault", is_down).await;
    assert!(
        matches!(f.last(), Some(LaneEvent::Down { reason })
            if reason.starts_with("protocol:") && reason.contains(OTHER)),
        "{:?}",
        f.last()
    );

    // The short reads: the host's hello names another host…
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    for read in ["history", "settings"] {
        let l = Arc::clone(&lane);
        let task = tokio::spawn(async move {
            match read {
                "history" => l.history(None, 50).await.map(|_| ()),
                _ => l.settings().await.map(|_| ()),
            }
        });
        let mut hub = next_conn(&mut conns).await;
        hub.splice_answered_by(HOST, OTHER).await;
        let got = task.await.unwrap();
        assert!(
            matches!(&got, Err(LaneError::Failed(m)) if m.starts_with("protocol:") && m.contains(OTHER)),
            "{read}: {got:?}"
        );
    }
    // …or its own row does.
    let l = Arc::clone(&lane);
    let task = tokio::spawn(async move { l.approvals().await });
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    let other = session_info(
        OTHER,
        "session-other",
        INC,
        shed_craze::testing::session_caps(false),
    );
    hub.listed(host_session_row(&other, json!({}))).await;
    let got = task.await.unwrap();
    assert!(
        matches!(&got, Err(LaneError::Failed(m)) if m.starts_with("protocol:") && m.contains(OTHER)),
        "the row: {got:?}"
    );
    assert_eq!(
        lane.craze_session_id().as_deref(),
        Some(SID),
        "the other host's session id was never learned"
    );
}

/// **The seeded reads belong to the watcher that seeded them.** Subscription
/// A seeds; subscription B seeds after it; A's end then leaves B's fold
/// answering the reads (no dial). Once B ends too, nothing is seeded and a
/// read dials a connection of its own.
#[tokio::test]
async fn the_seeded_reads_belong_to_the_watcher_that_seeded_them() {
    let (dial, mut conns, lane) = scripted(fast());
    let mut checker_a = LaneChecker::new();
    let (mut rx_a, stop_a) = subscribed(&lane).await;
    let (mut hub_a, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub_a.event(SUB, 2, permission_event("perm-1")).await;
    hub_a.synchronized(SUB, 2).await;
    drive(&mut rx_a, &mut checker_a, "A's seed", is_ready).await;

    let mut checker_b = LaneChecker::new();
    let (mut rx_b, stop_b) = subscribed(&lane).await;
    let (mut hub_b, _) = seed_conn(&mut conns, "s-b", 2, json!({})).await;
    hub_b.event("s-b", 3, permission_event("perm-2")).await;
    hub_b.synchronized("s-b", 3).await;
    drive(&mut rx_b, &mut checker_b, "B's seed", is_ready).await;

    // A ends AFTER B seeded: B's fold still answers, without a dial.
    stop_a.stop();
    drop(rx_a);
    tokio::time::sleep(Duration::from_millis(100)).await;
    let dials = dial.dials();
    let open: Vec<String> = lane
        .approvals()
        .await
        .unwrap()
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert_eq!(open, ["perm-2"], "B's fold, still seeded");
    assert!(lane.settings().await.is_ok());
    assert_eq!(dial.dials(), dials, "the cached reads never dialled");

    // B ends: nothing is seeded, so a read dials its own connection.
    stop_b.stop();
    drop(rx_b);
    tokio::time::sleep(Duration::from_millis(100)).await;
    let l = lane.clone();
    let task = tokio::spawn(async move { l.approvals().await });
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let req = hub.expect("asks.list").await;
    assert_eq!(req["params"], json!({"sessionId": SID}));
    hub.reply(&req, json!({"asks": []})).await;
    assert!(task.await.unwrap().unwrap().is_empty());
    assert_eq!(dial.dials(), dials + 1, "unseeded, the read dialled");
    drop((hub_a, hub_b));
}

/// **A reseed unseeds the reads until its own `Ready`**: while a watcher
/// rebuilds its fold (a `replay_failed` reset, its no-cursor re-attach
/// answered, no `synchronized` yet), `approvals()` does not answer from the
/// fold being rebuilt — it reads the registry, on the live connection.
#[tokio::test]
async fn a_reseed_unseeds_the_reads_until_its_ready() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.reset(SUB, "replay_failed").await;
    hub.attached(attach_result(
        "s-2",
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    drive(&mut rx, &mut checker, "the reseed's Reset", |e| {
        matches!(e, LaneEvent::Reset { generation: 2, .. })
    })
    .await;
    let lane = Arc::new(lane);
    let l = Arc::clone(&lane);
    let read = tokio::spawn(async move { l.approvals().await });
    let req = hub
        .recv_within(SCRIPT_WAIT)
        .await
        .expect("mid-reseed, the read asked craze rather than the fold");
    assert_eq!(req["method"], "asks.list");
    hub.reply(&req, json!({"asks": []})).await;
    assert!(read.await.unwrap().unwrap().is_empty());
}

/// **Another watcher's reseed never unseeds this one's reads** (C8
/// confirmation review). A seeds; B seeds after it and owns the reads; A's
/// connection drops, its redial's cursor is REFUSED and A reseeds — then A is
/// stopped before its `synchronized`. B is healthy throughout, so its fold
/// still answers `approvals()` and `settings()`, without a dial.
#[tokio::test]
async fn another_watchers_reseed_never_unseeds_the_reads() {
    let (dial, mut conns, lane) = scripted(fast());
    let mut perm_open = snapshot_at(INC, 2, json!({}));
    perm_open["asks"] = json!([{"id": "perm-1", "kind": "permission",
        "body": {"permission": {"id": "perm-1", "tool": "Shell", "options": []}}}]);

    let mut checker_a = LaneChecker::new();
    let (mut rx_a, stop_a) = subscribed(&lane).await;
    let (mut hub_a, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub_a.event(SUB, 2, permission_event("perm-1")).await;
    hub_a.synchronized(SUB, 2).await;
    drive(&mut rx_a, &mut checker_a, "A's seed", is_ready).await;

    let mut checker_b = LaneChecker::new();
    let (mut rx_b, _stop_b) = subscribed(&lane).await;
    let mut hub_b = next_conn(&mut conns).await;
    hub_b.splice(HOST).await;
    hub_b.listed(row()).await;
    let perm_1 = permission_event("perm-1")["permission"].clone();
    let registry = json!([ask_record("permission", &perm_1, "2026-01-01T00:00:01Z")]);
    hub_b
        .attached_only(attach_result(
            "s-b",
            &info(false),
            (INC, 2),
            Some(perm_open.clone()),
            None,
        ))
        .await;
    hub_b.registry(registry.clone(), 2).await;
    hub_b.synchronized("s-b", 2).await;
    drive(&mut rx_b, &mut checker_b, "B's seed", is_ready).await;

    // A loses its connection; its redial offers the cursor and is refused.
    hub_a.close().await;
    let mut hub_a = next_conn(&mut conns).await;
    hub_a.splice(HOST).await;
    hub_a.listed(row()).await;
    let req = hub_a
        .attached_only(attach_result(
            "s-a2",
            &info(false),
            (INC, 2),
            Some(perm_open),
            Some("journal_gap"),
        ))
        .await;
    hub_a.registry(registry, 2).await;
    assert!(
        req["params"].get("cursor").is_some(),
        "A offered its cursor"
    );
    drive(&mut rx_a, &mut checker_a, "A's reseed", |e| {
        matches!(e, LaneEvent::Reset { generation: 2, .. })
    })
    .await;
    // …and is stopped before its synchronized.
    stop_a.stop();
    drop(rx_a);
    tokio::time::sleep(Duration::from_millis(100)).await;

    let dials = dial.dials();
    let open: Vec<String> = lane
        .approvals()
        .await
        .expect("B's fold answers")
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert_eq!(open, ["perm-1"]);
    lane.settings().await.expect("B's fold answers");
    assert_eq!(
        dial.dials(),
        dials,
        "B still owns the reads: neither dialled"
    );
    drop((hub_a, hub_b));
}

// ---- Amendment A11: approvals from craze's ask registry, rows from its
// transcript, the fenced seed ----

/// The `Approval` frames among `frames`, last write per id.
fn approvals_seen(frames: &[LaneEvent]) -> std::collections::BTreeMap<String, bool> {
    let mut seen = std::collections::BTreeMap::new();
    for e in frames {
        if let LaneEvent::Approval { approval } = e {
            seen.insert(approval.id.clone(), approval.status.is_pending());
        }
    }
    seen
}

fn sub_permission(id: &str) -> Value {
    let mut ev = permission_event(id);
    ev["agent"] = json!("child-1");
    ev
}

/// **A sub-agent's ask is an answerable approval, seeded and live.** Seeded:
/// the snapshot lacks it (craze's transcript child-ignores it) but the
/// registry lists it, so it is an `Approval` before the `Ready`. Live: a
/// sub-agent's opening is an `Approval` with no transcript row, and its
/// ending resolves it with none either. Both answer through `asks.answer`.
#[tokio::test]
async fn a_sub_agents_ask_is_an_answerable_approval_seeded_and_live() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached_only(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    let seeded_ask = sub_permission("perm-seeded")["permission"].clone();
    hub.registry(
        json!([ask_record(
            "permission",
            &seeded_ask,
            "2026-01-01T00:00:01Z"
        )]),
        1,
    )
    .await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert_eq!(
        approvals_seen(&seed),
        [("perm-seeded".to_string(), true)].into()
    );
    assert!(texts(&seed).is_empty(), "no transcript row for it");
    assert_eq!(lane.session().await.unwrap().pending_approvals, 1);

    hub.event(SUB, 2, sub_permission("perm-live")).await;
    let live = drive(
        &mut rx,
        &mut checker,
        "the live opening",
        |e| matches!(e, LaneEvent::Approval { approval } if approval.id == "perm-live"),
    )
    .await;
    assert!(texts(&live).is_empty(), "no transcript row for it");
    let open: Vec<String> = lane
        .approvals()
        .await
        .unwrap()
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert_eq!(open, ["perm-live", "perm-seeded"]);

    for id in ["perm-seeded", "perm-live"] {
        let l = Arc::clone(&lane);
        let answer = tokio::spawn(async move {
            l.answer(
                id,
                LaneAnswer::Choice {
                    option_id: "allow".into(),
                },
            )
            .await
        });
        let req = hub.expect("asks.answer").await;
        assert_eq!(req["params"]["askId"], id);
        assert_eq!(req["params"]["answer"], json!({"optionId": "allow"}));
        hub.reply(&req, json!({})).await;
        answer.await.unwrap().unwrap();
    }
    let mut ending = json!({"type": "ask", "agent": "child-1", "ask": {"id": "perm-live",
        "kind": "permission", "outcome": "answered", "by": "client", "optionId": "allow"}});
    hub.event(SUB, 3, ending.take()).await;
    let ended = drive(&mut rx, &mut checker, "the live ending", |e| {
        matches!(e, LaneEvent::Approval { approval } if approval.id == "perm-live" && !approval.status.is_pending())
    })
    .await;
    assert!(texts(&ended).is_empty(), "no transcript row for its ending");
}

/// **`asks.get` saying `resolved`** — the ask ended between `asks.list` and
/// `asks.get` — is no approval at the seed: not an `Approval` frame, not in
/// `approvals()`.
#[tokio::test]
async fn an_ask_resolved_between_list_and_get_is_no_approval() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached_only(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    let perm = permission_event("perm-x")["permission"].clone();
    let mut resolved = ask_record("permission", &perm, "2026-01-01T00:00:01Z");
    resolved["status"] = json!("resolved");
    hub.registry(json!([resolved]), 1).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert!(approvals_seen(&seed).is_empty(), "{seed:#?}");
    assert!(lane.approvals().await.unwrap().is_empty());
}

/// **`asks.get` REFUSED `unknown_ask`** — craze's own client (`Session.Ask`)
/// treats only this code as "the ask ended since the list" — is skipped the
/// same way a `resolved` record is: no `Approval` frame, and the seed still
/// reaches `Ready` (plan 025 CR-fix finding 2).
#[tokio::test]
async fn an_asks_get_refused_unknown_ask_is_skipped_and_the_lane_seeds() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached_only(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    let list = hub.expect("asks.list").await;
    hub.reply(
        &list,
        json!({"asks": [{"id": "ask-gone", "kind": "permission", "label": "",
                          "openedAt": "2026-01-01T00:00:01Z"}]}),
    )
    .await;
    let get = hub.expect("asks.get").await;
    assert_eq!(get["params"]["askId"], "ask-gone");
    hub.refuse(
        &get,
        shed_craze::wire::code::UNKNOWN_ASK,
        "forgotten",
        json!({}),
    )
    .await;
    let sync = hub.expect("session.sync").await;
    hub.reply(&sync, json!({"seq": 1})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert!(approvals_seen(&seed).is_empty(), "{seed:#?}");
    assert!(lane.approvals().await.unwrap().is_empty());
}

/// **`asks.get` refused with ANY OTHER code** is not "gone since the list" —
/// it is a fault, exactly like `asks.list`'s own refusal a few lines up: the
/// connection is dropped and the next one reseeds. The catch-all this fixes
/// would instead have dropped the ask silently, leaving an open "needs you"
/// ask invisible — the regression Amendment A11 exists to prevent (plan 025
/// CR-fix finding 2).
#[tokio::test]
async fn an_asks_get_refused_with_another_code_faults_the_lane() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached_only(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    let list = hub.expect("asks.list").await;
    hub.reply(
        &list,
        json!({"asks": [{"id": "ask-bad", "kind": "permission", "label": "",
                          "openedAt": "2026-01-01T00:00:01Z"}]}),
    )
    .await;
    let get = hub.expect("asks.get").await;
    assert_eq!(get["params"]["askId"], "ask-bad");
    hub.refuse(
        &get,
        shed_craze::wire::code::BAD_REQUEST,
        "malformed",
        json!({}),
    )
    .await;
    let (mut hub2, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub2.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let frames = drive(&mut rx, &mut checker, "the fault then reseed", is_ready).await;
    assert_eq!(
        resets(&frames).last(),
        Some(&("protocol".to_string(), 1)),
        "{frames:#?}"
    );
    drop(hub);
}

/// **A hidden kind is never an approval**, seeded or live — craze's own
/// `HiddenBy`: on a session whose capabilities lack `askCards`, a QUESTION is
/// hidden (no `Approval`, no card row), while a PERMISSION is never hidden —
/// the registry's and a live one are approvals all the same.
#[tokio::test]
async fn a_hidden_kind_is_never_an_approval_through_the_lane() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut caps = shed_craze::testing::session_caps(false);
    caps["askCards"] = json!(false);
    let hidden = session_info(HOST, SID, INC, caps);
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(host_session_row(&hidden, json!({}))).await;
    hub.attached_only(attach_result(
        SUB,
        &hidden,
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    let perm = permission_event("perm-h")["permission"].clone();
    let q = json!({"id": "q-h", "title": "Pick", "questions": []});
    hub.registry(
        json!([
            ask_record("permission", &perm, "2026-01-01T00:00:01Z"),
            ask_record("question", &q, "2026-01-01T00:00:01Z")
        ]),
        1,
    )
    .await;
    hub.event(SUB, 2, permission_event("perm-live")).await;
    hub.event(
        SUB,
        3,
        json!({"type": "question", "question": {"id": "q-live", "title": "Pick live", "questions": []}}),
    )
    .await;
    hub.synchronized(SUB, 3).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert_eq!(
        approvals_seen(&seed),
        [
            ("perm-h".to_string(), true),
            ("perm-live".to_string(), true)
        ]
        .into(),
        "the permissions, never the questions"
    );
    assert_eq!(
        texts(&seed),
        ["Shell"],
        "the live permission's card row only"
    );
    let open: Vec<String> = lane
        .approvals()
        .await
        .unwrap()
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert_eq!(open, ["perm-h", "perm-live"]);
}

/// **The fence** (Amendment A11): the registry is read after the attach and
/// before `session.sync`, and the `Ready` waits for every event through the
/// barrier. The scripted host's order: the attach reply and its
/// `synchronized`; `perm-a` opened (2) and resolved (3) before `asks.list`;
/// the list naming `perm-b` (a sub-agent's — no snapshot carries it), open
/// at `asks.get`; `perm-b` resolved (4) before `session.sync`, which says 4.
/// At the `Ready`, neither is pending — though `perm-b` was, in the registry
/// the seed started from.
#[tokio::test]
async fn the_ready_waits_for_the_fence() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached_only(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    hub.synchronized(SUB, 1).await;
    hub.event(SUB, 2, permission_event("perm-a")).await;
    hub.event(
        SUB,
        3,
        json!({"type": "ask", "ask": {"id": "perm-a", "kind": "permission",
            "outcome": "answered", "by": "client", "optionId": "allow"}}),
    )
    .await;
    let perm_b = sub_permission("perm-b")["permission"].clone();
    assert!(
        hub.registry_reads(json!([ask_record(
            "permission",
            &perm_b,
            "2026-01-01T00:00:00Z"
        )]))
        .await
    );
    hub.event(
        SUB,
        4,
        json!({"type": "ask", "agent": "child-1", "ask": {"id": "perm-b", "kind": "permission",
            "outcome": "answered", "by": "client", "optionId": "allow"}}),
    )
    .await;
    let sync = hub.expect("session.sync").await;
    assert_eq!(sync["params"], json!({"sessionId": SID}));
    hub.reply(&sync, json!({"seq": 4})).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let at_ready = approvals_seen(&seed);
    assert!(
        at_ready.values().all(|pending| !pending),
        "nothing pending at the Ready: {at_ready:?}"
    );
    assert_eq!(
        at_ready.get("perm-b"),
        Some(&false),
        "the registry's, resolved by the fenced event"
    );
    assert!(lane.approvals().await.unwrap().is_empty());
    assert_eq!(lane.session().await.unwrap().pending_approvals, 0);
}

/// **A silent resume re-reads the registry** before its lone `Ready`: an
/// approval the registry no longer lists was resolved while the lane looked
/// away (the client is told), one the fold never saw is opened — with no
/// transcript row, and no `Reset`.
#[tokio::test]
async fn a_silent_resume_re_reads_the_registry() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.event(SUB, 2, permission_event("perm-1")).await;
    hub.synchronized(SUB, 2).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.close().await;
    drive(&mut rx, &mut checker, "Stale", |e| {
        matches!(e, LaneEvent::Stale { .. })
    })
    .await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    let attach = hub
        .attached_only(attach_result("s-2", &info(false), (INC, 2), None, None))
        .await;
    assert!(attach["params"].get("cursor").is_some(), "a resume");
    let perm_sub = sub_permission("perm-sub")["permission"].clone();
    hub.registry(
        json!([ask_record("permission", &perm_sub, "2026-01-01T00:00:03Z")]),
        2,
    )
    .await;
    hub.synchronized("s-2", 2).await;
    let resumed = drive(&mut rx, &mut checker, "the lone Ready", is_ready).await;
    assert!(resets(&resumed).is_empty(), "silent: {resumed:#?}");
    assert_eq!(
        approvals_seen(&resumed),
        [
            ("perm-1".to_string(), false),
            ("perm-sub".to_string(), true)
        ]
        .into()
    );
    assert!(texts(&resumed).is_empty());
    let open: Vec<String> = lane
        .approvals()
        .await
        .unwrap()
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert_eq!(open, ["perm-sub"]);
}

/// **`approvals` is craze's own session capability, never the cards**
/// (Amendment A11 item 5): a session with NEITHER `askCards` nor `planCards`,
/// whose capabilities state `approvals: true`, advertises approvals — and its
/// pending permission (never hidden) is an `Approval` answerable through the
/// lane.
#[tokio::test]
async fn a_session_with_no_cards_still_advertises_and_answers_approvals() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut caps = shed_craze::testing::session_caps(false);
    caps["askCards"] = json!(false);
    caps["planCards"] = json!(false);
    assert_eq!(caps["approvals"], json!(true));
    let cardless = session_info(HOST, SID, INC, caps);
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(host_session_row(&cardless, json!({}))).await;
    hub.attached_only(attach_result(
        SUB,
        &cardless,
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    let perm = permission_event("perm-1")["permission"].clone();
    hub.registry(
        json!([ask_record("permission", &perm, "2026-01-01T00:00:01Z")]),
        1,
    )
    .await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    assert!(
        seed.iter().any(
            |e| matches!(e, LaneEvent::Capabilities { capabilities } if capabilities.approvals)
        ),
        "approvals advertised: {seed:#?}"
    );
    assert_eq!(approvals_seen(&seed), [("perm-1".to_string(), true)].into());
    let l = Arc::clone(&lane);
    let answer = tokio::spawn(async move {
        l.answer(
            "perm-1",
            LaneAnswer::Choice {
                option_id: "allow".into(),
            },
        )
        .await
    });
    let req = hub.expect("asks.answer").await;
    assert_eq!(req["params"]["askId"], "perm-1");
    hub.reply(&req, json!({})).await;
    answer.await.unwrap().unwrap();
}

// ---- the settings milestone (plan 025 §3.10, C11) ----

/// The last `Settings` among `frames`.
fn last_settings(frames: &[LaneEvent]) -> Option<shed_core::lane::LaneSettings> {
    frames.iter().rev().find_map(|e| match e {
        LaneEvent::Settings { settings } => Some(settings.clone()),
        _ => None,
    })
}

fn is_settings(e: &LaneEvent) -> bool {
    matches!(e, LaneEvent::Settings { .. })
}

/// The effort option's current value in `s`.
fn effort(s: &shed_core::lane::LaneSettings) -> Option<String> {
    s.options
        .iter()
        .find(|o| o.id == "effort")
        .map(|o| o.current.clone())
}

/// A `meta` delta setting the effort option (the WIRE/01 option, `current`
/// as given) — what craze writes ahead of a config set's reply.
fn effort_delta(current: &str) -> Value {
    json!({"type": "meta", "state": {"config": {"options": [
        {"id": "effort", "name": "Effort", "category": "thought_level", "type": "select", "current": current,
         "selectValues": [{"value": "low", "name": "Low"}, {"value": "medium", "name": "Medium"}, {"value": "high", "name": "High"}]}]}}})
}

/// **`set` is `session.set`, bound to the model the lane shows** (plan 025
/// §3.10): each change a fresh commandId on the lane's connection; a config
/// change carries `forModel` — the model as the lane last folded it, so after
/// a model change the NEXT config change is bound to the new one; the `meta`
/// delta ahead of the reply is what re-emits `Settings`. A refusal maps
/// through the table (`stale_model` → `NotAccepting`), and the retry is a NEW
/// commandId.
#[tokio::test]
async fn set_is_session_set_bound_to_the_model_the_lane_shows() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let s = last_settings(&seed).expect("the seed's Settings");
    assert_eq!(s.model.as_deref(), Some("grok"));

    let set = |change: LaneSettingChange| {
        let l = Arc::clone(&lane);
        tokio::spawn(async move { l.set(change).await })
    };
    let command_id = |req: &Value| -> u64 {
        req["params"]["commandId"]
            .as_str()
            .and_then(|c| c.parse().ok())
            .unwrap_or_else(|| panic!("a canonical commandId: {req}"))
    };

    // A config change, bound to the model the lane shows.
    let pending = set(LaneSettingChange::Config {
        id: "effort".into(),
        value: "high".into(),
        for_model: None,
    });
    let req = hub.expect("session.set").await;
    assert_eq!(req["params"]["sessionId"], SID);
    assert_eq!(
        req["params"]["setting"],
        json!({"kind": "config", "id": "effort", "value": "high", "forModel": "grok"})
    );
    let first = command_id(&req);
    hub.event(SUB, 2, effort_delta("high")).await;
    hub.reply(&req, json!({"value": "high", "rev": 2})).await;
    pending.await.unwrap().unwrap();
    let f = drive(&mut rx, &mut checker, "the delta's Settings", is_settings).await;
    assert_eq!(effort(&last_settings(&f).unwrap()).as_deref(), Some("high"));

    // A model change: its id alone.
    let pending = set(LaneSettingChange::Model { id: "fast".into() });
    let req = hub.expect("session.set").await;
    assert_eq!(
        req["params"]["setting"],
        json!({"kind": "model", "value": "fast"})
    );
    let second = command_id(&req);
    assert!(second > first, "a fresh commandId: {first} then {second}");
    hub.event(SUB, 3, json!({"type": "meta", "state": {"model": "fast"}}))
        .await;
    hub.reply(&req, json!({"value": "fast", "rev": 3})).await;
    pending.await.unwrap().unwrap();
    let f = drive(&mut rx, &mut checker, "the model's Settings", is_settings).await;
    assert_eq!(last_settings(&f).unwrap().model.as_deref(), Some("fast"));

    // The next config change is bound to the model the lane now shows — and
    // craze, which has moved on again, refuses it `stale_model`.
    let pending = set(LaneSettingChange::Config {
        id: "effort".into(),
        value: "low".into(),
        for_model: None,
    });
    let req = hub.expect("session.set").await;
    assert_eq!(req["params"]["setting"]["forModel"], "fast");
    let third = command_id(&req);
    hub.refuse(&req, "stale_model", "stale_model", json!({}))
        .await;
    let refused = pending.await.unwrap();
    assert_eq!(
        refused,
        Err(LaneError::NotAccepting),
        "the table's stale_model"
    );

    // The retry is a NEW command.
    let pending = set(LaneSettingChange::Config {
        id: "effort".into(),
        value: "low".into(),
        for_model: None,
    });
    let req = hub.expect("session.set").await;
    assert!(
        command_id(&req) > third,
        "a retry never reuses the refused id"
    );
    hub.event(SUB, 4, effort_delta("low")).await;
    hub.reply(&req, json!({"value": "low", "rev": 4})).await;
    pending.await.unwrap().unwrap();

    // A mode change: its id alone.
    let pending = set(LaneSettingChange::Mode { id: "plan".into() });
    let req = hub.expect("session.set").await;
    assert_eq!(
        req["params"]["setting"],
        json!({"kind": "mode", "value": "plan"})
    );
    hub.refuse(&req, "unsupported", "unsupported", json!({}))
        .await;
    assert!(
        matches!(pending.await.unwrap(), Err(LaneError::Failed(_))),
        "unsupported is Failed (the table)"
    );
}

/// **A set lost to a drop is never resent, and the resume restates the
/// settings** (plan 025 §3.10): the reply lost with the connection is "outcome
/// unknown"; the resume's connection carries the attach and no `session.set`;
/// and before its lone `Ready` the resume says `Settings` — changed or not —
/// so a client showing the change "not confirmed" has the real value to
/// replace it with.
#[tokio::test]
async fn a_set_lost_to_a_drop_is_never_resent_and_the_resume_restates_the_settings() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let l = Arc::clone(&lane);
    let set = tokio::spawn(async move {
        l.set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "high".into(),
            for_model: None,
        })
        .await
    });
    hub.expect("session.set").await;
    hub.close().await;
    let got = set.await.unwrap().unwrap_err();
    assert!(is_outcome_unknown(&got), "{got:?}");
    drive(&mut rx, &mut checker, "Stale", |e| {
        matches!(e, LaneEvent::Stale { .. })
    })
    .await;

    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached(attach_result("s-2", &info(false), (INC, 1), None, None))
        .await;
    hub.synchronized("s-2", 1).await;
    let resumed = drive(&mut rx, &mut checker, "the lone Ready", is_ready).await;
    assert!(resets(&resumed).is_empty(), "a silent resume: {resumed:#?}");
    let restated = last_settings(&resumed).expect("the resume restates the settings");
    assert_eq!(
        effort(&restated).as_deref(),
        Some("medium"),
        "the change did not take: the real value, unchanged"
    );
    assert!(
        hub.recv_within(Duration::from_millis(300)).await.is_none(),
        "the set was resent"
    );
}

/// **craze's model order, on the stream** (plan 025 §3.10; PM "The session
/// info document"): the current model first, the remembered ones by `recent`
/// ascending, then the rest in the catalog's order — whatever order the
/// catalog lists them in.
#[tokio::test]
async fn the_models_are_ordered_current_then_by_rank_then_catalog() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut i = info(false);
    i["catalogs"]["models"] = json!([
        {"id": "x", "name": "X"},
        {"id": "fireworks/kimi-k3", "name": "Kimi K3 (Fireworks)", "recent": 2},
        {"id": "cur", "name": "Current"},
        {"id": "muse-spark-1.3-contributor", "name": "Muse Spark 1.3 Contributor (Meta)", "recent": 1},
        {"id": "y", "name": "Y"}
    ]);
    let mut snap = snapshot_at(INC, 1, json!({}));
    snap["settings"]["model"] = json!("cur");
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(host_session_row(&i, json!({}))).await;
    hub.attached(attach_result(SUB, &i, (INC, 1), Some(snap), None))
        .await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let order: Vec<String> = last_settings(&seed)
        .unwrap()
        .models
        .into_iter()
        .map(|m| m.id)
        .collect();
    assert_eq!(
        order,
        [
            "cur",
            "muse-spark-1.3-contributor",
            "fireworks/kimi-k3",
            "x",
            "y"
        ]
    );
}

/// **A catalog is applied only at an equal or higher revision** (plan 025
/// §3.10; PM "Live models"), on the stream: a `catalog` delta at revision 2
/// re-emits `Settings`; one at revision 1 — older than the list held — draws
/// nothing; revision 3 is applied again.
#[tokio::test]
async fn a_catalog_below_the_held_revision_is_never_applied() {
    let (_dial, mut conns, lane) = scripted(fast());
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let catalog = |seq: u64, ids: &[&str], revision: u64| {
        let models: Vec<Value> = ids
            .iter()
            .map(|id| json!({"id": id, "name": id.to_uppercase()}))
            .collect();
        (
            seq,
            json!({"type": "meta", "state": {"catalog": {"models": models, "revision": revision}}}),
        )
    };
    let ids = |s: &shed_core::lane::LaneSettings| -> Vec<String> {
        s.models.iter().map(|m| m.id.clone()).collect()
    };

    let (seq, ev) = catalog(2, &["grok", "k3"], 2);
    hub.event(SUB, seq, ev).await;
    let f = drive(&mut rx, &mut checker, "revision 2", is_settings).await;
    assert_eq!(ids(&last_settings(&f).unwrap()), ["grok", "k3"]);

    // Older than the list held: ignored. A text event after it is the marker
    // that the stream moved on with no `Settings` from it.
    let (seq, ev) = catalog(3, &["grok", "old"], 1);
    hub.event(SUB, seq, ev).await;
    hub.event(SUB, 4, text("after the old catalog")).await;
    hub.event(SUB, 5, json!({"type": "done", "stopReason": "end_turn"}))
        .await;
    let f = drive(&mut rx, &mut checker, "the marker", |e| {
        matches!(e, LaneEvent::Message { message, .. }
            if message.text.as_deref() == Some("after the old catalog"))
    })
    .await;
    assert!(
        last_settings(&f).is_none(),
        "a lower revision brought a list back: {f:#?}"
    );

    let (seq, ev) = catalog(6, &["grok", "k4"], 3);
    hub.event(SUB, seq, ev).await;
    let f = drive(&mut rx, &mut checker, "revision 3", is_settings).await;
    assert_eq!(ids(&last_settings(&f).unwrap()), ["grok", "k4"]);
}

/// **The model the client DISPLAYED binds a config change** (Amendment A13):
/// the lane's fold has already seen the session move to `fast`, but the client
/// still shows `grok` and chose the option there — the change goes out bound
/// to `grok`, and craze's `stale_model` refusal (`NotAccepting`) is what the
/// client hears, rather than the option being applied to `fast`. A change the
/// client sent no model with is bound to the fold's.
#[tokio::test]
async fn the_model_the_client_displayed_binds_a_config_change() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    hub.event(SUB, 2, json!({"type": "meta", "state": {"model": "fast"}}))
        .await;
    let f = drive(&mut rx, &mut checker, "the move, folded", is_settings).await;
    assert_eq!(last_settings(&f).unwrap().model.as_deref(), Some("fast"));

    let l = Arc::clone(&lane);
    let chosen_on_grok = tokio::spawn(async move {
        l.set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "high".into(),
            for_model: Some("grok".into()),
        })
        .await
    });
    let req = hub.expect("session.set").await;
    assert_eq!(
        req["params"]["setting"]["forModel"], "grok",
        "the model the client displayed, not the fold's"
    );
    hub.refuse(&req, "stale_model", "stale_model", json!({}))
        .await;
    assert_eq!(chosen_on_grok.await.unwrap(), Err(LaneError::NotAccepting));

    let l = Arc::clone(&lane);
    let unbound = tokio::spawn(async move {
        l.set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "high".into(),
            for_model: None,
        })
        .await
    });
    let req = hub.expect("session.set").await;
    assert_eq!(req["params"]["setting"]["forModel"], "fast", "the fold's");
    hub.event(SUB, 3, effort_delta("high")).await;
    hub.reply(&req, json!({"value": "high", "rev": 3})).await;
    unbound.await.unwrap().unwrap();
}

/// **A config change racing the lane's first seed waits for its settings**
/// (Amendment A13; C11 review): issued once the lane's connection is live but
/// before the seed's `Settings`, it is not written until they arrive — and
/// then goes out bound to the session's model, never unbound. A lane whose
/// settings never come refuses it `Unavailable` (unsent) at the verb deadline.
#[tokio::test]
async fn a_config_change_racing_the_seed_waits_for_the_settings() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let mut hub = next_conn(&mut conns).await;
    hub.splice(HOST).await;
    hub.listed(row()).await;
    hub.attached_only(attach_result(
        SUB,
        &info(false),
        (INC, 1),
        Some(snapshot_at(INC, 1, json!({}))),
        None,
    ))
    .await;
    // Attached (the connection is the verbs' now), not yet seeded.
    let l = Arc::clone(&lane);
    let early = tokio::spawn(async move {
        l.set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "high".into(),
            for_model: None,
        })
        .await
    });
    tokio::time::sleep(Duration::from_millis(150)).await;
    // The fenced read comes first, untouched by the change in waiting.
    hub.registry(json!([]), 1).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    let req = hub.expect("session.set").await;
    assert_eq!(
        req["params"]["setting"]["forModel"], "grok",
        "bound to the session's model once the settings arrived"
    );
    hub.event(SUB, 2, effort_delta("high")).await;
    hub.reply(&req, json!({"value": "high", "rev": 2})).await;
    early.await.unwrap().unwrap();

    // Settings that never come: refused at the deadline, never sent.
    let mut t = fast();
    t.request = Duration::from_millis(300);
    let (_dial2, _conns2, lonely) = scripted(t);
    let got = lonely
        .set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "high".into(),
            for_model: None,
        })
        .await;
    assert!(
        matches!(&got, Err(LaneError::Unavailable(m)) if m.contains("not sent")),
        "{got:?}"
    );
}

/// **A change confirmed at `rev: 0` reaches the settings anyway** (C11
/// review): craze answers `session.set` with the confirmed value and `rev: 0`
/// when it could learn no revision — and then NO `meta` delta follows. The
/// lane applies the confirmed value itself and re-emits `Settings`, so a
/// client never keeps the old value for want of an event.
#[tokio::test]
async fn a_change_confirmed_at_rev_zero_reaches_the_settings() {
    let (_dial, mut conns, lane) = scripted(fast());
    let lane = Arc::new(lane);
    let (mut rx, _stop) = subscribed(&lane).await;
    let (mut hub, _) = seed_conn(&mut conns, SUB, 1, json!({})).await;
    hub.synchronized(SUB, 1).await;
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;

    let l = Arc::clone(&lane);
    let set = tokio::spawn(async move {
        l.set(LaneSettingChange::Config {
            id: "effort".into(),
            value: "high".into(),
            for_model: None,
        })
        .await
    });
    let req = hub.expect("session.set").await;
    // The confirmed value, no revision — and no delta.
    hub.reply(&req, json!({"value": "high", "rev": 0})).await;
    set.await.unwrap().unwrap();
    let f = drive(&mut rx, &mut checker, "the confirmed Settings", is_settings).await;
    assert_eq!(effort(&last_settings(&f).unwrap()).as_deref(), Some("high"));

    let l = Arc::clone(&lane);
    let set =
        tokio::spawn(async move { l.set(LaneSettingChange::Mode { id: "plan".into() }).await });
    let req = hub.expect("session.set").await;
    hub.reply(&req, json!({"value": "plan", "rev": 0})).await;
    set.await.unwrap().unwrap();
    let f = drive(&mut rx, &mut checker, "the confirmed mode", is_settings).await;
    assert_eq!(last_settings(&f).unwrap().mode.as_deref(), Some("plan"));
}
