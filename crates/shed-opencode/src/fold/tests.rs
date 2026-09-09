//! Hand-authored event **inputs** for the paths `opencode_turn.jsonl` cannot
//! reach.
//!
//! The fixture was recorded from opencode 1.17.15 on a plain tool-using turn:
//! it carries no `permission.asked`, no `question.asked` and no `session.error`,
//! and it exercises neither `note_gap` nor `reset`. Everything here is an
//! INPUT written by hand against opencode's published event schemas
//! (`packages/schema/src/v1/{permission,question}.ts`), not a recording — no
//! claim is made that a live opencode emitted these bytes verbatim. The
//! recorded-turn behavior is pinned separately, by
//! `tests/fold_fixtures.rs` against `fixtures/opencode_turn.golden.json`.
//!
//! Every case asserts BOTH halves of the fold's output: the feed rows and the
//! verdict (plus, where the point is the contract, the [`LaneApproval`] state).

use super::*;

// ---- input builders ----

fn line(v: &serde_json::Value) -> Vec<u8> {
    serde_json::to_vec(v).expect("test input serializes")
}

fn permission_asked(id: &str, session: &str) -> Vec<u8> {
    line(&serde_json::json!({
        "type": "permission.asked",
        "properties": {
            "id": id,
            "sessionID": session,
            "permission": "bash",
            "patterns": ["ls *", "cat *"],
            "metadata": {"command": "ls -la"},
        }
    }))
}

fn permission_replied(id: &str, reply: &str) -> Vec<u8> {
    line(&serde_json::json!({
        "type": "permission.replied",
        "properties": {"sessionID": "ses_root", "requestID": id, "reply": reply}
    }))
}

fn question_asked(id: &str, session: &str) -> Vec<u8> {
    line(&serde_json::json!({
        "type": "question.asked",
        "properties": {
            "id": id,
            "sessionID": session,
            "questions": [{
                "header": "Pick a branch",
                "question": "Which branch should I target?",
                "options": [
                    {"label": "main", "description": "the trunk"},
                    {"label": "dev", "description": ""}
                ],
                "multiple": false,
                "custom": true
            }]
        }
    }))
}

/// A `question.replied` / `question.rejected` closing `id`.
fn question_closed(typ: &str, id: &str) -> Vec<u8> {
    line(&serde_json::json!({
        "type": typ,
        "properties": {"sessionID": "ses_root", "requestID": id, "answers": [["main"]]}
    }))
}

fn session_status(kind: &str) -> Vec<u8> {
    line(&serde_json::json!({
        "type": "session.status",
        "properties": {"sessionID": "ses_root", "status": {"type": kind}}
    }))
}

/// A tool part in one state, with the input that makes a `tool_use` row emit.
fn tool_part(call: &str, status: &str) -> Vec<u8> {
    line(&serde_json::json!({
        "type": "message.part.updated",
        "properties": {
            "sessionID": "ses_root",
            "time": 1_700_000_000_000i64,
            "part": {
                "id": "prt_1", "messageID": "msg_1", "type": "tool",
                "tool": "bash", "callID": call,
                "state": {"status": status, "input": {"command": "ls"}, "output": "a.txt"}
            }
        }
    }))
}

// ---- assertions ----

/// One expected row, in the shape the assertions read it.
#[derive(Debug, PartialEq, Eq)]
struct Row {
    role: &'static str,
    typ: &'static str,
    text: String,
    tool: Option<(String, String)>,
    approval: Option<(String, String, String, Vec<String>)>,
}

fn rows(f: &mut OpencodeFold) -> Vec<Row> {
    f.drain_messages()
        .into_iter()
        .map(|m| Row {
            role: match m.role.as_str() {
                "user" => "user",
                "assistant" => "assistant",
                "tool" => "tool",
                "system" => "system",
                other => panic!("unexpected role {other:?}"),
            },
            typ: match m.msg_type.as_str() {
                "text" => "text",
                "reasoning" => "reasoning",
                "tool_use" => "tool_use",
                "tool_result" => "tool_result",
                "status" => "status",
                "approval_request" => "approval_request",
                other => panic!("unexpected type {other:?}"),
            },
            text: m.text.unwrap_or_default(),
            tool: m
                .tool
                .map(|t| (t.name.unwrap_or_default(), t.detail.unwrap_or_default())),
            approval: m
                .approval
                .map(|a| (a.id, a.status, a.decision.unwrap_or_default(), a.decisions)),
        })
        .collect()
}

fn status_row(text: &str) -> Row {
    Row {
        role: "system",
        typ: "status",
        text: text.to_string(),
        tool: None,
        approval: None,
    }
}

/// The PENDING `approval_request` row a tracked `permission_asked` emits.
fn permission_pending_row(id: &str) -> Row {
    Row {
        role: "tool",
        typ: "approval_request",
        text: "awaiting approval: bash — ls *, cat *".into(),
        tool: Some(("bash".into(), "ls -la".into())),
        approval: Some((
            id.into(),
            "pending".into(),
            String::new(),
            vec!["allow".into(), "allow_always".into(), "deny".into()],
        )),
    }
}

/// The RESOLVED `approval_request` row a permission resolution appends.
fn permission_resolved_row(id: &str, text: &str, decision: &str) -> Row {
    Row {
        role: "tool",
        typ: "approval_request",
        text: text.into(),
        tool: None,
        approval: Some((id.into(), "resolved".into(), decision.into(), Vec::new())),
    }
}

// ---- expected DTOs ----

