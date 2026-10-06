//! `CrazeSource` against a SCRIPTED hub (`testing::ScriptedDial`): the cells
//! that need a hub to say exactly one thing at exactly one moment — a roster
//! `omitted` reset (a real one needs 513 hosts), an epoch change, an overflow,
//! a create whose connection drops at the worst instant. The recipe cells
//! (`tests/recipe_source.rs`) run the same paths against the real hub.

use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::conformance::{check_source, drive_source, SourceChecker};
use shed_core::lane::{
    AgentSource, LaneCreateRequest, LaneError, LanePromptOutcome, LaneProviderState, SourceEvent,
    SourceOffline, LANE_CHANNEL_CAPACITY,
};
use shed_core::rc::RcActivity;
use tokio::sync::mpsc::Receiver;

use super::*;
use crate::testing::{full_hub_capabilities, roster_row, HubEnd, ScriptedDial, SCRIPT_WAIT};
use crate::wire::RosterRow;

fn fast() -> Timings {
    Timings {
        dial: Duration::from_secs(5),
        hello: Duration::from_secs(5),
        request: Duration::from_secs(5),
        create: Duration::from_secs(120),
        backoff_base: Duration::from_millis(10),
        backoff_max: Duration::from_millis(40),
    }
}

fn source(dial: Arc<ScriptedDial>) -> CrazeSource {
    CrazeSource::new(dial, "shed-test").with_timings(fast())
}

async fn next_conn(conns: &mut tokio::sync::mpsc::UnboundedReceiver<HubEnd>) -> HubEnd {
    tokio::time::timeout(SCRIPT_WAIT, conns.recv())
        .await
        .expect("the source dialled in time")
        .expect("the dial is alive")
}

async fn next(rx: &mut Receiver<SourceEvent>, what: &str) -> SourceEvent {
    tokio::time::timeout(SCRIPT_WAIT, rx.recv())
        .await
        .unwrap_or_else(|_| panic!("timed out waiting for {what}"))
        .unwrap_or_else(|| panic!("the source stream ended waiting for {what}"))
}

/// Frames up to and including the first that matches.
async fn until(
    rx: &mut Receiver<SourceEvent>,
    what: &str,
    f: impl Fn(&SourceEvent) -> bool,
) -> Vec<SourceEvent> {
    let mut seen = Vec::new();
    loop {
        let ev = next(rx, what).await;
        let done = f(&ev);
        seen.push(ev);
        if done {
            return seen;
        }
    }
}

fn is_ready(e: &SourceEvent) -> bool {
    matches!(e, SourceEvent::Ready { .. })
}

fn row_a() -> Value {
    roster_row(
        "0a0a0a0a0a0a",
        "sess-a",
        "/w/alpha",
        json!({"title": "", "activity": "idle", "pendingAsks": 0}),
    )
}

fn row_b() -> Value {
    roster_row(
        "0b0b0b0b0b0b",
        "sess-b",
        "/w/beta",
        json!({"title": "beta work", "activity": "working", "pendingAsks": 0}),
    )
}

/// The seed, an upsert, a remove keyed by hostId — every frame through the
/// conformance kit.
#[tokio::test]
async fn the_roster_seeds_then_upserts_and_removes_by_host_id() {
    let (dial, mut conns) = ScriptedDial::new();
    let (mut rx, _stop) = source(dial).subscribe().await.unwrap().into_parts();
    let mut hub = next_conn(&mut conns).await;
    let hello = hub.hello("epoch-1", full_hub_capabilities()).await;
    assert_eq!(hello["params"]["client"]["kind"], "shed");
    assert_eq!(hello["params"]["client"]["name"], "shed-test");
    hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;

    let mut checker = SourceChecker::new();
    let seed = drive_source(&mut rx, &mut checker, SCRIPT_WAIT, is_ready)
        .await
        .unwrap();
    let rows: Vec<&LaneSession> = seed.frames.iter().filter_map(session_of).collect();
    assert_eq!(rows.len(), 1);
    assert_eq!(rows[0].id, "0a0a0a0a0a0a", "a row's id is its hostId (P11)");
    assert_eq!(
        rows[0].title, "alpha",
        "an empty title is the workspace's basename"
    );
    assert!(seed.frames.iter().any(|e| matches!(e,
        SourceEvent::Capabilities { capabilities } if capabilities.create && capabilities.create_options && capabilities.kind == "craze")));

    hub.roster("r-1", "epoch-1", json!([row_b()]), json!(["0a0a0a0a0a0a"]))
        .await;
    let live = drive_source(&mut rx, &mut checker, SCRIPT_WAIT, |e| {
        matches!(e, SourceEvent::Removed { .. })
    })
    .await
    .unwrap();
    assert!(
        matches!(&live.frames[0], SourceEvent::Session { session } if session.id == "0b0b0b0b0b0b" && session.title == "beta work")
    );
    assert_eq!(
        live.frames.last(),
        Some(&SourceEvent::Removed {
            session_id: "0a0a0a0a0a0a".into()
        }),
        "a remove names the row by its hostId — the id the seed listed it under"
    );
    // Another subscription's notification is not this one's.
    hub.roster("r-9", "epoch-1", json!([row_a()]), json!([]))
        .await;
    let quiet = drive_source(&mut rx, &mut checker, Duration::from_millis(200), |_| true)
        .await
        .unwrap();
    assert!(quiet.frames.is_empty(), "{:?}", quiet.frames);
}

