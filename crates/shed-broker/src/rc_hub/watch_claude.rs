//! The claude transcript fold + correlation — a port of
//! `internal/ext/rc/watch_claude.go`.
//!
//! claude transcripts live at `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`,
//! one JSON object per line with a top-level type. The types the fold reads:
//!
//! - type "user", `message.content` a STRING: a typed human prompt (a turn
//!   begins → working).
//! - type "user", `message.content` an array of `{type:"tool_result",
//!   tool_use_id}`: a tool result (resolves a pending tool_use).
//! - type "assistant", `message.content` blocks: `{type:"text", text}` → the
//!   assistant's message (preview); a text tail with no pending tool_use ⇒
//!   needs_input. `{type:"tool_use", id}` → a tool invocation ⇒ working (id
//!   pending until its tool_result).
//! - type "system"|"summary"|meta rows (custom-title, mode, …): ignored.
//!
//! Parsing is tolerant: unknown types, unparseable lines, or missing fields
//! are ignored; the fold retains its prior verdict (the hub falls back to pane
//! stability when the watcher goes non-fresh).

use std::collections::HashSet;
use std::io::{BufRead, Read};

use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::Value;
use shed_core::rc::RcActivity;
use shed_rc_engine::ops::GetEnv;

use super::messages::{null_default, sanitize_last_message};
use super::watch::{
    json_first_byte, object_opt, parse_jsonl_time, pick_correlation, value_opt, within_window,
    ActivityFold, Correlation, JsonlPeek, PeekCandidate, CORRELATE_WINDOW,
};

/// The generic envelope (`claudeLine`, `watch_claude.go:32`). `content` is held
/// as a `Value` because it is polymorphic (string for a typed prompt, array of
/// blocks otherwise) — Go keeps it a `json.RawMessage` for the same reason.
///
/// Every decoded string field rides `null_default` (H5 review, HIGH): Go's
/// `json.Unmarshal` treats an explicit `null` as a no-op, and real claude
/// transcripts carry `"stop_reason":null` on tens of thousands of lines — a
/// bare serde `String` would reject the whole line where Go folds it. A
/// genuinely wrong type still errors, like Go. (One accepted delta, recorded:
/// DUPLICATE JSON keys error here where Go takes last-wins — no real JSON
/// producer emits them.)
#[derive(Debug, Default, Deserialize)]
struct ClaudeLine {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    /// `object_opt`: Go decodes this through a pointer — an object decodes,
    /// `null`/absent is nil, and any other shape fails the line.
    #[serde(default, deserialize_with = "object_opt")]
    message: Option<ClaudeMessage>,
}

#[derive(Debug, Default, Deserialize)]
struct ClaudeMessage {
    /// Decoded-but-unused, exactly like Go's `Role` field: a wrong-typed
    /// `role` must fail the line on both sides (the underscore silences
    /// dead-code — the DECODE is the point).
    #[serde(default, rename = "role", deserialize_with = "null_default")]
    _role: String,
    /// `value_opt` keeps Go's absent-vs-null distinction (H5 review RES-1):
    /// an ABSENT content is a nil RawMessage (unmarshal into a string errors
    /// → not a prompt), while an explicit `null` no-ops into the empty string
    /// → a typed prompt, exactly like a real string.
    #[serde(default, deserialize_with = "value_opt")]
    content: Option<Value>,
    #[serde(default, deserialize_with = "null_default")]
    stop_reason: String,
}

/// One message-content block, only the read fields (`claudeBlock`,
/// `watch_claude.go:162`).
#[derive(Debug, Default, Deserialize)]
struct ClaudeBlock {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    /// text block
    #[serde(default, deserialize_with = "null_default")]
    text: String,
    /// tool_use block
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    /// tool_result block
    #[serde(default, deserialize_with = "null_default")]
    tool_use_id: String,
}

/// The claude event-kind tags for the tail verdict (`claudeKind*`,
/// `watch_claude.go:42`; Go's `""` initial is [`ClaudeKind::None`]).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
enum ClaudeKind {
    #[default]
    None,
    UserPrompt,
    AsstText,
    AsstTool,
    ToolResult,
}

