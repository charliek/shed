//! The cursor hook-event fold + transcript restart-backfill — the H5 half of
//! `internal/ext/rc/watch_cursor.go`. The push-fed `cursorWatcher` wrapper
//! (inbox bounds, freshness snapshot, the confirmed-id drain) arrives with the
//! transports in H7; what lands here is everything the watcher serializes
//! access TO: the fold itself and the transcript backfill reader.
//!
//! cursor exposes no server and its transcript JSONL is thin (user/assistant
//! lines only — no tool results, no ids, no timestamps, and it lags mid-turn).
//! Hooks are the only channel carrying tool output and turn boundaries, and
//! they only exist because the hub preseeds them; the hub's loopback ingest
//! route hands each POSTed event to the watcher, which folds it here.
//!
//! NOT an approval surface: cursor has no approval hook event (verified in the
//! plan-008 spike — a session blocked on the allowlist prompt is
//! indistinguishable, hook-wise, from a long tool call). needs_approval for
//! cursor comes from the pane anchor in reconcile
//! (`shed_core::rc_agents::approval_anchor_for`), so the fold deliberately
//! never produces it.

use std::io::{BufRead, Seek, SeekFrom};
use std::sync::LazyLock;

use regex::Regex;
use serde::Deserialize;
use serde_json::value::RawValue;
use shed_core::rc::RcActivity;

use super::messages::null_default;
use super::messages::{
    sanitize_last_message, trim_feed_text, FeedMessage, FeedTool, FEED_ROLE_ASSISTANT,
    FEED_ROLE_SYSTEM, FEED_ROLE_TOOL, FEED_ROLE_USER, FEED_TYPE_STATUS, FEED_TYPE_TEXT,
    FEED_TYPE_TOOL_RESULT, FEED_TYPE_TOOL_USE,
};
use super::watch::{
    compact_json, first_non_empty, json_first_byte, object_opt, raw_opt, vec_objects,
    MessageProducer,
};

/// Bounds what may be trusted as a cursor session id (`cursorSessionIDRe`,
/// `watch_cursor.go:314`). Cursor's session_id == conversation_id == the
/// transcript directory name — a UUID in every captured payload — so the
/// grammar is exactly a UUID. It guards two paths: a hostile
/// `SHED_RC_AGENT_SESSION` in the tmux env, and an id from a hook payload (the
/// payload arrives on a loopback route any local process can POST to, and the
/// id is back-written into the tmux env and used as a path segment by the
/// transcript backfill).
static CURSOR_SESSION_ID_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
        .expect("cursor session id regex compiles")
});

/// `validCursorSessionID`, `watch_cursor.go:316`.
pub fn valid_cursor_session_id(id: &str) -> bool {
    CURSOR_SESSION_ID_RE.is_match(id)
}

/// One hook delivery (`cursorHookEvent`, `watch_cursor.go:113`): the event
/// NAME (the ingest route's `?event=`, which the preseeded script passes as
/// argv[1]) plus the raw JSON payload cursor wrote to the script's stdin. The
/// payload is kept raw — the fold parses it — so the ingest path stays a dumb
/// pipe and every shape decision lives in one place.
#[derive(Debug, Clone)]
pub struct CursorHookEvent {
    pub event: String,
    pub payload: Vec<u8>,
}

/// The union of the fields the fold reads across cursor's hook events
/// (`cursorHookPayload`, `watch_cursor.go:323`). Every field is optional and
/// tolerantly typed — an event that carries none of them folds to nothing
/// rather than failing. Shapes verified against the live spike capture
/// (plan 008, cursor-agent 2026.08.11-e8db854).
///
/// Null tolerance (H5 review, HIGH): every decoded string/Vec field rides
/// `null_default` and the raw fields capture `null` verbatim — Go's
/// `json.Unmarshal` no-ops on an explicit `null`, and cursor's JS producer
/// demonstrably emits nulls; a bare serde field would drop the whole event,
/// dark-siding the lane. Accepted delta: duplicate JSON keys error here where
/// Go last-wins — no real JSON producer emits them.
#[derive(Debug, Default, Deserialize)]
struct CursorHookPayload {
    /// Cursor's conversation id; present on every event except workspaceOpen.
    #[serde(default, deserialize_with = "null_default")]
    session_id: String,
    /// beforeSubmitPrompt's submitted text.
    #[serde(default, deserialize_with = "null_default")]
    prompt: String,
    /// preToolUse / postToolUseFailure.
    #[serde(default, deserialize_with = "null_default")]
    tool_name: String,
    /// Kept RAW (never decoded to a Value) so the compact-JSON fallback detail
    /// preserves the producer's key order, like Go's json.Compact over the
    /// original bytes.
    #[serde(default, deserialize_with = "raw_opt")]
    tool_input: Option<Box<RawValue>>,
    /// afterShellExecution (output is the command's combined output —
    /// routinely far larger than the 16 KiB verb cap, which is why ingest has
    /// its own).
    #[serde(default, deserialize_with = "null_default")]
    command: String,
    #[serde(default, deserialize_with = "null_default")]
    output: String,
    /// afterFileEdit.
    #[serde(default, deserialize_with = "null_default")]
    file_path: String,
    /// The edit bodies are only COUNTED (see [`cursor_edit_detail`]), so they
    /// stay raw — Go's `[]json.RawMessage` — rather than being parsed into
    /// values that are never read.
    #[serde(default, deserialize_with = "null_default")]
    edits: Vec<Box<RawValue>>,
    /// postToolUseFailure.
    #[serde(default, deserialize_with = "null_default")]
    error_message: String,
    /// afterAgentResponse's assistant message.
    #[serde(default, deserialize_with = "null_default")]
    text: String,
    /// stop's turn outcome — "completed", or "aborted"/"error" for a turn the
    /// operator interrupted or that failed. reason/final_status ride
    /// sessionEnd.
    #[serde(default, deserialize_with = "null_default")]
    status: String,
    #[serde(default, deserialize_with = "null_default")]
    reason: String,
    #[serde(default, deserialize_with = "null_default")]
    final_status: String,
    /// sessionStart (and most others) — used only for the start status row.
    #[serde(default, deserialize_with = "null_default")]
    model: String,
}

/// Fold verdict states (`cursorState*`, `watch_cursor.go:373`; Go's `""` is
/// [`CursorState::Unknown`]). A cursor turn runs from beforeSubmitPrompt to
/// stop; everything in between (tool calls, agent responses) is working, and
/// stop is the settled boundary.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
enum CursorState {
    #[default]
    Unknown,
    Working,
    Idle,
}

/// Folds the hook event stream into an activity verdict + a normalized message
/// feed (`cursorFold`, `watch_cursor.go:361`) — the same contract the
/// codex/opencode folds implement. It holds cumulative state across
/// `apply_event` calls and is NOT safe for concurrent use — the watcher (H7)
/// serializes access under its mutex.
///
/// It deliberately does NOT implement [`super::watch::ActivityFold`]: that
/// trait's unit is a JSONL LINE, and a hook event is (name, payload). The
/// watcher calls `apply_event` directly; only [`MessageProducer`] is shared
/// verbatim, so reconcile's drain path is identical to every other watcher's.
#[derive(Debug, Default)]
pub struct CursorFold {
    /// ≥1 activity-relevant event folded (unknown until then).
    confirmed: bool,
    state: CursorState,
    /// The cursor session id this fold is following.
    pinned_id: String,
    /// A pin awaiting `drain_new_pin` (back-write to SHED_RC_AGENT_SESSION).
    new_pin: String,
    /// preToolUse seen without its matching postToolUse/postToolUseFailure.
    open_tools: usize,
    /// Latest assistant message text.
    last_msg: String,
    msgs: Vec<FeedMessage>,
}

