//! The **agent lane contract** — one normalized surface over "a coding agent
//! with sessions, a transcript, and approvals", with one adapter per agent.
//!
//! An adapter (opencode over its local HTTP server today, `gx` over its `/v1`
//! HTTP lane next) implements [`AgentLane`]; every client — the desktop app,
//! mobile, `sx` — renders the SAME DTOs regardless of which agent is behind
//! them. The contract lives HERE, in shed-core, and not in `shed-app`, because
//! shed-mobile links shed-core's DTOs through flutter_rust_bridge: the trait has
//! to live where the types it speaks in live.
//!
//! **No I/O in this module.** These are pure DTOs plus one async trait; the
//! transport, the fold, the ring and the reconnect loop belong to whatever crate
//! implements the trait.
//!
//! # The FRB-mirror rule (load-bearing)
//!
//! shed-mobile **hand-mirrors** every DTO here into Dart (the way
//! `dto_rc.rs`/`rc_feed.dart` already mirror [`crate::rc`]'s feed types). So
//! every field is an owned `String`/`Option`/`Vec`/scalar: **no
//! `serde_json::Value`, no `HashMap`, no borrowed lifetimes**. A fielded enum
//! becomes a Dart sealed class; a plain enum becomes a plain Dart enum. Free-form
//! payloads travel as a `String` holding raw JSON ([`LaneApproval::request_json`],
//! [`LaneAnswer::Raw`]) rather than as a `Value` — a `Value` is not mirrorable
//! and would silently strand mobile.
//!
//! # Semantics pinned for every adapter
//!
//! A (re)connect is bracketed by **[`LaneEvent::Reset`] … [`LaneEvent::Ready`]**:
//! between them the adapter replays the full seed (messages, then the session
//! row, then approvals); a client **stages** everything it receives after
//! `Reset` and swaps its view atomically on `Ready` (so a reconnect never
//! flickers), and discards anything stamped with an older `generation`. `seq` is
//! assigned by the adapter's bounded ring (not the fold) and is monotonic across
//! `Reset`s within one subscription — a client that sees a `seq` lower than one
//! it holds refetches, the rule [`crate::rc::RcFeedMessage`] already carries.
//! [`LaneEvent::Down`] means the transport is gone and the subscription ENDED
//! (the task exits after it); a client renders `Down` as stale-with-a-reason,
//! not as an error dialog.
//!
//! [`LaneEvent`] and [`LaneAnswer`] are tagged on a `kind` key, snake_case
//! (`#[serde(tag = "kind", rename_all = "snake_case")]`); the enum-like strings
//! ([`LaneApprovalKind`], [`LaneApprovalStatus`], [`LaneDecision`], [`SendMode`])
//! serialize as bare snake_case strings. [`LaneError`] is the exception whose
//! wire form is **shape-heterogeneous**: its `BadRequest(String)`-style variants
//! are newtypes over a non-map, which serde REFUSES to tag internally, so the
//! error type uses external tagging with `rename_all = "snake_case"` — a unit
//! variant crosses as a BARE STRING (`"unknown_session"`), a payload variant as a
//! ONE-KEY OBJECT (`{"bad_request":"…"}`). shed-mobile hand-mirrors that, so both
//! shapes are contract, not an implementation detail; a Dart decoder that assumes
//! one of the two breaks on the other half of the enum.
//!
//! # Unknown-value tolerance, applied ASYMMETRICALLY
//!
//! **Stream/inbound types tolerate unknown values; command/outbound types reject
//! them.** A frame arriving from an adapter must never fail a client's decode — a
//! newer adapter emitting a status or event kind this client has never heard of
//! must degrade, not vanish. But a command travelling the other way (a decision, a
//! send mode, an answer) must be REJECTED when unrecognized: silently coercing an
//! unknown decision would turn a user's "allow" into a no-op, which is worse than
//! an error.
//!
//! That is the rule a future adapter — and a future variant — follows. Today it
//! makes exactly three types tolerant: [`LaneApprovalKind`] and
//! [`LaneApprovalStatus`], each preserving an unrecognized wire string in an
//! `Other(String)`, and [`LaneEvent`], whose [`LaneEvent::Unknown`] absorbs a
//! `kind` this build has never heard of. It leaves [`LaneDecision`], [`SendMode`],
//! [`LaneAnswer`] and [`LaneError`] strict.
//!
//! The tolerant half matters most where it is least visible. This module already
//! argues, on [`LaneQuestion`]'s optional booleans, that a strict FIELD fails the
//! WHOLE enclosing [`LaneEvent`] — so an approval would vanish from the panel
//! rather than render conservatively. A strict enum-like VALUE nested in that same
//! approval does exactly the same damage: one `"status":"cancelled"` from a newer
//! producer and every approval frame it appears in is dropped by an older client,
//! with the session still rendering as blocked-on-you and no button to unblock it.
//! Tolerance is not politeness here; it is the difference between a stale row and
//! a stuck one.
//!
//! Because the version skew is real and one-directional (shed-mobile pins
//! shed-core by git rev and therefore LAGS), a tolerant decode is also the only
//! thing that lets a producer add a status or a frame kind without a coordinated
//! release of every client.
//!
//! # The opencode ↔ gx mapping
//!
//! Two adapters are planned; this is what each contract verb costs on each side,
//! and it is the reason the contract is shaped the way it is (session-scoped,
//! cursor-optional, approvals as first-class rows).
//!
//! | contract | opencode | gx |
//! |---|---|---|
//! | `sessions` | `GET /session` → every root on that server (the global store); activity from `/session/status`: `busy|retry → Working`, `idle → Idle` | `GET /v1/sessions` |
//! | `history` | `GET /session/{sessionID}/message` through a fresh fold (seq from 1); `cursor` ignored (`history_cursor: false`), `truncated` from the page cap | `GET …/history`; cursor = `lastEventId` |
//! | `send(Queue)` | `POST /session/{sessionID}/prompt_async` | `POST …/messages mode=queue` |
//! | `send(Interject)` | [`LaneError::NotAccepting`] | `mode=interject` |
//! | `cancel` | `POST /session/{sessionID}/abort` | `POST …/cancel` |
//! | `approvals` | fold state seeded from `GET /permission` + `GET /question` (`?directory=` of the session) filtered to the root id **and its descendants** (`GET /session/{sessionID}/children`, refreshed on a `session.created` whose `parentID` is in the set) | `GET …/approvals` |
//! | `answer(Permission)` | `POST /permission/{requestID}/reply {reply}` (the live route; the deprecated session-scoped route is the recorded fallback) | `POST …/approvals/{id}` `optionId` |
//! | `answer(Question)` | `POST /question/{requestID}/reply {answers}`; [`LaneAnswer::Reject`] → `…/reject` | `{outcome: accepted, answers}` |
//! | `subscribe` | `GET /event?directory=<session dir>` opened **first**, then the REST seed while live frames buffer; `Reset` … `Ready` brackets every (re)connect; transcript frames filtered to the root id, approval frames to root + descendants; empty-id frames count as liveness | `GET …/events` with `Last-Event-ID` |
//! | `create` | `POST /session?directory=<cwd>` then `prompt_async` | `POST /v1/sessions` |
//! | errors | 401 → [`LaneError::Unauthorized`]; 404 on a session route → [`LaneError::UnknownSession`], on a permission/question route → [`LaneError::UnknownApproval`]; 409/4xx with an opencode error body → `NotAccepting`/`BadRequest(message)` by body; other non-2xx → [`LaneError::Failed`]; dial failure → [`LaneError::Unavailable`] | the error table verbatim |

