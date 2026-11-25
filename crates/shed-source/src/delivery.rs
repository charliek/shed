//! The delivery stream client (plan 039 §3.1 and §3.7, commit 2): one machine source's
//! side of the provision contract's transport (`tests/provision-contract/v2/CONTRACT.md`,
//! section 3) against one shed-server.
//!
//! * `GET /api/delivery/requests` with the machine headers and the credential the
//!   [`StreamCredentials`] provide (the credentials-scope client certificate on an `mtls`
//!   server; nothing on an open one), held open; its `X-Shed-Delivery-Stream` id is kept
//!   for the stream's life.
//! * Each `request` event is read with `shed_provision`'s framing
//!   ([`shed_provision::stream::EventReader`], [`shed_provision::stream::read_event`]) and
//!   handed to the caller's [`Answerer`]; its answer is posted to
//!   `POST /api/delivery/requests/{id}` naming that stream.
//! * **A request dies with its stream.** An answer that is not on its way when the stream
//!   ends is dropped, never posted on a later stream; the server failed that request when
//!   the stream ended, and it is not replayed. An answer past its request's deadline is
//!   dropped too.
//! * When a stream ends (or is refused) a new one is opened after a wait, the reconnect
//!   rules of the broker's bus written out afresh ([`next_backoff`]): 1 s, doubled after
//!   each stream that failed or held under 60 s, at most 30 s, back to 1 s after one that
//!   held 60 s, and never less than a `429`'s `Retry-After`. A stream's response headers
//!   are waited for 30 s; a stream that then hears nothing (not even the 15-second
//!   keepalive) for 45 seconds is taken as dead.
//! * **One certificate per stream.** The stream and every answer it carries present the
//!   certificate the stream was opened with: each stream has a transport of its own whose
//!   identity is fixed when it opens. A `401` (on the stream or on an answer) marks the
//!   credential refused and ends the stream; when the credential needs renewing
//!   ([`StreamCredentials::needs_refresh`], checked on a timer) the stream is ended too.
//!   Either way its pending requests die with it, as the contract says, and the next
//!   stream opens with the new certificate. An answer is never re-sent on another
//!   identity.
//! * **Plain HTTP only on loopback.** A server in open mode listens on loopback only, so
//!   an `http://` server must be a loopback address (`127.0.0.0/8`, `::1`, or `localhost`
//!   resolving only to loopback); anything else must be `https://` with a pinned
//!   certificate fingerprint. Every mode, test mode included.
//!
//! * **The account service is a settled state** (plan 040 §3.9). On a directory server
//!   the account service, not a machine, answers for a directory user, and the stream
//!   route refuses `409 SOURCE_IS_SERVICE`. That is no failure to retry: the client stops
//!   reconnecting ([`StreamState::Service`]) and asks again only when its
//!   [`SourceCheck`] (the shell's `GET /api/sources`, which needs the control scope this
//!   client does not hold) says the service no longer answers, checked every
//!   [`SERVICE_RECHECK`] and whenever the server list changes. With no check, the
//!   stream itself is tried again on that schedule.
//!
//! [`StreamStatus`] is what a shell shows: a state, a last error CLASS (never a server's
//! message or a value), and counters. An answer's refusal is reported by the contract's
//! closed lists of codes and rule names only ([`known_code`], [`known_rule`]): anything
//! else a server sends there is `unknown`.

use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::StreamExt as _;
use rustls::sign::CertifiedKey;
use serde::Serialize;
use shed_core::tls::ClientCertResolver;
use shed_provision::stream::{read_event, EventReader, StreamItem};
use shed_provision::{Answer, Request};
use tokio::sync::watch;
use tokio::task::JoinSet;
use tokio::time::Instant;

use crate::machine::{self, MachineIdentity};
use crate::target::ServerTarget;

/// The stream route.
pub const STREAM_PATH: &str = "/api/delivery/requests";
/// The answer route's prefix; the request id follows.
pub const ANSWER_PATH_PREFIX: &str = "/api/delivery/requests/";
/// The stream's id, on the stream's `200` and on every answer.
pub const HEADER_STREAM: &str = "X-Shed-Delivery-Stream";
/// This machine's id (32 lowercase hex digits).
pub const HEADER_MACHINE_ID: &str = "X-Shed-Machine-Id";
/// This machine's display name, percent-encoded UTF-8.
pub const HEADER_MACHINE_NAME: &str = "X-Shed-Machine-Name";

