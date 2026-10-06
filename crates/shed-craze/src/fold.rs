//! The **fold** (plan 025 §3.3.6, P13): craze's events and snapshots in,
//! append-only transcript rows, an activity verdict, the approval book and the
//! settings out.
//!
//! No I/O, no clock, no transport — everything it knows is what it was handed,
//! which is what lets the same code fold an attach's snapshot, a cursor
//! replay, a live event, `history`'s `session.snapshot` and the committed live
//! recording and get the same rows. The watcher (`crate::watcher`) owns the
//! ring that numbers the rows, and the flush clock.
//!
//! # Rows are append-only
//!
//! craze's own fold (`internal/transcript/fold.go` at the pin) UPSERTS: its
//! open stream entry grows, and a tool row is replaced by its `tool.id`. The
//! contract's rows are append-only, so this fold SEGMENTS ([`crate::segment`],
//! ported from shed-gx's) and draws one `tool_use` per tool id and one
//! `tool_result` at its first terminal status. Every other rule is craze's own,
//! ported from the pin with its wordings — `noteTodos`, `compactionNote`,
//! `noteForForeignTurn`, the answered-ask notes (`internal/transcript/notes.go`)
//! and the shell-context/attachment strip (`internal/agent/shellcontext.go`,
//! `attachments.go`).
//!
//! # The event → rows table (§3.3.6), as built
//!
//! Only the MAIN transcript draws rows: an event with `agent` set (a
//! sub-agent's) draws none — its asks and their endings included, exactly as
//! craze's own fold (`internal/transcript/fold.go`'s table at the pin, where
//! all four ask kinds are `childIgnored`).
//!
//! **Rows follow craze's transcript; approvals follow craze's ask registry**
//! (Amendment A11). The ENGINE's registry (`asks.list`) has no agent field: a
//! sub-agent's ask is registered, answered through `asks.answer` and counted
//! in the row's `pendingAsks` like any other, though no snapshot carries it.
//! So the book holds every non-automatic open ask whose card the session's
//! capabilities expose — craze's own `HiddenBy`: `askCards` for a question,
//! `planCards` for a plan, a permission always ([`Cards`]) — whatever its
//! `agent`: a sub-agent's
//! opening opens its approval and its ending resolves it, drawing nothing. A
//! hidden kind is never an approval and draws no card row (a client hides
//! what a capability says the session cannot do; craze's own clients answer
//! it unseen). `detail` is the body's, for every ask: neither the registry
//! nor `asks.get` carries the agent, so a seed could never recover a name.
//!
//! **A restore books no approvals**: a snapshot's `asks` are the transcript's
//! open set (main asks only), so they draw their pending rows and are held
//! for their rows' sake, but the approval MEMBERSHIP is the registry's —
//! [`CrazeFold::adopt_registry`], which every seed and every resume runs over
//! `asks.list` + `asks.get` (the watcher's fenced read, `crate::watcher`).
//!
//! | event | rows |
//! |---|---|
//! | `turn` `started` | flush; `user/text`, craze's leading `<craze_attachments>`/`<shell_context>` blocks stripped ([`split_shell_context`]) |
//! | `user` | `interjection`: flush, `user/text`; inside a replay (`replay start…end`, or `replayed`): `user/text`; a live echo: nothing |
//! | `text` / `thought` | the open streak of `assistant/text` / `assistant/reasoning` |
//! | `tool` | flush; first sight of an id: `tool/tool_use {name: toolName, else title, else name; detail: title}`; first terminal status (`completed`, `failed`, `cancelled`): one `tool/tool_result`; updates and repeats absorbed; the todo writer ignored |
//! | `todos` | `noteTodos`: `tasks: N planned` when the most grows, `tasks: c/n done` once all close |
//! | `permission`, non-`auto` `question`/`plan` | flush; the book opens a [`LaneApproval`] and one `tool/approval_request` row (pending) |
//! | non-`auto` main `plan` | first, the plan entry's `system/status` row (`plan <name>`, its overview on the next line) — the row a restore draws from the snapshot's `plan` entry, so live and restored read the same (Amendment A10) |
//! | `ask` (ending) | the approval resolved, and the second `approval_request` row (resolved); an answered question or plan also its `system/status` outcome note |
//! | `done` | flush; `stopReason` `cancelled`: `system/status` "cancelled" |
//! | `error`, `meta.detail`, `meta.indexErr`, a synthetic `turn` `ended` with `err` | `system/status`, the error's text |
//! | `foreign_turn` running | flush; `system/status`, craze's wording for its reason |
//! | `foreign_turn` end (`running` absent) | no row (A7) |
//! | `replay` `end` | flush; `system/status` "restored" |
//! | `compaction` | `started`: flush, and the session row's "compacting context…" state; `ended`: `compactionNote` |
//! | `command` | `system/status` `⤷ <qualified> (<kind>)` |
//! | `meta` settings sections | no row; the settings ([`crate::settings`]) |
//! | `turn` `ended`, `queue`, `subagent` | no row |
//! | a kind this build does not know | ignored |
//!
//! **Activity** (fold-owned): `turn` started, `foreign_turn` running and a main
//! `text`/`thought` chunk → `Working`; `done`, `turn` ended and `foreign_turn`
//! END → `Idle` (Amendment A7: a foreign turn has no `done` and no `turn
//! ended`, so without its end a session would sit at `Working` after every
//! wake); any pending approval → `NeedsApproval`, the override (correction 6).
//! The verdict is `None` until an event says something, so a session row read
//! from the host stands until the stream moves.
//!
//! # Snapshot → rows
//!
//! Entries oldest-first: `user` → `user/text`, `assistant` → `assistant/text`
//! (split at 8 KiB), `thought` → `assistant/reasoning` (likewise), `tool` →
//! `tool_use` (+ `tool_result` when terminal, `cancelled` included), `note` and
//! `plan` → `system/status`, `error` → `system/status`. Continuation follows
//! craze's restore (`internal/transcript/snapshot.go`'s `restore`):
//! `main.streamOpen` is transcript-level, and the open run is the final entry
//! when it is `streaming` — it becomes the open streak, so the next chunk
//! continues it — or is absent when the window dropped it, `omittedRun` naming
//! its kind: chunks of that kind then draw nothing until the run ends. The
//! omitted-entry ledger draws one `system/status` "earlier transcript omitted"
//! row at the top when non-empty. The open asks are booked (an `approval_request`
//! row each, after the entries), automatic ones never.
//!
//! # Bounds
//!
//! The tool map ([`MAX_TOOLS`]) never evicts a call still in flight; the book
//! ([`MAX_APPROVALS`]) never evicts a pending approval — gx's two bounds, and
//! gx's residuals: a tool replayed after more than [`MAX_TOOLS`] newer ones
//! could draw a second result row, which the next reseed heals.

use std::collections::{BTreeMap, HashMap, VecDeque};

use serde::Deserialize;
use serde_json::{Map, Value};
use shed_core::lane::feed::{
    sanitize_feed_text, truncate_bytes, APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS,
    APPROVAL_DECISION_DENY, APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED, FEED_ROLE_ASSISTANT,
    FEED_ROLE_SYSTEM, FEED_ROLE_TOOL, FEED_ROLE_USER, FEED_TYPE_APPROVAL_REQUEST,
    FEED_TYPE_REASONING, FEED_TYPE_STATUS, FEED_TYPE_TEXT, FEED_TYPE_TOOL_RESULT,
    FEED_TYPE_TOOL_USE,
};
use shed_core::lane::{
    option_kind, LaneApproval, LaneApprovalKind, LaneApprovalOption, LaneApprovalStatus,
    LaneQuestion,
};
use shed_core::rc::{RcActivity, RcFeedApproval, RcFeedMessage, RcFeedTool};

use crate::segment::{split_rows, Segmenter};
use crate::settings::{SettingsSections, SettingsState};
use crate::wire::{AskRecord, SessionCapabilities, SessionInfo};

/// How many tool states the fold remembers (gx's bound). Eviction takes only
/// tools that drew their result row.
pub const MAX_TOOLS: usize = 512;
/// How many approvals the book remembers (gx's bound). A pending one is never
/// evicted.
pub const MAX_APPROVALS: usize = 256;
/// A completed tool's result row carries this much of its output: craze's own
/// `outputHeadCap` (`internal/agent/tools.go`), which is what `stdoutHead`
/// already is, applied to `contentText` too.
pub const RESULT_HEAD_BYTES: usize = 512;
/// An outcome note's whole text, in bytes: craze's `outcomeNoteCap`.
pub const OUTCOME_NOTE_CAP: usize = 4 << 10;

