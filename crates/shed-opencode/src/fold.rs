//! The opencode pure fold — the lane's port of the rc hub's `OpencodeFold`
//! (`shed-broker/src/rc_hub/watch_opencode.rs`, itself a port of
//! `internal/ext/rc/watch_opencode.go`).
//!
//! [`OpencodeFold`] folds an opencode session's `/event` envelope stream into an
//! activity verdict, a normalized transcript ([`RcFeedMessage`] rows) and the
//! open-approval state ([`LaneApproval`]). This file is the PURE fold only — no
//! network, no transport, no reconnect loop (those are the watcher's, C3).
//!
//! The fold is SESSION-SCOPED: it assumes every envelope handed to it already
//! belongs to its session (the watcher filters by `sessionID` before calling
//! [`OpencodeFold::apply_line`]), so the fold itself does NOT filter. In the
//! roost model the session id **arrives by construction** — the roost tab
//! reports it — so unlike the hub there is nothing to correlate, discover or
//! claim: the lane never searches for its session, it scopes to it. The hub's
//! correlation machinery (`not_before`, `created_late_enough`, `dir_match_canon`,
//! `ClaimHolder`, `rest_find_candidate`, `root_pin_from_created`) is therefore
//! deliberately NOT ported.
//!
//! Wire shape (verified against opencode 1.18.25's schema + a live `/event`
//! capture at 1.17.15). Each `apply_line` receives one decoded SSE `data:`
//! payload: `{ "id": "evt_…", "type": "<dotted>", "properties": { … } }`
//! (top-level id ignored). The envelope types the fold reads:
//!
//! - `session.status {status:{type}}` — busy → working; idle → needs_input
//!   (settled); retry → working (keep last message).
//! - `session.idle {sessionID}` → needs_input (settled).
//! - `message.updated {info:{id,role,time:{created,completed}}}` — tracks
//!   messageID→role and completion so cached text/reasoning parts can be
//!   flushed (feed only — not activity).
//! - `message.part.updated {part,time}` — the part carries the FULL current
//!   snapshot (NOT a delta; `message.part.delta` is ignored): text/reasoning
//!   parts cache-and-emit under the terminal rule; tool parts track the pending
//!   set and emit tool_use/tool_result, keyed by callID.
//! - `permission.asked` → an OPEN APPROVAL: a tracked entry (which drives the
//!   verdict to needs_approval), a [`LaneApproval`] of kind
//!   [`LaneApprovalKind::Permission`], and an `approval_request` feed row.
//!   `permission.replied` closes the entry and appends the resolved row.
//! - `question.asked` / `question.replied` / `question.rejected` → a
//!   display-only `status` feed row, and an open question counts toward
//!   needs_approval.
//! - `session.error` → a display-only `status` feed row so an errored turn is
//!   visible.
//!
//! Everything else — `message.part.removed`, `session.created/updated`,
//! `server.*`, `message.part.delta`, step-start/step-finish, an unknown
//! type/part/field, or an unparseable line — is ignored: `apply_line` returns
//! false and leaves state untouched.
//!
//! Feed emission is DEDUPED by (part identity, phase) so a reseed after a
//! reconnect — which replays the same history without resetting the fold — can
//! never emit a part twice. Timestamps: opencode times are epoch-MILLIS
//! integers, converted to UTC RFC3339 so seeded history keeps its real ordering
//! instead of being stamped with append-now.
//!
//! # What this port changes, deliberately, from the hub's fold
//!
//! Three things, all pinned by plan 015 §3.2 — everything else is behavior the
//! `fixtures/opencode_turn.golden.json` pin holds identical:
//!
//! 1. The hub's synthesized `shed.approval.seed` envelope becomes the method
//!    [`OpencodeFold::seed_approvals`]. It was only ever OUR marker, pushed onto
//!    our own stream by our own REST seed; a method says so honestly and cannot
//!    be spoofed by a producer frame.
//! 2. Questions materialize as [`LaneApproval`]s of kind
//!    [`LaneApprovalKind::Question`], carrying opencode's `QuestionInfo` in
//!    [`LaneApproval::questions`]. The hub could only count them (its
//!    `pending_approvals` snapshot was permissions-only, because its decision
//!    vocabulary had no way to express an answer); the lane contract does, so
//!    they are addressable now. Their FEED rows are unchanged — still the
//!    display-only `status` row the hub emitted.
//! 3. `session.error`, which the hub ignores outright, emits a display-only
//!    `status` row. A turn that died on a provider error was previously
//!    invisible in the transcript.
//!
//! And one thing the fold no longer has: `settled()`. It existed to tell the
//! hub's freshness machinery which verdicts survive a quiet stream, which was
//! pane-stability's question. There is no pane here.

use std::collections::hash_map::Entry;
use std::collections::{HashMap, HashSet};

use serde::Deserialize;
use serde_json::value::RawValue;
use shed_core::lane::{
    LaneApproval, LaneApprovalKind, LaneApprovalOption, LaneApprovalStatus, LaneQuestion,
};
use shed_core::rc::{RcActivity, RcFeedApproval, RcFeedMessage, RcFeedTool};

use crate::helpers::{
    compact_json, first_non_empty, json_first_byte, null_default, null_string_vec, object_default,
    object_from_raw, object_opt, raw_opt, sanitize_last_message, trim_feed_text, vec_objects,
    APPROVAL_DECISION_ALLOW, APPROVAL_DECISION_ALLOW_ALWAYS, APPROVAL_DECISION_DENY,
    APPROVAL_STATUS_PENDING, APPROVAL_STATUS_RESOLVED, FEED_ROLE_ASSISTANT, FEED_ROLE_SYSTEM,
    FEED_ROLE_TOOL, FEED_ROLE_USER, FEED_TYPE_APPROVAL_REQUEST, FEED_TYPE_REASONING,
    FEED_TYPE_STATUS, FEED_TYPE_TEXT, FEED_TYPE_TOOL_RESULT, FEED_TYPE_TOOL_USE,
};

// ---- parse structs (all fields optional; tolerant — the null/shape discipline
// the copied helpers carry applies throughout) ----

/// The generic `{id,type,properties}` frame every `/event` payload shares.
///
/// `properties` is captured RAW rather than decoded in place: the fold hands the
/// verbatim bytes to [`LaneApproval::request_json`] (the contract's escape hatch
/// for an approval a client cannot otherwise render) and only then applies the
/// same object-shape gate `object_opt` would have. Duplicate JSON keys error
/// here where Go last-wins — no real producer emits them.
#[derive(Debug, Default, Deserialize)]
struct OcEnvelope {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    /// Absent and `null` both fold with zero properties (Go's RawMessage no-op);
    /// a non-object errors the line.
    #[serde(default, deserialize_with = "raw_opt")]
    properties: Option<Box<RawValue>>,
}

