//! The fold's tests — every **pure-fold** case plan 017 §3.4 names by hand, so
//! an implementer cannot skip one.
//!
//! The cases that need the watcher (the escalation ladder, a partial streak
//! across an EOF, the seed/live overlap) are C3's; everything here runs against
//! the fold alone, with no wire and no clock.

use serde_json::json;
use shed_core::lane::feed::{FEED_TRUNC_MARKER, MAX_FEED_MESSAGE_BYTES};
use shed_core::lane::ring::MessageRing;
use shed_core::lane::LaneDecision;

use super::*;

/// A real session id shape: a UUID, which is exactly why an event id is split
/// at its LAST hyphen and not its first.
const SID: &str = "01a0fa1e-0000-7000-8000-0000000000ab";

fn eid(n: u64) -> String {
    format!("{SID}-{n}")
}

fn envelope(v: serde_json::Value) -> GxEnvelope {
    serde_json::from_value(v).expect("the envelope decodes")
}

/// A chunk envelope. `prompt` of `None` writes `"promptId": null`, which is
/// what a `user_message_chunk` really carries.
fn chunk(n: u64, kind: &str, text: &str, prompt: Option<&str>) -> GxEnvelope {
    envelope(json!({
        "eventId": eid(n),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": kind, "content": { "type": "text", "text": text } },
            "_meta": { "agentTimestampMs": 1_788_927_621_024i64, "promptId": prompt },
        },
        "timestamp": 1_788_927_622i64,
    }))
}

/// One of the fifteen-in-forty `hook_execution` frames a real page carries.
fn hook(n: u64) -> GxEnvelope {
    envelope(json!({
        "eventId": eid(n),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": "hook_execution",
                "event_name": "post_tool_use",
                "tool_name": "run_terminal_command",
                "runs": [{ "name": "h", "status": { "status": "success", "elapsed_ms": 9 } }],
            },
            "_meta": { "agentTimestampMs": 1_788_927_595_489i64 },
        },
        "timestamp": 1_788_927_595i64,
    }))
}

fn tool_call(n: u64, call_id: &str, name: &str, command: &str) -> GxEnvelope {
    envelope(json!({
        "eventId": eid(n),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": "tool_call",
                "toolCallId": call_id,
                "title": name,
                "rawInput": { "command": command, "description": "a description" },
                "_meta": { "x.ai/tool": { "name": name, "kind": "execute", "read_only": false } },
            },
            "_meta": {
                "agentTimestampMs": 1_788_927_595_431i64,
                "promptId": "p1",
                "updateParams": { "toolCallId": call_id, "status": "Pending" },
            },
        },
        "timestamp": 1_788_927_595i64,
    }))
}

fn drain_rows(fold: &mut GxFold) -> Vec<RcFeedMessage> {
    fold.flush_open();
    fold.drain_messages()
}

// ---------------------------------------------------------------------------
// event ids
// ---------------------------------------------------------------------------

#[test]
fn an_event_id_splits_at_the_last_hyphen_and_compares_numerically() {
    let id = EventId::parse(&eid(509_197)).expect("parses");
    assert_eq!(
        id.prefix, SID,
        "the prefix is the whole UUID, hyphens and all"
    );
    assert_eq!(id.counter, 509_197);
    assert_eq!(id.raw, eid(509_197));
    assert!(id.belongs_to(SID));
    assert!(!id.belongs_to("some-other-session"));

    // The whole reason the comparison is numeric: `…-9` sorts AFTER `…-10` as
    // a string, and before it as a number.
    let nine = EventId::parse(&eid(9)).expect("parses");
    let ten = EventId::parse(&eid(10)).expect("parses");
    assert!(nine.counter < ten.counter);
    assert!(nine.raw > ten.raw, "string order really is the wrong order");

    for bad in [
        "",
        "-",
        "nohyphen",
        &format!("{SID}-"),
        "-5",
        &format!("{SID}-abc"),
    ] {
        assert!(EventId::parse(bad).is_none(), "accepted {bad:?}");
    }
}

#[test]
fn ids_ending_nine_and_ten_advance_the_cursor_in_numeric_order() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(9, "agent_message_chunk", "a", Some("p1")));
    fold.apply(&chunk(10, "agent_message_chunk", "b", Some("p1")));
    assert_eq!(fold.resume_cursor(), Some(eid(10).as_str()));

    // And the other way round on the wire: gx's counters are NOT monotonic in
    // transcript order, so a smaller id arriving later must not lower the
    // resume cursor.
    fold.apply(&chunk(9, "agent_message_chunk", "c", Some("p1")));
    let mut fold2 = GxFold::new(SID);
    fold2.apply(&chunk(10, "agent_message_chunk", "b", Some("p1")));
    fold2.apply(&chunk(9, "agent_message_chunk", "a", Some("p1")));
    assert_eq!(
        fold2.resume_cursor(),
        Some(eid(10).as_str()),
        "the resume cursor is the HIGHEST counter, not the last applied"
    );
    assert_eq!(
        fold2.last_applied().map(|e| e.counter),
        Some(9),
        "last_applied is positional and really is the last one folded"
    );
}

// ---------------------------------------------------------------------------
// chunks and streaks
// ---------------------------------------------------------------------------

#[test]
fn same_kind_chunks_coalesce_into_one_row() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "Hello, ", Some("p1")));
    fold.apply(&chunk(2, "agent_message_chunk", "world", Some("p1")));
    fold.apply(&chunk(3, "agent_message_chunk", "!", Some("p1")));

    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 1, "{rows:#?}");
    assert_eq!(rows[0].role, "assistant");
    assert_eq!(rows[0].msg_type, "text");
    assert_eq!(rows[0].text.as_deref(), Some("Hello, world!"));
    // Stamped when the streak STARTED, not when it was flushed — and from
    // `_meta.agentTimestampMs`, which is MILLISECONDS.
    assert_eq!(rows[0].ts.as_deref(), Some(&*rfc3339_z(1_788_927_621)));
}