/// The reconnect floor.
pub const MIN_BACKOFF: Duration = Duration::from_secs(1);
/// The reconnect ceiling.
pub const MAX_BACKOFF: Duration = Duration::from_secs(30);
/// A stream held this long resets the wait to the floor.
pub const HELD_RESET: Duration = Duration::from_secs(60);
/// The longest `Retry-After` honoured: a larger one is waited out as this.
pub const MAX_RETRY_AFTER: Duration = Duration::from_secs(600);
/// A stream that hears nothing for this long is dead (three missed keepalives).
pub const STREAM_SILENCE: Duration = Duration::from_secs(45);
/// How long a stream's response headers are waited for.
pub const HEADER_TIMEOUT: Duration = Duration::from_secs(30);
/// How often an open stream asks whether its credential needs renewing.
pub const REFRESH_CHECK: Duration = Duration::from_secs(30);
/// One answer POST's bound, from send to the response's end.
pub const ANSWER_TIMEOUT: Duration = Duration::from_secs(30);
/// How often a server whose stream was refused `SOURCE_IS_SERVICE` is checked again.
pub const SERVICE_RECHECK: Duration = Duration::from_secs(300);
/// How much of a refusal's body is read.
const MAX_ERROR_BODY: usize = 4 << 10;

/// What a re-check of a server settled on the account service found.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ServiceCheck {
    /// The account service still answers for the user there: stay settled.
    Service,
    /// It no longer does (a machine answers, or nobody yet): open the stream again.
    NotService,
    /// The check could not tell (the server could not be reached or read): stay
    /// settled and ask at the next re-check.
    Unknown,
}

/// A [`SourceCheck`]'s answer.
pub type CheckFuture = Pin<Box<dyn Future<Output = ServiceCheck> + Send>>;

/// Asks whether the account service still answers for this user on the server the
/// stream reads from (its current target: name and endpoint), through the shell's
/// `GET /api/sources` on a control-scope client for that same endpoint.
pub type SourceCheck = Arc<dyn Fn(&ServerTarget) -> CheckFuture + Send + Sync>;

/// The wait before the next stream: the floor after a stream that held
/// [`HELD_RESET`] (or before any), else double the last wait, at most [`MAX_BACKOFF`];
/// never less than `retry_after`. The contract's schedule, as the reference source has it.
pub fn next_backoff(
    timing: &Timing,
    last: Option<Duration>,
    held: Duration,
    retry_after: Option<Duration>,
) -> Duration {
    let wait = match last {
        Some(last) if held < timing.held_reset => (last * 2).min(timing.max_backoff),
        _ => timing.min_backoff,
    };
    wait.max(retry_after.unwrap_or_default())
}

/// The client's clocks: [`Timing::default`] is the contract's; tests shorten them.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Timing {
    pub min_backoff: Duration,
    pub max_backoff: Duration,
    pub held_reset: Duration,
    pub silence: Duration,
    pub answer_timeout: Duration,
    pub header_timeout: Duration,
    pub refresh_check: Duration,
    /// How often a server settled on the account service is checked again.
    pub service_recheck: Duration,
}

impl Default for Timing {
    fn default() -> Self {
        Timing {
            min_backoff: MIN_BACKOFF,
            max_backoff: MAX_BACKOFF,
            held_reset: HELD_RESET,
            silence: STREAM_SILENCE,
            answer_timeout: ANSWER_TIMEOUT,
            header_timeout: HEADER_TIMEOUT,
            refresh_check: REFRESH_CHECK,
            service_recheck: SERVICE_RECHECK,
        }
    }
}

/// Answers the requests a stream carries. Called once per request, on a task of its
/// own; `None` leaves the request unanswered (it ends at its deadline).
#[async_trait::async_trait]
pub trait Answerer: Send + Sync + 'static {
    /// The answer to `request` from the server named `server`.
    async fn answer(&self, server: &str, request: Request) -> Option<Answer>;

    /// What became of an answer this answerer gave: posted (and how the server took
    /// it), or dropped. Default: nothing.
    fn outcome(&self, server: &str, request_id: &str, outcome: &AnswerOutcome) {
        let _ = (server, request_id, outcome);
    }
}

/// What became of one answer.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "outcome", rename_all = "snake_case")]
pub enum AnswerOutcome {
    /// `204`.
    Accepted,
    /// `409 already_answered`: an earlier post of this answer was taken.
    AlreadyAnswered,
    /// Another refusal: its status, its `error.code`, and an `answer_refused`'s rule.
    Refused {
        status: u16,
        code: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        rule: Option<String>,
    },
    /// The post failed before a response.
    Failed { class: ErrorClass },
    /// Not posted: the stream it came on had ended.
    DroppedStreamEnded,
    /// Not posted: its deadline had passed.
    DroppedDeadline,
}

/// The credential a server's stream and answers authenticate with.
#[async_trait::async_trait]
pub trait StreamCredentials: Send + Sync {
    /// Make the credential ready for a new stream, minting when it must; the bearer
    /// token to send, `""` for none.
    async fn ready(&self) -> Result<String, String>;
    /// The client certificate [`Self::ready`] left armed, presented by the stream about
    /// to open and every answer on it; `None` presents none.
    fn certificate(&self) -> Option<Arc<CertifiedKey>>;
    /// The server refused the credential (a `401`): the next [`Self::ready`] mints.
    fn refused(&self);
    /// Whether the credential is due for renewal (its refresh window, or a refusal): an
    /// open stream is ended so the next opens with a new one. Default: never.
    fn needs_refresh(&self) -> bool {
        false
    }
}

