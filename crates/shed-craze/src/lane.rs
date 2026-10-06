//! [`CrazeLane`] — one craze session as the contract's session-scoped
//! [`AgentLane`] (plan 025 §3.3.4, P11, P12).
//!
//! # Identity (pinned)
//!
//! - The lane's id — [`AgentLane::session_id`], `LaneSession::id` — is the
//!   row's **`hostId`**: the key the hub's roster is keyed by, and what
//!   `session.connect` is given (exact; never `ambiguous_session` should a
//!   session ever be re-hosted).
//! - The **craze `sessionId`** is lane-internal: every session-scoped call
//!   carries it, compared exactly by the host. It comes from the roster row
//!   when the source that opened the lane held one, and is (re-)learned from
//!   the host's own `sessions.list` on every connection — which also re-reads
//!   the row (presence and activity are not journalled) — so a host that
//!   swapped its engine while the lane was away (`session_replaced`) is
//!   followed to its new session rather than refused `unknown_session`.
//! - Every connection CHECKS the host it reached: the host's `hello` and its
//!   own `sessions.list` row must both name the lane's hostId — on the
//!   watcher's connection and on a read's own alike. Another host answering is
//!   a protocol fault (`Down{"protocol: …"}`, `Failed("protocol: …")`), never
//!   a session id, row or snapshot to use.
//! - `provider_session_id` is the roost fold key only; no call takes it.
//!
//! # One connection per open lane (D10)
//!
//! A subscription's watcher (`crate::watcher`) holds the lane's connection:
//! dial → hub `hello` → `session.connect{hostId}` with the host's `hello`
//! pipelined behind it → `sessions.list` → `session.attach`. **The verbs share
//! it** — `send`, `cancel`, `answer`, `set` and `stop` are requests on that
//! live connection, demuxed by id, each with a fresh commandId (counted from 1
//! per lane, never repeated) and the [`VERB_DEADLINE`]. Three cases, never
//! blurred:
//!
//! - **issued while disconnected**: waits up to [`WAIT_CONNECTED`] for the
//!   watcher's reconnect, then [`LaneError::Unavailable`] — it was never
//!   sent, so a retry is safe;
//! - **in flight when the connection drops**: fails at once, "outcome
//!   unknown" ([`crate::errors::outcome_unknown`]) — craze's own
//!   `resume_lost`/`disconnected` — and is NEVER resent: the transcript shows
//!   whether it ran;
//! - **no reply within its deadline**: "outcome unknown" too, AND the
//!   connection is closed ([`crate::conn::Conn::close`]) — a half-open
//!   transport shows up exactly here — so the watcher goes `Stale`, redials
//!   and offers its cursor.
//!
//! No token resume and no command resend in this plan (§9's future work).
//!
//! # Settings (plan 025 §3.10)
//!
//! [`AgentLane::set`] is `session.set{sessionId, commandId, setting}`:
//! `{kind: "model", value}`, `{kind: "mode", value}`, or `{kind: "config", id,
//! value, forModel}` — a config change BOUND to the model the option was chosen
//! for ([`setting_for`]), so craze refuses it `stale_model` (the table's
//! `NotAccepting`) rather than apply an option chosen for one model to
//! another. That model is the one the CLIENT displayed —
//! `LaneSettingChange::Config`'s `for_model` (Amendment A13), because the
//! lane's fold can have seen a move the person has not — and only when the
//! client sent none, the model the fold shows; a change issued before the lane
//! has any settings waits for them (up to the verb deadline, then
//! `Unavailable`, unsent) rather than going out unbound. The reply says
//! nothing the stream does not: the `meta` delta that carries the change
//! reaches the attachment BEFORE it (the reply barrier), and the watcher's
//! fold re-emits `Settings` from it — so the stream, not this call, is where a
//! client learns the new value. The one exception is a reply at `rev: 0` (craze
//! could learn no revision, and no event will follow): its confirmed value is
//! handed to every running watcher (`LaneShared::confirmed`), which applies it
//! to its fold and re-emits `Settings`. Every `set` is a fresh commandId,
//! so a retry after a refusal is a new command (craze stores none of the retry
//! codes, `stale_model` among them); one lost to a drop is "outcome unknown"
//! like every verb, and never resent — the watcher's next `Settings` (a silent
//! resume restates them before its `Ready`; a reseed seeds them) says whether
//! it took.
//!
//! # `session()` never dials
//!
//! Both clients call it before `subscribe()`. It answers the row the lane was
//! opened with (the source's roster row), refreshed by the watcher once one
//! runs; an id the source never listed is [`LaneError::UnknownSession`] at
//! once.
//!
//! # The reads
//!
//! `approvals()` and `settings()` answer from a running watcher's fold once
//! it has seeded (its `Ready`, until it reseeds or ends). Otherwise
//! `approvals()` reads the engine's ask REGISTRY (`asks.list`, then
//! `asks.get` per id: a sub-agent's ask, which no snapshot carries, included —
//! Amendment A11) and `history()` (always) `session.snapshot`, each on the
//! live connection when there is one, else on a short connection of its own;
//! `settings()` reads `sessions.list`'s catalogs and a snapshot's settings on
//! a short connection.
//! `history` ignores its cursor (craze has no paging; the module doc of
//! `shed_core::lane`), and its rows carry seqs local to the call.
//!
//! # The session's end
//!
//! A lane opened through a [`crate::CrazeSource`] holds that source's rows
//! WEAKLY and says its session's end back to them, so a row only a create
//! answered with — one no roster will ever remove — leaves with its session
//! (the source's module doc; live leg 1's ghost row). The end is the
//! watcher's terminal `Down` that says the SESSION is over — `session_closed`,
//! `unknown_session`, `start_failed: <cause>` — said to the source BEFORE the
//! `Down` is published, so a client that re-reads its listing on the `Down`
//! finds the row gone. A `Down` that says only that this lane cannot go on
//! (`unreachable`, `protocol: …`, `connect refused: …`, the bounds) says
//! nothing of the session, which may well be running — UNLESS the host took a
//! `session.stop` this lane sent: its receipt is craze's word that the
//! session's end follows (PM "`session.stop`"), so from then on the lane's
//! end, however it comes — any `Down`, or its subscription let go before the
//! close reached it — is the session's. That holds for the lane's LAST
//! running watcher only: while another subscription is still live, it is the
//! one to see the close (or to end in turn). The receipt alone lets nothing
//! go: the session is closing, its closing records still to come — unless no
//! subscription is left to see the close (one let go while the stop was in
//! flight, before its receipt): then the receipt itself says it, since nothing
//! else will. Said at most once per lane, whichever of the watcher's `Down`,
//! its end, or the receipt gets there first. A lane bound with no source
//! ([`CrazeLane::new`]) says it to nothing.
//!
//! **The order.** Said-before-`Down` holds for every end the watcher itself
//! sees. One window is accepted, not closed: the receipt is on the wire, but
//! a `Down` that says nothing of the session (`unreachable`, say) runs before
//! the `stop()` future wakes to record it — so that `Down` does not say the
//! end, its watcher goes, and the receipt, finding no watcher left, says it
//! just AFTER the `Down`. The row still goes and the client's hook
//! ([`crate::CrazeSource::on_created_gone`]) still fires; only the order is
//! lost, within one task wake.