fn session_of(e: &SourceEvent) -> Option<&LaneSession> {
    match e {
        SourceEvent::Session { session } => Some(session),
        _ => None,
    }
}

/// `omitted` and `slow_consumer` resubscribe on the SAME connection: a fresh
/// `Reset … Ready`, no `Offline`, no second dial.
#[tokio::test]
async fn a_roster_reset_resubscribes_on_the_same_connection() {
    for reason in ["omitted", "slow_consumer"] {
        let (dial, mut conns) = ScriptedDial::new();
        let (mut rx, _stop) = source(Arc::clone(&dial))
            .subscribe()
            .await
            .unwrap()
            .into_parts();
        let mut hub = next_conn(&mut conns).await;
        hub.hello("epoch-1", full_hub_capabilities()).await;
        hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;
        let mut checker = SourceChecker::new();
        drive_source(&mut rx, &mut checker, SCRIPT_WAIT, is_ready)
            .await
            .unwrap();
        hub.reset("r-1", reason).await;
        hub.subscribed("r-2", "epoch-1", json!([row_a(), row_b()]))
            .await;
        let reseed = drive_source(&mut rx, &mut checker, SCRIPT_WAIT, is_ready)
            .await
            .unwrap();
        assert!(
            matches!(&reseed.frames[0], SourceEvent::Reset { generation: 2, reason: r } if r.contains(reason)),
            "{reason}: {:?}",
            reseed.frames
        );
        assert!(!reseed
            .frames
            .iter()
            .any(|e| matches!(e, SourceEvent::Offline { .. })));
        assert_eq!(reseed.frames.iter().filter_map(session_of).count(), 2);
        assert_eq!(dial.dials(), 1, "{reason}: the same connection");
    }
}

/// `hub_closing`, and a bare EOF, are `Offline{Unreachable}` and a redial; the
/// new hub's epoch reseeds under a new generation.
#[tokio::test]
async fn hub_closing_or_eof_is_offline_and_a_redial_that_reseeds() {
    for how in ["hub_closing", "eof"] {
        let (dial, mut conns) = ScriptedDial::new();
        let (mut rx, _stop) = source(Arc::clone(&dial))
            .subscribe()
            .await
            .unwrap()
            .into_parts();
        let mut hub = next_conn(&mut conns).await;
        hub.hello("epoch-1", full_hub_capabilities()).await;
        hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;
        let mut checker = SourceChecker::new();
        drive_source(&mut rx, &mut checker, SCRIPT_WAIT, is_ready)
            .await
            .unwrap();
        if how == "hub_closing" {
            hub.reset("r-1", "hub_closing").await;
        }
        hub.close().await;
        let offline = drive_source(&mut rx, &mut checker, SCRIPT_WAIT, |e| {
            matches!(e, SourceEvent::Offline { .. })
        })
        .await
        .unwrap();
        assert!(matches!(
            offline.frames.last(),
            Some(SourceEvent::Offline {
                cause: SourceOffline::Unreachable,
                ..
            })
        ));
        let mut hub2 = next_conn(&mut conns).await;
        hub2.hello("epoch-2", full_hub_capabilities()).await;
        hub2.subscribed("r-1", "epoch-2", json!([row_a()])).await;
        let reseed = drive_source(&mut rx, &mut checker, SCRIPT_WAIT, is_ready)
            .await
            .unwrap();
        assert!(
            matches!(&reseed.frames[0], SourceEvent::Reset { generation: 2, .. }),
            "{how}: {:?}",
            reseed.frames
        );
        assert_eq!(
            reseed
                .frames
                .iter()
                .filter_map(session_of)
                .map(|s| s.id.as_str())
                .collect::<Vec<_>>(),
            ["0a0a0a0a0a0a"],
            "the same row id under the new epoch"
        );
        assert_eq!(dial.dials(), 2);
    }
}

