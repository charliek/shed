//! The lane's watcher (plan 025 §3.3.5, P12): one subscription's pump — seed,
//! silent resume, reseed, terminal `Down` — over one connection at a time.
//!
//! The PATTERN is shed-gx's watcher (`crates/shed-gx/src/watcher.rs` at
//! `203250f`, deleted by plan 025 C1): a reseed announces itself and rebuilds,
//! a silent resume says nothing and keeps everything, a dropped client frame
//! ends the generation, the open streak is flushed on every terminal path and
//! by a 2 s clock. The wire is craze's: a host journal decides whether a cursor
//! is honoured, so where gx bounded its own lossy resume, this lane offers the
//! cursor on EVERY reconnect and lets the host's answer be the bound.
//!
//! # The state machine
//!
//! | state | trigger | action | emitted |
//! |---|---|---|---|
//! | dialling | a dial, the splice or `sessions.list` fails — a dial that hands back no connection within 30 s ([`LaneTimings::dial`]) included | backoff 200 ms → 30 s, redial | `Stale{"reconnecting"}`, once per outage |
//! | dialling | 10 minutes since the outage began without an attachment reaching its `synchronized` (checked before every dial and after every failed one; a dial never outlives the bound) | end | `Down{"unreachable"}` |
//! | dialling | the hub splices ANOTHER host than the hostId the connect named, or the host's row names another | end | `Down{"protocol: …"}` |
//! | dialling | `session.connect` refused `unknown_session`, or the host lists no session | end | `Down{"unknown_session"}` (`Down{"session_closed"}` when it confirms a closing `omitted`, below) |
//! | dialling | five consecutive other connect refusals | end | `Down{"connect refused: <code/reason>"}` |
//! | dialling | craze not installed / too old | end | `Down{"craze unavailable: …"}` |
//! | attaching | any attach answered (Amendment A11's fenced read) | read the engine's ask registry — `asks.list`, then `asks.get` per id — then `session.sync` (its `seq` is the FENCE); events after the attach queue meanwhile | — |
//! | attaching | a no-cursor attach answered with a snapshot | flush; new generation; fresh fold under the session's cards: the snapshot's rows, the REGISTRY's approvals (a sub-agent's included; a snapshot ask the registry lacks is rows only) | `Reset{reason}`, the rows, `Session`, `Capabilities`, `Settings` (when they say so), one `Approval` per open registry ask |
//! | attaching | a cursor REFUSED (a snapshot came back) | the same reseed | `Reset{cursor_lost:<reason>}` … |
//! | attaching | the cursor honoured (no snapshot) | keep the fold; re-read row, info and the registry (an approval it no longer lists resolved, one it lists opened) | `Session`/`Approval`/`Capabilities`/`Settings` when changed |
//! | attaching | refused `unknown_session` / `not_accepting`+`start_failed` / any other `not_accepting` | end | `Down{"unknown_session"}` (`"session_closed"` when confirming a closing `omitted`) / `Down{"start_failed: <cause>"}` / `Down{"session_closed"}` |
//! | attaching | refused `unavailable`/`in_progress` | wait, re-attach (counted) | — |
//! | attaching | the 9th attach of an episode | end | `Down{"re-attach bound"}` |
//! | following | `event` (contiguous seq, this subscription) | fold | `Message`s, `Session`/`Approval`/`Capabilities`/`Settings` on change |
//! | following | 60 s ([`LaneTimings::sync_wait`]) without a notification of this attachment, before its `synchronized` — progress-based, so a long replay that keeps delivering is never cut | close; redial by the rules below (before the seed's `Ready`, reseed; after it, the cursor) | `Stale{"reconnecting"}` |
//! | following | `synchronized` at the last seq held | the episode count and the outage reset | — |
//! | following | the attachment SETTLED — its `synchronized` came AND every event through the fence is folded — while seeding | the seed is complete | `Ready{generation}` |
//! | following | the same, after a loss | the resume is complete | `Session` (re-read), `Settings` (restated, changed or not), lone `Ready{same generation}` |
//! | following | a hole or duplicate seq; a second, early, late or wrong-subscription `synchronized`; a frame that does not read | close; reseed | `Stale`, then `Reset{protocol}` … |
//! | following | `presence` | — | `Session{attached}` |
//! | following | a `session.set` this lane made was answered `rev: 0` (no `meta` delta will carry it) | apply its confirmed value to the fold | `Settings` when changed |
//! | following | `ready` (the final info document) | — | `Capabilities`/`Settings` on change; `Down{"start_failed: …"}` when it failed |
//! | following | `reset{slow_consumer}` after readiness | re-attach WITH the cursor, same connection | — (a snapshot back reseeds) |
//! | following | `reset{slow_consumer}` before readiness, `omitted`, `replay_failed`, an unknown reason | re-attach with NO cursor, same connection | `Reset{server_reset:<r>}` … |
//! | following | `reset{omitted}`, and its re-attach then gets no answer (the connection ends, or the deadline passes) | that is how the closing `omitted` ends a connection — and how a dropped transport does: redial and CONFIRM with a no-cursor attach; a definite "no such session" answer ends it, a snapshot reseeds | `Stale`, then `Down{"session_closed"}` or `Reset{server_reset:omitted}` … |
//! | following | `reset{session_replaced}` | forget the session id; redial; no-cursor attach | `Reset{server_reset:session_replaced}` … |
//! | following | `reset{session_closed}` | end | `Down{"session_closed"}` |
//! | following | EOF, a read error, a verb's deadline (it closes the connection) — after `Ready` | flush; redial; attach WITH the cursor | `Stale{"reconnecting"}` |
//! | following | the same, before the seed's `Ready` | flush; redial; reseed (never resume a half seed) | `Stale`, then `Reset{reconnect}` … |
//! | any | the client channel full (`Lagged`), or the connection's notification queue (`ConnEnd::Backlog`) | drop the connection; wait for the drain holding nothing; backoff; reseed (correction 13 — never resume) | `Reset{lagged}` … |
//! | any | the flush clock (2 s without growth) | flush the open segment | `Message` |
//!
//! **A silent resume never changes the generation; any generation change has
//! the full `Reset … Ready` bracket.** Every `Ready` waits for its
//! attachment's `synchronized` at the last event held (PM: "the stream has
//! delivered through this attachment's cutoff") AND for every event through
//! the fenced read's `session.sync` seq (Amendment A11): the registry was read
//! after the attach and before the barrier, so an ask that opened or ended
//! around the read is folded over it, in seq order, before the `Ready` — none
//! is published pending at a `Ready` that the engine had already resolved. The open segment is flushed
//! before `Stale`, before a reseed's `Reset`, and before `Down`; `Down` goes
//! through `publish_final`, the one frame never dropped, and the rows that
//! flush puts ahead of it wait for room the same way (`publish_waiting`) —
//! nothing after a `Down` could restore them.
//!
//! # The bounds (PM "Re-attaching, and the per-episode bound")
//!
//! Every attach attempt after the subscription's first — after a reset, a
//! reconnect, or a retried refusal — counts toward
//! [`LaneTimings::reattaches`] (8), reset only at a `synchronized`. Dials give
//! up [`LaneTimings::give_up`] (10 minutes) after the outage began without an
//! attachment reaching its `synchronized` — an attach a host answers and then
//! never synchronizes recovers nothing, so a host that does that forever is
//! ended by one bound or the other. A client-channel lag resets both: it is positive
//! evidence the host served (gx's lesson: a slow consumer must never be
//! convicted of an unreachable agent). Every clock here is tokio's, so a test
//! runs the bounds on a paused clock.