/// No credential: an open server (plain HTTP on loopback) asks for none.
pub struct NoCredentials;

#[async_trait::async_trait]
impl StreamCredentials for NoCredentials {
    async fn ready(&self) -> Result<String, String> {
        Ok(String::new())
    }
    fn certificate(&self) -> Option<Arc<CertifiedKey>> {
        None
    }
    fn refused(&self) {}
}

/// Where a server's stream stands.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum StreamState {
    /// No stream yet.
    #[default]
    Connecting,
    /// A stream is open.
    Connected,
    /// Another machine is this user's bound source on the server (`SOURCE_NOT_BOUND`).
    NotBound,
    /// The server says this machine holds a stream already (`SOURCE_DUPLICATE_STREAM`).
    Duplicate,
    /// The account service answers for this user on the server (`SOURCE_IS_SERVICE`):
    /// no stream is held, and none is tried until a re-check finds otherwise.
    Service,
    /// The last attempt failed for another reason ([`StreamStatus::last_error`]).
    Offline,
    /// Stopped (shutdown, or the server left the config).
    Stopped,
}

/// Why the last stream ended or was refused: a class, never the server's words.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorClass {
    /// The connection failed or broke.
    Transport,
    /// The stream heard nothing for the silence bound.
    Silent,
    /// The server ended the stream.
    Closed,
    /// `401`: the credential was refused (the next attempt mints).
    Unauthorized,
    /// No credential could be made ready (a mint failed).
    Credentials,
    /// `403 SOURCE_NOT_BOUND`.
    NotBound,
    /// `409 SOURCE_DUPLICATE_STREAM`.
    Duplicate,
    /// `409 SOURCE_IS_SERVICE`: the account service answers for this user there.
    SourceIsService,
    /// `429 SOURCE_RATE_LIMITED`.
    RateLimited,
    /// `403 SOURCE_MACHINE_ID_REQUIRED`.
    MachineIdRequired,
    /// Another `403` (a service client, a script key).
    Forbidden,
    /// `404`: the server has no delivery routes (older than delivery).
    Unsupported,
    /// `503`: delivery is not running on the server.
    Unavailable,
    /// Any other status.
    BadStatus,
    /// The stream broke the contract's framing or named no valid stream id.
    Protocol,
    /// This client could not be built for the server (its URL or pin): plain HTTP to a
    /// host that is not loopback, `https` with no pin, or no URL at all.
    Config,
    /// The stream was ended to renew its credential (not a failure).
    Rotated,
}

/// The contract's error codes, and the API's own codes the two delivery routes answer.
/// Anything else a server sends is reported as `unknown`.
pub const KNOWN_CODES: &[&str] = &[
    "unknown_request",
    "already_answered",
    "deadline_passed",
    "not_stream_owner",
    "answer_refused",
    "UNAUTHORIZED",
    "FORBIDDEN",
    "SOURCE_MACHINE_ID_REQUIRED",
    "SOURCE_RATE_LIMITED",
    "SOURCE_NOT_BOUND",
    "SOURCE_DUPLICATE_STREAM",
    "SOURCE_IS_SERVICE",
    "DELIVERY_DISABLED",
    "INTERNAL_ERROR",
    "INVALID_REQUEST",
    "REQUEST_TOO_LARGE",
];

/// What every server-supplied code or rule outside the closed lists is reported as.
pub const UNKNOWN: &str = "unknown";

/// `code` if it is one of [`KNOWN_CODES`], else [`UNKNOWN`] (empty stays empty).
pub fn known_code(code: &str) -> String {
    if code.is_empty() {
        String::new()
    } else if KNOWN_CODES.contains(&code) {
        code.to_string()
    } else {
        UNKNOWN.to_string()
    }
}

/// `rule` if it is one of the contract's answer rule names, else [`UNKNOWN`].
pub fn known_rule(rule: &str) -> String {
    match shed_provision::Rule::from_name(rule) {
        Some(r) if shed_provision::ANSWER_RULES.contains(&r) => r.as_str().to_string(),
        _ => UNKNOWN.to_string(),
    }
}

impl ErrorClass {
    fn state(self) -> StreamState {
        match self {
            ErrorClass::NotBound => StreamState::NotBound,
            ErrorClass::Duplicate => StreamState::Duplicate,
            ErrorClass::SourceIsService => StreamState::Service,
            ErrorClass::Rotated => StreamState::Connecting,
            _ => StreamState::Offline,
        }
    }
}

