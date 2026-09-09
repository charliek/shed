//! The bounded feed ring — **the owner of `seq`**.
//!
//! Ported from the rc hub's `MessageRing` (`shed-broker/src/rc_hub/messages.rs`,
//! itself `hub_messages.go`'s `messageRing`): item + byte caps with drop-oldest,
//! field sanitization and text trimming on the way in, and a monotonic `seq`
//! assigned at [`MessageRing::append`]. It lived in `shed-opencode` until the
//! second adapter needed the same ring; `shed-opencode` re-exports it, so its
//! goldens and call sites are unchanged.
//!
//! A fold mints every row with `seq: 0` deliberately. `seq` is a TRANSPORT
//! property, not a fold property — the contract requires it to be monotonic
//! **across `Reset`s within one subscription** ([`crate::lane`]'s module doc),
//! and a fold that is wiped at the head of every generation cannot carry it. So
//! a watcher holds one ring for the life of a subscription, the fold is reset
//! per generation, and the seq keeps counting: a client that sees a seq lower
//! than one it holds knows it is looking at a different ring and refetches
//! ([`crate::rc::RcFeedMessage`]'s rule).
//!
//! # Three differences from the hub's ring, all deliberate
//!
//! 1. **No lock.** The hub's ring is shared between a reconcile thread and the
//!    HTTP handlers, so it wraps a `Mutex`. Here a ring is owned outright by one
//!    task (a watcher's pump) or is a local of one `history()` call, so it takes
//!    `&mut self` and the borrow checker enforces what the mutex was for.
//! 2. **[`MessageRing::page`] returns the TAIL**, where the hub's `since(0, n)`
//!    returned the head. The hub was a cursor-walking poller; an adapter that
//!    refolds from the top wants the END of the conversation back.
//!    `truncated` then means what [`crate::lane::LaneHistory`] says it means:
//!    you are not holding a complete history, refetch rather than splice.
//! 3. **No `chrono`.** [`MessageRing::append`] takes `now_unix_ms: i64` and
//!    formats a missing `ts` through [`crate::roost::rfc3339_z`]. `shed-core` is
//!    the dependency-clean crate — it is what the Swift staticlib and the
//!    Android build link — and pulling a date-time crate down here for one
//!    `format!` is exactly the invariant plan 017 §3.1 #10 forbids. Callers that
//!    already hold a `DateTime<Utc>` pass `…timestamp_millis()`.

use std::collections::VecDeque;

use crate::lane::feed::{bound_token, sanitize_feed_text, MAX_APPROVAL_DECISIONS};
use crate::rc::RcFeedMessage;
use crate::roost::rfc3339_z;

/// Caps the ring by row count, drop-oldest past it (`maxRingMessages`).
pub const MAX_RING_MESSAGES: usize = 500;
/// Caps the ring by total accounted bytes, drop-oldest past it
/// (`maxRingBytes`).
pub const MAX_RING_BYTES: usize = 1 << 20;
/// [`MessageRing::page`]'s default when the caller asks for 0
/// (`defaultMessagesLimit`).
pub const DEFAULT_MESSAGES_LIMIT: usize = 100;
/// [`MessageRing::page`]'s hard ceiling (`maxMessagesLimit`).
pub const MAX_MESSAGES_LIMIT: usize = 200;

/// One session's bounded, drop-oldest feed. `seq` is assigned on append,
/// monotonic from 1.
#[derive(Debug, Default)]
pub struct MessageRing {
    msgs: VecDeque<RcFeedMessage>,
    /// Last assigned seq. Continues across the fold's resets — that is the
    /// whole reason `seq` lives here.
    seq: u64,
    /// Running sum of the retained rows' [`row_size`].
    bytes: usize,
    /// Whether drop-oldest has ever evicted a row. Read by [`Self::page`],
    /// which cannot infer it from `seq` alone on a ring whose seq deliberately
    /// outlives its contents.
    dropped: bool,
}

impl MessageRing {
    pub fn new() -> MessageRing {
        MessageRing::default()
    }

