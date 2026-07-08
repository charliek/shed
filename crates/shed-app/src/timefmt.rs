//! Flexible ISO-8601 timestamp parse/format, ported from shed-desktop's
//! `DateFormatting.swift`.
//!
//! shed-server (and the host agent) render timestamps in more than one shape —
//! `2026-05-31T13:33:00.884935839-05:00` (nanosecond fraction + local offset),
//! plain `...Z` UTC, and occasionally a trailing ` (UTC)` annotation — so a
//! single fixed parser would silently fail on one of them. The pure crates carry
//! timestamps as strings; this is the one place they become instants.
//!
//! **Fail-closed contract (the reason this matters for security):** an
//! unparseable timestamp returns `None`. The approval coordinator MUST treat a
//! `None` `expires_at` as *already expired* (never a far-future default) — a
//! naive `parse().unwrap_or(FAR_FUTURE)` would invert F4 into a never-expiring,
//! always-approvable request. The parse itself is deliberately lenient; the
//! caller's *default* is what must fail closed.

use chrono::{DateTime, SecondsFormat, Utc};

/// Parse a shed-server timestamp string into **unix seconds**, trying RFC-3339
/// (which covers both the fractional-seconds+offset and the plain `Z` forms),
/// then a form with a trailing ` (UTC)` annotation stripped. Returns `None` on
/// empty or unparseable input — the fail-closed primitive.
pub fn parse_unix(raw: &str) -> Option<i64> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Ok(dt) = DateTime::parse_from_rfc3339(trimmed) {
        return Some(dt.timestamp());
    }
    if let Some(stripped) = trimmed.strip_suffix(" (UTC)") {
        return parse_unix(stripped);
    }
    None
}

/// Format unix seconds as a plain ISO-8601 UTC string with a `Z` suffix and no
/// fractional seconds — the wire `ts` format (matches Swift's `nowISO8601`).
/// The Clock seam supplies "now"; this only formats it.
pub fn format_iso8601(unix: i64) -> String {
    DateTime::<Utc>::from_timestamp(unix, 0)
        .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).expect("epoch is valid"))
        .to_rfc3339_opts(SecondsFormat::Secs, true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_fractional_seconds_with_offset() {
        // created_at shape: nanosecond fraction + local offset.
        let u = parse_unix("2026-05-31T13:33:00.884935839-05:00").unwrap();
        // 13:33:00 at -05:00 == 18:33:00 UTC.
        assert_eq!(format_iso8601(u), "2026-05-31T18:33:00Z");
    }

    #[test]
    fn parses_plain_utc_z() {
        let u = parse_unix("2026-07-03T00:00:00Z").unwrap();
        assert_eq!(format_iso8601(u), "2026-07-03T00:00:00Z");
    }

    #[test]
    fn parses_trailing_utc_annotation() {
        let u = parse_unix("2026-07-03T00:00:00Z (UTC)").unwrap();
        assert_eq!(format_iso8601(u), "2026-07-03T00:00:00Z");
    }

    #[test]
    fn empty_and_whitespace_are_none() {
        assert_eq!(parse_unix(""), None);
        assert_eq!(parse_unix("   "), None);
    }

    #[test]
    fn garbage_is_none_fail_closed() {
        // The fail-closed primitive: unparseable -> None, so the coordinator
        // treats the request as already expired (never far-future).
        assert_eq!(parse_unix("garbage"), None);
        assert_eq!(parse_unix("2026-07-03"), None); // date only, no time/offset
        assert_eq!(parse_unix("not a timestamp"), None);
    }

    #[test]
    fn format_epoch() {
        assert_eq!(format_iso8601(0), "1970-01-01T00:00:00Z");
    }

    #[test]
    fn round_trips_seconds() {
        let s = "2026-07-03T12:34:56Z";
        let u = parse_unix(s).unwrap();
        assert_eq!(format_iso8601(u), s);
    }
}