use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;

use crate::rc::{RcActivity, RcFeedMessage};

// ---- sessions ----

/// One agent session as the contract sees it: an id, where it lives, what it is
/// doing, and how much is waiting on the human.
///
/// `activity` reuses [`RcActivity`] rather than minting a parallel vocabulary —
/// every client already renders that badge, and a lane row has to sort into the
/// same sessions view as an RC row.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneSession {
    /// The adapter-scoped session id — the address every other verb takes.
    pub id: String,
    pub title: String,
    /// The session's working directory. Also the routing key on adapters that
    /// scope their endpoints by directory (opencode's `?directory=`).
    pub cwd: String,
    /// The session's WORK dimension, and only that — it does NOT subsume
    /// `pending_approvals`. An adapter that derives activity from a cheap status
    /// poll cannot see an approval from there, which is why the pinned opencode
    /// mapping (`busy|retry → Working`, `idle → Idle`) never emits
    /// [`RcActivity::NeedsApproval`] and reports a session sitting on a
    /// permission prompt as `Idle`. Read `activity` for "is it running",
    /// `pending_approvals` for "is it blocked on me"; a client that keys its
    /// blocked badge off `activity` alone will miss every opencode approval.
    pub activity: RcActivity,
    /// How many approvals are open. The authoritative answer to "is this session
    /// blocked on me" — an approval ROW can be evicted from a bounded ring, this
    /// count cannot.
    pub pending_approvals: u32,
    /// `true` when the adapter derived `activity`/`pending_approvals` from a
    /// cheap or stale source (a roster poll rather than a live fold), so a client
    /// can render the row without claiming precision it does not have.
    pub approximate: bool,
    /// Set on a child session; `None` on a root. Descendants exist because an
    /// agent can spawn sub-sessions whose approvals still block the parent.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent_id: Option<String>,
    /// Unix epoch milliseconds of the last change the adapter observed, when it
    /// has one. Never parsed into a datetime in this crate (crate convention:
    /// timestamps cross the wire as the producer minted them).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_change_unix_ms: Option<i64>,
}