#[test]
fn a_different_chunk_kind_ends_the_streak() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_thought_chunk", "thinking", Some("p1")));
    fold.apply(&chunk(2, "agent_message_chunk", "answering", Some("p1")));
    fold.apply(&chunk(3, "user_message_chunk", "asking", None));

    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 3);
    assert_eq!(
        (rows[0].role.as_str(), rows[0].msg_type.as_str()),
        ("assistant", "reasoning")
    );
    assert_eq!(
        (rows[1].role.as_str(), rows[1].msg_type.as_str()),
        ("assistant", "text")
    );
    assert_eq!(
        (rows[2].role.as_str(), rows[2].msg_type.as_str()),
        ("user", "text")
    );
}

#[test]
fn a_prompt_id_change_ends_the_streak_and_an_absent_one_does_not() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "turn one", Some("p1")));
    // Absent means NO CHANGE, not "a different prompt" — a user_message_chunk
    // carries no promptId at all, and splitting on that would shred every
    // user turn off from itself.
    fold.apply(&chunk(2, "agent_message_chunk", " still one", None));
    assert_eq!(drain_rows(&mut fold).len(), 1);

    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "turn one", Some("p1")));
    fold.apply(&chunk(2, "agent_message_chunk", "turn two", Some("p2")));
    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 2, "{rows:#?}");
    assert_eq!(rows[0].text.as_deref(), Some("turn one"));
    assert_eq!(rows[1].text.as_deref(), Some("turn two"));
}

#[test]
fn ignored_kinds_between_chunks_are_transparent_but_still_advance_the_cursor() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "before ", Some("p1")));
    // A real page is 15 hook_execution frames in 40, interleaved with chunk
    // streaks. Closing a streak on one would shred every assistant turn.
    fold.apply(&hook(2));
    fold.apply(&hook(3));
    fold.apply(&envelope(json!({
        "eventId": eid(4),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "session_recap", "summary": "a recap", "auto": true },
            "_meta": { "agentTimestampMs": 1i64 },
        },
    })));
    // A kind this build has never heard of is ignored the same way.
    fold.apply(&envelope(json!({
        "eventId": eid(5),
        "method": "session/update",
        "params": { "sessionId": SID, "update": { "sessionUpdate": "a_kind_from_2027" } },
    })));
    fold.apply(&chunk(6, "agent_message_chunk", "after", Some("p1")));

    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 1, "the streak survived: {rows:#?}");
    assert_eq!(rows[0].text.as_deref(), Some("before after"));
    assert_eq!(
        fold.last_applied().map(|e| e.counter),
        Some(6),
        "every ignored kind still advanced last_applied"
    );

    for kind in [
        "hook_execution",
        "session_recap",
        "plan",
        "available_commands_update",
        "current_mode_update",
    ] {
        assert!(is_ignored_kind(kind), "{kind}");
    }
    assert!(!is_ignored_kind("agent_message_chunk"));
}

/// Segmentation is **lossless**, splits only on character boundaries, and
/// never emits a row over the cap.
///
/// The cap used to be checked only after a whole chunk had been appended, so a
/// streak could end up over it and the capping layer would TRUNCATE — dropping
/// text. §3.4 calls this segmentation: the next chunk opens a new streak, and
/// nothing is lost.
#[test]
fn a_streak_segments_losslessly_at_the_eight_kib_cap() {
    let mut fold = GxFold::new(SID);
    let block = "x".repeat(1000);
    // Nine 1,000-byte chunks: 9,000 bytes over an 8,192-byte cap.
    for n in 1..=9 {
        fold.apply(&chunk(n, "agent_message_chunk", &block, Some("p1")));
    }
    let emitted = fold.drain_messages();
    assert_eq!(emitted.len(), 1, "one full segment was emitted at the cap");
    assert_eq!(
        emitted[0].text.as_deref().unwrap_or_default().len(),
        MAX_STREAK_BYTES,
        "a segment is exactly the cap — never over it",
    );
    assert!(
        fold.has_open_streak(),
        "the remainder carried into a new streak rather than being dropped",
    );
    let tail = drain_rows(&mut fold);
    assert_eq!(
        emitted[0].text.as_deref().unwrap_or_default().len()
            + tail[0].text.as_deref().unwrap_or_default().len(),
        9_000,
        "every byte survives across the segment boundary",
    );
}

/// The exact case that used to lose text: a streak one byte short of the cap,
/// then a multi-byte character.
#[test]
fn a_multi_byte_character_at_the_cap_is_never_split_and_never_dropped() {
    let mut fold = GxFold::new(SID);
    // 8,191 ASCII bytes, then `é` (2 bytes) — 8,193 in total.
    fold.apply(&chunk(
        1,
        "agent_message_chunk",
        &"x".repeat(MAX_STREAK_BYTES - 1),
        Some("p1"),
    ));
    fold.apply(&chunk(2, "agent_message_chunk", "é", Some("p1")));

    let rows = drain_rows(&mut fold);
    let joined: String = rows
        .iter()
        .map(|r| r.text.as_deref().unwrap_or_default())
        .collect();
    assert_eq!(
        joined,
        format!("{}é", "x".repeat(MAX_STREAK_BYTES - 1)),
        "not one byte lost, and the é is intact",
    );
    for r in &rows {
        let text = r.text.as_deref().unwrap_or_default();
        assert!(text.len() <= MAX_STREAK_BYTES, "{} bytes", text.len());
        // Would have panicked already if a codepoint had been split, but say so.
        assert!(std::str::from_utf8(text.as_bytes()).is_ok());
    }
}

/// One chunk several times the cap segments across as many rows as it takes.
#[test]
fn a_single_chunk_many_times_the_cap_segments_across_rows_losslessly() {
    let mut fold = GxFold::new(SID);
    // Multi-byte throughout, so every boundary is a chance to split a codepoint.
    let huge = "é".repeat(MAX_STREAK_BYTES * 2);
    fold.apply(&chunk(1, "agent_message_chunk", &huge, Some("p1")));

    let rows = drain_rows(&mut fold);
    assert!(
        rows.len() >= 4,
        "segmented across several rows: {}",
        rows.len()
    );
    for r in &rows {
        assert!(
            r.text.as_deref().unwrap_or_default().len() <= MAX_STREAK_BYTES,
            "no row exceeds the cap",
        );
    }
    let joined: String = rows
        .iter()
        .map(|r| r.text.as_deref().unwrap_or_default())
        .collect();
    assert_eq!(joined, huge, "every codepoint survived, in order");
}