/// Folds the claude transcript stream into an activity verdict (`claudeFold`,
/// `watch_claude.go:50`).
#[derive(Debug, Default)]
pub struct ClaudeFold {
    confirmed: bool,
    /// Open tool_use ids (awaiting tool_result).
    pending: HashSet<String>,
    last_kind: ClaudeKind,
    /// stop_reason of the last assistant message.
    last_stop: String,
    /// Last assistant text (sanitized on read).
    last_msg: String,
}

impl ClaudeFold {
    pub fn new() -> ClaudeFold {
        ClaudeFold::default()
    }
}

impl ActivityFold for ClaudeFold {
    // `(*claudeFold).applyLine`, `watch_claude.go:68`.
    fn apply_line(&mut self, line: &[u8]) -> bool {
        // Go's Unmarshal-into-struct rejects every top-level shape but an
        // object (a top-level `null` no-ops to the zero value, whose nil
        // message returns false below — same outcome as rejecting here).
        if json_first_byte(line) != Some(b'{') {
            return false;
        }
        let Ok(env) = serde_json::from_slice::<ClaudeLine>(line) else {
            return false;
        };
        let Some(message) = env.message else {
            return false;
        };
        match env.typ.as_str() {
            "user" => {
                // content is either a typed-prompt string or a tool_result
                // block array. An explicit `null` counts as a prompt too:
                // Go's `Unmarshal("null", &str)` succeeds as a no-op (RES-1).
                if matches!(&message.content, Some(v) if v.is_string() || v.is_null()) {
                    self.confirmed = true;
                    self.last_kind = ClaudeKind::UserPrompt;
                    return true;
                }
                let Some(blocks) = claude_blocks(message.content.as_ref()) else {
                    return false;
                };
                let mut advanced = false;
                for b in &blocks {
                    if b.typ == "tool_result" && !b.tool_use_id.is_empty() {
                        self.pending.remove(&b.tool_use_id);
                        self.confirmed = true;
                        self.last_kind = ClaudeKind::ToolResult;
                        advanced = true;
                    }
                }
                advanced
            }
            "assistant" => {
                let Some(blocks) = claude_blocks(message.content.as_ref()) else {
                    return false;
                };
                self.last_stop = message.stop_reason;
                let mut advanced = false;
                for b in &blocks {
                    match b.typ.as_str() {
                        "tool_use" => {
                            if !b.id.is_empty() {
                                self.pending.insert(b.id.clone());
                            }
                            self.confirmed = true;
                            self.last_kind = ClaudeKind::AsstTool;
                            advanced = true;
                        }
                        "text" => {
                            if !b.text.is_empty() {
                                self.last_msg = b.text.clone();
                            }
                            self.confirmed = true;
                            // A tool_use in the same message dominates the
                            // tail verdict (working); don't let a leading text
                            // block downgrade it.
                            if self.last_kind != ClaudeKind::AsstTool {
                                self.last_kind = ClaudeKind::AsstText;
                            }
                            advanced = true;
                        }
                        _ => {}
                    }
                }
                advanced
            }
            _ => false, // system/summary/meta rows: not activity
        }
    }

    fn reset(&mut self) {
        *self = ClaudeFold::default();
    }

    /// Drops the pending tool_use set: a lost (oversized, skipped) record may
    /// have been the tool_result resolving one of them — same rationale as the
    /// codex fold's note_gap. The verdict then rides the message tail
    /// (last_kind/last_stop) until the next exchange (`watch_claude.go:155`).
    fn note_gap(&mut self) {
        self.pending.clear();
    }