use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde::Serialize;
use serde_json::{json, Map, Value};
use shed_core::lane::ring::MessageRing;
use shed_core::lane::{
    normalize_question_answer, AgentLane, LaneAnswer, LaneApproval, LaneApprovalKind,
    LaneCapabilities, LaneDecision, LaneError, LaneHistory, LaneSession, LaneSettingChange,
    LaneSettings, LaneSubscription, SendMode,
};
use tokio::sync::{broadcast, watch};

use crate::conn::{
    judge_hello_refusal, judge_host_hello, lock, CallError, Conn, HelloError, HostHello,
    Notifications, HELLO_DEADLINE,
};
use crate::dial::{connect_hub, CrazeDial, DialError, DIAL_DEADLINE};
use crate::errors::{craze_says, lane_error, outcome_unknown};
use crate::fold::{plan_option, record_approval, Cards, CrazeFold, Restored};
use crate::settings::{SettingsSections, SettingsState};
use crate::source::{SourceRows, KIND};
use crate::wire::{
    self, code, method, AskGetResult, AskParams, AsksListResult, ClientInfo, CommandParams,
    ConnectParams, Empty, HelloParams, PromptParams, RosterRow, SessionCapabilities, SessionInfo,
    SessionParams, SessionsListResult, SetParams, SetResult, Setting,
};

/// Every verb's deadline: the write and the reply (§3.3.4). The host answers
/// a `stop` receipt at once, and every other verb's reply waits only for the
/// event barrier.
pub const VERB_DEADLINE: Duration = Duration::from_secs(30);
/// How long a verb issued while disconnected waits for the watcher's
/// reconnect before it fails `Unavailable` (§3.3.4).
pub const WAIT_CONNECTED: Duration = Duration::from_secs(10);
/// The lane's reconnect floor (§3.3.5).
pub const BACKOFF_BASE: Duration = Duration::from_millis(200);
/// The lane's reconnect ceiling.
pub const BACKOFF_MAX: Duration = Duration::from_secs(30);
/// Dials give up after this long without a successful attach:
/// `Down{"unreachable"}` (§3.3.5's bound — a phone over cellular rides out a
/// minute's outage; craze's own "3 dials in 10 s" is deliberately not
/// shed's, plan 025 §4).
pub const GIVE_UP_AFTER: Duration = Duration::from_secs(10 * 60);
/// Attach attempts per episode, reset only at a `synchronized`
/// (`ReattachesPerEpisode`, PM "Re-attaching, and the per-episode bound").
pub const REATTACHES_PER_EPISODE: u32 = 8;
/// How long an attachment may go without a word — an event, or any other
/// notification of it — before its `synchronized` (PM: "Sent once per
/// attachment"): a host that answers `session.attach` and then says nothing
/// is a dead connection, not a seed that is still coming. Progress-based, so
/// a long replay that keeps delivering events is never cut short.
pub const SYNC_WAIT: Duration = Duration::from_secs(60);
/// Consecutive refused `session.connect`s (of any kind but
/// `unknown_session`, with no successful attach between them) that end the
/// lane rather than loop.
pub const CONNECT_REFUSALS: u32 = 5;

/// The lane's clocks and bounds — the defaults are the pinned ones; a test
/// shortens them (the bounds cells run on tokio's paused clock, never on
/// wall time).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LaneTimings {
    /// How long a dial may take to hand back a stream ([`DIAL_DEADLINE`]).
    pub dial: Duration,
    /// How long a `hello` may take ([`HELLO_DEADLINE`]).
    pub hello: Duration,
    /// [`VERB_DEADLINE`] — and every request the watcher makes.
    pub request: Duration,
    /// [`WAIT_CONNECTED`].
    pub wait_connected: Duration,
    /// [`BACKOFF_BASE`].
    pub backoff_base: Duration,
    /// [`BACKOFF_MAX`].
    pub backoff_max: Duration,
    /// [`GIVE_UP_AFTER`].
    pub give_up: Duration,
    /// [`SYNC_WAIT`].
    pub sync_wait: Duration,
    /// The segment flush clock ([`crate::segment::FLUSH_AFTER`]).
    pub flush_after: Duration,
    /// [`REATTACHES_PER_EPISODE`].
    pub reattaches: u32,
    /// [`CONNECT_REFUSALS`].
    pub connect_refusals: u32,
}

impl Default for LaneTimings {
    fn default() -> LaneTimings {
        LaneTimings {
            dial: DIAL_DEADLINE,
            hello: HELLO_DEADLINE,
            request: VERB_DEADLINE,
            wait_connected: WAIT_CONNECTED,
            backoff_base: BACKOFF_BASE,
            backoff_max: BACKOFF_MAX,
            give_up: GIVE_UP_AFTER,
            sync_wait: SYNC_WAIT,
            flush_after: crate::segment::FLUSH_AFTER,
            reattaches: REATTACHES_PER_EPISODE,
            connect_refusals: CONNECT_REFUSALS,
        }
    }
}

/// The capabilities a craze session states, as the contract's (§3.9):
/// `interject`, `cancel`, `stop` and `approvals` as the session says —
/// `approvals` is craze's own session capability (`true` on every protocol-1
/// host), never derived from the cards: a session with neither `askCards` nor
/// `planCards` still raises permissions, which are never hidden (Amendment A11
/// item 5), and a client must not suppress the only way to answer one —
/// `history_cursor` always (protocol 1), `settings` when it has a model, mode
/// or option to show.
pub fn lane_capabilities(caps: &SessionCapabilities, settings: bool) -> LaneCapabilities {
    LaneCapabilities {
        kind: KIND.to_string(),
        interject: caps.interject,
        cancel: caps.cancel,
        approvals: caps.approvals,
        history_cursor: true,
        settings,
        stop: caps.stop,
    }
}

