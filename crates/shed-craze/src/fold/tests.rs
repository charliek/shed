//! The fold, cell by cell: §3.3.6's event → rows table row by row, gx's
//! segment/tool/book cases where they apply (ported from
//! `crates/shed-gx/src/fold/tests.rs` at `203250f`), craze's own (the
//! interjection, the strip, the foreign-turn wordings and their nesting, the
//! notes, compaction, replay), and snapshot → rows.

use super::*;
use serde_json::json;

const HOST: &str = "0123456789ab";
const AT: &str = "2026-01-01T00:00:00Z";

fn fold() -> CrazeFold {
    CrazeFold::new(HOST)
}

/// Fold `events`, flush, and take every row.
fn rows_of(f: &mut CrazeFold, events: &[Value]) -> Vec<RcFeedMessage> {
    for e in events {
        f.apply(e);
    }
    f.flush();
    f.drain()
}

fn kinds(rows: &[RcFeedMessage]) -> Vec<(String, String)> {
    rows.iter()
        .map(|r| (r.role.clone(), r.msg_type.clone()))
        .collect()
}

fn texts(rows: &[RcFeedMessage]) -> Vec<String> {
    rows.iter()
        .map(|r| r.text.clone().unwrap_or_default())
        .collect()
}

fn ev(kind: &str, extra: Value) -> Value {
    let mut v = json!({"type": kind, "at": AT});
    let Value::Object(extra) = extra else {
        panic!("extra members are an object")
    };
    for (k, x) in extra {
        v[k.as_str()] = x;
    }
    v
}

fn text(t: &str) -> Value {
    ev("text", json!({"text": t}))
}

fn tool(id: &str, status: &str, extra: Value) -> Value {
    let mut t =
        json!({"id": id, "status": status, "title": format!("Run {id}"), "toolName": "shell"});
    let Value::Object(extra) = extra else {
        panic!("extra members are an object")
    };
    for (k, x) in extra {
        t[k.as_str()] = x;
    }
    ev("tool", json!({ "tool": t }))
}

fn permission(id: &str) -> Value {
    ev(
        "permission",
        json!({"permission": {"id": id, "tool": "Run `git push`", "options": [
        {"optionId": "a1", "name": "Yes", "kind": "allow_once"},
        {"optionId": "a2", "name": "Yes, and never ask", "kind": "allow_once"},
        {"optionId": "always", "name": "Always", "kind": "allow_always"},
        {"optionId": "no", "name": "No", "kind": "reject_once"}]}}),
    )
}

fn ask_end(id: &str, kind: &str, extra: Value) -> Value {
    let mut a = json!({"id": id, "kind": kind, "outcome": "answered", "by": "client"});
    let Value::Object(extra) = extra else {
        panic!("extra members are an object")
    };
    for (k, x) in extra {
        a[k.as_str()] = x;
    }
    ev("ask", json!({ "ask": a }))
}

// ---- streams and segments ----

#[test]
fn text_and_thought_are_streaks_and_a_kind_change_ends_one() {
    let mut f = fold();
    let rows = rows_of(
        &mut f,
        &[
            ev("thought", json!({"text": "think "})),
            ev("thought", json!({"text": "hard"})),
            text("Hello, "),
            text("world"),
        ],
    );
    assert_eq!(
        kinds(&rows),
        [
            ("assistant".into(), "reasoning".into()),
            ("assistant".into(), "text".into())
        ]
    );
    assert_eq!(texts(&rows), ["think hard", "Hello, world"]);
    assert_eq!(rows[0].ts.as_deref(), Some(AT));
}

/// Lossless at 8 KiB through the fold (the segmenter's own cells pin the
/// edges).
#[test]
fn a_long_reply_segments_losslessly() {
    let mut f = fold();
    let block = "y".repeat(1000);
    let events: Vec<Value> = (0..9).map(|_| text(&block)).collect();
    let rows = rows_of(&mut f, &events);
    assert_eq!(rows.len(), 2);
    assert_eq!(texts(&rows).concat().len(), 9000);
    assert!(rows
        .iter()
        .all(|r| r.text.as_deref().unwrap().len() <= crate::segment::MAX_STREAK_BYTES));
}

/// Every other row ends the open streak first — and `turn ended` does not
/// (craze's rule: only the wire's `done` closes a run).
#[test]
fn any_other_row_flushes_the_streak_and_turn_ended_does_not() {
    let mut f = fold();
    f.apply(&text("partial"));
    f.apply(&ev(
        "turn",
        json!({"turn": {"id": "t", "phase": "ended", "stopReason": "end_turn"}}),
    ));
    assert!(f.drain().is_empty(), "turn ended does not flush");
    f.apply(&ev("done", json!({"stopReason": "end_turn"})));
    assert_eq!(texts(&f.drain()), ["partial"], "done does");
    f.apply(&text("more"));
    f.apply(&ev(
        "command",
        json!({"command": {"pluginCommand": {"qualified": "p:review", "kind": "skill"}}}),
    ));
    assert_eq!(texts(&f.drain()), ["more", "⤷ p:review (skill)"]);
}

// ---- tools ----

/// gx's tool cases: first sight is `tool_use {name: toolName, detail:
/// title}`, updates absorbed, the first terminal status one `tool_result`,
/// repeats absorbed — and a replayed call after its result does not draw a
/// second.
#[test]
fn a_tool_draws_one_use_and_one_result() {
    let mut f = fold();
    let rows = rows_of(
        &mut f,
        &[
            tool("t1", "pending", json!({})),
            tool("t1", "in_progress", json!({})),
            tool("t1", "completed", json!({"output": {"stdoutHead": "ok\n"}})),
            tool("t1", "completed", json!({"output": {"stdoutHead": "ok\n"}})),
            tool("t1", "in_progress", json!({})),
        ],
    );
    assert_eq!(
        kinds(&rows),
        [
            ("tool".into(), "tool_use".into()),
            ("tool".into(), "tool_result".into())
        ]
    );
    let use_ = rows[0].tool.as_ref().unwrap();
    assert_eq!(
        (use_.name.as_deref(), use_.detail.as_deref()),
        (Some("shell"), Some("Run t1"))
    );
    assert_eq!(rows[1].text.as_deref(), Some("ok\n"));
}

