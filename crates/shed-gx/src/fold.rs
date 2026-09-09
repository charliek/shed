//! The **pure fold**: gx envelopes in, transcript rows and an activity verdict
//! out; approval resources in, [`LaneApproval`]s out.
//!
//! No I/O, no clock, no transport. Everything the fold knows is explicit state
//! it was handed, which is what lets the same code fold a `GET …/history` page,
//! an SSE seed and a live frame and get the same rows.
//!
//! # The cursor model
//!
//! A gx event id is `<session-prefix>-<counter>`, and the prefix is itself a
//! UUID full of hyphens (`01a0fa1e-0000-7000-8000-0000000000ab-509197`), so it
//! is split at the **last** hyphen and the counter is compared **numerically**.
//! String ordering is not an option: `…-9` sorts after `…-10`.
//!
//! Two cursors, and they are different:
//!
//! - [`GxFold::last_applied`] advances on **every accepted id-bearing
//!   envelope**, ignored kinds and non-terminal chunks included. It is the
//!   positional mark — "everything up to here has been folded".
//! - [`GxFold::resume_cursor`] is the **highest counter seen**, and it is what a
//!   `Last-Event-ID` header carries. It differs from `last_applied` because gx's
//!   counters are **not monotonic in transcript order**: a real page runs
//!   `…509197, 509195, 509196, 509200, 509203, 509202…` (the counter is
//!   process-global and several sessions bump it). Resuming from the last
//!   APPLIED id would re-request frames already folded; resuming from the
//!   highest is what gx's own replay planner expects.
//!
//! [`GxFold::seen`] is the dedup — a bounded set of ids, [`MAX_SEEN`] of them —
//! and it is what makes the seed/live overlap safe. **Id-less envelopes are
//! never deduped**: they carry nothing to dedup on, and dropping them would lose
//! real rows.
//!
//! # Rows are append-only
//!
//! The contract has no in-place update event, so a long assistant turn is
//! emitted as SEGMENTS: a streak of chunks accumulates and is emitted when
//! something ends it (see [`OpenStreak`]). Up to [`MAX_STREAK_BYTES`] and — in
//! the watcher — up to `flush_after` of silence are therefore invisible to a
//! client. That is the known cost of append-only rows, and it is the reason
//! `turn_completed` and a tool call both close a streak: the moment there is
//! something else to show, whatever was accumulating is shown first.

use std::collections::{BTreeMap, HashMap, HashSet, VecDeque};
use std::sync::Arc;

use serde::Deserialize;
use serde_json::value::RawValue;
use serde_json::Value;

use shed_core::lane::feed::{
    sanitize_feed_text, FEED_ROLE_ASSISTANT, FEED_ROLE_SYSTEM, FEED_ROLE_TOOL, FEED_ROLE_USER,
    FEED_TYPE_APPROVAL_REQUEST, FEED_TYPE_REASONING, FEED_TYPE_STATUS, FEED_TYPE_TEXT,
    FEED_TYPE_TOOL_RESULT, FEED_TYPE_TOOL_USE,
};
use shed_core::lane::{
    option_kind, LaneApproval, LaneApprovalKind, LaneApprovalOption, LaneApprovalStatus,
    LaneQuestion,
};
use shed_core::rc::{RcActivity, RcFeedApproval, RcFeedMessage, RcFeedTool};
use shed_core::roost::rfc3339_z;

/// How many event ids the dedup set remembers. Four thousand is far more than
/// any overlap between a seed page and the frames buffered during it, and small
/// enough that a session running for days cannot grow it without bound.
pub const MAX_SEEN: usize = 4096;

/// How many tool states the fold remembers. Well above any live set — a session
/// has a handful of calls in flight — so eviction only ever reaches calls that
/// finished long ago. See [`GxFold::tools`] for what eviction costs.
pub const MAX_TOOLS: usize = 512;

/// How many approvals the fold remembers. **Pending ones are never evicted**;
/// only resolved/submitted ones are, oldest-first.
pub const MAX_APPROVALS: usize = 256;

/// How much text one transcript row may accumulate before the fold segments it.
///
/// It IS the feed's own per-field cap rather than a second spelling of its
/// value: the rationale — "a row that reaches this is one the ring would have
/// truncated anyway" — is only true while the two agree, so the tie is
/// expressed in code rather than asserted in prose.
pub const MAX_STREAK_BYTES: usize = shed_core::lane::feed::MAX_FEED_MESSAGE_BYTES;

/// A timestamp at or above this is milliseconds; below it, seconds.
///
/// gx's persisted envelopes stamp `timestamp` in **seconds** while
/// `_meta.agentTimestampMs` is milliseconds, and both reach this fold. 10¹¹
/// seconds is the year 5138 and 10¹¹ milliseconds is 1973, so the split is
/// unambiguous for every timestamp either wire will ever carry.
pub const MS_THRESHOLD: i64 = 100_000_000_000;

// ---------------------------------------------------------------------------
// event ids
// ---------------------------------------------------------------------------

/// A parsed `<session-prefix>-<counter>` event id.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EventId {
    /// The id exactly as it arrived — what a `Last-Event-ID` header sends. gx
    /// wants the WHOLE opaque string, never the counter alone.
    pub raw: String,
    /// Everything before the last hyphen. Compared, never parsed.
    pub prefix: String,
    /// The numeric suffix. Compared **numerically**; see the module doc.
    pub counter: u64,
}

impl EventId {
    /// Split at the LAST hyphen — the prefix is a UUID and contains four of its
    /// own. A missing hyphen, an empty prefix or a non-numeric suffix is not an
    /// event id.
    pub fn parse(raw: &str) -> Option<EventId> {
        let raw = raw.trim();
        let (prefix, counter) = split_id(raw)?;
        Some(EventId {
            raw: raw.to_string(),
            prefix: prefix.to_string(),
            counter,
        })
    }

    /// Whether this id belongs to `session_id`. A foreign prefix is what makes a
    /// resume cursor "unresolvable" on gx's side too.
    pub fn belongs_to(&self, session_id: &str) -> bool {
        self.prefix == session_id
    }
}