/// A server's stream, for a shell: no value, no server message.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Default)]
pub struct StreamStatus {
    pub state: StreamState,
    /// When `state` last changed (RFC 3339, UTC).
    pub since: String,
    /// Why the last stream ended or was refused.
    pub last_error: Option<ErrorClass>,
    /// Streams opened.
    pub streams: u64,
    /// Requests read.
    pub requests: u64,
    /// `request` events that did not parse (not answered).
    pub unreadable: u64,
    /// Answers the server took (`204`, or `409 already_answered`).
    pub answered: u64,
    /// Answers the server refused, or whose post failed.
    pub refused: u64,
    /// Answers not posted: their stream ended, or their deadline passed.
    pub dropped: u64,
    /// Re-checks made while settled on the account service.
    pub service_checks: u64,
}

/// A status shared between a stream's task and its readers.
pub type SharedStatus = Arc<Mutex<StreamStatus>>;

/// A fresh status, in `Connecting`.
pub fn new_status() -> SharedStatus {
    Arc::new(Mutex::new(StreamStatus {
        since: crate::time::now_rfc3339(),
        ..StreamStatus::default()
    }))
}

fn set_state(status: &SharedStatus, state: StreamState, error: Option<ErrorClass>) {
    let mut s = status.lock().unwrap_or_else(|e| e.into_inner());
    if s.state != state {
        s.state = state;
        s.since = crate::time::now_rfc3339();
    }
    if error.is_some() || state == StreamState::Connected {
        s.last_error = error;
    }
}

fn bump(status: &SharedStatus, f: impl FnOnce(&mut StreamStatus)) {
    f(&mut status.lock().unwrap_or_else(|e| e.into_inner()));
}

/// How this machine's name travels in [`HEADER_MACHINE_NAME`]: UTF-8, every byte that
/// is not an RFC 3986 unreserved character as `%XX` (the bus's encoding).
pub fn percent_encode_name(name: &str) -> String {
    let mut out = String::with_capacity(name.len());
    for b in name.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// How one stream ended.
#[derive(Debug)]
struct StreamEnd {
    /// `None`: shutdown.
    class: Option<ErrorClass>,
    held: Duration,
    retry_after: Option<Duration>,
}

impl StreamEnd {
    fn shutdown() -> Self {
        StreamEnd {
            class: None,
            held: Duration::ZERO,
            retry_after: None,
        }
    }
    fn refused(class: ErrorClass) -> Self {
        StreamEnd {
            class: Some(class),
            held: Duration::ZERO,
            retry_after: None,
        }
    }
}

/// Looks a host name up: the addresses it resolves to on `port`.
pub type HostLookup<'a> = &'a dyn Fn(&str, u16) -> std::io::Result<Vec<std::net::SocketAddr>>;

/// The system's lookup.
fn system_lookup(host: &str, port: u16) -> std::io::Result<Vec<std::net::SocketAddr>> {
    use std::net::ToSocketAddrs as _;
    Ok((host, port).to_socket_addrs()?.collect())
}

/// The loopback addresses `url`'s host is, when it is loopback: an IP literal in
/// `127.0.0.0/8` or `::1` as it is, or `localhost` when every address `lookup` gives it
/// is loopback (those addresses, which the transport then connects to, never looking
/// the name up again). `None` for anything else.
pub fn loopback_addrs(
    url: &reqwest::Url,
    lookup: HostLookup<'_>,
) -> Option<Vec<std::net::SocketAddr>> {
    let host = url.host_str()?;
    let port = url.port_or_known_default().unwrap_or(80);
    let bare = host.trim_start_matches('[').trim_end_matches(']');
    if let Ok(ip) = bare.parse::<std::net::IpAddr>() {
        return ip
            .is_loopback()
            .then(|| vec![std::net::SocketAddr::new(ip, port)]);
    }
    if !host.eq_ignore_ascii_case("localhost") {
        return None;
    }
    let addrs = lookup(host, port).ok()?;
    (!addrs.is_empty() && addrs.iter().all(|a| a.ip().is_loopback())).then_some(addrs)
}

/// Whether `url`'s host is loopback ([`loopback_addrs`] with the system's lookup).
pub fn loopback_host(url: &reqwest::Url) -> bool {
    loopback_addrs(url, &system_lookup).is_some()
}

/// How a server is reached.
#[derive(Debug, Clone)]
enum Endpoint {
    /// `https`, with the server's pinned fingerprint.
    Tls { pin: String },
    /// Plain `http` on loopback: no proxy ever, and a host name connects only to the
    /// loopback addresses it was validated with.
    Loopback {
        host: String,
        addrs: Vec<std::net::SocketAddr>,
    },
}

/// The transport a stream (and its answers) uses, presenting exactly `cert` on `https`
/// (the stream's certificate, fixed for its life).
fn transport(
    endpoint: &Endpoint,
    cert: Option<Arc<CertifiedKey>>,
) -> Result<reqwest::Client, String> {
    let mut builder = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(10));
    match endpoint {
        Endpoint::Tls { pin } => {
            let resolver = Arc::new(ClientCertResolver::new());
            resolver.set(cert);
            let cfg = shed_core::tls::pinned_client_config_with_client_auth(pin, resolver)
                .map_err(|e| e.to_string())?;
            builder = builder.use_preconfigured_tls(cfg);
        }
        Endpoint::Loopback { host, addrs } => {
            // Plaintext never goes through a proxy (one from the environment would see
            // the credentials), and a name is never looked up again.
            builder = builder.no_proxy();
            if host.parse::<std::net::IpAddr>().is_err() && !host.starts_with('[') {
                builder = builder.resolve_to_addrs(host, addrs);
            }
        }
    }
    builder.build().map_err(|e| e.to_string())
}

