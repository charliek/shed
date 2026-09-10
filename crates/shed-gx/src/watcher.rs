//! The pump — one `GET …/events` connection per generation, and the **bounded
//! silent resume** that is the whole reason gx advertises `history_cursor`.
//!
//! # Two kinds of reconnect, and only one of them is a frame
//!
//! `shed_core::lane`'s correction 1 splits a job the `Reset` … `Ready` bracket
//! used to do twice:
//!
//! - a **reseed** rebuilds everything and says so. `Reset`, a fresh
//!   [`GxFold`], a fresh view; the client stages until `Ready` and swaps.
//! - a **silent resume** rebuilds nothing. gx accepts a `Last-Event-ID` and
//!   replays the gap, so the ring, the open streak, the generation number and
//!   the client's view all survive — and the client is never told, because
//!   there is nothing for it to do.
//!
//! A resume is attempted at most [`GxTimings::resume_tries`] times inside
//! [`GxTimings::resume_window`] of the first loss. Past that it is a reseed
//! named `cursor_lost`, and reseeds that keep failing for
//! [`GxTimings::down_after`] end the subscription with `Down`. That ladder — 3
//! silent, then `cursor_lost`, then `Down` — is what keeps a dead leader from
//! looking like a slow one forever.
//!
//! **Transport repair is not on this ladder at all.** Every connect goes
//! through [`GxClient::open_events`], which dials through
//! [`crate::transport::GxTransport`] first; a client that owns an `ssh -N -L`
//! forward re-`ensure`s it inside that hook, and the watcher never learns that
//! anything moved. That is the difference between "the tunnel was repaired" and
//! "your transcript is wrong", which the contract used to spell the same way.
//!
//! # One generation, in the pinned order
//!
//! 1. **A reseed only**: `Reset`, then [`GxFold::reset`]. A resume skips this
//!    entirely — no frame, no state change.
//! 2. **Open the stream FIRST** and buffer everything it says. Seeding before
//!    subscribing loses whatever is minted between the two reads, and it is
//!    exactly the interesting frames (a `turn_completed`, an approval) that
//!    land in that window.
//! 3. Fetch, while the stream keeps filling the inbox: the session row, the
//!    history tail (reseed only), and the open approvals.
//! 4. Fold the seed, then **reconcile**: `approval` and `session` frames carry
//!    no `id:` and are therefore NOT replayed by a cursor resume, so every
//!    reconnect — silent or not — re-reads them, and a held-pending approval
//!    the refetch no longer lists gets a `Resolved` tombstone.
//! 5. Drain the buffered frames through the fold, whose `seen` set makes the
//!    overlap between the seed page and the replay free.
//! 6. Publish: `Session`, then the `Approval`s, then (a reseed only) `Ready`.
//!
//! From there, frames apply as they arrive.
//!
//! # What ends a generation, and what ends the subscription
//!
//! An EOF, a stall ([`GxTimings::stall`] with no bytes at all — gx's keepalive
//! is a comment line every 15 s, so silence past that window is real), a
//! transport error, an oversized SSE frame, an inbox overflow, or a server
//! `reset` frame all end the GENERATION. Only four things end the
//! SUBSCRIPTION, and each is a state no amount of reconnecting improves:
//! `unknown_session`, `unauthorized`, a `session` frame saying the row was
//! removed, and [`GxTimings::down_after`] of failing reseeds.
//!
//! **A client that stops reading ends a generation too**, and it is NOT one of
//! the four. The frame channel is bounded
//! ([`shed_core::lane::LANE_CHANNEL_CAPACITY`]); a full one drops the frame, and
//! every emitting helper here propagates that as [`GenEnd::Lagged`], so the
//! generation ends at the FIRST dropped frame — in steady state, mid-seed, or
//! inside a reseed's own `Reset` … `Ready`. The run loop then unpins the epoch
//! (as it does after every generation), waits for the client to drain **holding
//! no stream**, takes the ordinary failure backoff, and reseeds as
//! `Plan::Reseed("lagged")` — never `Plan::Resume`, because the dropped frames
//! were folded at or before the cursor and a by-counter resume would ask for
//! everything AFTER them, leaving the hole permanent. **`"overflow"` and
//! `"lagged"` are different overflows and both names stay:** `"overflow"` is
//! THIS crate's seed inbox (frames it had not folded yet), `"lagged"` is the
//! client's channel (frames it had published).

use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant};

use futures_util::{Stream, StreamExt as _};
use serde_json::Value;

use shed_core::lane::backoff::{jittered, next_backoff};
use shed_core::lane::ring::MessageRing;
use shed_core::lane::{
    AgentLane, LaneApproval, LaneApprovalStatus, LaneError, LaneEvent, LanePublisher, LaneSession,
    LaneStop, LaneSubscription, Publish,
};
use shed_core::rc::RcActivity;
use shed_core::sse::{SseEvent, SseParser};

use crate::client::{lane_session, GxClient, GxSessionRow, GxTimings};
use crate::discovery::redact_hex64;
use crate::fold::{lane_approval, GxApprovalResource, GxEnvelope, GxFold};

// ---------------------------------------------------------------------------
// bounds
// ---------------------------------------------------------------------------

/// One accumulated SSE event's cap. Past it the read is an error — disconnect
/// and reconnect — rather than unbounded buffering for a `data:` field that
/// never terminates.
pub const MAX_SSE_FRAME_BYTES: usize = 4 << 20;

/// The inbox is bounded by BOTH item count and total bytes, and an overflow
/// forces a **reseed** rather than dropping the oldest frame.
///
/// Dropping the oldest is the tempting cheap answer and it is wrong here: the
/// dropped frame could be the `turn_completed` that closes a streak or the
/// `approval` that says the agent is blocked, and a fold that misses one of
/// those is wrong until the next reseed anyway. Forcing the reseed immediately
/// is the same repair, taken at the moment the gap is known rather than
/// whenever it happens to be noticed.
pub const MAX_INBOX_ITEMS: usize = 4096;
/// See [`MAX_INBOX_ITEMS`].
pub const MAX_INBOX_BYTES: usize = 4 << 20;

