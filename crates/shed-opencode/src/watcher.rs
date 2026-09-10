//! The reconnecting watcher — one `GET /event` connection per subscription, and
//! the generation bracket that makes a reconnect invisible to a client.
//!
//! # A generation, in the pinned order
//!
//! Every connection is a **generation**, and every generation runs the same five
//! steps in this order (the rc hub's order, ported from
//! `shed-broker/src/rc_hub/watch_opencode_transport.rs`):
//!
//! 1. Emit [`LaneEvent::Reset`] and [`OpencodeFold::reset`]. The reason is
//!    `"seed"` on the first connect (`"cursor_unresolvable"` when the caller
//!    supplied a cursor this adapter cannot honor — `history_cursor: false`),
//!    then `"reconnect"`, `"stall"`, `"overflow"` or `"lagged"` by cause.
//! 2. **Open `GET /event?directory=` FIRST** and wait for `server.connected`,
//!    buffering every later frame into a bounded inbox
//!    ([`MAX_INBOX_ITEMS`] / [`MAX_INBOX_BYTES`]).
//! 3. Run the REST seed — `/session/{id}`, `/session/{id}/message`, a bounded
//!    recursive `/session/{id}/children` walk, then `/session/status`,
//!    `/permission` and
//!    `/question` (the last three `?directory=`-scoped, approvals filtered to
//!    the root and its descendants) — **while the stream keeps reading into the
//!    inbox**, and emit the rows it implies.
//! 4. Drain the buffered frames through the fold. A live `session.status` seen
//!    during the seed therefore lands AFTER the REST fallback and wins, which is
//!    the hub's seed-complete barrier expressed as an ordering rather than a
//!    marker frame. Their session/approval effects are folded but NOT emitted
//!    ([`Emit::Defer`]) — step 5 owns that half of the order.
//! 5. Emit [`LaneEvent::Session`], then the approval frames, then
//!    [`LaneEvent::Ready`]. From here frames apply as they arrive.
//!
//! Seeding BEFORE opening the stream is the bug this order exists to prevent: an
//! event minted between the REST read and the subscribe is gone forever, and it
//! is exactly the events that matter (an `idle`, a `permission.replied`) that
//! land in that window.
//!
//! # Scoping — the id is the only filter
//!
//! - A frame whose `properties.sessionID` is **empty** is NOT applied but DOES
//!   count as liveness. That is how `server.connected` and the wire's keep-alive
//!   arrive, and treating them as silence would stall a healthy stream.
//! - A frame for the **root** applies in full.
//! - A frame for a **descendant** applies only if it is an approval event, and
//!   its transcript rows are discarded: a child's approval blocks the same
//!   agent (so it must surface, attributed by the child's own `session_id`), but
//!   a child's conversation is not the root's transcript.
//! - Anything else is dropped. There is no `not_before`, no claim, no directory
//!   match — in the roost model the session id arrives by construction, so the
//!   lane scopes rather than searches, and the plan-012 cross-contamination bug
//!   (two sessions in one directory adopting each other's conversation) is
//!   unrepresentable.
//!
//! # Ending
//!
//! A stall (30 s with no bytes at all), an EOF, a transport error, an oversized
//! SSE frame or an inbox overflow ends the generation and the next one starts
//! after a crate-local jittered backoff ([`OC_BACKOFF_BASE`] →
//! [`OC_BACKOFF_MAX`], reset once a generation reaches its `Ready`).
//!
//! **A client that stops reading ends one too.** The frame channel is bounded
//! ([`shed_core::lane::LANE_CHANNEL_CAPACITY`]), a full one drops the frame, and
//! every emitting helper here propagates that as [`GenEnd::Lagged`] — so the
//! generation ends at the FIRST dropped frame, in steady state, mid-seed, or
//! mid-reseed. The run loop then waits for the client to drain (holding no
//! stream: this generation already dropped it), takes the ordinary failure
//! backoff, and reseeds as `"lagged"`. **`"overflow"` and `"lagged"` are
//! different overflows and both names stay:** `"overflow"` is THIS crate's inbox
//! (frames it had not folded yet), `"lagged"` is the client's channel (frames it
//! had published).
//!
//! The backoff is crate-local on purpose: `shed_app::backoff` is `pub(crate)`
//! and unreachable from here, and its 30 s ceiling is wrong for a feed that is a
//! loopback socket away.
//!
//! Only two things END the subscription with a [`LaneEvent::Down`]: a reseed
//! that answers 404 (`"unknown_session"` — the session was deleted, and no
//! amount of retrying brings it back), and a FIRST connect that never came up.
//! A dial failure after one successful connect keeps retrying, because a client
//! whose agent restarts should get its stream back rather than a dead panel.

use std::collections::{HashMap, HashSet};
use std::time::Duration;

use futures_util::{Stream, StreamExt as _};
use serde::{Deserialize, Serialize};
use serde_json::value::RawValue;
use shed_core::lane::{
    LaneApproval, LaneEvent, LanePublisher, LaneSession, LaneStop, LaneSubscription, Publish,
};
use shed_core::sse::SseParser;

use crate::client::{
    lane_session, OpencodeClient, RestMessage, RestPermission, RestQuestion, RestSession,
};
use crate::fold::OpencodeFold;
use crate::helpers::{null_default, object_default};
use crate::ring::{now_utc, MessageRing};

// ---- bounds + tuning (the hub's, ported) ----

/// The inbox is bounded by BOTH item count and total bytes. Overflow forces a
/// full reconnect + reseed rather than a drop-oldest: dropping the oldest frame
/// can permanently swallow a `permission.replied`, a `session.idle` or a
/// tool completion, and a fold that misses one of those is wrong forever.
///
/// The hub answers an overflow with [`OpencodeFold::note_gap`] plus a reseed on
/// the KEPT fold. This watcher never calls `note_gap`, and that is not an
/// omission: an overflow here starts a new GENERATION, whose first act is a full
/// [`OpencodeFold::reset`] — strictly stronger than the gap handling, since it
/// forgets the pending set `note_gap` clears AND everything else. The method
/// stays on the fold for a future consumer that wants the cheaper option.
pub const MAX_INBOX_ITEMS: usize = 1024;
/// See [`MAX_INBOX_ITEMS`].
pub const MAX_INBOX_BYTES: usize = 4 << 20;

