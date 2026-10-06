//! [`CrazeSource`] — a machine's craze hub as the contract's MACHINE-level
//! half (plan 025 §3.3.3, P8, P11, P14): its sessions listed live, what a create
//! can start, the create, and (from C8) a lane opened on a row.
//!
//! Every call dials its OWN connection through the client's [`CrazeDial`]
//! (D10, P8): the hub answers one request at a time, in order (craze
//! `internal/hub/server.go:204-205` — craze#89 item 5, unstated in
//! protocol.md), so a create that can take ~80 s must never sit in front of
//! the roster, and the roster connection carries its subscription and nothing
//! else.
//!
//! # `subscribe`
//!
//! dial → hub `hello` (protocol 1, codecs 1/1, `rosterSubscribe` + `connect`;
//! `sessionCreate` and `createOptions` recorded into [`SourceCapabilities`]) →
//! `sessions.subscribe` → `Reset`, one `Session` per row, `Capabilities`,
//! `Ready{truncated}` → each `roster` notification's `upserts` as `Session` and
//! its `removes` — host ids, which ARE this source's row ids (P11) — as
//! `Removed`.
//!
//! - A roster `reset` other than `hub_closing` (`slow_consumer`, `omitted`, or
//!   one this build does not know) **resubscribes on the same connection** — a
//!   fresh `Reset … Ready`. `hub_closing`, EOF or a read error is
//!   `Offline{Unreachable}` and a redial with backoff ([`BACKOFF_BASE`] →
//!   [`BACKOFF_MAX`], `shed_core::lane::backoff`, back to the floor once a
//!   cycle reached `Ready`). A new hub — a new `epoch` — reseeds by
//!   construction: every connection seeds afresh.
//! - **Protocol faults are lost connections, never skipped notifications.** The
//!   subscription's `epoch` must be the `endpoint.hostId` the hub said `hello`
//!   with, and a `roster` notification that names an epoch must name the
//!   subscription's; a `roster` or `reset` that does not read, and a roster row
//!   with no `hostId`/`sessionId` to key it by, are faults too. Each is
//!   `Offline{Unreachable}`, a redial and a reseed — a skipped `removes` would
//!   leave a row listed for good. So is a connection the pump let fall
//!   [`crate::conn::NOTIFICATION_QUEUE`] notifications behind
//!   ([`crate::conn::ConnEnd::Backlog`]).
//! - **No stall timer** (craze SF-139): a quiet roster is legitimate (the hub
//!   flushes only on change), so silence is never read as a dead hub; only EOF
//!   or an error ends a cycle.
//! - Not installed or too old is `Offline{NotInstalled|TooOld}` and a SLOW
//!   retry (the ceiling), so installing craze lights the machine up without a
//!   restart. An `Offline` is said once per distinct cause and reason, not per
//!   attempt.
//! - The channel's overflow (module doc correction 13) ends the cycle with
//!   the connection dropped, waits for the client to drain holding nothing,
//!   backs off, and reseeds as `Reset{lagged} … Ready`.
//!
//! # `create_options` and `create`
//!
//! Each on its own short connection. `create` refuses a `request_id` that is not
//! craze's form ([`valid_request_id`]) before it dials, then sends
//! `session.create{cwd, provider?, prompt?, requestId}` and nothing else (D6)
//! under a [`CREATE_DEADLINE`] that covers the write as well as the reply (a
//! hub that stops reading mid-prompt is an unknown outcome, not a hang). **The retry rule** (craze joins a running duplicate
//! and replays the stored answer — failures included — for 10 minutes, across
//! a hub restart, PM "`session.create`"): a dropped connection or the deadline
//! is an UNKNOWN outcome, retried ONCE under the same `requestId` on a fresh
//! connection; a second unknown is [`errors::outcome_unknown`] and the caller
//! keeps the id ([`errors::is_outcome_unknown`]). Any DEFINITE answer — a
//! result, or any refusal, `unavailable` included — ends that id's life: the
//! caller's next submission mints a new one ([`new_request_id`]).
//!
//! # The row (P11)
//!
//! A row's id is its `hostId` — the key the roster is keyed by, and what the
//! lane's `session.connect` is given. The mapping is [`lane_session`].

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use shed_core::lane::backoff::{jittered, next_backoff};
use shed_core::lane::{
    AgentLane, AgentSource, LaneCreateOptions, LaneCreateRequest, LaneCreated, LaneError,
    LanePromptOutcome, LaneProvider, LaneProviderState, LaneSession, LaneStop, Publish,
    SourceCapabilities, SourceEvent, SourceOffline, SourcePublisher, SourceSubscription,
    MAX_SOURCE_ROWS,
};
use shed_core::rc::RcActivity;

