//! The cursor hook ingest — `internal/ext/rc/hub_ingest.go`. Plan 010 H7
//! lands the PRE-WATCHER QUEUE half (the bounds, the event-name grammar, and
//! the queue/drain/prune mechanics reconcile and the route share); the HTTP
//! handler itself (`handleIngestCursor`, its 413→400→404→409→202 precedence)
//! arrives with the HTTP shell in H10.
//!
//! The queue exists for one real window: hook events that arrive before
//! reconcile has built the session's watcher. `shed attach --kind cursor
//! --prompt …` delivers the kickoff prompt within a second of create, well
//! before the first reconcile tick, and that beforeSubmitPrompt is the feed's
//! first row. THE QUEUE IS FOR THE PRE-WATCHER WINDOW ONLY: once a watcher
//! exists an event is either pushed or dropped, never queued (nothing drains
//! a queue after construction, and an event queued against a CLOSED watcher
//! would be drained into the NEXT watcher built at that slug — folding a dead
//! incarnation's turn into a recreated session's feed).

use std::collections::HashMap;
use std::sync::LazyLock;
use std::sync::Mutex;

use chrono::{DateTime, Utc};
use regex::Regex;

use super::watch::{CursorIngester, LogFn};
use super::watch_cursor::CursorHookEvent;

/// Caps one hook payload (413 past it, event dropped — the feed just misses
/// that event; the session is otherwise unaffected). The ingest route's OWN
/// cap, not the 16 KiB one every other POST shares:
/// afterShellExecution.output is the feed's only source of tool output and
/// routinely exceeds 16 KiB (`hubIngestMaxBodyBytes`, `hub_ingest.go:35`).
pub const HUB_INGEST_MAX_BODY_BYTES: usize = 256 << 10;

/// The per-slug pre-watcher inbox bounds (`maxPreWatcherEvents`/
/// `maxPreWatcherBytes`, `hub_ingest.go:42-43`).
pub const MAX_PRE_WATCHER_EVENTS: usize = 32;
pub const MAX_PRE_WATCHER_BYTES: usize = 256 << 10;

/// How long a queue is held for a slug that never grows a watcher
/// (`preWatcherTTL`, `hub_ingest.go:49`) — a session that died at create, a
/// slug that vanished, a kind whose watcher is never built. Checked on
/// reconcile ticks; the whole queue is dropped, not trimmed — half a turn's
/// events are worse than none.
pub const PRE_WATCHER_TTL: std::time::Duration = std::time::Duration::from_secs(60);

/// Bounds the `?event=` token (`cursorHookEventRe`, `hub_ingest.go:56`):
/// cursor's hook names are camelCase identifiers. Validated because the value
/// is stored, compared, and logged — and because an unbounded query parameter
/// is exactly the kind of field that grows into a ring-filling payload.
static CURSOR_HOOK_EVENT_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[A-Za-z][A-Za-z0-9_]{0,63}$").expect("hook event regex"));

/// `cursorHookEventRe.MatchString` for the H10 handler + tests.
pub fn valid_cursor_hook_event(event: &str) -> bool {
    CURSOR_HOOK_EVENT_RE.is_match(event)
}

/// One slug's bounded queue of hook events awaiting its watcher
/// (`preWatcherQueue`, `hub_ingest.go:59`).
struct PreWatcherQueue {
    events: Vec<CursorHookEvent>,
    bytes: usize,
    /// When the queue was opened (the TTL clock).
    first: DateTime<Utc>,
}

/// The hub's pre-watcher queues, keyed by slug. Guarded by its OWN lock —
/// Go's `ingestMu`, never `trackMu` — so an ingest burst cannot contend with
/// reconcile's tracked-state work. The Hub (H9) embeds one.
#[derive(Default)]
pub struct PreWatcherQueues {
    inner: Mutex<HashMap<String, PreWatcherQueue>>,
}

