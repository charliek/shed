//! Reconnecting rc-events watcher — the shed-app half of the live-activity
//! layer (plan 001 §3.3; the pure decode + fold half is
//! `shed_core::rc_events`, the single-connection SSE transport is
//! [`Client::rc_events`]).
//!
//! Ported from mobile's `liveActivityProvider` loop
//! (`shed-mobile/lib/providers.dart:274-365`): one watcher per (server,
//! subscription) drives `GET /api/rc/events`, folds each decoded [`RcEvent`]
//! into a held [`ActivityOverlay`], and emits typed [`RcWatcherUpdate`]s on an
//! unbounded channel. On disconnect it emits [`RcWatcherUpdate::Down`] and
//! reconnects under exponential backoff (500ms doubling to a 30s ceiling,
//! `providers.dart:238-239`), resetting to the initial delay on the first
//! data of a connection (`providers.dart:338`).
//!
//! **Resync contract** (mobile's `gotData` semantics,
//! `providers.dart:337-349`): on the first real event of a new connection —
//! not on stream-open — and only when an earlier connection already delivered
//! data (`connected_before`), the watcher clears the held overlay and emits
//! [`RcWatcherUpdate::Resynced`] BEFORE folding/emitting that event, so stale
//! pre-drop patches can never override the consumer's fresh snapshot. The
//! first-ever data-bearing connection skips `Resynced` (the consumer just
//! loaded a fresh overview). The overlay is deliberately NOT cleared on
//! disconnect — only on the first data of the next connection — so the
//! consumer keeps rendering the last snapshot across a blip (no
//! cleared-overlay update is ever emitted during the gap). There is no
//! buffering/ack protocol around the consumer's resync refetch: overlay
//! snapshots keep flowing while it refetches, because consumers render
//! base + overlay (same model as mobile; a patch for a row the refetched base
//! doesn't know yet simply lands when the base catches up). Mobile's
//! unknown-slug debounced overview refetch (`providers.dart:241-247`) stays
//! consumer-side — it requires overview knowledge the watcher doesn't have.
//!
//! **Spawn-once, non-restartable by construction:** [`RcEventsWatcher::spawn`]
//! is the constructor AND the start — there is no `start()` to call twice, so
//! "restart" cannot arise. This deliberately diverges from
//! [`HostAgentClient`]'s restart-safe `start()` (`host_agent.rs`): the FRB
//! bridge constructs one watcher per subscription and drops it on
//! unsubscribe; there is no in-place-reconfigure consumer. [`stop`] (and
//! `Drop`) aborts the loop task; aborting drops the in-flight connection
//! future, which closes the underlying HTTP connection.
//!
//! **Channel choice:** unbounded mpsc, per the repo precedent
//! (`host_agent.rs` event stream). The accepted risk — a stalled consumer
//! buffers updates — is fine here because the FRB/Tauri consumers drain
//! eagerly; the bounded-with-drop alternative was considered and rejected
//! (plan 001, panel round 2). The loop STOPS when a send fails (the receiver
//! was dropped), AND it watches `updates.closed()` while awaiting the
//! connection and the backoff sleep — a healthy heartbeat-only stream
//! delivers no events (comments are swallowed by the SSE parser), so a
//! send-failure alone would never be observed there and an abandoned
//! receiver would otherwise leak the task + connection for as long as the
//! server kept the quiet stream open.
//!
//! [`Client::rc_events`]: shed_core::http::Client::rc_events
//! [`HostAgentClient`]: crate::host_agent::HostAgentClient
//! [`stop`]: RcEventsWatcher::stop

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::mpsc;

use shed_core::http::{Client, RcEventSink, ShedError};
use shed_core::rc_events::{ActivityOverlay, RcEvent};

/// The reconnect schedule (`providers.dart:238-239`) — small first delay so a
/// transient blip resyncs quickly, capped so a server that stays down can't
/// retry-storm. Shared with [`crate::machine`]'s hub watcher so a shed row and
/// a machine row in one sessions view go stale at the same rate; see
/// [`crate::backoff`].
use crate::backoff::{step as backoff_step, INITIAL as INITIAL_BACKOFF};

/// One update from the watcher loop (pinned shape, plan 001 §3.3).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RcWatcherUpdate {
    /// The first event of a data-bearing reconnect is about to arrive and the
    /// held overlay was just cleared: the consumer should refetch its base
    /// overview (mobile invalidates `overviewProvider` here). Never emitted
    /// for the first-ever data-bearing connection, and never for a reconnect
    /// that delivers no data.
    Resynced,
    /// A decoded event plus the overlay snapshot AFTER folding it in.
    Event {
        event: RcEvent,
        overlay: ActivityOverlay,
    },
    /// The connection ended (clean EOF or error); the watcher backs off and
    /// reconnects. Emitted once per disconnect. The held overlay is NOT
    /// cleared — the consumer keeps rendering the last snapshot until the
    /// next connection's first data resyncs it.
    Down { reason: String },
}