impl MessageProducer for CursorFold {
    // `(*cursorFold).drainMessages`, `watch_cursor.go:596`.
    fn drain_messages(&mut self) -> Vec<FeedMessage> {
        std::mem::take(&mut self.msgs)
    }
}

impl CursorFold {
    /// `newCursorFold`, `watch_cursor.go:380`. `prior_id` is the back-written
    /// pin from an earlier hub lifetime (already grammar-validated by the
    /// watcher).
    pub fn new(prior_id: &str) -> CursorFold {
        CursorFold {
            pinned_id: prior_id.to_string(),
            ..CursorFold::default()
        }
    }

    /// Folds one hook event, returning true when it advanced meaningful state
    /// (`(*cursorFold).applyEvent`, `watch_cursor.go:389`). An unknown event
    /// name, an unparseable payload, or an event whose payload carries nothing
    /// the fold reads returns false and leaves state untouched — the same
    /// tolerant-parsing contract `ActivityFold::apply_line` states, so a
    /// cursor release that adds a field (or an event we never wired) is inert
    /// rather than fatal.
    pub fn apply_event(&mut self, ev: &CursorHookEvent) -> bool {
        // Go's Unmarshal semantics at the top level (H5 review RES-2/RES-3):
        // an empty body and a literal `null` both no-op into the zero payload
        // (the event still folds); any non-object shape errors → false.
        let trimmed = ev.payload.trim_ascii();
        let p = match json_first_byte(&ev.payload) {
            None => CursorHookPayload::default(),
            Some(b'n') if trimmed == b"null" => CursorHookPayload::default(),
            Some(b'{') => match serde_json::from_slice::<CursorHookPayload>(&ev.payload) {
                Ok(p) => p,
                Err(_) => return false,
            },
            Some(_) => return false,
        };
        // Pinning first: EVERY id-carrying event participates, because
        // sessionStart is absent on `cursor-agent --resume` and would
        // otherwise be the only way to learn the id.
        let repinned = self.note_pin(&p.session_id);
        if !self.fold_event(&ev.event, &p) {
            return repinned;
        }
        // Exactly one place sets confirmed: "a hook event told us this session
        // is alive". It follows fold_event so an event that folded NOTHING
        // (unwired, or empty text) leaves the verdict at unknown, which is
        // what lets pane stability keep driving.
        self.confirmed = true;
        true
    }

    /// Applies one recognized hook event to the fold's state
    /// (`(*cursorFold).foldEvent`, `watch_cursor.go:409`). See `apply_event`
    /// for the pinning + confirmed handling wrapped around it.
    fn fold_event(&mut self, event: &str, p: &CursorHookPayload) -> bool {
        match event {
            "sessionStart" => {
                self.state = CursorState::Idle; // a fresh TUI is parked at its composer
                let mut text = "cursor session started".to_string();
                if !p.model.is_empty() {
                    text = format!("{text} ({})", p.model);
                }
                self.emit_status(&text);
                true
            }
            "beforeSubmitPrompt" => {
                if trim_feed_text(&p.prompt).is_empty() {
                    return false;
                }
                self.state = CursorState::Working;
                self.emit(FeedMessage {
                    role: FEED_ROLE_USER.into(),
                    typ: FEED_TYPE_TEXT.into(),
                    text: p.prompt.clone(),
                    ..FeedMessage::default()
                });
                true
            }
            "preToolUse" => {
                self.state = CursorState::Working;
                self.open_tools += 1;
                self.emit(FeedMessage {
                    role: FEED_ROLE_TOOL.into(),
                    typ: FEED_TYPE_TOOL_USE.into(),
                    tool: Some(FeedTool {
                        name: p.tool_name.clone(),
                        detail: cursor_tool_detail(p),
                    }),
                    ..FeedMessage::default()
                });
                true
            }
            "afterShellExecution" => {
                // Deliberately NOT a close_tool: this is an EXTRA event riding
                // alongside the call's own postToolUse, not its terminator
                // (the spike's turn: 5 preToolUse ↔ 3 postToolUse + 2
                // postToolUseFailure, with afterShellExecution/afterFileEdit
                // on top of those). Decrementing here as well would
                // double-count and forget a genuinely open call.
                //
                // The ring sanitizes + caps the detail at 8 KiB, so the raw
                // output is handed over untouched: truncation policy lives in
                // exactly one place.
                self.emit(FeedMessage {
                    role: FEED_ROLE_TOOL.into(),
                    typ: FEED_TYPE_TOOL_RESULT.into(),
                    tool: Some(FeedTool {
                        name: "Shell".into(),
                        detail: first_non_empty(&p.output, &p.command).to_string(),
                    }),
                    ..FeedMessage::default()
                });
                true
            }
            "afterFileEdit" => {
                // Not a close_tool either — same reason as afterShellExecution
                // above.
                self.emit(FeedMessage {
                    role: FEED_ROLE_TOOL.into(),
                    typ: FEED_TYPE_TOOL_RESULT.into(),
                    tool: Some(FeedTool {
                        name: "Edit".into(),
                        detail: cursor_edit_detail(p),
                    }),
                    ..FeedMessage::default()
                });
                true
            }
            "postToolUse" => {
                // The counter's other half, and the ONLY thing this event is
                // wired for: every preToolUse is matched by exactly one
                // postToolUse or postToolUseFailure, including for the
                // Read/Grep/Glob-class tools that emit no after* event at all.
                // No feed row — its tool_output would duplicate
                // afterShellExecution/afterFileEdit for the two families that
                // DO emit one, and those carry the better-shaped detail.
                self.close_tool();
                true
            }
            "postToolUseFailure" => {
                self.close_tool();
                let mut text = "tool failed".to_string();
                if !p.tool_name.is_empty() {
                    text = format!("tool {} failed", p.tool_name);
                }
                if !p.error_message.is_empty() {
                    text = format!("{text}: {}", p.error_message);
                }
                self.emit_status(&text);
                true
            }
            "afterAgentResponse" => {
                if trim_feed_text(&p.text).is_empty() {
                    return false;
                }
                self.state = CursorState::Working; // the turn ends at `stop`, not at a message
                self.last_msg = p.text.clone();
                self.emit(FeedMessage {
                    role: FEED_ROLE_ASSISTANT.into(),
                    typ: FEED_TYPE_TEXT.into(),
                    text: p.text.clone(),
                    ..FeedMessage::default()
                });
                true
            }
            "stop" => {
                // The turn is over and cursor is back at its composer: the
                // SETTLED boundary, which snapshot then trusts indefinitely (a
                // waiting agent emits nothing at all).
                self.state = CursorState::Idle;
                self.open_tools = 0;
                // A turn that ended any way OTHER than "completed" (the spike
                // shows "aborted" and "error") settles identically — the
                // composer is live either way — but the operator needs to know
                // WHY the work stopped, or an interrupted turn reads on a
                // phone as a finished one. Only the non-completed case emits:
                // a row per normal turn end would be pure noise.
                if !p.status.is_empty() && p.status != "completed" {
                    self.emit_status(&format!("turn ended: {}", p.status));
                }
                true
            }
            "sessionEnd" => {
                self.state = CursorState::Idle;
                self.open_tools = 0;
                let mut text = "cursor session ended".to_string();
                let s = first_non_empty(&p.final_status, &p.reason);
                if !s.is_empty() {
                    text = format!("{text} ({s})");
                }
                self.emit_status(&text);
                true
            }
            _ => {
                // Unwired/unknown events (afterAgentThought,
                // beforeShellExecution, workspaceOpen, a future addition):
                // dropped silently. A repin carried by such an event still
                // counts.
                false
            }
        }
    }