/// The reconnect backoff's floor and ceiling.
///
/// They are DERIVED from [`GxTimings::down_after`] rather than being two more
/// constructor fields, and the reason is that a test which scales the windows
/// must scale this curve too: a suite with `down_after` at 600 ms and a fixed
/// 100 ms floor would spend its whole ladder asleep and reach `Down` before it
/// reached `cursor_lost`. At the default 60 s `down_after` the pair evaluates
/// to exactly the intended 100 ms and 5 s.
fn backoff_bounds(timings: &GxTimings) -> (Duration, Duration) {
    let base = Duration::from_millis(100).min(timings.down_after / 20);
    let max = Duration::from_secs(5).min(timings.down_after / 4);
    // A zero floor would busy-loop a reconnect; a ceiling under the floor would
    // make `next_backoff` walk downwards.
    let base = base.max(Duration::from_millis(1));
    (base, max.max(base))
}

// ---------------------------------------------------------------------------
// spawn
// ---------------------------------------------------------------------------

/// Spawn the pump for one subscription and hand back the contract's receiver +
/// stop handle.
pub(crate) fn spawn(client: GxClient, session: &str, cursor: Option<String>) -> LaneSubscription {
    // **The channel is BOUNDED, and the bound is the contract's**
    // (`shed_core::lane::LANE_CHANNEL_CAPACITY`, module-doc correction 13) —
    // shared with `shed-opencode` and hand-mirrored by shed-mobile, so neither
    // the number nor the policy is an adapter's call to make. What this adapter
    // owes it is propagation: a dropped frame ends the generation, here as
    // everywhere else.
    //
    // Everything else this crate owns was already bounded — the fold's `seen`,
    // tools and approvals maps, the seed inbox
    // (`MAX_INBOX_ITEMS`/`MAX_INBOX_BYTES`), one SSE frame
    // (`MAX_SSE_FRAME_BYTES`), the ring, and each row's text. The client queue
    // was the one thing that was not, and a client that held a subscription open
    // and never polled it grew it without limit.
    let (tx, rx) = LanePublisher::channel();
    let timings = client.timings().clone();
    let watcher = Watcher {
        client,
        session: session.to_string(),
        tx,
        fold: GxFold::new(session),
        ring: MessageRing::new(),
        generation: 0,
        timings,
        emitted_approvals: HashMap::new(),
        last_session: None,
        session_row: None,
        fold_at: Instant::now(),
        row_at: Instant::now(),
        streak_at: None,
        streak_bytes: 0,
    };
    let task = tokio::spawn(watcher.run(cursor));
    LaneSubscription {
        rx,
        stop: LaneStop::new(task),
    }
}

/// Drops the client's transport pin when the pump goes away — **however it goes
/// away**.
///
/// `run` unpins after every generation, but [`LaneStop`] aborts the task, and an
/// abort while the pump is awaiting the SSE body simply drops the future: no
/// arm of the loop runs, and the shared [`GxClient`] keeps an epoch pinned to a
/// connection that has just been destroyed. The next connect would then skip
/// `healthz` entirely and send the token to whatever now answers on that
/// address — which over a re-established `ssh -N -L` forward is the one case
/// the `instanceId` pin exists to catch.
///
/// A guard is the only shape that survives cancellation, and it has to be
/// synchronous, which is why it asks rather than awaits
/// ([`GxClient::request_unpin`]).
struct PinGuard(GxClient);

impl Drop for PinGuard {
    fn drop(&mut self) {
        self.0.request_unpin();
    }
}

// ---------------------------------------------------------------------------
// the pump
// ---------------------------------------------------------------------------

struct Watcher {
    client: GxClient,
    /// The subscribed session. Every route this watcher touches names it, and
    /// the fold refuses an event id that does not carry it as a prefix.
    session: String,
    tx: LanePublisher,
    fold: GxFold,
    /// One ring for the life of the subscription: `seq` is monotonic ACROSS
    /// generations, so a client that keeps its rows across a reseed can still
    /// order them.
    ring: MessageRing,
    generation: u64,
    timings: GxTimings,
    /// The last DTO emitted per approval id, so an unchanged approval is not
    /// re-sent on every frame and a tombstone is sent exactly once.
    emitted_approvals: HashMap<String, LaneApproval>,
    last_session: Option<LaneSession>,
    /// gx's own roster row — title, cwd, activity, the pending count. `None`
    /// until the first fetch, and nothing is published before then: a defaulted
    /// row would claim `idle` and `approximate: false` about a session nobody
    /// has read yet.
    session_row: Option<GxSessionRow>,
    /// When the fold last saw a transcript frame, and when the roster row was
    /// last replaced. Both sources are live and they can only disagree about
    /// ORDER; see [`Watcher::emit_session`].
    fold_at: Instant,
    row_at: Instant,
    /// When the OPEN STREAK last grew, and how big it was then — the flush
    /// clock. `None` when no streak is open. See [`Watcher::note_streak`].
    streak_at: Option<Instant>,
    streak_bytes: usize,
}

/// What the next connect is.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Plan {
    /// Emit `Reset`, discard the fold, connect with no cursor, seed from
    /// history.
    Reseed(String),
    /// Say nothing, keep everything, connect with this `Last-Event-ID`.
    Resume(String),
}

/// `Ok` while the client is keeping up; `Err(GenEnd::Lagged)` the instant a
/// frame is dropped, which every caller propagates.
type Emitted = Result<(), GenEnd>;