/// `failed` and `cancelled` are terminal (native tools end `cancelled`); a
/// completed tool's text is its stdout head, else its content's head.
#[test]
fn failed_and_cancelled_are_terminal_and_a_result_carries_a_head() {
    let mut f = fold();
    let long = "c".repeat(2000);
    let rows = rows_of(
        &mut f,
        &[
            tool("a", "failed", json!({})),
            tool("b", "cancelled", json!({})),
            tool("c", "completed", json!({"contentText": long})),
        ],
    );
    let results: Vec<String> = rows
        .iter()
        .filter(|r| r.msg_type == "tool_result")
        .map(|r| r.text.clone().unwrap())
        .collect();
    assert_eq!(results[0], "failed");
    assert_eq!(results[1], "cancelled");
    assert_eq!(results[2].len(), RESULT_HEAD_BYTES, "the content's head");
}

/// A tool ends the open streak; the todo writer is dropped BEFORE it can; an
/// id-less tool always appends; a name falls back to the title, then `name`.
#[test]
fn tool_edges() {
    let mut f = fold();
    f.apply(&text("before"));
    f.apply(&ev(
        "tool",
        json!({"tool": {"id": "todo", "toolName": "updateTodos", "status": "completed"}}),
    ));
    f.apply(&ev(
        "tool",
        json!({"tool": {"id": "x", "title": "Update TODOs", "status": "completed"}}),
    ));
    f.apply(&text(" still"));
    f.apply(&ev(
        "tool",
        json!({"tool": {"title": "Grep", "status": "pending"}}),
    ));
    f.apply(&ev(
        "tool",
        json!({"tool": {"title": "Grep", "status": "pending"}}),
    ));
    f.apply(&ev(
        "tool",
        json!({"tool": {"id": "n", "name": "Shell", "status": "pending"}}),
    ));
    let rows = f.drain();
    assert_eq!(
        texts(&rows)[0],
        "before still",
        "the todo writer closed nothing"
    );
    let names: Vec<Option<String>> = rows[1..]
        .iter()
        .map(|r| r.tool.as_ref().unwrap().name.clone())
        .collect();
    assert_eq!(
        names,
        [
            Some("Grep".into()),
            Some("Grep".into()),
            Some("Shell".into())
        ]
    );
}

/// The maps are bounded, and the bound never drops something live (gx's).
#[test]
fn the_tool_and_book_bounds_never_drop_a_live_entry() {
    let mut f = fold();
    // One in flight, then MAX_TOOLS finished ones.
    f.apply(&tool("live", "in_progress", json!({})));
    for i in 0..MAX_TOOLS + 50 {
        f.apply(&tool(&format!("t{i}"), "completed", json!({})));
    }
    assert!(f.tools_len() <= MAX_TOOLS + 1);
    let _ = f.drain();
    f.apply(&tool("live", "completed", json!({})));
    let rows = f.drain();
    assert_eq!(
        rows.len(),
        1,
        "the in-flight tool kept its state: one result, no new use"
    );
    assert_eq!(
        rows[0].tool.as_ref().unwrap().name.as_deref(),
        Some("shell")
    );

    // A pending approval survives a book full of resolved ones.
    f.apply(&permission("keep"));
    for i in 0..MAX_APPROVALS + 20 {
        let id = format!("p{i}");
        f.apply(&permission(&id));
        f.apply(&ask_end(&id, "permission", json!({"optionId": "no"})));
    }
    assert!(f.approvals_len() <= MAX_APPROVALS);
    assert!(f.approval("keep").is_some_and(|a| a.status.is_pending()));
    assert_eq!(f.open_approvals(), 1);
}

// ---- users and turns ----

#[test]
fn a_turn_starts_with_its_user_row_and_the_leading_blocks_stripped() {
    let mut f = fold();
    let prompt = "<craze_attachments>{\"v\":1,\"images\":[{\"n\":1,\"path\":\"/a/b.png\",\"mime\":\"image/png\"}]}</craze_attachments>\n<shell_context>\n<command exit=\"0\">git status</command>\n<output>\nclean\n</output>\n</shell_context>\n\nsummarise that";
    f.apply(&text("prior"));
    f.apply(&ev(
        "turn",
        json!({"turn": {"id": "t1", "phase": "started", "text": prompt, "origin": "submit"}}),
    ));
    let rows = f.drain();
    assert_eq!(texts(&rows), ["prior", "summarise that"]);
    assert_eq!(kinds(&rows)[1], ("user".into(), "text".into()));
    assert_eq!(f.activity(), Some(RcActivity::Working));
}

/// craze's `SplitShellContext` vectors: each block recognised only when
/// well-formed; the two parts are exactly the text.
#[test]
fn the_strip_recognises_only_well_formed_blocks() {
    let cases = [
        ("plain", "", "plain"),
        (
            "<shell_context>\n<command exit=\"1\">x</command>\n</shell_context>\n\nhi",
            "<shell_context>\n<command exit=\"1\">x</command>\n</shell_context>\n\n",
            "hi",
        ),
        // No entry line: the user's text.
        (
            "<shell_context>\nnot a command\n</shell_context>\n\nhi",
            "",
            "<shell_context>\nnot a command\n</shell_context>\n\nhi",
        ),
        // Never closed: the user's text.
        (
            "<shell_context>\n<command exit=\"0\">x</command>\nhi",
            "",
            "<shell_context>\n<command exit=\"0\">x</command>\nhi",
        ),
        // An envelope alone, at the very end.
        (
            "<craze_attachments>{\"v\":1}</craze_attachments>",
            "<craze_attachments>{\"v\":1}</craze_attachments>",
            "",
        ),
        // An envelope whose JSON is not an object, or a known member of the
        // wrong type, or not followed by a newline: the user's text.
        (
            "<craze_attachments>[1]</craze_attachments>\nhi",
            "",
            "<craze_attachments>[1]</craze_attachments>\nhi",
        ),
        (
            "<craze_attachments>{\"v\":\"one\"}</craze_attachments>\nhi",
            "",
            "<craze_attachments>{\"v\":\"one\"}</craze_attachments>\nhi",
        ),
        (
            "<craze_attachments>{\"v\":1}</craze_attachments>hi",
            "",
            "<craze_attachments>{\"v\":1}</craze_attachments>hi",
        ),
        // Members match case-insensitively, as Go's decoder does.
        (
            "<craze_attachments>{\"V\":1,\"IMAGES\":[{\"N\":2}]}</craze_attachments>\nhi",
            "<craze_attachments>{\"V\":1,\"IMAGES\":[{\"N\":2}]}</craze_attachments>\n",
            "hi",
        ),
    ];
    for (whole, block, rest) in cases {
        let (b, r) = split_shell_context(whole);
        assert_eq!((b, r), (block, rest), "{whole:?}");
        assert_eq!(format!("{b}{r}"), whole);
    }
}

