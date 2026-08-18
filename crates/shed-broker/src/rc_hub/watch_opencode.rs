//! The opencode pure fold — a port of `internal/ext/rc/watch_opencode.go`.
//!
//! [`OpencodeFold`] folds an opencode session's `/event` envelope stream into
//! an activity verdict AND a normalized message feed, mirroring the codex
//! fold's shape. Unlike codex (a tailed append-only JSONL file) opencode is a
//! client/server model: the hub subscribes to the embedded HTTP+SSE server's
//! `/event` endpoint as a second client. This file is the PURE fold only — no
//! network/transport (that is the opencode watcher, H8) and no correlation.
//! The fold is SESSION-SCOPED: it assumes every envelope it is handed already
//! belongs to its session (the transport filters by sessionID before calling
//! `apply_line`), so the fold itself does NOT filter by sessionID.
//!
//! Wire shape (verified against opencode 1.18.4 source + a live `/event`
//! capture). Each `apply_line` receives one decoded SSE `data:` payload:
//! `{ "id": "evt_…", "type": "<dotted>", "properties": { … } }` (top-level id
//! ignored). The envelope types the fold reads:
//!
//! - `session.status {status:{type}}` — busy → working; idle → needs_input
//!   (settled); retry → working (keep last message).
//! - `session.idle {sessionID}` → needs_input (settled).
//! - `message.updated {info:{id,role,time:{created,completed}}}` — tracks
//!   messageID→role and completion so cached text/reasoning parts can be
//!   flushed (feed only — not activity).
//! - `message.part.updated {part,time}` — the part carries the FULL current
//!   snapshot (NOT a delta; `message.part.delta` is ignored): text/reasoning
//!   parts cache-and-emit under the terminal rule; tool parts track the
//!   pending set and emit tool_use/tool_result, keyed by callID.
//! - `permission.asked` → an OPEN APPROVAL: a tracked pending entry (which
//!   drives the verdict to needs_approval) plus an `approval_request` feed
//!   row. `permission.replied` closes the entry and appends the resolved row.
//! - `question.asked` / `question.replied` / `question.rejected` → a
//!   display-only `status` feed row, but an open question DOES count toward
//!   needs_approval. Questions never enter pending_approvals (their answer
//!   vocabulary is not the decision enum, so they are not addressable) —
//!   needs_approval with an empty pending_approvals is a legal state.
//!
//! Everything else — `message.part.removed`, `session.error`,
//! `session.created/updated`, `server.*`, `message.part.delta`,
//! step-start/step-finish, an unknown type/part/field, or an unparseable
//! line — is ignored: `apply_line` returns false and leaves state untouched.
//!
//! Feed emission is DEDUPED by (part identity, phase) so a reseed after an
//! SSE reconnect — which replays the same history WITHOUT resetting the
//! fold — can never emit a part twice. Timestamps: opencode times are
//! epoch-MILLIS integers, converted to UTC RFC3339 so seeded history keeps
//! its real ordering instead of being stamped with reconcile-now.

use std::collections::hash_map::Entry;
use std::collections::{HashMap, HashSet};

use serde::Deserialize;
use serde_json::value::RawValue;
use shed_core::rc::RcActivity;

use super::messages::{
    null_default, sanitize_last_message, trim_feed_text, FeedApproval, FeedMessage, FeedTool,
    APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS, APPROVAL_DECISION_DENY,
    APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED, FEED_ROLE_ASSISTANT, FEED_ROLE_SYSTEM,
    FEED_ROLE_TOOL, FEED_ROLE_USER, FEED_TYPE_APPROVAL_REQUEST, FEED_TYPE_REASONING,
    FEED_TYPE_STATUS, FEED_TYPE_TEXT, FEED_TYPE_TOOL_RESULT, FEED_TYPE_TOOL_USE,
};
use super::watch::{
    compact_json, first_non_empty, json_first_byte, null_string_vec, object_default, object_opt,
    raw_opt, vec_objects, ActivityFold, MessageProducer,
};

/// The type of the hub-synthesized envelope the REST seed pushes AFTER the
/// individual permission.asked/question.asked replays (`ocApprovalSeedType`,
/// `watch_opencode.go:115`): it carries the authoritative set of asks still
/// open on the server, which is what lets the fold retire an ask that was
/// answered while the stream was down. Namespaced under `shed.` because it is
/// ours, not opencode's.
pub const OC_APPROVAL_SEED_TYPE: &str = "shed.approval.seed";

// ---- parse structs (all fields optional; tolerant — the H5 null/shape
// discipline applies throughout) ----

/// The generic `{id,type,properties}` frame every `/event` payload shares
/// (`ocEnvelope`, `watch_opencode.go:71`). (Accepted delta, as on
/// `ClaudeLine`: duplicate JSON keys error here where Go last-wins — no real
/// producer emits them.)
#[derive(Debug, Default, Deserialize)]
struct OcEnvelope {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    /// Absent and `null` both fold with zero properties (Go's RawMessage
    /// no-op); a non-object errors the line.
    #[serde(default, deserialize_with = "object_opt")]
    properties: Option<OcProperties>,
}

/// The union of the properties fields the fold reads across event types
/// (`ocProperties`, `watch_opencode.go:77`).
#[derive(Debug, Default, Deserialize)]
struct OcProperties {
    /// Decoded-but-unused, exactly like Go's `SessionID` field (the transport
    /// filters by session before the fold ever sees the envelope): a
    /// wrong-typed `sessionID` must fail the line on both sides — the DECODE is
    /// the point (the underscore silences dead-code).
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    _session_id: String,
    /// session.status
    #[serde(default, deserialize_with = "object_opt")]
    status: Option<OcStatus>,
    /// message.updated
    #[serde(default, deserialize_with = "object_opt")]
    info: Option<OcMessageInfo>,
    /// message.part.updated
    #[serde(default, deserialize_with = "object_opt")]
    part: Option<OcPart>,
    /// message.part.updated update time (epoch ms)
    #[serde(default, deserialize_with = "null_default")]
    time: i64,
    /// permission.asked (per_…) / question.asked (que_…)
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    /// permission.asked kind ("bash", "edit", …)
    #[serde(default, deserialize_with = "null_default")]
    permission: String,
    /// permission.asked matched commands/globs
    #[serde(default, deserialize_with = "null_string_vec")]
    patterns: Vec<String>,
    /// permission.asked call detail (metadata.command)
    #[serde(default, deserialize_with = "object_opt")]
    metadata: Option<OcPermMetadata>,
    /// question.asked
    #[serde(default, deserialize_with = "vec_objects")]
    questions: Vec<OcQuestion>,

    /// The reply-event fields (permission.replied,
    /// question.replied/rejected): opencode addresses the resolved request by
    /// requestID and names its native reply ("once"|"always"|"reject"). `id`
    /// is accepted as a tolerant fallback address (see [`OcProperties::reply_target`]) — a
    /// missed resolution would strand an approval pending forever, which is
    /// the one failure mode worth being generous about. The question
    /// answers[] payload is deliberately NOT read: the fold only needs to
    /// know the question closed.
    #[serde(default, rename = "requestID", deserialize_with = "null_default")]
    request_id: String,
    #[serde(default, deserialize_with = "null_default")]
    reply: String,

    /// These four ride the hub-SYNTHESIZED approval-seed marker only
    /// ([`OC_APPROVAL_SEED_TYPE`], pushed by the REST seed). No opencode event
    /// carries them, and the live-stream routing whitelist does not admit the
    /// marker's type, so they can only ever arrive from our own seed path.
    /// Each `*_known` flag says its half's REST read succeeded — only then is
    /// that half's id list authoritative.
    #[serde(
        default,
        rename = "permissionIDs",
        deserialize_with = "null_string_vec"
    )]
    permission_ids: Vec<String>,
    #[serde(default, rename = "questionIDs", deserialize_with = "null_string_vec")]
    question_ids: Vec<String>,
    #[serde(
        default,
        rename = "permissionsKnown",
        deserialize_with = "null_default"
    )]
    permissions_known: bool,
    #[serde(default, rename = "questionsKnown", deserialize_with = "null_default")]
    questions_known: bool,
}

impl OcProperties {
    /// The approval id a replied/rejected event addresses (`replyTarget`,
    /// `watch_opencode.go:126`). opencode carries it as requestID; `id` is a
    /// tolerant fallback so a spelling difference cannot strand a pending
    /// entry (and with it, a stuck needs_approval verdict).
    fn reply_target(&self) -> &str {
        first_non_empty(&self.request_id, &self.id)
    }
}

/// permission.asked's metadata bag (`ocPermMetadata`, `watch_opencode.go:119`).
/// Only the command is read — it is the tool detail on the approval row.
#[derive(Debug, Default, Deserialize)]
struct OcPermMetadata {
    #[serde(default, deserialize_with = "null_default")]
    command: String,
}

/// session.status's status union, discriminated by type (busy|idle|retry) —
/// `ocStatus`, `watch_opencode.go:134`.
#[derive(Debug, Default, Deserialize)]
struct OcStatus {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
}

/// message.updated's info (`ocMessageInfo`, `watch_opencode.go:139`).
#[derive(Debug, Default, Deserialize)]
struct OcMessageInfo {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    /// user | assistant
    #[serde(default, deserialize_with = "null_default")]
    role: String,
    /// {created, completed}
    #[serde(default, deserialize_with = "object_default")]
    time: OcTime,
}

/// Carries both a message time ({created,completed}) and a part/tool time
/// ({start,end}); the unused pair stays zero for whichever shape is present
/// (`ocTime`, `watch_opencode.go:147`).
#[derive(Debug, Default, Deserialize)]
struct OcTime {
    #[serde(default, deserialize_with = "null_default")]
    created: i64,
    #[serde(default, deserialize_with = "null_default")]
    completed: i64,
    #[serde(default, deserialize_with = "null_default")]
    start: i64,
    #[serde(default, deserialize_with = "null_default")]
    end: i64,
}

