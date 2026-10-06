//! [`OpencodeSource`] — opencode's MACHINE-level half of the contract (plan 025
//! P7, §3.2.6): one opencode server's sessions, listed, created and opened.
//!
//! `shed_core::lane` splits an agent into a source and a session-scoped lane
//! (D3), and opencode implements both because D3 says every adapter does. It is
//! small on purpose:
//!
//! - **`subscribe`** is the old `sessions()` read — `GET /session` plus a
//!   `/session/status` read per distinct directory — POLLED every
//!   [`SOURCE_POLL_INTERVAL`] and diffed into `Session`/`Removed`. opencode has
//!   no session-list stream to subscribe to, and a poll is honest about it: the
//!   rows are `approximate: true` (correction 7). A poll that fails emits ONLY
//!   [`SourceEvent::Offline`] with [`SourceOffline::Unreachable`] — the last
//!   `Ready` view stays the client's — and the first poll that succeeds after it
//!   reseeds the whole list as `Reset … Ready`. A seed carries at most
//!   [`shed_core::lane::MAX_SOURCE_ROWS`] rows (the first 512 by id), so it
//!   always fits the client channel, and says `Ready { truncated: true }` when
//!   it cut any; a poll whose list crosses the cap reseeds, so `truncated`
//!   never goes stale.
//! - **`create_options`** is one implicit provider, `opencode`, always ready:
//!   opencode IS the provider. No default, no recent directories.
//! - **`create`** is the old create made prompt-optional: `POST /session`, then
//!   `prompt_async` only when there is a prompt, and a send that fails is the
//!   create's error. `request_id` is accepted and unused — opencode has no
//!   create idempotency to hand it to.
//! - **`open`** binds a session id to an [`OpencodeLane`]; no I/O.
//!
//! **No client uses this source's `subscribe` or `create` in plan 025**: an
//! opencode row is still roost's (the tab's `agent_lane` stamp), and creating an
//! opencode session is still a roost tab. The desktop opens its lanes THROUGH a
//! source (`OpencodeSource::new(url, None).open(id)`) so that every client
//! reaches every adapter the same way.
//!
//! # The overflow, on this level too
//!
//! The source publishes through a [`shed_core::lane::SourcePublisher`] — the same
//! bounded channel and the same policy as a lane (module doc, correction 13). A
//! publish that lags ends the round where it is; the poller waits for the client
//! to drain (holding nothing — a poll is not a connection), takes the ordinary
//! failure backoff ([`crate::watcher::OC_BACKOFF_BASE`] →
//! [`crate::watcher::OC_BACKOFF_MAX`], reset once a seed reaches its `Ready`),
//! and reseeds as `Reset { reason: "lagged" } … Ready`. A consumer draining one
//! frame at a time therefore costs a poll per backoff step, never a poll per
//! frame.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use shed_core::lane::{
    AgentLane, AgentSource, LaneCreateOptions, LaneCreateRequest, LaneCreated, LaneError,
    LanePromptOutcome, LaneProvider, LaneProviderState, LaneSession, LaneStop, Publish, SendMode,
    SourceCapabilities, SourceEvent, SourceOffline, SourcePublisher, SourceSubscription,
    MAX_SOURCE_ROWS,
};
use shed_core::rc::RcActivity;

use crate::client::{lane_session, BasicAuth, OpencodeClient, OpencodeLane, KIND};
use crate::watcher::{jittered, next_backoff, OC_BACKOFF_BASE};

/// How often the source re-reads the session list. opencode has no list stream,
/// and a sessions panel tolerates five seconds where a transcript would not.
pub const SOURCE_POLL_INTERVAL: Duration = Duration::from_secs(5);

/// The one provider an opencode server offers: itself.
const PROVIDER: &str = KIND;

/// What an opencode source can do, as it rides every seed: it creates, and it
/// answers create options (trivially).
pub fn opencode_source_capabilities() -> SourceCapabilities {
    SourceCapabilities {
        kind: KIND.to_string(),
        create: true,
        create_options: true,
    }
}

/// One opencode server's sessions — [`AgentSource`] over its HTTP API.
///
/// Cheap to clone, and every lane it opens shares its transport (two `Arc`'d
/// reqwest clients).
#[derive(Debug, Clone)]
pub struct OpencodeSource {
    client: OpencodeClient,
    poll: Duration,
}

impl OpencodeSource {
    /// A source for the opencode server at `server_url`. `auth` is the
    /// credential the CLIENT supplies (correction 5) — `None` against a server
    /// that set no `OPENCODE_SERVER_PASSWORD`.
    ///
    /// Fails only if reqwest cannot build its client stack — a process-level
    /// problem, hence [`LaneError::Failed`].
    pub fn new(
        server_url: reqwest::Url,
        auth: Option<BasicAuth>,
    ) -> Result<OpencodeSource, LaneError> {
        Ok(OpencodeSource {
            client: OpencodeClient::new(server_url, auth)?,
            poll: SOURCE_POLL_INTERVAL,
        })
    }