/// One open stream: its id, its transport (and so its certificate), its bearer, and the
/// signal an answer raises to end it (a `401`).
#[derive(Clone)]
struct OpenStream {
    id: String,
    http: reqwest::Client,
    bearer: String,
    end: Arc<watch::Sender<bool>>,
}

/// The delivery stream client for one server.
#[derive(Clone)]
pub struct DeliveryClient {
    server: String,
    /// The target the client was built for (what a re-check asks about).
    target: ServerTarget,
    base_url: String,
    endpoint: Endpoint,
    machine: MachineIdentity,
    creds: Arc<dyn StreamCredentials>,
    timing: Timing,
    /// Asks whether the account service still answers, while settled on it.
    check: Option<SourceCheck>,
    /// Bumped when the server list changes: a settled client checks at once.
    recheck: Option<watch::Receiver<u64>>,
}

impl DeliveryClient {
    /// A client for `target`, as `machine`, authenticating with `creds`. Refused (an
    /// error) for an identity this build would not send, plain HTTP to a host that is not
    /// loopback, a pin on plain HTTP, or `https` with no pin.
    pub fn new(
        target: &ServerTarget,
        machine: MachineIdentity,
        creds: Arc<dyn StreamCredentials>,
    ) -> Result<Self, String> {
        Self::with_lookup(target, machine, creds, &system_lookup)
    }

    /// [`Self::new`] validating a loopback host name with `lookup` (tests).
    pub fn with_lookup(
        target: &ServerTarget,
        machine: MachineIdentity,
        creds: Arc<dyn StreamCredentials>,
        lookup: HostLookup<'_>,
    ) -> Result<Self, String> {
        if !machine::valid_id(&machine.id) || !machine::valid_name(&machine.name) {
            return Err("this machine's identity is not valid".into());
        }
        let base_url = target.url.trim_end_matches('/').to_string();
        let url = reqwest::Url::parse(&base_url)
            .map_err(|_| format!("server {}: not a URL", target.name))?;
        let pin = target.tls_fingerprint.as_str();
        let endpoint = match url.scheme() {
            "https" if pin.is_empty() => {
                return Err(format!(
                    "server {}: an https server needs a pinned certificate fingerprint",
                    target.name
                ))
            }
            "https" => Endpoint::Tls {
                pin: pin.to_string(),
            },
            "http" if !pin.is_empty() => {
                return Err(format!(
                    "server {}: a pin is configured for a non-https URL; refusing to send unpinned plaintext",
                    target.name
                ))
            }
            "http" => match loopback_addrs(&url, lookup) {
                Some(addrs) => Endpoint::Loopback {
                    host: url.host_str().unwrap_or_default().to_string(),
                    addrs,
                },
                None => {
                    return Err(format!(
                        "server {}: plain http is used only on loopback (a server in open mode listens nowhere else); use its https URL",
                        target.name
                    ))
                }
            },
            _ => return Err(format!("server {}: not an http(s) URL", target.name)),
        };
        // Built once here so a bad pin is refused now, not at every stream.
        transport(&endpoint, None).map_err(|e| format!("server {}: {e}", target.name))?;
        Ok(DeliveryClient {
            server: target.name.clone(),
            target: target.clone(),
            base_url,
            endpoint,
            machine,
            creds,
            timing: Timing::default(),
            check: None,
            recheck: None,
        })
    }

    /// The same client on other clocks (tests).
    pub fn with_timing(mut self, timing: Timing) -> Self {
        self.timing = timing;
        self
    }

    /// The same client asking `check` while settled on the account service, and
    /// checking at once whenever `recheck` changes (the server list did).
    pub fn with_service_check(
        mut self,
        check: Option<SourceCheck>,
        recheck: Option<watch::Receiver<u64>>,
    ) -> Self {
        self.check = check;
        self.recheck = recheck;
        self
    }