use crate::conn::{CallError, Conn, HubHello, Notifications, HELLO_DEADLINE};
use crate::dial::{connect_hub, CrazeDial, DialError};
use crate::errors::{create_error, lane_error, outcome_unknown};
use crate::wire::{
    self, method, notify, ClientInfo, ConnCapabilities, CreateOptionsResult, CreateParams,
    CreateResult, Empty, ResetParams, RosterParams, RosterRow, SubscribeResult,
};

/// The agent token: [`AgentSource::kind`], `SourceCapabilities::kind` and
/// (from C8) `LaneCapabilities::kind`.
pub const KIND: &str = "craze";

/// The roster's reconnect floor.
pub const BACKOFF_BASE: Duration = Duration::from_millis(500);
/// The roster's reconnect ceiling — and the slow retry while craze is not
/// installed or too old.
pub const BACKOFF_MAX: Duration = Duration::from_secs(30);
/// A create's client deadline: the hub's own bounds are 60 s for the start,
/// 15 s for the first prompt's answer and 2 s for the row (~80 s, PM
/// "`session.create`"), and this sits past them, so a slow start is reported by
/// craze's refusal rather than by this deadline.
pub const CREATE_DEADLINE: Duration = Duration::from_secs(120);
/// Every other request's deadline (`sessions.subscribe`, which the hub answers
/// within a second; `sessions.createOptions`, a config read).
pub const REQUEST_DEADLINE: Duration = Duration::from_secs(30);

/// The source's clocks — the defaults are the pinned ones; a test shortens
/// them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Timings {
    /// How long a `hello` may take to be answered ([`HELLO_DEADLINE`]).
    pub hello: Duration,
    /// [`REQUEST_DEADLINE`].
    pub request: Duration,
    /// [`CREATE_DEADLINE`].
    pub create: Duration,
    /// [`BACKOFF_BASE`].
    pub backoff_base: Duration,
    /// [`BACKOFF_MAX`].
    pub backoff_max: Duration,
}

impl Default for Timings {
    fn default() -> Timings {
        Timings {
            hello: HELLO_DEADLINE,
            request: REQUEST_DEADLINE,
            create: CREATE_DEADLINE,
            backoff_base: BACKOFF_BASE,
            backoff_max: BACKOFF_MAX,
        }
    }
}

/// What a craze source can do, from its hub's `hello`.
pub fn source_capabilities(caps: &ConnCapabilities) -> SourceCapabilities {
    SourceCapabilities {
        kind: KIND.to_string(),
        create: caps.session_create,
        create_options: caps.create_options,
    }
}

/// A fresh create request id: `shed-` and 32 hex digits — inside craze's
/// `requestId` rule ([`valid_request_id`]). Minted after every DEFINITE
/// answer; reused only while an outcome is unknown.
pub fn new_request_id() -> String {
    format!("shed-{}", uuid::Uuid::new_v4().simple())
}

/// craze's `requestId` rule (PM "`session.create`"): 1–64 of
/// `[A-Za-z0-9._-]`. [`AgentSource::create`] refuses any other id itself,
/// before it dials — the strict side of an outbound request.
pub fn valid_request_id(id: &str) -> bool {
    (1..=64).contains(&id.len())
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
}

/// One machine's craze hub, reached through a client-supplied dial.
///
/// Cheap to clone; every clone dials through the same dialer.
#[derive(Clone)]
pub struct CrazeSource {
    dial: Arc<dyn CrazeDial>,
    client: ClientInfo,
    timings: Timings,
}

impl std::fmt::Debug for CrazeSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CrazeSource")
            .field("client", &self.client)
            .field("timings", &self.timings)
            .finish_non_exhaustive()
    }
}

impl CrazeSource {
    /// A source dialling through `dial`, saying `hello` as `{kind: "shed",
    /// name: client_name, version: <this crate's>}` (`"shed-desktop"`,
    /// `"shed-mobile"`).
    pub fn new(dial: Arc<dyn CrazeDial>, client_name: &str) -> CrazeSource {
        CrazeSource {
            dial,
            client: ClientInfo::shed(client_name),
            timings: Timings::default(),
        }
    }