    /// Adopts the session id an event carries (`(*cursorFold).notePin`,
    /// `watch_cursor.go:520`). The FIRST valid id pins the fold; a DIFFERENT
    /// valid id later means the operator switched chats in the TUI (cursor
    /// keeps one process across conversations), so the fold RE-PINS — a
    /// session is scoped to whatever its TUI is showing — announcing the
    /// switch with a status row and dropping the previous chat's carry-over
    /// state (its open tool calls and last message describe a conversation
    /// that is no longer on screen). An invalid/absent id is ignored entirely.
    fn note_pin(&mut self, id: &str) -> bool {
        if id.is_empty() || !valid_cursor_session_id(id) {
            return false;
        }
        if self.pinned_id == id {
            return false;
        }
        let switched = !self.pinned_id.is_empty();
        self.pinned_id = id.to_string();
        self.new_pin = id.to_string();
        if switched {
            self.open_tools = 0;
            self.last_msg.clear();
            // The previous chat's turn state describes a conversation no
            // longer on screen — carrying it forward would read as working
            // indefinitely (until a `stop` for the NEW chat, which needs a
            // submitted turn that may never come if the parked chat is just
            // sitting idle). Reset to unknown-until-next-event so pane
            // stability drives the gap, same as a brand-new fold before its
            // first hook.
            self.state = CursorState::Unknown;
            self.emit_status(&format!("cursor switched to another chat ({id})"));
        }
        true
    }

    /// Returns and clears a newly adopted session id (`""` when unchanged
    /// since the last drain) — `drainNewPin`, `watch_cursor.go:546`. The
    /// watcher forwards it to reconcile's SHED_RC_AGENT_SESSION back-write.
    pub fn drain_new_pin(&mut self) -> String {
        std::mem::take(&mut self.new_pin)
    }

    /// Retires one open tool call (its postToolUse/postToolUseFailure arrived
    /// — NOT the after* events, which ride alongside rather than terminate).
    /// Floored at zero: hooks are a lossy channel by construction (a hub
    /// restart mid-turn, an inbox gap), so a result whose preToolUse was never
    /// seen must not push the counter negative (`closeTool`,
    /// `watch_cursor.go:556`).
    fn close_tool(&mut self) {
        self.open_tools = self.open_tools.saturating_sub(1);
    }

    fn emit(&mut self, m: FeedMessage) {
        self.msgs.push(m);
    }

    fn emit_status(&mut self, text: &str) {
        self.emit(FeedMessage {
            role: FEED_ROLE_SYSTEM.into(),
            typ: FEED_TYPE_STATUS.into(),
            text: text.to_string(),
            ..FeedMessage::default()
        });
    }

    pub fn last_message(&self) -> String {
        sanitize_last_message(&self.last_msg)
    }

    pub fn settled(&self) -> bool {
        self.confirmed && self.state == CursorState::Idle
    }

    /// The fold's verdict (`(*cursorFold).activity`, `watch_cursor.go:577`):
    /// unknown until a hook confirms the session is alive, then working while
    /// a turn (or a tool call) is in flight and needs_input once `stop` lands.
    /// A chat switch resets state to unknown — the previous chat's turn state
    /// does not apply to whatever the operator switched to — letting pane
    /// stability drive the gap. needs_approval is deliberately absent — cursor
    /// emits NO approval hook, so that verdict comes from the pane anchor in
    /// reconcile.
    pub fn activity(&self) -> RcActivity {
        if !self.confirmed {
            return RcActivity::Unknown;
        }
        if self.open_tools > 0 {
            return RcActivity::Working;
        }
        match self.state {
            CursorState::Idle => RcActivity::NeedsInput,
            CursorState::Working => RcActivity::Working,
            CursorState::Unknown => RcActivity::Unknown,
        }
    }

    /// Drops the open-tool count after an inbox overflow (`noteGap`,
    /// `watch_cursor.go:609`): a swallowed afterShellExecution/afterFileEdit/
    /// postToolUseFailure would otherwise pin the verdict at working until the
    /// next turn boundary. The turn state itself is KEPT — `stop` is what
    /// settles a turn, and a gap is no evidence that it arrived.
    pub fn note_gap(&mut self) {
        self.open_tools = 0;
    }

    #[cfg(test)]
    pub(crate) fn pinned_id(&self) -> &str {
        &self.pinned_id
    }

    #[cfg(test)]
    pub(crate) fn open_tools(&self) -> usize {
        self.open_tools
    }

    #[cfg(test)]
    pub(crate) fn confirmed(&self) -> bool {
        self.confirmed
    }

    #[cfg(test)]
    pub(crate) fn raw_last_msg(&self) -> &str {
        &self.last_msg
    }
}

/// Renders a preToolUse's compact detail (`cursorToolDetail`,
/// `watch_cursor.go:615`): the shell command or the file path when the
/// tool_input carries one (by far the most common shapes —
/// Shell/Read/Write/Edit), else the whole compacted input object. Falls back
/// to "" for an absent input, which the feed simply omits.
fn cursor_tool_detail(p: &CursorHookPayload) -> String {
    #[derive(Debug, Default, Deserialize)]
    struct In {
        #[serde(default)]
        command: String,
        #[serde(default)]
        file_path: String,
    }
    let raw = p
        .tool_input
        .as_deref()
        .map(RawValue::get)
        .unwrap_or_default();
    // Object-gated (RES-3): Go's Unmarshal errors on the seq form serde's
    // text decode would accept, falling through to the compact fallback.
    if json_first_byte(raw.as_bytes()) == Some(b'{') {
        if let Ok(input) = serde_json::from_str::<In>(raw) {
            if !input.command.is_empty() {
                return input.command;
            }
            if !input.file_path.is_empty() {
                return input.file_path;
            }
        }
    }
    compact_json(raw)
}

/// Renders an afterFileEdit's detail (`cursorEditDetail`,
/// `watch_cursor.go:634`): the edited path plus how many edits landed. The
/// edit BODIES are deliberately not included — they are diffs of arbitrary
/// size, and the path + count is what a phone-sized feed row can use.
fn cursor_edit_detail(p: &CursorHookPayload) -> String {
    let path = if p.file_path.is_empty() {
        "(unknown file)"
    } else {
        &p.file_path
    };
    match p.edits.len() {
        0 => path.to_string(),
        1 => format!("{path} (1 edit)"),
        n => format!("{path} ({n} edits)"),
    }
}

// ---- transcript restart-backfill (plan 008 §3.5 / C5) ----

/// Bounds the backfill to the transcript's LAST N raw lines
/// (`maxCursorTranscriptLines`, `watch_cursor.go:656`) — before parsing (a
/// line can fold to zero, one, or several feed rows). Cheap insurance on top
/// of the ring's own byte/count caps: a long-running session's transcript can
/// run to thousands of lines, and this is restart history, not the live feed.
pub const MAX_CURSOR_TRANSCRIPT_LINES: usize = 200;

/// Bounds the read to the file's last N bytes before scanning
/// (`readCursorTranscriptTailBytes`, `watch_cursor.go:663`), so a transcript
/// that has grown to thousands of lines does not get read in full just to
/// keep the last [`MAX_CURSOR_TRANSCRIPT_LINES`] — this runs synchronously in
/// the reconcile tick. (Go makes it a mutable package var for tests; the
/// bounded inner fn below is this port's test seam.)
const READ_CURSOR_TRANSCRIPT_TAIL_BYTES: u64 = 256 * 1024;

