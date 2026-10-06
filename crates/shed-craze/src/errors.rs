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
//! A code this build does not know (or a refusal with no `data.code` at all)
//! is [`LaneError::Failed`] with the message — the contract's rule that an
//! unknown code is `Failed`'s job.

use shed_core::lane::LaneError;

use crate::wire::{code, reason, RpcError};

/// The table, verbatim: one craze `data.code` → one [`LaneError`].
pub fn lane_error(e: &RpcError) -> LaneError {
    let text = e.text();
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

/// `session.create`'s refusal: the table, except P14's one row.
pub fn create_error(e: &RpcError) -> LaneError {
    let start_failed = e.data_code.as_deref() == Some(code::NOT_ACCEPTING)
        && e.reason.as_deref() == Some(reason::START_FAILED);
    if start_failed {
        return LaneError::Failed(start_failed_cause(e.cause.as_deref()).to_string());
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
    matches!(e, LaneError::Failed(m) if m.starts_with(OUTCOME_UNKNOWN))
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
            create_error(&e),
            LaneError::Failed(
                "acp: agent exited: exit status 1: Error: KEYCHAIN LOCKED / Run unlock and retry."
                    .into()
            )
        );
        let no_cause = refusal("not_accepting", "start_failed");
        assert_eq!(
            create_error(&no_cause),
            LaneError::Failed(START_FAILED_FALLBACK.into())
        );
        // Only that reason: any other not_accepting on create follows the table.
        assert_eq!(
            create_error(&refusal("not_accepting", "not_accepting")),
            LaneError::NotAccepting
        );
        assert_eq!(
            create_error(&refusal("unavailable", "spawn_failed")),
            LaneError::Unavailable("craze says unavailable".into())
        );
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
}
