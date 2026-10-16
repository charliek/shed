//! craze's refusals as the contract's [`LaneError`] (plan 025 §3.3.7, P14).
//!
//! craze publishes its own code → `LaneError` table (craze
//! `docs/reference/protocol.md`, "The code → shed `LaneError` table"): every
//! craze `data.code` onto one of the contract's nine variants. It is
//! reproduced here **verbatim and keyed on `data.code` ONLY** — never
//! `data.reason`, never `message` — because craze's own rule is that a client
//! decides everything from the code, and a caller of this crate then branches
//! on the variant, never on the text. `shed_core::lane`'s module doc carries
//! the same table; `tests/wire.rs` asserts it row by row.
//!
//! **One deviation, P14:** a `session.create` refused `not_accepting` with
//! reason `start_failed` is [`LaneError::Failed`] carrying `data.cause` (else
//! "the session failed to start"), because [`LaneError::NotAccepting`] is a
//! unit variant and the create flow must show the start failure's cause —
//! craze's macOS login-session hint included. Only that reason, and only on
//! create: [`create_error`]. Everywhere else, [`lane_error`].
//!
//! **And one more on a REPLAYED create** (plan 026 §3.1, AM1): a create sent
//! under a request id whose earlier create's outcome is unknown — the
//! source's own retry, or a caller replaying a held id
//! ([`AgentSource::create_held`]) — that craze refuses with a reason that does
//! not state THAT request's outcome is [`outcome_unknown`]: `unavailable` with
//! `host_unreachable` (its registry of session hosts could not be read, so
//! whether that id started a session "cannot be known: try again"), `busy`
//! (too many creates in flight) or `closing` (the hub's teardown began — even
//! after it found the id's LIVE session in its registry, without joining it),
//! and the rest of [`REPLAY_UNKNOWN`]'s audit. Ending the id there would let
//! the next press start a second session while the first runs. A FRESH
//! create refused so started nothing (craze refuses rather than spawn, and a
//! teardown ends what it cut), and keeps the table's variant — except
//! `failed`/`failed` and `failed`/`response_too_large`, which craze sends only
//! in place of a create SUCCESS it could not send ([`SUCCESS_UNSENT`]): the
//! session exists, so they are unknown at every attempt.
//!
//! [`AgentSource::create_held`]: shed_core::lane::AgentSource::create_held
//!
//! **And a rename's `index_write`** (plan 029 P19): `session.setTitle`
//! answered `index_write` RAN — the session is renamed and its `meta` delta
//! says so — and only craze's session index could not record it. It stays the
//! table's [`LaneError::Failed`], worded [`RENAMED_UNRECORDED`] + craze's
//! `data.cause` ([`set_title_error`]), and [`index_write_cause`] is how a
//! client tells it from a rename that failed: a warning beside the new
//! title, never a rollback or a retry.
//!
//! **And a reopen's own error** (plan 032 §3.4, PC10): `session.reopen`'s
//! refusals become [`LaneReopenError`], whose variants say what a client does
//! next — drop the saved row, show the cause, retry ([`reopen_error`]). Its
//! `not_accepting` row is **the documented exception to "keyed on `data.code`
//! ONLY"** (PC14e): craze's own reopen table gives the reasons
//! `not_resumable` (drop the row: `Gone`) and `start_failed` (show the cause,
//! keep the row: `StartFailed`) opposite client actions, and the contract's
//! `NotAccepting` is a unit variant that could carry neither. Only that code,
//! and only on `session.reopen`; `shed_core::lane`'s module doc and
//! `crates/CLAUDE.md` carry the same exception. A hub that cannot reopen at
//! all — its `hello` without `reopen`, or a refusal `unsupported` /
//! `unknown_method` — is said in one set of words, [`reopen_unsupported`],
//! which never infers a version it was not told; and an answer that never came
//! is [`REOPEN_LOST`], `Unavailable` — a reopen is idempotent, so Retry is
//! safe (PC14d), never a separate "outcome unknown".
//!
//! A code this build does not know (or a refusal with no `data.code` at all)
//! is [`LaneError::Failed`] with the message — the contract's rule that an
//! unknown code is `Failed`'s job.
//!
//! **craze's words never wear shed's "outcome unknown"** (nor its
//! [`RENAMED_UNRECORDED`] or [`REOPEN_LOST`]): a client keeps a create's
//! request id, and the desktop shows a change "not confirmed", only on
//! [`is_outcome_unknown`] — which reads a `Failed`'s text prefix, because the
//! contract's `LaneError` has no variant for it. So every text craze (or the
//! far side of a dial) wrote that becomes a message here goes through
//! [`craze_says`], which escapes one that happens to begin with any of the
//! three: only shed's own [`outcome_unknown`], [`set_title_error`] and
//! [`reopen_lost`] carry them.

use shed_core::lane::{LaneError, LaneReopenError};