/// The id grammar itself, in one place: trimmed, split at the LAST hyphen, a
/// non-empty prefix and a numeric suffix.
///
/// [`EventId::parse`] and [`counter_of`] are its only readers, and they share it
/// rather than each spelling it — a comparison that accepted an id the parser
/// refuses (or the reverse) would make the cursor hunt and the fold disagree
/// about which envelopes exist.
fn split_id(raw: &str) -> Option<(&str, u64)> {
    let (prefix, counter) = raw.trim().rsplit_once('-')?;
    if prefix.is_empty() || counter.is_empty() {
        return None;
    }
    Some((prefix, counter.parse().ok()?))
}

/// An id's counter, without building an [`EventId`].
///
/// The cursor hunt reads every id in up to ten 200-envelope pages, twice; going
/// through `parse` would allocate two `String`s per read and discard both.
fn counter_of(raw: &str) -> Option<u64> {
    split_id(raw).map(|(_, counter)| counter)
}

// ---------------------------------------------------------------------------
// the wire
// ---------------------------------------------------------------------------

/// Go-null tolerance: an explicit `null` decodes as the default rather than
/// killing the whole envelope. The same shim `shed-opencode`'s fold rides, for
/// the same reason — gx writes `"eventId": null` and `"timestamp": null` on any
/// line whose source carried none.
///
/// The crate's ONE copy: [`crate::client`]'s DTOs ride this same shim, because
/// a tolerance rule stated twice is one a fix reaches half of.
pub(crate) fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// One normalized gx envelope — the shape both `GET …/history` and an SSE
/// `update` frame carry.
#[derive(Debug, Default, Clone, Deserialize)]
pub struct GxEnvelope {
    #[serde(default, rename = "eventId")]
    pub event_id: Option<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub method: String,
    #[serde(default, deserialize_with = "null_default")]
    pub params: GxParams,
    /// **Seconds** on a persisted line, milliseconds if a producer ever changes
    /// its mind; [`MS_THRESHOLD`] decides. `_meta.agentTimestampMs` wins when
    /// present.
    #[serde(default)]
    pub timestamp: Option<i64>,
}

#[derive(Debug, Default, Clone, Deserialize)]
pub struct GxParams {
    #[serde(default, rename = "sessionId", deserialize_with = "null_default")]
    pub session_id: String,
    #[serde(default, deserialize_with = "null_default")]
    pub update: Value,
    #[serde(default, rename = "_meta", deserialize_with = "null_default")]
    pub meta: Value,
}

impl GxEnvelope {
    /// The `sessionUpdate` discriminator, or `""`.
    pub fn session_update(&self) -> &str {
        self.params
            .update
            .get("sessionUpdate")
            .and_then(Value::as_str)
            .unwrap_or_default()
    }

    /// The envelope's timestamp as RFC 3339 UTC, `None` when it carries none.
    ///
    /// `_meta.agentTimestampMs` (milliseconds) first, then the envelope's own
    /// `timestamp` under the [`MS_THRESHOLD`] rule.
    pub fn ts(&self) -> Option<String> {
        if let Some(ms) = self
            .params
            .meta
            .get("agentTimestampMs")
            .and_then(Value::as_i64)
        {
            return Some(rfc3339_z(ms.div_euclid(1_000)));
        }
        let t = self.timestamp?;
        // `unsigned_abs`, not `abs`: `i64::MIN.abs()` PANICS in a checked build,
        // and this value is agent-supplied — so a hostile or broken producer
        // could kill the fold with one envelope.
        let secs = if t.unsigned_abs() < MS_THRESHOLD as u64 {
            t
        } else {
            t.div_euclid(1_000)
        };
        Some(rfc3339_z(clamp_epoch_secs(secs)))
    }

    /// `params._meta.promptId`. **Absent means "no change"**, not "a different
    /// prompt": a `user_message_chunk` carries no `promptId` at all (only
    /// `_meta.promptIndex`), and treating that as a change would split every
    /// user turn off from itself.
    pub fn prompt_id(&self) -> Option<String> {
        self.params
            .meta
            .get("promptId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
    }
}

/// The widest instant this fold will format, in seconds since the epoch.
///
/// `rfc3339_z` is exact for every year an `i64` of days can express, but it
/// formats a year of `0000` with four digits and anything beyond that with as
/// many as it needs — so an absurd input yields an absurd year rather than an
/// error, and a client renders `+271821-04-20` in a transcript. Clamping keeps a
/// nonsense timestamp visibly at the edge of the range instead.
///
/// The bound is years 0001..=9999, which is exactly the range RFC 3339's
/// four-digit year can represent.
const MIN_EPOCH_SECS: i64 = -62_135_596_800; // 0001-01-01T00:00:00Z
const MAX_EPOCH_SECS: i64 = 253_402_300_799; // 9999-12-31T23:59:59Z

/// Clamps a wire timestamp into the range RFC 3339 can actually spell.
fn clamp_epoch_secs(secs: i64) -> i64 {
    secs.clamp(MIN_EPOCH_SECS, MAX_EPOCH_SECS)
}

// ---------------------------------------------------------------------------
// the fold's explicit state
// ---------------------------------------------------------------------------

/// The one open transcript streak: consecutive chunks of the same role and type
/// under the same prompt, accumulating until something ends them.
///
/// **At most one is open at a time.** A streak ends and is EMITTED when:
///
/// - a transcript-bearing update of a different kind arrives (another chunk
///   kind, a `tool_call`, a terminal `tool_call_update`);
/// - `turn_completed` arrives;
/// - the `promptId` changes (absent = no change);
/// - it passes [`MAX_STREAK_BYTES`] — the next chunk opens a new one.
///
/// Ignored kinds ([`is_ignored_kind`]) are **transparent**: `hook_execution` is
/// 15 frames in 40 on a real page and interleaves with chunk streaks, so a fold
/// that closed a streak on one would shred every assistant turn into fragments.
#[derive(Debug, Clone)]
pub struct OpenStreak {
    pub role: &'static str,
    pub feed_type: &'static str,
    pub prompt_id: Option<String>,
    pub text: String,
    /// The timestamp of the FIRST chunk — a row is stamped when it started, not
    /// when it was flushed.
    pub first_ts: Option<String>,
    pub last_event_id: Option<String>,
}

/// What the fold remembers about a tool call between its `tool_call` and its
/// terminal `tool_call_update`.
#[derive(Debug, Clone, Default)]
pub struct ToolState {
    pub name: String,
    pub detail: String,
    /// Set once a terminal update has produced a `tool_result` row, so repeats
    /// are absorbed. gx re-sends a terminal update on re-attach and on replay.
    pub terminal_emitted: bool,
}

/// A bounded insertion-ordered set of event ids.
///
/// The id is held as one `Arc<str>` shared by the set and the queue: a
/// duplicate — the seed/live overlap this set exists for — costs no allocation
/// at all, and a new id costs one rather than two.
#[derive(Debug, Default)]
struct SeenSet {
    set: HashSet<Arc<str>>,
    order: VecDeque<Arc<str>>,
}

impl SeenSet {
    /// `true` if this is the first sight of `id`.
    fn insert(&mut self, id: &str) -> bool {
        // Borrowed lookup: the duplicate path never allocates.
        if self.set.contains(id) {
            return false;
        }
        let id: Arc<str> = Arc::from(id);
        self.set.insert(Arc::clone(&id));
        self.order.push_back(id);
        while self.order.len() > MAX_SEEN {
            if let Some(old) = self.order.pop_front() {
                self.set.remove(&old);
            }
        }
        true
    }