    /// Run on other clocks (tests).
    pub fn with_timings(mut self, timings: Timings) -> CrazeSource {
        self.timings = timings;
        self
    }

    /// Who this source says it is.
    pub fn client(&self) -> &ClientInfo {
        &self.client
    }

    async fn connect(&self) -> Result<(Conn, Notifications, HubHello), DialError> {
        connect_hub(&*self.dial, &self.client, self.timings.hello).await
    }

    /// One create attempt. `retry` says an earlier attempt's outcome is
    /// unknown — so even a failure to reach craze leaves it unknown.
    async fn create_once(&self, params: &CreateParams, retry: bool) -> Attempt {
        let (conn, _notes, hello) = match self.connect().await {
            Ok(c) => c,
            Err(e) if retry => {
                return Attempt::Unknown(format!("the retry could not reach craze: {e}"))
            }
            Err(e) => return Attempt::Done(Err(e.into_lane_error())),
        };
        if !hello.capabilities.session_create {
            let why = "craze on this machine is too old to create sessions; update it";
            return if retry {
                Attempt::Unknown(format!("the retry reached a hub that cannot create: {why}"))
            } else {
                Attempt::Done(Err(LaneError::Failed(why.to_string())))
            };
        }
        match conn
            .request(method::SESSION_CREATE, params, self.timings.create)
            .await
        {
            Ok(result) => Attempt::Done(created(result)),
            Err(CallError::Refused(e)) => Attempt::Done(Err(create_error(&e))),
            Err(e) if e.outcome_unknown() => Attempt::Unknown(e.to_string()),
            Err(e) => Attempt::Done(Err(LaneError::Failed(e.to_string()))),
        }
    }
}

/// What one create attempt came to.
///
/// `Done` carries a whole created row and dwarfs `Unknown` (clippy's
/// `large_enum_variant`); one is built per create attempt, at most two per
/// create, so boxing would buy nothing.
#[allow(clippy::large_enum_variant)]
enum Attempt {
    /// A definite answer: the id's life is over.
    Done(Result<LaneCreated, LaneError>),
    /// Sent, and no answer: whether it ran is unknown.
    Unknown(String),
}

/// `session.create`'s result as the contract's.
fn created(result: serde_json::Value) -> Result<LaneCreated, LaneError> {
    let r: CreateResult = serde_json::from_value(result).map_err(|e| {
        LaneError::Failed(format!(
            "craze answered session.create with something shed cannot read: {e}"
        ))
    })?;
    let row = RosterRow::from_value(&r.session).map_err(|e| {
        LaneError::Failed(format!(
            "craze created a session and answered with a row shed cannot read ({e})"
        ))
    })?;
    Ok(LaneCreated {
        session: lane_session(&row),
        prompt: LanePromptOutcome::from_wire(r.prompt.as_deref().unwrap_or("none")),
        prompt_error: r.prompt_error,
    })
}

/// A read-only call's failure as the contract's.
fn read_error(what: &str, e: CallError) -> LaneError {
    match e {
        CallError::Refused(refusal) => lane_error(&refusal),
        e if e.outcome_unknown() => LaneError::Unavailable(format!("{what}: {e}")),
        e => LaneError::Failed(format!("{what}: {e}")),
    }
}

/// `sessions.createOptions`' result as the contract's: providers in craze's
/// order (D5: a client dims, never hides), the default as craze states it, and
/// the recent directories newest first.
pub fn lane_create_options(r: &CreateOptionsResult) -> LaneCreateOptions {
    LaneCreateOptions {
        providers: r
            .providers
            .iter()
            .map(|p| LaneProvider {
                id: p.id.clone(),
                label: if p.label.is_empty() {
                    p.id.clone()
                } else {
                    p.label.clone()
                },
                state: LaneProviderState::from_wire(&p.state),
                reason: p.reason.clone(),
                fix: p.fix.clone(),
            })
            .collect(),
        default_provider: r.default_provider.clone(),
        recent_dirs: r.recent_dirs.iter().map(|d| d.dir.clone()).collect(),
    }
}

