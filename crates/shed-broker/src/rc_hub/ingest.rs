//! The cursor hook ingest — `internal/ext/rc/hub_ingest.go`: the PRE-WATCHER
//! QUEUE (the bounds, the event-name grammar, the queue/drain/prune mechanics)
//! and the HTTP handler (`handleIngestCursor`, its 413→400→404→409→202
//! precedence) on the axum shell (H10).
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

/// `cursorHookEventRe.MatchString` — the route's `?event=` gate.
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

/// The queued-body JSON answer for an accepted hook (`{"accepted": true}`).
#[derive(serde::Serialize)]
struct IngestAccepted {
    accepted: bool,
}

/// Serves POST /v1/ingest/cursor (`handleIngestCursor`, `hub_ingest.go:74`) —
/// the hub's one guest-internal route, deliberately NOT on the server proxy's
/// allowlist. Precedence mirrors the verb handlers (body size → request
/// validation → tracked lookup → capability):
///
/// ```text
/// 413 too_large      — the payload exceeds the 256 KiB ingest cap (dropped)
/// 400 invalid_slug / invalid_event — malformed query parameters
/// 404 unknown_slug   — no such rc session
/// 409 not_supported  — tracked but not a cursor session
/// 202                — accepted (pushed to the watcher, or queued for it)
/// ```
///
/// EVERY failure mode is a plain status + envelope: the hook script ignores
/// the response entirely, so these codes exist for operators and tests.
pub(crate) async fn handle_ingest_cursor(
    axum::extract::State(hub): axum::extract::State<std::sync::Arc<super::hub::Hub>>,
    axum::extract::RawQuery(query): axum::extract::RawQuery,
    req: axum::extract::Request,
) -> axum::response::Response {
    use super::hub::{write_error, write_json};
    use super::verbs::ERR_NOT_SUPPORTED;
    use http::StatusCode;

    // Its OWN cap — 256 KiB, not the shared 16 KiB POST cap: an
    // afterShellExecution.output routinely exceeds 16 KiB. NOT the shared
    // wroteTooLarge helper either (that one spells the 16 KiB cap, which
    // would be a lie on this route).
    let payload =
        match super::verbs::read_body_capped(req.into_body(), HUB_INGEST_MAX_BODY_BYTES).await {
            Ok((_, true)) => {
                return write_error(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    "too_large",
                    "hook payload exceeds 256 KiB",
                );
            }
            Ok((buf, false)) => buf,
            Err(()) => {
                return write_error(
                    StatusCode::BAD_REQUEST,
                    "invalid_body",
                    "hook payload could not be read",
                );
            }
        };

    // Query params via Go's url.Values.Get contract (first occurrence wins on
    // duplicates — H10 review MEDIUM: last-wins re-addressed a hook event).
    let slug = super::hub::query_get(query.as_deref(), "slug");
    let slug = slug.as_str();
    if !shed_core::rc_agents::valid_caller_slug(slug) {
        return write_error(
            StatusCode::BAD_REQUEST,
            "invalid_slug",
            "slug is missing or malformed",
        );
    }
    let event = super::hub::query_get(query.as_deref(), "event");
    let event = event.as_str();
    if !valid_cursor_hook_event(event) {
        return write_error(
            StatusCode::BAD_REQUEST,
            "invalid_event",
            "event is missing or malformed",
        );
    }

    // The tracked entry is the authorization: an untracked slug is a 404 (the
    // handleMessages rule — no re-derivation from tmux), and a tracked slug
    // of another kind is a 409 — this payload shape is cursor's, and folding
    // it anywhere else would be a category error.
    let (kind, watcher) = {
        let ts = hub.lock_track();
        match ts.tracked.get(slug) {
            None => {
                drop(ts);
                return write_error(StatusCode::NOT_FOUND, "unknown_slug", "no such rc session");
            }
            Some(tr) => (tr.kind.clone(), tr.watcher.clone()),
        }
    };
    if kind != shed_core::rc::RcKind::Cursor {
        return write_error(
            StatusCode::CONFLICT,
            ERR_NOT_SUPPORTED,
            "this session's kind does not ingest cursor hook events",
        );
    }

    // The watcher pointer was copied out under the track lock; the push takes
    // the WATCHER's mutex, never the hub's. THE QUEUE IS FOR THE PRE-WATCHER
    // WINDOW ONLY: once a watcher exists the event is pushed or DROPPED,
    // never queued (see the module doc — a queued event against a closed
    // watcher would be drained into the NEXT incarnation). Either way the
    // answer is 202: the hook script cannot act on anything else.
    let ev = CursorHookEvent {
        event: event.to_string(),
        payload,
    };
    match watcher {
        None => {
            // The create → first-reconcile-tick window, where the kickoff
            // prompt's beforeSubmitPrompt lands.
            hub.ingest.queue(slug, ev, (hub.cfg.now)(), &hub.cfg.logf);
        }
        Some(w) => match w.as_cursor_ingester() {
            None => {
                // Unreachable today; dropping rather than queueing keeps the
                // pre-watcher-only invariant true if that ever changes.
                (hub.cfg.logf)(&format!(
                    "rc hub: cursor session {slug} has a non-ingesting watcher; dropping {event}"
                ));
            }
            Some(ing) => {
                if !ing.push_hook_event(ev) {
                    (hub.cfg.logf)(&format!(
                        "rc hub: cursor watcher for {slug} refused a {event} event (closed or full); dropping"
                    ));
                }
            }
        },
    }
    write_json(StatusCode::ACCEPTED, &IngestAccepted { accepted: true })
}

#[cfg(test)]
mod tests {
    use super::super::hub_test_support::{hook_ev as ev, CURSOR_SID};
    use super::super::watch::test_support::base_time as t0;
    use super::super::watch::{noop_logf, SessionWatcher};
    use super::super::watch_cursor::CursorWatcher;
    use super::*;

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
    // hub_ingest_test.go's pre-watcher cases; the route precedence is
    // asserted over the real shell in hub_http_tests.rs).
    #[test]
    fn queue_drain_order_and_idempotence() {
        let queues = PreWatcherQueues::new();
        let logf = noop_logf();
        queues.queue("abc123", ev("sessionStart", "{}"), t0(), &logf);
        queues.queue(
            "abc123",
            ev(
                "beforeSubmitPrompt",
                &format!(r#"{{"session_id":"{CURSOR_SID}","prompt":"go"}}"#),
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