/// The verbatim `properties` document of an input line — what the fold must
/// carry into [`LaneApproval::request_json`]. Round-tripped through `Value` so
/// the expectation is written the same way whatever key order serde_json uses.
fn props_json(input: &[u8]) -> String {
    let v: serde_json::Value = serde_json::from_slice(input).expect("test input parses");
    serde_json::to_string(&v["properties"]).expect("properties reserialize")
}

/// The full DTO `permission_asked(id, session)` must produce while OPEN.
fn expected_permission(id: &str, session: &str) -> LaneApproval {
    LaneApproval {
        id: id.into(),
        session_id: session.into(),
        kind: LaneApprovalKind::Permission,
        status: LaneApprovalStatus::Pending,
        title: "awaiting approval: bash — ls *, cat *".into(),
        detail: Some("ls -la".into()),
        options: [
            ("allow_once", "Allow once", "allow_once"),
            ("allow_always", "Always", "allow_always"),
            // The id is opencode's opaque `reject`; the KIND is the ACP
            // `reject_once`. Spelled out here rather than derived, so a future
            // edit that collapses the two fails this test.
            ("reject", "Reject", "reject_once"),
        ]
        .into_iter()
        .map(|(oid, label, kind)| LaneApprovalOption {
            id: oid.into(),
            label: label.into(),
            description: None,
            kind: Some(kind.into()),
        })
        .collect(),
        questions: Vec::new(),
        request_json: props_json(&permission_asked(id, session)),
        created_at_unix_ms: None,
    }
}

/// The full DTO `question_asked(id, session)` must produce while OPEN.
fn expected_question(id: &str, session: &str) -> LaneApproval {
    LaneApproval {
        id: id.into(),
        session_id: session.into(),
        kind: LaneApprovalKind::Question,
        status: LaneApprovalStatus::Pending,
        title: "awaiting answer: Pick a branch".into(),
        detail: None,
        options: Vec::new(),
        questions: vec![LaneQuestion {
            // Positional on opencode — the reply route takes a vec-of-vecs in
            // question order, so there is no key to publish.
            id: None,
            header: "Pick a branch".into(),
            question: "Which branch should I target?".into(),
            options: vec![
                LaneApprovalOption {
                    id: "main".into(),
                    label: "main".into(),
                    description: Some("the trunk".into()),
                    kind: None,
                },
                LaneApprovalOption {
                    id: "dev".into(),
                    label: "dev".into(),
                    description: None,
                    kind: None,
                },
            ],
            multiple: false,
            custom: true,
        }],
        request_json: props_json(&question_asked(id, session)),
        created_at_unix_ms: None,
    }
}

// ---- permissions ----

#[test]
fn permission_asked_opens_an_approval_and_emits_the_pending_row() {
    let mut f = OpencodeFold::new();
    assert!(f.apply_line(&permission_asked("per_1", "ses_root")));

    assert_eq!(
        rows(&mut f),
        vec![Row {
            role: "tool",
            typ: "approval_request",
            text: "awaiting approval: bash — ls *, cat *".into(),
            tool: Some(("bash".into(), "ls -la".into())),
            approval: Some((
                "per_1".into(),
                "pending".into(),
                String::new(),
                vec!["allow".into(), "allow_always".into(), "deny".into()],
            )),
        }]
    );
    // An open ask IS the verdict — it outranks the pending-tool arm and the
    // idle boundary alike.
    assert_eq!(f.activity(), RcActivity::NeedsApproval);
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(
        f.approval_state(LaneApprovalKind::Permission, "per_1"),
        Some(LaneApprovalStatus::Pending)
    );
    assert_eq!(
        f.approval_state(LaneApprovalKind::Permission, "per_nope"),
        None
    );
}

#[test]
fn permission_approval_renders_the_contract_dto() {
    let mut f = OpencodeFold::new();
    f.apply_line(&permission_asked("per_1", "ses_child"));

    let pending = f.pending_approvals();
    assert_eq!(pending.len(), 1);
    let a = &pending[0];
    assert_eq!(a.id, "per_1");
    // Attributed to the session that ASKED — which may be a descendant of the
    // one the client subscribed to.
    assert_eq!(a.session_id, "ses_child");
    assert_eq!(a.kind, LaneApprovalKind::Permission);
    assert_eq!(a.status, LaneApprovalStatus::Pending);
    assert_eq!(a.title, "awaiting approval: bash — ls *, cat *");
    assert_eq!(a.detail.as_deref(), Some("ls -la"));
    // `kind` selects: a permission fills `options` and leaves `questions` empty.
    assert_eq!(
        a.options.iter().map(|o| o.id.as_str()).collect::<Vec<_>>(),
        vec!["allow_once", "allow_always", "reject"]
    );
    assert!(a.questions.is_empty());
    // The ask's own payload, raw, for a client that cannot render the kind.
    let raw: serde_json::Value = serde_json::from_str(&a.request_json).expect("raw JSON");
    assert_eq!(raw["permission"], "bash");
    assert_eq!(
        *a,
        f.approval(LaneApprovalKind::Permission, "per_1")
            .expect("tracked")
    );
}

