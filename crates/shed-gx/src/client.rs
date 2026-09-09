//! The HTTP client — the credential pin, gx's `/v1` routes, and the
//! [`AgentLane`] implementation on top of them.
//!
//! # Two URLs, and which one each thing uses
//!
//! - the **reported** URL is what [`GxCredentialSource::discover`] matches a
//!   discovery record against. It is a `String`, verbatim as roost reported it,
//!   and it is never parsed into a `Url` here — that is what makes conflating it
//!   with the dial URL impossible rather than merely discouraged.
//! - the **dial** URL comes from [`GxTransport::dial`] and is where every
//!   request — `healthz` included — actually goes.
//!
//! # The pin, and what opens a new epoch
//!
//! gx's token is per-`$GROK_HOME` and shared by every leader on it; a leader
//! restart changes the `instanceId` and not the token. So a token alone proves
//! nothing about WHICH leader is answering on a port — and over an SSH forward
//! the port is a local one that could, in principle, have been re-reserved onto
//! something else entirely.
//!
//! [`GxClient::ensure_pinned`] is the gate: **no bearer request leaves this
//! adapter** until the token-free `GET /v1/healthz` on the dial URL has answered
//! an `instanceId` matching discovery's. It is one serialized async gate, so
//! concurrent first callers share a single pin rather than racing two health
//! checks. A new epoch — and therefore a fresh pin — is opened by:
//!
//! - a `dial()` that answers a different URL (a re-established forward);
//! - any [`LaneError::Unavailable`] (a dial failure, a timeout, the leader gone);
//! - a [`LaneError::Unauthorized`], which does not retry the failed request but
//!   does mean the next one re-discovers.
//!
//! The residual, stated because it cannot be closed from here: one round trip
//! separates the health check from the request it authorized, so a leader that
//! restarts inside that window receives one bearer request pinned to its
//! predecessor. gx answers that with `unauthorized` or `unknown_session`, both
//! of which are already handled.
//!
//! # Error mapping is code-driven
//!
//! gx answers `{"error":"<code>","message":"…"}` on every failure, with eight
//! stable codes. [`map_gx_error`] branches on the CODE, not the status: a caller
//! branches on a [`LaneError`] variant and must never string-match, and the code
//! is the only part gx promises is stable. Anything without a readable envelope
//! is [`LaneError::Failed`] carrying the status and a bounded head of the body —
//! including a bare 401 from something that is not gx, which is a loud residue
//! rather than a quiet "the agent wants credentials".

use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use serde_json::{json, Value};

use shed_core::lane::ring::MessageRing;
use shed_core::lane::{
    option_kind, AgentLane, LaneAnswer, LaneApproval, LaneApprovalKind, LaneApprovalStatus,
    LaneCapabilities, LaneDecision, LaneError, LaneHistory, LaneSession, LaneSubscription,
    SendMode,
};
use shed_core::rc::RcActivity;

use crate::discovery::{redact_hex64, GxCredentialSource, GxToken};
use crate::fold::{
    cursor_index, lane_approval, min_counter, null_default, EventId, GxApprovalResource,
    GxEnvelope, GxFold,
};
use crate::transport::GxTransport;

/// The REST client's total-request bound.
///
/// Three times `shed-opencode`'s 5 s, and the reason is the reach: opencode is
/// always a process on this machine, while a gx dial URL is frequently the near
/// end of an `ssh -N -L` forward to another host. A verb that has not answered
/// in 15 s is not going to, but 5 s would turn an ordinary slow link into a
/// stream of spurious `Unavailable`s.
pub const REST_TIMEOUT: Duration = Duration::from_secs(15);
/// The SSE client's CONNECT bound — the WATCHER's (plan 017 C3), spelled here
/// so the adapter's whole timing surface reads in one place.
///
/// There is deliberately no request timeout beside it: the SSE body is
/// long-lived, and a client-level request timeout would cut it mid-session —
/// which looks like nothing at all, because the stream just ends, the watcher
/// reconnects, and the transcript resets every N seconds forever. Liveness on
/// that stream is [`GxTimings::stall`], not a timeout here.
pub const STREAM_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// How long the SSE client waits for the things on that connection that are NOT
/// the long-lived body — the response head, and the error body of a non-2xx. A
/// peer that completes the TCP handshake and then says nothing satisfies
/// `connect_timeout` and would otherwise park the watcher forever, after its
/// `Reset` and before there is any body for the stall timer to watch.
pub const STREAM_HEAD_TIMEOUT: Duration = Duration::from_secs(10);

/// How many envelopes one backwards history page asks for while hunting a
/// cursor.
pub const HISTORY_PAGE_SIZE: u32 = 200;
/// How many such pages before the hunt gives up and reports `truncated`.
/// Ten pages is 2,000 envelopes — gx's own per-session ring — so a cursor older
/// than this is one the server could not have replayed either.
pub const MAX_HISTORY_PAGES: usize = 10;

/// The tunable windows, as constructor options so a test never waits out a real
/// one.
///
/// Every field is plan 017 §3.4's; the defaults are its numbers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GxTimings {
    /// How long a stream may be silent — keepalives included — before the
    /// watcher treats it as dead.
    pub stall: Duration,
    /// How long after the first loss a silent cursor resume may still be
    /// attempted.
    pub resume_window: Duration,
    /// How many silent resumes inside that window before escalating to a
    /// reseed.
    pub resume_tries: u32,
    /// How long an open streak may be silent before the watcher flushes it as a
    /// partial row.
    pub flush_after: Duration,
    /// How many envelopes a seed asks for.
    pub seed_limit: u32,
    /// The cap on one REST response body.
    pub rest_cap: usize,
    /// How long reseeds may keep failing before the subscription ends in
    /// `Down`.
    pub down_after: Duration,
}

impl Default for GxTimings {
    fn default() -> GxTimings {
        GxTimings {
            stall: Duration::from_secs(30),
            resume_window: Duration::from_secs(30),
            resume_tries: 3,
            flush_after: Duration::from_secs(2),
            seed_limit: 500,
            rest_cap: 8 << 20,
            down_after: Duration::from_secs(60),
        }
    }
}

