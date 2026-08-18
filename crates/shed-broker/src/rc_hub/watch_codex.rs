//! The codex rollout fold + correlation — a port of
//! `internal/ext/rc/watch_codex.go`.
//!
//! codex rollout files live at
//! `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`, one JSON object
//! per line: `{timestamp, type, payload}`. The types the fold reads:
//!
//! - `session_meta` (first line): session_id + cwd + timestamp — correlation
//!   only.
//! - `event_msg/task_started`, `task_complete`: bracket a turn (working ↔
//!   needs_input); task_complete also carries `last_agent_message`.
//! - `event_msg/agent_message`: the assistant's text (message preview).
//! - `event_msg/user_message`: a new turn's input (working).
//! - `response_item/(function_call|custom_tool_call)`: a tool invocation
//!   (working; its call_id is tracked as pending until the matching `*_output`
//!   line).
//! - `response_item/(function_call_output|custom_tool_call_output)`: resolves
//!   a call_id.
//! - `response_item/message role=assistant`: assistant text (preview fallback).
//! - `token_count` / `reasoning` / `world_state` / `turn_context`: ignored.
//!
//! Everything is parsed tolerantly: an unknown type, an unparseable line, or a
//! missing field is ignored, and the fold falls back to whatever verdict it
//! last held (the hub then falls back to pane stability when the watcher goes
//! non-fresh).

use std::collections::{HashMap, HashSet};
use std::io::BufRead;

use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::Value;
use shed_core::rc::RcActivity;
use shed_rc_engine::ops::GetEnv;

use super::messages::{
    null_default, sanitize_last_message, trim_feed_text, FeedMessage, FeedTool,
    FEED_ROLE_ASSISTANT, FEED_ROLE_TOOL, FEED_ROLE_USER, FEED_TYPE_REASONING, FEED_TYPE_TEXT,
    FEED_TYPE_TOOL_RESULT, FEED_TYPE_TOOL_USE,
};
use super::watch::{
    first_non_empty, json_first_byte, list_jsonl_under, parse_jsonl_time, pick_correlation,
    value_opt, within_window, ActivityFold, Correlation, JsonlPeek, MessageProducer, PeekCandidate,
    CORRELATE_WINDOW,
};
use super::watch_claude::base_of;

/// The generic envelope every rollout line shares (`codexLine`,
/// `watch_codex.go:32`). (Accepted delta, as on `ClaudeLine`: duplicate JSON
/// keys error here where Go last-wins — no real producer emits them.)
#[derive(Debug, Default, Deserialize)]
struct CodexLine {
    #[serde(default, deserialize_with = "null_default")]
    timestamp: String,
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    /// `Option` so the peek can distinguish an ABSENT payload (Go's empty
    /// RawMessage — unmarshal errors, peek fails) from an explicit `null`
    /// (Go's unmarshal no-ops and the peek succeeds with an empty meta).
    /// `value_opt` captures the null as `Some(Value::Null)` — a stock
    /// `Option<Value>` would map it to `None` and re-conflate the two.
    #[serde(default, deserialize_with = "value_opt")]
    payload: Option<Value>,
}

/// The union of the payload fields the fold reads, all optional
/// (`codexPayload`, `watch_codex.go:39`). Decoded FIELD-BY-FIELD from the
/// payload object: Go's `json.Unmarshal` populates every decodable field even
/// when one has the wrong type (the error is discarded at the call site,
/// `watch_codex.go:109`), which an all-or-nothing serde derive would not
/// mirror.
///
/// The raw fields are BORROWED from the decoded line (Go's `json.RawMessage`
/// fields are views into the line's bytes, not copies) so a large tool output
/// is not deep-cloned on every rollout line.
#[derive(Debug)]
struct CodexPayload<'a> {
    typ: String,
    /// event_msg/agent_message|user_message
    message: String,
    /// task_complete
    last_agent_message: String,
    /// (custom_)function_call(_output)
    call_id: String,
    /// (custom_)function_call tool name
    name: String,
    /// custom_tool_call invocation body
    input: String,
    /// function_call invocation body
    arguments: String,
    /// (custom_)function_call_output result
    output: &'a Value,
    /// reasoning summary blocks
    summary: &'a Value,
    /// response_item/message
    role: String,
    /// response_item/message
    content: &'a Value,
}