    /// Holds a stream, opening a new one each time it ends, until `shutdown` reads
    /// `true`. Each request read is answered by `answerer`; `status` is kept current.
    pub async fn run(
        &self,
        answerer: Arc<dyn Answerer>,
        status: SharedStatus,
        shutdown: watch::Receiver<bool>,
    ) {
        let mut last: Option<Duration> = None;
        let mut recheck = self.recheck.clone();
        loop {
            if *shutdown.borrow() {
                break;
            }
            let end = self.one_stream(&answerer, &status, shutdown.clone()).await;
            let Some(class) = end.class else { break };
            if class == ErrorClass::SourceIsService {
                // Settled: no backoff, no reconnect, until a re-check finds the account
                // service no longer answers here.
                set_state(&status, StreamState::Service, Some(class));
                if !self.settle(&status, &shutdown, &mut recheck).await {
                    break;
                }
                set_state(&status, StreamState::Connecting, None);
                last = None;
                continue;
            }
            if class == ErrorClass::Unauthorized {
                self.creds.refused();
            }
            if class == ErrorClass::Rotated {
                // A renewal is not a failure: reconnect from the floor.
                last = None;
            }
            set_state(&status, class.state(), Some(class));
            let wait = next_backoff(&self.timing, last, end.held, end.retry_after);
            last = Some(wait);
            let mut sd = shutdown.clone();
            tokio::select! {
                _ = sd.wait_for(|v| *v) => break,
                _ = tokio::time::sleep(wait) => {}
            }
        }
        set_state(&status, StreamState::Stopped, None);
    }

    /// Waits while the account service answers here: checks every
    /// [`Timing::service_recheck`] and whenever `recheck` changes, and returns `true`
    /// once a check says it no longer answers (with no check, at the first of those),
    /// `false` at shutdown. A change of `recheck` seen before settling is not one.
    async fn settle(
        &self,
        status: &SharedStatus,
        shutdown: &watch::Receiver<bool>,
        recheck: &mut Option<watch::Receiver<u64>>,
    ) -> bool {
        if let Some(r) = recheck.as_mut() {
            r.borrow_and_update();
        }
        loop {
            let mut sd = shutdown.clone();
            let list_changed = async {
                let changed = match recheck.as_mut() {
                    Some(r) => r.changed().await.is_ok(),
                    None => false,
                };
                if !changed {
                    // No list, or its sender is gone: only the timer wakes.
                    std::future::pending::<()>().await;
                }
            };
            tokio::select! {
                _ = sd.wait_for(|v| *v) => return false,
                _ = tokio::time::sleep(self.timing.service_recheck) => {}
                _ = list_changed => {}
            }
            bump(status, |s| s.service_checks += 1);
            let Some(check) = &self.check else {
                return true;
            };
            let found = tokio::select! {
                _ = sd.wait_for(|v| *v) => return false,
                f = check(&self.target) => f,
            };
            if found == ServiceCheck::NotService {
                return true;
            }
        }
    }

    fn with_machine(&self, req: reqwest::RequestBuilder, bearer: &str) -> reqwest::RequestBuilder {
        let req = req
            .header(HEADER_MACHINE_ID, self.machine.id.as_str())
            .header(HEADER_MACHINE_NAME, percent_encode_name(&self.machine.name));
        if bearer.is_empty() {
            req
        } else {
            req.bearer_auth(bearer)
        }
    }