/// The union of the properties fields the fold reads across event types.
#[derive(Debug, Default, Deserialize)]
struct OcProperties {
    /// The session this frame belongs to. The watcher has already filtered on
    /// it; the fold reads it to ATTRIBUTE an approval, because a descendant
    /// session's approval blocks the root's agent and surfaces on the root's
    /// panel with the child's id ([`LaneApproval::session_id`]). A wrong-typed
    /// `sessionID` still fails the line, as it does in the hub.
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    session_id: String,
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
    /// session.error
    #[serde(default, deserialize_with = "object_opt")]
    error: Option<OcError>,

    /// The reply-event fields (permission.replied,
    /// question.replied/rejected): opencode addresses the resolved request by
    /// requestID and names its native reply ("once"|"always"|"reject"). `id` is
    /// accepted as a tolerant fallback address (see
    /// [`OcProperties::reply_target`]) — a missed resolution would strand an
    /// approval pending forever, which is the one failure mode worth being
    /// generous about. The question `answers[]` payload is deliberately NOT
    /// read: the fold only needs to know the question closed.
    #[serde(default, rename = "requestID", deserialize_with = "null_default")]
    request_id: String,
    #[serde(default, deserialize_with = "null_default")]
    reply: String,
}

impl OcProperties {
    /// The approval id a replied/rejected event addresses. opencode carries it
    /// as requestID; `id` is a tolerant fallback so a spelling difference cannot
    /// strand a pending entry (and with it, a stuck needs_approval verdict).
    fn reply_target(&self) -> &str {
        first_non_empty(&self.request_id, &self.id)
    }
}

/// permission.asked's metadata bag. Only the command is read — it is the tool
/// detail on the approval row.
#[derive(Debug, Default, Deserialize)]
struct OcPermMetadata {
    #[serde(default, deserialize_with = "null_default")]
    command: String,
}

/// session.status's status union, discriminated by type (busy|idle|retry).
#[derive(Debug, Default, Deserialize)]
struct OcStatus {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
}

/// session.error's error union: a named error class with an optional message in
/// its data bag (opencode's `NamedError` shape).
#[derive(Debug, Default, Deserialize)]
struct OcError {
    #[serde(default, deserialize_with = "null_default")]
    name: String,
    #[serde(default, deserialize_with = "object_opt")]
    data: Option<OcErrorData>,
}

#[derive(Debug, Default, Deserialize)]
struct OcErrorData {
    #[serde(default, deserialize_with = "null_default")]
    message: String,
}

/// message.updated's info.
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
/// ({start,end}); the unused pair stays zero for whichever shape is present.
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

/// A message.part.updated part — the full snapshot.
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

/// A tool part's state union, discriminated by status.
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

/// One entry of question.asked's `questions[]` — opencode's `QuestionInfo`
/// (`packages/schema/src/v1/question.ts`).
///
/// `text` is not in that schema: opencode spells the prose field `question`. It
/// is read anyway because the hub's fold read it, and the status row's header
/// fallback (`header` → `text`) is pinned by the golden. Both feed
/// [`LaneQuestion::question`], `question` first.
#[derive(Debug, Default, Deserialize)]
struct OcQuestion {
    #[serde(default, deserialize_with = "null_default")]
    header: String,
    #[serde(default, deserialize_with = "null_default")]
    text: String,
    #[serde(default, deserialize_with = "null_default")]
    question: String,
    #[serde(default, deserialize_with = "vec_objects")]
    options: Vec<OcQuestionOption>,
    #[serde(default, deserialize_with = "null_default")]
    multiple: bool,
    #[serde(default, deserialize_with = "null_default")]
    custom: bool,
}

/// One `QuestionInfo.options[]` entry: display text plus an explanation. It
/// carries NO id of its own, so the contract's [`LaneApprovalOption::id`] is the
/// label — which is also exactly what opencode's reply route wants back
/// (`QuestionReply.answers` is "an array of selected labels").
#[derive(Debug, Default, Deserialize)]
struct OcQuestionOption {
    #[serde(default, deserialize_with = "null_default")]
    label: String,
    #[serde(default, deserialize_with = "null_default")]
    description: String,
}

// ---- cached parts ----

/// The latest snapshot of a text/reasoning part that has not yet been emitted —
/// either because its owning message's role is not known yet (parts can arrive
/// before their message.updated) or because its terminal condition
/// (part.time.end / message completed) has not been met. Dropped once the part
/// emits.
#[derive(Debug, Default)]
struct OcPartCache {
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

/// The approval map's key: the ask's KIND paired with its opencode request id.
///
/// The id alone is NOT a key. opencode's request ids are unique only within a
/// kind — a `permission.asked` and a `question.asked` may carry the same id (in
/// practice they are prefixed `per_…` / `que_…`, but that is the producer's
/// convention, not a guarantee the fold may lean on) — and the hub got that
/// isolation for free by holding two maps (`pending_perms` + `pending_questions`).
/// This port holds ONE map, so the kind has to be part of the key. Without it:
/// a `permission.replied` could resolve a QUESTION of the same id, and the
/// second of two colliding asks would be discarded rather than tracked. The
/// compound key restores the two maps' isolation exactly while keeping the
/// single map + ask-order vec.
type ApprovalKey = (LaneApprovalKind, String);

/// The [`ApprovalKey`] naming one kind's `id`.
fn approval_key(kind: LaneApprovalKind, id: &str) -> ApprovalKey {
    (kind, id.to_string())
}

/// One tracked approval — a permission ask or a question — in the fold's own
/// storage. [`OcApproval::to_lane`] renders it as the contract's
/// [`LaneApproval`].
///
/// Keyed by [`ApprovalKey`] — the kind plus the opencode request id (the
/// address the answer verb takes). The kind lives ONLY in the key, never
/// alongside it in the value, so the two can never disagree.
///
/// A RESOLVED entry is RETAINED, not deleted: the answer verb must distinguish
/// an id it never saw ([`shed_core::lane::LaneError::UnknownApproval`]) from one
/// already answered ([`shed_core::lane::LaneError::AlreadyResolved`]) — see
/// [`OpencodeFold::approval_state`]. What it retains is a LIGHTWEIGHT tombstone:
/// [`OcApproval::shed_payload`] drops the structured questions and the raw
/// request JSON at resolution, since nothing reads an answered ask's payload and
/// holding it would make the map grow with the size of the session's asks rather
/// than with their count. The hub had no equivalent because it DELETED a replied
/// question outright and its permission entry carried no payload to begin with.
/// Growth is bounded by the session's total asks, like the emitted dedup set.
#[derive(Debug)]
struct OcApproval {
    /// The session that asked — the ROOT's id normally, a DESCENDANT's when a
    /// child session blocks the same agent.
    session_id: String,
    /// The sanitized human-readable summary carried by both feed rows.
    text: String,
    /// A permission's `metadata.command`; empty on a question.
    detail: String,
    /// A question's structured form; empty on a permission (the contract's
    /// `kind` selects, and the two are never both populated). Shed on
    /// resolution.
    questions: Vec<LaneQuestion>,
    /// The ask's verbatim `properties` JSON. Shed on resolution — back to the
    /// `"{}"` empty document, never to `""`, because the contract documents the
    /// field as raw JSON a client may parse.
    request_json: String,
    /// Always `None` from the live stream: opencode's `PermissionRequest` /
    /// `QuestionRequest` schemas carry no timestamp. Kept as a field because the
    /// REST seed's list rows are a different shape and may.
    created_at_unix_ms: Option<i64>,
    resolved: bool,
    /// The contract decision that resolved it ("" when resolved outside this
    /// lane, and always on a question).
    decision: String,
}

impl OcApproval {
    fn to_lane(&self, kind: &LaneApprovalKind, id: &str) -> LaneApproval {
        let is_permission = matches!(kind, LaneApprovalKind::Permission);
        LaneApproval {
            id: id.to_string(),
            session_id: self.session_id.clone(),
            kind: kind.clone(),
            status: if self.resolved {
                LaneApprovalStatus::Resolved
            } else {
                LaneApprovalStatus::Pending
            },
            title: self.text.clone(),
            detail: (!self.detail.is_empty()).then(|| self.detail.clone()),
            // `kind` selects: a permission renders `options`, a question renders
            // `questions` (each with its own options). Never both.
            options: if is_permission {
                permission_options()
            } else {
                Vec::new()
            },
            questions: self.questions.clone(),
            request_json: self.request_json.clone(),
            created_at_unix_ms: self.created_at_unix_ms,
        }
    }