#[test]
fn non_text_content_blocks_become_markers_rather_than_nothing() {
    let mut fold = GxFold::new(SID);
    fold.apply(&envelope(json!({
        "eventId": eid(1),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": "agent_message_chunk",
                "content": { "type": "image", "data": "…", "mimeType": "image/png" },
            },
        },
    })));
    let rows = drain_rows(&mut fold);
    assert_eq!(rows[0].text.as_deref(), Some("[image]"));

    let mut fold = GxFold::new(SID);
    fold.apply(&envelope(json!({
        "eventId": eid(1),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": "agent_message_chunk",
                "content": { "type": "resource_link", "uri": "file:///tmp/a.txt" },
            },
        },
    })));
    let rows = drain_rows(&mut fold);
    assert_eq!(
        rows[0].text.as_deref(),
        Some("[resource: file:///tmp/a.txt]")
    );
}

// ---------------------------------------------------------------------------
// dedup and id-less envelopes
// ---------------------------------------------------------------------------

#[test]
fn a_repeated_event_id_is_folded_once() {
    let mut fold = GxFold::new(SID);
    assert!(fold.apply(&chunk(1, "agent_message_chunk", "once", Some("p1"))));
    assert!(
        !fold.apply(&chunk(1, "agent_message_chunk", "once", Some("p1"))),
        "the second sight is a duplicate"
    );
    let rows = drain_rows(&mut fold);
    assert_eq!(rows[0].text.as_deref(), Some("once"));
}

#[test]
fn id_less_envelopes_are_never_deduped_and_never_move_the_cursor() {
    let mut fold = GxFold::new(SID);
    let mut idless = chunk(1, "agent_message_chunk", "a", Some("p1"));
    idless.event_id = None;
    // Applied twice: identical, and both must land. They carry nothing to
    // dedup on, and dropping them would lose real rows.
    assert!(fold.apply(&idless));
    assert!(fold.apply(&idless));
    assert_eq!(fold.last_applied(), None);
    assert_eq!(fold.resume_cursor(), None);
    let rows = drain_rows(&mut fold);
    assert_eq!(rows[0].text.as_deref(), Some("aa"));
}

#[test]
fn an_id_foreign_to_this_session_is_folded_as_though_it_had_none() {
    let mut fold = GxFold::new(SID);
    let mut foreign = chunk(1, "agent_message_chunk", "still shown", Some("p1"));
    foreign.event_id = Some("some-other-session-77".to_string());
    assert!(fold.apply(&foreign));
    assert_eq!(
        fold.resume_cursor(),
        None,
        "a foreign prefix must never become the resume cursor — that is exactly \
         what gx answers cursor_unresolvable to"
    );
    let rows = drain_rows(&mut fold);
    assert_eq!(rows[0].text.as_deref(), Some("still shown"));
}

#[test]
fn the_seen_set_is_bounded() {
    let mut fold = GxFold::new(SID);
    for n in 0..(MAX_SEEN as u64 + 10) {
        fold.apply(&hook(n));
    }
    // The oldest ids have been forgotten, so a replay of one folds again
    // rather than being silently dropped — bounded dedup, not perfect dedup.
    assert!(fold.apply(&hook(0)));
    // A recent one is still remembered.
    assert!(!fold.apply(&hook(MAX_SEEN as u64 + 9)));
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

#[test]
fn interleaved_tool_calls_on_two_ids_keep_their_own_state() {
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "run_terminal_command", "ls -la"));
    fold.apply(&tool_call(2, "call-b", "read_file", "cat x"));
    // Completed out of order.
    fold.apply(&terminal_update(
        3,
        "call-b",
        "completed",
        &json!([
            { "type": "content", "content": { "type": "text", "text": "b finished" } }
        ]),
    ));
    fold.apply(&terminal_update(
        4,
        "call-a",
        "completed",
        &json!([
            { "type": "content", "content": { "type": "text", "text": "a finished" } }
        ]),
    ));

    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 4, "{rows:#?}");
    assert_eq!(
        rows[0].tool.as_ref().unwrap().name.as_deref(),
        Some("run_terminal_command")
    );
    assert_eq!(
        rows[1].tool.as_ref().unwrap().name.as_deref(),
        Some("read_file")
    );
    // The result rows carry the name recorded at `tool_call` time, not the
    // `tool_call_update`'s title (which is the whole rendered command).
    assert_eq!(rows[2].msg_type, "tool_result");
    assert_eq!(
        rows[2].tool.as_ref().unwrap().name.as_deref(),
        Some("read_file")
    );
    assert_eq!(
        rows[2].tool.as_ref().unwrap().detail.as_deref(),
        Some("b finished")
    );
    assert_eq!(
        rows[3].tool.as_ref().unwrap().name.as_deref(),
        Some("run_terminal_command")
    );
    assert_eq!(
        rows[3].tool.as_ref().unwrap().detail.as_deref(),
        Some("a finished")
    );
}

fn terminal_update(n: u64, call_id: &str, status: &str, content: &serde_json::Value) -> GxEnvelope {
    envelope(json!({
        "eventId": eid(n),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": "tool_call_update",
                "toolCallId": call_id,
                "status": status,
                "content": content,
            },
            "_meta": { "agentTimestampMs": 1_788_927_595_444i64, "promptId": "p1" },
        },
    }))
}

#[test]
fn a_repeated_terminal_update_is_absorbed() {
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "run_terminal_command", "ls"));
    let done = terminal_update(2, "call-a", "completed", &json!([]));
    fold.apply(&done);
    // gx re-sends terminal updates on re-attach and on replay; a row per
    // repeat would duplicate the transcript. A DIFFERENT id, so the seen-set
    // is not what is absorbing it.
    fold.apply(&terminal_update(3, "call-a", "completed", &json!([])));
    fold.apply(&terminal_update(4, "call-a", "failed", &json!([])));

    let rows = drain_rows(&mut fold);
    assert_eq!(
        rows.iter().filter(|r| r.msg_type == "tool_result").count(),
        1,
        "{rows:#?}"
    );
}

