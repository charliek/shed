//! SSE fan-out for GET /v1/events — `internal/ext/rc/hub_events.go`. Plan 010
//! H9 lands the PURE half (event payloads + frame encoding) and the subscriber
//! machinery (bounded queues, non-blocking broadcast, close); the HTTP handler
//! (`handleEvents`/`writeSSE`) arrives with the axum shell in H10.
//!
//! Each connected client gets a subscriber with a bounded queue; the reconcile
//! loop broadcasts pre-encoded event frames to every subscriber with a
//! NON-BLOCKING send — a slow client whose queue is full has events DROPPED
//! rather than stalling the broadcaster (the egress-stream precedent). SSE here
//! is best-effort notification: a client that misses events refetches the
//! /v1/sessions snapshot on reconnect (no Last-Event-ID replay).

use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::mpsc::{Receiver, SyncSender, TrySendError};
use std::sync::{Arc, Condvar, Mutex};

use serde::Serialize;
use shed_core::rc::{RcActivity, RcSessionDto, RcState};

/// One connected SSE client (`subscriber`, `hub_events.go:20`). The Go
/// buffered channel maps to a bounded `sync_channel`; the `closed` channel maps
/// to an atomic flag + condvar (H10's handler waits on the condvar the way
/// Go's select waits on the channel).
///
/// Frames are `Arc`-shared, not copied: Go hands every subscriber the same
/// `[]byte` from one `frame()` call, and [`Hub::broadcast`] does the same here.
pub struct Subscriber {
    tx: SyncSender<Arc<[u8]>>,
    /// The receive side, drained by the SSE handler (H10) or a test. Behind a
    /// mutex because `Receiver` is not Sync; only one drainer exists.
    rx: Mutex<Receiver<Arc<[u8]>>>,
    closed: AtomicBool,
    /// Wakes a handler blocked between frames when the hub closes the stream
    /// or a frame arrives (paired with the [`Hub`]-side notify on broadcast).
    ///
    /// CAVEAT: `wake_mu` guards no state — the queue lives in the channel and
    /// `closed` is an atomic — so this pair does NOT close the lost-wakeup
    /// window (a broadcast landing between the handler's empty `try_recv` and
    /// its `wait` is missed). H10's SSE handler must therefore use
    /// `wait_timeout`, which the 25 s heartbeat forces anyway.
    pub(crate) wake: Condvar,
    pub(crate) wake_mu: Mutex<()>,
    /// Frames dropped due to a full queue (debug/metric — `dropped`).
    pub dropped: AtomicI64,
}

impl Subscriber {
    /// Closes the stream (idempotent — Go's `once`-guarded channel close).
    pub fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
        self.notify();
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::SeqCst)
    }

    /// Wakes a handler parked between frames (see the [`wake`] caveat).
    ///
    /// [`wake`]: Subscriber::wake
    fn notify(&self) {
        let _g = self
            .wake_mu
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        self.wake.notify_all();
    }

    /// Non-blocking drain of one queued frame (the H10 handler's and the
    /// tests' read side).
    pub fn try_recv(&self) -> Option<Arc<[u8]>> {
        self.rx
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .try_recv()
            .ok()
    }
}

/// One SSE event: a name (the `event:` field) and its already-serialized JSON
/// data payload (`hubEvent`, `hub_events.go:33`). The payload is serialized at
/// construction — the constructors below take typed structs, so field ORDER on
/// the wire follows the struct declaration exactly as Go's `json.Marshal`
/// does.
#[derive(Debug, Clone)]
pub struct HubEvent {
    pub name: &'static str,
    /// The marshaled `data:` body ("{}" when serialization failed — Go's
    /// `frame()` fallback).
    pub data: String,
}

impl HubEvent {
    fn new<T: Serialize>(name: &'static str, data: &T) -> HubEvent {
        HubEvent {
            name,
            data: serde_json::to_string(data).unwrap_or_else(|_| "{}".to_string()),
        }
    }

