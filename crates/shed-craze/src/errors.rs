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
//! A code this build does not know (or a refusal with no `data.code` at all)
//! is [`LaneError::Failed`] with the message — the contract's rule that an
//! unknown code is `Failed`'s job.
//!
//! **craze's words never wear shed's "outcome unknown"**: a client keeps a
//! create's request id, and the desktop shows a change "not confirmed", only
//! on [`is_outcome_unknown`] — which reads a `Failed`'s text prefix, because
//! the contract's `LaneError` has no variant for it. So every text craze (or
//! the far side of a dial) wrote that becomes a message here goes through
//! [`craze_says`], which escapes one that happens to begin with that prefix:
//! only shed's own [`outcome_unknown`] carries it.

use shed_core::lane::LaneError;

use crate::wire::{code, reason, RpcError};

/// craze's own text (a refusal's message, a start failure's cause, a refused
/// `hello`'s or `session.connect`'s words) as a [`LaneError`]'s message:
/// verbatim — unless it begins with shed's [`OUTCOME_UNKNOWN`], which marks a
/// request whose answer was LOST and is never a definite answer's, so it is
/// said as craze's (`craze: outcome unknown: …`) and [`is_outcome_unknown`]
/// cannot mistake it for one.
pub fn craze_says(text: impl Into<String>) -> String {
    let text = text.into();
    if text.starts_with(OUTCOME_UNKNOWN) {
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
    cause
        .map(str::trim)
        .filter(|c| !c.is_empty())
        .unwrap_or(START_FAILED_FALLBACK)
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
    match e {
        LaneError::Failed(m) => m.strip_prefix(OUTCOME_UNKNOWN),
        _ => None,
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
    }
}