/// The interjection is a user row; a live echo draws nothing; inside a replay
/// bracket (or stamped `replayed`) a user event draws its row.
#[test]
fn user_events() {
    let mut f = fold();
    let rows = rows_of(
        &mut f,
        &[
            ev("user", json!({"text": "echoed"})),
            ev("user", json!({"text": "now this", "interjection": true})),
            ev("user", json!({"text": "", "interjection": true})),
            ev("replay", json!({"replay": {"phase": "start"}})),
            ev("user", json!({"text": "from before"})),
            text("old reply"),
            ev("replay", json!({"replay": {"phase": "end"}})),
            ev("user", json!({"text": "stamped", "replayed": true})),
        ],
    );
    assert_eq!(
        texts(&rows),
        [
            "now this",
            "from before",
            "old reply",
            "restored",
            "stamped"
        ]
    );
}

/// The turn's end bracket idles; `done` idles; a cancelled `done` is its
/// note; a synthetic ended with an error is the error's row (and with none,
/// nothing — §3.3.6 lists only the error).
#[test]
fn turn_endings() {
    let mut f = fold();
    f.apply(&ev(
        "turn",
        json!({"turn": {"id": "t", "phase": "started", "text": "x"}}),
    ));
    f.apply(&ev("done", json!({"stopReason": "cancelled"})));
    assert_eq!(f.activity(), Some(RcActivity::Idle));
    f.apply(&ev(
        "turn",
        json!({"turn": {"id": "t2", "phase": "ended", "synthetic": true, "err": "prompt refused"}}),
    ));
    f.apply(&ev("turn", json!({"turn": {"id": "t3", "phase": "ended", "synthetic": true, "stopReason": "cancelled"}})));
    f.apply(&ev(
        "error",
        json!({"err": {"message": "the agent exited", "class": "agent_exited", "code": 1}}),
    ));
    f.apply(&ev(
        "error",
        json!({"err": {"message": "", "class": "other", "code": 0}}),
    ));
    f.apply(&ev(
        "meta",
        json!({"state": {"detail": "cancel failed: x", "indexErr": "index: disk full"}}),
    ));
    let rows = f.drain();
    assert_eq!(
        texts(&rows)[1..],
        [
            "cancelled",
            "prompt refused",
            "the agent exited",
            "cancel failed: x",
            "index: disk full"
        ]
    );
    assert!(rows[1..]
        .iter()
        .all(|r| r.role == "system" && r.msg_type == "status"));
}

// ---- foreign turns (Amendment A7) ----

/// protocol.md's own example (~:1206-1207): the bracket nested under
/// `foreignTurn`, `running: true` on its start and ABSENT on its end. The
/// start draws craze's wording for its reason; the end draws NO row and idles
/// the session.
#[test]
fn a_foreign_turn_is_nested_and_its_end_draws_no_row_and_idles() {
    let mut f = fold();
    f.apply(&json!({"type":"foreign_turn","foreignTurn":{"id":"wake-1","text":"sub-agent result","reason":"subagent_wake","running":true},"at":"2026-01-01T00:00:00Z"}));
    assert_eq!(f.activity(), Some(RcActivity::Working));
    f.apply(&text("the agent continues"));
    f.apply(&json!({"type":"foreign_turn","foreignTurn":{"id":"wake-1","reason":"subagent_wake"},"at":"2026-01-01T00:00:00Z"}));
    let rows = f.drain();
    assert_eq!(
        texts(&rows),
        [
            "sub-agent finished — the agent continues",
            "the agent continues"
        ],
        "the end bracket flushed the run and drew nothing of its own"
    );
    assert_eq!(
        f.activity(),
        Some(RcActivity::Idle),
        "a foreign turn has no done"
    );
    // A pending approval still wins (the override).
    f.apply(&permission("p"));
    f.apply(&json!({"type":"foreign_turn","foreignTurn":{"id":"w2"}}));
    assert_eq!(f.activity(), Some(RcActivity::NeedsApproval));
}

/// The three wordings, `job_wake` included, and an unknown reason's default.
#[test]
fn the_foreign_turn_wordings() {
    let mut f = fold();
    for reason in ["", "subagent_wake", "job_wake", "a_reason_from_2027"] {
        f.apply(&ev(
            "foreign_turn",
            json!({"foreignTurn": {"id": "w", "reason": reason, "running": true}}),
        ));
    }
    assert_eq!(
        texts(&f.drain()),
        [
            "agent continued on its own (interjection fallback)",
            "sub-agent finished — the agent continues",
            "background command finished — the agent continues",
            "agent continued on its own (interjection fallback)",
        ]
    );
}

/// A FLAT payload (protocol.md's old, wrong example) is not a running
/// bracket: no wording is drawn from it.
#[test]
fn a_flat_foreign_turn_is_not_read_as_running() {
    let mut f = fold();
    f.apply(&ev(
        "foreign_turn",
        json!({"id": "w", "reason": "job_wake", "running": true}),
    ));
    assert!(f.drain().is_empty(), "nothing read from a flat payload");
    assert_eq!(f.activity(), Some(RcActivity::Idle));
}

// ---- todos, compaction, commands ----