/// The lane's live connection, as the watcher publishes it for the verbs.
#[derive(Clone)]
pub(crate) struct Live {
    pub conn: Arc<Conn>,
    /// The craze session id the connection attached with.
    pub session_id: String,
    /// Which watcher published it, so only that one takes it back.
    pub owner: u64,
    /// The ask cards the session exposes (its info document's capabilities).
    pub cards: Cards,
}

/// What the watcher has learned, for the calls that never dial.
#[derive(Default)]
pub(crate) struct LaneState {
    /// The craze session id: the roster row's, else learned from the host.
    pub session_id: Option<String>,
    /// The latest session row a watcher emitted — until one has, the row the
    /// source held when it opened the lane.
    pub session: Option<LaneSession>,
    pub capabilities: Option<LaneCapabilities>,
    pub settings: Option<LaneSettings>,
    /// Every approval the fold holds, whatever its status.
    pub approvals: Vec<LaneApproval>,
    /// The watcher whose fold the reads answer from: set at its `Ready`,
    /// cleared when it reseeds and when it ends — by that watcher alone, so
    /// another subscription reseeding or ending never unseeds this one's.
    pub seeded_by: Option<u64>,
}

/// The half of a lane its watchers share with its verbs.
pub(crate) struct LaneShared {
    pub host_id: String,
    pub dial: Arc<dyn CrazeDial>,
    pub client: ClientInfo,
    pub timings: LaneTimings,
    pub state: Mutex<LaneState>,
    pub live: watch::Sender<Option<Live>>,
    /// `true` once the lane KNOWS its settings — a watcher stored the
    /// session's `Settings`, or its capabilities say it has none — so a config
    /// change the client sent no model with can be bound to the folded one
    /// (Amendment A13: it waits for them rather than going out unbound).
    pub settings_known: watch::Sender<bool>,
    /// A change craze confirmed at `rev: 0` — no `meta` delta will carry it —
    /// for every running watcher to apply to its fold and re-emit `Settings`
    /// with (the module doc, "Settings").
    pub confirmed: broadcast::Sender<Setting>,
    /// The rows of the source the lane was opened through, held weakly — what
    /// the session's end is said to (the module doc).
    source: SourceRows,
    /// The host took a `session.stop` this lane sent (its receipt): the
    /// session's end follows, so the lane's own end is the session's.
    stop_taken: AtomicBool,
    /// The session's end has been said (at most once per lane).
    end_said: AtomicBool,
    /// The watchers running — counted from their spawn to their guard's drop,
    /// an abort included — so a stop receipted after the last one went knows
    /// that nothing is left to see the close.
    watchers: AtomicUsize,
    next_command: AtomicU64,
    next_watcher: AtomicU64,
}

/// How many `rev: 0` confirmations a watcher may have unread at once — far
/// more than a person can press while a watcher is between connections.
const CONFIRMED_BACKLOG: usize = 64;

impl LaneShared {
    /// A fresh commandId: a canonical positive decimal, counted from 1 per
    /// lane, never reused.
    pub fn command_id(&self) -> String {
        (self.next_command.fetch_add(1, Ordering::Relaxed) + 1).to_string()
    }

    /// The session ended for good, as this lane saw it (the module doc's
    /// "The session's end"): the source it was opened through lets its hostId
    /// go. Said ONCE per lane — whichever path gets here first; nothing for a
    /// lane bound with no source.
    pub fn session_ended(&self) {
        if !self.end_said.swap(true, Ordering::SeqCst) {
            self.source.session_ended(&self.host_id);
        }
    }

    /// Whether the host took a `session.stop` this lane sent — from then on
    /// the lane's end, however it comes, is the session's.
    pub fn stop_taken(&self) -> bool {
        self.stop_taken.load(Ordering::SeqCst)
    }

    /// A watcher began (counted from its spawn, before its task first runs).
    pub fn watcher_began(&self) {
        self.watchers.fetch_add(1, Ordering::SeqCst);
    }

    /// A watcher went — however it ended, an abort included. After a stop the
    /// host took, the LAST watcher's end is the session's (the module doc);
    /// one that leaves another running leaves the close to that one.
    ///
    /// Paired with [`LaneShared::took_stop`] in sequentially consistent order
    /// — each writes its own flag, then reads the other's — so of the last
    /// watcher going and a receipt arriving at once, at least one sees the
    /// other and says the end (both may; it is said once).
    pub fn watcher_gone(&self) {
        let last = self.watchers.fetch_sub(1, Ordering::SeqCst) == 1;
        if last && self.stop_taken() {
            self.session_ended();
        }
    }

    /// Whether the calling watcher is the only one running — so its end,
    /// after a stop the host took, leaves nothing to see the close.
    pub fn sole_watcher(&self) -> bool {
        self.watchers.load(Ordering::SeqCst) == 1
    }

    /// The host took this lane's `session.stop` (its receipt). With a watcher
    /// running, the close it will see — or its own end — says the session's
    /// end; with none left (the subscription let go while the stop was in
    /// flight), nothing would, so the receipt says it now.
    fn took_stop(&self) {
        self.stop_taken.store(true, Ordering::SeqCst);
        if self.watchers.load(Ordering::SeqCst) == 0 {
            self.session_ended();
        }
    }

    /// A watcher's id.
    pub fn watcher_id(&self) -> u64 {
        self.next_watcher.fetch_add(1, Ordering::Relaxed) + 1
    }

    /// Publish (or take back) a watcher's live connection. A watcher takes
    /// back only its own.
    pub fn set_live(&self, live: Option<Live>, owner: u64) {
        match live {
            Some(live) => {
                self.live.send_replace(Some(live));
            }
            None => {
                self.live.send_if_modified(|slot| {
                    let mine = slot.as_ref().is_some_and(|held| held.owner == owner);
                    if mine {
                        *slot = None;
                    }
                    mine
                });
            }
        }
    }

    /// A watcher ended or began a reseed: the reads stop answering from its
    /// fold — only if it is still the one they answer from.
    pub fn unseed(&self, owner: u64) {
        let mut state = lock(&self.state);
        if state.seeded_by == Some(owner) {
            state.seeded_by = None;
        }
    }