/// Not installed and too old are said once and retried SLOWLY (the ceiling):
/// installing craze lights the machine up without a restart.
#[tokio::test(start_paused = true)]
async fn not_installed_and_too_old_are_offline_with_a_slow_retry() {
    let (dial, mut conns) = ScriptedDial::new();
    dial.fail_next(DialError::NotInstalled("craze: command not found".into()));
    dial.fail_next(DialError::NotInstalled("craze: command not found".into()));
    dial.fail_next(DialError::TooOld("unknown flag: --hub".into()));
    let timings = Timings {
        backoff_base: Duration::from_millis(500),
        backoff_max: Duration::from_secs(30),
        ..fast()
    };
    let src = CrazeSource::new(Arc::clone(&dial) as Arc<dyn CrazeDial>, "t").with_timings(timings);
    let (mut rx, _stop) = src.subscribe().await.unwrap().into_parts();
    let first = next(&mut rx, "not installed").await;
    assert!(
        matches!(
            &first,
            SourceEvent::Offline {
                cause: SourceOffline::NotInstalled,
                ..
            }
        ),
        "{first:?}"
    );
    assert_eq!(dial.dials(), 1);
    // The retry waits at least half the ceiling (jittered within [15 s, 30 s]).
    tokio::time::sleep(Duration::from_secs(14)).await;
    assert_eq!(dial.dials(), 1, "no retry before the slow floor");
    // The second dial's same cause and words said nothing new: the next frame
    // is the third dial's, too old.
    let too_old = tokio::time::timeout(Duration::from_secs(61), rx.recv())
        .await
        .expect("the third dial's Offline")
        .unwrap();
    assert_eq!(dial.dials(), 3);
    assert!(
        matches!(
            &too_old,
            SourceEvent::Offline {
                cause: SourceOffline::TooOld,
                ..
            }
        ),
        "{too_old:?}"
    );
    // Then craze is updated: the next slow retry seeds.
    let mut hub = tokio::time::timeout(Duration::from_secs(31), conns.recv())
        .await
        .expect("the next slow retry dialled")
        .unwrap();
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;
    let seeded = until(&mut rx, "the seed", is_ready).await;
    check_source(&[vec![first, too_old], seeded].concat()).expect("the whole stream conforms");
}

/// A hub too old to create says so in its capabilities — derived from its
/// `hello`, fixture 19's shape (`sessionCreate: false`, no `createOptions`) —
/// and `create_options` refuses without asking.
#[tokio::test]
async fn capabilities_are_the_hubs_hello() {
    let (dial, mut conns) = ScriptedDial::new();
    let (mut rx, _stop) = source(Arc::clone(&dial))
        .subscribe()
        .await
        .unwrap()
        .into_parts();
    let mut hub = next_conn(&mut conns).await;
    hub.hello(
        "epoch-1",
        json!({"rosterSubscribe": true, "sessionCreate": false, "multiplex": false, "connect": true,
               "snapshot": false, "attachWhenNow": false}),
    )
    .await;
    hub.subscribed("r-1", "epoch-1", json!([])).await;
    let seed = until(&mut rx, "the seed", is_ready).await;
    assert!(
        seed.iter()
            .any(|e| matches!(e, SourceEvent::Capabilities { capabilities }
        if !capabilities.create && !capabilities.create_options)),
        "{seed:?}"
    );

    let src = source(Arc::clone(&dial));
    let asked = tokio::spawn(async move { src.create_options().await });
    let mut hub2 = next_conn(&mut conns).await;
    hub2.hello(
        "epoch-1",
        json!({"rosterSubscribe": true, "sessionCreate": true, "connect": true}),
    )
    .await;
    let got = asked.await.unwrap();
    assert!(
        matches!(&got, Err(LaneError::Failed(m)) if m.contains("too old to offer providers")),
        "{got:?}"
    );
    assert!(
        hub2.recv().await.is_none(),
        "nothing was asked: the connection just closed"
    );
}

/// The channel's overflow (correction 13): the generation ends at the dropped
/// frame, the connection goes, the client drains, and a fresh connection
/// reseeds as `Reset{lagged}`.
#[tokio::test]
async fn an_overflow_drops_the_connection_and_reseeds_after_the_drain() {
    let (dial, mut conns) = ScriptedDial::new();
    let (mut rx, _stop) = source(Arc::clone(&dial))
        .subscribe()
        .await
        .unwrap()
        .into_parts();
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;
    // Fill the channel past its capacity without reading a frame.
    for _ in 0..(LANE_CHANNEL_CAPACITY / 64 + 2) {
        let upserts: Vec<Value> = (0..64).map(|_| row_b()).collect();
        hub.roster("r-1", "epoch-1", Value::Array(upserts), json!([]))
            .await;
    }
    // The pump drops the connection at the first dropped frame.
    assert!(hub.recv().await.is_none(), "the lagged connection closed");
    let mut frames = Vec::new();
    while frames.len() < LANE_CHANNEL_CAPACITY {
        frames.push(next(&mut rx, "the queued frames").await);
    }
    let mut hub2 = next_conn(&mut conns).await;
    hub2.hello("epoch-1", full_hub_capabilities()).await;
    hub2.subscribed("r-2", "epoch-1", json!([row_a()])).await;
    let reseed = until(&mut rx, "the reseed", is_ready).await;
    assert!(
        matches!(&reseed[0], SourceEvent::Reset { reason, generation: 2 } if reason == "lagged"),
        "{:?}",
        reseed[0]
    );
    check_source(&[frames, reseed].concat()).expect("the stream conforms across the overflow");
}

fn create_request(id: &str) -> LaneCreateRequest {
    LaneCreateRequest {
        cwd: "/w/new".into(),
        provider: Some("grok".into()),
        prompt: Some("hello".into()),
        request_id: id.into(),
    }
}