    fn clear(&mut self) {
        self.set.clear();
        self.order.clear();
    }
}

/// The gx fold.
///
/// Feed it envelopes with [`GxFold::apply`], take rows out with
/// [`GxFold::drain_messages`], read the verdict with [`GxFold::activity`].
/// A reseed is [`GxFold::reset`]; a silent resume is simply not calling it.
#[derive(Debug)]
pub struct GxFold {
    session_id: String,
    open: Option<OpenStreak>,
    /// Tool state by `toolCallId`, **bounded at [`MAX_TOOLS`]**.
    ///
    /// It was unbounded, and the desktop holds a fold for the life of a
    /// session, so a long session leaked one entry per tool call forever.
    /// Eviction is oldest-first and takes only tools that have already emitted
    /// their terminal row; a call still in flight is never dropped, because
    /// losing it would lose the name its result row is built from.
    ///
    /// **Residual:** if the agent replays a `tool_call` for an id evicted after
    /// [`MAX_TOOLS`] newer calls, the fold has forgotten it completed and emits
    /// a second `tool_result`. Same shape as the [`GxFold::seen`] bound, and
    /// healed the same way — by the next reseed.
    tools: HashMap<String, ToolState>,
    /// Insertion order for [`GxFold::tools`], so eviction is oldest-first
    /// rather than hash order.
    tool_order: VecDeque<String>,
    /// Approvals by id, **bounded at [`MAX_APPROVALS`]** and never evicting a
    /// pending one — an approval the human still owes an answer to must not be
    /// forgotten at any cap.
    approvals: BTreeMap<String, LaneApproval>,
    /// Insertion order for [`GxFold::approvals`].
    approval_order: VecDeque<String>,
    last_applied: Option<EventId>,
    max_seen: Option<EventId>,
    /// The dedup set, **bounded at [`MAX_SEEN`]** and deliberately so.
    ///
    /// **Residual, and it is the right trade:** once more than [`MAX_SEEN`]
    /// newer ids have been seen, an old id is forgotten, so a frame re-delivered
    /// after that much traffic folds a second time and duplicates its row. The
    /// alternative — an unbounded set — trades a rare duplicate row for a leak
    /// that grows for the life of the session, which is the bug this bound
    /// exists to prevent. A reseed clears the set and rebuilds the transcript,
    /// which heals it.
    seen: SeenSet,
    out: Vec<RcFeedMessage>,
    activity: RcActivity,
}

impl GxFold {
    pub fn new(session_id: &str) -> GxFold {
        GxFold {
            session_id: session_id.to_string(),
            open: None,
            tools: HashMap::new(),
            tool_order: VecDeque::new(),
            approvals: BTreeMap::new(),
            approval_order: VecDeque::new(),
            last_applied: None,
            max_seen: None,
            seen: SeenSet::default(),
            out: Vec::new(),
            activity: RcActivity::Unknown,
        }
    }

    /// Fold one envelope. `false` means it was a duplicate and nothing changed.
    ///
    /// An envelope whose id is malformed or foreign to this session is folded as
    /// though it had **no id**: it is applied (dropping real rows would be
    /// worse), never deduped, and never advances either cursor. A cursor built
    /// from a foreign prefix is exactly what gx answers `cursor_unresolvable`
    /// to, so it must not become one.
    pub fn apply(&mut self, env: &GxEnvelope) -> bool {
        let id = env
            .event_id
            .as_deref()
            .and_then(EventId::parse)
            .filter(|e| e.belongs_to(&self.session_id));

        if let Some(id) = &id {
            if !self.seen.insert(&id.raw) {
                return false;
            }
        }

        self.apply_update(env);

        if let Some(id) = id {
            // Advances on EVERY accepted id-bearing envelope — ignored kinds
            // and non-terminal chunks included. That is what makes it a
            // positional mark rather than a "last interesting thing".
            if self
                .max_seen
                .as_ref()
                .is_none_or(|m| id.counter > m.counter)
            {
                self.max_seen = Some(id.clone());
            }
            self.last_applied = Some(id);
        }
        true
    }

    /// Fold one raw JSON envelope. A line that does not decode is ignored — the
    /// stream must survive a frame this build cannot read.
    pub fn apply_line(&mut self, line: &[u8]) -> bool {
        match serde_json::from_slice::<GxEnvelope>(line) {
            Ok(env) => self.apply(&env),
            Err(_) => false,
        }
    }