/// One accumulated SSE event's cap. Exceeding it is a read error — disconnect
/// and reconnect — rather than unbounded buffering for a never-terminating
/// `data:` field.
pub const MAX_SSE_FRAME_BYTES: usize = 4 << 20;

/// How long the stream may go with NO bytes at all before it is considered
/// wedged. Any received chunk resets it, comment pings included: opencode has no
/// `server.heartbeat` event, so the keep-alive is whatever its encoder emits and
/// only "did anything arrive" is a portable liveness test.
pub const STALL_WINDOW: Duration = Duration::from_secs(30);

/// The reconnect backoff floor and ceiling (jittered, reset on a generation
/// that reached `Ready`).
pub const OC_BACKOFF_BASE: Duration = Duration::from_millis(100);
/// See [`OC_BACKOFF_BASE`].
pub const OC_BACKOFF_MAX: Duration = Duration::from_secs(5);

/// The event types a DESCENDANT session is allowed to contribute. Everything
/// else a child emits — its own text, its own tools, its own status — belongs to
/// the child's transcript, not the root's.
fn is_approval_type(typ: &str) -> bool {
    matches!(
        typ,
        "permission.asked"
            | "permission.replied"
            | "question.asked"
            | "question.replied"
            | "question.rejected"
    )
}

// ---- spawn ----

/// Spawn the pump for one subscription and hand back the contract's
/// receiver + stop handle.
pub(crate) fn spawn(
    client: OpencodeClient,
    root: String,
    directory: String,
    cursor: Option<String>,
) -> LaneSubscription {
    let (tx, rx) = LanePublisher::channel();
    let watcher = Watcher {
        client,
        root: root.clone(),
        directory,
        tx,
        fold: OpencodeFold::new(),
        ring: MessageRing::new(),
        generation: 0,
        scope: HashSet::from([root]),
        emitted_approvals: HashMap::new(),
        last_session: None,
        session_row: None,
    };
    let task = tokio::spawn(watcher.run(cursor));
    LaneSubscription {
        rx,
        stop: LaneStop::new(task),
    }
}

// ---- the pump ----

struct Watcher {
    client: OpencodeClient,
    /// The subscribed session. The ONLY transcript filter.
    root: String,
    /// The root's directory, resolved once at `subscribe` and cached here:
    /// `/event`, `/session/status`, `/permission` and `/question` are
    /// instance-scoped and answer for the wrong workspace without it.
    directory: String,
    tx: LanePublisher,
    fold: OpencodeFold,
    /// One ring for the life of the subscription — see [`crate::ring`] for why
    /// `seq` outlives the fold's per-generation reset.
    ring: MessageRing,
    generation: u64,
    /// The root plus its descendants: the ids whose APPROVALS surface.
    scope: HashSet<String>,
    /// What was last emitted per (kind, id), so a change is emitted once and an
    /// unchanged approval is not re-sent on every frame.
    emitted_approvals: HashMap<(String, String), LaneApproval>,
    last_session: Option<LaneSession>,
    /// The REST row behind [`LaneEvent::Session`] (title, cwd, timestamps),
    /// refreshed by every seed.
    session_row: Option<RestSession>,
}

/// Whether a frame's SESSION and APPROVAL effects are emitted as it is applied.
///
/// Transcript rows are unaffected — they are first in the pinned seed order, so
/// they emit either way.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Emit {
    /// Steady state: a frame's effects reach the client as it lands.
    Now,
    /// The seed's buffered replay. The frames are FOLDED (so the closing
    /// sequence sees their effect) but nothing is emitted, because the pinned
    /// seed order is messages → session → approvals → `Ready` and step 5 owns
    /// the last three. Emitting here would put an `Approval` in front of the
    /// first `Session` — which the closing sequence cannot repair, since
    /// [`Watcher::emit_approvals`] and [`Watcher::emit_session`] both
    /// deduplicate against what they already sent.
    Defer,
}

/// How a generation ended.
#[derive(Debug)]
enum GenEnd {
    /// Terminal: emit [`LaneEvent::Down`] with this reason and stop.
    Down(String),
    /// Reconnect. `reason` is the next [`LaneEvent::Reset`]'s; `worked` is
    /// whether this generation reached its `Ready` (the backoff reset);
    /// `detail` is what to say if this was the FIRST connect and there is
    /// nothing to retry from.
    Retry {
        reason: &'static str,
        worked: bool,
        detail: String,
    },
    /// The CLIENT stopped reading and a frame was dropped
    /// ([`shed_core::lane::Publish::Lagged`]). Its own variant rather than a
    /// [`GenEnd::Retry`] because the run loop owes it two things a retry does
    /// not: it waits for the client to drain before reseeding, and it does NOT
    /// count towards "the agent is unreachable" — a slow consumer is not a dead
    /// lane, and must never be spent as one.
    Lagged,
}

/// The end a dropped frame produces. Named once so every emitting helper says
/// the same thing.
fn lagged() -> GenEnd {
    GenEnd::Lagged
}

/// `Ok` while the client is keeping up; `Err(GenEnd::Lagged)` the instant a
/// frame is dropped, which every caller propagates.
type Emitted = Result<(), GenEnd>;

impl Watcher {
    async fn run(mut self, cursor: Option<String>) {
        // A cursor cannot be honored (`history_cursor: false`), and silently
        // ignoring it would leave a client believing it resumed. Naming the
        // first Reset `cursor_unresolvable` says "discard what you held" in the
        // same frame that starts the full replay.
        let mut reason: &'static str = if cursor.is_some() {
            "cursor_unresolvable"
        } else {
            "seed"
        };
        let mut backoff = OC_BACKOFF_BASE;
        let mut ever_connected = false;