/// A message.part.updated part — the full snapshot (`ocPart`,
/// `watch_opencode.go:155`).
#[derive(Debug, Default, Deserialize)]
struct OcPart {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, rename = "messageID", deserialize_with = "null_default")]
    message_id: String,
    /// text | reasoning | tool | step-start | step-finish | …
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    /// text / reasoning
    #[serde(default, deserialize_with = "null_default")]
    text: String,
    /// text / reasoning: {start,end}
    #[serde(default, deserialize_with = "object_default")]
    time: OcTime,
    #[serde(default, deserialize_with = "null_default")]
    synthetic: bool,
    #[serde(default, deserialize_with = "null_default")]
    ignored: bool,
    /// tool part: the tool NAME
    #[serde(default, deserialize_with = "null_default")]
    tool: String,
    /// tool part: the call id (dedup key)
    #[serde(default, rename = "callID", deserialize_with = "null_default")]
    call_id: String,
    /// tool part: the state union
    #[serde(default, deserialize_with = "object_opt")]
    state: Option<OcToolState>,
}

/// A tool part's state union, discriminated by status (`ocToolState`,
/// `watch_opencode.go:169`).
#[derive(Debug, Default, Deserialize)]
struct OcToolState {
    /// pending | running | completed | error
    #[serde(default, deserialize_with = "null_default")]
    status: String,
    /// invocation arguments (compact JSON → tool_use detail); raw so the
    /// compacted detail preserves the producer's key order.
    #[serde(default, deserialize_with = "raw_opt")]
    input: Option<Box<RawValue>>,
    /// completed result → tool_result detail
    #[serde(default, deserialize_with = "null_default")]
    output: String,
    /// error message → tool_result detail
    #[serde(default, deserialize_with = "null_default")]
    error: String,
    /// {start,end}
    #[serde(default, deserialize_with = "object_default")]
    time: OcTime,
}

/// One entry of question.asked's questions[] (`ocQuestion`,
/// `watch_opencode.go:178`).
#[derive(Debug, Default, Deserialize)]
struct OcQuestion {
    #[serde(default, deserialize_with = "null_default")]
    header: String,
    #[serde(default, deserialize_with = "null_default")]
    text: String,
}

// ---- cached parts ----

/// The latest snapshot of a text/reasoning part that has not yet been
/// emitted (`ocPartCache`, `watch_opencode.go:189`) — either because its
/// owning message's role is not known yet (parts can arrive before their
/// message.updated) or because its terminal condition (part.time.end /
/// message completed) has not been met. Dropped once the part emits.
#[derive(Debug, Default)]
struct OcPartCache {
    // (Go also carries partID here; the Rust ownership shape passes the id to
    // try_emit_part alongside the map key instead, so the field is elided.)
    message_id: String,
    /// "text" | "reasoning"
    kind: String,
    /// Latest snapshot text (snapshots go empty→full, never additive).
    text: String,
    /// The latest snapshot carried a part.time.end.
    has_end: bool,
    /// part.time.start (epoch ms)
    ts_start: i64,
    /// part.time.end (epoch ms)
    ts_end: i64,
    /// properties.time (epoch ms) — the update-time fallback
    upd_time: i64,
}

/// One permission ask (`ocPendingPerm`, `watch_opencode.go:236`): the summary
/// text it produced (so the resolved row can repeat it) plus its resolution
/// state. Keyed by the opencode request id (per_… — the approvals verb's
/// address); the ask's kind/detail ride the pending row only.
#[derive(Debug, Default)]
struct OcPendingPerm {
    /// The sanitized human-readable summary carried by both rows.
    text: String,
    resolved: bool,
    /// The contract decision that resolved it ("" when resolved outside the
    /// hub).
    decision: String,
}

/// Folds the `/event` envelope stream into activity + feed (`opencodeFold`,
/// `watch_opencode.go:203`). Holds cumulative state across `apply_line` calls
/// and is NOT safe for concurrent use (the owning watcher serializes access,
/// mirroring the fold contract).
#[derive(Debug, Default)]
pub struct OpencodeFold {
    /// Seen ≥1 activity-relevant event (session.status/idle or a tool part).
    confirmed: bool,
    /// Seen ≥1 LIVE session.status/session.idle boundary (REST status is a
    /// fallback).
    saw_status: bool,
    /// "busy" | "idle" | "" (retry maps to busy).
    last_boundary: String,
    /// Open tool-call ids (pending/running) — Go's `map[string]bool` set.
    pending: HashSet<String>,
    /// Latest assistant text (sanitized on read).
    last_msg: String,

    /// messageID → role (user|assistant).
    msg_role: HashMap<String, String>,
    /// messageID → time.created (epoch ms).
    msg_created: HashMap<String, i64>,
    /// messageID → time.completed (epoch ms; 0 == not completed).
    msg_completed: HashMap<String, i64>,

    /// partID → latest un-emitted text/reasoning snapshot.
    parts: HashMap<String, OcPartCache>,
    /// partID insertion order (deterministic flush ordering).
    part_order: Vec<String>,

    /// Dedup: "<id>|<phase>" already emitted (survives reseed + gap) — Go's
    /// `map[string]bool` set.
    emitted: HashSet<String>,
    /// Produced-but-undrained feed messages.
    msgs: Vec<FeedMessage>,

    /// Permission asks by their opencode id (per_…); `perm_order` keeps ask
    /// order for the pending_approvals snapshot. A RESOLVED entry is RETAINED
    /// (not deleted): the approvals verb must distinguish an id it never saw
    /// (404 unknown_approval) from one already answered (same-decision replay
    /// is idempotent, a different decision is 409 already_resolved) — see
    /// [`OpencodeFold::approval_state`]. Growth is bounded by the session's
    /// total asks, like the emitted dedup set.
    pending_perms: HashMap<String, OcPendingPerm>,
    perm_order: Vec<String>,
    /// The OPEN question.asked ids → their summary text. Questions count
    /// toward needs_approval but never enter pending_approvals (see the
    /// module doc).
    pending_questions: HashMap<String, String>,
}

impl MessageProducer for OpencodeFold {
    // `(*opencodeFold).drainMessages`, `watch_opencode.go:810`.
    fn drain_messages(&mut self) -> Vec<FeedMessage> {
        std::mem::take(&mut self.msgs)
    }
}

impl ActivityFold for OpencodeFold {
    /// Folds one decoded `{id,type,properties}` envelope
    /// (`(*opencodeFold).applyLine`, `watch_opencode.go:265`). Returns true
    /// when the envelope was recognized and folded (activity change, a
    /// cached/emitted feed part, or a role/completion update); an ignored
    /// family or an unparseable line returns false and leaves state
    /// untouched.
    fn apply_line(&mut self, line: &[u8]) -> bool {
        // Top-level object gate (the H5 shape rule: Go rejects the seq form,
        // and a top-level null no-ops to the zero envelope whose empty type
        // returns false).
        if json_first_byte(line) != Some(b'{') {
            return false;
        }
        let Ok(env) = serde_json::from_slice::<OcEnvelope>(line) else {
            return false;
        };
        if env.typ.is_empty() {
            return false;
        }
        let props = env.properties.unwrap_or_default();

        match env.typ.as_str() {
            "session.status" => {
                let Some(status) = &props.status else {
                    return false;
                };
                match status.typ.as_str() {
                    // retry keeps working (and keeps the prior last message)
                    "busy" | "retry" => {
                        self.confirmed = true;
                        self.saw_status = true;
                        self.last_boundary = "busy".into();
                        true
                    }
                    "idle" => {
                        self.confirmed = true;
                        self.saw_status = true;
                        self.last_boundary = "idle".into();
                        true
                    }
                    _ => false,
                }
            }
            "session.idle" => {
                self.confirmed = true;
                self.saw_status = true;
                self.last_boundary = "idle".into();
                true
            }
            "message.updated" => self.apply_message_updated(props.info.as_ref()),
            "message.part.updated" => {
                let Some(part) = &props.part else {
                    return false;
                };
                match part.typ.as_str() {
                    "text" | "reasoning" => self.apply_text_part(part, props.time),
                    "tool" => self.apply_tool_part(part, props.time),
                    _ => false, // step-start, step-finish, file, agent, subtask, snapshot, …
                }
            }
            "permission.asked" => self.apply_permission_asked(&props),
            "permission.replied" => {
                // Map opencode's native reply back onto the contract's
                // decision enum. An unrecognized/absent reply still CLOSES
                // the ask (with no decision): the request is demonstrably
                // answered, and a stuck entry would pin needs_approval
                // forever.
                self.resolve_permission(
                    props.reply_target(),
                    opencode_decision_from_reply(&props.reply),
                )
            }
            "question.asked" => self.apply_question_asked(&props),
            "question.replied" | "question.rejected" => self.clear_question(props.reply_target()),
            OC_APPROVAL_SEED_TYPE => self.apply_approval_seed(&props),
            _ => {
                // session.created/updated, session.error,
                // message.part.removed, message.part.delta, session.next.*,
                // server.*, catalog/plugin, and any unknown type: ignored.
                false
            }
        }
    }

    /// Clears ALL fold state (`reset`, `watch_opencode.go:893`). Exists to
    /// satisfy the fold contract (the file watchers call it on a
    /// truncation/rotation). The opencode SSE transport does NOT call reset
    /// on reconnect — it relies on the dedup set surviving so a re-seed
    /// emits no duplicate rows (a reconnect is a reseed, not a fresh
    /// session).
    fn reset(&mut self) {
        *self = OpencodeFold::default();
    }