use std::collections::HashMap;
use std::sync::Arc;

use serde::Deserialize;
use serde_json::Value;
use shed_core::lane::backoff::{jittered, next_backoff};
use shed_core::lane::ring::MessageRing;
use shed_core::lane::{
    LaneApproval, LaneCapabilities, LaneEvent, LanePublisher, LaneSession, LaneSettings, LaneStop,
    LaneSubscription, Publish,
};
use tokio::time::Instant;

use crate::conn::{lock, CallError, Conn, ConnEnd, Notification, Notifications};
use crate::dial::{connect_hub, DialError};
use crate::errors::start_failed_cause;
use crate::fold::{wording, Cards, CrazeFold};
use crate::lane::{
    lane_capabilities, list_one, now_unix_ms, same_host, splice, LaneShared, ListError, Live,
    Splice,
};
use crate::segment::FlushClock;
use crate::source::{lane_session, work_activity};
use crate::wire::{
    code, method, notify, reason, reset, AskGetResult, AskParams, AskRecord, AsksListResult,
    AttachParams, AttachResult, Cursor, EventParams, PresenceParams, ReadyParams, ResetParams,
    RosterRow, SessionInfo, SessionParams, SyncParams, SyncResult,
};

/// The most transcript rows one seed carries: what the ring keeps anyway
/// ([`shed_core::lane::ring::MAX_RING_MESSAGES`], 500), so a seed — its rows,
/// `Reset`, `Session`, `Capabilities`, `Settings` and its approvals (at most
/// [`crate::fold::MAX_APPROVALS`]) — fits the 1,024-frame client channel with
/// room for the replay behind it. A longer snapshot window seeds its newest 500
/// rows under one "earlier transcript omitted" row.
pub const SEED_ROWS: usize = shed_core::lane::ring::MAX_RING_MESSAGES;

/// Spawn one subscription's watcher.
pub(crate) fn spawn(shared: Arc<LaneShared>, hint: Option<String>) -> LaneSubscription {
    let (tx, rx) = LanePublisher::channel();
    let owner = shared.watcher_id();
    let confirms = shared.confirmed.subscribe();
    let watcher = Watcher {
        host_id: shared.host_id.clone(),
        fold: CrazeFold::new(&shared.host_id),
        shared,
        tx,
        owner,
        ring: MessageRing::new(),
        generation: 0,
        seeded: false,
        stale: false,
        cursor: None,
        info: None,
        base_row: None,
        presence: None,
        session_ready: false,
        emitted_approvals: HashMap::new(),
        last_session: None,
        last_caps: None,
        last_settings: None,
        flush: FlushClock::default(),
        attached_once: false,
        reattaches: 0,
        outage: None,
        refusals: 0,
        confirm_closed: false,
        confirms,
    };
    let task = tokio::spawn(watcher.run(hint));
    LaneSubscription {
        rx,
        stop: LaneStop::new(task),
    }
}

/// What the next connection does.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Plan {
    /// A no-cursor attach: a snapshot, a new generation (`Reset{reason}`).
    Reseed(String),
    /// Attach WITH the cursor: the host decides.
    Resume,
}

/// How a connection — or the subscription — ended.
#[derive(Debug)]
enum End {
    /// Terminal: `Down{reason}`.
    Down(String),
    /// A frame was dropped (correction 13), or the connection's own
    /// notification queue overflowed: drain, then reseed.
    Lagged,
    /// The subscriber is gone.
    Closed,
    /// The connection is gone, or never came. The client sees only
    /// `Stale{"reconnecting"}` (and the bounds end in their own pinned
    /// words), so why it went is not kept.
    Lost {
        /// An attach succeeded on it (the backoff resets).
        worked: bool,
        /// The next connection must reseed, with this reason.
        reseed: Option<String>,
    },
}

impl End {
    /// [`End::Lost`] with nothing to reseed for: the next connection resumes
    /// or reseeds by the plan's own rule.
    fn lost(worked: bool) -> End {
        End::Lost {
            worked,
            reseed: None,
        }
    }
}

/// `Ok` while the client keeps up; `Err(End::Lagged)` at the first dropped
/// frame, which every emitting helper propagates.
type Emitted = Result<(), End>;