use crate::conn::{version_or_unknown, HubHello};
use crate::wire::{code, reason, RpcError};

/// craze's own text (a refusal's message, a start failure's cause, a refused
/// `hello`'s or `session.connect`'s words) as a [`LaneError`]'s message:
/// verbatim — unless it begins with shed's [`OUTCOME_UNKNOWN`], which marks a
/// request whose answer was LOST and is never a definite answer's, with its
/// [`RENAMED_UNRECORDED`], or with its [`REOPEN_LOST`]: then it is said as
/// craze's (`craze: outcome unknown: …`) and neither [`is_outcome_unknown`],
/// [`index_write_cause`] nor [`is_reopen_lost`] can mistake it for shed's own.
pub fn craze_says(text: impl Into<String>) -> String {
    let text = text.into();
    if text.starts_with(OUTCOME_UNKNOWN)
        || text.starts_with(RENAMED_UNRECORDED)
        || text.starts_with(REOPEN_LOST)
    {
        format!("craze: {text}")
    } else {
        text
    }
}

/// The table, verbatim: one craze `data.code` → one [`LaneError`].
pub fn lane_error(e: &RpcError) -> LaneError {
    let text = craze_says(e.text());
    match e.data_code.as_deref() {
        Some(code::BAD_REQUEST | code::STALE_VERSION | code::STALE_TURN) => {
            LaneError::BadRequest(text)
        }
        Some(code::UNKNOWN_SESSION) => LaneError::UnknownSession,
        Some(code::UNKNOWN_ASK) => LaneError::UnknownApproval,
        Some(code::ALREADY_SUBMITTED) => LaneError::AlreadySubmitted,
        Some(code::ALREADY_RESOLVED) => LaneError::AlreadyResolved,
        Some(code::NOT_ACCEPTING | code::FOREIGN_TURN | code::IN_PROGRESS | code::STALE_MODEL) => {
            LaneError::NotAccepting
        }
        Some(code::UNAVAILABLE) => LaneError::Unavailable(text),
        Some(
            code::UNSUPPORTED
            | code::QUEUE_FULL
            | code::TEXT_TOO_LONG
            | code::PROMPT_IN_FLIGHT
            | code::PROMPT_CANCELLED
            | code::UNKNOWN_ROW
            | code::UNKNOWN_COMMAND
            | code::UNKNOWN_SUBAGENT
            | code::ABORTED
            | code::FAILED
            | code::INDEX_WRITE,
        ) => LaneError::Failed(text),
        // No `Unauthorized` row: protocol 1 has no authentication.
        _ => LaneError::Failed(text),
    }
}

/// What a session that failed to start says when craze gives no cause.
pub const START_FAILED_FALLBACK: &str = "the session failed to start";

/// A start failure's cause as shown: craze's own text, trimmed, else
/// [`START_FAILED_FALLBACK`] — a create's refusal, a lane's attach refusal and
/// a `ready` that failed all word it this one way.
pub fn start_failed_cause(cause: Option<&str>) -> &str {
    said_cause(cause).unwrap_or(START_FAILED_FALLBACK)
}

/// craze's `data.cause`, trimmed — `None` when it gave none, or a blank one.
fn said_cause(cause: Option<&str>) -> Option<&str> {
    cause.map(str::trim).filter(|c| !c.is_empty())
}