        loop {
            if self.tx.is_closed() {
                return; // the subscription was dropped
            }
            self.generation += 1;
            let end = match self.begin_generation(reason) {
                Ok(()) => self.run_generation(&mut ever_connected).await,
                Err(end) => end,
            };
            match end {
                GenEnd::Down(r) => {
                    self.tx.publish_final(LaneEvent::Down { reason: r }).await;
                    return;
                }
                // The generation already dropped its `/event` stream on the way
                // out, so this wait holds no transport: a client that never
                // drains stalls its own lane and nothing else. When it does
                // drain, the backoff still runs before the reseed — a consumer
                // taking one frame at a time must not become a seed-per-frame
                // load on the agent.
                GenEnd::Lagged => {
                    if self.tx.wait_drained().await.is_err() {
                        return; // the subscriber went away while we waited
                    }
                    backoff = next_backoff(backoff, false);
                    tokio::time::sleep(jittered(backoff)).await;
                    reason = "lagged";
                }
                GenEnd::Retry {
                    reason: next,
                    worked,
                    detail,
                } => {
                    if !ever_connected {
                        // The caller's FIRST connect never came up. Retrying
                        // silently would leave a client staring at a Reset that
                        // never resolves; `Down` is the honest answer.
                        self.tx
                            .publish_final(LaneEvent::Down { reason: detail })
                            .await;
                        return;
                    }
                    backoff = next_backoff(backoff, worked);
                    tokio::time::sleep(jittered(backoff)).await;
                    reason = next;
                }
            }
        }
    }

    /// Step 1 of every generation: the `Reset` frame, then the state it
    /// promises. The RING is deliberately NOT reset — `seq` is monotonic across
    /// resets within one subscription.
    fn begin_generation(&mut self, reason: &str) -> Emitted {
        let sent = self.emit(LaneEvent::Reset {
            reason: reason.to_string(),
            generation: self.generation,
        });
        self.fold.reset();
        self.emitted_approvals.clear();
        self.last_session = None;
        self.scope = HashSet::from([self.root.clone()]);
        sent
    }

    /// The generation, with the client-lag exit split out into the `Err` half so
    /// every emitting helper can propagate it with `?` instead of each caller
    /// remembering to check.
    ///
    /// `Ok` and `Err` are both ends; only the reason differs.
    async fn run_generation(&mut self, ever_connected: &mut bool) -> GenEnd {
        match self.generation_body(ever_connected).await {
            Ok(end) | Err(end) => end,
        }
    }

    async fn generation_body(&mut self, ever_connected: &mut bool) -> Result<GenEnd, GenEnd> {
        // Step 2: the stream FIRST.
        let resp = match self.client.open_event_stream(&self.directory).await {
            Ok(r) => r,
            Err(e) => {
                return Ok(GenEnd::Retry {
                    reason: "reconnect",
                    worked: false,
                    detail: e.to_string(),
                })
            }
        };
        let mut stream = Box::pin(resp.bytes_stream());
        let mut parser = SseParser::new().with_max_event_bytes(MAX_SSE_FRAME_BYTES);
        let mut inbox = Inbox::new();

        // Wait for `server.connected`. Anything ahead of it is buffered rather
        // than dropped — opencode sends it first, and a fake or a proxy that
        // reorders must not cost us a frame.
        loop {
            match read_step(&mut stream, &mut parser).await {
                Read::Frames(frames) => {
                    let mut connected = false;
                    for raw in frames {
                        if peek(&raw).typ == "server.connected" {
                            connected = true;
                        } else if !inbox.push(raw) {
                            return Ok(retry_overflow());
                        }
                    }
                    if connected {
                        break;
                    }
                }
                other => return Ok(other.into_retry(false)),
            }
        }
        *ever_connected = true;

        // Step 3: the REST seed, WHILE the stream keeps filling the inbox. The
        // seed borrows the client immutably; the reader owns the stream and the
        // inbox, so the two genuinely run concurrently.
        let seed = {
            let seed_fut = fetch_seed(&self.client, &self.root, &self.directory);
            tokio::pin!(seed_fut);
            loop {
                tokio::select! {
                    // Biased so a completed seed is taken as soon as it is
                    // ready: whatever is still on the wire lands in the inbox
                    // and is drained in step 4, in arrival order.
                    biased;
                    done = &mut seed_fut => break done,
                    read = read_step(&mut stream, &mut parser) => match read {
                        Read::Frames(frames) => {
                            for raw in frames {
                                if !inbox.push(raw) {
                                    return Ok(retry_overflow());
                                }
                            }
                        }
                        other => return Ok(other.into_retry(false)),
                    },
                }
            }
        };
        let seed = match seed {
            Ok(s) => s,
            // The one 404 that ends a subscription: the session is gone.
            Err(shed_core::lane::LaneError::UnknownSession) => {
                return Ok(GenEnd::Down("unknown_session".to_string()))
            }
            Err(e) => {
                return Ok(GenEnd::Retry {
                    reason: "reconnect",
                    worked: false,
                    detail: e.to_string(),
                })
            }
        };
        self.apply_seed(seed)?;

        // Step 4: the frames that arrived while the seed ran, in order. Their
        // TRANSCRIPT rows emit as they fold (messages come first in the pinned
        // order anyway); their session/approval effects are DEFERRED to step 5,
        // because emitting them here would put an `Approval` ahead of the first
        // `Session` — and emission is deduplicated, so step 5 could not repair
        // it.
        for raw in inbox.take() {
            self.apply_frame(&raw, Emit::Defer)?;
        }

        // Step 5: the seed's closing frames. Session first, then approvals —
        // the order `shed_core::lane`'s module doc pins — then `Ready`.
        self.emit_session()?;
        self.emit_approvals()?;
        self.emit(LaneEvent::Ready {
            generation: self.generation,
        })?;

        // Steady state.
        loop {
            if self.tx.is_closed() {
                return Ok(GenEnd::Down("closed".to_string()));
            }
            match read_step(&mut stream, &mut parser).await {
                Read::Frames(frames) => {
                    for raw in frames {
                        self.apply_frame(&raw, Emit::Now)?;
                    }
                }
                other => return Ok(other.into_retry(true)),
            }
        }
    }

    // ---- applying ----

    fn apply_seed(&mut self, seed: Seed) -> Emitted {
        self.scope = seed.scope;
        // The root is the scope's floor whatever `/children` said.
        self.scope.insert(self.root.clone());
        self.session_row = Some(seed.session);

        for raw in seed_message_envelopes(&self.root, &seed.messages) {
            self.fold.apply_line(&raw);
        }
        // A seed BIGGER than the client channel lags right here, and the
        // generation ends without a `Ready` — deliberately, because a staged
        // generation with no `Ready` is a client stuck on its previous view
        // with nothing to swap in.
        self.drain_rows(true)?;

        // The REST status is a FALLBACK: it establishes the boundary only if no
        // live `session.status`/`session.idle` has been folded. A live one
        // buffered during the seed is drained in step 4, i.e. after this, and
        // wins by arriving later.
        self.fold.apply_status_fallback(seed.idle);

        let approvals = seed_approvals(
            &self.scope,
            seed.permissions.as_deref(),
            seed.questions.as_deref(),
        );
        for (session_id, raw) in approvals.envelopes {
            self.fold.apply_line(&raw);
            // A descendant's approval updates approval state but contributes
            // nothing to the ROOT's transcript.
            self.drain_rows(session_id == self.root)?;
        }
        // Retire anything the fold still holds open that the server no longer
        // lists. On a freshly-reset fold this retires nothing; it is here
        // because the seed's authority is the same on every generation and a
        // future kept-fold reseed must not need a second code path.
        self.fold.seed_approvals(
            approvals.permission_ids.as_deref(),
            approvals.question_ids.as_deref(),
        );
        self.drain_rows(true)
    }

    /// One live frame: scope it, fold it, emit what it changed.
    ///
    /// `emit` is [`Emit::Defer`] for the seed's buffered replay (step 4) and
    /// [`Emit::Now`] in steady state. See [`Emit`].
    fn apply_frame(&mut self, raw: &[u8], emit: Emit) -> Emitted {
        let pk = peek(raw);

        // `session.created` carries no `sessionID` — the new session's id is in
        // `properties.info` — so it is inspected BEFORE the id filter. A child
        // of anything already in scope joins the scope.
        if pk.typ == "session.created" {
            let info = &pk.properties.info;
            if !info.id.is_empty()
                && !info.parent_id.is_empty()
                && self.scope.contains(&info.parent_id)
            {
                self.scope.insert(info.id.clone());
            }
            return Ok(());
        }

        let sid = &pk.properties.session_id;
        // Empty id: liveness only. `read_step` already counted the bytes.
        if sid.is_empty() {
            return Ok(());
        }
        let is_root = *sid == self.root;
        if !is_root && !(self.scope.contains(sid) && is_approval_type(&pk.typ)) {
            return Ok(()); // a sibling root, or a child's conversation
        }

        self.fold.apply_line(raw);
        self.drain_rows(is_root)?;
        if emit == Emit::Now {
            self.emit_approvals()?;
            self.emit_session()?;
        }
        Ok(())
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
            Publish::Lagged => Err(lagged()),
        }
    }

    /// Takes the fold's queued rows and either emits them (through the ring,
    /// which assigns `seq`) or discards them.
    fn drain_rows(&mut self, emit: bool) -> Emitted {
        let rows = self.fold.drain_messages();
        if !emit {
            return Ok(());
        }
        let now = now_utc().timestamp_millis();
        for row in rows {
            let message = self.ring.append(row, now);
            self.emit(LaneEvent::Message {
                message,
                cursor: None,
            })?;
        }
        Ok(())
    }

    /// Emits an [`LaneEvent::Approval`] for every approval whose DTO changed,
    /// including one that just resolved (which leaves the fold's pending
    /// snapshot and is re-read from the fold by kind + id).
    fn emit_approvals(&mut self) -> Emitted {
        let pending = self.fold.pending_approvals();
        let mut now: HashMap<(String, String), LaneApproval> = HashMap::new();
        for a in pending {
            now.insert((a.kind.as_str().to_string(), a.id.clone()), a);
        }
        let mut changed: Vec<LaneApproval> = Vec::new();
        for (key, approval) in &now {
            if self.emitted_approvals.get(key) != Some(approval) {
                changed.push(approval.clone());
            }
        }
        for (key, previous) in &self.emitted_approvals {
            if now.contains_key(key) {
                continue;
            }
            // It left the pending set: it resolved. The fold still holds the
            // tombstone, which carries the decision.
            let resolved = self
                .fold
                .approval(shed_core::lane::LaneApprovalKind::from_wire(&key.0), &key.1)
                .unwrap_or_else(|| {
                    let mut gone = previous.clone();
                    gone.status = shed_core::lane::LaneApprovalStatus::Resolved;
                    gone
                });
            changed.push(resolved);
        }
        // Deterministic order: two approvals resolving in one frame must not
        // reach a client in hash order.
        changed.sort_by(|a, b| (a.kind.as_str(), &a.id).cmp(&(b.kind.as_str(), &b.id)));
        let mut sent = Ok(());
        for a in changed {
            self.emitted_approvals
                .insert((a.kind.as_str().to_string(), a.id.clone()), a.clone());
            sent = self.emit(LaneEvent::Approval { approval: a });
            if sent.is_err() {
                break;
            }
        }
        // Drop the resolved entries so a later re-ask of the same id is seen as
        // a change again. Done even on a lagged exit: this generation is over
        // either way, and the next one clears the map wholesale.
        self.emitted_approvals.retain(|k, _| now.contains_key(k));
        sent
    }

    /// Emits [`LaneEvent::Session`] when the row actually changed. `approximate`
    /// is FALSE here, and that is not an oversight: this row's activity comes
    /// from the live fold, not from the `/session/status` poll the roster rows
    /// ([`crate::OpencodeClient`]'s `sessions`/`session`) are built from, which
    /// report `true`. The flag is per-ROW — `shed_core::lane`'s correction 7.
    fn emit_session(&mut self) -> Emitted {
        let base = self.session_row.clone().unwrap_or_default();
        let mut row = lane_session(
            &base,
            self.fold.activity(),
            self.fold.open_approvals() as u32,
            false,
        );
        // `session_row` is always the root's, but a defaulted row would carry
        // an empty id — the id is contract, so it is asserted here.
        row.id = self.root.clone();
        if self.last_session.as_ref() == Some(&row) {
            return Ok(());
        }
        self.last_session = Some(row.clone());
        self.emit(LaneEvent::Session { session: row })
    }
}