/// craze's wordings (`internal/transcript/notes.go` at the pin), so every
/// client folding the same event writes the same row.
pub mod wording {
    /// A cancelled turn's row (`NoteCancelled`).
    pub const CANCELLED: &str = "cancelled";
    /// A foreign turn with no reason, or one this build does not know
    /// (`NoteForeignTurn`).
    pub const FOREIGN_TURN: &str = "agent continued on its own (interjection fallback)";
    /// `subagent_wake` (`NoteSubagentWake`).
    pub const SUBAGENT_WAKE: &str = "sub-agent finished — the agent continues";
    /// `job_wake` (`NoteJobWake`).
    pub const JOB_WAKE: &str = "background command finished — the agent continues";
    /// A replay's end (`NoteRestored`).
    pub const RESTORED: &str = "restored";
    /// What leads a command line (`CommandLineMark`).
    pub const COMMAND_MARK: &str = "⤷ ";
    /// The session row's working label while a compaction runs
    /// (`CompactingLabel`).
    pub const COMPACTING: &str = "compacting context…";
    pub const COMPACTED: &str = "context compacted";
    pub const COMPACTED_ON_REQUEST: &str = "context compacted on request";
    pub const COMPACTED_OVERFLOW: &str = "context was too large — compacted";
    pub const COMPACTION_FAILED: &str = "compaction failed";
    /// The row above a snapshot whose window omitted entries (plan 025
    /// §3.3.6).
    pub const OMITTED: &str = "earlier transcript omitted";
}

/// The three reasons a foreign turn names (an open string on the wire).
pub mod foreign_reason {
    pub const SUBAGENT_WAKE: &str = "subagent_wake";
    pub const JOB_WAKE: &str = "job_wake";
}

// ---------------------------------------------------------------------------
// the event codec, read tolerantly
// ---------------------------------------------------------------------------

/// A member of `obj`, decoded — or `None` when it is absent, `null`, or of a
/// type this build cannot read. Per member, so one odd member costs only
/// itself (PM "Tolerant inbound").
fn member<T: for<'de> Deserialize<'de>>(obj: &Map<String, Value>, key: &str) -> Option<T> {
    obj.get(key)
        .filter(|v| !v.is_null())
        .and_then(|v| T::deserialize(v).ok())
}

/// A string member, borrowed — `""` when it is absent or not a string.
fn str_of<'a>(obj: &'a Map<String, Value>, key: &str) -> &'a str {
    obj.get(key).and_then(Value::as_str).unwrap_or("")
}

/// A string member, owned — [`str_of`]'s rule.
fn text_of(obj: &Map<String, Value>, key: &str) -> String {
    str_of(obj, key).to_string()
}

/// An array member, borrowed — empty when it is absent or not an array.
fn array_of<'a>(obj: &'a Map<String, Value>, key: &str) -> &'a [Value] {
    obj.get(key)
        .and_then(Value::as_array)
        .map_or(&[], Vec::as_slice)
}

/// A tool call's state (the event codec's `tool`).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Tool {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub tool_name: String,
    #[serde(default)]
    pub content_text: String,
    #[serde(default)]
    pub output: Option<ToolOutput>,
    #[serde(default)]
    pub at: Option<String>,
}

impl Tool {
    /// craze's `IsTodoTool`: the todo writer, which the fold drops before it
    /// can close the run it fires in the middle of.
    pub fn is_todo_tool(&self) -> bool {
        self.tool_name == "updateTodos" || self.title.starts_with("Update TODOs")
    }

    /// The name a row shows: `toolName` (the stable identity), else the
    /// title, else the codec's own `name`.
    fn display_name(&self) -> Option<String> {
        [&self.tool_name, &self.title, &self.name]
            .into_iter()
            .find(|s| !s.trim().is_empty())
            .cloned()
    }

    fn terminal(&self) -> bool {
        matches!(self.status.as_str(), "completed" | "failed" | "cancelled")
    }
}

/// A tool's output (only the head this fold shows).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ToolOutput {
    #[serde(default)]
    pub stdout_head: String,
}

/// One todo.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct Todo {
    #[serde(default)]
    pub status: String,
}

/// A permission ask's opening.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct Permission {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub tool: String,
    #[serde(default)]
    pub options: Vec<PermissionOption>,
}

/// One offered option, verbatim (duplicate kinds included).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PermissionOption {
    #[serde(default)]
    pub option_id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub kind: String,
}

/// A question ask's opening.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct Question {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub questions: Vec<QuestionItem>,
    #[serde(default)]
    pub auto: bool,
}

/// One question of a question ask.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct QuestionItem {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub prompt: String,
    #[serde(default)]
    pub options: Vec<QuestionOption>,
    #[serde(default)]
    pub allow_multiple: bool,
}

/// One option of a question.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct QuestionOption {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub label: String,
    #[serde(default)]
    pub description: String,
}

/// A plan ask's opening.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct Plan {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub overview: String,
    #[serde(default)]
    pub auto: bool,
}

/// A foreign turn's bracket (Amendment A7): nested under `foreignTurn`,
/// `running: true` on its start, `running` ABSENT on its end — absent decodes
/// `false`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct ForeignTurn {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub running: bool,
}

/// One end of an engine-driven turn.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Turn {
    #[serde(default)]
    pub phase: String,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub err: String,
    #[serde(default)]
    pub synthetic: bool,
}

/// One ask's ending.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AskUpdate {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub kind: String,
    /// `answered`, `cancelled`, `turn_ended`, `closing`, `automatic`.
    #[serde(default)]
    pub outcome: String,
    #[serde(default)]
    pub option_id: String,
    /// Each question's id to the option ids chosen (a `null` list is not an
    /// empty one, and reads as none).
    #[serde(default)]
    pub answers: BTreeMap<String, Option<Vec<String>>>,
    #[serde(default)]
    pub skip: bool,
    #[serde(default)]
    pub accepted: bool,
}

/// One end of a compaction.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Compaction {
    #[serde(default)]
    pub phase: String,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub tokens_before: i64,
    #[serde(default)]
    pub tokens_after: i64,
    #[serde(default)]
    pub err: String,
}

/// The ask kinds.
mod ask_kind {
    pub const PERMISSION: &str = "permission";
    pub const QUESTION: &str = "question";
    pub const PLAN: &str = "plan";
}

/// The synthesized plan options (§3.3.6): their ids ARE the answers.
pub mod plan_option {
    pub const ACCEPT: &str = "accept";
    pub const REJECT: &str = "reject";
}

/// An ask's opening, as booked.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Opening {
    Permission(Permission),
    Question(Question),
    Plan(Plan),
}

impl Opening {
    /// Read the `kind` payload of an ask body (`{permission|question|plan:
    /// …}`: a snapshot's `asks[].body`, `asks.get`'s, or an opening event,
    /// which carries its payload the same way) — the opening, and the payload
    /// itself, borrowed. `None` when it does not read — or when it is
    /// automatic, which is never an approval.
    pub fn from_body<'a>(kind: &str, body: &'a Value) -> Option<(Opening, &'a Value)> {
        let payload = body.get(kind).filter(|v| v.is_object())?;
        let opening = match kind {
            ask_kind::PERMISSION => Opening::Permission(Permission::deserialize(payload).ok()?),
            ask_kind::QUESTION => {
                let q = Question::deserialize(payload).ok()?;
                if q.auto {
                    return None;
                }
                Opening::Question(q)
            }
            ask_kind::PLAN => {
                let p = Plan::deserialize(payload).ok()?;
                if p.auto {
                    return None;
                }
                Opening::Plan(p)
            }
            _ => return None,
        };
        Some((opening, payload))
    }

    fn id(&self) -> &str {
        match self {
            Opening::Permission(p) => &p.id,
            Opening::Question(q) => &q.id,
            Opening::Plan(p) => &p.id,
        }
    }

    fn kind_word(&self) -> &'static str {
        match self {
            Opening::Permission(_) => ask_kind::PERMISSION,
            Opening::Question(_) => ask_kind::QUESTION,
            Opening::Plan(_) => ask_kind::PLAN,
        }
    }
}

// ---------------------------------------------------------------------------
// the fold's state
// ---------------------------------------------------------------------------

/// What the fold remembers about a tool call between its first sight and its
/// terminal status.
#[derive(Debug, Clone, Default)]
struct ToolState {
    name: Option<String>,
    detail: Option<String>,
    /// The result row is out: repeats and later updates are absorbed.
    terminal_emitted: bool,
}