/// craze's `session.create` refusals that do NOT state the request's outcome
/// — admission, connection state, or a success that could not be sent — so a
/// REPLAYED request refused with one is still unknown (plan 026 §3.1, AM1).
///
/// **The audit**, every refusal craze v0.1.0 (`8d5676c`, `internal/hub/`) can
/// answer a `session.create` line with:
///
/// | `data.code`/`reason` | where | verdict |
/// |---|---|---|
/// | `unavailable`/`host_unreachable` | `create.go:595,600` (admit's registry read), `:783,:798` (join: the started session cannot be reached, or went) | unknown on a replay |
/// | `unavailable`/`busy` | `lifecycle.go:952-954` (`beginCreate`, from admit `create.go:533,:546`) | unknown on a replay |
/// | `unavailable`/`closing` | `lifecycle.go:950-951` via admit `create.go:533` (the registry branch: the session is ALIVE and not joined) and `:546`; `create.go:549,:714,:746,:754`; join `:781,:789`; `notReady` `:822` | unknown on a replay |
/// | `unsupported`/`unsupported` | `create.go:308` (a hub with no way to create) | unknown on a replay |
/// | `unsupported`/`unknown_method` | `server.go:312-314` | unknown on a replay |
/// | `bad_request`/`hello_required` | `server.go:318` (connection state, not the request) | unknown on a replay |
/// | `failed`/`failed`, `failed`/`response_too_large` | `server.go:568,:571` (`encodeReply`, their only source: a SUCCESS whose reply could not be encoded or sent — the session exists) | unknown at EVERY attempt ([`SUCCESS_UNSENT`]) |
/// | `bad_request`/`bad_request`, `bad_request`/`unknown_field`, `bad_request`/`line_too_long` | `create.go:349-435` (parse), `server.go:212,:307,:761-799` | definite: a function of the request's bytes, the same on every attempt |
/// | `bad_request`/`request_conflict` | `create.go:518,:529` (memory or registry hold the id with other params) | definite |
/// | `bad_request`/`bad_request` (the world) | `create.go:445-464` (`check`: only after memory AND registry showed no create of the id) | definite |
/// | `unavailable`/`spawn_failed` | `create.go:686,:702,:719,:735,:740,:749,:762`; `notReady` `:840` | definite: the create ran and failed (and is replayed so) |
/// | `not_accepting`/`start_failed` | `create.go:758`; join `:795` | definite: craze's own answer that the start failed. A fresh create's host is the hub's child and is ended (`abandonHost`); a registry-recovered one (join) is asked to stop and craze v0.1.0 reports `start_failed` even when that stop fails (`create.go:790-795`) — a craze-side gap, recorded in plan 026 §9, not classified away here |
///
/// No answer at all — the teardown's cut (`create.go:286-299,:320-322`) or a
/// hub that is closing (`server.go:246-253`) ends the connection unanswered —
/// is a lost answer already ([`crate::conn::CallError`]), unknown on every
/// attempt.
pub const REPLAY_UNKNOWN: &[(&str, &str)] = &[
    (code::UNAVAILABLE, reason::HOST_UNREACHABLE),
    (code::UNAVAILABLE, reason::BUSY),
    (code::UNAVAILABLE, reason::CLOSING),
    (code::UNSUPPORTED, reason::UNSUPPORTED),
    (code::UNSUPPORTED, reason::UNKNOWN_METHOD),
    (code::BAD_REQUEST, reason::HELLO_REQUIRED),
];

/// craze's refusals that stand in for a `session.create` SUCCESS it could not
/// send (`server.go:553-572`, `encodeReply` — their only source at v0.1.0):
/// the session exists, so the outcome is unknown at EVERY attempt, a fresh
/// one included — like a success shed cannot decode (plan 026 AM1).
pub const SUCCESS_UNSENT: &[(&str, &str)] = &[
    (code::FAILED, reason::FAILED),
    (code::FAILED, reason::RESPONSE_TOO_LARGE),
];

/// `session.create`'s refusal: the table, except P14's one row; a refusal
/// standing in for a success craze could not send ([`SUCCESS_UNSENT`]) is
/// unknown at any attempt; and, when the create `replays` a request id whose
/// earlier outcome is unknown, a refusal that does not state that request's
/// outcome ([`REPLAY_UNKNOWN`]) leaves it unknown too ([`outcome_unknown`], so
/// the caller keeps the id). A FRESH create's other refusals stay definite.
pub fn create_error(e: &RpcError, replays: bool) -> LaneError {
    let pair = (e.data_code.as_deref(), e.reason.as_deref());
    if let (Some(code::NOT_ACCEPTING), Some(reason::START_FAILED)) = pair {
        return LaneError::Failed(craze_says(start_failed_cause(e.cause.as_deref())));
    }
    if let (Some(c), Some(r)) = pair {
        if SUCCESS_UNSENT.contains(&(c, r)) {
            return outcome_unknown(&format!(
                "craze's {c}/{r} stands in for a create answer it could not send: the session may exist: {}",
                craze_says(e.text())
            ));
        }
        if replays && REPLAY_UNKNOWN.contains(&(c, r)) {
            return outcome_unknown(&format!(
                "craze's {c}/{r} says nothing about whether this request started a session: {}",
                craze_says(e.text())
            ));
        }
    }
    lane_error(e)
}

/// What a rename that RAN but could not be recorded says, before craze's cause
/// (plan 029 P19) — a client's warning, in the words it shows.
pub const RENAMED_UNRECORDED: &str = "renamed, but craze could not record it: ";

/// `session.setTitle`'s refusal: the table, except `index_write` — the rename
/// happened and only the session index write failed — which is
/// [`RENAMED_UNRECORDED`] and craze's `data.cause` (else its message).
pub fn set_title_error(e: &RpcError) -> LaneError {
    if e.data_code.as_deref() != Some(code::INDEX_WRITE) {
        return lane_error(e);
    }
    let cause = said_cause(e.cause.as_deref()).map_or_else(|| e.text(), str::to_string);
    LaneError::Failed(format!("{RENAMED_UNRECORDED}{cause}"))
}

/// craze's cause when `e` is [`set_title_error`]'s `index_write` — a rename
/// that took and was not recorded, a warning and not a failure — else `None`.
/// One implementation, so no client string-matches it on its own.
pub fn index_write_cause(e: &LaneError) -> Option<&str> {
    failed_after(e, RENAMED_UNRECORDED)
}

