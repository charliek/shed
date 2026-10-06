//! The lane's pure tables: the answer mapping (§3.3.6, "Answers"), row by
//! row, and the capability mapping (§3.3.9).

use super::*;
use serde_json::json;
use shed_core::lane::{LaneApprovalOption, LaneApprovalStatus, LaneQuestion};

fn opt(id: &str, label: &str, kind: Option<&str>) -> LaneApprovalOption {
    LaneApprovalOption {
        id: id.into(),
        label: label.into(),
        description: None,
        kind: kind.map(str::to_string),
    }
}

fn approval(
    kind: LaneApprovalKind,
    options: Vec<LaneApprovalOption>,
    questions: Vec<LaneQuestion>,
) -> LaneApproval {
    LaneApproval {
        id: "a".into(),
        session_id: "0123456789ab".into(),
        kind,
        status: LaneApprovalStatus::Pending,
        title: "t".into(),
        detail: None,
        options,
        questions,
        request_json: "{}".into(),
        created_at_unix_ms: None,
    }
}

fn permission() -> LaneApproval {
    approval(
        LaneApprovalKind::Permission,
        vec![
            opt("p-1", "Yes", Some("allow_once")),
            opt("p-2", "Yes, never ask", Some("allow_once")),
            opt("p-3", "Always", Some("allow_always")),
            opt("p-4", "No", Some("reject_always")),
        ],
        vec![],
    )
}

fn q(id: &str, options: Vec<LaneApprovalOption>) -> LaneQuestion {
    LaneQuestion {
        id: Some(id.into()),
        header: String::new(),
        question: format!("{id}?"),
        options,
        multiple: true,
        custom: false,
    }
}

fn question() -> LaneApproval {
    approval(
        LaneApprovalKind::Question,
        vec![],
        vec![
            q("lang", vec![opt("rs", "Rust", None), opt("go", "Go", None)]),
            q(
                "db",
                vec![opt("pg", "Postgres", None), opt("my", "Postgres", None)],
            ),
        ],
    )
}

fn plan() -> LaneApproval {
    approval(
        LaneApprovalKind::PlanApproval,
        vec![
            opt("accept", "Accept plan", Some("allow_once")),
            opt("reject", "Reject plan", Some("reject_once")),
        ],
        vec![],
    )
}

fn bad(r: Result<Value, LaneError>) -> bool {
    r.is_err_and(|e| matches!(e, LaneError::BadRequest(_)))
}

/// Permission: the offered id verbatim; a decision through `option_for`
/// (ambiguity refused, naming craze); `Reject` cancels; `Raw` verbatim.
#[test]
fn the_permission_rows() {
    let p = permission();
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Choice {
                option_id: "p-2".into()
            }
        )
        .unwrap(),
        json!({"optionId": "p-2"})
    );
    assert!(
        bad(answer_body(
            &p,
            &LaneAnswer::Choice {
                option_id: "p-9".into()
            }
        )),
        "an id not offered"
    );
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Permission {
                decision: LaneDecision::AllowAlways
            }
        )
        .unwrap(),
        json!({"optionId": "p-3"})
    );
    let amb = answer_body(
        &p,
        &LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce,
        },
    );
    assert!(
        matches!(&amb, Err(LaneError::BadRequest(m)) if m.contains("craze")),
        "two allow_once: refused, naming craze: {amb:?}"
    );
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Permission {
                decision: LaneDecision::Reject
            }
        )
        .unwrap(),
        json!({"optionId": "p-4"}),
        "reject falls back to the first reject-kind option"
    );
    assert_eq!(
        answer_body(&p, &LaneAnswer::Reject).unwrap(),
        json!({"cancel": true})
    );
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Raw {
                json: r#"{"optionId":"x"}"#.into()
            }
        )
        .unwrap(),
        json!({"optionId": "x"})
    );
    assert!(bad(answer_body(
        &p,
        &LaneAnswer::Raw { json: "[1]".into() }
    )));
    assert!(bad(answer_body(
        &p,
        &LaneAnswer::Question {
            answers: vec![],
            custom_text: vec![]
        }
    )));
}