/// What an absent raw field borrows — Go's nil `json.RawMessage`.
static NULL_VALUE: Value = Value::Null;

impl<'a> CodexPayload<'a> {
    fn from_value(payload: &'a Value) -> CodexPayload<'a> {
        let str_field = |k: &str| -> String {
            payload
                .get(k)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string()
        };
        let raw_field = |k: &str| -> &'a Value { payload.get(k).unwrap_or(&NULL_VALUE) };
        CodexPayload {
            typ: str_field("type"),
            message: str_field("message"),
            last_agent_message: str_field("last_agent_message"),
            call_id: str_field("call_id"),
            name: str_field("name"),
            input: str_field("input"),
            arguments: str_field("arguments"),
            output: raw_field("output"),
            summary: raw_field("summary"),
            role: str_field("role"),
            content: raw_field("content"),
        }
    }
}

/// The turn-boundary tag (`lastBoundary`, `watch_codex.go:62`; Go's `""` is
/// [`CodexBoundary::None`]).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
enum CodexBoundary {
    #[default]
    None,
    Start,
    Complete,
}

/// Folds the codex rollout stream into an activity verdict AND a normalized
/// message feed (`codexFold`, `watch_codex.go:58`) — the codex half of the
/// message feed; reconcile drains it into the per-session ring.
#[derive(Debug, Default)]
pub struct CodexFold {
    /// Seen ≥1 activity-relevant event.
    confirmed: bool,
    /// Open tool-call ids.
    pending: HashSet<String>,
    /// call_id → tool name (so an output resolves its name).
    tool_names: HashMap<String, String>,
    last_boundary: CodexBoundary,
    /// Raw last message text (sanitized on read).
    last_msg: String,
    /// Produced-but-undrained feed messages.
    msgs: Vec<FeedMessage>,
    /// Raw text of the most recently emitted assistant/text row (de-dup).
    last_asst_text: String,
}

impl CodexFold {
    pub fn new() -> CodexFold {
        CodexFold::default()
    }

    /// Queues one normalized feed message (seq/ts assigned later by the ring;
    /// ts is carried through from the rollout line when present). Empty-text
    /// non-tool messages are dropped so the feed carries no hollow rows
    /// (`(*codexFold).emit`, `watch_codex.go:85`).
    fn emit(&mut self, role: &str, typ: &str, text: &str, ts: &str, tool: Option<FeedTool>) {
        if tool.is_none() && trim_feed_text(text).is_empty() {
            return;
        }
        self.msgs.push(FeedMessage {
            ts: ts.to_string(),
            role: role.to_string(),
            typ: typ.to_string(),
            text: text.to_string(),
            tool,
            ..FeedMessage::default()
        });
    }