/// One pinned transport epoch: the dial URL it was pinned against, the leader
/// instance that answered `healthz`, and the token cleared to talk to it.
///
/// The three travel TOGETHER on purpose. A request uses `epoch.dial` and
/// `epoch.token` as one unit rather than re-reading the URL from the transport,
/// so there is no window in which a request could be issued against a URL other
/// than the one its pin was established for.
#[derive(Debug, Clone)]
struct Epoch {
    dial: reqwest::Url,
    instance_id: String,
    token: GxToken,
    /// Which epoch this is, counting from 1. It exists so a failing request can
    /// invalidate **its own** epoch and not a fresher one another caller
    /// established while it was in flight — otherwise a slow failure would keep
    /// unpinning good epochs and the client would health-check forever.
    generation: u64,
}

/// The pin gate's contents. The counter lives under the same lock as the epoch
/// so a generation can never be handed out twice.
#[derive(Debug, Default)]
struct PinState {
    epoch: Option<Epoch>,
    generations: u64,
}

/// The gx adapter.
///
/// Cheap to clone — the reqwest clients are `Arc` inside, and the pin is shared
/// on purpose: a clone handed to the pump task is the SAME adapter, and a second
/// health check from it would defeat the gate.
#[derive(Clone)]
pub struct GxClient {
    reported_url: String,
    transport: Arc<dyn GxTransport>,
    credentials: Arc<dyn GxCredentialSource>,
    timings: GxTimings,
    rest: reqwest::Client,
    pin: Arc<tokio::sync::Mutex<PinState>>,
}

/// Hand-written so a token can never reach a log through a derived `Debug`.
///
/// [`GxToken`]'s own `Debug` already redacts, but the credential SOURCE is a
/// trait object whose implementation this crate does not control — a Tauri
/// reader holding a cache of tokens would print them if this derived. So the
/// source is named and not printed.
impl std::fmt::Debug for GxClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("GxClient")
            .field("reported_url", &self.reported_url)
            .field("timings", &self.timings)
            .field("credentials", &"<GxCredentialSource>")
            .field("transport", &"<GxTransport>")
            .finish()
    }
}

impl GxClient {
    /// Build an adapter for the gx lane REPORTED at `reported_url`, dialling
    /// through `transport` and authenticating with whatever `credentials`
    /// discovers.
    ///
    /// Fails only if reqwest cannot build its connector stack, which is a
    /// process-level problem rather than a per-call one — hence
    /// [`LaneError::Failed`] rather than `Unavailable`.
    pub fn new(
        reported_url: impl Into<String>,
        transport: Arc<dyn GxTransport>,
        credentials: Arc<dyn GxCredentialSource>,
        timings: GxTimings,
    ) -> Result<GxClient, LaneError> {
        // `.no_proxy()` for the reason opencode has it: the lane is on loopback
        // (or the near end of a forward), and an inherited `http_proxy` would
        // send every call to a proxy that cannot reach it.
        let rest = reqwest::Client::builder()
            .no_proxy()
            .timeout(REST_TIMEOUT)
            .build()
            .map_err(|e| failed(format!("building the gx REST client: {e}")))?;
        Ok(GxClient {
            reported_url: reported_url.into(),
            transport,
            credentials,
            timings,
            rest,
            pin: Arc::new(tokio::sync::Mutex::new(PinState::default())),
        })
    }

    /// The URL this lane is REPORTED at — what an `Unavailable` names, and what
    /// a discovery record is matched against. Never dialled.
    pub fn reported_url(&self) -> &str {
        &self.reported_url
    }

    pub fn timings(&self) -> &GxTimings {
        &self.timings
    }

    /// The leader instance this adapter is currently pinned to, if any — what
    /// a diagnostic reads, and what a test asserts a rotation against.
    pub async fn pinned_instance(&self) -> Option<String> {
        self.pin
            .lock()
            .await
            .epoch
            .as_ref()
            .map(|e| e.instance_id.clone())
    }

    // ---- the pin ----

    /// Resolve the dial URL and return an epoch pinned for **that exact URL**,
    /// establishing one if there is not already a matching one.
    ///
    /// `dial()` runs on every invocation, and therefore before every request.
    /// §3.3 asks for it before every connect attempt; doing it every time is
    /// strictly more often, needs no bookkeeping to be right, and is what makes
    /// a forward that moved detectable at the moment it moves rather than at the
    /// next failure.
    ///
    /// **`dial()` runs INSIDE the gate**, and that placement is load-bearing
    /// rather than incidental. Dialling outside it leaves two races that both
    /// end with a bearer request under a pin that was never established for the
    /// URL it is using:
    ///
    /// - caller A reads the old URL, caller B re-pins for a URL that has since
    ///   moved, and A then overwrites the fresh pin with one for the obsolete
    ///   URL;
    /// - the URL A checked against the pin is not necessarily the URL A goes on
    ///   to use, because nothing holds it still in between.
    ///
    /// Inside the gate, whatever `dial()` answers is the current truth at that
    /// instant, and the [`Epoch`] the caller receives carries the URL and the
    /// token together — so the request cannot drift off the pin it was granted.
    ///
    /// The lock is held ACROSS the health check, which is also what makes
    /// concurrent first callers share one pin: the second caller blocks, then
    /// finds the epoch already established and sends no health check of its own.
    async fn ensure_pinned(&self) -> Result<Epoch, LaneError> {
        let mut st = self.pin.lock().await;
        let dial = match self.transport.dial().await {
            Ok(dial) => dial,
            Err(e) => {
                // A transport that cannot answer is a new epoch by definition:
                // whatever comes back may not be what the old pin described.
                st.epoch = None;
                return Err(e);
            }
        };
        if let Some(epoch) = st.epoch.as_ref() {
            if epoch.dial == dial {
                return Ok(epoch.clone());
            }
        }
        // A changed dial URL is a new epoch: the old pin says nothing about
        // whatever is listening on the new one.
        st.epoch = None;
        st.generations += 1;
        let generation = st.generations;
        // On failure the epoch stays `None` — already cleared above — so the
        // next call re-dials, re-discovers and re-checks rather than inheriting
        // a refusal.
        let epoch = self.pin_epoch(dial, generation).await?;
        st.epoch = Some(epoch.clone());
        Ok(epoch)
    }

