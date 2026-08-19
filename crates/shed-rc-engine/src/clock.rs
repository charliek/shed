//! The engine's injectable clock seam — its own, deliberately NOT shed-app's
//! `traits::Clock` (plan 010 H2 deviation note): that trait serves shed-app's
//! coordinator/host-agent layers and its default formatter lives in shed-app's
//! `timefmt`, above this crate. The engine needs exactly one thing from a
//! clock — a deterministic `created_at` stamp — so it carries a minimal seam
//! with the same shape and a byte-identical formatter (second-precision UTC
//! `Z`, matching Go's `time.Now().UTC().Format(time.RFC3339)` and the parity
//! goldens).

use std::sync::Arc;

use chrono::{DateTime, SecondsFormat, Utc};

/// Injectable "now" (unix seconds), so `created_at` — and everything derived
/// from it, like the `new-session` argv — is deterministic in tests.
pub trait Clock: Send + Sync {
    fn now_unix(&self) -> i64;
    fn now_iso8601(&self) -> String {
        format_iso8601(self.now_unix())
    }
}

/// A shared clock handle.
pub type ClockRef = Arc<dyn Clock>;

/// The real clock — the only place this crate reads the wall clock.
pub struct SystemClock;

impl Clock for SystemClock {
    fn now_unix(&self) -> i64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0)
    }
}

/// A `SystemClock` behind an `Arc`, for production wiring.
pub fn system_clock() -> ClockRef {
    Arc::new(SystemClock)
}

/// Second-precision UTC `Z` — byte-identical to shed-app's
/// `timefmt::format_iso8601` (a unit test there pins the shared shape via the
/// parity goldens' RFC3339 assert; the two implementations are the same three
/// chrono calls).
pub fn format_iso8601(unix: i64) -> String {
    DateTime::<Utc>::from_timestamp(unix, 0)
        .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).expect("epoch is valid"))
        .to_rfc3339_opts(SecondsFormat::Secs, true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_second_precision_utc_z() {
        assert_eq!(format_iso8601(1_770_000_000), "2026-02-02T02:40:00Z");
    }

    #[test]
    fn out_of_range_falls_back_to_epoch() {
        assert_eq!(format_iso8601(i64::MAX), "1970-01-01T00:00:00Z");
    }
}
