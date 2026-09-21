//! The reconnect schedule shared by every long-lived feed watcher in this
//! crate: 500 ms doubling to a 30 s ceiling.
//!
//! It lives in one place because several watchers document their schedules as
//! *deliberately identical*, so that every row in one unified sessions view
//! goes stale at the same rate — today that is [`crate::roost`]'s observer, and
//! it was the rc-events and machine-hub watchers before plan 022 (S6,
//! `charliek/shed#328`) retired them. A copy of the constants per watcher made
//! that claim true only by convention: each had its own numeric test, and
//! neither could catch one side drifting. Here it is true by construction.
//!
//! Ported from mobile's `liveActivityProvider` retry timer
//! (`providers.dart:238-239, 325-332`), which is the original both watchers
//! descend from.

use std::time::Duration;

/// First reconnect delay.
pub(crate) const INITIAL: Duration = Duration::from_millis(500);

/// Reconnect-delay ceiling, so a feed that stays down cannot retry-storm.
pub(crate) const MAX: Duration = Duration::from_secs(30);

/// Wait the CURRENT delay, then double it up to [`MAX`]: returns
/// `(wait_now, next_delay)`.
///
/// Resetting to [`INITIAL`] is the caller's business, because the two watchers
/// decide "this connection worked" differently — the shed watcher on the first
/// data of a connection, the machine watcher once the hub has answered and the
/// snapshot is out. Both reset on the connection having WORKED, never on how it
/// later ended.
pub(crate) fn step(prev: Duration) -> (Duration, Duration) {
    (prev, (prev * 2).min(MAX))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn doubles_from_the_initial_delay_to_the_ceiling_and_holds() {
        let mut d = INITIAL;
        let mut waits = Vec::new();
        for _ in 0..9 {
            let (wait, next) = step(d);
            waits.push(wait.as_millis());
            d = next;
        }
        assert_eq!(
            waits,
            vec![500, 1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]
        );
    }
}