fn created_result() -> Value {
    json!({"session": roster_row("cccccccccccc", "session-created", "/w/new",
                                 json!({"activity": "working", "pendingAsks": 0})),
           "prompt": "accepted"})
}

/// A create sends `cwd`, `prompt`, `provider` and `requestId` — nothing else —
/// and answers the new row (its id the new hostId) and the prompt's fate.
#[tokio::test]
async fn a_create_sends_exactly_its_four_members() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let create = tokio::spawn(async move { src.create(create_request("req-1")).await });
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    assert_eq!(
        req["params"],
        json!({"cwd": "/w/new", "prompt": "hello", "provider": "grok", "requestId": "req-1"})
    );
    hub.reply(&req, created_result()).await;
    let got = create.await.unwrap().unwrap();
    assert_eq!(got.session.id, "cccccccccccc");
    assert_eq!(got.prompt, LanePromptOutcome::Accepted);
    assert_eq!(dial.dials(), 1);
}

/// A connection that drops after the create was written is an UNKNOWN
/// outcome: one retry, the SAME requestId, a fresh connection.
#[tokio::test]
async fn an_unknown_outcome_retries_once_under_the_same_request_id() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let create = tokio::spawn(async move { src.create(create_request("req-7")).await });
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    let first = hub.expect("session.create").await;
    hub.close().await;
    let mut hub2 = next_conn(&mut conns).await;
    hub2.hello("epoch-2", full_hub_capabilities()).await;
    let second = hub2.expect("session.create").await;
    assert_eq!(
        second["params"], first["params"],
        "the retry is the same create, requestId included"
    );
    hub2.reply(&second, created_result()).await;
    assert_eq!(create.await.unwrap().unwrap().session.id, "cccccccccccc");
    assert_eq!(dial.dials(), 2);
}

/// Two unknowns: `outcome unknown`, and the caller keeps the id.
#[tokio::test]
async fn two_unknowns_are_outcome_unknown() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let create = tokio::spawn(async move { src.create(create_request("req-8")).await });
    for epoch in ["epoch-1", "epoch-2"] {
        let mut hub = next_conn(&mut conns).await;
        hub.hello(epoch, full_hub_capabilities()).await;
        hub.expect("session.create").await;
        hub.close().await;
    }
    let got = create.await.unwrap().unwrap_err();
    assert!(crate::errors::is_outcome_unknown(&got), "{got:?}");
    assert_eq!(dial.dials(), 2, "one retry, no more");
}

/// The deadline is an unknown outcome too (paused clock: the real 120 s).
#[tokio::test(start_paused = true)]
async fn the_create_deadline_is_an_unknown_outcome() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = CrazeSource::new(Arc::clone(&dial) as Arc<dyn CrazeDial>, "t");
    let create = tokio::spawn(async move { src.create(create_request("req-9")).await });
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.expect("session.create").await;
    // No answer: the 120 s deadline passes, then the retry.
    let mut hub2 = tokio::time::timeout(CREATE_DEADLINE + Duration::from_secs(1), conns.recv())
        .await
        .expect("the retry dialled after the deadline")
        .unwrap();
    hub2.hello("epoch-1", full_hub_capabilities()).await;
    let again = hub2.expect("session.create").await;
    assert_eq!(again["params"]["requestId"], "req-9");
    hub2.reply(&again, created_result()).await;
    assert!(create.await.unwrap().is_ok());
    drop(hub);
}

/// Any DEFINITE answer ends the id's life — no retry, whatever the refusal.
#[tokio::test]
async fn a_definite_refusal_is_never_retried() {
    for (code, reason, extra, want) in [
        (
            "unavailable",
            "spawn_failed",
            json!({}),
            LaneError::Unavailable("unavailable/spawn_failed".into()),
        ),
        (
            "bad_request",
            "request_conflict",
            json!({}),
            LaneError::BadRequest("bad_request/request_conflict".into()),
        ),
        (
            "not_accepting",
            "start_failed",
            json!({"cause": "Error: KEYCHAIN LOCKED"}),
            LaneError::Failed("Error: KEYCHAIN LOCKED".into()),
        ),
    ] {
        let (dial, mut conns) = ScriptedDial::new();
        let src = source(Arc::clone(&dial));
        let create = tokio::spawn(async move { src.create(create_request("req-x")).await });
        let mut hub = next_conn(&mut conns).await;
        hub.hello("epoch-1", full_hub_capabilities()).await;
        let req = hub.expect("session.create").await;
        hub.refuse(&req, code, reason, extra).await;
        assert_eq!(create.await.unwrap().unwrap_err(), want, "{code}/{reason}");
        assert_eq!(dial.dials(), 1, "{code}/{reason}: no retry");
    }
}

/// A create whose dial fails never sent anything: a definite, quiet answer.
#[tokio::test]
async fn a_create_that_never_reached_craze_is_definite() {
    let (dial, _conns) = ScriptedDial::new();
    dial.fail_next(DialError::NotInstalled("craze: command not found".into()));
    let got = source(Arc::clone(&dial))
        .create(create_request("req-y"))
        .await
        .unwrap_err();
    assert!(
        matches!(&got, LaneError::Unavailable(m) if m.contains("not installed")),
        "{got:?}"
    );
    assert!(!crate::errors::is_outcome_unknown(&got));
    assert_eq!(dial.dials(), 1);
}