#[test]
fn permission_replied_closes_the_ask_and_maps_the_decision() {
    for (reply, decision) in [
        ("once", "allow"),
        ("always", "allow_always"),
        ("reject", "deny"),
        // An unrecognized reply still CLOSES the ask: a stuck entry would pin
        // needs_approval forever.
        ("teleport", ""),
    ] {
        let mut f = OpencodeFold::new();
        f.apply_line(&permission_asked("per_1", "ses_root"));
        let _ = rows(&mut f);

        assert!(f.apply_line(&permission_replied("per_1", reply)), "{reply}");
        assert_eq!(
            rows(&mut f),
            vec![Row {
                role: "tool",
                typ: "approval_request",
                text: "awaiting approval: bash — ls *, cat *".into(),
                tool: None,
                approval: Some((
                    "per_1".into(),
                    "resolved".into(),
                    decision.to_string(),
                    Vec::new(),
                )),
            }],
            "{reply}"
        );
        assert_eq!(f.open_approvals(), 0, "{reply}");
        assert!(f.pending_approvals().is_empty(), "{reply}");
        // Retained, not deleted: the answer verb must tell "never saw it" from
        // "already answered".
        assert_eq!(
            f.approval_state(LaneApprovalKind::Permission, "per_1"),
            Some(LaneApprovalStatus::Resolved),
            "{reply}"
        );
        // Nothing is blocked and no boundary was ever seen, so the verdict falls
        // back to working — never idle, which this fold cannot produce.
        assert_eq!(f.activity(), RcActivity::Working, "{reply}");
    }
}

#[test]
fn permission_replied_is_idempotent_and_tombstones_an_unseen_ask() {
    let mut f = OpencodeFold::new();
    // A reply for an ask this fold never saw: a resolved tombstone, so a later
    // ask replay cannot strand the session at needs_approval.
    assert!(f.apply_line(&permission_replied("per_ghost", "once")));
    assert_eq!(
        rows(&mut f),
        vec![Row {
            role: "tool",
            typ: "approval_request",
            text: "approval resolved".into(),
            tool: None,
            approval: Some((
                "per_ghost".into(),
                "resolved".into(),
                "allow".into(),
                Vec::new()
            )),
        }]
    );
    // The second reply for the same id is a no-op — exactly one resolved row.
    assert!(!f.apply_line(&permission_replied("per_ghost", "once")));
    assert!(rows(&mut f).is_empty());

    // …and the ask replay does NOT reopen it: a real reply was observed.
    assert!(!f.apply_line(&permission_asked("per_ghost", "ses_root")));
    assert!(rows(&mut f).is_empty());
    assert_eq!(f.open_approvals(), 0);
}

#[test]
fn an_id_less_permission_stays_display_only() {
    let mut f = OpencodeFold::new();
    let ask = line(&serde_json::json!({
        "type": "permission.asked",
        "properties": {"sessionID": "ses_root", "permission": "bash"}
    }));
    assert!(f.apply_line(&ask));
    assert_eq!(rows(&mut f), vec![status_row("awaiting approval: bash")]);
    // Nothing could ever answer or retire it, so it must not block the session
    // — nor even confirm the fold.
    assert_eq!(f.open_approvals(), 0);
    assert_eq!(f.activity(), RcActivity::Unknown);
    // Content-keyed dedup: a reseed replay does not stack up a second row.
    assert!(f.apply_line(&ask));
    assert!(rows(&mut f).is_empty());
}

#[test]
fn a_permission_ask_with_no_kind_fabricates_nothing() {
    let mut f = OpencodeFold::new();
    let hollow = line(&serde_json::json!({
        "type": "permission.asked",
        "properties": {"id": "per_1", "sessionID": "ses_root"}
    }));
    assert!(!f.apply_line(&hollow));
    assert!(rows(&mut f).is_empty());
    assert_eq!(f.open_approvals(), 0);
}

// ---- questions ----

#[test]
fn question_asked_emits_a_status_row_and_opens_an_addressable_approval() {
    let mut f = OpencodeFold::new();
    assert!(f.apply_line(&question_asked("que_1", "ses_root")));

    // The FEED row is the hub's, unchanged: display-only, not an
    // approval_request.
    assert_eq!(
        rows(&mut f),
        vec![status_row("awaiting answer: Pick a branch")]
    );
    assert_eq!(f.activity(), RcActivity::NeedsApproval);
    assert_eq!(f.open_approvals(), 1);

    // What the LANE adds: the question is addressable state, which the hub
    // could only count.
    let a = f
        .approval(LaneApprovalKind::Question, "que_1")
        .expect("tracked");
    assert_eq!(a.kind, LaneApprovalKind::Question);
    assert_eq!(a.status, LaneApprovalStatus::Pending);
    assert_eq!(a.session_id, "ses_root");
    assert_eq!(a.title, "awaiting answer: Pick a branch");
    // `kind` selects the other way round for a question.
    assert!(a.options.is_empty());
    assert_eq!(a.questions.len(), 1);
    let q = &a.questions[0];
    assert_eq!(q.header, "Pick a branch");
    assert_eq!(q.question, "Which branch should I target?");
    assert!(!q.multiple);
    assert!(q.custom);
    // opencode's QuestionOption has no id of its own, and its reply route wants
    // the LABELS back, so `id == label`.
    assert_eq!(
        q.options
            .iter()
            .map(|o| (o.id.as_str(), o.label.as_str(), o.description.as_deref()))
            .collect::<Vec<_>>(),
        vec![("main", "main", Some("the trunk")), ("dev", "dev", None),]
    );
    assert_eq!(f.pending_approvals(), vec![a]);
}