/// What following an attachment came to.
enum Follow {
    /// Re-attach on the same connection.
    Reattach {
        cursor: bool,
        reason: String,
        /// After an `omitted`: an attach the connection ends under means the
        /// `omitted` was the session's closing reset.
        omitted: bool,
    },
    End(End),
}

struct Watcher {
    shared: Arc<LaneShared>,
    host_id: String,
    tx: LanePublisher,
    owner: u64,
    fold: CrazeFold,
    /// One ring for the life of the subscription: `seq` stays monotonic across
    /// reseeds (the contract's rule).
    ring: MessageRing,
    generation: u64,
    /// The current generation reached its `Ready`.
    seeded: bool,
    /// A `Stale` is out and no `Ready` has answered it.
    stale: bool,
    /// The last event folded: what a resume offers.
    cursor: Option<Cursor>,
    /// The latest info document.
    info: Option<SessionInfo>,
    /// The session row the host's `sessions.list` gave this connection.
    base_row: Option<LaneSession>,
    presence: Option<u32>,
    /// The session's start has run (the attach reply's `ready`, or a `ready`).
    session_ready: bool,
    emitted_approvals: HashMap<String, LaneApproval>,
    last_session: Option<LaneSession>,
    last_caps: Option<LaneCapabilities>,
    last_settings: Option<LaneSettings>,
    flush: FlushClock,
    /// The subscription's first attach has been made: every later one counts.
    attached_once: bool,
    /// Attach attempts this episode (since the last `synchronized`).
    reattaches: u32,
    /// When the current outage began; `None` once an attachment reached its
    /// `synchronized` (an attach that never synchronizes recovered nothing).
    outage: Option<Instant>,
    /// Consecutive refused `session.connect`s.
    refusals: u32,
    /// A `reset{omitted}`'s re-attach failed without an answer (a drop, a
    /// deadline): the `omitted` MAY have been the session's closing reset, and
    /// only the next connection's definite answer can say. Until then a
    /// "no such session" answer is that close, `Down{"session_closed"}`.
    confirm_closed: bool,
    /// The lane's `rev: 0` confirmations (`LaneShared::confirmed`): changes
    /// craze made that no `meta` delta will carry, applied to this fold.
    confirms: tokio::sync::broadcast::Receiver<crate::wire::Setting>,
}

/// Takes the live connection back from the verbs however the watcher ends —
/// an abort included (a dropped future runs no code but its destructors).
struct LiveGuard {
    shared: Arc<LaneShared>,
    owner: u64,
}

impl Drop for LiveGuard {
    fn drop(&mut self) {
        self.shared.set_live(None, self.owner);
        // The reads stop answering from this watcher's fold — unless a newer
        // watcher has seeded since, whose fold they answer from now.
        self.shared.unseed(self.owner);
    }
}

impl Watcher {
    async fn run(mut self, hint: Option<String>) {
        let _guard = LiveGuard {
            shared: Arc::clone(&self.shared),
            owner: self.owner,
        };
        let t = self.shared.timings;
        // The caller's cursor is not honoured (this lane resumes from a cursor
        // it keeps itself), and the first `Reset` says so.
        let mut plan = Plan::Reseed(
            if hint.is_some() {
                "connect:cursor-ignored"
            } else {
                "connect"
            }
            .to_string(),
        );
        let mut backoff = t.backoff_base;
        loop {
            if self.tx.is_closed() {
                return;
            }
            let end = self.connection(&plan).await;
            self.shared.set_live(None, self.owner);
            let lagged = match end {
                End::Closed => return,
                End::Down(reason) => return self.go_down(reason).await,
                End::Lagged => true,
                End::Lost { worked, reseed } => {
                    let staled = self.went_stale();
                    plan = match reseed {
                        Some(r) => Plan::Reseed(r),
                        None if self.seeded && self.cursor.is_some() => Plan::Resume,
                        None => Plan::Reseed("reconnect".to_string()),
                    };
                    backoff = next_backoff(backoff, worked, t.backoff_base, t.backoff_max);
                    staled.is_err()
                }
            };
            if lagged {
                // The connection is already gone: wait for the client to drain
                // holding nothing, then reseed — never resume, the dropped
                // frames may sit behind the cursor.
                if self.tx.wait_drained().await.is_err() {
                    return;
                }
                // A lag is evidence the host served: neither bound is the
                // consumer's to spend. The backoff still climbs, so a consumer
                // draining a frame at a time is no reseed-per-frame load.
                self.outage = None;
                self.reattaches = 0;
                plan = Plan::Reseed("lagged".to_string());
                backoff = next_backoff(backoff, false, t.backoff_base, t.backoff_max);
            }
            tokio::time::sleep(jittered(backoff)).await;
        }
    }

    // ---- emitting ----

    /// Publish one frame. A closed receiver is noticed at the loop's next
    /// check; a FULL one is the generation's end (correction 13).
    fn emit(&self, ev: LaneEvent) -> Emitted {
        match self.tx.publish(ev) {
            Publish::Sent | Publish::Closed => Ok(()),
            Publish::Lagged => Err(End::Lagged),
        }
    }

    /// The fold's rows, numbered by the ring, as `Message`s — then the flush
    /// clock noted.
    fn drain_rows(&mut self) -> Emitted {
        let rows = self.fold.drain();
        let now = now_unix_ms();
        let mut sent = Ok(());
        for row in rows {
            let message = self.ring.append(row, now);
            if sent.is_ok() {
                sent = self.emit(LaneEvent::Message {
                    message,
                    cursor: None,
                });
            }
        }
        self.flush.note(self.fold.open_bytes(), Instant::now());
        sent
    }