    /// Drop the pin **if it is still the one `generation` names**.
    ///
    /// The generation check is what keeps a slow failure from unpinning a
    /// fresher epoch that another caller established while the failing request
    /// was in flight. Invalidating unconditionally would let one dead request
    /// force a health check on every healthy caller behind it.
    async fn invalidate(&self, generation: u64) {
        let mut st = self.pin.lock().await;
        if st
            .epoch
            .as_ref()
            .is_some_and(|e| e.generation == generation)
        {
            st.epoch = None;
        }
    }

    /// Discover, health-check, compare — and on a mismatch, rediscover ONCE.
    ///
    /// One retry and no more: a leader that restarted between the client's last
    /// discovery and now has written a new record, so re-reading it is the fix.
    /// A second mismatch means the record and the port disagree about who is
    /// there, which is exactly the state where sending a token would be wrong.
    async fn pin_epoch(&self, dial: reqwest::Url, generation: u64) -> Result<Epoch, LaneError> {
        let first = self.credentials.discover(&self.reported_url).await?;
        let health = self.healthz(&dial).await?;
        if health.instance_id == first.instance_id && !health.instance_id.is_empty() {
            return Ok(Epoch {
                dial,
                instance_id: health.instance_id,
                token: first.token,
                generation,
            });
        }
        let second = self.credentials.discover(&self.reported_url).await?;
        if health.instance_id == second.instance_id && !health.instance_id.is_empty() {
            return Ok(Epoch {
                dial,
                instance_id: health.instance_id,
                token: second.token,
                generation,
            });
        }
        // The epoch stays UNPINNED: the next call discovers again rather than
        // inheriting a refusal.
        //
        // The message names the REPORTED url and no identifiers: an instance id
        // is not a secret, but a message a user reads should say what to do
        // about it, and "which leader" is not actionable.
        Err(unavailable(format!(
            "gx lane instance changed at {}",
            self.reported_url
        )))
    }

    /// `GET /v1/healthz` — gx's one unauthenticated route, and the only request
    /// this adapter ever makes without a bearer.
    async fn healthz(&self, dial: &reqwest::Url) -> Result<GxHealth, LaneError> {
        let url = join(dial, "/v1/healthz")?;
        let resp = self
            .rest
            .get(url)
            .send()
            .await
            .map_err(|e| dial_error("/v1/healthz", &e))?;
        let body = self.check_status("/v1/healthz", resp).await?;
        serde_json::from_str(&body).map_err(|e| failed(format!("decoding /v1/healthz: {e}")))
    }

    // ---- request plumbing ----

    async fn rest_get(&self, path: &str, query: &[(&str, String)]) -> Result<String, LaneError> {
        self.bearer_request(reqwest::Method::GET, path, query, None)
            .await
    }

    async fn rest_post(&self, path: &str, body: Option<Value>) -> Result<String, LaneError> {
        self.bearer_request(reqwest::Method::POST, path, &[], body)
            .await
    }

    /// **The one path every bearer request takes**, and the whole of §0's
    /// invariant 2 in one function: dial, pin, send — and invalidate the epoch
    /// on ANY failure of that request.
    ///
    /// It exists as a single wrapper because the invariant is not "invalidate in
    /// these four places". Four call sites meant four chances to return early
    /// past the invalidation, and that is exactly what happened: a `?` on the
    /// pin and a `?` on `.send()` both bypassed it, so a transport failure left
    /// the PRIOR epoch pinned. The next call then matched the same dial URL,
    /// skipped `healthz` entirely, and sent the token into what was effectively
    /// a new epoch. Over a forwarded `127.0.0.1:<local>` — where a leader
    /// restart or a re-pointed tunnel changes what answers while the URL string
    /// stays identical — the `instanceId` pin is the ONLY thing that notices.
    ///
    /// Which errors invalidate, and why not all of them:
    ///
    /// - [`LaneError::Unavailable`] — a dial failure, a send failure, a body
    ///   that stalled or EOF'd, or gx's own `leader_unavailable`. Every one of
    ///   them means the thing on the far side may not be the thing that was
    ///   pinned.
    /// - [`LaneError::Unauthorized`] — the credential is wrong for whoever is
    ///   answering, which is the pin being stale by another name. The failed
    ///   request is NOT retried; the next one re-discovers.
    /// - Everything else is left pinned. `NotAccepting`, `UnknownSession`,
    ///   `BadRequest` and friends are a coherent answer FROM the pinned leader —
    ///   evidence the pin is good, not that it is stale — and re-checking on
    ///   every 409 would health-check the lane to death.
    async fn bearer_request(
        &self,
        method: reqwest::Method,
        path: &str,
        query: &[(&str, String)],
        body: Option<Value>,
    ) -> Result<String, LaneError> {
        // A dial failure here has already invalidated inside the gate.
        let epoch = self.ensure_pinned().await?;
        let out = self.send_under(&epoch, method, path, query, body).await;
        if matches!(
            out,
            Err(LaneError::Unavailable(_)) | Err(LaneError::Unauthorized)
        ) {
            self.invalidate(epoch.generation).await;
        }
        out
    }

    /// One request, issued strictly under `epoch` — its URL and its token,
    /// never a freshly-read one. Every failure it can produce is returned to
    /// [`GxClient::bearer_request`], which decides what it means for the pin.
    async fn send_under(
        &self,
        epoch: &Epoch,
        method: reqwest::Method,
        path: &str,
        query: &[(&str, String)],
        body: Option<Value>,
    ) -> Result<String, LaneError> {
        let mut url = join(&epoch.dial, path)?;
        if !query.is_empty() {
            let mut q = url.query_pairs_mut();
            for (k, v) in query {
                q.append_pair(k, v);
            }
        }
        let mut rb = self.bearer(self.rest.request(method, url), &epoch.token)?;
        if let Some(b) = body {
            rb = rb.json(&b);
        }
        let resp = rb.send().await.map_err(|e| dial_error(path, &e))?;
        self.check_status(path, resp).await
    }

    /// Attach the bearer. The header value is built by [`GxToken::authorization`]
    /// and is marked **sensitive**, which is what keeps it out of reqwest's and
    /// hyper's own tracing.
    fn bearer(
        &self,
        rb: reqwest::RequestBuilder,
        token: &GxToken,
    ) -> Result<reqwest::RequestBuilder, LaneError> {
        let value = token.authorization().ok_or_else(|| {
            // Unreachable for a token that passed `GxToken::parse` (64 hex
            // digits are all legal header bytes). Reported without the value.
            failed("the gx token is not a usable header value")
        })?;
        Ok(rb.header(reqwest::header::AUTHORIZATION, value))
    }

