//! `CrazeSource` against a SCRIPTED hub (`testing::ScriptedDial`): the cells
//! that need a hub to say exactly one thing at exactly one moment — a roster
//! `omitted` reset (a real one needs 513 hosts), an epoch change, an overflow,
//! a create whose connection drops at the worst instant. The recipe cells
//! (`tests/recipe_source.rs`) run the same paths against the real hub.

use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::conformance::{check_source, drive_source, SourceChecker};
use shed_core::lane::{
    AgentSource, LaneCreateRequest, LaneError, LaneEvent, LanePromptOutcome, LaneProviderState,
    SourceEvent, SourceOffline, LANE_CHANNEL_CAPACITY,
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

/// One create answered `created_result()`'s row, on `src`, through the next
/// scripted connection.
async fn create_answered(
    src: &CrazeSource,
    conns: &mut tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
    id: &str,
) -> LaneCreated {
    let src = src.clone();
    let id = id.to_string();
    let create = tokio::spawn(async move { src.create(create_request(&id)).await });
    let mut hub = next_conn(conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    let req = hub.expect("session.create").await;
    hub.reply(&req, created_result()).await;
    create.await.unwrap().unwrap()
}

/// **A created session is openable at once, and stays so until a roster
/// speaks for it** (plan 025 §3.6.4, the C9 hand-off and the C10 review): the
/// create's own row is kept — through a clone that dials elsewhere too
/// ([`CrazeSource::dialling`]) — so `open(hostId)` binds a lane whose
/// `session()` answers with no roster at all; a roster seed that does NOT list
/// it yet keeps it (its answer can predate the new host); a roster that lists
/// it is the one from then on (its row is the fresher, and never overwritten);
/// a roster `Removed` lets it go for good — a replay of the same create's
/// answer (craze replays one for ten minutes) does not bring it back, nor does
/// one for a hostId an epoch reseed dropped.
#[tokio::test]
async fn a_created_row_is_kept_until_a_roster_speaks_for_it() {
    const NEW: &str = "cccccccccccc";
    let (roster_dial, mut roster_conns) = ScriptedDial::new();
    let (ask_dial, mut ask_conns) = ScriptedDial::new();
    let src = source(Arc::clone(&roster_dial));
    let ask = src.dialling(Arc::clone(&ask_dial) as Arc<dyn CrazeDial>);

    // No subscription anywhere: nothing has listed the new session.
    assert!(src.listed(NEW).is_none());
    let created = create_answered(&ask, &mut ask_conns, "req-open").await;
    assert_eq!(created.session.id, NEW);
    assert_eq!(
        roster_dial.dials(),
        0,
        "the create went through the clone's dial"
    );
    let (row, session_id) = src
        .listed(NEW)
        .expect("the create's row is the source's at once");
    assert_eq!(
        (row.id.as_str(), session_id.as_str()),
        (NEW, "session-created")
    );
    assert_eq!(
        src.created_rows()
            .iter()
            .map(|r| r.id.as_str())
            .collect::<Vec<_>>(),
        [NEW],
        "the authority a client reads its created rows from"
    );
    assert!(src.roster_row(NEW).is_none(), "no roster has listed it");
    let lane = src.open(NEW).await.unwrap();
    assert_eq!(
        lane.session()
            .await
            .expect("a lane on a just-created session knows its row")
            .id,
        NEW
    );
    assert_eq!(
        roster_dial.dials() + ask_dial.dials(),
        1,
        "session() never dials"
    );

    // A seed whose answer does not list it yet (it predates the new host):
    // the created row is KEPT — a lane opened now still knows it.
    let (mut rx, _stop) = src.subscribe().await.unwrap().into_parts();
    let mut hub = next_conn(&mut roster_conns).await;
    hub.hello("epoch-1", full_hub_capabilities()).await;
    hub.subscribed("r-1", "epoch-1", json!([row_a()])).await;
    until(&mut rx, "the seed", is_ready).await;
    assert!(
        src.listed(NEW).is_some(),
        "a seed that does not list it yet keeps the create's row"
    );
    let lane = src.open(NEW).await.unwrap();
    assert!(
        lane.session().await.is_ok(),
        "still openable after the seed"
    );

    // The roster lists it, with its own (fresher) row: that row is the one,
    // and a create's answer arriving after never overwrites it.
    let fresher = roster_row(
        NEW,
        "session-created",
        "/w/new",
        json!({"title": "the roster's", "activity": "idle", "pendingAsks": 0}),
    );
    hub.roster("r-1", "epoch-1", json!([fresher]), json!([]))
        .await;
    until(&mut rx, "the upsert", |e| {
        matches!(e, SourceEvent::Session { .. })
    })
    .await;
    assert_eq!(src.listed(NEW).unwrap().0.title, "the roster's");
    assert!(
        src.created_rows().is_empty(),
        "the roster speaks for it now"
    );
    assert_eq!(src.roster_row(NEW).unwrap().title, "the roster's");
    create_answered(&ask, &mut ask_conns, "req-open").await;
    assert_eq!(src.listed(NEW).unwrap().0.title, "the roster's");

    // The roster removes it: gone, and a REPLAY of the same create's answer
    // (same requestId, craze's ten-minute replay) does not bring it back.
    hub.roster("r-1", "epoch-1", json!([]), json!([NEW])).await;
    until(&mut rx, "the remove", |e| {
        matches!(e, SourceEvent::Removed { .. })
    })
    .await;
    assert!(src.listed(NEW).is_none(), "a Removed lets it go");
    create_answered(&ask, &mut ask_conns, "req-open").await;
    assert!(
        src.listed(NEW).is_none(),
        "a replayed create never resurrects a removed session's row"
    );
    assert!(src.created_rows().is_empty(), "nor offers it to a client");

    // An epoch reseed that no longer lists row A (no `Removed` for it) lets
    // it go too: a create answer naming it is not kept.
    hub.reset("r-1", "omitted").await;
    let resub = hub.expect("sessions.subscribe").await;
    hub.reply(
        &resub,
        json!({"subscription": "r-2", "epoch": "epoch-1", "cursor": 9, "sessions": []}),
    )
    .await;
    until(&mut rx, "the reseed", is_ready).await;
    assert!(src.listed("0a0a0a0a0a0a").is_none());
    let replay_a = json!({"session": row_a(), "prompt": "none"});
    {
        let ask = ask.clone();
        let create = tokio::spawn(async move { ask.create(create_request("req-a")).await });
        let mut hub = next_conn(&mut ask_conns).await;
        hub.hello("epoch-1", full_hub_capabilities()).await;
        let req = hub.expect("session.create").await;
        hub.reply(&req, replay_a).await;
        create.await.unwrap().unwrap();
    }
    assert!(
        src.listed("0a0a0a0a0a0a").is_none(),
        "a hostId a reseed dropped is not resurrected by a create's answer"
    );
}

/// A row with this hostId, for [`Held`]'s own cells.
fn session_row(host_id: &str) -> LaneSession {
    LaneSession {
        id: host_id.to_string(),
        title: host_id.to_string(),
        cwd: "/w".to_string(),
        ..LaneSession::default()
    }
}

/// **The created rows are bounded** (C10 review): at most [`CREATED_KEEP`],
/// the oldest out first, and none outlives [`CREATED_TTL`] — so creates on a
/// source with no roster subscription cannot pile up.
#[tokio::test(start_paused = true)]
async fn created_rows_are_bounded_and_expire() {
    let mut held = Held::default();
    for i in 0..=CREATED_KEEP {
        held.remember_created(&session_row(&format!("{i:012x}")), "s");
        tokio::time::advance(Duration::from_millis(1)).await;
    }
    assert_eq!(held.created.len(), CREATED_KEEP, "bounded");
    assert!(
        held.listed(&format!("{:012x}", 0)).is_none(),
        "the oldest went first"
    );
    assert!(held.listed(&format!("{CREATED_KEEP:012x}")).is_some());

    tokio::time::advance(CREATED_TTL).await;
    assert!(
        held.listed(&format!("{CREATED_KEEP:012x}")).is_none(),
        "expired with craze's replay window"
    );
    held.remember_created(&session_row("ffffffffffff"), "s");
    assert_eq!(held.created.len(), 1, "a remember prunes the expired");

    // The tombstones' memory backstop.
    for i in 0..TOMBSTONES + 10 {
        held.let_go(&format!("{i:012x}"));
    }
    assert_eq!(held.gone.len(), TOMBSTONES);
}

/// **`created_rows` is evaluated at the call** (C10 confirmation): a created
/// row offered to a client goes once craze's replay window has passed, with no
/// roster frame and no seed in between — the client reads it there rather than
/// keeping a copy that would outlive it.
#[tokio::test(start_paused = true)]
async fn created_rows_expire_at_the_read() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let created = create_answered(&src, &mut conns, "req-ttl").await;
    let listed: Vec<String> = src.created_rows().into_iter().map(|r| r.id).collect();
    assert_eq!(listed, std::slice::from_ref(&created.session.id));
    tokio::time::advance(CREATED_TTL).await;
    assert!(src.created_rows().is_empty(), "expired at the read");
    assert!(src.listed(&created.session.id).is_none());
}

/// **A tombstone lasts craze's whole replay window** (C10 confirmation): a
/// session let go, then 300 other hosts let go within the ten minutes — a
/// replayed create for the first is still refused; once the window has
/// passed (craze no longer replays it), its id is free again.
#[tokio::test(start_paused = true)]
async fn a_tombstone_outlives_many_other_removals_within_the_replay_window() {
    const A: &str = "aaaaaaaaaaaa";
    let mut held = Held::default();
    held.let_go(A);
    for i in 0..300 {
        held.let_go(&format!("{i:012x}"));
        tokio::time::advance(Duration::from_secs(1)).await;
    }
    held.remember_created(&session_row(A), "s");
    assert!(
        held.listed(A).is_none(),
        "a replayed create for A is refused, 300 removals and five minutes later"
    );
    tokio::time::advance(CREATED_TTL).await;
    held.remember_created(&session_row(A), "s");
    assert!(
        held.listed(A).is_some(),
        "past the replay window the id is free"
    );
}

/// The hostId [`created_result`] answers with.
const CREATED: &str = "cccccccccccc";

/// The host [`CREATED`]'s info document — a `craze serve`'s, so it can stop.
fn created_info() -> Value {
    crate::testing::session_info(
        CREATED,
        "session-created",
        "INC-1",
        crate::testing::session_caps(true),
    )
}

/// A session created through `src` (no roster lists it: `src` has no
/// subscription), a lane opened on its row through `src` and subscribed, and
/// craze's end of that lane's first connection, nothing on it read yet.
async fn created_lane(
    src: &CrazeSource,
    conns: &mut tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
    request_id: &str,
) -> (Receiver<LaneEvent>, LaneStop, Arc<dyn AgentLane>, HubEnd) {
    create_answered(src, conns, request_id).await;
    assert_eq!(
        ids(&src.created_rows()),
        [CREATED],
        "the create's row, and no roster's"
    );
    let lane = src.open(CREATED).await.unwrap();
    let (rx, stop) = lane.subscribe(None).await.unwrap().into_parts();
    let hub = next_conn(conns).await;
    (rx, stop, lane, hub)
}

fn ids(rows: &[LaneSession]) -> Vec<&str> {
    rows.iter().map(|r| r.id.as_str()).collect()
}

/// The created lane's connection seeded — the splice, the host's row, a
/// no-cursor attach — through its `Ready`.
async fn seeded_created(hub: &mut HubEnd, rx: &mut Receiver<LaneEvent>) {
    seeded_created_as(hub, rx, "s-1").await;
}

/// [`seeded_created`], the attachment's subscription named `sub`.
async fn seeded_created_as(hub: &mut HubEnd, rx: &mut Receiver<LaneEvent>, sub: &str) {
    hub.splice(CREATED).await;
    hub.listed(crate::testing::host_session_row(&created_info(), json!({})))
        .await;
    hub.attached(crate::testing::attach_result(
        sub,
        &created_info(),
        ("INC-1", 1),
        Some(crate::testing::snapshot_at("INC-1", 1, json!({}))),
        None,
    ))
    .await;
    hub.synchronized(sub, 1).await;
    loop {
        let ev = tokio::time::timeout(SCRIPT_WAIT, rx.recv())
            .await
            .expect("the seed in time")
            .expect("the lane is alive");
        if matches!(ev, LaneEvent::Ready { .. }) {
            return;
        }
    }
}

/// `lane.stop()` on `hub`, receipted.
async fn stop_receipted(lane: &Arc<dyn AgentLane>, hub: &mut HubEnd) {
    let stopping = {
        let lane = Arc::clone(lane);
        tokio::spawn(async move { lane.stop().await })
    };
    let req = hub.expect("session.stop").await;
    hub.reply(&req, json!({})).await;
    stopping.await.unwrap().expect("the host's receipt");
}

/// Wait until `lane` runs exactly `n` watchers.
async fn until_watching(lane: &CrazeLane, n: usize) {
    for _ in 0..500 {
        if lane.watching() == n {
            return;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert_eq!(lane.watching(), n, "the lane's running watchers");
}

/// A created session's lane with TWO subscriptions, each seeded on its own
/// connection (`s-a`, then `s-b` — whose attachment the verbs then use), and
/// the host's receipt for a stop taken on `s-b`'s.
async fn two_subscriptions_stopped(
    src: &CrazeSource,
    conns: &mut tokio::sync::mpsc::UnboundedReceiver<HubEnd>,
) -> (
    Arc<CrazeLane>,
    (Receiver<LaneEvent>, LaneStop, HubEnd),
    (Receiver<LaneEvent>, LaneStop, HubEnd),
) {
    create_answered(src, conns, "req-two").await;
    let lane = Arc::new(src.open_lane(CREATED));
    let (mut rx_a, stop_a) = lane.subscribe(None).await.unwrap().into_parts();
    let mut hub_a = next_conn(conns).await;
    seeded_created_as(&mut hub_a, &mut rx_a, "s-a").await;
    let (mut rx_b, stop_b) = lane.subscribe(None).await.unwrap().into_parts();
    let mut hub_b = next_conn(conns).await;
    seeded_created_as(&mut hub_b, &mut rx_b, "s-b").await;
    assert_eq!(lane.watching(), 2);
    let as_dyn: Arc<dyn AgentLane> = Arc::clone(&lane) as Arc<dyn AgentLane>;
    stop_receipted(&as_dyn, &mut hub_b).await;
    (lane, (rx_a, stop_a, hub_a), (rx_b, stop_b, hub_b))
}

/// A counter of [`CrazeSource::on_created_gone`]'s calls on `src`.
fn count_created_gone(src: &CrazeSource) -> Arc<std::sync::atomic::AtomicUsize> {
    let gone = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let counter = Arc::clone(&gone);
    src.on_created_gone(move || {
        counter.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    });
    gone
}

fn calls(gone: &std::sync::atomic::AtomicUsize) -> usize {
    gone.load(std::sync::atomic::Ordering::SeqCst)
}

/// The lane's frames up to and including its `Down`; that `Down`'s reason.
async fn lane_down(rx: &mut Receiver<LaneEvent>) -> String {
    loop {
        let ev = tokio::time::timeout(SCRIPT_WAIT, rx.recv())
            .await
            .expect("the lane's Down in time")
            .expect("the lane's stream ends with its Down");
        if let LaneEvent::Down { reason } = ev {
            return reason;
        }
    }
}

/// **A created session that ends before any roster listed it leaves the
/// source for good** (live leg 1's ghost row, re-run on 82eabbd: a session
/// the create sheet started and `lane.stop` ended 0.4 s later stayed listed
/// for its whole ten minutes). craze's roster sends a remove only for a host
/// it SENT — one that came and went between two flushes gets neither
/// (`internal/hub/roster.go`'s `take`) — so no `Removed` ever comes for it,
/// and the lane is the one that sees the end. Its stop's RECEIPT lets nothing
/// go (the session is closing, its closing records still to come); its
/// `Down{"session_closed"}` drops the created row and tombstones the hostId,
/// as a roster `Removed` would: a replay of the same create's answer (craze
/// replays one for ten minutes) does not bring it back.
#[tokio::test]
async fn a_created_session_stopped_before_any_roster_lists_it_is_let_go() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let gone = count_created_gone(&src);
    let (mut rx, _stop, lane, mut hub) = created_lane(&src, &mut conns, "req-ghost").await;
    seeded_created(&mut hub, &mut rx).await;

    stop_receipted(&lane, &mut hub).await;
    assert_eq!(
        ids(&src.created_rows()),
        [CREATED],
        "the receipt is not the end: the row stays while the session closes"
    );
    assert_eq!(calls(&gone), 0);

    hub.reset("s-1", "session_closed").await;
    assert_eq!(lane_down(&mut rx).await, "session_closed");
    assert!(
        src.created_rows().is_empty(),
        "the ended session's created row is gone: {:?}",
        ids(&src.created_rows())
    );
    assert!(src.listed(CREATED).is_none(), "and nothing opens on it");
    assert_eq!(
        calls(&gone),
        1,
        "the client is told, before the Down: no roster frame will say it"
    );
    assert!(
        lane.session().await.is_ok(),
        "the lane itself keeps the row it knew"
    );

    // craze replays the create's stored answer under the same request id.
    create_answered(&src, &mut conns, "req-ghost").await;
    assert!(
        src.created_rows().is_empty(),
        "a replayed create never resurrects an ended session's row"
    );
    assert!(src.listed(CREATED).is_none());
    assert_eq!(calls(&gone), 1, "said once");
}

/// **After a stop the host took, the lane's end is the session's, however it
/// comes** (`crate::lane`'s "The session's end"): the receipt is craze's word
/// that the close follows (PM "`session.stop`"), so a subscription let go
/// before the close reached it — a panel closed at once — still lets the
/// created row go, and the client's hook is told.
#[tokio::test]
async fn a_subscription_let_go_after_a_stop_receipt_still_lets_the_row_go() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let gone = count_created_gone(&src);
    let (mut rx, stop, lane, mut hub) = created_lane(&src, &mut conns, "req-let-go").await;
    seeded_created(&mut hub, &mut rx).await;
    stop_receipted(&lane, &mut hub).await;
    assert_eq!(ids(&src.created_rows()), [CREATED], "closing, not closed");
    drop(stop);
    drop(rx);
    for _ in 0..500 {
        if src.created_rows().is_empty() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(
        src.created_rows().is_empty(),
        "a subscription let go after the receipt still says the session's end"
    );
    assert_eq!(calls(&gone), 1);
}

/// **After a stop the host took, any terminal `Down` is the session's end** —
/// even one that on its own says only that this lane cannot go on: here the
/// connection drops before the close, and the redial reaches another host
/// (`protocol: …`). Without the receipt that `Down` keeps the row
/// (`only_the_sessions_own_end_lets_its_created_row_go`).
#[tokio::test]
async fn after_a_stop_receipt_any_terminal_down_lets_the_row_go() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let gone = count_created_gone(&src);
    let (mut rx, _stop, lane, mut hub) = created_lane(&src, &mut conns, "req-fault").await;
    seeded_created(&mut hub, &mut rx).await;
    stop_receipted(&lane, &mut hub).await;
    hub.close().await;
    let mut again = next_conn(&mut conns).await;
    again.splice_answered_by(CREATED, "dddddddddddd").await;
    assert!(lane_down(&mut rx).await.starts_with("protocol: "));
    assert!(
        src.created_rows().is_empty(),
        "after the receipt, any end of the lane is the session's"
    );
    assert_eq!(calls(&gone), 1);
}

/// **A stop receipted after its subscription was let go still says the
/// session's end** (the ghost-row review's race): the stop request goes out
/// on the lane's connection, the subscription is dropped — the watcher and its
/// guard gone, the stop's receipt still held by the host — and only THEN does
/// the receipt arrive. No watcher is left to see the close, so the receipt
/// itself says it: the created row goes, and the client's hook is told once.
#[tokio::test]
async fn a_stop_receipted_after_its_subscription_was_let_go_lets_the_row_go() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let gone = count_created_gone(&src);
    create_answered(&src, &mut conns, "req-race").await;
    let lane = Arc::new(src.open_lane(CREATED));
    let (mut rx, stop) = lane.subscribe(None).await.unwrap().into_parts();
    let mut hub = next_conn(&mut conns).await;
    seeded_created(&mut hub, &mut rx).await;

    let stopping = {
        let lane = Arc::clone(&lane);
        tokio::spawn(async move { lane.stop().await })
    };
    // Sent — and its receipt held.
    let req = hub.expect("session.stop").await;
    drop(stop);
    drop(rx);
    for _ in 0..500 {
        if lane.watching() == 0 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert_eq!(
        lane.watching(),
        0,
        "the subscription and its watcher are gone"
    );
    assert_eq!(
        ids(&src.created_rows()),
        [CREATED],
        "nothing has said the end: no receipt yet"
    );
    assert_eq!(calls(&gone), 0);

    hub.reply(&req, json!({})).await;
    stopping
        .await
        .unwrap()
        .expect("the receipt reaches the stop");
    assert!(
        src.created_rows().is_empty(),
        "with no watcher left, the receipt says the session's end"
    );
    assert!(src.listed(CREATED).is_none());
    assert_eq!(calls(&gone), 1, "told once");
}

/// **The session's end is said BEFORE the `Down`** (the ghost-row review):
/// a client that re-reads its listing on the `Down` must already find the
/// created row gone. The client's hook here BLOCKS until the test releases it
/// (bounded, so a broken order fails the cell rather than hanging it); while
/// it blocks, the row is already gone and no `Down` is observable — and once
/// it is released, the `Down` arrives.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_sessions_end_is_said_before_the_down() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let (entered_tx, entered) = std::sync::mpsc::channel::<()>();
    let (release, release_rx) = std::sync::mpsc::channel::<()>();
    let entered_tx = Mutex::new(entered_tx);
    let release_rx = Mutex::new(release_rx);
    src.on_created_gone(move || {
        let _ = lock(&entered_tx).send(());
        let _ = lock(&release_rx).recv_timeout(Duration::from_secs(5));
    });
    let (mut rx, _stop, _lane, mut hub) = created_lane(&src, &mut conns, "req-order").await;
    seeded_created(&mut hub, &mut rx).await;

    hub.reset("s-1", "session_closed").await;
    let mut inside = false;
    for _ in 0..1000 {
        if entered.try_recv().is_ok() {
            inside = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    assert!(inside, "the hook was called");
    assert!(
        src.created_rows().is_empty(),
        "the row is gone before the hook is told"
    );
    let early = tokio::time::timeout(Duration::from_millis(200), async {
        while let Some(ev) = rx.recv().await {
            if matches!(ev, LaneEvent::Down { .. }) {
                return true;
            }
        }
        false
    })
    .await;
    assert!(
        early.is_err(),
        "no Down while the session's end is still being said: {early:?}"
    );

    release.send(()).unwrap();
    assert_eq!(lane_down(&mut rx).await, "session_closed");
}

/// **Only the lane's LAST watcher's end is the session's** (the confirmation
/// review): with two subscriptions on one lane and the host's stop receipt
/// taken, letting ONE go leaves the row — the other is still live, its closing
/// records still to come — and the hook untold. The other's
/// `Down{"session_closed"}` then lets the row go, and the hook is told exactly
/// once, though that watcher's own end says it again.
#[tokio::test]
async fn after_a_stop_only_the_last_subscriptions_end_lets_the_row_go() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let gone = count_created_gone(&src);
    let (lane, (rx_a, stop_a, _hub_a), (mut rx_b, stop_b, mut hub_b)) =
        two_subscriptions_stopped(&src, &mut conns).await;

    drop(stop_a);
    drop(rx_a);
    until_watching(&lane, 1).await;
    assert_eq!(
        ids(&src.created_rows()),
        [CREATED],
        "another subscription is still live: the close is its to see"
    );
    assert_eq!(calls(&gone), 0);

    hub_b.reset("s-b", "session_closed").await;
    assert_eq!(lane_down(&mut rx_b).await, "session_closed");
    assert!(src.created_rows().is_empty(), "the close lets the row go");
    drop(stop_b);
    drop(rx_b);
    until_watching(&lane, 0).await;
    assert_eq!(calls(&gone), 1, "told exactly once");
}

/// **A lane fault after a stop leaves the row while another watcher is live**
/// — the same rule for a `Down` that says nothing of the session: after the
/// receipt, `s-a`'s connection drops and its redial reaches another host
/// (`protocol: …`), while `s-b` still follows the closing session. The row
/// stays; only once `s-b`, the last, is let go does the receipt's word end it.
#[tokio::test]
async fn after_a_stop_a_lane_fault_with_another_watcher_live_keeps_the_row() {
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let gone = count_created_gone(&src);
    let (lane, (mut rx_a, _stop_a, hub_a), (rx_b, stop_b, _hub_b)) =
        two_subscriptions_stopped(&src, &mut conns).await;

    hub_a.close().await;
    let mut again = next_conn(&mut conns).await;
    again.splice_answered_by(CREATED, "dddddddddddd").await;
    assert!(lane_down(&mut rx_a).await.starts_with("protocol: "));
    until_watching(&lane, 1).await;
    assert_eq!(
        ids(&src.created_rows()),
        [CREATED],
        "s-b still follows the close: s-a's fault is not the session's end"
    );
    assert_eq!(calls(&gone), 0);

    drop(stop_b);
    drop(rx_b);
    until_watching(&lane, 0).await;
    assert!(
        src.created_rows().is_empty(),
        "the last watcher gone after the receipt: the session's end"
    );
    assert_eq!(calls(&gone), 1);
}

/// **Only the session's own end lets it go** — every terminal `Down` that
/// says the SESSION is over (`unknown_session` from the hub's splice,
/// `start_failed: <cause>` from the attach) drops the created row, through a
/// clone that dials elsewhere too ([`CrazeSource::dialling`]); a terminal
/// `Down` that says only that THIS lane cannot go on (a splice to another
/// host, `protocol: …`) keeps it: the session may well be running.
#[tokio::test]
async fn only_the_sessions_own_end_lets_its_created_row_go() {
    // unknown_session: the hub knows no such host.
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let (mut rx, _stop, _lane, mut hub) = created_lane(&src, &mut conns, "req-unknown").await;
    hub.hello("0a1b2c3d4e5f", full_hub_capabilities()).await;
    let connect = hub.expect("session.connect").await;
    hub.refuse(&connect, "unknown_session", "unknown_session", json!({}))
        .await;
    assert_eq!(lane_down(&mut rx).await, "unknown_session");
    assert!(src.created_rows().is_empty(), "unknown_session ends it");

    // start_failed: the session never started — through a clone of the
    // source that dials elsewhere, sharing its rows.
    let (roster_dial, _roster_conns) = ScriptedDial::new();
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(roster_dial);
    let ask = src.dialling(Arc::clone(&dial) as Arc<dyn CrazeDial>);
    let (mut rx, _stop, _lane, mut hub) = created_lane(&ask, &mut conns, "req-failed").await;
    hub.splice(CREATED).await;
    hub.listed(crate::testing::host_session_row(&created_info(), json!({})))
        .await;
    let attach = hub.expect("session.attach").await;
    hub.refuse(
        &attach,
        "not_accepting",
        "start_failed",
        json!({"cause": "KEYCHAIN LOCKED"}),
    )
    .await;
    assert_eq!(lane_down(&mut rx).await, "start_failed: KEYCHAIN LOCKED");
    assert!(
        src.created_rows().is_empty() && ask.created_rows().is_empty(),
        "start_failed ends it, for every clone"
    );

    // protocol: another host answered — this lane's end, not the session's.
    let (dial, mut conns) = ScriptedDial::new();
    let src = source(Arc::clone(&dial));
    let (mut rx, _stop, _lane, mut hub) = created_lane(&src, &mut conns, "req-fault").await;
    hub.splice_answered_by(CREATED, "dddddddddddd").await;
    assert!(lane_down(&mut rx).await.starts_with("protocol: "));
    assert_eq!(
        ids(&src.created_rows()),
        [CREATED],
        "a lane's own fault says nothing of the session"
    );
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