    fn apply_update(&mut self, env: &GxEnvelope) {
        let kind = env.session_update();
        let update = &env.params.update;

        match kind {
            "user_message_chunk" => self.chunk(env, FEED_ROLE_USER, FEED_TYPE_TEXT),
            "agent_message_chunk" => self.chunk(env, FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT),
            "agent_thought_chunk" => self.chunk(env, FEED_ROLE_ASSISTANT, FEED_TYPE_REASONING),
            "tool_call" => self.tool_call(env, update),
            "tool_call_update" => self.tool_call_update(env, update),
            "turn_completed" => self.turn_completed(env, update),
            // Transparent: state untouched, the open streak untouched, and the
            // caller still advances `last_applied` for them. Spelled as the
            // named list rather than folded into the catch-all so the module
            // doc's claim is true — the fold CONSULTS `is_ignored_kind` — and
            // so "deliberately ignored" stays distinguishable from "a kind this
            // build has never heard of", which is the distinction C3's watcher
            // wants for a counter.
            k if is_ignored_kind(k) => {}
            _ => {}
        }
    }

    // ---- chunks ----

    fn chunk(&mut self, env: &GxEnvelope, role: &'static str, feed_type: &'static str) {
        let text = content_text(env.params.update.get("content"));
        let prompt_id = env.prompt_id();

        let ends = self.open.as_ref().is_none_or(|open| {
            open.role != role
                || open.feed_type != feed_type
                // Absent = no change; a change only when BOTH are known and differ.
                || matches!(
                    (&open.prompt_id, &prompt_id),
                    (Some(a), Some(b)) if a != b
                )
        });
        if ends {
            self.flush_open();
        }
        // `flush_open` TOOK the streak, so this opens a fresh one on exactly the
        // paths that ended one and reuses the streak on the paths that did not.
        // There is no third case — which is why this is an insert rather than a
        // fallible re-borrow.
        let open = self.open.get_or_insert_with(|| OpenStreak {
            role,
            feed_type,
            prompt_id: prompt_id.clone(),
            text: String::new(),
            first_ts: env.ts(),
            last_event_id: env.event_id.clone(),
        });
        if open.prompt_id.is_none() {
            open.prompt_id.clone_from(&prompt_id);
        }
        if open.first_ts.is_none() {
            open.first_ts = env.ts();
        }
        self.append_segmented(env, role, feed_type, prompt_id.as_deref(), &text);
    }

    /// Append `text` to the open streak, segmenting it across as many rows as it
    /// takes — **losslessly, and never mid-character**.
    ///
    /// The cap used to be checked only AFTER a whole chunk had been appended,
    /// so a streak could finish 8,191 bytes long, take one `é`, and emit a
    /// 8,193-byte row that the capping layer then TRUNCATED — losing text. §3.4
    /// calls this *segmentation*: the point is that the next chunk opens a new
    /// streak, not that content is dropped. So now a chunk that does not fit is
    /// split at a UTF-8 boundary, the full segment is emitted, and the remainder
    /// carries into a fresh streak — as many times as needed for a chunk several
    /// times the cap.
    ///
    /// No row leaves here longer than [`MAX_STREAK_BYTES`], no byte is dropped,
    /// and no multi-byte codepoint is split.
    fn append_segmented(
        &mut self,
        env: &GxEnvelope,
        role: &'static str,
        feed_type: &'static str,
        prompt_id: Option<&str>,
        text: &str,
    ) {
        let mut remaining = text;
        loop {
            let used = self.open.as_ref().map_or(0, |o| o.text.len());
            let room = MAX_STREAK_BYTES.saturating_sub(used);
            if remaining.len() <= room {
                if let Some(open) = self.open.as_mut() {
                    open.text.push_str(remaining);
                    open.last_event_id = env.event_id.clone();
                }
                return;
            }
            let take = floor_char_boundary(remaining, room);
            if take > 0 {
                if let Some(open) = self.open.as_mut() {
                    open.text.push_str(&remaining[..take]);
                    open.last_event_id = env.event_id.clone();
                }
                remaining = &remaining[take..];
            }
            // `take == 0` means not even one character fits in what is left of
            // this streak; the flush below gives the next pass a full-width one,
            // where a character always fits (the cap is kibibytes and a
            // codepoint is at most four bytes), so this cannot spin.
            self.flush_open();
            self.open = Some(OpenStreak {
                role,
                feed_type,
                prompt_id: prompt_id.map(str::to_string),
                text: String::new(),
                first_ts: env.ts(),
                last_event_id: env.event_id.clone(),
            });
        }
    }

    /// Emit the open streak as a row, if there is one with anything in it.
    ///
    /// Public because the watcher (C3) flushes on silence and on `Down` — an
    /// open streak survives a silent resume and is flushed as a PARTIAL row
    /// when the subscription ends, so a half-finished turn is shown rather than
    /// lost. A reseed discards it instead ([`GxFold::reset`]).
    pub fn flush_open(&mut self) {
        let Some(open) = self.open.take() else { return };
        // **A whitespace-only streak is deliberately dropped.** A chunk of
        // `" \n"` followed by `turn_completed` produces no transcript row at
        // all, and that is intended: an all-whitespace assistant message is
        // noise, and an empty row in a transcript reads as a bug to whoever is
        // looking at it. Stated here because the behaviour is otherwise silent
        // and a later reader would take it for one.
        if open.text.trim().is_empty() {
            return;
        }
        self.out.push(RcFeedMessage {
            seq: 0,
            ts: open.first_ts,
            role: open.role.to_string(),
            msg_type: open.feed_type.to_string(),
            text: Some(open.text),
            tool: None,
            approval: None,
        });
    }

    /// Whether a streak is open — the watcher's "is there anything to flush
    /// after `flush_after` of silence" question.
    pub fn has_open_streak(&self) -> bool {
        self.open.is_some()
    }

    // ---- tools ----