    /// The session row, when it changed: the host's row, the fold's activity
    /// (newer than the row once the stream has said something) with the
    /// approval override and its pending count, the presence count, the
    /// compaction's working label.
    fn emit_session(&mut self) -> Emitted {
        let Some(base) = self.base_row.as_ref() else {
            return Ok(());
        };
        let mut row = base.clone();
        row.id.clone_from(&self.host_id);
        row.approximate = false;
        row.tab_id = None;
        if let Some(activity) = self.fold.activity() {
            row.activity = activity;
        }
        row.pending_approvals = self.fold.open_approvals();
        if self.fold.compacting() {
            row.doing = Some(wording::COMPACTING.to_string());
        }
        if self.presence.is_some() {
            row.attached = self.presence;
        }
        if let Some(info) = &self.info {
            if !info.provider_session_id.is_empty() {
                row.provider_session_id = Some(info.provider_session_id.clone());
            }
            if info.permission_mode.is_some() {
                row.permission_mode.clone_from(&info.permission_mode);
            }
        }
        if self.last_session.as_ref() == Some(&row) {
            return Ok(());
        }
        self.last_session = Some(row.clone());
        lock(&self.shared.state).session = Some(row.clone());
        self.emit(LaneEvent::Session { session: row })
    }

    /// The session's capabilities (§3.9), when they changed.
    fn emit_capabilities(&mut self) -> Emitted {
        let Some(info) = &self.info else {
            return Ok(());
        };
        let caps = lane_capabilities(&info.capabilities, self.fold.settings().has_any());
        if !caps.settings {
            // Nothing to wait for: a config change has no model to bind.
            self.shared.settings_known.send_replace(true);
        }
        if self.last_caps.as_ref() == Some(&caps) {
            return Ok(());
        }
        self.last_caps = Some(caps.clone());
        lock(&self.shared.state).capabilities = Some(caps.clone());
        self.emit(LaneEvent::Capabilities { capabilities: caps })
    }

    /// The settings, when they changed — and only on a session whose
    /// capabilities say `settings`.
    fn emit_settings(&mut self) -> Emitted {
        if !self.last_caps.as_ref().is_some_and(|c| c.settings) {
            return Ok(());
        }
        let settings = self.fold.settings().lane_settings();
        if self.last_settings.as_ref() == Some(&settings) {
            return Ok(());
        }
        self.last_settings = Some(settings.clone());
        lock(&self.shared.state).settings = Some(settings.clone());
        self.shared.settings_known.send_replace(true);
        self.emit(LaneEvent::Settings { settings })
    }

    /// An `Approval` for every approval whose DTO changed, in id order (gx's
    /// `emit_approvals`): id-keyed, last-write-wins.
    fn emit_approvals(&mut self) -> Emitted {
        let held = self.fold.held_approvals();
        let changed: Vec<LaneApproval> = held
            .iter()
            .filter(|a| self.emitted_approvals.get(&a.id) != Some(*a))
            .cloned()
            .collect();
        // The verbs' view first, so a client that reacts to the frame finds
        // the approval as it says.
        lock(&self.shared.state).approvals = held;
        let mut sent = Ok(());
        for a in changed {
            self.emitted_approvals.insert(a.id.clone(), a.clone());
            sent = self.emit(LaneEvent::Approval { approval: a });
            if sent.is_err() {
                break;
            }
        }
        // The book is bounded and evicts settled entries; this map follows it.
        let fold = &self.fold;
        self.emitted_approvals
            .retain(|k, _| fold.approval(k).is_some());
        sent
    }

    /// What a folded event may have changed, published in the seed's own
    /// order: rows, the row, approvals (only when the book moved),
    /// capabilities, settings.
    fn after_fold(&mut self) -> Emitted {
        self.drain_rows()?;
        self.emit_session()?;
        if self.fold.take_approvals_changed() {
            self.emit_approvals()?;
        }
        if self.fold.take_settings_changed() {
            self.emit_capabilities()?;
            self.emit_settings()?;
        }
        Ok(())
    }

    /// A loss: flush the open segment, then say `Stale` — once per outage.
    fn went_stale(&mut self) -> Emitted {
        self.fold.flush();
        self.drain_rows()?;
        if self.stale {
            return Ok(());
        }
        self.stale = true;
        self.emit(LaneEvent::Stale {
            reason: "reconnecting".to_string(),
        })
    }

    /// End the subscription: the open segment flushed as a partial row, then
    /// `Down` — the frame `publish_final` never drops. The rows ahead of it
    /// WAIT for room too (`publish_waiting`): there is no reseed after a
    /// `Down` to restore a row a full channel dropped, so a terminal end never
    /// costs the transcript its last words.
    async fn go_down(mut self, reason: String) {
        self.fold.flush();
        let now = now_unix_ms();
        for row in self.fold.drain() {
            let message = self.ring.append(row, now);
            let row = LaneEvent::Message {
                message,
                cursor: None,
            };
            if self.tx.publish_waiting(row).await.is_err() {
                return;
            }
        }
        self.tx.publish_final(LaneEvent::Down { reason }).await;
    }

    /// A definite "no such session" answer — the host gone from the hub, its
    /// list empty, an attach refused `unknown_session` or `not_accepting`:
    /// terminal as `reason`, or as the session's close when it confirms that a
    /// closing `omitted` was one (`confirm_closed`).
    fn no_session(&self, reason: &str) -> End {
        End::Down(if self.confirm_closed {
            "session_closed".to_string()
        } else {
            reason.to_string()
        })
    }

    // ---- one connection ----

