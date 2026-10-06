//! `CrazeLane` against the REAL hub — craze's hermetic recipe
//! (`shed_craze::testing::Recipe`): sessions served by `craze-fake-host`
//! registry entries (driven through their stdin ops) and by hosts the hub
//! creates (`craze serve` running `craze-fake-agent`), every craze process
//! under the recipe's six variables (plan 025 §3.3.8, the lane half).
//!
//! Every cell skips with a message without `SHED_CRAZE_BIN_DIR` and FAILS
//! instead under `SHED_CRAZE_REQUIRE=1` (CI). Every frame a lane emits here
//! goes through the contract's conformance kit; every lane is opened through
//! a subscribed source, as both clients open them.

use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use shed_core::lane::conformance::{drive_lane, Drive, DriveEnd, LaneChecker};
use shed_core::lane::{
    AgentLane, AgentSource, LaneAnswer, LaneApprovalKind, LaneCreateRequest, LaneError, LaneEvent,
    SendMode, SourceEvent, LANE_CHANNEL_CAPACITY,
};
use shed_core::rc::RcActivity;
use shed_craze::testing::{bins, HookDial, Recipe, TeeDial, RECIPE_WAIT};
use shed_craze::{new_request_id, CrazeDial, CrazeSource};
use tokio::sync::mpsc::Receiver;

const FAKE: &str = "0c0c0c0c0c0c";
const FAKE_SESSION: &str = "recipe-lane";

async fn drive(
    rx: &mut Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    what: &str,
    until: impl FnMut(&LaneEvent) -> bool,
) -> Vec<LaneEvent> {
    let d: Drive<LaneEvent> = drive_lane(rx, checker, RECIPE_WAIT, until)
        .await
        .unwrap_or_else(|v| panic!("{what}: the stream broke a contract rule: {v}"));
    assert_eq!(
        d.end,
        DriveEnd::Matched,
        "{what}: not seen within {RECIPE_WAIT:?}: {:#?}",
        d.frames
    );
    d.frames
}

fn is_ready(e: &LaneEvent) -> bool {
    matches!(e, LaneEvent::Ready { .. })
}

fn texts(frames: &[LaneEvent]) -> Vec<String> {
    frames
        .iter()
        .filter_map(|e| match e {
            LaneEvent::Message { message, .. } => Some(message.text.clone().unwrap_or_default()),
            _ => None,
        })
        .collect()
}

fn resets(frames: &[LaneEvent]) -> usize {
    frames
        .iter()
        .filter(|e| matches!(e, LaneEvent::Reset { .. }))
        .count()
}

/// A source subscribed until it lists `host_id`, and a lane opened on that
/// row through it. The source's subscription is returned so it outlives the
/// lane's cell (dropped before teardown).
async fn lane_on(
    source: &CrazeSource,
    host_id: &str,
) -> (Arc<dyn AgentLane>, shed_core::lane::SourceSubscription) {
    let mut sub = source.subscribe().await.unwrap();
    let deadline = tokio::time::Instant::now() + RECIPE_WAIT;
    loop {
        let ev = tokio::time::timeout_at(deadline, sub.rx.recv())
            .await
            .unwrap_or_else(|_| panic!("the roster never listed {host_id}"))
            .expect("the roster stream is alive");
        if matches!(&ev, SourceEvent::Session { session } if session.id == host_id) {
            break;
        }
    }
    let lane = source.open(host_id).await.unwrap();
    (lane, sub)
}

/// A lane seeded on the recipe's fake host.
async fn seeded(
    lane: &Arc<dyn AgentLane>,
) -> (Receiver<LaneEvent>, shed_core::lane::LaneStop, LaneChecker) {
    let (mut rx, stop) = lane.subscribe(None).await.unwrap().into_parts();
    let mut checker = LaneChecker::new();
    drive(&mut rx, &mut checker, "the seed", is_ready).await;
    (rx, stop, checker)
}