/// What every client-side "outcome unknown" error's text starts with.
pub const OUTCOME_UNKNOWN: &str = "outcome unknown: ";

/// The client-side "outcome unknown" (craze's `resume_lost`/`disconnected`/
/// `no_answer`, which no host ever sends): a request was written and its answer
/// never came, so whether it ran is not known. `what` says why.
pub fn outcome_unknown(what: &str) -> LaneError {
    LaneError::Failed(format!("{OUTCOME_UNKNOWN}{what}"))
}

/// Whether `e` is [`outcome_unknown`]'s — the one answer after which a create's
/// caller KEEPS its request id (plan 025 §3.3.3, §3.8: any other answer, a
/// refusal included, ends the id's life). One implementation, so no client
/// string-matches it on its own.
pub fn is_outcome_unknown(e: &LaneError) -> bool {
    unknown_why(e).is_some()
}

/// Why [`outcome_unknown`]'s error is unknown — its text after the prefix —
/// or `None` for any other error.
pub fn unknown_why(e: &LaneError) -> Option<&str> {
    failed_after(e, OUTCOME_UNKNOWN)
}

/// A `Failed`'s text after shed's own `prefix`, or `None`.
fn failed_after<'a>(e: &'a LaneError, prefix: &str) -> Option<&'a str> {
    match e {
        LaneError::Failed(m) => m.strip_prefix(prefix),
        _ => None,
    }
}

// ---- a reopen's refusals (plan 032 §3.4) ----

/// What a reopen whose answer was LOST says — the connection ended or the
/// deadline passed on both tries, the retry's dial failed, or craze answered
/// with a success shed cannot read: the session may be running, and since a
/// reopen means "ensure it is running", trying again is safe (PC10, PC14d).
/// A caller's [`LaneReopenError::Unavailable`] starts with these words; what
/// was lost follows in parentheses.
pub const REOPEN_LOST: &str =
    "the reopen's answer was lost; the session may already be running — Retry is safe";

/// [`REOPEN_LOST`], and what was lost.
pub fn reopen_lost(why: &str) -> LaneReopenError {
    LaneReopenError::Unavailable(format!("{REOPEN_LOST} ({why})"))
}

/// Whether `e` is [`reopen_lost`]'s — the session may be running though no
/// answer said so. One implementation, so no client string-matches it on its
/// own. craze's own `unavailable` words never read as it: [`craze_says`]
/// escapes a message that happens to begin with [`REOPEN_LOST`].
pub fn is_reopen_lost(e: &LaneReopenError) -> bool {
    matches!(e, LaneReopenError::Unavailable(m) if m.starts_with(REOPEN_LOST))
}

/// The first craze version that reopens saved sessions: a hub that says it is
/// older, and says its pid, is one an upgrade left running.
const REOPEN_SINCE: (u64, u64, u64) = (0, 2, 0);

/// The words for a hub that does not offer reopening saved sessions — its
/// `hello` without `reopen`, or a refusal `unsupported`/`unknown_method` —
/// in the ONE wording every client says it in (the source's gate, and the
/// desktop's live-capability gate, PC14i).
pub fn reopen_unsupported(hello: &HubHello) -> String {
    reopen_unsupported_by(&hello.craze_version, hello.pid)
}

/// [`reopen_unsupported`] from what a caller knows of the hub: its craze
/// version (`""` when unknown) and its pid. The upgrade remedy — a hub from
/// before craze 0.2.0 outlives an upgrade until it is idle, and `kill <pid>`
/// ends it — is said ONLY when the version parses as one below 0.2.0 and the
/// pid is known: the words never infer a version (PC10), so a `dev` or test
/// build, or no version at all, gets the plain sentence.
pub fn reopen_unsupported_by(craze_version: &str, pid: Option<i64>) -> String {
    let text = format!(
        "this craze hub (craze {}) does not offer reopening saved sessions",
        version_or_unknown(craze_version)
    );
    let older = parse_version(craze_version).is_some_and(|v| v < REOPEN_SINCE);
    match pid.filter(|p| older && *p > 0) {
        Some(pid) => format!(
            "{text}; a hub from before craze 0.2.0 keeps running after an upgrade until it is idle — end it with kill {pid}"
        ),
        None => text,
    }
}

/// A craze version as `(major, minor, patch)` — an optional `v`, three decimal
/// numbers, anything after a `-` or `+` ignored — or `None` for anything else
/// (`dev`, a test build's `test-…`, empty).
fn parse_version(v: &str) -> Option<(u64, u64, u64)> {
    let core = v.strip_prefix('v').unwrap_or(v).split(['-', '+']).next()?;
    let parts: Vec<u64> = core
        .split('.')
        .map(|p| p.parse().ok())
        .collect::<Option<_>>()?;
    match parts[..] {
        [major, minor, patch] => Some((major, minor, patch)),
        _ => None,
    }
}