    async fn connection(&mut self, plan: &Plan) -> End {
        let t = self.shared.timings;
        let since = *self.outage.get_or_insert_with(Instant::now);
        if since.elapsed() >= t.give_up {
            return End::Down("unreachable".to_string());
        }
        // A dial never outlives the bound: it gets what is left of it, at most
        // its own deadline.
        let dial_within = t.dial.min(t.give_up.saturating_sub(since.elapsed()));
        let (conn, notes, _hub) = match connect_hub(
            &*self.shared.dial,
            &self.shared.client,
            dial_within,
            t.hello,
        )
        .await
        {
            Ok(c) => c,
            Err(e @ (DialError::NotInstalled(_) | DialError::TooOld(_))) => {
                return End::Down(format!("craze unavailable: {e}"))
            }
            // Re-checked after every attempt, a held dial's included.
            Err(_) if since.elapsed() >= t.give_up => return End::Down("unreachable".to_string()),
            Err(_) => return End::lost(false),
        };
        let conn = Arc::new(conn);
        let end = self.on_connection(&conn, notes, plan.clone()).await;
        // However it ended, nothing more is read or written on it — an
        // in-flight verb learns that its outcome is unknown.
        conn.close("the lane let this connection go");
        end
    }

    async fn on_connection(&mut self, conn: &Arc<Conn>, notes: Notifications, plan: Plan) -> End {
        let t = self.shared.timings;
        match splice(conn, &self.host_id, &self.shared.client, &t).await {
            Ok(_) => {}
            Err(Splice::UnknownSession) => return self.no_session("unknown_session"),
            Err(Splice::TooOld(m)) => return End::Down(format!("craze unavailable: {m}")),
            Err(Splice::Refused(r)) => {
                // A refused connection is poisoned (PM "The hub splice").
                self.refusals += 1;
                if self.refusals >= t.connect_refusals {
                    return End::Down(format!("connect refused: {r}"));
                }
                return End::lost(false);
            }
            Err(Splice::Lost(_)) => return End::lost(false),
            // Another host answered: the row check's own verdict, below.
            Err(Splice::Fault(m)) => return End::Down(format!("protocol: {m}")),
        }
        let row = match list_one(conn, t.request).await {
            Ok(row) => row,
            Err(ListError::Empty) => return self.no_session("unknown_session"),
            Err(ListError::Fault(m)) => return End::Down(format!("protocol: {m}")),
            Err(ListError::Lost(_)) => return End::lost(false),
        };
        let plan = match (self.take_row(&row), plan) {
            // The host serves another session than the one this lane was
            // folding: a cursor from that one cannot be honoured.
            (Ok(true), Plan::Resume) => Plan::Reseed("server_reset:session_replaced".to_string()),
            (Ok(_), plan) => plan,
            (Err(m), _) => return End::Down(format!("protocol: {m}")),
        };
        self.attachment(conn, notes, plan).await
    }

    /// The host's own row: the session row this connection starts from, and
    /// the craze session id — learned when the lane does not know it, and
    /// RE-learned when the host serves another (`Ok(true)`).
    fn take_row(&mut self, row: &Value) -> Result<bool, String> {
        let (roster, info) = RosterRow::from_host_row(row)?;
        same_host(&info.host_id, &self.host_id)?;
        // The roster's own mapping (live, so not approximate), but the row's
        // own activity without its approval override: the fold's book is what
        // says whether this lane holds an open approval.
        let mut session = lane_session(&roster);
        if let Some(host) = &roster.row {
            session.activity = work_activity(host);
        }
        self.base_row = Some(session);
        self.presence = None;
        self.fold.clear_activity();
        let mut state = lock(&self.shared.state);
        let replaced = match &state.session_id {
            Some(held) => held != &info.session_id,
            None => false,
        };
        state.session_id = Some(info.session_id.clone());
        Ok(replaced)
    }

    async fn attachment(&mut self, conn: &Arc<Conn>, mut notes: Notifications, plan: Plan) -> End {
        let t = self.shared.timings;
        let sid = lock(&self.shared.state)
            .session_id
            .clone()
            .unwrap_or_default();
        let (mut offer_cursor, mut reason) = match plan {
            Plan::Resume => (true, "reconnect".to_string()),
            Plan::Reseed(r) => (false, r),
        };
        let mut after_omitted = false;
        let mut worked = false;
        loop {
            if self.tx.is_closed() {
                return End::Closed;
            }
            if self.attached_once {
                if self.reattaches >= t.reattaches {
                    return End::Down("re-attach bound".to_string());
                }
                self.reattaches += 1;
            }
            self.attached_once = true;
            let cursor = if offer_cursor {
                self.cursor.clone()
            } else {
                None
            };
            let params = AttachParams {
                session_id: sid.clone(),
                cursor: cursor.clone(),
            };
            let r = match conn
                .request(method::SESSION_ATTACH, &params, t.request)
                .await
            {
                // By value: the snapshot (up to 16 MiB) moves, never copied.
                Ok(v) => match serde_json::from_value::<AttachResult>(v) {
                    Ok(r) => r,
                    Err(e) => {
                        return fault(
                            conn,
                            &format!("an attach reply shed cannot read: {e}"),
                            worked,
                        )
                    }
                },
                Err(CallError::Refused(e)) => {
                    let start_failed = e.reason.as_deref() == Some(reason::START_FAILED);
                    match e.data_code.as_deref() {
                        Some(code::UNKNOWN_SESSION) => return self.no_session("unknown_session"),
                        Some(code::NOT_ACCEPTING) if start_failed => {
                            let cause = start_failed_cause(e.cause.as_deref());
                            return End::Down(format!("start_failed: {cause}"));
                        }
                        // An attach to a closed engine (craze's own client
                        // reads it so).
                        Some(code::NOT_ACCEPTING) => return self.no_session("session_closed"),
                        // A gate refusal (not ready yet, busy, closing): the
                        // same attach again, counted.
                        Some(code::UNAVAILABLE | code::IN_PROGRESS) => {
                            tokio::time::sleep(jittered(t.backoff_base)).await;
                            continue;
                        }
                        _ => {
                            return fault(
                                conn,
                                &format!("craze refused session.attach: {e}"),
                                worked,
                            )
                        }
                    }
                }
                Err(_) => {
                    if after_omitted {
                        // The connection ended (or went quiet) under the
                        // re-attach an `omitted` asked for. That is how the
                        // session's closing `omitted` ends a connection (PM
                        // "Notifications") — and how a dropped transport
                        // does. Only the next connection's answer can tell
                        // them apart: redial, no cursor, and confirm.
                        self.confirm_closed = true;
                        return End::Lost {
                            worked,
                            reseed: Some(reason),
                        };
                    }
                    return End::lost(worked);
                }
            };
            worked = true;
            self.refusals = 0;
            // A session that answered an attach is not closed.
            self.confirm_closed = false;
            self.session_ready = r.ready;
            self.shared.set_live(
                Some(Live {
                    conn: Arc::clone(conn),
                    session_id: sid.clone(),
                    owner: self.owner,
                    cards: Cards::of(&r.session.capabilities),
                }),
                self.owner,
            );
            // The cursor honoured: the stream continues from it.
            if r.snapshot.is_none() && (cursor.is_none() || cursor.as_ref() != Some(&r.after)) {
                return fault(
                    conn,
                    "an attach reply with no snapshot that does not continue from the cursor",
                    worked,
                );
            }
            // The fenced read (Amendment A11): attached first — so every
            // event after `after` is on its way to this attachment — then the
            // registry, then the barrier. The events up to the barrier fold
            // over the registry's approvals before the `Ready`.
            let (registry, fence) = match self.fenced_read(conn, &sid).await {
                Ok(read) => read,
                Err(end) => return end,
            };
            let seeding = if r.snapshot.is_some() {
                let why = match (&cursor, &r.reset) {
                    (Some(_), Some(refused)) => format!("cursor_lost:{refused}"),
                    (Some(_), None) => "cursor_lost".to_string(),
                    (None, _) => reason.clone(),
                };
                if let Err(end) = self.seed(conn, &r, why, &registry) {
                    return end;
                }
                true
            } else {
                if let Err(end) = self.resumed(&r, &registry) {
                    return end;
                }
                false
            };
            match self.follow(conn, &mut notes, &r, seeding, fence).await {
                Follow::Reattach {
                    cursor,
                    reason: why,
                    omitted,
                } => {
                    offer_cursor = cursor;
                    reason = why;
                    after_omitted = omitted;
                }
                Follow::End(end) => return end,
            }
        }
    }

