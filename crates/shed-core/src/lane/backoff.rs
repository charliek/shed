//! The reconnect backoff every lane watcher shares: double-to-a-ceiling, reset
//! on a generation that worked, spread by jitter.
//!
//! Lifted out of `shed-opencode`'s watcher when the second adapter needed the
//! identical curve. It stays a pair of free functions taking their bounds as
//! arguments rather than a struct holding them, because each adapter owns its
//! own floor and ceiling (opencode's `OC_BACKOFF_BASE`/`OC_BACKOFF_MAX`, gx's
//! `GxTimings`) and the only thing genuinely shared is the shape.
//!
//! `shed_app::backoff` is `pub(crate)` and shaped for a different consumer, which
//! is why this is not that one.

use std::time::Duration;

/// The next delay after a connect attempt: `base` when the last generation
/// reached steady state, otherwise the current delay doubled and clamped at
/// `max`.
///
/// `worked` is "did the last generation get as far as being useful", not "did
/// the connect succeed" — a server that accepts a connection and drops it a
/// frame later must not reset the curve, or a flapping agent is reconnected at
/// the floor forever.
pub fn next_backoff(current: Duration, worked: bool, base: Duration, max: Duration) -> Duration {
    if worked {
        return base;
    }
    let doubled = current.saturating_mul(2);
    if doubled > max {
        max
    } else {
        doubled
    }
}

/// A duration in `[d/2, d]`. Spreading matters when several subscriptions lose
/// the same server at once; the precision does not, so the wall clock's
/// sub-second component is entropy enough and no `rand` dependency is taken.
pub fn jittered(d: Duration) -> Duration {
    let half = d / 2;
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| u64::from(d.subsec_nanos()))
        .unwrap_or(0);
    let span = half.as_nanos() as u64 + 1;
    half + Duration::from_nanos(nanos % span)
}

#[cfg(test)]
mod tests {
    use super::*;

    const BASE: Duration = Duration::from_millis(500);
    const MAX: Duration = Duration::from_secs(30);

    #[test]
    fn backoff_doubles_to_the_ceiling_and_resets_on_a_generation_that_worked() {
        let mut d = BASE;
        for _ in 0..12 {
            d = next_backoff(d, false, BASE, MAX);
        }
        assert_eq!(d, MAX, "doubling saturates at the ceiling, never past it");
        assert_eq!(next_backoff(d, true, BASE, MAX), BASE);
        assert_eq!(next_backoff(BASE, false, BASE, MAX), BASE * 2);
    }

    /// `saturating_mul` is what keeps a pathological `current` from wrapping;
    /// the clamp then puts it back on the ceiling.
    #[test]
    fn a_huge_current_saturates_rather_than_overflowing() {
        assert_eq!(next_backoff(Duration::MAX, false, BASE, MAX), MAX);
    }

    #[test]
    fn jitter_stays_within_the_upper_half_of_the_window() {
        for d in [BASE, MAX, Duration::from_millis(1), Duration::ZERO] {
            let j = jittered(d);
            assert!(j >= d / 2, "{j:?} is at least half of {d:?}");
            assert!(j <= d, "{j:?} is at most {d:?}");
        }
    }
}