/// The seed and a send: the session row is the roster's hostId, the prompt
/// is a user row, the fake host's reply its `echo:` row, the turn's `done`
/// idles the session; capabilities are the fake host's (no stop, no
/// interject). `session()` answered from the roster row before any dial of
/// the lane's own.
#[tokio::test(flavor = "multi_thread")]
async fn a_lane_seeds_sends_and_echoes() {
    let Some(bins) = bins("a_lane_seeds_sends_and_echoes") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let (lane, _sub) = lane_on(&source, FAKE).await;
        let before = hook.dials();
        let s = lane.session().await.unwrap();
        assert_eq!(s.id, FAKE, "a lane's id is its hostId (P11)");
        assert_eq!(hook.dials(), before, "session() never dials");

        let (mut rx, _stop) = lane.subscribe(None).await.unwrap().into_parts();
        let mut checker = LaneChecker::new();
        let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
        let caps = checker.live_capabilities().unwrap().clone();
        assert_eq!(
            (
                caps.kind.as_str(),
                caps.interject,
                caps.stop,
                caps.approvals,
                caps.history_cursor
            ),
            ("craze", false, false, true, true)
        );
        assert!(seed
            .iter()
            .any(|e| matches!(e, LaneEvent::Session { session } if session.id == FAKE)));
        lane.send("hello there", SendMode::Queue).await.unwrap();
        let turn = drive(
            &mut rx,
            &mut checker,
            "the echo and the idle row",
            |e| matches!(e, LaneEvent::Session { session } if session.activity == RcActivity::Idle),
        )
        .await;
        let t = texts(&turn);
        assert_eq!(t, ["hello there", "echo: hello there"], "{turn:#?}");
        assert_eq!(resets(&turn), 0);
    }
    recipe.teardown().await.unwrap();
}

/// **A just-created session is openable at once** (plan 025 §3.6.4, the C9
/// hand-off): with NO roster subscription anywhere — nothing has listed the
/// new hostId — `open` on the create's row binds a lane whose `session()`
/// answers, and whose subscription seeds the transcript with the first
/// prompt and its echo.
#[tokio::test(flavor = "multi_thread")]
async fn a_created_session_opens_at_once_with_no_roster_wait() {
    let Some(bins) = bins("a_created_session_opens_at_once_with_no_roster_wait") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.set_grok_agent(&recipe.script_agent("grok-echo"));
    {
        let source = recipe.source();
        let created = source
            .create(LaneCreateRequest {
                cwd: recipe.work.to_string_lossy().into_owned(),
                provider: None,
                prompt: Some("hello at once".into()),
                request_id: new_request_id(),
            })
            .await
            .unwrap();
        let host = created.session.id.clone();
        let lane = source.open(&host).await.unwrap();
        let s = lane
            .session()
            .await
            .expect("the create's row is the lane's, before any roster");
        assert_eq!(s.id, host);
        let (mut rx, _stop) = lane.subscribe(None).await.unwrap().into_parts();
        let mut checker = LaneChecker::new();
        let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
        let mut t = texts(&seed);
        if !t.iter().any(|x| x == "echo: hello at once") {
            let more = drive(&mut rx, &mut checker, "the first prompt's echo", |e| {
                matches!(e, LaneEvent::Message { message, .. }
                    if message.text.as_deref() == Some("echo: hello at once"))
            })
            .await;
            t.extend(texts(&more));
        }
        assert!(t.iter().any(|x| x == "hello at once"), "{t:?}");
        assert!(t.iter().any(|x| x == "echo: hello at once"), "{t:?}");
    }
    recipe.teardown().await.unwrap();
}

/// Cancel with nothing to cancel is craze's `not_accepting`:
/// `NotAccepting` (correction 8); interject on a session that cannot is
/// `NotAccepting` too (its capabilities say so).
#[tokio::test(flavor = "multi_thread")]
async fn cancel_and_interject_when_idle_are_not_accepting() {
    let Some(bins) = bins("cancel_and_interject_when_idle_are_not_accepting") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let (lane, _sub) = lane_on(&recipe.source(), FAKE).await;
        let (_rx, _stop, _checker) = seeded(&lane).await;
        assert_eq!(lane.cancel().await, Err(LaneError::NotAccepting));
        assert_eq!(
            lane.send("now", SendMode::Interject).await,
            Err(LaneError::NotAccepting)
        );
    }
    recipe.teardown().await.unwrap();
}