/// Whether a frame's SESSION and APPROVAL effects reach the client as it is
/// applied. Transcript rows are unaffected — they are first in the pinned
/// order, so they emit either way.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Emit {
    /// Steady state.
    Now,
    /// The buffered replay (step 5). Folded, but silent: step 6 owns the
    /// `Session`/`Approval` frames, and emitting here would put an `Approval`
    /// ahead of the first `Session` — which step 6 could not repair, because
    /// both emitters deduplicate against what they already sent.
    Defer,
}

/// How a generation ended.
#[derive(Debug)]
enum GenEnd {
    /// Terminal: emit `Down` with this reason and stop.
    Down(String),
    /// The CLIENT stopped reading and a frame was dropped
    /// (`shed_core::lane::Publish::Lagged`).
    ///
    /// Its own variant rather than an [`GenEnd::Ended`] with
    /// `force_reseed: Some("lagged")`, because the run loop owes it two things
    /// an ordinary end does not. It **waits for the client to drain** before
    /// reseeding, and it does **not** advance the `down_after` clock: that clock
    /// spends the subscription on an agent that cannot be reached, and a
    /// consumer that is merely slow is not one. A lagged generation still takes
    /// the failure backoff, so consecutive lags cannot become a seed-per-frame
    /// load on a live leader.
    Lagged,
    Ended {
        /// Did this generation get as far as being USEFUL — a reseed that
        /// reached `Ready`, or a resume that reconciled? It resets the backoff.
        worked: bool,
        /// Was the connection up for at least [`GxTimings::resume_window`]?
        /// That clears the silent-resume streak: a stream that ran healthily
        /// for a whole window and then dropped is a NEW loss, not the fourth
        /// attempt at an old one.
        long_lived: bool,
        /// What to say if this turns out to be the last thing that happened.
        detail: String,
        /// Set when the next generation MUST be a reseed with this reason —
        /// a server `reset` frame, or an inbox overflow.
        force_reseed: Option<String>,
    },
}

impl Watcher {
    async fn run(mut self, cursor: Option<String>) {
        // Held for the whole pump. Every ordinary exit already unpins; this is
        // what covers the one that cannot — an abort mid-await.
        let _pin_guard = PinGuard(self.client.clone());

        // The caller's cursor is not honored (see `AgentLane::subscribe`'s doc
        // on `GxClient`), and the first `Reset` SAYS so rather than leaving a
        // client to believe it resumed.
        let mut plan = Plan::Reseed(
            if cursor.is_some() {
                "connect:cursor-ignored"
            } else {
                "connect"
            }
            .to_string(),
        );
        let (base, max) = backoff_bounds(&self.timings);
        let mut backoff = base;
        /// The silent-resume budget: when the current streak of losses began,
        /// and how many attempts it has spent.
        struct Streak {
            began: Instant,
            tries: u32,
        }
        let mut streak: Option<Streak> = None;
        // When reseeds started failing. `None` while one is reaching `Ready`.
        let mut failing_since: Option<Instant> = None;

        loop {
            if self.tx.is_closed() {
                return; // the subscription was dropped
            }
            let was_reseed = matches!(plan, Plan::Reseed(_));
            let end = self.run_generation(&plan).await;
            // Every generation ends its transport epoch, whatever ended it.
            // §0's invariant 2 is "every (re)connect opens a new epoch", and a
            // stream that EOFs never goes through the request path that would
            // otherwise notice.
            self.client.unpin().await;
            let (worked, long_lived, detail, force_reseed) = match end {
                GenEnd::Down(reason) => {
                    self.go_down(reason).await;
                    return;
                }
                // The generation already dropped its SSE stream and the `unpin`
                // above already ended its transport epoch, so this wait holds
                // nothing: a client that never drains stalls its own lane and
                // no one else's. When it does drain, the backoff still runs —
                // a consumer taking one frame at a time must not turn into a
                // reseed per frame against a live leader. And the reseed is a
                // reseed, never a `Plan::Resume`: the frames the channel
                // dropped were folded at or before the cursor, so resuming
                // from it would ask gx for everything AFTER them and leave the
                // hole permanent.
                GenEnd::Lagged => {
                    if self.tx.wait_drained().await.is_err() {
                        return; // the subscriber went away while we waited
                    }
                    // **The `down_after` clock is CLEARED, not merely skipped.**
                    // That clock spends the subscription on a leader that
                    // cannot be reached, and a lag is positive evidence of the
                    // opposite: this adapter only filled 1024 slots because gx
                    // served the seed and kept streaming. Leaving a stale
                    // `failing_since` running across the drain wait is how a
                    // slow consumer earns an "unreachable" verdict — stall past
                    // `down_after`, and the next brief reseed failure trips it
                    // on the spot. True mid-seed too: the seed was served.
                    //
                    // The BACKOFF is deliberately not reset with it. The two
                    // are different questions — "is the agent there" and "how
                    // hard am I hammering it" — and only the first one is
                    // answered by a lag.
                    failing_since = None;
                    streak = None;
                    plan = Plan::Reseed("lagged".to_string());
                    backoff = next_backoff(backoff, false, base, max);
                    tokio::time::sleep(jittered(backoff)).await;
                    continue;
                }
                GenEnd::Ended {
                    worked,
                    long_lived,
                    detail,
                    force_reseed,
                } => (worked, long_lived, detail, force_reseed),
            };

            // `down_after` is the RESEED clock, and it must not tick while the
            // ladder is still climbing.
            //
            // Measuring from the first loss instead lets a slow network skip
            // the ladder entirely: three resume attempts that each take a long
            // time to fail can exhaust `down_after` between them, so the pump
            // goes `Down` before the third resume and before any `cursor_lost`
            // reseed ever happens — the opposite of §3.4, which spends `Down`
            // on *reseeds* that keep failing. Resumes are bounded by
            // `resume_tries`/`resume_window` and need no second bound; only a
            // failed reseed starts or advances this clock.
            if worked {
                failing_since = None;
            } else if was_reseed {
                let since = failing_since.get_or_insert_with(Instant::now);
                if since.elapsed() >= self.timings.down_after {
                    self.go_down(format!("unreachable: {detail}")).await;
                    return;
                }
            }
            if long_lived {
                streak = None;
            }

            plan = match force_reseed {
                Some(reason) => {
                    streak = None;
                    Plan::Reseed(reason)
                }
                // A resume needs a cursor to resume FROM. Without one — the
                // fold was just reset, or the session has never carried an
                // id-bearing envelope — there is nothing to be silent about.
                //
                // **The accepted residual lives here** (plan 017 §9, filed
                // against gx, not fixable in this adapter). gx replays by
                // COUNTER and its counters are not monotonic in transcript
                // order, so a frame whose counter is BELOW the maximum this
                // fold has seen — but which gx persists after the cursor was
                // taken — is never replayed on a resume: gx answers "everything
                // after N" and that frame is not after N. A second, rarer one:
                // a replay reaching further back than `MAX_SEEN` can refold an
                // id the dedup set has already evicted, duplicating its row.
                //
                // Both are bounded by the same thing — a silent resume is
                // capped at `resume_tries` inside `resume_window`, after which
                // `cursor_lost` reseeds and rebuilds the transcript from
                // history. So the residual is a rare missing (or repeated) row
                // that the next reseed heals, which is why the ladder is short.
                None => match self.fold.resume_cursor().map(str::to_string) {
                    None => {
                        streak = None;
                        Plan::Reseed("reconnect".to_string())
                    }
                    Some(cursor) => {
                        let s = streak.get_or_insert(Streak {
                            began: Instant::now(),
                            tries: 0,
                        });
                        if s.tries >= self.timings.resume_tries
                            || s.began.elapsed() > self.timings.resume_window
                        {
                            streak = None;
                            Plan::Reseed("cursor_lost".to_string())
                        } else {
                            s.tries += 1;
                            Plan::Resume(cursor)
                        }
                    }
                },
            };

            backoff = next_backoff(backoff, worked, base, max);
            tokio::time::sleep(jittered(backoff)).await;
        }
    }

