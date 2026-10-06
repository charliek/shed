//! Append-only SEGMENTS (plan 025 §3.3.6, P13): how a streamed run of text
//! becomes transcript rows when the contract has no in-place row update.
//!
//! **Ported from shed-gx's segmenter** (`crates/shed-gx/src/fold.rs` at
//! `203250f` — `OpenStreak`, `append_segmented`, `flush_open` (its own
//! `floor_char_boundary` is std's `str::floor_char_boundary` here) — and its
//! flush clock in `src/watcher.rs`,
//! `note_streak`/`read_live`), which plan 025 C1 deleted with the gx lane. The
//! behaviour and its tests come across; gx's wire readers (its `promptId`
//! rule, its event-id cursor) do not, because craze's events carry neither.
//!
//! craze's own fold UPSERTS — its open stream entry grows in place
//! (`internal/transcript/transcript.go`'s `appendStream`) — and shed's rows are
//! append-only (`shed_core::lane`, the closing paragraph of its module doc). So
//! a run of chunks accumulates in ONE open [`Streak`] and becomes a row when
//! something ends it:
//!
//! - a chunk of another kind (`assistant/text` ↔ `assistant/reasoning`);
//! - any other row (the fold flushes before every row it emits), a tool
//!   event, `done`, a foreign turn, a replay's end, a compaction's start;
//! - **2 s of no growth** — the flush clock, [`FlushClock`], which the watcher
//!   runs (the fold has no clock);
//! - **8 KiB** ([`MAX_STREAK_BYTES`], the feed's own per-row cap): the chunk
//!   that does not fit is split losslessly at a UTF-8 boundary, the full
//!   segment is emitted, and the rest opens a new streak of the same kind.
//!
//! A continuing stream after a flush starts a new segment row of the same
//! role and type. **A whitespace-only streak is dropped** (an empty row reads
//! as a bug). `turn ended` does not flush — craze's own rule: only the wire's
//! `done` closes a run — and the clock covers it.
//!
//! The cost is gx's, stated there and true here: up to 8 KiB, or up to 2 s of
//! a stream that has paused, is invisible to a client until the flush.

use shed_core::rc::RcFeedMessage;
use tokio::time::{Duration, Instant};

/// How much text one row may accumulate before it is segmented. It IS the
/// feed's own per-field cap, so a row that reaches it is one the ring would
/// have truncated anyway — tied in code rather than asserted in prose.
pub const MAX_STREAK_BYTES: usize = shed_core::lane::feed::MAX_FEED_MESSAGE_BYTES;

/// How long an open streak may sit without growing before it is flushed as a
/// partial row (plan 025 §3.3.6: "after 2 s of growth").
pub const FLUSH_AFTER: Duration = Duration::from_secs(2);

/// The one open run: consecutive chunks of the same role and type,
/// accumulating until something ends them. At most one is open at a time.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Streak {
    pub role: &'static str,
    pub feed_type: &'static str,
    pub text: String,
    /// The FIRST chunk's time — a row is stamped when it started, not when it
    /// was flushed.
    pub first_ts: Option<String>,
}

impl Streak {
    /// A streak of `role`/`feed_type` with no text yet, stamped `ts`.
    fn empty(role: &'static str, feed_type: &'static str, ts: Option<&str>) -> Streak {
        Streak {
            role,
            feed_type,
            text: String::new(),
            first_ts: ts.map(str::to_string),
        }
    }
}

/// The segmenter: the open streak, and the rows it has closed.
#[derive(Debug, Default)]
pub struct Segmenter {
    open: Option<Streak>,
}

impl Segmenter {
    pub fn new() -> Segmenter {
        Segmenter::default()
    }

    /// Append one chunk of `role`/`feed_type`. A chunk of another kind closes
    /// the open streak first (into `out`); a chunk that does not fit is split
    /// losslessly across as many rows as it takes ([`MAX_STREAK_BYTES`] each,
    /// never mid-character). An empty chunk changes nothing — not even the kind
    /// of the open streak.
    pub fn push(
        &mut self,
        role: &'static str,
        feed_type: &'static str,
        text: &str,
        ts: Option<&str>,
        out: &mut Vec<RcFeedMessage>,
    ) {
        if text.is_empty() {
            return;
        }
        let same = self
            .open
            .as_ref()
            .is_some_and(|o| o.role == role && o.feed_type == feed_type);
        if !same {
            self.flush(out);
        }
        let open = self
            .open
            .get_or_insert_with(|| Streak::empty(role, feed_type, ts));
        if open.first_ts.is_none() {
            open.first_ts = ts.map(str::to_string);
        }
        self.append_segmented(role, feed_type, text, ts, out);
    }