#[test]
fn both_status_spellings_are_read_and_compared_case_insensitively() {
    // Lowercase on `update.status` (ACP), PascalCase under
    // `_meta.updateParams.status` — and gx really does use both.
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "t", "x"));
    fold.apply(&terminal_update(2, "call-a", "Completed", &json!([])));
    assert_eq!(
        drain_rows(&mut fold)
            .iter()
            .filter(|r| r.msg_type == "tool_result")
            .count(),
        1,
        "PascalCase on the top-level status"
    );

    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "t", "x"));
    fold.apply(&envelope(json!({
        "eventId": eid(2),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "tool_call_update", "toolCallId": "call-a" },
            "_meta": { "updateParams": { "toolCallId": "call-a", "status": "Completed" } },
        },
    })));
    assert_eq!(
        drain_rows(&mut fold)
            .iter()
            .filter(|r| r.msg_type == "tool_result")
            .count(),
        1,
        "the _meta.updateParams spelling"
    );
}

#[test]
fn a_non_terminal_update_refreshes_state_without_emitting() {
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "t", "old command"));
    // The real wire: `_meta.updateParams.status` is null while a call runs.
    fold.apply(&envelope(json!({
        "eventId": eid(2),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": {
                "sessionUpdate": "tool_call_update",
                "toolCallId": "call-a",
                "rawInput": { "command": "new command" },
            },
            "_meta": { "updateParams": { "toolCallId": "call-a", "status": null } },
        },
    })));
    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 1, "no result row yet: {rows:#?}");

    fold.apply(&terminal_update(3, "call-a", "completed", &json!([])));
    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 1);
    assert_eq!(
        rows[0].tool.as_ref().unwrap().detail.as_deref(),
        Some("new command"),
        "the non-terminal update's rawInput was inherited"
    );
}

#[test]
fn a_failed_tool_says_so_and_a_diff_becomes_its_own_marker() {
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "t", "x"));
    fold.apply(&terminal_update(2, "call-a", "failed", &json!([])));
    let rows = drain_rows(&mut fold);
    let result = rows.iter().find(|r| r.msg_type == "tool_result").unwrap();
    assert_eq!(result.text.as_deref(), Some("failed"));

    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-b", "edit", "x"));
    fold.apply(&terminal_update(
        2,
        "call-b",
        "completed",
        &json!([{ "type": "diff", "path": "/src/main.rs" }]),
    ));
    let rows = drain_rows(&mut fold);
    let result = rows.iter().find(|r| r.msg_type == "tool_result").unwrap();
    assert_eq!(
        result.tool.as_ref().unwrap().detail.as_deref(),
        Some("[diff: /src/main.rs]")
    );

    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-c", "sh", "x"));
    fold.apply(&terminal_update(
        2,
        "call-c",
        "completed",
        &json!([{ "type": "terminal", "terminalId": "t-1" }]),
    ));
    let rows = drain_rows(&mut fold);
    let result = rows.iter().find(|r| r.msg_type == "tool_result").unwrap();
    assert_eq!(
        result.tool.as_ref().unwrap().detail.as_deref(),
        Some("[terminal]")
    );
}

#[test]
fn a_tool_call_ends_an_open_streak() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "about to run", Some("p1")));
    fold.apply(&tool_call(2, "call-a", "t", "ls"));
    let rows = drain_rows(&mut fold);
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[0].msg_type, "text", "the streak was flushed FIRST");
    assert_eq!(rows[1].msg_type, "tool_use");
}

// ---------------------------------------------------------------------------
// turns and timestamps
// ---------------------------------------------------------------------------

#[test]
fn turn_completed_closes_the_streak_and_only_an_unusual_stop_reason_gets_a_row() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "done", Some("p1")));
    fold.apply(&turn(2, "end_turn"));
    let rows = fold.drain_messages();
    assert_eq!(
        rows.len(),
        1,
        "end_turn says nothing a reader needs: {rows:#?}"
    );
    assert_eq!(rows[0].msg_type, "text");
    assert!(!fold.has_open_streak());
    assert_eq!(fold.activity(), RcActivity::Idle);

    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "cut off", Some("p1")));
    fold.apply(&turn(2, "max_tokens"));
    let rows = fold.drain_messages();
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[1].role, "system");
    assert_eq!(rows[1].msg_type, "status");
    assert_eq!(rows[1].text.as_deref(), Some("turn completed (max_tokens)"));
}

fn turn(n: u64, stop_reason: &str) -> GxEnvelope {
    envelope(json!({
        "eventId": eid(n),
        "method": "_x.ai/session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "turn_completed", "prompt_id": "p1", "stop_reason": stop_reason },
            "_meta": { "agentTimestampMs": 1_788_927_663_192i64 },
        },
        "timestamp": 1_788_927_663i64,
    }))
}

#[test]
fn both_timestamp_units_are_read_correctly() {
    // `_meta.agentTimestampMs` wins when present, and it is MILLISECONDS.
    let ms = envelope(json!({
        "eventId": eid(1),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "agent_message_chunk", "content": { "type": "text", "text": "x" } },
            "_meta": { "agentTimestampMs": 1_788_927_621_024i64 },
        },
        "timestamp": 999i64,
    }));
    assert_eq!(ms.ts().as_deref(), Some(&*rfc3339_z(1_788_927_621)));

    // The envelope's own `timestamp` is SECONDS on a persisted line.
    let secs = envelope(json!({
        "eventId": eid(2),
        "method": "session/update",
        "params": {
            "sessionId": SID,
            "update": { "sessionUpdate": "agent_message_chunk", "content": { "type": "text", "text": "x" } },
        },
        "timestamp": 1_788_927_622i64,
    }));
    assert_eq!(secs.ts().as_deref(), Some(&*rfc3339_z(1_788_927_622)));

    // A producer that ever stamps `timestamp` in millis is read as millis:
    // 10^11 seconds is the year 5138 and 10^11 millis is 1973, so the split
    // is unambiguous for anything either wire will carry.
    let millis_in_timestamp = envelope(json!({
        "eventId": eid(3),
        "method": "session/update",
        "params": { "sessionId": SID, "update": { "sessionUpdate": "agent_message_chunk" } },
        "timestamp": 1_788_927_622_000i64,
    }));
    assert_eq!(
        millis_in_timestamp.ts().as_deref(),
        Some(&*rfc3339_z(1_788_927_622))
    );

    // Null on both: no timestamp at all, which the ring then defaults.
    let none = envelope(json!({
        "eventId": eid(4),
        "method": "session/update",
        "params": { "sessionId": SID, "update": { "sessionUpdate": "agent_message_chunk" } },
        "timestamp": null,
    }));
    assert_eq!(none.ts(), None);
}

