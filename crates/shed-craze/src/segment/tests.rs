//! The segmenter's cells — gx's segment cases (`crates/shed-gx/src/fold/tests.rs`
//! at `203250f`: coalescing, a kind change, the lossless 8 KiB split, a
//! multi-byte character at the cap, one chunk many times the cap, the
//! whitespace-only streak) ported onto craze's chunk kinds, plus the flush
//! clock gx kept in its watcher.

use super::*;
use shed_core::lane::feed::{FEED_ROLE_ASSISTANT, FEED_TYPE_REASONING, FEED_TYPE_TEXT};

const A: &str = FEED_ROLE_ASSISTANT;
const T: &str = FEED_TYPE_TEXT;
const R: &str = FEED_TYPE_REASONING;

fn texts(rows: &[RcFeedMessage]) -> Vec<&str> {
    rows.iter()
        .map(|r| r.text.as_deref().unwrap_or_default())
        .collect()
}

#[test]
fn same_kind_chunks_coalesce_into_one_row_stamped_when_it_started() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    s.push(A, T, "Hello, ", Some("2026-01-01T00:00:01Z"), &mut out);
    s.push(A, T, "world", Some("2026-01-01T00:00:02Z"), &mut out);
    s.push(A, T, "!", Some("2026-01-01T00:00:03Z"), &mut out);
    assert!(out.is_empty(), "nothing ended the streak: {out:?}");
    s.flush(&mut out);
    assert_eq!(texts(&out), ["Hello, world!"]);
    assert_eq!((out[0].role.as_str(), out[0].msg_type.as_str()), (A, T));
    assert_eq!(out[0].ts.as_deref(), Some("2026-01-01T00:00:01Z"));
}

#[test]
fn a_different_chunk_kind_ends_the_streak() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    s.push(A, R, "thinking", None, &mut out);
    s.push(A, T, "answering", None, &mut out);
    assert_eq!(out.len(), 1, "the thought closed when the text began");
    assert_eq!(out[0].msg_type, R);
    s.push(A, R, "again", None, &mut out);
    s.flush(&mut out);
    let kinds: Vec<&str> = out.iter().map(|r| r.msg_type.as_str()).collect();
    assert_eq!(kinds, [R, T, R]);
    assert_eq!(texts(&out), ["thinking", "answering", "again"]);
}

/// Segmentation is **lossless**, splits only on character boundaries, and
/// never emits a row over the cap (gx's case, nine 1,000-byte chunks).
#[test]
fn a_streak_segments_losslessly_at_the_eight_kib_cap() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    let block = "x".repeat(1000);
    for _ in 0..9 {
        s.push(A, T, &block, None, &mut out);
    }
    assert_eq!(out.len(), 1, "one full segment was emitted at the cap");
    assert_eq!(texts(&out)[0].len(), MAX_STREAK_BYTES, "exactly the cap");
    assert_eq!(
        s.open_kind(),
        Some((A, T)),
        "the remainder carried into a new streak of the same kind"
    );
    s.flush(&mut out);
    assert_eq!(texts(&out).iter().map(|t| t.len()).sum::<usize>(), 9_000);
}

/// The exact case that used to lose text: a streak one byte short of the cap,
/// then a multi-byte character.
#[test]
fn a_multi_byte_character_at_the_cap_is_never_split_and_never_dropped() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    s.push(A, T, &"x".repeat(MAX_STREAK_BYTES - 1), None, &mut out);
    s.push(A, T, "é", None, &mut out);
    s.flush(&mut out);
    assert_eq!(
        texts(&out).concat(),
        format!("{}é", "x".repeat(MAX_STREAK_BYTES - 1))
    );
    assert!(out
        .iter()
        .all(|r| r.text.as_deref().unwrap().len() <= MAX_STREAK_BYTES));
}

#[test]
fn a_single_chunk_many_times_the_cap_segments_across_rows_losslessly() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    let huge = "é".repeat(MAX_STREAK_BYTES * 2);
    s.push(A, T, &huge, None, &mut out);
    s.flush(&mut out);
    assert!(out.len() >= 4, "{} rows", out.len());
    assert!(out
        .iter()
        .all(|r| r.text.as_deref().unwrap().len() <= MAX_STREAK_BYTES));
    assert_eq!(texts(&out).concat(), huge);
}

/// A whitespace-only streak emits no row — deliberately (see `flush`).
#[test]
fn a_whitespace_only_streak_emits_no_row() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    s.push(A, T, " \n\t ", None, &mut out);
    s.flush(&mut out);
    assert!(out.is_empty(), "{out:?}");
    // And an empty chunk opens nothing at all.
    s.push(A, R, "", None, &mut out);
    assert_eq!(s.open_kind(), None);
}

/// A restored open run continues with the next chunk of its kind.
#[test]
fn a_restored_run_continues() {
    let mut s = Segmenter::new();
    let mut out = Vec::new();
    s.restore(A, T, "main ", Some("2026-01-01T00:00:00Z"), &mut out);
    s.push(A, T, "newest", None, &mut out);
    s.flush(&mut out);
    assert_eq!(texts(&out), ["main newest"]);
    assert_eq!(out[0].ts.as_deref(), Some("2026-01-01T00:00:00Z"));
}

#[test]
fn split_rows_is_lossless_and_bounded() {
    assert!(split_rows("").is_empty());
    assert_eq!(split_rows("short"), ["short"]);
    let long = "é".repeat(MAX_STREAK_BYTES);
    let parts = split_rows(&long);
    assert_eq!(parts.len(), 2);
    assert!(parts.iter().all(|p| p.len() <= MAX_STREAK_BYTES));
    assert_eq!(parts.concat(), long);
}

/// The flush clock restarts only when the streak's length changes, and is due
/// [`FLUSH_AFTER`] after the last growth.
#[tokio::test(start_paused = true)]
async fn the_flush_clock_runs_from_the_last_growth() {
    let mut c = FlushClock::default();
    let t0 = Instant::now();
    c.note(None, t0);
    assert_eq!(c.deadline(FLUSH_AFTER), None, "no streak, no clock");
    c.note(Some(5), t0);
    assert_eq!(c.deadline(FLUSH_AFTER), Some(t0 + FLUSH_AFTER));
    // A frame that leaves the streak alone does not restart it.
    c.note(Some(5), t0 + Duration::from_secs(1));
    assert_eq!(c.deadline(FLUSH_AFTER), Some(t0 + FLUSH_AFTER));
    assert!(!c.due(t0 + Duration::from_millis(1999), FLUSH_AFTER));
    assert!(c.due(t0 + FLUSH_AFTER, FLUSH_AFTER));
    // Growth does.
    let t1 = t0 + Duration::from_millis(1500);
    c.note(Some(9), t1);
    assert_eq!(c.deadline(FLUSH_AFTER), Some(t1 + FLUSH_AFTER));
    // So does a shrink — a segmenting append reopened a shorter streak.
    let t2 = t1 + Duration::from_millis(100);
    c.note(Some(3), t2);
    assert_eq!(c.deadline(FLUSH_AFTER), Some(t2 + FLUSH_AFTER));
}