    /// The live connection, waiting up to [`LaneTimings::wait_connected`] for
    /// one (§3.3.4's "issued while disconnected").
    async fn connected(&self, what: &str) -> Result<Live, LaneError> {
        let mut rx = self.live.subscribe();
        let wait = async move {
            rx.wait_for(|l| l.as_ref().is_some_and(|l| l.conn.ended().is_none()))
                .await
                .ok()
                .and_then(|l| l.clone())
        };
        match tokio::time::timeout(self.timings.wait_connected, wait).await {
            Ok(Some(live)) => Ok(live),
            _ => Err(LaneError::Unavailable(format!(
                "not connected to craze within {:?}; the {what} was not sent — try again",
                self.timings.wait_connected
            ))),
        }
    }

    /// One mutating verb on the live connection: the session id and a fresh
    /// commandId filled in by `params`, the reply mapped through craze's table
    /// (§3.3.7), an unknown outcome never resent (the module doc).
    async fn command<P: Serialize>(
        &self,
        what: &str,
        method: &str,
        params: impl FnOnce(String, String) -> P,
    ) -> Result<Value, LaneError> {
        let live = self.connected(what).await?;
        let p = params(live.session_id.clone(), self.command_id());
        self.call_on(&live.conn, method, &p).await
    }

    /// The model the lane's fold shows, for a config change the client sent
    /// no model with (Amendment A13) — once the lane knows its settings,
    /// waiting up to the verb deadline for them when a change races the
    /// lane's first seed, so it never goes out unbound while the session HAS
    /// a model. They not arriving in time is `Unavailable`: the change was not
    /// sent, so a retry is safe.
    async fn folded_model(&self) -> Result<Option<String>, LaneError> {
        let mut known = self.settings_known.subscribe();
        let arrived = tokio::time::timeout(self.timings.request, known.wait_for(|k| *k))
            .await
            .is_ok_and(|r| r.is_ok());
        if !arrived {
            return Err(LaneError::Unavailable(format!(
                "the session's settings did not arrive within {:?}; the change was not sent — try again",
                self.timings.request
            )));
        }
        Ok(lock(&self.state)
            .settings
            .as_ref()
            .and_then(|s| s.model.clone()))
    }

    /// A request on `conn`, mapped: a refusal through the table; a dropped
    /// connection or a passed deadline "outcome unknown" — and, past the
    /// deadline, the connection closed, so the watcher reconnects.
    async fn call_on<P: Serialize>(
        &self,
        conn: &Conn,
        method: &str,
        params: &P,
    ) -> Result<Value, LaneError> {
        match conn.request(method, params, self.timings.request).await {
            Ok(v) => Ok(v),
            Err(CallError::Refused(e)) => Err(lane_error(&e)),
            Err(CallError::Deadline(d)) => {
                conn.close(&format!(
                    "{method} had no answer within {d:?}: the connection is taken for dead"
                ));
                Err(outcome_unknown(&format!(
                    "craze did not answer {method} within {d:?}; check the transcript"
                )))
            }
            Err(CallError::Ended(_)) => Err(outcome_unknown(
                "the connection to craze dropped; check the transcript",
            )),
            Err(e @ CallError::TooLong { .. }) => Err(LaneError::BadRequest(e.to_string())),
            Err(e @ CallError::Encode(_)) => Err(LaneError::Failed(e.to_string())),
        }
    }

    /// A connection to read on — the live one when there is one (never
    /// waiting for it), else a short one of its own ([`open_connection`]) —
    /// with the session id and the cards it serves. The notifications are
    /// handed back so a short connection lives as long as its reads.
    async fn reader(&self) -> Result<Reader, LaneError> {
        let live = self.live.borrow().clone();
        if let Some(live) = live.filter(|l| l.conn.ended().is_none()) {
            return Ok(Reader {
                conn: live.conn,
                session_id: live.session_id,
                cards: live.cards,
                _notes: None,
            });
        }
        let (conn, notes, info) = self.open_connection().await?;
        Ok(Reader {
            conn,
            session_id: info.session_id,
            cards: Cards::of(&info.capabilities),
            _notes: Some(notes),
        })
    }

    /// `session.snapshot` through a fresh fold — `history()`'s rows, under
    /// the session's cards.
    async fn read_snapshot(&self) -> Result<(CrazeFold, Restored), LaneError> {
        let r = self.reader().await?;
        let v = self
            .call_on(
                &r.conn,
                method::SESSION_SNAPSHOT,
                &SessionParams {
                    session_id: r.session_id.clone(),
                },
            )
            .await?;
        let mut fold = CrazeFold::new(&self.host_id);
        fold.set_cards(r.cards);
        let restored = fold
            .restore(v.get("snapshot").unwrap_or(&Value::Null))
            .map_err(|e| {
                LaneError::Failed(format!("craze sent a snapshot shed cannot fold: {e}"))
            })?;
        Ok((fold, restored))
    }

    /// The engine's ask registry read now (`asks.list`, then `asks.get` for
    /// each listed id) as the open approvals — the reads that answer before
    /// a subscription has seeded (Amendment A11: a sub-agent's ask, which no
    /// snapshot carries, included; a hidden kind and an ask that ended
    /// between the two reads not).
    async fn read_registry(&self) -> Result<Vec<LaneApproval>, LaneError> {
        let r = self.reader().await?;
        let v = self
            .call_on(
                &r.conn,
                method::ASKS_LIST,
                &SessionParams {
                    session_id: r.session_id.clone(),
                },
            )
            .await?;
        let list: AsksListResult = serde_json::from_value(v).map_err(|e| {
            LaneError::Failed(format!(
                "craze answered asks.list with something shed cannot read: {e}"
            ))
        })?;
        let mut records = Vec::with_capacity(list.asks.len());
        for ask in list.asks {
            let params = AskParams {
                session_id: r.session_id.clone(),
                ask_id: ask.id,
            };
            match self.call_on(&r.conn, method::ASKS_GET, &params).await {
                Ok(v) => {
                    let got: AskGetResult = serde_json::from_value(v).map_err(|e| {
                        LaneError::Failed(format!(
                            "craze answered asks.get with something shed cannot read: {e}"
                        ))
                    })?;
                    records.push(got.ask);
                }
                // Ended (and forgotten) since the list.
                Err(LaneError::UnknownApproval) => {}
                Err(e) => return Err(e),
            }
        }
        let mut fold = CrazeFold::new(&self.host_id);
        fold.set_cards(r.cards);
        fold.adopt_registry(&records);
        Ok(fold.pending_approvals())
    }