    /// Test-visible pending-call count (the Go tests read `f.pending`
    /// directly).
    #[cfg(test)]
    pub(crate) fn pending_len(&self) -> usize {
        self.pending.len()
    }
}

impl MessageProducer for CodexFold {
    fn drain_messages(&mut self) -> Vec<FeedMessage> {
        std::mem::take(&mut self.msgs)
    }
}

impl ActivityFold for CodexFold {
    // `(*codexFold).applyLine`, `watch_codex.go:102`.
    fn apply_line(&mut self, line: &[u8]) -> bool {
        // Top-level object gate (RES-3): Go rejects the seq form and no-ops a
        // top-level `null` into the zero envelope, whose empty type is the
        // default arm — false either way.
        if json_first_byte(line) != Some(b'{') {
            return false;
        }
        let Ok(env) = serde_json::from_slice::<CodexLine>(line) else {
            return false;
        };
        let p = CodexPayload::from_value(env.payload.as_ref().unwrap_or(&NULL_VALUE));
        let ts = env.timestamp.as_str();

        match env.typ.as_str() {
            "event_msg" => match p.typ.as_str() {
                "task_started" => {
                    self.confirmed = true;
                    self.last_boundary = CodexBoundary::Start;
                    // New turn: reset the assistant-text de-dup so a turn whose
                    // response is identical to the previous turn's is emitted
                    // rather than swallowed as a mirrored final_answer. The
                    // de-dup is only meaningful WITHIN a turn.
                    self.last_asst_text.clear();
                    true
                }
                "task_complete" => {
                    self.confirmed = true;
                    self.last_boundary = CodexBoundary::Complete;
                    if !p.last_agent_message.is_empty() {
                        self.last_msg = p.last_agent_message;
                    }
                    true
                }
                "agent_message" => {
                    self.confirmed = true;
                    if !p.message.is_empty() {
                        self.last_msg = p.message.clone();
                        // Emit EVERY agent_message — a commentary row can be
                        // the only place an interim message (a preamble between
                        // tool calls, or a turn interrupted before its
                        // final_answer) ever appears, so suppressing by phase
                        // would permanently lose it. De-dup by TEXT instead:
                        // codex re-emits the settled text as a final_answer
                        // identical to the commentary it streamed, so a row
                        // whose text equals the most recently emitted assistant
                        // row is the mirror, not a new message.
                        if p.message != self.last_asst_text {
                            self.emit(FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT, &p.message, ts, None);
                            self.last_asst_text = p.message;
                        }
                    }
                    true
                }
                "user_message" => {
                    self.confirmed = true;
                    self.last_boundary = CodexBoundary::Start;
                    // New turn boundary: reset the assistant-text de-dup (see
                    // task_started).
                    self.last_asst_text.clear();
                    self.emit(FEED_ROLE_USER, FEED_TYPE_TEXT, &p.message, ts, None);
                    true
                }
                _ => false, // token_count and other event_msg subtypes: noise
            },
            "response_item" => match p.typ.as_str() {
                "function_call" | "custom_tool_call" => {
                    self.confirmed = true;
                    if !p.call_id.is_empty() {
                        self.pending.insert(p.call_id.clone());
                        if !p.name.is_empty() {
                            self.tool_names.insert(p.call_id.clone(), p.name.clone());
                        }
                    }
                    // NOTE: detail carries the raw tool invocation (command
                    // lines, file paths — whatever the agent typed), bounded at
                    // 8 KiB by the ring's sanitizer. The feed is same-trust as
                    // the tmux pane itself; see docs/extensions/rc-helper.md.
                    let detail = first_non_empty(&p.input, &p.arguments).to_string();
                    self.emit(
                        FEED_ROLE_TOOL,
                        FEED_TYPE_TOOL_USE,
                        "",
                        ts,
                        Some(FeedTool {
                            name: p.name,
                            detail,
                        }),
                    );
                    true
                }
                "function_call_output" | "custom_tool_call_output" => {
                    self.confirmed = true;
                    let mut name = String::new();
                    if !p.call_id.is_empty() {
                        name = self.tool_names.remove(&p.call_id).unwrap_or_default();
                        self.pending.remove(&p.call_id);
                    }
                    // NOTE: detail carries the raw tool output (command
                    // results, file contents), bounded at 8 KiB — same-trust-
                    // as-the-pane posture as tool_use above.
                    self.emit(
                        FEED_ROLE_TOOL,
                        FEED_TYPE_TOOL_RESULT,
                        "",
                        ts,
                        Some(FeedTool {
                            name,
                            detail: codex_tool_output_text(p.output),
                        }),
                    );
                    true
                }
                "reasoning" => {
                    // Reasoning is activity noise (an open turn already reads
                    // working from its boundaries), but a readable summary is
                    // worth surfacing in the feed. Encrypted reasoning with no
                    // summary text emits nothing.
                    let txt = codex_reasoning_text(p.summary);
                    if !txt.is_empty() {
                        self.emit(FEED_ROLE_ASSISTANT, FEED_TYPE_REASONING, &txt, ts, None);
                    }
                    false
                }
                "message" => {
                    if p.role == "assistant" {
                        let txt = codex_message_text(p.content);
                        if !txt.is_empty() {
                            self.confirmed = true;
                            self.last_msg = txt;
                            // The assistant text is emitted from the
                            // event_msg/agent_message mirror (with text
                            // de-duplication); this response_item is
                            // activity-only so the feed doesn't carry the
                            // message twice.
                            return true;
                        }
                    }
                    false // developer/user instruction messages: not activity
                }
                _ => false, // other response_items: noise
            },
            _ => false, // session_meta, turn_context, world_state, unknown: noise
        }
    }

    fn reset(&mut self) {
        *self = CodexFold::default();
    }