/// Handle to one spawned rc-events watcher loop. Constructing it
/// ([`RcEventsWatcher::spawn`]) starts the loop; dropping it (or [`stop`])
/// aborts it. Not restartable — build a new one per subscription.
///
/// [`stop`]: RcEventsWatcher::stop
pub struct RcEventsWatcher {
    server_name: String,
    running: Arc<AtomicBool>,
    task: tokio::task::JoinHandle<Result<(), LoopExit>>,
}

impl RcEventsWatcher {
    /// Spawn the reconnect-and-fold loop for `client` (one shed-server host)
    /// onto `handle`, returning the watcher handle and the update stream.
    /// `server_name` identifies the host to the consumer (see
    /// [`server_name`]); the loop itself keys nothing off it.
    ///
    /// [`server_name`]: RcEventsWatcher::server_name
    pub fn spawn(
        handle: &tokio::runtime::Handle,
        client: Client,
        server_name: String,
    ) -> (RcEventsWatcher, mpsc::UnboundedReceiver<RcWatcherUpdate>) {
        Self::spawn_with(
            handle,
            Arc::new(ClientConnector { client }),
            Arc::new(TokioSleeper),
            server_name,
        )
    }

    /// The seam-injected constructor behind [`spawn`]: tests script the
    /// connection outcomes ([`EventsConnector`]) and capture/gate the backoff
    /// waits ([`Sleeper`]) so every loop test is deterministic.
    ///
    /// [`spawn`]: RcEventsWatcher::spawn
    fn spawn_with(
        handle: &tokio::runtime::Handle,
        connector: Arc<dyn EventsConnector>,
        sleeper: Arc<dyn Sleeper>,
        server_name: String,
    ) -> (RcEventsWatcher, mpsc::UnboundedReceiver<RcWatcherUpdate>) {
        let (updates_tx, updates_rx) = mpsc::unbounded_channel();
        let running = Arc::new(AtomicBool::new(true));
        let task = handle.spawn(run_loop(connector, sleeper, running.clone(), updates_tx));
        (
            RcEventsWatcher {
                server_name,
                running,
                task,
            },
            updates_rx,
        )
    }

    /// The server this watcher was spawned for.
    pub fn server_name(&self) -> &str {
        &self.server_name
    }

    /// Stop the loop: clears `running` and aborts the task (the
    /// `host_agent.rs` `stop()` precedent). Aborting drops whatever the loop
    /// was awaiting — an in-flight SSE connection (closing it) or a backoff
    /// sleep — so termination is prompt in either phase. Idempotent.
    ///
    /// **Soft-stop contract:** `stop()` returns without joining the task, so
    /// a poll already in flight can still deliver a final update — or launch
    /// one more connection attempt, immediately dropped — before the abort
    /// lands (this also covers the backoff→reconnect launch race). The loop
    /// re-checks `running` before every send to shrink that window, but
    /// consumers must tolerate stragglers. In practice the FRB bridge drops
    /// the receiver alongside `stop()`, which both discards stragglers and
    /// is itself a teardown signal the loop watches (`updates.closed()`). A
    /// hard cutoff would need an async stop-and-join — deliberately not
    /// offered (no consumer needs it).
    pub fn stop(&self) {
        self.running.store(false, Ordering::SeqCst);
        self.task.abort();
    }
}

impl Drop for RcEventsWatcher {
    /// Dropping the handle stops the loop — the FRB bridge's unsubscribe path
    /// is simply dropping the watcher.
    fn drop(&mut self) {
        self.stop();
    }
}

/// One SSE connection attempt, running until the stream ends. The prod impl
/// is [`Client::rc_events`]; tests inject a scripted connector so the
/// reconnect/resync/backoff loop is exercised without real sockets or the
/// transport's own timing (which has its own test matrix in shed-core).
///
/// [`Client::rc_events`]: shed_core::http::Client::rc_events
#[async_trait::async_trait]
trait EventsConnector: Send + Sync {
    async fn connect(&self, sink: &dyn RcEventSink) -> Result<(), ShedError>;
}

struct ClientConnector {
    client: Client,
}

#[async_trait::async_trait]
impl EventsConnector for ClientConnector {
    async fn connect(&self, sink: &dyn RcEventSink) -> Result<(), ShedError> {
        self.client.rc_events(sink).await
    }
}

/// Injectable sleep seam for deterministic backoff tests (the pattern
/// `shed-broker/src/egress.rs` uses — replicated, not imported: the broker
/// crate is unreachable from default-features shed-app by design).
#[async_trait::async_trait]
trait Sleeper: Send + Sync {
    async fn sleep(&self, d: Duration);
}