    /// Drops the pending tool-call set (`noteGap`, `watch_opencode.go:885`) —
    /// a gap (an SSE inbox overflow / dropped frame) may have swallowed a
    /// completed/error part, and a forever-pending callID would pin the
    /// verdict at working. It KEEPS the emitted-part dedup set (and the
    /// cached snapshots) so reseed idempotency survives a gap. The
    /// open-approval state is deliberately KEPT too: an approval whose reply
    /// the gap may have swallowed is retired authoritatively by the reseed's
    /// approval-seed marker, so clearing it here would only blink
    /// needs_approval off and back on for genuinely-open asks.
    fn note_gap(&mut self) {
        self.pending.clear();
    }

    // `(*opencodeFold).activity`, `watch_opencode.go:819`.
    fn activity(&self) -> RcActivity {
        if !self.confirmed {
            return RcActivity::Unknown;
        }
        // needs_approval is checked BEFORE the pending-tool arm: the tool
        // call that triggered the ask is still open (opencode holds it) so
        // the pending set says "working", but the session is blocked on the
        // operator, not on the model. The block is what the client must see.
        if self.open_approvals() > 0 {
            return RcActivity::NeedsApproval;
        }
        if !self.pending.is_empty() {
            return RcActivity::Working;
        }
        if self.last_boundary == "idle" {
            return RcActivity::NeedsInput;
        }
        RcActivity::Working
    }

    // `(*opencodeFold).lastMessage`, `watch_opencode.go:854`.
    fn last_message(&self) -> String {
        sanitize_last_message(&self.last_msg)
    }

    /// An EVENT-BOUNDED end state (`settled`, `watch_opencode.go:845`) — one
    /// that stays true until an event changes it, so the transport trusts it
    /// indefinitely while the stream is healthy instead of expiring it after
    /// the 30s window. needs_approval qualifies alongside needs_input: an
    /// open ask is cleared only by a replied/rejected event or a reseed,
    /// never by the passage of time. (A DEAD stream is the deliberate
    /// exception — an unhealthy transport reports not-fresh and pane
    /// stability drives, so a needs_approval derived from a wedged
    /// connection cannot outlive the evidence for it.)
    fn settled(&self) -> bool {
        matches!(
            self.activity(),
            RcActivity::NeedsInput | RcActivity::NeedsApproval
        )
    }
}

impl OpencodeFold {
    /// A fold with every map/slice empty (`newOpencodeFold`,
    /// `watch_opencode.go:242`).
    pub fn new() -> OpencodeFold {
        OpencodeFold::default()
    }

    /// Tracks a message's role + completion time (feed bookkeeping — it does
    /// NOT touch activity) and flushes any cached parts for that message now
    /// that its role / completion is known (`applyMessageUpdated`,
    /// `watch_opencode.go:341`). Returns true ONLY when it advanced state — a
    /// newly-known role, a newly-known completion, or a cached part it
    /// flushed. A repeated or id-only snapshot that changes nothing returns
    /// false (this path is feed-tracking, not activity, so a no-op must not
    /// count as an event).
    fn apply_message_updated(&mut self, info: Option<&OcMessageInfo>) -> bool {
        let Some(info) = info else {
            return false;
        };
        if info.id.is_empty() {
            return false;
        }
        let mut advanced = false;
        if !info.role.is_empty() && self.msg_role.get(&info.id) != Some(&info.role) {
            self.msg_role.insert(info.id.clone(), info.role.clone());
            advanced = true;
        }
        if info.time.created != 0 {
            self.msg_created.insert(info.id.clone(), info.time.created);
        }
        if info.time.completed != 0 && self.msg_completed.get(&info.id).copied().unwrap_or(0) == 0 {
            self.msg_completed
                .insert(info.id.clone(), info.time.completed);
            advanced = true;
        }
        for part_id in self.parts_for_message(&info.id) {
            if self.try_emit_part(&part_id) {
                advanced = true;
            }
        }
        advanced
    }

    /// Caches a text/reasoning part's latest snapshot and attempts to emit it
    /// (`applyTextPart`, `watch_opencode.go:373`).
    ///
    /// - A synthetic/ignored snapshot SUPPRESSES the part: any earlier cached
    ///   partial for the same partID is dropped so it can never be flushed on
    ///   message-completion.
    /// - A part with no messageID can never have its role resolved, so it is
    ///   never cached.
    /// - A part already emitted (its body dedup key is set — a reseed replay)
    ///   is not re-cached: re-appending it to part_order on every reconnect
    ///   would leak unboundedly.
    fn apply_text_part(&mut self, p: &OcPart, upd_time: i64) -> bool {
        if p.id.is_empty() {
            return false;
        }
        if p.synthetic || p.ignored {
            self.drop_part(&p.id); // suppressing snapshot: drop any cached partial
            return false;
        }
        if p.message_id.is_empty() {
            return false; // ownerless part: its role is unresolvable, so never cache it
        }
        if self.emitted.contains(&body_key(&p.id)) {
            return false; // already emitted (reseed replay): don't re-cache/re-append
        }
        // Go creates the cache entry (recording `kind` once) on first sight and
        // updates it in place thereafter.
        let ent = match self.parts.entry(p.id.clone()) {
            Entry::Occupied(e) => e.into_mut(),
            Entry::Vacant(v) => {
                self.part_order.push(p.id.clone());
                v.insert(OcPartCache {
                    kind: p.typ.clone(),
                    ..OcPartCache::default()
                })
            }
        };
        ent.message_id = p.message_id.clone();
        ent.text = p.text.clone();
        if p.time.start != 0 {
            ent.ts_start = p.time.start;
        }
        if p.time.end != 0 {
            ent.ts_end = p.time.end;
            ent.has_end = true;
        }
        ent.upd_time = upd_time;
        self.try_emit_part(&p.id);
        true
    }

    /// Emits a cached text/reasoning part if its role is known and its
    /// terminal condition is met (`tryEmitPart`, `watch_opencode.go:411`):
    /// user text emits immediately; assistant text/reasoning emits at
    /// part.time.end or message completion. Deduped by partID so a reseed
    /// re-fold is a no-op. Returns true only when it actually emitted a feed
    /// row.
    fn try_emit_part(&mut self, part_id: &str) -> bool {
        let key = body_key(part_id);
        if self.emitted.contains(&key) {
            self.drop_part(part_id);
            return false;
        }
        let Some(ent) = self.parts.get(part_id) else {
            return false;
        };
        let role: &str = self
            .msg_role
            .get(&ent.message_id)
            .map(String::as_str)
            .unwrap_or_default();
        if role.is_empty() {
            return false; // role not known yet: keep cached, flush on message.updated
        }
        // The feed contract carries only user/assistant/tool/system roles. A
        // text/reasoning part whose owning message role is neither user nor
        // assistant can never resolve to a valid feed row, so drop it rather
        // than hold it cached forever.
        if role != FEED_ROLE_USER && role != FEED_ROLE_ASSISTANT {
            self.drop_part(part_id);
            return false;
        }
        let typ = match ent.kind.as_str() {
            "text" => FEED_TYPE_TEXT,
            "reasoning" => FEED_TYPE_REASONING,
            _ => return false,
        };
        let msg_completed = self
            .msg_completed
            .get(&ent.message_id)
            .copied()
            .unwrap_or(0);
        let terminal = role == FEED_ROLE_USER || ent.has_end || msg_completed != 0;
        if !terminal {
            return false;
        }
        let feed_role = if ent.kind == "reasoning" {
            FEED_ROLE_ASSISTANT
        } else {
            role
        };
        let ts = opencode_ts(first_non_zero(&[
            ent.ts_end,
            ent.ts_start,
            ent.upd_time,
            msg_completed,
            self.msg_created.get(&ent.message_id).copied().unwrap_or(0),
        ]));
        let msg = FeedMessage {
            ts,
            role: feed_role.to_string(),
            typ: typ.to_string(),
            text: ent.text.clone(),
            ..FeedMessage::default()
        };
        let is_assistant_text = role == FEED_ROLE_ASSISTANT && ent.kind == "text";
        if self.emit_once(key, msg) {
            if is_assistant_text {
                // Go's `f.lastMsg = ent.text` immediately before dropPart: the
                // cached snapshot is about to go, so move its text out rather
                // than clone it a second time.
                if let Some(ent) = self.parts.get_mut(part_id) {
                    self.last_msg = std::mem::take(&mut ent.text);
                }
            }
            self.drop_part(part_id);
            return true;
        }
        false
    }

    /// Tracks a tool call's pending state and emits tool_use / tool_result
    /// rows, each once, keyed by callID (`applyToolPart`,
    /// `watch_opencode.go:460`). tool_use emits on the first
    /// running/completed/error snapshot carrying non-empty input; tool_result
    /// emits on completed (output) / error (error). A completed snapshot
    /// seen with neither yet emitted emits BOTH (tool_use then tool_result).
    fn apply_tool_part(&mut self, p: &OcPart, upd_time: i64) -> bool {
        if p.call_id.is_empty() {
            return false;
        }
        let Some(st) = &p.state else {
            return false;
        };
        match st.status.as_str() {
            "pending" | "running" => {
                self.pending.insert(p.call_id.clone());
            }
            "completed" | "error" => {
                self.pending.remove(&p.call_id);
            }
            _ => {
                // An unrecognized tool state is tolerantly ignored: it must
                // NOT confirm activity, mutate the pending set, or emit — a
                // bogus/unknown status is noise, not a call.
                return false;
            }
        }
        self.confirmed = true; // a recognized tool part is activity-relevant

        let detail = compact_json(st.input.as_deref().map(RawValue::get).unwrap_or_default());
        let has_input = !detail.is_empty() && detail != "{}" && detail != "null";
        if has_input && matches!(st.status.as_str(), "running" | "completed" | "error") {
            let ts = opencode_ts(first_non_zero(&[st.time.start, upd_time]));
            self.emit_once(
                format!("{}|use", p.call_id),
                FeedMessage {
                    ts,
                    role: FEED_ROLE_TOOL.into(),
                    typ: FEED_TYPE_TOOL_USE.into(),
                    tool: Some(FeedTool {
                        name: p.tool.clone(),
                        detail,
                    }),
                    ..FeedMessage::default()
                },
            );
        }
        if matches!(st.status.as_str(), "completed" | "error") {
            let result_detail = if st.status == "error" {
                st.error.clone()
            } else {
                st.output.clone()
            };
            let ts = opencode_ts(first_non_zero(&[st.time.end, st.time.start, upd_time]));
            self.emit_once(
                format!("{}|result", p.call_id),
                FeedMessage {
                    ts,
                    role: FEED_ROLE_TOOL.into(),
                    typ: FEED_TYPE_TOOL_RESULT.into(),
                    tool: Some(FeedTool {
                        name: p.tool.clone(),
                        detail: result_detail,
                    }),
                    ..FeedMessage::default()
                },
            );
        }
        true
    }

