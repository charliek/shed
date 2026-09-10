//! The REST half of the adapter, against [`FakeOpencode`]: the verbs, the
//! directory routing, the error table, and the pin guard.

mod common;

use std::time::Duration;

use common::{assert_clean, wait_for};
use serde_json::json;
use shed_core::lane::{AgentLane, LaneAnswer, LaneApprovalKind, LaneDecision, LaneError, SendMode};
use shed_core::rc::RcActivity;
use shed_opencode::client::{MAX_DESCENDANT_DEPTH, MAX_DESCENDANT_SESSIONS, MAX_REST_BYTES};
use shed_opencode::testing::FakeOpencode;
use shed_opencode::{BasicAuth, OpencodeClient};

fn client(fake: &FakeOpencode) -> OpencodeClient {
    OpencodeClient::new(fake.base_url(), None).expect("the client builds")
}

/// The fake with one root, one child and one sibling root, all in `/w`.
async fn three_sessions() -> FakeOpencode {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "root", "/w", None);
    fake.add_session("ses_child", "child", "/w", Some("ses_a"));
    fake.add_session("ses_sib", "sibling", "/w", None);
    fake.pin("ses_a");
    fake
}

#[tokio::test]
async fn capabilities_are_opencodes_pinned_row() {
    let fake = FakeOpencode::start().await;
    let caps = client(&fake).capabilities();
    assert_eq!(caps.kind, "opencode");
    assert!(
        !caps.interject,
        "prompt_async joins a turn, it cannot preempt"
    );
    assert!(caps.create);
    assert!(caps.cancel);
    assert!(caps.approvals);
    assert!(
        !caps.history_cursor,
        "there is no v1 per-session ?after= route: history refolds from the top"
    );
}

#[tokio::test]
async fn sessions_lists_roots_with_a_per_directory_status_poll() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w1", None);
    fake.add_session("ses_child", "child", "/w1", Some("ses_a"));
    fake.add_session("ses_c", "c", "/w2", None);
    fake.set_status("ses_a", "busy");
    // ses_c is absent from the status map, which opencode reads as idle.

    let rows = client(&fake).sessions().await.expect("sessions");
    let ids: Vec<&str> = rows.iter().map(|r| r.id.as_str()).collect();
    assert_eq!(ids, vec!["ses_a", "ses_c"], "children are not roster rows");
    assert_eq!(rows[0].activity, RcActivity::Working);
    assert_eq!(rows[1].activity, RcActivity::Idle);
    assert!(
        rows.iter().all(|r| r.approximate),
        "a status poll cannot see approvals, so the row says so"
    );

    // /session/status is INSTANCE-scoped: one read per distinct directory, or
    // the second directory's sessions are silently reported for the first.
    let gets = fake.get_paths();
    assert!(gets.iter().any(|g| g == "/session/status?directory=%2Fw1"));
    assert!(gets.iter().any(|g| g == "/session/status?directory=%2Fw2"));
    assert_clean(&fake);
}

#[tokio::test]
async fn a_failed_status_read_degrades_to_unknown_rather_than_claiming_idle() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w1", None);
    fake.fail_get("/session/status", 500);

    let rows = client(&fake)
        .sessions()
        .await
        .expect("the roster still loads");
    assert_eq!(rows[0].activity, RcActivity::Unknown);
    assert_clean(&fake);
}