    /// A connection to the session's host, not attached: dial, the hub's
    /// `hello`, the splice, the host's `hello`, and `sessions.list` — whose
    /// one row is the session to use (its id, and its info document).
    pub async fn open_connection(
        &self,
    ) -> Result<(Arc<Conn>, Notifications, SessionInfo), LaneError> {
        let (conn, notes, _hub) = connect_hub(
            &*self.dial,
            &self.client,
            self.timings.dial,
            self.timings.hello,
        )
        .await
        .map_err(DialError::into_lane_error)?;
        let conn = Arc::new(conn);
        splice(&conn, &self.host_id, &self.client, &self.timings)
            .await
            .map_err(Splice::into_lane_error)?;
        let row = list_one(&conn, self.timings.request)
            .await
            .map_err(|e| e.into_lane_error())?;
        let (_, info) = RosterRow::from_host_row(&row)
            .map_err(|e| LaneError::Failed(format!("craze's host answered with {e}")))?;
        // Never another host's session id, row or data for this lane.
        same_host(&info.host_id, &self.host_id)
            .map_err(|m| LaneError::Failed(format!("protocol: {m}")))?;
        lock(&self.state).session_id = Some(info.session_id.clone());
        Ok((conn, notes, info))
    }
}

/// A connection to read on, and what it serves ([`LaneShared::reader`]).
struct Reader {
    conn: Arc<Conn>,
    session_id: String,
    cards: Cards,
    /// A short connection's notifications, held for its life.
    _notes: Option<Notifications>,
}

/// How the splice — `session.connect` and the host's `hello` — went wrong.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Splice {
    /// No session on the machine has that host id: terminal.
    UnknownSession,
    /// The hub refused the connect for another reason (`code/reason`): the
    /// connection is poisoned; redial.
    Refused(String),
    /// The host is too old to fold (a codec this crate does not know).
    TooOld(String),
    /// The connection went away first, or something answered that is not a
    /// host.
    Lost(String),
    /// A host answered — ANOTHER one than the hostId the connect named: a
    /// protocol fault, never data to use for this lane.
    Fault(String),
}

impl Splice {
    pub fn into_lane_error(self) -> LaneError {
        match self {
            Splice::UnknownSession => LaneError::UnknownSession,
            Splice::Refused(m) | Splice::TooOld(m) => LaneError::Failed(craze_says(m)),
            Splice::Lost(m) => LaneError::Unavailable(m),
            Splice::Fault(m) => LaneError::Failed(format!("protocol: {m}")),
        }
    }
}

/// The host that answered is the one this lane is for, or why not.
pub(crate) fn same_host(answered: &str, wanted: &str) -> Result<(), String> {
    if answered == wanted {
        Ok(())
    } else {
        Err(format!(
            "the splice reached host {answered:?}, not {wanted:?}"
        ))
    }
}

/// `session.connect{sessionId: <hostId>}` with the host's `hello` pipelined
/// behind it (PM "The hub splice": bytes the client sent after the connect
/// reach the host first, in order). The connect is polled first, so it takes
/// the writer first; a refused connect returns at once, without waiting on the
/// `hello` the hub will never answer as a host.
pub(crate) async fn splice(
    conn: &Conn,
    host_id: &str,
    client: &ClientInfo,
    timings: &LaneTimings,
) -> Result<HostHello, Splice> {
    let connect_params = ConnectParams {
        session_id: host_id.to_string(),
    };
    let hello_params = HelloParams::new(client.clone());
    let connect = conn.request(method::SESSION_CONNECT, &connect_params, timings.request);
    let hello = conn.request(method::HELLO, &hello_params, timings.hello);
    tokio::pin!(connect, hello);
    let mut connected = false;
    let mut said = None;
    while !(connected && said.is_some()) {
        tokio::select! {
            biased;
            r = &mut connect, if !connected => match r {
                Ok(_) => connected = true,
                Err(CallError::Refused(e)) => {
                    return Err(if e.data_code.as_deref() == Some(code::UNKNOWN_SESSION) {
                        Splice::UnknownSession
                    } else {
                        Splice::Refused(format!(
                            "{}/{}",
                            e.data_code.as_deref().unwrap_or("?"),
                            e.reason.as_deref().unwrap_or("?")
                        ))
                    });
                }
                Err(e) => return Err(Splice::Lost(format!("session.connect: {e}"))),
            },
            r = &mut hello, if said.is_none() => said = Some(r),
        }
    }
    match said.expect("the loop ends with the hello answered") {
        Ok(v) => {
            let hello = judge_host_hello(&v).map_err(|e| match e {
                HelloError::TooOld(m) => Splice::TooOld(m),
                HelloError::Refused(m) | HelloError::NoAnswer(m) => Splice::Lost(m),
            })?;
            // The hub's lookup is exact, but the client checks what it got:
            // another host's hello is never this lane's host.
            same_host(&hello.host_id, host_id).map_err(Splice::Fault)?;
            Ok(hello)
        }
        Err(CallError::Refused(e)) => Err(match judge_hello_refusal(&e) {
            HelloError::TooOld(m) => Splice::TooOld(m),
            other => Splice::Lost(other.to_string()),
        }),
        Err(e) => Err(Splice::Lost(format!("the host's hello: {e}"))),
    }
}

/// How a host's `sessions.list` was not one row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum ListError {
    /// No row: the host serves no session (terminal, `unknown_session`).
    Empty,
    /// More than one row, or a row that does not read: a protocol fault.
    Fault(String),
    /// The request did not come back.
    Lost(String),
}

impl ListError {
    pub fn into_lane_error(self) -> LaneError {
        match self {
            ListError::Empty => LaneError::UnknownSession,
            ListError::Fault(m) => LaneError::Failed(format!("protocol: {m}")),
            ListError::Lost(m) => LaneError::Unavailable(m),
        }
    }
}

/// The host's own `sessions.list`: exactly one row.
pub(crate) async fn list_one(conn: &Conn, deadline: Duration) -> Result<Value, ListError> {
    let v = match conn
        .request(method::SESSIONS_LIST, &Empty {}, deadline)
        .await
    {
        Ok(v) => v,
        Err(CallError::Refused(e)) => {
            return Err(ListError::Fault(format!(
                "craze refused sessions.list: {e}"
            )))
        }
        Err(e) => return Err(ListError::Lost(format!("sessions.list: {e}"))),
    };
    let r: SessionsListResult = serde_json::from_value(v)
        .map_err(|e| ListError::Fault(format!("a sessions.list shed cannot read: {e}")))?;
    let mut rows = r.sessions.into_iter();
    match (rows.next(), rows.next()) {
        (None, _) => Err(ListError::Empty),
        (Some(row), None) => Ok(row),
        (Some(_), Some(_)) => Err(ListError::Fault(
            "the host listed more than one session".to_string(),
        )),
    }
}