    /// The fenced read (Amendment A11), after an attach: the engine's ask
    /// registry — `asks.list`, then `asks.get` for each listed id (an ask can
    /// end between the two; `adopt_registry` keeps only what is still open) —
    /// then `session.sync`, whose `seq` is the head every event of which is
    /// already queued to this attachment. Returns the records and that seq.
    async fn fenced_read(&self, conn: &Conn, sid: &str) -> Result<(Vec<AskRecord>, u64), End> {
        let t = self.shared.timings.request;
        let session = SessionParams {
            session_id: sid.to_string(),
        };
        let list: AsksListResult = match conn.request(method::ASKS_LIST, &session, t).await {
            Ok(v) => read(conn, v, "an asks.list")?,
            Err(CallError::Refused(e)) => {
                return Err(fault(conn, &format!("craze refused asks.list: {e}"), true))
            }
            Err(_) => return Err(End::lost(true)),
        };
        let mut records = Vec::with_capacity(list.asks.len());
        for ask in list.asks {
            let params = AskParams {
                session_id: sid.to_string(),
                ask_id: ask.id,
            };
            match conn.request(method::ASKS_GET, &params, t).await {
                Ok(v) => records.push(read::<AskGetResult>(conn, v, "an asks.get")?.ask),
                // Ended (and forgotten) since the list: nothing to adopt.
                Err(CallError::Refused(_)) => {}
                Err(_) => return Err(End::lost(true)),
            }
        }
        let synced: SyncResult = match conn.request(method::SESSION_SYNC, &session, t).await {
            Ok(v) => read(conn, v, "a session.sync")?,
            Err(CallError::Refused(e)) => {
                return Err(fault(
                    conn,
                    &format!("craze refused session.sync: {e}"),
                    true,
                ))
            }
            Err(_) => return Err(End::lost(true)),
        };
        Ok((records, synced.seq))
    }

    /// A reseed from an attach reply's snapshot: the old generation's open
    /// segment flushed, `Reset`, a fresh fold — the session's cards, the
    /// snapshot's rows, the REGISTRY's approvals — then `Session`,
    /// `Capabilities`, `Settings` (when they say so) and the approvals. The
    /// `Ready` waits for `synchronized` and the fence (`follow`).
    fn seed(
        &mut self,
        conn: &Conn,
        r: &AttachResult,
        reason: String,
        registry: &[AskRecord],
    ) -> Emitted {
        let Some(snapshot) = &r.snapshot else {
            return Ok(());
        };
        self.fold.flush();
        self.drain_rows()?;
        self.generation += 1;
        self.seeded = false;
        // The reads stop answering from a fold being rebuilt — THIS watcher's,
        // if they answered from it. Another watcher's seeded fold is no less
        // current for this one reseeding: compare-and-clear, as the end does.
        self.shared.unseed(self.owner);
        self.emit(LaneEvent::Reset {
            reason,
            generation: self.generation,
        })?;
        let mut fold = CrazeFold::new(&self.host_id);
        fold.set_cards(Cards::of(&r.session.capabilities));
        let restored = match fold.restore(snapshot) {
            Ok(r) => r,
            Err(e) => {
                return Err(fault(
                    conn,
                    &format!("a snapshot shed cannot fold: {e}"),
                    true,
                ))
            }
        };
        if restored.seq != r.after.seq || restored.incarnation != r.after.incarnation {
            return Err(fault(
                conn,
                &format!(
                    "an attach reply whose snapshot is cut at {}:{} but continues from {}:{}",
                    restored.incarnation, restored.seq, r.after.incarnation, r.after.seq
                ),
                true,
            ));
        }
        // The registry is the approval membership; the snapshot's asks are
        // its transcript's rows.
        fold.adopt_registry(registry);
        // A seed must fit the client channel (the contract's correction 13):
        // its rows are capped at what the ring keeps.
        fold.cap_pending(SEED_ROWS);
        self.fold = fold;
        // A confirmation queued before this snapshot was cut is in it already.
        while !matches!(
            self.confirms.try_recv(),
            Err(tokio::sync::broadcast::error::TryRecvError::Empty
                | tokio::sync::broadcast::error::TryRecvError::Closed)
        ) {}
        let _ = self.fold.take_approvals_changed();
        self.adopt_info(r.session.clone());
        self.cursor = Some(r.after.clone());
        self.emitted_approvals.clear();
        self.last_session = None;
        self.last_caps = None;
        self.last_settings = None;
        self.drain_rows()?;
        self.emit_session()?;
        self.emit_capabilities()?;
        self.emit_settings()?;
        self.emit_approvals()
    }