// ---------------------------------------------------------------------------
// approvals in the transcript
// ---------------------------------------------------------------------------

#[test]
fn the_first_sight_of_a_pending_approval_leaves_one_trace_row() {
    let mut fold = GxFold::new(SID);
    let approval = lane_approval(&permission_resource("tc-1"));

    assert!(
        fold.note_approval(approval.clone()),
        "the first sight emits"
    );
    assert!(
        !fold.note_approval(approval.clone()),
        "a later change rides LaneEvent::Approval only — rows are append-only"
    );

    let rows = fold.drain_messages();
    assert_eq!(rows.len(), 1, "{rows:#?}");
    assert_eq!(rows[0].role, "system");
    assert_eq!(rows[0].msg_type, "approval_request");
    let block = rows[0].approval.as_ref().expect("the approval block");
    assert_eq!(block.id, "tc-1");
    assert_eq!(block.status, "pending");
    assert_eq!(fold.open_approvals(), 1);
    assert_eq!(
        fold.activity(),
        RcActivity::NeedsApproval,
        "an open approval overrides whatever the transcript last said"
    );

    // Answering it clears the override without emitting a second row.
    let mut resolved = approval;
    resolved.status = LaneApprovalStatus::Resolved;
    fold.note_approval(resolved);
    assert!(fold.drain_messages().is_empty());
    assert_eq!(fold.open_approvals(), 0);
}

#[test]
fn a_submitted_approval_never_opens_a_trace_row() {
    let mut fold = GxFold::new(SID);
    let mut a = lane_approval(&permission_resource("tc-1"));
    a.status = LaneApprovalStatus::Submitted;
    assert!(!fold.note_approval(a));
    assert!(fold.drain_messages().is_empty());
    assert_eq!(fold.open_approvals(), 0);
}

// ---------------------------------------------------------------------------
// the reseed / silent-resume boundary
// ---------------------------------------------------------------------------

#[test]
fn reset_discards_everything_and_not_calling_it_keeps_everything() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", "half a turn", Some("p1")));
    fold.note_approval(lane_approval(&permission_resource("tc-1")));
    let _ = fold.drain_messages();

    // A SILENT resume does not reset: the open streak, the cursors, the
    // approvals and the seen-set all survive. That IS correction 1.
    assert!(fold.has_open_streak());
    assert_eq!(fold.open_approvals(), 1);
    assert_eq!(fold.resume_cursor(), Some(eid(1).as_str()));
    assert!(
        !fold.apply(&chunk(1, "agent_message_chunk", "half a turn", Some("p1"))),
        "the seen-set survived, so the overlap is still deduped"
    );

    fold.reset();
    assert!(!fold.has_open_streak(), "a reseed DISCARDS the open streak");
    assert_eq!(fold.open_approvals(), 0);
    assert_eq!(fold.resume_cursor(), None);
    assert_eq!(fold.last_applied(), None);
    assert_eq!(fold.activity(), RcActivity::Unknown);
    assert!(
        fold.apply(&chunk(1, "agent_message_chunk", "again", Some("p1"))),
        "the seen-set was cleared too"
    );
}

// ---------------------------------------------------------------------------
// positional cutting
// ---------------------------------------------------------------------------

#[test]
fn cut_at_cursor_is_positional_over_non_monotonic_counters() {
    // The real out-of-order shape from a live leader:
    // …509197, 509195, 509196, 509200, 509203, 509202…
    let page: Vec<GxEnvelope> = [509_197u64, 509_195, 509_196, 509_200, 509_203, 509_202]
        .iter()
        .map(|n| chunk(*n, "agent_message_chunk", "x", Some("p1")))
        .collect();

    let cursor = EventId::parse(&eid(509_196)).expect("parses");
    let tail = cut_at_cursor(&page, &cursor).expect("located");
    assert_eq!(
        tail.iter()
            .map(|e| EventId::parse(e.event_id.as_deref().unwrap())
                .unwrap()
                .counter)
            .collect::<Vec<_>>(),
        vec![509_200, 509_203, 509_202],
        "everything AFTER the cursor's position — a counter filter would have \
         dropped 509202 and kept nothing sensible"
    );

    // Id-less envelopes after the cut are kept.
    let mut with_gap = page.clone();
    let mut idless = chunk(0, "agent_message_chunk", "no id", Some("p1"));
    idless.event_id = None;
    with_gap.insert(4, idless);
    let tail = cut_at_cursor(&with_gap, &cursor).expect("located");
    assert_eq!(tail.len(), 4, "the id-less envelope rode along");

    // A cursor this page does not reach at all.
    assert!(cut_at_cursor(&page, &EventId::parse(&eid(1)).unwrap()).is_none());
    // Newer than anything here: everything is at-or-before it, so the tail is
    // empty rather than the whole page.
    let ahead = cut_at_cursor(&page, &EventId::parse(&eid(999_999)).unwrap()).expect("located");
    assert!(ahead.is_empty());

    assert_eq!(min_counter(&page), Some(509_195));
    assert_eq!(min_counter(&[]), None);
}

// ---------------------------------------------------------------------------
// approval resources → LaneApproval
// ---------------------------------------------------------------------------

fn resource(kind: &str, method: &str, request: &serde_json::Value) -> GxApprovalResource {
    serde_json::from_value(json!({
        "id": "tc-1",
        "sessionId": SID,
        "kind": kind,
        "method": method,
        "status": "pending",
        "request": request,
        "createdAt": 1_788_931_000_000i64,
    }))
    .expect("the resource decodes")
}