/// One craze session, bound to its row's hostId (the module doc).
///
/// Cheap to clone; every clone is the same lane.
#[derive(Clone)]
pub struct CrazeLane {
    shared: Arc<LaneShared>,
}

impl std::fmt::Debug for CrazeLane {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CrazeLane")
            .field("host_id", &self.shared.host_id)
            .field("timings", &self.shared.timings)
            .finish_non_exhaustive()
    }
}

impl CrazeLane {
    /// A lane on the session whose host is `host_id`, reached through `dial`.
    /// `row` is the roster row and craze session id a source holding the row
    /// opened it with — `None` when it never listed one, and then
    /// [`AgentLane::session`] is `UnknownSession` until a watcher reads the
    /// row. Binding, not dialling: nothing happens until a call.
    ///
    /// Bound to no source: its session's end is said to nothing (the module
    /// doc) — [`crate::CrazeSource`]'s `open` is what binds one that says it.
    pub fn new(
        dial: Arc<dyn CrazeDial>,
        client: ClientInfo,
        host_id: &str,
        row: Option<(LaneSession, String)>,
        timings: LaneTimings,
    ) -> CrazeLane {
        CrazeLane::opened_through(dial, client, host_id, row, timings, SourceRows::default())
    }

    /// [`CrazeLane::new`], opened through the source whose rows `source`
    /// names: the session's end is said back to them (the module doc).
    pub(crate) fn opened_through(
        dial: Arc<dyn CrazeDial>,
        client: ClientInfo,
        host_id: &str,
        row: Option<(LaneSession, String)>,
        timings: LaneTimings,
        source: SourceRows,
    ) -> CrazeLane {
        let (session, session_id) = match row {
            Some((session, sid)) => (Some(session), Some(sid)),
            None => (None, None),
        };
        let (live, _) = watch::channel(None);
        CrazeLane {
            shared: Arc::new(LaneShared {
                host_id: host_id.to_string(),
                dial,
                client,
                timings,
                state: Mutex::new(LaneState {
                    session_id,
                    session,
                    ..LaneState::default()
                }),
                live,
                settings_known: watch::channel(false).0,
                confirmed: broadcast::channel(CONFIRMED_BACKLOG).0,
                source,
                stop_taken: AtomicBool::new(false),
                end_said: AtomicBool::new(false),
                watchers: AtomicUsize::new(0),
                next_command: AtomicU64::new(0),
                next_watcher: AtomicU64::new(0),
            }),
        }
    }

    /// The craze session id the lane currently knows (the roster row's, or
    /// the one it learned) — what every session call carries.
    pub fn craze_session_id(&self) -> Option<String> {
        lock(&self.shared.state).session_id.clone()
    }

    /// How many of this lane's watchers are running (a test's sight of a
    /// subscription let go).
    #[cfg(test)]
    pub(crate) fn watching(&self) -> usize {
        self.shared.watchers.load(Ordering::SeqCst)
    }
}

/// The wire setting for `change` (plan 025 §3.10): a model or a mode by its
/// id, and nothing else; a config option by its id and value, BOUND
/// (`forModel`) to the model the CLIENT displayed it for — the change's own
/// `for_model` (Amendment A13) — and only when the client sent none, to
/// `folded`, the model the lane's fold shows. Unbound only when neither is
/// known. Pure, so the table is a test.
pub fn setting_for(change: &LaneSettingChange, folded: Option<&str>) -> Setting {
    match change {
        LaneSettingChange::Model { id } => Setting {
            kind: "model",
            id: None,
            value: id.clone(),
            for_model: None,
        },
        LaneSettingChange::Mode { id } => Setting {
            kind: "mode",
            id: None,
            value: id.clone(),
            for_model: None,
        },
        LaneSettingChange::Config {
            id,
            value,
            for_model,
        } => Setting {
            kind: "config",
            id: Some(id.clone()),
            value: value.clone(),
            for_model: [for_model.as_deref(), folded]
                .into_iter()
                .flatten()
                .find(|m| !m.is_empty())
                .map(str::to_string),
        },
    }
}

/// The body of `asks.answer` for `answer` to `approval` (§3.3.6, "Answers").
/// Pure, so the table is a test:
///
/// - **Permission**: `Choice{option_id}` → `{optionId}` (an id not offered is
///   `BadRequest`); `Permission{decision}` → the one option of that kind
///   ([`LaneApproval::option_for`]; none or several is `BadRequest` naming
///   craze); `Reject` → `{cancel: true}`.
/// - **Question**: `Question{answers, custom_text}` → `{answers: {<each
///   question's own id>: [<option id>…]}}`, position i of `answers` being
///   question i, each value matched to an offered option by id, then by label
///   (exactly one match, else `BadRequest`); any free text is `BadRequest`
///   (craze questions take none); zero questions → `{}`; `Reject` → `{skip:
///   true}`.
/// - **Plan**: `Choice{"accept"}`/`Permission{AllowOnce}` → `{accept: true}`;
///   `Choice{"reject"}`/`Permission{Reject}`/`Reject` → `{reject: true}`.
/// - `Raw{json}`, any kind: the object verbatim.
///
/// Anything else is `BadRequest`.
pub fn answer_body(approval: &LaneApproval, answer: &LaneAnswer) -> Result<Value, LaneError> {
    if let LaneAnswer::Raw { json } = answer {
        return match serde_json::from_str::<Value>(json) {
            Ok(v @ Value::Object(_)) => Ok(v),
            Ok(_) | Err(_) => Err(LaneError::BadRequest(
                "a raw answer must be one JSON object".to_string(),
            )),
        };
    }
    let wrong = || {
        LaneError::BadRequest(format!(
            "craze's {} approval takes no {} answer",
            approval.kind.as_str(),
            answer_name(answer)
        ))
    };
    match approval.kind {
        LaneApprovalKind::Permission => match answer {
            LaneAnswer::Choice { option_id } => {
                if approval.options.iter().any(|o| &o.id == option_id) {
                    Ok(json!({ "optionId": option_id }))
                } else {
                    Err(LaneError::BadRequest(format!(
                        "craze did not offer option {option_id:?} on this permission"
                    )))
                }
            }
            LaneAnswer::Permission { decision } => match approval.option_for(*decision) {
                Some(o) => Ok(json!({ "optionId": o.id })),
                None => Err(LaneError::BadRequest(format!(
                    "craze offered no single {} option on this permission; answer with the option's id",
                    decision_name(*decision)
                ))),
            },
            LaneAnswer::Reject => Ok(json!({ "cancel": true })),
            _ => Err(wrong()),
        },
        LaneApprovalKind::Question => match answer {
            LaneAnswer::Question {
                answers,
                custom_text,
            } => question_body(approval, answers, custom_text),
            LaneAnswer::Reject => Ok(json!({ "skip": true })),
            _ => Err(wrong()),
        },
        LaneApprovalKind::PlanApproval => match answer {
            LaneAnswer::Choice { option_id } if option_id == plan_option::ACCEPT => {
                Ok(json!({ "accept": true }))
            }
            LaneAnswer::Choice { option_id } if option_id == plan_option::REJECT => {
                Ok(json!({ "reject": true }))
            }
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce,
            } => Ok(json!({ "accept": true })),
            LaneAnswer::Permission {
                decision: LaneDecision::Reject,
            }
            | LaneAnswer::Reject => Ok(json!({ "reject": true })),
            LaneAnswer::Choice { option_id } => Err(LaneError::BadRequest(format!(
                "a craze plan offers only \"accept\" and \"reject\", not {option_id:?}"
            ))),
            _ => Err(wrong()),
        },
        _ => Err(wrong()),
    }
}