// ---- the REST seed ----

/// Everything one seed reads. The two approval halves are `Option` because
/// their authority is independent: a FAILED read says nothing (and must never
/// be folded as "nothing is open", which would retire live approvals), while a
/// successful one — the empty list included — is authoritative.
struct Seed {
    session: RestSession,
    messages: Vec<RestMessage>,
    /// The root and its descendants, transitively — see
    /// [`OpencodeClient::approval_scope`]. This is the same set the live
    /// `session.created` handler grows, so a reseed restores exactly what a
    /// reconnect threw away (a GRANDCHILD included, which a one-level
    /// `/children` read would silently drop).
    scope: HashSet<String>,
    /// The status boundary: `/session/status` omits idle sessions, so an absent
    /// id in a 200 body means idle.
    idle: bool,
    permissions: Option<Vec<RestPermission>>,
    questions: Option<Vec<RestQuestion>>,
}

/// Reads the seed. A [`shed_core::lane::LaneError::UnknownSession`] from either
/// session-addressed read is what ends the subscription — the caller maps it to
/// `Down{"unknown_session"}`.
async fn fetch_seed(
    client: &OpencodeClient,
    root: &str,
    directory: &str,
) -> Result<Seed, shed_core::lane::LaneError> {
    // First, so a deleted session is detected before anything else is done, and
    // so the row behind `LaneEvent::Session` is refreshed every generation (an
    // opencode session is titled after its first turn).
    let session = client.rest_session(root).await?;
    let messages = client.rest_messages(root).await?;
    // Transitive, bounded and cycle-safe; a failed `/children` read degrades to
    // "nothing below that node" rather than failing the seed, because missing a
    // child's approval is a gap and refusing to seed at all is an outage.
    let scope = client.approval_scope(root).await;
    // A failed status read means the activity boundary could not be
    // established, and a fold with no boundary reports the session wrong for as
    // long as it stays quiet. The hub fails the whole seed on it; so does this.
    let status = client.rest_status(directory).await?;
    let idle = status.get(root).map(|s| s.typ == "idle").unwrap_or(true);
    let permissions = client.rest_permissions(directory).await.ok();
    let questions = client.rest_questions(directory).await.ok();
    Ok(Seed {
        session,
        messages,
        scope,
        idle,
        permissions,
        questions,
    })
}