#[tokio::test]
async fn session_counts_the_root_and_its_childs_approvals() {
    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_root", "bash", "ls");
    fake.add_permission("ses_child", "per_child", "bash", "pwd");
    fake.add_permission("ses_sib", "per_sib", "bash", "rm -rf /");
    fake.set_status("ses_a", "busy");

    let row = client(&fake).session("ses_a").await.expect("session");
    assert_eq!(row.id, "ses_a");
    assert_eq!(row.cwd, "/w");
    assert_eq!(row.activity, RcActivity::Working);
    assert_eq!(
        row.pending_approvals, 2,
        "the root's and its child's — never the sibling root's"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn history_refolds_from_the_top_through_a_fresh_ring() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.set_simple_transcript("ses_a", "hello", "hi there");

    let page = client(&fake)
        .history("ses_a", None, 100)
        .await
        .expect("history");
    let texts: Vec<&str> = page
        .messages
        .iter()
        .filter_map(|m| m.text.as_deref())
        .collect();
    assert_eq!(texts, vec!["hello", "hi there"]);
    assert_eq!(
        page.messages.iter().map(|m| m.seq).collect::<Vec<_>>(),
        vec![1, 2],
        "a fresh ring per call: seq starts at 1 and is call-local"
    );
    assert!(!page.truncated);
    assert_eq!(page.cursor, None, "history_cursor is false");
    assert_clean(&fake);
}

#[tokio::test]
async fn history_returns_the_tail_and_reports_truncation() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    let turns: Vec<serde_json::Value> = (0..6)
        .map(|i| {
            json!({
                "info": { "id": format!("msg_{i}"), "role": "user",
                          "time": { "created": 1_700_000_000_000i64 + i } },
                "parts": [ { "id": format!("prt_{i}"), "messageID": format!("msg_{i}"),
                             "type": "text", "text": format!("turn {i}") } ],
            })
        })
        .collect();
    fake.set_messages("ses_a", json!(turns));

    let page = client(&fake)
        .history("ses_a", None, 2)
        .await
        .expect("history");
    let texts: Vec<&str> = page
        .messages
        .iter()
        .filter_map(|m| m.text.as_deref())
        .collect();
    assert_eq!(
        texts,
        vec!["turn 4", "turn 5"],
        "the END of the conversation"
    );
    assert!(
        page.truncated,
        "the client is not holding the whole history"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn history_ignores_a_cursor_it_cannot_honor() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.set_simple_transcript("ses_a", "hello", "hi there");
    let with = client(&fake)
        .history("ses_a", Some("whatever"), 100)
        .await
        .expect("history");
    let without = client(&fake)
        .history("ses_a", None, 100)
        .await
        .expect("history");
    assert_eq!(with, without, "the cursor is ignored, not honored halfway");
}

#[tokio::test]
async fn create_sends_the_directory_as_a_query_then_prompts() {
    let fake = FakeOpencode::start().await;
    let created = client(&fake)
        .create("/tmp/project", "first prompt")
        .await
        .expect("create");
    // The pin is only known AFTER the create, which is why it is set here.
    fake.pin(&created.id);

    let targets = fake.post_targets();
    assert_eq!(
        targets[0], "/session?directory=%2Ftmp%2Fproject",
        "opencode takes the directory as a QUERY on create, not in the body"
    );
    assert_eq!(targets[1], format!("/session/{}/prompt_async", created.id));
    let body = fake
        .post_body("/prompt_async")
        .expect("the prompt body was recorded");
    assert_eq!(body, r#"{"parts":[{"text":"first prompt","type":"text"}]}"#);
    assert_clean(&fake);
}

#[tokio::test]
async fn a_failed_create_surfaces_failed() {
    let fake = FakeOpencode::start().await;
    fake.fail_post("/session", 500);
    let err = client(&fake)
        .create("/tmp/project", "hi")
        .await
        .expect_err("a 500 create cannot succeed");
    assert!(matches!(err, LaneError::Failed(_)), "got {err:?}");
    assert_clean(&fake);
}

#[tokio::test]
async fn a_failed_prompt_surfaces_failed() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.pin("ses_a");
    fake.fail_post("/prompt_async", 500);
    let err = client(&fake)
        .send("ses_a", "hi", SendMode::Queue)
        .await
        .expect_err("a 500 prompt cannot succeed");
    assert!(matches!(err, LaneError::Failed(_)), "got {err:?}");
    assert_clean(&fake);
}

#[tokio::test]
async fn interject_is_refused_rather_than_downgraded() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.pin("ses_a");
    let err = client(&fake)
        .send("ses_a", "now", SendMode::Interject)
        .await
        .expect_err("opencode cannot interject");
    assert_eq!(err, LaneError::NotAccepting);
    assert!(
        fake.post_paths().is_empty(),
        "a refusal must not reach the wire at all"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn the_verbs_address_only_the_pinned_session() {
    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_root", "bash", "ls");
    let lane = client(&fake);

    lane.send("ses_a", "go", SendMode::Queue)
        .await
        .expect("send");
    lane.cancel("ses_a").await.expect("cancel");
    lane.answer(
        "ses_a",
        "per_root",
        LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce,
        },
    )
    .await
    .expect("answer");

    assert_eq!(
        fake.post_paths(),
        vec![
            "/session/ses_a/prompt_async",
            "/session/ses_a/abort",
            "/permission/per_root/reply",
        ]
    );
    assert_eq!(
        fake.post_body("/permission/per_root/reply").as_deref(),
        Some(r#"{"reply":"once"}"#)
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn approvals_cover_the_root_and_its_child_never_a_sibling() {
    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_root", "bash", "ls");
    fake.add_permission("ses_child", "per_child", "bash", "pwd");
    fake.add_permission("ses_sib", "per_sib", "bash", "rm -rf /");
    fake.add_question(
        "ses_a",
        "que_root",
        "Pick",
        "Which one?",
        &["left", "right"],
    );

    let open = client(&fake).approvals("ses_a").await.expect("approvals");
    let ids: Vec<&str> = open.iter().map(|a| a.id.as_str()).collect();
    assert_eq!(ids, vec!["per_root", "per_child", "que_root"]);

    let child = &open[1];
    assert_eq!(
        child.session_id, "ses_child",
        "a descendant's approval is attributed to the DESCENDANT"
    );
    assert_eq!(child.kind, LaneApprovalKind::Permission);
    assert!(
        !child.options.is_empty() && child.questions.is_empty(),
        "kind selects: a permission fills options"
    );

    let question = &open[2];
    assert_eq!(question.kind, LaneApprovalKind::Question);
    assert!(
        question.options.is_empty() && !question.questions.is_empty(),
        "kind selects: a question fills questions"
    );
    assert_eq!(question.questions[0].options[0].id, "left");
    assert_clean(&fake);
}

#[tokio::test]
async fn answering_a_child_hits_the_childs_own_request_id() {
    let fake = three_sessions().await;
    fake.add_permission("ses_child", "per_child", "bash", "pwd");
    client(&fake)
        .answer(
            "ses_a",
            "per_child",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowAlways,
            },
        )
        .await
        .expect("answering a descendant's approval from the root's panel");
    assert_eq!(fake.post_paths(), vec!["/permission/per_child/reply"]);
    assert_eq!(
        fake.post_body("/reply").as_deref(),
        Some(r#"{"reply":"always"}"#)
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn a_question_answer_and_a_reject_take_their_own_routes() {
    let fake = three_sessions().await;
    fake.add_question("ses_a", "que_1", "Pick", "Which?", &["left"]);
    fake.add_question("ses_a", "que_2", "Pick", "Which?", &["left"]);
    let lane = client(&fake);
    lane.answer(
        "ses_a",
        "que_1",
        LaneAnswer::Question {
            answers: vec![vec!["left".to_string()]],
        },
    )
    .await
    .expect("question answer");
    lane.answer("ses_a", "que_2", LaneAnswer::Reject)
        .await
        .expect("question reject");
    assert_eq!(
        fake.post_paths(),
        vec!["/question/que_1/reply", "/question/que_2/reject"]
    );
    assert_eq!(
        fake.post_body("/question/que_1/reply").as_deref(),
        Some(r#"{"answers":[["left"]]}"#)
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn basic_auth_is_honored_and_its_absence_is_unauthorized() {
    let fake = FakeOpencode::start_with_auth("opencode", "hunter2").await;
    fake.add_session("ses_a", "a", "/w", None);

    let anonymous = client(&fake);
    assert_eq!(
        anonymous
            .session("ses_a")
            .await
            .expect_err("no credentials"),
        LaneError::Unauthorized
    );
    assert_eq!(
        anonymous
            .subscribe("ses_a", None)
            .await
            .err()
            .expect("subscribe demands the same credentials"),
        LaneError::Unauthorized
    );

    let authed = OpencodeClient::new(fake.base_url(), Some(BasicAuth::password("hunter2")))
        .expect("the client builds");
    assert_eq!(
        authed.session("ses_a").await.expect("authorized").id,
        "ses_a"
    );
    assert_clean(&fake);
}

/// Every arm of the contract's error enum, reached the way a caller would reach
/// it. A variant nothing can produce is a variant a client will never handle
/// correctly.
#[tokio::test]
async fn every_lane_error_variant_is_reachable() {
    // Unavailable: nothing listening. The port is taken and released first, so
    // the address is real and the dial is refused rather than routed.
    let dead = {
        let socket = tokio::net::TcpListener::bind(("127.0.0.1", 0))
            .await
            .expect("a throwaway port");
        let addr = socket.local_addr().expect("its address");
        drop(socket);
        OpencodeClient::new(format!("http://{addr}/").parse().expect("url"), None)
            .expect("the client builds")
    };
    assert!(
        matches!(
            dead.session("ses_a").await.expect_err("nothing to dial"),
            LaneError::Unavailable(_)
        ),
        "a refused dial is quiet, not a Failed"
    );

    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_root", "bash", "ls");
    fake.add_permission("ses_a", "per_gone", "bash", "ls");
    fake.add_permission("ses_a", "per_bad", "bash", "ls");
    fake.add_permission("ses_a", "per_busy", "bash", "ls");
    let lane = client(&fake);

    // Unauthorized has its own test (it needs an auth-demanding fake).
    // UnknownSession: an id the server does not have.
    assert_eq!(
        lane.session("ses_missing")
            .await
            .expect_err("no such session"),
        LaneError::UnknownSession
    );

    // UnknownApproval: a 404 on an APPROVAL route maps to the approval arm, not
    // the session one.
    fake.fail_post("/permission/per_gone/reply", 404);
    assert_eq!(
        lane.answer(
            "ses_a",
            "per_gone",
            LaneAnswer::Permission {
                decision: LaneDecision::Reject
            },
        )
        .await
        .expect_err("the request is gone"),
        LaneError::UnknownApproval
    );

    // BadRequest: a 4xx carrying opencode's own error body, message kept whole.
    fake.fail_post("/permission/per_bad/reply", 400);
    assert_eq!(
        lane.answer(
            "ses_a",
            "per_bad",
            LaneAnswer::Permission {
                decision: LaneDecision::Reject
            },
        )
        .await
        .expect_err("a 400 with a body"),
        LaneError::BadRequest("injected failure".to_string())
    );

    // NotAccepting: a 409, and (separately) an Interject.
    fake.fail_post("/permission/per_busy/reply", 409);
    assert_eq!(
        lane.answer(
            "ses_a",
            "per_busy",
            LaneAnswer::Permission {
                decision: LaneDecision::Reject
            },
        )
        .await
        .expect_err("a 409"),
        LaneError::NotAccepting
    );

    // AlreadySubmitted / AlreadyResolved: the client-side double-tap gate. The
    // first answer succeeds, so the second is `AlreadyResolved`; a second tap
    // while the first is still IN FLIGHT is `AlreadySubmitted`, which is what
    // the concurrent pair below exercises.
    lane.answer(
        "ses_a",
        "per_root",
        LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce,
        },
    )
    .await
    .expect("the first answer lands");
    assert_eq!(
        lane.answer(
            "ses_a",
            "per_root",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce
            },
        )
        .await
        .expect_err("the same approval, twice"),
        LaneError::AlreadyResolved
    );

    // Failed: a 5xx with nothing to read.
    fake.fail_post("/abort", 500);
    assert!(
        matches!(
            lane.cancel("ses_a").await.expect_err("a 500"),
            LaneError::Failed(_)
        ),
        "a 5xx is the loud residue"
    );
    assert_clean(&fake);
}

/// The in-flight half of the double-tap gate, isolated: the second tap arrives
/// while the first POST is parked, so it cannot be an `AlreadyResolved`.
#[tokio::test]
async fn a_double_tap_while_the_answer_is_in_flight_is_already_submitted() {
    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_root", "bash", "ls");
    let lane = client(&fake);
    // Park the first answer INSIDE the fake, so "in flight" is a condition the
    // test stands on rather than a race it hopes to win.
    fake.hold_post("/permission/per_root/reply");
    let first = {
        let lane = lane.clone();
        tokio::spawn(async move {
            lane.answer(
                "ses_a",
                "per_root",
                LaneAnswer::Permission {
                    decision: LaneDecision::AllowOnce,
                },
            )
            .await
        })
    };
    wait_for("the first answer to reach the server", || {
        !fake.post_paths().is_empty()
    })
    .await;

    assert_eq!(
        lane.answer(
            "ses_a",
            "per_root",
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce
            },
        )
        .await
        .expect_err("a second tap while the first is in flight"),
        LaneError::AlreadySubmitted
    );

    fake.release_post("/permission/per_root/reply");
    first
        .await
        .expect("the first answer's task")
        .expect("the first answer");
    assert_eq!(
        fake.post_paths(),
        vec!["/permission/per_root/reply"],
        "exactly ONE answer reached opencode"
    );
    assert_clean(&fake);
}

/// The guard itself, driven with raw HTTP rather than through the lane — so
/// this proves the guard is not vacuous WITHOUT making the lane look guilty.
/// It is the one fake in this suite whose violation list is deliberately
/// non-empty.
#[tokio::test]
async fn the_pin_guard_catches_an_off_pin_mutation() {
    let fake = three_sessions().await;
    let raw = reqwest::Client::builder()
        .no_proxy()
        .build()
        .expect("a raw client");

    let base = fake.base_url();
    // A session-scoped mutation naming a session other than the pin.
    let resp = raw
        .post(base.join("session/ses_sib/abort").expect("url"))
        .send()
        .await
        .expect("the fake answers");
    assert_eq!(resp.status(), 500, "a violation can never look successful");

    // An answer route naming a request the fake never issued.
    let resp = raw
        .post(base.join("permission/per_nobody/reply").expect("url"))
        .json(&json!({ "reply": "once" }))
        .send()
        .await
        .expect("the fake answers");
    assert_eq!(resp.status(), 500);

    let violations = fake.violations();
    assert_eq!(violations.len(), 2, "both were recorded: {violations:#?}");
    assert!(violations[0].contains("ses_sib"));
    assert!(violations[1].contains("per_nobody"));
}

/// The DEPRECATED session-scoped answer route owes the same ledger check as the
/// live one. Naming the pinned session is not enough — it still answers an
/// approval, and answering one the fake never issued is exactly the
/// cross-contamination the guard exists to catch. Without the ledger check this
/// returns 200 and records nothing, which is how a bypass hides: the suite stays
/// green while the guard silently stops guarding half the surface.
#[tokio::test]
async fn the_pin_guard_catches_an_unissued_answer_on_the_deprecated_route() {
    let fake = three_sessions().await;
    let raw = reqwest::Client::builder()
        .no_proxy()
        .build()
        .expect("a raw client");
    let base = fake.base_url();

    // The pinned session, but a request id the fake never issued.
    let resp = raw
        .post(
            base.join("session/ses_a/permissions/per_nobody")
                .expect("url"),
        )
        .json(&json!({ "response": "once" }))
        .send()
        .await
        .expect("the fake answers");
    assert_eq!(
        resp.status(),
        500,
        "an unissued answer can never look successful, even on the legacy route"
    );

    let violations = fake.violations();
    assert_eq!(violations.len(), 1, "recorded: {violations:#?}");
    assert!(
        violations[0].contains("per_nobody") && violations[0].contains("never issued"),
        "names the offending request: {}",
        violations[0]
    );
}

#[tokio::test]
async fn an_id_with_a_slash_cannot_re_address_the_request() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    // Left UNPINNED on purpose: the point here is the ESCAPING, and pinning
    // would make the guard fire on the (correctly) escaped id and muddy the
    // suite's violation ledger.
    //
    // The id is untrusted input — it arrives from a remote roost tab — so it is
    // percent-encoded into ONE path segment rather than pasted into the path.
    let err = client(&fake)
        .send("ses_a/../ses_b", "hi", SendMode::Queue)
        .await
        .expect_err("the fake has no such session");
    assert_eq!(
        err,
        LaneError::UnknownSession,
        "the escaped id is one segment naming a session that does not exist"
    );
    assert_eq!(
        fake.post_paths(),
        vec!["/session/ses_a%2F..%2Fses_b/prompt_async"],
        "one segment, escaped — `..` never becomes a path traversal"
    );
    assert_clean(&fake);
}

#[tokio::test]
async fn a_held_request_is_observable_before_it_answers() {
    // The scaffolding the overflow and seed-injection arms stand on, asserted
    // once here so a failure there is not mistaken for a watcher bug.
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.hold_get("/session/ses_a/message");
    let lane = client(&fake);
    let pending = tokio::spawn(async move { lane.history("ses_a", None, 10).await });
    wait_for("the held GET to be recorded", || {
        fake.get_paths()
            .iter()
            .any(|g| g == "/session/ses_a/message")
    })
    .await;
    assert!(
        !pending.is_finished(),
        "the request is parked, not answered"
    );
    fake.release_get("/session/ses_a/message");
    pending
        .await
        .expect("the parked task")
        .expect("the released request answers");
    assert_clean(&fake);
}

/// `parentID` comes off opencode's wire, so a cycle is representable input. The
/// descendant walk must terminate on one rather than fetching `/children`
/// forever — the visited set is what does it.
#[tokio::test]
async fn a_parent_cycle_terminates_the_descendant_walk() {
    let fake = FakeOpencode::start().await;
    // ses_a → ses_b → ses_a.
    fake.add_session("ses_a", "a", "/w", Some("ses_b"));
    fake.add_session("ses_b", "b", "/w", Some("ses_a"));
    fake.add_permission("ses_b", "per_b", "bash", "ls");
    fake.pin("ses_a");

    let open = tokio::time::timeout(Duration::from_secs(15), client(&fake).approvals("ses_a"))
        .await
        .expect("the walk terminated on a cycle instead of looping")
        .expect("approvals");
    let ids: Vec<&str> = open.iter().map(|a| a.id.as_str()).collect();
    assert_eq!(ids, vec!["per_b"]);
    // One request per VISITED id, never one per traversal step.
    let children_gets = fake
        .get_paths()
        .iter()
        .filter(|g| g.ends_with("/children"))
        .count();
    assert_eq!(
        children_gets, 2,
        "ses_a and ses_b, each expanded exactly once"
    );
    assert_clean(&fake);
}

/// Depth is bounded: a chain deeper than [`MAX_DESCENDANT_DEPTH`] stops there
/// rather than walking an arbitrarily long tree of untrusted `parentID`s.
#[tokio::test]
async fn the_descendant_walk_stops_at_the_depth_bound() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_0", "root", "/w", None);
    for level in 1..=(MAX_DESCENDANT_DEPTH + 2) {
        let id = format!("ses_{level}");
        fake.add_session(&id, "child", "/w", Some(&format!("ses_{}", level - 1)));
        fake.add_permission(&id, &format!("per_{level}"), "bash", "ls");
    }
    fake.pin("ses_0");

    let open = client(&fake).approvals("ses_0").await.expect("approvals");
    let ids: Vec<String> = open.iter().map(|a| a.id.clone()).collect();
    assert_eq!(
        ids.len(),
        MAX_DESCENDANT_DEPTH,
        "exactly the levels the bound allows: {ids:?}"
    );
    assert!(
        ids.contains(&format!("per_{MAX_DESCENDANT_DEPTH}")),
        "the last allowed level is IN scope: {ids:?}"
    );
    assert!(
        !ids.contains(&format!("per_{}", MAX_DESCENDANT_DEPTH + 1)),
        "the level past the bound is not: {ids:?}"
    );
    assert_clean(&fake);
}

/// And the total is bounded: a pathologically WIDE tree stops at
/// [`MAX_DESCENDANT_SESSIONS`] ids (the root included) instead of growing the
/// scope — and the request count, one per expanded id — without limit.
#[tokio::test]
async fn the_descendant_walk_stops_at_the_total_bound() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_root", "root", "/w", None);
    for i in 0..(MAX_DESCENDANT_SESSIONS + 50) {
        let id = format!("ses_{i}");
        fake.add_session(&id, "child", "/w", Some("ses_root"));
        fake.add_permission(&id, &format!("per_{i}"), "bash", "ls");
    }
    fake.pin("ses_root");

    let open = client(&fake)
        .approvals("ses_root")
        .await
        .expect("approvals");
    assert_eq!(
        open.len(),
        MAX_DESCENDANT_SESSIONS - 1,
        "the root plus its ceiling of descendants, and no more"
    );
    let children_gets = fake
        .get_paths()
        .iter()
        .filter(|g| g.ends_with("/children"))
        .count();
    assert_eq!(
        children_gets, 1,
        "the bound was reached at depth 1, so no second level was expanded"
    );
    assert_clean(&fake);
}

/// A REST body is capped WHILE it is consumed. `.text()` collects the whole
/// response before anything can look at it, so a large `/message` answer can
/// exhaust memory well inside the 5 s timeout — the ring's own bounds only apply
/// after the bytes are allocated and decoded. A declared `Content-Length` past
/// the cap is refused before a single body byte is read.
#[tokio::test]
async fn an_oversized_rest_body_is_refused_from_its_declared_length() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.flood_get("/session/ses_a/message", MAX_REST_BYTES * 8, true);

    let err = client(&fake)
        .history("ses_a", None, 10)
        .await
        .expect_err("an over-cap body is not a transcript");
    match &err {
        LaneError::Failed(msg) => assert!(
            msg.contains(&MAX_REST_BYTES.to_string()),
            "the error names the cap: {msg}"
        ),
        other => panic!("got {other:?}"),
    }

    wait_for("the flood to stop writing", || {
        fake.flooded_bytes().is_some()
    })
    .await;
    let wrote = fake.flooded_bytes().expect("the flood finished");
    assert!(
        wrote < MAX_REST_BYTES,
        "the body was refused from the head, not drained: {wrote} bytes written"
    );
    assert_clean(&fake);
}

/// The same cap with nothing to refuse it from: a close-delimited body carries
/// no `Content-Length`, so the bound has to hold while the body is consumed.
#[tokio::test]
async fn an_oversized_close_delimited_body_is_refused_while_it_is_consumed() {
    let fake = FakeOpencode::start().await;
    fake.add_session("ses_a", "a", "/w", None);
    fake.flood_get("/session/ses_a/message", MAX_REST_BYTES + (1 << 20), false);

    let err = client(&fake)
        .history("ses_a", None, 10)
        .await
        .expect_err("an over-cap body is not a transcript");
    match &err {
        LaneError::Failed(msg) => assert!(
            msg.contains(&MAX_REST_BYTES.to_string()),
            "the error names the cap: {msg}"
        ),
        other => panic!("got {other:?}"),
    }
    assert_clean(&fake);
}

/// A cancelled answer must not leave its ledger claim stuck "in flight". The
/// `answer` future can be dropped at the await inside it — a `select!` arm that
/// lost, an aborted task, a client that navigated away — and a claim released
/// only on the return path would refuse every later attempt at that approval
/// with `AlreadySubmitted` while nothing at all was on the wire.
#[tokio::test]
async fn a_cancelled_answer_releases_its_claim_so_a_retry_is_accepted() {
    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_root", "bash", "ls");
    let lane = client(&fake);

    // Park the POST inside the fake, so "mid-request" is a condition the test
    // stands on rather than a race it hopes to win.
    fake.hold_post("/permission/per_root/reply");
    let inflight = {
        let lane = lane.clone();
        tokio::spawn(async move {
            lane.answer(
                "ses_a",
                "per_root",
                LaneAnswer::Permission {
                    decision: LaneDecision::AllowOnce,
                },
            )
            .await
        })
    };
    wait_for("the answer to reach the server", || {
        !fake.post_paths().is_empty()
    })
    .await;

    // Drop the future mid-request. Awaiting the handle is what proves the future
    // is GONE — and so that its guard has run — before the retry.
    inflight.abort();
    assert!(inflight
        .await
        .expect_err("the task was cancelled")
        .is_cancelled());

    fake.release_post("/permission/per_root/reply");
    lane.answer(
        "ses_a",
        "per_root",
        LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce,
        },
    )
    .await
    .expect("the retry after a cancelled answer is accepted");

    assert_eq!(
        fake.post_paths(),
        vec!["/permission/per_root/reply", "/permission/per_root/reply"],
        "the retry is a SECOND attempt, not a resend of the cancelled one"
    );
    assert_clean(&fake);
}

// ---------------------------------------------------------------------------
// The answer lookup: every LaneAnswer shape × the addressed kind × where the
// addressed request actually lives.
// ---------------------------------------------------------------------------

/// Which of opencode's two answer routes an answer shape lands on when it is
/// pointed at an approval of the kind it is FOR.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Posts {
    PermissionReply,
    QuestionReply,
    QuestionReject,
}