fn permission_resource(id: &str) -> GxApprovalResource {
    serde_json::from_value(json!({
        "id": id,
        "sessionId": SID,
        "kind": "permission",
        "method": "session/request_permission",
        "status": "pending",
        "request": {
            "toolCall": { "toolCallId": id, "title": "rm -rf build/", "rawInput": { "command": "rm -rf build/" } },
            "options": [
                { "optionId": "p-1", "name": "Allow once", "kind": "allow_once" },
                { "optionId": "p-2", "name": "Always allow", "kind": "allow_always" },
                { "optionId": "p-3", "name": "Reject", "kind": "reject_once" },
                { "optionId": "p-4", "name": "Never allow", "kind": "reject_always" },
            ],
        },
        "createdAt": 1_788_931_000_000i64,
    }))
    .expect("the resource decodes")
}

#[test]
fn a_permission_keeps_its_option_ids_opaque_and_its_kinds_semantic() {
    let a = lane_approval(&permission_resource("tc-1"));
    assert_eq!(a.kind, LaneApprovalKind::Permission);
    assert_eq!(a.status, LaneApprovalStatus::Pending);
    assert_eq!(a.title, "rm -rf build/");
    assert_eq!(a.detail.as_deref(), Some("rm -rf build/"));
    assert!(
        a.questions.is_empty(),
        "kind selects which list is populated"
    );

    // Ids numbered `p-1…p-4` on purpose: they are unrelated to their kinds,
    // which is what makes id-sniffing fail loudly instead of by luck.
    assert_eq!(
        a.options.iter().map(|o| o.id.as_str()).collect::<Vec<_>>(),
        vec!["p-1", "p-2", "p-3", "p-4"]
    );
    assert_eq!(
        a.options
            .iter()
            .map(|o| o.kind.as_deref().unwrap_or("?"))
            .collect::<Vec<_>>(),
        vec!["allow_once", "allow_always", "reject_once", "reject_always"]
    );
    assert_eq!(a.options[0].label, "Allow once");

    // The contract's own by-kind resolver picks by KIND, and it is the single
    // implementation gx, opencode and the Dart mirror all use.
    assert_eq!(a.option_for(LaneDecision::AllowOnce).unwrap().id, "p-1");
    assert_eq!(a.option_for(LaneDecision::AllowAlways).unwrap().id, "p-2");
    assert_eq!(a.option_for(LaneDecision::Reject).unwrap().id, "p-3");

    assert_eq!(a.created_at_unix_ms, Some(1_788_931_000_000));
    assert!(a.request_json.contains(r#""optionId":"p-4""#));
}

#[test]
fn request_json_is_compacted_and_keeps_the_producers_key_order() {
    // Parsed from a STRING, not through `json!`: a `serde_json::Value` sorts
    // an object's keys, so only a raw parse can prove the claim. This is the
    // path a real HTTP body takes, and it is why `request` is a `RawValue` —
    // a re-encoded `Value` would hand a client a request that no longer
    // matches the one gx is holding.
    let res: GxApprovalResource = serde_json::from_str(
        r#"{ "id": "tc-1", "sessionId": "s", "kind": "permission",
             "method": "session/request_permission", "status": "pending",
             "request": { "zeta": 1, "alpha": { "nested": "x y" }, "middle": [1, 2] } }"#,
    )
    .expect("decodes");
    let a = lane_approval(&res);
    assert_eq!(
        a.request_json, r#"{"zeta":1,"alpha":{"nested":"x y"},"middle":[1,2]}"#,
        "whitespace stripped OUTSIDE string literals, key order untouched"
    );
}

#[test]
fn a_question_is_keyed_by_its_text_and_its_options_carry_no_posture() {
    let a = lane_approval(&resource(
        "question",
        "x.ai/ask_user_question",
        &json!({
            "toolCallId": "tc-1",
            "questions": [
                {
                    "id": "q1",
                    "question": "Which database?",
                    "options": [
                        { "label": "Postgres", "description": "the boring one" },
                        { "label": "Redis" },
                    ],
                },
                { "id": "q2", "question": "Which cache?", "multiSelect": true, "options": [{ "label": "None" }] },
            ],
        }),
    ));
    assert_eq!(a.kind, LaneApprovalKind::Question);
    assert!(a.options.is_empty(), "kind selects which list is populated");
    assert_eq!(a.questions.len(), 2, "EVERY question, not just the first");
    assert_eq!(a.title, "Which database?");

    // The key is the question's TEXT, not its `id` field: gx's own TUI files
    // answers in a map keyed by `q.question`, so an answer under `q1` is one
    // the agent never reads.
    assert_eq!(a.questions[0].id.as_deref(), Some("Which database?"));
    assert_eq!(a.questions[1].id.as_deref(), Some("Which cache?"));
    assert_eq!(a.questions[0].header, "");
    assert!(!a.questions[0].multiple);
    assert!(a.questions[1].multiple);
    // Free text on gx is an "Other" answer plus an annotations channel the
    // contract cannot carry yet, so typing is never invited.
    assert!(!a.questions[0].custom && !a.questions[1].custom);

    // A question's answer labels ARE its option ids, and they carry no
    // permission posture — "Postgres" is not `allow_once`.
    assert_eq!(a.questions[0].options[0].id, "Postgres");
    assert_eq!(a.questions[0].options[0].label, "Postgres");
    assert_eq!(
        a.questions[0].options[0].description.as_deref(),
        Some("the boring one")
    );
    assert!(a.questions[0].options.iter().all(|o| o.kind.is_none()));
}

#[test]
fn a_plan_approval_gets_two_synthesized_options_whose_ids_are_the_outcomes() {
    let a = lane_approval(&resource(
        "plan_approval",
        "x.ai/exit_plan_mode",
        &json!({ "toolCallId": "tc-1", "planContent": "# Plan\n1. Do it" }),
    ));
    assert_eq!(a.kind, LaneApprovalKind::PlanApproval);
    assert_eq!(a.title, "Approve plan");
    assert!(a.detail.as_deref().unwrap().contains("Do it"));
    assert_eq!(
        a.options
            .iter()
            .map(|o| (o.id.as_str(), o.label.as_str(), o.kind.as_deref().unwrap()))
            .collect::<Vec<_>>(),
        vec![
            ("approved", "Approve", "allow_once"),
            ("cancelled", "Cancel", "reject_once"),
        ]
    );
}

#[test]
fn an_elicitation_an_unknown_kind_and_a_placeholder_offer_nothing_to_press() {
    let elicit = lane_approval(&resource(
        "mcp_elicitation",
        "x.ai/mcp/elicit",
        &json!({ "toolCallId": "tc-1", "serverName": "files", "message": "Which mailbox?" }),
    ));
    assert_eq!(elicit.kind, LaneApprovalKind::McpElicitation);
    assert_eq!(elicit.title, "Which mailbox?");
    assert!(elicit.options.is_empty() && elicit.questions.is_empty());
    assert!(elicit.request_json.contains("\"serverName\":\"files\""));

    let future = lane_approval(&resource("a_kind_from_2027", "x.ai/whatever", &json!({})));
    assert_eq!(
        future.kind,
        LaneApprovalKind::Other("a_kind_from_2027".to_string()),
        "an unrecognized kind is PRESERVED, not coerced"
    );
    assert!(future.options.is_empty());

    // The placeholder a `pending_interaction` creates before the request
    // itself arrives: no method, no request.
    let placeholder: GxApprovalResource = serde_json::from_value(json!({
        "id": "tc-1",
        "sessionId": SID,
        "kind": "permission",
        "method": null,
        "status": "pending",
        "request": null,
        "createdAt": 1i64,
    }))
    .expect("decodes");
    let a = lane_approval(&placeholder);
    assert_eq!(a.kind, LaneApprovalKind::Other("placeholder".to_string()));
    assert!(a.options.is_empty());
    assert_eq!(a.request_json, "null");
}

#[test]
fn an_unrecognized_status_is_preserved_and_is_not_pending() {
    let mut res = permission_resource("tc-1");
    res.status = "cancelled".to_string();
    let a = lane_approval(&res);
    assert_eq!(a.status, LaneApprovalStatus::Other("cancelled".to_string()));
    assert!(
        !a.status.is_pending(),
        "an unknown status is at least as likely to be terminal as live"
    );
}

/// A detail cut down to the cap must SAY it was cut down.
///
/// The fold deliberately emits tool details RAW and lets the capping layer do
/// the bounding — `shed_core::lane::feed::sanitize_feed_text` for an approval's
/// detail, and the ring's `append` for a transcript row. Both cap at
/// `MAX_FEED_MESSAGE_BYTES` **and append `FEED_TRUNC_MARKER`**.
///
/// This is a regression test for a real bug: the fold used to pre-truncate at
/// exactly that cap first, so the capping layer then saw a string that no longer
/// EXCEEDED the cap and appended no marker — a truncated detail was
/// indistinguishable from a complete one. Catching it before C3's golden froze
/// the unmarked shape is the point.
#[test]
fn a_truncated_detail_is_always_marked_as_truncated() {
    let huge = "x".repeat(MAX_FEED_MESSAGE_BYTES * 2);

    // A tool row: the fold emits it raw, the ring caps and marks it.
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "run_terminal_command", &huge));
    let rows = drain_rows(&mut fold);
    let raw_detail = rows[0]
        .tool
        .as_ref()
        .and_then(|t| t.detail.as_deref())
        .expect("a detail");
    assert!(
        raw_detail.len() > MAX_FEED_MESSAGE_BYTES,
        "the fold emits raw and leaves bounding to the capping layer, exactly          as shed-opencode's fold does"
    );
    let mut ring = MessageRing::new();
    let stored = ring.append(rows[0].clone(), 0);
    let detail = stored
        .tool
        .as_ref()
        .and_then(|t| t.detail.as_deref())
        .expect("a detail");
    assert!(
        detail.ends_with(FEED_TRUNC_MARKER),
        "a capped tool detail must be marked; got a {}-byte detail ending {:?}",
        detail.len(),
        &detail[detail.len().saturating_sub(20)..]
    );

    // An approval's detail does not go through a ring at all — `lane_approval`
    // sanitizes it directly — so it is capped and marked there.
    let a = lane_approval(&resource(
        "plan_approval",
        "x.ai/exit_plan_mode",
        &json!({ "toolCallId": "tc-1", "planContent": huge }),
    ));
    let detail = a.detail.as_deref().expect("a plan detail");
    assert!(
        detail.ends_with(FEED_TRUNC_MARKER),
        "{} bytes",
        detail.len()
    );

    // And a detail that FITS is left exactly alone — no marker on a complete
    // value, which is the other half of the signal being worth anything.
    let small = lane_approval(&resource(
        "plan_approval",
        "x.ai/exit_plan_mode",
        &json!({ "toolCallId": "tc-1", "planContent": "# Plan\n1. Do it" }),
    ));
    let detail = small.detail.as_deref().expect("a plan detail");
    assert!(!detail.contains(FEED_TRUNC_MARKER), "{detail}");
    assert_eq!(detail, "# Plan\n1. Do it");
}