    /// Encodes the SSE wire form (`frame`, `hub_events.go:43`):
    ///
    /// ```text
    /// event: <name>\n
    /// data: <json>\n
    /// \n
    /// ```
    pub fn frame(&self) -> Vec<u8> {
        let mut buf = Vec::with_capacity(16 + self.name.len() + self.data.len());
        buf.extend_from_slice(b"event: ");
        buf.extend_from_slice(self.name.as_bytes());
        buf.push(b'\n');
        buf.extend_from_slice(b"data: ");
        buf.extend_from_slice(self.data.as_bytes());
        buf.extend_from_slice(b"\n\n");
        buf
    }
}

/// `activity.changed` payload (`activityChangedData`, `hub_events.go:72`).
/// `shed` is part of the wire contract but unknown to the local hub — left
/// empty and filled by the server-side aggregator. Contract: `activity` is
/// always one of the advertised non-empty enum values — an activity.changed is
/// NEVER emitted for the suppressed dimension (reconcile enforces it).
#[derive(Debug, Serialize)]
struct ActivityChangedData<'a> {
    shed: &'a str,
    slug: &'a str,
    activity: RcActivity,
    activity_at: &'a str,
    state: RcState,
    /// The sanitized preview current at the transition, so badge clients can
    /// refresh their subtitle without a /messages round-trip. Omitted when no
    /// watcher feeds the session (stability-only kinds).
    #[serde(skip_serializing_if = "str::is_empty")]
    last_message: &'a str,
}

/// `session.updated` payload (`sessionUpdatedData`, `hub_events.go:84`);
/// `session` is null on disappear (kill).
#[derive(Debug, Serialize)]
struct SessionUpdatedData<'a> {
    shed: &'a str,
    slug: &'a str,
    session: Option<&'a RcSessionDto>,
}

/// `message.appended` payload (`messageAppendedData`, `hub_events.go:94`):
/// identity + seq only — the body is fetched from /messages so the fan-out
/// payload stays tiny and a dropped notification is harmless.
#[derive(Debug, Serialize)]
struct MessageAppendedData<'a> {
    shed: &'a str,
    slug: &'a str,
    seq: u64,
}

pub fn activity_changed_event(
    slug: &str,
    activity: RcActivity,
    activity_at: &str,
    state: RcState,
    last_message: &str,
) -> HubEvent {
    HubEvent::new(
        "activity.changed",
        &ActivityChangedData {
            shed: "",
            slug,
            activity,
            activity_at,
            state,
            last_message,
        },
    )
}

/// Fires on appear / recreate / lifecycle-state change; carries the base
/// session DTO (activity travels on activity.changed). Serialized here, so a
/// later mutation of the caller's session can't alias the event payload
/// (`sessionUpdatedEvent`, `hub_events.go:109`).
pub fn session_updated_event(s: &RcSessionDto) -> HubEvent {
    HubEvent::new(
        "session.updated",
        &SessionUpdatedData {
            shed: "",
            slug: &s.slug,
            session: Some(s),
        },
    )
}

/// Fires when a tracked session disappears (killed). `session` is null —
/// clients refetch the snapshot (`sessionGoneEvent`, `hub_events.go:116`).
pub fn session_gone_event(slug: &str) -> HubEvent {
    HubEvent::new(
        "session.updated",
        &SessionUpdatedData {
            shed: "",
            slug,
            session: None,
        },
    )
}

/// Fires when a new feed message lands in a session's ring
/// (`messageAppendedEvent`, `hub_events.go:121`).
pub fn message_appended_event(slug: &str, seq: u64) -> HubEvent {
    HubEvent::new(
        "message.appended",
        &MessageAppendedData {
            shed: "",
            slug,
            seq,
        },
    )
}

impl super::hub::Hub {
    /// Registers a new SSE subscriber (`subscribe`, `hub_events.go:126`).
    pub fn subscribe(&self) -> Arc<Subscriber> {
        let (tx, rx) = std::sync::mpsc::sync_channel(self.cfg.sub_buffer);
        let s = Arc::new(Subscriber {
            tx,
            rx: Mutex::new(rx),
            closed: AtomicBool::new(false),
            wake: Condvar::new(),
            wake_mu: Mutex::new(()),
            dropped: AtomicI64::new(0),
        });
        self.lock_subs().push(Arc::clone(&s));
        s
    }