/// A roster row as the contract's [`LaneSession`] (plan 025 §3.3.3):
///
/// - `id` = `hostId` (P11); `title` = the row's title, else the workspace's
///   basename; `cwd` = `host.workspace`; `provider` = `host.provider`;
/// - the row facts when present: `model`, `doing`, `last_reply`, `since` (→
///   `since_unix_ms`), `start_error` (`startErr` when `startFailed`),
///   `head_ask_summary` (`headAsk.summary`, else its label), `attached`,
///   `provider_session_id` (`""` → `None`), and the info document's
///   `permission_mode`;
/// - `pending_approvals` = `pendingAsks`; `approximate` = the roster row's
///   own, or a status other than `reachable`, or a row that did not read;
///   `last_change_unix_ms` = `since`, else `host.startedAt`;
/// - **activity:** `pendingAsks > 0` → `NeedsApproval` (correction 6's
///   override); else `working` or `foreignTurn` → `Working`; `idle` → `Idle`;
///   `starting`/`replaying` → `Working`; `error`/`closing`, an unknown word, a
///   row-less or an `unreachable` row → `Unknown`.
///
/// A source never sets `tab_id` (the conformance kit checks it).
pub fn lane_session(r: &RosterRow) -> LaneSession {
    let cwd = r.host.workspace.clone();
    let row = r.row.as_ref();
    let some = |s: Option<&String>| s.filter(|v| !v.trim().is_empty()).cloned();
    let title = some(row.and_then(|w| w.title.as_ref())).unwrap_or_else(|| {
        Path::new(&cwd)
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(|| cwd.clone())
    });
    let pending = row.map_or(0, |w| w.pending_asks);
    let activity = match row {
        _ if pending > 0 => RcActivity::NeedsApproval,
        None => RcActivity::Unknown,
        Some(_) if r.status == "unreachable" => RcActivity::Unknown,
        Some(w) if w.foreign_turn => RcActivity::Working,
        Some(w) => match w.activity.as_deref() {
            Some("working" | "starting" | "replaying") => RcActivity::Working,
            Some("idle") => RcActivity::Idle,
            _ => RcActivity::Unknown,
        },
    };
    let since = row
        .and_then(|w| w.since.as_deref())
        .and_then(shed_core::time::rfc3339_unix_ms);
    let started = r
        .host
        .started_at
        .as_deref()
        .and_then(shed_core::time::rfc3339_unix_ms);
    let start_error = row.filter(|w| w.start_failed).map(|w| {
        some(w.start_err.as_ref())
            .unwrap_or_else(|| crate::errors::START_FAILED_FALLBACK.to_string())
    });
    LaneSession {
        id: r.host_id.clone(),
        title,
        cwd,
        activity,
        pending_approvals: pending,
        approximate: r.approximate || r.status != "reachable" || r.malformed,
        parent_id: None,
        last_change_unix_ms: since.or(started),
        provider: some(Some(&r.host.provider)),
        model: some(row.and_then(|w| w.model.as_ref())),
        doing: some(row.and_then(|w| w.doing.as_ref())),
        head_ask_summary: row
            .and_then(|w| w.head_ask.as_ref())
            .and_then(|h| some(h.summary.as_ref()).or_else(|| some(Some(&h.label)))),
        last_reply: some(row.and_then(|w| w.last_reply.as_ref())),
        since_unix_ms: since,
        attached: row.and_then(|w| w.attached),
        start_error,
        provider_session_id: some(row.and_then(|w| w.provider_session_id.as_ref())),
        permission_mode: some(row.and_then(|w| w.permission_mode.as_ref())),
        tab_id: None,
    }
}

#[async_trait::async_trait]
impl AgentSource for CrazeSource {
    fn kind(&self) -> &str {
        KIND
    }

    /// Starts the roster pump and returns at once — a machine with no craze,
    /// or no reachable hub, is the stream's [`SourceEvent::Offline`], never this
    /// call's error.
    async fn subscribe(&self) -> Result<SourceSubscription, LaneError> {
        let (tx, rx) = SourcePublisher::channel();
        let pump = RosterPump {
            source: self.clone(),
            tx,
            generation: 0,
            offline: None,
        };
        let task = tokio::spawn(pump.run());
        Ok(SourceSubscription {
            rx,
            stop: LaneStop::new(task),
        })
    }