// ---- synthesized seed envelopes ----

/// A synthesized `/event` envelope. The passthrough fields borrow their
/// `RawValue` so a seeded row carries EXACTLY the bytes opencode served — key
/// order included, which is what keeps a seeded tool detail byte-identical to a
/// live one.
#[derive(Serialize)]
struct SynthEnvelope<'a> {
    #[serde(rename = "type")]
    typ: &'a str,
    properties: SynthProps<'a>,
}

#[derive(Serialize, Default)]
struct SynthProps<'a> {
    #[serde(rename = "sessionID")]
    session_id: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    info: Option<&'a RawValue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    part: Option<&'a RawValue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    id: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    permission: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    patterns: Option<&'a [String]>,
    #[serde(skip_serializing_if = "Option::is_none")]
    metadata: Option<&'a RawValue>,
    #[serde(skip_serializing_if = "Option::is_none")]
    questions: Option<&'a RawValue>,
}

fn synth(typ: &str, props: SynthProps<'_>) -> Option<Vec<u8>> {
    serde_json::to_vec(&SynthEnvelope {
        typ,
        properties: props,
    })
    .ok()
}

/// `GET /session/{id}/message` → the `message.updated` +
/// `message.part.updated` envelopes the fold already knows how to read. Shared
/// with [`crate::client::OpencodeClient`]'s `history`, which folds them through
/// a throwaway fold and ring.
pub(crate) fn seed_message_envelopes(id: &str, msgs: &[RestMessage]) -> Vec<Vec<u8>> {
    let mut out = Vec::new();
    for m in msgs {
        if m.info.is_some() {
            if let Some(raw) = synth(
                "message.updated",
                SynthProps {
                    session_id: id,
                    info: m.info.as_deref(),
                    ..SynthProps::default()
                },
            ) {
                out.push(raw);
            }
        }
        for part in &m.parts {
            if let Some(raw) = synth(
                "message.part.updated",
                SynthProps {
                    session_id: id,
                    part: Some(part),
                    ..SynthProps::default()
                },
            ) {
                out.push(raw);
            }
        }
    }
    out
}

/// The approval half of a seed: the replay envelopes (each tagged with the
/// session it belongs to, so a descendant's transcript rows can be discarded)
/// plus the authoritative open-id sets [`OpencodeFold::seed_approvals`] retires
/// against.
pub(crate) struct ApprovalSeed {
    pub(crate) envelopes: Vec<(String, Vec<u8>)>,
    pub(crate) permission_ids: Option<Vec<String>>,
    pub(crate) question_ids: Option<Vec<String>>,
}

/// Filters `/permission` and `/question` to `scope` (the root and its
/// descendants) and synthesizes the `asked` envelopes for what remains.
pub(crate) fn seed_approvals(
    scope: &HashSet<String>,
    permissions: Option<&[RestPermission]>,
    questions: Option<&[RestQuestion]>,
) -> ApprovalSeed {
    let mut envelopes = Vec::new();
    let permission_ids = permissions.map(|all| {
        let mut ids = Vec::new();
        for p in all.iter().filter(|p| scope.contains(&p.session_id)) {
            if let Some(raw) = synth(
                "permission.asked",
                SynthProps {
                    session_id: &p.session_id,
                    id: Some(&p.id),
                    permission: Some(&p.permission),
                    patterns: Some(&p.patterns),
                    metadata: p.metadata.as_deref(),
                    ..SynthProps::default()
                },
            ) {
                envelopes.push((p.session_id.clone(), raw));
            }
            ids.push(p.id.clone());
        }
        ids
    });
    let question_ids = questions.map(|all| {
        let mut ids = Vec::new();
        for q in all.iter().filter(|q| scope.contains(&q.session_id)) {
            if let Some(raw) = synth(
                "question.asked",
                SynthProps {
                    session_id: &q.session_id,
                    id: Some(&q.id),
                    questions: q.questions.as_deref(),
                    ..SynthProps::default()
                },
            ) {
                envelopes.push((q.session_id.clone(), raw));
            }
            ids.push(q.id.clone());
        }
        ids
    });
    ApprovalSeed {
        envelopes,
        permission_ids,
        question_ids,
    }
}

// ---- the bounded inbox ----

/// The frames buffered between `server.connected` and the end of the REST seed.
struct Inbox {
    items: Vec<Vec<u8>>,
    bytes: usize,
}

impl Inbox {
    fn new() -> Inbox {
        Inbox {
            items: Vec::new(),
            bytes: 0,
        }
    }

    /// Returns false on overflow — the caller reconnects and reseeds rather
    /// than dropping a frame it cannot know the importance of.
    fn push(&mut self, raw: Vec<u8>) -> bool {
        if self.items.len() >= MAX_INBOX_ITEMS || self.bytes + raw.len() > MAX_INBOX_BYTES {
            return false;
        }
        self.bytes += raw.len();
        self.items.push(raw);
        true
    }