/// craze's `noteTodos`: "planned" only when the most grows, one "done" when
/// all close; repeats and intermediate updates draw nothing.
#[test]
fn todos_draw_only_growth_and_one_completion() {
    let mut f = fold();
    let todos = |statuses: &[&str]| {
        ev(
            "todos",
            json!({"todos": statuses.iter().map(|s| json!({"content": "x", "status": s})).collect::<Vec<_>>()}),
        )
    };
    let rows = rows_of(
        &mut f,
        &[
            todos(&["pending", "pending"]),
            todos(&["in_progress", "pending"]),
            todos(&["completed", "pending"]),
            todos(&["completed", "pending", "pending"]),
            todos(&["completed", "completed", "pending"]),
            todos(&["completed", "cancelled", "completed"]),
            todos(&["completed", "cancelled", "completed"]),
        ],
    );
    assert_eq!(
        texts(&rows),
        ["tasks: 2 planned", "tasks: 3 planned", "tasks: 3/3 done"]
    );
}

/// A compaction's start is STATE (no row); its end is craze's note — the
/// reason's wording, the failure's.
#[test]
fn compaction_is_state_then_one_note() {
    let mut f = fold();
    f.apply(&text("before"));
    f.apply(&ev(
        "compaction",
        json!({"compaction": {"phase": "started", "reason": "auto"}}),
    ));
    assert!(f.compacting());
    assert_eq!(
        texts(&f.drain()),
        ["before"],
        "started flushed, drew nothing"
    );
    f.apply(&ev("compaction", json!({"compaction": {"phase": "ended", "reason": "overflow", "tokensBefore": 1234567, "tokensAfter": 999}})));
    f.apply(&ev("compaction", json!({"compaction": {"phase": "ended", "reason": "auto", "tokensBefore": 890000, "err": "native: provider \"x\" failed\n(HTTP 500)"}})));
    f.apply(&ev("compaction", json!({"agent": "child-1", "compaction": {"phase": "ended", "reason": "manual", "tokensBefore": 1, "tokensAfter": 1}})));
    assert!(!f.compacting());
    assert_eq!(
        texts(&f.drain()),
        [
            "context was too large — compacted · 1.23M → 999 tokens",
            "compaction failed: native: provider \"x\" failed (HTTP 500)",
        ],
        "a child's compaction draws nothing"
    );
}

#[test]
fn token_counts_are_crazes_three_figures() {
    for (n, s) in [
        (-5, "0"),
        (850, "850"),
        (1234, "1.23k"),
        (12345, "12.3k"),
        (890_000, "890k"),
        (999_600, "1M"),
        (1_210_000, "1.21M"),
        (2_000_000, "2M"),
        (10_000, "10k"),
    ] {
        assert_eq!(token_count(n), s, "{n}");
    }
}

// ---- the approval book ----

/// A permission: options VERBATIM (duplicate kinds included, ids opaque),
/// one pending `approval_request` row, `request_json` the body compact; its
/// ending: resolved, a second row, no note.
#[test]
fn a_permission_is_booked_verbatim_and_resolved_by_its_ending() {
    let mut f = fold();
    f.apply(&text("thinking"));
    f.apply(&permission("perm-1"));
    let rows = f.drain();
    assert_eq!(texts(&rows), ["thinking", "Run `git push`"]);
    let row = &rows[1];
    assert_eq!(
        (row.role.as_str(), row.msg_type.as_str()),
        ("tool", "approval_request")
    );
    let a = row.approval.as_ref().unwrap();
    assert_eq!((a.id.as_str(), a.status.as_str()), ("perm-1", "pending"));
    assert_eq!(a.decisions, ["allow", "allow_always", "deny"]);
    let ap = f.approval("perm-1").unwrap().clone();
    assert_eq!(ap.kind, LaneApprovalKind::Permission);
    assert_eq!(ap.session_id, HOST);
    assert_eq!(
        ap.options.iter().map(|o| o.id.as_str()).collect::<Vec<_>>(),
        ["a1", "a2", "always", "no"]
    );
    assert_eq!(
        ap.options[1].kind.as_deref(),
        Some("allow_once"),
        "duplicate kinds kept"
    );
    let req: Value = serde_json::from_str(&ap.request_json).unwrap();
    assert_eq!(req["id"], "perm-1");
    assert!(
        !ap.request_json.contains(' ') || ap.request_json.contains("git push"),
        "compact"
    );
    assert_eq!(f.activity(), Some(RcActivity::NeedsApproval));

    f.apply(&ask_end("perm-1", "permission", json!({"optionId": "no"})));
    f.apply(&ask_end("perm-1", "permission", json!({"optionId": "no"})));
    let rows = f.drain();
    assert_eq!(
        rows.len(),
        1,
        "one resolved row; a second ending is absorbed"
    );
    let a = rows[0].approval.as_ref().unwrap();
    assert_eq!(
        (a.status.as_str(), a.decision.as_deref()),
        ("resolved", Some("deny"))
    );
    assert_eq!(
        f.approval("perm-1").unwrap().status,
        LaneApprovalStatus::Resolved
    );
    assert_eq!(f.open_approvals(), 0);
}

/// A question carries EVERY question of its ask (answered atomically), each
/// keyed by its own id with no free text; an automatic one is never an
/// approval; its answered ending draws craze's notes.
#[test]
fn a_question_carries_all_its_questions_and_its_answer_draws_notes() {
    let mut f = fold();
    f.apply(&ev("question", json!({"question": {"id": "q-auto", "auto": true, "questions": [{"id": "x", "prompt": "x"}]}})));
    f.apply(&ev(
        "plan",
        json!({"plan": {"id": "p-auto", "auto": true, "name": "x"}}),
    ));
    assert!(f.drain().is_empty(), "automatic asks are never approvals");
    assert_eq!(f.approvals_len(), 0);
    f.apply(&ev("question", json!({"question": {"id": "q-1", "questions": [
        {"id": "lang", "prompt": "Which\nlanguage?", "options": [{"id": "rs", "label": "Rust", "description": "fast"}, {"id": "go", "label": "Go"}], "allowMultiple": true},
        {"id": "db", "prompt": "Which database?", "options": [{"id": "pg", "label": "Postgres"}, {"id": "pg", "label": "PG twin"}]},
        {"id": "none", "prompt": "Anything?", "options": [{"id": "y", "label": "Yes"}]}]}})));
    let q = f.approval("q-1").unwrap().clone();
    assert_eq!(q.kind, LaneApprovalKind::Question);
    assert_eq!(q.title, "Which language?", "the first prompt, on one line");
    assert_eq!(q.questions.len(), 3);
    assert_eq!(q.questions[0].id.as_deref(), Some("lang"));
    assert!(q.questions[0].multiple && !q.questions[0].custom);
    assert_eq!(
        q.questions[0].options[0].description.as_deref(),
        Some("fast")
    );
    let _ = f.drain();
    f.apply(&ask_end(
        "q-1",
        "question",
        json!({"answers": {"lang": ["rs", "go"], "db": ["pg"], "none": []}}),
    ));
    let rows = f.drain();
    assert_eq!(
        texts(&rows),
        [
            "Which language?",
            "? Which language? → Rust, Go",
            "? Which database? → …",
            "? Anything? → nothing",
        ],
        "the resolved row, then one note per question (an ambiguous pick is the ellipsis)"
    );
    // A skipped question.
    f.apply(&ev(
        "question",
        json!({"question": {"id": "q-2", "title": "Pick", "questions": []}}),
    ));
    f.apply(&ask_end("q-2", "question", json!({"skip": true})));
    assert!(texts(&f.drain()).contains(&"? Pick → skipped".to_string()));
}