    fn tool_call(&mut self, env: &GxEnvelope, update: &Value) {
        self.flush_open();
        self.activity = RcActivity::Working;

        let id = str_at(update, "toolCallId");
        let title = str_at(update, "title");
        let name = xai_tool_name(update).unwrap_or_else(|| title.clone());
        let detail = raw_input_detail(update.get("rawInput")).unwrap_or_else(|| title.clone());

        if !id.is_empty() {
            // A REPLAYED `tool_call` must not un-complete a tool that already
            // emitted its result. gx re-sends a call on re-attach and on
            // resume, so `call(X) → completed(X) → call(X) → completed(X)`
            // is ordinary traffic, not a contrived case — and resetting the
            // flag made it emit two `tool_result` rows for one X, against
            // §3.4's "one per toolCallId, repeats absorbed".
            let already_done = self.tools.get(&id).is_some_and(|s| s.terminal_emitted);
            self.remember_tool(
                id,
                ToolState {
                    name: name.clone(),
                    detail: detail.clone(),
                    terminal_emitted: already_done,
                },
            );
        }
        self.out.push(RcFeedMessage {
            seq: 0,
            ts: env.ts(),
            role: FEED_ROLE_TOOL.to_string(),
            msg_type: FEED_TYPE_TOOL_USE.to_string(),
            text: None,
            tool: Some(RcFeedTool {
                name: non_empty(&name),
                detail: non_empty(&detail),
            }),
            approval: None,
        });
    }

    fn tool_call_update(&mut self, env: &GxEnvelope, update: &Value) {
        self.activity = RcActivity::Working;
        let id = str_at(update, "toolCallId");

        // Both spellings, and the case gx actually uses differs between them:
        // the top-level `status` is lowercase ACP, `_meta.updateParams.status`
        // is PascalCase. Compared case-insensitively so neither has to be
        // guessed at a call site.
        let status = str_at(update, "status");
        let status = if status.is_empty() {
            env.params
                .meta
                .get("updateParams")
                .and_then(|u| u.get("status"))
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string()
        } else {
            status
        };
        let status = status.to_ascii_lowercase();

        let terminal = matches!(status.as_str(), "completed" | "failed");
        if !terminal {
            // State refresh only: the detail a later terminal update inherits.
            if let Some(state) = self.tools.get_mut(&id) {
                if let Some(d) = raw_input_detail(update.get("rawInput")) {
                    state.detail = d;
                }
            }
            return;
        }
        if self.tools.get(&id).is_some_and(|s| s.terminal_emitted) {
            // Repeats absorbed: gx re-sends terminal updates on re-attach and
            // on replay, and a row per repeat would duplicate the transcript.
            return;
        }
        self.flush_open();

        let name = self
            .tools
            .get(&id)
            .map(|s| s.name.clone())
            .filter(|n| !n.is_empty())
            .or_else(|| xai_tool_name(update))
            .unwrap_or_else(|| str_at(update, "title"));
        let detail = tool_result_detail(update.get("content"))
            .or_else(|| self.tools.get(&id).map(|s| s.detail.clone()))
            .unwrap_or_default();

        if let Some(state) = self.tools.get_mut(&id) {
            state.terminal_emitted = true;
        } else if !id.is_empty() {
            self.remember_tool(
                id,
                ToolState {
                    name: name.clone(),
                    detail: detail.clone(),
                    terminal_emitted: true,
                },
            );
        }

        self.out.push(RcFeedMessage {
            seq: 0,
            ts: env.ts(),
            role: FEED_ROLE_TOOL.to_string(),
            msg_type: FEED_TYPE_TOOL_RESULT.to_string(),
            text: (status == "failed").then(|| "failed".to_string()),
            tool: Some(RcFeedTool {
                name: non_empty(&name),
                detail: non_empty(&detail),
            }),
            approval: None,
        });
    }

    /// Record a tool's state, evicting the oldest COMPLETED tool when the map
    /// is over [`MAX_TOOLS`].
    ///
    /// A tool still in flight is never evicted: its name is what the terminal
    /// row is built from, and dropping it would emit a result row labelled with
    /// the update's `title`, which for gx is the whole rendered command.
    fn remember_tool(&mut self, id: String, state: ToolState) {
        if self.tools.insert(id.clone(), state).is_none() {
            self.tool_order.push_back(id);
        }
        while self.tools.len() > MAX_TOOLS {
            // Oldest-first, skipping anything still running.
            let Some(pos) = self
                .tool_order
                .iter()
                .position(|k| self.tools.get(k).is_some_and(|s| s.terminal_emitted))
            else {
                // Every remembered tool is still in flight. Refusing to evict
                // is the right answer: the cap is a bound on memory, not a
                // licence to lose a live call.
                break;
            };
            if let Some(k) = self.tool_order.remove(pos) {
                self.tools.remove(&k);
            }
        }
    }

    /// Record an approval, evicting the oldest NON-pending one when the map is
    /// over [`MAX_APPROVALS`].
    fn remember_approval(&mut self, approval: LaneApproval) {
        let id = approval.id.clone();
        if self.approvals.insert(id.clone(), approval).is_none() {
            self.approval_order.push_back(id);
        }
        while self.approvals.len() > MAX_APPROVALS {
            let Some(pos) = self.approval_order.iter().position(|k| {
                self.approvals
                    .get(k)
                    .is_some_and(|a| !a.status.is_pending())
            }) else {
                // Everything held is still waiting on the human. Never evict
                // one of those — an approval the panel would stop offering is
                // worse than a map slightly over its cap.
                break;
            };
            if let Some(k) = self.approval_order.remove(pos) {
                self.approvals.remove(&k);
            }
        }
    }

    // ---- turns ----

    fn turn_completed(&mut self, env: &GxEnvelope, update: &Value) {
        self.flush_open();
        self.activity = RcActivity::Idle;
        let stop = str_at(update, "stop_reason");
        // `end_turn` is the ordinary ending and says nothing a reader needs; any
        // other reason (a refusal, a cancel, a limit) is worth a row.
        if stop.is_empty() || stop == "end_turn" {
            return;
        }
        self.out.push(RcFeedMessage {
            seq: 0,
            ts: env.ts(),
            role: FEED_ROLE_SYSTEM.to_string(),
            msg_type: FEED_TYPE_STATUS.to_string(),
            text: Some(format!("turn completed ({stop})")),
            tool: None,
            approval: None,
        });
    }

    // ---- approvals ----