/// A page of a session's transcript.
///
/// Messages reuse [`RcFeedMessage`] verbatim — the same rows the RC feed already
/// renders, so a lane transcript and an RC transcript are one widget. `truncated`
/// carries the same meaning it does on `RcMessagesPage`: the client is NOT
/// holding a complete history and must refetch from the earliest retained row
/// rather than splice.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneHistory {
    #[serde(default)]
    pub messages: Vec<RcFeedMessage>,
    pub truncated: bool,
    /// An opaque cursor to resume from, on adapters that advertise
    /// [`LaneCapabilities::history_cursor`]. `None` — and ignored on the way in —
    /// when they do not.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cursor: Option<String>,
}

// ---- approvals ----

/// What KIND of thing is blocking on the human.
///
/// [`LaneApprovalKind::Other`] implements the same **unknown-value policy**
/// [`crate::rc::RcKind`] does: an unrecognized wire value is PRESERVED verbatim
/// rather than coerced or dropped, so an approval minted by a newer agent
/// renders neutrally (its raw kind shown, no kind-specific affordance) instead of
/// disappearing. Because of the owned string this enum is not `Copy`.
///
/// **Round-trip identity holds for every value the wire can produce, and not for
/// one it cannot.** A hand-constructed `Other("permission")` serializes to
/// `"permission"` and decodes back as [`LaneApprovalKind::Permission`], so
/// `encode → decode == self` is false for it. That is benign and deliberate:
/// [`LaneApprovalKind::from_wire`] — the ONLY thing that mints an `Other` from
/// input — never puts a known string in it, so no decoded value is ever in that
/// state. It is noted here so a later reader does not "fix" the non-bug by
/// escaping or namespacing the raw string, which WOULD break the wire. Same
/// pre-existing shape as [`crate::rc::RcKind`].
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum LaneApprovalKind {
    /// "May I run this tool / touch this file?" — answered with a
    /// [`LaneDecision`].
    Permission,
    /// A structured question with options — answered with
    /// [`LaneAnswer::Question`].
    Question,
    /// "Do you accept this plan?"
    PlanApproval,
    /// An MCP server asking the user for input through the agent.
    McpElicitation,
    /// An unrecognized kind, its raw wire string preserved.
    Other(String),
}

impl LaneApprovalKind {
    pub fn as_str(&self) -> &str {
        match self {
            LaneApprovalKind::Permission => "permission",
            LaneApprovalKind::Question => "question",
            LaneApprovalKind::PlanApproval => "plan_approval",
            LaneApprovalKind::McpElicitation => "mcp_elicitation",
            LaneApprovalKind::Other(s) => s,
        }
    }

    /// Parse a wire kind, preserving an unrecognized value as
    /// [`LaneApprovalKind::Other`] rather than failing or defaulting.
    pub fn from_wire(s: &str) -> LaneApprovalKind {
        match s {
            "permission" => LaneApprovalKind::Permission,
            "question" => LaneApprovalKind::Question,
            "plan_approval" => LaneApprovalKind::PlanApproval,
            "mcp_elicitation" => LaneApprovalKind::McpElicitation,
            other => LaneApprovalKind::Other(other.to_string()),
        }
    }

    /// A recognized kind. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, LaneApprovalKind::Other(_))
    }
}

impl Serialize for LaneApprovalKind {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for LaneApprovalKind {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(LaneApprovalKind::from_wire(&String::deserialize(d)?))
    }
}

/// Where an approval is in its life.
///
/// [`LaneApprovalStatus::Submitted`] is the optimistic middle: the client sent an
/// answer and the adapter has not yet seen the agent acknowledge it. It exists so
/// a double-tap is refusable ([`LaneError::AlreadySubmitted`]) without waiting a
/// round trip.
///
/// **Tolerant**, under the module doc's asymmetric unknown-value rule, and it is
/// the sharpest case of it: this is a STREAM value, riding inbound inside a
/// [`LaneApproval`] and therefore inside a [`LaneEvent::Approval`] frame. A strict
/// enum here means one `"status":"cancelled"` from a producer that later grows a
/// cancelled/expired state fails the decode of the WHOLE enclosing event — the
/// approval does not render conservatively, it VANISHES, exactly the failure
/// [`LaneQuestion`]'s optional booleans are defaulted to avoid. So an unrecognized
/// status is preserved verbatim as [`LaneApprovalStatus::Other`], the
/// [`LaneApprovalKind`] pattern. Because of the owned string this enum is not
/// `Copy`.
///
/// **Ask [`LaneApprovalStatus::is_pending`], never `!= Resolved`.** `Other` is
/// deliberately NOT pending: an unknown status is at least as likely to be
/// terminal (`cancelled`, `expired`) as live, and offering answer buttons for it
/// would post an answer the agent has stopped listening for. The conservative
/// render for an `Other` is the row, its raw status, and no affordance.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum LaneApprovalStatus {
    Pending,
    Submitted,
    Resolved,
    /// An unrecognized status, its raw wire string preserved.
    Other(String),
}