/// A plan: its plan entry's row first (Amendment A10), then two synthesized
/// options (accept/reject), `detail` its name and overview; its answered
/// ending the plan note. An ending that is not an answer (cancelled, closing)
/// resolves with no note.
#[test]
fn a_plan_has_two_synthesized_options_and_its_note() {
    let mut f = fold();
    f.apply(&ev("plan", json!({"plan": {"id": "pl", "name": "Refactor", "overview": "Split the module", "plan": "1. split"}})));
    let opened = f.drain();
    assert_eq!(
        kinds(&opened),
        [
            ("system".to_string(), "status".to_string()),
            ("tool".to_string(), "approval_request".to_string())
        ],
        "the plan entry's row, then the pending approval row"
    );
    assert_eq!(
        texts(&opened),
        ["plan Refactor\nSplit the module", "Refactor"]
    );
    let p = f.approval("pl").unwrap().clone();
    assert_eq!(p.kind, LaneApprovalKind::PlanApproval);
    let opts: Vec<(&str, Option<&str>)> = p
        .options
        .iter()
        .map(|o| (o.id.as_str(), o.kind.as_deref()))
        .collect();
    assert_eq!(
        opts,
        [
            ("accept", Some("allow_once")),
            ("reject", Some("reject_once"))
        ]
    );
    assert_eq!(p.detail.as_deref(), Some("Refactor\nSplit the module"));
    f.apply(&ask_end("pl", "plan", json!({"accepted": false})));
    assert_eq!(texts(&f.drain()), ["Refactor", "plan Refactor → rejected"]);
    f.apply(&ev("plan", json!({"plan": {"id": "pl2", "name": "Other"}})));
    f.apply(&ask_end(
        "pl2",
        "plan",
        json!({"outcome": "closing", "by": "close"}),
    ));
    let rows = f.drain();
    assert_eq!(
        texts(&rows),
        ["plan Other", "Other", "Other"],
        "the entry's row (no overview: one line), the pending row and the resolved one, no note"
    );
    assert_eq!(rows[2].approval.as_ref().unwrap().decision, None);
    // An automatic plan is neither an approval nor transcript material.
    f.apply(&ev(
        "plan",
        json!({"plan": {"id": "pl3", "name": "Auto", "auto": true}}),
    ));
    assert!(f.drain().is_empty(), "an automatic plan draws nothing");
}

/// **Live and restored read the same** (Amendment A10): a non-automatic plan
/// folded live draws exactly the rows a restore of the snapshot holding that
/// same plan — its `plan` entry and its open ask — draws, in the same order.
#[test]
fn a_plan_reads_the_same_live_and_restored() {
    let plan =
        json!({"id": "pl", "name": "Refactor", "overview": "Split the module", "plan": "1. split"});
    let mut live = fold();
    live.apply(&ev("plan", json!({ "plan": plan.clone() })));
    let live_rows = live.drain();

    let mut restored = fold();
    restored
        .restore(&json!({"version": 1, "incarnation": "I", "seq": 1,
            "main": {"entries": [{"id": "1.0", "kind": "plan", "plan": plan.clone(), "at": AT}]},
            "asks": [{"id": "pl", "kind": "plan", "at": AT, "body": {"plan": plan.clone()}}]}))
        .unwrap();
    restored.adopt_registry(&[record("plan", &plan)]);
    let restored_rows = restored.drain();
    assert_eq!(live_rows.len(), 2, "{live_rows:#?}");
    assert_eq!(
        live_rows, restored_rows,
        "one committed plan, one projection — live or after a reseed or history()"
    );
    assert_eq!(live.held_approvals(), restored.held_approvals());
}

/// A sub-agent's events draw no row and leave the main streak open — its
/// asks included (craze's fold child-ignores all four ask kinds) — but its
/// ask IS an approval (the engine's registry has no agent field, Amendment
/// A11), its `detail` the body's alone.
#[test]
fn a_sub_agents_events_draw_nothing_but_its_ask_is_an_approval() {
    let mut f = fold();
    f.apply(&text("main "));
    f.apply(&ev(
        "text",
        json!({"agent": "child-1", "text": "child talk"}),
    ));
    f.apply(&ev(
        "tool",
        json!({"agent": "child-1", "tool": {"id": "c", "status": "completed"}}),
    ));
    f.apply(&ev(
        "turn",
        json!({"agent": "child-1", "turn": {"id": "x", "phase": "started", "text": "x"}}),
    ));
    f.apply(&text("still"));
    assert!(f.drain().is_empty(), "the main streak is still open");
    let mut perm = permission("child-perm");
    perm["agent"] = json!("child-1");
    f.apply(&perm);
    assert!(
        f.drain().is_empty(),
        "a sub-agent's ask neither draws nor ends the main streak"
    );
    let a = f.approval("child-perm").expect("an approval all the same");
    assert!(a.status.is_pending());
    assert_eq!(a.detail, None, "the body's detail: no sub-agent named");
    assert_eq!(f.open_approvals(), 1);
    f.flush();
    assert_eq!(texts(&f.drain()), ["main still"]);
}