    fn take(&mut self) -> Vec<Vec<u8>> {
        self.bytes = 0;
        std::mem::take(&mut self.items)
    }
}

fn retry_overflow() -> GenEnd {
    GenEnd::Retry {
        reason: "overflow",
        worked: false,
        detail: "the opencode event inbox overflowed".to_string(),
    }
}

// ---- reading the stream ----

/// One read step's outcome. Every non-`Frames` variant ends the generation.
enum Read {
    Frames(Vec<Vec<u8>>),
    /// [`STALL_WINDOW`] elapsed with no bytes at all.
    Stall,
    /// The server closed the stream.
    Eof,
    /// A transport error, or an SSE event past [`MAX_SSE_FRAME_BYTES`].
    Failed(String),
}

impl Read {
    fn into_retry(self, worked: bool) -> GenEnd {
        match self {
            // Not reachable from a caller that matched `Frames` first, but the
            // exhaustive arm keeps that a compile-time fact.
            Read::Frames(_) => GenEnd::Retry {
                reason: "reconnect",
                worked,
                detail: "unexpected frames".to_string(),
            },
            Read::Stall => GenEnd::Retry {
                reason: "stall",
                worked,
                detail: format!("no data for {}s", STALL_WINDOW.as_secs()),
            },
            Read::Eof => GenEnd::Retry {
                reason: "reconnect",
                worked,
                detail: "the opencode event stream ended".to_string(),
            },
            Read::Failed(e) => GenEnd::Retry {
                reason: "reconnect",
                worked,
                detail: e,
            },
        }
    }
}

/// Reads ONE chunk and returns whatever complete SSE events it finished.
///
/// The stall timer lives here and is restarted per chunk, so ANY byte —
/// a comment ping, half a frame — counts as liveness. That is the only portable
/// keep-alive test against a server with no heartbeat event.
async fn read_step<S, B>(stream: &mut S, parser: &mut SseParser) -> Read
where
    S: Stream<Item = reqwest::Result<B>> + Unpin,
    B: AsRef<[u8]>,
{
    match tokio::time::timeout(STALL_WINDOW, stream.next()).await {
        Err(_) => Read::Stall,
        Ok(None) => Read::Eof,
        Ok(Some(Err(e))) => Read::Failed(format!("reading the opencode event stream: {e}")),
        Ok(Some(Ok(chunk))) => match parser.try_feed(chunk.as_ref()) {
            Err(overflow) => Read::Failed(overflow.to_string()),
            Ok(events) => Read::Frames(
                events
                    .into_iter()
                    .map(|e| e.data.into_bytes())
                    .filter(|d| !d.is_empty())
                    .collect(),
            ),
        },
    }
}

// ---- backoff ----

// The curve itself is `shed_core::lane::backoff` — the second lane adapter needs
// the identical one, and its floor/ceiling differ, which is why the shared half
// takes them as arguments. `jittered` is re-exported unchanged; `next_backoff`
// keeps this crate's two-argument spelling by binding opencode's own bounds, so
// every call site (and every test) below is untouched.
pub(crate) use shed_core::lane::backoff::jittered;

fn next_backoff(current: Duration, worked: bool) -> Duration {
    shed_core::lane::backoff::next_backoff(current, worked, OC_BACKOFF_BASE, OC_BACKOFF_MAX)
}

// ---- the frame peek ----

/// The three fields the ROUTER needs, decoded without disturbing the bytes the
/// fold will read. Tolerant throughout: an unparseable frame peeks as the
/// default, whose empty `sessionID` routes it to liveness-only.
#[derive(Debug, Default, Deserialize)]
struct Peek {
    #[serde(default, rename = "type", deserialize_with = "null_default")]
    typ: String,
    #[serde(default, deserialize_with = "object_default")]
    properties: PeekProps,
}

#[derive(Debug, Default, Deserialize)]
struct PeekProps {
    #[serde(default, rename = "sessionID", deserialize_with = "null_default")]
    session_id: String,
    #[serde(default, deserialize_with = "object_default")]
    info: PeekInfo,
}

#[derive(Debug, Default, Deserialize)]
struct PeekInfo {
    #[serde(default, deserialize_with = "null_default")]
    id: String,
    #[serde(default, rename = "parentID", deserialize_with = "null_default")]
    parent_id: String,
}