/// Whether `e` is a hub that cannot answer a reopen at all: `unsupported`
/// (reason `unsupported` from a hub given no answer, `unknown_method` from one
/// from before the method), or a bare JSON-RPC `-32601` with no `data.code`.
pub fn is_reopen_unsupported(e: &RpcError) -> bool {
    match e.data_code.as_deref() {
        Some(c) => c == code::UNSUPPORTED,
        None => e.code == -32601,
    }
}

/// `session.reopen`'s refusal as [`LaneReopenError`] (the module doc; the
/// table is `shed_core::lane`'s too):
///
/// | craze's answer | [`LaneReopenError`] |
/// |---|---|
/// | `unknown_session` | `Gone` (craze's message) |
/// | `not_accepting` / `not_resumable` | `Gone` (craze's message) |
/// | `not_accepting` / `start_failed` | `StartFailed("could not start: <data.cause, else the message>")` |
/// | `unavailable` / any reason | `Unavailable` (craze's message) |
/// | `unsupported` (any reason), a bare `-32601` | `Unavailable(`[`reopen_unsupported`]`)` |
/// | `bad_request` | `Lane(BadRequest)` |
/// | anything else | `Lane(Failed)` (craze's message) |
///
/// The `not_accepting` split is the one place a reason decides the variant
/// (PC14e).
pub fn reopen_error(e: &RpcError, hello: &HubHello) -> LaneReopenError {
    if is_reopen_unsupported(e) {
        return LaneReopenError::Unavailable(reopen_unsupported(hello));
    }
    let text = craze_says(e.text());
    match (e.data_code.as_deref(), e.reason.as_deref()) {
        (Some(code::UNKNOWN_SESSION), _) => LaneReopenError::Gone(text),
        (Some(code::NOT_ACCEPTING), Some(reason::NOT_RESUMABLE)) => LaneReopenError::Gone(text),
        (Some(code::NOT_ACCEPTING), Some(reason::START_FAILED)) => {
            let cause = said_cause(e.cause.as_deref()).map_or(text, craze_says);
            LaneReopenError::StartFailed(format!("could not start: {cause}"))
        }
        (Some(code::UNAVAILABLE), _) => LaneReopenError::Unavailable(text),
        (Some(code::BAD_REQUEST), _) => LaneReopenError::Lane(LaneError::BadRequest(text)),
        _ => LaneReopenError::Lane(LaneError::Failed(text)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn refusal(code: &str, reason: &str) -> RpcError {
        RpcError::from_value(
            &json!({"code": -32000, "message": format!("craze says {code}"),
            "data": {"code": code, "reason": reason}}),
        )
    }

    /// craze's published table (PM "The code → shed `LaneError` table"), row
    /// by row — every one of craze's twenty-three codes.
    #[test]
    fn the_published_table_row_by_row() {
        let text = |c: &str| format!("craze says {c}");
        let rows: Vec<(&str, LaneError)> = vec![
            ("bad_request", LaneError::BadRequest(text("bad_request"))),
            (
                "stale_version",
                LaneError::BadRequest(text("stale_version")),
            ),
            ("stale_turn", LaneError::BadRequest(text("stale_turn"))),
            ("unknown_session", LaneError::UnknownSession),
            ("unknown_ask", LaneError::UnknownApproval),
            ("already_submitted", LaneError::AlreadySubmitted),
            ("already_resolved", LaneError::AlreadyResolved),
            ("not_accepting", LaneError::NotAccepting),
            ("foreign_turn", LaneError::NotAccepting),
            ("in_progress", LaneError::NotAccepting),
            ("stale_model", LaneError::NotAccepting),
            ("unavailable", LaneError::Unavailable(text("unavailable"))),
            ("unsupported", LaneError::Failed(text("unsupported"))),
            ("queue_full", LaneError::Failed(text("queue_full"))),
            ("text_too_long", LaneError::Failed(text("text_too_long"))),
            (
                "prompt_in_flight",
                LaneError::Failed(text("prompt_in_flight")),
            ),
            (
                "prompt_cancelled",
                LaneError::Failed(text("prompt_cancelled")),
            ),
            ("unknown_row", LaneError::Failed(text("unknown_row"))),
            (
                "unknown_command",
                LaneError::Failed(text("unknown_command")),
            ),
            (
                "unknown_subagent",
                LaneError::Failed(text("unknown_subagent")),
            ),
            ("aborted", LaneError::Failed(text("aborted"))),
            ("failed", LaneError::Failed(text("failed"))),
            ("index_write", LaneError::Failed(text("index_write"))),
        ];
        assert_eq!(rows.len(), 23, "craze's code set has twenty-three codes");
        for (code, want) in rows {
            assert_eq!(lane_error(&refusal(code, code)), want, "data.code {code}");
        }
    }

    /// Keyed on `data.code` ONLY: the reason and the message never move a
    /// refusal to another variant.
    #[test]
    fn the_reason_never_decides() {
        assert_eq!(
            lane_error(&refusal("not_accepting", "start_failed")),
            LaneError::NotAccepting
        );
        assert_eq!(
            lane_error(&refusal("bad_request", "protocol_version")),
            LaneError::BadRequest("craze says bad_request".into())
        );
        // An unknown code, and a refusal with no data at all, are Failed.
        assert_eq!(
            lane_error(&refusal("brand_new_code", "x")),
            LaneError::Failed("craze says brand_new_code".into())
        );
        assert_eq!(
            lane_error(&RpcError::from_value(
                &json!({"code": -32601, "message": "unknown method"})
            )),
            LaneError::Failed("unknown method".into())
        );
    }

    /// P14: on create, `not_accepting/start_failed` carries its cause.
    #[test]
    fn a_create_that_failed_to_start_says_why() {
        let e = RpcError::from_value(
            &json!({"code": -32000, "message": "the session did not start: x",
            "data": {"code": "not_accepting", "reason": "start_failed",
                     "cause": "acp: agent exited: exit status 1: Error: KEYCHAIN LOCKED / Run unlock and retry."}}),
        );
        assert_eq!(
            create_error(&e, false),
            LaneError::Failed(
                "acp: agent exited: exit status 1: Error: KEYCHAIN LOCKED / Run unlock and retry."
                    .into()
            )
        );
        let no_cause = refusal("not_accepting", "start_failed");
        assert_eq!(
            create_error(&no_cause, false),
            LaneError::Failed(START_FAILED_FALLBACK.into())
        );
        // Only that reason: any other not_accepting on create follows the table.
        assert_eq!(
            create_error(&refusal("not_accepting", "not_accepting"), false),
            LaneError::NotAccepting
        );
        assert_eq!(
            create_error(&refusal("unavailable", "spawn_failed"), false),
            LaneError::Unavailable("craze says unavailable".into())
        );
    }

    /// **A replayed create** (plan 026 §3.1, AM1): every refusal craze v0.1.0
    /// can give a `session.create` that does not state the request's outcome
    /// — `host_unreachable`, `busy`, `closing`, a hub that cannot create, a
    /// connection without its `hello`, a success that could not be sent — is
    /// unknown; every refusal that states it stays definite. A FRESH create's
    /// refusals are all definite.
    #[test]
    fn a_replayed_create_craze_could_not_answer_for_stays_unknown() {
        for (code, why) in [
            ("unavailable", "host_unreachable"),
            ("unavailable", "busy"),
            ("unavailable", "closing"),
            ("unsupported", "unsupported"),
            ("unsupported", "unknown_method"),
            ("bad_request", "hello_required"),
        ] {
            let e = refusal(code, why);
            assert!(
                is_outcome_unknown(&create_error(&e, true)),
                "{code}/{why} on a replay: {:?}",
                create_error(&e, true)
            );
            assert_eq!(
                create_error(&e, false),
                lane_error(&e),
                "{code}/{why} on a fresh create: the table's"
            );
            assert!(!is_outcome_unknown(&create_error(&e, false)));
        }
        // A refusal standing in for a success craze could not send: the
        // session exists, so unknown at EVERY attempt, a fresh one included.
        for why in ["failed", "response_too_large"] {
            let e = refusal("failed", why);
            for replays in [false, true] {
                assert!(
                    is_outcome_unknown(&create_error(&e, replays)),
                    "failed/{why} (replay {replays}): {:?}",
                    create_error(&e, replays)
                );
            }
        }
        for (code, why) in [
            ("unavailable", "spawn_failed"),
            ("bad_request", "request_conflict"),
            ("bad_request", "bad_request"),
            ("bad_request", "unknown_field"),
            ("bad_request", "line_too_long"),
            ("not_accepting", "start_failed"),
            ("failed", "host_unreachable"),
            ("unavailable", "not_ready"),
        ] {
            let e = refusal(code, why);
            assert!(
                !is_outcome_unknown(&create_error(&e, true)),
                "{code}/{why} is craze's answer about the request"
            );
            assert_eq!(
                create_error(&e, true),
                create_error(&e, false),
                "{code}/{why}"
            );
        }
    }

    #[test]
    fn outcome_unknown_is_recognized_and_nothing_else_is() {
        assert!(is_outcome_unknown(&outcome_unknown(
            "the connection dropped"
        )));
        assert!(!is_outcome_unknown(&LaneError::Failed(
            "the session failed to start".into()
        )));
        assert!(!is_outcome_unknown(&LaneError::Unavailable(
            "outcome unknown: no".into()
        )));
    }

    /// **craze's words never wear shed's "outcome unknown"** (C11 review): a
    /// DEFINITE craze failure whose message happens to begin with the prefix
    /// — a refusal, and a create's start-failure cause — is said as craze's,
    /// so [`is_outcome_unknown`] (a create keeping its id, the desktop's "not
    /// confirmed") never takes it for a lost answer. Any other text is
    /// untouched.
    #[test]
    fn a_craze_message_never_reads_as_outcome_unknown() {
        let failed = RpcError::from_value(
            &json!({"code": -32000, "message": "outcome unknown: x", "data": {"code": "failed", "reason": "failed"}}),
        );
        let e = lane_error(&failed);
        assert_eq!(e, LaneError::Failed("craze: outcome unknown: x".into()));
        assert!(
            !is_outcome_unknown(&e),
            "a definite failure, not a lost answer"
        );
        let start = RpcError::from_value(
            &json!({"code": -32000, "message": "m", "data": {"code": "not_accepting",
                    "reason": "start_failed", "cause": "outcome unknown: the agent said so"}}),
        );
        assert!(!is_outcome_unknown(&create_error(&start, false)));
        assert!(!is_outcome_unknown(&create_error(&start, true)));
        assert_eq!(craze_says("craze says failed"), "craze says failed");
        // Nor shed's rename warning.
        let renamed = RpcError::from_value(
            &json!({"code": -32000, "message": "renamed, but craze could not record it: x",
                    "data": {"code": "failed", "reason": "failed"}}),
        );
        assert_eq!(index_write_cause(&lane_error(&renamed)), None);
        assert_eq!(index_write_cause(&set_title_error(&renamed)), None);
    }

    /// **A rename's `index_write`** (plan 029 P19): the session WAS renamed;
    /// only its index could not be written. `Failed` in the table's words
    /// for every other refusal, and for this one [`RENAMED_UNRECORDED`] with
    /// craze's `data.cause` (its message when it gives none) — which
    /// [`index_write_cause`] reads back. Every other code keeps the table's
    /// variant.
    #[test]
    fn a_renames_index_write_is_the_renamed_warning_with_crazes_cause() {
        let index = RpcError::from_value(&json!({"code": -32000,
            "message": "engine: the session index could not be written: open /x/index.json: permission denied",
            "data": {"code": "index_write", "reason": "index_write",
                     "cause": "open /x/index.json: permission denied"}}));
        let e = set_title_error(&index);
        assert_eq!(
            e,
            LaneError::Failed(
                "renamed, but craze could not record it: open /x/index.json: permission denied"
                    .into()
            )
        );
        assert_eq!(
            index_write_cause(&e),
            Some("open /x/index.json: permission denied")
        );
        assert!(!is_outcome_unknown(&e), "it ran: a definite answer");
        let no_cause = refusal("index_write", "index_write");
        assert_eq!(
            index_write_cause(&set_title_error(&no_cause)),
            Some("craze says index_write"),
            "no cause: craze's message"
        );
        // The table's own row for every method but this one.
        assert_eq!(
            lane_error(&index),
            LaneError::Failed(
                "engine: the session index could not be written: open /x/index.json: permission denied"
                    .into()
            )
        );
        assert_eq!(index_write_cause(&lane_error(&index)), None);
        for (code, want) in [
            (
                "bad_request",
                LaneError::BadRequest("craze says bad_request".into()),
            ),
            ("not_accepting", LaneError::NotAccepting),
            (
                "unsupported",
                LaneError::Failed("craze says unsupported".into()),
            ),
        ] {
            assert_eq!(set_title_error(&refusal(code, code)), want, "{code}");
        }
    }

    /// A hub `hello` as a reopen sees it: `version`, `pid`, nothing else read.
    fn hub(version: &str, pid: Option<i64>) -> HubHello {
        HubHello {
            epoch: "0a1b2c3d4e5f".into(),
            craze_version: version.into(),
            pid,
            capabilities: crate::wire::ConnCapabilities::default(),
        }
    }

    /// **The reopen table, row by row** (plan 032 §3.4, PC10): each craze
    /// answer lands on the variant that says what a client does next — and
    /// `not_accepting` is split on its reason (the documented exception,
    /// PC14e) while every other method keeps the code-only table.
    #[test]
    fn the_reopen_table_row_by_row() {
        let h = hub("0.2.0", Some(5150));
        let with = |code: &str, reason: &str, cause: Option<&str>| {
            RpcError::from_value(&json!({"code": -32000,
                "message": format!("craze says {code}/{reason}"),
                "data": {"code": code, "reason": reason, "cause": cause}}))
        };
        let rows: Vec<(RpcError, LaneReopenError)> = vec![
            (
                with("unknown_session", "unknown_session", None),
                LaneReopenError::Gone("craze says unknown_session/unknown_session".into()),
            ),
            (
                with("not_accepting", "not_resumable", None),
                LaneReopenError::Gone("craze says not_accepting/not_resumable".into()),
            ),
            (
                with(
                    "not_accepting",
                    "start_failed",
                    Some(" acp: agent exited: KEYCHAIN LOCKED "),
                ),
                LaneReopenError::StartFailed(
                    "could not start: acp: agent exited: KEYCHAIN LOCKED".into(),
                ),
            ),
            (
                with("not_accepting", "start_failed", None),
                LaneReopenError::StartFailed(
                    "could not start: craze says not_accepting/start_failed".into(),
                ),
            ),
            (
                with("unavailable", "busy", None),
                LaneReopenError::Unavailable("craze says unavailable/busy".into()),
            ),
            (
                with("unavailable", "closing", None),
                LaneReopenError::Unavailable("craze says unavailable/closing".into()),
            ),
            (
                with("unavailable", "spawn_failed", None),
                LaneReopenError::Unavailable("craze says unavailable/spawn_failed".into()),
            ),
            (
                with("unsupported", "unknown_method", None),
                LaneReopenError::Unavailable(reopen_unsupported(&h)),
            ),
            (
                with("unsupported", "unsupported", None),
                LaneReopenError::Unavailable(reopen_unsupported(&h)),
            ),
            (
                RpcError::from_value(&json!({"code": -32601, "message": "method not found"})),
                LaneReopenError::Unavailable(reopen_unsupported(&h)),
            ),
            (
                with("bad_request", "unknown_field", None),
                LaneReopenError::Lane(LaneError::BadRequest(
                    "craze says bad_request/unknown_field".into(),
                )),
            ),
            (
                with("not_accepting", "not_accepting", None),
                LaneReopenError::Lane(LaneError::Failed(
                    "craze says not_accepting/not_accepting".into(),
                )),
            ),
            (
                with("failed", "failed", None),
                LaneReopenError::Lane(LaneError::Failed("craze says failed/failed".into())),
            ),
            (
                with("brand_new_code", "x", None),
                LaneReopenError::Lane(LaneError::Failed("craze says brand_new_code/x".into())),
            ),
        ];
        for (refusal, want) in rows {
            assert_eq!(reopen_error(&refusal, &h), want, "{refusal}");
        }
        // The exception is the reopen's alone: the code-only table still
        // reads `not_accepting/not_resumable` as `NotAccepting`.
        assert_eq!(
            lane_error(&with("not_accepting", "not_resumable", None)),
            LaneError::NotAccepting
        );
        // craze's words never wear shed's "outcome unknown", here either.
        let sneaky = with("not_accepting", "start_failed", Some("outcome unknown: x"));
        assert_eq!(
            reopen_error(&sneaky, &h),
            LaneReopenError::StartFailed("could not start: craze: outcome unknown: x".into())
        );
    }

    /// **The older-hub words never infer a version** (PC10): the upgrade
    /// remedy — `kill <pid>` — only for a version that PARSES below 0.2.0 and
    /// a known pid; a 0.2.0-or-later hub, a dev or test build, an empty
    /// version or no pid get the plain sentence.
    #[test]
    fn the_older_hub_words_name_a_kill_only_for_a_parsed_older_version_and_a_pid() {
        let plain =
            |v: &str| format!("this craze hub (craze {v}) does not offer reopening saved sessions");
        let remedy = "; a hub from before craze 0.2.0 keeps running after an upgrade until it is idle — end it with kill 5150";
        for v in ["0.1.1", "v0.1.0", "0.0.1", "0.1.9-rc1", "0.1.1+abc"] {
            assert_eq!(
                reopen_unsupported(&hub(v, Some(5150))),
                format!("{}{remedy}", plain(v)),
                "{v}"
            );
        }
        for (v, pid) in [
            ("0.2.0", Some(5150)),
            ("0.10.0", Some(5150)),
            ("1.0.0", Some(5150)),
            ("0.2.0-rc1", Some(5150)),
            ("test-75b046893f9a", Some(5150)),
            ("dev", Some(5150)),
            ("0.1", Some(5150)),
            ("0.1.1.1", Some(5150)),
            ("0.1.x", Some(5150)),
            ("0.1.1", None),
            ("0.1.1", Some(0)),
        ] {
            assert_eq!(reopen_unsupported(&hub(v, pid)), plain(v), "{v} {pid:?}");
        }
        assert_eq!(
            reopen_unsupported_by("", Some(5150)),
            "this craze hub (craze version unknown) does not offer reopening saved sessions"
        );
    }

    /// A lost reopen is `Unavailable`, worded [`REOPEN_LOST`] and then what
    /// was lost — never shed's "outcome unknown" (a reopen is idempotent).
    #[test]
    fn a_lost_reopen_is_unavailable_and_says_retry_is_safe() {
        let LaneReopenError::Unavailable(m) = reopen_lost("no answer from craze within 200s")
        else {
            panic!("a lost reopen is Unavailable")
        };
        assert_eq!(
            m,
            "the reopen's answer was lost; the session may already be running — Retry is safe (no answer from craze within 200s)"
        );
    }
}