#[test]
fn question_replied_and_rejected_both_retire_the_question_without_a_row() {
    for typ in ["question.replied", "question.rejected"] {
        let mut f = OpencodeFold::new();
        f.apply_line(&question_asked("que_1", "ses_root"));
        let _ = rows(&mut f);

        let close = line(&serde_json::json!({
            "type": typ,
            "properties": {"sessionID": "ses_root", "requestID": "que_1", "answers": [["main"]]}
        }));
        assert!(f.apply_line(&close), "{typ}");
        // The ask's row was display-only; the resolution reaches a client as the
        // approval's own state, not as a second feed row.
        assert!(rows(&mut f).is_empty(), "{typ}");
        assert_eq!(f.open_approvals(), 0, "{typ}");
        assert!(f.pending_approvals().is_empty(), "{typ}");
        assert_eq!(
            f.approval_state(LaneApprovalKind::Question, "que_1"),
            Some(LaneApprovalStatus::Resolved),
            "{typ}"
        );
        assert_eq!(f.activity(), RcActivity::Working, "{typ}");
        // Idempotent.
        assert!(!f.apply_line(&close), "{typ}");
    }
}

#[test]
fn a_question_with_no_readable_header_fabricates_nothing() {
    let mut f = OpencodeFold::new();
    for props in [
        serde_json::json!({"id": "que_1", "sessionID": "ses_root", "questions": []}),
        serde_json::json!({"id": "que_1", "sessionID": "ses_root",
                           "questions": [{"header": "", "question": "", "options": []}]}),
    ] {
        let hollow = line(&serde_json::json!({"type": "question.asked", "properties": props}));
        assert!(!f.apply_line(&hollow));
        assert!(rows(&mut f).is_empty());
    }
    assert_eq!(f.open_approvals(), 0);
    assert_eq!(f.activity(), RcActivity::Unknown);
}

// ---- (kind, id) isolation ----
//
// opencode's request ids are unique only WITHIN a kind. The hub got that
// isolation for free from two maps (`pending_perms` + `pending_questions`); this
// fold holds ONE map and must get it from the compound key. Every case here is
// written against the hub's behavior on the same frames.

#[test]
fn a_permission_and_a_question_sharing_an_id_are_two_independent_approvals() {
    // Either arrival order: neither ask may overwrite or be discarded by the
    // other, which an id-only key would do in one direction or the other.
    for perm_first in [true, false] {
        let ctx = format!("perm_first={perm_first}");
        let mut f = OpencodeFold::new();
        if perm_first {
            assert!(f.apply_line(&permission_asked("x", "ses_root")), "{ctx}");
            assert!(f.apply_line(&question_asked("x", "ses_child")), "{ctx}");
        } else {
            assert!(f.apply_line(&question_asked("x", "ses_child")), "{ctx}");
            assert!(f.apply_line(&permission_asked("x", "ses_root")), "{ctx}");
        }

        assert_eq!(f.open_approvals(), 2, "{ctx}");
        assert_eq!(
            f.approval(LaneApprovalKind::Permission, "x"),
            Some(expected_permission("x", "ses_root")),
            "{ctx}"
        );
        assert_eq!(
            f.approval(LaneApprovalKind::Question, "x"),
            Some(expected_question("x", "ses_child")),
            "{ctx}"
        );
        // The snapshot carries both, in ASK order.
        let expected = if perm_first {
            vec![
                expected_permission("x", "ses_root"),
                expected_question("x", "ses_child"),
            ]
        } else {
            vec![
                expected_question("x", "ses_child"),
                expected_permission("x", "ses_root"),
            ]
        };
        assert_eq!(f.pending_approvals(), expected, "{ctx}");
        // One row per ask: the permission's approval_request and the question's
        // display-only status row.
        assert_eq!(rows(&mut f).len(), 2, "{ctx}");

        // A reply to ONE of them leaves the other blocking the session.
        assert!(f.apply_line(&permission_replied("x", "once")), "{ctx}");
        assert_eq!(
            f.approval_state(LaneApprovalKind::Permission, "x"),
            Some(LaneApprovalStatus::Resolved),
            "{ctx}"
        );
        assert_eq!(
            f.approval_state(LaneApprovalKind::Question, "x"),
            Some(LaneApprovalStatus::Pending),
            "{ctx}"
        );
        assert_eq!(f.open_approvals(), 1, "{ctx}");
        assert_eq!(f.activity(), RcActivity::NeedsApproval, "{ctx}");
        assert_eq!(
            f.pending_approvals(),
            vec![expected_question("x", "ses_child")],
            "{ctx}"
        );
    }
}

#[test]
fn a_permission_reply_never_touches_a_question_of_the_same_id() {
    let mut f = OpencodeFold::new();
    assert!(f.apply_line(&question_asked("x", "ses_root")));
    let _ = rows(&mut f);

    // The reply addresses a PERMISSION the fold never saw asked, so it mints a
    // permission tombstone — exactly what the hub did — and the question stays
    // open. An id-only key would have closed the question instead.
    assert!(f.apply_line(&permission_replied("x", "once")));
    assert_eq!(
        rows(&mut f),
        vec![permission_resolved_row("x", "approval resolved", "allow")]
    );
    assert_eq!(
        f.approval_state(LaneApprovalKind::Question, "x"),
        Some(LaneApprovalStatus::Pending)
    );
    assert_eq!(
        f.approval_state(LaneApprovalKind::Permission, "x"),
        Some(LaneApprovalStatus::Resolved)
    );
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(f.activity(), RcActivity::NeedsApproval);

    // …and the question, never closed, is still the same open approval a third
    // frame re-asks. (An id-only key could not reopen it here: the tombstone's
    // decision is non-empty.)
    assert!(f.apply_line(&question_asked("x", "ses_root")));
    assert_eq!(
        f.pending_approvals(),
        vec![expected_question("x", "ses_root")]
    );
    assert_eq!(f.activity(), RcActivity::NeedsApproval);
    assert!(rows(&mut f).is_empty(), "the status row is announce-once");
}