    /// Drops what only an OPEN ask needs, leaving the tombstone (session, text,
    /// detail, status, decision) a resolved entry is kept for. Called on every
    /// resolution path — a reply, a rejection, a seed retirement — so an
    /// answered ask's payload cannot be retained for the life of the session.
    /// A later re-ask REOPENS the entry and refreshes both fields from the new
    /// frame, so shedding can never leave a pending approval with a stale or
    /// missing payload.
    fn shed_payload(&mut self) {
        self.questions = Vec::new();
        self.request_json = EMPTY_REQUEST_JSON.to_string();
    }
}

/// Folds the `/event` envelope stream into activity + transcript + approvals.
/// Holds cumulative state across [`OpencodeFold::apply_line`] calls and is NOT
/// safe for concurrent use (the owning watcher serializes access).
#[derive(Debug, Default)]
pub struct OpencodeFold {
    /// Seen ≥1 activity-relevant event (session.status/idle, a tool part, or an
    /// open approval).
    confirmed: bool,
    /// Seen ≥1 LIVE session.status/session.idle boundary (the REST status is a
    /// fallback).
    saw_status: bool,
    /// "busy" | "idle" | "" (retry maps to busy).
    last_boundary: String,
    /// Open tool-call ids (pending/running).
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

    /// Dedup: "<id>|<phase>" already emitted (survives reseed + gap).
    emitted: HashSet<String>,
    /// Produced-but-undrained feed rows.
    msgs: Vec<RcFeedMessage>,

    /// Every tracked approval — permissions AND questions — by [`ApprovalKey`].
    /// The hub kept the two in separate maps because only permissions were
    /// addressable; the lane contract addresses both, so one map (plus one order
    /// vec) serves both and makes the question snapshot deterministically
    /// ordered where the hub's was not. The KIND stays in the key so the single
    /// map keeps the two maps' isolation — see [`ApprovalKey`].
    approvals: HashMap<ApprovalKey, OcApproval>,
    /// Ask order, for the pending-approvals snapshot.
    approval_order: Vec<ApprovalKey>,
}

impl OpencodeFold {
    /// A fold with every map/slice empty.
    pub fn new() -> OpencodeFold {
        OpencodeFold::default()
    }

    /// Folds one decoded `{id,type,properties}` envelope. Returns true when the
    /// envelope was recognized and folded (an activity change, a cached/emitted
    /// feed row, or a role/completion update); an ignored family or an
    /// unparseable line returns false and leaves state untouched.
    pub fn apply_line(&mut self, line: &[u8]) -> bool {
        // Top-level object gate (Go rejects the seq form, and a top-level null
        // no-ops to the zero envelope whose empty type returns false).
        if json_first_byte(line) != Some(b'{') {
            return false;
        }
        let Ok(env) = serde_json::from_slice::<OcEnvelope>(line) else {
            return false;
        };
        if env.typ.is_empty() {
            return false;
        }
        // The raw bytes are kept for `request_json`; the shape gate is the one
        // `object_opt` applies (object decodes, null/absent no-ops to zero
        // properties, anything else errors the line).
        let raw_props = env.properties.as_deref();
        let props = match raw_props.map(object_from_raw::<OcProperties>) {
            Some(Ok(p)) => p.unwrap_or_default(),
            Some(Err(_)) => return false,
            None => OcProperties::default(),
        };

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
            "permission.asked" => self.apply_permission_asked(&props, raw_props),
            "permission.replied" => {
                // Map opencode's native reply back onto the contract's decision
                // enum. An unrecognized/absent reply still CLOSES the ask (with
                // no decision): the request is demonstrably answered, and a
                // stuck entry would pin needs_approval forever.
                self.resolve_permission(
                    props.reply_target(),
                    opencode_decision_from_reply(&props.reply),
                )
            }
            "question.asked" => self.apply_question_asked(&props, raw_props),
            "question.replied" | "question.rejected" => self.resolve_question(props.reply_target()),
            // The hub ignores this one; the lane surfaces it, so a turn that
            // died on a provider/abort error is visible in the transcript
            // instead of just going quiet.
            "session.error" => self.apply_session_error(&props),
            _ => {
                // session.created/updated, message.part.removed,
                // message.part.delta, session.next.*, server.*, catalog/plugin,
                // and any unknown type: ignored.
                false
            }
        }
    }