struct TokioSleeper;

#[async_trait::async_trait]
impl Sleeper for TokioSleeper {
    async fn sleep(&self, d: Duration) {
        tokio::time::sleep(d).await;
    }
}

/// Bridges the synchronous [`RcEventSink`] callback into the loop task:
/// `Client::rc_events` calls `on_event` inline while the loop awaits the
/// connection future, so the sink forwards each event through an unbounded
/// channel the loop `select!`s on alongside the connection. Send failures are
/// ignored here — the loop side owns shutdown (it stops when ITS sends fail).
struct ChannelSink {
    tx: mpsc::UnboundedSender<RcEvent>,
}

impl RcEventSink for ChannelSink {
    fn on_event(&self, ev: RcEvent) {
        let _ = self.tx.send(ev);
    }
}

/// The loop must exit: the [`RcWatcherUpdate`] receiver was dropped (the
/// subscription is gone — observed as a failed send or via
/// `updates.closed()`), or `running` was cleared by a racing `stop()` whose
/// abort hasn't landed yet. Bubbles up through `?`.
struct LoopExit;

/// Emit one update, mapping a closed channel to [`LoopExit`].
fn send(
    updates: &mpsc::UnboundedSender<RcWatcherUpdate>,
    update: RcWatcherUpdate,
) -> Result<(), LoopExit> {
    updates.send(update).map_err(|_| LoopExit)
}

/// The cross-connection fold + resync state (mobile's `overlay` /
/// `connectedBefore` / `backoff` closure vars, `providers.dart:287-289`).
/// `got_data` is per-connection (mobile's `gotData`, `:323`) and is reset by
/// [`run_connection`] at each connect.
struct FoldState {
    overlay: ActivityOverlay,
    /// True once ANY connection has delivered data (mobile sets
    /// `connectedBefore` on first data, not on stream-open — a connection
    /// that dies dataless does not count).
    connected_before: bool,
    /// First-data-of-this-connection latch; reset per connection.
    got_data: bool,
    /// The NEXT reconnect wait ([`backoff_step`] doubles it as it's spent;
    /// first data resets it to [`INITIAL_BACKOFF`]).
    backoff: Duration,
}

impl FoldState {
    fn new() -> FoldState {
        FoldState {
            overlay: ActivityOverlay::empty(),
            connected_before: false,
            got_data: false,
            backoff: INITIAL_BACKOFF,
        }
    }

    /// Handle one decoded event (mobile's listen callback,
    /// `providers.dart:335-359`): first data of a connection resets the
    /// backoff and — when a prior connection delivered data — clears the
    /// overlay and emits `Resynced` BEFORE the event; then folds and emits
    /// the event with the post-fold snapshot.
    fn on_event(
        &mut self,
        ev: RcEvent,
        updates: &mpsc::UnboundedSender<RcWatcherUpdate>,
    ) -> Result<(), LoopExit> {
        if !self.got_data {
            self.got_data = true;
            self.backoff = INITIAL_BACKOFF;
            if self.connected_before {
                self.overlay = ActivityOverlay::empty();
                send(updates, RcWatcherUpdate::Resynced)?;
            }
            self.connected_before = true;
        }
        self.overlay = self.overlay.apply(&ev);
        send(
            updates,
            RcWatcherUpdate::Event {
                event: ev,
                overlay: self.overlay.clone(),
            },
        )
    }
}

/// One connection attempt: connect, fold/forward each event as it arrives,
/// and — once the connection future completes — drain the queued tail.
/// Returns the connection's outcome for the caller's `Down`. (The caller's
/// `while running` condition is the entry-side `running` re-check.)
async fn run_connection(
    connector: &dyn EventsConnector,
    state: &mut FoldState,
    updates: &mpsc::UnboundedSender<RcWatcherUpdate>,
    running: &AtomicBool,
) -> Result<Result<(), ShedError>, LoopExit> {
    state.got_data = false;
    let (ev_tx, mut ev_rx) = mpsc::unbounded_channel::<RcEvent>();
    let sink = ChannelSink { tx: ev_tx };
    let mut conn = connector.connect(&sink);
    let outcome = loop {
        tokio::select! {
            res = &mut conn => break res,
            // `recv` can't yield None while the connection runs (the sink
            // holds the sender); if it somehow did, the branch disables and
            // the select waits out the connection.
            Some(ev) = ev_rx.recv() => {
                // The running re-check shrinks the soft-stop window (see
                // `stop()`): a poll in flight when stop() raced must not
                // emit after `running` cleared.
                if !running.load(Ordering::SeqCst) {
                    return Err(LoopExit);
                }
                state.on_event(ev, updates)?;
            }
            // The consumer unsubscribed (receiver dropped). A healthy
            // heartbeat-only stream delivers no events (the SSE parser
            // swallows comments), so a send-failure would never be observed
            // on it — without this arm an abandoned receiver would leak the
            // task + TCP connection for as long as the server kept the quiet
            // stream open.
            () = updates.closed() => return Err(LoopExit),
        }
    };
    // The connection future is done, so every sink.on_event send has already
    // happened (rc_events calls the sink inline, including the final
    // unterminated-record flush) — drain the queued tail so those events are
    // emitted BEFORE the Down. (No closed() watch needed here: these sends
    // observe a dropped receiver directly.)
    while let Ok(ev) = ev_rx.try_recv() {
        if !running.load(Ordering::SeqCst) {
            return Err(LoopExit);
        }
        state.on_event(ev, updates)?;
    }
    Ok(outcome)
}