impl LaneApprovalStatus {
    pub fn as_str(&self) -> &str {
        match self {
            LaneApprovalStatus::Pending => "pending",
            LaneApprovalStatus::Submitted => "submitted",
            LaneApprovalStatus::Resolved => "resolved",
            LaneApprovalStatus::Other(s) => s,
        }
    }

    /// Parse a wire status, preserving an unrecognized value as
    /// [`LaneApprovalStatus::Other`] rather than failing or defaulting. Never
    /// mints an `Other` holding a known string, so the round-trip caveat on
    /// [`LaneApprovalKind`] applies here identically and is equally unreachable.
    pub fn from_wire(s: &str) -> LaneApprovalStatus {
        match s {
            "pending" => LaneApprovalStatus::Pending,
            "submitted" => LaneApprovalStatus::Submitted,
            "resolved" => LaneApprovalStatus::Resolved,
            other => LaneApprovalStatus::Other(other.to_string()),
        }
    }

    /// A recognized status. A `false` here is the neutral-render signal.
    pub fn is_known(&self) -> bool {
        !matches!(self, LaneApprovalStatus::Other(_))
    }

    /// Whether this approval is still waiting on the human — the ONLY predicate a
    /// client should gate its answer affordance on. See the type doc for why
    /// [`LaneApprovalStatus::Other`] answers `false`.
    pub fn is_pending(&self) -> bool {
        matches!(self, LaneApprovalStatus::Pending)
    }
}

impl Serialize for LaneApprovalStatus {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for LaneApprovalStatus {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        Ok(LaneApprovalStatus::from_wire(&String::deserialize(d)?))
    }
}

/// One selectable answer.
///
/// `id` is what goes back to the agent. Some agents' options have no id of their
/// own (opencode's `QuestionOption` is `label` + `description`), in which case
/// the adapter sets `id = label` — the contract keeps an id so a client never has
/// to send display text back as an identifier by accident.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneApprovalOption {
    pub id: String,
    pub label: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

/// A structured question inside an approval: a header, the question itself, and
/// how it may be answered.
///
/// `multiple` allows several options in one answer; `custom` allows free text
/// alongside (or instead of) the options, and DEFAULTS TO FALSE — an adapter that
/// cannot tell must not invite typing the agent will reject.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct LaneQuestion {
    pub header: String,
    pub question: String,
    #[serde(default)]
    pub options: Vec<LaneApprovalOption>,
    /// Absent decodes to `false`: opencode marks both this and `custom` optional
    /// on its own wire (`questions[]{…, multiple?, custom?}`), and a producer one
    /// version behind may omit them. Without the `default` a missing key fails the
    /// WHOLE enclosing [`LaneEvent`], so an approval would vanish rather than
    /// render conservatively.
    #[serde(default)]
    pub multiple: bool,
    #[serde(default)]
    pub custom: bool,
}

/// One thing waiting on the human.
///
/// A permission and a question are the same row shape on purpose: a client
/// renders ONE approval panel, and `kind` picks which affordance it attaches.
/// **[`LaneApproval::kind`] selects which field a client renders — the two are
/// never both populated.** A [`LaneApprovalKind::Permission`] puts its fixed
/// choices in `options` (`allow-once`, `allow-always`, `reject`) and leaves
/// `questions` empty; a [`LaneApprovalKind::Question`] puts the structured form in
/// `questions` — each [`LaneQuestion`] carrying its OWN `options` plus a `custom`
/// flag for free text — and leaves the top-level `options` empty. Rendering the
/// wrong one yields an approval with no buttons, so branch on `kind`, never on
/// which list happens to be non-empty.
///
/// **This type is authoritative over the transcript row's approval block.** The
/// same approval ALSO reaches a client inside
/// [`crate::rc::RcFeedMessage::approval`] on the `approval_request` row — same
/// id, but the narrower [`crate::rc::RcFeedApproval`] shape, whose status is only
/// `pending`/`resolved` and so cannot express [`LaneApprovalStatus::Submitted`].
/// That block is the TRANSCRIPT's inline rendering; take status from — and answer
/// against — this type and [`LaneEvent::Approval`], or an answered approval keeps
/// reading as pending and the [`LaneError::AlreadySubmitted`] refusal looks like
/// a bug.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneApproval {
    /// The adapter-scoped approval id — the address [`AgentLane::answer`] takes.
    pub id: String,
    /// The session this blocks. May be a DESCENDANT of the session the client
    /// subscribed to: a child session's approval still blocks the parent's agent,
    /// so it surfaces on the parent's panel even though the transcript does not.
    pub session_id: String,
    pub kind: LaneApprovalKind,
    pub status: LaneApprovalStatus,
    pub title: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
    #[serde(default)]
    pub options: Vec<LaneApprovalOption>,
    #[serde(default)]
    pub questions: Vec<LaneQuestion>,
    /// The agent's own request payload, RAW JSON in a string — not a
    /// `serde_json::Value` (the FRB-mirror rule). It is the escape hatch a client
    /// renders when `kind` is unknown, and the body [`LaneAnswer::Raw`] answers
    /// against.
    pub request_json: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub created_at_unix_ms: Option<i64>,
}

