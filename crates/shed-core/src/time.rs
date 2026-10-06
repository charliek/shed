//! Reading a timestamp off the wire — the PARSE direction, pure and
//! dependency-free.
//!
//! shed-core's public time helper before plan 025 only FORMATTED
//! ([`crate::roost::rfc3339_z`] writes the RFC 3339 a feed row's `ts` carries);
//! its one parser is `token.rs`'s private `parse_rfc3339_to_unix`, which reads a
//! token bundle's expiry to whole SECONDS under a deliberately fail-closed
//! grammar (an upper-case `T` and `Z` only) — the wrong shape for a row's
//! millisecond times, so this is a separate reader, not a widened one. craze's
//! roster rows state times as RFC 3339 strings (`since`, `startedAt`), and the
//! contract's [`crate::lane::LaneSession`] carries epoch milliseconds, so an
//! adapter needs the other direction too. It lives here rather than in that
//! adapter because both clients' Rust (the desktop, and the phone through FRB)
//! may need it, and here rather than behind a date crate for the reason the lane
//! module's correction 10 gives: `cargo tree -p shed-core-ffi` and the Android
//! tree must list no crate they did not list before. `chrono` stays out.
//!
//! What it accepts is RFC 3339's `date-time` as a Go producer writes it
//! (`time.RFC3339Nano`): `YYYY-MM-DDTHH:MM:SS`, an optional fraction of any
//! length, and a zone that is `Z` or a `±HH:MM` offset. The `T` and the `Z` may
//! be lower-case (RFC 3339 §5.6's note). Anything else — a missing zone, a space
//! for the `T`, a day that month does not have — is `None`: a timestamp this
//! cannot read is reported as absent, never guessed.

/// Parse an RFC 3339 timestamp to Unix epoch **milliseconds**, or `None` if it
/// is not one.
///
/// The fraction is TRUNCATED to milliseconds (floored toward the earlier
/// instant, so `.9999` is `999` ms and a pre-epoch instant still floors), and
/// the offset is applied, so every spelling of one instant answers the same
/// number. A leap second (`:60`) is accepted only where one can occur — an
/// instant that is 23:59:60 in UTC once its offset is applied (RFC 3339 §5.7's
/// rule; `2016-12-31T18:59:60-05:00` is one, `12:00:60Z` is not) — and reads
/// as the first instant of the next minute, since Unix time cannot name it
/// either.
pub fn rfc3339_unix_ms(s: &str) -> Option<i64> {
    let b = s.as_bytes();
    // `YYYY-MM-DDTHH:MM:SS` is 19 bytes, and the shortest zone is one more.
    if b.len() < 20 || !b.is_ascii() {
        return None;
    }
    let year = digits(&b[0..4])?;
    let month = digits(&b[5..7])?;
    let day = digits(&b[8..10])?;
    let hour = digits(&b[11..13])?;
    let minute = digits(&b[14..16])?;
    let second = digits(&b[17..19])?;
    if b[4] != b'-' || b[7] != b'-' || !matches!(b[10], b'T' | b't') {
        return None;
    }
    if b[13] != b':' || b[16] != b':' {
        return None;
    }
    if !(1..=12).contains(&month)
        || day < 1
        || day > days_in_month(year, month)
        || hour > 23
        || minute > 59
        || second > 60
    {
        return None;
    }

    // The fraction: a `.` and at least one digit, any number of them.
    let mut i = 19;
    let mut millis = 0i64;
    if b[i] == b'.' {
        i += 1;
        let start = i;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return None;
        }
        // The first three digits, right-padded: `.5` is 500 ms, `.05` is 50.
        for k in 0..3 {
            let d = b.get(start + k).filter(|c| c.is_ascii_digit());
            millis = millis * 10 + d.map_or(0, |c| i64::from(c - b'0'));
        }
    }

    // The zone: `Z` alone, or `±HH:MM`, and nothing after it.
    let offset_secs = match &b[i..] {
        [b'Z' | b'z'] => 0,
        [sign @ (b'+' | b'-'), h1, h2, b':', m1, m2] => {
            let oh = digits(&[*h1, *h2])?;
            let om = digits(&[*m1, *m2])?;
            if oh > 23 || om > 59 {
                return None;
            }
            let secs = oh * 3_600 + om * 60;
            if *sign == b'-' {
                -secs
            } else {
                secs
            }
        }
        _ => return None,
    };

    // `:60` names a leap second, which exists only at the last second of a UTC
    // day: the instant ONE second earlier must be 23:59:59 in UTC once the
    // offset is taken off. Anywhere else a `60` is not a time at all.
    if second == 60 && (hour * 3_600 + minute * 60 + 59 - offset_secs).rem_euclid(86_400) != 86_399
    {
        return None;
    }

    let days = days_from_civil(year, month, day);
    let local = days * 86_400 + hour * 3_600 + minute * 60 + second;
    // A `+02:00` clock reads two hours AHEAD of UTC, so UTC is the local
    // reading minus the offset.
    Some((local - offset_secs) * 1_000 + millis)
}