    /// `sessions.createOptions`, on its own connection, where the hub's
    /// `hello` says `createOptions`.
    async fn create_options(&self) -> Result<LaneCreateOptions, LaneError> {
        let (conn, _notes, hello) = self.connect().await.map_err(DialError::into_lane_error)?;
        if !hello.capabilities.create_options {
            return Err(LaneError::Failed(
                "craze on this machine is too old to offer providers; update it".to_string(),
            ));
        }
        let result = conn
            .request(
                method::SESSIONS_CREATE_OPTIONS,
                &Empty {},
                self.timings.request,
            )
            .await
            .map_err(|e| read_error("sessions.createOptions", e))?;
        let r: CreateOptionsResult = serde_json::from_value(result).map_err(|e| {
            LaneError::Failed(format!(
                "craze answered sessions.createOptions with something shed cannot read: {e}"
            ))
        })?;
        Ok(lane_create_options(&r))
    }

    /// `session.create`, on its own connection, retried once under the same
    /// `requestId` when its outcome is unknown (the module doc).
    async fn create(&self, request: LaneCreateRequest) -> Result<LaneCreated, LaneError> {
        if !valid_request_id(&request.request_id) {
            return Err(LaneError::BadRequest(format!(
                "request id {:?} is not craze's form (1–64 of [A-Za-z0-9._-]); mint one with new_request_id",
                request.request_id
            )));
        }
        let params = CreateParams {
            cwd: request.cwd,
            prompt: request.prompt,
            provider: request.provider,
            request_id: request.request_id,
        };
        match self.create_once(&params, false).await {
            Attempt::Done(answer) => answer,
            Attempt::Unknown(_) => match self.create_once(&params, true).await {
                Attempt::Done(answer) => answer,
                Attempt::Unknown(why) => Err(outcome_unknown(&format!(
                    "the connection to craze dropped twice during the create ({why}); it may have started a session — check the session list, or try again with the same request id, which craze answers with that session"
                ))),
            },
        }
    }

    /// The craze lane is plan 025 C8; this build lists and creates craze
    /// sessions, and opens none.
    async fn open(&self, _session_id: &str) -> Result<Arc<dyn AgentLane>, LaneError> {
        Err(LaneError::Failed(
            "the craze lane arrives in plan 025 C8: this build lists and creates craze sessions, and does not open them yet".to_string(),
        ))
    }
}

// ---- the roster pump ----

/// How one cycle — one connection — ended.
enum CycleEnd {
    /// The subscriber is gone.
    Closed,
    /// A frame was dropped: the generation ends here (correction 13).
    Lagged,
    /// The connection is gone, or never came: say `Offline` and redial.
    Lost {
        cause: SourceOffline,
        reason: String,
    },
}

/// A protocol fault on the roster connection: the connection is not to be
/// trusted further, so it is a lost one — `Offline`, a redial, a reseed.
fn fault(reason: String) -> CycleEnd {
    CycleEnd::Lost {
        cause: SourceOffline::Unreachable,
        reason,
    }
}

/// Every row of a roster answer or notification, or the first that is not a
/// roster row (no `hostId`/`sessionId` to key it by) as a protocol fault. A
/// host-written `row` inside one that does not read is NOT a fault — that row
/// keeps its host half and reads `approximate` ([`RosterRow::from_value`]).
fn roster_rows(values: &[serde_json::Value]) -> Result<Vec<RosterRow>, CycleEnd> {
    values
        .iter()
        .map(|v| RosterRow::from_value(v).map_err(|e| fault(format!("craze sent {e}"))))
        .collect()
}

/// Why a publish stopped short.
enum End {
    Closed,
    Lagged,
}

impl From<End> for CycleEnd {
    fn from(end: End) -> CycleEnd {
        match end {
            End::Closed => CycleEnd::Closed,
            End::Lagged => CycleEnd::Lagged,
        }
    }
}

struct RosterPump {
    source: CrazeSource,
    tx: SourcePublisher,
    /// The last generation started.
    generation: u64,
    /// The last `Offline` said, so a repeat says nothing new.
    offline: Option<(SourceOffline, String)>,
}