/// The three fixed decisions a permission approval accepts.
///
/// **Strict** under the module doc's asymmetric rule — this is a client→adapter
/// COMMAND, and an unrecognized decision must be rejected at decode rather than
/// tolerated: a swallowed decision is a user's "allow" silently becoming nothing.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LaneDecision {
    /// Allow this one invocation.
    AllowOnce,
    /// Allow this and every matching future invocation in the session.
    AllowAlways,
    Reject,
}

/// The answer to an approval.
///
/// [`LaneAnswer::Question::answers`] is a vec-of-vecs: one inner vec per question
/// in [`LaneApproval::questions`], each holding the option ids chosen for it (one
/// entry unless that question is `multiple`, and a free-text string when it is
/// `custom`). [`LaneAnswer::Raw`] carries a body the contract does not model, as
/// raw JSON in a `String` — the escape hatch for an unknown
/// [`LaneApprovalKind`].
///
/// **Strict** under the module doc's asymmetric rule (no `#[serde(other)]`, unlike
/// its mirror-image [`LaneEvent`]) — an answer travels client→adapter, so an
/// unrecognized `kind` is a command this build cannot honor and must be refused,
/// not degraded into a silent no-op.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum LaneAnswer {
    Permission {
        decision: LaneDecision,
    },
    Question {
        #[serde(default)]
        answers: Vec<Vec<String>>,
    },
    /// Decline the whole request — distinct from a permission's
    /// [`LaneDecision::Reject`], which is one option among three.
    Reject,
    Raw {
        json: String,
    },
}

// ---- the event stream ----

/// One frame of a [`AgentLane::subscribe`] stream.
///
/// The [`LaneEvent::Reset`] … [`LaneEvent::Ready`] bracket is the whole
/// reconnect story: see the module doc's "Semantics pinned for every adapter".
/// `generation` increments per (re)connect within one subscription and is what a
/// client discards stale frames by.
///
/// **Every payload-carrying variant nests its payload under a named key** rather
/// than flattening a newtype into the tagged object. That is forced, not
/// stylistic: `tag = "kind"` writes the discriminator into the SAME object as the
/// variant's fields, and [`LaneApproval`] has a `kind` field of its own — a
/// flattened `Approval(LaneApproval)` emits `"kind"` twice and decodes back as
/// the approval's kind, so the round trip silently loses the event. Nesting also
/// keeps the Dart sealed-class mirror uniform (one payload field per case).
///
/// **Tolerant** under the module doc's asymmetric unknown-value rule: this is THE
/// inbound type, and [`LaneEvent::Unknown`] is the catch-all that keeps one
/// unrecognized frame from failing the decode of the stream it arrived on.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum LaneEvent {
    /// A transcript row. `cursor` is the resume token on adapters that advertise
    /// [`LaneCapabilities::history_cursor`].
    Message {
        message: RcFeedMessage,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        cursor: Option<String>,
    },
    /// The session row changed (title, activity, pending count).
    Session { session: LaneSession },
    /// An approval appeared, or an existing one changed status. Id-keyed and
    /// LAST-WRITE-WINS, exactly like [`crate::rc::RcFeedApproval`]: a client must
    /// not require having seen the `pending` frame before the `resolved` one.
    Approval { approval: LaneApproval },
    /// A (re)connect started: DISCARD nothing yet, but stage every frame that
    /// follows until the matching [`LaneEvent::Ready`], then swap atomically.
    Reset { reason: String, generation: u64 },
    /// The seed for `generation` is complete — swap the staged view in.
    Ready { generation: u64 },
    /// The transport is gone and this subscription has ENDED (the adapter's task
    /// exits after emitting it). A normal state, not an error: render the panel
    /// stale-with-a-reason.
    Down { reason: String },
    /// A frame whose `kind` this build has never heard of.
    ///
    /// **Clients IGNORE an `Unknown` frame.** It is not an error, not a gap, and
    /// not a reason to resubscribe or to refetch — it is one frame this build
    /// cannot name, from a producer that grew a frame kind (a heartbeat, an error
    /// frame, a per-turn cost row) after this client was built. Without it, that
    /// one frame fails `serde`'s decode and takes the whole stream's read loop
    /// with it: every older client loses the subscription the moment a newer
    /// adapter ships. `#[serde(other)]` IS supported on an internally tagged enum,
    /// which this is.
    ///
    /// Nothing from the original object survives, by construction on both sides:
    /// `#[serde(other)]` admits only a UNIT variant, and the FRB-mirror rule bars
    /// the `serde_json::Value` that could otherwise carry the body. That is an
    /// acceptable loss — a client that cannot name the kind has nothing to do with
    /// the payload anyway — but it does mean `Unknown` must never be used to carry
    /// meaning. An adapter NEVER CONSTRUCTS it; it exists only as a decode
    /// outcome. (It does serialize, as `{"kind":"unknown"}`, so a relay that
    /// re-encodes what it decoded degrades the frame rather than corrupting it.)
    #[serde(other)]
    Unknown,
}