/// A session the hub created, whose agent can interject (grok): interject
/// when idle is refused by craze (`not_in_turn`) — `NotAccepting`.
#[tokio::test(flavor = "multi_thread")]
async fn interject_on_an_idle_session_that_can_is_refused_not_accepting() {
    let Some(bins) = bins("interject_on_an_idle_session_that_can_is_refused_not_accepting") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    {
        let source = recipe.source();
        let created = source
            .create(LaneCreateRequest {
                cwd: recipe.work.to_string_lossy().into_owned(),
                provider: None,
                prompt: None,
                request_id: new_request_id(),
            })
            .await
            .unwrap();
        let (lane, _sub) = lane_on(&source, &created.session.id).await;
        let (_rx, _stop, checker) = seeded(&lane).await;
        let caps = checker.live_capabilities().unwrap().clone();
        assert!(caps.stop, "a hub-created host serves session.stop");
        if caps.interject {
            assert_eq!(
                lane.send("now", SendMode::Interject).await,
                Err(LaneError::NotAccepting)
            );
        } else {
            // A provider that cannot: refused here, the same way.
            assert_eq!(
                lane.send("now", SendMode::Interject).await,
                Err(LaneError::NotAccepting)
            );
            eprintln!("note: the fake grok session states interject: false");
        }
    }
    recipe.teardown().await.unwrap();
}

/// The three asks through the fake host's ops, each an `Approval{pending}`
/// with its `approval_request` row, answered through `asks.answer`, then
/// `Approval{resolved}`, the resolved row — and, for the question and the
/// plan, craze's outcome note.
#[tokio::test(flavor = "multi_thread")]
async fn permission_question_and_plan_are_answered() {
    let Some(bins) = bins("permission_question_and_plan_are_answered") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let (lane, _sub) = lane_on(&recipe.source(), FAKE).await;
        let (mut rx, _stop, mut checker) = seeded(&lane).await;

        // The permission: answered by its kind.
        recipe
            .op(
                FAKE,
                &json!({"name": "permission", "id": "perm-1", "tool": "Shell",
            "options": [{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
                        {"optionId": "deny", "name": "Deny", "kind": "reject_once"}]}),
            )
            .await;
        let f = drive(&mut rx, &mut checker, "the permission", |e| {
            matches!(e, LaneEvent::Approval { .. })
        })
        .await;
        let LaneEvent::Approval { approval } = f.last().unwrap() else {
            unreachable!()
        };
        assert_eq!(approval.kind, LaneApprovalKind::Permission);
        assert!(approval.status.is_pending());
        assert_eq!(
            approval
                .options
                .iter()
                .map(|o| o.id.as_str())
                .collect::<Vec<_>>(),
            ["allow", "deny"]
        );
        assert_eq!(lane.approvals().await.unwrap().len(), 1);
        lane.answer(
            "perm-1",
            LaneAnswer::Permission {
                decision: shed_core::lane::LaneDecision::Reject,
            },
        )
        .await
        .unwrap();
        drive(&mut rx, &mut checker, "resolved", |e| matches!(e, LaneEvent::Approval { approval } if approval.id == "perm-1" && !approval.status.is_pending())).await;
        assert_eq!(
            lane.answer("perm-1", LaneAnswer::Reject).await,
            Err(LaneError::AlreadyResolved),
            "a second answer"
        );

        // The question: two items, answered positionally, by label for one.
        recipe.op(FAKE, &json!({"name": "question", "id": "q-1", "title": "Pick",
            "questions": [
                {"id": "lang", "prompt": "Which language?", "options": [{"id": "rs", "label": "Rust"}, {"id": "go", "label": "Go"}]},
                {"id": "db", "prompt": "Which database?", "options": [{"id": "pg", "label": "Postgres"}]}]})).await;
        let f = drive(
            &mut rx,
            &mut checker,
            "the question",
            |e| matches!(e, LaneEvent::Approval { approval } if approval.id == "q-1"),
        )
        .await;
        let LaneEvent::Approval { approval } = f.last().unwrap() else {
            unreachable!()
        };
        assert_eq!(
            approval.questions.len(),
            2,
            "one approval carries every question"
        );
        assert_eq!(approval.questions[0].id.as_deref(), Some("lang"));
        assert!(!approval.questions[0].custom);
        lane.answer(
            "q-1",
            LaneAnswer::Question {
                answers: vec![vec!["Rust".into()], vec!["pg".into()]],
                custom_text: vec![],
            },
        )
        .await
        .unwrap();
        let f = drive(&mut rx, &mut checker, "the question's notes", |e| matches!(e, LaneEvent::Message { message, .. } if message.text.as_deref() == Some("? Which database? → Postgres"))).await;
        assert!(
            texts(&f).contains(&"? Which language? → Rust".to_string()),
            "{f:#?}"
        );

        // The plan: accepted.
        recipe.op(FAKE, &json!({"name": "plan", "id": "plan-1", "planName": "Refactor", "overview": "Split it", "plan": "1. split"})).await;
        let f = drive(
            &mut rx,
            &mut checker,
            "the plan",
            |e| matches!(e, LaneEvent::Approval { approval } if approval.id == "plan-1"),
        )
        .await;
        let LaneEvent::Approval { approval } = f.last().unwrap() else {
            unreachable!()
        };
        assert_eq!(approval.kind, LaneApprovalKind::PlanApproval);
        lane.answer(
            "plan-1",
            LaneAnswer::Choice {
                option_id: "accept".into(),
            },
        )
        .await
        .unwrap();
        let f = drive(&mut rx, &mut checker, "the plan resolved", |e| matches!(e, LaneEvent::Approval { approval } if approval.id == "plan-1" && !approval.status.is_pending())).await;
        assert!(
            texts(&f).contains(&"plan Refactor → accepted".to_string()),
            "{f:#?}"
        );
        assert!(lane.approvals().await.unwrap().is_empty());
    }
    recipe.teardown().await.unwrap();
}