/// Which ask cards the session's capabilities expose — exactly craze's own
/// `transcript.HiddenBy` (`internal/transcript/model.go`): a QUESTION card
/// needs `askCards`, a PLAN card `planCards`, and a PERMISSION is never
/// hidden — an agent blocked on one would be stranded. Both shown by default
/// (craze's zero value hides nothing, and every shipped provider shows both)
/// until an info document says otherwise ([`CrazeFold::apply_info`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Cards {
    pub questions: bool,
    pub plans: bool,
}

impl Default for Cards {
    fn default() -> Cards {
        Cards {
            questions: true,
            plans: true,
        }
    }
}

impl Cards {
    /// The cards an info document's capabilities expose.
    pub fn of(caps: &SessionCapabilities) -> Cards {
        Cards {
            questions: caps.ask_cards,
            plans: caps.plan_cards,
        }
    }

    /// Whether an ask of `opening`'s kind is shown — and so can be an
    /// approval at all.
    pub fn expose(&self, opening: &Opening) -> bool {
        match opening {
            Opening::Permission(_) => true,
            Opening::Question(_) => self.questions,
            Opening::Plan(_) => self.plans,
        }
    }
}

/// One booked ask.
#[derive(Debug, Clone)]
struct Booked {
    approval: LaneApproval,
    opening: Opening,
    /// Its pending `approval_request` row was drawn: it is in craze's
    /// TRANSCRIPT (a main opening, or a snapshot's open ask), so its ending
    /// draws the resolved row and the outcome note.
    rowed: bool,
    /// It is an approval: the REGISTRY holds it (a live opening, or
    /// [`CrazeFold::adopt_registry`]). A snapshot ask the registry no longer
    /// holds is kept for its rows only.
    member: bool,
}

/// Where an opening came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Source {
    /// An event: `main` when no `agent` is set.
    Live { main: bool },
    /// A snapshot's open asks: transcript rows, no membership yet.
    Snapshot,
}

/// The kinds a main-transcript stream can be: the segmenter's role and type.
type StreamKind = (&'static str, &'static str);

const ASSISTANT_TEXT: StreamKind = (FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT);
const ASSISTANT_REASONING: StreamKind = (FEED_ROLE_ASSISTANT, FEED_TYPE_REASONING);

/// The stream a snapshot's run kind is (`main.omittedRun`, an open entry's
/// `kind`): only the agent's text and thinking stream on the main transcript.
fn run_kind(kind: &str) -> Option<StreamKind> {
    match kind {
        "assistant" => Some(ASSISTANT_TEXT),
        "thought" => Some(ASSISTANT_REASONING),
        _ => None,
    }
}

/// What a [`CrazeFold::restore`] found beyond the rows.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Restored {
    /// The snapshot's cut: its incarnation and seq.
    pub incarnation: String,
    pub seq: u64,
    /// The window did not hold the whole transcript: `windowed`, `dropped`, or
    /// an omitted-entry ledger (`history`'s `truncated`).
    pub truncated: bool,
    /// Open asks whose body a snapshot string cut. The approvals never use
    /// that cut body: the fenced registry read takes every open ask whole
    /// from `asks.get` ([`CrazeFold::adopt_registry`]).
    pub truncated_asks: Vec<String>,
}

/// The craze fold. Feed it a snapshot ([`CrazeFold::restore`]) and events
/// ([`CrazeFold::apply`]); take rows out with [`CrazeFold::drain`].
#[derive(Debug)]
pub struct CrazeFold {
    /// The lane's row id (the hostId): every approval's `session_id`.
    session_id: String,
    seg: Segmenter,
    out: Vec<RcFeedMessage>,
    tools: HashMap<String, ToolState>,
    tool_order: VecDeque<String>,
    book: BTreeMap<String, Booked>,
    book_order: VecDeque<String>,
    todo_planned: usize,
    todo_done: bool,
    replaying: bool,
    /// A restored window dropped the open run's entry: chunks of this kind
    /// draw nothing until the run ends.
    omitted_run: Option<StreamKind>,
    compacting: bool,
    activity: Option<RcActivity>,
    settings: SettingsState,
    settings_changed: bool,
    /// The book changed — an ask opened, ended, read whole or evicted — since
    /// [`CrazeFold::take_approvals_changed`].
    approvals_changed: bool,
    /// The cards the session exposes.
    cards: Cards,
}

impl CrazeFold {
    /// A fresh fold for the lane whose row id is `session_id` (the hostId).
    pub fn new(session_id: &str) -> CrazeFold {
        CrazeFold {
            session_id: session_id.to_string(),
            seg: Segmenter::new(),
            out: Vec::new(),
            tools: HashMap::new(),
            tool_order: VecDeque::new(),
            book: BTreeMap::new(),
            book_order: VecDeque::new(),
            todo_planned: 0,
            todo_done: false,
            replaying: false,
            omitted_run: None,
            compacting: false,
            activity: None,
            settings: SettingsState::new(),
            settings_changed: false,
            approvals_changed: false,
            cards: Cards::default(),
        }
    }

    // ---- output ----

    /// Take the rows folded so far. The open streak is deliberately NOT
    /// flushed — it is still growing; [`CrazeFold::flush`] says when.
    pub fn drain(&mut self) -> Vec<RcFeedMessage> {
        std::mem::take(&mut self.out)
    }

    /// Keep only the newest `max` rows waiting to be drained, and say so: a
    /// cut drops the oldest and leads with one `system/status` "earlier
    /// transcript omitted" row (the snapshot ledger's own wording, which a cut
    /// window already led with). A seed is one synchronous burst, and one
    /// bigger than the client channel would lag at the same frame on every
    /// reseed, forever (the contract's correction 13) — so the watcher caps a
    /// seed's rows at what the ring keeps anyway.
    pub fn cap_pending(&mut self, max: usize) {
        if self.out.len() <= max {
            return;
        }
        let ts = self.out.first().and_then(|r| r.ts.clone());
        let cut = self.out.len() - max + 1;
        self.out.drain(..cut);
        let omitted = Some(wording::OMITTED.to_string());
        self.out.insert(
            0,
            RcFeedMessage {
                ts,
                ..feed_row(FEED_ROLE_SYSTEM, FEED_TYPE_STATUS, omitted, None)
            },
        );
    }

    /// Emit the open streak as a (partial) row — the flush clock's, and
    /// every terminal path's (before `Stale`, a reseed's `Reset`, `Down`).
    pub fn flush(&mut self) {
        self.seg.flush(&mut self.out);
    }

    /// The open streak's length, for the flush clock.
    pub fn open_bytes(&self) -> Option<usize> {
        self.seg.open_bytes()
    }

    /// The activity verdict with the approval override (correction 6): a
    /// pending approval is `NeedsApproval` whatever the stream said; else
    /// the last thing the stream said, or `None` when it has said nothing
    /// since [`CrazeFold::clear_activity`].
    pub fn activity(&self) -> Option<RcActivity> {
        if self.open_approvals() > 0 {
            return Some(RcActivity::NeedsApproval);
        }
        self.activity
    }

    /// Forget the verdict: a session row read from the host is newer than
    /// anything folded before it, and stands until the stream moves again.
    pub fn clear_activity(&mut self) {
        self.activity = None;
    }

    /// Whether a compaction of the main transcript is running.
    pub fn compacting(&self) -> bool {
        self.compacting
    }

    /// How many approvals are open — the session row's `pending_approvals`.
    pub fn open_approvals(&self) -> u32 {
        self.members().filter(|a| a.status.is_pending()).count() as u32
    }

    /// Every approval the book holds, whatever its status, in id order.
    pub fn held_approvals(&self) -> Vec<LaneApproval> {
        self.members().cloned().collect()
    }

    /// The approvals still waiting on the human.
    pub fn pending_approvals(&self) -> Vec<LaneApproval> {
        self.members()
            .filter(|a| a.status.is_pending())
            .cloned()
            .collect()
    }

    /// One booked approval.
    pub fn approval(&self, id: &str) -> Option<&LaneApproval> {
        self.book.get(id).filter(|b| b.member).map(|b| &b.approval)
    }

    /// The book's approvals: its members, in id order.
    fn members(&self) -> impl Iterator<Item = &LaneApproval> {
        self.book.values().filter(|b| b.member).map(|b| &b.approval)
    }

    /// How many tool states are retained — the bound's observable.
    pub fn tools_len(&self) -> usize {
        self.tools.len()
    }