/// A live subscription: the frames, and the handle that ends it.
///
/// Deliberately the shape of `shed_app::roost::RoostWatcher` — a spawned task, an
/// unbounded channel, [`LaneStop::stop`] aborts, `Drop` stops — so a lane
/// subscription and a roost watcher are torn down the same way in a client that
/// holds both. Not restartable: call [`AgentLane::subscribe`] again.
///
/// # Keep BOTH halves alive — the partial move that silently kills the pump
///
/// Both fields are public (the contract pins them that way), so taking just the
/// receiver COMPILES. It is a partial move, and the abandoned `stop` half is
/// dropped — which aborts the pump, because aborting on drop IS the documented
/// teardown. The receiver you kept then yields nothing, forever, with no `Err`,
/// no [`LaneEvent::Down`] and nothing in a log. A client written this way renders
/// an empty transcript and blames the adapter.
///
/// **WRONG — dead on arrival.** When the subscription is a temporary or is owned
/// by the function doing the move, `stop` drops right there:
///
/// ```text
/// Ok(lane.subscribe(id, None).await?.rx)     // temporary: `stop` dies at the `?`
/// let rx = lane.subscribe(id, None).await?.rx;
/// fn open(..) -> Receiver<LaneEvent> { let sub = ..; sub.rx }   // dies on return
/// ```
///
/// **ALSO WRONG, and worse to debug** — a partial move out of a LOCAL leaves
/// `stop` in place until the end of that local's scope, so the frames flow for as
/// long as you are looking at them and stop the instant the receiver outlives the
/// scope it was taken in:
///
/// ```text
/// let rx = { let sub = lane.subscribe(id, None).await?; sub.rx };  // `stop` dies here
/// let LaneSubscription { rx, .. } = sub;                           // same, discarded by `..`
/// self.rx = Some(sub.rx);                                          // and stored, `stop` dropped
/// ```
///
/// Use [`LaneSubscription::into_parts`], and bind BOTH halves for as long as you
/// intend to read frames:
///
/// ```text
/// let (mut rx, stop) = lane.subscribe(id, None).await?.into_parts();
/// while let Some(ev) = rx.recv().await { render(ev); }
/// drop(stop);                       // or just let it fall out of scope
/// ```
///
/// Storing only `rx` on a long-lived struct is the same bug wearing a field name:
/// keep the [`LaneStop`] next to it (or somewhere that outlives it), which is the
/// reason the two are split at all.
pub struct LaneSubscription {
    pub rx: mpsc::UnboundedReceiver<LaneEvent>,
    pub stop: LaneStop,
}

impl LaneSubscription {
    /// Split into the frames and the lifetime handle — the named, safe idiom.
    ///
    /// Mechanically this does nothing a field move cannot; the point is that the
    /// call site is forced to say what it does with BOTH halves, where `sub.rx`
    /// lets the important one disappear without a word (see the type doc). The
    /// returned [`LaneStop`] still aborts the pump when it drops, so bind it —
    /// `let (rx, _) = ….into_parts();` reintroduces the exact bug this exists to
    /// prevent.
    pub fn into_parts(self) -> (mpsc::UnboundedReceiver<LaneEvent>, LaneStop) {
        (self.rx, self.stop)
    }
}