/// **The fenced read against a REAL host** (Amendment A11): an ask opened
/// BEFORE the lane subscribes is seeded from the engine's registry — the real
/// host's `asks.list`, `asks.get` and `session.sync` — as a pending
/// `Approval` before the `Ready`, and is answered through the lane. (craze's
/// fake host cannot raise a SUB-AGENT's ask at the pin — its `permission` op
/// takes no `agent`, and no fake-agent script raises one — so the sub-agent
/// case is pinned against a scripted host, `tests/lane.rs`.)
#[tokio::test(flavor = "multi_thread")]
async fn an_ask_open_before_the_seed_comes_from_the_registry_and_is_answerable() {
    let Some(bins) = bins("an_ask_open_before_the_seed_comes_from_the_registry_and_is_answerable")
    else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    recipe
        .op(
            FAKE,
            &json!({"name": "permission", "id": "perm-early", "tool": "Shell",
                "options": [{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
                            {"optionId": "deny", "name": "Deny", "kind": "reject_once"}]}),
        )
        .await;
    {
        let (lane, _sub) = lane_on(&recipe.source(), FAKE).await;
        let (mut rx, _stop) = lane.subscribe(None).await.unwrap().into_parts();
        let mut checker = LaneChecker::new();
        let seed = drive(&mut rx, &mut checker, "the seed", is_ready).await;
        assert!(
            seed.iter()
                .any(|e| matches!(e, LaneEvent::Approval { approval }
                if approval.id == "perm-early" && approval.status.is_pending())),
            "{seed:#?}"
        );
        assert_eq!(lane.approvals().await.unwrap().len(), 1);
        lane.answer(
            "perm-early",
            LaneAnswer::Choice {
                option_id: "deny".into(),
            },
        )
        .await
        .unwrap();
        drive(&mut rx, &mut checker, "resolved", |e| matches!(e, LaneEvent::Approval { approval } if approval.id == "perm-early" && !approval.status.is_pending())).await;
        assert!(lane.approvals().await.unwrap().is_empty());
    }
    recipe.teardown().await.unwrap();
}

/// `stop()` on a session the hub created, with a turn running and an ask open
/// (a `grok-ask` agent, which waits on its question): the receipt, then the
/// closing records — the ask's ending, the turn's — then
/// `Down{"session_closed"}` (Amendment A7: WIRE/14 alone shows only receipt →
/// reset, so this arranges the records in between).
#[tokio::test(flavor = "multi_thread")]
async fn stop_ends_the_lane_after_its_closing_records() {
    let Some(bins) = bins("stop_ends_the_lane_after_its_closing_records") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.set_grok_agent(&recipe.script_agent("grok-ask"));
    {
        let source = recipe.source();
        let created = source
            .create(LaneCreateRequest {
                cwd: recipe.work.to_string_lossy().into_owned(),
                provider: None,
                prompt: None,
                request_id: new_request_id(),
            })
            .await
            .unwrap();
        let (lane, _sub) = lane_on(&source, &created.session.id).await;
        let (mut rx, _stop, mut checker) = seeded(&lane).await;
        assert!(checker.live_capabilities().unwrap().stop);
        lane.send("ask me", SendMode::Queue).await.unwrap();
        let f = drive(
            &mut rx,
            &mut checker,
            "the ask opens",
            |e| matches!(e, LaneEvent::Approval { approval } if approval.status.is_pending()),
        )
        .await;
        let ask = f
            .iter()
            .rev()
            .find_map(|e| match e {
                LaneEvent::Approval { approval } => Some(approval.id.clone()),
                _ => None,
            })
            .unwrap();
        lane.stop().await.unwrap();
        let f = drive(&mut rx, &mut checker, "Down", |e| {
            matches!(e, LaneEvent::Down { .. })
        })
        .await;
        assert_eq!(
            f.last(),
            Some(&LaneEvent::Down {
                reason: "session_closed".into()
            })
        );
        assert!(
            f.iter().any(|e| matches!(e, LaneEvent::Approval { approval } if approval.id == ask && !approval.status.is_pending())),
            "the closing records (the open ask's ending) folded before the Down: {f:#?}"
        );
        assert!(checker.ended());
    }
    recipe.teardown().await.unwrap();
}

/// **The silent resume**: the lane's bridge killed mid-stream, the fake host
/// writes more while it is gone, and the lane reconnects with NO `Reset`: the
/// same generation, the rows contiguous, the missed text folded once.
#[tokio::test(flavor = "multi_thread")]
async fn a_killed_bridge_resumes_silently() {
    let Some(bins) = bins("a_killed_bridge_resumes_silently") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let (lane, _sub) = lane_on(&source, FAKE).await;
        let (mut rx, _stop, mut checker) = seeded(&lane).await;
        recipe
            .op(
                FAKE,
                &json!({"name": "tool", "id": "t-1", "toolName": "Shell", "status": "completed"}),
            )
            .await;
        let before = drive(&mut rx, &mut checker, "a row before", |e| matches!(e, LaneEvent::Message { message, .. } if message.msg_type == "tool_result")).await;
        let first_seq = before
            .iter()
            .rev()
            .find_map(|e| match e {
                LaneEvent::Message { message, .. } => Some(message.seq),
                _ => None,
            })
            .unwrap();
        hook.hold_dials();
        assert!(hook.sever_live() >= 1, "the lane's bridge was cut");
        drive(&mut rx, &mut checker, "Stale", |e| {
            matches!(e, LaneEvent::Stale { .. })
        })
        .await;
        recipe
            .op(
                FAKE,
                &json!({"name": "tool", "id": "t-2", "toolName": "Shell", "status": "completed"}),
            )
            .await;
        recipe
            .op(
                FAKE,
                &json!({"name": "tool", "id": "t-3", "toolName": "Shell", "status": "completed"}),
            )
            .await;
        hook.release_dials();
        let resumed = drive(&mut rx, &mut checker, "the lone Ready", is_ready).await;
        assert_eq!(resets(&resumed), 0, "a silent resume: {resumed:#?}");
        assert_eq!(checker.resets(), 1);
        assert_eq!(
            checker.live_generation(),
            Some(1),
            "the generation unchanged"
        );
        let seqs: Vec<u64> = resumed
            .iter()
            .filter_map(|e| match e {
                LaneEvent::Message { message, .. } => Some(message.seq),
                _ => None,
            })
            .collect();
        assert_eq!(
            seqs.len(),
            4,
            "t-2 and t-3, each a tool_use and a tool_result: {resumed:#?}"
        );
        let expect: Vec<u64> = (first_seq + 1..=first_seq + 4).collect();
        assert_eq!(seqs, expect, "contiguous");
    }
    recipe.teardown().await.unwrap();
}

