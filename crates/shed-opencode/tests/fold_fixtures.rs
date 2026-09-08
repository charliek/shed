//! Two replay goldens over this crate's fold. **They carry DIFFERENT claims and
//! this file must never blur them:**
//!
//! - `fixtures/opencode_turn.golden.json` (C2) is a **fidelity** pin. It is a
//!   recording of what the rc hub's fold
//!   (`shed_broker::rc_hub::watch_opencode::OpencodeFold`) produced on
//!   `crates/fixtures/jsonl/opencode_turn.jsonl` — a 32-line opencode **1.17.15**
//!   turn — so the port matching it is evidence the port changed no behavior.
//!   Because the OTHER program minted it, it is **not** regenerable from this
//!   crate: see [`c2_golden_is_not_regenerable`].
//! - `fixtures/1.18.29/fold.golden.json` (C3b) is a **regression-detection** pin
//!   on the port's OWN behavior over the 155-frame opencode **1.18.29**
//!   recording. Nobody else minted it; it says "this is what the fold does
//!   today", and a diff on it is a behavior change to review, not proof of
//!   agreement with anything. **One subset of it is also fidelity:** a throwaway
//!   harness with a temporary `shed-broker` dev-dep ran both folds over the same
//!   recording and asserted the TRANSCRIPT projections byte-identical (three
//!   deliberate divergences excluded by construction — see `fixtures/README.md`
//!   § "the 1.18.29 replay"). The approval DTOs and the `session.error` status
//!   row in this golden have no hub counterpart and are the port's alone.
//!
//! **Neither test may take a `shed-broker` dependency** — a port that links the
//! thing it ported proves nothing, and the hub's copy is scheduled for deletion.
//! The golden files are the pins.
//!
//! ## Regenerating
//!
//! ```text
//! SHED_OPENCODE_REGOLD=1 cargo test -p shed-opencode --test fold_fixtures
//! ```
//!
//! rewrites `fixtures/1.18.29/fold.golden.json` from the COMMITTED recording —
//! offline, free, deterministic, no credentials — and then asserts against what
//! it wrote, so a regen that does not round-trip still fails. Re-recording the
//! wire itself is a separate, live-server step; `fixtures/README.md` documents
//! both and which one costs money.

use serde_json::{json, Map, Value};
use shed_core::lane::LaneApprovalKind;
use shed_core::rc::RcFeedMessage;
use shed_opencode::OpencodeFold;

/// Set to rewrite the regenerable goldens instead of only asserting them.
const REGOLD_ENV: &str = "SHED_OPENCODE_REGOLD";

fn regold() -> bool {
    std::env::var_os(REGOLD_ENV).is_some_and(|v| !v.is_empty() && v != "0")
}

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

/// Reads a golden and drops the prose keys, which are documentation for a human
/// opening the file rather than part of what the fold produces.
fn read_golden(rel: &str) -> Value {
    let path = format!("{}/{rel}", env!("CARGO_MANIFEST_DIR"));
    let data = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{rel} readable: {e}"));
    let mut v: Value = serde_json::from_str(&data).expect("golden is JSON");
    let o = v.as_object_mut().expect("golden is an object");
    o.remove("_comment");
    o.remove("fixture");
    v
}

fn golden() -> Value {
    read_golden("fixtures/opencode_turn.golden.json")
}