    /// Drops the pending tool-call set (and its name map): a lost (oversized,
    /// skipped) record may have been the `*_output` resolving one of them, and
    /// a forever-pending call_id would pin the verdict at working until the
    /// freshness grace expired. After a gap the verdict rides the turn-boundary
    /// events alone until the next turn (`watch_codex.go:235`).
    fn note_gap(&mut self) {
        self.pending.clear();
        self.tool_names.clear();
    }

    // `(*codexFold).activity`, `watch_codex.go:215`.
    fn activity(&self) -> RcActivity {
        if !self.confirmed {
            return RcActivity::Unknown;
        }
        if !self.pending.is_empty() {
            return RcActivity::Working;
        }
        if self.last_boundary == CodexBoundary::Complete {
            return RcActivity::NeedsInput;
        }
        RcActivity::Working
    }

    fn last_message(&self) -> String {
        sanitize_last_message(&self.last_msg)
    }

    fn settled(&self) -> bool {
        self.activity() == RcActivity::NeedsInput
    }

    fn drain_fold_messages(&mut self) -> Vec<FeedMessage> {
        MessageProducer::drain_messages(self)
    }
}

/// Renders a `(custom_)function_call_output` `output` field, which is either a
/// bare string or an array of `{type, text}` blocks (the custom-tool shape),
/// into concatenated text for the feed's tool_result detail
/// (`codexToolOutputText`, `watch_codex.go:245`).
fn codex_tool_output_text(raw: &Value) -> String {
    if let Some(s) = raw.as_str() {
        return s.to_string();
    }
    codex_message_text(raw)
}

/// Concatenates a reasoning payload's `summary` blocks
/// (`[{type:"summary_text", text:"…"}]`) — empty for encrypted reasoning with
/// no summary (`codexReasoningText`, `watch_codex.go:258`).
fn codex_reasoning_text(raw: &Value) -> String {
    codex_message_text(raw)
}

/// Extracts the concatenated text of a `response_item/message` content array
/// (`[{type:"output_text"|"input_text"|"text", text:"..."}]`) —
/// `codexMessageText`, `watch_codex.go:264`. Mirrors Go's whole-array
/// unmarshal: a non-array or an array with a non-conforming element yields "".
fn codex_message_text(content: &Value) -> String {
    #[derive(Debug, Default, Deserialize)]
    struct TextBlock {
        #[serde(default, deserialize_with = "null_default")]
        text: String,
    }
    // Element-wise with FULL Go semantics: a non-array yields "", a `null`
    // element is the zero block (Go's null no-op applies at array-element
    // level too — H6 review), and any other non-object element fails the
    // whole array (the positional form serde would accept — RES-3).
    let Some(arr) = content.as_array() else {
        return String::new();
    };
    let mut blocks = Vec::with_capacity(arr.len());
    for el in arr {
        if el.is_null() {
            blocks.push(TextBlock::default());
        } else if el.is_object() {
            let Ok(b) = TextBlock::deserialize(el) else {
                return String::new();
            };
            blocks.push(b);
        } else {
            return String::new();
        }
    }
    let mut out = String::new();
    for b in blocks {
        if !b.text.is_empty() {
            if !out.is_empty() {
                out.push(' ');
            }
            out.push_str(&b.text);
        }
    }
    out
}

// ---- correlation ----

/// `~/.codex/sessions` (`""` when HOME is unset) — `codexSessionsRoot`,
/// `watch_codex.go:289`.
fn codex_sessions_root(getenv: GetEnv<'_>) -> String {
    let home = getenv("HOME");
    if home.is_empty() {
        return String::new();
    }
    format!("{home}/.codex/sessions")
}

/// Reads a rollout file's first line (`session_meta`) for correlation
/// (`peekCodexRollout`, `watch_codex.go:298`).
fn peek_codex_rollout(path: &str) -> Option<JsonlPeek> {
    #[derive(Debug, Default, Deserialize)]
    struct Meta {
        #[serde(default, deserialize_with = "null_default")]
        session_id: String,
        #[serde(default, deserialize_with = "null_default")]
        id: String,
        #[serde(default, deserialize_with = "null_default")]
        cwd: String,
        #[serde(default, deserialize_with = "null_default")]
        timestamp: String,
    }

    let f = std::fs::File::open(path).ok()?;
    let mut reader = std::io::BufReader::with_capacity(128 * 1024, f);
    let mut line = Vec::new();
    reader.read_until(b'\n', &mut line).ok()?;
    if line.is_empty() {
        return None;
    }
    // Unicode trim, like Go's bytes.TrimSpace (a U+00A0-prefixed line must
    // still peek); the lossy conversion only matters for invalid UTF-8, which
    // fails the JSON parse either way.
    let text = String::from_utf8_lossy(&line);
    let text = text.trim();
    if json_first_byte(text.as_bytes()) != Some(b'{') {
        return None; // Go: a non-object first line never peeks (null no-ops → type "")
    }
    let env = serde_json::from_str::<CodexLine>(text).ok()?;
    if env.typ != "session_meta" {
        return None;
    }
    // An ABSENT payload fails the peek (Go's unmarshal of an empty RawMessage
    // errors); an explicit `null` payload succeeds with an empty meta (Go's
    // unmarshal no-ops) — the envelope timestamp then still counts.
    let meta = match env.payload {
        None => return None,
        Some(Value::Null) => Meta::default(),
        // Go's unmarshal-into-struct requires an object (RES-3).
        Some(v) if v.is_object() => serde_json::from_value::<Meta>(v).ok()?,
        Some(_) => return None,
    };
    let mut pk = JsonlPeek {
        session_id: first_non_empty(&meta.session_id, &meta.id).to_string(),
        cwd: meta.cwd,
        created_at: None,
    };
    pk.created_at = parse_jsonl_time(first_non_empty(&meta.timestamp, &env.timestamp));
    Some(pk)
}

/// Maps a codex session to its rollout file (`correlateCodex`,
/// `watch_codex.go:333`). With a back-written agent session id it matches the
/// file whose name embeds that uuid (exact). Otherwise it filters candidates
/// by cwd + the created-at window and pins the newest, flagging ambiguity when
/// more than one survives the window. Go's `(createdAt, hasCreatedAt)` pair is
/// the `Option` here.
pub fn correlate_codex(
    getenv: GetEnv<'_>,
    cwd: &str,
    agent_session_id: &str,
    created_at: Option<DateTime<Utc>>,
) -> Option<Correlation> {
    let root = codex_sessions_root(getenv);
    if root.is_empty() {
        return None;
    }
    let files = list_jsonl_under(&root, |base| base.starts_with("rollout-"));
    if files.is_empty() {
        return None;
    }

    if !agent_session_id.is_empty() {
        for p in &files {
            if base_of(p).contains(agent_session_id) {
                return Some(Correlation {
                    path: p.clone(),
                    session_id: agent_session_id.to_string(),
                    ambiguous: false,
                });
            }
        }
        // The pinned file is gone — fall through to a fresh window match.
    }

    let mut matches = Vec::new();
    for p in files {
        // A candidate with no peeked timestamp can't be window-matched at all —
        // it is eligible only for the exact-id path above. Including it here
        // would bypass the window and could get a wrong file pinned (and
        // back-written).
        let Some(pk) = peek_codex_rollout(&p) else {
            continue;
        };
        let Some(pk_created) = pk.created_at else {
            continue;
        };
        if !cwd.is_empty() && pk.cwd != cwd {
            continue;
        }
        if let Some(created) = created_at {
            if !within_window(pk_created, created, CORRELATE_WINDOW) {
                continue;
            }
        }
        matches.push(PeekCandidate { path: p, peek: pk });
    }
    if matches.is_empty() {
        return None;
    }
    // name_tiebreak=true: rollout filenames are timestamp-prefixed, so a
    // lexical comparison is a valid chronological tiebreak for an exact
    // created-at tie.
    Some(pick_correlation(&matches, true))
}

#[cfg(test)]
mod tests {
    use super::super::watch::test_support::{base_time, fixture_lines, home_getenv};
    use super::*;