#[test]
fn a_question_reply_never_touches_a_permission_of_the_same_id() {
    for typ in ["question.replied", "question.rejected"] {
        let mut f = OpencodeFold::new();
        assert!(f.apply_line(&permission_asked("x", "ses_root")), "{typ}");
        let _ = rows(&mut f);

        // Nothing of that kind is tracked, so the close is a no-op — and unlike
        // a permission reply it leaves no tombstone, as in the hub (which simply
        // removed a question from its map and had no question tombstone at all).
        assert!(!f.apply_line(&question_closed(typ, "x")), "{typ}");
        assert!(rows(&mut f).is_empty(), "{typ}");
        assert_eq!(
            f.approval_state(LaneApprovalKind::Question, "x"),
            None,
            "{typ}"
        );
        assert_eq!(
            f.approval_state(LaneApprovalKind::Permission, "x"),
            Some(LaneApprovalStatus::Pending),
            "{typ}"
        );
        assert_eq!(f.open_approvals(), 1, "{typ}");
        assert_eq!(f.activity(), RcActivity::NeedsApproval, "{typ}");
        assert_eq!(
            f.pending_approvals(),
            vec![expected_permission("x", "ses_root")],
            "{typ}"
        );
    }
}

#[test]
fn approvals_for_id_reports_every_kind_and_picks_none() {
    let mut f = OpencodeFold::new();
    assert!(f.approvals_for_id("x").is_empty());

    f.apply_line(&permission_asked("x", "ses_root"));
    f.apply_line(&question_asked("x", "ses_root"));
    // A caller holding only a bare id (a Reject/Raw answer names no kind) gets
    // BOTH and decides; the fold refuses to guess.
    assert_eq!(
        f.approvals_for_id("x"),
        vec![
            expected_permission("x", "ses_root"),
            expected_question("x", "ses_root")
        ]
    );

    // Resolved entries are reported too — telling "already answered" from "never
    // saw it" is the whole reason the tombstone is retained.
    f.apply_line(&permission_replied("x", "once"));
    let all = f.approvals_for_id("x");
    assert_eq!(all.len(), 2);
    assert_eq!(all[0].status, LaneApprovalStatus::Resolved);
    assert_eq!(all[1].status, LaneApprovalStatus::Pending);
}

// ---- resolution keeps a tombstone, not a payload ----

#[test]
fn a_resolved_approval_keeps_its_tombstone_and_sheds_its_payload() {
    let mut f = OpencodeFold::new();
    f.apply_line(&permission_asked("per_1", "ses_root"));
    f.apply_line(&question_asked("que_1", "ses_root"));
    let _ = rows(&mut f);
    // While OPEN both carry their raw ask.
    assert_eq!(
        f.approval(LaneApprovalKind::Permission, "per_1")
            .expect("open")
            .request_json,
        props_json(&permission_asked("per_1", "ses_root"))
    );
    assert_eq!(
        f.approval(LaneApprovalKind::Question, "que_1")
            .expect("open")
            .questions
            .len(),
        1
    );

    f.apply_line(&permission_replied("per_1", "once"));
    f.apply_line(&question_closed("question.replied", "que_1"));

    // Resolved: the lightweight tombstone the answer verb needs stays; the
    // structured questions and the raw request — which nothing reads on an
    // answered ask, and which the hub did not retain at all — do not.
    let p = f
        .approval(LaneApprovalKind::Permission, "per_1")
        .expect("tombstone");
    assert_eq!(p.status, LaneApprovalStatus::Resolved);
    assert_eq!(p.title, "awaiting approval: bash — ls *, cat *");
    assert_eq!(p.request_json, "{}"); // an empty DOCUMENT, never an empty string
    assert!(p.questions.is_empty());
    let q = f
        .approval(LaneApprovalKind::Question, "que_1")
        .expect("tombstone");
    assert_eq!(q.status, LaneApprovalStatus::Resolved);
    assert_eq!(q.title, "awaiting answer: Pick a branch");
    assert_eq!(q.request_json, "{}");
    assert!(q.questions.is_empty());

    // The seed's retirement path sheds identically.
    let mut f = OpencodeFold::new();
    f.apply_line(&question_asked("que_1", "ses_root"));
    f.seed_approvals(None, Some(&[]));
    let q = f
        .approval(LaneApprovalKind::Question, "que_1")
        .expect("tombstone");
    assert!(q.questions.is_empty());
    assert_eq!(q.request_json, "{}");
}

// ---- reopen rebuilds the WHOLE DTO ----

#[test]
fn a_tombstoned_permission_reopens_with_its_session_and_raw_request() {
    let mut f = OpencodeFold::new();
    // A reply whose vocabulary we cannot read, for an ask the fold never saw:
    // a resolved tombstone with no session and no raw request (nothing reads
    // either while it is resolved).
    assert!(f.apply_line(&permission_replied("per_1", "unknown")));
    let tomb = f
        .approval(LaneApprovalKind::Permission, "per_1")
        .expect("tombstone");
    assert_eq!(tomb.status, LaneApprovalStatus::Resolved);
    assert_eq!(tomb.session_id, "");
    assert_eq!(tomb.request_json, "{}");
    let _ = rows(&mut f);

    // The ask arrives after it (a gap swallowed it, or the frames raced). It
    // REOPENS the entry — and must rebuild the DTO completely: `session_id` is
    // what attributes a child session's approval on the parent's panel, and
    // `request_json` is the contract's raw escape hatch. Leaving either at the
    // tombstone's empty value ships a pending approval that violates both.
    assert!(f.apply_line(&permission_asked("per_1", "ses_child")));
    assert_eq!(
        f.approval(LaneApprovalKind::Permission, "per_1"),
        Some(expected_permission("per_1", "ses_child"))
    );
    assert_eq!(
        f.pending_approvals(),
        vec![expected_permission("per_1", "ses_child")]
    );
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(f.activity(), RcActivity::NeedsApproval);
    // Both dedup slots were cleared, so the pending row is re-announced and the
    // client can render the buttons again.
    assert_eq!(rows(&mut f), vec![permission_pending_row("per_1")]);
}