    // `(*claudeFold).activity`, `watch_claude.go:131`.
    fn activity(&self) -> RcActivity {
        if !self.confirmed {
            return RcActivity::Unknown;
        }
        if !self.pending.is_empty() {
            return RcActivity::Working;
        }
        // A text tail is needs_input UNLESS the message's stop_reason marks it
        // as a mid-turn text block that a tool_use follows (claude splits the
        // two across lines with a shared stop_reason:"tool_use"). Any
        // other/absent stop_reason falls back to the base rule — a text tail
        // with no pending tool_use ⇒ needs_input.
        if self.last_kind == ClaudeKind::AsstText && self.last_stop != "tool_use" {
            return RcActivity::NeedsInput;
        }
        // user_prompt (awaiting the assistant), tool_result (assistant will
        // continue), or a mid-turn text block awaiting its tool_use.
        RcActivity::Working
    }

    fn last_message(&self) -> String {
        sanitize_last_message(&self.last_msg)
    }

    fn settled(&self) -> bool {
        self.activity() == RcActivity::NeedsInput
    }
}

/// Decodes a content array; `None` when content is not an array (e.g. a bare
/// string, handled by the caller) or any element fails the block shape —
/// `claudeBlocks`, `watch_claude.go:171` (Go's whole-array unmarshal fails on
/// one bad element; `from_value` on the array mirrors that).
fn claude_blocks(content: Option<&Value>) -> Option<Vec<ClaudeBlock>> {
    let arr = content?.as_array()?;
    // Element-wise with FULL Go semantics: an object decodes, a `null`
    // element is the zero block (Go's null-is-a-no-op applies at every level,
    // array elements included — H6 review), and any other shape fails the
    // whole array like Go's unmarshal (the positional seq form serde would
    // accept — RES-3).
    let mut blocks = Vec::with_capacity(arr.len());
    for el in arr {
        if el.is_null() {
            blocks.push(ClaudeBlock::default());
        } else if el.is_object() {
            blocks.push(ClaudeBlock::deserialize(el).ok()?);
        } else {
            return None;
        }
    }
    Some(blocks)
}

// ---- correlation ----

/// `~/.claude/projects` (`""` when HOME is unset) — `claudeProjectsRoot`,
/// `watch_claude.go:185`.
pub(crate) fn claude_projects_root(getenv: GetEnv<'_>) -> String {
    let home = getenv("HOME");
    if home.is_empty() {
        return String::new();
    }
    format!("{home}/.claude/projects")
}

/// Maps a cwd to claude's project-dir name: every rune that is not an ASCII
/// letter or digit becomes '-'. e.g. "/home/shed" → "-home-shed"
/// (`encodeClaudeProject`, `watch_claude.go:195`).
pub(crate) fn encode_claude_project(cwd: &str) -> String {
    cwd.chars()
        .map(|r| if r.is_ascii_alphanumeric() { r } else { '-' })
        .collect()
}

/// The scanner bound `peekClaudeTranscript` reads lines under
/// (`watch_claude.go:217` — Go's bufio.Scanner 2 MiB max token; a longer line
/// stops the scan).
const PEEK_MAX_LINE: usize = 2 * 1024 * 1024;

/// Reads a transcript's first lines for correlation (`peekClaudeTranscript`,
/// `watch_claude.go:210`): the session id (on every row), the cwd (on
/// system/message rows), and the earliest timestamp.
fn peek_claude_transcript(path: &str) -> Option<JsonlPeek> {
    #[derive(Debug, Default, Deserialize)]
    struct Row {
        #[serde(default, rename = "sessionId")]
        session_id: String,
        #[serde(default)]
        cwd: String,
        #[serde(default)]
        timestamp: String,
    }

    let f = std::fs::File::open(path).ok()?;
    let mut reader = std::io::BufReader::new(f);
    let mut pk = JsonlPeek::default();
    let mut buf = Vec::new();
    for _ in 0..40 {
        buf.clear();
        // Bounded like Go's scanner buffer: never buffer more than one
        // oversized line's worth before the max-token stop below.
        match (&mut reader)
            .take(PEEK_MAX_LINE as u64 + 1)
            .read_until(b'\n', &mut buf)
        {
            Ok(0) => break,
            Ok(_) => {}
            Err(_) => break,
        }
        if buf.len() > PEEK_MAX_LINE {
            break; // the Go scanner's max-token stop
        }
        let Ok(row) = serde_json::from_slice::<Row>(&buf) else {
            continue;
        };
        if pk.session_id.is_empty() {
            pk.session_id = row.session_id;
        }
        if pk.cwd.is_empty() {
            pk.cwd = row.cwd;
        }
        if pk.created_at.is_none() {
            pk.created_at = parse_jsonl_time(&row.timestamp);
        }
    }
    if pk.session_id.is_empty() {
        return None;
    }
    Some(pk)
}