/// The watcher loop: connect → forward/fold events as they arrive → on
/// stream end emit `Down` → back off → reconnect. Exits when the receiver is
/// dropped (a failed send, or `updates.closed()` during the connection /
/// backoff awaits — [`LoopExit`] via `?`) or `running` clears; `stop()`/
/// `Drop` additionally abort it mid-await.
async fn run_loop(
    connector: Arc<dyn EventsConnector>,
    sleeper: Arc<dyn Sleeper>,
    running: Arc<AtomicBool>,
    updates: mpsc::UnboundedSender<RcWatcherUpdate>,
) -> Result<(), LoopExit> {
    let mut state = FoldState::new();
    while running.load(Ordering::SeqCst) {
        let outcome = run_connection(&*connector, &mut state, &updates, &running).await?;
        // Soft-stop window shrink: no Down for a stop()-initiated teardown.
        if !running.load(Ordering::SeqCst) {
            break;
        }
        let reason = match outcome {
            Ok(()) => "stream ended".to_string(),
            Err(e) => e.to_string(),
        };
        send(&updates, RcWatcherUpdate::Down { reason })?;
        let (wait, next) = backoff_step(state.backoff);
        state.backoff = next;
        tokio::select! {
            () = sleeper.sleep(wait) => {}
            // An unsubscribe during the backoff must neither wait out the
            // sleep nor launch one more connection for a gone subscription.
            () = updates.closed() => return Err(LoopExit),
        }
    }
    Ok(())
}

// Deterministic loop tests via the scripted-connector + sleeper seams
// (backoff/resync/lifecycle), plus real-transport end-to-end tests (httpmock
// + a raw-TCP held-open stream) for the connect→fold→emit path and the
// abort-closes-the-connection guarantee. Covers plan 001 AC#7.
#[cfg(test)]
mod tests {
    use super::*;

    use std::collections::VecDeque;
    use std::sync::atomic::AtomicUsize;
    use std::sync::Mutex;

    use httpmock::prelude::*;
    use tokio::runtime::Handle;

    use shed_core::rc::{RcActivity, RcState};

    /// One scripted connection outcome.
    enum Step {
        /// Deliver these events to the sink, then return (Ok = clean EOF,
        /// Err(msg) = `ShedError::Transport(msg)`).
        Deliver(Vec<RcEvent>, Result<(), &'static str>),
        /// Never return; sets the connector's `pending_dropped` flag when the
        /// connection future is dropped (abort / loop exit).
        Pend,
    }

    /// Scripted [`EventsConnector`]: each `connect` pops the next [`Step`];
    /// an exhausted script pends (so a finished test scenario just idles
    /// until the watcher is stopped/dropped).
    struct ScriptedConnector {
        steps: Mutex<VecDeque<Step>>,
        connects: AtomicUsize,
        pending_dropped: Arc<AtomicBool>,
    }

    impl ScriptedConnector {
        fn new(steps: Vec<Step>) -> Arc<ScriptedConnector> {
            Arc::new(ScriptedConnector {
                steps: Mutex::new(steps.into()),
                connects: AtomicUsize::new(0),
                pending_dropped: Arc::new(AtomicBool::new(false)),
            })
        }

        fn connects(&self) -> usize {
            self.connects.load(Ordering::SeqCst)
        }

        fn pending_dropped(&self) -> bool {
            self.pending_dropped.load(Ordering::SeqCst)
        }
    }

    /// Sets a flag when dropped — held across the `Pend` await so a test can
    /// prove the in-flight connection future was dropped (not leaked) on
    /// abort.
    struct SetOnDrop(Arc<AtomicBool>);
    impl Drop for SetOnDrop {
        fn drop(&mut self) {
            self.0.store(true, Ordering::SeqCst);
        }
    }