    /// Removes a subscriber on client disconnect (`unsubscribe`,
    /// `hub_events.go:138`).
    pub fn unsubscribe(&self, s: &Arc<Subscriber>) {
        self.lock_subs().retain(|x| !Arc::ptr_eq(x, s));
    }

    /// The current number of SSE clients — drives the reconcile cadence
    /// (`subscriberCount`, `hub_events.go:146`).
    pub fn subscriber_count(&self) -> usize {
        self.lock_subs().len()
    }

    /// Delivers an event to every subscriber (`broadcast`,
    /// `hub_events.go:155`). The per-subscriber send is non-blocking: a full
    /// queue drops the frame (counted) instead of blocking the broadcaster.
    /// Encoding happens once, up front, and the `Arc` means every subscriber
    /// gets the SAME bytes — Go shares one `[]byte` across the fan-out.
    pub fn broadcast(&self, e: &HubEvent) {
        let frame: Arc<[u8]> = Arc::from(e.frame());
        let subs = self.lock_subs();
        for s in subs.iter() {
            match s.tx.try_send(Arc::clone(&frame)) {
                Ok(()) => s.notify(),
                Err(TrySendError::Full(_) | TrySendError::Disconnected(_)) => {
                    s.dropped.fetch_add(1, Ordering::Relaxed);
                }
            }
        }
    }

    /// Ends every SSE stream — shutdown (`closeAllSubscribers`,
    /// `hub_events.go:170`). Idempotent per subscriber.
    pub fn close_all_subscribers(&self) {
        let subs = self.lock_subs();
        for s in subs.iter() {
            s.close();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::super::hub::{Hub, HubConfig};
    use super::super::hub_test_support::{hub_config, HubClock, HubTmux};
    use super::*;

    // The SSE wire form and the payload field ORDER (Go marshals its structs
    // in declaration order; serde does the same — pinned so the frames stay
    // byte-shaped for the H12 harness).
    #[test]
    fn frame_shapes() {
        let e = activity_changed_event(
            "s1",
            RcActivity::Working,
            "2026-01-01T00:00:00Z",
            RcState::Ready,
            "preview",
        );
        assert_eq!(
            String::from_utf8_lossy(&e.frame()),
            "event: activity.changed\ndata: {\"shed\":\"\",\"slug\":\"s1\",\"activity\":\"working\",\"activity_at\":\"2026-01-01T00:00:00Z\",\"state\":\"ready\",\"last_message\":\"preview\"}\n\n"
        );
        // last_message rides omitempty.
        let quiet = activity_changed_event("s1", RcActivity::Idle, "t", RcState::Ready, "");
        assert!(!quiet.data.contains("last_message"));

        // A disappear carries session:null.
        let gone = session_gone_event("s2");
        assert_eq!(gone.name, "session.updated");
        assert_eq!(gone.data, r#"{"shed":"","slug":"s2","session":null}"#);

        let msg = message_appended_event("s3", 7);
        assert_eq!(msg.data, r#"{"shed":"","slug":"s3","seq":7}"#);
    }

    // Mirrors TestHubBroadcastDropsOnOverflow (`hub_test.go:676`): broadcast
    // never blocks on a full subscriber queue; the excess is dropped and
    // counted, and the queue holds exactly the buffer's worth.
    #[test]
    fn broadcast_drops_on_overflow() {
        let f = HubTmux::new();
        let clk = HubClock::new();
        let h = Hub::new(HubConfig {
            subscriber_buffer: 4,
            ..hub_config(&f, &clk)
        });
        let sub = h.subscribe();

        // Broadcast far more than the buffer without the subscriber reading —
        // must never block (try_send), and the excess must be dropped.
        for _ in 0..100 {
            h.broadcast(&activity_changed_event(
                "s",
                RcActivity::Working,
                "t",
                RcState::Ready,
                "preview",
            ));
        }
        assert!(
            sub.dropped.load(std::sync::atomic::Ordering::Relaxed) > 0,
            "expected dropped frames on overflow"
        );
        let mut drained = 0;
        while sub.try_recv().is_some() {
            drained += 1;
        }
        assert_eq!(drained, 4, "queue depth = buffer, rest dropped");

        // Unsubscribe empties the fan-out set.
        h.unsubscribe(&sub);
        assert_eq!(h.subscriber_count(), 0);
    }
}