    /// Sanitizes `m`, assigns the next seq, stores it, and drops the oldest
    /// rows until both caps hold. `now_unix_ms` stamps `ts` when the row carries
    /// none (a fold-minted row always does; a synthesized status row may not) —
    /// as RFC 3339 UTC to the second, the shape every `ts` on this wire has.
    ///
    /// Returns the stored row — sanitized, stamped and seq'd — because that,
    /// not the caller's original, is what goes on the wire.
    pub fn append(&mut self, mut m: RcFeedMessage, now_unix_ms: i64) -> RcFeedMessage {
        m.text = m.text.as_deref().map(sanitize_feed_text);
        if let Some(t) = &mut m.tool {
            t.name = t.name.as_deref().map(sanitize_feed_text);
            t.detail = t.detail.as_deref().map(sanitize_feed_text);
        }
        if let Some(a) = &mut m.approval {
            a.id = bound_token(&a.id);
            a.status = bound_token(&a.status);
            a.decision = a.decision.as_deref().map(bound_token);
            a.decisions.truncate(MAX_APPROVAL_DECISIONS);
            for d in &mut a.decisions {
                *d = bound_token(d);
            }
        }
        if m.ts.as_deref().unwrap_or_default().is_empty() {
            // Euclidean so a pre-epoch instant floors to the second it is IN
            // rather than the one after — the same rule `rfc3339_z` applies to
            // its own arithmetic.
            m.ts = Some(rfc3339_z(now_unix_ms.div_euclid(1_000)));
        }
        self.seq += 1;
        m.seq = self.seq;

        let stored = m.clone();
        self.bytes += row_size(&m);
        self.msgs.push_back(m);

        // Drop-oldest until within BOTH caps. Never drop the just-appended sole
        // row: a lone row that alone exceeds the byte cap is impossible after
        // the 8 KiB per-field caps, but the guard keeps the ring from going
        // empty regardless.
        while self.msgs.len() > 1
            && (self.msgs.len() > MAX_RING_MESSAGES || self.bytes > MAX_RING_BYTES)
        {
            if let Some(old) = self.msgs.pop_front() {
                self.bytes -= row_size(&old);
                self.dropped = true;
            }
        }
        stored
    }

    /// The most recent `limit` retained rows, oldest-first within the page,
    /// plus whether the caller is therefore missing history.
    ///
    /// **Call-local by construction** on an adapter that builds a FRESH ring per
    /// `history()` call: the seqs a page carries start at 1 and belong to that
    /// call alone — they are NOT the subscription's seqs and must never be
    /// spliced onto a stream's rows. That is exactly what `truncated` tells a
    /// client.
    ///
    /// `limit` is clamped: 0 → [`DEFAULT_MESSAGES_LIMIT`], anything larger than
    /// [`MAX_MESSAGES_LIMIT`] → that ceiling.
    pub fn page(&self, limit: u32) -> (Vec<RcFeedMessage>, bool) {
        let limit = if limit == 0 {
            DEFAULT_MESSAGES_LIMIT
        } else {
            (limit as usize).min(MAX_MESSAGES_LIMIT)
        };
        let skip = self.msgs.len().saturating_sub(limit);
        let page: Vec<RcFeedMessage> = self.msgs.iter().skip(skip).cloned().collect();
        (page, self.dropped || skip > 0)
    }

    /// The last assigned seq (0 on a ring nothing has been appended to).
    pub fn last_seq(&self) -> u64 {
        self.seq
    }
}

