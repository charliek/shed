//! The hub-vs-port PIN: replay `crates/fixtures/jsonl/opencode_turn.jsonl`
//! through THIS crate's fold and assert it still equals what the rc hub's fold
//! produced on the same input.
//!
//! `fixtures/opencode_turn.golden.json` was recorded once, from
//! `shed_broker::rc_hub::watch_opencode::OpencodeFold`, by a throwaway harness
//! that ran both folds and checked they agreed (they did, first run). The
//! harness is gone and **this test must never take a `shed-broker`
//! dependency** — a port that links the thing it ported proves nothing, and the
//! hub's copy is scheduled for deletion. The golden file is the pin.
//!
//! See `fixtures/README.md` for the canonical projection and for what the
//! fixture does not reach (approvals, questions, `session.error`, `note_gap`,
//! `reset` — hand-authored unit inputs in `src/fold.rs` cover those).

use serde_json::{json, Value};
use shed_core::rc::RcFeedMessage;
use shed_opencode::OpencodeFold;

fn fixture_lines() -> Vec<Vec<u8>> {
    let path = format!(
        "{}/../fixtures/jsonl/opencode_turn.jsonl",
        env!("CARGO_MANIFEST_DIR")
    );
    let data = std::fs::read(&path).expect("fixture readable");
    data.split(|&b| b == b'\n')
        .filter(|l| !l.iter().all(u8::is_ascii_whitespace))
        .map(<[u8]>::to_vec)
        .collect()
}

/// One feed row reduced to the fields the hub's `FeedMessage` and the port's
/// [`RcFeedMessage`] share, with the port's `Option`s flattened to the hub's
/// plain strings (`None` ⇒ `""`).
fn row(m: &RcFeedMessage) -> Value {
    let (tool_name, tool_detail) = m
        .tool
        .as_ref()
        .map(|t| {
            (
                t.name.clone().unwrap_or_default(),
                t.detail.clone().unwrap_or_default(),
            )
        })
        .unwrap_or_default();
    let (id, status, decision, decisions) = m
        .approval
        .as_ref()
        .map(|a| {
            (
                a.id.clone(),
                a.status.clone(),
                a.decision.clone().unwrap_or_default(),
                a.decisions.clone(),
            )
        })
        .unwrap_or_default();
    json!({
        "seq": m.seq,
        "ts": m.ts.clone().unwrap_or_default(),
        "role": m.role,
        "type": m.msg_type,
        "text": m.text.clone().unwrap_or_default(),
        "tool_name": tool_name,
        "tool_detail": tool_detail,
        "approval_id": id,
        "approval_status": status,
        "approval_decision": decision,
        "approval_decisions": decisions,
    })
}

/// The whole replay as the golden's canonical projection.
fn projection(lines: &[Vec<u8>]) -> Value {
    let mut f = OpencodeFold::new();
    let mut steps = Vec::new();
    for (i, line) in lines.iter().enumerate() {
        let applied = f.apply_line(line);
        let rows: Vec<Value> = f.drain_messages().iter().map(row).collect();
        steps.push(json!({
            "line": i,
            "applied": applied,
            "activity": f.activity().as_str(),
            "rows": rows,
        }));
    }
    let pending: Vec<Value> = f
        .pending_approvals()
        .iter()
        .map(|a| json!({"id": a.id, "status": a.status.as_str()}))
        .collect();
    json!({
        "steps": steps,
        "final": {
            "activity": f.activity().as_str(),
            "last_message": f.last_message(),
            "open_approvals": f.open_approvals(),
            "pending_approvals": pending,
        },
    })
}

fn golden() -> Value {
    let path = format!(
        "{}/fixtures/opencode_turn.golden.json",
        env!("CARGO_MANIFEST_DIR")
    );
    let data = std::fs::read_to_string(&path).expect("golden readable");
    let mut v: Value = serde_json::from_str(&data).expect("golden is JSON");
    // The two prose keys are documentation for a human opening the file, not
    // part of what the fold produces.
    let o = v.as_object_mut().expect("golden is an object");
    o.remove("_comment");
    o.remove("fixture");
    v
}

#[test]
fn port_matches_the_hub_fold_on_the_recorded_turn() {
    let got = projection(&fixture_lines());
    let want = golden();
    // Compared as pretty JSON so a failure diff is readable line-by-line rather
    // than one enormous `Value` debug blob.
    assert_eq!(
        serde_json::to_string_pretty(&got).unwrap(),
        serde_json::to_string_pretty(&want).unwrap(),
        "the fold's output drifted from the hub-recorded golden \
         (fixtures/opencode_turn.golden.json). If the change is deliberate, \
         re-record it and say which rule changed."
    );
}

/// The reseed-idempotency rule the whole dedup set exists for: replaying the
/// exact same stream into a fold that was NOT reset emits nothing the second
/// time, and leaves the verdict where it was.
#[test]
fn replaying_the_turn_on_a_kept_fold_emits_nothing() {
    let lines = fixture_lines();
    let mut f = OpencodeFold::new();
    for line in &lines {
        f.apply_line(line);
    }
    let first = f.drain_messages();
    assert_eq!(first.len(), 5, "rows: {first:?}");
    let before = f.activity();

    // A gap, then the full replay a reconnect's seed would push.
    f.note_gap();
    for line in &lines {
        f.apply_line(line);
    }
    assert!(
        f.drain_messages().is_empty(),
        "a reseed on a KEPT fold must emit no duplicate rows"
    );
    assert_eq!(f.activity(), before);
}

/// …and the mirror image: `reset()` forgets everything, so the SAME stream
/// replays in full. That is what makes a `Reset` … `Ready` generation a
/// complete seed rather than a silent no-op.
#[test]
fn replaying_the_turn_after_reset_emits_the_whole_turn_again() {
    let lines = fixture_lines();
    let mut f = OpencodeFold::new();
    for line in &lines {
        f.apply_line(line);
    }
    let first = f.drain_messages();

    f.reset();
    assert_eq!(f.activity(), shed_core::rc::RcActivity::Unknown);
    for line in &lines {
        f.apply_line(line);
    }
    let second = f.drain_messages();
    assert_eq!(first, second);
}