    /// Reads the body under [`GxTimings::rest_cap`] and applies the error
    /// table.
    async fn check_status(&self, path: &str, resp: reqwest::Response) -> Result<String, LaneError> {
        let status = resp.status();
        match read_body_capped(path, resp, self.timings.rest_cap).await {
            Ok(body) if status.is_success() => Ok(body),
            Ok(body) => Err(map_gx_error(status, path, &body)),
            // The body could not be read, on a 2xx or a non-2xx alike, and that
            // failure is PRESERVED rather than replaced by a status-derived
            // `Failed`.
            //
            // Two reasons. The error mapping is code-driven — a status with no
            // readable envelope cannot say WHICH gx code it was, so `Failed`
            // there is a guess dressed as a verdict. And the read failure is
            // transport-level (`Unavailable`), which is what invalidates the
            // epoch; discarding it left a stalled or EOF'd body looking like an
            // ordinary application error and quietly kept a stale pin.
            Err(e) => Err(e),
        }
    }

    // ---- typed routes ----

    async fn rest_sessions(&self) -> Result<Vec<GxSessionRow>, LaneError> {
        let body = self.rest_get("/v1/sessions", &[]).await?;
        let page: GxSessionsPage = serde_json::from_str(&body)
            .map_err(|e| failed(format!("decoding /v1/sessions: {e}")))?;
        Ok(page.sessions)
    }

    async fn rest_session(&self, id: &str) -> Result<GxSessionRow, LaneError> {
        let path = session_path(id, &[]);
        let body = self.rest_get(&path, &[]).await?;
        serde_json::from_str(&body).map_err(|e| failed(format!("decoding {path}: {e}")))
    }

    /// One `GET …/history?offset&limit` page.
    pub(crate) async fn rest_history(
        &self,
        id: &str,
        offset: i64,
        limit: u32,
    ) -> Result<GxHistoryPage, LaneError> {
        let path = session_path(id, &["history"]);
        let body = self
            .rest_get(
                &path,
                &[("offset", offset.to_string()), ("limit", limit.to_string())],
            )
            .await?;
        serde_json::from_str(&body).map_err(|e| failed(format!("decoding {path}: {e}")))
    }

    /// The newest `limit` envelopes, with gx's own `hasMore` and page cursor.
    ///
    /// Both of `history`'s branches ask for the tail — the no-cursor case and
    /// the cursor-not-located fallback — so the negative-offset convention is
    /// stated here once instead of twice.
    async fn history_tail(
        &self,
        id: &str,
        limit: u32,
    ) -> Result<(Vec<GxEnvelope>, bool, Option<String>), LaneError> {
        let n = limit.max(1);
        let page = self.rest_history(id, -(n as i64), n).await?;
        Ok((page.updates, page.has_more, page.last_event_id))
    }

    pub(crate) async fn rest_approvals(
        &self,
        id: &str,
    ) -> Result<Vec<GxApprovalResource>, LaneError> {
        let path = session_path(id, &["approvals"]);
        let body = self.rest_get(&path, &[]).await?;
        let page: GxApprovalsPage =
            serde_json::from_str(&body).map_err(|e| failed(format!("decoding {path}: {e}")))?;
        Ok(page.approvals)
    }

    /// `GET …/approvals/{toolCallId}` — the authoritative request, re-read
    /// immediately before an answer is translated.
    ///
    /// **The response's identity is verified before it is used.** The whole
    /// point of the re-read is race-safety: the option set that gets translated
    /// comes from THIS response, and the chosen `optionId` is then POSTed to the
    /// approval the caller named. If the two are not the same approval, the
    /// client would answer one request with another's option — a resource for a
    /// different tool call, or one belonging to a different session, and neither
    /// is something a caller could detect afterwards.
    ///
    /// So a mismatched `id` or `sessionId` is [`LaneError::Failed`] and nothing
    /// is posted. `Failed` rather than a quiet variant because a server that
    /// answers an id-addressed GET with a different resource is not a state to
    /// render stale — it is a server this adapter cannot reason about.
    async fn rest_approval(&self, id: &str, approval_id: &str) -> Result<LaneApproval, LaneError> {
        let path = session_path(id, &["approvals", &encode_segment(approval_id)]);
        let body = self.rest_get(&path, &[]).await?;
        let res: GxApprovalResource =
            serde_json::from_str(&body).map_err(|e| failed(format!("decoding {path}: {e}")))?;
        if res.id != approval_id {
            return Err(failed(format!(
                "gx {path}: answered with approval {}, not the one that was asked for",
                res.id
            )));
        }
        // An empty `sessionId` is a producer that does not stamp it rather than
        // a mismatch; the id check above is what carries the guarantee then.
        if !res.session_id.is_empty() && res.session_id != id {
            return Err(failed(format!(
                "gx {path}: approval {approval_id} belongs to session {}, not the one being answered",
                res.session_id
            )));
        }
        Ok(lane_approval(&res))
    }
}

// ---------------------------------------------------------------------------
// wire DTOs
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Deserialize)]
struct GxHealth {
    #[serde(default, rename = "instanceId", deserialize_with = "null_default")]
    instance_id: String,
}

#[derive(Debug, Default, Deserialize)]
struct GxSessionsPage {
    #[serde(default, deserialize_with = "null_default")]
    sessions: Vec<GxSessionRow>,
}

#[derive(Debug, Default, Deserialize)]
struct GxApprovalsPage {
    #[serde(default, deserialize_with = "null_default")]
    approvals: Vec<GxApprovalResource>,
}