    /// Clears ALL fold state. The watcher calls it at the head of every
    /// generation, immediately after emitting
    /// [`shed_core::lane::LaneEvent::Reset`] — the contract's rule that a
    /// (re)connect replays a full seed, which is only true if the fold has
    /// forgotten what it already emitted.
    pub fn reset(&mut self) {
        *self = OpencodeFold::default();
    }

    /// Drops the pending tool-call set — a gap (a dropped frame, an inbox
    /// overflow) may have swallowed a completed/error part, and a
    /// forever-pending callID would pin the verdict at working.
    ///
    /// It KEEPS the emitted-part dedup set (and the cached snapshots) so reseed
    /// idempotency survives a gap: a reseed on a KEPT fold emits nothing. The
    /// open-approval state is deliberately KEPT too — an approval whose reply
    /// the gap may have swallowed is retired authoritatively by the reseed's
    /// [`OpencodeFold::seed_approvals`], so clearing it here would only blink
    /// needs_approval off and back on for genuinely-open asks.
    pub fn note_gap(&mut self) {
        self.pending.clear();
    }

    /// Returns and clears the feed rows produced since the last drain.
    ///
    /// `seq` is 0 on every row the fold mints: the bounded ring owns `seq`
    /// (contract: monotonic across resets within one subscription), and it
    /// assigns it on push.
    pub fn drain_messages(&mut self) -> Vec<RcFeedMessage> {
        std::mem::take(&mut self.msgs)
    }

    /// The current verdict, [`RcActivity::Unknown`] until a confirming event.
    ///
    /// **Never [`RcActivity::Idle`].** The hub could report idle because it also
    /// watched a tmux pane for stability; the lane has no pane, so a session
    /// with nothing to say is `needs_input` (it reached an idle boundary) or
    /// `working` (it has not).
    pub fn activity(&self) -> RcActivity {
        if !self.confirmed {
            return RcActivity::Unknown;
        }
        // needs_approval is checked BEFORE the pending-tool arm: the tool call
        // that triggered the ask is still open (opencode holds it) so the
        // pending set says "working", but the session is blocked on the
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

    /// A sanitized, bounded preview of the most recent assistant message
    /// (`""` if none).
    pub fn last_message(&self) -> String {
        sanitize_last_message(&self.last_msg)
    }

    /// Tracks a message's role + completion time (feed bookkeeping — it does NOT
    /// touch activity) and flushes any cached parts for that message now that
    /// its role / completion is known. Returns true ONLY when it advanced state
    /// — a newly-known role, a newly-known completion, or a cached part it
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

    /// Caches a text/reasoning part's latest snapshot and attempts to emit it.
    ///
    /// - A synthetic/ignored snapshot SUPPRESSES the part: any earlier cached
    ///   partial for the same partID is dropped so it can never be flushed on
    ///   message-completion.
    /// - A part with no messageID can never have its role resolved, so it is
    ///   never cached.
    /// - A part already emitted (its body dedup key is set — a reseed replay) is
    ///   not re-cached: re-appending it to part_order on every reconnect would
    ///   leak unboundedly.
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

    /// Emits a cached text/reasoning part if its role is known and its terminal
    /// condition is met: user text emits immediately; assistant text/reasoning
    /// emits at part.time.end or message completion. Deduped by partID so a
    /// reseed re-fold is a no-op. Returns true only when it actually emitted a
    /// feed row.
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
        let msg = RcFeedMessage {
            ts,
            role: feed_role.to_string(),
            msg_type: typ.to_string(),
            text: opt_text(&ent.text),
            ..RcFeedMessage::default()
        };
        let is_assistant_text = role == FEED_ROLE_ASSISTANT && ent.kind == "text";
        if self.emit_once(key, msg) {
            if is_assistant_text {
                // The cached snapshot is about to go, so move its text out
                // rather than clone it a second time.
                if let Some(ent) = self.parts.get_mut(part_id) {
                    self.last_msg = std::mem::take(&mut ent.text);
                }
            }
            self.drop_part(part_id);
            return true;
        }
        false
    }