// ---- sub-agents' asks: rows from the transcript, approvals from the registry ----
//
// craze's fold (`internal/transcript/fold.go`'s table at the pin) routes an
// event with `Agent` set to the child's transcript, and all four ask kinds —
// `EventPermission`, `EventQuestion`, `EventPlan`, `EventAsk` — are
// `childIgnored` there: no transcript row, and no snapshot carries the ask.
// The ENGINE's registry (`asks.list`) holds it all the same — it has no agent
// field — so it is an approval live AND seeded (Amendment A11): each cell
// folds the live sub-agent event, and seeds the equivalent (the snapshot
// craze would cut, which lacks it, plus the registry, which has it).

/// An `asks.get` record of `kind`'s `payload`, open, opened at `AT`.
fn record(kind: &str, payload: &Value) -> AskRecord {
    serde_json::from_value(json!({"id": payload["id"], "kind": kind, "status": "open",
        "body": { kind: payload }, "openedAt": AT}))
    .unwrap()
}

/// `live_events` folded one by one, and `snapshot` restored with `registry`
/// adopted, draw the same rows and hold the same open approvals.
fn same_live_and_seeded(live_events: &[Value], snapshot: &Value, registry: &[AskRecord]) {
    let mut live = fold();
    for e in live_events {
        live.apply(e);
    }
    live.flush();
    let mut seeded = fold();
    seeded.restore(snapshot).unwrap();
    seeded.adopt_registry(registry);
    seeded.flush();
    assert_eq!(live.drain(), seeded.drain(), "the rows");
    assert_eq!(
        live.pending_approvals(),
        seeded.pending_approvals(),
        "the approvals"
    );
    assert_eq!(live.open_approvals(), seeded.open_approvals());
}

/// The snapshot craze cuts after each cell's events, with these open
/// transcript `asks` and no entries.
fn snapshot_with_asks(asks: Value) -> Value {
    let mut snapshot = json!({"version": 1, "incarnation": "I", "seq": 1, "main": {"entries": []}});
    snapshot["asks"] = asks;
    snapshot
}

fn by_sub_agent(mut event: Value) -> Value {
    event["agent"] = json!("child-1");
    event
}

#[test]
fn a_sub_agents_permission_is_an_approval_live_and_seeded() {
    let open = permission("perm-sub");
    same_live_and_seeded(
        &[by_sub_agent(open.clone())],
        &snapshot_with_asks(json!([])),
        &[record("permission", &open["permission"])],
    );
    let mut live = fold();
    live.apply(&by_sub_agent(open));
    assert_eq!(live.open_approvals(), 1, "not vacuous: it is pending");
}

#[test]
fn a_sub_agents_question_is_an_approval_live_and_seeded() {
    let q = json!({"id": "q-sub", "title": "Pick", "questions": [
        {"id": "q1", "prompt": "Which?", "options": [{"id": "a", "label": "A"}]}]});
    same_live_and_seeded(
        &[by_sub_agent(ev(
            "question",
            json!({ "question": q.clone() }),
        ))],
        &snapshot_with_asks(json!([])),
        &[record("question", &q)],
    );
}

/// A sub-agent's plan: an approval, but no plan entry row — that is the
/// MAIN transcript's (A10 is the main agent's).
#[test]
fn a_sub_agents_plan_is_an_approval_and_no_entry_live_and_seeded() {
    let plan = json!({"id": "pl-sub", "name": "Sub", "overview": "o"});
    same_live_and_seeded(
        &[by_sub_agent(ev("plan", json!({ "plan": plan.clone() })))],
        &snapshot_with_asks(json!([])),
        &[record("plan", &plan)],
    );
}

/// A sub-agent's `ask` ending resolves its approval and draws nothing — for
/// a main ask, too: craze's transcript keeps that ask open (its snapshot
/// still carries it, so its pending row stands), while the registry has
/// resolved it (so it is no approval).
#[test]
fn a_sub_agents_ask_ending_resolves_and_draws_nothing_live_and_seeded() {
    let open = permission("perm-main");
    same_live_and_seeded(
        &[
            open.clone(),
            by_sub_agent(ask_end(
                "perm-main",
                "permission",
                json!({"optionId": "a1"}),
            )),
        ],
        &snapshot_with_asks(json!([{"id": "perm-main", "kind": "permission", "at": AT,
            "body": {"permission": open["permission"].clone()}}])),
        &[],
    );
    let sub_open = by_sub_agent(permission("perm-sub"));
    let mut live = fold();
    live.apply(&sub_open);
    live.apply(&by_sub_agent(ask_end(
        "perm-sub",
        "permission",
        json!({"optionId": "a1"}),
    )));
    assert!(live.drain().is_empty());
    assert_eq!(
        live.approval("perm-sub").map(|a| a.status.is_pending()),
        Some(false),
        "resolved"
    );
}

/// A MAIN agent's ask is unchanged: its pending row live and from the
/// snapshot, and its approval live and from the registry.
#[test]
fn a_main_agents_ask_is_the_same_live_and_seeded() {
    let open = permission("perm-main");
    same_live_and_seeded(
        std::slice::from_ref(&open),
        &snapshot_with_asks(json!([{"id": "perm-main", "kind": "permission", "at": AT,
            "body": {"permission": open["permission"].clone()}}])),
        &[record("permission", &open["permission"])],
    );
    let mut live = fold();
    live.apply(&open);
    assert_eq!(texts(&live.drain()), ["Run `git push`"], "not vacuous");
}

/// `asks.get` saying `resolved` (the ask ended between `asks.list` and
/// `asks.get`) — or an automatic body — is no approval; a snapshot ask the
/// registry does not hold keeps its row but is no approval either.
#[test]
fn a_registry_record_that_is_not_open_is_no_approval() {
    let open = permission("perm-1");
    let mut resolved = record("permission", &open["permission"]);
    resolved.status = "resolved".into();
    let auto = record(
        "question",
        &json!({"id": "q-auto", "auto": true, "questions": []}),
    );
    let mut f = fold();
    f.restore(&snapshot_with_asks(
        json!([{"id": "perm-1", "kind": "permission", "at": AT,
        "body": {"permission": open["permission"].clone()}}]),
    ))
    .unwrap();
    assert!(!f.adopt_registry(&[resolved, auto]));
    assert_eq!(f.open_approvals(), 0);
    assert!(f.approval("perm-1").is_none());
    assert_eq!(
        texts(&f.drain()),
        ["Run `git push`"],
        "the transcript's row"
    );
}