    /// How many approvals are retained — the bound's observable.
    pub fn approvals_len(&self) -> usize {
        self.book.len()
    }

    /// The settings as folded.
    pub fn settings(&self) -> &SettingsState {
        &self.settings
    }

    /// Whether a `meta` delta changed the settings since the last call. (A
    /// snapshot's and an info document's never set it: the watcher re-emits
    /// capabilities and settings after every seed, resume and `ready`.)
    pub fn take_settings_changed(&mut self) -> bool {
        std::mem::take(&mut self.settings_changed)
    }

    /// Whether the book changed since the last call — so a caller re-reads
    /// [`CrazeFold::held_approvals`] only when there is something new in it.
    pub fn take_approvals_changed(&mut self) -> bool {
        std::mem::take(&mut self.approvals_changed)
    }

    /// An info document's catalogs (an attach reply's, a `ready`
    /// notification's), under the revision rule, and its capabilities'
    /// say on modes ([`SettingsState::apply_info`]).
    pub fn apply_info(&mut self, info: &SessionInfo) {
        self.settings.apply_info(info);
        self.cards = Cards::of(&info.capabilities);
    }

    /// A `session.set` craze confirmed at `rev: 0` — no `meta` delta will
    /// carry it — applied to the settings
    /// ([`SettingsState::apply_confirmed`]). Whether they changed.
    pub fn apply_confirmed(&mut self, s: &crate::wire::Setting) -> bool {
        self.settings
            .apply_confirmed(s.kind, s.id.as_deref(), &s.value)
    }

    /// The cards the session exposes — before a [`CrazeFold::restore`], so a
    /// hidden kind's open ask draws no card row.
    pub fn set_cards(&mut self, cards: Cards) {
        self.cards = cards;
    }

    // ---- rows ----

    /// End the run (craze's `endRun`): flush the open streak, and forget an
    /// omitted run — every row, and every event that closes a run, does this.
    fn end_run(&mut self) {
        self.seg.flush(&mut self.out);
        self.omitted_run = None;
    }

    /// One non-stream row, after ending the run above it.
    fn row(&mut self, role: &str, ty: &str, text: Option<String>, ts: Option<&str>) {
        self.end_run();
        self.out.push(feed_row(role, ty, text, ts));
    }

    fn status(&mut self, text: &str, ts: Option<&str>) {
        if text.is_empty() {
            return;
        }
        self.row(
            FEED_ROLE_SYSTEM,
            FEED_TYPE_STATUS,
            Some(text.to_string()),
            ts,
        );
    }

    /// One chunk of the main transcript's stream.
    fn stream(&mut self, kind: StreamKind, text: &str, ts: Option<&str>) {
        if text.is_empty() {
            return;
        }
        if self.omitted_run == Some(kind) {
            // The run continues on the host, in an entry the restored window
            // dropped: nothing here to draw (craze's appendStream).
            return;
        }
        self.omitted_run = None;
        self.seg.push(kind.0, kind.1, text, ts, &mut self.out);
    }

    // ---- the snapshot ----

    /// Fold a snapshot into a FRESH fold (§3.3.6, "Snapshot → rows"): the
    /// rows, the open run, the open asks, the settings. Everything this fold
    /// held is discarded first — a snapshot is a reseed's, never a resume's.
    /// `Err` when it is not a snapshot of the one codec this crate folds.
    pub fn restore(&mut self, snapshot: &Value) -> Result<Restored, String> {
        let Value::Object(snap) = snapshot else {
            return Err("a snapshot that is not an object".to_string());
        };
        let version = member::<u64>(snap, "version").unwrap_or(0);
        if version != u64::from(crate::wire::CODEC_SNAPSHOT) {
            return Err(format!(
                "a snapshot of codec version {version}; shed folds {}",
                crate::wire::CODEC_SNAPSHOT
            ));
        }
        let keep = self.session_id.clone();
        let cards = self.cards;
        *self = CrazeFold::new(&keep);
        self.cards = cards;
        let mut restored = Restored {
            incarnation: text_of(snap, "incarnation"),
            seq: member::<u64>(snap, "seq").unwrap_or(0),
            ..Restored::default()
        };
        self.replaying = member::<bool>(snap, "replaying").unwrap_or(false);
        let empty = Map::new();
        let main = snap
            .get("main")
            .and_then(Value::as_object)
            .unwrap_or(&empty);
        let omitted = array_of(main, "omitted");
        let windowed = member::<bool>(main, "windowed").unwrap_or(false);
        let dropped = member::<u64>(main, "dropped").unwrap_or(0);
        restored.truncated = windowed || dropped > 0 || !omitted.is_empty();
        if !omitted.is_empty() {
            self.status(wording::OMITTED, None);
        }
        self.todo_planned = member::<usize>(main, "todoPlanned").unwrap_or(0);
        self.todo_done = member::<bool>(main, "todoDone").unwrap_or(false);
        self.compacting = main.get("compacting").is_some_and(Value::is_object);
        let entries = array_of(main, "entries");
        let stream_open = member::<bool>(main, "streamOpen").unwrap_or(false);
        let open_last = stream_open
            && entries
                .last()
                .and_then(Value::as_object)
                .is_some_and(|e| member::<bool>(e, "streaming").unwrap_or(false));
        let (closed, open) = match entries.split_last() {
            Some((last, rest)) if open_last => (rest, Some(last)),
            _ => (entries, None),
        };
        for e in closed.iter().filter_map(Value::as_object) {
            self.entry_rows(e);
        }
        if let Some(settings) = snap.get("settings") {
            self.settings
                .apply_sections(&SettingsSections::from_value(settings));
        }
        // The open asks, after the closed entries and BEFORE the open run: a
        // non-automatic opening ends the run above it, so a run still open at
        // the cut began after every ask still open. Their rows only: the
        // approvals are the registry's (`adopt_registry`).
        for a in array_of(snap, "asks").iter().filter_map(Value::as_object) {
            let Some(body) = a.get("body") else { continue };
            let Some((opening, payload)) = Opening::from_body(str_of(a, "kind"), body) else {
                continue;
            };
            let id = opening.id().to_string();
            self.open_ask(
                opening,
                payload,
                a.get("at").and_then(Value::as_str),
                Source::Snapshot,
            );
            if member::<bool>(a, "truncated").unwrap_or(false) && !id.is_empty() {
                restored.truncated_asks.push(id);
            }
        }
        if let Some(e) = open.and_then(Value::as_object) {
            self.restore_run(e);
        }
        // The window dropped every entry, the open run's included: its kind
        // draws nothing until the run ends (craze's restore, `omittedRun`).
        if stream_open && entries.is_empty() && !omitted.is_empty() {
            self.omitted_run = run_kind(str_of(main, "omittedRun"));
        }
        Ok(restored)
    }

    /// The open run a snapshot ended on: its text becomes the open streak
    /// (craze's `restoreRun`; a `tailCut` run already leads with "…", so
    /// `main.tailCut` asks nothing more of it).
    fn restore_run(&mut self, e: &Map<String, Value>) {
        // Anything but a stream is a closed entry.
        let Some(kind) = run_kind(str_of(e, "kind")) else {
            return self.entry_rows(e);
        };
        self.seg.restore(
            kind.0,
            kind.1,
            str_of(e, "text"),
            e.get("at").and_then(Value::as_str),
            &mut self.out,
        );
    }

    /// One closed snapshot entry's rows.
    fn entry_rows(&mut self, e: &Map<String, Value>) {
        let at = e.get("at").and_then(Value::as_str);
        let text = str_of(e, "text");
        match str_of(e, "kind") {
            "user" => self.row(FEED_ROLE_USER, FEED_TYPE_TEXT, Some(text.to_string()), at),
            "assistant" => self.closed_text(ASSISTANT_TEXT, text, at),
            "thought" => self.closed_text(ASSISTANT_REASONING, text, at),
            "tool" => {
                if let Some(tool) = member::<Tool>(e, "tool") {
                    self.fold_tool(&tool, at);
                }
            }
            "note" | "error" => self.status(text, at),
            "plan" => {
                if let Some(plan) = member::<Plan>(e, "plan") {
                    self.status(&plan_entry_text(&plan), at);
                }
            }
            _ => {}
        }
    }

    /// A closed run's text, split at 8 KiB (lossless).
    fn closed_text(&mut self, kind: StreamKind, text: &str, at: Option<&str>) {
        if text.trim().is_empty() {
            return;
        }
        self.end_run();
        for part in split_rows(text) {
            self.out
                .push(feed_row(kind.0, kind.1, Some(part.to_string()), at));
        }
    }