    /// Tracks a tool call's pending state and emits tool_use / tool_result rows,
    /// each once, keyed by callID. tool_use emits on the first
    /// running/completed/error snapshot carrying non-empty input; tool_result
    /// emits on completed (output) / error (error). A completed snapshot seen
    /// with neither yet emitted emits BOTH (tool_use then tool_result).
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
                // An unrecognized tool state is tolerantly ignored: it must NOT
                // confirm activity, mutate the pending set, or emit — a
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
                RcFeedMessage {
                    ts,
                    role: FEED_ROLE_TOOL.into(),
                    msg_type: FEED_TYPE_TOOL_USE.into(),
                    tool: Some(feed_tool(&p.tool, &detail)),
                    ..RcFeedMessage::default()
                },
            );
        }
        if matches!(st.status.as_str(), "completed" | "error") {
            let result_detail = if st.status == "error" {
                st.error.as_str()
            } else {
                st.output.as_str()
            };
            let ts = opencode_ts(first_non_zero(&[st.time.end, st.time.start, upd_time]));
            self.emit_once(
                format!("{}|result", p.call_id),
                RcFeedMessage {
                    ts,
                    role: FEED_ROLE_TOOL.into(),
                    msg_type: FEED_TYPE_TOOL_RESULT.into(),
                    tool: Some(feed_tool(&p.tool, result_detail)),
                    ..RcFeedMessage::default()
                },
            );
        }
        true
    }

    // ---- approvals (permission asks + questions) ----

    /// Tracks a permission ask and emits its PENDING approval_request row.
    ///
    /// An ask with NO id stays on the display-only status-row path: the id is
    /// both the row's wire address and the key a permission.replied clears, so
    /// an id-less ask could never be answered remotely NOR retired — tracking it
    /// would pin needs_approval forever on a session nothing can unblock.
    ///
    /// A re-ask of an id already tracked is usually a reseed replay and changes
    /// nothing — with ONE exception, the REOPEN rule: an entry resolved with an
    /// EMPTY decision was closed by [`OpencodeFold::seed_approvals`] (or by a
    /// reply whose vocabulary we could not read), i.e. it was retired on the
    /// evidence "the server no longer lists it". A later ask for that same id is
    /// NEWER, stronger evidence that it IS open — a stale/racing REST snapshot
    /// retired it wrongly — so the entry is reopened and its rows re-announced
    /// (both dedup slots are cleared, since the client needs the pending row
    /// again to render the buttons). An entry resolved with a KNOWN decision
    /// stays closed: a real reply was observed for it, and no ask replay
    /// outranks that.
    ///
    /// A reopen rebuilds EVERY field the DTO carries from this frame, not just
    /// the display ones — a tombstone minted by
    /// [`OpencodeFold::resolve_permission`] for an ask the fold never saw has an
    /// empty `session_id` and no `request_json`, and leaving those behind would
    /// hand a client a pending approval it cannot attribute to a session
    /// ([`LaneApproval::session_id`] is what makes a descendant's approval
    /// attributable) nor render through the raw escape hatch
    /// ([`LaneApproval::request_json`]).
    ///
    /// A question of the SAME id is a different [`ApprovalKey`] and is neither
    /// read nor touched here.
    fn apply_permission_asked(&mut self, props: &OcProperties, raw: Option<&RawValue>) -> bool {
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
        let detail = props
            .metadata
            .as_ref()
            .map(|m| m.command.clone())
            .unwrap_or_default();
        let key = approval_key(LaneApprovalKind::Permission, &props.id);
        match self.approvals.get_mut(&key) {
            Some(ent) => {
                if !ent.resolved || !ent.decision.is_empty() {
                    return false; // still open, or genuinely answered: a replay is not a state change
                }
                // REOPEN (see the doc): drop the resolution, refresh every DTO
                // field from THIS frame (a tombstone's were never populated),
                // and clear both dedup slots so the pending row is re-announced
                // and a later real resolution can emit its own row.
                ent.resolved = false;
                ent.session_id = props.session_id.clone();
                ent.text = text.clone();
                ent.detail = detail.clone();
                ent.request_json = request_json(raw);
                self.emitted
                    .remove(&perm_key(&props.id, APPROVAL_STATUS_PENDING));
                self.emitted
                    .remove(&perm_key(&props.id, APPROVAL_STATUS_RESOLVED));
            }
            None => self.track_approval(
                key,
                OcApproval {
                    session_id: props.session_id.clone(),
                    text: text.clone(),
                    detail: detail.clone(),
                    questions: Vec::new(),
                    request_json: request_json(raw),
                    created_at_unix_ms: None,
                    resolved: false,
                    decision: String::new(),
                },
            ),
        }
        // An open approval IS the activity verdict now (needs_approval), so —
        // unlike a display-only row — an ask is activity-relevant evidence and
        // confirms the fold.
        self.confirmed = true;
        self.emit_once(
            perm_key(&props.id, APPROVAL_STATUS_PENDING),
            RcFeedMessage {
                role: FEED_ROLE_TOOL.into(),
                msg_type: FEED_TYPE_APPROVAL_REQUEST.into(),
                text: opt_text(&text),
                tool: Some(feed_tool(&props.permission, &detail)),
                approval: Some(RcFeedApproval {
                    id: props.id.clone(),
                    status: APPROVAL_STATUS_PENDING.into(),
                    decisions: opencode_decisions(),
                    ..RcFeedApproval::default()
                }),
                ..RcFeedMessage::default()
            },
        );
        true
    }

    /// Marks an open permission ask resolved with `decision` (which may be ""
    /// when the answer happened outside this lane) and appends the RESOLVED
    /// approval row.
    ///
    /// IDEMPOTENT BY DESIGN, and that is load-bearing: the answer verb marks an
    /// entry resolved synchronously the moment its POST succeeds (so a
    /// same-decision replay cannot re-POST), and opencode's own
    /// permission.replied event for the same id arrives on the stream moments
    /// later. Exactly one resolved row must reach the feed, so the second call
    /// is a no-op.
    ///
    /// A reply for an id this fold never saw asked (the ask predates the
    /// subscription, or a gap swallowed it) records a resolved TOMBSTONE rather
    /// than being dropped: without it a later ask replay would open a PENDING
    /// entry for a permission that is demonstrably answered, stranding the
    /// session at needs_approval. Its resolved row still goes out (the client
    /// folding rule explicitly allows a resolved row with no pending row before
    /// it).
    ///
    /// Accepted, not fixed: an entry the answer verb resolved that a reconnect's
    /// `GET /permission` still lists stays resolved here — the ask replay's
    /// reopen rule does not apply to it because its decision is known. It
    /// self-heals the moment the TUI drops the request from its store.
    ///
    /// Scoped to [`LaneApprovalKind::Permission`] by the [`ApprovalKey`]: a
    /// QUESTION carrying the same id is a different entry and is never read,
    /// resolved or tombstoned by this path, exactly as when the hub held the two
    /// in separate maps.
    pub fn resolve_permission(&mut self, id: &str, decision: &str) -> bool {
        if id.is_empty() {
            return false;
        }
        let key = approval_key(LaneApprovalKind::Permission, id);
        match self.approvals.get(&key) {
            None => {
                self.track_approval(
                    key.clone(),
                    OcApproval {
                        // A reply frame names the session, but a tombstone is
                        // never rendered as an open approval, so nothing reads
                        // this; leaving it empty keeps the tombstone honest
                        // about being a placeholder for an ask we never saw. A
                        // later ask REOPENS the entry and fills it in.
                        session_id: String::new(),
                        text: "approval resolved".into(),
                        detail: String::new(),
                        questions: Vec::new(),
                        request_json: EMPTY_REQUEST_JSON.into(),
                        created_at_unix_ms: None,
                        resolved: false,
                        decision: String::new(),
                    },
                );
            }
            Some(ent) if ent.resolved => return false,
            Some(_) => {}
        }
        let ent = self.approvals.get_mut(&key).expect("present");
        ent.resolved = true;
        ent.decision = decision.to_string();
        ent.shed_payload();
        let text = ent.text.clone();
        self.emit_once(
            perm_key(id, APPROVAL_STATUS_RESOLVED),
            RcFeedMessage {
                role: FEED_ROLE_TOOL.into(),
                msg_type: FEED_TYPE_APPROVAL_REQUEST.into(),
                text: opt_text(&text),
                approval: Some(RcFeedApproval {
                    id: id.to_string(),
                    status: APPROVAL_STATUS_RESOLVED.into(),
                    decision: (!decision.is_empty()).then(|| decision.to_string()),
                    ..RcFeedApproval::default()
                }),
                ..RcFeedMessage::default()
            },
        );
        true
    }

    /// Emits the display-only status row for a question and, when the question
    /// is addressable (it carries an id), tracks it as an open
    /// [`LaneApprovalKind::Question`] approval so it counts toward
    /// needs_approval AND can be answered. An id-less question is display-only
    /// for the same reason an id-less permission is: nothing could ever clear
    /// it.
    ///
    /// The FEED row is the hub's, unchanged — a `status` row, not an
    /// `approval_request` one. What the lane adds is the approval STATE: the
    /// hub could only count a question (its `pending_approvals` snapshot was
    /// permissions-only, because a question's answer vocabulary is not the
    /// decision enum), while [`LaneApproval::questions`] carries opencode's
    /// `QuestionInfo` verbatim and the contract's `LaneAnswer::Question` answers
    /// it.
    fn apply_question_asked(&mut self, props: &OcProperties, raw: Option<&RawValue>) -> bool {
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
        if props.id.is_empty() {
            return true;
        }
        // Same reopen rule the permission path carries, and for questions it is
        // unconditional: a question is only ever resolved with an empty decision
        // (its reply vocabulary is labels, not a decision enum), so a re-ask
        // always reopens — which is exactly what the hub did by REMOVING a
        // replied question and re-inserting it on the next ask. Unlike a
        // permission reopen, no dedup slot is cleared: the status row is
        // announce-once, as it was in the hub.
        //
        // The reopen refreshes EVERY field from this frame — the hub's
        // remove-and-reinsert did so by construction, and a half-refreshed entry
        // would hand a client structured questions from the new ask beside a
        // `request_json` from the old one (or, after a resolution shed it, none
        // at all). A permission of the SAME id is a different key: untouched.
        let key = approval_key(LaneApprovalKind::Question, &props.id);
        match self.approvals.get_mut(&key) {
            Some(ent) if ent.resolved && ent.decision.is_empty() => {
                ent.resolved = false;
                ent.session_id = props.session_id.clone();
                ent.text = text;
                ent.questions = props.questions.iter().map(lane_question).collect();
                ent.request_json = request_json(raw);
                self.confirmed = true;
            }
            Some(_) => {}
            None => {
                self.track_approval(
                    key,
                    OcApproval {
                        session_id: props.session_id.clone(),
                        text,
                        detail: String::new(),
                        questions: props.questions.iter().map(lane_question).collect(),
                        request_json: request_json(raw),
                        created_at_unix_ms: None,
                        resolved: false,
                        decision: String::new(),
                    },
                );
                self.confirmed = true; // an open question is the needs_approval verdict
            }
        }
        true
    }

    /// Retires an open question (question.replied / question.rejected). No feed
    /// row: the ask's row was display-only, and the resolution reaches a client
    /// as the approval's own [`LaneApprovalStatus::Resolved`] frame.
    ///
    /// The entry is marked resolved, not dropped: the answer verb has to tell an
    /// id it never saw from one already answered, exactly as for a permission.
    /// What it keeps is the lightweight tombstone — the structured questions and
    /// the raw request go with the resolution ([`OcApproval::shed_payload`]),
    /// which is the hub's outright REMOVAL of a replied question, minus the
    /// forgetting.
    ///
    /// Scoped to [`LaneApprovalKind::Question`] by the [`ApprovalKey`]: a
    /// PERMISSION carrying the same id is a different entry and is never
    /// resolved by a question reply.
    pub fn resolve_question(&mut self, id: &str) -> bool {
        if id.is_empty() {
            return false;
        }
        match self
            .approvals
            .get_mut(&approval_key(LaneApprovalKind::Question, id))
        {
            Some(ent) if !ent.resolved => {
                ent.resolved = true;
                ent.shed_payload();
                true
            }
            _ => false,
        }
    }

    /// Emits a display-only `status` row for `session.error`.
    ///
    /// **The hub ignores this event outright**; plan 015 §3.2 surfaces it,
    /// because a turn that died on a provider auth failure or an abort otherwise
    /// just goes quiet — the transcript ends mid-thought with no explanation and
    /// the verdict falls back to whatever the last boundary said.
    ///
    /// Display-only in the strict sense the status row already means: it does
    /// NOT confirm the fold and does NOT touch the activity verdict. An error is
    /// evidence about a turn, not about whether the session is running, and
    /// opencode follows a real error with its own `session.status`/`session.idle`
    /// boundary.
    fn apply_session_error(&mut self, props: &OcProperties) -> bool {
        let (name, message) = match &props.error {
            Some(e) => (
                e.name.as_str(),
                e.data.as_ref().map(|d| d.message.as_str()).unwrap_or(""),
            ),
            None => ("", ""),
        };
        let detail = first_non_empty(message, name);
        let text = if detail.is_empty() {
            "session error".to_string()
        } else {
            format!("session error: {detail}")
        };
        // Content-keyed, like every id-less status row: the same error replayed
        // by a reseed must not stack up a row per reconnect.
        self.emit_status_row("err", "", &text);
        true
    }

    /// Reconciles the fold's open asks against a seed's AUTHORITATIVE view of
    /// what is still open on the server — the method form of the hub's
    /// synthesized `shed.approval.seed` envelope.
    ///
    /// Every reconnect reseeds, which is what makes this the SELF-HEALING path:
    /// a `permission.replied` or `question.replied` lost to a disconnect or an
    /// inbox gap leaves a locally-open entry the server no longer lists, and it
    /// is retired here — with NO decision, since the answer was given in the TUI
    /// and the lane cannot know which way it went.
    ///
    /// The two halves carry INDEPENDENT authority, which is what `Option`
    /// encodes: `None` means "that REST read failed, so this half says nothing"
    /// — a failed read must never be mistaken for "nothing is open", which would
    /// retire live approvals — while `Some(ids)` (the empty slice included) is
    /// the authoritative open set. Neither half blocks the other's healing.
    ///
    /// Returns whether anything was retired.
    pub fn seed_approvals(
        &mut self,
        permissions: Option<&[String]>,
        questions: Option<&[String]>,
    ) -> bool {
        let mut changed = false;
        if let Some(open) = permissions {
            let open: HashSet<&str> = open.iter().map(String::as_str).collect();
            // The ids to retire are collected first so the borrow ends before
            // resolve_permission mutates.
            let stale = self.stale_ids(&LaneApprovalKind::Permission, &open);
            for id in stale {
                // resolve_permission is the guard: an entry already resolved
                // reports false.
                if self.resolve_permission(&id, "") {
                    changed = true;
                }
            }
        }
        if let Some(open) = questions {
            let open: HashSet<&str> = open.iter().map(String::as_str).collect();
            let stale = self.stale_ids(&LaneApprovalKind::Question, &open);
            for id in stale {
                if self.resolve_question(&id) {
                    changed = true;
                }
            }
        }
        changed
    }

    /// Counts what the session is BLOCKED on: unresolved permission asks plus
    /// open questions. Both drive needs_approval.
    pub fn open_approvals(&self) -> usize {
        self.approvals.values().filter(|a| !a.resolved).count()
    }

    /// A tracked approval's status. `None` means this (kind, id) is unknown to
    /// the fold — the answer verb's
    /// [`shed_core::lane::LaneError::UnknownApproval`]. This, NOT the
    /// [`OpencodeFold::pending_approvals`] snapshot, is the resolution-state
    /// oracle: the snapshot is pending-only by contract.
    ///
    /// **Takes the KIND, because an id alone does not identify an approval** —
    /// see [`ApprovalKey`]. A caller holding a
    /// [`shed_core::lane::LaneAnswer::Permission`] or
    /// [`shed_core::lane::LaneAnswer::Question`] knows the kind from the variant
    /// it is about to honor and passes it. A caller holding only a bare id (a
    /// `Reject` or `Raw` answer names no kind) resolves it FIRST through
    /// [`OpencodeFold::approvals_for_id`], which reports every kind that id is
    /// tracked under and leaves the choice — including the choice to refuse an
    /// ambiguous one — with the caller. The fold never picks a kind on its own.
    ///
    /// Only [`LaneApprovalStatus::Pending`] and [`LaneApprovalStatus::Resolved`]
    /// are ever returned. `Submitted` is the client's optimistic middle state
    /// and belongs to whatever tracks an in-flight answer, not to the fold,
    /// which reports only what the agent has told it.
    pub fn approval_state(&self, kind: LaneApprovalKind, id: &str) -> Option<LaneApprovalStatus> {
        self.approvals.get(&approval_key(kind, id)).map(|ent| {
            if ent.resolved {
                LaneApprovalStatus::Resolved
            } else {
                LaneApprovalStatus::Pending
            }
        })
    }

    /// One tracked approval, resolved ones included, as the contract's DTO.
    /// Kind-scoped for the same reason [`OpencodeFold::approval_state`] is.
    pub fn approval(&self, kind: LaneApprovalKind, id: &str) -> Option<LaneApproval> {
        let key = approval_key(kind, id);
        self.approvals.get(&key).map(|ent| ent.to_lane(&key.0, id))
    }

    /// EVERY approval tracked under `id`, whatever its kind, in ask order — the
    /// disambiguation hook for a caller that does not know the kind.
    ///
    /// Normally 0 or 1 entries: opencode prefixes its request ids `per_…` /
    /// `que_…`, so a collision does not arise in practice. It is nonetheless
    /// representable, and when it happens this returns BOTH — the fold refuses
    /// to guess which one a bare id meant, because guessing is exactly how a
    /// permission reply ends up resolving a question. The caller decides
    /// (branching on `kind`, or refusing the ambiguity).
    pub fn approvals_for_id(&self, id: &str) -> Vec<LaneApproval> {
        self.approval_order
            .iter()
            .filter(|(_, tracked)| tracked == id)
            .filter_map(|key| Some(self.approvals.get(key)?.to_lane(&key.0, &key.1)))
            .collect()
    }

    /// The still-open approvals — permissions and questions alike — in ask
    /// order. Freshly allocated on every call so a consumer can hand it straight
    /// to a DTO without aliasing fold state.
    pub fn pending_approvals(&self) -> Vec<LaneApproval> {
        self.approval_order
            .iter()
            .filter_map(|key| {
                let ent = self.approvals.get(key)?;
                (!ent.resolved).then(|| ent.to_lane(&key.0, &key.1))
            })
            .collect()
    }

    /// Applies a REST-seed-derived status boundary as a FALLBACK: it establishes
    /// the activity boundary ONLY when the fold has NOT observed a live
    /// session.status/session.idle event. The live `/event` stream is
    /// authoritative and ordered; the REST `/session/status` snapshot is only
    /// for reconstructing a session that was quiescent at connect — it must
    /// never override a newer buffered live busy/idle. Returns whether it
    /// actually set the boundary (so the caller can count it as a seed event).
    pub fn apply_status_fallback(&mut self, idle: bool) -> bool {
        if self.saw_status {
            return false; // a live status boundary was present: the live stream wins
        }
        self.confirmed = true;
        self.last_boundary = if idle { "idle" } else { "busy" }.into();
        true
    }

    // ---- internals ----

    /// Inserts a new tracked approval and records its ask order.
    fn track_approval(&mut self, key: ApprovalKey, ent: OcApproval) {
        self.approvals.insert(key.clone(), ent);
        self.approval_order.push(key);
    }

    /// The tracked ids of one kind, in ask order, that `open` does NOT list —
    /// the seed's retirement set. Reading the kind off the KEY is what keeps one
    /// half of a seed from retiring the other half's entries.
    fn stale_ids(&self, kind: &LaneApprovalKind, open: &HashSet<&str>) -> Vec<String> {
        self.approval_order
            .iter()
            .filter(|(tracked_kind, id)| tracked_kind == kind && !open.contains(id.as_str()))
            .map(|(_, id)| id.clone())
            .collect()
    }

    /// Queues a feed row unless its dedup key was already emitted; empty-text
    /// non-tool rows are dropped (and do NOT consume the dedup slot, so a later
    /// non-empty snapshot of the same part can still emit).
    fn emit_once(&mut self, key: String, m: RcFeedMessage) -> bool {
        if !key.is_empty() && self.emitted.contains(&key) {
            return false;
        }
        if m.tool.is_none() && trim_feed_text(m.text.as_deref().unwrap_or_default()).is_empty() {
            return false;
        }
        if !key.is_empty() {
            self.emitted.insert(key);
        }
        self.msgs.push(m);
        true
    }

    /// Queues a display-only system/status feed row (permission.asked /
    /// question.asked / session.error), deduped by request id. When the wire
    /// carries no id, the dedup key is derived from the row's CONTENT instead —
    /// otherwise a reseed replay (which has no id to key on) would emit an
    /// identical status row on every reconnect. The row is informational: it
    /// never changes the activity verdict.
    fn emit_status_row(&mut self, prefix: &str, id: &str, text: &str) {
        let key = if id.is_empty() {
            format!("{prefix}|text:{text}")
        } else {
            format!("{prefix}|id:{id}")
        };
        self.emit_once(
            key,
            RcFeedMessage {
                role: FEED_ROLE_SYSTEM.into(),
                msg_type: FEED_TYPE_STATUS.into(),
                text: opt_text(text),
                ..RcFeedMessage::default()
            },
        );
    }

    /// The cached part ids owned by `msg_id` in insertion order (deterministic
    /// flush ordering).
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

    /// Removes an emitted/duplicate cached part from the parts map. Its
    /// part_order slot is left behind and skipped by later reads; part_order
    /// growth is bounded by the session's total parts, like the emitted dedup
    /// set.
    ///
    /// **Leaving the slot is INHERITED and DELIBERATE**, not an oversight of the
    /// port: the hub's fold does exactly this
    /// (`shed-broker/src/rc_hub/watch_opencode.rs`, its own `drop_part`), and a
    /// port that "fixed" it here would change transcript behavior the
    /// `opencode_turn.golden.json` pin exists to hold identical. The same goes
    /// for the `emitted` dedup set, which is never evicted at all — that is the
    /// contract [`OpencodeFold::note_gap`] leans on (a reseed on a KEPT fold must
    /// emit nothing), so evicting from it would reintroduce duplicate rows after
    /// every gap. Both grow with the session's ask/part COUNT, which a session's
    /// lifetime bounds; if that ever needs bounding for real, it is a change to
    /// make in both folds at once, with the golden re-agreed, not a port detail.
    fn drop_part(&mut self, part_id: &str) {
        self.parts.remove(part_id);
    }

    // ---- test-visible accessors ----

    #[cfg(test)]
    pub(crate) fn pending_len(&self) -> usize {
        self.pending.len()
    }

    #[cfg(test)]
    pub(crate) fn parts_len(&self) -> usize {
        self.parts.len()
    }
}