/// The scanner's max line (Go's bufio.Scanner 2 MiB buffer — a longer line
/// stops the scan, keeping whatever landed in the window).
const TRANSCRIPT_MAX_LINE: usize = 2 * 1024 * 1024;

/// Renders cursor's own project-directory slug for a workdir
/// (`cursorSlugForWorkdir`, `watch_cursor.go:670`): the absolute path with
/// every '/' replaced by '-' and the leading '/' dropped. Verified live in the
/// plan-008 spike: `/home/shed/proj` slugs to `home-shed-proj`.
pub fn cursor_slug_for_workdir(workdir: &str) -> String {
    if workdir.is_empty() {
        return String::new();
    }
    let abs = abs_lexical(workdir);
    let trimmed = abs.strip_prefix('/').unwrap_or(&abs);
    if trimmed.is_empty() {
        return String::new();
    }
    trimmed.replace('/', "-")
}

/// Go's `filepath.Abs` for the slug: join a relative path onto the current
/// dir, then clean lexically (collapse `.`/`..`/`//` — no symlink
/// resolution). The hub always hands an absolute workdir; the relative arm is
/// the same best-effort Go has.
fn abs_lexical(path: &str) -> String {
    let joined = if path.starts_with('/') {
        path.to_string()
    } else {
        match std::env::current_dir() {
            Ok(cwd) => format!("{}/{}", cwd.to_string_lossy(), path),
            Err(_) => return path.to_string(), // Go: Abs error → keep as-is
        }
    };
    let mut stack: Vec<&str> = Vec::new();
    for part in joined.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                stack.pop();
            }
            p => stack.push(p),
        }
    }
    format!("/{}", stack.join("/"))
}

/// Derives the transcript JSONL path for a (home, workdir, session_id) triple
/// (`cursorTranscriptPath`, `watch_cursor.go:689`), or "" when any input is
/// unusable (no HOME, no workdir, or a session id that fails the same UUID
/// grammar every other cursor id is held to — this id ends up as a path
/// segment). "" tells the seeding path to skip silently.
pub fn cursor_transcript_path(home: &str, workdir: &str, session_id: &str) -> String {
    if home.is_empty() || !valid_cursor_session_id(session_id) {
        return String::new();
    }
    let slug = cursor_slug_for_workdir(workdir);
    if slug.is_empty() {
        return String::new();
    }
    format!("{home}/.cursor/projects/{slug}/agent-transcripts/{session_id}/{session_id}.jsonl")
}

/// Reads path's last [`MAX_CURSOR_TRANSCRIPT_LINES`] lines and folds each into
/// zero or more feed rows, in file order (`readCursorTranscript`,
/// `watch_cursor.go:713`). Best-effort: a missing/unreadable file returns
/// empty (construction must never fail on this), and an unscannable line
/// (including a partial last line, since the transcript is written
/// incrementally and a hub can restart mid-write) is simply skipped rather
/// than aborting the whole read.
///
/// The read itself is bounded to the file's last tail-bytes window: a larger
/// file is opened and seeked to the tail rather than scanned start-to-finish.
/// A seek lands mid-line more often than not, so the first line scanned after
/// a seek is always discarded — its head is missing and it is not a real line.
pub fn read_cursor_transcript(path: &str) -> Vec<FeedMessage> {
    read_cursor_transcript_bounded(path, READ_CURSOR_TRANSCRIPT_TAIL_BYTES)
}

/// The bounded inner read — `tail_bytes` is injectable so a test exercises the
/// seek-and-drop-fragment path with a small fixture (Go shrinks the package
/// var instead).
pub fn read_cursor_transcript_bounded(path: &str, tail_bytes: u64) -> Vec<FeedMessage> {
    let Ok(mut f) = std::fs::File::open(path) else {
        return Vec::new();
    };
    let mut sought = false;
    if let Ok(info) = f.metadata() {
        if info.len() > tail_bytes && f.seek(SeekFrom::End(-(tail_bytes as i64))).is_ok() {
            sought = true;
        }
    }

    let mut reader = std::io::BufReader::new(f);
    let mut window: std::collections::VecDeque<Vec<u8>> =
        std::collections::VecDeque::with_capacity(MAX_CURSOR_TRANSCRIPT_LINES);
    let mut buf = Vec::new();
    let mut first = true;
    loop {
        buf.clear();
        match reader.read_until(b'\n', &mut buf) {
            Ok(0) | Err(_) => break,
            Ok(_) => {}
        }
        if buf.len() > TRANSCRIPT_MAX_LINE {
            break; // the Go scanner's max-token stop: keep what landed so far
        }
        if buf.last() == Some(&b'\n') {
            buf.pop();
        }
        if first {
            first = false;
            if sought {
                continue; // the seek's first line is a headless fragment, not a real line
            }
        }
        if window.len() == MAX_CURSOR_TRANSCRIPT_LINES {
            window.pop_front();
        }
        window.push_back(buf.clone());
    }

    let mut rows = Vec::new();
    for raw in &window {
        rows.extend(parse_cursor_transcript_line(raw));
    }
    rows
}

/// One row of cursor's transcript JSONL — a DIFFERENT shape than a hook
/// payload (`cursorTranscriptLine`, `watch_cursor.go:763`; verified in the
/// spike: user/assistant lines only, no tool results, no ids, no timestamps,
/// plus a trailing `{"type":"turn_ended",...}`). (Accepted delta, as on
/// `CursorHookPayload`: duplicate JSON keys error here where Go last-wins.)
#[derive(Debug, Default, Deserialize)]
struct CursorTranscriptLine {
    /// "user" | "assistant" (absent on turn_ended).
    #[serde(default, deserialize_with = "null_default")]
    role: String,
    /// Decoded-but-unused, exactly like Go's `Type` field ("turn_ended" on the
    /// trailing row): a wrong-typed `type` must fail the line on both sides.
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    _typ: String,
    /// `object_opt`: Go decodes this through a pointer — object/`null`/absent
    /// only; the seq form fails the line (RES-3).
    #[serde(default, deserialize_with = "object_opt")]
    message: Option<CursorTranscriptMessage>,
}

#[derive(Debug, Default, Deserialize)]
struct CursorTranscriptMessage {
    /// `vec_objects`: Go's element unmarshal requires objects (RES-3); `null`
    /// is the nil slice.
    #[serde(default, deserialize_with = "vec_objects")]
    content: Vec<CursorTranscriptBlock>,
}

/// One content block. tool_use blocks carry no id (unlike a hook's
/// preToolUse/postToolUse pair, the transcript has nothing to correlate a
/// result back to — which is exactly why tool RESULTS never appear here at
/// all) — `cursorTranscriptBlock`, `watch_cursor.go:776`.
#[derive(Debug, Default, Deserialize)]
struct CursorTranscriptBlock {
    /// "text" | "tool_use".
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    #[serde(default, deserialize_with = "null_default")]
    text: String,
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    /// Raw for the same key-order reason as the hook payload's tool_input.
    #[serde(default, deserialize_with = "raw_opt")]
    input: Option<Box<RawValue>>,
}

