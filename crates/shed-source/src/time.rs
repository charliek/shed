//! The RFC3339 time helpers the source's modules share (std-only; no chrono).
//!
//! Moved here from the retired broker's `status` module (plan 039 §3.7).

use std::time::{SystemTime, UNIX_EPOCH};

/// `now_rfc3339` formats the current UTC time as RFC3339 with second precision
/// (`2006-01-02T15:04:05Z`), matching the Go daemon's
/// `time.Now().UTC().Format(time.RFC3339)`. No chrono dependency — a std-only
/// civil-date conversion.
pub fn now_rfc3339() -> String {
    rfc3339_utc(now_unix())
}

/// Current wall-clock time as whole Unix seconds (UTC). `pub` so the desktop server
/// can compute `now + approval_timeout` for `expires_at`.
pub fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Convert a Unix timestamp (seconds) to an RFC3339 UTC string using Howard
/// Hinnant's days-from-civil inverse (epoch 1970-01-01). `pub` so the desktop server
/// can stamp `approval_request.expires_at` (= now + timeout) with the same formatter.
pub fn rfc3339_utc(unix_secs: i64) -> String {
    let days = unix_secs.div_euclid(86_400);
    let sod = unix_secs.rem_euclid(86_400);
    let (hour, minute, second) = (sod / 3600, (sod % 3600) / 60, sod % 60);

    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let year_civil = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let day = doy - (153 * mp + 2) / 5 + 1; // [1, 31]
    let month = if mp < 10 { mp + 3 } else { mp - 9 }; // [1, 12]
    let year = if month <= 2 {
        year_civil + 1
    } else {
        year_civil
    };

    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}Z")
}

/// Days from the civil date to the unix epoch (Howard Hinnant's `days_from_civil`;
/// the inverse of the civil-from-days math in [`rfc3339_utc`] above). Used by
/// this crate's `bootstrap`.
pub fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400; // [0, 399]
    let mp = if m > 2 { m - 3 } else { m + 9 } as i64; // [0, 11]
    let doy = (153 * mp + 2) / 5 + d as i64 - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe - 719_468
}

/// Parse an RFC3339 timestamp to unix seconds, matching what Go's `time.Time` collapses
/// to: `Ok(None)` for the zero time (`0001-01-01T00:00:00Z`), `Ok(Some(secs))` for a
/// valid instant (offset applied, sub-second **truncated** as Go's `Format(time.RFC3339)`
/// drops it), `Err(())` for a malformed non-empty value. The inverse of
/// [`rfc3339_utc`]; a round-trip against it is unit-pinned.
///
/// Used by the mint-bundle deserializer (`bootstrap::de_expires_at`). Go's `d.Time.UTC().Format(RFC3339)` is reproduced exactly incl. the
/// zero-time and offset-normalize cases.
#[allow(clippy::result_unit_err)]
pub fn parse_rfc3339_to_unix(s: &str) -> Result<Option<i64>, ()> {
    // Layout: YYYY-MM-DDTHH:MM:SS[.frac](Z|±HH:MM). Split date/time on 'T'.
    let (date, time_and_zone) = s.split_once('T').ok_or(())?;
    let (y, mo, d) = {
        let mut it = date.split('-');
        let y: i64 = it.next().ok_or(())?.parse().map_err(|_| ())?;
        let mo: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let d: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (y, mo, d)
    };
    // Split the zone suffix off the time, then split off any fractional seconds.
    let (time_part, offset_secs) = split_zone(time_and_zone)?;
    let (hms, frac) = match time_part.split_once('.') {
        Some((h, f)) => (h, Some(f)),
        None => (time_part, None),
    };
    if let Some(f) = frac {
        // Sub-second digits are validated (Go's RFC3339Nano parse would) then dropped.
        if f.is_empty() || !f.bytes().all(|b| b.is_ascii_digit()) {
            return Err(());
        }
    }
    let (h, mi, se) = {
        let mut it = hms.split(':');
        let h: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let mi: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        let se: u32 = parse_fixed(it.next().ok_or(())?, 2)?;
        if it.next().is_some() {
            return Err(());
        }
        (h, mi, se)
    };
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || se > 60 {
        return Err(());
    }
    // The Go zero time → None (Go's `IsZero()` → omitted `expires_at`).
    if y == 1 && mo == 1 && d == 1 && h == 0 && mi == 0 && se == 0 && offset_secs == 0 {
        return Ok(None);
    }
    let days = days_from_civil(y, mo, d);
    let secs = days * 86_400 + (h as i64) * 3_600 + (mi as i64) * 60 + (se as i64) - offset_secs;
    Ok(Some(secs))
}