#[test]
fn a_replied_or_rejected_question_reopens_fully_on_a_later_ask() {
    for typ in ["question.replied", "question.rejected"] {
        let mut f = OpencodeFold::new();
        f.apply_line(&question_asked("que_1", "ses_root"));
        let _ = rows(&mut f);
        assert!(f.apply_line(&question_closed(typ, "que_1")), "{typ}");
        assert_eq!(f.open_approvals(), 0, "{typ}");

        // A question is only ever resolved with an empty decision, so a re-ask
        // always reopens — and rebuilds every field from THIS frame, session and
        // raw request included, so the DTO can never carry the new structured
        // questions beside a stale (or shed) raw request.
        assert!(f.apply_line(&question_asked("que_1", "ses_child")), "{typ}");
        assert_eq!(
            f.approval(LaneApprovalKind::Question, "que_1"),
            Some(expected_question("que_1", "ses_child")),
            "{typ}"
        );
        assert_eq!(
            f.pending_approvals(),
            vec![expected_question("que_1", "ses_child")],
            "{typ}"
        );
        assert_eq!(f.activity(), RcActivity::NeedsApproval, "{typ}");
        // The status row is announce-once, as it was in the hub.
        assert!(rows(&mut f).is_empty(), "{typ}");
    }
}

// ---- session.error (the hub ignores it; the lane surfaces it) ----

#[test]
fn session_error_emits_a_display_only_status_row() {
    let mut f = OpencodeFold::new();
    let err = line(&serde_json::json!({
        "type": "session.error",
        "properties": {
            "sessionID": "ses_root",
            "error": {"name": "ProviderAuthError", "data": {"message": "no api key"}}
        }
    }));
    assert!(f.apply_line(&err));
    assert_eq!(rows(&mut f), vec![status_row("session error: no api key")]);
    // Display-only in the strict sense: an error says nothing about whether the
    // session is running, so it neither confirms the fold nor moves the verdict.
    assert_eq!(f.activity(), RcActivity::Unknown);
    // Content-keyed dedup, so a reseed replay does not stack rows.
    assert!(f.apply_line(&err));
    assert!(rows(&mut f).is_empty());
}

#[test]
fn session_error_falls_back_through_name_then_a_bare_label() {
    let mut f = OpencodeFold::new();
    assert!(f.apply_line(&line(&serde_json::json!({
        "type": "session.error",
        "properties": {"sessionID": "ses_root", "error": {"name": "MessageAbortedError"}}
    }))));
    assert!(f.apply_line(&line(&serde_json::json!({
        "type": "session.error",
        "properties": {"sessionID": "ses_root"}
    }))));
    assert_eq!(
        rows(&mut f),
        vec![
            status_row("session error: MessageAbortedError"),
            status_row("session error"),
        ]
    );
}

// ---- seed_approvals (the method that replaced `shed.approval.seed`) ----

#[test]
fn seed_approvals_retires_what_the_server_no_longer_lists() {
    let mut f = OpencodeFold::new();
    f.apply_line(&permission_asked("per_1", "ses_root"));
    f.apply_line(&question_asked("que_1", "ses_root"));
    let _ = rows(&mut f);
    assert_eq!(f.open_approvals(), 2);

    // The server lists neither: both were answered in the TUI while the stream
    // was down. Retired with NO decision — the lane cannot know which way.
    assert!(f.seed_approvals(Some(&[]), Some(&[])));
    assert_eq!(
        rows(&mut f),
        vec![Row {
            role: "tool",
            typ: "approval_request",
            text: "awaiting approval: bash — ls *, cat *".into(),
            tool: None,
            approval: Some(("per_1".into(), "resolved".into(), String::new(), Vec::new())),
        }]
    );
    assert_eq!(f.open_approvals(), 0);
    assert_eq!(f.activity(), RcActivity::Working);
    // A second identical seed changes nothing.
    assert!(!f.seed_approvals(Some(&[]), Some(&[])));
}

#[test]
fn seed_approvals_halves_carry_independent_authority() {
    let mut f = OpencodeFold::new();
    f.apply_line(&permission_asked("per_1", "ses_root"));
    f.apply_line(&question_asked("que_1", "ses_root"));
    let _ = rows(&mut f);

    // `None` is "that REST read FAILED", not "nothing is open" — mistaking the
    // two would retire live approvals.
    assert!(!f.seed_approvals(None, None));
    assert_eq!(f.open_approvals(), 2);

    // One half can heal while the other says nothing.
    assert!(f.seed_approvals(None, Some(&[])));
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(f.activity(), RcActivity::NeedsApproval);

    // A listed id survives its own half's seed.
    assert!(!f.seed_approvals(Some(&["per_1".to_string()]), None));
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(
        f.pending_approvals()
            .iter()
            .map(|a| a.id.clone())
            .collect::<Vec<_>>(),
        vec!["per_1"]
    );
}