    /// A cursor honoured: the fold keeps everything; the info document (and
    /// the row read before the attach) may say something new.
    fn resumed(&mut self, r: &AttachResult, registry: &[AskRecord]) -> Emitted {
        self.adopt_info(r.session.clone());
        // The registry re-read (Amendment A11): an ask opened while away,
        // and one resolved while away, as the engine holds them now.
        self.fold.adopt_registry(registry);
        self.emit_session()?;
        if self.fold.take_approvals_changed() {
            self.emit_approvals()?;
        }
        self.emit_capabilities()?;
        self.emit_settings()
    }

    /// An info document — an attach reply's, or a `ready` notification's
    /// final one — adopted: the fold's catalogs, and what the capabilities and
    /// the session row read next (each caller re-emits them).
    fn adopt_info(&mut self, info: SessionInfo) {
        self.fold.apply_info(&info);
        self.info = Some(info);
    }

    /// One attachment's notifications, until it is reset or the connection
    /// ends; the flush clock folded into the wait.
    async fn follow(
        &mut self,
        conn: &Conn,
        notes: &mut Notifications,
        r: &AttachResult,
        seeding: bool,
        fence: u64,
    ) -> Follow {
        let flush_after = self.shared.timings.flush_after;
        let sync_wait = self.shared.timings.sync_wait;
        let mut at = Attachment {
            last: r.after.seq,
            synced: false,
            seeding,
            fence,
        };
        // Until the attachment has settled (its `synchronized`, and every
        // event through the fence), it must keep saying something (the module
        // doc's table): each notification moves this on.
        let mut sync_by = Some(Instant::now() + sync_wait);
        loop {
            if self.tx.is_closed() {
                return Follow::End(End::Closed);
            }
            let deadline = self.flush.deadline(flush_after);
            let note = tokio::select! {
                n = notes.recv() => n,
                c = self.confirms.recv() => {
                    // A lagged receiver lost only confirmations the stream's
                    // next `Settings` restates anyway.
                    if let Ok(c) = c {
                        if let Err(end) = self.confirmed(&c) {
                            return Follow::End(end);
                        }
                    }
                    continue;
                }
                () = sleep_until(deadline), if deadline.is_some() => {
                    self.fold.flush();
                    if let Err(end) = self.drain_rows() {
                        return Follow::End(end);
                    }
                    continue;
                }
                () = sleep_until(sync_by), if sync_by.is_some() => {
                    // Attached, then silent: a dead connection, not a seed
                    // still coming. Redial by the cursor rules — a seed that
                    // never reached its `Ready` reseeds, a resume resumes.
                    conn.close(&format!(
                        "the attachment said nothing for {sync_wait:?} before it settled"
                    ));
                    return Follow::End(End::lost(false));
                }
            };
            let Some(note) = note else {
                return Follow::End(match conn.ended() {
                    Some(ConnEnd::Backlog) => End::Lagged,
                    _ => End::lost(true),
                });
            };
            let next = self.on_note(conn, note, r, &mut at);
            sync_by = (!at.settled()).then(|| Instant::now() + sync_wait);
            match next {
                Ok(None) => {}
                Ok(Some(next)) => return next,
                Err(end) => return Follow::End(end),
            }
        }
    }

    /// The `Ready` an attachment owes, once it has settled ([`Attachment`]):
    /// a seed's `Ready` of its new generation, or a silent resume's lone
    /// `Ready` of the same one (the row re-read before the attach first —
    /// presence and activity are not journalled). Said once.
    fn settle(&mut self, at: &mut Attachment) -> Emitted {
        if !at.settled() {
            return Ok(());
        }
        if at.seeding {
            at.seeding = false;
            self.emit(LaneEvent::Ready {
                generation: self.generation,
            })?;
            self.seeded = true;
            self.stale = false;
            lock(&self.shared.state).seeded_by = Some(self.owner);
        } else if self.stale {
            self.emit_session()?;
            self.restate_settings()?;
            self.emit(LaneEvent::Ready {
                generation: self.generation,
            })?;
            self.stale = false;
        }
        Ok(())
    }

    /// A change craze confirmed at `rev: 0` (`LaneShared::confirmed`): no
    /// `meta` delta will carry it, so it is folded here and `Settings`
    /// re-emitted — a client never keeps the old value for want of an event.
    fn confirmed(&mut self, s: &crate::wire::Setting) -> Emitted {
        if self.fold.apply_confirmed(s) {
            self.emit_capabilities()?;
            self.emit_settings()?;
        }
        Ok(())
    }

    /// A silent resume's own `Settings`, said whether or not they changed
    /// (plan 025 §3.10): a `set` in flight at the loss was answered "outcome
    /// unknown" and is never resent, and a client shows that change "not
    /// confirmed" until the NEXT `Settings` states what the session is at.
    /// Said once the resume has folded everything through its fence — the
    /// lost change's own `meta` delta among it, if it ran — so it is the real
    /// value, not the one from before the outage. Only on a session whose
    /// capabilities say `settings`, as every `Settings`.
    fn restate_settings(&mut self) -> Emitted {
        self.last_settings = None;
        self.emit_settings()
    }