/// `createOptions`: craze's order, states, reasons, the default and the recent
/// directories (fixture 26's answer).
#[tokio::test]
async fn create_options_keep_crazes_order_and_words() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let asked = tokio::spawn(async move { src.create_options().await });
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    let req = hub.expect("sessions.createOptions").await;
    assert_eq!(req["params"], json!({}));
    hub.reply(&req, json!({"providers": [
        {"id": "cursor", "label": "cursor", "state": "unavailable", "reason": "cursor-agent not found on PATH", "fix": "install it"},
        {"id": "grok", "label": "grok", "state": "ready"},
        {"id": "native", "label": "native", "state": "needs_setup", "reason": "no key", "fix": "craze auth login"},
        {"id": "future", "label": "", "state": "warming_up", "reason": "r", "fix": "f"}],
        "defaultProvider": "grok", "recentDirs": [{"dir": "/w/a", "usedAt": "2026-10-03T12:00:00Z"}, {"dir": "/w/b"}]}))
        .await;
    let got = asked.await.unwrap().unwrap();
    let ids: Vec<&str> = got.providers.iter().map(|p| p.id.as_str()).collect();
    assert_eq!(ids, ["cursor", "grok", "native", "future"]);
    assert_eq!(got.providers[0].state, LaneProviderState::Unavailable);
    assert_eq!(
        got.providers[0].reason.as_deref(),
        Some("cursor-agent not found on PATH")
    );
    assert_eq!(got.providers[1].state, LaneProviderState::Ready);
    assert_eq!(got.providers[2].state, LaneProviderState::NeedsSetup);
    assert_eq!(
        got.providers[3].state,
        LaneProviderState::Other("warming_up".into())
    );
    assert_eq!(
        got.providers[3].label, "future",
        "an empty label shows the id"
    );
    assert_eq!(got.default_provider.as_deref(), Some("grok"));
    assert_eq!(got.recent_dirs, ["/w/a", "/w/b"]);
}

fn mapped(row: Value, status: &str, approximate: bool) -> LaneSession {
    let mut v = roster_row("0123456789ab", "s", "/home/me/projects/lumen", row);
    v["status"] = json!(status);
    v["approximate"] = json!(approximate);
    lane_session(&RosterRow::from_value(&v).unwrap())
}

/// The activity mapping (plan 025 §3.3.3), row by row.
#[test]
fn the_activity_mapping_row_by_row() {
    let act = |row: Value, status: &str| mapped(row, status, false).activity;
    let row = |activity: &str, foreign: bool, pending: u32| json!({"activity": activity, "foreignTurn": foreign, "pendingAsks": pending});
    assert_eq!(
        act(row("idle", false, 1), "reachable"),
        RcActivity::NeedsApproval,
        "the override"
    );
    assert_eq!(
        act(row("working", false, 2), "reachable"),
        RcActivity::NeedsApproval
    );
    assert_eq!(
        act(row("working", false, 0), "reachable"),
        RcActivity::Working
    );
    assert_eq!(
        act(row("idle", true, 0), "reachable"),
        RcActivity::Working,
        "a foreign turn reads idle"
    );
    assert_eq!(act(row("idle", false, 0), "reachable"), RcActivity::Idle);
    assert_eq!(
        act(row("starting", false, 0), "reachable"),
        RcActivity::Working
    );
    assert_eq!(
        act(row("replaying", false, 0), "reachable"),
        RcActivity::Working
    );
    assert_eq!(
        act(row("error", false, 0), "reachable"),
        RcActivity::Unknown
    );
    assert_eq!(
        act(row("closing", false, 0), "reachable"),
        RcActivity::Unknown
    );
    assert_eq!(
        act(row("brand_new", false, 0), "reachable"),
        RcActivity::Unknown
    );
    assert_eq!(
        act(row("idle", false, 0), "unreachable"),
        RcActivity::Unknown
    );
    assert_eq!(
        act(Value::Null, "connecting"),
        RcActivity::Unknown,
        "row-less"
    );
}