/// **A refused cursor**: the lane cut off, the fake host restarted into a new
/// incarnation while it is away, and the cursor offered on the redial is
/// refused — one `Reset{cursor_lost:…} … Ready`.
#[tokio::test(flavor = "multi_thread")]
async fn a_restarted_host_refuses_the_cursor_and_the_lane_reseeds() {
    let Some(bins) = bins("a_restarted_host_refuses_the_cursor_and_the_lane_reseeds") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let hook = HookDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&hook) as Arc<dyn CrazeDial>);
        let (lane, _sub) = lane_on(&source, FAKE).await;
        let (mut rx, _stop, mut checker) = seeded(&lane).await;
        hook.hold_dials();
        hook.sever_live();
        drive(&mut rx, &mut checker, "Stale", |e| {
            matches!(e, LaneEvent::Stale { .. })
        })
        .await;
        recipe.op(FAKE, &json!({"name": "restart"})).await;
        hook.release_dials();
        let f = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
        let reasons: Vec<&str> = f
            .iter()
            .filter_map(|e| match e {
                LaneEvent::Reset { reason, .. } => Some(reason.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(reasons.len(), 1, "{f:#?}");
        assert!(reasons[0].starts_with("cursor_lost:"), "{reasons:?}");
        assert_eq!(checker.live_generation(), Some(2));
    }
    recipe.teardown().await.unwrap();
}

/// The fake host restarted WHILE the lane is attached: `session_replaced`, a
/// fresh `hello`, the session id re-learned, a reseed.
#[tokio::test(flavor = "multi_thread")]
async fn a_restart_while_attached_is_session_replaced() {
    let Some(bins) = bins("a_restart_while_attached_is_session_replaced") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let (lane, _sub) = lane_on(&recipe.source(), FAKE).await;
        let (mut rx, _stop, mut checker) = seeded(&lane).await;
        recipe.op(FAKE, &json!({"name": "restart"})).await;
        let f = drive(&mut rx, &mut checker, "the reseed", is_ready).await;
        assert!(f.iter().any(|e| matches!(e, LaneEvent::Reset { reason, .. } if reason == "server_reset:session_replaced")), "{f:#?}");
    }
    recipe.teardown().await.unwrap();
}