    /// Poll every `every` instead of every [`SOURCE_POLL_INTERVAL`]. Tests use
    /// it to watch a poll cycle in milliseconds; a client could use it to poll
    /// less often on battery.
    pub fn with_poll_interval(mut self, every: Duration) -> OpencodeSource {
        self.poll = every;
        self
    }

    /// The server this source talks to.
    pub fn server_url(&self) -> &reqwest::Url {
        self.client.base_url()
    }

    /// The concrete lane [`AgentSource::open`] hands back behind an `Arc`, for
    /// a caller that wants the type. Binding only; no I/O.
    pub fn lane(&self, session_id: &str) -> OpencodeLane {
        OpencodeLane::new(self.client.clone(), session_id.to_string())
    }
}

#[async_trait::async_trait]
impl AgentSource for OpencodeSource {
    fn kind(&self) -> &str {
        KIND
    }

    /// Starts the poller and returns at once — an unreachable server is the
    /// stream's [`SourceEvent::Offline`], not this call's error.
    async fn subscribe(&self) -> Result<SourceSubscription, LaneError> {
        let (tx, rx) = SourcePublisher::channel();
        let poller = Poller {
            client: self.client.clone(),
            poll: self.poll,
            tx,
            generation: 0,
            rows: BTreeMap::new(),
            offline: false,
            truncated: false,
        };
        let task = tokio::spawn(poller.run());
        Ok(SourceSubscription {
            rx,
            stop: LaneStop::new(task),
        })
    }

    async fn create_options(&self) -> Result<LaneCreateOptions, LaneError> {
        Ok(LaneCreateOptions {
            providers: vec![LaneProvider {
                id: PROVIDER.to_string(),
                label: PROVIDER.to_string(),
                state: LaneProviderState::Ready,
                reason: None,
                fix: None,
            }],
            default_provider: None,
            recent_dirs: Vec::new(),
        })
    }

    /// `POST /session?directory=<cwd>`, then — only with a prompt —
    /// `prompt_async`.
    ///
    /// A provider other than opencode's one is refused up front rather than
    /// ignored: creating an opencode session for a request that named another
    /// agent would be the wrong thing done quietly. `request_id` is accepted and
    /// unused (the module doc). A send that fails after the session was created
    /// is the create's error, exactly as before the split; the session it
    /// created stays on the server, where the next poll lists it.
    async fn create(&self, request: LaneCreateRequest) -> Result<LaneCreated, LaneError> {
        if let Some(provider) = request.provider.as_deref() {
            if provider != PROVIDER {
                return Err(LaneError::BadRequest(format!(
                    "opencode offers one provider, {PROVIDER:?}; {provider:?} is not it"
                )));
            }
        }
        let created = self.client.create_session(&request.cwd).await?;
        let (activity, prompt) = match request.prompt.as_deref() {
            None => (RcActivity::Idle, LanePromptOutcome::None),
            Some(text) => {
                self.client.send(&created.id, text, SendMode::Queue).await?;
                // `Working` is asserted rather than polled: a prompt was just
                // accepted, and a `/session/status` read this instant would race
                // the runner and report idle. `approximate: true` says so.
                (RcActivity::Working, LanePromptOutcome::Accepted)
            }
        };
        Ok(LaneCreated {
            session: lane_session(&created, activity, 0, true),
            prompt,
            prompt_error: None,
        })
    }

    async fn open(&self, session_id: &str) -> Result<Arc<dyn AgentLane>, LaneError> {
        Ok(Arc::new(self.lane(session_id)))
    }
}

// ---- the poller ----

/// The subscriber is gone, or a frame was dropped: the two ways a round stops
/// early.
enum End {
    Closed,
    Lagged,
}

struct Poller {
    client: OpencodeClient,
    poll: Duration,
    tx: SourcePublisher,
    /// The last generation STARTED.
    generation: u64,
    /// The list as the client's last `Ready` view plus every change published
    /// since — what the next poll is diffed against.
    rows: BTreeMap<String, LaneSession>,
    /// An `Offline` was published and no seed has replaced it yet, so a further
    /// failed poll says nothing new.
    offline: bool,
    /// The last seed was cut to [`MAX_SOURCE_ROWS`]. A diff cannot say that
    /// changed, so a poll whose list crosses the cap — either way — reseeds.
    truncated: bool,
}