/// The abort handle half of a [`LaneSubscription`].
///
/// Dropping it stops the adapter's task, which is what closes the transport it
/// holds. Split out from the receiver so a client can move the frames into a
/// render loop and keep the lifetime handle somewhere else.
pub struct LaneStop {
    task: tokio::task::JoinHandle<()>,
}

impl LaneStop {
    /// Wrap the adapter's spawned pump task. The adapter calls this; a client
    /// only ever calls [`LaneStop::stop`] (or drops it).
    pub fn new(task: tokio::task::JoinHandle<()>) -> LaneStop {
        LaneStop { task }
    }

    /// Abort the pump. Dropping the in-flight future closes whatever transport
    /// it held.
    pub fn stop(&self) {
        self.task.abort();
    }
}

impl Drop for LaneStop {
    fn drop(&mut self) {
        self.stop();
    }
}

// ---- capabilities, send mode, errors ----

/// How a [`AgentLane::send`] is meant to land.
///
/// **Strict** under the module doc's asymmetric rule — a send mode is a
/// client→adapter COMMAND, so an unrecognized value is refused rather than
/// tolerated; degrading an unknown mode to a default would deliver the message
/// with semantics the caller did not ask for.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SendMode {
    /// "Accepted, and ordered after whatever is in flight."
    ///
    /// **Not a queue position.** On opencode this is `prompt_async`, which a
    /// `Working` session accepts (204) and whose runner delivers per its own
    /// rules — it joins the running runner rather than appending to a strict
    /// queue (`prompt.ts:1052`, `runner.ts:115` in the vendored source). The
    /// contract promises ACCEPTANCE and ORDERING, nothing about where in a
    /// backlog the text sits.
    Queue,
    /// Interrupt the turn in flight and deliver now. Adapters that cannot do
    /// this advertise `interject: false` and answer
    /// [`LaneError::NotAccepting`].
    Interject,
}

/// What this adapter can actually do — advertised, so a client greys out the
/// affordance instead of discovering the refusal on a tap.
///
/// Opencode's row is
/// `{ kind: "opencode", interject: false, create: true, cancel: true, approvals: true, history_cursor: false }`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LaneCapabilities {
    /// The adapter's agent token (`"opencode"`, `"gx"`, …) — the same vocabulary
    /// [`crate::rc::RcKind::tool`] speaks.
    pub kind: String,
    /// [`SendMode::Interject`] is honored.
    pub interject: bool,
    /// [`AgentLane::create`] works.
    pub create: bool,
    /// [`AgentLane::cancel`] works.
    pub cancel: bool,
    /// Approvals surface and can be answered.
    pub approvals: bool,
    /// [`AgentLane::history`] honors a cursor. When `false` the adapter refolds
    /// from the top and IGNORES the cursor argument, so a client must not treat
    /// a page as splice-able onto what it holds.
    pub history_cursor: bool,
}

/// What a lane call can go wrong with.
///
/// Split so a caller never string-matches to decide what to do: the
/// `Unknown*`/`Already*` variants are "your view is stale, refetch", `Unauthorized`
/// and `NotAccepting` are "the affordance should not have been offered",
/// `Unavailable` is quiet (the agent is not running — render stale, keep the row),
/// and `Failed` is the loud residue.
///
/// **Wire shape is heterogeneous** and that is forced, not stylistic:
/// `BadRequest(String)`/`Unavailable(String)`/`Failed(String)` are newtypes over a
/// non-map, which serde REFUSES to tag internally, so this enum is externally
/// tagged — a unit variant is a bare string (`"unknown_session"`), a payload
/// variant a one-key object (`{"bad_request":"…"}`). shed-mobile mirrors both
/// shapes by hand; a decoder that handles only one of them breaks on half the
/// enum. The `rename_all = "snake_case"` codes line up with the plan's error
/// envelope.
///
/// The VARIANTS stay **strict** (no `Other`, no `#[serde(other)]`): an error is
/// something a caller branches on to decide what to do next, and a
/// silently-tolerated unknown code would land in whatever arm it fell into —
/// "refetch, your view is stale" and "the affordance should not have been offered"
/// are not interchangeable. A code this build does not know is `Failed`'s job,
/// carried as text by whoever mapped the wire.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, thiserror::Error)]
#[serde(rename_all = "snake_case")]
pub enum LaneError {
    /// The agent demanded a credential this build cannot supply.
    #[error("the agent requires authentication")]
    Unauthorized,
    /// The agent rejected the request and said why. The string is ITS message,
    /// kept whole — it is the only thing that explains a schema-level refusal.
    #[error("{0}")]
    BadRequest(String),
    /// No such session — it ended, or was never on this adapter. Refetch the
    /// roster.
    #[error("no such session")]
    UnknownSession,
    /// No such approval — evicted, or answered elsewhere. Refetch the approvals.
    #[error("no such approval")]
    UnknownApproval,
    /// An answer is already in flight for this approval (the optimistic
    /// [`LaneApprovalStatus::Submitted`] state). A double-tap, not an error to
    /// surface loudly.
    #[error("that approval already has an answer in flight")]
    AlreadySubmitted,
    /// The approval was already resolved — by another client, or by the agent's
    /// own TUI.
    #[error("that approval is already resolved")]
    AlreadyResolved,
    /// The adapter or the session will not take this right now: an
    /// [`SendMode::Interject`] on an adapter that cannot, a prompt to a session
    /// that is closing.
    #[error("the session is not accepting that right now")]
    NotAccepting,
    /// Nothing to talk to: the agent is not running, the dial was refused, the
    /// tunnel is down. **Quiet** — the caller renders an unreachable row with
    /// this reason, not an error dialog. The string names what was tried.
    #[error("{0}")]
    Unavailable(String),
    /// Anything else that went wrong on the wire, kept whole.
    #[error("{0}")]
    Failed(String),
}