    #[async_trait::async_trait]
    impl EventsConnector for ScriptedConnector {
        async fn connect(&self, sink: &dyn RcEventSink) -> Result<(), ShedError> {
            self.connects.fetch_add(1, Ordering::SeqCst);
            let step = self.steps.lock().unwrap().pop_front();
            match step {
                Some(Step::Deliver(events, outcome)) => {
                    for ev in events {
                        sink.on_event(ev);
                    }
                    outcome.map_err(|msg| ShedError::Transport(msg.to_string()))
                }
                Some(Step::Pend) | None => {
                    let _guard = SetOnDrop(self.pending_dropped.clone());
                    std::future::pending().await
                }
            }
        }
    }

    /// Records each requested wait and returns immediately — deterministic
    /// backoff assertions with no real time.
    #[derive(Default)]
    struct RecordingSleeper {
        waits: Mutex<Vec<Duration>>,
    }

    impl RecordingSleeper {
        fn waits(&self) -> Vec<Duration> {
            self.waits.lock().unwrap().clone()
        }
    }

    #[async_trait::async_trait]
    impl Sleeper for RecordingSleeper {
        async fn sleep(&self, d: Duration) {
            self.waits.lock().unwrap().push(d);
        }
    }

    /// Records the wait, then pends forever — parks the loop "mid-backoff"
    /// so stop-during-backoff is testable without real time.
    #[derive(Default)]
    struct ParkingSleeper {
        waits: Mutex<Vec<Duration>>,
        parked_dropped: Arc<AtomicBool>,
    }

    #[async_trait::async_trait]
    impl Sleeper for ParkingSleeper {
        async fn sleep(&self, d: Duration) {
            self.waits.lock().unwrap().push(d);
            let _guard = SetOnDrop(self.parked_dropped.clone());
            std::future::pending::<()>().await;
        }
    }

    fn spawn_scripted(
        connector: &Arc<ScriptedConnector>,
        sleeper: Arc<dyn Sleeper>,
    ) -> (RcEventsWatcher, mpsc::UnboundedReceiver<RcWatcherUpdate>) {
        RcEventsWatcher::spawn_with(&Handle::current(), connector.clone(), sleeper, "srv".into())
    }

    async fn recv(rx: &mut mpsc::UnboundedReceiver<RcWatcherUpdate>) -> RcWatcherUpdate {
        tokio::time::timeout(Duration::from_secs(5), rx.recv())
            .await
            .expect("timed out waiting for an update")
            .expect("update channel closed")
    }

    fn expect_event(u: RcWatcherUpdate) -> (RcEvent, ActivityOverlay) {
        match u {
            RcWatcherUpdate::Event { event, overlay } => (event, overlay),
            other => panic!("expected Event, got {other:?}"),
        }
    }

    fn expect_down(u: RcWatcherUpdate) -> String {
        match u {
            RcWatcherUpdate::Down { reason } => reason,
            other => panic!("expected Down, got {other:?}"),
        }
    }