/// A question's positional answers as craze's `{answers: {<id>: [<option
/// id>…]}}` (or `{}` for an ask with no questions).
fn question_body(
    approval: &LaneApproval,
    answers: &[Vec<String>],
    custom_text: &[Option<String>],
) -> Result<Value, LaneError> {
    // The contract's one reader of the pair: too many answers, and free text
    // where a question takes none (every craze question), refuse here.
    let replies = normalize_question_answer(&approval.questions, answers, custom_text)?;
    if approval.questions.is_empty() {
        return Ok(json!({}));
    }
    let mut map = Map::new();
    for (i, (q, reply)) in approval.questions.iter().zip(replies).enumerate() {
        let mut picked = Vec::with_capacity(reply.labels.len());
        for value in &reply.labels {
            let by_id: Vec<_> = q.options.iter().filter(|o| &o.id == value).collect();
            let chosen = match by_id.as_slice() {
                [one] => one,
                [] => {
                    let by_label: Vec<_> = q.options.iter().filter(|o| &o.label == value).collect();
                    match by_label.as_slice() {
                        [one] => *one,
                        [] => {
                            return Err(LaneError::BadRequest(format!(
                                "question {i} offers no option {value:?}"
                            )))
                        }
                        _ => {
                            return Err(LaneError::BadRequest(format!(
                                "question {i} offers several options labelled {value:?}; answer with the option's id"
                            )))
                        }
                    }
                }
                _ => {
                    return Err(LaneError::BadRequest(format!(
                        "question {i} offers several options with id {value:?}"
                    )))
                }
            };
            picked.push(Value::String(chosen.id.clone()));
        }
        let key = q.id.clone().unwrap_or_default();
        map.insert(key, Value::Array(picked));
    }
    Ok(json!({ "answers": Value::Object(map) }))
}

fn answer_name(a: &LaneAnswer) -> &'static str {
    match a {
        LaneAnswer::Permission { .. } => "permission",
        LaneAnswer::Choice { .. } => "choice",
        LaneAnswer::Question { .. } => "question",
        LaneAnswer::Reject => "reject",
        LaneAnswer::Raw { .. } => "raw",
    }
}

fn decision_name(d: LaneDecision) -> &'static str {
    match d {
        LaneDecision::AllowOnce => "allow_once",
        LaneDecision::AllowAlways => "allow_always",
        LaneDecision::Reject => "reject",
    }
}

#[async_trait::async_trait]
impl AgentLane for CrazeLane {
    fn session_id(&self) -> &str {
        &self.shared.host_id
    }

    /// Never dials (the module doc).
    async fn session(&self) -> Result<LaneSession, LaneError> {
        let state = lock(&self.shared.state);
        state.session.clone().ok_or(LaneError::UnknownSession)
    }

    /// `session.snapshot` folded into rows through a fresh fold and ring; the
    /// cursor is ignored (craze has no paging), and `truncated` says the
    /// snapshot's window was cut or the page was.
    async fn history(&self, _cursor: Option<&str>, limit: u32) -> Result<LaneHistory, LaneError> {
        let (mut fold, restored) = self.shared.read_snapshot().await?;
        // A page has no later chunk to wait for: the open run is a row now.
        fold.flush();
        let mut ring = MessageRing::new();
        let now = now_unix_ms();
        for row in fold.drain() {
            ring.append(row, now);
        }
        let (messages, cut) = ring.page(limit);
        Ok(LaneHistory {
            messages,
            truncated: cut || restored.truncated,
            cursor: None,
        })
    }

    /// Starts the watcher and returns at once: a session that cannot be
    /// reached is the stream's `Stale` (and, past the bounds, its `Down`),
    /// never this call's error. A `cursor` is not honoured — this lane resumes
    /// from a cursor it keeps for itself — and the first `Reset` says so
    /// (`connect:cursor-ignored`).
    async fn subscribe(&self, cursor: Option<String>) -> Result<LaneSubscription, LaneError> {
        Ok(crate::watcher::spawn(Arc::clone(&self.shared), cursor))
    }

    async fn send(&self, text: &str, mode: SendMode) -> Result<(), LaneError> {
        let mode = match mode {
            SendMode::Queue => "queue",
            SendMode::Interject => {
                // A session that says it cannot interject is refused here,
                // NotAccepting (SendMode's contract); one that can is asked,
                // and its refusal when idle (`not_in_turn`) maps the same.
                let caps = lock(&self.shared.state).capabilities.clone();
                if caps.is_some_and(|c| !c.interject) {
                    return Err(LaneError::NotAccepting);
                }
                "interject"
            }
        };
        let text = text.to_string();
        self.shared
            .command("prompt", method::SESSION_PROMPT, |sid, cmd| PromptParams {
                session_id: sid,
                command_id: cmd,
                text,
                mode,
            })
            .await
            .map(|_| ())
    }