impl Poller {
    async fn run(mut self) {
        let mut need_seed = true;
        let mut reason: &'static str = "seed";
        let mut backoff = OC_BACKOFF_BASE;
        loop {
            if self.tx.is_closed() {
                return;
            }
            let round = match self.client.sessions().await.map(capped) {
                // ONLY `Offline`, and only once per outage: no `Reset`, no
                // `Removed` — the client keeps its last `Ready` view.
                Err(e) => {
                    need_seed = true;
                    if self.generation > 0 {
                        reason = "reconnect";
                    }
                    if self.offline {
                        Ok(())
                    } else {
                        self.offline = true;
                        self.emit(SourceEvent::Offline {
                            reason: e.to_string(),
                            cause: SourceOffline::Unreachable,
                        })
                    }
                }
                Ok((rows, truncated)) if need_seed || truncated != self.truncated => {
                    if !need_seed {
                        reason = "truncated";
                    }
                    let seeded = self.seed(rows, truncated, reason);
                    if seeded.is_ok() {
                        need_seed = false;
                        self.offline = false;
                        // A seed that reached its `Ready` worked: the lag curve
                        // starts over.
                        backoff = OC_BACKOFF_BASE;
                    }
                    seeded
                }
                Ok((rows, _)) => self.diff(rows),
            };
            match round {
                Ok(()) => tokio::time::sleep(self.poll).await,
                Err(End::Closed) => return,
                // Correction 13: the round ends at the first dropped frame. Wait
                // for the client to drain, back off, and reseed — at once, not a
                // poll interval later. `offline` is forgotten so that a server
                // still down announces it again to the client that just lost the
                // frame saying so.
                Err(End::Lagged) => {
                    if self.tx.wait_drained().await.is_err() {
                        return;
                    }
                    need_seed = true;
                    reason = "lagged";
                    self.offline = false;
                    backoff = next_backoff(backoff, false);
                    tokio::time::sleep(jittered(backoff)).await;
                }
            }
        }
    }

    fn emit(&self, ev: SourceEvent) -> Result<(), End> {
        match self.tx.publish(ev) {
            Publish::Sent => Ok(()),
            Publish::Closed => Err(End::Closed),
            Publish::Lagged => Err(End::Lagged),
        }
    }

    /// `Reset`, every row, the capabilities, `Ready` — a whole list, already
    /// cut to [`MAX_SOURCE_ROWS`] by [`capped`] so the burst fits the channel.
    fn seed(
        &mut self,
        rows: BTreeMap<String, LaneSession>,
        truncated: bool,
        reason: &str,
    ) -> Result<(), End> {
        self.generation += 1;
        let generation = self.generation;
        self.emit(SourceEvent::Reset {
            reason: reason.to_string(),
            generation,
        })?;
        for row in rows.values() {
            self.emit(SourceEvent::Session {
                session: row.clone(),
            })?;
        }
        self.emit(SourceEvent::Capabilities {
            capabilities: opencode_source_capabilities(),
        })?;
        self.emit(SourceEvent::Ready {
            generation,
            truncated,
        })?;
        self.rows = rows;
        self.truncated = truncated;
        Ok(())
    }

    /// Between seeds: a `Session` for every row that is new or changed, a
    /// `Removed` for every row that left. In id order, so two polls that saw the
    /// same change say it the same way.
    fn diff(&mut self, next: BTreeMap<String, LaneSession>) -> Result<(), End> {
        for (id, row) in &next {
            if self.rows.get(id) != Some(row) {
                self.emit(SourceEvent::Session {
                    session: row.clone(),
                })?;
            }
        }
        for id in self.rows.keys().filter(|id| !next.contains_key(*id)) {
            self.emit(SourceEvent::Removed {
                session_id: id.clone(),
            })?;
        }
        self.rows = next;
        Ok(())
    }
}

/// A poll's rows keyed by id — in id order, which is the order a seed and a
/// diff say them in — cut to the first [`MAX_SOURCE_ROWS`] of that order, and
/// whether anything was cut.
///
/// The cap is what lets a seed fit the client channel (`shed_core::lane`'s
/// module doc, "Sources"): an opencode server with a thousand sessions would
/// otherwise publish a seed that lags at the same frame on every attempt and
/// never reaches its `Ready`. Cutting by id keeps the visible set stable from
/// one poll to the next, so a diff between seeds does not churn rows in and out
/// of the cut.
fn capped(rows: Vec<LaneSession>) -> (BTreeMap<String, LaneSession>, bool) {
    let mut by_id: BTreeMap<String, LaneSession> =
        rows.into_iter().map(|r| (r.id.clone(), r)).collect();
    let truncated = by_id.len() > MAX_SOURCE_ROWS;
    while by_id.len() > MAX_SOURCE_ROWS {
        by_id.pop_last();
    }
    (by_id, truncated)
}