    /// One notification of the attachment `r`. `Ok(Some(_))` ends the
    /// attachment; `Err` is an emit that lagged, or a fault.
    fn on_note(
        &mut self,
        conn: &Conn,
        note: Notification,
        r: &AttachResult,
        at: &mut Attachment,
    ) -> Result<Option<Follow>, End> {
        let sub = r.subscription.as_str();
        let wrong_sub = |theirs: &str, what: &str| {
            Err(fault(
                conn,
                &format!("{what} for subscription {theirs:?}, not this attachment's {sub:?}"),
                true,
            ))
        };
        match note.method.as_str() {
            notify::EVENT => {
                let p: EventParams = read(conn, note.params, "an event")?;
                if p.subscription != sub {
                    return wrong_sub(&p.subscription, "an event");
                }
                if p.seq != at.last + 1 {
                    return Err(fault(
                        conn,
                        &format!(
                            "event seq {} after {}: a {}",
                            p.seq,
                            at.last,
                            if p.seq <= at.last {
                                "duplicate"
                            } else {
                                "hole"
                            }
                        ),
                        true,
                    ));
                }
                at.last = p.seq;
                self.fold.apply(&p.event);
                self.cursor = Some(Cursor {
                    incarnation: r.after.incarnation.clone(),
                    seq: p.seq,
                });
                self.after_fold()?;
                self.settle(at)?;
            }
            notify::SYNCHRONIZED => {
                let p: SyncParams = read(conn, note.params, "a synchronized")?;
                if p.subscription != sub {
                    return wrong_sub(&p.subscription, "a synchronized");
                }
                if at.synced {
                    return Err(fault(conn, "a second synchronized", true));
                }
                if p.seq != at.last {
                    return Err(fault(
                        conn,
                        &format!(
                            "synchronized at {} while the last event held is {}",
                            p.seq, at.last
                        ),
                        true,
                    ));
                }
                at.synced = true;
                self.reattaches = 0;
                // The episode is over: the outage it began with is too.
                self.outage = None;
                self.settle(at)?;
            }
            notify::READY => {
                let p: ReadyParams = read(conn, note.params, "a ready")?;
                if p.subscription != sub {
                    return wrong_sub(&p.subscription, "a ready");
                }
                if p.start_failed {
                    let cause = start_failed_cause(p.err.as_deref());
                    return Err(End::Down(format!("start_failed: {cause}")));
                }
                self.session_ready = true;
                self.adopt_info(p.session);
                // The final info document: capabilities (and so settings)
                // may have changed (§3.9).
                self.emit_capabilities()?;
                self.emit_settings()?;
            }
            notify::PRESENCE => {
                let p: PresenceParams = read(conn, note.params, "a presence")?;
                if p.subscription != sub {
                    return wrong_sub(&p.subscription, "a presence");
                }
                self.presence = Some(p.attached);
                self.emit_session()?;
            }
            notify::RESET => {
                let p: ResetParams = read(conn, note.params, "a reset")?;
                if p.subscription != sub {
                    return wrong_sub(&p.subscription, "a reset");
                }
                let tag = format!("server_reset:{}", p.reason);
                return Ok(Some(match p.reason.as_str() {
                    reset::SESSION_CLOSED => Follow::End(End::Down("session_closed".to_string())),
                    reset::SESSION_REPLACED => {
                        // The connection closes; the next one says `hello`
                        // afresh and re-learns the session id.
                        lock(&self.shared.state).session_id = None;
                        Follow::End(End::Lost {
                            worked: true,
                            reseed: Some(tag),
                        })
                    }
                    reset::SLOW_CONSUMER => Follow::Reattach {
                        // After readiness — the session's start AND this
                        // lane's seed — the cursor; before it, a snapshot.
                        cursor: self.seeded && !at.seeding && self.session_ready,
                        reason: tag,
                        omitted: false,
                    },
                    reset::OMITTED => Follow::Reattach {
                        cursor: false,
                        reason: tag,
                        omitted: true,
                    },
                    // `replay_failed`, and a reason protocol 1 does not name.
                    _ => Follow::Reattach {
                        cursor: false,
                        reason: tag,
                        omitted: false,
                    },
                }));
            }
            // A notification this build does not know (a roster's never
            // reaches a spliced connection).
            _ => {}
        }
        Ok(None)
    }
}

/// Where one attachment stands, for its `Ready`.
struct Attachment {
    /// The last event's seq (the attach reply's `after.seq` until one comes).
    last: u64,
    /// Its `synchronized` has come.
    synced: bool,
    /// It opened a generation (a seed) that has not reached its `Ready`.
    seeding: bool,
    /// The fenced read's `session.sync` seq: every event through it folds
    /// before the `Ready` (Amendment A11).
    fence: u64,
}

impl Attachment {
    /// The `Ready` may go: the `synchronized`, and the fence folded.
    fn settled(&self) -> bool {
        self.synced && self.last >= self.fence
    }
}

/// A protocol fault on the connection: it cannot be trusted further — close it
/// and reseed (`Reset{protocol}`) on the next.
fn fault(conn: &Conn, why: &str, worked: bool) -> End {
    conn.close(&format!("protocol: {why}"));
    End::Lost {
        worked,
        reseed: Some("protocol".to_string()),
    }
}

/// A notification's params, or a fault.
fn read<T: for<'de> Deserialize<'de>>(conn: &Conn, params: Value, what: &str) -> Result<T, End> {
    // By value: an event's body moves into its params, never copied.
    serde_json::from_value(params)
        .map_err(|e| fault(conn, &format!("{what} shed cannot read: {e}"), true))
}

/// Sleep until `deadline` (never called without one).
async fn sleep_until(deadline: Option<Instant>) {
    match deadline {
        Some(d) => tokio::time::sleep_until(d).await,
        None => std::future::pending().await,
    }
}