    /// Record an approval's current state.
    ///
    /// The **first sight of a pending approval leaves a trace in the
    /// transcript** — one `system`/`approval_request` row carrying the id and
    /// `pending` — so a reader scrolling back sees where the agent stopped to
    /// ask. Every later change rides [`shed_core::lane::LaneEvent::Approval`]
    /// only: rows are append-only, and a second row per status change would
    /// read as a second question.
    ///
    /// Returns `true` when this was the first sight (i.e. a row was emitted).
    pub fn note_approval(&mut self, approval: LaneApproval) -> bool {
        let first = !self.approvals.contains_key(&approval.id);
        let emit = first && approval.status.is_pending();
        if emit {
            self.out.push(RcFeedMessage {
                seq: 0,
                ts: approval
                    .created_at_unix_ms
                    .map(|ms| rfc3339_z(ms.div_euclid(1_000))),
                role: FEED_ROLE_SYSTEM.to_string(),
                msg_type: FEED_TYPE_APPROVAL_REQUEST.to_string(),
                text: Some(approval.title.clone()),
                tool: None,
                approval: Some(RcFeedApproval {
                    id: approval.id.clone(),
                    status: LaneApprovalStatus::Pending.as_str().to_string(),
                    decision: None,
                    decisions: Vec::new(),
                }),
            });
        }
        self.remember_approval(approval);
        emit
    }

    /// Every approval still waiting on the human.
    pub fn pending_approvals(&self) -> Vec<LaneApproval> {
        self.approvals
            .values()
            .filter(|a| a.status.is_pending())
            .cloned()
            .collect()
    }

    /// The approvals this fold holds, whatever their status — what a reconcile
    /// diffs against to find the ones a fresh `GET` no longer lists.
    pub fn held_approvals(&self) -> Vec<LaneApproval> {
        self.approvals.values().cloned().collect()
    }

    /// How many tool states are retained — the bound's observable.
    pub fn tools_len(&self) -> usize {
        self.tools.len()
    }

    /// How many approvals are retained — the bound's observable.
    pub fn approvals_len(&self) -> usize {
        self.approvals.len()
    }

    /// How many approvals are open. Overrides a session row's own count.
    pub fn open_approvals(&self) -> u32 {
        self.approvals
            .values()
            .filter(|a| a.status.is_pending())
            .count() as u32
    }

    // ---- output and cursors ----

    /// Take the rows folded so far. The open streak is deliberately NOT
    /// flushed: it is still growing, and a caller that wants it flushed says so
    /// ([`GxFold::flush_open`]).
    pub fn drain_messages(&mut self) -> Vec<RcFeedMessage> {
        std::mem::take(&mut self.out)
    }

    /// The work verdict, with the approval override applied: a fold holding an
    /// unanswered approval reports [`RcActivity::NeedsApproval`] whatever the
    /// last transcript frame said, because it knows something the roster poll
    /// does not.
    pub fn activity(&self) -> RcActivity {
        if self.open_approvals() > 0 {
            return RcActivity::NeedsApproval;
        }
        self.activity
    }

    /// The last id-bearing envelope folded — the positional mark.
    pub fn last_applied(&self) -> Option<&EventId> {
        self.last_applied.as_ref()
    }

    /// The **highest counter** seen, whole and opaque, for a `Last-Event-ID`
    /// header. See the module doc for why it is not `last_applied`.
    pub fn resume_cursor(&self) -> Option<&str> {
        self.max_seen.as_ref().map(|e| e.raw.as_str())
    }

    /// Reseed: discard everything, the open streak included.
    ///
    /// This is what a `Reset` … `Ready` bracket wraps. A SILENT resume does not
    /// call it — the ring, the open streak, the tools and the seen-set all
    /// survive, which is the whole point of correction 1.
    pub fn reset(&mut self) {
        self.open = None;
        self.tools.clear();
        self.tool_order.clear();
        self.approvals.clear();
        self.approval_order.clear();
        self.last_applied = None;
        self.max_seen = None;
        self.seen.clear();
        self.out.clear();
        self.activity = RcActivity::Unknown;
    }
}

// ---------------------------------------------------------------------------
// history cutting
// ---------------------------------------------------------------------------

/// Which `sessionUpdate` kinds the fold deliberately ignores.
///
/// Listed rather than defaulted so the set is greppable and a new gx extension
/// shows up in a diff. Anything not named here and not folded is ignored too —
/// an unknown kind is transparent, not an error.
pub fn is_ignored_kind(kind: &str) -> bool {
    matches!(
        kind,
        "hook_execution"
            | "session_recap"
            | "plan"
            | "available_commands_update"
            | "current_mode_update"
    )
}

/// Cut a history page at a cursor, **positionally**.
///
/// The counters are not monotonic in transcript order, so "everything with a
/// higher counter" would drop real rows and keep ones already seen. What is
/// well-defined is the cursor's POSITION: find the last envelope at or before it
/// and keep everything after that index, id-less envelopes included.
///
/// "At or before" prefers an exact id match; failing that, the last envelope
/// whose counter is `<= cursor`. `None` means the cursor was not located in this
/// page at all — the caller pages further back, or gives up and reseeds.
pub fn cut_at_cursor<'a>(page: &'a [GxEnvelope], cursor: &EventId) -> Option<&'a [GxEnvelope]> {
    Some(&page[cursor_index(page, cursor)? + 1..])
}

/// [`cut_at_cursor`]'s rule, as the INDEX it cuts at.
///
/// A caller holding an owned page splits it off at this index instead of
/// cloning the tail out of it — the hunt concatenates up to ten pages, and every
/// envelope carries two `serde_json::Value` trees, so the clone is the most
/// expensive thing in a cursor-bearing `history`.
pub fn cursor_index(page: &[GxEnvelope], cursor: &EventId) -> Option<usize> {
    let mut exact: Option<usize> = None;
    let mut at_or_before: Option<usize> = None;
    for (i, env) in page.iter().enumerate() {
        let Some(raw) = env.event_id.as_deref() else {
            continue;
        };
        // Full parse, PREFIX INCLUDED — not just the counter.
        //
        // A counter-only comparison let a FOREIGN id anchor the cut: gx's
        // counter is process-global, so another session's id sitting in the
        // page compares perfectly well against this session's cursor. Cursor
        // `S-5` over `[id-less, foreign-5, S-6]` then cut at the foreign
        // envelope and silently dropped the id-less one, instead of reporting
        // the cursor absent so the caller could page further back.
        let Some(id) = EventId::parse(raw) else {
            continue;
        };
        if !id.belongs_to(&cursor.prefix) {
            continue;
        }
        if id.raw == cursor.raw {
            exact = Some(i);
        }
        if id.counter <= cursor.counter {
            at_or_before = Some(i);
        }
    }
    exact.or(at_or_before)
}