    /// The engine's ask REGISTRY, read whole (`asks.list`, then `asks.get`
    /// for each listed id): from now on it IS the approval membership
    /// (Amendment A11). A record that is no longer `open`, an automatic one,
    /// and a hidden kind are skipped. Every other opens (or refreshes) its
    /// approval — a sub-agent's included, drawing no row; a snapshot ask it
    /// lists becomes an approval, one it does not list stays rows only. An
    /// approval this fold held pending and the registry no longer lists was
    /// resolved while it looked away: it is resolved here (a resume's client
    /// is told so). Whether the book changed.
    pub fn adopt_registry(&mut self, records: &[AskRecord]) -> bool {
        let open: Vec<(Opening, &Value, &AskRecord)> = records
            .iter()
            .filter(|r| r.status == crate::wire::ASK_OPEN && !r.id.is_empty())
            .filter_map(|r| {
                let (opening, payload) = Opening::from_body(&r.kind, &r.body)?;
                (opening.id() == r.id && self.cards.expose(&opening))
                    .then_some((opening, payload, r))
            })
            .collect();
        let mut changed = false;
        for (id, b) in &mut self.book {
            let listed = open.iter().any(|(_, _, r)| &r.id == id);
            if b.member && b.approval.status.is_pending() && !listed {
                b.approval.status = LaneApprovalStatus::Resolved;
                changed = true;
            }
        }
        for (opening, payload, r) in open {
            let mut approval =
                approval_of(&self.session_id, &opening, payload, r.opened_at.as_deref());
            match self.book.get_mut(&r.id) {
                Some(b) => {
                    approval.created_at_unix_ms = b
                        .approval
                        .created_at_unix_ms
                        .or(approval.created_at_unix_ms);
                    if !b.member || b.approval != approval {
                        changed = true;
                    }
                    b.approval = approval;
                    b.opening = opening;
                    b.member = true;
                }
                None => {
                    self.book.insert(
                        r.id.clone(),
                        Booked {
                            approval,
                            opening,
                            rowed: false,
                            member: true,
                        },
                    );
                    self.book_order.push_back(r.id.clone());
                    changed = true;
                }
            }
        }
        self.evict();
        self.approvals_changed |= changed;
        changed
    }

    // ---- events ----

    /// Fold one event body (an `event` notification's `event`, verbatim). A
    /// body that is not an object is ignored — the stream survives a frame
    /// this build cannot read.
    pub fn apply(&mut self, event: &Value) {
        let Value::Object(ev) = event else { return };
        let kind = str_of(ev, "type");
        let at = ev.get("at").and_then(Value::as_str);
        // A sub-agent's event draws no row: craze's fold sends it to the
        // child's transcript, and child-ignores every ask kind (the module
        // doc). Its asks are still the REGISTRY's — approvals, no rows.
        let main = str_of(ev, "agent").is_empty();
        match kind {
            "permission" | "question" | "plan" => {
                let Some((opening, payload)) = Opening::from_body(kind, event) else {
                    return;
                };
                if main {
                    self.end_run();
                    // A main non-automatic plan is transcript material too:
                    // craze's fold records the plan ENTRY and the open ask
                    // (`foldPlan`, `internal/transcript/fold.go`), so the
                    // restore path draws this same row from the snapshot's
                    // `plan` entry — live and restored read the same
                    // (Amendment A10). Hidden or not: the entry is the
                    // transcript's, the card is what a capability hides.
                    if let Opening::Plan(plan) = &opening {
                        self.status(&plan_entry_text(plan), at);
                    }
                }
                self.open_ask(opening, payload, at, Source::Live { main });
            }
            "ask" => {
                if let Some(u) = member::<AskUpdate>(ev, "ask") {
                    self.end_ask(&u, at, main);
                }
            }
            _ if !main => {}
            "text" => {
                self.activity = Some(RcActivity::Working);
                self.stream(ASSISTANT_TEXT, str_of(ev, "text"), at);
            }
            "thought" => {
                self.activity = Some(RcActivity::Working);
                self.stream(ASSISTANT_REASONING, str_of(ev, "text"), at);
            }
            "user" => {
                let (_, text) = split_shell_context(str_of(ev, "text"));
                if member::<bool>(ev, "interjection").unwrap_or(false) {
                    if !text.is_empty() {
                        self.row(FEED_ROLE_USER, FEED_TYPE_TEXT, Some(text.to_string()), at);
                    }
                } else if self.replaying || member::<bool>(ev, "replayed").unwrap_or(false) {
                    self.row(FEED_ROLE_USER, FEED_TYPE_TEXT, Some(text.to_string()), at);
                }
                // A live echo draws nothing: the row is the turn's started.
            }
            "tool" => {
                if let Some(tool) = member::<Tool>(ev, "tool") {
                    if tool.is_todo_tool() {
                        return;
                    }
                    let ts = tool.at.clone().or_else(|| at.map(str::to_string));
                    self.fold_tool(&tool, ts.as_deref());
                }
            }
            "todos" => {
                let todos: Vec<Todo> = member(ev, "todos").unwrap_or_default();
                self.note_todos(&todos, at);
            }
            "done" => {
                self.end_run();
                self.compacting = false;
                self.activity = Some(RcActivity::Idle);
                if member::<String>(ev, "stopReason").as_deref() == Some("cancelled") {
                    self.status(wording::CANCELLED, at);
                }
            }
            "error" => {
                // The turn is over, whatever becomes of its row.
                self.compacting = false;
                let text = ev
                    .get("err")
                    .and_then(Value::as_object)
                    .map_or("", |e| str_of(e, "message"));
                self.status(text, at);
            }
            "meta" => {
                let Some(state) = ev.get("state") else { return };
                let Value::Object(fields) = state else { return };
                let sections = SettingsSections::from_value(state);
                if self.settings.apply_sections(&sections) {
                    self.settings_changed = true;
                }
                for report in ["detail", "indexErr"] {
                    self.status(str_of(fields, report), at);
                }
            }
            "command" => {
                let line = ev
                    .get("command")
                    .and_then(|c| c.get("pluginCommand"))
                    .and_then(Value::as_object)
                    .map(|cmd| command_line(str_of(cmd, "qualified"), str_of(cmd, "kind")))
                    .unwrap_or_default();
                self.status(&line, at);
            }
            "foreign_turn" => {
                // Nested under `foreignTurn` (A7); a flat payload reads as
                // no bracket at all, which still closes the run.
                let ft = member::<ForeignTurn>(ev, "foreignTurn");
                self.end_run();
                match ft {
                    Some(ft) if ft.running => {
                        self.activity = Some(RcActivity::Working);
                        self.status(foreign_turn_note(&ft.reason), at);
                    }
                    _ => {
                        // Its end: no row, the session idle (A7), and no
                        // compaction a wake ran outlives it.
                        self.activity = Some(RcActivity::Idle);
                        self.compacting = false;
                    }
                }
            }
            "replay" => {
                let phase = ev
                    .get("replay")
                    .and_then(Value::as_object)
                    .map_or("", |r| str_of(r, "phase"));
                match phase {
                    "start" => self.replaying = true,
                    "end" => {
                        self.replaying = false;
                        self.compacting = false;
                        self.status(wording::RESTORED, at);
                    }
                    _ => {}
                }
            }
            "turn" => {
                let Some(turn) = member::<Turn>(ev, "turn") else {
                    return;
                };
                match turn.phase.as_str() {
                    "started" => {
                        self.activity = Some(RcActivity::Working);
                        let (_, text) = split_shell_context(&turn.text);
                        self.row(FEED_ROLE_USER, FEED_TYPE_TEXT, Some(text.to_string()), at);
                    }
                    "ended" => {
                        // No row and no flush (craze: only the wire's own
                        // `done` closes a run; the clock does here) — but the
                        // session is between turns.
                        self.activity = Some(RcActivity::Idle);
                        self.compacting = false;
                        if turn.synthetic && !turn.err.is_empty() {
                            self.status(&turn.err, at);
                        }
                    }
                    _ => {}
                }
            }
            "compaction" => {
                let Some(c) = member::<Compaction>(ev, "compaction") else {
                    return;
                };
                match c.phase.as_str() {
                    "started" => {
                        self.end_run();
                        self.compacting = true;
                    }
                    "ended" => {
                        self.compacting = false;
                        self.status(&compaction_note(&c), at);
                    }
                    _ => {}
                }
            }
            // `queue`, `subagent`, and a kind this build does not know.
            _ => {}
        }
    }