/// One `QuestionInfo` as the contract's [`LaneQuestion`].
///
/// The option ids ARE the labels: opencode's `QuestionOption` has no id of its
/// own, and its reply route wants the selected labels back
/// (`QuestionReply.answers`), so `id = label` is both the contract's stated
/// convention for id-less options AND the literal wire value.
fn lane_question(q: &OcQuestion) -> LaneQuestion {
    LaneQuestion {
        header: q.header.clone(),
        question: first_non_empty(&q.question, &q.text).to_string(),
        options: q
            .options
            .iter()
            .map(|o| LaneApprovalOption {
                id: o.label.clone(),
                label: o.label.clone(),
                description: (!o.description.is_empty()).then(|| o.description.clone()),
            })
            .collect(),
        multiple: q.multiple,
        custom: q.custom,
    }
}

/// The three fixed choices a permission approval accepts, as the contract's
/// options: the id is [`shed_core::lane::LaneDecision`]'s wire spelling (what
/// `LaneAnswer::Permission` carries back), the label is what the panel shows.
/// A FRESH vec per call — every copy ends up on a DTO that must not alias
/// another's.
fn permission_options() -> Vec<LaneApprovalOption> {
    [
        ("allow_once", "Allow once"),
        ("allow_always", "Always"),
        ("reject", "Reject"),
    ]
    .into_iter()
    .map(|(id, label)| LaneApprovalOption {
        id: id.to_string(),
        label: label.to_string(),
        description: None,
    })
    .collect()
}