/// Maps a claude session to its transcript (`correlateClaude`,
/// `watch_claude.go:250`). The project dir is derived from the cwd encoding;
/// with a back-written agent session id the file is named directly, otherwise
/// candidates in the dir are filtered by the created-at window and the newest
/// is pinned (ambiguous when >1 survive). Go's `(createdAt, hasCreatedAt)`
/// pair is the `Option` here.
pub fn correlate_claude(
    getenv: GetEnv<'_>,
    cwd: &str,
    agent_session_id: &str,
    created_at: Option<DateTime<Utc>>,
) -> Option<Correlation> {
    let root = claude_projects_root(getenv);
    if root.is_empty() || cwd.is_empty() {
        return None;
    }
    let dir = format!("{root}/{}", encode_claude_project(cwd));

    if !agent_session_id.is_empty() {
        let p = format!("{dir}/{agent_session_id}.jsonl");
        if std::fs::metadata(&p).is_ok_and(|m| !m.is_dir()) {
            return Some(Correlation {
                path: p,
                session_id: agent_session_id.to_string(),
                ambiguous: false,
            });
        }
        // The pinned file is gone — fall through to a window match.
    }

    let entries = std::fs::read_dir(&dir).ok()?;
    // Sorted by name, like Go's os.ReadDir: pick_correlation's
    // name_tiebreak=false leaves an exact created-at tie to SLICE order, so
    // the iteration order is contract (H5 review: fs::read_dir is
    // platform-arbitrary).
    let mut sorted: Vec<_> = entries.flatten().collect();
    sorted.sort_by_key(std::fs::DirEntry::file_name);
    let mut matches = Vec::new();
    for e in sorted {
        let path = e.path();
        // ends_with mirrors Go's filepath.Ext check, dotfiles included
        // (`filepath.Ext(".jsonl") == ".jsonl"`; Path::extension() would see
        // none).
        let is_jsonl = e
            .file_name()
            .to_str()
            .is_some_and(|n| n.ends_with(".jsonl"));
        if e.file_type().is_ok_and(|t| t.is_dir()) || !is_jsonl {
            continue;
        }
        let Some(p) = path.to_str() else { continue };
        // No peeked timestamp → the window can't be applied; such a file is
        // eligible only for the exact-id path above (a UUID filename is no
        // chronology signal).
        let Some(pk) = peek_claude_transcript(p) else {
            continue;
        };
        let Some(pk_created) = pk.created_at else {
            continue;
        };
        // The encoded project-dir name is LOSSY ("a-b" and "a_b" share a dir),
        // so the dir alone does not prove the cwd: when the transcript records
        // its cwd it must equal the session's workdir exactly.
        if !pk.cwd.is_empty() && pk.cwd != cwd {
            continue;
        }
        if let Some(created) = created_at {
            if !within_window(pk_created, created, CORRELATE_WINDOW) {
                continue;
            }
        }
        matches.push(PeekCandidate {
            path: p.to_string(),
            peek: pk,
        });
    }
    if matches.is_empty() {
        return None;
    }
    // name_tiebreak=false: claude transcript names are bare session UUIDs —
    // lexical order carries no chronology, so an exact created-at tie is left
    // to slice order.
    Some(pick_correlation(&matches, false))
}

/// A path's basename (`filepath.Base`) — used by `correlate_codex`'s
/// exact-id scan and by both fold test mods' path assertions.
pub(crate) fn base_of(path: &str) -> &str {
    std::path::Path::new(path)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or(path)
}

#[cfg(test)]
mod tests {
    use super::super::watch::test_support::{base_time, fixture_lines, home_getenv};
    use super::*;
    use std::io::Write;
    use std::path::Path;