impl RosterPump {
    async fn run(mut self) {
        let timings = self.source.timings;
        let mut backoff = timings.backoff_base;
        let mut reason = "connect";
        loop {
            if self.tx.is_closed() {
                return;
            }
            let (end, worked) = self.cycle(reason).await;
            match end {
                CycleEnd::Closed => return,
                CycleEnd::Lagged => {
                    // The connection is already dropped: wait for the client
                    // to drain holding nothing, then reseed after a backoff.
                    if !self.drained().await {
                        return;
                    }
                    backoff =
                        next_backoff(backoff, false, timings.backoff_base, timings.backoff_max);
                    reason = "lagged";
                }
                CycleEnd::Lost { cause, reason: why } => {
                    let slow = matches!(cause, SourceOffline::NotInstalled | SourceOffline::TooOld);
                    match self.say_offline(cause, why) {
                        Ok(()) => {}
                        Err(End::Closed) => return,
                        Err(End::Lagged) => {
                            if !self.drained().await {
                                return;
                            }
                        }
                    }
                    backoff = if slow {
                        timings.backoff_max
                    } else {
                        next_backoff(backoff, worked, timings.backoff_base, timings.backoff_max)
                    };
                    reason = "reconnect";
                }
            }
            tokio::time::sleep(jittered(backoff)).await;
        }
    }

    fn emit(&self, ev: SourceEvent) -> Result<(), End> {
        match self.tx.publish(ev) {
            Publish::Sent => Ok(()),
            Publish::Closed => Err(End::Closed),
            Publish::Lagged => Err(End::Lagged),
        }
    }

    /// After a dropped frame: wait for the client to drain, and forget the
    /// last `Offline`, so a cause that still holds is said again to the client
    /// that may have lost the frame saying it. `false`: the subscriber is gone.
    async fn drained(&mut self) -> bool {
        if self.tx.wait_drained().await.is_err() {
            return false;
        }
        self.offline = None;
        true
    }

    fn say_offline(&mut self, cause: SourceOffline, reason: String) -> Result<(), End> {
        let said = (cause, reason);
        if self.offline.as_ref() == Some(&said) {
            return Ok(());
        }
        let (cause, reason) = said.clone();
        self.emit(SourceEvent::Offline { reason, cause })?;
        self.offline = Some(said);
        Ok(())
    }

    /// One connection: dial, `hello`, then subscribe — again on the same
    /// connection after every roster `reset` but `hub_closing` — until the
    /// connection ends. The bool says whether a seed reached `Ready`.
    async fn cycle(&mut self, first_reason: &str) -> (CycleEnd, bool) {
        let (conn, mut notes, hello) = match self.source.connect().await {
            Ok(c) => c,
            Err(e) => {
                return (
                    CycleEnd::Lost {
                        cause: e.cause(),
                        reason: e.to_string(),
                    },
                    false,
                )
            }
        };
        let caps = source_capabilities(&hello.capabilities);
        let mut reached = false;
        let mut reason = first_reason.to_string();
        loop {
            let sub = match self.subscribe_on(&conn).await {
                Ok(sub) => sub,
                Err(lost) => return (lost, reached),
            };
            // The roster is the hub's that said hello, or nothing on this
            // connection can be trusted.
            if sub.epoch != hello.epoch {
                return (
                    fault(format!(
                        "craze's roster names epoch {:?}, but its hub said hello as {:?}",
                        sub.epoch, hello.epoch
                    )),
                    reached,
                );
            }
            let rows = match roster_rows(&sub.sessions) {
                Ok(rows) => rows,
                Err(lost) => return (lost, reached),
            };
            match self.seed(&rows, sub.truncated, &caps, &reason) {
                Ok(()) => {}
                Err(end) => return (end.into(), reached),
            }
            reached = true;
            self.offline = None;
            match self
                .follow(&conn, &mut notes, &sub.subscription, &sub.epoch)
                .await
            {
                Follow::Resubscribe(why) => reason = format!("server_reset:{why}"),
                Follow::End(end) => return (end, reached),
            }
        }
    }

    async fn subscribe_on(&self, conn: &Conn) -> Result<SubscribeResult, CycleEnd> {
        let lost = |cause, reason| CycleEnd::Lost { cause, reason };
        match conn
            .request(
                method::SESSIONS_SUBSCRIBE,
                &Empty {},
                self.source.timings.request,
            )
            .await
        {
            Ok(v) => serde_json::from_value(v).map_err(|e| {
                lost(
                    SourceOffline::Failed,
                    format!(
                        "craze answered sessions.subscribe with something shed cannot read: {e}"
                    ),
                )
            }),
            Err(CallError::Refused(e)) => Err(lost(
                SourceOffline::Failed,
                format!("craze refused sessions.subscribe: {e}"),
            )),
            Err(e) => Err(lost(SourceOffline::Unreachable, e.to_string())),
        }
    }