/// The decision set the FEED row advertises (allow/allow_always/deny — the rc
/// feed's vocabulary, which mobile's `RcFeedApproval` mirror already renders).
/// Distinct from [`permission_options`], which is the lane contract's. A FRESH
/// vec per call.
fn opencode_decisions() -> Vec<String> {
    vec![
        APPROVAL_DECISION_ALLOW.into(),
        APPROVAL_DECISION_ALLOW_ALWAYS.into(),
        APPROVAL_DECISION_DENY.into(),
    ]
}

/// The emit/dedup key for one phase (pending|resolved) of an approval row.
/// Distinct from `emit_status_row`'s "perm|id:<id>" key so the two paths can
/// never collide.
fn perm_key(id: &str, phase: &str) -> String {
    format!("perm|id:{id}|{phase}")
}

/// Maps opencode's native permission reply vocabulary onto the feed's decision
/// tokens — the inverse of what the answer verb sends. An
/// unrecognized/absent reply maps to "" — "resolved, decision unknown", which
/// still closes the ask.
fn opencode_decision_from_reply(reply: &str) -> &'static str {
    match reply {
        "once" => APPROVAL_DECISION_ALLOW,
        "always" => APPROVAL_DECISION_ALLOW_ALWAYS,
        "reject" => APPROVAL_DECISION_DENY,
        _ => "",
    }
}