    /// The generation, with the client-lag exit split out into the `Err` half
    /// so every emitting helper can propagate it with `?` instead of each caller
    /// remembering to check. `Ok` and `Err` are both ends; only the reason
    /// differs.
    async fn run_generation(&mut self, plan: &Plan) -> GenEnd {
        match self.generation_body(plan).await {
            Ok(end) | Err(end) => end,
        }
    }

    async fn generation_body(&mut self, plan: &Plan) -> Result<GenEnd, GenEnd> {
        // ---- step 1: a reseed announces itself; a resume says nothing ----
        let cursor = match plan {
            Plan::Reseed(reason) => {
                self.generation += 1;
                let sent = self.emit(LaneEvent::Reset {
                    reason: reason.clone(),
                    generation: self.generation,
                });
                self.fold.reset();
                // §0's invariant 3: a reseed "rebuilds ALL of them" — ring
                // included. The ring is what assigns `seq`, and a reseed hands
                // the client a transcript rebuilt from scratch, so carrying the
                // old numbering and occupancy into the new generation would
                // number a fresh view as though it were a continuation of the
                // one the client was just told to discard. (A silent resume is
                // the opposite case and keeps everything, which is why this
                // lives in the reseed arm and not at the top of the function.)
                self.ring = MessageRing::new();
                self.streak_at = None;
                self.streak_bytes = 0;
                self.emitted_approvals.clear();
                self.last_session = None;
                self.session_row = None;
                // Checked AFTER the state reset, so a lagged `Reset` still
                // leaves this watcher in the shape the next generation needs.
                sent?;
                None
            }
            Plan::Resume(cursor) => Some(cursor.as_str()),
        };
        let reseed = cursor.is_none();

        // ---- step 2: the stream FIRST ----
        let resp = match self.client.open_events(&self.session, cursor).await {
            Ok(resp) => resp,
            Err(e) => return Ok(self.connect_failure(e)),
        };
        let up = Instant::now();
        let mut stream = Box::pin(resp.bytes_stream());
        let mut parser = SseParser::new().with_max_event_bytes(MAX_SSE_FRAME_BYTES);
        let mut inbox = Inbox::new();

        // ---- step 3: fetch, while the stream keeps filling the inbox ----
        let stall = self.timings.stall;
        let seed_limit = self.timings.seed_limit;
        let window = self.timings.resume_window;
        let fetched = {
            let fut = fetch(&self.client, &self.session, reseed, seed_limit);
            tokio::pin!(fut);
            loop {
                tokio::select! {
                    // Biased so a completed fetch is taken the moment it is
                    // ready; whatever is still on the wire lands in the inbox
                    // and is drained in step 5, in arrival order.
                    biased;
                    done = &mut fut => break done,
                    read = read_step(&mut stream, &mut parser, stall) => match read {
                        Read::Frames(events) => {
                            for ev in events {
                                if !inbox.push(ev) {
                                    return Ok(overflow(up, window));
                                }
                            }
                        }
                        other => return Ok(other.into_end(up, window, false)),
                    },
                }
            }
        };
        let fetched = match fetched {
            Ok(f) => f,
            Err(e) => return Ok(self.connect_failure(e)),
        };

        // ---- step 4: fold the seed, then reconcile ----
        if reseed {
            for env in &fetched.history {
                self.fold.apply(env);
            }
            self.fold_at = Instant::now();
            self.note_streak();
            // A seed BIGGER than the client channel lags right here, and the
            // generation ends without a `Ready` — deliberately, because a staged
            // generation with no `Ready` leaves a client holding a view it was
            // told to replace and given nothing to replace it with.
            self.drain_rows()?;
        }
        self.reconcile(fetched.approvals, fetched.row)?;

        // ---- step 5: the frames that arrived while the fetch ran ----
        for ev in inbox.take() {
            if let Some(end) = self.apply_frame(&ev, Emit::Defer)? {
                return Ok(end);
            }
        }

        // ---- step 6: publish ----
        self.emit_session()?;
        self.emit_approvals()?;
        if reseed {
            self.emit(LaneEvent::Ready {
                generation: self.generation,
            })?;
        }

        // ---- steady state ----
        let mut last_byte = Instant::now();
        loop {
            if self.tx.is_closed() {
                return Ok(GenEnd::Down("closed".to_string()));
            }
            match self
                .read_live(&mut stream, &mut parser, &mut last_byte)
                .await?
            {
                Read::Frames(events) => {
                    for ev in events {
                        if let Some(end) = self.apply_frame(&ev, Emit::Now)? {
                            return Ok(end);
                        }
                    }
                }
                other => return Ok(other.into_end(up, window, true)),
            }
        }
    }