    /// A tool event (or a snapshot's tool entry): the run above it ends; its
    /// first sight draws `tool_use`, its first terminal status `tool_result`.
    fn fold_tool(&mut self, tool: &Tool, ts: Option<&str>) {
        self.end_run();
        let first = tool.id.is_empty() || !self.tools.contains_key(&tool.id);
        let detail = Some(tool.title.clone()).filter(|t| !t.trim().is_empty());
        if first {
            let state = ToolState {
                name: tool.display_name(),
                detail: detail.clone(),
                terminal_emitted: false,
            };
            self.out.push(RcFeedMessage {
                tool: Some(RcFeedTool {
                    name: state.name.clone(),
                    detail: state.detail.clone(),
                }),
                ..feed_row(FEED_ROLE_TOOL, FEED_TYPE_TOOL_USE, None, ts)
            });
            if !tool.id.is_empty() {
                self.remember_tool(tool.id.clone(), state);
            }
        }
        if !tool.terminal() {
            return;
        }
        let (name, held_detail) = match self.tools.get(&tool.id) {
            Some(s) if s.terminal_emitted => return,
            Some(s) => (s.name.clone(), s.detail.clone()),
            None => (tool.display_name(), detail.clone()),
        };
        if let Some(s) = self.tools.get_mut(&tool.id) {
            s.terminal_emitted = true;
        }
        let text = match tool.status.as_str() {
            "completed" => {
                let stdout = tool
                    .output
                    .as_ref()
                    .map(|o| o.stdout_head.clone())
                    .filter(|s| !s.trim().is_empty());
                stdout.or_else(|| {
                    Some(truncate_bytes(&tool.content_text, RESULT_HEAD_BYTES).to_string())
                        .filter(|s| !s.trim().is_empty())
                })
            }
            other => Some(other.to_string()),
        };
        self.out.push(RcFeedMessage {
            tool: Some(RcFeedTool {
                name,
                detail: detail.or(held_detail),
            }),
            ..feed_row(FEED_ROLE_TOOL, FEED_TYPE_TOOL_RESULT, text, ts)
        });
    }

    /// Record a tool's state, evicting the oldest COMPLETED tool when the map
    /// is over [`MAX_TOOLS`] — never one still in flight (gx's rule).
    fn remember_tool(&mut self, id: String, state: ToolState) {
        if self.tools.insert(id.clone(), state).is_none() {
            self.tool_order.push_back(id);
        }
        while self.tools.len() > MAX_TOOLS {
            let Some(pos) = self
                .tool_order
                .iter()
                .position(|k| self.tools.get(k).is_some_and(|s| s.terminal_emitted))
            else {
                break;
            };
            if let Some(k) = self.tool_order.remove(pos) {
                self.tools.remove(&k);
            }
        }
    }

    /// craze's `noteTodos`, rule for rule: `tasks: N planned` only when the
    /// list grows past the largest it was noted at, `tasks: c/n done` once when
    /// every item is closed; repeated and intermediate updates draw nothing.
    fn note_todos(&mut self, todos: &[Todo], at: Option<&str>) {
        if todos.is_empty() {
            return;
        }
        let closed = todos
            .iter()
            .filter(|t| t.status == "completed" || t.status == "cancelled")
            .count();
        if closed == todos.len() {
            if !self.todo_done {
                self.todo_done = true;
                self.status(&format!("tasks: {closed}/{} done", todos.len()), at);
            }
            return;
        }
        self.todo_done = false;
        if todos.len() > self.todo_planned {
            self.todo_planned = todos.len();
            self.status(&format!("tasks: {} planned", todos.len()), at);
        }
    }

    // ---- the approval book ----

    /// Open an ask (a second opening of an id replaces the first where it
    /// stands): book it, and — when it is in craze's transcript (a main
    /// opening, a snapshot's open ask) and not already showing — draw its
    /// pending `approval_request` row. A live opening is an approval (the
    /// registry holds every open ask); a snapshot's is rows only until
    /// [`CrazeFold::adopt_registry`] says. A hidden kind is neither, and an
    /// opening with no id cannot be answered or ended: neither is booked.
    fn open_ask(&mut self, opening: Opening, payload: &Value, at: Option<&str>, source: Source) {
        let id = opening.id().to_string();
        if id.is_empty() || !self.cards.expose(&opening) {
            return;
        }
        let approval = approval_of(&self.session_id, &opening, payload, at);
        let showing = self
            .book
            .get(&id)
            .is_some_and(|b| b.rowed && b.approval.status.is_pending());
        let (in_transcript, member) = match source {
            Source::Live { main } => (main, true),
            Source::Snapshot => (true, false),
        };
        if in_transcript && !showing {
            let decisions = advertised_decisions(&opening);
            self.end_run();
            self.out.push(RcFeedMessage {
                tool: Some(RcFeedTool {
                    name: Some(opening.kind_word().to_string()),
                    detail: approval.detail.clone(),
                }),
                approval: Some(RcFeedApproval {
                    id: id.clone(),
                    status: APPROVAL_STATUS_PENDING.to_string(),
                    decision: None,
                    decisions,
                }),
                ..feed_row(
                    FEED_ROLE_TOOL,
                    FEED_TYPE_APPROVAL_REQUEST,
                    Some(approval.title.clone()),
                    at,
                )
            });
        }
        let booked = Booked {
            approval,
            opening,
            rowed: in_transcript || showing,
            member,
        };
        if self.book.insert(id.clone(), booked).is_none() {
            self.book_order.push_back(id);
        }
        if member {
            self.approvals_changed = true;
        }
        self.evict();
    }

    /// The book's bound: settled entries go first, oldest first; a pending
    /// one never does.
    fn evict(&mut self) {
        while self.book.len() > MAX_APPROVALS {
            let Some(pos) = self.book_order.iter().position(|k| {
                self.book
                    .get(k)
                    .is_some_and(|b| !b.approval.status.is_pending())
            }) else {
                break;
            };
            if let Some(k) = self.book_order.remove(pos) {
                self.book.remove(&k);
            }
        }
    }

    /// An ask's ending: the approval resolved (last-write-wins) and — when the
    /// transcript holds the ask and the ending is the main agent's — the second
    /// `approval_request` row and, for an answered question or plan, craze's
    /// outcome note. An ending for an ask the book does not hold open (an
    /// automatic one, a hidden kind, one already ended) draws nothing.
    fn end_ask(&mut self, u: &AskUpdate, at: Option<&str>, main: bool) {
        let Some(booked) = self.book.get_mut(&u.id) else {
            return;
        };
        if !booked.approval.status.is_pending() {
            return;
        }
        booked.approval.status = LaneApprovalStatus::Resolved;
        if booked.member {
            self.approvals_changed = true;
        }
        // Rows follow craze's transcript: only a main ending of an ask the
        // transcript holds draws its resolved row and outcome note — a
        // sub-agent's ending (child-ignored) resolves the approval alone.
        if !(main && booked.rowed) {
            return;
        }
        let opening = booked.opening.clone();
        let title = booked.approval.title.clone();
        let decision = resolved_decision(&opening, u);
        self.end_run();
        self.out.push(RcFeedMessage {
            approval: Some(RcFeedApproval {
                id: u.id.clone(),
                status: APPROVAL_STATUS_RESOLVED.to_string(),
                decision,
                decisions: Vec::new(),
            }),
            ..feed_row(FEED_ROLE_TOOL, FEED_TYPE_APPROVAL_REQUEST, Some(title), at)
        });
        if u.outcome != "answered" || u.kind != opening.kind_word() {
            return;
        }
        match &opening {
            Opening::Question(q) => {
                if u.skip {
                    let note = format!("? {} → skipped", question_title(q));
                    self.status(&cap_note(&note), at);
                    return;
                }
                for item in &q.questions {
                    let picks = u
                        .answers
                        .get(&item.id)
                        .and_then(Option::as_ref)
                        .cloned()
                        .unwrap_or_default();
                    let note = answer_note(item, &picks);
                    self.status(&cap_note(&note), at);
                }
            }
            Opening::Plan(p) => {
                let verb = if u.accepted { "accepted" } else { "rejected" };
                let note = format!("plan {} → {verb}", plan_name(p));
                self.status(&cap_note(&note), at);
            }
            Opening::Permission(_) => {}
        }
    }
}