    /// Real-time poll helper (the `host_agent.rs` test convention): bounded
    /// at 4s so a hung condition fails the test instead of the suite.
    async fn wait_until(mut cond: impl FnMut() -> bool) -> bool {
        for _ in 0..400 {
            if cond() {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        false
    }

    fn activity_ev(shed: &str, slug: &str) -> RcEvent {
        RcEvent::ActivityChanged {
            shed: shed.into(),
            slug: slug.into(),
            activity: Some(RcActivity::Working),
            activity_at: None,
            state: Some(RcState::Ready),
            last_message: None,
        }
    }

    fn message_ev(shed: &str, slug: &str, seq: u64) -> RcEvent {
        RcEvent::MessageAppended {
            shed: shed.into(),
            slug: slug.into(),
            seq,
        }
    }

    // ---- backoff ----

    #[test]
    fn backoff_step_doubles_from_initial_to_the_cap() {
        // 500ms → 1s → 2s → 4s → 8s → 16s → 30s (capped, not 32s) → 30s.
        let mut backoff = INITIAL_BACKOFF;
        let mut waits = Vec::new();
        for _ in 0..8 {
            let (wait, next) = backoff_step(backoff);
            waits.push(wait);
            backoff = next;
        }
        let secs: Vec<f64> = waits.iter().map(Duration::as_secs_f64).collect();
        assert_eq!(secs, [0.5, 1.0, 2.0, 4.0, 8.0, 16.0, 30.0, 30.0]);
    }

    #[tokio::test]
    async fn backoff_progresses_across_dataless_reconnects_and_resets_on_first_data() {
        // conn1 (no data, err) → wait 500ms; conn2 (no data) → 1s; conn3
        // delivers data (reset!) then dies → 500ms again; conn4 (no data) → 1s.
        let connector = ScriptedConnector::new(vec![
            Step::Deliver(vec![], Err("e1")),
            Step::Deliver(vec![], Err("e2")),
            Step::Deliver(vec![activity_ev("p", "a")], Err("e3")),
            Step::Deliver(vec![], Err("e4")),
        ]);
        let sleeper = Arc::new(RecordingSleeper::default());
        let (watcher, mut rx) = spawn_scripted(&connector, sleeper.clone());

        expect_down(recv(&mut rx).await);
        expect_down(recv(&mut rx).await);
        // conn3's first (and only) data: no prior connection ever delivered
        // data, so there is NO Resynced even though connections existed.
        expect_event(recv(&mut rx).await);
        expect_down(recv(&mut rx).await);
        expect_down(recv(&mut rx).await);

        assert!(wait_until(|| sleeper.waits().len() == 4).await);
        let secs: Vec<f64> = sleeper.waits().iter().map(Duration::as_secs_f64).collect();
        assert_eq!(secs, [0.5, 1.0, 0.5, 1.0]);
        watcher.stop();
    }

    // ---- resync contract ----

    #[tokio::test]
    async fn first_ever_connection_emits_events_without_resynced() {
        let connector = ScriptedConnector::new(vec![Step::Deliver(
            vec![activity_ev("p", "a"), message_ev("p", "a", 5)],
            Ok(()),
        )]);
        let (watcher, mut rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));

        let (ev1, o1) = expect_event(recv(&mut rx).await);
        assert_eq!(ev1, activity_ev("p", "a"));
        assert_eq!(
            o1.lookup("p", "a").unwrap().activity,
            Some(RcActivity::Working)
        );
        // Post-fold snapshot: the second event's overlay carries BOTH the
        // prior fold and the new seq.
        let (ev2, o2) = expect_event(recv(&mut rx).await);
        assert_eq!(ev2, message_ev("p", "a", 5));
        let patch = o2.lookup("p", "a").unwrap();
        assert_eq!(patch.activity, Some(RcActivity::Working));
        assert_eq!(patch.last_seq, Some(5));
        // Clean EOF is a Down too (the watcher will reconnect either way).
        assert_eq!(expect_down(recv(&mut rx).await), "stream ended");
        watcher.stop();
    }

    #[tokio::test]
    async fn resynced_clears_overlay_before_first_event_of_data_bearing_reconnect_only() {
        // conn1 delivers data (connected_before latches) and dies; conn2 dies
        // DATALESS (must emit no Resynced); conn3 delivers data → exactly one
        // Resynced, before conn3's first Event, whose overlay was rebuilt
        // from empty.
        let connector = ScriptedConnector::new(vec![
            Step::Deliver(vec![activity_ev("p", "a")], Err("d1")),
            Step::Deliver(vec![], Err("d2")),
            Step::Deliver(vec![message_ev("p", "b", 7)], Err("d3")),
        ]);
        let (watcher, mut rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));

        let (_, o1) = expect_event(recv(&mut rx).await);
        assert!(o1.lookup("p", "a").is_some());
        assert_eq!(expect_down(recv(&mut rx).await), "transport error: d1");
        // The dataless reconnect: Down only, no Resynced in between.
        assert_eq!(expect_down(recv(&mut rx).await), "transport error: d2");
        // Data-bearing reconnect: Resynced FIRST, then the event folded onto
        // a cleared overlay — conn1's (p,a) patch is gone.
        assert_eq!(recv(&mut rx).await, RcWatcherUpdate::Resynced);
        let (ev, o2) = expect_event(recv(&mut rx).await);
        assert_eq!(ev, message_ev("p", "b", 7));
        assert!(o2.lookup("p", "a").is_none(), "stale patch survived resync");
        assert_eq!(o2.lookup("p", "b").unwrap().last_seq, Some(7));
        assert_eq!(expect_down(recv(&mut rx).await), "transport error: d3");
        watcher.stop();
    }

    #[tokio::test]
    async fn overlay_persists_across_disconnect_no_cleared_snapshot_is_emitted() {
        // After Down, the channel stays SILENT while the watcher is between
        // connections: the consumer keeps rendering the last Event's overlay.
        // (The internal overlay is cleared only by the next connection's
        // first data — the resync test above pins that; this test pins that
        // no cleared-overlay update leaks out during the gap.)
        let connector = ScriptedConnector::new(vec![
            Step::Deliver(vec![activity_ev("p", "a")], Err("blip")),
            Step::Pend, // reconnects, then the "new connection" stays quiet
        ]);
        let (watcher, mut rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));

        let (_, o) = expect_event(recv(&mut rx).await);
        assert!(o.lookup("p", "a").is_some());
        expect_down(recv(&mut rx).await);
        // The second connection is open-but-quiet: nothing may be emitted.
        assert!(wait_until(|| connector.connects() == 2).await);
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(
            matches!(rx.try_recv(), Err(mpsc::error::TryRecvError::Empty)),
            "an update leaked during the disconnect gap"
        );
        watcher.stop();
    }