/// The rest of the row mapping.
#[test]
fn the_row_mapping() {
    let full = json!({
        "title": "fix the flake", "activity": "working", "foreignTurn": false, "pendingAsks": 1,
        "headAsk": {"id": "p", "kind": "permission", "label": "permission Run `git push`", "summary": "Run `git push`"},
        "doing": "Thinking", "lastReply": "The clock was the flake.", "since": "2026-01-01T00:03:00Z",
        "model": "grok", "attached": 2, "providerSessionId": "stub-session-1", "permissionMode": "prompt",
        "provider": {"name": "grok", "label": "grok"}, "newerMember": {"x": 1}});
    let s = mapped(full, "reachable", false);
    assert_eq!(s.id, "0123456789ab");
    assert_eq!(s.title, "fix the flake");
    assert_eq!(s.cwd, "/home/me/projects/lumen");
    assert_eq!(s.provider.as_deref(), Some("grok"));
    assert_eq!(s.model.as_deref(), Some("grok"));
    assert_eq!(s.doing.as_deref(), Some("Thinking"));
    assert_eq!(s.last_reply.as_deref(), Some("The clock was the flake."));
    assert_eq!(s.head_ask_summary.as_deref(), Some("Run `git push`"));
    assert_eq!(s.since_unix_ms, Some(1_767_225_780_000));
    assert_eq!(s.last_change_unix_ms, Some(1_767_225_780_000));
    assert_eq!(s.attached, Some(2));
    assert_eq!(s.provider_session_id.as_deref(), Some("stub-session-1"));
    assert_eq!(s.permission_mode.as_deref(), Some("prompt"));
    assert_eq!(s.pending_approvals, 1);
    assert!(!s.approximate);
    assert_eq!((s.parent_id, s.tab_id), (None, None));

    // The label stands in for a summary; startedAt for since; "" for None.
    let lean = mapped(
        json!({"activity": "idle", "pendingAsks": 1, "providerSessionId": "",
               "headAsk": {"id": "p", "kind": "permission", "label": "permission Shell"}}),
        "reachable",
        false,
    );
    assert_eq!(lean.head_ask_summary.as_deref(), Some("permission Shell"));
    assert_eq!(
        lean.last_change_unix_ms,
        Some(1_767_225_600_000),
        "startedAt"
    );
    assert_eq!(lean.since_unix_ms, None);
    assert_eq!(lean.provider_session_id, None);
    assert_eq!(lean.title, "lumen");

    // A failed start says why; without words, the fallback.
    let failed = mapped(
        json!({"activity": "error", "startFailed": true, "startErr": "no such binary"}),
        "reachable",
        false,
    );
    assert_eq!(failed.start_error.as_deref(), Some("no such binary"));
    let failed = mapped(
        json!({"activity": "error", "startFailed": true}),
        "reachable",
        false,
    );
    assert_eq!(
        failed.start_error.as_deref(),
        Some(crate::errors::START_FAILED_FALLBACK)
    );
    assert_eq!(
        mapped(json!({"startErr": "x"}), "reachable", false).start_error,
        None
    );

    // approximate: the hub's own, any status but reachable, or a row that
    // does not read (which keeps the host half).
    assert!(mapped(json!({}), "reachable", true).approximate);
    assert!(mapped(json!({}), "connecting", false).approximate);
    assert!(mapped(json!({}), "unreachable", false).approximate);
    let bad = mapped(json!({"pendingAsks": "many"}), "reachable", false);
    assert!(bad.approximate);
    assert_eq!(
        (bad.cwd.as_str(), bad.provider.as_deref()),
        ("/home/me/projects/lumen", Some("grok"))
    );
}