    // ---- approvals (permission asks + questions) ----

    /// Tracks a permission ask and emits its PENDING approval_request row
    /// (`applyPermissionAsked`, `watch_opencode.go:511`).
    ///
    /// An ask with NO id stays on the display-only status-row path: the id is
    /// both the row's wire address and the key a permission.replied clears,
    /// so an id-less ask could never be answered remotely NOR retired —
    /// tracking it would pin needs_approval forever on a session nothing can
    /// unblock.
    ///
    /// A re-ask of an id already tracked is usually a reseed replay and
    /// changes nothing — with ONE exception, the REOPEN rule: an entry
    /// resolved with an EMPTY decision was closed by the approval seed (or by
    /// a reply whose vocabulary we could not read), i.e. it was retired on
    /// the evidence "the server no longer lists it". A later ask for that
    /// same id is NEWER, stronger evidence that it IS open — a stale/racing
    /// REST snapshot retired it wrongly — so the entry is reopened and its
    /// rows re-announced (both dedup slots are cleared, since the client
    /// needs the pending row again to render the buttons). An entry resolved
    /// with a KNOWN decision stays closed: a real reply was observed for it,
    /// and no ask replay outranks that.
    fn apply_permission_asked(&mut self, props: &OcProperties) -> bool {
        // Require a meaningful permission kind: absent/null properties (or an
        // empty kind) must not fabricate a hollow "awaiting approval:" row.
        if props.permission.is_empty() {
            return false;
        }
        let mut text = format!("awaiting approval: {}", props.permission);
        if !props.patterns.is_empty() {
            text.push_str(" — ");
            text.push_str(&props.patterns.join(", "));
        }
        if props.id.is_empty() {
            self.emit_status_row("perm", "", &text);
            return true;
        }
        if let Some(ent) = self.pending_perms.get_mut(&props.id) {
            if !ent.resolved || !ent.decision.is_empty() {
                return false; // still open, or genuinely answered: a replay is not a state change
            }
            // REOPEN (see the doc): drop the resolution and both dedup slots
            // so the pending row is re-announced and a later real resolution
            // can emit its own row.
            ent.resolved = false;
            ent.text = text.clone();
            self.emitted
                .remove(&perm_key(&props.id, APPROVAL_STATUS_PENDING));
            self.emitted
                .remove(&perm_key(&props.id, APPROVAL_STATUS_RESOLVED));
        } else {
            self.pending_perms.insert(
                props.id.clone(),
                OcPendingPerm {
                    text: text.clone(),
                    ..OcPendingPerm::default()
                },
            );
            self.perm_order.push(props.id.clone());
        }
        let detail = props
            .metadata
            .as_ref()
            .map(|m| m.command.clone())
            .unwrap_or_default();
        // An open approval IS the activity verdict now (needs_approval), so —
        // unlike the old display-only row — an ask is activity-relevant
        // evidence and confirms the fold.
        self.confirmed = true;
        self.emit_once(
            perm_key(&props.id, APPROVAL_STATUS_PENDING),
            FeedMessage {
                role: FEED_ROLE_TOOL.into(),
                typ: FEED_TYPE_APPROVAL_REQUEST.into(),
                text,
                tool: Some(FeedTool {
                    name: props.permission.clone(),
                    detail,
                }),
                approval: Some(FeedApproval {
                    id: props.id.clone(),
                    status: APPROVAL_STATUS_PENDING.into(),
                    decisions: opencode_decisions(),
                    ..FeedApproval::default()
                }),
                ..FeedMessage::default()
            },
        );
        true
    }

    /// Marks an open ask resolved with `decision` (which may be "" when the
    /// answer happened outside the hub) and appends the RESOLVED approval row
    /// (`resolvePermission`, `watch_opencode.go:580`).
    ///
    /// IDEMPOTENT BY DESIGN, and that is load-bearing: the approvals verb
    /// marks an entry resolved synchronously the moment its POST succeeds (so
    /// a same-decision replay cannot re-POST), and opencode's own
    /// permission.replied event for the same id arrives on the stream moments
    /// later. Exactly one resolved row must reach the feed, so the second
    /// call is a no-op.
    ///
    /// A reply for an id this fold never saw asked (the ask predates the
    /// watcher, or a gap swallowed it) records a resolved TOMBSTONE rather
    /// than being dropped: without it a later ask replay would open a PENDING
    /// entry for a permission that is demonstrably answered, stranding the
    /// session at needs_approval. Its resolved row still goes out (the client
    /// folding rule explicitly allows a resolved row with no pending row
    /// before it).
    ///
    /// Accepted, not fixed: an entry the approvals verb resolved that a
    /// reconnect's GET /permission still lists stays resolved here — the ask
    /// replay's reopen rule does not apply to it because its decision is
    /// known. It self-heals the moment the TUI drops the request from its
    /// store.
    pub fn resolve_permission(&mut self, id: &str, decision: &str) -> bool {
        if id.is_empty() {
            return false;
        }
        match self.pending_perms.get(id) {
            None => {
                self.pending_perms.insert(
                    id.to_string(),
                    OcPendingPerm {
                        text: "approval resolved".into(),
                        ..OcPendingPerm::default()
                    },
                );
                self.perm_order.push(id.to_string());
            }
            Some(ent) if ent.resolved => return false,
            Some(_) => {}
        }
        let ent = self.pending_perms.get_mut(id).expect("present");
        ent.resolved = true;
        ent.decision = decision.to_string();
        let text = ent.text.clone();
        self.emit_once(
            perm_key(id, APPROVAL_STATUS_RESOLVED),
            FeedMessage {
                role: FEED_ROLE_TOOL.into(),
                typ: FEED_TYPE_APPROVAL_REQUEST.into(),
                text,
                approval: Some(FeedApproval {
                    id: id.to_string(),
                    status: APPROVAL_STATUS_RESOLVED.into(),
                    decision: decision.to_string(),
                    ..FeedApproval::default()
                }),
                ..FeedMessage::default()
            },
        );
        true
    }

    /// Emits the display-only status row for a question and, when the
    /// question is addressable (it carries an id), tracks it as open so it
    /// counts toward needs_approval (`applyQuestionAsked`,
    /// `watch_opencode.go:608`). An id-less question is display-only for the
    /// same reason an id-less permission is: nothing could ever clear it.
    fn apply_question_asked(&mut self, props: &OcProperties) -> bool {
        // Require at least one question with real text: no questions (or an
        // empty question) must not fabricate a hollow "awaiting answer:" row.
        if props.questions.is_empty() {
            return false;
        }
        let hdr = first_non_empty(&props.questions[0].header, &props.questions[0].text);
        if hdr.is_empty() {
            return false;
        }
        let text = format!("awaiting answer: {hdr}");
        self.emit_status_row("ques", &props.id, &text);
        if !props.id.is_empty() && !self.pending_questions.contains_key(&props.id) {
            self.pending_questions.insert(props.id.clone(), text);
            self.confirmed = true; // an open question is the needs_approval verdict
        }
        true
    }

    /// Retires an open question (question.replied / question.rejected) —
    /// `clearQuestion`, `watch_opencode.go:631`. No feed row: the ask's row
    /// was display-only, and questions carry no approval state to resolve.
    fn clear_question(&mut self, id: &str) -> bool {
        if id.is_empty() {
            return false;
        }
        self.pending_questions.remove(id).is_some()
    }

    /// Reconciles the fold's open asks against the REST seed's AUTHORITATIVE
    /// view of what is still open on the server (`applyApprovalSeed`,
    /// `watch_opencode.go:653`). Every reconnect reseeds, which is what makes
    /// this the SELF-HEALING path: a permission.replied or question.replied
    /// lost to a disconnect or an inbox gap leaves a locally-open entry the
    /// server no longer lists, and it is retired here — with NO decision, the
    /// answer was given in the TUI and the hub cannot know which way it went.
    ///
    /// The two halves carry INDEPENDENT authority (permissions_known /
    /// questions_known): a failed REST read must never be mistaken for
    /// "nothing is open", which would retire live approvals — but neither may
    /// it block the other's healing.
    fn apply_approval_seed(&mut self, props: &OcProperties) -> bool {
        let mut changed = false;
        if props.permissions_known {
            let open = id_set(&props.permission_ids);
            // The ids to retire are collected first (Go ranges over the
            // permOrder slice header, so nothing appended during the loop is
            // visited — and nothing is, since every permOrder id already has an
            // entry).
            let stale: Vec<String> = self
                .perm_order
                .iter()
                .filter(|id| !open.contains(id.as_str()))
                .cloned()
                .collect();
            for id in stale {
                // resolve_permission is the guard: an entry already resolved
                // reports false.
                if self.resolve_permission(&id, "") {
                    changed = true;
                }
            }
        }
        if props.questions_known {
            let open = id_set(&props.question_ids);
            let stale: Vec<String> = self
                .pending_questions
                .keys()
                .filter(|id| !open.contains(id.as_str()))
                .cloned()
                .collect();
            for id in stale {
                self.pending_questions.remove(&id);
                changed = true;
            }
        }
        changed
    }

