//! The bounded feed ring — **the owner of `seq`** — now
//! [`shed_core::lane::ring`], re-exported here.
//!
//! The ring moved down into the contract crate when the SECOND lane adapter
//! needed the identical one: item + byte caps with drop-oldest, field
//! sanitization on the way in, and a monotonic `seq` that outlives the fold's
//! per-generation reset. Keeping it here would have made `shed-gx` depend on
//! `shed-opencode` for a data structure neither adapter owns.
//!
//! **Two things changed at the move, neither of them behaviour.**
//!
//! 1. [`MessageRing::append`] takes `now_unix_ms: i64` instead of a
//!    `DateTime<Utc>` — `shed-core` is the dependency-clean crate (the Swift
//!    staticlib and the Android build link it) and must not gain `chrono` for
//!    one `format!`. This crate still holds chrono for [`crate::fold`]'s
//!    timestamp conversion, so its call sites simply pass
//!    [`now_utc`]`().timestamp_millis()` and produce the same second-precision
//!    RFC 3339 stamp `to_rfc3339_opts(Secs, true)` did.
//! 2. The tests moved with the type. `fixtures/` is unchanged — asserted by
//!    hash, not by eye.
//!
//! [`now_utc`] stays here: it is chrono-shaped and this crate's, not the
//! contract's.

use chrono::{DateTime, Utc};

pub use shed_core::lane::ring::{
    MessageRing, DEFAULT_MESSAGES_LIMIT, MAX_MESSAGES_LIMIT, MAX_RING_BYTES, MAX_RING_MESSAGES,
};

/// The current UTC instant, WITHOUT chrono's `clock` feature.
///
/// This crate pins chrono `default-features = false, features = ["std"]` (the
/// same spelling as shed-app's and shed-broker's) precisely so it does not pull
/// `iana-time-zone` in, and `clock` is what `Utc::now()` needs. `SystemTime` is
/// the same wall clock without the dependency; a clock before the epoch — the
/// only way the conversion fails — degrades to the epoch rather than panicking,
/// and the only consequence is a `ts` on a row the producer did not stamp.
pub(crate) fn now_utc() -> DateTime<Utc> {
    let since_epoch = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    DateTime::from_timestamp(since_epoch.as_secs() as i64, since_epoch.subsec_nanos())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_core::rc::RcFeedMessage;

    /// The ring resolves at BOTH paths and is the SAME type — a `MessageRing`
    /// built here is accepted where `shed_core::lane::ring::MessageRing` is
    /// asked for, which would not compile if this were a second copy.
    #[test]
    fn the_ring_is_shed_cores_own() {
        fn takes_core_ring(r: &mut shed_core::lane::ring::MessageRing) -> u64 {
            r.append(RcFeedMessage::default(), 1_700_000_000_000).seq
        }
        let mut r: MessageRing = MessageRing::new();
        assert_eq!(takes_core_ring(&mut r), 1);
        assert_eq!(r.last_seq(), 1);
    }

    /// The millisecond stamp this crate now passes produces the same
    /// second-precision RFC 3339 `ts` chrono's `to_rfc3339_opts(Secs, true)`
    /// did — the equivalence the two migrated call sites rest on.
    #[test]
    fn a_millisecond_stamp_matches_chronos_second_precision_rfc3339() {
        use chrono::SecondsFormat;
        let t = DateTime::from_timestamp(1_700_000_000, 123_456_789).expect("a fixed instant");
        let mut r = MessageRing::new();
        let stored = r.append(RcFeedMessage::default(), t.timestamp_millis());
        assert_eq!(
            stored.ts.as_deref(),
            Some(t.to_rfc3339_opts(SecondsFormat::Secs, true).as_str()),
        );
    }
}