/// Subscribe, seed `row_a` under `epoch-1`, and hand back the frames so far,
/// the checker and the connection.
async fn seeded(
    conns: &mut tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
    rx: &mut Receiver<SourceEvent>,
    checker: &mut SourceChecker,
) -> HubEnd {
    let mut hub = next_conn(conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;
    drive_source(rx, checker, SCRIPT_WAIT, is_ready)
        .await
        .unwrap();
    hub
}

/// After a protocol fault: `Offline{Unreachable}` naming it, then a redial
/// whose fresh seed is a new generation.
async fn faulted_then_reseeded(
    conns: &mut tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
    rx: &mut Receiver<SourceEvent>,
    checker: &mut SourceChecker,
    says: &str,
) {
    let lost = drive_source(rx, checker, SCRIPT_WAIT, |e| {
        matches!(e, SourceEvent::Offline { .. })
    })
    .await
    .unwrap();
    assert!(
        matches!(lost.frames.last(), Some(SourceEvent::Offline { cause: SourceOffline::Unreachable, reason }) if reason.contains(says)),
        "{says}: {:?}",
        lost.frames
    );
    let before = checker.live_generation().unwrap_or(0);
    let mut hub = next_conn(conns).await;
    hub.hello("epoch-2", full_hub_capabilities()).await;
    hub.subscribed("r-1", "epoch-2", json!([row_a()])).await;
    let reseed = drive_source(rx, checker, SCRIPT_WAIT, is_ready)
        .await
        .unwrap();
    assert!(
        matches!(&reseed.frames[0], SourceEvent::Reset { generation, .. } if *generation == before + 1),
        "{says}: {:?}",
        reseed.frames
    );
}

/// A roster whose epoch is not the hub's that said hello — at the
/// subscription, or on a later notification — is a protocol fault: Offline,
/// a redial, a reseed. A notification that names no epoch is the
/// subscription's.
#[tokio::test]
async fn a_roster_from_another_epoch_is_a_fault_and_a_reseed() {
    // The subscription's epoch is not the hello's.
    let (dial, mut conns) = ScriptedDial::new();
    let (mut rx, _stop) = source(Arc::clone(&dial))
        .subscribe()
        .await
        .unwrap()
        .into_parts();
    let mut checker = SourceChecker::new();
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.subscribed("r-1", "epoch-9", json!([row_a()])).await;
    faulted_then_reseeded(&mut conns, &mut rx, &mut checker, "epoch").await;
    assert_eq!(
        checker.readies(),
        1,
        "the mismatched roster was never seeded"
    );

    // A notification's epoch is not the subscription's.
    let (dial, mut conns) = ScriptedDial::new();
    let (mut rx, _stop) = source(Arc::clone(&dial))
        .subscribe()
        .await
        .unwrap()
        .into_parts();
    let mut checker = SourceChecker::new();
    let mut hub = seeded(&mut conns, &mut rx, &mut checker).await;
    // No epoch at all: the subscription's, applied.
    hub.send(&json!({"jsonrpc": "2.0", "method": "roster",
                     "params": {"subscription": "r-1", "cursor": 2, "upserts": [row_b()], "removes": []}}))
        .await;
    drive_source(&mut rx, &mut checker, SCRIPT_WAIT, |e| {
        matches!(e, SourceEvent::Session { .. })
    })
    .await
    .unwrap();
    hub.roster("r-1", "epoch-9", json!([row_b()]), json!([]))
        .await;
    faulted_then_reseeded(&mut conns, &mut rx, &mut checker, "epoch").await;
}

/// A `roster` or `reset` that does not read, or a roster row with no key, is
/// a protocol fault — never a notification skipped.
#[tokio::test]
async fn a_malformed_roster_or_reset_is_a_fault_and_a_reseed() {
    for (what, bad) in [
        (
            "roster",
            json!({"jsonrpc": "2.0", "method": "roster",
                          "params": {"subscription": "r-1", "epoch": "epoch-1", "upserts": [], "removes": [42]}}),
        ),
        (
            "roster",
            json!({"jsonrpc": "2.0", "method": "roster",
                          "params": {"subscription": "r-1", "epoch": "epoch-1", "upserts": "none", "removes": []}}),
        ),
        (
            "reset",
            json!({"jsonrpc": "2.0", "method": "reset", "params": {"subscription": "r-1", "reason": 7}}),
        ),
        (
            "roster row",
            json!({"jsonrpc": "2.0", "method": "roster",
                          "params": {"subscription": "r-1", "epoch": "epoch-1",
                                     "upserts": [{"sessionId": "s", "status": "reachable"}], "removes": []}}),
        ),
    ] {
        let (dial, mut conns) = ScriptedDial::new();
        let (mut rx, _stop) = source(Arc::clone(&dial))
            .subscribe()
            .await
            .unwrap()
            .into_parts();
        let mut checker = SourceChecker::new();
        let mut hub = seeded(&mut conns, &mut rx, &mut checker).await;
        hub.send(&bad).await;
        faulted_then_reseeded(
            &mut conns,
            &mut rx,
            &mut checker,
            what.split(' ').next().unwrap(),
        )
        .await;
        assert_eq!(dial.dials(), 2, "{what}: redialled");
    }
}

/// A caller's request id must be craze's form — refused locally, before any
/// dial; the 64-character limit itself is fine.
#[tokio::test]
async fn a_request_id_not_in_crazes_form_is_refused_before_any_dial() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    for bad in [
        String::new(),
        "bad id".to_string(),
        "a".repeat(65),
        "naïve".to_string(),
        "x/y".to_string(),
    ] {
        let got = src.create(create_request(&bad)).await;
        assert!(
            matches!(&got, Err(LaneError::BadRequest(m)) if m.contains("request id")),
            "{bad:?}: {got:?}"
        );
    }
    assert_eq!(dial.dials(), 0, "nothing was dialled");
    let ok = "A-z_0.9".repeat(9) + "Z";
    assert_eq!(ok.len(), 64);
    let src = source(Arc::clone(&dial));
    let id = ok.clone();
    let create = tokio::spawn(async move { src.create(create_request(&id)).await });
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    assert_eq!(req["params"]["requestId"], ok.as_str());
    hub.reply(&req, created_result()).await;
    assert!(create.await.unwrap().is_ok());
}