/// One row of `GET /v1/sessions`, or the whole body of `GET /v1/sessions/{id}`.
#[derive(Debug, Default, Clone, Deserialize)]
pub struct GxSessionRow {
    #[serde(default, rename = "sessionId", deserialize_with = "null_default")]
    pub session_id: String,
    /// **Nullable on the real wire** — an untitled session carries
    /// `"title": null`.
    #[serde(default, deserialize_with = "null_default")]
    pub title: String,
    #[serde(default, deserialize_with = "null_default")]
    pub cwd: String,
    /// One of gx's six: `working|needs_input|idle|completed|dormant|dead`.
    #[serde(default, deserialize_with = "null_default")]
    pub activity: String,
    #[serde(
        default,
        rename = "pendingApprovals",
        deserialize_with = "null_default"
    )]
    pub pending_approvals: u32,
    /// gx's own meaning: `true` while `pendingApprovals` is INFERRED from
    /// activity rather than counted, which is the case for any session the lane
    /// has not attached. Passed through unchanged — gx is the first producer to
    /// report `false`, and it earns it per row rather than per producer.
    #[serde(default, deserialize_with = "null_default")]
    pub approximate: bool,
    #[serde(default, rename = "lastChangeUnixMs")]
    pub last_change_unix_ms: Option<i64>,
}