/// Question: `{answers: {<id>: [<option id>…]}}` positionally, each value by
/// id then by label (exactly one), free text refused, `Reject` skips, zero
/// questions `{}`.
#[test]
fn the_question_rows() {
    let qa = question();
    let ans = |a: Vec<Vec<&str>>, t: Vec<Option<&str>>| LaneAnswer::Question {
        answers: a
            .into_iter()
            .map(|v| v.into_iter().map(str::to_string).collect())
            .collect(),
        custom_text: t.into_iter().map(|t| t.map(str::to_string)).collect(),
    };
    assert_eq!(
        answer_body(&qa, &ans(vec![vec!["Rust", "go"], vec!["pg"]], vec![])).unwrap(),
        json!({"answers": {"lang": ["rs", "go"], "db": ["pg"]}}),
        "by label, then by id"
    );
    assert_eq!(
        answer_body(&qa, &ans(vec![vec!["rs"]], vec![])).unwrap(),
        json!({"answers": {"lang": ["rs"], "db": []}}),
        "an unanswered question is its own empty list"
    );
    assert!(
        bad(answer_body(
            &qa,
            &ans(vec![vec![], vec!["Postgres"]], vec![])
        )),
        "two options labelled Postgres"
    );
    assert!(
        bad(answer_body(&qa, &ans(vec![vec!["Cobol"]], vec![]))),
        "nothing offered"
    );
    assert!(
        bad(answer_body(
            &qa,
            &ans(vec![vec!["rs"]], vec![Some("also Zig")])
        )),
        "craze questions take no free text"
    );
    assert!(
        bad(answer_body(&qa, &ans(vec![vec![], vec![], vec![]], vec![]))),
        "more answers than questions"
    );
    assert_eq!(
        answer_body(&qa, &LaneAnswer::Reject).unwrap(),
        json!({"skip": true})
    );
    let empty = approval(LaneApprovalKind::Question, vec![], vec![]);
    assert_eq!(
        answer_body(&empty, &ans(vec![], vec![])).unwrap(),
        json!({}),
        "a question that asks nothing takes {{}}"
    );
    assert!(bad(answer_body(
        &qa,
        &LaneAnswer::Choice {
            option_id: "rs".into()
        }
    )));
}

/// Plan: accept, reject, and nothing else.
#[test]
fn the_plan_rows() {
    let p = plan();
    let accept = json!({"accept": true});
    let reject = json!({"reject": true});
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Choice {
                option_id: "accept".into()
            }
        )
        .unwrap(),
        accept
    );
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce
            }
        )
        .unwrap(),
        accept
    );
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Choice {
                option_id: "reject".into()
            }
        )
        .unwrap(),
        reject
    );
    assert_eq!(
        answer_body(
            &p,
            &LaneAnswer::Permission {
                decision: LaneDecision::Reject
            }
        )
        .unwrap(),
        reject
    );
    assert_eq!(answer_body(&p, &LaneAnswer::Reject).unwrap(), reject);
    assert!(bad(answer_body(
        &p,
        &LaneAnswer::Permission {
            decision: LaneDecision::AllowAlways
        }
    )));
    assert!(bad(answer_body(
        &p,
        &LaneAnswer::Choice {
            option_id: "maybe".into()
        }
    )));
}

/// §3.9: `approvals` is craze's own session capability, never the cards;
/// `history_cursor` always; the rest as the session states them.
#[test]
fn the_capability_mapping() {
    let caps = |v: Value| -> SessionCapabilities { serde_json::from_value(v).unwrap() };
    let c = lane_capabilities(
        &caps(
            json!({"interject": true, "cancel": true, "askCards": false, "planCards": true,
                   "approvals": true, "stop": true}),
        ),
        true,
    );
    assert_eq!(
        c,
        LaneCapabilities {
            kind: "craze".into(),
            interject: true,
            cancel: true,
            approvals: true,
            history_cursor: true,
            settings: true,
            stop: true
        }
    );
    let c = lane_capabilities(&caps(json!({})), false);
    assert!(
        !c.interject && !c.cancel && !c.approvals && !c.stop && !c.settings && c.history_cursor
    );
    // Neither card, `approvals` stated: approvals — a permission is never
    // hidden (A11 item 5), so a session with no cards still raises one.
    let c = lane_capabilities(
        &caps(json!({"askCards": false, "planCards": false, "approvals": true})),
        false,
    );
    assert!(c.approvals, "craze's own approvals, not the cards");
    // …and the cards alone never make it so.
    let c = lane_capabilities(
        &caps(json!({"askCards": true, "planCards": true, "approvals": false})),
        false,
    );
    assert!(!c.approvals);
}

/// commandIds are canonical decimals from 1, per lane, never repeated.
#[test]
fn command_ids_count_from_one() {
    let (dial, _conns) = crate::testing::ScriptedDial::new();
    let lane = CrazeLane::new(
        dial,
        ClientInfo::shed("t"),
        "0123456789ab",
        None,
        LaneTimings::default(),
    );
    let ids: Vec<String> = (0..3).map(|_| lane.shared.command_id()).collect();
    assert_eq!(ids, ["1", "2", "3"]);
}