/// The create's 120 s deadline covers WRITING it: craze stops reading
/// mid-prompt, the deadline passes — an unknown outcome — and the one retry
/// goes out under the same request id.
#[tokio::test(start_paused = true)]
async fn a_create_whose_write_stalls_is_an_unknown_outcome_and_retried() {
    let (dial, mut conns) = ScriptedDial::new();
    dial.set_capacity(1024);
    let src = CrazeSource::new(Arc::clone(&dial) as Arc<dyn CrazeDial>, "t");
    let mut request = create_request("req-stall");
    request.prompt = Some("p".repeat(64 << 10));
    let create = tokio::spawn(async move { src.create(request).await });
    let mut hub = next_conn(&mut conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    // ... and reads nothing more: the create's line cannot be written whole.
    dial.set_capacity(4 << 20);
    let mut hub2 = tokio::time::timeout(CREATE_DEADLINE + Duration::from_secs(1), conns.recv())
        .await
        .expect("the retry dialled once the deadline covered the stalled write")
        .unwrap();
    hub2.hello("epoch-1", full_hub_capabilities()).await;
    let again = hub2.expect("session.create").await;
    assert_eq!(again["params"]["requestId"], "req-stall");
    hub2.reply(&again, created_result()).await;
    assert!(create.await.unwrap().is_ok());
    assert_eq!(dial.dials(), 2);
    drop(hub);
}

// ---- the dial deadline (C7+C8 review) ----

/// A source on a [`HookDial`](crate::testing::HookDial) around `dial`, on
/// the pinned clocks (the paused-clock cells run them).
fn hooked(dial: &Arc<ScriptedDial>) -> (Arc<crate::testing::HookDial>, CrazeSource) {
    let hook = crate::testing::HookDial::new(Arc::clone(dial) as Arc<dyn CrazeDial>);
    let src = CrazeSource::new(Arc::clone(&hook) as Arc<dyn CrazeDial>, "t");
    (hook, src)
}

/// **A dial that never resolves is `Offline{Unreachable}` and a redial**, on
/// a paused clock: the held dial gives up at the dial deadline — never a
/// roster that waits without end and says nothing — and once dials go through
/// again the roster seeds.
#[tokio::test(start_paused = true)]
async fn a_held_dial_is_offline_and_redialled() {
    let (dial, mut conns) = ScriptedDial::new();
    let (hook, src) = hooked(&dial);
    hook.hold_dials();
    let started = tokio::time::Instant::now();
    let (mut rx, _stop) = src.subscribe().await.unwrap().into_parts();
    let first = tokio::time::timeout(DIAL_DEADLINE + Duration::from_secs(1), rx.recv())
        .await
        .expect("the held dial gave up at its deadline")
        .unwrap();
    assert!(
        matches!(&first, SourceEvent::Offline { cause: SourceOffline::Unreachable, reason }
            if reason.contains("handed back no connection")),
        "{first:?}"
    );
    assert!(
        started.elapsed() >= DIAL_DEADLINE,
        "{:?}",
        started.elapsed()
    );
    assert_eq!(dial.dials(), 0, "nothing got through");
    hook.release_dials();
    let mut hub = tokio::time::timeout(Duration::from_secs(120), conns.recv())
        .await
        .expect("the source redialled")
        .unwrap();
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.subscribed("sub-1", "epoch-1", json!([row_a()])).await;
    until(&mut rx, "the seed", is_ready).await;
}

/// **A create whose dial never resolves is an UNKNOWN outcome**: the one
/// retry goes out under the same `requestId` (nothing was written, so the
/// retry is safe), and a second held dial is `outcome unknown` — the caller
/// keeps the id. Never a create that waits without end.
#[tokio::test(start_paused = true)]
async fn a_held_dial_is_an_unknown_create_outcome_retried_under_the_same_id() {
    let (dial, mut conns) = ScriptedDial::new();
    let (hook, src) = hooked(&dial);
    hook.hold_dials();
    let create = tokio::spawn(async move { src.create(create_request("req-held")).await });
    // The first dial gives up at 30 s; the retry is dialling (held) when the
    // dials are let through.
    tokio::time::sleep(DIAL_DEADLINE + Duration::from_secs(1)).await;
    assert_eq!(
        hook.dials(),
        2,
        "the first held dial gave up, the retry is dialling"
    );
    hook.release_dials();
    let mut hub = tokio::time::timeout(Duration::from_secs(60), conns.recv())
        .await
        .expect("the retry got through")
        .unwrap();
    hub.hello("epoch-1", full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    assert_eq!(req["params"]["requestId"], "req-held", "the same id");
    hub.reply(&req, created_result()).await;
    assert!(create.await.unwrap().is_ok());

    let (dial, _conns) = ScriptedDial::new();
    let (hook, src) = hooked(&dial);
    hook.hold_dials();
    let got = tokio::time::timeout(
        DIAL_DEADLINE * 2 + Duration::from_secs(1),
        src.create(create_request("req-held-2")),
    )
    .await
    .expect("two held dials end the create")
    .unwrap_err();
    assert!(crate::errors::is_outcome_unknown(&got), "{got:?}");
    assert_eq!(hook.dials(), 2, "one retry, no more");
}

/// **`createOptions` whose dial never resolves fails** — `Unavailable` at the
/// dial deadline, a read with nothing to guess about.
#[tokio::test(start_paused = true)]
async fn a_held_dial_fails_create_options() {
    let (dial, _conns) = ScriptedDial::new();
    let (hook, src) = hooked(&dial);
    hook.hold_dials();
    let got = tokio::time::timeout(DIAL_DEADLINE + Duration::from_secs(1), src.create_options())
        .await
        .expect("the held dial ended the call at its deadline")
        .unwrap_err();
    assert!(
        matches!(&got, LaneError::Unavailable(m) if m.contains("handed back no connection")),
        "{got:?}"
    );
    assert!(!crate::errors::is_outcome_unknown(&got));
}