    /// End the subscription, **flushing the open streak first**.
    ///
    /// §3.4: an open streak "is flushed as a partial row at `Down`". Without
    /// this, a chunk that opened a streak and was followed by a terminal event
    /// before `flush_after` elapsed is simply lost — the user loses the last
    /// thing the agent said at exactly the moment they most want to see it, and
    /// there is no later generation to recover it because the subscription is
    /// over.
    ///
    /// Every terminal path goes through here, so "flush on the way down" is one
    /// place rather than a rule five call sites have to remember.
    async fn go_down(mut self, reason: String) {
        self.fold.flush_open();
        // A full channel drops the flushed partial row — there is nothing to
        // reseed into, so there is nothing to be done about it. The `Down`
        // below is the frame that must NOT be dropped, and `publish_final`
        // waits for room rather than losing it.
        let _ = self.drain_rows();
        self.tx.publish_final(LaneEvent::Down { reason }).await;
    }

    /// A dial, pin, connect or fetch failure, mapped to what it means for the
    /// SUBSCRIPTION.
    ///
    /// Two of them are terminal, and both for the same reason: retrying cannot
    /// change the answer. A session gx does not know is not coming back, and a
    /// token gx refuses will be refused again — the epoch is already
    /// invalidated, so the next subscribe re-discovers, which is the client's
    /// call to make and not a loop's.
    ///
    /// One of them forces a RESEED. On this path a `BadRequest` means the
    /// request could not even be built the way it was asked for — in practice a
    /// resume cursor that cannot be a header value — and
    /// retrying the same resume would refuse identically until `down_after`.
    /// The repair is to stop resuming: `cursor_lost` discards the fold and
    /// rebuilds the transcript, which is exactly what a cursor this adapter
    /// cannot send has cost. (Should gx itself ever answer `bad_request` on
    /// `/events`, a reseed is the right response to that too.)
    fn connect_failure(&self, e: LaneError) -> GenEnd {
        match e {
            LaneError::UnknownSession => GenEnd::Down("unknown_session".to_string()),
            LaneError::Unauthorized => GenEnd::Down("unauthorized".to_string()),
            LaneError::BadRequest(detail) => GenEnd::Ended {
                worked: false,
                long_lived: false,
                detail,
                force_reseed: Some("cursor_lost".to_string()),
            },
            other => GenEnd::Ended {
                worked: false,
                long_lived: false,
                detail: other.to_string(),
                force_reseed: None,
            },
        }
    }

    /// Restart the flush clock **iff the open streak actually changed**.
    ///
    /// Called after anything that can touch the fold's streak. The comparison
    /// is on the streak's byte length rather than on "a frame arrived", and
    /// that is the whole fix: `hook_execution` and every other ignored kind
    /// leave the length alone, so they cannot postpone a flush they contribute
    /// nothing to. A segmenting append (which emits a row and reopens a shorter
    /// streak) changes the length too, which is why this is `!=` and not `>`.
    fn note_streak(&mut self) {
        match self.fold.open_streak_bytes() {
            None => {
                self.streak_at = None;
                self.streak_bytes = 0;
            }
            Some(bytes) if self.streak_at.is_none() || bytes != self.streak_bytes => {
                self.streak_bytes = bytes;
                self.streak_at = Some(Instant::now());
            }
            Some(_) => {}
        }
    }

    // ---- applying ----

    /// Fold what a reconnect's GETs said, and tombstone what they no longer
    /// say.
    ///
    /// `approval` and `session` frames carry no `id:` on gx's wire — they are
    /// state INVALIDATIONS, not positions — so a cursor resume never replays
    /// them and this is the only thing that keeps them true across a reconnect.
    ///
    /// The tombstones are computed BEFORE the fetched set is noted, so an
    /// approval that is in both lists is refreshed rather than buried.
    fn reconcile(&mut self, approvals: Vec<LaneApproval>, row: GxSessionRow) -> Emitted {
        let fetched: HashSet<&str> = approvals.iter().map(|a| a.id.as_str()).collect();
        let gone: Vec<LaneApproval> = self
            .fold
            .held_approvals()
            .into_iter()
            .filter(|a| a.status.is_pending() && !fetched.contains(a.id.as_str()))
            .map(|mut a| {
                // The lane held it open and the server no longer lists it, so
                // it was answered elsewhere — the TUI, another client, or the
                // agent giving up on it. `Resolved` is the honest end state;
                // leaving it pending would keep a panel blocked on a question
                // nobody can answer any more.
                a.status = LaneApprovalStatus::Resolved;
                a
            })
            .collect();
        for a in gone {
            self.fold.note_approval(a);
        }
        for a in approvals {
            self.fold.note_approval(a);
        }
        // `note_approval` mints an `approval_request` row on first sight, and a
        // reconcile is where a client first learns about an approval raised
        // while the stream was down.
        let sent = self.drain_rows();
        self.session_row = Some(row);
        self.row_at = Instant::now();
        sent
    }