    /// Append `text` to the open streak, segmenting it across as many rows as
    /// it takes — **losslessly, and never mid-character** (gx's
    /// `append_segmented`). No row leaves here longer than
    /// [`MAX_STREAK_BYTES`], no byte is dropped, no codepoint is split.
    fn append_segmented(
        &mut self,
        role: &'static str,
        feed_type: &'static str,
        text: &str,
        ts: Option<&str>,
        out: &mut Vec<RcFeedMessage>,
    ) {
        let mut remaining = text;
        loop {
            let used = self.open.as_ref().map_or(0, |o| o.text.len());
            let room = MAX_STREAK_BYTES.saturating_sub(used);
            if remaining.len() <= room {
                if let Some(open) = self.open.as_mut() {
                    open.text.push_str(remaining);
                }
                return;
            }
            let take = remaining.floor_char_boundary(room);
            if take > 0 {
                if let Some(open) = self.open.as_mut() {
                    open.text.push_str(&remaining[..take]);
                }
                remaining = &remaining[take..];
            }
            // `take == 0`: not one character fits in what is left of this
            // streak; the flush gives the next pass a full-width one, where a
            // character always fits (the cap is kibibytes, a codepoint at most
            // four bytes), so this cannot spin.
            self.flush(out);
            self.open = Some(Streak::empty(role, feed_type, ts));
        }
    }

    /// Emit the open streak as a row, if it holds anything but whitespace —
    /// **a whitespace-only streak is deliberately dropped** (gx's rule: an
    /// empty row in a transcript reads as a bug to whoever is looking at it).
    pub fn flush(&mut self, out: &mut Vec<RcFeedMessage>) {
        let Some(open) = self.open.take() else {
            return;
        };
        if open.text.trim().is_empty() {
            return;
        }
        out.push(RcFeedMessage {
            ts: open.first_ts,
            role: open.role.to_string(),
            msg_type: open.feed_type.to_string(),
            text: Some(open.text),
            ..RcFeedMessage::default()
        });
    }

    /// Restore an open run a snapshot carried (craze's `restoreRun`): `text`
    /// becomes the open streak, so the next chunk of the same kind continues
    /// it. A run longer than one row is segmented on the way in.
    pub fn restore(
        &mut self,
        role: &'static str,
        feed_type: &'static str,
        text: &str,
        ts: Option<&str>,
        out: &mut Vec<RcFeedMessage>,
    ) {
        self.flush(out);
        self.open = Some(Streak::empty(role, feed_type, ts));
        self.append_segmented(role, feed_type, text, ts, out);
    }

    /// The open streak's role and type, when one is open.
    pub fn open_kind(&self) -> Option<(&'static str, &'static str)> {
        self.open.as_ref().map(|o| (o.role, o.feed_type))
    }

    /// How much text the open streak holds — **the flush clock's signal**
    /// ([`FlushClock::note`]): a length rather than a time, because the
    /// segmenter has no clock, and its growth is the one thing that tracks
    /// what a reader is waiting for (gx's `open_streak_bytes`).
    pub fn open_bytes(&self) -> Option<usize> {
        self.open.as_ref().map(|o| o.text.len())
    }
}

/// The flush clock (gx's `streak_at`/`streak_bytes`): when the open streak
/// last GREW. It restarts only when the streak's length changes — a frame that
/// adds nothing to the streak (a sub-agent's chunk, a `queue` event, a
/// presence count) cannot postpone a flush it contributes nothing to — and it
/// is due [`FLUSH_AFTER`] after that.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct FlushClock {
    at: Option<Instant>,
    bytes: usize,
}

impl FlushClock {
    /// Restart the clock iff the open streak changed. `open_bytes` is
    /// [`Segmenter::open_bytes`]; `!=` and not `>`, because a segmenting
    /// append emits a row and reopens a SHORTER streak.
    pub fn note(&mut self, open_bytes: Option<usize>, now: Instant) {
        match open_bytes {
            None => *self = FlushClock::default(),
            Some(bytes) if self.at.is_none() || bytes != self.bytes => {
                self.bytes = bytes;
                self.at = Some(now);
            }
            Some(_) => {}
        }
    }

    /// When the open streak is due a flush; `None` with no streak open.
    pub fn deadline(&self, after: Duration) -> Option<Instant> {
        self.at.map(|at| at + after)
    }

    /// Whether the open streak is due a flush at `now`.
    pub fn due(&self, now: Instant, after: Duration) -> bool {
        self.deadline(after).is_some_and(|d| now >= d)
    }
}

/// Split `text` into rows of at most [`MAX_STREAK_BYTES`], losslessly and never
/// mid-character — a closed entry a snapshot carried (`assistant` text split at
/// 8 KiB, §3.3.6's snapshot rule). Empty text is no rows.
pub fn split_rows(text: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut rest = text;
    while !rest.is_empty() {
        let take = rest.floor_char_boundary(MAX_STREAK_BYTES);
        // A first character wider than the cap is impossible (the cap is
        // kibibytes); taking at least one char keeps this total regardless.
        let take = if take == 0 {
            rest.chars().next().map_or(rest.len(), char::len_utf8)
        } else {
            take
        };
        out.push(&rest[..take]);
        rest = &rest[take..];
    }
    out
}

#[cfg(test)]
mod tests;