/// Converts an opencode epoch-millis timestamp to a UTC RFC3339 string, or
/// `None` when there is no usable time — the ring then stamps the row with
/// append-now. A non-positive value, or one whose expanded year falls outside
/// RFC3339's 0001..9999 range, yields `None`: an out-of-range year formats as a
/// non-RFC3339 expanded-year string, which would corrupt the wire contract, so
/// it is treated as "no usable time" instead.
fn opencode_ts(ms: i64) -> Option<String> {
    if ms <= 0 {
        return None;
    }
    let t = chrono::DateTime::from_timestamp_millis(ms)?;
    let year = chrono::Datelike::year(&t);
    if !(1..=9999).contains(&year) {
        return None;
    }
    Some(t.to_rfc3339_opts(chrono::SecondsFormat::Secs, true))
}

/// The emit/dedup key for a text/reasoning part's body row. Shared by the cache
/// (`apply_text_part`'s already-emitted skip) and the emitter (`try_emit_part`)
/// so the two never drift.
fn body_key(part_id: &str) -> String {
    format!("{part_id}|body")
}

/// The first non-zero value, 0 when all are.
fn first_non_zero(vals: &[i64]) -> i64 {
    vals.iter().copied().find(|&v| v != 0).unwrap_or(0)
}

/// An optional feed string under Go's `omitempty` posture: an empty value is
/// ABSENT, never `""`. [`RcFeedMessage`]'s fields are `Option`s where the hub's
/// producer-side `FeedMessage` used plain `String`s, and this is the one-way
/// mapping between them.
fn opt_text(s: &str) -> Option<String> {
    (!s.is_empty()).then(|| s.to_string())
}

/// A feed row's tool block, with both halves under the same `omitempty` rule.
fn feed_tool(name: &str, detail: &str) -> RcFeedTool {
    RcFeedTool {
        name: opt_text(name),
        detail: opt_text(detail),
    }
}

/// The empty-document stand-in for [`LaneApproval::request_json`] — used when a
/// frame carried no properties at all, and when a resolution sheds an ask's
/// payload. NEVER an empty string: the field is documented as raw JSON, and a
/// client that parses it must always get a document.
const EMPTY_REQUEST_JSON: &str = "{}";

/// An approval's verbatim `properties` JSON for [`LaneApproval::request_json`]
/// — the contract's escape hatch, carried as a raw-JSON `String` because the
/// FRB-mirror rule bars a `serde_json::Value`. [`EMPTY_REQUEST_JSON`] when the
/// frame carried no properties at all.
fn request_json(raw: Option<&RawValue>) -> String {
    raw.map(|r| r.get().to_string())
        .unwrap_or_else(|| EMPTY_REQUEST_JSON.to_string())
}

#[cfg(test)]
mod tests;