/// One transcript row, unnumbered (the ring numbers it): no tool, no approval.
fn feed_row(role: &str, ty: &str, text: Option<String>, ts: Option<&str>) -> RcFeedMessage {
    RcFeedMessage {
        ts: ts.map(str::to_string),
        role: role.to_string(),
        msg_type: ty.to_string(),
        text,
        ..RcFeedMessage::default()
    }
}

/// The contract's approval for one opening (§3.3.6, "Approvals →
/// `LaneApproval`"): a permission's options verbatim (ids opaque, kinds as
/// offered); a question's items, each its own [`LaneQuestion`] keyed by its own
/// id, with no free text; a plan's two synthesized options. `request_json` is
/// the opening as compact JSON.
fn approval_of(
    session_id: &str,
    opening: &Opening,
    payload: &Value,
    at: Option<&str>,
) -> LaneApproval {
    let (kind, title, detail, options, questions) = match opening {
        Opening::Permission(p) => (
            LaneApprovalKind::Permission,
            non_empty(&sanitize_line(&p.tool)).unwrap_or_else(|| "permission".to_string()),
            None,
            p.options
                .iter()
                .map(|o| LaneApprovalOption {
                    id: o.option_id.clone(),
                    label: o.name.clone(),
                    description: None,
                    kind: non_empty(&o.kind),
                })
                .collect(),
            Vec::new(),
        ),
        Opening::Question(q) => (
            LaneApprovalKind::Question,
            question_title(q),
            None,
            Vec::new(),
            q.questions
                .iter()
                .map(|item| LaneQuestion {
                    id: Some(item.id.clone()),
                    header: String::new(),
                    question: sanitize_feed_text(&item.prompt),
                    options: item
                        .options
                        .iter()
                        .map(|o| LaneApprovalOption {
                            id: o.id.clone(),
                            label: o.label.clone(),
                            description: non_empty(&o.description),
                            kind: None,
                        })
                        .collect(),
                    multiple: item.allow_multiple,
                    // craze's questions take no free text.
                    custom: false,
                })
                .collect(),
        ),
        Opening::Plan(p) => {
            let name = sanitize_line(&p.name);
            let overview = sanitize_feed_text(&p.overview);
            let detail = match (name.is_empty(), overview.trim().is_empty()) {
                (true, true) => None,
                (false, true) => Some(name.clone()),
                (true, false) => Some(overview),
                (false, false) => Some(format!("{name}\n{overview}")),
            };
            (
                LaneApprovalKind::PlanApproval,
                plan_name(p),
                detail,
                vec![
                    LaneApprovalOption {
                        id: plan_option::ACCEPT.to_string(),
                        label: "Accept plan".to_string(),
                        description: None,
                        kind: Some(option_kind::ALLOW_ONCE.to_string()),
                    },
                    LaneApprovalOption {
                        id: plan_option::REJECT.to_string(),
                        label: "Reject plan".to_string(),
                        description: None,
                        kind: Some(option_kind::REJECT_ONCE.to_string()),
                    },
                ],
                Vec::new(),
            )
        }
    };
    LaneApproval {
        id: opening.id().to_string(),
        session_id: session_id.to_string(),
        kind,
        status: LaneApprovalStatus::Pending,
        title: sanitize_feed_text(&title),
        detail: detail.map(|d| sanitize_feed_text(&d)),
        options,
        questions,
        request_json: serde_json::to_string(payload).unwrap_or_else(|_| "null".to_string()),
        created_at_unix_ms: at.and_then(shed_core::time::rfc3339_unix_ms),
    }
}

/// An `asks.get` record's approval as the book would open it — no time of its
/// own — and its opening; `None` when the body does not
/// read as a non-automatic ask of its `kind`.
pub(crate) fn record_approval(
    session_id: &str,
    record: &AskRecord,
) -> Option<(LaneApproval, Opening)> {
    let (opening, payload) = Opening::from_body(&record.kind, &record.body)?;
    Some((approval_of(session_id, &opening, payload, None), opening))
}

/// The feed's decision words an opening offers (the pending row's
/// `decisions`): a permission's option kinds as `allow` / `allow_always` /
/// `deny`, each once, in offered order; a plan's two; a question's none.
fn advertised_decisions(opening: &Opening) -> Vec<String> {
    match opening {
        Opening::Permission(p) => {
            let mut out: Vec<String> = Vec::new();
            for o in &p.options {
                if let Some(d) = decision_of_kind(&o.kind) {
                    if !out.iter().any(|x| x == d) {
                        out.push(d.to_string());
                    }
                }
            }
            out
        }
        Opening::Plan(_) => vec![
            APPROVAL_DECISION_ALLOW.to_string(),
            APPROVAL_DECISION_DENY.to_string(),
        ],
        Opening::Question(_) => Vec::new(),
    }
}

/// A permission option kind as the feed's decision word.
fn decision_of_kind(kind: &str) -> Option<&'static str> {
    match kind {
        option_kind::ALLOW_ONCE => Some(APPROVAL_DECISION_ALLOW),
        option_kind::ALLOW_ALWAYS => Some(APPROVAL_DECISION_ALLOW_ALWAYS),
        k if k.starts_with("reject") => Some(APPROVAL_DECISION_DENY),
        _ => None,
    }
}

/// The resolved row's decision: an answered permission's chosen option's kind,
/// an answered plan's accept or reject; `None` for anything else — a question
/// has no decision word, and an ending that is not an answer decided nothing.
fn resolved_decision(opening: &Opening, u: &AskUpdate) -> Option<String> {
    if u.outcome != "answered" {
        return None;
    }
    match opening {
        Opening::Permission(p) => p
            .options
            .iter()
            .find(|o| o.option_id == u.option_id)
            .and_then(|o| decision_of_kind(&o.kind))
            .map(str::to_string),
        Opening::Plan(_) => Some(
            if u.accepted {
                APPROVAL_DECISION_ALLOW
            } else {
                APPROVAL_DECISION_DENY
            }
            .to_string(),
        ),
        Opening::Question(_) => None,
    }
}

// ---------------------------------------------------------------------------
// craze's wordings, ported
// ---------------------------------------------------------------------------

/// craze's `noteForForeignTurn`: the native wake's own wordings for its two
/// reasons, and the interjection fallback's for `""` and for any reason this
/// build does not know.
pub fn foreign_turn_note(reason: &str) -> &'static str {
    match reason {
        foreign_reason::SUBAGENT_WAKE => wording::SUBAGENT_WAKE,
        foreign_reason::JOB_WAKE => wording::JOB_WAKE,
        _ => wording::FOREIGN_TURN,
    }
}

/// craze's `compactionNote`: how it went and what it did to the context —
/// `context compacted · 890k → 21k tokens`, worded by the reason (an unknown
/// one reads as automatic) — or why it failed.
pub fn compaction_note(c: &Compaction) -> String {
    if !c.err.is_empty() {
        let why = sanitize_line(&c.err);
        if why.is_empty() {
            return wording::COMPACTION_FAILED.to_string();
        }
        return format!("{}: {why}", wording::COMPACTION_FAILED);
    }
    let head = match c.reason.as_str() {
        "manual" => wording::COMPACTED_ON_REQUEST,
        "overflow" => wording::COMPACTED_OVERFLOW,
        _ => wording::COMPACTED,
    };
    format!(
        "{head} · {} → {} tokens",
        token_count(c.tokens_before),
        token_count(c.tokens_after)
    )
}

/// craze's `tokenCount`: the number under a thousand, else in k or M to three
/// significant figures with trailing zeros dropped — 850, 1.23k, 12.3k, 890k,
/// 1.21M, 2M; a negative count reads as 0.
pub fn token_count(n: i64) -> String {
    if n < 1000 {
        return n.max(0).to_string();
    }
    let (mut v, mut unit) = (n as f64 / 1e3, "k");
    if n >= 1_000_000 {
        v = n as f64 / 1e6;
        unit = "M";
    }
    let digits = if v < 10.0 {
        2
    } else if v < 100.0 {
        1
    } else {
        0
    };
    let mut s = format!("{v:.digits$}");
    if unit == "k" && s.parse::<f64>().is_ok_and(|r| r >= 1000.0) {
        // 999,600 rounds to "1000k": it is a million.
        s = "1".to_string();
        unit = "M";
    }
    if s.contains('.') {
        s = s.trim_end_matches('0').trim_end_matches('.').to_string();
    }
    format!("{s}{unit}")
}