/// The absent-cursor fallback must respect the session PREFIX.
///
/// gx's counter is process-global, so another session's id compares perfectly
/// well against this session's cursor. A counter-only fallback let a foreign
/// envelope anchor the cut — and silently dropped everything before it,
/// including id-less envelopes, instead of reporting the cursor absent so the
/// caller could page further back.
#[test]
fn the_cursor_fallback_ignores_a_foreign_id() {
    let mut idless = chunk(0, "agent_message_chunk", "no id", Some("p1"));
    idless.event_id = None;
    let mut foreign = chunk(5, "agent_message_chunk", "another session", Some("p1"));
    foreign.event_id = Some("some-other-session-5".to_string());
    let mine = chunk(6, "agent_message_chunk", "mine", Some("p1"));
    let page = vec![idless, foreign, mine];

    let cursor = EventId::parse(&eid(5)).expect("parses");
    assert!(
        cut_at_cursor(&page, &cursor).is_none(),
        "cursor S-5 is not IN this page — the only counter that matched belongs \
         to another session, and treating it as a position would drop the \
         id-less envelope in front of it",
    );

    // With a real S-5 present it locates normally, id-less envelope and all.
    let mut with_mine = page.clone();
    with_mine.insert(2, chunk(5, "agent_message_chunk", "mine 5", Some("p1")));
    let tail = cut_at_cursor(&with_mine, &cursor).expect("located");
    assert_eq!(tail.len(), 1);
    assert_eq!(
        tail[0].event_id.as_deref(),
        Some(eid(6).as_str()),
        "cut after MY S-5, not after the foreign one",
    );
}