    // Mirrors TestCodexFoldFixtureArc (watch_test.go:39).
    #[test]
    fn codex_fold_fixture_arc() {
        let mut f = CodexFold::new();
        assert_eq!(f.activity(), RcActivity::Unknown, "initial verdict");

        let (mut saw_working, mut saw_pending_tool) = (false, false);
        for ln in fixture_lines("codex_turn.jsonl") {
            f.apply_line(&ln);
            saw_working = saw_working || f.activity() == RcActivity::Working;
            saw_pending_tool = saw_pending_tool || f.pending_len() > 0; // a tool call is open → working
        }
        assert!(saw_working, "expected a working verdict during the turn");
        assert!(saw_pending_tool, "expected an open tool call mid-turn");
        // The arc ends at task_complete → needs_input, settled, with the final
        // answer.
        assert_eq!(f.activity(), RcActivity::NeedsInput, "final activity");
        assert!(f.settled(), "final verdict should be settled");
        assert_eq!(f.last_message(), "2+2 equals 4.");
    }

    // Mirrors TestCodexFoldMessageMapping (hub_messages_test.go:305): the turn
    // in stream order — user prompt → assistant commentary → tool_use(exec) →
    // tool_result(exec); the final_answer's identical text is de-duped, the
    // response_item mirrors and the encrypted reasoning emit nothing.
    #[test]
    fn codex_fold_message_mapping() {
        let mut f = CodexFold::new();
        let mut msgs = Vec::new();
        for ln in fixture_lines("codex_turn.jsonl") {
            f.apply_line(&ln);
            msgs.extend(f.drain_messages());
        }

        let wants: [(&str, &str, &str, &str); 4] = [
            (FEED_ROLE_USER, FEED_TYPE_TEXT, "", "what is 2+2"),
            (FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT, "", "2+2 equals 4."),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_USE, "exec", ""),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_RESULT, "exec", ""),
        ];
        assert_eq!(msgs.len(), wants.len(), "produced rows: {msgs:?}");
        for (i, (role, typ, tool_name, text_has)) in wants.iter().enumerate() {
            let m = &msgs[i];
            assert_eq!((m.role.as_str(), m.typ.as_str()), (*role, *typ), "row {i}");
            if !tool_name.is_empty() {
                assert_eq!(
                    m.tool.as_ref().map(|t| t.name.as_str()),
                    Some(*tool_name),
                    "row {i} tool name"
                );
            }
            if !text_has.is_empty() {
                assert!(m.text.contains(text_has), "row {i} text = {:?}", m.text);
            }
        }
        // The tool_use detail carries the invocation body; the tool_result
        // detail the output.
        assert!(msgs[2]
            .tool
            .as_ref()
            .is_some_and(|t| t.detail.contains("echo hello-from-codex")));
        assert!(msgs[3]
            .tool
            .as_ref()
            .is_some_and(|t| t.detail.contains("hello-from-codex")));
        // ts flows through from the rollout line (not stamped by the clock
        // here).
        assert!(
            msgs[0].ts.starts_with("2026-07-11T"),
            "ts = {:?}",
            msgs[0].ts
        );
    }

    // Mirrors TestCodexFoldAssistantTextDedup (hub_messages_test.go:362).
    #[test]
    fn codex_fold_assistant_text_dedup() {
        let line = |phase: &str, msg: &str| -> Vec<u8> {
            format!(
                r#"{{"type":"event_msg","payload":{{"type":"agent_message","phase":"{phase}","message":"{msg}"}}}}"#
            )
            .into_bytes()
        };

        // commentary ≠ final: both emitted, in order.
        let mut f = CodexFold::new();
        f.apply_line(&line("commentary", "Let me check the tests first."));
        f.apply_line(&line("final_answer", "All tests pass."));
        let msgs = f.drain_messages();
        assert_eq!(
            msgs.iter().map(|m| m.text.as_str()).collect::<Vec<_>>(),
            ["Let me check the tests first.", "All tests pass."],
            "distinct commentary+final must both emit"
        );

        // commentary == final: one message.
        let mut f2 = CodexFold::new();
        f2.apply_line(&line("commentary", "4."));
        f2.apply_line(&line("final_answer", "4."));
        let msgs = f2.drain_messages();
        assert_eq!(msgs.len(), 1, "identical commentary+final must emit once");
        assert_eq!(msgs[0].text, "4.");

        // An interrupted turn (commentary only, no final_answer) still has its
        // message.
        let mut f3 = CodexFold::new();
        f3.apply_line(&line("commentary", "Starting the refactor now."));
        assert_eq!(f3.drain_messages().len(), 1, "commentary-only turn emits");
    }

    // The fold half of TestCodexFoldGapClearsPendingThenTaskCompleteSettles
    // (watch_test.go:1335 drives it through the fileWatcher, which arrives in
    // H7): a gap must clear the pending call so a swallowed *_output line
    // can't pin the verdict at working past task_complete.
    #[test]
    fn codex_fold_gap_clears_pending_then_task_complete_settles() {
        let mut f = CodexFold::new();
        f.apply_line(br#"{"type":"event_msg","payload":{"type":"task_started"}}"#);
        f.apply_line(
            br#"{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec"}}"#,
        );
        assert_eq!(f.activity(), RcActivity::Working, "open tool call");

        // The tool's OUTPUT line was pathological and skipped — the tailer
        // reports a gap.
        f.note_gap();
        f.apply_line(
            br#"{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}"#,
        );
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "gap cleared the pending call"
        );
    }

    // The codex arm of TestFoldsToleratePathologicalLines (watch_test.go:768).
    #[test]
    fn codex_fold_tolerates_pathological_lines() {
        let mut f = CodexFold::new();
        let bad: [&[u8]; 6] = [
            b"not json at all",
            br#"{"type":"#,
            br#"{"type":"totally_unknown_type"}"#,
            b"{}",
            b"",
            br#"{"type":"event_msg","payload":{"type":"token_count"}}"#,
        ];
        for b in bad {
            assert!(!f.apply_line(b), "line {:?} should not advance state", b);
        }
        assert_eq!(f.activity(), RcActivity::Unknown, "after only-noise");
    }

    /// Writes a rollout at the real dated layout (`writeCodexRollout`,
    /// `watch_test.go:1047`).
    fn write_codex_rollout(
        root: &std::path::Path,
        session_id: &str,
        cwd: &str,
        created_at: DateTime<Utc>,
    ) -> String {
        let day = created_at.format("%Y/%m/%d").to_string();
        let dir = root.join(day);
        std::fs::create_dir_all(&dir).expect("mkdir");
        let name = format!(
            "rollout-{}-{session_id}.jsonl",
            created_at.format("%Y-%m-%dT%H-%M-%S")
        );
        let path = dir.join(name);
        let line = format!(
            r#"{{"timestamp":{ts},"type":"session_meta","payload":{{"session_id":{sid},"cwd":{cwd},"timestamp":{tsn}}}}}"#,
            ts = serde_json::to_string(&created_at.to_rfc3339()).unwrap(),
            sid = serde_json::to_string(session_id).unwrap(),
            cwd = serde_json::to_string(cwd).unwrap(),
            tsn = serde_json::to_string(&created_at.to_rfc3339()).unwrap(),
        );
        std::fs::write(&path, format!("{line}\n")).expect("write");
        path.to_str().unwrap().to_string()
    }

    // Mirrors TestCorrelateCodexTwoSessionsOneWorkdir (watch_test.go:1062).
    #[test]
    fn correlate_codex_two_sessions_one_workdir() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let root = home.path().join(".codex").join("sessions");
        let base = base_time();

        // Session A (target): created at base. Session B: 10 min earlier
        // (outside the ±60s window). Same cwd.
        let path_a = write_codex_rollout(&root, "aaaa-a", "/home/shed", base);
        write_codex_rollout(
            &root,
            "bbbb-b",
            "/home/shed",
            base - chrono::Duration::minutes(10),
        );

        let corr = correlate_codex(
            &getenv,
            "/home/shed",
            "",
            Some(base + chrono::Duration::seconds(5)),
        )
        .expect("a correlation");
        assert_eq!(corr.path, path_a, "the in-window session wins");
        assert!(!corr.ambiguous, "single in-window match is unambiguous");
        assert_eq!(corr.session_id, "aaaa-a");

        // A second session inside the window makes it ambiguous → newest
        // chosen.
        let newer = write_codex_rollout(
            &root,
            "cccc-c",
            "/home/shed",
            base + chrono::Duration::seconds(20),
        );
        let corr = correlate_codex(
            &getenv,
            "/home/shed",
            "",
            Some(base + chrono::Duration::seconds(5)),
        )
        .expect("ambiguous match");
        assert!(corr.ambiguous, ">1 in-window candidate is ambiguous");
        assert_eq!(corr.path, newer, "the newest wins");
    }

    // Mirrors TestCorrelateCodexByBackWrittenID (watch_test.go:1103): the
    // back-written id pins the OLD file exactly, even outside any created-at
    // window.
    #[test]
    fn correlate_codex_by_back_written_id() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let root = home.path().join(".codex").join("sessions");
        let base = base_time();
        let old = write_codex_rollout(
            &root,
            "pinned-id",
            "/home/shed",
            base - chrono::Duration::hours(1),
        );
        write_codex_rollout(&root, "other-id", "/home/shed", base);

        let corr =
            correlate_codex(&getenv, "/home/shed", "pinned-id", Some(base)).expect("id match");
        assert_eq!(corr.path, old);
    }

    // Null-tolerance pins (H5 review): an explicit `null` payload peeks like
    // Go (empty meta, envelope timestamp counts); an ABSENT payload fails the
    // peek (Go's unmarshal of an empty RawMessage errors).
    #[test]
    fn peek_codex_rollout_null_payload() {
        let dir = tempfile::tempdir().expect("tempdir");
        let null_payload = dir.path().join("rollout-null.jsonl");
        std::fs::write(
            &null_payload,
            concat!(
                r#"{"timestamp":"2026-07-11T17:00:00Z","type":"session_meta","payload":null}"#,
                "
"
            ),
        )
        .expect("write");
        let pk = peek_codex_rollout(null_payload.to_str().unwrap())
            .expect("null payload peeks with an empty meta");
        assert_eq!(pk.session_id, "");
        assert!(pk.created_at.is_some(), "the envelope timestamp counts");

        let absent = dir.path().join("rollout-absent.jsonl");
        std::fs::write(
            &absent,
            concat!(
                r#"{"timestamp":"2026-07-11T17:00:00Z","type":"session_meta"}"#,
                "
"
            ),
        )
        .expect("write");
        assert!(
            peek_codex_rollout(absent.to_str().unwrap()).is_none(),
            "an absent payload fails the peek, like Go"
        );
    }

    // The null-ELEMENT pin (H6 review, HIGH): a null message-content element
    // is the zero block, like Go — the surrounding text still folds.
    #[test]
    fn codex_null_content_element() {
        let mut f = CodexFold::new();
        assert!(f.apply_line(
            br#"{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"a"},null]}}"#
        ));
        assert_eq!(f.last_message(), "a");
    }

    // RES-3 pins (H5 review re-run): the top-level seq form and a seq-form
    // peek payload are rejected like Go; a seq-form message-content element
    // yields "".
    #[test]
    fn codex_fold_seq_forms_rejected() {
        let mut f = CodexFold::new();
        assert!(!f.apply_line(
            br#"["2026-07-11T17:00:00Z","event_msg",{"type":"agent_message","message":"seqform"}]"#
        ));
        assert!(!f.apply_line(b"null"));
        assert_eq!(f.activity(), RcActivity::Unknown);
        assert!(f.drain_messages().is_empty());

        // A message-content element in positional form contributes nothing.
        f.apply_line(
            br#"{"type":"response_item","payload":{"type":"message","role":"assistant","content":[["a"]]}}"#,
        );
        assert_eq!(f.last_message(), "", "seq-form content must not fold text");

        // A seq-form session_meta payload never peeks.
        let dir = tempfile::tempdir().expect("tempdir");
        let p = dir.path().join("rollout-seq.jsonl");
        std::fs::write(
            &p,
            concat!(
                r#"{"timestamp":"2026-07-11T17:00:00Z","type":"session_meta","payload":["sid","id2","/c","t"]}"#,
                "
"
            ),
        )
        .expect("write");
        assert!(peek_codex_rollout(p.to_str().unwrap()).is_none());
    }

    // The codex arm of TestCorrelateExcludesNoTimestampCandidates
    // (watch_test.go:1394).
    #[test]
    fn correlate_codex_excludes_no_timestamp_candidates() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let dir = home
            .path()
            .join(".codex")
            .join("sessions")
            .join("2026/07/11");
        std::fs::create_dir_all(&dir).expect("mkdir");
        std::fs::write(
            dir.join("rollout-x-notime.jsonl"),
            concat!(
                r#"{"type":"session_meta","payload":{"session_id":"nt-1","cwd":"/home/shed"}}"#,
                "\n"
            ),
        )
        .expect("write");

        assert!(
            correlate_codex(&getenv, "/home/shed", "", Some(base_time())).is_none(),
            "a no-timestamp candidate must not window-match"
        );
        // It remains reachable via the exact-id path (the filename embeds the
        // id).
        let corr = correlate_codex(&getenv, "/home/shed", "notime", Some(base_time()))
            .expect("exact-id match still works");
        assert!(base_of(&corr.path).contains("notime"));
    }
}