#[test]
fn a_seed_retired_ask_reopens_on_a_later_ask_but_a_replied_one_does_not() {
    let mut f = OpencodeFold::new();
    f.apply_line(&permission_asked("per_1", "ses_root"));
    f.seed_approvals(Some(&[]), None);
    let _ = rows(&mut f);
    assert_eq!(f.open_approvals(), 0);

    // The seed retired it on the evidence "the server no longer lists it". A
    // later ask is NEWER, stronger evidence that it IS open — a racing REST
    // snapshot got it wrong — so it reopens and re-announces both rows.
    assert!(f.apply_line(&permission_asked("per_1", "ses_root")));
    assert_eq!(rows(&mut f).len(), 1);
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(f.activity(), RcActivity::NeedsApproval);

    // A REAL reply outranks any ask replay: no reopen after that.
    f.apply_line(&permission_replied("per_1", "reject"));
    let _ = rows(&mut f);
    assert!(!f.apply_line(&permission_asked("per_1", "ses_root")));
    assert!(rows(&mut f).is_empty());
    assert_eq!(f.open_approvals(), 0);
}

#[test]
fn a_retired_question_reopens_on_a_later_ask() {
    let mut f = OpencodeFold::new();
    f.apply_line(&question_asked("que_1", "ses_root"));
    f.seed_approvals(None, Some(&[]));
    let _ = rows(&mut f);
    assert_eq!(f.open_approvals(), 0);

    // A question is only ever resolved with an empty decision, so the reopen
    // rule is unconditional for it — which is what the hub did by REMOVING a
    // replied question and re-inserting it on the next ask.
    assert!(f.apply_line(&question_asked("que_1", "ses_root")));
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(f.activity(), RcActivity::NeedsApproval);
    // The status row is announce-once, so the reopen emits none.
    assert!(rows(&mut f).is_empty());
}

// ---- note_gap / reset ----

#[test]
fn note_gap_drops_pending_tool_calls_but_keeps_approvals_and_dedup() {
    let mut f = OpencodeFold::new();
    f.apply_line(&session_status("idle"));
    f.apply_line(&tool_part("call_1", "running"));
    f.apply_line(&permission_asked("per_1", "ses_root"));
    let before = rows(&mut f);
    assert_eq!(before.len(), 2, "{before:?}"); // tool_use + the pending approval row
    assert_eq!(f.pending_len(), 1);

    f.note_gap();

    // The pending tool call is gone: a gap may have swallowed its completion,
    // and a forever-pending callID would pin the verdict at working.
    assert_eq!(f.pending_len(), 0);
    // The open ask is KEPT — only a reply or a seed retires it, never a gap.
    assert_eq!(f.open_approvals(), 1);
    assert_eq!(f.activity(), RcActivity::NeedsApproval);
    // And the dedup set is kept, so the reseed a gap triggers emits nothing.
    f.apply_line(&permission_asked("per_1", "ses_root"));
    f.apply_line(&tool_part("call_1", "running"));
    assert!(rows(&mut f).is_empty());
}

#[test]
fn note_gap_unpins_the_verdict_once_the_approval_is_answered() {
    let mut f = OpencodeFold::new();
    f.apply_line(&session_status("idle"));
    f.apply_line(&tool_part("call_1", "running"));
    let _ = rows(&mut f);
    // A pending tool call outranks the idle boundary.
    assert_eq!(f.activity(), RcActivity::Working);
    f.note_gap();
    assert_eq!(f.activity(), RcActivity::NeedsInput);
}

#[test]
fn reset_clears_approvals_dedup_and_the_verdict() {
    let mut f = OpencodeFold::new();
    f.apply_line(&session_status("busy"));
    f.apply_line(&permission_asked("per_1", "ses_root"));
    let _ = rows(&mut f);
    assert_eq!(f.open_approvals(), 1);

    f.reset();

    assert_eq!(f.activity(), RcActivity::Unknown);
    assert_eq!(f.open_approvals(), 0);
    assert_eq!(
        f.approval_state(LaneApprovalKind::Permission, "per_1"),
        None
    );
    assert_eq!(f.last_message(), "");
    // The dedup set went with it, which is what makes the next generation a
    // full seed rather than a silent no-op.
    f.apply_line(&permission_asked("per_1", "ses_root"));
    assert_eq!(rows(&mut f).len(), 1);
}

// ---- verdict + fallback edges the fixture does not reach ----

#[test]
fn the_fold_never_reports_idle() {
    // The hub could, because it also watched a tmux pane for stability. There is
    // no pane here: a session with nothing to say is needs_input or working.
    let mut f = OpencodeFold::new();
    assert_eq!(f.activity(), RcActivity::Unknown);
    f.apply_line(&session_status("idle"));
    assert_eq!(f.activity(), RcActivity::NeedsInput);
    f.apply_line(&session_status("busy"));
    assert_eq!(f.activity(), RcActivity::Working);
    // retry keeps working.
    f.apply_line(&session_status("retry"));
    assert_eq!(f.activity(), RcActivity::Working);
    f.apply_line(&line(&serde_json::json!({
        "type": "session.idle", "properties": {"sessionID": "ses_root"}
    })));
    assert_eq!(f.activity(), RcActivity::NeedsInput);
}