    /// `Reset`, the rows (at most [`MAX_SOURCE_ROWS`]), `Capabilities`,
    /// `Ready` — one burst that fits the channel.
    fn seed(
        &mut self,
        rows: &[RosterRow],
        truncated: bool,
        caps: &SourceCapabilities,
        reason: &str,
    ) -> Result<(), End> {
        self.generation += 1;
        let generation = self.generation;
        self.emit(SourceEvent::Reset {
            reason: reason.to_string(),
            generation,
        })?;
        let truncated = truncated || rows.len() > MAX_SOURCE_ROWS;
        for row in rows.iter().take(MAX_SOURCE_ROWS) {
            self.emit(SourceEvent::Session {
                session: lane_session(row),
            })?;
        }
        self.emit(SourceEvent::Capabilities {
            capabilities: caps.clone(),
        })?;
        self.emit(SourceEvent::Ready {
            generation,
            truncated,
        })
    }

    /// The live roster on one subscription, until it is reset or the
    /// connection ends. A `roster` or `reset` that does not read, a roster row
    /// with no key, or a roster naming another epoch is a protocol fault — the
    /// connection is lost, never a notification silently skipped (a skipped
    /// remove would leave a row listed for good). A connection that ended
    /// because this pump fell [`crate::conn::NOTIFICATION_QUEUE`] notifications
    /// behind ([`crate::conn::ConnEnd::Backlog`]) is lost the same way: redial
    /// and reseed.
    async fn follow(
        &self,
        conn: &Conn,
        notes: &mut Notifications,
        subscription: &str,
        epoch: &str,
    ) -> Follow {
        loop {
            let Some(note) = notes.recv().await else {
                let why = conn.ended().map_or_else(
                    || "the connection to craze's hub closed".to_string(),
                    |e| e.to_string(),
                );
                return Follow::End(fault(why));
            };
            match note.method.as_str() {
                notify::ROSTER => {
                    let p = match serde_json::from_value::<RosterParams>(note.params) {
                        Ok(p) => p,
                        Err(e) => {
                            return Follow::End(fault(format!(
                                "craze sent a roster notification shed cannot read: {e}"
                            )))
                        }
                    };
                    if p.subscription != subscription {
                        continue;
                    }
                    if let Some(theirs) = p.epoch.as_deref().filter(|e| *e != epoch) {
                        return Follow::End(fault(format!(
                            "a roster notification names epoch {theirs:?}, but this subscription's is {epoch:?}"
                        )));
                    }
                    let rows = match roster_rows(&p.upserts) {
                        Ok(rows) => rows,
                        Err(lost) => return Follow::End(lost),
                    };
                    if let Err(end) = self.apply(&rows, &p.removes) {
                        return Follow::End(end.into());
                    }
                }
                notify::RESET => {
                    let p = match serde_json::from_value::<ResetParams>(note.params) {
                        Ok(p) => p,
                        Err(e) => {
                            return Follow::End(fault(format!(
                                "craze sent a reset notification shed cannot read: {e}"
                            )))
                        }
                    };
                    if p.subscription != subscription {
                        continue;
                    }
                    if p.reason == wire::reset::HUB_CLOSING {
                        return Follow::End(fault("craze's hub is shutting down".to_string()));
                    }
                    // slow_consumer, omitted — and a reason this build does
                    // not know: subscribe again here. A connection that is
                    // going away fails that request, and the cycle ends then.
                    return Follow::Resubscribe(p.reason);
                }
                _ => {}
            }
        }
    }

    /// One `roster` notification: upserts as `Session`, removes — host ids —
    /// as `Removed`.
    fn apply(&self, upserts: &[RosterRow], removes: &[String]) -> Result<(), End> {
        for row in upserts {
            self.emit(SourceEvent::Session {
                session: lane_session(row),
            })?;
        }
        for host_id in removes {
            self.emit(SourceEvent::Removed {
                session_id: host_id.clone(),
            })?;
        }
        Ok(())
    }
}

enum Follow {
    Resubscribe(String),
    End(CycleEnd),
}

#[cfg(test)]
mod tests;