/// Folds one transcript line into feed rows (`parseCursorTranscriptLine`,
/// `watch_cursor.go:789`): a user text block becomes a user row; an assistant
/// text block becomes an assistant row and an assistant tool_use block a
/// tool_use row. turn_ended and any block type this shape does not name are
/// ignored. A line that fails to unmarshal (malformed or a partial last line)
/// yields no rows rather than erroring.
pub fn parse_cursor_transcript_line(raw: &[u8]) -> Vec<FeedMessage> {
    // Top-level object gate (RES-3): Go rejects the seq form; a top-level
    // `null` no-ops to the zero line, whose nil message yields no rows —
    // same outcome.
    if json_first_byte(raw) != Some(b'{') {
        return Vec::new();
    }
    let Ok(line) = serde_json::from_slice::<CursorTranscriptLine>(raw) else {
        return Vec::new();
    };
    let Some(message) = line.message else {
        return Vec::new();
    };
    let mut rows = Vec::new();
    match line.role.as_str() {
        FEED_ROLE_USER => {
            for b in &message.content {
                if b.typ != "text" || trim_feed_text(&b.text).is_empty() {
                    continue;
                }
                rows.push(FeedMessage {
                    role: FEED_ROLE_USER.into(),
                    typ: FEED_TYPE_TEXT.into(),
                    text: b.text.clone(),
                    ..FeedMessage::default()
                });
            }
        }
        FEED_ROLE_ASSISTANT => {
            for b in &message.content {
                match b.typ.as_str() {
                    "text" => {
                        if trim_feed_text(&b.text).is_empty() {
                            continue;
                        }
                        rows.push(FeedMessage {
                            role: FEED_ROLE_ASSISTANT.into(),
                            typ: FEED_TYPE_TEXT.into(),
                            text: b.text.clone(),
                            ..FeedMessage::default()
                        });
                    }
                    "tool_use" => {
                        rows.push(FeedMessage {
                            role: FEED_ROLE_TOOL.into(),
                            typ: FEED_TYPE_TOOL_USE.into(),
                            tool: Some(FeedTool {
                                name: b.name.clone(),
                                detail: cursor_transcript_tool_detail(b.input.as_deref()),
                            }),
                            ..FeedMessage::default()
                        });
                    }
                    _ => {}
                }
            }
        }
        _ => {}
    }
    rows
}