    /// Counts what the session is BLOCKED on (`openApprovals`,
    /// `watch_opencode.go:687`): unresolved permission asks plus open
    /// questions. Both drive needs_approval; only the permissions are
    /// addressable.
    pub fn open_approvals(&self) -> usize {
        self.pending_questions.len()
            + self
                .pending_perms
                .values()
                .filter(|ent| !ent.resolved)
                .count()
    }

    /// A tracked ask's status/decision (`approvalState`,
    /// `watch_opencode.go:700`). `None` means the id is unknown to this
    /// fold — the approvals verb's 404 unknown_approval. This, NOT the
    /// pending_approvals snapshot, is the resolution-state oracle: the
    /// snapshot is pending-only by wire contract.
    pub fn approval_state(&self, id: &str) -> Option<(String, String)> {
        let ent = self.pending_perms.get(id)?;
        if ent.resolved {
            Some((APPROVAL_STATUS_RESOLVED.to_string(), ent.decision.clone()))
        } else {
            Some((APPROVAL_STATUS_PENDING.to_string(), String::new()))
        }
    }

    /// Renders the still-open permission asks as the wire's pending_approvals
    /// snapshot, in ask order (`pendingApprovals`, `watch_opencode.go:714`).
    /// Freshly allocated on every call (the decisions vec included) so a
    /// consumer can hand it straight to the session DTO without aliasing fold
    /// state.
    pub fn pending_approvals(&self) -> Vec<FeedApproval> {
        let mut out = Vec::new();
        for id in &self.perm_order {
            let Some(ent) = self.pending_perms.get(id) else {
                continue;
            };
            if ent.resolved {
                continue;
            }
            out.push(FeedApproval {
                id: id.clone(),
                status: APPROVAL_STATUS_PENDING.into(),
                decisions: opencode_decisions(),
                ..FeedApproval::default()
            });
        }
        out
    }

    /// Applies a REST-seed-derived status boundary as a FALLBACK
    /// (`applyStatusFallback`, `watch_opencode.go:862`): it establishes the
    /// activity boundary ONLY when the fold has NOT observed a live
    /// session.status/session.idle event. The live `/event` stream is
    /// authoritative and ordered; the REST /session/status snapshot is only
    /// for reconstructing a session that was quiescent at connect — it must
    /// never override a newer buffered live busy/idle. Returns whether it
    /// actually set the boundary (so the caller can count it as a seed
    /// event).
    pub fn apply_status_fallback(&mut self, idle: bool) -> bool {
        if self.saw_status {
            return false; // a live status boundary was present: the live stream wins
        }
        self.confirmed = true;
        self.last_boundary = if idle { "idle" } else { "busy" }.into();
        true
    }

    /// Queues a feed message unless its dedup key was already emitted;
    /// empty-text non-tool messages are dropped (and do NOT consume the
    /// dedup slot, so a later non-empty snapshot of the same part can still
    /// emit) — `emitOnce`, `watch_opencode.go:761`.
    /// (Go's `emitOnce(key string, …)` takes the key by value; so does this —
    /// every caller builds a fresh key string, so the dedup set can take
    /// ownership instead of re-allocating it.)
    fn emit_once(&mut self, key: String, m: FeedMessage) -> bool {
        if !key.is_empty() && self.emitted.contains(&key) {
            return false;
        }
        if m.tool.is_none() && trim_feed_text(&m.text).is_empty() {
            return false;
        }
        if !key.is_empty() {
            self.emitted.insert(key);
        }
        self.msgs.push(m);
        true
    }

    /// Queues a display-only system/status feed row (permission.asked /
    /// question.asked), deduped by request id (`emitStatusRow`,
    /// `watch_opencode.go:780`). When the wire carries no id, the dedup key
    /// is derived from the row's CONTENT instead — otherwise a reseed replay
    /// (which has no id to key on) would emit an identical status row on
    /// every reconnect. The row is informational: it never changes the
    /// activity verdict.
    fn emit_status_row(&mut self, prefix: &str, id: &str, text: &str) {
        let key = if id.is_empty() {
            format!("{prefix}|text:{text}")
        } else {
            format!("{prefix}|id:{id}")
        };
        self.emit_once(
            key,
            FeedMessage {
                role: FEED_ROLE_SYSTEM.into(),
                typ: FEED_TYPE_STATUS.into(),
                text: text.to_string(),
                ..FeedMessage::default()
            },
        );
    }

    /// The cached part ids owned by `msg_id` in insertion order
    /// (`partsForMessage`, `watch_opencode.go:792` — deterministic flush
    /// ordering; map iteration order is not stable in Go either).
    fn parts_for_message(&self, msg_id: &str) -> Vec<String> {
        self.part_order
            .iter()
            .filter(|id| {
                self.parts
                    .get(*id)
                    .is_some_and(|ent| ent.message_id == msg_id)
            })
            .cloned()
            .collect()
    }

    /// Removes an emitted/duplicate cached part from the parts map
    /// (`dropPart`, `watch_opencode.go:805`). Its part_order slot is left
    /// behind and skipped by later reads; part_order growth is bounded by
    /// the session's total parts, like the emitted dedup set.
    fn drop_part(&mut self, part_id: &str) {
        self.parts.remove(part_id);
    }

    // ---- test-visible accessors (the Go tests read the fields directly) ----

    #[cfg(test)]
    pub(crate) fn pending_len(&self) -> usize {
        self.pending.len()
    }

    #[cfg(test)]
    pub(crate) fn parts_len(&self) -> usize {
        self.parts.len()
    }

    #[cfg(test)]
    pub(crate) fn part_order_len(&self) -> usize {
        self.part_order.len()
    }

    #[cfg(test)]
    pub(crate) fn pending_questions_len(&self) -> usize {
        self.pending_questions.len()
    }
}

/// Lifts an id list to a membership set (`idSet`, `watch_opencode.go:677`).
fn id_set(ids: &[String]) -> HashSet<&str> {
    ids.iter().map(String::as_str).collect()
}

/// The decision set opencode's session-scoped reply route honors
/// (once/always/reject), in contract spelling (`opencodeDecisions`,
/// `watch_opencode.go:733`). A FRESH vec per call: every copy of it ends up
/// on the wire in a row or a snapshot that must not alias another's.
fn opencode_decisions() -> Vec<String> {
    vec![
        APPROVAL_DECISION_ALLOW.into(),
        APPROVAL_DECISION_ALLOW_ALWAYS.into(),
        APPROVAL_DECISION_DENY.into(),
    ]
}

/// The emit/dedup key for one phase (pending|resolved) of an approval row
/// (`permKey`, `watch_opencode.go:739`). Distinct from emit_status_row's
/// "perm|id:<id>" key so the two paths can never collide.
fn perm_key(id: &str, phase: &str) -> String {
    format!("perm|id:{id}|{phase}")
}

/// Maps opencode's native permission reply vocabulary onto the contract's
/// decision enum (`opencodeDecisionFromReply`, `watch_opencode.go:745`) — the
/// inverse of what the approvals verb sends. An unrecognized/absent reply
/// maps to "" — "resolved, decision unknown", which still closes the ask.
pub(crate) fn opencode_decision_from_reply(reply: &str) -> &'static str {
    match reply {
        "once" => APPROVAL_DECISION_ALLOW,
        "always" => APPROVAL_DECISION_ALLOW_ALWAYS,
        "reject" => APPROVAL_DECISION_DENY,
        _ => "",
    }
}

/// Converts an opencode epoch-millis timestamp to a UTC RFC3339 string, or ""
/// when there is no usable time — the ring then stamps it with reconcile-now
/// on append (`opencodeTS`, `watch_opencode.go:904`). A non-positive value,
/// or one whose expanded year falls outside RFC3339's 0001..9999 range,
/// yields "" — an out-of-range year formats as a non-RFC3339 expanded-year
/// string, which would corrupt the wire contract, so it is treated as "no
/// usable time" instead.
pub(crate) fn opencode_ts(ms: i64) -> String {
    if ms <= 0 {
        return String::new();
    }
    let Some(t) = chrono::DateTime::from_timestamp_millis(ms) else {
        return String::new();
    };
    let year = chrono::Datelike::year(&t);
    if !(1..=9999).contains(&year) {
        return String::new();
    }
    t.to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
}

/// The emit/dedup key for a text/reasoning part's body row (`bodyKey`,
/// `watch_opencode.go:917`). Shared by the cache (apply_text_part's
/// already-emitted skip) and the emitter (try_emit_part) so the two never
/// drift.
fn body_key(part_id: &str) -> String {
    format!("{part_id}|body")
}