impl PreWatcherQueues {
    pub fn new() -> PreWatcherQueues {
        PreWatcherQueues::default()
    }

    /// Appends an event to the slug's queue (`queuePreWatcher`,
    /// `hub_ingest.go:162`). Overflow of either bound drops the event: the
    /// queue exists to preserve the FIRST events of a session (above all the
    /// kickoff prompt), and the watcher normally exists within one tick, so a
    /// queue that fills means something is wrong and dropping is better than
    /// growing.
    pub fn queue(&self, slug: &str, ev: CursorHookEvent, now: DateTime<Utc>, logf: &LogFn) {
        let mut queues = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let q = queues
            .entry(slug.to_string())
            .or_insert_with(|| PreWatcherQueue {
                events: Vec::new(),
                bytes: 0,
                first: now,
            });
        if q.events.len() >= MAX_PRE_WATCHER_EVENTS
            || q.bytes + ev.payload.len() > MAX_PRE_WATCHER_BYTES
        {
            logf(&format!(
                "rc hub: cursor pre-watcher queue full for {slug}; dropping {} event",
                ev.event
            ));
            return;
        }
        q.bytes += ev.payload.len();
        q.events.push(ev);
    }

    /// Hands a freshly built watcher every event queued for its slug, in
    /// arrival order, and clears the queue (`drainPreWatcher`,
    /// `hub_ingest.go:185`). Reconcile calls it TWICE per construction, and
    /// both calls matter: once at construction (before the watcher's first
    /// refresh, so everything already queued folds on the same tick the
    /// watcher appears — the kickoff prompt is in the feed immediately), and
    /// once right after the watcher is published under the track lock, to
    /// sweep up anything the ingest handler queued during the window in
    /// between. Idempotent: the second call finds no entry and does nothing.
    pub fn drain(&self, slug: &str, ingester: &dyn CursorIngester) {
        // The ingest lock is released BEFORE the pushes (Go spells this as an
        // explicit `ingestMu.Unlock()` ahead of the loop): each push takes the
        // WATCHER's mutex, and holding this one across that would pin a
        // lock-order dependency between two independent locks.
        let q = {
            let mut queues = self
                .inner
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            queues.remove(slug)
        };
        let Some(q) = q else {
            return;
        };
        for ev in q.events {
            ingester.push_hook_event(ev);
        }
    }

    /// Drops queues whose slug still has no watcher after [`PRE_WATCHER_TTL`],
    /// and any queue for a slug that is no longer present at all
    /// (`prunePreWatcher`, `hub_ingest.go:206`). Run on every reconcile
    /// tick — without it, hook events for a session that died at create would
    /// be retained for the hub's whole life. `present` is a PREBUILT snapshot,
    /// exactly like Go's `map[string]bool` parameter: a callback here would
    /// invite the caller to consult hub state under the ingest lock and
    /// invert the lock order (H7 review).
    pub fn prune(&self, now: DateTime<Utc>, present: &std::collections::HashSet<String>) {
        // Compared as a SIGNED chrono duration, mirroring Go's
        // `now.Sub(q.first) >= preWatcherTTL`: a backwards clock step yields a
        // negative age, which must read as "not yet expired" (keep) rather
        // than drop the queue.
        let ttl = chrono::Duration::from_std(PRE_WATCHER_TTL).expect("ttl in range");
        let mut queues = self
            .inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        queues.retain(|slug, q| present.contains(slug) && now.signed_duration_since(q.first) < ttl);
    }

    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .len()
    }

    #[cfg(test)]
    pub(crate) fn queued_events(&self, slug: &str) -> usize {
        self.inner
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(slug)
            .map_or(0, |q| q.events.len())
    }
}

#[cfg(test)]
mod tests {
    use super::super::watch::test_support::base_time as t0;
    use super::super::watch::{noop_logf, SessionWatcher};
    use super::super::watch_cursor::CursorWatcher;
    use super::*;