    /// One SSE frame. `Ok(Some(end))` when the frame ENDS the generation;
    /// `Err` when publishing what it produced hit a full client channel.
    ///
    /// A frame this build cannot decode is dropped, never fatal: the stream has
    /// to survive a gx that grew a field, and an `update` that will not parse is
    /// one lost row rather than a lost subscription.
    fn apply_frame(&mut self, ev: &SseEvent, emit: Emit) -> Result<Option<GenEnd>, GenEnd> {
        match ev.event.as_str() {
            "update" => {
                if let Ok(env) = serde_json::from_str::<GxEnvelope>(&ev.data) {
                    self.fold.apply(&env);
                    self.fold_at = Instant::now();
                    self.note_streak();
                    self.drain_rows()?;
                }
                if emit == Emit::Now {
                    self.emit_session()?;
                }
                Ok(None)
            }
            "session" => {
                // Either the summary row or `{sessionId, removed: true}`. The
                // two are told apart by a peek rather than by a flattened
                // struct: the row's fields all carry `null`-tolerant
                // deserializers, and `#[serde(flatten)]` buffers through a
                // content map where that tolerance is easy to get subtly wrong.
                //
                // ONE parse: the peek's `Value` is handed to `from_value`
                // rather than the row being tokenized a second time from the
                // same bytes.
                match serde_json::from_str::<Value>(&ev.data) {
                    Ok(v) if v.get("removed").and_then(Value::as_bool) == Some(true) => {
                        return Ok(Some(GenEnd::Down("session_removed".to_string())));
                    }
                    Ok(v) => {
                        if let Ok(row) = serde_json::from_value::<GxSessionRow>(v) {
                            self.session_row = Some(row);
                            self.row_at = Instant::now();
                        }
                    }
                    Err(_) => {}
                }
                if emit == Emit::Now {
                    self.emit_session()?;
                }
                Ok(None)
            }
            "approval" => {
                if let Ok(res) = serde_json::from_str::<GxApprovalResource>(&ev.data) {
                    self.fold.note_approval(lane_approval(&res));
                    self.drain_rows()?;
                }
                if emit == Emit::Now {
                    self.emit_approvals()?;
                    self.emit_session()?;
                }
                Ok(None)
            }
            "reset" => {
                let reason = serde_json::from_str::<Value>(&ev.data)
                    .ok()
                    .and_then(|v| {
                        v.get("reason")
                            .and_then(Value::as_str)
                            .map(str::to_string)
                            .filter(|r| !r.is_empty())
                    })
                    .unwrap_or_else(|| "unspecified".to_string());
                // Server-influenced text on its way into a `LaneEvent`, so it
                // goes through the crate's one redactor — the same rule
                // `crate::client`'s error constructors follow. A gx that put a
                // token in a reset reason already knows it; what must not
                // happen is shed writing it somewhere it outlives the frame.
                let reason = redact_hex64(&reason);
                Ok(Some(GenEnd::Ended {
                    // The connection worked — the server is telling us our VIEW
                    // is stale, which is a different thing from a dead lane and
                    // must not push the backoff up.
                    worked: true,
                    long_lived: false,
                    detail: format!("the gx stream asked for a reseed: {reason}"),
                    force_reseed: Some(format!("server_reset:{reason}")),
                }))
            }
            // A frame kind this build has never heard of, or a keepalive that
            // somehow arrived as a named event: liveness, nothing more.
            _ => Ok(None),
        }
    }

    // ---- emitting ----

    /// Publish one frame, and hand the caller the ONE outcome it must not
    /// absorb.
    ///
    /// A CLOSED receiver stays what it always was — the subscription is gone and
    /// the run loop notices at its next check, so there is nothing to unwind. A
    /// LAGGED one is different in kind: the frame was dropped, the client cannot
    /// tell, and every caller ends the generation on it (`shed_core::lane`'s
    /// module doc, correction 13).
    fn emit(&self, ev: LaneEvent) -> Emitted {
        match self.tx.publish(ev) {
            Publish::Sent | Publish::Closed => Ok(()),
            Publish::Lagged => Err(GenEnd::Lagged),
        }
    }

    /// Take the fold's queued rows through the ring (which assigns `seq`) and
    /// emit them.
    ///
    /// Each row carries the CURSOR as of that row — gx advertises
    /// `history_cursor`, and this is the token a client hands back to
    /// [`AgentLane::history`]. It is the fold's highest counter, not its last
    /// applied one: gx's counters are not monotonic in transcript order, and a
    /// resume from anything but the maximum would ask for frames it already
    /// has.
    fn drain_rows(&mut self) -> Emitted {
        let rows = self.fold.drain_messages();
        if rows.is_empty() {
            return Ok(());
        }
        let now = now_unix_ms();
        let cursor = self.fold.resume_cursor().map(str::to_string);
        for row in rows {
            let message = self.ring.append(row, now);
            self.emit(LaneEvent::Message {
                message,
                cursor: cursor.clone(),
            })?;
        }
        Ok(())
    }