/// Fixed-width ASCII decimal, or `None` if any byte is not a digit.
fn digits(b: &[u8]) -> Option<i64> {
    b.iter().try_fold(0i64, |acc, c| {
        c.is_ascii_digit().then(|| acc * 10 + i64::from(c - b'0'))
    })
}

fn is_leap(year: i64) -> bool {
    (year % 4 == 0 && year % 100 != 0) || year % 400 == 0
}

fn days_in_month(year: i64, month: i64) -> i64 {
    match month {
        2 if is_leap(year) => 29,
        2 => 28,
        4 | 6 | 9 | 11 => 30,
        _ => 31,
    }
}

/// Proleptic-Gregorian `(year, month, day)` → days since 1970-01-01.
///
/// Howard Hinnant's `days_from_civil` — the inverse of the `civil_from_days`
/// [`crate::roost::rfc3339_z`] formats with, using the same trick: shift the
/// year to start on 1 March, so the leap day is the last day of the year and the
/// month lengths become one linear formula. Exact for every year a four-digit
/// field can hold.
fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let y = if month <= 2 { year - 1 } else { year };
    let era = y.div_euclid(400);
    let yoe = y.rem_euclid(400); // [0, 399]
    let mp = (month + 9) % 12; // March-based month, [0, 11]
    let doy = (153 * mp + 2) / 5 + day - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146_097 + doe - 719_468
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::roost::rfc3339_z;

    #[test]
    fn the_epoch_and_a_known_instant() {
        assert_eq!(rfc3339_unix_ms("1970-01-01T00:00:00Z"), Some(0));
        // `date -u -d 2026-10-03T12:00:00Z +%s` = 1791028800.
        assert_eq!(
            rfc3339_unix_ms("2026-10-03T12:00:00Z"),
            Some(1_791_028_800_000)
        );
    }

    /// Every fraction length a Go producer writes (it trims trailing zeros, so
    /// any length from 1 to 9 occurs), and one longer than nanoseconds.
    #[test]
    fn fractions_truncate_to_milliseconds() {
        let base = 1_791_028_800_000;
        for (frac, ms) in [
            (".5", 500),
            (".05", 50),
            (".123", 123),
            (".1239", 123),
            (".999999999", 999),
            (".000000001", 0),
            (".1234567891234", 123),
        ] {
            assert_eq!(
                rfc3339_unix_ms(&format!("2026-10-03T12:00:00{frac}Z")),
                Some(base + ms),
                "{frac}"
            );
        }
    }

    /// One instant, three spellings: the offset is applied, in the right
    /// direction.
    #[test]
    fn offsets_name_the_same_instant() {
        let utc = rfc3339_unix_ms("2026-10-03T12:00:00Z");
        assert_eq!(rfc3339_unix_ms("2026-10-03T14:30:00+02:30"), utc);
        assert_eq!(rfc3339_unix_ms("2026-10-03T05:00:00-07:00"), utc);
        assert_eq!(rfc3339_unix_ms("2026-10-03T12:00:00+00:00"), utc);
        assert_eq!(rfc3339_unix_ms("2026-10-03T12:00:00-00:00"), utc);
        // An offset that crosses midnight, with a fraction beside it.
        assert_eq!(
            rfc3339_unix_ms("2026-10-04T01:00:00.250+13:00"),
            utc.map(|t| t + 250)
        );
    }

    /// RFC 3339 §5.6's note: `T` and `Z` may be lower-case.
    #[test]
    fn lower_case_t_and_z_are_accepted() {
        assert_eq!(
            rfc3339_unix_ms("2026-10-03t12:00:00z"),
            rfc3339_unix_ms("2026-10-03T12:00:00Z")
        );
    }

    /// The parse is the exact inverse of the formatter shed-core already has,
    /// across leap years, century rules and pre-epoch instants.
    #[test]
    fn it_inverts_rfc3339_z() {
        let mut t = -2_208_988_800i64; // 1900-01-01, a non-leap century
        while t < 4_102_444_800 {
            // 2100-01-01
            assert_eq!(rfc3339_unix_ms(&rfc3339_z(t)), Some(t * 1_000), "{t}");
            t += 86_400 * 37 + 3_671; // a stride that walks every month and hour
        }
        for day in ["2000-02-29", "2024-02-29", "1904-02-29"] {
            assert!(
                rfc3339_unix_ms(&format!("{day}T00:00:00Z")).is_some(),
                "{day}"
            );
        }
    }

    #[test]
    fn a_pre_epoch_fraction_floors() {
        // Half a second before the epoch: -1 s + 500 ms.
        assert_eq!(rfc3339_unix_ms("1969-12-31T23:59:59.5Z"), Some(-500));
    }

    #[test]
    fn a_leap_second_reads_as_the_next_minute() {
        assert_eq!(
            rfc3339_unix_ms("2016-12-31T23:59:60Z"),
            rfc3339_unix_ms("2017-01-01T00:00:00Z")
        );
        // The same slot, spelled from a zone whose clock reads 18:59:60 then.
        assert_eq!(
            rfc3339_unix_ms("2016-12-31T18:59:60-05:00"),
            rfc3339_unix_ms("2017-01-01T00:00:00Z")
        );
        assert_eq!(
            rfc3339_unix_ms("2017-01-01T05:29:60.5+05:30"),
            rfc3339_unix_ms("2017-01-01T00:00:00.5Z")
        );
    }

    /// `:60` anywhere but the last second of a UTC day is not a time (review,
    /// sol 4): it used to be accepted at ANY minute and quietly normalised into
    /// the next one.
    #[test]
    fn a_sixtieth_second_outside_a_leap_slot_is_none() {
        for bad in [
            "2026-10-03T12:00:60Z",
            "2026-10-03T23:58:60Z",
            "2016-12-31T23:59:60+01:00", // 22:59:60 in UTC
            "2016-12-31T18:59:60-04:00", // 22:59:60 in UTC
            "2016-12-31T00:00:60Z",
        ] {
            assert_eq!(rfc3339_unix_ms(bad), None, "{bad:?}");
        }
    }

    /// Anything that is not an RFC 3339 `date-time` is `None` — never a guess.
    #[test]
    fn malformed_input_is_none() {
        for bad in [
            "",
            "2026-10-03",
            "2026-10-03T12:00:00",       // no zone
            "2026-10-03 12:00:00Z",      // a space for the T
            "2026-10-03T12:00Z",         // no seconds
            "2026-13-03T12:00:00Z",      // month 13
            "2026-00-03T12:00:00Z",      // month 0
            "2026-02-29T12:00:00Z",      // not a leap year
            "1900-02-29T12:00:00Z",      // a century that is not one either
            "2026-04-31T12:00:00Z",      // April has 30
            "2026-10-00T12:00:00Z",      // day 0
            "2026-10-03T24:00:00Z",      // hour 24
            "2026-10-03T12:60:00Z",      // minute 60
            "2026-10-03T12:00:61Z",      // past a leap second
            "2026-10-03T12:00:60Z",      // a leap second outside its slot
            "2026-10-03T12:00:00.Z",     // a dot and no digits
            "2026-10-03T12:00:00+0200",  // an offset without its colon
            "2026-10-03T12:00:00+24:00", // offset hour 24
            "2026-10-03T12:00:00+02:60", // offset minute 60
            "2026-10-03T12:00:00ZZ",     // trailing junk
            "2026-10-03T12:00:00Z ",     // trailing space
            "+2026-10-03T12:00:00Z",     // a signed year
            "2026/10/03T12:00:00Z",      // wrong separators
            "2026-1a-03T12:00:00Z",      // a letter in a number
            "２026-10-03T12:00:00Z",     // a non-ASCII digit
        ] {
            assert_eq!(rfc3339_unix_ms(bad), None, "{bad:?}");
        }
    }
}