/// craze's `commandLine`: the mark, the qualified name and the kind, each on
/// one line; nothing for a name that sanitises to empty.
pub fn command_line(qualified: &str, kind: &str) -> String {
    let name = sanitize_line(qualified);
    if name.is_empty() {
        return String::new();
    }
    let kind = sanitize_line(kind);
    if kind.is_empty() {
        format!("{}{name}", wording::COMMAND_MARK)
    } else {
        format!("{}{name} ({kind})", wording::COMMAND_MARK)
    }
}

/// craze's `questionTitle`: the title, else the first question's prompt, else
/// "question".
fn question_title(q: &Question) -> String {
    let t = sanitize_line(&q.title);
    if !t.is_empty() {
        return t;
    }
    match q.questions.first() {
        Some(first) => sanitize_line(&first.prompt),
        None => "question".to_string(),
    }
}

/// A plan entry's `system/status` row: `plan <name>`, and its overview on a
/// line of its own when it has one — the same text whether the plan was
/// folded live or restored from a snapshot's `plan` entry (Amendment A10).
fn plan_entry_text(p: &Plan) -> String {
    let text = format!("plan {}", plan_name(p));
    let overview = sanitize_line(&p.overview);
    if overview.is_empty() {
        text
    } else {
        format!("{text}\n{overview}")
    }
}

/// craze's `PlanName`: the name on one line, else "plan".
fn plan_name(p: &Plan) -> String {
    non_empty(&sanitize_line(&p.name)).unwrap_or_else(|| "plan".to_string())
}

/// craze's `answerNote`: `? <prompt> → ` and what each pick names, in order,
/// comma-separated; a pick naming nothing is left out, nothing named reads
/// "nothing". It stops naming once the note is past [`OUTCOME_NOTE_CAP`].
fn answer_note(item: &QuestionItem, picks: &[String]) -> String {
    let mut note = format!("? {} → ", sanitize_line(&item.prompt));
    let mut named = 0;
    for id in picks {
        if note.len() > OUTCOME_NOTE_CAP {
            break;
        }
        let Some(name) = pick_name(&item.options, id) else {
            continue;
        };
        if named > 0 {
            note.push_str(", ");
        }
        note.push_str(&name);
        named += 1;
    }
    if named == 0 {
        note.push_str("nothing");
    }
    note
}

/// craze's `pickName`: one option with that id names its label; none names
/// nothing; two or more name the ellipsis (ambiguous — naming the first would
/// word the wrong option).
fn pick_name(options: &[QuestionOption], id: &str) -> Option<String> {
    let mut found = options.iter().filter(|o| o.id == id);
    match (found.next(), found.next()) {
        (None, _) => None,
        (Some(o), None) => Some(sanitize_line(&o.label)),
        (Some(_), Some(_)) => Some("…".to_string()),
    }
}

/// craze's `capNote`: the note itself when it fits [`OUTCOME_NOTE_CAP`], else
/// its head cut back to a character boundary and closed by "…".
fn cap_note(s: &str) -> String {
    if s.len() <= OUTCOME_NOTE_CAP {
        return s.to_string();
    }
    format!("{}…", truncate_bytes(s, OUTCOME_NOTE_CAP - "…".len()))
}

/// craze's `sanitizeLine`: escape sequences and control characters go, runs of
/// whitespace collapse to one space — one clean line.
pub fn sanitize_line(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    // craze drops every control character but \n, \r and \t (which become
    // spaces); the feed's sanitizer keeps \v and \f as whitespace, so they go
    // first.
    let s = s.replace(['\u{0b}', '\u{0c}'], "");
    sanitize_feed_text(&s)
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

fn non_empty(s: &str) -> Option<String> {
    (!s.trim().is_empty()).then(|| s.to_string())
}

// ---------------------------------------------------------------------------
// the strip: what craze put in front of what the user wrote
// ---------------------------------------------------------------------------

/// craze's `SplitShellContext` (`internal/agent/shellcontext.go`), ported:
/// the leading blocks craze writes in front of a message — a well-formed
/// attachment envelope (`<craze_attachments>{…}</craze_attachments>`), then a
/// well-formed shell context block — and the rest, which is what the user
/// wrote. The two parts together are exactly `text`; a block that is not
/// well-formed is the user's text and stays in the rest.
pub fn split_shell_context(text: &str) -> (&str, &str) {
    let envelope = leading_envelope(text);
    let (shell, _) = split_shell_block(&text[envelope..]);
    let cut = envelope + shell;
    (&text[..cut], &text[cut..])
}

const ATTACHMENTS_OPEN: &str = "<craze_attachments>";
const ATTACHMENTS_CLOSE: &str = "</craze_attachments>";
const SHELL_CONTEXT_OPEN: &str = "<shell_context>";
const SHELL_CONTEXT_CLOSE: &str = "</shell_context>";
const SHELL_COMMAND_OPEN: &str = "<command exit=\"";

/// craze's `leadingEnvelope`: the open tag first; the first close tag after
/// it, followed by a newline or the end of the text; between them one JSON
/// object of the envelope's shape (Go's `json.Unmarshal` into `{v int, images
/// []{n, path, mime, ow, oh}}`: members matched case-insensitively, every
/// other member ignored, a known member of the wrong type refused). Returns
/// the envelope's length — tags, JSON and newline — or 0 for none.
fn leading_envelope(text: &str) -> usize {
    let Some(body) = text.strip_prefix(ATTACHMENTS_OPEN) else {
        return 0;
    };
    let Some(i) = body.find(ATTACHMENTS_CLOSE) else {
        return 0;
    };
    let mut end = ATTACHMENTS_OPEN.len() + i + ATTACHMENTS_CLOSE.len();
    if end < text.len() {
        if text.as_bytes()[end] != b'\n' {
            return 0;
        }
        end += 1;
    }
    let json = &body[..i];
    if !json
        .trim_start_matches([' ', '\t', '\r', '\n'])
        .starts_with('{')
    {
        return 0;
    }
    match serde_json::from_str::<Value>(json) {
        Ok(Value::Object(obj)) if envelope_shape(&obj) => end,
        _ => 0,
    }
}

/// Whether a decoded object would decode into craze's envelope struct.
fn envelope_shape(obj: &Map<String, Value>) -> bool {
    let int = |v: &Value| v.is_null() || v.as_i64().is_some();
    let string = |v: &Value| v.is_null() || v.is_string();
    for (k, v) in obj {
        if k.eq_ignore_ascii_case("v") && !int(v) {
            return false;
        }
        if k.eq_ignore_ascii_case("images") {
            match v {
                Value::Null => {}
                Value::Array(items) => {
                    for item in items {
                        match item {
                            Value::Null => {}
                            Value::Object(r) => {
                                for (rk, rv) in r {
                                    let ok = match rk.to_ascii_lowercase().as_str() {
                                        "n" | "ow" | "oh" => int(rv),
                                        "path" | "mime" => string(rv),
                                        _ => true,
                                    };
                                    if !ok {
                                        return false;
                                    }
                                }
                            }
                            _ => return false,
                        }
                    }
                }
                _ => return false,
            }
        }
    }
    true
}

/// craze's `splitShellBlock`: a block's frame — a first line of exactly
/// `<shell_context>`, the next line opening an entry (`<command exit="`), and
/// some later line of exactly `</shell_context>` — and the blank line after
/// it. Returns the block's length (0 for none) and whether one was found.
fn split_shell_block(text: &str) -> (usize, bool) {
    let open = format!("{SHELL_CONTEXT_OPEN}\n");
    if !text.starts_with(&open) {
        return (0, false);
    }
    let mut at = open.len();
    if !text[at..].starts_with(SHELL_COMMAND_OPEN) {
        return (0, false);
    }
    while at < text.len() {
        let rest = &text[at..];
        let (line, next) = match rest.find('\n') {
            Some(nl) => (&rest[..nl], at + nl + 1),
            None => (rest, text.len()),
        };
        if line == SHELL_CONTEXT_CLOSE {
            let mut end = next;
            if end < text.len() && text.as_bytes()[end] == b'\n' {
                // The blank line the builder writes belongs to the block.
                end += 1;
            }
            return (end, true);
        }
        at = next;
    }
    (0, false)
}

/// Fold a whole snapshot on its own — `history`'s read, and the live
/// recording's offline replay: the rows (the open run flushed, since a page has
/// no later chunk to wait for) and whether the window was cut.
pub fn snapshot_rows(
    session_id: &str,
    snapshot: &Value,
) -> Result<(Vec<RcFeedMessage>, Restored), String> {
    let mut fold = CrazeFold::new(session_id);
    let restored = fold.restore(snapshot)?;
    fold.flush();
    Ok((fold.drain(), restored))
}

#[cfg(test)]
mod tests;