    fn ev(event: &str, payload: &str) -> CursorHookEvent {
        CursorHookEvent {
            event: event.to_string(),
            payload: payload.as_bytes().to_vec(),
        }
    }

    // The hook-event grammar (`cursorHookEventRe`).
    #[test]
    fn hook_event_grammar() {
        for good in [
            "beforeSubmitPrompt",
            "stop",
            "afterShellExecution",
            "a",
            "A_1",
        ] {
            assert!(valid_cursor_hook_event(good), "{good:?}");
        }
        let long = "a".repeat(65);
        for bad in ["", "1abc", "has space", "has-dash", long.as_str()] {
            assert!(!valid_cursor_hook_event(bad), "{bad:?}");
        }
    }

    // queue → drain in arrival order, exactly once (the mechanics half of
    // hub_ingest_test.go's pre-watcher cases; the route precedence arrives
    // with the H10 handler).
    #[test]
    fn queue_drain_order_and_idempotence() {
        let queues = PreWatcherQueues::new();
        let logf = noop_logf();
        queues.queue("abc123", ev("sessionStart", "{}"), t0(), &logf);
        queues.queue(
            "abc123",
            ev(
                "beforeSubmitPrompt",
                r#"{"session_id":"4113a71f-0a42-4a6d-89b9-483e44b74103","prompt":"go"}"#,
            ),
            t0(),
            &logf,
        );
        assert_eq!(queues.queued_events("abc123"), 2);

        let w = CursorWatcher::new("", None);
        queues.drain("abc123", &w);
        assert_eq!(queues.len(), 0, "the queue clears on drain");
        w.refresh(t0());
        let rows = w.drain_pending();
        assert!(
            rows.iter()
                .any(|m| m.text.contains("cursor session started")),
            "queued events reached the watcher in order: {rows:?}"
        );
        // Second drain: no entry, nothing pushed.
        queues.drain("abc123", &w);
        w.refresh(t0());
        assert!(w.drain_pending().is_empty(), "drain is idempotent");
    }

    // The count + byte bounds drop overflow (the queue preserves the FIRST
    // events).
    #[test]
    fn queue_bounds() {
        let queues = PreWatcherQueues::new();
        let logf = noop_logf();
        for i in 0..MAX_PRE_WATCHER_EVENTS + 5 {
            queues.queue("abc123", ev("stop", &format!("{{\"i\":{i}}}")), t0(), &logf);
        }
        assert_eq!(
            queues.queued_events("abc123"),
            MAX_PRE_WATCHER_EVENTS,
            "count bound drops the newest"
        );

        let queues2 = PreWatcherQueues::new();
        let big = "x".repeat(MAX_PRE_WATCHER_BYTES + 1);
        queues2.queue("abc123", ev("stop", &big), t0(), &logf);
        assert_eq!(queues2.queued_events("abc123"), 0, "byte bound");
    }

    // TTL + presence pruning (`prunePreWatcher`).
    #[test]
    fn prune_ttl_and_presence() {
        let queues = PreWatcherQueues::new();
        let logf = noop_logf();
        queues.queue("keep11", ev("stop", "{}"), t0(), &logf);
        queues.queue("gone22", ev("stop", "{}"), t0(), &logf);

        // A slug no longer present is dropped regardless of age.
        queues.prune(t0(), &std::iter::once("keep11".to_string()).collect());
        assert_eq!(queues.queued_events("keep11"), 1);
        assert_eq!(queues.queued_events("gone22"), 0);

        // Past the TTL the queue is dropped whole even for a present slug.
        let later = t0() + chrono::Duration::from_std(PRE_WATCHER_TTL).unwrap();
        queues.prune(
            later,
            &["keep11".to_string(), "gone22".to_string()]
                .into_iter()
                .collect(),
        );
        assert_eq!(queues.len(), 0, "TTL prune drops the whole queue");
    }
}