    // Mirrors TestClaudeFoldFixtureArc (watch_test.go:76).
    #[test]
    fn claude_fold_fixture_arc() {
        let lines = fixture_lines("claude_turn.jsonl");
        let mut f = ClaudeFold::new();
        assert_eq!(f.activity(), RcActivity::Unknown, "initial verdict");

        // Step the arc; working must appear (prompt / tool_use / tool_result)
        // before the final tail.
        let mut saw_working = false;
        for ln in &lines {
            if f.apply_line(ln) && f.activity() == RcActivity::Working {
                saw_working = true;
            }
        }
        assert!(saw_working, "expected working during the turn");
        assert_eq!(f.activity(), RcActivity::NeedsInput, "final activity");
        assert!(f.settled(), "final verdict should be settled");
        assert_eq!(
            f.last_message(),
            "Done — the command printed `hello-from-claude`."
        );
    }

    // Mirrors TestClaudeFoldNoMidTurnFlap (watch_test.go:116): the mid-turn
    // split (assistant text with stop_reason:"tool_use", then a tool_use line)
    // must read working the whole way — no transient needs_input flap.
    #[test]
    fn claude_fold_no_mid_turn_flap() {
        let mut f = ClaudeFold::new();
        f.apply_line(br#"{"type":"user","message":{"role":"user","content":"hi"}}"#);
        assert_eq!(f.activity(), RcActivity::Working, "after prompt");
        // Text block carrying stop_reason tool_use (more of the turn follows).
        f.apply_line(br#"{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"working on it"}]}}"#);
        assert_eq!(f.activity(), RcActivity::Working, "mid-turn text (no flap)");
        f.apply_line(br#"{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}"#);
        assert_eq!(f.activity(), RcActivity::Working, "tool_use");
        f.apply_line(br#"{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}"#);
        assert_eq!(
            f.activity(),
            RcActivity::Working,
            "tool_result (turn continues)"
        );
        f.apply_line(br#"{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"all done"}]}}"#);
        assert_eq!(f.activity(), RcActivity::NeedsInput, "end_turn text");
        assert_eq!(f.last_message(), "all done");
    }

    // The claude arm of TestFoldsToleratePathologicalLines (watch_test.go:768).
    #[test]
    fn claude_fold_tolerates_pathological_lines() {
        let mut f = ClaudeFold::new();
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

    // The null-tolerance pin (H5 review, HIGH): Go's json.Unmarshal no-ops on
    // an explicit null, and real transcripts carry `"stop_reason":null` on
    // tens of thousands of lines — the line must still fold.
    #[test]
    fn claude_fold_tolerates_null_fields() {
        let mut f = ClaudeFold::new();
        assert!(f.apply_line(
            br#"{"type":"assistant","message":{"role":"assistant","stop_reason":null,"stop_sequence":null,"content":[{"type":"text","text":"all done"}]}}"#
        ));
        assert_eq!(f.activity(), RcActivity::NeedsInput);
        assert_eq!(f.last_message(), "all done");
        // A genuinely WRONG type still fails the line, like Go (role is
        // decoded-but-unused on both sides).
        let mut f2 = ClaudeFold::new();
        assert!(!f2.apply_line(br#"{"type":"user","message":{"content":"hi","role":123}}"#));
        // Null block fields tolerated too.
        let mut f3 = ClaudeFold::new();
        assert!(f3.apply_line(
            br#"{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","text":null}]}}"#
        ));
        assert_eq!(f3.activity(), RcActivity::Working);
    }

    // RES-1/RES-3 pins (H5 review re-run): an explicit `content:null` IS a
    // typed prompt in Go (Unmarshal into a string no-ops), an ABSENT content
    // is not, and the positional seq form serde derives would accept is
    // rejected like Go's decoder.
    #[test]
    fn claude_fold_null_content_and_seq_form() {
        let mut f = ClaudeFold::new();
        assert!(
            f.apply_line(br#"{"type":"user","message":{"role":"user","content":null}}"#),
            "null content is a typed prompt"
        );
        assert_eq!(f.activity(), RcActivity::Working);
        let mut f2 = ClaudeFold::new();
        assert!(
            !f2.apply_line(br#"{"type":"user","message":{"role":"user"}}"#),
            "absent content is not a prompt"
        );
        // Top-level and nested seq forms are rejected.
        assert!(!f2.apply_line(br#"["user",{"role":"user","content":"hi"}]"#));
        assert!(!f2.apply_line(br#"null"#));
        assert!(!f2.apply_line(br#"{"type":"user","message":["user",{"content":"hi"}]}"#));
        // A non-object BLOCK element fails the array, like Go.
        assert!(!f2.apply_line(br#"{"type":"assistant","message":{"content":[["text","x"]]}}"#));
        assert_eq!(f2.activity(), RcActivity::Unknown);
    }

    // The null-ELEMENT pin (H6 review, HIGH): a null content-block element
    // decodes as the zero block, like Go — the line (and its text preview)
    // must survive.
    #[test]
    fn claude_fold_null_block_element() {
        let mut f = ClaudeFold::new();
        assert!(f.apply_line(
            br#"{"type":"assistant","message":{"content":[{"type":"text","text":"a"},null]}}"#
        ));
        assert_eq!(f.activity(), RcActivity::NeedsInput);
        assert_eq!(f.last_message(), "a");
    }

    // Mirrors TestEncodeClaudeProject (watch_test.go:1032).
    #[test]
    fn encode_claude_project_cases() {
        let cases = [
            ("/home/shed", "-home-shed"),
            ("/home/shed/my.project", "-home-shed-my-project"),
            ("/Users/dev/code_2", "-Users-dev-code-2"),
        ];
        for (input, want) in cases {
            assert_eq!(encode_claude_project(input), want);
        }
    }

    fn write_claude_transcript(
        project_dir: &Path,
        session_id: &str,
        cwd: &str,
        created_at: DateTime<Utc>,
    ) -> String {
        std::fs::create_dir_all(project_dir).expect("mkdir");
        let path = project_dir.join(format!("{session_id}.jsonl"));
        let first = format!(
            r#"{{"type":"system","cwd":{},"timestamp":{},"sessionId":{}}}"#,
            serde_json::to_string(cwd).unwrap(),
            serde_json::to_string(&created_at.to_rfc3339()).unwrap(),
            serde_json::to_string(session_id).unwrap(),
        );
        let mut f = std::fs::File::create(&path).expect("create");
        writeln!(f, "{first}").expect("write");
        path.to_str().unwrap().to_string()
    }

    // Mirrors TestCorrelateClaudeWindowAndID (watch_test.go:1136).
    #[test]
    fn correlate_claude_window_and_id() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let cwd = "/home/shed";
        let project_dir = home
            .path()
            .join(".claude")
            .join("projects")
            .join(encode_claude_project(cwd));
        let base = base_time();

        let in_window = write_claude_transcript(&project_dir, "aaaa-a", cwd, base);
        write_claude_transcript(
            &project_dir,
            "bbbb-b",
            cwd,
            base - chrono::Duration::minutes(10),
        );

        let corr = correlate_claude(&getenv, cwd, "", Some(base + chrono::Duration::seconds(3)))
            .expect("window match");
        assert_eq!(corr.path, in_window, "the in-window transcript wins");
        assert!(!corr.ambiguous, "single window match is unambiguous");

        // Exact id match ignores the window.
        let corr = correlate_claude(&getenv, cwd, "bbbb-b", Some(base)).expect("id match");
        assert_eq!(base_of(&corr.path), "bbbb-b.jsonl");
    }

    // Mirrors TestCorrelateClaudeCwdCollisionRejected (watch_test.go:1363):
    // the encoded dir name is lossy, so a transcript whose PEEKED cwd is a
    // colliding-but-different path must not correlate.
    #[test]
    fn correlate_claude_cwd_collision_rejected() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let (cwd_a, cwd_b) = ("/home/shed/a-b", "/home/shed/a_b");
        assert_eq!(
            encode_claude_project(cwd_a),
            encode_claude_project(cwd_b),
            "precondition: the two cwds must collide in the encoding"
        );
        let project_dir = home
            .path()
            .join(".claude")
            .join("projects")
            .join(encode_claude_project(cwd_a));
        let base = base_time();

        // Only a transcript whose peeked cwd is the OTHER path exists: no
        // match for cwd_a.
        write_claude_transcript(&project_dir, "bbbb-b", cwd_b, base);
        assert!(
            correlate_claude(&getenv, cwd_a, "", Some(base)).is_none(),
            "a colliding-but-different cwd must not correlate"
        );
        // The exact-cwd transcript does match.
        let want = write_claude_transcript(&project_dir, "aaaa-a", cwd_a, base);
        let corr = correlate_claude(&getenv, cwd_a, "", Some(base)).expect("exact-cwd match");
        assert_eq!(corr.path, want);
    }

    // An exact created-at tie is left to SLICE order with name_tiebreak=false,
    // and Go's slice order is os.ReadDir's sorted order — the Rust scan must
    // sort read_dir too or the pick is platform-arbitrary (H5 review). A
    // dotfile named exactly ".jsonl" must also stay eligible (Go's
    // filepath.Ext sees it; Path::extension would not).
    #[test]
    fn correlate_claude_tie_is_lexical_and_dotfile_visible() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let cwd = "/home/shed";
        let project_dir = home
            .path()
            .join(".claude")
            .join("projects")
            .join(encode_claude_project(cwd));
        let base = base_time();
        // Same created-at, several names: the lexically-first must win
        // deterministically.
        for sid in ["zzzz-z", "aaaa-a", "mmmm-m"] {
            write_claude_transcript(&project_dir, sid, cwd, base);
        }
        for _ in 0..3 {
            let corr = correlate_claude(&getenv, cwd, "", Some(base)).expect("a pick");
            assert_eq!(
                base_of(&corr.path),
                "aaaa-a.jsonl",
                "lexically-first on a tie"
            );
            assert!(corr.ambiguous);
        }
        // A transcript named exactly ".jsonl" correlates when it is the only
        // candidate.
        let dot = tempfile::tempdir().expect("tempdir");
        let getenv2 = home_getenv(dot.path());
        let project2 = dot
            .path()
            .join(".claude")
            .join("projects")
            .join(encode_claude_project(cwd));
        std::fs::create_dir_all(&project2).expect("mkdir");
        std::fs::write(
            project2.join(".jsonl"),
            format!(
                r#"{{"type":"system","cwd":"/home/shed","timestamp":{},"sessionId":"dot-1"}}{}"#,
                serde_json::to_string(&base.to_rfc3339()).unwrap(),
                "
"
            ),
        )
        .expect("write");
        let corr = correlate_claude(&getenv2, cwd, "", Some(base)).expect("dotfile visible");
        assert_eq!(base_of(&corr.path), ".jsonl");
    }

    // The claude arm of TestCorrelateExcludesNoTimestampCandidates
    // (watch_test.go:1394): a transcript with rows carrying no timestamp never
    // window-matches; the exact-id path still resolves it by filename.
    #[test]
    fn correlate_claude_excludes_no_timestamp_candidates() {
        let home = tempfile::tempdir().expect("tempdir");
        let getenv = home_getenv(home.path());
        let cwd = "/home/shed";
        let project_dir = home
            .path()
            .join(".claude")
            .join("projects")
            .join(encode_claude_project(cwd));
        std::fs::create_dir_all(&project_dir).expect("mkdir");
        std::fs::write(
            project_dir.join("nt-2.jsonl"),
            concat!(
                r#"{"type":"system","cwd":"/home/shed","sessionId":"nt-2"}"#,
                "\n"
            ),
        )
        .expect("write");

        assert!(
            correlate_claude(&getenv, cwd, "", Some(base_time())).is_none(),
            "a no-timestamp candidate must not window-match"
        );
        let corr = correlate_claude(&getenv, cwd, "nt-2", Some(base_time()))
            .expect("exact-id match still works");
        assert_eq!(base_of(&corr.path), "nt-2.jsonl");
    }
}