    async fn one_stream(
        &self,
        answerer: &Arc<dyn Answerer>,
        status: &SharedStatus,
        shutdown: watch::Receiver<bool>,
    ) -> StreamEnd {
        let mut sd = shutdown.clone();
        let bearer = tokio::select! {
            _ = sd.wait_for(|v| *v) => return StreamEnd::shutdown(),
            b = self.creds.ready() => match b {
                Ok(b) => b,
                Err(_) => return StreamEnd::refused(ErrorClass::Credentials),
            },
        };
        // The stream's identity, fixed for its life and every answer on it.
        let http = match transport(&self.endpoint, self.creds.certificate()) {
            Ok(h) => h,
            Err(_) => return StreamEnd::refused(ErrorClass::Config),
        };
        let req = self.with_machine(
            http.get(format!("{}{STREAM_PATH}", self.base_url))
                .header(reqwest::header::ACCEPT, "text/event-stream"),
            &bearer,
        );
        let resp = tokio::select! {
            _ = sd.wait_for(|v| *v) => return StreamEnd::shutdown(),
            r = tokio::time::timeout(self.timing.header_timeout, req.send()) => match r {
                Err(_) => return StreamEnd::refused(ErrorClass::Silent),
                Ok(Ok(r)) => r,
                Ok(Err(e)) => {
                    let msg = shed_core::authfail::flatten(&e);
                    return StreamEnd::refused(if shed_core::authfail::is_auth_shaped_message(&msg) {
                        ErrorClass::Unauthorized
                    } else {
                        ErrorClass::Transport
                    });
                }
            },
        };
        let code = resp.status().as_u16();
        if code != 200 {
            let retry_after = retry_after(resp.headers().get(reqwest::header::RETRY_AFTER));
            let body = read_bounded(resp, Duration::from_secs(5)).await;
            let class = classify_refusal(code, &body);
            return StreamEnd {
                class: Some(class),
                held: Duration::ZERO,
                retry_after: (class == ErrorClass::RateLimited)
                    .then_some(retry_after)
                    .flatten(),
            };
        }
        let stream_id = resp
            .headers()
            .get(HEADER_STREAM)
            .and_then(|v| v.to_str().ok())
            .filter(|v| shed_provision::valid_id(v))
            .map(str::to_string);
        let Some(stream_id) = stream_id else {
            return StreamEnd::refused(ErrorClass::Protocol);
        };
        set_state(status, StreamState::Connected, None);
        bump(status, |s| s.streams += 1);
        let opened = Instant::now();

        let (end_tx, mut end_rx) = watch::channel(false);
        let open = OpenStream {
            id: stream_id,
            http,
            bearer,
            end: Arc::new(end_tx),
        };
        let (alive_tx, alive_rx) = watch::channel(true);
        let mut answers: JoinSet<()> = JoinSet::new();
        let mut reader = EventReader::new();
        let mut body = resp.bytes_stream();
        let mut refresh = tokio::time::interval_at(
            Instant::now() + self.timing.refresh_check,
            self.timing.refresh_check,
        );
        // One silence deadline for the stream, moved only when body bytes arrive: the
        // refresh timer and finishing answers do not restart it.
        let mut silent_at = Instant::now() + self.timing.silence;
        let class = loop {
            tokio::select! {
                _ = sd.wait_for(|v| *v) => break None,
                _ = end_rx.wait_for(|v| *v) => break Some(ErrorClass::Unauthorized),
                _ = refresh.tick() => {
                    if self.creds.needs_refresh() {
                        break Some(ErrorClass::Rotated);
                    }
                }
                Some(_) = answers.join_next(), if !answers.is_empty() => {}
                chunk = tokio::time::timeout_at(silent_at, body.next()) => {
                    let bytes = match chunk {
                        Err(_) => break Some(ErrorClass::Silent),
                        Ok(None) => break Some(ErrorClass::Closed),
                        Ok(Some(Err(_))) => break Some(ErrorClass::Transport),
                        Ok(Some(Ok(b))) => b,
                    };
                    silent_at = Instant::now() + self.timing.silence;
                    let events = match reader.push(&bytes) {
                        Ok(evs) => evs,
                        Err(_) => break Some(ErrorClass::Protocol),
                    };
                    for ev in events {
                        match read_event(&ev) {
                            StreamItem::Request(r) => {
                                bump(status, |s| s.requests += 1);
                                answers.spawn(self.clone().answer_task(
                                    answerer.clone(),
                                    status.clone(),
                                    open.clone(),
                                    *r,
                                    alive_rx.clone(),
                                ));
                            }
                            StreamItem::Unreadable => bump(status, |s| s.unreadable += 1),
                            StreamItem::Ignored => {}
                        }
                    }
                }
            }
        };
        let held = opened.elapsed();
        // The stream is over: answers not yet on their way are dropped; those being
        // posted finish (each is bounded), unless this is a shutdown.
        let _ = alive_tx.send(false);
        drop(body);
        match class {
            None => answers.abort_all(),
            Some(_) => loop {
                tokio::select! {
                    _ = sd.wait_for(|v| *v) => { answers.abort_all(); break; }
                    next = answers.join_next() => if next.is_none() { break },
                }
            },
        }
        StreamEnd {
            class,
            held,
            retry_after: None,
        }
    }

    async fn answer_task(
        self,
        answerer: Arc<dyn Answerer>,
        status: SharedStatus,
        open: OpenStream,
        request: Request,
        mut alive: watch::Receiver<bool>,
    ) {
        let id = request.id.clone();
        let deadline = shed_provision::parse_time(&request.deadline);
        let answer = tokio::select! {
            _ = alive.wait_for(|v| !*v) => None,
            a = answerer.answer(&self.server, request) => Some(a),
        };
        let drop_with = |outcome: AnswerOutcome| {
            bump(&status, |s| s.dropped += 1);
            answerer.outcome(&self.server, &id, &outcome);
        };
        let answer = match answer {
            // The stream ended while the answer was being built.
            None => return drop_with(AnswerOutcome::DroppedStreamEnded),
            // The answerer chose not to answer.
            Some(None) => return,
            Some(Some(a)) => a,
        };
        if !*alive.borrow() {
            return drop_with(AnswerOutcome::DroppedStreamEnded);
        }
        if deadline.is_some_and(|d| crate::time::now_unix() >= d) {
            return drop_with(AnswerOutcome::DroppedDeadline);
        }
        let outcome = self.post(&open, &id, answer.to_json()).await;
        bump(&status, |s| match outcome {
            AnswerOutcome::Accepted | AnswerOutcome::AlreadyAnswered => s.answered += 1,
            _ => s.refused += 1,
        });
        answerer.outcome(&self.server, &id, &outcome);
    }

