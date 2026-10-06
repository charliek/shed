//! opencode, run through the contract's **conformance kit**
//! (`shed_core::lane::conformance`, plan 025 §3.2.4) — at both levels.
//!
//! The kit is the one statement of the stream rules every adapter keeps: the
//! bracket, `Capabilities` (and `Settings` only when advertised) before
//! `Ready`, `seq` climbing across reseeds, a silent resume keeping the
//! generation, `Stale` non-terminal and `Down` last, no source-set `tab_id`.
//! craze runs the same kit over its recipe (plan 025 C7/C8), so two adapters at two
//! levels cannot drift apart. Every frame below is judged as it is read; the
//! cells only steer opencode through the shapes worth judging.

mod common;

use std::time::Duration;

use common::{assert_clean, assistant_turn, lane_on, permission_asked, source, DEADLINE};
use shed_core::lane::conformance::{
    drive_lane, drive_source, DriveEnd, LaneChecker, SourceChecker,
};
use shed_core::lane::{AgentLane, AgentSource, LaneEvent, SourceEvent};
use shed_opencode::testing::FakeOpencode;

fn is_ready(e: &LaneEvent) -> bool {
    matches!(e, LaneEvent::Ready { .. })
}

/// A lane's whole life — seed, live rows and an approval, a reconnect that
/// reseeds, a second reseed, then the session deleted under it — conforms at
/// every frame, ends with `Down`, and never once says `Stale` or `Settings`
/// (opencode reseeds every reconnect, and has no settings).
#[tokio::test]
async fn an_opencode_lane_conforms_through_reseeds_and_its_end() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "root", "/w", None);
    fake.set_simple_transcript("ses_a", "hello", "hi there");
    fake.set_status("ses_a", "idle");
    fake.pin("ses_a");
    let lane = lane_on(&fake, "ses_a");
    let (mut rx, _stop) = lane.subscribe(None).await.expect("subscribe").into_parts();
    let mut checker = LaneChecker::new();

    let seed = drive_lane(&mut rx, &mut checker, DEADLINE, is_ready)
        .await
        .expect("the seed conforms");
    assert_eq!(seed.end, DriveEnd::Matched);
    assert_eq!(
        checker.live_capabilities(),
        Some(&shed_opencode::opencode_capabilities()),
        "the seed carried opencode's capabilities"
    );

    // Live frames: a turn and an approval.
    for frame in assistant_turn("ses_a", "msg_9", "prt_9", "live row") {
        fake.push_event(&frame);
    }
    fake.push_event(&permission_asked("ses_a", "per_1", "ls"));
    drive_lane(&mut rx, &mut checker, DEADLINE, |e| {
        matches!(e, LaneEvent::Approval { .. })
    })
    .await
    .expect("steady state conforms");

    // Two reconnects: each a full reseed, generations climbing, seqs climbing.
    for want in [2, 3] {
        fake.close_streams();
        drive_lane(&mut rx, &mut checker, DEADLINE, is_ready)
            .await
            .expect("the reseed conforms");
        assert_eq!(checker.live_generation(), Some(want));
    }

    // The session goes away: the next reseed answers 404 and the lane ENDS.
    fake.remove_session("ses_a");
    fake.close_streams();
    let end = drive_lane(&mut rx, &mut checker, DEADLINE, |_| false)
        .await
        .expect("the end conforms");
    assert_eq!(
        end.end,
        DriveEnd::Closed,
        "the stream closed, after its Down"
    );
    assert!(checker.ended());
    assert_eq!(
        checker.stales(),
        0,
        "opencode never resumes, so never Stale"
    );
    assert!(
        !end.frames
            .iter()
            .chain(seed.frames.iter())
            .any(|e| matches!(e, LaneEvent::Settings { .. })),
        "and never sends Settings"
    );
    assert_clean(&fake);
}

/// A lane whose FIRST connect never comes up ends with `Down` before any seed —
/// the "before the first seed" shape the kit allows.
#[tokio::test]
async fn an_opencode_lane_that_never_connects_ends_cleanly() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "root", "/w", None);
    fake.fail_get("/event", 500);
    let (mut rx, _stop) = lane_on(&fake, "ses_a")
        .subscribe(None)
        .await
        .expect("subscribe")
        .into_parts();
    let mut checker = LaneChecker::new();
    let drive = drive_lane(&mut rx, &mut checker, DEADLINE, |_| false)
        .await
        .expect("an early end conforms");
    assert_eq!(drive.end, DriveEnd::Closed);
    assert!(checker.ended(), "it ended with a Down");
    assert_eq!(checker.readies(), 0, "no seed ever completed");
}

/// The source's life — seed, a diff, an outage, the reseed after it — conforms
/// at every frame, and its stream does not end while it is held.
#[tokio::test]
async fn an_opencode_source_conforms_through_an_outage() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    let source = source(&fake).with_poll_interval(Duration::from_millis(20));
    let (mut rx, _stop) = source.subscribe().await.expect("subscribe").into_parts();
    let mut checker = SourceChecker::new();
    let ready = |e: &SourceEvent| matches!(e, SourceEvent::Ready { .. });

    drive_source(&mut rx, &mut checker, DEADLINE, ready)
        .await
        .expect("the seed conforms");
    fake.add_session("ses_b", "b", "/w", None);
    drive_source(&mut rx, &mut checker, DEADLINE, |e| {
        matches!(e, SourceEvent::Session { .. })
    })
    .await
    .expect("a diff conforms");
    fake.fail_get("/session", 500);
    drive_source(&mut rx, &mut checker, DEADLINE, |e| {
        matches!(e, SourceEvent::Offline { .. })
    })
    .await
    .expect("an outage conforms");
    fake.unfail_get("/session");
    drive_source(&mut rx, &mut checker, DEADLINE, ready)
        .await
        .expect("the reseed conforms");
    assert_eq!(checker.live_generation(), Some(2));
    let quiet = drive_source(&mut rx, &mut checker, Duration::from_millis(150), |_| false)
        .await
        .expect("a quiet source conforms");
    assert_eq!(quiet.end, DriveEnd::Deadline, "it is still subscribed");
    assert_clean(&fake);
}

/// The lanes a source opens are the lanes the kit judged above: `open` binds
/// the id it is given and reaches the same session.
#[tokio::test]
async fn open_binds_the_row_id() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    let lane = source(&fake).open("ses_a").await.expect("open binds");
    assert_eq!(lane.session_id(), "ses_a");
    assert!(
        fake.get_paths().is_empty(),
        "opening is binding: nothing was dialled"
    );
    assert_eq!(lane.session().await.expect("the row").id, "ses_a");
}