/// The C2 golden is what the HUB's fold emitted. Regenerating it from this
/// crate would replace a claim about another program's output with a claim about
/// our own — the test would then assert the port against itself and pass through
/// any drift. So `SHED_OPENCODE_REGOLD` deliberately does NOT touch it, and this
/// test states that in the suite rather than only in prose.
///
/// The honest way to re-record it is the way it was recorded: a throwaway
/// harness with a temporary `shed-broker` dev-dep running both folds (see
/// `fixtures/README.md`).
#[test]
fn c2_golden_is_not_regenerable() {
    let path = format!(
        "{}/fixtures/opencode_turn.golden.json",
        env!("CARGO_MANIFEST_DIR")
    );
    let before = std::fs::read(&path).expect("golden readable");
    // Drive the C2 golden's whole code path under whatever the environment says
    // — it is the only path that could touch the file — then prove it did not.
    let got = projection(&fixture_lines());
    assert_eq!(
        serde_json::to_string_pretty(&got).unwrap(),
        serde_json::to_string_pretty(&golden()).unwrap(),
        "see port_matches_the_hub_fold_on_the_recorded_turn"
    );
    assert_eq!(
        before,
        std::fs::read(&path).expect("golden readable"),
        "{REGOLD_ENV} must never rewrite the hub-recorded golden"
    );
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

// ---------------------------------------------------------------------------
// The 1.18.29 replay golden — the port's OWN behavior over real 1.18.29 wire.
// ---------------------------------------------------------------------------

const RECORDING: &str = "fixtures/1.18.29/event-frames.jsonl";
const REPLAY_GOLDEN: &str = "fixtures/1.18.29/fold.golden.json";

/// The prose header the regen writes back, so a human opening the file learns
/// which claim it carries before reading a single row.
fn replay_header() -> Vec<(&'static str, Value)> {
    vec![
        (
            "_comment",
            json!(
                "REGRESSION PIN on shed-opencode's OWN fold, replayed over the committed \
                 opencode 1.18.29 recording. Unlike fixtures/opencode_turn.golden.json this \
                 is NOT a hub recording and NOT a fidelity proof: nothing but this crate \
                 minted it, so a diff here is a behavior change to review, never evidence \
                 of agreement with the rc hub. (The transcript subset WAS separately proven \
                 identical to the hub's fold — see fixtures/README.md.) Regenerate offline \
                 with SHED_OPENCODE_REGOLD=1 cargo test -p shed-opencode --test fold_fixtures."
            ),
        ),
        ("fixture", json!(RECORDING)),
    ]
}

fn recording_lines() -> Vec<Vec<u8>> {
    let path = format!("{}/{RECORDING}", env!("CARGO_MANIFEST_DIR"));
    let data = std::fs::read(&path).expect("recording readable");
    data.split(|&b| b == b'\n')
        .filter(|l| !l.iter().all(u8::is_ascii_whitespace))
        .map(<[u8]>::to_vec)
        .collect()
}

/// The envelope `type` of one recorded line — used to label a step and to spot
/// the two ask events whose approval DTOs the golden pins.
fn event_type(line: &[u8]) -> String {
    serde_json::from_slice::<Value>(line)
        .ok()
        .and_then(|v| v.get("type").and_then(Value::as_str).map(str::to_string))
        .unwrap_or_default()
}

/// `(kind, id)` when this line is an addressable approval ask, else `None`.
fn ask_key(line: &[u8]) -> Option<(LaneApprovalKind, String)> {
    let v: Value = serde_json::from_slice(line).ok()?;
    let kind = match v.get("type").and_then(Value::as_str)? {
        "permission.asked" => LaneApprovalKind::Permission,
        "question.asked" => LaneApprovalKind::Question,
        _ => return None,
    };
    let id = v.get("properties")?.get("id")?.as_str()?;
    (!id.is_empty()).then(|| (kind, id.to_string()))
}

fn lane_value(a: &shed_core::lane::LaneApproval) -> Value {
    serde_json::to_value(a).expect("LaneApproval serializes")
}

/// The replay's canonical projection.
///
/// Per recorded line: the input's `event` type (a label, so a golden diff reads
/// as prose rather than as line numbers), `apply_line`'s return, the verdict
/// after it, `open_approvals`, the rows drained after it as the **full**
/// [`RcFeedMessage`] wire shape (this golden pins the port, so nothing is reduced
/// to a shared subset the way C2's is), and the still-open approvals as
/// `{kind, id, status}`.
///
/// Then `approvals`: for every addressable ask in the recording, the FULL
/// [`shed_core::lane::LaneApproval`] DTO as it stood the moment it was asked and
/// again at the end of the replay — which is what pins the payload, the
/// structured questions, and `shed_payload`'s tombstone-on-resolution.
///
/// Then `final`: the terminal verdict, last message, open count and pending
/// snapshot.
fn replay_projection(lines: &[Vec<u8>]) -> Value {
    let mut f = OpencodeFold::new();
    let mut steps = Vec::new();
    let mut approvals = Vec::new();

    for (i, line) in lines.iter().enumerate() {
        let event = event_type(line);
        let applied = f.apply_line(line);
        let rows: Vec<Value> = f
            .drain_messages()
            .iter()
            .map(|m| serde_json::to_value(m).expect("RcFeedMessage serializes"))
            .collect();
        let pending: Vec<Value> = f
            .pending_approvals()
            .iter()
            .map(|a| json!({"kind": a.kind.as_str(), "id": a.id, "status": a.status.as_str()}))
            .collect();
        steps.push(json!({
            "line": i,
            "event": event,
            "applied": applied,
            "activity": f.activity().as_str(),
            "open_approvals": f.open_approvals(),
            "rows": rows,
            "pending": pending,
        }));
        if let Some((kind, id)) = ask_key(line) {
            let asked = f
                .approval(kind.clone(), &id)
                .unwrap_or_else(|| panic!("line {i} asked {id} but the fold does not track it"));
            approvals.push((i, kind, id, lane_value(&asked)));
        }
    }

    let approvals: Vec<Value> = approvals
        .into_iter()
        .map(|(line, kind, id, asked)| {
            let end = f
                .approval(kind.clone(), &id)
                .map(|a| lane_value(&a))
                .unwrap_or(Value::Null);
            json!({
                "asked_at_line": line,
                "kind": kind.as_str(),
                "id": id,
                "asked": asked,
                "final": end,
            })
        })
        .collect();

    let pending: Vec<Value> = f.pending_approvals().iter().map(lane_value).collect();
    json!({
        "steps": steps,
        "approvals": approvals,
        "final": {
            "activity": f.activity().as_str(),
            "last_message": f.last_message(),
            "open_approvals": f.open_approvals(),
            "pending_approvals": pending,
        },
    })
}

/// Writes `projection` back to the golden, prose header first, in exactly the
/// bytes the assert path reads: `serde_json` orders object keys, so a regen on
/// an unchanged fold reproduces the committed file byte-for-byte.
fn write_replay_golden(projection: &Value) {
    let mut out = Map::new();
    for (k, v) in replay_header() {
        out.insert(k.to_string(), v);
    }
    for (k, v) in projection.as_object().expect("projection is an object") {
        out.insert(k.clone(), v.clone());
    }
    let path = format!("{}/{REPLAY_GOLDEN}", env!("CARGO_MANIFEST_DIR"));
    let body = serde_json::to_string_pretty(&Value::Object(out)).expect("golden serializes");
    std::fs::write(&path, format!("{body}\n")).expect("golden writable");
    eprintln!("{REGOLD_ENV}: rewrote {REPLAY_GOLDEN}");
}

/// **A REGRESSION pin, not a fidelity proof** (see the module doc): this is what
/// THIS crate's fold does to the committed opencode 1.18.29 wire, including the
/// approval and question paths the 1.17.15 fixture could not reach and the
/// `session.error` status row the hub has no counterpart for.
#[test]
fn replays_the_1_18_29_recording() {
    let lines = recording_lines();
    assert_eq!(lines.len(), 155, "the committed recording changed size");
    let got = replay_projection(&lines);
    if regold() {
        write_replay_golden(&got);
    }
    // Read back from disk either way: under regen this proves the write
    // round-trips, and normally it is the assertion itself.
    assert_eq!(
        serde_json::to_string_pretty(&got).unwrap(),
        serde_json::to_string_pretty(&read_golden(REPLAY_GOLDEN)).unwrap(),
        "the fold's output drifted from {REPLAY_GOLDEN}. If the change is \
         deliberate, re-derive it offline with {REGOLD_ENV}=1 and review the \
         diff as the substantive change."
    );
}

/// The recording is the first fixture that reaches the approval and question
/// paths with REAL wire, so the suite says out loud what it now covers — a
/// silently-truncated re-record must not quietly drop that coverage.
#[test]
fn the_recording_covers_the_approval_and_question_paths() {
    let lines = recording_lines();
    let types: Vec<String> = lines.iter().map(|l| event_type(l)).collect();
    for want in [
        "permission.asked",
        "permission.replied",
        "question.asked",
        "question.replied",
        "session.error",
        "message.part.delta",
    ] {
        assert!(
            types.iter().any(|t| t == want),
            "the recording no longer carries {want}"
        );
    }

    let mut f = OpencodeFold::new();
    let mut saw_needs_approval = false;
    for line in &lines {
        f.apply_line(line);
        saw_needs_approval |= f.activity() == shed_core::rc::RcActivity::NeedsApproval;
    }
    assert!(
        saw_needs_approval,
        "the replay never reached needs_approval — the approval path is unproven"
    );
    assert_eq!(
        f.open_approvals(),
        0,
        "every ask in the recording was answered, so none may still be open"
    );
}