    /// Emit the session row when it actually changed.
    ///
    /// **Nothing is published before the first fetch.** A defaulted row would
    /// claim an empty title, an `unknown` activity and `approximate: false`
    /// about a session that has not been read.
    ///
    /// The activity has two live sources — gx's own roster row and this fold's
    /// verdict — and they disagree only about ORDER, because both track the
    /// same turn. So the newer one wins. Without the tie-break, a
    /// `turn_completed` folded just after a roster frame would be overwritten by
    /// a row that predates it (a spinner that never stops), and the mirror image
    /// is just as wrong.
    fn emit_session(&mut self) -> Emitted {
        // BORROWED, not cloned: this runs on every `update` frame — one per
        // message chunk — and a clone here would allocate the row's four
        // strings only to drop them again at the unchanged check below.
        let Some(base) = self.session_row.as_ref() else {
            return Ok(());
        };
        let mut row = lane_session(base, self.fold.open_approvals());
        // The id is contract: a row gx served without one must still be
        // addressable by the caller that subscribed.
        row.id = self.session.clone();
        if self.fold_at > self.row_at {
            let folded = self.fold.activity();
            if folded != RcActivity::Unknown {
                row.activity = folded;
            }
        }
        if self.last_session.as_ref() == Some(&row) {
            return Ok(());
        }
        self.last_session = Some(row.clone());
        self.emit(LaneEvent::Session { session: row })
    }

    /// Emit an `Approval` for every approval whose DTO changed, tombstones
    /// included.
    ///
    /// Id-keyed and last-write-wins, exactly as the contract says a client must
    /// read them: nothing here requires a client to have seen the `pending`
    /// frame before the `resolved` one.
    fn emit_approvals(&mut self) -> Emitted {
        let held = self.fold.held_approvals();
        let mut changed: Vec<LaneApproval> = held
            .iter()
            .filter(|a| self.emitted_approvals.get(&a.id) != Some(a))
            .cloned()
            .collect();
        // Deterministic order: two approvals changing in one frame must not
        // reach a client in hash order.
        changed.sort_by(|a, b| a.id.cmp(&b.id));
        let mut sent = Ok(());
        for a in changed {
            self.emitted_approvals.insert(a.id.clone(), a.clone());
            sent = self.emit(LaneEvent::Approval { approval: a });
            if sent.is_err() {
                break;
            }
        }
        // The fold's own approval map is bounded and evicts settled entries;
        // this map follows it, so a re-ask of an evicted id is seen as a change
        // again rather than being suppressed by a memory of the last one.
        let ids: HashSet<&str> = held.iter().map(|a| a.id.as_str()).collect();
        self.emitted_approvals
            .retain(|k, _| ids.contains(k.as_str()));
        sent
    }

    // ---- reading ----

    /// One live read, with the OPEN-STREAK FLUSH folded into its deadline.
    ///
    /// A gx turn arrives as a run of `agent_message_chunk`s and the fold
    /// coalesces them into one row, which is what stops a transcript from
    /// becoming one row per token. The cost is that a turn which stops
    /// mid-sentence — the model is thinking, the tool is slow — would show
    /// nothing at all. So a streak silent for [`GxTimings::flush_after`] is
    /// emitted as a partial row, and the next chunk opens a new streak.
    ///
    /// The stall timer is the OUTER bound and counts BYTES, not events: gx's
    /// keepalive is a comment line, which carries no event and would otherwise
    /// look exactly like silence.
    async fn read_live<S, B>(
        &mut self,
        stream: &mut S,
        parser: &mut SseParser,
        last_byte: &mut Instant,
    ) -> Result<Read, GenEnd>
    where
        S: Stream<Item = reqwest::Result<B>> + Unpin,
        B: AsRef<[u8]>,
    {
        loop {
            let since = last_byte.elapsed();
            if since >= self.timings.stall {
                return Ok(Read::Timeout);
            }
            // TWO clocks, and they measure different things. The STALL runs on
            // bytes-on-the-connection, because any byte proves the far side is
            // alive. The FLUSH runs on `streak_at` — when the open streak last
            // GREW — because an ignored kind is transparent to a streak and
            // must not be able to postpone a row it contributes nothing to.
            let mut wait = self.timings.stall - since;
            if let Some(at) = self.streak_at {
                wait = wait.min(self.timings.flush_after.saturating_sub(at.elapsed()));
            }
            match read_step(stream, parser, wait).await {
                Read::Timeout => {
                    // Only the flush is decided here. Whether the STALL has
                    // expired is the loop head's verdict and is deliberately
                    // not re-taken: two places deciding it is two places to
                    // keep in step.
                    if self
                        .streak_at
                        .is_some_and(|at| at.elapsed() >= self.timings.flush_after)
                    {
                        self.fold.flush_open();
                        self.drain_rows()?;
                        self.note_streak();
                    }
                    continue;
                }
                Read::Frames(events) => {
                    // ANY byte is liveness, an incomplete frame and a comment
                    // ping included.
                    *last_byte = Instant::now();
                    if events.is_empty() {
                        continue;
                    }
                    return Ok(Read::Frames(events));
                }
                other => return Ok(other),
            }
        }
    }
}

// ---------------------------------------------------------------------------
// the fetch
// ---------------------------------------------------------------------------

/// What one connect's GETs read. `history` is empty on a resume — the cursor
/// replay is the transcript then, and re-reading the tail would only make the
/// `seen` set work harder for the same rows.
struct Fetched {
    row: GxSessionRow,
    history: Vec<GxEnvelope>,
    approvals: Vec<LaneApproval>,
}

/// The session row FIRST, so a deleted session is detected before anything else
/// is spent on it.
///
/// A failing approvals read fails the whole fetch, and that is deliberate: the
/// reconcile turns "not in this list" into a `Resolved` tombstone, so treating
/// an unreadable list as an empty one would retire every open approval on the
/// session. A retry costs a reconnect; a wrong tombstone costs the human the
/// question they were being asked.
async fn fetch(
    client: &GxClient,
    id: &str,
    reseed: bool,
    seed_limit: u32,
) -> Result<Fetched, LaneError> {
    let row = client.rest_session(id).await?;
    let history = if reseed {
        let n = seed_limit.max(1);
        client.rest_history(id, -(n as i64), n).await?.updates
    } else {
        Vec::new()
    };
    // Through the contract verb, so the "drop `resolved`, keep `submitted`"
    // rule has exactly one implementation.
    let approvals = client.approvals(id).await?;
    Ok(Fetched {
        row,
        history,
        approvals,
    })
}