/// Renders a transcript tool_use block's compact detail
/// (`cursorTranscriptToolDetail`, `watch_cursor.go:827`). The transcript's own
/// input shapes differ from a hook's tool_input (e.g. Write carries `path`,
/// not `file_path` — see the spike capture), so this is deliberately a
/// SEPARATE field list from [`cursor_tool_detail`] rather than a shared
/// helper.
fn cursor_transcript_tool_detail(input: Option<&RawValue>) -> String {
    #[derive(Debug, Default, Deserialize)]
    struct In {
        #[serde(default)]
        command: String,
        #[serde(default)]
        path: String,
    }
    let raw = input.map(RawValue::get).unwrap_or_default();
    // Object-gated (RES-3): Go's Unmarshal errors on the seq form serde's
    // text decode would accept, falling through to the compact fallback.
    if json_first_byte(raw.as_bytes()) == Some(b'{') {
        if let Ok(parsed) = serde_json::from_str::<In>(raw) {
            if !parsed.command.is_empty() {
                return parsed.command;
            }
            if !parsed.path.is_empty() {
                return parsed.path;
            }
        }
    }
    compact_json(raw)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    /// The spike capture's own conversation id (`cursorTestSessionID`,
    /// `watch_cursor_test.go:20`).
    const SID: &str = "4113a71f-0a42-4a6d-89b9-483e44b74103";
    const OTHER_SID: &str = "9129668a-885b-48ef-b61b-d80f981d4d68";

    fn hook_ev(event: &str, payload: &str) -> CursorHookEvent {
        CursorHookEvent {
            event: event.to_string(),
            payload: payload.as_bytes().to_vec(),
        }
    }

    fn sid_field(id: &str) -> String {
        format!(r#""session_id":"{id}""#)
    }

    fn fold_events(evs: &[CursorHookEvent]) -> CursorFold {
        let mut f = CursorFold::new("");
        for ev in evs {
            f.apply_event(ev);
        }
        f
    }

    // Mirrors TestCursorFoldTurnMapping (watch_cursor_test.go:42): a whole
    // turn, event by event, as the spike recorded it — pins the feed row for
    // each event AND the activity at each boundary.
    #[test]
    fn cursor_fold_turn_mapping() {
        let mut f = CursorFold::new("");
        assert_eq!(f.activity(), RcActivity::Unknown, "no events yet");

        f.apply_event(&hook_ev(
            "sessionStart",
            &format!(r#"{{{},"model":"cursor-grok-4.5-high"}}"#, sid_field(SID)),
        ));
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "parked at the composer"
        );

        f.apply_event(&hook_ev(
            "beforeSubmitPrompt",
            &format!(
                r#"{{{},"prompt":"Run the build","attachments":[]}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(f.activity(), RcActivity::Working, "turn started");

        f.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"Shell","tool_input":{{"command":"echo hello-tui","cwd":"","timeout":30000}},"tool_use_id":"e52e7aed"}}"#,
                sid_field(SID)
            ),
        ));
        f.apply_event(&hook_ev(
            "afterShellExecution",
            &format!(
                r#"{{{},"command":"echo hello-tui","output":"hello-tui\n","duration":39300.384}}"#,
                sid_field(SID)
            ),
        ));
        f.apply_event(&hook_ev(
            "afterFileEdit",
            &format!(
                r#"{{{},"file_path":"/home/shed/proj/notes.txt","edits":[{{"old_string":"","new_string":"hi\n"}},{{"old_string":"a","new_string":"b"}}]}}"#,
                sid_field(SID)
            ),
        ));
        f.apply_event(&hook_ev(
            "afterAgentResponse",
            &format!(
                r#"{{{},"text":"Done — the build passed."}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(
            f.activity(),
            RcActivity::Working,
            "a message does not end a turn"
        );
        assert_eq!(f.last_message(), "Done — the build passed.");

        f.apply_event(&hook_ev(
            "stop",
            &format!(
                r#"{{{},"status":"completed","loop_count":0}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "stop settles the turn"
        );
        assert!(f.settled());

        let msgs = f.drain_messages();
        let wants: [(&str, &str, &str); 6] = [
            (FEED_ROLE_SYSTEM, FEED_TYPE_STATUS, "cursor session started"),
            (FEED_ROLE_USER, FEED_TYPE_TEXT, "Run the build"),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_USE, "echo hello-tui"),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_RESULT, "hello-tui"),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_RESULT, "notes.txt (2 edits)"),
            (
                FEED_ROLE_ASSISTANT,
                FEED_TYPE_TEXT,
                "Done — the build passed.",
            ),
        ];
        assert_eq!(msgs.len(), wants.len(), "rows: {msgs:?}");
        for (i, (role, typ, contains)) in wants.iter().enumerate() {
            let m = &msgs[i];
            let body = match &m.tool {
                Some(t) => format!("{} {}", t.name, t.detail),
                None => m.text.clone(),
            };
            assert_eq!((m.role.as_str(), m.typ.as_str()), (*role, *typ), "row {i}");
            assert!(body.contains(contains), "row {i} body = {body:?}");
        }
        // The tool rows name the tool, so a client can render them without
        // parsing the detail.
        assert_eq!(msgs[2].tool.as_ref().unwrap().name, "Shell");
        assert_eq!(msgs[4].tool.as_ref().unwrap().name, "Edit");
        // stop emits NO row.
        assert!(
            f.drain_messages().is_empty(),
            "stop must not emit a feed row"
        );
    }

    // Mirrors TestCursorFoldToolFailureAndOpenToolTracking
    // (watch_cursor_test.go:122).
    #[test]
    fn cursor_fold_tool_failure_and_open_tool_tracking() {
        let mut f = fold_events(&[
            hook_ev(
                "beforeSubmitPrompt",
                &format!(r#"{{{},"prompt":"read notes"}}"#, sid_field(SID)),
            ),
            hook_ev(
                "preToolUse",
                &format!(
                    r#"{{{},"tool_name":"Read","tool_input":{{"file_path":"/home/shed/notes.txt"}}}}"#,
                    sid_field(SID)
                ),
            ),
        ]);
        assert_eq!((f.open_tools(), f.activity()), (1, RcActivity::Working));
        f.apply_event(&hook_ev(
            "postToolUseFailure",
            &format!(
                r#"{{{},"tool_name":"Read","error_message":"File not found: /home/shed/notes.txt","failure_type":"error"}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(f.open_tools(), 0, "a failure ends the call like an output");
        let msgs = f.drain_messages();
        let last = msgs.last().expect("a failure row");
        assert_eq!(
            (last.role.as_str(), last.typ.as_str()),
            (FEED_ROLE_SYSTEM, FEED_TYPE_STATUS)
        );
        assert!(last.text.contains("tool Read failed") && last.text.contains("File not found"));

        // A tool still open when `stop` lands does not hold the session at
        // working: stop is the authoritative boundary and clears the count.
        f.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"Shell","tool_input":{{"command":"sleep 1"}}}}"#,
                sid_field(SID)
            ),
        ));
        f.apply_event(&hook_ev(
            "stop",
            &format!(r#"{{{},"status":"completed"}}"#, sid_field(SID)),
        ));
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "stop clears the open tool"
        );
    }

    // Mirrors TestCursorFoldOpenCallCounterPairing (watch_cursor_test.go:157):
    // every preToolUse is matched by exactly one postToolUse or
    // postToolUseFailure, while afterShellExecution/afterFileEdit ride
    // ALONGSIDE the pair rather than terminating it.
    #[test]
    fn cursor_fold_open_call_counter_pairing() {
        let mut f = CursorFold::new("");
        f.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"Shell","tool_input":{{"command":"make"}}}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(f.open_tools(), 1);
        f.apply_event(&hook_ev(
            "afterShellExecution",
            &format!(r#"{{{},"command":"make","output":"ok\n"}}"#, sid_field(SID)),
        ));
        assert_eq!(
            f.open_tools(),
            1,
            "afterShellExecution rides alongside, does not end it"
        );
        f.apply_event(&hook_ev(
            "postToolUse",
            &format!(
                r#"{{{},"tool_name":"Shell","tool_output":"ok"}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(f.open_tools(), 0, "postToolUse closes it");

        // A tool family with NO after* event at all (Read/Grep/Glob) — the
        // case that used to leak.
        f.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"Read","tool_input":{{"file_path":"/x"}}}}"#,
                sid_field(SID)
            ),
        ));
        f.apply_event(&hook_ev(
            "postToolUse",
            &format!(
                r#"{{{},"tool_name":"Read","tool_output":"…"}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(f.open_tools(), 0, "a Read call closes on its postToolUse");
        // postToolUse emits no feed row of its own.
        for m in f.drain_messages() {
            assert!(
                !(m.typ == FEED_TYPE_TOOL_RESULT
                    && m.tool.as_ref().is_some_and(|t| t.name == "Read")),
                "postToolUse must not emit a feed row: {m:?}"
            );
        }
        // It IS activity-relevant evidence: a fold that has only seen a tool
        // close is confirmed, not unknown.
        let f2 = fold_events(&[hook_ev(
            "postToolUse",
            &format!(r#"{{{},"tool_name":"Read"}}"#, sid_field(SID)),
        )]);
        assert!(
            f2.confirmed(),
            "postToolUse must confirm the session is alive"
        );
    }

    // Mirrors TestCursorFoldStopStatusRow (watch_cursor_test.go:208).
    #[test]
    fn cursor_fold_stop_status_row() {
        let cases: [(&str, Option<&str>); 4] = [
            ("completed", None),
            ("aborted", Some("turn ended: aborted")),
            ("error", Some("turn ended: error")),
            ("", None), // an absent status is not an outcome to report
        ];
        for (status, want_row) in cases {
            let mut f = fold_events(&[hook_ev(
                "stop",
                &format!(r#"{{{},"status":"{status}"}}"#, sid_field(SID)),
            )]);
            assert_eq!(f.activity(), RcActivity::NeedsInput, "every stop settles");
            let msgs = f.drain_messages();
            match want_row {
                None => assert!(msgs.is_empty(), "status {status:?}: rows = {msgs:?}"),
                Some(row) => {
                    assert_eq!(msgs.len(), 1, "status {status:?}");
                    assert_eq!(msgs[0].role, FEED_ROLE_SYSTEM);
                    assert_eq!(msgs[0].text, row);
                }
            }
        }
    }

    // Mirrors TestCursorFoldToleratesUnknownAndMalformed
    // (watch_cursor_test.go:240).
    #[test]
    fn cursor_fold_tolerates_unknown_and_malformed() {
        let mut f = CursorFold::new("");
        let cases = [
            hook_ev(
                "afterAgentThought",
                &format!(r#"{{{},"text":"thinking out loud"}}"#, sid_field(SID)),
            ), // not wired
            hook_ev("someFutureEvent", &format!(r#"{{{}}}"#, sid_field(SID))), // unknown
            hook_ev("beforeSubmitPrompt", r#"{not json"#),                     // malformed
            hook_ev(
                "beforeSubmitPrompt",
                &format!(r#"{{{},"prompt":"   "}}"#, sid_field(SID)),
            ), // empty prompt
            hook_ev(
                "afterAgentResponse",
                &format!(r#"{{{},"text":""}}"#, sid_field(SID)),
            ), // empty text
        ];
        for ev in &cases {
            f.apply_event(ev);
        }
        assert_eq!(
            f.activity(),
            RcActivity::Unknown,
            "nothing confirms a session"
        );
        assert!(f.drain_messages().is_empty(), "no feed rows expected");
        // Pinning runs BEFORE fold_event, so a tolerated event with a valid
        // session_id still pins.
        assert_eq!(f.pinned_id(), SID);
    }

    // Mirrors TestCursorFoldPinningAndRepin (watch_cursor_test.go:272).
    #[test]
    fn cursor_fold_pinning_and_repin() {
        let mut f = CursorFold::new("");
        // No sessionStart: the pin comes off the prompt event, exactly as a
        // --resume session would deliver it.
        f.apply_event(&hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{{},"prompt":"hello"}}"#, sid_field(SID)),
        ));
        assert_eq!(f.pinned_id(), SID);
        assert_eq!(f.drain_new_pin(), SID, "surfaced for the back-write");
        assert_eq!(f.drain_new_pin(), "", "cleared on the second drain");

        // A hostile id is refused outright — never the pin, never a back-write.
        let long = "a".repeat(300);
        for bad in ["../../etc/passwd", "not-a-uuid", long.as_str()] {
            f.apply_event(&hook_ev(
                "afterAgentResponse",
                &format!(
                    r#"{{"session_id":{},"text":"x"}}"#,
                    serde_json::to_string(bad).unwrap()
                ),
            ));
            assert_eq!(f.pinned_id(), SID, "malformed id {bad:?} must not re-pin");
            assert_eq!(f.drain_new_pin(), "", "malformed id {bad:?} never surfaces");
        }

        // A tool is open and a message is remembered — both belong to the OLD
        // chat.
        f.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"Shell","tool_input":{{"command":"sleep 30"}}}}"#,
                sid_field(SID)
            ),
        ));
        f.apply_event(&hook_ev(
            "afterAgentResponse",
            &format!(r#"{{{},"text":"old chat message"}}"#, sid_field(SID)),
        ));
        f.drain_messages();

        // The operator switched chats in the TUI: a different valid id re-pins.
        f.apply_event(&hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{{},"prompt":"new chat"}}"#, sid_field(OTHER_SID)),
        ));
        assert_eq!(f.pinned_id(), OTHER_SID);
        assert_eq!(f.drain_new_pin(), OTHER_SID);
        assert_eq!(f.open_tools(), 0, "old chat's open tools dropped");
        assert_ne!(
            f.raw_last_msg(),
            "old chat message",
            "old chat's message dropped"
        );
        let msgs = f.drain_messages();
        assert!(
            msgs.first()
                .is_some_and(|m| m.text.contains("switched to another chat")),
            "the switch must be announced: {msgs:?}"
        );
    }

    // Mirrors TestCursorFoldRepinResetsState (watch_cursor_test.go:327): a
    // repin via an event that does NOT itself set working (postToolUse) must
    // not leave activity() reading the OLD chat's working state forever.
    #[test]
    fn cursor_fold_repin_resets_state() {
        let mut f = CursorFold::new("");
        f.apply_event(&hook_ev(
            "beforeSubmitPrompt",
            &format!(r#"{{{},"prompt":"go"}}"#, sid_field(SID)),
        ));
        f.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"Shell","tool_input":{{"command":"sleep 30"}}}}"#,
                sid_field(SID)
            ),
        ));
        assert_eq!(f.activity(), RcActivity::Working, "setup: mid-turn");

        f.apply_event(&hook_ev(
            "postToolUse",
            &format!(r#"{{{},"tool_name":"Shell"}}"#, sid_field(OTHER_SID)),
        ));
        assert_eq!(f.pinned_id(), OTHER_SID);
        assert_ne!(
            f.activity(),
            RcActivity::Working,
            "must not stay stuck working after a repin via a non-working event"
        );
    }

    // Null-tolerance pins (H5 review, HIGH): a stop with `"status":null` must
    // still settle AND pin (Go's json.Unmarshal no-ops on null; a rejected
    // event would leave the lane dark and never back-write the session id),
    // and a `tool_input:null` renders the detail "null", exactly like Go's
    // compactJSON over a raw `null`.
    #[test]
    fn cursor_fold_tolerates_null_fields() {
        let mut f = CursorFold::new("");
        assert!(f.apply_event(&hook_ev(
            "stop",
            &format!(r#"{{{},"status":null}}"#, sid_field(SID)),
        )));
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "null status still settles"
        );
        assert_eq!(f.pinned_id(), SID, "null elsewhere must not block the pin");

        let mut f2 = CursorFold::new("");
        f2.apply_event(&hook_ev(
            "preToolUse",
            &format!(
                r#"{{{},"tool_name":"X","tool_input":null}}"#,
                sid_field(SID)
            ),
        ));
        let msgs = f2.drain_messages();
        assert_eq!(msgs.len(), 1);
        assert_eq!(
            msgs[0].tool.as_ref().unwrap().detail,
            "null",
            "a null tool_input compacts to the literal, like Go"
        );
        // A null edits list is Go's nil slice: path-only detail.
        let mut f3 = CursorFold::new("");
        f3.apply_event(&hook_ev(
            "afterFileEdit",
            &format!(
                r#"{{{},"file_path":"/tmp/x","edits":null}}"#,
                sid_field(SID)
            ),
        ));
        let msgs = f3.drain_messages();
        assert_eq!(msgs[0].tool.as_ref().unwrap().detail, "/tmp/x");
    }

    // RES-2/RES-3 pins (H5 review re-run): a literal `null` payload folds
    // with a zero payload (Go's Unmarshal no-ops), while any non-object
    // payload — notably the bare-array form that could smuggle a session id
    // through the loopback ingest route — is rejected like Go.
    #[test]
    fn cursor_fold_null_and_seq_payloads() {
        let mut f = CursorFold::new("");
        assert!(
            f.apply_event(&hook_ev("stop", "null")),
            "a literal null payload still folds"
        );
        assert_eq!(f.activity(), RcActivity::NeedsInput);

        let mut f2 = CursorFold::new("");
        assert!(f2.apply_event(&hook_ev("sessionStart", "null")));
        let msgs = f2.drain_messages();
        assert!(msgs[0].text.contains("cursor session started"));

        // The bare-array form must NOT fold — and must NOT pin the id.
        let mut f3 = CursorFold::new("");
        assert!(!f3.apply_event(&hook_ev("stop", &format!(r#"["{SID}"]"#))));
        assert_eq!(
            f3.pinned_id(),
            "",
            "a seq-form payload must not adopt an id"
        );
        assert_eq!(f3.activity(), RcActivity::Unknown);
        // Trailing garbage after null is invalid JSON in Go too.
        assert!(!f3.apply_event(&hook_ev("stop", "nullX")));

        // Transcript seq forms yield no rows.
        assert!(parse_cursor_transcript_line(br#"["user","x",{"content":[]}]"#).is_empty());
        assert!(
            parse_cursor_transcript_line(br#"{"role":"user","message":["u",{"content":[]}]}"#)
                .is_empty()
        );
        assert!(parse_cursor_transcript_line(
            br#"{"role":"user","message":{"content":[["text","x"]]}}"#
        )
        .is_empty());
        // A seq-form tool input falls back to the compact literal, like Go.
        let rows = parse_cursor_transcript_line(
            br#"{"role":"assistant","message":{"content":[{"type":"tool_use","name":"X","input":["x"]}]}}"#,
        );
        assert_eq!(rows[0].tool.as_ref().unwrap().detail, r#"["x"]"#);
    }

    // ---- transcript backfill ----

    // Mirrors TestCursorSlugForWorkdir (watch_cursor_transcript_test.go:23).
    #[test]
    fn cursor_slug_for_workdir_cases() {
        let cases = [
            ("/home/shed", "home-shed"),
            ("/home/shed/myproj", "home-shed-myproj"),
            ("/", ""),
            ("", ""),
        ];
        for (workdir, want) in cases {
            assert_eq!(
                cursor_slug_for_workdir(workdir),
                want,
                "workdir {workdir:?}"
            );
        }
    }

    // Mirrors TestCursorTranscriptPath (watch_cursor_transcript_test.go:41).
    #[test]
    fn cursor_transcript_path_cases() {
        assert_eq!(
            cursor_transcript_path("/home/shed", "/home/shed/proj", SID),
            format!(
                "/home/shed/.cursor/projects/home-shed-proj/agent-transcripts/{SID}/{SID}.jsonl"
            )
        );
        assert_eq!(cursor_transcript_path("", "/home/shed", SID), "", "no home");
        assert_eq!(
            cursor_transcript_path("/home/shed", "", SID),
            "",
            "no workdir"
        );
        assert_eq!(
            cursor_transcript_path("/home/shed", "/home/shed", "not-a-uuid"),
            "",
            "malformed session id"
        );
        assert_eq!(
            cursor_transcript_path("/home/shed", "/home/shed", ""),
            "",
            "empty id"
        );
    }

    // Mirrors TestParseCursorTranscriptLine (watch_cursor_transcript_test.go:68).
    #[test]
    fn parse_cursor_transcript_line_cases() {
        // user text
        let rows = parse_cursor_transcript_line(
            br#"{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}"#,
        );
        assert_eq!(rows.len(), 1);
        assert_eq!(
            (
                rows[0].role.as_str(),
                rows[0].typ.as_str(),
                rows[0].text.as_str()
            ),
            (FEED_ROLE_USER, FEED_TYPE_TEXT, "hello")
        );

        // assistant text and tool_use blocks
        let rows = parse_cursor_transcript_line(
            br#"{"role":"assistant","message":{"content":[{"type":"text","text":"doing it"},{"type":"tool_use","name":"Shell","input":{"command":"echo hi"}}]}}"#,
        );
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].text, "doing it");
        let tool = rows[1].tool.as_ref().expect("tool block");
        assert_eq!(
            (tool.name.as_str(), tool.detail.as_str()),
            ("Shell", "echo hi")
        );

        // tool_use with path input falls back to path
        let rows = parse_cursor_transcript_line(
            br#"{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"path":"/tmp/notes.txt","contents":"hi\n"}}]}}"#,
        );
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].tool.as_ref().unwrap().detail, "/tmp/notes.txt");

        // turn_ended, malformed, whitespace-only text, no message field,
        // unknown block type: all yield no rows.
        for raw in [
            br#"{"type":"turn_ended","status":"completed"}"#.as_slice(),
            br#"{"role":"user","message":"#.as_slice(),
            br#"{"role":"user","message":{"content":[{"type":"text","text":"   "}]}}"#.as_slice(),
            br#"{"role":"user"}"#.as_slice(),
            br#"{"role":"assistant","message":{"content":[{"type":"tool_result","text":"output"}]}}"#
                .as_slice(),
        ] {
            assert!(
                parse_cursor_transcript_line(raw).is_empty(),
                "line {:?} must yield no rows",
                String::from_utf8_lossy(raw)
            );
        }
    }

    // Mirrors TestReadCursorTranscriptFixture
    // (watch_cursor_transcript_test.go:145) against the SHARED fixture copy.
    #[test]
    fn read_cursor_transcript_fixture() {
        let path = format!(
            "{}/../fixtures/jsonl/cursor_transcript.jsonl",
            env!("CARGO_MANIFEST_DIR")
        );
        let rows = read_cursor_transcript(&path);
        // The fixture is: user, assistant(text+tool_use), assistant(text),
        // user, turn_ended (ignored) — five rows in stream order.
        assert_eq!(rows.len(), 5, "rows: {rows:?}");
        let want = [
            (FEED_ROLE_USER, FEED_TYPE_TEXT),
            (FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT),
            (FEED_ROLE_TOOL, FEED_TYPE_TOOL_USE),
            (FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT),
            (FEED_ROLE_USER, FEED_TYPE_TEXT),
        ];
        for (i, (role, typ)) in want.iter().enumerate() {
            assert_eq!(
                (rows[i].role.as_str(), rows[i].typ.as_str()),
                (*role, *typ),
                "row {i}"
            );
        }
        let tool = rows[2].tool.as_ref().expect("tool row");
        assert_eq!(
            (tool.name.as_str(), tool.detail.as_str()),
            ("Shell", "make build")
        );
        assert_eq!(rows[4].text, "Thanks");
    }

    #[test]
    fn read_cursor_transcript_missing_file() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("does-not-exist.jsonl");
        assert!(read_cursor_transcript(path.to_str().unwrap()).is_empty());
    }

    // Mirrors TestReadCursorTranscriptPartialLastLine
    // (watch_cursor_transcript_test.go:190): a mid-write restart must not lose
    // the earlier, complete lines.
    #[test]
    fn read_cursor_transcript_partial_last_line() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        let content = concat!(
            r#"{"role":"user","message":{"content":[{"type":"text","text":"complete line"}]}}"#,
            "\n",
            r#"{"role":"assistant","message":{"content":[{"type":"text","text":"cut off mid-w"#,
        );
        std::fs::write(&path, content).expect("write");
        let rows = read_cursor_transcript(path.to_str().unwrap());
        assert_eq!(rows.len(), 1, "rows: {rows:?}");
        assert_eq!(rows[0].text, "complete line");
    }

    // Mirrors TestReadCursorTranscriptUnreadableDirNoPanic
    // (watch_cursor_transcript_test.go:204).
    #[test]
    fn read_cursor_transcript_dir_path_yields_nothing() {
        let dir = tempfile::tempdir().expect("tempdir");
        assert!(read_cursor_transcript(dir.path().to_str().unwrap()).is_empty());
    }

    // Mirrors TestReadCursorTranscriptLineCap
    // (watch_cursor_transcript_test.go:215): only the LAST
    // MAX_CURSOR_TRANSCRIPT_LINES lines survive.
    #[test]
    fn read_cursor_transcript_line_cap() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        let total = MAX_CURSOR_TRANSCRIPT_LINES + 50;
        let mut content = String::new();
        for i in 0..total {
            content.push_str(&format!(
                r#"{{"role":"user","message":{{"content":[{{"type":"text","text":"line-{i}"}}]}}}}"#
            ));
            content.push('\n');
        }
        std::fs::write(&path, content).expect("write");
        let rows = read_cursor_transcript(path.to_str().unwrap());
        assert_eq!(rows.len(), MAX_CURSOR_TRANSCRIPT_LINES);
        assert_eq!(
            rows[0].text,
            format!("line-{}", total - MAX_CURSOR_TRANSCRIPT_LINES),
            "oldest lines dropped"
        );
        assert_eq!(rows.last().unwrap().text, format!("line-{}", total - 1));
    }

    // Mirrors TestReadCursorTranscriptBoundedTail
    // (watch_cursor_transcript_test.go:258): the read is bounded to the
    // file's tail, and the seek's headless first line must not leak a
    // spurious row.
    #[test]
    fn read_cursor_transcript_bounded_tail() {
        let line = |label: &str| -> String {
            format!(
                r#"{{"role":"user","message":{{"content":[{{"type":"text","text":"{label}"}}]}}}}"#
            ) + "\n"
        };
        const OLD_COUNT: usize = 50;
        const NEW_COUNT: usize = 10;
        let old_lines: Vec<String> = (0..OLD_COUNT)
            .map(|i| line(&format!("OLD-{i:03}")))
            .collect();
        let new_lines: Vec<String> = (0..NEW_COUNT)
            .map(|i| line(&format!("NEW-{i:03}")))
            .collect();
        // Every line is the same fixed length so the byte math is exact.
        let line_len = old_lines[0].len();
        for l in old_lines.iter().chain(new_lines.iter()) {
            assert_eq!(l.len(), line_len, "fixture lines must be fixed-length");
        }

        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("t.jsonl");
        let mut f = std::fs::File::create(&path).expect("create");
        for l in old_lines.iter().chain(new_lines.iter()) {
            f.write_all(l.as_bytes()).expect("write");
        }
        drop(f);

        // The bound covers exactly the "new" lines plus half of the last "old"
        // line, so the seek lands mid-line in that last old line — it must be
        // dropped, not misread as a row — and every "new" line is fully intact.
        let tail_bytes = (NEW_COUNT * line_len + line_len / 2) as u64;
        let rows = read_cursor_transcript_bounded(path.to_str().unwrap(), tail_bytes);
        assert_eq!(
            rows.len(),
            NEW_COUNT,
            "only the lines inside the tail bound"
        );
        for (i, r) in rows.iter().enumerate() {
            assert_eq!(r.text, format!("NEW-{i:03}"), "row {i}");
        }
    }
}