/// The registry is the membership on a RESUME too: a pending approval it
/// no longer lists was resolved while the lane looked away — resolved here,
/// so the client is told; one it lists that the fold never saw is opened.
#[test]
fn adopting_the_registry_resolves_what_it_no_longer_lists() {
    let mut f = fold();
    f.apply(&permission("perm-1"));
    let _ = f.drain();
    let _ = f.take_approvals_changed();
    let other = json!({"id": "perm-2", "tool": "Write", "options": []});
    assert!(f.adopt_registry(&[record("permission", &other)]));
    assert!(f.take_approvals_changed());
    assert_eq!(
        f.approval("perm-1").map(|a| a.status.is_pending()),
        Some(false)
    );
    assert!(f.approval("perm-2").is_some_and(|a| a.status.is_pending()));
    assert!(f.drain().is_empty(), "no rows either way");
    assert!(
        !f.adopt_registry(&[record("permission", &other)]),
        "idempotent"
    );
}

/// **A hidden kind is never an approval**, live or seeded — craze's own
/// `HiddenBy`: a question without `askCards`, a plan without `planCards` —
/// and draws no card row; a main plan's ENTRY row is the transcript's, and
/// stays. A permission is never hidden (the next cell).
#[test]
fn a_hidden_question_or_plan_is_never_an_approval_live_or_seeded() {
    let perm = permission("perm-h");
    let q = json!({"id": "q-h", "title": "Pick", "questions": []});
    let plan = json!({"id": "pl-h", "name": "Hidden", "overview": "o"});
    for cards in [
        Cards {
            questions: false,
            plans: true,
        },
        Cards {
            questions: true,
            plans: false,
        },
    ] {
        let mut live = fold();
        live.set_cards(cards);
        live.apply(&perm);
        live.apply(&ev("question", json!({ "question": q.clone() })));
        live.apply(&ev("plan", json!({ "plan": plan.clone() })));
        let rows = live.drain();
        let mut seeded = fold();
        seeded.set_cards(cards);
        seeded.restore(&snapshot_with_asks(json!([]))).unwrap();
        seeded.adopt_registry(&[
            record("permission", &perm["permission"]),
            record("question", &q),
            record("plan", &plan),
        ]);
        let shown: Vec<String> = live.pending_approvals().into_iter().map(|a| a.id).collect();
        let seeded_shown: Vec<String> = seeded
            .pending_approvals()
            .into_iter()
            .map(|a| a.id)
            .collect();
        if cards.questions {
            assert_eq!(shown, ["perm-h", "q-h"], "{cards:?}");
            assert_eq!(texts(&rows), ["Run `git push`", "Pick", "plan Hidden\no"]);
        } else {
            assert_eq!(shown, ["perm-h", "pl-h"], "{cards:?}");
            assert_eq!(texts(&rows), ["Run `git push`", "plan Hidden\no", "Hidden"]);
        }
        assert_eq!(seeded_shown, shown, "{cards:?}: seeded as live");
    }
}

/// **A permission is never hidden** (craze's `HiddenBy` has no permission
/// case: an agent blocked on one would be stranded): on a session WITHOUT
/// `askCards` — or `planCards` — it is still an approval with its card row,
/// live and seeded, while a question there is hidden.
#[test]
fn a_permission_is_never_hidden_live_or_seeded() {
    let perm = permission("perm-1");
    let q = json!({"id": "q-1", "title": "Pick", "questions": []});
    let none = Cards {
        questions: false,
        plans: false,
    };
    let mut live = fold();
    live.set_cards(none);
    live.apply(&perm);
    live.apply(&ev("question", json!({ "question": q.clone() })));
    assert_eq!(texts(&live.drain()), ["Run `git push`"], "its card row");
    let mut seeded = fold();
    seeded.set_cards(none);
    seeded.restore(&snapshot_with_asks(json!([]))).unwrap();
    seeded.adopt_registry(&[
        record("permission", &perm["permission"]),
        record("question", &q),
    ]);
    for f in [&live, &seeded] {
        let open: Vec<String> = f.pending_approvals().into_iter().map(|a| a.id).collect();
        assert_eq!(open, ["perm-1"], "the permission, not the question");
    }
}

/// An ending for an ask the book never opened (an automatic one) draws
/// nothing.
#[test]
fn an_unknown_asks_ending_draws_nothing() {
    let mut f = fold();
    f.apply(&ask_end(
        "never",
        "question",
        json!({"outcome": "automatic"}),
    ));
    assert!(f.drain().is_empty());
}

/// An `asks.get` record is the approval the book opens for the same ask —
/// `answer` reads one this way when the fold does not hold it — less the
/// opening event's time; an automatic one is none.
#[test]
fn an_ask_record_is_the_approval_the_book_opens() {
    let mut f = fold();
    let opened = permission("p1");
    f.apply(&opened);
    let booked = f.approval("p1").cloned().unwrap();
    let record: AskRecord = serde_json::from_value(json!({"id": "p1", "kind": "permission",
        "status": "open", "body": {"permission": opened["permission"].clone()}}))
    .unwrap();
    let (approval, opening) = record_approval(HOST, &record).unwrap();
    assert_eq!(
        approval,
        LaneApproval {
            created_at_unix_ms: None,
            ..booked
        }
    );
    assert_eq!(opening.kind_word(), "permission");
    let auto: AskRecord = serde_json::from_value(json!({"id": "q1", "kind": "question",
        "body": {"question": {"id": "q1", "auto": true, "questions": []}}}))
    .unwrap();
    assert!(record_approval(HOST, &auto).is_none());
}

// ---- meta and settings ----

#[test]
fn meta_sections_are_settings_and_draw_no_row() {
    let mut f = fold();
    f.apply(&ev(
        "meta",
        json!({"state": {"model": "fast", "title": "t"}}),
    ));
    assert!(f.drain().is_empty());
    assert!(f.take_settings_changed());
    assert!(!f.take_settings_changed());
    assert_eq!(f.settings().lane_settings().model.as_deref(), Some("fast"));
}

// ---- snapshots ----