/// A replayed `tool_call` must not un-complete a tool that already finished.
///
/// gx re-sends a call on re-attach and on resume, so
/// `call → completed → call → completed` is ordinary traffic. Resetting the
/// flag made it emit TWO `tool_result` rows for one id, against §3.4's "one per
/// toolCallId, repeats absorbed".
#[test]
fn a_replayed_tool_call_does_not_produce_a_second_result_row() {
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "call-a", "run_terminal_command", "ls"));
    fold.apply(&terminal_update(2, "call-a", "completed", &json!([])));
    // The replay: a NEW event id, so the seen-set is not what absorbs it.
    fold.apply(&tool_call(3, "call-a", "run_terminal_command", "ls"));
    fold.apply(&terminal_update(4, "call-a", "completed", &json!([])));

    let rows = drain_rows(&mut fold);
    assert_eq!(
        rows.iter().filter(|r| r.msg_type == "tool_result").count(),
        1,
        "one result row for one tool call, however many times it is replayed: \
         {rows:#?}",
    );
}

/// An agent-supplied timestamp can be any `i64`, and two of them used to be
/// fatal or absurd.
#[test]
fn extreme_timestamps_neither_panic_nor_format_an_absurd_year() {
    fn ts_of(t: i64) -> String {
        envelope(json!({
            "eventId": eid(1),
            "method": "session/update",
            "params": { "sessionId": SID, "update": { "sessionUpdate": "agent_message_chunk" } },
            "timestamp": t,
        }))
        .ts()
        .expect("a timestamp")
    }

    // `i64::MIN.abs()` PANICS in a checked build — and this value comes off the
    // wire, so one envelope could have killed the fold.
    let low = ts_of(i64::MIN);
    let high = ts_of(i64::MAX);
    assert_eq!(low, "0001-01-01T00:00:00Z", "clamped, not an absurd year");
    assert_eq!(high, "9999-12-31T23:59:59Z");

    // The seconds/milliseconds boundary either side of 10^11 is unchanged.
    assert_eq!(ts_of(MS_THRESHOLD - 1), rfc3339_z(MS_THRESHOLD - 1));
    assert_eq!(
        ts_of(MS_THRESHOLD),
        rfc3339_z(MS_THRESHOLD / 1_000),
        "at the threshold it is read as milliseconds",
    );
    assert_eq!(ts_of(0), rfc3339_z(0), "zero is the epoch, not absent");
    // Negative-but-ordinary (pre-epoch) still works rather than clamping.
    assert_eq!(ts_of(-1), rfc3339_z(-1));
}

/// The fold's maps are bounded, and the bound never drops something live.
#[test]
fn the_tool_and_approval_maps_are_bounded_without_losing_live_entries() {
    // Tools: one in flight, then far more than the cap, all completed.
    let mut fold = GxFold::new(SID);
    fold.apply(&tool_call(1, "still-running", "t", "x"));
    for n in 0..(MAX_TOOLS as u64 + 50) {
        let id = format!("done-{n}");
        fold.apply(&tool_call(1000 + n * 2, &id, "t", "x"));
        fold.apply(&terminal_update(1001 + n * 2, &id, "completed", &json!([])));
    }
    let _ = fold.drain_messages();
    assert!(
        fold.tools_len() <= MAX_TOOLS,
        "bounded: {} entries",
        fold.tools_len()
    );
    // The call still in flight survived eviction — its name is what its result
    // row will be built from.
    fold.apply(&terminal_update(
        99_999,
        "still-running",
        "completed",
        &json!([]),
    ));
    let rows = drain_rows(&mut fold);
    let result = rows
        .iter()
        .find(|r| r.msg_type == "tool_result")
        .expect("the long-running tool still produced a result row");
    assert_eq!(
        result.tool.as_ref().unwrap().name.as_deref(),
        Some("t"),
        "and it still knows the tool's NAME, not the update's title",
    );

    // Approvals: pending ones are never evicted, however many resolve after.
    let mut fold = GxFold::new(SID);
    let mut pending = lane_approval(&permission_resource("held-open"));
    pending.status = LaneApprovalStatus::Pending;
    fold.note_approval(pending);
    for n in 0..(MAX_APPROVALS + 50) {
        let mut a = lane_approval(&permission_resource(&format!("done-{n}")));
        a.status = LaneApprovalStatus::Resolved;
        fold.note_approval(a);
    }
    assert!(
        fold.approvals_len() <= MAX_APPROVALS,
        "bounded: {} entries",
        fold.approvals_len()
    );
    assert_eq!(
        fold.pending_approvals().len(),
        1,
        "the pending one is still held — an approval the human owes an answer \
         to must survive any cap",
    );
    assert_eq!(fold.pending_approvals()[0].id, "held-open");
    assert_eq!(fold.activity(), RcActivity::NeedsApproval);
}

/// A whitespace-only streak emits no row. Deliberate — see `flush_open`.
#[test]
fn a_whitespace_only_streak_emits_no_row() {
    let mut fold = GxFold::new(SID);
    fold.apply(&chunk(1, "agent_message_chunk", " \n\t ", Some("p1")));
    fold.apply(&turn(2, "end_turn"));
    assert!(
        fold.drain_messages().is_empty(),
        "an all-whitespace assistant message is noise, and an empty transcript \
         row reads as a bug to whoever is looking at it",
    );
}

#[test]
fn a_malformed_or_null_envelope_never_kills_the_fold() {
    let mut fold = GxFold::new(SID);
    assert!(!fold.apply_line(b"not json"));
    assert!(!fold.apply_line(b""));
    // Go-null discipline: `method` and `timestamp` null must not fail the line.
    assert!(fold.apply_line(br#"{"eventId":null,"method":null,"params":null,"timestamp":null}"#));
    assert!(drain_rows(&mut fold).is_empty());
}