/// A row's contribution to the byte budget: every string that rides it — text,
/// the tool block, and the approval block — so the 1 MiB budget stays honest
/// for an approval-heavy feed.
fn row_size(m: &RcFeedMessage) -> usize {
    let mut n = m.text.as_deref().unwrap_or_default().len();
    if let Some(t) = &m.tool {
        n += t.name.as_deref().unwrap_or_default().len()
            + t.detail.as_deref().unwrap_or_default().len();
    }
    if let Some(a) = &m.approval {
        n += a.id.len()
            + a.status.len()
            + a.decision.as_deref().unwrap_or_default().len()
            + a.decisions.iter().map(String::len).sum::<usize>();
    }
    n
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::lane::feed::{FEED_TRUNC_MARKER, MAX_APPROVAL_TOKEN_BYTES, MAX_FEED_MESSAGE_BYTES};
    use crate::rc::{RcFeedApproval, RcFeedTool};

    fn row(text: &str) -> RcFeedMessage {
        RcFeedMessage {
            role: "assistant".into(),
            msg_type: "text".into(),
            text: Some(text.to_string()),
            ..RcFeedMessage::default()
        }
    }

    /// The same fixed instant the pre-move ring's `now()` produced, in ms.
    fn now_ms() -> i64 {
        1_700_000_000_000
    }

    #[test]
    fn seq_is_monotonic_from_one_and_stamps_ts() {
        let mut r = MessageRing::new();
        let a = r.append(row("one"), now_ms());
        let b = r.append(row("two"), now_ms());
        assert_eq!((a.seq, b.seq), (1, 2));
        assert_eq!(a.ts.as_deref(), Some("2023-11-14T22:13:20Z"));
        assert_eq!(r.last_seq(), 2);
    }

    /// The `i64` millisecond stamp is truncated to the second it is IN, both
    /// sides of the epoch — the sub-second remainder never rounds a row into the
    /// next second, and a pre-epoch instant floors rather than truncating toward
    /// zero (which would date it one second late).
    #[test]
    fn a_millisecond_stamp_floors_to_its_own_second() {
        let mut r = MessageRing::new();
        assert_eq!(
            r.append(row("a"), 1_700_000_000_999).ts.as_deref(),
            Some("2023-11-14T22:13:20Z"),
        );
        assert_eq!(
            r.append(row("b"), 0).ts.as_deref(),
            Some("1970-01-01T00:00:00Z")
        );
        assert_eq!(
            r.append(row("c"), -1).ts.as_deref(),
            Some("1969-12-31T23:59:59Z"),
        );
    }

    #[test]
    fn a_supplied_ts_is_kept() {
        let mut r = MessageRing::new();
        let mut m = row("one");
        m.ts = Some("2020-01-01T00:00:00Z".into());
        assert_eq!(
            r.append(m, now_ms()).ts.as_deref(),
            Some("2020-01-01T00:00:00Z")
        );
    }

    #[test]
    fn text_is_sanitized_and_capped() {
        let mut r = MessageRing::new();
        let m = r.append(row("\u{1b}[31mred\u{1b}[0m\nkept"), now_ms());
        assert_eq!(m.text.as_deref(), Some("red\nkept"));

        let long = "x".repeat(MAX_FEED_MESSAGE_BYTES + 100);
        let m = r.append(row(&long), now_ms());
        let got = m.text.expect("text survives");
        assert!(got.ends_with(FEED_TRUNC_MARKER), "truncation is marked");
        assert_eq!(got.len(), MAX_FEED_MESSAGE_BYTES + FEED_TRUNC_MARKER.len());
    }

    #[test]
    fn approval_tokens_are_bounded_and_counted() {
        let mut r = MessageRing::new();
        let mut m = row("");
        m.tool = Some(RcFeedTool {
            name: Some("bash".into()),
            detail: Some("ls -la".into()),
        });
        m.approval = Some(RcFeedApproval {
            id: "a".repeat(MAX_APPROVAL_TOKEN_BYTES + 40),
            status: "pend\ning".into(),
            decision: None,
            decisions: (0..20).map(|i| format!("d{i}")).collect(),
        });
        let stored = r.append(m, now_ms());
        let a = stored.approval.expect("approval survives");
        assert_eq!(a.id.len(), MAX_APPROVAL_TOKEN_BYTES);
        // A token has no legitimate whitespace: it is stripped, not preserved.
        assert_eq!(a.status, "pending");
        assert_eq!(a.decisions.len(), MAX_APPROVAL_DECISIONS);
    }

    #[test]
    fn drop_oldest_holds_the_item_cap_and_seq_keeps_counting() {
        let mut r = MessageRing::new();
        let total = MAX_RING_MESSAGES + 50;
        for i in 0..total {
            r.append(row(&format!("m{i}")), now_ms());
        }
        assert_eq!(r.last_seq(), total as u64);
        let (page, truncated) = r.page(MAX_MESSAGES_LIMIT as u32);
        assert!(truncated, "eviction plus the page cap both truncate");
        assert_eq!(page.len(), MAX_MESSAGES_LIMIT);
        // The TAIL, not the head: the last row is the newest.
        assert_eq!(page.last().expect("a page row").seq, total as u64);
    }

    #[test]
    fn drop_oldest_holds_the_byte_cap() {
        let mut r = MessageRing::new();
        for _ in 0..300 {
            r.append(row(&"y".repeat(8000)), now_ms());
        }
        let retained: usize = r.msgs.iter().map(row_size).sum();
        assert!(retained <= MAX_RING_BYTES, "retained bytes within the cap");
        assert!(r.dropped);
    }

    #[test]
    fn page_clamps_the_limit_and_reports_a_complete_page() {
        let mut r = MessageRing::new();
        for i in 0..5 {
            r.append(row(&format!("m{i}")), now_ms());
        }
        let (page, truncated) = r.page(0);
        assert_eq!(page.len(), 5, "0 means the default, not an empty page");
        assert!(!truncated, "nothing was dropped or clipped");

        let (page, truncated) = r.page(2);
        assert_eq!(page.len(), 2);
        assert!(truncated);
        assert_eq!(page[0].seq, 4);
        assert_eq!(page[1].seq, 5);

        let (page, _) = r.page(10_000);
        assert_eq!(page.len(), 5, "the ceiling does not invent rows");
    }
}