// ---------------------------------------------------------------------------
// the bounded inbox
// ---------------------------------------------------------------------------

/// The frames buffered between the connect and the end of the fetch.
struct Inbox {
    items: Vec<SseEvent>,
    bytes: usize,
}

impl Inbox {
    fn new() -> Inbox {
        Inbox {
            items: Vec::new(),
            bytes: 0,
        }
    }

    /// `false` on overflow — the caller reseeds rather than dropping a frame
    /// whose importance it cannot know.
    fn push(&mut self, ev: SseEvent) -> bool {
        if self.items.len() >= MAX_INBOX_ITEMS || self.bytes + ev.data.len() > MAX_INBOX_BYTES {
            return false;
        }
        self.bytes += ev.data.len();
        self.items.push(ev);
        true
    }

    fn take(&mut self) -> Vec<SseEvent> {
        self.bytes = 0;
        std::mem::take(&mut self.items)
    }
}

fn overflow(up: Instant, window: Duration) -> GenEnd {
    GenEnd::Ended {
        worked: false,
        long_lived: up.elapsed() >= window,
        detail: "the gx event inbox overflowed".to_string(),
        force_reseed: Some("overflow".to_string()),
    }
}

// ---------------------------------------------------------------------------
// reading the stream
// ---------------------------------------------------------------------------

/// One read step's outcome.
enum Read {
    /// A chunk arrived; these are the events it COMPLETED, which may be none.
    Frames(Vec<SseEvent>),
    /// The deadline passed with no bytes at all.
    Timeout,
    /// The server closed the stream.
    Eof,
    /// A transport error, or an SSE event past [`MAX_SSE_FRAME_BYTES`].
    Failed(String),
}

impl Read {
    fn into_end(self, up: Instant, window: Duration, worked: bool) -> GenEnd {
        let detail = match self {
            // Not reachable from a caller that matched `Frames` first; the
            // exhaustive arm keeps that a compile-time fact.
            Read::Frames(_) => "unexpected frames".to_string(),
            Read::Timeout => "the gx event stream went silent".to_string(),
            Read::Eof => "the gx event stream ended".to_string(),
            Read::Failed(e) => e,
        };
        GenEnd::Ended {
            worked,
            long_lived: up.elapsed() >= window,
            detail,
            force_reseed: None,
        }
    }
}

/// Read ONE chunk, under `wait`, and return whatever complete SSE events it
/// finished.
async fn read_step<S, B>(stream: &mut S, parser: &mut SseParser, wait: Duration) -> Read
where
    S: Stream<Item = reqwest::Result<B>> + Unpin,
    B: AsRef<[u8]>,
{
    match tokio::time::timeout(wait, stream.next()).await {
        Err(_) => Read::Timeout,
        Ok(None) => Read::Eof,
        // Redacted for the same reason the reset reason is: this string becomes
        // a `Down`'s text, which a client renders and a log keeps.
        Ok(Some(Err(e))) => {
            Read::Failed(redact_hex64(&format!("reading the gx event stream: {e}")))
        }
        Ok(Some(Ok(chunk))) => match parser.try_feed(chunk.as_ref()) {
            Err(over) => Read::Failed(over.to_string()),
            Ok(events) => Read::Frames(events),
        },
    }
}

/// The wall clock in milliseconds, for the ring's "stamp a row that carries no
/// time of its own".
///
/// `SystemTime` rather than `chrono::Utc::now()` because this crate
/// deliberately has no chrono ([`crate`]'s Cargo.toml says why), and the ring
/// takes a plain `i64`.
fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_backoff_bounds_are_the_intended_numbers_at_the_default_timings() {
        let (base, max) = backoff_bounds(&GxTimings::default());
        assert_eq!(base, Duration::from_millis(100));
        assert_eq!(max, Duration::from_secs(5));
    }

    #[test]
    fn the_backoff_bounds_scale_with_a_test_sized_down_after() {
        // A suite that scales `down_after` to sub-second must not then spend its
        // whole ladder asleep at a fixed 100 ms floor.
        let (base, max) = backoff_bounds(&GxTimings {
            down_after: Duration::from_millis(600),
            ..GxTimings::default()
        });
        assert_eq!(base, Duration::from_millis(30));
        assert_eq!(max, Duration::from_millis(150));
        assert!(base <= max, "the floor never exceeds the ceiling");

        // And an absurdly small one still cannot produce a zero-length sleep,
        // which would busy-loop the reconnect.
        let (base, max) = backoff_bounds(&GxTimings {
            down_after: Duration::from_millis(1),
            ..GxTimings::default()
        });
        assert!(
            base >= Duration::from_millis(1) && max >= base,
            "{base:?} {max:?}"
        );
    }

    #[test]
    fn the_inbox_is_bounded_by_items_and_by_bytes() {
        let ev = |n: usize| SseEvent {
            event: "update".to_string(),
            data: "x".repeat(n),
        };
        let mut inbox = Inbox::new();
        for _ in 0..MAX_INBOX_ITEMS {
            assert!(inbox.push(ev(1)));
        }
        assert!(!inbox.push(ev(1)), "the item cap holds");
        assert_eq!(inbox.take().len(), MAX_INBOX_ITEMS);

        let mut inbox = Inbox::new();
        assert!(inbox.push(ev(MAX_INBOX_BYTES)));
        assert!(!inbox.push(ev(1)), "the byte cap holds");
    }
}