/// Snapshot → rows: the ledger row at the top, each entry kind its row, an
/// assistant entry split at 8 KiB, a terminal tool its use and result; the
/// open asks booked after the entries; `truncated` from the window.
#[test]
fn a_snapshot_becomes_rows() {
    let mut f = fold();
    let big = "z".repeat(crate::segment::MAX_STREAK_BYTES + 10);
    let snap = json!({"version": 1, "incarnation": "I", "seq": 9,
        "main": {"windowed": true, "omitted": [[100], [20, "t0"]], "todoPlanned": 2, "entries": [
            {"id": "1.0", "kind": "user", "text": "do it", "at": AT},
            {"id": "2.0", "kind": "thought", "text": "hmm"},
            {"id": "3.0", "kind": "assistant", "text": big},
            {"id": "4.0", "kind": "tool", "tool": {"id": "t1", "title": "Run", "toolName": "shell", "status": "cancelled"}},
            {"id": "5.0", "kind": "note", "text": "tasks: 2 planned"},
            {"id": "6.0", "kind": "plan", "plan": {"id": "pl", "name": "P", "overview": "o"}},
            {"id": "7.0", "kind": "error", "text": "boom", "err": {"class": "other", "code": 0}},
            {"id": "8.0", "kind": "a_kind_from_2027", "text": "?"}]},
        "asks": [{"id": "perm-1", "kind": "permission", "truncated": true,
                  "body": {"permission": {"id": "perm-1", "tool": "Shell", "options": []}}},
                 {"id": "q-auto", "kind": "question", "body": {"question": {"id": "q-auto", "auto": true}}}]});
    let r = f.restore(&snap).unwrap();
    assert_eq!((r.incarnation.as_str(), r.seq, r.truncated), ("I", 9, true));
    assert_eq!(r.truncated_asks, ["perm-1"]);
    let rows = f.drain();
    let t = texts(&rows);
    assert_eq!(t[0], "earlier transcript omitted");
    assert_eq!(t[1..3], ["do it".to_string(), "hmm".to_string()]);
    assert_eq!(t[3].len() + t[4].len(), big.len(), "split losslessly");
    assert_eq!(kinds(&rows)[5], ("tool".into(), "tool_use".into()));
    assert_eq!(t[6], "cancelled", "cancelled is terminal");
    assert_eq!(
        t[7..10],
        [
            "tasks: 2 planned".to_string(),
            "plan P\no".to_string(),
            "boom".to_string()
        ]
    );
    assert_eq!(
        t[10], "Shell",
        "the open ask's pending row, after the entries"
    );
    assert_eq!(rows.len(), 11);
    assert_eq!(
        f.open_approvals(),
        0,
        "a restore books no approvals: the registry is the membership (A11)"
    );
    let perm = json!({"id": "perm-1", "tool": "Shell", "options": []});
    assert!(f.adopt_registry(&[record("permission", &perm)]));
    assert_eq!(f.open_approvals(), 1, "the registry's open ask");
    assert!(f.drain().is_empty(), "adopting the registry draws no row");
    // The restored tool absorbs its own later update.
    f.apply(&tool("t1", "cancelled", json!({})));
    assert!(f.drain().is_empty());
    // And the todo dedupe continues from the snapshot's.
    f.apply(&ev(
        "todos",
        json!({"todos": [{"status": "pending"}, {"status": "pending"}]}),
    ));
    assert!(f.drain().is_empty());
}

/// `streamOpen` with a streaming last entry: the run continues with the next
/// chunk of its kind (no second row); an open ask's row comes before it.
#[test]
fn an_open_run_continues_after_a_restore() {
    let mut f = fold();
    let snap = json!({"version": 1, "incarnation": "I", "seq": 3, "main": {"streamOpen": true, "tailCut": true, "entries": [
        {"id": "1.0", "kind": "assistant", "text": "…tail of a long run ", "streaming": true, "at": AT}]},
        "asks": [{"id": "p", "kind": "permission", "body": {"permission": {"id": "p", "tool": "T"}}}]});
    f.restore(&snap).unwrap();
    assert_eq!(texts(&f.drain()), ["T"], "the run is still open");
    f.apply(&text("continues"));
    f.flush();
    assert_eq!(texts(&f.drain()), ["…tail of a long run continues"]);
}

/// The window dropped every entry, the open run's too (`omittedRun`): chunks
/// of that kind draw nothing until the run ends; anything else ends it.
#[test]
fn an_omitted_run_draws_nothing_until_it_ends() {
    let mut f = fold();
    let snap = json!({"version": 1, "incarnation": "I", "seq": 3, "main": {"streamOpen": true, "omittedRun": "assistant", "omitted": [[5000]]}});
    f.restore(&snap).unwrap();
    assert_eq!(texts(&f.drain()), ["earlier transcript omitted"]);
    f.apply(&text("the rest of a run this client never saw"));
    f.apply(&ev("thought", json!({"text": "a new run"})));
    f.apply(&text("a fresh reply"));
    f.flush();
    assert_eq!(texts(&f.drain()), ["a new run", "a fresh reply"]);
}

#[test]
fn a_snapshot_of_another_codec_is_refused() {
    let mut f = fold();
    assert!(f.restore(&json!({"version": 2, "main": {}})).is_err());
    assert!(f.restore(&json!([1])).is_err());
}

/// A seed capped to what the ring keeps leads with the omitted row.
#[test]
fn cap_pending_keeps_the_newest_rows_under_one_omitted_row() {
    let mut f = fold();
    for i in 0..10 {
        f.apply(&tool(&format!("t{i}"), "pending", json!({})));
    }
    f.cap_pending(4);
    let rows = f.drain();
    assert_eq!(rows.len(), 4);
    assert_eq!(rows[0].text.as_deref(), Some("earlier transcript omitted"));
    assert_eq!(
        rows[3].tool.as_ref().unwrap().detail.as_deref(),
        Some("Run t9")
    );
}

/// A malformed event never kills the fold.
#[test]
fn a_malformed_event_is_ignored() {
    let mut f = fold();
    for v in [
        json!(null),
        json!([1]),
        json!({"type": 5}),
        json!({"type": "tool", "tool": "x"}),
        json!({"type": "permission", "permission": {"id": 5}}),
        json!({"type": "ask"}),
    ] {
        f.apply(&v);
    }
    assert!(f.drain().is_empty());
}
