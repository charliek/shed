//! SSE fan-out for GET /v1/events — `internal/ext/rc/hub_events.go`: the event
//! payloads + frame encoding, the subscriber machinery (bounded queues,
//! non-blocking broadcast, close), and the streaming HTTP handler
//! (`handleEvents`/`writeSSE`) on the axum shell (H10).
//!
//! Each connected client gets a subscriber with a bounded queue; the reconcile
//! loop broadcasts pre-encoded event frames to every subscriber with a
//! NON-BLOCKING send — a slow client whose queue is full has events DROPPED
//! rather than stalling the broadcaster (the egress-stream precedent). SSE here
//! is best-effort notification: a client that misses events refetches the
//! /v1/sessions snapshot on reconnect (no Last-Event-ID replay).

use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};

use axum::extract::State;
use axum::response::Response;
use bytes::Bytes;
use serde::Serialize;
use shed_core::rc::{RcActivity, RcSessionDto, RcState};
use tokio::sync::mpsc::error::TrySendError;
use tokio::sync::mpsc::{Receiver, Sender};
use tokio::sync::Notify;

use super::hub::Hub;

/// One connected SSE client (`subscriber`, `hub_events.go:20`). The Go
/// buffered channel maps to a bounded `tokio::sync::mpsc` channel — its
/// `try_send` is callable from the SYNC broadcast path (the reconcile thread)
/// while the SSE handler `recv().await`s, which is exactly Go's
/// buffered-chan-select shape (that arm, and only that arm, has no lost-wakeup
/// window). The `closed` channel maps to an atomic flag + [`Notify`], whose
/// `notify_one` STORES a permit so a close landing between the pump's
/// `is_closed` check and its next park is still delivered.
///
/// Frames are `Arc`-shared, not copied: Go hands every subscriber the same
/// `[]byte` from one `frame()` call, and [`Hub::broadcast`] does the same here.
pub struct Subscriber {
    tx: Sender<Arc<[u8]>>,
    /// The receive side; the SSE handler takes it once
    /// ([`Subscriber::take_rx`]), tests drain in place via
    /// [`Subscriber::try_recv`].
    rx: Mutex<Option<Receiver<Arc<[u8]>>>>,
    closed: AtomicBool,
    /// Wakes the handler parked in its select when the hub closes the stream
    /// (Go's `<-sub.closed` arm).
    closed_wake: Notify,
    /// Frames dropped due to a full queue (debug/metric — `dropped`).
    pub dropped: AtomicI64,
}