    // ---- Down reasons ----

    #[tokio::test]
    async fn down_carries_the_error_reason() {
        let connector = ScriptedConnector::new(vec![Step::Deliver(vec![], Err("boom"))]);
        let (watcher, mut rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));
        assert_eq!(expect_down(recv(&mut rx).await), "transport error: boom");
        watcher.stop();
    }

    // ---- lifecycle ----

    #[tokio::test]
    async fn stop_during_an_idle_stream_terminates_promptly_and_drops_the_connection() {
        let connector = ScriptedConnector::new(vec![Step::Pend]);
        let (watcher, _rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));
        assert!(wait_until(|| connector.connects() == 1).await);
        watcher.stop();
        assert!(
            wait_until(|| watcher.task.is_finished()).await,
            "loop did not terminate after stop()"
        );
        // The in-flight connection future was dropped, not leaked.
        assert!(connector.pending_dropped());
    }

    #[tokio::test]
    async fn stop_during_the_backoff_sleep_terminates_promptly() {
        let connector = ScriptedConnector::new(vec![Step::Deliver(vec![], Err("down"))]);
        let sleeper = Arc::new(ParkingSleeper::default());
        let parked_dropped = sleeper.parked_dropped.clone();
        let (watcher, mut rx) = spawn_scripted(&connector, sleeper.clone());
        expect_down(recv(&mut rx).await);
        // The loop is parked inside the backoff sleep.
        assert!(wait_until(|| !sleeper.waits.lock().unwrap().is_empty()).await);
        watcher.stop();
        assert!(
            wait_until(|| watcher.task.is_finished()).await,
            "loop did not terminate out of the backoff sleep"
        );
        assert!(parked_dropped.load(Ordering::SeqCst));
    }

    #[tokio::test]
    async fn dropping_the_watcher_aborts_the_loop() {
        let connector = ScriptedConnector::new(vec![Step::Pend]);
        let (watcher, _rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));
        assert!(wait_until(|| connector.connects() == 1).await);
        drop(watcher);
        assert!(
            wait_until(|| connector.pending_dropped()).await,
            "Drop did not abort the loop"
        );
    }

    #[test]
    fn fold_send_failure_stops_the_loop() {
        // The send-fail half of receiver-drop detection, pinned at the fold
        // level (the loop-level paths race a send against `closed()`, either
        // of which exits): both the Event send and the Resynced send map a
        // dropped receiver to LoopExit.
        let (tx, rx) = mpsc::unbounded_channel();
        drop(rx);
        let mut state = FoldState::new();
        assert!(state.on_event(activity_ev("p", "a"), &tx).is_err());
        // The Resynced path (a data-bearing reconnect) fails the same way.
        let mut state = FoldState::new();
        state.connected_before = true;
        assert!(state.on_event(activity_ev("p", "a"), &tx).is_err());
    }

    #[tokio::test]
    async fn receiver_drop_during_a_quiet_stream_stops_the_loop_and_drops_the_connection() {
        // Codex C5 review finding 1 (the real-leak case): a healthy
        // heartbeat-only stream never reaches the sink (the SSE parser
        // swallows comments), so no send ever happens and a send-failure
        // could never be observed — the loop must notice the dropped
        // receiver via `updates.closed()` and exit, dropping the in-flight
        // connection, instead of living as long as the server keeps the
        // quiet stream open.
        let connector = ScriptedConnector::new(vec![Step::Pend]);
        let (watcher, rx) = spawn_scripted(&connector, Arc::new(RecordingSleeper::default()));
        assert!(wait_until(|| connector.connects() == 1).await);
        drop(rx);
        assert!(
            wait_until(|| watcher.task.is_finished()).await,
            "loop kept running after the receiver was dropped mid-connection"
        );
        assert!(connector.pending_dropped(), "connection future leaked");
    }

    #[tokio::test]
    async fn receiver_drop_during_the_backoff_sleep_stops_the_loop() {
        // The sibling of the quiet-stream case: an unsubscribe while the
        // loop is parked in the backoff sleep must neither wait out the
        // sleep nor launch one more connection for the gone subscription.
        let connector = ScriptedConnector::new(vec![Step::Deliver(vec![], Err("d1"))]);
        let sleeper = Arc::new(ParkingSleeper::default());
        let (watcher, mut rx) = spawn_scripted(&connector, sleeper.clone());
        assert_eq!(expect_down(recv(&mut rx).await), "transport error: d1");
        // The loop is parked inside the backoff sleep.
        assert!(wait_until(|| !sleeper.waits.lock().unwrap().is_empty()).await);
        drop(rx);
        assert!(
            wait_until(|| watcher.task.is_finished()).await,
            "loop waited out the backoff after the receiver was dropped"
        );
        assert_eq!(
            connector.connects(),
            1,
            "loop launched another connection for a gone subscription"
        );
    }

    // ---- end-to-end over the real transport ----

    #[tokio::test]
    async fn end_to_end_connects_folds_and_emits_then_resyncs_on_reconnect() {
        // Real Client + httpmock: the mock serves the same two-event body on
        // every connection, so cycle 1 pins connect → decode → fold → emit
        // ordering and cycle 2 (a real 500ms-backoff reconnect) pins the
        // Resynced-then-rebuilt-overlay contract end-to-end.
        let server = MockServer::start_async().await;
        let sse = ": ok\n\n\
                   event: activity.changed\n\
                   data: {\"shed\":\"p\",\"slug\":\"a\",\"activity\":\"working\",\"state\":\"ready\"}\n\n\
                   event: message.appended\n\
                   data: {\"shed\":\"p\",\"slug\":\"a\",\"seq\":5}\n\n";
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/rc/events")
                    .header("accept", "text/event-stream");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(sse);
            })
            .await;
        let client =
            Client::new(server.base_url(), "srv".into(), String::new(), None, None).unwrap();
        let (watcher, mut rx) = RcEventsWatcher::spawn(&Handle::current(), client, "srv".into());
        assert_eq!(watcher.server_name(), "srv");

        // Cycle 1: events in order, each with its post-fold snapshot.
        let (ev1, o1) = expect_event(recv(&mut rx).await);
        assert_eq!(ev1, activity_ev("p", "a"));
        let p1 = o1.lookup("p", "a").unwrap();
        assert_eq!(p1.activity, Some(RcActivity::Working));
        assert_eq!(p1.last_seq, None);
        let (ev2, o2) = expect_event(recv(&mut rx).await);
        assert_eq!(ev2, message_ev("p", "a", 5));
        let p2 = o2.lookup("p", "a").unwrap();
        assert_eq!(p2.activity, Some(RcActivity::Working)); // held across the fold
        assert_eq!(p2.last_seq, Some(5));
        assert_eq!(expect_down(recv(&mut rx).await), "stream ended");

        // Cycle 2 (after the real initial backoff): a data-bearing reconnect
        // resyncs — and the first event's overlay proves the clear (last_seq
        // from cycle 1 is gone until this cycle's message.appended re-folds).
        assert_eq!(recv(&mut rx).await, RcWatcherUpdate::Resynced);
        let (ev3, o3) = expect_event(recv(&mut rx).await);
        assert_eq!(ev3, activity_ev("p", "a"));
        assert_eq!(o3.lookup("p", "a").unwrap().last_seq, None);

        watcher.stop();
        assert!(wait_until(|| watcher.task.is_finished()).await);
    }

    #[tokio::test]
    async fn stop_closes_a_live_real_connection() {
        // Plan 001 §9's named risk: abort-based cancel must not leak the
        // reqwest connection. A raw-TCP server (httpmock can't hold a stream
        // open) writes one event and then BLOCKS reading the socket; it can
        // only unblock when the peer closes. stop() → task abort → the
        // connection future (and the Client) drop → TCP close → the server
        // thread finishes. A bounded join proves the whole chain.
        use std::io::{Read, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let srv = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            stream.set_nodelay(true).unwrap();
            let mut buf = [0u8; 4096];
            let mut req = Vec::new();
            loop {
                let n = stream.read(&mut buf).unwrap_or(0);
                assert!(n > 0, "client closed before sending a request");
                req.extend_from_slice(&buf[..n]);
                if req.windows(4).any(|w| w == b"\r\n\r\n") {
                    break;
                }
            }
            stream
                .write_all(
                    b"HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\nconnection: close\r\n\r\n\
                      event: message.appended\ndata: {\"shed\":\"p\",\"slug\":\"a\",\"seq\":1}\n\n",
                )
                .unwrap();
            // Block until the client closes the connection (read → 0/Err).
            loop {
                match stream.read(&mut buf) {
                    Ok(0) | Err(_) => return,
                    Ok(_) => {}
                }
            }
        });
        let client = Client::new(url, "srv".into(), String::new(), None, None).unwrap();
        let (watcher, mut rx) = RcEventsWatcher::spawn(&Handle::current(), client, "srv".into());
        // The stream is live: the event arrived, the connection is held open.
        let (ev, _) = expect_event(recv(&mut rx).await);
        assert_eq!(ev, message_ev("p", "a", 1));
        watcher.stop();
        assert!(wait_until(|| watcher.task.is_finished()).await);
        // Bounded join: hangs (→ test failure) iff the connection leaked.
        tokio::time::timeout(
            Duration::from_secs(10),
            tokio::task::spawn_blocking(move || srv.join().expect("sse server thread panicked")),
        )
        .await
        .expect("server thread did not observe the connection close")
        .unwrap();
    }
}