    /// `session.cancel` with no `turnId` — the current turn, craze's own or
    /// the agent's (§3.3.7). A cancel with nothing to cancel is craze's
    /// `not_accepting`: `NotAccepting` (correction 8).
    async fn cancel(&self) -> Result<(), LaneError> {
        self.shared
            .command("cancel", method::SESSION_CANCEL, |sid, cmd| CommandParams {
                session_id: sid,
                command_id: cmd,
            })
            .await
            .map(|_| ())
    }

    /// Every open approval on the session — craze's ENGINE ask registry, a
    /// sub-agent's asks included (Amendment A11, `crate::fold`'s module doc):
    /// a running watcher's fold once it has seeded, else the registry read now
    /// (`asks.list`, then `asks.get` per id — `read_registry`).
    async fn approvals(&self) -> Result<Vec<LaneApproval>, LaneError> {
        {
            let state = lock(&self.shared.state);
            if state.seeded_by.is_some() {
                return Ok(state
                    .approvals
                    .iter()
                    .filter(|a| a.status.is_pending())
                    .cloned()
                    .collect());
            }
        }
        self.shared.read_registry().await
    }

    async fn answer(&self, approval_id: &str, answer: LaneAnswer) -> Result<(), LaneError> {
        let held = lock(&self.shared.state)
            .approvals
            .iter()
            .find(|a| a.id == approval_id)
            .cloned();
        let live = self.shared.connected("answer").await?;
        let approval = match held {
            Some(a) => a,
            // Not in the fold (raised before this lane saw it, or evicted):
            // read it whole.
            None => {
                let v = self
                    .shared
                    .call_on(
                        &live.conn,
                        method::ASKS_GET,
                        &AskParams {
                            session_id: live.session_id.clone(),
                            ask_id: approval_id.to_string(),
                        },
                    )
                    .await?;
                let r: AskGetResult = serde_json::from_value(v).map_err(|e| {
                    LaneError::Failed(format!(
                        "craze answered asks.get with something shed cannot read: {e}"
                    ))
                })?;
                record_approval(&self.shared.host_id, &r.ask)
                    .map(|(approval, _)| approval)
                    .filter(|a| !a.id.is_empty() && a.id == approval_id)
                    .ok_or_else(|| {
                        LaneError::Failed(format!(
                            "craze's ask {approval_id} is not one shed can answer (an automatic one, or a kind it does not know)"
                        ))
                    })?
            }
        };
        let body = answer_body(&approval, &answer)?;
        let params = wire::AnswerParams {
            session_id: live.session_id.clone(),
            command_id: self.shared.command_id(),
            ask_id: approval_id.to_string(),
            answer: body,
        };
        self.shared
            .call_on(&live.conn, method::ASKS_ANSWER, &params)
            .await
            .map(|_| ())
    }

    /// A running watcher's settings once it has seeded; else
    /// `sessions.list`'s catalogs and a snapshot's settings, read now. A
    /// session with nothing to show answers the default (its capabilities say
    /// `settings: false`).
    async fn settings(&self) -> Result<LaneSettings, LaneError> {
        {
            let state = lock(&self.shared.state);
            if state.seeded_by.is_some() {
                return Ok(state.settings.clone().unwrap_or_default());
            }
        }
        let (conn, _notes, info) = self.shared.open_connection().await?;
        let v = self
            .shared
            .call_on(
                &conn,
                method::SESSION_SNAPSHOT,
                &SessionParams {
                    session_id: info.session_id.clone(),
                },
            )
            .await?;
        let mut s = SettingsState::new();
        s.apply_info(&info);
        if let Some(settings) = v.get("snapshot").and_then(|s| s.get("settings")) {
            s.apply_sections(&SettingsSections::from_value(settings));
        }
        Ok(if s.has_any() {
            s.lane_settings()
        } else {
            LaneSettings::default()
        })
    }

    /// `session.set` on the live connection (the module doc, "Settings"): a
    /// config change bound to the model the client displayed (its
    /// `for_model`), else to the model the lane's fold shows — read when the
    /// call is made, waiting for the lane's first settings if none has
    /// arrived, so a change chosen on one model is never sent for another.
    /// `Ok` is craze's confirmation; the new value arrives on the stream (its
    /// `meta` delta is ahead of the reply) — or, answered `rev: 0`, from the
    /// confirmed value itself, which every running watcher applies. A refusal
    /// maps through the table (`stale_model` → `NotAccepting`); a reply lost
    /// to a drop is "outcome unknown", never resent.
    async fn set(&self, change: LaneSettingChange) -> Result<(), LaneError> {
        let folded = match &change {
            LaneSettingChange::Config {
                for_model: Some(m), ..
            } if !m.is_empty() => None,
            LaneSettingChange::Config { .. } => self.shared.folded_model().await?,
            LaneSettingChange::Model { .. } | LaneSettingChange::Mode { .. } => None,
        };
        let setting = setting_for(&change, folded.as_deref());
        let sent = setting.clone();
        let v = self
            .shared
            .command("set", method::SESSION_SET, |sid, cmd| SetParams {
                session_id: sid,
                command_id: cmd,
                setting: sent,
            })
            .await?;
        // `rev: 0`: confirmed, and no event will say so (wire::SetResult).
        if let Ok(r) = serde_json::from_value::<SetResult>(v) {
            if r.rev == Some(0) {
                let _ = self.shared.confirmed.send(Setting {
                    value: r.value,
                    for_model: None,
                    ..setting
                });
            }
        }
        Ok(())
    }

    /// `session.stop`: `Ok` is the host's RECEIPT, never the stop's completion
    /// (PM "`session.stop`"; Amendment A7): the session's closing records
    /// follow on the stream, then `reset{session_closed}`, which ends the
    /// subscription `Down{"session_closed"}`. A host that cannot stop (`stop:
    /// false`) answers `unsupported` — `Failed` (the table). A receipt makes
    /// the lane's own end, however it comes, the session's — and says it at
    /// once when no subscription is left to (the module doc's "The session's
    /// end").
    async fn stop(&self) -> Result<(), LaneError> {
        self.shared
            .command("stop", method::SESSION_STOP, |sid, cmd| CommandParams {
                session_id: sid,
                command_id: cmd,
            })
            .await?;
        self.shared.took_stop();
        Ok(())
    }
}

/// The wall clock in milliseconds, for the ring's stamp on a row that carries
/// no time of its own.
pub(crate) fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests;