impl Subscriber {
    /// Closes the stream (idempotent — Go's `once`-guarded channel close).
    ///
    /// `notify_one`, not `notify_waiters`: the latter wakes only waiters that
    /// are ALREADY parked, so a close landing between the pump's loop-top
    /// `is_closed` check and its next park would be missed and the stream
    /// would linger until the next heartbeat. `notify_one` stores a permit for
    /// the next `notified()`, closing that window. There is only ever one
    /// waiter (the pump).
    pub fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
        self.closed_wake.notify_one();
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::SeqCst)
    }

    /// Parks until [`close`](Subscriber::close) is called (the handler's
    /// `<-sub.closed` select arm). Re-check [`is_closed`](Subscriber::is_closed)
    /// after waking.
    pub(crate) async fn closed_notified(&self) {
        self.closed_wake.notified().await;
    }

    /// Hands the receive side to the SSE handler (once).
    pub(crate) fn take_rx(&self) -> Option<Receiver<Arc<[u8]>>> {
        self.rx
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take()
    }

    /// Non-blocking drain of one queued frame (the tests' read side).
    pub fn try_recv(&self) -> Option<Arc<[u8]>> {
        self.rx
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_mut()?
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
        let (tx, rx) = tokio::sync::mpsc::channel(self.cfg.sub_buffer.max(1));
        let s = Arc::new(Subscriber {
            tx,
            rx: Mutex::new(Some(rx)),
            closed: AtomicBool::new(false),
            closed_wake: Notify::new(),
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
                Ok(()) => {}
                Err(TrySendError::Full(_) | TrySendError::Closed(_)) => {
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

/// Unsubscribes on drop — the pump's `defer h.unsubscribe(sub)`.
struct Unsubscriber {
    hub: Arc<Hub>,
    sub: Arc<Subscriber>,
}

impl Drop for Unsubscriber {
    fn drop(&mut self) {
        self.hub.unsubscribe(&self.sub);
    }
}

/// The streaming response body: frames handed off from the pump task through a
/// CAPACITY-1 channel (see [`handle_events`]).
struct FrameStream(Receiver<Result<Bytes, std::convert::Infallible>>);

impl futures_util::Stream for FrameStream {
    type Item = Result<Bytes, std::convert::Infallible>;
    fn poll_next(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        self.0.poll_recv(cx)
    }
}

/// Serves GET /v1/events as an SSE stream (`handleEvents`,
/// `hub_events.go:182`): registers a subscriber, then writes queued event
/// frames plus a periodic heartbeat comment until the client disconnects or
/// the hub closes the stream. The `: ok` opener and `: heartbeat` comments are
/// LITERAL (wire-pinned).
///
/// Go's per-frame `SetWriteDeadline` — which covers the Flush, so a wedged
/// client can't park the handler as a permanent subscriber — maps to the §2.2
/// design: a CAPACITY-1 channel between this pump and the connection's body
/// stream, with the hub write timeout on each frame handoff. hyper only polls
/// the body while the connection can make write progress, so a wedged TCP peer
/// stops draining the channel, the handoff misses the deadline, and the pump
/// drops the subscriber — preserving Go's observable semantics (slow → frames
/// dropped by broadcast; wedged → stream ended, subscriber removed).
///
/// Go's `case <-ctx.Done(): return` (the client went away) maps to the
/// `body_tx.closed()` arm: hyper drops the response body when the connection
/// dies, which drops the receiving half of the handoff channel. Without that
/// arm a vanished client would stay a registered subscriber until the next
/// frame or heartbeat — up to 25 s of holding the reconcile loop at the ACTIVE
/// cadence.
///
/// NOT PORTED (recorded): Go's opening `rc.Flush()` guard, which answers 500
/// `no_flush` "streaming unsupported" when the `ResponseWriter` cannot flush.
/// That branch is unreachable on Go's own `net/http` server and has no analog
/// here — an axum streaming body IS the flush contract, so there is nothing to
/// interrogate before committing the head.
pub(crate) async fn handle_events(State(hub): State<Arc<Hub>>) -> Response {
    let sub = hub.subscribe();
    let mut rx = sub.take_rx().expect("fresh subscriber holds its receiver");
    let heartbeat = hub.cfg.heartbeat;
    let write_timeout = hub.cfg.write_timeout;

    let (body_tx, body_rx) =
        tokio::sync::mpsc::channel::<Result<Bytes, std::convert::Infallible>>(1);
    let pump_hub = Arc::clone(&hub);
    let pump_sub = Arc::clone(&sub);
    tokio::spawn(async move {
        let _guard = Unsubscriber {
            hub: pump_hub,
            sub: Arc::clone(&pump_sub),
        };
        // A frame handoff under the write deadline (`writeSSE`,
        // `hub_events.go:236`): false ends the stream — the client is gone
        // (send error) or wedged (deadline hit).
        async fn hand_off(
            tx: &Sender<Result<Bytes, std::convert::Infallible>>,
            deadline: std::time::Duration,
            frame: Bytes,
        ) -> bool {
            matches!(
                tokio::time::timeout(deadline, tx.send(Ok(frame))).await,
                Ok(Ok(()))
            )
        }

        // Open the stream with a comment so proxies see bytes immediately.
        if !hand_off(&body_tx, write_timeout, Bytes::from_static(b": ok\n\n")).await {
            return;
        }
        // Heartbeat comments keep the connection warm through idle periods
        // (Go's Ticker — the first tick lands one interval in, not at start).
        // `Skip` is the Ticker's own missed-tick semantics: a tick the pump was
        // too busy to take is DROPPED and the cadence keeps its original phase.
        let mut heartbeat_tick =
            tokio::time::interval_at(tokio::time::Instant::now() + heartbeat, heartbeat);
        heartbeat_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            if pump_sub.is_closed() {
                return; // hub idle-exit / shutdown closed the stream
            }
            let frame: Bytes = tokio::select! {
                _ = body_tx.closed() => return, // Go's <-ctx.Done(): client went away
                _ = pump_sub.closed_notified() => continue, // re-check at loop top
                got = rx.recv() => match got {
                    // Zero-copy: the frame the broadcast built is handed on as
                    // the SAME allocation every subscriber shares.
                    Some(f) => Bytes::from_owner(f),
                    None => return, // subscriber dropped from the hub side
                },
                _ = heartbeat_tick.tick() => Bytes::from_static(b": heartbeat\n\n"),
            };
            if !hand_off(&body_tx, write_timeout, frame).await {
                return;
            }
        }
    });

    // Headers before the body, as in Go. hyper owns the `Connection` header on
    // HTTP/1.1 keep-alive connections, so Go's explicit `Connection:
    // keep-alive` is implicit here (the harness compares canonicalized
    // headers, not this hop-by-hop one).
    Response::builder()
        .header("Content-Type", "text/event-stream")
        .header("Cache-Control", "no-cache")
        .body(axum::body::Body::from_stream(FrameStream(body_rx)))
        .expect("static SSE response head")
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