    /// Posts one answer on `open`'s transport (its certificate) naming its stream. A
    /// `401` is not retried on another identity: the credential is marked refused and the
    /// stream is ended, so its requests die and the next stream opens renewed.
    async fn post(&self, open: &OpenStream, id: &str, body: Vec<u8>) -> AnswerOutcome {
        let req = self.with_machine(
            open.http
                .post(format!("{}{ANSWER_PATH_PREFIX}{id}", self.base_url))
                .header(reqwest::header::CONTENT_TYPE, "application/json")
                .header(HEADER_STREAM, open.id.as_str())
                .timeout(self.timing.answer_timeout)
                .body(body),
            &open.bearer,
        );
        let unauthorized = || {
            self.creds.refused();
            let _ = open.end.send(true);
            AnswerOutcome::Failed {
                class: ErrorClass::Unauthorized,
            }
        };
        let resp = match req.send().await {
            Ok(r) => r,
            Err(e) => {
                let msg = shed_core::authfail::flatten(&e);
                if shed_core::authfail::is_auth_shaped_message(&msg) {
                    return unauthorized();
                }
                return AnswerOutcome::Failed {
                    class: ErrorClass::Transport,
                };
            }
        };
        let status = resp.status().as_u16();
        match status {
            204 => return AnswerOutcome::Accepted,
            401 => return unauthorized(),
            _ => {}
        }
        let body = read_bounded(resp, self.timing.answer_timeout).await;
        let (code, rule) = error_code(&body);
        if status == 409 && code == "already_answered" {
            return AnswerOutcome::AlreadyAnswered;
        }
        AnswerOutcome::Refused {
            status,
            code: known_code(&code),
            rule: rule.map(|r| known_rule(&r)),
        }
    }
}

/// A refusal's class, by its status and `error.code`.
pub fn classify_refusal(status: u16, body: &[u8]) -> ErrorClass {
    let (code, _) = error_code(body);
    match (status, code.as_str()) {
        (401, _) => ErrorClass::Unauthorized,
        (403, "SOURCE_NOT_BOUND") => ErrorClass::NotBound,
        (403, "SOURCE_MACHINE_ID_REQUIRED") => ErrorClass::MachineIdRequired,
        (403, _) => ErrorClass::Forbidden,
        (409, "SOURCE_DUPLICATE_STREAM") => ErrorClass::Duplicate,
        (409, "SOURCE_IS_SERVICE") => ErrorClass::SourceIsService,
        (429, _) => ErrorClass::RateLimited,
        (404, _) => ErrorClass::Unsupported,
        (503, _) => ErrorClass::Unavailable,
        _ => ErrorClass::BadStatus,
    }
}

/// An error body's `error.code` and, beside it, `rule` (empty and `None` when absent).
fn error_code(body: &[u8]) -> (String, Option<String>) {
    #[derive(serde::Deserialize)]
    struct Detail {
        code: Option<String>,
    }
    #[derive(serde::Deserialize)]
    struct Body {
        error: Option<Detail>,
        rule: Option<String>,
    }
    match serde_json::from_slice::<Body>(body) {
        Ok(b) => (
            b.error.and_then(|e| e.code).unwrap_or_default(),
            b.rule.filter(|r| !r.is_empty()),
        ),
        Err(_) => (String::new(), None),
    }
}

/// A whole-seconds `Retry-After`, capped at [`MAX_RETRY_AFTER`]; anything else is none.
fn retry_after(v: Option<&reqwest::header::HeaderValue>) -> Option<Duration> {
    let secs: u64 = v?.to_str().ok()?.trim().parse().ok()?;
    Some(Duration::from_secs(secs).min(MAX_RETRY_AFTER))
}

/// At most [`MAX_ERROR_BODY`] bytes of a response's body, within `bound`.
async fn read_bounded(mut resp: reqwest::Response, bound: Duration) -> Vec<u8> {
    let mut out = Vec::new();
    let _ = tokio::time::timeout(bound, async {
        while let Ok(Some(chunk)) = resp.chunk().await {
            let room = MAX_ERROR_BODY - out.len();
            out.extend_from_slice(&chunk[..chunk.len().min(room)]);
            if out.len() >= MAX_ERROR_BODY {
                break;
            }
        }
    })
    .await;
    out
}

#[cfg(test)]
pub(crate) mod fake;

#[cfg(test)]
pub(crate) mod tests;