/// The lowest counter in a page — what says whether paging further back is
/// needed. `None` when no envelope in the page carried an id.
pub fn min_counter(page: &[GxEnvelope]) -> Option<u64> {
    page.iter()
        .filter_map(|e| e.event_id.as_deref().and_then(counter_of))
        .min()
}

// ---------------------------------------------------------------------------
// approvals: the gx resource → LaneApproval
// ---------------------------------------------------------------------------

/// One entry of `GET /v1/sessions/{id}/approvals`, or the body of
/// `GET …/approvals/{toolCallId}`.
#[derive(Debug, Default, Deserialize)]
pub struct GxApprovalResource {
    #[serde(default, deserialize_with = "null_default")]
    pub id: String,
    #[serde(default, rename = "sessionId", deserialize_with = "null_default")]
    pub session_id: String,
    #[serde(default, deserialize_with = "null_default")]
    pub kind: String,
    #[serde(default, deserialize_with = "null_default")]
    pub method: String,
    #[serde(default, deserialize_with = "null_default")]
    pub status: String,
    /// Kept RAW so [`LaneApproval::request_json`] carries gx's own key order —
    /// a decoded `Value` would re-sort an object's keys.
    #[serde(default)]
    pub request: Option<Box<RawValue>>,
    #[serde(default, rename = "createdAt")]
    pub created_at: Option<i64>,
}

/// The gx approval resource, as the contract sees it.
///
/// Branching is on `kind`, and the two option lists are never both populated —
/// [`LaneApproval`]'s own doc is emphatic about that, because rendering the
/// wrong one yields an approval with no buttons.
///
/// **Option ids stay opaque.** gx's own fixtures pair `optionId: "allow-once"`
/// with `kind: "allow_once"`, which is the coincidence that makes id-sniffing
/// look correct; the semantic kind rides
/// [`LaneApprovalOption::kind`] and nothing here parses an id.
pub fn lane_approval(res: &GxApprovalResource) -> LaneApproval {
    let request: Option<&RawValue> = res.request.as_deref().filter(|r| r.get().trim() != "null");
    let request_value: Value = request
        .and_then(|r| serde_json::from_str(r.get()).ok())
        .unwrap_or(Value::Null);

    // The placeholder the lane creates from a `pending_interaction` before the
    // request itself arrives: no method, no request, and nothing to render but
    // the raw body and a Reject.
    let placeholder = request.is_none() && res.method.trim().is_empty();
    let kind = if placeholder {
        LaneApprovalKind::Other("placeholder".to_string())
    } else {
        LaneApprovalKind::from_wire(&res.kind)
    };

    let mut options: Vec<LaneApprovalOption> = Vec::new();
    let mut questions: Vec<LaneQuestion> = Vec::new();
    // Assigned by every arm below — declared without a value so a missing arm
    // is a compile error rather than a silently empty title.
    let title;
    let mut detail: Option<String> = None;

    match &kind {
        LaneApprovalKind::Permission => {
            let tool_call = request_value.get("toolCall");
            title = tool_call
                .map(|t| str_at(t, "title"))
                .filter(|t| !t.is_empty())
                .unwrap_or_else(|| res.id.clone());
            // The tool call's own `rawInput` first — that is where ACP puts it —
            // then the request's, for a producer that hoisted it.
            detail = tool_call
                .and_then(|t| raw_input_detail(t.get("rawInput")))
                .or_else(|| raw_input_detail(request_value.get("rawInput")));
            if let Some(list) = request_value.get("options").and_then(Value::as_array) {
                options = list
                    .iter()
                    .map(|o| LaneApprovalOption {
                        id: str_at(o, "optionId"),
                        label: str_at(o, "name"),
                        description: non_empty(&str_at(o, "description")),
                        kind: non_empty(&str_at(o, "kind")),
                    })
                    .collect();
            }
        }
        LaneApprovalKind::Question => {
            if let Some(list) = request_value.get("questions").and_then(Value::as_array) {
                questions = list.iter().map(lane_question).collect();
            }
            title = questions
                .first()
                .map(|q| q.question.clone())
                .filter(|q| !q.is_empty())
                .unwrap_or_else(|| res.id.clone());
        }
        LaneApprovalKind::PlanApproval => {
            title = "Approve plan".to_string();
            detail = non_empty(&str_at(&request_value, "planContent"));
            // gx takes a bare string outcome here, so these two ids ARE the
            // outcomes — the one place an id is not arbitrary, and it is still
            // round-tripped rather than parsed.
            options = vec![
                LaneApprovalOption {
                    id: "approved".to_string(),
                    label: "Approve".to_string(),
                    description: None,
                    kind: Some(option_kind::ALLOW_ONCE.to_string()),
                },
                LaneApprovalOption {
                    id: "cancelled".to_string(),
                    label: "Cancel".to_string(),
                    description: None,
                    kind: Some(option_kind::REJECT_ONCE.to_string()),
                },
            ];
        }
        // mcp_elicitation, an unrecognized kind and the placeholder all render
        // the raw request and offer Reject. Synthesizing options for a schema
        // this build cannot read would be inventing buttons.
        _ => {
            title = non_empty(&str_at(&request_value, "message"))
                .or_else(|| non_empty(&res.method))
                .unwrap_or_else(|| res.id.clone());
        }
    }

    LaneApproval {
        id: res.id.clone(),
        session_id: res.session_id.clone(),
        kind,
        status: LaneApprovalStatus::from_wire(&res.status),
        title: sanitize_feed_text(&title),
        detail: detail.map(|d| sanitize_feed_text(&d)),
        options,
        questions,
        // Compacted, never truncated: a client parses this, and half a JSON
        // document parses as nothing. The REST body cap is what bounds it.
        request_json: compact_json(request.map(RawValue::get).unwrap_or("null")),
        created_at_unix_ms: res.created_at,
    }
}