/// `GET …/history`'s body.
#[derive(Debug, Default, Deserialize)]
pub struct GxHistoryPage {
    #[serde(default, deserialize_with = "null_default")]
    pub updates: Vec<GxEnvelope>,
    #[serde(default, rename = "totalCount", deserialize_with = "null_default")]
    pub total_count: i64,
    #[serde(default, rename = "hasMore", deserialize_with = "null_default")]
    pub has_more: bool,
    /// The newest id in the RETURNED PAGE, `null` when no line in it carried
    /// one.
    #[serde(default, rename = "lastEventId")]
    pub last_event_id: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
struct GxCreated {
    #[serde(default, rename = "sessionId", deserialize_with = "null_default")]
    session_id: String,
}

// ---------------------------------------------------------------------------
// session rows
// ---------------------------------------------------------------------------

/// gx's six activity states, mapped ([`shed_core::lane`], correction 6).
///
/// `needs_input` splits on whether anything is actually pending: gx reports it
/// for "an interaction is open", and a client's blocked badge is
/// [`RcActivity::NeedsApproval`]'s job. `dead` and any state this build has
/// never heard of collapse to [`RcActivity::Unknown`] rather than to a claim.
pub fn activity_from(state: &str, pending_approvals: u32) -> RcActivity {
    match state {
        "working" => RcActivity::Working,
        "needs_input" => {
            if pending_approvals > 0 {
                RcActivity::NeedsApproval
            } else {
                RcActivity::NeedsInput
            }
        }
        "idle" | "completed" | "dormant" => RcActivity::Idle,
        _ => RcActivity::Unknown,
    }
}

/// A gx row as the contract sees it.
///
/// `held_pending` is the fold's own count of unanswered approvals, which
/// **overrides upward**: a lane holding an approval the roster poll has not
/// caught up to reports [`RcActivity::NeedsApproval`] and the larger count. It
/// never overrides downward — the fold seeing nothing does not disprove what gx
/// reports, and a lane that has only just attached has seen nothing yet.
pub fn lane_session(row: &GxSessionRow, held_pending: u32) -> LaneSession {
    let pending = row.pending_approvals.max(held_pending);
    let activity = if held_pending > 0 {
        RcActivity::NeedsApproval
    } else {
        activity_from(&row.activity, pending)
    };
    LaneSession {
        id: row.session_id.clone(),
        title: row.title.clone(),
        cwd: row.cwd.clone(),
        activity,
        pending_approvals: pending,
        approximate: row.approximate,
        // gx's sessions are flat: the lane has no sub-session tree, so a row
        // never has a parent.
        parent_id: None,
        last_change_unix_ms: row.last_change_unix_ms.filter(|v| *v > 0),
    }
}

// ---------------------------------------------------------------------------
// answers
// ---------------------------------------------------------------------------

/// Why a [`LaneDecision`] could not be resolved against what gx offered.
///
/// [`LaneApproval::option_for`] answers `None` for two different reasons and a
/// caller cannot tell them apart, so the message is rebuilt here from the option
/// list. They call for different responses: "no such option" means this agent
/// does not support that decision at all, while "two of them" means the human
/// has to pick one by name — which is what [`LaneAnswer::Choice`] is for, and
/// what the panel sends anyway.
fn unresolvable_decision(approval: &LaneApproval, decision: LaneDecision) -> LaneError {
    let wanted = match decision {
        LaneDecision::AllowOnce => option_kind::ALLOW_ONCE,
        LaneDecision::AllowAlways => option_kind::ALLOW_ALWAYS,
        LaneDecision::Reject => option_kind::REJECT_ONCE,
    };
    let matching: Vec<&str> = approval
        .options
        .iter()
        .filter(|o| o.kind.as_deref() == Some(wanted))
        .map(|o| o.id.as_str())
        .collect();
    if matching.len() > 1 {
        return bad_request(format!(
            "gx offered {} options of kind {wanted} ({}) — answer with the \
             option's id instead, because a {decision:?} cannot say which one \
             you meant",
            matching.len(),
            matching.join(", "),
        ));
    }
    bad_request(format!(
        "gx offered no option of kind {wanted} on this approval"
    ))
}

/// Translate a contract answer into gx's `response` value, against the
/// **authoritative** approval just re-read from the server.
///
/// Pure and public so the whole table is testable without a wire, and so the
/// harness can assert the exact body a decision produces.
///
/// The rules, each of which exists because guessing would post a decision the
/// human did not make:
///
/// - [`LaneAnswer::Permission`] resolves through
///   [`LaneApproval::option_for`] — the contract's single implementation of the
///   by-kind rule, so gx, opencode and the Dart mirror cannot each re-derive the
///   `Reject` fallback differently. No match is [`LaneError::BadRequest`], and
///   so is an AMBIGUOUS one: a real gx permission offers TWO options declaring
///   `allow_once` (the second of which disables prompting for the whole
///   session), and a three-valued decision cannot say which the human meant.
///   The message names which of the two it was, because "gx offered no
///   allow_once option" and "gx offered two of them" call for different
///   responses from whoever reads it.
/// - [`LaneAnswer::Choice`] must name an id the approval OFFERED. Never the
///   nearest match.
/// - [`LaneAnswer::Question`] is positional on the way in and keyed on the way
///   out, by the question's TEXT.
/// - [`LaneAnswer::Raw`] is verbatim, and must be JSON.
pub fn answer_body(approval: &LaneApproval, answer: &LaneAnswer) -> Result<Value, LaneError> {
    match answer {
        LaneAnswer::Permission { decision } => {
            let opt = approval
                .option_for(*decision)
                .ok_or_else(|| unresolvable_decision(approval, *decision))?;
            Ok(json!({ "outcome": { "outcome": "selected", "optionId": opt.id } }))
        }
        LaneAnswer::Choice { option_id } => match &approval.kind {
            LaneApprovalKind::Permission => {
                let offered = approval.options.iter().any(|o| &o.id == option_id);
                if !offered {
                    return Err(bad_request(format!(
                        "gx did not offer the option {option_id} on this approval"
                    )));
                }
                Ok(json!({ "outcome": { "outcome": "selected", "optionId": option_id } }))
            }
            // A plan approval takes a BARE string outcome, which is why its two
            // synthesized ids ARE the outcomes. Echoed rather than re-spelled,
            // so that identity is expressed in the code and not only in this
            // comment; anything else still falls through to the refusal below.
            LaneApprovalKind::PlanApproval
                if matches!(option_id.as_str(), "approved" | "cancelled") =>
            {
                Ok(json!({ "outcome": option_id }))
            }
            _ => Err(bad_request(format!(
                "gx does not take a choice of {option_id} on a {} approval",
                approval.kind.as_str()
            ))),
        },
        LaneAnswer::Question { answers } => {
            if answers.len() > approval.questions.len() {
                return Err(bad_request(format!(
                    "the approval has {} questions; {} answers were given",
                    approval.questions.len(),
                    answers.len()
                )));
            }
            let mut map = serde_json::Map::new();
            for (q, chosen) in approval.questions.iter().zip(answers.iter()) {
                // `id` is the question's text — see `fold::lane_question`.
                let key = q.id.clone().unwrap_or_else(|| q.question.clone());
                if key.is_empty() {
                    return Err(LaneError::BadRequest(
                        "gx files answers by the question's text, and this question has none"
                            .to_string(),
                    ));
                }
                map.insert(key, json!(chosen));
            }
            Ok(json!({ "outcome": "accepted", "answers": Value::Object(map) }))
        }
        LaneAnswer::Reject => Ok(match &approval.kind {
            LaneApprovalKind::Permission => json!({ "outcome": { "outcome": "cancelled" } }),
            LaneApprovalKind::McpElicitation => json!({ "outcome": "decline" }),
            // question, plan_approval, an unknown kind and the placeholder all
            // decline the same way: `cancelled` is the outcome every one of
            // gx's four interaction types accepts.
            _ => json!({ "outcome": "cancelled" }),
        }),
        LaneAnswer::Raw { json } => serde_json::from_str::<Value>(json)
            .map_err(|e| bad_request(format!("the raw answer is not valid JSON: {e}"))),
    }
}

// ---------------------------------------------------------------------------
// paths, errors, bodies
// ---------------------------------------------------------------------------

/// `/v1/sessions/{id}` plus trailing segments. The id is percent-encoded so a
/// hostile id — it reached us off a roost tab, which is untrusted input —
/// cannot escape its path segment.
fn session_path(id: &str, tail: &[&str]) -> String {
    let mut p = format!("/v1/sessions/{}", encode_segment(id));
    for seg in tail {
        p.push('/');
        p.push_str(seg);
    }
    p
}

/// Percent-encodes everything outside the unreserved set. Hand-rolled rather
/// than taking a dependency: gx ids are UUIDs and tool-call ids and never need
/// it, but a bare `/` in one would re-address the request.
fn encode_segment(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// `base` + `path`. A path that will not join is a programming error in this
/// crate, so it surfaces as `Failed`.
fn join(base: &reqwest::Url, path: &str) -> Result<reqwest::Url, LaneError> {
    base.join(path.trim_start_matches('/'))
        .map_err(|e| failed(format!("building {path}: {e}")))
}

/// The three [`LaneError`] constructors this module is allowed to use, and the
/// single place server-influenced text is scrubbed.
///
/// **Redaction is at the boundary, not at selected call sites.** The rule in
/// §3.3 is absolute — a token appears in no `LaneEvent`, IPC response, Tauri
/// event, log line or error string — and the way to keep an absolute rule is to
/// make the unsafe spelling unavailable rather than to remember it at each of a
/// dozen `format!`s. Two of them had already been missed: a path built from a
/// server-supplied session id reached `decoding /v1/sessions/<id>: …`
/// verbatim, so a hostile gx returning the bearer AS a session id would have
/// had shed write the whole token into its own error.
///
/// "A malicious server already knows the token" is true and beside the point:
/// the damage is that WE then persist it somewhere it outlives the request and
/// is readable by anything that can read logs.
fn failed(msg: impl AsRef<str>) -> LaneError {
    LaneError::Failed(redact_hex64(msg.as_ref()))
}

/// See [`failed`].
fn unavailable(msg: impl AsRef<str>) -> LaneError {
    LaneError::Unavailable(redact_hex64(msg.as_ref()))
}

/// See [`failed`].
fn bad_request(msg: impl AsRef<str>) -> LaneError {
    LaneError::BadRequest(redact_hex64(msg.as_ref()))
}

/// Everything reqwest reports before a response head is a dial/transport
/// failure: nothing to talk to. The contract renders that as a quiet
/// unreachable row, so it must never come back as `Failed`.
fn dial_error(path: &str, e: &reqwest::Error) -> LaneError {
    unavailable(format!("gx {path}: {e}"))
}

/// gx's error envelope: a stable `error` code and a prose `message`.
#[derive(Debug, Default, Deserialize)]
struct GxErrorBody {
    #[serde(default, deserialize_with = "null_default")]
    error: String,
    #[serde(default, deserialize_with = "null_default")]
    message: String,
}

impl GxErrorBody {
    fn parse(body: &str) -> Option<GxErrorBody> {
        let parsed: GxErrorBody = serde_json::from_str(body).ok()?;
        (!parsed.error.trim().is_empty()).then_some(parsed)
    }
}

/// gx's eight codes → the contract's variants, row for row.
///
/// The CODE decides, not the status: gx promises the code is stable and the
/// message is prose. A body without a readable envelope — including a bare 401
/// from a proxy that is not gx — is [`LaneError::Failed`], the loud residue,
/// rather than being guessed at from the status.
pub fn map_gx_error(status: reqwest::StatusCode, path: &str, body: &str) -> LaneError {
    if let Some(e) = GxErrorBody::parse(body) {
        return match e.error.as_str() {
            "unauthorized" => LaneError::Unauthorized,
            // gx's message, kept WHOLE — it is the only thing that explains a
            // schema-level refusal — but run through [`redact_hex64`] first.
            "bad_request" => LaneError::BadRequest(redact_hex64(&e.message)),
            "unknown_session" => LaneError::UnknownSession,
            "unknown_approval" => LaneError::UnknownApproval,
            "already_submitted" => LaneError::AlreadySubmitted,
            "already_resolved" => LaneError::AlreadyResolved,
            "not_accepting" => LaneError::NotAccepting,
            // Quiet: the leader is gone or did not answer, which is "retry",
            // not "something is broken". The code is kept in the text because
            // it is the one thing that says which retryable condition it was.
            "leader_unavailable" => {
                unavailable(format!("leader_unavailable: {}", redact_hex64(&e.message)))
            }
            _ => failed_status(status, path, body),
        };
    }
    failed_status(status, path, body)
}

fn failed_status(status: reqwest::StatusCode, path: &str, body: &str) -> LaneError {
    failed(format!(
        "gx {path}: status {}{}",
        status.as_u16(),
        first_line(body)
    ))
}

/// A one-line, bounded echo of an unrecognized error body — enough to debug
/// with, not enough to paste a page of HTML into a log line.
///
/// **Redaction runs BEFORE truncation**, and the order is the whole point. Cut
/// first and a token straddling the 200-character boundary is left as a partial
/// run shorter than the redactor's 64-character threshold — so 150 characters of
/// filler followed by the token used to leave its first ~50 characters sitting
/// in the error, unscrubbed. Scrubbing the whole line first means the marker,
/// not a fragment of the secret, is what the truncation can cut.
fn first_line(body: &str) -> String {
    let line = body.lines().next().unwrap_or_default().trim();
    if line.is_empty() {
        return String::new();
    }
    let scrubbed = redact_hex64(line);
    let bounded: String = scrubbed.chars().take(200).collect();
    format!(": {bounded}")
}

/// Consumes a response body under `cap`, chunk by chunk.
///
/// Two refusals, in order: a declared `Content-Length` past the cap is refused
/// before a single body byte is read; an undeclared (or lying) body is refused
/// the moment the accumulated total would cross it. This is why `.text()` is not
/// used — it collects the WHOLE body before anything can look at it.
async fn read_body_capped(
    path: &str,
    mut resp: reqwest::Response,
    cap: usize,
) -> Result<String, LaneError> {
    if let Some(len) = resp.content_length() {
        if len > cap as u64 {
            return Err(over_cap(path, len, cap));
        }
    }
    let mut buf: Vec<u8> = Vec::new();
    loop {
        let next = resp
            .chunk()
            .await
            .map_err(|e| unavailable(format!("gx {path}: reading the body: {e}")))?;
        let Some(chunk) = next else { break };
        if buf.len() + chunk.len() > cap {
            return Err(over_cap(path, (buf.len() + chunk.len()) as u64, cap));
        }
        buf.extend_from_slice(&chunk);
    }
    // `from_utf8_lossy(..).into_owned()` copies the entire body a SECOND time on
    // the valid-UTF-8 path, which is every gx response; the lossy path is kept
    // for the one that is not.
    Ok(String::from_utf8(buf)
        .unwrap_or_else(|e| String::from_utf8_lossy(e.as_bytes()).into_owned()))
}

fn over_cap(path: &str, seen: u64, cap: usize) -> LaneError {
    failed(format!(
        "gx {path}: response body of at least {seen} bytes exceeds the {cap}-byte cap"
    ))
}

// ---------------------------------------------------------------------------
// the contract
// ---------------------------------------------------------------------------

#[async_trait::async_trait]
impl AgentLane for GxClient {
    /// Plan 017 §3.4's row. It differs from opencode's on exactly the two flags
    /// a client's UI branches on — `interject` and `history_cursor` — which is
    /// why those flags exist.
    fn capabilities(&self) -> LaneCapabilities {
        LaneCapabilities {
            kind: "gx".to_string(),
            interject: true,
            create: true,
            cancel: true,
            approvals: true,
            history_cursor: true,
        }
    }

    async fn sessions(&self) -> Result<Vec<LaneSession>, LaneError> {
        Ok(self
            .rest_sessions()
            .await?
            .iter()
            .map(|r| lane_session(r, 0))
            .collect())
    }

    async fn session(&self, id: &str) -> Result<LaneSession, LaneError> {
        Ok(lane_session(&self.rest_session(id).await?, 0))
    }

    /// A page of transcript, folded.
    ///
    /// **Without a cursor** it is the tail: `offset=-{limit}&limit={limit}`
    /// folded through a fresh fold, the final open streak flushed (a standalone
    /// page ends there, so leaving it open would silently drop the last turn).
    ///
    /// **With a cursor** it pages BACKWARDS by [`HISTORY_PAGE_SIZE`] until a
    /// page's minimum counter reaches the cursor's or [`MAX_HISTORY_PAGES`] is
    /// spent, then cuts POSITIONALLY at the cursor's envelope — the counters are
    /// not monotonic in transcript order, so "everything with a bigger counter"
    /// would both drop and duplicate rows.
    ///
    /// A cursor that is malformed, foreign to this session, or not found within
    /// the page budget is not an error: the tail comes back with
    /// `truncated: true`, which is the contract's "refetch, do not splice".
    async fn history(
        &self,
        id: &str,
        cursor: Option<&str>,
        limit: u32,
    ) -> Result<LaneHistory, LaneError> {
        let want = cursor.and_then(EventId::parse).filter(|c| c.belongs_to(id));

        let (envelopes, mut truncated, page_cursor) = match want {
            None => self.history_tail(id, limit).await?,
            Some(want) => {
                let mut pages: Vec<Vec<GxEnvelope>> = Vec::new();
                let mut newest_cursor: Option<String> = None;
                let mut found = false;
                for k in 0..MAX_HISTORY_PAGES {
                    let offset = -((HISTORY_PAGE_SIZE as i64) * (k as i64 + 1));
                    let page = self.rest_history(id, offset, HISTORY_PAGE_SIZE).await?;
                    if k == 0 {
                        newest_cursor = page.last_event_id.clone();
                    }
                    let reached = min_counter(&page.updates).is_some_and(|m| m <= want.counter);
                    let empty = page.updates.is_empty();
                    pages.push(page.updates);
                    if reached {
                        found = true;
                        break;
                    }
                    if empty {
                        // The start of the transcript: paging further back
                        // cannot find anything.
                        break;
                    }
                }
                // Pages were fetched newest-first; the transcript is oldest-first.
                let mut all: Vec<GxEnvelope> = pages.into_iter().rev().flatten().collect();
                match found.then(|| cursor_index(&all, &want)).flatten() {
                    // `split_off` MOVES the tail out of `all`, which dies at the
                    // end of this arm anyway. Cloning it would deep-copy every
                    // JSON tree in up to ten concatenated pages.
                    Some(idx) => (all.split_off(idx + 1), false, newest_cursor),
                    // Not located: hand back what is newest and say so, rather
                    // than replaying a history the client already holds.
                    None => {
                        let (updates, _, cursor) = self.history_tail(id, limit).await?;
                        (updates, true, cursor)
                    }
                }
            }
        };

        let mut fold = GxFold::new(id);
        for env in &envelopes {
            fold.apply(env);
        }
        // A standalone page ENDS here — there is no live stream about to close
        // the streak, so an unflushed one would drop the last turn.
        fold.flush_open();

        let mut ring = MessageRing::new();
        // Every row a fold mints carries its own `ts`; the `0` is the default
        // for a row that somehow does not, and stamping such a row 1970 is
        // more honest than inventing a clock this crate does not have.
        for m in fold.drain_messages() {
            ring.append(m, 0);
        }
        let (messages, paged) = ring.page(limit);
        // `hasMore` is gx's "there is history before this page"; `paged` is the
        // ring's "I dropped rows off the front of it". Either one means the
        // client is not holding a complete history, which is exactly what
        // `truncated` promises.
        truncated = truncated || paged;

        Ok(LaneHistory {
            messages,
            truncated,
            cursor: page_cursor.or_else(|| fold.resume_cursor().map(str::to_string)),
        })
    }

    /// `POST /v1/sessions {cwd, text}`, then the created row.
    ///
    /// gx's lane is an OBSERVER: a session created this way has no driver until
    /// a TUI attaches, and the leader drops driver-only reverse-requests in the
    /// meantime. The row comes back regardless — the caveat belongs in the
    /// client's UI, not in a refusal here.
    async fn create(&self, cwd: &str, text: &str) -> Result<LaneSession, LaneError> {
        let body = self
            .rest_post("/v1/sessions", Some(json!({ "cwd": cwd, "text": text })))
            .await?;
        let created: GxCreated = serde_json::from_str(&body)
            .map_err(|e| failed(format!("decoding POST /v1/sessions: {e}")))?;
        if created.session_id.is_empty() {
            return Err(failed("POST /v1/sessions returned no session id"));
        }
        self.session(&created.session_id).await
    }

    /// `POST …/messages {text, mode}`. `queue` is always accepted; `interject`
    /// is `409 not_accepting` unless the session is working, which the error
    /// table turns into [`LaneError::NotAccepting`] — the refusal a client's
    /// `Working`-gated affordance is supposed to make rare.
    async fn send(&self, id: &str, text: &str, mode: SendMode) -> Result<(), LaneError> {
        let mode = match mode {
            SendMode::Queue => "queue",
            SendMode::Interject => "interject",
        };
        self.rest_post(
            &session_path(id, &["messages"]),
            Some(json!({ "text": text, "mode": mode })),
        )
        .await?;
        Ok(())
    }

    /// `POST …/cancel`. gx refuses a cancel with nothing to cancel
    /// (`409 not_accepting`), and that refusal is surfaced rather than swallowed
    /// — see the contract's correction 8.
    async fn cancel(&self, id: &str) -> Result<(), LaneError> {
        self.rest_post(&session_path(id, &["cancel"]), None).await?;
        Ok(())
    }

    /// Everything open on this session.
    ///
    /// `resolved` entries are dropped — gx returns the last 50 resolved within
    /// an hour, and a resolved approval is history, not a thing waiting on the
    /// human. `submitted` is KEPT: an answer is on the wire and the client
    /// should see the optimistic state rather than a row that vanished.
    async fn approvals(&self, id: &str) -> Result<Vec<LaneApproval>, LaneError> {
        Ok(self
            .rest_approvals(id)
            .await?
            .iter()
            .map(lane_approval)
            .filter(|a| a.status != LaneApprovalStatus::Resolved)
            .collect())
    }

    /// Answer one approval.
    ///
    /// The authoritative request is re-read (`GET …/approvals/{id}`)
    /// **immediately before** the answer is translated, because the translation
    /// depends on what the agent OFFERED and a client's copy may be a reseed
    /// out of date. It costs one round trip and removes a whole class of
    /// "answered an option that is no longer on the request".
    ///
    /// A `202` means *sent*, not *accepted*: gx's first-answer-wins means a
    /// losing answer is discarded in silence, and only an `interaction_resolved`
    /// moves the approval to resolved.
    async fn answer(
        &self,
        id: &str,
        approval_id: &str,
        answer: LaneAnswer,
    ) -> Result<(), LaneError> {
        let approval = self.rest_approval(id, approval_id).await?;
        let response = answer_body(&approval, &answer)?;
        self.rest_post(
            &session_path(id, &["approvals", &encode_segment(approval_id)]),
            Some(json!({ "response": response })),
        )
        .await?;
        Ok(())
    }

    /// **Not yet wired** — the watcher is plan 017 C3.
    ///
    /// `Failed` and not `Unavailable` on purpose: `Unavailable` is the quiet
    /// "the agent is not running" a client renders as a stale row, and a missing
    /// implementation must not be able to look like one.
    async fn subscribe(
        &self,
        _id: &str,
        _cursor: Option<String>,
    ) -> Result<LaneSubscription, LaneError> {
        Err(failed(
            "the gx lane watcher is not implemented yet (plan 017 C3)",
        ))
    }
}

#[cfg(test)]
mod tests;