impl Posts {
    fn path(self, id: &str) -> String {
        match self {
            Posts::PermissionReply => format!("/permission/{id}/reply"),
            Posts::QuestionReply => format!("/question/{id}/reply"),
            Posts::QuestionReject => format!("/question/{id}/reject"),
        }
    }
}

/// One answer shape: the [`LaneAnswer`] a client sends, the approval KIND it can
/// legitimately answer, and the exact wire call it makes when it is pointed at
/// one of that kind.
struct Shape {
    label: &'static str,
    for_kind: LaneApprovalKind,
    answer: LaneAnswer,
    posts: Posts,
    body: &'static str,
}

/// Every variant of [`LaneAnswer`], with the three permission decisions and both
/// `Choice` directions spelled out — the rows of the matrix.
fn shapes() -> Vec<Shape> {
    let permission = |label, decision, body| Shape {
        label,
        for_kind: LaneApprovalKind::Permission,
        answer: LaneAnswer::Permission { decision },
        posts: Posts::PermissionReply,
        body,
    };
    let choice = |label, id: &str, body| Shape {
        label,
        for_kind: LaneApprovalKind::Permission,
        answer: LaneAnswer::Choice {
            option_id: id.to_string(),
        },
        posts: Posts::PermissionReply,
        body,
    };
    vec![
        permission(
            "permission{allow_once}",
            LaneDecision::AllowOnce,
            r#"{"reply":"once"}"#,
        ),
        permission(
            "permission{allow_always}",
            LaneDecision::AllowAlways,
            r#"{"reply":"always"}"#,
        ),
        permission(
            "permission{reject}",
            LaneDecision::Reject,
            r#"{"reply":"reject"}"#,
        ),
        // The option ids are opencode's own (`permission_options`), and the
        // decision comes from each option's ACP `kind` — `reject`'s kind is
        // `reject_once`, so an id-sniffing resolver would miss it.
        choice("choice{allow_once}", "allow_once", r#"{"reply":"once"}"#),
        choice("choice{reject}", "reject", r#"{"reply":"reject"}"#),
        Shape {
            label: "question",
            for_kind: LaneApprovalKind::Question,
            answer: LaneAnswer::Question {
                answers: vec![vec!["left".to_string()]],
            },
            posts: Posts::QuestionReply,
            body: r#"{"answers":[["left"]]}"#,
        },
        Shape {
            label: "reject",
            for_kind: LaneApprovalKind::Question,
            answer: LaneAnswer::Reject,
            posts: Posts::QuestionReject,
            body: "{}",
        },
        Shape {
            label: "raw",
            for_kind: LaneApprovalKind::Permission,
            answer: LaneAnswer::Raw {
                json: r#"{"reply":"always"}"#.to_string(),
            },
            posts: Posts::PermissionReply,
            body: r#"{"reply":"always"}"#,
        },
    ]
}

/// Where the addressed request lives, as far as the two DIRECTORY-WIDE lists the
/// lookup reads are concerned.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Placement {
    /// Open on the subscribed root.
    Root,
    /// Open on a DESCENDANT of the root — a child's approval blocks the same
    /// agent, so it is answerable from the root's panel.
    Child,
    /// Open on a SIBLING root in the same directory. It IS in the lists, and it
    /// must not be answerable from this session's panel.
    Sibling,
    /// In neither list.
    Absent,
    /// In BOTH lists, under one id.
    Both,
    /// Open on the root, but the `/permission` read fails.
    PermissionListDown,
    /// Open on the root, but the `/question` read fails.
    QuestionListDown,
}

/// What the answer must do.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Expect {
    /// One POST, on this path, with this body.
    Posted(String, &'static str),
    /// [`LaneError::UnknownApproval`], and NO POST.
    Unknown,
    /// A [`LaneError::BadRequest`] whose message contains ALL of these, and NO
    /// POST.
    Refused(Vec<String>),
    /// The failed list read's OWN error, naming that route, and NO POST.
    Propagated(&'static str),
}

/// The lookup's world: a root, a DESCENDANT of it, and a SIBLING root — all in
/// ONE directory, which is what puts the sibling's requests in the very lists
/// the lookup has to read. Both lists always carry a decoy, so resolving is
/// never "the only entry there is".
///
/// Returns the fake and the id the answer will address.
async fn placed(placement: Placement, kind: &LaneApprovalKind) -> (FakeOpencode, String) {
    let fake = three_sessions().await;
    fake.add_permission("ses_a", "per_decoy", "bash", "ls");
    fake.add_question("ses_a", "que_decoy", "Pick", "Which?", &["left"]);
    fake.add_permission("ses_sib", "per_sib_decoy", "bash", "rm -rf /");
    fake.add_question("ses_sib", "que_sib_decoy", "Pick", "Which?", &["left"]);

    let question = *kind == LaneApprovalKind::Question;
    let id = if question { "que_target" } else { "per_target" };
    let add = |session: &str, id: &str| {
        if question {
            fake.add_question(session, id, "Pick", "Which?", &["left", "right"]);
        } else {
            fake.add_permission(session, id, "bash", "ls");
        }
    };
    match placement {
        Placement::Root => add("ses_a", id),
        Placement::Child => add("ses_child", id),
        Placement::Sibling => add("ses_sib", id),
        // Nothing added: the id is in neither list. The decoys are, so the
        // lookup is reading a populated directory and still not finding it.
        Placement::Absent => {}
        // One id, tracked under BOTH kinds. opencode prefixes its request ids
        // `per_`/`que_` so this does not arise in practice, but it is
        // representable and the fold refuses to guess a kind for it.
        Placement::Both => {
            fake.add_permission("ses_a", "amb_target", "bash", "ls");
            fake.add_question("ses_a", "amb_target", "Pick", "Which?", &["left"]);
        }
        Placement::PermissionListDown => {
            add("ses_a", id);
            fake.fail_get("/permission", 500);
        }
        Placement::QuestionListDown => {
            add("ses_a", id);
            fake.fail_get("/question", 500);
        }
    }
    let addressed = match placement {
        Placement::Both => "amb_target".to_string(),
        _ => id.to_string(),
    };
    (fake, addressed)
}

/// Assert one cell: the RESULT, and whether a POST happened.
fn check(case: &str, got: Result<(), LaneError>, expect: &Expect, fake: &FakeOpencode) {
    match expect {
        Expect::Posted(path, body) => {
            got.unwrap_or_else(|e| panic!("{case}: expected the answer to land, got {e:?}"));
            assert_eq!(
                fake.post_paths(),
                vec![path.clone()],
                "{case}: the ONE post"
            );
            assert_eq!(fake.post_body(path).as_deref(), Some(*body), "{case}: body");
        }
        Expect::Unknown => {
            assert_eq!(
                got.expect_err(&format!("{case}: expected a refusal")),
                LaneError::UnknownApproval,
                "{case}"
            );
            assert!(
                fake.post_paths().is_empty(),
                "{case}: NOTHING may reach the wire, got {:?}",
                fake.post_paths()
            );
        }
        Expect::Refused(needles) => {
            let err = got.expect_err(&format!("{case}: expected a refusal"));
            match &err {
                LaneError::BadRequest(msg) => {
                    for needle in needles {
                        assert!(
                            msg.contains(needle.as_str()),
                            "{case}: {msg:?} does not say {needle:?}"
                        );
                    }
                }
                other => panic!("{case}: expected a BadRequest, got {other:?}"),
            }
            assert!(
                fake.post_paths().is_empty(),
                "{case}: NOTHING may reach the wire, got {:?}",
                fake.post_paths()
            );
        }
        Expect::Propagated(route) => {
            let err = got.expect_err(&format!("{case}: expected the read's error"));
            match &err {
                LaneError::Failed(msg) => assert!(
                    msg.contains(route),
                    "{case}: {msg:?} does not name the failed {route} read"
                ),
                other => panic!("{case}: expected the list read's Failed, got {other:?}"),
            }
            assert!(
                fake.post_paths().is_empty(),
                "{case}: a half-read directory decides NOTHING, got {:?}",
                fake.post_paths()
            );
        }
    }
    assert_clean(fake);
}

/// The whole answer contract as one table: every [`LaneAnswer`] shape × the kind
/// of the approval it is pointed at × where that approval lives.
///
/// The load-bearing rows are the SIBLING ones. opencode has no by-id GET, so the
/// lookup is the two directory-wide lists — and a directory holds unrelated
/// roots, whose approvals are in those same lists. Every sibling row addresses a
/// request that IS open and IS listed, and every one of them must refuse with no
/// POST at all: an unscoped lookup would resolve it, translate the answer
/// against the sibling's options, and post it to a global id-addressed route
/// that has no idea whose panel asked.
#[tokio::test]
async fn the_answer_matrix_resolves_the_approval_it_was_addressed_to() {
    for shape in shapes() {
        for kind in [LaneApprovalKind::Permission, LaneApprovalKind::Question] {
            let right_kind = shape.for_kind == kind;
            for placement in [
                Placement::Root,
                Placement::Child,
                Placement::Sibling,
                Placement::Absent,
                Placement::Both,
                Placement::PermissionListDown,
                Placement::QuestionListDown,
            ] {
                // The sibling row is only meaningful for the shape's OWN kind:
                // pointed at the other kind it would be refused on the shape
                // alone, which is not what that row is proving.
                if placement == Placement::Sibling && !right_kind {
                    continue;
                }
                // The ambiguous id has no kind of its own — it is BOTH — so the
                // second pass would be the identical case. It is swept once per
                // shape rather than twice for nothing. The two list-down rows
                // ARE run for both kinds on purpose: with the addressed request
                // in the SURVIVING half, a tolerant lookup would answer it, and
                // that is the row that catches one.
                if matches!(placement, Placement::Both) && kind == LaneApprovalKind::Question {
                    continue;
                }
                let (fake, id) = placed(placement, &kind).await;
                let expect = match placement {
                    Placement::Absent | Placement::Sibling => Expect::Unknown,
                    Placement::Both => Expect::Refused(vec!["ambiguous approval id".to_string()]),
                    Placement::PermissionListDown => Expect::Propagated("/permission"),
                    Placement::QuestionListDown => Expect::Propagated("/question"),
                    Placement::Root | Placement::Child if right_kind => {
                        Expect::Posted(shape.posts.path(&id), shape.body)
                    }
                    // The mismatch refusal names the id AND the kind that was
                    // actually resolved — both, because a client that sent the
                    // wrong variant needs both to fix it.
                    Placement::Root | Placement::Child => {
                        Expect::Refused(vec![id.clone(), kind.as_str().to_string()])
                    }
                };
                let case = format!("{} × {} × {placement:?}", shape.label, kind.as_str());
                let got = client(&fake)
                    .answer("ses_a", &id, shape.answer.clone())
                    .await;
                check(&case, got, &expect, &fake);
            }
        }
    }
}

/// The rows the matrix cannot express: an answer of the RIGHT shape for the
/// resolved kind that is still not answerable — an option this approval never
/// offered, and a `raw` body that is not JSON. Both are refused before the wire,
/// like every other mismatch.
#[tokio::test]
async fn an_answer_of_the_right_shape_can_still_name_something_that_was_not_offered() {
    // An id opencode's permissions never offer.
    let (fake, id) = placed(Placement::Root, &LaneApprovalKind::Permission).await;
    check(
        "choice{nope} × permission",
        client(&fake)
            .answer(
                "ses_a",
                &id,
                LaneAnswer::Choice {
                    option_id: "nope".to_string(),
                },
            )
            .await,
        &Expect::Refused(vec![format!("did not offer the option \"nope\" on {id}")]),
        &fake,
    );

    // A QUESTION's own option label, sent as a `Choice`. It is a real offered
    // id — on the question's inner form, which is answered positionally through
    // `LaneAnswer::Question` — so the refusal is about the KIND, not the id.
    let (fake, id) = placed(Placement::Root, &LaneApprovalKind::Question).await;
    check(
        "choice{left} × question",
        client(&fake)
            .answer(
                "ses_a",
                &id,
                LaneAnswer::Choice {
                    option_id: "left".to_string(),
                },
            )
            .await,
        &Expect::Refused(vec![format!(
            "{id} is a question; answer it with `question`"
        )]),
        &fake,
    );

    // `Raw` on a resolved permission is allowed — but the body still has to be
    // JSON, and that check stays where it was: after the lookup, before the
    // POST.
    let (fake, id) = placed(Placement::Root, &LaneApprovalKind::Permission).await;
    check(
        "raw{not json} × permission",
        client(&fake)
            .answer(
                "ses_a",
                &id,
                LaneAnswer::Raw {
                    json: "not json".to_string(),
                },
            )
            .await,
        &Expect::Refused(vec!["raw answer is not JSON".to_string()]),
        &fake,
    );
}

/// The moved shared pieces resolve at BOTH paths, from OUTSIDE the crate.
///
/// `MessageRing`, the feed vocabulary and the sanitizers now live in
/// `shed_core::lane::{ring,feed}`; every one of them is still reachable at the
/// `shed_opencode::*` path a consumer (or a lagging call site) already spells,
/// and each is the SAME item rather than a second copy — a ring built through
/// this crate's re-export is accepted where the contract crate's type is asked
/// for, which would not compile if the two had forked.
#[test]
fn the_shared_ring_and_feed_helpers_resolve_at_both_paths() {
    fn core_seq(r: &mut shed_core::lane::ring::MessageRing) -> u64 {
        r.append(shed_core::rc::RcFeedMessage::default(), 1_700_000_000_000)
            .seq
    }

    // The crate-root re-export (`shed_opencode::MessageRing`) and the module
    // one (`shed_opencode::ring::MessageRing`) are both shed-core's type.
    let mut root: shed_opencode::MessageRing = shed_opencode::MessageRing::new();
    let mut module: shed_opencode::ring::MessageRing = shed_opencode::ring::MessageRing::new();
    assert_eq!(core_seq(&mut root), 1);
    assert_eq!(core_seq(&mut module), 1);

    // The ring's caps came along unchanged, at both paths.
    assert_eq!(
        shed_opencode::ring::MAX_RING_MESSAGES,
        shed_core::lane::ring::MAX_RING_MESSAGES
    );
    assert_eq!(
        shed_opencode::ring::MAX_MESSAGES_LIMIT,
        shed_core::lane::ring::MAX_MESSAGES_LIMIT
    );

    // And the feed half is reachable from the contract crate by any consumer —
    // which is the whole reason it moved: `shed-gx` must never `use
    // shed_opencode::…` for a sanitizer.
    assert_eq!(shed_core::lane::feed::FEED_ROLE_ASSISTANT, "assistant");
    assert_eq!(
        shed_core::lane::feed::sanitize_feed_text("\u{1b}[31mred\u{1b}[0m\nkept"),
        "red\nkept"
    );
    assert_eq!(shed_core::lane::feed::bound_token("pend\ning"), "pending");
    assert_eq!(
        shed_core::lane::backoff::next_backoff(
            Duration::from_millis(500),
            false,
            Duration::from_millis(500),
            Duration::from_secs(30)
        ),
        Duration::from_secs(1)
    );
}