/// **`Lagged` → reseed** (correction 13, gx's overflow cell on the real
/// hub): a client that reads nothing while more rows than the channel holds
/// go past gets the generation abandoned, the connection dropped, and — once
/// it drains — one `Reset{lagged} … Ready`.
#[tokio::test(flavor = "multi_thread")]
async fn a_client_that_stops_reading_gets_a_lagged_reseed() {
    let Some(bins) = bins("a_client_that_stops_reading_gets_a_lagged_reseed") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let (lane, _sub) = lane_on(&recipe.source(), FAKE).await;
        let (mut rx, _stop, mut checker) = seeded(&lane).await;
        // From here the client reads nothing: each op is two rows.
        let n = LANE_CHANNEL_CAPACITY / 2 + 50;
        for i in 0..n {
            recipe.op(FAKE, &json!({"name": "tool", "id": format!("t-{i}"), "toolName": "Shell", "status": "completed"})).await;
        }
        // Wait until the queue is full, then drain it.
        let deadline = tokio::time::Instant::now() + RECIPE_WAIT;
        while rx.len() < LANE_CHANNEL_CAPACITY && tokio::time::Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        assert_eq!(
            rx.len(),
            LANE_CHANNEL_CAPACITY,
            "the client holds a full queue"
        );
        let f = drive(&mut rx, &mut checker, "the lagged reseed", |e| {
            matches!(e, LaneEvent::Ready { generation: 2 })
        })
        .await;
        let reasons: Vec<&str> = f
            .iter()
            .filter_map(|e| match e {
                LaneEvent::Reset { reason, .. } => Some(reason.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(reasons, ["lagged"]);
    }
    recipe.teardown().await.unwrap();
}

/// **The identity cell, on the wire**: a lane opened by hostId — its connect
/// names the host, and every session-scoped request it writes carries the
/// craze sessionId the roster gave the row.
#[tokio::test(flavor = "multi_thread")]
async fn every_session_call_carries_the_craze_session_id() {
    let Some(bins) = bins("every_session_call_carries_the_craze_session_id") else {
        return;
    };
    let recipe = Recipe::start(&bins);
    recipe.fake_host(FAKE, FAKE_SESSION).await;
    {
        let tee = TeeDial::new(Arc::new(recipe.dial()));
        let source = recipe.source_on(Arc::clone(&tee) as Arc<dyn CrazeDial>);
        let (lane, _sub) = lane_on(&source, FAKE).await;
        let (_rx, _stop, _checker) = seeded(&lane).await;
        lane.send("hi", SendMode::Queue).await.unwrap();
        let _ = lane.cancel().await;
        let _ = lane.history(None, 50).await.unwrap();
        let mut scoped = 0;
        let mut connects = 0;
        for l in tee.lines().iter().filter(|l| l.dir == "c2s") {
            let msg: Value = serde_json::from_str(&l.line).unwrap();
            let method = msg["method"].as_str().unwrap_or_default();
            if method == "session.connect" {
                connects += 1;
                assert_eq!(msg["params"]["sessionId"], FAKE, "connect names the hostId");
            } else if method.starts_with("session.") || method.starts_with("asks.") {
                scoped += 1;
                assert_eq!(msg["params"]["sessionId"], FAKE_SESSION, "{msg}");
            }
        }
        assert!(
            connects >= 1 && scoped >= 4,
            "{connects} connects, {scoped} session calls"
        );
    }
    recipe.teardown().await.unwrap();
}