/// One `{question, options, multiSelect?, id?}` → [`LaneQuestion`].
///
/// **`id` is the question's TEXT**, not its `id` field, and that is not a
/// mistake: gx's own TUI files answers in a map keyed by
/// `q.question` (`ask_user_question`'s `answers.insert(q.question.clone(), …)`),
/// so an answer filed under `q1` is an answer the agent never reads. The
/// contract carries the key so a client can see it; the adapter is what uses it.
///
/// `custom` stays `false`: free text on gx is an "Other" answer plus a separate
/// annotations channel the contract cannot carry yet, and inviting typing the
/// agent will reject is worse than not offering it.
fn lane_question(q: &Value) -> LaneQuestion {
    let text = str_at(q, "question");
    LaneQuestion {
        id: non_empty(&text),
        header: String::new(),
        question: sanitize_feed_text(&text),
        options: q
            .get("options")
            .and_then(Value::as_array)
            .map(|list| {
                list.iter()
                    .map(|o| {
                        let label = str_at(o, "label");
                        LaneApprovalOption {
                            // The answer is a list of chosen LABELS, so the
                            // label is the id. Round-tripped, never parsed.
                            id: label.clone(),
                            label,
                            description: non_empty(&str_at(o, "description")),
                            // A question's answers carry no permission
                            // posture — "yes" is not `allow_once`.
                            kind: None,
                        }
                    })
                    .collect()
            })
            .unwrap_or_default(),
        multiple: q
            .get("multiSelect")
            .and_then(Value::as_bool)
            .unwrap_or(false),
        custom: false,
    }
}

// ---------------------------------------------------------------------------
// small readers
// ---------------------------------------------------------------------------

fn str_at(v: &Value, key: &str) -> String {
    v.get(key)
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string()
}

/// The largest index `<= n` that is a UTF-8 character boundary in `s`.
///
/// `str::floor_char_boundary` is unstable, and splitting a multi-byte codepoint
/// would panic on the slice — so the fold walks back to a safe index itself.
fn floor_char_boundary(s: &str, mut n: usize) -> usize {
    if n >= s.len() {
        return s.len();
    }
    while n > 0 && !s.is_char_boundary(n) {
        n -= 1;
    }
    n
}

fn non_empty(s: &str) -> Option<String> {
    (!s.trim().is_empty()).then(|| s.to_string())
}

/// `update._meta["x.ai/tool"].name` — gx's own name for a tool, which is
/// stabler than the `title` (a `tool_call_update`'s title is the whole rendered
/// command).
fn xai_tool_name(update: &Value) -> Option<String> {
    non_empty(&str_at(update.get("_meta")?.get("x.ai/tool")?, "name"))
}

/// A tool's human detail: the command it runs, else its description.
fn raw_input_detail(raw_input: Option<&Value>) -> Option<String> {
    let ri = raw_input?;
    non_empty(&str_at(ri, "command")).or_else(|| non_empty(&str_at(ri, "description")))
}

/// One chunk's `content` block as display text.
///
/// Non-text blocks become a marker rather than nothing: a turn that was one
/// image would otherwise fold to an empty streak and vanish.
fn content_text(content: Option<&Value>) -> String {
    let Some(c) = content else {
        return String::new();
    };
    if let Some(arr) = c.as_array() {
        return arr
            .iter()
            .map(|b| content_text(Some(b)))
            .collect::<Vec<_>>()
            .join("");
    }
    match c.get("type").and_then(Value::as_str).unwrap_or_default() {
        "text" => str_at(c, "text"),
        "image" => "[image]".to_string(),
        "resource" | "resource_link" => {
            let uri = non_empty(&str_at(c, "uri"))
                .or_else(|| c.get("resource").map(|r| str_at(r, "uri")))
                .unwrap_or_default();
            format!("[resource: {uri}]")
        }
        // A block kind this build has never heard of: name it and move on.
        other if !other.is_empty() => format!("[{other}]"),
        // No `type` at all — a bare `{"text": …}` from a producer one version
        // behind reads as text rather than as nothing.
        _ => str_at(c, "text"),
    }
}

/// A terminal `tool_call_update`'s detail: the first text block, else the diff's
/// path, else a bare terminal marker.
fn tool_result_detail(content: Option<&Value>) -> Option<String> {
    let arr = content?.as_array()?;
    for block in arr {
        // gx nests the ACP content block one level down: `{"type":"content",
        // "content":{"type":"text","text":…}}`.
        let inner = block.get("content").unwrap_or(block);
        match block
            .get("type")
            .and_then(Value::as_str)
            .unwrap_or_default()
        {
            "content" | "text" => {
                if let Some(t) = non_empty(&content_text(Some(inner))) {
                    return Some(t);
                }
            }
            "diff" => {
                let path = non_empty(&str_at(block, "path"))
                    .or_else(|| non_empty(&str_at(inner, "path")))
                    .unwrap_or_default();
                return Some(format!("[diff: {path}]"));
            }
            "terminal" => return Some("[terminal]".to_string()),
            _ => {}
        }
    }
    None
}

/// Renders raw JSON compact — whitespace outside string literals stripped, key
/// order and number spelling untouched. The same rule `shed-opencode`'s
/// `compact_json` applies, and for the same reason: a re-encoded `Value` would
/// re-sort keys and the request a client renders would stop matching the one gx
/// holds.
fn compact_json(raw: &str) -> String {
    if raw.trim().is_empty() {
        return "null".to_string();
    }
    if serde_json::from_str::<serde::de::IgnoredAny>(raw).is_err() {
        return raw.trim().to_string();
    }
    let mut out = String::with_capacity(raw.len());
    let mut in_str = false;
    let mut escaped = false;
    for c in raw.chars() {
        if in_str {
            out.push(c);
            if escaped {
                escaped = false;
            } else if c == '\\' {
                escaped = true;
            } else if c == '"' {
                in_str = false;
            }
            continue;
        }
        match c {
            '"' => {
                in_str = true;
                out.push(c);
            }
            ' ' | '\t' | '\n' | '\r' => {}
            _ => out.push(c),
        }
    }
    out
}

#[cfg(test)]
mod tests;
