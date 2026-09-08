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