fn peek(raw: &[u8]) -> Peek {
    serde_json::from_slice(raw).unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::RestSessionTime;

    #[test]
    fn peek_reads_the_router_fields_and_tolerates_junk() {
        let pk = peek(br#"{"type":"session.idle","properties":{"sessionID":"ses_a"}}"#);
        assert_eq!(pk.typ, "session.idle");
        assert_eq!(pk.properties.session_id, "ses_a");

        let pk = peek(br#"{"type":"session.created","properties":{"info":{"id":"ses_c","parentID":"ses_a"}}}"#);
        assert_eq!(pk.properties.info.id, "ses_c");
        assert_eq!(pk.properties.info.parent_id, "ses_a");
        assert!(pk.properties.session_id.is_empty());

        // A ping, a null properties bag and outright junk all peek to the
        // liveness-only default rather than failing.
        assert!(peek(br#"{"type":"server.connected","properties":{}}"#)
            .properties
            .session_id
            .is_empty());
        assert!(peek(br#"{"type":"x","properties":null}"#).typ == "x");
        assert!(peek(b"not json").typ.is_empty());
    }

    #[test]
    fn approval_types_are_the_only_descendant_passthrough() {
        for typ in [
            "permission.asked",
            "permission.replied",
            "question.asked",
            "question.replied",
            "question.rejected",
        ] {
            assert!(is_approval_type(typ), "{typ} must reach the root's panel");
        }
        for typ in [
            "message.updated",
            "message.part.updated",
            "session.idle",
            "session.status",
            "session.error",
        ] {
            assert!(
                !is_approval_type(typ),
                "{typ} is the child's own transcript"
            );
        }
    }

    #[test]
    fn inbox_is_bounded_by_items_and_bytes() {
        let mut inbox = Inbox::new();
        for _ in 0..MAX_INBOX_ITEMS {
            assert!(inbox.push(vec![b'x']));
        }
        assert!(!inbox.push(vec![b'x']), "the item cap holds");
        assert_eq!(inbox.take().len(), MAX_INBOX_ITEMS);

        let mut inbox = Inbox::new();
        assert!(inbox.push(vec![0u8; MAX_INBOX_BYTES]));
        assert!(!inbox.push(vec![0u8; 1]), "the byte cap holds");
    }

    #[test]
    fn backoff_doubles_to_the_ceiling_and_resets_on_a_generation_that_worked() {
        let mut d = OC_BACKOFF_BASE;
        for _ in 0..20 {
            d = next_backoff(d, false);
        }
        assert_eq!(d, OC_BACKOFF_MAX);
        assert_eq!(next_backoff(d, true), OC_BACKOFF_BASE);
        assert_eq!(next_backoff(OC_BACKOFF_BASE, false), OC_BACKOFF_BASE * 2);

        // The curve itself now lives in `shed_core::lane::backoff`, and this
        // crate's two-argument spelling is that function bound to opencode's own
        // floor and ceiling — asserted, so the binding cannot quietly acquire a
        // second set of bounds.
        for current in [OC_BACKOFF_BASE, OC_BACKOFF_MAX, Duration::from_secs(3)] {
            for worked in [true, false] {
                assert_eq!(
                    next_backoff(current, worked),
                    shed_core::lane::backoff::next_backoff(
                        current,
                        worked,
                        OC_BACKOFF_BASE,
                        OC_BACKOFF_MAX
                    ),
                    "{current:?} worked={worked}",
                );
            }
        }
        assert!(std::ptr::fn_addr_eq(
            jittered as fn(Duration) -> Duration,
            shed_core::lane::backoff::jittered as fn(Duration) -> Duration,
        ));
    }

    #[test]
    fn jitter_stays_in_the_half_to_full_window() {
        for d in [OC_BACKOFF_BASE, OC_BACKOFF_MAX, Duration::from_secs(1)] {
            let j = jittered(d);
            assert!(j >= d / 2 && j <= d, "{j:?} outside [{:?}, {d:?}]", d / 2);
        }
    }

    /// A stream that never yields, on a PAUSED clock: tokio auto-advances to
    /// the deadline, so the real 30 s constant is asserted in milliseconds.
    #[tokio::test(start_paused = true)]
    async fn silence_stalls_after_the_window() {
        let mut stream = futures_util::stream::pending::<reqwest::Result<Vec<u8>>>();
        let mut parser = SseParser::new();
        assert!(
            matches!(read_step(&mut stream, &mut parser).await, Read::Stall),
            "a stream with nothing on it must stall, not hang"
        );
    }

    /// A comment ping carries no event — and MUST still count as liveness, or a
    /// healthy stream whose only traffic is keep-alives reconnects every 30 s
    /// forever. opencode has no `server.heartbeat` variant, so this is the only
    /// signal there is.
    #[tokio::test(start_paused = true)]
    async fn a_comment_ping_is_liveness_and_the_silence_after_it_stalls() {
        let first: Vec<reqwest::Result<Vec<u8>>> = vec![Ok(b": ping\n\n".to_vec())];
        let mut stream = futures_util::stream::iter(first).chain(futures_util::stream::pending());
        let mut parser = SseParser::new();

        match read_step(&mut stream, &mut parser).await {
            Read::Frames(frames) => assert!(frames.is_empty(), "a ping is not an event"),
            _ => panic!("the ping must be read as liveness, not as a stall"),
        }
        assert!(
            matches!(read_step(&mut stream, &mut parser).await, Read::Stall),
            "silence AFTER the ping stalls"
        );
    }

    #[test]
    fn a_stall_names_itself_on_the_next_reset() {
        match Read::Stall.into_retry(true) {
            GenEnd::Retry { reason, worked, .. } => {
                assert_eq!(reason, "stall");
                assert!(worked, "a generation that reached Ready resets the backoff");
            }
            other => panic!("a stall reconnects, it does not end the subscription: {other:?}"),
        }
        match Read::Eof.into_retry(false) {
            GenEnd::Retry { reason, .. } => assert_eq!(reason, "reconnect"),
            other => panic!("an EOF reconnects, not {other:?}"),
        }
        match retry_overflow() {
            GenEnd::Retry { reason, .. } => assert_eq!(reason, "overflow"),
            other => panic!("an overflow reconnects, it is not {other:?}"),
        }
    }

    /// The INBOX overflow and the CLIENT lag are different ends with different
    /// names, and neither may be spelled as the other: `"overflow"` says this
    /// crate lost frames it had not folded, `"lagged"` says the client lost
    /// frames it had been sent.
    #[test]
    fn a_dropped_frame_ends_the_generation_as_lagged_not_as_overflow() {
        assert!(matches!(lagged(), GenEnd::Lagged));
        let (tx, rx) = LanePublisher::channel();
        let w = offline_watcher(tx);
        for generation in 0..shed_core::lane::LANE_CHANNEL_CAPACITY as u64 {
            w.emit(LaneEvent::Ready { generation })
                .expect("everything inside the bound is published");
        }
        assert!(
            matches!(
                w.emit(LaneEvent::Ready { generation: 9_999 }),
                Err(GenEnd::Lagged)
            ),
            "the frame past the bound ends the generation"
        );
        // A CLOSED receiver is the other half of the rule: it is NOT a lag, and
        // the run loop's own `is_closed` check is what ends the subscription.
        drop(rx);
        assert!(
            w.emit(LaneEvent::Ready { generation: 1 }).is_ok(),
            "a closed channel is not a lag"
        );
    }

    /// An SSE event past the frame cap is a read ERROR, not a giant frame: the
    /// parser refuses to buffer it and the generation ends.
    #[tokio::test]
    async fn an_oversized_frame_ends_the_generation() {
        let huge = format!("data: {}\n\n", "x".repeat(MAX_SSE_FRAME_BYTES + 1));
        let chunks: Vec<reqwest::Result<Vec<u8>>> = vec![Ok(huge.into_bytes())];
        let mut stream = futures_util::stream::iter(chunks);
        let mut parser = SseParser::new().with_max_event_bytes(MAX_SSE_FRAME_BYTES);
        match read_step(&mut stream, &mut parser).await {
            Read::Failed(msg) => assert!(
                msg.contains(&MAX_SSE_FRAME_BYTES.to_string()),
                "the message names the cap: {msg}"
            ),
            _ => panic!("an oversized frame must end the read"),
        }
    }

    /// A watcher wired to a client that will never be called — every step this
    /// test drives is pure.
    fn offline_watcher(tx: LanePublisher) -> Watcher {
        Watcher {
            client: OpencodeClient::new("http://127.0.0.1:1/".parse().expect("a base url"), None)
                .expect("the client builds"),
            root: "ses_a".to_string(),
            directory: "/w".to_string(),
            tx,
            fold: OpencodeFold::new(),
            ring: MessageRing::new(),
            generation: 1,
            scope: HashSet::from(["ses_a".to_string()]),
            emitted_approvals: HashMap::new(),
            last_session: None,
            session_row: None,
        }
    }

    /// A seed whose `/permission` read found ONE open ask on the root.
    fn seed_with_a_pending_permission() -> Seed {
        Seed {
            session: RestSession {
                id: "ses_a".to_string(),
                title: "root".to_string(),
                directory: "/w".to_string(),
                parent_id: String::new(),
                time: RestSessionTime {
                    updated: 1_700_000_001_000,
                },
            },
            messages: Vec::new(),
            scope: HashSet::from(["ses_a".to_string()]),
            idle: true,
            permissions: Some(vec![RestPermission {
                id: "per_1".to_string(),
                session_id: "ses_a".to_string(),
                permission: "bash".to_string(),
                patterns: vec!["ls".to_string()],
                metadata: None,
            }]),
            questions: Some(Vec::new()),
        }
    }

    fn kind_of(ev: &LaneEvent) -> &'static str {
        match ev {
            LaneEvent::Reset { .. } => "reset",
            LaneEvent::Message { .. } => "message",
            LaneEvent::Session { .. } => "session",
            LaneEvent::Approval { .. } => "approval",
            LaneEvent::Ready { .. } => "ready",
            LaneEvent::Down { .. } => "down",
            // The contract's forward-compat arm; this watcher never mints one.
            _ => "unknown",
        }
    }

    /// The pinned seed order under the interleaving that breaks it: the REST
    /// seed finds a pending approval AND a root frame is buffered while the seed
    /// runs. Applying that frame with [`Emit::Now`] emits `Approval` before the
    /// first `Session`, and the closing sequence CANNOT repair it — both
    /// emitters deduplicate against what they already sent.
    ///
    /// Driven through the same calls, in the same order, `run_generation` makes:
    /// step 1, step 3, step 4's replay, step 5's closing sequence.
    #[test]
    fn a_frame_replayed_during_the_seed_never_jumps_ahead_of_the_session_row() {
        let (tx, mut rx) = LanePublisher::channel();
        let mut w = offline_watcher(tx);

        w.begin_generation("seed")
            .expect("an empty channel takes the Reset");
        w.apply_seed(seed_with_a_pending_permission())
            .expect("the seed fits");
        // Step 4: a root `session.status` that landed in the inbox while the
        // REST seed was in flight.
        w.apply_frame(
            br#"{"type":"session.status","properties":{"sessionID":"ses_a","status":{"type":"busy"}}}"#,
            Emit::Defer,
        )
        .expect("the replay fits");
        // Step 5.
        w.emit_session().expect("the session row fits");
        w.emit_approvals().expect("the approval fits");
        w.emit(LaneEvent::Ready { generation: 1 })
            .expect("the Ready fits");

        let mut kinds: Vec<&str> = Vec::new();
        while let Ok(ev) = rx.try_recv() {
            kinds.push(kind_of(&ev));
        }
        assert_eq!(kinds.first(), Some(&"reset"), "{kinds:?}");
        assert_eq!(kinds.last(), Some(&"ready"), "{kinds:?}");
        let session_at = kinds
            .iter()
            .position(|k| *k == "session")
            .unwrap_or_else(|| panic!("a Session frame: {kinds:?}"));
        let approval_at = kinds
            .iter()
            .position(|k| *k == "approval")
            .unwrap_or_else(|| panic!("an Approval frame: {kinds:?}"));
        assert!(
            session_at < approval_at,
            "messages, then session, then approvals — got {kinds:?}"
        );
        assert!(
            kinds[session_at..].iter().all(|k| *k != "message"),
            "every transcript row is ahead of the session row: {kinds:?}"
        );
    }

    /// The steady-state half of the same switch: after `Ready` there is no seed
    /// order to protect, and a frame's effects reach the client as it lands.
    #[test]
    fn a_frame_applied_in_steady_state_emits_its_effects_immediately() {
        let (tx, mut rx) = LanePublisher::channel();
        let mut w = offline_watcher(tx);
        w.begin_generation("seed")
            .expect("an empty channel takes the Reset");
        w.apply_seed(seed_with_a_pending_permission())
            .expect("the seed fits");
        // Drain the seed's own frames, then emit the closing sequence so the
        // dedup state matches a live subscription's.
        w.emit_session().expect("the session row fits");
        w.emit_approvals().expect("the approval fits");
        while rx.try_recv().is_ok() {}

        w.apply_frame(
            br#"{"type":"permission.replied","properties":{"sessionID":"ses_a","requestID":"per_1","reply":"once"}}"#,
            Emit::Now,
        )
        .expect("the reply fits");
        let mut kinds: Vec<&str> = Vec::new();
        while let Ok(ev) = rx.try_recv() {
            kinds.push(kind_of(&ev));
        }
        assert!(
            kinds.contains(&"approval"),
            "the resolution reaches the client without waiting for a Ready: {kinds:?}"
        );
    }

    #[test]
    fn synth_envelopes_preserve_the_producer_bytes() {
        let msgs = vec![RestMessage {
            info: Some(
                serde_json::value::RawValue::from_string(
                    r#"{"id":"msg_1","role":"assistant"}"#.to_string(),
                )
                .expect("raw info"),
            ),
            parts: vec![serde_json::value::RawValue::from_string(
                r#"{"id":"prt_1","type":"text","text":"hi"}"#.to_string(),
            )
            .expect("raw part")],
        }];
        let out = seed_message_envelopes("ses_a", &msgs);
        assert_eq!(out.len(), 2);
        let first = String::from_utf8(out[0].clone()).expect("utf8");
        assert_eq!(
            first,
            r#"{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_1","role":"assistant"}}}"#
        );
        let second = String::from_utf8(out[1].clone()).expect("utf8");
        assert!(second.contains(r#""part":{"id":"prt_1","type":"text","text":"hi"}"#));
    }
}