// ---- the trait ----

/// One agent, normalized.
///
/// Implemented once per agent (opencode over its local HTTP server; `gx` over its
/// `/v1` lane). Every method borrows `&self`, so the futures are `Send` under
/// `async_trait`'s default and an adapter can be held in an `Arc<dyn AgentLane>`
/// and driven from any task.
///
/// Adapters are expected to be cheap to call and to hold their own transport;
/// none of these methods is a place to build a connection per call.
#[async_trait::async_trait]
pub trait AgentLane: Send + Sync {
    /// What this adapter can do. Static for the life of the adapter — a client
    /// may cache it and gate its UI on it.
    fn capabilities(&self) -> LaneCapabilities;

    /// Every session this adapter can see. Scope is the ADAPTER's, not a
    /// directory's: opencode's session store is per-server and global, so this
    /// returns every root on that server.
    async fn sessions(&self) -> Result<Vec<LaneSession>, LaneError>;

    /// One session row.
    async fn session(&self, id: &str) -> Result<LaneSession, LaneError>;

    /// A page of transcript. `cursor` is honored only when
    /// [`LaneCapabilities::history_cursor`] is `true`; otherwise it is IGNORED
    /// and the adapter refolds from the top. `limit` is a request, not a
    /// promise — [`LaneHistory::truncated`] is the answer.
    async fn history(
        &self,
        id: &str,
        cursor: Option<&str>,
        limit: u32,
    ) -> Result<LaneHistory, LaneError>;

    /// Start a session in `cwd` and send `text` as its first prompt. Returns the
    /// new session row.
    async fn create(&self, cwd: &str, text: &str) -> Result<LaneSession, LaneError>;

    /// Send `text` to a session. See [`SendMode`] for what each mode promises.
    async fn send(&self, id: &str, text: &str, mode: SendMode) -> Result<(), LaneError>;

    /// Stop the turn in flight. A no-op turn is not an error.
    async fn cancel(&self, id: &str) -> Result<(), LaneError>;

    /// Everything open on this session — INCLUDING its descendants' approvals,
    /// which block the same agent even though their transcript rows do not
    /// surface.
    async fn approvals(&self, id: &str) -> Result<Vec<LaneApproval>, LaneError>;

    /// Answer one approval. `id` is the session, `approval_id` the approval on
    /// it. A second answer to the same approval is refused with
    /// [`LaneError::AlreadySubmitted`] or [`LaneError::AlreadyResolved`].
    async fn answer(
        &self,
        id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<(), LaneError>;

    /// Open a live stream for one session.
    ///
    /// The adapter spawns its own pump and hands back the receiver plus the stop
    /// handle. The stream opens with a [`LaneEvent::Reset`] and reaches steady
    /// state at the matching [`LaneEvent::Ready`]; it ENDS at a
    /// [`LaneEvent::Down`]. `cursor` is a resume hint, honored under the same
    /// [`LaneCapabilities::history_cursor`] rule as [`AgentLane::history`]. Two
    /// subscriptions to one session are two independent streams (and, on
    /// opencode, two connections).
    ///
    /// Take the result apart with [`LaneSubscription::into_parts`] and keep BOTH
    /// halves — `…await?.rx` compiles, drops the stop handle, and hands back a
    /// receiver that will never yield a frame. See [`LaneSubscription`].
    async fn subscribe(
        &self,
        id: &str,
        cursor: Option<String>,
    ) -> Result<LaneSubscription, LaneError>;
}

#[cfg(test)]
mod tests;