/// Split an RFC3339 zone suffix (`Z` or `±HH:MM`) off the end, returning the remaining
/// time text and the offset in seconds to SUBTRACT to reach UTC.
fn split_zone(time_and_zone: &str) -> Result<(&str, i64), ()> {
    if let Some(rest) = time_and_zone.strip_suffix('Z') {
        return Ok((rest, 0));
    }
    // Find the last '+' or '-' that starts the offset (after the time, so index >= 1).
    let bytes = time_and_zone.as_bytes();
    let mut idx = None;
    for (i, &b) in bytes.iter().enumerate() {
        if (b == b'+' || b == b'-') && i > 0 {
            idx = Some(i);
        }
    }
    let i = idx.ok_or(())?;
    let (time_part, off) = time_and_zone.split_at(i);
    // A `+05:00` zone is 5h AHEAD of UTC, so 5h is SUBTRACTED from the local time to
    // reach UTC (positive value to subtract); `-05:00` adds (negative value).
    let sign = if off.as_bytes()[0] == b'+' { 1 } else { -1 };
    let off = &off[1..];
    let (oh, om) = off.split_once(':').ok_or(())?;
    let oh: i64 = parse_fixed::<u32>(oh, 2)? as i64;
    let om: i64 = parse_fixed::<u32>(om, 2)? as i64;
    if oh > 23 || om > 59 {
        return Err(());
    }
    Ok((time_part, sign * (oh * 3_600 + om * 60)))
}

/// Parse an exactly-`width`-digit unsigned field (RFC3339 fields are fixed-width).
fn parse_fixed<T: std::str::FromStr>(s: &str, width: usize) -> Result<T, ()> {
    if s.len() != width || !s.bytes().all(|b| b.is_ascii_digit()) {
        return Err(());
    }
    s.parse().map_err(|_| ())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rfc3339_known_epochs() {
        assert_eq!(rfc3339_utc(0), "1970-01-01T00:00:00Z");
        // The Unix "billennium": 2001-09-09 01:46:40 UTC.
        assert_eq!(rfc3339_utc(1_000_000_000), "2001-09-09T01:46:40Z");
        assert_eq!(rfc3339_utc(1_700_000_000), "2023-11-14T22:13:20Z");
    }

    #[test]
    fn rfc3339_round_trip_against_renderer() {
        // parse_rfc3339_to_unix is the inverse of rfc3339_utc.
        for unix in [0i64, 1_893_456_000, 1_700_000_000, 253_402_300_799] {
            let s = rfc3339_utc(unix);
            assert_eq!(parse_rfc3339_to_unix(&s), Ok(Some(unix)), "round-trip {s}");
        }
    }

    #[test]
    fn rfc3339_offset_and_fraction() {
        // +05:00 offset normalizes to UTC; fractional seconds are truncated.
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T05:00:00+05:00"),
            Ok(Some(1_893_456_000))
        );
        assert_eq!(
            parse_rfc3339_to_unix("2030-01-01T00:00:00.500Z"),
            Ok(Some(1_893_456_000))
        );
        assert_eq!(parse_rfc3339_to_unix("garbage"), Err(()));
        assert_eq!(parse_rfc3339_to_unix("2030-13-01T00:00:00Z"), Err(())); // bad month
                                                                            // The Go zero time collapses to None (omitted expires_at / absent egress ts).
        assert_eq!(parse_rfc3339_to_unix("0001-01-01T00:00:00Z"), Ok(None));
    }
}