/// The first non-zero value, 0 when all are (`firstNonZero`,
/// `watch_opencode.go:920`).
fn first_non_zero(vals: &[i64]) -> i64 {
    vals.iter().copied().find(|&v| v != 0).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::super::watch::test_support::fixture_lines;
    use super::*;

    fn fold_fixture() -> (OpencodeFold, Vec<Vec<u8>>) {
        (OpencodeFold::new(), fixture_lines("opencode_turn.jsonl"))
    }

    /// One expected drained feed row, mirroring the Go suite's
    /// `opencodeFeedRow` (`watch_test.go:147`).
    struct Row {
        role: &'static str,
        typ: &'static str,
        text_prefix: &'static str,
        tool_name: &'static str,
        detail_has: &'static str,
    }

    fn assert_rows(got: &[FeedMessage], want: &[Row]) {
        assert_eq!(got.len(), want.len(), "rows: {got:?}");
        for (i, w) in want.iter().enumerate() {
            let m = &got[i];
            assert_eq!(
                (m.role.as_str(), m.typ.as_str()),
                (w.role, w.typ),
                "row {i}"
            );
            if !w.text_prefix.is_empty() {
                assert!(
                    m.text.starts_with(w.text_prefix),
                    "row {i} text {:?}",
                    m.text
                );
            }
            if !w.tool_name.is_empty() {
                assert_eq!(
                    m.tool.as_ref().map(|t| t.name.as_str()),
                    Some(w.tool_name),
                    "row {i}"
                );
            }
            if !w.detail_has.is_empty() {
                assert!(
                    m.tool
                        .as_ref()
                        .is_some_and(|t| t.detail.contains(w.detail_has)),
                    "row {i} detail {:?}",
                    m.tool
                );
            }
        }
    }

    const FIXTURE_ROWS: [Row; 5] = [
        Row {
            role: FEED_ROLE_USER,
            typ: FEED_TYPE_TEXT,
            text_prefix: "Use the bash tool",
            tool_name: "",
            detail_has: "",
        },
        Row {
            role: FEED_ROLE_ASSISTANT,
            typ: FEED_TYPE_REASONING,
            text_prefix: "The user wants",
            tool_name: "",
            detail_has: "",
        },
        Row {
            role: FEED_ROLE_TOOL,
            typ: FEED_TYPE_TOOL_USE,
            text_prefix: "",
            tool_name: "bash",
            detail_has: "ls",
        },
        Row {
            role: FEED_ROLE_TOOL,
            typ: FEED_TYPE_TOOL_RESULT,
            text_prefix: "",
            tool_name: "bash",
            detail_has: "a.txt",
        },
        Row {
            role: FEED_ROLE_ASSISTANT,
            typ: FEED_TYPE_TEXT,
            text_prefix: "3 .txt files.",
            tool_name: "",
            detail_has: "",
        },
    ];

    // Mirrors TestOpencodeFoldFixtureArc (watch_test.go:154).
    #[test]
    fn fixture_arc() {
        let (mut f, lines) = fold_fixture();
        assert_eq!(f.activity(), RcActivity::Unknown, "initial verdict");

        let mut saw_working = false;
        for ln in &lines {
            f.apply_line(ln);
            saw_working = saw_working || f.activity() == RcActivity::Working;
        }
        assert!(saw_working, "expected working during the turn");
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "session.idle settles the arc"
        );
        assert!(f.settled());
        assert_eq!(f.last_message(), "3 .txt files.");

        let got = f.drain_messages();
        assert_rows(&got, &FIXTURE_ROWS);

        // Every row carried a source time: TS non-empty and chronological
        // (RFC3339 sorts lexicographically; equal-second rows allowed).
        let mut prev = String::new();
        for (i, m) in got.iter().enumerate() {
            assert!(!m.ts.is_empty(), "row {i} needs a source-derived TS");
            assert!(m.ts >= prev, "row {i} TS {:?} before {:?}", m.ts, prev);
            prev = m.ts.clone();
        }
    }

    // Mirrors TestOpencodeFoldReseedIdempotent (watch_test.go:246): a
    // reconnect re-seeds the same history WITHOUT resetting the fold; the
    // partID/callID dedup makes the second fold emit ZERO new rows.
    #[test]
    fn reseed_idempotent() {
        let (mut f, lines) = fold_fixture();
        for ln in &lines {
            f.apply_line(ln);
        }
        assert_eq!(f.drain_messages().len(), 5, "first drain");
        for ln in &lines {
            f.apply_line(ln);
        }
        assert!(f.drain_messages().is_empty(), "reseed drain must be empty");
        assert_eq!(
            f.activity(),
            RcActivity::NeedsInput,
            "settled end-state after reseed"
        );
    }

    // Mirrors TestOpencodeFoldMultiSnapshot (watch_test.go:271): a partial
    // (no time.end) is cached, the full snapshot emits the complete text once.
    #[test]
    fn multi_snapshot_emits_once() {
        let mut f = OpencodeFold::new();
        f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"msgX","role":"assistant","time":{"created":1784613627000}}}}"#);
        f.apply_line(br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"prtX","messageID":"msgX","type":"text","text":"3 .txt","time":{"start":1784613627679}}},"time":1784613627679}"#);
        assert!(
            f.drain_messages().is_empty(),
            "partial (no time.end) must not emit"
        );
        f.apply_line(br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"prtX","messageID":"msgX","type":"text","text":"3 .txt files.","time":{"start":1784613627679,"end":1784613627681}}},"time":1784613627681}"#);
        let got = f.drain_messages();
        assert_eq!(got.len(), 1, "full snapshot emits once");
        assert_eq!(
            (
                got[0].role.as_str(),
                got[0].typ.as_str(),
                got[0].text.as_str()
            ),
            (FEED_ROLE_ASSISTANT, FEED_TYPE_TEXT, "3 .txt files.")
        );
        assert!(!got[0].ts.is_empty(), "TS from the part's time.end");
    }

    // Mirrors TestOpencodeFoldPermissionApprovalRow (watch_test.go:297).
    #[test]
    fn permission_approval_row() {
        let mut f = OpencodeFold::new();
        f.apply_line(
            br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#,
        );
        assert_eq!(f.activity(), RcActivity::Working);
        f.apply_line(br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"tool","tool":"bash","callID":"c1","state":{"status":"running","input":{"command":"rm -rf /tmp/x"}}}},"time":1784613621168}"#);
        assert_eq!(f.activity(), RcActivity::Working, "open tool call");

        f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["rm -rf /tmp/x","ls"],"metadata":{"command":"rm -rf /tmp/x"}}}"#);
        assert_eq!(
            f.activity(),
            RcActivity::NeedsApproval,
            "the ask outranks the open tool call"
        );
        assert!(f.settled(), "needs_approval is event-bounded settled");

        let got = f.drain_messages();
        assert_eq!(got.len(), 2, "tool_use + approval_request: {got:?}");
        let m = &got[1];
        assert_eq!(
            (m.role.as_str(), m.typ.as_str()),
            (FEED_ROLE_TOOL, FEED_TYPE_APPROVAL_REQUEST)
        );
        assert!(m.text.contains("awaiting approval: bash") && m.text.contains("rm -rf /tmp/x"));
        let tool = m.tool.as_ref().expect("tool block");
        assert_eq!(
            (tool.name.as_str(), tool.detail.as_str()),
            ("bash", "rm -rf /tmp/x")
        );
        let a = m.approval.as_ref().expect("approval block");
        assert_eq!(
            (a.id.as_str(), a.status.as_str(), a.decision.as_str()),
            ("per_1", APPROVAL_STATUS_PENDING, "")
        );
        assert_eq!(
            a.decisions,
            vec![
                APPROVAL_DECISION_ALLOW,
                APPROVAL_DECISION_ALLOW_ALWAYS,
                APPROVAL_DECISION_DENY
            ]
        );

        // The snapshot surface: pending-only, ask-ordered, state-queryable.
        let pend = f.pending_approvals();
        assert_eq!(pend.len(), 1);
        assert_eq!(
            (pend[0].id.as_str(), pend[0].status.as_str()),
            ("per_1", APPROVAL_STATUS_PENDING)
        );
        assert_eq!(
            f.approval_state("per_1"),
            Some((APPROVAL_STATUS_PENDING.to_string(), String::new()))
        );
        assert_eq!(
            f.approval_state("per_nope"),
            None,
            "unseen id reports unknown"
        );
    }

    // Mirrors TestOpencodeFoldPermissionReplied (watch_test.go:358).
    #[test]
    fn permission_replied() {
        let cases: [(&str, &str); 4] = [
            ("once", APPROVAL_DECISION_ALLOW),
            ("always", APPROVAL_DECISION_ALLOW_ALWAYS),
            ("reject", APPROVAL_DECISION_DENY),
            ("sideways", ""), // an unrecognized reply still closes the ask
        ];
        for (reply, decision) in cases {
            let mut f = OpencodeFold::new();
            f.apply_line(br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#);
            f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#);
            f.drain_messages();

            let line = format!(
                r#"{{"type":"permission.replied","properties":{{"sessionID":"s","requestID":"per_1","reply":"{reply}"}}}}"#
            );
            assert!(
                f.apply_line(line.as_bytes()),
                "reply {reply:?} must advance state"
            );
            assert_eq!(
                f.activity(),
                RcActivity::Working,
                "the ask no longer blocks"
            );
            assert!(f.pending_approvals().is_empty());
            assert_eq!(
                f.approval_state("per_1"),
                Some((APPROVAL_STATUS_RESOLVED.to_string(), decision.to_string())),
                "reply {reply:?}"
            );

            let got = f.drain_messages();
            assert_eq!(got.len(), 1, "one resolved row: {got:?}");
            let a = got[0].approval.as_ref().expect("approval block");
            assert_eq!(
                (a.id.as_str(), a.status.as_str(), a.decision.as_str()),
                ("per_1", APPROVAL_STATUS_RESOLVED, decision)
            );

            // Idempotent: a replayed reply is a no-op.
            assert!(
                !f.apply_line(line.as_bytes()),
                "replayed reply must not advance"
            );
            assert!(f.drain_messages().is_empty());
        }
    }

    // Mirrors TestOpencodeFoldResolveDedupAgainstLocalMark
    // (watch_test.go:417): the verb handler's local mark and opencode's own
    // permission.replied are two announcements of ONE resolution; the
    // tombstone path keeps a never-seen id closed.
    #[test]
    fn resolve_dedup_and_tombstone() {
        let mut f = OpencodeFold::new();
        f.apply_line(
            br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#,
        );
        f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#);
        f.drain_messages();

        assert!(
            f.resolve_permission("per_1", APPROVAL_DECISION_ALLOW),
            "local mark resolves"
        );
        assert!(
            !f.resolve_permission("per_1", APPROVAL_DECISION_ALLOW),
            "second mark is a no-op"
        );
        assert!(
            !f.apply_line(br#"{"type":"permission.replied","properties":{"sessionID":"s","requestID":"per_1","reply":"once"}}"#),
            "the stream's own event after a local mark must not advance"
        );
        let got = f.drain_messages();
        assert_eq!(got.len(), 1, "exactly one resolved row: {got:?}");
        assert_eq!(
            got[0].approval.as_ref().map(|a| a.status.as_str()),
            Some(APPROVAL_STATUS_RESOLVED)
        );

        // A reply for an id never seen asked leaves a resolved TOMBSTONE.
        assert!(f.resolve_permission("per_never_asked", APPROVAL_DECISION_DENY));
        assert_eq!(
            f.approval_state("per_never_asked"),
            Some((
                APPROVAL_STATUS_RESOLVED.to_string(),
                APPROVAL_DECISION_DENY.to_string()
            ))
        );
        let got = f.drain_messages();
        assert_eq!(got.len(), 1, "the tombstone's resolved row");
        assert_eq!(
            got[0].approval.as_ref().map(|a| a.status.as_str()),
            Some(APPROVAL_STATUS_RESOLVED)
        );
        // The ask finally replays (a reseed racing the reply): it must NOT
        // re-open the entry.
        assert!(!f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_never_asked","sessionID":"s","permission":"bash","patterns":["ls"]}}"#));
        assert!(
            f.pending_approvals().is_empty(),
            "the tombstone stays closed"
        );
        assert_eq!(
            f.open_approvals(),
            0,
            "a replied permission never blocks the session"
        );
    }

    // Mirrors TestOpencodeFoldQuestionBlocksWithoutPendingApproval
    // (watch_test.go:469).
    #[test]
    fn question_blocks_without_pending_approval() {
        for clear_type in ["question.replied", "question.rejected"] {
            let mut f = OpencodeFold::new();
            f.apply_line(br#"{"type":"session.idle","properties":{"sessionID":"s"}}"#);
            f.apply_line(br#"{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?","text":"pick one"}]}}"#);
            assert_eq!(f.activity(), RcActivity::NeedsApproval, "{clear_type}");
            assert!(
                f.pending_approvals().is_empty(),
                "questions are not addressable"
            );
            assert_eq!(
                f.approval_state("que_1"),
                None,
                "not resolvable via the verb"
            );
            let got = f.drain_messages();
            assert_eq!(got.len(), 1);
            assert_eq!(
                (got[0].role.as_str(), got[0].typ.as_str()),
                (FEED_ROLE_SYSTEM, FEED_TYPE_STATUS)
            );

            let line = format!(
                r#"{{"type":"{clear_type}","properties":{{"sessionID":"s","requestID":"que_1"}}}}"#
            );
            f.apply_line(line.as_bytes());
            assert_eq!(f.activity(), RcActivity::NeedsInput, "the block cleared");
            assert!(
                f.drain_messages().is_empty(),
                "clearing a question emits nothing"
            );
        }
    }

    // Mirrors TestOpencodeFoldApprovalSeedRetiresAnsweredAsks
    // (watch_test.go:504).
    #[test]
    fn approval_seed_retires_answered_asks() {
        let mut f = OpencodeFold::new();
        f.apply_line(
            br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#,
        );
        f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#);
        f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_2","sessionID":"s","permission":"edit","patterns":["a.go"]}}"#);
        f.apply_line(br#"{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?"}]}}"#);
        assert_eq!(f.drain_messages().len(), 3, "initial drain");

        // A reconnect replays the still-open asks, then the marker: per_2 +
        // que_1 remain open, per_1 was answered in the TUI meanwhile.
        f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_2","sessionID":"s","permission":"edit","patterns":["a.go"]}}"#);
        f.apply_line(br#"{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?"}]}}"#);
        let marker = format!(
            r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s","permissionIDs":["per_2"],"questionIDs":["que_1"],"permissionsKnown":true,"questionsKnown":true}}}}"#
        );
        assert!(f.apply_line(marker.as_bytes()), "the marker retired an ask");

        let pend = f.pending_approvals();
        assert_eq!(pend.len(), 1);
        assert_eq!(pend[0].id, "per_2", "only per_2 stays open");
        assert_eq!(
            f.approval_state("per_1"),
            Some((APPROVAL_STATUS_RESOLVED.to_string(), String::new())),
            "answered outside the hub: resolved with no decision"
        );
        let got = f.drain_messages();
        assert_eq!(got.len(), 1, "per_1 resolved; no duplicates: {got:?}");
        let a = got[0].approval.as_ref().expect("approval block");
        assert_eq!((a.id.as_str(), a.decision.as_str()), ("per_1", ""));

        // A marker listing nothing (both halves known) retires everything
        // still open, questions included.
        let empty = format!(
            r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s","permissionsKnown":true,"questionsKnown":true}}}}"#
        );
        f.apply_line(empty.as_bytes());
        assert_eq!(f.open_approvals(), 0);
        assert_eq!(f.activity(), RcActivity::Working, "nothing blocks any more");
    }

    // Mirrors TestOpencodeFoldSeedRetiredAskReopens (watch_test.go:1882): a
    // seed-retired ask (resolved, NO decision) is REOPENED by a later ask
    // replay; a genuinely answered one is not.
    #[test]
    fn seed_retired_ask_reopens() {
        let ask = br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#;
        let empty_seed = format!(
            r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s","permissionsKnown":true,"questionsKnown":true}}}}"#
        );

        let mut f = OpencodeFold::new();
        f.apply_line(
            br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#,
        );
        f.apply_line(ask);
        f.apply_line(empty_seed.as_bytes()); // a stale snapshot retires it
        assert_eq!(f.activity(), RcActivity::Working, "after the retiring seed");
        f.drain_messages();

        // The next reseed replays the ask: it is still open after all.
        assert!(
            f.apply_line(ask),
            "an ask replay for a seed-retired entry must reopen it"
        );
        assert_eq!(f.activity(), RcActivity::NeedsApproval);
        let pend = f.pending_approvals();
        assert_eq!(pend.len(), 1);
        assert_eq!(pend[0].id, "per_1", "per_1 open again");
        assert_eq!(
            f.approval_state("per_1"),
            Some((APPROVAL_STATUS_PENDING.to_string(), String::new()))
        );
        // The client needs the pending row again to render the buttons.
        let got = f.drain_messages();
        assert_eq!(got.len(), 1, "one re-announced pending row: {got:?}");
        let a = got[0].approval.as_ref().expect("approval");
        assert_eq!(a.status, APPROVAL_STATUS_PENDING);
        assert_eq!(a.decisions.len(), 3);

        // A real reply closes it; now an ask replay must NOT reopen it.
        f.apply_line(br#"{"type":"permission.replied","properties":{"sessionID":"s","requestID":"per_1","reply":"once"}}"#);
        f.drain_messages();
        assert!(!f.apply_line(ask), "a known-decision entry stays closed");
        assert_eq!(f.open_approvals(), 0);
    }

    // Mirrors TestOpencodeFoldApprovalSeedHalvesAreIndependent
    // (watch_test.go:1929).
    #[test]
    fn approval_seed_halves_are_independent() {
        let new_fold = || {
            let mut f = OpencodeFold::new();
            f.apply_line(br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#);
            f.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#);
            f.apply_line(br#"{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?"}]}}"#);
            f.drain_messages();
            f
        };

        // Permissions heal while the question read failed.
        let mut f = new_fold();
        let perms_only = format!(
            r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s","permissionsKnown":true}}}}"#
        );
        f.apply_line(perms_only.as_bytes());
        assert_eq!(
            f.approval_state("per_1").map(|(s, _)| s),
            Some(APPROVAL_STATUS_RESOLVED.to_string()),
            "its half's read succeeded"
        );
        assert_eq!(f.pending_questions_len(), 1, "the question is retained");
        assert_eq!(
            f.activity(),
            RcActivity::NeedsApproval,
            "the question still blocks"
        );

        // Questions heal while the permission read failed.
        let mut f = new_fold();
        let questions_only = format!(
            r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s","questionsKnown":true}}}}"#
        );
        f.apply_line(questions_only.as_bytes());
        assert_eq!(f.pending_questions_len(), 0, "its half's read succeeded");
        assert_eq!(
            f.approval_state("per_1").map(|(s, _)| s),
            Some(APPROVAL_STATUS_PENDING.to_string()),
            "a failed read is never 'nothing is open'"
        );

        // A marker with neither half known retires nothing.
        let mut f = new_fold();
        let hollow =
            format!(r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s"}}}}"#);
        assert!(
            !f.apply_line(hollow.as_bytes()),
            "no authority must not advance state"
        );
        assert_eq!(f.open_approvals(), 2, "both asks retained");
    }

    // Mirrors TestOpencodeFoldNoteGapKeepsDedup (watch_test.go:551).
    #[test]
    fn note_gap_keeps_dedup() {
        let (mut f, lines) = fold_fixture();
        for ln in &lines {
            f.apply_line(ln);
        }
        assert_eq!(f.drain_messages().len(), 5, "first drain");
        f.note_gap();
        for ln in &lines {
            f.apply_line(ln);
        }
        assert!(f.drain_messages().is_empty(), "dedup survives a gap");
    }

    // Mirrors TestOpencodeFoldIDLessAskStaysDisplayOnly (watch_test.go:576).
    #[test]
    fn id_less_ask_stays_display_only() {
        let mut f = OpencodeFold::new();
        let line = br#"{"type":"permission.asked","properties":{"sessionID":"s","permission":"bash","patterns":["ls"]}}"#;
        f.apply_line(line);
        f.apply_line(line); // reseed replay of the identical, id-less ask
        let got = f.drain_messages();
        assert_eq!(got.len(), 1, "content-keyed dedup: {got:?}");
        assert_eq!(
            (got[0].role.as_str(), got[0].typ.as_str()),
            (FEED_ROLE_SYSTEM, FEED_TYPE_STATUS)
        );
        assert!(
            got[0].approval.is_none(),
            "no approval block on an id-less ask"
        );
        assert_eq!(f.open_approvals(), 0, "an unclearable ask must not block");
        assert_eq!(
            f.activity(),
            RcActivity::Unknown,
            "a display-only ask confirms nothing"
        );
    }

    // Fix 2 mirror: TestOpencodeFoldUnknownToolStateIgnored
    // (watch_test.go:601).
    #[test]
    fn unknown_tool_state_ignored() {
        let mut f = OpencodeFold::new();
        let line = br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"tool","tool":"bash","callID":"c1","state":{"status":"bogus","input":{"command":"ls"}}}},"time":1784613621168}"#;
        assert!(
            !f.apply_line(line),
            "an unrecognized tool state must not advance"
        );
        assert_eq!(f.activity(), RcActivity::Unknown, "no confirm");
        assert_eq!(f.pending_len(), 0, "pending set untouched");
        assert!(f.drain_messages().is_empty());
    }

    // Fix 3 mirror: TestOpencodeFoldSyntheticSnapshotDropsCachedPartial
    // (watch_test.go:620).
    #[test]
    fn synthetic_snapshot_drops_cached_partial() {
        let mut f = OpencodeFold::new();
        f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000}}}}"#);
        f.apply_line(br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"text","text":"stale partial","time":{"start":1784613627679}}},"time":1784613627679}"#);
        assert!(f.drain_messages().is_empty(), "partial cached, not emitted");
        // A later SYNTHETIC snapshot for the SAME partID drops the partial.
        f.apply_line(br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"text","text":"stale partial","synthetic":true,"time":{"start":1784613627679}}},"time":1784613627680}"#);
        // The message completes — the dropped partial must NOT be flushed.
        f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000,"completed":1784613627684}}}}"#);
        assert!(
            f.drain_messages().is_empty(),
            "the stale partial never flushes"
        );
    }

    // Fix 4 mirror: TestOpencodeFoldEmptyAskNoRow (watch_test.go:640).
    #[test]
    fn empty_ask_no_row() {
        let cases: [&[u8]; 2] = [
            br#"{"type":"permission.asked","properties":{"sessionID":"s"}}"#,
            br#"{"type":"question.asked","properties":{"sessionID":"s"}}"#,
        ];
        for line in cases {
            let mut f = OpencodeFold::new();
            assert!(!f.apply_line(line), "a hollow ask must return false");
            assert!(f.drain_messages().is_empty());
            assert_eq!(f.open_approvals(), 0, "a hollow ask opens nothing");
        }
    }

    // Fix 5 mirror: TestOpencodeFoldUnknownMessageRoleNotEmitted
    // (watch_test.go:666).
    #[test]
    fn unknown_message_role_not_emitted() {
        let mut f = OpencodeFold::new();
        f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"bogus","time":{"created":1784613627000}}}}"#);
        f.apply_line(br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"text","text":"should not emit","time":{"start":1784613627679,"end":1784613627681}}},"time":1784613627681}"#);
        f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"bogus","time":{"created":1784613627000,"completed":1784613627684}}}}"#);
        assert!(
            f.drain_messages().is_empty(),
            "non-user/assistant parts never emit"
        );
    }

    // Fix 6 mirror: TestOpencodeFoldOwnerlessPartNotCached
    // (watch_test.go:678).
    #[test]
    fn ownerless_part_not_cached() {
        let mut f = OpencodeFold::new();
        let line = br#"{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","type":"text","text":"orphan","time":{"start":1784613627679,"end":1784613627681}}},"time":1784613627681}"#;
        assert!(!f.apply_line(line), "no messageID: never role-resolvable");
        assert_eq!((f.parts_len(), f.part_order_len()), (0, 0), "not cached");
        assert!(f.drain_messages().is_empty());
    }

    // Fix 7 mirror: TestOpencodeFoldReseedDoesNotGrowCache
    // (watch_test.go:694).
    #[test]
    fn reseed_does_not_grow_cache() {
        let (mut f, lines) = fold_fixture();
        for ln in &lines {
            f.apply_line(ln);
        }
        f.drain_messages();
        let order_after_first = f.part_order_len();
        let parts_after_first = f.parts_len();
        assert!(order_after_first > 0, "precondition: parts were appended");
        for ln in &lines {
            f.apply_line(ln);
        }
        assert!(f.drain_messages().is_empty(), "reseed emits nothing");
        assert_eq!(
            f.part_order_len(),
            order_after_first,
            "part_order must not grow"
        );
        assert_eq!(
            f.parts_len(),
            parts_after_first,
            "parts cache must not grow"
        );
    }

    // Fix 8 mirror: TestOpencodeTSOutOfRange (watch_test.go:723).
    #[test]
    fn opencode_ts_out_of_range() {
        assert_eq!(opencode_ts(1 << 62), "", "year out of RFC3339 range");
        assert_eq!(opencode_ts(0), "");
        assert_eq!(opencode_ts(-5), "");
        assert_eq!(opencode_ts(1784613627681), "2026-07-21T06:00:27Z");
    }

    // Fix 9 mirror: TestOpencodeFoldNoOpMessageUpdatedReturnsFalse
    // (watch_test.go:750).
    #[test]
    fn no_op_message_updated_returns_false() {
        let mut f = OpencodeFold::new();
        assert!(f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000}}}}"#));
        assert!(
            !f.apply_line(br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000}}}}"#),
            "a repeated message.updated must return false"
        );
        assert!(
            !f.apply_line(
                br#"{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m2"}}}"#
            ),
            "an id-only frame is not activity-relevant"
        );
    }

    // The opencode arm of TestFoldsToleratePathologicalLines
    // (watch_test.go:768) + the H5 shape rules.
    #[test]
    fn tolerates_pathological_lines() {
        let mut f = OpencodeFold::new();
        let bad: [&[u8]; 8] = [
            b"not json at all",
            br#"{"type":"#,
            br#"{"type":"totally_unknown_type"}"#,
            b"{}",
            b"",
            br#"{"type":"event_msg","payload":{"type":"token_count"}}"#,
            b"null",
            br#"["session.idle",{"sessionID":"s"}]"#,
        ];
        for b in bad {
            assert!(!f.apply_line(b), "line {:?} should not advance state", b);
        }
        assert_eq!(f.activity(), RcActivity::Unknown, "after only-noise");
        // Null-tolerance: null-valued fields no-op like Go.
        assert!(f.apply_line(
            br#"{"type":"session.idle","properties":{"sessionID":null,"status":null}}"#
        ));
        assert_eq!(f.activity(), RcActivity::NeedsInput);
        // A null VALUE-TYPED nested struct (`time`) folds to the zero value
        // (object_default), while a non-object shape errors the line — Go's
        // `json.Unmarshal` into `ocTime` does exactly this.
        assert!(f.apply_line(
            br#"{"type":"message.updated","properties":{"info":{"id":"m1","role":"assistant","time":null}}}"#
        ));
        assert!(!f.apply_line(
            br#"{"type":"message.updated","properties":{"info":{"id":"m2","role":"assistant","time":[1,2]}}}"#
        ));
    }

    // The null-ELEMENT pin (H6 review, HIGH): Go's null-is-a-no-op applies
    // at array-element level — a null pattern/question/seed-id must not drop
    // the line (the ask arms would silently lose an open approval and the
    // seed's self-healing retire).
    #[test]
    fn null_array_elements_tolerated() {
        let mut f = OpencodeFold::new();
        assert!(f.apply_line(
            br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["a",null]}}"#
        ));
        assert_eq!(
            f.activity(),
            RcActivity::NeedsApproval,
            "the ask still tracks"
        );
        assert_eq!(f.pending_approvals().len(), 1);

        let mut f2 = OpencodeFold::new();
        assert!(f2.apply_line(
            br#"{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"H"},null]}}"#
        ));
        assert_eq!(f2.activity(), RcActivity::NeedsApproval);

        // A null seed id must not disable the self-healing retire.
        let mut f3 = OpencodeFold::new();
        f3.apply_line(
            br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#,
        );
        f3.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}"#);
        f3.apply_line(br#"{"type":"permission.asked","properties":{"id":"per_2","sessionID":"s","permission":"edit","patterns":["a.go"]}}"#);
        f3.drain_messages();
        let seed = format!(
            r#"{{"type":"{OC_APPROVAL_SEED_TYPE}","properties":{{"sessionID":"s","permissionIDs":["per_1",null],"permissionsKnown":true}}}}"#
        );
        assert!(
            f3.apply_line(seed.as_bytes()),
            "the seed still retires per_2"
        );
        assert_eq!(f3.pending_approvals().len(), 1);
        assert_eq!(f3.pending_approvals()[0].id, "per_1");
    }

    // apply_status_fallback (watch_opencode.go:862): REST status is a
    // fallback only — a live boundary wins.
    #[test]
    fn status_fallback_yields_to_live() {
        let mut f = OpencodeFold::new();
        assert!(
            f.apply_status_fallback(true),
            "no live boundary yet: fallback applies"
        );
        assert_eq!(f.activity(), RcActivity::NeedsInput);

        let mut f2 = OpencodeFold::new();
        f2.apply_line(
            br#"{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}"#,
        );
        assert!(
            !f2.apply_status_fallback(true),
            "a live boundary was present: live wins"
        );
        assert_eq!(f2.activity(), RcActivity::Working);
    }
}