#[test]
fn a_live_status_boundary_beats_the_rest_fallback() {
    let mut f = OpencodeFold::new();
    // With no live boundary the REST snapshot establishes one.
    assert!(f.apply_status_fallback(true));
    assert_eq!(f.activity(), RcActivity::NeedsInput);

    let mut f = OpencodeFold::new();
    f.apply_line(&session_status("busy"));
    // The live `/event` stream is authoritative and ordered; a REST snapshot
    // taken before it must not overwrite a newer buffered boundary.
    assert!(!f.apply_status_fallback(true));
    assert_eq!(f.activity(), RcActivity::Working);
}

#[test]
fn an_unknown_tool_state_is_noise_not_a_call() {
    let mut f = OpencodeFold::new();
    assert!(!f.apply_line(&tool_part("call_1", "levitating")));
    assert!(rows(&mut f).is_empty());
    assert_eq!(f.pending_len(), 0);
    assert_eq!(f.activity(), RcActivity::Unknown);
}

#[test]
fn a_text_part_stays_cached_until_its_role_is_known() {
    let mut f = OpencodeFold::new();
    let part = line(&serde_json::json!({
        "type": "message.part.updated",
        "properties": {
            "sessionID": "ses_root", "time": 1_700_000_000_000i64,
            "part": {"id": "prt_1", "messageID": "msg_1", "type": "text",
                     "text": "hello", "time": {"start": 1, "end": 2}}
        }
    }));
    assert!(f.apply_line(&part));
    // Cached, not emitted: the owning message's role is not known yet.
    assert!(rows(&mut f).is_empty());
    assert_eq!(f.parts_len(), 1);

    assert!(f.apply_line(&line(&serde_json::json!({
        "type": "message.updated",
        "properties": {"sessionID": "ses_root",
                       "info": {"id": "msg_1", "role": "assistant",
                                "time": {"created": 1, "completed": 2}}}
    }))));
    assert_eq!(
        rows(&mut f),
        vec![Row {
            role: "assistant",
            typ: "text",
            text: "hello".into(),
            tool: None,
            approval: None,
        }]
    );
    assert_eq!(f.parts_len(), 0);
    assert_eq!(f.last_message(), "hello");
}

#[test]
fn a_malformed_or_unknown_line_leaves_state_untouched() {
    let mut f = OpencodeFold::new();
    f.apply_line(&session_status("busy"));
    let before = f.activity();
    for bad in [
        &b"not json"[..],
        &b"[1,2,3]"[..],                   // Go rejects a non-object top level
        &b"null"[..],                      // …and a top-level null no-ops
        &b"{}"[..],                        // no type
        br#"{"type":"server.connected"}"#, // a type this fold ignores
        br#"{"type":"session.status","properties":[]}"#, // a non-object properties errors the line
        br#"{"type":"session.status","properties":{"status":{"type":"levitating"}}}"#,
    ] {
        assert!(!f.apply_line(bad), "{}", String::from_utf8_lossy(bad));
    }
    assert!(rows(&mut f).is_empty());
    assert_eq!(f.activity(), before);
}

/// opencode's three permission options, pinned by NAME on both axes.
///
/// The point of the pin is that the two axes are independent and one of them
/// has already been dropped once. `id` is opencode's own opaque token — it is
/// what `LaneAnswer::Choice` hands straight back and what the reply route
/// resolves — and `kind` is the ACP semantics a client styles a destructive
/// button from and a by-kind resolver matches `LaneAnswer::Permission` on.
///
/// The third row is the one that matters: `id: "reject"` with
/// `kind: "reject_once"`. If a future edit ever "tidies" these into one field,
/// or derives the kind from the id, this fails — and so does every client that
/// sniffed the id expecting to find `reject_once` there.
#[test]
fn opencode_permission_options_carry_both_the_opaque_id_and_the_acp_kind() {
    let got: Vec<(String, String, Option<String>)> = permission_options()
        .into_iter()
        .map(|o| (o.id, o.label, o.kind))
        .collect();
    assert_eq!(
        got,
        vec![
            (
                "allow_once".to_string(),
                "Allow once".to_string(),
                Some("allow_once".to_string())
            ),
            (
                "allow_always".to_string(),
                "Always".to_string(),
                Some("allow_always".to_string())
            ),
            (
                "reject".to_string(),
                "Reject".to_string(),
                Some("reject_once".to_string())
            ),
        ],
    );

    // Every option states a kind — an absent one is what the contract reads as
    // "this agent offers no semantics", which is false for a permission here.
    assert!(permission_options().iter().all(|o| o.kind.is_some()));

    // The kinds are the contract's constants, not strings that merely look like
    // them, and they are the ACP vocabulary the panel branches on.
    let kinds: Vec<String> = permission_options()
        .into_iter()
        .filter_map(|o| o.kind)
        .collect();
    assert_eq!(
        kinds,
        vec![
            option_kind::ALLOW_ONCE,
            option_kind::ALLOW_ALWAYS,
            option_kind::REJECT_ONCE
        ],
    );
    // opencode offers no reject-always, which is exactly why the contract's
    // `Reject` mapping falls back to "the first option whose kind starts
    // `reject`" instead of demanding an exact `reject_once`.
    assert!(!kinds.iter().any(|k| k == option_kind::REJECT_ALWAYS));
    assert!(kinds
        .iter()
        .any(|k| k.starts_with("reject") && k == option_kind::REJECT_ONCE));

    // A FRESH vec per call — every copy lands on a DTO that must not alias
    // another's.
    let mut a = permission_options();
    a[0].label = "mutated".into();
    assert_eq!(permission_options()[0].label, "Allow once");
}
